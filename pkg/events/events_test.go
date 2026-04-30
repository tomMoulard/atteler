package events

import (
	"context"
	"encoding/json"
	"io"
	"os"
	"testing"

	"github.com/tommoulard/atteler/pkg/config"
)

func TestRunner_EmitRunsHookWithPayloadAndEnv(t *testing.T) {
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
		t.Fatal(err)
	}

	data, err := os.ReadFile(out)
	if err != nil {
		t.Fatal(err)
	}

	var got Event
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatal(err)
	}
	if got.Type != AssistantMessage {
		t.Errorf("Type = %q", got.Type)
	}
	if got.SessionID != "session-1" {
		t.Errorf("SessionID = %q", got.SessionID)
	}
	if got.Agent != "reviewer" {
		t.Errorf("Agent = %q", got.Agent)
	}
}

func TestRunner_EmitNoHooksIsNoop(t *testing.T) {
	if err := NewRunner(nil).Emit(context.Background(), Event{Type: UserMessage}); err != nil {
		t.Fatal(err)
	}
}

func helperHook(t *testing.T) {
	t.Helper()

	if os.Getenv("ATTELER_EVENT_TYPE") != AssistantMessage {
		t.Fatalf("ATTELER_EVENT_TYPE = %q", os.Getenv("ATTELER_EVENT_TYPE"))
	}
	data, err := io.ReadAll(os.Stdin)
	if err != nil {
		t.Fatal(err)
	}
	// #nosec G703 -- test helper writes to a temp path supplied by the parent test.
	if err := os.WriteFile(os.Getenv("ATTELER_TEST_OUT"), data, 0o600); err != nil {
		t.Fatal(err)
	}
	os.Exit(0)
}
