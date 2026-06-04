package events

import (
	"encoding/json"
	"strconv"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestPrivacySchemasCoverSupportedEventTypes(t *testing.T) {
	t.Parallel()

	supported := make(map[string]struct{})
	for _, eventType := range SupportedEventTypes() {
		supported[eventType.Type] = struct{}{}

		_, ok := eventSchemas[eventType.Type]
		assert.True(t, ok, "missing privacy schema for %s", eventType.Type)
	}

	for eventType := range eventSchemas {
		_, ok := supported[eventType]
		assert.True(t, ok, "privacy schema for unsupported event %s", eventType)
	}
}

func TestSanitizeEventForHook_DropsUnknownEventPayload(t *testing.T) {
	t.Parallel()

	got := sanitizeEventForHook(Event{
		Type:        "custom_event",
		SessionID:   "session-1",
		SessionPath: "/Users/example/private/session.json",
		Agent:       "agent-name",
		Model:       "model-name",
		Role:        "user",
		Content:     "prompt with OPENAI_API_KEY=sk-unknownsecret1234567890",
		Error:       "raw error",
		Metadata: map[string]string{
			"token": "sk-metasecret1234567890",
			"path":  "/Users/example/private/file.txt",
		},
	}, PayloadFull)

	assert.Equal(t, "custom_event", got.Type)
	assert.Empty(t, got.SessionID)
	assert.Empty(t, got.SessionPath)
	assert.Empty(t, got.Agent)
	assert.Empty(t, got.Model)
	assert.Empty(t, got.Role)
	assert.Empty(t, got.Content)
	assert.Empty(t, got.Error)
	assert.Empty(t, got.Metadata)
	assert.True(t, got.Redacted)
}

func TestSanitizeEventForHook_RedactsSecretEventType(t *testing.T) {
	t.Parallel()

	got := sanitizeEventForHook(Event{
		Type: "custom_sk-eventsecret1234567890",
	}, PayloadMetadata)

	assert.Contains(t, got.Type, redactedValue)
	assert.NotContains(t, got.Type, "sk-eventsecret")
	assert.True(t, got.Redacted)
}

func TestSanitizeEventForHook_NormalizesUnsafeEventType(t *testing.T) {
	t.Parallel()

	got := sanitizeEventForHook(Event{
		Type: "custom\nevent type",
	}, PayloadMetadata)

	assert.Equal(t, "custom_event_type", got.Type)
	assert.True(t, got.Redacted)
}

func TestSanitizeEventForHook_PreservesExistingPrivacyMarkers(t *testing.T) {
	t.Parallel()

	got := sanitizeEventForHook(Event{
		Type:      SessionStart,
		Redacted:  true,
		Truncated: true,
	}, PayloadMetadata)

	assert.True(t, got.Redacted)
	assert.True(t, got.Truncated)
}

func TestSanitizeEventForHook_DropsProvidedSummaries(t *testing.T) {
	t.Parallel()

	got := sanitizeEventForHook(Event{
		Type:           CommandOutput,
		ContentSummary: "prompt summary token=sk-contentsummarysecret1234567890",
		ErrorSummary:   "error summary token=sk-errorsummarysecret1234567890",
	}, PayloadSummary)

	data, err := json.Marshal(got)
	require.NoError(t, err)

	assert.Empty(t, got.ContentSummary)
	assert.Empty(t, got.ErrorSummary)
	assert.True(t, got.Redacted)
	assert.NotContains(t, string(data), "prompt summary")
	assert.NotContains(t, string(data), "error summary")
	assert.NotContains(t, string(data), "sk-contentsummarysecret")
	assert.NotContains(t, string(data), "sk-errorsummarysecret")
}

func TestSanitizeEventForHook_CommandExecuteKeepsSafeMetadataOnly(t *testing.T) {
	t.Parallel()

	got := sanitizeEventForHook(Event{
		Type: CommandExecute,
		Metadata: map[string]string{
			"provider":     "codex",
			"source":       "llm_tool",
			"model_mode":   "fast",
			"service_tier": "priority",
			"count":        "3",
			"waves":        "2",
			"command":      "cat ~/.env",
			"cwd":          "/Users/example/project",
			"token":        "sk-metasecret1234567890",
		},
	}, PayloadMetadata)

	assert.Equal(t, map[string]string{
		"count":        "3",
		"model_mode":   "fast",
		"provider":     "codex",
		"service_tier": "priority",
		"source":       "llm_tool",
		"waves":        "2",
	}, got.Metadata)
	assert.True(t, got.Redacted)
}

func TestSanitizeEventForHook_LifecycleKeepsModelSettingsMetadata(t *testing.T) {
	t.Parallel()

	for _, eventType := range []string{SessionStart, SessionEnd} {
		got := sanitizeEventForHook(Event{
			Type: eventType,
			Metadata: map[string]string{
				"agent_loop_budget": `{"max_model_calls":2}`,
				"model_mode":        "fast",
				"reasoning_level":   "high",
				"prompt":            "secret=sk-metasecret1234567890",
			},
		}, PayloadMetadata)

		assert.Equal(t, map[string]string{
			"agent_loop_budget": `{"max_model_calls":2}`,
			"model_mode":        "fast",
			"reasoning_level":   "high",
		}, got.Metadata)
		assert.True(t, got.Redacted)
	}
}

func TestSanitizeEventForHook_ToolExecuteKeepsModelSettingsMetadata(t *testing.T) {
	t.Parallel()

	got := sanitizeEventForHook(Event{
		Type: ToolExecute,
		Metadata: map[string]string{
			"provider":           "openai",
			"tool":               "llm.complete",
			"model_mode":         "fast",
			"reasoning_level":    "high",
			"service_tier":       "priority",
			"option_adjustments": "Temperature omitted",
			"prompt":             "secret=sk-metasecret1234567890",
		},
	}, PayloadMetadata)

	assert.Equal(t, map[string]string{
		"provider":           "openai",
		"tool":               "llm.complete",
		"model_mode":         "fast",
		"reasoning_level":    "high",
		"service_tier":       "priority",
		"option_adjustments": "Temperature omitted",
	}, got.Metadata)
	assert.True(t, got.Redacted)
}

func TestSanitizeEventForHook_ContextManifestKeepsBackgroundSuggestionAuditMetadata(t *testing.T) {
	t.Parallel()

	got := sanitizeEventForHook(Event{
		Type: ContextManifest,
		Metadata: map[string]string{
			"context_manifest":      `{"schema_version":1}`,
			"request_kind":          "background_suggestion",
			"background_suggestion": "true",
			"context_summary":       "agent=1,file/task/issue=omitted-private",
			"draft":                 "send api_key=sk-backgroundsecret1234567890",
		},
	}, PayloadMetadata)

	assert.Equal(t, `{"schema_version":1}`, got.Metadata["context_manifest"])
	assert.Equal(t, "background_suggestion", got.Metadata["request_kind"])
	assert.Equal(t, "true", got.Metadata["background_suggestion"])
	assert.Equal(t, "agent=1,file/task/issue=omitted-private", got.Metadata["context_summary"])
	assert.NotContains(t, got.Metadata, "draft")
	assert.True(t, got.Redacted)
}

func TestSanitizeEventForHook_RedactsPathLookingSafeMetadataValues(t *testing.T) {
	t.Parallel()

	got := sanitizeEventForHook(Event{
		Type: CommandExecute,
		Metadata: map[string]string{
			"source":       "/Users/example/private/output.log",
			"provider":     `C:\Users\example\secret.txt`,
			"tool_call_id": "file:///Users/example/private/trace.json",
			"mode":         "  ~/private/session.log",
			"waves":        `\Users\example\rooted.txt`,
			"count":        "3",
		},
	}, PayloadMetadata)

	assert.Equal(t, redactedValue, got.Metadata["source"])
	assert.Equal(t, redactedValue, got.Metadata["provider"])
	assert.Equal(t, redactedValue, got.Metadata["tool_call_id"])
	assert.Equal(t, redactedValue, got.Metadata["mode"])
	assert.Equal(t, redactedValue, got.Metadata["waves"])
	assert.Equal(t, "3", got.Metadata["count"])
	assert.True(t, got.Redacted)

	data, err := json.Marshal(got)
	require.NoError(t, err)

	assert.NotContains(t, string(data), "/Users/example")
	assert.NotContains(t, string(data), `C:\Users\example`)
	assert.NotContains(t, string(data), "file:///Users/example")
	assert.NotContains(t, string(data), "~/private")
	assert.NotContains(t, string(data), `\Users\example`)
}

func TestSanitizeEventForHook_RedactsEmbeddedPathMetadataValues(t *testing.T) {
	t.Parallel()

	got := sanitizeEventForHook(Event{
		Type: CommandExecute,
		Metadata: map[string]string{
			"source":       "log=/Users/example/private/output.log",
			"provider":     `cwd:C:\Users\example\secret.txt`,
			"tool_call_id": "see file:///Users/example/private/trace.json",
			"mode":         "from ~/private/session.log",
			"waves":        "2",
		},
	}, PayloadMetadata)

	assert.Equal(t, redactedValue, got.Metadata["source"])
	assert.Equal(t, redactedValue, got.Metadata["provider"])
	assert.Equal(t, redactedValue, got.Metadata["tool_call_id"])
	assert.Equal(t, redactedValue, got.Metadata["mode"])
	assert.Equal(t, "2", got.Metadata["waves"])
	assert.True(t, got.Redacted)

	data, err := json.Marshal(got)
	require.NoError(t, err)

	assert.NotContains(t, string(data), "/Users/example")
	assert.NotContains(t, string(data), `C:\Users\example`)
	assert.NotContains(t, string(data), "file:///Users/example")
	assert.NotContains(t, string(data), "~/private")
}

func TestSanitizeEventForHook_DropsArbitraryMetadataWhileKeepingAllowlist(t *testing.T) {
	t.Parallel()

	metadata := map[string]string{
		"source": "llm_tool",
	}
	for i := range 1000 {
		metadata["unknown_"+strconv.Itoa(i)] = "secret=sk-metasecret1234567890"
	}

	got := sanitizeEventForHook(Event{
		Type:     CommandOutput,
		Metadata: metadata,
	}, PayloadMetadata)

	assert.Equal(t, map[string]string{"source": "llm_tool"}, got.Metadata)
	assert.True(t, got.Redacted)
}

func TestSanitizeEventForHook_KeepsRouteDecisionProfileModeMetadata(t *testing.T) {
	t.Parallel()

	got := sanitizeEventForHook(Event{
		Type: RouteDecision,
		Metadata: map[string]string{
			"selected":    "openai/gpt-4.1-mini",
			"interactive": "true",
			"batch":       "true",
			"raw_prompt":  "secret=sk-metasecret1234567890",
		},
	}, PayloadMetadata)

	assert.Equal(t, "openai/gpt-4.1-mini", got.Metadata["selected"])
	assert.Equal(t, "true", got.Metadata["interactive"])
	assert.Equal(t, "true", got.Metadata["batch"])
	assert.NotContains(t, got.Metadata, "raw_prompt")
	assert.True(t, got.Redacted)
}

func TestSanitizeEventForHook_FileEventsFullDropFileContent(t *testing.T) {
	t.Parallel()

	for _, eventType := range []string{FileRead, FileWrite, ContextAdd} {
		got := sanitizeEventForHook(Event{
			Type:    eventType,
			Content: "file content with token=sk-filecontentsecret1234567890",
			Metadata: map[string]string{
				"kind": "file",
				"path": "/Users/example/private/secret.txt",
			},
		}, PayloadFull)

		data, err := json.Marshal(got)
		require.NoError(t, err)

		assert.Empty(t, got.Content, eventType)
		assert.Equal(t, "file", got.Metadata["kind"], eventType)
		assert.Equal(t, "/Users/example/private/secret.txt", got.Metadata["path"], eventType)
		assert.True(t, got.Redacted, eventType)
		assert.NotContains(t, string(data), "file content", eventType)
		assert.NotContains(t, string(data), "sk-filecontentsecret", eventType)
	}
}

func TestSanitizeEventForHook_BoundsSerializedPayload(t *testing.T) {
	t.Parallel()

	got := sanitizeEventForHook(Event{
		Type:        CommandOutput,
		SessionID:   strings.Repeat("session", maxScalarBytes),
		SessionPath: "/Users/example/" + strings.Repeat("private/", maxMetadataValueBytes),
		Agent:       strings.Repeat("agent", maxScalarBytes),
		Model:       strings.Repeat("model", maxScalarBytes),
		Content:     "OPENAI_API_KEY=sk-contentsecret1234567890 " + strings.Repeat("content", maxContentBytes),
		Error:       "bearer abcdefghijklmnopqrstuvwxyz " + strings.Repeat("error", maxErrorBytes),
		Metadata: map[string]string{
			"command":      "cat ~/.env " + strings.Repeat("arg", maxMetadataValueBytes),
			"cwd":          "/Users/example/" + strings.Repeat("project/", maxMetadataValueBytes),
			"source":       strings.Repeat("source", maxMetadataValueBytes),
			"tool_call_id": strings.Repeat("tool", maxMetadataValueBytes),
			"api_token":    "sk-metasecret1234567890",
		},
	}, PayloadFull)

	data, err := json.Marshal(got)
	require.NoError(t, err)

	assert.LessOrEqual(t, len(data)+1, maxHookPayloadBytes, "payload plus newline should stay within the hook byte limit")
	assert.True(t, got.Redacted)
	assert.True(t, got.Truncated)

	for _, leaked := range []string{
		"sk-contentsecret",
		"abcdefghijklmnopqrstuvwxyz",
		"sk-metasecret",
	} {
		assert.NotContains(t, string(data), leaked)
	}
}

func TestSanitizeEventForHook_ShrinksEscapedSerializedPayload(t *testing.T) {
	t.Parallel()

	got := sanitizeEventForHook(Event{
		Type:    CommandOutput,
		Content: strings.Repeat("\x00", maxContentBytes),
		Error:   strings.Repeat("\x00", maxErrorBytes),
		Metadata: map[string]string{
			"command":      strings.Repeat("\x00", maxMetadataValueBytes),
			"cwd":          strings.Repeat("\x00", maxMetadataValueBytes),
			"source":       strings.Repeat("\x00", maxMetadataValueBytes),
			"tool_call_id": strings.Repeat("\x00", maxMetadataValueBytes),
		},
	}, PayloadFull)

	data, err := json.Marshal(got)
	require.NoError(t, err)

	assert.LessOrEqual(t, len(data)+1, maxHookPayloadBytes, "payload plus newline should stay within the hook byte limit after JSON escaping")
	assert.Empty(t, got.Content)
	assert.Empty(t, got.Error)
	assert.Empty(t, got.Metadata)
	assert.Contains(t, got.ContentSummary, "payload_limit=true")
	assert.Contains(t, got.ErrorSummary, "payload_limit=true")
	assert.True(t, got.Redacted)
	assert.True(t, got.Truncated)
}

func TestSanitizeValue_RedactsSecretCrossingOutputLimit(t *testing.T) {
	t.Parallel()

	prefix := strings.Repeat("x", maxMetadataValueBytes-len(redactedValue)-1)
	value := prefix + "sk-" + strings.Repeat("a", 40)

	got, redacted, truncated := sanitizeValue("content", value, maxMetadataValueBytes)

	assert.True(t, redacted)
	assert.True(t, truncated)
	assert.Contains(t, got, redactedValue)
	assert.NotContains(t, got, "sk-")
	assert.NotContains(t, got, strings.Repeat("a", 20))
	assert.LessOrEqual(t, len(got), maxMetadataValueBytes)
}

func TestSanitizeValue_RedactsCommonSecretPatterns(t *testing.T) {
	t.Parallel()

	value := strings.Join([]string{
		"google=AIza" + strings.Repeat("A", 35),
		"gitlab=glpat-" + strings.Repeat("G", 24),
		"gitlab_runner=glrt-" + strings.Repeat("R", 24),
		"npm=npm_" + strings.Repeat("N", 36),
		"pypi=pypi-" + strings.Repeat("P", 32),
		"stripe=sk_live_" + strings.Repeat("B", 24),
		"google_oauth=ya29." + strings.Repeat("Y", 32),
		"sendgrid=SG." + strings.Repeat("C", 20) + "." + strings.Repeat("D", 20),
		"slack=https://hooks.slack.com/services/T00000000/B00000000/" + strings.Repeat("E", 24),
		"discord=https://discord.com/api/webhooks/123456789012345678/" + strings.Repeat("F", 32),
		"sas=https://blob.example.invalid/container?sv=2024-01-01&sig=plain-sas-signature&se=2026-01-01",
	}, " ")

	got, redacted, _ := sanitizeValue("content", value, maxContentBytes)

	assert.True(t, redacted)
	assert.NotContains(t, got, "AIza")
	assert.NotContains(t, got, "glpat-")
	assert.NotContains(t, got, "glrt-")
	assert.NotContains(t, got, "npm_")
	assert.NotContains(t, got, "pypi-")
	assert.NotContains(t, got, "sk_live_")
	assert.NotContains(t, got, "ya29.")
	assert.NotContains(t, got, "SG.")
	assert.NotContains(t, got, "hooks.slack.com")
	assert.NotContains(t, got, "discord.com/api/webhooks")
	assert.NotContains(t, got, "plain-sas-signature")
	assert.Contains(t, got, redactedValue)
}

func TestSanitizeValue_RedactsAWSAccessKeyIDs(t *testing.T) {
	t.Parallel()

	value := "primary=AKIA1234567890ABCDEF temporary=ASIA1234567890ABCDEF"

	got, redacted, _ := sanitizeValue("content", value, maxContentBytes)

	assert.True(t, redacted)
	assert.NotContains(t, got, "AKIA1234567890ABCDEF")
	assert.NotContains(t, got, "ASIA1234567890ABCDEF")
	assert.Contains(t, got, redactedValue)
}

func TestSanitizeValue_RedactsQuotedSecretAssignments(t *testing.T) {
	t.Parallel()

	value := `{"password":"plain-password","client_secret": "plain-secret", 'api_token': 'plain-token'}`

	got, redacted, _ := sanitizeValue("content", value, maxContentBytes)

	assert.True(t, redacted)
	assert.Contains(t, got, redactedValue)
	assert.NotContains(t, got, "plain-password")
	assert.NotContains(t, got, "plain-secret")
	assert.NotContains(t, got, "plain-token")
}

func TestSanitizeValue_RedactsEscapedQuotedSecretAssignments(t *testing.T) {
	t.Parallel()

	value := `{\"password\":\"plain-password\",\"client_secret\":\"plain-secret\"}`

	got, redacted, _ := sanitizeValue("content", value, maxContentBytes)

	assert.True(t, redacted)
	assert.Contains(t, got, redactedValue)
	assert.NotContains(t, got, "plain-password")
	assert.NotContains(t, got, "plain-secret")
}

func TestSanitizeValue_RedactsHyphenatedAndDottedSecretAssignments(t *testing.T) {
	t.Parallel()

	value := strings.Join([]string{
		"aws-access-key-id=plain-access-key",
		"AccountKey=plain-account-key",
		"account-key=plain-hyphen-account-key",
		"api.key=plain-api-key",
		"access.key=plain-dotted-access-key",
		"private.key=plain-private-key",
		"client.secret=plain-client-secret",
		"x-signature=plain-request-signature",
		"webhook=https://example.invalid/plain-webhook-secret",
		"db-pwd=plain-db-password",
	}, " ")

	got, redacted, _ := sanitizeValue("content", value, maxContentBytes)

	assert.True(t, redacted)
	assert.Contains(t, got, redactedValue)
	assert.NotContains(t, got, "plain-access-key")
	assert.NotContains(t, got, "plain-account-key")
	assert.NotContains(t, got, "plain-hyphen-account-key")
	assert.NotContains(t, got, "plain-api-key")
	assert.NotContains(t, got, "plain-dotted-access-key")
	assert.NotContains(t, got, "plain-private-key")
	assert.NotContains(t, got, "plain-client-secret")
	assert.NotContains(t, got, "plain-request-signature")
	assert.NotContains(t, got, "plain-webhook-secret")
	assert.NotContains(t, got, "plain-db-password")
}

func TestSanitizeValue_RedactsSpacedSecretAssignments(t *testing.T) {
	t.Parallel()

	value := strings.Join([]string{
		"api key: plain-api-key",
		"access token = plain-access-token",
		"id token: plain-id-token",
		"oauth token: plain-oauth-token",
		"personal access token: plain-personal-access-token",
		"client secret: plain-client-secret",
		"secret key=plain-secret-key",
		"private key: plain-private-key",
	}, " ")

	got, redacted, _ := sanitizeValue("content", value, maxContentBytes)

	assert.True(t, redacted)
	assert.Contains(t, got, redactedValue)

	for _, leaked := range []string{
		"plain-api-key",
		"plain-access-token",
		"plain-id-token",
		"plain-oauth-token",
		"plain-personal-access-token",
		"plain-client-secret",
		"plain-secret-key",
		"plain-private-key",
	} {
		assert.NotContains(t, got, leaked)
	}
}

func TestSanitizeValue_RedactsBasicAuthCredentials(t *testing.T) {
	t.Parallel()

	value := "Authorization: Basic dXNlcjpwYXNzd29yZA=="

	got, redacted, _ := sanitizeValue("content", value, maxContentBytes)

	assert.True(t, redacted)
	assert.Contains(t, got, redactedValue)
	assert.NotContains(t, got, "dXNlcjpwYXNzd29yZA")
	assert.NotContains(t, got, "user")
	assert.NotContains(t, got, "password")
}

func TestSanitizeValue_RedactsAuthorizationSchemeCredentials(t *testing.T) {
	t.Parallel()

	value := "Authorization: Token plain-session-token\nProxy-Authorization: ApiKey plain-proxy-token"

	got, redacted, _ := sanitizeValue("content", value, maxContentBytes)

	assert.True(t, redacted)
	assert.Contains(t, got, redactedValue)
	assert.NotContains(t, got, "plain-session-token")
	assert.NotContains(t, got, "plain-proxy-token")
}

func TestSanitizeValue_RedactsCookieAndSessionCredentials(t *testing.T) {
	t.Parallel()

	value := strings.Join([]string{
		"Set-Cookie: sessionid=plain-cookie-secret; Path=/; HttpOnly",
		"Cookie: theme=light; session=plain-cookie-session",
		"session_id=plain-session-id csrf_token=plain-csrf-token",
	}, "\n")

	got, redacted, _ := sanitizeValue("content", value, maxContentBytes)

	assert.True(t, redacted)
	assert.Contains(t, got, "Set-Cookie:"+redactedValue)
	assert.Contains(t, got, "Cookie:"+redactedValue)
	assert.Contains(t, got, "session_id="+redactedValue)
	assert.Contains(t, got, "csrf_token="+redactedValue)
	assert.NotContains(t, got, "plain-cookie-secret")
	assert.NotContains(t, got, "plain-cookie-session")
	assert.NotContains(t, got, "plain-session-id")
	assert.NotContains(t, got, "plain-csrf-token")
}

func TestSanitizeValue_RedactsURLCredentials(t *testing.T) {
	t.Parallel()

	value := "repo=https://user:plain-password@example.com/private.git " +
		"mirror=ssh://oauth:plain-token@git.example.com/repo " +
		"database=postgres://dbuser:plain-db-password@db.example.com/app"

	got, redacted, _ := sanitizeValue("content", value, maxContentBytes)

	assert.True(t, redacted)
	assert.Contains(t, got, "https://"+redactedValue+"@example.com/private.git")
	assert.Contains(t, got, "ssh://"+redactedValue+"@git.example.com/repo")
	assert.Contains(t, got, "postgres://"+redactedValue+"@db.example.com/app")
	assert.NotContains(t, got, "user:plain-password")
	assert.NotContains(t, got, "oauth:plain-token")
	assert.NotContains(t, got, "dbuser:plain-db-password")
}

func TestSanitizeValue_BoundsInputBeforeUTF8Cleanup(t *testing.T) {
	t.Parallel()

	value := strings.Repeat("x", maxSecretScanBytes*4) + "\xffsk-" + strings.Repeat("a", 40)

	got, redacted, truncated := sanitizeValue("content", value, maxMetadataValueBytes)

	assert.False(t, redacted, "secrets outside the bounded scan window must not force unbounded processing")
	assert.True(t, truncated)
	assert.True(t, utf8.ValidString(got))
	assert.NotContains(t, got, "sk-")
	assert.LessOrEqual(t, len(got), maxMetadataValueBytes)
}

func TestSanitizeEventForHook_RedactsPrivateKeysInFullPayload(t *testing.T) {
	t.Parallel()

	got := sanitizeEventForHook(Event{
		Type: CommandOutput,
		Content: "before -----BEGIN OPENSSH PRIVATE KEY-----\n" +
			"private-key-material\n" +
			"-----END OPENSSH PRIVATE KEY----- after",
		Metadata: map[string]string{
			"source": "tool",
		},
	}, PayloadFull)

	assert.Contains(t, got.Content, "before ")
	assert.Contains(t, got.Content, redactedValue)
	assert.Contains(t, got.Content, " after")
	assert.NotContains(t, got.Content, "PRIVATE KEY")
	assert.NotContains(t, got.Content, "private-key-material")
	assert.True(t, got.Redacted)
}

func TestSanitizeEventForHook_RedactsOversizedPrivateKeyPrefix(t *testing.T) {
	t.Parallel()

	got := sanitizeEventForHook(Event{
		Type: CommandOutput,
		Content: "prefix -----BEGIN OPENSSH PRIVATE KEY-----\n" +
			strings.Repeat("private-key-material\n", maxContentBytes),
		Metadata: map[string]string{
			"source": "tool",
		},
	}, PayloadFull)

	assert.Contains(t, got.Content, "prefix ")
	assert.Contains(t, got.Content, redactedValue)
	assert.NotContains(t, got.Content, "PRIVATE KEY")
	assert.NotContains(t, got.Content, "private-key-material")
	assert.LessOrEqual(t, len(got.Content), maxContentBytes)
	assert.True(t, got.Redacted)
	assert.True(t, got.Truncated)
}

func TestSanitizeEventForHook_SummaryDetectsKnownSecretWithoutContent(t *testing.T) {
	t.Parallel()

	got := sanitizeEventForHook(Event{
		Type: AssistantMessage,
		Content: "-----BEGIN OPENSSH PRIVATE KEY-----\n" +
			"private-key-material\n" +
			"-----END OPENSSH PRIVATE KEY-----",
	}, PayloadSummary)

	assert.Empty(t, got.Content)
	assert.Contains(t, got.ContentSummary, "content redacted bytes=")
	assert.Contains(t, got.ContentSummary, "known_secret=true")
	assert.NotContains(t, got.ContentSummary, "PRIVATE KEY")
	assert.NotContains(t, got.ContentSummary, "private-key-material")
	assert.True(t, got.Redacted)
}
