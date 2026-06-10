package llm

import (
	"context"
	"crypto/rand"
	"encoding/binary"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/tommoulard/atteler/pkg/events"
)

// RetryPolicy controls retry behavior for transient provider errors. MaxAttempts
// is the number of additional attempts after the first request. The zero value
// disables retries (MaxAttempts == 0).
type RetryPolicy struct {
	randFloat64 func() float64
	sleep       func(context.Context, time.Duration) error
	now         func() time.Time

	InitialBackoff time.Duration
	MaxBackoff     time.Duration
	MaxElapsedTime time.Duration
	JitterFraction float64
	MaxAttempts    int
}

type retryConfig = RetryPolicy

// RetryPolicyConfig is the config-file/API shape for provider retry overrides.
// Nil fields inherit the registry default policy.
type RetryPolicyConfig struct {
	// MaxAttempts is the number of additional attempts after the first request.
	MaxAttempts *int
	// InitialBackoffMS is the first exponential backoff delay in milliseconds.
	InitialBackoffMS *int
	// MaxBackoffMS caps exponential, Retry-After, and jittered delays in milliseconds.
	MaxBackoffMS *int
	// MaxElapsedMS caps the total retry elapsed budget in milliseconds.
	MaxElapsedMS *int
	// JitterFraction adds positive jitter as a fraction of the selected delay.
	JitterFraction *float64
}

// RetryPolicyInfo is the diagnostic-safe view of the active retry policy.
type RetryPolicyInfo struct {
	InitialBackoff time.Duration
	MaxBackoff     time.Duration
	MaxElapsedTime time.Duration
	JitterFraction float64
	MaxAttempts    int
}

// RetryError reports the final provider error together with the retry path that
// led to it.
type RetryError struct {
	Err        error
	Provider   string
	Model      string
	Outcome    string
	RequestID  string
	Elapsed    time.Duration
	StatusCode int
	Attempts   int
	Retries    int
}

// Error returns a compact retry summary followed by the final provider error.
func (e *RetryError) Error() string {
	if e == nil {
		return "<nil>"
	}

	subject := retrySubject(e.Provider, e.Model)

	status := ""
	if e.StatusCode > 0 {
		status = ", last_status=" + strconv.Itoa(e.StatusCode)
	}

	requestID := ""
	if e.RequestID != "" {
		requestID = ", request_id=" + e.RequestID
	}

	return fmt.Sprintf(
		"llm: %s failed after %d %s (%d %s, outcome=%s, elapsed=%s%s%s): %v",
		subject,
		e.Attempts,
		plural(e.Attempts, "attempt", "attempts"),
		e.Retries,
		plural(e.Retries, "retry", "retries"),
		e.Outcome,
		e.Elapsed.Round(time.Millisecond),
		status,
		requestID,
		e.Err,
	)
}

// Unwrap exposes the final provider error for errors.As/errors.Is callers.
func (e *RetryError) Unwrap() error {
	if e == nil {
		return nil
	}

	return e.Err
}

// defaultRetryConfig returns a conservative retry policy. Two additional
// attempts (three total including the first) with exponential backoff starting
// at 1s. A small positive jitter prevents concurrent failures from stampeding
// the same provider.
func defaultRetryConfig() retryConfig {
	return retryConfig{
		MaxAttempts:    2,
		InitialBackoff: time.Second,
		MaxBackoff:     10 * time.Second,
		MaxElapsedTime: 30 * time.Second,
		JitterFraction: 0.2,
	}
}

func (c RetryPolicyConfig) hasOverrides() bool {
	return c.MaxAttempts != nil ||
		c.InitialBackoffMS != nil ||
		c.MaxBackoffMS != nil ||
		c.MaxElapsedMS != nil ||
		c.JitterFraction != nil
}

