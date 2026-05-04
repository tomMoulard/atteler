package review

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestNewPlan_DefaultsGatesAndCreatesRounds(t *testing.T) {
	t.Parallel()

	plan, err := NewPlan(
		[]Reviewer{{Name: "quality", Categories: []Category{CategoryTests}}},
		[]string{"pkg/review/plan.go"},
		nil,
	)
	require.NoError(t, err)

	assert.Equal(t, DefaultRequiredGates, plan.RequiredGates())
	assert.Equal(t, []RoundKind{RoundIndependentReview, RoundCrossReview, RoundAggregateVerdict}, roundKinds(plan.Rounds()))
	assert.Equal(t, []int{1, 2, 3}, roundNumbers(plan.Rounds()))
}

func TestNewPlan_ValidatesReviewersPathsAndGates(t *testing.T) {
	t.Parallel()

	//nolint:govet // Test table order follows readability.
	tests := []struct {
		name      string
		reviewers []Reviewer
		paths     []string
		gates     []string
		want      string
	}{
		{
			name:      "missing reviewer",
			reviewers: nil,
			paths:     []string{"pkg/review/plan.go"},
			want:      "at least one reviewer is required",
		},
		{
			name:      "invalid reviewer",
			reviewers: []Reviewer{{Name: " ", Categories: []Category{CategoryTests}}},
			paths:     []string{"pkg/review/plan.go"},
			want:      "reviewer 0: reviewer name is required",
		},
		{
			name: "duplicate reviewer",
			reviewers: []Reviewer{
				{Name: "quality", Categories: []Category{CategoryTests}},
				{Name: "quality", Categories: []Category{CategoryCorrectness}},
			},
			paths: []string{"pkg/review/plan.go"},
			want:  `duplicate reviewer "quality"`,
		},
		{
			name:      "invalid path",
			reviewers: []Reviewer{{Name: "quality", Categories: []Category{CategoryTests}}},
			paths:     []string{" "},
			want:      "review path is required",
		},
		{
			name:      "duplicate gate",
			reviewers: []Reviewer{{Name: "quality", Categories: []Category{CategoryTests}}},
			paths:     []string{"pkg/review/plan.go"},
			gates:     []string{"tests pass", "tests pass"},
			want:      `duplicate required gate "tests pass"`,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			_, err := NewPlan(test.reviewers, test.paths, test.gates)
			require.Error(t, err)
			assert.Contains(t, err.Error(), test.want)
		})
	}
}

func TestNewPlan_CreatesCrossReviewPairsForEveryOtherReviewer(t *testing.T) {
	t.Parallel()

	plan, err := NewPlan(
		[]Reviewer{
			{Name: "security", Categories: []Category{CategorySecurity}},
			{Name: "quality", Categories: []Category{CategoryCorrectness}},
			{Name: "tests", Categories: []Category{CategoryTests}},
		},
		[]string{"pkg/review/plan.go"},
		[]string{"tests pass"},
	)
	require.NoError(t, err)

	want := []CrossReview{
		{Reviewer: "quality", ReviewedReviewer: "security"},
		{Reviewer: "quality", ReviewedReviewer: "tests"},
		{Reviewer: "security", ReviewedReviewer: "quality"},
		{Reviewer: "security", ReviewedReviewer: "tests"},
		{Reviewer: "tests", ReviewedReviewer: "quality"},
		{Reviewer: "tests", ReviewedReviewer: "security"},
	}
	assert.Equal(t, want, plan.CrossReviews())

	rounds := plan.Rounds()
	require.Len(t, rounds, 3)
	assert.Equal(t, want, rounds[1].CrossReviews)
	assert.Empty(t, rounds[0].CrossReviews)
	assert.Empty(t, rounds[2].CrossReviews)
}

