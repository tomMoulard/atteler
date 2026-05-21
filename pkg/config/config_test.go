package config

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestLoadFiles_MergesInOrder(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	global := writeConfig(t, dir, "global.yaml", `
default_provider: anthropic
default_model: claude-default
fallback_models: [claude-fallback]
providers:
  anthropic:
    base_url: https://anthropic.global
  openai:
    disabled: true
    base_url: https://openai.global
`)
	local := writeConfig(t, dir, "local.yml", `
default_model: gpt-local
fallback_models: [gpt-backup]
providers:
  openai:
    disabled: false
agents:
  reviewer:
    description: Reviews code changes
    personality: concise
    system_prompt: review code
    fallback_models: [gpt-review-backup]
    capabilities: [review, security]
    temperature: 0.2
    seed: 42
    reasoning_level: high
    triggers: ["review this", "code review"]
hooks:
  assistant_message:
    - command: [logger, --assistant]
      timeout_seconds: 3
      env:
        EXTRA: "1"
context:
  max_file_bytes: 123
  max_total_bytes: 456
  max_input_tokens: 789
generation:
  temperature: 0
  top_p: 0.8
  seed: 7
  reasoning_level: medium
  max_tokens: 900
plugins:
  paths:
    - ./plugin-a
`)

	cfg, loaded, err := LoadFiles([]string{global, filepath.Join(dir, "missing.json"), local})
	if err != nil {
		require.NoError(t, err)
	}

	if !reflect.DeepEqual(loaded, []string{global, local}) {
		require.Failf(t, "unexpected failure", "loaded = %v, want [%s %s]", loaded, global, local)
	}

	if cfg.DefaultProvider != "anthropic" {
		assert.Failf(t, "assertion failed", "DefaultProvider = %q, want anthropic", cfg.DefaultProvider)
	}

	if cfg.DefaultModel != "gpt-local" {
		assert.Failf(t, "assertion failed", "DefaultModel = %q, want gpt-local", cfg.DefaultModel)
	}

	if !reflect.DeepEqual(cfg.FallbackModels, []string{"gpt-backup"}) {
		assert.Failf(t, "assertion failed", "FallbackModels = %v", cfg.FallbackModels)
	}

	openai := cfg.Providers["openai"]
	if openai.Disabled {
		assert.Fail(t, "openai disabled should be overridden to false")
	}

	if openai.BaseURL != "https://openai.global" {
		assert.Failf(t, "assertion failed", "openai base_url = %q", openai.BaseURL)
	}

	anthropic := cfg.Providers["anthropic"]
	if anthropic.BaseURL != "https://anthropic.global" {
		assert.Failf(t, "assertion failed", "anthropic base_url = %q", anthropic.BaseURL)
	}

	reviewer := cfg.Agents["reviewer"]
	if reviewer.SystemPrompt != "review code" {
		assert.Failf(t, "assertion failed", "reviewer system_prompt = %q", reviewer.SystemPrompt)
	}

	if reviewer.Description != "Reviews code changes" {
		assert.Failf(t, "assertion failed", "reviewer description = %q", reviewer.Description)
	}

	if reviewer.Personality != "concise" {
		assert.Failf(t, "assertion failed", "reviewer personality = %q", reviewer.Personality)
	}

	if !reflect.DeepEqual(reviewer.FallbackModels, []string{"gpt-review-backup"}) {
		assert.Failf(t, "assertion failed", "reviewer fallback_models = %v", reviewer.FallbackModels)
	}

	if !reflect.DeepEqual(reviewer.Capabilities, []string{"review", "security"}) {
		assert.Failf(t, "assertion failed", "reviewer capabilities = %v", reviewer.Capabilities)
	}

	if reviewer.Temperature == nil || *reviewer.Temperature != 0.2 {
		assert.Failf(t, "assertion failed", "reviewer temperature = %v", reviewer.Temperature)
	}

	if reviewer.Seed == nil || *reviewer.Seed != 42 {
		assert.Failf(t, "assertion failed", "reviewer seed = %v", reviewer.Seed)
	}

	if reviewer.ReasoningLevel != "high" {
		assert.Failf(t, "assertion failed", "reviewer reasoning_level = %q", reviewer.ReasoningLevel)
	}

	if !reflect.DeepEqual(reviewer.Triggers, []string{"review this", "code review"}) {
		assert.Failf(t, "assertion failed", "reviewer triggers = %v", reviewer.Triggers)
	}

	hooks := cfg.Hooks["assistant_message"]
	if len(hooks) != 1 {
		require.Failf(t, "unexpected failure", "assistant hooks len = %d, want 1", len(hooks))
	}

	if !reflect.DeepEqual(hooks[0].Command, []string{"logger", "--assistant"}) {
		assert.Failf(t, "assertion failed", "hook command = %v", hooks[0].Command)
	}

	if hooks[0].TimeoutSeconds != 3 {
		assert.Failf(t, "assertion failed", "hook timeout = %d", hooks[0].TimeoutSeconds)
	}

	if hooks[0].Env["EXTRA"] != "1" {
		assert.Failf(t, "assertion failed", "hook env EXTRA = %q", hooks[0].Env["EXTRA"])
	}

	if cfg.Context.MaxFileBytes != 123 {
		assert.Failf(t, "assertion failed", "MaxFileBytes = %d, want 123", cfg.Context.MaxFileBytes)
	}

	if cfg.Context.MaxTotalBytes != 456 {
		assert.Failf(t, "assertion failed", "MaxTotalBytes = %d, want 456", cfg.Context.MaxTotalBytes)
	}

	if cfg.Context.MaxInputTokens != 789 {
		assert.Failf(t, "assertion failed", "MaxInputTokens = %d, want 789", cfg.Context.MaxInputTokens)
	}

	if cfg.Generation.Temperature == nil || *cfg.Generation.Temperature != 0 {
		assert.Failf(t, "assertion failed", "generation temperature = %v", cfg.Generation.Temperature)
	}

	if cfg.Generation.TopP == nil || *cfg.Generation.TopP != 0.8 {
		assert.Failf(t, "assertion failed", "generation top_p = %v", cfg.Generation.TopP)
	}

	if cfg.Generation.Seed == nil || *cfg.Generation.Seed != 7 {
		assert.Failf(t, "assertion failed", "generation seed = %v", cfg.Generation.Seed)
	}

	if cfg.Generation.ReasoningLevel != "medium" {
		assert.Failf(t, "assertion failed", "generation reasoning_level = %q", cfg.Generation.ReasoningLevel)
	}

	if cfg.Generation.MaxTokens != 900 {
		assert.Failf(t, "assertion failed", "generation max_tokens = %d, want 900", cfg.Generation.MaxTokens)
	}

	if !reflect.DeepEqual(cfg.Plugins.Paths, []string{"./plugin-a"}) {
		assert.Failf(t, "assertion failed", "plugin paths = %v", cfg.Plugins.Paths)
	}
}