func (c RetryPolicyConfig) apply(base retryConfig) retryConfig {
	if c.MaxAttempts != nil {
		base.MaxAttempts = *c.MaxAttempts
	}

	if c.InitialBackoffMS != nil {
		base.InitialBackoff = time.Duration(*c.InitialBackoffMS) * time.Millisecond
	}

	if c.MaxBackoffMS != nil {
		base.MaxBackoff = time.Duration(*c.MaxBackoffMS) * time.Millisecond
	}

	if c.MaxElapsedMS != nil {
		base.MaxElapsedTime = time.Duration(*c.MaxElapsedMS) * time.Millisecond
	}

	if c.JitterFraction != nil {
		base.JitterFraction = *c.JitterFraction
	}

	return base
}

func (c retryConfig) normalized() retryConfig {
	if c.MaxAttempts < 0 {
		c.MaxAttempts = 0
	}

	if c.InitialBackoff < 0 {
		c.InitialBackoff = 0
	}

	if c.MaxBackoff < 0 {
		c.MaxBackoff = 0
	}

	if c.MaxElapsedTime < 0 {
		c.MaxElapsedTime = 0
	}

	if c.JitterFraction < 0 {
		c.JitterFraction = 0
	}

	if c.JitterFraction > 1 {
		c.JitterFraction = 1
	}

	if c.randFloat64 == nil {
		c.randFloat64 = randomFloat64
	}

	if c.sleep == nil {
		c.sleep = sleepContext
	}

	if c.now == nil {
		c.now = time.Now
	}

	return c
}

func (c retryConfig) info() RetryPolicyInfo {
	c = c.normalized()

	return RetryPolicyInfo{
		MaxAttempts:    c.MaxAttempts,
		InitialBackoff: c.InitialBackoff,
		MaxBackoff:     c.MaxBackoff,
		MaxElapsedTime: c.MaxElapsedTime,
		JitterFraction: c.JitterFraction,
	}
}

// isRetryableStatus returns true for HTTP status codes that indicate transient
// errors worth retrying: 429 (rate limit), 500, 502, 503, 504.
func isRetryableStatus(code int) bool {
	switch code {
	case http.StatusTooManyRequests,
		http.StatusInternalServerError,
		http.StatusBadGateway,
		http.StatusServiceUnavailable,
		http.StatusGatewayTimeout:
		return true
	default:
		return false
	}
}

func retryableHTTPStatusError(err error, statusCode int, retryAfter string) error {
	if err == nil || !isRetryableStatus(statusCode) {
		return err
	}

	return &ProviderError{
		StatusCode:   statusCode,
		RetryAfter:   parseRetryAfter(retryAfter),
		Message:      err.Error(),
		Retryability: RetryabilityRetryable,
	}
}

type retryMetadata struct {
	provider string
	model    string
}

type retryDecision struct {
	requestID   string
	class       Retryability
	retryAfter  time.Duration
	statusCode  int
	retryable   bool
	legacyMatch bool
}

const (
	retryOutcomeBudgetExhausted = "budget_exhausted"
	retryOutcomeCanceled        = "canceled"
	retryOutcomeExhausted       = "exhausted"
	retryOutcomeNonRetryable    = "non_retryable"
	retryOutcomeScheduled       = "scheduled"
	retryOutcomeSuccess         = "success"
)

// completeWithRetry calls fn up to cfg.MaxAttempts additional times on
// transient failures. It uses exponential backoff, positive jitter, Retry-After
// hints, a total elapsed retry budget, and emits retry lifecycle events.
func completeWithRetry(
	ctx context.Context,
	cfg retryConfig,
	meta retryMetadata,
	fn func(context.Context) (*Response, error),
) (*Response, error) {
	return withRetry(ctx, cfg, meta, fn)
}

func embeddingWithRetry(
	ctx context.Context,
	cfg retryConfig,
	meta retryMetadata,
	fn func(context.Context) (*EmbeddingResponse, error),
) (*EmbeddingResponse, error) {
	return withRetry(ctx, cfg, meta, fn)
}

