package privacy

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestRedactText_RemovesCommonSecretValues(t *testing.T) {
	t.Parallel()

	openAIKey := "sk-" + "testsecretvalue"
	openAIProjectKey := "sk-" + "proj-example_secret_123"
	githubPAT := "github" + "_pat_123456789012345678901234"
	githubToken := "gh" + "p_123456789012345678901234"
	slackToken := "xox" + "b-1234567890-abcdefghijklmnop"
	googleAPIKey := "AI" + "za12345678901234567890123456789012345"

	got := RedactText(strings.Join([]string{
		"password=hunter2",
		"api_key=\"abc123\"",
		"auth_token=tok123",
		"OPENAI_API_KEY=" + openAIKey,
		"Authorization: Bearer " + openAIKey,
		"Authorization: Basic basic-secret-value",
		"Authorization: token token-secret-value",
		"openai=" + openAIProjectKey,
		githubPAT,
		githubToken,
		slackToken,
		googleAPIKey,
		`{"api_key":"json-secret-value","authorization":"Bearer json-auth-secret","name":"demo"}`,
		"-----BEGIN OPENSSH PRIVATE KEY-----\nprivate-key-material\n-----END OPENSSH PRIVATE KEY-----",
		"-----BEGIN PGP PRIVATE KEY BLOCK-----\npgp-private-key-material\n-----END PGP PRIVATE KEY BLOCK-----",
	}, " "))

	assert.NotContains(t, got, "hunter2")
	assert.NotContains(t, got, "abc123")
	assert.NotContains(t, got, "tok123")
	assert.NotContains(t, got, openAIKey)
	assert.NotContains(t, got, "basic-secret-value")
	assert.NotContains(t, got, "token-secret-value")
	assert.NotContains(t, got, openAIProjectKey)
	assert.NotContains(t, got, "json-secret-value")
	assert.NotContains(t, got, "json-auth-secret")
	assert.NotContains(t, got, githubPAT)
	assert.NotContains(t, got, githubToken)
	assert.NotContains(t, got, slackToken)
	assert.NotContains(t, got, googleAPIKey)
	assert.NotContains(t, got, "private-key-material")
	assert.NotContains(t, got, "pgp-private-key-material")
	assert.NotContains(t, got, "OPENSSH PRIVATE KEY")
	assert.NotContains(t, got, "PGP PRIVATE KEY BLOCK")
	assert.Contains(t, got, "password=[REDACTED]")
	assert.Contains(t, got, "api_key=[REDACTED]")
	assert.Contains(t, got, `"api_key":"[REDACTED]"`)
	assert.Contains(t, got, `"authorization":"[REDACTED]"`)
	assert.Contains(t, got, "auth_token=[REDACTED]")
	assert.Contains(t, got, "OPENAI_API_KEY=[REDACTED]")
	assert.Contains(t, got, "Authorization: Bearer [REDACTED]")
	assert.Contains(t, got, "Authorization: Basic [REDACTED]")
	assert.Contains(t, got, "Authorization: token [REDACTED]")
}

func TestRedactMetadata_RedactsSensitiveKeys(t *testing.T) {
	t.Parallel()

	got := RedactMetadata(map[string]string{
		"kind":       "note",
		"auth-token": "abc123",
		"summary":    "uses password=hunter2",
	})

	assert.Equal(t, "note", got["kind"])
	assert.Equal(t, "[REDACTED]", got["auth-token"])
	assert.NotContains(t, got["summary"], "hunter2")
}

func TestRedactMetadata_RedactsSensitiveKeyNames(t *testing.T) {
	t.Parallel()

	got := RedactMetadata(map[string]string{
		"source/access_token=key123/path": "safe value",
		"api_key=key456":                  "raw value",
	})

	for key, value := range got {
		assert.NotContains(t, key, "key123")
		assert.NotContains(t, key, "key456")
		assert.NotContains(t, value, "key123")
		assert.NotContains(t, value, "key456")
	}

	assert.Equal(t, "[REDACTED]", got["source/access_token=[REDACTED]/path"])
	assert.Equal(t, "[REDACTED]", got["api_key=[REDACTED]"])
}

func TestRedactMetadata_PreservesIdentifierStructure(t *testing.T) {
	t.Parallel()

	got := RedactMetadata(map[string]string{
		"session_id":    "tenant/access_token=artifact123/message/0",
		"artifact_path": "docs/oauth.md?refresh_token=refresh123&kind=note",
	})

	assert.Equal(t, "tenant/access_token=[REDACTED]/message/0", got["session_id"])
	assert.Equal(t, "docs/oauth.md?refresh_token=[REDACTED]&kind=note", got["artifact_path"])
	assert.NotContains(t, got["session_id"], "artifact123")
	assert.NotContains(t, got["artifact_path"], "refresh123")
}

func TestRedactMetadata_PreservesModelAndAgentIdentifierStructure(t *testing.T) {
	t.Parallel()

	got := RedactMetadata(map[string]string{
		"default_model": "tenant/embed?api_key=model123/v1",
		"source_agent":  "reviewer/access_token=agent123/team",
		"commit":        "auth_token=commit123/path",
		"worktree_ref":  "refs/heads/main?refresh_token=ref123",
		"branch":        "feature/access_token=branch123/team",
	})

	assert.Equal(t, "tenant/embed?api_key=[REDACTED]/v1", got["default_model"])
	assert.Equal(t, "reviewer/access_token=[REDACTED]/team", got["source_agent"])
	assert.Equal(t, "auth_token=[REDACTED]/path", got["commit"])
	assert.Equal(t, "refs/heads/main?refresh_token=[REDACTED]", got["worktree_ref"])
	assert.Equal(t, "feature/access_token=[REDACTED]/team", got["branch"])
	assert.NotContains(t, got["default_model"], "model123")
	assert.NotContains(t, got["source_agent"], "agent123")
	assert.NotContains(t, got["commit"], "commit123")
	assert.NotContains(t, got["worktree_ref"], "ref123")
	assert.NotContains(t, got["branch"], "branch123")
}

func TestRedactIdentifier_PreservesPathAndQueryStructure(t *testing.T) {
	t.Parallel()

	got := RedactIdentifier("session/access_token=artifact123/message/0?refresh_token=refresh123&kind=note")

	assert.Equal(t, "session/access_token=[REDACTED]/message/0?refresh_token=[REDACTED]&kind=note", got)
	assert.NotContains(t, got, "artifact123")
	assert.NotContains(t, got, "refresh123")
}
