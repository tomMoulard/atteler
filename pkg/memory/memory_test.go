package memory

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/tommoulard/atteler/pkg/privacy"
	"github.com/tommoulard/atteler/pkg/retrieval"
)

const duplicateID = "same"

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

func TestStore_SearchReturnsDefensiveDocumentCopies(t *testing.T) {
	t.Parallel()

	expiresAt := time.Now().UTC().Add(time.Hour)

	store := NewStore()
	if err := store.Add(Document{
		ID:        "auth",
		Text:      "OAuth callback token refresh guidance",
		ExpiresAt: &expiresAt,
		Metadata: map[string]string{
			"kind": "note",
		},
		Provenance: map[string]string{
			"source_type": "test",
		},
	}); err != nil {
		t.Fatalf("Add() error = %v", err)
	}

	results, err := store.Search("token refresh", 1)
	if err != nil {
		t.Fatalf("Search() error = %v", err)
	}

	if len(results) != 1 {
		t.Fatalf("Search() returned %d result(s), want 1", len(results))
	}

	results[0].Document.Metadata["kind"] = "tampered"
	results[0].Document.Provenance["privacy_policy"] = "stale"
	*results[0].Document.ExpiresAt = time.Now().UTC().Add(-time.Hour)

	results, err = store.Search("token refresh", 1)
	if err != nil {
		t.Fatalf("Search() after result mutation error = %v", err)
	}

	if len(results) != 1 {
		t.Fatalf("Search() after result mutation returned %d result(s), want 1", len(results))
	}

	if got := results[0].Document.Metadata["kind"]; got != "note" {
		t.Fatalf("result metadata kind = %q, want note", got)
	}

	if got := results[0].Document.Provenance["privacy_policy"]; got != privacy.RedactionPolicyVersion {
		t.Fatalf("result privacy policy = %q, want %q", got, privacy.RedactionPolicyVersion)
	}

	if results[0].Document.ExpiresAt == nil || !results[0].Document.ExpiresAt.Equal(expiresAt) {
		t.Fatalf("result expires_at = %v, want %v", results[0].Document.ExpiresAt, expiresAt)
	}
}

func TestStore_AddReplacesExistingDocument(t *testing.T) {
	t.Parallel()

	store := NewStore()
	if err := store.AddText(duplicateID, "old auth text"); err != nil {
		t.Fatalf("AddText(old) error = %v", err)
	}

	if err := store.AddText(duplicateID, "new release text"); err != nil {
		t.Fatalf("AddText(new) error = %v", err)
	}

	if len(store.Documents) != 1 {
		t.Fatalf("documents len = %d, want 1", len(store.Documents))
	}

	if store.Documents[0].Text != "new release text" {
		t.Fatalf("document text = %q, want replacement", store.Documents[0].Text)
	}
}

func TestStore_AddRejectsInvalidUTF8Text(t *testing.T) {
	t.Parallel()

	store := NewStore()

	err := store.AddText("invalid", string([]byte{0xff}))
	if !errors.Is(err, ErrInvalidUTF8) {
		t.Fatalf("AddText(invalid UTF-8) error = %v, want ErrInvalidUTF8", err)
	}

	if len(store.Documents) != 0 {
		t.Fatalf("documents len = %d, want 0", len(store.Documents))
	}
}

func TestStore_AddTextRecordsDirectProvenance(t *testing.T) {
	t.Parallel()

	store := NewStore()
	if err := store.AddText("note", "direct memory provenance"); err != nil {
		t.Fatalf("AddText() error = %v", err)
	}

	if got := store.Documents[0].Provenance["source_type"]; got != "direct" {
		t.Fatalf("source_type = %q, want direct", got)
	}

	if got := store.Documents[0].Provenance["privacy_policy"]; got != privacy.RedactionPolicyVersion {
		t.Fatalf("privacy_policy = %q, want %q", got, privacy.RedactionPolicyVersion)
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

	if results[0].Document.Provenance["source_type"] != "file" || results[0].Document.Provenance["path"] != filepath.Clean(path) {
		t.Fatalf("Provenance = %#v, want file path provenance", results[0].Document.Provenance)
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
	if err := store.Save(path); err != nil {
		t.Fatalf("Save() error = %v", err)
	}

	loaded, err := Load(path)
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}

	if !reflect.DeepEqual(loaded.Documents, store.Documents) {
		t.Fatalf("loaded documents = %#v, want %#v", loaded.Documents, store.Documents)
	}

	if loaded.SchemaVersion != StoreSchemaVersion {
		t.Fatalf("SchemaVersion = %d, want %d", loaded.SchemaVersion, StoreSchemaVersion)
	}

	if loaded.Normalization != StoreTextNormalization {
		t.Fatalf("Normalization = %q, want %q", loaded.Normalization, StoreTextNormalization)
	}

	if loaded.CreatedAt.IsZero() || loaded.UpdatedAt.IsZero() {
		t.Fatalf("loaded store timestamps missing: created=%v updated=%v", loaded.CreatedAt, loaded.UpdatedAt)
	}

	if loaded.Documents[0].CreatedAt.IsZero() || loaded.Documents[0].UpdatedAt.IsZero() {
		t.Fatalf("loaded document timestamps missing: created=%v updated=%v", loaded.Documents[0].CreatedAt, loaded.Documents[0].UpdatedAt)
	}

	if loaded.Documents[0].SourceHash == "" {
		t.Fatal("loaded document SourceHash is empty")
	}

	results, err := loaded.Search("rag", 1)
	if err != nil {
		t.Fatalf("Search() error = %v", err)
	}

	if len(results) != 1 || results[0].Document.ID != "design" {
		t.Fatalf("loaded search results = %#v, want design", results)
	}
}

func TestStore_SaveTightensExistingFilePermissions(t *testing.T) {
	t.Parallel()

	store := NewStore()
	if err := store.AddText("note", "private lexical memory"); err != nil {
		t.Fatalf("AddText() error = %v", err)
	}

	path := filepath.Join(t.TempDir(), "memory.json")
	//nolint:gosec // Intentionally start with loose permissions to prove Save tightens persisted memory stores.
	if err := os.WriteFile(path, []byte("{}"), 0o644); err != nil {
		t.Fatalf("WriteFile() error = %v", err)
	}

	if err := store.Save(path); err != nil {
		t.Fatalf("Save() error = %v", err)
	}

	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("Stat() error = %v", err)
	}

	if got := info.Mode().Perm(); got != 0o600 {
		t.Fatalf("memory store mode = %v, want 0600", got)
	}
}

