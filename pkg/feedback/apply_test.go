package feedback

import (
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/tommoulard/atteler/pkg/config"
)

func TestApplyProposals_StoresStructuredGuidanceForConfiguredAgents(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 5, 21, 10, 0, 0, 0, time.UTC)
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
			Evidence:   []string{"evaluation: fail"},
			Confidence: 0.65,
		},
	}

	updated, history := ApplyProposalsWithOptions(agents, proposals, ApplyOptions{
		SourceRun: "run-123",
		Reviewer:  "alice",
		Status:    GuidanceStatusApproved,
		Now:       now,
	})

	require.Len(t, updated, 1)
	assert.Equal(t, "Review code.", updated["reviewer"].SystemPrompt, "prompt text should not be mutated")
	assert.Equal(t, "Review code.", agents["reviewer"].SystemPrompt, "input map should not be mutated")

	require.Len(t, updated["reviewer"].FeedbackGuidance, 1)
	record := updated["reviewer"].FeedbackGuidance[0]
	assert.NotEmpty(t, record.ID)
	assert.Equal(t, GuidanceStatusApproved, record.Status)
	assert.Equal(t, "run-123", record.SourceRun)
	assert.Equal(t, "alice", record.Reviewer)
	assert.Equal(t, now, record.CreatedAt)
	assert.Equal(t, now, record.UpdatedAt)
	assert.Equal(t, []string{"evaluation: fail; score 1; missed auth regression"}, record.Evidence)
	require.Len(t, record.Audit, 1)
	assert.Equal(t, auditActionApproved, record.Audit[0].Action)

	rendered := RenderSystemPrompt(updated["reviewer"].SystemPrompt, updated["reviewer"].FeedbackGuidance, now)
	assert.Contains(t, rendered, "Review code.")
	assert.Contains(t, rendered, "Feedback-derived guidance:")
	assert.Contains(t, rendered, "- Action: Run focused regression checks before approving.")
	assert.Contains(t, rendered, "Source run: run-123")
	assert.NotContains(t, rendered, "Improve release notes")

	require.Len(t, history, 1)
	assert.Equal(t, record.ID, history[0].ID)
	assert.Equal(t, "reviewer", history[0].Agent)
	assert.Equal(t, GuidanceStatusApproved, history[0].Status)
	assert.InDelta(t, 0.8, history[0].Confidence, 0.000001)
}

func TestApplyProposals_DefaultsToPendingGuidance(t *testing.T) {
	t.Parallel()

	updated, history := ApplyProposals(map[string]config.AgentConfig{
		"reviewer": {SystemPrompt: "Review code."},
	}, []Proposal{{
		ID:         "fg-default-pending",
		Agent:      "reviewer",
		Action:     "Always run focused tests.",
		Reason:     "Previous run skipped tests.",
		Evidence:   []string{"evaluation: fail"},
		Confidence: 0.8,
	}})

	require.Len(t, history, 1)
	assert.Equal(t, GuidanceStatusPending, history[0].Status)
	require.Len(t, updated["reviewer"].FeedbackGuidance, 1)
	assert.Equal(t, GuidanceStatusPending, updated["reviewer"].FeedbackGuidance[0].Status)
	assert.Equal(t, "Review code.", RenderSystemPrompt(updated["reviewer"].SystemPrompt, updated["reviewer"].FeedbackGuidance, time.Now().UTC()))
}

