package llm

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/tommoulard/atteler/pkg/permission"
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

	key, bearer, err := ResolveAnthropicKeyWithConfigContext(
		context.Background(),
		ProviderConfig{CredentialPolicy: CredentialSourcePolicy{AllowedStores: []string{CredentialStoreEnv}, AllowBorrowedOAuth: true}},
	)
	if err != nil {
		require.NoError(t, err)
	}

	if key != "oauth-tok" || !bearer {
		assert.Failf(t, "assertion failed", "got key=%q bearer=%v, want oauth-tok/true", key, bearer)
	}
}

func TestResolveAnthropicKey_ClaudeCodeOAuthEnvRequiresBorrowedOAuthPolicy(t *testing.T) {
	t.Setenv("ANTHROPIC_API_KEY", "")
	t.Setenv("ANTHROPIC_AUTH_TOKEN", "")
	t.Setenv("CLAUDE_CODE_OAUTH_TOKEN", "oauth-secret")
	t.Setenv("FORGE_CONFIG", "")

	auditDir := t.TempDir()
	ctx := permission.ContextWithAuditDir(context.Background(), auditDir)

	_, _, err := ResolveAnthropicKeyContext(ctx)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "allow_borrowed_oauth")
	assert.NotContains(t, err.Error(), "oauth-secret")

	audit, readErr := os.ReadFile(filepath.Join(auditDir, credentialAuditLedgerFileName))
	require.NoError(t, readErr)
	assert.Contains(t, string(audit), "CLAUDE_CODE_OAUTH_TOKEN")
	assert.Contains(t, string(audit), `"borrowed_oauth":true`)
	assert.NotContains(t, string(audit), "oauth-secret")
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

	key, bearer, err := ResolveAnthropicKeyWithConfigContext(context.Background(), ProviderConfig{CredentialPolicy: CredentialSourcePolicy{AllowedStores: []string{CredentialStoreClaudeCodeFile}, AllowBorrowedOAuth: true}})
	require.NoError(t, err)

	if key != "sk-from-file" || !bearer {
		assert.Failf(t, "assertion failed", "got key=%q bearer=%v, want sk-from-file/true", key, bearer)
	}
}

func TestResolveAnthropicKey_ClaudeCodeFileAllowedProviderUsesResolvedProvider(t *testing.T) {
	t.Setenv("ANTHROPIC_API_KEY", "")
	t.Setenv("ANTHROPIC_AUTH_TOKEN", "")
	t.Setenv("CLAUDE_CODE_OAUTH_TOKEN", "")
	t.Setenv("FORGE_CONFIG", "")

	dir := t.TempDir()
	t.Setenv("HOME", dir)

	claudeDir := filepath.Join(dir, ".claude")
	require.NoError(t, os.MkdirAll(claudeDir, 0o750))

	data := `{"claudeAiOauth":{"accessToken":"sk-from-allowed-provider-file","refreshToken":"rt","expiresAt":9999999999999}}`
	require.NoError(t, os.WriteFile(filepath.Join(claudeDir, ".credentials.json"), []byte(data), 0o600))

	key, bearer, err := ResolveAnthropicKeyWithConfigContext(context.Background(), ProviderConfig{
		CredentialPolicy: CredentialSourcePolicy{
			AllowedProviders:   []string{providerAnthropic},
			AllowedStores:      []string{CredentialStoreClaudeCodeFile},
			AllowBorrowedOAuth: true,
		},
	})
	require.NoError(t, err)
	assert.Equal(t, "sk-from-allowed-provider-file", key)
	assert.True(t, bearer)

	_, _, err = ResolveAnthropicKeyWithConfigContext(context.Background(), ProviderConfig{
		CredentialPolicy: CredentialSourcePolicy{
			AllowedProviders:   []string{providerOpenAI},
			AllowedStores:      []string{CredentialStoreClaudeCodeFile},
			AllowBorrowedOAuth: true,
		},
	})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "allowed_providers")
	assert.NotContains(t, err.Error(), "sk-from-allowed-provider-file")
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

	key, bearer, err := ResolveAnthropicKeyWithConfigContext(context.Background(), ProviderConfig{CredentialPolicy: CredentialSourcePolicy{AllowedStores: []string{CredentialStoreForgeCredentials}, AllowBorrowedOAuth: true}})
	require.NoError(t, err)

	if key != "forge-oauth" || !bearer {
		assert.Failf(t, "assertion failed", "got key=%q bearer=%v, want forge-oauth/true", key, bearer)
	}
}

