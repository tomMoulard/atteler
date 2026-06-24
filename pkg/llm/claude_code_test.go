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
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	llmevents "github.com/tommoulard/atteler/pkg/events"
	"github.com/tommoulard/atteler/pkg/permission"
)

func TestClaudeCodeProvider_Complete(t *testing.T) {
	t.Parallel()

	var (
		gotReq     anthropicRequest
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

		w.Header().Set("Content-Type", "application/json")
		assert.NoError(t, json.NewEncoder(w).Encode(map[string]any{
			"model": "claude-opus-4-7",
			"content": []map[string]any{
				{"type": "text", "text": "hello back"},
			},
			"usage": map[string]any{
				"input_tokens":            12,
				"cache_read_input_tokens": 4,
				"output_tokens":           3,
			},
		}))
	}))
	defer srv.Close()

	auth := newTestClaudeCodeAuth(t, "access-1", "refresh-1", futureExpiry())
	p := &ClaudeCodeProvider{
		client:  srv.Client(),
		auth:    auth,
		baseURL: srv.URL,
		models:  []string{"claude-opus-4-7"},
	}

	resp, err := p.Complete(context.Background(), CompleteParams{
		Model: "claude-opus-4-7",
		Messages: []Message{
			{Role: RoleSystem, Content: "be brief"},
			{Role: RoleUser, Content: "say ok"},
		},
	})
	require.NoError(t, err)

	assert.Equal(t, "hello back", resp.Content)
	assert.Equal(t, providerClaudeCode, resp.Provider)
	assert.Equal(t, "claude-opus-4-7", resp.Model)
	// 12 input + 4 cache_read = 16 total in our reporting; 4 marked as cached.
	assert.Equal(t, 16, resp.InputTokens)
	assert.Equal(t, 4, resp.CachedInputTokens)
	assert.Equal(t, 3, resp.OutputTokens)

	assert.Equal(t, "/v1/messages", gotPath)
	assert.JSONEq(t, `{
		"model": "claude-opus-4-7",
		"system": [
			{"type": "text", "text": "be brief", "cache_control": {"type": "ephemeral"}}
		],
		"messages": [
			{"role": "user", "content": [
				{"type": "text", "text": "say ok", "cache_control": {"type": "ephemeral"}}
			]}
		],
		"max_tokens": 4096
	}`, string(gotBody))
	require.Len(t, gotReq.System, 1)
	assert.Equal(t, "be brief", gotReq.System[0].Text)
	require.NotNil(t, gotReq.System[0].CacheControl)
	assert.Equal(t, "ephemeral", gotReq.System[0].CacheControl.Type)
	require.Len(t, gotReq.Messages, 1)
	assert.Equal(t, "user", gotReq.Messages[0].Role)
	assert.JSONEq(t, `[{"type":"text","text":"say ok","cache_control":{"type":"ephemeral"}}]`, string(gotReq.Messages[0].Content))
	assert.Equal(t, "claude-opus-4-7", gotReq.Model)

	assert.Equal(t, "Bearer access-1", gotHeaders.Get("Authorization"))
	assert.Equal(t, "application/json", gotHeaders.Get("Content-Type"))
	assert.Equal(t, defaultAnthropicVersion, gotHeaders.Get("anthropic-version"))
	assert.Equal(t, anthropicOAuthBetas, gotHeaders.Get("anthropic-beta"))
}

func TestClaudeCodeProvider_CompleteStream(t *testing.T) {
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
data: {"type":"message_start","message":{"model":"claude-opus-4-7","usage":{"input_tokens":4,"cache_read_input_tokens":1}}}

event: content_block_delta
data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"stre"}}

event: content_block_delta
data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"amed"}}

