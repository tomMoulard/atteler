package main

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestValidateConfig_PrintsHarnessImporterWarnings(t *testing.T) {
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

	out := captureStdoutForStateDiagnostics(t, func() {
		require.NoError(t, validateConfig())
	})

	assert.Contains(t, out, "Config importer warnings:")
	assert.Contains(t, out, "claude: "+filepath.Join(claudeDir, "settings.json")+" unsupported: ignored unsupported field")
	assert.Contains(t, out, "Config valid:")
}

func TestValidateConfig_PrintsMalformedHarnessImporterWarnings(t *testing.T) {
	tempDir := t.TempDir()
	codexHome := filepath.Join(tempDir, "codex")
	require.NoError(t, os.MkdirAll(codexHome, 0o700))
	codexConfig := filepath.Join(codexHome, "config.toml")
	require.NoError(t, os.WriteFile(codexConfig, []byte(`
model = "gpt-5.5"
[model_providers.openai
base_url = "https://openai.example"
`), 0o600))

	t.Setenv("HOME", tempDir)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(tempDir, "config"))
	t.Setenv("CODEX_HOME", codexHome)
	t.Setenv("OPENCODE_CONFIG", "")
	t.Setenv("OPENCODE_CONFIG_DIR", "")
	t.Setenv("FORGE_CONFIG", filepath.Join(tempDir, "missing-forge"))
	t.Setenv("ATTELER_CONFIG", "")
	t.Chdir(tempDir)

	out := captureStdoutForStateDiagnostics(t, func() {
		require.NoError(t, validateConfig())
	})

	assert.Contains(t, out, "Config importer warnings:")
	assert.Contains(t, out, "codex: "+codexConfig+": ignored harness config: parse TOML")
	assert.Contains(t, out, "Config valid: no config files loaded.")
}

func TestValidateConfig_PrintsHarnessImporterWarningsBeforeConfigError(t *testing.T) {
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
		err = validateConfig()
	})

	require.Error(t, err)
	assert.Contains(t, err.Error(), "validate config:")
	assert.Contains(t, out, "Config importer warnings:")
	assert.Contains(t, out, "claude: "+filepath.Join(claudeDir, "settings.json")+" unsupported: ignored unsupported field")
	assert.NotContains(t, out, "Config valid:")
}
