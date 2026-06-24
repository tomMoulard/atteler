package e2e

import (
	"bytes"
	"context"
	"encoding/json"
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
	e2eBinary         string
	e2eSymphonyBinary string
	e2eTmpDir         string
)

const (
	noLegacyDeprecationNotice = "No legacy flag is deprecated in this release."
	windowsGOOS               = "windows"
)

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

	e2eBinary = binaryPath("atteler")
	e2eSymphonyBinary = binaryPath("symphony")

	if err := buildE2EBinary(root, e2eBinary, "./cmd/atteler"); err != nil {
		fmt.Fprintln(os.Stderr, err)

		_ = os.RemoveAll(e2eTmpDir)

		os.Exit(1)
	}

	if err := buildE2EBinary(root, e2eSymphonyBinary, "./cmd/symphony"); err != nil {
		fmt.Fprintln(os.Stderr, err)

		_ = os.RemoveAll(e2eTmpDir)

		os.Exit(1)
	}

	code := m.Run()
	_ = os.RemoveAll(e2eTmpDir)

	os.Exit(code)
}

func binaryPath(name string) string {
	path := filepath.Join(e2eTmpDir, name)
	if runtime.GOOS == windowsGOOS {
		path += ".exe"
	}

	return path
}

func buildE2EBinary(root, outputPath, pkg string) error {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	cmd := exec.CommandContext(ctx, "go", "build", "-o", outputPath, pkg)
	cmd.Dir = root
	cmd.Env = os.Environ()

	output, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("build %s: %w\n%s", pkg, err, output)
	}

	return nil
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
	assertContains(t, result.stdout, "schema: config=")
	assertContains(t, result.stdout, "config_diagnostics:")
	assertContains(t, result.stdout, "state_diagnostics:")
	assertContains(t, result.stdout, "known_providers:")
	assertContains(t, result.stdout, "compatibility_matrix:")
	assertContains(t, result.stdout, "offline_mode=metadata-only")
	assertContains(t, result.stdout, "ollama")
	assertContains(t, result.stdout, "hook_events:")

	badConfig := filepath.Join(workDir, "bad-atteler.yaml")
	writeFile(t, badConfig, `not_a_valid_atteler_key: true
agent_loop:
  nope: 1
`)
	result, err := runAtteler(t, runSpec{dir: workDir}, "--config", badConfig, "--doctor-offline")
	require.Error(t, err)
	assertContains(t, result.stdout, "Atteler offline doctor")
	assertContains(t, result.stdout, "config_status: failed")
	assertContains(t, result.stdout, "config: no config files loaded successfully")
	assertContains(t, result.stdout, "doctor_status: failed")
	assertNotContains(t, result.stdout, "config_error:")
	assertContains(t, result.stderr, "fatal:")
	assertContains(t, result.stderr, badConfig)

	result, err = runAtteler(t, runSpec{dir: workDir}, "--config", badConfig, "config", "doctor-offline", "--output", "json")
	require.Error(t, err)
	assertNotContains(t, result.stdout, "Atteler offline doctor")
	assertContains(t, result.stderr, "error:")

	var report struct {
		Status string `json:"status"`
		Config struct {
			Status    string `json:"status"`
			LoadError string `json:"load_error"`
		} `json:"config"`
		Diagnostics []struct {
			Severity string `json:"severity"`
			Path     string `json:"path"`
		} `json:"diagnostics"`
	}
	require.NoError(t, json.Unmarshal([]byte(result.stdout), &report))
	require.Equal(t, "failed", report.Status)
	require.Equal(t, "failed", report.Config.Status)
	assertContains(t, report.Config.LoadError, badConfig)
	require.NotEmpty(t, report.Diagnostics)
	require.Equal(t, "fatal", report.Diagnostics[0].Severity)
	require.Equal(t, badConfig, report.Diagnostics[0].Path)

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

func TestHeadlessSessionPersistence(t *testing.T) {
	t.Parallel()

	workDir := t.TempDir()
	sessionDir := filepath.Join(workDir, "sessions")
	replayPath := filepath.Join(workDir, "once.json")
	writeFile(t, replayPath, `{"response":{"content":"persisted fixture","model":"replay/model"}}`)

	runOK(t, runSpec{dir: workDir, sessionDir: sessionDir},
		"--headless", "--replay-response", replayPath, "--once", "remember this run",
	)

	sessionID := onlySessionID(t, sessionDir)

	result := runOK(t, runSpec{dir: workDir, sessionDir: sessionDir}, "session", "--session", sessionID, "messages")
	assertContains(t, result.stdout, "remember this run")
	assertContains(t, result.stdout, "persisted fixture")
}

func TestSymphonyBinaryBuildAndValidate(t *testing.T) {
	t.Parallel()

	workDir := t.TempDir()
	workflowPath := filepath.Join(workDir, "WORKFLOW.md")
	writeFile(t, workflowPath, `---
autonomy: medium
tracker:
  kind: github
  repository: tomMoulard/atteler
---
Implement the issue safely.
`)

	result := runSymphonyOK(t, runSpec{dir: workDir, env: []string{"GITHUB_TOKEN=e2e-token"}},
		"--validate", "--workflow", workflowPath, "--autonomy", "high",
	)
	assertContains(t, result.stdout, "Symphony workflow valid:")
	assertContains(t, result.stdout, "tracker=github")
	assertContains(t, result.stdout, "autonomy=high")
}

