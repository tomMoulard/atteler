package vector

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/tommoulard/atteler/pkg/memory"
	"github.com/tommoulard/atteler/pkg/privacy"
	"github.com/tommoulard/atteler/pkg/retrieval"
)

const (
	indexTestAuthText = "OAuth callback state validation"
	indexTestCommit   = "abc123"
)

func TestChunkText_StableChunkIDs(t *testing.T) {
	t.Parallel()

	text := "alpha beta gamma delta epsilon zeta eta theta iota kappa"
	chunks, err := ChunkText("docs/retrieval.md", text, ChunkOptions{MaxRunes: 18, OverlapRunes: 4})
	require.NoError(t, err)
	require.Greater(t, len(chunks), 1)

	again, err := ChunkText("docs/retrieval.md", text, ChunkOptions{MaxRunes: 18, OverlapRunes: 4})
	require.NoError(t, err)
	assert.Equal(t, chunks, again)

	assert.Equal(t, "docs/retrieval.md#chunk=0000", chunks[0].ID)
	assert.Equal(t, "docs/retrieval.md#chunk=0001", chunks[1].ID)
	assert.Greater(t, chunks[1].StartRune, chunks[0].StartRune)
	assert.LessOrEqual(t, chunks[0].EndRune-chunks[1].StartRune, 4)
}

func TestChunkOptionsNormalizeDefaults(t *testing.T) {
	t.Parallel()

	got := (ChunkOptions{}).Normalize()

	assert.Equal(t, DefaultChunkMaxRunes, got.MaxRunes)
	assert.Equal(t, DefaultChunkOverlapRunes, got.OverlapRunes)
}

func TestIndex_ValidateForMetadataMismatch(t *testing.T) {
	t.Parallel()

	vectorizer, err := NewTextVectorizer(8)
	require.NoError(t, err)

	source := Source{Path: "docs/vector.md", Text: "hashed token fallback search"}
	idx, err := BuildIndex(
		context.TODO(),
		[]Source{source},
		vectorizer,
		vectorizer.Metadata(),
		ChunkOptions{MaxRunes: 100},
		time.Unix(1, 0),
	)
	require.NoError(t, err)

	current := []SourceMetadata{SourceMetadataForText(source.Path, source.Text)}
	err = idx.ValidateFor(NewEmbeddingMetadata("ollama", "nomic-embed-text", "http://127.0.0.1:11434", 0), current)
	require.Error(t, err)
	assert.ErrorIs(t, err, ErrMetadataMismatch)
}

func TestIndex_PersistsLocalRAGSourcesWithInvalidationMetadata(t *testing.T) {
	t.Parallel()

	vectorizer, err := NewTextVectorizer(32)
	require.NoError(t, err)

	sources := []Source{
		{
			Kind: SourceKindFile,
			Path: "docs/retrieval.md",
			Text: "Workspace files chunk into persistent local RAG indexes.",
			Metadata: map[string]string{
				"language": "markdown",
			},
			Provenance: map[string]string{
				"root": "workspace",
			},
		},
		{
			Kind: SourceKindSession,
			Path: "sessions/session-123/messages/0",
			Text: "Planner session selected embedding-backed memory for local RAG.",
			Metadata: map[string]string{
				"session_id": "session-123",
				"agent":      "planner",
			},
			Provenance: map[string]string{
				"session_id": "session-123",
			},
		},
		{
			Kind: SourceKindGitHistory,
			Path: "git/" + indexTestCommit,
			Text: indexTestCommit + " Make vector indexes reuse source digests for invalidation.",
			Metadata: map[string]string{
				"commit": indexTestCommit,
			},
			Provenance: map[string]string{
				"commit": indexTestCommit,
			},
		},
		{
			Kind: SourceKindADR,
			Path: "docs/adr/0001-local-rag.md",
			Text: "ADR 0001 records the decision to keep retrieval indexes local.",
			Metadata: map[string]string{
				"adr_id": "0001",
			},
			Provenance: map[string]string{
				"adr_id": "0001",
			},
		},
	}

	idx, err := BuildIndex(
		context.TODO(),
		sources,
		vectorizer,
		vectorizer.Metadata(),
		ChunkOptions{MaxRunes: 400, OverlapRunes: 40},
		time.Unix(5, 0),
	)
	require.NoError(t, err)
	require.Len(t, idx.Sources, len(sources))

	path := filepath.Join(t.TempDir(), "local-rag-index.json")
	require.NoError(t, idx.Save(path))

	loaded, err := LoadIndex(path)
	require.NoError(t, err)
	assert.ElementsMatch(t,
		[]string{SourceKindFile, SourceKindSession, SourceKindGitHistory, SourceKindADR},
		sourceMetadataKinds(loaded.Sources),
	)
	assert.Contains(t, documentMetadataSourceKinds(loaded.Documents), SourceKindFile)
	assert.Contains(t, documentMetadataSourceKinds(loaded.Documents), SourceKindSession)
	assert.Contains(t, documentMetadataSourceKinds(loaded.Documents), SourceKindGitHistory)
	assert.Contains(t, documentMetadataSourceKinds(loaded.Documents), SourceKindADR)
	assert.Contains(t, documentProvenanceTypes(loaded.Documents), SourceKindFile)
	assert.Contains(t, documentProvenanceTypes(loaded.Documents), SourceKindSession)
	assert.Contains(t, documentProvenanceTypes(loaded.Documents), SourceKindGitHistory)
	assert.Contains(t, documentProvenanceTypes(loaded.Documents), SourceKindADR)

	for i := range loaded.Documents {
		assert.Equal(t, time.Unix(5, 0).UTC(), loaded.Documents[i].CreatedAt)
		assert.Equal(t, time.Unix(5, 0).UTC(), loaded.Documents[i].UpdatedAt)
	}

	current := sourceMetadataForSources(sources)
	require.NoError(t, loaded.ValidateFor(vectorizer.Metadata(), current, ChunkOptions{MaxRunes: 400, OverlapRunes: 40}))

	stale := append([]SourceMetadata(nil), current...)
	stale[1] = SourceMetadataForTextWithKind(sources[1].Path, "changed session text", sources[1].Kind)
	err = loaded.ValidateFor(vectorizer.Metadata(), stale, ChunkOptions{MaxRunes: 400, OverlapRunes: 40})
	require.Error(t, err)
	require.ErrorIs(t, err, ErrSourceStale)

	wrongKind := append([]SourceMetadata(nil), current...)
	wrongKind[2] = SourceMetadataForTextWithKind(sources[2].Path, sources[2].Text, SourceKindSession)
	err = loaded.ValidateFor(vectorizer.Metadata(), wrongKind, ChunkOptions{MaxRunes: 400, OverlapRunes: 40})
	require.Error(t, err)
	require.ErrorIs(t, err, ErrSourceStale)

	deleted := current[:len(current)-1]
	err = loaded.ValidateFor(vectorizer.Metadata(), deleted, ChunkOptions{MaxRunes: 400, OverlapRunes: 40})
	require.Error(t, err)
	require.ErrorIs(t, err, ErrSourceStale)
}

