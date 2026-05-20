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

	"github.com/tommoulard/atteler/pkg/config"
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
	// CommandExecute is emitted when Atteler starts a local command.
	CommandExecute = "command_execute"
	// CommandOutput is emitted when a local command finishes and output is available.
	CommandOutput = "command_output"
	// ToolExecute is emitted when Atteler invokes a provider/tool.
	ToolExecute = "tool_execute"
	// AgentExecute is emitted when a configured agent is selected for work.
	AgentExecute = "agent_execute"

	defaultTimeout = 10 * time.Second
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
	{Type: CommandOutput, Description: "Emitted when a local command finishes and captured output is available."},
	{Type: ContextAdd, Description: "Emitted when a local reference is added to LLM context."},
	{Type: Error, Description: "Emitted when an LLM request or session operation fails."},
	{Type: FileRead, Description: "Emitted when Atteler reads a user or project file."},
	{Type: FileWrite, Description: "Emitted when Atteler writes a local file."},
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
	Metadata    map[string]string `json:"metadata,omitempty"`
	Timestamp   time.Time         `json:"timestamp"`
	Type        string            `json:"type"`
	SessionID   string            `json:"session_id,omitempty"`
	SessionPath string            `json:"session_path,omitempty"`
	Agent       string            `json:"agent,omitempty"`
	Model       string            `json:"model,omitempty"`
	Role        string            `json:"role,omitempty"`
	Content     string            `json:"content,omitempty"`
	Error       string            `json:"error,omitempty"`
}

// Hook is a local command hook for one event type.
type Hook struct {
	Env     map[string]string
	Command []string
	Timeout time.Duration
}

// Runner emits events to configured hooks.
type Runner struct {
	logger *Logger
	hooks  map[string][]Hook
}

// NewRunner creates a hook runner from config.
func NewRunner(configured map[string][]config.HookConfig) *Runner {
	return NewRunnerWithLogger(configured, nil)
}

// NewRunnerWithLogger creates a hook runner and optional built-in event logger.
func NewRunnerWithLogger(configured map[string][]config.HookConfig, logWriter io.Writer) *Runner {
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
				Command: append([]string(nil), cfg.Command...),
				Env:     cloneMap(cfg.Env),
				Timeout: timeout,
			})
		}
	}

	return &Runner{hooks: hooks, logger: NewLogger(logWriter)}
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
				Command: append([]string(nil), hook.Command...),
				Env:     cloneMap(hook.Env),
				Timeout: hook.Timeout,
			})
		}
	}

	return &Runner{hooks: hooks, logger: NewLogger(logWriter)}
}

// Emit sends event to every hook registered for event.Type.
func (r *Runner) Emit(ctx context.Context, event Event) error {
	if r == nil || event.Type == "" {
		return nil
	}

	if event.Timestamp.IsZero() {
		event.Timestamp = time.Now().UTC()
	}

	if r.logger != nil {
		r.logger.Log(event)
	}

	if len(r.hooks) == 0 {
		return nil
	}

	hooks := r.hooks[event.Type]
	if len(hooks) == 0 {
		return nil
	}

	payload, err := json.Marshal(event)
	if err != nil {
		return fmt.Errorf("events: marshal %s: %w", event.Type, err)
	}

	payload = append(payload, '\n')

	var failures []error

	for _, hook := range hooks {
		if err := runHook(ctx, hook, event, payload); err != nil {
			failures = append(failures, err)
		}
	}

	return errors.Join(failures...)
}

func runHook(ctx context.Context, hook Hook, event Event, payload []byte) error {
	timeout := hook.Timeout
	if timeout <= 0 {
		timeout = defaultTimeout
	}

	hookCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	cmd := exec.CommandContext(hookCtx, hook.Command[0], hook.Command[1:]...) //nolint:gosec // commands are explicit user-configured local hooks
	cmd.Stdin = bytes.NewReader(payload)

	cmd.Env = append(os.Environ(), eventEnv(event, hook.Env)...)

	var stderr bytes.Buffer

	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		detail := strings.TrimSpace(stderr.String())
		if detail != "" {
			return fmt.Errorf("events: %s hook %q failed: %w: %s", event.Type, strings.Join(hook.Command, " "), err, detail)
		}

		return fmt.Errorf("events: %s hook %q failed: %w", event.Type, strings.Join(hook.Command, " "), err)
	}

	return nil
}

func eventEnv(event Event, extra map[string]string) []string {
	env := []string{
		"ATTELER_EVENT_TYPE=" + event.Type,
		"ATTELER_SESSION_ID=" + event.SessionID,
		"ATTELER_SESSION_PATH=" + event.SessionPath,
		"ATTELER_AGENT=" + event.Agent,
		"ATTELER_MODEL=" + event.Model,
		"ATTELER_ROLE=" + event.Role,
	}
	if !event.Timestamp.IsZero() {
		env = append(env, "ATTELER_EVENT_UNIX="+strconv.FormatInt(event.Timestamp.Unix(), 10))
	}

	for key, value := range extra {
		env = append(env, key+"="+value)
	}

	return env
}

func cloneMap(in map[string]string) map[string]string {
	if len(in) == 0 {
		return nil
	}

	out := make(map[string]string, len(in))
	maps.Copy(out, in)

	return out
}
