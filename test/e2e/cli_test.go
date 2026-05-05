package e2e

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

var (
	e2eBinary string
	e2eTmpDir string
)

const windowsGOOS = "windows"

func TestMain(m *testing.M) {
	root, err := repoRoot()
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}

	e2eTmpDir, err = os.MkdirTemp("", "atteler-e2e-*")
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}

	e2eBinary = filepath.Join(e2eTmpDir, "atteler")
	if runtime.GOOS == windowsGOOS {
		e2eBinary += ".exe"
	}

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	cmd := exec.CommandContext(ctx, "go", "build", "-o", e2eBinary, "./cmd/atteler")
	cmd.Dir = root
	cmd.Env = os.Environ()
	output, err := cmd.CombinedOutput()

	cancel()

	if err != nil {
		fmt.Fprintf(os.Stderr, "build atteler: %v\n%s", err, output)

		_ = os.RemoveAll(e2eTmpDir)

		os.Exit(1)
	}

	code := m.Run()
	_ = os.RemoveAll(e2eTmpDir)

	os.Exit(code)
}

func TestConfigCommands(t *testing.T) {
	t.Parallel()
	workDir := t.TempDir()
	configPath := filepath.Join(workDir, "atteler.yaml")

	result := runOK(t, runSpec{dir: workDir}, "--print-config-template")
	assertContains(t, result.stdout, "default_provider: openai")
	assertNotContains(t, result.stdout, "api.openai.com/v1")

	result = runOK(t, runSpec{dir: workDir}, "--init-config", configPath)
	assertContains(t, result.stdout, "Wrote "+configPath)

	if _, err := os.Stat(configPath); err != nil {
		require.Failf(t, "unexpected failure", "config was not written: %v", err)
	}

	_, err := runAtteler(t, runSpec{dir: workDir}, "--init-config", configPath)
	if err == nil {
		require.FailNow(t, "expected init-config to refuse overwriting an existing file")
	}

	result = runOK(t, runSpec{dir: workDir}, "--config", configPath, "--validate-config")
	assertContains(t, result.stdout, "Config valid:")

	result = runOK(t, runSpec{dir: workDir}, "--config", configPath, "--list-config-paths")
	assertContains(t, result.stdout, configPath+"\tpresent")
}

func TestOfflineProviderCommands(t *testing.T) {
	t.Parallel()
	workDir := t.TempDir()

	result := runOK(t, runSpec{dir: workDir}, "--version")
	assertContains(t, result.stdout, "atteler")

	result = runOK(t, runSpec{dir: workDir}, "--doctor-offline")
	assertContains(t, result.stdout, "Atteler offline doctor")
	assertContains(t, result.stdout, "known_providers:")
	assertContains(t, result.stdout, "ollama")
	assertContains(t, result.stdout, "hook_events:")

	result = runOK(t, runSpec{dir: workDir}, "--list-providers")
	assertContains(t, result.stdout, "anthropic")
	assertContains(t, result.stdout, "openai")

	result = runOK(t, runSpec{dir: workDir}, "--list-known-models")
	assertContains(t, result.stdout, "openai/gpt-4.1")
	assertContains(t, result.stdout, "anthropic/claude-sonnet")
	assertContains(t, result.stdout, "ollama/llama3.2")

	result = runOK(t, runSpec{dir: workDir, env: []string{"DEBUG_ATTELER_LIST_PROVIDERS=1"}})
	assertContains(t, result.stdout, "anthropic")
	assertContains(t, result.stdout, "openai")

	result = runOK(t, runSpec{dir: workDir}, "--list-hook-events")
	assertContains(t, result.stdout, "agent_execute")
	assertContains(t, result.stdout, "context_add")
	assertContains(t, result.stdout, "session_start")

	result = runOK(t, runSpec{dir: workDir}, "--list-hook-events-json")
	assertContains(t, result.stdout, `"type":"context_add"`)
	assertContains(t, result.stdout, `"description":`)
}