func TestResolveAnthropicKey_ForgeClaudeCodeRequiresBorrowedOAuthPolicy(t *testing.T) {
	t.Setenv("ANTHROPIC_API_KEY", "")
	t.Setenv("ANTHROPIC_AUTH_TOKEN", "")
	t.Setenv("CLAUDE_CODE_OAUTH_TOKEN", "")

	dir := t.TempDir()
	t.Setenv("HOME", dir)

	forgeDir := filepath.Join(dir, "forge")
	require.NoError(t, os.MkdirAll(forgeDir, 0o750))

	data := `[
		{"id":"claude_code","auth_details":{"o_auth":{"tokens":{"access_token":"forge-oauth","refresh_token":"rt","expires_at":"2099-01-01T00:00:00Z"}}}}
	]`
	require.NoError(t, os.WriteFile(filepath.Join(forgeDir, ".credentials.json"), []byte(data), 0o600))

	_, _, err := ResolveAnthropicKeyWithConfigContext(context.Background(), ProviderConfig{
		CredentialPolicy: CredentialSourcePolicy{AllowedStores: []string{CredentialStoreForgeCredentials}},
	})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "allow_borrowed_oauth")
	assert.NotContains(t, err.Error(), "forge-oauth")
}

func TestReadForgeCredentialsFile_BorrowedOAuthDenialFallsBackToAnthropicAPIKey(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), ".credentials.json")
	data := `[
		{"id":"claude_code","auth_details":{"o_auth":{"tokens":{"access_token":"forge-oauth","refresh_token":"rt","expires_at":"2099-01-01T00:00:00Z"}}}},
		{"id":"anthropic","auth_details":{"api_key":"sk-api"}}
	]`
	require.NoError(t, os.WriteFile(path, []byte(data), 0o600))

	key, bearer, err := readForgeCredentialsFile(context.Background(), ProviderConfig{
		CredentialPolicy: CredentialSourcePolicy{AllowedStores: []string{CredentialStoreForgeCredentials}},
	}, path)
	require.NoError(t, err)
	assert.Equal(t, "sk-api", key)
	assert.False(t, bearer)
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
		_, _, err := ResolveAnthropicKeyWithConfigContext(ctx, ProviderConfig{CredentialPolicy: permissiveCredentialSourcePolicy()})
		errCh <- err
	}()

	<-requestStarted
	cancel()

	err := <-errCh
	require.Error(t, err)
	assert.ErrorIs(t, err, context.Canceled)
}

func TestResolveAnthropicKeyWithConfig_APIKeyWorksWhenBorrowedCredentialsDisabled(t *testing.T) {
	t.Setenv("ANTHROPIC_API_KEY", "api-key")
	t.Setenv("ANTHROPIC_AUTH_TOKEN", "")
	t.Setenv("CLAUDE_CODE_OAUTH_TOKEN", "oauth-token")
	t.Setenv("FORGE_CONFIG", "")
	t.Setenv("ATTELER_DISABLE_PRIVATE_ADAPTERS", "1")

	key, bearer, err := ResolveAnthropicKeyWithConfigContext(context.Background(), ProviderConfig{})
	require.NoError(t, err)

	assert.Equal(t, "api-key", key)
	assert.False(t, bearer)
}

func TestResolveOpenAIKeyContext_PermissionPolicyDeniesCredentialAccess(t *testing.T) {
	t.Setenv("OPENAI_API_KEY", "sk-test")

	policy := permission.DefaultPolicy()
	policy.SetMode(permission.OperationCredentialAccess, permission.ModeDeny)
	ctx := permission.ContextWithPolicy(context.Background(), &policy)

	_, _, err := ResolveOpenAIKeyContext(ctx)
	require.Error(t, err)
	require.True(t, permission.ErrDenied(err))
	assert.Contains(t, err.Error(), "permission.credential_access.deny")
}

func TestResolveOpenAIKeyContext_PermissionPolicyDeniesAuthFileRead(t *testing.T) {
	t.Setenv("OPENAI_API_KEY", "")

	root := t.TempDir()
	t.Setenv("HOME", root)

	codexDir := filepath.Join(root, ".codex")
	require.NoError(t, os.MkdirAll(codexDir, 0o750))
	require.NoError(t, os.WriteFile(filepath.Join(codexDir, "auth.json"), []byte(`{"OPENAI_API_KEY":"sk-file"}`), 0o600))

	policy := permission.DefaultPolicy()
	policy.SetMode(permission.OperationRead, permission.ModeDeny)

	auditDir := filepath.Join(root, "audit")
	ctx := permission.ContextWithPolicy(context.Background(), &policy)
	ctx = permission.ContextWithAuditDir(ctx, auditDir)
	ctx = ContextWithCredentialSourcePolicy(ctx, CredentialSourcePolicy{AllowedStores: []string{CredentialStoreCodexAuthJSON}})

	_, _, err := ResolveOpenAIKeyContext(ctx)
	require.Error(t, err)
	require.True(t, permission.ErrDenied(err))
	assert.Contains(t, err.Error(), "permission.read.deny")

	auditData, readErr := os.ReadFile(filepath.Join(auditDir, "side_effects.jsonl"))
	require.NoError(t, readErr)
	assert.Contains(t, string(auditData), "read OpenAI auth.json")
	assert.Contains(t, string(auditData), "permission.read.deny")
}

