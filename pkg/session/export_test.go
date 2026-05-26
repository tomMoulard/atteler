package session

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/tommoulard/atteler/pkg/llm"
)

var fixedExportedAt = time.Date(2026, 4, 30, 14, 0, 0, 0, time.UTC)

func shareableTestOptions() ExportOptions {
	return ExportOptions{Profile: ExportProfileShareable, ExportedAt: fixedExportedAt}
}

func credentialLike(value string) string {
	return "sk-" + "proj-" + value + strings.Repeat("x", 24)
}

func TestMarkdown_RendersTranscript(t *testing.T) {
	t.Parallel()

	session := Session{
		ID:           "abc",
		CreatedAt:    time.Date(2026, 4, 30, 10, 0, 0, 0, time.UTC),
		UpdatedAt:    time.Date(2026, 4, 30, 10, 5, 0, 0, time.UTC),
		DefaultAgent: "reviewer",
		DefaultModel: "gpt-test",
		Messages: []llm.Message{
			{Role: llm.RoleUser, Content: "hello"},
			{Role: llm.RoleAssistant, Content: "hi"},
		},
	}

	got := MarkdownWithOptions(session, shareableTestOptions())
	for _, want := range []string{
		"# Atteler Session abc",
		"- **Created:** 2026-04-30T10:00:00Z",
		"- **Updated:** 2026-04-30T10:05:00Z",
		"- **Agent:** reviewer",
		"- **Model:** gpt-test",
		"## Export Manifest",
		"- **Redaction profile:** redacted-shareable",
		"### User\n\n```text\nhello\n```",
		"### Assistant\n\n```text\nhi\n```",
	} {
		assert.Contains(t, got, want)
	}
}

func TestMarkdown_EmptyTranscript(t *testing.T) {
	t.Parallel()

	got := MarkdownWithOptions(Session{}, shareableTestOptions())
	assert.Contains(t, got, "_No messages._")
	assert.Contains(t, got, "- **Omitted sections:** none")
}

func TestMarkdown_UsesTitle(t *testing.T) {
	t.Parallel()

	got := MarkdownWithOptions(Session{ID: "abc", Title: "Auth review", Tags: []string{"auth", "review"}}, shareableTestOptions())
	for _, want := range []string{
		"# Auth review",
		"- **Session:** abc",
		"- **Tags:** auth, review",
	} {
		assert.Contains(t, got, want)
	}
}

func TestMarkdown_RendersNegativeKnowledge(t *testing.T) {
	t.Parallel()

	session := Session{
		ID: "abc",
		NegativeKnowledge: []NegativeKnowledge{
			{
				Approach:  "Patch token refresh timer",
				Reason:    "Created retry storms",
				Commit:    "abc123",
				Agent:     "reviewer",
				TaskType:  "migration",
				Severity:  "critical",
				CreatedAt: time.Date(2026, 4, 30, 11, 0, 0, 0, time.UTC),
			},
		},
	}

	got := MarkdownWithOptions(session, shareableTestOptions())
	for _, want := range []string{
		"## Negative Knowledge",
		"- **Approach:** Patch token refresh timer",
		"  - **Reason:** Created retry storms",
		"  - **Commit:** abc123",
		"  - **Agent:** reviewer",
		"  - **Task Type:** migration",
		"  - **Severity:** critical",
		"  - **Created:** 2026-04-30T11:00:00Z",
		"_No messages._",
	} {
		assert.Contains(t, got, want)
	}
}

