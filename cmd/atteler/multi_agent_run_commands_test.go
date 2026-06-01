package main

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/tommoulard/atteler/pkg/agent"
	appconfig "github.com/tommoulard/atteler/pkg/config"
	"github.com/tommoulard/atteler/pkg/contextref"
	"github.com/tommoulard/atteler/pkg/llm"
	"github.com/tommoulard/atteler/pkg/review"
	"github.com/tommoulard/atteler/pkg/session"
	"github.com/tommoulard/atteler/pkg/speculate"
)

type multiAgentRunLLMProvider struct {
	failures map[string]error
	models   []string
}

func (p multiAgentRunLLMProvider) Name() string {
	return "multi-agent-test"
}

func (p multiAgentRunLLMProvider) Models() []string {
	return append([]string(nil), p.models...)
}

func (p multiAgentRunLLMProvider) FetchModels(context.Context) ([]string, error) {
	return p.Models(), nil
}

func (p multiAgentRunLLMProvider) HealthCheck(context.Context) error {
	return nil
}

func (p multiAgentRunLLMProvider) ModelContextWindow(string) int {
	return 128_000
}

func (p multiAgentRunLLMProvider) Complete(ctx context.Context, params llm.CompleteParams) (*llm.Response, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	if err := p.failures[params.Model]; err != nil {
		return nil, err
	}

	systemPrompt, _ := splitPromptMessages(params.Messages)
	agentName := multiAgentRunPromptAgent(systemPrompt, params.Model)
	content := "proposal from " + agentName

	switch {
	case strings.Contains(systemPrompt, "aggregating speculative execution results"):
		content = "WINNER: planner\nREASON: best recorded evidence\nGATE tests pass: PASS recorded"
	case strings.Contains(systemPrompt, "reviewing a proposal"):
		content = "cross-review from " + agentName
	case strings.Contains(systemPrompt, "cross-reviewing"):
		content = `{"notes":"cross-review note","challenges":[]}`
	case strings.Contains(systemPrompt, "review judge"):
		content = multiAgentRunReviewReportJSON("aggregate-verdict")
	case strings.Contains(systemPrompt, "code review workflow"):
		content = multiAgentRunReviewReportJSON(agentName)
	}

	return &llm.Response{
		Content:      content,
		Model:        params.Model,
		InputTokens:  llm.EstimateTokens(params.Messages),
		OutputTokens: llm.EstimateTokens([]llm.Message{{Role: llm.RoleAssistant, Content: content}}),
	}, nil
}

func multiAgentRunReviewReportJSON(reviewer string) string {
	return `{"reviewer":"` + reviewer + `","findings":[],"gate_checks":[{"name":"source reviewed","passed":true,"notes":"recorded","proof":"func Example() {}","not_run_reason":"","provenance":[{"type":"review-context","source":"review.txt:1","summary":"func Example() {}"}]}]}`
}

func multiAgentRunPromptAgent(systemPrompt, fallback string) string {
	for _, match := range []struct {
		prefix string
		suffix string
	}{
		{prefix: "You are agent ", suffix: " participating"},
		{prefix: "You are agent ", suffix: " reviewing"},
		{prefix: "You are reviewer ", suffix: " participating"},
		{prefix: "You are reviewer ", suffix: " cross-reviewing"},
	} {
		if agentName := targetAfter(systemPrompt, match.prefix, match.suffix); agentName != "" {
			return agentName
		}
	}

	return fallback
}

type blockingMultiAgentRunLLMProvider struct {
	models []string
}

func (p blockingMultiAgentRunLLMProvider) Name() string {
	return "multi-agent-blocking-test"
}

func (p blockingMultiAgentRunLLMProvider) Models() []string {
	return append([]string(nil), p.models...)
}

func (p blockingMultiAgentRunLLMProvider) FetchModels(context.Context) ([]string, error) {
	return p.Models(), nil
}

func (p blockingMultiAgentRunLLMProvider) HealthCheck(context.Context) error {
	return nil
}

func (p blockingMultiAgentRunLLMProvider) ModelContextWindow(string) int {
	return 128_000
}

func (p blockingMultiAgentRunLLMProvider) Complete(ctx context.Context, _ llm.CompleteParams) (*llm.Response, error) {
	<-ctx.Done()

	return nil, ctx.Err()
}

func TestMultiAgentCallInfoUsesPromptRoleInsteadOfReservedAgentName(t *testing.T) {
	t.Parallel()

	specProposal := speculateCallInfo(
		"judge",
		"You are agent judge participating in a speculative execution workflow.",
		"Task:\nship it",
	)
	assert.Equal(t, multiAgentPhaseProposal, specProposal.Phase)
	assert.Equal(t, "judge", specProposal.Agent)

	specAggregate := speculateCallInfo(
		"judge",
		"You are a judge agent aggregating speculative execution results.",
		"Select a winner.",
	)
	assert.Equal(t, multiAgentPhaseAggregateVerdict, specAggregate.Phase)

	reviewReport := reviewCallInfo(
		"review-judge",
		"You are reviewer review-judge participating in a three-round code review workflow.",
	)
	assert.Equal(t, multiAgentPhaseReviewReport, reviewReport.Phase)
	assert.Equal(t, "review-judge", reviewReport.Agent)

	reviewAggregate := reviewCallInfo(
		"review-judge",
		"You are the review judge aggregating a three-round code review.",
	)
	assert.Equal(t, multiAgentPhaseAggregateVerdict, reviewAggregate.Phase)
}

func TestCompleteMultiAgentRegistryCallAllowsNilRecorder(t *testing.T) {
	t.Parallel()

	registry := llm.NewRegistry()
	registry.Register(multiAgentRunLLMProvider{
		failures: map[string]error{
			"primary-model": errors.New("primary unavailable"),
		},
		models: []string{"primary-model", "fallback-model"},
	})

	resp, err := completeMultiAgentRegistryCall(
		t.Context(),
		nil,
		registry,
		multiAgentCallInfo{Phase: multiAgentPhaseProposal, Agent: "planner"},
		llm.CompleteParams{
			Model: "primary-model",
			Messages: []llm.Message{{
				Role:    llm.RoleSystem,
				Content: "You are agent planner participating in a speculative execution workflow.",
			}},
		},
		[]string{"fallback-model"},
	)
	require.NoError(t, err)
	require.NotNil(t, resp)
	assert.Equal(t, "fallback-model", resp.Model)
	assert.Contains(t, resp.Content, "proposal from planner")
}

func TestMultiAgentBudgetFromStateRecordsPerCallOutputBudget(t *testing.T) {
	t.Parallel()

	budget := multiAgentBudgetFromState(appState{
		generationDefaults:  generationSettings{MaxTokens: 9},
		generationOverrides: generationSettings{MaxTokens: 4},
		agentLoopBudget: llm.AgentLoopBudget{
			MaxInputTokens:  100,
			MaxOutputTokens: 50,
		},
		maxInputTokens: 12,
	})

	assert.Equal(t, 12, budget.PerCallMaxInputTokens)
	assert.Equal(t, 4, budget.PerCallMaxOutputTokens)
	assert.Equal(t, 100, budget.MaxRunInputTokens)
	assert.Equal(t, 50, budget.MaxRunOutputTokens)

	defaultOnlyBudget := multiAgentBudgetFromState(appState{
		generationDefaults: generationSettings{MaxTokens: 9},
	})
	assert.Zero(t, defaultOnlyBudget.PerCallMaxOutputTokens)
}

func TestMultiAgentRunRecorderPersistsPartialFailure(t *testing.T) {
	t.Parallel()

	store := session.NewStore(t.TempDir())
	sessionState := session.New("gpt-test", nil)
	require.NoError(t, store.Save(sessionState))

	recorder := newMultiAgentRunRecorder(
		store,
		&sessionState,
		session.MultiAgentRunKindSpeculation,
		"ship audit trail",
		"gpt-test",
		[]string{"backup"},
		session.MultiAgentRunBudget{},
		nil,
		nil,
	)
	require.NoError(t, recorder.start())

	params := llm.CompleteParams{
		Model: "gpt-test",
		Messages: []llm.Message{
			{Role: llm.RoleSystem, Content: "system prompt"},
			{Role: llm.RoleUser, Content: "user prompt"},
		},
	}

	_, err := recorder.complete(t.Context(), multiAgentCallInfo{Phase: multiAgentPhaseProposal, Agent: "planner"}, params, nil,
		func(context.Context, llm.CompleteParams, []string) (*llm.Response, error) {
			return &llm.Response{Content: "proposal", Model: "gpt-test", InputTokens: 7, OutputTokens: 3}, nil
		})
	require.NoError(t, err)

	providerErr := errors.New("provider boom")
	_, err = recorder.complete(t.Context(), multiAgentCallInfo{Phase: multiAgentPhaseProposal, Agent: "executor"}, params, nil,
		func(context.Context, llm.CompleteParams, []string) (*llm.Response, error) {
			return nil, providerErr
		})
	require.ErrorIs(t, err, providerErr)

	require.NoError(t, recorder.recordSpeculateSession(speculate.Session{
		Proposals: []speculate.Proposal{{Agent: "planner", Round: speculate.RoundProposal, Content: "proposal"}},
		Verdict: speculate.Verdict{
			Winner: "planner",
			Reason: "only completed branch",
			GateChecks: []speculate.GateCheck{{
				Name:   "tests pass",
				Passed: false,
				Notes:  "not run after provider failure",
			}},
		},
	}))
	require.NoError(t, recorder.finish(err))

	loaded, err := store.Load(sessionState.ID)
	require.NoError(t, err)
	require.Len(t, loaded.MultiAgentRuns, 1)

	run := loaded.MultiAgentRuns[0]
	assert.Equal(t, run.ID, run.ReceiptID)
	assert.Equal(t, session.MultiAgentRunStatusFailed, run.Status)
	assert.Contains(t, run.Error, "provider boom")
	require.Len(t, run.Calls, 2)
	assert.Equal(t, session.MultiAgentRunStatusCompleted, run.Calls[0].Status)
	assert.Equal(t, session.MultiAgentRunStatusFailed, run.Calls[1].Status)
	assert.NotEmpty(t, run.Calls[0].PromptHash)
	assert.Equal(t, "system prompt", run.Calls[0].SystemPrompt)
	assert.Equal(t, "user prompt", run.Calls[0].UserPrompt)
	assert.Equal(t, "proposal", run.Calls[0].Response)
	assert.Equal(t, 7, run.Usage.InputTokens)
	assert.Equal(t, 3, run.Usage.OutputTokens)
	require.Len(t, run.Artifacts, 2)
	assert.Equal(t, multiAgentArtifactProposal, run.Artifacts[0].Kind)
	require.Len(t, run.Gates, 1)
	assert.False(t, run.Gates[0].Passed)
}

func TestMultiAgentRunRecorderReturnsProviderAndPersistErrors(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	store := session.NewStore(dir)
	sessionState := session.New("gpt-test", nil)
	recorder := newMultiAgentRunRecorder(
		store,
		&sessionState,
		session.MultiAgentRunKindSpeculation,
		"surface persistence failures",
		"gpt-test",
		nil,
		session.MultiAgentRunBudget{},
		nil,
		nil,
	)
	require.NoError(t, recorder.start())

	providerErr := errors.New("provider failed after receipt start")
	_, err := recorder.complete(t.Context(), multiAgentCallInfo{Phase: multiAgentPhaseProposal, Agent: "planner"}, llm.CompleteParams{
		Model:    "gpt-test",
		Messages: []llm.Message{{Role: llm.RoleUser, Content: "draft"}},
	}, nil, func(context.Context, llm.CompleteParams, []string) (*llm.Response, error) {
		require.NoError(t, os.RemoveAll(dir))
		require.NoError(t, os.WriteFile(dir, []byte("not a directory"), 0o600))

		return nil, providerErr
	})

	require.ErrorIs(t, err, providerErr)
	assert.Contains(t, err.Error(), "persist multi-agent run")
}

func TestMultiAgentRunRecorderReturnsBudgetAndPersistErrors(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	store := session.NewStore(dir)
	sessionState := session.New("gpt-test", nil)
	recorder := newMultiAgentRunRecorder(
		store,
		&sessionState,
		session.MultiAgentRunKindSpeculation,
		"surface budget persistence failures",
		"gpt-test",
		nil,
		session.MultiAgentRunBudget{MaxRunOutputTokens: 1},
		nil,
		nil,
	)
	require.NoError(t, recorder.start())
	require.NoError(t, os.RemoveAll(dir))
	require.NoError(t, os.WriteFile(dir, []byte("not a directory"), 0o600))

	called := false
	_, err := recorder.complete(t.Context(), multiAgentCallInfo{Phase: multiAgentPhaseProposal, Agent: "planner"}, llm.CompleteParams{
		Model:     "gpt-test",
		MaxTokens: 2,
		Messages:  []llm.Message{{Role: llm.RoleUser, Content: "draft"}},
	}, nil, func(context.Context, llm.CompleteParams, []string) (*llm.Response, error) {
		called = true

		return &llm.Response{Content: "should not happen"}, nil
	})
	require.Error(t, err)
	assert.True(t, isBudgetRejection(err))
	assert.Contains(t, err.Error(), multiAgentBudgetRuleRunOutput)
	assert.Contains(t, err.Error(), "persist multi-agent run")
	assert.False(t, called)
}

func TestMultiAgentRunRecorderReturnsPostflightBudgetAndPersistErrors(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	store := session.NewStore(dir)
	sessionState := session.New("gpt-test", nil)
	recorder := newMultiAgentRunRecorder(
		store,
		&sessionState,
		session.MultiAgentRunKindSpeculation,
		"surface postflight budget persistence failures",
		"gpt-test",
		nil,
		session.MultiAgentRunBudget{MaxRunOutputTokens: 2},
		nil,
		nil,
	)
	require.NoError(t, recorder.start())

	called := false
	_, err := recorder.complete(t.Context(), multiAgentCallInfo{Phase: multiAgentPhaseProposal, Agent: "planner"}, llm.CompleteParams{
		Model:     "gpt-test",
		MaxTokens: 2,
		Messages:  []llm.Message{{Role: llm.RoleUser, Content: "draft"}},
	}, nil, func(context.Context, llm.CompleteParams, []string) (*llm.Response, error) {
		called = true

		require.NoError(t, os.RemoveAll(dir))
		require.NoError(t, os.WriteFile(dir, []byte("not a directory"), 0o600))

		return &llm.Response{Content: "too many tokens", Model: "gpt-test", InputTokens: 1, OutputTokens: 3}, nil
	})
	require.Error(t, err)
	assert.True(t, isBudgetRejection(err))
	assert.Contains(t, err.Error(), multiAgentBudgetRuleRunOutput)
	assert.Contains(t, err.Error(), "persist multi-agent run")
	assert.True(t, called)
}

func TestMultiAgentRunRecorderPersistsNilProviderResponseAsError(t *testing.T) {
	t.Parallel()

	store := session.NewStore(t.TempDir())
	sessionState := session.New("gpt-test", nil)
	recorder := newMultiAgentRunRecorder(
		store,
		&sessionState,
		session.MultiAgentRunKindSpeculation,
		"nil response should be auditable",
		"gpt-test",
		nil,
		session.MultiAgentRunBudget{},
		nil,
		nil,
	)
	require.NoError(t, recorder.start())

	_, err := recorder.complete(t.Context(), multiAgentCallInfo{Phase: multiAgentPhaseProposal, Agent: "planner"}, llm.CompleteParams{
		Model:    "gpt-test",
		Messages: []llm.Message{{Role: llm.RoleUser, Content: "draft"}},
	}, nil, func(context.Context, llm.CompleteParams, []string) (*llm.Response, error) {
		return nil, nil
	})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "provider returned nil response")
	require.NoError(t, recorder.finish(err))

	loaded, loadErr := store.Load(sessionState.ID)
	require.NoError(t, loadErr)
	require.Len(t, loaded.MultiAgentRuns, 1)

	run := loaded.MultiAgentRuns[0]
	assert.Equal(t, session.MultiAgentRunStatusError, run.Status)
	assert.Contains(t, run.Error, "provider returned nil response")
	require.Len(t, run.Calls, 1)
	assert.Equal(t, session.MultiAgentRunStatusError, run.Calls[0].Status)
	assert.Contains(t, run.Calls[0].Error, "provider returned nil response")
}

func TestMultiAgentRunRecorderPrefersCancellationOverNilProviderResponse(t *testing.T) {
	t.Parallel()

	store := session.NewStore(t.TempDir())
	sessionState := session.New("gpt-test", nil)
	recorder := newMultiAgentRunRecorder(
		store,
		&sessionState,
		session.MultiAgentRunKindSpeculation,
		"canceled nil response should stay cancellation",
		"gpt-test",
		nil,
		session.MultiAgentRunBudget{},
		nil,
		nil,
	)
	require.NoError(t, recorder.start())

	ctx, cancel := context.WithCancel(t.Context())
	_, err := recorder.complete(ctx, multiAgentCallInfo{Phase: multiAgentPhaseProposal, Agent: "planner"}, llm.CompleteParams{
		Model:    "gpt-test",
		Messages: []llm.Message{{Role: llm.RoleUser, Content: "draft"}},
	}, nil, func(context.Context, llm.CompleteParams, []string) (*llm.Response, error) {
		cancel()

		return nil, nil
	})
	require.ErrorIs(t, err, context.Canceled)
	require.NoError(t, recorder.finish(err))

	loaded, loadErr := store.Load(sessionState.ID)
	require.NoError(t, loadErr)
	require.Len(t, loaded.MultiAgentRuns, 1)

	run := loaded.MultiAgentRuns[0]
	assert.Equal(t, session.MultiAgentRunStatusCanceled, run.Status)
	require.Len(t, run.Calls, 1)
	assert.Equal(t, session.MultiAgentRunStatusCanceled, run.Calls[0].Status)
	assert.NotContains(t, run.Calls[0].Error, "provider returned nil response")
}

func TestMultiAgentPersistenceErrorPreservesRunAndPersistErrors(t *testing.T) {
	t.Parallel()

	runErr := errors.New("provider failed before receipt finalized")
	persistErr := errors.New("persist receipt failed")

	err := multiAgentPersistenceError(runErr, persistErr)
	require.ErrorIs(t, err, runErr)
	require.ErrorIs(t, err, persistErr)

	require.NoError(t, multiAgentPersistenceError(runErr, nil))
	require.ErrorIs(t, multiAgentPersistenceError(nil, persistErr), persistErr)
}

