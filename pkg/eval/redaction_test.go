package eval

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestRedact_RemovesCommonSecrets(t *testing.T) {
	t.Parallel()

	privateKeyLabel := "PRIVATE" + " KEY"
	pgpPrivateKeyLabel := "PGP " + privateKeyLabel + " BLOCK"
	jwt := "eyJhbGciOiJIUzI1NiIs" + "." + "eyJzdWIiOiIxMjM0NTY3ODkwIn0" + "." + "c2lnbmF0dXJlX3ZhbHVlMTIz"
	input := strings.Join([]string{
		`api_key="supersecret" Authorization: Bearer live-token sk-testsecret123 password=hunter2`,
		`quoted_secret="two words secret"`,
		"AWS_ACCESS_" + "KEY_ID=AKIA" + "IOSFODNN7EXAMPLE",
		"aws_secret_access_" + "key=wJalrXUtnFEMI/K7MDENG/bPxRfiCYEXAMPLEKEY",
		"lowercase_aws_access_" + "key_id=akia" + "iosfodnn7example",
		"X-Amz-Signature=aws-signature-secret",
		"x-api-" + "key: upstream-api-key",
		"session_" + "token=temporary-session",
		`{"api_key":"json-secret-value","authorization":"Bearer json-bearer-token"}`,
		`'password': 'single-quoted-secret'`,
		"url=https://user:" + "url-password@example.com/path",
		"standalone=" + "https://example.com/" + "AKIA" + "IOSFODNN7EXAMPLE" + "/docs",
		"github=" + "ghp_" + "abcdefghijklmnopqrstuvwxyz123456",
		"jwt=" + jwt,
		"-----BEGIN OPENSSH " + privateKeyLabel + "-----",
		"private-key-material",
		"-----END OPENSSH " + privateKeyLabel + "-----",
		"-----BEGIN " + pgpPrivateKeyLabel + "-----",
		"pgp-private-key-material",
		"-----END " + pgpPrivateKeyLabel + "-----",
	}, "\n")
	got := Redact(input)

	assert.Contains(t, got, "[REDACTED]")
	assert.NotContains(t, got, "supersecret")
	assert.NotContains(t, got, "live-token")
	assert.NotContains(t, got, "sk-testsecret123")
	assert.NotContains(t, got, "hunter2")
	assert.NotContains(t, got, "two words secret")
	assert.NotContains(t, got, "AKIAIOSFODNN7EXAMPLE")
	assert.NotContains(t, got, "akiaiosfodnn7example")
	assert.NotContains(t, got, "aws-signature-secret")
	assert.NotContains(t, got, "wJalrXUtnFEMI")
	assert.NotContains(t, got, "upstream-api-key")
	assert.NotContains(t, got, "temporary-session")
	assert.NotContains(t, got, "json-secret-value")
	assert.NotContains(t, got, "json-bearer-token")
	assert.NotContains(t, got, "single-quoted-secret")
	assert.NotContains(t, got, "url-password")
	assert.NotContains(t, got, "ghp_abcdefghijklmnopqrstuvwxyz123456")
	assert.NotContains(t, got, jwt)
	assert.NotContains(t, got, "private-key-material")
	assert.NotContains(t, got, "pgp-private-key-material")
	assert.Contains(t, got, "[REDACTED PRIVATE KEY]")
}

func TestRedact_RemovesUnterminatedPrivateKeyBlock(t *testing.T) {
	t.Parallel()

	privateKeyLabel := "PRIVATE" + " KEY"
	input := "before\n-----BEGIN OPENSSH " + privateKeyLabel + "-----\nprivate-key-material\n"

	got := Redact(input)
	assert.Contains(t, got, "before")
	assert.Contains(t, got, "[REDACTED PRIVATE KEY]")
	assert.NotContains(t, got, "private-key-material")
	assert.NotContains(t, got, "BEGIN OPENSSH")
}

func TestCheckFailure_RedactsSnippets(t *testing.T) {
	t.Parallel()

	result := Check("Authorization: Bearer actual-token", "Authorization: Bearer expected-token", ModeExact)

	assert.False(t, result.Passed)
	assert.Contains(t, result.Failure(), "[REDACTED]")
	assert.NotContains(t, result.Failure(), "actual-token")
	assert.NotContains(t, result.Failure(), "expected-token")
}

func TestCheckFailure_RedactsContainsSummary(t *testing.T) {
	t.Parallel()

	result := Check("no secret here", "api_key=expected-token", ModeContains)

	assert.False(t, result.Passed)
	assert.Contains(t, result.Failure(), "[REDACTED]")
	assert.NotContains(t, result.Failure(), "expected-token")
}
