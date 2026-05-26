package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	appconfig "github.com/tommoulard/atteler/pkg/config"
)

func TestWriteConfigExplanation_IncludesProviderModelAndRuntimeProvenance(t *testing.T) {
	t.Parallel()

	origins := appconfig.OriginMap{
		"default_model": {
			Chain: []appconfig.OriginEvent{{
				Kind:      appconfig.OriginProjectFile,
				Operation: appconfig.OriginSet,
				Source:    ".atteler/config.yaml",
				Value:     "config-model",
			}},
		},
		"default_provider": {
			Chain: []appconfig.OriginEvent{{
				Kind:      appconfig.OriginProjectFile,
				Operation: appconfig.OriginSet,
				Source:    ".atteler/config.yaml",
				Value:     "openai",
			}},
		},
		"generation.reasoning_level": {
			Chain: []appconfig.OriginEvent{{
				Kind:      appconfig.OriginProjectFile,
				Operation: appconfig.OriginSet,
				Source:    ".atteler/config.yaml",
				Value:     "medium",
			}},
		},
		"providers.openai.base_url": {
			Chain: []appconfig.OriginEvent{{
				Kind:      appconfig.OriginProjectFile,
				Operation: appconfig.OriginSet,
				Source:    ".atteler/config.yaml",
				Value:     "https://openai.project",
			}},
		},
	}
	cfg := appconfig.Config{
		DefaultProvider: "openai",
		DefaultModel:    "config-model",
		Generation: appconfig.GenerationConfig{
			ReasoningLevel: "medium",
		},
	}

	addRuntimeConfigOrigins(
		origins,
		cfg,
		cliOptions{model: "cli-model", reasoningLevel: "high"},
		appconfig.State{DefaultReasoningLevel: "low"},
		"/repo",
		"state.yaml",
	)

	var out strings.Builder
	writeConfigExplanation(&out, []string{".atteler/config.yaml"}, origins, "")
	got := out.String()

	assert.Contains(t, got, "Precedence (lowest to highest):")
	assert.Contains(t, got, "Implicit defaults")
	assert.Contains(t, got, "agent_loop.max_iterations: unset/0")
	assert.Contains(t, got, "providers.openai.base_url: https://openai.project")
	assert.Contains(t, got, "runtime.selected_model: cli-model")
	assert.Contains(t, got, "--model [cli-flag]")
	assert.Contains(t, got, "runtime.selected_provider: openai")
	assert.Contains(t, got, "runtime.generation.reasoning_level: high")
	assert.Contains(t, got, "state.yaml global [state-override]")
	assert.Contains(t, got, "--reasoning-level [cli-flag]")
}

func TestAddRuntimeConfigOrigins_UsesProviderQualifiedModelPrefix(t *testing.T) {
	t.Parallel()

	origins := appconfig.OriginMap{}
	addRuntimeConfigOrigins(
		origins,
		appconfig.Config{DefaultProvider: "anthropic"},
		cliOptions{model: "openai/gpt-test"},
		appconfig.State{},
		"/repo",
		"state.yaml",
	)

	final, ok := origins.Final("runtime.selected_provider")
	require.True(t, ok)
	assert.Equal(t, "openai", final.Value)
	assert.Equal(t, appconfig.OriginRuntimeSelection, final.Kind)
	assert.Contains(t, final.Note, "provider-qualified")
}

func TestWriteConfigExplanation_FiltersFieldPrefixes(t *testing.T) {
	t.Parallel()

	origins := appconfig.OriginMap{
		"default_model": {
			Chain: []appconfig.OriginEvent{{
				Kind:      appconfig.OriginProjectFile,
				Operation: appconfig.OriginSet,
				Source:    ".atteler/config.yaml",
				Value:     "config-model",
			}},
		},
		"providers.openai.base_url": {
			Chain: []appconfig.OriginEvent{{
				Kind:      appconfig.OriginProjectFile,
				Operation: appconfig.OriginSet,
				Source:    ".atteler/config.yaml",
				Value:     "https://openai.project",
			}},
		},
		"providers.anthropic.base_url": {
			Chain: []appconfig.OriginEvent{{
				Kind:      appconfig.OriginProjectFile,
				Operation: appconfig.OriginSet,
				Source:    ".atteler/config.yaml",
				Value:     "https://anthropic.project",
			}},
		},
	}

	var out strings.Builder
	writeConfigExplanation(&out, []string{".atteler/config.yaml"}, origins, "providers.openai")
	got := out.String()

	assert.Contains(t, got, `Field origins matching "providers.openai":`)
	assert.Contains(t, got, "providers.openai.base_url: https://openai.project")
	assert.NotContains(t, got, "providers.anthropic.base_url:")
	assert.NotContains(t, got, "default_model:")
}

func TestWriteConfigExplanationWithDiagnostics_PrintsImporterWarnings(t *testing.T) {
	t.Parallel()

	diagnostics := []appconfig.Diagnostic{{
		Severity: appconfig.DiagnosticWarning,
		Importer: "opencode",
		Source:   "opencode.jsonc",
		Path:     "agent.bad.hidden",
		Message:  "ignored value: expected boolean, got string",
	}}

	var out strings.Builder
	writeConfigExplanationWithDiagnostics(&out, nil, appconfig.OriginMap{}, diagnostics, "")
	got := out.String()

	assert.Contains(t, got, "Config importer warnings:")
	assert.Contains(t, got, "[warning] opencode: opencode.jsonc agent.bad.hidden: ignored value")
}