func TestMultiAgentRunRecorderPersistsRunningResumeReason(t *testing.T) {
	t.Parallel()

	store := session.NewStore(t.TempDir())
	sessionState := session.New("gpt-test", nil)
	recorder := newMultiAgentRunRecorder(
		store,
		&sessionState,
		session.MultiAgentRunKindSpeculation,
		"resume running receipt",
		"gpt-test",
		nil,
		session.MultiAgentRunBudget{},
		nil,
		nil,
	)
	require.NoError(t, recorder.start())

	loaded, err := store.Load(sessionState.ID)
	require.NoError(t, err)
	require.Len(t, loaded.MultiAgentRuns, 1)
	assert.Equal(t, session.MultiAgentRunStatusRunning, loaded.MultiAgentRuns[0].Status)
	assert.Contains(t, loaded.MultiAgentRuns[0].ResumeReason, "current state "+string(session.MultiAgentRunStatusRunning))

	_, err = recorder.complete(t.Context(), multiAgentCallInfo{Phase: multiAgentPhaseProposal, Agent: "planner"}, llm.CompleteParams{
		Model:    "gpt-test",
		Messages: []llm.Message{{Role: llm.RoleUser, Content: "plan"}},
	}, nil, func(context.Context, llm.CompleteParams, []string) (*llm.Response, error) {
		return &llm.Response{Content: "proposal", Model: "gpt-test", InputTokens: 1, OutputTokens: 1}, nil
	})
	require.NoError(t, err)

	loaded, err = store.Load(sessionState.ID)
	require.NoError(t, err)
	require.Len(t, loaded.MultiAgentRuns, 1)
	assert.Equal(t, session.MultiAgentRunStatusRunning, loaded.MultiAgentRuns[0].Status)
	assert.Contains(t, loaded.MultiAgentRuns[0].ResumeReason, "from 1 recorded calls")
	require.Len(t, loaded.MultiAgentRuns[0].Artifacts, 1)
	assert.Equal(t, multiAgentArtifactProposal, loaded.MultiAgentRuns[0].Artifacts[0].Kind)
	assert.Equal(t, "call-001", loaded.MultiAgentRuns[0].Artifacts[0].Metadata["call_id"])
	assert.Equal(t, "true", loaded.MultiAgentRuns[0].Artifacts[0].Metadata["raw_provider_response"])
	assert.Equal(t, "proposal", loaded.MultiAgentRuns[0].Artifacts[0].Content)

	require.NoError(t, recorder.finish(nil))

	loaded, err = store.Load(sessionState.ID)
	require.NoError(t, err)
	require.Len(t, loaded.MultiAgentRuns, 1)
	assert.Equal(t, session.MultiAgentRunStatusCompleted, loaded.MultiAgentRuns[0].Status)
	assert.Empty(t, loaded.MultiAgentRuns[0].ResumeReason)
}

func TestRunSpeculateExecutionPersistsReplayableSessionArtifacts(t *testing.T) { //nolint:paralleltest // captures process-global stdout.
	store := session.NewStore(t.TempDir())
	sessionState := session.New("gpt-test", nil)
	registry := llm.NewRegistry()
	registry.Register(multiAgentRunLLMProvider{models: []string{"gpt-test"}})

	state := appState{
		registry:      registry,
		sessionStore:  store,
		sessionState:  sessionState,
		selectedModel: "gpt-test",
	}

	var runErr error

	out := captureStdoutForExport(t, func() {
		runErr = runSpeculateExecution(t.Context(), state, speculateRunCommandInput{
			Prompt: "persist this workflow",
			Agents: []string{"planner", "coder"},
			Gates:  []string{"tests pass"},
		})
	})
	require.NoError(t, runErr)
	assert.Contains(t, out, "winner: planner")

	loaded, err := store.Load(sessionState.ID)
	require.NoError(t, err)
	require.Len(t, loaded.MultiAgentRuns, 1)

	run := loaded.MultiAgentRuns[0]
	assert.Equal(t, session.MultiAgentRunKindSpeculation, run.Kind)
	assert.Equal(t, session.MultiAgentRunStatusCompleted, run.Status)
	assert.Equal(t, run.ID, run.ReceiptID)
	assert.Equal(t, "persist this workflow", run.Prompt)
	assert.Equal(t, "gpt-test", run.Model)
	assert.Equal(t, "planner", run.Summary.Winner)
	assert.Equal(t, "best recorded evidence", run.Summary.Reason)
	assert.Equal(t, 5, run.Usage.ModelCalls)
	assert.Equal(t, 5, run.Usage.CompletedCalls)
	require.Len(t, run.Calls, 5)
	require.Len(t, run.Artifacts, 5)
	assertMultiAgentRunArtifactsHaveProviderCallIDs(t, run.Artifacts)
	require.Len(t, run.Decisions, 3)
	require.Len(t, run.Gates, 1)
	assert.Equal(t, multiAgentArtifactProposal, run.Artifacts[0].Kind)
	assert.Equal(t, multiAgentArtifactCrossReview, run.Artifacts[2].Kind)
	assert.Equal(t, multiAgentArtifactVerdict, run.Artifacts[4].Kind)
	assert.Equal(t, multiAgentDecisionAccepted, run.Decisions[0].Outcome)
	assert.Equal(t, multiAgentDecisionRejected, run.Decisions[1].Outcome)
	assert.True(t, run.Gates[0].Passed)
	require.Len(t, run.Reviewers, 3)
	assert.True(t, multiAgentRunAggregateReviewerExists(run.Reviewers, "judge"))
	assert.False(t, multiAgentRunAggregateReviewerExists(run.Reviewers, "planner"))

	replay := formatMultiAgentRunReplay(run)
	assert.Contains(t, formatMultiAgentRunResume(run), "provider_calls: skipped")
	assert.Contains(t, replay, "prompt: persist this workflow")
	assert.Contains(t, replay, "model: gpt-test")
	assert.Contains(t, replay, "branches:")
	assert.Contains(t, replay, "reviewers:")
	assert.Contains(t, replay, "disagreements:")
	assert.Contains(t, replay, "recorded_calls:")
	assert.Contains(t, replay, "prompt_hash=sha256:")
	assert.Contains(t, replay, "decisions:")
	assert.Contains(t, replay, "proposal from planner")
	assert.Contains(t, replay, "cross-review from coder")
}

func TestRunSpeculateExecutionUsesConfiguredAgentModels(t *testing.T) { //nolint:paralleltest // captures process-global stdout.
	store := session.NewStore(t.TempDir())
	sessionState := session.New("shared-model", nil)
	registry := llm.NewRegistry()
	registry.Register(multiAgentRunLLMProvider{
		models: []string{"shared-model", "global-backup", "planner-model", "planner-backup", "coder-model"},
	})

	state := appState{
		registry: registry,
		agentRegistry: agent.NewRegistry(map[string]appconfig.AgentConfig{
			"planner": {Model: "planner-model", FallbackModels: []string{"planner-backup"}, MaxTokens: 77},
			"coder":   {Model: "coder-model", MaxTokens: 80},
		}),
		sessionStore:   store,
		sessionState:   sessionState,
		selectedModel:  "shared-model",
		fallbackModels: []string{"global-backup"},
		generationDefaults: generationSettings{
			MaxTokens: 99,
		},
	}

	var runErr error

	_ = captureStdoutForExport(t, func() {
		runErr = runSpeculateExecution(t.Context(), state, speculateRunCommandInput{
			Prompt: "use branch agents without treating names as models",
			Agents: []string{"planner", "coder"},
			Gates:  []string{"tests pass"},
		})
	})
	require.NoError(t, runErr)

	loaded, err := store.Load(sessionState.ID)
	require.NoError(t, err)
	require.Len(t, loaded.MultiAgentRuns, 1)

	run := loaded.MultiAgentRuns[0]
	assert.Equal(t, "shared-model", run.Model)

	plannerProposal := requireMultiAgentRunCall(t, run, multiAgentPhaseProposal, "planner", "")
	assert.Equal(t, "planner-model", plannerProposal.RequestedModel)
	assert.Equal(t, []string{"planner-backup"}, plannerProposal.FallbackModels)
	assert.Equal(t, 77, plannerProposal.MaxOutputTokens)

	coderProposal := requireMultiAgentRunCall(t, run, multiAgentPhaseProposal, "coder", "")
	assert.Equal(t, "coder-model", coderProposal.RequestedModel)
	assert.Equal(t, []string{"global-backup"}, coderProposal.FallbackModels)
	assert.Equal(t, 80, coderProposal.MaxOutputTokens)

	plannerCrossReview := requireMultiAgentRunCall(t, run, multiAgentPhaseCrossReview, "planner", "coder")
	assert.Equal(t, "planner-model", plannerCrossReview.RequestedModel)
	assert.Equal(t, []string{"planner-backup"}, plannerCrossReview.FallbackModels)

	aggregate := requireMultiAgentRunCall(t, run, multiAgentPhaseAggregateVerdict, "judge", "")
	assert.Equal(t, "shared-model", aggregate.RequestedModel)
	assert.Equal(t, []string{"global-backup"}, aggregate.FallbackModels)
	assert.Equal(t, 99, aggregate.MaxOutputTokens)
	assert.Equal(t, "planner", run.Summary.Winner)
}

func TestRunSpeculateExecutionPersistsFallbackAttemptErrors(t *testing.T) { //nolint:paralleltest // captures process-global stdout.
	primaryErr := errors.New("primary model unavailable")
	store := session.NewStore(t.TempDir())
	sessionState := session.New("judge-model", nil)
	registry := llm.NewRegistry()
	registry.Register(multiAgentRunLLMProvider{
		models:   []string{"judge-model", "primary-model", "fallback-model"},
		failures: map[string]error{"primary-model": primaryErr},
	})

	state := appState{
		registry: registry,
		agentRegistry: agent.NewRegistry(map[string]appconfig.AgentConfig{
			"planner": {Model: "primary-model", FallbackModels: []string{"fallback-model"}},
		}),
		sessionStore:  store,
		sessionState:  sessionState,
		selectedModel: "judge-model",
	}

	var runErr error

	_ = captureStdoutForExport(t, func() {
		runErr = runSpeculateExecution(t.Context(), state, speculateRunCommandInput{
			Prompt: "record fallback failure",
			Agents: []string{"planner"},
			Gates:  []string{"tests pass"},
		})
	})
	require.NoError(t, runErr)

	loaded, err := store.Load(sessionState.ID)
	require.NoError(t, err)
	require.Len(t, loaded.MultiAgentRuns, 1)

	run := loaded.MultiAgentRuns[0]
	assert.Equal(t, session.MultiAgentRunStatusCompleted, run.Status)
	assert.Equal(t, 3, run.Usage.ModelCalls)
	assert.Equal(t, 1, run.Usage.ProviderFailedCalls)
	assert.Equal(t, 2, run.Usage.CompletedCalls)
	require.Len(t, run.Calls, 3)
	assert.Equal(t, session.MultiAgentRunStatusError, run.Calls[0].Status)
	assert.Equal(t, "primary-model", run.Calls[0].RequestedModel)
	assert.Equal(t, []string{"fallback-model"}, run.Calls[0].FallbackModels)
	assert.Contains(t, run.Calls[0].Error, "primary model unavailable")
	assert.Equal(t, session.MultiAgentRunStatusCompleted, run.Calls[1].Status)
	assert.Equal(t, "fallback-model", run.Calls[1].RequestedModel)
	assert.Empty(t, run.Calls[1].FallbackModels)
	assert.Equal(t, "proposal from planner", run.Calls[1].Response)
	assert.Equal(t, "judge-model", run.Calls[2].RequestedModel)
	require.Len(t, run.Artifacts, 2)
	assert.Equal(t, "call-002", run.Artifacts[0].Metadata["call_id"])

	replay := formatMultiAgentRunReplay(run)
	assert.Contains(t, replay, "id=call-001")
	assert.Contains(t, replay, "status=error")
	assert.Contains(t, replay, "model=primary-model")
	assert.Contains(t, replay, "error=llm: multi-agent-test/primary-model failed after 1 attempt")
	assert.Contains(t, replay, "primary model unavailable")
}

func TestMultiAgentRunRecorderAnnotatesStructuredArtifactWithSuccessfulFallbackCall(t *testing.T) {
	t.Parallel()

	store := session.NewStore(t.TempDir())
	sessionState := session.New("gpt-test", nil)
	recorder := newMultiAgentRunRecorder(
		store,
		&sessionState,
		session.MultiAgentRunKindSpeculation,
		"retry branch with partial output",
		"gpt-test",
		nil,
		session.MultiAgentRunBudget{},
		nil,
		nil,
	)
	require.NoError(t, recorder.start())

	params := llm.CompleteParams{
		Model:    "gpt-test",
		Messages: []llm.Message{{Role: llm.RoleUser, Content: "draft"}},
	}

	partialErr := errors.New("stream interrupted before retry")
	_, err := recorder.complete(
		t.Context(),
		multiAgentCallInfo{Phase: multiAgentPhaseProposal, Agent: "planner"},
		params,
		[]string{"backup"},
		func(context.Context, llm.CompleteParams, []string) (*llm.Response, error) {
			return &llm.Response{Content: "partial proposal", Model: "gpt-test", InputTokens: 4, OutputTokens: 2}, partialErr
		},
	)
	require.ErrorIs(t, err, partialErr)

	_, err = recorder.complete(
		t.Context(),
		multiAgentCallInfo{Phase: multiAgentPhaseProposal, Agent: "planner"},
		params,
		nil,
		func(context.Context, llm.CompleteParams, []string) (*llm.Response, error) {
			return &llm.Response{Content: "final proposal", Model: "backup", InputTokens: 4, OutputTokens: 2}, nil
		},
	)
	require.NoError(t, err)

	require.NoError(t, recorder.recordSpeculateSession(speculate.Session{
		Proposals: []speculate.Proposal{{
			Agent:   "planner",
			Round:   speculate.RoundProposal,
			Content: "final proposal",
		}},
	}))

	loaded, err := store.Load(sessionState.ID)
	require.NoError(t, err)
	require.Len(t, loaded.MultiAgentRuns, 1)

	run := loaded.MultiAgentRuns[0]
	require.Len(t, run.Calls, 2)
	assert.Equal(t, session.MultiAgentRunStatusError, run.Calls[0].Status)
	assert.Equal(t, session.MultiAgentRunStatusCompleted, run.Calls[1].Status)
	require.Len(t, run.Artifacts, 2)
	assert.Equal(t, "final proposal", run.Artifacts[0].Content)
	assert.Equal(t, "call-002", run.Artifacts[0].Metadata["call_id"])
	assert.NotEqual(t, multiAgentMetadataTrue, run.Artifacts[0].Metadata[multiAgentRawProviderResponseKey])
	assert.Equal(t, "partial proposal", run.Artifacts[1].Content)
	assert.Equal(t, "call-001", run.Artifacts[1].Metadata["call_id"])
	assert.Equal(t, multiAgentMetadataTrue, run.Artifacts[1].Metadata[multiAgentRawProviderResponseKey])
	require.Len(t, run.Decisions, 2)
	assert.Equal(t, 1, run.Decisions[0].Index)
	assert.Equal(t, 2, run.Decisions[1].Index)
}

func TestRunSpeculateExecutionEnforcesRunWallTimeBudget(t *testing.T) {
	t.Parallel()

	store := session.NewStore(t.TempDir())
	sessionState := session.New("gpt-test", nil)
	registry := llm.NewRegistry()
	registry.Register(blockingMultiAgentRunLLMProvider{models: []string{"gpt-test"}})

	state := appState{
		registry:        registry,
		sessionStore:    store,
		sessionState:    sessionState,
		selectedModel:   "gpt-test",
		agentLoopBudget: llm.AgentLoopBudget{MaxWallTime: 5 * time.Millisecond},
	}

	err := runSpeculateExecution(t.Context(), state, speculateRunCommandInput{
		Prompt: "respect run wall-time budget",
		Agents: []string{"planner", "coder"},
		Gates:  []string{"tests pass"},
	})
	require.Error(t, err)
	assert.Contains(t, err.Error(), multiAgentBudgetRuleRunWallTime)

	loaded, loadErr := store.Load(sessionState.ID)
	require.NoError(t, loadErr)
	require.Len(t, loaded.MultiAgentRuns, 1)

	run := loaded.MultiAgentRuns[0]
	assert.Equal(t, session.MultiAgentRunStatusBudgetExhausted, run.Status)
	assert.Equal(t, int64(5), run.Budget.MaxRunWallTimeMS)
	assert.Contains(t, run.Error, multiAgentBudgetRuleRunWallTime)
	assert.Contains(t, run.ResumeReason, "terminal state "+string(session.MultiAgentRunStatusBudgetExhausted))
	require.Len(t, run.Calls, 2)
	assert.Equal(t, 2, run.Usage.CanceledCalls)
	assert.Equal(t, session.MultiAgentRunStatusCanceled, run.Calls[0].Status)
	assert.Equal(t, session.MultiAgentRunStatusCanceled, run.Calls[1].Status)
}

func TestRunSpeculateExecutionFailsClosedWhenCostBudgetHasNoEstimator(t *testing.T) {
	t.Parallel()

	store := session.NewStore(t.TempDir())
	sessionState := session.New("gpt-test", nil)
	registry := llm.NewRegistry()
	registry.Register(multiAgentRunLLMProvider{models: []string{"gpt-test"}})

	state := appState{
		registry:        registry,
		sessionStore:    store,
		sessionState:    sessionState,
		selectedModel:   "gpt-test",
		agentLoopBudget: llm.AgentLoopBudget{MaxCostMicros: 5},
	}

	err := runSpeculateExecution(t.Context(), state, speculateRunCommandInput{
		Prompt: "do not run without auditable cost",
		Agents: []string{"planner", "coder"},
		Gates:  []string{"tests pass"},
	})
	require.Error(t, err)
	assert.Contains(t, err.Error(), multiAgentBudgetRuleRunCost)

	loaded, loadErr := store.Load(sessionState.ID)
	require.NoError(t, loadErr)
	require.Len(t, loaded.MultiAgentRuns, 1)

	run := loaded.MultiAgentRuns[0]
	assert.Equal(t, session.MultiAgentRunStatusBudgetExhausted, run.Status)
	assert.Equal(t, int64(5), run.Budget.MaxRunCostMicros)
	assert.Contains(t, run.Error, "requires a cost estimator")
	assert.Contains(t, run.ResumeReason, "terminal state "+string(session.MultiAgentRunStatusBudgetExhausted))
	assert.Empty(t, run.Calls)
	assert.Empty(t, run.Artifacts)
	assert.Zero(t, run.Usage.ModelCalls)
}

func TestRunReviewExecutionEnforcesRunWallTimeBudget(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	require.NoError(t, os.WriteFile(
		filepath.Join(root, "review.txt"),
		[]byte("func Example() {}\n\nCommand output:\ngo test ./... PASS\n"),
		0o600,
	))

	store := session.NewStore(t.TempDir())
	sessionState := session.New("quality", nil)
	registry := llm.NewRegistry()
	registry.Register(blockingMultiAgentRunLLMProvider{models: []string{"quality", "tests"}})

	state := appState{
		registry:        registry,
		sessionStore:    store,
		sessionState:    sessionState,
		selectedModel:   "quality",
		contextOptions:  contextref.Options{Root: root},
		agentLoopBudget: llm.AgentLoopBudget{MaxWallTime: 5 * time.Millisecond},
	}

	err := runReviewExecution(t.Context(), state, reviewRunCommandInput{
		Prompt: "respect review run wall-time budget",
		Agents: []string{"quality", "tests"},
		Paths:  []string{"review.txt"},
		Gates:  []string{"tests pass"},
	})
	require.Error(t, err)
	assert.Contains(t, err.Error(), multiAgentBudgetRuleRunWallTime)

	loaded, loadErr := store.Load(sessionState.ID)
	require.NoError(t, loadErr)
	require.Len(t, loaded.MultiAgentRuns, 1)

	run := loaded.MultiAgentRuns[0]
	assert.Equal(t, session.MultiAgentRunStatusBudgetExhausted, run.Status)
	assert.Equal(t, session.MultiAgentRunKindReview, run.Kind)
	assert.Equal(t, int64(5), run.Budget.MaxRunWallTimeMS)
	assert.Contains(t, run.Error, multiAgentBudgetRuleRunWallTime)
	assert.Contains(t, run.ResumeReason, "terminal state "+string(session.MultiAgentRunStatusBudgetExhausted))
	require.Len(t, run.Calls, 2)
	assert.Equal(t, 2, run.Usage.CanceledCalls)
	assert.Equal(t, session.MultiAgentRunStatusCanceled, run.Calls[0].Status)
	assert.Equal(t, session.MultiAgentRunStatusCanceled, run.Calls[1].Status)
}

