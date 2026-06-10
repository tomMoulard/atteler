package llm

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/tommoulard/atteler/pkg/modelroute"
)

func TestOpenAIProvider_Complete(t *testing.T) {
	t.Parallel()

	var (
		gotReq     openaiRequest
		gotHeaders http.Header
	)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotHeaders = r.Header.Clone()

		body, err := io.ReadAll(r.Body)
		if !assert.NoError(t, err) {
			return
		}

		if !assert.NoError(t, json.Unmarshal(body, &gotReq)) {
			return
		}

		resp := openaiResponse{
			Model: gotReq.Model,
			Choices: []struct {
				FinishReason string `json:"finish_reason"`
				Message      struct {
					Content   string           `json:"content"`
					ToolCalls []openaiToolCall `json:"tool_calls,omitempty"`
				} `json:"message"`
			}{{
				FinishReason: "stop",
				Message: struct {
					Content   string           `json:"content"`
					ToolCalls []openaiToolCall `json:"tool_calls,omitempty"`
				}{Content: "hello back"},
			}},
		}
		resp.Usage.PromptTokens = 8
		resp.Usage.PromptTokensDetails.CachedTokens = 2
		resp.Usage.CompletionTokens = 3

		w.Header().Set("Content-Type", "application/json")
		assert.NoError(t, json.NewEncoder(w).Encode(resp))
	}))
	defer srv.Close()

	p := &OpenAIProvider{
		apiKey:  "sk-test",
		bearer:  false,
		baseURL: srv.URL,
		client:  srv.Client(),
	}
	temperature := 0.7
	seed := 123

	resp, err := p.Complete(context.Background(), CompleteParams{
		Model:          "gpt-4.1",
		MaxTokens:      200,
		Temperature:    &temperature,
		Seed:           &seed,
		ReasoningLevel: "high",
		Messages: []Message{
			{Role: RoleSystem, Content: "you are helpful"},
			{Role: RoleUser, Content: "hi"},
		},
	})
	if err != nil {
		require.NoError(t, err)
	}

	// Verify response.
	if resp.Content != "hello back" {
		assert.Failf(t, "assertion failed", "content = %q, want %q", resp.Content, "hello back")
	}

	assert.Equal(t, providerOpenAI, resp.Provider)

	if resp.InputTokens != 8 || resp.CachedInputTokens != 2 || resp.OutputTokens != 3 {
		assert.Failf(t, "assertion failed", "tokens = %d/%d/%d, want 8/2/3", resp.InputTokens, resp.CachedInputTokens, resp.OutputTokens)
	}

	// Verify request shape — system message stays inline for OpenAI.
	if len(gotReq.Messages) != 2 {
		require.Failf(t, "unexpected failure", "messages len = %d, want 2", len(gotReq.Messages))
	}

	if gotReq.Messages[0].Role != "system" {
		assert.Failf(t, "assertion failed", "messages[0].role = %q", gotReq.Messages[0].Role)
	}

	if gotReq.MaxTokens != 200 {
		assert.Failf(t, "assertion failed", "max_tokens = %d", gotReq.MaxTokens)
	}

	if gotReq.Temperature == nil || *gotReq.Temperature != 0.7 {
		assert.Failf(t, "assertion failed", "temperature = %v", gotReq.Temperature)
	}

	if gotReq.Seed == nil || *gotReq.Seed != 123 {
		assert.Failf(t, "assertion failed", "seed = %v", gotReq.Seed)
	}

	if gotReq.ReasoningEffort != "high" {
		assert.Failf(t, "assertion failed", "reasoning_effort = %q, want high", gotReq.ReasoningEffort)
	}

	// Verify auth header.
	if gotHeaders.Get("Authorization") != "Bearer sk-test" {
		assert.Failf(t, "assertion failed", "Authorization = %q", gotHeaders.Get("Authorization"))
	}
}

func TestBuildOpenAIRequest_ModelModeFastUsesPriorityServiceTier(t *testing.T) {
	t.Parallel()

	req, err := buildOpenAIRequest(CompleteParams{
		Model:     "gpt-5.5",
		ModelMode: ModelModeFast,
		Messages:  []Message{{Role: RoleUser, Content: "hi"}},
	})
	require.NoError(t, err)

	assert.Equal(t, modelModePriority, req.ServiceTier)
}

func TestBuildOpenAIRequest_ModelModeFastRejectsUnsupportedModel(t *testing.T) {
	t.Parallel()

	_, err := buildOpenAIRequest(CompleteParams{
		Model:     "gpt-5.4-nano",
		ModelMode: ModelModeFast,
		Messages:  []Message{{Role: RoleUser, Content: "hi"}},
	})

	require.Error(t, err)
	assert.Contains(t, err.Error(), "does not support")
}