func TestLoadFiles_JSONCompatibility(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	path := writeConfig(t, dir, "legacy.json", `{"default_model":"gpt-json"}`)

	cfg, loaded, err := LoadFiles([]string{path})
	if err != nil {
		require.NoError(t, err)
	}

	if !reflect.DeepEqual(loaded, []string{path}) {
		require.Failf(t, "unexpected failure", "loaded = %v, want [%s]", loaded, path)
	}

	if cfg.DefaultModel != "gpt-json" {
		require.Failf(t, "unexpected failure", "DefaultModel = %q, want gpt-json", cfg.DefaultModel)
	}
}

func TestLoadFilesWithOrigins_TracksScalarOverwriteAndSliceReplacement(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	first := writeConfig(t, dir, "first.yaml", `
default_model: gpt-first
fallback_models: [gpt-first-fallback]
`)
	second := writeConfig(t, dir, "second.yaml", `
default_model: gpt-second
fallback_models: [gpt-second-fallback]
`)

	cfg, loaded, origins, err := LoadFilesWithOrigins([]string{first, second})
	require.NoError(t, err)
	require.Equal(t, []string{first, second}, loaded)
	assert.Equal(t, "gpt-second", cfg.DefaultModel)
	assert.Equal(t, []string{"gpt-second-fallback"}, cfg.FallbackModels)

	modelOrigin := origins["default_model"].Chain
	require.Len(t, modelOrigin, 2)
	assert.Equal(t, OriginSet, modelOrigin[0].Operation)
	assert.Equal(t, first, modelOrigin[0].Source)
	assert.Equal(t, OriginOverride, modelOrigin[1].Operation)
	assert.Equal(t, second, modelOrigin[1].Source)

	fallbackOrigin := origins["fallback_models"].Chain
	require.Len(t, fallbackOrigin, 2)
	assert.Equal(t, OriginSet, fallbackOrigin[0].Operation)
	assert.Equal(t, OriginReplace, fallbackOrigin[1].Operation)
	assert.Equal(t, second, fallbackOrigin[1].Source)
	assert.Contains(t, fallbackOrigin[1].Note, "replaces")
}