func TestReadOnlyCommandsDoNotAutoRegisterProviders(t *testing.T) {
	t.Parallel()
	workDir := t.TempDir()
	forgeDir := filepath.Join(workDir, "forge")

	var refreshRequests atomic.Int32

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		refreshRequests.Add(1)
		w.Header().Set("Content-Type", "application/json")

		if _, err := w.Write([]byte(`{"access_token":"refreshed-token","refresh_token":"refreshed-refresh","expires_in":3600}`)); err != nil {
			t.Errorf("write refresh response: %v", err)
		}
	}))
	defer server.Close()

	credentials := `[
  {"id":"claude_code","auth_details":{"o_auth":{
    "config":{"token_url":"` + server.URL + `","client_id":"client-123"},
    "tokens":{"access_token":"expired","refresh_token":"old-refresh","expires_at":"2000-01-01T00:00:00Z"}
  }}}
]
`
	credentialsPath := filepath.Join(forgeDir, ".credentials.json")
	writeFile(t, credentialsPath, credentials)

	result := runOK(t, runSpec{dir: workDir, env: []string{"FORGE_CONFIG=" + forgeDir}}, "--list-hook-events")
	assertContains(t, result.stdout, "session_start")

	data, err := os.ReadFile(credentialsPath)
	require.NoError(t, err)
	require.Equal(t, credentials, string(data))
	require.Equal(t, int32(0), refreshRequests.Load())
}

func TestAgentCommands(t *testing.T) {
	t.Parallel()
	workDir := t.TempDir()
	configPath := filepath.Join(workDir, "atteler.yaml")
	writeFile(t, configPath, `agents:
  reviewer:
    model: gpt-test
    triggers: ["review this"]
    temperature: 0
    system_prompt: Review code carefully.
`)

	result := runOK(t, runSpec{dir: workDir}, "--config", configPath, "--list-agents")
	assertContains(t, result.stdout, "reviewer")

	result = runOK(t, runSpec{dir: workDir}, "--config", configPath, "--describe-agent", "reviewer")
	assertContains(t, result.stdout, "name: reviewer")
	assertContains(t, result.stdout, "model: gpt-test")
	assertContains(t, result.stdout, "system_prompt: Review code carefully.")
}

