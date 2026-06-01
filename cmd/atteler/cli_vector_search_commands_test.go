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
	"github.com/tommoulard/atteler/pkg/privacy"
	"github.com/tommoulard/atteler/pkg/vector"
)

const testGlobalVectorModel = "global-model"

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

func TestVectorSearchCommandInputFromOptionsCarriesRuntimeOverrides(t *testing.T) {
	t.Parallel()

	input := vectorSearchCommandInputFromOptions(cliOptions{
		vectorSearch:            "semantic retrieval",
		vectorIndexFiles:        stringListFlag{"docs/research.md"},
		vectorLimit:             positiveIntFlag{value: 3, set: true},
		vectorizer:              vector.VectorizerKindEmbedding,
		vectorProvider:          "ollama",
		vectorModel:             "cli-embed",
		vectorBaseURL:           "http://127.0.0.1:11434",
		vectorFallbackPolicy:    vector.VectorizerKindLexical,
		vectorStorePath:         "./cli-vector-index.json",
		vectorTimeout:           positiveIntFlag{value: 9, set: true},
		vectorChunkMaxRunes:     positiveIntFlag{value: 700, set: true},
		vectorChunkOverlapRunes: positiveIntFlag{value: 70, set: true},
	})

	assert.Equal(t, "semantic retrieval", input.Query)
	assert.Equal(t, []string{"docs/research.md"}, input.IndexFiles)
	assert.Equal(t, 3, input.Limit)
	assert.Equal(t, vector.VectorizerKindEmbedding, input.Vector.Vectorizer)
	assert.Equal(t, "ollama", input.Vector.Provider)
	assert.Equal(t, "cli-embed", input.Vector.Model)
	assert.Equal(t, "http://127.0.0.1:11434", input.Vector.BaseURL)
	assert.Equal(t, vector.VectorizerKindLexical, input.Vector.FallbackPolicy)
	assert.Equal(t, "./cli-vector-index.json", input.Vector.StorePath)
	assert.Equal(t, 9, input.Vector.TimeoutSeconds)
	assert.Equal(t, 700, input.Vector.ChunkMaxRunes)
	assert.Equal(t, 70, input.Vector.ChunkOverlapRunes)
	assert.True(t, input.Vector.TimeoutSet)
	assert.True(t, input.Vector.ChunkMaxSet)
	assert.True(t, input.Vector.ChunkOverlapSet)
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
	require.NoError(t, os.WriteFile(note, []byte("embedding retrieval ranking"), 0o600))

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
		if strings.Contains(req.Input, "embedding") {
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
		vectorSearch:     "embedding retrieval",
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

func TestRunVectorSearchRejectsRemoteEmbeddingWithoutConsent(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	note := filepath.Join(dir, "remote.md")
	require.NoError(t, os.WriteFile(note, []byte("remote embedding should require consent"), 0o600))

	indexPath := filepath.Join(dir, "embedding-index.json")
	err := runVectorSearch(context.TODO(), dir, appconfig.VectorConfig{}, cliOptions{
		vectorSearch:     "remote embedding",
		vectorIndexFiles: stringListFlag{note},
		vectorStorePath:  indexPath,
		vectorizer:       vector.VectorizerKindEmbedding,
		vectorBaseURL:    privateRemoteEmbeddingEndpoint(),
	})

	require.Error(t, err)
	require.ErrorContains(t, err, "remote embedding endpoint")
	require.ErrorContains(t, err, "vector.workspace_allow_remote_embeddings")
	assert.NoFileExists(t, indexPath)
	assert.NoFileExists(t, lexicalFallbackIndexPath(indexPath))
}

//nolint:paralleltest // Captures process stdout to verify user-facing CLI output.
func TestRunVectorSearchUsesLexicalFallbackForRemoteEmbeddingWithoutConsent(t *testing.T) {
	dir := t.TempDir()
	note := filepath.Join(dir, "remote-fallback.md")
	require.NoError(t, os.WriteFile(note, []byte("remote embedding fallback stays local"), 0o600))

	indexPath := filepath.Join(dir, "embedding-index.json")

	var runErr error

	output := captureVectorSearchStdout(t, func() {
		runErr = runVectorSearch(context.TODO(), dir, appconfig.VectorConfig{}, cliOptions{
			vectorSearch:         "remote fallback",
			vectorIndexFiles:     stringListFlag{note},
			vectorStorePath:      indexPath,
			vectorizer:           vector.VectorizerKindEmbedding,
			vectorBaseURL:        privateRemoteEmbeddingEndpoint(),
			vectorFallbackPolicy: vector.VectorizerKindLexical,
			vectorLimit:          positiveIntFlag{value: 1, set: true},
		})
	})
	require.NoError(t, runErr)
	assert.Contains(t, output, "vectorizer=lexical-fallback")
	assert.Contains(t, output, filepath.Clean(note)+"#chunk=0000")
	assert.NoFileExists(t, indexPath)
	assert.FileExists(t, lexicalFallbackIndexPath(indexPath))
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

func TestLoadOrBuildVectorIndexRefreshesAddedFilesIncrementally(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	first := filepath.Join(dir, "first.md")
	second := filepath.Join(dir, "second.md")

	require.NoError(t, os.WriteFile(first, []byte("alpha retrieval"), 0o600))
	require.NoError(t, os.WriteFile(second, []byte("beta retrieval"), 0o600))

	indexPath := filepath.Join(dir, "vector-index.json")
	settings := vectorSearchSettings{
		IndexPath: indexPath,
		Chunk:     vector.ChunkOptions{MaxRunes: 100, OverlapRunes: 10}.Normalize(),
	}
	vectorizer := &countingTestVectorizer{}
	metadata := vectorizer.Metadata()

	idx, rebuilt, err := loadOrBuildVectorIndex(context.TODO(), settings, []string{first}, vectorizer, metadata)
	require.NoError(t, err)
	require.True(t, rebuilt)
	require.NotNil(t, idx)
	require.FileExists(t, indexPath)
	assert.Equal(t, 1, vectorizer.calls)

	vectorizer.calls = 0
	idx, rebuilt, err = loadOrBuildVectorIndex(context.TODO(), settings, []string{second}, vectorizer, metadata)
	require.NoError(t, err)
	require.True(t, rebuilt)
	require.NotNil(t, idx)
	assert.Equal(t, 1, vectorizer.calls, "refresh should vectorize only the added file")

	loaded, err := vector.LoadIndex(indexPath)
	require.NoError(t, err)
	assert.ElementsMatch(t, []string{filepath.Clean(first), filepath.Clean(second)}, indexSourcePaths(loaded))
}

func TestRunVectorSearchRefusesToOverwriteNonIndexPathWithSources(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	note := filepath.Join(dir, "note.md")
	indexPath := filepath.Join(dir, "not-an-index.json")
	original := []byte("not a vector index")

	require.NoError(t, os.WriteFile(note, []byte("local rag source"), 0o600))
	require.NoError(t, os.WriteFile(indexPath, original, 0o600))

	err := runVectorSearch(context.TODO(), dir, appconfig.VectorConfig{}, cliOptions{
		vectorIndexFiles: stringListFlag{note},
		vectorStorePath:  indexPath,
	})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "refusing to overwrite existing non-index file")

	data, readErr := os.ReadFile(indexPath)
	require.NoError(t, readErr)
	assert.Equal(t, original, data)
}

func TestRunVectorSearchRefusesToRefreshDifferentSourceIndex(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	note := filepath.Join(dir, "note.md")
	indexPath := filepath.Join(dir, "shared-vector-index.json")

	require.NoError(t, os.WriteFile(note, []byte("local rag file source"), 0o600))
	writeSourceVectorIndex(t, indexPath, []vector.Source{{
		Kind: vector.SourceKindSession,
		Path: "sessions/session-123",
		Text: "Session source should not be replaced by file vector indexing.",
	}})

	err := runVectorSearch(context.TODO(), dir, appconfig.VectorConfig{}, cliOptions{
		vectorIndexFiles: stringListFlag{note},
		vectorStorePath:  indexPath,
	})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "refusing to refresh file vector index")

	loaded, loadErr := vector.LoadIndex(indexPath)
	require.NoError(t, loadErr)
	assert.ElementsMatch(t, []string{vector.SourceKindSession}, sourceMetadataKinds(loaded.Sources))
}

func TestRunVectorSearchRefusesToRefreshDifferentStaleSourceIndex(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	note := filepath.Join(dir, "note.md")
	indexPath := filepath.Join(dir, "stale-session-vector-index.json")

	require.NoError(t, os.WriteFile(note, []byte("local rag file source"), 0o600))
	writeSourceVectorIndex(t, indexPath, []vector.Source{{
		Kind: vector.SourceKindSession,
		Path: "sessions/session-123",
		Text: "Stale session vectors should still protect the source family.",
	}})
	makePersistedVectorIndexStale(t, indexPath)

	err := runVectorSearch(context.TODO(), dir, appconfig.VectorConfig{}, cliOptions{
		vectorIndexFiles: stringListFlag{note},
		vectorStorePath:  indexPath,
	})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "refusing to refresh file vector index")

	data, readErr := os.ReadFile(indexPath)
	require.NoError(t, readErr)
	assert.Contains(t, string(data), `"kind": "session"`)
}