func TestStore_RedactsSensitiveTextAndMetadataBeforePersistence(t *testing.T) {
	t.Parallel()

	store := NewStore()
	if err := store.Add(Document{
		ID:       "secret",
		Path:     "docs/secret.md?access_token=artifact123",
		Text:     "deploy password=hunter2 with api_key=abc123 Authorization: Basic basic-secret-value and config {\"api_key\":\"json-secret-value\",\"authorization\":\"Bearer json-auth-secret\"}\n-----BEGIN RSA PRIVATE KEY-----\nprivate-key-material\n-----END RSA PRIVATE KEY-----",
		Metadata: map[string]string{"auth_token": "abc123", "kind": "note"},
	}); err != nil {
		t.Fatalf("Add() error = %v", err)
	}

	doc := store.Documents[0]
	if strings.Contains(doc.Text, "hunter2") || strings.Contains(doc.Text, "abc123") {
		t.Fatalf("text was not redacted: %q", doc.Text)
	}

	if strings.Contains(doc.Text, "json-secret-value") {
		t.Fatalf("JSON-like secret was not redacted: %q", doc.Text)
	}

	if strings.Contains(doc.Text, "json-auth-secret") {
		t.Fatalf("JSON-like authorization secret was not redacted: %q", doc.Text)
	}

	if strings.Contains(doc.Text, "basic-secret-value") {
		t.Fatalf("authorization secret was not redacted: %q", doc.Text)
	}

	if strings.Contains(doc.Text, "private-key-material") || strings.Contains(doc.Text, "RSA PRIVATE KEY") {
		t.Fatalf("private key block was not redacted: %q", doc.Text)
	}

	if doc.Metadata["auth_token"] != "[REDACTED]" {
		t.Fatalf("metadata auth_token = %q, want redacted", doc.Metadata["auth_token"])
	}

	if strings.Contains(doc.Path, "artifact123") {
		t.Fatalf("path was not redacted: %q", doc.Path)
	}

	if doc.SourceHash == "" {
		t.Fatal("SourceHash is empty")
	}
}

func TestStore_RedactsSensitiveProvenanceBeforePersistence(t *testing.T) {
	t.Parallel()

	store := NewStore()
	if err := store.Add(Document{
		ID:   "provenance",
		Text: "safe memory",
		Provenance: map[string]string{
			"api_key": "abc123",
			"source":  "captured password=hunter2 from notes",
		},
	}); err != nil {
		t.Fatalf("Add() error = %v", err)
	}

	doc := store.Documents[0]
	if doc.Provenance["api_key"] != "[REDACTED]" {
		t.Fatalf("provenance api_key = %q, want redacted", doc.Provenance["api_key"])
	}

	if strings.Contains(doc.Provenance["source"], "hunter2") {
		t.Fatalf("provenance source was not redacted: %q", doc.Provenance["source"])
	}
}

func TestStore_RedactsSensitiveIDBeforePersistenceAndDelete(t *testing.T) {
	t.Parallel()

	store := NewStore()
	rawID := "docs/secret.md?access_token=artifact123"

	if err := store.Add(Document{ID: rawID, Text: "safe memory"}); err != nil {
		t.Fatalf("Add() error = %v", err)
	}

	if strings.Contains(store.Documents[0].ID, "artifact123") {
		t.Fatalf("ID was not redacted: %q", store.Documents[0].ID)
	}

	if !store.Delete(rawID) {
		t.Fatalf("Delete(%q) = false, want true for redacted ID", rawID)
	}

	if len(store.Documents) != 0 {
		t.Fatalf("documents len = %d, want 0", len(store.Documents))
	}
}

func TestStore_RedactsSensitiveIDWithoutCollapsingPathSuffix(t *testing.T) {
	t.Parallel()

	store := NewStore()
	firstID := "tenant/access_token=artifact123/first"
	secondID := "tenant/access_token=artifact123/second"

	if err := store.AddText(firstID, "first safe memory"); err != nil {
		t.Fatalf("AddText(first) error = %v", err)
	}

	if err := store.AddText(secondID, "second safe memory"); err != nil {
		t.Fatalf("AddText(second) error = %v", err)
	}

	if len(store.Documents) != 2 {
		t.Fatalf("documents len = %d, want 2", len(store.Documents))
	}

	if strings.Contains(store.Documents[0].ID, "artifact123") || strings.Contains(store.Documents[1].ID, "artifact123") {
		t.Fatalf("IDs retained raw secret: %#v", store.Documents)
	}

	if !strings.HasSuffix(store.Documents[0].ID, "/first") || !strings.HasSuffix(store.Documents[1].ID, "/second") {
		t.Fatalf("redacted IDs collapsed path suffixes: %#v", store.Documents)
	}

	if !store.Delete(firstID) {
		t.Fatal("Delete(first) = false, want true")
	}

	if len(store.Documents) != 1 || !strings.HasSuffix(store.Documents[0].ID, "/second") {
		t.Fatalf("documents after delete = %#v, want second document", store.Documents)
	}
}

func TestLoadRefusesUnredactedPersistedContent(t *testing.T) {
	t.Parallel()

	store := Store{
		SchemaVersion: StoreSchemaVersion,
		Normalization: StoreTextNormalization,
		Documents: []Document{{
			ID:         "secret",
			Text:       "deploy password=hunter2",
			Metadata:   map[string]string{"auth_token": "abc123"},
			Provenance: map[string]string{"source": "Authorization: Bearer openai-secret-value"},
		}},
	}

	path := filepath.Join(t.TempDir(), "memory.json")

	data, err := json.Marshal(store)
	if err != nil {
		t.Fatalf("Marshal() error = %v", err)
	}

	if writeErr := os.WriteFile(path, data, 0o600); writeErr != nil {
		t.Fatalf("WriteFile() error = %v", writeErr)
	}

	_, err = Load(path)
	if !errors.Is(err, ErrPrivacyPolicy) {
		t.Fatalf("Load() error = %v, want ErrPrivacyPolicy", err)
	}
}

