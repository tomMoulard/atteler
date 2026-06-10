package main

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/tommoulard/atteler/pkg/autonomy"
	appconfig "github.com/tommoulard/atteler/pkg/config"
	"github.com/tommoulard/atteler/pkg/llm"
	"github.com/tommoulard/atteler/pkg/mcp"
	attelerplugin "github.com/tommoulard/atteler/pkg/plugin"
	"github.com/tommoulard/atteler/pkg/session"
)

func TestAutonomyFromConfigOptions(t *testing.T) {
	t.Parallel()

	level, err := autonomyFromConfigOptions(appconfig.Config{Autonomy: "high"}, cliOptions{})
	require.NoError(t, err)
	assert.Equal(t, autonomy.High, level)

	var opts cliOptions
	require.NoError(t, opts.autonomy.Set("low"))
	level, err = autonomyFromConfigOptions(appconfig.Config{Autonomy: "high"}, opts)
	require.NoError(t, err)
	assert.Equal(t, autonomy.Low, level)
}

func TestAutonomyFromConfigOptionsRejectsInvalidConfig(t *testing.T) {
	t.Parallel()

	_, err := autonomyFromConfigOptions(appconfig.Config{Autonomy: "unsafe"}, cliOptions{})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "unsupported autonomy")
}

func TestProviderAutoStartDisabledByAutonomy(t *testing.T) {
	t.Parallel()

	assert.True(t, shouldDisableProviderAutoStart(cliOptions{}, autonomy.Low))
	assert.False(t, shouldDisableProviderAutoStart(cliOptions{}, autonomy.Medium))
	assert.False(t, shouldDisableProviderAutoStart(cliOptions{}, autonomy.High))
	assert.True(t, shouldDisableProviderAutoStart(cliOptions{listModels: true}, autonomy.High))
}

func TestAutonomyForEarlyCommandUsesConfigAndCLIOverride(t *testing.T) {
	configPath := filepath.Join(t.TempDir(), "config.yaml")
	require.NoError(t, os.WriteFile(configPath, []byte("autonomy: low\n"), 0o600))
	t.Setenv(appconfig.EnvPath, configPath)

	level, err := autonomyForEarlyCommand(cliOptions{})
	require.NoError(t, err)
	assert.Equal(t, autonomy.Low, level)

	var opts cliOptions
	require.NoError(t, opts.autonomy.Set("full"))
	level, err = autonomyForEarlyCommand(opts)
	require.NoError(t, err)
	assert.Equal(t, autonomy.Full, level)
}

func TestProviderlessStateUsesCLIAutonomyOverride(t *testing.T) {
	configPath := filepath.Join(t.TempDir(), "config.yaml")
	require.NoError(t, os.WriteFile(configPath, []byte("autonomy: high\n"), 0o600))
	t.Setenv(appconfig.EnvPath, configPath)

	var opts cliOptions
	require.NoError(t, opts.autonomy.Set("low"))
	state, err := providerlessState(context.Background(), session.NewStore(filepath.Join(t.TempDir(), "sessions")), opts)

	require.NoError(t, err)
	assert.Equal(t, autonomy.Low, state.autonomy)
}

func TestProviderlessStateRejectsInvalidAutonomyConfig(t *testing.T) {
	configPath := filepath.Join(t.TempDir(), "config.yaml")
	require.NoError(t, os.WriteFile(configPath, []byte("autonomy: unsafe\n"), 0o600))
	t.Setenv(appconfig.EnvPath, configPath)

	_, err := providerlessState(context.Background(), session.NewStore(filepath.Join(t.TempDir(), "sessions")), cliOptions{})

	require.Error(t, err)
	assert.Contains(t, err.Error(), "unsupported autonomy")
}

func TestSetupWorktreeIfRequestedRequiresHighAutonomy(t *testing.T) {
	t.Parallel()

	selection := selectionState{sessionState: session.New("test-model", nil)}
	_, err := setupWorktreeIfRequested(
		context.Background(),
		cliOptions{useWorktree: true},
		t.TempDir(),
		&selection,
		cliWorktreeMergePolicy{},
		autonomy.Medium,
	)

	require.Error(t, err)
	assert.Contains(t, err.Error(), "autonomy medium blocks branch creation")
	assert.Contains(t, err.Error(), "--autonomy high or full")
	assert.Empty(t, selection.sessionState.WorktreePath)
}

