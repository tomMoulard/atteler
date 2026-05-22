//nolint:wsl_v5 // Diagnostic redaction code keeps short guard branches together.
package lsp

import (
	"bytes"
	"regexp"
	"strings"
	"sync"
)

var diagnosticSecretAssignmentPattern = regexp.MustCompile(`(?i)\b(api[_-]?key|password|secret|token)=\S+`)

//nolint:govet // Field order keeps mutex-protected buffer state together.
type diagnosticBuffer struct {
	mu        sync.Mutex
	buf       bytes.Buffer
	maxBytes  int
	secrets   []string
	truncated bool
}

func newDiagnosticBuffer(maxBytes int, secrets []string) *diagnosticBuffer {
	return &diagnosticBuffer{maxBytes: maxBytes, secrets: append([]string(nil), secrets...)}
}

func (b *diagnosticBuffer) Write(p []byte) (int, error) {
	if b == nil {
		return len(p), nil
	}

	b.mu.Lock()
	defer b.mu.Unlock()

	if b.maxBytes <= 0 {
		b.truncated = true
		return len(p), nil
	}

	remaining := b.maxBytes - b.buf.Len()
	if remaining <= 0 {
		b.truncated = true
		return len(p), nil
	}

	if len(p) > remaining {
		_, _ = b.buf.Write(p[:remaining])
		b.truncated = true
		return len(p), nil
	}

	_, _ = b.buf.Write(p)

	return len(p), nil
}

func (b *diagnosticBuffer) WriteString(value string) {
	if b == nil || strings.TrimSpace(value) == "" {
		return
	}

	if _, err := b.Write([]byte(value)); err != nil {
		return
	}
}

func (b *diagnosticBuffer) String() (string, bool) {
	if b == nil {
		return "", false
	}

	b.mu.Lock()
	defer b.mu.Unlock()

	return redactDiagnostics(b.buf.String(), b.secrets), b.truncated
}

func secretValuesFromEnv(env []string) []string {
	secrets := make([]string, 0)
	for _, assignment := range env {
		name, value, ok := strings.Cut(assignment, "=")
		if !ok || strings.TrimSpace(value) == "" {
			continue
		}

		if isSecretName(name) {
			secrets = append(secrets, value)
		}
	}

	return secrets
}

func isSecretName(name string) bool {
	name = strings.ToLower(strings.TrimSpace(name))
	return strings.Contains(name, "token") || strings.Contains(name, "secret") || strings.Contains(name, "password") || strings.Contains(name, "api_key") || strings.Contains(name, "apikey")
}

func redactDiagnostics(value string, secrets []string) string {
	redacted := value
	for _, secret := range secrets {
		if secret == "" {
			continue
		}

		redacted = strings.ReplaceAll(redacted, secret, "[REDACTED]")
	}

	return diagnosticSecretAssignmentPattern.ReplaceAllString(redacted, "$1=[REDACTED]")
}
