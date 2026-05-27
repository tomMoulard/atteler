package events

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"os"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/tommoulard/atteler/pkg/config"
)

type recordingObserver struct {
	err    error
	events []Event
}

func (o *recordingObserver) ObserveEvent(_ context.Context, event Event) error {
	o.events = append(o.events, event)

	return o.err
}

type mutatingObserver struct{}

func (mutatingObserver) ObserveEvent(_ context.Context, event Event) error {
	event.Metadata["command"] = "mutated"

	return nil
}

type panickingObserver struct{}

func (panickingObserver) ObserveEvent(context.Context, Event) error {
	panic("observer failed")
}

func TestRunner_EmitRunsHookWithPayloadAndEnv(t *testing.T) {
	t.Parallel()

	out := t.TempDir() + "/event.json"
	runner := NewRunner(map[string][]config.HookConfig{
		AssistantMessage: {{
			Command: []string{os.Args[0], "-test.run=^TestHookHelperProcess$"},
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

	assert.Empty(t, got.Content, "default hook payload must not include assistant text")
	assert.Equal(t, string(PayloadMetadata), got.PayloadMode)
	assert.True(t, got.Redacted, "omitting content should mark the payload redacted")
}

func TestRunner_EmitUsesLeastPrivilegeEnvByDefault(t *testing.T) {
	t.Setenv("ATTELER_PARENT_SECRET", "do-not-inherit")

	dir := t.TempDir()
	payloadOut := dir + "/event.json"
	envOut := dir + "/env.txt"

	runner := NewRunner(map[string][]config.HookConfig{
		UserMessage: {{
			Command: []string{os.Args[0], "-test.run=^TestHookHelperProcess$"},
			Env: map[string]string{
				"ATTELER_TEST_HOOK":    "1",
				"ATTELER_TEST_OUT":     payloadOut,
				"ATTELER_TEST_ENV_OUT": envOut,
				"CUSTOM_HOOK_ENV":      "configured",
			},
			TimeoutSeconds: 2,
		}},
	})

	err := runner.Emit(context.Background(), Event{
		Type:        UserMessage,
		SessionID:   "session-1",
		SessionPath: "/Users/example/private/session.json",
		Role:        "user",
		Content:     "prompt with token=sk-parentsecret1234567890",
	})
	require.NoError(t, err)

	envData, err := os.ReadFile(envOut)
	require.NoError(t, err)

	env := strings.Split(strings.TrimSpace(string(envData)), "\n")

	assert.Contains(t, env, "ATTELER_EVENT_TYPE=user_message")
	assert.Contains(t, env, "ATTELER_PAYLOAD_MODE=metadata")
	assert.Contains(t, env, "ATTELER_SESSION_ID=session-1")
	assert.Contains(t, env, "ATTELER_EVENT_REDACTED=true")
	assert.Contains(t, env, "CUSTOM_HOOK_ENV=configured")
	assert.NotContains(t, env, "ATTELER_PARENT_SECRET=do-not-inherit")
	assert.False(t, slices.ContainsFunc(env, func(value string) bool {
		return strings.HasPrefix(value, "PATH=") || strings.HasPrefix(value, "HOME=")
	}), "default hook environment must not inherit ambient PATH or HOME")
	assert.False(t, slices.ContainsFunc(env, func(value string) bool {
		return strings.HasPrefix(value, "ATTELER_SESSION_PATH=")
	}), "default payload/env must not expose session paths")
}

func TestRunner_EmitProtectsReservedEventEnvKeys(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	payloadOut := dir + "/event.json"
	envOut := dir + "/env.txt"

	runner := NewRunner(map[string][]config.HookConfig{
		UserMessage: {{
			Command: []string{os.Args[0], "-test.run=^TestHookHelperProcess$"},
			Env: map[string]string{
				"ATTELER_TEST_HOOK":    "1",
				"ATTELER_TEST_OUT":     payloadOut,
				"ATTELER_TEST_ENV_OUT": envOut,
				"ATTELER_EVENT_TYPE":   "sk-envtypesecret1234567890",
				"ATTELER_PAYLOAD_MODE": string(PayloadFull),
				"ATTELER_SESSION_ID":   "sk-envsessionsecret1234567890",
				"ATTELER_SESSION_PATH": "/Users/example/private/session.json",
			},
			TimeoutSeconds: 2,
		}},
	})

	err := runner.Emit(context.Background(), Event{
		Type:        UserMessage,
		SessionID:   "session-1",
		SessionPath: "/Users/example/private/actual-session.json",
		Content:     "prompt text",
	})
	require.NoError(t, err)

	envData, err := os.ReadFile(envOut)
	require.NoError(t, err)

	env := strings.Split(strings.TrimSpace(string(envData)), "\n")
	assert.Contains(t, env, "ATTELER_EVENT_TYPE=user_message")
	assert.Contains(t, env, "ATTELER_PAYLOAD_MODE=metadata")
	assert.Contains(t, env, "ATTELER_SESSION_ID=session-1")
	assert.False(t, slices.ContainsFunc(env, func(value string) bool {
		return strings.HasPrefix(value, "ATTELER_SESSION_PATH=")
	}), "reserved event env keys must come only from sanitized event data")

	got := string(envData)
	for _, leaked := range []string{"sk-envtypesecret", "sk-envsessionsecret", "/Users/example/private"} {
		assert.NotContains(t, got, leaked)
	}
}

func TestRunner_EmitInheritEnvFiltersReservedEventEnvKeys(t *testing.T) {
	t.Setenv("ATTELER_EVENT_TYPE", "sk-parenteventsecret1234567890")
	t.Setenv("ATTELER_PAYLOAD_MODE", string(PayloadFull))
	t.Setenv("ATTELER_SESSION_ID", "sk-parentsessionsecret1234567890")
	t.Setenv("ATTELER_SESSION_PATH", "/Users/example/private/session.json")
	t.Setenv("ATTELER_EVENT_REDACTED", "false")

	dir := t.TempDir()
	payloadOut := dir + "/event.json"
	envOut := dir + "/env.txt"

	runner := NewRunner(map[string][]config.HookConfig{
		UserMessage: {{
			Command: []string{os.Args[0], "-test.run=^TestHookHelperProcess$"},
			Env: map[string]string{
				"ATTELER_TEST_HOOK":    "1",
				"ATTELER_TEST_OUT":     payloadOut,
				"ATTELER_TEST_ENV_OUT": envOut,
			},
			InheritEnv:     true,
			TimeoutSeconds: 2,
		}},
	})

	err := runner.Emit(context.Background(), Event{
		Type:        UserMessage,
		SessionID:   "session-1",
		SessionPath: "/Users/example/private/actual-session.json",
		Content:     "prompt text",
	})
	require.NoError(t, err)

	envData, err := os.ReadFile(envOut)
	require.NoError(t, err)

	env := strings.Split(strings.TrimSpace(string(envData)), "\n")
	assert.Contains(t, env, "ATTELER_EVENT_TYPE=user_message")
	assert.Contains(t, env, "ATTELER_PAYLOAD_MODE=metadata")
	assert.Contains(t, env, "ATTELER_SESSION_ID=session-1")
	assert.Contains(t, env, "ATTELER_EVENT_REDACTED=true")
	assert.False(t, slices.ContainsFunc(env, func(value string) bool {
		return strings.HasPrefix(value, "ATTELER_SESSION_PATH=")
	}), "inherited reserved event env keys must be filtered before sanitized event env is added")

	got := string(envData)
	for _, leaked := range []string{"sk-parenteventsecret", "sk-parentsessionsecret", "/Users/example/private"} {
		assert.NotContains(t, got, leaked)
	}
}

func TestRunner_EmitRedactsSecretEventTypeInPayloadAndEnv(t *testing.T) {
	t.Parallel()

	eventType := "custom_sk-eventenvsecret1234567890"
	dir := t.TempDir()
	payloadOut := dir + "/event.json"
	envOut := dir + "/env.txt"

	runner := NewRunner(map[string][]config.HookConfig{
		eventType: {{
			Command: []string{os.Args[0], "-test.run=^TestHookHelperProcess$"},
			Env: map[string]string{
				"ATTELER_TEST_HOOK":    "1",
				"ATTELER_TEST_OUT":     payloadOut,
				"ATTELER_TEST_ENV_OUT": envOut,
			},
			TimeoutSeconds: 2,
		}},
	})

	err := runner.Emit(context.Background(), Event{
		Type:    eventType,
		Content: "raw prompt text",
	})
	require.NoError(t, err)

	payload := readHookEvent(t, payloadOut)
	assert.Contains(t, payload.Type, redactedValue)
	assert.NotContains(t, payload.Type, "sk-eventenvsecret")
	assert.Empty(t, payload.Content)
	assert.True(t, payload.Redacted)

	envData, err := os.ReadFile(envOut)
	require.NoError(t, err)

	env := strings.Split(strings.TrimSpace(string(envData)), "\n")
	assert.Contains(t, env, "ATTELER_EVENT_TYPE=custom_"+redactedValue)
	assert.NotContains(t, string(envData), "sk-eventenvsecret")
}

func TestRunner_EmitRedactsSecretScalarsInEnv(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	payloadOut := dir + "/event.json"
	envOut := dir + "/env.txt"

	runner := NewRunner(map[string][]config.HookConfig{
		UserMessage: {{
			Command: []string{os.Args[0], "-test.run=^TestHookHelperProcess$"},
			Env: map[string]string{
				"ATTELER_TEST_HOOK":    "1",
				"ATTELER_TEST_OUT":     payloadOut,
				"ATTELER_TEST_ENV_OUT": envOut,
			},
			TimeoutSeconds: 2,
		}},
	})

	err := runner.Emit(context.Background(), Event{
		Type:      UserMessage,
		SessionID: "session-sk-envsessionsecret1234567890",
		Agent:     "agent-sk-envagentsecret1234567890",
		Model:     "model-sk-envmodelsecret1234567890",
		Role:      "user-sk-envrolesecret1234567890",
	})
	require.NoError(t, err)

	envData, err := os.ReadFile(envOut)
	require.NoError(t, err)

	got := string(envData)
	for _, want := range []string{
		"ATTELER_SESSION_ID=session-" + redactedValue,
		"ATTELER_AGENT=agent-" + redactedValue,
		"ATTELER_MODEL=model-" + redactedValue,
		"ATTELER_ROLE=user-" + redactedValue,
		"ATTELER_EVENT_REDACTED=true",
	} {
		assert.Contains(t, got, want)
	}

	for _, leaked := range []string{"sk-envsessionsecret", "sk-envagentsecret", "sk-envmodelsecret", "sk-envrolesecret"} {
		assert.NotContains(t, got, leaked)
	}
}

func TestRunner_EmitRedactsSecretSessionPathInFullEnvAndPayload(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	payloadOut := dir + "/event.json"
	envOut := dir + "/env.txt"

	runner := NewRunner(map[string][]config.HookConfig{
		UserMessage: {{
			Command: []string{os.Args[0], "-test.run=^TestHookHelperProcess$"},
			Payload: string(PayloadFull),
			Env: map[string]string{
				"ATTELER_TEST_HOOK":    "1",
				"ATTELER_TEST_OUT":     payloadOut,
				"ATTELER_TEST_ENV_OUT": envOut,
			},
			TimeoutSeconds: 2,
		}},
	})

	err := runner.Emit(context.Background(), Event{
		Type:        UserMessage,
		SessionPath: "/tmp/session-sk-envpathsecret1234567890.json",
		Content:     "hello",
	})
	require.NoError(t, err)

	payload := readHookEvent(t, payloadOut)
	assert.Equal(t, "/tmp/session-"+redactedValue+".json", payload.SessionPath)
	assert.True(t, payload.Redacted)
	assert.NotContains(t, string(readFile(t, payloadOut)), "sk-envpathsecret")

	envData, err := os.ReadFile(envOut)
	require.NoError(t, err)

	got := string(envData)
	assert.Contains(t, got, "ATTELER_SESSION_PATH=/tmp/session-"+redactedValue+".json")
	assert.Contains(t, got, "ATTELER_EVENT_REDACTED=true")
	assert.NotContains(t, got, "sk-envpathsecret")
}

func TestRunner_EmitBoundsScalarsInEnv(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	payloadOut := dir + "/event.json"
	envOut := dir + "/env.txt"

	runner := NewRunner(map[string][]config.HookConfig{
		UserMessage: {{
			Command: []string{os.Args[0], "-test.run=^TestHookHelperProcess$"},
			Env: map[string]string{
				"ATTELER_TEST_HOOK":    "1",
				"ATTELER_TEST_OUT":     payloadOut,
				"ATTELER_TEST_ENV_OUT": envOut,
			},
			TimeoutSeconds: 2,
		}},
	})

	err := runner.Emit(context.Background(), Event{
		Type:      UserMessage,
		SessionID: strings.Repeat("session", maxScalarBytes),
	})
	require.NoError(t, err)

	envData, err := os.ReadFile(envOut)
	require.NoError(t, err)

	sessionEnv := findEnvValue(t, strings.Split(strings.TrimSpace(string(envData)), "\n"), "ATTELER_SESSION_ID")
	assert.LessOrEqual(t, len(sessionEnv), maxScalarBytes)
	assert.Contains(t, sessionEnv, truncationMarker)
	assert.Contains(t, string(envData), "ATTELER_EVENT_TRUNCATED=true")
}

func TestEventEnvSanitizesGeneratedValues(t *testing.T) {
	t.Parallel()

	env := eventEnv(Event{
		Type:        "custom\nkind",
		PayloadMode: "metadata\nmode",
		SessionID:   "session\nid",
		SessionPath: "/tmp/a\rb",
		Agent:       "agent\tname",
		Model:       "model\x00name",
		Role:        "role\nname",
		Redacted:    true,
	}, nil)

	expected := map[string]string{
		"ATTELER_EVENT_TYPE":   "custom_kind",
		"ATTELER_PAYLOAD_MODE": "metadata_mode",
		"ATTELER_SESSION_ID":   "session_id",
		"ATTELER_SESSION_PATH": "/tmp/a_b",
		"ATTELER_AGENT":        "agent_name",
		"ATTELER_MODEL":        "modelname",
		"ATTELER_ROLE":         "role_name",
	}

	for key, want := range expected {
		got := findEnvValue(t, env, key)
		assert.Equal(t, want, got)
		assert.NotContains(t, got, "\x00")
		assert.NotContains(t, got, "\n")
		assert.NotContains(t, got, "\r")
		assert.NotContains(t, got, "\t")
	}
}

func TestRunner_EmitCanOptIntoInheritedEnv(t *testing.T) {
	t.Setenv("ATTELER_PARENT_SECRET", "inherit-when-requested")

	dir := t.TempDir()
	payloadOut := dir + "/event.json"
	envOut := dir + "/env.txt"

	runner := NewRunner(map[string][]config.HookConfig{
		SessionStart: {{
			Command: []string{os.Args[0], "-test.run=^TestHookHelperProcess$"},
			Env: map[string]string{
				"ATTELER_TEST_HOOK":    "1",
				"ATTELER_TEST_OUT":     payloadOut,
				"ATTELER_TEST_ENV_OUT": envOut,
			},
			InheritEnv:     true,
			TimeoutSeconds: 2,
		}},
	})

	err := runner.Emit(context.Background(), Event{Type: SessionStart})
	require.NoError(t, err)

	envData, err := os.ReadFile(envOut)
	require.NoError(t, err)

	env := strings.Split(strings.TrimSpace(string(envData)), "\n")
	assert.Contains(t, env, "ATTELER_PARENT_SECRET=inherit-when-requested")
}

func TestRunner_EmitCanOptIntoSummaryAndFullPayloads(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	summaryOut := dir + "/summary.json"
	fullOut := dir + "/full.json"

	runner := NewRunner(map[string][]config.HookConfig{
		CommandOutput: {
			{
				Command: []string{os.Args[0], "-test.run=^TestHookHelperProcess$"},
				Payload: string(PayloadSummary),
				Env: map[string]string{
					"ATTELER_TEST_HOOK": "1",
					"ATTELER_TEST_OUT":  summaryOut,
				},
				TimeoutSeconds: 2,
			},
			{
				Command: []string{os.Args[0], "-test.run=^TestHookHelperProcess$"},
				Payload: string(PayloadFull),
				Env: map[string]string{
					"ATTELER_TEST_HOOK": "1",
					"ATTELER_TEST_OUT":  fullOut,
				},
				TimeoutSeconds: 2,
			},
		},
	})

	err := runner.Emit(context.Background(), Event{
		Type:        CommandOutput,
		SessionID:   "session-1",
		SessionPath: "/Users/example/private/session.json",
		Content:     "raw command output with OPENAI_API_KEY=sk-outputsecret1234567890",
		Error:       "failed with bearer abcdefghijklmnopqrstuvwxyz",
		Metadata: map[string]string{
			"command": "cat ~/.env",
			"cwd":     "/Users/example/project",
			"source":  "llm_tool",
		},
	})
	require.NoError(t, err)

	summary := readHookEvent(t, summaryOut)
	assert.Equal(t, string(PayloadSummary), summary.PayloadMode)
	assert.Empty(t, summary.Content)
	assert.Empty(t, summary.Error)
	assert.NotEmpty(t, summary.ContentSummary)
	assert.NotEmpty(t, summary.ErrorSummary)
	assert.Equal(t, "llm_tool", summary.Metadata["source"])
	assert.NotContains(t, string(readFile(t, summaryOut)), "raw command output")
	assert.NotContains(t, string(readFile(t, summaryOut)), "sk-outputsecret")
	assert.NotContains(t, string(readFile(t, summaryOut)), "/Users/example")

	full := readHookEvent(t, fullOut)
	fullJSON := string(readFile(t, fullOut))
	assert.Equal(t, string(PayloadFull), full.PayloadMode)
	assert.Contains(t, full.Content, "raw command output")
	assert.Contains(t, full.Content, redactedValue)
	assert.Contains(t, full.Error, "bearer "+redactedValue)
	assert.Equal(t, "cat ~/.env", full.Metadata["command"])
	assert.Equal(t, "/Users/example/project", full.Metadata["cwd"])
	assert.Equal(t, "/Users/example/private/session.json", full.SessionPath)
	assert.NotContains(t, fullJSON, "sk-outputsecret")
	assert.NotContains(t, fullJSON, "abcdefghijklmnopqrstuvwxyz")
}

func TestRunner_WithLoggerPreservesHookPrivacyConfig(t *testing.T) {
	t.Setenv("ATTELER_PARENT_SECRET", "preserved-inherited-env")

	dir := t.TempDir()
	payloadOut := dir + "/event.json"
	envOut := dir + "/env.txt"

	runner := NewRunner(map[string][]config.HookConfig{
		CommandOutput: {{
			Command: []string{os.Args[0], "-test.run=^TestHookHelperProcess$"},
			Payload: string(PayloadFull),
			Env: map[string]string{
				"ATTELER_TEST_HOOK":    "1",
				"ATTELER_TEST_OUT":     payloadOut,
				"ATTELER_TEST_ENV_OUT": envOut,
			},
			InheritEnv:     true,
			TimeoutSeconds: 2,
		}},
	}).WithLogger(io.Discard)

	err := runner.Emit(context.Background(), Event{
		Type:    CommandOutput,
		Content: "raw output token=sk-withloggerpayloadsecret1234567890",
	})
	require.NoError(t, err)

	payload := readHookEvent(t, payloadOut)
	assert.Equal(t, string(PayloadFull), payload.PayloadMode)
	assert.Contains(t, payload.Content, "raw output token="+redactedValue)
	assert.NotContains(t, string(readFile(t, payloadOut)), "sk-withloggerpayloadsecret")

	envData, err := os.ReadFile(envOut)
	require.NoError(t, err)

	env := strings.Split(strings.TrimSpace(string(envData)), "\n")
	assert.Contains(t, env, "ATTELER_PAYLOAD_MODE=full")
	assert.Contains(t, env, "ATTELER_PARENT_SECRET=preserved-inherited-env")
}

func TestRunner_EmitInvalidPayloadModeDefaultsToMetadataWithoutLeakingValue(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	payloadOut := dir + "/event.json"
	envOut := dir + "/env.txt"

	runner := NewRunner(map[string][]config.HookConfig{
		AssistantMessage: {{
			Command: []string{os.Args[0], "-test.run=^TestHookHelperProcess$"},
			Payload: "sk-payloadmodesecret1234567890",
			Env: map[string]string{
				"ATTELER_TEST_HOOK":    "1",
				"ATTELER_TEST_OUT":     payloadOut,
				"ATTELER_TEST_ENV_OUT": envOut,
			},
			TimeoutSeconds: 2,
		}},
	})

	err := runner.Emit(context.Background(), Event{
		Type:    AssistantMessage,
		Content: "raw assistant content",
	})
	require.NoError(t, err)

	payloadData := string(readFile(t, payloadOut))
	payload := readHookEvent(t, payloadOut)
	assert.Equal(t, string(PayloadMetadata), payload.PayloadMode)
	assert.Empty(t, payload.Content)
	assert.True(t, payload.Redacted)
	assert.NotContains(t, payloadData, "sk-payloadmodesecret")
	assert.NotContains(t, payloadData, "raw assistant content")

	envData, err := os.ReadFile(envOut)
	require.NoError(t, err)

	env := strings.Split(strings.TrimSpace(string(envData)), "\n")
	assert.Contains(t, env, "ATTELER_PAYLOAD_MODE=metadata")
	assert.NotContains(t, string(envData), "sk-payloadmodesecret")
}

func TestRunner_EmitBoundsFullPayloadFields(t *testing.T) {
	t.Parallel()

	out := t.TempDir() + "/event.json"
	runner := NewRunner(map[string][]config.HookConfig{
		AssistantMessage: {{
			Command: []string{os.Args[0], "-test.run=^TestHookHelperProcess$"},
			Payload: string(PayloadFull),
			Env: map[string]string{
				"ATTELER_TEST_HOOK": "1",
				"ATTELER_TEST_OUT":  out,
			},
			TimeoutSeconds: 2,
		}},
	})

	err := runner.Emit(context.Background(), Event{
		Type:    AssistantMessage,
		Content: strings.Repeat("x", maxContentBytes*2),
	})
	require.NoError(t, err)

	got := readHookEvent(t, out)
	assert.LessOrEqual(t, len(got.Content), maxContentBytes)
	assert.True(t, got.Truncated)
}

func TestRunner_EmitNoHooksIsNoop(t *testing.T) {
	t.Parallel()

	if err := NewRunner(nil).Emit(context.Background(), Event{Type: UserMessage}); err != nil {
		require.NoError(t, err)
	}
}

func TestRunner_EmitNotifiesObserversWithoutInterrupting(t *testing.T) {
	t.Parallel()

	observer := &recordingObserver{err: assert.AnError}
	err := NewRunnerWithLoggerAndObservers(nil, nil, observer).Emit(context.Background(), Event{
		Type:    CommandExecute,
		Content: "go test ./pkg/events",
	})
	require.NoError(t, err)
	require.Len(t, observer.events, 1)
	require.Equal(t, CommandExecute, observer.events[0].Type)
}

func TestRunner_ObserversCannotMutateCallerEvent(t *testing.T) {
	t.Parallel()

	metadata := map[string]string{"command": "go test ./pkg/events"}
	err := NewRunnerWithLoggerAndObservers(nil, nil, mutatingObserver{}).Emit(context.Background(), Event{
		Type:     CommandExecute,
		Metadata: metadata,
	})
	require.NoError(t, err)
	require.Equal(t, "go test ./pkg/events", metadata["command"])
}

func TestRunner_ObserverPanicDoesNotInterrupt(t *testing.T) {
	t.Parallel()

	observer := &recordingObserver{}
	err := NewRunnerWithLoggerAndObservers(nil, nil, panickingObserver{}, observer).Emit(context.Background(), Event{
		Type:    CommandExecute,
		Content: "go test ./pkg/events",
	})
	require.NoError(t, err)
	require.Len(t, observer.events, 1)
}

func TestRunner_EmitRequiresContext(t *testing.T) {
	t.Parallel()

	err := NewRunner(map[string][]config.HookConfig{
		UserMessage: {{
			Command: []string{"echo", "unused"},
		}},
	}).Emit(nil, Event{Type: UserMessage}) //nolint:staticcheck // Verify nil contexts are rejected instead of panicking.
	require.Error(t, err)
	assert.Contains(t, err.Error(), "context is required")
}

func TestRunner_EmitRejectsCanceledContext(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	err := NewRunner(map[string][]config.HookConfig{
		UserMessage: {{
			Command: []string{"echo", "unused"},
		}},
	}).Emit(ctx, Event{Type: UserMessage})
	require.Error(t, err)
	require.ErrorIs(t, err, context.Canceled)
}

func TestRunner_EmitHookFailureOmitsStderrAndCommandArgs(t *testing.T) {
	t.Parallel()

	err := NewRunner(map[string][]config.HookConfig{
		UserMessage: {{
			Command: []string{os.Args[0], "-test.run=^TestFailingHookHelperProcess$", "--token=sk-argsecret1234567890"},
			Env: map[string]string{
				"ATTELER_TEST_FAIL_HOOK": "1",
			},
			TimeoutSeconds: 2,
		}},
	}).Emit(context.Background(), Event{Type: UserMessage})
	require.Error(t, err)

	got := err.Error()
	assert.Contains(t, got, "user_message")
	assert.Contains(t, got, "stderr")
	assert.Contains(t, got, "bytes omitted")
	assert.NotContains(t, got, "--token")
	assert.NotContains(t, got, "sk-argsecret")
	assert.NotContains(t, got, "sk-stderrsecret")
	assert.NotContains(t, got, "hook stderr secret")
}

func TestRunner_EmitHookFailureCountsStderrWithoutLeakingContent(t *testing.T) {
	t.Parallel()

	stderrText := strings.Repeat("hook stderr secret sk-stderrsecret1234567890\n", 32)

	err := NewRunner(map[string][]config.HookConfig{
		UserMessage: {{
			Command: []string{os.Args[0], "-test.run=^TestFailingHookHelperProcess$"},
			Env: map[string]string{
				"ATTELER_TEST_FAIL_HOOK":   "1",
				"ATTELER_TEST_FAIL_STDERR": stderrText,
			},
			TimeoutSeconds: 2,
		}},
	}).Emit(context.Background(), Event{Type: UserMessage})
	require.Error(t, err)

	got := err.Error()
	assert.Contains(t, got, fmt.Sprintf("stderr %d bytes omitted", len(stderrText)))
	assert.NotContains(t, got, "sk-stderrsecret")
	assert.NotContains(t, got, "hook stderr secret")
}

func TestRunner_EmitHookFailureRedactsSecretEventType(t *testing.T) {
	t.Parallel()

	eventType := "custom_sk-hookfailureeventsecret1234567890"
	err := NewRunner(map[string][]config.HookConfig{
		eventType: {{
			Command: []string{os.Args[0], "-test.run=^TestFailingHookHelperProcess$"},
			Env: map[string]string{
				"ATTELER_TEST_FAIL_HOOK": "1",
			},
			TimeoutSeconds: 2,
		}},
	}).Emit(context.Background(), Event{Type: eventType})
	require.Error(t, err)

	got := err.Error()
	assert.Contains(t, got, "custom_"+redactedValue)
	assert.NotContains(t, got, "sk-hookfailureeventsecret")
}

func TestRunner_EmitHookStartFailureOmitsCommandPathAndArgs(t *testing.T) {
	t.Parallel()

	err := NewRunner(map[string][]config.HookConfig{
		UserMessage: {{
			Command:        []string{"sk-missingsecret1234567890-hook", "--token=sk-argsecret1234567890"},
			TimeoutSeconds: 2,
		}},
	}).Emit(context.Background(), Event{Type: UserMessage})
	require.Error(t, err)

	got := err.Error()
	assert.Contains(t, got, "user_message")
	assert.Contains(t, got, "command start failed")
	assert.NotContains(t, got, "sk-missingsecret")
	assert.NotContains(t, got, "--token")
	assert.NotContains(t, got, "sk-argsecret")
}

func TestRunner_EmitMarshalFailureRedactsEventType(t *testing.T) {
	t.Parallel()

	eventType := "custom_sk-marshalerrorsecret1234567890"
	err := NewRunner(map[string][]config.HookConfig{
		eventType: {{
			Command:        []string{"unused"},
			TimeoutSeconds: 2,
		}},
	}).Emit(context.Background(), Event{
		Type:      eventType,
		Timestamp: time.Date(10000, 1, 1, 0, 0, 0, 0, time.UTC),
	})
	require.Error(t, err)

	got := err.Error()
	assert.Contains(t, got, "events: marshal")
	assert.Contains(t, got, redactedValue)
	assert.NotContains(t, got, "sk-marshalerrorsecret")
}

func TestLogger_LogsAnyEvent(t *testing.T) {
	t.Parallel()

	var out bytes.Buffer

	runner := NewRunnerWithLogger(nil, &out)

	err := runner.Emit(context.Background(), Event{
		Type:  FileRead,
		Model: "codex/gpt-5.5",
		Metadata: map[string]string{
			"path":      "/Users/example/private/README.md",
			"kind":      "file",
			"bytes":     "10",
			"api_token": "sk-logsecret1234567890",
		},
		Error: "raw error with OPENAI_API_KEY=sk-errorsecret1234567890",
	})
	if err != nil {
		require.NoError(t, err)
	}

	got := out.String()
	for _, want := range []string{"event:file_read", "model=codex/gpt-5.5", "kind=file", "bytes=10", "redacted=true"} {
		if !strings.Contains(got, want) {
			require.Failf(t, "unexpected failure", "log line missing %q in %q", want, got)
		}
	}

	for _, leaked := range []string{"README.md", "raw error", "sk-logsecret", "sk-errorsecret", "api_token"} {
		assert.NotContains(t, got, leaked)
	}
}

func TestFormatLine_RedactsContentErrorAndArbitraryMetadata(t *testing.T) {
	t.Parallel()

	got := FormatLine(Event{
		Type:    CommandOutput,
		Content: "raw command output with OPENAI_API_KEY=sk-contentsecret1234567890",
		Error:   "raw error with bearer abcdefghijklmnopqrstuvwxyz",
		Metadata: map[string]string{
			"command":   "cat ~/.env",
			"cwd":       "/Users/example/project",
			"source":    "llm_tool",
			"api_token": "sk-metasecret1234567890",
		},
	})

	for _, want := range []string{"event:command_output", "source=llm_tool", "error=", "redacted=true"} {
		assert.Contains(t, got, want)
	}

	for _, leaked := range []string{
		"raw command output",
		"raw error",
		"sk-contentsecret",
		"abcdefghijklmnopqrstuvwxyz",
		"cat ~/.env",
		"/Users/example",
		"api_token",
		"sk-metasecret",
	} {
		assert.NotContains(t, got, leaked)
	}
}

func TestFormatLine_RedactsSecretLookingAllowlistedMetadataValues(t *testing.T) {
	t.Parallel()

	got := FormatLine(Event{
		Type: CommandOutput,
		Metadata: map[string]string{
			"source": "tool token=sk-sourcemetasecret1234567890",
		},
	})

	assert.Contains(t, got, "event:command_output")
	assert.Contains(t, got, "source=")
	assert.Contains(t, got, redactedValue)
	assert.Contains(t, got, "redacted=true")
	assert.NotContains(t, got, "sk-sourcemetasecret")
}

func TestFormatLine_RedactsSecretLookingScalarFields(t *testing.T) {
	t.Parallel()

	got := FormatLine(Event{
		Type:      AgentExecute,
		SessionID: "session-sk-sessionsecret1234567890",
		Agent:     "agent-sk-agentsecret1234567890",
		Model:     "model-sk-modelsecret1234567890",
	})

	assert.Contains(t, got, "event:agent_execute")
	assert.Contains(t, got, "agent=agent-"+redactedValue)
	assert.Contains(t, got, "model=model-"+redactedValue)
	assert.Contains(t, got, "session=session-"+redactedValue)
	assert.Contains(t, got, "redacted=true")
	assert.NotContains(t, got, "sk-sessionsecret")
	assert.NotContains(t, got, "sk-agentsecret")
	assert.NotContains(t, got, "sk-modelsecret")
}

func TestFormatLine_QuotesSanitizedScalarFields(t *testing.T) {
	t.Parallel()

	got := FormatLine(Event{
		Type:      AgentExecute,
		SessionID: "session\nnext=field",
		Agent:     "review agent",
		Model:     "model\tname",
	})

	assert.Contains(t, got, `agent="review agent"`)
	assert.Contains(t, got, `model="model\tname"`)
	assert.Contains(t, got, `session="session\nnext=field"`)
	assert.NotContains(t, got, "\nnext=field")
	assert.NotContains(t, got, "agent=review agent")
}

func TestFormatLine_QuotesNonWhitespaceControlCharacters(t *testing.T) {
	t.Parallel()

	got := FormatLine(Event{
		Type:  ToolExecute,
		Model: "model\x00name",
		Metadata: map[string]string{
			"provider": "codex\x1b[31m",
			"tool":     "llm.complete",
		},
	})

	assert.Contains(t, got, `model="model\x00name"`)
	assert.Contains(t, got, `provider="codex\x1b[31m"`)
	assert.NotContains(t, got, "\x00")
	assert.NotContains(t, got, "\x1b")
}

func TestFormatLine_DropsProvidedContentSummary(t *testing.T) {
	t.Parallel()

	got := FormatLine(Event{
		Type:           AssistantMessage,
		ContentSummary: "raw prompt summary with token=sk-contentsummarysecret1234567890",
	})

	assert.Contains(t, got, "event:assistant_message")
	assert.Contains(t, got, "redacted=true")
	assert.NotContains(t, got, "raw prompt summary")
	assert.NotContains(t, got, "sk-contentsummarysecret")
}

func TestLogger_RedactsProvidedErrorSummary(t *testing.T) {
	t.Parallel()

	var out bytes.Buffer

	NewLogger(&out).Log(Event{
		Type:         Error,
		ErrorSummary: "upstream returned token=sk-summarysecret1234567890",
	})

	got := out.String()
	assert.Contains(t, got, "event:error")
	assert.Contains(t, got, "error=")
	assert.Contains(t, got, "redacted=true")
	assert.NotContains(t, got, "upstream returned")
	assert.NotContains(t, got, "sk-summarysecret")
}

func TestLogger_LogDoesNotDoubleSummarizeRawError(t *testing.T) {
	t.Parallel()

	var out bytes.Buffer

	NewLogger(&out).Log(Event{
		Type:  Error,
		Error: strings.Repeat("x", 123),
	})

	got := out.String()
	assert.Contains(t, got, "event:error")
	assert.Contains(t, got, "error=\"error redacted bytes=123\"")
	assert.NotContains(t, got, "bytes=24")
}

//nolint:paralleltest // Mutates the process-global slog default.
func TestLogger_SlogRedactsSensitiveFields(t *testing.T) {
	var out bytes.Buffer

	original := slog.Default()

	slog.SetDefault(slog.New(slog.NewTextHandler(&out, &slog.HandlerOptions{Level: slog.LevelDebug})))
	t.Cleanup(func() {
		slog.SetDefault(original)
	})

	NewLogger(io.Discard).Log(Event{
		Type:    CommandOutput,
		Content: "raw command output with OPENAI_API_KEY=sk-contentsecret1234567890",
		Error:   "raw error with bearer abcdefghijklmnopqrstuvwxyz",
		Metadata: map[string]string{
			"command":   "cat ~/.env",
			"cwd":       "/Users/example/project",
			"source":    "llm_tool",
			"api_token": "sk-metasecret1234567890",
		},
	})

	got := out.String()
	for _, want := range []string{"lifecycle event", "event_type=command_output", "source=llm_tool", "error_summary=", "redacted=true"} {
		assert.Contains(t, got, want)
	}

	for _, leaked := range []string{
		"raw command output",
		"raw error",
		"sk-contentsecret",
		"abcdefghijklmnopqrstuvwxyz",
		"cat ~/.env",
		"/Users/example",
		"api_token",
		"sk-metasecret",
	} {
		assert.NotContains(t, got, leaked)
	}
}

//nolint:paralleltest // Mutates the process-global slog default.
func TestLogger_SlogRedactsSecretLookingScalarFields(t *testing.T) {
	var out bytes.Buffer

	original := slog.Default()

	slog.SetDefault(slog.New(slog.NewTextHandler(&out, &slog.HandlerOptions{Level: slog.LevelDebug})))
	t.Cleanup(func() {
		slog.SetDefault(original)
	})

	NewLogger(io.Discard).Log(Event{
		Type:      AgentExecute,
		SessionID: "session-sk-slogsessionsecret1234567890",
		Agent:     "agent-sk-slogagentsecret1234567890",
		Model:     "model-sk-slogmodelsecret1234567890",
	})

	got := out.String()
	for _, want := range []string{"lifecycle event", "event_type=agent_execute", redactedValue, "redacted=true"} {
		assert.Contains(t, got, want)
	}

	for _, leaked := range []string{"sk-slogsessionsecret", "sk-slogagentsecret", "sk-slogmodelsecret"} {
		assert.NotContains(t, got, leaked)
	}
}

//nolint:paralleltest // Mutates the process-global slog default.
func TestHookAuditLogOmitsPayloadAndEnvValues(t *testing.T) {
	var out bytes.Buffer

	original := slog.Default()

	slog.SetDefault(slog.New(slog.NewTextHandler(&out, &slog.HandlerOptions{Level: slog.LevelDebug})))
	t.Cleanup(func() {
		slog.SetDefault(original)
	})

	logHookInvocation(Event{
		Type:    CommandExecute,
		Content: "OPENAI_API_KEY=sk-payloadsecret1234567890",
		Metadata: map[string]string{
			"command": "cat ~/.env",
			"cwd":     "/Users/example/project",
		},
	}, Hook{
		Command:     []string{"/usr/local/bin/notify-hook", "--token", "sk-argsecret1234567890"},
		Env:         map[string]string{"OPENAI_API_KEY": "sk-envsecret1234567890"},
		PayloadMode: PayloadFull,
		InheritEnv:  true,
	}, 123)

	got := out.String()
	for _, want := range []string{"lifecycle hook invocation", "event_type=command_execute", "notify-hook", "payload_mode=full", "payload_bytes=123", "inherit_env=true", "env_keys=1"} {
		assert.Contains(t, got, want)
	}

	for _, leaked := range []string{"sk-payloadsecret", "cat ~/.env", "/Users/example", "--token", "sk-argsecret", "sk-envsecret", "OPENAI_API_KEY"} {
		assert.NotContains(t, got, leaked)
	}
}

//nolint:paralleltest // Mutates the process-global slog default.
func TestHookAuditLogRedactsEventTypeAndNormalizesPayloadMode(t *testing.T) {
	var out bytes.Buffer

	original := slog.Default()

	slog.SetDefault(slog.New(slog.NewTextHandler(&out, &slog.HandlerOptions{Level: slog.LevelDebug})))
	t.Cleanup(func() {
		slog.SetDefault(original)
	})

	logHookInvocation(Event{
		Type:        "custom/sk-auditeventsecret1234567890\nroot",
		PayloadMode: "sk-auditpayloadsecret1234567890",
	}, Hook{
		Command: []string{"/usr/local/bin/notify-hook"},
	}, 123)

	got := out.String()
	assert.Contains(t, got, "event_type=custom_"+redactedValue+"_root")
	assert.Contains(t, got, "payload_mode=metadata")
	assert.NotContains(t, got, "custom/sk")
	assert.NotContains(t, got, "sk-auditeventsecret")
	assert.NotContains(t, got, "sk-auditpayloadsecret")
}

func TestHookAuditCommandRedactsAndNormalizesBasename(t *testing.T) {
	t.Parallel()

	got := hookAuditCommand([]string{"/tmp/notify hook\nsk-hookcommandsecret1234567890", "--token=sk-argsecret1234567890"})

	assert.Equal(t, "notify_hook_"+redactedValue+" (+1 args)", got)
	assert.NotContains(t, got, "/tmp")
	assert.NotContains(t, got, "notify hook")
	assert.NotContains(t, got, "\n")
	assert.NotContains(t, got, "sk-hookcommandsecret")
	assert.NotContains(t, got, "--token")
	assert.NotContains(t, got, "sk-argsecret")

	windowsPath := hookAuditCommand([]string{`C:\Users\sk-dirsecret1234567890\hooks\notify-hook`, "--token=sk-argsecret1234567890"})

	assert.Equal(t, "notify-hook (+1 args)", windowsPath)
	assert.NotContains(t, windowsPath, "Users")
	assert.NotContains(t, windowsPath, "sk-dirsecret")
	assert.NotContains(t, windowsPath, "--token")

	urlCommand := hookAuditCommand([]string{"https://user:plain-password@example.com", "--token=sk-argsecret1234567890"})

	assert.Contains(t, urlCommand, redactedValue)
	assert.NotContains(t, urlCommand, "user:plain-password")
	assert.NotContains(t, urlCommand, "--token")
	assert.NotContains(t, urlCommand, "sk-argsecret")
}

//nolint:paralleltest // Helper-process sentinel exits the child test binary.
func TestHookHelperProcess(t *testing.T) {
	if os.Getenv("ATTELER_TEST_HOOK") != "1" {
		return
	}

	helperHook(t)
}

//nolint:paralleltest // Helper-process sentinel exits the child test binary.
func TestFailingHookHelperProcess(_ *testing.T) {
	if os.Getenv("ATTELER_TEST_FAIL_HOOK") != "1" {
		return
	}

	stderrText := os.Getenv("ATTELER_TEST_FAIL_STDERR")
	if stderrText == "" {
		stderrText = "hook stderr secret sk-stderrsecret1234567890\n"
	}

	_, _ = os.Stderr.WriteString(stderrText)

	os.Exit(2)
}

func TestLogger_FlushesBufferedWriter(t *testing.T) {
	t.Parallel()

	var out flushBuffer

	NewLogger(&out).Log(Event{Type: CommandOutput})

	assert.Equal(t, 1, out.flushes)
	assert.Contains(t, out.String(), "event:command_output")
}

type flushBuffer struct {
	bytes.Buffer
	flushes int
}

func (b *flushBuffer) Flush() error {
	b.flushes++

	return nil
}

func helperHook(t *testing.T) {
	t.Helper()

	data, err := io.ReadAll(os.Stdin)
	if err != nil {
		require.NoError(t, err)
	}
	// #nosec G703 -- test helper writes to a temp path supplied by the parent test.
	if err := os.WriteFile(os.Getenv("ATTELER_TEST_OUT"), data, 0o600); err != nil {
		require.NoError(t, err)
	}

	if envOut := os.Getenv("ATTELER_TEST_ENV_OUT"); envOut != "" {
		env := selectedEnv(
			"ATTELER_EVENT_TYPE",
			"ATTELER_PAYLOAD_MODE",
			"ATTELER_SESSION_ID",
			"ATTELER_SESSION_PATH",
			"ATTELER_AGENT",
			"ATTELER_MODEL",
			"ATTELER_ROLE",
			"ATTELER_EVENT_REDACTED",
			"ATTELER_EVENT_TRUNCATED",
			"CUSTOM_HOOK_ENV",
			"ATTELER_PARENT_SECRET",
			"PATH",
			"HOME",
		)
		// #nosec G703 -- test helper writes to a temp path supplied by the parent test.
		if err := os.WriteFile(envOut, []byte(strings.Join(env, "\n")), 0o600); err != nil {
			require.NoError(t, err)
		}
	}

	os.Exit(0)
}

func selectedEnv(keys ...string) []string {
	var out []string

	for _, key := range keys {
		if value, ok := os.LookupEnv(key); ok {
			out = append(out, key+"="+value)
		}
	}

	return out
}

func findEnvValue(t *testing.T, env []string, key string) string {
	t.Helper()

	prefix := key + "="
	for _, entry := range env {
		if value, ok := strings.CutPrefix(entry, prefix); ok {
			return value
		}
	}

	require.Failf(t, "missing environment variable", "%s not found in %v", key, env)

	return ""
}

func readHookEvent(t *testing.T, path string) Event {
	t.Helper()

	var got Event
	require.NoError(t, json.Unmarshal(readFile(t, path), &got))

	return got
}

func readFile(t *testing.T, path string) []byte {
	t.Helper()

	data, err := os.ReadFile(path)
	require.NoError(t, err)

	return data
}
