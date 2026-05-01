package feedback

import (
	"reflect"
	"testing"

	"github.com/tommoulard/atteler/pkg/session"
)

func TestProposals_GroupsEvidenceByAgent(t *testing.T) {
	t.Parallel()

	proposals := Proposals(
		[]session.AgentEvaluation{
			{Agent: "reviewer", Outcome: "fail", Notes: "missed auth regression", Reference: "eval-1", Score: 1},
			{Agent: "writer", Outcome: "pass", Notes: "clear release notes", Score: 5},
		},
		[]session.NegativeKnowledge{
			{Agent: "reviewer", Approach: "skip integration tests", Reason: "missed OAuth breakage"},
		},
	)

	if len(proposals) != 1 {
		t.Fatalf("len(proposals) = %d, want 1: %+v", len(proposals), proposals)
	}
	proposal := proposals[0]
	if proposal.Agent != "reviewer" {
		t.Fatalf("Agent = %q, want reviewer", proposal.Agent)
	}
	if proposal.Action == "" {
		t.Fatal("Action is empty")
	}
	if proposal.Reason != "Recorded negative knowledge and failed evaluations indicate recurring improvement opportunities." {
		t.Fatalf("Reason = %q", proposal.Reason)
	}
	wantEvidence := []string{
		"negative knowledge: skip integration tests -> missed OAuth breakage",
		"evaluation: fail; score 1; missed auth regression; ref eval-1",
	}
	if !reflect.DeepEqual(proposal.Evidence, wantEvidence) {
		t.Fatalf("Evidence = %#v, want %#v", proposal.Evidence, wantEvidence)
	}
	if proposal.Confidence != 0.8 {
		t.Fatalf("Confidence = %v, want 0.8", proposal.Confidence)
	}
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

func proposalAgents(proposals []Proposal) []string {
	agents := make([]string, 0, len(proposals))
	for _, proposal := range proposals {
		agents = append(agents, proposal.Agent)
	}
	return agents
}