func TestGroupedCLIHelpAndRouting(t *testing.T) {
	t.Parallel()

	workDir := t.TempDir()
	sessionDir := filepath.Join(workDir, "sessions")
	writeSession(t, sessionDir)

	configPath := filepath.Join(workDir, "atteler.yaml")
	pluginDir := filepath.Join(workDir, "plugin")

	writeFile(t, configPath, `agents:
  reviewer:
    model: gpt-test
    triggers: ["review this"]
    system_prompt: Review code carefully.
plugins:
  paths: ["./plugin"]
  policy:
    permissions:
      filesystem:
        read:
          - "."
        write: []
      network:
        allow: false
        hosts: []
      shell:
        allow: true
      env: []
      secrets: []
      tools: []
    output:
      stdout_max_bytes: 4096
      stderr_max_bytes: 4096
    trusted_install_sources:
      - e2e
`)
	writeFile(t, filepath.Join(pluginDir, "plugin.yaml"), `name: runner
version: "1.0.0"
description: Runner plugin
entrypoints:
  check: bin/check
`)
	writeExecutable(t, filepath.Join(pluginDir, "bin", "check"), `#!/bin/sh
printf 'plugin-check\n'
	`)
	actualPath := filepath.Join(workDir, "actual.txt")
	writeFile(t, actualPath, "hello deterministic eval\n")

	mcpManifest := filepath.Join(workDir, "mcp.yaml")
	writeFile(t, mcpManifest, `servers:
  - name: helper
    command: helper-bin
    capabilities: ["tools"]
`)

	replayPath := filepath.Join(workDir, "once.json")
	writeFile(t, replayPath, `{"response":{"content":"fixture replayed","model":"replay/model"}}`)

	writeFile(t, filepath.Join(workDir, "main.go"), "package main\nfunc main() {}\n")

	result := runOK(t, runSpec{dir: workDir}, "--help")
	assertContains(t, result.stdout, "atteler <domain> <command> [args]")
	assertContains(t, result.stdout, "chat/session")
	assertContains(t, result.stdout, "code-intel")
	assertContains(t, result.stdout, "atteler help legacy")
	assertContains(t, result.stdout, noLegacyDeprecationNotice)
	require.Zero(t, countIndentedFlagLines(result.stdout), "top-level help should not print the legacy flag catalog")

	result = runOK(t, runSpec{dir: workDir}, "--model", "test/model", "-help")
	assertContains(t, result.stdout, "atteler help [domain]")
	require.Zero(t, countIndentedFlagLines(result.stdout), "flag-prefixed help should stay focused")

	result = runOK(t, runSpec{dir: workDir}, "help", "code-intel")
	assertContains(t, result.stdout, "Code intelligence")
	assertContains(t, result.stdout, "atteler code-intel summary")
	assertContains(t, result.stdout, "--code-summary")
	assertContains(t, result.stdout, noLegacyDeprecationNotice)
	assertNotContains(t, result.stdout, "--review-scan")

	result = runOK(t, runSpec{dir: workDir}, "help", "session")
	assertContains(t, result.stdout, "Chat & sessions")
	assertContains(t, result.stdout, "Usage: atteler session <command> [args]")
	assertContains(t, result.stdout, noLegacyDeprecationNotice)

	result = runOK(t, runSpec{dir: workDir}, "session", "help")
	assertContains(t, result.stdout, "Chat & sessions")
	assertContains(t, result.stdout, "Usage: atteler session <command> [args]")
	assertContains(t, result.stdout, noLegacyDeprecationNotice)

	result = runOK(t, runSpec{dir: workDir}, "session", "--session", "--help")
	assertContains(t, result.stdout, "Chat & sessions")
	assertContains(t, result.stdout, "Usage: atteler session <command> [args]")

	for _, domain := range []string{
		"config",
		"providers",
		"agents",
		"memory",
		"code-intel",
		"incident",
		"review",
		"watch",
		"plugins",
		"worktrees",
		"eval",
	} {
		result = runOK(t, runSpec{dir: workDir}, "help", domain)
		assertContains(t, result.stdout, "Usage: atteler "+domain+" <command> [args]")
		assertContains(t, result.stdout, "Examples:")
		assertContains(t, result.stdout, noLegacyDeprecationNotice)
		assertNotContains(t, result.stdout, "Compatibility flag catalog:")
	}

	result = runOK(t, runSpec{dir: workDir}, "help", "--code-summary")
	assertContains(t, result.stdout, "Code intelligence")
	assertContains(t, result.stdout, "--code-summary")
	assertContains(t, result.stdout, noLegacyDeprecationNotice)
	assertNotContains(t, result.stdout, "--review-scan")

	result = runOK(t, runSpec{dir: workDir}, "help", "legacy")
	assertContains(t, result.stdout, "Compatibility flag catalog:")
	assertContains(t, result.stdout, noLegacyDeprecationNotice)
	assertContains(t, result.stdout, "--code-summary")

	result = runOK(t, runSpec{dir: workDir}, "help", "help")
	assertContains(t, result.stdout, "atteler help [domain]")
	assertContains(t, result.stdout, "Domains:")

	failedHelp, helpErr := runAtteler(t, runSpec{dir: workDir}, "help", "wat")
	require.Error(t, helpErr)
	assertContains(t, failedHelp.stderr, `unknown help domain "wat"`)
	assertNotContains(t, failedHelp.stderr, "event:session_start")

	result = runOK(t, runSpec{dir: workDir}, "providers", "list")
	assertContains(t, result.stdout, "anthropic")
	assertContains(t, result.stdout, "openai")

	result = runOK(t, runSpec{dir: workDir}, "config", "paths")
	assertContains(t, result.stdout, "atteler/config.yaml")

	result = runOK(t, runSpec{dir: workDir}, "watch", "json", "--model", "test/model")
	assertContains(t, result.stdout, `"findings":`)

	result = runOK(t, runSpec{dir: workDir, sessionDir: sessionDir}, "session", "list")
	assertContains(t, result.stdout, "demo")

	result = runOK(t, runSpec{dir: workDir, sessionDir: sessionDir}, "session", "--session", "demo", "messages")
	assertContains(t, result.stdout, "preview=hello auth")

	result = runOK(t, runSpec{dir: workDir, sessionDir: sessionDir}, "memory", "search", "hello", "auth")
	assertContains(t, result.stdout, "Searched corpus:")
	assertContains(t, result.stdout, "scope=repo")
	assertContains(t, result.stdout, "session/demo/message/0")

	rebuiltMemoryStore := filepath.Join(workDir, "rebuilt-memory.json")
	result = runOK(t, runSpec{dir: workDir, sessionDir: sessionDir}, "memory", "rebuild", "--memory-store", rebuiltMemoryStore, "--memory-scope", "repo")
	assertContains(t, result.stdout, "Rebuilt memory store")

	memoryJSON, err := os.ReadFile(rebuiltMemoryStore)
	require.NoError(t, err)
	assertContains(t, string(memoryJSON), `"schema_version": 1`)
	assertContains(t, string(memoryJSON), `"corpus":`)
	assertContains(t, string(memoryJSON), `"created_at":`)
	assertContains(t, string(memoryJSON), `"updated_at":`)

	result = runOK(t, runSpec{dir: workDir}, "memory", "search", "hello", "auth", "--memory-store", rebuiltMemoryStore)
	assertContains(t, result.stdout, "Searched corpus:")
	assertContains(t, result.stdout, "store="+filepath.Clean(rebuiltMemoryStore))
	assertContains(t, result.stdout, "session/demo/message/0")

	result = runOK(t, runSpec{dir: workDir}, "memory", "list-corpus", "--memory-store", rebuiltMemoryStore)
	assertContains(t, result.stdout, "Memory corpus:")
	assertContains(t, result.stdout, "sessions=demo")

	result = runOK(t, runSpec{dir: workDir}, "memory", "purge", "session:demo", "--memory-store", rebuiltMemoryStore)
	assertContains(t, result.stdout, "Purged")

	result = runOK(t, runSpec{dir: workDir}, "memory", "search", "hello", "auth", "--memory-store", rebuiltMemoryStore, "--memory-scope", "store")
	assertContains(t, result.stdout, "Searched corpus:")
	assertContains(t, result.stdout, "No memory results found.")

	result = runOK(t, runSpec{dir: workDir}, "--config", configPath, "agents", "list")
	assertContains(t, result.stdout, "reviewer")

	result = runOK(t, runSpec{dir: workDir, timeout: time.Minute}, "code-intel", "summary")
	assertContains(t, result.stdout, "files=")

	incidentPath := filepath.Join(workDir, "incident.json")
	writeFile(t, incidentPath, `{
  "source": "file",
  "reference": "e2e-incident",
  "service": "api-gateway",
  "message": "nil OAuth state for alice@example.com access_token=secret-token",
  "stack_trace": [{"file": "main.go", "line": 1, "function": "main"}]
}`)
	result = runOK(t, runSpec{dir: workDir}, "incident", "diagnose", "--incident-file", incidentPath, "--json")
	assertContains(t, result.stdout, `"reference": "e2e-incident"`)
	assertContains(t, result.stdout, `"redaction_policy":`)
	assertContains(t, result.stdout, "main.go:1")
	assertContains(t, result.stdout, "[REDACTED")
	assertNotContains(t, result.stdout, "alice@example.com")
	assertNotContains(t, result.stdout, "secret-token")

	result = runOK(t, runSpec{dir: workDir}, "review", "scan")
	assertContains(t, result.stdout, "reviewer: watch-scan")

	result = runOK(t, runSpec{dir: workDir}, "--config", configPath, "plugins", "describe", "runner")
	assertContains(t, result.stdout, "name: runner")

	result = runOK(t, runSpec{dir: workDir}, "plugins", "manifest", mcpManifest, "--mcp-capability", "tools")
	assertContains(t, result.stdout, "helper")
	assertContains(t, result.stdout, "command=helper-bin")

	root, err := repoRoot()
	require.NoError(t, err)
	result = runOK(t, runSpec{dir: root}, "worktrees", "list")
	assertContains(t, result.stdout, "active atteler worktrees")

	result = runOK(t, runSpec{dir: workDir}, "eval", "output", actualPath, "--eval-expected", "deterministic")
	assertContains(t, result.stdout, "PASS\tmode=contains")

	evalSuitePath := filepath.Join(workDir, "actual.eval.yaml")
	evalReportPath := filepath.Join(workDir, "eval-report.json")

	writeFile(t, evalSuitePath, `version: 1
metadata:
  target_command: atteler e2e
  owner: e2e
actual: actual.txt
assertions:
  - id: contains-deterministic
    type: contains
    value: deterministic
  - id: forbidden-secret
    type: not_contains
    value: api_key=
`)
	result = runOK(t, runSpec{dir: workDir}, "eval", "run", evalSuitePath, "--eval-json", "--eval-report", evalReportPath)
	assertContains(t, result.stdout, `"passed": true`)
	assertContains(t, result.stdout, `"id": "contains-deterministic"`)
	assertContains(t, result.stdout, `"owner": "e2e"`)

	reportData, err := os.ReadFile(evalReportPath)
	require.NoError(t, err)
	assertContains(t, string(reportData), `"passed": true`)

	result = runOK(t, runSpec{dir: workDir}, "eval", "replay-response", replayPath, "summarize", "fixture")
	assertContains(t, result.stdout, "fixture replayed")

	result = runOK(t, runSpec{dir: workDir, stdin: "summarize fixture from stdin\n"}, "eval", "replay-response", replayPath, "--stdin")
	assertContains(t, result.stdout, "fixture replayed")

	result = runOK(t, runSpec{dir: workDir, stdin: "stdin-only prompt\n"}, "chat", "once", "--stdin", "--replay-response", replayPath)
	assertContains(t, result.stdout, "fixture replayed")

	result = runOK(t, runSpec{dir: workDir, stdin: "stdin before command\n"}, "chat", "--stdin", "once", "--replay-response", replayPath)
	assertContains(t, result.stdout, "fixture replayed")
}

