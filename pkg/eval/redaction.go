package eval

import "regexp"

var (
	secretAssignmentPattern = regexp.MustCompile(`(?i)\b(api[_-]?key|access[_-]?token|auth[_-]?token|token|secret|password)(\s*[:=]\s*)(["']?)[^\s,"']+`)
	authorizationPattern    = regexp.MustCompile(`(?i)\b(authorization)(\s*[:=]\s*)(["']?)(bearer|basic|token)\s+[^\s,"']+`)
	openAIKeyPattern        = regexp.MustCompile(`\bsk-[A-Za-z0-9_-]{8,}\b`)
)

// Redact removes common secret-looking values before snippets are written to
// human-readable failures or machine-readable eval reports.
func Redact(s string) string {
	redacted := openAIKeyPattern.ReplaceAllString(s, "[REDACTED]")
	redacted = authorizationPattern.ReplaceAllString(redacted, "$1$2$3[REDACTED]")

	return secretAssignmentPattern.ReplaceAllString(redacted, "$1$2$3[REDACTED]")
}
