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

func TestRunHookRequiresActiveContext(t *testing.T) {
	t.Parallel()

	cfg := Config{Hooks: HooksConfig{AfterCreate: "echo unused", Timeout: time.Second}}
	issue := Issue{ID: "1", Identifier: "GH-1", Title: "Fix", State: "OPEN"}
	workspace := Workspace{Path: t.TempDir(), WorkspaceKey: "GH-1"}

	err := RunHook(nil, cfg, issue, workspace, "after_create", cfg.Hooks.AfterCreate) //nolint:staticcheck // Verify nil contexts are rejected instead of panicking.
	require.Error(t, err)
	assert.Contains(t, err.Error(), "context is required")

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	err = RunHook(ctx, cfg, issue, workspace, "after_create", cfg.Hooks.AfterCreate)
	require.Error(t, err)
	require.ErrorIs(t, err, context.Canceled)
}
