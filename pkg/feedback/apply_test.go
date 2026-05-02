package feedback

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/tommoulard/atteler/pkg/config"
)

func TestApplyProposals_AppliesOnlyConfiguredAgents(t *testing.T) {
	t.Parallel()

	agents := map[string]config.AgentConfig{
		"reviewer": {SystemPrompt: "Review code."},
	}
	proposals := []Proposal{
		{
			Agent:      "reviewer",
			Action:     "Run focused regression checks before approving.",
			Reason:     "Previous reviews missed auth regressions.",
			Evidence:   []string{"evaluation: fail; score 1; missed auth regression"},
			Confidence: 0.8,
		},
		{
			Agent:      "writer",
			Action:     "Improve release notes.",
			Reason:     "Not configured in this runtime.",
			Confidence: 0.65,
		},
	}

	updated, history := ApplyProposals(agents, proposals)

	require.Len(t, updated, 1)
	assert.Contains(t, updated["reviewer"].SystemPrompt, "Review code.")
	assert.Contains(t, updated["reviewer"].SystemPrompt, "Feedback-derived guidance:")
	assert.Contains(t, updated["reviewer"].SystemPrompt, "- Action: Run focused regression checks before approving.")
	assert.NotContains(t, updated["reviewer"].SystemPrompt, "Improve release notes.")
	assert.Equal(t, "Review code.", agents["reviewer"].SystemPrompt, "input map should not be mutated")

	require.Len(t, history, 1)
	assert.Equal(t, "reviewer", history[0].Agent)
	assert.InDelta(t, 0.8, history[0].Confidence, 0.000001)
}

func TestApplyProposals_AvoidsDuplicateGuidance(t *testing.T) {
	t.Parallel()

	proposal := Proposal{
		Agent:      "reviewer",
		Action:     "Add fallback guidance.",
		Reason:     "Repeated failed approach.",
		Evidence:   []string{"negative knowledge: skip tests -> hid regression"},
		Confidence: 0.8,
	}
	agents := map[string]config.AgentConfig{
		"reviewer": {SystemPrompt: "Review code."},
	}

	updated, firstHistory := ApplyProposals(agents, []Proposal{proposal})
	updatedAgain, secondHistory := ApplyProposals(updated, []Proposal{proposal})

	require.Len(t, firstHistory, 1)
	assert.Empty(t, secondHistory)
	assert.Equal(t, updated["reviewer"].SystemPrompt, updatedAgain["reviewer"].SystemPrompt)
	assert.Equal(t, 1, countOccurrences(updatedAgain["reviewer"].SystemPrompt, "Feedback-derived guidance:"))
	assert.Equal(t, 1, countOccurrences(updatedAgain["reviewer"].SystemPrompt, "- Action: Add fallback guidance."))
}

func TestFormatHistoryEntry_StableFormatting(t *testing.T) {
	t.Parallel()

	entry := HistoryEntry{
		Agent:      "reviewer",
		Action:     "Run focused regression checks before approving.",
		Reason:     "Previous reviews missed auth regressions.",
		Evidence:   []string{"evaluation: fail; score 1; missed auth regression", "ref eval-1"},
		Confidence: 0.8,
	}

	got := FormatHistoryEntry(entry)
	want := "agent: reviewer\n" +
		"confidence: 0.80\n" +
		"action: Run focused regression checks before approving.\n" +
		"reason: Previous reviews missed auth regressions.\n" +
		"evidence:\n" +
		"  - evaluation: fail; score 1; missed auth regression\n" +
		"  - ref eval-1\n"
	assert.Equal(t, want, got)
}

func countOccurrences(s, substr string) int {
	return strings.Count(s, substr)
}
