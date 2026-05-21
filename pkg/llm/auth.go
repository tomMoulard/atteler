package llm

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"sync"
	"time"
)

// ---------------------------------------------------------------------------
// Anthropic credential resolution
// ---------------------------------------------------------------------------

var defaultContextFactory = context.Background

func defaultCredentialContext() context.Context {
	return defaultContextFactory()
}

func nonNilCredentialContext(ctx context.Context) context.Context {
	if ctx != nil {
		return ctx
	}

	return defaultCredentialContext()
}

// ResolveAnthropicKey returns an Anthropic API credential by trying, in order:
//  1. ANTHROPIC_API_KEY env var                (Console API key -> X-Api-Key header)
//  2. ANTHROPIC_AUTH_TOKEN env var             (bearer token)
//  3. CLAUDE_CODE_OAUTH_TOKEN env var         (long-lived OAuth from `claude setup-token`)
//  4. ForgeCode ClaudeCode/Anthropic credentials
//  5. macOS Keychain "Claude Code-credentials" (reuse Claude Code's OAuth session)
//  6. ~/.claude/.credentials.json              (Linux/Windows fallback)
//
// The second return value indicates whether the credential is a bearer token
// (true) or a plain API key (false).
func ResolveAnthropicKey() (key string, bearer bool, err error) {
	return ResolveAnthropicKeyContext(defaultCredentialContext())
}

// ResolveAnthropicKeyContext returns an Anthropic API credential using ctx for
// any credential-store command or OAuth refresh request that may block.
func ResolveAnthropicKeyContext(ctx context.Context) (key string, bearer bool, err error) {
	ctx = nonNilCredentialContext(ctx)

	if v := os.Getenv("ANTHROPIC_API_KEY"); v != "" {
		return v, false, nil
	}

	if v := os.Getenv("ANTHROPIC_AUTH_TOKEN"); v != "" {
		return v, true, nil
	}

	if v := os.Getenv("CLAUDE_CODE_OAUTH_TOKEN"); v != "" {
		return v, true, nil
	}

	// Try loading from ForgeCode's credential store. Forge stores provider
	// login state in FORGE_CONFIG/.credentials.json or the default config dir.
	if tok, isBearer, err := resolveForgeAnthropicCredentials(ctx); err == nil && tok != "" {
		return tok, isBearer, nil
	}

	// Try loading from Claude Code's local credential store.
	if tok, err := resolveClaudeCodeCredentials(ctx); err == nil && tok != "" {
		return tok, true, nil
	}

	return "", false, errors.New(
		"no Anthropic credentials found: set ANTHROPIC_API_KEY, ANTHROPIC_AUTH_TOKEN, " +
			"CLAUDE_CODE_OAUTH_TOKEN, log in with `forge provider login claude_code`, or log in with `claude` CLI",
	)
}

// claudeCodeCredentials is the JSON stored in the Keychain / credentials file.
type claudeCodeCredentials struct {
	ClaudeAIOAuth *claudeOAuthBlock `json:"claudeAiOauth"`
}

type claudeOAuthBlock struct {
	AccessToken  string `json:"accessToken"`
	RefreshToken string `json:"refreshToken"`
	ExpiresAt    int64  `json:"expiresAt"` // epoch ms
}

// parseClaudeCodeCredentialsRaw extracts the OAuth block without enforcing
// expiry. Used by the auto-refreshing ClaudeCodeProvider, which can recover
// from an expired access token by exchanging the refresh token. Callers that
// only have an access token to use as-is should call parseClaudeCodeCredentials
// instead.
func parseClaudeCodeCredentialsRaw(data []byte) (claudeOAuthBlock, error) {
	var creds claudeCodeCredentials
	if err := json.Unmarshal(data, &creds); err != nil {
		return claudeOAuthBlock{}, fmt.Errorf("invalid Claude Code credentials JSON: %w", err)
	}

	if creds.ClaudeAIOAuth == nil {
		return claudeOAuthBlock{}, errors.New("no claudeAiOauth block in Claude Code credentials")
	}

	if creds.ClaudeAIOAuth.RefreshToken == "" && creds.ClaudeAIOAuth.AccessToken == "" {
		return claudeOAuthBlock{}, errors.New("claude code credentials contain neither access nor refresh token")
	}

	return *creds.ClaudeAIOAuth, nil
}

// resolveClaudeCodeCredentials tries platform-specific credential stores.
func resolveClaudeCodeCredentials(ctx context.Context) (string, error) {
	ctx = nonNilCredentialContext(ctx)
	// macOS: read from Keychain.
	if runtime.GOOS == "darwin" {
		if tok, err := readClaudeCodeKeychain(ctx); err == nil {
			return tok, nil
		}
	}

	// Linux / Windows / fallback: read plaintext credentials file.
	return readClaudeCodeCredentialsFile()
}

