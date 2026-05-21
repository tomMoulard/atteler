package llm

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// ---------------------------------------------------------------------------
// Anthropic credential resolution
// ---------------------------------------------------------------------------

func TestResolveAnthropicKey_EnvAPIKey(t *testing.T) {
	t.Setenv("ANTHROPIC_API_KEY", "sk-ant-test")
	t.Setenv("ANTHROPIC_AUTH_TOKEN", "")
	t.Setenv("CLAUDE_CODE_OAUTH_TOKEN", "")
	t.Setenv("FORGE_CONFIG", "")

	key, bearer, err := ResolveAnthropicKeyContext(context.Background())
	if err != nil {
		require.NoError(t, err)
	}

	if key != "sk-ant-test" {
		assert.Failf(t, "assertion failed", "expected sk-ant-test, got %q", key)
	}

	if bearer {
		assert.Fail(t, "expected bearer=false for API key")
	}
}

func TestResolveAnthropicKey_EnvAuthToken(t *testing.T) {
	t.Setenv("ANTHROPIC_API_KEY", "")
	t.Setenv("ANTHROPIC_AUTH_TOKEN", "bearer-tok")
	t.Setenv("CLAUDE_CODE_OAUTH_TOKEN", "")
	t.Setenv("FORGE_CONFIG", "")

	key, bearer, err := ResolveAnthropicKeyContext(context.Background())
	if err != nil {
		require.NoError(t, err)
	}

	if key != "bearer-tok" || !bearer {
		assert.Failf(t, "assertion failed", "got key=%q bearer=%v, want bearer-tok/true", key, bearer)
	}
}

func TestResolveAnthropicKey_ClaudeCodeOAuth(t *testing.T) {
	t.Setenv("ANTHROPIC_API_KEY", "")
	t.Setenv("ANTHROPIC_AUTH_TOKEN", "")
	t.Setenv("CLAUDE_CODE_OAUTH_TOKEN", "oauth-tok")
	t.Setenv("FORGE_CONFIG", "")

	key, bearer, err := ResolveAnthropicKeyContext(context.Background())
	if err != nil {
		require.NoError(t, err)
	}

	if key != "oauth-tok" || !bearer {
		assert.Failf(t, "assertion failed", "got key=%q bearer=%v, want oauth-tok/true", key, bearer)
	}
}

func TestResolveAnthropicKey_CredentialsFile(t *testing.T) {
	t.Setenv("ANTHROPIC_API_KEY", "")
	t.Setenv("ANTHROPIC_AUTH_TOKEN", "")
	t.Setenv("CLAUDE_CODE_OAUTH_TOKEN", "")
	t.Setenv("FORGE_CONFIG", "")

	dir := t.TempDir()
	t.Setenv("HOME", dir)

	claudeDir := filepath.Join(dir, ".claude")
	if err := os.MkdirAll(claudeDir, 0o750); err != nil {
		require.NoError(t, err)
	}

	data := `{"claudeAiOauth":{"accessToken":"sk-from-file","refreshToken":"rt","expiresAt":9999999999999}}`
	if err := os.WriteFile(filepath.Join(claudeDir, ".credentials.json"), []byte(data), 0o600); err != nil {
		require.NoError(t, err)
	}

	key, bearer, err := ResolveAnthropicKeyContext(context.Background())
	if err != nil {
		require.NoError(t, err)
	}

	if key != "sk-from-file" || !bearer {
		assert.Failf(t, "assertion failed", "got key=%q bearer=%v, want sk-from-file/true", key, bearer)
	}
}

func TestResolveAnthropicKey_ForgeClaudeCodeCredentials(t *testing.T) {
	t.Setenv("ANTHROPIC_API_KEY", "")
	t.Setenv("ANTHROPIC_AUTH_TOKEN", "")
	t.Setenv("CLAUDE_CODE_OAUTH_TOKEN", "")

	dir := t.TempDir()
	t.Setenv("HOME", dir)

	forgeDir := filepath.Join(dir, "forge")
	if err := os.MkdirAll(forgeDir, 0o750); err != nil {
		require.NoError(t, err)
	}

	data := `[
		{"id":"forge_services","auth_details":{"api_key":"forge-service-key"}},
		{"id":"claude_code","auth_details":{"o_auth":{"tokens":{"access_token":"forge-oauth","refresh_token":"rt","expires_at":"2099-01-01T00:00:00Z"}}}}
	]`
	if err := os.WriteFile(filepath.Join(forgeDir, ".credentials.json"), []byte(data), 0o600); err != nil {
		require.NoError(t, err)
	}

	key, bearer, err := ResolveAnthropicKeyContext(context.Background())
	if err != nil {
		require.NoError(t, err)
	}

	if key != "forge-oauth" || !bearer {
		assert.Failf(t, "assertion failed", "got key=%q bearer=%v, want forge-oauth/true", key, bearer)
	}
}

