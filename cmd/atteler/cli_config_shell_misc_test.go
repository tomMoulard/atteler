package main

import (
	"bufio"
	"context"
	"encoding/json"
	"flag"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/tommoulard/atteler/pkg/agent"
	"github.com/tommoulard/atteler/pkg/autonomy"
	appconfig "github.com/tommoulard/atteler/pkg/config"
	"github.com/tommoulard/atteler/pkg/events"
	"github.com/tommoulard/atteler/pkg/llm"
	"github.com/tommoulard/atteler/pkg/permission"
	"github.com/tommoulard/atteler/pkg/session"
	attshell "github.com/tommoulard/atteler/pkg/shell"
)

const (
	testUnifiedDiffPatch = "--- a/file.txt\n+++ b/file.txt\n@@ -1 +1 @@\n-old\n+new\n"
	liveOutputTimeout    = 5 * time.Second
)

func TestMergeTags_DeduplicatesCaseInsensitive(t *testing.T) {
	t.Parallel()

	got := mergeTags([]string{"auth"}, []string{"Auth", "review", " "})

	want := []string{"auth", "review"}
	if !reflect.DeepEqual(got, want) {
		require.Failf(t, "unexpected failure", "tags = %v, want %v", got, want)
	}
}

func TestWorktreeMergeProvenance_IncludesSessionMetadata(t *testing.T) {
	t.Parallel()

	got := worktreeMergeProvenance(session.Session{
		ID:    "session-123",
		Title: "GH-83 worktree hardening",
		Tags:  []string{"security", "symphony"},
	})

	assert.Equal(t, []string{
		"session=session-123",
		"title=GH-83 worktree hardening",
		"tags=security,symphony",
	}, got)
}

func TestRecordFailure_SavesNegativeKnowledge(t *testing.T) {
	t.Parallel()
	store := session.NewStore(t.TempDir())
	sessionState := session.New("gpt-test", nil)

	if err := recordFailure(t.Context(), store, sessionState, "try cache bust", "broke auth", "abc123", "reviewer"); err != nil {
		require.NoError(t, err)
	}

	loaded, err := store.Load(sessionState.ID)
	if err != nil {
		require.NoError(t, err)
	}

	if len(loaded.NegativeKnowledge) != 1 {
		require.Failf(t, "unexpected negative knowledge", "entries = %+v", loaded.NegativeKnowledge)
	}

	entry := loaded.NegativeKnowledge[0]
	if entry.Approach != "try cache bust" || entry.Reason != "broke auth" || entry.Commit != "abc123" || entry.Agent != "reviewer" {
		require.Failf(t, "unexpected negative knowledge", "entry = %+v", entry)
	}
}

func TestRecordFailureDetails_SavesCategorizedNegativeKnowledge(t *testing.T) {
	t.Parallel()

	store := session.NewStore(t.TempDir())
	sessionState := session.New("gpt-test", nil)

	err := recordFailureDetails(t.Context(), store, sessionState, session.NegativeKnowledge{
		Approach: "skip tests",
		Reason:   "missed regression",
		Agent:    "reviewer",
		TaskType: "migration",
		Severity: "high",
	})
	require.NoError(t, err)

	loaded, err := store.Load(sessionState.ID)
	require.NoError(t, err)
	require.Len(t, loaded.NegativeKnowledge, 1)
	assert.Equal(t, "migration", loaded.NegativeKnowledge[0].TaskType)
	assert.Equal(t, "high", loaded.NegativeKnowledge[0].Severity)
}

func TestRecordFailureDetailsPermissionPolicyDeniesSessionWrite(t *testing.T) {
	t.Parallel()

	store := session.NewStore(t.TempDir())
	sessionState := session.New("gpt-test", nil)
	policy := permission.DefaultPolicy()
	policy.SetMode(permission.OperationWrite, permission.ModeDeny)

	ctx := permission.ContextWithPolicy(t.Context(), &policy)
	ctx = permission.ContextWithAuditDir(ctx, t.TempDir())
	err := recordFailureDetails(ctx, store, sessionState, session.NegativeKnowledge{
		Approach: "skip tests",
		Reason:   "missed regression",
		Agent:    "reviewer",
	})

	require.Error(t, err)
	assert.True(t, permission.ErrDenied(err))
	assert.Contains(t, err.Error(), "permission.write.deny")
	assert.NoFileExists(t, store.Path(sessionState.ID))
}

func TestPathStatus(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	if got := pathStatus(t.Context(), dir); got != "ok" {
		require.Failf(t, "unexpected failure", "pathStatus(dir) = %q, want ok", got)
	}

	missing := filepath.Join(dir, "missing")
	if got := pathStatus(t.Context(), missing); got != "will be created on first save" {
		require.Failf(t, "unexpected failure", "pathStatus(missing) = %q", got)
	}
}

func TestLLMConfigCarriesProviderRetryPolicy(t *testing.T) {
	t.Parallel()

	maxAttempts := 3
	initialBackoffMS := 50
	maxBackoffMS := 500
	maxElapsedMS := 5000
	jitterFraction := 0.4

	got := llmConfig(appconfig.Config{
		DefaultProvider: "openai",
		DefaultModel:    "gpt-default",
		Providers: map[string]appconfig.ProviderConfig{
			"openai": {
				BaseURL:        "https://openai.example.test",
				TimeoutSeconds: 30,
				Retry: appconfig.RetryConfig{
					MaxAttempts:      &maxAttempts,
					InitialBackoffMS: &initialBackoffMS,
					MaxBackoffMS:     &maxBackoffMS,
					MaxElapsedMS:     &maxElapsedMS,
					JitterFraction:   &jitterFraction,
				},
			},
		},
	}, "gpt-selected", nil, "", nil)

	assert.Equal(t, "openai", got.DefaultProvider)
	assert.Equal(t, "gpt-default", got.DefaultModel)
	assert.Equal(t, "gpt-selected", got.SelectedModel)

	provider := got.Providers["openai"]
	assert.Equal(t, "https://openai.example.test", provider.BaseURL)
	assert.Equal(t, 30, provider.TimeoutSeconds)
	require.NotNil(t, provider.Retry.MaxAttempts)
	assert.Equal(t, maxAttempts, *provider.Retry.MaxAttempts)
	require.NotNil(t, provider.Retry.InitialBackoffMS)
	assert.Equal(t, initialBackoffMS, *provider.Retry.InitialBackoffMS)
	require.NotNil(t, provider.Retry.MaxBackoffMS)
	assert.Equal(t, maxBackoffMS, *provider.Retry.MaxBackoffMS)
	require.NotNil(t, provider.Retry.MaxElapsedMS)
	assert.Equal(t, maxElapsedMS, *provider.Retry.MaxElapsedMS)
	require.NotNil(t, provider.Retry.JitterFraction)
	assert.InEpsilon(t, jitterFraction, *provider.Retry.JitterFraction, 0.0001)
}