func TestRefreshSourceIndex_PersistsSessionAndGitHistoryIncrementally(t *testing.T) {
	t.Parallel()

	textVectorizer, err := NewTextVectorizer(32)
	require.NoError(t, err)

	counting := &countingVectorizer{inner: textVectorizer}
	indexPath := filepath.Join(t.TempDir(), "source-index.json")
	sources := []Source{
		{
			Kind: SourceKindSession,
			Path: "sessions/session-123",
			Text: "Planner session chose embedding-backed local RAG memory.",
			Metadata: map[string]string{
				"session_id": "session-123",
				"agent":      "planner",
			},
			Provenance: map[string]string{
				"session_id": "session-123",
			},
		},
		{
			Kind: SourceKindGitHistory,
			Path: "git/abc123",
			Text: "abc123 Persist git history source digests for local RAG.",
			Metadata: map[string]string{
				"commit": "abc123",
			},
			Provenance: map[string]string{
				"commit": "abc123",
			},
		},
	}
	opts := SourceIndexOptions{
		IndexPath:          indexPath,
		Sources:            sources,
		Vectorizer:         counting,
		VectorizerMetadata: textVectorizer.Metadata(),
		Chunk:              ChunkOptions{MaxRunes: 400, OverlapRunes: 40},
	}
	now := time.Unix(10, 0).UTC()
	opts.Now = func() time.Time { return now }

	first, err := RefreshSourceIndex(context.TODO(), opts)
	require.NoError(t, err)
	require.NotNil(t, first.Index)
	assert.True(t, first.Initialized)
	assert.True(t, first.Refreshed)
	assert.Equal(t, 2, first.Added)
	assert.Equal(t, 2, counting.calls)
	assert.ElementsMatch(t, []string{SourceKindSession, SourceKindGitHistory}, sourceMetadataKinds(first.Index.Sources))
	assert.Equal(t, time.Unix(10, 0).UTC(), first.Index.CreatedAt)
	assert.Equal(t, time.Unix(10, 0).UTC(), first.Index.UpdatedAt)

	counting.calls = 0
	now = time.Unix(20, 0).UTC()
	opts.Sources = []Source{
		{
			Kind: SourceKindSession,
			Path: "sessions/session-123",
			Text: "Planner session changed to prefer lexical fallback during outage.",
			Metadata: map[string]string{
				"session_id": "session-123",
				"agent":      "planner",
			},
			Provenance: map[string]string{
				"session_id": "session-123",
			},
		},
	}

	second, err := RefreshSourceIndex(context.TODO(), opts)
	require.NoError(t, err)
	require.NotNil(t, second.Index)
	assert.False(t, second.Initialized)
	assert.True(t, second.Refreshed)
	assert.Equal(t, 1, second.Updated)
	assert.Equal(t, 1, second.Deleted)
	assert.Equal(t, 1, counting.calls, "incremental refresh should vectorize only the changed session source")
	assert.ElementsMatch(t, []string{SourceKindSession}, sourceMetadataKinds(second.Index.Sources))
	assert.NotContains(t, sourceMetadataPaths(second.Index.Sources), "git/abc123")
	assert.Equal(t, time.Unix(10, 0).UTC(), second.Index.CreatedAt)
	assert.Equal(t, time.Unix(20, 0).UTC(), second.Index.UpdatedAt)

	counting.calls = 0
	now = time.Unix(30, 0).UTC()

	third, err := RefreshSourceIndex(context.TODO(), opts)
	require.NoError(t, err)
	require.NotNil(t, third.Index)
	assert.False(t, third.Refreshed)
	assert.Equal(t, 1, third.Unchanged)
	assert.Equal(t, 0, counting.calls)
	assert.Equal(t, time.Unix(10, 0).UTC(), third.Index.CreatedAt)
	assert.Equal(t, time.Unix(20, 0).UTC(), third.Index.UpdatedAt)

	loaded, err := LoadIndex(indexPath)
	require.NoError(t, err)
	require.NoError(t, loaded.ValidateFor(textVectorizer.Metadata(), sourceMetadataForSources(opts.Sources), opts.Chunk))
	assert.Contains(t, documentMetadataSourceKinds(loaded.Documents), SourceKindSession)
	assert.NotContains(t, documentMetadataSourceKinds(loaded.Documents), SourceKindGitHistory)
	assert.Equal(t, time.Unix(10, 0).UTC(), loaded.CreatedAt)
	assert.Equal(t, time.Unix(20, 0).UTC(), loaded.UpdatedAt)
}

func TestRefreshSourceIndex_UpdatesFreshnessMetadataWithoutRevectorizing(t *testing.T) {
	t.Parallel()

	textVectorizer, err := NewTextVectorizer(32)
	require.NoError(t, err)

	counting := &countingVectorizer{inner: textVectorizer}
	indexPath := filepath.Join(t.TempDir(), "source-index.json")
	firstSourceUpdatedAt := time.Date(2026, 6, 1, 10, 30, 0, 0, time.UTC)
	secondSourceUpdatedAt := firstSourceUpdatedAt.Add(time.Hour)
	source := Source{
		Kind: SourceKindSession,
		Path: "sessions/session-123",
		Text: "Planner session keeps the same text while saved metadata changes.",
		Metadata: map[string]string{
			"session_id":                      "session-123",
			retrieval.MetadataSourceUpdatedAt: firstSourceUpdatedAt.Format(time.RFC3339Nano),
		},
		Provenance: map[string]string{"session_id": "session-123"},
	}

	now := time.Unix(10, 0).UTC()
	opts := SourceIndexOptions{
		IndexPath:          indexPath,
		Sources:            []Source{source},
		Vectorizer:         counting,
		VectorizerMetadata: textVectorizer.Metadata(),
		Chunk:              ChunkOptions{MaxRunes: 400, OverlapRunes: 40},
		Now:                func() time.Time { return now },
	}

	first, err := RefreshSourceIndex(context.TODO(), opts)
	require.NoError(t, err)
	require.NotNil(t, first.Index)
	assert.Equal(t, 1, counting.calls)
	require.Len(t, first.Index.Documents, 1)
	assert.Equal(t, firstSourceUpdatedAt.Format(time.RFC3339Nano),
		first.Index.Documents[0].Metadata[retrieval.MetadataSourceUpdatedAt])

	source.Metadata[retrieval.MetadataSourceUpdatedAt] = secondSourceUpdatedAt.Format(time.RFC3339Nano)
	opts.Sources = []Source{source}
	counting.calls = 0
	now = time.Unix(20, 0).UTC()

	second, err := RefreshSourceIndex(context.TODO(), opts)
	require.NoError(t, err)
	require.NotNil(t, second.Index)
	assert.True(t, second.Refreshed)
	assert.Equal(t, 1, second.Updated)
	assert.Equal(t, 0, second.Unchanged)
	assert.Equal(t, 0, counting.calls, "freshness-only metadata updates should not re-vectorize unchanged text")
	require.Len(t, second.Index.Documents, 1)
	assert.Equal(t, time.Unix(10, 0).UTC(), second.Index.Documents[0].UpdatedAt)
	assert.Equal(t, secondSourceUpdatedAt.Format(time.RFC3339Nano),
		second.Index.Documents[0].Metadata[retrieval.MetadataSourceUpdatedAt])
	assert.Equal(t, time.Unix(20, 0).UTC(), second.Index.UpdatedAt)
}

func TestRefreshSourceIndex_InvalidatesRemovedMetadataAndChangedProvenance(t *testing.T) {
	t.Parallel()

	textVectorizer, err := NewTextVectorizer(32)
	require.NoError(t, err)

	counting := &countingVectorizer{inner: textVectorizer}
	indexPath := filepath.Join(t.TempDir(), "source-index.json")
	opts := SourceIndexOptions{
		IndexPath: indexPath,
		Sources: []Source{
			{
				Kind: SourceKindSession,
				Path: "sessions/session-123",
				Text: "Stable session text about local RAG metadata invalidation.",
				Metadata: map[string]string{
					"session_id":     "session-123",
					"stale_metadata": "remove-me",
				},
				Provenance: map[string]string{
					"default_model":    "gpt-old",
					"stale_provenance": "remove-me",
				},
			},
		},
		Vectorizer:         counting,
		VectorizerMetadata: textVectorizer.Metadata(),
		Chunk:              ChunkOptions{MaxRunes: 400, OverlapRunes: 40},
	}

	first, err := RefreshSourceIndex(context.TODO(), opts)
	require.NoError(t, err)
	require.NotNil(t, first.Index)
	assert.Equal(t, 1, first.Added)
	assert.Equal(t, 1, counting.calls)

	counting.calls = 0
	opts.Sources = []Source{
		{
			Kind: SourceKindSession,
			Path: "sessions/session-123",
			Text: "Stable session text about local RAG metadata invalidation.",
			Metadata: map[string]string{
				"session_id": "session-123",
			},
			Provenance: map[string]string{
				"default_model": "gpt-new",
			},
		},
	}

	second, err := RefreshSourceIndex(context.TODO(), opts)
	require.NoError(t, err)
	require.NotNil(t, second.Index)
	assert.True(t, second.Refreshed)
	assert.Equal(t, 1, second.Updated)
	assert.Equal(t, 0, second.Unchanged)
	assert.Equal(t, 1, counting.calls, "metadata/provenance invalidation should re-vectorize only the changed source")

	loaded, err := LoadIndex(indexPath)
	require.NoError(t, err)
	require.Len(t, loaded.Documents, 1)
	assert.Equal(t, "session-123", loaded.Documents[0].Metadata["session_id"])
	assert.NotContains(t, loaded.Documents[0].Metadata, "stale_metadata")
	assert.Equal(t, "gpt-new", loaded.Documents[0].Provenance["default_model"])
	assert.NotContains(t, loaded.Documents[0].Provenance, "stale_provenance")
}

