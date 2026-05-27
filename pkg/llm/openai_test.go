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
