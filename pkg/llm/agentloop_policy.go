package llm

import (
	"context"
	"fmt"
	"strings"
)

// ToolPolicyVerdict is the action a policy selected for a requested tool call.
type ToolPolicyVerdict string

const (
	// ToolPolicyAllow permits tool execution.
	ToolPolicyAllow ToolPolicyVerdict = "allow"
	// ToolPolicyDeny blocks tool execution and stops the loop.
	ToolPolicyDeny ToolPolicyVerdict = "deny"
	// ToolPolicyRequireConfirm requires caller confirmation before execution.
	ToolPolicyRequireConfirm ToolPolicyVerdict = "require_confirm"
	// ToolPolicyDryRun records the tool call without executing it.
	ToolPolicyDryRun ToolPolicyVerdict = "dry_run"
)

// ToolPolicyDecision records the policy verdict for a tool call.
type ToolPolicyDecision struct {
	Verdict     ToolPolicyVerdict `json:"verdict"`
	Reason      string            `json:"reason"`
	MatchedRule string            `json:"matched_rule,omitempty"`
	Confirmed   bool              `json:"confirmed,omitempty"`
}

// ToolPolicy decides whether a requested tool call may execute.
type ToolPolicy func(ctx context.Context, call ToolCall, budget AgentLoopBudgetSnapshot) ToolPolicyDecision

// ConfirmToolCallFunc is invoked when policy returns require-confirm.
type ConfirmToolCallFunc func(ctx context.Context, call ToolCall, decision ToolPolicyDecision) bool

func defaultToolPolicy(_ context.Context, call ToolCall, _ AgentLoopBudgetSnapshot) ToolPolicyDecision {
	return ToolPolicyDecision{
		Verdict:     ToolPolicyAllow,
		Reason:      fmt.Sprintf("default allow for tool %q", call.Name),
		MatchedRule: "policy.default_allow",
	}
}

func advertisedToolPolicy(tools []ToolDefinition, next ToolPolicy) ToolPolicy {
	allowed := make(map[string]struct{}, len(tools))
	for _, tool := range tools {
		name := strings.TrimSpace(tool.Name)
		if name == "" {
			continue
		}

		allowed[name] = struct{}{}
	}

	return func(ctx context.Context, call ToolCall, budget AgentLoopBudgetSnapshot) ToolPolicyDecision {
		name := strings.TrimSpace(call.Name)
		if _, ok := allowed[name]; !ok {
			return ToolPolicyDecision{
				Verdict:     ToolPolicyDeny,
				Reason:      fmt.Sprintf("tool %q was not advertised in this request", call.Name),
				MatchedRule: "tool.deny.unadvertised",
			}
		}

		return next(ctx, call, budget)
	}
}

func normalizeToolPolicyDecision(call ToolCall, decision ToolPolicyDecision) ToolPolicyDecision {
	if decision.Verdict == "" {
		decision.Verdict = ToolPolicyAllow
	}

	if decision.Reason == "" {
		decision.Reason = fmt.Sprintf("policy %s for tool %q", decision.Verdict, call.Name)
	}

	return decision
}

func budgetDenyPolicy(reason, matchedRule string) ToolPolicyDecision {
	return ToolPolicyDecision{
		Verdict:     ToolPolicyDeny,
		Reason:      reason,
		MatchedRule: matchedRule,
	}
}
