package main

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	appconfig "github.com/tommoulard/atteler/pkg/config"
	"github.com/tommoulard/atteler/pkg/vector"
)

func TestFormatVectorResult(t *testing.T) {
	t.Parallel()

	got := formatVectorResult(vector.Result{
		Document: vector.Document{
			ID:       "docs/research.md",
			Metadata: map[string]string{"path": "docs/research.md"},
		},
		Score: 0.75,
	})

	want := "docs/research.md\tscore=0.7500\tpath=docs/research.md"
	if got != want {
		require.Failf(t, "unexpected vector result format", "got %q, want %q", got, want)
	}
}

//nolint:paralleltest // Captures process stdout to verify user-facing CLI output.
func TestRunVectorSearchPersistsLexicalIndexAndReportsVectorizer(t *testing.T) {
	dir := t.TempDir()
	note := filepath.Join(dir, "retrieval.md")
	require.NoError(t, os.WriteFile(note, []byte("OAuth retry retrieval notes"), 0o600))

	indexPath := filepath.Join(dir, "vector-index.json")
	opts := cliOptions{
		vectorSearch:     "OAuth retry",
		vectorIndexFiles: stringListFlag{note},
		vectorStorePath:  indexPath,
		vectorLimit:      positiveIntFlag{value: 1, set: true},
	}

	var runErr error

	output := captureVectorSearchStdout(t, func() {
		runErr = runVectorSearch(context.TODO(), dir, appconfig.VectorConfig{}, opts)
	})
	require.NoError(t, runErr)
	assert.Contains(t, output, "vectorizer=lexical-fallback")
	assert.Contains(t, output, "model=hashed-token-frequency")
	assert.Contains(t, output, "chunks=1")
	assert.Contains(t, output, "chunk=0")

	loaded, err := vector.LoadIndex(indexPath)
	require.NoError(t, err)
	require.Len(t, loaded.Documents, 1)
	assert.Equal(t, filepath.Clean(note)+"#chunk=0000", loaded.Documents[0].ID)

	opts = cliOptions{
		vectorSearch:    "OAuth retry",
		vectorStorePath: indexPath,
		vectorLimit:     positiveIntFlag{value: 1, set: true},
	}
	output = captureVectorSearchStdout(t, func() {
		runErr = runVectorSearch(context.TODO(), dir, appconfig.VectorConfig{}, opts)
	})
	require.NoError(t, runErr)
	assert.Contains(t, output, "vectorizer=lexical-fallback")
	assert.NotContains(t, output, "rebuilt=true")
}

//nolint:paralleltest // Captures process stdout to verify user-facing CLI output.
func TestRunVectorSearchPersistsEmbeddingIndexAndReportsModel(t *testing.T) {
	dir := t.TempDir()
	note := filepath.Join(dir, "embedding.md")
	require.NoError(t, os.WriteFile(note, []byte("semantic retrieval ranking"), 0o600))

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, "/api/embed", r.URL.Path)

		var req struct {
			Input string `json:"input"`
			Model string `json:"model"`
		}
		if !assert.NoError(t, json.NewDecoder(r.Body).Decode(&req)) {
			http.Error(w, "bad request", http.StatusBadRequest)
			return
		}

		assert.Equal(t, "cmd-embed", req.Model)

		embedding := []float64{0, 1}
		if strings.Contains(req.Input, "semantic") {
			embedding = []float64{1, 0}
		}

		w.Header().Set("Content-Type", "application/json")
		assert.NoError(t, json.NewEncoder(w).Encode(struct {
			Embeddings [][]float64 `json:"embeddings"`
		}{Embeddings: [][]float64{embedding}}))
	}))
	defer server.Close()

	indexPath := filepath.Join(dir, "embedding-index.json")
	opts := cliOptions{
		vectorSearch:     "semantic retrieval",
		vectorIndexFiles: stringListFlag{note},
		vectorStorePath:  indexPath,
		vectorizer:       vector.VectorizerKindEmbedding,
		vectorProvider:   "ollama",
		vectorModel:      "cmd-embed",
		vectorBaseURL:    server.URL,
		vectorLimit:      positiveIntFlag{value: 1, set: true},
	}

	var runErr error

	output := captureVectorSearchStdout(t, func() {
		runErr = runVectorSearch(context.TODO(), dir, appconfig.VectorConfig{}, opts)
	})
	require.NoError(t, runErr)
	assert.Contains(t, output, "vectorizer=embedding/ollama")
	assert.Contains(t, output, "model=cmd-embed")
	assert.Contains(t, output, "base_url="+server.URL)

	loaded, err := vector.LoadIndex(indexPath)
	require.NoError(t, err)
	assert.Equal(t, vector.VectorizerKindEmbedding, loaded.Vectorizer.Kind)
	assert.Equal(t, "cmd-embed", loaded.Vectorizer.Model)
	assert.Equal(t, server.URL, loaded.Vectorizer.BaseURL)
	assert.Equal(t, 2, loaded.Dimensions)
}

