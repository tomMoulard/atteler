package llm

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestAnthropicProvider_Complete(t *testing.T) {
	var gotReq anthropicRequest
	var gotHeaders http.Header

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotHeaders = r.Header.Clone()
		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Fatalf("read body: %v", err)
		}
		if err := json.Unmarshal(body, &gotReq); err != nil {
			t.Fatalf("unmarshal request: %v", err)
		}

		resp := anthropicResponse{
			Model: gotReq.Model,
			Content: []struct {
				Text string `json:"text"`
			}{{Text: "hello back"}},
		}
		resp.Usage.InputTokens = 10
		resp.Usage.OutputTokens = 5
		w.Header().Set("Content-Type", "application/json")
		if err := json.NewEncoder(w).Encode(resp); err != nil {
			t.Fatalf("encode response: %v", err)
		}
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
		Model:       "claude-sonnet-4-20250514",
		MaxTokens:   100,
		Temperature: &temperature,
		Messages: []Message{
			{Role: RoleSystem, Content: "you are helpful"},
			{Role: RoleUser, Content: "hi"},
		},
	})
	if err != nil {
		t.Fatal(err)
	}

	// Verify response.
	if resp.Content != "hello back" {
		t.Errorf("content = %q, want %q", resp.Content, "hello back")
	}
	if resp.InputTokens != 10 || resp.OutputTokens != 5 {
		t.Errorf("tokens = %d/%d, want 10/5", resp.InputTokens, resp.OutputTokens)
	}

	// Verify request shape.
	if gotReq.Model != "claude-sonnet-4-20250514" {
		t.Errorf("model = %q", gotReq.Model)
	}
	if gotReq.System != "you are helpful" {
		t.Errorf("system = %q", gotReq.System)
	}
	if len(gotReq.Messages) != 1 || gotReq.Messages[0].Role != "user" {
		t.Errorf("messages = %+v", gotReq.Messages)
	}
	if gotReq.MaxTokens != 100 {
		t.Errorf("max_tokens = %d", gotReq.MaxTokens)
	}
	if gotReq.Temperature == nil || *gotReq.Temperature != 0.5 {
		t.Errorf("temperature = %v", gotReq.Temperature)
	}

	// Verify headers.
	if gotHeaders.Get("X-Api-Key") != "test-key" {
		t.Errorf("X-Api-Key = %q", gotHeaders.Get("X-Api-Key"))
	}
	if gotHeaders.Get("anthropic-version") != defaultAnthropicVersion {
		t.Errorf("anthropic-version = %q", gotHeaders.Get("anthropic-version"))
	}
}

func TestAnthropicProvider_BearerAuth(t *testing.T) {
	var gotHeaders http.Header

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotHeaders = r.Header.Clone()
		w.Header().Set("Content-Type", "application/json")
		if err := json.NewEncoder(w).Encode(anthropicResponse{
			Content: []struct {
				Text string `json:"text"`
			}{{Text: "ok"}},
		}); err != nil {
			t.Fatalf("encode response: %v", err)
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
		t.Fatal(err)
	}

	if gotHeaders.Get("Authorization") != "Bearer bearer-tok" {
		t.Errorf("Authorization = %q, want Bearer bearer-tok", gotHeaders.Get("Authorization"))
	}
	if gotHeaders.Get("X-Api-Key") != "" {
		t.Error("X-Api-Key should be empty for bearer auth")
	}
	if gotHeaders.Get("anthropic-beta") != anthropicOAuthBetas {
		t.Errorf("anthropic-beta = %q, want %q", gotHeaders.Get("anthropic-beta"), anthropicOAuthBetas)
	}
}

func TestAnthropicProvider_HTTPError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusTooManyRequests)
		if _, err := w.Write([]byte(`{"error":{"type":"rate_limit","message":"slow down"}}`)); err != nil {
			t.Fatalf("write response: %v", err)
		}
	}))
	defer srv.Close()

	p := &AnthropicProvider{apiKey: "k", baseURL: srv.URL, client: srv.Client()}
	_, err := p.Complete(context.Background(), CompleteParams{
		Model:    "claude-sonnet-4-20250514",
		Messages: []Message{{Role: RoleUser, Content: "hi"}},
	})
	if err == nil {
		t.Fatal("expected error on 429")
	}
}

func TestAnthropicProvider_DefaultMaxTokens(t *testing.T) {
	var gotReq anthropicRequest

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Fatalf("read body: %v", err)
		}
		if err := json.Unmarshal(body, &gotReq); err != nil {
			t.Fatalf("unmarshal request: %v", err)
		}
		w.Header().Set("Content-Type", "application/json")
		if err := json.NewEncoder(w).Encode(anthropicResponse{
			Content: []struct {
				Text string `json:"text"`
			}{{Text: "ok"}},
		}); err != nil {
			t.Fatalf("encode response: %v", err)
		}
	}))
	defer srv.Close()

	p := &AnthropicProvider{apiKey: "k", baseURL: srv.URL, client: srv.Client()}
	_, err := p.Complete(context.Background(), CompleteParams{
		Model:    "claude-sonnet-4-20250514",
		Messages: []Message{{Role: RoleUser, Content: "hi"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if gotReq.MaxTokens != 4096 {
		t.Errorf("default max_tokens = %d, want 4096", gotReq.MaxTokens)
	}
	if gotReq.Temperature != nil {
		t.Errorf("temperature = %v, want omitted", *gotReq.Temperature)
	}
	if gotReq.TopP != nil {
		t.Errorf("top_p = %v, want omitted", *gotReq.TopP)
	}
}

func TestAnthropicProvider_NameAndModels(t *testing.T) {
	p := &AnthropicProvider{}
	if p.Name() != providerAnthropic {
		t.Errorf("Name() = %q", p.Name())
	}
	if len(p.Models()) == 0 {
		t.Error("Models() returned empty")
	}
}

func TestAnthropicProvider_ConfigBaseURL(t *testing.T) {
	t.Setenv("ANTHROPIC_API_KEY", "sk-ant-test")
	t.Setenv("ANTHROPIC_BASE_URL", "")

	p, err := NewAnthropicProviderWithConfig(ProviderConfig{BaseURL: "https://anthropic.config"})
	if err != nil {
		t.Fatal(err)
	}
	if p.baseURL != "https://anthropic.config" {
		t.Errorf("baseURL = %q, want config value", p.baseURL)
	}
}

func TestAnthropicProvider_EnvBaseURLOverridesConfig(t *testing.T) {
	t.Setenv("ANTHROPIC_API_KEY", "sk-ant-test")
	t.Setenv("ANTHROPIC_BASE_URL", "https://anthropic.env")

	p, err := NewAnthropicProviderWithConfig(ProviderConfig{BaseURL: "https://anthropic.config"})
	if err != nil {
		t.Fatal(err)
	}
	if p.baseURL != "https://anthropic.env" {
		t.Errorf("baseURL = %q, want env value", p.baseURL)
	}
}
