package promptcomplete

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestRevamp_BlankInput(t *testing.T) {
	t.Parallel()

	got, ok := Revamp(" \n\t ", RevampStyleDetailed)

	assert.False(t, ok)
	assert.Empty(t, got)
}

func TestRevamp_DetailedLeavesStructuredPromptWithoutBoilerplate(t *testing.T) {
	t.Parallel()

	got, ok := Revamp("  Goal: write release notes\nContext: minor CLI fix  ", RevampStyleDetailed)
	require.True(t, ok)

	assert.Equal(t, "Goal: write release notes\nContext: minor CLI fix", got)
	assert.NotContains(t, got, "Goal: clarify the desired outcome.")
	assert.NotContains(t, got, "Context: include relevant background or inputs.")
	assert.NotContains(t, got, "Constraints: note limits, preferences, and must-haves.")
	assert.NotContains(t, got, "Output format: specify the expected structure.")
}

func TestRevamp_ConciseCleansFillerAndWhitespace(t *testing.T) {
	t.Parallel()

	got, ok := Revamp("  please   summarize\n\tthis   diff  ", RevampStyleConcise)
	require.True(t, ok)

	assert.Equal(t, "summarize this diff", got)
}

func TestRevamp_UnknownStyleUsesDetailedFallback(t *testing.T) {
	t.Parallel()

	got, ok := Revamp("explain the failing test", RevampStyle("verbose"))
	require.True(t, ok)

	assert.Contains(t, got, "explain the failing test")
	assert.Contains(t, got, "Goal: explain the failing test.")
	assert.Contains(t, got, "Context to add: relevant files, errors, prior attempts, or session state.")
	assert.Contains(t, got, "Constraints to preserve: scope, safety, and behavior that must not change.")
	assert.Contains(t, got, "Output: the concrete answer, patch, or verification evidence expected.")
}