// readClaudeCodeCredentialsFile reads ~/.claude/.credentials.json (Linux/Windows).
func readClaudeCodeCredentialsFile() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("cannot determine home directory: %w", err)
	}

	data, err := os.ReadFile(filepath.Join(home, ".claude", ".credentials.json"))
	if err != nil {
		return "", fmt.Errorf("cannot read Claude Code credentials: %w", err)
	}

	return parseClaudeCodeCredentials(data)
}

// parseClaudeCodeCredentials extracts the access token from the JSON blob.
func parseClaudeCodeCredentials(data []byte) (string, error) {
	var creds claudeCodeCredentials
	if err := json.Unmarshal(data, &creds); err != nil {
		return "", fmt.Errorf("invalid Claude Code credentials JSON: %w", err)
	}

	if creds.ClaudeAIOAuth != nil && creds.ClaudeAIOAuth.AccessToken != "" {
		if creds.ClaudeAIOAuth.expired(time.Now().Add(forgeTokenExpirySkew)) {
			return "", errors.New("claude code accessToken expired")
		}

		return creds.ClaudeAIOAuth.AccessToken, nil
	}

	return "", errors.New("no accessToken in Claude Code credentials")
}

func (c claudeOAuthBlock) expired(cutoff time.Time) bool {
	if c.ExpiresAt <= 0 {
		return false
	}

	return !time.UnixMilli(c.ExpiresAt).After(cutoff)
}

// forgeCredentialEntry mirrors one entry in ForgeCode's .credentials.json.
// Current ForgeCode stores this file as an array keyed by provider ID.
type forgeCredentialEntry struct {
	ID               string           `json:"id"`
	AuthDetails      forgeAuthDetails `json:"auth_details"`
	AuthDetailsCamel forgeAuthDetails `json:"authDetails"`
}

type forgeAuthDetails struct {
	APIKey      string      `json:"api_key"`
	APIKeyCamel string      `json:"apiKey"`
	OAuth       *forgeOAuth `json:"o_auth"`
	OAuthCamel  *forgeOAuth `json:"oAuth"`
	Token       string      `json:"token"`
	AccessToken string      `json:"access_token"`
	AccessCamel string      `json:"accessToken"`
}

type forgeOAuth struct {
	Config forgeOAuthConfig `json:"config"`
	Tokens forgeOAuthTokens `json:"tokens"`
}

type forgeOAuthConfig struct {
	ClientID      string `json:"client_id"`
	ClientCamel   string `json:"clientId"`
	TokenURL      string `json:"token_url"`
	TokenURLCamel string `json:"tokenUrl"`
}

type forgeOAuthTokens struct {
	AccessToken  string `json:"access_token"`
	AccessCamel  string `json:"accessToken"`
	RefreshToken string `json:"refresh_token"`
	RefreshCamel string `json:"refreshToken"`
	ExpiresAt    string `json:"expires_at"`
	ExpiresCamel string `json:"expiresAt"`
}

const (
	forgeTokenExpirySkew       = 2 * time.Minute
	forgeOAuthRefreshTimeout   = 30 * time.Second
	maxOAuthErrorBodyBytes     = 4096
	forgeClaudeCodeProviderID  = "claude_code"
	forgeOAuthRefreshGrantType = "refresh_token"
)

var (
	errForgeOAuthRefreshUnavailable = errors.New("ForgeCode OAuth refresh unavailable")
	forgeOAuthHTTPClient            = &http.Client{Timeout: forgeOAuthRefreshTimeout}
)

// resolveForgeAnthropicCredentials tries ForgeCode credential files for the
// built-in ClaudeCode OAuth provider first, then the plain Anthropic API-key
// provider. The returned bool indicates whether the credential is a bearer
// token.
func resolveForgeAnthropicCredentials(ctx context.Context) (key string, bearer bool, err error) {
	ctx = nonNilCredentialContext(ctx)

	var failures []error

	for _, path := range forgeCredentialPaths() {
		key, bearer, err := readForgeCredentialsFile(ctx, path)
		if err == nil && key != "" {
			return key, bearer, nil
		}

		if err != nil {
			failures = append(failures, err)
		}
	}

	if len(failures) == 0 {
		return "", false, errors.New("no ForgeCode credential paths")
	}

	return "", false, fmt.Errorf("no ForgeCode Anthropic credentials found: %w", errors.Join(failures...))
}

func readForgeCredentialsFile(ctx context.Context, path string) (key string, bearer bool, err error) {
	ctx = nonNilCredentialContext(ctx)

	data, err := os.ReadFile(path)
	if err != nil {
		return "", false, fmt.Errorf("cannot read ForgeCode credentials %s: %w", path, err)
	}

	entries, err := parseForgeCredentialEntries(data)
	if err != nil {
		return "", false, err
	}

	if key := forgeCredentialForProvider(entries, forgeClaudeCodeProviderID); key != "" {
		return key, true, nil
	}

	refreshErr := error(nil)

	if key, err := refreshForgeClaudeCodeCredential(ctx, path, data, entries); err == nil && key != "" {
		return key, true, nil
	} else if err != nil && !errors.Is(err, errForgeOAuthRefreshUnavailable) {
		refreshErr = err
	}

	if key := forgeCredentialForProvider(entries, providerAnthropic); key != "" {
		return key, false, nil
	}

	if refreshErr != nil {
		return "", false, refreshErr
	}

	return "", false, errors.New("no claude_code or anthropic credential in ForgeCode credentials")
}