func TestWorktreeMergeReviewGateE2E(t *testing.T) {
	t.Parallel()

	if runtime.GOOS == windowsGOOS {
		t.Skip("worktree verification commands use POSIX shell syntax")
	}

	t.Run("preserve by default", func(t *testing.T) {
		t.Parallel()
		repo := initE2EGitRepo(t)
		sessionDir := filepath.Join(t.TempDir(), "sessions")
		replayPath := writeReplayResponse(t, t.TempDir())

		result := runOK(t, runSpec{dir: repo, sessionDir: sessionDir}, "--autonomy", "high", "--worktree", "--replay-response", replayPath, "--once", "noop")
		assertContains(t, result.stderr, "worktree: session files are in ")
		assertContains(t, result.stderr, "worktree: merge with: atteler --merge-worktree ")
		assertNotContains(t, result.stderr, "worktree: merged and cleaned up")

		sess := readOnlyE2ESession(t, sessionDir)
		require.DirExists(t, sess.WorktreePath)
		assertContains(t, gitOutputE2E(t, repo, "branch", "--list", sess.WorktreeBranch), sess.WorktreeBranch)
	})

	t.Run("ungated auto merge rejected before worktree creation", func(t *testing.T) {
		t.Parallel()
		repo := initE2EGitRepo(t)
		sessionDir := filepath.Join(t.TempDir(), "sessions")
		replayPath := writeReplayResponse(t, t.TempDir())

		result, err := runAtteler(t, runSpec{dir: repo, sessionDir: sessionDir},
			"--worktree",
			"--autonomy", "high",
			"--worktree-auto-merge",
			"--replay-response", replayPath,
			"--once", "noop",
		)
		require.Error(t, err)
		assertContains(t, result.stderr, "worktree auto-merge requires")
		assertContains(t, result.stderr, "--worktree-verify-command")
		assertNotContains(t, result.stderr, "worktree: created")

		matches, globErr := filepath.Glob(filepath.Join(sessionDir, "*.json"))
		require.NoError(t, globErr)
		require.Empty(t, matches)
	})

	t.Run("verified auto merge", func(t *testing.T) {
		t.Parallel()
		repo := initE2EGitRepo(t)
		sessionDir := filepath.Join(t.TempDir(), "sessions")
		replayPath := writeReplayResponse(t, t.TempDir())
		runOK(t, runSpec{dir: repo, sessionDir: sessionDir}, "--autonomy", "high", "--worktree", "--replay-response", replayPath, "--once", "setup")
		sess := readOnlyE2ESession(t, sessionDir)

		writeFile(t, filepath.Join(sess.WorktreePath, "verified.txt"), "verified\n")

		result := runOK(t, runSpec{dir: repo, sessionDir: sessionDir},
			"--session", sess.ID,
			"--autonomy", "high",
			"--worktree",
			"--worktree-auto-merge",
			"--worktree-verify-command", "test -f verified.txt",
			"--replay-response", replayPath,
			"--once", "merge",
		)

		assertContains(t, result.stderr, "worktree: diff summary:")
		assertContains(t, result.stderr, "verified.txt")
		assertContains(t, result.stderr, "worktree: tests run:")
		assertContains(t, result.stderr, "PASS test -f verified.txt")
		assertContains(t, result.stderr, "worktree: commit SHA:")
		assertContains(t, result.stderr, "worktree: rollback instructions:")
		require.FileExists(t, filepath.Join(repo, "verified.txt"))
		require.NoDirExists(t, sess.WorktreePath)
	})

	t.Run("failed verification preserves worktree", func(t *testing.T) {
		t.Parallel()
		repo := initE2EGitRepo(t)
		sessionDir := filepath.Join(t.TempDir(), "sessions")
		replayPath := writeReplayResponse(t, t.TempDir())
		runOK(t, runSpec{dir: repo, sessionDir: sessionDir}, "--autonomy", "high", "--worktree", "--replay-response", replayPath, "--once", "setup")
		sess := readOnlyE2ESession(t, sessionDir)
		writeFile(t, filepath.Join(sess.WorktreePath, "blocked.txt"), "blocked\n")

		result, err := runAtteler(t, runSpec{dir: repo, sessionDir: sessionDir},
			"--session", sess.ID,
			"--autonomy", "high",
			"--worktree",
			"--worktree-auto-merge",
			"--worktree-verify-command", "test -f missing.txt",
			"--replay-response", replayPath,
			"--once", "merge",
		)
		require.Error(t, err)

		assertContains(t, result.stderr, "worktree: auto-merge failed:")
		assertContains(t, result.stderr, "verification command")
		require.NoFileExists(t, filepath.Join(repo, "blocked.txt"))
		require.DirExists(t, sess.WorktreePath)
	})

	t.Run("conflict preservation", func(t *testing.T) {
		t.Parallel()
		repo := initE2EGitRepo(t)
		writeFile(t, filepath.Join(repo, "conflict.txt"), "base\n")
		gitOutputE2E(t, repo, "add", "conflict.txt")
		gitOutputE2E(t, repo, "commit", "-m", "base conflict")

		sessionDir := filepath.Join(t.TempDir(), "sessions")
		replayPath := writeReplayResponse(t, t.TempDir())
		runOK(t, runSpec{dir: repo, sessionDir: sessionDir}, "--autonomy", "high", "--worktree", "--replay-response", replayPath, "--once", "setup")
		sess := readOnlyE2ESession(t, sessionDir)

		writeFile(t, filepath.Join(sess.WorktreePath, "conflict.txt"), "branch\n")
		gitOutputE2E(t, sess.WorktreePath, "add", "conflict.txt")
		gitOutputE2E(t, sess.WorktreePath, "commit", "-m", "branch conflict")
		writeFile(t, filepath.Join(repo, "conflict.txt"), "main\n")
		gitOutputE2E(t, repo, "add", "conflict.txt")
		gitOutputE2E(t, repo, "commit", "-m", "main conflict")

		result, err := runAtteler(t, runSpec{dir: repo, sessionDir: sessionDir}, "--autonomy", "high", "worktrees", "merge", sess.ID)
		require.Error(t, err)
		assertContains(t, result.stderr, "merge dry-run reported conflicts")
		assertContains(t, result.stderr, "recovery: manual merge after review:")
		require.DirExists(t, sess.WorktreePath)

		data, readErr := os.ReadFile(filepath.Join(repo, "conflict.txt"))
		require.NoError(t, readErr)
		require.Equal(t, "main\n", string(data))
	})

	t.Run("manual merge reports review output", func(t *testing.T) {
		t.Parallel()
		repo := initE2EGitRepo(t)
		sessionDir := filepath.Join(t.TempDir(), "sessions")
		replayPath := writeReplayResponse(t, t.TempDir())
		runOK(t, runSpec{dir: repo, sessionDir: sessionDir}, "--autonomy", "high", "--worktree", "--replay-response", replayPath, "--once", "setup")
		sess := readOnlyE2ESession(t, sessionDir)

		writeFile(t, filepath.Join(sess.WorktreePath, "manual.txt"), "manual\n")
		gitOutputE2E(t, sess.WorktreePath, "add", "manual.txt")
		gitOutputE2E(t, sess.WorktreePath, "commit", "-m", "manual change")

		result := runOK(t, runSpec{dir: repo, sessionDir: sessionDir}, "--autonomy", "high", "worktrees", "merge", sess.ID)
		assertContains(t, result.stderr, "worktree: diff summary:")
		assertContains(t, result.stderr, "manual.txt")
		assertContains(t, result.stderr, "worktree: tests run:")
		assertContains(t, result.stderr, "verification override")
		assertContains(t, result.stderr, "worktree: commit SHA:")
		assertContains(t, result.stderr, "worktree: rollback instructions:")
		require.FileExists(t, filepath.Join(repo, "manual.txt"))
		require.NoDirExists(t, sess.WorktreePath)
	})
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
  policy:
    permissions:
      filesystem:
        read:
          - "."
        write: []
      network:
        allow: false
        hosts: []
      shell:
        allow: true
      env: []
      secrets: []
      tools: []
    output:
      stdout_max_bytes: 4096
      stderr_max_bytes: 4096
    trusted_install_sources:
      - e2e
`)
	writeFile(t, filepath.Join(pluginDir, "plugin.yaml"), `name: runner
version: "1.0.0"
min_atteler_version: "0.1.0"
description: Runner plugin
entrypoints:
  run: bin/run
entrypoint_args:
  run: []
entrypoint_contracts:
  run:
    output:
      format: text
permissions:
  filesystem:
    read:
      - "."
    write: []
  network:
    allow: false
    hosts: []
  shell:
    allow: true
  env: []
  secrets: []
  tools: []
output:
  stdout_max_bytes: 4096
  stderr_max_bytes: 4096
trust:
  enabled: true
  install_source: e2e
  checksum: sha256:e2e
  revoked: false
  audit:
    - action: accepted
      actor: e2e
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
	assertContains(t, result.stdout, "permissions:")
	assertContains(t, result.stdout, "output:")
	assertContains(t, result.stdout, "trust:")
	assertContains(t, result.stdout, "install_source: e2e")

	result = runOK(t, runSpec{dir: workDir}, "--config", configPath, "--run-plugin", "runner/run", "--plugin-dry-run")
	assertContains(t, result.stdout, `would run plugin "runner" entrypoint "run"`)

	result = runOK(t, runSpec{dir: workDir}, "--config", configPath, "--run-plugin", "runner", "--plugin-entrypoint", "run")
	assertContains(t, result.stdout, "plugin-output")

	if runtime.GOOS != windowsGOOS {
		result = runOK(t, runSpec{dir: workDir}, "--bash", "printf cli-bash")
		assertContains(t, result.stdout, "cli-bash")

		mcpHelper := filepath.Join(workDir, "mcp-helper")
		writeExecutable(t, mcpHelper, `#!/bin/sh
read init
printf '{"jsonrpc":"2.0","id":1,"result":{"protocolVersion":"2025-11-25","capabilities":{"tools":{}},"serverInfo":{"name":"e2e-helper","version":"1.0.0"}}}\n'
read initialized
read list
printf '{"jsonrpc":"2.0","id":2,"result":{"tools":[{"name":"echo","inputSchema":{"type":"object","properties":{"message":{"type":"string"}}}}]}}\n'
read call
printf '{"jsonrpc":"2.0","id":3,"result":{"ok":true,"source":"mcp-helper"}}\n'
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
	assertContains(t, result.stdout, "Searched corpus:")
	assertContains(t, result.stdout, "session/demo/message/0")

	notesPath := filepath.Join(workDir, "notes.txt")
	memoryStore := filepath.Join(workDir, "memory.json")

	writeFile(t, notesPath, "OAuth callback notes\n")
	result = runOK(t, runSpec{dir: workDir}, "--memory-store", memoryStore, "--memory-index", notesPath)
	assertContains(t, result.stdout, "Indexed")
	result = runOK(t, runSpec{dir: workDir}, "memory", "list-corpus", "--memory-store", memoryStore)
	assertContains(t, result.stdout, "Memory corpus:")
	assertContains(t, result.stdout, "schema=1")
	result = runOK(t, runSpec{dir: workDir}, "--memory-store", memoryStore, "--memory-search", "callback")
	assertContains(t, result.stdout, "Searched corpus:")
	assertContains(t, result.stdout, "store="+filepath.Clean(memoryStore))
	assertContains(t, result.stdout, notesPath)
	result = runOK(t, runSpec{dir: workDir}, "memory", "purge", "repo:"+workDir, "--memory-store", memoryStore)
	assertContains(t, result.stdout, "Purged 1 memory document")
	result = runOK(t, runSpec{dir: workDir}, "--memory-store", memoryStore, "--memory-search", "callback")
	assertContains(t, result.stdout, "No memory results found.")

	result = runOK(t, runSpec{dir: workDir}, "--skill-step", "plan", "--skill-step", "code", "--skill-step", "test", "--skill-step", "plan", "--skill-step", "code", "--skill-step", "test")
	assertContains(t, result.stdout, "slug: plan-code-test")
	assertContains(t, result.stdout, "occurrences: 2")

	reviewOnlySkillDir := filepath.Join(workDir, "review-only-skills")
	result = runOK(t, runSpec{dir: workDir}, "--skill-review-only", "--skill-save-dir", reviewOnlySkillDir, "--skill-step", "plan", "--skill-step", "code", "--skill-step", "plan", "--skill-step", "code")
	assertContains(t, result.stdout, "diff --git a/plan-code/SKILL.md b/plan-code/SKILL.md")
	assertContains(t, result.stdout, "trigger-evals: pass 4 cases")
	assertContains(t, result.stdout, "review-only: no files written")

	_, err := os.Stat(filepath.Join(reviewOnlySkillDir, "plan-code", "SKILL.md"))
	require.ErrorIs(t, err, os.ErrNotExist)

	skillDir := filepath.Join(workDir, "skills")
	result = runOK(t, runSpec{dir: workDir}, "--skill-save-dir", skillDir, "--skill-step", "plan", "--skill-step", "code", "--skill-step", "plan", "--skill-step", "code")
	assertContains(t, result.stdout, "diff --git a/plan-code/SKILL.md b/plan-code/SKILL.md")
	assertContains(t, result.stdout, "trigger-evals: pass 4 cases")
	assertContains(t, result.stdout, "saved: "+filepath.Join(skillDir, "plan-code", "SKILL.md"))
	skillData, err := os.ReadFile(filepath.Join(skillDir, "plan-code", "SKILL.md"))
	require.NoError(t, err)
	assertContains(t, string(skillData), "# Plan Code Skill")
	assertContains(t, string(skillData), "## Workflow")

	evalData, err := os.ReadFile(filepath.Join(skillDir, "plan-code", "evals", "triggers.yaml"))
	require.NoError(t, err)
	assertContains(t, string(evalData), "should_trigger: true")
	assertContains(t, string(evalData), "should_trigger: false")

	_, err = os.Stat(filepath.Join(skillDir, "plan-code.md"))
	require.ErrorIs(t, err, os.ErrNotExist)

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
	assertContains(t, result.stdout, "openai/too-expensive\tstatus=rejected")
	assertContains(t, result.stdout, "over budget")

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

	openAIAPI := startFakeOpenAIChatCompletions(t, []string{
		"proposal from gpt-4.1",
		"WINNER: gpt-4.1\nREASON: no structured gate output",
	})
	defer openAIAPI.Close()

	speculateConfigPath := filepath.Join(workDir, "speculate-openai.yaml")
	writeSpeculateOpenAIConfig(t, speculateConfigPath)

	result, err = runAtteler(t, runSpec{
		dir: workDir,
		env: []string{
			"OPENAI_API_KEY=e2e-test-key",
			"OPENAI_BASE_URL=" + openAIAPI.URL,
		},
	}, "--config", speculateConfigPath,
		"--speculate-run",
		"--speculate-agent", "gpt-4.1",
		"--speculate-gate", "tests pass",
		"--speculate-gate", "lint pass",
		"--speculate-prompt", "ship safely")
	require.Error(t, err)
	assertContains(t, result.stdout, "winner: gpt-4.1")
	assertContains(t, result.stdout, `error: missing gate checks: "tests pass", "lint pass"`)
	assertContains(t, result.stderr, `error: speculate-run: missing gate checks: "tests pass", "lint pass"`)

	result = runOK(t, runSpec{dir: workDir}, "--review-plan", "--review-agent", "alpha", "--review-agent", "beta", "--review-path", "pkg/auth.go", "--review-gate", "tests pass")
	assertContains(t, result.stdout, "reviewers:")
	assertContains(t, result.stdout, "paths:\n  - pkg/auth.go")
	assertContains(t, result.stdout, "alpha -> beta")
	assertContains(t, result.stdout, "gates:\n  - tests pass")

	result = runOK(t, runSpec{dir: workDir}, "--spawn-agent", "reviewer|dry run this child prompt", "--spawn-dry-run")
	assertContains(t, result.stdout, "Would spawn 1 sub-agent")
	assertContains(t, result.stdout, "agent=reviewer")
	assertContains(t, result.stdout, "prompt=dry run this child prompt")

	if runtime.GOOS != windowsGOOS {
		fakeAtteler := filepath.Join(workDir, "fake-atteler")
		childRunLog := filepath.Join(workDir, "child-runs.log")

		writeExecutable(t, fakeAtteler, `#!/bin/sh
if [ -n "$RUN_LOG" ]; then
  printf '%s\n' "$ATTELER_CHILD_ID" >> "$RUN_LOG"
fi
printf 'child:%s agent:%s scope:%s\n' "$ATTELER_CHILD_ID" "$ATTELER_CHILD_AGENT" "$ATTELER_ALLOWED_WRITE_SCOPE"
printf 'workspace:%s\n' "$ATTELER_CHILD_WORKSPACE_ID" >&2
`)

		spawnLedger := filepath.Join(workDir, "spawn-ledger.json")
		childRunSpec := runSpec{dir: workDir, env: []string{"RUN_LOG=" + childRunLog}}
		result = runOK(t, childRunSpec,
			"--spawn-agent", "child|reviewer|real child prompt",
			"--spawn-binary", fakeAtteler,
			"--spawn-ledger", spawnLedger,
		)
		assertContains(t, result.stdout, "id=child\tagent=reviewer\tstatus=ok")
		assertContains(t, result.stdout, "ledger="+spawnLedger)
		assertContains(t, result.stdout, "admission_id=")
		assertContains(t, result.stdout, "transcript=")
		assertContains(t, result.stdout, "output=child:child agent:reviewer scope:")

		spawnLedgerData, readErr := os.ReadFile(spawnLedger)
		require.NoError(t, readErr)
		assertContains(t, string(spawnLedgerData), `"admissions"`)
		assertContains(t, string(spawnLedgerData), `"admitted": true`)
		assertContains(t, string(spawnLedgerData), `"child_id": "child"`)
		assertContains(t, string(spawnLedgerData), `"attempts"`)

		transcriptPath := valueFromOutputLine(result.stdout, "transcript=")
		require.NotEmpty(t, transcriptPath)
		transcriptData, readErr := os.ReadFile(transcriptPath)
		require.NoError(t, readErr)
		assertContains(t, string(transcriptData), "# stdout")
		assertContains(t, string(transcriptData), "child:child agent:reviewer")
		assertContains(t, string(transcriptData), "# stderr")
		assertContains(t, string(transcriptData), "workspace:")

		childRunLogData, readErr := os.ReadFile(childRunLog)
		require.NoError(t, readErr)
		require.Equal(t, "child\n", string(childRunLogData))

		result = runOK(t, childRunSpec,
			"--spawn-agent", "child|reviewer|real child prompt",
			"--spawn-binary", fakeAtteler,
			"--spawn-ledger", spawnLedger,
			"--spawn-resume",
		)
		assertContains(t, result.stdout, "id=child\tagent=reviewer\tstatus=skipped")
		assertContains(t, result.stdout, "ledger="+spawnLedger)
		assertContains(t, result.stdout, "admission_id=")

		childRunLogData, readErr = os.ReadFile(childRunLog)
		require.NoError(t, readErr)
		require.Equal(t, "child\n", string(childRunLogData))

		denyLedger := filepath.Join(workDir, "spawn-denied-ledger.json")
		denyRunLog := filepath.Join(workDir, "deny-runs.log")
		deniedResult, deniedErr := runAtteler(t, runSpec{dir: workDir, env: []string{"RUN_LOG=" + denyRunLog}},
			"--spawn-agent", "blocked|reviewer|this prompt should exceed a one token budget",
			"--spawn-binary", fakeAtteler,
			"--spawn-ledger", denyLedger,
			"--spawn-token-budget", "1",
		)
		require.Error(t, deniedErr)
		assertContains(t, deniedResult.stdout, "id=blocked\tagent=reviewer\tstatus=budget_exhausted")
		assertContains(t, deniedResult.stdout, "ledger="+denyLedger)
		assertContains(t, deniedResult.stdout, "admission_id=")
		assertNotContains(t, deniedResult.stdout, "transcript=")

		denyLedgerData, readErr := os.ReadFile(denyLedger)
		require.NoError(t, readErr)
		assertContains(t, string(denyLedgerData), `"admissions"`)
		assertContains(t, string(denyLedgerData), `"admitted": false`)
		assertContains(t, string(denyLedgerData), `"deny_reason": "prompt token budget exceeded`)
		assertContains(t, string(denyLedgerData), `"child_id": "blocked"`)
		assertNotContains(t, string(denyLedgerData), `"attempts"`)

		_, readErr = os.ReadFile(denyRunLog)
		require.ErrorIs(t, readErr, os.ErrNotExist)

		haltFakeAtteler := filepath.Join(workDir, "halt-fake-atteler")
		haltRunLog := filepath.Join(workDir, "halt-runs.log")

		writeExecutable(t, haltFakeAtteler, `#!/bin/sh
if [ -n "$RUN_LOG" ]; then
  printf '%s\n' "$ATTELER_CHILD_ID" >> "$RUN_LOG"
fi
printf 'child:%s agent:%s\n' "$ATTELER_CHILD_ID" "$ATTELER_CHILD_AGENT"
if [ "$ATTELER_CHILD_ID" = "fail" ]; then
  waited=0
  while [ ! -f "$ATTELER_ALLOWED_WRITE_SCOPE/slow-started" ] && [ "$waited" -lt 100 ]; do
    waited=$((waited + 1))
    sleep 0.05
  done
  printf 'failing child\n' >&2
  exit 7
fi
if [ "$ATTELER_CHILD_ID" = "slow" ]; then
  touch "$ATTELER_ALLOWED_WRITE_SCOPE/slow-started"
  sleep 5
  printf 'slow child completed\n'
fi
`)

		haltLedger := filepath.Join(workDir, "spawn-halted-ledger.json")
		haltedResult, haltedErr := runAtteler(t, runSpec{dir: workDir, env: []string{"RUN_LOG=" + haltRunLog}, timeout: 15 * time.Second},
			"--spawn-agent", "fail|reviewer|fail after sibling is admitted",
			"--spawn-agent", "slow|reviewer|wait for cancellation",
			"--spawn-binary", haltFakeAtteler,
			"--spawn-ledger", haltLedger,
			"--spawn-max-concurrency", "2",
			"--spawn-cancel-on-failure",
		)
		require.Error(t, haltedErr)
		assertContains(t, haltedResult.stdout, "id=fail\tagent=reviewer\tstatus=error")
		assertContains(t, haltedResult.stdout, "id=slow\tagent=reviewer\tstatus=canceled")
		assertContains(t, haltedResult.stdout, "ledger="+haltLedger)
		assertContains(t, haltedResult.stdout, "admission_id=")
		assertContains(t, haltedResult.stdout, "stop_id=")
		assertContains(t, haltedResult.stdout, "transcript=")

		haltLedgerData, readErr := os.ReadFile(haltLedger)
		require.NoError(t, readErr)
		assertContains(t, string(haltLedgerData), `"admissions"`)
		assertContains(t, string(haltLedgerData), `"admitted": true`)
		assertContains(t, string(haltLedgerData), `"child_id": "fail"`)
		assertContains(t, string(haltLedgerData), `"child_id": "slow"`)
		assertContains(t, string(haltLedgerData), `"stop_receipts"`)
		assertContains(t, string(haltLedgerData), `"status": "canceled"`)

		haltRunLogData, readErr := os.ReadFile(haltRunLog)
		require.NoError(t, readErr)
		assertContains(t, string(haltRunLogData), "fail\n")
		assertContains(t, string(haltRunLogData), "slow\n")

		asyncLedger := filepath.Join(workDir, "async-ledger.json")
		result = runOK(t, childRunSpec,
			"--async-run",
			"--async-task", "plan|planner|draft plan",
			"--async-task", "code|coder|implement|plan",
			"--spawn-binary", fakeAtteler,
			"--spawn-ledger", asyncLedger,
		)
		assertContains(t, result.stdout, "wave=1\torder=1\tid=plan\tagent=planner\tstatus=ok")
		assertContains(t, result.stdout, "wave=2\torder=1\tid=code\tagent=coder\tstatus=ok")
		assertContains(t, result.stdout, "ledger="+asyncLedger)
		assertContains(t, result.stdout, "admission_id=")
		assertContains(t, result.stdout, "transcript=")
		assertContains(t, result.stdout, "output=child:plan agent:planner scope:")

		asyncLedgerData, readErr := os.ReadFile(asyncLedger)
		require.NoError(t, readErr)
		assertContains(t, string(asyncLedgerData), `"admissions"`)
		assertContains(t, string(asyncLedgerData), `"child_id": "plan"`)
		assertContains(t, string(asyncLedgerData), `"child_id": "code"`)
		assertContains(t, string(asyncLedgerData), `"attempts"`)

		asyncTranscriptPath := valueFromOutputLine(result.stdout, "transcript=")
		require.NotEmpty(t, asyncTranscriptPath)
		asyncTranscriptData, readErr := os.ReadFile(asyncTranscriptPath)
		require.NoError(t, readErr)
		assertContains(t, string(asyncTranscriptData), "# stdout")
		assertContains(t, string(asyncTranscriptData), "child:plan agent:planner")
		assertContains(t, string(asyncTranscriptData), "# stderr")
		assertContains(t, string(asyncTranscriptData), "workspace:")

		childRunLogData, readErr = os.ReadFile(childRunLog)
		require.NoError(t, readErr)
		require.Equal(t, "child\nplan\ncode\n", string(childRunLogData))

		result = runOK(t, childRunSpec,
			"--async-run",
			"--async-task", "plan|planner|draft plan",
			"--async-task", "code|coder|implement|plan",
			"--spawn-binary", fakeAtteler,
			"--spawn-ledger", asyncLedger,
			"--spawn-resume",
		)
		assertContains(t, result.stdout, "wave=1\torder=1\tid=plan\tagent=planner\tstatus=skipped")
		assertContains(t, result.stdout, "wave=2\torder=1\tid=code\tagent=coder\tstatus=skipped")
		assertContains(t, result.stdout, "ledger="+asyncLedger)
		assertContains(t, result.stdout, "admission_id=")

		childRunLogData, readErr = os.ReadFile(childRunLog)
		require.NoError(t, readErr)
		require.Equal(t, "child\nplan\ncode\n", string(childRunLogData))
	}

	taskFile := filepath.Join(workDir, "tasks.json")
	result = runOK(t, runSpec{dir: workDir}, "--task-file", taskFile, "--task-add", "draft the task list CLI", "--task-id", "todo-1")
	assertContains(t, result.stdout, "id=todo-1")
	assertContains(t, result.stdout, "status=ready")

	result = runOK(t, runSpec{dir: workDir}, "--task-file", taskFile, "--task-assign", "todo-1:executor")
	assertContains(t, result.stdout, "status=in_progress")
	assertContains(t, result.stdout, "agent=executor")

	result = runOK(t, runSpec{dir: workDir}, "--task-file", taskFile, "--task-complete", "todo-1", "--task-agent", "executor")
	assertContains(t, result.stdout, "status=completed")
	assertContains(t, result.stdout, "agent=executor")

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
  "evaluations": [
    {"created_at": "2026-05-02T10:00:30Z", "agent": "reviewer", "outcome": "fail", "notes": "missed auth regression", "reference": "eval-before.md", "score": 1},
    {"created_at": "2026-05-02T10:01:00Z", "agent": "reviewer", "outcome": "pass", "notes": "auth regression covered", "reference": "eval-after.md", "score": 5}
  ],
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
	assertContains(t, result.stdout, "Recorded 1 pending feedback guidance decision")

	configData, err := os.ReadFile(configPath)
	require.NoError(t, err)
	assertContains(t, string(configData), "feedback_guidance:")
	assertContains(t, string(configData), "status: pending")
	assertContains(t, string(configData), "source_run: feedback")
	assertNotContains(t, string(configData), "Feedback-derived guidance:")

	historyData, err := os.ReadFile(feedbackHistory)
	require.NoError(t, err)
	assertContains(t, string(historyData), "agent: reviewer")
	assertContains(t, string(historyData), "status: pending")
}

func TestAutomaticSkillLearningCommands(t *testing.T) {
	t.Parallel()

	workDir := t.TempDir()
	learningDir := filepath.Join(workDir, "learning")
	generatedSkillDir := filepath.Join(workDir, "generated-skills")
	spec := runSpec{
		dir: workDir,
		env: []string{
			"ATTELER_SKILL_LEARNING=true",
			"ATTELER_SKILL_LEARNING_DIR=" + learningDir,
			"ATTELER_SKILL_LEARNING_SKILL_DIR=" + generatedSkillDir,
		},
	}

	for range 2 {
		runOK(t, spec, "--bash", "echo plan")
		runOK(t, spec, "--bash", "echo code")
	}

	result := runOK(t, spec, "agents", "skill-learning-list")
	assertContains(t, result.stdout, "enabled: true")
	assertContains(t, result.stdout, "skills: 1")

	entries, err := os.ReadDir(generatedSkillDir)
	require.NoError(t, err)
	require.Len(t, entries, 1)

	slug := entries[0].Name()
	skillPath := filepath.Join(generatedSkillDir, slug, "SKILL.md")
	skillData, err := os.ReadFile(skillPath)
	require.NoError(t, err)
	assertContains(t, string(skillData), "run echo plan")
	assertContains(t, string(skillData), "run echo code")

	result = runOK(t, spec, "agents", "skill-learning-show", slug)
	assertContains(t, result.stdout, "run echo plan")

	runOK(t, spec, "agents", "skill-learning-disable", slug)
	result = runOK(t, spec, "agents", "skill-learning-list")
	assertContains(t, result.stdout, slug+"\tdisabled")

	runOK(t, spec, "agents", "skill-learning-delete", slug)
	result = runOK(t, spec, "agents", "skill-learning-list")
	assertContains(t, result.stdout, "skills: 0")
	require.NoDirExists(t, filepath.Join(generatedSkillDir, slug))
}

func TestSpeculateRunFailsClosedWhenJudgeGatesInvalid(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name        string
		judgeOutput string
		wantErr     string
	}{
		{
			name:        "missing",
			judgeOutput: "WINNER: gpt-4.1\nREASON: no structured gates",
			wantErr:     `missing gate checks: "tests pass", "lint pass"`,
		},
		{
			name: "malformed",
			judgeOutput: "WINNER: gpt-4.1\nREASON: malformed gate\n" +
				"GATE tests pass: PASSING maybe\n" +
				"GATE lint pass: PASS clean",
			wantErr: `gate check "tests pass" failed: malformed gate status`,
		},
		{
			name: "duplicate",
			judgeOutput: "WINNER: gpt-4.1\nREASON: duplicate gate\n" +
				"GATE tests pass: PASS first\n" +
				"GATE tests pass: PASS second\n" +
				"GATE lint pass: PASS clean",
			wantErr: `duplicate gate check "tests pass"`,
		},
		{
			name: "unknown",
			judgeOutput: "WINNER: gpt-4.1\nREASON: unknown gate\n" +
				"GATE tests pass: PASS covered\n" +
				"GATE lint pass: PASS clean\n" +
				"GATE deploy pass: PASS shipped",
			wantErr: `unknown gate check "deploy pass"`,
		},
		{
			name: "failed",
			judgeOutput: "WINNER: gpt-4.1\nREASON: explicit failure\n" +
				"GATE tests pass: FAIL tests red\n" +
				"GATE lint pass: PASS clean",
			wantErr: `gate check "tests pass" failed: tests red`,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			workDir := t.TempDir()
			configPath := filepath.Join(workDir, "atteler.yaml")
			writeSpeculateOpenAIConfig(t, configPath)

			openAIAPI := startFakeOpenAIChatCompletions(t, []string{
				"proposal from gpt-4.1",
				tt.judgeOutput,
			})
			defer openAIAPI.Close()

			result, err := runAtteler(t, runSpec{
				dir: workDir,
				env: []string{
					"OPENAI_API_KEY=e2e-test-key",
					"OPENAI_BASE_URL=" + openAIAPI.URL,
				},
			}, "--config", configPath,
				"--speculate-run",
				"--speculate-agent", "gpt-4.1",
				"--speculate-gate", "tests pass",
				"--speculate-gate", "lint pass",
				"--speculate-prompt", "ship safely")
			require.Error(t, err)
			assertContains(t, result.stdout, "winner: gpt-4.1")
			assertContains(t, result.stdout, "error: "+tt.wantErr)
			assertContains(t, result.stderr, "error: speculate-run: "+tt.wantErr)
		})
	}
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
	assertContains(t, result.stdout, "failures=0")
	assertContains(t, result.stdout, "routing_eligible=false")
	assertContains(t, result.stdout, "validity_eligible_buckets=0")
	assertContains(t, result.stdout, "validity_min_sample_size=10")
	assertContains(t, result.stdout, "validity_min_recent_samples=3")
	assertContains(t, result.stdout, "validity_max_stderr=5.00")
	assertContains(t, result.stdout, "validity_min_confidence=0.70")
	assertContains(t, result.stdout, "provenance=legacy:1")
	assertContains(t, result.stdout, "rubrics=legacy-unversioned:1")
	assertContains(t, result.stdout, "avg_score=8.00")
	assertContains(t, result.stdout, "score_buckets=source=legacy/rubric=legacy-unversioned")
	assertContains(t, result.stdout, "negative_knowledge_breakdown=unspecified/unspecified:1")
	assertContains(t, result.stdout, "outcomes=pass:1")

	result = runOK(t, spec,
		"--session", "demo",
		"--record-evaluation", "planner",
		"--evaluation-outcome", "pass",
		"--evaluation-source", "ci",
		"--evaluation-evaluator", "ci-eval",
		"--evaluation-rubric-version", "planning/v3",
		"--evaluation-task-type", "planning",
		"--evaluation-difficulty", "hard",
		"--evaluation-expected-outcome", "accepted plan",
		"--evaluation-agent-version", "planner@2",
		"--evaluation-score", "88",
		"--evaluation-duration-millis", "2500",
		"--evaluation-cost", "0.02",
		"--evaluation-confidence", "0.90",
	)
	assertContains(t, result.stdout, "Recorded evaluation on session demo")

	result = runOK(t, spec, "--session", "demo", "--list-evaluations")
	assertContains(t, result.stdout, "agent=planner")
	assertContains(t, result.stdout, "source=ci")
	assertContains(t, result.stdout, "evaluator=ci-eval")
	assertContains(t, result.stdout, "rubric_version=planning/v3")
	assertContains(t, result.stdout, "task_type=planning")
	assertContains(t, result.stdout, "difficulty=hard")
	assertContains(t, result.stdout, "expected_outcome=accepted plan")
	assertContains(t, result.stdout, "model=gpt-test")
	assertContains(t, result.stdout, "agent_version=planner@2")
	assertContains(t, result.stdout, "duration_millis=2500")
	assertContains(t, result.stdout, "cost=0.020000")
	assertContains(t, result.stdout, "confidence=0.90")

	result = runOK(t, spec,
		"--session", "demo",
		"--record-failure", "skip migration dry-run",
		"--failure-reason", "missed data loss",
		"--failure-task-type", "migration",
		"--failure-severity", "critical",
	)
	assertContains(t, result.stdout, "Recorded failure on session demo")

	result = runOK(t, spec, "--session", "demo", "--list-failures")
	assertContains(t, result.stdout, "task_type=migration")
	assertContains(t, result.stdout, "severity=critical")
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
spawn env PATH=$env(ATTELER_TEST_PATH) ATTELER_STATE=$env(ATTELER_STATE) ATTELER_SESSION_DIR=$env(ATTELER_SESSION_DIR) CODEX_HOME=$env(CODEX_HOME) ATTELER_CREDENTIAL_ALLOWED_STORES=codex_auth_json ATTELER_ALLOW_BORROWED_OAUTH=1 $env(ATTELER_BIN)
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
spawn env PATH=$env(ATTELER_TEST_PATH) ATTELER_STATE=$env(ATTELER_STATE) ATTELER_SESSION_DIR=$env(ATTELER_SESSION_DIR) CODEX_HOME=$env(CODEX_HOME) ATTELER_CREDENTIAL_ALLOWED_STORES=codex_auth_json ATTELER_ALLOW_BORROWED_OAUTH=1 $env(ATTELER_BIN)
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
  codex:
    credential_policy:
      allowed_stores: [codex_auth_json]
      allow_borrowed_oauth: true
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
		"bytes=18",
		"truncated=false",
		"event:context_add",
		"event:file_write",
		"kind=session",
		"event:agent_execute",
		"agent=reviewer",
		"event:tool_execute",
		"provider=codex",
		"event:command_execute",
		"redacted=true",
	} {
		assertContains(t, result.stderr, want)
	}

	assertNotContains(t, result.stderr, "path=README.md")
	assertNotContains(t, result.stderr, `command=codex.responses`)
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