func TestLLMConfigPreservesExplicitFalseCredentialPolicy(t *testing.T) {
	t.Parallel()

	got := llmConfig(appconfig.Config{
		Providers: map[string]appconfig.ProviderConfig{
			"codex": {
				CredentialPolicy: appconfig.CredentialPolicyConfig{
					Configured:            true,
					AllowBorrowedOAuthSet: true,
					AllowRefreshSet:       true,
					AllowWriteBackSet:     true,
					AllowBorrowedOAuth:    false,
					AllowRefresh:          false,
					AllowWriteBack:        false,
				},
			},
		},
	}, "", nil, "", nil)

	policy := got.Providers["codex"].CredentialPolicy
	assert.True(t, policy.Configured)
	assert.False(t, policy.AllowBorrowedOAuth)
	assert.False(t, policy.AllowRefresh)
	assert.False(t, policy.AllowWriteBack)
}

func TestLLMConfigPreservesExplicitEmptyCredentialPolicyLists(t *testing.T) {
	t.Parallel()

	got := llmConfig(appconfig.Config{
		Providers: map[string]appconfig.ProviderConfig{
			"codex": {
				CredentialPolicy: appconfig.CredentialPolicyConfig{
					Configured:          true,
					AllowedProviders:    []string{},
					AllowedProvidersSet: true,
					AllowedStores:       []string{},
					AllowedStoresSet:    true,
				},
			},
		},
	}, "", nil, "", nil)

	policy := got.Providers["codex"].CredentialPolicy
	assert.True(t, policy.Configured)
	assert.Empty(t, policy.AllowedProviders)
	assert.True(t, policy.AllowedProvidersSet)
	assert.Empty(t, policy.AllowedStores)
	assert.True(t, policy.AllowedStoresSet)
}

func TestRetryPolicyInfoForConfig_AppliesOverridesAndNormalization(t *testing.T) {
	t.Parallel()

	maxAttempts := 4
	initialBackoffMS := 25
	maxBackoffMS := 250
	maxElapsedMS := 500
	jitterFraction := 0.3

	info := retryPolicyInfoForConfig("openai", appconfig.ProviderConfig{
		Retry: appconfig.RetryConfig{
			MaxAttempts:      &maxAttempts,
			InitialBackoffMS: &initialBackoffMS,
			MaxBackoffMS:     &maxBackoffMS,
			MaxElapsedMS:     &maxElapsedMS,
			JitterFraction:   &jitterFraction,
		},
	})

	assert.Equal(t, maxAttempts, info.MaxAttempts)
	assert.Equal(t, 25*time.Millisecond, info.InitialBackoff)
	assert.Equal(t, 250*time.Millisecond, info.MaxBackoff)
	assert.Equal(t, 500*time.Millisecond, info.MaxElapsedTime)
	assert.InEpsilon(t, jitterFraction, info.JitterFraction, 0.0001)

	negative := -1
	tooHigh := 2.0
	info = retryPolicyInfoForConfig("openai", appconfig.ProviderConfig{
		Retry: appconfig.RetryConfig{
			MaxAttempts:    &negative,
			JitterFraction: &tooHigh,
		},
	})

	assert.Equal(t, 0, info.MaxAttempts)
	assert.InEpsilon(t, 1.0, info.JitterFraction, 0.0001)
}

func TestDoctorOfflinePrintsProviderRetryPolicies(t *testing.T) {
	dir := t.TempDir()
	configPath := filepath.Join(dir, "config.yaml")
	require.NoError(t, os.WriteFile(configPath, []byte(`
providers:
  openai:
    retry:
      max_attempts: 4
      initial_backoff_ms: 25
      max_backoff_ms: 250
      max_elapsed_ms: 500
      jitter_fraction: 0.3
  custom-gateway:
    retry:
      max_attempts: 1
`), 0o600))

	t.Setenv(appconfig.EnvPath, configPath)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(dir, "xdg"))

	out := captureStdoutForConfigTest(t, func() {
		require.NoError(t, doctorOffline(t.Context(), cliOptions{sessionDir: filepath.Join(dir, "sessions")}))
	})

	assert.Contains(t, out, "provider_retries:")
	assert.Contains(t, out, "openai: max_retries=4 initial=25ms max_backoff=250ms budget=500ms jitter=30%")
	assert.Contains(t, out, "custom-gateway: max_retries=1 initial=1s max_backoff=10s budget=30s jitter=20%")
}

func TestDoctorPrintsHealthRetryPolicy(t *testing.T) { //nolint:paralleltest // captures process stdout
	r := llm.NewRegistry()
	r.Register(modelPickerProvider{
		name:   "healthy",
		models: []string{"healthy-model"},
	})
	r.SetProviderRetry("healthy", llm.RetryPolicy{
		MaxAttempts:    3,
		InitialBackoff: 20 * time.Millisecond,
		MaxBackoff:     200 * time.Millisecond,
		MaxElapsedTime: 2 * time.Second,
		JitterFraction: 0.25,
	})

	out := captureStdoutForConfigTest(t, func() {
		require.NoError(t, doctor(context.Background(), appState{
			registry:      r,
			agentRegistry: agent.NewRegistry(nil),
			sessionStore:  session.NewStore(t.TempDir()),
		}))
	})

	assert.Contains(t, out, "[ok] healthy")
	assert.Contains(t, out, "retry: max_retries=3 initial=20ms max_backoff=200ms budget=2s jitter=25%")
	assert.Contains(t, out, "- healthy-model")
}

