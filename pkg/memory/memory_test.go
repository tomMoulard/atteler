package memory

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/tommoulard/atteler/pkg/retrieval"
)

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

	results, err := loaded.Search("rag", 1)
	if err != nil {
		t.Fatalf("Search() error = %v", err)
	}

	if len(results) != 1 || results[0].Document.ID != "design" {
		t.Fatalf("loaded search results = %#v, want design", results)
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

	store, err := Load(path)
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
