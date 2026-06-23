package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/tommoulard/atteler/pkg/agent"
	"github.com/tommoulard/atteler/pkg/autonomy"
	"github.com/tommoulard/atteler/pkg/config"
	"github.com/tommoulard/atteler/pkg/contextpack"
	"github.com/tommoulard/atteler/pkg/contextref"
	"github.com/tommoulard/atteler/pkg/events"
	"github.com/tommoulard/atteler/pkg/llm"
	"github.com/tommoulard/atteler/pkg/modelroute"
	"github.com/tommoulard/atteler/pkg/permission"
	"github.com/tommoulard/atteler/pkg/session"
	attshell "github.com/tommoulard/atteler/pkg/shell"
	"github.com/tommoulard/atteler/pkg/vector"
)

//nolint:govet // Test helper field order keeps call assertions next to captured request state.
type runOnceCostProvider struct {
	calls    int
	response *llm.Response
	params   *llm.CompleteParams
	name     string
	models   []string
}

func (p *runOnceCostProvider) Name() string { return p.name }

func (p *runOnceCostProvider) Models() []string { return append([]string(nil), p.models...) }

func (p *runOnceCostProvider) FetchModels(context.Context) ([]string, error) {
	return p.Models(), nil
}

func (p *runOnceCostProvider) HealthCheck(context.Context) error { return nil }

func (p *runOnceCostProvider) ModelContextWindow(string) int { return 128_000 }

func (p *runOnceCostProvider) Complete(_ context.Context, params llm.CompleteParams) (*llm.Response, error) {
	if p.response == nil {
		return nil, errors.New("missing response")
	}

	p.params = &params
	p.calls++

	return p.response, nil
}

type runOnceCapabilityProvider struct {
	routeFakeProvider
	capabilities llm.ProviderCapabilities
}

func (p runOnceCapabilityProvider) Capabilities() llm.ProviderCapabilities {
	return p.capabilities
}

type runOnceSequenceProvider struct {
	name      string
	model     string
	responses []*llm.Response
	requests  []llm.CompleteParams
}

func (p *runOnceSequenceProvider) Name() string { return p.name }

func (p *runOnceSequenceProvider) Models() []string { return []string{p.model} }

func (p *runOnceSequenceProvider) FetchModels(context.Context) ([]string, error) {
	return p.Models(), nil
}

func (p *runOnceSequenceProvider) HealthCheck(context.Context) error { return nil }

func (p *runOnceSequenceProvider) ModelContextWindow(string) int { return 128_000 }

func (p *runOnceSequenceProvider) Complete(_ context.Context, params llm.CompleteParams) (*llm.Response, error) {
	p.requests = append(p.requests, params)
	if len(p.responses) == 0 {
		return nil, errors.New("missing response")
	}

	resp := p.responses[0]
	p.responses = append([]*llm.Response(nil), p.responses[1:]...)

	return resp, nil
}

func TestRunOnceCompleteWithAutonomyTranscriptReturnsToolMessages(t *testing.T) {
	t.Parallel()

	provider := &runOnceSequenceProvider{
		name:  "openai",
		model: "openai/gpt-test",
		responses: []*llm.Response{
			{
				Content: "need file",
				Model:   "openai/gpt-test",
				ToolCalls: []llm.ToolCall{{
					ID:    "tool-1",
					Name:  llm.ToolNameBash,
					Input: map[string]any{"command": "printf tool-output"},
				}},
				StopReason: llm.StopToolUse,
			},
			{
				Content:      "done",
				Model:        "openai/gpt-test",
				StopReason:   llm.StopEndTurn,
				InputTokens:  3,
				OutputTokens: 2,
			},
		},
	}
	reg := llm.NewRegistry()
	reg.Register(provider)

	resp, messages, replayReferences, err := runOnceCompleteWithAutonomyTranscript(
		context.Background(),
		reg,
		llm.CompleteParams{
			Model:    "openai/gpt-test",
			Messages: []llm.Message{{Role: llm.RoleUser, Content: "use a tool"}},
			Tools:    []llm.ToolDefinition{{Name: llm.ToolNameBash}},
		},
		nil,
		llm.AgentLoopBudget{MaxModelCalls: 3, MaxToolCalls: 2},
		autonomy.Full,
		agent.Agent{},
		false,
		0,
		responseRecordOptions{},
		nil,
		false,
		"",
		nil,
		attshell.AuditContext{Caller: "test"},
	)
	require.NoError(t, err)
	require.NotNil(t, resp)
	assert.Equal(t, "done", resp.Content)
	assert.Empty(t, replayReferences)
	require.Len(t, provider.requests, 2)
	require.NotEmpty(t, messages)

	var sawToolCall, sawToolResult bool

	for index := range messages {
		message := &messages[index]
		sawToolCall = sawToolCall || len(message.ToolCalls) > 0
		sawToolResult = sawToolResult || message.ToolResult != nil
	}

	assert.True(t, sawToolCall)
	assert.True(t, sawToolResult)
	assert.Greater(t, len(provider.requests[1].Messages), len(provider.requests[0].Messages))
}

func TestRunOnce_ReplaysResponseWithoutProvider(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()

	replayPath := filepath.Join(dir, "response.json")
	if err := saveRecordedResponse(t.Context(),
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
		contextref.ReferenceManifest{},
		"",
		nil,
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

	require.Len(t, loaded.ProviderCalls, 1)
	require.Len(t, loaded.ProviderCalls[0].ReferencedFiles, 1)
	replayRef := loaded.ProviderCalls[0].ReferencedFiles[0]
	assert.Equal(t, replayPath, replayRef.Path)
	assert.Equal(t, filepath.Base(replayPath), replayRef.LogicalPath)
	assert.Equal(t, "response_fixture", replayRef.Kind)
	assert.Equal(t, "replay_response", replayRef.Source)
	assert.NotEmpty(t, replayRef.SHA256)
	assert.Positive(t, replayRef.SizeBytes)
}

//nolint:paralleltest // Captures process stdout while saveRecordedResponse prints the user-facing privacy hint.
func TestSaveRecordedResponse_PrintsPrivacyHintForAttelerFixture(t *testing.T) {
	dir := t.TempDir()
	recordPath := filepath.Join(dir, ".atteler", "fixtures", "once.json")

	stdout := captureProcessOutput(t, &os.Stdout)
	require.NoError(t, saveRecordedResponse(t.Context(),
		recordPath,
		llm.CompleteParams{
			Model: "gpt-test",
			Messages: []llm.Message{{
				Role:    llm.RoleUser,
				Content: "hello",
			}},
		},
		nil,
		&llm.Response{Content: "recorded answer", Model: "gpt-test"},
	))

	line := requireLineBefore(t, stdout.lines, time.Second)
	assert.Contains(t, line, "privacy_hint=")
	assert.Contains(t, line, "ignored/private by default")
	assert.Contains(t, line, "review and redact")
}

func TestSaveRecordedResponse_IncludesResponseFormat(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	recordPath := filepath.Join(dir, "response.json")

	require.NoError(t, saveRecordedResponse(t.Context(),
		recordPath,
		llm.CompleteParams{
			Model: "gpt-test",
			Messages: []llm.Message{{
				Role:    llm.RoleUser,
				Content: "hello",
			}},
			ResponseFormat: &llm.ResponseFormat{
				Type:   llm.ResponseFormatJSONSchema,
				Name:   "answer",
				Strict: true,
				Schema: map[string]any{
					"type": "object",
					"properties": map[string]any{
						"answer": map[string]any{"type": "string"},
					},
				},
			},
		},
		nil,
		&llm.Response{Content: "recorded answer", Model: "gpt-test"},
	))

	data, err := os.ReadFile(recordPath)
	require.NoError(t, err)

	var record responseRecordFile
	require.NoError(t, json.Unmarshal(data, &record))
	require.NotNil(t, record.Request.ResponseFormat)
	assert.Equal(t, llm.ResponseFormatJSONSchema, record.Request.ResponseFormat.Type)
	assert.Equal(t, "answer", record.Request.ResponseFormat.Name)
	assert.True(t, record.Request.ResponseFormat.Strict)
	assert.Equal(t, "object", record.Request.ResponseFormat.Schema["type"])
}

func TestRunOnceComplete_CostBudgetFailsClosedWithoutPricing(t *testing.T) {
	t.Parallel()

	provider := &runOnceCostProvider{
		name:   "ollama",
		models: []string{"llama3.2"},
		response: &llm.Response{
			Content:    "unpriced answer",
			Provider:   "ollama",
			Model:      "llama3.2",
			StopReason: llm.StopEndTurn,
		},
	}
	reg := llm.NewRegistry()
	reg.Register(provider)

	_, err := runOnceComplete(
		context.Background(),
		reg,
		llm.CompleteParams{Model: "ollama/llama3.2"},
		nil,
		llm.AgentLoopBudget{MaxCostMicros: 1},
		0,
		responseRecordOptions{},
		nil,
		false,
		"",
		nil,
		attshell.AuditContext{},
	)
	require.ErrorIs(t, err, llm.ErrAgentLoopCostPricingUnavailable)
	assert.Contains(t, err.Error(), "agent_loop.max_cost_micros")
	assert.Zero(t, provider.calls, "unpriced cost budgets should fail before model usage")
}

func TestRunOnceComplete_CostBudgetPassesWithCatalogPricing(t *testing.T) {
	t.Parallel()

	provider := &runOnceCostProvider{
		name:   "openai",
		models: []string{"gpt-4.1-mini"},
		response: &llm.Response{
			Content:      "priced answer",
			Provider:     "openai",
			Model:        "gpt-4.1-mini",
			StopReason:   llm.StopEndTurn,
			InputTokens:  1,
			OutputTokens: 1,
		},
	}
	reg := llm.NewRegistry()
	reg.Register(provider)

	resp, err := runOnceComplete(
		context.Background(),
		reg,
		llm.CompleteParams{Model: "openai/gpt-4.1-mini"},
		nil,
		llm.AgentLoopBudget{MaxCostMicros: 10},
		0,
		responseRecordOptions{},
		nil,
		false,
		"",
		nil,
		attshell.AuditContext{},
	)
	require.NoError(t, err)
	assert.Equal(t, "priced answer", resp.Content)
	assert.Equal(t, 1, provider.calls)
}

func TestRunOnceWithOptions_AutonomyControlsToolExposure(t *testing.T) {
	t.Parallel()

	for _, tt := range []struct {
		level     autonomy.Level
		name      string
		wantTools bool
	}{
		{level: autonomy.Low, name: "low", wantTools: false},
		{level: autonomy.Medium, name: "medium", wantTools: true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			dir := t.TempDir()
			provider := &runOnceCostProvider{
				name:   "openai",
				models: []string{"gpt-4.1-mini"},
				response: &llm.Response{
					Content:    "answer",
					Provider:   "openai",
					Model:      "gpt-4.1-mini",
					StopReason: llm.StopEndTurn,
				},
			}
			reg := llm.NewRegistry()
			reg.Register(provider)

			err := runOnceWithOptions(
				context.Background(),
				reg,
				agent.NewRegistry(nil),
				nil,
				session.NewStore(filepath.Join(dir, "sessions")),
				session.New("openai/gpt-4.1-mini", nil),
				contextref.Options{Root: dir},
				"",
				contextref.ReferenceManifest{},
				"",
				nil,
				"openai/gpt-4.1-mini",
				"",
				nil,
				generationSettings{},
				generationSettings{},
				0,
				runOnceExecutionOptions{Autonomy: tt.level},
				true,
				"implement the change",
			)
			require.NoError(t, err)
			require.Equal(t, 1, provider.calls)
			require.NotNil(t, provider.params)
			assert.Equal(t, tt.wantTools, len(provider.params.Tools) > 0)
			assertRunOnceRequestContains(t, *provider.params, "Autonomy: "+tt.level.String())
		})
	}
}