func TestMarkdown_RendersEvaluationsAndArtifacts(t *testing.T) {
	t.Parallel()

	session := Session{
		ID: "abc",
		Evaluations: []AgentEvaluation{{
			Agent:           "reviewer",
			Outcome:         "pass",
			Notes:           "caught issue",
			Reference:       "eval.md",
			Source:          EvaluationSourceHarness,
			Evaluator:       "eval-bot",
			RubricVersion:   "review/v2",
			TaskType:        "code-review",
			Difficulty:      "hard",
			ExpectedOutcome: "catch regression",
			Model:           "gpt-test",
			AgentVersion:    "reviewer@abc123",
			SchemaVersion:   AgentEvaluationSchemaVersion,
			Score:           90,
			DurationMillis:  1200,
			Cost:            0.012300,
			Confidence:      0.91,
			CreatedAt:       time.Date(2026, 4, 30, 12, 0, 0, 0, time.UTC),
		}},
		Artifacts: []Artifact{{
			Path:        "docs/research.md",
			Kind:        "research",
			Summary:     "OAuth notes",
			SourceAgent: "researcher",
			CreatedAt:   time.Date(2026, 4, 30, 13, 0, 0, 0, time.UTC),
		}},
	}

	got := MarkdownWithOptions(session, shareableTestOptions())
	for _, want := range []string{
		"## Agent Evaluations",
		"- **Agent:** reviewer",
		"  - **Outcome:** pass",
		"  - **Score:** 90",
		"  - **Source:** harness",
		"  - **Evaluator:** eval-bot",
		"  - **Rubric Version:** review/v2",
		"  - **Task Type:** code-review",
		"  - **Difficulty:** hard",
		"  - **Expected Outcome:** catch regression",
		"  - **Model:** gpt-test",
		"  - **Agent Version:** reviewer@abc123",
		"  - **Schema Version:** 1",
		"  - **Duration Millis:** 1200",
		"  - **Cost:** 0.012300",
		"  - **Confidence:** 0.91",
		"## Artifacts",
		"- **Path:** docs/research.md",
		"  - **Kind:** research",
		"  - **Source Agent:** researcher",
	} {
		assert.Contains(t, got, want)
	}
}

func TestMarkdown_DefaultShareableRedactsSecretsAndAbsolutePaths(t *testing.T) {
	t.Parallel()

	const (
		openAIKey    = "sk-proj-abcdefghijklmnopqrstuvwx1234567890"
		bearerToken  = "abcdefghijklmnopqrstuvwxyz1234567890"
		customSecret = "ultra-private-value"
		quotedSecret = "two words secret"
	)

	dsnMarker := "p" + "@" + "ssw0rd"
	awsMarker := "aws" + strings.Repeat("x", 40)
	githubMarker := "github" + strings.Repeat("x", 40)
	session := Session{
		ID:           "abc",
		WorktreePath: "/Users/tom/work/private-repo",
		DefaultModel: openAIKey,
		Tags:         []string{"token=" + customSecret},
		Messages: []llm.Message{{
			Role: llm.RoleUser,
			Content: strings.Join([]string{
				"Authorization: Bearer " + bearerToken,
				"OPENAI_API_KEY=" + openAIKey,
				"AWS_SECRET_ACCESS_KEY=" + awsMarker,
				"GITHUB_TOKEN=" + githubMarker,
				"DATABASE_URL=postgres://alice:" + dsnMarker + "@db.example/app",
				"tenant_secret=" + customSecret,
				"password=\"" + quotedSecret + "\"",
				"read /Users/tom/work/private-repo/.env",
				"temp /tmp",
				"home ~/private/project/.env",
				"path:/Users/tom/work/private-repo/config.yaml",
				"file='/Users/tom/My Project/config.yaml'",
				"root=\"C:\\Users\\tom\\My Project\"",
				"open file:///Users/tom/work/private-repo/config.yaml",
				"share \\\\server\\share\\secret.txt",
				"Windows C:\\Users\\tom\\secret.txt",
			}, "\n"),
		}},
		Artifacts: []Artifact{{Path: "/Users/tom/work/private-repo/report.md", Kind: "note"}},
	}

	got := MarkdownWithOptions(session, ExportOptions{
		Profile:         ExportProfileShareable,
		ExportedAt:      fixedExportedAt,
		SensitiveFields: []string{"tenant_secret"},
	})

	assert.NotContains(t, got, openAIKey)
	assert.NotContains(t, got, bearerToken)
	assert.NotContains(t, got, awsMarker)
	assert.NotContains(t, got, githubMarker)
	assert.NotContains(t, got, dsnMarker)
	assert.NotContains(t, got, "alice:"+dsnMarker)
	assert.NotContains(t, got, customSecret)
	assert.NotContains(t, got, quotedSecret)
	assert.NotContains(t, got, "/Users/tom")
	assert.NotContains(t, got, "/tmp")
	assert.NotContains(t, got, "~/private")
	assert.NotContains(t, got, "My Project")
	assert.NotContains(t, got, `\\server\share`)
	assert.NotContains(t, got, `C:\Users\tom`)
	assert.Contains(t, got, "[REDACTED]")
	assert.Contains(t, got, "[REDACTED_PATH]")
}

