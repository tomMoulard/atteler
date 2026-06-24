package llm

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	llmevents "github.com/tommoulard/atteler/pkg/events"
	"github.com/tommoulard/atteler/pkg/permission"
)

func TestCodexProvider_Complete(t *testing.T) {
	t.Parallel()

	var (
		gotReq     codexResponsesRequest
		gotBody    []byte
		gotHeaders http.Header
		gotPath    string
	)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotHeaders = r.Header.Clone()
		gotPath = r.URL.Path

		body, err := io.ReadAll(r.Body)
		if !assert.NoError(t, err) {
			return
		}

		gotBody = body

		if !assert.NoError(t, json.Unmarshal(gotBody, &gotReq)) {
			return
		}

		w.Header().Set("Content-Type", "text/event-stream")
		writeCodexSSE(t, w, codexFakeSuccess("hello back", "gpt-5.5", 12, 4, 3))
	}))
	defer srv.Close()

	auth := newTestCodexAuth(t)
	p := &CodexProvider{
		client:  srv.Client(),
		auth:    auth,
		baseURL: srv.URL,
		models:  []string{"gpt-5.5"},
	}

	resp, err := p.Complete(context.Background(), CompleteParams{
		Model:          "gpt-5.5",
		ReasoningLevel: "high",
		Messages: []Message{
			{Role: RoleSystem, Content: "be brief"},
			{Role: RoleUser, Content: "say ok"},
		},
	})
	require.NoError(t, err)

	assert.Equal(t, "hello back", resp.Content)
	assert.Equal(t, "gpt-5.5", resp.Model)
	assert.Equal(t, StopEndTurn, resp.StopReason)
	assert.Equal(t, 12, resp.InputTokens)
	assert.Equal(t, 4, resp.CachedInputTokens)
	assert.Equal(t, 3, resp.OutputTokens)
	assert.Positive(t, resp.FirstTokenLatency)

	// Request shape: system → instructions, user → input.
	assert.Equal(t, "/responses", gotPath)
	assert.JSONEq(t, `{
		"model": "gpt-5.5",
		"instructions": "be brief",
		"input": [
			{
				"type": "message",
				"role": "user",
				"content": [
					{"type": "input_text", "text": "say ok"}
				]
			}
		],
		"stream": true,
		"store": false,
		"reasoning": {"effort": "high"}
	}`, string(gotBody))
	assert.Equal(t, "be brief", gotReq.Instructions)
	require.Len(t, gotReq.Input, 1)
	assert.Equal(t, "message", gotReq.Input[0].Type)
	assert.Equal(t, "user", gotReq.Input[0].Role)
	require.Len(t, gotReq.Input[0].Content, 1)
	assert.Equal(t, "input_text", gotReq.Input[0].Content[0].Type)
	assert.Equal(t, "say ok", gotReq.Input[0].Content[0].Text)
	assert.True(t, gotReq.Stream)

	if assert.NotNil(t, gotReq.Reasoning) {
		assert.Equal(t, "high", gotReq.Reasoning.Effort)
	}

	// Auth headers carry the chatgpt access token + account id.
	assert.Equal(t, "Bearer access-1", gotHeaders.Get("Authorization"))
	assert.Equal(t, "acct-42", gotHeaders.Get("ChatGPT-Account-ID"))
	assert.Equal(t, "text/event-stream", gotHeaders.Get("Accept"))
	assert.Equal(t, "responses=experimental", gotHeaders.Get("OpenAI-Beta"))
	assert.Equal(t, codexOriginatorHeader, gotHeaders.Get("originator"))
}

func TestBuildCodexResponsesRequest_ModelModeFastUsesPriorityServiceTier(t *testing.T) {
	t.Parallel()

	req, err := buildCodexResponsesRequest(CompleteParams{
		Model:     "gpt-5.3-codex",
		ModelMode: ModelModeFast,
		Messages:  []Message{{Role: RoleUser, Content: "hi"}},
	})
	require.NoError(t, err)

	assert.Equal(t, modelModePriority, req.ServiceTier)
}

func TestCodexProvider_CompleteOmitsPortableUnsupportedOptions(t *testing.T) {
	t.Parallel()

	var gotBody []byte

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		if !assert.NoError(t, err) {
			return
		}

		gotBody = body

		w.Header().Set("Content-Type", "text/event-stream")
		writeCodexSSE(t, w, codexFakeSuccess("ok", "gpt-5.5", 1, 0, 1))
	}))
	defer srv.Close()

	auth := newTestCodexAuth(t)
	p := &CodexProvider{
		client:  srv.Client(),
		auth:    auth,
		baseURL: srv.URL,
		models:  []string{"gpt-5.5"},
	}

	var log bytes.Buffer

	ctx := llmevents.WithEmitter(context.Background(), llmevents.NewRunnerWithLogger(nil, &log), llmevents.Event{})

	temperature := 0.2
	resp, err := p.Complete(ctx, CompleteParams{
		Model:       "gpt-5.5",
		ModelMode:   ModelModeFast,
		Temperature: &temperature,
		MaxTokens:   16,
		Messages:    []Message{{Role: RoleUser, Content: "say ok"}},
	})
	require.NoError(t, err)

	assert.Equal(t, "ok", resp.Content)
	assert.NotContains(t, string(gotBody), "temperature")
	assert.NotContains(t, string(gotBody), "max_output_tokens")
	assert.NotContains(t, string(gotBody), "max_tokens")
	assert.Contains(t, log.String(), "option_adjustments")
	assert.Contains(t, log.String(), "Temperature omitted")
	assert.Contains(t, log.String(), "model_mode=fast")
	assert.Contains(t, log.String(), "service_tier=priority")
	assert.Contains(t, log.String(), "MaxTokens omitted")
	assert.JSONEq(t, `{
		"model": "gpt-5.5",
		"service_tier": "priority",
		"instructions": "You are a helpful assistant.",
		"input": [
			{
				"type": "message",
				"role": "user",
				"content": [
					{"type": "input_text", "text": "say ok"}
				]
			}
		],
		"stream": true,
		"store": false
	}`, string(gotBody))
}

func TestCodexProvider_CompletePermissionDeniesNetworkBeforeActivity(t *testing.T) {
	t.Parallel()

	var requestCount atomic.Int32

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		requestCount.Add(1)
		http.Error(w, "unexpected request", http.StatusInternalServerError)
	}))
	defer srv.Close()

	p := &CodexProvider{
		client:  srv.Client(),
		auth:    newTestCodexAuth(t),
		baseURL: srv.URL,
		models:  []string{"gpt-5.5"},
	}

	policy := permission.DefaultPolicy()
	policy.SetMode(permission.OperationNetwork, permission.ModeDeny)

	auditDir := t.TempDir()

	var log bytes.Buffer

	ctx := llmevents.WithEmitter(context.Background(), llmevents.NewRunnerWithLogger(nil, &log), llmevents.Event{})
	ctx = permission.ContextWithPolicy(ctx, &policy)
	ctx = permission.ContextWithAuditDir(ctx, auditDir)

	_, err := p.Complete(ctx, CompleteParams{
		Model:    "gpt-5.5",
		Messages: []Message{{Role: RoleUser, Content: "say ok"}},
	})
	require.Error(t, err)
	require.True(t, permission.ErrDenied(err))
	assert.Contains(t, err.Error(), "permission.network.deny")
	assert.Equal(t, int32(0), requestCount.Load())
	assert.NotContains(t, log.String(), "event:command_execute")
	assert.NotContains(t, log.String(), "codex.responses")

	auditData, readErr := os.ReadFile(filepath.Join(auditDir, "side_effects.jsonl"))
	require.NoError(t, readErr)
	assert.Contains(t, string(auditData), "permission.network.deny")
}

