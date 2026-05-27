package llm

import (
	"context"
	"errors"
	"fmt"
	"time"
)

const (
	defaultMaxHistoryToolBytes = 16 << 10 // 16 KiB per tool result in prompt history.

	agentLoopBudgetRuleMaxInputTokens  = "budget.max_input_tokens"  // #nosec G101 -- budget rule path, not a credential.
	agentLoopBudgetRuleMaxOutputTokens = "budget.max_output_tokens" // #nosec G101 -- budget rule path, not a credential.
	agentLoopBudgetRuleMaxTotalTokens  = "budget.max_total_tokens"  // #nosec G101 -- budget rule path, not a credential.
)

// AgentLoopBudget is the hard-stop envelope for an agentic tool loop. All
// ceilings (MaxToolCalls, MaxWallTime, MaxIterations, MaxModelCalls,
// MaxOutputBytes, MaxCostMicros, MaxTotalTokens, MaxInputTokens,
// MaxOutputTokens) are disabled when zero — callers that want a hard ceiling
// must set them explicitly.
type AgentLoopBudget struct {
	MaxWallTime     time.Duration `json:"max_wall_time"`
	MaxOutputBytes  int64         `json:"max_output_bytes"`
	MaxCostMicros   int64         `json:"max_cost_micros"`
	MaxIterations   int           `json:"max_iterations"`
	MaxModelCalls   int           `json:"max_model_calls"`
	MaxToolCalls    int           `json:"max_tool_calls"`
	MaxInputTokens  int           `json:"max_input_tokens"`
	MaxOutputTokens int           `json:"max_output_tokens"`
	MaxTotalTokens  int           `json:"max_total_tokens"`
}

// IsZero reports whether the budget has no configured ceilings. It supports
// json:",omitzero" on parent metadata records while keeping non-zero budget
// payloads explicit about every ceiling.
func (b AgentLoopBudget) IsZero() bool {
	return b.MaxWallTime == 0 &&
		b.MaxOutputBytes == 0 &&
		b.MaxCostMicros == 0 &&
		b.MaxIterations == 0 &&
		b.MaxModelCalls == 0 &&
		b.MaxToolCalls == 0 &&
		b.MaxInputTokens == 0 &&
		b.MaxOutputTokens == 0 &&
		b.MaxTotalTokens == 0
}

// AgentLoopUsage is the cumulative budget consumption observed so far.
type AgentLoopUsage struct {
	Elapsed             time.Duration `json:"elapsed"`
	OutputBytes         int64         `json:"output_bytes"`
	EstimatedCostMicros int64         `json:"estimated_cost_micros,omitempty"`
	Iterations          int           `json:"iterations"`
	ModelCalls          int           `json:"model_calls"`
	ToolCalls           int           `json:"tool_calls"`
	InputTokens         int           `json:"input_tokens"`
	CachedInputTokens   int           `json:"cached_input_tokens"`
	CacheWriteTokens    int           `json:"cache_write_tokens,omitempty"`
	OutputTokens        int           `json:"output_tokens"`
	TotalTokens         int           `json:"total_tokens"`
}

// AgentLoopBudgetSnapshot captures the per-tool budget state recorded before a
// tool is allowed to execute. It is intentionally serializable so checkpoint
// ledgers can be audited without reconstructing process state.
type AgentLoopBudgetSnapshot struct {
	Budget                AgentLoopBudget `json:"budget"`
	Used                  AgentLoopUsage  `json:"used"`
	RemainingWallTime     time.Duration   `json:"remaining_wall_time"`
	RemainingOutputBytes  int64           `json:"remaining_output_bytes"`
	RemainingCostMicros   int64           `json:"remaining_cost_micros"`
	RemainingIterations   int             `json:"remaining_iterations"`
	RemainingModelCalls   int             `json:"remaining_model_calls"`
	RemainingToolCalls    int             `json:"remaining_tool_calls"`
	RemainingInputTokens  int             `json:"remaining_input_tokens"`
	RemainingOutputTokens int             `json:"remaining_output_tokens"`
	RemainingTotalTokens  int             `json:"remaining_total_tokens"`
}

// AgentLoopCostEstimator converts one model response into estimated cost in
// micro-units of currency. Cost ceilings are enforced only when callers set a
// MaxCostMicros budget and provide this estimator. Estimators must fail closed:
// returning an error stops the loop instead of allowing unpriced usage through.
type AgentLoopCostEstimator func(resp *Response) (int64, error)

