package retrieval

import (
	"regexp"
	"slices"
	"strings"
)

const (
	metadataTrue = "true"

	// MetadataSafetyInjectAllowed records whether indexed text may be injected into prompts.
	MetadataSafetyInjectAllowed = "retrieval.inject_allowed"
	// MetadataSafetyPrivate records whether indexed text came from a private source.
	MetadataSafetyPrivate = "retrieval.private"
	// MetadataSafetyRedacted records whether indexed text had sensitive values redacted.
	MetadataSafetyRedacted = "retrieval.redacted"
	// MetadataSafetySensitive records whether indexed text is credential-adjacent or sensitive.
	MetadataSafetySensitive = "retrieval.sensitive"
	// MetadataSafetyReasons stores semicolon-separated safety policy reasons.
	MetadataSafetyReasons = "retrieval.safety_reasons"
	// MetadataContentHash stores a stable short hash of sanitized indexed content.
	MetadataContentHash = "retrieval.content_hash"
	// MetadataStableID stores the retrieval contract's deterministic document ID.
	MetadataStableID = "retrieval.stable_id"
	// MetadataSourceUpdatedAt stores the source modification timestamp in RFC3339 format.
	MetadataSourceUpdatedAt = "retrieval.source_updated_at"
)

// PolicyContext gives the sanitizer source hints such as path and source type.
type PolicyContext struct {
	Source     Source
	Metadata   map[string]string
	DocumentID string
	Path       string
}

// Sanitize applies Atteler's default retrieval privacy policy. It redacts
// credential-shaped values before indexing and marks private/sensitive content
// as unsafe for direct prompt injection unless a caller explicitly overrides
// that decision.
func Sanitize(text string, ctx PolicyContext) (string, Safety) {
	safety := Safety{InjectAllowed: true}
	redacted := text

	for _, rule := range secretRules {
		if !rule.re.MatchString(redacted) {
			continue
		}

		redacted = rule.re.ReplaceAllString(redacted, rule.replacement)
		safety.Redacted = true
		safety.Sensitive = true
		safety.InjectAllowed = false
		safety.Reasons = appendReason(safety.Reasons, rule.reason)
	}

	if sensitivePath(ctx.Path) || sensitivePath(ctx.DocumentID) {
		safety.Private = true
		safety.Sensitive = true
		safety.InjectAllowed = false
		safety.Reasons = appendReason(safety.Reasons, "credential-adjacent path")
	}

	if ctx.Source.Type == SourceSession {
		// Session transcripts often include copied commands, logs, and credentials.
		// Redacted session text remains searchable, but callers must opt in before
		// injecting private transcript snippets into prompts.
		safety.Private = true
		safety.InjectAllowed = false
		safety.Reasons = appendReason(safety.Reasons, "private session transcript")
	}

	return redacted, safety
}

// SanitizeMetadata redacts credential-shaped metadata values and reports any
// safety implications. Keys that themselves describe credentials (for example
// api_key) are evaluated together with their value so bare secret values do not
// leak through retrieval result metadata.
func SanitizeMetadata(metadata map[string]string, ctx PolicyContext) (map[string]string, Safety) {
	safety := Safety{InjectAllowed: true}
	if len(metadata) == 0 {
		return nil, safety
	}

	out := make(map[string]string, len(metadata))
	for key, value := range metadata {
		sanitized, fieldSafety := sanitizeMetadataValue(key, value, ctx)
		out[key] = sanitized
		safety = MergeSafety(safety, fieldSafety)
	}

	return out, safety
}

func sanitizeMetadataValue(key, value string, ctx PolicyContext) (string, Safety) {
	fieldContext := ctx
	if isPathMetadataKey(key) {
		fieldContext.Path = value
	}

	combined := key + "=" + value

	sanitized, safety := Sanitize(combined, fieldContext)
	if value, ok := strings.CutPrefix(sanitized, key+"="); ok {
		return value, safety
	}

	return sanitized, safety
}

// SafetyFromMetadata reconstructs safety flags stored by indexers. Missing
// metadata defaults to injectable public text for backward compatibility.
func SafetyFromMetadata(metadata map[string]string) Safety {
	safety := Safety{InjectAllowed: true}
	if len(metadata) == 0 {
		return safety
	}

	injectAllowedValue, explicitInjectAllowed := metadata[MetadataSafetyInjectAllowed]
	if value, ok := metadata[MetadataSafetyInjectAllowed]; ok {
		safety.InjectAllowed = value == metadataTrue
	}

	safety.Private = metadata[MetadataSafetyPrivate] == metadataTrue
	safety.Redacted = metadata[MetadataSafetyRedacted] == metadataTrue
	safety.Sensitive = metadata[MetadataSafetySensitive] == metadataTrue

	if !explicitInjectAllowed && (safety.Private || safety.Redacted || safety.Sensitive) {
		safety.InjectAllowed = false
	}

	if reasons := strings.TrimSpace(metadata[MetadataSafetyReasons]); reasons != "" {
		safety.Reasons = strings.Split(reasons, ";")
	}

	if explicitInjectAllowed && injectAllowedValue != metadataTrue && IsZeroSafety(safety) {
		safety.Reasons = appendReason(safety.Reasons, "metadata disallows injection")
	}

	return safety
}