func forgeCredentialPaths() []string {
	var paths []string

	add := func(path string) {
		path = strings.TrimSpace(path)
		if path == "" {
			return
		}

		if slices.Contains(paths, path) {
			return
		}

		paths = append(paths, path)
	}

	if dir := os.Getenv("FORGE_CONFIG"); dir != "" {
		add(filepath.Join(dir, ".credentials.json"))
	}

	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		return paths
	}

	// ForgeCode's current docs and builds have used both ~/forge and ~/.forge
	// as the default config directory. Support both to avoid tying auth to one
	// release's path convention.
	add(filepath.Join(home, "forge", ".credentials.json"))
	add(filepath.Join(home, ".forge", ".credentials.json"))

	return paths
}

// parseForgeAnthropicCredentials extracts the best Anthropic-compatible
// credential from ForgeCode's provider credential array.
func parseForgeAnthropicCredentials(data []byte) (key string, bearer bool, err error) {
	entries, err := parseForgeCredentialEntries(data)
	if err != nil {
		return "", false, err
	}

	return forgeAnthropicCredentialFromEntries(entries)
}

func parseForgeCredentialEntries(data []byte) ([]forgeCredentialEntry, error) {
	var entries []forgeCredentialEntry
	if err := json.Unmarshal(data, &entries); err != nil {
		return nil, fmt.Errorf("invalid ForgeCode credentials JSON: %w", err)
	}

	return entries, nil
}

func forgeAnthropicCredentialFromEntries(entries []forgeCredentialEntry) (key string, bearer bool, err error) {
	if key := forgeCredentialForProvider(entries, forgeClaudeCodeProviderID); key != "" {
		return key, true, nil
	}

	if key := forgeCredentialForProvider(entries, providerAnthropic); key != "" {
		return key, false, nil
	}

	return "", false, errors.New("no claude_code or anthropic credential in ForgeCode credentials")
}

func refreshForgeClaudeCodeCredential(ctx context.Context, path string, data []byte, entries []forgeCredentialEntry) (string, error) {
	ctx = nonNilCredentialContext(ctx)

	entry := forgeProviderEntry(entries, forgeClaudeCodeProviderID)
	if entry == nil {
		return "", errForgeOAuthRefreshUnavailable
	}

	oauth := entry.authDetails().oauth()
	if oauth == nil || oauth.Tokens.refreshToken() == "" {
		return "", errForgeOAuthRefreshUnavailable
	}

	if token := oauth.Tokens.validAccessToken(); token != "" {
		return token, nil
	}

	tokens, err := refreshForgeOAuthToken(ctx, oauth.Config, oauth.Tokens.refreshToken())
	if err != nil {
		return "", err
	}

	if err := writeRefreshedForgeCredentials(path, data, tokens); err != nil {
		return "", err
	}

	return tokens.accessToken(), nil
}

type forgeOAuthRefreshResponse struct {
	AccessToken    string `json:"access_token"`
	AccessCamel    string `json:"accessToken"`
	RefreshToken   string `json:"refresh_token"`
	RefreshCamel   string `json:"refreshToken"`
	ExpiresAt      string `json:"expires_at"`
	ExpiresCamel   string `json:"expiresAt"`
	ExpiresIn      int64  `json:"expires_in"`
	ExpiresInCamel int64  `json:"expiresIn"`
}