func TestRunReviewExecutionFailsClosedWhenCostBudgetHasNoEstimator(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(root, "review.txt"), []byte("func Example() {}\n"), 0o600))

	store := session.NewStore(t.TempDir())
	sessionState := session.New("quality", nil)
	registry := llm.NewRegistry()
	registry.Register(multiAgentRunLLMProvider{models: []string{"quality", "tests"}})

	state := appState{
		registry:        registry,
		sessionStore:    store,
		sessionState:    sessionState,
		selectedModel:   "quality",
		contextOptions:  contextref.Options{Root: root},
		agentLoopBudget: llm.AgentLoopBudget{MaxCostMicros: 5},
	}

	err := runReviewExecution(t.Context(), state, reviewRunCommandInput{
		Prompt: "do not run review without auditable cost",
		Agents: []string{"quality", "tests"},
		Paths:  []string{"review.txt"},
		Gates:  []string{"tests pass"},
	})
	require.Error(t, err)
	assert.Contains(t, err.Error(), multiAgentBudgetRuleRunCost)

	loaded, loadErr := store.Load(sessionState.ID)
	require.NoError(t, loadErr)
	require.Len(t, loaded.MultiAgentRuns, 1)

	run := loaded.MultiAgentRuns[0]
	assert.Equal(t, session.MultiAgentRunStatusBudgetExhausted, run.Status)
	assert.Equal(t, session.MultiAgentRunKindReview, run.Kind)
	assert.Equal(t, int64(5), run.Budget.MaxRunCostMicros)
	assert.Contains(t, run.Error, "requires a cost estimator")
	assert.Contains(t, run.ResumeReason, "terminal state "+string(session.MultiAgentRunStatusBudgetExhausted))
	assert.Empty(t, run.Calls)
	assert.Empty(t, run.Artifacts)
	assert.Zero(t, run.Usage.ModelCalls)
}

func TestRunReviewExecutionPersistsReplayableSessionArtifacts(t *testing.T) { //nolint:paralleltest // captures process-global stdout.
	root := t.TempDir()
	require.NoError(t, os.WriteFile(
		filepath.Join(root, "review.txt"),
		[]byte("func Example() {}\n\nCommand output:\ngo test ./... PASS\n"),
		0o600,
	))

	store := session.NewStore(t.TempDir())
	sessionState := session.New("quality", nil)
	registry := llm.NewRegistry()
	registry.Register(multiAgentRunLLMProvider{models: []string{"quality"}})

	state := appState{
		registry:       registry,
		sessionStore:   store,
		sessionState:   sessionState,
		selectedModel:  "quality",
		contextOptions: contextref.Options{Root: root},
	}

	var runErr error

	out := captureStdoutForExport(t, func() {
		runErr = runReviewExecution(t.Context(), state, reviewRunCommandInput{
			Prompt: "persist review workflow",
			Agents: []string{"quality", "tests"},
			Paths:  []string{"review.txt"},
			Gates:  []string{"source reviewed"},
		})
	})
	require.NoError(t, runErr)
	assert.Contains(t, out, "aggregate_report")

	loaded, err := store.Load(sessionState.ID)
	require.NoError(t, err)
	require.Len(t, loaded.MultiAgentRuns, 1)

	run := loaded.MultiAgentRuns[0]
	assert.Equal(t, session.MultiAgentRunKindReview, run.Kind)
	assert.Equal(t, session.MultiAgentRunStatusCompleted, run.Status)
	assert.Equal(t, run.ID, run.ReceiptID)
	assert.Contains(t, run.Prompt, "persist review workflow")
	assert.Equal(t, "quality", run.Model)
	assert.Equal(t, "aggregate-verdict", run.Summary.VerdictReviewer)
	assert.Equal(t, 5, run.Usage.ModelCalls)
	assert.Equal(t, 5, run.Usage.CompletedCalls)
	require.Len(t, run.Calls, 5)
	require.Len(t, run.Artifacts, 5)
	assertMultiAgentRunArtifactsHaveProviderCallIDs(t, run.Artifacts)
	require.Len(t, run.Decisions, 3)
	require.Len(t, run.Gates, 3)
	assert.Equal(t, multiAgentArtifactReviewReport, run.Artifacts[0].Kind)
	assert.Equal(t, multiAgentArtifactCrossReview, run.Artifacts[2].Kind)
	assert.Equal(t, multiAgentArtifactVerdict, run.Artifacts[4].Kind)
	assert.Contains(t, run.Artifacts[2].Content, "cross-review note")
	assert.Equal(t, multiAgentDecisionRejected, run.Decisions[0].Outcome)
	assert.Equal(t, "superseded by aggregate verdict", run.Decisions[0].Rationale)
	assert.Equal(t, multiAgentDecisionRejected, run.Decisions[1].Outcome)
	assert.Equal(t, multiAgentDecisionAccepted, run.Decisions[2].Outcome)
	assert.True(t, run.Gates[0].Passed)
	require.Len(t, run.Reviewers, 5)
	assert.True(t, multiAgentRunAggregateReviewerExists(run.Reviewers, "review-judge"))
	assert.False(t, multiAgentRunAggregateReviewerExists(run.Reviewers, "aggregate-verdict"))

	replay := formatMultiAgentRunReplay(run)
	assert.Contains(t, formatMultiAgentRunResume(run), "provider_calls: skipped")
	assert.Contains(t, replay, "model: quality")
	assert.Contains(t, replay, "usage: model_calls=5")
	assert.Contains(t, replay, "branches:")
	assert.Contains(t, replay, "reviewers:")
	assert.Contains(t, replay, "disagreements:")
	assert.Contains(t, replay, "independent_reports:")
	assert.Contains(t, replay, "decisions:")
	assert.Contains(t, replay, "cross-review note")
	assert.Contains(t, replay, "aggregate_report:")
}

func TestMultiAgentRunRecorderRecordsCancellation(t *testing.T) {
	t.Parallel()

	store := session.NewStore(t.TempDir())
	sessionState := session.New("gpt-test", nil)
	recorder := newMultiAgentRunRecorder(
		store,
		&sessionState,
		session.MultiAgentRunKindReview,
		"review context",
		"gpt-test",
		nil,
		session.MultiAgentRunBudget{},
		nil,
		nil,
	)
	require.NoError(t, recorder.start())

	ctx, cancel := context.WithCancel(t.Context())
	cancel()

	called := false
	_, err := recorder.complete(ctx, multiAgentCallInfo{Phase: multiAgentPhaseReviewReport, Agent: "quality"}, llm.CompleteParams{
		Model:    "gpt-test",
		Messages: []llm.Message{{Role: llm.RoleUser, Content: "review"}},
	}, nil, func(ctx context.Context, _ llm.CompleteParams, _ []string) (*llm.Response, error) {
		called = true
		return nil, ctx.Err()
	})
	require.ErrorIs(t, err, context.Canceled)
	assert.False(t, called)
	require.NoError(t, recorder.finish(err))

	loaded, err := store.Load(sessionState.ID)
	require.NoError(t, err)
	require.Len(t, loaded.MultiAgentRuns, 1)
	assert.Equal(t, session.MultiAgentRunStatusCanceled, loaded.MultiAgentRuns[0].Status)
	assert.NotEmpty(t, loaded.MultiAgentRuns[0].CancellationReason)
	assert.Contains(t, loaded.MultiAgentRuns[0].ResumeReason, "terminal state "+string(session.MultiAgentRunStatusCanceled))
	require.Len(t, loaded.MultiAgentRuns[0].Calls, 1)
	assert.Equal(t, session.MultiAgentRunStatusCanceled, loaded.MultiAgentRuns[0].Calls[0].Status)
	assert.Equal(t, "review", loaded.MultiAgentRuns[0].Calls[0].UserPrompt)
	assert.Equal(t, 0, loaded.MultiAgentRuns[0].Usage.ModelCalls)
	assert.Equal(t, 1, loaded.MultiAgentRuns[0].Usage.CanceledCalls)
}

func TestMultiAgentRunRecorderRecordsPartialCancellationResponse(t *testing.T) {
	t.Parallel()

	store := session.NewStore(t.TempDir())
	sessionState := session.New("gpt-test", nil)
	recorder := newMultiAgentRunRecorder(
		store,
		&sessionState,
		session.MultiAgentRunKindReview,
		"review context",
		"gpt-test",
		nil,
		session.MultiAgentRunBudget{},
		nil,
		nil,
	)
	require.NoError(t, recorder.start())

	ctx, cancel := context.WithCancel(t.Context())
	_, err := recorder.complete(ctx, multiAgentCallInfo{Phase: multiAgentPhaseReviewReport, Agent: "quality"}, llm.CompleteParams{
		Model:    "gpt-test",
		Messages: []llm.Message{{Role: llm.RoleUser, Content: "review"}},
	}, nil, func(context.Context, llm.CompleteParams, []string) (*llm.Response, error) {
		cancel()

		return &llm.Response{Content: "partial report", Model: "gpt-test", InputTokens: 4, OutputTokens: 2}, nil
	})
	require.ErrorIs(t, err, context.Canceled)
	require.NoError(t, recorder.recordReviewSession(review.Session{}))
	require.NoError(t, recorder.finish(err))

	loaded, err := store.Load(sessionState.ID)
	require.NoError(t, err)
	require.Len(t, loaded.MultiAgentRuns, 1)
	require.Len(t, loaded.MultiAgentRuns[0].Calls, 1)

	call := loaded.MultiAgentRuns[0].Calls[0]
	assert.Equal(t, session.MultiAgentRunStatusCanceled, call.Status)
	assert.NotEmpty(t, call.PromptHash)
	assert.Equal(t, "partial report", call.Response)
	assert.Equal(t, 4, call.InputTokens)
	assert.Equal(t, 2, call.OutputTokens)
	assert.Equal(t, 6, call.TotalTokens)
	assert.Equal(t, 1, loaded.MultiAgentRuns[0].Usage.CanceledCalls)
	require.Len(t, loaded.MultiAgentRuns[0].Artifacts, 1)
	assert.Equal(t, multiAgentArtifactReviewReport, loaded.MultiAgentRuns[0].Artifacts[0].Kind)
	assert.Equal(t, "partial report", loaded.MultiAgentRuns[0].Artifacts[0].Content)
	assert.Equal(t, "call-001", loaded.MultiAgentRuns[0].Artifacts[0].Metadata["call_id"])
	require.Len(t, loaded.MultiAgentRuns[0].Decisions, 1)
	assert.Equal(t, multiAgentArtifactReviewReport, loaded.MultiAgentRuns[0].Decisions[0].Kind)
	assert.Equal(t, multiAgentDecisionRejected, loaded.MultiAgentRuns[0].Decisions[0].Outcome)
	assert.Equal(t, "run stopped before aggregate verdict", loaded.MultiAgentRuns[0].Decisions[0].Rationale)
}

func TestMultiAgentRunRecorderRecordsPartialProviderErrorResponse(t *testing.T) {
	t.Parallel()

	store := session.NewStore(t.TempDir())
	sessionState := session.New("gpt-test", nil)
	recorder := newMultiAgentRunRecorder(
		store,
		&sessionState,
		session.MultiAgentRunKindSpeculation,
		"partial provider error",
		"gpt-test",
		nil,
		session.MultiAgentRunBudget{},
		nil,
		nil,
	)
	require.NoError(t, recorder.start())

	providerErr := errors.New("stream interrupted")
	_, err := recorder.complete(t.Context(), multiAgentCallInfo{Phase: multiAgentPhaseProposal, Agent: "planner"}, llm.CompleteParams{
		Model:    "gpt-test",
		Messages: []llm.Message{{Role: llm.RoleUser, Content: "draft"}},
	}, nil, func(context.Context, llm.CompleteParams, []string) (*llm.Response, error) {
		return &llm.Response{Content: "partial proposal", Model: "gpt-test", InputTokens: 4, OutputTokens: 2}, providerErr
	})
	require.ErrorIs(t, err, providerErr)
	require.NoError(t, recorder.finish(err))

	loaded, loadErr := store.Load(sessionState.ID)
	require.NoError(t, loadErr)
	require.Len(t, loaded.MultiAgentRuns, 1)

	run := loaded.MultiAgentRuns[0]
	assert.Equal(t, session.MultiAgentRunStatusError, run.Status)
	assert.Contains(t, run.Error, "stream interrupted")
	assert.Equal(t, 1, run.Usage.ProviderFailedCalls)
	require.Len(t, run.Calls, 1)
	assert.Equal(t, session.MultiAgentRunStatusError, run.Calls[0].Status)
	assert.Equal(t, "partial proposal", run.Calls[0].Response)
	assert.Equal(t, 6, run.Calls[0].TotalTokens)
	require.Len(t, run.Artifacts, 1)
	assert.Equal(t, multiAgentArtifactProposal, run.Artifacts[0].Kind)
	assert.Equal(t, "partial proposal", run.Artifacts[0].Content)
	assert.Equal(t, "call-001", run.Artifacts[0].Metadata["call_id"])
	assert.Equal(t, "true", run.Artifacts[0].Metadata["raw_provider_response"])
}

func TestMultiAgentRunRecorderKeepsDistinctRawArtifactsForSameReviewer(t *testing.T) {
	t.Parallel()

	store := session.NewStore(t.TempDir())
	sessionState := session.New("gpt-test", nil)
	recorder := newMultiAgentRunRecorder(
		store,
		&sessionState,
		session.MultiAgentRunKindReview,
		"review context",
		"gpt-test",
		nil,
		session.MultiAgentRunBudget{},
		nil,
		nil,
	)
	require.NoError(t, recorder.start())

	for _, content := range []string{"first unparsed report", "second unparsed report"} {
		_, err := recorder.complete(t.Context(), multiAgentCallInfo{Phase: multiAgentPhaseReviewReport, Agent: "quality"}, llm.CompleteParams{
			Model:    "gpt-test",
			Messages: []llm.Message{{Role: llm.RoleUser, Content: "review " + content}},
		}, nil, func(context.Context, llm.CompleteParams, []string) (*llm.Response, error) {
			return &llm.Response{Content: content, Model: "gpt-test", InputTokens: 4, OutputTokens: 2}, nil
		})
		require.NoError(t, err)
	}

	require.NoError(t, recorder.recordReviewSession(review.Session{}))

	loaded, err := store.Load(sessionState.ID)
	require.NoError(t, err)
	require.Len(t, loaded.MultiAgentRuns, 1)

	run := loaded.MultiAgentRuns[0]
	require.Len(t, run.Artifacts, 2)
	assert.Equal(t, "first unparsed report", run.Artifacts[0].Content)
	assert.Equal(t, "call-001", run.Artifacts[0].Metadata["call_id"])
	assert.Equal(t, "second unparsed report", run.Artifacts[1].Content)
	assert.Equal(t, "call-002", run.Artifacts[1].Metadata["call_id"])
	require.Len(t, run.Decisions, 2)
	assert.Equal(t, 1, run.Decisions[0].Index)
	assert.Equal(t, 2, run.Decisions[1].Index)
}

func TestArtifactDecisionRepresentedMatchesSameReviewerByIndex(t *testing.T) {
	t.Parallel()

	decisions := []session.MultiAgentRunDecision{
		{
			Kind:      multiAgentArtifactReviewReport,
			Phase:     multiAgentPhaseReviewReport,
			Agent:     "quality",
			Outcome:   multiAgentDecisionRejected,
			Rationale: "first",
			Index:     1,
		},
		{
			Kind:      multiAgentArtifactReviewReport,
			Phase:     multiAgentPhaseReviewReport,
			Agent:     "quality",
			Outcome:   multiAgentDecisionRejected,
			Rationale: "second",
			Index:     2,
		},
	}

	assert.True(t, artifactDecisionRepresented(session.MultiAgentRunArtifact{
		Kind:  multiAgentArtifactReviewReport,
		Phase: multiAgentPhaseReviewReport,
		Agent: "quality",
		Index: 2,
	}, decisions))
	assert.False(t, artifactDecisionRepresented(session.MultiAgentRunArtifact{
		Kind:  multiAgentArtifactReviewReport,
		Phase: multiAgentPhaseReviewReport,
		Agent: "quality",
		Index: 3,
	}, decisions))
	assert.True(t, artifactDecisionRepresented(session.MultiAgentRunArtifact{
		Kind:  multiAgentArtifactReviewReport,
		Phase: multiAgentPhaseReviewReport,
		Agent: "quality",
	}, decisions), "legacy artifacts without index should still match by reviewer identity")
}

func TestMultiAgentRunRecorderRecordsReviewSessionArtifacts(t *testing.T) {
	t.Parallel()

	store := session.NewStore(t.TempDir())
	sessionState := session.New("gpt-test", nil)
	recorder := newMultiAgentRunRecorder(
		store,
		&sessionState,
		session.MultiAgentRunKindReview,
		"review context",
		"gpt-test",
		nil,
		session.MultiAgentRunBudget{},
		nil,
		nil,
	)
	require.NoError(t, recorder.start())

	plan, err := review.NewPlan(
		[]review.Reviewer{{Name: "quality"}, {Name: "tests"}},
		[]string{"pkg/example.go"},
		[]string{"tests pass"},
	)
	require.NoError(t, err)

	require.NoError(t, recorder.recordReviewSession(review.Session{
		Plan: plan,
		Reports: []review.Report{{
			Reviewer: "quality",
			Findings: []review.Finding{{
				Severity: review.SeverityHigh,
				Category: review.CategoryCorrectness,
				Path:     "pkg/example.go",
				Line:     12,
				Message:  "recorded finding",
			}},
			GateChecks: []review.GateCheck{{Name: "tests pass", Passed: true, Notes: "unit evidence"}},
		}},
		CrossReviews: []review.CrossReviewNote{{
			Reviewer:         "tests",
			ReviewedReviewer: "quality",
			Notes:            "challenge recorded finding",
		}},
		Verdict: review.Report{
			Reviewer:   "aggregate-verdict",
			GateChecks: []review.GateCheck{{Name: "tests pass", Passed: false, Notes: "integration missing"}},
		},
		Errors: []review.RunError{{
			Stage:            string(review.RoundCrossReview),
			Reviewer:         "tests",
			ReviewedReviewer: "quality",
			Message:          "cross-review parse failed",
		}},
	}))

	loaded, err := store.Load(sessionState.ID)
	require.NoError(t, err)
	require.Len(t, loaded.MultiAgentRuns, 1)

	run := loaded.MultiAgentRuns[0]
	assert.Equal(t, session.MultiAgentRunKindReview, run.Kind)
	assert.Empty(t, run.Summary.VerdictReviewer)
	assert.Zero(t, run.Summary.Findings)
	require.Len(t, run.Artifacts, 3)
	require.Len(t, run.Decisions, 2)
	assert.Equal(t, multiAgentArtifactReviewReport, run.Artifacts[0].Kind)
	assert.Equal(t, multiAgentArtifactCrossReview, run.Artifacts[1].Kind)
	assert.Equal(t, multiAgentArtifactVerdict, run.Artifacts[2].Kind)
	assert.Contains(t, run.Artifacts[0].Content, "recorded finding")
	assert.Contains(t, run.Artifacts[0].Content, "tests pass: PASS unit evidence")
	assert.Contains(t, run.Artifacts[2].Content, "tests pass: FAIL integration missing")
	assert.Equal(t, multiAgentDecisionRejected, run.Decisions[0].Outcome)
	assert.Equal(t, "aggregate verdict failed validation: gate check \"tests pass\" failed", run.Decisions[0].Rationale)
	assert.Equal(t, multiAgentDecisionRejected, run.Decisions[1].Outcome)
	assert.Equal(t, "aggregate verdict failed validation: gate check \"tests pass\" failed", run.Decisions[1].Rationale)
	assert.Equal(t, "tests", run.Artifacts[1].Agent)
	assert.Equal(t, "quality", run.Artifacts[1].TargetAgent)
	require.Len(t, run.Branches, 1)
	assert.Equal(t, "quality", run.Branches[0].Name)
	assert.Equal(t, multiAgentPhaseReviewReport, run.Branches[0].Role)
	assert.Equal(t, "artifact:review_report:1", run.Branches[0].Provenance)
	require.Len(t, run.Reviewers, 3)
	assert.Equal(t, "quality", run.Reviewers[0].Name)
	assert.Equal(t, multiAgentPhaseReviewReport, run.Reviewers[0].Role)
	assert.Equal(t, "tests", run.Reviewers[1].Name)
	assert.Equal(t, multiAgentPhaseCrossReview, run.Reviewers[1].Role)
	assert.Equal(t, "quality", run.Reviewers[1].TargetAgent)
	assert.Equal(t, "aggregate-verdict", run.Reviewers[2].Name)
	assert.Equal(t, multiAgentPhaseAggregateVerdict, run.Reviewers[2].Role)
	require.Len(t, run.Gates, 2)
	assert.True(t, run.Gates[0].Passed)
	assert.False(t, run.Gates[1].Passed)
	require.Len(t, run.Disagreements, 2)
	assert.Equal(t, "cross-review", run.Disagreements[0].Subject)
	assert.Equal(t, "gate:tests pass", run.Disagreements[1].Subject)
	assert.Equal(t, "pass=quality fail=aggregate-verdict", run.Disagreements[1].Notes)
	require.Len(t, run.Errors, 2)
	assert.Equal(t, string(review.RoundCrossReview), run.Errors[0].Stage)
	assert.Equal(t, "tests", run.Errors[0].Reviewer)
	assert.Equal(t, "quality", run.Errors[0].TargetAgent)
	assert.Equal(t, "cross-review parse failed", run.Errors[0].Message)
	assert.Equal(t, multiAgentPhaseAggregateVerdict, run.Errors[1].Stage)
	assert.Equal(t, "aggregate-verdict", run.Errors[1].Reviewer)
	assert.Equal(t, "aggregate verdict failed validation: gate check \"tests pass\" failed", run.Errors[1].Message)
	assert.Contains(t, formatMultiAgentRunSummary(run), "errors=2")
	assert.Contains(t, formatMultiAgentRunResume(run), "gates=2\terrors=2")

	replay := formatMultiAgentRunReplay(run)
	assert.Contains(t, replay, "aggregate_report:")
	assert.Contains(t, replay, "tests pass: FAIL integration missing")
	assert.Contains(t, replay, "workflow_errors:")
	assert.Contains(t, replay, "message=cross-review parse failed")
	assert.Contains(t, replay, "message=aggregate verdict failed validation: gate check \"tests pass\" failed")
}