func TestRunVectorSearchAddsFilesToReusableIndex(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	first := filepath.Join(dir, "first.md")
	second := filepath.Join(dir, "second.md")

	require.NoError(t, os.WriteFile(first, []byte("alpha retrieval"), 0o600))
	require.NoError(t, os.WriteFile(second, []byte("beta retrieval"), 0o600))

	indexPath := filepath.Join(dir, "vector-index.json")
	err := runVectorSearch(context.TODO(), dir, appconfig.VectorConfig{}, cliOptions{
		vectorIndexFiles: stringListFlag{first},
		vectorStorePath:  indexPath,
	})
	require.NoError(t, err)

	err = runVectorSearch(context.TODO(), dir, appconfig.VectorConfig{}, cliOptions{
		vectorIndexFiles: stringListFlag{second},
		vectorStorePath:  indexPath,
	})
	require.NoError(t, err)

	loaded, err := vector.LoadIndex(indexPath)
	require.NoError(t, err)
	require.Len(t, loaded.Sources, 2)
	require.Len(t, loaded.Documents, 2)
	assert.Equal(t, filepath.Clean(first), loaded.Sources[0].Path)
	assert.Equal(t, filepath.Clean(second), loaded.Sources[1].Path)
}

func TestRunVectorSearchRejectsStaleReusableIndex(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	note := filepath.Join(dir, "stale.md")
	indexPath := filepath.Join(dir, "vector-index.json")

	require.NoError(t, os.WriteFile(note, []byte("original retrieval metadata"), 0o600))
	require.NoError(t, runVectorSearch(context.TODO(), dir, appconfig.VectorConfig{}, cliOptions{
		vectorIndexFiles: stringListFlag{note},
		vectorStorePath:  indexPath,
	}))

	require.NoError(t, os.WriteFile(note, []byte("changed retrieval metadata"), 0o600))

	err := runVectorSearch(context.TODO(), dir, appconfig.VectorConfig{}, cliOptions{
		vectorSearch:    "retrieval metadata",
		vectorStorePath: indexPath,
	})

	require.Error(t, err)
	require.ErrorIs(t, err, vector.ErrSourceStale)
	assert.Contains(t, err.Error(), "pass --vector-index to rebuild")
}

func TestRunVectorSearchRejectsMetadataMismatchWithoutRebuildSources(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	note := filepath.Join(dir, "metadata.md")
	indexPath := filepath.Join(dir, "vector-index.json")

	require.NoError(t, os.WriteFile(note, []byte("lexical retrieval metadata"), 0o600))
	require.NoError(t, runVectorSearch(context.TODO(), dir, appconfig.VectorConfig{}, cliOptions{
		vectorIndexFiles: stringListFlag{note},
		vectorStorePath:  indexPath,
	}))

	err := runVectorSearch(context.TODO(), dir, appconfig.VectorConfig{}, cliOptions{
		vectorSearch:    "retrieval metadata",
		vectorStorePath: indexPath,
		vectorizer:      vector.VectorizerKindEmbedding,
		vectorModel:     "eval-embed",
		vectorBaseURL:   "http://127.0.0.1:1",
	})

	require.Error(t, err)
	require.ErrorIs(t, err, vector.ErrMetadataMismatch)
	assert.Contains(t, err.Error(), "pass --vector-index to rebuild")
}

func TestRunVectorSearchRejectsChunkMismatchWithoutRebuildSources(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	note := filepath.Join(dir, "chunks.md")
	indexPath := filepath.Join(dir, "vector-index.json")

	require.NoError(t, os.WriteFile(note, []byte("chunked retrieval metadata needs stable chunk settings"), 0o600))
	require.NoError(t, runVectorSearch(context.TODO(), dir, appconfig.VectorConfig{}, cliOptions{
		vectorIndexFiles: stringListFlag{note},
		vectorStorePath:  indexPath,
	}))

	err := runVectorSearch(context.TODO(), dir, appconfig.VectorConfig{}, cliOptions{
		vectorSearch:            "retrieval metadata",
		vectorStorePath:         indexPath,
		vectorChunkMaxRunes:     positiveIntFlag{value: 40, set: true},
		vectorChunkOverlapRunes: positiveIntFlag{value: 4, set: true},
	})

	require.Error(t, err)
	require.ErrorIs(t, err, vector.ErrMetadataMismatch)
	assert.Contains(t, err.Error(), "chunk options")
	assert.Contains(t, err.Error(), "pass --vector-index to rebuild")
}

