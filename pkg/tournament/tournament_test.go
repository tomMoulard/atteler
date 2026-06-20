package tournament

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestMerge_DeduplicatesAndRanksVariantCandidates(t *testing.T) {
	t.Parallel()

	variants := []Variant{
		{
			ID:   "ux",
			Lens: "user leverage",
			Candidates: []Candidate{
				{Title: "Guided roadmap scout", Fit: 4, Feasibility: 3, Evidence: 2},
				{Title: "Deep refactor mode", Fit: 2, Feasibility: 1, Evidence: 0, RiskPenalty: 2},
			},
		},
		{
			ID:   "feasibility",
			Lens: "implementation feasibility",
			Candidates: []Candidate{
				{Title: "Guided Roadmap Scout", Fit: 3, Feasibility: 4, Evidence: 1},
				{Title: "Issue import wizard", Fit: 2, Feasibility: 3, Evidence: 1},
			},
		},
	}

	result, err := Merge(variants, Options{KeepTop: 2})

	require.NoError(t, err)
	require.Len(t, result.Ranked, 3)
	assert.Equal(t, "Guided roadmap scout", result.Ranked[0].Title)
	assert.Equal(t, []string{"ux", "feasibility"}, result.Ranked[0].SourceVariants)
	assert.Equal(t, "kept", result.Ranked[0].Decision)
	require.Len(t, result.Discarded, 1)
	assert.Equal(t, "Deep refactor mode", result.Discarded[0].Title)
	assert.Empty(t, variants[0].Candidates[0].Variant, "Merge should not mutate caller-owned variants")
}

func TestMerge_ReturnsNormalizedVariantsWithoutMutatingInputs(t *testing.T) {
	t.Parallel()

	variants := []Variant{
		{
			Candidates: []Candidate{
				{Title: "Guided roadmap scout", Fit: 4, Feasibility: 3},
			},
		},
	}

	result, err := Merge(variants, Options{})

	require.NoError(t, err)
	require.Len(t, result.Variants, 1)
	assert.Equal(t, "variant-1", result.Variants[0].ID)
	assert.Equal(t, "variant-1", result.Variants[0].Candidates[0].Variant)
	assert.Equal(t, "variant-1-1", result.Variants[0].Candidates[0].ID)
	assert.Equal(t, 0, result.Variants[0].Candidates[0].OriginalIndex)
	assert.Empty(t, variants[0].ID, "Merge should not mutate caller-owned variant IDs")
	assert.Empty(t, variants[0].Candidates[0].Variant, "Merge should not mutate caller-owned candidates")
}

func TestMerge_RequiresCandidates(t *testing.T) {
	t.Parallel()

	_, err := Merge([]Variant{{ID: "empty"}}, Options{})

	require.Error(t, err)
	assert.Contains(t, err.Error(), "has no candidates")
}