func TestMultiAgentRunRecorderDoesNotDuplicateReviewAggregateErrors(t *testing.T) {
	t.Parallel()

	store := session.NewStore(t.TempDir())
	sessionState := session.New("gpt-test", nil)
	recorder := newMultiAgentRunRecorder(
		store,
		&sessionState,
		session.MultiAgentRunKindReview,
		"review aggregate error",
		"gpt-test",
		nil,
		session.MultiAgentRunBudget{},
		nil,
		nil,
	)
	require.NoError(t, recorder.start())

	plan, err := review.NewPlan(
		[]review.Reviewer{{Name: "quality"}},
		[]string{"pkg/example.go"},
		[]string{"tests pass"},
	)
	require.NoError(t, err)

	require.NoError(t, recorder.recordReviewSession(review.Session{
		Plan: plan,
		Verdict: review.Report{
			Reviewer:   string(review.RoundAggregateVerdict),
			GateChecks: []review.GateCheck{{Name: "tests pass", Passed: false}},
		},
		Errors: []review.RunError{{
			Stage:    string(review.RoundAggregateVerdict),
			Reviewer: string(review.RoundAggregateVerdict),
			Message:  `gate check "tests pass" failed`,
		}},
	}))

	loaded, err := store.Load(sessionState.ID)
	require.NoError(t, err)
	require.Len(t, loaded.MultiAgentRuns, 1)

	run := loaded.MultiAgentRuns[0]
	require.Len(t, run.Errors, 1)
	assert.Equal(t, multiAgentPhaseAggregateVerdict, run.Errors[0].Stage)
	assert.Equal(t, string(review.RoundAggregateVerdict), run.Errors[0].Reviewer)
	assert.Equal(t, `gate check "tests pass" failed`, run.Errors[0].Message)

	replay := formatMultiAgentRunReplay(run)
	assert.Equal(t, 1, strings.Count(replay, "workflow_errors:"))
	assert.Equal(t, 1, strings.Count(replay, `message=gate check "tests pass" failed`))
}

func multiAgentRunAggregateReviewerExists(
	reviewers []session.MultiAgentRunReviewer,
	name string,
) bool {
	for i := range reviewers {
		reviewer := reviewers[i]
		if reviewer.Name == name && reviewer.Role == multiAgentPhaseAggregateVerdict {
			return true
		}
	}

	return false
}

func requireMultiAgentRunCall(
	t *testing.T,
	run session.MultiAgentRun,
	phase string,
	agentName string,
	targetAgent string,
) session.MultiAgentRunCall {
	t.Helper()

	for i := range run.Calls {
		call := run.Calls[i]
		if call.Phase == phase && call.Agent == agentName && call.TargetAgent == targetAgent {
			return call
		}
	}

	require.Failf(
		t,
		"missing multi-agent run call",
		"phase=%q agent=%q target=%q calls=%+v",
		phase,
		agentName,
		targetAgent,
		run.Calls,
	)

	return session.MultiAgentRunCall{}
}

func assertMultiAgentRunArtifactsHaveProviderCallIDs(
	t *testing.T,
	artifacts []session.MultiAgentRunArtifact,
) {
	t.Helper()

	for i := range artifacts {
		artifact := artifacts[i]
		require.NotEmpty(t, artifact.Metadata["call_id"], "artifact %d should retain provider-call provenance", i+1)
		assert.NotEqual(t, multiAgentMetadataTrue, artifact.Metadata[multiAgentRawProviderResponseKey])
	}
}

func TestMultiAgentRunRecorderExplainsPartialReviewWithoutVerdict(t *testing.T) {
	t.Parallel()

	store := session.NewStore(t.TempDir())
	sessionState := session.New("gpt-test", nil)
	recorder := newMultiAgentRunRecorder(
		store,
		&sessionState,
		session.MultiAgentRunKindReview,
		"partial review context",
		"gpt-test",
		nil,
		session.MultiAgentRunBudget{},
		nil,
		nil,
	)
	require.NoError(t, recorder.start())

	require.NoError(t, recorder.recordReviewSession(review.Session{
		Reports: []review.Report{{
			Reviewer:   "quality",
			GateChecks: []review.GateCheck{{Name: "tests pass", Passed: true, Notes: "unit evidence"}},
		}},
	}))

	loaded, err := store.Load(sessionState.ID)
	require.NoError(t, err)
	require.Len(t, loaded.MultiAgentRuns, 1)

	run := loaded.MultiAgentRuns[0]
	require.Len(t, run.Decisions, 1)
	assert.Equal(t, multiAgentArtifactReviewReport, run.Decisions[0].Kind)
	assert.Equal(t, multiAgentDecisionRejected, run.Decisions[0].Outcome)
	assert.Equal(t, "run stopped before aggregate verdict", run.Decisions[0].Rationale)
	require.Len(t, run.Artifacts, 1)
	assert.Equal(t, multiAgentArtifactReviewReport, run.Artifacts[0].Kind)
	require.Len(t, run.Branches, 1)
	assert.Equal(t, "quality", run.Branches[0].Name)
	assert.Equal(t, multiAgentPhaseReviewReport, run.Branches[0].Role)
	assert.Equal(t, "artifact:review_report:1", run.Branches[0].Provenance)
	require.Len(t, run.Reviewers, 1)
	assert.Equal(t, "quality", run.Reviewers[0].Name)
	assert.Equal(t, multiAgentPhaseReviewReport, run.Reviewers[0].Role)
}

func TestMultiAgentRunRecorderExplainsPartialSpeculationWithoutVerdict(t *testing.T) {
	t.Parallel()

	store := session.NewStore(t.TempDir())
	sessionState := session.New("gpt-test", nil)
	recorder := newMultiAgentRunRecorder(
		store,
		&sessionState,
		session.MultiAgentRunKindSpeculation,
		"partial speculation prompt",
		"gpt-test",
		nil,
		session.MultiAgentRunBudget{},
		nil,
		nil,
	)
	require.NoError(t, recorder.start())

	plan, err := speculate.NewPlan([]string{"planner"}, []string{"tests pass"})
	require.NoError(t, err)

	require.NoError(t, recorder.recordSpeculateSession(speculate.Session{
		Plan: plan,
		Proposals: []speculate.Proposal{{
			Agent:   "planner",
			Round:   speculate.RoundProposal,
			Content: "partial plan",
		}},
	}))

	loaded, err := store.Load(sessionState.ID)
	require.NoError(t, err)
	require.Len(t, loaded.MultiAgentRuns, 1)

	run := loaded.MultiAgentRuns[0]
	require.Len(t, run.Decisions, 1)
	assert.Equal(t, multiAgentArtifactProposal, run.Decisions[0].Kind)
	assert.Equal(t, multiAgentDecisionRejected, run.Decisions[0].Outcome)
	assert.Equal(t, "run stopped before aggregate verdict", run.Decisions[0].Rationale)
	require.Len(t, run.Artifacts, 1)
	assert.Equal(t, multiAgentArtifactProposal, run.Artifacts[0].Kind)
	require.Len(t, run.Branches, 1)
	assert.Equal(t, "planner", run.Branches[0].Name)
	assert.Equal(t, multiAgentPhaseProposal, run.Branches[0].Role)
	assert.Equal(t, "artifact:proposal:1", run.Branches[0].Provenance)
}

func TestMultiAgentRunRecorderExplainsSpeculationVerdictWithoutWinner(t *testing.T) {
	t.Parallel()

	store := session.NewStore(t.TempDir())
	sessionState := session.New("gpt-test", nil)
	recorder := newMultiAgentRunRecorder(
		store,
		&sessionState,
		session.MultiAgentRunKindSpeculation,
		"malformed speculation prompt",
		"gpt-test",
		nil,
		session.MultiAgentRunBudget{},
		nil,
		nil,
	)
	require.NoError(t, recorder.start())

	plan, err := speculate.NewPlan([]string{"planner"}, []string{"tests pass"})
	require.NoError(t, err)

	require.NoError(t, recorder.recordSpeculateSession(speculate.Session{
		Plan: plan,
		Proposals: []speculate.Proposal{{
			Agent:   "planner",
			Round:   speculate.RoundProposal,
			Content: "candidate plan",
		}},
		Verdict: speculate.Verdict{
			GateChecks: []speculate.GateCheck{{Name: "tests pass", Passed: true, Notes: "recorded"}},
		},
	}))

	loaded, err := store.Load(sessionState.ID)
	require.NoError(t, err)
	require.Len(t, loaded.MultiAgentRuns, 1)

	run := loaded.MultiAgentRuns[0]
	require.Len(t, run.Decisions, 2)
	assert.Equal(t, multiAgentDecisionRejected, run.Decisions[0].Outcome)
	assert.Equal(t, "aggregate verdict failed validation: verdict winner is required", run.Decisions[0].Rationale)
	assert.Equal(t, multiAgentArtifactVerdict, run.Decisions[1].Kind)
	assert.Equal(t, multiAgentDecisionRejected, run.Decisions[1].Outcome)
	assert.Equal(t, "aggregate verdict failed validation: verdict winner is required", run.Decisions[1].Rationale)
	require.Len(t, run.Artifacts, 2)
	assert.Equal(t, multiAgentArtifactVerdict, run.Artifacts[1].Kind)
	assert.Contains(t, run.Artifacts[1].Content, "gate tests pass: PASS recorded")
	require.Len(t, run.Gates, 1)
	assert.True(t, run.Gates[0].Passed)
}

func TestMultiAgentRunRecorderRejectsRawAggregateVerdictWithoutStructuredVerdict(t *testing.T) {
	t.Parallel()

	store := session.NewStore(t.TempDir())
	sessionState := session.New("gpt-test", nil)
	recorder := newMultiAgentRunRecorder(
		store,
		&sessionState,
		session.MultiAgentRunKindSpeculation,
		"malformed aggregate prompt",
		"gpt-test",
		nil,
		session.MultiAgentRunBudget{},
		nil,
		nil,
	)
	require.NoError(t, recorder.start())

	_, err := recorder.complete(t.Context(), multiAgentCallInfo{Phase: multiAgentPhaseAggregateVerdict, Agent: "judge"}, llm.CompleteParams{
		Model:    "gpt-test",
		Messages: []llm.Message{{Role: llm.RoleUser, Content: "aggregate"}},
	}, nil, func(context.Context, llm.CompleteParams, []string) (*llm.Response, error) {
		return &llm.Response{Content: "not a parseable verdict", Model: "gpt-test", InputTokens: 4, OutputTokens: 3}, nil
	})
	require.NoError(t, err)

	require.NoError(t, recorder.recordSpeculateSession(speculate.Session{}))

	loaded, err := store.Load(sessionState.ID)
	require.NoError(t, err)
	require.Len(t, loaded.MultiAgentRuns, 1)

	run := loaded.MultiAgentRuns[0]
	require.Len(t, run.Artifacts, 1)
	assert.Equal(t, multiAgentArtifactVerdict, run.Artifacts[0].Kind)
	assert.Equal(t, "call-001", run.Artifacts[0].Metadata["call_id"])
	require.Len(t, run.Decisions, 1)
	assert.Equal(t, multiAgentArtifactVerdict, run.Decisions[0].Kind)
	assert.Equal(t, multiAgentDecisionRejected, run.Decisions[0].Outcome)
	assert.Equal(t, multiAgentRationaleNoAcceptedVerdict, run.Decisions[0].Rationale)
}

func TestMultiAgentRunRecorderRejectsSpeculationWinnerWhenGateFails(t *testing.T) {
	t.Parallel()

	store := session.NewStore(t.TempDir())
	sessionState := session.New("gpt-test", nil)
	recorder := newMultiAgentRunRecorder(
		store,
		&sessionState,
		session.MultiAgentRunKindSpeculation,
		"gated speculation prompt",
		"gpt-test",
		nil,
		session.MultiAgentRunBudget{},
		nil,
		nil,
	)
	require.NoError(t, recorder.start())

	plan, err := speculate.NewPlan([]string{"planner", "coder"}, []string{"tests pass"})
	require.NoError(t, err)

	require.NoError(t, recorder.recordSpeculateSession(speculate.Session{
		Plan: plan,
		Proposals: []speculate.Proposal{
			{Agent: "planner", Round: speculate.RoundProposal, Content: "candidate plan"},
			{Agent: "coder", Round: speculate.RoundProposal, Content: "fallback plan"},
		},
		Verdict: speculate.Verdict{
			Winner: "planner",
			Reason: "best evidence",
			GateChecks: []speculate.GateCheck{{
				Name:   "tests pass",
				Passed: false,
				Notes:  "tests were not run",
			}},
		},
	}))

	loaded, err := store.Load(sessionState.ID)
	require.NoError(t, err)
	require.Len(t, loaded.MultiAgentRuns, 1)

	run := loaded.MultiAgentRuns[0]
	require.Len(t, run.Decisions, 3)
	assert.Equal(t, multiAgentDecisionRejected, run.Decisions[0].Outcome)
	assert.Equal(t, "aggregate verdict failed validation: gate check \"tests pass\" failed: tests were not run", run.Decisions[0].Rationale)
	assert.Equal(t, multiAgentDecisionRejected, run.Decisions[1].Outcome)
	assert.Equal(t, "aggregate verdict failed validation: gate check \"tests pass\" failed: tests were not run", run.Decisions[1].Rationale)
	assert.Equal(t, multiAgentArtifactVerdict, run.Decisions[2].Kind)
	assert.Equal(t, multiAgentDecisionRejected, run.Decisions[2].Outcome)
	assert.Equal(t, "aggregate verdict failed validation: gate check \"tests pass\" failed: tests were not run", run.Decisions[2].Rationale)
	assert.Empty(t, run.Summary.Winner)
	assert.Empty(t, run.Summary.Reason)
	require.Len(t, run.Errors, 1)
	assert.Equal(t, multiAgentPhaseAggregateVerdict, run.Errors[0].Stage)
	assert.Equal(t, "judge", run.Errors[0].Reviewer)
	assert.Equal(t, "planner", run.Errors[0].TargetAgent)
	assert.Equal(t, "aggregate verdict failed validation: gate check \"tests pass\" failed: tests were not run", run.Errors[0].Message)
	replay := formatMultiAgentRunReplay(run)
	assert.Contains(t, replay, "aggregate_verdict:")
	assert.Contains(t, replay, "winner: planner")
	assert.Contains(t, replay, "workflow_errors:")
	assert.Contains(t, replay, "message=aggregate verdict failed validation: gate check \"tests pass\" failed: tests were not run")
}

func TestMultiAgentRunRecorderRejectsSpeculationWinnerOutsideCandidateBranches(t *testing.T) {
	t.Parallel()

	store := session.NewStore(t.TempDir())
	sessionState := session.New("gpt-test", nil)
	recorder := newMultiAgentRunRecorder(
		store,
		&sessionState,
		session.MultiAgentRunKindSpeculation,
		"candidate-bound speculation prompt",
		"gpt-test",
		nil,
		session.MultiAgentRunBudget{},
		nil,
		nil,
	)
	require.NoError(t, recorder.start())

	plan, err := speculate.NewPlan([]string{"planner", "coder"}, []string{"tests pass"})
	require.NoError(t, err)

	require.NoError(t, recorder.recordSpeculateSession(speculate.Session{
		Plan: plan,
		Proposals: []speculate.Proposal{
			{Agent: "planner", Round: speculate.RoundProposal, Content: "candidate plan"},
			{Agent: "coder", Round: speculate.RoundProposal, Content: "fallback plan"},
		},
		Verdict: speculate.Verdict{
			Winner: "ghost",
			Reason: "not actually a branch",
			GateChecks: []speculate.GateCheck{{
				Name:   "tests pass",
				Passed: true,
				Notes:  "claimed pass",
			}},
		},
	}))

	loaded, err := store.Load(sessionState.ID)
	require.NoError(t, err)
	require.Len(t, loaded.MultiAgentRuns, 1)

	run := loaded.MultiAgentRuns[0]
	require.Len(t, run.Decisions, 3)

	for _, decision := range run.Decisions {
		assert.Equal(t, multiAgentDecisionRejected, decision.Outcome)
		assert.Contains(t, decision.Rationale, `winner "ghost" is not a recorded candidate branch`)
	}

	assert.Empty(t, run.Summary.Winner)
	assert.Empty(t, run.Summary.Reason)
	require.Len(t, run.Gates, 1)
	assert.True(t, run.Gates[0].Passed)

	replay := formatMultiAgentRunReplay(run)
	assert.Contains(t, replay, "aggregate_verdict:")
	assert.Contains(t, replay, "winner: ghost")
	assert.Contains(t, replay, `winner "ghost" is not a recorded candidate branch`)
}