func TestApplyProposals_QuarantinesPreapprovedGuidanceMissingProvenance(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 5, 21, 10, 0, 0, 0, time.UTC)

	updated, history := ApplyProposalsWithOptions(map[string]config.AgentConfig{
		"reviewer": {SystemPrompt: "Review code."},
	}, []Proposal{{
		ID:         "fg-preapproved-missing-source",
		Agent:      "reviewer",
		Action:     "Always run focused tests.",
		Reason:     "Previous run skipped tests.",
		Evidence:   []string{"evaluation: fail"},
		Confidence: 0.8,
	}}, ApplyOptions{Status: GuidanceStatusApproved, Reviewer: "alice", Now: now})

	require.Len(t, history, 1)
	assert.Equal(t, GuidanceStatusQuarantined, history[0].Status)
	assert.Equal(t, unknownSourceRun, history[0].SourceRun)
	require.Len(t, updated["reviewer"].FeedbackGuidance, 1)
	assert.Equal(t, GuidanceStatusQuarantined, updated["reviewer"].FeedbackGuidance[0].Status)
	assert.Equal(t, "Review code.", RenderSystemPrompt(updated["reviewer"].SystemPrompt, updated["reviewer"].FeedbackGuidance, now))
}

func TestApplyProposals_AvoidsDuplicateGuidanceByID(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 5, 21, 10, 0, 0, 0, time.UTC)
	proposal := Proposal{
		ID:         "fg-auth-regression",
		Agent:      "reviewer",
		Action:     "Add fallback guidance.",
		Reason:     "Repeated failed approach.",
		Evidence:   []string{"negative knowledge: skip tests -> hid regression"},
		Confidence: 0.8,
	}
	agents := map[string]config.AgentConfig{
		"reviewer": {SystemPrompt: "Review code."},
	}

	updated, firstHistory := ApplyProposalsWithOptions(agents, []Proposal{proposal}, ApplyOptions{
		SourceRun: "run-1",
		Reviewer:  "alice",
		Status:    GuidanceStatusApproved,
		Now:       now,
	})
	updatedAgain, secondHistory := ApplyProposalsWithOptions(updated, []Proposal{{
		ID:         proposal.ID,
		Agent:      proposal.Agent,
		Action:     "Different text must not bypass duplicate detection.",
		Reason:     proposal.Reason,
		Evidence:   proposal.Evidence,
		Confidence: proposal.Confidence,
	}}, ApplyOptions{
		SourceRun: "run-1",
		Reviewer:  "alice",
		Status:    GuidanceStatusApproved,
		Now:       now.Add(time.Minute),
	})

	require.Len(t, firstHistory, 1)
	assert.Empty(t, secondHistory)
	require.Len(t, updatedAgain["reviewer"].FeedbackGuidance, 1)
	assert.Equal(t, "Add fallback guidance.", updatedAgain["reviewer"].FeedbackGuidance[0].Action)

	rendered := RenderSystemPrompt(updatedAgain["reviewer"].SystemPrompt, updatedAgain["reviewer"].FeedbackGuidance, now)
	assert.Equal(t, 1, countOccurrences(rendered, "Feedback-derived guidance:"))
	assert.Equal(t, 1, countOccurrences(rendered, "- Action: Add fallback guidance."))
}

func TestRollbackGuidance_DeactivatesWithoutDeletingAuditTrail(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 5, 21, 10, 0, 0, 0, time.UTC)
	updated, history := ApplyProposalsWithOptions(map[string]config.AgentConfig{
		"reviewer": {SystemPrompt: "Review code."},
	}, []Proposal{{
		ID:         "fg-rollback",
		Agent:      "reviewer",
		Action:     "Always run focused tests.",
		Reason:     "Previous run skipped them.",
		Evidence:   []string{"negative knowledge: skipped tests -> hid regression"},
		Confidence: 0.8,
	}}, ApplyOptions{Reviewer: "alice", SourceRun: "run-1", Status: GuidanceStatusApproved, Now: now})
	require.Len(t, history, 1)

	rolledBack, rollbackHistory, ok := RollbackGuidance(updated, "reviewer", "fg-rollback", "bob", "superseded by eval-9", now.Add(time.Hour))

	require.True(t, ok)
	assert.Equal(t, GuidanceStatusRolledBack, rollbackHistory.Status)
	assert.Equal(t, "superseded by eval-9", rollbackHistory.RollbackReason)
	require.Len(t, rolledBack["reviewer"].FeedbackGuidance, 1)
	record := rolledBack["reviewer"].FeedbackGuidance[0]
	assert.Equal(t, GuidanceStatusRolledBack, record.Status)
	assert.Equal(t, "superseded by eval-9", record.RollbackReason)
	require.Len(t, record.Audit, 2)
	assert.Equal(t, auditActionRolledBack, record.Audit[1].Action)

	rendered := RenderSystemPrompt(rolledBack["reviewer"].SystemPrompt, rolledBack["reviewer"].FeedbackGuidance, now.Add(time.Hour))
	assert.Equal(t, "Review code.", rendered)
}

