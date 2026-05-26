package llm

import (
	"context"
	"encoding/json"
	"math"
	"net/http"
	"net/http/httptest"
	"reflect"
	"sort"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestProviderCapabilities_CoverCompleteParams(t *testing.T) {
	t.Parallel()

	wantFields := completeParamFieldNames()

	for _, providerName := range protocolProviderNames() {
		t.Run(providerName, func(t *testing.T) {
			t.Parallel()

			capabilities, ok := BuiltInProviderCapabilities(providerName)
			require.True(t, ok, "missing capability metadata for %s", providerName)
			assert.ElementsMatch(t, wantFields, mapKeys(capabilities.CompleteParams))

			for field, support := range capabilities.CompleteParams {
				assert.NotEmpty(t, support.Status, field)
				assert.Contains(t, []CompleteParamSupportStatus{
					CompleteParamSupported,
					CompleteParamUnsupported,
					CompleteParamOmitted,
					CompleteParamLossy,
				}, support.Status, field)

				if support.Status != CompleteParamSupported {
					assert.NotEmpty(t, support.Note, field)
				}
			}

			assert.Equal(t, capabilities.SupportsSeed, capabilities.CompleteParams["Seed"].Status == CompleteParamSupported)
			assert.Equal(t, capabilities.SupportsTools, capabilities.CompleteParams["Tools"].Status == CompleteParamSupported)
			assert.Equal(t, capabilities.SupportsReasoning, capabilities.CompleteParams["ReasoningLevel"].Status != CompleteParamUnsupported)
		})
	}
}

func TestProviderCapabilities_FeatureMatrix(t *testing.T) {
	t.Parallel()

	want := map[string]ProviderCapabilities{
		providerOpenAI: {
			SupportsSeed:                  true,
			SupportsTools:                 true,
			SupportsReasoning:             true,
			SupportsCacheAccounting:       true,
			SupportsStreaming:             false,
			SupportsNetworkModelDiscovery: true,
		},
		providerAnthropic: {
			SupportsSeed:                  false,
			SupportsTools:                 true,
			SupportsReasoning:             true,
			SupportsCacheAccounting:       true,
			SupportsStreaming:             false,
			SupportsNetworkModelDiscovery: true,
		},
		providerClaudeCode: {
			SupportsSeed:                  false,
			SupportsTools:                 true,
			SupportsReasoning:             true,
			SupportsCacheAccounting:       true,
			SupportsStreaming:             false,
			SupportsNetworkModelDiscovery: false,
		},
		providerCodex: {
			SupportsSeed:                  false,
			SupportsTools:                 true,
			SupportsReasoning:             true,
			SupportsCacheAccounting:       true,
			SupportsStreaming:             true,
			SupportsNetworkModelDiscovery: false,
		},
		providerOllama: {
			SupportsSeed:                  true,
			SupportsTools:                 true,
			SupportsReasoning:             true,
			SupportsCacheAccounting:       false,
			SupportsStreaming:             true,
			SupportsNetworkModelDiscovery: true,
		},
	}

	for _, providerName := range protocolProviderNames() {
		t.Run(providerName, func(t *testing.T) {
			t.Parallel()

			capabilities, ok := BuiltInProviderCapabilities(providerName)
			require.True(t, ok)

			assert.Equal(t, want[providerName].SupportsSeed, capabilities.SupportsSeed)
			assert.Equal(t, want[providerName].SupportsTools, capabilities.SupportsTools)
			assert.Equal(t, want[providerName].SupportsReasoning, capabilities.SupportsReasoning)
			assert.Equal(t, want[providerName].SupportsCacheAccounting, capabilities.SupportsCacheAccounting)
			assert.Equal(t, want[providerName].SupportsStreaming, capabilities.SupportsStreaming)
			assert.Equal(t, want[providerName].SupportsNetworkModelDiscovery, capabilities.SupportsNetworkModelDiscovery)
		})
	}
}

func TestProviderCapabilities_ExposedThroughKnownProviders(t *testing.T) {
	t.Parallel()

	for _, provider := range KnownProviders() {
		t.Run(provider.Name, func(t *testing.T) {
			t.Parallel()

			assert.NotEmpty(t, provider.Models)
			assert.NotEmpty(t, provider.Capabilities.CompleteParams)
		})
	}
}

func TestProviderCapabilitiesFor_BuiltInsAndCustomProviders(t *testing.T) {
	t.Parallel()

	providers := []Provider{
		&AnthropicProvider{},
		&ClaudeCodeProvider{},
		&CodexProvider{},
		&OpenAIProvider{},
		&OllamaProvider{},
	}

	for _, provider := range providers {
		t.Run(provider.Name(), func(t *testing.T) {
			t.Parallel()

			want, ok := BuiltInProviderCapabilities(provider.Name())
			require.True(t, ok)
			assert.Equal(t, want, ProviderCapabilitiesFor(provider))
		})
	}

	assert.Empty(t, ProviderCapabilitiesFor(nil).CompleteParams)
	assert.Empty(t, ProviderCapabilitiesFor(&fakeProvider{name: "custom"}).CompleteParams)
}

func TestProviderCapabilities_StreamingFlagMatchesProviderInterfaces(t *testing.T) {
	t.Parallel()

	providers := []Provider{
		&AnthropicProvider{},
		&ClaudeCodeProvider{},
		&CodexProvider{},
		&OpenAIProvider{},
		&OllamaProvider{},
	}

	for _, provider := range providers {
		t.Run(provider.Name(), func(t *testing.T) {
			t.Parallel()

			capabilities := ProviderCapabilitiesFor(provider)
			_, implementsStreaming := provider.(StreamProvider)
			assert.Equal(t, implementsStreaming, capabilities.SupportsStreaming)
		})
	}
}

func TestProviderCapabilities_NetworkModelDiscoveryFixtures(t *testing.T) {
	t.Parallel()

	t.Run(providerOpenAI, func(t *testing.T) {
		t.Parallel()

		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			assert.Equal(t, "/v1/models", r.URL.Path)
			w.Header().Set("Content-Type", "application/json")
			_, err := w.Write([]byte(`{"data":[{"id":"live-openai-model"}]}`))
			assert.NoError(t, err)
		}))
		defer srv.Close()

		provider := &OpenAIProvider{apiKey: "test-key", baseURL: srv.URL, client: srv.Client()}
		assert.True(t, ProviderCapabilitiesFor(provider).SupportsNetworkModelDiscovery)

		models, err := provider.FetchModels(context.Background())
		require.NoError(t, err)
		assert.Equal(t, []string{"live-openai-model"}, models)
	})

	t.Run(providerAnthropic, func(t *testing.T) {
		t.Parallel()

		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			assert.Equal(t, "/v1/models", r.URL.Path)
			w.Header().Set("Content-Type", "application/json")
			_, err := w.Write([]byte(`{"data":[{"id":"live-anthropic-model"}]}`))
			assert.NoError(t, err)
		}))
		defer srv.Close()

		provider := &AnthropicProvider{apiKey: "test-key", baseURL: srv.URL, client: srv.Client()}
		assert.True(t, ProviderCapabilitiesFor(provider).SupportsNetworkModelDiscovery)

		models, err := provider.FetchModels(context.Background())
		require.NoError(t, err)
		assert.Equal(t, []string{"live-anthropic-model"}, models)
	})

	t.Run(providerOllama, func(t *testing.T) {
		t.Parallel()

		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			assert.Equal(t, "/api/tags", r.URL.Path)
			w.Header().Set("Content-Type", "application/json")
			_, err := w.Write([]byte(`{"models":[{"name":"live-ollama-model"}]}`))
			assert.NoError(t, err)
		}))
		defer srv.Close()

		provider := &OllamaProvider{baseURL: srv.URL, client: srv.Client()}
		assert.True(t, ProviderCapabilitiesFor(provider).SupportsNetworkModelDiscovery)

		models, err := provider.FetchModels(context.Background())
		require.NoError(t, err)
		assert.Equal(t, []string{"live-ollama-model"}, models)
	})

	t.Run("static local catalogs", func(t *testing.T) {
		t.Parallel()

		providers := []Provider{
			&ClaudeCodeProvider{models: []string{"local-claude-code-model"}},
			&CodexProvider{models: []string{"local-codex-model"}},
		}

		for _, provider := range providers {
			capabilities := ProviderCapabilitiesFor(provider)
			assert.False(t, capabilities.SupportsNetworkModelDiscovery, provider.Name())

			models, err := provider.FetchModels(context.Background())
			require.NoError(t, err, provider.Name())
			assert.Equal(t, provider.Models(), models, provider.Name())
		}
	})
}

