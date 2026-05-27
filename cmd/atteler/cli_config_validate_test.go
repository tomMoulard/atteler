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

func TestValidateConfig_AcceptsAllAgentLoopBudgetFields(t *testing.T) {
	tempDir := t.TempDir()
	t.Setenv("HOME", tempDir)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(tempDir, "config"))
	t.Setenv("CODEX_HOME", filepath.Join(tempDir, "missing-codex"))
	t.Setenv("OPENCODE_CONFIG", "")
	t.Setenv("OPENCODE_CONFIG_DIR", "")
	t.Setenv("FORGE_CONFIG", filepath.Join(tempDir, "missing-forge"))
	t.Setenv("ATTELER_CONFIG", "")
	t.Chdir(tempDir)

	configPath := filepath.Join(tempDir, ".atteler", "config.yaml")
	require.NoError(t, os.MkdirAll(filepath.Dir(configPath), 0o700))
	require.NoError(t, os.WriteFile(configPath, []byte(`agent_loop:
  max_output_bytes: 4096
  max_cost_micros: 250000
  max_input_tokens: 1000
  max_output_tokens: 2000
  max_total_tokens: 3000
  max_iterations: 4
  max_model_calls: 5
  max_tool_calls: 6
  max_wall_time: 30m
  checkpoint_interval: 7
`), 0o600))

	out := captureStdoutForStateDiagnostics(t, func() {
		require.NoError(t, validateConfig())
	})

	assert.Contains(t, out, "Config valid: "+configPath)
}

func TestValidateConfig_RejectsAgentLoopBudgetFields(t *testing.T) {
	for _, tc := range []struct {
		name      string
		field     string
		value     string
		wantError string
	}{
		{
			name:      "output bytes",
			field:     "max_output_bytes",
			value:     "-1",
			wantError: "agent_loop.max_output_bytes must be >= 0",
		},
		{
			name:      "cost",
			field:     "max_cost_micros",
			value:     "-1",
			wantError: "agent_loop.max_cost_micros must be >= 0",
		},
		{
			name:      "input tokens",
			field:     "max_input_tokens",
			value:     "-1",
			wantError: "agent_loop.max_input_tokens must be >= 0",
		},
		{
			name:      "output tokens",
			field:     "max_output_tokens",
			value:     "-1",
			wantError: "agent_loop.max_output_tokens must be >= 0",
		},
		{
			name:      "total tokens",
			field:     "max_total_tokens",
			value:     "-1",
			wantError: "agent_loop.max_total_tokens must be >= 0",
		},
		{
			name:      "iterations",
			field:     "max_iterations",
			value:     "-1",
			wantError: "agent_loop.max_iterations must be >= 0",
		},
		{
			name:      "model calls",
			field:     "max_model_calls",
			value:     "-1",
			wantError: "agent_loop.max_model_calls must be >= 0",
		},
		{
			name:      "tool calls",
			field:     "max_tool_calls",
			value:     "-1",
			wantError: "agent_loop.max_tool_calls must be >= 0",
		},
		{
			name:      "wall time",
			field:     "max_wall_time",
			value:     "-1s",
			wantError: "agent_loop.max_wall_time must be >= 0",
		},
		{
			name:      "checkpoint interval",
			field:     "checkpoint_interval",
			value:     "-1",
			wantError: "agent_loop.checkpoint_interval must be >= 0",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			tempDir := t.TempDir()
			t.Setenv("HOME", tempDir)
			t.Setenv("XDG_CONFIG_HOME", filepath.Join(tempDir, "config"))
			t.Setenv("CODEX_HOME", filepath.Join(tempDir, "missing-codex"))
			t.Setenv("OPENCODE_CONFIG", "")
			t.Setenv("OPENCODE_CONFIG_DIR", "")
			t.Setenv("FORGE_CONFIG", filepath.Join(tempDir, "missing-forge"))
			t.Setenv("ATTELER_CONFIG", "")
			t.Chdir(tempDir)

			configDir := filepath.Join(tempDir, ".atteler")
			require.NoError(t, os.MkdirAll(configDir, 0o700))
			require.NoError(t, os.WriteFile(filepath.Join(configDir, "config.yaml"), []byte("agent_loop:\n  "+tc.field+": "+tc.value+"\n"), 0o600))

			err := validateConfig()
			require.Error(t, err)
			assert.Contains(t, err.Error(), tc.wantError)
		})
	}
}
