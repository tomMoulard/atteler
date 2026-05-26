//nolint:wsl_v5 // Tests keep setup, action, and assertions close together.
package memory

import (
	"reflect"
	"strings"
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

func TestFromSessions_AggregatesCorpusTagsAndSessions(t *testing.T) {
	t.Parallel()

	store, err := FromSessions(
		session.Session{
			ID:       "one",
			Title:    "Auth work",
			Tags:     []string{"auth"},
			Messages: []llm.Message{{Role: llm.RoleUser, Content: "OAuth auth note"}},
		},
		session.Session{
			ID:       "two",
			Title:    "Docs work",
			Tags:     []string{"docs"},
			Messages: []llm.Message{{Role: llm.RoleUser, Content: "Docs note"}},
		},
	)
	if err != nil {
		t.Fatalf("FromSessions() error = %v", err)
	}

	if !reflect.DeepEqual(store.Corpus.SessionIDs, []string{"one", "two"}) {
		t.Fatalf("session ids = %#v, want both sessions", store.Corpus.SessionIDs)
	}
	if !reflect.DeepEqual(store.Corpus.Tags, []string{"auth", "docs"}) {
		t.Fatalf("tags = %#v, want aggregate tags", store.Corpus.Tags)
	}
	if store.Corpus.SessionCount != 2 || store.Corpus.DocumentCount != len(store.Documents) {
		t.Fatalf("corpus counts = %#v, documents=%d", store.Corpus, len(store.Documents))
	}
	if store.Corpus.Scope == ScopeSession {
		t.Fatalf("corpus scope = %q, want multi-session corpus not to claim current-session scope", store.Corpus.Scope)
	}
}

func TestFromSessions_ClearsMixedRepoCorpusMetadata(t *testing.T) {
	t.Parallel()

	firstRepo := "/repo/one"
	secondRepo := "/repo/two"
	store, err := FromSessions(
		session.Session{
			ID:           "one",
			WorktreePath: firstRepo,
			Messages:     []llm.Message{{Role: llm.RoleUser, Content: "OAuth one"}},
		},
		session.Session{
			ID:           "two",
			WorktreePath: secondRepo,
			Messages:     []llm.Message{{Role: llm.RoleUser, Content: "OAuth two"}},
		},
	)
	if err != nil {
		t.Fatalf("FromSessions() error = %v", err)
	}

	if store.Corpus.RepoPath != "" {
		t.Fatalf("corpus repo path = %q, want empty for mixed-repo corpus", store.Corpus.RepoPath)
	}
	if strings.Contains(store.Corpus.Description, firstRepo) || strings.Contains(store.Corpus.Description, secondRepo) {
		t.Fatalf("corpus description = %q, want no single repo claim", store.Corpus.Description)
	}
	if store.Corpus.Scope == ScopeSession || store.Corpus.Scope == ScopeRepo {
		t.Fatalf("corpus scope = %q, want mixed corpus not to claim session/repo scope", store.Corpus.Scope)
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

	artifact := findDocument(t, store, "session/session-2/artifact/0")
	if artifact.Path != "docs/oauth.md" || artifact.Provenance == nil || artifact.Provenance.Path != "docs/oauth.md" {
		t.Fatalf("artifact path/provenance = path:%q provenance:%#v, want docs/oauth.md", artifact.Path, artifact.Provenance)
	}
}

func TestStore_AddSession_RequiresSessionID(t *testing.T) {
	t.Parallel()

	if err := NewStore().AddSession(session.Session{Title: "missing id"}); err == nil {
		t.Fatal("AddSession() error = nil, want missing ID error")
	}
}

func TestStore_AddSessionWithOptionsStoresProvenancePolicyAndRedacts(t *testing.T) {
	t.Parallel()

	const secret = "ghp_1234567890abcdefSECRET"
	saved := session.Session{
		ID:           "session-privacy",
		Title:        "Privacy review",
		DefaultAgent: "reviewer",
		WorktreePath: "/repo/privacy",
		Tags:         []string{"security"},
		Messages: []llm.Message{
			{Role: llm.RoleUser, Content: "Token leak " + secret},
		},
	}

	store := NewStore()
	err := store.AddSessionWithOptions(saved, SessionIndexOptions{
		Scope:     ScopeRepo,
		Retention: testRetentionThirtyDays,
	})
	if err != nil {
		t.Fatalf("AddSessionWithOptions() error = %v", err)
	}

	message := findDocument(t, store, "session/session-privacy/message/0")
	if store.Corpus.Scope != ScopeRepo || store.Corpus.SessionCount != 1 || store.Corpus.SessionIDs[0] != "session-privacy" {
		t.Fatalf("corpus = %#v, want repo-scoped session corpus", store.Corpus)
	}
	if message.Provenance == nil {
		t.Fatal("message provenance is nil, want source provenance")
	}
	if message.Provenance.SessionID != "session-privacy" || message.Provenance.RepoPath != "/repo/privacy" {
		t.Fatalf("provenance = %#v, want session/repo source", message.Provenance)
	}
	if !reflect.DeepEqual(message.Provenance.Tags, []string{"security"}) {
		t.Fatalf("tags = %#v, want security", message.Provenance.Tags)
	}
	if message.Policy == nil || message.Policy.Scope != ScopeRepo || message.Policy.Retention != testRetentionThirtyDays || !message.Policy.Redacted {
		t.Fatalf("policy = %#v, want scoped redaction/retention decision", message.Policy)
	}
	if strings.Contains(message.Text, secret) {
		t.Fatalf("message text contains raw secret: %q", message.Text)
	}
}

func TestStore_AddSessionWithOptionsAgentScopeExcludesOtherAgentArtifacts(t *testing.T) {
	t.Parallel()

	saved := session.Session{
		ID:           "agent-scope",
		DefaultAgent: "reviewer",
		Messages: []llm.Message{
			{Role: llm.RoleUser, Content: "Reviewer transcript note"},
		},
		NegativeKnowledge: []session.NegativeKnowledge{{
			Agent:    "writer",
			Approach: "Writer-only approach",
			Reason:   "Should not leak into reviewer memory",
		}},
		Artifacts: []session.Artifact{{
			SourceAgent: "writer",
			Path:        "writer.md",
			Summary:     "Writer-only artifact",
		}},
	}

	store := NewStore()
	err := store.AddSessionWithOptions(saved, SessionIndexOptions{
		Scope: ScopeAgent,
		Agent: "reviewer",
	})
	if err != nil {
		t.Fatalf("AddSessionWithOptions() error = %v", err)
	}

	if got := findDocument(t, store, "session/agent-scope/message/0"); got.Provenance.Agent != "reviewer" {
		t.Fatalf("message agent = %q, want reviewer", got.Provenance.Agent)
	}
	if _, ok := maybeFindDocument(store, "session/agent-scope/negative_knowledge/0"); ok {
		t.Fatal("writer negative knowledge was indexed into reviewer-scoped memory")
	}
	if _, ok := maybeFindDocument(store, "session/agent-scope/artifact/0"); ok {
		t.Fatal("writer artifact was indexed into reviewer-scoped memory")
	}
}

func TestStore_AddSessionWithOptionsGlobalScopeRecordsOptIn(t *testing.T) {
	t.Parallel()

	saved := session.Session{
		ID: "global-scope",
		Messages: []llm.Message{
			{Role: llm.RoleUser, Content: "Global opt-in memory note"},
		},
	}

	store := NewStore()
	if err := store.AddSessionWithOptions(saved, SessionIndexOptions{Scope: ScopeGlobal}); err != nil {
		t.Fatalf("AddSessionWithOptions() error = %v", err)
	}

	if store.Corpus.Scope != ScopeGlobal || !store.Corpus.Global {
		t.Fatalf("corpus = %#v, want opt-in global corpus", store.Corpus)
	}
	if !strings.Contains(store.Corpus.Description, "global=opt-in") {
		t.Fatalf("corpus description = %q, want opt-in global marker", store.Corpus.Description)
	}
}

func TestStore_AddSessionWithOptionsDowngradesUnsupportedScope(t *testing.T) {
	t.Parallel()

	saved := session.Session{
		ID: "unsupported-scope",
		Messages: []llm.Message{
			{Role: llm.RoleUser, Content: "OAuth unsupported scope note"},
		},
	}

	store := NewStore()
	if err := store.AddSessionWithOptions(saved, SessionIndexOptions{Scope: "everything"}); err != nil {
		t.Fatalf("AddSessionWithOptions() error = %v", err)
	}

	doc := findDocument(t, store, "session/unsupported-scope/message/0")
	if doc.Policy == nil || doc.Policy.Scope != ScopeSession {
		t.Fatalf("policy = %#v, want session scope for unsupported option", doc.Policy)
	}
	if store.Corpus.Scope != ScopeSession {
		t.Fatalf("corpus scope = %q, want session", store.Corpus.Scope)
	}
}

func findDocument(t *testing.T, store *Store, id string) Document {
	t.Helper()

	if doc, ok := maybeFindDocument(store, id); ok {
		return doc
	}

	t.Fatalf("document %q not found in %#v", id, store.Documents)

	return Document{}
}

func maybeFindDocument(store *Store, id string) (Document, bool) {
	for i := range store.Documents {
		if store.Documents[i].ID == id {
			return store.Documents[i], true
		}
	}

	return Document{}, false
}