func TestRefreshSourceIndex_RefusesToOverwriteExistingNonIndexPath(t *testing.T) {
	t.Parallel()

	vectorizer, err := NewTextVectorizer(16)
	require.NoError(t, err)

	indexPath := filepath.Join(t.TempDir(), "source-index.json")
	original := []byte("not a vector index")
	require.NoError(t, os.WriteFile(indexPath, original, 0o600))

	_, err = RefreshSourceIndex(context.TODO(), SourceIndexOptions{
		IndexPath:          indexPath,
		Sources:            []Source{{Kind: SourceKindSession, Path: "sessions/session-123", Text: "Session memory source"}},
		Vectorizer:         vectorizer,
		VectorizerMetadata: vectorizer.Metadata(),
		Chunk:              ChunkOptions{MaxRunes: 200, OverlapRunes: 20},
	})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "refusing to overwrite existing non-index file")

	data, readErr := os.ReadFile(indexPath)
	require.NoError(t, readErr)
	assert.Equal(t, original, data)
}

func TestRefreshSourceIndex_RemovesIndexWhenAllSourcesDeleted(t *testing.T) {
	t.Parallel()

	vectorizer, err := NewTextVectorizer(16)
	require.NoError(t, err)

	indexPath := filepath.Join(t.TempDir(), "source-index.json")
	opts := SourceIndexOptions{
		IndexPath:          indexPath,
		Sources:            []Source{{Kind: SourceKindSession, Path: "sessions/session-123", Text: "Session memory source"}},
		Vectorizer:         vectorizer,
		VectorizerMetadata: vectorizer.Metadata(),
		Chunk:              ChunkOptions{MaxRunes: 200, OverlapRunes: 20},
	}

	_, err = RefreshSourceIndex(context.TODO(), opts)
	require.NoError(t, err)
	require.FileExists(t, indexPath)

	opts.Sources = nil
	refresh, err := RefreshSourceIndex(context.TODO(), opts)
	require.ErrorIs(t, err, ErrNoSources)
	assert.Equal(t, 1, refresh.Deleted)
	assert.True(t, refresh.Refreshed)
	assert.NoFileExists(t, indexPath)
}

func TestRefreshSourceIndex_RemovesStaleManagedIndexWhenAllSourcesDeleted(t *testing.T) {
	t.Parallel()

	indexPath := filepath.Join(t.TempDir(), "source-index.json")
	staleManagedIndex := `{
  "version": 1,
  "created_at": "2026-01-01T00:00:00Z",
  "vectorizer": {"kind": "lexical", "dimensions": 2},
  "chunk": {"max_runes": 200, "overlap_runes": 20},
  "dimensions": 2,
  "sources": [
    {"path": "sessions/deleted", "digest": "not-a-valid-digest", "kind": "session", "bytes": 7}
  ],
  "documents": []
}`
	require.NoError(t, os.WriteFile(indexPath, []byte(staleManagedIndex), 0o600))

	refresh, err := RefreshSourceIndex(context.TODO(), SourceIndexOptions{
		IndexPath: indexPath,
		Sources:   nil,
	})
	require.ErrorIs(t, err, ErrNoSources)
	assert.True(t, refresh.Refreshed)
	assert.NoFileExists(t, indexPath)
}

func TestRefreshSourceIndex_RejectsDuplicateRedactedSourcePaths(t *testing.T) {
	t.Parallel()

	vectorizer, err := NewTextVectorizer(16)
	require.NoError(t, err)

	rawSecret := "secret-" + "token"
	_, err = RefreshSourceIndex(context.TODO(), SourceIndexOptions{
		IndexPath:          filepath.Join(t.TempDir(), "source-index.json"),
		Vectorizer:         vectorizer,
		VectorizerMetadata: vectorizer.Metadata(),
		Sources: []Source{
			{Kind: SourceKindSession, Path: "sessions/api_key=" + rawSecret + "/notes.md", Text: "first session note"},
			{Kind: SourceKindSession, Path: "sessions/api_key=other-" + rawSecret + "/notes.md", Text: "second session note"},
		},
	})
	require.Error(t, err)
	require.ErrorIs(t, err, ErrSourceStale)
	assert.Contains(t, err.Error(), "duplicate source path")
	assert.Contains(t, err.Error(), "[REDACTED]")
	assert.NotContains(t, err.Error(), rawSecret)
}

func TestBuildIndex_RejectsDuplicateRedactedSourcePathsBeforeVectorizing(t *testing.T) {
	t.Parallel()

	vectorizer := &recordingVectorizer{vector: Vector{1, 0}}
	rawSecret := "secret-" + "token"

	_, err := BuildIndex(
		context.TODO(),
		[]Source{
			{Kind: SourceKindFile, Path: "docs/api_key=" + rawSecret + "/auth.md", Text: "first auth note"},
			{Kind: SourceKindFile, Path: "docs/api_key=other-" + rawSecret + "/auth.md", Text: "second auth note"},
		},
		vectorizer,
		NewLexicalMetadata(2),
		ChunkOptions{MaxRunes: 100},
		time.Unix(1, 0),
	)
	require.Error(t, err)
	require.ErrorIs(t, err, ErrSourceStale)
	assert.Contains(t, err.Error(), "duplicate source path")
	assert.Contains(t, err.Error(), "[REDACTED]")
	assert.NotContains(t, err.Error(), rawSecret)
	assert.Empty(t, vectorizer.texts)
}

func TestRefreshSourceIndexAsync_IncrementallyHandlesChangedAndDeletedSources(t *testing.T) {
	t.Parallel()

	textVectorizer, err := NewTextVectorizer(32)
	require.NoError(t, err)

	counting := &countingVectorizer{inner: textVectorizer}
	indexPath := filepath.Join(t.TempDir(), "async-source-index.json")
	opts := SourceIndexOptions{
		IndexPath: indexPath,
		Sources: []Source{
			{
				Kind: SourceKindSession,
				Path: "sessions/session-123",
				Text: "Session source records the embedding memory plan.",
			},
			{
				Kind: SourceKindGitHistory,
				Path: "git/abc123",
				Text: "Commit source records persisted local RAG indexes.",
			},
		},
		Vectorizer:         counting,
		VectorizerMetadata: textVectorizer.Metadata(),
		Chunk:              ChunkOptions{MaxRunes: 400, OverlapRunes: 40},
	}

	first := <-RefreshSourceIndexAsync(context.TODO(), opts)
	require.NoError(t, first.Err)
	require.NotNil(t, first.Refresh.Index)
	assert.Equal(t, 2, first.Refresh.Added)

	counting.calls = 0
	opts.Sources = []Source{
		{
			Kind: SourceKindSession,
			Path: "sessions/session-123",
			Text: "Session source changed after the local RAG plan update.",
		},
	}

	second := <-RefreshSourceIndexAsync(context.TODO(), opts)
	require.NoError(t, second.Err)
	require.NotNil(t, second.Refresh.Index)
	assert.True(t, second.Refresh.Refreshed)
	assert.Equal(t, 1, second.Refresh.Updated)
	assert.Equal(t, 1, second.Refresh.Deleted)
	assert.Equal(t, 0, second.Refresh.Added)
	assert.Equal(t, 1, counting.calls, "async source refresh should vectorize only the changed source")

	loaded, err := LoadIndex(indexPath)
	require.NoError(t, err)
	assert.ElementsMatch(t, []string{"sessions/session-123"}, sourceMetadataPaths(loaded.Sources))
	assert.ElementsMatch(t, []string{SourceKindSession}, sourceMetadataKinds(loaded.Sources))
}

