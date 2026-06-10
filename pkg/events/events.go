// Package events emits atteler lifecycle events to local configured hooks.
package events

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"maps"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
	"unicode"

	"github.com/tommoulard/atteler/pkg/config"
	"github.com/tommoulard/atteler/pkg/shell"
)

const (
	// SessionStart is emitted when an interactive or one-shot session starts.
	SessionStart = "session_start"
	// UserMessage is emitted after a user message is appended to a session.
	UserMessage = "user_message"
	// AssistantMessage is emitted after an assistant response is appended.
	AssistantMessage = "assistant_message"
	// Error is emitted when an LLM request or session operation fails.
	Error = "error"
	// SessionEnd is emitted when an interactive or one-shot session ends.
	SessionEnd = "session_end"
	// FileRead is emitted when Atteler reads a user/project file.
	FileRead = "file_read"
	// FileWrite is emitted when Atteler writes a local file.
	FileWrite = "file_write"
	// ContextAdd is emitted when a local reference is added to LLM context.
	ContextAdd = "context_add"
	// ContextManifest is emitted with an audit manifest before an LLM request.
	ContextManifest = "context_manifest"
	// CommandExecute is emitted when Atteler starts a local command.
	CommandExecute = "command_execute"
	// CommandOutput is emitted when local command output is available.
	CommandOutput = "command_output"
	// ToolExecute is emitted when Atteler invokes a provider/tool.
	ToolExecute = "tool_execute"
	// ProviderRetry is emitted for each provider retry schedule/final outcome.
	ProviderRetry = "provider_retry"
	// AgentExecute is emitted when a configured agent is selected for work.
	AgentExecute = "agent_execute"
	// RouteDecision is emitted when model routing selects or rejects candidates.
	RouteDecision = "route_decision"

	// EventSchemaVersion is the current lifecycle hook payload schema.
	EventSchemaVersion = 1

	defaultTimeout      = 10 * time.Second
	defaultMaxAttempts  = 1
	maxHookAttempts     = 10
	defaultRetryBackoff = 100 * time.Millisecond
	maxRetryBackoff     = 30 * time.Second
)

var eventIDCounter atomic.Uint64

// PayloadMode controls how much event data is passed to a hook.
type PayloadMode string

const (
	// PayloadMetadata sends only non-sensitive event metadata.
	PayloadMetadata PayloadMode = "metadata"
	// PayloadSummary adds bounded summaries for sensitive content/error fields.
	PayloadSummary PayloadMode = "summary"
	// PayloadFull sends content-bearing fields after secret redaction and size limits.
	PayloadFull PayloadMode = "full"
)

// SupportedEventType describes one hook event type supported by this package.
type SupportedEventType struct {
	Type        string `json:"type"`
	Description string `json:"description"`
}

var supportedEventTypes = []SupportedEventType{
	{Type: AgentExecute, Description: "Emitted when a configured agent is selected for work."},
	{Type: AssistantMessage, Description: "Emitted after an assistant response is appended."},
	{Type: CommandExecute, Description: "Emitted when Atteler starts a local command."},
	{Type: CommandOutput, Description: "Emitted when local command output is available."},
	{Type: ContextAdd, Description: "Emitted when a local reference is added to LLM context."},
	{Type: ContextManifest, Description: "Emitted before an LLM request with context audit metadata."},
	{Type: Error, Description: "Emitted when an LLM request or session operation fails."},
	{Type: FileRead, Description: "Emitted when Atteler reads a user or project file."},
	{Type: FileWrite, Description: "Emitted when Atteler writes a local file."},
	{Type: ProviderRetry, Description: "Emitted when provider retry lifecycle state changes."},
	{Type: RouteDecision, Description: "Emitted when model routing selects or rejects candidates."},
	{Type: SessionEnd, Description: "Emitted when an interactive or one-shot session ends."},
	{Type: SessionStart, Description: "Emitted when an interactive or one-shot session starts."},
	{Type: ToolExecute, Description: "Emitted when Atteler invokes a provider or tool."},
	{Type: UserMessage, Description: "Emitted after a user message is appended to a session."},
}

