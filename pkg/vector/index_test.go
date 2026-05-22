package vector

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/tommoulard/atteler/pkg/memory"
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

func TestVectorizerMetadata_CompatibleWithProviderAlias(t *testing.T) {
	t.Parallel()

	actual := NewEmbeddingMetadata("ollama-compatible", "nomic-embed-text", "http://127.0.0.1:11434/", 3)
	expected := NewEmbeddingMetadata("ollama", "nomic-embed-text", "http://127.0.0.1:11434", 0)

	require.NoError(t, actual.CompatibleWith(expected))
	assert.Equal(t, "ollama", actual.Provider)
	assert.Equal(t, "http://127.0.0.1:11434", actual.BaseURL)
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
