//nolint:wsl_v5 // Tests keep setup, action, and assertions close together.
package memory

import (
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"slices"
	"strings"
	"testing"
	"time"
)

const testRetentionThirtyDays = "30 days"

func TestTokenize_NormalizesUnicodeWordsAndDigits(t *testing.T) {
	t.Parallel()

	got := Tokenize("Hello, RAG-2 café AUTH auth!")

	want := []string{"hello", "rag", "2", "café", "auth", "auth"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("Tokenize() = %#v, want %#v", got, want)
	}
}

func TestStore_SearchRanksLexicalResultsWithSnippets(t *testing.T) {
	t.Parallel()

	store := NewStore()
	if err := store.Add(Document{ID: "auth", Text: "Auth auth tokens rotate safely. Login uses tokens."}); err != nil {
		t.Fatalf("Add(auth) error = %v", err)
	}

	if err := store.Add(Document{ID: "docs", Text: "Release notes explain docs updates."}); err != nil {
		t.Fatalf("Add(docs) error = %v", err)
	}

	if err := store.Add(Document{ID: "login", Text: "Login creates session tokens."}); err != nil {
		t.Fatalf("Add(login) error = %v", err)
	}

	results, err := store.Search("auth tokens", 2)
	if err != nil {
		t.Fatalf("Search() error = %v", err)
	}

	if len(results) != 2 {
		t.Fatalf("Search() len = %d, want 2: %#v", len(results), results)
	}

	if results[0].Document.ID != "auth" {
		t.Fatalf("top result = %q, want auth", results[0].Document.ID)
	}

	if !reflect.DeepEqual(results[0].Matches, []string{"auth", "tokens"}) {
		t.Fatalf("matches = %#v, want auth/token", results[0].Matches)
	}

	if !strings.Contains(strings.ToLower(results[0].Snippet), "auth") {
		t.Fatalf("snippet = %q, want auth excerpt", results[0].Snippet)
	}

	if results[0].Score <= results[1].Score {
		t.Fatalf("scores not ranked: first=%v second=%v", results[0].Score, results[1].Score)
	}
}

func TestStore_SearchRejectsEmptyQuery(t *testing.T) {
	t.Parallel()

	_, err := NewStore().Search(" !!! ", 10)
	if !errors.Is(err, ErrEmptyQuery) {
		t.Fatalf("Search(empty) error = %v, want ErrEmptyQuery", err)
	}
}

func TestStore_AddReplacesExistingDocument(t *testing.T) {
	t.Parallel()

	store := NewStore()
	if err := store.AddText("same", "old auth text"); err != nil {
		t.Fatalf("AddText(old) error = %v", err)
	}

	if err := store.AddText("same", "new release text"); err != nil {
		t.Fatalf("AddText(new) error = %v", err)
	}

	if len(store.Documents) != 1 {
		t.Fatalf("documents len = %d, want 1", len(store.Documents))
	}

	if store.Documents[0].Text != "new release text" {
		t.Fatalf("document text = %q, want replacement", store.Documents[0].Text)
	}
}

func TestStore_AddTextStoresDefaultProvenanceAndPolicy(t *testing.T) {
	t.Parallel()

	store := NewStore()
	if err := store.AddText("note", "manual memory note"); err != nil {
		t.Fatalf("AddText() error = %v", err)
	}

	doc := store.Documents[0]
	if doc.Provenance == nil {
		t.Fatalf("provenance is nil, want default provenance")
	}
	if doc.Provenance.SourceType != ScopeManual || doc.Provenance.SourceID != "note" {
		t.Fatalf("provenance = %#v, want manual source for note", doc.Provenance)
	}
	if doc.Policy == nil || doc.Policy.Scope != ScopeManual {
		t.Fatalf("policy = %#v, want manual scope", doc.Policy)
	}
}

func TestStore_AddEnrichesPartialSessionProvenanceAndPolicy(t *testing.T) {
	t.Parallel()

	store := NewStore()
	if err := store.Add(Document{
		ID:         "session/partial/message/0",
		Text:       "partial session provenance",
		Provenance: &Provenance{SessionID: "partial"},
	}); err != nil {
		t.Fatalf("Add() error = %v", err)
	}

	doc := store.Documents[0]
	if doc.Provenance == nil ||
		doc.Provenance.SourceType != sessionSourceType ||
		doc.Provenance.SourceID != "partial" ||
		doc.Provenance.Kind != "message" {
		t.Fatalf("provenance = %#v, want enriched session provenance", doc.Provenance)
	}
	if doc.Policy == nil || doc.Policy.Scope != ScopeSession {
		t.Fatalf("policy = %#v, want inferred session scope", doc.Policy)
	}
}

func TestStore_AddFileIndexesUTF8Text(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()

	path := filepath.Join(dir, "note.txt")
	if err := os.WriteFile(path, []byte("Local memory keeps useful context."), 0o600); err != nil {
		t.Fatalf("WriteFile() error = %v", err)
	}

	store := NewStore()
	if err := store.AddFile(path); err != nil {
		t.Fatalf("AddFile() error = %v", err)
	}

	results, err := store.Search("memory context", 0)
	if err != nil {
		t.Fatalf("Search() error = %v", err)
	}

	if len(results) != 1 {
		t.Fatalf("results len = %d, want 1", len(results))
	}

	if results[0].Document.Path != filepath.Clean(path) {
		t.Fatalf("Path = %q, want %q", results[0].Document.Path, filepath.Clean(path))
	}

	if store.Corpus.Scope != ScopeFile || store.Corpus.FileCount != 1 || store.Corpus.DocumentCount != 1 {
		t.Fatalf("corpus = %#v, want file-scoped one-document corpus", store.Corpus)
	}
}

func TestStore_AddManualDocumentToFileCorpusResetsManualScope(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	path := filepath.Join(dir, "note.txt")
	if err := os.WriteFile(path, []byte("Local memory keeps useful context."), 0o600); err != nil {
		t.Fatalf("WriteFile() error = %v", err)
	}

	store := NewStore()
	if err := store.AddFile(path); err != nil {
		t.Fatalf("AddFile() error = %v", err)
	}
	if err := store.AddText("manual", "manual memory note"); err != nil {
		t.Fatalf("AddText() error = %v", err)
	}

	if store.Corpus.Scope != ScopeManual || store.Corpus.FileCount != 1 || store.Corpus.DocumentCount != 2 {
		t.Fatalf("corpus = %#v, want mixed manual/file corpus to use manual scope", store.Corpus)
	}

	manual := findDocumentByID(t, store, "manual")
	if manual.Policy == nil || manual.Policy.Scope != ScopeManual {
		t.Fatalf("manual policy = %#v, want manual scope", manual.Policy)
	}
}

func TestStore_AddManualDocumentToSessionCorpusResetsManualScope(t *testing.T) {
	t.Parallel()

	store := NewStore()
	if err := store.Add(Document{
		ID:         "session/current/message/0",
		Text:       "session memory note",
		Metadata:   map[string]string{"source_type": sessionSourceType, "session_id": "current"},
		Provenance: &Provenance{SourceType: sessionSourceType, SessionID: "current"},
	}); err != nil {
		t.Fatalf("Add(session) error = %v", err)
	}
	store.Corpus = CorpusMetadata{Scope: ScopeSession, SessionIDs: []string{"current"}, SessionCount: 1, DocumentCount: 1}

	if err := store.AddText("manual", "manual memory note"); err != nil {
		t.Fatalf("AddText(manual) error = %v", err)
	}

	if store.Corpus.Scope != ScopeManual || store.Corpus.SessionCount != 1 || store.Corpus.DocumentCount != 2 {
		t.Fatalf("corpus = %#v, want mixed manual/session corpus to use manual scope", store.Corpus)
	}
	if strings.Contains(store.Corpus.Description, "scope=session") {
		t.Fatalf("corpus description = %q, want no session-scope claim", store.Corpus.Description)
	}
}

func TestStore_AddManualDocumentToTagsCorpusResetsManualScope(t *testing.T) {
	t.Parallel()

	store := NewStore()
	if err := store.Add(Document{
		ID:         "session/auth/message/0",
		Text:       "tagged session memory note",
		Metadata:   map[string]string{"session_id": "auth", "tags": "auth"},
		Provenance: &Provenance{SourceType: sessionSourceType, SessionID: "auth", Tags: []string{"auth"}},
	}); err != nil {
		t.Fatalf("Add(tagged) error = %v", err)
	}
	store.Corpus = CorpusMetadata{Scope: ScopeTags, Tags: []string{"auth"}, SessionIDs: []string{"auth"}, SessionCount: 1, DocumentCount: 1}

	if err := store.AddText("manual", "manual memory note"); err != nil {
		t.Fatalf("AddText(manual) error = %v", err)
	}

	if store.Corpus.Scope != ScopeManual || store.Corpus.SessionCount != 1 || store.Corpus.DocumentCount != 2 {
		t.Fatalf("corpus = %#v, want mixed manual/tagged corpus to use manual scope", store.Corpus)
	}
	if strings.Contains(store.Corpus.Description, "scope=tags") {
		t.Fatalf("corpus description = %q, want no tags-scope claim", store.Corpus.Description)
	}
}

