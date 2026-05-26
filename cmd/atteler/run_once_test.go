package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/tommoulard/atteler/pkg/agent"
	"github.com/tommoulard/atteler/pkg/config"
	"github.com/tommoulard/atteler/pkg/contextref"
	"github.com/tommoulard/atteler/pkg/events"
	"github.com/tommoulard/atteler/pkg/llm"
	"github.com/tommoulard/atteler/pkg/modelroute"
	"github.com/tommoulard/atteler/pkg/session"
)

func TestRunOnce_ReplaysResponseWithoutProvider(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()

	replayPath := filepath.Join(dir, "response.json")
	if err := saveRecordedResponse(
		replayPath,
		llm.CompleteParams{Model: "gpt-test", Messages: []llm.Message{{Role: llm.RoleUser, Content: "hello"}}},
		nil,
		&llm.Response{Content: "recorded answer", Model: "gpt-test", InputTokens: 2, CachedInputTokens: 1, OutputTokens: 3},
	); err != nil {
		require.NoError(t, err)
	}

	store := session.NewStore(filepath.Join(dir, "sessions"))

	err := runOnce(
		context.Background(),
		llm.NewRegistry(),
		agent.NewRegistry(nil),
		nil,
		store,
		session.New("gpt-test", nil),
		contextref.Options{Root: dir},
		"",
		"gpt-test",
		"",
		nil,
		generationSettings{},
		generationSettings{},
		0,
		responseRecordOptions{ReplayPath: replayPath},
		true,
		"hello",
	)
	if err != nil {
		require.NoError(t, err)
	}

	summaries, err := store.List()
	if err != nil {
		require.NoError(t, err)
	}

	if len(summaries) != 1 {
		require.Failf(t, "unexpected sessions", "summaries = %+v", summaries)
	}

	loaded, err := store.Load(summaries[0].ID)
	if err != nil {
		require.NoError(t, err)
	}

	if len(loaded.Messages) != 2 || loaded.Messages[1].Content != "recorded answer" {
		require.Failf(t, "unexpected replayed session", "messages = %+v", loaded.Messages)
	}
}

func TestPrepareRunOnceRequestReturnsRouteDecisionOnRoutingError(t *testing.T) {
	t.Parallel()

	agents := agent.NewRegistry(map[string]config.AgentConfig{
		"reviewer": {
			Model:          "openai/gpt-4.1-mini",
			FallbackModels: []string{"openai/gpt-4.1-nano"},
			RoutingPolicy: config.RoutingPolicyConfig{
				BannedProviders: []string{"openai"},
			},
		},
	})

	prepared, err := prepareRunOnceRequest(
		context.Background(),
		llm.NewRegistry(),
		agents,
		contextref.Options{Root: t.TempDir()},
		"",
		"",
		"reviewer",
		nil,
		generationSettings{},
		generationSettings{},
		false,
		"review this",
	)

	require.Error(t, err)
	require.NotNil(t, prepared.routeDecision)
	assert.Empty(t, prepared.routeDecision.Selected)
	assert.Equal(t, "reviewer", prepared.activeAgent.name)
	assertRejectionContainsCommand(t, *prepared.routeDecision, "openai/gpt-4.1-mini", modelroute.ReasonProviderBanned)
	assertRejectionContainsCommand(t, *prepared.routeDecision, "openai/gpt-4.1-nano", modelroute.ReasonProviderBanned)
}

func TestRunOnceWithOptions_EmitsEstimatedAndActualRouteDecisionEvents(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	replayPath := filepath.Join(dir, "response.json")
	require.NoError(t, saveRecordedResponse(
		replayPath,
		llm.CompleteParams{Model: "openai/gpt-4.1-mini", Messages: []llm.Message{{Role: llm.RoleUser, Content: "review this"}}},
		nil,
		&llm.Response{
			Content:           "recorded answer",
			Provider:          "openai",
			Model:             "gpt-4.1-nano",
			Latency:           42 * time.Millisecond,
			FirstTokenLatency: 7 * time.Millisecond,
			InputTokens:       100,
			CachedInputTokens: 20,
			OutputTokens:      10,
		},
	))

	registry := llm.NewRegistry()
	registry.Register(routeFakeProvider{name: "openai", models: []string{"gpt-4.1-mini", "gpt-4.1-nano"}})
	_, err := registry.ProviderModels(context.Background(), "openai")
	require.NoError(t, err)

	agents := agent.NewRegistry(map[string]config.AgentConfig{
		"reviewer": {
			Model:          "openai/gpt-4.1-mini",
			FallbackModels: []string{"openai/gpt-4.1-nano"},
			RoutingPolicy: config.RoutingPolicyConfig{
				PreferredProviders: []string{"openai"},
			},
		},
	})

	var eventLog bytes.Buffer

	hooks := events.NewRunnerWithLogger(nil, &eventLog)
	store := session.NewStore(filepath.Join(dir, "sessions"))

	err = runOnceWithOptions(
		context.Background(),
		registry,
		agents,
		hooks,
		store,
		session.New("", nil),
		contextref.Options{Root: dir},
		"",
		"",
		"reviewer",
		nil,
		generationSettings{},
		generationSettings{},
		0,
		runOnceExecutionOptions{
			OutputFormat: outputFormatText,
			HeadlessID:   "route-decision-events",
			Headless:     true,
			Response:     responseRecordOptions{ReplayPath: replayPath},
		},
		false,
		"review this",
	)
	require.NoError(t, err)

	log := eventLog.String()
	assert.Contains(t, log, "event:route_decision")
	assert.Contains(t, log, "agent=reviewer")
	assert.Contains(t, log, "phase=estimated")
	assert.Contains(t, log, "phase=actual")
	assert.Contains(t, log, "actual_cost=")
	assert.Contains(t, log, "actual_latency_ms=42")
	assert.Contains(t, log, "actual_ttft_ms=7")
	assert.Contains(t, log, "actual_input_tokens=100")
	assert.Contains(t, log, "actual_output_tokens=10")
	assert.Contains(t, log, "actual_selected=openai/gpt-4.1-nano")
	assert.Contains(t, log, "fallback_order=openai/gpt-4.1-nano,openai/gpt-4.1-mini")
	assert.Contains(t, log, "verified_provider_model_count=1")
}