func TestWorkflowUtilityCommands(t *testing.T) {
	t.Parallel()
	workDir := t.TempDir()
	sessionDir := filepath.Join(workDir, "sessions")
	writeSession(t, sessionDir)

	configPath := filepath.Join(workDir, "atteler.yaml")
	pluginDir := filepath.Join(workDir, "plugin")

	writeFile(t, configPath, `agents:
  reviewer:
    model: gpt-test
    capabilities: ["review", "security"]
    triggers: ["review this"]
plugins:
  paths: ["./plugin"]
`)
	writeFile(t, filepath.Join(pluginDir, "plugin.yaml"), `name: runner
version: "1.0.0"
description: Runner plugin
entrypoints:
  run: bin/run
`)
	writeExecutable(t, filepath.Join(pluginDir, "bin", "run"), `#!/bin/sh
printf 'plugin-output\n'
`)

	result := runOK(t, runSpec{dir: workDir}, "--config", configPath, "--plan-agents", "review this auth change")
	assertContains(t, result.stdout, "reviewer\tsource=trigger\tmatch=review this")

	actualPath := filepath.Join(workDir, "actual.txt")
	writeFile(t, actualPath, "hello deterministic eval\n")
	result = runOK(t, runSpec{dir: workDir}, "--eval-output", actualPath, "--eval-expected", "deterministic", "--eval-mode", "contains")
	assertContains(t, result.stdout, "PASS\tmode=contains")

	result = runOK(t, runSpec{dir: workDir}, "--config", configPath, "--describe-plugin", "runner")
	assertContains(t, result.stdout, "name: runner")
	assertContains(t, result.stdout, "entrypoints:")

	result = runOK(t, runSpec{dir: workDir}, "--config", configPath, "--run-plugin", "runner/run", "--plugin-dry-run")
	assertContains(t, result.stdout, `would run plugin "runner" entrypoint "run"`)

	result = runOK(t, runSpec{dir: workDir}, "--config", configPath, "--run-plugin", "runner", "--plugin-entrypoint", "run")
	assertContains(t, result.stdout, "plugin-output")

	if runtime.GOOS != windowsGOOS {
		result = runOK(t, runSpec{dir: workDir}, "--bash", "printf cli-bash")
		assertContains(t, result.stdout, "cli-bash")

		mcpHelper := filepath.Join(workDir, "mcp-helper")
		writeExecutable(t, mcpHelper, `#!/bin/sh
read line
printf '{"jsonrpc":"2.0","id":1,"result":{"ok":true,"source":"mcp-helper"}}\n'
`)

		mcpManifest := filepath.Join(workDir, "mcp.yaml")
		writeFile(t, mcpManifest, fmt.Sprintf(`servers:
  - name: helper
    command: %q
    capabilities: ["tools"]
`, mcpHelper))
		result = runOK(t, runSpec{dir: workDir},
			"--mcp-manifest", mcpManifest,
			"--mcp-server", "helper",
			"--mcp-tool", "echo",
			"--mcp-tool-args", `{"message":"hello"}`,
		)
		assertContains(t, result.stdout, `"ok": true`)
		assertContains(t, result.stdout, `"source": "mcp-helper"`)
	}

	result = runOK(t, runSpec{dir: workDir, sessionDir: sessionDir}, "--memory-search", "hello auth")
	assertContains(t, result.stdout, "session/demo/message/0")

	notesPath := filepath.Join(workDir, "notes.txt")
	memoryStore := filepath.Join(workDir, "memory.json")

	writeFile(t, notesPath, "OAuth callback notes\n")
	result = runOK(t, runSpec{dir: workDir}, "--memory-store", memoryStore, "--memory-index", notesPath)
	assertContains(t, result.stdout, "Indexed")
	result = runOK(t, runSpec{dir: workDir}, "--memory-store", memoryStore, "--memory-search", "callback")
	assertContains(t, result.stdout, notesPath)

	result = runOK(t, runSpec{dir: workDir}, "--skill-step", "plan", "--skill-step", "code", "--skill-step", "test", "--skill-step", "plan", "--skill-step", "code", "--skill-step", "test")
	assertContains(t, result.stdout, "slug: plan-code-test")
	assertContains(t, result.stdout, "occurrences: 2")

	skillDir := filepath.Join(workDir, "skills")
	result = runOK(t, runSpec{dir: workDir}, "--skill-save-dir", skillDir, "--skill-step", "plan", "--skill-step", "code", "--skill-step", "plan", "--skill-step", "code")
	assertContains(t, result.stdout, "saved: "+filepath.Join(skillDir, "plan-code.md"))
	skillData, err := os.ReadFile(filepath.Join(skillDir, "plan-code.md"))
	require.NoError(t, err)
	assertContains(t, string(skillData), "# Plan Code Skill")

	result = runOK(t, runSpec{dir: workDir},
		"--route-candidate", "openai/gpt-fast,input=0.001,output=0.002,max=1000",
		"--route-input-tokens", "100",
		"--route-output-tokens", "50",
	)
	assertContains(t, result.stdout, "openai/gpt-fast")
	assertContains(t, result.stdout, "cost=0.200000")

	result = runOK(t, runSpec{dir: workDir},
		"--route-candidate", "openai/too-expensive,input=0.01,output=0.01,max=1000",
		"--route-candidate", "openai/gpt-budget,input=0.001,output=0.001,max=1000",
		"--route-input-tokens", "100",
		"--route-output-tokens", "50",
		"--route-budget", "0.2",
	)
	assertContains(t, result.stdout, "openai/gpt-budget")
	assertNotContains(t, result.stdout, "openai/too-expensive")

	result = runOK(t, runSpec{dir: workDir},
		"--async-plan",
		"--async-task", "plan|planner|draft plan",
		"--async-task", "code|coder|implement|plan",
	)
	assertContains(t, result.stdout, "wave 1:")
	assertContains(t, result.stdout, "id=plan")
	assertContains(t, result.stdout, "wave 2:")
	assertContains(t, result.stdout, "id=code")

	result = runOK(t, runSpec{dir: workDir}, "--speculate-plan", "--speculate-agent", "alpha", "--speculate-agent", "beta", "--speculate-prompt", "implement auth flow")
	assertContains(t, result.stdout, "agents: alpha,beta")
	assertContains(t, result.stdout, "cross_reviews:")
	assertContains(t, result.stdout, "alpha -> beta")
	assertContains(t, result.stdout, "prompt_cache:")

	result = runOK(t, runSpec{dir: workDir}, "--review-plan", "--review-agent", "alpha", "--review-agent", "beta", "--review-path", "pkg/auth.go", "--review-gate", "tests pass")
	assertContains(t, result.stdout, "reviewers:")
	assertContains(t, result.stdout, "paths:\n  - pkg/auth.go")
	assertContains(t, result.stdout, "alpha -> beta")
	assertContains(t, result.stdout, "gates:\n  - tests pass")

	result = runOK(t, runSpec{dir: workDir}, "--spawn-agent", "reviewer|dry run this child prompt", "--spawn-dry-run")
	assertContains(t, result.stdout, "Would spawn 1 sub-agent")
	assertContains(t, result.stdout, "agent=reviewer")
	assertContains(t, result.stdout, "prompt=dry run this child prompt")

	taskFile := filepath.Join(workDir, "tasks.json")
	result = runOK(t, runSpec{dir: workDir}, "--task-file", taskFile, "--task-add", "draft the task list CLI", "--task-id", "todo-1", "--task-agent", "planner")
	assertContains(t, result.stdout, "id=todo-1")
	assertContains(t, result.stdout, "status=assigned")
	assertContains(t, result.stdout, "agent=planner")

	result = runOK(t, runSpec{dir: workDir}, "--task-file", taskFile, "--task-assign", "todo-1:executor")
	assertContains(t, result.stdout, "agent=executor")

	result = runOK(t, runSpec{dir: workDir}, "--task-file", taskFile, "--task-complete", "todo-1", "--task-agent", "verifier")
	assertContains(t, result.stdout, "status=completed")
	assertContains(t, result.stdout, "agent=verifier")

	result = runOK(t, runSpec{dir: workDir}, "--task-file", taskFile, "--task-list")
	assertContains(t, result.stdout, "id=todo-1")
	assertContains(t, result.stdout, "status=completed")

	writeFile(t, filepath.Join(workDir, "TODO-loop.txt"), "TODO: loop once\n")
	result = runOK(t, runSpec{dir: workDir}, "--watch-loop", "--watch-max-iterations", "1", "--watch-interval-seconds", "1")
	assertContains(t, result.stdout, "iteration=1")
	assertContains(t, result.stdout, "kind=stale_todo")

	result = runOK(t, runSpec{dir: workDir}, "--watch-scan", "--watch-json")
	assertContains(t, result.stdout, `"findings":`)
	assertContains(t, result.stdout, `"kind":"stale_todo"`)

	agentMemoryFile := filepath.Join(workDir, "agent-memory-note.txt")
	writeFile(t, agentMemoryFile, "OAuth callback retry memory\n")

	agentMemoryStore := filepath.Join(workDir, "agent-memory.json")
	result = runOK(t, runSpec{dir: workDir},
		"--agent-memory-agent", "reviewer",
		"--agent-memory-store", agentMemoryStore,
		"--agent-memory-index", agentMemoryFile,
		"--agent-memory-search", "callback retry",
	)
	assertContains(t, result.stdout, agentMemoryFile)

	artifactSessionDir := filepath.Join(workDir, "artifact-sessions")
	writeFile(t, filepath.Join(workDir, "research.md"), "artifact merge notes\n")
	writeFile(t, filepath.Join(artifactSessionDir, "artifact.json"), `{
  "created_at": "2026-05-02T10:00:00Z",
  "updated_at": "2026-05-02T10:01:00Z",
  "id": "artifact",
  "default_model": "gpt-test",
  "messages": [],
  "artifacts": [
    {"created_at": "2026-05-02T10:00:00Z", "path": "research.md", "kind": "research", "summary": "merge notes", "source_agent": "reviewer"}
  ]
}`)
	mergedArtifacts := filepath.Join(workDir, "merged-artifacts.md")
	result = runOK(t, runSpec{dir: workDir, sessionDir: artifactSessionDir},
		"--session", "artifact",
		"--merge-artifacts", mergedArtifacts,
	)
	assertContains(t, result.stdout, "Merged artifacts into "+mergedArtifacts)
	mergedData, err := os.ReadFile(mergedArtifacts)
	require.NoError(t, err)
	assertContains(t, string(mergedData), "artifact merge notes")

	feedbackSessionDir := filepath.Join(workDir, "feedback-sessions")
	writeFile(t, filepath.Join(feedbackSessionDir, "feedback.json"), `{
  "created_at": "2026-05-02T10:00:00Z",
  "updated_at": "2026-05-02T10:01:00Z",
  "id": "feedback",
  "default_model": "gpt-test",
  "messages": [],
  "negative_knowledge": [
    {"created_at": "2026-05-02T10:00:00Z", "approach": "skip regression tests", "reason": "hid auth regression", "agent": "reviewer"}
  ]
}`)
	feedbackHistory := filepath.Join(workDir, "feedback.md")
	result = runOK(t, runSpec{dir: workDir, sessionDir: feedbackSessionDir},
		"--config", configPath,
		"--session", "feedback",
		"--feedback-apply-config", configPath,
		"--feedback-history", feedbackHistory,
	)
	assertContains(t, result.stdout, "Applied 1 feedback proposal")

	configData, err := os.ReadFile(configPath)
	require.NoError(t, err)
	assertContains(t, string(configData), "Feedback-derived guidance:")

	historyData, err := os.ReadFile(feedbackHistory)
	require.NoError(t, err)
	assertContains(t, string(historyData), "agent: reviewer")
}