func TestBuiltInProviderCapabilities_ReturnsClone(t *testing.T) {
	t.Parallel()

	capabilities, ok := BuiltInProviderCapabilities(providerCodex)
	require.True(t, ok)

	capabilities.CompleteParams["Seed"] = CompleteParamSupport{Status: CompleteParamSupported}

	capabilities, ok = BuiltInProviderCapabilities(providerCodex)
	require.True(t, ok)
	assert.Equal(t, CompleteParamUnsupported, capabilities.CompleteParams["Seed"].Status)
}

func TestProviderProtocolRequestFixtures(t *testing.T) {
	t.Parallel()

	for _, fixture := range providerProtocolRequestFixtures() {
		t.Run(fixture.name, func(t *testing.T) {
			t.Parallel()

			for _, adapter := range protocolRequestAdapters() {
				t.Run(adapter.name, func(t *testing.T) {
					t.Parallel()

					wantJSON, hasWant := fixture.want[adapter.name]
					unsupportedField, hasUnsupported := fixture.unsupported[adapter.name]
					require.True(t, hasWant || hasUnsupported, "fixture must record %s mapping or unsupported status", adapter.name)

					got, err := adapter.build(fixture.params)
					if hasUnsupported {
						require.Error(t, err)
						assert.Contains(t, err.Error(), unsupportedField)

						return
					}

					require.NoError(t, err)
					assert.JSONEq(t, wantJSON, mustJSON(t, got))
				})
			}
		})
	}
}

func TestProviderProtocolRequestFixtures_CoverCompleteParamsFields(t *testing.T) {
	t.Parallel()

	assert.ElementsMatch(t, completeParamFieldNames(), requestFixtureCoveredCompleteParams())
}

func TestProviderProtocolFixtures_CoverPublicLLMSchema(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name string
		got  []string
		want []string
	}{
		{
			name: "CompleteParams",
			got:  structFieldNames[CompleteParams](),
			want: []string{
				"Temperature",
				"TopP",
				"Seed",
				"Model",
				"ReasoningLevel",
				"Messages",
				"Stop",
				"Tools",
				"MaxTokens",
			},
		},
		{
			name: "Message",
			got:  structFieldNames[Message](),
			want: []string{
				"ToolResult",
				"Role",
				"Content",
				"ToolCalls",
			},
		},
		{
			name: "ToolDefinition",
			got:  structFieldNames[ToolDefinition](),
			want: []string{
				"Parameters",
				"Name",
				"Description",
			},
		},
		{
			name: "ToolCall",
			got:  structFieldNames[ToolCall](),
			want: []string{
				"Input",
				"ID",
				"Name",
			},
		},
		{
			name: "ToolResult",
			got:  structFieldNames[ToolResult](),
			want: []string{
				"ToolCallID",
				"Content",
				"IsError",
			},
		},
		{
			name: "Response",
			got:  structFieldNames[Response](),
			want: []string{
				"Content",
				"Provider",
				"Model",
				"StopReason",
				"ToolCalls",
				"Latency",
				"FirstTokenLatency",
				"InputTokens",
				"CachedInputTokens",
				"CacheWriteInputTokens",
				"OutputTokens",
			},
		},
		{
			name: "ProviderCapabilities",
			got:  structFieldNames[ProviderCapabilities](),
			want: []string{
				"CompleteParams",
				"SupportsSeed",
				"SupportsTools",
				"SupportsReasoning",
				"SupportsCacheAccounting",
				"SupportsStreaming",
				"SupportsNetworkModelDiscovery",
			},
		},
		{
			name: "CompleteParamSupport",
			got:  structFieldNames[CompleteParamSupport](),
			want: []string{
				"Status",
				"Note",
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			assert.ElementsMatch(t, tc.want, tc.got)
		})
	}
}

func TestProviderProtocolRequestUnsupportedParams(t *testing.T) {
	t.Parallel()

	adapters := protocolRequestAdaptersByName()

	for _, providerName := range protocolProviderNames() {
		capabilities, ok := BuiltInProviderCapabilities(providerName)
		require.True(t, ok)

		adapter, ok := adapters[providerName]
		require.True(t, ok)

		for field, support := range capabilities.CompleteParams {
			if support.Status != CompleteParamUnsupported {
				continue
			}

			t.Run(providerName+"/"+field, func(t *testing.T) {
				t.Parallel()

				_, err := adapter.build(paramsWithOnlyFieldSet(t, field))
				require.Error(t, err)
				assert.Contains(t, err.Error(), field)
			})
		}
	}
}

func TestProviderProtocolRequestImpossibleCombinations(t *testing.T) {
	t.Parallel()

	for _, fixture := range providerProtocolRequestImpossibleCombinations() {
		t.Run(fixture.name, func(t *testing.T) {
			t.Parallel()

			for _, adapter := range protocolRequestAdapters() {
				t.Run(adapter.name, func(t *testing.T) {
					t.Parallel()

					_, err := adapter.build(fixture.params)

					wantErr, shouldError := fixture.wantError[adapter.name]
					if shouldError {
						require.Error(t, err)
						assert.Contains(t, err.Error(), wantErr)

						return
					}

					require.NoError(t, err)
				})
			}
		})
	}
}

