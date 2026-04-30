package llm

import (
	"os"
	"path/filepath"
	"testing"
)

// ---------------------------------------------------------------------------
// Anthropic credential resolution
// ---------------------------------------------------------------------------

func TestResolveAnthropicKey_EnvAPIKey(t *testing.T) {
	t.Setenv("ANTHROPIC_API_KEY", "sk-ant-test")
	t.Setenv("ANTHROPIC_AUTH_TOKEN", "")
	t.Setenv("CLAUDE_CODE_OAUTH_TOKEN", "")

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

func TestResolveAnthropicKey_NoCreds(t *testing.T) {
	t.Setenv("ANTHROPIC_API_KEY", "")
	t.Setenv("ANTHROPIC_AUTH_TOKEN", "")
	t.Setenv("CLAUDE_CODE_OAUTH_TOKEN", "")

	dir := t.TempDir()
	t.Setenv("HOME", dir)

	_, _, err := ResolveAnthropicKey()
	if err == nil {
		t.Fatal("expected error when no credentials set")
	}
}

func TestParseClaudeCodeCredentials(t *testing.T) {
	good := `{"claudeAiOauth":{"accessToken":"tok123","refreshToken":"rt","expiresAt":1}}`
	tok, err := parseClaudeCodeCredentials([]byte(good))
	if err != nil {
		t.Fatal(err)
	}
	if tok != "tok123" {
		t.Errorf("got %q, want tok123", tok)
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

func TestResolveAnthropicKey_Precedence(t *testing.T) {
	t.Setenv("ANTHROPIC_API_KEY", "api-key")
	t.Setenv("ANTHROPIC_AUTH_TOKEN", "auth-token")
	t.Setenv("CLAUDE_CODE_OAUTH_TOKEN", "oauth-token")

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

func TestResolveOpenAIKey_CodexAuthJSON_OAuthToken(t *testing.T) {
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

	key, bearer, err := ResolveOpenAIKey()
	if err != nil {
		t.Fatal(err)
	}
	if key != "chatgpt-access" || !bearer {
		t.Errorf("got key=%q bearer=%v, want chatgpt-access/true", key, bearer)
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