func TestWriteRunOnceResult_JSONAndHeadlessText(t *testing.T) {
	t.Parallel()

	result := runOnceResult{
		SessionID:               "session-id",
		SessionPath:             "/tmp/session.json",
		AgentLoopCheckpointPath: "/tmp/session.agentloop.jsonl",
		HeadlessID:              "headless-id",
		Model:                   "gpt-test",
		Content:                 "answer",
		TokenUsage:              tokenUsage{InputTokens: 1, CachedInputTokens: 2, OutputTokens: 3, Responses: 1},
	}

	var (
		stdout bytes.Buffer
		stderr bytes.Buffer
	)

	require.NoError(t, writeRunOnceResult(&stdout, &stderr, result, "json", true))

	var decoded runOnceResult
	require.NoError(t, json.Unmarshal(stdout.Bytes(), &decoded))
	assert.Equal(t, result.SessionID, decoded.SessionID)
	assert.Equal(t, result.HeadlessID, decoded.HeadlessID)
	assert.Equal(t, result.AgentLoopCheckpointPath, decoded.AgentLoopCheckpointPath)
	assert.Equal(t, result.TokenUsage.OutputTokens, decoded.TokenUsage.OutputTokens)
	assert.Empty(t, stderr.String())

	stdout.Reset()
	stderr.Reset()
	require.NoError(t, writeRunOnceResult(&stdout, &stderr, result, "text", true))
	assert.Empty(t, stdout.String())
	assert.Empty(t, stderr.String())

	require.NoError(t, writeRunOnceResult(&stdout, &stderr, result, "text", false))
	assert.Contains(t, stderr.String(), "agent loop checkpoint: /tmp/session.agentloop.jsonl")
}

func TestRunOnceWithOptions_HeadlessReplayCreatesMetadata(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	replayPath := filepath.Join(dir, "response.json")
	require.NoError(t, saveRecordedResponse(
		replayPath,
		llm.CompleteParams{Model: "gpt-test", Messages: []llm.Message{{Role: llm.RoleUser, Content: "hello"}}},
		nil,
		&llm.Response{Content: "recorded answer", Model: "gpt-test", InputTokens: 2, CachedInputTokens: 1, OutputTokens: 3},
	))

	store := session.NewStore(filepath.Join(dir, "sessions"))
	headlessID := "test-headless"

	err := runOnceWithOptions(
		context.Background(),
		llm.NewRegistry(),
		agent.NewRegistry(nil),
		nil,
		store,
		session.New("gpt-test", nil),
		contextref.Options{Root: dir},
		"",
		"gpt-test",
		"",
		nil,
		generationSettings{},
		generationSettings{},
		0,
		runOnceExecutionOptions{
			OutputFormat: outputFormatText,
			HeadlessID:   headlessID,
			Response:     responseRecordOptions{ReplayPath: replayPath},
			Headless:     true,
		},
		true,
		"hello",
	)
	require.NoError(t, err)

	run, err := store.LoadHeadlessRun(headlessID)
	require.NoError(t, err)
	assert.Equal(t, session.HeadlessStatusCompleted, run.Status)
	assert.Equal(t, "gpt-test", run.Model)
	assert.Equal(t, "headless", run.StartMethod)
	assert.NotNil(t, run.CompletedAt)

	log, err := store.ReadHeadlessLog(headlessID)
	require.NoError(t, err)
	assert.Contains(t, log, "started")
	assert.Contains(t, log, "assistant_message")
	assert.Contains(t, log, "completed")

	headlessEvents, err := store.ReadHeadlessEvents(headlessID)
	require.NoError(t, err)
	require.Len(t, headlessEvents, 4)
	assert.Equal(t, session.HeadlessEventStarted, headlessEvents[0].Type)
	assert.Equal(t, session.HeadlessEventUserMessage, headlessEvents[1].Type)
	assert.Equal(t, session.HeadlessEventAssistantMessage, headlessEvents[2].Type)
	assert.Equal(t, session.HeadlessEventCompleted, headlessEvents[3].Type)
	assert.Equal(t, session.HeadlessStatusRunning, headlessEvents[0].Status)
	assert.Equal(t, session.HeadlessStatusRunning, headlessEvents[1].Status)
	assert.Equal(t, session.HeadlessStatusRunning, headlessEvents[2].Status)
	assert.Equal(t, session.HeadlessStatusCompleted, headlessEvents[3].Status)
	assert.Equal(t, run.SessionID, headlessEvents[0].SessionID)
	assert.Equal(t, run.SessionID, headlessEvents[1].SessionID)
	assert.Equal(t, run.SessionID, headlessEvents[2].SessionID)
	assert.Equal(t, run.SessionID, headlessEvents[3].SessionID)
	assert.Equal(t, run.SessionPath, headlessEvents[0].SessionPath)
	assert.Equal(t, run.SessionPath, headlessEvents[1].SessionPath)
	assert.Equal(t, run.SessionPath, headlessEvents[2].SessionPath)
	assert.Equal(t, run.SessionPath, headlessEvents[3].SessionPath)
	assert.Equal(t, "gpt-test", headlessEvents[0].Model)
	assert.Equal(t, "gpt-test", headlessEvents[1].Model)
	assert.Equal(t, "gpt-test", headlessEvents[2].Model)
	assert.Equal(t, "gpt-test", headlessEvents[3].Model)
	assert.Equal(t, "headless", headlessEvents[0].StartMethod)
	assert.NotEmpty(t, headlessEvents[0].StartedCommand)
	assert.NotEmpty(t, headlessEvents[0].CommandArgs)
	assert.Equal(t, string(llm.RoleUser), headlessEvents[1].Role)
	assert.Equal(t, "hello", headlessEvents[1].Message)
	assert.Equal(t, "5", headlessEvents[1].Metadata["bytes"])
	assert.Equal(t, string(llm.RoleAssistant), headlessEvents[2].Role)
	assert.Equal(t, "15", headlessEvents[2].Metadata["bytes"])
	assert.Equal(t, "headless", headlessEvents[3].StartMethod)
	assert.NotEmpty(t, headlessEvents[3].StartedCommand)
	assert.NotEmpty(t, headlessEvents[3].CommandArgs)
	assert.Equal(t, "completed", headlessEvents[3].TerminalReason)

	runs, err := store.ListHeadlessRuns()
	require.NoError(t, err)
	require.Len(t, runs, 1)
	assert.Equal(t, headlessID, runs[0].ID)
	require.NoError(t, streamHeadlessLog(context.Background(), store, headlessID))
}

