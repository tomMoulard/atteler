package llm

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/tommoulard/atteler/pkg/autonomy"
)

// ToolExecutor executes a single tool call and returns the result content.
// Implementations should handle the tool by name and return the textual
// output. If the tool fails, return is_error=true with the error message
// as content.
type ToolExecutor func(ctx context.Context, call ToolCall) ToolResult

// AgentLoopConfig configures the multi-turn agentic completion loop.
//
//nolint:govet // Field order groups callbacks, policy, budget, checkpoint, replay, and legacy knobs.
type AgentLoopConfig struct {
	OnToolCall   func(call ToolCall)
	OnToolResult func(call ToolCall, result ToolResult)
	OnContent    func(content string)

	// BeforeModelCall is invoked immediately before each model call with the
	// exact request that will be sent. Returning an error stops the loop before
	// the provider sees the request; callers can use this for token preflight
	// checks and audit manifests.
	BeforeModelCall func(iteration int, params CompleteParams) error

	// ConfirmContinue is called when the legacy human checkpoint interval is
	// reached. Durable checkpoints are emitted per loop step through
	// CheckpointSink; this callback is only for user-facing continuation gates.
	ConfirmContinue func(iterations int) bool

	// ConfirmToolCall is invoked when Policy returns require-confirm. If nil, a
	// require-confirm decision stops the loop without executing the tool.
	ConfirmToolCall ConfirmToolCallFunc

	// Policy decides whether a requested tool call may execute. Nil defaults to a
	// conservative explicit allow verdict that is still recorded in checkpoints.
	Policy ToolPolicy

	// Budget is the hard-stop envelope for iterations, wall time, model calls,
	// tool calls, output bytes, tokens, and optional estimated cost.
	Budget AgentLoopBudget

	// Autonomy records the selected risk-based action boundary for checkpoints.
	// Tool enforcement lives in Policy so callers can combine autonomy with
	// provider-specific or UI confirmation gates.
	Autonomy autonomy.Level

	// EstimateCostMicros is required only when Budget.MaxCostMicros is set.
	EstimateCostMicros AgentLoopCostEstimator

	// CheckpointSink receives durable, structured ledger records for every model
	// response, tool decision/result, and stop condition.
	CheckpointSink AgentLoopCheckpointSink

	// ReplaySteps are previously recorded checkpoint steps to apply to the chat
	// state before continuing the live loop. This allows callers to resume after
	// a failed or interrupted run without re-executing already-recorded tools.
	ReplaySteps []AgentLoopStep

	// Now is injectable for tests. Nil uses time.Now.
	Now func() time.Time

	// MaxIterations is a legacy shortcut for Budget.MaxIterations.
	MaxIterations int

	// CheckpointInterval is the number of completed tool-use iterations
	// between ConfirmContinue prompts. Zero or negative disables the prompt
	// entirely — the loop runs without asking the user to confirm continuation.
	CheckpointInterval int

	// MaxHistoryToolResultBytes caps each tool result appended back into model
	// chat history. Full output is still recorded in checkpoint steps. A
	// non-positive value uses a safe default.
	MaxHistoryToolResultBytes int
}

//nolint:govet // Field order keeps lifecycle, transcript, budget, and sequencing state together.
type agentLoopState struct {
	startedAt time.Time
	messages  []Message
	budget    AgentLoopBudget
	autonomy  autonomy.Level
	usage     AgentLoopUsage
	now       func() time.Time
	sequence  int
}

