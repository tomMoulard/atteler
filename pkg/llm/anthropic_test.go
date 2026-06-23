package llm

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	llmevents "github.com/tommoulard/atteler/pkg/events"
)

func TestAnthropicProvider_Complete(t *testing.T) {
	t.Parallel()

	var (
		gotReq     anthropicRequest
		gotBody    []byte
		gotHeaders http.Header
	)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotHeaders = r.Header.Clone()

		body, err := io.ReadAll(r.Body)
		if !assert.NoError(t, err) {
			return
		}

		gotBody = body

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

	var log bytes.Buffer

	ctx := llmevents.WithEmitter(context.Background(), llmevents.NewRunnerWithLogger(nil, &log), llmevents.Event{})

	resp, err := p.Complete(ctx, CompleteParams{
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

	assert.Equal(t, providerAnthropic, resp.Provider)

	if resp.InputTokens != 20 || resp.CachedInputTokens != 6 || resp.CacheWriteInputTokens != 4 || resp.OutputTokens != 5 {
		assert.Failf(t, "assertion failed", "tokens = %d/%d/%d/%d, want 20/6/4/5", resp.InputTokens, resp.CachedInputTokens, resp.CacheWriteInputTokens, resp.OutputTokens)
	}

	// Verify request shape.
	if gotReq.Model != "claude-sonnet-4-20250514" {
		assert.Failf(t, "assertion failed", "model = %q", gotReq.Model)
	}

	assert.JSONEq(t, `{
		"temperature": 1,
		"model": "claude-sonnet-4-20250514",
		"system": [
			{"type": "text", "text": "you are helpful", "cache_control": {"type": "ephemeral"}}
		],
		"thinking": {"type": "enabled", "budget_tokens": 2048},
		"messages": [
			{"role": "user", "content": [
				{"type": "text", "text": "hi", "cache_control": {"type": "ephemeral"}}
			]}
		],
		"max_tokens": 4096
	}`, string(gotBody))

	if len(gotReq.Messages) != 1 || gotReq.Messages[0].Role != "user" {
		assert.Failf(t, "assertion failed", "messages = %+v", gotReq.Messages)
	}

	require.Len(t, gotReq.System, 1)
	assert.Equal(t, "you are helpful", gotReq.System[0].Text)
	require.NotNil(t, gotReq.System[0].CacheControl)
	assert.Equal(t, "ephemeral", gotReq.System[0].CacheControl.Type)

	if gotReq.MaxTokens != 4096 {
		assert.Failf(t, "assertion failed", "max_tokens = %d", gotReq.MaxTokens)
	}

	if gotReq.Thinking == nil || gotReq.Thinking.Type != "enabled" || gotReq.Thinking.BudgetTokens != 2048 {
		assert.Failf(t, "assertion failed", "thinking = %+v, want enabled/2048", gotReq.Thinking)
	}

	if gotReq.Temperature == nil || *gotReq.Temperature != 1 {
		assert.Failf(t, "assertion failed", "temperature = %v", gotReq.Temperature)
	}

	assert.Contains(t, log.String(), "option_adjustments")
	assert.Contains(t, log.String(), "Temperature coerced")

	// Verify headers.
	if gotHeaders.Get("X-Api-Key") != "test-key" {
		assert.Failf(t, "assertion failed", "X-Api-Key = %q", gotHeaders.Get("X-Api-Key"))
	}

	if gotHeaders.Get("anthropic-version") != defaultAnthropicVersion {
		assert.Failf(t, "assertion failed", "anthropic-version = %q", gotHeaders.Get("anthropic-version"))
	}
}

func TestAnthropicProvider_CompleteStream(t *testing.T) {
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

		w.Header().Set("Content-Type", "text/event-stream")
		_, err = io.WriteString(w, `event: message_start
data: {"type":"message_start","message":{"model":"claude-stream","usage":{"input_tokens":5,"cache_creation_input_tokens":2,"cache_read_input_tokens":3}}}

event: content_block_delta
data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"hel"}}

event: content_block_delta
data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"lo"}}

event: message_delta
data: {"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"output_tokens":2}}

event: message_stop
data: {"type":"message_stop"}

`)
		assert.NoError(t, err)
	}))
	defer srv.Close()

	p := &AnthropicProvider{
		apiKey:  "test-key",
		baseURL: srv.URL,
		client:  srv.Client(),
	}

	ch, err := p.CompleteStream(context.Background(), CompleteParams{
		Model:    "claude-stream",
		Messages: []Message{{Role: RoleUser, Content: "hi"}},
	})
	require.NoError(t, err)

	chunks := drainChunks(ch)
	require.Len(t, chunks, 3)
	assert.Equal(t, "hel", chunks[0].Content)
	assert.False(t, chunks[0].Done)
	assert.Equal(t, "lo", chunks[1].Content)
	assert.False(t, chunks[1].Done)
	assert.True(t, chunks[2].Done)

	resp, err := CollectStream(chunksToStream(chunks))
	require.NoError(t, err)

	assert.True(t, gotReq.Stream)
	assert.Equal(t, "text/event-stream", gotHeaders.Get("Accept"))
	assert.Equal(t, "test-key", gotHeaders.Get("X-Api-Key"))
	assert.Equal(t, "hello", resp.Content)
	assert.Equal(t, providerAnthropic, resp.Provider)
	assert.Equal(t, "claude-stream", resp.Model)
	assert.Equal(t, StopEndTurn, resp.StopReason)
	assert.Equal(t, 10, resp.InputTokens)
	assert.Equal(t, 3, resp.CachedInputTokens)
	assert.Equal(t, 2, resp.CacheWriteInputTokens)
	assert.Equal(t, 2, resp.OutputTokens)
	assert.Positive(t, resp.FirstTokenLatency)
}

