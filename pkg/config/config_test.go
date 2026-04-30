package config

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func TestLoadFiles_MergesInOrder(t *testing.T) {
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
    system_prompt: review code
    fallback_models: [gpt-review-backup]
    temperature: 0.2
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
generation:
  temperature: 0
  top_p: 0.8
  max_tokens: 900
`)

	cfg, loaded, err := LoadFiles([]string{global, filepath.Join(dir, "missing.json"), local})
	if err != nil {
		t.Fatal(err)
	}

	if !reflect.DeepEqual(loaded, []string{global, local}) {
		t.Fatalf("loaded = %v, want [%s %s]", loaded, global, local)
	}
	if cfg.DefaultProvider != "anthropic" {
		t.Errorf("DefaultProvider = %q, want anthropic", cfg.DefaultProvider)
	}
	if cfg.DefaultModel != "gpt-local" {
		t.Errorf("DefaultModel = %q, want gpt-local", cfg.DefaultModel)
	}
	if !reflect.DeepEqual(cfg.FallbackModels, []string{"gpt-backup"}) {
		t.Errorf("FallbackModels = %v", cfg.FallbackModels)
	}

	openai := cfg.Providers["openai"]
	if openai.Disabled {
		t.Error("openai disabled should be overridden to false")
	}
	if openai.BaseURL != "https://openai.global" {
		t.Errorf("openai base_url = %q", openai.BaseURL)
	}

	anthropic := cfg.Providers["anthropic"]
	if anthropic.BaseURL != "https://anthropic.global" {
		t.Errorf("anthropic base_url = %q", anthropic.BaseURL)
	}

	reviewer := cfg.Agents["reviewer"]
	if reviewer.SystemPrompt != "review code" {
		t.Errorf("reviewer system_prompt = %q", reviewer.SystemPrompt)
	}
	if !reflect.DeepEqual(reviewer.FallbackModels, []string{"gpt-review-backup"}) {
		t.Errorf("reviewer fallback_models = %v", reviewer.FallbackModels)
	}
	if reviewer.Temperature == nil || *reviewer.Temperature != 0.2 {
		t.Errorf("reviewer temperature = %v", reviewer.Temperature)
	}
	if !reflect.DeepEqual(reviewer.Triggers, []string{"review this", "code review"}) {
		t.Errorf("reviewer triggers = %v", reviewer.Triggers)
	}

	hooks := cfg.Hooks["assistant_message"]
	if len(hooks) != 1 {
		t.Fatalf("assistant hooks len = %d, want 1", len(hooks))
	}
	if !reflect.DeepEqual(hooks[0].Command, []string{"logger", "--assistant"}) {
		t.Errorf("hook command = %v", hooks[0].Command)
	}
	if hooks[0].TimeoutSeconds != 3 {
		t.Errorf("hook timeout = %d", hooks[0].TimeoutSeconds)
	}
	if hooks[0].Env["EXTRA"] != "1" {
		t.Errorf("hook env EXTRA = %q", hooks[0].Env["EXTRA"])
	}

	if cfg.Context.MaxFileBytes != 123 {
		t.Errorf("MaxFileBytes = %d, want 123", cfg.Context.MaxFileBytes)
	}
	if cfg.Context.MaxTotalBytes != 456 {
		t.Errorf("MaxTotalBytes = %d, want 456", cfg.Context.MaxTotalBytes)
	}
	if cfg.Generation.Temperature == nil || *cfg.Generation.Temperature != 0 {
		t.Errorf("generation temperature = %v", cfg.Generation.Temperature)
	}
	if cfg.Generation.TopP == nil || *cfg.Generation.TopP != 0.8 {
		t.Errorf("generation top_p = %v", cfg.Generation.TopP)
	}
	if cfg.Generation.MaxTokens != 900 {
		t.Errorf("generation max_tokens = %d, want 900", cfg.Generation.MaxTokens)
	}
}

func TestLoadFiles_JSONCompatibility(t *testing.T) {
	dir := t.TempDir()
	path := writeConfig(t, dir, "legacy.json", `{"default_model":"gpt-json"}`)

	cfg, loaded, err := LoadFiles([]string{path})
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(loaded, []string{path}) {
		t.Fatalf("loaded = %v, want [%s]", loaded, path)
	}
	if cfg.DefaultModel != "gpt-json" {
		t.Fatalf("DefaultModel = %q, want gpt-json", cfg.DefaultModel)
	}
}

func TestLoadFiles_InvalidYAMLIncludesPath(t *testing.T) {
	dir := t.TempDir()
	path := writeConfig(t, dir, "bad.yaml", `default_model: [`)

	_, _, err := LoadFiles([]string{path})
	if err == nil {
		t.Fatal("expected parse error")
	}
	if got := err.Error(); !strings.Contains(got, path) {
		t.Fatalf("error = %q, want path %q", got, path)
	}
}

func TestDefaultPaths_IncludesEnvOverrideLast(t *testing.T) {
	t.Setenv(EnvPath, "one"+string(os.PathListSeparator)+"two")

	paths := DefaultPaths()
	if len(paths) < 2 {
		t.Fatalf("paths = %v, want env paths included", paths)
	}

	gotTail := paths[len(paths)-2:]
	if !reflect.DeepEqual(gotTail, []string{"one", "two"}) {
		t.Fatalf("tail = %v, want [one two]", gotTail)
	}
}

func TestDefaultPaths_PrefersYAML(t *testing.T) {
	t.Setenv(EnvPath, "")

	paths := DefaultPaths()
	if len(paths) < 3 {
		t.Fatalf("paths = %v, want default paths", paths)
	}

	for i, path := range paths {
		if strings.HasSuffix(path, "config.json") && i > 0 && strings.HasSuffix(paths[i-1], "config.yml") {
			return
		}
	}
	t.Fatalf("paths = %v, want config.yaml/config.yml before config.json", paths)
}

func writeConfig(t *testing.T, dir, name, content string) string {
	t.Helper()

	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}
