package llm

import (
	"errors"
	"regexp"
	"strings"
)

type providerFailureKind string

const (
	providerFailureUnknown        providerFailureKind = "unknown_error"
	providerFailureRateLimit      providerFailureKind = "transient_rate_limit"
	providerFailureTransient      providerFailureKind = "transient_error"
	providerFailureConfiguration  providerFailureKind = "configuration_error"
	providerFailureAuthentication providerFailureKind = "authentication_error"
	providerFailureNotReady       providerFailureKind = "provider_not_ready"
	providerFailureRouteExhausted providerFailureKind = "exhausted_fallback_route"
	providerFailurePermanent      providerFailureKind = "permanent_error"
)

const openAIRegionalHostnameSummary = "OpenAI regional hostname mismatch"

var openAIRegionalHostnamePattern = regexp.MustCompile(`(?i)\b([a-z0-9-]+\.api\.openai\.com)\b`)

type providerFailureClassification struct {
	Kind        providerFailureKind
	Summary     string
	Remediation string
	RateLimited bool
}

func classifyProviderFailure(err error) providerFailureClassification {
	if err == nil {
		return providerFailureClassification{
			Kind:    providerFailureUnknown,
			Summary: "unknown provider failure",
		}
	}

	msg := err.Error()
	lower := strings.ToLower(msg)

	if isFallbackRouteExhaustedError(lower) {
		return providerFailureClassification{
			Kind:    providerFailureRouteExhausted,
			Summary: "fallback route cannot be used",
		}
	}

	if isOpenAIRegionalHostnameError(err) {
		return providerFailureClassification{
			Kind:        providerFailureConfiguration,
			Summary:     openAIRegionalHostnameSummary,
			Remediation: openAIRegionalHostnameRemediation(msg),
		}
	}

	if isRateLimitError(err) {
		return providerFailureClassification{
			Kind:        providerFailureRateLimit,
			Summary:     "provider rate limit",
			RateLimited: true,
		}
	}

	if retryAfter, retryable := isRetryable(err); retryable || retryAfter > 0 {
		return providerFailureClassification{
			Kind:    providerFailureTransient,
			Summary: "transient provider error",
		}
	}

	if isAuthenticationOrConfigurationError(lower) {
		return providerFailureClassification{
			Kind:    providerFailureAuthentication,
			Summary: "provider authentication/configuration error",
		}
	}

	if isProviderNotReadyError(lower) {
		return providerFailureClassification{
			Kind:    providerFailureNotReady,
			Summary: "provider readiness is stale or unavailable",
		}
	}

	return providerFailureClassification{
		Kind:    providerFailurePermanent,
		Summary: "permanent provider error",
	}
}

func isOpenAIRegionalHostnameError(err error) bool {
	if err == nil {
		return false
	}

	msg := strings.ToLower(err.Error())

	return strings.Contains(msg, "incorrect regional hostname") ||
		(strings.Contains(msg, "regional hostname") && strings.Contains(msg, ".api.openai.com"))
}

func openAIRegionalHostnameRemediation(msg string) string {
	host := openAIRegionalHostname(msg)
	if host == "" {
		return "set OPENAI_BASE_URL or providers.openai.base_url to the regional API host required by OpenAI (base URL without the v1 path)"
	}

	return "set OPENAI_BASE_URL or providers.openai.base_url to https://" + host + " (base URL without the v1 path)"
}

func openAIRegionalHostname(msg string) string {
	match := openAIRegionalHostnamePattern.FindStringSubmatch(msg)
	if len(match) < 2 {
		return ""
	}

	return strings.ToLower(match[1])
}

func isAuthenticationOrConfigurationError(lower string) bool {
	return strings.Contains(lower, "http 401") ||
		strings.Contains(lower, "http 403") ||
		strings.Contains(lower, "unauthorized") ||
		strings.Contains(lower, "invalid_api_key") ||
		strings.Contains(lower, "invalid api key") ||
		strings.Contains(lower, "api key found") ||
		strings.Contains(lower, "missing_credentials") ||
		strings.Contains(lower, "missing credential") ||
		credentialDiscoveryFailed(lower) ||
		strings.Contains(lower, "permission_denied")
}

func credentialDiscoveryFailed(lower string) bool {
	if !strings.Contains(lower, "credential") {
		return false
	}

	return strings.Contains(lower, "no ") ||
		strings.Contains(lower, "cannot read") ||
		strings.Contains(lower, "invalid ")
}

func isProviderNotReadyError(lower string) bool {
	return strings.Contains(lower, "provider readiness") ||
		strings.Contains(lower, "failed_health_check") ||
		strings.Contains(lower, "stale=true") ||
		strings.Contains(lower, "using stale static fallback")
}

func isFallbackRouteExhaustedError(lower string) bool {
	return strings.Contains(lower, "llm: unknown model") ||
		strings.Contains(lower, "llm: unknown provider") ||
		strings.Contains(lower, "llm: ambiguous model") ||
		strings.Contains(lower, "llm: no providers registered") ||
		strings.Contains(lower, string(providerFailureRouteExhausted)) ||
		strings.Contains(lower, "fallback route cannot be used")
}

func classifiedProviderError(err error) string {
	classification := classifyProviderFailure(err)
	if classification.Remediation != "" {
		return classification.Summary + "; " + classification.Remediation
	}

	msg := shortError(err)
	if classification.Summary != "" {
		if msg != "" && msg != classification.Summary {
			return classification.Summary + ": " + msg
		}

		return classification.Summary
	}

	return msg
}

// ProviderFailureSummary returns a concise, user-facing provider failure
// summary. Known configuration failures include remediation hints and avoid
// repeating long raw provider payloads.
func ProviderFailureSummary(err error) string {
	return classifiedProviderError(err)
}

// ProviderFailureRemediationSummary returns an actionable provider failure
// summary when the failure classifier knows a remediation hint. It returns
// false for ordinary provider errors where callers should prefer the original
// provider text for diagnostic detail.
func ProviderFailureRemediationSummary(err error) (string, bool) {
	classification := classifyProviderFailure(err)
	if classification.Remediation == "" {
		return "", false
	}

	return classification.Summary + "; " + classification.Remediation, true
}

func wrapOpenAIRegionalHostnameError(err error) error {
	if err == nil || !isOpenAIRegionalHostnameError(err) {
		return err
	}

	var regionalErr *openAIRegionalHostnameError
	if errors.As(err, &regionalErr) {
		return err
	}

	return &openAIRegionalHostnameError{wrapped: err}
}

type openAIRegionalHostnameError struct {
	wrapped error
}

func (e *openAIRegionalHostnameError) Error() string {
	message := RedactDiagnosticMessage(e.wrapped.Error())

	return message + " (" + openAIRegionalHostnameSummary + "; " + openAIRegionalHostnameRemediation(message) + ")"
}

func (e *openAIRegionalHostnameError) Unwrap() error {
	return e.wrapped
}
