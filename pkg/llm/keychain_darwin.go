package llm

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os/exec"
	"strings"
)

// keychainService is the macOS Keychain "service" attribute under which the
// claude CLI stores its OAuth credentials.
const keychainService = "Claude Code-credentials"

// claudeCodeKeychainSource is the diagnostic location string used when
// credentials originate from the macOS keychain.
const claudeCodeKeychainSource = "keychain:" + keychainService

// readClaudeCodeKeychain reads the Claude Code OAuth token from the macOS Keychain.
func readClaudeCodeKeychain(ctx context.Context) (string, error) {
	if err := requireCredentialContext(ctx); err != nil {
		return "", err
	}

	raw, err := readClaudeCodeKeychainPassword(ctx)
	if err != nil {
		return "", err
	}

	return parseClaudeCodeCredentials([]byte(raw))
}

// readClaudeCodeKeychainPassword runs `security find-generic-password -w` and
// returns the raw password blob (the JSON the claude CLI stored).
func readClaudeCodeKeychainPassword(ctx context.Context) (string, error) {
	out, err := exec.CommandContext(ctx,
		"security", "find-generic-password",
		"-s", keychainService,
		"-w",
	).Output()
	if err != nil {
		return "", fmt.Errorf("keychain lookup failed: %w", err)
	}

	raw := strings.TrimSpace(string(out))
	if raw == "" {
		return "", fmt.Errorf("keychain entry %q is empty", keychainService)
	}

	return raw, nil
}

// readClaudeCodeKeychainAccount reads the keychain entry's metadata and
// returns the account name ("acct") so callers can update the entry under the
// same account on write.
func readClaudeCodeKeychainAccount(ctx context.Context) (string, error) {
	out, err := exec.CommandContext(ctx,
		"security", "find-generic-password",
		"-s", keychainService,
	).Output()
	if err != nil {
		return "", fmt.Errorf("keychain metadata lookup failed: %w", err)
	}

	scanner := bufio.NewScanner(bytes.NewReader(out))
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if !strings.HasPrefix(line, `"acct"<blob>=`) {
			continue
		}

		// Format: "acct"<blob>="username"
		_, after, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}

		return strings.Trim(strings.TrimSpace(after), `"`), nil
	}

	return "", errors.New("keychain entry has no acct attribute")
}

// readClaudeCodeKeychainAuth reads the keychain entry, parses the OAuth block
// (allowing expired tokens — the caller will refresh), and returns a persister
// bound to the same keychain entry.
func readClaudeCodeKeychainAuth(ctx context.Context) (claudeOAuthBlock, claudeCodeCredentialPersister, error) {
	raw, err := readClaudeCodeKeychainPassword(ctx)
	if err != nil {
		return claudeOAuthBlock{}, nil, err
	}

	block, err := parseClaudeCodeCredentialsRaw([]byte(raw))
	if err != nil {
		return claudeOAuthBlock{}, nil, err
	}

	account, err := readClaudeCodeKeychainAccount(ctx)
	if err != nil {
		return claudeOAuthBlock{}, nil, err
	}

	return block, &claudeCodeKeychainPersister{account: account}, nil
}

// claudeCodeKeychainPersister updates the macOS keychain entry "Claude
// Code-credentials" in place, preserving any fields the claude CLI may have
// stored alongside the OAuth block.
type claudeCodeKeychainPersister struct {
	account string
}

func (p *claudeCodeKeychainPersister) location() string { return claudeCodeKeychainSource }

func (p *claudeCodeKeychainPersister) persist(ctx context.Context, accessToken, refreshToken string, expiresAtMs int64) error {
	current, err := readClaudeCodeKeychainPassword(ctx)
	if err != nil {
		return err
	}

	var raw map[string]any
	if parseErr := json.Unmarshal([]byte(current), &raw); parseErr != nil {
		return fmt.Errorf("keychain credentials parse: %w", parseErr)
	}

	if raw == nil {
		raw = map[string]any{}
	}

	mergeClaudeCodeOAuth(raw, accessToken, refreshToken, expiresAtMs)

	updated, err := json.Marshal(raw)
	if err != nil {
		return fmt.Errorf("keychain credentials marshal: %w", err)
	}

	args := []string{"add-generic-password", "-U", "-s", keychainService, "-w", string(updated)}
	if p.account != "" {
		args = append(args, "-a", p.account)
	}

	cmd := exec.CommandContext(ctx, "security", args...)

	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("keychain update failed: %w: %s", err, strings.TrimSpace(string(out)))
	}

	return nil
}
