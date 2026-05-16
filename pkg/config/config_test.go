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
