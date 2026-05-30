package llm

import (
	"encoding/json"
	"fmt"
	"maps"
	"math"
	"strings"
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
	SupportsSeed                  bool                            `json:"supports_seed"`
	SupportsTools                 bool                            `json:"supports_tools"`
	SupportsReasoning             bool                            `json:"supports_reasoning"`
	SupportsModelMode             bool                            `json:"supports_model_mode"`
	SupportsCacheAccounting       bool                            `json:"supports_cache_accounting"`
	SupportsStreaming             bool                            `json:"supports_streaming"`
	SupportsNetworkModelDiscovery bool                            `json:"supports_network_model_discovery"`
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
	return capabilities
}

// Capabilities returns Ollama's provider protocol metadata.
func (o *OllamaProvider) Capabilities() ProviderCapabilities {
	capabilities, _ := BuiltInProviderCapabilities(providerOllama)
	return capabilities
}

var builtInProviderCapabilities = map[string]ProviderCapabilities{
	providerOpenAI: {
		SupportsSeed:                  true,
		SupportsTools:                 true,
		SupportsReasoning:             true,
		SupportsModelMode:             true,
		SupportsCacheAccounting:       true,
		SupportsStreaming:             false,
		SupportsNetworkModelDiscovery: true,
		CompleteParams: map[string]CompleteParamSupport{
			"Temperature":    supported("maps to chat.completions temperature"),
			"TopP":           supported("maps to chat.completions top_p"),
			"Seed":           supported("maps to chat.completions seed"),
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
		SupportsSeed:                  false,
		SupportsTools:                 true,
		SupportsReasoning:             true,
		SupportsModelMode:             false,
		SupportsCacheAccounting:       true,
		SupportsStreaming:             false,
		SupportsNetworkModelDiscovery: true,
		CompleteParams: map[string]CompleteParamSupport{
			"Temperature":    supported("maps to messages temperature; coerced to 1 when thinking is enabled"),
			"TopP":           supported("maps to messages top_p"),
			"Seed":           unsupported("Anthropic Messages has no seed parameter"),
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
		SupportsSeed:                  false,
		SupportsTools:                 true,
		SupportsReasoning:             true,
		SupportsModelMode:             false,
		SupportsCacheAccounting:       true,
		SupportsStreaming:             false,
		SupportsNetworkModelDiscovery: false,
		CompleteParams: map[string]CompleteParamSupport{
			"Temperature":    supported("maps to Anthropic Messages temperature; coerced to 1 when thinking is enabled"),
			"TopP":           supported("maps to Anthropic Messages top_p"),
			"Seed":           unsupported("Claude Code OAuth path uses Anthropic Messages, which has no seed parameter"),
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
		SupportsSeed:                  false,
		SupportsTools:                 true,
		SupportsReasoning:             true,
		SupportsModelMode:             true,
		SupportsCacheAccounting:       true,
		SupportsStreaming:             true,
		SupportsNetworkModelDiscovery: false,
		CompleteParams: map[string]CompleteParamSupport{
			"Temperature":    omitted("Codex ChatGPT responses endpoint does not expose temperature in this adapter"),
			"TopP":           unsupported("Codex ChatGPT responses endpoint does not expose top_p in this adapter"),
			"Seed":           unsupported("Codex ChatGPT responses endpoint does not expose seed in this adapter"),
			"Model":          supported("maps to model"),
			"ModelMode":      supported("fast maps to Responses service_tier=priority when model metadata supports fast mode"),
			"ReasoningLevel": supported("maps to responses reasoning.effort"),
			"Messages":       lossy("system messages become instructions; tool calls/results become Responses input items; ToolResult.IsError is not represented"),
			"Stop":           unsupported("Codex ChatGPT responses endpoint does not expose stop sequences in this adapter"),
			"Tools":          supported("maps to Responses function tools"),
			"MaxTokens":      unsupported("Codex ChatGPT responses endpoint does not expose max output tokens in this adapter"),
		},
	},
	providerOllama: {
		SupportsSeed:                  true,
		SupportsTools:                 true,
		SupportsReasoning:             true,
		SupportsModelMode:             false,
		SupportsCacheAccounting:       false,
		SupportsStreaming:             true,
		SupportsNetworkModelDiscovery: true,
		CompleteParams: map[string]CompleteParamSupport{
			"Temperature":    supported("maps to options.temperature"),
			"TopP":           supported("maps to options.top_p"),
			"Seed":           supported("maps to options.seed"),
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
		return nil
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
		{name: "Model", set: strings.TrimSpace(params.Model) != ""},
		{name: "ModelMode", set: normalizeModelMode(params.ModelMode) != ""},
		{name: "ReasoningLevel", set: strings.TrimSpace(params.ReasoningLevel) != ""},
		{name: "Messages", set: len(params.Messages) > 0},
		{name: "Stop", set: len(params.Stop) > 0},
		{name: "Tools", set: len(params.Tools) > 0},
		{name: "MaxTokens", set: params.MaxTokens > 0},
	}

	for _, check := range checks {
		if !check.set {
			continue
		}

		support, ok := capabilities.CompleteParams[check.name]
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
	if err := validateCompleteParamsWireSafe(providerName, params); err != nil {
		return params, nil, err
	}

	params, adjustments := normalizeCompleteParamsForProvider(providerName, params)

	return params, adjustments, nil
}

func normalizeCompleteParamsForProvider(
	providerName string,
	params CompleteParams,
) (CompleteParams, []completeParamAdjustment) {
	var adjustments []completeParamAdjustment

	if params.Temperature == nil {
		return params, adjustments
	}

	switch providerName {
	case providerCodex:
		params.Temperature = nil

		adjustments = append(adjustments, completeParamAdjustment{
			Name:   "Temperature",
			Action: "omitted",
			Reason: "Codex ChatGPT responses endpoint does not expose temperature in this adapter",
		})
	case providerAnthropic, providerClaudeCode:
		if anthropicThinkingRequested(params.ReasoningLevel) && *params.Temperature != 1 {
			one := 1.0
			params.Temperature = &one

			adjustments = append(adjustments, completeParamAdjustment{
				Name:   "Temperature",
				Action: "coerced",
				Reason: "Anthropic extended thinking only accepts temperature=1",
			})
		}
	}

	return params, adjustments
}

func anthropicThinkingRequested(level string) bool {
	switch normalizeReasoningLevel(level) {
	case "", reasoningLevelNone, reasoningLevelMinimal:
		return false
	default:
		return true
	}
}