func TestRunOnceWithOptions_LowAutonomyDoesNotPersistSession(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	provider := &runOnceCostProvider{
		name:   "openai",
		models: []string{"gpt-4.1-mini"},
		response: &llm.Response{
			Content:    "plan only",
			Provider:   "openai",
			Model:      "gpt-4.1-mini",
			StopReason: llm.StopEndTurn,
		},
	}
	reg := llm.NewRegistry()
	reg.Register(provider)

	store := session.NewStore(filepath.Join(dir, "sessions"))
	sessionState := session.New("openai/gpt-4.1-mini", nil)
	sessionPath := store.Path(sessionState.ID)

	err := runOnceWithOptions(
		context.Background(),
		reg,
		agent.NewRegistry(nil),
		nil,
		store,
		sessionState,
		contextref.Options{Root: dir},
		"",
		contextref.ReferenceManifest{},
		"",
		nil,
		"openai/gpt-4.1-mini",
		"",
		nil,
		generationSettings{},
		generationSettings{},
		0,
		runOnceExecutionOptions{Autonomy: autonomy.Low},
		true,
		"implement the change",
	)

	require.NoError(t, err)
	assert.NoFileExists(t, sessionPath)
	assert.NoFileExists(t, agentLoopCheckpointPath(sessionPath))
	require.NotNil(t, provider.params)
	assertRunOnceRequestContains(t, *provider.params, "Autonomy: low")
}

func TestRunOnceWithOptions_AutoLoadsProjectInstructions(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	require.NoError(t, os.MkdirAll(filepath.Join(dir, ".git"), 0o750))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "AGENTS.md"), []byte("Always run make test.\n"), 0o600))

	provider := &runOnceCostProvider{
		name:   "openai",
		models: []string{"gpt-4.1-mini"},
		response: &llm.Response{
			Content:    "answer",
			Provider:   "openai",
			Model:      "gpt-4.1-mini",
			StopReason: llm.StopEndTurn,
		},
	}
	reg := llm.NewRegistry()
	reg.Register(provider)

	err := runOnceWithOptions(
		context.Background(),
		reg,
		agent.NewRegistry(nil),
		nil,
		session.NewStore(filepath.Join(dir, "sessions")),
		session.New("openai/gpt-4.1-mini", nil),
		contextref.Options{Root: dir},
		"",
		contextref.ReferenceManifest{},
		"",
		nil,
		"openai/gpt-4.1-mini",
		"",
		nil,
		generationSettings{},
		generationSettings{},
		0,
		runOnceExecutionOptions{Autonomy: autonomy.Low},
		true,
		"implement the change",
	)

	require.NoError(t, err)
	require.NotNil(t, provider.params)
	assertRunOnceRequestContains(t, *provider.params, "<project_instructions")
	assertRunOnceRequestContains(t, *provider.params, "Always run make test.")
}

func TestRunOnceWithOptions_LowAutonomyBlocksHeadlessMetadataWrites(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	store := session.NewStore(filepath.Join(dir, "sessions"))

	err := runOnceWithOptions(
		context.Background(),
		nil,
		nil,
		nil,
		store,
		session.New("gpt-test", nil),
		contextref.Options{Root: dir},
		"",
		contextref.ReferenceManifest{},
		"",
		nil,
		"gpt-test",
		"",
		nil,
		generationSettings{},
		generationSettings{},
		0,
		runOnceExecutionOptions{
			Autonomy:   autonomy.Low,
			Headless:   true,
			HeadlessID: "low-headless",
		},
		true,
		"implement the change",
	)

	require.Error(t, err)
	assert.Contains(t, err.Error(), "autonomy low blocks file writes")
	assert.Contains(t, err.Error(), "--headless run metadata")
	assert.NoDirExists(t, filepath.Join(store.Dir(), "headless"))
}

func assertRunOnceRequestContains(t *testing.T, params llm.CompleteParams, want string) {
	t.Helper()

	for _, message := range params.Messages {
		if strings.Contains(message.Content, want) {
			return
		}
	}

	require.Failf(t, "missing request content", "request messages do not contain %q: %+v", want, params.Messages)
}

func TestRunOnceComplete_ReplayCostBudgetFailsClosedWithoutPricing(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	replayPath := filepath.Join(dir, "response.json")
	require.NoError(t, saveRecordedResponse(t.Context(),
		replayPath,
		llm.CompleteParams{Model: "gpt-test", Messages: []llm.Message{{Role: llm.RoleUser, Content: "hello"}}},
		nil,
		&llm.Response{
			Content:      "recorded answer",
			Provider:     "test",
			Model:        "gpt-test",
			InputTokens:  1,
			OutputTokens: 1,
		},
	))

	_, err := runOnceComplete(
		context.Background(),
		llm.NewRegistry(),
		llm.CompleteParams{Model: "test/gpt-test"},
		nil,
		llm.AgentLoopBudget{MaxCostMicros: 10},
		0,
		responseRecordOptions{ReplayPath: replayPath},
		nil,
		false,
		"",
		nil,
		attshell.AuditContext{},
	)
	require.ErrorIs(t, err, llm.ErrAgentLoopCostPricingUnavailable)
	assert.Contains(t, err.Error(), "agent_loop.max_cost_micros")
}

func TestRunOnceComplete_ReplayCostBudgetPassesWithCatalogPricing(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	replayPath := filepath.Join(dir, "response.json")
	require.NoError(t, saveRecordedResponse(t.Context(),
		replayPath,
		llm.CompleteParams{Model: "openai/gpt-4.1-mini", Messages: []llm.Message{{Role: llm.RoleUser, Content: "hello"}}},
		nil,
		&llm.Response{
			Content:      "recorded answer",
			Provider:     "openai",
			Model:        "gpt-4.1-mini",
			InputTokens:  1,
			OutputTokens: 1,
		},
	))

	resp, err := runOnceComplete(
		context.Background(),
		llm.NewRegistry(),
		llm.CompleteParams{Model: "openai/gpt-4.1-mini"},
		nil,
		llm.AgentLoopBudget{MaxCostMicros: 10},
		0,
		responseRecordOptions{ReplayPath: replayPath},
		nil,
		false,
		"",
		nil,
		attshell.AuditContext{},
	)
	require.NoError(t, err)
	assert.Equal(t, "recorded answer", resp.Content)
}

func TestRunOnceComplete_ReplayTokenBudgetsAreEnforced(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	replayPath := filepath.Join(dir, "response.json")
	require.NoError(t, saveRecordedResponse(t.Context(),
		replayPath,
		llm.CompleteParams{Model: "openai/gpt-4.1-mini", Messages: []llm.Message{{Role: llm.RoleUser, Content: "hello"}}},
		nil,
		&llm.Response{
			Content:      "recorded answer",
			Provider:     "openai",
			Model:        "gpt-4.1-mini",
			InputTokens:  7,
			OutputTokens: 3,
		},
	))

	_, err := runOnceComplete(
		context.Background(),
		llm.NewRegistry(),
		llm.CompleteParams{Model: "openai/gpt-4.1-mini"},
		nil,
		llm.AgentLoopBudget{MaxInputTokens: 6},
		0,
		responseRecordOptions{ReplayPath: replayPath},
		nil,
		false,
		"",
		nil,
		attshell.AuditContext{},
	)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "agent_loop.max_input_tokens")
	assert.Contains(t, err.Error(), "input token budget exceeded")
}

