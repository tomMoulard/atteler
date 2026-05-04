package llm

import "testing"

func TestNormalizeReasoningLevel(t *testing.T) {
	cases := map[string]string{
		"":            "",
		"  ":          "",
		"LOW":         "low",
		"Low":         "low",
		"x-high":      "xhigh",
		"X-High":      "xhigh",
		"x_high":      "xhigh",
		"extra-high":  "xhigh",
		"extra_high":  "xhigh",
		"extra":       "xhigh",
		"medium":      "medium",
		"none":        "none",
		"max":         "max",
		"unknown":     "unknown",
		"  medium  ":  "medium",
	}
	for in, want := range cases {
		if got := normalizeReasoningLevel(in); got != want {
			t.Errorf("normalizeReasoningLevel(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestReasoningEffortRank(t *testing.T) {
	cases := map[string]int{
		ReasoningLevelDefault: 0,
		"none":                1,
		"low":                 2,
		"medium":              3,
		"high":                4,
		"xhigh":               5,
		"x-high":              5,
		"extra":               5,
		"max":                 -1,
		"bogus":               -1,
	}
	for in, want := range cases {
		if got := ReasoningEffortRank(in); got != want {
			t.Errorf("ReasoningEffortRank(%q) = %d, want %d", in, got, want)
		}
	}
}

func TestOpenAIReasoningEffort(t *testing.T) {
	cases := map[string]string{
		"":           "",
		"none":       "none",
		"minimal":    "minimal",
		"low":        "low",
		"medium":     "medium",
		"high":       "high",
		"xhigh":      "xhigh",
		"x-high":     "xhigh",
		"max":        "xhigh",
		"  custom ": "custom",
	}
	for in, want := range cases {
		if got := openAIReasoningEffort(in); got != want {
			t.Errorf("openAIReasoningEffort(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestCLIReasoningEffort(t *testing.T) {
	cases := map[string]string{
		"":        "",
		"none":    "",
		"minimal": "low",
		"low":     "low",
		"medium":  "medium",
		"high":    "high",
		"xhigh":   "xhigh",
		"x-high":  "xhigh",
		"max":     "max",
	}
	for in, want := range cases {
		if got := cliReasoningEffort(in); got != want {
			t.Errorf("cliReasoningEffort(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestOllamaThink(t *testing.T) {
	type result struct {
		val any
		ok  bool
	}
	cases := map[string]result{
		"":        {nil, false},
		"none":    {false, true},
		"minimal": {"low", true},
		"low":     {"low", true},
		"medium":  {"medium", true},
		"high":    {"high", true},
		"xhigh":   {"high", true},
		"x-high":  {"high", true},
		"max":     {"high", true},
		"custom":  {"custom", true},
	}
	for in, want := range cases {
		gotVal, gotOK := ollamaThink(in)
		if gotOK != want.ok || gotVal != want.val {
			t.Errorf("ollamaThink(%q) = (%v, %v), want (%v, %v)", in, gotVal, gotOK, want.val, want.ok)
		}
	}
}

func TestAnthropicThinkingBudgetDisabled(t *testing.T) {
	cases := []string{"", "none", "minimal"}
	for _, level := range cases {
		budget, enabled, err := anthropicThinkingBudget(level, 8192)
		if err != nil {
			t.Errorf("anthropicThinkingBudget(%q): unexpected error %v", level, err)
		}
		if enabled || budget != 0 {
			t.Errorf("anthropicThinkingBudget(%q) = (%d, %v), want (0, false)", level, budget, enabled)
		}
	}
}

func TestAnthropicThinkingBudgetMaxTokensTooSmall(t *testing.T) {
	for _, level := range []string{"low", "medium", "high", "xhigh", "max"} {
		_, _, err := anthropicThinkingBudget(level, 1024)
		if err == nil {
			t.Errorf("anthropicThinkingBudget(%q, 1024): expected error, got nil", level)
		}
	}
}

func TestAnthropicThinkingBudgetMappings(t *testing.T) {
	const maxTokens = 8192
	cases := map[string]int{
		"low":    1024,
		"medium": maxTokens / 3,
		"high":   maxTokens / 2,
		"xhigh":  (maxTokens * 3) / 4,
		"max":    (maxTokens * 3) / 4,
	}
	for level, wantBudget := range cases {
		budget, enabled, err := anthropicThinkingBudget(level, maxTokens)
		if err != nil {
			t.Fatalf("anthropicThinkingBudget(%q, %d): unexpected error %v", level, maxTokens, err)
		}
		if !enabled {
			t.Errorf("anthropicThinkingBudget(%q): enabled=false, want true", level)
		}
		if budget != wantBudget {
			t.Errorf("anthropicThinkingBudget(%q) budget = %d, want %d", level, budget, wantBudget)
		}
	}
}

func TestAnthropicThinkingBudgetClampedBelowMaxTokens(t *testing.T) {
	// xhigh wants (maxTokens*3)/4. Pick a maxTokens where that's >= maxTokens (impossible
	// arithmetically, but mediumish levels at small maxTokens can land just under). Force
	// the clamp by checking budget < maxTokens for several boundary sizes.
	for _, mx := range []int{1025, 1100, 2048, 4096, 8192} {
		budget, enabled, err := anthropicThinkingBudget("xhigh", mx)
		if err != nil {
			t.Fatalf("xhigh @ maxTokens=%d: unexpected error %v", mx, err)
		}
		if !enabled {
			t.Fatalf("xhigh @ maxTokens=%d: not enabled", mx)
		}
		if budget >= mx {
			t.Errorf("xhigh @ maxTokens=%d: budget %d should be < maxTokens", mx, budget)
		}
	}
}

func TestAnthropicThinkingBudgetUnknownLevelDefaults(t *testing.T) {
	// Unknown non-empty levels fall through to the medium-equivalent default.
	const maxTokens = 8192
	budget, enabled, err := anthropicThinkingBudget("unknown", maxTokens)
	if err != nil {
		t.Fatalf("unknown level: unexpected error %v", err)
	}
	if !enabled {
		t.Fatalf("unknown level: not enabled")
	}
	if want := maxTokens / 3; budget != want {
		t.Errorf("unknown level: budget = %d, want %d (medium fallthrough)", budget, want)
	}
}