// AgentLoop runs a multi-turn completion loop where the LLM can request
// tool executions. It keeps calling Complete until the model stops asking
// for tools (StopReason != StopToolUse) or a configured budget/policy cap is
// hit.
//
// The loop records structured checkpoint steps for model responses, every tool
// policy verdict/result, and final stop conditions when CheckpointSink is set.
// Full tool output is split from chat history: checkpoint records receive the
// full ToolResult, while the prompt history receives a bounded result string.
//
//nolint:cyclop,gocognit // AgentLoop is the orchestration boundary for budget, policy, checkpoint, and model control flow.
func AgentLoop(
	ctx context.Context,
	reg *Registry,
	params CompleteParams,
	fallbackModels []string,
	executor ToolExecutor,
	cfg AgentLoopConfig,
) (*Response, []Message, error) {
	if reg == nil {
		return nil, nil, errors.New("llm: agent loop registry is required")
	}

	if cfg.Budget.MaxCostMicros > 0 && cfg.EstimateCostMicros == nil {
		return nil, nil, errors.New("llm: agent loop cost budget requires EstimateCostMicros")
	}

	state := newAgentLoopState(params.Messages, cfg)
	if err := state.applyReplay(ctx, cfg); err != nil {
		return nil, state.messages, err
	}

	checkpoint := max(cfg.CheckpointInterval, 0)

	policy := cfg.Policy
	if policy == nil {
		policy = defaultToolPolicy
	}

	historyToolLimit := cfg.MaxHistoryToolResultBytes
	if historyToolLimit <= 0 {
		historyToolLimit = defaultMaxHistoryToolBytes
	}

	for {
		state.refreshElapsed()

		if err := ctx.Err(); err != nil {
			return nil, state.messages, fmt.Errorf("llm: agent loop canceled: %w", err)
		}

		if cond := modelCallBudgetStop(state.budget, state.usage); cond != nil {
			if err := state.recordStop(ctx, cfg.CheckpointSink, *cond); err != nil {
				return nil, state.messages, err
			}

			return nil, state.messages, stopConditionError(*cond)
		}

		if cond := budgetExhaustedStop(state.budget, state.usage); cond != nil {
			if err := state.recordStop(ctx, cfg.CheckpointSink, *cond); err != nil {
				return nil, state.messages, err
			}

			return nil, state.messages, stopConditionError(*cond)
		}

		if err := checkpointGate(state.usage.Iterations, checkpoint, cfg.ConfirmContinue); err != nil {
			cond := AgentLoopStopCondition{
				Kind:        AgentLoopStopUserCheckpoint,
				Reason:      err.Error(),
				MatchedRule: "checkpoint.confirm_continue",
			}
			if recordErr := state.recordStop(ctx, cfg.CheckpointSink, cond); recordErr != nil {
				return nil, state.messages, recordErr
			}

			return nil, state.messages, err
		}

		iterParams := params

		iterParams.Messages = append([]Message(nil), state.messages...)
		if cfg.BeforeModelCall != nil {
			if err := cfg.BeforeModelCall(state.usage.Iterations, iterParams); err != nil {
				cond := AgentLoopStopCondition{
					Kind:        AgentLoopStopModelError,
					Reason:      fmt.Sprintf("model request rejected on iteration %d: %v", state.usage.Iterations, err),
					MatchedRule: "model.before_call",
				}
				if recordErr := state.recordStop(ctx, cfg.CheckpointSink, cond); recordErr != nil {
					return nil, state.messages, recordErr
				}

				return nil, state.messages, fmt.Errorf("llm: agent loop iteration %d preflight: %w", state.usage.Iterations, err)
			}
		}

		requestSummary := summarizeModelRequest(iterParams, fallbackModels)

		resp, err := reg.CompleteWithFallback(ctx, iterParams, fallbackModels)
		if err != nil {
			cond := AgentLoopStopCondition{
				Kind:        AgentLoopStopModelError,
				Reason:      fmt.Sprintf("model call failed on iteration %d: %v", state.usage.Iterations, err),
				MatchedRule: "model.complete",
				Metadata:    fallbackFailureMetadata(err),
			}
			if recordErr := state.recordStop(ctx, cfg.CheckpointSink, cond); recordErr != nil {
				return nil, state.messages, recordErr
			}

			return nil, state.messages, fmt.Errorf("llm: agent loop iteration %d: %w", state.usage.Iterations, err)
		}

		previousCostMicros := state.usage.EstimatedCostMicros
		usageErr := state.usage.addResponse(resp, state.budget, cfg.EstimateCostMicros)
		responseCostMicros := state.usage.EstimatedCostMicros - previousCostMicros
		state.refreshElapsed()

		if err := state.recordModelResponse(ctx, cfg.CheckpointSink, requestSummary, resp, responseCostMicros); err != nil {
			return nil, state.messages, err
		}

		if usageErr != nil {
			cond := usageErrorStopCondition(usageErr, state.budget)
			if err := state.recordStop(ctx, cfg.CheckpointSink, cond); err != nil {
				return nil, state.messages, err
			}

			return nil, state.messages, stopConditionError(cond)
		}

		if resp.Content != "" && cfg.OnContent != nil {
			cfg.OnContent(resp.Content)
		}

		if cond := budgetExceededStop(state.budget, state.usage); cond != nil {
			if err := state.recordStop(ctx, cfg.CheckpointSink, *cond); err != nil {
				return nil, state.messages, err
			}

			return nil, state.messages, stopConditionError(*cond)
		}

		if !resp.WantsToolUse() {
			resp.InputTokens = state.usage.InputTokens
			resp.CachedInputTokens = state.usage.CachedInputTokens
			resp.CacheWriteInputTokens = state.usage.CacheWriteTokens
			resp.OutputTokens = state.usage.OutputTokens

			cond := AgentLoopStopCondition{
				Kind:        AgentLoopStopFinalResponse,
				Reason:      "model returned final response",
				MatchedRule: "model.stop_reason",
			}
			if err := state.recordStop(ctx, cfg.CheckpointSink, cond); err != nil {
				return nil, state.messages, err
			}

			return resp, state.messages, nil
		}

		if executor == nil {
			cond := AgentLoopStopCondition{
				Kind:        AgentLoopStopPolicyDenied,
				Reason:      "tool executor is required when model requests tools",
				MatchedRule: "tool.executor",
			}
			if err := state.recordStop(ctx, cfg.CheckpointSink, cond); err != nil {
				return nil, state.messages, err
			}

			return nil, state.messages, stopConditionError(cond)
		}

		state.messages = append(state.messages, Message{
			Role:      RoleAssistant,
			Content:   resp.Content,
			ToolCalls: append([]ToolCall(nil), resp.ToolCalls...),
		})

		for callIndex, call := range resp.ToolCalls {
			stop, err := state.executeToolCall(
				ctx,
				cfg.CheckpointSink,
				executor,
				policy,
				cfg.ConfirmToolCall,
				historyToolLimit,
				call,
				callIndex,
				cfg,
			)
			if err != nil {
				return nil, state.messages, err
			}

			if stop != nil {
				if err := state.recordStop(ctx, cfg.CheckpointSink, *stop); err != nil {
					return nil, state.messages, err
				}

				return nil, state.messages, stopConditionError(*stop)
			}
		}

		state.usage.Iterations++
	}
}