func refreshForgeOAuthToken(ctx context.Context, config forgeOAuthConfig, refreshToken string) (forgeOAuthTokens, error) {
	ctx = nonNilCredentialContext(ctx)
	tokenURL := config.tokenURL()

	clientID := config.clientID()
	if tokenURL == "" || clientID == "" {
		return forgeOAuthTokens{}, errForgeOAuthRefreshUnavailable
	}

	reqBody, err := json.Marshal(map[string]string{
		"client_id":     clientID,
		"grant_type":    forgeOAuthRefreshGrantType,
		"refresh_token": refreshToken,
	})
	if err != nil {
		return forgeOAuthTokens{}, fmt.Errorf("ForgeCode OAuth refresh request: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, tokenURL, bytes.NewReader(reqBody))
	if err != nil {
		return forgeOAuthTokens{}, fmt.Errorf("ForgeCode OAuth refresh request: %w", err)
	}

	req.Header.Set("Accept", "application/json")
	req.Header.Set("Content-Type", "application/json")

	resp, err := forgeOAuthHTTPClient.Do(req)
	if err != nil {
		return forgeOAuthTokens{}, fmt.Errorf("ForgeCode OAuth refresh: %w", err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(io.LimitReader(resp.Body, maxOAuthErrorBodyBytes))
	if err != nil {
		return forgeOAuthTokens{}, fmt.Errorf("ForgeCode OAuth refresh read response: %w", err)
	}

	if resp.StatusCode != http.StatusOK {
		return forgeOAuthTokens{}, fmt.Errorf("ForgeCode OAuth refresh HTTP %d: %s", resp.StatusCode, body)
	}

	var refreshed forgeOAuthRefreshResponse
	if err := json.Unmarshal(body, &refreshed); err != nil {
		return forgeOAuthTokens{}, fmt.Errorf("ForgeCode OAuth refresh response: %w", err)
	}

	tokens := refreshed.tokens(refreshToken)
	if tokens.accessToken() == "" {
		return forgeOAuthTokens{}, errors.New("ForgeCode OAuth refresh response missing access token")
	}

	return tokens, nil
}

func writeRefreshedForgeCredentials(path string, data []byte, tokens forgeOAuthTokens) error {
	var raw []map[string]any
	if err := json.Unmarshal(data, &raw); err != nil {
		return fmt.Errorf("update ForgeCode credentials: %w", err)
	}

	updated := false

	for i := range raw {
		id, ok := raw[i]["id"].(string)
		if !ok || !strings.EqualFold(strings.TrimSpace(id), forgeClaudeCodeProviderID) {
			continue
		}

		tokenMap := forgeTokenMap(raw[i])
		if tokenMap == nil {
			return errForgeOAuthRefreshUnavailable
		}

		setCredentialString(tokenMap, "access_token", "accessToken", tokens.accessToken())
		setCredentialString(tokenMap, "refresh_token", "refreshToken", tokens.refreshToken())
		setCredentialString(tokenMap, "expires_at", "expiresAt", tokens.expiresAt())

		updated = true

		break
	}

	if !updated {
		return errForgeOAuthRefreshUnavailable
	}

	out, err := json.MarshalIndent(raw, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal refreshed ForgeCode credentials: %w", err)
	}

	out = append(out, '\n')
	if err := os.WriteFile(path, out, 0o600); err != nil {
		return fmt.Errorf("write refreshed ForgeCode credentials: %w", err)
	}

	return nil
}

func forgeTokenMap(entry map[string]any) map[string]any {
	authDetails := mapStringAny(entry, "auth_details", "authDetails")
	if authDetails == nil {
		return nil
	}

	oauth := mapStringAny(authDetails, "o_auth", "oAuth")
	if oauth == nil {
		return nil
	}

	tokens, ok := oauth["tokens"].(map[string]any)
	if !ok {
		tokens = make(map[string]any)
		oauth["tokens"] = tokens
	}

	return tokens
}

func mapStringAny(raw map[string]any, keys ...string) map[string]any {
	for _, key := range keys {
		nested, ok := raw[key].(map[string]any)
		if ok {
			return nested
		}
	}

	return nil
}

func setCredentialString(raw map[string]any, snakeKey, camelKey, value string) {
	if value == "" {
		return
	}

	if _, ok := raw[camelKey]; ok {
		raw[camelKey] = value
		return
	}

	raw[snakeKey] = value
}

func (r forgeOAuthRefreshResponse) tokens(currentRefreshToken string) forgeOAuthTokens {
	expiresAt := firstNonEmptyString(r.ExpiresAt, r.ExpiresCamel)
	if expiresAt == "" {
		if expiresIn := firstNonZeroInt64(r.ExpiresIn, r.ExpiresInCamel); expiresIn > 0 {
			expiresAt = time.Now().Add(time.Duration(expiresIn) * time.Second).UTC().Format(time.RFC3339Nano)
		}
	}

	return forgeOAuthTokens{
		AccessToken:  firstNonEmptyString(r.AccessToken, r.AccessCamel),
		RefreshToken: firstNonEmptyString(r.RefreshToken, r.RefreshCamel, currentRefreshToken),
		ExpiresAt:    expiresAt,
	}
}

func forgeProviderEntry(entries []forgeCredentialEntry, providerID string) *forgeCredentialEntry {
	for i := range entries {
		entry := &entries[i]
		if strings.EqualFold(strings.TrimSpace(entry.ID), providerID) {
			return entry
		}
	}

	return nil
}

func forgeCredentialForProvider(entries []forgeCredentialEntry, providerID string) string {
	entry := forgeProviderEntry(entries, providerID)
	if entry == nil {
		return ""
	}

	return entry.authDetails().credential()
}

func (e forgeCredentialEntry) authDetails() forgeAuthDetails {
	if !e.AuthDetails.empty() {
		return e.AuthDetails
	}

	return e.AuthDetailsCamel
}

func (a forgeAuthDetails) credential() string {
	if oauth := a.oauth(); oauth != nil {
		if token := oauth.Tokens.validAccessToken(); token != "" {
			return token
		}
	}

	return firstNonEmptyString(a.APIKey, a.APIKeyCamel, a.AccessToken, a.AccessCamel, a.Token)
}

func (a forgeAuthDetails) oauth() *forgeOAuth {
	if a.OAuth != nil {
		return a.OAuth
	}

	return a.OAuthCamel
}

func (a forgeAuthDetails) empty() bool {
	return a.APIKey == "" &&
		a.APIKeyCamel == "" &&
		a.OAuth == nil &&
		a.OAuthCamel == nil &&
		a.Token == "" &&
		a.AccessToken == "" &&
		a.AccessCamel == ""
}

func (t forgeOAuthTokens) accessToken() string {
	return firstNonEmptyString(t.AccessToken, t.AccessCamel)
}

func (t forgeOAuthTokens) refreshToken() string {
	return firstNonEmptyString(t.RefreshToken, t.RefreshCamel)
}

func (t forgeOAuthTokens) expiresAt() string {
	return firstNonEmptyString(t.ExpiresAt, t.ExpiresCamel)
}

func (t forgeOAuthTokens) validAccessToken() string {
	token := t.accessToken()
	if token == "" || t.expired(time.Now().Add(forgeTokenExpirySkew)) {
		return ""
	}

	return token
}

func (t forgeOAuthTokens) expired(cutoff time.Time) bool {
	expiresAt := firstNonEmptyString(t.ExpiresAt, t.ExpiresCamel)
	if expiresAt == "" {
		return false
	}

	expiry, err := time.Parse(time.RFC3339Nano, expiresAt)
	if err != nil {
		return false
	}

	return !expiry.After(cutoff)
}

func firstNonEmptyString(values ...string) string {
	for _, value := range values {
		if value != "" {
			return value
		}
	}

	return ""
}

func firstNonZeroInt64(values ...int64) int64 {
	for _, value := range values {
		if value != 0 {
			return value
		}
	}

	return 0
}

func (c forgeOAuthConfig) clientID() string {
	return firstNonEmptyString(c.ClientID, c.ClientCamel)
}

func (c forgeOAuthConfig) tokenURL() string {
	return firstNonEmptyString(c.TokenURL, c.TokenURLCamel)
}

// ---------------------------------------------------------------------------
// OpenAI credential resolution
// ---------------------------------------------------------------------------

// codexAuth mirrors the relevant fields of ~/.codex/auth.json.
type codexAuth struct {
	AuthMode string      `json:"auth_mode"`
	APIKey   *string     `json:"OPENAI_API_KEY"`
	Tokens   codexTokens `json:"tokens"`
}

type codexTokens struct {
	AccessToken  string `json:"access_token"`
	RefreshToken string `json:"refresh_token"`
	AccountID    string `json:"account_id"`
}

// codexChatGPTAuth is a thread-safe handle to a ChatGPT-mode codex auth.json
// file. It reads the access/refresh tokens and persists refreshed tokens back
// to disk atomically so concurrent codex CLI invocations stay in sync.
type codexChatGPTAuth struct {
	httpClient   *http.Client
	refreshURL   string // overridable for tests
	authPath     string
	accessToken  string
	refreshToken string
	accountID    string
	mu           sync.Mutex
}

// codexChatGPTOAuthClientID is the OAuth client_id codex uses for the
// ChatGPT-login refresh flow. It is published in the open-source codex
// repository (Apache 2.0) and is not a secret.
const codexChatGPTOAuthClientID = "app_EMoamEEZ73f0CkXaXp7hrann"

// codexChatGPTRefreshURL is the OpenAI OAuth token endpoint codex uses for
// chatgpt-mode refresh.
const codexChatGPTRefreshURL = "https://auth.openai.com/oauth/token"

var codexChatGPTHTTPClient = &http.Client{Timeout: forgeOAuthRefreshTimeout}

// loadCodexChatGPTAuth returns a chatgpt-mode auth handle for the codex
// auth.json under codexHome (which may be empty to use ~/.codex or
// $CODEX_HOME). It returns an error if auth.json is missing, malformed, or
// not in chatgpt mode.
func loadCodexChatGPTAuth(codexHome string) (*codexChatGPTAuth, error) {
	if codexHome == "" {
		codexHome = codexConfigDir()
	}

	if codexHome == "" {
		return nil, errors.New("cannot determine codex home directory")
	}

	authPath := filepath.Join(codexHome, "auth.json")

	data, err := os.ReadFile(authPath)
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", authPath, err)
	}

	var auth codexAuth
	if err := json.Unmarshal(data, &auth); err != nil {
		return nil, fmt.Errorf("parse %s: %w", authPath, err)
	}

	if !strings.EqualFold(auth.AuthMode, "chatgpt") {
		return nil, fmt.Errorf("codex auth_mode is %q, want chatgpt", auth.AuthMode)
	}

	if auth.Tokens.AccessToken == "" || auth.Tokens.RefreshToken == "" {
		return nil, fmt.Errorf("codex %s missing tokens", authPath)
	}

	return &codexChatGPTAuth{
		authPath:     authPath,
		accessToken:  auth.Tokens.AccessToken,
		refreshToken: auth.Tokens.RefreshToken,
		accountID:    auth.Tokens.AccountID,
		httpClient:   codexChatGPTHTTPClient,
		refreshURL:   codexChatGPTRefreshURL,
	}, nil
}

// snapshot returns a copy of the current tokens for use in an outgoing
// request. Reads are mutex-protected so they observe the latest refresh.
func (a *codexChatGPTAuth) snapshot() (access, account string) {
	a.mu.Lock()
	defer a.mu.Unlock()

	return a.accessToken, a.accountID
}

// refresh exchanges the stored refresh_token for fresh tokens and writes the
// new state back to auth.json. The caller may pass a previously observed
// access token to skip the refresh if another goroutine has already refreshed.
func (a *codexChatGPTAuth) refresh(ctx context.Context, observedAccess string) error {
	a.mu.Lock()
	defer a.mu.Unlock()

	if observedAccess != "" && observedAccess != a.accessToken {
		// Another caller refreshed concurrently; the stored token is already new.
		return nil
	}

	body, err := json.Marshal(map[string]string{
		"client_id":     codexChatGPTOAuthClientID,
		"grant_type":    forgeOAuthRefreshGrantType,
		"refresh_token": a.refreshToken,
	})
	if err != nil {
		return fmt.Errorf("codex chatgpt refresh: marshal: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, a.refreshURL, bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("codex chatgpt refresh: %w", err)
	}

	req.Header.Set("Accept", "application/json")
	req.Header.Set("Content-Type", "application/json")

	resp, err := a.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("codex chatgpt refresh: %w", err)
	}
	defer resp.Body.Close()

	respBody, err := io.ReadAll(io.LimitReader(resp.Body, maxOAuthErrorBodyBytes))
	if err != nil {
		return fmt.Errorf("codex chatgpt refresh: read response: %w", err)
	}

	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("codex chatgpt refresh: HTTP %d: %s", resp.StatusCode, respBody)
	}

	var refreshed struct {
		IDToken      string `json:"id_token"`
		AccessToken  string `json:"access_token"`
		RefreshToken string `json:"refresh_token"`
	}
	if err := json.Unmarshal(respBody, &refreshed); err != nil {
		return fmt.Errorf("codex chatgpt refresh: decode response: %w", err)
	}

	if refreshed.AccessToken == "" {
		return errors.New("codex chatgpt refresh: response missing access_token")
	}

	a.accessToken = refreshed.AccessToken
	if refreshed.RefreshToken != "" {
		a.refreshToken = refreshed.RefreshToken
	}

	return persistRefreshedCodexAuth(a.authPath, refreshed.AccessToken, refreshed.RefreshToken, refreshed.IDToken)
}

// persistRefreshedCodexAuth merges the refreshed tokens into auth.json while
// preserving any unrelated fields. The write is atomic via tempfile + rename.
func persistRefreshedCodexAuth(path, accessToken, refreshToken, idToken string) error {
	raw, err := readCodexAuthMap(path)
	if err != nil {
		return err
	}

	mergeCodexTokens(raw, accessToken, refreshToken, idToken)
	raw["last_refresh"] = time.Now().UTC().Format(time.RFC3339Nano)

	out, err := json.MarshalIndent(raw, "", "  ")
	if err != nil {
		return fmt.Errorf("codex auth.json marshal: %w", err)
	}

	return atomicWriteFile(path, append(out, '\n'), 0o600)
}

func readCodexAuthMap(path string) (map[string]any, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("codex auth.json read: %w", err)
	}

	var raw map[string]any
	if err := json.Unmarshal(data, &raw); err != nil {
		return nil, fmt.Errorf("codex auth.json parse: %w", err)
	}

	if raw == nil {
		raw = map[string]any{}
	}

	return raw, nil
}

func mergeCodexTokens(raw map[string]any, accessToken, refreshToken, idToken string) {
	tokens, ok := raw["tokens"].(map[string]any)
	if !ok {
		tokens = map[string]any{}
		raw["tokens"] = tokens
	}

	if accessToken != "" {
		tokens["access_token"] = accessToken
	}

	if refreshToken != "" {
		tokens["refresh_token"] = refreshToken
	}

	if idToken != "" {
		tokens["id_token"] = idToken
	}
}

func atomicWriteFile(path string, data []byte, mode os.FileMode) error {
	tmp, err := os.CreateTemp(filepath.Dir(path), ".auth.json.tmp-*")
	if err != nil {
		return fmt.Errorf("codex auth.json tempfile: %w", err)
	}

	tmpPath := tmp.Name()
	cleanup := true

	defer func() {
		if cleanup {
			_ = os.Remove(tmpPath)
		}
	}()

	if err := os.Chmod(tmpPath, mode); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("codex auth.json chmod tempfile: %w", err)
	}

	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("codex auth.json write tempfile: %w", err)
	}

	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("codex auth.json sync tempfile: %w", err)
	}

	if err := tmp.Close(); err != nil {
		return fmt.Errorf("codex auth.json close tempfile: %w", err)
	}

	if err := os.Rename(tmpPath, path); err != nil {
		return fmt.Errorf("codex auth.json rename: %w", err)
	}

	cleanup = false

	return nil
}

