package events

import (
	"context"
	"fmt"
	"io"
	"sort"
	"strings"
)

type contextKey struct{}

type contextEmitter struct {
	base    Event
	emitter *Runner
}

// Logger prints one compact line for every event it receives.
type Logger struct {
	w io.Writer
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
	fmt.Fprintln(l.w, FormatLine(event))
}

// FormatLine formats an event as one human-readable line.
func FormatLine(event Event) string {
	parts := []string{"event:" + event.Type}
	if event.Agent != "" {
		parts = append(parts, "agent="+event.Agent)
	}
	if event.Model != "" {
		parts = append(parts, "model="+event.Model)
	}
	if event.SessionID != "" {
		parts = append(parts, "session="+event.SessionID)
	}
	for _, key := range sortedMetadataKeys(event.Metadata) {
		parts = append(parts, key+"="+quoteValue(event.Metadata[key]))
	}
	if event.Error != "" {
		parts = append(parts, "error="+quoteValue(event.Error))
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
	if strings.ContainsAny(value, " \t\n\r") {
		return fmt.Sprintf("%q", value)
	}
	return value
}