func TestVectorizerMetadata_CompatibleWithProviderAlias(t *testing.T) {
	t.Parallel()

	actual := NewEmbeddingMetadata("ollama-compatible", "nomic-embed-text", "http://127.0.0.1:11434/", 3)
	expected := NewEmbeddingMetadata("ollama", "nomic-embed-text", "http://127.0.0.1:11434", 0)

	require.NoError(t, actual.CompatibleWith(expected))
	assert.Equal(t, "ollama", actual.Provider)
	assert.Equal(t, "http://127.0.0.1:11434", actual.BaseURL)
}

func TestVectorizerMetadata_RedactsPrivateEmbeddingEndpoint(t *testing.T) {
	t.Parallel()

	meta := NewEmbeddingMetadata("ollama", "nomic-embed-text", privateEmbeddingEndpoint()+"&safe=1", 3)

	assert.Equal(t, "https://%5BREDACTED%5D:%5BREDACTED%5D@example.com/embed?api_key=[REDACTED]&safe=1", meta.BaseURL)
	assert.NotContains(t, meta.BaseURL, "user")
	assert.NotContains(t, meta.BaseURL, "pass")
	assert.NotContains(t, meta.BaseURL, "secret-token")
}

func TestVectorizerMetadata_RedactsMalformedEmbeddingEndpointCredentials(t *testing.T) {
	t.Parallel()

	meta := NewEmbeddingMetadata("ollama", "nomic-embed-text", malformedPrivateEmbeddingEndpoint(), 3)

	assert.Contains(t, meta.BaseURL, "[REDACTED]")
	assert.NotContains(t, meta.BaseURL, "user")
	assert.NotContains(t, meta.BaseURL, "pass")
	assert.NotContains(t, meta.BaseURL, "secret-token")
	require.NoError(t, validateVectorizerMetadataPrivacy(meta))
}

func TestIndex_ValidateRejectsPrivateVectorizerMetadata(t *testing.T) {
	t.Parallel()

	text := indexTestAuthText
	idx := &Index{
		Version: IndexVersion,
		Vectorizer: VectorizerMetadata{
			Kind:     VectorizerKindEmbedding,
			Provider: "ollama",
			Model:    "nomic-embed-text",
			BaseURL:  privateEmbeddingEndpoint(),
		},
		Dimensions: 2,
		Chunk:      ChunkOptions{MaxRunes: 100, OverlapRunes: 10},
		Sources:    []SourceMetadata{SourceMetadataForText("docs/auth.md", text)},
		Documents: []Document{{
			ID:         "docs/auth.md#chunk=0000",
			Text:       text,
			SourceHash: sourceHash(text),
			Vector:     Vector{1, 0},
			Metadata:   map[string]string{"path": "docs/auth.md"},
			Provenance: ensureProvenance(map[string]string{"source_type": "file"}, "file"),
		}},
	}

	err := idx.Validate()
	require.Error(t, err)
	assert.ErrorIs(t, err, ErrPrivacyPolicy)
}

func TestIndex_ValidateRejectsMalformedPrivateVectorizerMetadata(t *testing.T) {
	t.Parallel()

	text := indexTestAuthText
	idx := &Index{
		Version: IndexVersion,
		Vectorizer: VectorizerMetadata{
			Kind:     VectorizerKindEmbedding,
			Provider: "ollama",
			Model:    "nomic-embed-text",
			BaseURL:  malformedPrivateEmbeddingEndpoint(),
		},
		Dimensions: 2,
		Chunk:      ChunkOptions{MaxRunes: 100, OverlapRunes: 10},
		Sources:    []SourceMetadata{SourceMetadataForText("docs/auth.md", text)},
		Documents: []Document{{
			ID:         "docs/auth.md#chunk=0000",
			Text:       text,
			SourceHash: sourceHash(text),
			Vector:     Vector{1, 0},
			Metadata:   map[string]string{"path": "docs/auth.md"},
			Provenance: ensureProvenance(map[string]string{"source_type": "file"}, "file"),
		}},
	}

	err := idx.Validate()
	require.Error(t, err)
	assert.ErrorIs(t, err, ErrPrivacyPolicy)
}

func TestIndex_ValidateRejectsPrivateSourceMetadataPath(t *testing.T) {
	t.Parallel()

	text := indexTestAuthText
	rawSecret := "secret-" + "token"
	idx := &Index{
		Version:    IndexVersion,
		Vectorizer: NewLexicalMetadata(2),
		Dimensions: 2,
		Chunk:      ChunkOptions{MaxRunes: 100, OverlapRunes: 10},
		Sources: []SourceMetadata{{
			Path:   "docs/api_key=" + rawSecret + "/auth.md",
			Digest: DigestText(text),
			Bytes:  len([]byte(text)),
		}},
		Documents: []Document{{
			ID:         "docs/auth.md#chunk=0000",
			Text:       text,
			SourceHash: sourceHash(text),
			Vector:     Vector{1, 0},
			Metadata:   map[string]string{"path": "docs/auth.md"},
			Provenance: ensureProvenance(map[string]string{"source_type": "file"}, "file"),
		}},
	}

	err := idx.Validate()
	require.Error(t, err)
	require.ErrorIs(t, err, ErrPrivacyPolicy)
	assert.NotContains(t, err.Error(), rawSecret)
}

func TestIndex_ValidateRejectsDuplicateSourceMetadataPath(t *testing.T) {
	t.Parallel()

	text := indexTestAuthText
	idx := &Index{
		Version:    IndexVersion,
		Vectorizer: NewLexicalMetadata(2),
		Dimensions: 2,
		Chunk:      ChunkOptions{MaxRunes: 100, OverlapRunes: 10},
		Sources: []SourceMetadata{
			{Path: "docs/auth.md", Digest: DigestText(text), Bytes: len([]byte(text))},
			{Path: "docs/auth.md", Digest: DigestText(text), Bytes: len([]byte(text))},
		},
		Documents: []Document{{
			ID:         "docs/auth.md#chunk=0000",
			Text:       text,
			SourceHash: sourceHash(text),
			Vector:     Vector{1, 0},
			Metadata:   map[string]string{"path": "docs/auth.md"},
			Provenance: ensureProvenance(map[string]string{"source_type": "file"}, "file"),
		}},
	}

	err := idx.Validate()
	require.Error(t, err)
	require.ErrorIs(t, err, ErrSourceStale)
}

func TestIndex_ValidateRejectsInvalidSourceMetadataDigest(t *testing.T) {
	t.Parallel()

	text := indexTestAuthText
	idx := &Index{
		Version:    IndexVersion,
		Vectorizer: NewLexicalMetadata(2),
		Dimensions: 2,
		Chunk:      ChunkOptions{MaxRunes: 100, OverlapRunes: 10},
		Sources: []SourceMetadata{{
			Path:   "docs/auth.md",
			Digest: "not-a-digest",
			Bytes:  len([]byte(text)),
		}},
		Documents: []Document{{
			ID:         "docs/auth.md#chunk=0000",
			Text:       text,
			SourceHash: sourceHash(text),
			Vector:     Vector{1, 0},
			Metadata:   map[string]string{"path": "docs/auth.md"},
			Provenance: ensureProvenance(map[string]string{"source_type": "file"}, "file"),
		}},
	}

	err := idx.Validate()
	require.Error(t, err)
	require.ErrorIs(t, err, ErrSourceStale)
}