func TestMultiAgentRunRecorderRejectsSpeculationWinnerWithoutRecordedProposal(t *testing.T) {
	t.Parallel()

	store := session.NewStore(t.TempDir())
	sessionState := session.New("gpt-test", nil)
	recorder := newMultiAgentRunRecorder(
		store,
		&sessionState,
		session.MultiAgentRunKindSpeculation,
		"missing candidate branch prompt",
		"gpt-test",
		nil,
		session.MultiAgentRunBudget{},
		nil,
		nil,
	)
	require.NoError(t, recorder.start())

	plan, err := speculate.NewPlan([]string{"planner", "coder"}, []string{"tests pass"})
	require.NoError(t, err)

	require.NoError(t, recorder.recordSpeculateSession(speculate.Session{
		Plan: plan,
		Verdict: speculate.Verdict{
			Winner: "planner",
			Reason: "selected without a proposal artifact",
			GateChecks: []speculate.GateCheck{{
				Name:   "tests pass",
				Passed: true,
				Notes:  "claimed pass",
			}},
		},
	}))

	loaded, err := store.Load(sessionState.ID)
	require.NoError(t, err)
	require.Len(t, loaded.MultiAgentRuns, 1)

	run := loaded.MultiAgentRuns[0]
	require.Len(t, run.Decisions, 1)
	assert.Equal(t, multiAgentArtifactVerdict, run.Decisions[0].Kind)
	assert.Equal(t, multiAgentDecisionRejected, run.Decisions[0].Outcome)
	assert.Contains(t, run.Decisions[0].Rationale, `winner "planner" has no recorded candidate branch`)
	assert.Empty(t, run.Summary.Winner)
	assert.Empty(t, run.Branches)
	require.Len(t, run.Gates, 1)
	assert.True(t, run.Gates[0].Passed)

	replay := formatMultiAgentRunReplay(run)
	assert.Contains(t, replay, "aggregate_verdict:")
	assert.Contains(t, replay, "winner: planner")
	assert.Contains(t, replay, `winner "planner" has no recorded candidate branch`)
}

func TestMultiAgentRunRecorderClearsAcceptedSummaryWhenRunDoesNotComplete(t *testing.T) {
	t.Parallel()

	store := session.NewStore(t.TempDir())
	sessionState := session.New("gpt-test", nil)
	recorder := newMultiAgentRunRecorder(
		store,
		&sessionState,
		session.MultiAgentRunKindSpeculation,
		"cancel after aggregate",
		"gpt-test",
		nil,
		session.MultiAgentRunBudget{},
		nil,
		nil,
	)
	require.NoError(t, recorder.start())

	plan, err := speculate.NewPlan([]string{"planner", "coder"}, []string{"tests pass"})
	require.NoError(t, err)

	require.NoError(t, recorder.recordSpeculateSession(speculate.Session{
		Plan: plan,
		Proposals: []speculate.Proposal{
			{Agent: "planner", Round: speculate.RoundProposal, Content: "candidate plan"},
			{Agent: "coder", Round: speculate.RoundProposal, Content: "fallback plan"},
		},
		Verdict: speculate.Verdict{
			Winner: "planner",
			Reason: "best evidence",
			GateChecks: []speculate.GateCheck{{
				Name:   "tests pass",
				Passed: true,
				Notes:  "recorded",
			}},
		},
	}))

	loaded, err := store.Load(sessionState.ID)
	require.NoError(t, err)
	require.Len(t, loaded.MultiAgentRuns, 1)

	run := loaded.MultiAgentRuns[0]
	assert.Equal(t, session.MultiAgentRunStatusRunning, run.Status)
	assert.Empty(t, run.Summary.Winner)
	assert.Empty(t, run.Summary.Reason)
	require.Len(t, run.Decisions, 3)
	assert.Equal(t, multiAgentArtifactVerdict, run.Decisions[2].Kind)
	assert.Equal(t, multiAgentDecisionAccepted, run.Decisions[2].Outcome)
	runningResume := formatMultiAgentRunResume(run)
	assert.Contains(t, runningResume, "current_state: running")
	assert.Contains(t, runningResume, "next_action: finalize accepted aggregate output from recorded receipt without provider calls")
	assert.Contains(t, runningResume, "resumable_artifacts:")
	assert.NotContains(t, runningResume, "resumed_output:")

	require.NoError(t, recorder.finish(context.Canceled))

	loaded, err = store.Load(sessionState.ID)
	require.NoError(t, err)
	require.Len(t, loaded.MultiAgentRuns, 1)

	run = loaded.MultiAgentRuns[0]
	assert.Equal(t, session.MultiAgentRunStatusCanceled, run.Status)
	assert.Empty(t, run.Summary.Winner)
	assert.Empty(t, run.Summary.Reason)

	replay := formatMultiAgentRunReplay(run)
	assert.NotContains(t, replay, "\nwinner: planner\n")
	assert.Contains(t, replay, "aggregate_verdict:")
	assert.Contains(t, replay, "content=winner: planner")
	assert.Contains(t, replay, "cancellation_reason:")

	resume := formatMultiAgentRunResume(run)
	assert.Contains(t, resume, "terminal_state: "+string(session.MultiAgentRunStatusCanceled))
	assert.Contains(t, resume, "next_action: inspect cancellation and aggregate recorded partial output before accepting final output")
	assert.Contains(t, resume, "resumable_artifacts:")
	assert.Contains(t, resume, "kind=verdict\tphase=aggregate-verdict\tagent=planner\tindex=1\tcontent=winner: planner")
	assert.NotContains(t, resume, "resumed_output:")
}

func TestMultiAgentRunRecorderEnforcesBudgetWithoutProviderCall(t *testing.T) {
	t.Parallel()

	store := session.NewStore(t.TempDir())
	sessionState := session.New("gpt-test", nil)
	recorder := newMultiAgentRunRecorder(
		store,
		&sessionState,
		session.MultiAgentRunKindSpeculation,
		"large prompt",
		"gpt-test",
		nil,
		session.MultiAgentRunBudget{PerCallMaxInputTokens: 1},
		nil,
		nil,
	)
	require.NoError(t, recorder.start())

	called := false
	_, err := recorder.complete(t.Context(), multiAgentCallInfo{Phase: multiAgentPhaseProposal, Agent: "planner"}, llm.CompleteParams{
		Model:    "gpt-test",
		Messages: []llm.Message{{Role: llm.RoleUser, Content: strings.Repeat("x", 32)}},
	}, nil, func(context.Context, llm.CompleteParams, []string) (*llm.Response, error) {
		called = true
		return &llm.Response{Content: "should not happen"}, nil
	})
	require.Error(t, err)
	assert.Contains(t, err.Error(), multiAgentBudgetRuleInput)
	assert.False(t, called)

	loaded, err := store.Load(sessionState.ID)
	require.NoError(t, err)
	require.Len(t, loaded.MultiAgentRuns, 1)
	require.Len(t, loaded.MultiAgentRuns[0].Calls, 1)
	require.Len(t, loaded.MultiAgentRuns[0].Branches, 1)
	assert.Equal(t, session.MultiAgentRunStatusBudgetExhausted, loaded.MultiAgentRuns[0].Calls[0].Status)
	assert.Equal(t, multiAgentBudgetRuleInput, loaded.MultiAgentRuns[0].Calls[0].BudgetRejectionRule)
	assert.Greater(t, loaded.MultiAgentRuns[0].Calls[0].BudgetRejectionUsage, 1)
	assert.Equal(t, multiAgentBudgetRuleInput, loaded.MultiAgentRuns[0].Branches[0].BudgetRejectionRule)
	assert.Equal(t, loaded.MultiAgentRuns[0].Calls[0].BudgetRejectionUsage, loaded.MultiAgentRuns[0].Branches[0].BudgetRejectionUsage)
	assert.Equal(t, loaded.MultiAgentRuns[0].Calls[0].BudgetRejectionLimit, loaded.MultiAgentRuns[0].Branches[0].BudgetRejectionLimit)
	assert.Equal(t, 1, loaded.MultiAgentRuns[0].Usage.BudgetRejectedCalls)
}

func TestMultiAgentRunRecorderEnforcesContextWindowWithRequestedOutput(t *testing.T) {
	t.Parallel()

	store := session.NewStore(t.TempDir())
	sessionState := session.New("gpt-test", nil)
	recorder := newMultiAgentRunRecorder(
		store,
		&sessionState,
		session.MultiAgentRunKindSpeculation,
		"respect model context",
		"gpt-test",
		nil,
		session.MultiAgentRunBudget{},
		func(string) int { return 10 },
		nil,
	)
	require.NoError(t, recorder.start())

	called := false
	_, err := recorder.complete(t.Context(), multiAgentCallInfo{Phase: multiAgentPhaseProposal, Agent: "planner"}, llm.CompleteParams{
		Model:     "gpt-test",
		MaxTokens: 10,
		Messages:  []llm.Message{{Role: llm.RoleUser, Content: "abcd"}},
	}, nil, func(context.Context, llm.CompleteParams, []string) (*llm.Response, error) {
		called = true
		return &llm.Response{Content: "should not happen"}, nil
	})
	require.Error(t, err)
	assert.Contains(t, err.Error(), multiAgentBudgetRuleContextLimit)
	assert.Contains(t, err.Error(), "used 23 of 10")
	assert.False(t, called)

	loaded, err := store.Load(sessionState.ID)
	require.NoError(t, err)
	require.Len(t, loaded.MultiAgentRuns, 1)
	require.Len(t, loaded.MultiAgentRuns[0].Calls, 1)

	call := loaded.MultiAgentRuns[0].Calls[0]
	assert.Equal(t, session.MultiAgentRunStatusBudgetExhausted, call.Status)
	assert.Equal(t, multiAgentBudgetRuleContextLimit, call.BudgetRejectionRule)
	assert.Equal(t, 23, call.BudgetRejectionUsage)
	assert.Equal(t, 10, call.BudgetRejectionLimit)
	assert.Equal(t, 10, call.ContextWindow)
	assert.Equal(t, 10, call.MaxOutputTokens)
	assert.Equal(t, 1, loaded.MultiAgentRuns[0].Usage.BudgetRejectedCalls)
}

func TestMultiAgentRunRecorderEnforcesSmallestFallbackContextWindow(t *testing.T) {
	t.Parallel()

	store := session.NewStore(t.TempDir())
	sessionState := session.New("gpt-test", nil)
	recorder := newMultiAgentRunRecorder(
		store,
		&sessionState,
		session.MultiAgentRunKindSpeculation,
		"respect fallback context",
		"gpt-large",
		[]string{"gpt-small"},
		session.MultiAgentRunBudget{},
		func(model string) int {
			switch model {
			case "gpt-large":
				return 100
			case "gpt-small":
				return 8
			default:
				return 0
			}
		},
		nil,
	)
	require.NoError(t, recorder.start())

	called := false
	_, err := recorder.complete(t.Context(), multiAgentCallInfo{Phase: multiAgentPhaseProposal, Agent: "planner"}, llm.CompleteParams{
		Model:     "gpt-large",
		MaxTokens: 8,
		Messages:  []llm.Message{{Role: llm.RoleUser, Content: "abcd"}},
	}, []string{"gpt-small"}, func(context.Context, llm.CompleteParams, []string) (*llm.Response, error) {
		called = true
		return &llm.Response{Content: "should not happen"}, nil
	})
	require.Error(t, err)
	assert.Contains(t, err.Error(), multiAgentBudgetRuleContextLimit)
	assert.Contains(t, err.Error(), "used 21 of 8")
	assert.False(t, called)

	loaded, err := store.Load(sessionState.ID)
	require.NoError(t, err)
	require.Len(t, loaded.MultiAgentRuns, 1)
	require.Len(t, loaded.MultiAgentRuns[0].Calls, 1)

	call := loaded.MultiAgentRuns[0].Calls[0]
	assert.Equal(t, session.MultiAgentRunStatusBudgetExhausted, call.Status)
	assert.Equal(t, multiAgentBudgetRuleContextLimit, call.BudgetRejectionRule)
	assert.Equal(t, 8, call.ContextWindow)
	assert.Equal(t, []string{"gpt-small"}, call.FallbackModels)
}

func TestMultiAgentRunRecorderCapsUnboundedCallToContextWindow(t *testing.T) {
	t.Parallel()

	store := session.NewStore(t.TempDir())
	sessionState := session.New("gpt-test", nil)
	recorder := newMultiAgentRunRecorder(
		store,
		&sessionState,
		session.MultiAgentRunKindSpeculation,
		"cap default provider context",
		"gpt-test",
		nil,
		session.MultiAgentRunBudget{},
		func(string) int { return 25 },
		nil,
	)
	require.NoError(t, recorder.start())

	_, err := recorder.complete(t.Context(), multiAgentCallInfo{Phase: multiAgentPhaseProposal, Agent: "planner"}, llm.CompleteParams{
		Model:    "gpt-test",
		Messages: []llm.Message{{Role: llm.RoleUser, Content: "abcd"}},
	}, nil, func(_ context.Context, params llm.CompleteParams, _ []string) (*llm.Response, error) {
		assert.Equal(t, 12, params.MaxTokens)

		return &llm.Response{Content: "", Model: "gpt-test", InputTokens: 1, OutputTokens: 2}, nil
	})
	require.NoError(t, err)

	loaded, err := store.Load(sessionState.ID)
	require.NoError(t, err)
	require.Len(t, loaded.MultiAgentRuns, 1)
	require.Len(t, loaded.MultiAgentRuns[0].Calls, 1)

	call := loaded.MultiAgentRuns[0].Calls[0]
	assert.Equal(t, 25, call.ContextWindow)
	assert.Equal(t, 12, call.MaxOutputTokens)
	assert.Equal(t, session.MultiAgentRunStatusCompleted, call.Status)
}

func TestMultiAgentRunRecorderRejectsUnboundedCallWhenContextWindowFull(t *testing.T) {
	t.Parallel()

	store := session.NewStore(t.TempDir())
	sessionState := session.New("gpt-test", nil)
	recorder := newMultiAgentRunRecorder(
		store,
		&sessionState,
		session.MultiAgentRunKindSpeculation,
		"reject full context",
		"gpt-test",
		nil,
		session.MultiAgentRunBudget{},
		func(string) int { return 1 },
		nil,
	)
	require.NoError(t, recorder.start())

	called := false
	_, err := recorder.complete(t.Context(), multiAgentCallInfo{Phase: multiAgentPhaseProposal, Agent: "planner"}, llm.CompleteParams{
		Model:    "gpt-test",
		Messages: []llm.Message{{Role: llm.RoleUser, Content: "abcd"}},
	}, nil, func(context.Context, llm.CompleteParams, []string) (*llm.Response, error) {
		called = true
		return &llm.Response{Content: "should not happen"}, nil
	})
	require.Error(t, err)
	assert.Contains(t, err.Error(), multiAgentBudgetRuleContextLimit)
	assert.Contains(t, err.Error(), "used 14 of 1")
	assert.False(t, called)

	loaded, err := store.Load(sessionState.ID)
	require.NoError(t, err)
	require.Len(t, loaded.MultiAgentRuns, 1)
	require.Len(t, loaded.MultiAgentRuns[0].Calls, 1)

	call := loaded.MultiAgentRuns[0].Calls[0]
	assert.Equal(t, session.MultiAgentRunStatusBudgetExhausted, call.Status)
	assert.Equal(t, multiAgentBudgetRuleContextLimit, call.BudgetRejectionRule)
	assert.Equal(t, 14, call.BudgetRejectionUsage)
	assert.Equal(t, 1, call.BudgetRejectionLimit)
	assert.Equal(t, 0, call.MaxOutputTokens)
}

func TestMultiAgentRunRecorderEnforcesPostflightContextWindow(t *testing.T) {
	t.Parallel()

	store := session.NewStore(t.TempDir())
	sessionState := session.New("gpt-test", nil)
	recorder := newMultiAgentRunRecorder(
		store,
		&sessionState,
		session.MultiAgentRunKindSpeculation,
		"actual provider usage can exceed estimates",
		"gpt-test",
		nil,
		session.MultiAgentRunBudget{},
		func(string) int { return 17 },
		nil,
	)
	require.NoError(t, recorder.start())

	called := false
	_, err := recorder.complete(t.Context(), multiAgentCallInfo{Phase: multiAgentPhaseProposal, Agent: "planner"}, llm.CompleteParams{
		Model:    "gpt-test",
		Messages: []llm.Message{{Role: llm.RoleUser, Content: "abcd"}},
	}, nil, func(_ context.Context, params llm.CompleteParams, _ []string) (*llm.Response, error) {
		called = true

		assert.Equal(t, 4, params.MaxTokens)

		return &llm.Response{Content: "", Model: "gpt-test", InputTokens: 4, OutputTokens: 5}, nil
	})
	require.Error(t, err)
	assert.Contains(t, err.Error(), multiAgentBudgetRuleContextLimit)
	assert.True(t, called)

	loaded, err := store.Load(sessionState.ID)
	require.NoError(t, err)
	require.Len(t, loaded.MultiAgentRuns, 1)
	require.Len(t, loaded.MultiAgentRuns[0].Calls, 1)

	call := loaded.MultiAgentRuns[0].Calls[0]
	assert.Equal(t, session.MultiAgentRunStatusBudgetExhausted, call.Status)
	assert.Equal(t, multiAgentBudgetRuleContextLimit, call.BudgetRejectionRule)
	assert.Equal(t, 25, call.BudgetRejectionUsage)
	assert.Equal(t, 17, call.BudgetRejectionLimit)
	assert.Equal(t, 4, call.InputTokens)
	assert.Equal(t, 5, call.OutputTokens)
	assert.Equal(t, 1, loaded.MultiAgentRuns[0].Usage.BudgetRejectedCalls)
	assert.Equal(t, 0, loaded.MultiAgentRuns[0].Usage.CompletedCalls)
}

func TestMultiAgentRunRecorderEnforcesRunModelCallBudget(t *testing.T) {
	t.Parallel()

	store := session.NewStore(t.TempDir())
	sessionState := session.New("gpt-test", nil)
	recorder := newMultiAgentRunRecorder(
		store,
		&sessionState,
		session.MultiAgentRunKindReview,
		"limit fan-out",
		"gpt-test",
		nil,
		session.MultiAgentRunBudget{MaxModelCalls: 1},
		nil,
		nil,
	)
	require.NoError(t, recorder.start())

	params := llm.CompleteParams{
		Model:    "gpt-test",
		Messages: []llm.Message{{Role: llm.RoleUser, Content: "review"}},
	}

	_, err := recorder.complete(t.Context(), multiAgentCallInfo{Phase: multiAgentPhaseReviewReport, Agent: "quality"}, params, nil,
		func(context.Context, llm.CompleteParams, []string) (*llm.Response, error) {
			return &llm.Response{Content: "ok", Model: "gpt-test", InputTokens: 1, OutputTokens: 1}, nil
		})
	require.NoError(t, err)

	called := false
	_, err = recorder.complete(t.Context(), multiAgentCallInfo{Phase: multiAgentPhaseReviewReport, Agent: "tests"}, params, nil,
		func(context.Context, llm.CompleteParams, []string) (*llm.Response, error) {
			called = true
			return &llm.Response{Content: "should not happen"}, nil
		})
	require.Error(t, err)
	assert.Contains(t, err.Error(), multiAgentBudgetRuleModelCalls)
	assert.Contains(t, err.Error(), "used 2 of 1")
	assert.False(t, called)

	loaded, err := store.Load(sessionState.ID)
	require.NoError(t, err)
	require.Len(t, loaded.MultiAgentRuns, 1)
	require.Len(t, loaded.MultiAgentRuns[0].Calls, 2)

	run := loaded.MultiAgentRuns[0]
	assert.Equal(t, 1, run.Usage.ModelCalls)
	assert.Equal(t, 1, run.Usage.CompletedCalls)
	assert.Equal(t, 1, run.Usage.BudgetRejectedCalls)
	assert.Equal(t, session.MultiAgentRunStatusBudgetExhausted, run.Calls[1].Status)
	assert.Equal(t, multiAgentBudgetRuleModelCalls, run.Calls[1].BudgetRejectionRule)
	assert.Equal(t, 2, run.Calls[1].BudgetRejectionUsage)
	assert.Equal(t, 1, run.Calls[1].BudgetRejectionLimit)
}

