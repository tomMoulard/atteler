package llm

import (
	"encoding/json"
	"fmt"
	"maps"
	"math"
	"strings"

	"github.com/tommoulard/atteler/pkg/modelroute"
)

// CompleteParamSupportStatus describes whether a provider preserves a common
// completion parameter in its wire protocol.
type CompleteParamSupportStatus string

const (
	// CompleteParamSupported means the provider has a direct contract for the
	// parameter.
	CompleteParamSupported CompleteParamSupportStatus = "supported"

	// CompleteParamUnsupported means the provider has no safe equivalent and
	// must reject a non-zero value instead of silently dropping it.
	CompleteParamUnsupported CompleteParamSupportStatus = "unsupported"

	// CompleteParamOmitted means the provider cannot honor the parameter but the
	// adapter intentionally omits it from the wire request to keep shared
	// generation options portable across fallback providers.
	CompleteParamOmitted CompleteParamSupportStatus = "omitted"

	// CompleteParamLossy means the provider has an intentional approximation
	// documented in the support note.
	CompleteParamLossy CompleteParamSupportStatus = "lossy"
)

// CompleteParamSupport documents how a provider maps one CompleteParams field.
type CompleteParamSupport struct {
	Status CompleteParamSupportStatus `json:"status"`
	Note   string                     `json:"note,omitempty"`
}

// ProviderCapabilities describes provider wire-contract features that callers
// can inspect before setting provider-specific knobs. SupportsStreaming refers
// to caller-facing StreamProvider support, not internal provider transport.
type ProviderCapabilities struct {
	CompleteParams                map[string]CompleteParamSupport `json:"complete_params"`
	SupportsChatCompletions       bool                            `json:"supports_chat_completions"`
	SupportsSeed                  bool                            `json:"supports_seed"`
	SupportsTools                 bool                            `json:"supports_tools"`
	SupportsReasoning             bool                            `json:"supports_reasoning"`
	SupportsModelMode             bool                            `json:"supports_model_mode"`
	SupportsJSONSchema            bool                            `json:"supports_json_schema"`
	SupportsEmbeddings            bool                            `json:"supports_embeddings"`
	SupportsMultimodalInput       bool                            `json:"supports_multimodal_input"`
	SupportsMultimodalOutput      bool                            `json:"supports_multimodal_output"`
	SupportsBatch                 bool                            `json:"supports_batch"`
	SupportsPromptCaching         bool                            `json:"supports_prompt_caching"`
	SupportsCacheAccounting       bool                            `json:"supports_cache_accounting"`
	SupportsStreaming             bool                            `json:"supports_streaming"`
	SupportsNetworkModelDiscovery bool                            `json:"supports_network_model_discovery"`
	SupportsRateLimitMetadata     bool                            `json:"supports_rate_limit_metadata"`
	SupportsRetries               bool                            `json:"supports_retries"`
	SupportsFallbacks             bool                            `json:"supports_fallbacks"`
	SupportsCostTracking          bool                            `json:"supports_cost_tracking"`
}

// ProviderCapabilitiesFor returns the provider's declared capabilities. Custom
// providers that do not expose metadata get an empty capability record.
func ProviderCapabilitiesFor(provider Provider) ProviderCapabilities {
	if provider == nil {
		return ProviderCapabilities{}
	}

	if p, ok := provider.(interface{ Capabilities() ProviderCapabilities }); ok {
		return p.Capabilities()
	}

	if capabilities, ok := BuiltInProviderCapabilities(provider.Name()); ok {
		return capabilities
	}

	return ProviderCapabilities{}
}

// BuiltInProviderCapabilities returns the static capability metadata for a
// built-in provider name.
func BuiltInProviderCapabilities(providerName string) (ProviderCapabilities, bool) {
	capabilities, ok := builtInProviderCapabilities[providerName]
	if !ok {
		return ProviderCapabilities{}, false
	}

	return cloneProviderCapabilities(capabilities), true
}

// Capabilities returns Anthropic's provider protocol metadata.
func (a *AnthropicProvider) Capabilities() ProviderCapabilities {
	capabilities, _ := BuiltInProviderCapabilities(providerAnthropic)
	return capabilities
}

