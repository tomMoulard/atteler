package e2e

import (
	"context"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/tommoulard/atteler/pkg/llm"
)

const liveTimeout = 2 * time.Minute

const (
	liveOpenAIDefaultModel         = "gpt-4.1-mini"
	liveAnthropicDefaultModel      = "claude-haiku-4-5-20251001"
	liveForgeAnthropicDefaultModel = "claude-haiku-4-5-20251001"
	liveClaudeCodeDefaultModel     = "claude-haiku-4-5-20251001"
	liveCodexDefaultModel          = "gpt-5.5"
)

//nolint:paralleltest // reads live provider environment and may consume provider quota.
func TestLiveOpenAIOneShot(t *testing.T) {
	requireLive(t)
	apiKey, baseURL := requireOpenAI(t)
	model := envOrDefault("ATTELER_E2E_OPENAI_MODEL", liveOpenAIDefaultModel)
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
			"OPENAI_BASE_URL=" + baseURL,
		},
	}, "--config", configPath, "--model", model, "--once", "Reply with exactly: "+marker)

	assertContains(t, result.stdout, marker)
	assertContains(t, result.stderr, "session:")
}

//nolint:paralleltest // reads live provider environment and may consume provider quota.
func TestLiveAnthropicOneShot(t *testing.T) {
	requireLive(t)
	apiKey := requireAnthropic(t)
	model := envOrDefault("ATTELER_E2E_ANTHROPIC_MODEL", liveAnthropicDefaultModel)
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
	model := envOrDefault("ATTELER_E2E_FORGE_ANTHROPIC_MODEL", liveForgeAnthropicDefaultModel)
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

	model := envOrDefault("ATTELER_E2E_CLAUDE_CODE_MODEL", liveClaudeCodeDefaultModel)
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

	// Create an isolated CODEX_HOME with an empty config.toml so the harness
	// import reads it (and gets nothing) instead of falling through to the
	// real ~/.codex/config.toml which may set model_mode or other overrides.
	codexHome := t.TempDir()
	writeFile(t, filepath.Join(codexHome, "config.toml"), "")

	result := runOK(t, runSpec{
		dir:     workDir,
		timeout: liveTimeout,
		env: []string{
			"HOME=" + os.Getenv("HOME"),
			"CODEX_HOME=" + codexHome,
			"ATTELER_STATE=" + filepath.Join(t.TempDir(), "state.yaml"),
		},
	}, "--config", configPath, "--model", model, "--once", "Reply with exactly: "+marker)

	assertContains(t, result.stdout, marker)
	assertContains(t, result.stderr, "session:")
}