func TestOpenAIProvider_Embed(t *testing.T) {
	t.Parallel()

	var (
		gotReq     openaiEmbeddingRequest
		gotPath    string
		gotHeaders http.Header
	)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotHeaders = r.Header.Clone()

		body, err := io.ReadAll(r.Body)
		if !assert.NoError(t, err) {
			return
		}

		if !assert.NoError(t, json.Unmarshal(body, &gotReq)) {
			return
		}

		resp := openaiEmbeddingResponse{
			Model: gotReq.Model,
			Data: []openaiEmbeddingData{
				{Index: 1, Embedding: []float64{0, 1}},
				{Index: 0, Embedding: []float64{1, 0}},
			},
		}
		resp.Usage.PromptTokens = 9
		resp.Usage.TotalTokens = 9

		w.Header().Set("Content-Type", "application/json")
		assert.NoError(t, json.NewEncoder(w).Encode(resp))
	}))
	defer srv.Close()

	p := &OpenAIProvider{
		apiKey:  "sk-test",
		baseURL: srv.URL,
		client:  srv.Client(),
	}

	resp, err := p.Embed(context.Background(), EmbeddingParams{
		Model:      "text-embedding-3-small",
		Input:      []string{"alpha", "beta"},
		Dimensions: 2,
	})
	require.NoError(t, err)

	assert.Equal(t, "/v1/embeddings", gotPath)
	assert.Equal(t, "Bearer sk-test", gotHeaders.Get("Authorization"))
	assert.Equal(t, "text-embedding-3-small", gotReq.Model)
	assert.Equal(t, []any{"alpha", "beta"}, gotReq.Input)
	assert.Equal(t, 2, gotReq.Dimensions)
	assert.Equal(t, providerOpenAI, resp.Provider)
	assert.Equal(t, "text-embedding-3-small", resp.Model)
	assert.Equal(t, [][]float64{{1, 0}, {0, 1}}, resp.Embeddings)
	assert.Equal(t, 9, resp.InputTokens)
}

func TestOpenAICompatibleProviderCapabilityOverrideRejectsMissingEmbeddings(t *testing.T) {
	t.Parallel()

	provider, err := NewOpenAICompatibleProviderWithConfigContext(context.Background(), "chat-only", ProviderConfig{
		Type:         "openai_compatible",
		BaseURL:      "https://chat.internal.example",
		Models:       []string{"chat-model"},
		Capabilities: []string{modelroute.CapabilityChat},
	})
	require.NoError(t, err)

	_, err = provider.Embed(context.Background(), EmbeddingParams{
		Model: "chat-model",
		Input: []string{"hello"},
	})

	require.Error(t, err)
	require.ErrorIs(t, err, ErrEmbeddingsUnsupported)
	assert.Contains(t, err.Error(), "capabilities do not include embeddings")
}

func TestOpenAIProvider_HTTPError(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Retry-After", "2")
		w.Header().Set("X-Request-ID", "req-openai")
		w.WriteHeader(http.StatusUnauthorized)

		if _, err := w.Write([]byte(`{"error":{"type":"invalid_api_key","message":"bad key"}}`)); err != nil {
			return // best-effort in test handler
		}
	}))
	defer srv.Close()

	p := &OpenAIProvider{apiKey: "k", baseURL: srv.URL, client: srv.Client()}

	_, err := p.Complete(context.Background(), CompleteParams{
		Model:    "gpt-4.1",
		Messages: []Message{{Role: RoleUser, Content: "hi"}},
	})
	if err == nil {
		require.FailNow(t, "expected error on 401")
	}

	var providerErr *ProviderError
	require.ErrorAs(t, err, &providerErr)
	assert.Equal(t, providerOpenAI, providerErr.Provider)
	assert.Equal(t, http.StatusUnauthorized, providerErr.StatusCode)
	assert.Equal(t, 2*time.Second, providerErr.RetryAfter)
	assert.Equal(t, "req-openai", providerErr.RequestID)
	assert.Equal(t, RetryabilityNonRetryable, providerErr.Retryability)
	assert.Contains(t, providerErr.Message, "invalid_api_key")
}

func TestOpenAIProvider_FetchModelsHTTPErrorIsTyped(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Retry-After", "4")
		w.Header().Set("X-Request-ID", "req-openai-models")
		w.WriteHeader(http.StatusInternalServerError)

		if _, err := w.Write([]byte(`{"error":{"type":"server_error","message":"try again"}}`)); err != nil {
			return // best-effort in test handler
		}
	}))
	defer srv.Close()

	p := &OpenAIProvider{apiKey: "k", baseURL: srv.URL, client: srv.Client()}

	_, err := p.FetchModels(context.Background())
	require.Error(t, err)

	var providerErr *ProviderError
	require.ErrorAs(t, err, &providerErr)
	assert.Equal(t, providerOpenAI, providerErr.Provider)
	assert.Equal(t, http.StatusInternalServerError, providerErr.StatusCode)
	assert.Equal(t, 4*time.Second, providerErr.RetryAfter)
	assert.Equal(t, "req-openai-models", providerErr.RequestID)
	assert.Equal(t, RetryabilityRetryable, providerErr.Retryability)
	assert.Contains(t, providerErr.Message, "server_error")
}