func TestMergeWorktreeBySessionRequiresHighAutonomy(t *testing.T) {
	t.Parallel()

	err := mergeWorktreeBySession(context.Background(), "session-id", cliWorktreeMergePolicy{}, autonomy.Medium)

	require.Error(t, err)
	assert.Contains(t, err.Error(), "autonomy medium blocks git commits")
	assert.Contains(t, err.Error(), "--autonomy high or full")
}

func TestWorktreeShellAuditContextIncludesAutonomy(t *testing.T) {
	t.Parallel()

	got := worktreeShellAuditContext(session.Session{ID: "session-id"}, autonomy.High)
	assert.Equal(t, "atteler.worktree.git", got.Caller)
	assert.Equal(t, "session-id", got.SessionID)
	assert.Equal(t, "high", got.Autonomy)
}

func TestRunSpawnAgentsBlocksLowAutonomy(t *testing.T) {
	t.Parallel()

	err := runSpawnAgents(context.Background(), appState{autonomy: autonomy.Low}, spawnAgentsCommandInput{
		Specs: []string{"reviewer|check the diff"},
	})

	require.Error(t, err)
	assert.Contains(t, err.Error(), "autonomy low blocks mutating shell commands")
	assert.Contains(t, err.Error(), "--autonomy medium or higher")
}

func TestRunAsyncTasksBlocksLowAutonomy(t *testing.T) {
	t.Parallel()

	err := runAsyncTasks(context.Background(), appState{autonomy: autonomy.Low}, asyncRunCommandInput{
		TaskSpecs: []string{"task-1|reviewer|check the diff"},
	})

	require.Error(t, err)
	assert.Contains(t, err.Error(), "autonomy low blocks mutating shell commands")
	assert.Contains(t, err.Error(), "--async-run")
}

func TestAuthorizeBashCommandBlocksLowAutonomyEvenForReadOnlyCommands(t *testing.T) {
	t.Parallel()

	err := authorizeBashCommandWithAutonomy(context.Background(), autonomy.Low, "pwd", "--bash")

	require.Error(t, err)
	assert.Contains(t, err.Error(), "autonomy low is advisory-only")
	assert.Contains(t, err.Error(), "--autonomy medium or higher")
}

func TestSaveSessionSkipsLowAutonomyPersistence(t *testing.T) {
	t.Parallel()

	store := session.NewStore(filepath.Join(t.TempDir(), "sessions"))
	sessionState := session.New("gpt-test", []llm.Message{{Role: llm.RoleUser, Content: "plan only"}})
	sessionState.Autonomy = autonomy.Low.String()

	msg, ok := saveSession(context.Background(), store, sessionState, nil)().(sessionSavedMsg)
	require.True(t, ok)
	require.NoError(t, msg.err)
	assert.NoFileExists(t, store.Path(sessionState.ID))
}

func TestRunStatefulSessionWriteBlocksLowAutonomy(t *testing.T) {
	t.Parallel()

	store := session.NewStore(filepath.Join(t.TempDir(), "sessions"))
	sessionState := session.New("test-model", nil)
	err := runStatefulSessionWriteCommand(context.Background(), cliOptions{
		recordFailure: "overfit prompt",
		failureReason: "missed guardrail",
	}, appState{
		autonomy:      autonomy.Low,
		sessionStore:  store,
		sessionState:  sessionState,
		selectedAgent: "reviewer",
	})

	require.Error(t, err)
	assert.Contains(t, err.Error(), "autonomy low blocks file writes")
	assert.Contains(t, err.Error(), "record-failure")
}

func TestStatefulRegistryBlocksLowAutonomyChildAndArtifactWrites(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name    string
		opts    cliOptions
		command *command
		want    string
	}{
		{
			name: "async-run",
			opts: cliOptions{asyncRun: true},
			command: mustSelectRegistryCommand(t,
				statefulExecutionCommands(),
				cliOptions{asyncRun: true},
			),
			want: "--async-run",
		},
		{
			name: "merge-artifacts",
			opts: cliOptions{mergeArtifactsPath: "-"},
			command: mustSelectRegistryCommand(t,
				statefulRetrievalCommands(),
				cliOptions{mergeArtifactsPath: "-"},
			),
			want: "--merge-artifacts",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			err := tc.command.runStateful(context.Background(), tc.opts, appState{autonomy: autonomy.Low})
			require.Error(t, err)
			assert.Contains(t, err.Error(), tc.want)
		})
	}
}