// SupportedEventTypes returns hook event types supported by this package.
//
// The result is sorted by Type and each call returns a new slice that callers
// may modify without affecting later calls.
func SupportedEventTypes() []SupportedEventType {
	return append([]SupportedEventType(nil), supportedEventTypes...)
}

// Event is the JSON payload sent to hooks on stdin.
//
//nolint:govet // Field order follows the public JSON payload schema.
type Event struct {
	Metadata       map[string]string `json:"metadata,omitempty"`
	Timestamp      time.Time         `json:"timestamp"`
	SchemaVersion  int               `json:"schema_version,omitempty"`
	EventID        string            `json:"event_id,omitempty"`
	Type           string            `json:"type"`
	SessionID      string            `json:"session_id,omitempty"`
	SessionPath    string            `json:"session_path,omitempty"`
	Agent          string            `json:"agent,omitempty"`
	Model          string            `json:"model,omitempty"`
	Role           string            `json:"role,omitempty"`
	Content        string            `json:"content,omitempty"`
	ContentSummary string            `json:"content_summary,omitempty"`
	Error          string            `json:"error,omitempty"`
	ErrorSummary   string            `json:"error_summary,omitempty"`
	PayloadMode    string            `json:"payload_mode,omitempty"`
	Redacted       bool              `json:"redacted,omitempty"`
	Truncated      bool              `json:"truncated,omitempty"`
}

// Hook is a local command hook for one event type.
//
//nolint:govet // fieldalignment: field order groups command, timeout, and privacy controls.
type Hook struct {
	Env          map[string]string
	Command      []string
	Timeout      time.Duration
	RetryBackoff time.Duration
	MaxAttempts  int
	PayloadMode  PayloadMode
	InheritEnv   bool
	Blocking     bool
}

// Observer receives lifecycle events for best-effort local background work.
// Observer failures must not interrupt the user-facing command or hook flow.
type Observer interface {
	ObserveEvent(context.Context, Event) error
}

// Runner emits events to configured hooks.
type Runner struct {
	deliveryCtx context.Context
	logger      *Logger
	ledger      *Ledger
	hooks       map[string][]Hook
	wg          *sync.WaitGroup
	observers   []Observer
}

// RunnerOptions configures lifecycle event delivery surfaces.
type RunnerOptions struct {
	DeliveryContext context.Context
	LogWriter       io.Writer
	LedgerWriter    io.Writer
	Ledger          *Ledger
	Observers       []Observer
}

// NewRunner creates a hook runner from config.
func NewRunner(configured map[string][]config.HookConfig) *Runner {
	return NewRunnerWithLogger(configured, nil)
}

// NewRunnerWithLogger creates a hook runner and optional built-in event logger.
func NewRunnerWithLogger(configured map[string][]config.HookConfig, logWriter io.Writer) *Runner {
	return NewRunnerWithLoggerAndObservers(configured, logWriter)
}

// NewRunnerWithLoggerAndObservers creates a hook runner, optional event logger,
// and best-effort local observers.
func NewRunnerWithLoggerAndObservers(configured map[string][]config.HookConfig, logWriter io.Writer, observers ...Observer) *Runner {
	return NewRunnerWithOptions(configured, RunnerOptions{LogWriter: logWriter, Observers: observers})
}

// NewRunnerWithOptions creates a hook runner with explicit logging, durable
// ledger, and observer options.
func NewRunnerWithOptions(configured map[string][]config.HookConfig, opts RunnerOptions) *Runner {
	hooks := make(map[string][]Hook, len(configured))
	for eventType, configs := range configured {
		for _, cfg := range configs {
			if len(cfg.Command) == 0 {
				continue
			}

			timeout := defaultTimeout
			if cfg.TimeoutSeconds > 0 {
				timeout = time.Duration(cfg.TimeoutSeconds) * time.Second
			}

			hooks[eventType] = append(hooks[eventType], Hook{
				Command:      append([]string(nil), cfg.Command...),
				Env:          cloneMap(cfg.Env),
				Timeout:      timeout,
				RetryBackoff: normalizeRetryBackoff(cfg.RetryBackoffMillis),
				MaxAttempts:  normalizeMaxAttempts(cfg.MaxAttempts),
				PayloadMode:  normalizePayloadMode(cfg.Payload),
				InheritEnv:   cfg.InheritEnv,
				Blocking:     cfg.Blocking,
			})
		}
	}

	ledger := opts.Ledger
	if ledger == nil && opts.LedgerWriter != nil {
		ledger = NewLedger(opts.LedgerWriter)
	}

	return &Runner{
		deliveryCtx: opts.DeliveryContext,
		hooks:       hooks,
		logger:      NewLogger(opts.LogWriter),
		ledger:      ledger,
		observers:   compactObservers(opts.Observers),
		wg:          &sync.WaitGroup{},
	}
}