// ErrAgentLoopTokenUsageUnavailable means a token ceiling was requested but a
// model response did not include usable provider-reported token usage. Token
// budgets fail closed rather than treating unknown usage as zero.
var ErrAgentLoopTokenUsageUnavailable = errors.New("llm: agent loop token usage metadata unavailable")

type agentLoopTokenUsageError struct {
	err   error
	field agentLoopTokenUsageField
}

func (e agentLoopTokenUsageError) Error() string {
	if e.err == nil {
		return ""
	}

	return e.err.Error()
}

func (e agentLoopTokenUsageError) Unwrap() error {
	return e.err
}

type agentLoopTokenUsageField uint8

const (
	agentLoopTokenUsageFieldAny agentLoopTokenUsageField = iota
	agentLoopTokenUsageFieldInput
	agentLoopTokenUsageFieldOutput
)

type agentLoopBudgetContextKey struct{}

// AgentLoopBudgetSnapshotFromContext returns the per-tool budget snapshot for
// the tool call currently being executed, when the context came from AgentLoop.
func AgentLoopBudgetSnapshotFromContext(ctx context.Context) (AgentLoopBudgetSnapshot, bool) {
	if ctx == nil {
		return AgentLoopBudgetSnapshot{}, false
	}

	snapshot, ok := ctx.Value(agentLoopBudgetContextKey{}).(AgentLoopBudgetSnapshot)

	return snapshot, ok
}

func contextWithAgentLoopBudgetSnapshot(ctx context.Context, snapshot AgentLoopBudgetSnapshot) context.Context {
	return context.WithValue(ctx, agentLoopBudgetContextKey{}, snapshot)
}

func normalizeAgentLoopBudget(b AgentLoopBudget, legacyMaxIterations int) AgentLoopBudget {
	if legacyMaxIterations > 0 {
		b.MaxIterations = legacyMaxIterations
	}

	if b.MaxIterations < 0 {
		b.MaxIterations = 0
	}

	if b.MaxOutputBytes < 0 {
		b.MaxOutputBytes = 0
	}

	if b.MaxCostMicros < 0 {
		b.MaxCostMicros = 0
	}

	if b.MaxModelCalls < 0 {
		b.MaxModelCalls = 0
	}

	if b.MaxWallTime < 0 {
		b.MaxWallTime = 0
	}

	if b.MaxToolCalls < 0 {
		b.MaxToolCalls = 0
	}

	if b.MaxInputTokens < 0 {
		b.MaxInputTokens = 0
	}

	if b.MaxOutputTokens < 0 {
		b.MaxOutputTokens = 0
	}

	if b.MaxTotalTokens < 0 {
		b.MaxTotalTokens = 0
	}

	return b
}

func (u *AgentLoopUsage) addResponse(resp *Response, budget AgentLoopBudget, costEstimator AgentLoopCostEstimator) error {
	if resp == nil {
		return nil
	}

	u.ModelCalls++

	if err := validateAgentLoopTokenUsage(resp, budget); err != nil {
		return err
	}

	u.InputTokens += resp.InputTokens
	u.CachedInputTokens += resp.CachedInputTokens
	u.CacheWriteTokens += resp.CacheWriteInputTokens
	u.OutputTokens += resp.OutputTokens
	u.TotalTokens = u.InputTokens + u.OutputTokens

	if costEstimator != nil {
		costMicros, err := costEstimator(resp)
		if err != nil {
			return err
		}

		if costMicros < 0 {
			return fmt.Errorf("negative agent loop cost estimate: %d micros", costMicros)
		}

		u.EstimatedCostMicros += costMicros
	}

	return nil
}