func TestBuildMachineReadableExport_ExcludedFieldsOmitByPolicy(t *testing.T) {
	t.Parallel()

	session := Session{
		ID:           "abc",
		Title:        "Private title",
		CreatedAt:    time.Date(2026, 4, 30, 10, 0, 0, 0, time.UTC),
		UpdatedAt:    time.Date(2026, 4, 30, 11, 0, 0, 0, time.UTC),
		DefaultAgent: "reviewer",
		DefaultModel: "gpt-secret",
		WorktreePath: "/private/repo",
		Tags:         []string{"private-tag"},
		Messages:     []llm.Message{{Role: llm.RoleUser, Content: "private transcript"}},
		NegativeKnowledge: []NegativeKnowledge{{
			Approach:  "private failed approach",
			Reason:    "private failure reason",
			Agent:     "reviewer",
			CreatedAt: time.Date(2026, 4, 30, 12, 0, 0, 0, time.UTC),
		}},
		Evaluations: []AgentEvaluation{{
			Agent:     "reviewer",
			Outcome:   "private evaluation",
			CreatedAt: time.Date(2026, 4, 30, 13, 0, 0, 0, time.UTC),
		}},
		Artifacts: []Artifact{{
			Path:      "/private/artifact.md",
			Kind:      "private artifact",
			Summary:   "private artifact summary",
			CreatedAt: time.Date(2026, 4, 30, 14, 0, 0, 0, time.UTC),
		}},
	}

	export := BuildMachineReadableExport(session, ExportOptions{
		Profile:    ExportProfilePrivate,
		ExportedAt: fixedExportedAt,
		ExcludedFields: []SearchField{
			SearchFieldTranscript,
			SearchFieldFailures,
			SearchFieldEvaluations,
			SearchFieldArtifacts,
			SearchFieldTags,
			SearchFieldRepo,
			SearchFieldAgent,
			SearchFieldModel,
			SearchFieldDate,
			SearchFieldSession,
			SearchFieldTitle,
		},
	})

	assert.Empty(t, export.Messages)
	assert.Empty(t, export.NegativeKnowledge)
	assert.Empty(t, export.Evaluations)
	assert.Empty(t, export.Artifacts)
	assert.Empty(t, export.Session.Tags)
	assert.Empty(t, export.Session.WorktreePath)
	assert.Empty(t, export.Session.DefaultAgent)
	assert.Empty(t, export.Session.DefaultModel)
	assert.Empty(t, export.Session.ID)
	assert.Empty(t, export.Session.Title)
	assert.Empty(t, export.Manifest.SessionID)
	assert.True(t, export.Manifest.ExportedAt.IsZero())
	assert.True(t, export.Session.CreatedAt.IsZero())
	assert.True(t, export.Session.UpdatedAt.IsZero())
	assert.Equal(t, 1, export.Session.MessageCount)
	assert.Equal(t, 1, export.Session.NegativeKnowledgeCount)
	assert.Equal(t, 1, export.Session.EvaluationCount)
	assert.Equal(t, 1, export.Session.ArtifactCount)
	assert.Contains(t, export.Manifest.OmittedSections, "transcript omitted by export field policy")
	assert.Contains(t, export.Manifest.OmittedSections, "negative knowledge omitted by export field policy")
	assert.Contains(t, export.Manifest.OmittedSections, "evaluations omitted by export field policy")
	assert.Contains(t, export.Manifest.OmittedSections, "artifacts omitted by export field policy")
	assert.Contains(t, export.Manifest.OmittedSections, "session.tags omitted by export field policy")
	assert.Contains(t, export.Manifest.OmittedSections, "session.worktree_path omitted by export field policy")
	assert.Contains(t, export.Manifest.OmittedSections, "manifest.exported_at omitted by export field policy")
}

