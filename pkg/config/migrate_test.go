package config

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestMigrateConfigFile_RewritesLegacyFieldsAndVersion(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), "config.yaml")
	require.NoError(t, os.WriteFile(path, []byte(`
provider: openai
model: gpt-legacy
generation:
  reasoning: high
agents:
  reviewer:
    prompt: review safely
`), 0o600))

	result, err := MigrateConfigFile(path)
	require.NoError(t, err)
	assert.True(t, result.Changed)

	data, err := os.ReadFile(path)
	require.NoError(t, err)

	text := string(data)
	assert.Contains(t, text, "version: 1")
	assert.Contains(t, text, "default_provider: openai")
	assert.Contains(t, text, "default_model: gpt-legacy")
	assert.Contains(t, text, "reasoning_level: high")
	assert.Contains(t, text, "system_prompt: review safely")
	assert.NotContains(t, text, "\nprovider:")
	assert.NotContains(t, text, "\nmodel:")
	assert.NotContains(t, text, "reasoning: high")
	assert.NotContains(t, text, "\n        prompt: review safely")

	cfg, _, err := LoadFiles([]string{path})
	require.NoError(t, err)
	assert.Equal(t, ConfigSchemaVersion, cfg.Version)
	assert.Equal(t, "openai", cfg.DefaultProvider)
	assert.Equal(t, "gpt-legacy", cfg.DefaultModel)
	assert.Equal(t, "high", cfg.Generation.ReasoningLevel)
	assert.Equal(t, "review safely", cfg.Agents["reviewer"].SystemPrompt)
}

func TestMigrateConfigFile_IsIdempotent(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), "config.yaml")
	require.NoError(t, os.WriteFile(path, []byte(`
version: 1
default_model: gpt-current
`), 0o600))

	result, err := MigrateConfigFile(path)
	require.NoError(t, err)
	assert.False(t, result.Changed)
}

func TestMigrateConfigFile_EmptyIsNoop(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), "config.yaml")
	require.NoError(t, os.WriteFile(path, []byte(" \n\t"), 0o600))

	result, err := MigrateConfigFile(path)
	require.NoError(t, err)
	assert.False(t, result.Changed)
}

func TestMigrateConfigFile_PreservesJSONFormat(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), "config.json")
	require.NoError(t, os.WriteFile(path, []byte(`{"model":"gpt-json","generation":{"reasoning":"medium"}}`), 0o600))

	result, err := MigrateConfigFile(path)
	require.NoError(t, err)
	assert.True(t, result.Changed)

	data, err := os.ReadFile(path)
	require.NoError(t, err)
	assert.Contains(t, string(data), `"version": 1`)
	assert.Contains(t, string(data), `"default_model": "gpt-json"`)
	assert.Contains(t, string(data), `"reasoning_level": "medium"`)

	cfg, _, err := LoadFiles([]string{path})
	require.NoError(t, err)
	assert.Equal(t, "gpt-json", cfg.DefaultModel)
	assert.Equal(t, "medium", cfg.Generation.ReasoningLevel)
}

func TestMigrateConfigFile_PreservesUnknownFields(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), "config.yaml")
	require.NoError(t, os.WriteFile(path, []byte(`
provider: openai
future_setting:
  enabled: true
`), 0o600))

	result, err := MigrateConfigFile(path)
	require.NoError(t, err)
	assert.True(t, result.Changed)
	assertDiagnostic(t, result.Diagnostics, DiagnosticError, "future_setting", "")

	data, err := os.ReadFile(path)
	require.NoError(t, err)
	assert.Contains(t, string(data), "default_provider: openai")
	assert.Contains(t, string(data), "future_setting:")
	assert.Contains(t, string(data), "enabled: true")
}

func TestMigrateConfigFile_RejectsFutureVersion(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), "config.yaml")
	require.NoError(t, os.WriteFile(path, []byte("version: 99\n"), 0o600))

	result, err := MigrateConfigFile(path)
	require.Error(t, err)
	assert.False(t, result.Changed)
	assert.Contains(t, err.Error(), "unsupported version 99")
}

func TestMigrateConfigFile_RejectsNegativeVersion(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), "config.yaml")
	require.NoError(t, os.WriteFile(path, []byte("version: -1\n"), 0o600))

	result, err := MigrateConfigFile(path)
	require.Error(t, err)
	assert.False(t, result.Changed)
	assert.Contains(t, err.Error(), "unsupported version -1")
}
