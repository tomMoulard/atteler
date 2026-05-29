package events

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"regexp"
	"sort"
	"strings"
	"unicode"
	"unicode/utf8"
)

const (
	maxHookPayloadBytes   = 64 * 1024
	maxContentBytes       = 8 * 1024
	maxErrorBytes         = 2 * 1024
	maxMetadataEntries    = 64
	maxMetadataKeyBytes   = 64
	maxMetadataValueBytes = 8 * 1024
	maxScalarBytes        = 256
	maxSummaryBytes       = 256
	maxSecretScanBytes    = maxContentBytes

	truncationMarker = "...[truncated]"
	redactedValue    = "[redacted]"
)

type metadataPolicy int

const (
	metadataSafe metadataPolicy = iota
	metadataSensitive
)

type eventSchema struct {
	Metadata    map[string]metadataPolicy
	SessionID   bool
	Agent       bool
	Model       bool
	Role        bool
	Content     bool
	Error       bool
	SessionPath bool
}

var eventSchemas = map[string]eventSchema{
	AgentExecute: lifecycleEventSchema(eventSchema{
		Metadata: map[string]metadataPolicy{
			"agent": metadataSafe,
		},
	}),
	AssistantMessage: lifecycleEventSchema(eventSchema{
		Content: true,
		Role:    true,
	}),
	CommandExecute: lifecycleEventSchema(eventSchema{
		Content: true,
		Metadata: map[string]metadataPolicy{
			"count":              metadataSafe,
			"source":             metadataSafe,
			"mode":               metadataSafe,
			"model_mode":         metadataSafe,
			"tool_call_id":       metadataSafe,
			"provider":           metadataSafe,
			"service_tier":       metadataSafe,
			"waves":              metadataSafe,
			"option_adjustments": metadataSafe,
			"command":            metadataSensitive,
			"cwd":                metadataSensitive,
			"input":              metadataSensitive,
		},
	}),
	CommandOutput: lifecycleEventSchema(eventSchema{
		Content: true,
		Error:   true,
		Metadata: map[string]metadataPolicy{
			"partial":      metadataSafe,
			"sequence":     metadataSafe,
			"source":       metadataSafe,
			"stream":       metadataSafe,
			"tool_call_id": metadataSafe,
			"command":      metadataSensitive,
			"cwd":          metadataSensitive,
		},
	}),
	ContextAdd: lifecycleEventSchema(eventSchema{
		Metadata: fileReferenceMetadataSchema(),
	}),
	ContextManifest: lifecycleEventSchema(eventSchema{
		Metadata: map[string]metadataPolicy{
			"configured_reference_entry_count":      metadataSafe,
			"context_manifest":                      metadataSafe,
			"estimated_token_error_bound":           metadataSafe,
			"estimated_token_upper_bound":           metadataSafe,
			"estimated_tokens":                      metadataSafe,
			"fallback_model_count":                  metadataSafe,
			"fits_configured_token_budget":          metadataSafe,
			"fits_model_context_window":             metadataSafe,
			"included_reference_count":              metadataSafe,
			"inline_reference_count":                metadataSafe,
			"input_budget_checked":                  metadataSafe,
			"max_input_tokens":                      metadataSafe,
			"message_count":                         metadataSafe,
			"model_context_window":                  metadataSafe,
			"model_context_window_checked":          metadataSafe,
			"omitted_reference_count":               metadataSafe,
			"background_suggestion":                 metadataSafe,
			"context_summary":                       metadataSafe,
			"reference_bytes":                       metadataSafe,
			"reference_estimated_token_error_bound": metadataSafe,
			"reference_estimated_token_upper_bound": metadataSafe,
			"reference_estimated_tokens":            metadataSafe,
			"request_kind":                          metadataSafe,
			"rejected_reference_count":              metadataSafe,
			"schema_version":                        metadataSafe,
			"skipped_reference_count":               metadataSafe,
			"token_estimator":                       metadataSafe,
			"truncated_reference_count":             metadataSafe,
		},
	}),
	Error: lifecycleEventSchema(eventSchema{
		Error: true,
	}),
	FileRead: lifecycleEventSchema(eventSchema{
		Metadata: fileReferenceMetadataSchema(),
	}),
	FileWrite: lifecycleEventSchema(eventSchema{
		Metadata: map[string]metadataPolicy{
			"kind": metadataSafe,
			"path": metadataSensitive,
		},
	}),
	RouteDecision: lifecycleEventSchema(eventSchema{
		Content: true,
		Metadata: map[string]metadataPolicy{
			"actual_cached_input_tokens":      metadataSafe,
			"actual_cache_write_tokens":       metadataSafe,
			"actual_cost":                     metadataSafe,
			"actual_cost_delta":               metadataSafe,
			"actual_input_tokens":             metadataSafe,
			"actual_latency_ms":               metadataSafe,
			"actual_output_tokens":            metadataSafe,
			"actual_selected":                 metadataSafe,
			"actual_ttft_ms":                  metadataSafe,
			"availability_checked":            metadataSafe,
			"availability_refresh_attempted":  metadataSafe,
			"availability_refresh_timeout_ms": metadataSafe,
			"budget":                          metadataSafe,
			"candidate_count":                 metadataSafe,
			"catalog_stale":                   metadataSafe,
			"catalog_version":                 metadataSafe,
			"constraints":                     metadataSafe,
			"estimated_cache_write_tokens":    metadataSafe,
			"estimated_cost":                  metadataSafe,
			"estimated_input_tokens":          metadataSafe,
			"estimated_output_tokens":         metadataSafe,
			"fallback_order":                  metadataSafe,
			"max_output_tokens":               metadataSafe,
			"model_count":                     metadataSafe,
			"phase":                           metadataSafe,
			"prompt_cache_reuse_estimate":     metadataSafe,
			"provider_count":                  metadataSafe,
			"provider_model_count":            metadataSafe,
			"rejected_count":                  metadataSafe,
			"selected":                        metadataSafe,
			"unavailable_count":               metadataSafe,
			"unverified_count":                metadataSafe,
			"verified_provider_model_count":   metadataSafe,
			"warning_count":                   metadataSafe,
		},
	}),
	SessionEnd: lifecycleEventSchema(eventSchema{
		Metadata: map[string]metadataPolicy{
			"agent_loop_budget": metadataSafe,
			"model_mode":        metadataSafe,
			"reasoning_level":   metadataSafe,
		},
	}),
	SessionStart: lifecycleEventSchema(eventSchema{
		Metadata: map[string]metadataPolicy{
			"agent_loop_budget": metadataSafe,
			"model_mode":        metadataSafe,
			"reasoning_level":   metadataSafe,
		},
	}),
	ToolExecute: lifecycleEventSchema(eventSchema{
		Metadata: map[string]metadataPolicy{
			"model_mode":         metadataSafe,
			"option_adjustments": metadataSafe,
			"provider":           metadataSafe,
			"reasoning_level":    metadataSafe,
			"service_tier":       metadataSafe,
			"tool":               metadataSafe,
		},
	}),
	UserMessage: lifecycleEventSchema(eventSchema{
		Content: true,
		Role:    true,
		Metadata: map[string]metadataPolicy{
			"context_references": metadataSensitive,
		},
	}),
}