func TestMultiAgentRunRecorderEnforcesRunOutputBudgetBeforeProviderCall(t *testing.T) {
	t.Parallel()

	store := session.NewStore(t.TempDir())
	sessionState := session.New("gpt-test", nil)
	recorder := newMultiAgentRunRecorder(
		store,
		&sessionState,
		session.MultiAgentRunKindSpeculation,
		"limit branch output",
		"gpt-test",
		nil,
		session.MultiAgentRunBudget{MaxRunOutputTokens: 5},
		nil,
		nil,
	)
	require.NoError(t, recorder.start())

	called := false
	_, err := recorder.complete(t.Context(), multiAgentCallInfo{Phase: multiAgentPhaseProposal, Agent: "planner"}, llm.CompleteParams{
		Model:     "gpt-test",
		MaxTokens: 6,
		Messages:  []llm.Message{{Role: llm.RoleUser, Content: "review"}},
	}, nil, func(context.Context, llm.CompleteParams, []string) (*llm.Response, error) {
		called = true
		return &llm.Response{Content: "should not happen"}, nil
	})
	require.Error(t, err)
	assert.Contains(t, err.Error(), multiAgentBudgetRuleRunOutput)
	assert.False(t, called)

	loaded, err := store.Load(sessionState.ID)
	require.NoError(t, err)
	require.Len(t, loaded.MultiAgentRuns, 1)
	require.Len(t, loaded.MultiAgentRuns[0].Calls, 1)

	call := loaded.MultiAgentRuns[0].Calls[0]
	assert.Equal(t, session.MultiAgentRunStatusBudgetExhausted, call.Status)
	assert.Equal(t, multiAgentBudgetRuleRunOutput, call.BudgetRejectionRule)
	assert.Equal(t, 6, call.BudgetRejectionUsage)
	assert.Equal(t, 5, call.BudgetRejectionLimit)
	assert.Equal(t, 0, loaded.MultiAgentRuns[0].Usage.ModelCalls)
	assert.Equal(t, 1, loaded.MultiAgentRuns[0].Usage.BudgetRejectedCalls)
}

func TestMultiAgentRunRecorderEnforcesPerCallOutputBudgetBeforeProviderCall(t *testing.T) {
	t.Parallel()

	store := session.NewStore(t.TempDir())
	sessionState := session.New("gpt-test", nil)
	recorder := newMultiAgentRunRecorder(
		store,
		&sessionState,
		session.MultiAgentRunKindSpeculation,
		"limit branch output",
		"gpt-test",
		nil,
		session.MultiAgentRunBudget{PerCallMaxOutputTokens: 3},
		nil,
		nil,
	)
	require.NoError(t, recorder.start())

	called := false
	_, err := recorder.complete(t.Context(), multiAgentCallInfo{Phase: multiAgentPhaseProposal, Agent: "planner"}, llm.CompleteParams{
		Model:     "gpt-test",
		MaxTokens: 4,
		Messages:  []llm.Message{{Role: llm.RoleUser, Content: "review"}},
	}, nil, func(context.Context, llm.CompleteParams, []string) (*llm.Response, error) {
		called = true
		return &llm.Response{Content: "should not happen"}, nil
	})
	require.Error(t, err)
	assert.Contains(t, err.Error(), multiAgentBudgetRuleOutput)
	assert.False(t, called)

	loaded, err := store.Load(sessionState.ID)
	require.NoError(t, err)
	require.Len(t, loaded.MultiAgentRuns, 1)
	require.Len(t, loaded.MultiAgentRuns[0].Calls, 1)

	call := loaded.MultiAgentRuns[0].Calls[0]
	assert.Equal(t, session.MultiAgentRunStatusBudgetExhausted, call.Status)
	assert.Equal(t, multiAgentBudgetRuleOutput, call.BudgetRejectionRule)
	assert.Equal(t, 4, call.BudgetRejectionUsage)
	assert.Equal(t, 3, call.BudgetRejectionLimit)
}

func TestMultiAgentRunRecorderCapsUnboundedCallToPerCallOutputBudget(t *testing.T) {
	t.Parallel()

	store := session.NewStore(t.TempDir())
	sessionState := session.New("gpt-test", nil)
	recorder := newMultiAgentRunRecorder(
		store,
		&sessionState,
		session.MultiAgentRunKindSpeculation,
		"cap branch output",
		"gpt-test",
		nil,
		session.MultiAgentRunBudget{PerCallMaxOutputTokens: 12},
		nil,
		nil,
	)
	require.NoError(t, recorder.start())

	_, err := recorder.complete(t.Context(), multiAgentCallInfo{Phase: multiAgentPhaseProposal, Agent: "planner"}, llm.CompleteParams{
		Model:    "gpt-test",
		Messages: []llm.Message{{Role: llm.RoleUser, Content: "review"}},
	}, nil, func(_ context.Context, params llm.CompleteParams, _ []string) (*llm.Response, error) {
		assert.Equal(t, 12, params.MaxTokens)

		return &llm.Response{Content: "", Model: "gpt-test", InputTokens: 2, OutputTokens: 2}, nil
	})
	require.NoError(t, err)

	loaded, err := store.Load(sessionState.ID)
	require.NoError(t, err)
	require.Len(t, loaded.MultiAgentRuns, 1)
	require.Len(t, loaded.MultiAgentRuns[0].Calls, 1)

	call := loaded.MultiAgentRuns[0].Calls[0]
	assert.Equal(t, 12, loaded.MultiAgentRuns[0].Budget.PerCallMaxOutputTokens)
	assert.Equal(t, 12, call.MaxOutputTokens)
	assert.Equal(t, session.MultiAgentRunStatusCompleted, call.Status)
}

func TestMultiAgentRunRecorderCapsUnboundedCallToRunOutputBudget(t *testing.T) {
	t.Parallel()

	store := session.NewStore(t.TempDir())
	sessionState := session.New("gpt-test", nil)
	recorder := newMultiAgentRunRecorder(
		store,
		&sessionState,
		session.MultiAgentRunKindSpeculation,
		"cap default provider output",
		"gpt-test",
		nil,
		session.MultiAgentRunBudget{MaxRunOutputTokens: 12},
		nil,
		nil,
	)
	require.NoError(t, recorder.start())

	_, err := recorder.complete(t.Context(), multiAgentCallInfo{Phase: multiAgentPhaseProposal, Agent: "planner"}, llm.CompleteParams{
		Model:    "gpt-test",
		Messages: []llm.Message{{Role: llm.RoleUser, Content: "review"}},
	}, nil, func(_ context.Context, params llm.CompleteParams, _ []string) (*llm.Response, error) {
		assert.Equal(t, 12, params.MaxTokens)

		return &llm.Response{Content: "", Model: "gpt-test", InputTokens: 2, OutputTokens: 2}, nil
	})
	require.NoError(t, err)

	loaded, err := store.Load(sessionState.ID)
	require.NoError(t, err)
	require.Len(t, loaded.MultiAgentRuns, 1)
	require.Len(t, loaded.MultiAgentRuns[0].Calls, 1)

	call := loaded.MultiAgentRuns[0].Calls[0]
	assert.Equal(t, 12, call.MaxOutputTokens)
	assert.Equal(t, session.MultiAgentRunStatusCompleted, call.Status)
	assert.Equal(t, 1, loaded.MultiAgentRuns[0].Usage.ModelCalls)
}

func TestMultiAgentRunRecorderCapsUnboundedCallToRunTotalBudget(t *testing.T) {
	t.Parallel()

	store := session.NewStore(t.TempDir())
	sessionState := session.New("gpt-test", nil)
	messages := []llm.Message{{Role: llm.RoleUser, Content: "review this"}}
	inputEstimate := llm.EstimateTokens(messages)
	recorder := newMultiAgentRunRecorder(
		store,
		&sessionState,
		session.MultiAgentRunKindSpeculation,
		"cap default provider total",
		"gpt-test",
		nil,
		session.MultiAgentRunBudget{MaxRunTotalTokens: inputEstimate + 12},
		nil,
		nil,
	)
	require.NoError(t, recorder.start())

	_, err := recorder.complete(t.Context(), multiAgentCallInfo{Phase: multiAgentPhaseProposal, Agent: "planner"}, llm.CompleteParams{
		Model:    "gpt-test",
		Messages: messages,
	}, nil, func(_ context.Context, params llm.CompleteParams, _ []string) (*llm.Response, error) {
		assert.Equal(t, 12, params.MaxTokens)

		return &llm.Response{Content: "", Model: "gpt-test", InputTokens: inputEstimate, OutputTokens: 2}, nil
	})
	require.NoError(t, err)

	loaded, err := store.Load(sessionState.ID)
	require.NoError(t, err)
	require.Len(t, loaded.MultiAgentRuns, 1)
	require.Len(t, loaded.MultiAgentRuns[0].Calls, 1)
	assert.Equal(t, 12, loaded.MultiAgentRuns[0].Calls[0].MaxOutputTokens)
}

func TestMultiAgentRunRecorderRejectsUnboundedCallWhenOutputBudgetReserved(t *testing.T) {
	t.Parallel()

	store := session.NewStore(t.TempDir())
	sessionState := session.New("gpt-test", nil)
	recorder := newMultiAgentRunRecorder(
		store,
		&sessionState,
		session.MultiAgentRunKindSpeculation,
		"reserve default output",
		"gpt-test",
		nil,
		session.MultiAgentRunBudget{MaxRunOutputTokens: 5},
		nil,
		nil,
	)
	require.NoError(t, recorder.start())

	params := llm.CompleteParams{
		Model:    "gpt-test",
		Messages: []llm.Message{{Role: llm.RoleUser, Content: "propose"}},
	}

	callID, err := recorder.startCall(multiAgentCallInfo{Phase: multiAgentPhaseProposal, Agent: "planner"}, params)
	require.NoError(t, err)
	assert.Equal(t, "call-001", callID)

	_, err = recorder.startCall(multiAgentCallInfo{Phase: multiAgentPhaseProposal, Agent: "coder"}, params)
	require.Error(t, err)
	assert.Contains(t, err.Error(), multiAgentBudgetRuleRunOutput)
	assert.Contains(t, err.Error(), "used 6 of 5")

	loaded, err := store.Load(sessionState.ID)
	require.NoError(t, err)
	require.Len(t, loaded.MultiAgentRuns, 1)
	require.Len(t, loaded.MultiAgentRuns[0].Calls, 2)

	run := loaded.MultiAgentRuns[0]
	assert.Equal(t, 1, run.Usage.ModelCalls)
	assert.Equal(t, 5, run.Usage.EstimatedOutputTokens)
	assert.Equal(t, session.MultiAgentRunStatusRunning, run.Calls[0].Status)
	assert.Equal(t, 5, run.Calls[0].MaxOutputTokens)
	assert.Equal(t, session.MultiAgentRunStatusBudgetExhausted, run.Calls[1].Status)
	assert.Equal(t, multiAgentBudgetRuleRunOutput, run.Calls[1].BudgetRejectionRule)
	assert.Equal(t, 6, run.Calls[1].BudgetRejectionUsage)
	assert.Equal(t, 5, run.Calls[1].BudgetRejectionLimit)
}

func TestMultiAgentRunRecorderReservesRunOutputBudgetForInFlightCalls(t *testing.T) {
	t.Parallel()

	store := session.NewStore(t.TempDir())
	sessionState := session.New("gpt-test", nil)
	recorder := newMultiAgentRunRecorder(
		store,
		&sessionState,
		session.MultiAgentRunKindSpeculation,
		"reserve branch output",
		"gpt-test",
		nil,
		session.MultiAgentRunBudget{MaxRunOutputTokens: 10},
		nil,
		nil,
	)
	require.NoError(t, recorder.start())

	params := llm.CompleteParams{
		Model:     "gpt-test",
		MaxTokens: 6,
		Messages:  []llm.Message{{Role: llm.RoleUser, Content: "propose"}},
	}

	callID, err := recorder.startCall(multiAgentCallInfo{Phase: multiAgentPhaseProposal, Agent: "planner"}, params)
	require.NoError(t, err)
	assert.Equal(t, "call-001", callID)

	_, err = recorder.startCall(multiAgentCallInfo{Phase: multiAgentPhaseProposal, Agent: "coder"}, params)
	require.Error(t, err)
	assert.Contains(t, err.Error(), multiAgentBudgetRuleRunOutput)
	assert.Contains(t, err.Error(), "used 12 of 10")

	loaded, err := store.Load(sessionState.ID)
	require.NoError(t, err)
	require.Len(t, loaded.MultiAgentRuns, 1)
	require.Len(t, loaded.MultiAgentRuns[0].Calls, 2)

	run := loaded.MultiAgentRuns[0]
	assert.Equal(t, 1, run.Usage.ModelCalls)
	assert.Equal(t, 6, run.Usage.EstimatedOutputTokens)
	assert.Equal(t, 1, run.Usage.BudgetRejectedCalls)
	assert.Equal(t, 6, run.Calls[0].OutputTokenEstimate)
	assert.Equal(t, session.MultiAgentRunStatusBudgetExhausted, run.Calls[1].Status)
	assert.Equal(t, 6, run.Calls[1].OutputTokenEstimate)
	assert.Equal(t, 12, run.Calls[1].BudgetRejectionUsage)
}

func TestMultiAgentRunRecorderEnforcesRunTotalBudgetBeforeProviderCall(t *testing.T) {
	t.Parallel()

	store := session.NewStore(t.TempDir())
	sessionState := session.New("gpt-test", nil)
	recorder := newMultiAgentRunRecorder(
		store,
		&sessionState,
		session.MultiAgentRunKindSpeculation,
		"limit branch total",
		"gpt-test",
		nil,
		session.MultiAgentRunBudget{MaxRunTotalTokens: 10},
		nil,
		nil,
	)
	require.NoError(t, recorder.start())

	called := false
	_, err := recorder.complete(t.Context(), multiAgentCallInfo{Phase: multiAgentPhaseProposal, Agent: "planner"}, llm.CompleteParams{
		Model:     "gpt-test",
		MaxTokens: 10,
		Messages:  []llm.Message{{Role: llm.RoleUser, Content: "abcd"}},
	}, nil, func(context.Context, llm.CompleteParams, []string) (*llm.Response, error) {
		called = true
		return &llm.Response{Content: "should not happen"}, nil
	})
	require.Error(t, err)
	assert.Contains(t, err.Error(), multiAgentBudgetRuleRunTotal)
	assert.Contains(t, err.Error(), "used 23 of 10")
	assert.False(t, called)

	loaded, err := store.Load(sessionState.ID)
	require.NoError(t, err)
	require.Len(t, loaded.MultiAgentRuns, 1)
	require.Len(t, loaded.MultiAgentRuns[0].Calls, 1)

	call := loaded.MultiAgentRuns[0].Calls[0]
	assert.Equal(t, session.MultiAgentRunStatusBudgetExhausted, call.Status)
	assert.Equal(t, multiAgentBudgetRuleRunTotal, call.BudgetRejectionRule)
	assert.Equal(t, 23, call.BudgetRejectionUsage)
	assert.Equal(t, 10, call.BudgetRejectionLimit)
	assert.Equal(t, 0, loaded.MultiAgentRuns[0].Usage.ModelCalls)
	assert.Equal(t, 1, loaded.MultiAgentRuns[0].Usage.BudgetRejectedCalls)
}

func TestMultiAgentRunRecorderEnforcesPostflightActualTokenBudget(t *testing.T) {
	t.Parallel()

	store := session.NewStore(t.TempDir())
	sessionState := session.New("gpt-test", nil)
	recorder := newMultiAgentRunRecorder(
		store,
		&sessionState,
		session.MultiAgentRunKindReview,
		"audit output usage",
		"gpt-test",
		nil,
		session.MultiAgentRunBudget{MaxRunOutputTokens: 5},
		nil,
		nil,
	)
	require.NoError(t, recorder.start())

	called := false
	_, err := recorder.complete(t.Context(), multiAgentCallInfo{Phase: multiAgentPhaseReviewReport, Agent: "quality"}, llm.CompleteParams{
		Model:    "gpt-test",
		Messages: []llm.Message{{Role: llm.RoleUser, Content: "review"}},
	}, nil, func(context.Context, llm.CompleteParams, []string) (*llm.Response, error) {
		called = true

		return &llm.Response{Content: "", Model: "gpt-test", InputTokens: 2, OutputTokens: 12}, nil
	})
	require.Error(t, err)
	assert.Contains(t, err.Error(), multiAgentBudgetRuleRunOutput)
	assert.True(t, called)

	loaded, err := store.Load(sessionState.ID)
	require.NoError(t, err)
	require.Len(t, loaded.MultiAgentRuns, 1)
	require.Len(t, loaded.MultiAgentRuns[0].Calls, 1)

	call := loaded.MultiAgentRuns[0].Calls[0]
	assert.Equal(t, session.MultiAgentRunStatusBudgetExhausted, call.Status)
	assert.Equal(t, multiAgentBudgetRuleRunOutput, call.BudgetRejectionRule)
	assert.Equal(t, 12, call.BudgetRejectionUsage)
	assert.Equal(t, 12, call.OutputTokens)
	assert.Equal(t, 1, loaded.MultiAgentRuns[0].Usage.BudgetRejectedCalls)
	assert.Equal(t, 0, loaded.MultiAgentRuns[0].Usage.CompletedCalls)
}

func TestMultiAgentRunRecorderRecordsAndEnforcesRunCostBudget(t *testing.T) {
	t.Parallel()

	store := session.NewStore(t.TempDir())
	sessionState := session.New("gpt-test", nil)
	recorder := newMultiAgentRunRecorder(
		store,
		&sessionState,
		session.MultiAgentRunKindReview,
		"audit cost usage",
		"gpt-test",
		nil,
		session.MultiAgentRunBudget{MaxRunCostMicros: 5},
		nil,
		func(*llm.Response) (int64, error) { return 7, nil },
	)
	require.NoError(t, recorder.start())

	_, err := recorder.complete(t.Context(), multiAgentCallInfo{Phase: multiAgentPhaseReviewReport, Agent: "quality"}, llm.CompleteParams{
		Model:    "gpt-test",
		Messages: []llm.Message{{Role: llm.RoleUser, Content: "review"}},
	}, nil, func(context.Context, llm.CompleteParams, []string) (*llm.Response, error) {
		return &llm.Response{Content: "ok", Model: "gpt-test", InputTokens: 2, OutputTokens: 3}, nil
	})
	require.Error(t, err)
	assert.Contains(t, err.Error(), multiAgentBudgetRuleRunCost)

	loaded, err := store.Load(sessionState.ID)
	require.NoError(t, err)
	require.Len(t, loaded.MultiAgentRuns, 1)
	require.Len(t, loaded.MultiAgentRuns[0].Calls, 1)
	require.Len(t, loaded.MultiAgentRuns[0].Branches, 1)

	run := loaded.MultiAgentRuns[0]
	call := run.Calls[0]
	assert.Equal(t, session.MultiAgentRunStatusBudgetExhausted, call.Status)
	assert.Equal(t, multiAgentBudgetRuleRunCost, call.BudgetRejectionRule)
	assert.Equal(t, 7, call.BudgetRejectionUsage)
	assert.Equal(t, 5, call.BudgetRejectionLimit)
	assert.Equal(t, int64(7), call.EstimatedCostMicros)
	assert.Equal(t, int64(7), run.Usage.EstimatedCostMicros)
	assert.Equal(t, int64(7), run.Branches[0].EstimatedCostMicros)
	assert.Equal(t, multiAgentBudgetRuleRunCost, run.Branches[0].BudgetRejectionRule)
	assert.Equal(t, 7, run.Branches[0].BudgetRejectionUsage)
	assert.Equal(t, 5, run.Branches[0].BudgetRejectionLimit)
	assert.Equal(t, 1, run.Usage.BudgetRejectedCalls)
	assert.Equal(t, 0, run.Usage.CompletedCalls)
}