func TestIndex_ValidateRejectsUncleanSourceMetadataPath(t *testing.T) {
	t.Parallel()

	text := indexTestAuthText
	idx := &Index{
		Version:    IndexVersion,
		Vectorizer: NewLexicalMetadata(2),
		Dimensions: 2,
		Chunk:      ChunkOptions{MaxRunes: 100, OverlapRunes: 10},
		Sources: []SourceMetadata{{
			Path:   "docs/../auth.md",
			Digest: DigestText(text),
			Bytes:  len([]byte(text)),
		}},
		Documents: []Document{{
			ID:         "docs/auth.md#chunk=0000",
			Text:       text,
			SourceHash: sourceHash(text),
			Vector:     Vector{1, 0},
			Metadata:   map[string]string{"path": "docs/auth.md"},
			Provenance: ensureProvenance(map[string]string{"source_type": "file"}, "file"),
		}},
	}

	err := idx.Validate()
	require.Error(t, err)
	require.ErrorIs(t, err, ErrSourceStale)
}

func TestIndex_ValidateAllowsBenignVectorizerMetadataURLQuery(t *testing.T) {
	t.Parallel()

	text := indexTestAuthText
	idx := &Index{
		Version: IndexVersion,
		Vectorizer: VectorizerMetadata{
			Kind:     VectorizerKindEmbedding,
			Provider: "ollama",
			Model:    "nomic-embed-text",
			BaseURL:  "https://example.com/embed?b=2&a=1",
		},
		Dimensions: 2,
		Chunk:      ChunkOptions{MaxRunes: 100, OverlapRunes: 10},
		Sources:    []SourceMetadata{SourceMetadataForText("docs/auth.md", text)},
		Documents: []Document{{
			ID:         "docs/auth.md#chunk=0000",
			Text:       text,
			SourceHash: sourceHash(text),
			Vector:     Vector{1, 0},
			Metadata:   map[string]string{"path": "docs/auth.md"},
			Provenance: ensureProvenance(map[string]string{"source_type": "file"}, "file"),
		}},
	}

	require.NoError(t, idx.Validate())
	assert.Equal(t, "https://example.com/embed?b=2&a=1", idx.Vectorizer.BaseURL)
}

func TestIndex_ValidateAllowsAlreadyRedactedVectorizerMetadataURL(t *testing.T) {
	t.Parallel()

	text := indexTestAuthText
	idx := &Index{
		Version: IndexVersion,
		Vectorizer: VectorizerMetadata{
			Kind:     VectorizerKindEmbedding,
			Provider: "ollama",
			Model:    "nomic-embed-text",
			BaseURL:  NewEmbeddingMetadata("ollama", "nomic-embed-text", privateEmbeddingEndpoint(), 2).BaseURL,
		},
		Dimensions: 2,
		Chunk:      ChunkOptions{MaxRunes: 100, OverlapRunes: 10},
		Sources:    []SourceMetadata{SourceMetadataForText("docs/auth.md", text)},
		Documents: []Document{{
			ID:         "docs/auth.md#chunk=0000",
			Text:       text,
			SourceHash: sourceHash(text),
			Vector:     Vector{1, 0},
			Metadata:   map[string]string{"path": "docs/auth.md"},
			Provenance: ensureProvenance(map[string]string{"source_type": "file"}, "file"),
		}},
	}

	require.NoError(t, idx.Validate())
	assert.NotContains(t, idx.Vectorizer.BaseURL, "pass")
	assert.NotContains(t, idx.Vectorizer.BaseURL, "secret-token")
}

func TestIndex_ValidateRejectsPrivateDocumentVectorizerSpec(t *testing.T) {
	t.Parallel()

	text := indexTestAuthText
	idx := &Index{
		Version:    IndexVersion,
		Vectorizer: NewLexicalMetadata(2),
		Dimensions: 2,
		Chunk:      ChunkOptions{MaxRunes: 100, OverlapRunes: 10},
		Sources:    []SourceMetadata{SourceMetadataForText("docs/auth.md", text)},
		Documents: []Document{{
			ID:         "docs/auth.md#chunk=0000",
			Text:       text,
			SourceHash: sourceHash(text),
			Vector:     Vector{1, 0},
			Vectorizer: VectorizerSpec{
				ID:            TextHashVectorizerID,
				Model:         "api_key=secret-token",
				Normalization: TextHashVectorizerNormalization,
				Version:       vectorizerSpecVersion,
				Dimensions:    2,
			},
			Metadata:   map[string]string{"path": "docs/auth.md"},
			Provenance: ensureProvenance(map[string]string{"source_type": "file"}, "file"),
		}},
	}

	err := idx.Validate()
	require.Error(t, err)
	assert.ErrorIs(t, err, ErrPrivacyPolicy)
}

func TestIndex_ValidateRejectsDocumentVectorizerDimensionMismatch(t *testing.T) {
	t.Parallel()

	text := indexTestAuthText
	idx := &Index{
		Version:    IndexVersion,
		Vectorizer: NewLexicalMetadata(2),
		Dimensions: 2,
		Chunk:      ChunkOptions{MaxRunes: 100, OverlapRunes: 10},
		Sources:    []SourceMetadata{SourceMetadataForText("docs/auth.md", text)},
		Documents: []Document{{
			ID:         "docs/auth.md#chunk=0000",
			Text:       text,
			SourceHash: sourceHash(text),
			Vector:     Vector{1, 0},
			Vectorizer: TextVectorizerSpec(3),
			Metadata:   map[string]string{"path": "docs/auth.md"},
			Provenance: ensureProvenance(map[string]string{"source_type": "file"}, "file"),
		}},
	}

	err := idx.Validate()
	require.Error(t, err)
	assert.ErrorIs(t, err, ErrDimensionMismatch)
}

func TestIndex_ValidateRejectsDocumentVectorizerMetadataMismatch(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		spec VectorizerSpec
	}{
		{
			name: "provider",
			spec: VectorizerSpec{
				ID:            "ollama-compatible-embedding",
				Provider:      "other-provider",
				Model:         "nomic-embed-text",
				BaseURL:       "http://127.0.0.1:11434",
				Normalization: "trim-space-v1",
				Version:       vectorizerSpecVersion,
				Dimensions:    2,
			},
		},
		{
			name: "model",
			spec: VectorizerSpec{
				ID:            "ollama-compatible-embedding",
				Model:         "other-embedding-model",
				Normalization: "trim-space-v1",
				Version:       vectorizerSpecVersion,
				Dimensions:    2,
			},
		},
		{
			name: "base_url",
			spec: VectorizerSpec{
				ID:            "ollama-compatible-embedding",
				Provider:      "ollama",
				Model:         "nomic-embed-text",
				BaseURL:       "http://127.0.0.1:11435",
				Normalization: "trim-space-v1",
				Version:       vectorizerSpecVersion,
				Dimensions:    2,
			},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			text := indexTestAuthText
			idx := &Index{
				Version:    IndexVersion,
				Vectorizer: NewEmbeddingMetadata("ollama", "nomic-embed-text", "http://127.0.0.1:11434", 2),
				Dimensions: 2,
				Chunk:      ChunkOptions{MaxRunes: 100, OverlapRunes: 10},
				Sources:    []SourceMetadata{SourceMetadataForText("docs/auth.md", text)},
				Documents: []Document{{
					ID:         "docs/auth.md#chunk=0000",
					Text:       text,
					SourceHash: sourceHash(text),
					Vector:     Vector{1, 0},
					Vectorizer: tc.spec,
					Metadata:   map[string]string{"path": "docs/auth.md"},
					Provenance: ensureProvenance(map[string]string{"source_type": "file"}, "file"),
				}},
			}

			err := idx.Validate()
			require.Error(t, err)
			assert.ErrorIs(t, err, ErrVectorizerMismatch)
		})
	}
}