var secretPatterns = []*regexp.Regexp{
	regexp.MustCompile(`(?i)(bearer\s+)[A-Za-z0-9._~+/\-]+=*`),
	regexp.MustCompile(`(?i)(basic\s+)[A-Za-z0-9._~+/\-]+=*`),
	regexp.MustCompile(`(?i)([A-Z0-9_.-]*authorization[A-Z0-9_.-]*\s*[:=]\s*(?:token|apikey|api-key|oauth|bearer|basic)\s+)[A-Za-z0-9._~+/\-]+=*`),
	regexp.MustCompile(`(?i)((?:set-cookie|cookie)\s*[:=]\s*)[^\r\n]+`),
	regexp.MustCompile(`(?i)((?:session[_.-]?id|sessionid|csrf[_.-]?token|xsrf[_.-]?token)\s*[:=]\s*)(?:"[^"]+"|'[^']+'|[^\s,;}]+)`),
	regexp.MustCompile(`(?i)((?:^|[?&;\s])sig\s*=\s*)(?:"[^"]+"|'[^']+'|[^\s&;]+)`),
	regexp.MustCompile(`(?i)((?:api\s+key|access\s+token|refresh\s+token|id\s+token|oauth\s+token|personal\s+access\s+token|client\s+secret|secret\s+key|private\s+key)\s*[:=]\s*)(?:"[^"]+"|'[^']+'|[^\s,;}]+)`),
	regexp.MustCompile(`(?i)((?:\\?["'][A-Z0-9_.-]*(?:api[_.-]?key|api[_.-]?token|access[_.-]?token|refresh[_.-]?token|id[_.-]?token|oauth[_.-]?token|session[_.-]?token|csrf[_.-]?token|xsrf[_.-]?token|auth[_.-]?token|personal[_.-]?access[_.-]?token|secret|password|passwd|pwd|authorization|credential|access[_.-]?key|account[_.-]?key|accountkey|private[_.-]?key|signature|webhook)[A-Z0-9_.-]*\\?["'])\s*[:=]\s*)(?:\\?["'][^"']+\\?["']|[^\s,;}]+)`),
	regexp.MustCompile(`(?i)((?:"[A-Z0-9_.-]*(?:api[_.-]?key|api[_.-]?token|access[_.-]?token|refresh[_.-]?token|id[_.-]?token|oauth[_.-]?token|session[_.-]?token|csrf[_.-]?token|xsrf[_.-]?token|auth[_.-]?token|personal[_.-]?access[_.-]?token|secret|password|passwd|pwd|authorization|credential|access[_.-]?key|account[_.-]?key|accountkey|private[_.-]?key|signature|webhook)[A-Z0-9_.-]*"|'[A-Z0-9_.-]*(?:api[_.-]?key|api[_.-]?token|access[_.-]?token|refresh[_.-]?token|id[_.-]?token|oauth[_.-]?token|session[_.-]?token|csrf[_.-]?token|xsrf[_.-]?token|auth[_.-]?token|personal[_.-]?access[_.-]?token|secret|password|passwd|pwd|authorization|credential|access[_.-]?key|account[_.-]?key|accountkey|private[_.-]?key|signature|webhook)[A-Z0-9_.-]*'|[A-Z0-9_.-]*(?:api[_.-]?key|api[_.-]?token|access[_.-]?token|refresh[_.-]?token|id[_.-]?token|oauth[_.-]?token|session[_.-]?token|csrf[_.-]?token|xsrf[_.-]?token|auth[_.-]?token|personal[_.-]?access[_.-]?token|secret|password|passwd|pwd|authorization|credential|access[_.-]?key|account[_.-]?key|accountkey|private[_.-]?key|signature|webhook)[A-Z0-9_.-]*)\s*[:=]\s*)(?:"[^"]+"|'[^']+'|[^\s,;}]+)`),
	regexp.MustCompile(`sk-[A-Za-z0-9][A-Za-z0-9_\-]{10,}`),
	regexp.MustCompile(`gl(?:pat|rt)-[A-Za-z0-9_\-]{20,}`),
	regexp.MustCompile(`github_pat_[A-Za-z0-9_]{20,}`),
	regexp.MustCompile(`gh[pousr]_[A-Za-z0-9_]{20,}`),
	regexp.MustCompile(`npm_[A-Za-z0-9]{36,}`),
	regexp.MustCompile(`pypi-[A-Za-z0-9_\-]{20,}`),
	regexp.MustCompile(`xox[baprs]-[A-Za-z0-9\-]{10,}`),
	regexp.MustCompile(`(?:AKIA|ASIA)[0-9A-Z]{16}`),
	regexp.MustCompile(`AIza[0-9A-Za-z_\-]{20,}`),
	regexp.MustCompile(`ya29\.[0-9A-Za-z_\-]{20,}`),
	regexp.MustCompile(`(?:sk|rk)_(?:live|test)_[0-9A-Za-z]{16,}`),
	regexp.MustCompile(`SG\.[A-Za-z0-9_\-]{16,}\.[A-Za-z0-9_\-]{16,}`),
	regexp.MustCompile(`https://hooks\.slack\.com/services/[A-Za-z0-9/_\-]+`),
	regexp.MustCompile(`https://(?:discord(?:app)?|canary\.discord)\.com/api/webhooks/[0-9]+/[A-Za-z0-9._\-]+`),
	regexp.MustCompile(`eyJ[A-Za-z0-9_\-]+\.[A-Za-z0-9_\-]+\.[A-Za-z0-9_\-]+`),
	regexp.MustCompile(`(?i)[a-z][a-z0-9+.-]*://[^\s/@:]+:[^\s/@]+@`),
	regexp.MustCompile(`(?is)-----BEGIN [A-Z0-9 ]*PRIVATE KEY-----.*?(?:-----END [A-Z0-9 ]*PRIVATE KEY-----|$)`),
}

