// Package privacy centralizes the conservative redaction rules used before
// local memory persistence.
package privacy

import (
	"crypto/sha256"
	"encoding/hex"
	"regexp"
	"strings"
)

const (
	// RedactionPolicyVersion identifies the conservative local-memory
	// redaction rules used before persistence. Stores include this in
	// provenance so future migrations can tell which privacy policy shaped the
	// persisted text and source hashes.
	RedactionPolicyVersion = "atteler-redaction-v1"

	redacted = "[REDACTED]"
)

var (
	secretAssignments           = regexp.MustCompile(`(?i)\b([a-z0-9_-]*(?:password|passwd|pwd|api[_-]?key|auth[_-]?token|access[_-]?token|refresh[_-]?token|session[_-]?token|token|secret|private[_-]?key)[a-z0-9_-]*)\s*[:=]\s*("[^"]*"|'[^']*'|[^"'\\\s]+)`)
	quotedSecretAssignments     = regexp.MustCompile(`(?i)["']([a-z0-9_-]*(?:password|passwd|pwd|api[_-]?key|authorization|auth[_-]?token|access[_-]?token|refresh[_-]?token|session[_-]?token|token|secret|private[_-]?key)[a-z0-9_-]*)["']\s*:\s*("[^"]*"|'[^']*'|[^"'\\\s]+)`)
	identifierSecretAssignments = regexp.MustCompile(`(?i)\b([a-z0-9_-]*(?:password|passwd|pwd|api[_-]?key|auth[_-]?token|access[_-]?token|refresh[_-]?token|session[_-]?token|token|secret|private[_-]?key)[a-z0-9_-]*)\s*[:=]\s*("[^"]*"|'[^']*'|[^\s/?&#]+)`)
)

var sensitiveTextPatterns = []struct {
	pattern     *regexp.Regexp
	replacement string
}{
	{pattern: regexp.MustCompile(`(?is)-----BEGIN [A-Z0-9 ]*(?:PRIVATE|SECRET) KEY(?: BLOCK)?-----.*?-----END [A-Z0-9 ]*(?:PRIVATE|SECRET) KEY(?: BLOCK)?-----`), replacement: redacted},
	{pattern: regexp.MustCompile(`(?i)\bauthorization\s*[:=]\s*([a-z][a-z0-9._-]*\s+)?\S+`), replacement: `Authorization: ${1}` + redacted},
	{pattern: quotedSecretAssignments, replacement: `"$1":"` + redacted + `"`},
	{pattern: secretAssignments, replacement: `$1=` + redacted},
	{pattern: regexp.MustCompile(`(?i)\bbearer\s+[A-Za-z0-9._~+\-/]+=*`), replacement: `Bearer ` + redacted},
	{pattern: regexp.MustCompile(`\bsk-[A-Za-z0-9_-]{8,}\b`), replacement: redacted},
	{pattern: regexp.MustCompile(`\bgithub_pat_[A-Za-z0-9_]{20,}\b`), replacement: redacted},
	{pattern: regexp.MustCompile(`\bgh[pousr]_[A-Za-z0-9_]{20,}\b`), replacement: redacted},
	{pattern: regexp.MustCompile(`\bxox[baprs]-[A-Za-z0-9-]{10,}\b`), replacement: redacted},
	{pattern: regexp.MustCompile(`\bAIza[0-9A-Za-z_-]{35}\b`), replacement: redacted},
	{pattern: regexp.MustCompile(`\bAKIA[0-9A-Z]{16}\b`), replacement: redacted},
}

