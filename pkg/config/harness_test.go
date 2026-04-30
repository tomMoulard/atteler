package config

import "testing"

const (
	testAnthropicProvider  = providerAnthropic
	testClaudeCodeProvider = providerClaudeCode
	testCodexProvider      = providerCodex
	testOpenAIProvider     = providerOpenAI
)

func TestParseCodexConfig(t *testing.T) {
	cfg := parseCodexConfig([]byte(`
model = "gpt-4.1-mini" # top-level default
model_provider = "openai"

[model_providers.openai]
base_url = "https://openai.example"

[model_providers.anthropic]
base_url = 'https://anthropic.example'
`))

	if cfg.DefaultProvider != testOpenAIProvider {
		t.Errorf("DefaultProvider = %q, want openai", cfg.DefaultProvider)
	}
	if cfg.DefaultModel != "gpt-4.1-mini" {
		t.Errorf("DefaultModel = %q, want gpt-4.1-mini", cfg.DefaultModel)
	}
	if cfg.Providers[testOpenAIProvider].BaseURL != "https://openai.example" {
		t.Errorf("openai base_url = %q", cfg.Providers[testOpenAIProvider].BaseURL)
	}
	if cfg.Providers[testAnthropicProvider].BaseURL != "https://anthropic.example" {
		t.Errorf("anthropic base_url = %q", cfg.Providers[testAnthropicProvider].BaseURL)
	}
}

func TestParseCodexConfig_DefaultsToCodexProvider(t *testing.T) {
	cfg := parseCodexConfig([]byte(`model = "gpt-5.5"`))

	if cfg.DefaultProvider != testCodexProvider {
		t.Errorf("DefaultProvider = %q, want codex", cfg.DefaultProvider)
	}
}

func TestParseGenericJSONHarness(t *testing.T) {
	cfg := parseGenericJSONHarness([]byte(`{
		"model": "claude-sonnet-4-20250514",
		"apiBase": "https://anthropic.example"
	}`), testAnthropicProvider)

	if cfg.DefaultProvider != testAnthropicProvider {
		t.Errorf("DefaultProvider = %q, want anthropic", cfg.DefaultProvider)
	}
	if cfg.DefaultModel != "claude-sonnet-4-20250514" {
		t.Errorf("DefaultModel = %q", cfg.DefaultModel)
	}
	if cfg.Providers[testAnthropicProvider].BaseURL != "https://anthropic.example" {
		t.Errorf("base_url = %q", cfg.Providers[testAnthropicProvider].BaseURL)
	}
}

func TestParseGenericJSONHarness_NestedKeys(t *testing.T) {
	cfg := parseGenericJSONHarness([]byte(`{
		"llm": {
			"provider": "openai",
			"model": "gpt-4.1"
		}
	}`), "")

	if cfg.DefaultProvider != testOpenAIProvider {
		t.Errorf("DefaultProvider = %q, want openai", cfg.DefaultProvider)
	}
	if cfg.DefaultModel != "gpt-4.1" {
		t.Errorf("DefaultModel = %q, want gpt-4.1", cfg.DefaultModel)
	}
}

func TestNormalizeProvider_ClaudeCode(t *testing.T) {
	if got := normalizeProvider("claude-code"); got != testClaudeCodeProvider {
		t.Fatalf("normalizeProvider(claude-code) = %q, want claude-code", got)
	}
}

func TestParseForgeConfig(t *testing.T) {
	cfg := parseForgeConfig([]byte(`
"$schema" = "https://forgecode.dev/schema.json"

[session]
provider_id = "claude_code"
model_id = "claude-opus-4-6"
`))

	if cfg.DefaultProvider != testClaudeCodeProvider {
		t.Errorf("DefaultProvider = %q, want claude-code", cfg.DefaultProvider)
	}
	if cfg.DefaultModel != "claude-opus-4-6" {
		t.Errorf("DefaultModel = %q, want claude-opus-4-6", cfg.DefaultModel)
	}
}