func TestStore_AddUndatedDocumentToDateRangeCorpusResetsManualScope(t *testing.T) {
	t.Parallel()

	activity := time.Date(2026, 5, 1, 12, 0, 0, 0, time.UTC)
	store := NewStore()
	if err := store.Add(Document{
		ID:         "session/dated/message/0",
		Text:       "dated session memory note",
		Metadata:   map[string]string{"session_id": "dated", "updated_at": activity.Format(time.RFC3339)},
		Provenance: &Provenance{SourceType: sessionSourceType, SessionID: "dated", UpdatedAt: activity.Format(time.RFC3339)},
	}); err != nil {
		t.Fatalf("Add(dated) error = %v", err)
	}
	store.Corpus = CorpusMetadata{
		Scope:         ScopeDateRange,
		DateStart:     activity.Add(-time.Hour).Format(time.RFC3339),
		DateEnd:       activity.Add(time.Hour).Format(time.RFC3339),
		SessionIDs:    []string{"dated"},
		SessionCount:  1,
		DocumentCount: 1,
	}

	if err := store.AddText("manual", "manual memory note"); err != nil {
		t.Fatalf("AddText(manual) error = %v", err)
	}

	if store.Corpus.Scope != ScopeManual || store.Corpus.DateStart != "" || store.Corpus.DateEnd != "" {
		t.Fatalf("corpus = %#v, want mixed undated/date-range corpus to use manual scope without stale dates", store.Corpus)
	}
	if strings.Contains(store.Corpus.Description, "date_range=") {
		t.Fatalf("corpus description = %q, want no stale date range", store.Corpus.Description)
	}
}

func TestStore_SaveLoadJSONRoundTrip(t *testing.T) {
	t.Parallel()

	store := NewStore()
	if err := store.Add(Document{
		ID:       "design",
		Path:     "docs/design.txt",
		Text:     "RAG memory design",
		Metadata: map[string]string{"kind": "note"},
	}); err != nil {
		t.Fatalf("Add() error = %v", err)
	}

	path := filepath.Join(t.TempDir(), "nested", "memory.json")
	if saveErr := store.Save(path); saveErr != nil {
		t.Fatalf("Save() error = %v", saveErr)
	}

	loaded, err := Load(path)
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}

	if !reflect.DeepEqual(loaded.Documents, store.Documents) {
		t.Fatalf("loaded documents = %#v, want %#v", loaded.Documents, store.Documents)
	}

	results, err := loaded.Search("rag", 1)
	if err != nil {
		t.Fatalf("Search() error = %v", err)
	}

	if len(results) != 1 || results[0].Document.ID != "design" {
		t.Fatalf("loaded search results = %#v, want design", results)
	}
}

func TestLoadEmptyStoreReturnsNewStore(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), "memory.json")
	if err := os.WriteFile(path, []byte(" \n\t"), 0o600); err != nil {
		t.Fatalf("WriteFile() error = %v", err)
	}

	loaded, err := Load(path)
	if err != nil {
		t.Fatalf("Load(empty) error = %v", err)
	}

	if loaded.SchemaVersion != SchemaVersion {
		t.Fatalf("schema version = %d, want %d", loaded.SchemaVersion, SchemaVersion)
	}
	if loaded.Corpus.Scope != ScopeManual || len(loaded.Documents) != 0 {
		t.Fatalf("empty load = corpus:%#v documents:%#v, want manual empty store", loaded.Corpus, loaded.Documents)
	}
}

func TestStore_SavePreservesEmptyExplicitCorpusMetadata(t *testing.T) {
	t.Parallel()

	store := NewStore()
	store.Corpus = CorpusMetadata{
		Scope:     ScopeRepo,
		RepoPath:  "/repo/empty",
		Tags:      []string{"security", "security"},
		Retention: testRetentionThirtyDays,
		DateStart: "2026-05-01T00:00:00Z",
	}

	path := filepath.Join(t.TempDir(), "memory.json")
	if err := store.Save(path); err != nil {
		t.Fatalf("Save() error = %v", err)
	}

	loaded, err := Load(path)
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}

	if loaded.Corpus.Scope != ScopeRepo || loaded.Corpus.RepoPath != "/repo/empty" {
		t.Fatalf("empty corpus = %#v, want explicit repo scope preserved", loaded.Corpus)
	}
	if loaded.Corpus.DocumentCount != 0 || loaded.Corpus.FileCount != 0 || loaded.Corpus.SessionCount != 0 {
		t.Fatalf("empty corpus counts = %#v, want zero document/file/session counts", loaded.Corpus)
	}
	if !reflect.DeepEqual(loaded.Corpus.Tags, []string{"security"}) {
		t.Fatalf("empty corpus tags = %#v, want de-duplicated selected tag", loaded.Corpus.Tags)
	}
	if !strings.Contains(loaded.Corpus.Description, "scope=repo") ||
		!strings.Contains(loaded.Corpus.Description, "repo=/repo/empty") ||
		!strings.Contains(loaded.Corpus.Description, "retention=30 days") {
		t.Fatalf("empty corpus description = %q, want selected corpus policy", loaded.Corpus.Description)
	}
}

func TestStore_SaveTightensExistingFilePermissions(t *testing.T) {
	t.Parallel()

	if runtime.GOOS == "windows" {
		t.Skip("Unix permission bits are not portable on Windows")
	}

	path := filepath.Join(t.TempDir(), "memory.json")
	if err := os.WriteFile(path, []byte("{}\n"), 0o644); err != nil { //nolint:gosec // Intentional loose mode to verify Save tightens it.
		t.Fatalf("WriteFile() error = %v", err)
	}

	store := NewStore()
	if err := store.Add(Document{ID: "secret-note", Text: "OAuth token notes"}); err != nil {
		t.Fatalf("Add() error = %v", err)
	}

	if err := store.Save(path); err != nil {
		t.Fatalf("Save() error = %v", err)
	}

	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("Stat() error = %v", err)
	}
	if got := info.Mode().Perm(); got != 0o600 {
		t.Fatalf("memory store permissions = %#o, want 0600", got)
	}
}

func TestStore_RedactsSecretsBeforeSaveAndSearch(t *testing.T) {
	t.Parallel()

	const secret = "sk-1234567890abcdefSECRET"

	store := NewStore()
	if err := store.Add(Document{
		ID:       "doc-" + secret,
		Path:     "notes/" + secret + ".txt",
		Text:     "Rotate OAuth token. OPENAI_API_KEY=" + secret,
		Metadata: map[string]string{"token": secret, "metadata-" + secret: "key should redact"},
		Provenance: &Provenance{
			SessionID: secret,
			Tags:      []string{secret},
		},
	}); err != nil {
		t.Fatalf("Add() error = %v", err)
	}

	if strings.Contains(store.Documents[0].ID, secret) {
		t.Fatalf("stored id contains raw secret: %q", store.Documents[0].ID)
	}
	if strings.Contains(store.Documents[0].Path, secret) {
		t.Fatalf("stored path contains raw secret: %q", store.Documents[0].Path)
	}
	if strings.Contains(store.Documents[0].Text, secret) {
		t.Fatalf("stored text contains raw secret: %q", store.Documents[0].Text)
	}
	for key, value := range store.Documents[0].Metadata {
		if strings.Contains(key, secret) || strings.Contains(value, secret) {
			t.Fatalf("stored metadata contains raw secret: %#v", store.Documents[0].Metadata)
		}
	}
	if strings.Contains(store.Documents[0].Provenance.SessionID, secret) || strings.Contains(store.Documents[0].Provenance.Tags[0], secret) {
		t.Fatalf("stored provenance contains raw secret: %#v", store.Documents[0].Provenance)
	}
	if store.Documents[0].Policy == nil || !store.Documents[0].Policy.Redacted {
		t.Fatalf("policy = %#v, want redacted decision", store.Documents[0].Policy)
	}

	path := filepath.Join(t.TempDir(), "memory.json")
	if saveErr := store.Save(path); saveErr != nil {
		t.Fatalf("Save() error = %v", saveErr)
	}

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile() error = %v", err)
	}
	if strings.Contains(string(data), secret) {
		t.Fatalf("saved store contains raw secret:\n%s", data)
	}

	results, err := store.Search("oauth rotate", 1)
	if err != nil {
		t.Fatalf("Search() error = %v", err)
	}
	if len(results) != 1 {
		t.Fatalf("results len = %d, want 1", len(results))
	}
	if strings.Contains(results[0].Snippet, secret) {
		t.Fatalf("snippet contains raw secret: %q", results[0].Snippet)
	}
	if strings.Contains(results[0].Document.ID, secret) || strings.Contains(results[0].Document.Path, secret) {
		t.Fatalf("result document contains raw secret: %#v", results[0].Document)
	}
}