func TestLoadRefusesUnredactedPersistedPathUnlessMigrated(t *testing.T) {
	t.Parallel()

	text := "safe memory"
	store := Store{
		SchemaVersion: StoreSchemaVersion,
		Normalization: StoreTextNormalization,
		Documents: []Document{{
			ID:         "doc.md?access_token=id123",
			Path:       "docs/secret.md?access_token=artifact123",
			Text:       text,
			SourceHash: privacy.SourceHash(text),
			Provenance: map[string]string{"source_type": "test"},
		}},
	}

	path := filepath.Join(t.TempDir(), "path-secret.json")

	data, err := json.Marshal(store)
	if err != nil {
		t.Fatalf("Marshal() error = %v", err)
	}

	if writeErr := os.WriteFile(path, data, 0o600); writeErr != nil {
		t.Fatalf("WriteFile() error = %v", writeErr)
	}

	_, err = Load(path)
	if !errors.Is(err, ErrPrivacyPolicy) {
		t.Fatalf("Load() error = %v, want ErrPrivacyPolicy", err)
	}

	migrated, err := LoadWithOptions(path, LoadOptions{Migrate: true})
	if err != nil {
		t.Fatalf("LoadWithOptions(Migrate) error = %v", err)
	}

	if strings.Contains(migrated.Documents[0].Path, "artifact123") {
		t.Fatalf("migrated path was not redacted: %q", migrated.Documents[0].Path)
	}

	if strings.Contains(migrated.Documents[0].ID, "id123") {
		t.Fatalf("migrated ID was not redacted: %q", migrated.Documents[0].ID)
	}
}

func TestLoadWithOptionsMigratesUnredactedPersistedContent(t *testing.T) {
	t.Parallel()

	store := Store{
		Documents: []Document{{
			ID:         "secret",
			Text:       "deploy password=hunter2",
			Metadata:   map[string]string{"auth_token": "abc123"},
			Provenance: map[string]string{"source": "Authorization: Bearer openai-secret-value"},
		}},
	}

	path := filepath.Join(t.TempDir(), "legacy-memory.json")

	data, err := json.Marshal(store)
	if err != nil {
		t.Fatalf("Marshal() error = %v", err)
	}

	if writeErr := os.WriteFile(path, data, 0o600); writeErr != nil {
		t.Fatalf("WriteFile() error = %v", writeErr)
	}

	migrated, err := LoadWithOptions(path, LoadOptions{Migrate: true})
	if err != nil {
		t.Fatalf("LoadWithOptions(Migrate) error = %v", err)
	}

	if migrated.SchemaVersion != StoreSchemaVersion {
		t.Fatalf("SchemaVersion = %d, want %d", migrated.SchemaVersion, StoreSchemaVersion)
	}

	if len(migrated.Documents) != 1 {
		t.Fatalf("documents len = %d, want 1", len(migrated.Documents))
	}

	doc := migrated.Documents[0]
	for _, raw := range []string{"hunter2", "abc123", "openai-secret-value"} {
		if strings.Contains(doc.Text, raw) {
			t.Fatalf("migrated text retained %q: %q", raw, doc.Text)
		}

		for key, value := range doc.Metadata {
			if strings.Contains(value, raw) {
				t.Fatalf("migrated metadata %q retained %q: %q", key, raw, value)
			}
		}

		for key, value := range doc.Provenance {
			if strings.Contains(value, raw) {
				t.Fatalf("migrated provenance %q retained %q: %q", key, raw, value)
			}
		}
	}

	if doc.SourceHash != privacy.SourceHash(doc.Text) {
		t.Fatalf("SourceHash = %q, want hash of redacted text", doc.SourceHash)
	}

	if migrated.Normalization != StoreTextNormalization {
		t.Fatalf("Normalization = %q, want %q", migrated.Normalization, StoreTextNormalization)
	}
}

func TestLoadRejectsMissingProvenanceUnlessMigrated(t *testing.T) {
	t.Parallel()

	store := Store{
		SchemaVersion: StoreSchemaVersion,
		Normalization: StoreTextNormalization,
		Documents: []Document{{
			ID:         "note",
			Text:       "safe memory without provenance",
			SourceHash: privacy.SourceHash("safe memory without provenance"),
		}},
	}

	path := filepath.Join(t.TempDir(), "missing-provenance.json")

	data, err := json.Marshal(store)
	if err != nil {
		t.Fatalf("Marshal() error = %v", err)
	}

	if writeErr := os.WriteFile(path, data, 0o600); writeErr != nil {
		t.Fatalf("WriteFile() error = %v", writeErr)
	}

	_, err = Load(path)
	if !errors.Is(err, ErrProvenanceMissing) {
		t.Fatalf("Load() error = %v, want ErrProvenanceMissing", err)
	}

	migrated, err := LoadWithOptions(path, LoadOptions{Migrate: true})
	if err != nil {
		t.Fatalf("LoadWithOptions(Migrate) error = %v", err)
	}

	if got := migrated.Documents[0].Provenance["source_type"]; got != "legacy" {
		t.Fatalf("source_type = %q, want legacy", got)
	}
}

func TestLoadRejectsMissingPrivacyPolicyUnlessMigrated(t *testing.T) {
	t.Parallel()

	store := NewStore()
	if err := store.AddText("note", "redacted memory text"); err != nil {
		t.Fatalf("AddText() error = %v", err)
	}

	delete(store.Documents[0].Provenance, "privacy_policy")

	path := filepath.Join(t.TempDir(), "missing-privacy-policy.json")

	data, err := json.Marshal(store)
	if err != nil {
		t.Fatalf("Marshal() error = %v", err)
	}

	if writeErr := os.WriteFile(path, data, 0o600); writeErr != nil {
		t.Fatalf("WriteFile() error = %v", writeErr)
	}

	_, err = Load(path)
	if !errors.Is(err, ErrPrivacyPolicy) {
		t.Fatalf("Load() error = %v, want ErrPrivacyPolicy", err)
	}

	migrated, err := LoadWithOptions(path, LoadOptions{Migrate: true})
	if err != nil {
		t.Fatalf("LoadWithOptions(Migrate) error = %v", err)
	}

	if got := migrated.Documents[0].Provenance["privacy_policy"]; got != privacy.RedactionPolicyVersion {
		t.Fatalf("privacy_policy = %q, want %q", got, privacy.RedactionPolicyVersion)
	}
}

