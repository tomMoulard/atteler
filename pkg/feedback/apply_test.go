package feedback

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/tommoulard/atteler/pkg/config"
)

func TestApplyProposals_AppliesOnlyConfiguredAcceptedAgents(t *testing.T) {
	t.Parallel()

	agents := map[string]config.AgentConfig{
		"reviewer": {SystemPrompt: "Review code."},
	}
	proposals := []Proposal{
		auditedProposal("reviewer", "Run focused regression checks before approving."),
		auditedProposal("writer", "Improve release notes."),
	}

	updated, history := ApplyProposalsWithOptions(agents, proposals, ApplyOptions{
		Author: "human-reviewer",
		Source: "session:test-session",
	})

	require.Len(t, updated, 1)
	assert.Contains(t, updated["reviewer"].SystemPrompt, "Review code.")
	assert.Contains(t, updated["reviewer"].SystemPrompt, "Feedback-derived guidance:")
	assert.Contains(t, updated["reviewer"].SystemPrompt, "- Learning ID:")
	assert.Contains(t, updated["reviewer"].SystemPrompt, "- Root cause: evaluation-regression")
	assert.Contains(t, updated["reviewer"].SystemPrompt, "- Target behavior: Run focused regression checks before approving.")
	assert.NotContains(t, updated["reviewer"].SystemPrompt, "Improve release notes.")
	assert.Equal(t, "Review code.", agents["reviewer"].SystemPrompt, "input map should not be mutated")

	require.Len(t, history, 1)
	entry := history[0]
	assert.Equal(t, "reviewer", entry.Agent)
	assert.Equal(t, PatchStatusAccepted, entry.Status)
	assert.Equal(t, "human-reviewer", entry.Author)
	assert.Equal(t, "session:test-session", entry.Source)
	assert.Equal(t, "evaluation-regression", entry.RootCause.Category)
	assert.Equal(t, "Run focused regression checks before approving.", entry.TargetBehavior)
	assert.InDelta(t, 0.8, entry.Confidence, 0.000001)
	assert.True(t, strings.HasPrefix(entry.BeforePromptHash, "sha256:"))
	assert.True(t, strings.HasPrefix(entry.AfterPromptHash, "sha256:"))
	assert.NotEqual(t, entry.BeforePromptHash, entry.AfterPromptHash)
	assert.Contains(t, entry.Diff, "+++ b/agents/reviewer/system_prompt")
	assert.Contains(t, entry.Diff, "+Feedback-derived guidance:")
	assert.Contains(t, entry.RollbackDiff, "--- a/agents/reviewer/system_prompt")
	assert.Contains(t, entry.RollbackDiff, "-Feedback-derived guidance:")
	assert.Contains(t, entry.RollbackInstructions, entry.AfterPromptHash)
	assert.Contains(t, entry.RollbackInstructions, entry.BeforePromptHash)
	require.Len(t, entry.Verification, 2)
	assert.Equal(t, VerificationPhaseBefore, entry.Verification[0].Phase)
	assert.False(t, entry.Verification[0].Passed)
	assert.Equal(t, VerificationPhaseAfter, entry.Verification[1].Phase)
	assert.True(t, entry.Verification[1].Passed)
}

func TestApplyProposals_RequiresPassingProofBeforeAccepted(t *testing.T) {
	t.Parallel()

	agents := map[string]config.AgentConfig{
		"reviewer": {SystemPrompt: "Review code."},
	}
	proposal := auditedProposal("reviewer", "Run regression checks.")
	proposal.Verification = []VerificationRecord{{
		Kind:    VerificationKindEval,
		Phase:   VerificationPhaseBefore,
		Outcome: "fail",
		Passed:  false,
	}}

	updated, history := ApplyProposals(agents, []Proposal{proposal})

	assert.Empty(t, history)
	assert.Equal(t, agents["reviewer"].SystemPrompt, updated["reviewer"].SystemPrompt)
}

func TestApplyProposals_DoesNotAcceptBeforePhasePassingProof(t *testing.T) {
	t.Parallel()

	agents := map[string]config.AgentConfig{
		"reviewer": {SystemPrompt: "Review code."},
	}
	proposal := auditedProposal("reviewer", "Run regression checks.")
	proposal.Verification = []VerificationRecord{{
		Kind:    VerificationKindEval,
		Phase:   VerificationPhaseBefore,
		Outcome: "pass",
		Passed:  true,
	}}

	updated, history := ApplyProposals(agents, []Proposal{proposal})

	assert.Empty(t, history)
	assert.Equal(t, agents["reviewer"].SystemPrompt, updated["reviewer"].SystemPrompt)
}