// ResolveOpenAIKey returns an OpenAI Platform API credential by trying, in order:
//  1. OPENAI_API_KEY env var
//  2. ~/.codex/auth.json  ->  OPENAI_API_KEY field  (if non-null)
//
// The second return value indicates whether the credential is a bearer token
// (true) or a plain API key (false).
func ResolveOpenAIKey() (key string, bearer bool, err error) {
	if v := os.Getenv("OPENAI_API_KEY"); v != "" {
		return v, false, nil
	}

	home, err := os.UserHomeDir()
	if err != nil {
		return "", false, errors.New("no OpenAI credentials found: set OPENAI_API_KEY")
	}

	data, err := os.ReadFile(filepath.Join(home, ".codex", "auth.json"))
	if err != nil {
		return "", false, errors.New("no OpenAI credentials found: set OPENAI_API_KEY or log in with `codex` CLI")
	}

	var auth codexAuth
	if err := json.Unmarshal(data, &auth); err != nil {
		return "", false, errors.New("failed to parse ~/.codex/auth.json")
	}

	// Prefer an explicit API key stored in auth.json.
	if auth.APIKey != nil && *auth.APIKey != "" {
		return *auth.APIKey, false, nil
	}

	return "", false, errors.New("no OpenAI Platform API key found in OPENAI_API_KEY or ~/.codex/auth.json")
}