func TestProviderProtocolResponseFixtures(t *testing.T) {
	t.Parallel()

	for _, fixture := range providerProtocolResponseFixtures() {
		t.Run(fixture.name, func(t *testing.T) {
			t.Parallel()

			resp := fixture.parse(t)
			assert.Equal(t, fixture.wantContent, resp.Content)
			assert.Equal(t, fixture.wantModel, resp.Model)
			assert.Equal(t, fixture.wantStop, resp.StopReason)
			assert.Equal(t, fixture.wantInputTokens, resp.InputTokens)
			assert.Equal(t, fixture.wantCachedTokens, resp.CachedInputTokens)
			assert.Equal(t, fixture.wantOutputTokens, resp.OutputTokens)

			if fixture.wantToolCall != nil {
				require.Len(t, resp.ToolCalls, 1)
				assert.Equal(t, *fixture.wantToolCall, resp.ToolCalls[0])
			} else {
				assert.Empty(t, resp.ToolCalls)
			}
		})
	}
}

func TestProviderProtocolResponseFixtures_CoverProviderContracts(t *testing.T) {
	t.Parallel()

	coverage := make(map[string]map[string]struct{})

	for _, fixture := range providerProtocolResponseFixtures() {
		require.NotEmpty(t, fixture.provider, fixture.name)

		if coverage[fixture.provider] == nil {
			coverage[fixture.provider] = make(map[string]struct{})
		}

		for _, cover := range fixture.covers {
			coverage[fixture.provider][cover] = struct{}{}
		}
	}

	for _, providerName := range protocolProviderNames() {
		providerCoverage := coverage[providerName]
		require.NotNil(t, providerCoverage, "missing response fixtures for %s", providerName)

		for _, cover := range []string{"usage", "stop_end_turn", "stop_max_tokens", "tool_use"} {
			assert.Contains(t, providerCoverage, cover, "%s response fixtures must cover %s", providerName, cover)
		}

		capabilities, ok := BuiltInProviderCapabilities(providerName)
		require.True(t, ok)

		if capabilities.SupportsCacheAccounting {
			assert.Contains(t, providerCoverage, "cached_tokens", "%s response fixtures must cover cache accounting", providerName)
		} else {
			assert.Contains(t, providerCoverage, "no_cache_accounting", "%s response fixtures must record absent cache accounting", providerName)
		}
	}

	for _, providerName := range []string{providerOpenAI, providerCodex} {
		assert.Contains(t, coverage[providerName], "invalid_tool_json", "%s response fixtures must cover raw fallback", providerName)
	}

	for _, providerName := range []string{providerAnthropic, providerClaudeCode} {
		assert.Contains(t, coverage[providerName], "stop_sequence", "%s response fixtures must cover stop_sequence normalization", providerName)
	}
}

func TestProviderProtocolStopReasonNormalization(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		provider string
		got      StopReason
		want     StopReason
	}{
		{name: "openai stop", provider: providerOpenAI, got: openaiStopReason("stop"), want: StopEndTurn},
		{name: "openai tool calls", provider: providerOpenAI, got: openaiStopReason("tool_calls"), want: StopToolUse},
		{name: "openai length", provider: providerOpenAI, got: openaiStopReason("length"), want: StopMaxToks},
		{name: "openai unknown", provider: providerOpenAI, got: openaiStopReason("content_filter"), want: StopUnknown},
		{name: "anthropic end turn", provider: providerAnthropic, got: anthropicStopReason("end_turn"), want: StopEndTurn},
		{name: "anthropic stop sequence", provider: providerAnthropic, got: anthropicStopReason("stop_sequence"), want: StopEndTurn},
		{name: "anthropic tool use", provider: providerAnthropic, got: anthropicStopReason("tool_use"), want: StopToolUse},
		{name: "anthropic max tokens", provider: providerAnthropic, got: anthropicStopReason("max_tokens"), want: StopMaxToks},
		{name: "anthropic unknown", provider: providerAnthropic, got: anthropicStopReason("pause_turn"), want: StopUnknown},
		{name: "codex completed", provider: providerCodex, got: codexStopReason(&codexEventPayload{Status: "completed"}), want: StopEndTurn},
		{name: "codex max output tokens", provider: providerCodex, got: codexStopReason(&codexEventPayload{Status: "incomplete", IncompleteDetails: &codexIncompleteDetails{Reason: "max_output_tokens"}}), want: StopMaxToks},
		{name: "codex max tokens", provider: providerCodex, got: codexStopReason(&codexEventPayload{Status: "incomplete", IncompleteDetails: &codexIncompleteDetails{Reason: "max_tokens"}}), want: StopMaxToks},
		{name: "codex unknown incomplete", provider: providerCodex, got: codexStopReason(&codexEventPayload{Status: "incomplete"}), want: StopUnknown},
		{name: "codex nil", provider: providerCodex, got: codexStopReason(nil), want: StopUnknown},
		{name: "ollama stop", provider: providerOllama, got: ollamaStopReason("stop"), want: StopEndTurn},
		{name: "ollama length", provider: providerOllama, got: ollamaStopReason("length"), want: StopMaxToks},
		{name: "ollama unknown", provider: providerOllama, got: ollamaStopReason("load"), want: StopUnknown},
	}

	coverage := make(map[string]map[StopReason]struct{})

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			assert.Equal(t, tt.want, tt.got)
		})

		if coverage[tt.provider] == nil {
			coverage[tt.provider] = make(map[StopReason]struct{})
		}

		coverage[tt.provider][tt.want] = struct{}{}
	}

	for _, providerName := range []string{providerOpenAI, providerAnthropic, providerCodex, providerOllama} {
		for _, stop := range []StopReason{StopEndTurn, StopMaxToks, StopUnknown} {
			assert.Contains(t, coverage[providerName], stop, "%s stop mapping must cover %s", providerName, stop)
		}
	}

	for _, providerName := range []string{providerOpenAI, providerAnthropic} {
		assert.Contains(t, coverage[providerName], StopToolUse, "%s stop mapping must cover tool use", providerName)
	}
}

type protocolRequestAdapter struct {
	build func(CompleteParams) (any, error)
	name  string
}

func protocolRequestAdapters() []protocolRequestAdapter {
	return []protocolRequestAdapter{
		{name: providerOpenAI, build: func(params CompleteParams) (any, error) { return buildOpenAIRequest(params) }},
		{name: providerAnthropic, build: func(params CompleteParams) (any, error) {
			return buildAnthropicRequestForProvider(providerAnthropic, params)
		}},
		{name: providerClaudeCode, build: func(params CompleteParams) (any, error) {
			return buildAnthropicRequestForProvider(providerClaudeCode, params)
		}},
		{name: providerCodex, build: func(params CompleteParams) (any, error) { return buildCodexResponsesRequest(params) }},
		{name: providerOllama, build: func(params CompleteParams) (any, error) { return buildOllamaChatRequest(params) }},
	}
}

