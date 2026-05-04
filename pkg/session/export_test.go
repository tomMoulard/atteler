package session

import (
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/tommoulard/atteler/pkg/llm"
)

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

	got := Markdown(session)
	for _, want := range []string{
		"# Atteler Session abc",
		"- **Created:** 2026-04-30T10:00:00Z",
		"- **Updated:** 2026-04-30T10:05:00Z",
		"- **Agent:** reviewer",
		"- **Model:** gpt-test",
		"### User\n\nhello",
		"### Assistant\n\nhi",
	} {
		if !strings.Contains(got, want) {
			require.Failf(t, "unexpected failure", "Markdown missing %q in:\n%s", want, got)
		}
	}
}

func TestMarkdown_EmptyTranscript(t *testing.T) {
	t.Parallel()

	got := Markdown(Session{})
	if !strings.Contains(got, "_No messages._") {
		require.Failf(t, "unexpected failure", "Markdown = %q, want empty marker", got)
	}
}

func TestMarkdown_UsesTitle(t *testing.T) {
	t.Parallel()

	got := Markdown(Session{ID: "abc", Title: "Auth review", Tags: []string{"auth", "review"}})
	for _, want := range []string{
		"# Auth review",
		"- **Session:** abc",
		"- **Tags:** auth, review",
	} {
		if !strings.Contains(got, want) {
			require.Failf(t, "unexpected failure", "Markdown missing %q in:\n%s", want, got)
		}
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

	got := Markdown(session)
	for _, want := range []string{
		"## Negative Knowledge",
		"- **Approach:** Patch token refresh timer",
		"  - **Reason:** Created retry storms",
		"  - **Commit:** abc123",
		"  - **Agent:** reviewer",
		"  - **Created:** 2026-04-30T11:00:00Z",
		"_No messages._",
	} {
		if !strings.Contains(got, want) {
			require.Failf(t, "unexpected failure", "Markdown missing %q in:\n%s", want, got)
		}
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

	got := Markdown(session)
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
		if !strings.Contains(got, want) {
			require.Failf(t, "unexpected failure", "Markdown missing %q in:\n%s", want, got)
		}
	}
}