func captureStdoutForConfigTest(t *testing.T, fn func()) string {
	t.Helper()

	oldStdout := os.Stdout

	defer func() {
		os.Stdout = oldStdout
	}()

	reader, writer, err := os.Pipe()
	require.NoError(t, err)

	os.Stdout = writer

	fn()

	require.NoError(t, writer.Close())

	out, err := io.ReadAll(reader)
	require.NoError(t, err)

	return string(out)
}

func TestPathStatusPermissionPolicyDeniesRead(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	auditDir := filepath.Join(dir, "audit")
	policy := permission.DefaultPolicy()
	policy.SetMode(permission.OperationRead, permission.ModeDeny)

	ctx := permission.ContextWithPolicy(t.Context(), &policy)
	ctx = permission.ContextWithAuditDir(ctx, auditDir)

	got := pathStatus(ctx, dir)
	assert.Contains(t, got, "permission.read.deny")

	auditData, err := os.ReadFile(filepath.Join(auditDir, "side_effects.jsonl"))
	require.NoError(t, err)
	assert.Contains(t, string(auditData), "inspect session path status")
	assert.Contains(t, string(auditData), "permission.read.deny")
}

func TestFormatAgentDescription(t *testing.T) {
	t.Parallel()

	temperature := 0.1
	seed := 99

	out, err := formatAgentDescription(agent.Agent{
		Name:           "reviewer",
		Model:          "gpt-test",
		Description:    "Reviews code",
		Personality:    "concise",
		FallbackModels: []string{"gpt-fallback"},
		Capabilities:   []string{"review"},
		Triggers:       []string{"review this"},
		Temperature:    &temperature,
		Seed:           &seed,
		ReasoningLevel: "high",
		MaxTokens:      100,
	})
	if err != nil {
		require.NoError(t, err)
	}

	for _, want := range []string{
		"name: reviewer",
		"model: gpt-test",
		"description: Reviews code",
		"personality: concise",
		"capabilities:",
		"fallback_models:",
		"triggers:",
		"temperature: 0.1",
		"seed: 99",
		"reasoning_level: high",
		"max_tokens: 100",
	} {
		if !strings.Contains(out, want) {
			require.Failf(t, "unexpected failure", "agent description missing %q in:\n%s", want, out)
		}
	}
}

func TestFormatTokenUsageSummary(t *testing.T) {
	t.Parallel()

	got := formatTokenUsageSummary(tokenUsage{InputTokens: 1500, CachedInputTokens: 500, OutputTokens: 42, Responses: 2})

	want := "tokens:\tin=1.5k\tcached=500\tout=42\tresponses=2"
	if got != want {
		require.Failf(t, "unexpected token usage summary", "got %q, want %q", got, want)
	}

	got = formatTokenUsageSummary(tokenUsage{InputTokens: 1500, CachedInputTokens: 500, CacheWriteInputTokens: 250, OutputTokens: 42, Responses: 2})

	want = "tokens:\tin=1.5k\tcached=500\tout=42\tcache_write=250\tresponses=2"
	if got != want {
		require.Failf(t, "unexpected token usage summary", "got %q, want %q", got, want)
	}
}

func TestFormatTokenCount(t *testing.T) {
	t.Parallel()

	tests := []struct {
		want  string
		input int
	}{
		{"0", 0},
		{"1", 1},
		{"999", 999},
		{"1k", 1000},
		{"1.5k", 1500},
		{"4.1k", 4096},
		{"128k", 128_000},
		{"200k", 200_000},
		{"1.0M", 1_000_000},
		{"1.0M", 1_047_576},
		{"2.5M", 2_500_000},
	}
	for _, tt := range tests {
		got := formatTokenCount(tt.input)
		if got != tt.want {
			assert.Failf(t, "assertion failed", "formatTokenCount(%d) = %q, want %q", tt.input, got, tt.want)
		}
	}
}

func TestApplyDebugEnvOptions(t *testing.T) {
	t.Parallel()

	values := map[string]string{
		"DEBUG_ATTELER_DOCTOR":                    "true",
		"DEBUG_ATTELER_DOCTOR_OFFLINE":            "true",
		"DEBUG_ATTELER_CONFIG_REPORT":             "true",
		"DEBUG_ATTELER_EXPLAIN_CONFIG":            "true",
		"DEBUG_ATTELER_EXPLAIN_CONFIG_FIELD":      "providers.openai",
		"DEBUG_ATTELER_STATE_DIAGNOSTICS":         "true",
		"DEBUG_ATTELER_LIST_HOOK_EVENTS":          "true",
		"DEBUG_ATTELER_LIST_HOOK_EVENTS_JSON":     "true",
		"DEBUG_ATTELER_OLLAMA_STATUS":             "true",
		"DEBUG_ATTELER_WATCH_SCAN":                "1",
		"DEBUG_ATTELER_WATCH_JSON":                "1",
		"DEBUG_ATTELER_REVIEW_PLAN":               "true",
		"DEBUG_ATTELER_REVIEW_RUN":                "true",
		"DEBUG_ATTELER_AGENT_PERFORMANCE_SUMMARY": "true",
		"DEBUG_ATTELER_MCP_MANIFEST":              "mcp.yaml",
		"DEBUG_ATTELER_MCP_CAPABILITY":            "symbols",
		"DEBUG_ATTELER_MCP_SERVER":                "repo",
		"DEBUG_ATTELER_MCP_TOOL":                  "search",
		"DEBUG_ATTELER_MCP_TOOL_ARGS":             `{"query":"symbols"}`,
		"DEBUG_ATTELER_LSP_SYMBOLS":               "yes",
		"DEBUG_ATTELER_LSP_COMMAND":               "gopls",
		"DEBUG_ATTELER_LSP_ARGS":                  "serve",
		"DEBUG_ATTELER_LSP_FILE":                  "main.go",
		"DEBUG_ATTELER_LSP_WORKSPACE_SYMBOLS":     "Handler",
		"DEBUG_ATTELER_WATCH_MAX_ITERATIONS":      "3",
		"DEBUG_ATTELER_WATCH_INTERVAL_SECONDS":    "5",
	}
	opts := cliOptions{}

	applyDebugEnvOptions(&opts, func(name string) string { return values[name] })

	assert.True(t, opts.doctor)
	assert.True(t, opts.doctorOffline)
	assert.True(t, opts.configReport)
	assert.True(t, opts.explainConfig)
	assert.Equal(t, "providers.openai", opts.explainConfigPath)
	assert.True(t, opts.stateDiagnostics)
	assert.True(t, opts.listHookEvents)
	assert.True(t, opts.listHookEventsJSON)
	assert.True(t, opts.ollamaStatus)
	assert.True(t, opts.watchScan)
	assert.True(t, opts.watchJSON)
	assert.True(t, opts.reviewPlan)
	assert.True(t, opts.reviewRun)
	assert.True(t, opts.agentPerformanceSummary)
	assert.Equal(t, "mcp.yaml", opts.mcpManifestPath)
	assert.Equal(t, "symbols", opts.mcpCapability)
	assert.Equal(t, "repo", opts.mcpServerName)
	assert.Equal(t, "search", opts.mcpToolName)
	assert.JSONEq(t, `{"query":"symbols"}`, opts.mcpToolArgsJSON)
	assert.True(t, opts.lspSymbols)
	assert.Equal(t, "gopls", opts.lspCommand)
	assert.Equal(t, rawStringListFlag{"serve"}, opts.lspArgs)
	assert.Equal(t, "main.go", opts.lspFilePath)
	assert.Equal(t, "Handler", opts.lspWorkspaceSymbols)
	assert.Equal(t, 3, opts.watchMaxIterations.value)
	assert.True(t, opts.watchMaxIterations.set)
	assert.Equal(t, 5, opts.watchIntervalSeconds.value)
	assert.True(t, opts.watchIntervalSeconds.set)
}