func TestMultiAgentRunRecorderRejectsCostBudgetWithoutEstimator(t *testing.T) {
	t.Parallel()

	store := session.NewStore(t.TempDir())
	sessionState := session.New("gpt-test", nil)
	recorder := newMultiAgentRunRecorder(
		store,
		&sessionState,
		session.MultiAgentRunKindSpeculation,
		"audit cost budget",
		"gpt-test",
		nil,
		session.MultiAgentRunBudget{MaxRunCostMicros: 5},
		nil,
		nil,
	)

	err := recorder.start()
	require.Error(t, err)
	assert.Contains(t, err.Error(), multiAgentBudgetRuleRunCost)
	assert.Contains(t, err.Error(), "requires a cost estimator")

	loaded, loadErr := store.Load(sessionState.ID)
	require.NoError(t, loadErr)
	require.Len(t, loaded.MultiAgentRuns, 1)

	run := loaded.MultiAgentRuns[0]
	assert.Equal(t, session.MultiAgentRunStatusBudgetExhausted, run.Status)
	assert.Equal(t, int64(5), run.Budget.MaxRunCostMicros)
	assert.Contains(t, run.Error, "requires a cost estimator")
	assert.Contains(t, run.ResumeReason, "terminal state "+string(session.MultiAgentRunStatusBudgetExhausted))
	assert.Empty(t, run.Calls)
	assert.Equal(t, 0, run.Usage.ModelCalls)
}

func TestMultiAgentRunRecorderReturnsCostEstimatorAndPersistErrors(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	store := session.NewStore(dir)
	sessionState := session.New("gpt-test", nil)

	require.NoError(t, os.RemoveAll(dir))
	require.NoError(t, os.WriteFile(dir, []byte("not a directory"), 0o600))

	recorder := newMultiAgentRunRecorder(
		store,
		&sessionState,
		session.MultiAgentRunKindSpeculation,
		"audit missing estimator persistence failure",
		"gpt-test",
		nil,
		session.MultiAgentRunBudget{MaxRunCostMicros: 5},
		nil,
		nil,
	)

	err := recorder.start()
	require.Error(t, err)
	assert.Contains(t, err.Error(), multiAgentBudgetRuleRunCost)
	assert.Contains(t, err.Error(), "requires a cost estimator")
	assert.Contains(t, err.Error(), "persist multi-agent run")
}

func TestMultiAgentRunRecorderEnforcesPerCallOutputLimit(t *testing.T) {
	t.Parallel()

	store := session.NewStore(t.TempDir())
	sessionState := session.New("gpt-test", nil)
	recorder := newMultiAgentRunRecorder(
		store,
		&sessionState,
		session.MultiAgentRunKindSpeculation,
		"cap each branch",
		"gpt-test",
		nil,
		session.MultiAgentRunBudget{},
		nil,
		nil,
	)
	require.NoError(t, recorder.start())

	_, err := recorder.complete(t.Context(), multiAgentCallInfo{Phase: multiAgentPhaseProposal, Agent: "planner"}, llm.CompleteParams{
		Model:     "gpt-test",
		MaxTokens: 1,
		Messages:  []llm.Message{{Role: llm.RoleUser, Content: "propose"}},
	}, nil, func(context.Context, llm.CompleteParams, []string) (*llm.Response, error) {
		return &llm.Response{Content: "", Model: "gpt-test", InputTokens: 2, OutputTokens: 4}, nil
	})
	require.Error(t, err)
	assert.Contains(t, err.Error(), multiAgentBudgetRuleOutput)

	loaded, err := store.Load(sessionState.ID)
	require.NoError(t, err)
	require.Len(t, loaded.MultiAgentRuns, 1)
	require.Len(t, loaded.MultiAgentRuns[0].Calls, 1)

	call := loaded.MultiAgentRuns[0].Calls[0]
	assert.Equal(t, session.MultiAgentRunStatusBudgetExhausted, call.Status)
	assert.Equal(t, multiAgentBudgetRuleOutput, call.BudgetRejectionRule)
	assert.Equal(t, 12, call.BudgetRejectionUsage)
	assert.Equal(t, 1, call.MaxOutputTokens)
	assert.Equal(t, 4, call.OutputTokens)
	assert.Equal(t, 1, loaded.MultiAgentRuns[0].Usage.BudgetRejectedCalls)
	assert.Equal(t, 0, loaded.MultiAgentRuns[0].Usage.CompletedCalls)
}

func TestFormatMultiAgentRunReplayUsesRecordedArtifacts(t *testing.T) {
	t.Parallel()

	longContent := strings.Repeat("x", 260) + "\nfinal line"
	run := session.MultiAgentRun{
		ID:     "run-1",
		Kind:   session.MultiAgentRunKindSpeculation,
		Status: session.MultiAgentRunStatusCompleted,
		Budget: session.MultiAgentRunBudget{
			PerCallMaxInputTokens:  100,
			PerCallMaxOutputTokens: 50,
			MaxRunTotalTokens:      500,
			MaxModelCalls:          6,
			MaxRunWallTimeMS:       30_000,
		},
		Usage: session.MultiAgentRunUsage{
			ModelCalls:            2,
			CompletedCalls:        1,
			CanceledCalls:         1,
			EstimatedInputTokens:  4,
			EstimatedOutputTokens: 8,
			EstimatedTotalTokens:  12,
			CachedInputTokens:     1,
			InputTokens:           4,
			OutputTokens:          8,
			TotalTokens:           12,
		},
		Summary: session.MultiAgentRunSummary{
			Winner: "planner",
			Reason: "best evidence",
		},
		Artifacts: []session.MultiAgentRunArtifact{
			{
				Kind:    multiAgentArtifactProposal,
				Phase:   multiAgentPhaseProposal,
				Agent:   "planner",
				Content: "plan",
				Index:   1,
				Metadata: map[string]string{
					"call_id":                        "call-001",
					multiAgentRawProviderResponseKey: multiAgentMetadataTrue,
				},
			},
			{
				Kind:        multiAgentArtifactCrossReview,
				Phase:       multiAgentPhaseCrossReview,
				Agent:       "executor",
				TargetAgent: "planner",
				Content:     longContent,
				Index:       2,
			},
			{
				Kind:    multiAgentArtifactVerdict,
				Phase:   multiAgentPhaseAggregateVerdict,
				Agent:   "judge",
				Content: "winner: planner\nreason: best evidence",
				Index:   1,
			},
		},
		Branches: []session.MultiAgentRunBranch{{
			Name:                 "planner",
			Role:                 multiAgentPhaseProposal,
			Provenance:           "provider-call:call-001",
			Model:                "gpt-test",
			Status:               session.MultiAgentRunStatusBudgetExhausted,
			InputTokenEstimate:   4,
			OutputTokenEstimate:  8,
			ContextWindow:        128,
			MaxOutputTokens:      64,
			CachedInputTokens:    1,
			BudgetRejectionRule:  multiAgentBudgetRuleRunTotal,
			BudgetRejectionUsage: 512,
			BudgetRejectionLimit: 500,
		}},
		Reviewers: []session.MultiAgentRunReviewer{{
			Name:        "executor",
			Role:        multiAgentPhaseCrossReview,
			TargetAgent: "planner",
			Model:       "gpt-test",
			PromptHash:  "sha256:def",
			CallID:      "call-002",
		}},
		Disagreements: []session.MultiAgentRunDisagreement{{
			Phase:       multiAgentPhaseCrossReview,
			Reviewer:    "executor",
			TargetAgent: "planner",
			Subject:     "cross-review",
			Notes:       "needs tests",
			Index:       2,
		}},
		Calls: []session.MultiAgentRunCall{{
			ID:                 "call-001",
			Phase:              multiAgentPhaseProposal,
			Agent:              "planner",
			Status:             session.MultiAgentRunStatusCompleted,
			RequestedModel:     "gpt-test",
			ResponseModel:      "backup-a",
			FallbackModels:     []string{"backup-a", "backup-b"},
			SystemPrompt:       "system\nprompt\twith tab",
			UserPrompt:         "user prompt",
			Response:           "stored response",
			InputTokenEstimate: 4,
			InputTokens:        4,
			CachedInputTokens:  1,
			OutputTokens:       8,
			TotalTokens:        12,
		}},
		Decisions: []session.MultiAgentRunDecision{
			{
				Kind:      multiAgentArtifactProposal,
				Phase:     multiAgentPhaseProposal,
				Agent:     "planner",
				Outcome:   multiAgentDecisionAccepted,
				Rationale: "best evidence",
				Index:     1,
			},
			{
				Kind:      multiAgentArtifactVerdict,
				Phase:     multiAgentPhaseAggregateVerdict,
				Agent:     "judge",
				Outcome:   multiAgentDecisionAccepted,
				Rationale: "best evidence",
				Index:     1,
			},
		},
		Gates: []session.MultiAgentRunGate{{
			Name:   "tests pass",
			Phase:  multiAgentPhaseAggregateVerdict,
			Agent:  "planner",
			Passed: true,
			Notes:  "recorded",
		}},
	}

	got := formatMultiAgentRunReplay(run)

	assert.Contains(t, got, "run: run-1")
	assert.Contains(t, got, "winner: planner")
	assert.Contains(t, got, "budget: per_call_max_input_tokens=100")
	assert.Contains(t, got, "per_call_max_output_tokens=50")
	assert.Contains(t, got, "max_run_total_tokens=500")
	assert.Contains(t, got, "max_model_calls=6")
	assert.Contains(t, got, "max_run_wall_time_ms=30000")
	assert.Contains(t, got, "usage: model_calls=2")
	assert.Contains(t, got, "completed_calls=1")
	assert.Contains(t, got, "canceled_calls=1")
	assert.Contains(t, got, "estimated_total_tokens=12")
	assert.Contains(t, got, "cached_input_tokens=1")
	assert.Contains(t, got, "branches:")
	assert.Contains(t, got, "name=planner")
	assert.Contains(t, got, "context_window=128")
	assert.Contains(t, got, "max_output_tokens=64")
	assert.Contains(t, got, "cached_input_tokens=1")
	assert.Contains(t, got, "budget_rejection=budget.max_run_total_tokens\tbudget_used=512\tbudget_limit=500")
	assert.Contains(t, got, "reviewers:")
	assert.Contains(t, got, "call=call-002")
	assert.Contains(t, got, "proposals:")
	assert.Contains(t, got, "agent=planner")
	assert.Contains(t, got, "phase=proposal")
	assert.Contains(t, got, "index=1")
	assert.Contains(t, got, "metadata.call_id=call-001\tmetadata.raw_provider_response=true")
	assert.Contains(t, got, "reviews:")
	assert.Contains(t, got, "target=planner")
	assert.Contains(t, got, "aggregate_verdict:")
	assert.Contains(t, got, "kind=verdict\tphase=aggregate-verdict\tagent=judge\tindex=1\tcontent=winner: planner\\nreason: best evidence")
	assert.Contains(t, got, "disagreements:")
	assert.Contains(t, got, "index=2")
	assert.Contains(t, got, "notes=needs tests")
	assert.Contains(t, got, "decisions:")
	assert.Contains(t, got, "kind=proposal\toutcome=accepted\tphase=proposal\tagent=planner\tindex=1")
	assert.Contains(t, got, "recorded_calls:")
	assert.Contains(t, got, "model=backup-a\trequested_model=gpt-test\tresponse_model=backup-a")
	assert.Contains(t, got, "fallback_models=backup-a,backup-b")
	assert.Contains(t, got, "input_tokens=4\tcached_input_tokens=1\toutput_tokens=8\ttotal_tokens=12")
	assert.Contains(t, got, "system_prompt=system\\nprompt\\twith tab")
	assert.Contains(t, got, "user_prompt=user prompt")
	assert.Contains(t, got, "response=stored response")
	assert.Contains(t, got, longContent[:260])
	assert.Contains(t, got, "\\nfinal line")
	assert.Contains(t, got, "name=tests pass\tstatus=PASS\tphase=aggregate-verdict\tagent=planner\tnotes=recorded")

	resume := formatMultiAgentRunResume(run)
	assert.Contains(t, resume, "resume_source: recorded_artifacts")
	assert.Contains(t, resume, "provider_calls: skipped")
	assert.Contains(t, resume, "terminal_state: completed")
	assert.Contains(t, resume, "resume_reason: run already completed; replay durable receipt run-1")
	assert.Contains(t, resume, "resume_cursor: recorded_calls=1\tartifacts=3\tdecisions=2\tgates=1")
	assert.Contains(t, resume, "last_call: id=call-001\tphase=proposal\tstatus=completed\tagent=planner")
	assert.Contains(t, resume, "next_action: inspect or export the completed decision trail")
	assert.Contains(t, resume, "resumed_output:")
	assert.Contains(t, resume, "source=artifact:verdict:1\tkind=verdict\tphase=aggregate-verdict\tagent=judge\tindex=1\tcontent=winner: planner\\nreason: best evidence")
	assert.Contains(t, resume, "winner: planner")
}

func TestFormatMultiAgentRunResumeExplainsPartialContinuationCursor(t *testing.T) {
	t.Parallel()

	run := session.MultiAgentRun{
		ID:           "run-budget",
		ReceiptID:    "receipt-budget",
		Kind:         session.MultiAgentRunKindSpeculation,
		Status:       session.MultiAgentRunStatusBudgetExhausted,
		ResumeReason: "continue budget receipt",
		Calls: []session.MultiAgentRunCall{
			{
				ID:     "call-001",
				Phase:  multiAgentPhaseProposal,
				Agent:  "planner",
				Status: session.MultiAgentRunStatusCompleted,
			},
			{
				ID:                  "call-002",
				Phase:               multiAgentPhaseProposal,
				Agent:               "coder",
				Status:              session.MultiAgentRunStatusBudgetExhausted,
				Error:               "budget.max_run_total_tokens exceeded",
				BudgetRejectionRule: multiAgentBudgetRuleRunTotal,
			},
		},
		Artifacts: []session.MultiAgentRunArtifact{{
			Kind:    multiAgentArtifactProposal,
			Phase:   multiAgentPhaseProposal,
			Agent:   "planner",
			Content: "partial plan",
			Index:   1,
		}},
		Decisions: []session.MultiAgentRunDecision{{
			Kind:      multiAgentArtifactProposal,
			Phase:     multiAgentPhaseProposal,
			Agent:     "planner",
			Outcome:   multiAgentDecisionRejected,
			Rationale: multiAgentRationaleStoppedBeforeVerdict,
			Index:     1,
		}},
	}

	resume := formatMultiAgentRunResume(run)

	assert.Contains(t, resume, "terminal_state: budget_exhausted")
	assert.Contains(t, resume, "resume_cursor: recorded_calls=2\tartifacts=1\tdecisions=1\tgates=0")
	assert.Contains(t, resume, "last_call: id=call-002\tphase=proposal\tstatus=budget_exhausted\tagent=coder\terror=budget.max_run_total_tokens exceeded")
	assert.Contains(t, resume, "next_action: resolve budget rejection at proposal agent coder call call-002")
	assert.Contains(t, resume, "resumable_artifacts:")
	assert.Contains(t, resume, "source=artifact:proposal:1\tkind=proposal\tphase=proposal\tagent=planner\tindex=1\tcontent=partial plan")
}

func TestFormatMultiAgentRunResumeUsesAcceptedDecisionToSelectOutput(t *testing.T) {
	t.Parallel()

	run := session.MultiAgentRun{
		ID:     "run-review",
		Kind:   session.MultiAgentRunKindReview,
		Status: session.MultiAgentRunStatusCompleted,
		Artifacts: []session.MultiAgentRunArtifact{
			{
				Kind:    multiAgentArtifactVerdict,
				Phase:   multiAgentPhaseAggregateVerdict,
				Agent:   "first-judge",
				Content: "stale verdict",
				Index:   1,
			},
			{
				Kind:    multiAgentArtifactVerdict,
				Phase:   multiAgentPhaseAggregateVerdict,
				Agent:   "final-judge",
				Content: "legacy indexless stale verdict",
			},
			{
				Kind:    multiAgentArtifactVerdict,
				Phase:   multiAgentPhaseAggregateVerdict,
				Agent:   "final-judge",
				Content: "accepted verdict",
				Index:   2,
			},
		},
		Decisions: []session.MultiAgentRunDecision{{
			Kind:    multiAgentArtifactVerdict,
			Phase:   multiAgentPhaseAggregateVerdict,
			Agent:   "final-judge",
			Outcome: multiAgentDecisionAccepted,
			Index:   2,
		}},
	}

	resume := formatMultiAgentRunResume(run)

	assert.Contains(t, resume, "resumed_output:")
	assert.Contains(t, resume, "source=artifact:verdict:2\tkind=verdict\tphase=aggregate-verdict\tagent=final-judge\tindex=2\tcontent=accepted verdict")
	assert.NotContains(t, resume, "source=artifact:verdict:1\tkind=verdict\tphase=aggregate-verdict\tagent=first-judge\tindex=1\tcontent=stale verdict")
	assert.NotContains(t, resume, "source=artifact:verdict\tkind=verdict\tphase=aggregate-verdict\tagent=final-judge\tcontent=legacy indexless stale verdict")
}

func TestFormatMultiAgentRunResumeFlagsCompletedReceiptWithoutAcceptedOutput(t *testing.T) {
	t.Parallel()

	run := session.MultiAgentRun{
		ID:     "run-incomplete-decision",
		Kind:   session.MultiAgentRunKindSpeculation,
		Status: session.MultiAgentRunStatusCompleted,
		Artifacts: []session.MultiAgentRunArtifact{{
			Kind:    multiAgentArtifactVerdict,
			Phase:   multiAgentPhaseAggregateVerdict,
			Agent:   "judge",
			Content: "rejected verdict",
			Index:   1,
		}},
		Decisions: []session.MultiAgentRunDecision{{
			Kind:      multiAgentArtifactVerdict,
			Phase:     multiAgentPhaseAggregateVerdict,
			Agent:     "judge",
			Outcome:   multiAgentDecisionRejected,
			Rationale: "gate failed",
			Index:     1,
		}},
	}

	resume := formatMultiAgentRunResume(run)

	assert.Contains(t, resume, "terminal_state: completed")
	assert.Contains(t, resume, "next_action: inspect completed receipt; accepted aggregate output is not recorded")
	assert.Contains(t, resume, "resumable_artifacts:")
	assert.NotContains(t, resume, "resumed_output:")
}

func TestFormatMultiAgentRunResumeDoesNotResumeEmptyAcceptedArtifact(t *testing.T) {
	t.Parallel()

	run := session.MultiAgentRun{
		ID:     "run-empty-accepted",
		Kind:   session.MultiAgentRunKindReview,
		Status: session.MultiAgentRunStatusCompleted,
		Artifacts: []session.MultiAgentRunArtifact{{
			Kind:    multiAgentArtifactVerdict,
			Phase:   multiAgentPhaseAggregateVerdict,
			Agent:   "judge",
			Content: " \t\n",
			Index:   1,
		}},
		Decisions: []session.MultiAgentRunDecision{{
			Kind:    multiAgentArtifactVerdict,
			Phase:   multiAgentPhaseAggregateVerdict,
			Agent:   "judge",
			Outcome: multiAgentDecisionAccepted,
			Index:   1,
		}},
		Summary: session.MultiAgentRunSummary{
			VerdictReviewer: "judge",
			Findings:        1,
		},
	}

	resume := formatMultiAgentRunResume(run)

	assert.Contains(t, resume, "terminal_state: completed")
	assert.Contains(t, resume, "next_action: inspect completed receipt; accepted aggregate output is not recorded")
	assert.NotContains(t, resume, "resumed_output:")
	assert.NotContains(t, resume, "verdict_reviewer: judge")
}