func TestStartHeadlessRunRejectsWhitespacePaddedExplicitID(t *testing.T) {
	t.Parallel()

	store := session.NewStore(t.TempDir())
	run, err := startHeadlessRun(
		store,
		runOnceExecutionOptions{Headless: true, HeadlessID: " headless-id "},
		session.New("gpt-test", nil),
		"hello",
		"gpt-test",
		"default",
	)

	require.ErrorContains(t, err, "headless id must not have leading or trailing whitespace")
	assert.Nil(t, run)
}

func TestStartHeadlessRunRejectsWhitespacePaddedParentID(t *testing.T) {
	store := session.NewStore(t.TempDir())
	t.Setenv(headlessParentRunIDEnv, " parent-headless ")

	run, err := startHeadlessRun(
		store,
		runOnceExecutionOptions{Headless: true, HeadlessID: "child-headless"},
		session.New("gpt-test", nil),
		"hello",
		"gpt-test",
		"default",
	)

	require.ErrorContains(t, err, "invalid parent headless id")
	assert.Nil(t, run)

	_, err = store.LoadHeadlessRun("child-headless")
	require.ErrorIs(t, err, os.ErrNotExist)
}

func TestStartHeadlessRunRecordsParentRunRelationship(t *testing.T) {
	store := session.NewStore(t.TempDir())
	require.NoError(t, store.SaveHeadlessRun(session.HeadlessRun{
		ID:     "parent-headless",
		Status: session.HeadlessStatusRunning,
	}))

	t.Setenv(headlessParentRunIDEnv, "parent-headless")

	child, err := startHeadlessRun(
		store,
		runOnceExecutionOptions{
			HeadlessID: "child-headless",
			Headless:   true,
		},
		session.New("gpt-test", nil),
		"hello",
		"gpt-test",
		"",
	)
	require.NoError(t, err)
	require.NotNil(t, child)
	assert.Equal(t, "parent-headless", child.ParentRunID)

	parent, err := store.LoadHeadlessRun("parent-headless")
	require.NoError(t, err)
	assert.Contains(t, parent.ChildRunIDs, "child-headless")

	headlessEvents, err := store.ReadHeadlessEvents("child-headless")
	require.NoError(t, err)
	require.Len(t, headlessEvents, 2)
	assert.Equal(t, "parent-headless", headlessEvents[0].ParentRunID)
	assert.Equal(t, session.HeadlessEventUserMessage, headlessEvents[1].Type)
	assert.Equal(t, "parent-headless", headlessEvents[1].ParentRunID)
}

func TestStartHeadlessRunRejectsExistingExplicitID(t *testing.T) {
	t.Parallel()

	store := session.NewStore(t.TempDir())
	require.NoError(t, store.SaveHeadlessRun(session.HeadlessRun{
		ID:        "duplicate-headless",
		SessionID: "session-existing",
		Hostname:  "foreign-host",
		PID:       12345,
		Status:    session.HeadlessStatusCompleted,
	}))

	run, err := startHeadlessRun(
		store,
		runOnceExecutionOptions{
			HeadlessID: "duplicate-headless",
			Headless:   true,
		},
		session.New("gpt-test", nil),
		"hello",
		"gpt-test",
		"",
	)
	require.ErrorContains(t, err, "already exists")
	assert.Nil(t, run)

	loaded, err := store.LoadHeadlessRun("duplicate-headless")
	require.NoError(t, err)
	assert.Equal(t, session.HeadlessStatusCompleted, loaded.Status)
	assert.Equal(t, "session-existing", loaded.SessionID)
	assert.Equal(t, 12345, loaded.PID)
}