func usageErrorStopCondition(err error, budget AgentLoopBudget) AgentLoopStopCondition {
	if errors.Is(err, ErrAgentLoopTokenUsageUnavailable) {
		matchedRule := agentLoopTokenBudgetMatchedRule(budget)

		var usageErr agentLoopTokenUsageError
		if errors.As(err, &usageErr) {
			matchedRule = agentLoopTokenUsageMatchedRule(usageErr.field, budget)
		}

		return AgentLoopStopCondition{
			Kind:        AgentLoopStopTokenBudget,
			Reason:      fmt.Sprintf("token budget could not be enforced: %v", err),
			MatchedRule: matchedRule,
		}
	}

	return AgentLoopStopCondition{
		Kind:        AgentLoopStopCostBudget,
		Reason:      fmt.Sprintf("cost budget could not be enforced: %v", err),
		MatchedRule: "budget.max_cost_micros",
	}
}

func agentLoopTokenBudgetMatchedRule(budget AgentLoopBudget) string {
	switch {
	case budget.MaxInputTokens > 0:
		return agentLoopBudgetRuleMaxInputTokens
	case budget.MaxOutputTokens > 0:
		return agentLoopBudgetRuleMaxOutputTokens
	case budget.MaxTotalTokens > 0:
		return agentLoopBudgetRuleMaxTotalTokens
	default:
		return agentLoopBudgetRuleMaxTotalTokens
	}
}