// Capabilities returns Claude Code's provider protocol metadata.
func (c *ClaudeCodeProvider) Capabilities() ProviderCapabilities {
	capabilities, _ := BuiltInProviderCapabilities(providerClaudeCode)
	return capabilities
}

// Capabilities returns Codex's provider protocol metadata.
func (c *CodexProvider) Capabilities() ProviderCapabilities {
	capabilities, _ := BuiltInProviderCapabilities(providerCodex)
	return capabilities
}

// Capabilities returns OpenAI's provider protocol metadata.
func (o *OpenAIProvider) Capabilities() ProviderCapabilities {
	capabilities, _ := BuiltInProviderCapabilities(providerOpenAI)

	if o.Name() != providerOpenAI && len(o.capabilities) > 0 {
		capabilities = providerCapabilitiesForRouteCapabilities(capabilities, o.capabilities)
	}

	// The current OpenAI-compatible adapter exposes buffered chat and embedding
	// calls only. A custom endpoint may support streaming on the wire, but routing
	// must advertise caller-facing streaming only once this provider implements
	// StreamProvider; otherwise a role requiring streaming could silently receive
	// a buffered response.
	capabilities.SupportsStreaming = false

	if o.Name() != providerOpenAI && strings.TrimSpace(o.effectiveModelsPath()) == "" {
		capabilities.SupportsNetworkModelDiscovery = false
	}

	return capabilities
}

// Capabilities returns Ollama's provider protocol metadata.
func (o *OllamaProvider) Capabilities() ProviderCapabilities {
	capabilities, _ := BuiltInProviderCapabilities(providerOllama)
	return capabilities
}