func TestCodexProvider_HealthCheckPermissionDeniesCredentialBeforeActivity(t *testing.T) {
	t.Parallel()

	p := &CodexProvider{auth: newTestCodexAuth(t)}

	policy := permission.DefaultPolicy()
	policy.SetMode(permission.OperationCredentialAccess, permission.ModeDeny)

	auditDir := t.TempDir()

	var log bytes.Buffer

	ctx := llmevents.WithEmitter(context.Background(), llmevents.NewRunnerWithLogger(nil, &log), llmevents.Event{})
	ctx = permission.ContextWithPolicy(ctx, &policy)
	ctx = permission.ContextWithAuditDir(ctx, auditDir)

	err := p.HealthCheck(ctx)
	require.Error(t, err)
	require.True(t, permission.ErrDenied(err))
	assert.Contains(t, err.Error(), "permission.credential_access.deny")
	assert.NotContains(t, log.String(), "event:command_execute")
	assert.NotContains(t, log.String(), "codex.auth.check")

	auditData, readErr := os.ReadFile(filepath.Join(auditDir, "side_effects.jsonl"))
	require.NoError(t, readErr)
	assert.Contains(t, string(auditData), "permission.credential_access.deny")
}

func TestLoadCodexChatGPTAuthPermissionPolicyDeniesAuthFileRead(t *testing.T) {
	t.Parallel()

	authPath := writeCodexAuthFile(t, "access-1", "refresh-1", "acct-42")

	policy := permission.DefaultPolicy()
	policy.SetMode(permission.OperationRead, permission.ModeDeny)

	auditDir := t.TempDir()
	ctx := permission.ContextWithPolicy(t.Context(), &policy)
	ctx = permission.ContextWithAuditDir(ctx, auditDir)

	_, err := loadCodexChatGPTAuthContext(ContextWithCredentialSourcePolicy(ctx, permissiveCredentialSourcePolicy()), filepath.Dir(authPath))
	require.Error(t, err)
	require.True(t, permission.ErrDenied(err))
	assert.Contains(t, err.Error(), "permission.read.deny")

	auditData, readErr := os.ReadFile(filepath.Join(auditDir, "side_effects.jsonl"))
	require.NoError(t, readErr)
	assert.Contains(t, string(auditData), "load Codex ChatGPT credentials")
	assert.Contains(t, string(auditData), "permission.read.deny")
}

func TestCodexProvider_CompleteStream_Success(t *testing.T) {
	t.Parallel()

	var gotBody []byte

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		if !assert.NoError(t, err) {
			return
		}

		gotBody = body

		w.Header().Set("Content-Type", "text/event-stream")
		writeCodexSSE(t, w, codexFakeSuccess("hello stream", "gpt-5.5", 8, 2, 4))
	}))
	defer srv.Close()

	p := &CodexProvider{
		client:  srv.Client(),
		auth:    newTestCodexAuth(t),
		baseURL: srv.URL,
		models:  []string{"gpt-5.5"},
	}

	temperature := 0.2
	ch, err := p.CompleteStream(context.Background(), CompleteParams{
		Model:       "gpt-5.5",
		Temperature: &temperature,
		Messages:    []Message{{Role: RoleUser, Content: "hi"}},
	})
	require.NoError(t, err)

	resp, err := CollectStream(ch)
	require.NoError(t, err)
	assert.Equal(t, "hello stream", resp.Content)
	assert.Equal(t, "gpt-5.5", resp.Model)
	assert.Equal(t, StopEndTurn, resp.StopReason)
	assert.Equal(t, 8, resp.InputTokens)
	assert.Equal(t, 2, resp.CachedInputTokens)
	assert.Equal(t, 4, resp.OutputTokens)
	assert.NotContains(t, string(gotBody), "temperature")
}

func TestCodexProvider_CompleteStream_ToolUseStopReason(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		writeCodexSSE(t, w, []map[string]any{
			{
				"type": "response.output_item.done",
				"item": map[string]any{
					"type":      "function_call",
					"call_id":   "call-abc",
					"name":      "bash",
					"arguments": `{"command":"pwd"}`,
				},
			},
			{
				"type": "response.completed",
				"response": map[string]any{
					"model": "gpt-5.5",
					"usage": map[string]any{
						"input_tokens":  3,
						"output_tokens": 1,
					},
				},
			},
		})
	}))
	defer srv.Close()

	p := &CodexProvider{
		client:  srv.Client(),
		auth:    newTestCodexAuth(t),
		baseURL: srv.URL,
		models:  []string{"gpt-5.5"},
	}

	ch, err := p.CompleteStream(context.Background(), CompleteParams{
		Model:    "gpt-5.5",
		Messages: []Message{{Role: RoleUser, Content: "run pwd"}},
		Tools:    DefaultTools(),
	})
	require.NoError(t, err)

	resp, err := CollectStream(ch)
	require.NoError(t, err)
	assert.Equal(t, "gpt-5.5", resp.Model)
	assert.Equal(t, StopToolUse, resp.StopReason)
	require.Len(t, resp.ToolCalls, 1)
	assert.Equal(t, "call-abc", resp.ToolCalls[0].ID)
	assert.Equal(t, "bash", resp.ToolCalls[0].Name)
	assert.Equal(t, "pwd", resp.ToolCalls[0].Input["command"])
	assert.Equal(t, 3, resp.InputTokens)
	assert.Equal(t, 1, resp.OutputTokens)
}

func TestCodexProvider_CompleteStream_MidStreamErrorReturnsPartial(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		writeCodexSSE(t, w, []map[string]any{
			{"type": "response.output_text.delta", "delta": "partial "},
			{"type": "response.failed", "response": map[string]any{"error": "provider exploded"}},
		})
	}))
	defer srv.Close()

	p := &CodexProvider{
		client:  srv.Client(),
		auth:    newTestCodexAuth(t),
		baseURL: srv.URL,
		models:  []string{"gpt-5.5"},
	}

	ch, err := p.CompleteStream(context.Background(), CompleteParams{
		Model:    "gpt-5.5",
		Messages: []Message{{Role: RoleUser, Content: "hi"}},
	})
	require.NoError(t, err)

	resp, err := CollectStream(ch)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "codex stream error")
	assert.Equal(t, "partial ", resp.Content)
}

func TestCodexProvider_CompleteStream_CancellationReturnsPartial(t *testing.T) {
	t.Parallel()

	streamStarted := make(chan struct{})

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		writeCodexSSE(t, w, []map[string]any{
			{"type": "response.output_text.delta", "delta": "partial"},
		})
		close(streamStarted)
		<-r.Context().Done()
	}))
	defer srv.Close()

	p := &CodexProvider{
		client:  srv.Client(),
		auth:    newTestCodexAuth(t),
		baseURL: srv.URL,
		models:  []string{"gpt-5.5"},
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	ch, err := p.CompleteStream(ctx, CompleteParams{
		Model:    "gpt-5.5",
		Messages: []Message{{Role: RoleUser, Content: "hi"}},
	})
	require.NoError(t, err)

	select {
	case first := <-ch:
		assert.Equal(t, "partial", first.Content)
	case <-time.After(time.Second):
		require.Fail(t, "timed out waiting for first stream chunk")
	}

	select {
	case <-streamStarted:
	case <-time.After(time.Second):
		require.Fail(t, "timed out waiting for stream handler")
	}

	cancel()

	select {
	case terminal := <-ch:
		require.Error(t, terminal.Err)
		require.ErrorIs(t, terminal.Err, context.Canceled)
	case <-time.After(time.Second):
		require.Fail(t, "timed out waiting for terminal cancellation error")
	}
}