func TestStore_RedactsSecretMetadataValuesByKey(t *testing.T) {
	t.Parallel()

	const secret = "plain-secret-only-the-key-identifies"

	store := NewStore()
	if err := store.Add(Document{
		ID:   "metadata-secret",
		Text: "metadata-only secret should not leak",
		Metadata: map[string]string{
			"api_key":     secret,
			"token_count": "42",
		},
	}); err != nil {
		t.Fatalf("Add() error = %v", err)
	}

	if strings.Contains(store.Documents[0].Metadata["api_key"], secret) {
		t.Fatalf("stored metadata api_key contains raw secret: %#v", store.Documents[0].Metadata)
	}
	if got := store.Documents[0].Metadata["token_count"]; got != "42" {
		t.Fatalf("token_count metadata = %q, want 42", got)
	}
	if store.Documents[0].Policy == nil || !store.Documents[0].Policy.Redacted {
		t.Fatalf("policy = %#v, want redacted decision", store.Documents[0].Policy)
	}

	if !slices.Contains(store.Documents[0].Policy.RedactionRules, "secret_assignment") {
		t.Fatalf("redaction rules = %#v, want secret_assignment", store.Documents[0].Policy.RedactionRules)
	}

	path := filepath.Join(t.TempDir(), "memory.json")
	if err := store.Save(path); err != nil {
		t.Fatalf("Save() error = %v", err)
	}

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile() error = %v", err)
	}
	if strings.Contains(string(data), secret) {
		t.Fatalf("saved store contains raw metadata secret:\n%s", data)
	}
}

func TestStore_RedactsQuotedSecretAssignmentsBeforeSaveAndSearch(t *testing.T) {
	t.Parallel()

	const secret = "correct horse battery"

	store := NewStore()
	if err := store.Add(Document{
		ID:   "quoted-secret",
		Text: "OAuth credential note\n\"password\": \"" + secret + "\"\nrotate soon",
	}); err != nil {
		t.Fatalf("Add() error = %v", err)
	}

	if strings.Contains(store.Documents[0].Text, secret) {
		t.Fatalf("stored text contains quoted secret: %q", store.Documents[0].Text)
	}
	if store.Documents[0].Policy == nil || !store.Documents[0].Policy.Redacted {
		t.Fatalf("policy = %#v, want redacted decision", store.Documents[0].Policy)
	}

	path := filepath.Join(t.TempDir(), "memory.json")
	if err := store.Save(path); err != nil {
		t.Fatalf("Save() error = %v", err)
	}

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile() error = %v", err)
	}
	if strings.Contains(string(data), secret) {
		t.Fatalf("saved store contains quoted secret:\n%s", data)
	}

	results, err := store.Search("oauth credential", 1)
	if err != nil {
		t.Fatalf("Search(oauth) error = %v", err)
	}
	if len(results) != 1 {
		t.Fatalf("Search(oauth) len = %d, want 1", len(results))
	}
	if strings.Contains(results[0].Snippet, secret) || strings.Contains(results[0].Document.Text, secret) {
		t.Fatalf("result leaked quoted secret: %#v", results[0])
	}

	results, err = store.Search("correct horse", 10)
	if err != nil {
		t.Fatalf("Search(secret) error = %v", err)
	}
	if len(results) != 0 {
		t.Fatalf("Search(secret) results = %#v, want no match", results)
	}
}

func TestStore_RedactsPolicyBeforeSaveAndSearch(t *testing.T) {
	t.Parallel()

	const secret = "sk-1234567890abcdefSECRET"

	store := NewStore()
	if err := store.Add(Document{
		ID:   "policy-secret",
		Text: "OAuth policy note",
		Policy: &PolicyDecision{
			Scope:          "scope-" + secret,
			Retention:      "retain " + secret,
			RedactionRules: []string{"rule-" + secret},
		},
	}); err != nil {
		t.Fatalf("Add() error = %v", err)
	}

	policy := store.Documents[0].Policy
	if policy == nil ||
		strings.Contains(policy.Scope, secret) ||
		strings.Contains(policy.Retention, secret) ||
		len(policy.RedactionRules) == 0 ||
		strings.Contains(strings.Join(policy.RedactionRules, ","), secret) {
		t.Fatalf("stored policy leaked raw secret: %#v", policy)
	}

	path := filepath.Join(t.TempDir(), "memory.json")
	if err := store.Save(path); err != nil {
		t.Fatalf("Save() error = %v", err)
	}

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile() error = %v", err)
	}
	if strings.Contains(string(data), secret) {
		t.Fatalf("saved store leaked policy secret:\n%s", data)
	}

	results, err := store.Search("oauth policy", 1)
	if err != nil {
		t.Fatalf("Search() error = %v", err)
	}
	if len(results) != 1 {
		t.Fatalf("results len = %d, want 1", len(results))
	}

	resultPolicy := results[0].Document.Policy
	if resultPolicy == nil ||
		strings.Contains(resultPolicy.Scope, secret) ||
		strings.Contains(resultPolicy.Retention, secret) ||
		strings.Contains(strings.Join(resultPolicy.RedactionRules, ","), secret) {
		t.Fatalf("result policy leaked raw secret: %#v", resultPolicy)
	}
}

func TestStore_RedactedDocumentIDsRemainDistinct(t *testing.T) {
	t.Parallel()

	const firstSecret = "sk-1234567890abcdefSECRET"
	const secondSecret = "sk-abcdef1234567890SECRET"

	store := NewStore()
	if err := store.Add(Document{ID: "doc-" + firstSecret, Text: "first OAuth note"}); err != nil {
		t.Fatalf("Add(first) error = %v", err)
	}
	if err := store.Add(Document{ID: "doc-" + secondSecret, Text: "second OAuth note"}); err != nil {
		t.Fatalf("Add(second) error = %v", err)
	}

	if len(store.Documents) != 2 {
		t.Fatalf("documents len = %d, want 2 distinct redacted IDs", len(store.Documents))
	}
	if store.Documents[0].ID == store.Documents[1].ID {
		t.Fatalf("redacted IDs collided: %#v", store.Documents)
	}
	for _, doc := range store.Documents {
		if strings.Contains(doc.ID, firstSecret) || strings.Contains(doc.ID, secondSecret) {
			t.Fatalf("redacted ID contains raw secret: %q", doc.ID)
		}
		if !strings.Contains(doc.ID, "[REDACTED:openai_api_key]#") {
			t.Fatalf("redacted ID = %q, want redaction marker with fingerprint", doc.ID)
		}
	}
}

func TestStore_RedactedSessionIDsRemainDistinctInCorpus(t *testing.T) {
	t.Parallel()

	const firstSecret = "sk-1234567890abcdefSECRET"
	const secondSecret = "sk-abcdef1234567890SECRET"

	store := NewStore()
	for _, sessionID := range []string{firstSecret, secondSecret} {
		if err := store.Add(Document{
			ID:         "session/" + sessionID + "/message/0",
			Text:       "OAuth session note",
			Metadata:   map[string]string{"session_id": sessionID, "source_id": sessionID},
			Provenance: &Provenance{SourceType: sessionSourceType, SourceID: sessionID, SessionID: sessionID},
		}); err != nil {
			t.Fatalf("Add(%s) error = %v", sessionID, err)
		}
	}

	if store.Corpus.SessionCount != 2 || len(store.Corpus.SessionIDs) != 2 {
		t.Fatalf("corpus = %#v, want two distinct redacted session IDs", store.Corpus)
	}
	if store.Corpus.SessionIDs[0] == store.Corpus.SessionIDs[1] {
		t.Fatalf("redacted session IDs collided: %#v", store.Corpus.SessionIDs)
	}
	for _, sessionID := range store.Corpus.SessionIDs {
		if strings.Contains(sessionID, firstSecret) || strings.Contains(sessionID, secondSecret) {
			t.Fatalf("session id contains raw secret: %q", sessionID)
		}
		if !strings.Contains(sessionID, "[REDACTED:openai_api_key]#") {
			t.Fatalf("session id = %q, want redaction marker with fingerprint", sessionID)
		}
	}
}