var secretKeyPattern = regexp.MustCompile(`(?i)(api[_.-]?\s*key|api[_.-]?\s*token|access[_.-]?\s*token|refresh[_.-]?\s*token|id[_.-]?\s*token|oauth[_.-]?\s*token|session[_.-]?\s*token|csrf[_.-]?\s*token|xsrf[_.-]?\s*token|auth[_.-]?\s*token|personal[_.-]?\s*access[_.-]?\s*token|secret|password|passwd|pwd|authorization|credential|access[_.-]?\s*key|account[_.-]?\s*key|accountkey|private[_.-]?\s*key|signature|webhook)`)

func fileReferenceMetadataSchema() map[string]metadataPolicy {
	return map[string]metadataPolicy{
		"kind":      metadataSafe,
		"bytes":     metadataSafe,
		"truncated": metadataSafe,
		"path":      metadataSensitive,
	}
}

func lifecycleEventSchema(schema eventSchema) eventSchema {
	schema.SessionID = true
	schema.Agent = true
	schema.Model = true
	schema.SessionPath = true

	return schema
}

func normalizePayloadMode(mode string) PayloadMode {
	switch PayloadMode(strings.ToLower(strings.TrimSpace(mode))) {
	case PayloadSummary:
		return PayloadSummary
	case PayloadFull:
		return PayloadFull
	default:
		return PayloadMetadata
	}
}