func TestApproveGuidance_ActivatesPendingGuidance(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 5, 21, 10, 0, 0, 0, time.UTC)
	staged, history := ApplyProposalsWithOptions(map[string]config.AgentConfig{
		"reviewer": {SystemPrompt: "Review code."},
	}, []Proposal{{
		ID:         "fg-pending-approval",
		Agent:      "reviewer",
		Action:     "Always run focused tests.",
		Reason:     "Previous run skipped them.",
		Evidence:   []string{"negative knowledge: skipped tests -> hid regression"},
		Confidence: 0.8,
	}}, ApplyOptions{Status: GuidanceStatusPending, Reviewer: "alice", SourceRun: "run-1", Now: now})
	require.Len(t, history, 1)
	assert.Equal(t, GuidanceStatusPending, history[0].Status)
	assert.Equal(t, "Review code.", RenderSystemPrompt(staged["reviewer"].SystemPrompt, staged["reviewer"].FeedbackGuidance, now))

	approved, approvalHistory, ok := ApproveGuidance(staged, "reviewer", "fg-pending-approval", "bob", now.Add(time.Minute))

	require.True(t, ok)
	assert.Equal(t, GuidanceStatusApproved, approvalHistory.Status)

	reviewer := approved["reviewer"]
	require.Len(t, reviewer.FeedbackGuidance, 1)
	assert.Equal(t, GuidanceStatusApproved, reviewer.FeedbackGuidance[0].Status)
	assert.Equal(t, "bob", reviewer.FeedbackGuidance[0].Reviewer)
	require.Len(t, reviewer.FeedbackGuidance[0].Audit, 2)
	assert.Equal(t, auditActionApproved, reviewer.FeedbackGuidance[0].Audit[1].Action)
	assert.Equal(t, "bob", reviewer.FeedbackGuidance[0].Audit[1].Actor)
	assert.Contains(t, RenderSystemPrompt(reviewer.SystemPrompt, reviewer.FeedbackGuidance, now.Add(time.Minute)), "Always run focused tests.")
}

func TestApproveGuidance_QuarantinesMissingProvenance(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 5, 21, 10, 0, 0, 0, time.UTC)
	staged, history := ApplyProposalsWithOptions(map[string]config.AgentConfig{
		"reviewer": {SystemPrompt: "Review code."},
	}, []Proposal{{
		ID:         "fg-missing-source",
		Agent:      "reviewer",
		Action:     "Always run focused tests.",
		Reason:     "Previous run skipped them.",
		Evidence:   []string{"evaluation: fail"},
		Confidence: 0.8,
	}}, ApplyOptions{Reviewer: "alice", Now: now})
	require.Len(t, history, 1)
	assert.Equal(t, unknownSourceRun, history[0].SourceRun)

	approved, approvalHistory, ok := ApproveGuidance(staged, "reviewer", "fg-missing-source", "bob", now.Add(time.Minute))

	require.True(t, ok)
	assert.Equal(t, GuidanceStatusQuarantined, approvalHistory.Status)

	reviewer := approved["reviewer"]
	require.Len(t, reviewer.FeedbackGuidance, 1)
	assert.Equal(t, GuidanceStatusQuarantined, reviewer.FeedbackGuidance[0].Status)
	assert.Equal(t, "bob", reviewer.FeedbackGuidance[0].Reviewer)
	require.Len(t, reviewer.FeedbackGuidance[0].Audit, 2)
	assert.Equal(t, auditActionQuarantined, reviewer.FeedbackGuidance[0].Audit[1].Action)
	assert.Equal(t, "bob", reviewer.FeedbackGuidance[0].Audit[1].Actor)
	assert.Equal(t, "Review code.", RenderSystemPrompt(reviewer.SystemPrompt, reviewer.FeedbackGuidance, now.Add(time.Minute)))
}