func TestSessionCommands(t *testing.T) {
	t.Parallel()
	workDir := t.TempDir()
	sessionDir := filepath.Join(workDir, "sessions")
	writeSession(t, sessionDir)
	writeFile(t, filepath.Join(sessionDir, "perf.json"), `{
  "created_at": "2026-05-02T10:00:00Z",
  "updated_at": "2026-05-02T10:10:00Z",
  "id": "perf",
  "default_agent": "reviewer",
  "messages": [],
  "evaluations": [
    {"created_at": "2026-05-02T10:05:00Z", "agent": "reviewer", "outcome": "pass", "score": 8}
  ],
  "negative_knowledge": [
    {"created_at": "2026-05-02T10:06:00Z", "approach": "skip tests", "reason": "missed bug", "agent": "reviewer"}
  ]
}`)
	spec := runSpec{dir: workDir, sessionDir: sessionDir}

	result := runOK(t, spec, "--list-sessions")
	assertContains(t, result.stdout, "demo")
	assertContains(t, result.stdout, "title=Auth review")
	assertContains(t, result.stdout, "tags=auth,review")

	result = runOK(t, spec, "--list-sessions", "--list-sessions-tag", "AUTH")
	assertContains(t, result.stdout, "demo")
	assertContains(t, result.stdout, "tags=auth,review")

	result = runOK(t, spec, "--list-sessions", "--list-sessions-tag", "missing")
	assertContains(t, result.stdout, "No sessions found.")

	result = runOK(t, spec, "--show-session", "demo")
	assertContains(t, result.stdout, "id: demo")
	assertContains(t, result.stdout, "title: Auth review")
	assertContains(t, result.stdout, "content: hello auth")

	result = runOK(t, spec, "--replay", "demo")
	assertContains(t, result.stdout, "hello auth")
	assertContains(t, result.stdout, "hi there")

	result = runOK(t, spec, "--export-session", "demo")
	assertContains(t, result.stdout, "# Auth review")
	assertContains(t, result.stdout, "### User")

	result = runOK(t, spec, "--export-session", "demo", "--export-format", "json")
	assertContains(t, result.stdout, `"id": "demo"`)

	result = runOK(t, spec, "--search-sessions", "auth")
	assertContains(t, result.stdout, "demo")
	assertContains(t, result.stdout, "user: hello auth")

	result = runOK(t, spec, "--list-session-tags")
	assertContains(t, result.stdout, "auth\t1 sessions")
	assertContains(t, result.stdout, "review\t1 sessions")

	result = runOK(t, spec, "--agent-performance-summary")
	assertContains(t, result.stdout, "agent=reviewer")
	assertContains(t, result.stdout, "evaluations=1")
	assertContains(t, result.stdout, "failures=1")
	assertContains(t, result.stdout, "avg_score=8.00")
	assertContains(t, result.stdout, "outcomes=pass:1")
}