func TestStore_RedactedRepoPathsRemainDistinctForPurge(t *testing.T) {
	t.Parallel()

	const firstSecret = "sk-1234567890abcdefSECRET"
	const secondSecret = "sk-abcdef1234567890SECRET"

	firstRepo := filepath.Join(string(os.PathSeparator), "tmp", firstSecret)
	secondRepo := filepath.Join(string(os.PathSeparator), "tmp", secondSecret)

	store := NewStore()
	for _, repoPath := range []string{firstRepo, secondRepo} {
		if err := store.Add(Document{
			ID:       "repo-" + repoPath,
			Text:     "OAuth repo memory",
			Metadata: map[string]string{"repo_path": repoPath},
			Provenance: &Provenance{
				SourceType: ScopeManual,
				SourceID:   repoPath,
				RepoPath:   repoPath,
			},
		}); err != nil {
			t.Fatalf("Add(%s) error = %v", repoPath, err)
		}
	}

	if len(store.Documents) != 2 {
		t.Fatalf("documents len = %d, want 2 distinct redacted repo docs", len(store.Documents))
	}
	if store.Documents[0].Provenance.RepoPath == store.Documents[1].Provenance.RepoPath {
		t.Fatalf("redacted repo paths collided: %#v", store.Documents)
	}
	for _, doc := range store.Documents {
		if strings.Contains(doc.ID, firstSecret) ||
			strings.Contains(doc.ID, secondSecret) ||
			strings.Contains(doc.Metadata["repo_path"], firstSecret) ||
			strings.Contains(doc.Metadata["repo_path"], secondSecret) ||
			strings.Contains(doc.Provenance.RepoPath, firstSecret) ||
			strings.Contains(doc.Provenance.RepoPath, secondSecret) {
			t.Fatalf("document contains raw repo secret: %#v", doc)
		}
		if !strings.Contains(doc.Provenance.RepoPath, "[REDACTED:openai_api_key]#") {
			t.Fatalf("repo path = %q, want redaction marker with fingerprint", doc.Provenance.RepoPath)
		}
	}

	if removed := store.Purge(PurgeFilter{RepoPath: firstRepo}); removed != 1 {
		t.Fatalf("purge removed %d, want 1", removed)
	}
	if len(store.Documents) != 1 {
		t.Fatalf("documents after purge = %#v, want one remaining repo", store.Documents)
	}
	if strings.Contains(store.Documents[0].Provenance.RepoPath, firstSecret) ||
		!strings.Contains(store.Documents[0].Provenance.RepoPath, "[REDACTED:openai_api_key]#") {
		t.Fatalf("remaining repo path = %q, want redacted second repo", store.Documents[0].Provenance.RepoPath)
	}
}

func TestStore_RedactedTagsRemainDistinctForPurge(t *testing.T) {
	t.Parallel()

	const firstTag = "sk-1234567890abcdefSECRET"
	const secondTag = "sk-abcdef1234567890SECRET"

	store := NewStore()
	for _, tag := range []string{firstTag, secondTag} {
		if err := store.Add(Document{
			ID:         "tagged-" + tag,
			Text:       "OAuth tagged memory",
			Metadata:   map[string]string{"tags": tag},
			Provenance: &Provenance{Tags: []string{tag}},
		}); err != nil {
			t.Fatalf("Add(%s) error = %v", tag, err)
		}
	}

	if len(store.Corpus.Tags) != 2 {
		t.Fatalf("corpus tags = %#v, want two distinct redacted tags", store.Corpus.Tags)
	}
	if store.Corpus.Tags[0] == store.Corpus.Tags[1] {
		t.Fatalf("redacted tags collided: %#v", store.Corpus.Tags)
	}
	for _, tag := range store.Corpus.Tags {
		if strings.Contains(tag, firstTag) || strings.Contains(tag, secondTag) {
			t.Fatalf("corpus tag contains raw secret: %q", tag)
		}
		if !strings.Contains(tag, "[REDACTED:openai_api_key]#") {
			t.Fatalf("tag = %q, want redaction marker with fingerprint", tag)
		}
	}

	if removed := store.Purge(PurgeFilter{Tag: firstTag}); removed != 1 {
		t.Fatalf("purge removed %d, want 1", removed)
	}
	if len(store.Documents) != 1 || len(store.Corpus.Tags) != 1 {
		t.Fatalf("store after purge = docs:%#v corpus:%#v, want one remaining tag", store.Documents, store.Corpus)
	}
	if strings.Contains(store.Corpus.Tags[0], firstTag) || !strings.Contains(store.Corpus.Tags[0], "[REDACTED:openai_api_key]#") {
		t.Fatalf("remaining tag = %q, want redacted second tag", store.Corpus.Tags[0])
	}
}

func TestStore_RedactsAndPurgesSessionIDOnlyDocuments(t *testing.T) {
	t.Parallel()

	const (
		firstSecret  = "sk-1234567890abcdefSECRET"
		secondSecret = "sk-abcdef1234567890SECRET"
	)

	store := NewStore()
	for _, sessionID := range []string{firstSecret, secondSecret} {
		if err := store.Add(Document{
			ID:   "session/" + sessionID + "/message/0",
			Text: "OAuth legacy session note",
		}); err != nil {
			t.Fatalf("Add(%s) error = %v", sessionID, err)
		}
	}

	if store.Corpus.SessionCount != 2 || len(store.Corpus.SessionIDs) != 2 {
		t.Fatalf("corpus = %#v, want two distinct ID-derived sessions", store.Corpus)
	}
	if store.Corpus.SessionIDs[0] == store.Corpus.SessionIDs[1] {
		t.Fatalf("redacted ID-derived session IDs collided: %#v", store.Corpus.SessionIDs)
	}
	for _, doc := range store.Documents {
		if strings.Contains(doc.ID, firstSecret) || strings.Contains(doc.ID, secondSecret) {
			t.Fatalf("document ID contains raw session secret: %q", doc.ID)
		}
	}

	if removed := store.Purge(PurgeFilter{SessionID: firstSecret}); removed != 1 {
		t.Fatalf("purge removed %d, want 1", removed)
	}
	if len(store.Documents) != 1 || store.Corpus.SessionCount != 1 || len(store.Corpus.SessionIDs) != 1 {
		t.Fatalf("store after purge = docs:%#v corpus:%#v, want one remaining distinct session", store.Documents, store.Corpus)
	}
	if strings.Contains(store.Documents[0].ID, firstSecret) {
		t.Fatalf("remaining document contains purged raw session secret: %q", store.Documents[0].ID)
	}
}

func TestStore_PurgeMatchesRedactedSessionSelector(t *testing.T) {
	t.Parallel()

	const secretSessionID = "sk-1234567890abcdefSECRET"
	store := NewStore()
	if err := store.Add(Document{
		ID:         "session/" + secretSessionID + "/message/0",
		Text:       "OAuth session note",
		Metadata:   map[string]string{"session_id": secretSessionID},
		Provenance: &Provenance{SourceType: sessionSourceType, SourceID: secretSessionID, SessionID: secretSessionID},
	}); err != nil {
		t.Fatalf("Add(secret session) error = %v", err)
	}

	if strings.Contains(store.Documents[0].ID, secretSessionID) || strings.Contains(store.Corpus.SessionIDs[0], secretSessionID) {
		t.Fatalf("store leaked raw session secret: doc=%#v corpus=%#v", store.Documents[0], store.Corpus)
	}

	if removed := store.Purge(PurgeFilter{SessionID: secretSessionID}); removed != 1 {
		t.Fatalf("purge removed %d, want 1", removed)
	}
	if len(store.Documents) != 0 || store.Corpus.SessionCount != 0 {
		t.Fatalf("store after purge = docs:%#v corpus:%#v, want empty", store.Documents, store.Corpus)
	}
}

func TestStore_PurgeUsesCustomRedactionRulesBeforeMatching(t *testing.T) {
	t.Parallel()

	const sessionID = "ACME-12345"
	store := NewStore()
	if err := store.SetCustomRedactionRules(`ACME-[0-9]+`); err != nil {
		t.Fatalf("SetCustomRedactionRules() error = %v", err)
	}

	// Simulate a legacy store loaded before the custom redaction rule existed.
	store.Documents = []Document{{
		ID:         "session/" + sessionID + "/message/0",
		Text:       "legacy custom-redacted session",
		Metadata:   map[string]string{"session_id": sessionID},
		Provenance: &Provenance{SourceType: sessionSourceType, SourceID: sessionID, SessionID: sessionID},
	}}

	if removed := store.Purge(PurgeFilter{SessionID: sessionID}); removed != 1 {
		t.Fatalf("purge removed %d, want 1", removed)
	}
	if len(store.Documents) != 0 || store.Corpus.SessionCount != 0 {
		t.Fatalf("store after purge = docs:%#v corpus:%#v, want empty", store.Documents, store.Corpus)
	}
}

