package llm

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// ---------------------------------------------------------------------------
// Anthropic credential resolution
// ---------------------------------------------------------------------------

func TestResolveAnthropicKey_EnvAPIKey(t *testing.T) {
	t.Setenv("ANTHROPIC_API_KEY", "sk-ant-test")
	t.Setenv("ANTHROPIC_AUTH_TOKEN", "")
	t.Setenv("CLAUDE_CODE_OAUTH_TOKEN", "")
	t.Setenv("FORGE_CONFIG", "")

	key, bearer, err := ResolveAnthropicKey()
	if err != nil {
		t.Fatal(err)
	}
	if key != "sk-ant-test" {
		t.Errorf("expected sk-ant-test, got %q", key)
	}
	if bearer {
		t.Error("expected bearer=false for API key")
	}
}

func TestResolveAnthropicKey_EnvAuthToken(t *testing.T) {
	t.Setenv("ANTHROPIC_API_KEY", "")
	t.Setenv("ANTHROPIC_AUTH_TOKEN", "bearer-tok")
	t.Setenv("CLAUDE_CODE_OAUTH_TOKEN", "")
	t.Setenv("FORGE_CONFIG", "")

	key, bearer, err := ResolveAnthropicKey()
	if err != nil {
		t.Fatal(err)
	}
	if key != "bearer-tok" || !bearer {
		t.Errorf("got key=%q bearer=%v, want bearer-tok/true", key, bearer)
	}
}

func TestResolveAnthropicKey_ClaudeCodeOAuth(t *testing.T) {
	t.Setenv("ANTHROPIC_API_KEY", "")
	t.Setenv("ANTHROPIC_AUTH_TOKEN", "")
	t.Setenv("CLAUDE_CODE_OAUTH_TOKEN", "oauth-tok")
	t.Setenv("FORGE_CONFIG", "")

	key, bearer, err := ResolveAnthropicKey()
	if err != nil {
		t.Fatal(err)
	}
	if key != "oauth-tok" || !bearer {
		t.Errorf("got key=%q bearer=%v, want oauth-tok/true", key, bearer)
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
		t.Fatal(err)
	}

	data := `{"claudeAiOauth":{"accessToken":"sk-from-file","refreshToken":"rt","expiresAt":9999999999999}}`
	if err := os.WriteFile(filepath.Join(claudeDir, ".credentials.json"), []byte(data), 0o600); err != nil {
		t.Fatal(err)
	}

	key, bearer, err := ResolveAnthropicKey()
	if err != nil {
		t.Fatal(err)
	}
	if key != "sk-from-file" || !bearer {
		t.Errorf("got key=%q bearer=%v, want sk-from-file/true", key, bearer)
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
		t.Fatal(err)
	}

	data := `[
		{"id":"forge_services","auth_details":{"api_key":"forge-service-key"}},
		{"id":"claude_code","auth_details":{"o_auth":{"tokens":{"access_token":"forge-oauth","refresh_token":"rt","expires_at":"2099-01-01T00:00:00Z"}}}}
	]`
	if err := os.WriteFile(filepath.Join(forgeDir, ".credentials.json"), []byte(data), 0o600); err != nil {
		t.Fatal(err)
	}

	key, bearer, err := ResolveAnthropicKey()
	if err != nil {
		t.Fatal(err)
	}
	if key != "forge-oauth" || !bearer {
		t.Errorf("got key=%q bearer=%v, want forge-oauth/true", key, bearer)
	}
}

func TestResolveAnthropicKey_NoCreds(t *testing.T) {
	t.Setenv("ANTHROPIC_API_KEY", "")
	t.Setenv("ANTHROPIC_AUTH_TOKEN", "")
	t.Setenv("CLAUDE_CODE_OAUTH_TOKEN", "")
	t.Setenv("FORGE_CONFIG", "")

	dir := t.TempDir()
	t.Setenv("HOME", dir)

	_, _, err := ResolveAnthropicKey()
	if err == nil {
		t.Fatal("expected error when no credentials set")
	}
}