func TestInteractiveFZFModelPickerPersistsFolderDefault(t *testing.T) {
	t.Parallel()

	if runtime.GOOS == windowsGOOS {
		t.Skip("expect-driven TUI e2e is POSIX-only")
	}

	expectPath, err := exec.LookPath("expect")
	if err != nil {
		t.Skip("expect is required for interactive TUI e2e tests")
	}

	workDir := t.TempDir()
	toolDir := filepath.Join(workDir, "tools")
	codexHome := filepath.Join(workDir, "codex-home")
	statePath := filepath.Join(workDir, "state.yaml")
	sessionDir := filepath.Join(workDir, "sessions")

	writeExecutable(t, filepath.Join(toolDir, "fzf"), `#!/bin/sh
cat >/dev/null
printf 'codex/gpt-5.5\tcodex\tgpt-5.5\n'
`)
	writeExecutable(t, filepath.Join(toolDir, "codex"), `#!/bin/sh
exit 0
`)
	writeFile(t, filepath.Join(codexHome, "auth.json"), `{"auth_mode":"chatgpt","tokens":{"access_token":"token","refresh_token":"refresh","account_id":"acct"}}`)

	selectScript := filepath.Join(workDir, "select-model.exp")
	writeFile(t, selectScript, `set timeout 10
spawn env PATH=$env(ATTELER_TEST_PATH) ATTELER_STATE=$env(ATTELER_STATE) ATTELER_SESSION_DIR=$env(ATTELER_SESSION_DIR) CODEX_HOME=$env(CODEX_HOME) $env(ATTELER_BIN)
expect -exact "Send a message"
send "\017"
expect -exact "Keep selected model?"
send "2"
expect -exact "folder default"
send "\004"
expect eof
`)
	runExpect(t, expectPath, selectScript, runSpec{
		dir: workDir,
		env: []string{
			"ATTELER_BIN=" + e2eBinary,
			"ATTELER_TEST_PATH=" + toolDir + string(os.PathListSeparator) + os.Getenv("PATH"),
			"ATTELER_STATE=" + statePath,
			"ATTELER_SESSION_DIR=" + sessionDir,
			"CODEX_HOME=" + codexHome,
		},
	})

	stateData, err := os.ReadFile(statePath)
	if err != nil {
		require.Failf(t, "unexpected failure", "read state: %v", err)
	}

	assertContains(t, string(stateData), "default_model: codex/gpt-5.5")
	assertContains(t, string(stateData), filepath.ToSlash(workDir))

	reopenScript := filepath.Join(workDir, "reopen.exp")
	writeFile(t, reopenScript, `set timeout 10
spawn env PATH=$env(ATTELER_TEST_PATH) ATTELER_STATE=$env(ATTELER_STATE) ATTELER_SESSION_DIR=$env(ATTELER_SESSION_DIR) CODEX_HOME=$env(CODEX_HOME) $env(ATTELER_BIN)
expect -exact "\[model:codex/gpt-5.5\]"
send "\004"
expect eof
`)
	runExpect(t, expectPath, reopenScript, runSpec{
		dir: workDir,
		env: []string{
			"ATTELER_BIN=" + e2eBinary,
			"ATTELER_TEST_PATH=" + toolDir + string(os.PathListSeparator) + os.Getenv("PATH"),
			"ATTELER_STATE=" + statePath,
			"ATTELER_SESSION_DIR=" + sessionDir,
			"CODEX_HOME=" + codexHome,
		},
	})
}

