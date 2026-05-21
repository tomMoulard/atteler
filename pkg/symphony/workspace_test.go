package symphony

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestWorkspaceManager_EnsureSanitizesAndRunsCreateHookOnce(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	marker := filepath.Join(root, "created.txt")
	cfg := Config{
		Workspace: WorkspaceConfig{Root: root},
		Hooks: HooksConfig{
			AfterCreate: "printf created >> " + marker,
			Timeout:     time.Second,
		},
	}
	issue := Issue{ID: "1", Identifier: "ABC 123/unsafe", Title: "Fix", State: "OPEN"}

	manager := NewWorkspaceManager(nil)
	first, err := manager.Ensure(context.Background(), cfg, issue)
	require.NoError(t, err)
	assert.True(t, first.CreatedNow)
	assert.Equal(t, "ABC_123_unsafe", first.WorkspaceKey)

	second, err := manager.Ensure(context.Background(), cfg, issue)
	require.NoError(t, err)
	assert.False(t, second.CreatedNow)

	data, err := os.ReadFile(marker)
	require.NoError(t, err)
	assert.Equal(t, "created", string(data))
}
