package llm

import (
	"fmt"
	"strings"
)

const (
	reasoningLevelNone    = "none"
	reasoningLevelLow     = "low"
	reasoningLevelMedium  = "medium"
	reasoningLevelHigh    = "high"
	reasoningLevelXHigh   = "xhigh"
	reasoningLevelMinimal = "minimal"
	reasoningLevelMax     = "max"

	// ReasoningLevelDefault is a picker-only meta-level meaning "use the
	// configured default" — i.e. clear any session override and fall back to
	// the agent or config-level reasoning_level.
	ReasoningLevelDefault = "default"

	anthropicThinkingMinMaxTokens = 1024
)

// ReasoningEffortLevels returns the canonical mappable reasoning-effort labels
// in ascending order. Pass the returned values through the provider-specific
// helpers (e.g. openAIReasoningEffort) for actual mapping. The list excludes
// aliases (e.g. "minimal", "max") and the "default" meta-level used only by
// the picker.
func ReasoningEffortLevels() []string {
	return []string{
		reasoningLevelNone,
		reasoningLevelLow,
		reasoningLevelMedium,
		reasoningLevelHigh,
		reasoningLevelXHigh,
	}
}

// ReasoningPickerLevels returns the labels offered by the model picker,
// starting with "default" (clear override) followed by each mappable level in
// ascending order.
func ReasoningPickerLevels() []string {
	return append([]string{ReasoningLevelDefault}, ReasoningEffortLevels()...)
}

// ReasoningEffortRank returns the position of level in the picker order.
// "default" is rank 0; mappable levels follow. Unknown levels return -1.
func ReasoningEffortRank(level string) int {
	level = normalizeReasoningLevel(level)
	for i, candidate := range ReasoningPickerLevels() {
		if candidate == level {
			return i
		}
	}

	return -1
}

func normalizeReasoningLevel(level string) string {
	level = strings.ToLower(strings.TrimSpace(level))

	level = strings.ReplaceAll(level, "_", "-")
	switch level {
	case "x-high", "extra-high", "extra":
		return reasoningLevelXHigh
	default:
		return level
	}
}

func openAIReasoningEffort(level string) string {
	switch normalizeReasoningLevel(level) {
	case "", ReasoningLevelDefault:
		return ""
	case reasoningLevelNone, reasoningLevelMinimal, reasoningLevelLow,
		reasoningLevelMedium, reasoningLevelHigh, reasoningLevelXHigh:
		return normalizeReasoningLevel(level)
	case reasoningLevelMax:
		return reasoningLevelXHigh
	default:
		return strings.TrimSpace(level)
	}
}

func cliReasoningEffort(level string) string {
	switch normalizeReasoningLevel(level) {
	case "", ReasoningLevelDefault:
		return ""
	case reasoningLevelMinimal:
		return reasoningLevelLow
	case reasoningLevelNone:
		return ""
	default:
		return normalizeReasoningLevel(level)
	}
}

func ollamaThink(level string) (any, bool) {
	switch normalizeReasoningLevel(level) {
	case "", ReasoningLevelDefault:
		return nil, false
	case reasoningLevelNone:
		return false, true
	case reasoningLevelMinimal, reasoningLevelLow:
		return reasoningLevelLow, true
	case reasoningLevelMedium:
		return reasoningLevelMedium, true
	case reasoningLevelHigh, reasoningLevelXHigh, reasoningLevelMax:
		return reasoningLevelHigh, true
	default:
		return strings.TrimSpace(level), true
	}
}

func anthropicThinkingBudget(level string, maxTokens int) (budget int, enabled bool, err error) {
	switch normalizeReasoningLevel(level) {
	case "", ReasoningLevelDefault:
		return 0, false, nil
	case reasoningLevelNone, reasoningLevelMinimal:
		return 0, false, nil
	}

	if maxTokens <= anthropicThinkingMinMaxTokens {
		return 0, false, fmt.Errorf("reasoning_level requires anthropic max_tokens greater than 1024, got %d", maxTokens)
	}

	switch normalizeReasoningLevel(level) {
	case reasoningLevelLow:
		budget = anthropicThinkingMinMaxTokens
	case reasoningLevelMedium:
		budget = max(anthropicThinkingMinMaxTokens, maxTokens/3)
	case reasoningLevelHigh:
		budget = max(anthropicThinkingMinMaxTokens, maxTokens/2)
	case reasoningLevelXHigh, reasoningLevelMax:
		budget = max(anthropicThinkingMinMaxTokens, (maxTokens*3)/4)
	default:
		budget = max(anthropicThinkingMinMaxTokens, maxTokens/3)
	}

	if budget >= maxTokens {
		budget = maxTokens - 1
	}

	return budget, true, nil
}
