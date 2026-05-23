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
		ID:               "abc",
		CreatedAt:        time.Date(2026, 4, 30, 10, 0, 0, 0, time.UTC),
		UpdatedAt:        time.Date(2026, 4, 30, 10, 5, 0, 0, time.UTC),
		DefaultAgent:     "reviewer",
		DefaultModel:     "gpt-test",
		DefaultModelMode: "fast",
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
		"- **Mode:** fast",
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
			Agent:     "reviewer",
			Outcome:   "pass",
			Notes:     "caught issue",
			Reference: "eval.md",
			Score:     90,
			CreatedAt: time.Date(2026, 4, 30, 12, 0, 0, 0, time.UTC),
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