func TestApplyDebugEnvOptionsDoesNotOverrideExplicitOptions(t *testing.T) {
	t.Parallel()

	opts := cliOptions{
		mcpManifestPath: "explicit.yaml",
		watchMaxIterations: positiveIntFlag{
			value: 2,
			set:   true,
		},
	}

	applyDebugEnvOptions(&opts, func(name string) string {
		switch name {
		case "DEBUG_ATTELER_MCP_MANIFEST":
			return "env.yaml"
		case "DEBUG_ATTELER_WATCH_MAX_ITERATIONS":
			return "9"
		default:
			return ""
		}
	})

	assert.Equal(t, "explicit.yaml", opts.mcpManifestPath)
	assert.Equal(t, 2, opts.watchMaxIterations.value)
}

func TestApplyDebugEnvOptionsExplainConfigFieldActivatesExplain(t *testing.T) {
	t.Parallel()

	opts := cliOptions{}

	applyDebugEnvOptions(&opts, func(name string) string {
		if name == "DEBUG_ATTELER_EXPLAIN_CONFIG_FIELD" {
			return "providers.openai"
		}

		return ""
	})

	assert.True(t, opts.explainConfig)
	assert.Equal(t, "providers.openai", opts.explainConfigPath)
}

func TestFormatShellContext(t *testing.T) {
	t.Parallel()

	t.Run("stdout only", func(t *testing.T) {
		t.Parallel()

		got := formatShellContext(shellResultMsg{
			command: "ls",
			stdout:  "a.go\nb.go\n",
		})
		assert.Equal(t, "$ ls\na.go\nb.go", got)
	})

	t.Run("stdout and stderr", func(t *testing.T) {
		t.Parallel()

		got := formatShellContext(shellResultMsg{
			command: "ls /nope",
			stdout:  "",
			stderr:  "ls: /nope: No such file or directory\n",
		})
		assert.Equal(t, "$ ls /nope\n[stderr]\nls: /nope: No such file or directory", got)
	})

	t.Run("includes error message", func(t *testing.T) {
		t.Parallel()

		got := formatShellContext(shellResultMsg{
			command: "false",
			err:     assert.AnError,
		})
		assert.Contains(t, got, "$ false")
		assert.Contains(t, got, "[error] "+assert.AnError.Error())
	})
}

func TestUpdateShellResult_ClearsCompletedTaskTimer(t *testing.T) {
	t.Parallel()

	startedAt := time.Date(2026, 5, 5, 12, 0, 0, 0, time.UTC)
	m := model{
		runningTaskLabel:   "command",
		runningTaskStarted: startedAt,
		runningTaskID:      3,
		sessionState:       session.New("gpt-test", nil),
	}

	nextModel, cmd := m.updateShellResult(shellResultMsg{
		completedAt: startedAt.Add(2 * time.Second),
		command:     "echo hi",
		stdout:      "hi\n",
	})
	next, ok := nextModel.(model)
	require.True(t, ok)
	require.NotNil(t, cmd)
	assert.False(t, next.waiting)
	assert.Empty(t, next.runningTaskLabel)
	assert.True(t, next.runningTaskStarted.IsZero())
}

func TestRunShellCommandCmd_StreamsOutputBeforeCompletion(t *testing.T) {
	t.Parallel()

	outputCh := make(chan tea.Msg, 2)
	done := make(chan any, 1)

	go func() {
		done <- runShellCommandCmd(context.Background(), `printf first; sleep 0.4; printf second`, "", outputCh, attshell.AuditContext{}, nil, nil)()
	}()

	chunk := requireShellOutputBefore(t, outputCh, liveOutputTimeout)
	assert.Equal(t, "first", chunk.data)
	assert.Equal(t, string(attshell.OutputStreamStdout), chunk.stream)
	assert.Equal(t, int64(1), chunk.sequence)

	select {
	case msg := <-done:
		require.Failf(t, "command completed before delayed output", "msg=%+v", msg)
	default:
	}

	select {
	case msg := <-nextShellResult(t, outputCh):
		require.NoError(t, msg.err)
		assert.True(t, msg.streamed)
		assert.Equal(t, "firstsecond", msg.stdout)
	case <-time.After(time.Second):
		require.FailNow(t, "timed out waiting for command completion")
	}

	select {
	case raw := <-done:
		assert.Nil(t, raw)
	case <-time.After(time.Second):
		require.FailNow(t, "timed out waiting for command goroutine")
	}
}