func TestLoadFilesWithOrigins_TracksMapMergeProviderAgentAndPluginReplacement(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	first := writeConfig(t, dir, "first.yaml", `
providers:
  anthropic:
    base_url: https://anthropic.first
  openai:
    base_url: https://openai.first
agents:
  reviewer:
    model: gpt-review-first
    tools:
      bash: true
plugins:
  paths: [./plugin-a]
`)
	second := writeConfig(t, dir, "second.yaml", `
providers:
  openai:
    base_url: https://openai.second
    disabled: false
agents:
  reviewer:
    model: gpt-review-second
    tools:
      shell: false
plugins:
  paths: [./plugin-b]
`)

	cfg, _, origins, err := LoadFilesWithOrigins([]string{first, second})
	require.NoError(t, err)
	assert.Equal(t, "https://anthropic.first", cfg.Providers["anthropic"].BaseURL)
	assert.Equal(t, "https://openai.second", cfg.Providers["openai"].BaseURL)
	assert.Equal(t, "gpt-review-second", cfg.Agents["reviewer"].Model)
	assert.Equal(t, map[string]bool{"shell": false}, cfg.Agents["reviewer"].ToolPermissions)
	assert.Equal(t, []string{"./plugin-b"}, cfg.Plugins.Paths)

	providerMapOrigin := origins["providers"].Chain
	require.Len(t, providerMapOrigin, 2)
	assert.Equal(t, OriginSet, providerMapOrigin[0].Operation)
	assert.Equal(t, OriginMerge, providerMapOrigin[1].Operation)

	providerFieldOrigin := origins["providers.openai.base_url"].Chain
	require.Len(t, providerFieldOrigin, 2)
	assert.Equal(t, OriginOverride, providerFieldOrigin[1].Operation)
	assert.Equal(t, second, providerFieldOrigin[1].Source)

	agentOrigin := origins["agents.reviewer.model"].Chain
	require.Len(t, agentOrigin, 2)
	assert.Equal(t, OriginOverride, agentOrigin[1].Operation)
	assert.Equal(t, second, agentOrigin[1].Source)

	toolsOrigin := origins["agents.reviewer.tools"].Chain
	require.Len(t, toolsOrigin, 2)
	assert.Equal(t, OriginReplace, toolsOrigin[1].Operation)

	pluginOrigin := origins["plugins.paths"].Chain
	require.Len(t, pluginOrigin, 2)
	assert.Equal(t, OriginReplace, pluginOrigin[1].Operation)
	assert.Equal(t, second, pluginOrigin[1].Source)
}

func TestOriginChain_MergesMapOriginsAcrossOriginMaps(t *testing.T) {
	t.Parallel()

	dst := OriginMap{
		"providers": {
			Chain: []OriginEvent{{
				Kind:      OriginHarnessImport,
				Operation: OriginSet,
				Source:    "harness",
				Value:     `["codex"]`,
				Note:      "merges provider definitions by name",
			}},
		},
	}
	src := OriginMap{
		"providers": {
			Chain: []OriginEvent{{
				Kind:      OriginEnvFile,
				Operation: OriginSet,
				Source:    "env.yaml",
				Value:     `["openai"]`,
				Note:      "merges provider definitions by name",
			}},
		},
		"plugins.paths": {
			Chain: []OriginEvent{{
				Kind:      OriginEnvFile,
				Operation: OriginSet,
				Source:    "env.yaml",
				Value:     `["./plugin"]`,
				Note:      "replaces the entire plugin path list",
			}},
		},
	}

	appendOriginChain(dst, "providers", src, false)
	appendOriginChain(dst, "plugins.paths", src, true)

	require.Len(t, dst["providers"].Chain, 2)
	assert.Equal(t, OriginMerge, dst["providers"].Chain[1].Operation)
	require.Len(t, dst["plugins.paths"].Chain, 1)
	assert.Equal(t, OriginSet, dst["plugins.paths"].Chain[0].Operation)

	appendOriginChain(dst, "plugins.paths", src, true)
	require.Len(t, dst["plugins.paths"].Chain, 2)
	assert.Equal(t, OriginReplace, dst["plugins.paths"].Chain[1].Operation)
}

