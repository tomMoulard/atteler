package e2e

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

const liveTimeout = 2 * time.Minute

//nolint:paralleltest // reads live provider environment and may consume provider quota.
func TestLiveOpenAIOneShot(t *testing.T) {
	requireLive(t)
	apiKey := requireEnv(t, "OPENAI_API_KEY")
	model := envOrDefault("ATTELER_E2E_OPENAI_MODEL", "gpt-4.1-mini")
	marker := "atteler-live-openai-ok"

	workDir := t.TempDir()
	configPath := filepath.Join(workDir, "atteler.yaml")
	writeFile(t, configPath, `default_provider: openai
providers:
  anthropic:
    disabled: true
generation:
  temperature: 0
  max_tokens: 16
`)

	result := runOK(t, runSpec{
		dir:     workDir,
		timeout: liveTimeout,
		env: []string{
			"OPENAI_API_KEY=" + apiKey,
		},
	}, "--config", configPath, "--model", model, "--once", "Reply with exactly: "+marker)

	assertContains(t, result.stdout, marker)
	assertContains(t, result.stderr, "session:")
}

//nolint:paralleltest // reads live provider environment and may consume provider quota.
func TestLiveAnthropicOneShot(t *testing.T) {
	requireLive(t)
	apiKey := requireEnv(t, "ANTHROPIC_API_KEY")
	model := envOrDefault("ATTELER_E2E_ANTHROPIC_MODEL", "claude-haiku-4-20250414")
	marker := "atteler-live-anthropic-ok"

	workDir := t.TempDir()
	configPath := filepath.Join(workDir, "atteler.yaml")
	writeFile(t, configPath, `default_provider: anthropic
providers:
  openai:
    disabled: true
generation:
  temperature: 0
  max_tokens: 16
`)

	result := runOK(t, runSpec{
		dir:     workDir,
		timeout: liveTimeout,
		env: []string{
			"ANTHROPIC_API_KEY=" + apiKey,
		},
	}, "--config", configPath, "--model", model, "--once", "Reply with exactly: "+marker)

	assertContains(t, result.stdout, marker)
	assertContains(t, result.stderr, "session:")
}

//nolint:paralleltest // reads live provider environment and may consume provider quota.
func TestLiveForgeClaudeOneShot(t *testing.T) {
	requireLive(t)
	forgeConfig := requireForgeConfig(t)
	model := envOrDefault("ATTELER_E2E_FORGE_ANTHROPIC_MODEL", "claude-haiku-4-5-20251001")
	marker := "atteler-live-forge-claude-ok"

	workDir := t.TempDir()
	configPath := filepath.Join(workDir, "atteler.yaml")
	writeFile(t, configPath, `default_provider: anthropic
providers:
  openai:
    disabled: true
generation:
  temperature: 0
  max_tokens: 16
`)

	result := runOK(t, runSpec{
		dir:     workDir,
		timeout: liveTimeout,
		env: []string{
			"FORGE_CONFIG=" + forgeConfig,
		},
	}, "--config", configPath, "--model", model, "--once", "Reply with exactly: "+marker)

	assertContains(t, result.stdout, marker)
	assertContains(t, result.stderr, "session:")
}

//nolint:paralleltest // reads live provider environment and may consume provider quota.
func TestLiveClaudeCodeOneShot(t *testing.T) {
	requireLive(t)
	requireClaudeCode(t)

	model := envOrDefault("ATTELER_E2E_CLAUDE_CODE_MODEL", "claude-haiku-4-5")
	marker := "atteler-live-claude-code-ok"

	workDir := t.TempDir()
	configPath := filepath.Join(workDir, "atteler.yaml")
	writeFile(t, configPath, `default_provider: claude-code
providers:
  anthropic:
    disabled: true
  codex:
    disabled: true
  openai:
    disabled: true
generation:
  temperature: 0
  max_tokens: 16
`)

	result := runOK(t, runSpec{
		dir:     workDir,
		timeout: liveTimeout,
		env: []string{
			"HOME=" + os.Getenv("HOME"),
		},
	}, "--config", configPath, "--model", model, "--once", "Reply with exactly: "+marker)

	assertContains(t, result.stdout, marker)
	assertContains(t, result.stderr, "session:")
}

//nolint:paralleltest // reads live provider environment and may consume provider quota.
func TestLiveCodexOneShot(t *testing.T) {
	requireLive(t)
	codexHome := requireCodexHome(t)
	model := envOrDefault("ATTELER_E2E_CODEX_MODEL", "gpt-5.5")
	marker := "atteler-live-codex-ok"

	workDir := t.TempDir()
	configPath := filepath.Join(workDir, "atteler.yaml")
	writeFile(t, configPath, `default_provider: codex
providers:
  anthropic:
    disabled: true
  openai:
    disabled: true
generation:
  temperature: 0
  max_tokens: 16
`)

	result := runOK(t, runSpec{
		dir:     workDir,
		timeout: liveTimeout,
		env: []string{
			"CODEX_HOME=" + codexHome,
		},
	}, "--config", configPath, "--model", model, "--once", "Reply with exactly: "+marker)

	assertContains(t, result.stdout, marker)
	assertContains(t, result.stderr, "session:")
}

func requireLive(t *testing.T) {
	t.Helper()

	if os.Getenv("ATTELER_E2E_LIVE") != "1" {
		t.Skip("set ATTELER_E2E_LIVE=1 to run live LLM e2e tests")
	}
}

func requireEnv(t *testing.T, name string) string {
	t.Helper()

	value := os.Getenv(name)
	if value == "" {
		t.Skipf("%s is required for this live e2e test", name)
	}

	return value
}

func requireClaudeCode(t *testing.T) {
	t.Helper()

	path, err := exec.LookPath("claude")
	if err != nil {
		t.Skip("claude executable is required for this live Claude Code test")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	output, err := exec.CommandContext(ctx, path, "auth", "status").CombinedOutput()
	if err != nil || !strings.Contains(string(output), `"loggedIn": true`) {
		t.Skipf("Claude Code login is required for this live test: %s", output)
	}
}

func requireCodexHome(t *testing.T) string {
	t.Helper()

	dir := os.Getenv("ATTELER_E2E_CODEX_HOME")
	if dir == "" {
		dir = filepath.Join(os.Getenv("HOME"), ".codex")
	}
	//nolint:gosec // Live e2e intentionally probes the caller-provided Codex home.
	if _, err := os.Stat(filepath.Join(dir, "auth.json")); err != nil {
		t.Skipf("Codex credentials not available in %s: %v", dir, err)
	}

	return dir
}

func requireForgeConfig(t *testing.T) string {
	t.Helper()

	dir := os.Getenv("ATTELER_E2E_FORGE_CONFIG")
	if dir == "" {
		t.Skip("ATTELER_E2E_FORGE_CONFIG is required for this live ForgeCode test")
	}
	//nolint:gosec // Live e2e intentionally probes the caller-provided Forge config directory.
	if _, err := os.Stat(filepath.Join(dir, ".credentials.json")); err != nil {
		t.Skipf("ForgeCode credentials not available in %s: %v", dir, err)
	}

	return dir
}

func envOrDefault(name, fallback string) string {
	if value := os.Getenv(name); value != "" {
		return value
	}

	return fallback
}