func protocolRequestAdaptersByName() map[string]protocolRequestAdapter {
	adapters := make(map[string]protocolRequestAdapter, len(protocolRequestAdapters()))
	for _, adapter := range protocolRequestAdapters() {
		adapters[adapter.name] = adapter
	}

	return adapters
}

type protocolRequestFixture struct {
	want        map[string]string
	unsupported map[string]string
	covers      []string
	name        string
	params      CompleteParams
}

func providerProtocolRequestFixtures() []protocolRequestFixture {
	temperature := 0.4
	topP := 0.8
	seed := 42

	return []protocolRequestFixture{
		{
			name:   "plain chat",
			params: baseContractParams([]Message{{Role: RoleUser, Content: "Hello"}}),
			covers: []string{
				"Model",
				"Messages",
			},
			want: map[string]string{
				providerOpenAI:     `{"model":"contract-model","messages":[{"role":"user","content":"Hello"}]}`,
				providerAnthropic:  `{"model":"contract-model","messages":[{"role":"user","content":"Hello"}],"max_tokens":4096}`,
				providerClaudeCode: `{"model":"contract-model","messages":[{"role":"user","content":"Hello"}],"max_tokens":4096}`,
				providerCodex:      `{"model":"contract-model","instructions":"You are a helpful assistant.","input":[{"type":"message","role":"user","content":[{"type":"input_text","text":"Hello"}]}],"stream":true,"store":false}`,
				providerOllama:     `{"model":"contract-model","messages":[{"role":"user","content":"Hello"}],"options":{},"stream":false}`,
			},
		},
		{
			name: "system prompt",
			params: baseContractParams([]Message{
				{Role: RoleSystem, Content: "Be terse."},
				{Role: RoleUser, Content: "Hello"},
			}),
			covers: []string{"Messages"},
			want: map[string]string{
				providerOpenAI:     `{"model":"contract-model","messages":[{"role":"system","content":"Be terse."},{"role":"user","content":"Hello"}]}`,
				providerAnthropic:  `{"model":"contract-model","system":"Be terse.","messages":[{"role":"user","content":"Hello"}],"max_tokens":4096}`,
				providerClaudeCode: `{"model":"contract-model","system":"Be terse.","messages":[{"role":"user","content":"Hello"}],"max_tokens":4096}`,
				providerCodex:      `{"model":"contract-model","instructions":"Be terse.","input":[{"type":"message","role":"user","content":[{"type":"input_text","text":"Hello"}]}],"stream":true,"store":false}`,
				providerOllama:     `{"model":"contract-model","messages":[{"role":"system","content":"Be terse."},{"role":"user","content":"Hello"}],"options":{},"stream":false}`,
			},
		},
		{
			name: "tool definition",
			params: withContractParams(baseContractParams([]Message{{Role: RoleUser, Content: "Use a tool if needed."}}), func(params *CompleteParams) {
				params.Tools = []ToolDefinition{contractToolDefinition()}
			}),
			covers: []string{"Tools"},
			want: map[string]string{
				providerOpenAI:     `{"model":"contract-model","messages":[{"role":"user","content":"Use a tool if needed."}],"tools":[{"type":"function","function":{"name":"lookup","description":"Look up a value.","parameters":{"type":"object","properties":{"query":{"type":"string"}},"required":["query"],"additionalProperties":false}}}]}`,
				providerAnthropic:  `{"model":"contract-model","messages":[{"role":"user","content":"Use a tool if needed."}],"tools":[{"name":"lookup","description":"Look up a value.","input_schema":{"type":"object","properties":{"query":{"type":"string"}},"required":["query"],"additionalProperties":false}}],"max_tokens":4096}`,
				providerClaudeCode: `{"model":"contract-model","messages":[{"role":"user","content":"Use a tool if needed."}],"tools":[{"name":"lookup","description":"Look up a value.","input_schema":{"type":"object","properties":{"query":{"type":"string"}},"required":["query"],"additionalProperties":false}}],"max_tokens":4096}`,
				providerCodex:      `{"model":"contract-model","instructions":"You are a helpful assistant.","input":[{"type":"message","role":"user","content":[{"type":"input_text","text":"Use a tool if needed."}]}],"tools":[{"type":"function","name":"lookup","description":"Look up a value.","parameters":{"type":"object","properties":{"query":{"type":"string"}},"required":["query"],"additionalProperties":false}}],"stream":true,"store":false}`,
				providerOllama:     `{"model":"contract-model","messages":[{"role":"user","content":"Use a tool if needed."}],"tools":[{"type":"function","function":{"name":"lookup","description":"Look up a value.","parameters":{"type":"object","properties":{"query":{"type":"string"}},"required":["query"],"additionalProperties":false}}}],"options":{},"stream":false}`,
			},
		},
		{
			name: "assistant tool call",
			params: baseContractParams([]Message{
				{Role: RoleUser, Content: "Find Go."},
				{Role: RoleAssistant, Content: "I'll check.", ToolCalls: []ToolCall{{ID: "call-1", Name: "lookup", Input: map[string]any{"query": "go"}}}},
			}),
			covers: []string{"Messages"},
			want: map[string]string{
				providerOpenAI:     `{"model":"contract-model","messages":[{"role":"user","content":"Find Go."},{"role":"assistant","content":"I'll check.","tool_calls":[{"id":"call-1","type":"function","function":{"name":"lookup","arguments":"{\"query\":\"go\"}"}}]}]}`,
				providerAnthropic:  `{"model":"contract-model","messages":[{"role":"user","content":"Find Go."},{"role":"assistant","content":[{"type":"text","text":"I'll check."},{"type":"tool_use","id":"call-1","name":"lookup","input":{"query":"go"}}]}],"max_tokens":4096}`,
				providerClaudeCode: `{"model":"contract-model","messages":[{"role":"user","content":"Find Go."},{"role":"assistant","content":[{"type":"text","text":"I'll check."},{"type":"tool_use","id":"call-1","name":"lookup","input":{"query":"go"}}]}],"max_tokens":4096}`,
				providerCodex:      `{"model":"contract-model","instructions":"You are a helpful assistant.","input":[{"type":"message","role":"user","content":[{"type":"input_text","text":"Find Go."}]},{"type":"message","role":"assistant","content":[{"type":"output_text","text":"I'll check."}]},{"type":"function_call","call_id":"call-1","name":"lookup","arguments":"{\"query\":\"go\"}"}],"stream":true,"store":false}`,
				providerOllama:     `{"model":"contract-model","messages":[{"role":"user","content":"Find Go."},{"role":"assistant","content":"I'll check.","tool_calls":[{"function":{"name":"lookup","arguments":{"query":"go"}}}]}],"options":{},"stream":false}`,
			},
		},
		{
			name:   "tool result",
			params: baseContractParams([]Message{{Role: RoleTool, ToolResult: &ToolResult{ToolCallID: "call-1", Content: "result", IsError: true}}}),
			covers: []string{"Messages"},
			want: map[string]string{
				providerOpenAI:     `{"model":"contract-model","messages":[{"role":"tool","content":"result","tool_call_id":"call-1"}]}`,
				providerAnthropic:  `{"model":"contract-model","messages":[{"role":"user","content":[{"type":"tool_result","tool_use_id":"call-1","content":"result","is_error":true}]}],"max_tokens":4096}`,
				providerClaudeCode: `{"model":"contract-model","messages":[{"role":"user","content":[{"type":"tool_result","tool_use_id":"call-1","content":"result","is_error":true}]}],"max_tokens":4096}`,
				providerCodex:      `{"model":"contract-model","instructions":"You are a helpful assistant.","input":[{"type":"function_call_output","call_id":"call-1","output":"result"}],"stream":true,"store":false}`,
				providerOllama:     `{"model":"contract-model","messages":[{"role":"tool","content":"result"}],"options":{},"stream":false}`,
			},
		},
		{
			name: "reasoning level",
			params: withContractParams(baseContractParams([]Message{{Role: RoleUser, Content: "Think."}}), func(params *CompleteParams) {
				params.ReasoningLevel = "medium"
			}),
			covers: []string{"ReasoningLevel"},
			want: map[string]string{
				providerOpenAI:     `{"model":"contract-model","reasoning_effort":"medium","messages":[{"role":"user","content":"Think."}]}`,
				providerAnthropic:  `{"model":"contract-model","thinking":{"type":"enabled","budget_tokens":1365},"messages":[{"role":"user","content":"Think."}],"max_tokens":4096}`,
				providerClaudeCode: `{"model":"contract-model","thinking":{"type":"enabled","budget_tokens":1365},"messages":[{"role":"user","content":"Think."}],"max_tokens":4096}`,
				providerCodex:      `{"reasoning":{"effort":"medium"},"model":"contract-model","instructions":"You are a helpful assistant.","input":[{"type":"message","role":"user","content":[{"type":"input_text","text":"Think."}]}],"stream":true,"store":false}`,
				providerOllama:     `{"think":"medium","model":"contract-model","messages":[{"role":"user","content":"Think."}],"options":{},"stream":false}`,
			},
		},
		{
			name: "reasoning none",
			params: withContractParams(baseContractParams([]Message{{Role: RoleUser, Content: "Answer directly."}}), func(params *CompleteParams) {
				params.ReasoningLevel = "none"
			}),
			covers: []string{"ReasoningLevel"},
			want: map[string]string{
				providerOpenAI:     `{"model":"contract-model","reasoning_effort":"none","messages":[{"role":"user","content":"Answer directly."}]}`,
				providerAnthropic:  `{"model":"contract-model","messages":[{"role":"user","content":"Answer directly."}],"max_tokens":4096}`,
				providerClaudeCode: `{"model":"contract-model","messages":[{"role":"user","content":"Answer directly."}],"max_tokens":4096}`,
				providerCodex:      `{"reasoning":{"effort":"none"},"model":"contract-model","instructions":"You are a helpful assistant.","input":[{"type":"message","role":"user","content":[{"type":"input_text","text":"Answer directly."}]}],"stream":true,"store":false}`,
				providerOllama:     `{"think":false,"model":"contract-model","messages":[{"role":"user","content":"Answer directly."}],"options":{},"stream":false}`,
			},
		},
		{
			name: "max tokens",
			params: withContractParams(baseContractParams([]Message{{Role: RoleUser, Content: "Limit."}}), func(params *CompleteParams) {
				params.MaxTokens = 123
			}),
			covers: []string{"MaxTokens"},
			want: map[string]string{
				providerOpenAI:     `{"model":"contract-model","messages":[{"role":"user","content":"Limit."}],"max_tokens":123}`,
				providerAnthropic:  `{"model":"contract-model","messages":[{"role":"user","content":"Limit."}],"max_tokens":123}`,
				providerClaudeCode: `{"model":"contract-model","messages":[{"role":"user","content":"Limit."}],"max_tokens":123}`,
				providerOllama:     `{"model":"contract-model","messages":[{"role":"user","content":"Limit."}],"options":{"num_predict":123},"stream":false}`,
			},
			unsupported: map[string]string{providerCodex: "MaxTokens"},
		},
		{
			name: "stop sequences",
			params: withContractParams(baseContractParams([]Message{{Role: RoleUser, Content: "Stop."}}), func(params *CompleteParams) {
				params.Stop = []string{"END"}
			}),
			covers: []string{"Stop"},
			want: map[string]string{
				providerOpenAI:     `{"model":"contract-model","messages":[{"role":"user","content":"Stop."}],"stop":["END"]}`,
				providerAnthropic:  `{"model":"contract-model","messages":[{"role":"user","content":"Stop."}],"stop_sequences":["END"],"max_tokens":4096}`,
				providerClaudeCode: `{"model":"contract-model","messages":[{"role":"user","content":"Stop."}],"stop_sequences":["END"],"max_tokens":4096}`,
				providerOllama:     `{"model":"contract-model","messages":[{"role":"user","content":"Stop."}],"options":{"stop":["END"]},"stream":false}`,
			},
			unsupported: map[string]string{providerCodex: "Stop"},
		},
		{
			name: "seed",
			params: withContractParams(baseContractParams([]Message{{Role: RoleUser, Content: "Seed."}}), func(params *CompleteParams) {
				params.Seed = &seed
			}),
			covers: []string{"Seed"},
			want: map[string]string{
				providerOpenAI: `{"model":"contract-model","seed":42,"messages":[{"role":"user","content":"Seed."}]}`,
				providerOllama: `{"model":"contract-model","messages":[{"role":"user","content":"Seed."}],"options":{"seed":42},"stream":false}`,
			},
			unsupported: map[string]string{providerAnthropic: "Seed", providerClaudeCode: "Seed", providerCodex: "Seed"},
		},
		{
			name: "temperature",
			params: withContractParams(baseContractParams([]Message{{Role: RoleUser, Content: "Sample."}}), func(params *CompleteParams) {
				params.Temperature = &temperature
			}),
			covers: []string{
				"Temperature",
			},
			want: map[string]string{
				providerOpenAI:     `{"temperature":0.4,"model":"contract-model","messages":[{"role":"user","content":"Sample."}]}`,
				providerAnthropic:  `{"temperature":0.4,"model":"contract-model","messages":[{"role":"user","content":"Sample."}],"max_tokens":4096}`,
				providerClaudeCode: `{"temperature":0.4,"model":"contract-model","messages":[{"role":"user","content":"Sample."}],"max_tokens":4096}`,
				providerCodex:      `{"model":"contract-model","instructions":"You are a helpful assistant.","input":[{"type":"message","role":"user","content":[{"type":"input_text","text":"Sample."}]}],"stream":true,"store":false}`,
				providerOllama:     `{"model":"contract-model","messages":[{"role":"user","content":"Sample."}],"options":{"temperature":0.4},"stream":false}`,
			},
		},
		{
			name: "top p",
			params: withContractParams(baseContractParams([]Message{{Role: RoleUser, Content: "Sample."}}), func(params *CompleteParams) {
				params.TopP = &topP
			}),
			covers: []string{
				"TopP",
			},
			want: map[string]string{
				providerOpenAI:     `{"top_p":0.8,"model":"contract-model","messages":[{"role":"user","content":"Sample."}]}`,
				providerAnthropic:  `{"top_p":0.8,"model":"contract-model","messages":[{"role":"user","content":"Sample."}],"max_tokens":4096}`,
				providerClaudeCode: `{"top_p":0.8,"model":"contract-model","messages":[{"role":"user","content":"Sample."}],"max_tokens":4096}`,
				providerOllama:     `{"model":"contract-model","messages":[{"role":"user","content":"Sample."}],"options":{"top_p":0.8},"stream":false}`,
			},
			unsupported: map[string]string{providerCodex: "TopP"},
		},
	}
}