func TestParseClaudeCodeCredentials(t *testing.T) {
	good := `{"claudeAiOauth":{"accessToken":"tok123","refreshToken":"rt","expiresAt":9999999999999}}`
	tok, err := parseClaudeCodeCredentials([]byte(good))
	if err != nil {
		t.Fatal(err)
	}
	if tok != "tok123" {
		t.Errorf("got %q, want tok123", tok)
	}

	expired := `{"claudeAiOauth":{"accessToken":"tok123","refreshToken":"rt","expiresAt":1}}`
	_, err = parseClaudeCodeCredentials([]byte(expired))
	if err == nil {
		t.Fatal("expected error for expired accessToken")
	}

	// Missing token.
	empty := `{"claudeAiOauth":{"accessToken":"","refreshToken":"rt","expiresAt":1}}`
	_, err = parseClaudeCodeCredentials([]byte(empty))
	if err == nil {
		t.Fatal("expected error for empty accessToken")
	}

	// No claudeAiOauth block.
	noBlock := `{}`
	_, err = parseClaudeCodeCredentials([]byte(noBlock))
	if err == nil {
		t.Fatal("expected error for missing block")
	}

	// Invalid JSON.
	_, err = parseClaudeCodeCredentials([]byte(`not json`))
	if err == nil {
		t.Fatal("expected error for bad JSON")
	}
}

func TestReadForgeCredentialsFile_RefreshesExpiredClaudeCodeOAuth(t *testing.T) {
	var got map[string]string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			t.Errorf("method = %s, want POST", r.Method)
		}
		if r.Header.Get("Content-Type") != "application/json" {
			t.Errorf("Content-Type = %q, want application/json", r.Header.Get("Content-Type"))
		}
		if err := json.NewDecoder(r.Body).Decode(&got); err != nil {
			t.Fatalf("decode request: %v", err)
		}
		w.Header().Set("Content-Type", "application/json")
		if err := json.NewEncoder(w).Encode(map[string]any{
			"access_token":  "new-access",
			"refresh_token": "new-refresh",
			"expires_in":    3600,
		}); err != nil {
			t.Fatalf("encode response: %v", err)
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
		t.Fatal(err)
	}

	key, bearer, err := readForgeCredentialsFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if key != "new-access" || !bearer {
		t.Errorf("got key=%q bearer=%v, want new-access/true", key, bearer)
	}
	if got["grant_type"] != forgeOAuthRefreshGrantType ||
		got["refresh_token"] != "old-refresh" ||
		got["client_id"] != "client-123" {
		t.Errorf("refresh request = %#v", got)
	}

	refreshed, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !json.Valid(refreshed) {
		t.Fatalf("refreshed credentials are not valid JSON: %s", refreshed)
	}
	if !containsAll(string(refreshed), "new-access", "new-refresh", "user_id") {
		t.Fatalf("refreshed credentials did not preserve/update expected fields: %s", refreshed)
	}
}

