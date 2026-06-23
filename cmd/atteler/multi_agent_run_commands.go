package main

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/tommoulard/atteler/pkg/llm"
	"github.com/tommoulard/atteler/pkg/review"
	"github.com/tommoulard/atteler/pkg/session"
	"github.com/tommoulard/atteler/pkg/speculate"
)

const (
	multiAgentPhaseProposal                 = "proposal"
	multiAgentPhaseReviewReport             = "review-report"
	multiAgentPhaseCrossReview              = "cross-review"
	multiAgentPhaseAggregateVerdict         = "aggregate-verdict"
	multiAgentArtifactProposal              = "proposal"
	multiAgentArtifactReviewReport          = "review_report"
	multiAgentArtifactCrossReview           = "cross_review"
	multiAgentArtifactVerdict               = "verdict"
	multiAgentDecisionAccepted              = "accepted"
	multiAgentDecisionRejected              = "rejected"
	multiAgentReplayRefLatest               = "latest"
	multiAgentBudgetRuleInput               = "budget.per_call_max_input_tokens"
	multiAgentBudgetRuleOutput              = "budget.per_call_max_output_tokens"
	multiAgentBudgetRuleRunInput            = "budget.max_run_input_tokens"
	multiAgentBudgetRuleRunOutput           = "budget.max_run_output_tokens"
	multiAgentBudgetRuleRunTotal            = "budget.max_run_total_tokens"
	multiAgentBudgetRuleRunCost             = "budget.max_run_cost_micros"
	multiAgentBudgetRuleRunWallTime         = "budget.max_run_wall_time_ms"
	multiAgentBudgetRuleModelCalls          = "budget.max_model_calls"
	multiAgentBudgetRuleContextLimit        = "model.context_window"
	multiAgentRawProviderResponseKey        = "raw_provider_response"
	multiAgentMetadataTrue                  = "true"
	multiAgentRationaleStoppedBeforeVerdict = "run stopped before aggregate verdict"
	multiAgentRationaleNoAcceptedVerdict    = "run stopped before accepted aggregate verdict"
	gateStatusFail                          = "FAIL"
	gateStatusPass                          = "PASS"
)

type multiAgentCallInfo struct {
	Phase       string
	Agent       string
	TargetAgent string
}

type multiAgentRunRecorder struct {
	contextWindow  func(string) int
	costEstimator  llm.AgentLoopCostEstimator
	sessionState   *session.Session
	store          *session.Store
	run            session.MultiAgentRun
	pendingSummary session.MultiAgentRunSummary
	mu             sync.Mutex
	nextCall       int
}

func newMultiAgentRunRecorder(
	store *session.Store,
	sessionState *session.Session,
	kind string,
	prompt string,
	model string,
	fallbackModels []string,
	budget session.MultiAgentRunBudget,
	contextWindow func(string) int,
	costEstimator llm.AgentLoopCostEstimator,
) *multiAgentRunRecorder {
	return &multiAgentRunRecorder{
		store:         store,
		sessionState:  sessionState,
		contextWindow: contextWindow,
		costEstimator: costEstimator,
		run:           session.NewMultiAgentRun(kind, prompt, model, fallbackModels, budget),
	}
}

func multiAgentBudgetFromState(state appState) session.MultiAgentRunBudget {
	return session.MultiAgentRunBudget{
		PerCallMaxInputTokens:  state.maxInputTokens,
		PerCallMaxOutputTokens: state.generationOverrides.MaxTokens,
		MaxRunInputTokens:      state.agentLoopBudget.MaxInputTokens,
		MaxRunOutputTokens:     state.agentLoopBudget.MaxOutputTokens,
		MaxRunTotalTokens:      state.agentLoopBudget.MaxTotalTokens,
		MaxModelCalls:          state.agentLoopBudget.MaxModelCalls,
		MaxRunCostMicros:       state.agentLoopBudget.MaxCostMicros,
		MaxRunWallTimeMS:       multiAgentDurationBudgetMS(state.agentLoopBudget.MaxWallTime),
	}
}

func multiAgentDurationBudgetMS(duration time.Duration) int64 {
	if duration <= 0 {
		return 0
	}

	if duration < time.Millisecond {
		return 1
	}

	return duration.Milliseconds()
}

func (r *multiAgentRunRecorder) start() error {
	if r == nil {
		return nil
	}

	r.mu.Lock()
	defer r.mu.Unlock()

	if err := r.costBudgetEstimatorErrorLocked(); err != nil {
		now := time.Now().UTC()
		r.run.Status = session.MultiAgentRunStatusBudgetExhausted
		r.run.CompletedAt = &now
		r.run.Usage.DurationMS = durationMS(r.run.StartedAt, now)
		r.run.Error = err.Error()
		r.run.ResumeReason = multiAgentResumeReason(r.run)

		if persistErr := r.persistLocked(); persistErr != nil {
			return errors.Join(err, persistErr)
		}

		return err
	}

	return r.persistLocked()
}

func (r *multiAgentRunRecorder) costBudgetEstimatorErrorLocked() error {
	if r.run.Budget.MaxRunCostMicros <= 0 || r.costEstimator != nil {
		return nil
	}

	return fmt.Errorf("multi-agent run: %s requires a cost estimator", multiAgentBudgetRuleRunCost)
}

func (r *multiAgentRunRecorder) complete(
	ctx context.Context,
	info multiAgentCallInfo,
	params llm.CompleteParams,
	fallbackModels []string,
	call func(context.Context, llm.CompleteParams, []string) (*llm.Response, error),
) (*llm.Response, error) {
	if r == nil {
		return call(ctx, params, fallbackModels)
	}

	if ctxErr := contextErr(ctx); ctxErr != nil {
		if recordErr := r.recordCanceledCall(info, params, fallbackModels, ctxErr); recordErr != nil {
			return nil, errors.Join(ctxErr, recordErr)
		}

		return nil, ctxErr
	}

	callID, callParams, err := r.startCallWithParams(info, params, fallbackModels)
	if err != nil {
		return nil, err
	}

	resp, callErr := call(ctx, callParams, fallbackModels)
	if callErr == nil {
		callErr = contextErr(ctx)
	}

	if resp == nil && callErr == nil {
		callErr = errors.New("multi-agent run provider returned nil response")
	}

	finishErr := r.finishCall(callID, resp, callErr)
	if callErr != nil {
		return resp, errors.Join(callErr, finishErr)
	}

	if finishErr != nil {
		return resp, finishErr
	}

	return resp, nil
}

func completeMultiAgentRegistryCall(
	ctx context.Context,
	recorder *multiAgentRunRecorder,
	registry *llm.Registry,
	info multiAgentCallInfo,
	params llm.CompleteParams,
	fallbackModels []string,
) (*llm.Response, error) {
	if recorder == nil {
		resp, err := completeRegistryStreamWithFallback(ctx, registry, params, fallbackModels)
		if err != nil {
			return resp, fmt.Errorf("multi-agent registry complete: %w", err)
		}

		return resp, nil
	}

	chain := multiAgentModelFallbackChain(params.Model, fallbackModels)
	if len(chain) == 0 {
		return recorder.complete(ctx, info, params, nil, completeWithRegistry(registry))
	}

	failures := make([]error, 0, len(chain))
	for i, model := range chain {
		attemptParams := params
		attemptParams.Model = model

		resp, err := recorder.complete(ctx, info, attemptParams, chain[i+1:], completeWithRegistry(registry))
		if err == nil {
			return resp, nil
		}

		if isContextCancellation(err) || isBudgetRejection(err) {
			return resp, err
		}

		failures = append(failures, fmt.Errorf("%s: %w", model, err))
	}

	return nil, fmt.Errorf("multi-agent run: all fallback models failed: %w", errors.Join(failures...))
}

func completeWithRegistry(
	registry *llm.Registry,
) func(context.Context, llm.CompleteParams, []string) (*llm.Response, error) {
	return func(ctx context.Context, params llm.CompleteParams, _ []string) (*llm.Response, error) {
		return completeRegistryStream(ctx, registry, params)
	}
}

func multiAgentModelFallbackChain(primary string, fallbacks []string) []string {
	models := append([]string{primary}, fallbacks...)
	chain := make([]string, 0, len(models))
	seen := make(map[string]struct{}, len(models))

	for _, model := range models {
		model = strings.TrimSpace(model)
		if model == "" {
			continue
		}

		if _, ok := seen[model]; ok {
			continue
		}

		seen[model] = struct{}{}
		chain = append(chain, model)
	}

	return chain
}

func (r *multiAgentRunRecorder) startCall(
	info multiAgentCallInfo,
	params llm.CompleteParams,
) (string, error) {
	callID, _, err := r.startCallWithParams(info, params, nil)
	return callID, err
}

func (r *multiAgentRunRecorder) startCallWithParams(
	info multiAgentCallInfo,
	params llm.CompleteParams,
	fallbackModels []string,
) (string, llm.CompleteParams, error) {
	r.mu.Lock()
	defer r.mu.Unlock()

	r.capUnboundedOutputLocked(&params, fallbackModels)

	call := r.newCallLocked(info, params, fallbackModels)

	if rule, used, limit := r.preflightBudgetRejectionLocked(call); rule != "" {
		now := time.Now().UTC()
		call.Status = session.MultiAgentRunStatusBudgetExhausted
		call.CompletedAt = &now
		call.DurationMS = durationMS(call.StartedAt, now)
		call.Error = budgetRejectionMessage(rule, used, limit)
		call.BudgetRejectionRule = rule
		call.BudgetRejectionUsage = used
		call.BudgetRejectionLimit = limit
		r.run.Calls = append(r.run.Calls, call)
		r.run.Usage.BudgetRejectedCalls++

		if err := r.persistLocked(); err != nil {
			return "", params, multiAgentBudgetErrorWithPersistence(rule, used, limit, err)
		}

		return "", params, newMultiAgentBudgetError(rule, used, limit)
	}

	r.run.Calls = append(r.run.Calls, call)
	r.run.Usage.ModelCalls++
	r.run.Usage.EstimatedInputTokens += call.InputTokenEstimate
	r.run.Usage.EstimatedOutputTokens += call.OutputTokenEstimate
	r.run.Usage.EstimatedTotalTokens = r.run.Usage.EstimatedInputTokens + r.run.Usage.EstimatedOutputTokens

	if err := r.persistLocked(); err != nil {
		return "", params, err
	}

	return call.ID, params, nil
}

func (r *multiAgentRunRecorder) capUnboundedOutputLocked(params *llm.CompleteParams, fallbackModels []string) {
	if params.MaxTokens > 0 {
		return
	}

	capacity := r.unboundedOutputCapacityLocked(*params, fallbackModels)
	if capacity > 0 {
		params.MaxTokens = capacity
	}
}

func (r *multiAgentRunRecorder) unboundedOutputCapacityLocked(params llm.CompleteParams, fallbackModels []string) int {
	capacity := 0
	budget := r.run.Budget

	if contextWindow := multiAgentEffectiveContextWindow(params.Model, fallbackModels, r.contextWindow); contextWindow > 0 {
		capacity = minPositiveInt(capacity, contextWindow-llm.EstimateTokens(params.Messages))
	}

	if budget.PerCallMaxOutputTokens > 0 {
		capacity = minPositiveInt(capacity, budget.PerCallMaxOutputTokens)
	}

	if budget.MaxRunOutputTokens > 0 {
		capacity = minPositiveInt(capacity, budget.MaxRunOutputTokens-r.outputBudgetUsageLocked())
	}

	if budget.MaxRunTotalTokens > 0 {
		remainingTotal := budget.MaxRunTotalTokens - r.totalBudgetUsageLocked() - llm.EstimateTokens(params.Messages)
		capacity = minPositiveInt(capacity, remainingTotal)
	}

	return capacity
}

func minPositiveInt(current, candidate int) int {
	if candidate <= 0 {
		return current
	}

	if current == 0 || candidate < current {
		return candidate
	}

	return current
}

func (r *multiAgentRunRecorder) recordCanceledCall(
	info multiAgentCallInfo,
	params llm.CompleteParams,
	fallbackModels []string,
	callErr error,
) error {
	r.mu.Lock()
	defer r.mu.Unlock()

	call := r.newCallLocked(info, params, fallbackModels)
	now := time.Now().UTC()
	call.Status = session.MultiAgentRunStatusCanceled
	call.CompletedAt = &now
	call.DurationMS = durationMS(call.StartedAt, now)
	call.Error = callErr.Error()

	r.run.Calls = append(r.run.Calls, call)
	r.run.Usage.CanceledCalls++

	return r.persistLocked()
}

