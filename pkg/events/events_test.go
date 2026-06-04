package events

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/tommoulard/atteler/pkg/config"
	"github.com/tommoulard/atteler/pkg/shell"
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

func TestDetachContextPreservesValuesWithoutCancellation(t *testing.T) {
	t.Parallel()

	type contextKey string

	ctx, cancel := context.WithCancel(context.WithValue(t.Context(), contextKey("key"), "value"))
	cancel()

	detached := DetachContext(ctx)

	assert.Equal(t, "value", detached.Value(contextKey("key")))
	assert.Nil(t, detached.Done())
	assert.NoError(t, detached.Err())
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
			TimeoutSeconds: 10,
			Blocking:       true,
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

	assert.Equal(t, EventSchemaVersion, got.SchemaVersion)
	assert.NotEmpty(t, got.EventID)
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
			TimeoutSeconds: 10,
			Blocking:       true,
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
	assert.True(t, slices.ContainsFunc(env, func(value string) bool {
		return strings.HasPrefix(value, "ATTELER_EVENT_ID=evt_")
	}), "hook env should include a generated event id")
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
				"ATTELER_EVENT_ID":     "sk-envidsecret1234567890",
				"ATTELER_PAYLOAD_MODE": string(PayloadFull),
				"ATTELER_SESSION_ID":   "sk-envsessionsecret1234567890",
				"ATTELER_SESSION_PATH": "/Users/example/private/session.json",
			},
			TimeoutSeconds: 10,
			Blocking:       true,
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
	for _, leaked := range []string{"sk-envtypesecret", "sk-envidsecret", "sk-envsessionsecret", "/Users/example/private"} {
		assert.NotContains(t, got, leaked)
	}
}