func TestIndex_ValidateRejectsMismatchedTextHashVector(t *testing.T) {
	t.Parallel()

	vectorizer, err := NewTextVectorizer(16)
	require.NoError(t, err)

	idx, err := BuildIndex(
		context.TODO(),
		[]Source{{Path: "docs/auth.md", Text: indexTestAuthText}},
		vectorizer,
		vectorizer.Metadata(),
		ChunkOptions{MaxRunes: 100},
		time.Unix(1, 0),
	)
	require.NoError(t, err)
	require.Len(t, idx.Documents, 1)

	for i, value := range idx.Documents[0].Vector {
		if value != 0 {
			idx.Documents[0].Vector[i] = -value

			break
		}
	}

	err = idx.Validate()
	require.Error(t, err)
	assert.ErrorIs(t, err, ErrVectorMismatch)
}

func privateEmbeddingEndpoint() string {
	return "https://user:p" + "ass@example.com/embed?api_" + "key=secret-" + "token"
}

func malformedPrivateEmbeddingEndpoint() string {
	return "user:p" + "ass@example.com/embed?api_" + "key=secret-" + "token"
}

func TestIndex_ValidateForChunkOptionsMismatch(t *testing.T) {
	t.Parallel()

	vectorizer, err := NewTextVectorizer(8)
	require.NoError(t, err)

	source := Source{Path: "docs/vector.md", Text: "hashed token fallback search"}
	idx, err := BuildIndex(
		context.TODO(),
		[]Source{source},
		vectorizer,
		vectorizer.Metadata(),
		ChunkOptions{MaxRunes: 100, OverlapRunes: 10},
		time.Unix(1, 0),
	)
	require.NoError(t, err)

	current := []SourceMetadata{SourceMetadataForText(source.Path, source.Text)}
	err = idx.ValidateFor(vectorizer.Metadata(), current, ChunkOptions{MaxRunes: 80, OverlapRunes: 8})
	require.Error(t, err)
	require.ErrorIs(t, err, ErrMetadataMismatch)
	assert.Contains(t, err.Error(), "chunk options")
}

func TestLoadIndex_DimensionMismatch(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	path := filepath.Join(dir, "index.json")
	idx := Index{
		Version:    IndexVersion,
		CreatedAt:  time.Unix(1, 0).UTC(),
		Vectorizer: NewLexicalMetadata(2),
		Dimensions: 2,
		Chunk:      ChunkOptions{MaxRunes: 100, OverlapRunes: 0},
		Sources: []SourceMetadata{{
			Path:   "docs/vector.md",
			Digest: DigestText("hello"),
			Bytes:  5,
		}},
		Documents: []Document{{
			ID:     "docs/vector.md#chunk=0000",
			Text:   "hello",
			Vector: Vector{1, 0, 1},
		}},
	}

	data, err := json.Marshal(idx)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(path, data, 0o600))

	_, err = LoadIndex(path)
	require.Error(t, err)
	assert.ErrorIs(t, err, ErrDimensionMismatch)
}

func TestBuildIndex_RejectsMetadataDimensionMismatch(t *testing.T) {
	t.Parallel()

	vectorizer, err := NewTextVectorizer(8)
	require.NoError(t, err)

	_, err = BuildIndex(
		context.TODO(),
		[]Source{{Path: "docs/vector.md", Text: "hashed token fallback search"}},
		vectorizer,
		NewLexicalMetadata(16),
		ChunkOptions{MaxRunes: 100},
		time.Unix(1, 0),
	)
	require.Error(t, err)
	assert.ErrorIs(t, err, ErrDimensionMismatch)
}

func TestBuildIndex_HonorsCanceledContextBeforeVectorizing(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithCancel(context.TODO())
	cancel()

	vectorizer := &recordingVectorizer{vector: Vector{1, 0}}
	_, err := BuildIndex(
		ctx,
		[]Source{{Path: "docs/auth.md", Text: indexTestAuthText}},
		vectorizer,
		NewLexicalMetadata(2),
		ChunkOptions{MaxRunes: 100},
		time.Unix(1, 0),
	)
	require.Error(t, err)
	require.ErrorIs(t, err, context.Canceled)
	assert.Empty(t, vectorizer.texts)
}

func TestBuildIndex_RejectsNilContext(t *testing.T) {
	t.Parallel()

	vectorizer := &recordingVectorizer{vector: Vector{1, 0}}
	_, err := BuildIndex(
		nil, //nolint:staticcheck // Verify nil contexts are rejected instead of panicking.
		[]Source{{Path: "docs/auth.md", Text: indexTestAuthText}},
		vectorizer,
		NewLexicalMetadata(2),
		ChunkOptions{MaxRunes: 100},
		time.Unix(1, 0),
	)
	require.Error(t, err)
	require.ErrorIs(t, err, ErrContextRequired)
	assert.Empty(t, vectorizer.texts)
}

func TestBuildIndex_RejectsNilContextBeforeOtherValidation(t *testing.T) {
	t.Parallel()

	_, err := BuildIndex(
		nil, //nolint:staticcheck // Verify nil contexts are rejected before other validation.
		nil,
		nil,
		VectorizerMetadata{},
		ChunkOptions{},
		time.Unix(1, 0),
	)
	require.Error(t, err)
	require.ErrorIs(t, err, ErrContextRequired)
}

func TestBuildIndex_InfersVectorizerMetadata(t *testing.T) {
	t.Parallel()

	vectorizer := &metadataRecordingVectorizer{
		recordingVectorizer: &recordingVectorizer{vector: Vector{1, 0}},
		metadata:            NewEmbeddingMetadata("test-provider", "test-embed", "http://127.0.0.1:11434", 0),
		spec: VectorizerSpec{
			ID:            "test-embedding",
			Model:         "test-embed",
			Normalization: "test-v1",
			Version:       vectorizerSpecVersion,
		},
	}

	idx, err := BuildIndex(
		context.TODO(),
		[]Source{{Path: "docs/auth.md", Text: indexTestAuthText}},
		vectorizer,
		VectorizerMetadata{},
		ChunkOptions{MaxRunes: 100},
		time.Unix(1, 0),
	)
	require.NoError(t, err)
	require.NoError(t, idx.Validate())

	assert.Equal(t, VectorizerKindEmbedding, idx.Vectorizer.Kind)
	assert.Equal(t, "test-provider", idx.Vectorizer.Provider)
	assert.Equal(t, "test-embed", idx.Vectorizer.Model)
	assert.Equal(t, 2, idx.Vectorizer.Dimensions)
	require.Len(t, idx.Documents, 1)
	assert.Equal(t, "test-embedding", idx.Documents[0].Vectorizer.ID)
}

func TestBuildIndex_FillsPartialVectorizerMetadata(t *testing.T) {
	t.Parallel()

	vectorizer := &metadataRecordingVectorizer{
		recordingVectorizer: &recordingVectorizer{vector: Vector{1, 0}},
		metadata:            NewEmbeddingMetadata("test-provider", "partial-embed", "http://127.0.0.1:11434", 0),
		spec: VectorizerSpec{
			ID:            "test-embedding",
			Model:         "partial-embed",
			Normalization: "test-v1",
			Version:       vectorizerSpecVersion,
		},
	}

	idx, err := BuildIndex(
		context.TODO(),
		[]Source{{Path: "docs/auth.md", Text: indexTestAuthText}},
		vectorizer,
		VectorizerMetadata{Kind: VectorizerKindEmbedding},
		ChunkOptions{MaxRunes: 100},
		time.Unix(1, 0),
	)
	require.NoError(t, err)
	require.NoError(t, idx.Validate())

	assert.Equal(t, "test-provider", idx.Vectorizer.Provider)
	assert.Equal(t, "partial-embed", idx.Vectorizer.Model)
	assert.Equal(t, "http://127.0.0.1:11434", idx.Vectorizer.BaseURL)
	assert.Equal(t, 2, idx.Vectorizer.Dimensions)
}

func TestBuildIndex_RejectsMissingVectorizerMetadata(t *testing.T) {
	t.Parallel()

	vectorizer := &recordingVectorizer{vector: Vector{1, 0}}
	_, err := BuildIndex(
		context.TODO(),
		[]Source{{Path: "docs/auth.md", Text: indexTestAuthText}},
		vectorizer,
		VectorizerMetadata{},
		ChunkOptions{MaxRunes: 100},
		time.Unix(1, 0),
	)
	require.Error(t, err)
	require.ErrorIs(t, err, ErrMetadataMismatch)
	assert.Empty(t, vectorizer.texts)
}

