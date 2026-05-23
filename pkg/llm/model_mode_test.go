package llm

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestModelModePickerModes_OpenAIGPT54(t *testing.T) {
	t.Parallel()

	assert.Equal(t, []string{ModelModeDefault, ModelModeFast}, ModelModePickerModes(providerOpenAI, modelOpenAIGPT54))
	assert.Equal(t, []string{ModelModeDefault, ModelModeFast}, ModelModePickerModes(providerCodex, "gpt-5.4-2026-03-05"))
	assert.Equal(t, []string{ModelModeDefault}, ModelModePickerModes(providerOpenAI, modelOpenAIGPT54Mini))
	assert.Equal(t, []string{ModelModeDefault}, ModelModePickerModes(providerAnthropic, modelOpenAIGPT54))
}

func TestOpenAIServiceTierForModelMode(t *testing.T) {
	t.Parallel()

	assert.Equal(t, modelModePriority, openAIServiceTierForModelMode(ModelModeFast))
	assert.Equal(t, modelModePriority, openAIServiceTierForModelMode(modelModePriority))
	assert.Empty(t, openAIServiceTierForModelMode(ModelModeDefault))
	assert.Empty(t, openAIServiceTierForModelMode(""))
}

func TestValidateModelMode(t *testing.T) {
	t.Parallel()

	for _, mode := range []string{"", ModelModeDefault, "standard", "auto", ModelModeFast, modelModePriority} {
		require.NoError(t, validateModelMode(mode))
	}

	err := validateModelMode("turbo")
	require.Error(t, err)
	assert.Contains(t, err.Error(), `unsupported model_mode "turbo"`)
}
