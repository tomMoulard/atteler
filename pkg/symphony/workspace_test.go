package symphony

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/tommoulard/atteler/pkg/autonomy"
	"github.com/tommoulard/atteler/pkg/permission"
	"github.com/tommoulard/atteler/pkg/shell"
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

func TestWorkspaceManager_EnsurePermissionPolicyDeniesWriteBeforeCreate(t *testing.T) {
	t.Parallel()

	policy := permission.DefaultPolicy()
	policy.SetMode(permission.OperationWrite, permission.ModeDeny)

	auditDir := t.TempDir()
	ctx := permission.ContextWithPolicy(t.Context(), &policy)
	ctx = permission.ContextWithAuditDir(ctx, auditDir)

	root := filepath.Join(t.TempDir(), "workspaces")
	cfg := Config{Workspace: WorkspaceConfig{Root: root}}
	issue := Issue{ID: "1", Identifier: "GH-8", Title: "Policy", State: "OPEN"}

	manager := NewWorkspaceManager(nil)
	workspace, err := manager.Ensure(ctx, cfg, issue)
	require.Error(t, err)
	require.True(t, permission.ErrDenied(err))
	assert.Empty(t, workspace.Path)
	assert.Contains(t, err.Error(), "permission.write.deny")

	_, statErr := os.Stat(root)
	require.ErrorIs(t, statErr, os.ErrNotExist)

	auditData, readErr := os.ReadFile(filepath.Join(auditDir, "side_effects.jsonl"))
	require.NoError(t, readErr)
	assert.Contains(t, string(auditData), "create Symphony workspace root")
	assert.Contains(t, string(auditData), "permission.write.deny")
}

func TestWorkspaceManager_EnsurePermissionPolicyDeniesReadBeforeIssueStat(t *testing.T) {
	t.Parallel()

	policy := permission.DefaultPolicy()
	policy.SetMode(permission.OperationRead, permission.ModeDeny)

	auditDir := t.TempDir()
	ctx := permission.ContextWithPolicy(t.Context(), &policy)
	ctx = permission.ContextWithAuditDir(ctx, auditDir)

	root := t.TempDir()
	cfg := Config{Workspace: WorkspaceConfig{Root: root}}
	issue := Issue{ID: "1", Identifier: "GH-8", Title: "Policy", State: "OPEN"}

	manager := NewWorkspaceManager(nil)
	workspace, err := manager.Ensure(ctx, cfg, issue)
	require.Error(t, err)
	require.True(t, permission.ErrDenied(err))
	assert.Empty(t, workspace.Path)
	assert.Contains(t, err.Error(), "permission.read.deny")

	_, statErr := os.Stat(filepath.Join(root, "GH-8"))
	require.ErrorIs(t, statErr, os.ErrNotExist)

	auditData, readErr := os.ReadFile(filepath.Join(auditDir, "side_effects.jsonl"))
	require.NoError(t, readErr)
	assert.Contains(t, string(auditData), "inspect Symphony issue workspace")
	assert.Contains(t, string(auditData), "permission.read.deny")
}

func TestWorkspaceManager_RemovePermissionPolicyDeniesMergeDeleteBeforeCleanup(t *testing.T) {
	t.Parallel()

	policy := permission.DefaultPolicy()
	policy.SetMode(permission.OperationMergeDelete, permission.ModeDeny)

	auditDir := t.TempDir()
	ctx := permission.ContextWithPolicy(t.Context(), &policy)
	ctx = permission.ContextWithAuditDir(ctx, auditDir)

	root := t.TempDir()
	workspacePath := filepath.Join(root, "GH-8")
	require.NoError(t, os.Mkdir(workspacePath, workspaceDirPermissions))

	cfg := Config{
		Workspace: WorkspaceConfig{Root: root},
		Hooks: HooksConfig{
			BeforeRemove: "printf removed > hook-ran",
			Timeout:      time.Second,
		},
	}
	issue := Issue{ID: "1", Identifier: "GH-8", Title: "Policy", State: "OPEN"}

	manager := NewWorkspaceManager(nil)
	err := manager.Remove(ctx, cfg, issue)
	require.Error(t, err)
	require.True(t, permission.ErrDenied(err))
	assert.Contains(t, err.Error(), "permission.merge_delete.deny")

	info, statErr := os.Stat(workspacePath)
	require.NoError(t, statErr)
	assert.True(t, info.IsDir())

	_, hookStatErr := os.Stat(filepath.Join(workspacePath, "hook-ran"))
	require.ErrorIs(t, hookStatErr, os.ErrNotExist)

	auditData, readErr := os.ReadFile(filepath.Join(auditDir, "side_effects.jsonl"))
	require.NoError(t, readErr)
	assert.Contains(t, string(auditData), "remove Symphony workspace")
	assert.Contains(t, string(auditData), "permission.merge_delete.deny")
}

