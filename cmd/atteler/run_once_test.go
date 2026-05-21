package main

import (
	"bytes"
	"context"
	"encoding/json"
	"path/filepath"
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
}
