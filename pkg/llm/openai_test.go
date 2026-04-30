package llm

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestOpenAIProvider_Complete(t *testing.T) {
	var gotReq openaiRequest
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
		resp.Usage.CompletionTokens = 3
		w.Header().Set("Content-Type", "application/json")
		if err := json.NewEncoder(w).Encode(resp); err != nil {
			t.Fatalf("encode response: %v", err)
		}
	}))
	defer srv.Close()

	p := &OpenAIProvider{
		apiKey:  "sk-test",
		bearer:  false,
		baseURL: srv.URL,
		client:  srv.Client(),
	}

	resp, err := p.Complete(context.Background(), CompleteParams{
		Model:       "gpt-4.1",
		MaxTokens:   200,
		Temperature: 0.7,
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
	if resp.InputTokens != 8 || resp.OutputTokens != 3 {
		t.Errorf("tokens = %d/%d, want 8/3", resp.InputTokens, resp.OutputTokens)
	}

	// Verify request shape — system message stays inline for OpenAI.
	if len(gotReq.Messages) != 2 {
		t.Fatalf("messages len = %d, want 2", len(gotReq.Messages))
	}
	if gotReq.Messages[0].Role != "system" {
		t.Errorf("messages[0].role = %q", gotReq.Messages[0].Role)
	}
	if gotReq.MaxTokens != 200 {
		t.Errorf("max_tokens = %d", gotReq.MaxTokens)
	}
	if gotReq.Temperature == nil || *gotReq.Temperature != 0.7 {
		t.Errorf("temperature = %v", gotReq.Temperature)
	}

	// Verify auth header.
	if gotHeaders.Get("Authorization") != "Bearer sk-test" {
		t.Errorf("Authorization = %q", gotHeaders.Get("Authorization"))
	}
}

func TestOpenAIProvider_HTTPError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		if _, err := w.Write([]byte(`{"error":{"type":"invalid_api_key","message":"bad key"}}`)); err != nil {
			t.Fatalf("write response: %v", err)
		}
	}))
	defer srv.Close()

	p := &OpenAIProvider{apiKey: "k", baseURL: srv.URL, client: srv.Client()}
	_, err := p.Complete(context.Background(), CompleteParams{
		Model:    "gpt-4.1",
		Messages: []Message{{Role: RoleUser, Content: "hi"}},
	})
	if err == nil {
		t.Fatal("expected error on 401")
	}
}

func TestOpenAIProvider_OmitsZeroMaxTokens(t *testing.T) {
	var gotReq openaiRequest

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Fatalf("read body: %v", err)
		}
		if err := json.Unmarshal(body, &gotReq); err != nil {
			t.Fatalf("unmarshal request: %v", err)
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
			t.Fatalf("encode response: %v", err)
		}
	}))
	defer srv.Close()

	p := &OpenAIProvider{apiKey: "k", baseURL: srv.URL, client: srv.Client()}
	_, err := p.Complete(context.Background(), CompleteParams{
		Model:    "gpt-4.1",
		Messages: []Message{{Role: RoleUser, Content: "hi"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if gotReq.MaxTokens != 0 {
		t.Errorf("max_tokens = %d, want 0 (omitted)", gotReq.MaxTokens)
	}
}

func TestOpenAIProvider_NameAndModels(t *testing.T) {
	p := &OpenAIProvider{}
	if p.Name() != "openai" {
		t.Errorf("Name() = %q", p.Name())
	}
	if len(p.Models()) == 0 {
		t.Error("Models() returned empty")
	}
}