//nolint:govet // Test fixture field order groups assertions by purpose.
type protocolResponseFixture struct {
	parse            func(*testing.T) *Response
	wantToolCall     *ToolCall
	covers           []string
	name             string
	provider         string
	wantContent      string
	wantModel        string
	wantStop         StopReason
	wantInputTokens  int
	wantCachedTokens int
	wantOutputTokens int
}

func providerProtocolResponseFixtures() []protocolResponseFixture {
	return []protocolResponseFixture{
		{
			name:     "openai end turn usage and cache",
			provider: providerOpenAI,
			covers:   []string{"usage", "cached_tokens", "stop_end_turn"},
			parse: func(t *testing.T) *Response {
				t.Helper()

				return parseOpenAIContractJSON(t, `{"model":"wire-model","choices":[{"finish_reason":"stop","message":{"content":"openai text"}}],"usage":{"prompt_tokens":10,"completion_tokens":3,"prompt_tokens_details":{"cached_tokens":2}}}`)
			},
			wantContent: "openai text", wantModel: "wire-model", wantStop: StopEndTurn,
			wantInputTokens: 10, wantCachedTokens: 2, wantOutputTokens: 3,
		},
		{
			name:     "openai max tokens",
			provider: providerOpenAI,
			covers:   []string{"stop_max_tokens"},
			parse: func(t *testing.T) *Response {
				t.Helper()

				return parseOpenAIContractJSON(t, `{"model":"wire-model","choices":[{"finish_reason":"length","message":{"content":"truncated"}}],"usage":{"prompt_tokens":1,"completion_tokens":2}}`)
			},
			wantContent: "truncated", wantModel: "wire-model", wantStop: StopMaxToks,
			wantInputTokens: 1, wantOutputTokens: 2,
		},
		{
			name:     "openai invalid tool json falls back to raw",
			provider: providerOpenAI,
			covers:   []string{"usage", "cached_tokens", "tool_use", "invalid_tool_json"},
			parse: func(t *testing.T) *Response {
				t.Helper()

				return parseOpenAIContractJSON(t, `{"model":"wire-model","choices":[{"finish_reason":"tool_calls","message":{"tool_calls":[{"id":"call-bad","type":"function","function":{"name":"lookup","arguments":"{bad json"}}]}}],"usage":{"prompt_tokens":4,"completion_tokens":0,"prompt_tokens_details":{"cached_tokens":1}}}`)
			},
			wantModel: "wire-model", wantStop: StopToolUse, wantInputTokens: 4, wantCachedTokens: 1,
			wantToolCall: &ToolCall{ID: "call-bad", Name: "lookup", Input: map[string]any{"raw": `{bad json`}},
		},
		{
			name:     "anthropic end turn usage and cache",
			provider: providerAnthropic,
			covers:   []string{"usage", "cached_tokens", "stop_end_turn"},
			parse: func(t *testing.T) *Response {
				t.Helper()

				return parseAnthropicContractJSON(t, `{"model":"anthropic-model","stop_reason":"end_turn","content":[{"type":"text","text":"anthropic text"}],"usage":{"input_tokens":10,"cache_creation_input_tokens":4,"cache_read_input_tokens":2,"output_tokens":3}}`)
			},
			wantContent: "anthropic text", wantModel: "anthropic-model", wantStop: StopEndTurn,
			wantInputTokens: 16, wantCachedTokens: 2, wantOutputTokens: 3,
		},
		{
			name:     "anthropic max tokens",
			provider: providerAnthropic,
			covers:   []string{"stop_max_tokens"},
			parse: func(t *testing.T) *Response {
				t.Helper()

				return parseAnthropicContractJSON(t, `{"model":"anthropic-model","stop_reason":"max_tokens","content":[{"type":"text","text":"cut"}],"usage":{"input_tokens":1,"output_tokens":2}}`)
			},
			wantContent: "cut", wantModel: "anthropic-model", wantStop: StopMaxToks, wantInputTokens: 1, wantOutputTokens: 2,
		},
		{
			name:     "anthropic stop sequence",
			provider: providerAnthropic,
			covers:   []string{"stop_sequence"},
			parse: func(t *testing.T) *Response {
				t.Helper()

				return parseAnthropicContractJSON(t, `{"model":"anthropic-model","stop_reason":"stop_sequence","content":[{"type":"text","text":"stopped"}],"usage":{"input_tokens":1,"output_tokens":2}}`)
			},
			wantContent: "stopped", wantModel: "anthropic-model", wantStop: StopEndTurn,
			wantInputTokens: 1, wantOutputTokens: 2,
		},
		{
			name:     "anthropic tool use",
			provider: providerAnthropic,
			covers:   []string{"usage", "cached_tokens", "tool_use"},
			parse: func(t *testing.T) *Response {
				t.Helper()

				return parseAnthropicContractJSON(t, `{"model":"anthropic-model","stop_reason":"tool_use","content":[{"type":"tool_use","id":"tool-1","name":"lookup","input":{"query":"go"}}],"usage":{"input_tokens":5,"cache_read_input_tokens":1}}`)
			},
			wantModel: "anthropic-model", wantStop: StopToolUse, wantInputTokens: 6, wantCachedTokens: 1,
			wantToolCall: &ToolCall{ID: "tool-1", Name: "lookup", Input: map[string]any{"query": "go"}},
		},
		{
			name:     "claude code uses anthropic response contract",
			provider: providerClaudeCode,
			covers:   []string{"usage", "cached_tokens", "stop_end_turn"},
			parse: func(t *testing.T) *Response {
				t.Helper()

				return parseAnthropicContractJSON(t, `{"model":"claude-code-model","stop_reason":"end_turn","content":[{"type":"text","text":"claude code text"}],"usage":{"input_tokens":7,"cache_read_input_tokens":3,"output_tokens":2}}`)
			},
			wantContent: "claude code text", wantModel: "claude-code-model", wantStop: StopEndTurn,
			wantInputTokens: 10, wantCachedTokens: 3, wantOutputTokens: 2,
		},
		{
			name:     "claude code max tokens",
			provider: providerClaudeCode,
			covers:   []string{"stop_max_tokens"},
			parse: func(t *testing.T) *Response {
				t.Helper()

				return parseAnthropicContractJSON(t, `{"model":"claude-code-model","stop_reason":"max_tokens","content":[{"type":"text","text":"cut"}],"usage":{"input_tokens":1,"output_tokens":2}}`)
			},
			wantContent: "cut", wantModel: "claude-code-model", wantStop: StopMaxToks,
			wantInputTokens: 1, wantOutputTokens: 2,
		},
		{
			name:     "claude code stop sequence",
			provider: providerClaudeCode,
			covers:   []string{"stop_sequence"},
			parse: func(t *testing.T) *Response {
				t.Helper()

				return parseAnthropicContractJSON(t, `{"model":"claude-code-model","stop_reason":"stop_sequence","content":[{"type":"text","text":"stopped"}],"usage":{"input_tokens":1,"output_tokens":2}}`)
			},
			wantContent: "stopped", wantModel: "claude-code-model", wantStop: StopEndTurn,
			wantInputTokens: 1, wantOutputTokens: 2,
		},
		{
			name:     "claude code tool use",
			provider: providerClaudeCode,
			covers:   []string{"usage", "cached_tokens", "tool_use"},
			parse: func(t *testing.T) *Response {
				t.Helper()

				return parseAnthropicContractJSON(t, `{"model":"claude-code-model","stop_reason":"tool_use","content":[{"type":"tool_use","id":"tool-1","name":"lookup","input":{"query":"go"}}],"usage":{"input_tokens":5,"cache_read_input_tokens":1}}`)
			},
			wantModel: "claude-code-model", wantStop: StopToolUse, wantInputTokens: 6, wantCachedTokens: 1,
			wantToolCall: &ToolCall{ID: "tool-1", Name: "lookup", Input: map[string]any{"query": "go"}},
		},
		{
			name:     "codex end turn usage and cache",
			provider: providerCodex,
			covers:   []string{"usage", "cached_tokens", "stop_end_turn"},
			parse: func(t *testing.T) *Response {
				t.Helper()

				return parseCodexContractSSE(t, `data: {"type":"response.output_text.delta","delta":"codex text"}

data: {"type":"response.output_item.done","item":{"type":"message","role":"assistant","content":[{"type":"output_text","text":"codex text"}]}}

data: {"type":"response.completed","response":{"model":"codex-model","status":"completed","usage":{"input_tokens":12,"output_tokens":3,"input_tokens_details":{"cached_tokens":4}}}}

`)
			},
			wantContent: "codex text", wantModel: "codex-model", wantStop: StopEndTurn,
			wantInputTokens: 12, wantCachedTokens: 4, wantOutputTokens: 3,
		},
		{
			name:     "codex max tokens",
			provider: providerCodex,
			covers:   []string{"stop_max_tokens"},
			parse: func(t *testing.T) *Response {
				t.Helper()

				return parseCodexContractSSE(t, `data: {"type":"response.output_text.delta","delta":"cut"}

data: {"type":"response.output_item.done","item":{"type":"message","role":"assistant","content":[{"type":"output_text","text":"cut"}]}}

data: {"type":"response.completed","response":{"model":"codex-model","status":"incomplete","incomplete_details":{"reason":"max_output_tokens"},"usage":{"input_tokens":1,"output_tokens":2,"input_tokens_details":{"cached_tokens":0}}}}

`)
			},
			wantContent: "cut", wantModel: "codex-model", wantStop: StopMaxToks, wantInputTokens: 1, wantOutputTokens: 2,
		},
		{
			name:     "codex invalid tool json falls back to raw",
			provider: providerCodex,
			covers:   []string{"usage", "cached_tokens", "tool_use", "invalid_tool_json"},
			parse: func(t *testing.T) *Response {
				t.Helper()

				return parseCodexContractSSE(t, `data: {"type":"response.output_item.done","item":{"type":"function_call","call_id":"call-bad","name":"lookup","arguments":"{bad json"}}

data: {"type":"response.completed","response":{"model":"codex-model","status":"completed","usage":{"input_tokens":3,"output_tokens":0,"input_tokens_details":{"cached_tokens":1}}}}

`)
			},
			wantModel: "codex-model", wantStop: StopToolUse, wantInputTokens: 3, wantCachedTokens: 1,
			wantToolCall: &ToolCall{ID: "call-bad", Name: "lookup", Input: map[string]any{"raw": `{bad json`}},
		},
		{
			name:     "ollama end turn usage without cache",
			provider: providerOllama,
			covers:   []string{"usage", "no_cache_accounting", "stop_end_turn"},
			parse: func(t *testing.T) *Response {
				t.Helper()

				return parseOllamaContractJSON(t, `{"model":"ollama-model","done_reason":"stop","message":{"content":"ollama text"},"prompt_eval_count":9,"eval_count":4}`)
			},
			wantContent: "ollama text", wantModel: "ollama-model", wantStop: StopEndTurn,
			wantInputTokens: 9, wantOutputTokens: 4,
		},
		{
			name:     "ollama max tokens",
			provider: providerOllama,
			covers:   []string{"stop_max_tokens"},
			parse: func(t *testing.T) *Response {
				t.Helper()

				return parseOllamaContractJSON(t, `{"model":"ollama-model","done_reason":"length","message":{"content":"cut"},"prompt_eval_count":1,"eval_count":2}`)
			},
			wantContent: "cut", wantModel: "ollama-model", wantStop: StopMaxToks, wantInputTokens: 1, wantOutputTokens: 2,
		},
		{
			name:     "ollama tool use generates stable call id",
			provider: providerOllama,
			covers:   []string{"usage", "tool_use"},
			parse: func(t *testing.T) *Response {
				t.Helper()

				return parseOllamaContractJSON(t, `{"model":"ollama-model","done_reason":"stop","message":{"tool_calls":[{"function":{"name":"lookup","arguments":{"query":"go"}}}]},"prompt_eval_count":3}`)
			},
			wantModel: "ollama-model", wantStop: StopToolUse, wantInputTokens: 3,
			wantToolCall: &ToolCall{ID: "ollama_0", Name: "lookup", Input: map[string]any{"query": "go"}},
		},
	}
}