func TestRunOnceComplete_ReplayTokenBudgetFailsClosedWithoutUsageMetadata(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	replayPath := filepath.Join(dir, "response.json")
	require.NoError(t, saveRecordedResponse(t.Context(),
		replayPath,
		llm.CompleteParams{Model: "openai/gpt-4.1-mini", Messages: []llm.Message{{Role: llm.RoleUser, Content: "hello"}}},
		nil,
		&llm.Response{
			Content:  "recorded answer",
			Provider: "openai",
			Model:    "gpt-4.1-mini",
		},
	))

	_, err := runOnceComplete(
		context.Background(),
		llm.NewRegistry(),
		llm.CompleteParams{Model: "openai/gpt-4.1-mini"},
		nil,
		llm.AgentLoopBudget{MaxOutputTokens: 100},
		0,
		responseRecordOptions{ReplayPath: replayPath},
		nil,
		false,
		"",
		nil,
		attshell.AuditContext{},
	)
	require.ErrorIs(t, err, llm.ErrAgentLoopTokenUsageUnavailable)
	assert.Contains(t, err.Error(), "agent_loop.max_output_tokens")
	assert.Contains(t, err.Error(), "token budget could not be enforced")

	partialReplayPath := filepath.Join(dir, "partial-response.json")
	require.NoError(t, saveRecordedResponse(t.Context(),
		partialReplayPath,
		llm.CompleteParams{Model: "openai/gpt-4.1-mini", Messages: []llm.Message{{Role: llm.RoleUser, Content: "hello"}}},
		nil,
		&llm.Response{
			Content:      "recorded answer",
			Provider:     "openai",
			Model:        "gpt-4.1-mini",
			OutputTokens: 3,
		},
	))

	_, err = runOnceComplete(
		context.Background(),
		llm.NewRegistry(),
		llm.CompleteParams{Model: "openai/gpt-4.1-mini"},
		nil,
		llm.AgentLoopBudget{MaxInputTokens: 100},
		0,
		responseRecordOptions{ReplayPath: partialReplayPath},
		nil,
		false,
		"",
		nil,
		attshell.AuditContext{},
	)
	require.ErrorIs(t, err, llm.ErrAgentLoopTokenUsageUnavailable)
	assert.Contains(t, err.Error(), "agent_loop.max_input_tokens")
	assert.Contains(t, err.Error(), "input token usage unavailable")

	combinedReplayPath := filepath.Join(dir, "combined-response.json")
	require.NoError(t, saveRecordedResponse(t.Context(),
		combinedReplayPath,
		llm.CompleteParams{Model: "openai/gpt-4.1-mini", Messages: []llm.Message{{Role: llm.RoleUser, Content: "hello"}}},
		nil,
		&llm.Response{
			Content:     "recorded answer",
			Provider:    "openai",
			Model:       "gpt-4.1-mini",
			InputTokens: 5,
		},
	))

	_, err = runOnceComplete(
		context.Background(),
		llm.NewRegistry(),
		llm.CompleteParams{Model: "openai/gpt-4.1-mini"},
		nil,
		llm.AgentLoopBudget{MaxInputTokens: 100, MaxOutputTokens: 100},
		0,
		responseRecordOptions{ReplayPath: combinedReplayPath},
		nil,
		false,
		"",
		nil,
		attshell.AuditContext{},
	)
	require.ErrorIs(t, err, llm.ErrAgentLoopTokenUsageUnavailable)
	assert.Contains(t, err.Error(), "agent_loop.max_output_tokens")
	assert.Contains(t, err.Error(), "output token usage unavailable")
	assert.NotContains(t, err.Error(), "agent_loop.max_input_tokens")
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
		nil,
		generationSettings{},
		generationSettings{},
		false,
		true,
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
	require.NoError(t, saveRecordedResponse(t.Context(),
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
		contextref.ReferenceManifest{},
		"",
		nil,
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

func TestRunOnceWithOptions_EmitsRouteDecisionForModelRole(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	replayPath := filepath.Join(dir, "response.json")
	require.NoError(t, saveRecordedResponse(t.Context(),
		replayPath,
		llm.CompleteParams{Model: "openai/gpt-4.1-mini", Messages: []llm.Message{{Role: llm.RoleUser, Content: "plan this"}}},
		nil,
		&llm.Response{
			Content:           "recorded plan",
			Provider:          "openai",
			Model:             "gpt-4.1-mini",
			Latency:           21 * time.Millisecond,
			FirstTokenLatency: 5 * time.Millisecond,
			InputTokens:       25,
			OutputTokens:      10,
		},
	))

	registry := llm.NewRegistry()
	registry.Register(routeFakeProvider{name: "openai", models: []string{"gpt-4.1", "gpt-4.1-mini"}})
	require.NoError(t, registry.SetModelRole("planner", llm.ModelRole{
		Preferred:            "openai/gpt-4.1",
		FallbackModels:       []string{"openai/gpt-4.1-mini"},
		RequiredCapabilities: []string{modelroute.CapabilityJSONSchema},
		MaxCostUSD:           0.00008,
	}))

	var eventLog bytes.Buffer

	err := runOnceWithOptions(
		context.Background(),
		registry,
		agent.NewRegistry(nil),
		events.NewRunnerWithLogger(nil, &eventLog),
		session.NewStore(filepath.Join(dir, "sessions")),
		session.New("planner", nil),
		contextref.Options{Root: dir},
		"",
		contextref.ReferenceManifest{},
		"",
		nil,
		"planner",
		"",
		nil,
		generationSettings{},
		generationSettings{},
		0,
		runOnceExecutionOptions{
			OutputFormat: outputFormatText,
			Headless:     true,
			Response:     responseRecordOptions{ReplayPath: replayPath},
		},
		false,
		"plan this",
	)
	require.NoError(t, err)

	log := eventLog.String()
	assert.Contains(t, log, "event:route_decision")
	assert.Contains(t, log, "phase=estimated")
	assert.Contains(t, log, "model_role=planner")
	assert.Contains(t, log, "phase=actual")
	assert.Contains(t, log, "selected=openai/gpt-4.1-mini")
	assert.Contains(t, log, "fallback_order=openai/gpt-4.1-mini")
	assert.Contains(t, log, "constraints=")
	assert.Contains(t, log, modelroute.ConstraintRequiredCapabilities)
	assert.Contains(t, log, modelroute.ConstraintBudget)
	assert.Contains(t, log, modelroute.ConstraintRuntimeAvailability)
	assert.Contains(t, log, "actual_selected=openai/gpt-4.1-mini")
	assert.Contains(t, log, "actual_cost=")
	assert.Contains(t, log, "actual_latency_ms=21")
	assert.Contains(t, log, "actual_ttft_ms=5")
	assert.Contains(t, log, "actual_input_tokens=25")
	assert.Contains(t, log, "actual_output_tokens=10")
}

func TestRunOnceWithOptions_ModelRoleInfersToolCapabilityFromOneShotRequest(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	replayPath := filepath.Join(dir, "response.json")
	require.NoError(t, saveRecordedResponse(t.Context(),
		replayPath,
		llm.CompleteParams{Model: "toolbox/small", Messages: []llm.Message{{Role: llm.RoleUser, Content: "fix this"}}},
		nil,
		&llm.Response{
			Content:      "recorded fix",
			Provider:     "toolbox",
			Model:        "small",
			InputTokens:  12,
			OutputTokens: 8,
		},
	))

	registry := llm.NewRegistry()
	registry.Register(runOnceCapabilityProvider{
		routeFakeProvider: routeFakeProvider{name: "basic", models: []string{"small"}},
		capabilities: llm.ProviderCapabilities{
			SupportsChatCompletions: true,
		},
	})
	registry.Register(runOnceCapabilityProvider{
		routeFakeProvider: routeFakeProvider{name: "toolbox", models: []string{"small"}},
		capabilities: llm.ProviderCapabilities{
			SupportsChatCompletions: true,
			SupportsTools:           true,
		},
	})
	require.NoError(t, registry.SetModelRole("fast_coder", llm.ModelRole{
		Preferred:      "basic/small",
		FallbackModels: []string{"toolbox/small"},
	}))

	var eventLog bytes.Buffer

	err := runOnceWithOptions(
		context.Background(),
		registry,
		agent.NewRegistry(nil),
		events.NewRunnerWithLogger(nil, &eventLog),
		session.NewStore(filepath.Join(dir, "sessions")),
		session.New("fast_coder", nil),
		contextref.Options{Root: dir},
		"",
		contextref.ReferenceManifest{},
		"",
		nil,
		"fast_coder",
		"",
		nil,
		generationSettings{},
		generationSettings{},
		0,
		runOnceExecutionOptions{
			OutputFormat: outputFormatText,
			Headless:     true,
			Response:     responseRecordOptions{ReplayPath: replayPath},
		},
		false,
		"fix this",
	)
	require.NoError(t, err)

	log := eventLog.String()
	assert.Contains(t, log, "event:route_decision")
	assert.Contains(t, log, "model_role=fast_coder")
	assert.Contains(t, log, "selected=toolbox/small")
	assert.Contains(t, log, "fallback_order=toolbox/small")
	assert.Contains(t, log, modelroute.ConstraintRequiredCapabilities)
	assert.Contains(t, log, modelroute.ReasonMissingCapability)
	assert.Contains(t, log, "actual_selected=toolbox/small")
}

func TestRunOnceWithOptions_AgentModelRoleBypassesAgentCatalogRoute(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	replayPath := filepath.Join(dir, "response.json")
	require.NoError(t, saveRecordedResponse(t.Context(),
		replayPath,
		llm.CompleteParams{Model: "openai/gpt-4.1-mini", Messages: []llm.Message{{Role: llm.RoleUser, Content: "plan this"}}},
		nil,
		&llm.Response{
			Content:      "recorded plan",
			Provider:     "openai",
			Model:        "gpt-4.1-mini",
			InputTokens:  25,
			OutputTokens: 10,
		},
	))

	registry := llm.NewRegistry()
	registry.Register(routeFakeProvider{name: "openai", models: []string{"gpt-4.1", "gpt-4.1-mini"}})
	require.NoError(t, registry.SetModelRole("planner", llm.ModelRole{
		Preferred:            "openai/gpt-4.1",
		FallbackModels:       []string{"openai/gpt-4.1-mini"},
		RequiredCapabilities: []string{modelroute.CapabilityJSONSchema},
		MaxCostUSD:           0.00008,
	}))

	var eventLog bytes.Buffer

	err := runOnceWithOptions(
		context.Background(),
		registry,
		agent.NewRegistry(map[string]config.AgentConfig{
			"reviewer": {
				Model: "planner",
				RoutingPolicy: config.RoutingPolicyConfig{
					RequiredCapabilities: []string{modelroute.CapabilityJSONSchema},
				},
			},
		}),
		events.NewRunnerWithLogger(nil, &eventLog),
		session.NewStore(filepath.Join(dir, "sessions")),
		session.New("", nil),
		contextref.Options{Root: dir},
		"",
		contextref.ReferenceManifest{},
		"",
		nil,
		"",
		"reviewer",
		nil,
		generationSettings{},
		generationSettings{},
		0,
		runOnceExecutionOptions{
			OutputFormat: outputFormatText,
			Headless:     true,
			Response:     responseRecordOptions{ReplayPath: replayPath},
		},
		false,
		"plan this",
	)
	require.NoError(t, err)

	log := eventLog.String()
	assert.Contains(t, log, "event:route_decision")
	assert.Contains(t, log, "agent=reviewer")
	assert.Contains(t, log, "model_role=planner")
	assert.Contains(t, log, "selected=openai/gpt-4.1-mini")
	assert.Contains(t, log, modelroute.ConstraintRequiredCapabilities)
}

func TestRunOnceWithOptions_AppendsWorkspaceVectorContext(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	require.NoError(t, os.MkdirAll(filepath.Join(dir, "docs"), 0o750))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "docs", "auth.md"), []byte("OAuth callback state validation and token exchange retry notes."), 0o600))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "docs", "shell.md"), []byte("Shell process output capture and timeout notes."), 0o600))

	replayPath := filepath.Join(dir, "response.json")
	recordPath := filepath.Join(dir, "recorded-request.json")

	require.NoError(t, saveRecordedResponse(t.Context(),
		replayPath,
		llm.CompleteParams{Model: "gpt-test", Messages: []llm.Message{{Role: llm.RoleUser, Content: "Where are OAuth retry notes?"}}},
		nil,
		&llm.Response{Content: "recorded answer", Model: "gpt-test"},
	))

	enabled := true
	store := session.NewStore(filepath.Join(dir, "sessions"))

	err := runOnceWithOptions(
		context.Background(),
		llm.NewRegistry(),
		agent.NewRegistry(nil),
		nil,
		store,
		session.New("gpt-test", nil),
		contextref.Options{Root: dir},
		"",
		contextref.ReferenceManifest{},
		"",
		nil,
		"gpt-test",
		"",
		nil,
		generationSettings{},
		generationSettings{},
		0,
		runOnceExecutionOptions{
			VectorConfig: config.VectorConfig{
				WorkspaceEnabled:      &enabled,
				WorkspaceIndexPath:    filepath.Join(dir, ".atteler", "workspace-index.json"),
				Vectorizer:            vector.VectorizerKindLexical,
				WorkspaceLimit:        1,
				WorkspaceMaxFileBytes: 1024,
				WorkspaceExclude:      []string{"response.json", "recorded-request.json", "sessions/"},
			},
			Response: responseRecordOptions{
				ReplayPath: replayPath,
				RecordPath: recordPath,
			},
		},
		true,
		"Where are OAuth retry notes?",
	)
	require.NoError(t, err)

	data, err := os.ReadFile(recordPath)
	require.NoError(t, err)

	var record responseRecordFile
	require.NoError(t, json.Unmarshal(data, &record))

	var workspaceContext string

	for _, message := range record.Request.Messages {
		if strings.Contains(message.Content, "<workspace_vector_context") {
			workspaceContext = message.Content

			break
		}
	}

	require.NotEmpty(t, workspaceContext)
	assert.Contains(t, workspaceContext, `index_path=".atteler/workspace-index.json"`)
	assert.Contains(t, workspaceContext, `path="docs/auth.md"`)
	assert.Contains(t, workspaceContext, "OAuth callback")
	assert.NotContains(t, workspaceContext, dir)
}

