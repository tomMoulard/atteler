package llm

import (
	"fmt"
	"strings"

	"github.com/tommoulard/atteler/pkg/modelroute"
)

const (
	// ModelModeDefault is a picker/config meta-mode meaning "use the default
	// service tier", which is represented on the wire by omitting mode-specific
	// request fields.
	ModelModeDefault = "default"
	// ModelModeFast asks compatible OpenAI-family models for their lower
	// latency processing path.
	ModelModeFast = "fast"

	modelModePriority       = "priority"
	modelCapabilityFastMode = "fast_mode"
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

func validateModelModeForProviderModel(providerName string, params CompleteParams) error {
	mode := normalizeModelMode(params.ModelMode)
	if mode == "" {
		return nil
	}

	if mode == ModelModeFast && !ModelSupportsFastMode(providerName, params.Model) {
		return fmt.Errorf("%s: model %q does not support model_mode %q", providerName, params.Model, ModelModeFast)
	}

	return nil
}

// ModelModePickerModes returns the modes that should be shown for a
// provider/model picker row. All rows include "default" so selecting a new
// model clears any previous session mode override; rows for model metadata
// advertising fast-mode support also include "fast".
func ModelModePickerModes(provider, model string) []string {
	modes := []string{ModelModeDefault}
	if ModelSupportsFastMode(provider, model) {
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

// ModelSupportsFastMode reports whether maintained provider/model metadata
// advertises OpenAI priority-processing compatibility.
func ModelSupportsFastMode(provider, model string) bool {
	return catalogProviderModelSupportsCapability(provider, model, modelCapabilityFastMode)
}

func catalogProviderModelSupportsCapability(provider, model, capability string) bool {
	provider = strings.ToLower(strings.TrimSpace(provider))
	model = strings.TrimSpace(model)

	if provider == "" || model == "" {
		return false
	}

	catalog := modelroute.BuiltinCatalog()
	if modelMetadataSupportsCapability(catalog, provider, model, capability) {
		return true
	}

	base := snapshotBaseModelID(model)

	if base != model && modelMetadataSupportsCapability(catalog, provider, base, capability) {
		return true
	}

	return false
}

func modelMetadataSupportsCapability(catalog modelroute.Catalog, provider, model, capability string) bool {
	metadata, ok := catalog.Lookup(provider, model)
	if !ok {
		return false
	}

	for _, candidate := range metadata.Capabilities {
		if strings.EqualFold(strings.TrimSpace(candidate), capability) {
			return true
		}
	}

	return false
}

func snapshotBaseModelID(model string) string {
	model = strings.TrimSpace(model)
	if len(model) < len("-2006-01-02")+1 {
		return model
	}

	suffix := model[len(model)-len("-2006-01-02"):]
	if suffix[0] != '-' ||
		!allDigits(suffix[1:5]) ||
		suffix[5] != '-' ||
		!allDigits(suffix[6:8]) ||
		suffix[8] != '-' ||
		!allDigits(suffix[9:11]) {
		return model
	}

	return strings.TrimSuffix(model, suffix)
}

func allDigits(value string) bool {
	if value == "" {
		return false
	}

	for _, r := range value {
		if r < '0' || r > '9' {
			return false
		}
	}

	return true
}

func openAIServiceTierForModelMode(mode string) string {
	switch normalizeModelMode(mode) {
	case ModelModeFast:
		return modelModePriority
	default:
		return ""
	}
}