type protocolNegativeRequestFixture struct {
	wantError map[string]string
	name      string
	params    CompleteParams
}

func providerProtocolRequestImpossibleCombinations() []protocolNegativeRequestFixture {
	return []protocolNegativeRequestFixture{
		{
			name: "non-json tool definition parameters",
			params: withContractParams(baseContractParams([]Message{{Role: RoleUser, Content: "Use a tool."}}), func(params *CompleteParams) {
				tool := contractToolDefinition()
				tool.Parameters["invalid"] = make(chan int)
				params.Tools = []ToolDefinition{tool}
			}),
			wantError: map[string]string{
				providerOpenAI:     "Tools[0].Parameters",
				providerAnthropic:  "Tools[0].Parameters",
				providerClaudeCode: "Tools[0].Parameters",
				providerCodex:      "Tools[0].Parameters",
				providerOllama:     "Tools[0].Parameters",
			},
		},
		{
			name: "non-finite sampling values",
			params: withContractParams(baseContractParams([]Message{{Role: RoleUser, Content: "Sample."}}), func(params *CompleteParams) {
				temperature := math.NaN()
				topP := math.Inf(1)
				params.Temperature = &temperature
				params.TopP = &topP
			}),
			wantError: map[string]string{
				providerOpenAI:     "Temperature",
				providerAnthropic:  "Temperature",
				providerClaudeCode: "Temperature",
				providerCodex:      "Temperature",
				providerOllama:     "Temperature",
			},
		},
		{
			name: "non-finite top_p",
			params: withContractParams(baseContractParams([]Message{{Role: RoleUser, Content: "Sample."}}), func(params *CompleteParams) {
				topP := math.Inf(1)
				params.TopP = &topP
			}),
			wantError: map[string]string{
				providerOpenAI:     "TopP",
				providerAnthropic:  "TopP",
				providerClaudeCode: "TopP",
				providerCodex:      "TopP",
				providerOllama:     "TopP",
			},
		},
		{
			name: "non-json assistant tool call input",
			params: baseContractParams([]Message{
				{Role: RoleUser, Content: "Find Go."},
				{Role: RoleAssistant, ToolCalls: []ToolCall{{ID: "call-1", Name: "lookup", Input: map[string]any{"invalid": make(chan int)}}}},
			}),
			wantError: map[string]string{
				providerOpenAI:     "ToolCalls[0].Input",
				providerAnthropic:  "ToolCalls[0].Input",
				providerClaudeCode: "ToolCalls[0].Input",
				providerCodex:      "ToolCalls[0].Input",
				providerOllama:     "ToolCalls[0].Input",
			},
		},
		{
			name: "negative max tokens",
			params: withContractParams(baseContractParams([]Message{{Role: RoleUser, Content: "Limit."}}), func(params *CompleteParams) {
				params.MaxTokens = -1
			}),
			wantError: map[string]string{
				providerOpenAI:     "MaxTokens",
				providerAnthropic:  "MaxTokens",
				providerClaudeCode: "MaxTokens",
				providerCodex:      "MaxTokens",
				providerOllama:     "MaxTokens",
			},
		},
		{
			name: "reasoning with too-small max tokens",
			params: withContractParams(baseContractParams([]Message{{Role: RoleUser, Content: "Think briefly."}}), func(params *CompleteParams) {
				params.ReasoningLevel = "low"
				params.MaxTokens = 1024
			}),
			wantError: map[string]string{
				providerAnthropic:  "max_tokens greater than 1024",
				providerClaudeCode: "max_tokens greater than 1024",
				providerCodex:      "MaxTokens",
			},
		},
	}
}