var builtInProviderCapabilities = map[string]ProviderCapabilities{
	providerOpenAI: {
		SupportsChatCompletions:       true,
		SupportsSeed:                  true,
		SupportsTools:                 true,
		SupportsReasoning:             true,
		SupportsModelMode:             true,
		SupportsJSONSchema:            true,
		SupportsEmbeddings:            true,
		SupportsMultimodalInput:       true,
		SupportsMultimodalOutput:      true,
		SupportsBatch:                 true,
		SupportsPromptCaching:         true,
		SupportsCacheAccounting:       true,
		SupportsStreaming:             false,
		SupportsNetworkModelDiscovery: true,
		SupportsRateLimitMetadata:     true,
		SupportsRetries:               true,
		SupportsFallbacks:             true,
		SupportsCostTracking:          true,
		CompleteParams: map[string]CompleteParamSupport{
			"Temperature":    supported("maps to chat.completions temperature"),
			"TopP":           supported("maps to chat.completions top_p"),
			"Seed":           supported("maps to chat.completions seed"),
			"ResponseFormat": supported("maps to chat.completions response_format"),
			"Model":          supported("maps to model"),
			"ModelMode":      supported("fast maps to service_tier=priority when model metadata supports fast mode"),
			"ReasoningLevel": supported("maps to reasoning_effort"),
			"Messages":       lossy("maps to chat.completions messages; ToolResult.IsError is not represented"),
			"Stop":           supported("maps to stop"),
			"Tools":          supported("maps to function tools"),
			"MaxTokens":      supported("maps to max_tokens"),
		},
	},
	providerAnthropic: {
		SupportsChatCompletions:       true,
		SupportsSeed:                  false,
		SupportsTools:                 true,
		SupportsReasoning:             true,
		SupportsModelMode:             false,
		SupportsJSONSchema:            false,
		SupportsEmbeddings:            false,
		SupportsMultimodalInput:       true,
		SupportsMultimodalOutput:      false,
		SupportsBatch:                 true,
		SupportsPromptCaching:         true,
		SupportsCacheAccounting:       true,
		SupportsStreaming:             false,
		SupportsNetworkModelDiscovery: true,
		SupportsRateLimitMetadata:     true,
		SupportsRetries:               true,
		SupportsFallbacks:             true,
		SupportsCostTracking:          true,
		CompleteParams: map[string]CompleteParamSupport{
			"Temperature":    supported("maps to messages temperature; coerced to 1 when thinking is enabled"),
			"TopP":           supported("maps to messages top_p"),
			"Seed":           unsupported("Anthropic Messages has no seed parameter"),
			"ResponseFormat": unsupported("Anthropic Messages has no provider-native JSON/schema response_format parameter"),
			"Model":          supported("maps to model"),
			"ModelMode":      unsupported("Anthropic Messages has no OpenAI priority-processing model mode"),
			"ReasoningLevel": lossy("maps Atteler levels to Anthropic thinking token budgets"),
			"Messages":       lossy("system messages are lifted to system; tool results become user content blocks"),
			"Stop":           supported("maps to stop_sequences"),
			"Tools":          supported("maps to tools input_schema"),
			"MaxTokens":      supported("maps to max_tokens; defaults to 4096 when unset"),
		},
	},
	providerClaudeCode: {
		SupportsChatCompletions:       true,
		SupportsSeed:                  false,
		SupportsTools:                 true,
		SupportsReasoning:             true,
		SupportsModelMode:             false,
		SupportsJSONSchema:            false,
		SupportsEmbeddings:            false,
		SupportsMultimodalInput:       true,
		SupportsMultimodalOutput:      false,
		SupportsBatch:                 false,
		SupportsPromptCaching:         true,
		SupportsCacheAccounting:       true,
		SupportsStreaming:             false,
		SupportsNetworkModelDiscovery: false,
		SupportsRateLimitMetadata:     true,
		SupportsRetries:               true,
		SupportsFallbacks:             true,
		SupportsCostTracking:          true,
		CompleteParams: map[string]CompleteParamSupport{
			"Temperature":    supported("maps to Anthropic Messages temperature; coerced to 1 when thinking is enabled"),
			"TopP":           supported("maps to Anthropic Messages top_p"),
			"Seed":           unsupported("Claude Code OAuth path uses Anthropic Messages, which has no seed parameter"),
			"ResponseFormat": unsupported("Claude Code OAuth path uses Anthropic Messages, which has no provider-native JSON/schema response_format parameter"),
			"Model":          supported("maps to model"),
			"ModelMode":      unsupported("Claude Code OAuth path uses Anthropic Messages, which has no OpenAI priority-processing model mode"),
			"ReasoningLevel": lossy("maps Atteler levels to Anthropic thinking token budgets"),
			"Messages":       lossy("system messages are lifted to system; tool results become user content blocks"),
			"Stop":           supported("maps to stop_sequences"),
			"Tools":          supported("maps to tools input_schema"),
			"MaxTokens":      supported("maps to max_tokens; defaults to 4096 when unset"),
		},
	},
	providerCodex: {
		SupportsChatCompletions:       true,
		SupportsSeed:                  false,
		SupportsTools:                 true,
		SupportsReasoning:             true,
		SupportsModelMode:             true,
		SupportsJSONSchema:            true,
		SupportsEmbeddings:            false,
		SupportsMultimodalInput:       true,
		SupportsMultimodalOutput:      false,
		SupportsBatch:                 false,
		SupportsPromptCaching:         true,
		SupportsCacheAccounting:       true,
		SupportsStreaming:             true,
		SupportsNetworkModelDiscovery: false,
		SupportsRateLimitMetadata:     true,
		SupportsRetries:               true,
		SupportsFallbacks:             true,
		SupportsCostTracking:          true,
		CompleteParams: map[string]CompleteParamSupport{
			"Temperature":    omitted("Codex ChatGPT responses endpoint does not expose temperature in this adapter"),
			"TopP":           unsupported("Codex ChatGPT responses endpoint does not expose top_p in this adapter"),
			"Seed":           unsupported("Codex ChatGPT responses endpoint does not expose seed in this adapter"),
			"ResponseFormat": supported("maps to Responses text.format"),
			"Model":          supported("maps to model"),
			"ModelMode":      supported("fast maps to Responses service_tier=priority when model metadata supports fast mode"),
			"ReasoningLevel": supported("maps to responses reasoning.effort"),
			"Messages":       lossy("system messages become instructions; tool calls/results become Responses input items; ToolResult.IsError is not represented"),
			"Stop":           unsupported("Codex ChatGPT responses endpoint does not expose stop sequences in this adapter"),
			"Tools":          supported("maps to Responses function tools"),
			"MaxTokens":      omitted("Codex ChatGPT responses endpoint does not expose max output tokens in this adapter"),
		},
	},
	providerOllama: {
		SupportsChatCompletions:       true,
		SupportsSeed:                  true,
		SupportsTools:                 true,
		SupportsReasoning:             true,
		SupportsModelMode:             false,
		SupportsJSONSchema:            true,
		SupportsEmbeddings:            true,
		SupportsMultimodalInput:       true,
		SupportsMultimodalOutput:      false,
		SupportsBatch:                 false,
		SupportsPromptCaching:         false,
		SupportsCacheAccounting:       false,
		SupportsStreaming:             true,
		SupportsNetworkModelDiscovery: true,
		SupportsRateLimitMetadata:     true,
		SupportsRetries:               true,
		SupportsFallbacks:             true,
		SupportsCostTracking:          true,
		CompleteParams: map[string]CompleteParamSupport{
			"Temperature":    supported("maps to options.temperature"),
			"TopP":           supported("maps to options.top_p"),
			"Seed":           supported("maps to options.seed"),
			"ResponseFormat": lossy("maps json_object to format=json and json_schema to format schema; name/strict are not represented"),
			"Model":          supported("maps to model"),
			"ModelMode":      unsupported("Ollama chat has no OpenAI priority-processing model mode"),
			"ReasoningLevel": lossy("maps Atteler levels to Ollama think false/low/medium/high"),
			"Messages":       lossy("tool-call IDs, tool-result IDs, and ToolResult.IsError are not represented in Ollama chat messages"),
			"Stop":           supported("maps to options.stop"),
			"Tools":          supported("maps to function tools"),
			"MaxTokens":      supported("maps to options.num_predict"),
		},
	},
}

