package llm

import (
	"encoding/json"
	"fmt"
	"net/http"
	"regexp"
	"strings"
	"time"
)

const (
	providerErrorMessageLimit = 4096
	nilErrorMessage           = "<nil>"
)

var ( // #nosec G101 -- redaction keyword regexps, not credentials.
	providerErrorSecretAssignmentPattern = regexp.MustCompile(`(?i)\b([A-Za-z0-9_.-]*(api[_-]?key|access[_-]?token|refresh[_-]?token|auth[_-]?token|authorization|token|secret|password|credential)[A-Za-z0-9_.-]*)(\s*[:=]\s*)(?:"[^"]*"|'[^']*'|(?:Bearer|Basic|Token)\s+(?:"[^"]*"|'[^']*'|\[[^\]]+\]|[^\s,"'&}\]]+)|\[[^\]]+\]|[^\s,"'&}\]]+)`)
	providerErrorBearerPattern           = regexp.MustCompile(`(?i)\b(Bearer|Basic|Token)\s+[A-Za-z0-9._~+/=-]{8,}`)
	providerErrorOpenAIKeyPattern        = regexp.MustCompile(`\bsk-[A-Za-z0-9_-]{8,}\b`)
	providerErrorJWTPattern              = regexp.MustCompile(`\beyJ[A-Za-z0-9_-]{10,}\.[A-Za-z0-9_-]{10,}\.[A-Za-z0-9_-]{10,}\b`)
)

// Retryability describes whether a provider error is safe to retry.
type Retryability string

const (
	// RetryabilityRetryable marks transient provider failures such as rate limits
	// and 5xx responses.
	RetryabilityRetryable Retryability = "retryable"
	// RetryabilityNonRetryable marks client/auth/provider failures that should not
	// be retried without caller action.
	RetryabilityNonRetryable Retryability = "non_retryable"
	// RetryabilityUnknown marks errors that do not expose enough typed data for a
	// confident retry decision.
	RetryabilityUnknown Retryability = "unknown"
)

// ProviderError is the common typed HTTP/payload error returned by provider
// adapters. It intentionally carries retry metadata without including prompt
// text or credentials in lifecycle events.
type ProviderError struct {
	Provider     string
	RequestID    string
	Message      string
	Retryability Retryability
	RetryAfter   time.Duration
	StatusCode   int
}

// Error formats the provider error for users while preserving typed fields for
// callers via errors.As.
func (e *ProviderError) Error() string {
	if e == nil {
		return nilErrorMessage
	}

	provider := strings.TrimSpace(e.Provider)
	if provider == "" {
		provider = "provider"
	}

	message := RedactDiagnosticMessage(strings.TrimSpace(e.Message))
	if message == "" {
		message = http.StatusText(e.StatusCode)
	}

	var metadata []string
	if retryability := e.retryability(); retryability != RetryabilityUnknown || e.Retryability != "" || e.StatusCode > 0 {
		metadata = append(metadata, string(retryability))
	}

	if e.RequestID != "" {
		metadata = append(metadata, "request_id="+RedactDiagnosticMessage(e.RequestID))
	}

	if e.RetryAfter > 0 {
		metadata = append(metadata, "retry_after="+e.RetryAfter.String())
	}

	if len(metadata) == 0 {
		return fmt.Sprintf("%s: HTTP %d: %s", provider, e.StatusCode, message)
	}

	return fmt.Sprintf("%s: HTTP %d (%s): %s", provider, e.StatusCode, strings.Join(metadata, ", "), message)
}

// IsRetryable reports whether this failure is safe to retry. Explicit
// retryable/non-retryable classifications win; otherwise the typed HTTP status
// code is used.
func (e *ProviderError) IsRetryable() bool {
	return e != nil && e.retryability() == RetryabilityRetryable
}

func (e *ProviderError) retryability() Retryability {
	if e == nil {
		return RetryabilityUnknown
	}

	switch e.Retryability {
	case RetryabilityRetryable, RetryabilityNonRetryable:
		return e.Retryability
	case RetryabilityUnknown:
		if e.StatusCode <= 0 {
			return RetryabilityUnknown
		}
	default:
		if e.StatusCode <= 0 {
			return RetryabilityUnknown
		}
	}

	return retryabilityForStatus(e.StatusCode)
}

func newProviderHTTPError(provider string, resp *http.Response, body []byte) *ProviderError {
	return &ProviderError{
		Provider:     provider,
		StatusCode:   resp.StatusCode,
		RetryAfter:   parseRetryAfter(resp.Header.Get("Retry-After")),
		RequestID:    providerRequestID(resp.Header),
		Message:      providerErrorMessage(body),
		Retryability: retryabilityForStatus(resp.StatusCode),
	}
}