func TestRunVectorSearchRejectsQueryDimensionMismatchWithRebuildHint(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	note := filepath.Join(dir, "dimension.md")
	noteText := "embedding dimension metadata"
	require.NoError(t, os.WriteFile(note, []byte(noteText), 0o600))

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		assert.NoError(t, json.NewEncoder(w).Encode(struct {
			Embeddings [][]float64 `json:"embeddings"`
		}{Embeddings: [][]float64{{1, 0, 0}}}))
	}))
	defer server.Close()

	indexPath := filepath.Join(dir, "embedding-index.json")
	idx := &vector.Index{
		Version:    vector.IndexVersion,
		CreatedAt:  time.Unix(1, 0).UTC(),
		Vectorizer: vector.NewEmbeddingMetadata("ollama", "dimension-embed", server.URL, 2),
		Dimensions: 2,
		Chunk:      vector.ChunkOptions{MaxRunes: vector.DefaultChunkMaxRunes, OverlapRunes: vector.DefaultChunkOverlapRunes},
		Sources: []vector.SourceMetadata{
			vector.SourceMetadataForText(note, noteText),
		},
		Documents: []vector.Document{{
			ID:     filepath.Clean(note) + "#chunk=0000",
			Text:   noteText,
			Vector: vector.Vector{1, 0},
			Metadata: map[string]string{
				"path":          filepath.Clean(note),
				"source_digest": vector.DigestText(noteText),
			},
		}},
	}
	require.NoError(t, idx.Save(indexPath))

	err := runVectorSearch(context.TODO(), dir, appconfig.VectorConfig{}, cliOptions{
		vectorSearch:    "embedding dimension",
		vectorStorePath: indexPath,
		vectorizer:      vector.VectorizerKindEmbedding,
		vectorModel:     "dimension-embed",
		vectorBaseURL:   server.URL,
	})

	require.Error(t, err)
	require.ErrorIs(t, err, vector.ErrDimensionMismatch)
	assert.Contains(t, err.Error(), "pass --vector-index to rebuild")
}

//nolint:paralleltest // Captures process stdout to verify user-facing rebuild output.
func TestRunVectorSearchRebuildsDimensionMismatchWhenSourcesProvided(t *testing.T) {
	dir := t.TempDir()
	note := filepath.Join(dir, "dimension-rebuild.md")
	noteText := "embedding dimension metadata rebuild"
	require.NoError(t, os.WriteFile(note, []byte(noteText), 0o600))

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		assert.NoError(t, json.NewEncoder(w).Encode(struct {
			Embeddings [][]float64 `json:"embeddings"`
		}{Embeddings: [][]float64{{1, 0, 0}}}))
	}))
	defer server.Close()

	indexPath := filepath.Join(dir, "embedding-index.json")
	idx := &vector.Index{
		Version:    vector.IndexVersion,
		CreatedAt:  time.Unix(1, 0).UTC(),
		Vectorizer: vector.NewEmbeddingMetadata("ollama", "dimension-embed", server.URL, 2),
		Dimensions: 2,
		Chunk:      vector.ChunkOptions{MaxRunes: vector.DefaultChunkMaxRunes, OverlapRunes: vector.DefaultChunkOverlapRunes},
		Sources: []vector.SourceMetadata{
			vector.SourceMetadataForText(note, noteText),
		},
		Documents: []vector.Document{{
			ID:     filepath.Clean(note) + "#chunk=0000",
			Text:   noteText,
			Vector: vector.Vector{1, 0},
			Metadata: map[string]string{
				"path":          filepath.Clean(note),
				"source_digest": vector.DigestText(noteText),
			},
		}},
	}
	require.NoError(t, idx.Save(indexPath))

	var runErr error

	output := captureVectorSearchStdout(t, func() {
		runErr = runVectorSearch(context.TODO(), dir, appconfig.VectorConfig{}, cliOptions{
			vectorSearch:     "embedding dimension",
			vectorIndexFiles: stringListFlag{note},
			vectorStorePath:  indexPath,
			vectorizer:       vector.VectorizerKindEmbedding,
			vectorModel:      "dimension-embed",
			vectorBaseURL:    server.URL,
			vectorLimit:      positiveIntFlag{value: 1, set: true},
		})
	})
	require.NoError(t, runErr)
	assert.Contains(t, output, "rebuilt=true")

	loaded, err := vector.LoadIndex(indexPath)
	require.NoError(t, err)
	assert.Equal(t, 3, loaded.Dimensions)
	assert.Equal(t, 3, loaded.Vectorizer.Dimensions)
}