func TestStore_MigratePreservesExistingProvenance(t *testing.T) {
	t.Parallel()

	store := Store{
		Documents: []Document{{
			ID:   "file-doc",
			Text: "safe file-backed memory",
			Provenance: map[string]string{
				"source_type": "file",
				"path":        "docs/memory-note.md",
				"api_key":     "abc123",
			},
		}},
	}

	if err := store.Migrate(); err != nil {
		t.Fatalf("Migrate() error = %v", err)
	}

	doc := store.Documents[0]
	if got := doc.Provenance["source_type"]; got != "file" {
		t.Fatalf("source_type = %q, want file", got)
	}

	wantRedacted := privacy.RedactMetadata(map[string]string{"api_key": "abc123"})["api_key"]
	if got := doc.Provenance["api_key"]; got != wantRedacted {
		t.Fatalf("api_key provenance = %q, want redacted", got)
	}

	if doc.SourceHash != privacy.SourceHash(doc.Text) {
		t.Fatalf("SourceHash = %q, want hash of migrated text", doc.SourceHash)
	}
}

func TestLoadRefusesLegacySchemaUnlessMigrated(t *testing.T) {
	t.Parallel()

	store := Store{
		Documents: []Document{{
			ID:   "legacy",
			Text: "redacted legacy memory",
		}},
	}

	path := filepath.Join(t.TempDir(), "legacy-memory.json")

	data, err := json.Marshal(store)
	if err != nil {
		t.Fatalf("Marshal() error = %v", err)
	}

	if writeErr := os.WriteFile(path, data, 0o600); writeErr != nil {
		t.Fatalf("WriteFile() error = %v", writeErr)
	}

	_, err = Load(path)
	if !errors.Is(err, ErrIncompatibleSchema) {
		t.Fatalf("Load() error = %v, want ErrIncompatibleSchema", err)
	}

	migrated, err := LoadWithOptions(path, LoadOptions{Migrate: true})
	if err != nil {
		t.Fatalf("LoadWithOptions(Migrate) error = %v", err)
	}

	if migrated.SchemaVersion != StoreSchemaVersion {
		t.Fatalf("SchemaVersion = %d, want %d", migrated.SchemaVersion, StoreSchemaVersion)
	}

	if len(migrated.Documents) != 1 || migrated.Documents[0].SourceHash == "" {
		t.Fatalf("migrated documents = %#v, want source hash", migrated.Documents)
	}
}

func TestLoadRefusesMissingOrStaleNormalizationUnlessMigrated(t *testing.T) {
	t.Parallel()

	store := Store{
		SchemaVersion: StoreSchemaVersion,
		Documents: []Document{{
			ID:         "note",
			Text:       "redacted searchable memory",
			SourceHash: privacy.SourceHash("redacted searchable memory"),
		}},
	}

	path := filepath.Join(t.TempDir(), "stale-normalization.json")

	data, err := json.Marshal(store)
	if err != nil {
		t.Fatalf("Marshal(missing normalization) error = %v", err)
	}

	if writeErr := os.WriteFile(path, data, 0o600); writeErr != nil {
		t.Fatalf("WriteFile(missing normalization) error = %v", writeErr)
	}

	_, err = Load(path)
	if !errors.Is(err, ErrNormalizationMismatch) {
		t.Fatalf("Load(missing normalization) error = %v, want ErrNormalizationMismatch", err)
	}

	store.Normalization = "legacy-tokenizer-v0"

	data, err = json.Marshal(store)
	if err != nil {
		t.Fatalf("Marshal(stale normalization) error = %v", err)
	}

	if writeErr := os.WriteFile(path, data, 0o600); writeErr != nil {
		t.Fatalf("WriteFile(stale normalization) error = %v", writeErr)
	}

	_, err = Load(path)
	if !errors.Is(err, ErrNormalizationMismatch) {
		t.Fatalf("Load(stale normalization) error = %v, want ErrNormalizationMismatch", err)
	}

	migrated, err := LoadWithOptions(path, LoadOptions{Migrate: true})
	if err != nil {
		t.Fatalf("LoadWithOptions(Migrate) error = %v", err)
	}

	if migrated.Normalization != StoreTextNormalization {
		t.Fatalf("Normalization = %q, want %q", migrated.Normalization, StoreTextNormalization)
	}
}

func TestStore_RefusesMissingSchemaOrNormalizationForExistingDocuments(t *testing.T) {
	t.Parallel()

	store := &Store{
		SchemaVersion: StoreSchemaVersion,
		Documents: []Document{{
			ID:         "note",
			Text:       "redacted searchable memory",
			SourceHash: privacy.SourceHash("redacted searchable memory"),
		}},
	}

	path := filepath.Join(t.TempDir(), "memory.json")
	if err := store.Save(path); !errors.Is(err, ErrNormalizationMismatch) {
		t.Fatalf("Save(missing normalization) error = %v, want ErrNormalizationMismatch", err)
	}

	if _, err := store.Search("searchable", 1); !errors.Is(err, ErrNormalizationMismatch) {
		t.Fatalf("Search(missing normalization) error = %v, want ErrNormalizationMismatch", err)
	}

	store.Normalization = StoreTextNormalization
	store.SchemaVersion = 0

	if err := store.Save(path); !errors.Is(err, ErrIncompatibleSchema) {
		t.Fatalf("Save(missing schema) error = %v, want ErrIncompatibleSchema", err)
	}

	if _, err := store.Search("searchable", 1); !errors.Is(err, ErrIncompatibleSchema) {
		t.Fatalf("Search(missing schema) error = %v, want ErrIncompatibleSchema", err)
	}

	if err := store.Migrate(); err != nil {
		t.Fatalf("Migrate() error = %v", err)
	}

	if err := store.Save(path); err != nil {
		t.Fatalf("Save(migrated) error = %v", err)
	}
}

func TestStore_MigrateCompactsExpiredDocuments(t *testing.T) {
	t.Parallel()

	expiredAt := time.Now().UTC().Add(-time.Second)
	store := &Store{
		Documents: []Document{
			{
				ID:        "expired",
				Text:      "expired secret api_key=abc123",
				ExpiresAt: &expiredAt,
			},
			{
				ID:   "active",
				Text: "active migration memory",
			},
		},
	}

	if err := store.Migrate(); err != nil {
		t.Fatalf("Migrate() error = %v", err)
	}

	if len(store.Documents) != 1 {
		t.Fatalf("documents after migrate = %#v, want only active document", store.Documents)
	}

	if store.Documents[0].ID != "active" {
		t.Fatalf("remaining document = %q, want active", store.Documents[0].ID)
	}

	if strings.Contains(store.Documents[0].Text, "abc123") {
		t.Fatalf("expired secret survived migrate: %#v", store.Documents)
	}
}

