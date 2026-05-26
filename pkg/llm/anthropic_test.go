package llm

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestAnthropicProvider_Complete(t *testing.T) {
	t.Parallel()

	var (
		gotReq     anthropicRequest
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

		resp := anthropicResponse{
			Model: gotReq.Model,
			Content: []anthropicContentBlock{
				{Type: "text", Text: "hello back"},
			},
		}
		resp.Usage.InputTokens = 10
		resp.Usage.CacheCreationInputTokens = 4
		resp.Usage.CacheReadInputTokens = 6
		resp.Usage.OutputTokens = 5

		w.Header().Set("Content-Type", "application/json")
		assert.NoError(t, json.NewEncoder(w).Encode(resp))
	}))
	defer srv.Close()

	p := &AnthropicProvider{
		apiKey:  "test-key",
		bearer:  false,
		baseURL: srv.URL,
		client:  srv.Client(),
	}
	temperature := 0.5

	resp, err := p.Complete(context.Background(), CompleteParams{
		Model:          "claude-sonnet-4-20250514",
		MaxTokens:      4096,
		Temperature:    &temperature,
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

	if resp.InputTokens != 20 || resp.CachedInputTokens != 6 || resp.CacheWriteInputTokens != 4 || resp.OutputTokens != 5 {
		assert.Failf(t, "assertion failed", "tokens = %d/%d/%d/%d, want 20/6/4/5", resp.InputTokens, resp.CachedInputTokens, resp.CacheWriteInputTokens, resp.OutputTokens)
	}

	// Verify request shape.
	if gotReq.Model != "claude-sonnet-4-20250514" {
		assert.Failf(t, "assertion failed", "model = %q", gotReq.Model)
	}

	if gotReq.System != "you are helpful" {
		assert.Failf(t, "assertion failed", "system = %q", gotReq.System)
	}

	if len(gotReq.Messages) != 1 || gotReq.Messages[0].Role != "user" {
		assert.Failf(t, "assertion failed", "messages = %+v", gotReq.Messages)
	}

	if gotReq.MaxTokens != 4096 {
		assert.Failf(t, "assertion failed", "max_tokens = %d", gotReq.MaxTokens)
	}

	if gotReq.Thinking == nil || gotReq.Thinking.Type != "enabled" || gotReq.Thinking.BudgetTokens != 2048 {
		assert.Failf(t, "assertion failed", "thinking = %+v, want enabled/2048", gotReq.Thinking)
	}

	if gotReq.Temperature == nil || *gotReq.Temperature != 0.5 {
		assert.Failf(t, "assertion failed", "temperature = %v", gotReq.Temperature)
	}

	// Verify headers.
	if gotHeaders.Get("X-Api-Key") != "test-key" {
		assert.Failf(t, "assertion failed", "X-Api-Key = %q", gotHeaders.Get("X-Api-Key"))
	}

	if gotHeaders.Get("anthropic-version") != defaultAnthropicVersion {
		assert.Failf(t, "assertion failed", "anthropic-version = %q", gotHeaders.Get("anthropic-version"))
	}
}

func TestAnthropicProvider_BearerAuth(t *testing.T) {
	t.Parallel()

	var gotHeaders http.Header

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotHeaders = r.Header.Clone()
		w.Header().Set("Content-Type", "application/json")

		if err := json.NewEncoder(w).Encode(anthropicResponse{
			Content: []anthropicContentBlock{
				{Type: "text", Text: "ok"},
			},
		}); err != nil {
			assert.NoError(t, err)
		}
	}))
	defer srv.Close()

	p := &AnthropicProvider{
		apiKey:  "bearer-tok",
		bearer:  true,
		baseURL: srv.URL,
		client:  srv.Client(),
	}

	_, err := p.Complete(context.Background(), CompleteParams{
		Model:    "claude-sonnet-4-20250514",
		Messages: []Message{{Role: RoleUser, Content: "hi"}},
	})
	if err != nil {
		require.NoError(t, err)
	}

	if gotHeaders.Get("Authorization") != "Bearer bearer-tok" {
		assert.Failf(t, "assertion failed", "Authorization = %q, want Bearer bearer-tok", gotHeaders.Get("Authorization"))
	}

	if gotHeaders.Get("X-Api-Key") != "" {
		assert.Fail(t, "X-Api-Key should be empty for bearer auth")
	}

	if gotHeaders.Get("anthropic-beta") != anthropicOAuthBetas {
		assert.Failf(t, "assertion failed", "anthropic-beta = %q, want %q", gotHeaders.Get("anthropic-beta"), anthropicOAuthBetas)
	}
}

func TestAnthropicProvider_ProviderWarningsForBearerBetaRouting(t *testing.T) {
	t.Parallel()

	assert.Empty(t, (&AnthropicProvider{bearer: false}).ProviderWarnings())

	warnings := (&AnthropicProvider{bearer: true}).ProviderWarnings()
	require.Len(t, warnings, 1)
	assert.Contains(t, warnings[0], "beta routing headers")
	assert.Contains(t, warnings[0], "disable_private_adapter")
	assert.Contains(t, warnings[0], "ATTELER_DISABLE_CLAUDE_CODE_ADAPTER")
}

func TestAnthropicProvider_HTTPError(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusTooManyRequests)

		if _, err := w.Write([]byte(`{"error":{"type":"rate_limit","message":"slow down"}}`)); err != nil {
			return // best-effort in test handler
		}
	}))
	defer srv.Close()

	p := &AnthropicProvider{apiKey: "k", baseURL: srv.URL, client: srv.Client()}

	_, err := p.Complete(context.Background(), CompleteParams{
		Model:    "claude-sonnet-4-20250514",
		Messages: []Message{{Role: RoleUser, Content: "hi"}},
	})
	if err == nil {
		require.FailNow(t, "expected error on 429")
	}
}