func TestFormatVectorSearchHeaderReportsEmbeddingMetadata(t *testing.T) {
	t.Parallel()

	header := formatVectorSearchHeader(&vector.Index{
		Vectorizer: vector.NewEmbeddingMetadata("ollama", "eval-embed", "http://127.0.0.1:11434", 2),
		Dimensions: 2,
		Documents: []vector.Document{{
			ID:     "docs/retrieval.md#chunk=0000",
			Text:   "semantic retrieval metadata",
			Vector: vector.Vector{1, 0},
		}},
		Sources: []vector.SourceMetadata{{
			Path:   "docs/retrieval.md",
			Digest: vector.DigestText("semantic retrieval metadata"),
			Bytes:  len("semantic retrieval metadata"),
		}},
	}, ".atteler/vector-index.json", false)

	assert.Contains(t, header, "vectorizer=embedding/ollama")
	assert.Contains(t, header, "model=eval-embed")
	assert.Contains(t, header, "base_url=http://127.0.0.1:11434")
}

func TestVectorSearchSettings_CLIOverridesConfig(t *testing.T) {
	t.Parallel()

	settings, err := vectorSearchSettingsFromOptions(t.TempDir(), appconfig.VectorConfig{
		Vectorizer:        "embedding",
		Provider:          "ollama",
		Model:             "from-config",
		BaseURL:           "http://config.example",
		FallbackPolicy:    "lexical",
		TimeoutSeconds:    60,
		ChunkMaxRunes:     900,
		ChunkOverlapRunes: 90,
	}, cliOptions{
		vectorizer:              "lexical",
		vectorModel:             "from-cli",
		vectorFallbackPolicy:    "fail",
		vectorTimeout:           positiveIntFlag{value: 5, set: true},
		vectorChunkMaxRunes:     positiveIntFlag{value: 300, set: true},
		vectorChunkOverlapRunes: positiveIntFlag{value: 30, set: true},
		vectorLimit:             positiveIntFlag{value: 7, set: true},
	})
	require.NoError(t, err)

	assert.Equal(t, vector.VectorizerKindLexical, settings.Vectorizer)
	assert.Equal(t, "from-cli", settings.Model)
	assert.Equal(t, "fail", settings.FallbackPolicy)
	assert.Equal(t, 5, int(settings.Timeout.Seconds()))
	assert.Equal(t, 300, settings.Chunk.MaxRunes)
	assert.Equal(t, 30, settings.Chunk.OverlapRunes)
	assert.Equal(t, 7, settings.Limit)
}

func TestVectorSearchSettings_DefaultsChunkOverlapWhenOnlyMaxRunesConfigured(t *testing.T) {
	t.Parallel()

	settings, err := vectorSearchSettingsFromOptions(t.TempDir(), appconfig.VectorConfig{
		ChunkMaxRunes: 300,
	}, cliOptions{})
	require.NoError(t, err)

	assert.Equal(t, 300, settings.Chunk.MaxRunes)
	assert.Equal(t, vector.DefaultChunkOverlapRunes, settings.Chunk.OverlapRunes)
}

func TestNewVectorSearchVectorizerAcceptsOllamaCompatibleProviderAlias(t *testing.T) {
	t.Parallel()

	_, metadata, err := newVectorSearchVectorizer(vectorSearchSettings{
		Vectorizer: vector.VectorizerKindEmbedding,
		Provider:   "ollama-compatible",
		Model:      "eval-embed",
		BaseURL:    "http://127.0.0.1:11434/",
	})
	require.NoError(t, err)

	assert.Equal(t, vector.VectorizerKindEmbedding, metadata.Kind)
	assert.Equal(t, "ollama", metadata.Provider)
	assert.Equal(t, "eval-embed", metadata.Model)
	assert.Equal(t, "http://127.0.0.1:11434", metadata.BaseURL)
}

func TestLexicalFallbackIndexPathDoesNotClobberEmbeddingIndex(t *testing.T) {
	t.Parallel()

	assert.Equal(t, ".atteler/vector-index.lexical.json", lexicalFallbackIndexPath(".atteler/vector-index.json"))
	assert.Equal(t, ".atteler/vector-index.lexical", lexicalFallbackIndexPath(".atteler/vector-index"))
}