func TestOpenAIProvider_ErrorPayloadIsTyped(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Retry-After", "5")
		w.Header().Set("X-Request-ID", "req-openai-payload")
		w.Header().Set("Content-Type", "application/json")
		assert.NoError(t, json.NewEncoder(w).Encode(openaiResponse{
			Error: &struct {
				Message string `json:"message"`
				Type    string `json:"type"`
				Code    string `json:"code"`
			}{
				Type:    "rate_limit_error",
				Message: "slow down",
			},
		}))
	}))
	defer srv.Close()

	p := &OpenAIProvider{apiKey: "k", baseURL: srv.URL, client: srv.Client()}

	_, err := p.Complete(context.Background(), CompleteParams{
		Model:    "gpt-4.1",
		Messages: []Message{{Role: RoleUser, Content: "hi"}},
	})
	require.Error(t, err)

	var providerErr *ProviderError
	require.ErrorAs(t, err, &providerErr)
	assert.Equal(t, providerOpenAI, providerErr.Provider)
	assert.Equal(t, http.StatusOK, providerErr.StatusCode)
	assert.Equal(t, 5*time.Second, providerErr.RetryAfter)
	assert.Equal(t, "req-openai-payload", providerErr.RequestID)
	assert.Equal(t, RetryabilityRetryable, providerErr.Retryability)
	assert.Equal(t, "rate_limit_error: slow down", providerErr.Message)
}

func TestOpenAIProvider_ErrorPayloadPrefersSpecificCode(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		assert.NoError(t, json.NewEncoder(w).Encode(openaiResponse{
			Error: &struct {
				Message string `json:"message"`
				Type    string `json:"type"`
				Code    string `json:"code"`
			}{
				Type:    "error",
				Code:    "server_error",
				Message: "try again",
			},
		}))
	}))
	defer srv.Close()

	p := &OpenAIProvider{apiKey: "k", baseURL: srv.URL, client: srv.Client()}

	_, err := p.Complete(context.Background(), CompleteParams{
		Model:    "gpt-4.1",
		Messages: []Message{{Role: RoleUser, Content: "hi"}},
	})
	require.Error(t, err)

	var providerErr *ProviderError
	require.ErrorAs(t, err, &providerErr)
	assert.Equal(t, providerOpenAI, providerErr.Provider)
	assert.Equal(t, http.StatusOK, providerErr.StatusCode)
	assert.Equal(t, RetryabilityRetryable, providerErr.Retryability)
	assert.Equal(t, "server_error: try again", providerErr.Message)
}

func TestOpenAIProvider_OmitsZeroMaxTokens(t *testing.T) {
	t.Parallel()

	var gotReq openaiRequest

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}

		if err := json.Unmarshal(body, &gotReq); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}

		w.Header().Set("Content-Type", "application/json")

		if err := json.NewEncoder(w).Encode(openaiResponse{
			Choices: []struct {
				FinishReason string `json:"finish_reason"`
				Message      struct {
					Content   string           `json:"content"`
					ToolCalls []openaiToolCall `json:"tool_calls,omitempty"`
				} `json:"message"`
			}{{
				FinishReason: "stop",
				Message: struct {
					Content   string           `json:"content"`
					ToolCalls []openaiToolCall `json:"tool_calls,omitempty"`
				}{Content: "ok"},
			}},
		}); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
		}
	}))
	defer srv.Close()

	p := &OpenAIProvider{apiKey: "k", baseURL: srv.URL, client: srv.Client()}

	_, err := p.Complete(context.Background(), CompleteParams{
		Model:    "gpt-4.1",
		Messages: []Message{{Role: RoleUser, Content: "hi"}},
	})
	if err != nil {
		require.NoError(t, err)
	}

	if gotReq.MaxTokens != 0 {
		assert.Failf(t, "assertion failed", "max_tokens = %d, want 0 (omitted)", gotReq.MaxTokens)
	}

	if gotReq.Temperature != nil {
		assert.Failf(t, "assertion failed", "temperature = %v, want omitted", *gotReq.Temperature)
	}

	if gotReq.TopP != nil {
		assert.Failf(t, "assertion failed", "top_p = %v, want omitted", *gotReq.TopP)
	}

	if gotReq.Seed != nil {
		assert.Failf(t, "assertion failed", "seed = %v, want omitted", *gotReq.Seed)
	}

	if gotReq.ReasoningEffort != "" {
		assert.Failf(t, "assertion failed", "reasoning_effort = %q, want omitted", gotReq.ReasoningEffort)
	}
}

func TestOpenAIProvider_NameAndModels(t *testing.T) {
	t.Parallel()

	p := &OpenAIProvider{}
	if p.Name() != providerOpenAI {
		assert.Failf(t, "assertion failed", "Name() = %q", p.Name())
	}

	if len(p.Models()) == 0 {
		assert.Fail(t, "Models() returned empty")
	}

	assert.Contains(t, p.Models(), "gpt-5.5")
	assert.Contains(t, p.Models(), "gpt-5")
	assert.Contains(t, p.Models(), "gpt-5.3-codex")
	assert.Contains(t, p.Models(), "gpt-4o")
	assert.Contains(t, p.Models(), "o3")
}