// WithLogger returns a runner with the same hooks and a new optional logger.
func (r *Runner) WithLogger(logWriter io.Writer) *Runner {
	if r == nil {
		return NewRunnerWithLogger(nil, logWriter)
	}

	return r.clone(logWriter, append([]Observer(nil), r.observers...))
}

// WithLoggerAndObservers returns a runner with the same hooks, durable ledger,
// and delivery group, while replacing the compact logger and observer list.
func (r *Runner) WithLoggerAndObservers(logWriter io.Writer, observers ...Observer) *Runner {
	if r == nil {
		return NewRunnerWithLoggerAndObservers(nil, logWriter, observers...)
	}

	return r.clone(logWriter, compactObservers(observers))
}

func (r *Runner) clone(logWriter io.Writer, observers []Observer) *Runner {
	hooks := make(map[string][]Hook, len(r.hooks))
	for eventType, configured := range r.hooks {
		for _, hook := range configured {
			hooks[eventType] = append(hooks[eventType], Hook{
				Command:      append([]string(nil), hook.Command...),
				Env:          cloneMap(hook.Env),
				Timeout:      hook.Timeout,
				RetryBackoff: hook.RetryBackoff,
				MaxAttempts:  hook.MaxAttempts,
				PayloadMode:  hook.PayloadMode,
				InheritEnv:   hook.InheritEnv,
				Blocking:     hook.Blocking,
			})
		}
	}

	return &Runner{
		deliveryCtx: r.deliveryCtx,
		hooks:       hooks,
		logger:      NewLogger(logWriter),
		ledger:      r.ledger,
		observers:   observers,
		wg:          r.waitGroup(),
	}
}

// Emit sends event to every hook registered for event.Type.
func (r *Runner) Emit(ctx context.Context, event Event) error {
	if r == nil || event.Type == "" {
		return nil
	}

	event, err := normalizeEventForEmit(ctx, event)
	if err != nil {
		return err
	}

	if r.logger != nil {
		r.logger.Log(event)
	}

	r.notifyObservers(ctx, event)

	if err := r.appendLedgerEvent(event); err != nil {
		return err
	}

	if len(r.hooks) == 0 {
		return nil
	}

	hooks := r.hooks[event.Type]
	if len(hooks) == 0 {
		return nil
	}

	var failures []error

	for _, hook := range hooks {
		hookEvent := sanitizeEventForHook(event, hook.PayloadMode)

		payload, err := json.Marshal(hookEvent)
		if err != nil {
			return fmt.Errorf("events: marshal %s: %w", hookEvent.Type, err)
		}

		payload = append(payload, '\n')
		logHookInvocation(hookEvent, hook, len(payload))

		if hook.Blocking {
			if err := r.deliverHook(ctx, hook, hookEvent, payload); err != nil {
				failures = append(failures, err)
			}

			continue
		}

		r.queueHook(ctx, hook, hookEvent, payload)
	}

	return errors.Join(failures...)
}

func normalizeEventForEmit(ctx context.Context, event Event) (Event, error) {
	if ctx == nil {
		return Event{}, errors.New("events: context is required")
	}

	if err := ctx.Err(); err != nil {
		return Event{}, fmt.Errorf("events: context already done: %w", err)
	}

	return normalizeEventFields(event), nil
}

func normalizeEventFields(event Event) Event {
	if event.Timestamp.IsZero() {
		event.Timestamp = time.Now().UTC()
	}

	if event.SchemaVersion == 0 {
		event.SchemaVersion = EventSchemaVersion
	}

	if event.EventID == "" {
		event.EventID = nextEventID(event.Timestamp)
	}

	return event
}