func TestLoadPathSources_EnvPathPrecedence(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	global := writeConfig(t, dir, "global.yaml", `default_model: gpt-global`)
	project := writeConfig(t, dir, "project.yaml", `default_model: gpt-project`)
	envPath := writeConfig(t, dir, "env.yaml", `default_model: gpt-env`)

	cfg, loaded, origins, err := LoadPathSources([]PathSource{
		{Path: global, Kind: OriginGlobalFile},
		{Path: project, Kind: OriginProjectFile},
		{Path: envPath, Kind: OriginEnvFile},
	})
	require.NoError(t, err)
	require.Equal(t, []string{global, project, envPath}, loaded)
	assert.Equal(t, "gpt-env", cfg.DefaultModel)

	chain := origins["default_model"].Chain
	require.Len(t, chain, 3)
	assert.Equal(t, OriginGlobalFile, chain[0].Kind)
	assert.Equal(t, OriginProjectFile, chain[1].Kind)
	assert.Equal(t, OriginEnvFile, chain[2].Kind)
	assert.Equal(t, OriginOverride, chain[2].Operation)
}

func TestLoadWithOrigins_DefaultStackClassifiesEnvAndOverridesProject(t *testing.T) {
	tempDir := t.TempDir()
	configHome := filepath.Join(tempDir, "xdg-config")
	projectDir := filepath.Join(tempDir, "project")
	require.NoError(t, os.MkdirAll(filepath.Join(configHome, "atteler"), 0o700))
	require.NoError(t, os.MkdirAll(filepath.Join(projectDir, ".atteler"), 0o700))
	require.NoError(t, os.MkdirAll(filepath.Join(tempDir, "codex-home"), 0o700))

	global := writeConfig(t, filepath.Join(configHome, "atteler"), "config.yaml", `default_model: gpt-global`)
	project := writeConfig(t, filepath.Join(projectDir, ".atteler"), "config.yaml", `default_model: gpt-project`)
	envPath := writeConfig(t, tempDir, "env.yaml", `default_model: gpt-env`)

	t.Setenv("HOME", tempDir)
	t.Setenv("XDG_CONFIG_HOME", configHome)
	t.Setenv("CODEX_HOME", filepath.Join(tempDir, "codex-home"))
	t.Setenv("OPENCODE_CONFIG", "")
	t.Setenv("OPENCODE_CONFIG_DIR", "")
	t.Setenv("FORGE_CONFIG", "")
	t.Setenv(EnvPath, envPath)
	t.Chdir(projectDir)

	cfg, loaded, origins, err := LoadWithOrigins()
	require.NoError(t, err)
	assert.Equal(t, "gpt-env", cfg.DefaultModel)
	assert.Contains(t, loaded, global)
	assert.Contains(t, loaded, project)
	assert.Contains(t, loaded, envPath)

	chain := origins["default_model"].Chain
	require.Len(t, chain, 3)
	assert.Equal(t, OriginGlobalFile, chain[0].Kind)
	assert.Equal(t, OriginProjectFile, chain[1].Kind)
	assert.Equal(t, OriginEnvFile, chain[2].Kind)
	assert.Equal(t, envPath, chain[2].Source)
}

func TestLoadFiles_InvalidYAMLIncludesPath(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	path := writeConfig(t, dir, "bad.yaml", `default_model: [`)

	_, _, err := LoadFiles([]string{path})
	if err == nil {
		require.FailNow(t, "expected parse error")
	}

	if got := err.Error(); !strings.Contains(got, path) {
		require.Failf(t, "unexpected failure", "error = %q, want path %q", got, path)
	}
}