func withRetry[T any](
	ctx context.Context,
	cfg retryConfig,
	meta retryMetadata,
	fn func(context.Context) (T, error),
) (T, error) {
	var zero T

	if err := requireCredentialContext(ctx); err != nil {
		return zero, err
	}

	cfg = cfg.normalized()
	start := cfg.now()

	resp, err := fn(ctx)
	if err == nil || cfg.MaxAttempts <= 0 {
		return resp, err
	}

	decision := retryDecisionForError(err)

	if ctxErr := ctx.Err(); ctxErr != nil {
		outcome := retryOutcomeCanceled
		emitRetryFinal(ctx, meta, decision, 1, cfg.MaxAttempts, start, cfg, outcome)

		return zero, newRetryError(meta, retryCanceledError(err, ctxErr), decision, 1, start, cfg, outcome)
	}

	if !decision.retryable {
		outcome := retryOutcomeNonRetryable
		emitRetryFinal(ctx, meta, decision, 1, cfg.MaxAttempts, start, cfg, outcome)

		return resp, newRetryError(meta, err, decision, 1, start, cfg, outcome)
	}

	lastErr := err
	attempts := 1

	for retryIndex := range cfg.MaxAttempts {
		if ctxErr := ctx.Err(); ctxErr != nil {
			outcome := retryOutcomeCanceled
			emitRetryFinal(ctx, meta, decision, attempts, cfg.MaxAttempts, start, cfg, outcome)

			return zero, newRetryError(meta, retryCanceledError(lastErr, ctxErr), decision, attempts, start, cfg, outcome)
		}

		delay := retryDelay(cfg, retryIndex, decision.retryAfter)
		if retryBudgetExceeded(cfg, start, delay) {
			outcome := retryOutcomeBudgetExhausted

			emitRetryBudgetExhausted(ctx, meta, decision, attempts, cfg.MaxAttempts, start, cfg, delay)

			return zero, newRetryError(meta, lastErr, decision, attempts, start, cfg, outcome)
		}

		emitRetryScheduled(ctx, meta, decision, attempts+1, cfg.MaxAttempts, delay)

		if sleepErr := cfg.sleep(ctx, delay); sleepErr != nil {
			outcome := retryOutcomeCanceled
			emitRetryFinal(ctx, meta, decision, attempts, cfg.MaxAttempts, start, cfg, outcome)

			return zero, newRetryError(meta, retryCanceledError(lastErr, sleepErr), decision, attempts, start, cfg, outcome)
		}

		resp, err = fn(ctx)

		attempts++
		if err == nil {
			emitRetryFinal(ctx, meta, decision, attempts, cfg.MaxAttempts, start, cfg, retryOutcomeSuccess)

			return resp, nil
		}

		attemptDecision := retryDecisionForError(err)

		if ctxErr := ctx.Err(); ctxErr != nil {
			outcome := retryOutcomeCanceled
			decision = retryDecisionWithFallback(attemptDecision, decision)
			emitRetryFinal(ctx, meta, decision, attempts, cfg.MaxAttempts, start, cfg, outcome)

			return zero, newRetryError(meta, retryAttemptCanceledError(lastErr, err, ctxErr), decision, attempts, start, cfg, outcome)
		}

		lastErr = err

		decision = attemptDecision
		if !decision.retryable {
			outcome := retryOutcomeNonRetryable
			emitRetryFinal(ctx, meta, decision, attempts, cfg.MaxAttempts, start, cfg, outcome)

			return resp, newRetryError(meta, lastErr, decision, attempts, start, cfg, outcome)
		}
	}

	outcome := retryOutcomeExhausted
	emitRetryFinal(ctx, meta, decision, attempts, cfg.MaxAttempts, start, cfg, outcome)

	return zero, newRetryError(meta, lastErr, decision, attempts, start, cfg, outcome)
}