func TestApplyProposals_QuarantinesContradictoryGuidance(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 5, 21, 10, 0, 0, 0, time.UTC)
	agents := map[string]config.AgentConfig{
		"reviewer": {
			SystemPrompt: "Review code.",
			FeedbackGuidance: []config.FeedbackGuidance{
				activeTestGuidance("fg-existing", "Always run focused tests.", now.Add(-time.Hour)),
			},
		},
	}

	updated, history := ApplyProposalsWithOptions(agents, []Proposal{{
		ID:         "fg-new",
		Agent:      "reviewer",
		Action:     "Never run focused tests.",
		Reason:     "Bad proposed lesson.",
		Evidence:   []string{"evaluation: fail"},
		Confidence: 0.7,
	}}, ApplyOptions{Reviewer: "bob", SourceRun: "run-new", Now: now})

	require.Len(t, history, 1)
	assert.Equal(t, GuidanceStatusQuarantined, history[0].Status)
	assert.Equal(t, []string{"fg-existing"}, history[0].ConflictsWith)
	require.Len(t, updated["reviewer"].FeedbackGuidance, 2)
	assert.Equal(t, GuidanceStatusQuarantined, updated["reviewer"].FeedbackGuidance[1].Status)
	assert.Equal(t, []string{"fg-existing"}, updated["reviewer"].FeedbackGuidance[1].ConflictWith)

	rendered := RenderSystemPrompt(updated["reviewer"].SystemPrompt, updated["reviewer"].FeedbackGuidance, now)
	assert.Contains(t, rendered, "Always run focused tests.")
	assert.NotContains(t, rendered, "Never run focused tests.")
}

func TestApplyProposals_QuarantinesStaleGuidance(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 5, 21, 10, 0, 0, 0, time.UTC)
	expiresAt := now.Add(-time.Minute)

	updated, history := ApplyProposalsWithOptions(map[string]config.AgentConfig{
		"reviewer": {SystemPrompt: "Review code."},
	}, []Proposal{{
		ID:         "fg-stale",
		Agent:      "reviewer",
		Action:     "Always run stale checks.",
		Reason:     "Expired proposal.",
		Evidence:   []string{"evaluation: fail"},
		ExpiresAt:  &expiresAt,
		Confidence: 0.7,
	}}, ApplyOptions{Now: now})

	require.Len(t, history, 1)
	assert.Equal(t, GuidanceStatusQuarantined, history[0].Status)
	require.Len(t, updated["reviewer"].FeedbackGuidance, 1)
	assert.Equal(t, GuidanceStatusQuarantined, updated["reviewer"].FeedbackGuidance[0].Status)

	rendered := RenderSystemPrompt(updated["reviewer"].SystemPrompt, updated["reviewer"].FeedbackGuidance, now)
	assert.Equal(t, "Review code.", rendered)
}

