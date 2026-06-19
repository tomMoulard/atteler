//nolint:gosec // Tests create a local fake gh executable to verify CLI token fallback.
package symphony

import (
	"os"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/tommoulard/atteler/pkg/autonomy"
	"github.com/tommoulard/atteler/pkg/permission"
	"github.com/tommoulard/atteler/pkg/shell"
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

func TestValidateWorkflow_AcceptsValidFrontMatter(t *testing.T) {
	t.Setenv(githubTokenEnv, "token")
	dir, path := writeTestWorkflow(t, `tracker:
  kind: github
  repository: openai/symphony
workspace:
  root: ./workspaces
polling:
  interval_ms: 30000
agent:
  max_concurrent_agents: 1
  max_turns: 1
  max_retry_backoff_ms: 1000
codex:
  command: codex app-server
  turn_timeout_ms: 1000
  read_timeout_ms: 1000
  stall_timeout_ms: 1000
`)

	cfg, err := ValidateWorkflow(t.Context(), dir, path)
	require.NoError(t, err)
	assert.Equal(t, "github", cfg.Tracker.Kind)
}

func TestValidateWorkflow_RejectsUnknownTopLevelFrontMatterKey(t *testing.T) {
	t.Setenv(githubTokenEnv, "token")

	badKey := "publ" + "sih"
	dir, path := writeTestWorkflow(t, `tracker:
  kind: github
  repository: openai/symphony
`+badKey+`:
  enabled: true
`)

	_, err := ValidateWorkflow(t.Context(), dir, path)
	require.Error(t, err)
	assert.Contains(t, err.Error(), `unknown workflow config field "`+badKey+`"`)
	assert.Contains(t, err.Error(), `did you mean "publish"`)
}

func TestValidateWorkflow_RejectsUnknownNestedFrontMatterKey(t *testing.T) {
	t.Setenv(githubTokenEnv, "token")
	dir, path := writeTestWorkflow(t, `tracker:
  kind: github
  repository: openai/symphony
publish:
  monitr_checks: true
`)

	_, err := ValidateWorkflow(t.Context(), dir, path)
	require.Error(t, err)
	assert.Contains(t, err.Error(), `unknown workflow config field "publish.monitr_checks"`)
	assert.Contains(t, err.Error(), `did you mean "publish.monitor_checks"`)
}

func TestValidateWorkflow_RejectsUnknownVerificationGateKey(t *testing.T) {
	t.Setenv(githubTokenEnv, "token")
	dir, path := writeTestWorkflow(t, `tracker:
  kind: github
  repository: openai/symphony
publish:
  verification_gates:
    - name: unit
      command: go test ./...
      timeout_mss: 1000
`)

	_, err := ValidateWorkflow(t.Context(), dir, path)
	require.Error(t, err)
	assert.Contains(t, err.Error(), `unknown workflow config field "publish.verification_gates[0].timeout_mss"`)
	assert.Contains(t, err.Error(), `did you mean "publish.verification_gates[0].timeout_ms"`)
}

func TestValidateWorkflow_AllowsExtensionNamespaces(t *testing.T) {
	t.Setenv(githubTokenEnv, "token")
	dir, path := writeTestWorkflow(t, `tracker:
  kind: github
  repository: openai/symphony
extensions:
  operator_note:
    owner: platform
publish:
  x_operator_note: keep for local tooling
`)

	_, err := ValidateWorkflow(t.Context(), dir, path)
	require.NoError(t, err)
}

func TestWorkflowManagerLoadPermissionPolicyDeniesWorkflowRead(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	path := filepath.Join(dir, "WORKFLOW.md")
	require.NoError(t, os.WriteFile(path, []byte("Inspect the issue."), 0o600))

	policy := permission.DefaultPolicy()
	policy.SetMode(permission.OperationRead, permission.ModeDeny)

	auditDir := t.TempDir()
	ctx := permission.ContextWithPolicy(t.Context(), &policy)
	ctx = permission.ContextWithAuditDir(ctx, auditDir)

	manager, err := NewWorkflowManager(dir, path)
	require.NoError(t, err)

	_, err = manager.Load(ctx)
	require.Error(t, err)
	require.True(t, permission.ErrDenied(err))
	assert.Contains(t, err.Error(), "permission.read.deny")

	auditData, readErr := os.ReadFile(filepath.Join(auditDir, "side_effects.jsonl"))
	require.NoError(t, readErr)
	assert.Contains(t, string(auditData), "read workflow file")
	assert.Contains(t, string(auditData), "permission.read.deny")
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
	assert.Equal(t, autonomy.DefaultLevel, cfg.Autonomy)
}

func TestResolveConfig_Autonomy(t *testing.T) {
	t.Setenv(githubTokenEnv, "token")
	dir := t.TempDir()
	path := filepath.Join(dir, "WORKFLOW.md")

	cfg, err := ResolveConfig(t.Context(), map[string]any{
		"autonomy": "full",
		"tracker": map[string]any{
			"kind":       "github",
			"repository": "openai/symphony",
		},
	}, path)
	require.NoError(t, err)
	assert.Equal(t, autonomy.Full, cfg.Autonomy)
}

func TestWorkflowManager_AutonomyOverridePersistsAcrossReloads(t *testing.T) {
	t.Setenv(githubTokenEnv, "token")
	dir := t.TempDir()
	path := filepath.Join(dir, "WORKFLOW.md")
	writeWorkflow := func(level, prompt string) {
		t.Helper()
		require.NoError(t, os.WriteFile(path, []byte("---\nautonomy: "+level+"\ntracker:\n  kind: github\n  repository: openai/symphony\n---\n"+prompt+"\n"), 0o600))
	}

	writeWorkflow("low", "first")

	manager, err := NewWorkflowManager(dir, path)
	require.NoError(t, err)
	require.NoError(t, manager.SetAutonomyOverride(autonomy.Full))

	snapshot, err := manager.Load(t.Context())
	require.NoError(t, err)
	assert.Equal(t, autonomy.Full, snapshot.Config.Autonomy)

	writeWorkflow("medium", "second prompt with different size")

	future := time.Now().Add(time.Second)
	require.NoError(t, os.Chtimes(path, future, future))

	snapshot, changed, err := manager.ReloadIfChanged(t.Context())
	require.NoError(t, err)
	require.True(t, changed)
	assert.Equal(t, autonomy.Full, snapshot.Config.Autonomy)
}

func TestWorkflowManager_AutonomyOverrideAppliesBeforeGitHubCLIFallback(t *testing.T) {
	t.Setenv(githubTokenEnv, "")
	t.Setenv(githubCLITokenEnv, "")
	auditDir := filepath.Join(t.TempDir(), "audit")
	t.Setenv(shell.EnvAuditDir, auditDir)

	dir := t.TempDir()
	path := filepath.Join(dir, "WORKFLOW.md")
	require.NoError(t, os.WriteFile(path, []byte("---\nautonomy: high\ntracker:\n  kind: github\n  repository: openai/symphony\n---\nDo the issue.\n"), 0o600))

	_, err := ValidateWorkflowWithOptions(t.Context(), Options{WorkflowPath: path, Autonomy: autonomy.Low})

	require.Error(t, err)
	assert.Contains(t, err.Error(), "missing_github_token")
	assert.NoFileExists(t, filepath.Join(auditDir, "commands.jsonl"))
}

func TestResolveConfig_RejectsInvalidAutonomy(t *testing.T) {
	t.Setenv(githubTokenEnv, "token")
	dir := t.TempDir()
	path := filepath.Join(dir, "WORKFLOW.md")

	_, err := ResolveConfig(t.Context(), map[string]any{
		"autonomy": "unsafe",
		"tracker": map[string]any{
			"kind":       "github",
			"repository": "openai/symphony",
		},
	}, path)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "unsupported autonomy")
}

