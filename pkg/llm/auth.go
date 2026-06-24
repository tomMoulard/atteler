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

	"github.com/tommoulard/atteler/pkg/permission"
)

// ErrContextRequired is returned by APIs that may perform blocking credential
// or network work when the caller does not provide a context.
var ErrContextRequired = errors.New("llm: context is required")

func requireCredentialContext(ctx context.Context) error {
	if ctx == nil {
		return ErrContextRequired
	}

	if err := ctx.Err(); err != nil {
		return fmt.Errorf("llm: context already done: %w", err)
	}

	return nil
}

// ---------------------------------------------------------------------------
// Anthropic credential resolution
// ---------------------------------------------------------------------------

// ResolveAnthropicKey is kept for source compatibility only.
//
// Deprecated: use ResolveAnthropicKeyContext so credential-store commands and
// OAuth refresh requests inherit caller cancellation and deadlines.
func ResolveAnthropicKey() (key string, bearer bool, err error) {
	return "", false, ErrContextRequired
}

// ResolveAnthropicKeyContext returns an Anthropic API credential by trying, in order:
//  1. ANTHROPIC_API_KEY env var                (Console API key -> X-Api-Key header)
//  2. ANTHROPIC_AUTH_TOKEN env var             (bearer token)
//  3. CLAUDE_CODE_OAUTH_TOKEN env var         (long-lived OAuth from `claude setup-token`)
//  4. ForgeCode ClaudeCode/Anthropic credentials
//  5. macOS Keychain "Claude Code-credentials" (reuse Claude Code's OAuth session)
//  6. ~/.claude/.credentials.json              (Linux/Windows fallback)
//
// Borrowed credential sources in steps 3-6 are skipped when the global
// private/borrowed adapter kill switches are set.
//
// The second return value indicates whether the credential is a bearer token
// (true) or a plain API key (false).
func ResolveAnthropicKeyContext(ctx context.Context) (key string, bearer bool, err error) {
	return resolveAnthropicKeyContext(ctx, ProviderConfig{}, !borrowedAnthropicCredentialsDisabled(ProviderConfig{}))
}

// ResolveAnthropicKeyWithConfigContext resolves Anthropic credentials while
// honoring provider kill switches for borrowed Claude Code/Forge credential
// stores. API keys and explicit ANTHROPIC_AUTH_TOKEN bearer tokens remain
// available so disabling private adapters does not disable the normal Anthropic
// provider.
func ResolveAnthropicKeyWithConfigContext(ctx context.Context, cfg ProviderConfig) (key string, bearer bool, err error) {
	return resolveAnthropicKeyContext(ctx, cfg, !borrowedAnthropicCredentialsDisabled(cfg))
}

//nolint:cyclop,gocognit // Credential precedence intentionally stays explicit and auditable.
func resolveAnthropicKeyContext(ctx context.Context, cfg ProviderConfig, allowBorrowedCredentials bool) (key string, bearer bool, err error) {
	if err := requireCredentialContext(ctx); err != nil {
		return "", false, err
	}

	if policyErr := authorizeProviderPermission(
		ctx,
		providerAnthropic,
		"resolve Anthropic credentials",
		"ANTHROPIC_API_KEY/ANTHROPIC_AUTH_TOKEN/borrowed credential stores",
		permission.OperationCredentialAccess,
	); policyErr != nil {
		return "", false, policyErr
	}

	if v := os.Getenv("ANTHROPIC_API_KEY"); v != "" {
		if err := authorizeCredentialSourcePolicy(ctx, cfg, credentialSource{
			Provider:    providerAnthropic,
			Store:       CredentialStoreEnv,
			Description: "ANTHROPIC_API_KEY",
		}, credentialActionUse); err != nil {
			return "", false, err
		}

		return v, false, nil
	}

	if v := os.Getenv("ANTHROPIC_AUTH_TOKEN"); v != "" {
		if err := authorizeCredentialSourcePolicy(ctx, cfg, credentialSource{
			Provider:    providerAnthropic,
			Store:       CredentialStoreEnv,
			Description: "ANTHROPIC_AUTH_TOKEN",
		}, credentialActionUse); err != nil {
			return "", false, err
		}

		return v, true, nil
	}

	if !allowBorrowedCredentials {
		return "", false, errors.New(
			"no Anthropic API credentials found: borrowed Claude Code/Forge credential stores are disabled; set ANTHROPIC_API_KEY or ANTHROPIC_AUTH_TOKEN",
		)
	}

	if v := os.Getenv("CLAUDE_CODE_OAUTH_TOKEN"); v != "" {
		if err := authorizeCredentialSourcePolicy(ctx, cfg, credentialSource{
			Provider:      providerAnthropic,
			Store:         CredentialStoreEnv,
			Description:   "CLAUDE_CODE_OAUTH_TOKEN",
			BorrowedOAuth: true,
		}, credentialActionUse); err != nil {
			return "", false, err
		}

		return v, true, nil
	}

	var sourcePolicyFailures []error

	// Try loading from ForgeCode's credential store. Forge stores provider
	// login state in FORGE_CONFIG/.credentials.json or the default config dir.
	if tok, isBearer, err := resolveForgeAnthropicCredentials(ctx, cfg); err == nil && tok != "" {
		return tok, isBearer, nil
	} else if isBlockingCredentialResolutionError(err) {
		return "", false, err
	} else if err != nil && containsCredentialSourcePolicyError(err) {
		sourcePolicyFailures = append(sourcePolicyFailures, err)
	}

	// Try loading from Claude Code's local credential store.
	if tok, err := resolveClaudeCodeCredentials(ctx, cfg); err == nil && tok != "" {
		return tok, true, nil
	} else if isBlockingCredentialResolutionError(err) {
		return "", false, err
	} else if err != nil && containsCredentialSourcePolicyError(err) {
		sourcePolicyFailures = append(sourcePolicyFailures, err)
	}

	if len(sourcePolicyFailures) > 0 {
		return "", false, fmt.Errorf(
			"no Anthropic credentials available from allowed credential sources: %w; set ANTHROPIC_API_KEY/ANTHROPIC_AUTH_TOKEN or configure providers.anthropic.credential_policy.allowed_stores",
			errors.Join(sourcePolicyFailures...),
		)
	}

	return "", false, errors.New(
		"no Anthropic credentials found: set ANTHROPIC_API_KEY, ANTHROPIC_AUTH_TOKEN, " +
			"CLAUDE_CODE_OAUTH_TOKEN, log in with `forge provider login claude_code`, or log in with `claude` CLI",
	)
}

func isContextError(err error) bool {
	return errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded)
}

func isBlockingCredentialResolutionError(err error) bool {
	return isContextError(err) || permission.ErrDenied(err) || isBlockingCredentialSourcePolicyError(err)
}

