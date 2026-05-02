package speculate

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
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

func TestSharedPromptPrefix_EmptyInputs(t *testing.T) {
	t.Parallel()

	assert.Empty(t, SharedPromptPrefix(nil))
	assert.Empty(t, SharedPromptPrefix([]string{}))
	assert.Empty(t, SharedPromptPrefix([]string{"prefix", ""}))
}

func TestSharedPromptPrefix_FullPartialAndNoSharedPrefix(t *testing.T) {
	t.Parallel()

	tests := []struct { //nolint:govet // Test case field order follows input-then-expected readability.
		name    string
		prompts []string
		want    string
	}{
		{
			name:    "full",
			prompts: []string{"system: same", "system: same"},
			want:    "system: same",
		},
		{
			name:    "partial",
			prompts: []string{"shared prefix: branch one", "shared prefix: branch two"},
			want:    "shared prefix: branch ",
		},
		{
			name:    "none",
			prompts: []string{"alpha", "beta"},
			want:    "",
		},
		{
			name:    "utf8 safe",
			prompts: []string{"cache élan", "cache éclair"},
			want:    "cache é",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			assert.Equal(t, tt.want, SharedPromptPrefix(tt.prompts))
		})
	}
}

func TestEstimatePromptCacheReuse_EmptyInputs(t *testing.T) {
	t.Parallel()

	estimate, err := EstimatePromptCacheReuse(nil)
	require.NoError(t, err)

	assert.Empty(t, estimate.SharedPrefix)
	assert.Zero(t, estimate.SharedPrefixBytes)
	assert.Zero(t, estimate.TotalPromptBytes)
	assert.Zero(t, estimate.ReusablePromptBytes)
	assert.Zero(t, estimate.ReuseRatio)
	assert.Empty(t, estimate.Branches)
}

func TestEstimatePromptCacheReuse_FullPartialAndNoSharedPrefix(t *testing.T) {
	t.Parallel()

	tests := []struct { //nolint:govet // Test case field order follows input-then-expected readability.
		name       string
		branches   []BranchPrompt
		wantPrefix string
		wantRatio  float64
	}{
		{
			name: "full",
			branches: []BranchPrompt{
				{Branch: "executor", Prompt: "same prompt"},
				{Branch: "architect", Prompt: "same prompt"},
			},
			wantPrefix: "same prompt",
			wantRatio:  1,
		},
		{
			name: "partial",
			branches: []BranchPrompt{
				{Branch: "executor", Prompt: "shared: implement"},
				{Branch: "architect", Prompt: "shared: design"},
			},
			wantPrefix: "shared: ",
			wantRatio:  float64(len("shared: ")*2) / float64(len("shared: implement")+len("shared: design")),
		},
		{
			name: "none",
			branches: []BranchPrompt{
				{Branch: "executor", Prompt: "build"},
				{Branch: "architect", Prompt: "plan"},
			},
			wantPrefix: "",
			wantRatio:  0,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			estimate, err := EstimatePromptCacheReuse(tt.branches)
			require.NoError(t, err)

			assert.Equal(t, tt.wantPrefix, estimate.SharedPrefix)
			assert.InDelta(t, tt.wantRatio, estimate.ReuseRatio, 0.000001)
			for _, branch := range estimate.Branches {
				assert.Equal(t, len(tt.wantPrefix), branch.SharedPrefixBytes)
			}
		})
	}
}