func TestProviderlessConfigCommandsBlockLowAutonomyWrites(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name string
		opts cliOptions
		want string
	}{
		{name: "feedback-approve", opts: cliOptions{feedbackApproveConfig: "config.yaml"}, want: "--feedback-approve"},
		{name: "feedback-rollback", opts: cliOptions{feedbackRollbackConfig: "config.yaml"}, want: "--feedback-rollback"},
		{name: "run-plugin", opts: cliOptions{runPluginTarget: "rtk/version"}, want: "--run-plugin"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			cmd := mustSelectRegistryCommand(t, providerlessConfigAgentPluginCommands(), tc.opts)
			err := cmd.runProviderlessConfig(context.Background(), tc.opts, appState{autonomy: autonomy.Low})
			require.Error(t, err)
			assert.Contains(t, err.Error(), tc.want)
		})
	}
}

func TestInlineCommandsBlockLowAutonomyWrites(t *testing.T) {
	t.Parallel()

	var low cliOptions
	require.NoError(t, low.autonomy.Set("low"))

	initPath := filepath.Join(t.TempDir(), "atteler.yaml")
	handled, err := runInlineConfigCommand(context.Background(), cliOptions{
		autonomy:       low.autonomy,
		initConfigPath: initPath,
	})
	require.True(t, handled)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "autonomy low blocks file writes")
	assert.Contains(t, err.Error(), "--init-config")
	assert.NoFileExists(t, initPath)

	handled, err = runInlineConfigCommand(context.Background(), cliOptions{
		autonomy:      low.autonomy,
		configMigrate: true,
	})
	require.True(t, handled)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "--config-migrate")

	handled, err = runInlineCommand(context.Background(), cliOptions{
		autonomy:   low.autonomy,
		ollamaStop: true,
	})
	require.True(t, handled)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "--ollama-stop")
}

func TestShouldReconcileHeadlessRunsAtStartupRespectsAutonomy(t *testing.T) {
	var low cliOptions
	require.NoError(t, low.autonomy.Set("low"))
	assert.False(t, shouldReconcileHeadlessRunsAtStartup(low))

	var medium cliOptions
	require.NoError(t, medium.autonomy.Set("medium"))
	assert.True(t, shouldReconcileHeadlessRunsAtStartup(medium))

	configPath := filepath.Join(t.TempDir(), "config.yaml")
	require.NoError(t, os.WriteFile(configPath, []byte("autonomy: unsafe\n"), 0o600))
	t.Setenv(appconfig.EnvPath, configPath)

	assert.False(t, shouldReconcileHeadlessRunsAtStartup(cliOptions{}))
}

func TestRecordHeadlessLoadStateFailureRespectsConfigAutonomy(t *testing.T) {
	configPath := filepath.Join(t.TempDir(), "config.yaml")
	require.NoError(t, os.WriteFile(configPath, []byte("autonomy: low\n"), 0o600))
	t.Setenv(appconfig.EnvPath, configPath)

	store := session.NewStore(filepath.Join(t.TempDir(), "sessions"))
	recordHeadlessLoadStateFailure(context.Background(), store, cliOptions{headless: true, headlessID: "load-failed"}, errors.New("load failed"))

	assert.NoDirExists(t, filepath.Join(store.Dir(), "headless"))
}

func TestHeadlessEvalAndRetrievalBlockLowAutonomyWrites(t *testing.T) {
	t.Parallel()

	err := runHeadlessCommandWithAutonomy(context.Background(), cliOptions{listHeadless: true}, session.NewStore(t.TempDir()), autonomy.Low)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "--list-headless")

	err = evalOutputCommandWithAutonomy(cliOptions{evalReportPath: filepath.Join(t.TempDir(), "report.json")}, autonomy.Low)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "--eval-report")

	workspaceVectorEnabled := true
	err = runRetrievalCommand(context.Background(), appState{
		autonomy: autonomy.Low,
		vectorConfig: appconfig.VectorConfig{
			WorkspaceEnabled: &workspaceVectorEnabled,
		},
	}, retrievalCommandInput{
		Search:  "query",
		Sources: []string{"vector"},
	})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "--retrieval-source vector")

	err = runRetrievalCommand(context.Background(), appState{
		autonomy:     autonomy.Low,
		sessionStore: session.NewStore(t.TempDir()),
	}, retrievalCommandInput{
		Search:  "query",
		Sources: []string{"session"},
	})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "--retrieval-source session")
}

