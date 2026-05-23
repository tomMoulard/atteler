package main

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/tommoulard/atteler/pkg/agent"
	"github.com/tommoulard/atteler/pkg/contextref"
	"github.com/tommoulard/atteler/pkg/llm"
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

func TestSaveRecordedResponse_IncludesModelMode(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), "response.json")
	require.NoError(t, saveRecordedResponse(
		path,
		llm.CompleteParams{
			Model:     "gpt-5.4",
			ModelMode: llm.ModelModeFast,
			Messages:  []llm.Message{{Role: llm.RoleUser, Content: "hello"}},
		},
		nil,
		&llm.Response{Content: "answer", Model: "gpt-5.4"},
	))

	var record responseRecordFile

	data, err := os.ReadFile(path)
	require.NoError(t, err)
	require.NoError(t, json.Unmarshal(data, &record))
	assert.Equal(t, llm.ModelModeFast, record.Request.ModelMode)
}

func TestWriteRunOnceResult_JSONAndHeadlessText(t *testing.T) {
	t.Parallel()

	result := runOnceResult{
		SessionID:               "session-id",
		SessionPath:             "/tmp/session.json",
		AgentLoopCheckpointPath: "/tmp/session.agentloop.jsonl",
		HeadlessID:              "headless-id",
		Model:                   "gpt-test",
		ModelMode:               llm.ModelModeFast,
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
	assert.Equal(t, result.ModelMode, decoded.ModelMode)
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
		generationSettings{ModelMode: llm.ModelModeFast},
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
	assert.Equal(t, llm.ModelModeFast, run.ModelMode)
	assert.NotNil(t, run.CompletedAt)

	log, err := store.ReadHeadlessLog(headlessID)
	require.NoError(t, err)
	assert.Contains(t, log, "started")
	assert.Contains(t, log, "assistant_message")
	assert.Contains(t, log, "completed")

	runs, err := store.ListHeadlessRuns()
	require.NoError(t, err)
	require.Len(t, runs, 1)
	assert.Equal(t, headlessID, runs[0].ID)
	require.NoError(t, streamHeadlessLog(context.Background(), store, headlessID))
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
	assert.Equal(t, session.HeadlessStatusFailed, canceled.Status)
	assert.Equal(t, context.Canceled.Error(), canceled.Error)
	assert.Equal(t, context.Canceled.Error(), canceled.CancellationReason)
	require.NotNil(t, canceled.ExitCode)
	assert.Equal(t, 1, *canceled.ExitCode)
}

func TestFormatHeadlessRun(t *testing.T) {
	t.Parallel()

	run := session.HeadlessRun{
		ID:        "headless-id",
		SessionID: "session-id",
		LogPath:   "/tmp/headless.log",
		Model:     "gpt-test",
		Status:    session.HeadlessStatusRunning,
		StartedAt: time.Date(2026, 5, 3, 10, 0, 0, 0, time.UTC),
		UpdatedAt: time.Date(2026, 5, 3, 10, 1, 0, 0, time.UTC),
	}

	got := formatHeadlessRun(run)
	for _, want := range []string{
		"headless-id",
		"status=running",
		"session=session-id",
		"model=gpt-test",
		"started=2026-05-03T10:00:00Z",
		"updated=2026-05-03T10:01:00Z",
		"log=/tmp/headless.log",
	} {
		assert.Contains(t, got, want)
	}

	run.Status = session.HeadlessStatusStale
	run.StaleReason = "process pid 123 is not running"
	stale := formatHeadlessRun(run)
	assert.Contains(t, stale, "status=stale")
	assert.Contains(t, stale, "recover=atteler session recover-headless")
}