func TestCodexProvider_CompleteStream_MissingFinalChunkIsIncomplete(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		writeCodexSSE(t, w, []map[string]any{
			{"type": "response.output_text.delta", "delta": "partial"},
		})
	}))
	defer srv.Close()

	p := &CodexProvider{
		client:  srv.Client(),
		auth:    newTestCodexAuth(t),
		baseURL: srv.URL,
		models:  []string{"gpt-5.5"},
	}

	ch, err := p.CompleteStream(context.Background(), CompleteParams{
		Model:    "gpt-5.5",
		Messages: []Message{{Role: RoleUser, Content: "hi"}},
	})
	require.NoError(t, err)

	resp, err := CollectStream(ch)
	require.Error(t, err)
	require.ErrorIs(t, err, ErrStreamIncomplete)
	assert.Equal(t, "partial", resp.Content)
}

func TestParseCodexSSE_MissingCompletedIsIncomplete(t *testing.T) {
	t.Parallel()

	_, err := parseCodexSSE(context.Background(), strings.NewReader(codexSSEString(t, []map[string]any{
		{"type": "response.output_text.delta", "delta": "partial"},
	})))
	require.Error(t, err)
	require.ErrorIs(t, err, ErrStreamIncomplete)
}

func TestCodexStreamSSE_CancelUnblocksBackpressuredContentSend(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	ch := make(chan Chunk, DefaultStreamBuffer)
	done := make(chan struct{})
	payload := codexSSEString(t, []map[string]any{
		{"type": "response.output_text.delta", "delta": "one"},
		{"type": "response.output_text.delta", "delta": "two"},
		{"type": "response.output_text.delta", "delta": "three"},
	})

	go func() {
		defer close(done)

		streamCodexSSE(ctx, strings.NewReader(payload), ch, "gpt-5.5")
	}()

	waitForBufferedChunks(t, ch, DefaultStreamBuffer)
	cancel()

	select {
	case <-done:
	case <-time.After(time.Second):
		require.Fail(t, "stream goroutine stayed blocked after cancellation")
	}
}

func TestCodexStreamSSE_CancelUnblocksBackpressuredTerminalError(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	ch := make(chan Chunk, DefaultStreamBuffer)
	done := make(chan struct{})
	payload := codexSSEString(t, []map[string]any{
		{"type": "response.output_text.delta", "delta": "one"},
		{"type": "response.output_text.delta", "delta": "two"},
		{"type": "response.failed", "response": map[string]any{"error": "provider failed"}},
	})

	go func() {
		defer close(done)

		streamCodexSSE(ctx, strings.NewReader(payload), ch, "gpt-5.5")
	}()

	waitForBufferedChunks(t, ch, DefaultStreamBuffer)
	cancel()

	select {
	case <-done:
	case <-time.After(time.Second):
		require.Fail(t, "stream goroutine stayed blocked on terminal error after cancellation")
	}
}

func TestParseCodexSSERecordsFirstTokenLatency(t *testing.T) {
	t.Parallel()

	startedAt := time.Date(2026, time.May, 22, 12, 0, 0, 0, time.UTC)
	resp, err := parseCodexSSEWithClock(
		context.Background(),
		strings.NewReader(codexSSEText(t, codexFakeSuccess("hello", "gpt-5.4", 1, 0, 1))),
		startedAt,
		func() time.Time { return startedAt.Add(42 * time.Millisecond) },
	)

	require.NoError(t, err)
	assert.Equal(t, "gpt-5.4", resp.Model)
	assert.Equal(t, 42*time.Millisecond, resp.FirstTokenLatency)
}

func TestCodexProvider_RefreshOn401(t *testing.T) {
	t.Parallel()

	var refreshHits atomic.Int32

	refreshSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		refreshHits.Add(1)

		var req map[string]string
		if !assert.NoError(t, json.NewDecoder(r.Body).Decode(&req)) {
			return
		}

		assert.Equal(t, "refresh_token", req["grant_type"])
		assert.Equal(t, "refresh-1", req["refresh_token"])
		assert.Equal(t, codexChatGPTOAuthClientID, req["client_id"])

		w.Header().Set("Content-Type", "application/json")
		assert.NoError(t, json.NewEncoder(w).Encode(map[string]string{
			"access_token":  "access-2",
			"refresh_token": "refresh-2",
			"id_token":      "ignored",
		}))
	}))
	defer refreshSrv.Close()

	var apiCalls atomic.Int32

	apiSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		call := apiCalls.Add(1)
		token := r.Header.Get("Authorization")

		if call == 1 {
			assert.Equal(t, "Bearer access-1", token)
			w.WriteHeader(http.StatusUnauthorized)
			_, err := w.Write([]byte(`{"error":"invalid_token"}`))
			assert.NoError(t, err)

			return
		}

		assert.Equal(t, "Bearer access-2", token)

		w.Header().Set("Content-Type", "text/event-stream")
		writeCodexSSE(t, w, codexFakeSuccess("ok-after-refresh", "gpt-5.5", 1, 0, 1))
	}))
	defer apiSrv.Close()

	authPath := writeCodexAuthFile(t, "access-1", "refresh-1", "acct-42")

	auth, err := loadCodexChatGPTAuthContext(permissiveCredentialContext(context.Background()), filepath.Dir(authPath))
	require.NoError(t, err)

	auth.refreshURL = refreshSrv.URL
	auth.httpClient = refreshSrv.Client()

	p := &CodexProvider{
		client:  apiSrv.Client(),
		auth:    auth,
		baseURL: apiSrv.URL,
		models:  []string{"gpt-5.5"},
	}

	resp, err := p.Complete(context.Background(), CompleteParams{
		Model:    "gpt-5.5",
		Messages: []Message{{Role: RoleUser, Content: "hi"}},
	})
	require.NoError(t, err)

	assert.Equal(t, "ok-after-refresh", resp.Content)
	assert.EqualValues(t, 1, refreshHits.Load())
	assert.EqualValues(t, 2, apiCalls.Load())

	// Persisted access token now matches the refreshed value.
	persisted, err := os.ReadFile(authPath)
	require.NoError(t, err)

	var diskAuth codexAuth
	require.NoError(t, json.Unmarshal(persisted, &diskAuth))
	assert.Equal(t, "access-2", diskAuth.Tokens.AccessToken)
	assert.Equal(t, "refresh-2", diskAuth.Tokens.RefreshToken)
	assert.Equal(t, "acct-42", diskAuth.Tokens.AccountID)
}

func TestCodexProvider_RefreshFailureSurfaced(t *testing.T) {
	t.Parallel()

	refreshSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		_, err := w.Write([]byte(`{"error":{"code":"refresh_token_expired"}}`))
		assert.NoError(t, err)
	}))
	defer refreshSrv.Close()

	apiSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		_, err := w.Write([]byte(`{"error":"invalid_token"}`))
		assert.NoError(t, err)
	}))
	defer apiSrv.Close()

	authPath := writeCodexAuthFile(t, "access-1", "refresh-1", "acct-42")

	auth, err := loadCodexChatGPTAuthContext(permissiveCredentialContext(context.Background()), filepath.Dir(authPath))
	require.NoError(t, err)

	auth.refreshURL = refreshSrv.URL
	auth.httpClient = refreshSrv.Client()

	p := &CodexProvider{client: apiSrv.Client(), auth: auth, baseURL: apiSrv.URL, models: []string{"gpt-5.5"}}

	_, err = p.Complete(context.Background(), CompleteParams{
		Model:    "gpt-5.5",
		Messages: []Message{{Role: RoleUser, Content: "hi"}},
	})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "refresh after 401")
}

func TestCodexProvider_HTTPErrorIsTyped(t *testing.T) {
	t.Parallel()

	apiSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("x-request-id", "req-codex")
		w.WriteHeader(http.StatusBadGateway)
		_, err := w.Write([]byte(`{"error":"temporary gateway"}`))
		assert.NoError(t, err)
	}))
	defer apiSrv.Close()

	auth := newTestCodexAuth(t, "access-1", "refresh-1", "acct-42")
	p := &CodexProvider{client: apiSrv.Client(), auth: auth, baseURL: apiSrv.URL, models: []string{"gpt-5.5"}}

	_, err := p.Complete(context.Background(), CompleteParams{
		Model:    "gpt-5.5",
		Messages: []Message{{Role: RoleUser, Content: "hi"}},
	})
	require.Error(t, err)

	var providerErr *ProviderError
	require.ErrorAs(t, err, &providerErr)
	assert.Equal(t, providerCodex, providerErr.Provider)
	assert.Equal(t, http.StatusBadGateway, providerErr.StatusCode)
	assert.Equal(t, "req-codex", providerErr.RequestID)
	assert.Equal(t, RetryabilityRetryable, providerErr.Retryability)
}

