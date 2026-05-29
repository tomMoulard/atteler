package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const (
	testAnthropicProvider  = providerAnthropic
	testClaudeCodeProvider = providerClaudeCode
	testCodexProvider      = providerCodex
	testOpenAIProvider     = providerOpenAI
	testAnthropicBaseURL   = "https://anthropic.example"
	testOpenAIGPT54        = "openai/gpt-5.4"
)

func TestParseCodexConfig(t *testing.T) {
	t.Parallel()

	cfg := parseCodexConfig([]byte(`
model = "gpt-4.1-mini" # top-level default
model_provider = "openai"

[model_providers.openai]
base_url = "https://openai.example"

[model_providers.anthropic]
base_url = '` + testAnthropicBaseURL + `'
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

	if cfg.Providers[testAnthropicProvider].BaseURL != testAnthropicBaseURL {
		assert.Failf(t, "assertion failed", "anthropic base_url = %q", cfg.Providers[testAnthropicProvider].BaseURL)
	}
}

func TestParseCodexConfigWithDiagnostics_ParsesRealTOMLAndWarnsForUnsupportedProfiles(t *testing.T) {
	t.Parallel()

	cfg, diagnostics := parseCodexConfigWithDiagnostics("codex-realistic.toml", readHarnessFixture(t, "codex-realistic.toml"))

	assert.Equal(t, testOpenAIProvider, cfg.DefaultProvider)
	assert.Equal(t, "gpt-5.5", cfg.DefaultModel)
	assert.Equal(t, "https://openai.example/v1", cfg.Providers[testOpenAIProvider].BaseURL)
	assert.Equal(t, testAnthropicBaseURL, cfg.Providers[testAnthropicProvider].BaseURL)
	assertDiagnosticContains(t, diagnostics, "profile-specific")
	assertDiagnosticContains(t, diagnostics, "active Codex profile")
	assertDiagnosticContains(t, diagnostics, "trusted_project_roots: ignored unsupported field")
	assertDiagnosticContains(t, diagnostics, "model_providers.openai.wire_api: ignored unsupported field")
}

func TestParseCodexConfigWithDiagnostics_ParsesDottedInlineTables(t *testing.T) {
	t.Parallel()

	cfg, diagnostics := parseCodexConfigWithDiagnostics("codex-dotted.toml", []byte(`
model = "gpt-5.5"
model_provider = "openai"
model_providers.openai = { base_url = "https://openai.example/v1" }
`))

	require.Empty(t, diagnostics)
	assert.Equal(t, testOpenAIProvider, cfg.DefaultProvider)
	assert.Equal(t, "gpt-5.5", cfg.DefaultModel)
	assert.Equal(t, "https://openai.example/v1", cfg.Providers[testOpenAIProvider].BaseURL)
}

func TestParseCodexConfigWithDiagnostics_ImportsPriorityServiceTierAsFastMode(t *testing.T) {
	t.Parallel()

	cfg, diagnostics := parseCodexConfigWithDiagnostics("codex-service-tier.toml", []byte(`
model = "gpt-5.5"
model_provider = "openai"
service_tier = "priority"
`))

	require.Empty(t, diagnostics)
	assert.Equal(t, "fast", cfg.Generation.ModelMode)
}

func TestParseCodexConfigWithDiagnostics_IgnoresDefaultServiceTier(t *testing.T) {
	t.Parallel()

	cfg, diagnostics := parseCodexConfigWithDiagnostics("codex-service-tier.toml", []byte(`
model = "gpt-5.5"
service_tier = "auto"
`))

	require.Empty(t, diagnostics)
	assert.Empty(t, cfg.Generation.ModelMode)
}

func TestParseCodexConfigWithDiagnostics_WarnsForUnsupportedServiceTier(t *testing.T) {
	t.Parallel()

	cfg, diagnostics := parseCodexConfigWithDiagnostics("codex-service-tier.toml", []byte(`
model = "gpt-5.5"
service_tier = "flex"
`))

	assert.Empty(t, cfg.Generation.ModelMode)
	assertDiagnosticContains(t, diagnostics, `service_tier: ignored value: unsupported service tier "flex"`)
}

func TestParseCodexConfigWithDiagnostics_WarnsForUnsupportedTypes(t *testing.T) {
	t.Parallel()

	cfg, diagnostics := parseCodexConfigWithDiagnostics("codex-malformed.toml", readHarnessFixture(t, "codex-malformed.toml"))

	assert.Equal(t, testOpenAIProvider, cfg.DefaultProvider)
	assert.Empty(t, cfg.DefaultModel)
	assert.Empty(t, cfg.Providers[testOpenAIProvider].BaseURL)
	assertDiagnosticContains(t, diagnostics, "model: ignored value: expected string, got array")
	assertDiagnosticContains(t, diagnostics, "model_providers.openai.base_url: ignored value: expected string, got array")
	assertDiagnosticContains(t, diagnostics, "model_providers.openai.unknown_provider_knob: ignored unsupported field")
}

func TestParseCodexConfigWithDiagnostics_DoesNotDefaultProviderWithoutImportableDefaults(t *testing.T) {
	t.Parallel()

	cfg, diagnostics := parseCodexConfigWithDiagnostics("codex-unsupported-only.toml", []byte(`
trusted_project_roots = ["/repo"]
`))

	assert.True(t, cfg.empty())
	assertDiagnosticContains(t, diagnostics, "trusted_project_roots: ignored unsupported field")
}

func TestParseCodexConfigWithDiagnostics_WarnsForInvalidTOMLSyntax(t *testing.T) {
	t.Parallel()

	cfg, diagnostics := parseCodexConfigWithDiagnostics("codex-invalid.toml", []byte(`
model = "gpt-5.5"
[model_providers.openai
base_url = "https://openai.example"
`))

	assert.True(t, cfg.empty())
	assertDiagnosticContains(t, diagnostics, "codex-invalid.toml: ignored harness config: parse TOML")
}

func TestParseCodexConfig_DefaultsToCodexProvider(t *testing.T) {
	t.Parallel()

	cfg := parseCodexConfig([]byte(`model = "gpt-5.5"`))

	if cfg.DefaultProvider != testCodexProvider {
		assert.Failf(t, "assertion failed", "DefaultProvider = %q, want codex", cfg.DefaultProvider)
	}
}

func TestLoadHarnessDefaultsWithOrigins_RecordsHarnessImport(t *testing.T) {
	tempDir := t.TempDir()
	codexHome := filepath.Join(tempDir, "codex")
	require.NoError(t, os.MkdirAll(codexHome, 0o700))
	codexConfig := filepath.Join(codexHome, "config.toml")
	require.NoError(t, os.WriteFile(codexConfig, []byte(`
model = "gpt-5.5"
model_provider = "codex"
`), 0o600))

	t.Setenv("HOME", tempDir)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(tempDir, "config"))
	t.Setenv("CODEX_HOME", codexHome)
	t.Setenv("OPENCODE_CONFIG", "")
	t.Setenv("OPENCODE_CONFIG_DIR", "")
	t.Setenv("FORGE_CONFIG", "")

	cfg, loaded, origins := LoadHarnessDefaultsWithOrigins()

	require.Equal(t, []string{codexConfig}, loaded)
	assert.Equal(t, "gpt-5.5", cfg.DefaultModel)

	final, ok := origins.Final("default_model")
	require.True(t, ok)
	assert.Equal(t, OriginHarnessImport, final.Kind)
	assert.Equal(t, codexConfig, final.Source)
}

func TestParseGenericJSONHarness(t *testing.T) {
	t.Parallel()

	cfg := parseGenericJSONHarness([]byte(`{
		"model": "claude-sonnet-4-20250514",
		"apiBase": "`+testAnthropicBaseURL+`"
	}`), testAnthropicProvider)

	if cfg.DefaultProvider != testAnthropicProvider {
		assert.Failf(t, "assertion failed", "DefaultProvider = %q, want anthropic", cfg.DefaultProvider)
	}

	if cfg.DefaultModel != "claude-sonnet-4-20250514" {
		assert.Failf(t, "assertion failed", "DefaultModel = %q", cfg.DefaultModel)
	}

	if cfg.Providers[testAnthropicProvider].BaseURL != testAnthropicBaseURL {
		assert.Failf(t, "assertion failed", "base_url = %q", cfg.Providers[testAnthropicProvider].BaseURL)
	}
}

func TestParseClaudeConfigWithDiagnostics_UsesSchemaAndWarnsUnknownFields(t *testing.T) {
	t.Parallel()

	cfg, diagnostics := parseClaudeConfigWithDiagnostics("claude-settings.json", readHarnessFixture(t, "claude-settings.json"))

	assert.Equal(t, testClaudeCodeProvider, cfg.DefaultProvider)
	assert.Equal(t, "claude-sonnet-4-20250514", cfg.DefaultModel)
	assert.Equal(t, testAnthropicBaseURL, cfg.Providers[testClaudeCodeProvider].BaseURL)
	assertDiagnosticContains(t, diagnostics, "permissions: ignored unsupported field")
	assertDiagnosticContains(t, diagnostics, "unknownClaudeKnob: ignored unsupported field")
}

func TestParseClaudeConfigWithDiagnostics_WarnsForMalformedData(t *testing.T) {
	t.Parallel()

	cfg, diagnostics := parseClaudeConfigWithDiagnostics("claude-malformed.json", readHarnessFixture(t, "claude-malformed.json"))

	assert.True(t, cfg.empty())
	assertDiagnosticContains(t, diagnostics, "unsupportedTop: ignored unsupported field")
	assertDiagnosticContains(t, diagnostics, "llm.unsupportedLLMKnob: ignored unsupported field")
	assertDiagnosticContains(t, diagnostics, "model: ignored value: expected string, got array")
	assertDiagnosticContains(t, diagnostics, "llm.model: ignored value: expected string, got object")
	assertDiagnosticContains(t, diagnostics, "llm.provider: ignored value: expected string, got number")
	assertDiagnosticContains(t, diagnostics, "apiBase: ignored value: expected string, got array")
	assertDiagnosticContains(t, diagnostics, "llm.base_url: ignored value: expected string, got array")
}

func TestParseClaudeConfigWithDiagnostics_WarnsForInvalidJSONSyntax(t *testing.T) {
	t.Parallel()

	cfg, diagnostics := parseClaudeConfigWithDiagnostics("claude-invalid.json", []byte(`{
		"model": "claude-sonnet-4-20250514",
		"llm": {
	`))

	assert.True(t, cfg.empty())
	assertDiagnosticContains(t, diagnostics, "claude-invalid.json: ignored harness config: parse JSON/JSONC")
}

func TestParseClaudeConfigWithDiagnostics_DoesNotDefaultProviderWithoutImportableDefaults(t *testing.T) {
	t.Parallel()

	cfg, diagnostics := parseClaudeConfigWithDiagnostics("claude-unsupported-only.json", []byte(`{
		"permissions": {
			"allow": ["Bash(go test ./...)"]
		}
	}`))

	assert.True(t, cfg.empty())
	assertDiagnosticContains(t, diagnostics, "permissions: ignored unsupported field")
}

func TestParseClaudeConfigWithDiagnostics_ImportsBaseURLWithoutDefaultProvider(t *testing.T) {
	t.Parallel()

	cfg, diagnostics := parseClaudeConfigWithDiagnostics("claude-base-only.json", []byte(`{
		"apiBase": "`+testAnthropicBaseURL+`"
	}`))

	require.Empty(t, diagnostics)
	assert.Empty(t, cfg.DefaultProvider)
	assert.Empty(t, cfg.DefaultModel)
	assert.Equal(t, testAnthropicBaseURL, cfg.Providers[testClaudeCodeProvider].BaseURL)
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

func TestLoadHarnessDefaultsWithDiagnostics_UnsupportedClaudeDoesNotOverrideCodexDefaults(t *testing.T) {
	tempDir := t.TempDir()
	codexHome := filepath.Join(tempDir, "codex")
	require.NoError(t, os.MkdirAll(codexHome, 0o700))
	codexConfig := filepath.Join(codexHome, "config.toml")
	require.NoError(t, os.WriteFile(codexConfig, []byte(`
model = "gpt-5.5"
model_provider = "codex"
`), 0o600))

	claudeDir := filepath.Join(tempDir, ".claude")
	require.NoError(t, os.MkdirAll(claudeDir, 0o700))
	claudeConfig := filepath.Join(claudeDir, "settings.json")
	require.NoError(t, os.WriteFile(claudeConfig, []byte(`{
		"permissions": {
			"allow": ["Bash(go test ./...)"]
		}
	}`), 0o600))

	t.Setenv("HOME", tempDir)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(tempDir, "config"))
	t.Setenv("CODEX_HOME", codexHome)
	t.Setenv("OPENCODE_CONFIG", "")
	t.Setenv("OPENCODE_CONFIG_DIR", "")
	t.Setenv("FORGE_CONFIG", filepath.Join(tempDir, "missing-forge"))
	t.Chdir(tempDir)

	cfg, loaded, origins, diagnostics := LoadHarnessDefaultsWithDiagnostics()

	assert.Equal(t, testCodexProvider, cfg.DefaultProvider)
	assert.Equal(t, "gpt-5.5", cfg.DefaultModel)
	assert.Contains(t, loaded, codexConfig)
	assert.NotContains(t, loaded, claudeConfig)
	assertDiagnosticContains(t, diagnostics, "permissions: ignored unsupported field")

	providerOrigin, ok := origins.Final("default_provider")
	require.True(t, ok)
	assert.Equal(t, codexConfig, providerOrigin.Source)
	assert.Equal(t, testCodexProvider, providerOrigin.Value)
}

func TestParseOpencodeConfig_UsesTopLevelModelProviderConfigAndAgents(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	promptPath := filepath.Join(dir, "review.txt")
	require.NoError(t, os.WriteFile(promptPath, []byte("Review code carefully."), 0o600))

	cfg := parseOpencodeConfig(filepath.Join(dir, "opencode.json"), []byte(`{
		"$schema": "https://opencode.ai/config.json",
		// OpenCode supports JSONC.
		"model": "`+testOpenAIGPT54+`",
		"agent": {
			"reviewer": {
				"description": "Reviews code",
				"model": "anthropic/claude-sonnet-4-20250514",
				"prompt": "{file:./review.txt}",
				"temperature": 0.1,
				"topP": 0.8,
			},
			"hidden-helper": {
				"prompt": "Internal helper",
				"hidden": true,
			},
		},
		"provider": {
			"openai": {
				"baseURL": "https://openai.example",
			},
			"anthropic": {
				"base_url": "`+testAnthropicBaseURL+`",
			},
		},
	}`))

	if cfg.DefaultProvider != testOpenAIProvider {
		assert.Failf(t, "assertion failed", "DefaultProvider = %q, want openai", cfg.DefaultProvider)
	}

	if cfg.DefaultModel != testOpenAIGPT54 {
		assert.Failf(t, "assertion failed", "DefaultModel = %q, want openai/gpt-5.4", cfg.DefaultModel)
	}

	if cfg.Providers[testOpenAIProvider].BaseURL != "https://openai.example" {
		assert.Failf(t, "assertion failed", "openai base_url = %q", cfg.Providers[testOpenAIProvider].BaseURL)
	}

	if cfg.Providers[testAnthropicProvider].BaseURL != testAnthropicBaseURL {
		assert.Failf(t, "assertion failed", "anthropic base_url = %q", cfg.Providers[testAnthropicProvider].BaseURL)
	}

	if reviewer := cfg.Agents["reviewer"]; reviewer.Description != "Reviews code" ||
		reviewer.Model != "anthropic/claude-sonnet-4-20250514" ||
		reviewer.SystemPrompt != "Review code carefully." {
		assert.Failf(t, "assertion failed", "reviewer = %+v", reviewer)
	}

	if reviewer := cfg.Agents["reviewer"]; reviewer.Temperature == nil || *reviewer.Temperature != 0.1 {
		assert.Failf(t, "assertion failed", "reviewer temperature = %v", reviewer.Temperature)
	}

	if reviewer := cfg.Agents["reviewer"]; reviewer.TopP == nil || *reviewer.TopP != 0.8 {
		assert.Failf(t, "assertion failed", "reviewer top_p = %v", reviewer.TopP)
	}

	if !cfg.Agents["hidden-helper"].Hidden {
		assert.FailNow(t, "expected hidden-helper to be hidden")
	}
}

func TestParseOpencodeConfigWithDiagnostics_ParsesJSONCAndWarnsBoundedFallbackSections(t *testing.T) {
	t.Parallel()

	cfg, diagnostics := parseOpencodeConfigWithDiagnostics("opencode-realistic.jsonc", readHarnessFixture(t, "opencode-realistic.jsonc"))

	assert.Equal(t, testOpenAIProvider, cfg.DefaultProvider)
	assert.Equal(t, testOpenAIGPT54, cfg.DefaultModel)
	assert.Equal(t, "https://openai.example/v1", cfg.Providers[testOpenAIProvider].BaseURL)
	assert.Equal(t, testAnthropicBaseURL, cfg.Providers[testAnthropicProvider].BaseURL)
	assert.False(t, cfg.Agents["reviewer"].Hidden)
	assert.True(t, cfg.Agents["hidden-helper"].Hidden)
	assertDiagnosticContains(t, diagnostics, "permission: ignored unsupported field")
	assertDiagnosticContains(t, diagnostics, "agent.reviewer.permission: ignored unsupported field")
	assertDiagnosticContains(t, diagnostics, "provider.openai.models: ignored unsupported field")
	assertDiagnosticContains(t, diagnostics, "categories section is used only as a default-model fallback")
}

func TestParseOpencodeConfigWithDiagnostics_WarnsForMalformedData(t *testing.T) {
	t.Parallel()

	cfg, diagnostics := parseOpencodeConfigWithDiagnostics("opencode-malformed.jsonc", readHarnessFixture(t, "opencode-malformed.jsonc"))

	assert.Empty(t, cfg.DefaultModel)
	assert.Equal(t, "Bad agent", cfg.Agents["bad"].SystemPrompt)
	assertDiagnosticContains(t, diagnostics, "model: ignored value: expected string, got array")
	assertDiagnosticContains(t, diagnostics, "provider: ignored section: expected object/table, got array")
	assertDiagnosticContains(t, diagnostics, "agent.bad.tools.read: ignored tool permission: expected boolean, got string")
	assertDiagnosticContains(t, diagnostics, "agent.bad.hidden: ignored value: expected boolean, got string")
}

func TestParseOpencodeConfigWithDiagnostics_WarnsOnActualAliasPath(t *testing.T) {
	t.Parallel()

	_, diagnostics := parseOpencodeConfigWithDiagnostics("opencode-alias-errors.jsonc", []byte(`{
		"agent": {
			"bad": {
				"prompt": "Bad agent",
				"topP": "high",
				"maxTokens": "many"
			}
		}
	}`))

	assertDiagnosticContains(t, diagnostics, "agent.bad.topP: ignored value: expected number, got string")
	assertDiagnosticContains(t, diagnostics, "agent.bad.maxTokens: ignored value: expected integer, got string")
}

func TestParseOpencodeConfigWithDiagnostics_WarnsForNonIntegerMaxTokens(t *testing.T) {
	t.Parallel()

	cfg, diagnostics := parseOpencodeConfigWithDiagnostics("opencode-non-integer-max-tokens.jsonc", []byte(`{
		"agent": {
			"bad": {
				"prompt": "Bad agent",
				"maxTokens": 12.5
			}
		}
	}`))

	assert.Zero(t, cfg.Agents["bad"].MaxTokens)
	assertDiagnosticContains(t, diagnostics, "agent.bad.maxTokens: ignored value: expected integer, got number")
}

func TestParseOpencodeConfigWithDiagnostics_WarnsForNegativeMaxTokens(t *testing.T) {
	t.Parallel()

	cfg, diagnostics := parseOpencodeConfigWithDiagnostics("opencode-negative-max-tokens.jsonc", []byte(`{
		"agent": {
			"bad": {
				"prompt": "Bad agent",
				"maxTokens": -1
			}
		}
	}`))

	assert.Zero(t, cfg.Agents["bad"].MaxTokens)
	assertDiagnosticContains(t, diagnostics, "agent.bad.maxTokens: ignored value: expected non-negative integer, got -1")
}

func TestParseOpencodeConfigWithDiagnostics_ImportsModeAndToolsOnlyAgent(t *testing.T) {
	t.Parallel()

	cfg, diagnostics := parseOpencodeConfigWithDiagnostics("opencode-mode-tools-only.jsonc", []byte(`{
		"agent": {
			"planner": {
				"mode": "subagent",
				"tools": {
					"read": true,
					"write": false
				}
			}
		}
	}`))

	require.Empty(t, diagnostics)

	planner, ok := cfg.Agents["planner"]
	require.True(t, ok)
	assert.Equal(t, "subagent", planner.Mode)
	assert.Equal(t, map[string]bool{"read": true, "write": false}, planner.ToolPermissions)
}

func TestParseOpencodeConfigWithDiagnostics_DropsMalformedToolsOnlyAgent(t *testing.T) {
	t.Parallel()

	cfg, diagnostics := parseOpencodeConfigWithDiagnostics("opencode-malformed-tools-only.jsonc", []byte(`{
		"agent": {
			"bad": {
				"tools": {
					"read": "yes"
				}
			}
		}
	}`))

	assert.NotContains(t, cfg.Agents, "bad")
	assertDiagnosticContains(t, diagnostics, "agent.bad.tools.read: ignored tool permission: expected boolean, got string")
}

func TestParseOpencodeConfigWithDiagnostics_WarnsForDuplicateJSONFields(t *testing.T) {
	t.Parallel()

	cfg, diagnostics := parseOpencodeConfigWithDiagnostics("opencode-duplicate.jsonc", []byte(`{
		"model": "openai/gpt-first",
		"model": "openai/gpt-second",
		"provider": {
			"openai": {
				"baseURL": "https://first.example",
				"baseURL": "https://second.example"
			}
		}
	}`))

	assert.Equal(t, "openai/gpt-second", cfg.DefaultModel)
	assert.Equal(t, "https://second.example", cfg.Providers[testOpenAIProvider].BaseURL)
	assertDiagnosticContains(t, diagnostics, "model: duplicate object field")
	assertDiagnosticContains(t, diagnostics, "provider.openai.baseURL: duplicate object field")
}

func TestParseOpencodeConfigWithDiagnostics_WarnsForInvalidJSONCSyntax(t *testing.T) {
	t.Parallel()

	cfg, diagnostics := parseOpencodeConfigWithDiagnostics("opencode-invalid.jsonc", []byte(`{
		"model": "openai/gpt-5.4",
		"agent": {
	`))

	assert.True(t, cfg.empty())
	assertDiagnosticContains(t, diagnostics, "opencode-invalid.jsonc: ignored harness config: parse JSON/JSONC")
}

func TestParseOpencodeConfigWithDiagnostics_WarnsForNonObjectRoot(t *testing.T) {
	t.Parallel()

	cfg, diagnostics := parseOpencodeConfigWithDiagnostics("opencode-array.jsonc", []byte(`[
		{"model": "`+testOpenAIGPT54+`"},
	]`))

	assert.True(t, cfg.empty())
	assertDiagnosticContains(t, diagnostics, "ignored harness config: decode JSON object")
}

func TestParseOpencodeConfig_SkipsUnsafeOrMissingPromptReferences(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	outside := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(outside, "secret.txt"), []byte("secret"), 0o600))
	require.NoError(t, os.Symlink(outside, filepath.Join(dir, "link")))

	cfg := parseOpencodeConfig(filepath.Join(dir, "opencode.json"), []byte(`{
		"agent": {
			"escape": {
				"prompt": "{file:../secret.txt}"
			},
			"symlink": {
				"prompt": "{file:./link/secret.txt}"
			},
			"missing": {
				"prompt": "{file:./missing.txt}"
			},
			"inline": {
				"prompt": "Use inline prompt."
			}
		}
	}`))

	assert.NotContains(t, cfg.Agents, "escape")
	assert.NotContains(t, cfg.Agents, "symlink")
	assert.NotContains(t, cfg.Agents, "missing")
	assert.Equal(t, "Use inline prompt.", cfg.Agents["inline"].SystemPrompt)
}

func TestParseOpenCodeAgentFile_SkipsUnsafePromptReference(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	path := filepath.Join(dir, "agent.md")
	require.NoError(t, os.WriteFile(path, []byte(`---
prompt: "{file:../secret.txt}"
---
`), 0o600))

	_, ok := parseOpenCodeAgentFile(path)

	assert.False(t, ok)
}

func TestParseOpencodeConfig_FallsBackToOhMyOpenagentDefaults(t *testing.T) {
	t.Parallel()

	cfg := parseOpencodeConfig("opencode.json", []byte(`{
		"agents": {
			"sisyphus": {
				"model": "anthropic/claude-opus-4-6"
			}
		},
		"categories": {
			"deep": {
				"model": "`+testOpenAIGPT54+`"
			},
			"quick": {
				"model": "openai/gpt-5.4-mini"
			}
		}
	}`))

	if cfg.DefaultProvider != testOpenAIProvider {
		assert.Failf(t, "assertion failed", "DefaultProvider = %q, want openai", cfg.DefaultProvider)
	}

	if cfg.DefaultModel != testOpenAIGPT54 {
		assert.Failf(t, "assertion failed", "DefaultModel = %q, want openai/gpt-5.4", cfg.DefaultModel)
	}
}

func TestParseOpencodeConfig_FallbackModelSelectionIsDeterministic(t *testing.T) {
	t.Parallel()

	cfg := parseOpencodeConfig("opencode.json", []byte(`{
		"categories": {
			"zeta": {
				"model": "openai/gpt-zeta"
			},
			"alpha": {
				"model": "openai/gpt-alpha"
			}
		}
	}`))

	assert.Equal(t, "openai/gpt-alpha", cfg.DefaultModel)
	assert.Equal(t, testOpenAIProvider, cfg.DefaultProvider)
}

func TestParseOpencodeConfig_FallbackModelSelectionRecursesDeterministically(t *testing.T) {
	t.Parallel()

	cfg := parseOpencodeConfig("opencode.json", []byte(`{
		"categories": {
			"deep": {
				"zeta": {
					"model": "openai/gpt-zeta"
				},
				"alpha": {
					"model": "openai/gpt-alpha"
				}
			}
		}
	}`))

	assert.Equal(t, "openai/gpt-alpha", cfg.DefaultModel)
	assert.Equal(t, testOpenAIProvider, cfg.DefaultProvider)
}

func TestParseOpencodeConfigWithDiagnostics_WarnsForMalformedFallbackModelSections(t *testing.T) {
	t.Parallel()

	cfg, diagnostics := parseOpencodeConfigWithDiagnostics("opencode-fallback-malformed.jsonc", []byte(`{
		"categories": {
			"deep": {
				"model": ["openai/gpt-array"]
			},
			"quick": "openai/gpt-string"
		},
		"agents": {
			"oracle": {
				"model": ["openai/gpt-array"]
			},
			"atlas": {
				"model": "openai/gpt-atlas"
			}
		}
	}`))

	assert.Equal(t, "openai/gpt-atlas", cfg.DefaultModel)
	assertDiagnosticContains(t, diagnostics, "categories.deep.model: ignored value: expected string, got array")
	assertDiagnosticContains(t, diagnostics, "categories.quick: ignored default-model fallback entry: expected object, got string")
	assertDiagnosticContains(t, diagnostics, "agents.oracle.model: ignored value: expected string, got array")
	assert.Equal(t, 1, countDiagnosticsContaining(diagnostics, "categories.deep.model: ignored value"))
	assert.Equal(t, 1, countDiagnosticsContaining(diagnostics, "categories.quick: ignored default-model fallback entry"))
}

func TestLoadOpencodeAgentDir_LoadsMarkdownAgents(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(dir, "README.txt"), []byte("not an agent"), 0o600))
	require.NoError(t, os.WriteFile(filepath.Join(dir, ".hidden.md"), []byte("not an agent"), 0o600))
	outsideAgent := filepath.Join(t.TempDir(), "outside.md")
	require.NoError(t, os.WriteFile(outsideAgent, []byte("not an agent"), 0o600))
	require.NoError(t, os.Symlink(outsideAgent, filepath.Join(dir, "linked.md")))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "review.md"), []byte(`---
description: Reviews code
model: `+testOpenAIGPT54+`
temperature: 0.1
hidden: false
---
Review code thoroughly.
`), 0o600))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "internal.md"), []byte(`---
hidden: true
---
Internal helper prompt.
`), 0o600))

	cfg, loaded, ok := loadOpencodeAgentDir(dir)
	if !ok {
		require.FailNow(t, "expected markdown agents to load")
	}

	assert.Contains(t, loaded, filepath.Join(dir, "review.md"))

	if reviewer := cfg.Agents["review"]; reviewer.SystemPrompt != "Review code thoroughly." ||
		reviewer.Description != "Reviews code" ||
		reviewer.Model != testOpenAIGPT54 {
		assert.Failf(t, "assertion failed", "review agent = %+v", reviewer)
	}

	if !cfg.Agents["internal"].Hidden {
		assert.FailNow(t, "expected internal agent to be hidden")
	}

	assert.NotContains(t, cfg.Agents, "README")
	assert.NotContains(t, cfg.Agents, ".hidden")
	assert.NotContains(t, cfg.Agents, "linked")
}

func TestLoadHarnessDefaultsWithDiagnostics_RecordsPerImportedSourceOrigins(t *testing.T) {
	tempDir := t.TempDir()
	t.Setenv("HOME", tempDir)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(tempDir, "config"))
	t.Setenv("CODEX_HOME", filepath.Join(tempDir, "missing-codex"))
	t.Setenv("OPENCODE_CONFIG", "")
	t.Setenv("OPENCODE_CONFIG_DIR", "")
	t.Setenv("FORGE_CONFIG", filepath.Join(tempDir, "missing-forge"))
	t.Chdir(tempDir)

	opencodePath := filepath.Join(tempDir, "opencode.json")
	require.NoError(t, os.WriteFile(opencodePath, []byte(`{
		"model": "`+testOpenAIGPT54+`",
		"provider": {
			"openai": {
				"baseURL": "https://openai.example"
			}
		}
	}`), 0o600))

	agentDir := filepath.Join(tempDir, ".opencode", "agents")
	require.NoError(t, os.MkdirAll(agentDir, 0o700))
	agentPath := filepath.Join(agentDir, "reviewer.md")
	require.NoError(t, os.WriteFile(agentPath, []byte(`---
model: anthropic/claude-sonnet-4-20250514
---
Review carefully.
`), 0o600))

	cfg, loaded, origins, diagnostics := LoadHarnessDefaultsWithDiagnostics()

	require.Empty(t, diagnostics)
	assert.Equal(t, testOpenAIGPT54, cfg.DefaultModel)
	assert.Equal(t, "anthropic/claude-sonnet-4-20250514", cfg.Agents["reviewer"].Model)
	assert.Contains(t, loaded, opencodePath)
	assert.Contains(t, loaded, agentPath)

	modelOrigin, ok := origins.Final("default_model")
	require.True(t, ok)
	assert.Equal(t, OriginHarnessImport, modelOrigin.Kind)
	assert.Equal(t, opencodePath, modelOrigin.Source)

	providerOrigin, ok := origins.Final("providers.openai.base_url")
	require.True(t, ok)
	assert.Equal(t, OriginHarnessImport, providerOrigin.Kind)
	assert.Equal(t, opencodePath, providerOrigin.Source)
	assert.Equal(t, "https://openai.example", providerOrigin.Value)

	_, ok = origins.Final("providers.openai.disabled")
	assert.False(t, ok, "omitted provider disabled flag should not be reported as an imported value")

	_, ok = origins.Final("providers.openai.disable_private_adapter")
	assert.False(t, ok, "omitted private-adapter flag should not be reported as an imported value")

	agentOrigin, ok := origins.Final("agents.reviewer.model")
	require.True(t, ok)
	assert.Equal(t, OriginHarnessImport, agentOrigin.Kind)
	assert.Equal(t, agentPath, agentOrigin.Source)
}

func TestLoadHarnessDefaultsWithDiagnostics_RecordsOpenCodeAgentFileOriginsBeforeMerge(t *testing.T) {
	tempDir := t.TempDir()
	t.Setenv("HOME", tempDir)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(tempDir, "config"))
	t.Setenv("CODEX_HOME", filepath.Join(tempDir, "missing-codex"))
	t.Setenv("OPENCODE_CONFIG", "")
	t.Setenv("OPENCODE_CONFIG_DIR", "")
	t.Setenv("FORGE_CONFIG", filepath.Join(tempDir, "missing-forge"))
	t.Chdir(tempDir)

	agentDir := filepath.Join(tempDir, ".opencode", "agents")
	require.NoError(t, os.MkdirAll(agentDir, 0o700))
	markdownPath := filepath.Join(agentDir, "review.markdown")
	require.NoError(t, os.WriteFile(markdownPath, []byte(`---
model: openai/from-markdown
---
Markdown agent.
`), 0o600))

	mdPath := filepath.Join(agentDir, "review.md")
	require.NoError(t, os.WriteFile(mdPath, []byte(`---
model: openai/from-md
---
MD agent.
`), 0o600))

	cfg, loaded, origins, diagnostics := LoadHarnessDefaultsWithDiagnostics()

	require.Empty(t, diagnostics)
	assert.Equal(t, "openai/from-md", cfg.Agents["review"].Model)
	assert.Contains(t, loaded, markdownPath)
	assert.Contains(t, loaded, mdPath)

	origin := origins["agents.review.model"]
	require.Len(t, origin.Chain, 2)
	assert.Equal(t, markdownPath, origin.Chain[0].Source)
	assert.Equal(t, "openai/from-markdown", origin.Chain[0].Value)
	assert.Equal(t, mdPath, origin.Chain[1].Source)
	assert.Equal(t, "openai/from-md", origin.Chain[1].Value)
}

func TestLoadHarnessDefaultsWithDiagnostics_WarnsForUnreadableHarnessSource(t *testing.T) {
	tempDir := t.TempDir()
	codexHome := filepath.Join(tempDir, "codex")
	codexConfig := filepath.Join(codexHome, "config.toml")
	require.NoError(t, os.MkdirAll(codexConfig, 0o700))

	t.Setenv("HOME", tempDir)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(tempDir, "config"))
	t.Setenv("CODEX_HOME", codexHome)
	t.Setenv("OPENCODE_CONFIG", "")
	t.Setenv("OPENCODE_CONFIG_DIR", "")
	t.Setenv("FORGE_CONFIG", filepath.Join(tempDir, "missing-forge"))
	t.Chdir(tempDir)

	cfg, loaded, origins, diagnostics := LoadHarnessDefaultsWithDiagnostics()

	assert.True(t, cfg.empty())
	assert.Empty(t, loaded)
	assert.Empty(t, origins)
	assertDiagnosticContains(t, diagnostics, codexConfig+": ignored harness config: read file")
}

func TestLoadOpencodeAgentDir_ParsesModeAndToolPermissions(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(dir, "cr.md"), []byte(`---
mode: subagent
description: Code review agent
tools:
  write: true
  edit: true
  bash: true
  read: true
  grep: true
  glob: true
---
Run coderabbit review.
`), 0o600))

	cfg, _, ok := loadOpencodeAgentDir(dir)
	require.True(t, ok)

	cr, exists := cfg.Agents["cr"]
	require.True(t, exists, "cr agent should exist")
	assert.Equal(t, "subagent", cr.Mode)
	assert.Equal(t, "Run coderabbit review.", cr.SystemPrompt)
	assert.Equal(t, "Code review agent", cr.Description)

	require.NotNil(t, cr.ToolPermissions)
	assert.True(t, cr.ToolPermissions["bash"])
	assert.True(t, cr.ToolPermissions["write"])
	assert.True(t, cr.ToolPermissions["edit"])
	assert.True(t, cr.ToolPermissions["read"])
	assert.True(t, cr.ToolPermissions["grep"])
	assert.True(t, cr.ToolPermissions["glob"])
}

func TestLoadOpencodeAgentDir_NilToolPermissionsWhenOmitted(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(dir, "basic.md"), []byte(`---
description: Basic agent
---
Do basic things.
`), 0o600))

	cfg, _, ok := loadOpencodeAgentDir(dir)
	require.True(t, ok)

	basic, exists := cfg.Agents["basic"]
	require.True(t, exists)
	assert.Nil(t, basic.ToolPermissions, "should be nil when not specified in frontmatter")
	assert.Empty(t, basic.Mode)
}

func TestParseOpenCodeAgentFile_EmptyFrontmatterUsesBodyAsPrompt(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), "agent.md")
	require.NoError(t, os.WriteFile(path, []byte(`---
---
Use the body only.
`), 0o600))

	cfg, diagnostics, ok := parseOpenCodeAgentFileWithDiagnostics(path)
	require.True(t, ok)
	require.Empty(t, diagnostics)
	assert.Equal(t, "Use the body only.", cfg.SystemPrompt)
}

func TestLoadOpencodeAgentDirWithDiagnostics_WarnsForUnreadableAgentDirectory(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), "agents")
	require.NoError(t, os.WriteFile(path, []byte("not a directory"), 0o600))

	cfg, loaded, diagnostics, ok := loadOpencodeAgentDirWithDiagnostics(path)

	assert.False(t, ok)
	assert.True(t, cfg.empty())
	assert.Empty(t, loaded)
	assertDiagnosticContains(t, diagnostics, path+": ignored agent directory: read directory")
}

func TestParseOpenCodeAgentFile_ParsesCamelCaseFrontmatterAliases(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), "agent.md")
	require.NoError(t, os.WriteFile(path, []byte(`---
topP: 0.7
maxTokens: 4096
---
Use aliases.
`), 0o600))

	cfg, diagnostics, ok := parseOpenCodeAgentFileWithDiagnostics(path)
	require.True(t, ok)
	require.Empty(t, diagnostics)
	assert.Equal(t, 4096, cfg.MaxTokens)
	require.NotNil(t, cfg.TopP)
	assert.InDelta(t, 0.7, *cfg.TopP, 0.0001)
	assert.Equal(t, "Use aliases.", cfg.SystemPrompt)
}

func TestParseOpenCodeAgentFile_ImportsModeAndToolsOnlyFrontmatter(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), "agent.md")
	require.NoError(t, os.WriteFile(path, []byte(`---
mode: subagent
tools:
  read: true
  write: false
---
`), 0o600))

	cfg, diagnostics, ok := parseOpenCodeAgentFileWithDiagnostics(path)
	require.True(t, ok)
	require.Empty(t, diagnostics)
	assert.Equal(t, "subagent", cfg.Mode)
	assert.Equal(t, map[string]bool{"read": true, "write": false}, cfg.ToolPermissions)
}

func TestParseOpenCodeAgentFileWithDiagnostics_WarnsForUnsupportedFrontmatterFields(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), "agent.md")
	require.NoError(t, os.WriteFile(path, []byte(`---
description: Reviews code
permission:
  bash: deny
---
Review carefully.
`), 0o600))

	cfg, diagnostics, ok := parseOpenCodeAgentFileWithDiagnostics(path)
	require.True(t, ok)
	assert.Equal(t, "Reviews code", cfg.Description)
	assert.Equal(t, "Review carefully.", cfg.SystemPrompt)
	assertDiagnosticContains(t, diagnostics, "frontmatter.permission: ignored unsupported field")
}

func TestParseOpenCodeAgentFileWithDiagnostics_WarnsForMalformedFrontmatterFields(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), "agent.md")
	require.NoError(t, os.WriteFile(path, []byte(`---
description: ["not", "a string"]
temperature: hot
hidden: sometimes
maxTokens: 1.5
tools:
  read: yes
  write: true
---
Review carefully.
`), 0o600))

	cfg, diagnostics, ok := parseOpenCodeAgentFileWithDiagnostics(path)
	require.True(t, ok)
	assert.Equal(t, "Review carefully.", cfg.SystemPrompt)
	assert.Equal(t, map[string]bool{"write": true}, cfg.ToolPermissions)
	assertDiagnosticContains(t, diagnostics, "frontmatter.description: ignored value: expected string, got array")
	assertDiagnosticContains(t, diagnostics, "frontmatter.temperature: ignored value: expected number, got string")
	assertDiagnosticContains(t, diagnostics, "frontmatter.hidden: ignored value: expected boolean, got string")
	assertDiagnosticContains(t, diagnostics, "frontmatter.maxTokens: ignored value: expected integer, got number")
	assertDiagnosticContains(t, diagnostics, "frontmatter.tools.read: ignored tool permission: expected boolean, got string")
}

func TestParseOpenCodeAgentFileWithDiagnostics_WarnsForDuplicateFrontmatterFields(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), "agent.md")
	require.NoError(t, os.WriteFile(path, []byte(`---
description: first
description: second
---
Review carefully.
`), 0o600))

	cfg, diagnostics, ok := parseOpenCodeAgentFileWithDiagnostics(path)

	assert.False(t, ok)
	assert.True(t, agentConfigEmpty(cfg))
	assertDiagnosticContains(t, diagnostics, "frontmatter.description: duplicate frontmatter field")
	assertDiagnosticContains(t, diagnostics, "frontmatter: ignored agent file: parse frontmatter")
}

func TestParseOpenCodeAgentFileWithDiagnostics_WarnsForNegativeMaxTokens(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), "agent.md")
	require.NoError(t, os.WriteFile(path, []byte(`---
maxTokens: -1
---
Use aliases.
`), 0o600))

	cfg, diagnostics, ok := parseOpenCodeAgentFileWithDiagnostics(path)
	require.True(t, ok)
	assert.Zero(t, cfg.MaxTokens)
	assertDiagnosticContains(t, diagnostics, "frontmatter.maxTokens: ignored value: expected non-negative integer, got -1")
}

func TestMergeConfigAgent_CanClearExplicitHidden(t *testing.T) {
	t.Parallel()

	current := AgentConfig{Hidden: true, hiddenSet: true}

	mergeConfigAgent(&current, AgentConfig{Hidden: false, hiddenSet: true}, nil, originSource{}, "reviewer")

	assert.False(t, current.Hidden)
}

func TestParseOpencodeConfig_PreservesHiddenFalseOnlyOverride(t *testing.T) {
	t.Parallel()

	cfg := parseOpencodeConfig("opencode.json", []byte(`{
		"agent": {
			"reviewer": {
				"hidden": false
			}
		}
	}`))

	agentCfg, ok := cfg.Agents["reviewer"]
	require.True(t, ok)
	assert.False(t, agentCfg.Hidden)
	assert.True(t, agentCfg.hiddenSet)
}

func TestImportOpencodeConfig_CustomConfigOverridesAgentDirs(t *testing.T) {
	tempDir := t.TempDir()
	t.Setenv("HOME", tempDir)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(tempDir, "config"))
	t.Chdir(tempDir)

	agentDir := filepath.Join(tempDir, ".opencode", "agents")
	require.NoError(t, os.MkdirAll(agentDir, 0o700))
	require.NoError(t, os.WriteFile(filepath.Join(agentDir, "reviewer.md"), []byte(`---
model: openai/from-agent-dir
---
Agent dir prompt.
`), 0o600))
	customPath := filepath.Join(tempDir, "custom-opencode.json")
	require.NoError(t, os.WriteFile(customPath, []byte(`{
		"agent": {
			"reviewer": {
				"model": "openai/from-env",
				"prompt": "Custom prompt."
			}
		}
	}`), 0o600))
	t.Setenv("OPENCODE_CONFIG", customPath)

	cfg, loaded, ok := importOpencodeConfig()

	require.True(t, ok)
	assert.Contains(t, loaded, filepath.Join(agentDir, "reviewer.md"))
	assert.True(t, strings.HasSuffix(loaded, customPath))
	assert.Equal(t, "openai/from-env", cfg.Agents["reviewer"].Model)
	assert.Equal(t, "Custom prompt.", cfg.Agents["reviewer"].SystemPrompt)
}

func TestOpenCodeConfigPaths_EnvConfigHasHighestPrecedence(t *testing.T) {
	t.Setenv("OPENCODE_CONFIG", "/tmp/custom-opencode.json")

	paths := opencodeConfigPaths()

	require.NotEmpty(t, paths)
	assert.Equal(t, "/tmp/custom-opencode.json", paths[len(paths)-1])
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

func TestParseForgeConfigWithDiagnostics_ParsesFixture(t *testing.T) {
	t.Parallel()

	cfg, diagnostics := parseForgeConfigWithDiagnostics("forge-realistic.toml", readHarnessFixture(t, "forge-realistic.toml"))

	require.Empty(t, diagnostics)
	assert.Equal(t, testClaudeCodeProvider, cfg.DefaultProvider)
	assert.Equal(t, "claude-opus-4-6", cfg.DefaultModel)
	assert.Equal(t, "https://claude-code.example", cfg.Providers[testClaudeCodeProvider].BaseURL)
}

func TestParseForgeConfigWithDiagnostics_ParsesDottedSessionKeys(t *testing.T) {
	t.Parallel()

	cfg, diagnostics := parseForgeConfigWithDiagnostics("forge-dotted.toml", []byte(`
session.provider_id = "claude_code"
session.model_id = "claude-opus-4-6"
session.base_url = "https://claude-code.example"
`))

	require.Empty(t, diagnostics)
	assert.Equal(t, testClaudeCodeProvider, cfg.DefaultProvider)
	assert.Equal(t, "claude-opus-4-6", cfg.DefaultModel)
	assert.Equal(t, "https://claude-code.example", cfg.Providers[testClaudeCodeProvider].BaseURL)
}

func TestParseForgeConfigWithDiagnostics_WarnsForMalformedData(t *testing.T) {
	t.Parallel()

	cfg, diagnostics := parseForgeConfigWithDiagnostics("forge-malformed.toml", readHarnessFixture(t, "forge-malformed.toml"))

	assert.Equal(t, testClaudeCodeProvider, cfg.DefaultProvider)
	assert.Empty(t, cfg.DefaultModel)
	assertDiagnosticContains(t, diagnostics, "unknown_forge_knob: ignored unsupported field")
	assertDiagnosticContains(t, diagnostics, "session.extra_session_knob: ignored unsupported field")
	assertDiagnosticContains(t, diagnostics, "session.model_id: ignored value: expected string, got array")
	assertDiagnosticContains(t, diagnostics, "provider_id: ignored value: expected string, got number")
}

func TestParseForgeConfigWithDiagnostics_WarnsForInvalidTOMLSyntax(t *testing.T) {
	t.Parallel()

	cfg, diagnostics := parseForgeConfigWithDiagnostics("forge-invalid.toml", []byte(`
[session
model_id = "claude-opus-4-6"
`))

	assert.True(t, cfg.empty())
	assertDiagnosticContains(t, diagnostics, "forge-invalid.toml: ignored harness config: parse TOML")
}

func readHarnessFixture(t *testing.T, name string) []byte {
	t.Helper()

	data, err := os.ReadFile(filepath.Join("testdata", "harness", name))
	require.NoError(t, err)

	return data
}

func assertDiagnosticContains(t *testing.T, diagnostics []Diagnostic, want string) {
	t.Helper()

	for _, diagnostic := range diagnostics {
		if strings.Contains(diagnostic.String(), want) {
			return
		}
	}

	require.Failf(t, "diagnostic not found", "wanted diagnostic containing %q in %#v", want, diagnostics)
}

func countDiagnosticsContaining(diagnostics []Diagnostic, want string) int {
	count := 0

	for _, diagnostic := range diagnostics {
		if strings.Contains(diagnostic.String(), want) {
			count++
		}
	}

	return count
}