func TestRegistry_ProviderModelsLiveFetchReplacesOpenAICatalogFallback(t *testing.T) {
	t.Parallel()

	var gotAuth string

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		assert.Equal(t, "/v1/models", r.URL.Path)

		w.Header().Set("Content-Type", "application/json")
		assert.NoError(t, json.NewEncoder(w).Encode(openaiModelsResponse{
			Data: []struct {
				ID string `json:"id"`
			}{
				{ID: "gpt-live-only"},
			},
		}))
	}))
	defer srv.Close()

	registry := NewRegistry()
	registry.Register(&OpenAIProvider{
		apiKey:  "sk-test",
		baseURL: srv.URL,
		client:  srv.Client(),
	})

	assert.True(t, registry.ProviderHasModel(providerOpenAI, "gpt-5.5"))
	assert.False(t, registry.ProviderModelsVerified(providerOpenAI))

	models, err := registry.ProviderModels(context.Background(), providerOpenAI)
	require.NoError(t, err)

	assert.Equal(t, "Bearer sk-test", gotAuth)
	assert.Equal(t, []string{"gpt-live-only"}, models)
	assert.True(t, registry.ProviderHasModel(providerOpenAI, "gpt-live-only"))
	assert.False(t, registry.ProviderHasModel(providerOpenAI, "gpt-5.5"))
	assert.True(t, registry.ProviderModelsVerified(providerOpenAI))
}

func TestOpenAIProvider_ConfigBaseURL(t *testing.T) {
	t.Setenv("OPENAI_API_KEY", "sk-test")
	t.Setenv("OPENAI_BASE_URL", "")

	p, err := NewOpenAIProviderWithConfigContext(context.Background(), ProviderConfig{BaseURL: "https://openai.config"})
	if err != nil {
		require.NoError(t, err)
	}

	if p.baseURL != "https://openai.config" {
		assert.Failf(t, "assertion failed", "baseURL = %q, want config value", p.baseURL)
	}
}

func TestOpenAIProvider_EnvBaseURLOverridesConfig(t *testing.T) {
	t.Setenv("OPENAI_API_KEY", "sk-test")
	t.Setenv("OPENAI_BASE_URL", "https://openai.env")

	p, err := NewOpenAIProviderWithConfigContext(context.Background(), ProviderConfig{BaseURL: "https://openai.config"})
	if err != nil {
		require.NoError(t, err)
	}

	if p.baseURL != "https://openai.env" {
		assert.Failf(t, "assertion failed", "baseURL = %q, want env value", p.baseURL)
	}
}

func TestAutoRegisterWithConfigContext_OpenAICompatibleProvider(t *testing.T) {
	apiKeyEnv := "ATTELER_COMPAT_" + "KEY"
	t.Setenv(apiKeyEnv, "sk-compatible")

	var (
		gotAuth  string
		gotModel string
	)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")

		assert.Equal(t, "/v1/chat/completions", r.URL.Path)

		var gotReq openaiRequest

		body, err := io.ReadAll(r.Body)
		if !assert.NoError(t, err) {
			return
		}

		if !assert.NoError(t, json.Unmarshal(body, &gotReq)) {
			return
		}

		gotModel = gotReq.Model

		resp := openaiResponse{
			Model: gotReq.Model,
			Choices: []struct {
				FinishReason string `json:"finish_reason"`
				Message      struct {
					Content   string           `json:"content"`
					ToolCalls []openaiToolCall `json:"tool_calls,omitempty"`
				} `json:"message"`
			}{{
				FinishReason: "stop",
				Message: struct {
					Content   string           `json:"content"`
					ToolCalls []openaiToolCall `json:"tool_calls,omitempty"`
				}{Content: "compatible answer"},
			}},
		}

		w.Header().Set("Content-Type", "application/json")
		assert.NoError(t, json.NewEncoder(w).Encode(resp))
	}))
	defer srv.Close()

	registry := AutoRegisterWithConfigContext(context.Background(), AutoRegisterConfig{
		Providers: map[string]ProviderConfig{
			"vllm": {
				Type:      "openai_compatible",
				BaseURL:   srv.URL,
				APIKeyEnv: apiKeyEnv,
				Models:    []string{"qwen2.5-coder"},
			},
		},
		DefaultProvider:        "vllm",
		DefaultModel:           "vllm/qwen2.5-coder",
		DisableReadinessChecks: true,
	})

	assert.True(t, registry.ProviderHasModel("vllm", "qwen2.5-coder"))

	resp, err := registry.Complete(context.Background(), CompleteParams{
		Model:    "vllm/qwen2.5-coder",
		Messages: []Message{{Role: RoleUser, Content: "hello"}},
	})
	require.NoError(t, err)

	assert.Equal(t, "compatible answer", resp.Content)
	assert.Equal(t, "vllm", resp.Provider)
	assert.Equal(t, "qwen2.5-coder", resp.Model)
	assert.Equal(t, "qwen2.5-coder", gotModel)
	assert.Equal(t, "Bearer sk-compatible", gotAuth)
}