func TestBuildMachineReadableExport_MetadataPolicyDoesNotLeakNestedFields(t *testing.T) {
	t.Parallel()

	session := Session{
		ID: "abc",
		NegativeKnowledge: []NegativeKnowledge{{
			Approach:  "cache patch",
			Reason:    "broke auth",
			Agent:     "reviewer",
			CreatedAt: time.Date(2026, 4, 30, 12, 0, 0, 0, time.UTC),
		}},
		Evaluations: []AgentEvaluation{{
			Agent:     "reviewer",
			Outcome:   "pass",
			Notes:     "retained notes",
			CreatedAt: time.Date(2026, 4, 30, 13, 0, 0, 0, time.UTC),
		}},
		Artifacts: []Artifact{{
			Path:        "docs/research.md",
			Kind:        "research",
			SourceAgent: "reviewer",
			CreatedAt:   time.Date(2026, 4, 30, 14, 0, 0, 0, time.UTC),
		}},
	}

	export := BuildMachineReadableExport(session, ExportOptions{
		Profile:        ExportProfilePrivate,
		ExportedAt:     fixedExportedAt,
		ExcludedFields: []SearchField{SearchFieldAgent, SearchFieldDate},
	})

	require.Len(t, export.NegativeKnowledge, 1)
	require.Len(t, export.Evaluations, 1)
	require.Len(t, export.Artifacts, 1)
	assert.Equal(t, "cache patch", export.NegativeKnowledge[0].Approach)
	assert.Equal(t, "retained notes", export.Evaluations[0].Notes)
	assert.Equal(t, "docs/research.md", export.Artifacts[0].Path)
	assert.Empty(t, export.NegativeKnowledge[0].Agent)
	assert.Empty(t, export.Evaluations[0].Agent)
	assert.Empty(t, export.Artifacts[0].SourceAgent)
	assert.True(t, export.NegativeKnowledge[0].CreatedAt.IsZero())
	assert.True(t, export.Evaluations[0].CreatedAt.IsZero())
	assert.True(t, export.Artifacts[0].CreatedAt.IsZero())
}

func TestMarkdown_DefaultUsesShareableProfile(t *testing.T) {
	t.Parallel()

	secret := credentialLike("default")
	session := Session{
		ID:           "abc",
		WorktreePath: "/Users/tom/private",
		Messages:     []llm.Message{{Role: llm.RoleUser, Content: "key=" + secret + " path=/Users/tom/private/.env"}},
	}

	got := Markdown(session)

	assert.Contains(t, got, "- **Redaction profile:** redacted-shareable")
	assert.Contains(t, got, "[REDACTED_PATH]")
	assert.NotContains(t, got, secret)
	assert.NotContains(t, got, "/Users/tom")
}

func TestMarkdown_PrivateProfileIsExplicitAndFullFidelity(t *testing.T) {
	t.Parallel()

	const openAIKey = "sk-proj-abcdefghijklmnopqrstuvwx1234567890"

	session := Session{
		ID:           "abc",
		WorktreePath: "/Users/tom/work/private-repo",
		Messages: []llm.Message{{
			Role:    llm.RoleUser,
			Content: "OPENAI_API_KEY=" + openAIKey + " at /Users/tom/work/private-repo/.env",
		}},
	}

	got := MarkdownWithOptions(session, ExportOptions{Profile: ExportProfilePrivate, ExportedAt: fixedExportedAt})

	assert.Contains(t, got, "Private full-fidelity export")
	assert.Contains(t, got, "- **Redaction profile:** private-full")
	assert.Contains(t, got, "- **Privacy notice:** Private full-fidelity export")
	assert.Contains(t, got, openAIKey)
	assert.Contains(t, got, "/Users/tom/work/private-repo/.env")
}