func TestEstimatePromptCacheReuse_DeterministicBranchMetadata(t *testing.T) {
	t.Parallel()

	estimate, err := EstimatePromptCacheReuse([]BranchPrompt{
		{Branch: " verifier ", Prompt: "shared prefix: verify"},
		{Branch: "architect", Prompt: "shared prefix: design"},
		{Branch: "executor", Prompt: "shared prefix: build"},
	})
	require.NoError(t, err)

	assert.Equal(t, "shared prefix: ", estimate.SharedPrefix)
	assert.Equal(t, []PromptBranchCacheMetadata{
		{Branch: "architect", PromptBytes: len("shared prefix: design"), SharedPrefixBytes: len("shared prefix: "), ReuseRatio: float64(len("shared prefix: ")) / float64(len("shared prefix: design"))},
		{Branch: "executor", PromptBytes: len("shared prefix: build"), SharedPrefixBytes: len("shared prefix: "), ReuseRatio: float64(len("shared prefix: ")) / float64(len("shared prefix: build"))},
		{Branch: "verifier", PromptBytes: len("shared prefix: verify"), SharedPrefixBytes: len("shared prefix: "), ReuseRatio: float64(len("shared prefix: ")) / float64(len("shared prefix: verify"))},
	}, estimate.Branches)
}

func TestEstimatePromptCacheReuse_RejectsInvalidBranchMetadata(t *testing.T) {
	t.Parallel()

	_, err := EstimatePromptCacheReuse([]BranchPrompt{{Branch: "executor"}, {Branch: " executor "}})
	require.Error(t, err)
	assert.Contains(t, err.Error(), `duplicate branch "executor"`)
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

func TestRun_ExecutesThreeRoundSession(t *testing.T) {
	t.Parallel()

	const executorAgent = "executor"

	plan, err := NewPlan([]string{"architect", executorAgent, "verifier"}, []string{"tests", "scope"})
	if err != nil {
		t.Fatalf("NewPlan() error = %v", err)
	}

	proposalGate := newConcurrentGate(len(plan.Agents))
	wantReviewCount := len(plan.Agents) * (len(plan.Agents) - 1)
	reviewGate := newConcurrentGate(wantReviewCount)

	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Second)
	defer cancel()

	result, err := Run(ctx, plan, Runner{
		Propose: func(ctx context.Context, agent string) (string, error) {
			if arrivalErr := proposalGate.arrive(ctx); arrivalErr != nil {
				return "", arrivalErr
			}
			return "proposal from " + agent, nil
		},
		Review: func(ctx context.Context, assignment Review, proposal Proposal) (string, error) {
			if arrivalErr := reviewGate.arrive(ctx); arrivalErr != nil {
				return "", arrivalErr
			}
			if proposal.Agent != assignment.TargetAgent {
				return "", errors.New("target proposal did not match assignment")
			}
			return assignment.Reviewer + " reviewed " + proposal.Content, nil
		},
		Aggregate: func(_ context.Context, session Session) (Verdict, error) {
			if len(session.Proposals) != len(plan.Agents) {
				t.Fatalf("aggregate saw %d proposals, want %d", len(session.Proposals), len(plan.Agents))
			}
			if len(session.Reviews) != wantReviewCount {
				t.Fatalf("aggregate saw %d reviews, want %d", len(session.Reviews), wantReviewCount)
			}
			return Verdict{
				Winner: executorAgent,
				Reason: "best supported by reviews",
				GateChecks: []GateCheck{
					{Name: "tests", Passed: true},
					{Name: "scope", Passed: true},
				},
			}, nil
		},
	})
	if err != nil {
		t.Fatalf("Run() error = %v", err)
	}

	if result.Winner != executorAgent {
		t.Fatalf("Winner = %q, want executor", result.Winner)
	}
	if result.Session.Verdict.Reason != "best supported by reviews" {
		t.Fatalf("Verdict reason = %q, want aggregator reason", result.Session.Verdict.Reason)
	}

	gotProposalAgents := make([]string, len(result.Session.Proposals))
	for i, proposal := range result.Session.Proposals {
		gotProposalAgents[i] = proposal.Agent
		if proposal.Round != RoundProposal {
			t.Fatalf("proposal round = %d, want %d", proposal.Round, RoundProposal)
		}
	}
	if !reflect.DeepEqual(gotProposalAgents, plan.Agents) {
		t.Fatalf("proposal order = %v, want %v", gotProposalAgents, plan.Agents)
	}

	gotReviews := reviewPairs(result.Session.Reviews)
	wantReviews := []string{
		"architect->executor",
		"architect->verifier",
		"executor->architect",
		"executor->verifier",
		"verifier->architect",
		"verifier->executor",
	}
	if !reflect.DeepEqual(gotReviews, wantReviews) {
		t.Fatalf("reviews = %v, want %v", gotReviews, wantReviews)
	}
}