func agentLoopTokenUsageMatchedRule(field agentLoopTokenUsageField, budget AgentLoopBudget) string {
	switch field {
	case agentLoopTokenUsageFieldInput:
		return agentLoopInputUsageMatchedRule(budget)
	case agentLoopTokenUsageFieldOutput:
		return agentLoopOutputUsageMatchedRule(budget)
	default:
		return agentLoopTokenBudgetMatchedRule(budget)
	}
}

func newAgentLoopState(messages []Message, cfg AgentLoopConfig) *agentLoopState {
	now := cfg.Now
	if now == nil {
		now = time.Now
	}

	startedAt := now()

	return &agentLoopState{
		startedAt: startedAt,
		messages:  append([]Message(nil), messages...),
		budget:    normalizeAgentLoopBudget(cfg.Budget, cfg.MaxIterations),
		autonomy:  autonomy.Normalize(cfg.Autonomy),
		now:       now,
	}
}

//nolint:cyclop // Tool policy verdict handling is deliberately explicit and auditable.
func (s *agentLoopState) executeToolCall(
	ctx context.Context,
	sink AgentLoopCheckpointSink,
	executor ToolExecutor,
	policy ToolPolicy,
	confirm ConfirmToolCallFunc,
	historyToolLimit int,
	call ToolCall,
	callIndex int,
	cfg AgentLoopConfig,
) (*AgentLoopStopCondition, error) {
	s.refreshElapsed()
	budget := s.budgetSnapshot()

	if cond := s.preToolStopCondition(); cond != nil {
		budget = s.budgetSnapshot()
		decision := budgetDenyPolicy(cond.Reason, cond.MatchedRule)
		result := ToolResult{
			ToolCallID: call.ID,
			Content:    "tool call denied: " + cond.Reason,
			IsError:    true,
		}
		s.appendPromptToolResult(result, historyToolLimit)

		if err := s.recordToolCall(ctx, sink, call, callIndex, decision, budget, result, result); err != nil {
			return nil, err
		}

		return cond, nil
	}

	decision := normalizeToolPolicyDecision(call, policy(ctx, call, budget))

	s.usage.ToolCalls++

	if cfg.OnToolCall != nil {
		cfg.OnToolCall(call)
	}

	toolCtx := contextWithAgentLoopBudgetSnapshot(ctx, budget)
	if budget.RemainingWallTime > 0 {
		var cancel context.CancelFunc

		toolCtx, cancel = context.WithTimeout(toolCtx, budget.RemainingWallTime)
		defer cancel()
	}

	var result ToolResult

	var stop *AgentLoopStopCondition

	switch decision.Verdict {
	case ToolPolicyAllow:
		result = executor(toolCtx, call)
	case ToolPolicyRequireConfirm:
		if confirm == nil || !confirm(toolCtx, call, decision) {
			result = ToolResult{
				ToolCallID: call.ID,
				Content:    "tool call requires confirmation and was not executed: " + decision.Reason,
				IsError:    true,
			}
			stop = &AgentLoopStopCondition{
				Kind:        AgentLoopStopConfirmationRequired,
				Reason:      result.Content,
				MatchedRule: decision.MatchedRule,
			}
		} else {
			decision.Confirmed = true
			result = executor(toolCtx, call)
		}
	case ToolPolicyDeny:
		result = ToolResult{
			ToolCallID: call.ID,
			Content:    "tool call denied by policy: " + decision.Reason,
			IsError:    true,
		}
		stop = &AgentLoopStopCondition{
			Kind:        AgentLoopStopPolicyDenied,
			Reason:      result.Content,
			MatchedRule: decision.MatchedRule,
		}
	case ToolPolicyDryRun:
		result = ToolResult{
			ToolCallID: call.ID,
			Content:    "dry run: tool call was not executed: " + decision.Reason,
		}
	default:
		result = ToolResult{
			ToolCallID: call.ID,
			Content:    fmt.Sprintf("tool call denied by policy: unsupported verdict %q", decision.Verdict),
			IsError:    true,
		}
		stop = &AgentLoopStopCondition{
			Kind:        AgentLoopStopPolicyDenied,
			Reason:      result.Content,
			MatchedRule: "policy.verdict",
		}
	}

	if result.ToolCallID == "" {
		result.ToolCallID = call.ID
	}

	s.usage.OutputBytes += int64(len([]byte(result.Content)))
	s.refreshElapsed()

	promptResult := promptToolResult(result, historyToolLimit)
	s.messages = append(s.messages, Message{
		Role:       RoleTool,
		Content:    promptResult.Content,
		ToolResult: &promptResult,
	})

	if cfg.OnToolResult != nil {
		cfg.OnToolResult(call, result)
	}

	if err := s.recordToolCall(ctx, sink, call, callIndex, decision, budget, result, promptResult); err != nil {
		return nil, err
	}

	if stop != nil {
		return stop, nil
	}

	if cond := budgetExceededStop(s.budget, s.usage); cond != nil {
		return cond, nil
	}

	return nil, nil
}