func TestStore_RejectsIncompatibleSchemaVersion(t *testing.T) {
	t.Parallel()

	store := NewStore()
	store.SchemaVersion = StoreSchemaVersion + 1

	if err := store.AddText("doc", "memory"); !errors.Is(err, ErrIncompatibleSchema) {
		t.Fatalf("AddText() error = %v, want ErrIncompatibleSchema", err)
	}

	if _, err := store.Search("memory", 1); !errors.Is(err, ErrIncompatibleSchema) {
		t.Fatalf("Search() error = %v, want ErrIncompatibleSchema", err)
	}

	path := filepath.Join(t.TempDir(), "memory.json")
	if err := store.Save(path); !errors.Is(err, ErrIncompatibleSchema) {
		t.Fatalf("Save() error = %v, want ErrIncompatibleSchema", err)
	}
}

func TestStore_MigrateRejectsMalformedSchemaVersion(t *testing.T) {
	t.Parallel()

	store := NewStore()
	store.SchemaVersion = -1

	if err := store.Migrate(); !errors.Is(err, ErrIncompatibleSchema) {
		t.Fatalf("Migrate() error = %v, want ErrIncompatibleSchema", err)
	}
}

func TestLoadRefusesSourceHashMismatch(t *testing.T) {
	t.Parallel()

	store := NewStore()
	if err := store.AddText("note", "original memory text"); err != nil {
		t.Fatalf("AddText() error = %v", err)
	}

	store.Documents[0].Text = "tampered memory text"

	path := filepath.Join(t.TempDir(), "memory.json")

	data, err := json.Marshal(store)
	if err != nil {
		t.Fatalf("Marshal() error = %v", err)
	}

	if writeErr := os.WriteFile(path, data, 0o600); writeErr != nil {
		t.Fatalf("WriteFile() error = %v", writeErr)
	}

	_, err = Load(path)
	if !errors.Is(err, ErrSourceHashMismatch) {
		t.Fatalf("Load() error = %v, want ErrSourceHashMismatch", err)
	}
}

func TestLoadRefusesMissingSourceHashUnlessMigrated(t *testing.T) {
	t.Parallel()

	store := NewStore()
	if err := store.AddText("note", "redacted memory text"); err != nil {
		t.Fatalf("AddText() error = %v", err)
	}

	store.Documents[0].SourceHash = ""

	path := filepath.Join(t.TempDir(), "missing-source-hash.json")

	data, err := json.Marshal(store)
	if err != nil {
		t.Fatalf("Marshal() error = %v", err)
	}

	if writeErr := os.WriteFile(path, data, 0o600); writeErr != nil {
		t.Fatalf("WriteFile() error = %v", writeErr)
	}

	_, err = Load(path)
	if !errors.Is(err, ErrSourceHashMismatch) {
		t.Fatalf("Load() error = %v, want ErrSourceHashMismatch", err)
	}

	migrated, err := LoadWithOptions(path, LoadOptions{Migrate: true})
	if err != nil {
		t.Fatalf("LoadWithOptions(Migrate) error = %v", err)
	}

	if len(migrated.Documents) != 1 {
		t.Fatalf("documents len = %d, want 1", len(migrated.Documents))
	}

	if migrated.Documents[0].SourceHash != privacy.SourceHash(migrated.Documents[0].Text) {
		t.Fatalf("SourceHash = %q, want recomputed hash", migrated.Documents[0].SourceHash)
	}
}

func TestStore_SaveRefusesMissingOrStaleSourceHash(t *testing.T) {
	t.Parallel()

	store := NewStore()
	if err := store.AddText("note", "trusted memory text"); err != nil {
		t.Fatalf("AddText() error = %v", err)
	}

	store.Documents[0].Text = "tampered memory text"

	path := filepath.Join(t.TempDir(), "memory.json")
	if err := store.Save(path); !errors.Is(err, ErrSourceHashMismatch) {
		t.Fatalf("Save(stale hash) error = %v, want ErrSourceHashMismatch", err)
	}

	if err := store.AddText("note", "trusted memory text"); err != nil {
		t.Fatalf("AddText(reset) error = %v", err)
	}

	store.Documents[0].SourceHash = ""
	if err := store.Save(path); !errors.Is(err, ErrSourceHashMismatch) {
		t.Fatalf("Save(missing hash) error = %v, want ErrSourceHashMismatch", err)
	}
}

func TestLoadAndSaveRefuseDuplicateIDs(t *testing.T) {
	t.Parallel()

	store := NewStore()
	if err := store.AddText(duplicateID, "first duplicate memory"); err != nil {
		t.Fatalf("AddText(same) error = %v", err)
	}

	if err := store.AddText("other", "second duplicate memory"); err != nil {
		t.Fatalf("AddText(other) error = %v", err)
	}

	store.Documents[1].ID = duplicateID

	path := filepath.Join(t.TempDir(), "memory.json")
	if err := store.Save(path); !errors.Is(err, ErrDuplicateID) {
		t.Fatalf("Save(duplicate) error = %v, want ErrDuplicateID", err)
	}

	data, err := json.Marshal(store)
	if err != nil {
		t.Fatalf("Marshal() error = %v", err)
	}

	if writeErr := os.WriteFile(path, data, 0o600); writeErr != nil {
		t.Fatalf("WriteFile() error = %v", writeErr)
	}

	_, err = Load(path)
	if !errors.Is(err, ErrDuplicateID) {
		t.Fatalf("Load(duplicate) error = %v, want ErrDuplicateID", err)
	}

	_, err = LoadWithOptions(path, LoadOptions{Migrate: true})
	if !errors.Is(err, ErrDuplicateID) {
		t.Fatalf("LoadWithOptions(Migrate duplicate) error = %v, want ErrDuplicateID", err)
	}
}

func TestStore_SearchRejectsMissingOrDuplicateIDs(t *testing.T) {
	t.Parallel()

	store := NewStore()
	if err := store.AddText("first", "first duplicate search memory"); err != nil {
		t.Fatalf("AddText(first) error = %v", err)
	}

	if err := store.AddText("second", "second duplicate search memory"); err != nil {
		t.Fatalf("AddText(second) error = %v", err)
	}

	store.Documents[0].ID = " "
	if _, err := store.Search("duplicate", 10); !errors.Is(err, ErrMissingID) {
		t.Fatalf("Search(missing id) error = %v, want ErrMissingID", err)
	}

	store.Documents[0].ID = duplicateID

	store.Documents[1].ID = duplicateID
	if _, err := store.Search("duplicate", 10); !errors.Is(err, ErrDuplicateID) {
		t.Fatalf("Search(duplicate id) error = %v, want ErrDuplicateID", err)
	}
}

