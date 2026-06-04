package autonomy

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestParseSupportedLevels(t *testing.T) {
	t.Parallel()

	for _, raw := range []string{"low", "MEDIUM", " high ", "full"} {
		level, err := Parse(raw)
		require.NoError(t, err)
		assert.Contains(t, SupportedValues(), level.String())
	}
}

func TestParseRejectsUnsupportedLevel(t *testing.T) {
	t.Parallel()

	_, err := Parse("unsafe")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "low, medium, high, full")
}

func TestAllowsMatchesAutonomyBoundaries(t *testing.T) {
	t.Parallel()

	assert.False(t, Low.Allows(ActionFileWrite))
	assert.False(t, Low.Allows(ActionMutatingShell))
	assert.True(t, Medium.Allows(ActionFileWrite))
	assert.True(t, Medium.Allows(ActionMutatingShell))
	assert.False(t, Medium.Allows(ActionRemoteMutation))
	assert.False(t, Medium.Allows(ActionCommit))
	assert.False(t, Medium.Allows(ActionPush))
	assert.False(t, Medium.Allows(ActionPullRequestCreate))
	assert.True(t, High.Allows(ActionRemoteMutation))
	assert.True(t, High.Allows(ActionCommit))
	assert.True(t, High.Allows(ActionPush))
	assert.True(t, High.Allows(ActionPullRequestCreate))
	assert.True(t, Full.Allows(ActionPullRequestCreate))
	assert.False(t, Full.Allows(ActionPullRequestMerge))
}

func TestDenialMessageExplainsRequiredLevel(t *testing.T) {
	t.Parallel()

	assert.Contains(t, DenialMessage(Low, ActionFileWrite, "edit foo.go"), "--autonomy medium or higher")
	assert.Contains(t, DenialMessage(Medium, ActionRemoteMutation, "gh issue comment"), "--autonomy high or full")
	assert.Contains(t, DenialMessage(Medium, ActionPush, "git push"), "--autonomy high or full")
	assert.Contains(t, DenialMessage(Full, ActionPullRequestMerge, "gh pr merge"), "leaves merging to a human")
}