func TestWorkspaceManager_RemovePermissionPolicyDeniesReadBeforeStat(t *testing.T) {
	t.Parallel()

	policy := permission.DefaultPolicy()
	policy.SetMode(permission.OperationRead, permission.ModeDeny)

	auditDir := t.TempDir()
	ctx := permission.ContextWithPolicy(t.Context(), &policy)
	ctx = permission.ContextWithAuditDir(ctx, auditDir)

	root := t.TempDir()
	workspacePath := filepath.Join(root, "GH-8")
	require.NoError(t, os.Mkdir(workspacePath, workspaceDirPermissions))

	cfg := Config{
		Workspace: WorkspaceConfig{Root: root},
		Hooks: HooksConfig{
			BeforeRemove: "printf removed > hook-ran",
			Timeout:      time.Second,
		},
	}
	issue := Issue{ID: "1", Identifier: "GH-8", Title: "Policy", State: "OPEN"}

	manager := NewWorkspaceManager(nil)
	err := manager.Remove(ctx, cfg, issue)
	require.Error(t, err)
	require.True(t, permission.ErrDenied(err))
	assert.Contains(t, err.Error(), "permission.read.deny")

	info, statErr := os.Stat(workspacePath)
	require.NoError(t, statErr)
	assert.True(t, info.IsDir())

	_, hookStatErr := os.Stat(filepath.Join(workspacePath, "hook-ran"))
	require.ErrorIs(t, hookStatErr, os.ErrNotExist)

	auditData, readErr := os.ReadFile(filepath.Join(auditDir, "side_effects.jsonl"))
	require.NoError(t, readErr)
	assert.Contains(t, string(auditData), "inspect Symphony workspace before removal")
	assert.Contains(t, string(auditData), "permission.read.deny")
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

func TestRunHookDerivesMissingWorkspaceKeyFromIssueIdentifier(t *testing.T) {
	t.Parallel()

	workspacePath := t.TempDir()
	marker := filepath.Join(workspacePath, "workspace-key.txt")
	cfg := Config{
		Hooks: HooksConfig{
			BeforeRun: "printf '%s' \"$SYMPHONY_WORKSPACE_KEY\" > workspace-key.txt",
			Timeout:   time.Second,
		},
	}
	issue := Issue{ID: "issue-node", Identifier: "GH-61", Title: "Fix", State: "OPEN"}

	err := RunHook(t.Context(), cfg, issue, Workspace{Path: workspacePath}, "before_run", cfg.Hooks.BeforeRun)
	require.NoError(t, err)

	data, err := os.ReadFile(marker)
	require.NoError(t, err)
	assert.Equal(t, "GH-61", string(data))
}

func TestRunHookExposesPublishBaseBranch(t *testing.T) {
	t.Parallel()

	workspacePath := t.TempDir()
	cfg := Config{
		Publish: PublishConfig{BaseBranch: "release/next"},
		Hooks: HooksConfig{
			BeforeRun: "printf '%s' \"$SYMPHONY_BASE_BRANCH\" > base-branch.txt",
			Timeout:   time.Second,
		},
	}
	issue := Issue{ID: "issue-node", Identifier: "GH-61", Title: "Fix", State: "OPEN"}

	err := RunHook(t.Context(), cfg, issue, Workspace{Path: workspacePath}, "before_run", cfg.Hooks.BeforeRun)
	require.NoError(t, err)

	data, err := os.ReadFile(filepath.Join(workspacePath, "base-branch.txt"))
	require.NoError(t, err)
	assert.Equal(t, "release/next", string(data))
}

func TestRunHookRejectsEmptyWorkspaceKeyBeforeExecutingScript(t *testing.T) {
	t.Parallel()

	workspacePath := t.TempDir()
	cfg := Config{
		Hooks: HooksConfig{
			BeforeRun: "touch should-not-exist",
			Timeout:   time.Second,
		},
	}
	issue := Issue{ID: "issue-node", Title: "Fix", State: "OPEN"}

	err := RunHook(context.Background(), cfg, issue, Workspace{Path: workspacePath}, "before_run", cfg.Hooks.BeforeRun)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "workspace key is empty")

	_, statErr := os.Stat(filepath.Join(workspacePath, "should-not-exist"))
	require.ErrorIs(t, statErr, os.ErrNotExist)
}