func TestResolveConfig_PermissionPolicyDeniesTrackerEnvCredentialAccess(t *testing.T) {
	t.Parallel()

	policy := permission.DefaultPolicy()
	policy.SetMode(permission.OperationCredentialAccess, permission.ModeDeny)

	auditDir := t.TempDir()
	ctx := permission.ContextWithPolicy(t.Context(), &policy)
	ctx = permission.ContextWithAuditDir(ctx, auditDir)

	dir := t.TempDir()
	path := filepath.Join(dir, "WORKFLOW.md")

	_, err := ResolveConfig(ctx, map[string]any{
		"tracker": map[string]any{
			"kind":       "github",
			"repository": "openai/symphony",
		},
	}, path)
	require.Error(t, err)
	require.True(t, permission.ErrDenied(err))
	assert.Contains(t, err.Error(), "permission.credential_access.deny")

	auditData, readErr := os.ReadFile(filepath.Join(auditDir, "side_effects.jsonl"))
	require.NoError(t, readErr)
	assert.Contains(t, string(auditData), "resolve GitHub token from environment")
	assert.Contains(t, string(auditData), "permission.credential_access.deny")
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
	assert.Equal(t, PullRequestNoChecksPass, cfg.Publish.NoChecksPolicy)
	assert.True(t, cfg.Publish.DraftOnFailedValidation)
	assert.Equal(t, int64(defaultPRGateOutputBytes), cfg.Publish.VerificationOutputMaxBytes)
	assert.True(t, cfg.Publish.DiscoverRequiredChecks)
}