func TestApplyProposals_AcceptsPassingFixtureProof(t *testing.T) {
	t.Parallel()

	agents := map[string]config.AgentConfig{
		"reviewer": {SystemPrompt: "Review code."},
	}
	proposal := auditedProposal("reviewer", "Run fixture-backed regression checks.")
	proposal.Verification = []VerificationRecord{{
		Kind:      VerificationKindFixture,
		Phase:     VerificationPhaseAfter,
		Name:      "fixtures/reviewer/auth.md",
		Reference: "fixtures/reviewer/auth.md",
		Passed:    true,
	}}

	updated, history := ApplyProposals(agents, []Proposal{proposal})

	require.Len(t, history, 1)
	assert.Contains(t, updated["reviewer"].SystemPrompt, "Run fixture-backed regression checks.")
	assert.Equal(t, VerificationKindFixture, history[0].Verification[0].Kind)
}

func TestApplyProposals_SynthesizesLinkedEvidenceWhenReferencesAreMissing(t *testing.T) {
	t.Parallel()

	agents := map[string]config.AgentConfig{
		"reviewer": {SystemPrompt: "Review code."},
	}
	proposal := auditedProposal("reviewer", "Run regression checks.")
	proposal.LinkedEvidence = nil
	proposal.Verification = []VerificationRecord{{
		Kind:    VerificationKindEval,
		Phase:   VerificationPhaseAfter,
		Outcome: "pass",
		Notes:   "covered auth regression",
		Passed:  true,
	}}

	_, history := ApplyProposals(agents, []Proposal{proposal})

	require.Len(t, history, 1)
	require.NotEmpty(t, history[0].LinkedEvidence)
	assert.Equal(t, VerificationKindEval, history[0].LinkedEvidence[0].Kind)
	assert.True(t, strings.HasPrefix(history[0].LinkedEvidence[0].Reference, "eval:"))
	assert.Contains(t, FormatHistoryEntry(history[0]), "linked_evidence:")
}

func TestApplyProposals_AvoidsDuplicateGuidance(t *testing.T) {
	t.Parallel()

	proposal := auditedProposal("reviewer", "Add fallback guidance.")
	agents := map[string]config.AgentConfig{
		"reviewer": {SystemPrompt: "Review code."},
	}

	updated, firstHistory := ApplyProposals(agents, []Proposal{proposal})
	updatedAgain, secondHistory := ApplyProposals(updated, []Proposal{proposal})

	require.Len(t, firstHistory, 1)
	assert.Empty(t, secondHistory)
	assert.Equal(t, updated["reviewer"].SystemPrompt, updatedAgain["reviewer"].SystemPrompt)
	assert.Equal(t, 1, countOccurrences(updatedAgain["reviewer"].SystemPrompt, "Feedback-derived guidance:"))
	assert.Equal(t, 1, countOccurrences(updatedAgain["reviewer"].SystemPrompt, "- Target behavior: Add fallback guidance."))
}

func TestApplyProposals_SkipsContradictoryGuidance(t *testing.T) {
	t.Parallel()

	agents := map[string]config.AgentConfig{
		"reviewer": {SystemPrompt: "Review code."},
	}
	first := auditedProposal("reviewer", "Run auth regression tests before approval.")
	contradiction := auditedProposal("reviewer", "Skip auth regression tests when time is short.")

	updated, firstHistory := ApplyProposals(agents, []Proposal{first})
	updatedAgain, secondHistory := ApplyProposals(updated, []Proposal{contradiction})

	require.Len(t, firstHistory, 1)
	assert.Empty(t, secondHistory)
	assert.Equal(t, updated["reviewer"].SystemPrompt, updatedAgain["reviewer"].SystemPrompt)
	assert.NotContains(t, updatedAgain["reviewer"].SystemPrompt, "Skip auth regression tests")
}

func TestApplyProposals_IgnoresUnknownAgents(t *testing.T) {
	t.Parallel()

	agents := map[string]config.AgentConfig{
		"reviewer": {SystemPrompt: "Review code."},
	}

	updated, history := ApplyProposals(agents, []Proposal{auditedProposal("unknown-writer", "Improve release notes.")})

	assert.Empty(t, history)
	assert.Equal(t, agents["reviewer"].SystemPrompt, updated["reviewer"].SystemPrompt)
}