func TestDefaultPaths_IncludesEnvOverrideLast(t *testing.T) {
	t.Setenv(EnvPath, "one"+string(os.PathListSeparator)+"two")

	paths := DefaultPaths()
	if len(paths) < 2 {
		require.Failf(t, "unexpected failure", "paths = %v, want env paths included", paths)
	}

	gotTail := paths[len(paths)-2:]
	if !reflect.DeepEqual(gotTail, []string{"one", "two"}) {
		require.Failf(t, "unexpected failure", "tail = %v, want [one two]", gotTail)
	}
}

func TestDefaultPaths_PrefersYAML(t *testing.T) {
	t.Setenv(EnvPath, "")

	paths := DefaultPaths()
	if len(paths) < 3 {
		require.Failf(t, "unexpected failure", "paths = %v, want default paths", paths)
	}

	for i, path := range paths {
		if strings.HasSuffix(path, "config.json") && i > 0 && strings.HasSuffix(paths[i-1], "config.yml") {
			return
		}
	}

	require.Failf(t, "unexpected failure", "paths = %v, want config.yaml/config.yml before config.json", paths)
}

func TestLoadFiles_ContextReferences(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	path := writeConfig(t, dir, "refs.yaml", `
context:
  references:
    - ./docs/guide.md
    - https://example.com/api-docs
`)

	cfg, loaded, err := LoadFiles([]string{path})
	require.NoError(t, err)
	require.Equal(t, []string{path}, loaded)
	assert.Equal(t, []string{"./docs/guide.md", "https://example.com/api-docs"}, cfg.Context.References)
}

func TestLoadFiles_ContextReferencesOverride(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	first := writeConfig(t, dir, "first.yaml", `
context:
  references:
    - ./old-docs.md
`)
	second := writeConfig(t, dir, "second.yaml", `
context:
  references:
    - ./new-docs.md
    - https://example.com/ref
`)

	cfg, _, err := LoadFiles([]string{first, second})
	require.NoError(t, err)
	assert.Equal(t, []string{"./new-docs.md", "https://example.com/ref"}, cfg.Context.References)
}

func TestLoadFiles_ContextReferencePolicy(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	path := writeConfig(t, dir, "refs-policy.yaml", `
context:
  reference_policy:
    allowed_schemes: [https]
    allowed_hosts:
      - docs.example.com
      - "*.trusted.example"
    local_roots:
      - ../shared-style
    max_redirects: 2
    content_types:
      - text/*
      - application/json
    allow_private_networks: false
`)

	cfg, _, err := LoadFiles([]string{path})
	require.NoError(t, err)
	assert.Equal(t, []string{"https"}, cfg.Context.ReferencePolicy.AllowedSchemes)
	assert.Equal(t, []string{"docs.example.com", "*.trusted.example"}, cfg.Context.ReferencePolicy.AllowedHosts)
	assert.Equal(t, []string{"../shared-style"}, cfg.Context.ReferencePolicy.LocalRoots)
	assert.Equal(t, 2, cfg.Context.ReferencePolicy.MaxRedirects)
	assert.Equal(t, []string{"text/*", "application/json"}, cfg.Context.ReferencePolicy.ContentTypes)
	assert.False(t, cfg.Context.ReferencePolicy.AllowPrivateNetworks)
}

func TestLoadFiles_AgentReferences(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	path := writeConfig(t, dir, "agent-refs.yaml", `
agents:
  reviewer:
    description: Reviews code
    references:
      - ./review-guidelines.md
      - https://example.com/style-guide
`)

	cfg, _, err := LoadFiles([]string{path})
	require.NoError(t, err)

	reviewer := cfg.Agents["reviewer"]
	assert.Equal(t, "Reviews code", reviewer.Description)
	assert.Equal(t, []string{"./review-guidelines.md", "https://example.com/style-guide"}, reviewer.References)
}

func TestLoadFiles_AgentReferencesOverride(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	first := writeConfig(t, dir, "first.yaml", `
agents:
  reviewer:
    references:
      - ./old-guide.md
`)
	second := writeConfig(t, dir, "second.yaml", `
agents:
  reviewer:
    references:
      - ./new-guide.md
`)

	cfg, _, err := LoadFiles([]string{first, second})
	require.NoError(t, err)
	assert.Equal(t, []string{"./new-guide.md"}, cfg.Agents["reviewer"].References)
}

func writeConfig(t *testing.T, dir, name, content string) string {
	t.Helper()

	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		require.NoError(t, err)
	}

	return path
}