func TestMarkdown_PrivateProfileHonorsSensitiveFieldPolicy(t *testing.T) {
	t.Parallel()

	const (
		secret     = "ultra-private-value"
		toolSecret = "tool-private-value"
	)

	session := Session{
		ID:           "abc",
		WorktreePath: "/Users/tom/work/private-repo",
		Messages: []llm.Message{{
			Role:    llm.RoleUser,
			Content: "tenant_secret=" + secret + " path=/Users/tom/work/private-repo/.env",
			ToolCalls: []llm.ToolCall{{
				ID:    "call-1",
				Name:  "read_file",
				Input: map[string]any{"tenant_secret": toolSecret},
			}},
			ToolResult: &llm.ToolResult{ToolCallID: "call-1", Content: "tenant_secret=" + toolSecret},
		}},
	}

	got := MarkdownWithOptions(session, ExportOptions{
		Profile:         ExportProfilePrivate,
		ExportedAt:      fixedExportedAt,
		SensitiveFields: []string{"tenant_secret"},
	})
	export := BuildMachineReadableExport(session, ExportOptions{
		Profile:         ExportProfilePrivate,
		ExportedAt:      fixedExportedAt,
		SensitiveFields: []string{"tenant_secret"},
	})

	assert.Contains(t, got, "Private export with sensitive-field redaction")
	assert.Contains(t, got, "[REDACTED]")
	assert.Contains(t, got, "redacted private export")
	assert.NotContains(t, got, secret)
	assert.NotContains(t, got, toolSecret)
	assert.NotContains(t, got, "/Users/tom")
	require.Len(t, export.Messages, 1)
	assert.Empty(t, export.Messages[0].ToolCalls)
	assert.Nil(t, export.Messages[0].ToolResult)
	assert.Equal(t, 1, export.Messages[0].ToolCallCount)
	assert.True(t, export.Messages[0].ToolResultOmitted)
	assert.NotContains(t, export.Messages[0].Content, secret)
	assert.Contains(t, export.Manifest.PrivacyNotice, "sensitive-field redaction")
}

func TestBuildMachineReadableExport_SensitiveFieldNamesRedactWholeValues(t *testing.T) {
	t.Parallel()

	const (
		rawModel   = "raw-model-field-secret"
		rawTag     = "raw-tag-field-secret"
		rawContent = "raw-content-field-secret"
	)

	session := Session{
		ID:           "abc",
		DefaultModel: rawModel,
		Tags:         []string{rawTag},
		Messages:     []llm.Message{{Role: llm.RoleUser, Content: rawContent}},
	}

	export := BuildMachineReadableExport(session, ExportOptions{
		Profile:         ExportProfilePrivate,
		ExportedAt:      fixedExportedAt,
		SensitiveFields: []string{"default_model", "tags", "content"},
	})
	data, err := json.Marshal(export)
	require.NoError(t, err)

	assert.Equal(t, "[REDACTED]", export.Session.DefaultModel)
	assert.Equal(t, []string{"[REDACTED]"}, export.Session.Tags)
	require.Len(t, export.Messages, 1)
	assert.Equal(t, "[REDACTED]", export.Messages[0].Content)
	assert.NotContains(t, string(data), rawModel)
	assert.NotContains(t, string(data), rawTag)
	assert.NotContains(t, string(data), rawContent)
	assert.Contains(t, export.Manifest.PrivacyNotice, "sensitive-field redaction")
}

func TestMarkdown_FencesTranscriptAndEscapesInlineInjection(t *testing.T) {
	t.Parallel()

	session := Session{
		ID:    "abc",
		Title: "Review\n## injected title <script>",
		Messages: []llm.Message{{
			Role:    llm.RoleUser,
			Content: "before\n```\n## injected\n<script>alert(1)</script>\n```",
		}},
		Evaluations: []AgentEvaluation{{Agent: "reviewer", Outcome: "pass", Notes: "ok\n## injected note"}},
	}

	got := MarkdownWithOptions(session, shareableTestOptions())

	assert.Contains(t, got, "# Review \\#\\# injected title &lt;script&gt;")
	assert.NotContains(t, got, "Review\n## injected title")
	assert.Contains(t, got, "  - **Notes:** ok \\#\\# injected note")
	assert.NotContains(t, got, "  - **Notes:** ok\n## injected note")
	assert.Contains(t, got, "````text\nbefore\n```\n## injected\n<script>alert(1)</script>\n```\n````")
}

func TestMarkdown_TruncatesHugeTranscriptContent(t *testing.T) {
	t.Parallel()

	content := strings.Repeat("a", 30)
	session := Session{ID: "abc", Messages: []llm.Message{{Role: llm.RoleUser, Content: content}}}

	got := MarkdownWithOptions(session, ExportOptions{
		Profile:         ExportProfileShareable,
		ExportedAt:      fixedExportedAt,
		MaxContentRunes: 10,
	})

	assert.NotContains(t, got, content)
	assert.Contains(t, got, "aaaaaaaaaa\n\n[Truncated: omitted 20 runes]")
	assert.Contains(t, got, "truncated by 20 runes")
}