var sensitiveIdentifierPatterns = []struct {
	pattern     *regexp.Regexp
	replacement string
}{
	{pattern: regexp.MustCompile(`(?i)\bauthorization\s*[:=]\s*([a-z][a-z0-9._-]*\s+)?[^\s/?&#]+`), replacement: `Authorization: ${1}` + redacted},
	{pattern: identifierSecretAssignments, replacement: `$1=` + redacted},
	{pattern: regexp.MustCompile(`(?i)\bbearer\s+[A-Za-z0-9._~+\-=]+`), replacement: `Bearer ` + redacted},
	{pattern: regexp.MustCompile(`\bsk-[A-Za-z0-9_-]{8,}\b`), replacement: redacted},
	{pattern: regexp.MustCompile(`\bgithub_pat_[A-Za-z0-9_]{20,}\b`), replacement: redacted},
	{pattern: regexp.MustCompile(`\bgh[pousr]_[A-Za-z0-9_]{20,}\b`), replacement: redacted},
	{pattern: regexp.MustCompile(`\bxox[baprs]-[A-Za-z0-9-]{10,}\b`), replacement: redacted},
	{pattern: regexp.MustCompile(`\bAIza[0-9A-Za-z_-]{35}\b`), replacement: redacted},
	{pattern: regexp.MustCompile(`\bAKIA[0-9A-Z]{16}\b`), replacement: redacted},
}

// RedactText removes common token, key, password, and Authorization bearer
// values before text is written to local memory stores. It intentionally keeps
// labels (for example, "token") so search can still explain why redaction
// happened without retaining the secret value.
func RedactText(text string) string {
	out := text
	for _, rule := range sensitiveTextPatterns {
		out = rule.pattern.ReplaceAllString(out, rule.replacement)
	}

	return out
}

// RedactIdentifier removes common secrets from paths and document IDs while
// preserving structural separators. This keeps derived IDs such as
// "session/access_token=.../message/0" distinct after redaction instead of
// letting a greedy secret value consume the trailing path suffix.
func RedactIdentifier(identifier string) string {
	out := identifier
	for _, rule := range sensitiveIdentifierPatterns {
		out = rule.pattern.ReplaceAllString(out, rule.replacement)
	}

	return out
}

// RedactMetadata returns a defensive copy of metadata with sensitive values
// redacted. Empty values are omitted.
func RedactMetadata(metadata map[string]string) map[string]string {
	if len(metadata) == 0 {
		return nil
	}

	out := make(map[string]string, len(metadata))
	for key, value := range metadata {
		key = strings.TrimSpace(key)
		value = strings.TrimSpace(value)

		if key == "" || value == "" {
			continue
		}

		key = RedactIdentifier(key)

		if IsSensitiveKey(key) {
			out[key] = redacted
			continue
		}

		if IsIdentifierKey(key) {
			out[key] = RedactIdentifier(value)
			continue
		}

		out[key] = RedactText(value)
	}

	if len(out) == 0 {
		return nil
	}

	return out
}

// IsIdentifierKey reports whether a metadata value is expected to be a path,
// URL, ID, or reference where secret redaction should preserve separators such
// as '/', '?', and '&' instead of treating the entire suffix as one prose token.
func IsIdentifierKey(key string) bool {
	key = strings.ToLower(strings.TrimSpace(key))
	key = strings.ReplaceAll(key, "-", "_")

	switch key {
	case "agent", "branch", "commit", "id", "model", "path", "file", "ref", "uri", "url", "reference":
		return true
	}

	for _, suffix := range []string{
		"_agent",
		"_branch",
		"_commit",
		"_id",
		"_model",
		"_path",
		"_file",
		"_ref",
		"_uri",
		"_url",
		"_reference",
	} {
		if strings.HasSuffix(key, suffix) {
			return true
		}
	}

	return false
}

// IsSensitiveKey reports whether a metadata key should never retain its raw
// value in persisted memory.
func IsSensitiveKey(key string) bool {
	key = strings.ToLower(strings.TrimSpace(key))
	key = strings.ReplaceAll(key, "-", "_")

	for _, marker := range []string{"password", "passwd", "pwd", "secret", "token", "api_key", "apikey", "private_key", "authorization"} {
		if strings.Contains(key, marker) {
			return true
		}
	}

	return false
}

// SourceHash returns a stable hash for the already-redacted source text. Memory
// stores use this to detect manual store edits or stale embedding payloads
// without retaining fingerprints of raw secrets.
func SourceHash(text string) string {
	sum := sha256.Sum256([]byte(text))

	return "sha256:" + hex.EncodeToString(sum[:])
}