func TestRunShellCommandCmd_StreamsStderrBeforeCompletion(t *testing.T) {
	t.Parallel()

	outputCh := make(chan tea.Msg, 2)
	done := make(chan any, 1)

	go func() {
		done <- runShellCommandCmd(context.Background(), `printf warn >&2; sleep 0.4; printf done >&2`, "", outputCh, attshell.AuditContext{}, nil, nil)()
	}()

	chunk := requireShellOutputBefore(t, outputCh, liveOutputTimeout)
	assert.Equal(t, "warn", chunk.data)
	assert.Equal(t, string(attshell.OutputStreamStderr), chunk.stream)

	select {
	case msg := <-done:
		require.Failf(t, "command completed before delayed stderr", "msg=%+v", msg)
	default:
	}

	select {
	case msg := <-nextShellResult(t, outputCh):
		require.NoError(t, msg.err)
		assert.True(t, msg.streamed)
		assert.Equal(t, "warndone", msg.stderr)
	case <-time.After(time.Second):
		require.FailNow(t, "timed out waiting for command completion")
	}
}

//nolint:paralleltest // Temporarily redirects process stdout.
func TestRunBashCommand_StreamsStdoutBeforeCompletion(t *testing.T) {
	store := session.NewStore(t.TempDir())
	stdout := captureProcessOutput(t, &os.Stdout)

	done := make(chan error, 1)

	go func() {
		done <- runBashCommand(context.Background(), appState{
			cwd:          t.TempDir(),
			sessionStore: store,
			sessionState: session.New("gpt-test", nil),
		}, bashCommandInput{
			Command:        `printf 'live\n'; sleep 0.4; printf 'done\n'`,
			TimeoutSeconds: 2,
		})
	}()

	assert.Equal(t, "live\n", requireLineBefore(t, stdout.lines, liveOutputTimeout))

	select {
	case err := <-done:
		require.Failf(t, "command completed before delayed stdout", "err=%v", err)
	default:
	}

	require.NoError(t, <-done)
}

//nolint:paralleltest // Temporarily redirects process stderr.
func TestRunBashCommand_StreamsStderrBeforeCompletion(t *testing.T) {
	store := session.NewStore(t.TempDir())
	stderr := captureProcessOutput(t, &os.Stderr)

	done := make(chan error, 1)

	go func() {
		done <- runBashCommand(context.Background(), appState{
			cwd:          t.TempDir(),
			sessionStore: store,
			sessionState: session.New("gpt-test", nil),
		}, bashCommandInput{
			Command:        `printf 'warn\n' >&2; sleep 0.4; printf 'done\n' >&2`,
			TimeoutSeconds: 2,
		})
	}()

	assert.Equal(t, "warn\n", requireLineBefore(t, stderr.lines, liveOutputTimeout))

	select {
	case err := <-done:
		require.Failf(t, "command completed before delayed stderr", "err=%v", err)
	default:
	}

	require.NoError(t, <-done)
}

//nolint:paralleltest // Temporarily redirects process stdout.
func TestRunBashCommand_EmitsCommandEventTimeline(t *testing.T) {
	store := session.NewStore(t.TempDir())
	recorder := newEventLogRecorder()
	stdout := captureProcessOutput(t, &os.Stdout)

	done := make(chan error, 1)

	go func() {
		done <- runBashCommand(context.Background(), appState{
			cwd:          t.TempDir(),
			hookRunner:   events.NewRunnerWithLogger(nil, recorder),
			sessionStore: store,
			sessionState: session.New("gpt-test", nil),
		}, bashCommandInput{
			Command:        `printf 'live\n'; sleep 0.4; printf 'done\n'`,
			TimeoutSeconds: 2,
		})
	}()

	assert.Equal(t, "live\n", requireLineBefore(t, stdout.lines, liveOutputTimeout))
	partial := recorder.requireLineContainingBefore(t, "partial=true", liveOutputTimeout)
	assert.Contains(t, partial, "event:command_output")
	assert.Contains(t, partial, "source=cli")
	assert.Contains(t, partial, "stream=stdout")

	select {
	case err := <-done:
		require.Failf(t, "command completed before partial output event was observed", "err=%v", err)
	default:
	}

	require.NoError(t, <-done)

	lines := recorder.Lines()
	assertLineOrder(t, lines, "event:command_execute", "partial=true", "partial=false")
}

func TestRunBashCommand_PermissionPolicyDeniesWritesBeforeProcessStart(t *testing.T) {
	t.Parallel()

	cwd := t.TempDir()
	recorder := newEventLogRecorder()
	policy := permission.ReadOnlyPolicy()
	err := runBashCommand(context.Background(), appState{
		cwd:              cwd,
		hookRunner:       events.NewRunnerWithLogger(nil, recorder),
		sessionStore:     session.NewStore(t.TempDir()),
		sessionState:     session.New("gpt-test", nil),
		permissionPolicy: &policy,
	}, bashCommandInput{
		Command:        "touch denied-by-policy",
		TimeoutSeconds: 2,
	})

	require.Error(t, err)
	assert.True(t, permission.ErrDenied(err))
	assert.NoFileExists(t, filepath.Join(cwd, "denied-by-policy"))
	assert.NotContains(t, strings.Join(recorder.Lines(), "\n"), "event:command_execute")
}

func TestRunBashCommand_PermissionAllowFlagsPermitExplicitHeadlessWrite(t *testing.T) {
	t.Parallel()

	var opts cliOptions

	opts.permissionMode = "read-only"
	opts.headless = true
	require.NoError(t, opts.allowOperations.Set("execute,write"))

	policy, err := permissionPolicyFromOptions(opts)
	require.NoError(t, err)

	cwd := t.TempDir()
	err = runBashCommand(contextWithPermissionPolicyForOptions(context.Background(), opts, policy), appState{
		cwd:              cwd,
		sessionStore:     session.NewStore(t.TempDir()),
		sessionState:     session.New("gpt-test", nil),
		permissionPolicy: policy,
	}, bashCommandInput{
		Command:        "printf allowed > allowed-by-policy",
		TimeoutSeconds: 2,
	})

	require.NoError(t, err)
	assert.FileExists(t, filepath.Join(cwd, "allowed-by-policy"))
}

