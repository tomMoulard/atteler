//go:build !darwin

package llm

import "context"

// readClaudeCodeKeychain is a no-op on non-macOS platforms.
// Credential resolution falls back to ~/.claude/.credentials.json.
func readClaudeCodeKeychain(_ context.Context) (string, error) {
	return "", errKeychainUnsupported
}

// readClaudeCodeKeychainAuth is a no-op on non-macOS platforms; the
// ClaudeCodeProvider falls back to ~/.claude/.credentials.json.
func readClaudeCodeKeychainAuth(_ context.Context) (claudeOAuthBlock, claudeCodeCredentialPersister, error) {
	return claudeOAuthBlock{}, nil, errKeychainUnsupported
}

type keychainError string

func (e keychainError) Error() string { return string(e) }

const errKeychainUnsupported = keychainError("keychain: not supported on this platform")
