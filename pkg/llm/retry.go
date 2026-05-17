package llm

import (
	"context"
	"errors"
	"fmt"
	"math"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// retryConfig controls retry behavior for transient provider errors. The zero
// value disables retries (MaxAttempts == 0).
type retryConfig struct {
	InitialBackoff time.Duration
	MaxAttempts    int
}

// defaultRetryConfig returns a conservative retry policy. Two additional
// attempts (three total including the first) with exponential backoff starting
// at 1s. Tests can override this via the registry.
func defaultRetryConfig() retryConfig {
	return retryConfig{
		MaxAttempts:    2,
		InitialBackoff: 1 * time.Second,
	}
}

// retryableError wraps a provider error with the HTTP status code so the retry
// loop can decide whether to retry.
type retryableError struct {
	wrapped    error
	retryAfter time.Duration // parsed Retry-After header, zero if absent
}

func (r *retryableError) Error() string { return r.wrapped.Error() }
func (r *retryableError) Unwrap() error { return r.wrapped }

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

// isRetryable inspects err to decide whether the request should be retried.
// It checks for retryableError (explicit HTTP status) and falls back to
// heuristic string matching on common transient error messages.
func isRetryable(err error) (time.Duration, bool) {
	if err == nil {
		return 0, false
	}

	var re *retryableError
	if errors.As(err, &re) {
		return re.retryAfter, true
	}

	// Heuristic: detect transient HTTP status codes in formatted error strings
	// from providers that use fmt.Errorf("openai: HTTP %d: ...", statusCode).
	msg := err.Error()

	for _, code := range []int{429, 500, 502, 503, 504} {
		codeStr := fmt.Sprintf("HTTP %d", code)
		if strings.Contains(msg, codeStr) {
			return 0, true
		}
	}

	return 0, false
}

// completeWithRetry calls fn up to cfg.MaxAttempts additional times on
// transient failures. It uses exponential backoff and respects Retry-After.
func completeWithRetry(
	ctx context.Context,
	cfg retryConfig,
	fn func(context.Context) (*Response, error),
) (*Response, error) {
	resp, err := fn(ctx)
	if err == nil || cfg.MaxAttempts <= 0 {
		return resp, err
	}

	retryAfter, retryable := isRetryable(err)
	if !retryable {
		return resp, err
	}

	lastErr := err

	for attempt := range cfg.MaxAttempts {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return nil, fmt.Errorf("llm: retry canceled: %w", ctxErr)
		}

		backoff := retryAfter
		if backoff <= 0 {
			backoff = cfg.InitialBackoff * time.Duration(math.Pow(2, float64(attempt)))
		}

		timer := time.NewTimer(backoff)

		select {
		case <-ctx.Done():
			timer.Stop()

			return nil, fmt.Errorf("llm: retry canceled: %w", ctx.Err())
		case <-timer.C:
		}

		resp, err = fn(ctx)
		if err == nil {
			return resp, nil
		}

		lastErr = err

		retryAfter, retryable = isRetryable(err)
		if !retryable {
			return resp, err
		}
	}

	return nil, lastErr
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