func TestRunSpawnAgents_PermissionPolicyDeniesExecutionBeforeCommandEvent(t *testing.T) {
	t.Parallel()

	cwd := t.TempDir()
	recorder := newEventLogRecorder()
	policy := permission.DefaultPolicy()
	policy.SetMode(permission.OperationExecute, permission.ModeDeny)

	ctx := permission.ContextWithPolicy(context.Background(), &policy)
	err := runSpawnAgents(ctx, appState{
		cwd:              cwd,
		hookRunner:       events.NewRunnerWithLogger(nil, recorder),
		sessionStore:     session.NewStore(t.TempDir()),
		sessionState:     session.New("gpt-test", nil),
		permissionPolicy: &policy,
	}, spawnAgentsCommandInput{
		Specs:  []string{"worker|inspect the repository"},
		Binary: filepath.Join(cwd, "missing-atteler-binary"),
	})

	require.Error(t, err)
	assert.True(t, permission.ErrDenied(err))
	assert.NoDirExists(t, filepath.Join(cwd, ".atteler"))
	assert.NotContains(t, strings.Join(recorder.Lines(), "\n"), "event:command_execute")
}

func TestRunSpawnAgents_PermissionPolicyDeniesLedgerWriteBeforeLedgerCreation(t *testing.T) {
	t.Parallel()

	cwd := t.TempDir()
	auditDir := t.TempDir()
	ledgerPath := filepath.Join(t.TempDir(), "runs", "ledger.json")
	policy := permission.DefaultPolicy()
	policy.SetMode(permission.OperationWrite, permission.ModeDeny)

	ctx := permission.ContextWithPolicy(t.Context(), &policy)
	ctx = permission.ContextWithAuditDir(ctx, auditDir)
	err := runSpawnAgents(ctx, appState{
		cwd:              cwd,
		sessionStore:     session.NewStore(t.TempDir()),
		sessionState:     session.New("gpt-test", nil),
		permissionPolicy: &policy,
	}, spawnAgentsCommandInput{
		Specs:  []string{"worker|inspect the repository"},
		Binary: filepath.Join(cwd, "missing-atteler-binary"),
		Execution: childExecutionCommandInput{
			LedgerPath: ledgerPath,
		},
	})

	require.Error(t, err)
	assert.True(t, permission.ErrDenied(err))
	assert.Contains(t, err.Error(), "permission.write.deny")
	assert.NoFileExists(t, ledgerPath)

	auditData, readErr := os.ReadFile(filepath.Join(auditDir, "side_effects.jsonl"))
	require.NoError(t, readErr)
	assert.Contains(t, string(auditData), `"decision":"denied"`)
	assert.Contains(t, string(auditData), `"write"`)
}

type eventLogRecorder struct {
	lineCh chan string
	lines  []string
	mu     sync.Mutex
}

func newEventLogRecorder() *eventLogRecorder {
	return &eventLogRecorder{lineCh: make(chan string, 32)}
}

func (r *eventLogRecorder) Write(p []byte) (int, error) {
	if r == nil {
		return len(p), nil
	}

	text := strings.TrimRight(string(p), "\n")
	for line := range strings.SplitSeq(text, "\n") {
		if line == "" {
			continue
		}

		r.mu.Lock()
		r.lines = append(r.lines, line)
		r.mu.Unlock()

		select {
		case r.lineCh <- line:
		default:
		}
	}

	return len(p), nil
}

func (r *eventLogRecorder) Lines() []string {
	if r == nil {
		return nil
	}

	r.mu.Lock()
	defer r.mu.Unlock()

	return append([]string(nil), r.lines...)
}

func (r *eventLogRecorder) requireLineContainingBefore(t *testing.T, needle string, timeout time.Duration) string {
	t.Helper()

	deadline := time.After(timeout)

	for {
		select {
		case line := <-r.lineCh:
			if strings.Contains(line, needle) {
				return line
			}
		case <-deadline:
			require.FailNowf(t, "timed out waiting for event log line", "needle=%q lines=%v", needle, r.Lines())
		}
	}
}

func assertLineOrder(t *testing.T, lines []string, needles ...string) {
	t.Helper()

	next := 0
	for _, line := range lines {
		if next < len(needles) && strings.Contains(line, needles[next]) {
			next++
		}
	}

	require.Equalf(t, len(needles), next, "event log lines out of order: %v", lines)
}

type capturedProcessOutput struct {
	lines <-chan string
}

func captureProcessOutput(t *testing.T, target **os.File) capturedProcessOutput {
	t.Helper()

	original := *target
	reader, writer, err := os.Pipe()
	require.NoError(t, err)

	*target = writer

	t.Cleanup(func() {
		*target = original
		_ = writer.Close()
		_ = reader.Close()
	})

	lines := make(chan string, 1)

	go func() {
		line, err := bufio.NewReader(reader).ReadString('\n')
		if err != nil && line == "" {
			lines <- ""

			return
		}

		lines <- line
	}()

	return capturedProcessOutput{lines: lines}
}

func requireLineBefore(t *testing.T, lines <-chan string, timeout time.Duration) string {
	t.Helper()

	select {
	case line := <-lines:
		return line
	case <-time.After(timeout):
		require.FailNow(t, "timed out waiting for streamed output")
	}

	return ""
}

func TestRunShellCommandCmd_DeliversFinalResultWhenContextCanceled(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	outputCh := make(chan tea.Msg, 1)
	raw := runShellCommandCmd(ctx, `printf never`, "", outputCh, attshell.AuditContext{}, nil, nil)()
	assert.Nil(t, raw)

	select {
	case raw := <-outputCh:
		msg, ok := raw.(shellResultMsg)
		require.True(t, ok)
		require.Error(t, msg.err)
		assert.True(t, msg.streamed)
	case <-time.After(time.Second):
		require.FailNow(t, "timed out waiting for canceled command result")
	}
}

func TestRunShellCommandCmd_BlocksConfirmationOnlyCommands(t *testing.T) {
	t.Parallel()

	outputCh := make(chan tea.Msg, 1)
	raw := runShellCommandCmdWithAutonomy(context.Background(), "sudo true", "", outputCh, autonomy.Full, attshell.AuditContext{})()
	assert.Nil(t, raw)

	select {
	case raw := <-outputCh:
		msg, ok := raw.(shellResultMsg)
		require.True(t, ok)
		require.Error(t, msg.err)
		assert.Contains(t, msg.err.Error(), "requires confirmation")
		assert.Contains(t, msg.err.Error(), "TUI shell")
		assert.True(t, msg.streamed)
	case <-time.After(time.Second):
		require.FailNow(t, "timed out waiting for denied command result")
	}
}