func TestAnthropicProvider_DefaultMaxTokens(t *testing.T) {
	t.Parallel()

	var gotReq anthropicRequest

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		if !assert.NoError(t, err) {
			return
		}

		if !assert.NoError(t, json.Unmarshal(body, &gotReq)) {
			return
		}

		w.Header().Set("Content-Type", "application/json")

		if err := json.NewEncoder(w).Encode(anthropicResponse{
			Content: []anthropicContentBlock{
				{Type: "text", Text: "ok"},
			},
		}); err != nil {
			assert.NoError(t, err)
		}
	}))
	defer srv.Close()

	p := &AnthropicProvider{apiKey: "k", baseURL: srv.URL, client: srv.Client()}

	_, err := p.Complete(context.Background(), CompleteParams{
		Model:    "claude-sonnet-4-20250514",
		Messages: []Message{{Role: RoleUser, Content: "hi"}},
	})
	if err != nil {
		require.NoError(t, err)
	}

	if gotReq.MaxTokens != 4096 {
		assert.Failf(t, "assertion failed", "default max_tokens = %d, want 4096", gotReq.MaxTokens)
	}

	if gotReq.Temperature != nil {
		assert.Failf(t, "assertion failed", "temperature = %v, want omitted", *gotReq.Temperature)
	}

	if gotReq.TopP != nil {
		assert.Failf(t, "assertion failed", "top_p = %v, want omitted", *gotReq.TopP)
	}

	if gotReq.Thinking != nil {
		assert.Failf(t, "assertion failed", "thinking = %+v, want omitted", gotReq.Thinking)
	}
}

func TestAnthropicProvider_ReasoningRequiresThinkingBudgetRoom(t *testing.T) {
	t.Parallel()

	p := &AnthropicProvider{apiKey: "k", baseURL: "http://127.0.0.1", client: http.DefaultClient}

	_, err := p.Complete(context.Background(), CompleteParams{
		Model:          "claude-sonnet-4-20250514",
		MaxTokens:      100,
		ReasoningLevel: "low",
		Messages:       []Message{{Role: RoleUser, Content: "hi"}},
	})
	if err == nil || !strings.Contains(err.Error(), "max_tokens greater than 1024") {
		require.Failf(t, "unexpected failure", "error = %v, want max_tokens reasoning error", err)
	}
}

func TestAnthropicProvider_NameAndModels(t *testing.T) {
	t.Parallel()

	p := &AnthropicProvider{}
	if p.Name() != providerAnthropic {
		assert.Failf(t, "assertion failed", "Name() = %q", p.Name())
	}

	if len(p.Models()) == 0 {
		assert.Fail(t, "Models() returned empty")
	}

	assert.Contains(t, p.Models(), "claude-opus-4-7")
}

func TestRegistry_ProviderModelsLiveFetchReplacesAnthropicCatalogFallback(t *testing.T) {
	t.Parallel()

	var gotHeaders http.Header

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotHeaders = r.Header.Clone()
		assert.Equal(t, "/v1/models", r.URL.Path)

		w.Header().Set("Content-Type", "application/json")
		assert.NoError(t, json.NewEncoder(w).Encode(anthropicModelsResponse{
			Data: []struct {
				ID string `json:"id"`
			}{
				{ID: "claude-live-only"},
			},
		}))
	}))
	defer srv.Close()

	registry := NewRegistry()
	registry.Register(&AnthropicProvider{
		apiKey:  "test-api-key",
		baseURL: srv.URL,
		client:  srv.Client(),
	})

	assert.True(t, registry.ProviderHasModel(providerAnthropic, "claude-opus-4-7"))
	assert.False(t, registry.ProviderModelsVerified(providerAnthropic))

	models, err := registry.ProviderModels(context.Background(), providerAnthropic)
	require.NoError(t, err)

	assert.Equal(t, "test-api-key", gotHeaders.Get("X-Api-Key"))
	assert.Equal(t, defaultAnthropicVersion, gotHeaders.Get("anthropic-version"))
	assert.Equal(t, []string{"claude-live-only"}, models)
	assert.True(t, registry.ProviderHasModel(providerAnthropic, "claude-live-only"))
	assert.False(t, registry.ProviderHasModel(providerAnthropic, "claude-opus-4-7"))
	assert.True(t, registry.ProviderModelsVerified(providerAnthropic))
}

func TestAnthropicProvider_ConfigBaseURL(t *testing.T) {
	t.Setenv("ANTHROPIC_API_KEY", "sk-ant-test")
	t.Setenv("ANTHROPIC_BASE_URL", "")

	p, err := NewAnthropicProviderWithConfigContext(context.Background(), ProviderConfig{BaseURL: "https://anthropic.config"})
	if err != nil {
		require.NoError(t, err)
	}

	if p.baseURL != "https://anthropic.config" {
		assert.Failf(t, "assertion failed", "baseURL = %q, want config value", p.baseURL)
	}
}

func TestAnthropicProvider_EnvBaseURLOverridesConfig(t *testing.T) {
	t.Setenv("ANTHROPIC_API_KEY", "sk-ant-test")
	t.Setenv("ANTHROPIC_BASE_URL", "https://anthropic.env")

	p, err := NewAnthropicProviderWithConfigContext(context.Background(), ProviderConfig{BaseURL: "https://anthropic.config"})
	if err != nil {
		require.NoError(t, err)
	}

	if p.baseURL != "https://anthropic.env" {
		assert.Failf(t, "assertion failed", "baseURL = %q, want env value", p.baseURL)
	}
}