//nolint:paralleltest // reads live provider environment and may consume provider quota.
func TestLiveCodexOneShot(t *testing.T) {
	requireLive(t)
	codexHome := requireCodexHome(t)
	model := envOrDefault("ATTELER_E2E_CODEX_MODEL", liveCodexDefaultModel)
	marker := "atteler-live-codex-ok"

	workDir := t.TempDir()
	configPath := filepath.Join(workDir, "atteler.yaml")
	writeFile(t, configPath, liveCodexConfig())

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

func TestLiveCodexConfigAvoidsUnsupportedOrOmittedKnobs(t *testing.T) {
	t.Parallel()

	config := liveCodexConfig()
	for _, unsupported := range []string{"max_tokens", "temperature", "top_p", "seed"} {
		if strings.Contains(config, unsupported+":") {
			t.Fatalf("live Codex config must not set unsupported or omitted %s", unsupported)
		}
	}
}

func TestLiveProviderDefaultModelsAreKnown(t *testing.T) {
	t.Parallel()

	knownModels := knownModelsByProvider()
	for _, tc := range []struct {
		provider string
		model    string
	}{
		{provider: "openai", model: liveOpenAIDefaultModel},
		{provider: "anthropic", model: liveAnthropicDefaultModel},
		{provider: "anthropic", model: liveForgeAnthropicDefaultModel},
		{provider: "claude-code", model: liveClaudeCodeDefaultModel},
		{provider: "codex", model: liveCodexDefaultModel},
	} {
		if !knownModels[tc.provider][tc.model] {
			t.Fatalf("live default model %s/%s is not in llm.KnownProviders", tc.provider, tc.model)
		}
	}
}

func TestLiveProviderDocsMatchLiveTestConfiguration(t *testing.T) {
	t.Parallel()

	root, err := repoRoot()
	if err != nil {
		t.Fatal(err)
	}

	readme, err := os.ReadFile(filepath.Join(root, "README.md"))
	if err != nil {
		t.Fatal(err)
	}

	docs := string(readme)
	for _, expected := range []string{
		"set ATTELER_E2E_LIVE=1 to run live LLM e2e tests",
		"`TestLiveOpenAIOneShot` | `OPENAI_API_KEY` | `OPENAI_API_KEY is required for this live e2e test` | `ATTELER_E2E_OPENAI_MODEL` | `" + liveOpenAIDefaultModel + "`",
		"`TestLiveAnthropicOneShot` | `ANTHROPIC_API_KEY` | `ANTHROPIC_API_KEY is required for this live e2e test` | `ATTELER_E2E_ANTHROPIC_MODEL` | `" + liveAnthropicDefaultModel + "`",
		"`TestLiveForgeClaudeOneShot` | `ATTELER_E2E_FORGE_CONFIG`",
		"`ATTELER_E2E_FORGE_CONFIG is required for this live ForgeCode test`",
		"`ForgeCode credentials not available in <dir>`",
		"`ATTELER_E2E_FORGE_ANTHROPIC_MODEL` | `" + liveForgeAnthropicDefaultModel + "`",
		"`TestLiveClaudeCodeOneShot` | Claude Code login",
		"`Claude Code login is required for this live test`",
		"`ATTELER_E2E_CLAUDE_CODE_MODEL` | `" + liveClaudeCodeDefaultModel + "`",
		"`TestLiveCodexOneShot` | Codex `auth.json`",
		"`Codex credentials not available in <dir>`",
		"`ATTELER_E2E_CODEX_MODEL` | `" + liveCodexDefaultModel + "`",
	} {
		assertContains(t, docs, expected)
	}
}

func liveCodexConfig() string {
	return `default_provider: codex
providers:
  anthropic:
    disabled: true
  openai:
    disabled: true
`
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

// requireOpenAI verifies that the OPENAI_API_KEY is set and the account is
// reachable (correct regional endpoint, valid key). Skips the test when the
// key is missing or the API returns an auth error.
func requireOpenAI(t *testing.T) (apiKey, baseURL string) {
	t.Helper()

	apiKey = requireEnv(t, "OPENAI_API_KEY")
	baseURL = os.Getenv("OPENAI_BASE_URL")

	endpoint := baseURL
	if endpoint == "" {
		endpoint = "https://api.openai.com"
	}

	probeAPI(t, "OpenAI", endpoint+"/v1/models", func(r *http.Request) {
		r.Header.Set("Authorization", "Bearer "+apiKey)
	})

	return apiKey, baseURL
}

// requireAnthropic verifies that the ANTHROPIC_API_KEY is set and the account
// is usable (valid key, sufficient credits). Skips the test otherwise.
// The /v1/models endpoint succeeds even without credits, so we probe
// /v1/messages with a minimal request to catch billing issues early.
func requireAnthropic(t *testing.T) string {
	t.Helper()

	apiKey := requireEnv(t, "ANTHROPIC_API_KEY")

	// Minimal messages request — the model is cheap and the max_tokens is 1.
	// If the account has no credits, Anthropic returns HTTP 400 with
	// "credit balance is too low".
	body := `{"model":"claude-haiku-4-5-20251001","max_tokens":1,"messages":[{"role":"user","content":"x"}]}`

	probeAPI(t, "Anthropic", "https://api.anthropic.com/v1/messages", func(r *http.Request) {
		r.Method = http.MethodPost
		r.Body = io.NopCloser(strings.NewReader(body))
		r.Header.Set("Content-Type", "application/json")
		r.Header.Set("x-api-key", apiKey)
		r.Header.Set("anthropic-version", "2023-06-01")
	})

	return apiKey
}

// probeAPI makes a lightweight HTTP request to verify provider credentials are
// functional. The setAuth callback may set headers, change the method, or
// attach a body. Skips the test if the endpoint returns 401, 403, or any
// other 4xx status (e.g., billing issues).
func probeAPI(t *testing.T, provider, url string, setAuth func(*http.Request)) {
	t.Helper()

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	// The callers pass fixed provider endpoints from this file; the parameter
	// keeps the shared probe small without accepting user-controlled URLs.
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, http.NoBody) //nolint:gosec // trusted live-test provider endpoint.
	if err != nil {
		t.Skipf("%s API probe failed to build request: %v", provider, err)
	}

	// The callback may upgrade the method to POST and attach a body.
	setAuth(req)

	resp, err := http.DefaultClient.Do(req) //nolint:gosec // request was built from trusted live-test provider endpoint.
	if err != nil {
		t.Skipf("%s API probe failed: %v", provider, err)
	}
	defer resp.Body.Close()

	switch resp.StatusCode {
	case http.StatusOK:
		return
	case http.StatusUnauthorized, http.StatusForbidden:
		t.Skipf("%s credentials are not valid (HTTP %d); skipping live test", provider, resp.StatusCode)
	default:
		if resp.StatusCode >= 400 && resp.StatusCode < 500 {
			t.Skipf("%s API rejected the request (HTTP %d); skipping live test", provider, resp.StatusCode)
		}

		t.Skipf("%s API probe returned unexpected status %d; skipping live test", provider, resp.StatusCode)
	}
}

func requireClaudeCode(t *testing.T) {
	t.Helper()

	// Probe the same credential sources the ClaudeCodeProvider uses so we skip
	// — rather than fail — on machines where the user has not logged in via
	// the claude CLI. We don't shell out to `claude` here because the provider
	// itself no longer depends on the binary being on PATH.
	if runtime.GOOS == "darwin" {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()

		err := exec.CommandContext(ctx, "security", "find-generic-password", "-s", "Claude Code-credentials").Run()
		if err == nil {
			return
		}
	}

	credPath := filepath.Join(os.Getenv("HOME"), ".claude", ".credentials.json")
	//nolint:gosec // Live test probes the user's own Claude credentials path and never opens arbitrary input.
	if _, err := os.Stat(credPath); err != nil {
		t.Skipf("Claude Code login is required for this live test: %v", err)
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

func knownModelsByProvider() map[string]map[string]bool {
	out := make(map[string]map[string]bool)

	for _, provider := range llm.KnownProviders() {
		models := make(map[string]bool, len(provider.Models))
		for _, model := range provider.Models {
			models[model] = true
		}

		out[provider.Name] = models
	}

	return out
}

func envOrDefault(name, fallback string) string {
	if value := os.Getenv(name); value != "" {
		return value
	}

	return fallback
}
