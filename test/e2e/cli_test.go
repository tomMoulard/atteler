package e2e

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

var (
	e2eBinary string
	e2eTmpDir string
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

	e2eBinary = filepath.Join(e2eTmpDir, "atteler")
	if runtime.GOOS == "windows" {
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
	workDir := t.TempDir()
	configPath := filepath.Join(workDir, "atteler.yaml")

	result := runOK(t, runSpec{dir: workDir}, "--print-config-template")
	assertContains(t, result.stdout, "default_provider: openai")
	assertNotContains(t, result.stdout, "api.openai.com/v1")

	result = runOK(t, runSpec{dir: workDir}, "--init-config", configPath)
	assertContains(t, result.stdout, "Wrote "+configPath)
	if _, err := os.Stat(configPath); err != nil {
		t.Fatalf("config was not written: %v", err)
	}

	_, err := runAtteler(t, runSpec{dir: workDir}, "--init-config", configPath)
	if err == nil {
		t.Fatal("expected init-config to refuse overwriting an existing file")
	}

	result = runOK(t, runSpec{dir: workDir}, "--config", configPath, "--validate-config")
	assertContains(t, result.stdout, "Config valid:")

	result = runOK(t, runSpec{dir: workDir}, "--config", configPath, "--list-config-paths")
	assertContains(t, result.stdout, configPath+"\tpresent")
}

func TestOfflineProviderCommands(t *testing.T) {
	workDir := t.TempDir()

	result := runOK(t, runSpec{dir: workDir}, "--version")
	assertContains(t, result.stdout, "atteler")

	result = runOK(t, runSpec{dir: workDir}, "--list-providers")
	assertContains(t, result.stdout, "anthropic")
	assertContains(t, result.stdout, "openai")

	result = runOK(t, runSpec{dir: workDir}, "--list-known-models")
	assertContains(t, result.stdout, "openai/gpt-4.1")
	assertContains(t, result.stdout, "anthropic/claude-sonnet")
}

func TestAgentCommands(t *testing.T) {
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

func TestSessionCommands(t *testing.T) {
	workDir := t.TempDir()
	sessionDir := filepath.Join(workDir, "sessions")
	writeSession(t, sessionDir)
	spec := runSpec{dir: workDir, sessionDir: sessionDir}

	result := runOK(t, spec, "--list-sessions")
	assertContains(t, result.stdout, "demo")
	assertContains(t, result.stdout, "title=Auth review")
	assertContains(t, result.stdout, "tags=auth,review")

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
}

func TestInteractiveFZFModelPickerPersistsFolderDefault(t *testing.T) {
	if runtime.GOOS == "windows" {
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
	writeFile(t, filepath.Join(codexHome, "auth.json"), `{"tokens":{"access_token":"token"}}`)

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
		t.Fatalf("read state: %v", err)
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
		t.Fatalf("atteler %v failed: %v\nstdout:\n%s\nstderr:\n%s", args, err, result.stdout, result.stderr)
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
		t.Fatalf("expect %s failed: %v\nstdout:\n%s\nstderr:\n%s", scriptPath, err, stdout.String(), stderr.String())
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
		t.Fatal(err)
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
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
}

func assertContains(t *testing.T, haystack, needle string) {
	t.Helper()
	if !strings.Contains(haystack, needle) {
		t.Fatalf("missing %q in:\n%s", needle, haystack)
	}
}

func assertNotContains(t *testing.T, haystack, needle string) {
	t.Helper()
	if strings.Contains(haystack, needle) {
		t.Fatalf("unexpected %q in:\n%s", needle, haystack)
	}
}