func TestOpenAICompatibleProviderEndpointURLKeepsVersionedBaseURL(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		baseURL string
		path    string
		want    string
	}{
		{
			name:    "versioned local chat",
			baseURL: "http://127.0.0.1:8000/v1",
			path:    defaultOpenAIChatPath,
			want:    "http://127.0.0.1:8000/v1/chat/completions",
		},
		{
			name:    "versioned local embeddings",
			baseURL: "http://127.0.0.1:8000/v1",
			path:    defaultOpenAIEmbeddingsPath,
			want:    "http://127.0.0.1:8000/v1/embeddings",
		},
		{
			name:    "versioned local models",
			baseURL: "http://127.0.0.1:8000/v1",
			path:    defaultOpenAIModelsPath,
			want:    "http://127.0.0.1:8000/v1/models",
		},
		{
			name:    "unversioned groq root keeps default v1",
			baseURL: "https://api.groq.com/openai",
			path:    defaultOpenAIChatPath,
			want:    "https://api.groq.com/openai/v1/chat/completions",
		},
		{
			name:    "versioned groq root",
			baseURL: "https://api.groq.com/openai/v1",
			path:    defaultOpenAIChatPath,
			want:    "https://api.groq.com/openai/v1/chat/completions",
		},
		{
			name:    "google ai studio root embeds api version",
			baseURL: "https://generativelanguage.googleapis.com/v1beta/openai",
			path:    defaultOpenAIChatPath,
			want:    "https://generativelanguage.googleapis.com/v1beta/openai/chat/completions",
		},
		{
			name:    "unversioned custom openai root keeps default v1",
			baseURL: "https://gateway.internal.example/custom/openai",
			path:    defaultOpenAIChatPath,
			want:    "https://gateway.internal.example/custom/openai/v1/chat/completions",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			provider := &OpenAIProvider{
				baseURL:      tc.baseURL,
				providerName: "compatible",
			}

			got, err := provider.endpointURL(tc.path, "qwen2.5-coder")
			require.NoError(t, err)
			assert.Equal(t, tc.want, got)
		})
	}
}

func TestOpenAICompatibleProviderRejectsInvalidEndpointPath(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name  string
		field string
		value string
		want  string
	}{
		{
			name:  "chat completions",
			field: "chat_completions_path",
			value: "v1/chat/completions",
			want:  `chat_completions_path "v1/chat/completions" must start with /`,
		},
		{
			name:  "embeddings",
			field: "embeddings_path",
			value: "v1/embeddings",
			want:  `embeddings_path "v1/embeddings" must start with /`,
		},
		{
			name:  "models",
			field: "models_path",
			value: "v1/models",
			want:  `models_path "v1/models" must start with /`,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			cfg := ProviderConfig{
				Type:    "openai_compatible",
				BaseURL: "https://compatible.internal.example",
			}

			switch tc.field {
			case "chat_completions_path":
				cfg.ChatCompletionsPath = tc.value
			case "embeddings_path":
				cfg.EmbeddingsPath = tc.value
			case "models_path":
				cfg.ModelsPath = tc.value
			default:
				require.Failf(t, "unknown test field", "field %q", tc.field)
			}

			_, err := NewOpenAICompatibleProviderWithConfigContext(context.Background(), "compatible", cfg)
			require.Error(t, err)
			assert.Contains(t, err.Error(), tc.want)
		})
	}
}

func TestOpenAICompatibleProviderTypeAliases(t *testing.T) {
	t.Parallel()

	aliases := []string{
		"groq",
		"mistral",
		"cohere",
		"gemini",
		"google",
		"google_gemini",
		"google-ai-studio",
		"ai_studio",
		"vertex_ai",
		"bedrock",
		"aws_bedrock",
		"amazon-bedrock",
		"vllm",
		"tgi",
		"text-generation-inference",
		"self_hosted",
		"litellm",
		"openrouter",
	}

	for _, alias := range aliases {
		t.Run(alias, func(t *testing.T) {
			t.Parallel()

			assert.Equal(t, openAICompatibleType, normalizeOpenAIProviderType(alias))
			assert.True(t, isOpenAICompatibleProviderType(alias))
		})
	}
}

func TestAutoRegisterWithConfigContext_OpenAICompatibleProviderTypeAlias(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, "/v1/chat/completions", r.URL.Path)

		var gotReq openaiRequest
		assert.NoError(t, json.NewDecoder(r.Body).Decode(&gotReq))

		resp := openaiResponse{
			Model: gotReq.Model,
			Choices: []struct {
				FinishReason string `json:"finish_reason"`
				Message      struct {
					Content   string           `json:"content"`
					ToolCalls []openaiToolCall `json:"tool_calls,omitempty"`
				} `json:"message"`
			}{{
				FinishReason: "stop",
				Message: struct {
					Content   string           `json:"content"`
					ToolCalls []openaiToolCall `json:"tool_calls,omitempty"`
				}{Content: "alias answer"},
			}},
		}

		w.Header().Set("Content-Type", "application/json")
		assert.NoError(t, json.NewEncoder(w).Encode(resp))
	}))
	defer srv.Close()

	registry := AutoRegisterWithConfigContext(context.Background(), AutoRegisterConfig{
		Providers: map[string]ProviderConfig{
			"groq": {
				Type:    "groq",
				BaseURL: srv.URL,
				Models:  []string{"llama-3.3-70b-versatile"},
			},
		},
		DefaultProvider:        "groq",
		DefaultModel:           "groq/llama-3.3-70b-versatile",
		DisableReadinessChecks: true,
	})

	assert.True(t, registry.ProviderHasModel("groq", "llama-3.3-70b-versatile"))

	resp, err := registry.Complete(context.Background(), CompleteParams{
		Model:    "groq/llama-3.3-70b-versatile",
		Messages: []Message{{Role: RoleUser, Content: "hello"}},
	})
	require.NoError(t, err)

	assert.Equal(t, "alias answer", resp.Content)
	assert.Equal(t, "groq", resp.Provider)
	assert.Equal(t, "llama-3.3-70b-versatile", resp.Model)
}