func TestWriteConfigExplanationWithDiagnostics_PrintsHarnessImportOrigins(t *testing.T) {
	t.Parallel()

	const source = "/home/test/.codex/config.toml"

	origins := appconfig.OriginMap{
		"default_model": {
			Chain: []appconfig.OriginEvent{{
				Kind:      appconfig.OriginHarnessImport,
				Operation: appconfig.OriginSet,
				Source:    source,
				Value:     "gpt-5.5",
			}},
		},
	}

	var out strings.Builder
	writeConfigExplanationWithDiagnostics(&out, []string{source}, origins, nil, "")
	got := out.String()

	assert.Contains(t, got, "1. [harness-import] "+source)
	assert.Contains(t, got, "default_model: gpt-5.5")
	assert.Contains(t, got, "set by "+source+" [harness-import] => gpt-5.5")
}

func TestExplainConfig_PrintsHarnessImporterWarningsAndOrigins(t *testing.T) {
	tempDir := t.TempDir()
	codexHome := filepath.Join(tempDir, "codex")
	require.NoError(t, os.MkdirAll(codexHome, 0o700))
	codexConfig := filepath.Join(codexHome, "config.toml")
	require.NoError(t, os.WriteFile(codexConfig, []byte(`
model = "gpt-5.5"
model_provider = "openai"
trusted_project_roots = ["/repo"]

[model_providers.openai]
base_url = "https://openai.example"
wire_api = "responses"
`), 0o600))

	t.Setenv("HOME", tempDir)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(tempDir, "config"))
	t.Setenv("CODEX_HOME", codexHome)
	t.Setenv("OPENCODE_CONFIG", "")
	t.Setenv("OPENCODE_CONFIG_DIR", "")
	t.Setenv("FORGE_CONFIG", filepath.Join(tempDir, "missing-forge"))
	t.Setenv("ATTELER_CONFIG", "")
	t.Chdir(tempDir)

	var err error

	out := captureStdoutForStateDiagnostics(t, func() {
		err = explainConfig(cliOptions{})
	})

	require.NoError(t, err)
	assert.Contains(t, out, "Config importer warnings:")
	assert.Contains(t, out, "codex: "+codexConfig+" trusted_project_roots: ignored unsupported field")
	assert.Contains(t, out, "codex: "+codexConfig+" model_providers.openai.wire_api: ignored unsupported field")
	assert.Contains(t, out, "1. [harness-import] "+codexConfig)
	assert.Contains(t, out, "default_model: gpt-5.5")
	assert.Contains(t, out, "set by "+codexConfig+" [harness-import] => gpt-5.5")
	assert.Contains(t, out, "providers.openai.base_url: https://openai.example")
	assert.Contains(t, out, "runtime.selected_model: gpt-5.5")
}

func TestExplainConfig_PrintsHarnessImporterWarningsBeforeConfigError(t *testing.T) {
	tempDir := t.TempDir()
	t.Setenv("HOME", tempDir)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(tempDir, "config"))
	t.Setenv("CODEX_HOME", filepath.Join(tempDir, "missing-codex"))
	t.Setenv("OPENCODE_CONFIG", "")
	t.Setenv("OPENCODE_CONFIG_DIR", "")
	t.Setenv("FORGE_CONFIG", filepath.Join(tempDir, "missing-forge"))
	t.Setenv("ATTELER_CONFIG", "")
	t.Chdir(tempDir)

	claudeDir := filepath.Join(tempDir, ".claude")
	require.NoError(t, os.MkdirAll(claudeDir, 0o700))
	require.NoError(t, os.WriteFile(filepath.Join(claudeDir, "settings.json"), []byte(`{
		"model": "claude-sonnet-4-20250514",
		"unsupported": true
	}`), 0o600))

	configDir := filepath.Join(tempDir, ".atteler")
	require.NoError(t, os.MkdirAll(configDir, 0o700))
	require.NoError(t, os.WriteFile(filepath.Join(configDir, "config.yaml"), []byte(`default_model: [`), 0o600))

	var err error

	out := captureStdoutForStateDiagnostics(t, func() {
		err = explainConfig(cliOptions{})
	})

	require.Error(t, err)
	assert.Contains(t, err.Error(), "explain config:")
	assert.Contains(t, out, "Config importer warnings:")
	assert.Contains(t, out, "claude: "+filepath.Join(claudeDir, "settings.json")+" unsupported: ignored unsupported field")
	assert.NotContains(t, out, "Config explanation")
}

func TestConfigExplainPathMatches_WildcardDefaultsMatchConcreteFieldFilters(t *testing.T) {
	t.Parallel()

	assert.True(t, configExplainPathMatches("providers.*.disable_private_adapter", "providers.openai.disable_private_adapter"))
	assert.True(t, configExplainPathMatches("providers.*.disable_private_adapter", "providers.openai"))
	assert.False(t, configExplainPathMatches("providers.*.disable_private_adapter", "providers.openai.base_url"))
}
