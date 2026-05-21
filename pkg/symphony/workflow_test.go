//nolint:gosec // Tests create a local fake gh executable to verify CLI token fallback.
package symphony

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestParseWorkflow_FrontMatterAndPrompt(t *testing.T) {
	t.Parallel()

	def, err := ParseWorkflow([]byte("---\ntracker:\n  kind: github\n---\n\nDo {{ issue.identifier }}\n"))
	require.NoError(t, err)

	assert.Equal(t, "Do {{ issue.identifier }}", def.PromptTemplate)
	require.Contains(t, def.Config, "tracker")
}

func TestParseWorkflow_FrontMatterMustBeMap(t *testing.T) {
	t.Parallel()

	_, err := ParseWorkflow([]byte("---\n- nope\n---\nbody"))
	require.Error(t, err)

	var classed *ClassedError
	require.ErrorAs(t, err, &classed)
	assert.Equal(t, ErrWorkflowFrontMatterNotMap, classed.Class)
}

func TestResolveConfig_GitHubDefaultsAndRepository(t *testing.T) {
	t.Setenv(githubTokenEnv, "token")
	dir := t.TempDir()
	path := filepath.Join(dir, "WORKFLOW.md")

	cfg, err := ResolveConfig(t.Context(), map[string]any{
		"tracker": map[string]any{
			"kind":       "github",
			"repository": "openai/symphony",
		},
		"workspace": map[string]any{
			"root": "workspaces",
		},
	}, path)
	require.NoError(t, err)

	assert.Equal(t, "github", cfg.Tracker.Kind)
	assert.Equal(t, defaultGitHubEndpoint, cfg.Tracker.Endpoint)
	assert.Equal(t, "openai", cfg.Tracker.Owner)
	assert.Equal(t, "symphony", cfg.Tracker.Repo)
	assert.Equal(t, []string{"OPEN"}, cfg.Tracker.ActiveStates)
	assert.Equal(t, []string{"CLOSED"}, cfg.Tracker.TerminalStates)
	assert.Equal(t, filepath.Join(dir, "workspaces"), cfg.Workspace.Root)
}

func TestResolveConfig_PublishDefaultsRemoveTrackerLabels(t *testing.T) {
	t.Setenv(githubTokenEnv, "token")
	dir := t.TempDir()
	path := filepath.Join(dir, "WORKFLOW.md")

	cfg, err := ResolveConfig(t.Context(), map[string]any{
		"tracker": map[string]any{
			"kind":       "github",
			"repository": "openai/symphony",
			"labels":     []any{"symphony"},
		},
		"publish": map[string]any{
			"enabled": true,
		},
	}, path)
	require.NoError(t, err)

	assert.True(t, cfg.Publish.Enabled)
	assert.Equal(t, "origin", cfg.Publish.Remote)
	assert.Equal(t, "main", cfg.Publish.BaseBranch)
	assert.Equal(t, "symphony", cfg.Publish.BranchPrefix)
	assert.Equal(t, []string{"symphony"}, cfg.Publish.RemoveLabels)
	assert.Equal(t, "Atteler Symphony", cfg.Publish.GitUserName)
	assert.Equal(t, "symphony@users.noreply.github.com", cfg.Publish.GitUserEmail)
}

func TestResolveConfig_PublishCheckMonitorConfig(t *testing.T) {
	t.Setenv(githubTokenEnv, "token")
	dir := t.TempDir()
	path := filepath.Join(dir, "WORKFLOW.md")

	cfg, err := ResolveConfig(t.Context(), map[string]any{
		"tracker": map[string]any{
			"kind":       "github",
			"repository": "openai/symphony",
			"labels":     []any{"symphony"},
		},
		"publish": map[string]any{
			"enabled":                   true,
			"monitor_checks":            true,
			"check_interval_ms":         1234,
			"max_check_rework_attempts": 5,
		},
	}, path)
	require.NoError(t, err)

	assert.True(t, cfg.Publish.MonitorChecks)
	assert.Equal(t, 1234*time.Millisecond, cfg.Publish.CheckInterval)
	assert.Equal(t, 5, cfg.Publish.MaxCheckReworkAttempts)
}

func TestResolveConfig_DebugConfig(t *testing.T) {
	t.Setenv(githubTokenEnv, "token")
	dir := t.TempDir()
	path := filepath.Join(dir, "WORKFLOW.md")

	cfg, err := ResolveConfig(t.Context(), map[string]any{
		"tracker": map[string]any{
			"kind":       "github",
			"repository": "openai/symphony",
		},
		"debug": map[string]any{
			"enabled":     true,
			"address":     "127.0.0.1:0",
			"event_limit": 50,
		},
	}, path)
	require.NoError(t, err)

	assert.True(t, cfg.Debug.Enabled)
	assert.Equal(t, "127.0.0.1:0", cfg.Debug.Address)
	assert.Equal(t, 50, cfg.Debug.EventLimit)
}