func TestAnthropicProvider_CompleteStreamToolCall(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, err := io.WriteString(w, `event: message_start
data: {"type":"message_start","message":{"model":"claude-tools","usage":{"input_tokens":5}}}

event: content_block_start
data: {"type":"content_block_start","index":0,"content_block":{"type":"tool_use","id":"toolu_1","name":"bash","input":{}}}

event: content_block_delta
data: {"type":"content_block_delta","index":0,"delta":{"type":"input_json_delta","partial_json":"{\"command\":\""}}

event: content_block_delta
data: {"type":"content_block_delta","index":0,"delta":{"type":"input_json_delta","partial_json":"echo hi\"}"}}

event: content_block_stop
data: {"type":"content_block_stop","index":0}

event: message_delta
data: {"type":"message_delta","delta":{"stop_reason":"tool_use"},"usage":{"output_tokens":1}}

event: message_stop
data: {"type":"message_stop"}

`)
		assert.NoError(t, err)
	}))
	defer srv.Close()

	p := &AnthropicProvider{
		apiKey:  "test-key",
		baseURL: srv.URL,
		client:  srv.Client(),
	}

	ch, err := p.CompleteStream(context.Background(), CompleteParams{
		Model:    "claude-tools",
		Messages: []Message{{Role: RoleUser, Content: "run"}},
	})
	require.NoError(t, err)

	resp, err := CollectStream(ch)
	require.NoError(t, err)

	assert.Equal(t, providerAnthropic, resp.Provider)
	assert.Equal(t, "claude-tools", resp.Model)
	assert.Equal(t, StopToolUse, resp.StopReason)
	assert.Equal(t, 5, resp.InputTokens)
	assert.Equal(t, 1, resp.OutputTokens)
	require.Len(t, resp.ToolCalls, 1)
	assert.Equal(t, "toolu_1", resp.ToolCalls[0].ID)
	assert.Equal(t, "bash", resp.ToolCalls[0].Name)
	assert.Equal(t, map[string]any{"command": "echo hi"}, resp.ToolCalls[0].Input)
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
		w.Header().Set("request-id", "req-anthropic")
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

	var providerErr *ProviderError
	require.ErrorAs(t, err, &providerErr)
	assert.Equal(t, providerAnthropic, providerErr.Provider)
	assert.Equal(t, http.StatusTooManyRequests, providerErr.StatusCode)
	assert.Equal(t, "req-anthropic", providerErr.RequestID)
	assert.Equal(t, RetryabilityRetryable, providerErr.Retryability)
	assert.Contains(t, providerErr.Message, "rate_limit")
}