func retryDecisionWithFallback(primary, fallback retryDecision) retryDecision {
	if primary.statusCode > 0 ||
		primary.requestID != "" ||
		(primary.class != "" && primary.class != RetryabilityUnknown) ||
		primary.retryable ||
		primary.retryAfter > 0 ||
		primary.legacyMatch {
		return primary
	}

	return fallback
}

func retryDecisionForError(err error) retryDecision {
	if err == nil {
		return retryDecision{}
	}

	var providerErr *ProviderError
	if errors.As(err, &providerErr) {
		return retryDecision{
			retryAfter: providerErr.RetryAfter,
			requestID:  providerErr.RequestID,
			class:      providerErr.retryability(),
			statusCode: providerErr.StatusCode,
			retryable:  providerErr.IsRetryable(),
		}
	}

	return legacyRetryDecision(err)
}

// legacyRetryDecision is a degraded fallback for older adapters that still
// return formatted text errors instead of ProviderError. Only the first
// anchored, adapter-shaped status token in the message is honored ("HTTP 503:
// body", "HTTP 429 (request_id=...): body", "status=503"); statuses merely
// quoted inside response bodies ("... see HTTP 503 documentation ...") are not
// anchored and never consulted. New adapters should return ProviderError so
// retry decisions do not depend on message text.
func legacyRetryDecision(err error) retryDecision {
	code, ok := legacyStatusCode(err.Error())
	if !ok || !isRetryableStatus(code) {
		return retryDecision{class: RetryabilityUnknown}
	}

	return retryDecision{
		class:       RetryabilityRetryable,
		statusCode:  code,
		retryable:   true,
		legacyMatch: true,
	}
}

// legacyStatusPrefixes are the status-token shapes plain-text adapter errors
// emit, e.g. "ollama: embeddings HTTP 503: body" or "request failed:
// status=503".
var legacyStatusPrefixes = []string{"HTTP ", "status=", "status "}

// legacyStatusCode returns the HTTP status carried by the first anchored
// status token in msg. Error wrapping prepends context ("context: %w"), so the
// first anchored token belongs to the adapter itself; any later tokens sit in
// quoted body text and are ignored.
func legacyStatusCode(msg string) (int, bool) {
	for msg != "" {
		idx, prefixLen := legacyNextStatusToken(msg)
		if idx < 0 {
			return 0, false
		}

		rest := msg[idx+prefixLen:]
		digits := leadingDigits(rest)

		if code, ok := legacyAnchoredStatus(digits, rest[len(digits):]); ok {
			return code, true
		}

		msg = rest
	}

	return 0, false
}

// legacyNextStatusToken locates the earliest status-token prefix in msg and
// returns its index together with the matched prefix length.
func legacyNextStatusToken(msg string) (idx, prefixLen int) {
	idx = -1

	for _, prefix := range legacyStatusPrefixes {
		i := strings.Index(msg, prefix)
		if i < 0 {
			continue
		}

		if idx < 0 || i < idx {
			idx, prefixLen = i, len(prefix)
		}
	}

	return idx, prefixLen
}

// legacyAnchoredStatus reports whether digits form an HTTP status code that
// terminates the way adapter-formatted errors do.
func legacyAnchoredStatus(digits, remaining string) (int, bool) {
	if len(digits) != 3 {
		return 0, false
	}

	code, err := strconv.Atoi(digits)
	if err != nil || code < 100 || code > 599 {
		return 0, false
	}

	if !legacyStatusTerminated(remaining) {
		return 0, false
	}

	return code, true
}

// legacyStatusTerminated reports whether the text following a status code
// matches an adapter-emitted shape: end of message, a colon before the body,
// list punctuation, or a parenthesized metadata block. A bare space followed
// by prose ("HTTP 503 documentation") is not a terminator.
func legacyStatusTerminated(value string) bool {
	if value == "" {
		return true
	}

	switch value[0] {
	case ':', ')', ',', ';':
		return true
	}

	return strings.HasPrefix(value, " (")
}

func leadingDigits(value string) string {
	for i := range len(value) {
		if value[i] < '0' || value[i] > '9' {
			return value[:i]
		}
	}

	return value
}

