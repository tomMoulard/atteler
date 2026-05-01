package speculate

import (
	"reflect"
	"strings"
	"testing"
)

func TestNewPlan_CreatesThreeRoundWorkflow(t *testing.T) {
	t.Parallel()

	plan, err := NewPlan([]string{"architect", "executor"}, []string{"tests", "scope"})
	if err != nil {
		t.Fatalf("NewPlan() error = %v", err)
	}

	if !reflect.DeepEqual(plan.Agents, []string{"architect", "executor"}) {
		t.Fatalf("Agents = %v, want requested agents", plan.Agents)
	}
	if !reflect.DeepEqual(plan.GateChecks, []string{"tests", "scope"}) {
		t.Fatalf("GateChecks = %v, want requested gates", plan.GateChecks)
	}

	gotRounds := roundNumbers(plan.Rounds)
	wantRounds := []int{RoundProposal, RoundReview, RoundAggregate}
	if !reflect.DeepEqual(gotRounds, wantRounds) {
		t.Fatalf("Rounds = %v, want %v", gotRounds, wantRounds)
	}
}

func TestNewPlan_RejectsDuplicateAgent(t *testing.T) {
	t.Parallel()

	_, err := NewPlan([]string{"executor", "executor"}, nil)
	if err == nil || !strings.Contains(err.Error(), "duplicate agent") {
		t.Fatalf("NewPlan() error = %v, want duplicate agent error", err)
	}
}

func TestCrossReviews_MapsEveryAgentToOtherProposals(t *testing.T) {
	t.Parallel()

	reviews, err := CrossReviews([]Proposal{
		{Agent: "architect", Round: RoundProposal, Content: "plan"},
		{Agent: "executor", Round: RoundProposal, Content: "build"},
		{Agent: "verifier", Round: RoundProposal, Content: "test"},
	})
	if err != nil {
		t.Fatalf("CrossReviews() error = %v", err)
	}

	got := reviewPairs(reviews)
	want := []string{
		"architect->executor",
		"architect->verifier",
		"executor->architect",
		"executor->verifier",
		"verifier->architect",
		"verifier->executor",
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("CrossReviews() = %v, want %v", got, want)
	}
}

func TestCrossReviews_RejectsNonProposalRound(t *testing.T) {
	t.Parallel()

	_, err := CrossReviews([]Proposal{{Agent: "executor", Round: RoundReview}})
	if err == nil || !strings.Contains(err.Error(), "want round 1") {
		t.Fatalf("CrossReviews() error = %v, want round mismatch error", err)
	}
}

func TestValidateVerdict_PassesWhenRequiredGatesPass(t *testing.T) {
	t.Parallel()

	verdict := Verdict{
		Winner: "executor",
		Reason: "most complete plan",
		GateChecks: []GateCheck{
			{Name: "tests", Passed: true},
			{Name: "scope", Passed: true},
		},
	}

	if err := ValidateVerdict(verdict, []string{"tests", "scope"}); err != nil {
		t.Fatalf("ValidateVerdict() error = %v", err)
	}
}

func TestValidateVerdict_FailsWhenGateFails(t *testing.T) {
	t.Parallel()

	verdict := Verdict{
		Winner: "executor",
		Reason: "most complete plan",
		GateChecks: []GateCheck{
			{Name: "tests", Passed: true},
			{Name: "scope", Passed: false},
		},
	}

	err := ValidateVerdict(verdict, []string{"tests", "scope"})
	if err == nil || !strings.Contains(err.Error(), `gate check "scope" failed`) {
		t.Fatalf("ValidateVerdict() error = %v, want failed gate error", err)
	}
}

func TestValidateVerdict_FailsWhenGateMissing(t *testing.T) {
	t.Parallel()

	verdict := Verdict{
		Winner: "executor",
		Reason: "most complete plan",
		GateChecks: []GateCheck{
			{Name: "tests", Passed: true},
		},
	}

	err := ValidateVerdict(verdict, []string{"tests", "scope"})
	if err == nil || !strings.Contains(err.Error(), `missing gate check "scope"`) {
		t.Fatalf("ValidateVerdict() error = %v, want missing gate error", err)
	}
}

func roundNumbers(rounds []Round) []int {
	numbers := make([]int, len(rounds))
	for i, round := range rounds {
		numbers[i] = round.Number
	}
	return numbers
}

func reviewPairs(reviews []Review) []string {
	pairs := make([]string, len(reviews))
	for i, review := range reviews {
		pairs[i] = review.Reviewer + "->" + review.TargetAgent
	}
	return pairs
}
