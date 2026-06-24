package llm

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"

	"github.com/tommoulard/atteler/pkg/permission"
	"github.com/tommoulard/atteler/pkg/shell"
)

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
	out, err := runSecurityCommand(ctx, []string{"find-generic-password", "-s", keychainService, "-w"}, nil)
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
	out, err := runSecurityCommand(ctx, []string{"find-generic-password", "-s", keychainService}, nil)
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

	args, stdin, err := buildKeychainWritebackCommand(p.account, string(updated))
	if err != nil {
		return fmt.Errorf("keychain update command: %w", err)
	}

	if out, err := runSecurityCommandStdin(ctx, args, stdin, []string{string(updated)}, shell.OutputSensitive); err != nil {
		return fmt.Errorf("keychain update failed: %w: %s", err, strings.TrimSpace(string(out)))
	}

	return nil
}

// buildKeychainWritebackCommand builds the security(1) argv and stdin payload
// for updating the keychain entry without exposing the credential JSON in the
// process argument list (argv is visible to every local process via ps for
// the lifetime of the command). It uses interactive mode: argv is just
// ["-i"], and the full add-generic-password command — including the -w
// secret — travels on stdin instead.
func buildKeychainWritebackCommand(account, secret string) (args []string, stdin string, err error) {
	tokens := []string{"add-generic-password", "-U", "-s", keychainService, "-w", secret}
	if account != "" {
		tokens = append(tokens, "-a", account)
	}

	quoted := make([]string, 0, len(tokens))
	for _, token := range tokens {
		q, qErr := quoteSecurityInteractiveToken(token)
		if qErr != nil {
			return nil, "", qErr
		}

		quoted = append(quoted, q)
	}

	return []string{"-i"}, strings.Join(quoted, " ") + "\n", nil
}

// quoteSecurityInteractiveToken quotes one token for security(1)'s
// interactive-mode tokenizer, which splits on whitespace and supports
// double-quoted tokens with backslash escapes for `\` and `"` (verified
// against the macOS security tool by round-tripping JSON payloads). Line
// breaks are rejected: they would terminate the command and inject another.
func quoteSecurityInteractiveToken(token string) (string, error) {
	if strings.ContainsAny(token, "\n\r") {
		return "", errSecurityTokenLineBreak
	}

	var b strings.Builder

	b.WriteByte('"')

	for i := range len(token) {
		c := token[i]
		if c == '\\' || c == '"' {
			b.WriteByte('\\')
		}

		b.WriteByte(c)
	}

	b.WriteByte('"')

	return b.String(), nil
}

var errSecurityTokenLineBreak = errors.New("security interactive token contains a line break")

func runSecurityCommand(ctx context.Context, args, secretArgs []string) ([]byte, error) {
	return runSecurityCommandStdin(ctx, args, "", secretArgs, shell.OutputSensitive)
}

func runSecurityCommandStdin(ctx context.Context, args []string, stdin string, secretArgs []string, capture shell.OutputCapture) ([]byte, error) {
	var stdout, stderr bytes.Buffer

	var stdinReader io.Reader
	if stdin != "" {
		stdinReader = strings.NewReader(stdin)
	}

	cmd, invocation, err := shell.CommandContext(ctx, shell.CommandOptions{
		Program:              "security",
		Args:                 args,
		Stdin:                stdinReader,
		Stdout:               &stdout,
		Stderr:               &stderr,
		Mode:                 shell.ModeCaptured,
		SecretValues:         secretArgs,
		PermissionOperations: securityCommandPermissionOperations(args),
		Audit:                shell.AuditContext{Caller: "atteler.keychain.security"},
	})
	if err != nil {
		return nil, fmt.Errorf("authorize security command: %w", err)
	}

	runErr := cmd.Run()
	if finishErr := invocation.Finish(shell.FinishOptions{
		Stdout:        stdout.String(),
		Stderr:        stderr.String(),
		Error:         runErr,
		OutputCapture: capture,
		OutputNote:    "macOS keychain output may contain credentials",
	}); finishErr != nil && runErr == nil {
		return nil, fmt.Errorf("audit security command: %w", finishErr)
	}

	if runErr != nil {
		return append(stdout.Bytes(), stderr.Bytes()...), runErr
	}

	return stdout.Bytes(), nil
}

func securityCommandPermissionOperations(args []string) []permission.Operation {
	if len(args) == 0 {
		return nil
	}

	action := "security " + strings.TrimSpace(args[0])
	ops := []permission.Operation{{
		Kind:   permission.OperationCredentialAccess,
		Action: action,
		Target: keychainService,
		Source: "atteler.keychain.security",
	}}

	switch strings.TrimSpace(args[0]) {
	case "add-generic-password", "delete-generic-password", "set-generic-password-partition-list", "-i":
		ops = append(ops, permission.Operation{
			Kind:   permission.OperationWrite,
			Action: action,
			Target: keychainService,
			Source: "atteler.keychain.security",
		})
	}

	return ops
}