func TestRunOnceWithOptions_LowAutonomySkipsWorkspaceVectorIndex(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	require.NoError(t, os.MkdirAll(filepath.Join(dir, "docs"), 0o750))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "docs", "auth.md"), []byte("OAuth callback state validation and token exchange retry notes."), 0o600))

	indexPath := filepath.Join(dir, ".atteler", "workspace-index.json")

	provider := &runOnceCostProvider{
		name:   "openai",
		models: []string{"gpt-4.1-mini"},
		response: &llm.Response{
			Content:    "recorded answer",
			Provider:   "openai",
			Model:      "gpt-4.1-mini",
			StopReason: llm.StopEndTurn,
		},
	}
	reg := llm.NewRegistry()
	reg.Register(provider)

	enabled := true
	store := session.NewStore(filepath.Join(dir, "sessions"))

	err := runOnceWithOptions(
		context.Background(),
		reg,
		agent.NewRegistry(nil),
		nil,
		store,
		session.New("openai/gpt-4.1-mini", nil),
		contextref.Options{Root: dir},
		"",
		contextref.ReferenceManifest{},
		"",
		nil,
		"openai/gpt-4.1-mini",
		"",
		nil,
		generationSettings{},
		generationSettings{},
		0,
		runOnceExecutionOptions{
			Autonomy: autonomy.Low,
			VectorConfig: config.VectorConfig{
				WorkspaceEnabled:      &enabled,
				WorkspaceIndexPath:    indexPath,
				Vectorizer:            vector.VectorizerKindLexical,
				WorkspaceLimit:        1,
				WorkspaceMaxFileBytes: 1024,
			},
		},
		true,
		"Where are OAuth retry notes?",
	)
	require.NoError(t, err)
	assert.NoFileExists(t, indexPath)
	require.NotNil(t, provider.params)

	for _, message := range provider.params.Messages {
		assert.NotContains(t, message.Content, "<workspace_vector_context")
	}
}

func TestRunOnceWithOptions_LowAutonomyBlocksResponseRecording(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	recordPath := filepath.Join(dir, "recorded-request.json")

	err := runOnceWithOptions(
		context.Background(),
		nil,
		nil,
		nil,
		session.NewStore(filepath.Join(dir, "sessions")),
		session.New("gpt-test", nil),
		contextref.Options{Root: dir},
		"",
		contextref.ReferenceManifest{},
		"",
		nil,
		"gpt-test",
		"",
		nil,
		generationSettings{},
		generationSettings{},
		0,
		runOnceExecutionOptions{
			Autonomy: autonomy.Low,
			Response: responseRecordOptions{RecordPath: recordPath},
		},
		true,
		"explain the change",
	)

	require.Error(t, err)
	assert.Contains(t, err.Error(), "autonomy low blocks file writes")
	assert.Contains(t, err.Error(), "--record-response")
	assert.NoFileExists(t, recordPath)
}

func TestRunOnceWithOptions_DoesNotIndexWorkspaceVectorContextWhenDisabled(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	require.NoError(t, os.MkdirAll(filepath.Join(dir, "docs"), 0o750))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "docs", "auth.md"), []byte("OAuth callback state validation and token exchange retry notes."), 0o600))

	replayPath := filepath.Join(dir, "response.json")
	recordPath := filepath.Join(dir, "recorded-request.json")

	require.NoError(t, saveRecordedResponse(t.Context(),
		replayPath,
		llm.CompleteParams{Model: "gpt-test", Messages: []llm.Message{{Role: llm.RoleUser, Content: "Where are OAuth retry notes?"}}},
		nil,
		&llm.Response{Content: "recorded answer", Model: "gpt-test"},
	))

	store := session.NewStore(filepath.Join(dir, "sessions"))

	err := runOnceWithOptions(
		context.Background(),
		llm.NewRegistry(),
		agent.NewRegistry(nil),
		nil,
		store,
		session.New("gpt-test", nil),
		contextref.Options{Root: dir},
		"",
		contextref.ReferenceManifest{},
		"",
		nil,
		"gpt-test",
		"",
		nil,
		generationSettings{},
		generationSettings{},
		0,
		runOnceExecutionOptions{
			Response: responseRecordOptions{
				ReplayPath: replayPath,
				RecordPath: recordPath,
			},
		},
		true,
		"Where are OAuth retry notes?",
	)
	require.NoError(t, err)

	assert.NoFileExists(t, filepath.Join(dir, vector.DefaultWorkspaceIndexPath))

	data, err := os.ReadFile(recordPath)
	require.NoError(t, err)

	var record responseRecordFile
	require.NoError(t, json.Unmarshal(data, &record))

	for _, message := range record.Request.Messages {
		assert.NotContains(t, message.Content, "<workspace_vector_context")
	}
}