// startFakeOpenAIChatCompletions spins up an httptest server that mimics the
// OpenAI /v1/chat/completions endpoint, returning one assistant text per call.
func startFakeOpenAIChatCompletions(t *testing.T, texts []string) *httptest.Server {
	t.Helper()

	var calls atomic.Int32

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasSuffix(r.URL.Path, "/chat/completions") {
			http.NotFound(w, r)
			return
		}

		var req struct {
			Stream bool `json:"stream"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Errorf("decode OpenAI chat request: %v", err)

			return
		}

		index := int(calls.Add(1)) - 1
		if index >= len(texts) {
			index = len(texts) - 1
		}

		if req.Stream {
			w.Header().Set("Content-Type", "text/event-stream")
			fmt.Fprintf(w, "data: {\"model\":\"gpt-4.1\",\"choices\":[{\"delta\":{\"content\":%q}}]}\n\n", texts[index])
			fmt.Fprint(w, "data: {\"model\":\"gpt-4.1\",\"choices\":[{\"finish_reason\":\"stop\",\"delta\":{}}],\"usage\":{\"prompt_tokens\":1,\"completion_tokens\":1}}\n\n")
			fmt.Fprint(w, "data: [DONE]\n\n")

			return
		}

		w.Header().Set("Content-Type", "application/json")
		fmt.Fprintf(w, `{"model":"gpt-4.1","choices":[{"finish_reason":"stop","message":{"content":%q}}],"usage":{"prompt_tokens":1,"completion_tokens":1}}`, texts[index])
	}))

	return srv
}

func writeSpeculateOpenAIConfig(t *testing.T, path string) {
	t.Helper()

	writeFile(t, path, `default_provider: openai
default_model: gpt-4.1
fallback_models:
  - gpt-4.1
providers:
  anthropic:
    disabled: true
  claude-code:
    disabled: true
  codex:
    disabled: true
  ollama:
    disabled: true
  openai:
`)
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
  claude-code:
    credential_policy:
      allowed_stores: [claude_code_file]
      allow_borrowed_oauth: true
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
	assertContains(t, result.stderr, "event:command_execute")
	assertNotContains(t, result.stderr, `command=claude_code.messages`)
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

		var req struct {
			Stream bool `json:"stream"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Errorf("decode Anthropic messages request: %v", err)

			return
		}

		if req.Stream {
			w.Header().Set("Content-Type", "text/event-stream")
			fmt.Fprintf(w, "event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"model\":%q,\"usage\":{\"input_tokens\":1}}}\n\n", model)
			fmt.Fprintf(w, "event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"text_delta\",\"text\":%q}}\n\n", text)
			fmt.Fprint(w, "event: message_delta\ndata: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"end_turn\"},\"usage\":{\"output_tokens\":1}}\n\n")
			fmt.Fprint(w, "event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n")

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
	stdin      string
	env        []string
	timeout    time.Duration
}

type runResult struct {
	stdout string
	stderr string
}

func TestE2ETestEnvIsolatesPersistentState(t *testing.T) {
	t.Setenv("ATTELER_STATE", filepath.Join(t.TempDir(), "host-state.yaml"))
	t.Setenv("XDG_STATE_HOME", filepath.Join(t.TempDir(), "host-xdg-state"))

	env := testEnv(t, runSpec{env: []string{"HOME=" + filepath.Join(t.TempDir(), "credential-home")}})
	values := envValues(env)

	require.NotEmpty(t, values["ATTELER_STATE"])
	require.NotContains(t, values["ATTELER_STATE"], "host-state")
	require.NotContains(t, values["ATTELER_STATE"], "credential-home")
	require.NotContains(t, values["XDG_STATE_HOME"], "host-xdg-state")
	require.NotContains(t, values["XDG_STATE_HOME"], "credential-home")
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

	if spec.stdin != "" {
		cmd.Stdin = strings.NewReader(spec.stdin)
	}

	err := cmd.Run()

	return runResult{stdout: stdout.String(), stderr: stderr.String()}, err
}

func runSymphonyOK(t *testing.T, spec runSpec, args ...string) runResult {
	t.Helper()

	result, err := runSymphony(t, spec, args...)
	if err != nil {
		require.Failf(t, "unexpected failure", "symphony %v failed: %v\nstdout:\n%s\nstderr:\n%s", args, err, result.stdout, result.stderr)
	}

	return result
}

func runSymphony(t *testing.T, spec runSpec, args ...string) (runResult, error) {
	t.Helper()

	timeout := spec.timeout
	if timeout <= 0 {
		timeout = 30 * time.Second
	}

	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()

	cmd := exec.CommandContext(ctx, e2eSymphonyBinary, args...)
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
		"ANTHROPIC_API_KEY":                true,
		"ANTHROPIC_BASE_URL":               true,
		"ATTELER_CONFIG":                   true,
		"ATTELER_STATE":                    true,
		"ATTELER_SKILL_LEARNING":           true,
		"ATTELER_SKILL_LEARNING_DIR":       true,
		"ATTELER_SKILL_LEARNING_SKILL_DIR": true,
		"ATTELER_SESSION_DIR":              true,
		"CODEX_HOME":                       true,
		"FORGE_CONFIG":                     true,
		"HOME":                             true,
		"OPENAI_API_KEY":                   true,
		"OPENAI_BASE_URL":                  true,
		"OLLAMA_BASE_URL":                  true,
		"XDG_CONFIG_HOME":                  true,
		"XDG_STATE_HOME":                   true,
	}

	env := make([]string, 0, len(os.Environ())+14+len(spec.env))
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
		"ATTELER_STATE="+filepath.Join(home, "state.yaml"),
		"ATTELER_SKILL_LEARNING=false",
		"ATTELER_SESSION_DIR="+sessionDir,
		"CODEX_HOME=",
		"FORGE_CONFIG=",
		"HOME="+home,
		"OPENAI_API_KEY=",
		"OPENAI_BASE_URL=",
		"OLLAMA_BASE_URL=",
		"XDG_CONFIG_HOME="+filepath.Join(home, ".config"),
		"XDG_STATE_HOME="+filepath.Join(home, ".local", "state"),
	)
	env = append(env, spec.env...)

	return env
}