//nolint:paralleltest // Captures process stdout to verify user-facing fallback output.
func TestRunVectorSearchEmbeddingFailureFallsBackToLexicalIndex(t *testing.T) {
	dir := t.TempDir()
	note := filepath.Join(dir, "fallback.md")
	require.NoError(t, os.WriteFile(note, []byte("fallback retrieval should remain usable"), 0o600))

	indexPath := filepath.Join(dir, "embedding-index.json")
	opts := cliOptions{
		vectorSearch:         "fallback retrieval",
		vectorIndexFiles:     stringListFlag{note},
		vectorStorePath:      indexPath,
		vectorizer:           vector.VectorizerKindEmbedding,
		vectorBaseURL:        "://invalid-embedding-endpoint",
		vectorFallbackPolicy: vector.VectorizerKindLexical,
		vectorLimit:          positiveIntFlag{value: 1, set: true},
	}

	var runErr error

	output := captureVectorSearchStdout(t, func() {
		runErr = runVectorSearch(context.TODO(), dir, appconfig.VectorConfig{}, opts)
	})
	require.NoError(t, runErr)
	assert.Contains(t, output, "vectorizer=lexical-fallback")
	assert.Contains(t, output, "model=hashed-token-frequency")

	_, err := os.Stat(indexPath)
	require.ErrorIs(t, err, os.ErrNotExist)

	loaded, err := vector.LoadIndex(lexicalFallbackIndexPath(indexPath))
	require.NoError(t, err)
	assert.Equal(t, vector.VectorizerKindLexical, loaded.Vectorizer.Kind)
}

//nolint:paralleltest // Captures process stdout to verify user-facing fallback output.
func TestRunVectorSearchEmbeddingFailureFallsBackUsingReusableIndexSources(t *testing.T) {
	dir := t.TempDir()
	note := filepath.Join(dir, "fallback-reuse.md")
	noteText := "fallback source paths survive embedding outage"
	require.NoError(t, os.WriteFile(note, []byte(noteText), 0o600))

	indexPath := filepath.Join(dir, "embedding-index.json")
	idx := &vector.Index{
		Version:    vector.IndexVersion,
		CreatedAt:  time.Unix(1, 0).UTC(),
		Vectorizer: vector.NewEmbeddingMetadata("ollama", "offline-embed", "://invalid-embedding-endpoint", 2),
		Dimensions: 2,
		Chunk:      vector.ChunkOptions{}.Normalize(),
		Sources: []vector.SourceMetadata{
			vector.SourceMetadataForText(note, noteText),
		},
		Documents: []vector.Document{{
			ID:     filepath.Clean(note) + "#chunk=0000",
			Text:   noteText,
			Vector: vector.Vector{1, 0},
			Metadata: map[string]string{
				"path":          filepath.Clean(note),
				"source_digest": vector.DigestText(noteText),
			},
		}},
	}
	require.NoError(t, idx.Save(indexPath))

	var runErr error

	output := captureVectorSearchStdout(t, func() {
		runErr = runVectorSearch(context.TODO(), dir, appconfig.VectorConfig{}, cliOptions{
			vectorSearch:         "fallback source",
			vectorStorePath:      indexPath,
			vectorizer:           vector.VectorizerKindEmbedding,
			vectorModel:          "offline-embed",
			vectorBaseURL:        "://invalid-embedding-endpoint",
			vectorFallbackPolicy: vector.VectorizerKindLexical,
			vectorLimit:          positiveIntFlag{value: 1, set: true},
		})
	})
	require.NoError(t, runErr)
	assert.Contains(t, output, "vectorizer=lexical-fallback")
	assert.Contains(t, output, filepath.Clean(note)+"#chunk=0000")

	loaded, err := vector.LoadIndex(lexicalFallbackIndexPath(indexPath))
	require.NoError(t, err)
	assert.Equal(t, vector.VectorizerKindLexical, loaded.Vectorizer.Kind)
	require.Len(t, loaded.Sources, 1)
	assert.Equal(t, filepath.Clean(note), loaded.Sources[0].Path)
}

func captureVectorSearchStdout(t *testing.T, fn func()) string {
	t.Helper()

	old := os.Stdout
	reader, writer, err := os.Pipe()
	require.NoError(t, err)

	os.Stdout = writer

	fn()

	require.NoError(t, writer.Close())

	os.Stdout = old

	data, err := io.ReadAll(reader)
	require.NoError(t, err)
	require.NoError(t, reader.Close())

	return strings.TrimSpace(string(data))
}