func (s *agentLoopState) preToolStopCondition() *AgentLoopStopCondition {
	s.refreshElapsed()

	if s.budget.MaxToolCalls > 0 && s.usage.ToolCalls >= s.budget.MaxToolCalls {
		return &AgentLoopStopCondition{
			Kind:        AgentLoopStopMaxToolCalls,
			Reason:      fmt.Sprintf("tool call budget exhausted: used %d of %d", s.usage.ToolCalls, s.budget.MaxToolCalls),
			MatchedRule: "budget.max_tool_calls",
		}
	}

	return budgetExhaustedStop(s.budget, s.usage)
}

func (s *agentLoopState) appendPromptToolResult(result ToolResult, historyToolLimit int) {
	promptResult := promptToolResult(result, historyToolLimit)
	s.messages = append(s.messages, Message{
		Role:       RoleTool,
		Content:    promptResult.Content,
		ToolResult: &promptResult,
	})
}

func (s *agentLoopState) refreshElapsed() {
	s.usage.Elapsed = s.now().Sub(s.startedAt)
}

func (s *agentLoopState) budgetSnapshot() AgentLoopBudgetSnapshot {
	s.refreshElapsed()

	var remainingDuration time.Duration
	if s.budget.MaxWallTime > 0 {
		remainingDuration = s.budget.MaxWallTime - s.usage.Elapsed
		remainingDuration = max(remainingDuration, 0)
	}

	return AgentLoopBudgetSnapshot{
		Budget:                s.budget,
		Used:                  s.usage,
		RemainingWallTime:     remainingDuration,
		RemainingOutputBytes:  remainingInt64(s.budget.MaxOutputBytes, s.usage.OutputBytes),
		RemainingCostMicros:   remainingInt64(s.budget.MaxCostMicros, s.usage.EstimatedCostMicros),
		RemainingIterations:   remainingInt(s.budget.MaxIterations, s.usage.Iterations),
		RemainingModelCalls:   remainingInt(s.budget.MaxModelCalls, s.usage.ModelCalls),
		RemainingToolCalls:    remainingInt(s.budget.MaxToolCalls, s.usage.ToolCalls),
		RemainingInputTokens:  remainingInt(s.budget.MaxInputTokens, s.usage.InputTokens),
		RemainingOutputTokens: remainingInt(s.budget.MaxOutputTokens, s.usage.OutputTokens),
		RemainingTotalTokens:  remainingInt(s.budget.MaxTotalTokens, s.usage.TotalTokens),
	}
}

func remainingInt(limit, used int) int {
	if limit <= 0 {
		return 0
	}

	remaining := limit - used
	if remaining < 0 {
		return 0
	}

	return remaining
}

func remainingInt64(limit, used int64) int64 {
	if limit <= 0 {
		return 0
	}

	remaining := limit - used
	if remaining < 0 {
		return 0
	}

	return remaining
}