func newProviderPayloadError(provider string, statusCode int, header http.Header, errorType, message string) *ProviderError {
	retryAfter := parseRetryAfter(header.Get("Retry-After"))

	return &ProviderError{
		Provider:     provider,
		StatusCode:   statusCode,
		RetryAfter:   retryAfter,
		RequestID:    providerRequestID(header),
		Message:      providerPayloadErrorMessage(errorType, message),
		Retryability: retryabilityForProviderPayload(statusCode, retryAfter, errorType, message),
	}
}

func retryabilityForStatus(code int) Retryability {
	if isRetryableStatus(code) {
		return RetryabilityRetryable
	}

	if code >= http.StatusMultipleChoices {
		return RetryabilityNonRetryable
	}

	return RetryabilityUnknown
}

func retryabilityForProviderPayload(statusCode int, retryAfter time.Duration, values ...string) Retryability {
	if statusCode > 0 && statusCode != http.StatusOK {
		return retryabilityForStatus(statusCode)
	}

	if retryAfter > 0 {
		return RetryabilityRetryable
	}

	text := strings.ToLower(strings.Join(values, " "))
	normalized := strings.NewReplacer("-", "_", " ", "_").Replace(text)

	for _, marker := range []string{
		"rate_limit",
		"too_many_requests",
		"overloaded",
		"server_error",
		"service_unavailable",
		"temporarily_unavailable",
		"timeout",
	} {
		if strings.Contains(normalized, marker) {
			return RetryabilityRetryable
		}
	}

	return RetryabilityUnknown
}

func providerRequestID(header http.Header) string {
	for _, key := range []string{
		"x-request-id",
		"request-id",
		"x-amzn-requestid",
		"x-amzn-request-id",
		"cf-ray",
	} {
		if value := strings.TrimSpace(header.Get(key)); value != "" {
			return redactProviderErrorMessage(value)
		}
	}

	return ""
}

func providerPayloadErrorMessage(errorType, message string) string {
	errorType = strings.TrimSpace(errorType)
	message = strings.TrimSpace(message)

	switch {
	case errorType != "" && message != "":
		return truncateProviderErrorMessage(errorType + ": " + message)
	case errorType != "":
		return truncateProviderErrorMessage(errorType)
	case message != "":
		return truncateProviderErrorMessage(message)
	default:
		return ""
	}
}

func providerErrorMessage(body []byte) string {
	trimmed := strings.TrimSpace(string(body))
	if trimmed == "" {
		return ""
	}

	if message := providerJSONErrorMessage([]byte(trimmed)); message != "" {
		return truncateProviderErrorMessage(message)
	}

	return truncateProviderErrorMessage(trimmed)
}

func providerJSONErrorMessage(body []byte) string {
	var payload map[string]any
	if err := json.Unmarshal(body, &payload); err != nil {
		return ""
	}

	if value, ok := payload["error"]; ok {
		return stringifyProviderErrorValue(value)
	}

	for _, key := range []string{"message", "detail"} {
		if value, ok := payload[key]; ok {
			if message := stringifyProviderErrorValue(value); message != "" {
				return message
			}
		}
	}

	return ""
}

func stringifyProviderErrorValue(value any) string {
	switch typed := value.(type) {
	case string:
		return strings.TrimSpace(typed)
	case map[string]any:
		return stringifyProviderErrorObject(typed)
	default:
		encoded, err := json.Marshal(typed)
		if err != nil {
			return ""
		}

		return strings.TrimSpace(string(encoded))
	}
}

func stringifyProviderErrorObject(value map[string]any) string {
	var parts []string

	for _, key := range []string{"type", "code"} {
		if part, ok := value[key].(string); ok && strings.TrimSpace(part) != "" {
			parts = append(parts, strings.TrimSpace(part))
		}
	}

	if message, ok := value["message"].(string); ok && strings.TrimSpace(message) != "" {
		parts = append(parts, strings.TrimSpace(message))
	}

	if len(parts) > 0 {
		return strings.Join(parts, ": ")
	}

	encoded, err := json.Marshal(value)
	if err != nil {
		return ""
	}

	return strings.TrimSpace(string(encoded))
}

func truncateProviderErrorMessage(message string) string {
	message = redactProviderErrorMessage(message)

	if len(message) <= providerErrorMessageLimit {
		return message
	}

	cut := 0

	for idx := range message {
		if idx > providerErrorMessageLimit {
			break
		}

		cut = idx
	}

	return message[:cut] + "…"
}

func redactProviderErrorMessage(message string) string {
	message = providerErrorOpenAIKeyPattern.ReplaceAllString(message, "[REDACTED]")
	message = providerErrorJWTPattern.ReplaceAllString(message, "[REDACTED]")
	message = providerErrorBearerPattern.ReplaceAllString(message, "$1 [REDACTED]")
	message = providerErrorSecretAssignmentPattern.ReplaceAllString(message, "$1$3[REDACTED]")

	return message
}

// RedactDiagnosticMessage removes credential-shaped values from diagnostic
// strings before they are displayed, logged, or serialized.
func RedactDiagnosticMessage(message string) string {
	return redactProviderErrorMessage(message)
}