func TestParseCodexSSE_ErrorEventIsTyped(t *testing.T) {
	t.Parallel()

	_, err := parseCodexSSE(context.Background(), strings.NewReader(`data: {"type":"error","code":"rate_limit_error","message":"slow down"}

`))
	require.Error(t, err)

	var providerErr *ProviderError
	require.ErrorAs(t, err, &providerErr)
	assert.Equal(t, providerCodex, providerErr.Provider)
	assert.Equal(t, http.StatusOK, providerErr.StatusCode)
	assert.Equal(t, RetryabilityRetryable, providerErr.Retryability)
	assert.Equal(t, "rate_limit_error: slow down", providerErr.Message)
}

func TestParseCodexSSE_ErrorEventCarriesResponseHeaders(t *testing.T) {
	t.Parallel()

	_, err := parseCodexSSEWithHeader(
		context.Background(),
		strings.NewReader(`data: {"type":"response.failed","response":{"error":{"code":"rate_limit_error","message":"slow down"}}}

`),
		http.Header{
			"Retry-After":  []string{"11"},
			"X-Request-Id": []string{"req-codex-stream"},
		},
	)
	require.Error(t, err)

	var providerErr *ProviderError
	require.ErrorAs(t, err, &providerErr)
	assert.Equal(t, providerCodex, providerErr.Provider)
	assert.Equal(t, http.StatusOK, providerErr.StatusCode)
	assert.Equal(t, 11*time.Second, providerErr.RetryAfter)
	assert.Equal(t, "req-codex-stream", providerErr.RequestID)
	assert.Equal(t, RetryabilityRetryable, providerErr.Retryability)
	assert.Equal(t, "rate_limit_error: slow down", providerErr.Message)
}

func TestParseCodexSSE_ResponseFailedPrefersSpecificErrorCode(t *testing.T) {
	t.Parallel()

	_, err := parseCodexSSE(context.Background(), strings.NewReader(`data: {"type":"response.failed","response":{"error":{"type":"error","code":"server_error","message":"try later"}}}

`))
	require.Error(t, err)

	var providerErr *ProviderError
	require.ErrorAs(t, err, &providerErr)
	assert.Equal(t, providerCodex, providerErr.Provider)
	assert.Equal(t, http.StatusOK, providerErr.StatusCode)
	assert.Equal(t, RetryabilityRetryable, providerErr.Retryability)
	assert.Equal(t, "server_error: try later", providerErr.Message)
}

func TestCodexProvider_RefreshHonorsCanceledContext(t *testing.T) {
	t.Parallel()

	requestStarted := make(chan struct{})
	release := make(chan struct{})

	refreshSrv := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		close(requestStarted)

		select {
		case <-r.Context().Done():
			return
		case <-release:
			return
		}
	}))
	defer refreshSrv.Close()
	defer close(release)

	apiSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		_, err := w.Write([]byte(`{"error":"invalid_token"}`))
		assert.NoError(t, err)
	}))
	defer apiSrv.Close()

	authPath := writeCodexAuthFile(t, "access-1", "refresh-1", "acct-42")

	auth, err := loadCodexChatGPTAuthContext(permissiveCredentialContext(context.Background()), filepath.Dir(authPath))
	require.NoError(t, err)

	auth.refreshURL = refreshSrv.URL
	auth.httpClient = refreshSrv.Client()

	p := &CodexProvider{client: apiSrv.Client(), auth: auth, baseURL: apiSrv.URL, models: []string{"gpt-5.5"}}

	ctx, cancel := context.WithCancel(context.Background())
	errCh := make(chan error, 1)

	go func() {
		_, completeErr := p.Complete(ctx, CompleteParams{
			Model:    "gpt-5.5",
			Messages: []Message{{Role: RoleUser, Content: "hi"}},
		})
		errCh <- completeErr
	}()

	<-requestStarted
	cancel()

	err = <-errCh
	require.Error(t, err)
	require.ErrorIs(t, err, context.Canceled)

	access, accountID := auth.snapshot()
	assert.Equal(t, "access-1", access)
	assert.Equal(t, "acct-42", accountID)

	persisted, err := os.ReadFile(authPath)
	require.NoError(t, err)

	var diskAuth codexAuth
	require.NoError(t, json.Unmarshal(persisted, &diskAuth))
	assert.Equal(t, "access-1", diskAuth.Tokens.AccessToken)
	assert.Equal(t, "refresh-1", diskAuth.Tokens.RefreshToken)
	assert.Equal(t, "acct-42", diskAuth.Tokens.AccountID)
}

func TestCodexProvider_MissingRefreshTokenFailsLoudlyOn401(t *testing.T) {
	t.Parallel()

	var apiCalls atomic.Int32

	apiSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		apiCalls.Add(1)
		w.WriteHeader(http.StatusUnauthorized)
		_, err := w.Write([]byte(`{"error":"invalid_token"}`))
		assert.NoError(t, err)
	}))
	defer apiSrv.Close()

	authPath := writeCodexAuthFile(t, "access-1", "", "acct-42")

	auth, err := loadCodexChatGPTAuthContext(permissiveCredentialContext(context.Background()), filepath.Dir(authPath))
	require.NoError(t, err)

	p := &CodexProvider{client: apiSrv.Client(), auth: auth, baseURL: apiSrv.URL, models: []string{"gpt-5.5"}}

	_, err = p.Complete(context.Background(), CompleteParams{
		Model:    "gpt-5.5",
		Messages: []Message{{Role: RoleUser, Content: "hi"}},
	})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "refresh after 401")
	assert.Contains(t, err.Error(), "no refresh_token")
	assert.EqualValues(t, 1, apiCalls.Load())
}

func TestLoadCodexChatGPTAuth(t *testing.T) {
	t.Parallel()

	authPath := writeCodexAuthFile(t, "access-1", "refresh-1", "acct-42")

	auth, err := loadCodexChatGPTAuthContext(permissiveCredentialContext(context.Background()), filepath.Dir(authPath))
	require.NoError(t, err)

	access, accountID := auth.snapshot()
	assert.Equal(t, "access-1", access)
	assert.Equal(t, "acct-42", accountID)
}

func TestLoadCodexChatGPTAuth_AllowsMissingRefreshForDiagnostics(t *testing.T) {
	t.Parallel()

	authPath := writeCodexAuthFile(t, "access-1", "", "acct-42")

	auth, err := loadCodexChatGPTAuthContext(permissiveCredentialContext(context.Background()), filepath.Dir(authPath))
	require.NoError(t, err)

	access, accountID := auth.snapshot()
	assert.Equal(t, "access-1", access)
	assert.Equal(t, "acct-42", accountID)
	assert.False(t, auth.hasRefreshToken())
}

func TestLoadCodexChatGPTAuth_RejectsAPIKeyMode(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	authJSON := `{"auth_mode":"apikey","OPENAI_API_KEY":"sk-test","tokens":{"access_token":"","refresh_token":""}}`
	require.NoError(t, os.WriteFile(filepath.Join(dir, "auth.json"), []byte(authJSON), 0o600))

	_, err := loadCodexChatGPTAuthContext(permissiveCredentialContext(context.Background()), dir)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "auth_mode")
}

func TestLoadCodexChatGPTAuth_RejectsMissingAccessToken(t *testing.T) {
	t.Parallel()

	authPath := writeCodexAuthFile(t, "", "refresh-1", "acct-42")

	_, err := loadCodexChatGPTAuthContext(permissiveCredentialContext(context.Background()), filepath.Dir(authPath))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "missing access_token")
}

