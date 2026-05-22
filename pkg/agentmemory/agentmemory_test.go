package agentmemory

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/tommoulard/atteler/pkg/retrieval"
	"github.com/tommoulard/atteler/pkg/vector"
)

func TestStore_SearchIsScopedToAgent(t *testing.T) {
	t.Parallel()

	store, err := NewStore(32)
	require.NoError(t, err)

	require.NoError(t, store.AddText("alice", "alice-go", "Go context propagation and cancellation."))
	require.NoError(t, store.AddText("alice", "alice-rust", "Rust ownership and borrowing."))
	require.NoError(t, store.AddText("bob", "bob-go", "Go context propagation should not leak into Alice results."))

	results, err := store.Search("alice", "go context", 1)
	require.NoError(t, err)
	require.Len(t, results, 1)
	assert.Equal(t, "alice-go", results[0].Document.ID)
}

func TestStore_AddTextReplacesExistingAgentDocument(t *testing.T) {
	t.Parallel()

	store, err := NewStore(16)
	require.NoError(t, err)

	require.NoError(t, store.AddText("agent", "note", "first topic"))
	require.NoError(t, store.AddText("agent", "note", "second topic"))
	require.NoError(t, store.AddText("other", "note", "first topic"))

	docs := store.Documents("agent")
	require.Len(t, docs, 1)
	assert.Equal(t, "second topic", docs[0].Text)

	updatedResults, err := store.Search("agent", "second", 1)
	require.NoError(t, err)
	require.Len(t, updatedResults, 1)
	assert.Equal(t, "note", updatedResults[0].Document.ID)

	otherResults, err := store.Search("other", "first", 1)
	require.NoError(t, err)
	require.Len(t, otherResults, 1)
	assert.Equal(t, "note", otherResults[0].Document.ID)
}

func TestStore_AddFileRequiresUTF8AndUsesPath(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	path := filepath.Join(dir, "memory.txt")
	require.NoError(t, os.WriteFile(path, []byte("vector memory from file"), 0o600))

	store, err := NewStore(16)
	require.NoError(t, err)
	require.NoError(t, store.AddFile("agent", path))

	docs := store.Documents("agent")
	require.Len(t, docs, 1)
	assert.Equal(t, filepath.Clean(path), docs[0].ID)
	assert.Equal(t, filepath.Clean(path), docs[0].Path)
	assert.Equal(t, "vector memory from file", docs[0].Text)

	badPath := filepath.Join(dir, "bad.bin")
	require.NoError(t, os.WriteFile(badPath, []byte{0xff, 0xfe}, 0o600))
	err = store.AddFile("agent", badPath)
	assert.ErrorIs(t, err, ErrInvalidUTF8)
}

func TestStore_SaveLoadJSONRoundTrip(t *testing.T) {
	t.Parallel()

	store, err := NewStore(24)
	require.NoError(t, err)
	require.NoError(t, store.Add("alice", Document{
		ID:       "design",
		Text:     "Keep the vector memory package simple and persistent.",
		Metadata: map[string]string{"kind": "note"},
	}))
	require.NoError(t, store.AddText("bob", "unrelated", "Bob owns a separate memory namespace."))

	path := filepath.Join(t.TempDir(), "agent-memory.json")
	require.NoError(t, store.Save(path))

	loaded, err := Load(path)
	require.NoError(t, err)
	assert.Equal(t, store.Dimensions, loaded.Dimensions)

	results, err := loaded.Search("alice", "simple persistent", 1)
	require.NoError(t, err)
	require.Len(t, results, 1)
	assert.Equal(t, "design", results[0].Document.ID)
	assert.Equal(t, map[string]string{"kind": "note"}, results[0].Document.Metadata)
	assert.NotEmpty(t, results[0].Document.Vector)

	bobResults, err := loaded.Search("bob", "separate namespace", 1)
	require.NoError(t, err)
	require.Len(t, bobResults, 1)
	assert.Equal(t, "unrelated", bobResults[0].Document.ID)
}

func TestStore_ValidationErrors(t *testing.T) {
	t.Parallel()

	store, err := NewStore(16)
	require.NoError(t, err)

	require.ErrorIs(t, store.AddText("", "id", "text"), ErrMissingAgent)
	require.ErrorIs(t, store.AddText("agent", "", "text"), ErrMissingID)
	require.ErrorIs(t, store.AddText("agent", "id", string([]byte{0xff})), ErrInvalidUTF8)
	require.ErrorIs(t, store.AddText("agent", "empty", "!!!"), vector.ErrEmptyText)
	_, searchErr := store.Search("", "query", 0)
	require.ErrorIs(t, searchErr, ErrMissingAgent)
	_, searchErr = store.Search("agent", "!!!", 0)
	require.ErrorIs(t, searchErr, vector.ErrEmptyText)
}