type e2eSavedSession struct {
	ID             string `json:"id"`
	WorktreePath   string `json:"worktree_path"`
	WorktreeBranch string `json:"worktree_branch"`
	WorktreeBase   string `json:"worktree_base"`
}

func initE2EGitRepo(t *testing.T) string {
	t.Helper()

	dir := t.TempDir()
	gitOutputE2E(t, dir, "init")
	gitOutputE2E(t, dir, "config", "user.email", "e2e@example.com")
	gitOutputE2E(t, dir, "config", "user.name", "Atteler E2E")
	gitOutputE2E(t, dir, "config", "commit.gpgsign", "false")
	gitOutputE2E(t, dir, "config", "core.excludesFile", os.DevNull)
	gitOutputE2E(t, dir, "commit", "--allow-empty", "-m", "init")

	return dir
}

func gitOutputE2E(t *testing.T, dir string, args ...string) string {
	t.Helper()

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	cmd := exec.CommandContext(ctx, "git", args...)
	cmd.Dir = dir

	output, err := cmd.CombinedOutput()
	if err != nil {
		require.Failf(t, "unexpected git failure", "git %v in %s failed: %v\n%s", args, dir, err, output)
	}

	return string(output)
}

func writeReplayResponse(t *testing.T, dir string) string {
	t.Helper()

	path := filepath.Join(dir, "replay.json")
	writeFile(t, path, `{"response":{"content":"ok","model":"replay/model"}}`)

	return path
}

