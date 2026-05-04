package review

import (
	"errors"
	"fmt"
	"slices"
	"strings"
)

// RoundKind identifies one phase of the speculative review plan.
type RoundKind string

const (
	// RoundIndependentReview asks each reviewer to inspect the request independently.
	RoundIndependentReview RoundKind = "independent-review"
	// RoundCrossReview asks reviewers to challenge each other's findings.
	RoundCrossReview RoundKind = "cross-review"
	// RoundAggregateVerdict consolidates all review output into the final verdict.
	RoundAggregateVerdict RoundKind = "aggregate-verdict"
)

// Plan describes the deterministic review-agent execution plan.
type Plan struct {
	reviewers     []Reviewer
	paths         []string
	requiredGates []string
	rounds        []Round
	crossReviews  []CrossReview
}

// Round is stable, CLI-friendly metadata for one review plan phase.
type Round struct {
	Kind          RoundKind
	Name          string
	Reviewers     []string
	Paths         []string
	RequiredGates []string
	CrossReviews  []CrossReview
	Number        int
}

// CrossReview assigns one reviewer to review another reviewer's output.
type CrossReview struct {
	Reviewer         string
	ReviewedReviewer string
}

// NewPlan validates review inputs and returns a deterministic speculative review plan.
func NewPlan(reviewers []Reviewer, paths, gates []string) (Plan, error) {
	normalizedReviewers, err := normalizePlanReviewers(reviewers)
	if err != nil {
		return Plan{}, err
	}

	normalizedPaths, err := normalizePlanPaths(paths)
	if err != nil {
		return Plan{}, err
	}

	normalizedGates, err := normalizePlanGates(gates)
	if err != nil {
		return Plan{}, err
	}

	reviewerNames := reviewerNames(normalizedReviewers)
	crossReviews := buildCrossReviews(reviewerNames)
	rounds := buildRounds(reviewerNames, normalizedPaths, normalizedGates, crossReviews)

	return Plan{
		reviewers:     cloneReviewers(normalizedReviewers),
		paths:         append([]string(nil), normalizedPaths...),
		requiredGates: append([]string(nil), normalizedGates...),
		rounds:        cloneRounds(rounds),
		crossReviews:  append([]CrossReview(nil), crossReviews...),
	}, nil
}

// Reviewers returns the plan reviewers in deterministic name order.
func (plan Plan) Reviewers() []Reviewer {
	return cloneReviewers(plan.reviewers)
}

// Paths returns the review request paths in deterministic lexical order.
func (plan Plan) Paths() []string {
	return append([]string(nil), plan.paths...)
}

// RequiredGates returns the gates that every review report must satisfy.
func (plan Plan) RequiredGates() []string {
	return append([]string(nil), plan.requiredGates...)
}

// Rounds returns the plan rounds in execution order.
func (plan Plan) Rounds() []Round {
	return cloneRounds(plan.rounds)
}

// CrossReviews returns every reviewer-to-reviewer challenge assignment.
func (plan Plan) CrossReviews() []CrossReview {
	return append([]CrossReview(nil), plan.crossReviews...)
}

func normalizePlanReviewers(reviewers []Reviewer) ([]Reviewer, error) {
	if len(reviewers) == 0 {
		return nil, errors.New("at least one reviewer is required")
	}

	normalized := make([]Reviewer, 0, len(reviewers))

	seen := make(map[string]struct{}, len(reviewers))
	for i, reviewer := range reviewers {
		if err := ValidateReviewer(reviewer); err != nil {
			return nil, fmt.Errorf("reviewer %d: %w", i, err)
		}

		name := strings.TrimSpace(reviewer.Name)
		if _, exists := seen[name]; exists {
			return nil, fmt.Errorf("duplicate reviewer %q", name)
		}

		seen[name] = struct{}{}

		categories, err := normalizeCategories(reviewer.Categories)
		if err != nil {
			return nil, fmt.Errorf("reviewer %d: %w", i, err)
		}

		slices.Sort(categories)

		normalized = append(normalized, Reviewer{Name: name, Categories: categories})
	}

	slices.SortStableFunc(normalized, func(left, right Reviewer) int {
		return strings.Compare(left.Name, right.Name)
	})

	return normalized, nil
}

func normalizePlanPaths(paths []string) ([]string, error) {
	if err := ValidateRequest(Request{Paths: paths}); err != nil {
		return nil, err
	}

	normalized, err := normalizeUnique("review path", paths)
	if err != nil {
		return nil, err
	}

	slices.Sort(normalized)

	return normalized, nil
}

func normalizePlanGates(gates []string) ([]string, error) {
	if len(gates) == 0 {
		return append([]string(nil), DefaultRequiredGates...), nil
	}

	return normalizeUnique("required gate", gates)
}

func reviewerNames(reviewers []Reviewer) []string {
	names := make([]string, len(reviewers))
	for i, reviewer := range reviewers {
		names[i] = reviewer.Name
	}

	return names
}

func buildCrossReviews(reviewers []string) []CrossReview {
	crossReviews := make([]CrossReview, 0, len(reviewers)*(len(reviewers)-1))
	for _, reviewer := range reviewers {
		for _, reviewedReviewer := range reviewers {
			if reviewer == reviewedReviewer {
				continue
			}

			crossReviews = append(crossReviews, CrossReview{
				Reviewer:         reviewer,
				ReviewedReviewer: reviewedReviewer,
			})
		}
	}

	return crossReviews
}

func buildRounds(reviewers, paths, gates []string, crossReviews []CrossReview) []Round {
	return []Round{
		{
			Number:        1,
			Kind:          RoundIndependentReview,
			Name:          "Independent review",
			Reviewers:     append([]string(nil), reviewers...),
			Paths:         append([]string(nil), paths...),
			RequiredGates: append([]string(nil), gates...),
		},
		{
			Number:        2,
			Kind:          RoundCrossReview,
			Name:          "Cross-review",
			Reviewers:     append([]string(nil), reviewers...),
			Paths:         append([]string(nil), paths...),
			RequiredGates: append([]string(nil), gates...),
			CrossReviews:  append([]CrossReview(nil), crossReviews...),
		},
		{
			Number:        3,
			Kind:          RoundAggregateVerdict,
			Name:          "Aggregate verdict",
			Reviewers:     append([]string(nil), reviewers...),
			Paths:         append([]string(nil), paths...),
			RequiredGates: append([]string(nil), gates...),
		},
	}
}

func cloneReviewers(reviewers []Reviewer) []Reviewer {
	cloned := make([]Reviewer, len(reviewers))
	for i, reviewer := range reviewers {
		cloned[i] = Reviewer{
			Name:       reviewer.Name,
			Categories: append([]Category(nil), reviewer.Categories...),
		}
	}

	return cloned
}

func cloneRounds(rounds []Round) []Round {
	cloned := make([]Round, len(rounds))
	for i := range rounds {
		round := rounds[i]
		cloned[i] = Round{
			Kind:          round.Kind,
			Name:          round.Name,
			Reviewers:     append([]string(nil), round.Reviewers...),
			Paths:         append([]string(nil), round.Paths...),
			RequiredGates: append([]string(nil), round.RequiredGates...),
			CrossReviews:  append([]CrossReview(nil), round.CrossReviews...),
			Number:        round.Number,
		}
	}

	return cloned
}