func supported(note string) CompleteParamSupport {
	return CompleteParamSupport{Status: CompleteParamSupported, Note: note}
}

func unsupported(note string) CompleteParamSupport {
	return CompleteParamSupport{Status: CompleteParamUnsupported, Note: note}
}

func omitted(note string) CompleteParamSupport {
	return CompleteParamSupport{Status: CompleteParamOmitted, Note: note}
}

func lossy(note string) CompleteParamSupport {
	return CompleteParamSupport{Status: CompleteParamLossy, Note: note}
}

func cloneProviderCapabilities(capabilities ProviderCapabilities) ProviderCapabilities {
	capabilities.CompleteParams = cloneCompleteParamSupport(capabilities.CompleteParams)
	return capabilities
}

func cloneCompleteParamSupport(in map[string]CompleteParamSupport) map[string]CompleteParamSupport {
	if len(in) == 0 {
		return nil
	}

	out := make(map[string]CompleteParamSupport, len(in))
	maps.Copy(out, in)

	return out
}

func validateCompleteParamsSupported(providerName string, params CompleteParams) error {
	capabilities, ok := BuiltInProviderCapabilities(providerName)
	if !ok {
		return validateCompleteParamsWireSafe(providerName, params)
	}

	return validateCompleteParamsAgainstCapabilities(providerName, capabilities, params)
}

func validateCompleteParamsAgainstCapabilities(
	providerName string,
	capabilities ProviderCapabilities,
	params CompleteParams,
) error {
	if !capabilities.SupportsChatCompletions {
		return fmt.Errorf("%s: chat completions are unsupported by provider capabilities", providerName)
	}

	if params.MaxTokens < 0 {
		return fmt.Errorf("%s: CompleteParams.MaxTokens must be non-negative, got %d", providerName, params.MaxTokens)
	}

	if err := validateCompleteParamsWireSafe(providerName, params); err != nil {
		return err
	}

	if err := validateModelMode(params.ModelMode); err != nil {
		return fmt.Errorf("%s: %w", providerName, err)
	}

	checks := []struct {
		name string
		set  bool
	}{
		{name: "Temperature", set: params.Temperature != nil},
		{name: "TopP", set: params.TopP != nil},
		{name: "Seed", set: params.Seed != nil},
		{name: "ResponseFormat", set: responseFormatRequested(params.ResponseFormat)},
		{name: "Model", set: strings.TrimSpace(params.Model) != ""},
		{name: "ModelMode", set: normalizeModelMode(params.ModelMode) != ""},
		{name: "ReasoningLevel", set: reasoningCapabilityRequested(params.ReasoningLevel)},
		{name: "Messages", set: len(params.Messages) > 0},
		{name: "Stop", set: len(params.Stop) > 0},
		{name: "Tools", set: len(params.Tools) > 0},
		{name: "MaxTokens", set: params.MaxTokens > 0},
	}

	for _, check := range checks {
		if !check.set {
			continue
		}

		support, ok := completeParamSupportForCapabilities(capabilities, check.name)
		if !ok || support.Status != CompleteParamUnsupported {
			continue
		}

		return fmt.Errorf("%s: CompleteParams.%s is unsupported: %s", providerName, check.name, support.Note)
	}

	if err := validateModelModeForProviderModel(providerName, params); err != nil {
		return err
	}

	return nil
}