// Wait waits for non-blocking hook deliveries queued before this call to finish.
func (r *Runner) Wait(ctx context.Context) error {
	if r == nil {
		return nil
	}

	if ctx == nil {
		return errors.New("events: context is required")
	}

	done := make(chan struct{})
	go func() { //nolint:wsl_v5 // Standard bridge from WaitGroup to select.
		r.waitGroup().Wait()
		close(done)
	}()

	select {
	case <-done:
		return nil
	case <-ctx.Done():
		return fmt.Errorf("events: wait for hooks: %w", ctx.Err())
	}
}

// Close waits for queued non-blocking hooks and closes the durable ledger when
// the runner owns a closeable ledger.
func (r *Runner) Close(ctx context.Context) error {
	if r == nil {
		return nil
	}

	if err := r.Wait(ctx); err != nil {
		return err
	}

	if err := r.ledger.Close(); err != nil {
		return err
	}

	return nil
}

func (r *Runner) notifyObservers(ctx context.Context, event Event) {
	if r == nil || len(r.observers) == 0 {
		return
	}

	for _, observer := range r.observers {
		if observer == nil {
			continue
		}

		if err := notifyObserver(ctx, observer, event.cloneForObserver()); err != nil {
			continue
		}
	}
}

func notifyObserver(ctx context.Context, observer Observer, event Event) (err error) {
	defer func() {
		if recovered := recover(); recovered != nil {
			err = fmt.Errorf("events: observer panic: %v", recovered)
		}
	}()

	if err := observer.ObserveEvent(ctx, event); err != nil {
		return fmt.Errorf("events: observer: %w", err)
	}

	return nil
}

func (e Event) cloneForObserver() Event {
	e.Metadata = maps.Clone(e.Metadata)

	return e
}

func compactObservers(observers []Observer) []Observer {
	out := make([]Observer, 0, len(observers))
	for _, observer := range observers {
		if observer != nil {
			out = append(out, observer)
		}
	}

	return out
}

//nolint:contextcheck // Uses the runner lifecycle context so queued telemetry delivery is not canceled by short-lived request contexts.
func (r *Runner) queueHook(ctx context.Context, hook Hook, event Event, payload []byte) {
	r.appendHookLedgerRecord(hook, event, hookLedgerRecord{
		Phase:        LedgerPhaseHookQueued,
		Outcome:      HookOutcomeQueued,
		PayloadBytes: len(payload),
	})

	// Telemetry hooks are non-blocking and run on the runner lifecycle context
	// when one is available, so request-scoped cancellation does not turn a
	// queued delivery into best-effort-only work.
	deliveryCtx := r.deliveryCtx
	if deliveryCtx == nil {
		deliveryCtx = DetachContext(ctx)
	}

	r.waitGroup().Go(func() {
		if err := r.deliverHook(deliveryCtx, hook, event, payload); err != nil {
			auditEvent := Event{}
			safeType := sanitizeEventType(&auditEvent, event.Type)
			slogWarnHookDelivery(safeType, hook, err)
		}
	})
}

func (r *Runner) waitGroup() *sync.WaitGroup {
	if r.wg == nil {
		r.wg = &sync.WaitGroup{}
	}

	return r.wg
}

// DetachContext returns a context that preserves values from ctx while ignoring
// its deadline and cancellation signal. Hook telemetry uses it so request
// cancellation does not discard already accepted non-blocking deliveries.
func DetachContext(ctx context.Context) context.Context {
	if ctx == nil {
		return nil
	}

	return detachedDeliveryContext{Context: ctx}
}

type detachedDeliveryContext struct {
	context.Context
}

func (detachedDeliveryContext) Deadline() (time.Time, bool) {
	return time.Time{}, false
}

func (detachedDeliveryContext) Done() <-chan struct{} {
	return nil
}

func (detachedDeliveryContext) Err() error {
	return nil
}