func TestStore_SearchRedactsLegacyDocumentsBeforeMatching(t *testing.T) {
	t.Parallel()

	const secret = "sk-1234567890abcdefSECRET"

	store := NewStore()
	// Bypass Add to simulate a legacy on-disk store created before redaction.
	store.Documents = []Document{{
		ID:   "legacy",
		Text: "legacy raw secret " + secret,
	}}

	results, err := store.Search(secret, 1)
	if err != nil {
		t.Fatalf("Search(secret) error = %v", err)
	}
	if len(results) != 0 {
		t.Fatalf("Search(secret) results = %#v, want no match against raw secret", results)
	}

	results, err = store.Search("legacy raw secret", 1)
	if err != nil {
		t.Fatalf("Search(legacy) error = %v", err)
	}
	if len(results) != 1 {
		t.Fatalf("Search(legacy) len = %d, want 1", len(results))
	}
	if strings.Contains(results[0].Snippet, secret) || strings.Contains(strings.Join(results[0].Matches, ","), "1234567890abcdefSECRET") {
		t.Fatalf("legacy result leaked secret: %#v", results[0])
	}
	if strings.Contains(results[0].Document.Text, secret) {
		t.Fatalf("legacy result document text leaked secret: %q", results[0].Document.Text)
	}
}

func TestStore_SearchDoesNotMatchRedactionMarkerTokens(t *testing.T) {
	t.Parallel()

	const bearer = "Bearer abcdef1234567890TOKEN"

	store := NewStore()
	if err := store.Add(Document{
		ID:   "bearer",
		Text: "OAuth leak investigation " + bearer,
	}); err != nil {
		t.Fatalf("Add() error = %v", err)
	}

	results, err := store.Search(bearer, 10)
	if err != nil {
		t.Fatalf("Search(bearer) error = %v", err)
	}
	if len(results) != 0 {
		t.Fatalf("Search(bearer) results = %#v, want no match against redaction marker tokens", results)
	}

	results, err = store.Search("oauth investigation", 10)
	if err != nil {
		t.Fatalf("Search(oauth) error = %v", err)
	}
	if len(results) != 1 {
		t.Fatalf("Search(oauth) len = %d, want 1", len(results))
	}
	if strings.Contains(results[0].Snippet, bearer) || strings.Contains(results[0].Document.Text, bearer) {
		t.Fatalf("result leaked bearer token: %#v", results[0])
	}
}

func TestStore_SearchDoesNotMatchRedactionMarkerFingerprint(t *testing.T) {
	t.Parallel()

	store := NewStore()
	// Bypass Add to simulate a legacy store whose text already contains a
	// fingerprinted redaction marker from an earlier identifier-style pass.
	store.Documents = []Document{{
		ID:   "legacy-marker",
		Text: "OAuth leak [REDACTED:openai_api_key]#deadbeef1234",
	}}

	results, err := store.Search("deadbeef1234", 10)
	if err != nil {
		t.Fatalf("Search(fingerprint) error = %v", err)
	}
	if len(results) != 0 {
		t.Fatalf("Search(fingerprint) results = %#v, want no match against redaction fingerprint", results)
	}

	results, err = store.Search("oauth leak", 10)
	if err != nil {
		t.Fatalf("Search(oauth) error = %v", err)
	}
	if len(results) != 1 {
		t.Fatalf("Search(oauth) len = %d, want 1", len(results))
	}
}

func TestStore_CustomRedactionRulesApplyBeforeIndexing(t *testing.T) {
	t.Parallel()

	store := NewStore()
	if err := store.SetCustomRedactionRules(`ACME-[0-9]+`); err != nil {
		t.Fatalf("SetCustomRedactionRules() error = %v", err)
	}

	if err := store.AddText("custom", "customer ACME-12345 should be hidden"); err != nil {
		t.Fatalf("AddText() error = %v", err)
	}

	if strings.Contains(store.Documents[0].Text, "ACME-12345") {
		t.Fatalf("stored text contains custom-redacted value: %q", store.Documents[0].Text)
	}
	if !strings.Contains(store.Documents[0].Text, "[REDACTED:custom_1]") {
		t.Fatalf("stored text = %q, want custom redaction marker", store.Documents[0].Text)
	}
}

func TestStore_SetRedactorRedactsExistingDocumentsAndCorpus(t *testing.T) {
	t.Parallel()

	const secret = "ACME-12345"

	store := NewStore()
	store.Corpus = CorpusMetadata{Scope: ScopeRepo, RepoPath: "/repo/" + secret, Tags: []string{secret}}
	if err := store.Add(Document{
		ID:         "session/" + secret + "/message/0",
		Text:       "customer " + secret + " should be hidden",
		Metadata:   map[string]string{"session_id": secret, "repo_path": "/repo/" + secret, "tags": secret},
		Provenance: &Provenance{SourceType: sessionSourceType, SessionID: secret, RepoPath: "/repo/" + secret, Tags: []string{secret}},
	}); err != nil {
		t.Fatalf("AddText() error = %v", err)
	}
	if !strings.Contains(store.Documents[0].Text, secret) {
		t.Fatalf("document was redacted before custom rule was installed: %#v", store.Documents[0])
	}

	if err := store.SetCustomRedactionRules(`ACME-[0-9]+`); err != nil {
		t.Fatalf("SetCustomRedactionRules() error = %v", err)
	}

	if strings.Contains(store.Documents[0].ID, secret) ||
		strings.Contains(store.Documents[0].Text, secret) ||
		strings.Contains(store.Documents[0].Metadata["repo_path"], secret) ||
		strings.Contains(store.Documents[0].Provenance.SessionID, secret) ||
		strings.Contains(store.Corpus.RepoPath, secret) ||
		strings.Contains(store.Corpus.Tags[0], secret) {
		t.Fatalf("store still contains raw custom-redacted value: doc=%#v corpus=%#v", store.Documents[0], store.Corpus)
	}
	if !strings.Contains(store.Documents[0].ID, "[REDACTED:custom_1]#") ||
		!strings.Contains(store.Corpus.Tags[0], "[REDACTED:custom_1]#") {
		t.Fatalf("store missing fingerprinted custom redaction: doc=%#v corpus=%#v", store.Documents[0], store.Corpus)
	}
}

func TestStore_SaveRedactsLoadedLegacyDocuments(t *testing.T) {
	t.Parallel()

	const secret = "sk-abcdef1234567890SECRET"

	path := filepath.Join(t.TempDir(), "memory.json")
	if err := os.WriteFile(path, []byte(`{"corpus":{"scope":"repo","repo_path":"`+secret+`"},"documents":[{"id":"legacy","text":"legacy api_key=`+secret+`"}]}`), 0o600); err != nil {
		t.Fatalf("WriteFile() error = %v", err)
	}

	store, err := Load(path)
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if strings.Contains(store.Corpus.RepoPath, secret) || strings.Contains(store.Documents[0].Text, secret) {
		t.Fatalf("loaded legacy store contains raw secret: corpus=%#v doc=%#v", store.Corpus, store.Documents[0])
	}
	if saveErr := store.Save(path); saveErr != nil {
		t.Fatalf("Save() error = %v", saveErr)
	}

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile() error = %v", err)
	}
	if strings.Contains(string(data), secret) {
		t.Fatalf("saved legacy store contains raw secret:\n%s", data)
	}
	if !strings.Contains(string(data), `"schema_version": 1`) ||
		!strings.Contains(string(data), `"corpus"`) ||
		!strings.Contains(string(data), `"created_at"`) ||
		!strings.Contains(string(data), `"updated_at"`) ||
		!strings.Contains(string(data), `"provenance"`) ||
		!strings.Contains(string(data), `"policy"`) {
		t.Fatalf("saved legacy store is missing schema, corpus, timestamps, provenance, or policy:\n%s", data)
	}
}