func TestResolveAnthropicKeyContext_PermissionPolicyDeniesForgeCredentialFileRead(t *testing.T) {
	t.Setenv("ANTHROPIC_API_KEY", "")
	t.Setenv("ANTHROPIC_AUTH_TOKEN", "")
	t.Setenv("CLAUDE_CODE_OAUTH_TOKEN", "")

	root := t.TempDir()
	t.Setenv("HOME", filepath.Join(root, "home"))

	forgeDir := filepath.Join(root, "forge")
	require.NoError(t, os.MkdirAll(forgeDir, 0o750))
	require.NoError(t, os.WriteFile(filepath.Join(forgeDir, ".credentials.json"), []byte(`[
		{"id":"anthropic","auth_details":{"api_key":"sk-forge"}}
	]`), 0o600))
	t.Setenv("FORGE_CONFIG", forgeDir)

	policy := permission.DefaultPolicy()
	policy.SetMode(permission.OperationRead, permission.ModeDeny)

	auditDir := filepath.Join(root, "audit")
	ctx := permission.ContextWithPolicy(context.Background(), &policy)
	ctx = permission.ContextWithAuditDir(ctx, auditDir)
	ctx = ContextWithCredentialSourcePolicy(ctx, CredentialSourcePolicy{AllowedStores: []string{CredentialStoreForgeCredentials}})

	_, _, err := ResolveAnthropicKeyContext(ctx)
	require.Error(t, err)
	require.True(t, permission.ErrDenied(err))
	assert.Contains(t, err.Error(), "permission.read.deny")
	assert.NotContains(t, err.Error(), "no Anthropic credentials found")

	auditData, readErr := os.ReadFile(filepath.Join(auditDir, "side_effects.jsonl"))
	require.NoError(t, readErr)
	assert.Contains(t, string(auditData), "read ForgeCode credentials file")
	assert.Contains(t, string(auditData), "permission.read.deny")
}

func TestLoadClaudeCodeAuthPermissionPolicyDeniesCredentialFileRead(t *testing.T) {
	t.Setenv("ATTELER_CLAUDE_CODE_SKIP_KEYCHAIN", "1")

	root := t.TempDir()
	t.Setenv("HOME", root)

	claudeDir := filepath.Join(root, ".claude")
	require.NoError(t, os.MkdirAll(claudeDir, 0o750))
	require.NoError(t, os.WriteFile(filepath.Join(claudeDir, ".credentials.json"), []byte(
		`{"claudeAiOauth":{"accessToken":"access","refreshToken":"refresh","expiresAt":9999999999999}}`,
	), 0o600))

	policy := permission.DefaultPolicy()
	policy.SetMode(permission.OperationRead, permission.ModeDeny)

	auditDir := filepath.Join(root, "audit")
	ctx := permission.ContextWithPolicy(context.Background(), &policy)
	ctx = permission.ContextWithAuditDir(ctx, auditDir)
	ctx = ContextWithCredentialSourcePolicy(ctx, CredentialSourcePolicy{AllowedStores: []string{CredentialStoreClaudeCodeFile}, AllowBorrowedOAuth: true})

	auth, err := loadClaudeCodeAuth(ctx)
	require.Error(t, err)
	require.Nil(t, auth)
	require.True(t, permission.ErrDenied(err))
	assert.Contains(t, err.Error(), "permission.read.deny")

	auditData, readErr := os.ReadFile(filepath.Join(auditDir, "side_effects.jsonl"))
	require.NoError(t, readErr)
	assert.Contains(t, string(auditData), "read Claude Code credentials file")
	assert.Contains(t, string(auditData), "permission.read.deny")
}

func TestResolveAnthropicKeyWithConfig_AuthTokenWorksWhenBorrowedCredentialsDisabled(t *testing.T) {
	t.Setenv("ANTHROPIC_API_KEY", "")
	t.Setenv("ANTHROPIC_AUTH_TOKEN", "auth-token")
	t.Setenv("CLAUDE_CODE_OAUTH_TOKEN", "oauth-token")
	t.Setenv("FORGE_CONFIG", "")

	key, bearer, err := ResolveAnthropicKeyWithConfigContext(
		context.Background(),
		ProviderConfig{DisablePrivateAdapter: true},
	)
	require.NoError(t, err)

	assert.Equal(t, "auth-token", key)
	assert.True(t, bearer)
}

func TestResolveAnthropicKeyWithConfig_SkipsClaudeCodeOAuthWhenBorrowedCredentialsDisabled(t *testing.T) {
	t.Setenv("ANTHROPIC_API_KEY", "")
	t.Setenv("ANTHROPIC_AUTH_TOKEN", "")
	t.Setenv("CLAUDE_CODE_OAUTH_TOKEN", "oauth-token")
	t.Setenv("FORGE_CONFIG", "")

	_, _, err := ResolveAnthropicKeyWithConfigContext(
		context.Background(),
		ProviderConfig{DisablePrivateAdapter: true},
	)
	require.Error(t, err)

	assert.Contains(t, err.Error(), "borrowed Claude Code/Forge credential stores are disabled")
}