func TestRunOnceWithOptions_HeadlessManifestIncludesWorkspaceVectorContext(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	require.NoError(t, os.MkdirAll(filepath.Join(dir, "docs"), 0o750))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "docs", "auth.md"), []byte("OAuth callback state validation and token exchange retry notes."), 0o600))

	replayPath := filepath.Join(dir, "response.json")
	require.NoError(t, saveRecordedResponse(t.Context(),
		replayPath,
		llm.CompleteParams{Model: "gpt-test", Messages: []llm.Message{{Role: llm.RoleUser, Content: "Where are OAuth retry notes?"}}},
		nil,
		&llm.Response{Content: "recorded answer", Model: "gpt-test"},
	))

	enabled := true
	headlessID := "test-headless-workspace-vector-manifest"
	store := session.NewStore(filepath.Join(dir, "sessions"))

	err := runOnceWithOptions(
		context.Background(),
		llm.NewRegistry(),
		agent.NewRegistry(nil),
		nil,
		store,
		session.New("gpt-test", nil),
		contextref.Options{Root: dir},
		"",
		contextref.ReferenceManifest{},
		"",
		nil,
		"gpt-test",
		"",
		nil,
		generationSettings{},
		generationSettings{},
		0,
		runOnceExecutionOptions{
			OutputFormat: outputFormatText,
			Headless:     true,
			HeadlessID:   headlessID,
			VectorConfig: config.VectorConfig{
				WorkspaceEnabled:      &enabled,
				WorkspaceIndexPath:    filepath.Join(dir, ".atteler", "workspace-index.json"),
				Vectorizer:            vector.VectorizerKindLexical,
				WorkspaceLimit:        1,
				WorkspaceMaxFileBytes: 1024,
				WorkspaceExclude:      []string{"response.json", "sessions/"},
			},
			Response: responseRecordOptions{ReplayPath: replayPath},
		},
		true,
		"Where are OAuth retry notes?",
	)
	require.NoError(t, err)

	log, err := store.ReadHeadlessLog(headlessID)
	require.NoError(t, err)

	manifest := decodeHeadlessContextManifest(t, log)
	require.Len(t, manifest.ConfiguredReferences.Entries, 1)

	entry := manifest.ConfiguredReferences.Entries[0]
	assert.Equal(t, workspaceVectorReferenceScope, entry.Scope)
	assert.Equal(t, "workspace-vector", entry.Source)
	assert.Equal(t, "vector", entry.Kind)
	assert.Equal(t, ".atteler/workspace-index.json", entry.ResolvedSource)
	assert.Equal(t, contextref.ReferenceDecisionLoaded, entry.PolicyDecision)
	assert.NotEmpty(t, entry.DigestSHA256)
	assert.Positive(t, entry.TokenEstimate.UpperBoundTokens)
	assert.Equal(t, entry.Bytes, manifest.ReferenceBytes)
	assert.NotContains(t, entry.ResolvedSource, dir)
}

func TestRunOnce_EmitsContextManifestBeforeBudgetFailure(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	store := session.NewStore(filepath.Join(dir, "sessions"))

	var eventLog bytes.Buffer

	hooks := events.NewRunnerWithLogger(nil, &eventLog)

	err := runOnce(
		context.Background(),
		llm.NewRegistry(),
		agent.NewRegistry(nil),
		hooks,
		store,
		session.New("gpt-test", nil),
		contextref.Options{Root: dir},
		"",
		contextref.ReferenceManifest{},
		"",
		nil,
		"gpt-test",
		"",
		nil,
		generationSettings{},
		generationSettings{},
		1,
		responseRecordOptions{},
		true,
		strings.Repeat("x", 200),
	)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "max_input_tokens")

	log := eventLog.String()
	assert.Contains(t, log, "context_manifest")
	assert.Contains(t, log, "fits_configured_token_budget=false")
	assert.Contains(t, log, "estimated_token_upper_bound")
	assert.Contains(t, log, "max_input_tokens=1")
}

func TestRunOnce_ContextInputBudgetPreemptsAgentLoopInputBudget(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	store := session.NewStore(filepath.Join(dir, "sessions"))
	provider := &runOnceCostProvider{
		name:   "openai",
		models: []string{"gpt-4.1-mini"},
	}
	reg := llm.NewRegistry()
	reg.Register(provider)

	var eventLog bytes.Buffer

	err := runOnceWithOptions(
		context.Background(),
		reg,
		agent.NewRegistry(nil),
		events.NewRunnerWithLogger(nil, &eventLog),
		store,
		session.New("openai/gpt-4.1-mini", nil),
		contextref.Options{Root: dir},
		"",
		contextref.ReferenceManifest{},
		"",
		nil,
		"openai/gpt-4.1-mini",
		"",
		nil,
		generationSettings{},
		generationSettings{},
		1,
		runOnceExecutionOptions{
			OutputFormat:    outputFormatText,
			AgentLoopBudget: llm.AgentLoopBudget{MaxInputTokens: 1_000},
		},
		true,
		strings.Repeat("x", 200),
	)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "max_input_tokens")
	assert.NotContains(t, err.Error(), "agent_loop.max_input_tokens")
	assert.Zero(t, provider.calls, "per-request context.max_input_tokens should reject before the agent loop calls the model")

	log := eventLog.String()
	assert.Contains(t, log, "context_manifest")
	assert.Contains(t, log, "agent_loop_budget=")
	assert.Contains(t, log, `"max_input_tokens":1000`)
}

func TestRunOnce_EmitsContextManifestForRejectedInlineReference(t *testing.T) {
	t.Parallel()

	parent := t.TempDir()
	dir := filepath.Join(parent, "repo")
	require.NoError(t, os.MkdirAll(dir, 0o750))
	require.NoError(t, os.WriteFile(filepath.Join(parent, "outside.txt"), []byte("secret"), 0o600))

	store := session.NewStore(filepath.Join(dir, "sessions"))

	var eventLog bytes.Buffer

	hooks := events.NewRunnerWithLogger(nil, &eventLog)
	referenceManifest := contextref.BuildReferenceManifest([]contextref.ReferenceEvent{
		{
			Source:         "style.md",
			Kind:           "file",
			Scope:          contextref.ReferenceScopeGlobal,
			Location:       "local",
			Bytes:          12,
			PolicyDecision: contextref.ReferenceDecisionLoaded,
			PolicyReason:   "allowed by policy",
			TokenEstimate:  contextpack.TokenEstimate{Tokens: 3, ErrorBoundTokens: 1, UpperBoundTokens: 4},
			TokenEstimator: "test-estimator",
		},
	})

	err := runOnce(
		context.Background(),
		llm.NewRegistry(),
		agent.NewRegistry(nil),
		hooks,
		store,
		session.New("gpt-test", nil),
		contextref.Options{Root: dir},
		"",
		referenceManifest,
		"",
		nil,
		"gpt-test",
		"",
		nil,
		generationSettings{},
		generationSettings{},
		0,
		responseRecordOptions{},
		true,
		"read @../outside.txt",
	)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "expand context references")

	log := eventLog.String()
	assert.Contains(t, log, "event:context_manifest")
	assert.Contains(t, log, "inline_reference_count=1")
	assert.Contains(t, log, "included_reference_count=0")
	assert.Contains(t, log, "omitted_reference_count=1")
	assert.Contains(t, log, "rejected_reference_count=1")
	assert.Contains(t, log, "context_manifest=")
	assert.Contains(t, log, "escapes root")
	assert.Contains(t, log, "rejected.root_escape")
	assert.Contains(t, log, "omitted.request_aborted")
}