func TestResolveConfig_PublishRequiresRemoveLabelsForScheduler(t *testing.T) {
	t.Setenv(githubTokenEnv, "token")
	dir := t.TempDir()
	path := filepath.Join(dir, "WORKFLOW.md")

	_, err := ResolveConfig(t.Context(), map[string]any{
		"tracker": map[string]any{
			"kind":       "github",
			"repository": "openai/symphony",
		},
		"publish": map[string]any{
			"enabled": true,
		},
	}, path)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "publish.remove_labels is required")
}

func TestResolveConfig_PublishVerificationGates(t *testing.T) {
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
			"enabled":                       true,
			"draft_on_failed_validation":    false,
			"verification_output_max_bytes": 4096,
			"verification_allow_commands":   []any{"go", "test"},
			"verification_deny_commands":    []any{"curl"},
			"verification_gates": []any{
				"go_test",
				map[string]any{
					"name":       "docs",
					"command":    "test -f README.md",
					"required":   false,
					"timeout_ms": 1234,
				},
				map[string]any{
					"name":       "golangci-lint",
					"required":   false,
					"timeout_ms": 5678,
				},
			},
		},
	}, path)
	require.NoError(t, err)

	require.Len(t, cfg.Publish.VerificationGates, 3)
	assert.Equal(t, VerificationGateConfig{
		Name:     "go_test",
		Command:  "go test ./...",
		Timeout:  defaultPRGateTimeout,
		Required: true,
	}, cfg.Publish.VerificationGates[0])
	assert.Equal(t, VerificationGateConfig{
		Name:     "docs",
		Command:  "test -f README.md",
		Timeout:  1234 * time.Millisecond,
		Required: false,
	}, cfg.Publish.VerificationGates[1])
	assert.Equal(t, VerificationGateConfig{
		Name:     "golangci_lint",
		Command:  "make lint",
		Timeout:  5678 * time.Millisecond,
		Required: false,
	}, cfg.Publish.VerificationGates[2])
	assert.False(t, cfg.Publish.DraftOnFailedValidation)
	assert.Equal(t, int64(4096), cfg.Publish.VerificationOutputMaxBytes)
	assert.Equal(t, []string{"go", "test"}, cfg.Publish.VerificationAllowCommands)
	assert.Equal(t, []string{"curl"}, cfg.Publish.VerificationDenyCommands)
}

func TestResolveConfig_RejectsUnsupportedPublishVerificationPreset(t *testing.T) {
	t.Setenv(githubTokenEnv, "token")
	dir := t.TempDir()
	path := filepath.Join(dir, "WORKFLOW.md")

	_, err := ResolveConfig(t.Context(), map[string]any{
		"tracker": map[string]any{
			"kind":       "github",
			"repository": "openai/symphony",
			"labels":     []any{"symphony"},
		},
		"publish": map[string]any{
			"enabled":            true,
			"verification_gates": []any{"security_scan"},
		},
	}, path)
	require.Error(t, err)

	assert.Contains(t, err.Error(), `publish.verification_gates "security_scan" requires command or a supported preset name`)
}