func (r *Runner) deliverHook(ctx context.Context, hook Hook, event Event, payload []byte) error {
	maxAttempts := normalizeMaxAttempts(hook.MaxAttempts)

	var finalErr error

	for attempt := 1; attempt <= maxAttempts; attempt++ {
		result := runHook(ctx, hook, event, payload)
		r.appendHookLedgerRecord(hook, event, hookLedgerRecord{
			Phase:                 LedgerPhaseHookAttempt,
			Attempt:               attempt,
			MaxAttempts:           maxAttempts,
			Outcome:               result.Outcome,
			ErrorSummary:          result.ErrorSummary,
			TimeoutClassification: result.TimeoutClassification,
			PayloadBytes:          len(payload),
			DurationMillis:        result.Duration.Milliseconds(),
			StderrBytes:           result.StderrBytes,
		})

		if result.Err == nil {
			return nil
		}

		finalErr = result.Err

		if attempt < maxAttempts {
			if err := sleepBeforeRetry(ctx, hook.RetryBackoff); err != nil {
				finalErr = err
				break
			}
		}
	}

	r.appendHookLedgerRecord(hook, event, hookLedgerRecord{
		Phase:                 LedgerPhaseHookDeadLetter,
		MaxAttempts:           maxAttempts,
		Outcome:               HookOutcomeDeadLetter,
		ErrorSummary:          safeErrorSummary(finalErr),
		TimeoutClassification: timeoutClassification(finalErr),
		PayloadBytes:          len(payload),
	})

	return finalErr
}

type hookRunResult struct {
	Err                   error
	Outcome               string
	ErrorSummary          string
	TimeoutClassification string
	Duration              time.Duration
	StderrBytes           int
}

func runHook(ctx context.Context, hook Hook, event Event, payload []byte) hookRunResult {
	started := time.Now()

	timeout := hook.Timeout
	if timeout <= 0 {
		timeout = defaultTimeout
	}

	hookCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	var stderr hookStderrCounter

	cmd, invocation, err := shell.CommandContext(hookCtx, shell.CommandOptions{
		Program: hook.Command[0],
		Args:    hook.Command[1:],
		Stdin:   bytes.NewReader(payload),
		Stderr:  &stderr,
		EnvList: hookEnvList(hook, event),
		EnvMode: shell.EnvModeExplicitOnly,
		Policy:  hookPolicy(hook),
		Mode:    shell.ModeCaptured,
		Audit: shell.AuditContext{
			Caller:      "atteler.event_hook." + event.Type,
			SessionID:   event.SessionID,
			SessionPath: event.SessionPath,
		},
		SecretValues: hook.Command,
	})
	if err != nil {
		outcome := HookOutcomeDenied
		classification := ""

		if errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled) {
			outcome = hookOutcomeForContextError(err)
			classification = timeoutClassification(err)
		}

		return hookRunResult{
			Err:                   hookAuthorizationError(hook, err),
			Outcome:               outcome,
			ErrorSummary:          safeErrorSummary(err),
			TimeoutClassification: classification,
			Duration:              time.Since(started),
		}
	}

	runErr := cmd.Run()

	finishErr := invocation.Finish(shell.FinishOptions{
		Error:         runErr,
		OutputCapture: shell.OutputSensitive,
		OutputNote:    "hook stdout/stderr omitted by lifecycle privacy policy",
	})
	if finishErr != nil {
		return hookRunResult{
			Err:          hookAuditError(hook, finishErr),
			Outcome:      HookOutcomeAuditFailed,
			ErrorSummary: safeErrorSummary(finishErr),
			Duration:     time.Since(started),
			StderrBytes:  stderr.Bytes(),
		}
	}

	if hookCtx.Err() != nil {
		err := hookTimeoutError(event, hook, timeout, hookCtx.Err(), stderr.Bytes())

		return hookRunResult{
			Err:                   err,
			Outcome:               hookOutcomeForContextError(hookCtx.Err()),
			ErrorSummary:          hookErrorSummary(hookCtx.Err()),
			TimeoutClassification: timeoutClassification(hookCtx.Err()),
			Duration:              time.Since(started),
			StderrBytes:           stderr.Bytes(),
		}
	}

	if runErr != nil {
		err := hookFailureError(event, hook, runErr, stderr.Bytes())

		return hookRunResult{
			Err:          err,
			Outcome:      HookOutcomeFailed,
			ErrorSummary: hookErrorSummary(runErr),
			Duration:     time.Since(started),
			StderrBytes:  stderr.Bytes(),
		}
	}

	return hookRunResult{
		Outcome:      HookOutcomeSuccess,
		Duration:     time.Since(started),
		StderrBytes:  stderr.Bytes(),
		ErrorSummary: "",
	}
}