func TestStore_SavePersistsSchemaCorpusAndTimestamps(t *testing.T) {
	t.Parallel()

	store := NewStore()
	store.Corpus = CorpusMetadata{Scope: ScopeRepo, RepoPath: "/repo"}
	if err := store.Add(Document{
		ID:         "design",
		Path:       "/repo/design.md",
		Text:       "memory design",
		Provenance: &Provenance{SourceType: ScopeFile, Path: "/repo/design.md"},
	}); err != nil {
		t.Fatalf("Add() error = %v", err)
	}

	path := filepath.Join(t.TempDir(), "memory.json")
	if err := store.Save(path); err != nil {
		t.Fatalf("Save() error = %v", err)
	}

	loaded, err := Load(path)
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}

	if loaded.SchemaVersion != SchemaVersion {
		t.Fatalf("schema version = %d, want %d", loaded.SchemaVersion, SchemaVersion)
	}
	if loaded.CreatedAt.IsZero() || loaded.UpdatedAt.IsZero() {
		t.Fatalf("timestamps not persisted: created=%v updated=%v", loaded.CreatedAt, loaded.UpdatedAt)
	}
	if loaded.Corpus.Scope != ScopeRepo || loaded.Corpus.RepoPath != "/repo" || loaded.Corpus.DocumentCount != 1 {
		t.Fatalf("corpus = %#v, want repo metadata with document count", loaded.Corpus)
	}
	if len(loaded.Documents) != 1 {
		t.Fatalf("documents len = %d, want 1", len(loaded.Documents))
	}
	doc := loaded.Documents[0]
	if doc.Provenance == nil || doc.Provenance.SourceType != ScopeFile || doc.Provenance.Path != "/repo/design.md" {
		t.Fatalf("provenance = %#v, want persisted file source provenance", doc.Provenance)
	}
	if doc.Policy == nil || doc.Policy.Scope != ScopeFile {
		t.Fatalf("policy = %#v, want persisted file policy decision", doc.Policy)
	}
}

func TestLoadClearsStaleLegacyCorpusMetadata(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), "memory.json")
	oldDate := time.Date(2026, 4, 1, 12, 0, 0, 0, time.UTC)
	newDate := time.Date(2026, 5, 1, 12, 0, 0, 0, time.UTC)
	data := `{
  "corpus": {
    "scope": "repo",
    "repo_path": "/repo/old",
    "agent": "old-agent",
    "date_start": "` + oldDate.Add(-time.Hour).Format(time.RFC3339) + `",
    "date_end": "` + oldDate.Add(time.Hour).Format(time.RFC3339) + `"
  },
  "documents": [
    {
      "id": "session/new/message/0",
      "text": "new auth",
      "metadata": {
        "session_id": "new",
        "repo_path": "/repo/new",
        "agent": "new-agent",
        "updated_at": "` + newDate.Format(time.RFC3339) + `"
      },
      "provenance": {
        "session_id": "new",
        "repo_path": "/repo/new",
        "agent": "new-agent",
        "updated_at": "` + newDate.Format(time.RFC3339) + `"
      }
    }
  ]
}`
	if err := os.WriteFile(path, []byte(data), 0o600); err != nil {
		t.Fatalf("WriteFile() error = %v", err)
	}

	loaded, err := Load(path)
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}

	if loaded.Corpus.RepoPath != "" || loaded.Corpus.Agent != "" {
		t.Fatalf("loaded corpus = %#v, want stale repo and agent metadata cleared", loaded.Corpus)
	}
	if loaded.Corpus.DateStart != "" || loaded.Corpus.DateEnd != "" {
		t.Fatalf("loaded corpus = %#v, want stale date range metadata cleared", loaded.Corpus)
	}
	if loaded.Corpus.Scope != ScopeManual {
		t.Fatalf("loaded corpus scope = %q, want manual after stale repo cleanup", loaded.Corpus.Scope)
	}
	if strings.Contains(loaded.Corpus.Description, "/repo/old") ||
		strings.Contains(loaded.Corpus.Description, "old-agent") ||
		strings.Contains(loaded.Corpus.Description, "date_range=") {
		t.Fatalf("loaded corpus description = %q, want stale values removed", loaded.Corpus.Description)
	}
}

func TestLoadClearsGlobalScopeWithoutOptIn(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), "memory.json")
	data := `{
  "corpus": {"scope": "global"},
  "documents": [
    {"id": "session/demo/message/0", "text": "demo auth", "metadata": {"session_id": "demo"}}
  ]
}`
	if err := os.WriteFile(path, []byte(data), 0o600); err != nil {
		t.Fatalf("WriteFile() error = %v", err)
	}

	loaded, err := Load(path)
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}

	if loaded.Corpus.Scope != ScopeManual || loaded.Corpus.Global {
		t.Fatalf("loaded corpus = %#v, want non-opt-in global scope downgraded to manual", loaded.Corpus)
	}
	if strings.Contains(loaded.Corpus.Description, "scope=global") || strings.Contains(loaded.Corpus.Description, "global=opt-in") {
		t.Fatalf("loaded corpus description = %q, want no global claim", loaded.Corpus.Description)
	}
}

func TestLoadClearsStaleGlobalOptInOutsideGlobalScope(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), "memory.json")
	data := `{
  "corpus": {"scope": "repo", "repo_path": "/repo", "global": true},
  "documents": [
    {
      "id": "session/demo/message/0",
      "text": "demo auth",
      "metadata": {"session_id": "demo", "repo_path": "/repo"},
      "provenance": {"session_id": "demo", "repo_path": "/repo"}
    }
  ]
}`
	if err := os.WriteFile(path, []byte(data), 0o600); err != nil {
		t.Fatalf("WriteFile() error = %v", err)
	}

	loaded, err := Load(path)
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}

	if loaded.Corpus.Scope != ScopeRepo || loaded.Corpus.Global {
		t.Fatalf("loaded corpus = %#v, want repo scope without stale global opt-in", loaded.Corpus)
	}
	if strings.Contains(loaded.Corpus.Description, "global=opt-in") {
		t.Fatalf("loaded corpus description = %q, want no stale global opt-in marker", loaded.Corpus.Description)
	}
}

func TestLoadDowngradesUnsupportedCorpusScope(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), "memory.json")
	data := `{
  "corpus": {"scope": "everything"},
  "documents": [
    {"id": "session/demo/message/0", "text": "demo auth", "metadata": {"session_id": "demo"}}
  ]
}`
	if err := os.WriteFile(path, []byte(data), 0o600); err != nil {
		t.Fatalf("WriteFile() error = %v", err)
	}

	loaded, err := Load(path)
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}

	if loaded.Corpus.Scope != ScopeManual {
		t.Fatalf("loaded corpus scope = %q, want unsupported scope downgraded to manual", loaded.Corpus.Scope)
	}
	if strings.Contains(loaded.Corpus.Description, "everything") {
		t.Fatalf("loaded corpus description = %q, want unsupported scope removed", loaded.Corpus.Description)
	}
}

func TestLoadRecountsLegacySessionCorpus(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), "memory.json")
	if err := os.WriteFile(path, []byte(`{"documents":[{"id":"session/legacy/message/0","text":"legacy auth"}]}`), 0o600); err != nil {
		t.Fatalf("WriteFile() error = %v", err)
	}

	loaded, err := Load(path)
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}

	if loaded.Corpus.DocumentCount != 1 || loaded.Corpus.SessionCount != 1 {
		t.Fatalf("corpus = %#v, want recounted legacy session corpus", loaded.Corpus)
	}
	if !reflect.DeepEqual(loaded.Corpus.SessionIDs, []string{"legacy"}) {
		t.Fatalf("session ids = %#v, want legacy", loaded.Corpus.SessionIDs)
	}
	if loaded.Documents[0].Provenance == nil ||
		loaded.Documents[0].Provenance.SourceType != sessionSourceType ||
		loaded.Documents[0].Provenance.SessionID != "legacy" ||
		loaded.Documents[0].Provenance.Kind != "message" {
		t.Fatalf("legacy provenance = %#v, want session source inferred from ID", loaded.Documents[0].Provenance)
	}
	if loaded.Documents[0].Policy == nil || loaded.Documents[0].Policy.Scope != ScopeSession {
		t.Fatalf("legacy policy = %#v, want session scope inferred from ID", loaded.Documents[0].Policy)
	}
}