func TestResolveAnthropicKey_NoCreds(t *testing.T) {
	t.Setenv("ANTHROPIC_API_KEY", "")
	t.Setenv("ANTHROPIC_AUTH_TOKEN", "")
	t.Setenv("CLAUDE_CODE_OAUTH_TOKEN", "")
	t.Setenv("FORGE_CONFIG", "")

	dir := t.TempDir()
	t.Setenv("HOME", dir)

	_, _, err := ResolveAnthropicKeyContext(context.Background())
	if err == nil {
		require.FailNow(t, "expected error when no credentials set")
	}
}

func TestResolveAnthropicKey_CompatibilityHelperRequiresContext(t *testing.T) {
	t.Parallel()

	_, _, err := ResolveAnthropicKey()
	require.Error(t, err)
	require.ErrorIs(t, err, ErrContextRequired)
}

func TestResolveAnthropicKeyContext_RefreshHonorsCanceledContext(t *testing.T) {
	t.Setenv("ANTHROPIC_API_KEY", "")
	t.Setenv("ANTHROPIC_AUTH_TOKEN", "")
	t.Setenv("CLAUDE_CODE_OAUTH_TOKEN", "")

	requestStarted := make(chan struct{})
	release := make(chan struct{})

	srv := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		close(requestStarted)

		select {
		case <-r.Context().Done():
			return
		case <-release:
			return
		}
	}))
	defer srv.Close()
	defer close(release)

	dir := t.TempDir()
	t.Setenv("FORGE_CONFIG", dir)
	t.Setenv("HOME", t.TempDir())

	data := `[
		{"id":"claude_code","auth_details":{"o_auth":{
			"config":{"token_url":"` + srv.URL + `","client_id":"client-123"},
			"tokens":{"access_token":"expired","refresh_token":"old-refresh","expires_at":"2000-01-01T00:00:00Z"}
		}}}
	]`
	require.NoError(t, os.WriteFile(filepath.Join(dir, ".credentials.json"), []byte(data), 0o600))

	ctx, cancel := context.WithCancel(context.Background())
	errCh := make(chan error, 1)

	go func() {
		_, _, err := ResolveAnthropicKeyContext(ctx)
		errCh <- err
	}()

	<-requestStarted
	cancel()

	err := <-errCh
	require.Error(t, err)
	assert.ErrorIs(t, err, context.Canceled)
}

func TestParseClaudeCodeCredentials(t *testing.T) {
	t.Parallel()

	good := `{"claudeAiOauth":{"accessToken":"tok123","refreshToken":"rt","expiresAt":9999999999999}}`

	tok, err := parseClaudeCodeCredentials([]byte(good))
	if err != nil {
		require.NoError(t, err)
	}

	if tok != "tok123" {
		assert.Failf(t, "assertion failed", "got %q, want tok123", tok)
	}

	expired := `{"claudeAiOauth":{"accessToken":"tok123","refreshToken":"rt","expiresAt":1}}`

	_, err = parseClaudeCodeCredentials([]byte(expired))
	if err == nil {
		require.FailNow(t, "expected error for expired accessToken")
	}

	// Missing token.
	empty := `{"claudeAiOauth":{"accessToken":"","refreshToken":"rt","expiresAt":1}}`

	_, err = parseClaudeCodeCredentials([]byte(empty))
	if err == nil {
		require.FailNow(t, "expected error for empty accessToken")
	}

	// No claudeAiOauth block.
	noBlock := `{}`

	_, err = parseClaudeCodeCredentials([]byte(noBlock))
	if err == nil {
		require.FailNow(t, "expected error for missing block")
	}

	// Invalid JSON.
	_, err = parseClaudeCodeCredentials([]byte(`not json`))
	if err == nil {
		require.FailNow(t, "expected error for bad JSON")
	}
}

