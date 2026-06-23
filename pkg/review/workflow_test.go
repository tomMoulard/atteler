package review

import (
	"fmt"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestNewRunPlan_AppliesSDKDefaults(t *testing.T) {
	t.Parallel()

	plan, err := NewRunPlan(RunPlanOptions{})
	require.NoError(t, err)

	assert.Equal(t, []string{"."}, plan.Paths())
	assert.Len(t, plan.Reviewers(), 2)
	assert.Contains(t, FormatPlan(plan), "behavioral diff reviewed")
}

func TestReviewersFromNames_TrimsNames(t *testing.T) {
	t.Parallel()

	reviewers := ReviewersFromNames([]string{" alpha ", "beta"})

	require.Len(t, reviewers, 2)
	assert.Equal(t, "alpha", reviewers[0].Name)
	assert.Equal(t, "beta", reviewers[1].Name)
}

func ExampleNewRunPlan() {
	plan, err := NewRunPlan(RunPlanOptions{
		Reviewers: []Reviewer{{Name: "quality-reviewer"}},
		Paths:     []string{"pkg/review"},
	})
	if err != nil {
		panic(err)
	}

	lines := strings.Split(FormatPlan(plan), "\n")
	for _, line := range lines[:4] {
		fmt.Println(line)
	}

	// Output:
	// reviewers:
	//   - quality-reviewer
	// paths:
	//   - pkg/review
}
