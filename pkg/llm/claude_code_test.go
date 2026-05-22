package llm

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestClaudeCodeProvider_Complete(t *testing.T) {
	t.Parallel()

	var (
		gotReq     anthropicRequest
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

		if !assert.NoError(t, json.Unmarshal(body, &gotReq)) {
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
	assert.Equal(t, "claude-opus-4-7", resp.Model)
	// 12 input + 4 cache_read = 16 total in our reporting; 4 marked as cached.
	assert.Equal(t, 16, resp.InputTokens)
	assert.Equal(t, 4, resp.CachedInputTokens)
	assert.Equal(t, 3, resp.OutputTokens)

	assert.Equal(t, "/v1/messages", gotPath)
	assert.Equal(t, "be brief", gotReq.System)
	require.Len(t, gotReq.Messages, 1)
	assert.Equal(t, "user", gotReq.Messages[0].Role)
	assert.JSONEq(t, `"say ok"`, string(gotReq.Messages[0].Content))
	assert.Equal(t, "claude-opus-4-7", gotReq.Model)

	assert.Equal(t, "Bearer access-1", gotHeaders.Get("Authorization"))
	assert.Equal(t, defaultAnthropicVersion, gotHeaders.Get("anthropic-version"))
	assert.Equal(t, anthropicOAuthBetas, gotHeaders.Get("anthropic-beta"))
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

func TestLoadClaudeCodeAuth_FilePath(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("HOME", dir)
	t.Setenv("ATTELER_CLAUDE_CODE_SKIP_KEYCHAIN", "1")

	claudeDir := filepath.Join(dir, ".claude")
	require.NoError(t, os.MkdirAll(claudeDir, 0o750))

	path := filepath.Join(claudeDir, ".credentials.json")
	body := `{"claudeAiOauth":{"accessToken":"a","refreshToken":"r","expiresAt":9999999999999}}`
	require.NoError(t, os.WriteFile(path, []byte(body), 0o600))

	auth, err := loadClaudeCodeAuth(context.Background())
	require.NoError(t, err)

	assert.Equal(t, "a", auth.snapshot())
	assert.Equal(t, path, auth.persist.location())
}

// ---------------------------------------------------------------------------
// Helpers
// ---------------------------------------------------------------------------

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
