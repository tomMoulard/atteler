package llm

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestModelModePickerModes_FastModeFromCatalogCapabilities(t *testing.T) {
	t.Parallel()

	assert.Equal(t, []string{ModelModeDefault, ModelModeFast}, ModelModePickerModes(providerOpenAI, "gpt-5.5"))
	assert.Equal(t, []string{ModelModeDefault, ModelModeFast}, ModelModePickerModes(providerOpenAI, "gpt-5.4"))
	assert.Equal(t, []string{ModelModeDefault, ModelModeFast}, ModelModePickerModes(providerOpenAI, "gpt-5.4-mini"))
	assert.Equal(t, []string{ModelModeDefault, ModelModeFast}, ModelModePickerModes(providerOpenAI, "gpt-5.2"))
	assert.Equal(t, []string{ModelModeDefault, ModelModeFast}, ModelModePickerModes(providerOpenAI, "gpt-5.1"))
	assert.Equal(t, []string{ModelModeDefault, ModelModeFast}, ModelModePickerModes(providerOpenAI, "gpt-5"))
	assert.Equal(t, []string{ModelModeDefault, ModelModeFast}, ModelModePickerModes(providerOpenAI, "gpt-5-mini"))
	assert.Equal(t, []string{ModelModeDefault, ModelModeFast}, ModelModePickerModes(providerOpenAI, "gpt-5.3-codex"))
	assert.Equal(t, []string{ModelModeDefault, ModelModeFast}, ModelModePickerModes(providerOpenAI, "gpt-5.1-codex"))
	assert.Equal(t, []string{ModelModeDefault, ModelModeFast}, ModelModePickerModes(providerOpenAI, "gpt-5-codex"))
	assert.Equal(t, []string{ModelModeDefault, ModelModeFast}, ModelModePickerModes(providerOpenAI, "gpt-4.1"))
	assert.Equal(t, []string{ModelModeDefault, ModelModeFast}, ModelModePickerModes(providerOpenAI, "gpt-4.1-mini"))
	assert.Equal(t, []string{ModelModeDefault, ModelModeFast}, ModelModePickerModes(providerOpenAI, "gpt-4.1-nano"))
	assert.Equal(t, []string{ModelModeDefault, ModelModeFast}, ModelModePickerModes(providerOpenAI, "gpt-4o"))
	assert.Equal(t, []string{ModelModeDefault, ModelModeFast}, ModelModePickerModes(providerOpenAI, "gpt-4o-mini"))
	assert.Equal(t, []string{ModelModeDefault, ModelModeFast}, ModelModePickerModes(providerOpenAI, "o3"))
	assert.Equal(t, []string{ModelModeDefault, ModelModeFast}, ModelModePickerModes(providerOpenAI, "o4-mini"))
	assert.Equal(t, []string{ModelModeDefault, ModelModeFast}, ModelModePickerModes(providerCodex, "gpt-5.3-codex"))
	assert.Equal(t, []string{ModelModeDefault}, ModelModePickerModes(providerOpenAI, "gpt-5.4-nano"))
}

func TestModelSupportsFastMode_RecognizesSnapshotIDs(t *testing.T) {
	t.Parallel()

	assert.True(t, ModelSupportsFastMode(providerOpenAI, "gpt-5.5-2026-05-28"))
	assert.True(t, ModelSupportsFastMode(providerOpenAI, "gpt-5.2-2025-12-11"))
	assert.True(t, ModelSupportsFastMode(providerOpenAI, "gpt-5.1-2025-11-13"))
	assert.True(t, ModelSupportsFastMode(providerOpenAI, "gpt-5-2025-08-07"))
	assert.True(t, ModelSupportsFastMode(providerOpenAI, "gpt-5-mini-2025-08-07"))
	assert.True(t, ModelSupportsFastMode(providerOpenAI, "gpt-4.1-2025-04-14"))
	assert.True(t, ModelSupportsFastMode(providerOpenAI, "gpt-4o-2024-11-20"))
	assert.True(t, ModelSupportsFastMode(providerCodex, "gpt-5.3-codex-2026-05-28"))
	assert.False(t, ModelSupportsFastMode(providerOpenAI, "gpt-5.4-nano-2026-05-28"))
}

func TestModelSupportsFastMode_RecognizesPriorityProcessingMetadata(t *testing.T) {
	t.Parallel()

	for _, model := range []string{
		"gpt-5.5",
		"gpt-5.4",
		"gpt-5.4-mini",
		"gpt-5.2",
		"gpt-5.1",
		"gpt-5",
		"gpt-5-mini",
		"gpt-5.3-codex",
		"gpt-5.3-codex-2026-05-28",
		"gpt-5.1-codex",
		"gpt-5-codex",
		"gpt-4.1",
		"gpt-4.1-mini",
		"gpt-4.1-nano",
		"gpt-4o",
		"gpt-4o-mini",
		"o3",
		"o4-mini",
	} {
		assert.True(t, ModelSupportsFastMode(providerOpenAI, model), model)
	}

	assert.False(t, ModelSupportsFastMode(providerOpenAI, "gpt-5.4-nano"))
	assert.False(t, ModelSupportsFastMode(providerAnthropic, "claude-opus-4-7"))
	assert.False(t, ModelSupportsFastMode(providerOpenAI, "gpt-4-turbo"))
}

func TestNormalizeModelMode(t *testing.T) {
	t.Parallel()

	assert.Empty(t, normalizeModelMode(""))
	assert.Empty(t, normalizeModelMode("default"))
	assert.Equal(t, ModelModeFast, normalizeModelMode("FAST"))
	assert.Equal(t, ModelModeFast, normalizeModelMode("priority"))
}

func TestValidateModelModeForProviderModel(t *testing.T) {
	t.Parallel()

	require.NoError(t, validateModelModeForProviderModel(providerOpenAI, CompleteParams{Model: "gpt-5.5", ModelMode: ModelModeFast}))
	assert.ErrorContains(t, validateModelModeForProviderModel(providerOpenAI, CompleteParams{Model: "gpt-5.4-nano", ModelMode: ModelModeFast}), "does not support")
}
