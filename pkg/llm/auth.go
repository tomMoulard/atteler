package llm

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
)

// ---------------------------------------------------------------------------
// Anthropic credential resolution
// ---------------------------------------------------------------------------

// ResolveAnthropicKey returns an Anthropic API credential by trying, in order:
//  1. ANTHROPIC_API_KEY env var                (Console API key -> X-Api-Key header)
//  2. ANTHROPIC_AUTH_TOKEN env var             (bearer token)
//  3. CLAUDE_CODE_OAUTH_TOKEN env var         (long-lived OAuth from `claude setup-token`)
//  4. macOS Keychain "Claude Code-credentials" (reuse Claude Code's OAuth session)
//  5. ~/.claude/.credentials.json              (Linux/Windows fallback)
//
// The second return value indicates whether the credential is a bearer token
// (true) or a plain API key (false).
func ResolveAnthropicKey() (key string, bearer bool, err error) {
	if v := os.Getenv("ANTHROPIC_API_KEY"); v != "" {
		return v, false, nil
	}
	if v := os.Getenv("ANTHROPIC_AUTH_TOKEN"); v != "" {
		return v, true, nil
	}
	if v := os.Getenv("CLAUDE_CODE_OAUTH_TOKEN"); v != "" {
		return v, true, nil
	}

	// Try loading from Claude Code's local credential store.
	if tok, err := resolveClaudeCodeCredentials(); err == nil && tok != "" {
		return tok, true, nil
	}

	return "", false, errors.New(
		"no Anthropic credentials found: set ANTHROPIC_API_KEY, ANTHROPIC_AUTH_TOKEN, " +
			"CLAUDE_CODE_OAUTH_TOKEN, or log in with `claude` CLI",
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
func resolveClaudeCodeCredentials() (string, error) {
	// macOS: read from Keychain.
	if runtime.GOOS == "darwin" {
		if tok, err := readClaudeCodeKeychain(); err == nil {
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
		return creds.ClaudeAIOAuth.AccessToken, nil
	}
	return "", errors.New("no accessToken in Claude Code credentials")
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

// ResolveOpenAIKey returns an OpenAI API credential by trying, in order:
//  1. OPENAI_API_KEY env var
//  2. ~/.codex/auth.json  ->  OPENAI_API_KEY field  (if non-null)
//  3. ~/.codex/auth.json  ->  tokens.access_token   (ChatGPT OAuth token)
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

	// Fall back to the ChatGPT OAuth access token.
	if auth.Tokens.AccessToken != "" {
		return auth.Tokens.AccessToken, true, nil
	}

	return "", false, errors.New("no OpenAI credentials found in OPENAI_API_KEY or ~/.codex/auth.json")
}