func TestLoadCodexChatGPTAuth_MissingAccessTokenRedactsPathSecrets(t *testing.T) {
	t.Parallel()

	dir := filepath.Join(t.TempDir(), "api_key=path-secret")
	require.NoError(t, os.MkdirAll(dir, 0o750))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "auth.json"), []byte(`{
		"auth_mode":"chatgpt",
		"tokens":{"access_token":"","refresh_token":"refresh-secret"}
	}`), 0o600))

	_, err := loadCodexChatGPTAuthContext(permissiveCredentialContext(context.Background()), dir)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "api_key=[REDACTED]")
	assert.NotContains(t, err.Error(), "path-secret")
	assert.NotContains(t, err.Error(), "refresh-secret")
}

func TestPersistRefreshedCodexAuth_AtomicAndPreservesUnknown(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	authPath := filepath.Join(dir, "auth.json")
	original := `{
		"auth_mode": "chatgpt",
		"OPENAI_API_KEY": null,
		"custom_field": "preserved",
		"tokens": {
			"id_token": "old-id",
			"access_token": "old-access",
			"refresh_token": "old-refresh",
			"account_id": "acct-9"
		},
		"last_refresh": "2025-01-01T00:00:00Z"
	}`
	require.NoError(t, os.WriteFile(authPath, []byte(original), 0o600))

	require.NoError(t, persistRefreshedCodexAuth(context.Background(), authPath, "new-access", "new-refresh", "new-id"))

	updated, err := os.ReadFile(authPath)
	require.NoError(t, err)

	var raw map[string]any
	require.NoError(t, json.Unmarshal(updated, &raw))

	assert.Equal(t, "preserved", raw["custom_field"])
	assert.NotEqual(t, "2025-01-01T00:00:00Z", raw["last_refresh"], "last_refresh should be updated")

	tokens, ok := raw["tokens"].(map[string]any)
	require.True(t, ok, "tokens field must be an object")
	assert.Equal(t, "new-access", tokens["access_token"])
	assert.Equal(t, "new-refresh", tokens["refresh_token"])
	assert.Equal(t, "new-id", tokens["id_token"])
	assert.Equal(t, "acct-9", tokens["account_id"], "account_id should be preserved")
}

func TestPersistRefreshedCodexAuth_RequiresActiveContext(t *testing.T) {
	t.Parallel()

	authPath := writeCodexAuthFile(t, "old-access", "old-refresh", "acct-9")

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	err := persistRefreshedCodexAuth(ctx, authPath, "new-access", "new-refresh", "new-id")
	require.Error(t, err)
	require.ErrorIs(t, err, context.Canceled)

	auth, err := loadCodexChatGPTAuthContext(permissiveCredentialContext(context.Background()), filepath.Dir(authPath))
	require.NoError(t, err)

	access, accountID := auth.snapshot()
	assert.Equal(t, "old-access", access)
	assert.Equal(t, "acct-9", accountID)
}

func TestPersistRefreshedCodexAuth_PermissionPolicyDeniesWrite(t *testing.T) {
	t.Parallel()

	authPath := writeCodexAuthFile(t, "old-access", "old-refresh", "acct-9")

	policy := permission.DefaultPolicy()
	policy.SetMode(permission.OperationWrite, permission.ModeDeny)

	auditDir := t.TempDir()
	ctx := permission.ContextWithPolicy(t.Context(), &policy)
	ctx = permission.ContextWithAuditDir(ctx, auditDir)

	err := persistRefreshedCodexAuth(ctx, authPath, "new-access", "new-refresh", "new-id")
	require.Error(t, err)
	require.True(t, permission.ErrDenied(err))
	assert.Contains(t, err.Error(), "permission.write.deny")

	auth, loadErr := loadCodexChatGPTAuthContext(permissiveCredentialContext(context.Background()), filepath.Dir(authPath))
	require.NoError(t, loadErr)

	access, accountID := auth.snapshot()
	assert.Equal(t, "old-access", access)
	assert.Equal(t, "acct-9", accountID)

	auditData, readErr := os.ReadFile(filepath.Join(auditDir, "side_effects.jsonl"))
	require.NoError(t, readErr)
	assert.Contains(t, string(auditData), "permission.write.deny")

	credentialAuditData, credentialAuditErr := os.ReadFile(filepath.Join(auditDir, credentialAuditLedgerFileName))
	require.NoError(t, credentialAuditErr)

	credentialAudit := string(credentialAuditData)
	assert.Contains(t, credentialAudit, credentialAuditEventWriteBack)
	assert.Contains(t, credentialAudit, `"decision":"failed"`)
	assert.Contains(t, credentialAudit, "permission.write.deny")
	assert.NotContains(t, credentialAudit, "new-access")
	assert.NotContains(t, credentialAudit, "new-refresh")
	assert.NotContains(t, credentialAudit, "new-id")
}

func TestPersistRefreshedCodexAuth_AuditsWriteBackWithoutSecrets(t *testing.T) {
	t.Parallel()

	authPath := writeCodexAuthFile(t, "old-access", "old-refresh", "account-secret-123456")
	auditDir := t.TempDir()
	ctx := permission.ContextWithAuditDir(t.Context(), auditDir)

	source := credentialSource{
		Provider:      providerCodex,
		Store:         CredentialStoreCodexAuthJSON,
		Description:   "Codex ChatGPT auth.json",
		Location:      authPath,
		Identifier:    "account-secret-123456",
		BorrowedOAuth: true,
	}

	_, err := persistRefreshedCodexAuthWithCAS(
		ctx,
		authPath,
		"old-access",
		"old-refresh",
		"new-access-secret",
		"new-refresh-secret",
		"new-id-secret",
		source,
		ProviderConfig{CredentialPolicy: permissiveCredentialSourcePolicy()},
	)
	require.NoError(t, err)

	auditData, readErr := os.ReadFile(filepath.Join(auditDir, credentialAuditLedgerFileName))
	require.NoError(t, readErr)

	audit := string(auditData)
	assert.Contains(t, audit, credentialAuditEventWriteBack)
	assert.Contains(t, audit, providerCodex)
	assert.Contains(t, audit, CredentialStoreCodexAuthJSON)
	assert.NotContains(t, audit, "old-access")
	assert.NotContains(t, audit, "old-refresh")
	assert.NotContains(t, audit, "new-access-secret")
	assert.NotContains(t, audit, "new-refresh-secret")
	assert.NotContains(t, audit, "new-id-secret")
	assert.NotContains(t, audit, "account-secret-123456")
	assert.Contains(t, audit, "sha256:")
}

func TestPersistRefreshedCodexAuth_WriteBackFailureIsAudited(t *testing.T) {
	t.Parallel()

	authDir := filepath.Join(t.TempDir(), "api_key=path-secret")
	require.NoError(t, os.MkdirAll(authDir, 0o700))

	authPath := filepath.Join(authDir, "auth.json")
	require.NoError(t, os.WriteFile(authPath, []byte(`{"tokens":`), 0o600))

	auditDir := t.TempDir()
	ctx := permission.ContextWithAuditDir(context.Background(), auditDir)
	source := credentialSource{
		Provider:      providerCodex,
		Store:         CredentialStoreCodexAuthJSON,
		Description:   "Codex ChatGPT auth.json",
		Location:      authPath,
		Identifier:    "acct-secret",
		BorrowedOAuth: true,
	}

	_, err := persistRefreshedCodexAuthWithCAS(
		ctx,
		authPath,
		"old-access-secret",
		"old-refresh-secret",
		"new-access-secret",
		"new-refresh-secret",
		"new-id-secret",
		source,
		ProviderConfig{CredentialPolicy: permissiveCredentialSourcePolicy()},
	)
	require.Error(t, err)

	auditData, readErr := os.ReadFile(filepath.Join(auditDir, credentialAuditLedgerFileName))
	require.NoError(t, readErr)

	audit := string(auditData)
	assert.Contains(t, audit, credentialAuditEventWriteBack)
	assert.Contains(t, audit, `"decision":"failed"`)
	assert.Contains(t, audit, "api_key=[REDACTED]")
	assert.Contains(t, audit, "sha256:")
	assert.NotContains(t, audit, "path-secret")
	assert.NotContains(t, audit, "acct-secret")
	assert.NotContains(t, audit, "old-access-secret")
	assert.NotContains(t, audit, "old-refresh-secret")
	assert.NotContains(t, audit, "new-access-secret")
	assert.NotContains(t, audit, "new-refresh-secret")
	assert.NotContains(t, audit, "new-id-secret")
}