func TestResolveConfig_NormalizesCodexSandboxModeObject(t *testing.T) {
	t.Setenv(githubTokenEnv, "token")
	dir := t.TempDir()
	path := filepath.Join(dir, "WORKFLOW.md")

	cfg, err := ResolveConfig(t.Context(), map[string]any{
		"tracker": map[string]any{
			"kind":       "github",
			"repository": "openai/symphony",
		},
		"codex": map[string]any{
			"thread_sandbox": map[string]any{
				"mode": "workspace-write",
			},
			"turn_sandbox_policy": map[string]any{
				"mode": "workspace-write",
			},
		},
	}, path)
	require.NoError(t, err)

	assert.Equal(t, "workspace-write", cfg.Codex.ThreadSandbox)
	assert.Equal(t, map[string]any{"type": "workspaceWrite"}, cfg.Codex.TurnSandboxPolicy)
}

func TestResolveConfig_NormalizesTurnSandboxScalar(t *testing.T) {
	t.Setenv(githubTokenEnv, "token")
	dir := t.TempDir()
	path := filepath.Join(dir, "WORKFLOW.md")

	cfg, err := ResolveConfig(t.Context(), map[string]any{
		"tracker": map[string]any{
			"kind":       "github",
			"repository": "openai/symphony",
		},
		"codex": map[string]any{
			"thread_sandbox":      "workspace-write",
			"turn_sandbox_policy": "workspace-write",
		},
	}, path)
	require.NoError(t, err)

	assert.Equal(t, "workspace-write", cfg.Codex.ThreadSandbox)
	assert.Equal(t, map[string]any{"type": "workspaceWrite"}, cfg.Codex.TurnSandboxPolicy)
}

func TestResolveConfig_GitHubTokenFallsBackToGHCLI(t *testing.T) {
	t.Setenv(githubTokenEnv, "")
	t.Setenv(githubCLITokenEnv, "")

	binDir := t.TempDir()
	ghPath := filepath.Join(binDir, "gh")
	require.NoError(t, os.WriteFile(ghPath, []byte(`#!/usr/bin/env sh
if [ "$1" = "auth" ] && [ "$2" = "token" ]; then
  printf '%s\n' 'cli-token'
  exit 0
fi
exit 1
`), 0o700)) //nolint:gosec // The test needs an executable fake gh command on PATH.
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))

	dir := t.TempDir()
	path := filepath.Join(dir, "WORKFLOW.md")

	cfg, err := ResolveConfig(t.Context(), map[string]any{
		"tracker": map[string]any{
			"kind":       "github",
			"repository": "openai/symphony",
		},
	}, path)
	require.NoError(t, err)

	assert.Equal(t, "cli-token", cfg.Tracker.APIKey)
}

func TestResolveConfig_GitHubTokenFallsBackToGHAuthStatus(t *testing.T) {
	t.Setenv(githubTokenEnv, "")
	t.Setenv(githubCLITokenEnv, "")

	binDir := t.TempDir()
	ghPath := filepath.Join(binDir, "gh")
	require.NoError(t, os.WriteFile(ghPath, []byte(`#!/usr/bin/env sh
if [ "$1" = "auth" ] && [ "$2" = "token" ]; then
  exit 1
fi
if [ "$1" = "auth" ] && [ "$2" = "status" ] && [ "$3" = "--show-token" ]; then
  printf '%s\n' 'github.com'
  printf '%s\n' '  - Token: status-token'
  exit 0
fi
exit 1
`), 0o700))
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))

	dir := t.TempDir()
	path := filepath.Join(dir, "WORKFLOW.md")

	cfg, err := ResolveConfig(t.Context(), map[string]any{
		"tracker": map[string]any{
			"kind":       "github",
			"repository": "openai/symphony",
		},
	}, path)
	require.NoError(t, err)

	assert.Equal(t, "status-token", cfg.Tracker.APIKey)
}

func TestWorkflowManager_ReloadKeepsLastGoodConfigOnInvalidChange(t *testing.T) {
	t.Setenv(githubTokenEnv, "token")
	dir := t.TempDir()
	path := filepath.Join(dir, "WORKFLOW.md")
	require.NoError(t, os.WriteFile(path, []byte("---\ntracker:\n  kind: github\n  repository: owner/repo\n---\nbody"), 0o600))

	manager, err := NewWorkflowManager(dir, path)
	require.NoError(t, err)
	first, err := manager.Load(t.Context())
	require.NoError(t, err)

	time.Sleep(time.Millisecond)
	require.NoError(t, os.WriteFile(path, []byte("---\n- broken\n---\nbody"), 0o600))

	next, changed, err := manager.ReloadIfChanged(t.Context())
	require.Error(t, err)
	assert.False(t, changed)
	assert.Equal(t, first.Config.Tracker.Kind, next.Config.Tracker.Kind)
}