func (r *multiAgentRunRecorder) newCallLocked(
	info multiAgentCallInfo,
	params llm.CompleteParams,
	fallbackModels []string,
) session.MultiAgentRunCall {
	r.nextCall++
	systemPrompt, userPrompt := splitPromptMessages(params.Messages)

	return session.MultiAgentRunCall{
		ID:                  fmt.Sprintf("call-%03d", r.nextCall),
		Phase:               strings.TrimSpace(info.Phase),
		Agent:               strings.TrimSpace(info.Agent),
		TargetAgent:         strings.TrimSpace(info.TargetAgent),
		Status:              session.MultiAgentRunStatusRunning,
		StartedAt:           time.Now().UTC(),
		RequestedModel:      strings.TrimSpace(params.Model),
		FallbackModels:      append([]string(nil), fallbackModels...),
		PromptHash:          promptHash(systemPrompt, userPrompt),
		SystemPrompt:        systemPrompt,
		UserPrompt:          userPrompt,
		InputTokenEstimate:  llm.EstimateTokens(params.Messages),
		OutputTokenEstimate: positiveInt(params.MaxTokens),
		ContextWindow:       multiAgentEffectiveContextWindow(params.Model, fallbackModels, r.contextWindow),
		MaxOutputTokens:     params.MaxTokens,
	}
}

func multiAgentEffectiveContextWindow(
	model string,
	fallbackModels []string,
	contextWindow func(string) int,
) int {
	if contextWindow == nil {
		return 0
	}

	candidates := append([]string{model}, fallbackModels...)
	effective := 0

	for _, candidate := range candidates {
		limit := contextWindow(strings.TrimSpace(candidate))
		if limit <= 0 {
			continue
		}

		if effective == 0 || limit < effective {
			effective = limit
		}
	}

	return effective
}

func (r *multiAgentRunRecorder) preflightBudgetRejectionLocked(call session.MultiAgentRunCall) (rule string, used, limit int) {
	budget := r.run.Budget

	checks := []func(session.MultiAgentRunCall, session.MultiAgentRunBudget) (string, int, int){
		r.perCallInputBudgetRejectionLocked,
		r.perCallOutputBudgetRejectionLocked,
		r.contextWindowBudgetRejectionLocked,
		r.modelCallBudgetRejectionLocked,
		r.runInputBudgetRejectionLocked,
		r.runOutputBudgetRejectionLocked,
		r.runTotalBudgetRejectionLocked,
	}

	for _, check := range checks {
		if rule, used, limit := check(call, budget); rule != "" {
			return rule, used, limit
		}
	}

	return "", 0, 0
}

func (r *multiAgentRunRecorder) perCallInputBudgetRejectionLocked(
	call session.MultiAgentRunCall,
	budget session.MultiAgentRunBudget,
) (rule string, used, limit int) {
	if budget.PerCallMaxInputTokens > 0 && call.InputTokenEstimate > budget.PerCallMaxInputTokens {
		return multiAgentBudgetRuleInput, call.InputTokenEstimate, budget.PerCallMaxInputTokens
	}

	return "", 0, 0
}

func (r *multiAgentRunRecorder) perCallOutputBudgetRejectionLocked(
	call session.MultiAgentRunCall,
	budget session.MultiAgentRunBudget,
) (rule string, used, limit int) {
	requestedOutput := positiveInt(call.MaxOutputTokens)
	if budget.PerCallMaxOutputTokens > 0 && requestedOutput > budget.PerCallMaxOutputTokens {
		return multiAgentBudgetRuleOutput, requestedOutput, budget.PerCallMaxOutputTokens
	}

	return "", 0, 0
}

func (r *multiAgentRunRecorder) contextWindowBudgetRejectionLocked(
	call session.MultiAgentRunCall,
	_ session.MultiAgentRunBudget,
) (rule string, used, limit int) {
	contextUsage := call.InputTokenEstimate + positiveInt(call.MaxOutputTokens)
	if call.ContextWindow > 0 {
		if call.MaxOutputTokens <= 0 && contextUsage >= call.ContextWindow {
			return multiAgentBudgetRuleContextLimit, contextUsage + 1, call.ContextWindow
		}

		if contextUsage > call.ContextWindow {
			return multiAgentBudgetRuleContextLimit, contextUsage, call.ContextWindow
		}
	}

	return "", 0, 0
}

func (r *multiAgentRunRecorder) modelCallBudgetRejectionLocked(
	_ session.MultiAgentRunCall,
	budget session.MultiAgentRunBudget,
) (rule string, used, limit int) {
	used = r.run.Usage.ModelCalls + 1
	if budget.MaxModelCalls > 0 && used > budget.MaxModelCalls {
		return multiAgentBudgetRuleModelCalls, used, budget.MaxModelCalls
	}

	return "", 0, 0
}

func (r *multiAgentRunRecorder) runInputBudgetRejectionLocked(
	call session.MultiAgentRunCall,
	budget session.MultiAgentRunBudget,
) (rule string, used, limit int) {
	estimatedInput := r.inputBudgetUsageLocked() + call.InputTokenEstimate
	if budget.MaxRunInputTokens > 0 && estimatedInput > budget.MaxRunInputTokens {
		return multiAgentBudgetRuleRunInput, estimatedInput, budget.MaxRunInputTokens
	}

	return "", 0, 0
}

func (r *multiAgentRunRecorder) runOutputBudgetRejectionLocked(
	call session.MultiAgentRunCall,
	budget session.MultiAgentRunBudget,
) (rule string, used, limit int) {
	currentOutput := r.outputBudgetUsageLocked()
	requestedOutput := positiveInt(call.MaxOutputTokens)
	estimatedOutput := currentOutput + requestedOutput

	if budget.MaxRunOutputTokens > 0 {
		if requestedOutput == 0 && currentOutput >= budget.MaxRunOutputTokens {
			return multiAgentBudgetRuleRunOutput, currentOutput + 1, budget.MaxRunOutputTokens
		}

		if estimatedOutput > budget.MaxRunOutputTokens {
			return multiAgentBudgetRuleRunOutput, estimatedOutput, budget.MaxRunOutputTokens
		}
	}

	return "", 0, 0
}

func (r *multiAgentRunRecorder) runTotalBudgetRejectionLocked(
	call session.MultiAgentRunCall,
	budget session.MultiAgentRunBudget,
) (rule string, used, limit int) {
	currentTotal := r.totalBudgetUsageLocked()
	requestedTotal := call.InputTokenEstimate + positiveInt(call.MaxOutputTokens)
	estimatedTotal := currentTotal + requestedTotal

	if budget.MaxRunTotalTokens > 0 {
		if call.MaxOutputTokens <= 0 && estimatedTotal >= budget.MaxRunTotalTokens {
			return multiAgentBudgetRuleRunTotal, estimatedTotal + 1, budget.MaxRunTotalTokens
		}

		if estimatedTotal > budget.MaxRunTotalTokens {
			return multiAgentBudgetRuleRunTotal, estimatedTotal, budget.MaxRunTotalTokens
		}
	}

	return "", 0, 0
}

func positiveInt(value int) int {
	if value < 0 {
		return 0
	}

	return value
}

func positiveInt64(value int64) int64 {
	if value < 0 {
		return 0
	}

	return value
}

func (r *multiAgentRunRecorder) perCallOutputRejectionLocked(call session.MultiAgentRunCall) (rule string, used, limit int) {
	limit = r.run.Budget.PerCallMaxOutputTokens
	if limit <= 0 {
		limit = call.MaxOutputTokens
	}

	if limit <= 0 {
		return "", 0, 0
	}

	used = maxInt(call.OutputTokens, call.OutputTokenEstimate)
	if used > limit {
		return multiAgentBudgetRuleOutput, used, limit
	}

	return "", 0, 0
}

//nolint:gocognit,nestif // State transitions intentionally stay together so call receipts remain atomic.
func (r *multiAgentRunRecorder) finishCall(callID string, resp *llm.Response, callErr error) error {
	r.mu.Lock()
	defer r.mu.Unlock()

	now := time.Now().UTC()

	for i := range r.run.Calls {
		call := &r.run.Calls[i]
		if call.ID != callID {
			continue
		}

		call.CompletedAt = &now
		call.DurationMS = durationMS(call.StartedAt, now)
		previousOutputEstimate := call.OutputTokenEstimate

		if resp != nil {
			call.Response = resp.Content
			call.ResponseModel = resp.Model
			call.InputTokens = resp.InputTokens
			call.CachedInputTokens = resp.CachedInputTokens
			call.OutputTokens = resp.OutputTokens
			call.TotalTokens = resp.InputTokens + resp.OutputTokens
			call.OutputTokenEstimate = llm.EstimateTokens([]llm.Message{{Role: llm.RoleAssistant, Content: resp.Content}})

			if r.costEstimator != nil {
				costMicros, costErr := r.costEstimator(resp)
				if costErr != nil {
					callErr = errors.Join(callErr, fmt.Errorf("multi-agent run cost estimate: %w", costErr))
				} else {
					call.EstimatedCostMicros = positiveInt64(costMicros)
				}
			}

			r.run.Usage.InputTokens += resp.InputTokens
			r.run.Usage.CachedInputTokens += resp.CachedInputTokens
			r.run.Usage.OutputTokens += resp.OutputTokens
			r.run.Usage.TotalTokens = r.run.Usage.InputTokens + r.run.Usage.OutputTokens
			r.run.Usage.EstimatedCostMicros += call.EstimatedCostMicros
			r.run.Usage.EstimatedOutputTokens += call.OutputTokenEstimate - previousOutputEstimate

			if r.run.Usage.EstimatedOutputTokens < 0 {
				r.run.Usage.EstimatedOutputTokens = 0
			}

			r.run.Usage.EstimatedTotalTokens = r.run.Usage.EstimatedInputTokens + r.run.Usage.EstimatedOutputTokens
		}

		switch {
		case callErr == nil:
			call.Status = session.MultiAgentRunStatusCompleted
			r.run.Usage.CompletedCalls++
		case isContextCancellation(callErr):
			call.Status = session.MultiAgentRunStatusCanceled
			call.Error = callErr.Error()
			r.run.Usage.CanceledCalls++
		case isBudgetRejection(callErr):
			call.Status = session.MultiAgentRunStatusBudgetExhausted
			call.Error = callErr.Error()
			r.run.Usage.BudgetRejectedCalls++
		default:
			call.Status = session.MultiAgentRunStatusError
			call.Error = callErr.Error()
			r.run.Usage.ProviderFailedCalls++
		}

		if resp != nil {
			r.upsertCallArtifactLocked(now, *call)
		}

		if callErr == nil {
			if budgetErr := r.rejectCompletedCallForBudgetLocked(call); budgetErr != nil {
				return budgetErr
			}
		}

		return r.persistLocked()
	}

	return fmt.Errorf("multi-agent run: missing call %s", callID)
}

func (r *multiAgentRunRecorder) upsertCallArtifactLocked(now time.Time, call session.MultiAgentRunCall) {
	if strings.TrimSpace(call.Response) == "" {
		return
	}

	kind := multiAgentArtifactKindForPhase(call.Phase)
	if kind == "" {
		return
	}

	artifact := session.MultiAgentRunArtifact{
		CreatedAt:   now,
		Kind:        kind,
		Phase:       call.Phase,
		Agent:       call.Agent,
		TargetAgent: call.TargetAgent,
		Content:     call.Response,
		Index:       len(r.run.Artifacts) + 1,
		Metadata: map[string]string{
			"call_id":                        call.ID,
			multiAgentRawProviderResponseKey: multiAgentMetadataTrue,
		},
	}

	for i := range r.run.Artifacts {
		if r.run.Artifacts[i].Metadata["call_id"] != call.ID {
			continue
		}

		artifact.CreatedAt = r.run.Artifacts[i].CreatedAt
		artifact.Index = r.run.Artifacts[i].Index
		r.run.Artifacts[i] = artifact

		return
	}

	r.run.Artifacts = append(r.run.Artifacts, artifact)
}

func multiAgentArtifactKindForPhase(phase string) string {
	switch phase {
	case multiAgentPhaseProposal:
		return multiAgentArtifactProposal
	case multiAgentPhaseReviewReport:
		return multiAgentArtifactReviewReport
	case multiAgentPhaseCrossReview:
		return multiAgentArtifactCrossReview
	case multiAgentPhaseAggregateVerdict:
		return multiAgentArtifactVerdict
	default:
		return ""
	}
}

func (r *multiAgentRunRecorder) rejectCompletedCallForBudgetLocked(call *session.MultiAgentRunCall) error {
	if rule, used, limit := r.postflightContextWindowRejectionLocked(*call); rule != "" {
		return r.failCallForBudgetLocked(call, rule, used, limit)
	}

	if rule, limit := r.postflightBudgetRejectionLocked(); rule != "" {
		return r.failCallForBudgetLocked(call, rule, r.postflightBudgetUsage(rule), limit)
	}

	if rule, used, limit := r.perCallOutputRejectionLocked(*call); rule != "" {
		return r.failCallForBudgetLocked(call, rule, used, limit)
	}

	return nil
}

func (r *multiAgentRunRecorder) postflightContextWindowRejectionLocked(
	call session.MultiAgentRunCall,
) (rule string, used, limit int) {
	if call.ContextWindow <= 0 {
		return "", 0, 0
	}

	used = maxInt(call.InputTokens, call.InputTokenEstimate) +
		maxInt(call.OutputTokens, call.OutputTokenEstimate)
	if used > call.ContextWindow {
		return multiAgentBudgetRuleContextLimit, used, call.ContextWindow
	}

	return "", 0, 0
}