func TestSaveStartedHeadlessRunRejectsAnyExistingID(t *testing.T) {
	t.Parallel()

	store := session.NewStore(t.TempDir())
	require.NoError(t, store.SaveHeadlessRun(session.HeadlessRun{
		ID:        "generated-collision",
		SessionID: "session-existing",
		Status:    session.HeadlessStatusCompleted,
	}))

	err := saveStartedHeadlessRun(store, session.HeadlessRun{
		ID:        "generated-collision",
		SessionID: "session-new",
		Status:    session.HeadlessStatusRunning,
	})
	require.ErrorContains(t, err, "already exists")

	loaded, err := store.LoadHeadlessRun("generated-collision")
	require.NoError(t, err)
	assert.Equal(t, "session-existing", loaded.SessionID)
	assert.Equal(t, session.HeadlessStatusCompleted, loaded.Status)
}

func TestFailStartedHeadlessRunRecordsFailedStatus(t *testing.T) {
	t.Parallel()

	store := session.NewStore(t.TempDir())
	run := session.HeadlessRun{
		ID:     "test-headless-start-failure",
		Status: session.HeadlessStatusRunning,
	}
	require.NoError(t, store.SaveHeadlessRun(run))

	loaded, err := store.LoadHeadlessRun(run.ID)
	require.NoError(t, err)

	failStartedHeadlessRun(store, &loaded, errors.New("write headless start log: permission denied"))

	failed, err := store.LoadHeadlessRun(run.ID)
	require.NoError(t, err)
	assert.Equal(t, session.HeadlessStatusFailed, failed.Status)
	assert.Equal(t, "write headless start log: permission denied", failed.Error)
	assert.Equal(t, failed.Error, failed.TerminalReason)
	require.NotNil(t, failed.ExitCode)
	assert.Equal(t, 1, *failed.ExitCode)

	log, err := store.ReadHeadlessLog(run.ID)
	require.NoError(t, err)
	assert.Contains(t, log, "failed")
	assert.Contains(t, log, "write headless start log")

	headlessEvents, err := store.ReadHeadlessEvents(run.ID)
	require.NoError(t, err)
	require.Len(t, headlessEvents, 1)
	assert.Equal(t, session.HeadlessEventFailed, headlessEvents[0].Type)
	assert.Contains(t, headlessEvents[0].Error, "write headless start log")
	assert.Equal(t, headlessEvents[0].Error, headlessEvents[0].TerminalReason)
}

func TestRunOnceWithOptions_HeadlessPrivateLogKeepsPrompt(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	replayPath := filepath.Join(dir, "response.json")
	require.NoError(t, saveRecordedResponse(
		replayPath,
		llm.CompleteParams{Model: "gpt-test", Messages: []llm.Message{{Role: llm.RoleUser, Content: "hello"}}},
		nil,
		&llm.Response{Content: "recorded answer", Model: "gpt-test"},
	))

	store := session.NewStore(filepath.Join(dir, "sessions"))
	headlessID := "test-headless-private"
	secretPrompt := "deploy with api_key=sk-" + strings.Repeat("p", 20)

	err := runOnceWithOptions(
		context.Background(),
		llm.NewRegistry(),
		agent.NewRegistry(nil),
		nil,
		store,
		session.New("gpt-test", nil),
		contextref.Options{Root: dir},
		"",
		"gpt-test",
		"",
		nil,
		generationSettings{},
		generationSettings{},
		0,
		runOnceExecutionOptions{
			OutputFormat:       outputFormatText,
			HeadlessID:         headlessID,
			Response:           responseRecordOptions{ReplayPath: replayPath},
			Headless:           true,
			HeadlessPrivateLog: true,
		},
		true,
		secretPrompt,
	)
	require.NoError(t, err)

	run, err := store.LoadHeadlessRun(headlessID)
	require.NoError(t, err)
	assert.True(t, run.PrivateLogs)
	assert.Equal(t, secretPrompt, run.Prompt)

	headlessEvents, err := store.ReadHeadlessEvents(headlessID)
	require.NoError(t, err)
	require.Len(t, headlessEvents, 4)
	assert.Equal(t, session.HeadlessEventUserMessage, headlessEvents[1].Type)
	assert.Equal(t, string(llm.RoleUser), headlessEvents[1].Role)
	assert.Equal(t, secretPrompt, headlessEvents[1].Message)
}

func TestRunOnceWithOptions_HeadlessRedactsPromptByDefault(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	replayPath := filepath.Join(dir, "response.json")
	require.NoError(t, saveRecordedResponse(
		replayPath,
		llm.CompleteParams{Model: "gpt-test", Messages: []llm.Message{{Role: llm.RoleUser, Content: "hello"}}},
		nil,
		&llm.Response{Content: "recorded answer", Model: "gpt-test"},
	))

	store := session.NewStore(filepath.Join(dir, "sessions"))
	headlessID := "test-headless-redacted"
	secretPrompt := "deploy with api_key=sk-" + strings.Repeat("q", 20)

	err := runOnceWithOptions(
		context.Background(),
		llm.NewRegistry(),
		agent.NewRegistry(nil),
		nil,
		store,
		session.New("gpt-test", nil),
		contextref.Options{Root: dir},
		"",
		"gpt-test",
		"",
		nil,
		generationSettings{},
		generationSettings{},
		0,
		runOnceExecutionOptions{
			OutputFormat: outputFormatText,
			HeadlessID:   headlessID,
			Response:     responseRecordOptions{ReplayPath: replayPath},
			Headless:     true,
		},
		true,
		secretPrompt,
	)
	require.NoError(t, err)

	run, err := store.LoadHeadlessRun(headlessID)
	require.NoError(t, err)
	assert.False(t, run.PrivateLogs)
	assert.NotContains(t, run.Prompt, "sk-"+strings.Repeat("q", 20))
	assert.Contains(t, run.Prompt, "[REDACTED]")

	headlessEvents, err := store.ReadHeadlessEvents(headlessID)
	require.NoError(t, err)
	require.Len(t, headlessEvents, 4)
	assert.Equal(t, session.HeadlessEventUserMessage, headlessEvents[1].Type)
	assert.Equal(t, string(llm.RoleUser), headlessEvents[1].Role)
	assert.NotContains(t, headlessEvents[1].Message, "sk-"+strings.Repeat("q", 20))
	assert.Contains(t, headlessEvents[1].Message, "[REDACTED]")
}

