package review

import "strings"

// RunPlanOptions configures the reusable SDK/CLI review-run plan builder.
//
// Empty Reviewers use DefaultReviewers. Empty Paths use DefaultPlanPaths.
// Empty RequiredGates use DefaultRequiredGates.
type RunPlanOptions struct {
	Reviewers     []Reviewer
	Paths         []string
	RequiredGates []string
}

// DefaultReviewers returns the review roles used by Atteler's default review
// workflow when callers do not provide explicit reviewer names.
func DefaultReviewers() []Reviewer {
	return []Reviewer{
		{Name: "quality-reviewer", Categories: []Category{CategoryCorrectness, CategoryMaintainability}},
		{Name: "test-engineer", Categories: []Category{CategoryTests}},
	}
}

// ReviewersFromNames converts CLI/API reviewer names into Reviewer values.
// Empty input returns DefaultReviewers.
func ReviewersFromNames(names []string) []Reviewer {
	if len(names) == 0 {
		return DefaultReviewers()
	}

	reviewers := make([]Reviewer, 0, len(names))
	for _, name := range names {
		reviewers = append(reviewers, Reviewer{Name: strings.TrimSpace(name)})
	}

	return reviewers
}

// DefaultPlanPaths applies the review workflow default target path.
func DefaultPlanPaths(paths []string) []string {
	if len(paths) == 0 {
		return []string{"."}
	}

	return append([]string(nil), paths...)
}

// NewRunPlan builds the deterministic review plan used by both SDK callers and
// the CLI review commands.
func NewRunPlan(options RunPlanOptions) (Plan, error) {
	reviewers := options.Reviewers
	if len(reviewers) == 0 {
		reviewers = DefaultReviewers()
	}

	return NewPlan(reviewers, DefaultPlanPaths(options.Paths), options.RequiredGates)
}