func TestResolveAnthropicKey_GlobalKillSwitchSkipsClaudeCodeOAuth(t *testing.T) {
	t.Setenv("ANTHROPIC_API_KEY", "")
	t.Setenv("ANTHROPIC_AUTH_TOKEN", "")
	t.Setenv("CLAUDE_CODE_OAUTH_TOKEN", "oauth-token")
	t.Setenv("FORGE_CONFIG", "")
	t.Setenv("ATTELER_DISABLE_PRIVATE_ADAPTERS", "1")

	_, _, err := ResolveAnthropicKeyContext(context.Background())
	require.Error(t, err)

	assert.Contains(t, err.Error(), "borrowed Claude Code/Forge credential stores are disabled")
}

func TestResolveAnthropicKey_ClaudeCodeKillSwitchSkipsBorrowedOAuth(t *testing.T) {
	t.Setenv("ANTHROPIC_API_KEY", "")
	t.Setenv("ANTHROPIC_AUTH_TOKEN", "")
	t.Setenv("CLAUDE_CODE_OAUTH_TOKEN", "oauth-token")
	t.Setenv("FORGE_CONFIG", "")
	t.Setenv("ATTELER_DISABLE_CLAUDE_CODE_ADAPTER", "1")

	_, _, err := ResolveAnthropicKeyContext(context.Background())
	require.Error(t, err)

	assert.Contains(t, err.Error(), "borrowed Claude Code/Forge credential stores are disabled")
}