func TestStore_SaveRefusesUnredactedBypassContent(t *testing.T) {
	t.Parallel()

	store := NewStore()
	store.Documents = []Document{{
		ID:         "secret",
		Text:       "deploy password=hunter2",
		SourceHash: privacy.SourceHash("deploy password=hunter2"),
	}}

	path := filepath.Join(t.TempDir(), "memory.json")
	if err := store.Save(path); !errors.Is(err, ErrPrivacyPolicy) {
		t.Fatalf("Save(unredacted) error = %v, want ErrPrivacyPolicy", err)
	}
}

func TestStore_SearchRefusesTamperedSensitiveContent(t *testing.T) {
	t.Parallel()

	store := NewStore()
	if err := store.AddText("safe", "safe searchable memory"); err != nil {
		t.Fatalf("AddText() error = %v", err)
	}

	store.Documents[0].Text = "safe searchable memory password=hunter2"
	store.Documents[0].SourceHash = privacy.SourceHash(store.Documents[0].Text)

	_, err := store.Search("safe", 1)
	if !errors.Is(err, ErrPrivacyPolicy) {
		t.Fatalf("Search() error = %v, want ErrPrivacyPolicy", err)
	}
}

func TestStore_SearchRefusesMissingSourceHash(t *testing.T) {
	t.Parallel()

	store := NewStore()
	if err := store.AddText("safe", "safe searchable memory"); err != nil {
		t.Fatalf("AddText() error = %v", err)
	}

	store.Documents[0].SourceHash = ""

	_, err := store.Search("safe", 1)
	if !errors.Is(err, ErrSourceHashMismatch) {
		t.Fatalf("Search() error = %v, want ErrSourceHashMismatch", err)
	}
}

func TestStore_SearchRejectsMissingProvenance(t *testing.T) {
	t.Parallel()

	store := NewStore()
	if err := store.AddText("safe", "safe searchable memory"); err != nil {
		t.Fatalf("AddText() error = %v", err)
	}

	store.Documents[0].Provenance = nil

	_, err := store.Search("safe", 1)
	if !errors.Is(err, ErrProvenanceMissing) {
		t.Fatalf("Search() error = %v, want ErrProvenanceMissing", err)
	}
}

func TestStore_SaveRejectsMissingProvenance(t *testing.T) {
	t.Parallel()

	store := NewStore()
	if err := store.AddText("safe", "safe persistent memory"); err != nil {
		t.Fatalf("AddText() error = %v", err)
	}

	store.Documents[0].Provenance = nil

	path := filepath.Join(t.TempDir(), "memory.json")
	if err := store.Save(path); !errors.Is(err, ErrProvenanceMissing) {
		t.Fatalf("Save() error = %v, want ErrProvenanceMissing", err)
	}
}

func TestStore_DeleteAndCompactRemovePersistedContent(t *testing.T) {
	t.Parallel()

	now := time.Now().UTC()
	expired := now.Add(-time.Second)

	store := NewStore()
	if err := store.Add(Document{ID: "expired", Text: "temporary memory", ExpiresAt: &expired}); err != nil {
		t.Fatalf("Add(expired) error = %v", err)
	}

	if err := store.AddText("keep", "durable memory"); err != nil {
		t.Fatalf("AddText(keep) error = %v", err)
	}

	results, err := store.Search("temporary", 1)
	if err != nil {
		t.Fatalf("Search() error = %v", err)
	}

	if len(results) != 0 {
		t.Fatalf("expired search results = %#v, want none", results)
	}

	if removed := store.Compact(now); removed != 1 {
		t.Fatalf("Compact() = %d, want 1", removed)
	}

	for _, doc := range store.Documents[:cap(store.Documents)] {
		if strings.Contains(doc.Text, "temporary memory") {
			t.Fatalf("expired content retained in memory backing array: %#v", doc)
		}
	}

	if !store.Delete("keep") {
		t.Fatal("Delete(keep) = false, want true")
	}

	for _, doc := range store.Documents[:cap(store.Documents)] {
		if doc.Text != "" {
			t.Fatalf("deleted content retained in memory backing array: %#v", doc)
		}
	}

	path := filepath.Join(t.TempDir(), "memory.json")
	if saveErr := store.Save(path); saveErr != nil {
		t.Fatalf("Save() error = %v", saveErr)
	}

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile() error = %v", err)
	}

	if strings.Contains(string(data), "temporary memory") || strings.Contains(string(data), "durable memory") {
		t.Fatalf("deleted/expired content still persisted: %s", data)
	}
}

func TestStore_DeleteRemovesOneDocumentAndPreservesSiblings(t *testing.T) {
	t.Parallel()

	store := NewStore()
	if err := store.AddText("delete-me", "obsolete local memory"); err != nil {
		t.Fatalf("AddText(delete-me) error = %v", err)
	}

	if err := store.AddText("keep-me", "durable local memory"); err != nil {
		t.Fatalf("AddText(keep-me) error = %v", err)
	}

	if !store.Delete("delete-me") {
		t.Fatal("Delete(delete-me) = false, want true")
	}

	if len(store.Documents) != 1 || store.Documents[0].ID != "keep-me" {
		t.Fatalf("documents after delete = %#v, want only keep-me", store.Documents)
	}

	results, err := store.Search("obsolete", 10)
	if err != nil {
		t.Fatalf("Search(obsolete) error = %v", err)
	}

	if len(results) != 0 {
		t.Fatalf("Search(obsolete) results = %#v, want none", results)
	}

	results, err = store.Search("durable", 10)
	if err != nil {
		t.Fatalf("Search(durable) error = %v", err)
	}

	if len(results) != 1 || results[0].Document.ID != "keep-me" {
		t.Fatalf("Search(durable) results = %#v, want keep-me", results)
	}

	path := filepath.Join(t.TempDir(), "memory.json")
	if saveErr := store.Save(path); saveErr != nil {
		t.Fatalf("Save() error = %v", saveErr)
	}

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile() error = %v", err)
	}

	if strings.Contains(string(data), "obsolete local memory") {
		t.Fatalf("deleted content persisted: %s", data)
	}

	if !strings.Contains(string(data), "durable local memory") {
		t.Fatalf("sibling content missing from persisted store: %s", data)
	}
}

