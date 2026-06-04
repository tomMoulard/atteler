package llm

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/tommoulard/atteler/pkg/permission"
)

func TestAgentLoopJSONLCheckpointPermissionPolicyDeniesWrite(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), "session.agentloop.jsonl")

	policy := permission.DefaultPolicy()
	policy.SetMode(permission.OperationWrite, permission.ModeDeny)

	ctx := permission.ContextWithPolicy(t.Context(), &policy)
	err := NewAgentLoopJSONLCheckpoint(path).SaveAgentLoopStep(ctx, AgentLoopStep{
		Kind: AgentLoopStepStop,
	})

	require.Error(t, err)
	require.True(t, permission.ErrDenied(err))
	assert.Contains(t, err.Error(), "permission.write.deny")

	_, statErr := os.Stat(path)
	require.ErrorIs(t, statErr, os.ErrNotExist)
}
