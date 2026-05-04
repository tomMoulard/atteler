package llm

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

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
				Message struct {
					Content string `json:"content"`
				} `json:"message"`
			}{{Message: struct {
				Content string `json:"content"`
			}{Content: "hello back"}}},
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
				Message struct {
					Content string `json:"content"`
				} `json:"message"`
			}{{Message: struct {
				Content string `json:"content"`
			}{Content: "ok"}}},
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
}

func TestOpenAIProvider_ConfigBaseURL(t *testing.T) {
	t.Setenv("OPENAI_API_KEY", "sk-test")
	t.Setenv("OPENAI_BASE_URL", "")

	p, err := NewOpenAIProviderWithConfig(ProviderConfig{BaseURL: "https://openai.config"})
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

	p, err := NewOpenAIProviderWithConfig(ProviderConfig{BaseURL: "https://openai.config"})
	if err != nil {
		require.NoError(t, err)
	}

	if p.baseURL != "https://openai.env" {
		assert.Failf(t, "assertion failed", "baseURL = %q, want env value", p.baseURL)
	}
}