func TestResolveConfig_RejectsInvalidPublishVerificationGateEntry(t *testing.T) {
	t.Setenv(githubTokenEnv, "token")
	dir := t.TempDir()
	path := filepath.Join(dir, "WORKFLOW.md")

	_, err := ResolveConfig(t.Context(), map[string]any{
		"tracker": map[string]any{
			"kind":       "github",
			"repository": "openai/symphony",
			"labels":     []any{"symphony"},
		},
		"publish": map[string]any{
			"enabled":            true,
			"verification_gates": []any{123},
		},
	}, path)
	require.Error(t, err)

	assert.Contains(t, err.Error(), "publish.verification_gates entries require name or command")
}

func TestResolveConfig_RejectsInvalidPublishVerificationGateTimeout(t *testing.T) {
	t.Setenv(githubTokenEnv, "token")
	dir := t.TempDir()
	path := filepath.Join(dir, "WORKFLOW.md")

	_, err := ResolveConfig(t.Context(), map[string]any{
		"tracker": map[string]any{
			"kind":       "github",
			"repository": "openai/symphony",
			"labels":     []any{"symphony"},
		},
		"publish": map[string]any{
			"enabled": true,
			"verification_gates": []any{
				map[string]any{
					"name":       "unit",
					"command":    "go test ./...",
					"timeout_ms": 0,
				},
			},
		},
	}, path)
	require.Error(t, err)

	assert.Contains(t, err.Error(), `publish.verification_gates "unit" timeout_ms must be > 0`)
}

func TestResolveConfig_RejectsCaseInsensitiveDuplicateVerificationGateNames(t *testing.T) {
	t.Setenv(githubTokenEnv, "token")
	dir := t.TempDir()
	path := filepath.Join(dir, "WORKFLOW.md")

	_, err := ResolveConfig(t.Context(), map[string]any{
		"tracker": map[string]any{
			"kind":       "github",
			"repository": "openai/symphony",
			"labels":     []any{"symphony"},
		},
		"publish": map[string]any{
			"enabled": true,
			"verification_gates": []any{
				map[string]any{"name": "unit", "command": "go test ./..."},
				map[string]any{"name": "UNIT", "command": "go test ./..."},
			},
		},
	}, path)
	require.Error(t, err)

	assert.Contains(t, err.Error(), `publish.verification_gates contains duplicate gate "UNIT"`)
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
			"required_checks":           []any{"test", "lint"},
			"required_check_patterns":   []any{"ci/*"},
			"no_checks_policy":          "pending",
			"discover_required_checks":  false,
			"rework_optional_checks":    true,
		},
	}, path)
	require.NoError(t, err)

	assert.True(t, cfg.Publish.MonitorChecks)
	assert.Equal(t, 1234*time.Millisecond, cfg.Publish.CheckInterval)
	assert.Equal(t, 5, cfg.Publish.MaxCheckReworkAttempts)
	assert.Equal(t, []string{"test", "lint"}, cfg.Publish.RequiredCheckNames)
	assert.Equal(t, []string{"ci/*"}, cfg.Publish.RequiredCheckPatterns)
	assert.Equal(t, PullRequestNoChecksPending, cfg.Publish.NoChecksPolicy)
	assert.False(t, cfg.Publish.DiscoverRequiredChecks)
	assert.True(t, cfg.Publish.ReworkOptionalChecks)
}

func TestResolveConfig_FullPublishRequiresCheckMonitoring(t *testing.T) {
	t.Setenv(githubTokenEnv, "token")
	dir := t.TempDir()
	path := filepath.Join(dir, "WORKFLOW.md")

	_, err := ResolveConfig(t.Context(), map[string]any{
		"autonomy": "full",
		"tracker": map[string]any{
			"kind":       "github",
			"repository": "openai/symphony",
			"labels":     []any{"symphony"},
		},
		"publish": map[string]any{
			"enabled": true,
		},
	}, path)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "autonomy full with publish.enabled requires publish.monitor_checks: true")
	assert.Contains(t, err.Error(), "use autonomy high")
}