func TestAnthropicProvider_FetchModelsHTTPErrorIsTyped(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("request-id", "req-anthropic-models")
		w.WriteHeader(http.StatusServiceUnavailable)

		if _, err := w.Write([]byte(`{"error":{"type":"overloaded_error","message":"busy"}}`)); err != nil {
			return // best-effort in test handler
		}
	}))
	defer srv.Close()

	p := &AnthropicProvider{apiKey: "k", baseURL: srv.URL, client: srv.Client()}

	_, err := p.FetchModels(context.Background())
	require.Error(t, err)

	var providerErr *ProviderError
	require.ErrorAs(t, err, &providerErr)
	assert.Equal(t, providerAnthropic, providerErr.Provider)
	assert.Equal(t, http.StatusServiceUnavailable, providerErr.StatusCode)
	assert.Equal(t, "req-anthropic-models", providerErr.RequestID)
	assert.Equal(t, RetryabilityRetryable, providerErr.Retryability)
	assert.Contains(t, providerErr.Message, "overloaded_error")
}

func TestAnthropicProvider_ErrorPayloadIsTyped(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Retry-After", "6")
		w.Header().Set("request-id", "req-anthropic-payload")
		w.Header().Set("Content-Type", "application/json")

		resp := anthropicResponse{}
		resp.Error = &struct {
			Type    string `json:"type"`
			Message string `json:"message"`
		}{
			Type:    "overloaded_error",
			Message: "busy",
		}

		assert.NoError(t, json.NewEncoder(w).Encode(resp))
	}))
	defer srv.Close()

	p := &AnthropicProvider{apiKey: "k", baseURL: srv.URL, client: srv.Client()}

	_, err := p.Complete(context.Background(), CompleteParams{
		Model:    "claude-sonnet-4-20250514",
		Messages: []Message{{Role: RoleUser, Content: "hi"}},
	})
	require.Error(t, err)

	var providerErr *ProviderError
	require.ErrorAs(t, err, &providerErr)
	assert.Equal(t, providerAnthropic, providerErr.Provider)
	assert.Equal(t, http.StatusOK, providerErr.StatusCode)
	assert.Equal(t, 6*time.Second, providerErr.RetryAfter)
	assert.Equal(t, "req-anthropic-payload", providerErr.RequestID)
	assert.Equal(t, RetryabilityRetryable, providerErr.Retryability)
	assert.Equal(t, "overloaded_error: busy", providerErr.Message)
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

func TestBuildAnthropicRequest_AddsPromptCacheBreakpoints(t *testing.T) {
	t.Parallel()

	req, err := buildAnthropicRequestForProvider(providerAnthropic, CompleteParams{
		Model: "claude-sonnet-4-20250514",
		Messages: []Message{
			{Role: RoleSystem, Content: "stable system"},
			{Role: RoleUser, Content: "first turn"},
			{
				Role:    RoleAssistant,
				Content: "checking",
				ToolCalls: []ToolCall{{
					ID:    "call-1",
					Name:  "lookup",
					Input: map[string]any{"query": "go"},
				}},
			},
		},
		Tools: []ToolDefinition{
			{Name: "first", Description: "First tool", Parameters: map[string]any{"type": "object"}},
			{Name: "lookup", Description: "Look up a value", Parameters: map[string]any{"type": "object"}},
		},
	})
	require.NoError(t, err)

	require.Len(t, req.System, 1)
	assert.Equal(t, "stable system", req.System[0].Text)
	require.NotNil(t, req.System[0].CacheControl)
	assert.Equal(t, "ephemeral", req.System[0].CacheControl.Type)

	require.Len(t, req.Tools, 2)
	assert.Nil(t, req.Tools[0].CacheControl)
	require.NotNil(t, req.Tools[1].CacheControl)
	assert.Equal(t, "ephemeral", req.Tools[1].CacheControl.Type)

	require.Len(t, req.Messages, 2)
	assert.JSONEq(t, `"first turn"`, string(req.Messages[0].Content))
	assert.JSONEq(t, `[
		{"type": "text", "text": "checking"},
		{"type": "tool_use", "id": "call-1", "name": "lookup", "input": {"query": "go"}, "cache_control": {"type": "ephemeral"}}
	]`, string(req.Messages[1].Content))
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