func retryDelay(cfg retryConfig, retryIndex int, retryAfter time.Duration) time.Duration {
	var delay time.Duration
	if retryAfter > 0 {
		delay = retryAfter
	} else {
		delay = exponentialBackoff(cfg, retryIndex)
	}

	delay = jitterDelay(cfg, delay)
	if cfg.MaxBackoff > 0 && delay > cfg.MaxBackoff {
		return cfg.MaxBackoff
	}

	return delay
}

func exponentialBackoff(cfg retryConfig, retryIndex int) time.Duration {
	delay := cfg.InitialBackoff
	for range retryIndex {
		if cfg.MaxBackoff > 0 && delay > cfg.MaxBackoff/2 {
			return cfg.MaxBackoff
		}

		delay *= 2
	}

	if cfg.MaxBackoff > 0 && delay > cfg.MaxBackoff {
		return cfg.MaxBackoff
	}

	return delay
}

func jitterDelay(cfg retryConfig, delay time.Duration) time.Duration {
	if delay <= 0 || cfg.JitterFraction <= 0 {
		return delay
	}

	jitter := cfg.randFloat64()
	if jitter < 0 {
		jitter = 0
	}

	if jitter > 1 {
		jitter = 1
	}

	return delay + time.Duration(float64(delay)*cfg.JitterFraction*jitter)
}

func retryBudgetExceeded(cfg retryConfig, start time.Time, delay time.Duration) bool {
	if cfg.MaxElapsedTime <= 0 {
		return false
	}

	elapsed := cfg.now().Sub(start)

	return elapsed+delay > cfg.MaxElapsedTime
}

func newRetryError(
	meta retryMetadata,
	err error,
	decision retryDecision,
	attempts int,
	start time.Time,
	cfg retryConfig,
	outcome string,
) *RetryError {
	return &RetryError{
		Err:        err,
		Provider:   meta.provider,
		Model:      meta.model,
		Attempts:   attempts,
		Retries:    attempts - 1,
		Outcome:    outcome,
		Elapsed:    cfg.now().Sub(start),
		StatusCode: decision.statusCode,
		RequestID:  decision.requestID,
	}
}

func retryCanceledError(lastErr, cancelErr error) error {
	if lastErr == nil {
		return cancelErr
	}

	if cancelErr == nil {
		return lastErr
	}

	if errors.Is(lastErr, cancelErr) {
		return lastErr
	}

	return fmt.Errorf("%w; %w", lastErr, cancelErr)
}

func retryAttemptCanceledError(previousErr, attemptErr, cancelErr error) error {
	if cancelErr != nil && errors.Is(attemptErr, cancelErr) {
		return retryCanceledError(previousErr, attemptErr)
	}

	err := retryCanceledError(attemptErr, previousErr)
	if cancelErr != nil && !errors.Is(err, cancelErr) {
		err = retryCanceledError(err, cancelErr)
	}

	return err
}

func emitRetryScheduled(
	ctx context.Context,
	meta retryMetadata,
	decision retryDecision,
	attempt int,
	maxRetries int,
	delay time.Duration,
) {
	metadata := retryEventMetadata(meta, decision, attempt, maxRetries)
	metadata["outcome"] = retryOutcomeScheduled
	metadata["delay_ms"] = strconv.FormatInt(delay.Milliseconds(), 10)

	emitRetryEvent(ctx, meta, metadata)
}

func emitRetryFinal(
	ctx context.Context,
	meta retryMetadata,
	decision retryDecision,
	attempts int,
	maxRetries int,
	start time.Time,
	cfg retryConfig,
	outcome string,
) {
	metadata := retryEventMetadata(meta, decision, attempts, maxRetries)
	metadata["outcome"] = outcome
	metadata["elapsed_ms"] = strconv.FormatInt(cfg.now().Sub(start).Milliseconds(), 10)

	emitRetryEvent(ctx, meta, metadata)
}