func TestRunShellCommandCmd_DefaultsAuditAutonomy(t *testing.T) {
	t.Parallel()

	auditDir := filepath.Join(t.TempDir(), "audit")
	raw := runShellCommandCmdWithAutonomy(context.Background(), "printf ok", "", nil, autonomy.High, attshell.AuditContext{
		AuditDir: auditDir,
	})()
	msg, ok := raw.(shellResultMsg)
	require.True(t, ok)
	require.NoError(t, msg.err)
	assert.Equal(t, "ok", msg.stdout)

	records := readCommandAuditRecords(t, auditDir)
	require.NotEmpty(t, records)

	for _, record := range records {
		assert.Equal(t, "high", record.Autonomy)
	}
}

func requireShellOutputBefore(t *testing.T, outputCh <-chan tea.Msg, timeout time.Duration) shellOutputMsg {
	t.Helper()

	select {
	case raw := <-outputCh:
		chunk, ok := raw.(shellOutputMsg)
		require.True(t, ok)

		return chunk
	case <-time.After(timeout):
		require.FailNow(t, "timed out waiting for streamed shell output")
	}

	return shellOutputMsg{}
}

func nextShellResult(t *testing.T, outputCh <-chan tea.Msg) <-chan shellResultMsg {
	t.Helper()

	resultCh := make(chan shellResultMsg, 1)

	go func() {
		for raw := range outputCh {
			if msg, ok := raw.(shellResultMsg); ok {
				resultCh <- msg

				return
			}
		}
	}()

	return resultCh
}

func readCommandAuditRecords(t *testing.T, auditDir string) []attshell.AuditRecord {
	t.Helper()

	data, err := os.ReadFile(filepath.Join(auditDir, "commands.jsonl"))
	require.NoError(t, err)

	lines := strings.Split(strings.TrimSpace(string(data)), "\n")
	records := make([]attshell.AuditRecord, 0, len(lines))

	for _, line := range lines {
		if strings.TrimSpace(line) == "" {
			continue
		}

		var record attshell.AuditRecord
		require.NoError(t, json.Unmarshal([]byte(line), &record))

		records = append(records, record)
	}

	return records
}

func TestPruneToPinned_ReindexesPinnedMessages(t *testing.T) {
	t.Parallel()

	m := model{
		history: []llm.Message{
			{Role: llm.RoleUser, Content: "drop"},
			{Role: llm.RoleAssistant, Content: "keep one"},
			{Role: llm.RoleUser, Content: "drop too"},
			{Role: llm.RoleAssistant, Content: "keep two"},
		},
		sessionState:   session.New("gpt-test", nil),
		pinnedMessages: map[int]bool{1: true, 3: true},
	}

	m.pruneToPinned()

	require.Len(t, m.history, 2)
	assert.Equal(t, "keep one", m.history[0].Content)
	assert.Equal(t, "keep two", m.history[1].Content)
	assert.Equal(t, m.history, m.sessionState.Messages)
	assert.Equal(t, map[int]bool{0: true, 1: true}, m.pinnedMessages)
}

func TestMarshalJSONLines_CompactsAndTerminatesLines(t *testing.T) {
	t.Parallel()

	got, err := marshalJSONLines([]llm.Message{{Role: llm.RoleUser, Content: "hello"}, {Role: llm.RoleAssistant, Content: "world"}})
	require.NoError(t, err)

	lines := strings.Split(strings.TrimSpace(string(got)), "\n")
	require.Len(t, lines, 2)
	assert.JSONEq(t, `{"role":"user","content":"hello"}`, lines[0])
	assert.JSONEq(t, `{"role":"assistant","content":"world"}`, lines[1])
	assert.True(t, strings.HasSuffix(string(got), "\n"))
}

func TestApplyPatch_UsesDirectGitApplyCommand(t *testing.T) {
	t.Parallel()

	m := model{ctx: context.Background(), history: []llm.Message{{Role: llm.RoleAssistant, Content: testUnifiedDiffPatch}}}

	next, cmd, handled := m.handleSlashCommand("/apply-patch")
	require.True(t, handled)
	require.NotNil(t, cmd)
	require.True(t, next.waiting)
}

func TestApplyPatch_BlocksLowAutonomy(t *testing.T) {
	t.Parallel()

	m := model{
		ctx:      context.Background(),
		autonomy: autonomy.Low,
		history:  []llm.Message{{Role: llm.RoleAssistant, Content: testUnifiedDiffPatch}},
	}

	next, cmd, handled := m.handleSlashCommand("/apply-patch")
	require.True(t, handled)
	require.NotNil(t, cmd)
	require.False(t, next.waiting)
}

func TestRunGitApplyPatchCmd_InvokesGitDirectlyWithPatchOnStdin(t *testing.T) {
	dir := t.TempDir()
	auditDir := filepath.Join(dir, "audit")
	logPath := filepath.Join(dir, "git.log")
	stdinPath := filepath.Join(dir, "stdin.log")
	fakeGit := filepath.Join(dir, "git")
	script := "#!/bin/sh\n" +
		"printf '%s\\n' \"$*\" >> \"$ATTELER_FAKE_GIT_LOG\"\n" +
		"payload=$(cat)\n" +
		"printf '%s\\n---stdin---\\n' \"$payload\" >> \"$ATTELER_FAKE_GIT_STDIN\"\n"
	require.NoError(t, os.WriteFile(fakeGit, []byte(script), 0o600))
	require.NoError(t, os.Chmod(fakeGit, 0o700)) //nolint:gosec // the test creates an executable fake git shim in a private temp directory.
	t.Setenv("ATTELER_FAKE_GIT_LOG", logPath)
	t.Setenv("ATTELER_FAKE_GIT_STDIN", stdinPath)
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))

	startedApply := false
	cmd := runGitApplyPatchCmd(context.Background(), testUnifiedDiffPatch, "", attshell.AuditContext{
		AuditDir: auditDir,
		Caller:   "atteler.test.git_apply",
		Autonomy: autonomy.Medium.String(),
	}, func() {
		startedApply = true
	})

	msg, ok := cmd().(shellResultMsg)
	require.True(t, ok)
	require.NoError(t, msg.err)
	assert.Equal(t, "git apply --check - && git apply -", msg.command)
	assert.True(t, startedApply)

	log, err := os.ReadFile(logPath)
	require.NoError(t, err)
	assert.Equal(t, "apply --check -\napply -\n", string(log))

	stdin, err := os.ReadFile(stdinPath)
	require.NoError(t, err)
	assert.Equal(t, testUnifiedDiffPatch+"---stdin---\n"+testUnifiedDiffPatch+"---stdin---\n", string(stdin))

	records := readCommandAuditRecords(t, auditDir)
	require.Len(t, records, 4)

	for _, record := range records {
		assert.Equal(t, "atteler.test.git_apply", record.Caller)
		assert.Equal(t, "medium", record.Autonomy)
	}
}