func TestRunOnceWithOptions_HeadlessRecordsOutputFormatFailure(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	store := session.NewStore(filepath.Join(dir, "sessions"))
	headlessID := "test-headless-output-format-failure"

	err := runOnceWithOptions(
		context.Background(),
		llm.NewRegistry(),
		agent.NewRegistry(nil),
		nil,
		store,
		session.New("gpt-test", nil),
		contextref.Options{Root: dir},
		"",
		"gpt-test",
		"",
		nil,
		generationSettings{},
		generationSettings{},
		0,
		runOnceExecutionOptions{
			OutputFormat: "xml",
			HeadlessID:   headlessID,
			Headless:     true,
		},
		true,
		"hello",
	)
	require.ErrorContains(t, err, "unsupported output format")

	run, err := store.LoadHeadlessRun(headlessID)
	require.NoError(t, err)
	assert.Equal(t, session.HeadlessStatusFailed, run.Status)
	assert.Contains(t, run.Error, "unsupported output format")
	assert.Equal(t, run.Error, run.TerminalReason)
	require.NotNil(t, run.ExitCode)
	assert.Equal(t, 1, *run.ExitCode)

	headlessEvents, err := store.ReadHeadlessEvents(headlessID)
	require.NoError(t, err)
	require.Len(t, headlessEvents, 3)
	assert.Equal(t, session.HeadlessEventStarted, headlessEvents[0].Type)
	assert.Equal(t, session.HeadlessEventUserMessage, headlessEvents[1].Type)
	assert.Equal(t, session.HeadlessEventFailed, headlessEvents[2].Type)
}

func TestRunWithState_HeadlessRecordsOutputFormatFailure(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	store := session.NewStore(filepath.Join(dir, "sessions"))
	sessionState := session.New("gpt-test", nil)
	headlessID := "run-state-invalid-output"

	err := runWithState(context.Background(), cliOptions{
		oncePrompt:   "hello",
		headless:     true,
		headlessID:   headlessID,
		outputFormat: "yaml",
	}, appState{
		registry:       llm.NewRegistry(),
		agentRegistry:  agent.NewRegistry(nil),
		sessionStore:   store,
		sessionState:   sessionState,
		contextOptions: contextref.Options{Root: dir},
		selectedModel:  "gpt-test",
	})
	require.ErrorContains(t, err, "unsupported output format")

	loaded, loadErr := store.LoadHeadlessRun(headlessID)
	require.NoError(t, loadErr)
	assert.Equal(t, session.HeadlessStatusFailed, loaded.Status)
	assert.Equal(t, sessionState.ID, loaded.SessionID)
	assert.Contains(t, loaded.Error, "unsupported output format")
}

func TestRunWithState_HeadlessRecordsMissingPromptFailure(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	store := session.NewStore(filepath.Join(dir, "sessions"))
	sessionState := session.New("gpt-test", nil)
	headlessID := "run-state-missing-prompt"

	err := runWithState(context.Background(), cliOptions{
		headless:   true,
		headlessID: headlessID,
	}, appState{
		registry:       llm.NewRegistry(),
		agentRegistry:  agent.NewRegistry(nil),
		sessionStore:   store,
		sessionState:   sessionState,
		contextOptions: contextref.Options{Root: dir},
		selectedModel:  "gpt-test",
	})
	require.ErrorContains(t, err, "headless mode requires")

	loaded, loadErr := store.LoadHeadlessRun(headlessID)
	require.NoError(t, loadErr)
	assert.Equal(t, session.HeadlessStatusFailed, loaded.Status)
	assert.Equal(t, sessionState.ID, loaded.SessionID)
	assert.Contains(t, loaded.Error, "headless mode requires")
	assert.Equal(t, loaded.Error, loaded.TerminalReason)
	require.NotNil(t, loaded.ExitCode)
	assert.Equal(t, 1, *loaded.ExitCode)

	headlessEvents, eventsErr := store.ReadHeadlessEvents(headlessID)
	require.NoError(t, eventsErr)
	require.Len(t, headlessEvents, 2)
	assert.Equal(t, session.HeadlessEventStarted, headlessEvents[0].Type)
	assert.Equal(t, session.HeadlessEventFailed, headlessEvents[1].Type)
}