func TestPersistRefreshedCodexAuth_CASPreventsStaleWriteBack(t *testing.T) {
	t.Parallel()

	authPath := writeCodexAuthFile(t, "old-access", "old-refresh", "acct-9")
	source := credentialSource{
		Provider:      providerCodex,
		Store:         CredentialStoreCodexAuthJSON,
		Description:   "Codex ChatGPT auth.json",
		Location:      authPath,
		BorrowedOAuth: true,
	}
	cfg := ProviderConfig{CredentialPolicy: permissiveCredentialSourcePolicy()}

	first, err := persistRefreshedCodexAuthWithCAS(
		t.Context(),
		authPath,
		"old-access",
		"old-refresh",
		"winner-access",
		"winner-refresh",
		"winner-id",
		source,
		cfg,
	)
	require.NoError(t, err)
	assert.Equal(t, "winner-access", first.accessToken)

	second, err := persistRefreshedCodexAuthWithCAS(
		t.Context(),
		authPath,
		"old-access",
		"old-refresh",
		"stale-access",
		"stale-refresh",
		"stale-id",
		source,
		cfg,
	)
	require.Error(t, err)
	assert.True(t, isCredentialFileCASMismatch(err))
	assert.Equal(t, "winner-access", second.accessToken)

	updated, readErr := os.ReadFile(authPath)
	require.NoError(t, readErr)
	assert.Contains(t, string(updated), "winner-access")
	assert.NotContains(t, string(updated), "stale-access")
}

func TestCodexChatGPTAuthRefresh_ConcurrentRefreshUsesCASWinner(t *testing.T) {
	t.Parallel()

	authPath := writeCodexAuthFile(t, "access-1", "refresh-1", "acct-42")

	var refreshHits atomic.Int32

	refreshSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hit := refreshHits.Add(1) + 1

		var req map[string]string
		if !assert.NoError(t, json.NewDecoder(r.Body).Decode(&req)) {
			return
		}

		assert.Equal(t, "refresh-1", req["refresh_token"])

		w.Header().Set("Content-Type", "application/json")
		assert.NoError(t, json.NewEncoder(w).Encode(map[string]string{
			"access_token":  fmt.Sprintf("access-%d", hit),
			"refresh_token": fmt.Sprintf("refresh-%d", hit),
			"id_token":      fmt.Sprintf("id-%d", hit),
		}))
	}))
	defer refreshSrv.Close()

	auth1, err := loadCodexChatGPTAuthContext(permissiveCredentialContext(context.Background()), filepath.Dir(authPath))
	require.NoError(t, err)
	auth2, err := loadCodexChatGPTAuthContext(permissiveCredentialContext(context.Background()), filepath.Dir(authPath))
	require.NoError(t, err)

	for _, auth := range []*codexChatGPTAuth{auth1, auth2} {
		auth.refreshURL = refreshSrv.URL
		auth.httpClient = refreshSrv.Client()
	}

	auditDir := t.TempDir()
	ctx := permission.ContextWithAuditDir(context.Background(), auditDir)
	errCh := make(chan error, 2)

	go func() { errCh <- auth1.refresh(ctx, "access-1") }()
	go func() { errCh <- auth2.refresh(ctx, "access-1") }()

	for range 2 {
		require.NoError(t, <-errCh)
	}

	persisted, err := os.ReadFile(authPath)
	require.NoError(t, err)

	var diskAuth codexAuth
	require.NoError(t, json.Unmarshal(persisted, &diskAuth))
	require.Contains(t, []string{"access-2", "access-3"}, diskAuth.Tokens.AccessToken)
	assert.Contains(t, []string{"refresh-2", "refresh-3"}, diskAuth.Tokens.RefreshToken)

	auth1Access, _ := auth1.snapshot()
	auth2Access, _ := auth2.snapshot()

	assert.Equal(t, diskAuth.Tokens.AccessToken, auth1Access)
	assert.Equal(t, diskAuth.Tokens.AccessToken, auth2Access)

	auditData, readErr := os.ReadFile(filepath.Join(auditDir, credentialAuditLedgerFileName))
	require.NoError(t, readErr)
	assert.Contains(t, string(auditData), credentialAuditEventCAS)
	assert.NotContains(t, string(auditData), "access-2")
	assert.NotContains(t, string(auditData), "access-3")
}

func TestCodexConfiguredModelContext(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("HOME", dir)
	t.Setenv("CODEX_HOME", "")

	codexDir := filepath.Join(dir, ".codex")
	require.NoError(t, os.MkdirAll(codexDir, 0o750))

	require.NoError(t, os.WriteFile(filepath.Join(codexDir, "config.toml"), []byte(`
# comment
model = "gpt-test-codex"
`), 0o600))

	auditDir := t.TempDir()
	ctx := permission.ContextWithAuditDir(t.Context(), auditDir)

	if got, err := codexConfiguredModelContext(ctx); err != nil || got != "gpt-test-codex" {
		require.Failf(t, "unexpected failure", "codexConfiguredModelContext = %q, %v; want gpt-test-codex", got, err)
	}

	models, err := codexModelsContext(ctx)
	require.NoError(t, err)

	if len(models) == 0 || models[0] != "gpt-test-codex" {
		require.Failf(t, "unexpected failure", "codexModelsContext = %v, want configured model first", models)
	}

	auditData, readErr := os.ReadFile(filepath.Join(auditDir, "side_effects.jsonl"))
	require.NoError(t, readErr)
	assert.Contains(t, string(auditData), "read Codex config")
	assert.Contains(t, string(auditData), "permission.allow")
}

func TestNewCodexProviderPermissionPolicyDeniesAuthFileRead(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("HOME", dir)
	t.Setenv("CODEX_HOME", "")

	codexDir := filepath.Join(dir, ".codex")
	require.NoError(t, os.MkdirAll(codexDir, 0o750))
	require.NoError(t, os.WriteFile(filepath.Join(codexDir, "auth.json"), []byte(
		`{"auth_mode":"chatgpt","OPENAI_API_KEY":null,"tokens":{"access_token":"access-1","refresh_token":"refresh-1","account_id":"acct-42"}}`,
	), 0o600))

	policy := permission.DefaultPolicy()
	policy.SetMode(permission.OperationRead, permission.ModeDeny)

	ctx := permission.ContextWithPolicy(t.Context(), &policy)
	ctx = ContextWithCredentialSourcePolicy(ctx, permissiveCredentialSourcePolicy())
	_, err := NewCodexProviderWithConfigContext(ctx, ProviderConfig{})

	require.Error(t, err)
	require.True(t, permission.ErrDenied(err))
	assert.Contains(t, err.Error(), "permission.read.deny")
}

func TestKnownProvidersContextPermissionPolicyDeniesCodexConfigRead(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("CODEX_HOME", dir)

	policy := permission.DefaultPolicy()
	policy.SetMode(permission.OperationRead, permission.ModeDeny)

	ctx := permission.ContextWithPolicy(t.Context(), &policy)
	_, err := KnownProvidersContext(ctx)

	require.Error(t, err)
	require.True(t, permission.ErrDenied(err))
	assert.Contains(t, err.Error(), "permission.read.deny")
}