func TestRunGitApplyPatchCmd_BlocksLowAutonomyBeforeGit(t *testing.T) {
	dir := t.TempDir()
	logPath := filepath.Join(dir, "git.log")
	fakeGit := filepath.Join(dir, "git")
	script := "#!/bin/sh\n" +
		"printf '%s\\n' \"$*\" >> \"$ATTELER_FAKE_GIT_LOG\"\n"
	require.NoError(t, os.WriteFile(fakeGit, []byte(script), 0o600))
	require.NoError(t, os.Chmod(fakeGit, 0o700)) //nolint:gosec // the test creates an executable fake git shim in a private temp directory.
	t.Setenv("ATTELER_FAKE_GIT_LOG", logPath)
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))

	cmd := runGitApplyPatchCmd(context.Background(), testUnifiedDiffPatch, "", attshell.AuditContext{
		AuditDir: filepath.Join(dir, "audit"),
		Autonomy: autonomy.Low.String(),
	})

	msg, ok := cmd().(shellResultMsg)
	require.True(t, ok)
	require.Error(t, msg.err)
	assert.Contains(t, msg.err.Error(), "autonomy low blocks mutating shell commands")
	assert.Contains(t, msg.stderr, "autonomy low blocks mutating shell commands")
	assert.NoFileExists(t, logPath)
}

func TestRunGitApplyPatchCmd_StopsWhenCheckFails(t *testing.T) {
	dir := t.TempDir()
	logPath := filepath.Join(dir, "git.log")
	fakeGit := filepath.Join(dir, "git")
	script := "#!/bin/sh\n" +
		"printf '%s\\n' \"$*\" >> \"$ATTELER_FAKE_GIT_LOG\"\n" +
		"if [ \"$1\" = \"apply\" ] && [ \"$2\" = \"--check\" ]; then\n" +
		"  printf 'bad patch\\n' >&2\n" +
		"  exit 42\n" +
		"fi\n"
	require.NoError(t, os.WriteFile(fakeGit, []byte(script), 0o600))
	require.NoError(t, os.Chmod(fakeGit, 0o700)) //nolint:gosec // the test creates an executable fake git shim in a private temp directory.
	t.Setenv("ATTELER_FAKE_GIT_LOG", logPath)
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))

	cmd := runGitApplyPatchCmd(context.Background(), testUnifiedDiffPatch, "", nil)

	msg, ok := cmd().(shellResultMsg)
	require.True(t, ok)
	require.Error(t, msg.err)
	assert.Contains(t, msg.err.Error(), "git apply --check -")
	assert.Contains(t, msg.stderr, "bad patch")

	log, err := os.ReadFile(logPath)
	require.NoError(t, err)
	assert.Equal(t, "apply --check -\n", string(log))
}

func TestRunGitApplyPatchCmd_PermissionPolicyDeniesWriteApply(t *testing.T) {
	dir := t.TempDir()
	logPath := filepath.Join(dir, "git.log")
	fakeGit := filepath.Join(dir, "git")
	script := "#!/bin/sh\n" +
		"printf '%s\\n' \"$*\" >> \"$ATTELER_FAKE_GIT_LOG\"\n"
	require.NoError(t, os.WriteFile(fakeGit, []byte(script), 0o600))
	require.NoError(t, os.Chmod(fakeGit, 0o700)) //nolint:gosec // the test creates an executable fake git shim in a private temp directory.
	t.Setenv("ATTELER_FAKE_GIT_LOG", logPath)
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))

	policy := permission.ReadOnlyPolicy()
	ctx := permission.ContextWithPolicy(context.Background(), &policy)
	startedApply := false
	cmd := runGitApplyPatchCmd(ctx, testUnifiedDiffPatch, "", func() {
		startedApply = true
	})

	msg, ok := cmd().(shellResultMsg)
	require.True(t, ok)
	require.Error(t, msg.err)
	assert.Contains(t, msg.err.Error(), "permission.git_mutation.deny")
	assert.False(t, startedApply)

	log, err := os.ReadFile(logPath)
	require.NoError(t, err)
	assert.Equal(t, "apply --check -\n", string(log))
}

func TestShellQuote_HandlesSingleQuotes(t *testing.T) {
	t.Parallel()

	quoted := shellQuote("/tmp/a'b.diff")
	assert.Equal(t, "'/tmp/a'\\''b.diff'", quoted)

	result, err := attshell.RunBash(context.Background(), attshell.Options{Command: "printf %s " + quoted})
	require.NoError(t, err)
	assert.Equal(t, "/tmp/a'b.diff", result.Stdout)
}

func TestDefaultValueForFlag_IncludesZeroAndImplicitDefaults(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		flag *flag.Flag
		want string
	}{
		{name: "empty string", flag: &flag.Flag{Name: "agent", DefValue: ""}, want: `""`},
		{name: "false bool", flag: &flag.Flag{Name: "doctor", DefValue: "false"}, want: "false"},
		{name: "zero numeric", flag: &flag.Flag{Name: "evaluation-score", DefValue: "0"}, want: "0"},
		{name: "implicit runtime default", flag: &flag.Flag{Name: "memory-limit", DefValue: ""}, want: "5"},
		{name: "implicit disabled default", flag: &flag.Flag{Name: "memory-retention-days", DefValue: ""}, want: "disabled"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			if got := defaultValueForFlag(tt.flag); got != tt.want {
				t.Fatalf("defaultValueForFlag() = %q, want %q", got, tt.want)
			}
		})
	}
}