func validateCompleteParamsAgainstDeclaredCapabilities(provider Provider, params CompleteParams) error {
	if provider == nil {
		return nil
	}

	capabilitiesProvider, ok := provider.(interface{ Capabilities() ProviderCapabilities })
	if !ok {
		return nil
	}

	return validateCompleteParamsAgainstCapabilities(provider.Name(), capabilitiesProvider.Capabilities(), params)
}

func completeParamSupportForCapabilities(capabilities ProviderCapabilities, name string) (CompleteParamSupport, bool) {
	if support, ok := capabilities.CompleteParams[name]; ok {
		return support, true
	}

	switch name {
	case "Seed":
		if !capabilities.SupportsSeed {
			return unsupported("provider capability metadata does not include seed"), true
		}
	case "ResponseFormat":
		if !capabilities.SupportsJSONSchema {
			return unsupported("provider capability metadata does not include json_schema"), true
		}
	case "ModelMode":
		if !capabilities.SupportsModelMode {
			return unsupported("provider capability metadata does not include model_mode"), true
		}
	case "ReasoningLevel":
		if !capabilities.SupportsReasoning {
			return unsupported("provider capability metadata does not include reasoning"), true
		}
	case "Messages":
		if !capabilities.SupportsChatCompletions {
			return unsupported("provider capability metadata does not include chat"), true
		}
	case "Tools":
		if !capabilities.SupportsTools {
			return unsupported("provider capability metadata does not include tools"), true
		}
	}

	return CompleteParamSupport{}, false
}

func providerCapabilitiesForRouteCapabilities(
	base ProviderCapabilities,
	routeCapabilities []string,
) ProviderCapabilities {
	set := routeCapabilitySet(routeCapabilities)
	capabilities := cloneProviderCapabilities(base)

	capabilities.SupportsChatCompletions = set[modelroute.CapabilityChat] || set[modelroute.CapabilityText]
	capabilities.SupportsTools = set[modelroute.CapabilityTools]
	capabilities.SupportsReasoning = set[modelroute.CapabilityReasoning]
	capabilities.SupportsModelMode = set[modelroute.CapabilityFastMode]
	capabilities.SupportsJSONSchema = set[modelroute.CapabilityJSONSchema]
	capabilities.SupportsEmbeddings = set[modelroute.CapabilityEmbeddings]
	capabilities.SupportsMultimodalInput = set[modelroute.CapabilityVision] || set[modelroute.CapabilityMultimodal]
	capabilities.SupportsMultimodalOutput = set[modelroute.CapabilityMultimodal]
	capabilities.SupportsBatch = set[modelroute.CapabilityBatch]
	capabilities.SupportsPromptCaching = set[modelroute.CapabilityPromptCache]
	capabilities.SupportsCacheAccounting = set[modelroute.CapabilityPromptCache]
	capabilities.SupportsStreaming = set[modelroute.CapabilityStreaming]
	capabilities.SupportsRateLimitMetadata = set[modelroute.CapabilityRateLimits]
	capabilities.SupportsRetries = set[modelroute.CapabilityRetries]
	capabilities.SupportsFallbacks = set[modelroute.CapabilityFallback]
	capabilities.SupportsCostTracking = set[modelroute.CapabilityCostTracking]

	setCapabilityParamSupport(&capabilities, "Tools", capabilities.SupportsTools, "provider capability override does not include tools")
	setCapabilityParamSupport(&capabilities, "ReasoningLevel", capabilities.SupportsReasoning, "provider capability override does not include reasoning")
	setCapabilityParamSupport(&capabilities, "ModelMode", capabilities.SupportsModelMode, "provider capability override does not include fast_mode")
	setCapabilityParamSupport(&capabilities, "ResponseFormat", capabilities.SupportsJSONSchema, "provider capability override does not include json_schema")
	setCapabilityParamSupport(&capabilities, "Messages", capabilities.SupportsChatCompletions, "provider capability override does not include chat")

	return capabilities
}