// NormalizeSafety treats an entirely omitted Safety value as the default
// public/injectable posture while preserving any explicit unsafe signals.
func NormalizeSafety(safety Safety) Safety {
	if IsZeroSafety(safety) {
		safety.InjectAllowed = true
	}

	return safety
}

// IsZeroSafety reports whether a Safety value was omitted by a legacy/custom
// backend. Retrieval treats this as unknown public text unless metadata says
// otherwise.
func IsZeroSafety(safety Safety) bool {
	return !safety.InjectAllowed &&
		!safety.Redacted &&
		!safety.Private &&
		!safety.Sensitive &&
		len(safety.Reasons) == 0
}

// IsDefaultSafety reports whether safety is the default public/injectable
// posture that does not need to be persisted as metadata.
func IsDefaultSafety(safety Safety) bool {
	return safety.InjectAllowed &&
		!safety.Redacted &&
		!safety.Private &&
		!safety.Sensitive &&
		len(safety.Reasons) == 0
}

// MergeSafety combines persisted and freshly evaluated safety flags. Unsafe
// decisions win over injectable defaults, and reasons are de-duplicated.
func MergeSafety(left, right Safety) Safety {
	left = NormalizeSafety(left)
	right = NormalizeSafety(right)

	left.InjectAllowed = left.InjectAllowed && right.InjectAllowed

	left.Redacted = left.Redacted || right.Redacted
	left.Private = left.Private || right.Private
	left.Sensitive = left.Sensitive || right.Sensitive

	for _, reason := range right.Reasons {
		if reason != "" {
			left.Reasons = appendReason(left.Reasons, reason)
		}
	}

	return left
}

// MergeSafetyMetadata stores safety flags in metadata for persisted indexes.
func MergeSafetyMetadata(metadata map[string]string, safety Safety) map[string]string {
	if metadata == nil {
		metadata = make(map[string]string)
	}

	if !safety.InjectAllowed && IsZeroSafety(safety) {
		safety.Reasons = appendReason(safety.Reasons, "metadata disallows injection")
	}

	metadata[MetadataSafetyInjectAllowed] = boolString(safety.InjectAllowed)
	if safety.Private {
		metadata[MetadataSafetyPrivate] = metadataTrue
	}

	if safety.Redacted {
		metadata[MetadataSafetyRedacted] = metadataTrue
	}

	if safety.Sensitive {
		metadata[MetadataSafetySensitive] = metadataTrue
	}

	if len(safety.Reasons) > 0 {
		metadata[MetadataSafetyReasons] = strings.Join(safety.Reasons, ";")
	}

	return metadata
}

type secretRule struct {
	re          *regexp.Regexp
	replacement string
	reason      string
}

var secretRules = []secretRule{
	{
		re:          regexp.MustCompile(`-----BEGIN [A-Z ]*PRIVATE KEY-----[\s\S]*?-----END [A-Z ]*PRIVATE KEY-----`),
		replacement: `[REDACTED PRIVATE KEY]`,
		reason:      "private key block",
	},
	{
		re:          regexp.MustCompile(`(?i)(\bauthorization\b\s*[:=]\s*)([^\r\n]+)`),
		replacement: `${1}[REDACTED]`,
		reason:      "authorization header",
	},
	{
		re:          regexp.MustCompile(`(?i)(bearer\s+)[a-z0-9._~+\-/=]{12,}`),
		replacement: `${1}[REDACTED]`,
		reason:      "bearer token",
	},
	{
		re:          regexp.MustCompile(`(?i)(["']?\b[a-z0-9_]*(?:api[_-]?key|access[_-]?token|auth[_-]?token|secret|password|passwd)[a-z0-9_]*\b["']?\s*[:=]\s*)(["']?)([^\s'\"\\,}]+)(["']?)`),
		replacement: `${1}${2}[REDACTED]${4}`,
		reason:      "credential-shaped assignment",
	},
}

var sensitivePathFragments = []string{
	".env",
	"id_rsa",
	"id_dsa",
	"id_ecdsa",
	"id_ed25519",
	"secret",
	"secrets",
	"credential",
	"credentials",
	"keychain",
	"token",
	"tokens",
}

func sensitivePath(path string) bool {
	path = strings.ToLower(strings.TrimSpace(path))
	if path == "" {
		return false
	}

	for _, fragment := range sensitivePathFragments {
		if strings.Contains(path, fragment) {
			return true
		}
	}

	return false
}

func appendReason(reasons []string, reason string) []string {
	if slices.Contains(reasons, reason) {
		return reasons
	}

	return append(reasons, reason)
}

func isPathMetadataKey(key string) bool {
	key = strings.ToLower(strings.TrimSpace(key))
	return key == "path" || strings.HasSuffix(key, "_path") || strings.HasSuffix(key, ".path")
}

func boolString(value bool) string {
	if value {
		return metadataTrue
	}

	return "false"
}