// ---------------------------------------------------------------------------
// Claude Code OAuth credential resolution + auto-refresh
// ---------------------------------------------------------------------------

// claudeCodeOAuthClientID is the OAuth client_id Claude Code embeds for its
// `claude login` flow. It is a public identifier shipped in every Claude Code
// distribution (verified by reading the bundled `claude` binary) and is not a
// secret.
const claudeCodeOAuthClientID = "9d1c250a-e61b-44d9-88ed-5944d1962f5e"

// claudeCodeRefreshURL is the OAuth token endpoint Claude Code uses for the
// refresh_token grant.
const claudeCodeRefreshURL = "https://platform.claude.com/v1/oauth/token"

var claudeCodeHTTPClient = &http.Client{Timeout: forgeOAuthRefreshTimeout}

// claudeCodeCredentialPersister writes a refreshed OAuth block back to wherever
// the credentials originated, preserving any unrelated fields in the stored
// JSON. Used by claudeCodeAuth.refresh so atteler stays in sync with whatever
// store the claude CLI itself reads.
type claudeCodeCredentialPersister interface {
	persist(ctx context.Context, accessToken, refreshToken string, expiresAtMs int64) error
	location() string
}

// claudeCodeAuth is a thread-safe handle to Claude Code's OAuth credentials.
// It refreshes access tokens against the Claude Code OAuth endpoint and writes
// the refreshed state back atomically, mirroring the codex chatgpt-mode flow.
type claudeCodeAuth struct {
	httpClient   *http.Client
	refreshURL   string
	persist      claudeCodeCredentialPersister
	accessToken  string
	refreshToken string
	expiresAt    int64 // epoch ms; 0 means "unknown"
	mu           sync.Mutex
}

