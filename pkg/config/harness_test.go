package config

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const (
	testAnthropicProvider  = providerAnthropic
	testClaudeCodeProvider = providerClaudeCode
	testCodexProvider      = providerCodex
	testOpenAIProvider     = providerOpenAI
)

func TestParseCodexConfig(t *testing.T) {
	t.Parallel()
	cfg := parseCodexConfig([]byte(`
model = "gpt-4.1-mini" # top-level default
model_provider = "openai"

[model_providers.openai]
base_url = "https://openai.example"

[model_providers.anthropic]
base_url = 'https://anthropic.example'
`))

	if cfg.DefaultProvider != testOpenAIProvider {
		assert.Failf(t, "assertion failed", "DefaultProvider = %q, want openai", cfg.DefaultProvider)
	}
	if cfg.DefaultModel != "gpt-4.1-mini" {
		assert.Failf(t, "assertion failed", "DefaultModel = %q, want gpt-4.1-mini", cfg.DefaultModel)
	}
	if cfg.Providers[testOpenAIProvider].BaseURL != "https://openai.example" {
		assert.Failf(t, "assertion failed", "openai base_url = %q", cfg.Providers[testOpenAIProvider].BaseURL)
	}
	if cfg.Providers[testAnthropicProvider].BaseURL != "https://anthropic.example" {
		assert.Failf(t, "assertion failed", "anthropic base_url = %q", cfg.Providers[testAnthropicProvider].BaseURL)
	}
}

func TestParseCodexConfig_DefaultsToCodexProvider(t *testing.T) {
	t.Parallel()
	cfg := parseCodexConfig([]byte(`model = "gpt-5.5"`))

	if cfg.DefaultProvider != testCodexProvider {
		assert.Failf(t, "assertion failed", "DefaultProvider = %q, want codex", cfg.DefaultProvider)
	}
}

func TestParseGenericJSONHarness(t *testing.T) {
	t.Parallel()
	cfg := parseGenericJSONHarness([]byte(`{
		"model": "claude-sonnet-4-20250514",
		"apiBase": "https://anthropic.example"
	}`), testAnthropicProvider)

	if cfg.DefaultProvider != testAnthropicProvider {
		assert.Failf(t, "assertion failed", "DefaultProvider = %q, want anthropic", cfg.DefaultProvider)
	}
	if cfg.DefaultModel != "claude-sonnet-4-20250514" {
		assert.Failf(t, "assertion failed", "DefaultModel = %q", cfg.DefaultModel)
	}
	if cfg.Providers[testAnthropicProvider].BaseURL != "https://anthropic.example" {
		assert.Failf(t, "assertion failed", "base_url = %q", cfg.Providers[testAnthropicProvider].BaseURL)
	}
}

func TestParseGenericJSONHarness_NestedKeys(t *testing.T) {
	t.Parallel()
	cfg := parseGenericJSONHarness([]byte(`{
		"llm": {
			"provider": "openai",
			"model": "gpt-4.1"
		}
	}`), "")

	if cfg.DefaultProvider != testOpenAIProvider {
		assert.Failf(t, "assertion failed", "DefaultProvider = %q, want openai", cfg.DefaultProvider)
	}
	if cfg.DefaultModel != "gpt-4.1" {
		assert.Failf(t, "assertion failed", "DefaultModel = %q, want gpt-4.1", cfg.DefaultModel)
	}
}

func TestNormalizeProvider_ClaudeCode(t *testing.T) {
	t.Parallel()
	if got := normalizeProvider("claude-code"); got != testClaudeCodeProvider {
		require.Failf(t, "unexpected failure", "normalizeProvider(claude-code) = %q, want claude-code", got)
	}
}

func TestParseForgeConfig(t *testing.T) {
	t.Parallel()
	cfg := parseForgeConfig([]byte(`
"$schema" = "https://forgecode.dev/schema.json"

[session]
provider_id = "claude_code"
model_id = "claude-opus-4-6"
`))

	if cfg.DefaultProvider != testClaudeCodeProvider {
		assert.Failf(t, "assertion failed", "DefaultProvider = %q, want claude-code", cfg.DefaultProvider)
	}
	if cfg.DefaultModel != "claude-opus-4-6" {
		assert.Failf(t, "assertion failed", "DefaultModel = %q, want claude-opus-4-6", cfg.DefaultModel)
	}
}