func TestParseForgeAnthropicCredentials(t *testing.T) {
	data := `[
		{"id":"anthropic","auth_details":{"api_key":"sk-api"}},
		{"id":"claude_code","auth_details":{"o_auth":{"tokens":{"access_token":"oauth-token"}}}}
	]`

	key, bearer, err := parseForgeAnthropicCredentials([]byte(data))
	if err != nil {
		t.Fatal(err)
	}
	if key != "oauth-token" || !bearer {
		t.Errorf("got key=%q bearer=%v, want oauth-token/true", key, bearer)
	}

	data = `[{"id":"anthropic","auth_details":{"api_key":"sk-api"}}]`
	key, bearer, err = parseForgeAnthropicCredentials([]byte(data))
	if err != nil {
		t.Fatal(err)
	}
	if key != "sk-api" || bearer {
		t.Errorf("got key=%q bearer=%v, want sk-api/false", key, bearer)
	}

	data = `[
		{"id":"anthropic","auth_details":{"api_key":"sk-api"}},
		{"id":"claude_code","auth_details":{"o_auth":{"tokens":{"access_token":"expired","expires_at":"2000-01-01T00:00:00Z"}}}}
	]`
	key, bearer, err = parseForgeAnthropicCredentials([]byte(data))
	if err != nil {
		t.Fatal(err)
	}
	if key != "sk-api" || bearer {
		t.Errorf("got key=%q bearer=%v, want expired OAuth to fall back to sk-api/false", key, bearer)
	}

	_, _, err = parseForgeAnthropicCredentials([]byte(`[]`))
	if err == nil {
		t.Fatal("expected error for missing ForgeCode Anthropic credentials")
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

	key, bearer, err := ResolveAnthropicKey()
	if err != nil {
		t.Fatal(err)
	}
	// API key should take precedence.
	if key != "api-key" || bearer {
		t.Errorf("got key=%q bearer=%v, want api-key/false", key, bearer)
	}
}

// ---------------------------------------------------------------------------
// OpenAI credential resolution
// ---------------------------------------------------------------------------

func TestResolveOpenAIKey_EnvVar(t *testing.T) {
	t.Setenv("OPENAI_API_KEY", "sk-openai-test")

	key, bearer, err := ResolveOpenAIKey()
	if err != nil {
		t.Fatal(err)
	}
	if key != "sk-openai-test" || bearer {
		t.Errorf("got key=%q bearer=%v, want sk-openai-test/false", key, bearer)
	}
}

func TestResolveOpenAIKey_CodexAuthJSON_APIKey(t *testing.T) {
	t.Setenv("OPENAI_API_KEY", "")

	dir := t.TempDir()
	t.Setenv("HOME", dir)

	codexDir := filepath.Join(dir, ".codex")
	if err := os.MkdirAll(codexDir, 0o750); err != nil {
		t.Fatal(err)
	}

	data := `{"auth_mode":"api_key","OPENAI_API_KEY":"sk-from-codex","tokens":{}}`
	if err := os.WriteFile(filepath.Join(codexDir, "auth.json"), []byte(data), 0o600); err != nil {
		t.Fatal(err)
	}

	key, bearer, err := ResolveOpenAIKey()
	if err != nil {
		t.Fatal(err)
	}
	if key != "sk-from-codex" || bearer {
		t.Errorf("got key=%q bearer=%v, want sk-from-codex/false", key, bearer)
	}
}

func TestResolveOpenAIKey_CodexAuthJSON_OAuthTokenIsNotPlatformKey(t *testing.T) {
	t.Setenv("OPENAI_API_KEY", "")

	dir := t.TempDir()
	t.Setenv("HOME", dir)

	codexDir := filepath.Join(dir, ".codex")
	if err := os.MkdirAll(codexDir, 0o750); err != nil {
		t.Fatal(err)
	}

	data := `{"auth_mode":"chatgpt","OPENAI_API_KEY":null,"tokens":{"access_token":"chatgpt-access","refresh_token":"rt"}}`
	if err := os.WriteFile(filepath.Join(codexDir, "auth.json"), []byte(data), 0o600); err != nil {
		t.Fatal(err)
	}

	_, _, err := ResolveOpenAIKey()
	if err == nil {
		t.Fatal("expected Codex ChatGPT OAuth token not to be used as an OpenAI Platform API key")
	}
}

func TestResolveOpenAIKey_NoCreds(t *testing.T) {
	t.Setenv("OPENAI_API_KEY", "")

	dir := t.TempDir()
	t.Setenv("HOME", dir)

	_, _, err := ResolveOpenAIKey()
	if err == nil {
		t.Fatal("expected error when no credentials available")
	}
}