func TestRun_ReturnsPartialSessionWhenProposalFails(t *testing.T) {
	t.Parallel()

	plan, err := NewPlan([]string{"architect", "executor"}, []string{"tests"})
	if err != nil {
		t.Fatalf("NewPlan() error = %v", err)
	}

	result, err := Run(t.Context(), plan, Runner{
		Propose: func(_ context.Context, agent string) (string, error) {
			if agent == "executor" {
				return "", errors.New("boom")
			}
			return "proposal", nil
		},
		Review: func(context.Context, Review, Proposal) (string, error) {
			return "", errors.New("review should not run after proposal failure")
		},
		Aggregate: func(context.Context, Session) (Verdict, error) {
			return Verdict{}, errors.New("aggregate should not run after proposal failure")
		},
	})
	if err == nil || !strings.Contains(err.Error(), `proposal "executor": boom`) {
		t.Fatalf("Run() error = %v, want proposal error", err)
	}
	if len(result.Session.Proposals) != len(plan.Agents) {
		t.Fatalf("partial proposals = %d, want %d", len(result.Session.Proposals), len(plan.Agents))
	}
	if len(result.Session.Reviews) != 0 {
		t.Fatalf("partial reviews = %d, want 0", len(result.Session.Reviews))
	}
}

func TestRun_ValidatesAggregatorVerdictGates(t *testing.T) {
	t.Parallel()

	plan, err := NewPlan([]string{"architect", "executor"}, []string{"tests", "scope"})
	if err != nil {
		t.Fatalf("NewPlan() error = %v", err)
	}

	result, err := Run(t.Context(), plan, Runner{
		Propose: func(_ context.Context, agent string) (string, error) {
			return "proposal from " + agent, nil
		},
		Review: func(_ context.Context, assignment Review, _ Proposal) (string, error) {
			return assignment.Reviewer + " notes", nil
		},
		Aggregate: func(context.Context, Session) (Verdict, error) {
			return Verdict{
				Winner: "executor",
				Reason: "best supported by reviews",
				GateChecks: []GateCheck{
					{Name: "tests", Passed: true},
				},
			}, nil
		},
	})
	if err == nil || !strings.Contains(err.Error(), `missing gate check "scope"`) {
		t.Fatalf("Run() error = %v, want missing gate error", err)
	}
	if result.Session.Verdict.Winner != "executor" {
		t.Fatalf("partial verdict winner = %q, want executor", result.Session.Verdict.Winner)
	}
}

func TestRun_RejectsMissingRunnerFunctions(t *testing.T) {
	t.Parallel()

	plan, err := NewPlan([]string{"executor"}, nil)
	if err != nil {
		t.Fatalf("NewPlan() error = %v", err)
	}

	_, err = Run(t.Context(), plan, Runner{})
	if err == nil || !strings.Contains(err.Error(), "proposal runner is required") {
		t.Fatalf("Run() error = %v, want missing proposal runner", err)
	}
}

type concurrentGate struct {
	ready  chan struct{}
	once   sync.Once
	mu     sync.Mutex
	needed int
	count  int
}

func newConcurrentGate(needed int) *concurrentGate {
	return &concurrentGate{
		needed: needed,
		ready:  make(chan struct{}),
	}
}

func (gate *concurrentGate) arrive(ctx context.Context) error {
	gate.mu.Lock()
	gate.count++
	if gate.count == gate.needed {
		gate.once.Do(func() { close(gate.ready) })
	}
	gate.mu.Unlock()

	select {
	case <-gate.ready:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}