func TestRunVectorSearchDropsDeletedReusableSourcesWhileKeepingPresentSources(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	first := filepath.Join(dir, "first.md")
	second := filepath.Join(dir, "second.md")
	third := filepath.Join(dir, "third.md")

	require.NoError(t, os.WriteFile(first, []byte("alpha retrieval"), 0o600))
	require.NoError(t, os.WriteFile(second, []byte("beta retrieval"), 0o600))
	require.NoError(t, os.WriteFile(third, []byte("gamma retrieval"), 0o600))

	indexPath := filepath.Join(dir, "vector-index.json")
	err := runVectorSearch(context.TODO(), dir, appconfig.VectorConfig{}, cliOptions{
		vectorIndexFiles: stringListFlag{first, second},
		vectorStorePath:  indexPath,
	})
	require.NoError(t, err)

	require.NoError(t, os.Remove(second))

	err = runVectorSearch(context.TODO(), dir, appconfig.VectorConfig{}, cliOptions{
		vectorIndexFiles: stringListFlag{third},
		vectorStorePath:  indexPath,
	})
	require.NoError(t, err)

	loaded, err := vector.LoadIndex(indexPath)
	require.NoError(t, err)
	assert.ElementsMatch(t,
		[]string{filepath.Clean(first), filepath.Clean(third)},
		indexSourcePaths(loaded),
	)
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
			ID:         filepath.Clean(note) + "#chunk=0000",
			Text:       noteText,
			SourceHash: privacy.SourceHash(noteText),
			Vector:     vector.Vector{1, 0},
			Metadata: map[string]string{
				"path":          filepath.Clean(note),
				"source_digest": vector.DigestText(noteText),
			},
			Provenance: map[string]string{
				"source_type":    "file",
				"privacy_policy": privacy.RedactionPolicyVersion,
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
			ID:         filepath.Clean(note) + "#chunk=0000",
			Text:       noteText,
			SourceHash: privacy.SourceHash(noteText),
			Vector:     vector.Vector{1, 0},
			Metadata: map[string]string{
				"path":          filepath.Clean(note),
				"source_digest": vector.DigestText(noteText),
			},
			Provenance: map[string]string{
				"source_type":    "file",
				"privacy_policy": privacy.RedactionPolicyVersion,
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
		CreatedAt:  time.Date(2026, 6, 1, 10, 0, 0, 0, time.UTC),
		UpdatedAt:  time.Date(2026, 6, 1, 10, 30, 0, 0, time.UTC),
		Documents: []vector.Document{{
			ID:     "docs/retrieval.md#chunk=0000",
			Text:   "embedding retrieval metadata",
			Vector: vector.Vector{1, 0},
		}},
		Sources: []vector.SourceMetadata{{
			Path:   "docs/retrieval.md",
			Digest: vector.DigestText("embedding retrieval metadata"),
			Bytes:  len("embedding retrieval metadata"),
		}},
	}, ".atteler/vector-index.json", false)

	assert.Contains(t, header, "vectorizer=embedding/ollama")
	assert.Contains(t, header, "model=eval-embed")
	assert.Contains(t, header, "base_url=http://127.0.0.1:11434")
	assert.Contains(t, header, "created_at=2026-06-01T10:00:00Z")
	assert.Contains(t, header, "updated_at=2026-06-01T10:30:00Z")
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

func TestVectorSearchSettings_UsesScopedVectorizerConfig(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	settings, err := vectorSearchSettingsFromOptions(root, appconfig.VectorConfig{
		Vectorizer:     vector.VectorizerKindLexical,
		Provider:       "ollama",
		Model:          testGlobalVectorModel,
		FallbackPolicy: "fail",
		Stores: map[string]appconfig.VectorizerConfig{
			vectorSearchVectorStore: {
				Vectorizer:        vector.VectorizerKindEmbedding,
				Model:             "search-embed",
				IndexPath:         "./scoped-index.json",
				TimeoutSeconds:    9,
				ChunkMaxRunes:     700,
				ChunkOverlapRunes: 70,
			},
		},
	}, cliOptions{})
	require.NoError(t, err)

	assert.Equal(t, vector.VectorizerKindEmbedding, settings.Vectorizer)
	assert.Equal(t, "ollama", settings.Provider)
	assert.Equal(t, "search-embed", settings.Model)
	assert.Equal(t, filepath.Join(root, "scoped-index.json"), settings.IndexPath)
	assert.Equal(t, 9, int(settings.Timeout.Seconds()))
	assert.Equal(t, 700, settings.Chunk.MaxRunes)
	assert.Equal(t, 70, settings.Chunk.OverlapRunes)
}

func TestVectorSearchSettings_SourceFileConfigOverridesStoreConfig(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	settings, err := vectorSearchSettingsFromOptions(root, appconfig.VectorConfig{
		Vectorizer:     vector.VectorizerKindLexical,
		Provider:       ollamaProviderName,
		Model:          testGlobalVectorModel,
		FallbackPolicy: "fail",
		Stores: map[string]appconfig.VectorizerConfig{
			vectorSearchVectorStore: {
				Vectorizer:        vector.VectorizerKindLexical,
				Model:             "store-file-embed",
				IndexPath:         "./store-file-index.json",
				TimeoutSeconds:    5,
				ChunkMaxRunes:     500,
				ChunkOverlapRunes: 50,
			},
		},
		Sources: map[string]appconfig.VectorizerConfig{
			vector.SourceKindFile: {
				Vectorizer:        vector.VectorizerKindEmbedding,
				Model:             "source-file-embed",
				IndexPath:         "./source-file-index.json",
				TimeoutSeconds:    7,
				ChunkMaxRunes:     700,
				ChunkOverlapRunes: 70,
			},
		},
	}, cliOptions{})
	require.NoError(t, err)

	assert.Equal(t, vector.VectorizerKindEmbedding, settings.Vectorizer)
	assert.Equal(t, ollamaProviderName, settings.Provider)
	assert.Equal(t, "source-file-embed", settings.Model)
	assert.Equal(t, filepath.Join(root, "source-file-index.json"), settings.IndexPath)
	assert.Equal(t, 7, int(settings.Timeout.Seconds()))
	assert.Equal(t, 700, settings.Chunk.MaxRunes)
	assert.Equal(t, 70, settings.Chunk.OverlapRunes)
}

func TestVectorSearchSettings_CLIOverridesSourceFileConfig(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	settings, err := vectorSearchSettingsFromOptions(root, appconfig.VectorConfig{
		Vectorizer:     vector.VectorizerKindLexical,
		Provider:       ollamaProviderName,
		Model:          testGlobalVectorModel,
		FallbackPolicy: vector.VectorizerKindLexical,
		Sources: map[string]appconfig.VectorizerConfig{
			vector.SourceKindFile: {
				Vectorizer:        vector.VectorizerKindEmbedding,
				Model:             "source-file-embed",
				IndexPath:         "./source-file-index.json",
				TimeoutSeconds:    7,
				ChunkMaxRunes:     700,
				ChunkOverlapRunes: 70,
			},
		},
	}, cliOptions{
		vectorizer:              vector.VectorizerKindLexical,
		vectorModel:             "cli-file-model",
		vectorStorePath:         "./cli-file-index.json",
		vectorFallbackPolicy:    "fail",
		vectorTimeout:           positiveIntFlag{value: 3, set: true},
		vectorChunkMaxRunes:     positiveIntFlag{value: 300, set: true},
		vectorChunkOverlapRunes: positiveIntFlag{value: 30, set: true},
		vectorLimit:             positiveIntFlag{value: 9, set: true},
	})
	require.NoError(t, err)

	assert.Equal(t, vector.VectorizerKindLexical, settings.Vectorizer)
	assert.Equal(t, "cli-file-model", settings.Model)
	assert.Equal(t, filepath.Join(root, "cli-file-index.json"), settings.IndexPath)
	assert.Equal(t, "fail", settings.FallbackPolicy)
	assert.Equal(t, 3, int(settings.Timeout.Seconds()))
	assert.Equal(t, 300, settings.Chunk.MaxRunes)
	assert.Equal(t, 30, settings.Chunk.OverlapRunes)
	assert.Equal(t, 9, settings.Limit)
}

func TestVectorSearchSettings_ResolvesRelativeCLIStoreAgainstRoot(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	settings, err := vectorSearchSettingsFromOptions(root, appconfig.VectorConfig{}, cliOptions{
		vectorStorePath: "./cli-index.json",
	})
	require.NoError(t, err)

	assert.Equal(t, filepath.Join(root, "cli-index.json"), settings.IndexPath)
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
	assert.Equal(t, ".atteler/vector-index.lexical.json", lexicalFallbackIndexPath(".atteler/vector-index.lexical.json"))
	assert.Equal(t, ".atteler/vector-index.lexical", lexicalFallbackIndexPath(".atteler/vector-index.lexical"))
}

func TestPresentVectorSearchFallbackPathsSkipsDeletedReusableSources(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	present := filepath.Join(dir, "present.md")
	deleted := filepath.Join(dir, "deleted.md")
	requested := filepath.Join(dir, "requested.md")

	require.NoError(t, os.WriteFile(present, []byte("present source"), 0o600))

	got := presentVectorSearchFallbackPaths([]string{present, deleted}, []string{requested})

	assert.ElementsMatch(t, []string{filepath.Clean(present), filepath.Clean(requested)}, got)
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
			ID:         filepath.Clean(note) + "#chunk=0000",
			Text:       noteText,
			SourceHash: privacy.SourceHash(noteText),
			Vector:     vector.Vector{1, 0},
			Metadata: map[string]string{
				"path":          filepath.Clean(note),
				"source_digest": vector.DigestText(noteText),
			},
			Provenance: map[string]string{
				"source_type":    "file",
				"privacy_policy": privacy.RedactionPolicyVersion,
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

//nolint:paralleltest // Captures process stdout to verify user-facing fallback output.
func TestRunVectorSearchEmbeddingFailureFallbackMergesReusableAndRequestedSources(t *testing.T) {
	dir := t.TempDir()
	existing := filepath.Join(dir, "fallback-existing.md")
	existingText := "existing fallback source remains searchable during embedding outage"
	requested := filepath.Join(dir, "fallback-requested.md")
	requestedText := "requested fallback source is appended during embedding outage"

	require.NoError(t, os.WriteFile(existing, []byte(existingText), 0o600))
	require.NoError(t, os.WriteFile(requested, []byte(requestedText), 0o600))

	indexPath := filepath.Join(dir, "embedding-index.json")
	idx := &vector.Index{
		Version:    vector.IndexVersion,
		CreatedAt:  time.Unix(1, 0).UTC(),
		Vectorizer: vector.NewEmbeddingMetadata("ollama", "offline-embed", "://invalid-embedding-endpoint", 2),
		Dimensions: 2,
		Chunk:      vector.ChunkOptions{}.Normalize(),
		Sources: []vector.SourceMetadata{
			vector.SourceMetadataForText(existing, existingText),
		},
		Documents: []vector.Document{{
			ID:         filepath.Clean(existing) + "#chunk=0000",
			Text:       existingText,
			SourceHash: privacy.SourceHash(existingText),
			Vector:     vector.Vector{1, 0},
			Metadata: map[string]string{
				"path":          filepath.Clean(existing),
				"source_digest": vector.DigestText(existingText),
			},
			Provenance: map[string]string{
				"source_type":    "file",
				"privacy_policy": privacy.RedactionPolicyVersion,
			},
		}},
	}
	require.NoError(t, idx.Save(indexPath))

	var runErr error

	output := captureVectorSearchStdout(t, func() {
		runErr = runVectorSearch(context.TODO(), dir, appconfig.VectorConfig{}, cliOptions{
			vectorSearch:         "existing fallback source",
			vectorIndexFiles:     stringListFlag{requested},
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
	assert.Contains(t, output, filepath.Clean(existing)+"#chunk=0000")

	loaded, err := vector.LoadIndex(lexicalFallbackIndexPath(indexPath))
	require.NoError(t, err)
	assert.Equal(t, vector.VectorizerKindLexical, loaded.Vectorizer.Kind)
	assert.ElementsMatch(t, []string{filepath.Clean(existing), filepath.Clean(requested)}, sourceMetadataPaths(loaded.Sources))
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

func sourceMetadataPaths(sources []vector.SourceMetadata) []string {
	paths := make([]string, 0, len(sources))
	for _, source := range sources {
		paths = append(paths, source.Path)
	}

	return paths
}

type countingTestVectorizer struct {
	calls int
}

func (v *countingTestVectorizer) Vectorize(text string) (vector.Vector, error) {
	v.calls++

	if strings.Contains(text, "beta") {
		return vector.Vector{0, 1}, nil
	}

	return vector.Vector{1, 0}, nil
}

func (v *countingTestVectorizer) Metadata() vector.VectorizerMetadata {
	return vector.NewEmbeddingMetadata("test-provider", "incremental-test", "http://127.0.0.1:11434", 2)
}

func (v *countingTestVectorizer) Spec(dimensions int) vector.VectorizerSpec {
	return vector.VectorizerSpec{
		ID:            "test-incremental-vectorizer",
		Model:         "incremental-test",
		Normalization: "test-v1",
		Version:       "1",
		Dimensions:    dimensions,
	}
}

func makePersistedVectorIndexStale(t *testing.T, indexPath string) {
	t.Helper()

	idx, err := vector.LoadIndex(indexPath)
	require.NoError(t, err)
	require.NotEmpty(t, idx.Documents)
	require.NotEmpty(t, idx.Documents[0].Vector)

	idx.Documents[0].Vector[0]++

	data, err := json.MarshalIndent(idx, "", "  ")
	require.NoError(t, err)

	data = append(data, '\n')

	require.NoError(t, os.WriteFile(indexPath, data, 0o600))
	_, err = vector.LoadIndex(indexPath)
	require.ErrorIs(t, err, vector.ErrVectorMismatch)
}
