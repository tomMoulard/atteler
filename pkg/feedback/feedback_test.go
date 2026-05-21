package feedback

import (
	"reflect"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/tommoulard/atteler/pkg/session"
)

func TestProposals_GroupsEvidenceByAgentWithAuditMetadata(t *testing.T) {
	t.Parallel()

	proposals := Proposals(
		[]session.AgentEvaluation{
			{Agent: "reviewer", Outcome: "fail", Notes: "missed auth regression", Reference: "eval-before.md", Score: 1},
			{Agent: "reviewer", Outcome: "pass", Notes: "auth regression now covered", Reference: "eval-after.md", Score: 5},
			{Agent: "writer", Outcome: "pass", Notes: "clear release notes", Score: 5},
		},
		[]session.NegativeKnowledge{
			{Agent: "reviewer", Approach: "skip integration tests", Reason: "missed OAuth breakage", Commit: "abc123"},
		},
	)

	require.Len(t, proposals, 1)

	proposal := proposals[0]
	assert.Equal(t, "reviewer", proposal.Agent)
	assert.NotEmpty(t, proposal.Action)
	assert.Equal(t, "Recorded negative knowledge and failed evaluations indicate recurring improvement opportunities.", proposal.Reason)
	assert.Equal(t, "repeated-failed-approach-and-evaluation-regression", proposal.RootCause.Category)
	assert.Equal(t, []string{"negative-knowledge", "failed-evaluation"}, proposal.RootCause.Signals)
	assert.Contains(t, proposal.TargetBehavior, "skip integration tests")
	require.NotEmpty(t, proposal.RejectedAlternatives)
	assert.Contains(t, proposal.RejectedAlternatives[0].Alternative, "generic")

	wantEvidence := []string{
		"negative knowledge: skip integration tests -> missed OAuth breakage",
		"evaluation: fail; score 1; missed auth regression; ref eval-before.md",
	}
	assert.Equal(t, wantEvidence, proposal.Evidence)
	assert.InDelta(t, 0.8, proposal.Confidence, 0.000001)

	require.Len(t, proposal.LinkedEvidence, 3)
	assert.Equal(t, EvidenceLink{Kind: "commit", Reference: "abc123", Description: "negative knowledge: skip integration tests -> missed OAuth breakage"}, proposal.LinkedEvidence[0])
	assert.Equal(t, EvidenceLink{Kind: VerificationKindEval, Reference: "eval-before.md", Description: "evaluation: fail; score 1; missed auth regression; ref eval-before.md"}, proposal.LinkedEvidence[1])
	assert.Equal(t, EvidenceLink{Kind: VerificationKindEval, Reference: "eval-after.md", Description: "evaluation: pass; score 5; auth regression now covered; ref eval-after.md"}, proposal.LinkedEvidence[2])

	require.Len(t, proposal.Verification, 2)
	assert.Equal(t, VerificationPhaseBefore, proposal.Verification[0].Phase)
	assert.False(t, proposal.Verification[0].Passed)
	assert.Equal(t, "eval-before.md", proposal.Verification[0].Reference)
	assert.Equal(t, VerificationPhaseAfter, proposal.Verification[1].Phase)
	assert.True(t, proposal.Verification[1].Passed)
	assert.Equal(t, "eval-after.md", proposal.Verification[1].Reference)
}

func TestProposals_NoOpWhenNoEvidence(t *testing.T) {
	t.Parallel()

	proposals := Proposals(
		[]session.AgentEvaluation{
			{Agent: "reviewer", Outcome: "pass", Notes: "caught regression", Score: 5},
			{Agent: "writer", Outcome: " ", Notes: "ignored because outcome is required"},
		},
		[]session.NegativeKnowledge{
			{Agent: "reviewer", Approach: " ", Reason: "\t"},
		},
	)

	if len(proposals) != 0 {
		t.Fatalf("len(proposals) = %d, want 0: %+v", len(proposals), proposals)
	}
}

func TestProposals_StableOrderingPrioritizesFailedEvidence(t *testing.T) {
	t.Parallel()

	evaluations := []session.AgentEvaluation{
		{Agent: "writer", Outcome: "fail", Notes: "unclear summary"},
		{Agent: "planner", Outcome: "fail", Notes: "missed dependency"},
		{Agent: "architect", Outcome: "fail", Notes: "missed boundary"},
	}
	negativeKnowledge := []session.NegativeKnowledge{
		{Agent: "planner", Approach: "ignore failing test", Reason: "hid regression"},
		{Agent: "architect", Approach: "defer API contract", Reason: "blocked implementation"},
	}

	first := Proposals(evaluations, negativeKnowledge)
	second := Proposals(evaluations, negativeKnowledge)

	if !reflect.DeepEqual(first, second) {
		t.Fatalf("Proposals were not stable:\nfirst:  %#v\nsecond: %#v", first, second)
	}

	gotAgents := proposalAgents(first)

	wantAgents := []string{"architect", "planner", "writer"}
	if !reflect.DeepEqual(gotAgents, wantAgents) {
		t.Fatalf("agents = %#v, want %#v", gotAgents, wantAgents)
	}
}