func TestIndex_SaveTightensExistingFilePermissions(t *testing.T) {
	t.Parallel()

	vectorizer, err := NewTextVectorizer(8)
	require.NoError(t, err)

	idx, err := BuildIndex(
		context.TODO(),
		[]Source{{Path: "docs/auth.md", Text: indexTestAuthText}},
		vectorizer,
		vectorizer.Metadata(),
		ChunkOptions{MaxRunes: 100},
		time.Unix(1, 0),
	)
	require.NoError(t, err)

	path := filepath.Join(t.TempDir(), "workspace-index.json")
	//nolint:gosec // Intentionally start loose to prove vector index persistence tightens permissions.
	require.NoError(t, os.WriteFile(path, []byte("{}"), 0o644))
	require.NoError(t, idx.Save(path))

	info, err := os.Stat(path)
	require.NoError(t, err)
	assert.Equal(t, os.FileMode(0o600), info.Mode().Perm())
}

func TestIndex_SaveRejectsUnsafePersistedDocument(t *testing.T) {
	t.Parallel()

	idx := &Index{
		Version:    IndexVersion,
		Vectorizer: NewLexicalMetadata(2),
		Dimensions: 2,
		Chunk:      ChunkOptions{MaxRunes: 100, OverlapRunes: 10},
		Sources:    []SourceMetadata{SourceMetadataForText("docs/auth.md", "api_key=super-secret-token")},
		Documents: []Document{{
			ID:         "docs/auth.md#chunk=0000",
			Text:       "api_key=super-secret-token",
			SourceHash: sourceHash("api_key=super-secret-token"),
			Vector:     Vector{1, 0},
			Metadata:   map[string]string{"path": "docs/auth.md"},
			Provenance: ensureProvenance(map[string]string{"source_type": "file"}, "file"),
		}},
	}

	err := idx.Save(filepath.Join(t.TempDir(), "workspace-index.json"))
	require.Error(t, err)
	require.ErrorIs(t, err, ErrPrivacyPolicy)
}

func TestIndex_SaveRejectsDocumentWithoutProvenance(t *testing.T) {
	t.Parallel()

	text := indexTestAuthText
	idx := &Index{
		Version:    IndexVersion,
		Vectorizer: NewLexicalMetadata(2),
		Dimensions: 2,
		Chunk:      ChunkOptions{MaxRunes: 100, OverlapRunes: 10},
		Sources:    []SourceMetadata{SourceMetadataForText("docs/auth.md", text)},
		Documents: []Document{{
			ID:         "docs/auth.md#chunk=0000",
			Text:       text,
			SourceHash: sourceHash(text),
			Vector:     Vector{1, 0},
			Metadata:   map[string]string{"path": "docs/auth.md"},
		}},
	}

	err := idx.Save(filepath.Join(t.TempDir(), "workspace-index.json"))
	require.Error(t, err)
	require.ErrorIs(t, err, ErrProvenanceMissing)
}

func TestSourceMetadataForText_RedactsBeforeDigesting(t *testing.T) {
	t.Parallel()

	rawText := "OAuth retry notes include api_key=super-secret-token"
	redactedText := privacy.RedactText(rawText)

	meta := SourceMetadataForText("docs/auth.md", rawText)

	assert.Equal(t, "docs/auth.md", meta.Path)
	assert.Equal(t, DigestText(redactedText), meta.Digest)
	assert.NotEqual(t, DigestText(rawText), meta.Digest)
	assert.Equal(t, len([]byte(redactedText)), meta.Bytes)
}

func TestBuildIndex_RedactsChunksBeforeVectorizingAndPersisting(t *testing.T) {
	t.Parallel()

	vectorizer := &recordingVectorizer{vector: Vector{1, 0}}
	rawText := "OAuth retry notes include api_key=super-secret-token"

	idx, err := BuildIndex(
		context.TODO(),
		[]Source{{Path: "docs/auth.md", Text: rawText}},
		vectorizer,
		NewLexicalMetadata(2),
		ChunkOptions{MaxRunes: 100},
		time.Unix(1, 0),
	)
	require.NoError(t, err)
	require.Len(t, idx.Documents, 1)

	wantText := privacy.RedactText(rawText)
	assert.Equal(t, []string{wantText}, vectorizer.texts)
	assert.Equal(t, wantText, idx.Documents[0].Text)
	assert.Equal(t, sourceHash(wantText), idx.Documents[0].SourceHash)
	assert.NotContains(t, idx.Documents[0].Text, "super-secret-token")
	assert.Equal(t, "true", idx.Documents[0].Metadata[retrieval.MetadataSafetyRedacted])
	assert.Equal(t, "false", idx.Documents[0].Metadata[retrieval.MetadataSafetyInjectAllowed])
	assert.Equal(t, "file", idx.Documents[0].Provenance["source_type"])
	assert.Equal(t, privacy.RedactionPolicyVersion, idx.Documents[0].Provenance["privacy_policy"])
}

func TestBuildIndex_RedactsSourcePathBeforePersisting(t *testing.T) {
	t.Parallel()

	vectorizer := &recordingVectorizer{vector: Vector{1, 0}}
	rawSecret := "super-" + "secret-token"
	rawPath := "docs/api_key=" + rawSecret + "/auth.md"
	rawText := "OAuth callback state validation"

	idx, err := BuildIndex(
		context.TODO(),
		[]Source{{Path: rawPath, Text: rawText}},
		vectorizer,
		NewLexicalMetadata(2),
		ChunkOptions{MaxRunes: 100},
		time.Unix(1, 0),
	)
	require.NoError(t, err)
	require.Len(t, idx.Sources, 1)
	require.Len(t, idx.Documents, 1)

	doc := idx.Documents[0]
	assert.Equal(t, "docs/api_key=[REDACTED]/auth.md", idx.Sources[0].Path)
	assert.Equal(t, idx.Sources[0].Path, doc.Metadata["path"])
	assert.Equal(t, idx.Sources[0].Path, doc.Provenance["path"])
	assert.Contains(t, doc.ID, "[REDACTED]")
	assert.NotContains(t, doc.ID, rawSecret)
	assert.NotContains(t, idx.Sources[0].Path, rawSecret)
	assert.NotContains(t, doc.Metadata["path"], rawSecret)
	assert.NotContains(t, doc.Provenance["path"], rawSecret)

	data, err := json.Marshal(idx)
	require.NoError(t, err)
	assert.NotContains(t, string(data), rawSecret)

	_, err = idx.SearchANN(Vector{1, 0}, 1, ANNOptions{})
	require.NoError(t, err)
}

func TestSearchANNRejectsMismatchedLexicalVector(t *testing.T) {
	t.Parallel()

	vectorizer, err := NewTextVectorizer(16)
	require.NoError(t, err)

	idx, err := BuildIndex(
		context.TODO(),
		[]Source{{Path: "docs/auth.md", Text: indexTestAuthText}},
		vectorizer,
		vectorizer.Metadata(),
		ChunkOptions{MaxRunes: 100},
		time.Unix(1, 0),
	)
	require.NoError(t, err)
	require.Len(t, idx.Documents, 1)
	assert.Equal(t, TextHashVectorizerID, idx.Documents[0].Vectorizer.ID)

	for i, value := range idx.Documents[0].Vector {
		if value != 0 {
			idx.Documents[0].Vector[i] = -value

			break
		}
	}

	_, err = idx.SearchANN(idx.Documents[0].Vector, 1, ANNOptions{})
	require.Error(t, err)
	assert.ErrorIs(t, err, ErrVectorMismatch)
}

