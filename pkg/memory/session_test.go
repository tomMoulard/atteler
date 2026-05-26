package memory

import (
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/tommoulard/atteler/pkg/llm"
	"github.com/tommoulard/atteler/pkg/retrieval"
	"github.com/tommoulard/atteler/pkg/session"
)

const (
	sessionFixtureID = "session-1"
	metadataKind     = "metadata"
	boolStringTrue   = "true"
)

func TestFromSessions_DefaultPolicyExcludesRawMessages(t *testing.T) {
	t.Parallel()

	created := time.Date(2026, 4, 30, 12, 0, 0, 0, time.UTC)
	saved := session.Session{
		ID:           sessionFixtureID,
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

	if len(store.Documents) != 1 {
		t.Fatalf("documents len = %d, want metadata only", len(store.Documents))
	}

	metadata := findDocument(t, store, "session/session-1/metadata")
	if metadata.Metadata["source_type"] != sessionSourceType || metadata.Metadata["kind"] != metadataKind {
		t.Fatalf("metadata source = %#v, want session metadata", metadata.Metadata)
	}

	if metadata.Provenance["source_type"] != sessionSourceType ||
		metadata.Provenance["session_id"] != sessionFixtureID ||
		metadata.Provenance["kind"] != metadataKind {
		t.Fatalf("metadata provenance = %#v, want session metadata provenance", metadata.Provenance)
	}

	if metadata.Metadata["session_title"] != "OAuth rollout" {
		t.Fatalf("metadata title = %q, want OAuth rollout", metadata.Metadata["session_title"])
	}

	if metadata.Metadata["created_at"] != "2026-04-30T12:00:00Z" {
		t.Fatalf("created_at = %q, want RFC3339 UTC", metadata.Metadata["created_at"])
	}

	results, err := store.Search("callback handling", 2)
	if err != nil {
		t.Fatalf("Search() error = %v", err)
	}

	if len(results) != 0 {
		t.Fatalf("default policy indexed raw message results = %#v, want none", results)
	}
}

func TestFromSessions_OptInPolicyIndexesMessagesWithStableSource(t *testing.T) {
	t.Parallel()

	created := time.Date(2026, 4, 30, 12, 0, 0, 0, time.UTC)
	saved := session.Session{
		ID:           sessionFixtureID,
		Title:        "OAuth rollout",
		DefaultAgent: "reviewer",
		DefaultModel: "gpt-test",
		CreatedAt:    created,
		UpdatedAt:    created.Add(time.Hour),
		Tags:         []string{"auth", "release"},
		Messages: []llm.Message{
			{Role: llm.RoleUser, Content: "Please review OAuth tokens and OAuth callback handling. api_key=abc123"},
			{Role: llm.RoleAssistant, Content: "Release notes mention docs only."},
		},
	}

	policy := DefaultSessionIndexPolicy()
	policy.IncludeMessages = true

	store, err := FromSessionsWithPolicy(policy, saved)
	if err != nil {
		t.Fatalf("FromSessionsWithPolicy() error = %v", err)
	}

	if len(store.Documents) != 3 {
		t.Fatalf("documents len = %d, want metadata plus two messages", len(store.Documents))
	}

	message := findDocument(t, store, "session/session-1/message/0")
	if message.Path != sessionFixtureID {
		t.Fatalf("message path = %q, want session-1", message.Path)
	}

	if message.Metadata["role"] != "user" || message.Metadata["index"] != "0" {
		t.Fatalf("message metadata = %#v, want role user index 0", message.Metadata)
	}

	if strings.Contains(message.Text, "abc123") {
		t.Fatalf("message text was not redacted: %q", message.Text)
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

func TestFromSessions_WorktreeMetadataRequiresOptIn(t *testing.T) {
	t.Parallel()

	saved := session.Session{
		ID:             "session-meta",
		Title:          "Privacy check",
		WorktreePath:   "/tmp/customer-alpha-worktree",
		WorktreeBranch: "feature/private-worktree-memory",
		WorktreeBase:   "main",
	}

	defaultStore, err := FromSessions(saved)
	if err != nil {
		t.Fatalf("FromSessions() error = %v", err)
	}

	defaultResults, err := defaultStore.Search("customer alpha worktree", 1)
	if err != nil {
		t.Fatalf("Search(default worktree) error = %v", err)
	}

	if len(defaultResults) != 0 {
		t.Fatalf("default policy indexed worktree metadata = %#v, want none", defaultResults)
	}

	policy := DefaultSessionIndexPolicy()
	policy.IncludeWorktreeMetadata = true

	optInStore, err := FromSessionsWithPolicy(policy, saved)
	if err != nil {
		t.Fatalf("FromSessionsWithPolicy() error = %v", err)
	}

	optInResults, err := optInStore.Search("customer alpha worktree", 1)
	if err != nil {
		t.Fatalf("Search(opt-in worktree) error = %v", err)
	}

	if len(optInResults) != 1 || optInResults[0].Document.ID != "session/session-meta/metadata" {
		t.Fatalf("opt-in worktree results = %#v, want metadata match", optInResults)
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
			TaskType: "migration",
			Severity: "critical",
		}},
		Evaluations: []session.AgentEvaluation{{
			Agent:         "verifier",
			Outcome:       "pass",
			Notes:         "Caught OAuth regression",
			Reference:     "eval.md",
			RubricVersion: "review rubric v2",
			TaskType:      "auth review",
			Score:         90,
		}},
		Artifacts: []session.Artifact{{
			Path:        "docs/oauth.md",
			Kind:        "research",
			Summary:     "OAuth findings and redirect risks",
			SourceAgent: "researcher",
		}},
	}

	store := NewStore()
	policy := DefaultSessionIndexPolicy()

	policy.IncludeMessages = true
	if err := store.AddSessionWithPolicy(saved, policy); err != nil {
		t.Fatalf("AddSessionWithPolicy() error = %v", err)
	}

	tests := []struct {
		name     string
		query    string
		wantID   string
		wantKind string
	}{
		{name: "negative knowledge", query: "retry storms", wantID: "session/session-2/negative_knowledge/0", wantKind: "negative_knowledge"},
		{name: "negative knowledge metadata", query: "critical migration", wantID: "session/session-2/negative_knowledge/0", wantKind: "negative_knowledge"},
		{name: "evaluation", query: "caught regression", wantID: "session/session-2/evaluation/0", wantKind: "evaluation"},
		{name: "evaluation metadata", query: "rubric auth", wantID: "session/session-2/evaluation/0", wantKind: "evaluation"},
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

func TestFromSessions_PolicyExcludesKnowledgeEvaluationsAndArtifacts(t *testing.T) {
	t.Parallel()

	saved := session.Session{
		ID:    "session-exclusions",
		Title: "Metadata stays searchable",
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

	policy := DefaultSessionIndexPolicy()
	policy.IncludeNegativeKnowledge = false
	policy.IncludeEvaluations = false
	policy.IncludeArtifacts = false

	store, err := FromSessionsWithPolicy(policy, saved)
	if err != nil {
		t.Fatalf("FromSessionsWithPolicy() error = %v", err)
	}

	if len(store.Documents) != 1 {
		t.Fatalf("documents len = %d, want metadata only: %#v", len(store.Documents), store.Documents)
	}

	results, err := store.Search("metadata stays searchable", 1)
	if err != nil {
		t.Fatalf("Search(metadata) error = %v", err)
	}

	if len(results) != 1 || results[0].Document.ID != "session/session-exclusions/metadata" {
		t.Fatalf("metadata results = %#v, want metadata document", results)
	}

	for _, query := range []string{"retry storms", "caught regression", "redirect risks"} {
		results, err := store.Search(query, 1)
		if err != nil {
			t.Fatalf("Search(%q) error = %v", query, err)
		}

		if len(results) != 0 {
			t.Fatalf("Search(%q) results = %#v, want none", query, results)
		}
	}
}

func TestStore_AddSession_RedactsSensitiveDerivedFields(t *testing.T) {
	t.Parallel()

	saved := session.Session{
		ID:    "session-secrets",
		Title: "Secret audit",
		NegativeKnowledge: []session.NegativeKnowledge{{
			Approach: "Tried deploy password=hunter2",
			Reason:   "Leaked api_key=abc123 in logs",
			Commit:   "auth_token=tok123",
			Agent:    "reviewer",
		}},
		Evaluations: []session.AgentEvaluation{{
			Agent:     "verifier",
			Outcome:   "pass",
			Notes:     "Authorization: Bearer openai-secret-value",
			Reference: "eval.md?refresh_token=refresh123",
			Score:     90,
		}},
		Artifacts: []session.Artifact{{
			Path:        "docs/secret.md?access_token=artifact123",
			Kind:        "research",
			Summary:     "Captured OPENAI_API_KEY=openai-project-secret",
			SourceAgent: "researcher",
		}},
	}

	store := NewStore()
	if err := store.AddSession(saved); err != nil {
		t.Fatalf("AddSession() error = %v", err)
	}

	const redactedMarker = "[REDACTED]"

	rawSecrets := []string{"hunter2", "abc123", "tok123", "openai-secret-value", "refresh123", "artifact123", "openai-project-secret"}
	sawRedaction := false

	for _, doc := range store.Documents {
		if strings.Contains(doc.Text, redactedMarker) {
			sawRedaction = true
		}

		for _, raw := range rawSecrets {
			if strings.Contains(doc.Text, raw) {
				t.Fatalf("document %q text retained secret %q: %q", doc.ID, raw, doc.Text)
			}

			for key, value := range doc.Metadata {
				if strings.Contains(value, redactedMarker) {
					sawRedaction = true
				}

				if strings.Contains(value, raw) {
					t.Fatalf("document %q metadata %q retained secret %q: %q", doc.ID, key, raw, value)
				}
			}
		}
	}

	if !sawRedaction {
		t.Fatal("AddSession() retained no redaction marker, want sensitive derived fields redacted")
	}
}

func TestStore_AddSession_RedactsStructuredAgentAndModelMetadata(t *testing.T) {
	t.Parallel()

	saved := session.Session{
		ID:           "structured-session",
		Title:        "Structured metadata redaction",
		DefaultAgent: "reviewer/access_token=agent123/team",
		DefaultModel: "tenant/embed?api_key=model123/v1",
		Artifacts: []session.Artifact{{
			Path:        "docs/metadata.md",
			Kind:        "note",
			Summary:     "Structured metadata note",
			SourceAgent: "researcher/access_token=source123/team",
		}},
	}

	store := NewStore()
	if err := store.AddSession(saved); err != nil {
		t.Fatalf("AddSession() error = %v", err)
	}

	metadata := findDocument(t, store, "session/structured-session/metadata")
	if got := metadata.Metadata["default_agent"]; got != "reviewer/access_token=[REDACTED]/team" {
		t.Fatalf("default_agent metadata = %q, want structured redaction", got)
	}

	if got := metadata.Metadata["default_model"]; got != "tenant/embed?api_key=[REDACTED]/v1" {
		t.Fatalf("default_model metadata = %q, want structured redaction", got)
	}

	artifact := findDocument(t, store, "session/structured-session/artifact/0")
	if got := artifact.Metadata["source_agent"]; got != "researcher/access_token=[REDACTED]/team" {
		t.Fatalf("source_agent metadata = %q, want structured redaction", got)
	}

	for _, raw := range []string{"agent123", "model123", "source123"} {
		for _, doc := range store.Documents {
			if strings.Contains(doc.Text, raw) {
				t.Fatalf("document %q text retained raw structured secret %q: %q", doc.ID, raw, doc.Text)
			}

			for key, value := range doc.Metadata {
				if strings.Contains(value, raw) {
					t.Fatalf("document %q metadata %q retained raw structured secret %q: %q", doc.ID, key, raw, value)
				}
			}
		}
	}
}

func TestStore_AddSession_RedactsSensitiveSessionIDWithoutCollapsingDocumentKinds(t *testing.T) {
	t.Parallel()

	saved := session.Session{
		ID:    "tenant/access_token=artifact123/session-1",
		Title: "Secret session ID",
		Messages: []llm.Message{{
			Role:    llm.RoleUser,
			Content: "Callback retry transcript detail.",
		}},
	}

	policy := DefaultSessionIndexPolicy()
	policy.IncludeMessages = true

	store, err := FromSessionsWithPolicy(policy, saved)
	if err != nil {
		t.Fatalf("FromSessionsWithPolicy() error = %v", err)
	}

	if len(store.Documents) != 2 {
		t.Fatalf("documents len = %d, want metadata and message without redacted ID collision: %#v", len(store.Documents), store.Documents)
	}

	var sawMetadata, sawMessage bool

	for _, doc := range store.Documents {
		if strings.Contains(doc.ID, "artifact123") ||
			strings.Contains(doc.Path, "artifact123") ||
			strings.Contains(doc.Text, "artifact123") {
			t.Fatalf("document retained raw session secret: %#v", doc)
		}

		for key, value := range doc.Metadata {
			if strings.Contains(value, "artifact123") {
				t.Fatalf("metadata %q retained raw session secret: %#v", key, doc.Metadata)
			}
		}

		for key, value := range doc.Provenance {
			if strings.Contains(value, "artifact123") {
				t.Fatalf("provenance %q retained raw session secret: %#v", key, doc.Provenance)
			}
		}

		if got := doc.Metadata["session_id"]; !strings.HasSuffix(got, "/session-1") {
			t.Fatalf("metadata session_id = %q, want redacted value preserving suffix", got)
		}

		if got := doc.Provenance["session_id"]; !strings.HasSuffix(got, "/session-1") {
			t.Fatalf("provenance session_id = %q, want redacted value preserving suffix", got)
		}

		if strings.HasSuffix(doc.ID, "/metadata") {
			sawMetadata = true
		}

		if strings.HasSuffix(doc.ID, "/message/0") {
			sawMessage = true
		}
	}

	if !sawMetadata || !sawMessage {
		t.Fatalf("document IDs = %#v, want distinct metadata and message IDs", store.Documents)
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

	for i := range store.Documents {
		if store.Documents[i].ID == id {
			return store.Documents[i]
		}
	}

	t.Fatalf("document %q not found in %#v", id, store.Documents)

	return Document{}
}

func TestFromSessions_IndexesMessagesAndMetadataWithStableSource(t *testing.T) {
	t.Parallel()

	created := time.Date(2026, 4, 30, 12, 0, 0, 0, time.UTC)
	saved := session.Session{
		ID:           sessionFixtureID,
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

	policy := DefaultSessionIndexPolicy()
	policy.IncludeMessages = true

	store, err := FromSessionsWithPolicy(policy, saved)
	if err != nil {
		t.Fatalf("FromSessionsWithPolicy() error = %v", err)
	}

	if len(store.Documents) != 3 {
		t.Fatalf("documents len = %d, want metadata plus two messages", len(store.Documents))
	}

	metadata := findDocument(t, store, "session/session-1/metadata")
	if metadata.Metadata["source_type"] != "session" || metadata.Metadata["kind"] != metadataKind {
		t.Fatalf("metadata source = %#v, want session metadata", metadata.Metadata)
	}

	if metadata.Metadata["session_title"] != "OAuth rollout" {
		t.Fatalf("metadata title = %q, want OAuth rollout", metadata.Metadata["session_title"])
	}

	if metadata.Metadata["created_at"] != "2026-04-30T12:00:00Z" {
		t.Fatalf("created_at = %q, want RFC3339 UTC", metadata.Metadata["created_at"])
	}

	message := findDocument(t, store, "session/session-1/message/0")
	if message.Path != sessionFixtureID {
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

func TestStore_AddSession_RedactsPrivateTranscriptBeforeStorage(t *testing.T) {
	t.Parallel()

	saved := session.Session{
		ID: "session-secret",
		Messages: []llm.Message{{
			Role:    llm.RoleUser,
			Content: "OAuth callback api_key=super-secret-token",
		}},
	}

	store := NewStore()
	policy := DefaultSessionIndexPolicy()

	policy.IncludeMessages = true
	if err := store.AddSessionWithPolicy(saved, policy); err != nil {
		t.Fatalf("AddSessionWithPolicy() error = %v", err)
	}

	message := findDocument(t, store, "session/session-secret/message/0")
	if strings.Contains(message.Text, "super-secret-token") {
		t.Fatalf("stored session text leaked secret: %q", message.Text)
	}

	if message.Metadata[retrieval.MetadataSafetyInjectAllowed] != "false" {
		t.Fatalf("inject_allowed = %q, want false for private transcript", message.Metadata[retrieval.MetadataSafetyInjectAllowed])
	}

	if message.Metadata[retrieval.MetadataSafetyPrivate] != boolStringTrue || message.Metadata[retrieval.MetadataSafetyRedacted] != boolStringTrue {
		t.Fatalf("safety metadata = %#v, want private redacted transcript", message.Metadata)
	}
}