func TestFormatMultiAgentRunResumeExplainsRunLevelContinuation(t *testing.T) {
	t.Parallel()

	runningResume := formatMultiAgentRunResume(session.MultiAgentRun{
		ID:     "run-running",
		Kind:   session.MultiAgentRunKindReview,
		Status: session.MultiAgentRunStatusRunning,
	})
	assert.Contains(t, runningResume, "current_state: running")
	assert.Contains(t, runningResume, "resume_cursor: recorded_calls=0\tartifacts=0\tdecisions=0\tgates=0")
	assert.Contains(t, runningResume, "next_action: start pending provider calls from recorded request")

	runLevelBudgetResume := formatMultiAgentRunResume(session.MultiAgentRun{
		ID:     "run-budget-start",
		Kind:   session.MultiAgentRunKindSpeculation,
		Status: session.MultiAgentRunStatusBudgetExhausted,
		Error:  "budget.max_run_cost_micros requires estimator",
	})
	assert.Contains(t, runLevelBudgetResume, "terminal_state: budget_exhausted")
	assert.Contains(t, runLevelBudgetResume, "next_action: resolve run-level budget before starting provider calls")

	wallTimeBudgetResume := formatMultiAgentRunResume(session.MultiAgentRun{
		ID:     "run-wall-time-budget",
		Kind:   session.MultiAgentRunKindSpeculation,
		Status: session.MultiAgentRunStatusBudgetExhausted,
		Error:  multiAgentBudgetRuleRunWallTime + " exceeded",
		Calls: []session.MultiAgentRunCall{{
			ID:     "call-001",
			Phase:  multiAgentPhaseProposal,
			Agent:  "planner",
			Status: session.MultiAgentRunStatusCanceled,
			Error:  "multi-agent run context: context deadline exceeded",
		}},
	})
	assert.Contains(
		t,
		wallTimeBudgetResume,
		"last_call: id=call-001\tphase=proposal\tstatus="+string(session.MultiAgentRunStatusCanceled),
	)
	assert.Contains(t, wallTimeBudgetResume, "next_action: resolve run-level budget before starting provider calls")
}

func TestFormatMultiAgentRunSummaryIncludesReceiptID(t *testing.T) {
	t.Parallel()

	assert.Contains(t, formatMultiAgentRunSummary(session.MultiAgentRun{
		ID:        "run-1",
		ReceiptID: "receipt-1",
		Kind:      session.MultiAgentRunKindReview,
		Status:    session.MultiAgentRunStatusCompleted,
	}), "receipt_id=receipt-1")

	assert.Contains(t, formatMultiAgentRunSummary(session.MultiAgentRun{
		ID:     "run-2",
		Kind:   session.MultiAgentRunKindSpeculation,
		Status: session.MultiAgentRunStatusCompleted,
	}), "receipt_id=run-2")

	assert.Contains(t, formatMultiAgentRunSummary(session.MultiAgentRun{
		ID:     "run-3",
		Kind:   session.MultiAgentRunKindSpeculation,
		Status: session.MultiAgentRunStatusError,
		Error:  "line one\nline two\twith tab",
	}), "error=line one\\nline two\\twith tab")

	assert.Contains(t, formatMultiAgentRunSummary(session.MultiAgentRun{
		ID:     "run-4",
		Kind:   session.MultiAgentRunKindSpeculation,
		Status: session.MultiAgentRunStatusCompleted,
		Summary: session.MultiAgentRunSummary{
			Winner:          "planner\nbranch",
			VerdictReviewer: "reviewer\tone",
		},
		Artifacts: []session.MultiAgentRunArtifact{{
			Kind:    multiAgentArtifactVerdict,
			Content: "winner: planner\nbranch",
		}},
		Decisions: []session.MultiAgentRunDecision{{
			Kind:    multiAgentArtifactVerdict,
			Outcome: multiAgentDecisionAccepted,
		}},
	}), "winner=planner\\nbranch\tverdict_reviewer=reviewer\\tone")
}

func TestFormatMultiAgentRunReplaySuppressesPrematureSummary(t *testing.T) {
	t.Parallel()

	run := session.MultiAgentRun{
		ID:     "run-partial-summary",
		Kind:   session.MultiAgentRunKindSpeculation,
		Status: session.MultiAgentRunStatusRunning,
		Summary: session.MultiAgentRunSummary{
			Winner: "planner",
			Reason: "aggregate text was recorded before terminal state",
		},
		Artifacts: []session.MultiAgentRunArtifact{{
			Kind:    multiAgentArtifactVerdict,
			Phase:   multiAgentPhaseAggregateVerdict,
			Agent:   "judge",
			Content: "winner: planner\nreason: aggregate text was recorded before terminal state",
			Index:   1,
		}},
		Decisions: []session.MultiAgentRunDecision{{
			Kind:    multiAgentArtifactVerdict,
			Outcome: multiAgentDecisionAccepted,
			Phase:   multiAgentPhaseAggregateVerdict,
			Agent:   "judge",
			Index:   1,
		}},
	}

	replay := formatMultiAgentRunReplay(run)
	assert.NotContains(t, replay, "\nwinner: planner\n")
	assert.NotContains(t, replay, "\nreason: aggregate text was recorded before terminal state\n")
	assert.Contains(t, replay, "content=winner: planner\\nreason: aggregate text was recorded before terminal state")
	assert.NotContains(t, formatMultiAgentRunSummary(run), "winner=planner")

	run.Status = session.MultiAgentRunStatusCompleted
	completedReplay := formatMultiAgentRunReplay(run)
	assert.Contains(t, completedReplay, "\nwinner: planner\n")
	assert.Contains(t, completedReplay, "\nreason: aggregate text was recorded before terminal state\n")
	assert.Contains(t, formatMultiAgentRunSummary(run), "winner=planner")
}

func TestFormatMultiAgentRunReplayEscapesAcceptedSummaryFields(t *testing.T) {
	t.Parallel()

	speculationReplay := formatMultiAgentRunReplay(session.MultiAgentRun{
		ID:     "run-summary-escape",
		Kind:   session.MultiAgentRunKindSpeculation,
		Status: session.MultiAgentRunStatusCompleted,
		Summary: session.MultiAgentRunSummary{
			Winner: "planner\nbranch",
			Reason: "line one\tline two",
		},
		Artifacts: []session.MultiAgentRunArtifact{{
			Kind:    multiAgentArtifactVerdict,
			Phase:   multiAgentPhaseAggregateVerdict,
			Agent:   "judge",
			Content: "winner: planner",
			Index:   1,
		}},
		Decisions: []session.MultiAgentRunDecision{{
			Kind:    multiAgentArtifactVerdict,
			Phase:   multiAgentPhaseAggregateVerdict,
			Agent:   "judge",
			Outcome: multiAgentDecisionAccepted,
			Index:   1,
		}},
	})
	assert.Contains(t, speculationReplay, "winner: planner\\nbranch\n")
	assert.Contains(t, speculationReplay, "reason: line one\\tline two\n")

	reviewReplay := formatMultiAgentRunReplay(session.MultiAgentRun{
		ID:     "run-review-escape",
		Kind:   session.MultiAgentRunKindReview,
		Status: session.MultiAgentRunStatusCompleted,
		Summary: session.MultiAgentRunSummary{
			VerdictReviewer: "reviewer\tone",
			Findings:        2,
		},
		Artifacts: []session.MultiAgentRunArtifact{{
			Kind:    multiAgentArtifactVerdict,
			Content: "aggregate report",
		}},
		Decisions: []session.MultiAgentRunDecision{{
			Kind:    multiAgentArtifactVerdict,
			Outcome: multiAgentDecisionAccepted,
		}},
	})
	assert.Contains(t, reviewReplay, "verdict_reviewer: reviewer\\tone\n")
	assert.Contains(t, reviewReplay, "findings: 2\n")
}

func TestFormatMultiAgentRunReplayEscapesStoredIdentityFields(t *testing.T) {
	t.Parallel()

	run := session.MultiAgentRun{
		ID:             "run\none",
		ReceiptID:      "receipt\tone",
		Kind:           session.MultiAgentRunKindSpeculation,
		Status:         session.MultiAgentRunStatusRunning,
		Model:          "model\nprimary",
		FallbackModels: []string{"fallback\tone"},
		Branches: []session.MultiAgentRunBranch{{
			Name:       "planner\nbranch",
			Role:       multiAgentPhaseProposal,
			Status:     session.MultiAgentRunStatusCompleted,
			Model:      "branch\tmodel",
			Provenance: "provider-call:call\n001",
		}},
		Reviewers: []session.MultiAgentRunReviewer{{
			Name:        "reviewer\none",
			Role:        multiAgentPhaseCrossReview,
			TargetAgent: "planner\tbranch",
			CallID:      "call\n002",
		}},
		Artifacts: []session.MultiAgentRunArtifact{{
			Kind:        multiAgentArtifactCrossReview,
			Phase:       multiAgentPhaseCrossReview,
			Agent:       "reviewer\none",
			TargetAgent: "planner\tbranch",
			Content:     "challenge",
			Index:       1,
			Metadata:    map[string]string{"unsafe\nkey": "value\tone"},
		}},
		Disagreements: []session.MultiAgentRunDisagreement{{
			Phase:       multiAgentPhaseCrossReview,
			Reviewer:    "reviewer\none",
			TargetAgent: "planner\tbranch",
			Subject:     "cross\nreview",
			Notes:       "notes",
		}},
		Errors: []session.MultiAgentRunError{{
			Stage:       "aggregate\nverdict",
			Reviewer:    "judge\tone",
			TargetAgent: "planner\nbranch",
			Message:     "parse\tfailed\nhard",
		}},
		Decisions: []session.MultiAgentRunDecision{{
			Kind:      multiAgentArtifactCrossReview,
			Phase:     multiAgentPhaseCrossReview,
			Agent:     "reviewer\none",
			Outcome:   multiAgentDecisionRejected,
			Rationale: "partial",
		}},
		Gates: []session.MultiAgentRunGate{{
			Name:   "tests\npass",
			Phase:  multiAgentPhaseAggregateVerdict,
			Agent:  "judge\tone",
			Passed: true,
		}},
		Calls: []session.MultiAgentRunCall{{
			ID:             "call\n001",
			Phase:          multiAgentPhaseProposal,
			Agent:          "planner\nbranch",
			TargetAgent:    "target\tone",
			Status:         session.MultiAgentRunStatusCompleted,
			RequestedModel: "request\nmodel",
			ResponseModel:  "response\tmodel",
			FallbackModels: []string{"fallback\nmodel"},
		}},
	}

	replay := formatMultiAgentRunReplay(run)
	resume := formatMultiAgentRunResume(run)

	assert.Contains(t, replay, "run: run\\none\n")
	assert.Contains(t, replay, "receipt_id: receipt\\tone\n")
	assert.Contains(t, replay, "model: model\\nprimary\n")
	assert.Contains(t, replay, "fallback_models: fallback\\tone\n")
	assert.Contains(t, replay, "name=planner\\nbranch")
	assert.Contains(t, replay, "model=branch\\tmodel")
	assert.Contains(t, replay, "provenance=provider-call:call\\n001")
	assert.Contains(t, replay, "name=reviewer\\none\trole=cross-review\ttarget=planner\\tbranch")
	assert.Contains(t, replay, "metadata.unsafe\\nkey=value\\tone")
	assert.Contains(t, replay, "reviewer=reviewer\\none\ttarget=planner\\tbranch\tsubject=cross\\nreview")
	assert.Contains(t, replay, "stage=aggregate\\nverdict\treviewer=judge\\tone\ttarget=planner\\nbranch\tmessage=parse\\tfailed\\nhard")
	assert.Contains(t, replay, "agent=reviewer\\none")
	assert.Contains(t, replay, "name=tests\\npass\tstatus=PASS\tphase=aggregate-verdict\tagent=judge\\tone")
	assert.Contains(t, replay, "id=call\\n001\tphase=proposal\tstatus=completed\tagent=planner\\nbranch\ttarget=target\\tone")
	assert.Contains(t, replay, "model=response\\tmodel\trequested_model=request\\nmodel\tresponse_model=response\\tmodel")
	assert.Contains(t, replay, "fallback_models=fallback\\nmodel")
	assert.Contains(t, resume, "last_call: id=call\\n001\tphase=proposal\tstatus=completed\tagent=planner\\nbranch\ttarget=target\\tone")
	assert.Contains(t, resume, "source=artifact:cross_review:1\tkind=cross_review\tphase=cross-review\tagent=reviewer\\none")
}

func TestReplayContentEscapesControlCharactersAndBackslashes(t *testing.T) {
	t.Parallel()

	assert.Equal(t, `literal\\n newline\ncrlf\ncr\rtab\t`, replayContent("literal\\n newline\ncrlf\r\ncr\rtab\t"))
	assert.Equal(t, `C:\\tmp\\receipt.json`, replayContent(`C:\tmp\receipt.json`))
	assert.Equal(t, `nul\x00bell\x07del\x7f`, replayContent("nul\x00bell\x07del\x7f"))
	assert.Equal(t, []string{`metadata.a=first`, `metadata.z=last`}, appendStoredMetadataParts(nil, map[string]string{"z": "last", "a": "first"}))
}

func TestMultiAgentRunReadCommandsSelectReceiptKindAndLatest(t *testing.T) { //nolint:paralleltest // captures process-global stdout.
	sessionState := session.Session{
		ID: "abc",
		MultiAgentRuns: []session.MultiAgentRun{
			{
				ID:        "run-review",
				ReceiptID: "receipt-review",
				Kind:      session.MultiAgentRunKindReview,
				Status:    session.MultiAgentRunStatusCompleted,
				Summary:   session.MultiAgentRunSummary{VerdictReviewer: "review-judge", Findings: 2},
				Artifacts: []session.MultiAgentRunArtifact{{
					Kind:    multiAgentArtifactVerdict,
					Content: "review-judge aggregate report",
				}},
				Decisions: []session.MultiAgentRunDecision{{
					Kind:    multiAgentArtifactVerdict,
					Outcome: multiAgentDecisionAccepted,
				}},
				Calls: []session.MultiAgentRunCall{{
					ID:                 "call-001",
					Phase:              multiAgentPhaseReviewReport,
					Agent:              "quality",
					Status:             session.MultiAgentRunStatusCompleted,
					InputTokenEstimate: 4,
				}},
			},
			{
				ID:        "run-spec",
				ReceiptID: "receipt-spec",
				Kind:      session.MultiAgentRunKindSpeculation,
				Status:    session.MultiAgentRunStatusBudgetExhausted,
				Summary:   session.MultiAgentRunSummary{Winner: "planner", Reason: "best recorded evidence"},
				Artifacts: []session.MultiAgentRunArtifact{{
					Kind:    multiAgentArtifactProposal,
					Phase:   multiAgentPhaseProposal,
					Agent:   "planner",
					Content: "recorded proposal",
				}},
				ResumeReason: "continue from partial receipt",
			},
		},
	}

	listOut := captureStdoutForExport(t, func() {
		listMultiAgentRuns(sessionState)
	})
	assert.Contains(t, listOut, "receipt_id=receipt-review")
	assert.Contains(t, listOut, "receipt_id=receipt-spec")

	var err error

	showOut := captureStdoutForExport(t, func() {
		err = showMultiAgentRun(sessionState, "receipt-review")
	})
	require.NoError(t, err)
	assert.Contains(t, showOut, "receipt_id: receipt-review")
	assert.Contains(t, showOut, "kind: review")

	replayOut := captureStdoutForExport(t, func() {
		err = replayMultiAgentRun(sessionState, "review")
	})
	require.NoError(t, err)
	assert.Contains(t, replayOut, "run: run-review")
	assert.Contains(t, replayOut, "verdict_reviewer: review-judge")

	resumeOut := captureStdoutForExport(t, func() {
		err = resumeMultiAgentRun(sessionState, "latest")
	})
	require.NoError(t, err)
	assert.Contains(t, resumeOut, "provider_calls: skipped")
	assert.Contains(t, resumeOut, "run: run-spec")
	assert.Contains(t, resumeOut, "resumable_artifacts:")
	assert.Contains(t, resumeOut, "content=recorded proposal")
	assert.NotContains(t, resumeOut, "\nwinner: planner\n")

	err = showMultiAgentRun(sessionState, "missing")
	require.Error(t, err)
	assert.Contains(t, err.Error(), `multi-agent run "missing" not found`)
}

func TestExportMultiAgentRunJSONUsesRedactedSessionExport(t *testing.T) { //nolint:paralleltest // captures process-global stdout.
	const secret = "sk-proj-abcdefghijklmnopqrstuvwx1234567890"

	sessionState := session.Session{
		ID: "abc",
		MultiAgentRuns: []session.MultiAgentRun{{
			ID:     "run-1",
			Kind:   session.MultiAgentRunKindSpeculation,
			Status: session.MultiAgentRunStatusFailed,
			Prompt: "review key=" + secret,
			Calls: []session.MultiAgentRunCall{{
				ID:            "call-001",
				Phase:         multiAgentPhaseProposal,
				Agent:         "planner",
				Status:        session.MultiAgentRunStatusCompleted,
				UserPrompt:    "user key=" + secret,
				Response:      "proposal key=" + secret,
				Error:         "provider key=" + secret,
				ResponseModel: "gpt-test",
			}},
			Artifacts: []session.MultiAgentRunArtifact{{
				Kind:    multiAgentArtifactProposal,
				Phase:   multiAgentPhaseProposal,
				Agent:   "planner",
				Content: "artifact key=" + secret,
			}},
			Error: "run key=" + secret,
		}},
	}

	var err error

	out := captureStdoutForExport(t, func() {
		err = exportMultiAgentRun(sessionState, "latest", "json")
	})
	require.NoError(t, err)

	assert.Contains(t, out, `"multi_agent_runs"`)
	assert.Contains(t, out, `"redaction_profile": "redacted-shareable"`)
	assert.Contains(t, out, `"id": "run-1"`)
	assert.NotContains(t, out, secret)
}

func TestExportMultiAgentRunTextFormatsReplayAndResumeRecordedArtifacts(t *testing.T) { //nolint:paralleltest // captures process-global stdout.
	sessionState := session.Session{
		ID: "abc",
		MultiAgentRuns: []session.MultiAgentRun{{
			ID:        "run-1",
			ReceiptID: "receipt-1",
			Kind:      session.MultiAgentRunKindSpeculation,
			Status:    session.MultiAgentRunStatusCanceled,
			Prompt:    "resume from recorded output",
			Artifacts: []session.MultiAgentRunArtifact{{
				Kind:    multiAgentArtifactProposal,
				Phase:   multiAgentPhaseProposal,
				Agent:   "planner",
				Content: "recorded proposal",
				Index:   1,
			}},
			Calls: []session.MultiAgentRunCall{{
				ID:       "call-001",
				Phase:    multiAgentPhaseProposal,
				Agent:    "planner",
				Status:   session.MultiAgentRunStatusCanceled,
				Response: "partial response",
			}},
			ResumeReason: "operator canceled after first branch",
		}},
	}

	var err error

	replayOut := captureStdoutForExport(t, func() {
		err = exportMultiAgentRun(sessionState, "receipt-1", "text")
	})
	require.NoError(t, err)
	assert.Contains(t, replayOut, "run: run-1")
	assert.Contains(t, replayOut, "prompt: resume from recorded output")
	assert.Contains(t, replayOut, "content=recorded proposal")
	assert.NotContains(t, replayOut, "provider_calls: skipped")

	resumeOut := captureStdoutForExport(t, func() {
		err = exportMultiAgentRun(sessionState, "latest", "resume")
	})
	require.NoError(t, err)
	assert.Contains(t, resumeOut, "provider_calls: skipped")
	assert.Contains(t, resumeOut, "resume_reason: operator canceled after first branch")
	assert.Contains(t, resumeOut, "last_call: id=call-001")
	assert.Contains(t, resumeOut, "run: run-1")
}