func (s *agentLoopState) recordModelResponse(
	ctx context.Context,
	sink AgentLoopCheckpointSink,
	request AgentLoopModelRequestSummary,
	resp *Response,
	estimatedCostMicros int64,
) error {
	if sink == nil {
		return nil
	}

	step := AgentLoopStep{
		Kind:         AgentLoopStepModelResponse,
		Iteration:    s.usage.Iterations,
		Autonomy:     s.autonomy,
		Budget:       s.budget,
		ModelRequest: &request,
		ModelResponse: &AgentLoopModelResponseSummary{
			Metadata:            cloneStringMap(resp.metadata),
			Model:               resp.Model,
			Provider:            resp.Provider,
			StopReason:          resp.StopReason,
			Content:             resp.Content,
			ContentBytes:        len([]byte(resp.Content)),
			ToolCalls:           append([]ToolCall(nil), resp.ToolCalls...),
			EstimatedCostMicros: max(0, estimatedCostMicros),
			LatencyMS:           agentLoopDurationMS(resp.Latency),
			FirstTokenLatencyMS: agentLoopDurationMS(resp.FirstTokenLatency),
			InputTokens:         resp.InputTokens,
			CachedInputTokens:   resp.CachedInputTokens,
			CacheWriteTokens:    resp.CacheWriteInputTokens,
			OutputTokens:        resp.OutputTokens,
		},
		Usage: s.usage,
	}

	return s.saveStep(ctx, sink, step)
}

func (s *agentLoopState) recordToolCall(
	ctx context.Context,
	sink AgentLoopCheckpointSink,
	call ToolCall,
	callIndex int,
	decision ToolPolicyDecision,
	budget AgentLoopBudgetSnapshot,
	result ToolResult,
	promptResult ToolResult,
) error {
	if sink == nil {
		return nil
	}

	callCopy := call
	decisionCopy := decision
	budgetCopy := budget
	resultCopy := result
	promptCopy := promptResult
	step := AgentLoopStep{
		Kind:          AgentLoopStepToolCall,
		Iteration:     s.usage.Iterations,
		Autonomy:      s.autonomy,
		Budget:        s.budget,
		ToolCallIndex: callIndex,
		ToolCall:      &callCopy,
		Policy:        &decisionCopy,
		ToolBudget:    &budgetCopy,
		ToolResult:    &resultCopy,
		PromptResult:  &promptCopy,
		Usage:         s.usage,
	}

	return s.saveStep(ctx, sink, step)
}

func (s *agentLoopState) recordStop(ctx context.Context, sink AgentLoopCheckpointSink, cond AgentLoopStopCondition) error {
	if sink == nil {
		return nil
	}

	condCopy := cond
	step := AgentLoopStep{
		Kind:          AgentLoopStepStop,
		Iteration:     s.usage.Iterations,
		Autonomy:      s.autonomy,
		Budget:        s.budget,
		Usage:         s.usage,
		StopCondition: &condCopy,
	}

	return s.saveStep(ctx, sink, step)
}

func (s *agentLoopState) saveStep(ctx context.Context, sink AgentLoopCheckpointSink, step AgentLoopStep) error {
	s.sequence++

	step.Sequence = s.sequence

	step.At = s.now().UTC()
	if err := sink.SaveAgentLoopStep(ctx, step); err != nil {
		return fmt.Errorf("llm: save agent loop checkpoint: %w", err)
	}

	return nil
}

//nolint:gocognit // Replay rebuilds transcript state from the small set of checkpoint record kinds.
func (s *agentLoopState) applyReplay(ctx context.Context, cfg AgentLoopConfig) error {
	for i := range cfg.ReplaySteps {
		step := &cfg.ReplaySteps[i]

		if err := ctx.Err(); err != nil {
			return fmt.Errorf("llm: agent loop replay canceled: %w", err)
		}

		if step.Sequence > s.sequence {
			s.sequence = step.Sequence
		}

		if step.Usage.ModelCalls > 0 || step.Usage.ToolCalls > 0 || step.Usage.Iterations > 0 {
			s.usage = step.Usage
		}

		switch step.Kind {
		case AgentLoopStepModelResponse:
			if step.ModelResponse != nil && len(step.ModelResponse.ToolCalls) > 0 {
				s.messages = append(s.messages, Message{
					Role:      RoleAssistant,
					Content:   step.ModelResponse.Content,
					ToolCalls: append([]ToolCall(nil), step.ModelResponse.ToolCalls...),
				})
			}
		case AgentLoopStepToolCall:
			if step.PromptResult != nil {
				result := *step.PromptResult
				s.messages = append(s.messages, Message{Role: RoleTool, Content: result.Content, ToolResult: &result})
			} else if step.ToolResult != nil {
				result := promptToolResult(*step.ToolResult, historyLimitFromConfig(cfg))
				s.messages = append(s.messages, Message{Role: RoleTool, Content: result.Content, ToolResult: &result})
			}

			if step.Iteration >= s.usage.Iterations {
				s.usage.Iterations = step.Iteration + 1
			}
		case AgentLoopStepStop:
			// Stop records are audit-only for resume; the caller decides the new budget.
		}
	}

	return nil
}