// loadClaudeCodeAuth discovers Claude Code OAuth credentials, in order:
//  1. macOS Keychain "Claude Code-credentials" (darwin only)
//  2. ~/.claude/.credentials.json
//
// The returned handle can refresh and persist credentials back to the same
// source so the claude CLI continues to see fresh tokens.
func loadClaudeCodeAuth(ctx context.Context) (*claudeCodeAuth, error) {
	ctx = nonNilCredentialContext(ctx)

	// Allow tests to opt out of the keychain probe even on darwin.
	if os.Getenv("ATTELER_CLAUDE_CODE_SKIP_KEYCHAIN") != "1" {
		if block, persister, err := readClaudeCodeKeychainAuth(ctx); err == nil {
			return newClaudeCodeAuthFromBlock(block, persister), nil
		}
	}

	home, err := os.UserHomeDir()
	if err != nil {
		return nil, fmt.Errorf("no Claude Code credentials: cannot determine home: %w", err)
	}

	path := filepath.Join(home, ".claude", ".credentials.json")

	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("no Claude Code credentials: %w (run `claude` to log in)", err)
	}

	block, err := parseClaudeCodeCredentialsRaw(data)
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", path, err)
	}

	return newClaudeCodeAuthFromBlock(block, &claudeCodeFilePersister{path: path}), nil
}

func newClaudeCodeAuthFromBlock(block claudeOAuthBlock, persister claudeCodeCredentialPersister) *claudeCodeAuth {
	return &claudeCodeAuth{
		httpClient:   claudeCodeHTTPClient,
		refreshURL:   claudeCodeRefreshURL,
		persist:      persister,
		accessToken:  block.AccessToken,
		refreshToken: block.RefreshToken,
		expiresAt:    block.ExpiresAt,
	}
}

