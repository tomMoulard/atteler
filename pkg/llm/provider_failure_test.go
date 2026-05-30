package llm

import (
	"errors"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestClassifyProviderFailure_CredentialDiscoveryErrors(t *testing.T) {
	t.Parallel()

	for _, msg := range []string{
		"no OpenAI credentials found: set OPENAI_API_KEY",
		"no OpenAI Platform API key found in OPENAI_API_KEY or ~/.codex/auth.json",
		"cannot read Claude Code credentials: permission denied",
		"invalid Claude Code credentials JSON: unexpected end of JSON input",
		"no accessToken in Claude Code credentials",
	} {
		t.Run(msg, func(t *testing.T) {
			t.Parallel()

			classification := classifyProviderFailure(errors.New(msg))
			assert.Equal(t, providerFailureAuthentication, classification.Kind)
		})
	}
}

func TestClassifyProviderFailure_RouteExhaustionWinsOverReadinessContext(t *testing.T) {
	t.Parallel()

	for _, msg := range []string{
		`llm: unknown model "missing" (provider readiness: openai=missing_credentials models=static classification=authentication_error error=provider authentication/configuration error: no OpenAI credentials found)`,
		`llm: unknown model "missing" (provider readiness: claude-code=registered models=static classification=transient_rate_limit error=provider rate limit: claude code: HTTP 429)`,
		`llm: unknown model "missing" (provider readiness: openai=registered models=static stale=true classification=configuration_error error=OpenAI regional hostname mismatch; set OPENAI_BASE_URL or providers.openai.base_url to https://us.api.openai.com)`,
		`llm: ambiguous model "shared" claimed by providers: openai, anthropic`,
		`llm: no providers registered`,
	} {
		t.Run(msg, func(t *testing.T) {
			t.Parallel()

			classification := classifyProviderFailure(errors.New(msg))
			assert.Equal(t, providerFailureRouteExhausted, classification.Kind)
		})
	}
}

func TestClassifyProviderFailure_UnresolvedProviderDetailIsNotRouteExhaustion(t *testing.T) {
	t.Parallel()

	classification := classifyProviderFailure(errors.New("provider returned unresolved internal state"))
	assert.Equal(t, providerFailurePermanent, classification.Kind)
}

func TestClassifyProviderFailure_ProviderUnknownModelMessageIsNotRouteExhaustion(t *testing.T) {
	t.Parallel()

	classification := classifyProviderFailure(errors.New(`llm: openai: HTTP 404: {"error":{"message":"unknown model"}}`))
	assert.Equal(t, providerFailurePermanent, classification.Kind)
}

func TestClassifyProviderFailure_OpenAIRegionalHostnameTruncated(t *testing.T) {
	t.Parallel()

	err := errors.New(`openai: models HTTP 401: {"error":{"message":"Attempted to access resource with incorrect regional hostname. Please make your request to us.api.openai…"}}`)

	classification := classifyProviderFailure(err)

	assert.Equal(t, providerFailureConfiguration, classification.Kind)
	assert.Equal(t, openAIRegionalHostnameSummary, classification.Summary)
	assert.Contains(t, classification.Remediation, "OPENAI_BASE_URL")
	assert.Contains(t, classification.Remediation, "regional API host")
	assert.NotContains(t, classification.Remediation, "https://")
}

func TestWrapOpenAIRegionalHostnameError_Idempotent(t *testing.T) {
	t.Parallel()

	err := errors.New(`openai: models HTTP 401: {"error":{"message":"Attempted to access resource with incorrect regional hostname. Please make your request to us.api.openai.com"}}`)

	wrapped := wrapOpenAIRegionalHostnameError(err)
	wrappedAgain := wrapOpenAIRegionalHostnameError(wrapped)

	assert.Equal(t, wrapped, wrappedAgain)
	assert.Equal(t, 1, strings.Count(wrappedAgain.Error(), openAIRegionalHostnameSummary))
}