func TestAutoRegisterWithConfigContext_OpenAICompatibleProviderNameAlias(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, "/v1/chat/completions", r.URL.Path)

		var gotReq openaiRequest
		assert.NoError(t, json.NewDecoder(r.Body).Decode(&gotReq))

		resp := openaiResponse{
			Model: gotReq.Model,
			Choices: []struct {
				FinishReason string `json:"finish_reason"`
				Message      struct {
					Content   string           `json:"content"`
					ToolCalls []openaiToolCall `json:"tool_calls,omitempty"`
				} `json:"message"`
			}{{
				FinishReason: "stop",
				Message: struct {
					Content   string           `json:"content"`
					ToolCalls []openaiToolCall `json:"tool_calls,omitempty"`
				}{Content: "name alias answer"},
			}},
		}

		w.Header().Set("Content-Type", "application/json")
		assert.NoError(t, json.NewEncoder(w).Encode(resp))
	}))
	defer srv.Close()

	registry := AutoRegisterWithConfigContext(context.Background(), AutoRegisterConfig{
		Providers: map[string]ProviderConfig{
			"groq": {
				BaseURL: srv.URL,
				Models:  []string{"llama-3.3-70b-versatile"},
			},
		},
		DefaultProvider:        "groq",
		DefaultModel:           "groq/llama-3.3-70b-versatile",
		DisableReadinessChecks: true,
	})

	assert.True(t, registry.ProviderHasModel("groq", "llama-3.3-70b-versatile"))

	resp, err := registry.Complete(context.Background(), CompleteParams{
		Model:    "groq/llama-3.3-70b-versatile",
		Messages: []Message{{Role: RoleUser, Content: "hello"}},
	})
	require.NoError(t, err)

	assert.Equal(t, "name alias answer", resp.Content)
	assert.Equal(t, "groq", resp.Provider)
	assert.Equal(t, "llama-3.3-70b-versatile", resp.Model)
}

func TestAutoRegisterWithConfigContext_ModelRoleRoutesBareCatalogNameToPreferredCompatibleProvider(t *testing.T) {
	t.Parallel()

	var gotModel string

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, "/v1/chat/completions", r.URL.Path)

		var gotReq openaiRequest
		assert.NoError(t, json.NewDecoder(r.Body).Decode(&gotReq))
		gotModel = gotReq.Model

		resp := openaiResponse{
			Model: gotReq.Model,
			Choices: []struct {
				FinishReason string `json:"finish_reason"`
				Message      struct {
					Content   string           `json:"content"`
					ToolCalls []openaiToolCall `json:"tool_calls,omitempty"`
				} `json:"message"`
			}{{
				FinishReason: "stop",
				Message: struct {
					Content   string           `json:"content"`
					ToolCalls []openaiToolCall `json:"tool_calls,omitempty"`
				}{Content: "compatible role answer"},
			}},
		}

		w.Header().Set("Content-Type", "application/json")
		assert.NoError(t, json.NewEncoder(w).Encode(resp))
	}))
	defer srv.Close()

	registry := AutoRegisterWithConfigContext(context.Background(), AutoRegisterConfig{
		Providers: map[string]ProviderConfig{
			providerAnthropic:  {Disabled: true},
			providerClaudeCode: {Disabled: true},
			providerCodex:      {Disabled: true},
			providerOllama:     {Disabled: true},
			providerOpenAI:     {Disabled: true},
			"groq": {
				Type:    "groq",
				BaseURL: srv.URL,
				Models:  []string{"gpt-4.1-mini"},
			},
		},
		DefaultModel: "planner",
		ModelRoles: map[string]ModelRole{
			"planner": {
				Preferred:          "gpt-4.1-mini",
				PreferredProviders: []string{"groq"},
			},
		},
		DisableReadinessChecks: true,
	})

	resolution, ok, err := registry.ResolveModelRole("planner", CompleteParams{}, nil)
	require.NoError(t, err)
	require.True(t, ok)
	assert.Equal(t, "groq/gpt-4.1-mini", resolution.SelectedModel)

	resp, err := registry.Complete(context.Background(), CompleteParams{
		Messages: []Message{{Role: RoleUser, Content: "hello"}},
	})
	require.NoError(t, err)

	assert.Equal(t, "compatible role answer", resp.Content)
	assert.Equal(t, "groq", resp.Provider)
	assert.Equal(t, "gpt-4.1-mini", resp.Model)
	assert.Equal(t, "gpt-4.1-mini", gotModel)
}