func TestStore_DeleteRemovesAllDuplicateIDsBeforePersistence(t *testing.T) {
	t.Parallel()

	store := NewStore()
	if err := store.AddText(duplicateID, "first duplicate memory"); err != nil {
		t.Fatalf("AddText(same) error = %v", err)
	}

	if err := store.AddText("other", "second duplicate memory"); err != nil {
		t.Fatalf("AddText(other) error = %v", err)
	}

	store.Documents[1].ID = duplicateID

	if !store.Delete(duplicateID) {
		t.Fatal("Delete(same) = false, want true")
	}

	if len(store.Documents) != 0 {
		t.Fatalf("documents after duplicate delete = %#v, want empty", store.Documents)
	}

	path := filepath.Join(t.TempDir(), "memory.json")
	if err := store.Save(path); err != nil {
		t.Fatalf("Save() error = %v", err)
	}

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile() error = %v", err)
	}

	if strings.Contains(string(data), "first duplicate memory") || strings.Contains(string(data), "second duplicate memory") {
		t.Fatalf("deleted duplicate content still persisted: %s", data)
	}
}

func TestStore_DeleteClearsBackingCapacity(t *testing.T) {
	t.Parallel()

	store := NewStore()
	docs := []Document{
		{ID: "delete-me", Text: "visible deleted memory"},
		{ID: "ghost", Text: "hidden stale memory"},
	}
	store.Documents = docs[:1]

	if !store.Delete("delete-me") {
		t.Fatal("Delete(delete-me) = false, want true")
	}

	for _, doc := range store.Documents[:cap(store.Documents)] {
		if doc.Text != "" {
			t.Fatalf("deleted content retained in memory backing array: %#v", doc)
		}
	}
}

func TestStore_SaveCompactsExpiredContent(t *testing.T) {
	t.Parallel()

	expired := time.Now().UTC().Add(-time.Second)

	store := NewStore()
	if err := store.Add(Document{ID: "expired", Text: "temporary save-only memory", ExpiresAt: &expired}); err != nil {
		t.Fatalf("Add(expired) error = %v", err)
	}

	path := filepath.Join(t.TempDir(), "memory.json")
	if err := store.Save(path); err != nil {
		t.Fatalf("Save() error = %v", err)
	}

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile() error = %v", err)
	}

	if strings.Contains(string(data), "temporary save-only memory") {
		t.Fatalf("expired content still persisted: %s", data)
	}

	loaded, err := Load(path)
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}

	if len(loaded.Documents) != 0 {
		t.Fatalf("loaded documents len = %d, want expired content compacted", len(loaded.Documents))
	}
}

func TestStore_SaveCompactsExpiredLegacyContentBeforeSchemaValidation(t *testing.T) {
	t.Parallel()

	expired := time.Now().UTC().Add(-time.Second)
	store := Store{
		Normalization: "legacy-tokenizer-v0",
		Documents: []Document{{
			ID:        "expired",
			Text:      "expired legacy memory",
			ExpiresAt: &expired,
		}},
	}

	path := filepath.Join(t.TempDir(), "memory.json")
	if err := store.Save(path); err != nil {
		t.Fatalf("Save() error = %v", err)
	}

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile() error = %v", err)
	}

	if strings.Contains(string(data), "expired legacy memory") {
		t.Fatalf("expired legacy content still persisted: %s", data)
	}

	loaded, err := Load(path)
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}

	if loaded.SchemaVersion != StoreSchemaVersion || len(loaded.Documents) != 0 {
		t.Fatalf("loaded store = %#v, want current empty store", loaded)
	}

	if loaded.Normalization != StoreTextNormalization {
		t.Fatalf("Normalization = %q, want %q", loaded.Normalization, StoreTextNormalization)
	}
}

func TestLoadCompactsExpiredInvalidContentBeforeValidation(t *testing.T) {
	t.Parallel()

	expired := time.Now().UTC().Add(-time.Second)
	store := Store{
		SchemaVersion: StoreSchemaVersion,
		Normalization: "legacy-tokenizer-v0",
		Documents: []Document{{
			ID:        "expired",
			Text:      "expired invalid memory",
			ExpiresAt: &expired,
		}},
	}

	path := filepath.Join(t.TempDir(), "memory.json")

	data, err := json.Marshal(store)
	if err != nil {
		t.Fatalf("Marshal() error = %v", err)
	}

	if writeErr := os.WriteFile(path, data, 0o600); writeErr != nil {
		t.Fatalf("WriteFile() error = %v", writeErr)
	}

	loaded, err := Load(path)
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}

	if len(loaded.Documents) != 0 {
		t.Fatalf("loaded documents = %#v, want expired content compacted", loaded.Documents)
	}

	if loaded.Normalization != StoreTextNormalization {
		t.Fatalf("Normalization = %q, want %q", loaded.Normalization, StoreTextNormalization)
	}
}

func TestLoadCompactsExpiredLegacySchemaContentBeforeValidation(t *testing.T) {
	t.Parallel()

	expired := time.Now().UTC().Add(-time.Second)
	store := Store{
		Documents: []Document{{
			ID:        "expired",
			Text:      "expired legacy-schema memory",
			ExpiresAt: &expired,
		}},
	}

	path := filepath.Join(t.TempDir(), "memory.json")

	data, err := json.Marshal(store)
	if err != nil {
		t.Fatalf("Marshal() error = %v", err)
	}

	if writeErr := os.WriteFile(path, data, 0o600); writeErr != nil {
		t.Fatalf("WriteFile() error = %v", writeErr)
	}

	loaded, err := Load(path)
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}

	if loaded.SchemaVersion != StoreSchemaVersion || len(loaded.Documents) != 0 {
		t.Fatalf("loaded store = %#v, want current empty store", loaded)
	}
}

func TestLoadCompactsExpiredDuplicateBeforeValidation(t *testing.T) {
	t.Parallel()

	expired := time.Now().UTC().Add(-time.Second)
	store := NewStore()

	if err := store.AddText(duplicateID, "active duplicate memory"); err != nil {
		t.Fatalf("AddText() error = %v", err)
	}

	store.Documents = append(store.Documents, Document{
		ID:        duplicateID,
		Text:      "expired duplicate memory",
		ExpiresAt: &expired,
	})

	path := filepath.Join(t.TempDir(), "memory.json")

	data, err := json.Marshal(store)
	if err != nil {
		t.Fatalf("Marshal() error = %v", err)
	}

	if writeErr := os.WriteFile(path, data, 0o600); writeErr != nil {
		t.Fatalf("WriteFile() error = %v", writeErr)
	}

	loaded, err := Load(path)
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}

	if len(loaded.Documents) != 1 || loaded.Documents[0].Text != "active duplicate memory" {
		t.Fatalf("loaded documents = %#v, want only active duplicate", loaded.Documents)
	}
}

