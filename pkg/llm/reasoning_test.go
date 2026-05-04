package llm

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestNormalizeReasoningLevel(t *testing.T) {
	t.Parallel()

	cases := []struct {
		input string
		want  string
	}{
		{input: "", want: ""},
		{input: "  ", want: ""},
		{input: "LOW", want: "low"},
		{input: "Low", want: "low"},
		{input: "x-high", want: "xhigh"},
		{input: "X-High", want: "xhigh"},
		{input: "x_high", want: "xhigh"},
		{input: "extra-high", want: "xhigh"},
		{input: "extra_high", want: "xhigh"},
		{input: "extra", want: "xhigh"},
		{input: "medium", want: "medium"},
		{input: "none", want: "none"},
		{input: "max", want: "max"},
		{input: "unknown", want: "unknown"},
		{input: "  medium  ", want: "medium"},
	}

	for _, tt := range cases {
		assert.Equal(t, tt.want, normalizeReasoningLevel(tt.input), tt.input)
	}
}

func TestReasoningEffortRank(t *testing.T) {
	t.Parallel()

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

	for input, want := range cases {
		assert.Equal(t, want, ReasoningEffortRank(input), input)
	}
}

func TestOpenAIReasoningEffort(t *testing.T) {
	t.Parallel()

	cases := []struct {
		input string
		want  string
	}{
		{input: "", want: ""},
		{input: "none", want: "none"},
		{input: "minimal", want: "minimal"},
		{input: "low", want: "low"},
		{input: "medium", want: "medium"},
		{input: "high", want: "high"},
		{input: "xhigh", want: "xhigh"},
		{input: "x-high", want: "xhigh"},
		{input: "max", want: "xhigh"},
		{input: "  custom ", want: "custom"},
	}

	for _, tt := range cases {
		assert.Equal(t, tt.want, openAIReasoningEffort(tt.input), tt.input)
	}
}

func TestCLIReasoningEffort(t *testing.T) {
	t.Parallel()

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

	for input, want := range cases {
		assert.Equal(t, want, cliReasoningEffort(input), input)
	}
}

func TestOllamaThink(t *testing.T) {
	t.Parallel()

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

	for input, want := range cases {
		gotVal, gotOK := ollamaThink(input)

		assert.Equal(t, want.ok, gotOK, input)
		assert.Equal(t, want.val, gotVal, input)
	}
}

func TestAnthropicThinkingBudgetDisabled(t *testing.T) {
	t.Parallel()

	for _, level := range []string{"", "none", "minimal"} {
		budget, enabled, err := anthropicThinkingBudget(level, 8192)

		require.NoError(t, err)
		assert.False(t, enabled, level)
		assert.Zero(t, budget, level)
	}
}

func TestAnthropicThinkingBudgetMaxTokensTooSmall(t *testing.T) {
	t.Parallel()

	for _, level := range []string{"low", "medium", "high", "xhigh", "max"} {
		_, _, err := anthropicThinkingBudget(level, 1024)
		assert.Error(t, err, level)
	}
}

func TestAnthropicThinkingBudgetMappings(t *testing.T) {
	t.Parallel()

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

		require.NoError(t, err)
		assert.True(t, enabled, level)
		assert.Equal(t, wantBudget, budget, level)
	}
}

func TestAnthropicThinkingBudgetClampedBelowMaxTokens(t *testing.T) {
	t.Parallel()

	// xhigh wants (maxTokens*3)/4. Pick a maxTokens where that's >= maxTokens (impossible
	// arithmetically, but mediumish levels at small maxTokens can land just under). Force
	// the clamp by checking budget < maxTokens for several boundary sizes.
	for _, mx := range []int{1025, 1100, 2048, 4096, 8192} {
		budget, enabled, err := anthropicThinkingBudget("xhigh", mx)

		require.NoError(t, err)
		require.True(t, enabled)
		assert.Less(t, budget, mx)
	}
}

func TestAnthropicThinkingBudgetUnknownLevelDefaults(t *testing.T) {
	t.Parallel()

	// Unknown non-empty levels fall through to the medium-equivalent default.
	const maxTokens = 8192

	budget, enabled, err := anthropicThinkingBudget("unknown", maxTokens)

	require.NoError(t, err)
	require.True(t, enabled)
	assert.Equal(t, maxTokens/3, budget)
}