func sanitizeEventForHook(event Event, mode PayloadMode) Event {
	mode = normalizePayloadMode(string(mode))

	schema, known := eventSchemas[event.Type]
	if !known {
		schema = eventSchema{}
	}

	out := Event{
		Timestamp:   event.Timestamp,
		PayloadMode: string(mode),
		Redacted:    event.Redacted,
		Truncated:   event.Truncated,
	}

	out.Type = sanitizeEventType(&out, event.Type)

	copySafeScalars(&out, event, schema, mode, known)

	metadata, metadataRedacted, metadataTruncated := sanitizeMetadata(event.Metadata, schema, mode)
	out.Metadata = metadata
	out.Redacted = out.Redacted || metadataRedacted
	out.Truncated = out.Truncated || metadataTruncated

	applySensitiveField(&out, event.Content, schema.Content, mode, "content", maxContentBytes, &out.Content, &out.ContentSummary)
	applySensitiveField(&out, event.Error, schema.Error, mode, "error", maxErrorBytes, &out.Error, &out.ErrorSummary)

	if event.ContentSummary != "" || event.ErrorSummary != "" {
		out.Redacted = true
	}

	if data, err := json.Marshal(out); err == nil && len(data)+1 > maxHookPayloadBytes {
		out = shrinkOversizedPayload(out)
	}

	return out
}

func sanitizeEventForLog(event Event) Event {
	out := sanitizeEventForHook(event, PayloadMetadata)

	out.PayloadMode = ""
	if event.Error != "" {
		out.ErrorSummary = sensitiveSummary("error", event.Error)
		out.Redacted = true
	} else if event.ErrorSummary != "" {
		out.ErrorSummary = sensitiveSummary("error", event.ErrorSummary)
		out.Redacted = true
	}

	out.Redacted = out.Redacted || event.Redacted
	out.Truncated = out.Truncated || event.Truncated

	return out
}

func copySafeScalars(out *Event, event Event, schema eventSchema, mode PayloadMode, known bool) {
	copyOptionalScalar(out, &out.SessionID, schema.SessionID, "session_id", event.SessionID)
	copyOptionalScalar(out, &out.Agent, schema.Agent, "agent", event.Agent)
	copyOptionalScalar(out, &out.Model, schema.Model, "model", event.Model)
	copyOptionalScalar(out, &out.Role, schema.Role, "role", event.Role)

	if known && schema.SessionPath && mode == PayloadFull && event.SessionPath != "" {
		setSanitizedScalar(out, &out.SessionPath, "session_path", event.SessionPath, maxMetadataValueBytes)
	} else if event.SessionPath != "" {
		out.Redacted = true
	}
}