func (r *multiAgentRunRecorder) failCallForBudgetLocked(
	call *session.MultiAgentRunCall,
	rule string,
	used int,
	limit int,
) error {
	call.Status = session.MultiAgentRunStatusBudgetExhausted
	call.Error = budgetRejectionMessage(rule, used, limit)
	call.BudgetRejectionRule = rule
	call.BudgetRejectionUsage = used
	call.BudgetRejectionLimit = limit
	r.run.Usage.CompletedCalls--
	r.run.Usage.BudgetRejectedCalls++

	if err := r.persistLocked(); err != nil {
		return multiAgentBudgetErrorWithPersistence(rule, used, limit, err)
	}

	return newMultiAgentBudgetError(rule, used, limit)
}

func (r *multiAgentRunRecorder) postflightBudgetRejectionLocked() (rule string, limit int) {
	budget := r.run.Budget
	if budget.MaxRunInputTokens > 0 && r.inputBudgetUsageLocked() > budget.MaxRunInputTokens {
		return multiAgentBudgetRuleRunInput, budget.MaxRunInputTokens
	}

	if budget.MaxRunOutputTokens > 0 && r.outputBudgetUsageLocked() > budget.MaxRunOutputTokens {
		return multiAgentBudgetRuleRunOutput, budget.MaxRunOutputTokens
	}

	if budget.MaxRunTotalTokens > 0 && r.totalBudgetUsageLocked() > budget.MaxRunTotalTokens {
		return multiAgentBudgetRuleRunTotal, budget.MaxRunTotalTokens
	}

	if budget.MaxRunCostMicros > 0 && r.run.Usage.EstimatedCostMicros > budget.MaxRunCostMicros {
		return multiAgentBudgetRuleRunCost, intFromInt64(budget.MaxRunCostMicros)
	}

	return "", 0
}

func (r *multiAgentRunRecorder) postflightBudgetUsage(rule string) int {
	switch rule {
	case multiAgentBudgetRuleRunInput:
		return r.inputBudgetUsageLocked()
	case multiAgentBudgetRuleRunOutput:
		return r.outputBudgetUsageLocked()
	case multiAgentBudgetRuleRunTotal:
		return r.totalBudgetUsageLocked()
	case multiAgentBudgetRuleRunCost:
		return intFromInt64(r.run.Usage.EstimatedCostMicros)
	default:
		return 0
	}
}

func (r *multiAgentRunRecorder) inputBudgetUsageLocked() int {
	return maxInt(r.run.Usage.InputTokens, r.run.Usage.EstimatedInputTokens)
}

func (r *multiAgentRunRecorder) outputBudgetUsageLocked() int {
	return maxInt(r.run.Usage.OutputTokens, r.run.Usage.EstimatedOutputTokens)
}

func (r *multiAgentRunRecorder) totalBudgetUsageLocked() int {
	return maxInt(r.run.Usage.TotalTokens, r.run.Usage.EstimatedTotalTokens)
}

func maxInt(left, right int) int {
	if left > right {
		return left
	}

	return right
}

func intFromInt64(value int64) int {
	if value <= 0 {
		return 0
	}

	maxValue := int64(^uint(0) >> 1)
	if value > maxValue {
		return int(maxValue)
	}

	return int(value)
}

func (r *multiAgentRunRecorder) recordSpeculateSession(specSession speculate.Session) error {
	if r == nil {
		return nil
	}

	r.mu.Lock()
	defer r.mu.Unlock()

	now := time.Now().UTC()
	rawArtifacts := rawProviderResponseArtifacts(r.run.Artifacts)
	r.run.Artifacts = nil
	r.run.Gates = nil
	r.run.Decisions = nil
	r.run.Errors = nil
	hasVerdict := strings.TrimSpace(specSession.Verdict.Winner) != "" ||
		strings.TrimSpace(specSession.Verdict.Reason) != "" ||
		len(specSession.Verdict.GateChecks) > 0

	verdictOutcome, verdictRationale := "", ""
	if hasVerdict {
		verdictOutcome, verdictRationale = speculateVerdictDecision(specSession)
	}

	for i, proposal := range specSession.Proposals {
		if strings.TrimSpace(proposal.Agent) == "" && strings.TrimSpace(proposal.Content) == "" {
			continue
		}

		r.run.Artifacts = append(r.run.Artifacts, session.MultiAgentRunArtifact{
			CreatedAt: now,
			Kind:      multiAgentArtifactProposal,
			Phase:     multiAgentPhaseProposal,
			Agent:     proposal.Agent,
			Content:   proposal.Content,
			Index:     i + 1,
		})

		outcome, rationale := speculateProposalDecision(
			specSession.Verdict,
			proposal.Agent,
			hasVerdict,
			verdictOutcome,
			verdictRationale,
		)

		r.run.Decisions = append(r.run.Decisions, session.MultiAgentRunDecision{
			Kind:      multiAgentArtifactProposal,
			Phase:     multiAgentPhaseProposal,
			Agent:     proposal.Agent,
			Outcome:   outcome,
			Rationale: rationale,
			Index:     i + 1,
		})
	}

	for i, crossReview := range specSession.Reviews {
		if strings.TrimSpace(crossReview.Reviewer) == "" && strings.TrimSpace(crossReview.Notes) == "" {
			continue
		}

		r.run.Artifacts = append(r.run.Artifacts, session.MultiAgentRunArtifact{
			CreatedAt:   now,
			Kind:        multiAgentArtifactCrossReview,
			Phase:       multiAgentPhaseCrossReview,
			Agent:       crossReview.Reviewer,
			TargetAgent: crossReview.TargetAgent,
			Content:     crossReview.Notes,
			Index:       i + 1,
		})
	}

	if hasVerdict {
		r.run.Artifacts = append(r.run.Artifacts, session.MultiAgentRunArtifact{
			CreatedAt: now,
			Kind:      multiAgentArtifactVerdict,
			Phase:     multiAgentPhaseAggregateVerdict,
			Agent:     specSession.Verdict.Winner,
			Content:   formatSpeculateVerdictArtifact(specSession.Verdict),
			Index:     1,
		})
		r.run.Decisions = append(r.run.Decisions, session.MultiAgentRunDecision{
			Kind:      multiAgentArtifactVerdict,
			Phase:     multiAgentPhaseAggregateVerdict,
			Agent:     specSession.Verdict.Winner,
			Outcome:   verdictOutcome,
			Rationale: verdictRationale,
			Index:     1,
		})
	}

	for _, gate := range specSession.Verdict.GateChecks {
		r.run.Gates = append(r.run.Gates, session.MultiAgentRunGate{
			Name:   gate.Name,
			Phase:  multiAgentPhaseAggregateVerdict,
			Agent:  specSession.Verdict.Winner,
			Passed: gate.Passed,
			Notes:  gate.Notes,
		})
	}

	r.recordSpeculationErrorsLocked(specSession, hasVerdict)

	r.pendingSummary = acceptedSpeculationSummary(specSession, verdictOutcome)
	r.run.Summary = session.MultiAgentRunSummary{}

	annotateArtifactsWithRawCallMetadata(r.run.Artifacts, rawArtifacts, r.run.Calls)
	r.appendUnrepresentedRawArtifactsLocked(rawArtifacts)

	return r.persistLocked()
}

func (r *multiAgentRunRecorder) recordSpeculationErrorsLocked(specSession speculate.Session, hasVerdict bool) {
	if !hasVerdict {
		return
	}

	if err := speculationVerdictValidationError(specSession); err != nil {
		r.appendRunErrorLocked(session.MultiAgentRunError{
			Stage:       multiAgentPhaseAggregateVerdict,
			Reviewer:    "judge",
			TargetAgent: specSession.Verdict.Winner,
			Message:     "aggregate verdict failed validation: " + err.Error(),
		})
	}
}

func acceptedSpeculationSummary(
	specSession speculate.Session,
	verdictOutcome string,
) session.MultiAgentRunSummary {
	if verdictOutcome != multiAgentDecisionAccepted {
		return session.MultiAgentRunSummary{}
	}

	return session.MultiAgentRunSummary{
		Winner: specSession.Verdict.Winner,
		Reason: specSession.Verdict.Reason,
	}
}

func speculateProposalDecision(
	verdict speculate.Verdict,
	agent string,
	hasVerdict bool,
	verdictOutcome string,
	verdictRationale string,
) (outcome, rationale string) {
	if !hasVerdict {
		return multiAgentDecisionRejected, multiAgentRationaleStoppedBeforeVerdict
	}

	if verdictOutcome != multiAgentDecisionAccepted {
		return multiAgentDecisionRejected, verdictRationale
	}

	if strings.TrimSpace(agent) == strings.TrimSpace(verdict.Winner) {
		return multiAgentDecisionAccepted, speculateProposalDecisionRationale(verdict, agent, hasVerdict)
	}

	return multiAgentDecisionRejected, speculateProposalDecisionRationale(verdict, agent, hasVerdict)
}

func speculateProposalDecisionRationale(verdict speculate.Verdict, agent string, hasVerdict bool) string {
	winner := strings.TrimSpace(verdict.Winner)
	reason := strings.TrimSpace(verdict.Reason)

	switch {
	case strings.TrimSpace(agent) == winner && reason != "":
		return reason
	case strings.TrimSpace(agent) == winner:
		return "selected by aggregate verdict"
	case !hasVerdict:
		return multiAgentRationaleStoppedBeforeVerdict
	case winner == "" && reason != "":
		return reason
	case winner == "":
		return "aggregate verdict did not select a winning branch"
	case reason != "":
		return reason
	default:
		return "rejected by aggregate verdict"
	}
}

func speculateVerdictDecisionRationale(verdict speculate.Verdict) string {
	if reason := strings.TrimSpace(verdict.Reason); reason != "" {
		return reason
	}

	return "aggregate verdict accepted as final speculation output"
}

func speculateVerdictDecision(specSession speculate.Session) (outcome, rationale string) {
	if err := speculationVerdictValidationError(specSession); err != nil {
		return multiAgentDecisionRejected, "aggregate verdict failed validation: " + err.Error()
	}

	return multiAgentDecisionAccepted, speculateVerdictDecisionRationale(specSession.Verdict)
}

func speculationVerdictValidationError(specSession speculate.Session) error {
	if err := speculate.ValidateVerdict(specSession.Verdict, specSession.Plan.GateChecks); err != nil {
		return fmt.Errorf("%w", err)
	}

	if err := validateSpeculationWinnerRecorded(specSession); err != nil {
		return err
	}

	return nil
}

func validateSpeculationWinnerRecorded(specSession speculate.Session) error {
	winner := strings.TrimSpace(specSession.Verdict.Winner)
	if winner == "" {
		return nil
	}

	candidates := recordedSpeculationCandidates(specSession)
	if len(candidates) == 0 {
		return fmt.Errorf("winner %q has no recorded candidate branch", winner)
	}

	if _, ok := candidates[winner]; !ok {
		return fmt.Errorf("winner %q is not a recorded candidate branch", winner)
	}

	return nil
}

func recordedSpeculationCandidates(specSession speculate.Session) map[string]struct{} {
	candidates := make(map[string]struct{}, len(specSession.Proposals))
	for _, proposal := range specSession.Proposals {
		agent := strings.TrimSpace(proposal.Agent)
		if agent != "" {
			candidates[agent] = struct{}{}
		}
	}

	return candidates
}

func formatSpeculateVerdictArtifact(verdict speculate.Verdict) string {
	var b strings.Builder

	if winner := strings.TrimSpace(verdict.Winner); winner != "" {
		b.WriteString("winner: " + winner + "\n")
	}

	if reason := strings.TrimSpace(verdict.Reason); reason != "" {
		b.WriteString("reason: " + reason + "\n")
	}

	for _, gate := range verdict.GateChecks {
		status := gateStatusFail
		if gate.Passed {
			status = gateStatusPass
		}

		fmt.Fprintf(&b, "gate %s: %s %s\n", strings.TrimSpace(gate.Name), status, strings.TrimSpace(gate.Notes))
	}

	return strings.TrimSpace(b.String())
}