func completeParamFieldNames() []string {
	return structFieldNames[CompleteParams]()
}

func structFieldNames[T any]() []string {
	typeOfParams := reflect.TypeFor[T]()

	fields := make([]string, 0, typeOfParams.NumField())
	for field := range typeOfParams.Fields() {
		fields = append(fields, field.Name)
	}

	sort.Strings(fields)

	return fields
}

func protocolProviderNames() []string {
	return []string{providerAnthropic, providerClaudeCode, providerCodex, providerOpenAI, providerOllama}
}

func requestFixtureCoveredCompleteParams() []string {
	seen := make(map[string]struct{})
	fixtures := providerProtocolRequestFixtures()

	for i := range fixtures {
		fixture := &fixtures[i]
		for _, field := range fixture.covers {
			seen[field] = struct{}{}
		}
	}

	return mapKeys(seen)
}

func mapKeys[V any](m map[string]V) []string {
	keys := make([]string, 0, len(m))
	for key := range m {
		keys = append(keys, key)
	}

	sort.Strings(keys)

	return keys
}

func baseContractParams(messages []Message) CompleteParams {
	return CompleteParams{Model: "contract-model", Messages: messages}
}

func withContractParams(params CompleteParams, edit func(*CompleteParams)) CompleteParams {
	edit(&params)
	return params
}

