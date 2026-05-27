package main

import (
	"bytes"
	"context"
	"log/slog"
	"testing"

	"github.com/stretchr/testify/assert"

	appconfig "github.com/tommoulard/atteler/pkg/config"
	"github.com/tommoulard/atteler/pkg/events"
)

//nolint:paralleltest // Mutates the process-global slog default.
func TestEmitFromContextWarningRedactsEventType(t *testing.T) {
	var out bytes.Buffer

	original := slog.Default()

	slog.SetDefault(slog.New(slog.NewTextHandler(&out, &slog.HandlerOptions{Level: slog.LevelDebug})))
	t.Cleanup(func() {
		slog.SetDefault(original)
	})

	eventType := "custom_sk-contextwarnsecret1234567890"
	runner := events.NewRunner(map[string][]appconfig.HookConfig{
		eventType: {{
			Command:        []string{"sk-missinghooksecret1234567890"},
			TimeoutSeconds: 2,
		}},
	})
	ctx := events.WithEmitter(context.Background(), runner, events.Event{})

	emitFromContextWarning(ctx, events.Event{
		Type:    eventType,
		Content: "raw prompt content",
	})

	got := out.String()
	assert.Contains(t, got, "emit hook from context")
	assert.Contains(t, got, "event:custom_[redacted]")
	assert.NotContains(t, got, "sk-contextwarnsecret")
	assert.NotContains(t, got, "sk-missinghooksecret")
	assert.NotContains(t, got, "raw prompt content")
}