func TestLoadValidatesPersistedDocuments(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), "bad.json")
	data := []byte(`{
  "dimensions": 2,
  "agents": {
    "agent": [
      {"id": "dup", "text": "alpha", "vector": [1, 0]},
      {"id": "dup", "text": "beta", "vector": [0, 1]}
    ]
  }
}`)
	require.NoError(t, os.WriteFile(path, data, 0o600))

	_, err := Load(path)
	assert.ErrorIs(t, err, ErrDuplicateID)
}

func TestStore_SearchRetrievalUsesPersistentIndexAndSafety(t *testing.T) {
	t.Parallel()

	store, err := NewStore(32)
	require.NoError(t, err)
	require.NoError(t, store.AddText("reviewer", "secret", "oauth callback api_key=super-secret-token"))

	results, err := store.SearchRetrieval(context.Background(), "reviewer", retrieval.Query{Text: "oauth callback", Limit: 1, IncludeUnsafe: true, Explain: true})
	require.NoError(t, err)
	require.Len(t, results, 1)
	assert.Equal(t, retrieval.SourceAgentMemory, results[0].Source.Type)
	assert.Equal(t, "reviewer", results[0].Source.Name)
	assert.Equal(t, "secret", results[0].DocumentID)
	assert.False(t, results[0].Safety.InjectAllowed)
	assert.True(t, results[0].Safety.Redacted)
	assert.NotContains(t, results[0].Snippet, "super-secret-token")
	assert.NotEmpty(t, results[0].Metadata[retrieval.MetadataStableID])
	assert.NotEmpty(t, results[0].Metadata[retrieval.MetadataContentHash])
	assert.NotEmpty(t, results[0].Scorer.Explanation)

	assert.True(t, store.Delete("reviewer", "secret"))
	results, err = store.SearchRetrieval(context.Background(), "reviewer", retrieval.Query{Text: "oauth callback", Limit: 1, IncludeUnsafe: true})
	require.NoError(t, err)
	assert.Empty(t, results)
}

func TestStore_SearchRetrievalReportsFileFreshness(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	path := filepath.Join(dir, "memory.txt")
	require.NoError(t, os.WriteFile(path, []byte("oauth callback reviewer context"), 0o600))

	store, err := NewStore(32)
	require.NoError(t, err)
	require.NoError(t, store.AddFile("reviewer", path))

	results, err := store.SearchRetrieval(context.Background(), "reviewer", retrieval.Query{Text: "oauth callback", Limit: 1})
	require.NoError(t, err)
	require.Len(t, results, 1)
	assert.Equal(t, "current", results[0].Freshness.Status)
	assert.False(t, results[0].Freshness.Deleted)

	future := time.Now().Add(2 * time.Hour).UTC().Truncate(time.Second)
	require.NoError(t, os.Chtimes(path, future, future))

	results, err = store.SearchRetrieval(context.Background(), "reviewer", retrieval.Query{Text: "oauth callback", Limit: 1})
	require.NoError(t, err)
	require.Len(t, results, 1)
	assert.Equal(t, "stale", results[0].Freshness.Status)
	assert.False(t, results[0].Freshness.Deleted)

	require.NoError(t, os.Remove(path))

	results, err = store.SearchRetrieval(context.Background(), "reviewer", retrieval.Query{Text: "oauth callback", Limit: 1})
	require.NoError(t, err)
	require.Len(t, results, 1)
	assert.Equal(t, "deleted", results[0].Freshness.Status)
	assert.True(t, results[0].Freshness.Deleted)
}

func TestLoad_NormalizesLegacyAgentMemoryBeforeRetrieval(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), "agent-memory.json")
	require.NoError(t, os.WriteFile(path, []byte(`{
  "dimensions": 4,
  "agents": {
    "reviewer": [
      {
        "id": "legacy-secret",
        "text": "oauth callback api_key=super-secret-token",
        "metadata": {"api_key": "metadata-secret-token"},
        "vector": [1, 0, 0, 0]
      }
    ]
  }
}`), 0o600))

	store, err := Load(path)
	require.NoError(t, err)

	docs := store.Documents("reviewer")
	require.Len(t, docs, 1)
	assert.NotContains(t, docs[0].Text, "super-secret-token")
	assert.Equal(t, "[REDACTED]", docs[0].Metadata["api_key"])
	assert.NotContains(t, docs[0].Metadata["api_key"], "metadata-secret-token")

	results, err := store.SearchRetrieval(context.Background(), "reviewer", retrieval.Query{
		Text:          "oauth callback",
		Limit:         1,
		IncludeUnsafe: true,
	})
	require.NoError(t, err)
	require.Len(t, results, 1)
	assert.False(t, results[0].Safety.InjectAllowed)
	assert.True(t, results[0].Safety.Redacted)
	assert.NotContains(t, results[0].Snippet, "super-secret-token")
	assert.Equal(t, "[REDACTED]", results[0].Metadata["api_key"])
	assert.NotContains(t, results[0].Metadata["api_key"], "metadata-secret-token")
}