func readOnlyE2ESession(t *testing.T, sessionDir string) e2eSavedSession {
	t.Helper()

	matches, err := filepath.Glob(filepath.Join(sessionDir, "*.json"))
	require.NoError(t, err)
	require.Len(t, matches, 1)

	data, err := os.ReadFile(matches[0])
	require.NoError(t, err)

	var sess e2eSavedSession
	require.NoError(t, json.Unmarshal(data, &sess))
	require.NotEmpty(t, sess.ID)
	require.NotEmpty(t, sess.WorktreePath)
	require.NotEmpty(t, sess.WorktreeBranch)
	require.NotEmpty(t, sess.WorktreeBase)

	return sess
}

func envValues(env []string) map[string]string {
	out := make(map[string]string, len(env))
	for _, item := range env {
		key, value, ok := strings.Cut(item, "=")
		if !ok {
			continue
		}

		out[key] = value
	}

	return out
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
	writeFile(t, filepath.Join(dir, "demo.json"), fmt.Sprintf(`{
  "created_at": "2026-04-30T10:00:00Z",
  "updated_at": "2026-04-30T10:05:00Z",
  "id": "demo",
  "title": "Auth review",
  "default_model": "gpt-test",
  "default_agent": "removed-agent",
  "worktree_path": %q,
  "tags": ["auth", "review"],
  "messages": [
    {"role": "user", "content": "hello auth"},
    {"role": "assistant", "content": "hi there"}
  ]
}`, filepath.Dir(dir)))
}

func onlySessionID(t *testing.T, sessionDir string) string {
	t.Helper()

	matches, err := filepath.Glob(filepath.Join(sessionDir, "*.json"))
	require.NoError(t, err)
	require.Len(t, matches, 1)

	return strings.TrimSuffix(filepath.Base(matches[0]), ".json")
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

func valueFromOutputLine(output, prefix string) string {
	for line := range strings.SplitSeq(output, "\n") {
		if value, ok := strings.CutPrefix(line, prefix); ok {
			return strings.TrimSpace(value)
		}
	}

	return ""
}

func assertNotContains(t *testing.T, haystack, needle string) {
	t.Helper()

	if strings.Contains(haystack, needle) {
		require.Failf(t, "unexpected failure", "unexpected %q in:\n%s", needle, haystack)
	}
}

func countIndentedFlagLines(text string) int {
	count := 0

	for line := range strings.SplitSeq(text, "\n") {
		if strings.HasPrefix(line, "  -") {
			count++
		}
	}

	return count
}