func TestFromSession_DerivesProposals(t *testing.T) {
	t.Parallel()

	saved := session.Session{
		Evaluations: []session.AgentEvaluation{{Agent: "executor", Outcome: "blocked", Notes: "could not recover"}},
	}

	proposals := FromSession(saved)
	if len(proposals) != 1 {
		t.Fatalf("len(proposals) = %d, want 1: %+v", len(proposals), proposals)
	}

	if proposals[0].Agent != "executor" {
		t.Fatalf("Agent = %q, want executor", proposals[0].Agent)
	}
}

func TestFromSession_AttachesPassingFixtureProofs(t *testing.T) {
	t.Parallel()

	saved := session.Session{
		Evaluations: []session.AgentEvaluation{{Agent: "executor", Outcome: "fail", Notes: "missed fixture"}},
		Artifacts: []session.Artifact{{
			Path:        "fixtures/executor/regression.md",
			Kind:        "fixture",
			Summary:     "passed regression fixture",
			SourceAgent: "executor",
		}},
	}

	proposals := FromSession(saved)
	require.Len(t, proposals, 1)
	require.Len(t, proposals[0].Verification, 2)
	fixture := proposals[0].Verification[1]
	assert.Equal(t, VerificationKindFixture, fixture.Kind)
	assert.Equal(t, VerificationPhaseAfter, fixture.Phase)
	assert.True(t, fixture.Passed)
	assert.Equal(t, "fixtures/executor/regression.md", fixture.Reference)
}

func TestFromSession_IgnoresFixtureProofsOlderThanFailure(t *testing.T) {
	t.Parallel()

	failureTime := time.Date(2026, 5, 2, 10, 0, 0, 0, time.UTC)
	saved := session.Session{
		Evaluations: []session.AgentEvaluation{{
			Agent:     "executor",
			Outcome:   "fail",
			Notes:     "missed fixture",
			CreatedAt: failureTime,
		}},
		Artifacts: []session.Artifact{{
			CreatedAt:   failureTime.Add(-time.Minute),
			Path:        "fixtures/executor/regression.md",
			Kind:        "fixture",
			Summary:     "passed regression fixture before the failure",
			SourceAgent: "executor",
		}},
	}

	proposals := FromSession(saved)

	require.Len(t, proposals, 1)
	require.Len(t, proposals[0].Verification, 1)
	assert.Equal(t, VerificationKindEval, proposals[0].Verification[0].Kind)
	assert.False(t, proposals[0].Verification[0].Passed)
}

func TestProposals_OnlyTreatsLaterPassingEvalAsAfterProof(t *testing.T) {
	t.Parallel()

	failureTime := time.Date(2026, 5, 2, 10, 0, 0, 0, time.UTC)
	proposals := Proposals(
		[]session.AgentEvaluation{
			{
				Agent:     "reviewer",
				Outcome:   "pass",
				Notes:     "old unrelated pass",
				Reference: "eval-old.md",
				Score:     5,
				CreatedAt: failureTime.Add(-time.Minute),
			},
			{
				Agent:     "reviewer",
				Outcome:   "fail",
				Notes:     "missed auth regression",
				Reference: "eval-before.md",
				Score:     1,
				CreatedAt: failureTime,
			},
			{
				Agent:     "reviewer",
				Outcome:   "pass",
				Notes:     "auth regression covered",
				Reference: "eval-after.md",
				Score:     5,
				CreatedAt: failureTime.Add(time.Minute),
			},
		},
		nil,
	)

	require.Len(t, proposals, 1)
	require.Len(t, proposals[0].Verification, 2)
	assert.Equal(t, "eval-before.md", proposals[0].Verification[0].Reference)
	assert.Equal(t, VerificationPhaseBefore, proposals[0].Verification[0].Phase)
	assert.Equal(t, "eval-after.md", proposals[0].Verification[1].Reference)
	assert.Equal(t, VerificationPhaseAfter, proposals[0].Verification[1].Phase)
	assert.NotContains(t, proposalVerificationReferences(proposals[0]), "eval-old.md")
}

func proposalAgents(proposals []Proposal) []string {
	agents := make([]string, 0, len(proposals))
	for i := range proposals {
		agents = append(agents, proposals[i].Agent)
	}

	return agents
}

func proposalVerificationReferences(proposal Proposal) []string {
	references := make([]string, 0, len(proposal.Verification))
	for i := range proposal.Verification {
		references = append(references, proposal.Verification[i].Reference)
	}

	return references
}