func sleepBeforeRetry(ctx context.Context, backoff time.Duration) error {
	if backoff <= 0 {
		backoff = defaultRetryBackoff
	}

	timer := time.NewTimer(backoff)
	defer timer.Stop()

	select {
	case <-timer.C:
		return nil
	case <-ctx.Done():
		return fmt.Errorf("events: retry canceled: %w", ctx.Err())
	}
}

func hookAuthorizationError(hook Hook, err error) error {
	if err == nil {
		return nil
	}

	var policyErr *shell.PolicyError
	if errors.As(err, &policyErr) {
		return fmt.Errorf("events: authorize hook %q: %w", hookAuditCommand(hook.Command), sanitizedPolicyError(policyErr))
	}

	if errors.Is(err, context.DeadlineExceeded) {
		return fmt.Errorf("events: authorize hook %q: %w", hookAuditCommand(hook.Command), context.DeadlineExceeded)
	}

	if errors.Is(err, context.Canceled) {
		return fmt.Errorf("events: authorize hook %q: %w", hookAuditCommand(hook.Command), context.Canceled)
	}

	return fmt.Errorf("events: authorize hook %q: hook authorization failed", hookAuditCommand(hook.Command))
}

func sanitizedPolicyError(err *shell.PolicyError) *shell.PolicyError {
	if err == nil {
		return nil
	}

	rule := strings.TrimSpace(err.Rule)
	if rule != "" {
		rule = sanitizeAuditLabel("policy_rule", rule)
	}

	return &shell.PolicyError{
		Rule:   rule,
		Reason: "command denied by policy",
	}
}

func hookAuditError(hook Hook, err error) error {
	if err == nil {
		return nil
	}

	if errors.Is(err, context.DeadlineExceeded) {
		return fmt.Errorf("events: audit hook %q: %w", hookAuditCommand(hook.Command), context.DeadlineExceeded)
	}

	if errors.Is(err, context.Canceled) {
		return fmt.Errorf("events: audit hook %q: %w", hookAuditCommand(hook.Command), context.Canceled)
	}

	return fmt.Errorf("events: audit hook %q: hook audit failed", hookAuditCommand(hook.Command))
}

func hookFailureError(event Event, hook Hook, err error, stderrBytes int) error {
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) {
		return hookExitError(event, hook, exitErr, stderrBytes)
	}

	if stderrBytes > 0 {
		return fmt.Errorf(
			"events: %s hook %q failed: %s (stderr %d bytes omitted)",
			event.Type,
			hookAuditCommand(hook.Command),
			hookErrorSummary(err),
			stderrBytes,
		)
	}

	return fmt.Errorf("events: %s hook %q failed: %s", event.Type, hookAuditCommand(hook.Command), hookErrorSummary(err))
}

func hookExitError(event Event, hook Hook, err *exec.ExitError, stderrBytes int) error {
	if stderrBytes > 0 {
		return fmt.Errorf(
			"events: %s hook %q failed: %w (stderr %d bytes omitted)",
			event.Type,
			hookAuditCommand(hook.Command),
			err,
			stderrBytes,
		)
	}

	return fmt.Errorf("events: %s hook %q failed: %w", event.Type, hookAuditCommand(hook.Command), err)
}

func hookTimeoutError(event Event, hook Hook, timeout time.Duration, err error, stderrBytes int) error {
	if stderrBytes > 0 {
		return fmt.Errorf(
			"events: %s hook %q timed out after %s: %w (stderr %d bytes omitted)",
			event.Type,
			hookAuditCommand(hook.Command),
			timeout,
			err,
			stderrBytes,
		)
	}

	return fmt.Errorf("events: %s hook %q timed out after %s: %w", event.Type, hookAuditCommand(hook.Command), timeout, err)
}