func TestStore_RetrievalQualityFixture(t *testing.T) {
	t.Parallel()

	data, err := os.ReadFile(filepath.Join("testdata", "retrieval_quality.json"))
	if err != nil {
		t.Fatalf("ReadFile(fixture) error = %v", err)
	}

	var fixture struct {
		Documents []struct {
			ID   string `json:"id"`
			Text string `json:"text"`
		} `json:"documents"`
		Cases []struct {
			Query   string `json:"query"`
			WantTop string `json:"want_top"`
		} `json:"cases"`
	}
	if err := json.Unmarshal(data, &fixture); err != nil {
		t.Fatalf("Unmarshal(fixture) error = %v", err)
	}

	store := NewStore()
	for _, doc := range fixture.Documents {
		if err := store.AddText(doc.ID, doc.Text); err != nil {
			t.Fatalf("AddText(%q) error = %v", doc.ID, err)
		}
	}

	for _, tc := range fixture.Cases {
		results, err := store.Search(tc.Query, 1)
		if err != nil {
			t.Fatalf("Search(%q) error = %v", tc.Query, err)
		}

		if len(results) != 1 || results[0].Document.ID != tc.WantTop {
			t.Fatalf("Search(%q) top = %#v, want %q", tc.Query, results, tc.WantTop)
		}
	}
}

func TestStore_SearchRetrievalAddsContractSafetyAndRange(t *testing.T) {
	t.Parallel()

	store := NewStore()
	require.NoError(t, store.Add(Document{ID: "secret", Path: ".env", Text: "api_key=super-secret-token oauth callback"}))

	results, err := store.SearchRetrieval(context.Background(), retrieval.Query{Text: "oauth callback", Limit: 1, Explain: true, IncludeUnsafe: true})
	require.NoError(t, err)
	require.Len(t, results, 1)

	result := results[0]
	assert.Equal(t, retrieval.SourceMemory, result.Source.Type)
	assert.Equal(t, "secret", result.DocumentID)
	assert.NotEmpty(t, result.Chunk.ID)
	assert.Equal(t, retrieval.RangeUnitRuneOffset, result.Chunk.Range.Unit)
	assert.False(t, result.Safety.InjectAllowed)
	assert.True(t, result.Safety.Sensitive)
	assert.True(t, result.Safety.Redacted)
	assert.NotContains(t, result.Snippet, "super-secret-token")
	assert.NotEmpty(t, result.Scorer.Explanation)
}

func TestLoad_NormalizesLegacyDocumentsBeforeRetrieval(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), "legacy-memory.json")
	require.NoError(t, os.WriteFile(path, []byte(`{
  "documents": [
    {"id": "legacy", "path": ".env", "text": "api_key=super-secret-token oauth callback", "metadata": {"api_key": "metadata-secret-token"}}
  ]
}`), 0o600))

	store, err := LoadWithOptions(path, LoadOptions{Migrate: true})
	require.NoError(t, err)
	require.Len(t, store.Documents, 1)
	assert.NotContains(t, store.Documents[0].Text, "super-secret-token")
	assert.Equal(t, "[REDACTED]", store.Documents[0].Metadata["api_key"])
	assert.NotContains(t, store.Documents[0].Metadata["api_key"], "metadata-secret-token")

	results, err := store.SearchRetrieval(context.Background(), retrieval.Query{
		Text:          "oauth callback",
		Limit:         1,
		IncludeUnsafe: true,
	})
	require.NoError(t, err)
	require.Len(t, results, 1)
	assert.False(t, results[0].Safety.InjectAllowed)
	assert.True(t, results[0].Safety.Redacted)
	assert.NotContains(t, results[0].Snippet, "super-secret-token")
	assert.NotContains(t, results[0].Metadata["api_key"], "metadata-secret-token")
	assert.Equal(t, "[REDACTED]", results[0].Metadata["api_key"])
}

func TestStore_SearchRetrievalReportsFileFreshness(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	path := filepath.Join(dir, "note.txt")
	require.NoError(t, os.WriteFile(path, []byte("oauth callback notes"), 0o600))

	store := NewStore()
	require.NoError(t, store.AddFile(path))

	results, err := store.SearchRetrieval(context.Background(), retrieval.Query{Text: "oauth callback", Limit: 1})
	require.NoError(t, err)
	require.Len(t, results, 1)
	assert.Equal(t, "current", results[0].Freshness.Status)
	assert.False(t, results[0].Freshness.Deleted)

	future := time.Now().Add(2 * time.Hour).UTC().Truncate(time.Second)
	require.NoError(t, os.Chtimes(path, future, future))

	results, err = store.SearchRetrieval(context.Background(), retrieval.Query{Text: "oauth callback", Limit: 1})
	require.NoError(t, err)
	require.Len(t, results, 1)
	assert.Equal(t, "stale", results[0].Freshness.Status)
	assert.False(t, results[0].Freshness.Deleted)

	require.NoError(t, os.Remove(path))

	results, err = store.SearchRetrieval(context.Background(), retrieval.Query{Text: "oauth callback", Limit: 1})
	require.NoError(t, err)
	require.Len(t, results, 1)
	assert.Equal(t, "deleted", results[0].Freshness.Status)
	assert.True(t, results[0].Freshness.Deleted)
}

func TestStore_SyncFilesDeletesRemovedFileDocuments(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	keep := filepath.Join(dir, "keep.txt")
	gone := filepath.Join(dir, "gone.txt")

	require.NoError(t, os.WriteFile(keep, []byte("keep oauth context"), 0o600))
	require.NoError(t, os.WriteFile(gone, []byte("gone oauth context"), 0o600))

	store := NewStore()
	require.NoError(t, store.SyncFiles(keep, gone))
	require.NoError(t, store.SyncFiles(keep))

	require.Len(t, store.Documents, 1)
	assert.Equal(t, filepath.Clean(keep), store.Documents[0].ID)
}