func TestCodexProvider_ModelMetadataAndContextFallback(t *testing.T) {
	t.Parallel()

	p := &CodexProvider{models: []string{"custom-codex-model", "gpt-5.5"}}

	metadata, ok := p.ModelMetadata("gpt-5.5")
	require.True(t, ok)
	assert.Equal(t, 1_050_000, metadata.ContextWindow)
	assert.Contains(t, metadata.Provenance, "built-in provider/model catalog")
	assert.NotEmpty(t, metadata.ReviewedAt)
	assert.NotEmpty(t, metadata.ReviewAfter)

	metadata, ok = p.ModelMetadata("custom-codex-model")
	require.True(t, ok)
	assert.Zero(t, metadata.ContextWindow)
	assert.NotEmpty(t, metadata.ReviewedAt)
	assert.Contains(t, metadata.Notes, "rather than guessing")

	assert.Zero(t, p.ModelContextWindow("gpt-unknown-private"))
}

func TestCodexProvider_StaticModelCatalogConformance(t *testing.T) {
	t.Parallel()

	p := &CodexProvider{models: []string{"custom-codex-model", "gpt-5.5"}}

	models, err := p.FetchModels(context.Background())
	require.NoError(t, err)
	assert.Equal(t, []string{"custom-codex-model", "gpt-5.5"}, models)

	catalog := p.ModelCatalog()
	require.Len(t, catalog, 2)
	assert.Equal(t, "custom-codex-model", catalog[0].ID)
	assert.Zero(t, catalog[0].ContextWindow)
	assert.Contains(t, catalog[0].Provenance, "user Codex config.toml model override")
	assert.Equal(t, codexAdapterReviewAfter, catalog[0].ReviewAfter)

	assert.Equal(t, "gpt-5.5", catalog[1].ID)
	assert.Equal(t, 1_050_000, catalog[1].ContextWindow)
	assert.Contains(t, catalog[1].Provenance, "built-in provider/model catalog")
	assert.Equal(t, codexAdapterReviewAfter, catalog[1].ReviewAfter)
}

func TestCodexProvider_AdapterDiagnostics(t *testing.T) {
	t.Parallel()

	p := &CodexProvider{
		auth:   newTestCodexAuth(t, "access-1", "refresh-1", "acct-42"),
		models: []string{"gpt-5.5"},
	}

	diagnostics := p.AdapterDiagnostics()
	assert.True(t, diagnostics.Healthy())
	assert.Equal(t, codexAdapterVersion, diagnostics.Contract.AdapterVersion)
	assert.NotEmpty(t, diagnostics.Contract.SourceCLIVersion)
	assert.Contains(t, diagnostics.Contract.KillSwitches, "providers.codex.disable_private_adapter")
	assert.Contains(t, diagnostics.Contract.KillSwitches, "ATTELER_DISABLE_CODEX_ADAPTER")
	assert.Equal(t, codexAdapterReviewAfter, diagnostics.Contract.ReviewAfter)

	checks := readinessChecksByName(diagnostics.Checks)
	assert.Equal(t, ReadinessOK, checks["local_credentials"].Status)
	assert.Equal(t, ReadinessOK, checks["token_refresh"].Status)
	assert.Equal(t, ReadinessOK, checks["credential_provenance"].Status)
	assert.Contains(t, checks["credential_provenance"].Detail, CredentialStoreCodexAuthJSON)
	assert.NotContains(t, checks["credential_provenance"].Detail, "access-1")
	assert.Contains(t, checks["credential_policy"].Detail, "allow_borrowed_oauth=true")
	assert.Equal(t, ReadinessSkipped, checks["network_reachability"].Status)
	assert.Equal(t, ReadinessWarning, checks["model_availability"].Status)
	assert.Contains(t, diagnostics.Warnings[0], "static")
}

func TestCodexProvider_AdapterDiagnosticsSeparatesAccessAndRefreshReadiness(t *testing.T) {
	t.Parallel()

	p := &CodexProvider{
		auth:   newTestCodexAuth(t, "access-without-refresh", "", ""),
		models: []string{"gpt-5.5"},
	}

	diagnostics := p.AdapterDiagnostics()
	assert.False(t, diagnostics.Healthy())

	checks := readinessChecksByName(diagnostics.Checks)
	assert.Equal(t, ReadinessOK, checks["local_credentials"].Status)
	assert.Equal(t, ReadinessFailed, checks["token_refresh"].Status)
	assert.Equal(t, ReadinessWarning, checks["account_scope"].Status)

	err := diagnostics.Error()
	require.Error(t, err)
	assert.Contains(t, err.Error(), "token_refresh")
}

func TestNewCodexProviderWithConfig_HonorsPrivateAdapterKillSwitchBeforeCredentials(t *testing.T) {
	t.Setenv("CODEX_HOME", filepath.Join(t.TempDir(), "missing-codex-home"))

	_, err := NewCodexProviderWithConfigContext(context.Background(), ProviderConfig{DisablePrivateAdapter: true})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "private adapter disabled")
}

func TestNewCodexProviderWithConfig_HonorsPrivateAdapterEnvKillSwitchBeforeCredentials(t *testing.T) {
	t.Setenv("CODEX_HOME", filepath.Join(t.TempDir(), "missing-codex-home"))
	t.Setenv("ATTELER_DISABLE_CODEX_ADAPTER", "1")

	_, err := NewCodexProviderWithConfigContext(context.Background(), ProviderConfig{})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "private adapter disabled")
}

func TestCodexBuildInput_SkipsSystemAndPicksContentType(t *testing.T) {
	t.Parallel()

	got := codexBuildInput([]Message{
		{Role: RoleSystem, Content: "system rules"},
		{Role: RoleUser, Content: "hello"},
		{Role: RoleAssistant, Content: "hi"},
	})

	require.Len(t, got, 2)
	assert.Equal(t, "user", got[0].Role)
	assert.Equal(t, "input_text", got[0].Content[0].Type)
	assert.Equal(t, "assistant", got[1].Role)
	assert.Equal(t, "output_text", got[1].Content[0].Type)
}

func TestCodexBuildInput_ToolCallAndResultMessages(t *testing.T) {
	t.Parallel()

	got := codexBuildInput([]Message{
		{Role: RoleUser, Content: "run ls"},
		{
			Role:    RoleAssistant,
			Content: "",
			ToolCalls: []ToolCall{{
				ID:    "call-1",
				Name:  "bash",
				Input: map[string]any{"command": "ls"},
			}},
		},
		{
			Role: RoleTool,
			ToolResult: &ToolResult{
				ToolCallID: "call-1",
				Content:    "file1.go\nfile2.go",
			},
		},
		{Role: RoleAssistant, Content: "Here are the files."},
	})

	require.Len(t, got, 4)

	// 1) user message
	assert.Equal(t, "message", got[0].Type)
	assert.Equal(t, "user", got[0].Role)

	// 2) function_call
	assert.Equal(t, "function_call", got[1].Type)
	assert.Equal(t, "call-1", got[1].CallID)
	assert.Equal(t, "bash", got[1].Name)
	assert.Contains(t, got[1].Arguments, `"command"`)

	// 3) function_call_output
	assert.Equal(t, "function_call_output", got[2].Type)
	assert.Equal(t, "call-1", got[2].CallID)
	assert.Equal(t, "file1.go\nfile2.go", got[2].Output)

	// 4) final assistant message
	assert.Equal(t, "message", got[3].Type)
	assert.Equal(t, "assistant", got[3].Role)
}

func TestCodexBuildTools(t *testing.T) {
	t.Parallel()

	tools := codexBuildTools([]ToolDefinition{BashTool()})
	require.Len(t, tools, 1)
	assert.Equal(t, "function", tools[0].Type)
	assert.Equal(t, "bash", tools[0].Name)
	assert.NotEmpty(t, tools[0].Description)
	assert.NotNil(t, tools[0].Parameters)
}

