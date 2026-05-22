package retrieval_test

import (
	"testing"

	"github.com/stretchr/testify/assert"

	"github.com/tommoulard/atteler/pkg/retrieval"
)

func TestSanitize_RedactsAuthorizationHeaderWithoutLeavingBearerToken(t *testing.T) {
	t.Parallel()

	text, safety := retrieval.Sanitize("Authorization: Bearer abcdefghijklmnop\nnext line", retrieval.PolicyContext{})

	assert.Equal(t, "Authorization: [REDACTED]\nnext line", text)
	assert.False(t, safety.InjectAllowed)
	assert.True(t, safety.Redacted)
	assert.True(t, safety.Sensitive)
	assert.Contains(t, safety.Reasons, "authorization header")
	assert.NotContains(t, text, "abcdefghijklmnop")
}

func TestSanitize_RedactsPrefixedEnvironmentCredentialNames(t *testing.T) {
	t.Parallel()

	text, safety := retrieval.Sanitize("OPENAI_API_KEY=sk-test-secret\n\"api_key\": \"quoted-secret\"\nsafe=value", retrieval.PolicyContext{})

	assert.Equal(t, "OPENAI_API_KEY=[REDACTED]\n\"api_key\": \"[REDACTED]\"\nsafe=value", text)
	assert.False(t, safety.InjectAllowed)
	assert.True(t, safety.Redacted)
	assert.Contains(t, safety.Reasons, "credential-shaped assignment")
	assert.NotContains(t, text, "sk-test-secret")
	assert.NotContains(t, text, "quoted-secret")
}

func TestMergeSafety_PreservesUnsafePersistedDecision(t *testing.T) {
	t.Parallel()

	got := retrieval.MergeSafety(
		retrieval.Safety{InjectAllowed: false, Private: true, Reasons: []string{"persisted"}},
		retrieval.Safety{InjectAllowed: true, Redacted: true, Reasons: []string{"persisted", "redacted"}},
	)

	assert.False(t, got.InjectAllowed)
	assert.True(t, got.Private)
	assert.True(t, got.Redacted)
	assert.ElementsMatch(t, []string{"persisted", "redacted"}, got.Reasons)
}

func TestMergeSafety_TreatsOmittedSideAsDefaultInjectable(t *testing.T) {
	t.Parallel()

	got := retrieval.MergeSafety(retrieval.Safety{}, retrieval.Safety{InjectAllowed: true})
	assert.True(t, got.InjectAllowed)
	assert.False(t, got.Private)
	assert.False(t, got.Redacted)
	assert.False(t, got.Sensitive)

	got = retrieval.MergeSafety(retrieval.Safety{InjectAllowed: true}, retrieval.Safety{})
	assert.True(t, got.InjectAllowed)
	assert.False(t, got.Private)
	assert.False(t, got.Redacted)
	assert.False(t, got.Sensitive)
}

func TestSanitizeMetadata_RedactsCredentialNamedValues(t *testing.T) {
	t.Parallel()

	metadata, safety := retrieval.SanitizeMetadata(map[string]string{
		"api_key": "plain-secret-token",
		"path":    ".env",
	}, retrieval.PolicyContext{})

	assert.Equal(t, "[REDACTED]", metadata["api_key"])
	assert.Equal(t, ".env", metadata["path"])
	assert.False(t, safety.InjectAllowed)
	assert.True(t, safety.Redacted)
	assert.True(t, safety.Sensitive)
	assert.True(t, safety.Private)
	assert.NotContains(t, metadata["api_key"], "plain-secret-token")
}

func TestSafetyFromMetadata_TreatsLegacyUnsafeFlagsAsNotInjectable(t *testing.T) {
	t.Parallel()

	safety := retrieval.SafetyFromMetadata(map[string]string{
		retrieval.MetadataSafetySensitive: "true",
		retrieval.MetadataSafetyRedacted:  "true",
	})

	assert.False(t, safety.InjectAllowed)
	assert.True(t, safety.Sensitive)
	assert.True(t, safety.Redacted)
}

func TestSafetyFromMetadata_PreservesExplicitInjectDeniedMetadata(t *testing.T) {
	t.Parallel()

	safety := retrieval.SafetyFromMetadata(map[string]string{
		retrieval.MetadataSafetyInjectAllowed: "false",
	})

	assert.False(t, safety.InjectAllowed)
	assert.Contains(t, safety.Reasons, "metadata disallows injection")

	merged := retrieval.MergeSafety(safety, retrieval.Safety{InjectAllowed: true})
	assert.False(t, merged.InjectAllowed)
	assert.Contains(t, merged.Reasons, "metadata disallows injection")
}

func TestMergeSafetyMetadata_RoundTripsExplicitInjectDenied(t *testing.T) {
	t.Parallel()

	metadata := retrieval.MergeSafetyMetadata(nil, retrieval.Safety{InjectAllowed: false})
	safety := retrieval.SafetyFromMetadata(metadata)

	assert.Equal(t, "false", metadata[retrieval.MetadataSafetyInjectAllowed])
	assert.Contains(t, metadata[retrieval.MetadataSafetyReasons], "metadata disallows injection")
	assert.False(t, safety.InjectAllowed)
	assert.Contains(t, safety.Reasons, "metadata disallows injection")
}