func TestNewPlan_SortsReviewersPathsAndCategories(t *testing.T) {
	t.Parallel()

	plan, err := NewPlan(
		[]Reviewer{
			{Name: "tests", Categories: []Category{CategoryTests, CategoryCorrectness}},
			{Name: "quality", Categories: []Category{CategoryMaintainability, CategoryCorrectness}},
		},
		[]string{"z.go", "a.go"},
		[]string{"lint pass", "tests pass"},
	)
	require.NoError(t, err)

	assert.Equal(t, []string{"a.go", "z.go"}, plan.Paths())
	assert.Equal(t, []string{"quality", "tests"}, reviewerNames(plan.Reviewers()))
	assert.Equal(t, []Category{CategoryCorrectness, CategoryMaintainability}, plan.Reviewers()[0].Categories)
	assert.Equal(t, []string{"quality", "tests"}, plan.Rounds()[0].Reviewers)
}

func TestNewPlan_ProtectsInputsAndReturnedSlices(t *testing.T) {
	t.Parallel()

	const mutatedAgain = "mutated again"

	reviewers := []Reviewer{{Name: "quality", Categories: []Category{CategoryTests}}}
	paths := []string{"pkg/review/plan.go"}
	gates := []string{"tests pass"}

	plan, err := NewPlan(reviewers, paths, gates)
	require.NoError(t, err)

	reviewers[0].Name = "mutated"
	reviewers[0].Categories[0] = CategorySecurity
	paths[0] = "mutated.go"
	gates[0] = "mutated gate"

	assert.Equal(t, []string{"quality"}, reviewerNames(plan.Reviewers()))
	assert.Equal(t, []Category{CategoryTests}, plan.Reviewers()[0].Categories)
	assert.Equal(t, []string{"pkg/review/plan.go"}, plan.Paths())
	assert.Equal(t, []string{"tests pass"}, plan.RequiredGates())

	returnedReviewers := plan.Reviewers()
	returnedReviewers[0].Name = mutatedAgain
	returnedReviewers[0].Categories[0] = CategorySecurity
	returnedPaths := plan.Paths()
	returnedPaths[0] = "mutated-again.go"
	returnedGates := plan.RequiredGates()
	returnedGates[0] = mutatedAgain
	returnedRounds := plan.Rounds()
	returnedRounds[0].Reviewers[0] = mutatedAgain

	returnedCrossReviews := plan.CrossReviews()
	if len(returnedCrossReviews) > 0 {
		returnedCrossReviews[0].Reviewer = mutatedAgain
	}

	assert.Equal(t, []string{"quality"}, reviewerNames(plan.Reviewers()))
	assert.Equal(t, []Category{CategoryTests}, plan.Reviewers()[0].Categories)
	assert.Equal(t, []string{"pkg/review/plan.go"}, plan.Paths())
	assert.Equal(t, []string{"tests pass"}, plan.RequiredGates())
	assert.Equal(t, []string{"quality"}, plan.Rounds()[0].Reviewers)
}

func TestNewPlan_TrimsReviewerPathAndGateValues(t *testing.T) {
	t.Parallel()

	plan, err := NewPlan(
		[]Reviewer{{Name: " quality ", Categories: []Category{" tests "}}},
		[]string{" pkg/review/plan.go "},
		[]string{" tests pass "},
	)
	require.NoError(t, err)

	assert.Equal(t, "quality", plan.Reviewers()[0].Name)
	assert.Equal(t, []Category{CategoryTests}, plan.Reviewers()[0].Categories)
	assert.Equal(t, []string{"pkg/review/plan.go"}, plan.Paths())
	assert.Equal(t, []string{"tests pass"}, plan.RequiredGates())
}

func TestNewPlan_WrapsDuplicatePathError(t *testing.T) {
	t.Parallel()

	_, err := NewPlan(
		[]Reviewer{{Name: "quality", Categories: []Category{CategoryTests}}},
		[]string{"pkg/review/plan.go", " pkg/review/plan.go "},
		nil,
	)
	require.Error(t, err)
	assert.Contains(t, err.Error(), `duplicate review path "pkg/review/plan.go"`)
}

func roundKinds(rounds []Round) []RoundKind {
	kinds := make([]RoundKind, len(rounds))
	for i := range rounds {
		round := rounds[i]
		kinds[i] = round.Kind
	}

	return kinds
}

func roundNumbers(rounds []Round) []int {
	numbers := make([]int, len(rounds))
	for i := range rounds {
		round := rounds[i]
		numbers[i] = round.Number
	}

	return numbers
}
