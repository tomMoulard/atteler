package events

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"os"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/tommoulard/atteler/pkg/config"
)

func TestRunner_EmitRunsHookWithPayloadAndEnv(t *testing.T) {
	t.Parallel()
	if os.Getenv("ATTELER_TEST_HOOK") == "1" {
		helperHook(t)
		return
	}

	out := t.TempDir() + "/event.json"
	runner := NewRunner(map[string][]config.HookConfig{
		AssistantMessage: {{
			Command: []string{os.Args[0], "-test.run=TestRunner_EmitRunsHookWithPayloadAndEnv"},
			Env: map[string]string{
				"ATTELER_TEST_HOOK": "1",
				"ATTELER_TEST_OUT":  out,
			},
			TimeoutSeconds: 2,
		}},
	})

	err := runner.Emit(context.Background(), Event{
		Type:      AssistantMessage,
		SessionID: "session-1",
		Agent:     "reviewer",
		Model:     "gpt-4.1-mini",
		Role:      "assistant",
		Content:   "hello",
	})
	if err != nil {
		require.NoError(t, err)
	}

	data, err := os.ReadFile(out)
	if err != nil {
		require.NoError(t, err)
	}

	var got Event
	if err := json.Unmarshal(data, &got); err != nil {
		require.NoError(t, err)
	}
	if got.Type != AssistantMessage {
		assert.Failf(t, "assertion failed", "Type = %q", got.Type)
	}
	if got.SessionID != "session-1" {
		assert.Failf(t, "assertion failed", "SessionID = %q", got.SessionID)
	}
	if got.Agent != "reviewer" {
		assert.Failf(t, "assertion failed", "Agent = %q", got.Agent)
	}
}

func TestRunner_EmitNoHooksIsNoop(t *testing.T) {
	t.Parallel()
	if err := NewRunner(nil).Emit(context.Background(), Event{Type: UserMessage}); err != nil {
		require.NoError(t, err)
	}
}

func TestLogger_LogsAnyEvent(t *testing.T) {
	t.Parallel()
	var out bytes.Buffer
	runner := NewRunnerWithLogger(nil, &out)

	err := runner.Emit(context.Background(), Event{
		Type:  FileRead,
		Model: "codex/gpt-5.5",
		Metadata: map[string]string{
			"path": "README.md",
			"kind": "file",
		},
	})
	if err != nil {
		require.NoError(t, err)
	}

	got := out.String()
	for _, want := range []string{"event:file_read", "model=codex/gpt-5.5", "kind=file", "path=README.md"} {
		if !strings.Contains(got, want) {
			require.Failf(t, "unexpected failure", "log line missing %q in %q", want, got)
		}
	}
}

func helperHook(t *testing.T) {
	t.Helper()

	if os.Getenv("ATTELER_EVENT_TYPE") != AssistantMessage {
		require.Failf(t, "unexpected failure", "ATTELER_EVENT_TYPE = %q", os.Getenv("ATTELER_EVENT_TYPE"))
	}
	data, err := io.ReadAll(os.Stdin)
	if err != nil {
		require.NoError(t, err)
	}
	// #nosec G703 -- test helper writes to a temp path supplied by the parent test.
	if err := os.WriteFile(os.Getenv("ATTELER_TEST_OUT"), data, 0o600); err != nil {
		require.NoError(t, err)
	}
	os.Exit(0)
}