func TestRunOnceWithOptions_HeadlessRecordsReferenceExpansionFailure(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	store := session.NewStore(filepath.Join(dir, "sessions"))
	headlessID := "test-headless-reference-failure"

	err := runOnceWithOptions(
		context.Background(),
		llm.NewRegistry(),
		agent.NewRegistry(nil),
		nil,
		store,
		session.New("gpt-test", nil),
		contextref.Options{Root: dir},
		"",
		"gpt-test",
		"",
		nil,
		generationSettings{},
		generationSettings{},
		0,
		runOnceExecutionOptions{
			OutputFormat: outputFormatText,
			HeadlessID:   headlessID,
			Headless:     true,
		},
		true,
		"read @../outside.txt",
	)
	require.ErrorContains(t, err, "expand context references")

	run, err := store.LoadHeadlessRun(headlessID)
	require.NoError(t, err)
	assert.Equal(t, session.HeadlessStatusFailed, run.Status)
	assert.Contains(t, run.Error, "expand context references")
	assert.Equal(t, run.Error, run.TerminalReason)
	require.NotNil(t, run.ExitCode)
	assert.Equal(t, 1, *run.ExitCode)

	headlessEvents, err := store.ReadHeadlessEvents(headlessID)
	require.NoError(t, err)
	require.Len(t, headlessEvents, 3)
	assert.Equal(t, session.HeadlessEventStarted, headlessEvents[0].Type)
	assert.Equal(t, session.HeadlessEventUserMessage, headlessEvents[1].Type)
	assert.Equal(t, session.HeadlessEventFailed, headlessEvents[2].Type)
}

func TestRunOnceWithOptions_HeadlessRecordsBudgetFailure(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	store := session.NewStore(filepath.Join(dir, "sessions"))
	headlessID := "test-headless-budget-failure"

	err := runOnceWithOptions(
		context.Background(),
		llm.NewRegistry(),
		agent.NewRegistry(nil),
		nil,
		store,
		session.New("gpt-test", nil),
		contextref.Options{Root: dir},
		"",
		"gpt-test",
		"",
		nil,
		generationSettings{},
		generationSettings{},
		1,
		runOnceExecutionOptions{
			OutputFormat: outputFormatText,
			HeadlessID:   headlessID,
			Headless:     true,
		},
		true,
		"this prompt is intentionally longer than one estimated token",
	)
	require.ErrorContains(t, err, "exceed")

	run, err := store.LoadHeadlessRun(headlessID)
	require.NoError(t, err)
	assert.Equal(t, session.HeadlessStatusFailed, run.Status)
	assert.Contains(t, run.Error, "exceed")
	assert.Equal(t, run.Error, run.TerminalReason)
	require.NotNil(t, run.ExitCode)
	assert.Equal(t, 1, *run.ExitCode)

	headlessEvents, err := store.ReadHeadlessEvents(headlessID)
	require.NoError(t, err)
	require.Len(t, headlessEvents, 3)
	assert.Equal(t, session.HeadlessEventStarted, headlessEvents[0].Type)
	assert.Equal(t, session.HeadlessEventUserMessage, headlessEvents[1].Type)
	assert.Equal(t, session.HeadlessEventFailed, headlessEvents[2].Type)
}

func TestFinishHeadlessRunRecordsCancellationReason(t *testing.T) {
	t.Parallel()

	store := session.NewStore(t.TempDir())
	run := session.HeadlessRun{
		ID:     "test-headless-canceled",
		Status: session.HeadlessStatusRunning,
	}
	require.NoError(t, store.SaveHeadlessRun(run))

	loaded, err := store.LoadHeadlessRun(run.ID)
	require.NoError(t, err)

	finishHeadlessRun(store, &loaded, session.HeadlessStatusFailed, context.Canceled.Error())

	canceled, err := store.LoadHeadlessRun(run.ID)
	require.NoError(t, err)
	assert.Equal(t, session.HeadlessStatusCanceled, canceled.Status)
	assert.Equal(t, context.Canceled.Error(), canceled.Error)
	assert.Equal(t, context.Canceled.Error(), canceled.CancellationReason)
	require.NotNil(t, canceled.ExitCode)
	assert.Equal(t, 130, *canceled.ExitCode)

	headlessEvents, err := store.ReadHeadlessEvents(run.ID)
	require.NoError(t, err)
	require.Len(t, headlessEvents, 1)
	assert.Equal(t, session.HeadlessEventCanceled, headlessEvents[0].Type)
	assert.Equal(t, context.Canceled.Error(), headlessEvents[0].CancelReason)
	assert.Equal(t, context.Canceled.Error(), headlessEvents[0].TerminalReason)
}

func TestFinishHeadlessStatusClassifiesCancelledMessages(t *testing.T) {
	t.Parallel()

	status := finishHeadlessStatus(session.HeadlessStatusFailed, "operation cancel"+"led by supervisor")

	assert.Equal(t, session.HeadlessStatusCanceled, status)
}

func TestFinishHeadlessRunRecordsFailedStatus(t *testing.T) {
	t.Parallel()

	store := session.NewStore(t.TempDir())
	run := session.HeadlessRun{
		ID:     "test-headless-failed",
		Status: session.HeadlessStatusRunning,
	}
	require.NoError(t, store.SaveHeadlessRun(run))

	loaded, err := store.LoadHeadlessRun(run.ID)
	require.NoError(t, err)

	finishHeadlessRun(store, &loaded, session.HeadlessStatusFailed, "provider unavailable")

	failed, err := store.LoadHeadlessRun(run.ID)
	require.NoError(t, err)
	assert.Equal(t, session.HeadlessStatusFailed, failed.Status)
	assert.Equal(t, "provider unavailable", failed.Error)
	assert.Equal(t, "provider unavailable", failed.TerminalReason)
	require.NotNil(t, failed.ExitCode)
	assert.Equal(t, 1, *failed.ExitCode)

	headlessEvents, err := store.ReadHeadlessEvents(run.ID)
	require.NoError(t, err)
	require.Len(t, headlessEvents, 1)
	assert.Equal(t, session.HeadlessEventFailed, headlessEvents[0].Type)
}