func historyLimitFromConfig(cfg AgentLoopConfig) int {
	if cfg.MaxHistoryToolResultBytes > 0 {
		return cfg.MaxHistoryToolResultBytes
	}

	return defaultMaxHistoryToolBytes
}

// checkpointGate asks the caller whether to continue when a checkpoint
// interval is reached. Returns nil to continue, or an error to stop.
func checkpointGate(iteration, interval int, confirm func(int) bool) error {
	if iteration == 0 || interval <= 0 || iteration%interval != 0 || confirm == nil {
		return nil
	}

	if !confirm(iteration) {
		return fmt.Errorf("llm: agent loop stopped by user after %d iterations", iteration)
	}

	return nil
}

func summarizeModelRequest(params CompleteParams, fallbackModels []string) AgentLoopModelRequestSummary {
	toolNames := make([]string, 0, len(params.Tools))
	for _, tool := range params.Tools {
		toolNames = append(toolNames, tool.Name)
	}

	messageBytes := 0
	for _, message := range params.Messages {
		messageBytes += len([]byte(message.Content))
		if message.ToolResult != nil {
			messageBytes += len([]byte(message.ToolResult.Content))
		}
	}

	return AgentLoopModelRequestSummary{
		Model:          params.Model,
		ModelMode:      params.ModelMode,
		FallbackModels: append([]string(nil), fallbackModels...),
		ToolNames:      toolNames,
		MessageCount:   len(params.Messages),
		MessageBytes:   messageBytes,
		MaxTokens:      params.MaxTokens,
	}
}

func agentLoopDurationMS(d time.Duration) int {
	if d <= 0 {
		return 0
	}

	return int(d / time.Millisecond)
}

func promptToolResult(result ToolResult, limit int) ToolResult {
	if limit <= 0 || len([]byte(result.Content)) <= limit {
		return result
	}

	originalBytes := len([]byte(result.Content))
	noticeText := truncatedToolOutputNotice(originalBytes)

	notice := "\n\n" + noticeText
	if len([]byte(notice)) >= limit {
		result.Content = truncateUTF8Bytes(noticeText, limit)

		return result
	}

	contentLimit := limit - len([]byte(notice))
	content := truncateUTF8Bytes(result.Content, contentLimit)
	content = strings.TrimRight(content, "\n")
	content += notice
	result.Content = content

	return result
}

func truncatedToolOutputNotice(originalBytes int) string {
	return fmt.Sprintf(
		"[atteler: tool output truncated from %d bytes; full output is recorded in the agent loop checkpoint ledger]",
		originalBytes,
	)
}

func truncateUTF8Bytes(value string, limit int) string {
	if limit <= 0 || len([]byte(value)) <= limit {
		return value
	}

	cut := limit
	for cut > 0 && !utf8.ValidString(value[:cut]) {
		cut--
	}

	if cut <= 0 {
		return ""
	}

	return value[:cut]
}

func stopConditionError(cond AgentLoopStopCondition) error {
	if cond.Kind == AgentLoopStopMaxIterations {
		return fmt.Errorf("llm: agent loop %s", cond.Reason)
	}

	if cond.MatchedRule != "" {
		return fmt.Errorf("llm: agent loop stopped (%s): %s", cond.MatchedRule, cond.Reason)
	}

	return fmt.Errorf("llm: agent loop stopped: %s", cond.Reason)
}