func setCapabilityParamSupport(capabilities *ProviderCapabilities, name string, enabled bool, disabledNote string) {
	if capabilities.CompleteParams == nil {
		capabilities.CompleteParams = make(map[string]CompleteParamSupport)
	}

	if enabled {
		if _, ok := capabilities.CompleteParams[name]; !ok || capabilities.CompleteParams[name].Status == CompleteParamUnsupported {
			capabilities.CompleteParams[name] = supported("enabled by provider capability override")
		}

		return
	}

	capabilities.CompleteParams[name] = unsupported(disabledNote)
}

func routeCapabilitySet(values []string) map[string]bool {
	set := make(map[string]bool, len(values))
	for _, value := range cleanCapabilityList(values) {
		set[value] = true
	}

	return set
}

func cleanCapabilityList(values []string) []string {
	out := make([]string, 0, len(values))
	seen := make(map[string]bool, len(values))

	for _, value := range values {
		value = strings.ToLower(strings.TrimSpace(value))
		if value == "" || seen[value] {
			continue
		}

		seen[value] = true
		out = append(out, value)
	}

	return out
}

func validateKnownRouteCapabilities(path string, capabilities []string) error {
	for _, capability := range capabilities {
		capability = strings.ToLower(strings.TrimSpace(capability))
		if capability == "" || modelroute.IsKnownCapability(capability) {
			continue
		}

		return fmt.Errorf(
			"%s contains unknown capability %q (valid: %s)",
			path,
			capability,
			strings.Join(modelroute.KnownCapabilities(), ","),
		)
	}

	return nil
}

func capabilityListContains(values []string, target string) bool {
	target = strings.ToLower(strings.TrimSpace(target))
	if target == "" {
		return false
	}

	for _, value := range values {
		if strings.ToLower(strings.TrimSpace(value)) == target {
			return true
		}
	}

	return false
}

func validateCompleteParamsWireSafe(providerName string, params CompleteParams) error {
	if params.Temperature != nil && !isFiniteFloat(*params.Temperature) {
		return fmt.Errorf(
			"%s: CompleteParams.Temperature must be finite, got %v",
			providerName,
			*params.Temperature,
		)
	}

	if params.TopP != nil && !isFiniteFloat(*params.TopP) {
		return fmt.Errorf(
			"%s: CompleteParams.TopP must be finite, got %v",
			providerName,
			*params.TopP,
		)
	}

	if _, err := normalizeResponseFormat(params.ResponseFormat); err != nil {
		return fmt.Errorf("%s: CompleteParams.ResponseFormat: %w", providerName, err)
	}

	for i, tool := range params.Tools {
		if _, err := json.Marshal(tool.Parameters); err != nil {
			return fmt.Errorf(
				"%s: CompleteParams.Tools[%d].Parameters must be JSON-serializable: %w",
				providerName,
				i,
				err,
			)
		}
	}

	for i, message := range params.Messages {
		for j, toolCall := range message.ToolCalls {
			if _, err := json.Marshal(toolCall.Input); err != nil {
				return fmt.Errorf(
					"%s: CompleteParams.Messages[%d].ToolCalls[%d].Input must be JSON-serializable: %w",
					providerName,
					i,
					j,
					err,
				)
			}
		}
	}

	return nil
}

func responseFormatRequested(format *ResponseFormat) bool {
	_, kind, err := normalizeResponseFormatKind(format)

	return err != nil || kind != ""
}