func borrowedAnthropicCredentialsDisabled(cfg ProviderConfig) bool {
	return cfg.DisablePrivateAdapter ||
		envBool("ATTELER_DISABLE_PRIVATE_ADAPTERS") ||
		envBool("ATTELER_DISABLE_BORROWED_CREDENTIAL_ADAPTERS") ||
		envBool("ATTELER_DISABLE_CLAUDE_CODE_ADAPTER")
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
func resolveClaudeCodeCredentials(ctx context.Context, cfg ProviderConfig) (string, error) {
	if err := requireCredentialContext(ctx); err != nil {
		return "", err
	}

	// macOS: read from Keychain.
	if runtime.GOOS == "darwin" {
		keychainErr := authorizeCredentialSourcePolicy(ctx, cfg, credentialSource{
			Provider:      providerClaudeCode,
			Store:         CredentialStoreClaudeCodeKeychain,
			Description:   "Claude Code keychain OAuth",
			Location:      claudeCodeKeychainSource,
			BorrowedOAuth: true,
		}, credentialActionRead)
		if keychainErr == nil {
			if tok, err := readClaudeCodeKeychain(ctx); err == nil {
				return tok, nil
			}
		} else if isBlockingCredentialResolutionError(keychainErr) {
			return "", keychainErr
		}
	}

	// Linux / Windows / fallback: read plaintext credentials file.
	return readClaudeCodeCredentialsFile(ctx, cfg)
}

// readClaudeCodeCredentialsFile reads ~/.claude/.credentials.json (Linux/Windows).
func readClaudeCodeCredentialsFile(ctx context.Context, cfg ProviderConfig) (string, error) {
	if err := requireCredentialContext(ctx); err != nil {
		return "", err
	}

	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("cannot determine home directory: %w", err)
	}

	path := filepath.Join(home, ".claude", ".credentials.json")
	if sourceErr := authorizeCredentialSourcePolicy(ctx, cfg, credentialSource{
		Provider:      providerClaudeCode,
		Store:         CredentialStoreClaudeCodeFile,
		Description:   "Claude Code credentials file",
		Location:      path,
		BorrowedOAuth: true,
	}, credentialActionRead); sourceErr != nil {
		return "", sourceErr
	}

	if policyErr := authorizeProviderCredentialFileRead(ctx, providerClaudeCode, "read Claude Code credentials file", path); policyErr != nil {
		return "", policyErr
	}

	data, err := os.ReadFile(path)
	if err != nil {
		return "", fmt.Errorf("cannot read Claude Code credentials: %s", redactCredentialPathError(err))
	}

	if err := requireCredentialContext(ctx); err != nil {
		return "", err
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
func resolveForgeAnthropicCredentials(ctx context.Context, cfg ProviderConfig) (key string, bearer bool, err error) {
	if err := requireCredentialContext(ctx); err != nil {
		return "", false, err
	}

	var failures []error

	for _, path := range forgeCredentialPaths() {
		key, bearer, err := readForgeCredentialsFile(ctx, cfg, path)
		if err == nil && key != "" {
			return key, bearer, nil
		}

		if err != nil {
			if isBlockingCredentialResolutionError(err) {
				return "", false, err
			}

			failures = append(failures, err)
		}
	}

	if len(failures) == 0 {
		return "", false, errors.New("no ForgeCode credential paths")
	}

	return "", false, fmt.Errorf("no ForgeCode Anthropic credentials found: %w", errors.Join(failures...))
}

//nolint:cyclop // Credential precedence and policy/fallback handling stay explicit and auditable.
func readForgeCredentialsFile(ctx context.Context, cfg ProviderConfig, path string) (key string, bearer bool, err error) {
	if ctxErr := requireCredentialContext(ctx); ctxErr != nil {
		return "", false, ctxErr
	}

	if sourceErr := authorizeCredentialSourcePolicy(ctx, cfg, credentialSource{
		Provider:    providerAnthropic,
		Store:       CredentialStoreForgeCredentials,
		Description: "ForgeCode credentials file",
		Location:    path,
	}, credentialActionRead); sourceErr != nil {
		return "", false, sourceErr
	}

	if policyErr := authorizeProviderCredentialFileRead(ctx, providerAnthropic, "read ForgeCode credentials file", path); policyErr != nil {
		return "", false, policyErr
	}

	data, err := os.ReadFile(path)
	if err != nil {
		return "", false, fmt.Errorf("cannot read ForgeCode credentials: %s", redactCredentialPathError(err))
	}

	entries, err := parseForgeCredentialEntries(data)
	if err != nil {
		return "", false, err
	}

	var refreshErr error

	claudeCodeOAuthSource := forgeClaudeCodeOAuthCredentialSource(path)
	if key := forgeCredentialForProvider(entries, forgeClaudeCodeProviderID); key != "" {
		if sourceErr := authorizeCredentialSourcePolicy(ctx, cfg, claudeCodeOAuthSource, credentialActionUse); sourceErr != nil {
			refreshErr = sourceErr
		} else {
			return key, true, nil
		}
	}

	if key, err := refreshForgeClaudeCodeCredential(ctx, cfg, path, data, entries); err == nil && key != "" {
		return key, true, nil
	} else if isContextError(err) || permission.ErrDenied(err) {
		return "", false, err
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

func forgeClaudeCodeOAuthCredentialSource(path string) credentialSource {
	return credentialSource{
		Provider:      providerAnthropic,
		Store:         CredentialStoreForgeCredentials,
		Description:   "ForgeCode claude_code OAuth",
		Location:      path,
		BorrowedOAuth: true,
	}
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

func refreshForgeClaudeCodeCredential(ctx context.Context, cfg ProviderConfig, path string, data []byte, entries []forgeCredentialEntry) (string, error) {
	if err := requireCredentialContext(ctx); err != nil {
		return "", fmt.Errorf("read current ForgeCode credentials: %w", err)
	}

	entry := forgeProviderEntry(entries, forgeClaudeCodeProviderID)
	if entry == nil {
		return "", errForgeOAuthRefreshUnavailable
	}

	oauth := entry.authDetails().oauth()
	if oauth == nil || oauth.Tokens.refreshToken() == "" {
		return "", errForgeOAuthRefreshUnavailable
	}

	if token := oauth.Tokens.validAccessToken(); token != "" {
		if err := authorizeCredentialSourcePolicy(ctx, cfg, forgeClaudeCodeOAuthCredentialSource(path), credentialActionUse); err != nil {
			return "", err
		}

		return token, nil
	}

	source := forgeClaudeCodeOAuthCredentialSource(path)
	if err := authorizeCredentialSourcePolicy(ctx, cfg, source, credentialActionRefresh); err != nil {
		return "", err
	}

	if err := authorizeCredentialSourcePolicy(ctx, cfg, source, credentialActionWriteBack); err != nil {
		return "", err
	}

	tokens, err := refreshForgeOAuthToken(ctx, oauth.Config, oauth.Tokens.refreshToken())
	if err != nil {
		return "", err
	}

	auditCredentialEvent(ctx, credentialAuditEvent{
		Event:         credentialAuditEventRefresh,
		Provider:      source.Provider,
		Store:         source.Store,
		Action:        credentialActionRefresh,
		Source:        source.Description,
		Location:      source.Location,
		BorrowedOAuth: source.BorrowedOAuth,
		Decision:      "allowed",
	})

	if err := writeRefreshedForgeCredentials(ctx, cfg, path, data, tokens); err != nil {
		if mismatch := new(credentialFileCASMismatch); errors.As(err, &mismatch) {
			if current, currentErr := currentForgeClaudeCodeToken(ctx, path); currentErr == nil && current != "" {
				return current, nil
			}
		}

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
	if err := requireCredentialContext(ctx); err != nil {
		return forgeOAuthTokens{}, err
	}

	tokenURL := config.tokenURL()

	clientID := config.clientID()
	if tokenURL == "" || clientID == "" {
		return forgeOAuthTokens{}, errForgeOAuthRefreshUnavailable
	}

	if policyErr := authorizeProviderPermission(
		ctx,
		providerAnthropic,
		"refresh ForgeCode OAuth token",
		tokenURL,
		permission.OperationNetwork,
		permission.OperationCredentialAccess,
	); policyErr != nil {
		return forgeOAuthTokens{}, policyErr
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
		return forgeOAuthTokens{}, fmt.Errorf("ForgeCode OAuth refresh: %w", newProviderHTTPError(providerAnthropic, resp, body))
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

func writeRefreshedForgeCredentials(ctx context.Context, cfg ProviderConfig, path string, data []byte, tokens forgeOAuthTokens) error {
	if err := requireCredentialContext(ctx); err != nil {
		return err
	}

	source := forgeClaudeCodeOAuthCredentialSource(path)
	if err := authorizeCredentialSourcePolicy(ctx, cfg, source, credentialActionWriteBack); err != nil {
		return err
	}

	if policyErr := authorizeProviderPermission(
		ctx,
		providerAnthropic,
		"write refreshed ForgeCode credentials",
		path,
		permission.OperationWrite,
		permission.OperationCredentialAccess,
	); policyErr != nil {
		return policyErr
	}

	expectedDigest := digestCredentialFile(data)

	return withCredentialFileLock(path, func() error {
		if err := requireCredentialContext(ctx); err != nil {
			return err
		}

		current, err := os.ReadFile(path)
		if err != nil {
			return fmt.Errorf("read ForgeCode credentials for write-back: %s", redactCredentialPathError(err))
		}

		if !compareCredentialFileDigest(current, expectedDigest) {
			auditCredentialEvent(ctx, credentialAuditEvent{
				Event:         credentialAuditEventCAS,
				Provider:      source.Provider,
				Store:         source.Store,
				Action:        credentialActionWriteBack,
				Source:        source.Description,
				Location:      source.Location,
				BorrowedOAuth: source.BorrowedOAuth,
				Decision:      "skipped",
				Reason:        "credential file changed before write-back",
			})

			return &credentialFileCASMismatch{path: path}
		}

		out, err := refreshedForgeCredentialsJSON(current, tokens)
		if err != nil {
			return err
		}

		if err := requireCredentialContext(ctx); err != nil {
			return err
		}

		if err := atomicWriteCredentialFile(ctx, path, out, 0o600); err != nil {
			return fmt.Errorf("write refreshed ForgeCode credentials: %w", err)
		}

		auditCredentialEvent(ctx, credentialAuditEvent{
			Event:         credentialAuditEventWriteBack,
			Provider:      source.Provider,
			Store:         source.Store,
			Action:        credentialActionWriteBack,
			Source:        source.Description,
			Location:      source.Location,
			BorrowedOAuth: source.BorrowedOAuth,
			Decision:      "allowed",
		})

		return nil
	})
}

func refreshedForgeCredentialsJSON(data []byte, tokens forgeOAuthTokens) ([]byte, error) {
	var raw []map[string]any
	if err := json.Unmarshal(data, &raw); err != nil {
		return nil, fmt.Errorf("update ForgeCode credentials: %w", err)
	}

	updated := false

	for i := range raw {
		id, ok := raw[i]["id"].(string)
		if !ok || !strings.EqualFold(strings.TrimSpace(id), forgeClaudeCodeProviderID) {
			continue
		}

		tokenMap := forgeTokenMap(raw[i])
		if tokenMap == nil {
			return nil, errForgeOAuthRefreshUnavailable
		}

		setCredentialString(tokenMap, "access_token", "accessToken", tokens.accessToken())
		setCredentialString(tokenMap, "refresh_token", "refreshToken", tokens.refreshToken())
		setCredentialString(tokenMap, "expires_at", "expiresAt", tokens.expiresAt())

		updated = true

		break
	}

	if !updated {
		return nil, errForgeOAuthRefreshUnavailable
	}

	out, err := json.MarshalIndent(raw, "", "  ")
	if err != nil {
		return nil, fmt.Errorf("marshal refreshed ForgeCode credentials: %w", err)
	}

	return append(out, '\n'), nil
}

func currentForgeClaudeCodeToken(ctx context.Context, path string) (string, error) {
	if err := requireCredentialContext(ctx); err != nil {
		return "", err
	}

	current, err := os.ReadFile(path)
	if err != nil {
		return "", fmt.Errorf("read current ForgeCode credentials: %s", redactCredentialPathError(err))
	}

	entries, err := parseForgeCredentialEntries(current)
	if err != nil {
		return "", fmt.Errorf("parse current ForgeCode credentials: %w", err)
	}

	return forgeCredentialForProvider(entries, forgeClaudeCodeProviderID), nil
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
	httpClient       *http.Client
	refreshURL       string // overridable for tests
	authPath         string
	accessToken      string
	refreshToken     string
	accountID        string
	source           credentialProvenance
	credentialPolicy CredentialSourcePolicy
	mu               sync.Mutex
}

// codexChatGPTOAuthClientID is the OAuth client_id codex uses for the
// ChatGPT-login refresh flow. It is published in the open-source codex
// repository (Apache 2.0) and is not a secret.
const codexChatGPTOAuthClientID = "app_EMoamEEZ73f0CkXaXp7hrann"

// codexChatGPTRefreshURL is the OpenAI OAuth token endpoint codex uses for
// chatgpt-mode refresh.
const codexChatGPTRefreshURL = "https://auth.openai.com/oauth/token"

var codexChatGPTHTTPClient = &http.Client{Timeout: forgeOAuthRefreshTimeout}

// loadCodexChatGPTAuthContext returns a chatgpt-mode auth handle for the codex
// auth.json under codexHome (which may be empty to use ~/.codex or
// $CODEX_HOME). It returns an error if auth.json is missing, malformed, or
// not in chatgpt mode.
func loadCodexChatGPTAuthContext(ctx context.Context, codexHome string) (*codexChatGPTAuth, error) {
	return loadCodexChatGPTAuthWithConfigContext(ctx, codexHome, ProviderConfig{})
}

func loadCodexChatGPTAuthWithConfigContext(ctx context.Context, codexHome string, cfg ProviderConfig) (*codexChatGPTAuth, error) {
	if err := requireCredentialContext(ctx); err != nil {
		return nil, err
	}

	if codexHome == "" {
		codexHome = codexConfigDir()
	}

	if codexHome == "" {
		return nil, errors.New("cannot determine codex home directory")
	}

	authPath := filepath.Join(codexHome, "auth.json")
	if err := authorizeCredentialSourcePolicy(ctx, cfg, credentialSource{
		Provider:      providerCodex,
		Store:         CredentialStoreCodexAuthJSON,
		Description:   "Codex ChatGPT auth.json",
		Location:      authPath,
		BorrowedOAuth: true,
	}, credentialActionRead); err != nil {
		return nil, err
	}

	if policyErr := authorizeProviderCredentialFileRead(ctx, providerCodex, "load Codex ChatGPT credentials", authPath); policyErr != nil {
		return nil, policyErr
	}

	data, err := os.ReadFile(authPath)
	if err != nil {
		return nil, fmt.Errorf("read Codex auth.json: %s", redactCredentialPathError(err))
	}

	if err := requireCredentialContext(ctx); err != nil {
		return nil, err
	}

	var auth codexAuth
	if err := json.Unmarshal(data, &auth); err != nil {
		return nil, fmt.Errorf("parse %s: %w", redactCredentialLocation(authPath), err)
	}

	if !strings.EqualFold(auth.AuthMode, "chatgpt") {
		return nil, fmt.Errorf("codex auth_mode is %q, want chatgpt", auth.AuthMode)
	}

	if auth.Tokens.AccessToken == "" {
		return nil, fmt.Errorf("codex %s missing access_token", authPath)
	}

	return &codexChatGPTAuth{
		authPath:     authPath,
		accessToken:  auth.Tokens.AccessToken,
		refreshToken: auth.Tokens.RefreshToken,
		accountID:    auth.Tokens.AccountID,
		httpClient:   codexChatGPTHTTPClient,
		refreshURL:   codexChatGPTRefreshURL,
		source: credentialProvenance{
			Provider:      providerCodex,
			Store:         CredentialStoreCodexAuthJSON,
			Location:      authPath,
			Identifier:    auth.Tokens.AccountID,
			BorrowedOAuth: true,
		},
		credentialPolicy: credentialPolicyForProvider(ctx, cfg),
	}, nil
}

// snapshot returns a copy of the current tokens for use in an outgoing
// request. Reads are mutex-protected so they observe the latest refresh.
func (a *codexChatGPTAuth) snapshot() (access, account string) {
	a.mu.Lock()
	defer a.mu.Unlock()

	return a.accessToken, a.accountID
}

func (a *codexChatGPTAuth) hasRefreshToken() bool {
	a.mu.Lock()
	defer a.mu.Unlock()

	return a.refreshToken != ""
}

// refresh exchanges the stored refresh_token for fresh tokens and writes the
// new state back to auth.json. The caller may pass a previously observed
// access token to skip the refresh if another goroutine has already refreshed.
//
//nolint:cyclop,gocognit,nestif,wsl_v5 // Security-sensitive linear validate/request/persist flow; splitting would obscure the audit sequence.
func (a *codexChatGPTAuth) refresh(ctx context.Context, observedAccess string) error {
	if err := requireCredentialContext(ctx); err != nil {
		return err
	}

	a.mu.Lock()
	defer a.mu.Unlock()

	if observedAccess != "" && observedAccess != a.accessToken {
		// Another caller refreshed concurrently; the stored token is already new.
		return nil
	}

	if a.refreshToken == "" {
		return errors.New("codex chatgpt refresh: no refresh_token available")
	}

	source := a.credentialSource()
	cfg := ProviderConfig{CredentialPolicy: a.credentialPolicy}
	if err := authorizeCredentialSourcePolicy(ctx, cfg, source, credentialActionRefresh); err != nil {
		return err
	}

	if err := authorizeCredentialSourcePolicy(ctx, cfg, source, credentialActionWriteBack); err != nil {
		return err
	}

	if policyErr := authorizeProviderPermission(
		ctx,
		providerCodex,
		"refresh Codex ChatGPT credentials",
		a.refreshURL,
		permission.OperationNetwork,
		permission.OperationWrite,
		permission.OperationCredentialAccess,
	); policyErr != nil {
		return policyErr
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
		return fmt.Errorf("codex chatgpt refresh: %w", newProviderHTTPError(providerCodex, resp, respBody))
	}

	var refreshed struct {
		IDToken      string `json:"id_token"`
		AccessToken  string `json:"access_token"`
		RefreshToken string `json:"refresh_token"`
	}
	if decodeErr := json.Unmarshal(respBody, &refreshed); decodeErr != nil {
		return fmt.Errorf("codex chatgpt refresh: decode response: %w", decodeErr)
	}

	if refreshed.AccessToken == "" {
		return errors.New("codex chatgpt refresh: response missing access_token")
	}

	if ctxErr := requireCredentialContext(ctx); ctxErr != nil {
		return ctxErr
	}

	auditCredentialEvent(ctx, credentialAuditEvent{
		Event:         credentialAuditEventRefresh,
		Provider:      source.Provider,
		Store:         source.Store,
		Action:        credentialActionRefresh,
		Source:        source.Description,
		Location:      source.Location,
		Identifier:    source.Identifier,
		BorrowedOAuth: source.BorrowedOAuth,
		Decision:      "allowed",
	})

	state, err := persistRefreshedCodexAuthWithCAS(
		ctx,
		a.authPath,
		a.accessToken,
		a.refreshToken,
		refreshed.AccessToken,
		refreshed.RefreshToken,
		refreshed.IDToken,
		source,
		cfg,
	)
	if err != nil {
		if isCredentialFileCASMismatch(err) && state.accessToken != "" {
			a.accessToken = state.accessToken
			if state.refreshToken != "" {
				a.refreshToken = state.refreshToken
			}
			if state.accountID != "" {
				a.accountID = state.accountID
			}

			return nil
		}

		return err
	}

	a.accessToken = state.accessToken
	if state.refreshToken != "" {
		a.refreshToken = state.refreshToken
	}
	if state.accountID != "" {
		a.accountID = state.accountID
	}

	return nil
}

//nolint:wsl_v5 // Compact defaulting keeps provenance construction readable.
func (a *codexChatGPTAuth) credentialSource() credentialSource {
	source := a.source
	if source.Provider == "" {
		source.Provider = providerCodex
	}
	if source.Store == "" {
		source.Store = CredentialStoreCodexAuthJSON
	}
	if source.Location == "" {
		source.Location = a.authPath
	}
	source.BorrowedOAuth = true

	return credentialSourceFromProvenance(source, "Codex ChatGPT auth.json")
}

type codexAuthState struct {
	accessToken  string
	refreshToken string
	accountID    string
}

// persistRefreshedCodexAuth merges the refreshed tokens into auth.json while
// preserving any unrelated fields. The write is atomic via tempfile + rename.
func persistRefreshedCodexAuth(ctx context.Context, path, accessToken, refreshToken, idToken string) error {
	source := credentialSource{
		Provider:      providerCodex,
		Store:         CredentialStoreCodexAuthJSON,
		Description:   "Codex ChatGPT auth.json",
		Location:      path,
		BorrowedOAuth: true,
	}
	cfg := ProviderConfig{CredentialPolicy: permissiveCredentialSourcePolicy()}
	_, err := persistRefreshedCodexAuthWithCAS(ctx, path, "", "", accessToken, refreshToken, idToken, source, cfg)

	return err
}

//nolint:wsl_v5 // CAS/write-back sequence is clearer as one guarded critical section.
func persistRefreshedCodexAuthWithCAS(
	ctx context.Context,
	path, expectedAccessToken, expectedRefreshToken, accessToken, refreshToken, idToken string,
	source credentialSource,
	cfg ProviderConfig,
) (codexAuthState, error) {
	if err := requireCredentialContext(ctx); err != nil {
		return codexAuthState{}, err
	}

	if err := authorizeCredentialSourcePolicy(ctx, cfg, source, credentialActionWriteBack); err != nil {
		return codexAuthState{}, err
	}

	if policyErr := authorizeProviderPermission(
		ctx,
		providerCodex,
		"write refreshed Codex auth",
		path,
		permission.OperationWrite,
		permission.OperationCredentialAccess,
	); policyErr != nil {
		return codexAuthState{}, policyErr
	}

	var state codexAuthState

	err := withCredentialFileLock(path, func() error {
		raw, err := readCodexAuthMap(ctx, path)
		if err != nil {
			return err
		}

		current := codexAuthStateFromMap(raw)
		if !codexExpectedTokensMatch(current, expectedAccessToken, expectedRefreshToken) {
			state = current
			auditCredentialEvent(ctx, credentialAuditEvent{
				Event:         credentialAuditEventCAS,
				Provider:      source.Provider,
				Store:         source.Store,
				Action:        credentialActionWriteBack,
				Source:        source.Description,
				Location:      source.Location,
				Identifier:    source.Identifier,
				BorrowedOAuth: source.BorrowedOAuth,
				Decision:      "skipped",
				Reason:        "credential file changed before write-back",
			})

			return &credentialFileCASMismatch{path: path}
		}

		mergeCodexTokens(raw, accessToken, refreshToken, idToken)
		raw["last_refresh"] = time.Now().UTC().Format(time.RFC3339Nano)

		state = codexAuthStateFromMap(raw)
		if state.accessToken == "" {
			state.accessToken = accessToken
		}
		if state.refreshToken == "" {
			state.refreshToken = refreshToken
		}

		out, err := json.MarshalIndent(raw, "", "  ")
		if err != nil {
			return fmt.Errorf("codex auth.json marshal: %w", err)
		}

		if err := requireCredentialContext(ctx); err != nil {
			return err
		}

		if err := atomicWriteCredentialFile(ctx, path, append(out, '\n'), 0o600); err != nil {
			return err
		}

		auditCredentialEvent(ctx, credentialAuditEvent{
			Event:         credentialAuditEventWriteBack,
			Provider:      source.Provider,
			Store:         source.Store,
			Action:        credentialActionWriteBack,
			Source:        source.Description,
			Location:      source.Location,
			Identifier:    source.Identifier,
			BorrowedOAuth: source.BorrowedOAuth,
			Decision:      "allowed",
		})

		return nil
	})

	return state, err
}

func readCodexAuthMap(ctx context.Context, path string) (map[string]any, error) {
	if err := requireCredentialContext(ctx); err != nil {
		return nil, err
	}

	if policyErr := authorizeProviderCredentialFileRead(ctx, providerCodex, "read Codex auth.json", path); policyErr != nil {
		return nil, policyErr
	}

	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("codex auth.json read: %s", redactCredentialPathError(err))
	}

	if err := requireCredentialContext(ctx); err != nil {
		return nil, err
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

func codexAuthStateFromMap(raw map[string]any) codexAuthState {
	tokens, ok := raw["tokens"].(map[string]any)
	if !ok {
		return codexAuthState{}
	}

	return codexAuthState{
		accessToken:  stringFromMap(tokens, "access_token"),
		refreshToken: stringFromMap(tokens, "refresh_token"),
		accountID:    stringFromMap(tokens, "account_id"),
	}
}

func codexExpectedTokensMatch(current codexAuthState, expectedAccessToken, expectedRefreshToken string) bool {
	if expectedAccessToken != "" && current.accessToken != expectedAccessToken {
		return false
	}

	if expectedRefreshToken != "" && current.refreshToken != expectedRefreshToken {
		return false
	}

	return true
}

func stringFromMap(raw map[string]any, key string) string {
	value, ok := raw[key].(string)
	if !ok {
		return ""
	}

	return value
}

// ResolveOpenAIKey is kept for source compatibility only.
//
// Deprecated: use ResolveOpenAIKeyContext so credential reads can honor caller
// cancellation checks before and after filesystem access.
func ResolveOpenAIKey() (key string, bearer bool, err error) {
	return "", false, ErrContextRequired
}

// ResolveOpenAIKeyContext returns an OpenAI Platform API credential by trying, in order:
//  1. OPENAI_API_KEY env var
//  2. ~/.codex/auth.json  ->  OPENAI_API_KEY field  (if non-null)
//
// The second return value indicates whether the credential is a bearer token
// (true) or a plain API key (false).
func ResolveOpenAIKeyContext(ctx context.Context) (key string, bearer bool, err error) {
	return ResolveOpenAIKeyWithConfigContext(ctx, ProviderConfig{})
}

// ResolveOpenAIKeyWithConfigContext resolves OpenAI Platform credentials while
// honoring credential-source policy from cfg.
func ResolveOpenAIKeyWithConfigContext(ctx context.Context, cfg ProviderConfig) (key string, bearer bool, err error) {
	if ctxErr := requireCredentialContext(ctx); ctxErr != nil {
		return "", false, ctxErr
	}

	if policyErr := authorizeProviderPermission(
		ctx,
		providerOpenAI,
		"resolve OpenAI credentials",
		"OPENAI_API_KEY or ~/.codex/auth.json",
		permission.OperationCredentialAccess,
	); policyErr != nil {
		return "", false, policyErr
	}

	if v := os.Getenv("OPENAI_API_KEY"); v != "" {
		if sourceErr := authorizeCredentialSourcePolicy(ctx, cfg, credentialSource{
			Provider:    providerOpenAI,
			Store:       CredentialStoreEnv,
			Description: "OPENAI_API_KEY",
		}, credentialActionUse); sourceErr != nil {
			return "", false, sourceErr
		}

		return v, false, nil
	}

	home, err := os.UserHomeDir()
	if err != nil {
		return "", false, errors.New("no OpenAI credentials found: set OPENAI_API_KEY")
	}

	authPath := filepath.Join(home, ".codex", "auth.json")
	if sourceErr := authorizeCredentialSourcePolicy(ctx, cfg, credentialSource{
		Provider:    providerOpenAI,
		Store:       CredentialStoreCodexAuthJSON,
		Description: "Codex auth.json OpenAI API key",
		Location:    authPath,
	}, credentialActionRead); sourceErr != nil {
		return "", false, sourceErr
	}

	if policyErr := authorizeProviderCredentialFileRead(ctx, providerOpenAI, "read OpenAI auth.json", authPath); policyErr != nil {
		return "", false, policyErr
	}

	data, err := os.ReadFile(authPath)
	if err != nil {
		return "", false, errors.New("no OpenAI credentials found: set OPENAI_API_KEY or log in with `codex` CLI")
	}

	if err := requireCredentialContext(ctx); err != nil {
		return "", false, err
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

type claudeCodeCredentialCASPersister interface {
	persistCAS(
		ctx context.Context,
		expectedAccessToken, expectedRefreshToken, accessToken, refreshToken string,
		expiresAtMs int64,
		source credentialSource,
		cfg ProviderConfig,
	) (claudeCodePersistState, error)
}

type claudeCodePersistState struct {
	AccessToken  string
	RefreshToken string
	ExpiresAt    int64
}

// claudeCodeAuth is a thread-safe handle to Claude Code's OAuth credentials.
// It refreshes access tokens against the Claude Code OAuth endpoint and writes
// the refreshed state back atomically, mirroring the codex chatgpt-mode flow.
//
//nolint:govet // Field order keeps auth state grouped for auditability.
type claudeCodeAuth struct {
	httpClient       *http.Client
	refreshURL       string
	persist          claudeCodeCredentialPersister
	accessToken      string
	refreshToken     string
	expiresAt        int64 // epoch ms; 0 means "unknown"
	source           credentialProvenance
	credentialPolicy CredentialSourcePolicy
	mu               sync.Mutex
}

// loadClaudeCodeAuth discovers Claude Code OAuth credentials, in order:
//  1. macOS Keychain "Claude Code-credentials" (darwin only)
//  2. ~/.claude/.credentials.json
//
// The returned handle can refresh and persist credentials back to the same
// source so the claude CLI continues to see fresh tokens.
func loadClaudeCodeAuth(ctx context.Context) (*claudeCodeAuth, error) {
	return loadClaudeCodeAuthWithConfig(ctx, ProviderConfig{})
}

func loadClaudeCodeAuthWithConfig(ctx context.Context, cfg ProviderConfig) (*claudeCodeAuth, error) {
	if err := requireCredentialContext(ctx); err != nil {
		return nil, err
	}

	if policyErr := authorizeProviderPermission(
		ctx,
		providerClaudeCode,
		"load Claude Code credentials",
		"Claude Code keychain or ~/.claude/.credentials.json",
		permission.OperationCredentialAccess,
	); policyErr != nil {
		return nil, policyErr
	}

	// Allow tests to opt out of the keychain probe even on darwin.
	if runtime.GOOS == "darwin" && os.Getenv("ATTELER_CLAUDE_CODE_SKIP_KEYCHAIN") != "1" {
		keychainErr := authorizeCredentialSourcePolicy(ctx, cfg, credentialSource{
			Provider:      providerClaudeCode,
			Store:         CredentialStoreClaudeCodeKeychain,
			Description:   "Claude Code keychain OAuth",
			Location:      claudeCodeKeychainSource,
			BorrowedOAuth: true,
		}, credentialActionRead)
		if keychainErr == nil {
			if block, persister, err := readClaudeCodeKeychainAuth(ctx); err == nil {
				return newClaudeCodeAuthFromBlockWithSource(block, persister, credentialProvenance{
					Provider:      providerClaudeCode,
					Store:         CredentialStoreClaudeCodeKeychain,
					Location:      claudeCodeKeychainSource,
					BorrowedOAuth: true,
				}, credentialPolicyForProvider(ctx, cfg)), nil
			}
		} else if isBlockingCredentialResolutionError(keychainErr) {
			return nil, keychainErr
		}
	}

	home, err := os.UserHomeDir()
	if err != nil {
		return nil, fmt.Errorf("no Claude Code credentials: cannot determine home: %w", err)
	}

	path := filepath.Join(home, ".claude", ".credentials.json")
	if sourceErr := authorizeCredentialSourcePolicy(ctx, cfg, credentialSource{
		Provider:      providerClaudeCode,
		Store:         CredentialStoreClaudeCodeFile,
		Description:   "Claude Code credentials file",
		Location:      path,
		BorrowedOAuth: true,
	}, credentialActionRead); sourceErr != nil {
		return nil, sourceErr
	}

	if policyErr := authorizeProviderCredentialFileRead(ctx, providerClaudeCode, "read Claude Code credentials file", path); policyErr != nil {
		return nil, policyErr
	}

	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("no Claude Code credentials: %s (run `claude` to log in)", redactCredentialPathError(err))
	}

	if ctxErr := requireCredentialContext(ctx); ctxErr != nil {
		return nil, ctxErr
	}

	block, err := parseClaudeCodeCredentialsRaw(data)
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", redactCredentialLocation(path), err)
	}

	return newClaudeCodeAuthFromBlockWithSource(block, &claudeCodeFilePersister{path: path}, credentialProvenance{
		Provider:      providerClaudeCode,
		Store:         CredentialStoreClaudeCodeFile,
		Location:      path,
		BorrowedOAuth: true,
	}, credentialPolicyForProvider(ctx, cfg)), nil
}

func newClaudeCodeAuthFromBlock(block claudeOAuthBlock, persister claudeCodeCredentialPersister) *claudeCodeAuth {
	return newClaudeCodeAuthFromBlockWithSource(block, persister, credentialProvenance{
		Provider:      providerClaudeCode,
		Store:         claudeCodePersisterStore(persister),
		Location:      persister.location(),
		BorrowedOAuth: true,
	}, permissiveCredentialSourcePolicy())
}

func newClaudeCodeAuthFromBlockWithSource(
	block claudeOAuthBlock,
	persister claudeCodeCredentialPersister,
	source credentialProvenance,
	policy CredentialSourcePolicy,
) *claudeCodeAuth {
	return &claudeCodeAuth{
		httpClient:       claudeCodeHTTPClient,
		refreshURL:       claudeCodeRefreshURL,
		persist:          persister,
		accessToken:      block.AccessToken,
		refreshToken:     block.RefreshToken,
		expiresAt:        block.ExpiresAt,
		source:           source,
		credentialPolicy: policy,
	}
}

func claudeCodePersisterStore(persister claudeCodeCredentialPersister) string {
	if persister == nil {
		return ""
	}

	if strings.HasPrefix(persister.location(), "keychain:") {
		return CredentialStoreClaudeCodeKeychain
	}

	return CredentialStoreClaudeCodeFile
}

// snapshot returns the current access token for an outgoing request.
func (a *claudeCodeAuth) snapshot() string {
	a.mu.Lock()
	defer a.mu.Unlock()

	return a.accessToken
}

func (a *claudeCodeAuth) hasRefreshToken() bool {
	a.mu.Lock()
	defer a.mu.Unlock()

	return a.refreshToken != ""
}

// refresh exchanges the stored refresh_token for fresh tokens and writes the
// new state back to the credential source. The caller may pass the access
// token it observed; if another goroutine has already refreshed since then,
// this call is a no-op.
//
//nolint:cyclop,gocognit,nestif,wsl_v5 // Security-sensitive linear validate/request/persist flow; splitting would obscure the audit sequence.
func (a *claudeCodeAuth) refresh(ctx context.Context, observedAccess string) error {
	if err := requireCredentialContext(ctx); err != nil {
		return err
	}

	a.mu.Lock()
	defer a.mu.Unlock()

	if observedAccess != "" && observedAccess != a.accessToken {
		return nil
	}

	if a.refreshToken == "" {
		return errors.New("claude code refresh: no refresh_token available")
	}

	source := a.credentialSource()
	cfg := ProviderConfig{CredentialPolicy: a.credentialPolicy}
	if err := authorizeCredentialSourcePolicy(ctx, cfg, source, credentialActionRefresh); err != nil {
		return err
	}

	if err := authorizeCredentialSourcePolicy(ctx, cfg, source, credentialActionWriteBack); err != nil {
		return err
	}

	if policyErr := authorizeProviderPermission(
		ctx,
		providerClaudeCode,
		"refresh Claude Code credentials",
		a.refreshURL,
		permission.OperationNetwork,
		permission.OperationWrite,
		permission.OperationCredentialAccess,
	); policyErr != nil {
		return policyErr
	}

	refreshed, err := a.exchangeClaudeCodeRefreshToken(ctx)
	if err != nil {
		return err
	}

	if err := requireCredentialContext(ctx); err != nil {
		return err
	}

	auditCredentialEvent(ctx, credentialAuditEvent{
		Event:         credentialAuditEventRefresh,
		Provider:      source.Provider,
		Store:         source.Store,
		Action:        credentialActionRefresh,
		Source:        source.Description,
		Location:      source.Location,
		Identifier:    source.Identifier,
		BorrowedOAuth: source.BorrowedOAuth,
		Decision:      "allowed",
	})

	accessToken, refreshToken, expiresAt := a.refreshedClaudeCodeState(refreshed)
	if casPersister, ok := a.persist.(claudeCodeCredentialCASPersister); ok {
		state, err := casPersister.persistCAS(
			ctx,
			a.accessToken,
			a.refreshToken,
			accessToken,
			refreshToken,
			expiresAt,
			source,
			cfg,
		)
		if err != nil {
			if isCredentialFileCASMismatch(err) && state.AccessToken != "" {
				a.accessToken = state.AccessToken
				if state.RefreshToken != "" {
					a.refreshToken = state.RefreshToken
				}
				if state.ExpiresAt > 0 {
					a.expiresAt = state.ExpiresAt
				}

				return nil
			}

			return err
		}

		accessToken, refreshToken, expiresAt = state.AccessToken, state.RefreshToken, state.ExpiresAt
	} else {
		if err := authorizeCredentialSourcePolicy(ctx, cfg, source, credentialActionWriteBack); err != nil {
			return err
		}

		if err := a.persist.persist(ctx, accessToken, refreshToken, expiresAt); err != nil {
			return err
		}

		auditCredentialEvent(ctx, credentialAuditEvent{
			Event:         credentialAuditEventWriteBack,
			Provider:      source.Provider,
			Store:         source.Store,
			Action:        credentialActionWriteBack,
			Source:        source.Description,
			Location:      source.Location,
			Identifier:    source.Identifier,
			BorrowedOAuth: source.BorrowedOAuth,
			Decision:      "allowed",
		})
	}

	a.accessToken = accessToken
	a.refreshToken = refreshToken
	a.expiresAt = expiresAt

	return nil
}

//nolint:wsl_v5 // Compact defaulting keeps provenance construction readable.
func (a *claudeCodeAuth) credentialSource() credentialSource {
	source := a.source
	if source.Provider == "" {
		source.Provider = providerClaudeCode
	}
	if source.Store == "" {
		if a.persist != nil {
			source.Store = claudeCodePersisterStore(a.persist)
		} else {
			source.Store = CredentialStoreClaudeCodeFile
		}
	}
	if source.Location == "" && a.persist != nil {
		source.Location = a.persist.location()
	}
	source.BorrowedOAuth = true

	return credentialSourceFromProvenance(source, "Claude Code OAuth credentials")
}

type claudeCodeRefreshResponse struct {
	AccessToken  string `json:"access_token"`
	RefreshToken string `json:"refresh_token"`
	ExpiresIn    int64  `json:"expires_in"`
}

func (a *claudeCodeAuth) exchangeClaudeCodeRefreshToken(ctx context.Context) (claudeCodeRefreshResponse, error) {
	body, err := json.Marshal(map[string]string{
		"client_id":     claudeCodeOAuthClientID,
		"grant_type":    forgeOAuthRefreshGrantType,
		"refresh_token": a.refreshToken,
	})
	if err != nil {
		return claudeCodeRefreshResponse{}, fmt.Errorf("claude code refresh: marshal: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, a.refreshURL, bytes.NewReader(body))
	if err != nil {
		return claudeCodeRefreshResponse{}, fmt.Errorf("claude code refresh: %w", err)
	}

	req.Header.Set("Accept", "application/json")
	req.Header.Set("Content-Type", "application/json")

	resp, err := a.httpClient.Do(req)
	if err != nil {
		return claudeCodeRefreshResponse{}, fmt.Errorf("claude code refresh: %w", err)
	}
	defer resp.Body.Close()

	respBody, err := io.ReadAll(io.LimitReader(resp.Body, maxOAuthErrorBodyBytes))
	if err != nil {
		return claudeCodeRefreshResponse{}, fmt.Errorf("claude code refresh: read response: %w", err)
	}

	if resp.StatusCode != http.StatusOK {
		return claudeCodeRefreshResponse{}, fmt.Errorf("claude code refresh: %w", newProviderHTTPError(providerClaudeCode, resp, respBody))
	}

	var refreshed claudeCodeRefreshResponse
	if err := json.Unmarshal(respBody, &refreshed); err != nil {
		return claudeCodeRefreshResponse{}, fmt.Errorf("claude code refresh: decode response: %w", err)
	}

	if refreshed.AccessToken == "" {
		return claudeCodeRefreshResponse{}, errors.New("claude code refresh: response missing access_token")
	}

	return refreshed, nil
}

func (a *claudeCodeAuth) refreshedClaudeCodeState(refreshed claudeCodeRefreshResponse) (accessToken, refreshToken string, expiresAt int64) {
	expiresAtMs := int64(0)
	if refreshed.ExpiresIn > 0 {
		expiresAtMs = time.Now().Add(time.Duration(refreshed.ExpiresIn) * time.Second).UnixMilli()
	}

	accessToken = refreshed.AccessToken

	refreshToken = a.refreshToken
	if refreshed.RefreshToken != "" {
		refreshToken = refreshed.RefreshToken
	}

	expiresAt = a.expiresAt
	if expiresAtMs > 0 {
		expiresAt = expiresAtMs
	}

	return accessToken, refreshToken, expiresAt
}

// claudeCodeFilePersister writes refreshed credentials back to ~/.claude/.credentials.json.
type claudeCodeFilePersister struct {
	path string
}

func (p *claudeCodeFilePersister) location() string { return p.path }

func (p *claudeCodeFilePersister) persist(ctx context.Context, accessToken, refreshToken string, expiresAtMs int64) error {
	source := credentialSource{
		Provider:      providerClaudeCode,
		Store:         CredentialStoreClaudeCodeFile,
		Description:   "Claude Code credentials file",
		Location:      p.path,
		BorrowedOAuth: true,
	}
	_, err := p.persistCAS(ctx, "", "", accessToken, refreshToken, expiresAtMs, source, ProviderConfig{
		CredentialPolicy: permissiveCredentialSourcePolicy(),
	})

	return err
}

//nolint:wsl_v5 // CAS/write-back sequence is clearer as one guarded critical section.
func (p *claudeCodeFilePersister) persistCAS(
	ctx context.Context,
	expectedAccessToken, expectedRefreshToken, accessToken, refreshToken string,
	expiresAtMs int64,
	source credentialSource,
	cfg ProviderConfig,
) (claudeCodePersistState, error) {
	if ctxErr := requireCredentialContext(ctx); ctxErr != nil {
		return claudeCodePersistState{}, ctxErr
	}

	if sourceErr := authorizeCredentialSourcePolicy(ctx, cfg, source, credentialActionWriteBack); sourceErr != nil {
		return claudeCodePersistState{}, sourceErr
	}

	if policyErr := authorizeProviderPermission(
		ctx,
		providerClaudeCode,
		"write refreshed Claude Code credentials",
		p.path,
		permission.OperationWrite,
		permission.OperationCredentialAccess,
	); policyErr != nil {
		return claudeCodePersistState{}, policyErr
	}

	var state claudeCodePersistState

	err := withCredentialFileLock(p.path, func() error {
		raw, err := readJSONObject(ctx, providerClaudeCode, p.path)
		if err != nil {
			return err
		}

		current := claudeCodePersistStateFromMap(raw)
		if !claudeCodeExpectedTokensMatch(current, expectedAccessToken, expectedRefreshToken) {
			state = current
			auditCredentialEvent(ctx, credentialAuditEvent{
				Event:         credentialAuditEventCAS,
				Provider:      source.Provider,
				Store:         source.Store,
				Action:        credentialActionWriteBack,
				Source:        source.Description,
				Location:      source.Location,
				Identifier:    source.Identifier,
				BorrowedOAuth: source.BorrowedOAuth,
				Decision:      "skipped",
				Reason:        "credential file changed before write-back",
			})

			return &credentialFileCASMismatch{path: p.path}
		}

		mergeClaudeCodeOAuth(raw, accessToken, refreshToken, expiresAtMs)
		state = claudeCodePersistStateFromMap(raw)
		if state.AccessToken == "" {
			state.AccessToken = accessToken
		}
		if state.RefreshToken == "" {
			state.RefreshToken = refreshToken
		}
		if state.ExpiresAt == 0 {
			state.ExpiresAt = expiresAtMs
		}

		out, err := json.MarshalIndent(raw, "", "  ")
		if err != nil {
			return fmt.Errorf("claude code credentials marshal: %w", err)
		}

		if ctxErr := requireCredentialContext(ctx); ctxErr != nil {
			return ctxErr
		}

		if err := atomicWriteCredentialFile(ctx, p.path, append(out, '\n'), 0o600); err != nil {
			return err
		}

		auditCredentialEvent(ctx, credentialAuditEvent{
			Event:         credentialAuditEventWriteBack,
			Provider:      source.Provider,
			Store:         source.Store,
			Action:        credentialActionWriteBack,
			Source:        source.Description,
			Location:      source.Location,
			Identifier:    source.Identifier,
			BorrowedOAuth: source.BorrowedOAuth,
			Decision:      "allowed",
		})

		return nil
	})

	return state, err
}

// readJSONObject reads a JSON object from path, returning an empty map when the
// file is missing so callers can write a fresh blob.
func readJSONObject(ctx context.Context, providerName, path string) (map[string]any, error) {
	if policyErr := authorizeProviderCredentialFileRead(ctx, providerName, "read credentials JSON", path); policyErr != nil {
		return nil, policyErr
	}

	data, err := os.ReadFile(path)

	switch {
	case errors.Is(err, os.ErrNotExist):
		return map[string]any{}, nil
	case err != nil:
		return nil, fmt.Errorf("read %s: %s", redactCredentialLocation(path), redactCredentialPathError(err))
	}

	if len(bytes.TrimSpace(data)) == 0 {
		return map[string]any{}, nil
	}

	var raw map[string]any
	if err := json.Unmarshal(data, &raw); err != nil {
		return nil, fmt.Errorf("parse %s: %w", redactCredentialLocation(path), err)
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

func claudeCodePersistStateFromMap(raw map[string]any) claudeCodePersistState {
	block, ok := raw["claudeAiOauth"].(map[string]any)
	if !ok {
		return claudeCodePersistState{}
	}

	return claudeCodePersistState{
		AccessToken:  stringFromMap(block, "accessToken"),
		RefreshToken: stringFromMap(block, "refreshToken"),
		ExpiresAt:    int64FromMap(block, "expiresAt"),
	}
}

func claudeCodeExpectedTokensMatch(current claudeCodePersistState, expectedAccessToken, expectedRefreshToken string) bool {
	if expectedAccessToken != "" && current.AccessToken != expectedAccessToken {
		return false
	}

	if expectedRefreshToken != "" && current.RefreshToken != expectedRefreshToken {
		return false
	}

	return true
}

func int64FromMap(raw map[string]any, key string) int64 {
	switch value := raw[key].(type) {
	case int64:
		return value
	case int:
		return int64(value)
	case float64:
		return int64(value)
	default:
		return 0
	}
}
