package eval

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestRedact_RemovesCommonSecrets(t *testing.T) {
	t.Parallel()

	input := `api_key="supersecret" Authorization: Bearer live-token sk-testsecret123 password=hunter2`
	got := Redact(input)

	assert.Contains(t, got, "[REDACTED]")
	assert.NotContains(t, got, "supersecret")
	assert.NotContains(t, got, "live-token")
	assert.NotContains(t, got, "sk-testsecret123")
	assert.NotContains(t, got, "hunter2")
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
