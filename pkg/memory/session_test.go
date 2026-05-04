package memory

import (
	"reflect"
	"testing"
	"time"

	"github.com/tommoulard/atteler/pkg/llm"
	"github.com/tommoulard/atteler/pkg/session"
)

func TestFromSessions_IndexesMessagesAndMetadataWithStableSource(t *testing.T) {
	t.Parallel()

	created := time.Date(2026, 4, 30, 12, 0, 0, 0, time.UTC)
	saved := session.Session{
		ID:           "session-1",
		Title:        "OAuth rollout",
		DefaultAgent: "reviewer",
		DefaultModel: "gpt-test",
		CreatedAt:    created,
		UpdatedAt:    created.Add(time.Hour),
		Tags:         []string{"auth", "release"},
		Messages: []llm.Message{
			{Role: llm.RoleUser, Content: "Please review OAuth tokens and OAuth callback handling."},
			{Role: llm.RoleAssistant, Content: "Release notes mention docs only."},
		},
	}

	store, err := FromSessions(saved)
	if err != nil {
		t.Fatalf("FromSessions() error = %v", err)
	}

	if len(store.Documents) != 3 {
		t.Fatalf("documents len = %d, want metadata plus two messages", len(store.Documents))
	}

	metadata := findDocument(t, store, "session/session-1/metadata")
	if metadata.Metadata["source_type"] != "session" || metadata.Metadata["kind"] != "metadata" {
		t.Fatalf("metadata source = %#v, want session metadata", metadata.Metadata)
	}

	if metadata.Metadata["session_title"] != "OAuth rollout" {
		t.Fatalf("metadata title = %q, want OAuth rollout", metadata.Metadata["session_title"])
	}

	if metadata.Metadata["created_at"] != "2026-04-30T12:00:00Z" {
		t.Fatalf("created_at = %q, want RFC3339 UTC", metadata.Metadata["created_at"])
	}

	message := findDocument(t, store, "session/session-1/message/0")
	if message.Path != "session-1" {
		t.Fatalf("message path = %q, want session-1", message.Path)
	}

	if message.Metadata["role"] != "user" || message.Metadata["index"] != "0" {
		t.Fatalf("message metadata = %#v, want role user index 0", message.Metadata)
	}

	results, err := store.Search("oauth tokens", 2)
	if err != nil {
		t.Fatalf("Search() error = %v", err)
	}

	if len(results) == 0 {
		t.Fatal("Search() returned no results, want message match")
	}

	if results[0].Document.ID != "session/session-1/message/0" {
		t.Fatalf("top result = %q, want first message", results[0].Document.ID)
	}

	if !reflect.DeepEqual(results[0].Matches, []string{"oauth", "tokens"}) {
		t.Fatalf("matches = %#v, want oauth/tokens", results[0].Matches)
	}
}

func TestStore_AddSession_IndexesKnowledgeEvaluationsAndArtifacts(t *testing.T) {
	t.Parallel()

	saved := session.Session{
		ID:    "session-2",
		Title: "Quality review",
		NegativeKnowledge: []session.NegativeKnowledge{{
			Approach: "Patch token refresh timer",
			Reason:   "Created retry storms",
			Commit:   "abc123",
			Agent:    "reviewer",
		}},
		Evaluations: []session.AgentEvaluation{{
			Agent:     "verifier",
			Outcome:   "pass",
			Notes:     "Caught OAuth regression",
			Reference: "eval.md",
			Score:     90,
		}},
		Artifacts: []session.Artifact{{
			Path:        "docs/oauth.md",
			Kind:        "research",
			Summary:     "OAuth findings and redirect risks",
			SourceAgent: "researcher",
		}},
	}

	store := NewStore()
	if err := store.AddSession(saved); err != nil {
		t.Fatalf("AddSession() error = %v", err)
	}

	tests := []struct {
		name     string
		query    string
		wantID   string
		wantKind string
	}{
		{name: "negative knowledge", query: "retry storms", wantID: "session/session-2/negative_knowledge/0", wantKind: "negative_knowledge"},
		{name: "evaluation", query: "caught regression", wantID: "session/session-2/evaluation/0", wantKind: "evaluation"},
		{name: "artifact", query: "redirect risks", wantID: "session/session-2/artifact/0", wantKind: "artifact"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			results, err := store.Search(tc.query, 1)
			if err != nil {
				t.Fatalf("Search(%q) error = %v", tc.query, err)
			}

			if len(results) != 1 {
				t.Fatalf("Search(%q) len = %d, want 1: %#v", tc.query, len(results), results)
			}

			if results[0].Document.ID != tc.wantID {
				t.Fatalf("Search(%q) top ID = %q, want %q", tc.query, results[0].Document.ID, tc.wantID)
			}

			if results[0].Document.Metadata["kind"] != tc.wantKind {
				t.Fatalf("kind = %q, want %q", results[0].Document.Metadata["kind"], tc.wantKind)
			}

			if results[0].Document.Metadata["source_type"] != "session" || results[0].Document.Metadata["session_id"] != "session-2" {
				t.Fatalf("source metadata = %#v, want session/session-2", results[0].Document.Metadata)
			}
		})
	}
}

func TestStore_AddSession_RequiresSessionID(t *testing.T) {
	t.Parallel()

	if err := NewStore().AddSession(session.Session{Title: "missing id"}); err == nil {
		t.Fatal("AddSession() error = nil, want missing ID error")
	}
}

func findDocument(t *testing.T, store *Store, id string) Document {
	t.Helper()

	for _, doc := range store.Documents {
		if doc.ID == id {
			return doc
		}
	}

	t.Fatalf("document %q not found in %#v", id, store.Documents)

	return Document{}
}
