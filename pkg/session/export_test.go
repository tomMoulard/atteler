package session

import (
	"strings"
	"testing"
	"time"

	"github.com/tommoulard/atteler/pkg/llm"
)

func TestMarkdown_RendersTranscript(t *testing.T) {
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
			t.Fatalf("Markdown missing %q in:\n%s", want, got)
		}
	}
}

func TestMarkdown_EmptyTranscript(t *testing.T) {
	got := Markdown(Session{})
	if !strings.Contains(got, "_No messages._") {
		t.Fatalf("Markdown = %q, want empty marker", got)
	}
}

func TestMarkdown_UsesTitle(t *testing.T) {
	got := Markdown(Session{ID: "abc", Title: "Auth review", Tags: []string{"auth", "review"}})
	for _, want := range []string{
		"# Auth review",
		"- **Session:** abc",
		"- **Tags:** auth, review",
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("Markdown missing %q in:\n%s", want, got)
		}
	}
}