func TestReadForgeCredentialsFile_RefreshesExpiredClaudeCodeOAuth(t *testing.T) {
	t.Parallel()

	var got map[string]string

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			assert.Failf(t, "assertion failed", "method = %s, want POST", r.Method)
		}

		if r.Header.Get("Content-Type") != "application/json" {
			assert.Failf(t, "assertion failed", "Content-Type = %q, want application/json", r.Header.Get("Content-Type"))
		}

		if !assert.NoError(t, json.NewDecoder(r.Body).Decode(&got)) {
			return
		}

		w.Header().Set("Content-Type", "application/json")

		if err := json.NewEncoder(w).Encode(map[string]any{
			"access_token":  "new-access",
			"refresh_token": "new-refresh",
			"expires_in":    3600,
		}); err != nil {
			assert.NoError(t, err)
		}
	}))
	defer srv.Close()

	path := filepath.Join(t.TempDir(), ".credentials.json")

	data := `[
		{"id":"forge_services","auth_details":{"api_key":"forge-service-key"},"url_params":{"user_id":"kept"}},
		{"id":"claude_code","auth_details":{"o_auth":{
			"config":{"token_url":"` + srv.URL + `","client_id":"client-123"},
			"tokens":{"access_token":"expired","refresh_token":"old-refresh","expires_at":"2000-01-01T00:00:00Z"}
		}}}
	]`
	if err := os.WriteFile(path, []byte(data), 0o600); err != nil {
		require.NoError(t, err)
	}

	key, bearer, err := readForgeCredentialsFile(context.Background(), path)
	if err != nil {
		require.NoError(t, err)
	}

	if key != "new-access" || !bearer {
		assert.Failf(t, "assertion failed", "got key=%q bearer=%v, want new-access/true", key, bearer)
	}

	if got["grant_type"] != forgeOAuthRefreshGrantType ||
		got["refresh_token"] != "old-refresh" ||
		got["client_id"] != "client-123" {
		assert.Failf(t, "assertion failed", "refresh request = %#v", got)
	}

	refreshed, err := os.ReadFile(path)
	if err != nil {
		require.NoError(t, err)
	}

	if !json.Valid(refreshed) {
		require.Failf(t, "unexpected failure", "refreshed credentials are not valid JSON: %s", refreshed)
	}

	if !containsAll(string(refreshed), "new-access", "new-refresh", "user_id") {
		require.Failf(t, "unexpected failure", "refreshed credentials did not preserve/update expected fields: %s", refreshed)
	}
}

func TestReadForgeCredentialsFile_RefreshHonorsCanceledContext(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), ".credentials.json")

	data := `[
		{"id":"claude_code","auth_details":{"o_auth":{
			"config":{"token_url":"http://127.0.0.1:1/token","client_id":"client-123"},
			"tokens":{"access_token":"expired","refresh_token":"old-refresh","expires_at":"2000-01-01T00:00:00Z"}
		}}}
	]`
	if err := os.WriteFile(path, []byte(data), 0o600); err != nil {
		require.NoError(t, err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	_, _, err := readForgeCredentialsFile(ctx, path)
	if err == nil {
		require.FailNow(t, "expected canceled context to stop OAuth refresh")
	}

	assert.Contains(t, err.Error(), "context canceled")
}

func TestReadForgeCredentialsFile_CanceledRefreshDoesNotFallBackToAPIKey(t *testing.T) {
	t.Parallel()

	requestStarted := make(chan struct{})
	release := make(chan struct{})

	srv := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		close(requestStarted)

		select {
		case <-r.Context().Done():
			return
		case <-release:
			return
		}
	}))
	defer srv.Close()
	defer close(release)

	path := filepath.Join(t.TempDir(), ".credentials.json")
	data := `[
		{"id":"claude_code","auth_details":{"o_auth":{
			"config":{"token_url":"` + srv.URL + `","client_id":"client-123"},
			"tokens":{"access_token":"expired","refresh_token":"old-refresh","expires_at":"2000-01-01T00:00:00Z"}
		}}},
		{"id":"anthropic","auth_details":{"api_key":"sk-api"}}
	]`
	require.NoError(t, os.WriteFile(path, []byte(data), 0o600))

	ctx, cancel := context.WithCancel(context.Background())
	errCh := make(chan error, 1)

	go func() {
		_, _, err := readForgeCredentialsFile(ctx, path)
		errCh <- err
	}()

	<-requestStarted
	cancel()

	err := <-errCh
	require.Error(t, err)
	assert.ErrorIs(t, err, context.Canceled)
}

func TestParseForgeAnthropicCredentials(t *testing.T) {
	t.Parallel()

	data := `[
		{"id":"anthropic","auth_details":{"api_key":"sk-api"}},
		{"id":"claude_code","auth_details":{"o_auth":{"tokens":{"access_token":"oauth-token"}}}}
	]`

	key, bearer, err := parseForgeAnthropicCredentials([]byte(data))
	if err != nil {
		require.NoError(t, err)
	}

	if key != "oauth-token" || !bearer {
		assert.Failf(t, "assertion failed", "got key=%q bearer=%v, want oauth-token/true", key, bearer)
	}

	data = `[{"id":"anthropic","auth_details":{"api_key":"sk-api"}}]`

	key, bearer, err = parseForgeAnthropicCredentials([]byte(data))
	if err != nil {
		require.NoError(t, err)
	}

	if key != "sk-api" || bearer {
		assert.Failf(t, "assertion failed", "got key=%q bearer=%v, want sk-api/false", key, bearer)
	}

	data = `[
		{"id":"anthropic","auth_details":{"api_key":"sk-api"}},
		{"id":"claude_code","auth_details":{"o_auth":{"tokens":{"access_token":"expired","expires_at":"2000-01-01T00:00:00Z"}}}}
	]`

	key, bearer, err = parseForgeAnthropicCredentials([]byte(data))
	if err != nil {
		require.NoError(t, err)
	}

	if key != "sk-api" || bearer {
		assert.Failf(t, "assertion failed", "got key=%q bearer=%v, want expired OAuth to fall back to sk-api/false", key, bearer)
	}

	_, _, err = parseForgeAnthropicCredentials([]byte(`[]`))
	if err == nil {
		require.FailNow(t, "expected error for missing ForgeCode Anthropic credentials")
	}
}

