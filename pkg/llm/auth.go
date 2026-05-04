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

func hasCodexAuth() bool {
	data, err := os.ReadFile(filepath.Join(codexConfigDir(), "auth.json"))
	if err != nil {
		return false
	}

	var auth codexAuth
	if err := json.Unmarshal(data, &auth); err != nil {
		return false
	}

	return (auth.APIKey != nil && *auth.APIKey != "") || auth.Tokens.AccessToken != ""
}