func TestCodexBuildTools_NilReturnsNil(t *testing.T) {
	t.Parallel()

	assert.Nil(t, codexBuildTools(nil))
}

func TestCodexProvider_Complete_WithToolUse(t *testing.T) {
	t.Parallel()

	var gotReq codexResponsesRequest

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		if !assert.NoError(t, err) {
			return
		}

		if !assert.NoError(t, json.Unmarshal(body, &gotReq)) {
			return
		}

		w.Header().Set("Content-Type", "text/event-stream")
		// Return a function_call response.
		events := []map[string]any{
			{
				"type": "response.output_item.done",
				"item": map[string]any{
					"type":      "function_call",
					"call_id":   "call-abc",
					"name":      "bash",
					"arguments": `{"command":"ls -la"}`,
				},
			},
			{
				"type": "response.completed",
				"response": map[string]any{
					"model": "gpt-5.5",
					"usage": map[string]any{
						"input_tokens":  10,
						"output_tokens": 5,
					},
				},
			},
		}
		writeCodexSSE(t, w, events)
	}))
	defer srv.Close()

	auth := newTestCodexAuth(t)
	p := &CodexProvider{
		client:  srv.Client(),
		auth:    auth,
		baseURL: srv.URL,
		models:  []string{"gpt-5.5"},
	}

	resp, err := p.Complete(context.Background(), CompleteParams{
		Model:    "gpt-5.5",
		Messages: []Message{{Role: RoleUser, Content: "list files"}},
		Tools:    DefaultTools(),
	})
	require.NoError(t, err)

	// Tools should be sent in the request.
	require.Len(t, gotReq.Tools, len(DefaultTools()))
	toolNames := make([]string, 0, len(gotReq.Tools))

	for _, tool := range gotReq.Tools {
		assert.Equal(t, "function", tool.Type)
		toolNames = append(toolNames, tool.Name)
	}

	assert.Contains(t, toolNames, "bash")
	assert.Contains(t, toolNames, "read")
	assert.Contains(t, toolNames, "write")
	assert.Contains(t, toolNames, "edit")
	assert.Contains(t, toolNames, "glob")
	assert.Contains(t, toolNames, "grep")

	// Response should contain tool calls.
	assert.Equal(t, StopToolUse, resp.StopReason)
	require.Len(t, resp.ToolCalls, 1)
	assert.Equal(t, "call-abc", resp.ToolCalls[0].ID)
	assert.Equal(t, "bash", resp.ToolCalls[0].Name)
	assert.Equal(t, "ls -la", resp.ToolCalls[0].Input["command"])
}

func TestParseCodexSSE_FailedEventIsConformanceFailure(t *testing.T) {
	t.Parallel()

	_, err := parseCodexSSE(context.Background(), strings.NewReader(`data: {"type":"response.failed","error":{"message":"wire changed"}}

`))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "response.failed")
}

func TestCodexExtractFunctionCall(t *testing.T) {
	t.Parallel()

	tc, ok := codexExtractFunctionCall(&codexEventItem{
		Type:      "function_call",
		CallID:    "call-99",
		Name:      "bash",
		Arguments: `{"command":"pwd"}`,
	})
	require.True(t, ok)
	assert.Equal(t, "call-99", tc.ID)
	assert.Equal(t, "bash", tc.Name)
	assert.Equal(t, "pwd", tc.Input["command"])
}

func TestCodexExtractFunctionCall_NilItem(t *testing.T) {
	t.Parallel()

	_, ok := codexExtractFunctionCall(nil)
	assert.False(t, ok)
}

func TestCodexExtractFunctionCall_NonFunctionCallType(t *testing.T) {
	t.Parallel()

	_, ok := codexExtractFunctionCall(&codexEventItem{Type: "message"})
	assert.False(t, ok)
}

// ---------------------------------------------------------------------------
// Helpers
// ---------------------------------------------------------------------------

func permissiveCredentialContext(ctx context.Context) context.Context {
	return ContextWithCredentialSourcePolicy(ctx, permissiveCredentialSourcePolicy())
}

func newTestCodexAuth(t *testing.T, values ...string) *codexChatGPTAuth {
	t.Helper()

	access, refresh, accountID := "access-1", "refresh-1", "acct-42"
	if len(values) > 0 {
		access = values[0]
	}

	if len(values) > 1 {
		refresh = values[1]
	}

	if len(values) > 2 {
		accountID = values[2]
	}

	authPath := writeCodexAuthFile(t, access, refresh, accountID)

	auth, err := loadCodexChatGPTAuthContext(permissiveCredentialContext(context.Background()), filepath.Dir(authPath))
	require.NoError(t, err)

	return auth
}

func writeCodexAuthFile(t *testing.T, access, refresh, accountID string) string {
	t.Helper()

	dir := t.TempDir()
	authPath := filepath.Join(dir, "auth.json")

	body := fmt.Sprintf(
		`{"auth_mode":"chatgpt","OPENAI_API_KEY":null,"tokens":{"access_token":%q,"refresh_token":%q,"account_id":%q}}`,
		access, refresh, accountID,
	)
	require.NoError(t, os.WriteFile(authPath, []byte(body), 0o600))

	return authPath
}

// codexFakeSuccess returns SSE event JSON values for a successful completion.
func codexFakeSuccess(text, model string, in, cached, out int) []map[string]any {
	return []map[string]any{
		{"type": "response.output_text.delta", "delta": text[:1]},
		{"type": "response.output_text.delta", "delta": text[1:]},
		{
			"type": "response.output_item.done",
			"item": map[string]any{
				"type": "message",
				"role": "assistant",
				"content": []map[string]any{
					{"type": "output_text", "text": text},
				},
			},
		},
		{
			"type": "response.completed",
			"response": map[string]any{
				"model": model,
				"usage": map[string]any{
					"input_tokens":         in,
					"output_tokens":        out,
					"input_tokens_details": map[string]any{"cached_tokens": cached},
				},
			},
		},
	}
}

func writeCodexSSE(t *testing.T, w http.ResponseWriter, events []map[string]any) {
	t.Helper()

	flusher, hasFlusher := w.(http.Flusher)

	payload := codexSSEString(t, events)

	_, err := w.Write([]byte(payload))
	require.NoError(t, err)

	if hasFlusher {
		flusher.Flush()
	}
}

func codexSSEString(t *testing.T, events []map[string]any) string {
	t.Helper()

	var b strings.Builder

	for _, ev := range events {
		payload, err := json.Marshal(ev)
		require.NoError(t, err)

		b.WriteString("event: ")
		b.WriteString(asString(ev["type"]))
		b.WriteString("\n")
		b.WriteString("data: ")
		b.Write(payload)
		b.WriteString("\n\n")
	}

	return b.String()
}

func waitForBufferedChunks(t *testing.T, ch <-chan Chunk, want int) {
	t.Helper()

	deadline := time.After(time.Second)
	ticker := time.NewTicker(10 * time.Millisecond)

	defer ticker.Stop()

	for {
		if len(ch) == want {
			return
		}

		select {
		case <-deadline:
			require.Failf(t, "timed out waiting for buffered chunks", "len(ch) = %d, want %d", len(ch), want)
		case <-ticker.C:
		}
	}
}

func codexSSEText(t *testing.T, events []map[string]any) string {
	t.Helper()

	var b strings.Builder

	for _, ev := range events {
		payload, err := json.Marshal(ev)
		require.NoError(t, err)

		b.WriteString("data: ")
		b.Write(payload)
		b.WriteString("\n\n")
	}

	return b.String()
}

func asString(v any) string {
	if s, ok := v.(string); ok {
		return s
	}

	return ""
}

func init() {
	// Ensure import compatibility for strings.* used elsewhere in tests.
	_ = strings.EqualFold
}