func TestBuildIndex_RedactsWholeSourceBeforeChunking(t *testing.T) {
	t.Parallel()

	vectorizer := &recordingVectorizer{vector: Vector{1, 0}}
	privateKey := "-----BEGIN PRIVATE KEY-----\n" + strings.Repeat("A", 120) + "\n-----END PRIVATE KEY-----"
	rawText := privateKey + "\nOAuth retry notes remain searchable."

	idx, err := BuildIndex(
		context.TODO(),
		[]Source{{Path: "docs/key.md", Text: rawText}},
		vectorizer,
		NewLexicalMetadata(2),
		ChunkOptions{MaxRunes: 40, OverlapRunes: 5},
		time.Unix(1, 0),
	)
	require.NoError(t, err)
	require.NotEmpty(t, idx.Documents)

	var persisted strings.Builder
	for i := range idx.Documents {
		persisted.WriteString(idx.Documents[i].Text)
		assert.NotContains(t, vectorizer.texts[i], "PRIVATE KEY")
		assert.NotContains(t, vectorizer.texts[i], strings.Repeat("A", 20))
	}

	assert.NotContains(t, persisted.String(), "PRIVATE KEY")
	assert.NotContains(t, persisted.String(), strings.Repeat("A", 20))
	assert.Contains(t, persisted.String(), "[REDACTED]")
	assert.Equal(t, "true", idx.Documents[0].Metadata[retrieval.MetadataSafetyRedacted])
	assert.Equal(t, "false", idx.Documents[0].Metadata[retrieval.MetadataSafetyInjectAllowed])
}

func TestIndex_ValidateForStaleSource(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	sourcePath := filepath.Join(dir, "notes.md")
	require.NoError(t, os.WriteFile(sourcePath, []byte("first retrieval design"), 0o600))

	source, err := SourceFromFile(sourcePath)
	require.NoError(t, err)

	vectorizer, err := NewTextVectorizer(8)
	require.NoError(t, err)

	idx, err := BuildIndex(
		context.TODO(),
		[]Source{source},
		vectorizer,
		vectorizer.Metadata(),
		ChunkOptions{MaxRunes: 100},
		time.Unix(1, 0),
	)
	require.NoError(t, err)

	require.NoError(t, os.WriteFile(sourcePath, []byte("second retrieval design"), 0o600))
	current, err := SourceMetadataFromFile(sourcePath)
	require.NoError(t, err)

	err = idx.ValidateFor(vectorizer.Metadata(), []SourceMetadata{current})
	require.Error(t, err)
	assert.ErrorIs(t, err, ErrSourceStale)
}

func TestRetrievalEval_RankingRegression(t *testing.T) {
	t.Parallel()

	docs := []Source{
		{Path: "docs/retrieval.md", Text: "Local retrieval chunks source files, stores model metadata, and invalidates stale source digests."},
		{Path: "docs/shell.md", Text: "Shell commands execute with timeout handling and process output capture."},
		{Path: "docs/oauth.md", Text: "OAuth callbacks validate state before retrying token exchange failures."},
	}
	cases := []struct {
		name   string
		query  string
		vector string
		memory string
	}{
		{
			name:   "stale vector index invalidation",
			query:  "How does retrieval invalidate stale indexed files?",
			vector: "docs/retrieval.md#chunk=0000",
			memory: "docs/retrieval.md",
		},
		{
			name:   "shell timeout output capture",
			query:  "Where is shell command timeout handling and process output capture?",
			vector: "docs/shell.md#chunk=0000",
			memory: "docs/shell.md",
		},
		{
			name:   "oauth callback token exchange",
			query:  "Where do OAuth callbacks validate state before retrying token exchange failures?",
			vector: "docs/oauth.md#chunk=0000",
			memory: "docs/oauth.md",
		},
	}

	lexical, err := NewTextVectorizer(64)
	require.NoError(t, err)
	lexicalIndex, err := BuildIndex(context.TODO(), docs, lexical, lexical.Metadata(), ChunkOptions{MaxRunes: 240}, time.Unix(1, 0))
	require.NoError(t, err)

	embedding := semanticEvalVectorizer{}
	embeddingIndex, err := BuildIndex(
		context.TODO(),
		docs,
		embedding,
		NewEmbeddingMetadata("ollama", "eval-embedding", "http://127.0.0.1:11434", 3),
		ChunkOptions{MaxRunes: 240},
		time.Unix(1, 0),
	)
	require.NoError(t, err)

	mem := memory.NewStore()
	for _, doc := range docs {
		require.NoError(t, mem.Add(memory.Document{ID: doc.Path, Path: doc.Path, Text: doc.Text}))
	}

	for _, tc := range cases {
		assertTopVectorResult(t, lexicalIndex, lexical, tc.query, tc.vector, tc.name)
		assertTopVectorResult(t, embeddingIndex, embedding, tc.query, tc.vector, tc.name)

		memoryResults, err := mem.Search(tc.query, 1)
		require.NoError(t, err, tc.name)
		require.Len(t, memoryResults, 1, tc.name)
		assert.Equal(t, tc.memory, memoryResults[0].Document.ID, tc.name)
	}
}

func assertTopVectorResult(t *testing.T, idx *Index, vectorizer Vectorizer, query, wantID, name string) {
	t.Helper()

	queryVector, err := vectorizer.Vectorize(query)
	require.NoError(t, err, name)

	store, err := idx.Store()
	require.NoError(t, err, name)

	results, err := store.Search(queryVector, 1)
	require.NoError(t, err, name)
	require.Len(t, results, 1, name)
	assert.Equal(t, wantID, results[0].Document.ID, name)
}

func sourceMetadataKinds(sources []SourceMetadata) []string {
	kinds := make([]string, 0, len(sources))
	for i := range sources {
		kinds = append(kinds, sources[i].Kind)
	}

	return kinds
}

func sourceMetadataForSources(sources []Source) []SourceMetadata {
	out := make([]SourceMetadata, 0, len(sources))
	for _, source := range sources {
		out = append(out, SourceMetadataForTextWithKind(source.Path, source.Text, source.Kind))
	}

	return out
}

func documentMetadataSourceKinds(docs []Document) []string {
	kinds := make([]string, 0, len(docs))
	for i := range docs {
		kinds = append(kinds, docs[i].Metadata["source_kind"])
	}

	return kinds
}

func documentProvenanceTypes(docs []Document) []string {
	types := make([]string, 0, len(docs))
	for i := range docs {
		types = append(types, docs[i].Provenance["source_type"])
	}

	return types
}

type semanticEvalVectorizer struct{}

func (semanticEvalVectorizer) Vectorize(text string) (Vector, error) {
	tokens := memory.Tokenize(text)
	if len(tokens) == 0 {
		return nil, ErrEmptyText
	}

	vec := Vector{0, 0, 0}

	for _, token := range tokens {
		switch token {
		case "rag", "retrieval", "chunks", "chunk", "metadata", "model", "invalidates", "invalidate", "stale", "digests", "indexed", "files":
			vec[0]++
		case "shell", "command", "commands", "timeout", "handling", "process", "output", "capture":
			vec[1]++
		case "oauth", "callback", "callbacks", "validate", "state", "retrying", "token", "exchange", "failures":
			vec[2]++
		}
	}

	if vec[0] == 0 && vec[1] == 0 && vec[2] == 0 {
		return nil, errors.New("semantic eval vectorizer: no known tokens")
	}

	return vec, nil
}

type recordingVectorizer struct {
	vector Vector
	texts  []string
}

func (v *recordingVectorizer) Vectorize(text string) (Vector, error) {
	v.texts = append(v.texts, text)

	return cloneVector(v.vector), nil
}

type metadataRecordingVectorizer struct {
	*recordingVectorizer
	metadata VectorizerMetadata
	spec     VectorizerSpec
}

func (v *metadataRecordingVectorizer) Metadata(dimensions int) VectorizerMetadata {
	metadata := v.metadata
	metadata.Dimensions = dimensions

	return metadata
}

func (v *metadataRecordingVectorizer) Spec(dimensions int) VectorizerSpec {
	spec := v.spec
	spec.Dimensions = dimensions

	return spec
}