func emitRetryBudgetExhausted(
	ctx context.Context,
	meta retryMetadata,
	decision retryDecision,
	attempts int,
	maxRetries int,
	start time.Time,
	cfg retryConfig,
	delay time.Duration,
) {
	metadata := retryEventMetadata(meta, decision, attempts, maxRetries)
	metadata["outcome"] = retryOutcomeBudgetExhausted
	metadata["elapsed_ms"] = strconv.FormatInt(cfg.now().Sub(start).Milliseconds(), 10)
	metadata["delay_ms"] = strconv.FormatInt(delay.Milliseconds(), 10)

	emitRetryEvent(ctx, meta, metadata)
}

func retryEventMetadata(meta retryMetadata, decision retryDecision, attempt, maxRetries int) map[string]string {
	metadata := map[string]string{
		"attempt":      strconv.Itoa(attempt),
		"max_attempts": strconv.Itoa(maxRetries + 1),
		"max_retries":  strconv.Itoa(maxRetries),
		"provider":     meta.provider,
		"retryable":    strconv.FormatBool(decision.retryable),
	}

	if decision.statusCode > 0 {
		metadata["status"] = strconv.Itoa(decision.statusCode)
	}

	if decision.class != "" {
		metadata["classification"] = string(decision.class)
	}

	if decision.requestID != "" {
		metadata["request_id"] = decision.requestID
	}

	if decision.retryAfter > 0 {
		metadata["retry_after_ms"] = strconv.FormatInt(decision.retryAfter.Milliseconds(), 10)
	}

	if decision.legacyMatch {
		metadata["legacy_fallback"] = "true"
	}

	return metadata
}

func emitRetryEvent(ctx context.Context, meta retryMetadata, metadata map[string]string) {
	event := events.Event{
		Type:     events.ProviderRetry,
		Model:    meta.model,
		Metadata: metadata,
	}

	if err := events.EmitFromContext(ctx, event); err == nil {
		return
	}

	if ctx.Err() == nil {
		return
	}

	// Cancellation is itself a retry outcome callers need to observe. The event
	// logger can still record the final retry event after the request context is
	// done. Hook execution remains canceled.
	if err := events.EmitFromContextBestEffort(ctx, event); err != nil {
		return
	}
}

func sleepContext(ctx context.Context, delay time.Duration) error {
	if delay <= 0 {
		if err := ctx.Err(); err != nil {
			return fmt.Errorf("llm: retry sleep canceled: %w", err)
		}

		return nil
	}

	timer := time.NewTimer(delay)
	defer timer.Stop()

	select {
	case <-ctx.Done():
		return fmt.Errorf("llm: retry sleep canceled: %w", ctx.Err())
	case <-timer.C:
		return nil
	}
}

func randomFloat64() float64 {
	var buf [8]byte
	if _, err := rand.Read(buf[:]); err != nil {
		return 0.5
	}

	const mantissaBits = 53

	value := binary.BigEndian.Uint64(buf[:]) >> (64 - mantissaBits)

	return float64(value) / (1 << mantissaBits)
}

func retrySubject(provider, model string) string {
	switch {
	case provider != "" && model != "":
		return provider + "/" + model
	case provider != "":
		return provider
	case model != "":
		return model
	default:
		return "provider request"
	}
}

func plural(count int, singular, plural string) string {
	if count == 1 {
		return singular
	}

	return plural
}

// parseRetryAfter attempts to parse a Retry-After header value. It supports
// both delay-seconds and HTTP-date formats. Returns zero on failure.
func parseRetryAfter(value string) time.Duration {
	value = strings.TrimSpace(value)
	if value == "" {
		return 0
	}

	if seconds, err := strconv.Atoi(value); err == nil && seconds > 0 {
		return time.Duration(seconds) * time.Second
	}

	if t, err := http.ParseTime(value); err == nil {
		d := time.Until(t)
		if d > 0 {
			return d
		}
	}

	return 0
}