func (r *multiAgentRunRecorder) recordReviewSession(reviewSession review.Session) error {
	if r == nil {
		return nil
	}

	r.mu.Lock()
	defer r.mu.Unlock()

	now := time.Now().UTC()
	rawArtifacts := rawProviderResponseArtifacts(r.run.Artifacts)
	r.run.Artifacts = nil
	r.run.Gates = nil
	r.run.Decisions = nil
	r.run.Errors = nil
	hasVerdict := strings.TrimSpace(reviewSession.Verdict.Reviewer) != "" ||
		len(reviewSession.Verdict.Findings) > 0 ||
		len(reviewSession.Verdict.GateChecks) > 0

	verdictOutcome, verdictRationale := "", ""
	if hasVerdict {
		verdictOutcome, verdictRationale = reviewVerdictDecision(reviewSession)
	}

	for i, report := range reviewSession.Reports {
		r.recordReportLocked(now, report, multiAgentArtifactReviewReport, multiAgentPhaseReviewReport, i+1)
		r.recordReviewReportDecisionLocked(report, i+1, hasVerdict, verdictOutcome, verdictRationale)
	}

	for i, crossReview := range reviewSession.CrossReviews {
		if strings.TrimSpace(crossReview.Reviewer) == "" && strings.TrimSpace(crossReview.Notes) == "" {
			continue
		}

		r.run.Artifacts = append(r.run.Artifacts, session.MultiAgentRunArtifact{
			CreatedAt:   now,
			Kind:        multiAgentArtifactCrossReview,
			Phase:       multiAgentPhaseCrossReview,
			Agent:       crossReview.Reviewer,
			TargetAgent: crossReview.ReviewedReviewer,
			Content:     crossReview.Notes,
			Index:       i + 1,
		})
	}

	r.recordReviewErrorsLocked(reviewSession.Errors)
	r.recordReviewValidationErrorLocked(reviewSession, hasVerdict)
	r.recordReportLocked(now, reviewSession.Verdict, multiAgentArtifactVerdict, multiAgentPhaseAggregateVerdict, 1)

	if hasVerdict {
		r.run.Decisions = append(r.run.Decisions, session.MultiAgentRunDecision{
			Kind:      multiAgentArtifactVerdict,
			Phase:     multiAgentPhaseAggregateVerdict,
			Agent:     reviewSession.Verdict.Reviewer,
			Outcome:   verdictOutcome,
			Rationale: verdictRationale,
			Index:     1,
		})
	}

	r.pendingSummary = acceptedReviewSummary(reviewSession, verdictOutcome)
	r.run.Summary = session.MultiAgentRunSummary{}

	annotateArtifactsWithRawCallMetadata(r.run.Artifacts, rawArtifacts, r.run.Calls)
	r.appendUnrepresentedRawArtifactsLocked(rawArtifacts)

	return r.persistLocked()
}

func (r *multiAgentRunRecorder) recordReviewErrorsLocked(reviewErrors []review.RunError) {
	for _, runError := range reviewErrors {
		r.appendRunErrorLocked(session.MultiAgentRunError{
			Stage:       runError.Stage,
			Reviewer:    runError.Reviewer,
			TargetAgent: runError.ReviewedReviewer,
			Message:     runError.Message,
		})
	}
}

func (r *multiAgentRunRecorder) recordReviewValidationErrorLocked(reviewSession review.Session, hasVerdict bool) {
	if !hasVerdict {
		return
	}

	if err := reviewVerdictValidationError(reviewSession); err != nil {
		rawMessage := err.Error()
		persistedMessage := "aggregate verdict failed validation: " + rawMessage

		if r.runErrorExistsLocked(multiAgentPhaseAggregateVerdict, reviewSession.Verdict.Reviewer, "", rawMessage) ||
			r.runErrorExistsLocked(multiAgentPhaseAggregateVerdict, reviewSession.Verdict.Reviewer, "", persistedMessage) {
			return
		}

		r.appendRunErrorLocked(session.MultiAgentRunError{
			Stage:    multiAgentPhaseAggregateVerdict,
			Reviewer: reviewSession.Verdict.Reviewer,
			Message:  persistedMessage,
		})
	}
}

func (r *multiAgentRunRecorder) appendRunErrorLocked(runError session.MultiAgentRunError) {
	runError.Stage = strings.TrimSpace(runError.Stage)
	runError.Reviewer = strings.TrimSpace(runError.Reviewer)
	runError.TargetAgent = strings.TrimSpace(runError.TargetAgent)
	runError.Message = strings.TrimSpace(runError.Message)

	if runError.Message == "" || r.runErrorExistsLocked(runError.Stage, runError.Reviewer, runError.TargetAgent, runError.Message) {
		return
	}

	r.run.Errors = append(r.run.Errors, runError)
}

func (r *multiAgentRunRecorder) runErrorExistsLocked(stage, reviewer, targetAgent, message string) bool {
	stage = strings.TrimSpace(stage)
	reviewer = strings.TrimSpace(reviewer)
	targetAgent = strings.TrimSpace(targetAgent)
	message = strings.TrimSpace(message)

	for i := range r.run.Errors {
		runError := &r.run.Errors[i]
		if strings.TrimSpace(runError.Stage) == stage &&
			strings.TrimSpace(runError.Reviewer) == reviewer &&
			strings.TrimSpace(runError.TargetAgent) == targetAgent &&
			strings.TrimSpace(runError.Message) == message {
			return true
		}
	}

	return false
}

func acceptedReviewSummary(
	reviewSession review.Session,
	verdictOutcome string,
) session.MultiAgentRunSummary {
	if verdictOutcome != multiAgentDecisionAccepted {
		return session.MultiAgentRunSummary{}
	}

	return session.MultiAgentRunSummary{
		VerdictReviewer: reviewSession.Verdict.Reviewer,
		Findings:        len(reviewSession.Verdict.Findings),
	}
}

func reviewVerdictDecision(reviewSession review.Session) (outcome, rationale string) {
	if err := reviewVerdictValidationError(reviewSession); err != nil {
		return multiAgentDecisionRejected, "aggregate verdict failed validation: " + err.Error()
	}

	return multiAgentDecisionAccepted, "aggregate verdict accepted as final review output"
}

func reviewVerdictValidationError(reviewSession review.Session) error {
	if err := review.ValidateReport(reviewSession.Verdict, reviewSession.Plan.RequiredGates()); err != nil {
		return fmt.Errorf("%w", err)
	}

	return nil
}

func (r *multiAgentRunRecorder) recordReviewReportDecisionLocked(
	report review.Report,
	index int,
	hasVerdict bool,
	verdictOutcome string,
	verdictRationale string,
) {
	if strings.TrimSpace(report.Reviewer) == "" && len(report.Findings) == 0 && len(report.GateChecks) == 0 {
		return
	}

	rationale := multiAgentRationaleStoppedBeforeVerdict
	if hasVerdict && verdictOutcome == multiAgentDecisionAccepted {
		rationale = "superseded by aggregate verdict"
	} else if hasVerdict {
		rationale = verdictRationale
	}

	r.run.Decisions = append(r.run.Decisions, session.MultiAgentRunDecision{
		Kind:      multiAgentArtifactReviewReport,
		Phase:     multiAgentPhaseReviewReport,
		Agent:     report.Reviewer,
		Outcome:   multiAgentDecisionRejected,
		Rationale: rationale,
		Index:     index,
	})
}

func (r *multiAgentRunRecorder) recordReportLocked(
	now time.Time,
	report review.Report,
	kind string,
	phase string,
	index int,
) {
	if strings.TrimSpace(report.Reviewer) == "" && len(report.Findings) == 0 && len(report.GateChecks) == 0 {
		return
	}

	r.run.Artifacts = append(r.run.Artifacts, session.MultiAgentRunArtifact{
		CreatedAt: now,
		Kind:      kind,
		Phase:     phase,
		Agent:     report.Reviewer,
		Content:   review.FormatReport(report),
		Index:     index,
		Metadata: map[string]string{
			"findings": strconv.Itoa(len(report.Findings)),
		},
	})

	for _, gate := range report.GateChecks {
		r.run.Gates = append(r.run.Gates, session.MultiAgentRunGate{
			Name:   gate.Name,
			Phase:  phase,
			Agent:  report.Reviewer,
			Passed: gate.Passed,
			Notes:  gate.Notes,
		})
	}
}

func rawProviderResponseArtifacts(artifacts []session.MultiAgentRunArtifact) []session.MultiAgentRunArtifact {
	raw := make([]session.MultiAgentRunArtifact, 0, len(artifacts))
	for i := range artifacts {
		artifact := artifacts[i]
		if artifact.Metadata[multiAgentRawProviderResponseKey] != multiAgentMetadataTrue {
			continue
		}

		raw = append(raw, artifact)
	}

	return raw
}

func annotateArtifactsWithRawCallMetadata(
	artifacts []session.MultiAgentRunArtifact,
	rawArtifacts []session.MultiAgentRunArtifact,
	calls []session.MultiAgentRunCall,
) {
	callStatuses := multiAgentCallStatuses(calls)

	for i := range artifacts {
		if artifactCallID(artifacts[i]) != "" {
			continue
		}

		bestCallID := ""
		bestScore := -1

		for j := range rawArtifacts {
			if !artifactsRepresentSameRunOutput(rawArtifacts[j], artifacts[i]) {
				continue
			}

			callID := artifactCallID(rawArtifacts[j])
			if callID == "" {
				continue
			}

			score := rawArtifactCallMatchScore(rawArtifacts[j], artifacts[i], callStatuses)
			if score > bestScore {
				bestCallID = callID
				bestScore = score
			}
		}

		if bestCallID == "" {
			continue
		}

		if artifacts[i].Metadata == nil {
			artifacts[i].Metadata = make(map[string]string, 1)
		}

		artifacts[i].Metadata["call_id"] = bestCallID
	}
}

func multiAgentCallStatuses(calls []session.MultiAgentRunCall) map[string]session.MultiAgentRunStatus {
	statuses := make(map[string]session.MultiAgentRunStatus, len(calls))
	for i := range calls {
		call := calls[i]
		if call.ID != "" {
			statuses[call.ID] = call.Status
		}
	}

	return statuses
}

func rawArtifactCallMatchScore(
	raw session.MultiAgentRunArtifact,
	artifact session.MultiAgentRunArtifact,
	callStatuses map[string]session.MultiAgentRunStatus,
) int {
	// Structured artifacts come from the successful run session; prefer completed
	// provider attempts, then use exact content matches to break ties.
	score := 0
	if strings.TrimSpace(raw.Content) == strings.TrimSpace(artifact.Content) {
		score += 4
	}

	if callStatuses[artifactCallID(raw)] == session.MultiAgentRunStatusCompleted {
		score += 8
	}

	return score
}

func (r *multiAgentRunRecorder) appendUnrepresentedRawArtifactsLocked(rawArtifacts []session.MultiAgentRunArtifact) {
	for i := range rawArtifacts {
		raw := rawArtifacts[i]
		if rawArtifactRepresented(raw, r.run.Artifacts) {
			continue
		}

		raw.Index = len(r.run.Artifacts) + 1
		r.run.Artifacts = append(r.run.Artifacts, raw)
		r.recordUnrepresentedRawArtifactDecisionLocked(raw)
	}
}

func (r *multiAgentRunRecorder) recordUnrepresentedRawArtifactDecisionLocked(
	raw session.MultiAgentRunArtifact,
) {
	switch raw.Kind {
	case multiAgentArtifactProposal, multiAgentArtifactReviewReport, multiAgentArtifactVerdict:
	default:
		return
	}

	if artifactDecisionRepresented(raw, r.run.Decisions) {
		return
	}

	rationale := multiAgentRationaleStoppedBeforeVerdict
	if raw.Kind == multiAgentArtifactVerdict {
		rationale = multiAgentRationaleNoAcceptedVerdict
	}

	r.run.Decisions = append(r.run.Decisions, session.MultiAgentRunDecision{
		Kind:        raw.Kind,
		Phase:       raw.Phase,
		Agent:       raw.Agent,
		TargetAgent: raw.TargetAgent,
		Outcome:     multiAgentDecisionRejected,
		Rationale:   rationale,
		Index:       raw.Index,
	})
}

func artifactDecisionRepresented(
	artifact session.MultiAgentRunArtifact,
	decisions []session.MultiAgentRunDecision,
) bool {
	for i := range decisions {
		decision := decisions[i]
		if decision.Kind == artifact.Kind &&
			decision.Phase == artifact.Phase &&
			decision.Agent == artifact.Agent &&
			decision.TargetAgent == artifact.TargetAgent {
			if decision.Index > 0 && artifact.Index > 0 && decision.Index != artifact.Index {
				continue
			}

			return true
		}
	}

	return false
}

func rawArtifactRepresented(raw session.MultiAgentRunArtifact, artifacts []session.MultiAgentRunArtifact) bool {
	for i := range artifacts {
		if artifactsRepresentSameRunOutput(raw, artifacts[i]) {
			return true
		}
	}

	return false
}

func artifactsRepresentSameRunOutput(raw, artifact session.MultiAgentRunArtifact) bool {
	rawCallID := artifactCallID(raw)
	artifactCallID := artifactCallID(artifact)

	if rawCallID != "" && artifactCallID != "" {
		return rawCallID == artifactCallID
	}

	if artifact.Metadata[multiAgentRawProviderResponseKey] == multiAgentMetadataTrue {
		return false
	}

	if raw.Kind == multiAgentArtifactVerdict {
		return artifact.Kind == raw.Kind && artifact.Phase == raw.Phase
	}

	return artifact.Phase == raw.Phase &&
		artifact.Agent == raw.Agent &&
		artifact.TargetAgent == raw.TargetAgent &&
		artifact.Kind == raw.Kind
}