func TestFinishHeadlessRunRecordsTimedOutStatus(t *testing.T) {
	t.Parallel()

	store := session.NewStore(t.TempDir())
	run := session.HeadlessRun{
		ID:     "test-headless-timed-out",
		Status: session.HeadlessStatusRunning,
	}
	require.NoError(t, store.SaveHeadlessRun(run))

	loaded, err := store.LoadHeadlessRun(run.ID)
	require.NoError(t, err)

	finishHeadlessRun(store, &loaded, session.HeadlessStatusFailed, context.DeadlineExceeded.Error())

	timedOut, err := store.LoadHeadlessRun(run.ID)
	require.NoError(t, err)
	assert.Equal(t, session.HeadlessStatusTimedOut, timedOut.Status)
	assert.Equal(t, context.DeadlineExceeded.Error(), timedOut.Error)
	assert.Equal(t, context.DeadlineExceeded.Error(), timedOut.TerminalReason)
	require.NotNil(t, timedOut.ExitCode)
	assert.Equal(t, 124, *timedOut.ExitCode)

	headlessEvents, err := store.ReadHeadlessEvents(run.ID)
	require.NoError(t, err)
	require.Len(t, headlessEvents, 1)
	assert.Equal(t, session.HeadlessEventTimedOut, headlessEvents[0].Type)
	assert.Equal(t, context.DeadlineExceeded.Error(), headlessEvents[0].TerminalReason)
}

func TestFinishHeadlessRunPreservesDurableCanceledStatus(t *testing.T) {
	t.Parallel()

	store := session.NewStore(t.TempDir())
	run := session.HeadlessRun{
		ID:              "test-headless-preserve-canceled",
		LastHeartbeatAt: time.Now().UTC(),
		Hostname:        "foreign-host",
		PID:             os.Getpid(),
		Status:          session.HeadlessStatusRunning,
	}
	require.NoError(t, store.SaveHeadlessRun(run))

	loaded, err := store.LoadHeadlessRun(run.ID)
	require.NoError(t, err)

	canceled, err := store.CancelHeadlessRun(run.ID, "operator requested stop")
	require.NoError(t, err)

	finishHeadlessRun(store, &loaded, session.HeadlessStatusCompleted, "")

	current, err := store.LoadHeadlessRun(run.ID)
	require.NoError(t, err)
	assert.Equal(t, session.HeadlessStatusCanceled, current.Status)
	assert.Equal(t, canceled.CancellationReason, current.CancellationReason)
	require.NotNil(t, current.ExitCode)
	assert.Equal(t, 130, *current.ExitCode)

	headlessEvents, err := store.ReadHeadlessEvents(run.ID)
	require.NoError(t, err)
	require.Len(t, headlessEvents, 1)
	assert.Equal(t, session.HeadlessEventCanceled, headlessEvents[0].Type)
}

func TestHeadlessCompletionErrorReportsPreservedTerminalStatus(t *testing.T) {
	t.Parallel()

	err := headlessCompletionError(&session.HeadlessRun{
		ID:             "test-headless-preserved-cancel",
		Status:         session.HeadlessStatusCanceled,
		TerminalReason: "operator requested stop",
	})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "test-headless-preserved-cancel")
	assert.Contains(t, err.Error(), "status canceled")
	assert.Contains(t, err.Error(), "operator requested stop")

	assert.NoError(t, headlessCompletionError(&session.HeadlessRun{
		ID:     "test-headless-completed",
		Status: session.HeadlessStatusCompleted,
	}))
	assert.NoError(t, headlessCompletionError(&session.HeadlessRun{
		ID:     "test-headless-running",
		Status: session.HeadlessStatusRunning,
	}))
}

func TestEnsureHeadlessRunCanRecordResponseStopsAfterDurableCancel(t *testing.T) {
	t.Parallel()

	store := session.NewStore(t.TempDir())
	run := session.HeadlessRun{
		ID:              "test-headless-response-after-cancel",
		LastHeartbeatAt: time.Now().UTC(),
		Hostname:        "foreign-host",
		PID:             os.Getpid(),
		Status:          session.HeadlessStatusRunning,
	}
	require.NoError(t, store.SaveHeadlessRun(run))

	loaded, err := store.LoadHeadlessRun(run.ID)
	require.NoError(t, err)

	_, err = store.CancelHeadlessRun(run.ID, "operator requested stop")
	require.NoError(t, err)

	err = ensureHeadlessRunCanRecordResponse(store, &loaded)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "status canceled")
	assert.Equal(t, session.HeadlessStatusCanceled, loaded.Status)
	assert.Equal(t, "operator requested stop", loaded.CancellationReason)
}

func TestRecordHeadlessAssistantMessageSkipsDurableTerminalStatus(t *testing.T) {
	t.Parallel()

	store := session.NewStore(t.TempDir())
	run := session.HeadlessRun{
		ID:              "test-headless-skip-assistant-after-cancel",
		LastHeartbeatAt: time.Now().UTC(),
		Hostname:        "foreign-host",
		PID:             os.Getpid(),
		Status:          session.HeadlessStatusRunning,
	}
	require.NoError(t, store.SaveHeadlessRun(run))

	loaded, err := store.LoadHeadlessRun(run.ID)
	require.NoError(t, err)

	_, err = store.CancelHeadlessRun(run.ID, "operator requested stop")
	require.NoError(t, err)

	recordHeadlessAssistantMessage(store, &loaded, 42)

	current, err := store.LoadHeadlessRun(run.ID)
	require.NoError(t, err)
	assert.Equal(t, session.HeadlessStatusCanceled, current.Status)

	log, err := store.ReadHeadlessLog(run.ID)
	require.NoError(t, err)
	assert.NotContains(t, log, "assistant_message")

	headlessEvents, err := store.ReadHeadlessEvents(run.ID)
	require.NoError(t, err)
	require.Len(t, headlessEvents, 1)
	assert.Equal(t, session.HeadlessEventCanceled, headlessEvents[0].Type)
}