func paramsWithOnlyFieldSet(t *testing.T, field string) CompleteParams {
	t.Helper()

	params := CompleteParams{Model: "contract-model"}

	switch field {
	case "Temperature":
		value := 0.4
		params.Temperature = &value
	case "TopP":
		value := 0.8
		params.TopP = &value
	case "Seed":
		value := 42
		params.Seed = &value
	case "Model":
		params.Model = "contract-model"
	case "ReasoningLevel":
		params.ReasoningLevel = "medium"
	case "Messages":
		params.Messages = []Message{{Role: RoleUser, Content: "hello"}}
	case "Stop":
		params.Stop = []string{"END"}
	case "Tools":
		params.Tools = []ToolDefinition{contractToolDefinition()}
	case "MaxTokens":
		params.MaxTokens = 123
	default:
		t.Fatalf("unsupported CompleteParams field fixture %q", field)
	}

	return params
}

func contractToolDefinition() ToolDefinition {
	return ToolDefinition{
		Name:        "lookup",
		Description: "Look up a value.",
		Parameters: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"query": map[string]any{"type": "string"},
			},
			"required":             []string{"query"},
			"additionalProperties": false,
		},
	}
}

func mustJSON(t *testing.T, v any) string {
	t.Helper()

	body, err := json.Marshal(v)
	require.NoError(t, err)

	return string(body)
}

func parseCodexContractSSE(t *testing.T, stream string) *Response {
	t.Helper()

	resp, err := parseCodexSSE(context.Background(), strings.NewReader(stream))
	require.NoError(t, err)

	return resp
}

func parseOpenAIContractJSON(t *testing.T, body string) *Response {
	t.Helper()

	var resp openaiResponse
	require.NoError(t, json.Unmarshal([]byte(body), &resp))

	return parseOpenAIResponse(resp)
}

func parseAnthropicContractJSON(t *testing.T, body string) *Response {
	t.Helper()

	var resp anthropicResponse
	require.NoError(t, json.Unmarshal([]byte(body), &resp))

	return parseAnthropicResponse(resp)
}

func parseOllamaContractJSON(t *testing.T, body string) *Response {
	t.Helper()

	var resp ollamaChatResponse
	require.NoError(t, json.Unmarshal([]byte(body), &resp))

	return parseOllamaChatResponse(resp, "fallback")
}