func artifactCallID(artifact session.MultiAgentRunArtifact) string {
	if artifact.Metadata == nil {
		return ""
	}

	return strings.TrimSpace(artifact.Metadata["call_id"])
}

func (r *multiAgentRunRecorder) finish(runErr error) error {
	if r == nil {
		return nil
	}

	r.mu.Lock()
	defer r.mu.Unlock()

	now := time.Now().UTC()
	r.run.CompletedAt = &now
	r.run.Usage.DurationMS = durationMS(r.run.StartedAt, now)

	if runErr != nil {
		r.run.Summary = session.MultiAgentRunSummary{}
	}

	switch {
	case runErr == nil:
		r.run.Status = session.MultiAgentRunStatusCompleted
		r.run.Summary = r.pendingSummary
		r.run.Error = ""
		r.run.CancellationReason = ""
		r.run.ResumeReason = ""
	case isContextCancellation(runErr):
		r.run.Status = session.MultiAgentRunStatusCanceled
		r.run.Error = runErr.Error()
		r.run.CancellationReason = runErr.Error()
		r.run.ResumeReason = multiAgentResumeReason(r.run)
	case isBudgetRejection(runErr):
		r.run.Status = session.MultiAgentRunStatusBudgetExhausted
		r.run.Error = runErr.Error()
		r.run.ResumeReason = multiAgentResumeReason(r.run)
	default:
		r.run.Status = session.MultiAgentRunStatusError
		r.run.Error = runErr.Error()
		r.run.ResumeReason = multiAgentResumeReason(r.run)
	}

	return r.persistLocked()
}

func multiAgentPersistenceError(runErr, persistErr error) error {
	if persistErr == nil {
		return nil
	}

	return errors.Join(runErr, persistErr)
}

func multiAgentResumeReason(run session.MultiAgentRun) string {
	return fmt.Sprintf(
		"continue %s run %s from %d recorded calls, %d artifacts, and %s",
		firstNonEmpty(run.Kind, "multi-agent"),
		firstNonEmpty(run.ReceiptID, run.ID),
		len(run.Calls),
		len(run.Artifacts),
		multiAgentRunStateLabel(run.Status),
	)
}

func multiAgentRunStateLabel(status session.MultiAgentRunStatus) string {
	if status == session.MultiAgentRunStatusRunning {
		return "current state " + string(status)
	}

	return "terminal state " + string(status)
}

func (r *multiAgentRunRecorder) persistLocked() error {
	if r.sessionState == nil {
		return errors.New("multi-agent run: session state is required")
	}

	r.refreshReceiptMetadataLocked()

	if !r.sessionState.UpsertMultiAgentRun(r.run) {
		return errors.New("multi-agent run: run id is required")
	}

	if r.store == nil {
		return nil
	}

	if err := r.store.Save(*r.sessionState); err != nil {
		return fmt.Errorf("persist multi-agent run: %w", err)
	}

	return nil
}

func (r *multiAgentRunRecorder) refreshReceiptMetadataLocked() {
	r.run.Branches = deriveMultiAgentRunBranches(r.run.Calls)
	r.run.Branches = appendArtifactBranches(r.run.Branches, r.run.Artifacts)
	r.run.Reviewers = deriveMultiAgentRunReviewers(r.run.Calls)
	r.run.Reviewers = appendArtifactReviewers(r.run.Reviewers, r.run.Artifacts, r.run.Kind)
	r.run.Disagreements = deriveMultiAgentRunDisagreements(r.run.Artifacts, r.run.Gates)

	if r.run.Status != session.MultiAgentRunStatusCompleted {
		r.run.ResumeReason = multiAgentResumeReason(r.run)
	}
}

func deriveMultiAgentRunBranches(calls []session.MultiAgentRunCall) []session.MultiAgentRunBranch {
	branches := make([]session.MultiAgentRunBranch, 0, len(calls))
	for i := range calls {
		call := &calls[i]
		if !multiAgentBranchPhase(call.Phase) {
			continue
		}

		branches = append(branches, session.MultiAgentRunBranch{
			Name:                 call.Agent,
			Role:                 call.Phase,
			Provenance:           "provider-call:" + call.ID,
			Model:                firstNonEmpty(call.ResponseModel, call.RequestedModel),
			PromptHash:           call.PromptHash,
			Error:                call.Error,
			Status:               call.Status,
			InputTokenEstimate:   call.InputTokenEstimate,
			OutputTokenEstimate:  call.OutputTokenEstimate,
			ContextWindow:        call.ContextWindow,
			MaxOutputTokens:      call.MaxOutputTokens,
			InputTokens:          call.InputTokens,
			CachedInputTokens:    call.CachedInputTokens,
			OutputTokens:         call.OutputTokens,
			TotalTokens:          call.TotalTokens,
			EstimatedCostMicros:  call.EstimatedCostMicros,
			DurationMS:           call.DurationMS,
			BudgetRejectionRule:  call.BudgetRejectionRule,
			BudgetRejectionUsage: call.BudgetRejectionUsage,
			BudgetRejectionLimit: call.BudgetRejectionLimit,
		})
	}

	return branches
}

func appendArtifactBranches(
	branches []session.MultiAgentRunBranch,
	artifacts []session.MultiAgentRunArtifact,
) []session.MultiAgentRunBranch {
	for i := range artifacts {
		artifact := artifacts[i]
		if !multiAgentBranchArtifact(artifact.Kind) {
			continue
		}

		if branchRepresentedByArtifact(branches, artifact) {
			continue
		}

		branches = append(branches, session.MultiAgentRunBranch{
			Name:       artifact.Agent,
			Role:       artifact.Phase,
			Provenance: artifactProvenance(artifact),
			Status:     session.MultiAgentRunStatusCompleted,
		})
	}

	return branches
}

func multiAgentBranchArtifact(kind string) bool {
	return kind == multiAgentArtifactProposal || kind == multiAgentArtifactReviewReport
}

func branchRepresentedByArtifact(
	branches []session.MultiAgentRunBranch,
	artifact session.MultiAgentRunArtifact,
) bool {
	for i := range branches {
		branch := &branches[i]
		if branch.Name == artifact.Agent && branch.Role == artifact.Phase {
			return true
		}
	}

	return false
}

func artifactProvenance(artifact session.MultiAgentRunArtifact) string {
	if callID := artifactCallID(artifact); callID != "" {
		return "provider-call:" + callID
	}

	if artifact.Index > 0 {
		return fmt.Sprintf("artifact:%s:%d", artifact.Kind, artifact.Index)
	}

	return "artifact:" + artifact.Kind
}

func multiAgentBranchPhase(phase string) bool {
	return phase == multiAgentPhaseProposal || phase == multiAgentPhaseReviewReport
}

func deriveMultiAgentRunReviewers(calls []session.MultiAgentRunCall) []session.MultiAgentRunReviewer {
	reviewers := make([]session.MultiAgentRunReviewer, 0, len(calls))
	for i := range calls {
		call := &calls[i]
		if call.Phase == multiAgentPhaseProposal {
			continue
		}

		reviewers = append(reviewers, session.MultiAgentRunReviewer{
			Name:        call.Agent,
			Role:        call.Phase,
			TargetAgent: call.TargetAgent,
			Model:       firstNonEmpty(call.ResponseModel, call.RequestedModel),
			PromptHash:  call.PromptHash,
			CallID:      call.ID,
		})
	}

	return reviewers
}

func appendArtifactReviewers(
	reviewers []session.MultiAgentRunReviewer,
	artifacts []session.MultiAgentRunArtifact,
	runKind string,
) []session.MultiAgentRunReviewer {
	for i := range artifacts {
		artifact := artifacts[i]
		if !multiAgentReviewerArtifact(artifact.Kind, runKind) {
			continue
		}

		if runKind == session.MultiAgentRunKindReview &&
			artifact.Kind == multiAgentArtifactVerdict &&
			reviewerRoleRepresented(reviewers, artifact.Phase) {
			continue
		}

		if reviewerRepresentedByArtifact(reviewers, artifact) {
			continue
		}

		reviewers = append(reviewers, session.MultiAgentRunReviewer{
			Name:        artifact.Agent,
			Role:        artifact.Phase,
			TargetAgent: artifact.TargetAgent,
			CallID:      artifactCallID(artifact),
		})
	}

	return reviewers
}

func multiAgentReviewerArtifact(kind, runKind string) bool {
	if runKind == session.MultiAgentRunKindSpeculation && kind == multiAgentArtifactVerdict {
		return false
	}

	return kind == multiAgentArtifactReviewReport ||
		kind == multiAgentArtifactCrossReview ||
		kind == multiAgentArtifactVerdict
}

func reviewerRoleRepresented(reviewers []session.MultiAgentRunReviewer, role string) bool {
	for i := range reviewers {
		if reviewers[i].Role == role {
			return true
		}
	}

	return false
}

func reviewerRepresentedByArtifact(
	reviewers []session.MultiAgentRunReviewer,
	artifact session.MultiAgentRunArtifact,
) bool {
	callID := artifactCallID(artifact)

	for i := range reviewers {
		reviewer := &reviewers[i]
		if callID != "" && reviewer.CallID == callID {
			return true
		}

		if reviewer.Name == artifact.Agent &&
			reviewer.Role == artifact.Phase &&
			reviewer.TargetAgent == artifact.TargetAgent {
			return true
		}
	}

	return false
}

func deriveMultiAgentRunDisagreements(
	artifacts []session.MultiAgentRunArtifact,
	gates []session.MultiAgentRunGate,
) []session.MultiAgentRunDisagreement {
	disagreements := make([]session.MultiAgentRunDisagreement, 0, len(artifacts))
	disagreements = append(disagreements, crossReviewDisagreements(artifacts)...)
	disagreements = append(disagreements, gateDisagreements(gates)...)

	return disagreements
}

func crossReviewDisagreements(artifacts []session.MultiAgentRunArtifact) []session.MultiAgentRunDisagreement {
	disagreements := make([]session.MultiAgentRunDisagreement, 0, len(artifacts))
	for i := range artifacts {
		artifact := artifacts[i]
		if artifact.Kind != multiAgentArtifactCrossReview || strings.TrimSpace(artifact.Content) == "" {
			continue
		}

		disagreements = append(disagreements, session.MultiAgentRunDisagreement{
			Phase:       artifact.Phase,
			Reviewer:    artifact.Agent,
			TargetAgent: artifact.TargetAgent,
			Subject:     "cross-review",
			Notes:       artifact.Content,
			Index:       artifact.Index,
		})
	}

	return disagreements
}

func gateDisagreements(gates []session.MultiAgentRunGate) []session.MultiAgentRunDisagreement {
	type gateStatus struct {
		pass []string
		fail []string
	}

	byName := make(map[string]gateStatus)

	order := make([]string, 0, len(gates))
	for _, gate := range gates {
		name := strings.TrimSpace(gate.Name)
		if name == "" {
			continue
		}

		status := byName[name]
		if len(status.pass) == 0 && len(status.fail) == 0 {
			order = append(order, name)
		}

		agent := firstNonEmpty(gate.Agent, gate.Phase, "unknown")
		if gate.Passed {
			status.pass = append(status.pass, agent)
		} else {
			status.fail = append(status.fail, agent)
		}

		byName[name] = status
	}

	disagreements := make([]session.MultiAgentRunDisagreement, 0, len(order))
	for _, name := range order {
		status := byName[name]
		if len(status.pass) == 0 || len(status.fail) == 0 {
			continue
		}

		disagreements = append(disagreements, session.MultiAgentRunDisagreement{
			Phase:   multiAgentPhaseAggregateVerdict,
			Subject: "gate:" + name,
			Notes:   "pass=" + strings.Join(status.pass, ",") + " fail=" + strings.Join(status.fail, ","),
			Index:   len(disagreements) + 1,
		})
	}

	return disagreements
}

func splitPromptMessages(messages []llm.Message) (systemPrompt, userPrompt string) {
	var systemParts []string

	var userParts []string

	for _, message := range messages {
		switch message.Role {
		case llm.RoleSystem:
			systemParts = append(systemParts, message.Content)
		case llm.RoleUser:
			userParts = append(userParts, message.Content)
		}
	}

	return strings.Join(systemParts, "\n\n"), strings.Join(userParts, "\n\n")
}

func targetAfter(value, prefix, suffix string) string {
	_, rest, ok := strings.Cut(value, prefix)
	if !ok {
		return ""
	}

	if suffix != "" {
		if before, _, ok := strings.Cut(rest, suffix); ok {
			rest = before
		}
	}

	return strings.TrimSpace(rest)
}

func budgetRejectionMessage(rule string, used, limit int) string {
	return fmt.Sprintf("%s exceeded: used %d of %d", rule, used, limit)
}

type multiAgentBudgetError struct {
	rule  string
	used  int
	limit int
}

func newMultiAgentBudgetError(rule string, used, limit int) error {
	return multiAgentBudgetError{rule: rule, used: used, limit: limit}
}

