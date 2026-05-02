package llm

import (
	"context"
	"fmt"
	"os/exec"
	"strings"
)

const keychainService = "Claude Code-credentials"

// readClaudeCodeKeychain reads the Claude Code OAuth token from the macOS Keychain.
func readClaudeCodeKeychain(ctx context.Context) (string, error) {
	ctx = nonNilCredentialContext(ctx)
	out, err := exec.CommandContext(ctx,
		"security", "find-generic-password",
		"-s", keychainService,
		"-w", // print password only
	).Output()
	if err != nil {
		return "", fmt.Errorf("keychain lookup failed: %w", err)
	}

	raw := strings.TrimSpace(string(out))
	if raw == "" {
		return "", fmt.Errorf("keychain entry %q is empty", keychainService)
	}

	return parseClaudeCodeCredentials([]byte(raw))
}