func TestLoadRecountsLegacyPathOnlyFileCorpus(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	notePath := filepath.Join(dir, "note.txt")
	storePath := filepath.Join(dir, "memory.json")
	if err := os.WriteFile(storePath, []byte(`{"documents":[{"id":"`+notePath+`","path":"`+notePath+`","text":"legacy file auth"}]}`), 0o600); err != nil {
		t.Fatalf("WriteFile() error = %v", err)
	}

	loaded, err := Load(storePath)
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}

	if loaded.Corpus.DocumentCount != 1 || loaded.Corpus.FileCount != 1 || loaded.Corpus.Scope != ScopeFile {
		t.Fatalf("corpus = %#v, want recounted legacy file corpus", loaded.Corpus)
	}
	if loaded.Documents[0].Provenance == nil ||
		loaded.Documents[0].Provenance.SourceType != ScopeFile ||
		loaded.Documents[0].Provenance.SourceID != notePath ||
		loaded.Documents[0].Provenance.Path != notePath ||
		loaded.Documents[0].Provenance.Kind != ScopeFile {
		t.Fatalf("legacy provenance = %#v, want file source inferred from path", loaded.Documents[0].Provenance)
	}
	if loaded.Documents[0].Policy == nil || loaded.Documents[0].Policy.Scope != ScopeFile {
		t.Fatalf("legacy policy = %#v, want file scope inferred from path", loaded.Documents[0].Policy)
	}
}

func TestStore_PurgeByRepoMatchesLegacyFileIDPath(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	notePath := filepath.Join(dir, "note.txt")

	store := NewStore()
	if err := store.Add(Document{
		ID:       notePath,
		Text:     "legacy file auth",
		Metadata: map[string]string{"source_type": ScopeFile, "kind": ScopeFile},
	}); err != nil {
		t.Fatalf("Add(legacy file) error = %v", err)
	}

	if store.Documents[0].Provenance == nil || store.Documents[0].Provenance.Path != notePath {
		t.Fatalf("legacy file provenance = %#v, want ID copied into path", store.Documents[0].Provenance)
	}

	if removed := store.Purge(PurgeFilter{RepoPath: dir}); removed != 1 {
		t.Fatalf("repo purge removed %d, want legacy file document", removed)
	}
	if len(store.Documents) != 0 {
		t.Fatalf("documents after purge = %#v, want empty", store.Documents)
	}
}

func TestStore_PurgeBySessionTagRepoAndAll(t *testing.T) {
	t.Parallel()

	store := NewStore()
	docs := []Document{
		{
			ID:         "session/one/message/0",
			Text:       "one auth",
			Metadata:   map[string]string{"session_id": "one", "tags": "auth", "worktree_path": "/repo/one"},
			Provenance: &Provenance{SessionID: "one", Tags: []string{"auth"}, RepoPath: "/repo/one"},
		},
		{
			ID:         "session/two/message/0",
			Text:       "two docs",
			Metadata:   map[string]string{"session_id": "two", "tags": "docs", "worktree_path": "/repo/two"},
			Provenance: &Provenance{SessionID: "two", Tags: []string{"docs"}, RepoPath: "/repo/two"},
		},
		{
			ID:         "session/three/message/0",
			Text:       "three auth",
			Metadata:   map[string]string{"session_id": "three", "tags": "auth", "worktree_path": "/repo/three"},
			Provenance: &Provenance{SessionID: "three", Tags: []string{"auth"}, RepoPath: "/repo/three"},
		},
		{
			ID:         "file-in-repo-two",
			Path:       "/repo/two/notes.txt",
			Text:       "repo two file",
			Metadata:   map[string]string{"source_type": ScopeFile, "kind": ScopeFile, "path": "/repo/two/notes.txt"},
			Provenance: &Provenance{SourceType: ScopeFile, Path: "/repo/two/notes.txt"},
		},
	}
	for _, doc := range docs {
		if err := store.Add(doc); err != nil {
			t.Fatalf("Add(%s) error = %v", doc.ID, err)
		}
	}

	if removed := store.Purge(PurgeFilter{SessionID: "one"}); removed != 1 {
		t.Fatalf("session purge removed %d, want 1", removed)
	}
	if removed := store.Purge(PurgeFilter{RepoPath: "/repo/two"}); removed != 2 {
		t.Fatalf("repo purge removed %d, want 2", removed)
	}
	if removed := store.Purge(PurgeFilter{Tag: "auth"}); removed != 1 {
		t.Fatalf("tag purge removed %d, want 1", removed)
	}
	if removed := store.Purge(PurgeFilter{All: true}); removed != 0 {
		t.Fatalf("empty all purge removed %d, want 0", removed)
	}
}

func TestStore_PurgeIgnoresBlankSelectors(t *testing.T) {
	t.Parallel()

	store := NewStore()
	if err := store.Add(Document{
		ID:         "session/one/message/0",
		Text:       "one auth",
		Metadata:   map[string]string{"session_id": "one", "tags": "auth"},
		Provenance: &Provenance{SessionID: "one", Tags: []string{"auth"}},
	}); err != nil {
		t.Fatalf("Add(one) error = %v", err)
	}

	if removed := store.Purge(PurgeFilter{SessionID: " \t "}); removed != 0 {
		t.Fatalf("blank session purge removed %d, want 0", removed)
	}
	if removed := store.Purge(PurgeFilter{Tag: " \t "}); removed != 0 {
		t.Fatalf("blank tag purge removed %d, want 0", removed)
	}
	if removed := store.Purge(PurgeFilter{RepoPath: " \t "}); removed != 0 {
		t.Fatalf("blank repo purge removed %d, want 0", removed)
	}
	if len(store.Documents) != 1 {
		t.Fatalf("documents len after blank purge = %d, want 1", len(store.Documents))
	}
}

//nolint:paralleltest // Uses t.Chdir to ensure relative session IDs are not treated as repo paths.
func TestStore_PurgeByRepoDoesNotRemoveSessionWithoutRepoProvenance(t *testing.T) {
	dir := t.TempDir()
	t.Chdir(dir)

	store := NewStore()
	if err := store.Add(Document{
		ID:   "session/legacy/message/0",
		Path: "legacy",
		Text: "legacy session without repo provenance",
	}); err != nil {
		t.Fatalf("Add(legacy) error = %v", err)
	}
	if err := store.Add(Document{
		ID:       "file-note",
		Path:     filepath.Join(dir, "note.txt"),
		Text:     "repo file note",
		Metadata: map[string]string{"source_type": ScopeFile, "kind": ScopeFile, "path": filepath.Join(dir, "note.txt")},
	}); err != nil {
		t.Fatalf("Add(file) error = %v", err)
	}

	if removed := store.Purge(PurgeFilter{RepoPath: dir}); removed != 1 {
		t.Fatalf("repo purge removed %d, want only the file document", removed)
	}

	if len(store.Documents) != 1 || store.Documents[0].ID != "session/legacy/message/0" {
		t.Fatalf("remaining documents = %#v, want legacy session retained", store.Documents)
	}
}

func TestStore_RecountsTagsAndClearsCorpusOnPurgeAll(t *testing.T) {
	t.Parallel()

	store := NewStore()
	store.Corpus = CorpusMetadata{Scope: ScopeTags, Tags: []string{"auth", "stale"}, SessionIDs: []string{"old"}}
	if err := store.Add(Document{
		ID:         "session/one/message/0",
		Text:       "one auth",
		Metadata:   map[string]string{"session_id": "one", "tags": "auth"},
		Provenance: &Provenance{SessionID: "one", Tags: []string{"auth"}},
	}); err != nil {
		t.Fatalf("Add(one) error = %v", err)
	}
	if err := store.Add(Document{
		ID:         "session/two/message/0",
		Text:       "two docs",
		Metadata:   map[string]string{"session_id": "two", "tags": "docs"},
		Provenance: &Provenance{SessionID: "two", Tags: []string{"docs"}},
	}); err != nil {
		t.Fatalf("Add(two) error = %v", err)
	}

	if !reflect.DeepEqual(store.Corpus.Tags, []string{"auth", "docs"}) {
		t.Fatalf("tags = %#v, want auth/docs", store.Corpus.Tags)
	}

	if removed := store.Purge(PurgeFilter{Tag: "auth"}); removed != 1 {
		t.Fatalf("tag purge removed %d, want 1", removed)
	}
	if !reflect.DeepEqual(store.Corpus.Tags, []string{"docs"}) {
		t.Fatalf("tags after tag purge = %#v, want docs", store.Corpus.Tags)
	}

	if removed := store.Purge(PurgeFilter{All: true}); removed != 1 {
		t.Fatalf("all purge removed %d, want remaining doc", removed)
	}
	if store.Corpus.Scope != ScopeManual ||
		store.Corpus.DocumentCount != 0 ||
		len(store.Corpus.Tags) != 0 ||
		len(store.Corpus.SessionIDs) != 0 ||
		strings.Contains(store.Corpus.Description, "auth") ||
		strings.Contains(store.Corpus.Description, "stale") {
		t.Fatalf("corpus after purge all = %#v, want reset manual empty corpus", store.Corpus)
	}
}

