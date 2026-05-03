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

func TestParseOpencodeConfig_UsesTopLevelModelProviderConfigAndAgents(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	promptPath := filepath.Join(dir, "review.txt")
	require.NoError(t, os.WriteFile(promptPath, []byte("Review code carefully."), 0o600))

	cfg := parseOpencodeConfig(filepath.Join(dir, "opencode.json"), []byte(`{
		"$schema": "https://opencode.ai/config.json",
		// OpenCode supports JSONC.
		"model": "openai/gpt-5.4",
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
				"base_url": "https://anthropic.example",
			},
		},
	}`))

	if cfg.DefaultProvider != testOpenAIProvider {
		assert.Failf(t, "assertion failed", "DefaultProvider = %q, want openai", cfg.DefaultProvider)
	}
	if cfg.DefaultModel != "openai/gpt-5.4" {
		assert.Failf(t, "assertion failed", "DefaultModel = %q, want openai/gpt-5.4", cfg.DefaultModel)
	}
	if cfg.Providers[testOpenAIProvider].BaseURL != "https://openai.example" {
		assert.Failf(t, "assertion failed", "openai base_url = %q", cfg.Providers[testOpenAIProvider].BaseURL)
	}
	if cfg.Providers[testAnthropicProvider].BaseURL != "https://anthropic.example" {
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
				"model": "openai/gpt-5.4"
			},
			"quick": {
				"model": "openai/gpt-5.4-mini"
			}
		}
	}`))

	if cfg.DefaultProvider != testOpenAIProvider {
		assert.Failf(t, "assertion failed", "DefaultProvider = %q, want openai", cfg.DefaultProvider)
	}
	if cfg.DefaultModel != "openai/gpt-5.4" {
		assert.Failf(t, "assertion failed", "DefaultModel = %q, want openai/gpt-5.4", cfg.DefaultModel)
	}
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
model: openai/gpt-5.4
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
		reviewer.Model != "openai/gpt-5.4" {
		assert.Failf(t, "assertion failed", "review agent = %+v", reviewer)
	}
	if !cfg.Agents["internal"].Hidden {
		assert.FailNow(t, "expected internal agent to be hidden")
	}
	assert.NotContains(t, cfg.Agents, "README")
	assert.NotContains(t, cfg.Agents, ".hidden")
	assert.NotContains(t, cfg.Agents, "linked")
}

func TestMergeConfigAgent_CanClearExplicitHidden(t *testing.T) {
	t.Parallel()
	current := AgentConfig{Hidden: true, hiddenSet: true}

	mergeConfigAgent(&current, AgentConfig{Hidden: false, hiddenSet: true})

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