func hookEnvList(hook Hook, event Event) []string {
	env := eventEnv(event, hook.Env)
	if hook.InheritEnv {
		env = append(filterReservedEventEnv(os.Environ()), env...)
	}

	return env
}

func hookPolicy(hook Hook) *shell.Policy {
	policy := shell.DefaultPolicy()
	policy.AllowCredentialEnv = hookAllowedCredentialEnv(hook)

	return &policy
}

func hookAllowedCredentialEnv(hook Hook) []string {
	if hook.InheritEnv {
		return []string{"*"}
	}

	if len(hook.Env) == 0 {
		return nil
	}

	env := make([]string, 0, len(hook.Env))
	for key := range hook.Env {
		if isReservedEventEnvKey(key) {
			continue
		}

		env = append(env, key)
	}

	return env
}

type hookStderrCounter struct {
	bytes int
}

func (w *hookStderrCounter) Write(p []byte) (int, error) {
	w.bytes += len(p)

	return len(p), nil
}

func (w *hookStderrCounter) Bytes() int {
	return w.bytes
}

func hookErrorSummary(err error) string {
	if err == nil {
		return ""
	}

	if errors.Is(err, context.DeadlineExceeded) {
		return context.DeadlineExceeded.Error()
	}

	if errors.Is(err, context.Canceled) {
		return context.Canceled.Error()
	}

	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) {
		return exitErr.String()
	}

	return "command start failed"
}

func safeErrorSummary(err error) string {
	if err == nil {
		return ""
	}

	if errors.Is(err, context.DeadlineExceeded) {
		return context.DeadlineExceeded.Error()
	}

	if errors.Is(err, context.Canceled) {
		return context.Canceled.Error()
	}

	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) {
		return exitErr.String()
	}

	var policyErr *shell.PolicyError
	if errors.As(err, &policyErr) {
		if policyErr.Rule != "" {
			return "command denied by policy (" + sanitizeAuditLabel("policy_rule", policyErr.Rule) + ")"
		}

		return "command denied by policy"
	}

	message := err.Error()
	switch {
	case strings.Contains(message, "audit hook"):
		return "hook audit failed"
	case strings.Contains(message, "authorize hook"):
		return "hook authorization failed"
	case strings.Contains(message, "retry canceled"):
		return "retry canceled"
	default:
		return "command start failed"
	}
}

func hookOutcomeForContextError(err error) string {
	switch {
	case errors.Is(err, context.DeadlineExceeded):
		return HookOutcomeTimeout
	case errors.Is(err, context.Canceled):
		return HookOutcomeCanceled
	default:
		return HookOutcomeFailed
	}
}

func timeoutClassification(err error) string {
	switch {
	case errors.Is(err, context.DeadlineExceeded):
		return "deadline_exceeded"
	case errors.Is(err, context.Canceled):
		return "canceled"
	default:
		return ""
	}
}

func normalizeMaxAttempts(attempts int) int {
	switch {
	case attempts <= 0:
		return defaultMaxAttempts
	case attempts > maxHookAttempts:
		return maxHookAttempts
	default:
		return attempts
	}
}

func normalizeRetryBackoff(backoffMillis int) time.Duration {
	if backoffMillis <= 0 {
		return defaultRetryBackoff
	}

	maxMillis := int(maxRetryBackoff / time.Millisecond)
	if backoffMillis > maxMillis {
		return maxRetryBackoff
	}

	return time.Duration(backoffMillis) * time.Millisecond
}

func nextEventID(ts time.Time) string {
	if ts.IsZero() {
		ts = time.Now().UTC()
	}

	return fmt.Sprintf("evt_%d_%d", ts.UnixNano(), eventIDCounter.Add(1))
}

func slogWarnHookDelivery(eventType string, hook Hook, err error) {
	if err == nil {
		return
	}

	auditEvent := Event{}
	eventType = sanitizeEventType(&auditEvent, eventType)

	slogAttrs := []any{
		"event_type", eventType,
		"hook_command", hookAuditCommand(hook.Command),
		"delivery", hookDelivery(hook),
		"error", safeErrorSummary(err),
	}
	if classification := timeoutClassification(err); classification != "" {
		slogAttrs = append(slogAttrs, "timeout_classification", classification)
	}

	slog.Warn("lifecycle hook delivery failed", slogAttrs...)
}

