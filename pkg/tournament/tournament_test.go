package tournament

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestNormalize_EnablesTournamentForExplicitVariantCount(t *testing.T) {
	t.Parallel()

	options := Normalize(false, 5)

	assert.True(t, options.Active())
	assert.Equal(t, 5, options.Count())
}

func TestNormalize_DefaultsTournamentCountWhenEnabled(t *testing.T) {
	t.Parallel()

	options := Normalize(true, 0)

	assert.True(t, options.Active())
	assert.Equal(t, DefaultVariants, options.Count())
}

func TestAutoresearchInstruction_UsesSharedTournamentOptions(t *testing.T) {
	t.Parallel()

	instruction := AutoresearchInstruction(Options{Enabled: true, Variants: 4})

	assert.Contains(t, instruction, "Tournament mode requested")
	assert.Contains(t, instruction, "4 independent")
	assert.Contains(t, instruction, "same evaluator")
}