func TestRunOnceWithOptions_HeadlessRejectedInlineReferenceWritesManifest(t *testing.T) {
	t.Parallel()

	parent := t.TempDir()
	dir := filepath.Join(parent, "repo")
	require.NoError(t, os.MkdirAll(dir, 0o750))
	require.NoError(t, os.WriteFile(filepath.Join(parent, "outside.txt"), []byte("secret"), 0o600))

	store := session.NewStore(filepath.Join(dir, "sessions"))
	headlessID := "test-headless-rejected-inline"

	err := runOnceWithOptions(
		context.Background(),
		llm.NewRegistry(),
		agent.NewRegistry(nil),
		nil,
		store,
		session.New("gpt-test", nil),
		contextref.Options{Root: dir},
		"",
		contextref.ReferenceManifest{},
		"",
		nil,
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
	require.Error(t, err)

	run, err := store.LoadHeadlessRun(headlessID)
	require.NoError(t, err)
	assert.Equal(t, session.HeadlessStatusFailed, run.Status)
	assert.Contains(t, run.Error, "expand context references")
	require.NotNil(t, run.ExitCode)
	assert.Equal(t, 1, *run.ExitCode)

	log, err := store.ReadHeadlessLog(headlessID)
	require.NoError(t, err)
	assert.Contains(t, log, "started")
	assert.Contains(t, log, "context_manifest")

	manifest := decodeHeadlessContextManifest(t, log)
	assert.Equal(t, 1, manifest.InlineReferenceCount)
	assert.Equal(t, 1, manifest.RejectedReferenceCount)
	require.Len(t, manifest.InlineReferences, 1)
	assert.Equal(t, contextref.ReferenceDecisionRejected, manifest.InlineReferences[0].PolicyDecision)
	assert.Contains(t, manifest.InlineReferences[0].PolicyReason, "escapes root")
	assert.Equal(t, "rejected.root_escape", manifest.InlineReferences[0].PolicyReasonCode)
	assert.NotContains(t, manifest.InlineReferences[0].PolicyReason, dir)
}

func TestRunOnceAgentLoopPreflightAppendsFollowupHeadlessManifest(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	store := session.NewStore(filepath.Join(dir, "sessions"))
	headlessRun := session.HeadlessRun{
		ID:        "run-manifest",
		SessionID: "session-manifest",
		Status:    session.HeadlessStatusRunning,
	}
	require.NoError(t, store.SaveHeadlessRun(headlessRun))

	preflight := runOnceAgentLoopManifestPreflight(
		context.Background(),
		nil,
		llm.NewRegistry(),
		store,
		&headlessRun,
		filepath.Join(dir, "sessions", "session-manifest.json"),
		headlessRun.SessionID,
		"agent-a",
		nil,
		nil,
		contextref.ReferenceManifest{},
		10_000,
	)

	err := preflight(1, llm.CompleteParams{
		Model:    "gpt-test",
		Messages: []llm.Message{{Role: llm.RoleUser, Content: "follow-up after tool output"}},
	})
	require.NoError(t, err)

	log, err := store.ReadHeadlessLog(headlessRun.ID)
	require.NoError(t, err)
	assert.Contains(t, log, "context_manifest")
	assert.Contains(t, log, `"schema_version":1`)
	assert.Contains(t, log, `"message_count":1`)
}

func TestWriteRunOnceResult_JSONAndHeadlessText(t *testing.T) {
	t.Parallel()

	result := runOnceResult{
		SessionID:               "session-id",
		SessionPath:             "/tmp/session.json",
		SessionPersisted:        true,
		AgentLoopCheckpointPath: "/tmp/session.agentloop.jsonl",
		AgentLoopBudget:         llm.AgentLoopBudget{MaxInputTokens: 100, MaxOutputTokens: 50},
		Autonomy:                "medium",
		HeadlessID:              "headless-id",
		Model:                   "gpt-test",
		ModelMode:               "fast",
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
	assert.Equal(t, result.ModelMode, decoded.ModelMode)
	assert.True(t, decoded.SessionPersisted)
	assert.Equal(t, result.AgentLoopCheckpointPath, decoded.AgentLoopCheckpointPath)
	assert.Equal(t, result.AgentLoopBudget, decoded.AgentLoopBudget)
	assert.Equal(t, result.TokenUsage.OutputTokens, decoded.TokenUsage.OutputTokens)
	assert.Empty(t, stderr.String())

	stdout.Reset()
	stderr.Reset()
	require.NoError(t, writeRunOnceResult(&stdout, &stderr, result, "text", true))
	assert.Empty(t, stdout.String())
	assert.Empty(t, stderr.String())

	require.NoError(t, writeRunOnceResult(&stdout, &stderr, result, "text", false))
	assert.Contains(t, stderr.String(), "session: session-id (/tmp/session.json)")
	assert.Contains(t, stderr.String(), "agent loop checkpoint: /tmp/session.agentloop.jsonl")
	assert.Contains(t, stderr.String(), "agent loop budget:")
	assert.Contains(t, stderr.String(), "in=100")
	assert.Contains(t, stderr.String(), "out=50")
	assert.Contains(t, stderr.String(), "autonomy: medium")
}

func TestWriteRunOnceResult_LowAutonomyMarksSessionUnpersisted(t *testing.T) {
	t.Parallel()

	result := runOnceResult{
		SessionID:        "session-id",
		SessionPath:      "/tmp/session.json",
		SessionPersisted: false,
		Autonomy:         "low",
		Content:          "plan only",
	}

	var (
		stdout bytes.Buffer
		stderr bytes.Buffer
	)

	require.NoError(t, writeRunOnceResult(&stdout, &stderr, result, "text", false))
	assert.Equal(t, "plan only\n", stdout.String())
	assert.Contains(t, stderr.String(), "session: session-id (not persisted: autonomy low)")
	assert.NotContains(t, stderr.String(), "session: session-id (/tmp/session.json)")
	assert.Contains(t, stderr.String(), "autonomy: low")
}

func TestFormatSessionLocationExplainsLowAutonomyNonPersistence(t *testing.T) {
	t.Parallel()

	assert.Equal(t, "session-id (/tmp/session.json)", formatSessionLocation("session-id", "/tmp/session.json", true, "low"))
	assert.Equal(t, "session-id (not persisted: autonomy low)", formatSessionLocation("session-id", "/tmp/session.json", false, "low"))
	assert.Equal(t, "session-id (not persisted)", formatSessionLocation("session-id", "/tmp/session.json", false, "medium"))
}

func TestBashExecutorStreamsOutputBeforeCompletion(t *testing.T) {
	t.Parallel()

	logw := newNotifyBuffer()
	done := make(chan llm.ToolResult, 1)

	go func() {
		done <- newBashExecutor(t.TempDir(), logw, attshell.AuditContext{}, nil)(context.Background(), llm.ToolCall{
			ID:    "call-1",
			Name:  "bash",
			Input: map[string]any{"command": `printf '\154ive\n'; sleep 0.4; printf '\144one\n'`},
		})
	}()

	requireLogWriteContainsBefore(t, logw.writes, "live\n", liveOutputTimeout)

	select {
	case result := <-done:
		require.Failf(t, "tool completed before delayed output", "result=%+v", result)
	default:
	}

	select {
	case result := <-done:
		require.False(t, result.IsError)

		plainLog := stripANSI(logw.String())
		assert.Contains(t, plainLog, "live\n")
		assert.Contains(t, plainLog, "done\n")
	case <-time.After(time.Second):
		require.FailNow(t, "timed out waiting for tool completion")
	}
}

func TestBashExecutorDefaultsAuditAutonomy(t *testing.T) {
	t.Parallel()

	auditDir := filepath.Join(t.TempDir(), "audit")
	result := newBashExecutor(t.TempDir(), nil, attshell.AuditContext{
		AuditDir: auditDir,
	}, nil)(context.Background(), llm.ToolCall{
		ID:    "call-1",
		Name:  "bash",
		Input: map[string]any{"command": "printf ok"},
	})
	require.False(t, result.IsError)
	assert.Contains(t, result.Content, "ok")

	records := readCommandAuditRecords(t, auditDir)
	require.NotEmpty(t, records)

	for _, record := range records {
		assert.Equal(t, autonomy.DefaultLevel.String(), record.Autonomy)
	}
}

func TestBashExecutorAppliesPermissionPolicyToFileTools(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	readOnly := permission.ReadOnlyPolicy()

	result := newBashExecutor(root, nil, attshell.AuditContext{}, &readOnly)(context.Background(), llm.ToolCall{
		ID:   "write-1",
		Name: llm.ToolNameWrite,
		Input: map[string]any{
			"path":    "blocked.txt",
			"content": "nope",
		},
	})

	require.True(t, result.IsError)
	assert.Contains(t, result.Content, "denied by permission policy")
	assert.NoFileExists(t, filepath.Join(root, "blocked.txt"))
}

func TestBashExecutorStreamsStderrBeforeCompletion(t *testing.T) {
	t.Parallel()

	logw := newNotifyBuffer()
	done := make(chan llm.ToolResult, 1)

	go func() {
		done <- newBashExecutor(t.TempDir(), logw, attshell.AuditContext{}, nil)(context.Background(), llm.ToolCall{
			ID:    "call-1",
			Name:  "bash",
			Input: map[string]any{"command": `printf '\167arn\n' >&2; sleep 0.4; printf '\144one\n' >&2`},
		})
	}()

	requireLogWriteContainsBefore(t, logw.writes, "warn\n", liveOutputTimeout)

	select {
	case result := <-done:
		require.Failf(t, "tool completed before delayed stderr", "result=%+v", result)
	default:
	}

	select {
	case result := <-done:
		require.False(t, result.IsError)

		plainLog := stripANSI(logw.String())
		assert.Contains(t, plainLog, "warn\n")
		assert.Contains(t, plainLog, "done\n")
	case <-time.After(time.Second):
		require.FailNow(t, "timed out waiting for tool completion")
	}
}

type notifyBuffer struct {
	writes chan string
	buffer bytes.Buffer
	mu     sync.Mutex
}

func newNotifyBuffer() *notifyBuffer {
	return &notifyBuffer{writes: make(chan string, 16)}
}

func (b *notifyBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()

	n, err := b.buffer.Write(p)
	select {
	case b.writes <- string(p):
	default:
	}

	return n, err
}

func (b *notifyBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()

	return b.buffer.String()
}

func requireLogWriteContainsBefore(t *testing.T, writes <-chan string, want string, timeout time.Duration) {
	t.Helper()

	deadline := time.After(timeout)

	for {
		select {
		case text := <-writes:
			if strings.Contains(stripANSI(text), want) {
				return
			}
		case <-deadline:
			require.FailNowf(t, "timed out waiting for streamed log output", "want=%q", want)
		}
	}
}

func TestRunOnceWithOptions_HeadlessReplayCreatesMetadata(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	replayPath := filepath.Join(dir, "response.json")
	require.NoError(t, saveRecordedResponse(t.Context(),
		replayPath,
		llm.CompleteParams{Model: "openai/gpt-4.1-mini", Messages: []llm.Message{{Role: llm.RoleUser, Content: "hello"}}},
		nil,
		&llm.Response{Content: "recorded answer", Provider: "openai", Model: "gpt-4.1-mini", InputTokens: 2, CachedInputTokens: 1, OutputTokens: 3},
	))

	store := session.NewStore(filepath.Join(dir, "sessions"))
	headlessID := "test-headless"

	var eventLog bytes.Buffer

	agentLoopBudget := llm.AgentLoopBudget{
		MaxWallTime:     time.Minute,
		MaxOutputBytes:  4096,
		MaxCostMicros:   25_000,
		MaxIterations:   3,
		MaxModelCalls:   4,
		MaxToolCalls:    5,
		MaxInputTokens:  100,
		MaxOutputTokens: 50,
		MaxTotalTokens:  150,
	}

	err := runOnceWithOptions(
		context.Background(),
		llm.NewRegistry(),
		agent.NewRegistry(nil),
		events.NewRunnerWithLogger(nil, &eventLog),
		store,
		session.New("openai/gpt-4.1-mini", nil),
		contextref.Options{Root: dir},
		"",
		contextref.ReferenceManifest{},
		"",
		nil,
		"openai/gpt-4.1-mini",
		"",
		nil,
		generationSettings{},
		generationSettings{},
		0,
		runOnceExecutionOptions{
			OutputFormat:    outputFormatText,
			HeadlessID:      headlessID,
			Response:        responseRecordOptions{ReplayPath: replayPath},
			AgentLoopBudget: agentLoopBudget,
			Headless:        true,
		},
		true,
		"hello",
	)
	require.NoError(t, err)

	run, err := store.LoadHeadlessRun(headlessID)
	require.NoError(t, err)

	hookLog := eventLog.String()
	assert.Contains(t, hookLog, "event:session_start")
	assert.Contains(t, hookLog, "event:session_end")
	assert.Contains(t, hookLog, "agent_loop_budget=")
	assert.Contains(t, hookLog, `"max_cost_micros":25000`)
	assert.Contains(t, hookLog, `"max_input_tokens":100`)

	assert.Equal(t, session.HeadlessStatusCompleted, run.Status)
	assert.Equal(t, "gpt-4.1-mini", run.Model)
	assert.Equal(t, agentLoopBudget, run.AgentLoopBudget)
	assert.Equal(t, "headless", run.StartMethod)
	assert.NotNil(t, run.CompletedAt)

	log, err := store.ReadHeadlessLog(headlessID)
	require.NoError(t, err)
	assert.Contains(t, log, "started")
	assert.Contains(t, log, "context_manifest")
	assert.Contains(t, log, `"schema_version":1`)
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
	assert.Equal(t, "openai/gpt-4.1-mini", headlessEvents[0].Model)
	assert.Equal(t, agentLoopBudget, headlessEvents[0].AgentLoopBudget)
	assert.Equal(t, "openai/gpt-4.1-mini", headlessEvents[1].Model)
	assert.Equal(t, agentLoopBudget, headlessEvents[1].AgentLoopBudget)
	assert.Equal(t, "gpt-4.1-mini", headlessEvents[2].Model)
	assert.Equal(t, agentLoopBudget, headlessEvents[2].AgentLoopBudget)
	assert.Equal(t, "gpt-4.1-mini", headlessEvents[3].Model)
	assert.Equal(t, agentLoopBudget, headlessEvents[3].AgentLoopBudget)
	assert.Equal(t, "headless", headlessEvents[0].StartMethod)
	assert.Equal(t, run.Executable, headlessEvents[0].Executable)
	assert.Equal(t, run.Version, headlessEvents[0].Version)
	assert.NotEmpty(t, headlessEvents[0].StartedCommand)
	assert.NotEmpty(t, headlessEvents[0].CommandArgs)
	assert.Equal(t, string(llm.RoleUser), headlessEvents[1].Role)
	assert.Equal(t, "hello", headlessEvents[1].Message)
	assert.Equal(t, "5", headlessEvents[1].Metadata["bytes"])
	assert.Equal(t, string(llm.RoleAssistant), headlessEvents[2].Role)
	assert.Equal(t, "15", headlessEvents[2].Metadata["bytes"])
	assert.Equal(t, "headless", headlessEvents[3].StartMethod)
	assert.Equal(t, run.Executable, headlessEvents[3].Executable)
	assert.Equal(t, run.Version, headlessEvents[3].Version)
	assert.NotEmpty(t, headlessEvents[3].StartedCommand)
	assert.NotEmpty(t, headlessEvents[3].CommandArgs)
	assert.Equal(t, "completed", headlessEvents[3].TerminalReason)

	runs, err := store.ListHeadlessRuns()
	require.NoError(t, err)
	require.Len(t, runs, 1)
	assert.Equal(t, headlessID, runs[0].ID)
	require.NoError(t, streamHeadlessLog(context.Background(), store, headlessID))
}

func TestRunOnceWithOptions_HeadlessPreflightRecordsModelMode(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	store := session.NewStore(filepath.Join(dir, "sessions"))
	headlessID := "preflight-model-mode"

	err := runOnceWithOptions(
		context.Background(),
		llm.NewRegistry(),
		agent.NewRegistry(nil),
		events.NewRunnerWithLogger(nil, nil),
		store,
		session.New("openai/gpt-5.5", nil),
		contextref.Options{Root: dir},
		"",
		contextref.ReferenceManifest{},
		"",
		nil,
		"openai/gpt-5.5",
		"",
		nil,
		generationSettings{ModelMode: llm.ModelModeFast},
		generationSettings{},
		0,
		runOnceExecutionOptions{
			OutputFormat: "yaml",
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
	assert.Equal(t, llm.ModelModeFast, run.ModelMode)
}

func TestStartHeadlessRunRejectsWhitespacePaddedExplicitID(t *testing.T) {
	t.Parallel()

	store := session.NewStore(t.TempDir())
	run, err := startHeadlessRun(
		t.Context(),
		store,
		runOnceExecutionOptions{Headless: true, HeadlessID: " headless-id "},
		session.New("gpt-test", nil),
		"hello",
		"gpt-test",
		"",
		"default",
	)

	require.ErrorContains(t, err, "headless id must not have leading or trailing whitespace")
	assert.Nil(t, run)
}

func TestStartHeadlessRunRejectsWhitespacePaddedParentID(t *testing.T) {
	store := session.NewStore(t.TempDir())
	t.Setenv(headlessParentRunIDEnv, " parent-headless ")

	run, err := startHeadlessRun(
		t.Context(),
		store,
		runOnceExecutionOptions{Headless: true, HeadlessID: "child-headless"},
		session.New("gpt-test", nil),
		"hello",
		"gpt-test",
		"",
		"default",
	)

	require.ErrorContains(t, err, "invalid parent headless id")
	assert.Nil(t, run)

	_, err = store.LoadHeadlessRun("child-headless")
	require.ErrorIs(t, err, os.ErrNotExist)
}

func TestStartHeadlessRunRejectsWhitespacePaddedRetryOfID(t *testing.T) {
	store := session.NewStore(t.TempDir())
	t.Setenv(headlessRetryOfRunIDEnv, " failed-headless ")

	run, err := startHeadlessRun(
		t.Context(),
		store,
		runOnceExecutionOptions{Headless: true, HeadlessID: "retry-child-headless"},
		session.New("gpt-test", nil),
		"hello",
		"gpt-test",
		"",
		"default",
	)

	require.ErrorContains(t, err, "invalid retry_of headless id")
	assert.Nil(t, run)

	_, err = store.LoadHeadlessRun("retry-child-headless")
	require.ErrorIs(t, err, os.ErrNotExist)
}

func TestStartHeadlessRunRejectsSelfRelationshipIDsFromEnv(t *testing.T) {
	tests := []struct {
		env     string
		wantErr string
		name    string
	}{
		{
			name:    "parent",
			env:     headlessParentRunIDEnv,
			wantErr: "cannot be its own parent",
		},
		{
			name:    "retry_of",
			env:     headlessRetryOfRunIDEnv,
			wantErr: "cannot retry itself",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			store := session.NewStore(t.TempDir())
			headlessID := "self-" + tt.name + "-headless"
			t.Setenv(tt.env, headlessID)

			run, err := startHeadlessRun(
				t.Context(),
				store,
				runOnceExecutionOptions{Headless: true, HeadlessID: headlessID},
				session.New("gpt-test", nil),
				"hello",
				"gpt-test",
				"",
				"default",
			)

			require.ErrorContains(t, err, tt.wantErr)
			assert.Nil(t, run)

			_, err = store.LoadHeadlessRun(headlessID)
			require.ErrorIs(t, err, os.ErrNotExist)
		})
	}
}

func TestStartHeadlessRunRecordsParentRunRelationship(t *testing.T) {
	store := session.NewStore(t.TempDir())
	require.NoError(t, store.SaveHeadlessRun(session.HeadlessRun{
		ID:     "parent-headless",
		Status: session.HeadlessStatusRunning,
	}))

	t.Setenv(headlessParentRunIDEnv, "parent-headless")

	child, err := startHeadlessRun(
		t.Context(),
		store,
		runOnceExecutionOptions{
			HeadlessID: "child-headless",
			Headless:   true,
		},
		session.New("gpt-test", nil),
		"hello",
		"gpt-test",
		"",
		"",
	)
	require.NoError(t, err)
	require.NotNil(t, child)
	assert.Equal(t, "parent-headless", child.ParentRunID)
	assert.Empty(t, child.RetryOfRunID)

	parent, err := store.LoadHeadlessRun("parent-headless")
	require.NoError(t, err)
	assert.Contains(t, parent.ChildRunIDs, "child-headless")

	headlessEvents, err := store.ReadHeadlessEvents("child-headless")
	require.NoError(t, err)
	require.Len(t, headlessEvents, 2)
	assert.Equal(t, "parent-headless", headlessEvents[0].ParentRunID)
	assert.Equal(t, session.HeadlessEventUserMessage, headlessEvents[1].Type)
	assert.Equal(t, "parent-headless", headlessEvents[1].ParentRunID)
	assert.Empty(t, headlessEvents[1].RetryOfRunID)
}

func TestStartHeadlessRunRecordsRetryRelationshipFromRetryEnv(t *testing.T) {
	store := session.NewStore(t.TempDir())
	t.Setenv(headlessParentRunIDEnv, "parent-headless")
	t.Setenv(headlessRetryOfRunIDEnv, "failed-headless")
	t.Setenv(headlessRetryCountEnv, "2")

	child, err := startHeadlessRun(
		t.Context(),
		store,
		runOnceExecutionOptions{
			HeadlessID: "retry-child-headless",
			Headless:   true,
		},
		session.New("gpt-test", nil),
		"hello",
		"gpt-test",
		"",
		"",
	)
	require.NoError(t, err)
	require.NotNil(t, child)
	assert.Equal(t, "parent-headless", child.ParentRunID)
	assert.Equal(t, "failed-headless", child.RetryOfRunID)
	assert.Equal(t, 2, child.RetryCount)

	headlessEvents, err := store.ReadHeadlessEvents("retry-child-headless")
	require.NoError(t, err)
	require.Len(t, headlessEvents, 2)
	assert.Equal(t, "failed-headless", headlessEvents[0].RetryOfRunID)
	assert.Equal(t, 2, headlessEvents[0].RetryCount)
	assert.Equal(t, "failed-headless", headlessEvents[1].RetryOfRunID)
	assert.Equal(t, 2, headlessEvents[1].RetryCount)
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
		t.Context(),
		store,
		runOnceExecutionOptions{
			HeadlessID: "duplicate-headless",
			Headless:   true,
		},
		session.New("gpt-test", nil),
		"hello",
		"gpt-test",
		"",
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

func TestRunOnceWithOptions_HeadlessBudgetFailureLogsManifest(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	store := session.NewStore(filepath.Join(dir, "sessions"))
	headlessID := "test-headless-budget"

	err := runOnceWithOptions(
		context.Background(),
		llm.NewRegistry(),
		agent.NewRegistry(nil),
		nil,
		store,
		session.New("gpt-test", nil),
		contextref.Options{Root: dir},
		"",
		contextref.ReferenceManifest{},
		"",
		nil,
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
		strings.Repeat("x", 200),
	)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "max_input_tokens")

	run, err := store.LoadHeadlessRun(headlessID)
	require.NoError(t, err)
	assert.Equal(t, session.HeadlessStatusFailed, run.Status)
	assert.Contains(t, run.Error, "max_input_tokens")

	log, err := store.ReadHeadlessLog(headlessID)
	require.NoError(t, err)
	assert.Contains(t, log, "started")
	assert.Contains(t, log, "context_manifest")
	assert.Contains(t, log, `"schema_version":1`)
	assert.Contains(t, log, "failed")
}

func TestRunOnceWithOptions_HeadlessManifestIncludesInlineReferenceAudit(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	require.NoError(t, os.MkdirAll(filepath.Join(dir, "docs"), 0o750))

	referenceContent := "guide content\n"
	require.NoError(t, os.WriteFile(filepath.Join(dir, "docs", "guide.md"), []byte(referenceContent), 0o600))

	replayPath := filepath.Join(dir, "response.json")
	require.NoError(t, saveRecordedResponse(t.Context(),
		replayPath,
		llm.CompleteParams{Model: "fallback/tiny", Messages: []llm.Message{{Role: llm.RoleUser, Content: "summarize @docs/guide.md"}}},
		nil,
		&llm.Response{Content: "recorded answer", Model: "fallback/tiny"},
	))

	registry := llm.NewRegistry()
	registry.Register(contextManifestBudgetProvider{name: "fallback", models: []string{"tiny"}, window: 10_000})

	store := session.NewStore(filepath.Join(dir, "sessions"))
	headlessID := "test-headless-inline-manifest"
	sessionState := session.New("", nil)

	var eventLog bytes.Buffer

	err := runOnceWithOptions(
		context.Background(),
		registry,
		agent.NewRegistry(nil),
		events.NewRunnerWithLogger(nil, &eventLog),
		store,
		sessionState,
		contextref.Options{Root: dir},
		"",
		contextref.ReferenceManifest{},
		"",
		nil,
		"",
		"",
		[]string{"fallback/tiny"},
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
		"summarize @docs/guide.md",
	)
	require.NoError(t, err)

	assert.Contains(t, eventLog.String(), "event:context_manifest model=fallback/tiny")

	log, err := store.ReadHeadlessLog(headlessID)
	require.NoError(t, err)

	manifest := decodeHeadlessContextManifest(t, log)
	assert.Equal(t, "fallback/tiny", manifest.Model)
	assert.Equal(t, 1, manifest.InlineReferenceCount)
	assert.Equal(t, 1, manifest.IncludedReferenceCount)
	assert.Equal(t, len(referenceContent), manifest.ReferenceBytes)
	assert.Positive(t, manifest.ReferenceEstimatedUpperBound)
	require.Len(t, manifest.InlineReferences, 1)
	assert.Equal(t, "docs/guide.md", manifest.InlineReferences[0].Source)
	assert.Equal(t, "file", manifest.InlineReferences[0].Kind)
	assert.Equal(t, contextref.ReferenceScopeInline, manifest.InlineReferences[0].Scope)
	assert.Equal(t, "local", manifest.InlineReferences[0].Location)
	assert.Equal(t, contextref.ReferenceDecisionLoaded, manifest.InlineReferences[0].PolicyDecision)
	assert.Equal(t, "loaded.allowed", manifest.InlineReferences[0].PolicyReasonCode)
	assert.Equal(t, len(referenceContent), manifest.InlineReferences[0].Bytes)
	assert.NotEmpty(t, manifest.InlineReferences[0].DigestSHA256)
	assert.Positive(t, manifest.InlineReferences[0].TokenEstimate.UpperBoundTokens)
	assert.Contains(t, manifest.InlineReferences[0].TokenEstimator, "provider=fallback")
	assert.Contains(t, manifest.InlineReferences[0].TokenEstimator, "model=tiny")

	saved, err := store.Load(sessionState.ID)
	require.NoError(t, err)
	require.Len(t, saved.ProviderCalls, 1)
	require.NotEmpty(t, saved.ProviderCalls[0].ReferencedFiles)
	assert.Equal(t, "docs/guide.md", saved.ProviderCalls[0].ReferencedFiles[0].LogicalPath)
	assert.Contains(t, saved.ProviderCalls[0].ReferencedFiles[0].Path, filepath.Join("docs", "guide.md"))
}

func TestRunOnceWithOptions_HeadlessPrivateLogKeepsPrompt(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	replayPath := filepath.Join(dir, "response.json")
	require.NoError(t, saveRecordedResponse(t.Context(),
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
		contextref.ReferenceManifest{},
		"",
		nil,
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
	require.NoError(t, saveRecordedResponse(t.Context(),
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
		contextref.ReferenceManifest{},
		"",
		nil,
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
		contextref.ReferenceManifest{},
		"",
		nil,
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
		generationDefaults: generationSettings{
			ModelMode: llm.ModelModeFast,
		},
	})
	require.ErrorContains(t, err, "unsupported output format")

	loaded, loadErr := store.LoadHeadlessRun(headlessID)
	require.NoError(t, loadErr)
	assert.Equal(t, session.HeadlessStatusFailed, loaded.Status)
	assert.Equal(t, llm.ModelModeFast, loaded.ModelMode)
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
		generationDefaults: generationSettings{
			ModelMode: llm.ModelModeFast,
		},
	})
	require.ErrorContains(t, err, "headless mode requires")

	loaded, loadErr := store.LoadHeadlessRun(headlessID)
	require.NoError(t, loadErr)
	assert.Equal(t, session.HeadlessStatusFailed, loaded.Status)
	assert.Equal(t, llm.ModelModeFast, loaded.ModelMode)
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
		contextref.ReferenceManifest{},
		"",
		nil,
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
		contextref.ReferenceManifest{},
		"",
		nil,
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

func decodeHeadlessContextManifest(t *testing.T, log string) requestContextManifest {
	t.Helper()

	for line := range strings.SplitSeq(log, "\n") {
		_, manifestJSON, ok := strings.Cut(line, "\tjson=")
		if !ok || !strings.HasPrefix(line, "context_manifest\t") {
			continue
		}

		var manifest requestContextManifest
		require.NoError(t, json.Unmarshal([]byte(manifestJSON), &manifest))

		return manifest
	}

	require.FailNow(t, "headless log did not contain a context manifest", "log:\n%s", log)

	return requestContextManifest{}
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

	finishHeadlessRun(store, &loaded, session.HeadlessStatusFailed, "provider unavailable", map[string]string{
		"fallback_failure_classifications": "alpha=permanent_error",
		"provider_readiness":               "provider readiness: alpha=registered models=static",
	})

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
	assert.Equal(t, "alpha=permanent_error", headlessEvents[0].Metadata["fallback_failure_classifications"])
	assert.Equal(t, "provider readiness: alpha=registered models=static", headlessEvents[0].Metadata["provider_readiness"])
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

func TestRecordHeadlessAssistantMessageIncludesProviderFailureMetadata(t *testing.T) {
	t.Parallel()

	store := session.NewStore(t.TempDir())
	run := session.HeadlessRun{
		ID:     "test-headless-assistant-provider-metadata",
		Model:  "anthropic/claude-sonnet-4-20250514",
		Status: session.HeadlessStatusRunning,
	}
	require.NoError(t, store.SaveHeadlessRun(run))

	recordHeadlessAssistantMessage(store, &run, 42, map[string]string{
		"fallback_failure_classifications": "claude-code=transient_rate_limit",
		"rate_limited_providers":           "claude-code",
		"bytes":                            "should-not-override-response-size",
	})

	headlessEvents, err := store.ReadHeadlessEvents(run.ID)
	require.NoError(t, err)
	require.Len(t, headlessEvents, 1)
	assert.Equal(t, session.HeadlessEventAssistantMessage, headlessEvents[0].Type)
	assert.Equal(t, "42", headlessEvents[0].Metadata["bytes"])
	assert.Equal(t, "claude-code=transient_rate_limit", headlessEvents[0].Metadata["fallback_failure_classifications"])
	assert.Equal(t, "claude-code", headlessEvents[0].Metadata["rate_limited_providers"])
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
		Owner:            "alice",
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
		"owner=alice",
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