func TestStore_PurgeClearsStaleRepoCorpusMetadata(t *testing.T) {
	t.Parallel()

	firstRepo := filepath.Join(string(os.PathSeparator), "repo", "one")
	secondRepo := filepath.Join(string(os.PathSeparator), "repo", "two")
	store := NewStore()
	store.Corpus = CorpusMetadata{Scope: ScopeRepo, RepoPath: firstRepo}

	for _, doc := range []Document{
		{
			ID:         "session/one/message/0",
			Text:       "one auth",
			Metadata:   map[string]string{"session_id": "one", "repo_path": firstRepo},
			Provenance: &Provenance{SessionID: "one", RepoPath: firstRepo},
		},
		{
			ID:         "session/two/message/0",
			Text:       "two auth",
			Metadata:   map[string]string{"session_id": "two", "repo_path": secondRepo},
			Provenance: &Provenance{SessionID: "two", RepoPath: secondRepo},
		},
	} {
		if err := store.Add(doc); err != nil {
			t.Fatalf("Add(%s) error = %v", doc.ID, err)
		}
	}

	if removed := store.Purge(PurgeFilter{RepoPath: firstRepo}); removed != 1 {
		t.Fatalf("repo purge removed %d, want 1", removed)
	}
	if len(store.Documents) != 1 {
		t.Fatalf("documents after purge = %#v, want one remaining", store.Documents)
	}
	if store.Corpus.RepoPath != "" || strings.Contains(store.Corpus.Description, firstRepo) {
		t.Fatalf("corpus after repo purge = %#v, want purged repo metadata cleared", store.Corpus)
	}
	if store.Corpus.Scope != ScopeManual {
		t.Fatalf("corpus scope after repo purge = %q, want manual mixed corpus", store.Corpus.Scope)
	}
}

func TestStore_ApplyRetentionRemovesExpiredSessionDocuments(t *testing.T) {
	t.Parallel()

	oldTime := time.Date(2026, 4, 1, 12, 0, 0, 0, time.UTC)
	newTime := time.Date(2026, 5, 1, 12, 0, 0, 0, time.UTC)
	store := NewStore()
	docs := []Document{
		{
			ID:         "session/old/message/0",
			Text:       "old auth",
			Provenance: &Provenance{SessionID: "old", UpdatedAt: oldTime.Format(time.RFC3339)},
		},
		{
			ID:         "session/new/message/0",
			Text:       "new auth",
			Provenance: &Provenance{SessionID: "new", UpdatedAt: newTime.Format(time.RFC3339)},
		},
		{ID: "file/no-date", Text: "file auth"},
	}
	for _, doc := range docs {
		if err := store.Add(doc); err != nil {
			t.Fatalf("Add(%s) error = %v", doc.ID, err)
		}
	}

	removed := store.ApplyRetention(time.Date(2026, 4, 15, 0, 0, 0, 0, time.UTC))
	if removed != 1 {
		t.Fatalf("ApplyRetention() removed %d, want 1", removed)
	}

	if len(store.Documents) != 2 {
		t.Fatalf("documents len = %d, want 2", len(store.Documents))
	}
	for _, doc := range store.Documents {
		if doc.ID == "session/old/message/0" {
			t.Fatalf("expired document was retained: %#v", store.Documents)
		}
	}
}

func TestStore_ApplyRetentionClearsCorpusWhenAllDocumentsExpire(t *testing.T) {
	t.Parallel()

	oldTime := time.Date(2026, 4, 1, 12, 0, 0, 0, time.UTC)
	store := NewStore()
	store.Corpus = CorpusMetadata{
		Scope:       ScopeSession,
		SessionIDs:  []string{"old"},
		Tags:        []string{"security"},
		Retention:   testRetentionThirtyDays,
		Description: "scope=session tags=security retention=30 days",
	}

	if err := store.Add(Document{
		ID:         "session/old/message/0",
		Text:       "old auth",
		Metadata:   map[string]string{"session_id": "old", "tags": "security", "updated_at": oldTime.Format(time.RFC3339)},
		Provenance: &Provenance{SourceType: sessionSourceType, SessionID: "old", Tags: []string{"security"}, UpdatedAt: oldTime.Format(time.RFC3339)},
	}); err != nil {
		t.Fatalf("Add() error = %v", err)
	}
	store.Corpus.Scope = ScopeSession

	removed := store.ApplyRetention(time.Date(2026, 4, 15, 0, 0, 0, 0, time.UTC))
	if removed != 1 {
		t.Fatalf("ApplyRetention() removed %d, want 1", removed)
	}
	if len(store.Documents) != 0 {
		t.Fatalf("documents len = %d, want 0", len(store.Documents))
	}
	if store.Corpus.Scope != ScopeManual ||
		store.Corpus.SessionCount != 0 ||
		len(store.Corpus.SessionIDs) != 0 ||
		len(store.Corpus.Tags) != 0 ||
		store.Corpus.Retention != testRetentionThirtyDays {
		t.Fatalf("corpus after full retention = %#v, want manual empty corpus with retention policy", store.Corpus)
	}
	if strings.Contains(store.Corpus.Description, "old") || strings.Contains(store.Corpus.Description, "security") {
		t.Fatalf("corpus description after full retention = %q, want no expired selectors", store.Corpus.Description)
	}
}

func TestStore_ApplyRetentionClearsStaleCorpusMetadata(t *testing.T) {
	t.Parallel()

	oldTime := time.Date(2026, 4, 1, 12, 0, 0, 0, time.UTC)
	newTime := time.Date(2026, 5, 1, 12, 0, 0, 0, time.UTC)
	oldRepo := filepath.Join(string(os.PathSeparator), "repo", "old")
	newRepo := filepath.Join(string(os.PathSeparator), "repo", "new")
	store := NewStore()
	store.Corpus = CorpusMetadata{Scope: ScopeRepo, RepoPath: oldRepo, Agent: "old-agent"}

	for _, doc := range []Document{
		{
			ID:         "session/old/message/0",
			Text:       "old auth",
			Metadata:   map[string]string{"session_id": "old", "repo_path": oldRepo, "agent": "old-agent", "updated_at": oldTime.Format(time.RFC3339)},
			Provenance: &Provenance{SessionID: "old", RepoPath: oldRepo, Agent: "old-agent", UpdatedAt: oldTime.Format(time.RFC3339)},
		},
		{
			ID:         "session/new/message/0",
			Text:       "new auth",
			Metadata:   map[string]string{"session_id": "new", "repo_path": newRepo, "agent": "new-agent", "updated_at": newTime.Format(time.RFC3339)},
			Provenance: &Provenance{SessionID: "new", RepoPath: newRepo, Agent: "new-agent", UpdatedAt: newTime.Format(time.RFC3339)},
		},
	} {
		if err := store.Add(doc); err != nil {
			t.Fatalf("Add(%s) error = %v", doc.ID, err)
		}
	}

	removed := store.ApplyRetention(time.Date(2026, 4, 15, 0, 0, 0, 0, time.UTC))
	if removed != 1 {
		t.Fatalf("ApplyRetention() removed %d, want 1", removed)
	}
	if store.Corpus.RepoPath != "" || store.Corpus.Agent != "" {
		t.Fatalf("corpus after retention = %#v, want stale repo and agent metadata cleared", store.Corpus)
	}
	if strings.Contains(store.Corpus.Description, oldRepo) || strings.Contains(store.Corpus.Description, "old-agent") {
		t.Fatalf("corpus description after retention = %q, want stale metadata removed", store.Corpus.Description)
	}
	if store.Corpus.Scope != ScopeManual {
		t.Fatalf("corpus scope after retention = %q, want manual mixed corpus", store.Corpus.Scope)
	}
}

func TestStore_ApplyPolicyUpdatesDocumentPolicy(t *testing.T) {
	t.Parallel()

	store := NewStore()
	if err := store.AddText("note", "retained auth note"); err != nil {
		t.Fatalf("AddText() error = %v", err)
	}
	store.Documents[0].Policy = nil

	store.ApplyPolicy(ScopeRepo, testRetentionThirtyDays)

	policy := store.Documents[0].Policy
	if policy == nil || policy.Scope != ScopeRepo || policy.Retention != testRetentionThirtyDays {
		t.Fatalf("policy = %#v, want repo scope and 30 day retention", policy)
	}
}

func findDocumentByID(t *testing.T, store *Store, id string) Document {
	t.Helper()

	for i := range store.Documents {
		if store.Documents[i].ID == id {
			return store.Documents[i]
		}
	}

	t.Fatalf("document %q not found in %#v", id, store.Documents)

	return Document{}
}