func eventEnv(event Event, extra map[string]string) []string {
	env := make([]string, 0, len(extra)+10)
	for key, value := range extra {
		if isReservedEventEnvKey(key) {
			continue
		}

		env = append(env, key+"="+value)
	}

	env = append(env, eventEnvEntry("ATTELER_EVENT_TYPE", event.Type))

	if event.EventID != "" {
		env = append(env, eventEnvEntry("ATTELER_EVENT_ID", event.EventID))
	}

	if event.PayloadMode != "" {
		env = append(env, eventEnvEntry("ATTELER_PAYLOAD_MODE", event.PayloadMode))
	}

	if event.SessionID != "" {
		env = append(env, eventEnvEntry("ATTELER_SESSION_ID", event.SessionID))
	}

	if event.SessionPath != "" {
		env = append(env, eventEnvEntry("ATTELER_SESSION_PATH", event.SessionPath))
	}

	if event.Agent != "" {
		env = append(env, eventEnvEntry("ATTELER_AGENT", event.Agent))
	}

	if event.Model != "" {
		env = append(env, eventEnvEntry("ATTELER_MODEL", event.Model))
	}

	if event.Role != "" {
		env = append(env, eventEnvEntry("ATTELER_ROLE", event.Role))
	}

	if event.Redacted {
		env = append(env, "ATTELER_EVENT_REDACTED=true")
	}

	if event.Truncated {
		env = append(env, "ATTELER_EVENT_TRUNCATED=true")
	}

	if event.SchemaVersion > 0 {
		env = append(env, "ATTELER_EVENT_SCHEMA_VERSION="+strconv.Itoa(event.SchemaVersion))
	}

	if !event.Timestamp.IsZero() {
		env = append(env, "ATTELER_EVENT_UNIX="+strconv.FormatInt(event.Timestamp.Unix(), 10))
	}

	return env
}

func eventEnvEntry(key, value string) string {
	return key + "=" + sanitizeEventEnvValue(value)
}

func sanitizeEventEnvValue(value string) string {
	value = strings.ToValidUTF8(value, "")

	return strings.Map(func(r rune) rune {
		if r == 0 {
			return -1
		}

		if unicode.IsControl(r) {
			return '_'
		}

		return r
	}, value)
}

func filterReservedEventEnv(env []string) []string {
	if len(env) == 0 {
		return nil
	}

	out := make([]string, 0, len(env))
	for _, entry := range env {
		key, _, _ := strings.Cut(entry, "=")
		if isReservedEventEnvKey(key) {
			continue
		}

		out = append(out, entry)
	}

	return out
}

func isReservedEventEnvKey(key string) bool {
	key, _, _ = strings.Cut(key, "=")

	switch {
	case strings.EqualFold(key, "ATTELER_EVENT_TYPE"),
		strings.EqualFold(key, "ATTELER_EVENT_ID"),
		strings.EqualFold(key, "ATTELER_PAYLOAD_MODE"),
		strings.EqualFold(key, "ATTELER_SESSION_ID"),
		strings.EqualFold(key, "ATTELER_SESSION_PATH"),
		strings.EqualFold(key, "ATTELER_AGENT"),
		strings.EqualFold(key, "ATTELER_MODEL"),
		strings.EqualFold(key, "ATTELER_ROLE"),
		strings.EqualFold(key, "ATTELER_EVENT_REDACTED"),
		strings.EqualFold(key, "ATTELER_EVENT_TRUNCATED"),
		strings.EqualFold(key, "ATTELER_EVENT_SCHEMA_VERSION"),
		strings.EqualFold(key, "ATTELER_EVENT_UNIX"):
		return true
	default:
		return false
	}
}

func cloneMap(in map[string]string) map[string]string {
	if len(in) == 0 {
		return nil
	}

	out := make(map[string]string, len(in))
	maps.Copy(out, in)

	return out
}