func TestOneShotPrintsActivityEvents(t *testing.T) {
	t.Parallel()
	workDir := t.TempDir()
	codexHome := filepath.Join(workDir, "codex-home")
	configPath := filepath.Join(workDir, "atteler.yaml")
	writeFile(t, filepath.Join(workDir, "README.md"), "hello from readme\n")
	writeFile(t, filepath.Join(codexHome, "auth.json"), `{"auth_mode":"chatgpt","tokens":{"access_token":"token","refresh_token":"refresh","account_id":"acct"}}`)

	codexAPI := startFakeCodexResponses(t, "ok", "gpt-5.5")
	defer codexAPI.Close()

	writeFile(t, configPath, `default_provider: codex
agents:
  reviewer:
    model: codex/gpt-5.5
    system_prompt: Review briefly.
providers:
  anthropic:
    disabled: true
  openai:
    disabled: true
`)

	result := runOK(t, runSpec{
		dir: workDir,
		env: []string{
			"CODEX_HOME=" + codexHome,
			"CODEX_BASE_URL=" + codexAPI.URL,
		},
	}, "--config", configPath, "--agent", "reviewer", "--once", "Summarize @README.md")

	for _, want := range []string{
		"event:session_start",
		"event:file_read",
		"path=README.md",
		"event:context_add",
		"event:file_write",
		"kind=session",
		"event:agent_execute",
		"agent=reviewer",
		"event:tool_execute",
		"provider=codex",
		"event:command_execute",
		`command=codex.responses`,
	} {
		assertContains(t, result.stderr, want)
	}
}