func containsAll(value string, needles ...string) bool {
	for _, needle := range needles {
		if !strings.Contains(value, needle) {
			return false
		}
	}

	return true
}

func TestResolveAnthropicKey_Precedence(t *testing.T) {
	t.Setenv("ANTHROPIC_API_KEY", "api-key")
	t.Setenv("ANTHROPIC_AUTH_TOKEN", "auth-token")
	t.Setenv("CLAUDE_CODE_OAUTH_TOKEN", "oauth-token")
	t.Setenv("FORGE_CONFIG", "")

	key, bearer, err := ResolveAnthropicKeyContext(context.Background())
	if err != nil {
		require.NoError(t, err)
	}
	// API key should take precedence.
	if key != "api-key" || bearer {
		assert.Failf(t, "assertion failed", "got key=%q bearer=%v, want api-key/false", key, bearer)
	}
}

// ---------------------------------------------------------------------------
// OpenAI credential resolution
// ---------------------------------------------------------------------------

func TestResolveOpenAIKey_EnvVar(t *testing.T) {
	t.Setenv("OPENAI_API_KEY", "sk-openai-test")

	key, bearer, err := ResolveOpenAIKeyContext(context.Background())
	if err != nil {
		require.NoError(t, err)
	}

	if key != "sk-openai-test" || bearer {
		assert.Failf(t, "assertion failed", "got key=%q bearer=%v, want sk-openai-test/false", key, bearer)
	}
}

func TestResolveOpenAIKey_CompatibilityHelperRequiresContext(t *testing.T) {
	t.Parallel()

	_, _, err := ResolveOpenAIKey()
	require.Error(t, err)
	require.ErrorIs(t, err, ErrContextRequired)
}

func TestResolveOpenAIKey_CodexAuthJSON_APIKey(t *testing.T) {
	t.Setenv("OPENAI_API_KEY", "")

	dir := t.TempDir()
	t.Setenv("HOME", dir)

	codexDir := filepath.Join(dir, ".codex")
	if err := os.MkdirAll(codexDir, 0o750); err != nil {
		require.NoError(t, err)
	}

	data := `{"auth_mode":"api_key","OPENAI_API_KEY":"sk-from-codex","tokens":{}}`
	if err := os.WriteFile(filepath.Join(codexDir, "auth.json"), []byte(data), 0o600); err != nil {
		require.NoError(t, err)
	}

	key, bearer, err := ResolveOpenAIKeyContext(context.Background())
	if err != nil {
		require.NoError(t, err)
	}

	if key != "sk-from-codex" || bearer {
		assert.Failf(t, "assertion failed", "got key=%q bearer=%v, want sk-from-codex/false", key, bearer)
	}
}

func TestResolveOpenAIKey_CodexAuthJSON_OAuthTokenIsNotPlatformKey(t *testing.T) {
	t.Setenv("OPENAI_API_KEY", "")

	dir := t.TempDir()
	t.Setenv("HOME", dir)

	codexDir := filepath.Join(dir, ".codex")
	if err := os.MkdirAll(codexDir, 0o750); err != nil {
		require.NoError(t, err)
	}

	data := `{"auth_mode":"chatgpt","OPENAI_API_KEY":null,"tokens":{"access_token":"chatgpt-access","refresh_token":"rt"}}`
	if err := os.WriteFile(filepath.Join(codexDir, "auth.json"), []byte(data), 0o600); err != nil {
		require.NoError(t, err)
	}

	_, _, err := ResolveOpenAIKeyContext(context.Background())
	if err == nil {
		require.FailNow(t, "expected Codex ChatGPT OAuth token not to be used as an OpenAI Platform API key")
	}
}

func TestResolveOpenAIKey_NoCreds(t *testing.T) {
	t.Setenv("OPENAI_API_KEY", "")

	dir := t.TempDir()
	t.Setenv("HOME", dir)

	_, _, err := ResolveOpenAIKeyContext(context.Background())
	if err == nil {
		require.FailNow(t, "expected error when no credentials available")
	}
}