func TestRunner_EmitInheritEnvFiltersReservedEventEnvKeys(t *testing.T) {
	t.Setenv("ATTELER_EVENT_TYPE", "sk-parenteventsecret1234567890")
	t.Setenv("ATTELER_EVENT_ID", "sk-parentidsecret1234567890")
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
			TimeoutSeconds: 10,
			Blocking:       true,
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
	for _, leaked := range []string{"sk-parenteventsecret", "sk-parentidsecret", "sk-parentsessionsecret", "/Users/example/private"} {
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
			TimeoutSeconds: 10,
			Blocking:       true,
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

func TestRunner_EmitRedactsSecretEventIDInPayloadEnvAndLedger(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	payloadOut := dir + "/event.json"
	envOut := dir + "/env.txt"

	var ledger bytes.Buffer

	runner := NewRunnerWithOptions(map[string][]config.HookConfig{
		UserMessage: {{
			Command: []string{os.Args[0], "-test.run=^TestHookHelperProcess$"},
			Env: map[string]string{
				"ATTELER_TEST_HOOK":    "1",
				"ATTELER_TEST_OUT":     payloadOut,
				"ATTELER_TEST_ENV_OUT": envOut,
			},
			TimeoutSeconds: 10,
			Blocking:       true,
		}},
	}, RunnerOptions{LedgerWriter: &ledger})

	err := runner.Emit(context.Background(), Event{
		Type:    UserMessage,
		EventID: "event-sk-eventidsecret1234567890",
		Content: "raw prompt text",
	})
	require.NoError(t, err)

	payload := readHookEvent(t, payloadOut)
	assert.Equal(t, "event-"+redactedValue, payload.EventID)
	assert.Empty(t, payload.Content)
	assert.True(t, payload.Redacted)

	envData, err := os.ReadFile(envOut)
	require.NoError(t, err)

	env := strings.Split(strings.TrimSpace(string(envData)), "\n")
	assert.Contains(t, env, "ATTELER_EVENT_ID=event-"+redactedValue)

	records := readLedgerRecords(t, ledger.String())
	require.Len(t, records, 2)
	require.NotNil(t, records[0].Event)
	assert.Equal(t, "event-"+redactedValue, records[0].Event.EventID)
	require.NotNil(t, records[1].Hook)
	assert.Equal(t, "event-"+redactedValue, records[1].Hook.EventID)

	for _, got := range []string{string(readFile(t, payloadOut)), string(envData), ledger.String()} {
		assert.NotContains(t, got, "sk-eventidsecret")
		assert.NotContains(t, got, "raw prompt text")
	}
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
			TimeoutSeconds: 10,
			Blocking:       true,
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
			TimeoutSeconds: 10,
			Blocking:       true,
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
			TimeoutSeconds: 10,
			Blocking:       true,
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
		EventID:     "event\nid",
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
		"ATTELER_EVENT_ID":     "event_id",
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
			TimeoutSeconds: 10,
			Blocking:       true,
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
				TimeoutSeconds: 10,
				Blocking:       true,
			},
			{
				Command: []string{os.Args[0], "-test.run=^TestHookHelperProcess$"},
				Payload: string(PayloadFull),
				Env: map[string]string{
					"ATTELER_TEST_HOOK": "1",
					"ATTELER_TEST_OUT":  fullOut,
				},
				TimeoutSeconds: 10,
				Blocking:       true,
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
			TimeoutSeconds: 10,
			Blocking:       true,
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

func TestRunner_WithLoggerSharesPendingNonBlockingDeliveries(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	markerPath := dir + "/delivered.txt"

	appCtx, cancelApp := context.WithCancel(t.Context())
	defer cancelApp()

	runner := NewRunnerWithOptions(map[string][]config.HookConfig{
		UserMessage: {{
			Command: []string{os.Args[0], "-test.run=^TestDelayedHookHelperProcess$"},
			Env: map[string]string{
				"ATTELER_TEST_DELAY_HOOK": "1",
				"ATTELER_TEST_MARKER_OUT": markerPath,
				"ATTELER_TEST_DELAY_MS":   "100",
			},
			TimeoutSeconds: 10,
		}},
	}, RunnerOptions{DeliveryContext: appCtx})

	withLogger := runner.WithLogger(io.Discard)

	err := withLogger.Emit(context.Background(), Event{Type: UserMessage})
	require.NoError(t, err)

	waitCtx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	require.NoError(t, runner.Wait(waitCtx))

	data, err := os.ReadFile(markerPath)
	require.NoError(t, err)
	assert.Equal(t, "delivered", strings.TrimSpace(string(data)))
}

func TestRunner_NonBlockingHookSurvivesCallerCancellation(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	markerPath := dir + "/delivered.txt"

	runner := NewRunner(map[string][]config.HookConfig{
		UserMessage: {{
			Command: []string{os.Args[0], "-test.run=^TestDelayedHookHelperProcess$"},
			Env: map[string]string{
				"ATTELER_TEST_DELAY_HOOK": "1",
				"ATTELER_TEST_MARKER_OUT": markerPath,
				"ATTELER_TEST_DELAY_MS":   "100",
			},
			TimeoutSeconds: 10,
		}},
	})

	emitCtx, cancelEmit := context.WithCancel(t.Context())
	require.NoError(t, runner.Emit(emitCtx, Event{Type: UserMessage}))
	cancelEmit()

	waitCtx, cancelWait := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancelWait()

	require.NoError(t, runner.Wait(waitCtx))

	data, err := os.ReadFile(markerPath)
	require.NoError(t, err)
	assert.Equal(t, "delivered", strings.TrimSpace(string(data)))
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
			TimeoutSeconds: 10,
			Blocking:       true,
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
			TimeoutSeconds: 10,
			Blocking:       true,
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

func TestRunner_EmitShrinksOversizedFullPayload(t *testing.T) {
	t.Parallel()

	out := t.TempDir() + "/event.json"
	runner := NewRunner(map[string][]config.HookConfig{
		RouteDecision: {{
			Command: []string{os.Args[0], "-test.run=^TestHookHelperProcess$"},
			Payload: string(PayloadFull),
			Env: map[string]string{
				"ATTELER_TEST_HOOK": "1",
				"ATTELER_TEST_OUT":  out,
			},
			TimeoutSeconds: 10,
			Blocking:       true,
		}},
	})

	metadata := make(map[string]string)

	for key, policy := range eventSchemas[RouteDecision].Metadata {
		if policy == metadataSafe {
			metadata[key] = strings.Repeat("x", maxMetadataValueBytes)
		}
	}

	err := runner.Emit(context.Background(), Event{
		Type:     RouteDecision,
		Content:  strings.Repeat("y", maxContentBytes),
		Metadata: metadata,
	})
	require.NoError(t, err)

	data := readFile(t, out)
	assert.LessOrEqual(t, len(data), maxHookPayloadBytes)

	got := readHookEvent(t, out)
	assert.Empty(t, got.Content)
	assert.Empty(t, got.Metadata)
	assert.Contains(t, got.ContentSummary, "payload_limit=true")
	assert.True(t, got.Redacted)
	assert.True(t, got.Truncated)
}

func TestRunner_EmitNoHooksIsNoop(t *testing.T) {
	t.Parallel()

	if err := NewRunner(nil).Emit(context.Background(), Event{Type: UserMessage}); err != nil {
		require.NoError(t, err)
	}
}

func TestRunner_EmitPolicyDeniedHookDeadLettersWithoutRawArgs(t *testing.T) {
	t.Parallel()

	var ledger bytes.Buffer

	runner := NewRunnerWithOptions(map[string][]config.HookConfig{
		UserMessage: {{
			Command:  []string{"rm", "-rf", "/", "--token=sk-pathsecret1234567890"},
			Blocking: true,
		}},
	}, RunnerOptions{LedgerWriter: &ledger})

	err := runner.Emit(context.Background(), Event{Type: UserMessage})
	require.Error(t, err)

	var policyErr *shell.PolicyError
	require.ErrorAs(t, err, &policyErr)

	records := readLedgerRecords(t, ledger.String())
	require.Len(t, records, 3)
	assert.Equal(t, []string{
		LedgerPhaseEvent,
		LedgerPhaseHookAttempt,
		LedgerPhaseHookDeadLetter,
	}, ledgerPhases(records))
	assert.Equal(t, HookOutcomeDenied, records[1].Outcome)
	assert.Equal(t, "command denied by policy (destructive.deny)", records[1].ErrorSummary)
	assert.Equal(t, HookOutcomeDeadLetter, records[2].Outcome)

	got := ledger.String()
	assert.Contains(t, got, `"command":"rm (+3 args)"`)
	assert.NotContains(t, got, "sk-pathsecret")
	assert.NotContains(t, got, "--token")
	assert.NotContains(t, got, "-rf")
}

func TestRunner_EmitNonBlockingHookFailureDeadLettersWithoutInterrupting(t *testing.T) {
	t.Parallel()

	var ledger bytes.Buffer

	runner := NewRunnerWithOptions(map[string][]config.HookConfig{
		UserMessage: {{
			Command: []string{os.Args[0], "-test.run=^TestFailingHookHelperProcess$", "--token=sk-argsecret1234567890"},
			Env: map[string]string{
				"ATTELER_TEST_FAIL_HOOK": "1",
			},
			TimeoutSeconds: 10,
		}},
	}, RunnerOptions{LedgerWriter: &ledger})

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	err := runner.Emit(ctx, Event{
		Type:      UserMessage,
		SessionID: "session-1",
		Content:   "prompt with token=sk-nonblockingpayloadsecret1234567890",
	})
	require.NoError(t, err)
	require.NoError(t, runner.Wait(ctx))

	records := readLedgerRecords(t, ledger.String())
	require.Len(t, records, 4)
	assert.Equal(t, []string{
		LedgerPhaseEvent,
		LedgerPhaseHookQueued,
		LedgerPhaseHookAttempt,
		LedgerPhaseHookDeadLetter,
	}, ledgerPhases(records))

	attempt := records[2]
	require.NotNil(t, attempt.Hook)
	assert.Equal(t, records[0].Event.EventID, attempt.Hook.EventID)
	assert.Equal(t, UserMessage, attempt.Hook.EventType)
	assert.Equal(t, "session-1", attempt.Hook.SessionID)
	assert.Equal(t, HookDeliveryNonBlocking, attempt.Hook.Delivery)
	assert.Equal(t, HookOutcomeFailed, attempt.Outcome)
	assert.Equal(t, 1, attempt.Attempt)
	assert.Equal(t, 1, attempt.MaxAttempts)
	assert.Equal(t, "exit status 2", attempt.ErrorSummary)

	deadLetter := records[3]
	assert.Equal(t, HookOutcomeDeadLetter, deadLetter.Outcome)
	assert.Equal(t, "exit status 2", deadLetter.ErrorSummary)

	got := ledger.String()
	for _, leaked := range []string{"sk-nonblockingpayloadsecret", "sk-argsecret", "sk-stderrsecret", "hook stderr secret"} {
		assert.NotContains(t, got, leaked)
	}
}

func TestRunner_EmitTelemetryFailureDoesNotBreakBlockingHookSuccess(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	successOut := dir + "/blocking-success.json"

	var ledger bytes.Buffer

	runner := NewRunnerWithOptions(map[string][]config.HookConfig{
		UserMessage: {
			{
				Command: []string{os.Args[0], "-test.run=^TestHookHelperProcess$"},
				Env: map[string]string{
					"ATTELER_TEST_HOOK": "1",
					"ATTELER_TEST_OUT":  successOut,
				},
				TimeoutSeconds: 10,
				Blocking:       true,
			},
			{
				Command: []string{os.Args[0], "-test.run=^TestFailingHookHelperProcess$"},
				Env: map[string]string{
					"ATTELER_TEST_FAIL_HOOK": "1",
				},
				TimeoutSeconds: 10,
			},
		},
	}, RunnerOptions{LedgerWriter: &ledger})

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	require.NoError(t, runner.Emit(ctx, Event{Type: UserMessage, Content: "prompt with token=sk-mixedhooksecret1234567890"}))
	require.NoError(t, runner.Wait(ctx))

	payload := readHookEvent(t, successOut)
	assert.Equal(t, UserMessage, payload.Type)
	assert.Empty(t, payload.Content)

	records := readLedgerRecords(t, ledger.String())

	var blockingSuccess, telemetryDeadLetter bool

	for i := range records {
		if records[i].Phase == LedgerPhaseHookAttempt &&
			records[i].Hook != nil &&
			records[i].Hook.Delivery == HookDeliveryBlocking &&
			records[i].Outcome == HookOutcomeSuccess {
			blockingSuccess = true
		}

		if records[i].Phase == LedgerPhaseHookDeadLetter &&
			records[i].Hook != nil &&
			records[i].Hook.Delivery == HookDeliveryNonBlocking &&
			records[i].Outcome == HookOutcomeDeadLetter {
			telemetryDeadLetter = true
		}
	}

	assert.True(t, blockingSuccess, "blocking safety hook should complete successfully")
	assert.True(t, telemetryDeadLetter, "failing telemetry hook should dead-letter without failing Emit")
	assert.NotContains(t, ledger.String(), "sk-mixedhooksecret")
}

func TestRunner_HookCommandAuditOmitsRawArgsAndPayload(t *testing.T) {
	auditDir := t.TempDir()
	t.Setenv(shell.EnvAuditDir, auditDir)

	out := t.TempDir() + "/event.json"
	hookProgram := filepath.Join(t.TempDir(), "sk-shellauditprogramsecret1234567890-events.test")
	require.NoError(t, os.Symlink(os.Args[0], hookProgram))

	runner := NewRunner(map[string][]config.HookConfig{
		UserMessage: {{
			Command: []string{
				hookProgram,
				"-test.run=^(TestHookHelperProcess|sk-shellauditargsecret1234567890)$",
			},
			Payload: string(PayloadFull),
			Env: map[string]string{
				"ATTELER_TEST_HOOK": "1",
				"ATTELER_TEST_OUT":  out,
			},
			TimeoutSeconds: 10,
			Blocking:       true,
		}},
	})

	err := runner.Emit(context.Background(), Event{
		Type:    UserMessage,
		Content: "prompt with OPENAI_API_KEY=sk-shellauditpayloadsecret1234567890",
	})
	require.NoError(t, err)

	audit := string(readFile(t, auditDir+"/commands.jsonl"))
	assert.Contains(t, audit, "atteler.event_hook.user_message")

	for _, leaked := range []string{
		"-test.run",
		"sk-shellauditprogramsecret",
		"sk-shellauditargsecret",
		"prompt with",
		"OPENAI_API_KEY",
		"sk-shellauditpayloadsecret",
	} {
		assert.NotContains(t, audit, leaked)
	}
}

func TestRunner_EmitRetriesBlockingHookUntilSuccess(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	attemptsPath := dir + "/attempts.txt"

	var ledger bytes.Buffer

	runner := NewRunnerWithOptions(map[string][]config.HookConfig{
		UserMessage: {{
			Command: []string{os.Args[0], "-test.run=^TestFlakyHookHelperProcess$"},
			Env: map[string]string{
				"ATTELER_TEST_FLAKY_HOOK":   "1",
				"ATTELER_TEST_ATTEMPTS_OUT": attemptsPath,
				"ATTELER_TEST_FAIL_UNTIL":   "2",
			},
			TimeoutSeconds:     10,
			MaxAttempts:        3,
			RetryBackoffMillis: 1,
			Blocking:           true,
		}},
	}, RunnerOptions{LedgerWriter: &ledger})

	err := runner.Emit(context.Background(), Event{Type: UserMessage})
	require.NoError(t, err)

	attemptsData, err := os.ReadFile(attemptsPath)
	require.NoError(t, err)
	assert.Equal(t, "3", strings.TrimSpace(string(attemptsData)))

	records := readLedgerRecords(t, ledger.String())

	var outcomes []string

	for i := range records {
		if records[i].Phase == LedgerPhaseHookAttempt {
			outcomes = append(outcomes, records[i].Outcome)
		}
	}

	assert.Equal(t, []string{HookOutcomeFailed, HookOutcomeFailed, HookOutcomeSuccess}, outcomes)
	assert.NotContains(t, ledgerPhases(records), LedgerPhaseHookDeadLetter)
}

func TestRunner_EmitRetriesNonBlockingHookUntilSuccessWithoutInterrupting(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	attemptsPath := dir + "/attempts.txt"

	var ledger bytes.Buffer

	runner := NewRunnerWithOptions(map[string][]config.HookConfig{
		UserMessage: {{
			Command: []string{os.Args[0], "-test.run=^TestFlakyHookHelperProcess$"},
			Env: map[string]string{
				"ATTELER_TEST_FLAKY_HOOK":   "1",
				"ATTELER_TEST_ATTEMPTS_OUT": attemptsPath,
				"ATTELER_TEST_FAIL_UNTIL":   "1",
			},
			TimeoutSeconds:     10,
			MaxAttempts:        3,
			RetryBackoffMillis: 1,
		}},
	}, RunnerOptions{LedgerWriter: &ledger})

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	err := runner.Emit(ctx, Event{Type: UserMessage})
	require.NoError(t, err)
	require.NoError(t, runner.Wait(ctx))

	attemptsData, err := os.ReadFile(attemptsPath)
	require.NoError(t, err)
	assert.Equal(t, "2", strings.TrimSpace(string(attemptsData)))

	records := readLedgerRecords(t, ledger.String())
	assert.Equal(t, []string{
		LedgerPhaseEvent,
		LedgerPhaseHookQueued,
		LedgerPhaseHookAttempt,
		LedgerPhaseHookAttempt,
	}, ledgerPhases(records))

	var outcomes []string

	for i := range records {
		if records[i].Phase == LedgerPhaseHookAttempt {
			outcomes = append(outcomes, records[i].Outcome)
		}
	}

	assert.Equal(t, []string{HookOutcomeFailed, HookOutcomeSuccess}, outcomes)
	assert.NotContains(t, ledgerPhases(records), LedgerPhaseHookDeadLetter)
	assert.Equal(t, HookDeliveryNonBlocking, records[2].Hook.Delivery)
}

func TestRunner_EmitCapsConfiguredHookAttempts(t *testing.T) {
	t.Parallel()

	var ledger bytes.Buffer

	runner := NewRunnerWithOptions(map[string][]config.HookConfig{
		UserMessage: {{
			Command:            []string{"rm", "-rf", "/"},
			TimeoutSeconds:     10,
			MaxAttempts:        999,
			RetryBackoffMillis: 1,
			Blocking:           true,
		}},
	}, RunnerOptions{LedgerWriter: &ledger})

	err := runner.Emit(context.Background(), Event{Type: UserMessage})
	require.Error(t, err)

	var policyErr *shell.PolicyError
	require.ErrorAs(t, err, &policyErr)

	records := readLedgerRecords(t, ledger.String())

	var attempts []LedgerRecord

	for i := range records {
		if records[i].Phase == LedgerPhaseHookAttempt {
			attempts = append(attempts, records[i])
		}
	}

	require.Len(t, attempts, maxHookAttempts)

	for i := range attempts {
		assert.Equal(t, i+1, attempts[i].Attempt)
		assert.Equal(t, maxHookAttempts, attempts[i].MaxAttempts)
		assert.Equal(t, HookOutcomeDenied, attempts[i].Outcome)
	}

	require.NotEmpty(t, records)
	assert.Equal(t, LedgerPhaseHookDeadLetter, records[len(records)-1].Phase)
	assert.Equal(t, maxHookAttempts, records[len(records)-1].MaxAttempts)
}

func TestNormalizeRetryBackoffBoundsConfiguredValues(t *testing.T) {
	t.Parallel()

	assert.Equal(t, defaultRetryBackoff, normalizeRetryBackoff(0))
	assert.Equal(t, defaultRetryBackoff, normalizeRetryBackoff(-1))
	assert.Equal(t, time.Second, normalizeRetryBackoff(1000))
	assert.Equal(t, maxRetryBackoff, normalizeRetryBackoff(int(maxRetryBackoff/time.Millisecond)+1))
	assert.Equal(t, maxRetryBackoff, normalizeRetryBackoff(int(^uint(0)>>1)))
}

func TestRunner_EmitStopsBlockingRetriesWhenContextCanceled(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	attemptsPath := dir + "/attempts.txt"

	var ledger bytes.Buffer

	runner := NewRunnerWithOptions(map[string][]config.HookConfig{
		UserMessage: {{
			Command: []string{os.Args[0], "-test.run=^TestFlakyHookHelperProcess$"},
			Env: map[string]string{
				"ATTELER_TEST_FLAKY_HOOK":   "1",
				"ATTELER_TEST_ATTEMPTS_OUT": attemptsPath,
				"ATTELER_TEST_FAIL_UNTIL":   "100",
			},
			TimeoutSeconds:     10,
			MaxAttempts:        3,
			RetryBackoffMillis: int(maxRetryBackoff / time.Millisecond),
			Blocking:           true,
		}},
	}, RunnerOptions{LedgerWriter: &ledger})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	cancelAfterFirstAttempt := make(chan struct{})

	go func() {
		defer close(cancelAfterFirstAttempt)

		deadline := time.Now().Add(5 * time.Second)
		for time.Now().Before(deadline) {
			if data, err := os.ReadFile(attemptsPath); err == nil && strings.TrimSpace(string(data)) == "1" {
				cancel()

				return
			}

			time.Sleep(10 * time.Millisecond)
		}

		cancel()
	}()

	err := runner.Emit(ctx, Event{Type: UserMessage})
	require.Error(t, err)
	require.ErrorIs(t, err, context.Canceled)
	<-cancelAfterFirstAttempt

	attemptsData, readErr := os.ReadFile(attemptsPath)
	require.NoError(t, readErr)
	assert.Equal(t, "1", strings.TrimSpace(string(attemptsData)))

	records := readLedgerRecords(t, ledger.String())

	var attemptRecords []LedgerRecord

	var deadLetter *LedgerRecord

	for i := range records {
		switch records[i].Phase {
		case LedgerPhaseHookAttempt:
			attemptRecords = append(attemptRecords, records[i])
		case LedgerPhaseHookDeadLetter:
			deadLetter = &records[i]
		}
	}

	require.Len(t, attemptRecords, 1)
	require.NotNil(t, deadLetter)
	assert.Equal(t, HookOutcomeDeadLetter, deadLetter.Outcome)
	assert.Equal(t, "canceled", deadLetter.TimeoutClassification)
	assert.Equal(t, "context canceled", deadLetter.ErrorSummary)
}

func TestRunner_EmitClassifiesBlockingHookTimeout(t *testing.T) {
	t.Parallel()

	var ledger bytes.Buffer

	runner := &Runner{
		hooks: map[string][]Hook{
			UserMessage: {{
				Command: []string{os.Args[0], "-test.run=^TestSlowHookHelperProcess$"},
				Env: map[string]string{
					"ATTELER_TEST_SLOW_HOOK": "1",
				},
				Timeout:     20 * time.Millisecond,
				MaxAttempts: defaultMaxAttempts,
				Blocking:    true,
			}},
		},
		ledger: NewLedger(&ledger),
	}

	err := runner.Emit(context.Background(), Event{Type: UserMessage})
	require.Error(t, err)
	require.ErrorIs(t, err, context.DeadlineExceeded)
	assert.Contains(t, err.Error(), "timed out after")

	records := readLedgerRecords(t, ledger.String())

	var timeoutAttempt *LedgerRecord

	var deadLetter *LedgerRecord

	for i := range records {
		switch records[i].Phase {
		case LedgerPhaseHookAttempt:
			timeoutAttempt = &records[i]
		case LedgerPhaseHookDeadLetter:
			deadLetter = &records[i]
		}
	}

	require.NotNil(t, timeoutAttempt)
	assert.Equal(t, HookOutcomeTimeout, timeoutAttempt.Outcome)
	assert.Equal(t, "deadline_exceeded", timeoutAttempt.TimeoutClassification)
	require.NotNil(t, deadLetter)
	assert.Equal(t, HookOutcomeDeadLetter, deadLetter.Outcome)
	assert.Equal(t, "deadline_exceeded", deadLetter.TimeoutClassification)
}

func TestRunner_EmitNonBlockingHookTimeoutDeadLettersWithoutInterrupting(t *testing.T) {
	t.Parallel()

	var ledger bytes.Buffer

	runner := &Runner{
		hooks: map[string][]Hook{
			UserMessage: {{
				Command: []string{os.Args[0], "-test.run=^TestSlowHookHelperProcess$"},
				Env: map[string]string{
					"ATTELER_TEST_SLOW_HOOK": "1",
				},
				Timeout: 20 * time.Millisecond,
			}},
		},
		ledger: NewLedger(&ledger),
	}

	err := runner.Emit(context.Background(), Event{Type: UserMessage})
	require.NoError(t, err)

	waitCtx, cancelWait := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancelWait()

	require.NoError(t, runner.Wait(waitCtx))

	records := readLedgerRecords(t, ledger.String())
	require.Len(t, records, 4)
	assert.Equal(t, []string{
		LedgerPhaseEvent,
		LedgerPhaseHookQueued,
		LedgerPhaseHookAttempt,
		LedgerPhaseHookDeadLetter,
	}, ledgerPhases(records))

	attempt := records[2]
	require.NotNil(t, attempt.Hook)
	assert.Equal(t, HookDeliveryNonBlocking, attempt.Hook.Delivery)
	assert.Equal(t, HookOutcomeTimeout, attempt.Outcome)
	assert.Equal(t, "deadline_exceeded", attempt.TimeoutClassification)

	deadLetter := records[3]
	assert.Equal(t, HookOutcomeDeadLetter, deadLetter.Outcome)
	assert.Equal(t, "deadline_exceeded", deadLetter.TimeoutClassification)
}

func TestRunner_CloseLeavesLedgerOpenWhenQueuedHooksAreStillRunning(t *testing.T) {
	t.Parallel()

	var ledger bytes.Buffer

	runner := &Runner{
		hooks: map[string][]Hook{
			UserMessage: {{
				Command: []string{os.Args[0], "-test.run=^TestSlowHookHelperProcess$"},
				Env: map[string]string{
					"ATTELER_TEST_SLOW_HOOK": "1",
				},
				Timeout: 10 * time.Second,
			}},
		},
		ledger: NewLedger(&ledger),
	}

	require.NoError(t, runner.Emit(context.Background(), Event{Type: UserMessage}))

	closeCtx, cancelClose := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancelClose()

	err := runner.Close(closeCtx)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "wait for hooks")
	require.False(t, runner.ledger.closed, "ledger must stay open so queued hook delivery can append its final record")

	waitCtx, cancelWait := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancelWait()

	require.NoError(t, runner.Close(waitCtx))
	require.True(t, runner.ledger.closed)

	records := readLedgerRecords(t, ledger.String())
	assert.Equal(t, []string{
		LedgerPhaseEvent,
		LedgerPhaseHookQueued,
		LedgerPhaseHookAttempt,
	}, ledgerPhases(records))
	assert.Equal(t, HookOutcomeSuccess, records[2].Outcome)
}

func TestRunner_LedgerRedactsSensitiveEventPayloads(t *testing.T) {
	t.Parallel()

	var ledger bytes.Buffer

	runner := NewRunnerWithOptions(nil, RunnerOptions{LedgerWriter: &ledger})

	err := runner.Emit(context.Background(), Event{
		Type:    CommandOutput,
		Content: "raw command output with OPENAI_API_KEY=sk-ledgersecret1234567890",
		Error:   "failed with bearer abcdefghijklmnopqrstuvwxyz",
		Metadata: map[string]string{
			"command": "cat ~/.env",
			"cwd":     "/Users/example/project",
			"source":  "llm_tool",
		},
	})
	require.NoError(t, err)

	records := readLedgerRecords(t, ledger.String())
	require.Len(t, records, 1)
	require.NotNil(t, records[0].Event)

	event := records[0].Event
	assert.Equal(t, LedgerSchemaVersion, records[0].SchemaVersion)
	assert.Equal(t, EventSchemaVersion, event.SchemaVersion)
	assert.Equal(t, string(PayloadSummary), event.PayloadMode)
	assert.Empty(t, event.Content)
	assert.Empty(t, event.Error)
	assert.NotEmpty(t, event.ContentSummary)
	assert.NotEmpty(t, event.ErrorSummary)
	assert.Equal(t, map[string]string{"source": "llm_tool"}, event.Metadata)
	assert.True(t, event.Redacted)

	got := ledger.String()
	for _, leaked := range []string{"raw command output", "sk-ledgersecret", "abcdefghijklmnopqrstuvwxyz", "cat ~/.env", "/Users/example"} {
		assert.NotContains(t, got, leaked)
	}
}

func TestRunner_LedgerSummarizesLargePayloadsWithoutRawContent(t *testing.T) {
	t.Parallel()

	var ledger bytes.Buffer

	runner := NewRunnerWithOptions(nil, RunnerOptions{LedgerWriter: &ledger})

	largeContent := "raw command output " + strings.Repeat("x", maxContentBytes*16)
	err := runner.Emit(context.Background(), Event{
		Type:    CommandOutput,
		Content: largeContent,
		Error:   strings.Repeat("error detail ", maxErrorBytes),
		Metadata: map[string]string{
			"source":       "llm_tool",
			"tool_call_id": "call-large-payload",
			"command":      "cat /Users/example/private/.env",
		},
	})
	require.NoError(t, err)

	data := ledger.String()
	assert.Less(t, len(data), maxHookPayloadBytes, "summary ledger line should stay bounded even for large events")
	assert.NotContains(t, data, "raw command output")
	assert.NotContains(t, data, strings.Repeat("x", 128))
	assert.NotContains(t, data, "error detail error detail")
	assert.NotContains(t, data, "/Users/example/private")

	records := readLedgerRecords(t, data)
	require.Len(t, records, 1)
	require.NotNil(t, records[0].Event)
	assert.Empty(t, records[0].Event.Content)
	assert.Empty(t, records[0].Event.Error)
	assert.Contains(t, records[0].Event.ContentSummary, "content redacted bytes=")
	assert.Contains(t, records[0].Event.ErrorSummary, "error redacted bytes=")
	assert.Equal(t, map[string]string{"source": "llm_tool", "tool_call_id": "call-large-payload"}, records[0].Event.Metadata)
	assert.True(t, records[0].Event.Redacted)
}

func TestNewFileLedgerPersistsAppendOnlyJSONLines(t *testing.T) {
	t.Parallel()

	path := t.TempDir() + "/nested/events.jsonl"

	ledger, err := NewFileLedger(path)
	require.NoError(t, err)

	runner := NewRunnerWithOptions(nil, RunnerOptions{Ledger: ledger})
	require.NoError(t, runner.Emit(context.Background(), Event{Type: SessionStart, SessionID: "session-1"}))
	require.NoError(t, runner.Emit(context.Background(), Event{Type: SessionEnd, SessionID: "session-1"}))
	require.NoError(t, runner.Close(context.Background()))

	info, err := os.Stat(path)
	require.NoError(t, err)
	assert.Equal(t, fs.FileMode(0o600), info.Mode().Perm())

	records := readLedgerRecords(t, string(readFile(t, path)))
	require.Len(t, records, 2)
	assert.Equal(t, []string{LedgerPhaseEvent, LedgerPhaseEvent}, ledgerPhases(records))
	assert.Equal(t, SessionStart, records[0].Event.Type)
	assert.Equal(t, SessionEnd, records[1].Event.Type)
	assert.NotEmpty(t, records[0].Event.EventID)
	assert.NotEqual(t, records[0].Event.EventID, records[1].Event.EventID)
}

func TestNewFileLedgerErrorsOmitRawPaths(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()

	parentFile := filepath.Join(dir, "sk-ledgerdirsecret1234567890")
	require.NoError(t, os.WriteFile(parentFile, []byte("not a directory"), 0o600))

	_, err := NewFileLedger(filepath.Join(parentFile, "events.jsonl"))
	require.Error(t, err)

	got := err.Error()
	assert.Contains(t, got, redactedValue)
	assert.NotContains(t, got, dir)
	assert.NotContains(t, got, "sk-ledgerdirsecret")

	secretDir := filepath.Join(dir, "sk-ledgerfilesecret1234567890")
	require.NoError(t, os.Mkdir(secretDir, 0o700))

	_, err = NewFileLedger(secretDir)
	require.Error(t, err)

	got = err.Error()
	assert.Contains(t, got, redactedValue)
	assert.NotContains(t, got, dir)
	assert.NotContains(t, got, "sk-ledgerfilesecret")
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

	ctx, cancel := context.WithCancel(t.Context())
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
			TimeoutSeconds: 10,
			Blocking:       true,
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
			TimeoutSeconds: 10,
			Blocking:       true,
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
			TimeoutSeconds: 10,
			Blocking:       true,
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
			TimeoutSeconds: 10,
			Blocking:       true,
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
			TimeoutSeconds: 10,
			Blocking:       true,
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

func TestEmitFromContextBestEffortLogsCanceledContext(t *testing.T) {
	t.Parallel()

	var out bytes.Buffer

	ctx := WithEmitter(
		context.Background(),
		NewRunnerWithLogger(nil, &out),
		Event{Model: "gpt-test"},
	)
	ctx, cancel := context.WithCancel(ctx)
	cancel()

	err := EmitFromContextBestEffort(ctx, Event{
		Type: ProviderRetry,
		Metadata: map[string]string{
			"outcome": "canceled",
		},
	})
	require.Error(t, err)
	require.ErrorIs(t, err, context.Canceled)

	logs := out.String()
	assert.Contains(t, logs, "event:provider_retry")
	assert.Contains(t, logs, "model=gpt-test")
	assert.Contains(t, logs, "outcome=canceled")
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

//nolint:paralleltest // Mutates the process-global slog default.
func TestHookDeliveryWarningRedactsSensitiveFields(t *testing.T) {
	var out bytes.Buffer

	original := slog.Default()

	slog.SetDefault(slog.New(slog.NewTextHandler(&out, &slog.HandlerOptions{Level: slog.LevelWarn})))
	t.Cleanup(func() {
		slog.SetDefault(original)
	})

	err := fmt.Errorf("events: authorize hook %q: %w", "notify (+1 args)", &shell.PolicyError{
		Reason: `path argument "/Users/example/private/sk-deliverypolicysecret1234567890" matches denied path`,
		Rule:   "path.deny",
	})

	slogWarnHookDelivery(
		"custom/sk-deliveryeventsecret1234567890\nroot",
		Hook{Command: []string{"/tmp/sk-deliverycmdsecret1234567890/notify-hook", "--token=sk-deliveryargsecret1234567890"}},
		err,
	)

	got := out.String()
	assert.Contains(t, got, "lifecycle hook delivery failed")
	assert.Contains(t, got, "event_type=custom_"+redactedValue+"_root")
	assert.Contains(t, got, `hook_command="notify-hook (+1 args)"`)
	assert.Contains(t, got, "error=\"command denied by policy (path.deny)\"")

	for _, leaked := range []string{
		"custom/sk",
		"sk-deliveryeventsecret",
		"/tmp/",
		"sk-deliverycmdsecret",
		"--token",
		"sk-deliveryargsecret",
		"/Users/example",
		"sk-deliverypolicysecret",
		"path argument",
	} {
		assert.NotContains(t, got, leaked)
	}
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

func TestSafeErrorSummaryOmitsPolicyErrorDetails(t *testing.T) {
	t.Parallel()

	err := fmt.Errorf("events: authorize hook %q: %w", "notify (+1 args)", &shell.PolicyError{
		Reason: `path argument "/Users/example/private/sk-pathsecret1234567890" matches a denied path`,
		Rule:   "path.deny",
	})

	got := safeErrorSummary(err)

	assert.Equal(t, "command denied by policy (path.deny)", got)
	assert.NotContains(t, got, "/Users/example")
	assert.NotContains(t, got, "sk-pathsecret")
	assert.NotContains(t, got, "notify")
}

func TestHookAuthorizationErrorOmitsPolicyDetails(t *testing.T) {
	t.Parallel()

	err := hookAuthorizationError(Hook{
		Command: []string{"rm", "-rf", "/Users/example/private/sk-hookauthsecret1234567890"},
	}, &shell.PolicyError{
		Reason: `path argument "/Users/example/private/sk-policysecret1234567890" matches a denied path`,
		Rule:   "path.deny",
	})
	require.Error(t, err)

	var policyErr *shell.PolicyError
	require.ErrorAs(t, err, &policyErr)

	got := err.Error()
	assert.Contains(t, got, "rm (+2 args)")
	assert.Contains(t, got, "path.deny")

	for _, leaked := range []string{
		"/Users/example",
		"sk-hookauthsecret",
		"sk-policysecret",
		"-rf",
		"path argument",
	} {
		assert.NotContains(t, got, leaked)
		assert.NotContains(t, policyErr.Error(), leaked)
	}
}

func TestHookAuditErrorOmitsRawDetails(t *testing.T) {
	t.Parallel()

	err := hookAuditError(Hook{
		Command: []string{"/tmp/sk-hookauditcommandsecret1234567890/notify-hook", "--token=sk-hookauditargsecret1234567890"},
	}, errors.New("write /Users/example/private/sk-auditdetailsecret1234567890/commands.jsonl: permission denied"))
	require.Error(t, err)

	got := err.Error()
	assert.Contains(t, got, "hook audit failed")
	assert.Contains(t, got, "notify-hook")

	for _, leaked := range []string{
		"/tmp/",
		"/Users/example",
		"sk-hookauditcommandsecret",
		"sk-hookauditargsecret",
		"sk-auditdetailsecret",
		"--token",
		"commands.jsonl",
		"permission denied",
	} {
		assert.NotContains(t, got, leaked)
	}
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

//nolint:paralleltest // Helper-process sentinel exits the child test binary.
func TestFlakyHookHelperProcess(t *testing.T) {
	if os.Getenv("ATTELER_TEST_FLAKY_HOOK") != "1" {
		return
	}

	attemptsPath := os.Getenv("ATTELER_TEST_ATTEMPTS_OUT")
	require.NotEmpty(t, attemptsPath)

	attempt := 1

	if data, err := os.ReadFile(attemptsPath); err == nil { //nolint:gosec // Test helper reads a temp path supplied by the parent test.
		previous, parseErr := strconv.Atoi(strings.TrimSpace(string(data)))
		require.NoError(t, parseErr)

		attempt = previous + 1
	}

	require.NoError(t, os.WriteFile(attemptsPath, []byte(strconv.Itoa(attempt)), 0o600)) //nolint:gosec // Test helper writes to a temp path supplied by the parent test.

	failUntil, err := strconv.Atoi(os.Getenv("ATTELER_TEST_FAIL_UNTIL"))
	require.NoError(t, err)

	if attempt <= failUntil {
		_, _ = os.Stderr.WriteString("transient hook failure sk-flakysecret1234567890")

		os.Exit(1)
	}

	os.Exit(0)
}

//nolint:paralleltest // Helper-process sentinel exits the child test binary.
func TestSlowHookHelperProcess(_ *testing.T) {
	if os.Getenv("ATTELER_TEST_SLOW_HOOK") != "1" {
		return
	}

	time.Sleep(2 * time.Second)
	os.Exit(0)
}

//nolint:paralleltest // Helper-process sentinel exits the child test binary.
func TestDelayedHookHelperProcess(t *testing.T) {
	if os.Getenv("ATTELER_TEST_DELAY_HOOK") != "1" {
		return
	}

	delayMillis, err := strconv.Atoi(os.Getenv("ATTELER_TEST_DELAY_MS"))
	require.NoError(t, err)

	time.Sleep(time.Duration(delayMillis) * time.Millisecond)

	markerPath := os.Getenv("ATTELER_TEST_MARKER_OUT")
	require.NotEmpty(t, markerPath)
	require.NoError(t, os.WriteFile(markerPath, []byte("delivered\n"), 0o600)) //nolint:gosec // Test helper writes to a temp path supplied by the parent test.

	os.Exit(0)
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
			"ATTELER_EVENT_ID",
			"ATTELER_PAYLOAD_MODE",
			"ATTELER_SESSION_ID",
			"ATTELER_SESSION_PATH",
			"ATTELER_AGENT",
			"ATTELER_MODEL",
			"ATTELER_ROLE",
			"ATTELER_EVENT_REDACTED",
			"ATTELER_EVENT_TRUNCATED",
			"ATTELER_EVENT_SCHEMA_VERSION",
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

func readLedgerRecords(t *testing.T, data string) []LedgerRecord {
	t.Helper()

	lines := strings.Split(strings.TrimSpace(data), "\n")

	records := make([]LedgerRecord, 0, len(lines))
	for _, line := range lines {
		if strings.TrimSpace(line) == "" {
			continue
		}

		var record LedgerRecord
		require.NoError(t, json.Unmarshal([]byte(line), &record))
		records = append(records, record)
	}

	return records
}

func ledgerPhases(records []LedgerRecord) []string {
	phases := make([]string, 0, len(records))
	for i := range records {
		phases = append(phases, records[i].Phase)
	}

	return phases
}
