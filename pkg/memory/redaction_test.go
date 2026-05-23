package memory

import (
	"errors"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestRedactor_DefaultRulesRedactCommonProviderSecrets(t *testing.T) {
	t.Parallel()

	redactor, err := NewRedactor()
	require.NoError(t, err)

	tests := []struct {
		name string
		rule string
		raw  string
	}{
		{name: "aws access key", rule: "aws_access_key_id", raw: "AKIA" + strings.Repeat("A", 16)},
		{name: "google api key", rule: "google_api_key", raw: "AIza" + strings.Repeat("A", 35)},
		{name: "stripe secret key", rule: "stripe_secret_key", raw: "sk_live_" + strings.Repeat("a", 24)},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			redacted, decision := redactor.Redact("token=" + tc.raw)
			assert.NotContains(t, redacted, tc.raw)
			assert.True(t, decision.Redacted)
			assert.Contains(t, decision.Rules, tc.rule)
		})
	}
}

func TestRedactor_SecretAssignmentRedactsQuotedAndJSONValues(t *testing.T) {
	t.Parallel()

	redactor, err := NewRedactor()
	require.NoError(t, err)

	tests := []struct {
		name   string
		text   string
		secret string
	}{
		{name: "quoted env value with spaces", text: `password="correct horse battery"`, secret: "correct horse battery"},
		{name: "single quoted env value with spaces", text: `api_key='correct horse battery'`, secret: "correct horse battery"},
		{name: "json quoted key and value", text: `"password": "correct horse battery"`, secret: "correct horse battery"},
		{name: "yaml unquoted value with spaces", text: `access_token: correct horse battery`, secret: "correct horse battery"},
		{name: "generic token assignment", text: `token="correct horse battery"`, secret: "correct horse battery"},
		{name: "client secret assignment", text: `client_secret="correct horse battery"`, secret: "correct horse battery"},
		{name: "private key camel case assignment", text: `privateKey="correct horse battery"`, secret: "correct horse battery"},
		{name: "provider-prefixed env api key", text: `OPENAI_API_KEY="correct horse battery"`, secret: "correct horse battery"},
		{name: "provider-prefixed token", text: `GITHUB_TOKEN='correct horse battery'`, secret: "correct horse battery"},
		{name: "provider key assignment", text: `password=sk-1234567890abcdefSECRET`, secret: "sk-1234567890abcdefSECRET"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			redacted, decision := redactor.Redact(tc.text)
			assert.True(t, decision.Redacted)
			assert.Contains(t, decision.Rules, "secret_assignment")
			assert.Contains(t, redacted, "[REDACTED:secret_assignment]")
			assert.NotContains(t, redacted, tc.secret)
			assert.NotContains(t, redacted, "]]")
		})
	}
}

func TestRedactor_RedactIdentifierKeepsDistinctFingerprints(t *testing.T) {
	t.Parallel()

	const (
		firstSecret  = "sk-1234567890abcdefSECRET"
		secondSecret = "sk-abcdef1234567890SECRET"
	)

	redactor, err := NewRedactor()
	require.NoError(t, err)

	first, firstDecision := redactor.RedactIdentifier("session/" + firstSecret + "/message/0")
	second, secondDecision := redactor.RedactIdentifier("session/" + secondSecret + "/message/0")

	assert.NotContains(t, first, firstSecret)
	assert.NotContains(t, second, secondSecret)
	assert.NotEqual(t, first, second)
	assert.True(t, firstDecision.Redacted)
	assert.True(t, secondDecision.Redacted)
	assert.Contains(t, firstDecision.Rules, "openai_api_key")
	assert.Contains(t, secondDecision.Rules, "openai_api_key")
}

func TestRedactor_InvalidCustomRuleRedactsBuiltInSecretsInError(t *testing.T) {
	t.Parallel()

	const secret = "sk-1234567890abcdefSECRET"

	_, err := NewRedactor("(" + secret)

	require.Error(t, err)
	assert.NotContains(t, err.Error(), secret)
	assert.Contains(t, err.Error(), "[REDACTED:openai_api_key]")
}

func TestRedactor_CustomRuleNamesIgnoreBlankPatterns(t *testing.T) {
	t.Parallel()

	redactor, err := NewRedactor("", `ACME-[0-9]+`)
	require.NoError(t, err)

	redacted, decision := redactor.Redact("customer ACME-12345")

	assert.Contains(t, redacted, "[REDACTED:custom_1]")
	assert.Contains(t, decision.Rules, "custom_1")
	assert.NotContains(t, decision.Rules, "custom_2")
}

func TestRedactor_InvalidCustomRuleDoesNotEchoUnrecognizedPattern(t *testing.T) {
	t.Parallel()

	const secret = "ACME-12345"

	_, err := NewRedactor("(" + secret)

	require.Error(t, err)
	assert.NotContains(t, err.Error(), secret)
	assert.Contains(t, err.Error(), "compile redaction rule custom_1")
	assert.Contains(t, err.Error(), "invalid regexp")

	for cause := errors.Unwrap(err); cause != nil; cause = errors.Unwrap(cause) {
		assert.NotContains(t, cause.Error(), secret)
	}
}