func multiAgentBudgetErrorWithPersistence(rule string, used, limit int, persistErr error) error {
	budgetErr := newMultiAgentBudgetError(rule, used, limit)
	if persistErr == nil {
		return budgetErr
	}

	return errors.Join(budgetErr, persistErr)
}

func (e multiAgentBudgetError) Error() string {
	return budgetRejectionMessage(e.rule, e.used, e.limit)
}

func isBudgetRejection(err error) bool {
	var budgetErr multiAgentBudgetError
	return errors.As(err, &budgetErr)
}

func isContextCancellation(err error) bool {
	return errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded)
}

func contextErr(ctx context.Context) error {
	if ctx == nil {
		return nil
	}

	if err := ctx.Err(); err != nil {
		return fmt.Errorf("multi-agent run context: %w", err)
	}

	return nil
}

func contextWithMultiAgentRunBudget(ctx context.Context, budget session.MultiAgentRunBudget) (context.Context, context.CancelFunc) {
	if budget.MaxRunWallTimeMS <= 0 {
		return ctx, func() {}
	}

	return context.WithTimeout(ctx, time.Duration(budget.MaxRunWallTimeMS)*time.Millisecond)
}

func multiAgentRunErrorForBudgetContext(
	parentCtx context.Context,
	runCtx context.Context,
	runErr error,
	budget session.MultiAgentRunBudget,
	startedAt time.Time,
) error {
	if budget.MaxRunWallTimeMS <= 0 || !errors.Is(contextErr(runCtx), context.DeadlineExceeded) {
		return runErr
	}

	if runErr != nil && !isContextCancellation(runErr) {
		return runErr
	}

	if parentCtx != nil && parentCtx.Err() != nil {
		return runErr
	}

	used := intFromInt64(durationMS(startedAt, time.Now().UTC()))

	limit := intFromInt64(budget.MaxRunWallTimeMS)
	if used < limit {
		used = limit
	}

	return newMultiAgentBudgetError(multiAgentBudgetRuleRunWallTime, used, limit)
}

func promptHash(systemPrompt, userPrompt string) string {
	if systemPrompt == "" && userPrompt == "" {
		return ""
	}

	sum := sha256.Sum256([]byte(systemPrompt + "\x00" + userPrompt))

	return fmt.Sprintf("sha256:%x", sum)
}

func durationMS(start, end time.Time) int64 {
	if start.IsZero() || end.IsZero() || end.Before(start) {
		return 0
	}

	return end.Sub(start).Milliseconds()
}

func listMultiAgentRuns(sessionState session.Session) {
	if len(sessionState.MultiAgentRuns) == 0 {
		fmt.Println("No multi-agent runs recorded.")
		return
	}

	for i := range sessionState.MultiAgentRuns {
		fmt.Println(formatMultiAgentRunSummary(sessionState.MultiAgentRuns[i]))
	}
}

func formatMultiAgentRunSummary(run session.MultiAgentRun) string {
	started := "-"
	if !run.StartedAt.IsZero() {
		started = run.StartedAt.UTC().Format(time.RFC3339)
	}

	parts := []string{
		multiAgentSummaryPart("id", run.ID),
		multiAgentSummaryPart("receipt_id", firstNonEmpty(run.ReceiptID, run.ID)),
		multiAgentSummaryPart("kind", run.Kind),
		multiAgentSummaryPart("status", string(run.Status)),
		multiAgentSummaryPart("started_at", started),
		"calls=" + strconv.Itoa(len(run.Calls)),
		"artifacts=" + strconv.Itoa(len(run.Artifacts)),
		"gates=" + strconv.Itoa(len(run.Gates)),
		"errors=" + strconv.Itoa(len(run.Errors)),
	}
	if multiAgentRunHasAcceptedOutput(run) {
		if run.Summary.Winner != "" {
			parts = append(parts, multiAgentSummaryPart("winner", run.Summary.Winner))
		}

		if run.Summary.VerdictReviewer != "" {
			parts = append(parts, multiAgentSummaryPart("verdict_reviewer", run.Summary.VerdictReviewer))
		}
	}

	if run.Error != "" {
		parts = append(parts, multiAgentSummaryPart("error", run.Error))
	}

	return strings.Join(parts, "\t")
}

func multiAgentSummaryPart(key, value string) string {
	return key + "=" + replayContent(value)
}

func showMultiAgentRun(sessionState session.Session, ref string) error {
	run, err := selectMultiAgentRun(sessionState, ref)
	if err != nil {
		return err
	}

	data, err := yaml.Marshal(run)
	if err != nil {
		return fmt.Errorf("show multi-agent run %q: %w", ref, err)
	}

	fmt.Print(string(data))

	return nil
}

func exportMultiAgentRun(sessionState session.Session, ref, format string) error {
	run, err := selectMultiAgentRun(sessionState, ref)
	if err != nil {
		return err
	}

	switch strings.ToLower(strings.TrimSpace(format)) {
	case "text", "replay":
		fmt.Print(formatMultiAgentRunReplay(run))
	case "resume":
		fmt.Print(formatMultiAgentRunResume(run))
	default:
		if err := exportSession(sessionForMultiAgentRunExport(sessionState, run), format); err != nil {
			return fmt.Errorf("export multi-agent run %q: %w", ref, err)
		}
	}

	return nil
}

func sessionForMultiAgentRunExport(sessionState session.Session, run session.MultiAgentRun) session.Session {
	return session.Session{
		ID:                    sessionState.ID,
		Title:                 sessionState.Title,
		CreatedAt:             sessionState.CreatedAt,
		UpdatedAt:             sessionState.UpdatedAt,
		DefaultAgent:          sessionState.DefaultAgent,
		DefaultModel:          sessionState.DefaultModel,
		DefaultReasoningLevel: sessionState.DefaultReasoningLevel,
		WorktreePath:          sessionState.WorktreePath,
		WorktreeBranch:        sessionState.WorktreeBranch,
		WorktreeBase:          sessionState.WorktreeBase,
		Tags:                  append([]string(nil), sessionState.Tags...),
		MultiAgentRuns:        []session.MultiAgentRun{run},
	}
}

func replayMultiAgentRun(sessionState session.Session, ref string) error {
	run, err := selectMultiAgentRun(sessionState, ref)
	if err != nil {
		return err
	}

	fmt.Print(formatMultiAgentRunReplay(run))

	return nil
}

func resumeMultiAgentRun(sessionState session.Session, ref string) error {
	run, err := selectMultiAgentRun(sessionState, ref)
	if err != nil {
		return err
	}

	fmt.Print(formatMultiAgentRunResume(run))

	return nil
}

func selectMultiAgentRun(sessionState session.Session, ref string) (session.MultiAgentRun, error) {
	ref = strings.TrimSpace(ref)
	if ref == "" {
		ref = multiAgentReplayRefLatest
	}

	run, ok := sessionState.FindMultiAgentRun(ref)
	if !ok {
		return session.MultiAgentRun{}, fmt.Errorf("multi-agent run %q not found on session %s", ref, sessionState.ID)
	}

	return run, nil
}

func formatMultiAgentRunResume(run session.MultiAgentRun) string {
	var b strings.Builder
	b.WriteString("resume_source: recorded_artifacts\n")
	b.WriteString("provider_calls: skipped\n")

	if run.Status == session.MultiAgentRunStatusRunning {
		b.WriteString("current_state: " + string(run.Status) + "\n")
	} else if run.Status != "" {
		b.WriteString("terminal_state: " + string(run.Status) + "\n")
	}

	if run.ResumeReason != "" {
		b.WriteString("resume_reason: " + replayContent(run.ResumeReason) + "\n")
	} else {
		b.WriteString("resume_reason: " + replayContent(defaultMultiAgentResumeReason(run)) + "\n")
	}

	writeMultiAgentResumeCursor(&b, run)
	writeMultiAgentResumeOutput(&b, run)

	b.WriteString(formatMultiAgentRunReplay(run))

	return b.String()
}

func writeMultiAgentResumeCursor(b *strings.Builder, run session.MultiAgentRun) {
	fmt.Fprintf(
		b,
		"resume_cursor: recorded_calls=%d\tartifacts=%d\tdecisions=%d\tgates=%d\terrors=%d\n",
		len(run.Calls),
		len(run.Artifacts),
		len(run.Decisions),
		len(run.Gates),
		len(run.Errors),
	)

	if call, ok := lastMultiAgentRunCall(run.Calls); ok {
		parts := []string{
			storedKeyValue("id", call.ID),
			storedKeyValue("phase", call.Phase),
			storedKeyValue("status", string(call.Status)),
		}

		if call.Agent != "" {
			parts = append(parts, storedKeyValue("agent", call.Agent))
		}

		if call.TargetAgent != "" {
			parts = append(parts, storedKeyValue("target", call.TargetAgent))
		}

		if call.Error != "" {
			parts = append(parts, "error="+replayContent(call.Error))
		}

		fmt.Fprintf(b, "last_call: %s\n", strings.Join(parts, "\t"))
	}

	if nextAction := multiAgentResumeNextAction(run); nextAction != "" {
		b.WriteString("next_action: " + replayContent(nextAction) + "\n")
	}
}

func lastMultiAgentRunCall(calls []session.MultiAgentRunCall) (session.MultiAgentRunCall, bool) {
	for i := len(calls) - 1; i >= 0; i-- {
		if calls[i].ID != "" || calls[i].Phase != "" {
			return calls[i], true
		}
	}

	return session.MultiAgentRunCall{}, false
}

func multiAgentResumeNextAction(run session.MultiAgentRun) string {
	if run.Status == session.MultiAgentRunStatusCompleted {
		if !multiAgentRunHasAcceptedOutput(run) {
			return "inspect completed receipt; accepted aggregate output is not recorded"
		}

		return "inspect or export the completed decision trail"
	}

	if call, ok := lastMultiAgentRunCallWithStatus(run.Calls, session.MultiAgentRunStatusBudgetExhausted); ok {
		return "resolve budget rejection at " + resumeCallLabel(call)
	}

	if run.Status == session.MultiAgentRunStatusBudgetExhausted {
		return "resolve run-level budget before starting provider calls"
	}

	if call, ok := lastMultiAgentRunCallWithStatus(run.Calls, session.MultiAgentRunStatusError); ok {
		return "inspect or retry failed " + resumeCallLabel(call)
	}

	if call, ok := lastMultiAgentRunCallWithStatus(run.Calls, session.MultiAgentRunStatusCanceled); ok {
		return "retry canceled " + resumeCallLabel(call) + " or aggregate recorded partial output"
	}

	if run.Status == session.MultiAgentRunStatusError {
		return "inspect run-level error before retrying provider calls"
	}

	if run.Status == session.MultiAgentRunStatusCanceled {
		return "inspect cancellation and aggregate recorded partial output before accepting final output"
	}

	if run.Status == session.MultiAgentRunStatusRunning {
		if len(run.Calls) == 0 && len(run.Artifacts) == 0 {
			return "start pending provider calls from recorded request"
		}

		if _, ok := acceptedAggregateArtifact(run); ok {
			return "finalize accepted aggregate output from recorded receipt without provider calls"
		}

		return "continue remaining workflow phases from recorded state"
	}

	if !hasAcceptedAggregateDecision(run.Decisions) {
		return "aggregate recorded artifacts before accepting final output"
	}

	return "continue from durable receipt without replaying provider calls"
}

func lastMultiAgentRunCallWithStatus(
	calls []session.MultiAgentRunCall,
	status session.MultiAgentRunStatus,
) (session.MultiAgentRunCall, bool) {
	for i := len(calls) - 1; i >= 0; i-- {
		if calls[i].Status == status {
			return calls[i], true
		}
	}

	return session.MultiAgentRunCall{}, false
}

func resumeCallLabel(call session.MultiAgentRunCall) string {
	parts := []string{}
	if call.Phase != "" {
		parts = append(parts, call.Phase)
	}

	if call.Agent != "" {
		parts = append(parts, "agent "+call.Agent)
	}

	if call.ID != "" {
		parts = append(parts, "call "+call.ID)
	}

	if len(parts) == 0 {
		return "recorded provider call"
	}

	return strings.Join(parts, " ")
}

func hasAcceptedAggregateDecision(decisions []session.MultiAgentRunDecision) bool {
	for _, decision := range decisions {
		if decision.Kind == multiAgentArtifactVerdict && decision.Outcome == multiAgentDecisionAccepted {
			return true
		}
	}

	return false
}

func multiAgentRunHasAcceptedOutput(run session.MultiAgentRun) bool {
	if run.Status != session.MultiAgentRunStatusCompleted {
		return false
	}

	artifact, ok := acceptedAggregateArtifact(run)
	if !ok {
		return false
	}

	return strings.TrimSpace(artifact.Content) != ""
}

func writeMultiAgentResumeOutput(b *strings.Builder, run session.MultiAgentRun) {
	if writeMultiAgentCompletedResumeOutput(b, run) {
		return
	}

	artifacts := resumablePartialArtifacts(run)
	if len(artifacts) == 0 {
		return
	}

	b.WriteString("resumable_artifacts:\n")

	for _, artifact := range artifacts {
		writeResumeArtifactLine(b, artifact)
	}
}