// startFakeCodexResponses spins up an httptest server that mimics the codex
// chatgpt backend's /responses endpoint, returning a minimal SSE stream that
// completes with the given assistant text.
func startFakeCodexResponses(t *testing.T, text, model string) *httptest.Server {
	t.Helper()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasSuffix(r.URL.Path, "/responses") {
			http.NotFound(w, r)
			return
		}

		w.Header().Set("Content-Type", "text/event-stream")

		flusher, ok := w.(http.Flusher)
		if !ok {
			http.Error(w, "streaming unsupported", http.StatusInternalServerError)
			return
		}

		write := func(payload string) {
			fmt.Fprintf(w, "data: %s\n\n", payload)

			flusher.Flush()
		}

		write(fmt.Sprintf(`{"type":"response.output_item.done","item":{"type":"message","role":"assistant","content":[{"type":"output_text","text":%q}]}}`, text))
		write(fmt.Sprintf(`{"type":"response.completed","response":{"model":%q,"usage":{"input_tokens":1,"output_tokens":1,"input_tokens_details":{"cached_tokens":0}}}}`, model))
	}))

	return srv
}

func TestClaudeCodeOneShotCallsAnthropicAPI(t *testing.T) {
	t.Parallel()

	workDir := t.TempDir()
	homeDir := filepath.Join(workDir, "home")
	configPath := filepath.Join(workDir, "atteler.yaml")
	credPath := filepath.Join(homeDir, ".claude", ".credentials.json")
	writeFile(t, credPath, `{"claudeAiOauth":{"accessToken":"test-access","refreshToken":"test-refresh","expiresAt":9999999999999}}`)

	anthropicAPI := startFakeAnthropicMessages(t, "claude code reply", "claude-opus-4-7")
	defer anthropicAPI.Close()

	writeFile(t, configPath, `default_provider: claude-code
default_model: claude-code/claude-opus-4-7
providers:
  anthropic:
    disabled: true
  codex:
    disabled: true
  ollama:
    disabled: true
  openai:
    disabled: true
`)

	result := runOK(t, runSpec{
		dir: workDir,
		env: []string{
			"HOME=" + homeDir,
			"ANTHROPIC_BASE_URL=" + anthropicAPI.URL,
			"ATTELER_CLAUDE_CODE_SKIP_KEYCHAIN=1",
		},
	}, "--config", configPath, "--once", "say hi")

	assertContains(t, result.stdout, "claude code reply")
	assertContains(t, result.stderr, `command=claude_code.messages`)
	assertContains(t, result.stderr, "provider=claude-code")
}

// startFakeAnthropicMessages spins up an httptest server that mimics the
// Anthropic /v1/messages endpoint, returning the given assistant text.
func startFakeAnthropicMessages(t *testing.T, text, model string) *httptest.Server {
	t.Helper()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasSuffix(r.URL.Path, "/messages") {
			http.NotFound(w, r)
			return
		}

		w.Header().Set("Content-Type", "application/json")
		fmt.Fprintf(w, `{"model":%q,"content":[{"type":"text","text":%q}],"usage":{"input_tokens":1,"output_tokens":1}}`, model, text)
	}))

	return srv
}

type runSpec struct {
	dir        string
	sessionDir string
	env        []string
	timeout    time.Duration
}

type runResult struct {
	stdout string
	stderr string
}

func runOK(t *testing.T, spec runSpec, args ...string) runResult {
	t.Helper()

	result, err := runAtteler(t, spec, args...)
	if err != nil {
		require.Failf(t, "unexpected failure", "atteler %v failed: %v\nstdout:\n%s\nstderr:\n%s", args, err, result.stdout, result.stderr)
	}

	return result
}

