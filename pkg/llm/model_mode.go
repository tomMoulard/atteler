package llm

import (
	"fmt"
	"strings"
)

const (
	// ModelModeDefault is a picker/config meta-mode meaning "use the default
	// service tier", which is represented on the wire by omitting mode-specific
	// request fields.
	ModelModeDefault = "default"
	// ModelModeFast asks supported OpenAI-family providers for their lower
	// latency processing path.
	ModelModeFast = "fast"

	modelModePriority    = "priority"
	modelOpenAIGPT54     = "gpt-5.4"
	modelOpenAIGPT54Mini = "gpt-5.4-mini"
	modelOpenAIGPT54Nano = "gpt-5.4-nano"
)

func normalizeModelMode(mode string) string {
	mode = strings.ToLower(strings.TrimSpace(mode))
	mode = strings.ReplaceAll(mode, "_", "-")

	switch mode {
	case "", ModelModeDefault, "standard", "auto":
		return ""
	case modelModePriority:
		return ModelModeFast
	default:
		return mode
	}
}

func validateModelMode(mode string) error {
	raw := strings.TrimSpace(mode)
	switch normalizeModelMode(mode) {
	case "", ModelModeFast:
		return nil
	default:
		return fmt.Errorf("unsupported model_mode %q (supported: %s, %s)", raw, ModelModeDefault, ModelModeFast)
	}
}

// ModelModePickerModes returns the modes that should be shown for a
// provider/model picker row. All rows include "default" so selecting a new
// model clears any previous session mode override; OpenAI GPT-5.4 rows also
// include "fast".
func ModelModePickerModes(provider, model string) []string {
	modes := []string{ModelModeDefault}
	if supportsOpenAIFastMode(provider, model) {
		modes = append(modes, ModelModeFast)
	}

	return modes
}

// ModelModeRank returns the display order for picker modes.
func ModelModeRank(mode string) int {
	switch normalizeModelMode(mode) {
	case "":
		return 0
	case ModelModeFast:
		return 1
	default:
		return 2
	}
}

func supportsOpenAIFastMode(provider, model string) bool {
	switch strings.ToLower(strings.TrimSpace(provider)) {
	case providerOpenAI, providerCodex:
	default:
		return false
	}

	model = strings.ToLower(strings.TrimSpace(model))
	if model == modelOpenAIGPT54 {
		return true
	}

	const snapshotPrefix = modelOpenAIGPT54 + "-"
	if !strings.HasPrefix(model, snapshotPrefix) {
		return false
	}

	suffix := strings.TrimPrefix(model, snapshotPrefix)

	return suffix != "" && suffix[0] >= '0' && suffix[0] <= '9'
}

func openAIServiceTierForModelMode(mode string) string {
	switch normalizeModelMode(mode) {
	case ModelModeFast:
		return modelModePriority
	default:
		return ""
	}
}