event: message_delta
data: {"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"output_tokens":2}}

event: message_stop
data: {"type":"message_stop"}

`)
		assert.NoError(t, err)
	}))
	defer srv.Close()

	auth := newTestClaudeCodeAuth(t, "access-1", "refresh-1", futureExpiry())
	p := &ClaudeCodeProvider{
		client:  srv.Client(),
		auth:    auth,
		baseURL: srv.URL,
		models:  []string{"claude-opus-4-7"},
	}

	ch, err := p.CompleteStream(context.Background(), CompleteParams{
		Model:    "claude-opus-4-7",
		Messages: []Message{{Role: RoleUser, Content: "say ok"}},
	})
	require.NoError(t, err)

	chunks := drainChunks(ch)
	require.Len(t, chunks, 3)
	assert.Equal(t, "stre", chunks[0].Content)
	assert.False(t, chunks[0].Done)
	assert.Equal(t, "amed", chunks[1].Content)
	assert.False(t, chunks[1].Done)
	assert.True(t, chunks[2].Done)

	resp, err := CollectStream(chunksToStream(chunks))
	require.NoError(t, err)

	assert.True(t, gotReq.Stream)
	assert.Equal(t, "Bearer access-1", gotHeaders.Get("Authorization"))
	assert.Equal(t, "text/event-stream", gotHeaders.Get("Accept"))
	assert.Equal(t, anthropicOAuthBetas, gotHeaders.Get("anthropic-beta"))
	assert.Equal(t, "streamed", resp.Content)
	assert.Equal(t, providerClaudeCode, resp.Provider)
	assert.Equal(t, "claude-opus-4-7", resp.Model)
	assert.Equal(t, StopEndTurn, resp.StopReason)
	assert.Equal(t, 5, resp.InputTokens)
	assert.Equal(t, 1, resp.CachedInputTokens)
	assert.Equal(t, 2, resp.OutputTokens)
}

func TestClaudeCodeProvider_CompleteCoercesThinkingTemperature(t *testing.T) {
	t.Parallel()

	var (
		gotReq  anthropicRequest
		gotBody []byte
	)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		if !assert.NoError(t, err) {
			return
		}

		gotBody = body

		if !assert.NoError(t, json.Unmarshal(gotBody, &gotReq)) {
			return
		}

		w.Header().Set("Content-Type", "application/json")
		assert.NoError(t, json.NewEncoder(w).Encode(map[string]any{
			"model": "claude-opus-4-7",
			"content": []map[string]any{
				{"type": "text", "text": "ok"},
			},
			"usage": map[string]any{
				"input_tokens":  1,
				"output_tokens": 1,
			},
		}))
	}))
	defer srv.Close()

	auth := newTestClaudeCodeAuth(t, "access-1", "refresh-1", futureExpiry())
	p := &ClaudeCodeProvider{
		client:  srv.Client(),
		auth:    auth,
		baseURL: srv.URL,
		models:  []string{"claude-opus-4-7"},
	}

	var log bytes.Buffer

	ctx := llmevents.WithEmitter(context.Background(), llmevents.NewRunnerWithLogger(nil, &log), llmevents.Event{})

	temperature := 0.2
	resp, err := p.Complete(ctx, CompleteParams{
		Model:          "claude-opus-4-7",
		ReasoningLevel: "high",
		Temperature:    &temperature,
		Messages:       []Message{{Role: RoleUser, Content: "think"}},
	})
	require.NoError(t, err)

	assert.Equal(t, "ok", resp.Content)
	require.NotNil(t, gotReq.Temperature)
	assert.InEpsilon(t, 1.0, *gotReq.Temperature, 0.0001)
	require.NotNil(t, gotReq.Thinking)
	assert.Equal(t, "enabled", gotReq.Thinking.Type)
	assert.Contains(t, log.String(), "option_adjustments")
	assert.Contains(t, log.String(), "Temperature coerced")
	assert.JSONEq(t, `{
		"temperature": 1,
		"model": "claude-opus-4-7",
		"thinking": {"type": "enabled", "budget_tokens": 2048},
		"messages": [
			{"role": "user", "content": [
				{"type": "text", "text": "think", "cache_control": {"type": "ephemeral"}}
			]}
		],
		"max_tokens": 4096
	}`, string(gotBody))
}

func TestClaudeCodeProvider_CompletePermissionDeniesNetworkBeforeActivity(t *testing.T) {
	t.Parallel()

	var requestCount atomic.Int32

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		requestCount.Add(1)
		http.Error(w, "unexpected request", http.StatusInternalServerError)
	}))
	defer srv.Close()

	p := &ClaudeCodeProvider{
		client:  srv.Client(),
		auth:    newTestClaudeCodeAuth(t, "access-1", "refresh-1", futureExpiry()),
		baseURL: srv.URL,
		models:  []string{"claude-opus-4-7"},
	}

	policy := permission.DefaultPolicy()
	policy.SetMode(permission.OperationNetwork, permission.ModeDeny)

	auditDir := t.TempDir()

	var log bytes.Buffer

	ctx := llmevents.WithEmitter(context.Background(), llmevents.NewRunnerWithLogger(nil, &log), llmevents.Event{})
	ctx = permission.ContextWithPolicy(ctx, &policy)
	ctx = permission.ContextWithAuditDir(ctx, auditDir)

	_, err := p.Complete(ctx, CompleteParams{
		Model:    "claude-opus-4-7",
		Messages: []Message{{Role: RoleUser, Content: "say ok"}},
	})
	require.Error(t, err)
	require.True(t, permission.ErrDenied(err))
	assert.Contains(t, err.Error(), "permission.network.deny")
	assert.Equal(t, int32(0), requestCount.Load())
	assert.NotContains(t, log.String(), "event:command_execute")
	assert.NotContains(t, log.String(), "claude_code.messages")

	auditData, readErr := os.ReadFile(filepath.Join(auditDir, "side_effects.jsonl"))
	require.NoError(t, readErr)
	assert.Contains(t, string(auditData), "permission.network.deny")
}

func TestClaudeCodeProvider_HealthCheckPermissionDeniesCredentialBeforeActivity(t *testing.T) {
	t.Parallel()

	p := &ClaudeCodeProvider{
		auth: newTestClaudeCodeAuth(t, "access-1", "refresh-1", futureExpiry()),
	}

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
	assert.NotContains(t, log.String(), "claude_code.auth.check")

	auditData, readErr := os.ReadFile(filepath.Join(auditDir, "side_effects.jsonl"))
	require.NoError(t, readErr)
	assert.Contains(t, string(auditData), "permission.credential_access.deny")
}

func TestClaudeCodeProvider_RefreshOn401(t *testing.T) {
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
		assert.Equal(t, claudeCodeOAuthClientID, req["client_id"])

		w.Header().Set("Content-Type", "application/json")
		assert.NoError(t, json.NewEncoder(w).Encode(map[string]any{
			"access_token":  "access-2",
			"refresh_token": "refresh-2",
			"expires_in":    3600,
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
			_, err := w.Write([]byte(`{"error":{"type":"authentication_error","message":"invalid_token"}}`))
			assert.NoError(t, err)

			return
		}

		assert.Equal(t, "Bearer access-2", token)

		w.Header().Set("Content-Type", "application/json")
		assert.NoError(t, json.NewEncoder(w).Encode(map[string]any{
			"model": "claude-haiku-4-5-20251001",
			"content": []map[string]any{
				{"type": "text", "text": "ok-after-refresh"},
			},
			"usage": map[string]any{"input_tokens": 1, "output_tokens": 1},
		}))
	}))
	defer apiSrv.Close()

	credPath := writeClaudeCodeCredentialsFile(t, "access-1", "refresh-1", futureExpiry())

	auth, err := loadClaudeCodeAuthFromFile(credPath)
	require.NoError(t, err)

	auth.refreshURL = refreshSrv.URL
	auth.httpClient = refreshSrv.Client()

	p := &ClaudeCodeProvider{
		client:  apiSrv.Client(),
		auth:    auth,
		baseURL: apiSrv.URL,
		models:  []string{"claude-haiku-4-5-20251001"},
	}

	resp, err := p.Complete(context.Background(), CompleteParams{
		Model:    "claude-haiku-4-5-20251001",
		Messages: []Message{{Role: RoleUser, Content: "hi"}},
	})
	require.NoError(t, err)

	assert.Equal(t, "ok-after-refresh", resp.Content)
	assert.EqualValues(t, 1, refreshHits.Load())
	assert.EqualValues(t, 2, apiCalls.Load())

	persisted, err := os.ReadFile(credPath)
	require.NoError(t, err)

	block, err := parseClaudeCodeCredentialsRaw(persisted)
	require.NoError(t, err)
	assert.Equal(t, "access-2", block.AccessToken)
	assert.Equal(t, "refresh-2", block.RefreshToken)
	assert.Positive(t, block.ExpiresAt, "expiresAt should be set from expires_in")
}

func TestClaudeCodeProvider_RefreshFailureSurfaced(t *testing.T) {
	t.Parallel()

	refreshSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		_, err := w.Write([]byte(`{"error":{"code":"invalid_grant"}}`))
		assert.NoError(t, err)
	}))
	defer refreshSrv.Close()

	apiSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		_, err := w.Write([]byte(`{"error":{"type":"authentication_error"}}`))
		assert.NoError(t, err)
	}))
	defer apiSrv.Close()

	credPath := writeClaudeCodeCredentialsFile(t, "access-1", "refresh-1", futureExpiry())

	auth, err := loadClaudeCodeAuthFromFile(credPath)
	require.NoError(t, err)

	auth.refreshURL = refreshSrv.URL
	auth.httpClient = refreshSrv.Client()

	p := &ClaudeCodeProvider{
		client:  apiSrv.Client(),
		auth:    auth,
		baseURL: apiSrv.URL,
		models:  []string{"claude-opus-4-7"},
	}

	_, err = p.Complete(context.Background(), CompleteParams{
		Model:    "claude-opus-4-7",
		Messages: []Message{{Role: RoleUser, Content: "hi"}},
	})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "refresh after 401")
}

func TestClaudeCodeProvider_HTTPErrorIsTyped(t *testing.T) {
	t.Parallel()

	apiSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("request-id", "req-claude-code")
		w.WriteHeader(http.StatusServiceUnavailable)
		_, err := w.Write([]byte(`{"error":{"type":"overloaded_error","message":"busy"}}`))
		assert.NoError(t, err)
	}))
	defer apiSrv.Close()

	auth := newTestClaudeCodeAuth(t, "access-1", "refresh-1", futureExpiry())
	p := &ClaudeCodeProvider{
		client:  apiSrv.Client(),
		auth:    auth,
		baseURL: apiSrv.URL,
		models:  []string{"claude-opus-4-7"},
	}

	_, err := p.Complete(context.Background(), CompleteParams{
		Model:    "claude-opus-4-7",
		Messages: []Message{{Role: RoleUser, Content: "hi"}},
	})
	require.Error(t, err)

	var providerErr *ProviderError
	require.ErrorAs(t, err, &providerErr)
	assert.Equal(t, providerClaudeCode, providerErr.Provider)
	assert.Equal(t, http.StatusServiceUnavailable, providerErr.StatusCode)
	assert.Equal(t, "req-claude-code", providerErr.RequestID)
	assert.Equal(t, RetryabilityRetryable, providerErr.Retryability)
	assert.Contains(t, providerErr.Message, "overloaded_error")
}

func TestClaudeCodeProvider_ErrorPayloadIsTyped(t *testing.T) {
	t.Parallel()

	apiSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Retry-After", "9")
		w.Header().Set("request-id", "req-claude-code-payload")
		w.Header().Set("Content-Type", "application/json")

		resp := anthropicResponse{}
		resp.Error = &struct {
			Type    string `json:"type"`
			Message string `json:"message"`
		}{
			Type:    "rate_limit_error",
			Message: "slow down",
		}

		assert.NoError(t, json.NewEncoder(w).Encode(resp))
	}))
	defer apiSrv.Close()

	auth := newTestClaudeCodeAuth(t, "access-1", "refresh-1", futureExpiry())
	p := &ClaudeCodeProvider{
		client:  apiSrv.Client(),
		auth:    auth,
		baseURL: apiSrv.URL,
		models:  []string{"claude-opus-4-7"},
	}

	_, err := p.Complete(context.Background(), CompleteParams{
		Model:    "claude-opus-4-7",
		Messages: []Message{{Role: RoleUser, Content: "hi"}},
	})
	require.Error(t, err)

	var providerErr *ProviderError
	require.ErrorAs(t, err, &providerErr)
	assert.Equal(t, providerClaudeCode, providerErr.Provider)
	assert.Equal(t, http.StatusOK, providerErr.StatusCode)
	assert.Equal(t, 9*time.Second, providerErr.RetryAfter)
	assert.Equal(t, "req-claude-code-payload", providerErr.RequestID)
	assert.Equal(t, RetryabilityRetryable, providerErr.Retryability)
	assert.Equal(t, "rate_limit_error: slow down", providerErr.Message)
}

func TestClaudeCodeProvider_RefreshHonorsCanceledContext(t *testing.T) {
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
		_, err := w.Write([]byte(`{"error":{"type":"authentication_error"}}`))
		assert.NoError(t, err)
	}))
	defer apiSrv.Close()

	credPath := writeClaudeCodeCredentialsFile(t, "access-1", "refresh-1", futureExpiry())

	auth, err := loadClaudeCodeAuthFromFile(credPath)
	require.NoError(t, err)

	auth.refreshURL = refreshSrv.URL
	auth.httpClient = refreshSrv.Client()

	p := &ClaudeCodeProvider{
		client:  apiSrv.Client(),
		auth:    auth,
		baseURL: apiSrv.URL,
		models:  []string{"claude-opus-4-7"},
	}

	ctx, cancel := context.WithCancel(context.Background())
	errCh := make(chan error, 1)

	go func() {
		_, completeErr := p.Complete(ctx, CompleteParams{
			Model:    "claude-opus-4-7",
			Messages: []Message{{Role: RoleUser, Content: "hi"}},
		})
		errCh <- completeErr
	}()

	<-requestStarted
	cancel()

	err = <-errCh
	require.Error(t, err)
	require.ErrorIs(t, err, context.Canceled)
	assert.Equal(t, "access-1", auth.snapshot())

	persisted, err := os.ReadFile(credPath)
	require.NoError(t, err)

	block, err := parseClaudeCodeCredentialsRaw(persisted)
	require.NoError(t, err)
	assert.Equal(t, "access-1", block.AccessToken)
	assert.Equal(t, "refresh-1", block.RefreshToken)
}

func TestClaudeCodeProvider_MissingRefreshTokenFailsLoudlyOn401(t *testing.T) {
	t.Parallel()

	var apiCalls atomic.Int32

	apiSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		apiCalls.Add(1)
		w.WriteHeader(http.StatusUnauthorized)
		_, err := w.Write([]byte(`{"error":{"type":"authentication_error"}}`))
		assert.NoError(t, err)
	}))
	defer apiSrv.Close()

	credPath := writeClaudeCodeCredentialsFile(t, "access-1", "", futureExpiry())

	auth, err := loadClaudeCodeAuthFromFile(credPath)
	require.NoError(t, err)

	p := &ClaudeCodeProvider{
		client:  apiSrv.Client(),
		auth:    auth,
		baseURL: apiSrv.URL,
		models:  []string{"claude-opus-4-7"},
	}

	_, err = p.Complete(context.Background(), CompleteParams{
		Model:    "claude-opus-4-7",
		Messages: []Message{{Role: RoleUser, Content: "hi"}},
	})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "refresh after 401")
	assert.Contains(t, err.Error(), "no refresh_token")
	assert.EqualValues(t, 1, apiCalls.Load())
}

func TestParseClaudeCodeCredentialsRaw_AcceptsExpired(t *testing.T) {
	t.Parallel()

	expired := `{"claudeAiOauth":{"accessToken":"a","refreshToken":"r","expiresAt":1}}`

	block, err := parseClaudeCodeCredentialsRaw([]byte(expired))
	require.NoError(t, err)
	assert.Equal(t, "a", block.AccessToken)
	assert.Equal(t, "r", block.RefreshToken)
	assert.EqualValues(t, 1, block.ExpiresAt)

	missingBlock := `{}`
	_, err = parseClaudeCodeCredentialsRaw([]byte(missingBlock))
	require.Error(t, err)

	emptyOAuthBlock, err := json.Marshal(map[string]any{"claudeAiOauth": map[string]any{}})
	require.NoError(t, err)

	_, err = parseClaudeCodeCredentialsRaw(emptyOAuthBlock)
	require.Error(t, err)
}

func TestPersistRefreshedClaudeCodeFile_AtomicAndPreservesUnknown(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	path := filepath.Join(dir, ".credentials.json")
	original := `{
  "claudeAiOauth": {
    "accessToken": "old-access",
    "refreshToken": "old-refresh",
    "expiresAt": 1,
    "scopes": ["user:inference"],
    "subscriptionType": "team"
  },
  "custom_field": "preserved"
}`
	require.NoError(t, os.WriteFile(path, []byte(original), 0o600))

	persister := &claudeCodeFilePersister{path: path}
	require.NoError(t, persister.persist(context.Background(), "new-access", "new-refresh", 9999999999999))

	updated, err := os.ReadFile(path)
	require.NoError(t, err)

	var raw map[string]any
	require.NoError(t, json.Unmarshal(updated, &raw))

	assert.Equal(t, "preserved", raw["custom_field"])

	block, ok := raw["claudeAiOauth"].(map[string]any)
	require.True(t, ok, "claudeAiOauth must remain a JSON object")
	assert.Equal(t, "new-access", block["accessToken"])
	assert.Equal(t, "new-refresh", block["refreshToken"])
	assert.EqualValues(t, 9999999999999, block["expiresAt"])
	assert.Equal(t, "team", block["subscriptionType"], "unrelated OAuth fields must be preserved")

	scopes, ok := block["scopes"].([]any)
	require.True(t, ok, "scopes must remain a JSON array")
	require.Len(t, scopes, 1)
	assert.Equal(t, "user:inference", scopes[0])
}

func TestPersistRefreshedClaudeCodeFile_RequiresActiveContext(t *testing.T) {
	t.Parallel()

	path := writeClaudeCodeCredentialsFile(t, "old-access", "old-refresh", futureExpiry())
	persister := &claudeCodeFilePersister{path: path}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	err := persister.persist(ctx, "new-access", "new-refresh", 9999999999999)
	require.Error(t, err)
	require.ErrorIs(t, err, context.Canceled)

	auth, err := loadClaudeCodeAuthFromFile(path)
	require.NoError(t, err)
	assert.Equal(t, "old-access", auth.snapshot())
}

func TestPersistRefreshedClaudeCodeFile_PermissionPolicyDeniesWrite(t *testing.T) {
	t.Parallel()

	path := writeClaudeCodeCredentialsFile(t, "old-access", "old-refresh", futureExpiry())
	persister := &claudeCodeFilePersister{path: path}

	policy := permission.DefaultPolicy()
	policy.SetMode(permission.OperationWrite, permission.ModeDeny)

	auditDir := t.TempDir()
	ctx := permission.ContextWithPolicy(t.Context(), &policy)
	ctx = permission.ContextWithAuditDir(ctx, auditDir)

	err := persister.persist(ctx, "new-access", "new-refresh", 9999999999999)
	require.Error(t, err)
	require.True(t, permission.ErrDenied(err))
	assert.Contains(t, err.Error(), "permission.write.deny")

	auth, loadErr := loadClaudeCodeAuthFromFile(path)
	require.NoError(t, loadErr)
	assert.Equal(t, "old-access", auth.snapshot())

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
}

func TestClaudeCodeAuthRefresh_AuditsPersisterIdentifierWithoutSecrets(t *testing.T) {
	t.Parallel()

	refreshSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		assert.NoError(t, json.NewEncoder(w).Encode(map[string]any{
			"access_token":  "new-access-audit",
			"refresh_token": "new-refresh-audit",
			"expires_in":    3600,
		}))
	}))
	defer refreshSrv.Close()

	auth := newClaudeCodeAuthFromBlock(claudeOAuthBlock{
		AccessToken:  "old-access-audit",
		RefreshToken: "old-refresh-audit",
		ExpiresAt:    1,
	}, &recordingClaudeCodePersister{
		locationValue:   claudeCodeKeychainSource,
		identifierValue: "keychain-account-audit",
	})
	auth.refreshURL = refreshSrv.URL
	auth.httpClient = refreshSrv.Client()

	auditDir := t.TempDir()
	ctx := permission.ContextWithAuditDir(context.Background(), auditDir)

	require.NoError(t, auth.refresh(ctx, "old-access-audit"))

	auditData, readErr := os.ReadFile(filepath.Join(auditDir, credentialAuditLedgerFileName))
	require.NoError(t, readErr)

	audit := string(auditData)
	assert.Contains(t, audit, credentialAuditEventRefresh)
	assert.Contains(t, audit, credentialAuditEventWriteBack)
	assert.Contains(t, audit, "sha256:")
	assert.NotContains(t, audit, "keychain-account-audit")
	assert.NotContains(t, audit, "old-access-audit")
	assert.NotContains(t, audit, "old-refresh-audit")
	assert.NotContains(t, audit, "new-access-audit")
	assert.NotContains(t, audit, "new-refresh-audit")
}

func TestClaudeCodeAuthRefresh_ConcurrentRefreshUsesCASWinner(t *testing.T) {
	t.Parallel()

	credPath := writeClaudeCodeCredentialsFile(t, "access-1", "refresh-1", futureExpiry())

	var refreshHits atomic.Int32

	refreshSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hit := refreshHits.Add(1) + 1

		var req map[string]string
		if !assert.NoError(t, json.NewDecoder(r.Body).Decode(&req)) {
			return
		}

		assert.Equal(t, "refresh-1", req["refresh_token"])

		w.Header().Set("Content-Type", "application/json")
		assert.NoError(t, json.NewEncoder(w).Encode(map[string]any{
			"access_token":  fmt.Sprintf("access-%d", hit),
			"refresh_token": fmt.Sprintf("refresh-%d", hit),
			"expires_in":    3600,
		}))
	}))
	defer refreshSrv.Close()

	auth1, err := loadClaudeCodeAuthFromFile(credPath)
	require.NoError(t, err)
	auth2, err := loadClaudeCodeAuthFromFile(credPath)
	require.NoError(t, err)

	for _, auth := range []*claudeCodeAuth{auth1, auth2} {
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

	persisted, err := os.ReadFile(credPath)
	require.NoError(t, err)

	block, err := parseClaudeCodeCredentialsRaw(persisted)
	require.NoError(t, err)
	require.Contains(t, []string{"access-2", "access-3"}, block.AccessToken)
	assert.Contains(t, []string{"refresh-2", "refresh-3"}, block.RefreshToken)

	assert.Equal(t, block.AccessToken, auth1.snapshot())
	assert.Equal(t, block.AccessToken, auth2.snapshot())

	auditData, readErr := os.ReadFile(filepath.Join(auditDir, credentialAuditLedgerFileName))
	require.NoError(t, readErr)
	assert.Contains(t, string(auditData), credentialAuditEventCAS)
	assert.NotContains(t, string(auditData), "access-2")
	assert.NotContains(t, string(auditData), "access-3")
}

func TestLoadClaudeCodeAuth_FilePath(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("HOME", dir)
	t.Setenv("ATTELER_CLAUDE_CODE_SKIP_KEYCHAIN", "1")

	claudeDir := filepath.Join(dir, ".claude")
	require.NoError(t, os.MkdirAll(claudeDir, 0o750))

	path := filepath.Join(claudeDir, ".credentials.json")
	body := `{"claudeAiOauth":{"accessToken":"a","refreshToken":"r","expiresAt":9999999999999}}`
	require.NoError(t, os.WriteFile(path, []byte(body), 0o600))

	auth, err := loadClaudeCodeAuth(ContextWithCredentialSourcePolicy(context.Background(), CredentialSourcePolicy{AllowedStores: []string{CredentialStoreClaudeCodeFile}, AllowBorrowedOAuth: true}))
	require.NoError(t, err)

	assert.Equal(t, "a", auth.snapshot())
	assert.Equal(t, path, auth.persist.location())
}

func TestClaudeCodeProvider_ModelMetadataAndContextFallback(t *testing.T) {
	t.Parallel()

	p := &ClaudeCodeProvider{models: []string{"claude-opus-4-7", "opus"}}

	metadata, ok := p.ModelMetadata("claude-opus-4-7")
	require.True(t, ok)
	assert.Equal(t, 1_000_000, metadata.ContextWindow)
	assert.Contains(t, metadata.Provenance, "built-in provider/model catalog")
	assert.NotEmpty(t, metadata.ReviewedAt)
	assert.NotEmpty(t, metadata.ReviewAfter)

	alias, ok := p.ModelMetadata("opus")
	require.True(t, ok)
	assert.Equal(t, 200_000, alias.ContextWindow)
	assert.Contains(t, alias.Provenance, "alias")

	haikuAlias, ok := p.ModelMetadata("claude-haiku-4-5")
	require.True(t, ok)
	assert.Equal(t, 200_000, haikuAlias.ContextWindow)
	assert.Contains(t, haikuAlias.Provenance, "built-in provider/model catalog")

	assert.Zero(t, p.ModelContextWindow("claude-unknown-private"))
}

func TestClaudeCodeProvider_StaticModelCatalogConformance(t *testing.T) {
	t.Parallel()

	p := &ClaudeCodeProvider{models: []string{"claude-opus-4-7", "opus"}}

	models, err := p.FetchModels(context.Background())
	require.NoError(t, err)
	assert.Equal(t, []string{"claude-opus-4-7", "opus"}, models)

	catalog := p.ModelCatalog()
	require.Len(t, catalog, 2)
	assert.Equal(t, "claude-opus-4-7", catalog[0].ID)
	assert.Equal(t, 1_000_000, catalog[0].ContextWindow)
	assert.Contains(t, catalog[0].Provenance, "built-in provider/model catalog")
	assert.Equal(t, claudeCodeAdapterReviewAfter, catalog[0].ReviewAfter)

	assert.Equal(t, "opus", catalog[1].ID)
	assert.Equal(t, 200_000, catalog[1].ContextWindow)
	assert.Contains(t, catalog[1].Provenance, "static Claude Code CLI alias")
	assert.Equal(t, claudeCodeAdapterReviewAfter, catalog[1].ReviewAfter)
}

func TestClaudeCodeProvider_AdapterDiagnostics(t *testing.T) {
	t.Parallel()

	p := &ClaudeCodeProvider{
		auth:   newTestClaudeCodeAuth(t, "access-1", "refresh-1", futureExpiry()),
		models: []string{"claude-opus-4-7"},
	}

	diagnostics := p.AdapterDiagnostics()
	assert.True(t, diagnostics.Healthy())
	assert.Equal(t, claudeCodeAdapterVersion, diagnostics.Contract.AdapterVersion)
	assert.NotEmpty(t, diagnostics.Contract.SourceCLIVersion)
	assert.Contains(t, diagnostics.Contract.KillSwitches, "providers.claude-code.disable_private_adapter")
	assert.Contains(t, diagnostics.Contract.KillSwitches, "ATTELER_DISABLE_CLAUDE_CODE_ADAPTER")
	assert.Equal(t, claudeCodeAdapterReviewAfter, diagnostics.Contract.ReviewAfter)

	checks := readinessChecksByName(diagnostics.Checks)
	assert.Equal(t, ReadinessOK, checks["local_credentials"].Status)
	assert.Equal(t, ReadinessOK, checks["token_refresh"].Status)
	assert.Equal(t, ReadinessOK, checks["credential_provenance"].Status)
	assert.Contains(t, checks["credential_provenance"].Detail, CredentialStoreClaudeCodeFile)
	assert.NotContains(t, checks["credential_provenance"].Detail, "access-1")
	assert.Contains(t, checks["credential_policy"].Detail, "allow_borrowed_oauth=true")
	assert.Equal(t, ReadinessSkipped, checks["network_reachability"].Status)
	assert.Equal(t, ReadinessWarning, checks["model_availability"].Status)
	assert.Contains(t, diagnostics.Warnings[0], "beta")
}

func TestClaudeCodeProvider_AdapterDiagnosticsSeparatesAccessAndRefreshReadiness(t *testing.T) {
	t.Parallel()

	p := &ClaudeCodeProvider{
		auth:   newTestClaudeCodeAuth(t, "access-without-refresh", "", futureExpiry()),
		models: []string{"claude-opus-4-7"},
	}

	diagnostics := p.AdapterDiagnostics()
	assert.False(t, diagnostics.Healthy())

	checks := readinessChecksByName(diagnostics.Checks)
	assert.Equal(t, ReadinessOK, checks["local_credentials"].Status)
	assert.Equal(t, ReadinessFailed, checks["token_refresh"].Status)

	err := diagnostics.Error()
	require.Error(t, err)
	assert.Contains(t, err.Error(), "token_refresh")
}

func TestNewClaudeCodeProviderWithConfig_HonorsPrivateAdapterKillSwitchBeforeCredentials(t *testing.T) {
	t.Setenv("HOME", filepath.Join(t.TempDir(), "missing-home"))

	_, err := NewClaudeCodeProviderWithConfigContext(
		context.Background(),
		ProviderConfig{DisablePrivateAdapter: true},
	)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "private adapter disabled")
}

func TestNewClaudeCodeProviderWithConfig_HonorsPrivateAdapterEnvKillSwitchBeforeCredentials(t *testing.T) {
	t.Setenv("HOME", filepath.Join(t.TempDir(), "missing-home"))
	t.Setenv("ATTELER_DISABLE_CLAUDE_CODE_ADAPTER", "1")

	_, err := NewClaudeCodeProviderWithConfigContext(context.Background(), ProviderConfig{})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "private adapter disabled")
}

// ---------------------------------------------------------------------------
// Helpers
// ---------------------------------------------------------------------------

type recordingClaudeCodePersister struct {
	locationValue   string
	identifierValue string
}

func (p *recordingClaudeCodePersister) persist(_ context.Context, _, _ string, _ int64) error {
	return nil
}

func (p *recordingClaudeCodePersister) location() string { return p.locationValue }

func (p *recordingClaudeCodePersister) identifier() string { return p.identifierValue }

func newTestClaudeCodeAuth(t *testing.T, access, refresh string, expiresAtMs int64) *claudeCodeAuth {
	t.Helper()

	credPath := writeClaudeCodeCredentialsFile(t, access, refresh, expiresAtMs)

	auth, err := loadClaudeCodeAuthFromFile(credPath)
	require.NoError(t, err)

	return auth
}

func writeClaudeCodeCredentialsFile(t *testing.T, access, refresh string, expiresAtMs int64) string {
	t.Helper()

	path := filepath.Join(t.TempDir(), ".credentials.json")
	body, err := json.Marshal(map[string]any{
		"claudeAiOauth": map[string]any{
			"accessToken":  access,
			"refreshToken": refresh,
			"expiresAt":    expiresAtMs,
		},
	})
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(path, body, 0o600))

	return path
}

// loadClaudeCodeAuthFromFile is a test-only loader bypassing the keychain
// probe so tests are deterministic across platforms.
func loadClaudeCodeAuthFromFile(path string) (*claudeCodeAuth, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}

	block, err := parseClaudeCodeCredentialsRaw(data)
	if err != nil {
		return nil, err
	}

	return newClaudeCodeAuthFromBlock(block, &claudeCodeFilePersister{path: path}), nil
}

// futureExpiry returns an epoch-ms timestamp comfortably in the future.
func futureExpiry() int64 { return 9999999999999 }