func TestRunHookLowAutonomyBlocksHookBeforeExecutingScript(t *testing.T) {
	t.Parallel()

	workspacePath := t.TempDir()
	cfg := Config{
		Autonomy: autonomy.Low,
		Hooks: HooksConfig{
			BeforeRun: "touch should-not-exist",
			Timeout:   time.Second,
		},
	}
	issue := Issue{ID: "issue-node", Identifier: "GH-185", Title: "Fix", State: "OPEN"}

	err := RunHook(context.Background(), cfg, issue, Workspace{Path: workspacePath}, "before_run", cfg.Hooks.BeforeRun)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "autonomy low is advisory-only")

	_, statErr := os.Stat(filepath.Join(workspacePath, "should-not-exist"))
	require.ErrorIs(t, statErr, os.ErrNotExist)
}

func TestRunHookMediumAutonomyBlocksPublishCommand(t *testing.T) {
	t.Parallel()

	cfg := Config{
		Autonomy: autonomy.Medium,
		Hooks: HooksConfig{
			BeforeRun: "git push origin HEAD",
			Timeout:   time.Second,
		},
	}
	issue := Issue{ID: "issue-node", Identifier: "GH-185", Title: "Fix", State: "OPEN"}

	err := RunHook(context.Background(), cfg, issue, Workspace{Path: t.TempDir()}, "before_run", cfg.Hooks.BeforeRun)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "autonomy medium blocks git pushes")
	assert.Contains(t, err.Error(), "--autonomy high or full")
}

func TestRunHookBlocksConfirmationOnlyCommands(t *testing.T) {
	t.Parallel()

	cfg := Config{
		Autonomy: autonomy.High,
		Hooks: HooksConfig{
			BeforeRun: "sudo true",
			Timeout:   time.Second,
		},
	}
	issue := Issue{ID: "issue-node", Identifier: "GH-185", Title: "Fix", State: "OPEN"}

	err := RunHook(context.Background(), cfg, issue, Workspace{Path: t.TempDir()}, "before_run", cfg.Hooks.BeforeRun)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "requires confirmation")
	assert.Contains(t, err.Error(), "non-interactively")
}

func TestRunHookRecordsAutonomyInAuditAndEnvironment(t *testing.T) {
	workspacePath := t.TempDir()
	auditDir := filepath.Join(t.TempDir(), "audit")
	t.Setenv(shell.EnvAuditDir, auditDir)

	cfg := Config{
		Autonomy: autonomy.Full,
		Hooks: HooksConfig{
			BeforeRun: "printf '%s' \"$ATTELER_AUTONOMY\" > autonomy.txt",
			Timeout:   time.Second,
		},
	}
	issue := Issue{ID: "issue-node", Identifier: "GH-185", Title: "Fix", State: "OPEN"}

	err := RunHook(context.Background(), cfg, issue, Workspace{Path: workspacePath}, "before_run", cfg.Hooks.BeforeRun)
	require.NoError(t, err)

	data, err := os.ReadFile(filepath.Join(workspacePath, "autonomy.txt"))
	require.NoError(t, err)
	assert.Equal(t, "full", string(data))

	records := readAppServerAuditRecords(t, auditDir)
	require.NotEmpty(t, records)

	for _, record := range records {
		assert.Equal(t, "full", record.Autonomy)
	}
}