func TestOpenAICompatibleProviderMarksLocalEndpoints(t *testing.T) {
	t.Parallel()

	provider, err := NewOpenAICompatibleProviderWithConfigContext(context.Background(), "vllm", ProviderConfig{
		Type:    "openai_compatible",
		BaseURL: "http://127.0.0.1:8000",
		Models:  []string{"qwen2.5-coder"},
	})
	require.NoError(t, err)

	assert.True(t, provider.Local())
	assert.Contains(t, providerRouteCapabilities(provider), modelroute.CapabilityLocal)
}

func TestOpenAICompatibleProviderCapabilityOverrideControlsRouteMetadata(t *testing.T) {
	t.Parallel()

	provider, err := NewOpenAICompatibleProviderWithConfigContext(context.Background(), "vllm", ProviderConfig{
		Type:         "openai_compatible",
		BaseURL:      "https://vllm.internal.example",
		Models:       []string{"qwen2.5-coder"},
		Capabilities: []string{modelroute.CapabilityChat, modelroute.CapabilityJSONSchema},
	})
	require.NoError(t, err)

	capabilities := provider.Capabilities()
	assert.True(t, capabilities.SupportsChatCompletions)
	assert.True(t, capabilities.SupportsJSONSchema)
	assert.False(t, capabilities.SupportsTools)
	assert.NotContains(t, providerRouteCapabilities(provider), modelroute.CapabilityTools)
	assert.Contains(t, providerRouteCapabilities(provider), modelroute.CapabilityJSONSchema)
}

func TestOpenAICompatibleProviderCapabilityOverrideRejectsUnknownCapability(t *testing.T) {
	t.Parallel()

	_, err := NewOpenAICompatibleProviderWithConfigContext(context.Background(), "vllm", ProviderConfig{
		Type:         "openai_compatible",
		BaseURL:      "https://vllm.internal.example",
		Models:       []string{"qwen2.5-coder"},
		Capabilities: []string{modelroute.CapabilityChat, "teleport"},
	})

	require.Error(t, err)
	assert.Contains(t, err.Error(), `openai-compatible provider "vllm" capabilities contains unknown capability "teleport"`)
	assert.Contains(t, err.Error(), "valid: text,chat,tools")
}

func TestOpenAICompatibleProviderCapabilityOverrideCannotAdvertiseStreamingWithoutStreamProvider(t *testing.T) {
	t.Parallel()

	provider, err := NewOpenAICompatibleProviderWithConfigContext(context.Background(), "vllm", ProviderConfig{
		Type:         "openai_compatible",
		BaseURL:      "https://vllm.internal.example",
		Models:       []string{"qwen2.5-coder"},
		Capabilities: []string{modelroute.CapabilityChat, modelroute.CapabilityStreaming},
	})
	require.NoError(t, err)

	capabilities := provider.Capabilities()
	assert.True(t, capabilities.SupportsChatCompletions)
	assert.False(t, capabilities.SupportsStreaming)
	assert.NotContains(t, providerRouteCapabilities(provider), modelroute.CapabilityStreaming)
}

func TestOpenAICompatibleProviderCapabilityOverrideRejectsUnsupportedTools(t *testing.T) {
	t.Parallel()

	provider, err := NewOpenAICompatibleProviderWithConfigContext(context.Background(), "vllm", ProviderConfig{
		Type:         "openai_compatible",
		BaseURL:      "https://vllm.internal.example",
		Models:       []string{"qwen2.5-coder"},
		Capabilities: []string{modelroute.CapabilityChat},
	})
	require.NoError(t, err)

	_, err = provider.Complete(context.Background(), CompleteParams{
		Model:    "qwen2.5-coder",
		Messages: []Message{{Role: RoleUser, Content: "hello"}},
		Tools:    DefaultTools(),
	})

	require.Error(t, err)
	assert.Contains(t, err.Error(), "CompleteParams.Tools is unsupported")
}

func TestOpenAICompatibleProviderCapabilityOverrideOmitsDisabledReasoning(t *testing.T) {
	t.Parallel()

	provider, err := NewOpenAICompatibleProviderWithConfigContext(context.Background(), "vllm", ProviderConfig{
		Type:         "openai_compatible",
		BaseURL:      "https://vllm.internal.example",
		Models:       []string{"qwen2.5-coder"},
		Capabilities: []string{modelroute.CapabilityChat},
	})
	require.NoError(t, err)

	for _, level := range []string{ReasoningLevelDefault, reasoningLevelNone, reasoningLevelMinimal} {
		req, err := provider.buildRequest(CompleteParams{
			Model:          "qwen2.5-coder",
			Messages:       []Message{{Role: RoleUser, Content: "hello"}},
			ReasoningLevel: level,
		})
		require.NoError(t, err, level)
		assert.Empty(t, req.ReasoningEffort, level)
	}
}