func normalizeResponseFormat(format *ResponseFormat) (ResponseFormat, error) {
	normalized, kind, err := normalizeResponseFormatKind(format)
	if err != nil || kind == "" {
		if err == nil && (normalized.Name != "" || normalized.Strict) {
			return normalized, fmt.Errorf("name/strict require %s", ResponseFormatJSONSchema)
		}

		return normalized, err
	}

	if kind == ResponseFormatJSONSchema {
		if normalized.Schema == nil {
			return normalized, fmt.Errorf("%s requires a non-nil schema", ResponseFormatJSONSchema)
		}

		if _, err := json.Marshal(normalized.Schema); err != nil {
			return normalized, fmt.Errorf("schema must be JSON-serializable: %w", err)
		}

		if strings.TrimSpace(normalized.Name) == "" {
			normalized.Name = "response"
		}
	}

	if kind == ResponseFormatJSONObject && normalized.Schema != nil {
		return normalized, fmt.Errorf("schema requires %s", ResponseFormatJSONSchema)
	}

	if kind == ResponseFormatJSONObject && (normalized.Name != "" || normalized.Strict) {
		return normalized, fmt.Errorf("name/strict require %s", ResponseFormatJSONSchema)
	}

	normalized.Type = kind

	return normalized, nil
}

func normalizeResponseFormatKind(format *ResponseFormat) (ResponseFormat, string, error) {
	if format == nil {
		return ResponseFormat{}, "", nil
	}

	normalized := *format
	normalized.Type = strings.ToLower(strings.TrimSpace(normalized.Type))
	normalized.Name = strings.TrimSpace(normalized.Name)

	switch normalized.Type {
	case "", ResponseFormatText:
		if normalized.Schema != nil {
			return normalized, ResponseFormatJSONSchema, nil
		}

		return normalized, "", nil
	case "json", "object", ResponseFormatJSONObject:
		return normalized, ResponseFormatJSONObject, nil
	case "schema", ResponseFormatJSONSchema:
		return normalized, ResponseFormatJSONSchema, nil
	default:
		return normalized, "", fmt.Errorf("unsupported type %q", format.Type)
	}
}

func isFiniteFloat(value float64) bool {
	return !math.IsNaN(value) && !math.IsInf(value, 0)
}

type completeParamAdjustment struct {
	Name   string
	Action string
	Reason string
}

func prepareCompleteParamsForProvider(
	providerName string,
	params CompleteParams,
) (CompleteParams, []completeParamAdjustment, error) {
	capabilities, ok := BuiltInProviderCapabilities(providerName)
	if !ok {
		if err := validateCompleteParamsWireSafe(providerName, params); err != nil {
			return params, nil, err
		}

		return params, nil, nil
	}

	return prepareCompleteParamsForProviderCapabilities(providerName, capabilities, params)
}

func prepareCompleteParamsForProviderCapabilities(
	providerName string,
	capabilities ProviderCapabilities,
	params CompleteParams,
) (CompleteParams, []completeParamAdjustment, error) {
	return prepareCompleteParamsForProviderCapabilitiesMode(providerName, capabilities, params, false)
}

func prepareRoutedCompleteParamsForProviderCapabilities(
	providerName string,
	capabilities ProviderCapabilities,
	params CompleteParams,
) (CompleteParams, []completeParamAdjustment, error) {
	return prepareCompleteParamsForProviderCapabilitiesMode(providerName, capabilities, params, true)
}

func prepareCompleteParamsForProviderCapabilitiesMode(
	providerName string,
	capabilities ProviderCapabilities,
	params CompleteParams,
	omitImpossiblePortableReasoning bool,
) (CompleteParams, []completeParamAdjustment, error) {
	if err := validateCompleteParamsWireSafe(providerName, params); err != nil {
		return params, nil, err
	}

	params, adjustments := normalizeCompleteParamsForProvider(
		providerName,
		capabilities,
		params,
		omitImpossiblePortableReasoning,
	)

	return params, adjustments, nil
}