func TestSearchSessionsBlocksLowAutonomyIndexWrites(t *testing.T) {
	t.Parallel()

	err := searchSessionsWithAutonomy(session.NewStore(t.TempDir()), "query", autonomy.Low)

	require.Error(t, err)
	assert.Contains(t, err.Error(), "autonomy low blocks file writes")
	assert.Contains(t, err.Error(), "--search")
}

func TestGitHistorySearchBlocksLowAutonomyBeforeShellExecution(t *testing.T) {
	t.Parallel()

	err := runGitHistorySearch(context.Background(), t.TempDir(), "query", 1, autonomy.Low)

	require.Error(t, err)
	assert.Contains(t, err.Error(), "autonomy low is advisory-only")
	assert.Contains(t, err.Error(), "--autonomy medium")
}

func TestMCPInvokeBlocksLowAutonomyToolExecution(t *testing.T) {
	t.Parallel()

	var opts cliOptions
	require.NoError(t, opts.autonomy.Set("low"))
	opts.mcpMethod = "tools/list"

	cmd := mustSelectRegistryCommand(t, providerlessPlanningCommands(), opts)
	err := cmd.runProviderless(context.Background(), opts, session.NewStore(t.TempDir()))

	require.Error(t, err)
	assert.Contains(t, err.Error(), "autonomy low blocks mutating shell commands")
	assert.Contains(t, err.Error(), "--mcp-method/--mcp-tool")
}

func TestMCPServerCommandRespectsAutonomyPolicy(t *testing.T) {
	t.Parallel()

	err := authorizeMCPServerCommandWithAutonomy(context.Background(), mcp.Server{
		Command: "cat",
		Args:    []string{"server.json"},
	}, autonomy.Low)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "autonomy low")
	assert.Contains(t, err.Error(), "mcp server command execution")

	err = authorizeMCPServerCommandWithAutonomy(context.Background(), mcp.Server{
		Command: "git",
		Args:    []string{"push", "origin", "HEAD"},
	}, autonomy.Medium)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "autonomy medium blocks git pushes")
	assert.Contains(t, err.Error(), "--autonomy high or full")

	err = authorizeMCPServerCommandWithAutonomy(context.Background(), mcp.Server{
		Command: "sudo",
		Args:    []string{"true"},
	}, autonomy.High)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "requires confirmation")
}

func TestPluginRunAutonomyRequiresHighForNetworkPlugins(t *testing.T) {
	t.Parallel()

	manifest := attelerplugin.Manifest{
		Permissions: &attelerplugin.PermissionSet{
			Network: attelerplugin.NetworkPermissions{Allow: true},
		},
	}

	err := authorizePluginRunAutonomy(autonomy.Medium, manifest)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "autonomy medium blocks remote service mutations")
	assert.Contains(t, err.Error(), "--autonomy high or full")

	require.NoError(t, authorizePluginRunAutonomy(autonomy.High, manifest))
}

func TestSessionRunEventMetadataIncludesAutonomy(t *testing.T) {
	t.Parallel()

	metadata := sessionRunEventMetadata(llm.AgentLoopBudget{MaxToolCalls: 2}, autonomy.Full)
	assert.Equal(t, "full", metadata["autonomy"])

	var budget llm.AgentLoopBudget
	require.NoError(t, json.Unmarshal([]byte(metadata["agent_loop_budget"]), &budget))
	assert.Equal(t, 2, budget.MaxToolCalls)
}

func TestPrependAutonomyInstructionsLowAdvisory(t *testing.T) {
	t.Parallel()

	params := llm.CompleteParams{Messages: []llm.Message{{Role: llm.RoleUser, Content: "implement GH-123"}}}
	prependAutonomyInstructions(&params, autonomy.Low)

	require.Len(t, params.Messages, 2)
	assert.Equal(t, llm.RoleSystem, params.Messages[0].Role)
	assert.Contains(t, params.Messages[0].Content, "Advisory-only")
	assert.Contains(t, params.Messages[0].Content, "--autonomy medium")
}

func mustSelectRegistryCommand(t *testing.T, commands []command, opts cliOptions) *command {
	t.Helper()

	cmd, handled, err := selectRegistryCommand(commands, tierStateful, opts)
	if !handled {
		cmd, handled, err = selectRegistryCommand(commands, tierProviderlessConfig, opts)
	}

	if !handled {
		cmd, handled, err = selectRegistryCommand(commands, tierProviderless, opts)
	}

	require.NoError(t, err)
	require.True(t, handled)

	return cmd
}