func TestResolveConfig_RejectsInvalidPublishNoChecksPolicy(t *testing.T) {
	t.Setenv(githubTokenEnv, "token")
	dir := t.TempDir()
	path := filepath.Join(dir, "WORKFLOW.md")

	_, err := ResolveConfig(t.Context(), map[string]any{
		"tracker": map[string]any{
			"kind":       "github",
			"repository": "openai/symphony",
			"labels":     []any{"symphony"},
		},
		"publish": map[string]any{
			"enabled":          true,
			"monitor_checks":   true,
			"no_checks_policy": "ignore",
		},
	}, path)
	require.Error(t, err)

	assert.Contains(t, err.Error(), "publish.no_checks_policy must be one of pass, pending, fail")
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

func TestResolveConfig_CodexConfigPassThrough(t *testing.T) {
	t.Setenv(githubTokenEnv, "token")
	dir := t.TempDir()
	path := filepath.Join(dir, "WORKFLOW.md")

	cfg, err := ResolveConfig(t.Context(), map[string]any{
		"tracker": map[string]any{
			"kind":       "github",
			"repository": "openai/symphony",
		},
		"codex": map[string]any{
			"config": map[string]any{
				"model": "gpt-5.5",
				"experimental": map[string]any{
					"reasoning": "high",
				},
			},
		},
	}, path)
	require.NoError(t, err)

	assert.Equal(t, map[string]any{
		"model": "gpt-5.5",
		"experimental": map[string]any{
			"reasoning": "high",
		},
	}, cfg.Codex.ExtraConfig)
}

func TestResolveConfig_RejectsArbitraryCodexKeysOutsideConfig(t *testing.T) {
	t.Setenv(githubTokenEnv, "token")
	dir := t.TempDir()
	path := filepath.Join(dir, "WORKFLOW.md")

	_, err := ResolveConfig(t.Context(), map[string]any{
		"tracker": map[string]any{
			"kind":       "github",
			"repository": "openai/symphony",
		},
		"codex": map[string]any{
			"model": "gpt-5.5",
		},
	}, path)
	require.Error(t, err)

	assert.Contains(t, err.Error(), `unknown workflow config field "codex.model"`)
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

func TestRunGitHubCLI_PermissionPolicyDeniesCredentialAccessBeforeExecution(t *testing.T) {
	binDir := t.TempDir()
	markerPath := filepath.Join(t.TempDir(), "ran")
	ghPath := filepath.Join(binDir, "gh")
	script := "#!/usr/bin/env sh\nprintf ran > " + strconv.Quote(markerPath) + "\nprintf '%s\\n' 'cli-token'\n"
	require.NoError(t, os.WriteFile(ghPath, []byte(script), 0o700))
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))

	policy := permission.DefaultPolicy()
	policy.SetMode(permission.OperationCredentialAccess, permission.ModeDeny)

	auditDir := t.TempDir()
	ctx := permission.ContextWithPolicy(t.Context(), &policy)
	ctx = permission.ContextWithAuditDir(ctx, auditDir)

	token := runGitHubCLI(ctx, "auth", "token")

	assert.Empty(t, token)

	_, markerErr := os.Stat(markerPath)
	require.True(t, os.IsNotExist(markerErr), "fake gh should not execute when credential access is denied")

	auditData, err := os.ReadFile(filepath.Join(auditDir, "side_effects.jsonl"))
	require.NoError(t, err)
	assert.Contains(t, string(auditData), "permission.credential_access.deny")
	assert.Contains(t, string(auditData), string(permission.OperationCredentialAccess))
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

func writeTestWorkflow(t *testing.T, frontMatter string) (dir, path string) {
	t.Helper()

	dir = t.TempDir()
	path = filepath.Join(dir, "WORKFLOW.md")
	require.NoError(t, os.WriteFile(path, []byte("---\n"+frontMatter+"---\nPrompt\n"), 0o600))

	return dir, path
}
