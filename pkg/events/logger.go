package events

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"sort"
	"strings"
	"unicode"
)

type contextKey struct{}

type contextEmitter struct {
	emitter *Runner
	base    Event
}

// Logger prints one compact line for every event it receives.
type Logger struct {
	w io.Writer
}

type flushWriter interface {
	Flush() error
}

// NewLogger creates a logger. A nil writer disables logging.
func NewLogger(w io.Writer) *Logger {
	if w == nil {
		return nil
	}

	return &Logger{w: w}
}

// Log writes a single line for event.
func (l *Logger) Log(event Event) {
	if l == nil || l.w == nil || event.Type == "" {
		return
	}

	event = sanitizeEventForLog(event)

	// Also emit to slog for structured logging consumers.
	attrs := []any{
		slog.String("event_type", event.Type),
	}

	if event.Agent != "" {
		attrs = append(attrs, slog.String("agent", event.Agent))
	}

	if event.Model != "" {
		attrs = append(attrs, slog.String("model", event.Model))
	}

	if event.SessionID != "" {
		attrs = append(attrs, slog.String("session_id", event.SessionID))
	}

	if event.ErrorSummary != "" {
		attrs = append(attrs, slog.String("error_summary", event.ErrorSummary))
	}

	if event.Redacted {
		attrs = append(attrs, slog.Bool("redacted", true))
	}

	if event.Truncated {
		attrs = append(attrs, slog.Bool("truncated", true))
	}

	for _, key := range sortedMetadataKeys(event.Metadata) {
		attrs = append(attrs, slog.String(key, event.Metadata[key]))
	}

	slog.Debug("lifecycle event", attrs...)

	fmt.Fprintln(l.w, formatLine(event))

	if flusher, ok := l.w.(flushWriter); ok {
		_ = flusher.Flush()
	}
}

// FormatLine formats an event as one human-readable line.
func FormatLine(event Event) string {
	return formatLine(sanitizeEventForLog(event))
}

func formatLine(event Event) string {
	parts := []string{"event:" + event.Type}
	if event.Agent != "" {
		parts = append(parts, "agent="+quoteValue(event.Agent))
	}

	if event.Model != "" {
		parts = append(parts, "model="+quoteValue(event.Model))
	}

	if event.SessionID != "" {
		parts = append(parts, "session="+quoteValue(event.SessionID))
	}

	for _, key := range sortedMetadataKeys(event.Metadata) {
		parts = append(parts, key+"="+quoteValue(event.Metadata[key]))
	}

	if event.ErrorSummary != "" {
		parts = append(parts, "error="+quoteValue(event.ErrorSummary))
	}

	if event.Redacted {
		parts = append(parts, "redacted=true")
	}

	if event.Truncated {
		parts = append(parts, "truncated=true")
	}

	return strings.Join(parts, " ")
}

// WithEmitter stores an event runner plus default event fields in ctx.
func WithEmitter(ctx context.Context, emitter *Runner, base Event) context.Context {
	if emitter == nil {
		return ctx
	}

	return context.WithValue(ctx, contextKey{}, contextEmitter{emitter: emitter, base: base})
}

// EmitFromContext emits an event through the runner stored by WithEmitter.
func EmitFromContext(ctx context.Context, event Event) error {
	value, ok := ctx.Value(contextKey{}).(contextEmitter)
	if !ok || value.emitter == nil {
		return nil
	}

	event = mergeBase(value.base, event)

	return value.emitter.Emit(ctx, event)
}

// EmitFromContextBestEffort emits an event through the runner stored by
// WithEmitter. If ctx is already canceled, it still writes to the local event
// logger, but it does not run configured hooks because hook execution must
// respect caller cancellation.
func EmitFromContextBestEffort(ctx context.Context, event Event) error {
	if ctx == nil {
		return errors.New("events: context is required")
	}

	value, ok := ctx.Value(contextKey{}).(contextEmitter)
	if !ok || value.emitter == nil {
		return nil
	}

	event = mergeBase(value.base, event)

	return value.emitter.emitBestEffort(ctx, event)
}

func (r *Runner) emitBestEffort(ctx context.Context, event Event) error {
	if r == nil || event.Type == "" {
		return nil
	}

	if err := ctx.Err(); err != nil {
		if r.logger != nil {
			r.logger.Log(event)
		}

		return fmt.Errorf("events: context already done: %w", err)
	}

	return r.Emit(ctx, event)
}

func mergeBase(base, event Event) Event {
	if event.SessionID == "" {
		event.SessionID = base.SessionID
	}

	if event.SessionPath == "" {
		event.SessionPath = base.SessionPath
	}

	if event.Agent == "" {
		event.Agent = base.Agent
	}

	if event.Model == "" {
		event.Model = base.Model
	}

	return event
}

func sortedMetadataKeys(metadata map[string]string) []string {
	keys := make([]string, 0, len(metadata))
	for key := range metadata {
		keys = append(keys, key)
	}

	sort.Strings(keys)

	return keys
}

func quoteValue(value string) string {
	if value == "" {
		return `""`
	}

	if strings.ContainsAny(value, " \t\n\r") || containsControlRune(value) {
		return fmt.Sprintf("%q", value)
	}

	return value
}

func containsControlRune(value string) bool {
	for _, r := range value {
		if unicode.IsControl(r) {
			return true
		}
	}

	return false
}