func writeMultiAgentCompletedResumeOutput(b *strings.Builder, run session.MultiAgentRun) bool {
	if run.Status != session.MultiAgentRunStatusCompleted {
		return false
	}

	if artifact, ok := acceptedAggregateArtifact(run); ok {
		if strings.TrimSpace(artifact.Content) == "" {
			return false
		}

		b.WriteString("resumed_output:\n")
		writeResumeArtifactLine(b, artifact)

		return true
	}

	return false
}

func acceptedAggregateArtifact(run session.MultiAgentRun) (session.MultiAgentRunArtifact, bool) {
	var accepted session.MultiAgentRunDecision

	foundDecision := false

	for _, decision := range run.Decisions {
		if decision.Kind != multiAgentArtifactVerdict || decision.Outcome != multiAgentDecisionAccepted {
			continue
		}

		accepted = decision
		foundDecision = true

		break
	}

	if !foundDecision {
		return session.MultiAgentRunArtifact{}, false
	}

	var legacyMatch session.MultiAgentRunArtifact

	foundLegacyMatch := false

	for i := range run.Artifacts {
		artifact := run.Artifacts[i]

		if !aggregateArtifactMatchesDecision(artifact, accepted) {
			continue
		}

		if accepted.Index > 0 && artifact.Index == 0 {
			legacyMatch = artifact
			foundLegacyMatch = true

			continue
		}

		if accepted.Index > 0 && artifact.Index != accepted.Index {
			continue
		}

		return artifact, true
	}

	return legacyMatch, foundLegacyMatch
}

func aggregateArtifactMatchesDecision(
	artifact session.MultiAgentRunArtifact,
	decision session.MultiAgentRunDecision,
) bool {
	if artifact.Kind != multiAgentArtifactVerdict {
		return false
	}

	if decision.Phase != "" && artifact.Phase != decision.Phase {
		return false
	}

	if decision.Agent != "" && artifact.Agent != decision.Agent {
		return false
	}

	if decision.TargetAgent != "" && artifact.TargetAgent != decision.TargetAgent {
		return false
	}

	return true
}

func resumablePartialArtifacts(run session.MultiAgentRun) []session.MultiAgentRunArtifact {
	artifacts := make([]session.MultiAgentRunArtifact, 0, len(run.Artifacts))
	for _, artifact := range run.Artifacts {
		if strings.TrimSpace(artifact.Content) == "" {
			continue
		}

		artifacts = append(artifacts, artifact)
	}

	return artifacts
}

func writeResumeArtifactLine(b *strings.Builder, artifact session.MultiAgentRunArtifact) {
	parts := []string{storedKeyValue("source", artifactProvenance(artifact))}
	parts = append(parts, storedArtifactParts(artifact)...)

	fmt.Fprintf(b, "  - %s\n", strings.Join(parts, "\t"))
}

func defaultMultiAgentResumeReason(run session.MultiAgentRun) string {
	receipt := firstNonEmpty(run.ReceiptID, run.ID)
	if run.Status == session.MultiAgentRunStatusCompleted {
		return "run already completed; replay durable receipt " + receipt
	}

	return "continue from durable receipt " + receipt
}

func formatMultiAgentRunReplay(run session.MultiAgentRun) string {
	switch run.Kind {
	case session.MultiAgentRunKindReview:
		return formatStoredReviewRun(run)
	case session.MultiAgentRunKindSpeculation:
		return formatStoredSpeculationRun(run)
	default:
		return formatStoredGenericRun(run)
	}
}

func formatStoredSpeculationRun(run session.MultiAgentRun) string {
	var b strings.Builder
	b.WriteString("run: " + replayContent(run.ID) + "\n")
	b.WriteString("status: " + string(run.Status) + "\n")
	writeStoredRunRequest(&b, run)

	if multiAgentRunHasAcceptedOutput(run) {
		if run.Summary.Winner != "" {
			b.WriteString("winner: " + replayContent(run.Summary.Winner) + "\n")
		}

		if run.Summary.Reason != "" {
			b.WriteString("reason: " + replayContent(run.Summary.Reason) + "\n")
		}
	}

	writeStoredBranches(&b, run.Branches)
	writeStoredReviewers(&b, run.Reviewers)
	writeStoredArtifacts(&b, "proposals", run.Artifacts, multiAgentArtifactProposal)
	writeStoredArtifacts(&b, "reviews", run.Artifacts, multiAgentArtifactCrossReview)
	writeStoredArtifacts(&b, "aggregate_verdict", run.Artifacts, multiAgentArtifactVerdict)
	writeStoredDisagreements(&b, run.Disagreements)
	writeStoredRunErrors(&b, run.Errors)
	writeStoredDecisions(&b, run.Decisions)
	writeStoredGates(&b, run.Gates)
	writeStoredCalls(&b, run.Calls)
	writeStoredError(&b, run)

	return b.String()
}

func formatStoredReviewRun(run session.MultiAgentRun) string {
	var b strings.Builder
	b.WriteString("run: " + replayContent(run.ID) + "\n")
	b.WriteString("status: " + string(run.Status) + "\n")
	writeStoredRunRequest(&b, run)

	if multiAgentRunHasAcceptedOutput(run) {
		if run.Summary.VerdictReviewer != "" {
			b.WriteString("verdict_reviewer: " + replayContent(run.Summary.VerdictReviewer) + "\n")
		}

		if run.Summary.Findings > 0 {
			b.WriteString("findings: " + strconv.Itoa(run.Summary.Findings) + "\n")
		}
	}

	writeStoredBranches(&b, run.Branches)
	writeStoredReviewers(&b, run.Reviewers)
	writeStoredArtifacts(&b, "independent_reports", run.Artifacts, multiAgentArtifactReviewReport)
	writeStoredArtifacts(&b, "cross_reviews", run.Artifacts, multiAgentArtifactCrossReview)
	writeStoredArtifacts(&b, "aggregate_report", run.Artifacts, multiAgentArtifactVerdict)
	writeStoredDisagreements(&b, run.Disagreements)
	writeStoredRunErrors(&b, run.Errors)
	writeStoredDecisions(&b, run.Decisions)
	writeStoredGates(&b, run.Gates)
	writeStoredCalls(&b, run.Calls)
	writeStoredError(&b, run)

	return b.String()
}

func formatStoredGenericRun(run session.MultiAgentRun) string {
	var b strings.Builder
	b.WriteString(formatMultiAgentRunSummary(run))
	b.WriteByte('\n')
	writeStoredRunRequest(&b, run)
	writeStoredBranches(&b, run.Branches)
	writeStoredReviewers(&b, run.Reviewers)
	writeStoredArtifacts(&b, "artifacts", run.Artifacts, "")
	writeStoredDisagreements(&b, run.Disagreements)
	writeStoredRunErrors(&b, run.Errors)
	writeStoredDecisions(&b, run.Decisions)
	writeStoredGates(&b, run.Gates)
	writeStoredCalls(&b, run.Calls)
	writeStoredError(&b, run)

	return b.String()
}

func writeStoredRunRequest(b *strings.Builder, run session.MultiAgentRun) {
	if run.ReceiptID != "" {
		b.WriteString("receipt_id: " + replayContent(run.ReceiptID) + "\n")
	}

	if run.Prompt != "" {
		b.WriteString("prompt: " + replayContent(run.Prompt) + "\n")
	}

	if run.Model != "" {
		b.WriteString("model: " + replayContent(run.Model) + "\n")
	}

	if len(run.FallbackModels) > 0 {
		b.WriteString("fallback_models: " + replayContent(strings.Join(run.FallbackModels, ",")) + "\n")
	}

	writeStoredRunBudget(b, run.Budget)
	writeStoredRunUsage(b, run.Usage)
}

func writeStoredRunUsage(b *strings.Builder, usage session.MultiAgentRunUsage) {
	if multiAgentRunUsageEmpty(usage) {
		return
	}

	fmt.Fprintf(
		b,
		"usage: model_calls=%d\tcompleted_calls=%d\tcanceled_calls=%d\tprovider_failed_calls=%d\tbudget_rejected_calls=%d\testimated_input_tokens=%d\testimated_output_tokens=%d\testimated_total_tokens=%d\testimated_cost_micros=%d\tinput_tokens=%d\tcached_input_tokens=%d\toutput_tokens=%d\ttotal_tokens=%d\tduration_ms=%d\n",
		usage.ModelCalls,
		usage.CompletedCalls,
		usage.CanceledCalls,
		usage.ProviderFailedCalls,
		usage.BudgetRejectedCalls,
		usage.EstimatedInputTokens,
		usage.EstimatedOutputTokens,
		usage.EstimatedTotalTokens,
		usage.EstimatedCostMicros,
		usage.InputTokens,
		usage.CachedInputTokens,
		usage.OutputTokens,
		usage.TotalTokens,
		usage.DurationMS,
	)
}

func multiAgentRunUsageEmpty(usage session.MultiAgentRunUsage) bool {
	return usage.ModelCalls == 0 && usage.CompletedCalls == 0 && usage.CanceledCalls == 0 &&
		usage.ProviderFailedCalls == 0 && usage.BudgetRejectedCalls == 0 &&
		usage.EstimatedInputTokens == 0 && usage.EstimatedOutputTokens == 0 &&
		usage.EstimatedTotalTokens == 0 && usage.EstimatedCostMicros == 0 &&
		usage.InputTokens == 0 && usage.CachedInputTokens == 0 &&
		usage.OutputTokens == 0 && usage.TotalTokens == 0 && usage.DurationMS == 0
}

func writeStoredRunBudget(b *strings.Builder, budget session.MultiAgentRunBudget) {
	if budget.PerCallMaxInputTokens == 0 &&
		budget.PerCallMaxOutputTokens == 0 &&
		budget.MaxRunInputTokens == 0 &&
		budget.MaxRunOutputTokens == 0 &&
		budget.MaxRunTotalTokens == 0 &&
		budget.MaxModelCalls == 0 &&
		budget.MaxRunCostMicros == 0 &&
		budget.MaxRunWallTimeMS == 0 {
		return
	}

	fmt.Fprintf(
		b,
		"budget: per_call_max_input_tokens=%d\tper_call_max_output_tokens=%d\tmax_run_input_tokens=%d\tmax_run_output_tokens=%d\tmax_run_total_tokens=%d\tmax_model_calls=%d\tmax_run_cost_micros=%d\tmax_run_wall_time_ms=%d\n",
		budget.PerCallMaxInputTokens,
		budget.PerCallMaxOutputTokens,
		budget.MaxRunInputTokens,
		budget.MaxRunOutputTokens,
		budget.MaxRunTotalTokens,
		budget.MaxModelCalls,
		budget.MaxRunCostMicros,
		budget.MaxRunWallTimeMS,
	)
}

func writeStoredArtifacts(b *strings.Builder, heading string, artifacts []session.MultiAgentRunArtifact, kind string) {
	filtered := make([]session.MultiAgentRunArtifact, 0, len(artifacts))
	for _, artifact := range artifacts {
		if kind == "" || artifact.Kind == kind {
			filtered = append(filtered, artifact)
		}
	}

	if len(filtered) == 0 {
		return
	}

	b.WriteString(heading + ":\n")

	for _, artifact := range filtered {
		fmt.Fprintf(b, "  - %s\n", strings.Join(storedArtifactParts(artifact), "\t"))
	}
}

func storedArtifactParts(artifact session.MultiAgentRunArtifact) []string {
	parts := []string{storedKeyValue("kind", artifact.Kind)}
	if artifact.Phase != "" {
		parts = append(parts, storedKeyValue("phase", artifact.Phase))
	}

	if artifact.Agent != "" {
		parts = append(parts, storedKeyValue("agent", artifact.Agent))
	}

	if artifact.TargetAgent != "" {
		parts = append(parts, storedKeyValue("target", artifact.TargetAgent))
	}

	if artifact.Index > 0 {
		parts = append(parts, "index="+strconv.Itoa(artifact.Index))
	}

	if artifact.Content != "" {
		parts = append(parts, "content="+replayContent(artifact.Content))
	}

	return appendStoredMetadataParts(parts, artifact.Metadata)
}

func appendStoredMetadataParts(parts []string, metadata map[string]string) []string {
	if len(metadata) == 0 {
		return parts
	}

	type metadataEntry struct {
		key   string
		value string
	}

	entries := make([]metadataEntry, 0, len(metadata))
	for key, value := range metadata {
		displayKey := strings.TrimSpace(key)
		if displayKey != "" {
			entries = append(entries, metadataEntry{key: displayKey, value: value})
		}
	}

	sort.Slice(entries, func(i, j int) bool {
		return entries[i].key < entries[j].key
	})

	for _, entry := range entries {
		parts = append(parts, storedKeyValue("metadata."+entry.key, entry.value))
	}

	return parts
}