func copyOptionalScalar(out *Event, field *string, allowed bool, key, value string) {
	if value == "" {
		return
	}

	if !allowed {
		out.Redacted = true

		return
	}

	setSanitizedScalar(out, field, key, value, maxScalarBytes)
}

func applySensitiveField(
	out *Event,
	value string,
	allowed bool,
	mode PayloadMode,
	label string,
	maxBytes int,
	full *string,
	summary *string,
) {
	if value == "" {
		return
	}

	if !allowed {
		out.Redacted = true

		return
	}

	switch mode {
	case PayloadFull:
		var redacted, truncated bool

		*full, redacted, truncated = sanitizeValue(label, value, maxBytes)
		out.Redacted = out.Redacted || redacted
		out.Truncated = out.Truncated || truncated
	case PayloadSummary:
		*summary = sensitiveSummary(label, value)
		out.Redacted = true
	default:
		out.Redacted = true
	}
}

func setSanitizedScalar(out *Event, field *string, key, value string, maxBytes int) {
	var redacted, truncated bool

	*field, redacted, truncated = sanitizeValue(key, value, maxBytes)
	out.Redacted = out.Redacted || redacted
	out.Truncated = out.Truncated || truncated
}

func sanitizeEventType(out *Event, value string) string {
	safe, redacted, truncated := sanitizeValue("type", value, maxScalarBytes)
	out.Redacted = out.Redacted || redacted
	out.Truncated = out.Truncated || truncated

	var builder strings.Builder

	for _, r := range safe {
		if isEventTypeRune(r) {
			builder.WriteRune(r)
			continue
		}

		builder.WriteByte('_')

		out.Redacted = true
	}

	if builder.Len() == 0 {
		out.Redacted = true

		return "event"
	}

	return builder.String()
}

func isEventTypeRune(r rune) bool {
	return (r >= 'a' && r <= 'z') ||
		(r >= 'A' && r <= 'Z') ||
		(r >= '0' && r <= '9') ||
		r == '_' ||
		r == '-' ||
		r == '.' ||
		r == ':' ||
		r == '[' ||
		r == ']'
}

func sanitizeMetadata(metadata map[string]string, schema eventSchema, mode PayloadMode) (sanitized map[string]string, redacted, truncated bool) {
	if len(metadata) == 0 || len(schema.Metadata) == 0 {
		return nil, len(metadata) > 0, false
	}

	keys := make([]string, 0, len(schema.Metadata))
	for key := range schema.Metadata {
		keys = append(keys, key)
	}

	sort.Strings(keys)

	out := make(map[string]string)
	consumed := 0

	for _, canonical := range keys {
		raw, ok := metadata[canonical]
		if !ok {
			continue
		}

		consumed++

		if len(out) >= maxMetadataEntries {
			redacted = true
			truncated = true

			break
		}

		policy := schema.Metadata[canonical]
		if policy == metadataSensitive && mode != PayloadFull {
			redacted = true
			continue
		}

		safeKey, keyRedacted, keyTruncated := sanitizeMetadataKey(canonical)
		value, valueRedacted, valueTruncated := sanitizeMetadataValue(safeKey, raw, policy)
		out[safeKey] = value
		redacted = redacted || keyRedacted || valueRedacted
		truncated = truncated || keyTruncated || valueTruncated
	}

	if len(metadata) > consumed {
		redacted = true
	}

	if len(out) == 0 {
		return nil, redacted, truncated
	}

	return out, redacted, truncated
}

func sanitizeMetadataKey(key string) (safeKey string, redacted, truncated bool) {
	out, truncated := truncateUTF8(strings.ToValidUTF8(key, ""), maxMetadataKeyBytes)
	if out == "" {
		out = "metadata"
	}

	return out, secretKeyPattern.MatchString(out), truncated
}

func sanitizeMetadataValue(key, value string, policy metadataPolicy) (safe string, redacted, truncated bool) {
	if policy == metadataSafe && containsLocalPath(value) {
		return redactedValue, true, false
	}

	return sanitizeValue(key, value, maxMetadataValueBytes)
}