func TestRenderSystemPrompt_DeterministicOrderAndActiveFilter(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 5, 21, 10, 0, 0, 0, time.UTC)
	expiresAt := now.Add(-time.Minute)
	records := []config.FeedbackGuidance{
		activeTestGuidance("fg-second", "Always run second check.", now.Add(time.Minute)),
		{
			ID:        "fg-pending",
			Status:    GuidanceStatusPending,
			SourceRun: "run-pending",
			Reviewer:  "alice",
			Action:    "Always run pending check.",
			Reason:    "Pending reason.",
			Evidence:  []string{"evaluation: fail pending"},
			CreatedAt: now.Add(-time.Minute),
			UpdatedAt: now.Add(-time.Minute),
			Audit: []config.FeedbackGuidanceAuditEvent{{
				At:     now.Add(-time.Minute),
				Actor:  "alice",
				Action: auditActionPending,
			}},
		},
		activeTestGuidance("fg-first", "Always run first check.", now),
		activeTestGuidanceWithExpiry("fg-expired", "Always run expired check.", now.Add(-2*time.Minute), &expiresAt),
	}

	rendered := RenderSystemPrompt("Review code.", records, now)

	first := strings.Index(rendered, "Always run first check.")
	second := strings.Index(rendered, "Always run second check.")

	require.NotEqual(t, -1, first)
	require.NotEqual(t, -1, second)
	assert.Less(t, first, second)
	assert.NotContains(t, rendered, "pending check")
	assert.NotContains(t, rendered, "expired check")
}

func TestFormatHistoryEntry_StableFormattingWithProvenance(t *testing.T) {
	t.Parallel()

	createdAt := time.Date(2026, 5, 21, 10, 0, 0, 0, time.UTC)
	updatedAt := createdAt.Add(time.Minute)
	expiresAt := createdAt.Add(24 * time.Hour)
	entry := HistoryEntry{
		ID:            "fg-history",
		Agent:         "reviewer",
		Status:        GuidanceStatusApproved,
		SourceRun:     "run-123",
		Reviewer:      "alice",
		CreatedAt:     createdAt,
		UpdatedAt:     updatedAt,
		ExpiresAt:     &expiresAt,
		Action:        "Run focused regression checks before approving.",
		Reason:        "Previous reviews missed auth regressions.",
		Evidence:      []string{"evaluation: fail; score 1; missed auth regression", "ref eval-1"},
		Confidence:    0.8,
		ConflictsWith: []string{"fg-old"},
	}

	got := FormatHistoryEntry(entry)
	want := "id: fg-history\n" +
		"agent: reviewer\n" +
		"status: approved\n" +
		"confidence: 0.80\n" +
		"source_run: run-123\n" +
		"reviewer: alice\n" +
		"created_at: 2026-05-21T10:00:00Z\n" +
		"updated_at: 2026-05-21T10:01:00Z\n" +
		"expires_at: 2026-05-22T10:00:00Z\n" +
		"action: Run focused regression checks before approving.\n" +
		"reason: Previous reviews missed auth regressions.\n" +
		"evidence:\n" +
		"  - evaluation: fail; score 1; missed auth regression\n" +
		"  - ref eval-1\n" +
		"conflicts_with:\n" +
		"  - fg-old\n"
	assert.Equal(t, want, got)
}

func countOccurrences(s, substr string) int {
	return strings.Count(s, substr)
}

func activeTestGuidance(id, action string, createdAt time.Time) config.FeedbackGuidance {
	return activeTestGuidanceWithExpiry(id, action, createdAt, nil)
}

func activeTestGuidanceWithExpiry(id, action string, createdAt time.Time, expiresAt *time.Time) config.FeedbackGuidance {
	return config.FeedbackGuidance{
		ID:         id,
		Status:     GuidanceStatusApproved,
		SourceRun:  "run-" + id,
		Action:     action,
		Reason:     "Test reason.",
		Evidence:   []string{"evaluation: fail"},
		Confidence: 0.8,
		Reviewer:   "alice",
		CreatedAt:  createdAt,
		UpdatedAt:  createdAt,
		ExpiresAt:  expiresAt,
		Audit: []config.FeedbackGuidanceAuditEvent{{
			At:     createdAt,
			Actor:  "alice",
			Action: auditActionApproved,
		}},
	}
}