func TestRollbackHistoryEntry_RestoresPromptAndRejectsDivergence(t *testing.T) {
	t.Parallel()

	agents := map[string]config.AgentConfig{
		"reviewer": {SystemPrompt: "Review code."},
	}

	updated, history := ApplyProposals(agents, []Proposal{auditedProposal("reviewer", "Run regression checks.")})
	require.Len(t, history, 1)

	rolledBack, err := RollbackHistoryEntry(updated, history[0])
	require.NoError(t, err)
	assert.Equal(t, "Review code.", rolledBack["reviewer"].SystemPrompt)

	updated["reviewer"] = config.AgentConfig{SystemPrompt: updated["reviewer"].SystemPrompt + "\nmanual edit"}
	_, err = RollbackHistoryEntry(updated, history[0])
	require.Error(t, err)
	assert.Contains(t, err.Error(), "prompt hash mismatch")
}

func TestFormatHistoryEntry_StableFormatting(t *testing.T) {
	t.Parallel()

	entry := HistoryEntry{
		Agent:      "reviewer",
		Action:     "Run focused regression checks before approving.",
		Reason:     "Previous reviews missed auth regressions.",
		Confidence: 0.8,
		RootCause: RootCauseClassification{
			Category: "evaluation-regression",
			Summary:  "Failed eval caught missed auth regression.",
			Signals:  []string{"failed-evaluation"},
		},
		TargetBehavior: "Run auth regression checks before approval.",
		RejectedAlternatives: []RejectedAlternative{{
			Alternative: "Append generic guidance",
			Reason:      "not auditable",
		}},
		Evidence:             []string{"evaluation: fail; score 1; missed auth regression", "ref eval-1"},
		LinkedEvidence:       []EvidenceLink{{Kind: VerificationKindEval, Reference: "eval-1", Description: "missed auth regression"}},
		Verification:         auditedVerification(),
		Author:               "human-reviewer",
		Source:               "session:test",
		Status:               PatchStatusAccepted,
		LearningID:           "abc123",
		BeforePromptHash:     "sha256:before",
		AfterPromptHash:      "sha256:after",
		RollbackInstructions: "restore previous prompt",
		Diff: "--- a/agents/reviewer/system_prompt\n" +
			"+++ b/agents/reviewer/system_prompt\n" +
			"@@\n" +
			"+Feedback-derived guidance:\n",
		RollbackDiff: "--- a/agents/reviewer/system_prompt\n" +
			"+++ b/agents/reviewer/system_prompt\n" +
			"@@\n" +
			"-Feedback-derived guidance:\n",
	}

	got := FormatHistoryEntry(entry)
	for _, want := range []string{
		"agent: reviewer\n",
		"status: accepted\n",
		"author: human-reviewer\n",
		"source: session:test\n",
		"learning_id: abc123\n",
		"confidence: 0.80\n",
		"before_prompt_hash: sha256:before\n",
		"after_prompt_hash: sha256:after\n",
		"root_cause: evaluation-regression",
		"target_behavior: Run auth regression checks before approval.\n",
		"rejected_alternatives:\n",
		"evidence:\n",
		"linked_evidence:\n",
		"verification:\n",
		"phase=before\tkind=eval\tpassed=false",
		"phase=after\tkind=eval\tpassed=true",
		"rollback: restore previous prompt\n",
		"diff:\n```diff\n",
		"+Feedback-derived guidance:\n",
		"rollback_diff:\n```diff\n",
		"-Feedback-derived guidance:\n",
	} {
		assert.Contains(t, got, want)
	}
}

func auditedProposal(agentName, target string) Proposal {
	return Proposal{
		Agent:          agentName,
		Action:         "Run focused regression checks before approving.",
		Reason:         "Previous reviews missed auth regressions.",
		RootCause:      RootCauseClassification{Category: "evaluation-regression", Summary: "Failed eval caught missed auth regression.", Signals: []string{"failed-evaluation"}},
		TargetBehavior: target,
		RejectedAlternatives: []RejectedAlternative{{
			Alternative: "Append generic guidance",
			Reason:      "not auditable",
		}},
		Evidence:       []string{"evaluation: fail; score 1; missed auth regression"},
		LinkedEvidence: []EvidenceLink{{Kind: VerificationKindEval, Reference: "eval-before.md", Description: "missed auth regression"}},
		Verification:   auditedVerification(),
		Confidence:     0.8,
	}
}

func auditedVerification() []VerificationRecord {
	return []VerificationRecord{
		{Kind: VerificationKindEval, Phase: VerificationPhaseBefore, Outcome: "fail", Reference: "eval-before.md", Notes: "missed auth regression", Score: 1, Passed: false},
		{Kind: VerificationKindEval, Phase: VerificationPhaseAfter, Outcome: "pass", Reference: "eval-after.md", Notes: "auth regression covered", Score: 5, Passed: true},
	}
}

func countOccurrences(s, substr string) int {
	return strings.Count(s, substr)
}