func normalizeCompleteParamsForProvider(
	providerName string,
	capabilities ProviderCapabilities,
	params CompleteParams,
	omitImpossiblePortableReasoning bool,
) (CompleteParams, []completeParamAdjustment) {
	var adjustments []completeParamAdjustment

	adjustments = appendOmittedCompleteParamAdjustment(
		adjustments,
		capabilities,
		"Temperature",
		params.Temperature != nil,
		func() { params.Temperature = nil },
		"provider omits temperature",
	)
	adjustments = appendOmittedCompleteParamAdjustment(
		adjustments,
		capabilities,
		"TopP",
		params.TopP != nil,
		func() { params.TopP = nil },
		"provider omits top_p",
	)
	adjustments = appendOmittedCompleteParamAdjustment(
		adjustments,
		capabilities,
		"Seed",
		params.Seed != nil,
		func() { params.Seed = nil },
		"provider omits seed",
	)
	adjustments = appendOmittedCompleteParamAdjustment(
		adjustments,
		capabilities,
		"ResponseFormat",
		responseFormatRequested(params.ResponseFormat),
		func() {
			params.ResponseFormat = nil
		},
		"provider omits response_format",
	)
	adjustments = appendOmittedCompleteParamAdjustment(
		adjustments,
		capabilities,
		"ReasoningLevel",
		reasoningCapabilityRequested(params.ReasoningLevel),
		func() {
			params.ReasoningLevel = ""
		},
		"provider omits reasoning level",
	)
	adjustments = appendOmittedCompleteParamAdjustment(
		adjustments,
		capabilities,
		"Stop",
		len(params.Stop) > 0,
		func() { params.Stop = nil },
		"provider omits stop sequences",
	)
	adjustments = appendOmittedCompleteParamAdjustment(
		adjustments,
		capabilities,
		"Tools",
		len(params.Tools) > 0,
		func() { params.Tools = nil },
		"provider omits tools",
	)
	adjustments = appendOmittedCompleteParamAdjustment(
		adjustments,
		capabilities,
		"MaxTokens",
		params.MaxTokens > 0,
		func() { params.MaxTokens = 0 },
		"provider omits max tokens",
	)

	if omitImpossiblePortableReasoning {
		params, adjustments = appendAnthropicReasoningOmission(providerName, params, adjustments)
	}

	return appendAnthropicTemperatureCoercion(providerName, params, adjustments)
}

func appendOmittedCompleteParamAdjustment(
	adjustments []completeParamAdjustment,
	capabilities ProviderCapabilities,
	name string,
	set bool,
	reset func(),
	fallbackReason string,
) []completeParamAdjustment {
	if !set {
		return adjustments
	}

	support, ok := capabilities.CompleteParams[name]
	if !ok || support.Status != CompleteParamOmitted {
		return adjustments
	}

	reset()

	reason := strings.TrimSpace(support.Note)
	if reason == "" {
		reason = fallbackReason
	}

	return append(adjustments, completeParamAdjustment{
		Name:   name,
		Action: "omitted",
		Reason: reason,
	})
}

func appendAnthropicReasoningOmission(
	providerName string,
	params CompleteParams,
	adjustments []completeParamAdjustment,
) (CompleteParams, []completeParamAdjustment) {
	if !anthropicProviderName(providerName) ||
		!anthropicThinkingRequested(params.ReasoningLevel) ||
		params.MaxTokens <= 0 ||
		params.MaxTokens > anthropicThinkingMinMaxTokens {
		return params, adjustments
	}

	params.ReasoningLevel = ""

	adjustments = append(adjustments, completeParamAdjustment{
		Name:   "ReasoningLevel",
		Action: "omitted",
		Reason: "Anthropic extended thinking requires max_tokens greater than 1024",
	})

	return params, adjustments
}

func appendAnthropicTemperatureCoercion(
	providerName string,
	params CompleteParams,
	adjustments []completeParamAdjustment,
) (CompleteParams, []completeParamAdjustment) {
	if !anthropicProviderName(providerName) || params.Temperature == nil ||
		!anthropicThinkingRequested(params.ReasoningLevel) || *params.Temperature == 1 {
		return params, adjustments
	}

	one := 1.0
	params.Temperature = &one

	adjustments = append(adjustments, completeParamAdjustment{
		Name:   "Temperature",
		Action: "coerced",
		Reason: "Anthropic extended thinking only accepts temperature=1",
	})

	return params, adjustments
}

func anthropicProviderName(providerName string) bool {
	return providerName == providerAnthropic || providerName == providerClaudeCode
}

func anthropicThinkingRequested(level string) bool {
	switch normalizeReasoningLevel(level) {
	case "", ReasoningLevelDefault, reasoningLevelNone, reasoningLevelMinimal:
		return false
	default:
		return true
	}
}