// snapshot returns the current access token for an outgoing request.
func (a *claudeCodeAuth) snapshot() string {
	a.mu.Lock()
	defer a.mu.Unlock()

	return a.accessToken
}

// refresh exchanges the stored refresh_token for fresh tokens and writes the
// new state back to the credential source. The caller may pass the access
// token it observed; if another goroutine has already refreshed since then,
// this call is a no-op.
func (a *claudeCodeAuth) refresh(ctx context.Context, observedAccess string) error {
	a.mu.Lock()
	defer a.mu.Unlock()

	if observedAccess != "" && observedAccess != a.accessToken {
		return nil
	}

	if a.refreshToken == "" {
		return errors.New("claude code refresh: no refresh_token available")
	}

	body, err := json.Marshal(map[string]string{
		"client_id":     claudeCodeOAuthClientID,
		"grant_type":    forgeOAuthRefreshGrantType,
		"refresh_token": a.refreshToken,
	})
	if err != nil {
		return fmt.Errorf("claude code refresh: marshal: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, a.refreshURL, bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("claude code refresh: %w", err)
	}

	req.Header.Set("Accept", "application/json")
	req.Header.Set("Content-Type", "application/json")

	resp, err := a.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("claude code refresh: %w", err)
	}
	defer resp.Body.Close()

	respBody, err := io.ReadAll(io.LimitReader(resp.Body, maxOAuthErrorBodyBytes))
	if err != nil {
		return fmt.Errorf("claude code refresh: read response: %w", err)
	}

	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("claude code refresh: HTTP %d: %s", resp.StatusCode, respBody)
	}

	var refreshed struct {
		AccessToken  string `json:"access_token"`
		RefreshToken string `json:"refresh_token"`
		ExpiresIn    int64  `json:"expires_in"`
	}
	if err := json.Unmarshal(respBody, &refreshed); err != nil {
		return fmt.Errorf("claude code refresh: decode response: %w", err)
	}

	if refreshed.AccessToken == "" {
		return errors.New("claude code refresh: response missing access_token")
	}

	expiresAtMs := int64(0)
	if refreshed.ExpiresIn > 0 {
		expiresAtMs = time.Now().Add(time.Duration(refreshed.ExpiresIn) * time.Second).UnixMilli()
	}

	a.accessToken = refreshed.AccessToken
	if refreshed.RefreshToken != "" {
		a.refreshToken = refreshed.RefreshToken
	}

	if expiresAtMs > 0 {
		a.expiresAt = expiresAtMs
	}

	return a.persist.persist(ctx, a.accessToken, a.refreshToken, a.expiresAt)
}

// claudeCodeFilePersister writes refreshed credentials back to ~/.claude/.credentials.json.
type claudeCodeFilePersister struct {
	path string
}

func (p *claudeCodeFilePersister) location() string { return p.path }

func (p *claudeCodeFilePersister) persist(_ context.Context, accessToken, refreshToken string, expiresAtMs int64) error {
	raw, err := readJSONObject(p.path)
	if err != nil {
		return err
	}

	mergeClaudeCodeOAuth(raw, accessToken, refreshToken, expiresAtMs)

	out, err := json.MarshalIndent(raw, "", "  ")
	if err != nil {
		return fmt.Errorf("claude code credentials marshal: %w", err)
	}

	return atomicWriteFile(p.path, append(out, '\n'), 0o600)
}

// readJSONObject reads a JSON object from path, returning an empty map when the
// file is missing so callers can write a fresh blob.
func readJSONObject(path string) (map[string]any, error) {
	data, err := os.ReadFile(path)

	switch {
	case errors.Is(err, os.ErrNotExist):
		return map[string]any{}, nil
	case err != nil:
		return nil, fmt.Errorf("read %s: %w", path, err)
	}

	if len(bytes.TrimSpace(data)) == 0 {
		return map[string]any{}, nil
	}

	var raw map[string]any
	if err := json.Unmarshal(data, &raw); err != nil {
		return nil, fmt.Errorf("parse %s: %w", path, err)
	}

	if raw == nil {
		raw = map[string]any{}
	}

	return raw, nil
}

// mergeClaudeCodeOAuth updates the claudeAiOauth fields in place, preserving
// any unrelated fields the claude CLI may have stored alongside.
func mergeClaudeCodeOAuth(raw map[string]any, accessToken, refreshToken string, expiresAtMs int64) {
	block, ok := raw["claudeAiOauth"].(map[string]any)
	if !ok {
		block = map[string]any{}
		raw["claudeAiOauth"] = block
	}

	if accessToken != "" {
		block["accessToken"] = accessToken
	}

	if refreshToken != "" {
		block["refreshToken"] = refreshToken
	}

	if expiresAtMs > 0 {
		block["expiresAt"] = expiresAtMs
	}
}