func TestOpenAICompatibleProviderCapabilityOverrideRejectsRequestedReasoning(t *testing.T) {
	t.Parallel()

	provider, err := NewOpenAICompatibleProviderWithConfigContext(context.Background(), "vllm", ProviderConfig{
		Type:         "openai_compatible",
		BaseURL:      "https://vllm.internal.example",
		Models:       []string{"qwen2.5-coder"},
		Capabilities: []string{modelroute.CapabilityChat},
	})
	require.NoError(t, err)

	_, err = provider.buildRequest(CompleteParams{
		Model:          "qwen2.5-coder",
		Messages:       []Message{{Role: RoleUser, Content: "hello"}},
		ReasoningLevel: reasoningLevelHigh,
	})

	require.Error(t, err)
	assert.Contains(t, err.Error(), "CompleteParams.ReasoningLevel is unsupported")
}

func TestOpenAICompatibleProviderCapabilityOverrideRejectsMissingChat(t *testing.T) {
	t.Parallel()

	provider, err := NewOpenAICompatibleProviderWithConfigContext(context.Background(), "embedder", ProviderConfig{
		Type:         "openai_compatible",
		BaseURL:      "https://embeddings.internal.example",
		Models:       []string{"embed-only"},
		Capabilities: []string{modelroute.CapabilityEmbeddings},
	})
	require.NoError(t, err)

	_, err = provider.Complete(context.Background(), CompleteParams{
		Model:    "embed-only",
		Messages: []Message{{Role: RoleUser, Content: "hello"}},
	})

	require.Error(t, err)
	assert.Contains(t, err.Error(), "chat completions are unsupported")
}

func TestOpenAIProviderRespectsExplicitLocalFlag(t *testing.T) {
	t.Setenv("OPENAI_API_KEY", "sk-test")

	provider, err := NewOpenAIProviderWithConfigContext(context.Background(), ProviderConfig{
		BaseURL: "https://openai-compatible.internal.example",
		Local:   true,
	})
	require.NoError(t, err)

	assert.True(t, provider.Local())
	assert.Contains(t, providerRouteCapabilities(provider), modelroute.CapabilityLocal)
}

func TestOpenAICompatibleProviderRespectsExplicitLocalFlag(t *testing.T) {
	t.Parallel()

	provider, err := NewOpenAICompatibleProviderWithConfigContext(context.Background(), "tgi", ProviderConfig{
		Type:    "openai_compatible",
		BaseURL: "https://tgi.internal.example",
		Local:   true,
		Models:  []string{"self-hosted"},
	})
	require.NoError(t, err)

	assert.True(t, provider.Local())
}

func TestAutoRegisterWithConfigContext_AzureOpenAIProviderPathAndAuth(t *testing.T) {
	apiKeyEnv := "ATTELER_AZURE_" + "KEY"
	t.Setenv(apiKeyEnv, "azure-key")

	var (
		gotAPIKey        string
		gotAuthorization string
		gotPath          string
		gotAPIVersion    string
		gotReq           openaiRequest
	)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAPIKey = r.Header.Get("api-key")
		gotAuthorization = r.Header.Get("Authorization")
		gotPath = r.URL.Path
		gotAPIVersion = r.URL.Query().Get("api-version")
		assert.NoError(t, json.NewDecoder(r.Body).Decode(&gotReq))

		resp := openaiResponse{
			Model: "deployment-a",
			Choices: []struct {
				FinishReason string `json:"finish_reason"`
				Message      struct {
					Content   string           `json:"content"`
					ToolCalls []openaiToolCall `json:"tool_calls,omitempty"`
				} `json:"message"`
			}{{
				FinishReason: "stop",
				Message: struct {
					Content   string           `json:"content"`
					ToolCalls []openaiToolCall `json:"tool_calls,omitempty"`
				}{Content: "azure answer"},
			}},
		}

		w.Header().Set("Content-Type", "application/json")
		assert.NoError(t, json.NewEncoder(w).Encode(resp))
	}))
	defer srv.Close()

	registry := AutoRegisterWithConfigContext(context.Background(), AutoRegisterConfig{
		Providers: map[string]ProviderConfig{
			"azure": {
				Type:       "azure_openai",
				BaseURL:    srv.URL,
				APIKeyEnv:  apiKeyEnv,
				APIVersion: "2025-01-01-preview",
				Models:     []string{"deployment-a"},
			},
		},
		DefaultProvider:        "azure",
		DefaultModel:           "azure/deployment-a",
		DisableReadinessChecks: true,
	})

	resp, err := registry.Complete(context.Background(), CompleteParams{
		Model:    "azure/deployment-a",
		Messages: []Message{{Role: RoleUser, Content: "hello"}},
	})
	require.NoError(t, err)

	assert.Equal(t, "azure answer", resp.Content)
	assert.Equal(t, "azure", resp.Provider)
	assert.Equal(t, "/openai/deployments/deployment-a/chat/completions", gotPath)
	assert.Equal(t, "2025-01-01-preview", gotAPIVersion)
	assert.Equal(t, "azure-key", gotAPIKey)
	assert.Empty(t, gotAuthorization)
	assert.Empty(t, gotReq.Model)
}