func TestMarkdown_LimitsTranscriptMessagesAndOmitsToolAttachments(t *testing.T) {
	t.Parallel()

	omittedMarker := credentialLike("omittedmessage")
	callMarker := credentialLike("toolcall")
	resultMarker := credentialLike("toolresult")

	session := Session{
		ID: "abc",
		Messages: []llm.Message{
			{
				Role:    llm.RoleAssistant,
				Content: "using a tool",
				ToolCalls: []llm.ToolCall{{
					ID:    "call-1",
					Name:  "read_file",
					Input: map[string]any{"path": "/Users/tom/private.txt", "token": callMarker},
				}},
				ToolResult: &llm.ToolResult{ToolCallID: "call-1", Content: "result " + resultMarker},
			},
			{Role: llm.RoleUser, Content: "keep this"},
			{Role: llm.RoleUser, Content: "omit this " + omittedMarker},
		},
	}

	options := ExportOptions{
		Profile:               ExportProfileShareable,
		ExportedAt:            fixedExportedAt,
		MaxTranscriptMessages: 2,
	}
	got := MarkdownWithOptions(session, options)
	export := BuildMachineReadableExport(session, options)

	require.Len(t, export.Messages, 2)
	assert.Equal(t, 1, export.Messages[0].ToolCallCount)
	assert.True(t, export.Messages[0].ToolResultOmitted)
	assert.Contains(t, got, "transcript messages 3-3 omitted by message limit 2")
	assert.Contains(t, got, "messages\\[1\\].tool\\_calls omitted from shareable export")
	assert.Contains(t, got, "messages\\[1\\].tool\\_result omitted from shareable export")
	assert.NotContains(t, got, omittedMarker)
	assert.NotContains(t, got, callMarker)
	assert.NotContains(t, got, resultMarker)
	assert.NotContains(t, got, "/Users/tom")
}

func TestIssueMarkdown_OmitsTranscriptBodies(t *testing.T) {
	t.Parallel()

	session := Session{
		ID:       "abc",
		Messages: []llm.Message{{Role: llm.RoleUser, Content: "private prompt body"}},
	}

	got := MarkdownWithOptions(session, ExportOptions{Profile: ExportProfileIssue, ExportedAt: fixedExportedAt})

	assert.Contains(t, got, "## Issue/PR Summary")
	assert.Contains(t, got, "transcript omitted by issue/PR summary profile")
	assert.Contains(t, got, "- **Messages:** 1 total, 0 exported")
	assert.NotContains(t, got, "private prompt body")
	assert.NotContains(t, got, "## Transcript")
}

func TestJSON_MachineReadableExportMatchesMarkdownRedaction(t *testing.T) {
	t.Parallel()

	const openAIKey = "sk-proj-abcdefghijklmnopqrstuvwx1234567890"

	session := Session{
		ID:       "abc",
		Messages: []llm.Message{{Role: llm.RoleUser, Content: "key=" + openAIKey + " path=/Users/tom/project"}},
	}
	options := shareableTestOptions()

	markdown := MarkdownWithOptions(session, options)
	data, err := JSONWithOptions(session, options)
	require.NoError(t, err)

	var decoded MachineReadableExport
	require.NoError(t, json.Unmarshal(data, &decoded))

	built := BuildMachineReadableExport(session, options)
	assert.Equal(t, built.Manifest.ContentHashes, decoded.Manifest.ContentHashes)
	require.Len(t, decoded.Messages, 1)
	assert.Equal(t, built.Messages[0].Content, decoded.Messages[0].Content)
	assert.Contains(t, markdown, decoded.Messages[0].Content)
	assert.Equal(t, ExportProfileShareable, decoded.Manifest.RedactionProfile)
	assert.Empty(t, decoded.Manifest.OmittedSections)
	assert.Contains(t, string(data), `"omitted_sections": []`)
	assert.NotContains(t, string(data), openAIKey)
	assert.NotContains(t, string(data), "/Users/tom")
	assert.NotContains(t, markdown, openAIKey)
	assert.NotContains(t, markdown, "/Users/tom")
}
