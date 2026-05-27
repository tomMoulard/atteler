package eval

import (
	"regexp"
	"strings"
)

var (
	quotedSecretAssignmentPattern = regexp.MustCompile(`(?i)(["'][A-Za-z0-9_.-]*(api[_-]?key|access[_-]?key|access[_-]?token|auth[_-]?token|authorization|token|secret|password|passwd|private[_-]?key|credential|signature|jwt|cookie|session)[A-Za-z0-9_.-]*["'])(\s*[:=]\s*)(?:"[^"]*"|'[^']*'|[^\s,"'&}\]]+)`)
	secretAssignmentPattern       = regexp.MustCompile(`(?i)\b([A-Za-z0-9_.-]*(api[_-]?key|access[_-]?key|access[_-]?token|auth[_-]?token|authorization|token|secret|password|passwd|private[_-]?key|credential|signature|jwt|cookie|session)[A-Za-z0-9_.-]*)(\s*[:=]\s*)(?:"[^"]*"|'[^']*'|[^\s,"'&]+)`)
	authorizationPattern          = regexp.MustCompile(`(?i)\b(authorization)(\s*[:=]\s*)(["']?)(bearer|basic|token)\s+[^\s,"']+`)
	openAIKeyPattern              = regexp.MustCompile(`\bsk-[A-Za-z0-9_-]{8,}\b`)
	awsAccessKeyPattern           = regexp.MustCompile(`(?i)\b(?:A3T[A-Z0-9]|AKIA|ASIA|AGPA|AIDA|AROA|AIPA|ANPA)[A-Z0-9]{16}\b`)
	githubTokenPattern            = regexp.MustCompile(`\b(?:gh[opsu]_[A-Za-z0-9_]{20,}|github_pat_[A-Za-z0-9_]{20,})\b`)
	jwtPattern                    = regexp.MustCompile(`\beyJ[A-Za-z0-9_-]{10,}\.[A-Za-z0-9_-]{10,}\.[A-Za-z0-9_-]{10,}\b`)
	privateKeyBlockPattern        = regexp.MustCompile(`(?s)-----BEGIN [A-Z0-9 ]*PRIVATE KEY(?: BLOCK)?-----.*?(?:-----END [A-Z0-9 ]*PRIVATE KEY(?: BLOCK)?-----|$)`)
	urlUserinfoPattern            = regexp.MustCompile(`(?i)\b([a-z][a-z0-9+.-]*://)([^/?#\s"'<>@]+@)`)
)

// Redact removes common secret-looking values before snippets are written to
// human-readable failures or machine-readable eval reports.
func Redact(s string) string {
	redacted := privateKeyBlockPattern.ReplaceAllString(s, "[REDACTED PRIVATE KEY]")
	redacted = openAIKeyPattern.ReplaceAllString(redacted, "[REDACTED]")
	redacted = awsAccessKeyPattern.ReplaceAllString(redacted, "[REDACTED]")
	redacted = githubTokenPattern.ReplaceAllString(redacted, "[REDACTED]")
	redacted = jwtPattern.ReplaceAllString(redacted, "[REDACTED]")
	redacted = urlUserinfoPattern.ReplaceAllString(redacted, "$1[REDACTED]@")
	redacted = authorizationPattern.ReplaceAllString(redacted, "$1$2$3[REDACTED]")
	redacted = quotedSecretAssignmentPattern.ReplaceAllString(redacted, "$1$3[REDACTED]")

	redacted = secretAssignmentPattern.ReplaceAllString(redacted, "$1$3[REDACTED]")

	return strings.ReplaceAll(redacted, "[REDACTED]]", "[REDACTED]")
}