func validateAgentLoopTokenUsage(resp *Response, budget AgentLoopBudget) error {
	if resp == nil || !agentLoopBudgetRequiresTokenUsage(budget) {
		return nil
	}

	if resp.InputTokens < 0 ||
		resp.CachedInputTokens < 0 ||
		resp.CacheWriteInputTokens < 0 ||
		resp.OutputTokens < 0 {
		return agentLoopTokenUsageError{
			field: agentLoopTokenUsageFieldAny,
			err:   fmt.Errorf("%w: negative token usage", ErrAgentLoopTokenUsageUnavailable),
		}
	}

	requireInput := budget.MaxInputTokens > 0 || budget.MaxTotalTokens > 0
	requireOutput := (budget.MaxOutputTokens > 0 || budget.MaxTotalTokens > 0) && agentLoopResponseHasVisibleOutput(resp)

	switch {
	case requireInput && resp.InputTokens <= 0:
		return agentLoopTokenUsageError{
			field: agentLoopTokenUsageFieldInput,
			err:   fmt.Errorf("%w: input token usage unavailable", ErrAgentLoopTokenUsageUnavailable),
		}
	case requireOutput && resp.OutputTokens <= 0:
		return agentLoopTokenUsageError{
			field: agentLoopTokenUsageFieldOutput,
			err:   fmt.Errorf("%w: output token usage unavailable", ErrAgentLoopTokenUsageUnavailable),
		}
	}

	if resp.CachedInputTokens+resp.CacheWriteInputTokens > resp.InputTokens {
		return agentLoopTokenUsageError{
			field: agentLoopTokenUsageFieldInput,
			err:   fmt.Errorf("%w: cache token usage exceeds input tokens", ErrAgentLoopTokenUsageUnavailable),
		}
	}

	return nil
}

func agentLoopBudgetRequiresTokenUsage(budget AgentLoopBudget) bool {
	return budget.MaxInputTokens > 0 ||
		budget.MaxOutputTokens > 0 ||
		budget.MaxTotalTokens > 0
}

func agentLoopResponseHasVisibleOutput(resp *Response) bool {
	return resp != nil && (resp.Content != "" || len(resp.ToolCalls) > 0)
}

func agentLoopInputUsageMatchedRule(budget AgentLoopBudget) string {
	if budget.MaxInputTokens > 0 {
		return agentLoopBudgetRuleMaxInputTokens
	}

	return agentLoopBudgetRuleMaxTotalTokens
}

func agentLoopOutputUsageMatchedRule(budget AgentLoopBudget) string {
	if budget.MaxOutputTokens > 0 {
		return agentLoopBudgetRuleMaxOutputTokens
	}

	return agentLoopBudgetRuleMaxTotalTokens
}

//nolint:cyclop // Each budget ceiling maps to a distinct durable stop condition.
func budgetExceededStop(b AgentLoopBudget, used AgentLoopUsage) *AgentLoopStopCondition {
	switch {
	case b.MaxIterations > 0 && used.Iterations >= b.MaxIterations:
		return &AgentLoopStopCondition{
			Kind:        AgentLoopStopMaxIterations,
			Reason:      fmt.Sprintf("exceeded %d iterations", b.MaxIterations),
			MatchedRule: "budget.max_iterations",
		}
	case b.MaxModelCalls > 0 && used.ModelCalls > b.MaxModelCalls:
		return &AgentLoopStopCondition{
			Kind:        AgentLoopStopMaxModelCalls,
			Reason:      fmt.Sprintf("model call budget exhausted: used %d of %d", used.ModelCalls, b.MaxModelCalls),
			MatchedRule: "budget.max_model_calls",
		}
	case b.MaxWallTime > 0 && used.Elapsed >= b.MaxWallTime:
		return &AgentLoopStopCondition{
			Kind:        AgentLoopStopWallTime,
			Reason:      fmt.Sprintf("wall-clock budget exhausted: elapsed %s of %s", used.Elapsed, b.MaxWallTime),
			MatchedRule: "budget.max_wall_time",
		}
	case b.MaxInputTokens > 0 && used.InputTokens > b.MaxInputTokens:
		return &AgentLoopStopCondition{
			Kind:        AgentLoopStopTokenBudget,
			Reason:      fmt.Sprintf("input token budget exceeded: used %d of %d", used.InputTokens, b.MaxInputTokens),
			MatchedRule: "budget.max_input_tokens",
		}
	case b.MaxOutputTokens > 0 && used.OutputTokens > b.MaxOutputTokens:
		return &AgentLoopStopCondition{
			Kind:        AgentLoopStopTokenBudget,
			Reason:      fmt.Sprintf("output token budget exceeded: used %d of %d", used.OutputTokens, b.MaxOutputTokens),
			MatchedRule: "budget.max_output_tokens",
		}
	case b.MaxTotalTokens > 0 && used.TotalTokens > b.MaxTotalTokens:
		return &AgentLoopStopCondition{
			Kind:        AgentLoopStopTokenBudget,
			Reason:      fmt.Sprintf("total token budget exceeded: used %d of %d", used.TotalTokens, b.MaxTotalTokens),
			MatchedRule: "budget.max_total_tokens",
		}
	case b.MaxCostMicros > 0 && used.EstimatedCostMicros > b.MaxCostMicros:
		return &AgentLoopStopCondition{
			Kind:        AgentLoopStopCostBudget,
			Reason:      fmt.Sprintf("cost budget exceeded: used %d micros of %d", used.EstimatedCostMicros, b.MaxCostMicros),
			MatchedRule: "budget.max_cost_micros",
		}
	case b.MaxOutputBytes > 0 && used.OutputBytes > b.MaxOutputBytes:
		return &AgentLoopStopCondition{
			Kind:        AgentLoopStopOutputBytes,
			Reason:      fmt.Sprintf("tool output byte budget exceeded: used %d of %d", used.OutputBytes, b.MaxOutputBytes),
			MatchedRule: "budget.max_output_bytes",
		}
	default:
		return nil
	}
}

