package memory

import (
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
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