func TestResolveAnthropicKeyWithConfig_SkipsCredentialFilesWhenBorrowedCredentialsDisabled(t *testing.T) {
	t.Setenv("ANTHROPIC_API_KEY", "")
	t.Setenv("ANTHROPIC_AUTH_TOKEN", "")
	t.Setenv("CLAUDE_CODE_OAUTH_TOKEN", "")
	t.Setenv("FORGE_CONFIG", "")
	t.Setenv("ATTELER_DISABLE_BORROWED_CREDENTIAL_ADAPTERS", "1")

	dir := t.TempDir()
	t.Setenv("HOME", dir)

	claudeDir := filepath.Join(dir, ".claude")
	require.NoError(t, os.MkdirAll(claudeDir, 0o750))

	claudeData := `{"claudeAiOauth":{"accessToken":"sk-from-file","refreshToken":"rt","expiresAt":9999999999999}}`
	require.NoError(t, os.WriteFile(filepath.Join(claudeDir, ".credentials.json"), []byte(claudeData), 0o600))

	forgeDir := filepath.Join(dir, "forge")
	require.NoError(t, os.MkdirAll(forgeDir, 0o750))

	forgeData := `[{"id":"anthropic","auth_details":{"api_key":"forge-service-key"}}]`
	require.NoError(t, os.WriteFile(filepath.Join(forgeDir, ".credentials.json"), []byte(forgeData), 0o600))

	_, _, err := ResolveAnthropicKeyWithConfigContext(
		context.Background(),
		ProviderConfig{},
	)
	require.Error(t, err)

	assert.Contains(t, err.Error(), "borrowed Claude Code/Forge credential stores are disabled")
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
		}},"url_params":{"user_id":"forge-user-secret"}}
	]`
	if err := os.WriteFile(path, []byte(data), 0o600); err != nil {
		require.NoError(t, err)
	}

	auditDir := t.TempDir()
	ctx := permission.ContextWithAuditDir(context.Background(), auditDir)

	key, bearer, err := readForgeCredentialsFile(ctx, ProviderConfig{CredentialPolicy: permissiveCredentialSourcePolicy()}, path)
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

	auditData, readErr := os.ReadFile(filepath.Join(auditDir, credentialAuditLedgerFileName))
	require.NoError(t, readErr)

	audit := string(auditData)
	assert.Contains(t, audit, credentialAuditEventRefresh)
	assert.Contains(t, audit, credentialAuditEventWriteBack)
	assert.Contains(t, audit, "sha256:")
	assert.NotContains(t, audit, "forge-user-secret")
	assert.NotContains(t, audit, "old-refresh")
	assert.NotContains(t, audit, "new-access")
	assert.NotContains(t, audit, "new-refresh")
}

func TestReadForgeCredentialsFile_ConcurrentRefreshUsesCASWinner(t *testing.T) {
	t.Parallel()

	var (
		refreshHits      atomic.Int32
		closeBothStarted sync.Once
		closeRelease     sync.Once
	)

	bothStarted := make(chan struct{})
	release := make(chan struct{})

	defer closeRelease.Do(func() { close(release) })

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hit := refreshHits.Add(1)
		if hit == 2 {
			closeBothStarted.Do(func() { close(bothStarted) })
		}

		var req map[string]string
		if !assert.NoError(t, json.NewDecoder(r.Body).Decode(&req)) {
			return
		}

		assert.Equal(t, "old-refresh", req["refresh_token"])

		<-release

		w.Header().Set("Content-Type", "application/json")
		assert.NoError(t, json.NewEncoder(w).Encode(map[string]any{
			"access_token":  fmt.Sprintf("access-%d", hit),
			"refresh_token": fmt.Sprintf("refresh-%d", hit),
			"expires_in":    3600,
		}))
	}))
	defer srv.Close()

	path := filepath.Join(t.TempDir(), ".credentials.json")
	data := `[
		{"id":"claude_code","auth_details":{"o_auth":{
			"config":{"token_url":"` + srv.URL + `","client_id":"client-123"},
			"tokens":{"access_token":"expired","refresh_token":"old-refresh","expires_at":"2000-01-01T00:00:00Z"}
		}}}
	]`
	require.NoError(t, os.WriteFile(path, []byte(data), 0o600))

	auditDir := t.TempDir()
	ctx := permission.ContextWithAuditDir(context.Background(), auditDir)
	cfg := ProviderConfig{CredentialPolicy: permissiveCredentialSourcePolicy()}

	type result struct {
		err    error
		key    string
		bearer bool
	}

	results := make(chan result, 2)
	readCredentials := func() {
		key, bearer, err := readForgeCredentialsFile(ctx, cfg, path)
		results <- result{key: key, bearer: bearer, err: err}
	}

	go readCredentials()
	go readCredentials()

	select {
	case <-bothStarted:
	case <-time.After(5 * time.Second):
		require.FailNow(t, "timed out waiting for concurrent Forge refresh requests")
	}

	closeRelease.Do(func() { close(release) })

	got := []result{<-results, <-results}
	for _, result := range got {
		require.NoError(t, result.err)
		assert.True(t, result.bearer)
	}

	persisted, err := os.ReadFile(path)
	require.NoError(t, err)

	entries, err := parseForgeCredentialEntries(persisted)
	require.NoError(t, err)

	finalToken := forgeCredentialForProvider(entries, forgeClaudeCodeProviderID)
	require.Contains(t, []string{"access-1", "access-2"}, finalToken)
	assert.Equal(t, finalToken, got[0].key)
	assert.Equal(t, finalToken, got[1].key)
	assert.NotContains(t, string(persisted), "expired")

	auditData, readErr := os.ReadFile(filepath.Join(auditDir, credentialAuditLedgerFileName))
	require.NoError(t, readErr)

	audit := string(auditData)
	assert.Contains(t, audit, credentialAuditEventCAS)
	assert.Contains(t, audit, credentialAuditEventWriteBack)
	assert.NotContains(t, audit, "old-refresh")
	assert.NotContains(t, audit, "access-1")
	assert.NotContains(t, audit, "access-2")
	assert.NotContains(t, audit, "refresh-1")
	assert.NotContains(t, audit, "refresh-2")
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

	_, _, err := readForgeCredentialsFile(ctx, ProviderConfig{CredentialPolicy: permissiveCredentialSourcePolicy()}, path)
	if err == nil {
		require.FailNow(t, "expected canceled context to stop OAuth refresh")
	}

	assert.Contains(t, err.Error(), "context canceled")
}

func TestReadForgeCredentialsFile_WriteBackPolicyDeniesRefreshBeforeNetwork(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), ".credentials.json")
	data := `[
		{"id":"claude_code","auth_details":{"o_auth":{
			"config":{"token_url":"http://127.0.0.1:1/token","client_id":"client-123"},
			"tokens":{"access_token":"expired","refresh_token":"old-refresh","expires_at":"2000-01-01T00:00:00Z"}
		}}}
	]`
	require.NoError(t, os.WriteFile(path, []byte(data), 0o600))

	auditDir := t.TempDir()
	ctx := permission.ContextWithAuditDir(context.Background(), auditDir)

	_, _, err := readForgeCredentialsFile(ctx, ProviderConfig{
		CredentialPolicy: CredentialSourcePolicy{
			AllowedStores:      []string{CredentialStoreForgeCredentials},
			AllowBorrowedOAuth: true,
			AllowRefresh:       true,
			AllowWriteBack:     false,
		},
	}, path)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "allow_write_back")
	assert.NotContains(t, err.Error(), "old-refresh")

	auditData, readErr := os.ReadFile(filepath.Join(auditDir, credentialAuditLedgerFileName))
	require.NoError(t, readErr)

	audit := string(auditData)
	assert.Contains(t, audit, credentialAuditEventWriteBack)
	assert.Contains(t, audit, `"decision":"failed"`)
	assert.Contains(t, audit, "allow_write_back")
	assert.NotContains(t, audit, "old-refresh")
}

func TestRefreshForgeOAuthToken_HTTPErrorIsTyped(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Retry-After", "3")
		w.Header().Set("x-request-id", "req-forge")
		w.WriteHeader(http.StatusServiceUnavailable)
		_, err := w.Write([]byte(`{"error":{"type":"temporarily_unavailable","message":"try later"}}`))
		assert.NoError(t, err)
	}))
	defer srv.Close()

	_, err := refreshForgeOAuthToken(
		context.Background(),
		forgeOAuthConfig{
			TokenURL: srv.URL,
			ClientID: "client-123",
		},
		"refresh-token",
	)
	require.Error(t, err)

	var providerErr *ProviderError
	require.ErrorAs(t, err, &providerErr)
	assert.Equal(t, providerAnthropic, providerErr.Provider)
	assert.Equal(t, http.StatusServiceUnavailable, providerErr.StatusCode)
	assert.Equal(t, "req-forge", providerErr.RequestID)
	assert.Equal(t, RetryabilityRetryable, providerErr.Retryability)
	assert.Contains(t, providerErr.Message, "temporarily_unavailable")
}

func TestReadForgeCredentialsFile_RefreshFailureIsAudited(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("x-request-id", "req-forge-refresh")
		w.WriteHeader(http.StatusBadGateway)
		_, err := w.Write([]byte(`{"error":{"message":"upstream token endpoint failed"}}`))
		assert.NoError(t, err)
	}))
	defer srv.Close()

	path := filepath.Join(t.TempDir(), ".credentials.json")
	data := `[
		{"id":"claude_code","auth_details":{"o_auth":{
			"config":{"token_url":"` + srv.URL + `","client_id":"client-123"},
			"tokens":{"access_token":"expired","refresh_token":"old-refresh-secret","expires_at":"2000-01-01T00:00:00Z"}
		}},"url_params":{"user_id":"forge-user-secret"}}
	]`
	require.NoError(t, os.WriteFile(path, []byte(data), 0o600))

	auditDir := t.TempDir()
	ctx := permission.ContextWithAuditDir(context.Background(), auditDir)

	_, _, err := readForgeCredentialsFile(ctx, ProviderConfig{CredentialPolicy: permissiveCredentialSourcePolicy()}, path)
	require.Error(t, err)

	auditData, readErr := os.ReadFile(filepath.Join(auditDir, credentialAuditLedgerFileName))
	require.NoError(t, readErr)

	audit := string(auditData)
	assert.Contains(t, audit, credentialAuditEventRefresh)
	assert.Contains(t, audit, `"decision":"failed"`)
	assert.Contains(t, audit, "upstream token [REDACTED] failed")
	assert.Contains(t, audit, "sha256:")
	assert.NotContains(t, audit, "old-refresh-secret")
	assert.NotContains(t, audit, "forge-user-secret")
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
		_, _, err := readForgeCredentialsFile(ctx, ProviderConfig{CredentialPolicy: permissiveCredentialSourcePolicy()}, path)
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

	ctx := ContextWithCredentialSourcePolicy(context.Background(), CredentialSourcePolicy{AllowedStores: []string{CredentialStoreCodexAuthJSON}})
	key, bearer, err := ResolveOpenAIKeyContext(ctx)
	require.NoError(t, err)

	if key != "sk-from-codex" || bearer {
		assert.Failf(t, "assertion failed", "got key=%q bearer=%v, want sk-from-codex/false", key, bearer)
	}
}

func TestResolveOpenAIKey_CredentialSourcePolicyDeniesCodexAuthJSON(t *testing.T) {
	t.Setenv("OPENAI_API_KEY", "")

	dir := t.TempDir()
	t.Setenv("HOME", dir)

	codexDir := filepath.Join(dir, ".codex")
	require.NoError(t, os.MkdirAll(codexDir, 0o750))
	require.NoError(t, os.WriteFile(filepath.Join(codexDir, "auth.json"), []byte(`{"OPENAI_API_KEY":"sk-from-codex"}`), 0o600))

	_, _, err := ResolveOpenAIKeyContext(context.Background())
	require.Error(t, err)
	assert.Contains(t, err.Error(), "credential source policy denied")
	assert.Contains(t, err.Error(), CredentialStoreCodexAuthJSON)
	assert.NotContains(t, err.Error(), "sk-from-codex")
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

	ctx := ContextWithCredentialSourcePolicy(context.Background(), CredentialSourcePolicy{AllowedStores: []string{CredentialStoreCodexAuthJSON}})

	_, _, err := ResolveOpenAIKeyContext(ctx)
	if err == nil {
		require.FailNow(t, "expected Codex ChatGPT OAuth token not to be used as an OpenAI Platform API key")
	}
}

func TestCodexChatGPTAuthRefresh_HTTPErrorIsTyped(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("x-request-id", "req-codex-refresh")
		w.WriteHeader(http.StatusTooManyRequests)
		_, err := w.Write([]byte(`{"error":"rate limited"}`))
		assert.NoError(t, err)
	}))
	defer srv.Close()

	auth := &codexChatGPTAuth{
		authPath:         filepath.Join(t.TempDir(), "auth.json"),
		httpClient:       srv.Client(),
		refreshURL:       srv.URL,
		accessToken:      "old-access",
		refreshToken:     "refresh-token-secret",
		credentialPolicy: permissiveCredentialSourcePolicy(),
	}

	auditDir := t.TempDir()
	ctx := permission.ContextWithAuditDir(context.Background(), auditDir)

	err := auth.refresh(ctx, "old-access")
	require.Error(t, err)

	var providerErr *ProviderError
	require.ErrorAs(t, err, &providerErr)
	assert.Equal(t, providerCodex, providerErr.Provider)
	assert.Equal(t, http.StatusTooManyRequests, providerErr.StatusCode)
	assert.Equal(t, "req-codex-refresh", providerErr.RequestID)
	assert.Equal(t, RetryabilityRetryable, providerErr.Retryability)
	assert.Equal(t, "rate limited", providerErr.Message)

	auditData, readErr := os.ReadFile(filepath.Join(auditDir, credentialAuditLedgerFileName))
	require.NoError(t, readErr)

	audit := string(auditData)
	assert.Contains(t, audit, credentialAuditEventRefresh)
	assert.Contains(t, audit, `"decision":"failed"`)
	assert.Contains(t, audit, "rate limited")
	assert.NotContains(t, audit, "refresh-token-secret")
}

func TestCodexChatGPTAuthRefresh_CredentialPolicyDeniesRefreshBeforeNetwork(t *testing.T) {
	t.Parallel()

	auth := &codexChatGPTAuth{
		authPath:     filepath.Join(t.TempDir(), "auth.json"),
		httpClient:   http.DefaultClient,
		refreshURL:   "http://127.0.0.1:1/token",
		accessToken:  "old-access",
		refreshToken: "refresh-token",
		credentialPolicy: CredentialSourcePolicy{
			AllowedStores:      []string{CredentialStoreCodexAuthJSON},
			AllowBorrowedOAuth: true,
			AllowRefresh:       false,
			AllowWriteBack:     true,
		},
	}

	auditDir := t.TempDir()
	ctx := permission.ContextWithAuditDir(context.Background(), auditDir)

	err := auth.refresh(ctx, "old-access")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "allow_refresh")
	assert.NotContains(t, err.Error(), "refresh-token")

	auditData, readErr := os.ReadFile(filepath.Join(auditDir, credentialAuditLedgerFileName))
	require.NoError(t, readErr)

	audit := string(auditData)
	assert.Contains(t, audit, credentialAuditEventRefresh)
	assert.Contains(t, audit, `"decision":"failed"`)
	assert.Contains(t, audit, "allow_refresh")
	assert.NotContains(t, audit, "refresh-token")
}

func TestCodexChatGPTAuthRefresh_CredentialPolicyDeniesWriteBackBeforeNetwork(t *testing.T) {
	t.Parallel()

	auth := &codexChatGPTAuth{
		authPath:     filepath.Join(t.TempDir(), "auth.json"),
		httpClient:   http.DefaultClient,
		refreshURL:   "http://127.0.0.1:1/token",
		accessToken:  "old-access",
		refreshToken: "refresh-token",
		credentialPolicy: CredentialSourcePolicy{
			AllowedStores:      []string{CredentialStoreCodexAuthJSON},
			AllowBorrowedOAuth: true,
			AllowRefresh:       true,
			AllowWriteBack:     false,
		},
	}

	auditDir := t.TempDir()
	ctx := permission.ContextWithAuditDir(context.Background(), auditDir)

	err := auth.refresh(ctx, "old-access")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "allow_write_back")
	assert.NotContains(t, err.Error(), "refresh-token")

	auditData, readErr := os.ReadFile(filepath.Join(auditDir, credentialAuditLedgerFileName))
	require.NoError(t, readErr)

	audit := string(auditData)
	assert.Contains(t, audit, credentialAuditEventWriteBack)
	assert.Contains(t, audit, `"decision":"failed"`)
	assert.Contains(t, audit, "allow_write_back")
	assert.NotContains(t, audit, "refresh-token")
}

func TestCodexChatGPTAuthRefresh_PermissionDenialIsAudited(t *testing.T) {
	t.Parallel()

	auth := &codexChatGPTAuth{
		authPath:         filepath.Join(t.TempDir(), "auth.json"),
		httpClient:       http.DefaultClient,
		refreshURL:       "http://127.0.0.1:1/token",
		accessToken:      "old-access",
		refreshToken:     "refresh-token-secret",
		credentialPolicy: permissiveCredentialSourcePolicy(),
	}

	policy := permission.DefaultPolicy()
	policy.SetMode(permission.OperationNetwork, permission.ModeDeny)

	auditDir := t.TempDir()
	ctx := permission.ContextWithPolicy(context.Background(), &policy)
	ctx = permission.ContextWithAuditDir(ctx, auditDir)

	err := auth.refresh(ctx, "old-access")
	require.Error(t, err)
	require.True(t, permission.ErrDenied(err))

	auditData, readErr := os.ReadFile(filepath.Join(auditDir, credentialAuditLedgerFileName))
	require.NoError(t, readErr)

	audit := string(auditData)
	assert.Contains(t, audit, credentialAuditEventRefresh)
	assert.Contains(t, audit, `"decision":"failed"`)
	assert.Contains(t, audit, "permission.network.deny")
	assert.NotContains(t, audit, "refresh-token-secret")
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

func TestClaudeCodeAuthRefresh_HTTPErrorIsTyped(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("request-id", "req-claude-refresh")
		w.WriteHeader(http.StatusUnauthorized)
		_, err := w.Write([]byte(`{"error":{"type":"invalid_grant","message":"expired refresh"}}`))
		assert.NoError(t, err)
	}))
	defer srv.Close()

	auth := &claudeCodeAuth{
		httpClient:       srv.Client(),
		refreshURL:       srv.URL,
		accessToken:      "old-access",
		refreshToken:     "refresh-token-secret",
		credentialPolicy: permissiveCredentialSourcePolicy(),
	}

	auditDir := t.TempDir()
	ctx := permission.ContextWithAuditDir(context.Background(), auditDir)

	err := auth.refresh(ctx, "old-access")
	require.Error(t, err)

	var providerErr *ProviderError
	require.ErrorAs(t, err, &providerErr)
	assert.Equal(t, providerClaudeCode, providerErr.Provider)
	assert.Equal(t, http.StatusUnauthorized, providerErr.StatusCode)
	assert.Equal(t, "req-claude-refresh", providerErr.RequestID)
	assert.Equal(t, RetryabilityNonRetryable, providerErr.Retryability)
	assert.Contains(t, providerErr.Message, "invalid_grant")

	auditData, readErr := os.ReadFile(filepath.Join(auditDir, credentialAuditLedgerFileName))
	require.NoError(t, readErr)

	audit := string(auditData)
	assert.Contains(t, audit, credentialAuditEventRefresh)
	assert.Contains(t, audit, `"decision":"failed"`)
	assert.Contains(t, audit, "invalid_grant")
	assert.NotContains(t, audit, "refresh-token-secret")
}

func TestClaudeCodeAuthRefresh_CredentialPolicyDeniesRefreshBeforeNetwork(t *testing.T) {
	t.Parallel()

	auth := &claudeCodeAuth{
		httpClient:   http.DefaultClient,
		refreshURL:   "http://127.0.0.1:1/token",
		accessToken:  "old-access",
		refreshToken: "refresh-token",
		credentialPolicy: CredentialSourcePolicy{
			AllowedStores:      []string{CredentialStoreClaudeCodeFile},
			AllowBorrowedOAuth: true,
			AllowRefresh:       false,
			AllowWriteBack:     true,
		},
	}

	auditDir := t.TempDir()
	ctx := permission.ContextWithAuditDir(context.Background(), auditDir)

	err := auth.refresh(ctx, "old-access")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "allow_refresh")
	assert.NotContains(t, err.Error(), "refresh-token")

	auditData, readErr := os.ReadFile(filepath.Join(auditDir, credentialAuditLedgerFileName))
	require.NoError(t, readErr)

	audit := string(auditData)
	assert.Contains(t, audit, credentialAuditEventRefresh)
	assert.Contains(t, audit, `"decision":"failed"`)
	assert.Contains(t, audit, "allow_refresh")
	assert.NotContains(t, audit, "refresh-token")
}

func TestClaudeCodeAuthRefresh_CredentialPolicyDeniesWriteBackBeforeNetwork(t *testing.T) {
	t.Parallel()

	auth := &claudeCodeAuth{
		httpClient:   http.DefaultClient,
		refreshURL:   "http://127.0.0.1:1/token",
		accessToken:  "old-access",
		refreshToken: "refresh-token",
		credentialPolicy: CredentialSourcePolicy{
			AllowedStores:      []string{CredentialStoreClaudeCodeFile},
			AllowBorrowedOAuth: true,
			AllowRefresh:       true,
			AllowWriteBack:     false,
		},
	}

	auditDir := t.TempDir()
	ctx := permission.ContextWithAuditDir(context.Background(), auditDir)

	err := auth.refresh(ctx, "old-access")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "allow_write_back")
	assert.NotContains(t, err.Error(), "refresh-token")

	auditData, readErr := os.ReadFile(filepath.Join(auditDir, credentialAuditLedgerFileName))
	require.NoError(t, readErr)

	audit := string(auditData)
	assert.Contains(t, audit, credentialAuditEventWriteBack)
	assert.Contains(t, audit, `"decision":"failed"`)
	assert.Contains(t, audit, "allow_write_back")
	assert.NotContains(t, audit, "refresh-token")
}

func TestClaudeCodeAuthRefresh_PermissionDenialIsAudited(t *testing.T) {
	t.Parallel()

	auth := &claudeCodeAuth{
		httpClient:       http.DefaultClient,
		refreshURL:       "http://127.0.0.1:1/token",
		accessToken:      "old-access",
		refreshToken:     "refresh-token-secret",
		credentialPolicy: permissiveCredentialSourcePolicy(),
	}

	policy := permission.DefaultPolicy()
	policy.SetMode(permission.OperationNetwork, permission.ModeDeny)

	auditDir := t.TempDir()
	ctx := permission.ContextWithPolicy(context.Background(), &policy)
	ctx = permission.ContextWithAuditDir(ctx, auditDir)

	err := auth.refresh(ctx, "old-access")
	require.Error(t, err)
	require.True(t, permission.ErrDenied(err))

	auditData, readErr := os.ReadFile(filepath.Join(auditDir, credentialAuditLedgerFileName))
	require.NoError(t, readErr)

	audit := string(auditData)
	assert.Contains(t, audit, credentialAuditEventRefresh)
	assert.Contains(t, audit, `"decision":"failed"`)
	assert.Contains(t, audit, "permission.network.deny")
	assert.NotContains(t, audit, "refresh-token-secret")
}
