package main

import (
	"bytes"
	"context"
	"log/slog"
	"os"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/tommoulard/atteler/pkg/autonomy"
	appconfig "github.com/tommoulard/atteler/pkg/config"
	"github.com/tommoulard/atteler/pkg/events"
)

func TestEmitHookLineIncludesRunnerDefaultAutonomy(t *testing.T) {
	t.Parallel()

	msg := emitHook(
		context.Background(),
		events.NewRunner(nil).WithAutonomy(autonomy.High),
		events.Event{Type: events.CommandExecute},
	)()

	hook, ok := msg.(hookMsg)
	require.True(t, ok)
	require.NoError(t, hook.err)
	assert.Contains(t, hook.line, "autonomy=high")
}

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
			Blocking:       true,
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

func TestCloseHookRunnerFlushesWithCanceledParentContext(t *testing.T) {
	t.Parallel()

	markerPath := t.TempDir() + "/hook-delivered.txt"
	runner := events.NewRunner(map[string][]appconfig.HookConfig{
		events.UserMessage: {{
			Command: []string{os.Args[0], "-test.run=^TestCloseHookRunnerMarkerHelperProcess$"},
			Env: map[string]string{
				"ATTELER_TEST_CLOSE_HOOK_HELPER": "1",
				"ATTELER_TEST_CLOSE_HOOK_MARKER": markerPath,
			},
			TimeoutSeconds: 10,
		}},
	})

	ctx, cancel := context.WithCancel(t.Context())
	require.NoError(t, runner.Emit(ctx, events.Event{Type: events.UserMessage}))
	cancel()

	closeHookRunner(ctx, runner)

	data, err := os.ReadFile(markerPath)
	require.NoError(t, err)
	assert.Equal(t, "delivered", string(data))
}

//nolint:paralleltest // Helper-process sentinel exits the child test binary.
func TestCloseHookRunnerMarkerHelperProcess(t *testing.T) {
	if os.Getenv("ATTELER_TEST_CLOSE_HOOK_HELPER") != "1" {
		return
	}

	markerPath := os.Getenv("ATTELER_TEST_CLOSE_HOOK_MARKER")
	require.NotEmpty(t, markerPath)
	require.NoError(t, os.WriteFile(markerPath, []byte("delivered"), 0o600)) //nolint:gosec // Test helper writes to a temp path supplied by the parent test.
}