func runAtteler(t *testing.T, spec runSpec, args ...string) (runResult, error) {
	t.Helper()

	timeout := spec.timeout
	if timeout <= 0 {
		timeout = 30 * time.Second
	}

	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()

	cmd := exec.CommandContext(ctx, e2eBinary, args...)
	cmd.Dir = spec.dir
	cmd.Env = testEnv(t, spec)

	var stdout, stderr bytes.Buffer

	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	err := cmd.Run()

	return runResult{stdout: stdout.String(), stderr: stderr.String()}, err
}

func runExpect(t *testing.T, expectPath, scriptPath string, spec runSpec) {
	t.Helper()

	timeout := spec.timeout
	if timeout <= 0 {
		timeout = 20 * time.Second
	}

	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()

	cmd := exec.CommandContext(ctx, expectPath, "-f", scriptPath)
	cmd.Dir = spec.dir
	cmd.Env = testEnv(t, spec)

	var stdout, stderr bytes.Buffer

	cmd.Stdout = &stdout

	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		require.Failf(t, "unexpected failure", "expect %s failed: %v\nstdout:\n%s\nstderr:\n%s", scriptPath, err, stdout.String(), stderr.String())
	}
}

func testEnv(t *testing.T, spec runSpec) []string {
	t.Helper()

	skip := map[string]bool{
		"ANTHROPIC_API_KEY":   true,
		"ANTHROPIC_BASE_URL":  true,
		"ATTELER_CONFIG":      true,
		"ATTELER_SESSION_DIR": true,
		"CODEX_HOME":          true,
		"FORGE_CONFIG":        true,
		"HOME":                true,
		"OPENAI_API_KEY":      true,
		"OPENAI_BASE_URL":     true,
		"OLLAMA_BASE_URL":     true,
		"XDG_CONFIG_HOME":     true,
	}

	env := make([]string, 0, len(os.Environ())+8+len(spec.env))
	for _, item := range os.Environ() {
		key, _, _ := strings.Cut(item, "=")
		if !skip[key] {
			env = append(env, item)
		}
	}

	home := filepath.Join(t.TempDir(), "home")

	sessionDir := spec.sessionDir
	if sessionDir == "" {
		sessionDir = filepath.Join(home, "sessions")
	}

	env = append(env,
		"ANTHROPIC_API_KEY=",
		"ANTHROPIC_BASE_URL=",
		"ATTELER_CONFIG=",
		"ATTELER_SESSION_DIR="+sessionDir,
		"CODEX_HOME=",
		"FORGE_CONFIG=",
		"HOME="+home,
		"OPENAI_API_KEY=",
		"OPENAI_BASE_URL=",
		"OLLAMA_BASE_URL=",
		"XDG_CONFIG_HOME="+filepath.Join(home, ".config"),
	)
	env = append(env, spec.env...)

	return env
}

func writeExecutable(t *testing.T, path, content string) {
	t.Helper()
	writeFile(t, path, content)
	//nolint:gosec // E2E fixtures must be executable by the spawned TUI process.
	if err := os.Chmod(path, 0o700); err != nil {
		require.NoError(t, err)
	}
}

func repoRoot() (string, error) {
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		return "", errors.New("locate e2e test file")
	}

	return filepath.Clean(filepath.Join(filepath.Dir(file), "..", "..")), nil
}

func writeSession(t *testing.T, dir string) {
	t.Helper()
	writeFile(t, filepath.Join(dir, "demo.json"), `{
  "created_at": "2026-04-30T10:00:00Z",
  "updated_at": "2026-04-30T10:05:00Z",
  "id": "demo",
  "title": "Auth review",
  "default_model": "gpt-test",
  "default_agent": "removed-agent",
  "tags": ["auth", "review"],
  "messages": [
    {"role": "user", "content": "hello auth"},
    {"role": "assistant", "content": "hi there"}
  ]
}`)
}

func writeFile(t *testing.T, path, content string) {
	t.Helper()

	if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
		require.NoError(t, err)
	}

	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		require.NoError(t, err)
	}
}

func assertContains(t *testing.T, haystack, needle string) {
	t.Helper()

	if !strings.Contains(haystack, needle) {
		require.Failf(t, "unexpected failure", "missing %q in:\n%s", needle, haystack)
	}
}

func assertNotContains(t *testing.T, haystack, needle string) {
	t.Helper()

	if strings.Contains(haystack, needle) {
		require.Failf(t, "unexpected failure", "unexpected %q in:\n%s", needle, haystack)
	}
}