//nolint:cyclop // Each exact ceiling maps to the same durable stop condition as its over-budget counterpart.
func budgetExhaustedStop(b AgentLoopBudget, used AgentLoopUsage) *AgentLoopStopCondition {
	switch {
	case b.MaxIterations > 0 && used.Iterations >= b.MaxIterations:
		return &AgentLoopStopCondition{
			Kind:        AgentLoopStopMaxIterations,
			Reason:      fmt.Sprintf("exceeded %d iterations", b.MaxIterations),
			MatchedRule: "budget.max_iterations",
		}
	case b.MaxWallTime > 0 && used.Elapsed >= b.MaxWallTime:
		return &AgentLoopStopCondition{
			Kind:        AgentLoopStopWallTime,
			Reason:      fmt.Sprintf("wall-clock budget exhausted: elapsed %s of %s", used.Elapsed, b.MaxWallTime),
			MatchedRule: "budget.max_wall_time",
		}
	case b.MaxInputTokens > 0 && used.InputTokens >= b.MaxInputTokens:
		return &AgentLoopStopCondition{
			Kind:        AgentLoopStopTokenBudget,
			Reason:      fmt.Sprintf("input token budget exhausted: used %d of %d", used.InputTokens, b.MaxInputTokens),
			MatchedRule: "budget.max_input_tokens",
		}
	case b.MaxOutputTokens > 0 && used.OutputTokens >= b.MaxOutputTokens:
		return &AgentLoopStopCondition{
			Kind:        AgentLoopStopTokenBudget,
			Reason:      fmt.Sprintf("output token budget exhausted: used %d of %d", used.OutputTokens, b.MaxOutputTokens),
			MatchedRule: "budget.max_output_tokens",
		}
	case b.MaxTotalTokens > 0 && used.TotalTokens >= b.MaxTotalTokens:
		return &AgentLoopStopCondition{
			Kind:        AgentLoopStopTokenBudget,
			Reason:      fmt.Sprintf("total token budget exhausted: used %d of %d", used.TotalTokens, b.MaxTotalTokens),
			MatchedRule: "budget.max_total_tokens",
		}
	case b.MaxCostMicros > 0 && used.EstimatedCostMicros >= b.MaxCostMicros:
		return &AgentLoopStopCondition{
			Kind:        AgentLoopStopCostBudget,
			Reason:      fmt.Sprintf("cost budget exhausted: used %d micros of %d", used.EstimatedCostMicros, b.MaxCostMicros),
			MatchedRule: "budget.max_cost_micros",
		}
	case b.MaxOutputBytes > 0 && used.OutputBytes >= b.MaxOutputBytes:
		return &AgentLoopStopCondition{
			Kind:        AgentLoopStopOutputBytes,
			Reason:      fmt.Sprintf("tool output byte budget exhausted: used %d of %d", used.OutputBytes, b.MaxOutputBytes),
			MatchedRule: "budget.max_output_bytes",
		}
	default:
		return nil
	}
}

func modelCallBudgetStop(b AgentLoopBudget, used AgentLoopUsage) *AgentLoopStopCondition {
	if b.MaxModelCalls <= 0 || used.ModelCalls < b.MaxModelCalls {
		return nil
	}

	return &AgentLoopStopCondition{
		Kind:        AgentLoopStopMaxModelCalls,
		Reason:      fmt.Sprintf("model call budget exhausted: used %d of %d", used.ModelCalls, b.MaxModelCalls),
		MatchedRule: "budget.max_model_calls",
	}
}