func containsLocalPath(value string) bool {
	value, _ = boundedValidPrefix(value, maxMetadataValueBytes)

	if looksLikeLocalPath(value) {
		return true
	}

	for token := range strings.FieldsFuncSeq(value, isPathTokenSeparator) {
		token = strings.Trim(token, `"'`)
		if token == "" || looksLikeRemoteURL(token) {
			continue
		}

		if looksLikeLocalPath(token) || tokenContainsLocalPath(token) {
			return true
		}
	}

	return false
}

func isPathTokenSeparator(r rune) bool {
	return unicode.IsSpace(r) || strings.ContainsRune(",;()[]{}", r)
}

func tokenContainsLocalPath(token string) bool {
	if _, suffix, ok := strings.Cut(token, "="); ok && looksLikeLocalPath(suffix) {
		return true
	}

	if _, suffix, ok := strings.Cut(token, ":"); ok {
		if strings.HasPrefix(strings.ToLower(token), "file:") {
			return true
		}

		if !isWindowsDrivePath(token) && looksLikeLocalPath(suffix) {
			return true
		}
	}

	return false
}

func looksLikeRemoteURL(value string) bool {
	lower := strings.ToLower(value)

	return strings.HasPrefix(lower, "http://") ||
		strings.HasPrefix(lower, "https://") ||
		strings.HasPrefix(lower, "ssh://") ||
		strings.HasPrefix(lower, "git://")
}

