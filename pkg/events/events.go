// Package events emits atteler lifecycle events to local configured hooks.
package events

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"os"
	"os/exec"
	"strconv"
	"strings"
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

	defaultTimeout = 10 * time.Second
)

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
type Event struct {
	Metadata       map[string]string `json:"metadata,omitempty"`
	Timestamp      time.Time         `json:"timestamp"`
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
	Env         map[string]string
	Command     []string
	Timeout     time.Duration
	PayloadMode PayloadMode
	InheritEnv  bool
}

// Observer receives lifecycle events for best-effort local background work.
// Observer failures must not interrupt the user-facing command or hook flow.
type Observer interface {
	ObserveEvent(context.Context, Event) error
}

// Runner emits events to configured hooks.
type Runner struct {
	logger    *Logger
	hooks     map[string][]Hook
	observers []Observer
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
				Command:     append([]string(nil), cfg.Command...),
				Env:         cloneMap(cfg.Env),
				Timeout:     timeout,
				PayloadMode: normalizePayloadMode(cfg.Payload),
				InheritEnv:  cfg.InheritEnv,
			})
		}
	}

	return &Runner{
		hooks:     hooks,
		logger:    NewLogger(logWriter),
		observers: compactObservers(observers),
	}
}

// WithLogger returns a runner with the same hooks and a new optional logger.
func (r *Runner) WithLogger(logWriter io.Writer) *Runner {
	if r == nil {
		return NewRunnerWithLogger(nil, logWriter)
	}

	hooks := make(map[string][]Hook, len(r.hooks))
	for eventType, configured := range r.hooks {
		for _, hook := range configured {
			hooks[eventType] = append(hooks[eventType], Hook{
				Command:     append([]string(nil), hook.Command...),
				Env:         cloneMap(hook.Env),
				Timeout:     hook.Timeout,
				PayloadMode: hook.PayloadMode,
				InheritEnv:  hook.InheritEnv,
			})
		}
	}

	return &Runner{hooks: hooks, logger: NewLogger(logWriter), observers: append([]Observer(nil), r.observers...)}
}

// Emit sends event to every hook registered for event.Type.
func (r *Runner) Emit(ctx context.Context, event Event) error {
	if r == nil || event.Type == "" {
		return nil
	}

	if ctx == nil {
		return errors.New("events: context is required")
	}

	if err := ctx.Err(); err != nil {
		return fmt.Errorf("events: context already done: %w", err)
	}

	if event.Timestamp.IsZero() {
		event.Timestamp = time.Now().UTC()
	}

	if r.logger != nil {
		r.logger.Log(event)
	}

	r.notifyObservers(ctx, event)

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

		if err := runHook(ctx, hook, hookEvent, payload); err != nil {
			failures = append(failures, err)
		}
	}

	return errors.Join(failures...)
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

func runHook(ctx context.Context, hook Hook, event Event, payload []byte) error {
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
		Command: strings.Join(hook.Command, " "),
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
	})
	if err != nil {
		return fmt.Errorf("events: authorize hook %q: %w", hookAuditCommand(hook.Command), err)
	}

	runErr := cmd.Run()

	finishErr := invocation.Finish(shell.FinishOptions{
		Error:         runErr,
		OutputCapture: shell.OutputSensitive,
		OutputNote:    "hook stdout/stderr omitted by lifecycle privacy policy",
	})
	if finishErr != nil {
		return fmt.Errorf("events: audit hook %q: %w", hookAuditCommand(hook.Command), finishErr)
	}

	if runErr != nil {
		if stderr.Bytes() > 0 {
			return fmt.Errorf(
				"events: %s hook %q failed: %s (stderr %d bytes omitted)",
				event.Type,
				hookAuditCommand(hook.Command),
				hookErrorSummary(runErr),
				stderr.Bytes(),
			)
		}

		return fmt.Errorf("events: %s hook %q failed: %s", event.Type, hookAuditCommand(hook.Command), hookErrorSummary(runErr))
	}

	return nil
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

func eventEnv(event Event, extra map[string]string) []string {
	env := make([]string, 0, len(extra)+10)
	for key, value := range extra {
		if isReservedEventEnvKey(key) {
			continue
		}

		env = append(env, key+"="+value)
	}

	env = append(env, eventEnvEntry("ATTELER_EVENT_TYPE", event.Type))

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
		strings.EqualFold(key, "ATTELER_PAYLOAD_MODE"),
		strings.EqualFold(key, "ATTELER_SESSION_ID"),
		strings.EqualFold(key, "ATTELER_SESSION_PATH"),
		strings.EqualFold(key, "ATTELER_AGENT"),
		strings.EqualFold(key, "ATTELER_MODEL"),
		strings.EqualFold(key, "ATTELER_ROLE"),
		strings.EqualFold(key, "ATTELER_EVENT_REDACTED"),
		strings.EqualFold(key, "ATTELER_EVENT_TRUNCATED"),
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