func storedKeyValue(key, value string) string {
	return replayContent(key) + "=" + replayContent(value)
}

func writeStoredBranches(b *strings.Builder, branches []session.MultiAgentRunBranch) {
	if len(branches) == 0 {
		return
	}

	b.WriteString("branches:\n")

	for i := range branches {
		fmt.Fprintf(b, "  - %s\n", strings.Join(storedBranchParts(&branches[i]), "\t"))
	}
}

func storedBranchParts(branch *session.MultiAgentRunBranch) []string {
	parts := []string{
		storedKeyValue("name", branch.Name),
		storedKeyValue("role", branch.Role),
		storedKeyValue("status", string(branch.Status)),
	}

	if branch.Model != "" {
		parts = append(parts, storedKeyValue("model", branch.Model))
	}

	if branch.Provenance != "" {
		parts = append(parts, storedKeyValue("provenance", branch.Provenance))
	}

	if branch.PromptHash != "" {
		parts = append(parts, storedKeyValue("prompt_hash", branch.PromptHash))
	}

	parts = append(parts,
		"input_estimate="+strconv.Itoa(branch.InputTokenEstimate),
		"output_estimate="+strconv.Itoa(branch.OutputTokenEstimate),
	)

	if branch.ContextWindow > 0 {
		parts = append(parts, "context_window="+strconv.Itoa(branch.ContextWindow))
	}

	if branch.MaxOutputTokens > 0 {
		parts = append(parts, "max_output_tokens="+strconv.Itoa(branch.MaxOutputTokens))
	}

	if branch.InputTokens > 0 ||
		branch.CachedInputTokens > 0 ||
		branch.OutputTokens > 0 ||
		branch.TotalTokens > 0 {
		parts = append(parts,
			"input_tokens="+strconv.Itoa(branch.InputTokens),
			"cached_input_tokens="+strconv.Itoa(branch.CachedInputTokens),
			"output_tokens="+strconv.Itoa(branch.OutputTokens),
			"total_tokens="+strconv.Itoa(branch.TotalTokens),
		)
	}

	if branch.EstimatedCostMicros > 0 {
		parts = append(parts, "estimated_cost_micros="+strconv.FormatInt(branch.EstimatedCostMicros, 10))
	}

	if branch.DurationMS > 0 {
		parts = append(parts, "duration_ms="+strconv.FormatInt(branch.DurationMS, 10))
	}

	parts = appendOptionalStoredBranchBudget(parts, branch)

	if branch.Error != "" {
		parts = append(parts, "error="+replayContent(branch.Error))
	}

	return parts
}

func appendOptionalStoredBranchBudget(parts []string, branch *session.MultiAgentRunBranch) []string {
	if branch.BudgetRejectionRule == "" {
		return parts
	}

	return append(parts,
		storedKeyValue("budget_rejection", branch.BudgetRejectionRule),
		"budget_used="+strconv.Itoa(branch.BudgetRejectionUsage),
		"budget_limit="+strconv.Itoa(branch.BudgetRejectionLimit),
	)
}

func writeStoredReviewers(b *strings.Builder, reviewers []session.MultiAgentRunReviewer) {
	if len(reviewers) == 0 {
		return
	}

	b.WriteString("reviewers:\n")

	for _, reviewer := range reviewers {
		parts := []string{
			storedKeyValue("name", reviewer.Name),
			storedKeyValue("role", reviewer.Role),
		}

		if reviewer.TargetAgent != "" {
			parts = append(parts, storedKeyValue("target", reviewer.TargetAgent))
		}

		if reviewer.Model != "" {
			parts = append(parts, storedKeyValue("model", reviewer.Model))
		}

		if reviewer.PromptHash != "" {
			parts = append(parts, storedKeyValue("prompt_hash", reviewer.PromptHash))
		}

		if reviewer.CallID != "" {
			parts = append(parts, storedKeyValue("call", reviewer.CallID))
		}

		fmt.Fprintf(b, "  - %s\n", strings.Join(parts, "\t"))
	}
}

func writeStoredCalls(b *strings.Builder, calls []session.MultiAgentRunCall) {
	if len(calls) == 0 {
		return
	}

	b.WriteString("recorded_calls:\n")

	for i := range calls {
		fmt.Fprintf(b, "  - %s\n", strings.Join(storedCallParts(&calls[i]), "\t"))
	}
}

func storedCallParts(call *session.MultiAgentRunCall) []string {
	parts := []string{
		storedKeyValue("id", call.ID),
		storedKeyValue("phase", call.Phase),
		storedKeyValue("status", string(call.Status)),
	}

	parts = appendOptionalStoredCallIdentity(parts, call)
	parts = append(parts,
		"input_estimate="+strconv.Itoa(call.InputTokenEstimate),
		"output_estimate="+strconv.Itoa(call.OutputTokenEstimate),
	)
	parts = appendOptionalStoredCallLimits(parts, call)
	parts = appendOptionalStoredCallUsage(parts, call)
	parts = appendOptionalStoredCallBudget(parts, call)
	parts = appendOptionalStoredCallContent(parts, call)

	if call.Error != "" {
		parts = append(parts, "error="+replayContent(call.Error))
	}

	return parts
}

func appendOptionalStoredCallIdentity(parts []string, call *session.MultiAgentRunCall) []string {
	if call.Agent != "" {
		parts = append(parts, storedKeyValue("agent", call.Agent))
	}

	if call.TargetAgent != "" {
		parts = append(parts, storedKeyValue("target", call.TargetAgent))
	}

	if model := firstNonEmpty(call.ResponseModel, call.RequestedModel); model != "" {
		parts = append(parts, storedKeyValue("model", model))
	}

	if call.RequestedModel != "" {
		parts = append(parts, storedKeyValue("requested_model", call.RequestedModel))
	}

	if call.ResponseModel != "" {
		parts = append(parts, storedKeyValue("response_model", call.ResponseModel))
	}

	if call.PromptHash != "" {
		parts = append(parts, storedKeyValue("prompt_hash", call.PromptHash))
	}

	if len(call.FallbackModels) > 0 {
		parts = append(parts, storedKeyValue("fallback_models", strings.Join(call.FallbackModels, ",")))
	}

	return parts
}

func appendOptionalStoredCallLimits(parts []string, call *session.MultiAgentRunCall) []string {
	if call.MaxOutputTokens > 0 {
		parts = append(parts, "max_output_tokens="+strconv.Itoa(call.MaxOutputTokens))
	}

	if call.ContextWindow > 0 {
		parts = append(parts, "context_window="+strconv.Itoa(call.ContextWindow))
	}

	return parts
}

func appendOptionalStoredCallUsage(parts []string, call *session.MultiAgentRunCall) []string {
	if call.InputTokens > 0 || call.CachedInputTokens > 0 || call.OutputTokens > 0 || call.TotalTokens > 0 {
		parts = append(parts,
			"input_tokens="+strconv.Itoa(call.InputTokens),
			"cached_input_tokens="+strconv.Itoa(call.CachedInputTokens),
			"output_tokens="+strconv.Itoa(call.OutputTokens),
			"total_tokens="+strconv.Itoa(call.TotalTokens),
		)
	}

	if call.DurationMS > 0 {
		parts = append(parts, "duration_ms="+strconv.FormatInt(call.DurationMS, 10))
	}

	if call.EstimatedCostMicros > 0 {
		parts = append(parts, "estimated_cost_micros="+strconv.FormatInt(call.EstimatedCostMicros, 10))
	}

	return parts
}

func appendOptionalStoredCallBudget(parts []string, call *session.MultiAgentRunCall) []string {
	if call.BudgetRejectionRule == "" {
		return parts
	}

	return append(parts,
		storedKeyValue("budget_rejection", call.BudgetRejectionRule),
		"budget_used="+strconv.Itoa(call.BudgetRejectionUsage),
		"budget_limit="+strconv.Itoa(call.BudgetRejectionLimit),
	)
}

func appendOptionalStoredCallContent(parts []string, call *session.MultiAgentRunCall) []string {
	if call.SystemPrompt != "" {
		parts = append(parts, "system_prompt="+replayContent(call.SystemPrompt))
	}

	if call.UserPrompt != "" {
		parts = append(parts, "user_prompt="+replayContent(call.UserPrompt))
	}

	if call.Response != "" {
		parts = append(parts, "response="+replayContent(call.Response))
	}

	return parts
}

func writeStoredDisagreements(b *strings.Builder, disagreements []session.MultiAgentRunDisagreement) {
	if len(disagreements) == 0 {
		return
	}

	b.WriteString("disagreements:\n")

	for _, disagreement := range disagreements {
		parts := []string{}
		if disagreement.Phase != "" {
			parts = append(parts, storedKeyValue("phase", disagreement.Phase))
		}

		if disagreement.Reviewer != "" {
			parts = append(parts, storedKeyValue("reviewer", disagreement.Reviewer))
		}

		if disagreement.TargetAgent != "" {
			parts = append(parts, storedKeyValue("target", disagreement.TargetAgent))
		}

		if disagreement.Subject != "" {
			parts = append(parts, storedKeyValue("subject", disagreement.Subject))
		}

		if disagreement.Index > 0 {
			parts = append(parts, "index="+strconv.Itoa(disagreement.Index))
		}

		if disagreement.Notes != "" {
			parts = append(parts, "notes="+replayContent(disagreement.Notes))
		}

		fmt.Fprintf(b, "  - %s\n", strings.Join(parts, "\t"))
	}
}

func writeStoredDecisions(b *strings.Builder, decisions []session.MultiAgentRunDecision) {
	if len(decisions) == 0 {
		return
	}

	b.WriteString("decisions:\n")

	for _, decision := range decisions {
		parts := []string{
			storedKeyValue("kind", decision.Kind),
			storedKeyValue("outcome", decision.Outcome),
		}

		if decision.Phase != "" {
			parts = append(parts, storedKeyValue("phase", decision.Phase))
		}

		if decision.Agent != "" {
			parts = append(parts, storedKeyValue("agent", decision.Agent))
		}

		if decision.TargetAgent != "" {
			parts = append(parts, storedKeyValue("target", decision.TargetAgent))
		}

		if decision.Index > 0 {
			parts = append(parts, "index="+strconv.Itoa(decision.Index))
		}

		if decision.Rationale != "" {
			parts = append(parts, "rationale="+replayContent(decision.Rationale))
		}

		fmt.Fprintf(b, "  - %s\n", strings.Join(parts, "\t"))
	}
}

func writeStoredRunErrors(b *strings.Builder, runErrors []session.MultiAgentRunError) {
	if len(runErrors) == 0 {
		return
	}

	b.WriteString("workflow_errors:\n")

	for _, runError := range runErrors {
		parts := []string{}
		if runError.Stage != "" {
			parts = append(parts, storedKeyValue("stage", runError.Stage))
		}

		if runError.Reviewer != "" {
			parts = append(parts, storedKeyValue("reviewer", runError.Reviewer))
		}

		if runError.TargetAgent != "" {
			parts = append(parts, storedKeyValue("target", runError.TargetAgent))
		}

		if runError.Message != "" {
			parts = append(parts, "message="+replayContent(runError.Message))
		}

		fmt.Fprintf(b, "  - %s\n", strings.Join(parts, "\t"))
	}
}

func replayContent(value string) string {
	var b strings.Builder

	for i := 0; i < len(value); i++ {
		switch value[i] {
		case '\\':
			b.WriteString(`\\`)
		case '\r':
			if i+1 < len(value) && value[i+1] == '\n' {
				b.WriteString(`\n`)

				i++
			} else {
				b.WriteString(`\r`)
			}
		case '\n':
			b.WriteString(`\n`)
		case '\t':
			b.WriteString(`\t`)
		default:
			if value[i] < 0x20 || value[i] == 0x7f {
				fmt.Fprintf(&b, `\x%02x`, value[i])
			} else {
				b.WriteByte(value[i])
			}
		}
	}

	return b.String()
}

func writeStoredGates(b *strings.Builder, gates []session.MultiAgentRunGate) {
	if len(gates) == 0 {
		return
	}

	b.WriteString("gates:\n")

	for _, gate := range gates {
		status := gateStatusFail
		if gate.Passed {
			status = gateStatusPass
		}

		parts := []string{storedKeyValue("name", gate.Name), storedKeyValue("status", status)}
		if gate.Phase != "" {
			parts = append(parts, storedKeyValue("phase", gate.Phase))
		}

		if gate.Agent != "" {
			parts = append(parts, storedKeyValue("agent", gate.Agent))
		}

		if gate.Notes != "" {
			parts = append(parts, "notes="+replayContent(gate.Notes))
		}

		fmt.Fprintf(b, "  - %s\n", strings.Join(parts, "\t"))
	}
}

func writeStoredError(b *strings.Builder, run session.MultiAgentRun) {
	if run.CancellationReason != "" {
		b.WriteString("cancellation_reason: " + replayContent(run.CancellationReason) + "\n")
	}

	if run.Error != "" {
		b.WriteString("error: " + replayContent(run.Error) + "\n")
	}
}