func looksLikeLocalPath(value string) bool {
	value, _ = boundedValidPrefix(value, maxMetadataValueBytes)

	value = strings.TrimSpace(value)
	if value == "" {
		return false
	}

	lower := strings.ToLower(value)
	if strings.HasPrefix(lower, "file:") {
		return true
	}

	switch {
	case strings.HasPrefix(value, "/"),
		strings.HasPrefix(value, "~/"),
		strings.HasPrefix(value, "./"),
		strings.HasPrefix(value, "../"),
		strings.HasPrefix(value, `\`):
		return true
	}

	return isWindowsDrivePath(value)
}

func isWindowsDrivePath(value string) bool {
	return len(value) >= 3 &&
		((value[0] >= 'A' && value[0] <= 'Z') || (value[0] >= 'a' && value[0] <= 'z')) &&
		value[1] == ':' &&
		(value[2] == '\\' || value[2] == '/')
}

func sanitizeScalar(key, value string) string {
	out, _, _ := sanitizeValue(key, value, maxScalarBytes)

	return out
}

func sanitizeValue(key, value string, maxBytes int) (safe string, redacted, truncated bool) {
	if value == "" {
		return "", false, false
	}

	if secretKeyPattern.MatchString(key) {
		return redactedValue, true, false
	}

	truncated = maxBytes > 0 && len(value) > maxBytes

	scanBytes := maxBytes + maxSecretScanBytes
	if scanBytes <= 0 {
		scanBytes = maxSecretScanBytes
	}

	value, scanTruncated := boundedValidPrefix(value, scanBytes)
	truncated = truncated || scanTruncated

	for _, pattern := range secretPatterns {
		next := pattern.ReplaceAllStringFunc(value, func(match string) string {
			redacted = true

			if prefix, ok := urlCredentialPrefix(match); ok {
				return prefix + redactedValue + "@"
			}

			if prefix, ok := authSchemePrefix(match); ok {
				return prefix + redactedValue
			}

			if isSecretURL(match) {
				return redactedValue
			}

			if idx := strings.IndexAny(match, "=:"); idx >= 0 {
				return strings.TrimRight(match[:idx+1], " \t") + redactedValue
			}

			return redactedValue
		})
		value = next
	}

	out, finalTruncated := truncateUTF8(value, maxBytes)
	truncated = truncated || finalTruncated

	return out, redacted, truncated
}

func isSecretURL(match string) bool {
	lower := strings.ToLower(match)

	return strings.HasPrefix(lower, "http://") ||
		strings.HasPrefix(lower, "https://") ||
		strings.HasPrefix(lower, "ssh://") ||
		strings.HasPrefix(lower, "git://")
}

func urlCredentialPrefix(match string) (string, bool) {
	schemeEnd := strings.Index(match, "://")

	at := strings.LastIndex(match, "@")
	if schemeEnd < 0 || at <= schemeEnd+len("://") {
		return "", false
	}

	return match[:schemeEnd+len("://")], true
}

func authSchemePrefix(match string) (string, bool) {
	lower := strings.ToLower(match)
	if !strings.HasPrefix(lower, "bearer") && !strings.HasPrefix(lower, "basic") {
		return "", false
	}

	idx := strings.IndexFunc(match, unicode.IsSpace)
	if idx < 0 {
		return "", false
	}

	_, size := utf8.DecodeRuneInString(match[idx:])

	return match[:idx+size], true
}

func sensitiveSummary(label, value string) string {
	redacted := false
	scanValue, _ := boundedValidPrefix(value, maxSecretScanBytes)

	for _, pattern := range secretPatterns {
		if pattern.MatchString(scanValue) {
			redacted = true
			break
		}
	}

	summary := fmt.Sprintf("%s redacted bytes=%d", label, len(value))
	if redacted {
		summary += " known_secret=true"
	}

	out, _ := truncateUTF8(summary, maxSummaryBytes)

	return out
}

func boundedValidPrefix(value string, maxBytes int) (string, bool) {
	if maxBytes <= 0 {
		maxBytes = maxSecretScanBytes
	}

	truncated := maxBytes > 0 && len(value) > maxBytes
	if truncated {
		value = value[:maxBytes]
	}

	return strings.ToValidUTF8(value, ""), truncated
}

func truncateUTF8(value string, maxBytes int) (string, bool) {
	if maxBytes <= 0 || len(value) <= maxBytes {
		return value, false
	}

	if maxBytes <= len(truncationMarker) {
		return truncationMarker[:maxBytes], true
	}

	limit := maxBytes - len(truncationMarker)

	out := value[:limit]
	for !utf8.ValidString(out) && out != "" {
		_, size := utf8.DecodeLastRuneInString(out)
		if size <= 0 {
			out = out[:len(out)-1]

			break
		}

		out = out[:len(out)-size]
	}

	return out + truncationMarker, true
}

func shrinkOversizedPayload(event Event) Event {
	event.Content = ""
	event.ContentSummary = "content redacted payload_limit=true"

	event.Error = ""
	if event.ErrorSummary == "" {
		event.ErrorSummary = "error redacted payload_limit=true"
	}

	event.Metadata = nil
	event.Redacted = true
	event.Truncated = true

	return event
}

func logHookInvocation(event Event, hook Hook, payloadBytes int) {
	payloadMode := string(normalizePayloadMode(event.PayloadMode))
	if event.PayloadMode == "" {
		payloadMode = string(normalizePayloadMode(string(hook.PayloadMode)))
	}

	auditEvent := Event{}
	eventType := sanitizeEventType(&auditEvent, event.Type)

	attrs := []any{
		slog.String("event_type", eventType),
		slog.String("hook_command", hookAuditCommand(hook.Command)),
		slog.String("payload_mode", payloadMode),
		slog.Int("payload_bytes", payloadBytes),
		slog.Bool("inherit_env", hook.InheritEnv),
		slog.Int("env_keys", len(hook.Env)),
	}
	if hook.Timeout > 0 {
		attrs = append(attrs, slog.Duration("timeout", hook.Timeout))
	}

	slog.Debug("lifecycle hook invocation", attrs...)
}

func hookAuditCommand(command []string) string {
	if len(command) == 0 {
		return ""
	}

	name := hookCommandBasename(sanitizeScalar("hook_command", command[0]))

	name = sanitizeAuditLabel("hook_command", name)
	if len(command) == 1 {
		return name
	}

	return fmt.Sprintf("%s (+%d args)", name, len(command)-1)
}

func hookCommandBasename(command string) string {
	command = strings.TrimRight(command, `/\`)
	if command == "" {
		return ""
	}

	if idx := strings.LastIndexAny(command, `/\`); idx >= 0 {
		return command[idx+1:]
	}

	return command
}

func sanitizeAuditLabel(key, value string) string {
	safe := sanitizeScalar(key, value)

	var builder strings.Builder

	for _, r := range safe {
		if isEventTypeRune(r) {
			builder.WriteRune(r)
			continue
		}

		builder.WriteByte('_')
	}

	if builder.Len() == 0 {
		return "value"
	}

	return builder.String()
}