func TestRecordHeadlessAssistantMessageSkipsCorruptMetadata(t *testing.T) {
	t.Parallel()

	store := session.NewStore(t.TempDir())
	run := session.HeadlessRun{
		ID:     "test-headless-skip-assistant-corrupt",
		Status: session.HeadlessStatusRunning,
	}
	require.NoError(t, store.SaveHeadlessRun(run))
	require.NoError(t, os.WriteFile(filepath.Join(store.Dir(), "headless", run.ID+".json"), []byte("{not-json"), 0o600))

	recordHeadlessAssistantMessage(store, &run, 42)

	_, err := store.ReadHeadlessLog(run.ID)
	require.ErrorIs(t, err, os.ErrNotExist)

	headlessEvents, err := store.ReadHeadlessEvents(run.ID)
	require.NoError(t, err)
	assert.Empty(t, headlessEvents)
}

func TestRecordHeadlessAssistantMessageRevivesOrphanedRun(t *testing.T) {
	t.Parallel()

	store := session.NewStore(t.TempDir())
	oldHeartbeat := time.Now().Add(-time.Hour).UTC()
	run := session.HeadlessRun{
		ID:              "test-headless-assistant-revives-orphaned",
		LastHeartbeatAt: oldHeartbeat,
		Model:           "gpt-original",
		OrphanedReason:  "no heartbeat since " + oldHeartbeat.Format(time.RFC3339),
		Status:          session.HeadlessStatusOrphaned,
		Stale:           true,
	}
	require.NoError(t, store.SaveHeadlessRun(run))
	run.Model = "gpt-newer-response"

	recordHeadlessAssistantMessage(store, &run, 42)

	loaded, err := store.LoadHeadlessRun(run.ID)
	require.NoError(t, err)
	assert.Equal(t, session.HeadlessStatusRunning, loaded.Status)
	assert.False(t, loaded.Stale)
	assert.Empty(t, loaded.OrphanedReason)

	log, err := store.ReadHeadlessLog(run.ID)
	require.NoError(t, err)
	assert.Contains(t, log, "assistant_message")

	headlessEvents, err := store.ReadHeadlessEvents(run.ID)
	require.NoError(t, err)
	require.Len(t, headlessEvents, 1)
	assert.Equal(t, session.HeadlessEventAssistantMessage, headlessEvents[0].Type)
	assert.Equal(t, session.HeadlessStatusRunning, headlessEvents[0].Status)
	assert.Equal(t, "gpt-newer-response", headlessEvents[0].Model)
	assert.Empty(t, headlessEvents[0].OrphanedReason)
}

func TestFormatHeadlessRun(t *testing.T) {
	t.Parallel()

	completedAt := time.Date(2026, 5, 3, 10, 2, 0, 0, time.UTC)
	run := session.HeadlessRun{
		StartedAt:        time.Date(2026, 5, 3, 10, 0, 0, 0, time.UTC),
		UpdatedAt:        time.Date(2026, 5, 3, 10, 1, 0, 0, time.UTC),
		LastHeartbeatAt:  time.Date(2026, 5, 3, 10, 1, 30, 0, time.UTC),
		CompletedAt:      &completedAt,
		ID:               "headless-id",
		ParentRunID:      "parent-headless-id",
		SessionID:        "session-id",
		LogPath:          "/tmp/headless.log",
		EventsPath:       "/tmp/headless.events.jsonl",
		Model:            "gpt-test",
		Hostname:         "host-one",
		StartedCommand:   "atteler chat once hello --headless",
		CommandArgs:      []string{"atteler", "chat", "once", "hello", "--headless"},
		ChildRunIDs:      []string{"child-headless-id"},
		StartMethod:      "headless",
		CWD:              "/repo",
		PID:              123,
		ParentPID:        12,
		ProcessGroupID:   10,
		LogMaxChunkBytes: 1048576,
		LogMaxChunks:     8,
		Status:           session.HeadlessStatusRunning,
	}

	got := formatHeadlessRun(run)
	for _, want := range []string{
		"headless-id",
		"status=running",
		"session=session-id",
		"model=gpt-test",
		"started=2026-05-03T10:00:00Z",
		"updated=2026-05-03T10:01:00Z",
		"heartbeat=2026-05-03T10:01:30Z",
		"completed=2026-05-03T10:02:00Z",
		"log=/tmp/headless.log",
		"events=/tmp/headless.events.jsonl",
		"log_chunk_pattern=/tmp/headless.log.NNNNNN",
		"pid=123",
		"ppid=12",
		"pgid=10",
		"parent_run=parent-headless-id",
		`child_runs=["child-headless-id"]`,
		"host=host-one",
		"cwd=/repo",
		"start_method=headless",
		"command=atteler chat once hello --headless",
		`command_args=["atteler","chat","once","hello","--headless"]`,
		"log_max_chunk_bytes=1048576",
		"log_max_chunks=8",
	} {
		assert.Contains(t, got, want)
	}

	run.Status = session.HeadlessStatusStale
	run.StaleReason = "process pid 123 is not running\ncheck\tlogs"
	stale := formatHeadlessRun(run)
	assert.Contains(t, stale, "status=stale")
	assert.Contains(t, stale, "stale_reason=process pid 123 is not running\\ncheck logs")
	assert.Contains(t, stale, "recover=atteler session recover-headless")
}
