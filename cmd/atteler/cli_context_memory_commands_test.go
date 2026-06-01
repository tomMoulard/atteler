//nolint:wsl_v5 // Tests keep setup, action, and assertions close together.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
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

	"github.com/tommoulard/atteler/pkg/agentmemory"
	appconfig "github.com/tommoulard/atteler/pkg/config"
	"github.com/tommoulard/atteler/pkg/githistory"
	"github.com/tommoulard/atteler/pkg/llm"
	"github.com/tommoulard/atteler/pkg/memory"
	"github.com/tommoulard/atteler/pkg/retrieval"
	"github.com/tommoulard/atteler/pkg/session"
	"github.com/tommoulard/atteler/pkg/vector"
)

const (
	localMemorySessionID = "local"
	otherMemoryName      = "other"
)

func TestRunAgentMemoryCommandIndexesAndSearchesSelectedAgent(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()

	note := filepath.Join(dir, "note.txt")
	if err := os.WriteFile(note, []byte("OAuth callback retry memory"), 0o600); err != nil {
		require.NoError(t, err)
	}

	storePath := filepath.Join(dir, "agent-memory.json")

	err := runAgentMemoryCommand(context.TODO(), dir, testReviewerName, appconfig.VectorConfig{}, agentMemoryCommandInputFromOptions(cliOptions{
		agentMemoryStorePath:  storePath,
		agentMemorySearch:     "callback retry",
		agentMemoryIndexFiles: stringListFlag{note},
		agentMemoryLimit:      positiveIntFlag{value: 1, set: true},
	}))

	require.NoError(t, err)
	loaded, err := agentmemory.Load(storePath)
	require.NoError(t, err)
	results, err := loaded.Search(testReviewerName, "callback", 1)
	require.NoError(t, err)
	require.Len(t, results, 1)
	assert.Equal(t, filepath.Clean(note), results[0].Document.ID)
}

func TestRunAgentMemoryCommandUsesScopedEmbeddingVectorizerConfig(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	note := filepath.Join(dir, "semantic.txt")
	require.NoError(t, os.WriteFile(note, []byte("Semantic retrieval memory for local RAG"), 0o600))

	server := newAgentMemoryEmbeddingTestServer()
	defer server.Close()

	storePath := filepath.Join(dir, "configured-agent-memory.json")
	cfg := appconfig.VectorConfig{
		Stores: map[string]appconfig.VectorizerConfig{
			"agent_memory": {
				Vectorizer: vector.VectorizerKindLexical,
				IndexPath:  storePath,
			},
		},
		Agents: map[string]appconfig.VectorizerConfig{
			testReviewerName: {
				Vectorizer: vector.VectorizerKindEmbedding,
				Model:      "agent-memory-test-embed",
				BaseURL:    server.URL,
			},
		},
		Sources: map[string]appconfig.VectorizerConfig{
			vector.SourceKindFile: {
				Vectorizer: vector.VectorizerKindLexical,
			},
		},
	}

	err := runAgentMemoryCommand(context.TODO(), dir, testReviewerName, cfg, agentMemoryCommandInputFromOptions(cliOptions{
		agentMemoryIndexFiles: stringListFlag{note},
	}))
	require.NoError(t, err)

	loaded, err := agentmemory.Load(storePath)
	require.NoError(t, err)
	assert.Equal(t, "ollama-compatible-embedding", loaded.Vectorizer.ID)
	assert.Equal(t, "agent-memory-test-embed", loaded.Vectorizer.Model)
	assert.Equal(t, 2, loaded.Dimensions)

	_, err = loaded.SearchContext(context.TODO(), testReviewerName, "semantic retrieval", 1)
	require.ErrorIs(t, err, vector.ErrVectorizerMismatch)

	err = runAgentMemoryCommand(context.TODO(), dir, testReviewerName, cfg, agentMemoryCommandInputFromOptions(cliOptions{
		agentMemorySearch: "semantic retrieval",
		agentMemoryLimit:  positiveIntFlag{value: 1, set: true},
	}))
	require.NoError(t, err)
}

func TestRunAgentMemoryCommandRejectsRemoteEmbeddingWithoutConsent(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	note := filepath.Join(dir, "semantic.txt")
	require.NoError(t, os.WriteFile(note, []byte("Semantic retrieval memory for local RAG"), 0o600))

	storePath := filepath.Join(dir, "remote-agent-memory.json")
	cfg := appconfig.VectorConfig{
		Stores: map[string]appconfig.VectorizerConfig{
			agentMemoryVectorStore: {
				Vectorizer: vector.VectorizerKindEmbedding,
				BaseURL:    privateRemoteEmbeddingEndpoint(),
				IndexPath:  storePath,
			},
		},
	}

	err := runAgentMemoryCommand(context.TODO(), dir, testReviewerName, cfg, agentMemoryCommandInputFromOptions(cliOptions{
		agentMemoryIndexFiles: stringListFlag{note},
	}))

	require.Error(t, err)
	require.ErrorContains(t, err, "agent memory: remote embedding endpoint")
	require.ErrorContains(t, err, "vector.workspace_allow_remote_embeddings")
	assert.NoFileExists(t, storePath)
}

func TestBuildAgentMemoryRetrievalSearcherRejectsRemoteEmbeddingWithoutConsent(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	storePath := filepath.Join(dir, "remote-agent-memory.json")
	cfg := appconfig.VectorConfig{
		Stores: map[string]appconfig.VectorizerConfig{
			agentMemoryVectorStore: {
				Vectorizer: vector.VectorizerKindEmbedding,
				BaseURL:    privateRemoteEmbeddingEndpoint(),
				IndexPath:  storePath,
			},
		},
	}

	_, err := buildAgentMemoryRetrievalSearcher(context.TODO(), dir, testReviewerName, cfg, retrievalCommandInput{
		Search: "semantic retrieval",
	})

	require.Error(t, err)
	require.ErrorContains(t, err, "agent memory: remote embedding endpoint")
	require.ErrorContains(t, err, "vector.workspace_allow_remote_embeddings")
	assert.NoFileExists(t, storePath)
}

func TestRunAgentMemoryCommandMigratesLexicalStoreToScopedEmbeddingVectorizer(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	storePath := filepath.Join(dir, "agent-memory.json")

	store, err := agentmemory.NewStore(16)
	require.NoError(t, err)
	require.NoError(t, store.AddText(testReviewerName, "rag", "Semantic retrieval memory for local RAG"))
	require.NoError(t, store.Save(storePath))

	server := newAgentMemoryEmbeddingTestServer()
	defer server.Close()

	cfg := appconfig.VectorConfig{
		Stores: map[string]appconfig.VectorizerConfig{
			agentMemoryVectorStore: {
				Vectorizer: vector.VectorizerKindEmbedding,
				Model:      "migrated-agent-memory-embed",
				BaseURL:    server.URL,
				IndexPath:  storePath,
			},
		},
	}

	err = runAgentMemoryCommand(context.TODO(), dir, "", cfg, agentMemoryCommandInputFromOptions(cliOptions{
		agentMemoryMigrate: true,
	}))
	require.NoError(t, err)

	loaded, err := agentmemory.Load(storePath)
	require.NoError(t, err)
	assert.Equal(t, "ollama-compatible-embedding", loaded.Vectorizer.ID)
	assert.Equal(t, "migrated-agent-memory-embed", loaded.Vectorizer.Model)
	assert.Equal(t, 2, loaded.Dimensions)
}

func TestAgentMemoryStorePathResolvesRelativeIndexPathAgainstRoot(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	cfg := appconfig.VectorConfig{
		Stores: map[string]appconfig.VectorizerConfig{
			agentMemoryVectorStore: {
				IndexPath: "./configured-agent-memory.json",
			},
		},
	}

	assert.Equal(t,
		filepath.Join(root, "configured-agent-memory.json"),
		agentMemoryStorePath(root, testReviewerName, "", cfg),
	)
	assert.Equal(t,
		filepath.Join(root, "cli-agent-memory.json"),
		agentMemoryStorePath(root, testReviewerName, "./cli-agent-memory.json", appconfig.VectorConfig{}),
	)
}

func TestFormatRetrievalResultIncludesScorerDetailsWhenExplaining(t *testing.T) {
	t.Parallel()

	result := retrieval.Result{
		Source:     retrieval.Source{Type: retrieval.SourceVector, Name: "workspace"},
		DocumentID: "docs/local-rag.md#chunk=0000",
		Score:      0.875,
		Safety:     retrieval.Safety{InjectAllowed: true},
		Scorer: retrieval.Scorer{
			Name: "embedding-file-vector-index",
			Details: map[string]float64{
				"ann_min_candidates": 64,
				"ann_exact_scan":     0,
				"ann_documents":      65,
				"cosine.similarity":  0.875,
			},
			Explanation: []string{"ranked by ANN candidate search"},
		},
	}

	got := formatRetrievalResult(result, true)

	assert.Contains(t, got, "detail_ann_documents=65")
	assert.Contains(t, got, "detail_ann_exact_scan=false")
	assert.Contains(t, got, "detail_ann_min_candidates=64")
	assert.Contains(t, got, "detail_cosine_similarity=0.875")
	assert.Contains(t, got, "why=ranked by ANN candidate search")
	assert.Less(t, strings.Index(got, "detail_ann_documents=65"), strings.Index(got, "detail_ann_exact_scan=false"))
	assert.Less(t, strings.Index(got, "detail_ann_exact_scan=false"), strings.Index(got, "detail_ann_min_candidates=64"))
}

func TestFormatRetrievalResultOmitsScorerDetailsWithoutExplain(t *testing.T) {
	t.Parallel()

	result := retrieval.Result{
		Source:     retrieval.Source{Type: retrieval.SourceVector},
		DocumentID: "docs/local-rag.md#chunk=0000",
		Score:      0.875,
		Safety:     retrieval.Safety{InjectAllowed: true},
		Scorer: retrieval.Scorer{
			Name:    "embedding-file-vector-index",
			Details: map[string]float64{"ann_documents": 65},
		},
	}

	got := formatRetrievalResult(result, false)

	assert.NotContains(t, got, "detail_ann_documents")
}

func TestBuildVectorRetrievalSearcherUsesScopedEmbeddingVectorizerConfig(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	semanticPath := filepath.Join(dir, "semantic.md")
	shellPath := filepath.Join(dir, "shell.md")
	require.NoError(t, os.WriteFile(semanticPath, []byte("Semantic retrieval memory for local RAG"), 0o600))
	require.NoError(t, os.WriteFile(shellPath, []byte("Shell output capture and command timeout notes"), 0o600))

	server := newAgentMemoryEmbeddingTestServer()
	defer server.Close()

	searcher, err := buildVectorRetrievalSearcher(context.TODO(), appState{
		cwd: dir,
		vectorConfig: appconfig.VectorConfig{
			Stores: map[string]appconfig.VectorizerConfig{
				vectorSearchVectorStore: {
					Vectorizer: vector.VectorizerKindEmbedding,
					Model:      "retrieval-file-embed",
					BaseURL:    server.URL,
				},
			},
		},
	}, retrievalCommandInput{
		Search:           "semantic retrieval",
		VectorIndexFiles: []string{semanticPath, shellPath},
	})
	require.NoError(t, err)

	results, err := retrieval.Search(context.TODO(), retrieval.Query{
		Text:          "semantic retrieval",
		Limit:         1,
		IncludeUnsafe: true,
	}, searcher)
	require.NoError(t, err)
	require.Len(t, results, 1)
	assert.Contains(t, filepath.ToSlash(results[0].Metadata["path"]), "semantic.md")
	assert.Equal(t, "embedding-file-vector-index", results[0].Scorer.Name)
}

func TestBuildVectorRetrievalSearcherUsesVectorCLIOverrides(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	semanticPath := filepath.Join(dir, "semantic.md")
	shellPath := filepath.Join(dir, "shell.md")
	require.NoError(t, os.WriteFile(semanticPath, []byte("Semantic retrieval memory for local RAG"), 0o600))
	require.NoError(t, os.WriteFile(shellPath, []byte("Shell output capture and command timeout notes"), 0o600))

	server := newAgentMemoryEmbeddingTestServer()
	defer server.Close()

	searcher, err := buildVectorRetrievalSearcher(context.TODO(), appState{
		cwd: dir,
		vectorConfig: appconfig.VectorConfig{
			Stores: map[string]appconfig.VectorizerConfig{
				vectorSearchVectorStore: {
					Vectorizer: vector.VectorizerKindLexical,
					IndexPath:  ".atteler/configured-file-index.json",
				},
			},
		},
	}, retrievalCommandInput{
		Search: "semantic retrieval",
		Vector: retrievalVectorCommandInput{
			Vectorizer:        vector.VectorizerKindEmbedding,
			Model:             "cli-retrieval-embed",
			BaseURL:           server.URL,
			StorePath:         "./cli-retrieval-vector-index.json",
			ChunkMaxRunes:     600,
			ChunkOverlapRunes: 60,
			ChunkMaxSet:       true,
			ChunkOverlapSet:   true,
		},
		VectorIndexFiles: []string{semanticPath, shellPath},
	})
	require.NoError(t, err)

	results, err := retrieval.Search(context.TODO(), retrieval.Query{
		Text:          "semantic retrieval",
		Limit:         1,
		IncludeUnsafe: true,
	}, searcher)
	require.NoError(t, err)
	require.Len(t, results, 1)
	assert.Equal(t, "embedding-file-vector-index", results[0].Scorer.Name)

	loaded, err := vector.LoadIndex(filepath.Join(dir, "cli-retrieval-vector-index.json"))
	require.NoError(t, err)
	assert.Equal(t, vector.VectorizerKindEmbedding, loaded.Vectorizer.Kind)
	assert.Equal(t, "cli-retrieval-embed", loaded.Vectorizer.Model)
	assert.Equal(t, 600, loaded.Chunk.MaxRunes)
	assert.Equal(t, 60, loaded.Chunk.OverlapRunes)
	assert.NoFileExists(t, filepath.Join(dir, ".atteler", "configured-file-index.json"))
}

func TestRetrievalCommandInputFromOptionsCarriesVectorOverrides(t *testing.T) {
	t.Parallel()

	input := retrievalCommandInputFromOptions(cliOptions{
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

func TestBuildVectorRetrievalSearcherPersistsConfiguredFileIndex(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	semanticPath := filepath.Join(dir, "semantic.md")
	shellPath := filepath.Join(dir, "shell.md")
	require.NoError(t, os.WriteFile(semanticPath, []byte("Semantic retrieval memory for local RAG"), 0o600))
	require.NoError(t, os.WriteFile(shellPath, []byte("Shell output capture and command timeout notes"), 0o600))

	indexPath := filepath.Join(dir, "configured-file-vector-index.json")
	searcher, err := buildVectorRetrievalSearcher(context.TODO(), appState{
		cwd: dir,
		vectorConfig: appconfig.VectorConfig{
			Stores: map[string]appconfig.VectorizerConfig{
				vectorSearchVectorStore: {
					Vectorizer: vector.VectorizerKindLexical,
					IndexPath:  indexPath,
				},
			},
		},
	}, retrievalCommandInput{
		Search:           "semantic retrieval",
		VectorIndexFiles: []string{semanticPath, shellPath},
	})
	require.NoError(t, err)

	results, err := retrieval.Search(context.TODO(), retrieval.Query{
		Text:          "semantic retrieval",
		Limit:         1,
		IncludeUnsafe: true,
	}, searcher)
	require.NoError(t, err)
	require.Len(t, results, 1)
	assert.Contains(t, filepath.ToSlash(results[0].Metadata["path"]), "semantic.md")
	assert.Equal(t, "configured-file-vector-index.json", results[0].Source.URI)
	assert.NotContains(t, results[0].Source.URI, dir)

	loaded, err := vector.LoadIndex(indexPath)
	require.NoError(t, err)
	assert.Equal(t, vector.VectorizerKindLexical, loaded.Vectorizer.Kind)
	assert.ElementsMatch(t, []string{vector.SourceKindFile, vector.SourceKindFile}, sourceMetadataKinds(loaded.Sources))
}

func TestBuildVectorRetrievalSearcherFallsBackToSeparateLexicalFileIndex(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	semanticPath := filepath.Join(dir, "semantic.md")
	require.NoError(t, os.WriteFile(semanticPath, []byte("Semantic retrieval memory for local RAG"), 0o600))

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "embedding endpoint unavailable", http.StatusInternalServerError)
	}))
	defer server.Close()

	indexPath := filepath.Join(dir, "embedding-file-vector-index.json")
	searcher, err := buildVectorRetrievalSearcher(context.TODO(), appState{
		cwd: dir,
		vectorConfig: appconfig.VectorConfig{
			Stores: map[string]appconfig.VectorizerConfig{
				vectorSearchVectorStore: {
					Vectorizer:     vector.VectorizerKindEmbedding,
					Model:          "retrieval-file-embed",
					BaseURL:        server.URL,
					FallbackPolicy: vector.VectorizerKindLexical,
					IndexPath:      indexPath,
				},
			},
		},
	}, retrievalCommandInput{
		Search:           "semantic retrieval",
		VectorIndexFiles: []string{semanticPath},
	})
	require.NoError(t, err)

	results, err := retrieval.Search(context.TODO(), retrieval.Query{
		Text:          "semantic retrieval",
		Limit:         1,
		IncludeUnsafe: true,
	}, searcher)
	require.NoError(t, err)
	require.Len(t, results, 1)
	assert.Equal(t, "lexical-file-vector-index", results[0].Scorer.Name)
	assert.NoFileExists(t, indexPath)
	assert.FileExists(t, lexicalFallbackIndexPath(indexPath))
}

func TestBuildVectorRetrievalSearcherRejectsRemoteFileEmbeddingWithoutConsent(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	semanticPath := filepath.Join(dir, "semantic.md")
	require.NoError(t, os.WriteFile(semanticPath, []byte("Semantic retrieval memory for local RAG"), 0o600))

	indexPath := filepath.Join(dir, "embedding-file-vector-index.json")
	_, err := buildVectorRetrievalSearcher(context.TODO(), appState{
		cwd: dir,
		vectorConfig: appconfig.VectorConfig{
			Stores: map[string]appconfig.VectorizerConfig{
				vectorSearchVectorStore: {
					Vectorizer: vector.VectorizerKindEmbedding,
					BaseURL:    privateRemoteEmbeddingEndpoint(),
					IndexPath:  indexPath,
				},
			},
		},
	}, retrievalCommandInput{
		Search:           "semantic retrieval",
		VectorIndexFiles: []string{semanticPath},
	})

	require.Error(t, err)
	require.ErrorContains(t, err, "remote file embedding endpoint")
	require.ErrorContains(t, err, "vector.workspace_allow_remote_embeddings")
	assert.NoFileExists(t, indexPath)
	assert.NoFileExists(t, lexicalFallbackIndexPath(indexPath))
}

func TestBuildVectorRetrievalSearcherUsesLexicalFallbackForRemoteFileEmbeddingWithoutConsent(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	semanticPath := filepath.Join(dir, "semantic.md")
	require.NoError(t, os.WriteFile(semanticPath, []byte("Semantic retrieval memory for local RAG"), 0o600))

	indexPath := filepath.Join(dir, "embedding-file-vector-index.json")
	searcher, err := buildVectorRetrievalSearcher(context.TODO(), appState{
		cwd: dir,
		vectorConfig: appconfig.VectorConfig{
			Stores: map[string]appconfig.VectorizerConfig{
				vectorSearchVectorStore: {
					Vectorizer:     vector.VectorizerKindEmbedding,
					BaseURL:        privateRemoteEmbeddingEndpoint(),
					FallbackPolicy: vector.VectorizerKindLexical,
					IndexPath:      indexPath,
				},
			},
		},
	}, retrievalCommandInput{
		Search:           "semantic retrieval",
		VectorIndexFiles: []string{semanticPath},
	})
	require.NoError(t, err)

	results, err := retrieval.Search(context.TODO(), retrieval.Query{
		Text:          "semantic retrieval",
		Limit:         1,
		IncludeUnsafe: true,
	}, searcher)
	require.NoError(t, err)
	require.Len(t, results, 1)
	assert.Equal(t, "lexical-file-vector-index", results[0].Scorer.Name)
	assert.NoFileExists(t, indexPath)
	assert.FileExists(t, lexicalFallbackIndexPath(indexPath))
}

func TestBuildVectorRetrievalSearcherFallsBackWhenReusableEmbeddingIndexCannotVectorizeQuery(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	semanticPath := filepath.Join(dir, "semantic.md")
	require.NoError(t, os.WriteFile(semanticPath, []byte("Semantic retrieval memory for local RAG"), 0o600))

	server := newAgentMemoryEmbeddingTestServer()
	indexPath := filepath.Join(dir, "embedding-file-vector-index.json")
	cfg := appconfig.VectorConfig{
		Stores: map[string]appconfig.VectorizerConfig{
			vectorSearchVectorStore: {
				Vectorizer:     vector.VectorizerKindEmbedding,
				Model:          "retrieval-file-embed",
				BaseURL:        server.URL,
				FallbackPolicy: vector.VectorizerKindLexical,
				IndexPath:      indexPath,
			},
		},
	}

	_, err := buildVectorRetrievalSearcher(context.TODO(), appState{
		cwd:          dir,
		vectorConfig: cfg,
	}, retrievalCommandInput{
		Search:           "semantic retrieval",
		VectorIndexFiles: []string{semanticPath},
	})
	require.NoError(t, err)
	require.FileExists(t, indexPath)

	server.Close()

	searcher, err := buildVectorRetrievalSearcher(context.TODO(), appState{
		cwd:          dir,
		vectorConfig: cfg,
	}, retrievalCommandInput{
		Search:           "semantic retrieval",
		VectorIndexFiles: []string{semanticPath},
	})
	require.NoError(t, err)

	results, err := retrieval.Search(context.TODO(), retrieval.Query{
		Text:          "semantic retrieval",
		Limit:         1,
		IncludeUnsafe: true,
	}, searcher)
	require.NoError(t, err)
	require.Len(t, results, 1)
	assert.Equal(t, "lexical-file-vector-index", results[0].Scorer.Name)
	assert.FileExists(t, lexicalFallbackIndexPath(indexPath))
}

func TestSourceVectorSettingsResolvesRelativeIndexPathAgainstRoot(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	settings, err := sourceVectorSettings(root, appconfig.VectorConfig{
		Sources: map[string]appconfig.VectorizerConfig{
			vector.SourceKindSession: {
				Vectorizer: vector.VectorizerKindLexical,
				IndexPath:  "./session-vector-index.json",
			},
		},
	}, vector.SourceKindSession, sourceVectorSessionIndex)
	require.NoError(t, err)

	assert.Equal(t, filepath.Join(root, "session-vector-index.json"), settings.IndexPath)
}

func TestSourceVectorIndexRequestedUsesExplicitLexicalSourceConfig(t *testing.T) {
	t.Parallel()

	requested, err := sourceVectorIndexRequested(appconfig.VectorConfig{}, vector.SourceKindGitHistory)
	require.NoError(t, err)
	assert.False(t, requested)

	requested, err = sourceVectorIndexRequested(appconfig.VectorConfig{
		Sources: map[string]appconfig.VectorizerConfig{
			"git-history": {
				Vectorizer: vector.VectorizerKindLexical,
			},
		},
	}, vector.SourceKindGitHistory)
	require.NoError(t, err)
	assert.True(t, requested)

	requested, err = sourceVectorIndexRequested(appconfig.VectorConfig{
		Vectorizer: vector.VectorizerKindEmbedding,
	}, vector.SourceKindSession)
	require.NoError(t, err)
	assert.True(t, requested)
}

func TestRetrievalSearcherUsesPersistedSessionVectorSourceConfig(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	store := session.NewStore(filepath.Join(dir, "sessions"))
	saved := session.New("test-model", []llm.Message{
		{Role: llm.RoleUser, Content: "Semantic retrieval session memory for local RAG"},
		{Role: llm.RoleAssistant, Content: "Use persisted embeddings for session recall."},
	})
	saved.ID = "session-vector-test"
	saved.Title = "Session vector test"
	saved.DefaultAgent = testReviewerName
	require.NoError(t, store.Save(saved))

	server := newAgentMemoryEmbeddingTestServer()
	defer server.Close()

	indexPath := filepath.Join(dir, "session-vector-index.json")
	searcher, err := retrievalSearcher(context.TODO(), appState{
		cwd:          dir,
		sessionStore: store,
		vectorConfig: appconfig.VectorConfig{
			Sources: map[string]appconfig.VectorizerConfig{
				vector.SourceKindSession: {
					Vectorizer: vector.VectorizerKindEmbedding,
					Model:      "session-test-embed",
					BaseURL:    server.URL,
					IndexPath:  indexPath,
				},
			},
		},
	}, retrievalCommandInput{}, retrieval.SourceSession)
	require.NoError(t, err)
	require.NotNil(t, searcher)

	results, err := retrieval.Search(context.TODO(), retrieval.Query{
		Text:          "semantic retrieval",
		Limit:         1,
		IncludeUnsafe: true,
	}, searcher)
	require.NoError(t, err)
	require.Len(t, results, 1)
	assert.Equal(t, retrieval.SourceSession, results[0].Source.Type)
	assert.Contains(t, results[0].DocumentID, "sessions/session-vector-test")
	assert.Equal(t, "embedding-session-ann", results[0].Scorer.Name)
	assert.NotZero(t, results[0].Freshness.SourceUpdatedAt)

	loaded, err := vector.LoadIndex(indexPath)
	require.NoError(t, err)
	assert.Equal(t, vector.VectorizerKindEmbedding, loaded.Vectorizer.Kind)
	assert.ElementsMatch(t, []string{vector.SourceKindSession}, sourceMetadataKinds(loaded.Sources))
	assert.NotEmpty(t, loaded.Documents[0].Metadata[retrieval.MetadataSourceUpdatedAt])
}

func TestBuildSessionVectorRetrievalSearcherFallbackPersistsLexicalIndex(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	store := session.NewStore(filepath.Join(dir, "sessions"))
	saved := session.New("test-model", []llm.Message{
		{Role: llm.RoleUser, Content: "Semantic retrieval session memory for local RAG"},
		{Role: llm.RoleAssistant, Content: "Persist fallback indexes for local session recall."},
	})
	saved.ID = "session-vector-fallback-test"
	saved.Title = "Session vector fallback test"
	require.NoError(t, store.Save(saved))

	indexPath := filepath.Join(dir, "session-vector-index.json")
	searcher, err := buildSessionVectorRetrievalSearcher(context.TODO(), appState{
		cwd:          dir,
		sessionStore: store,
		vectorConfig: appconfig.VectorConfig{
			Sources: map[string]appconfig.VectorizerConfig{
				vector.SourceKindSession: {
					Vectorizer:     vector.VectorizerKindEmbedding,
					BaseURL:        privateRemoteEmbeddingEndpoint(),
					FallbackPolicy: vector.VectorizerKindLexical,
					IndexPath:      indexPath,
				},
			},
		},
	})
	require.NoError(t, err)
	require.NotNil(t, searcher)

	results, err := retrieval.Search(context.TODO(), retrieval.Query{
		Text:          "semantic retrieval",
		Limit:         1,
		IncludeUnsafe: true,
	}, searcher)
	require.NoError(t, err)
	require.Len(t, results, 1)
	assert.Equal(t, retrieval.SourceSession, results[0].Source.Type)
	assert.Equal(t, "lexical-session-ann", results[0].Scorer.Name)
	assert.Equal(t, "session-vector-index.lexical.json", results[0].Source.URI)
	assert.NoFileExists(t, indexPath)
	assert.FileExists(t, lexicalFallbackIndexPath(indexPath))

	loaded, err := vector.LoadIndex(lexicalFallbackIndexPath(indexPath))
	require.NoError(t, err)
	assert.Equal(t, vector.VectorizerKindLexical, loaded.Vectorizer.Kind)
	assert.ElementsMatch(t, []string{vector.SourceKindSession}, sourceMetadataKinds(loaded.Sources))
}

func TestBuildGitHistoryVectorRetrievalSearcherUsesPersistedSourceConfig(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	server := newAgentMemoryEmbeddingTestServer()
	defer server.Close()

	indexPath := filepath.Join(dir, "git-history-vector-index.json")
	commits := []githistory.Commit{
		{
			Hash:       "abc123",
			AuthorName: "Dev",
			Date:       time.Unix(10, 0).UTC(),
			Subject:    "Semantic git history for local RAG",
			Body:       "Persist embeddings for commit retrieval.",
			Files:      []string{"pkg/vector/source_index.go"},
		},
		{
			Hash:    "def456",
			Subject: "Shell timeout cleanup",
			Files:   []string{"pkg/shell/shell.go"},
		},
	}

	searcher, err := buildGitHistoryVectorRetrievalSearcher(context.TODO(), appState{
		cwd: dir,
		vectorConfig: appconfig.VectorConfig{
			Sources: map[string]appconfig.VectorizerConfig{
				vector.SourceKindGitHistory: {
					Vectorizer: vector.VectorizerKindEmbedding,
					Model:      "git-history-test-embed",
					BaseURL:    server.URL,
					IndexPath:  indexPath,
				},
			},
		},
	}, commits)
	require.NoError(t, err)
	require.NotNil(t, searcher)

	results, err := retrieval.Search(context.TODO(), retrieval.Query{
		Text:  "semantic retrieval",
		Limit: 1,
	}, searcher)
	require.NoError(t, err)
	require.Len(t, results, 1)
	assert.Equal(t, retrieval.SourceGitHistory, results[0].Source.Type)
	assert.Contains(t, results[0].DocumentID, "git/abc123")
	assert.Equal(t, "embedding-git-history-ann", results[0].Scorer.Name)
	assert.Equal(t, commits[0].Date, results[0].Freshness.SourceUpdatedAt)

	loaded, err := vector.LoadIndex(indexPath)
	require.NoError(t, err)
	assert.Equal(t, vector.VectorizerKindEmbedding, loaded.Vectorizer.Kind)
	assert.NotEmpty(t, loaded.Sources)
	for _, kind := range sourceMetadataKinds(loaded.Sources) {
		assert.Equal(t, vector.SourceKindGitHistory, kind)
	}
}

func TestBuildGitHistoryVectorRetrievalSearcherFallbackPersistsLexicalIndex(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()

	indexPath := filepath.Join(dir, "git-history-vector-index.json")
	commits := []githistory.Commit{
		{
			Hash:    "abc123",
			Subject: "Semantic git history for local RAG",
			Body:    "Persist lexical fallback indexes for commit retrieval.",
			Files:   []string{"pkg/vector/source_index.go"},
		},
		{
			Hash:    "def456",
			Subject: "Shell timeout cleanup",
			Files:   []string{"pkg/shell/shell.go"},
		},
	}

	searcher, err := buildGitHistoryVectorRetrievalSearcher(context.TODO(), appState{
		cwd: dir,
		vectorConfig: appconfig.VectorConfig{
			Sources: map[string]appconfig.VectorizerConfig{
				vector.SourceKindGitHistory: {
					Vectorizer:     vector.VectorizerKindEmbedding,
					BaseURL:        privateRemoteEmbeddingEndpoint(),
					FallbackPolicy: vector.VectorizerKindLexical,
					IndexPath:      indexPath,
				},
			},
		},
	}, commits)
	require.NoError(t, err)
	require.NotNil(t, searcher)

	results, err := retrieval.Search(context.TODO(), retrieval.Query{
		Text:          "semantic retrieval",
		Limit:         1,
		IncludeUnsafe: true,
	}, searcher)
	require.NoError(t, err)
	require.Len(t, results, 1)
	assert.Equal(t, retrieval.SourceGitHistory, results[0].Source.Type)
	assert.Equal(t, "lexical-git-history-ann", results[0].Scorer.Name)
	assert.Equal(t, "git-history-vector-index.lexical.json", results[0].Source.URI)
	assert.NoFileExists(t, indexPath)
	assert.FileExists(t, lexicalFallbackIndexPath(indexPath))

	loaded, err := vector.LoadIndex(lexicalFallbackIndexPath(indexPath))
	require.NoError(t, err)
	assert.Equal(t, vector.VectorizerKindLexical, loaded.Vectorizer.Kind)
	assert.ElementsMatch(t, []string{vector.SourceKindGitHistory, vector.SourceKindGitHistory}, sourceMetadataKinds(loaded.Sources))
}

func TestBuildADRVectorRetrievalSearcherUsesPersistedSourceConfig(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	adrDir := filepath.Join(dir, "docs", "adr")
	require.NoError(t, os.MkdirAll(adrDir, 0o750))
	require.NoError(t, os.WriteFile(
		filepath.Join(adrDir, "0001-local-rag.md"),
		[]byte("# 0001 Local RAG\n\nSemantic retrieval ADR keeps embeddings local."),
		0o600,
	))
	require.NoError(t, os.WriteFile(
		filepath.Join(adrDir, "0002-shell-timeouts.md"),
		[]byte("# 0002 Shell timeouts\n\nCommand timeout policy for local execution."),
		0o600,
	))

	server := newAgentMemoryEmbeddingTestServer()
	defer server.Close()

	indexPath := filepath.Join(dir, "adr-vector-index.json")
	searcher, err := buildADRVectorRetrievalSearcher(context.TODO(), appState{
		cwd: dir,
		vectorConfig: appconfig.VectorConfig{
			Sources: map[string]appconfig.VectorizerConfig{
				vector.SourceKindADR: {
					Vectorizer: vector.VectorizerKindEmbedding,
					Model:      "adr-test-embed",
					BaseURL:    server.URL,
					IndexPath:  indexPath,
				},
			},
		},
	})
	require.NoError(t, err)
	require.NotNil(t, searcher)

	results, err := retrieval.Search(context.TODO(), retrieval.Query{
		Text:          "semantic retrieval",
		Limit:         1,
		IncludeUnsafe: true,
	}, searcher)
	require.NoError(t, err)
	require.Len(t, results, 1)
	assert.Equal(t, retrieval.SourceADR, results[0].Source.Type)
	assert.Contains(t, filepath.ToSlash(results[0].DocumentID), "docs/adr/0001-local-rag.md")
	assert.Equal(t, "embedding-adr-ann", results[0].Scorer.Name)
	assert.Equal(t, "0001-local-rag", results[0].Metadata["adr_id"])
	assert.NotZero(t, results[0].Freshness.SourceUpdatedAt)

	loaded, err := vector.LoadIndex(indexPath)
	require.NoError(t, err)
	assert.Equal(t, vector.VectorizerKindEmbedding, loaded.Vectorizer.Kind)
	assert.ElementsMatch(t, []string{vector.SourceKindADR, vector.SourceKindADR}, sourceMetadataKinds(loaded.Sources))
	assert.NotEmpty(t, loaded.Documents[0].Metadata[retrieval.MetadataSourceUpdatedAt])
}

func TestBuildADRVectorRetrievalSearcherFallbackUsesLexicalIndexURI(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	adrDir := filepath.Join(dir, "docs", "adr")
	require.NoError(t, os.MkdirAll(adrDir, 0o750))
	require.NoError(t, os.WriteFile(
		filepath.Join(adrDir, "0001-local-rag.md"),
		[]byte("# 0001 Local RAG\n\nSemantic retrieval ADR keeps embeddings local."),
		0o600,
	))

	indexPath := filepath.Join(dir, "adr-vector-index.json")
	searcher, err := buildADRVectorRetrievalSearcher(context.TODO(), appState{
		cwd: dir,
		vectorConfig: appconfig.VectorConfig{
			Sources: map[string]appconfig.VectorizerConfig{
				vector.SourceKindADR: {
					Vectorizer:        vector.VectorizerKindEmbedding,
					BaseURL:           privateRemoteEmbeddingEndpoint(),
					FallbackPolicy:    vector.VectorizerKindLexical,
					IndexPath:         indexPath,
					ChunkMaxRunes:     400,
					ChunkOverlapRunes: 40,
				},
			},
		},
	})
	require.NoError(t, err)
	require.NotNil(t, searcher)

	results, err := retrieval.Search(context.TODO(), retrieval.Query{
		Text:  "semantic retrieval",
		Limit: 1,
	}, searcher)
	require.NoError(t, err)
	require.Len(t, results, 1)
	assert.Equal(t, "lexical-adr-ann", results[0].Scorer.Name)
	assert.Equal(t, "adr-vector-index.lexical.json", results[0].Source.URI)
	assert.NoFileExists(t, indexPath)
	assert.FileExists(t, lexicalFallbackIndexPath(indexPath))
}

func TestBuildADRVectorRetrievalSearcherClearsIndexWhenADRsDeleted(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	indexPath := filepath.Join(dir, "adr-vector-index.json")
	writeSourceVectorIndex(t, indexPath, []vector.Source{{
		Kind: vector.SourceKindADR,
		Path: "docs/adr/0001-deleted.md",
		Text: "Deleted ADR should be removed from persisted local RAG index.",
	}})

	searcher, err := buildADRVectorRetrievalSearcher(context.TODO(), appState{
		cwd: dir,
		vectorConfig: appconfig.VectorConfig{
			Sources: map[string]appconfig.VectorizerConfig{
				vector.SourceKindADR: {
					Vectorizer: vector.VectorizerKindLexical,
					IndexPath:  indexPath,
				},
			},
		},
	})
	require.NoError(t, err)
	assert.Nil(t, searcher)
	assert.NoFileExists(t, indexPath)
}

func TestADRVectorSourcesSkipsSymlinkFiles(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	adrDir := filepath.Join(dir, "docs", "adr")
	require.NoError(t, os.MkdirAll(adrDir, 0o750))
	require.NoError(t, os.WriteFile(
		filepath.Join(adrDir, "0001-real.md"),
		[]byte("# 0001 Real ADR\n\nKeep local RAG indexes persistent."),
		0o600,
	))
	outside := filepath.Join(dir, "outside.md")
	require.NoError(t, os.WriteFile(outside, []byte("# Outside\n\nDo not follow symlinked ADR files."), 0o600))
	if err := os.Symlink(outside, filepath.Join(adrDir, "0002-linked.md")); err != nil {
		t.Skipf("symlink unavailable: %v", err)
	}

	sources, err := adrVectorSources(context.TODO(), dir)
	require.NoError(t, err)

	require.Len(t, sources, 1)
	assert.Equal(t, vector.SourceKindADR, sources[0].Kind)
	assert.Equal(t, "docs/adr/0001-real.md", sources[0].Path)
	assert.NotContains(t, sources[0].Text, "Do not follow symlinked ADR files")
}

func TestBuildSessionVectorRetrievalSearcherClearsIndexWhenSessionsDeleted(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	indexPath := filepath.Join(dir, "session-vector-index.json")
	writeSourceVectorIndex(t, indexPath, []vector.Source{{
		Kind: vector.SourceKindSession,
		Path: "sessions/deleted-session",
		Text: "Deleted session should be removed from persisted local RAG index.",
	}})

	searcher, err := buildSessionVectorRetrievalSearcher(context.TODO(), appState{
		cwd:          dir,
		sessionStore: session.NewStore(filepath.Join(dir, "sessions")),
		vectorConfig: appconfig.VectorConfig{
			Sources: map[string]appconfig.VectorizerConfig{
				vector.SourceKindSession: {
					Vectorizer: vector.VectorizerKindLexical,
					IndexPath:  indexPath,
				},
			},
		},
	})
	require.NoError(t, err)
	assert.Nil(t, searcher)
	assert.NoFileExists(t, indexPath)
}

func TestBuildSessionVectorRetrievalSearcherClearsFallbackIndexWhenSessionsDeleted(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	indexPath := filepath.Join(dir, "session-vector-index.json")
	staleSources := []vector.Source{{
		Kind: vector.SourceKindSession,
		Path: "sessions/deleted-session",
		Text: "Deleted session should be removed from embedding and fallback local RAG indexes.",
	}}
	writeSourceVectorIndex(t, indexPath, staleSources)
	writeSourceVectorIndex(t, lexicalFallbackIndexPath(indexPath), staleSources)

	searcher, err := buildSessionVectorRetrievalSearcher(context.TODO(), appState{
		cwd:          dir,
		sessionStore: session.NewStore(filepath.Join(dir, "sessions")),
		vectorConfig: appconfig.VectorConfig{
			Sources: map[string]appconfig.VectorizerConfig{
				vector.SourceKindSession: {
					Vectorizer:     vector.VectorizerKindEmbedding,
					BaseURL:        privateRemoteEmbeddingEndpoint(),
					FallbackPolicy: vector.VectorizerKindLexical,
					IndexPath:      indexPath,
				},
			},
		},
	})
	require.NoError(t, err)
	assert.Nil(t, searcher)
	assert.NoFileExists(t, indexPath)
	assert.NoFileExists(t, lexicalFallbackIndexPath(indexPath))
}

func TestBuildSessionVectorRetrievalSearcherRefusesToClearDifferentSourceIndex(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	indexPath := filepath.Join(dir, "shared-vector-index.json")
	writeSourceVectorIndex(t, indexPath, []vector.Source{{
		Kind: vector.SourceKindFile,
		Path: "docs/auth.md",
		Text: "OAuth token rotation notes for file retrieval.",
	}})

	searcher, err := buildSessionVectorRetrievalSearcher(context.TODO(), appState{
		cwd:          dir,
		sessionStore: session.NewStore(filepath.Join(dir, "sessions")),
		vectorConfig: appconfig.VectorConfig{
			Sources: map[string]appconfig.VectorizerConfig{
				vector.SourceKindSession: {
					Vectorizer: vector.VectorizerKindLexical,
					IndexPath:  indexPath,
				},
			},
		},
	})
	require.Error(t, err)
	assert.Nil(t, searcher)
	assert.Contains(t, err.Error(), "refusing to clear session vector index")
	assert.FileExists(t, indexPath)
}

func TestBuildSessionVectorRetrievalSearcherRefusesToRefreshDifferentSourceIndex(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	indexPath := filepath.Join(dir, "shared-vector-index.json")
	writeSourceVectorIndex(t, indexPath, []vector.Source{{
		Kind: vector.SourceKindFile,
		Path: "docs/auth.md",
		Text: "OAuth token rotation notes for file retrieval.",
	}})

	store := session.NewStore(filepath.Join(dir, "sessions"))
	saved := session.New("test-model", []llm.Message{
		{Role: llm.RoleUser, Content: "Session source should not clobber a file vector index."},
		{Role: llm.RoleAssistant, Content: "Persist source metadata by family before refreshing."},
	})
	saved.ID = "active-session"
	require.NoError(t, store.Save(saved))

	searcher, err := buildSessionVectorRetrievalSearcher(context.TODO(), appState{
		cwd:          dir,
		sessionStore: store,
		vectorConfig: appconfig.VectorConfig{
			Sources: map[string]appconfig.VectorizerConfig{
				vector.SourceKindSession: {
					Vectorizer: vector.VectorizerKindLexical,
					IndexPath:  indexPath,
				},
			},
		},
	})
	require.Error(t, err)
	assert.Nil(t, searcher)
	assert.Contains(t, err.Error(), "refusing to refresh session vector index")

	loaded, loadErr := vector.LoadIndex(indexPath)
	require.NoError(t, loadErr)
	assert.ElementsMatch(t, []string{vector.SourceKindFile}, sourceMetadataKinds(loaded.Sources))
}

func TestBuildGitHistoryVectorRetrievalSearcherClearsIndexWhenCommitsDeleted(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	indexPath := filepath.Join(dir, "git-history-vector-index.json")
	writeSourceVectorIndex(t, indexPath, []vector.Source{{
		Kind: vector.SourceKindGitHistory,
		Path: "git/deletedcommit",
		Text: "Deleted commit should be removed from persisted local RAG index.",
	}})

	searcher, err := buildGitHistoryVectorRetrievalSearcher(context.TODO(), appState{
		cwd: dir,
		vectorConfig: appconfig.VectorConfig{
			Sources: map[string]appconfig.VectorizerConfig{
				vector.SourceKindGitHistory: {
					Vectorizer: vector.VectorizerKindLexical,
					IndexPath:  indexPath,
				},
			},
		},
	}, nil)
	require.NoError(t, err)
	assert.Nil(t, searcher)
	assert.NoFileExists(t, indexPath)
}

func writeSourceVectorIndex(t *testing.T, indexPath string, sources []vector.Source) {
	t.Helper()

	vectorizer, err := vector.NewTextVectorizer(16)
	require.NoError(t, err)

	refresh, err := vector.RefreshSourceIndex(context.TODO(), vector.SourceIndexOptions{
		IndexPath:          indexPath,
		Sources:            sources,
		Vectorizer:         vectorizer,
		VectorizerMetadata: vectorizer.Metadata(),
		Chunk:              vector.ChunkOptions{MaxRunes: 200, OverlapRunes: 20},
	})
	require.NoError(t, err)
	require.NotNil(t, refresh.Index)
	require.FileExists(t, indexPath)
}

func sourceMetadataKinds(sources []vector.SourceMetadata) []string {
	out := make([]string, 0, len(sources))
	for _, source := range sources {
		out = append(out, source.Kind)
	}

	return out
}

func newAgentMemoryEmbeddingTestServer() *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Input any    `json:"input"`
			Model string `json:"model"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)

			return
		}

		text := strings.ToLower(fmt.Sprint(req.Input))
		embedding := []float64{0, 1}
		if strings.Contains(text, "semantic") || strings.Contains(text, "retrieval") || strings.Contains(text, "rag") {
			embedding = []float64{1, 0}
		}

		w.Header().Set("Content-Type", "application/json")
		if err := json.NewEncoder(w).Encode(map[string]any{
			"embeddings": [][]float64{embedding},
		}); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
		}
	}))
}

func TestFormatAgentMemoryResult(t *testing.T) {
	t.Parallel()

	got := formatAgentMemoryResult(agentmemory.Result{
		Document: agentmemory.Document{
			ID:       "docs/memory.md",
			Path:     "docs/memory.md",
			Metadata: map[string]string{"kind": "note"},
		},
		Score: 0.5,
	})

	want := "docs/memory.md\tscore=0.5000\tpath=docs/memory.md\tkind=note"
	if got != want {
		require.Failf(t, "unexpected agent memory result format", "got %q, want %q", got, want)
	}
}

func TestFormatMemoryResultRedactsLineAndSnippet(t *testing.T) {
	t.Parallel()

	const secret = "sk-1234567890abcdefSECRET"

	got := formatMemoryResult(memory.Result{
		Document: memory.Document{
			ID:       "doc-" + secret,
			Path:     "notes/" + secret + ".txt",
			Metadata: map[string]string{"kind": "message"},
		},
		Snippet: "token=" + secret,
		Score:   1,
	})

	assert.NotContains(t, got, secret)
	assert.Contains(t, got, "[REDACTED:")
}

//nolint:paralleltest // Uses t.Chdir to exercise default repo-scope resolution.
func TestBuildMemoryStore_DefaultRepoScopeDoesNotIndexAllSessions(t *testing.T) {
	dir := t.TempDir()
	t.Chdir(dir)

	store := session.NewStore(filepath.Join(dir, "sessions"))
	local := session.New("gpt-test", []llm.Message{{Role: llm.RoleUser, Content: "OAuth local repo note"}})
	local.ID = localMemorySessionID
	local.WorktreePath = dir
	require.NoError(t, store.Save(local))

	other := session.New("gpt-test", []llm.Message{{Role: llm.RoleUser, Content: "OAuth other repo note"}})
	other.ID = otherMemoryName
	other.WorktreePath = filepath.Join(dir, otherMemoryName)
	require.NoError(t, store.Save(other))

	legacyNoRepo := session.New("gpt-test", []llm.Message{{Role: llm.RoleUser, Content: "OAuth legacy no repo note"}})
	legacyNoRepo.ID = "legacy-no-repo"
	require.NoError(t, store.Save(legacyNoRepo))

	mem, err := buildMemoryStore(store, cliOptions{memorySearch: "oauth"})
	require.NoError(t, err)
	assert.Equal(t, memory.ScopeRepo, mem.Corpus.Scope)
	assert.Equal(t, []string{"legacy-no-repo", localMemorySessionID}, mem.Corpus.SessionIDs)

	results, err := mem.Search("oauth", 10)
	require.NoError(t, err)
	require.Len(t, results, 2)
	for _, result := range results {
		assert.NotEqual(t, "session/other/message/0", result.Document.ID)
	}

	global, err := buildMemoryStore(store, cliOptions{memorySearch: "oauth", memoryGlobal: true})
	require.NoError(t, err)
	assert.ElementsMatch(t, []string{"legacy-no-repo", localMemorySessionID, otherMemoryName}, global.Corpus.SessionIDs)
}

//nolint:paralleltest // Uses t.Chdir to exercise default repo-scope resolution.
func TestBuildMemoryStore_DefaultRepoScopeSkipsOutOfScopeBeforeFullLoad(t *testing.T) {
	dir := t.TempDir()
	t.Chdir(dir)

	store := session.NewStore(filepath.Join(dir, "sessions"))
	local := session.New("gpt-test", []llm.Message{{Role: llm.RoleUser, Content: "OAuth local repo note"}})
	local.ID = localMemorySessionID
	local.WorktreePath = dir
	require.NoError(t, store.Save(local))

	now := time.Now().UTC()
	malformedOther, err := json.Marshal(map[string]any{
		"id":            "malformed-other",
		"created_at":    now,
		"updated_at":    now,
		"worktree_path": filepath.Join(dir, otherMemoryName),
		"messages": []map[string]any{
			{"role": "user", "content": 123},
		},
	})
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(store.Path("malformed-other"), append(malformedOther, '\n'), 0o600))

	mem, err := buildMemoryStore(store, cliOptions{memorySearch: "oauth"})
	require.NoError(t, err)
	assert.Equal(t, []string{localMemorySessionID}, mem.Corpus.SessionIDs)
}

//nolint:paralleltest // Uses t.Chdir to exercise git-root default repo-scope resolution.
func TestBuildMemoryStore_DefaultRepoScopeUsesGitRootFromSubdir(t *testing.T) {
	dir := t.TempDir()
	require.NoError(t, os.Mkdir(filepath.Join(dir, ".git"), 0o750))

	subdir := filepath.Join(dir, "cmd", "atteler")
	require.NoError(t, os.MkdirAll(subdir, 0o750))
	t.Chdir(subdir)

	store := session.NewStore(filepath.Join(dir, ".atteler", "sessions"))
	local := session.New("gpt-test", []llm.Message{{Role: llm.RoleUser, Content: "OAuth root repo note"}})
	local.ID = "repo-root"
	local.WorktreePath = dir
	require.NoError(t, store.Save(local))

	other := session.New("gpt-test", []llm.Message{{Role: llm.RoleUser, Content: "OAuth other repo note"}})
	other.ID = otherMemoryName
	other.WorktreePath = filepath.Join(t.TempDir(), "other-repo")
	require.NoError(t, store.Save(other))

	mem, err := buildMemoryStore(store, cliOptions{memorySearch: "oauth"})
	require.NoError(t, err)
	assert.Equal(t, memory.ScopeRepo, mem.Corpus.Scope)
	assert.Equal(t, cleanMemoryPath(dir), mem.Corpus.RepoPath)
	assert.Equal(t, []string{"repo-root"}, mem.Corpus.SessionIDs)
}

func TestBuildMemoryStore_ExplicitRepoPathUsesGitRootFromSubdir(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	require.NoError(t, os.Mkdir(filepath.Join(dir, ".git"), 0o750))

	subdir := filepath.Join(dir, "cmd", "atteler")
	require.NoError(t, os.MkdirAll(subdir, 0o750))

	store := session.NewStore(filepath.Join(dir, ".atteler", "sessions"))
	local := session.New("gpt-test", []llm.Message{{Role: llm.RoleUser, Content: "OAuth explicit repo note"}})
	local.ID = "repo-root"
	local.WorktreePath = dir
	require.NoError(t, store.Save(local))

	other := session.New("gpt-test", []llm.Message{{Role: llm.RoleUser, Content: "OAuth other repo note"}})
	other.ID = otherMemoryName
	other.WorktreePath = filepath.Join(t.TempDir(), "other-repo")
	require.NoError(t, store.Save(other))

	mem, err := buildMemoryStore(store, cliOptions{memorySearch: "oauth", memoryRepoPath: subdir})
	require.NoError(t, err)
	assert.Equal(t, memory.ScopeRepo, mem.Corpus.Scope)
	assert.Equal(t, cleanMemoryPath(dir), mem.Corpus.RepoPath)
	assert.Equal(t, []string{"repo-root"}, mem.Corpus.SessionIDs)
}

func TestBuildMemoryStore_RepoScopePersistsInferredRepoForLegacySession(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	storePath := filepath.Join(dir, "memory.json")
	store := session.NewStore(filepath.Join(dir, ".atteler", "sessions"))

	legacy := session.New("gpt-test", []llm.Message{{Role: llm.RoleUser, Content: "OAuth legacy repo note"}})
	legacy.ID = "legacy-local"
	writeMemorySessionFixture(t, store, legacy)

	mem, err := buildMemoryStore(store, cliOptions{memoryRepoPath: dir, memorySearch: "oauth"})
	require.NoError(t, err)
	require.Len(t, mem.Documents, 2)
	for _, doc := range mem.Documents {
		require.NotNil(t, doc.Provenance)
		assert.Equal(t, cleanMemoryPath(dir), cleanMemoryPath(doc.Provenance.RepoPath))
	}
	require.NoError(t, mem.Save(storePath))

	reloaded, err := buildMemoryStore(session.NewStore(filepath.Join(t.TempDir(), "missing-sessions")), cliOptions{
		memoryStorePath: storePath,
		memoryRepoPath:  dir,
		memorySearch:    "oauth",
	})
	require.NoError(t, err)
	assert.Equal(t, memory.ScopeRepo, reloaded.Corpus.Scope)
	assert.Equal(t, []string{"legacy-local"}, reloaded.Corpus.SessionIDs)

	results, err := reloaded.Search("oauth", 10)
	require.NoError(t, err)
	require.NotEmpty(t, results)
}

//nolint:paralleltest // Uses t.Chdir to exercise default repo-scope resolution.
func TestBuildMemoryStore_DefaultRepoScopeExcludesLegacySessionsOutsideRepo(t *testing.T) {
	dir := t.TempDir()
	t.Chdir(dir)

	store := session.NewStore(filepath.Join(t.TempDir(), "global-sessions"))
	legacyNoRepo := session.New("gpt-test", []llm.Message{{Role: llm.RoleUser, Content: "OAuth global legacy note"}})
	legacyNoRepo.ID = "legacy-no-repo"
	require.NoError(t, store.Save(legacyNoRepo))

	mem, err := buildMemoryStore(store, cliOptions{memorySearch: "oauth"})
	require.NoError(t, err)
	assert.Equal(t, memory.ScopeRepo, mem.Corpus.Scope)
	assert.Empty(t, mem.Corpus.SessionIDs)

	results, err := mem.Search("oauth", 10)
	require.NoError(t, err)
	assert.Empty(t, results)
}

func TestBuildMemoryStore_StorePathDoesNotImplicitlyIndexSessions(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	storePath := filepath.Join(dir, "memory.json")
	store := session.NewStore(filepath.Join(dir, "sessions"))
	saved := session.New("gpt-test", []llm.Message{{Role: llm.RoleUser, Content: "OAuth saved session note"}})
	saved.ID = "saved-session"
	saved.WorktreePath = dir
	require.NoError(t, store.Save(saved))

	mem, err := buildMemoryStore(store, cliOptions{
		memoryStorePath: storePath,
		memorySearch:    "oauth",
	})
	require.NoError(t, err)
	assert.Empty(t, mem.Documents)
	assert.Empty(t, mem.Corpus.SessionIDs)

	results, err := mem.Search("oauth", 10)
	require.NoError(t, err)
	assert.Empty(t, results)
}

func TestBuildMemoryStore_EmptyStorePathDoesNotImplicitlyIndexSessions(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	storePath := filepath.Join(dir, "memory.json")
	require.NoError(t, os.WriteFile(storePath, []byte(" \n"), 0o600))
	store := session.NewStore(filepath.Join(dir, "sessions"))
	saved := session.New("gpt-test", []llm.Message{{Role: llm.RoleUser, Content: "OAuth saved session note"}})
	saved.ID = "saved-session"
	saved.WorktreePath = dir
	require.NoError(t, store.Save(saved))

	mem, err := buildMemoryStore(store, cliOptions{
		memoryStorePath: storePath,
		memorySearch:    "oauth",
	})
	require.NoError(t, err)
	assert.Empty(t, mem.Documents)
	assert.Empty(t, mem.Corpus.SessionIDs)
	assert.Equal(t, memory.ScopeRepo, mem.Corpus.Scope)
}

//nolint:paralleltest // Uses t.Chdir to verify the implicit repo scope for store-backed search.
func TestBuildMemoryStore_DefaultStoreSearchConstrainsLoadedStoreToRepo(t *testing.T) {
	dir := t.TempDir()
	t.Chdir(dir)

	otherRepo := filepath.Join(t.TempDir(), "other-repo")
	storePath := filepath.Join(dir, "memory.json")

	mem := memory.NewStore()
	require.NoError(t, mem.Add(memory.Document{
		ID:         "session/local/message/0",
		Text:       "OAuth local repo note",
		Metadata:   map[string]string{"session_id": localMemorySessionID, "repo_path": dir},
		Provenance: &memory.Provenance{SessionID: localMemorySessionID, RepoPath: dir},
	}))
	require.NoError(t, mem.Add(memory.Document{
		ID:         "session/other/message/0",
		Text:       "OAuth other repo note",
		Metadata:   map[string]string{"session_id": otherMemoryName, "repo_path": otherRepo},
		Provenance: &memory.Provenance{SessionID: otherMemoryName, RepoPath: otherRepo},
	}))
	require.NoError(t, mem.Save(storePath))

	built, err := buildMemoryStore(nil, cliOptions{
		memoryStorePath: storePath,
		memorySearch:    "oauth",
	})
	require.NoError(t, err)
	assert.Equal(t, memory.ScopeRepo, built.Corpus.Scope)
	assert.True(t, sameMemoryPath(built.Corpus.RepoPath, dir), "repo path = %q, want %q", built.Corpus.RepoPath, dir)
	assert.Equal(t, []string{localMemorySessionID}, built.Corpus.SessionIDs)

	results, err := built.Search("oauth", 10)
	require.NoError(t, err)
	require.Len(t, results, 1)
	assert.Equal(t, "session/local/message/0", results[0].Document.ID)
}

//nolint:paralleltest // Uses t.Chdir to ensure explicit store scope bypasses implicit repo filtering.
func TestBuildMemoryStore_ExplicitStoreScopeSearchesWholeStore(t *testing.T) {
	dir := t.TempDir()
	t.Chdir(dir)

	otherRepo := filepath.Join(t.TempDir(), "other-repo")
	storePath := filepath.Join(dir, "memory.json")

	mem := memory.NewStore()
	require.NoError(t, mem.Add(memory.Document{
		ID:         "session/local/message/0",
		Text:       "OAuth local repo note",
		Metadata:   map[string]string{"session_id": localMemorySessionID, "repo_path": dir},
		Provenance: &memory.Provenance{SessionID: localMemorySessionID, RepoPath: dir},
	}))
	require.NoError(t, mem.Add(memory.Document{
		ID:         "session/other/message/0",
		Text:       "OAuth other repo note",
		Metadata:   map[string]string{"session_id": otherMemoryName, "repo_path": otherRepo},
		Provenance: &memory.Provenance{SessionID: otherMemoryName, RepoPath: otherRepo},
	}))
	require.NoError(t, mem.Save(storePath))

	built, err := buildMemoryStore(nil, cliOptions{
		memoryStorePath: storePath,
		memoryScope:     memoryScopeStore,
		memorySearch:    "oauth",
	})
	require.NoError(t, err)
	assert.Equal(t, memoryScopeStore, built.Corpus.Scope)
	assert.ElementsMatch(t, []string{localMemorySessionID, otherMemoryName}, built.Corpus.SessionIDs)

	results, err := built.Search("oauth", 10)
	require.NoError(t, err)
	require.Len(t, results, 2)
}

func TestBuildMemoryStore_StoreScopeRequiresStorePath(t *testing.T) {
	t.Parallel()

	_, err := buildMemoryStore(session.NewStore(filepath.Join(t.TempDir(), "sessions")), cliOptions{
		memoryScope:  memoryScopeStore,
		memorySearch: "oauth",
	})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "--memory-store is required")
}

func TestBuildMemoryStore_RebuildRejectsStoreScope(t *testing.T) {
	t.Parallel()

	redactor, err := memory.NewRedactor()
	require.NoError(t, err)

	_, err = buildMemoryStoreWithRedactor(
		session.NewStore(filepath.Join(t.TempDir(), "sessions")),
		cliOptions{memoryStorePath: filepath.Join(t.TempDir(), "memory.json"), memoryScope: memoryScopeStore},
		redactor,
		true,
	)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "cannot be used with --memory-rebuild")
}

func TestBuildMemoryStore_RebuildDoesNotLoadExistingStore(t *testing.T) {
	t.Parallel()

	const sessionID = "demo"

	dir := t.TempDir()
	storePath := filepath.Join(dir, "memory.json")
	require.NoError(t, os.WriteFile(storePath, []byte("{not-json"), 0o600))

	store := session.NewStore(filepath.Join(dir, "sessions"))
	saved := session.New("gpt-test", []llm.Message{{Role: llm.RoleUser, Content: "OAuth rebuilt session note"}})
	saved.ID = sessionID
	saved.WorktreePath = dir
	require.NoError(t, store.Save(saved))

	redactor, err := memory.NewRedactor()
	require.NoError(t, err)

	mem, err := buildMemoryStoreWithRedactor(
		store,
		cliOptions{memoryStorePath: storePath, memoryRepoPath: dir},
		redactor,
		true,
	)
	require.NoError(t, err)
	assert.Equal(t, []string{sessionID}, mem.Corpus.SessionIDs)
	assert.NotEmpty(t, mem.Documents)
}

func TestBuildMemoryStore_ExplicitSessionTagAgentAndGlobalScopes(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	store := session.NewStore(filepath.Join(dir, "sessions"))

	reviewer := session.New("gpt-test", []llm.Message{{Role: llm.RoleUser, Content: "OAuth reviewer note"}})
	reviewer.ID = "reviewer-session"
	reviewer.DefaultAgent = testReviewerName
	reviewer.Tags = []string{"auth"}
	reviewer.WorktreePath = dir
	require.NoError(t, store.Save(reviewer))

	writer := session.New("gpt-test", []llm.Message{{Role: llm.RoleUser, Content: "Docs writer note"}})
	writer.ID = "writer-session"
	writer.DefaultAgent = "writer"
	writer.Tags = []string{"docs"}
	writer.WorktreePath = filepath.Join(dir, otherMemoryName)
	require.NoError(t, store.Save(writer))

	tests := []struct {
		name      string
		opts      cliOptions
		wantScope string
		wantIDs   []string
	}{
		{
			name:      "session",
			opts:      cliOptions{memorySearch: "note", memorySessionRef: "reviewer-session"},
			wantScope: memory.ScopeSession,
			wantIDs:   []string{"reviewer-session"},
		},
		{
			name:      "tags",
			opts:      cliOptions{memorySearch: "note", memoryTags: stringListFlag{"docs"}},
			wantScope: memory.ScopeTags,
			wantIDs:   []string{"writer-session"},
		},
		{
			name:      "agent",
			opts:      cliOptions{memorySearch: "note", memoryAgent: testReviewerName},
			wantScope: memory.ScopeAgent,
			wantIDs:   []string{"reviewer-session"},
		},
		{
			name:      "agent defaults to selected agent",
			opts:      cliOptions{memorySearch: "note", memoryScope: "agent", agentName: testReviewerName},
			wantScope: memory.ScopeAgent,
			wantIDs:   []string{"reviewer-session"},
		},
		{
			name:      "global",
			opts:      cliOptions{memorySearch: "note", memoryGlobal: true},
			wantScope: memory.ScopeGlobal,
			wantIDs:   []string{"writer-session", "reviewer-session"},
		},
		{
			name:      "global scope",
			opts:      cliOptions{memorySearch: "note", memoryScope: "global"},
			wantScope: memory.ScopeGlobal,
			wantIDs:   []string{"writer-session", "reviewer-session"},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			mem, err := buildMemoryStore(store, tc.opts)
			require.NoError(t, err)
			assert.Equal(t, tc.wantScope, mem.Corpus.Scope)
			if tc.wantScope == memory.ScopeGlobal {
				assert.True(t, mem.Corpus.Global)
			}
			assert.ElementsMatch(t, tc.wantIDs, mem.Corpus.SessionIDs)
		})
	}
}

func TestBuildMemoryStore_AgentScopeSkipsNonMatchingBeforeFullLoad(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	store := session.NewStore(filepath.Join(dir, "sessions"))

	reviewer := session.New("gpt-test", []llm.Message{{Role: llm.RoleUser, Content: "OAuth reviewer note"}})
	reviewer.ID = "reviewer-session"
	reviewer.DefaultAgent = testReviewerName
	require.NoError(t, store.Save(reviewer))

	malformedWriter, err := json.Marshal(map[string]any{
		"id":            "writer-malformed",
		"created_at":    time.Now().UTC(),
		"updated_at":    time.Now().UTC(),
		"default_agent": "writer",
		"messages": []map[string]any{
			{"role": "user", "content": 123},
		},
	})
	require.NoError(t, err)
	require.NoError(t, os.MkdirAll(store.Dir(), 0o750))
	require.NoError(t, os.WriteFile(store.Path("writer-malformed"), append(malformedWriter, '\n'), 0o600))

	mem, err := buildMemoryStore(store, cliOptions{memorySearch: "oauth", memoryAgent: testReviewerName})
	require.NoError(t, err)
	assert.Equal(t, memory.ScopeAgent, mem.Corpus.Scope)
	assert.Equal(t, []string{"reviewer-session"}, mem.Corpus.SessionIDs)
}

func TestBuildMemoryStore_AgentScopeCanUseSummaryOnlyArtifactAgent(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	store := session.NewStore(filepath.Join(dir, "sessions"))

	saved := session.New("gpt-test", nil)
	saved.ID = "artifact-session"
	saved.Artifacts = []session.Artifact{{
		Path:        "docs/oauth.md",
		Kind:        "markdown",
		Summary:     "OAuth reviewer artifact",
		SourceAgent: testReviewerName,
		CreatedAt:   time.Now().UTC(),
	}}
	require.NoError(t, store.Save(saved))

	mem, err := buildMemoryStore(store, cliOptions{memorySearch: "oauth", memoryAgent: testReviewerName})
	require.NoError(t, err)
	assert.Equal(t, memory.ScopeAgent, mem.Corpus.Scope)
	assert.Equal(t, []string{"artifact-session"}, mem.Corpus.SessionIDs)

	results, err := mem.Search("oauth", 10)
	require.NoError(t, err)
	require.Len(t, results, 1)
	assert.Equal(t, "session/artifact-session/artifact/0", results[0].Document.ID)
}

func TestBuildMemoryStore_GlobalScopeReportsStoredAndIndexedSessions(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	storePath := filepath.Join(dir, "memory.json")

	mem := memory.NewStore()
	require.NoError(t, mem.Add(memory.Document{
		ID:         "session/stored/message/0",
		Text:       "OAuth stored memory",
		Metadata:   map[string]string{"session_id": "stored"},
		Provenance: &memory.Provenance{SessionID: "stored"},
	}))
	require.NoError(t, mem.Save(storePath))

	sessions := session.NewStore(filepath.Join(dir, "sessions"))
	saved := session.New("gpt-test", []llm.Message{{Role: llm.RoleUser, Content: "OAuth saved session"}})
	saved.ID = "saved"
	require.NoError(t, sessions.Save(saved))

	built, err := buildMemoryStore(sessions, cliOptions{
		memoryStorePath: storePath,
		memoryGlobal:    true,
		memorySearch:    "oauth",
	})
	require.NoError(t, err)
	assert.Equal(t, memory.ScopeGlobal, built.Corpus.Scope)
	assert.ElementsMatch(t, []string{"saved", "stored"}, built.Corpus.SessionIDs)
	assert.Equal(t, 2, built.Corpus.SessionCount)

	results, err := built.Search("oauth", 10)
	require.NoError(t, err)
	require.Len(t, results, 2)
}

func TestBuildMemoryStore_DateRangeScopeFiltersSessions(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	store := session.NewStore(filepath.Join(dir, "sessions"))

	oldTime := time.Date(2026, 5, 1, 9, 0, 0, 0, time.UTC)
	recentTime := time.Date(2026, 5, 10, 9, 0, 0, 0, time.UTC)
	writeMemorySessionFixture(t, store, session.Session{
		ID:        "old-session",
		CreatedAt: oldTime,
		UpdatedAt: oldTime,
		Messages:  []llm.Message{{Role: llm.RoleUser, Content: "OAuth old note"}},
	})
	writeMemorySessionFixture(t, store, session.Session{
		ID:        "recent-session",
		CreatedAt: recentTime,
		UpdatedAt: recentTime,
		Messages:  []llm.Message{{Role: llm.RoleUser, Content: "OAuth recent note"}},
	})

	mem, err := buildMemoryStore(store, cliOptions{
		memorySearch: "oauth",
		memorySince:  "2026-05-05",
		memoryUntil:  "2026-05-12",
	})
	require.NoError(t, err)
	assert.Equal(t, memory.ScopeDateRange, mem.Corpus.Scope)
	assert.Equal(t, []string{"recent-session"}, mem.Corpus.SessionIDs)
	assert.Contains(t, mem.Corpus.Description, "date_range=2026-05-05T00:00:00Z..2026-05-12T23:59:59Z")

	results, err := mem.Search("oauth", 10)
	require.NoError(t, err)
	require.Len(t, results, 1)
	assert.Equal(t, "session/recent-session/message/0", results[0].Document.ID)
}

func TestBuildMemoryStore_TagScopeHonorsExplicitRepoPath(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	otherDir := filepath.Join(dir, otherMemoryName)
	store := session.NewStore(filepath.Join(dir, "sessions"))

	local := session.New("gpt-test", []llm.Message{{Role: llm.RoleUser, Content: "OAuth local auth note"}})
	local.ID = "local-auth"
	local.Tags = []string{"auth"}
	local.WorktreePath = dir
	require.NoError(t, store.Save(local))

	other := session.New("gpt-test", []llm.Message{{Role: llm.RoleUser, Content: "OAuth other auth note"}})
	other.ID = "other-auth"
	other.Tags = []string{"auth"}
	other.WorktreePath = otherDir
	require.NoError(t, store.Save(other))

	mem, err := buildMemoryStore(store, cliOptions{
		memorySearch:   "oauth",
		memoryRepoPath: dir,
		memoryTags:     stringListFlag{"auth"},
	})
	require.NoError(t, err)
	assert.Equal(t, memory.ScopeTags, mem.Corpus.Scope)
	assert.Equal(t, []string{"local-auth"}, mem.Corpus.SessionIDs)
	assert.Contains(t, mem.Corpus.Description, "repo="+cleanMemoryPath(dir))
	assert.Contains(t, mem.Corpus.Description, "tags=auth")
}

func TestBuildMemoryStore_ExplicitScopeConstrainsLoadedMemoryStore(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	storePath := filepath.Join(dir, "memory.json")

	mem := memory.NewStore()
	require.NoError(t, mem.Add(memory.Document{
		ID:         "session/local/message/0",
		Text:       "OAuth local repo note",
		Metadata:   map[string]string{"session_id": localMemorySessionID, "repo_path": dir},
		Provenance: &memory.Provenance{SessionID: localMemorySessionID, RepoPath: dir},
	}))
	require.NoError(t, mem.Add(memory.Document{
		ID:         "session/other/message/0",
		Text:       "OAuth other repo note",
		Metadata:   map[string]string{"session_id": otherMemoryName, "repo_path": filepath.Join(dir, otherMemoryName)},
		Provenance: &memory.Provenance{SessionID: otherMemoryName, RepoPath: filepath.Join(dir, otherMemoryName)},
	}))
	require.NoError(t, mem.Save(storePath))

	built, err := buildMemoryStore(session.NewStore(filepath.Join(dir, "sessions")), cliOptions{
		memoryStorePath: storePath,
		memoryRepoPath:  dir,
		memorySearch:    "oauth",
	})
	require.NoError(t, err)
	assert.Equal(t, memory.ScopeRepo, built.Corpus.Scope)
	assert.Equal(t, []string{localMemorySessionID}, built.Corpus.SessionIDs)

	results, err := built.Search("oauth", 10)
	require.NoError(t, err)
	require.Len(t, results, 1)
	assert.Equal(t, "session/local/message/0", results[0].Document.ID)
}

func TestBuildMemoryStore_ListCorpusSessionScopeConstrainsLoadedStore(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	storePath := filepath.Join(dir, "memory.json")

	mem := memory.NewStore()
	require.NoError(t, mem.Add(memory.Document{
		ID:         "session/local/message/0",
		Text:       "OAuth local session note",
		Metadata:   map[string]string{"session_id": localMemorySessionID},
		Provenance: &memory.Provenance{SessionID: localMemorySessionID},
	}))
	require.NoError(t, mem.Add(memory.Document{
		ID:         "session/other/message/0",
		Text:       "OAuth other session note",
		Metadata:   map[string]string{"session_id": otherMemoryName},
		Provenance: &memory.Provenance{SessionID: otherMemoryName},
	}))
	require.NoError(t, mem.Save(storePath))

	built, err := buildMemoryStore(session.NewStore(filepath.Join(dir, "sessions")), cliOptions{
		memoryStorePath:  storePath,
		memoryListCorpus: true,
		memorySessionRef: localMemorySessionID,
	})
	require.NoError(t, err)
	assert.Equal(t, memory.ScopeSession, built.Corpus.Scope)
	assert.Equal(t, []string{localMemorySessionID}, built.Corpus.SessionIDs)
	require.Len(t, built.Documents, 1)
	assert.Equal(t, "session/local/message/0", built.Documents[0].ID)
}

func TestBuildMemoryStore_ExplicitScopeFiltersLegacyStoreBeforeCustomRedaction(t *testing.T) {
	t.Parallel()

	const (
		localRepo = "ACME-12345"
		otherRepo = "ACME-99999"
	)

	dir := t.TempDir()
	storePath := filepath.Join(dir, "memory.json")

	mem := memory.NewStore()
	require.NoError(t, mem.Add(memory.Document{
		ID:         "session/local/message/0",
		Text:       "OAuth local custom-redacted repo note",
		Metadata:   map[string]string{"session_id": localMemorySessionID, "repo_path": localRepo},
		Provenance: &memory.Provenance{SessionID: localMemorySessionID, RepoPath: localRepo},
	}))
	require.NoError(t, mem.Add(memory.Document{
		ID:         "session/other/message/0",
		Text:       "OAuth other custom-redacted repo note",
		Metadata:   map[string]string{"session_id": otherMemoryName, "repo_path": otherRepo},
		Provenance: &memory.Provenance{SessionID: otherMemoryName, RepoPath: otherRepo},
	}))
	require.NoError(t, mem.Save(storePath))

	built, err := buildMemoryStore(session.NewStore(filepath.Join(dir, "sessions")), cliOptions{
		memoryStorePath:   storePath,
		memoryRepoPath:    localRepo,
		memorySearch:      "oauth",
		memoryRedactRules: rawStringListFlag{`ACME-[0-9]+`},
	})
	require.NoError(t, err)
	assert.Equal(t, []string{localMemorySessionID}, built.Corpus.SessionIDs)
	assert.Equal(t, localRepo, built.Corpus.RepoPath)

	results, err := built.Search("oauth", 10)
	require.NoError(t, err)
	require.Len(t, results, 1)
	assert.Equal(t, "session/local/message/0", results[0].Document.ID)
	assert.NotContains(t, results[0].Document.Metadata["repo_path"], localRepo)
	assert.Contains(t, results[0].Document.Metadata["repo_path"], "[REDACTED:custom_1]#")
}

func TestBuildMemoryStore_ConstrainedStoreCombinesTagAndRepoSelectors(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	storePath := filepath.Join(dir, "memory.json")

	mem := memory.NewStore()
	require.NoError(t, mem.Add(memory.Document{
		ID:         "session/local/message/0",
		Text:       "OAuth local auth note",
		Metadata:   map[string]string{"session_id": localMemorySessionID, "repo_path": dir, "tags": "auth"},
		Provenance: &memory.Provenance{SessionID: localMemorySessionID, RepoPath: dir, Tags: []string{"auth"}},
	}))
	require.NoError(t, mem.Add(memory.Document{
		ID:         "session/other/message/0",
		Text:       "OAuth other auth note",
		Metadata:   map[string]string{"session_id": otherMemoryName, "repo_path": filepath.Join(dir, otherMemoryName), "tags": "auth"},
		Provenance: &memory.Provenance{SessionID: otherMemoryName, RepoPath: filepath.Join(dir, otherMemoryName), Tags: []string{"auth"}},
	}))
	require.NoError(t, mem.Save(storePath))

	built, err := buildMemoryStore(session.NewStore(filepath.Join(dir, "sessions")), cliOptions{
		memoryStorePath: storePath,
		memoryRepoPath:  dir,
		memoryTags:      stringListFlag{"auth"},
		memorySearch:    "oauth",
	})
	require.NoError(t, err)
	assert.Equal(t, memory.ScopeTags, built.Corpus.Scope)
	assert.Equal(t, []string{localMemorySessionID}, built.Corpus.SessionIDs)

	results, err := built.Search("oauth", 10)
	require.NoError(t, err)
	require.Len(t, results, 1)
	assert.Equal(t, "session/local/message/0", results[0].Document.ID)
}

func TestBuildMemoryStore_StoreScopeCanStillUseSecondaryFilters(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	storePath := filepath.Join(dir, "memory.json")

	mem := memory.NewStore()
	require.NoError(t, mem.Add(memory.Document{
		ID:         "session/auth/message/0",
		Text:       "OAuth auth note",
		Metadata:   map[string]string{"session_id": "auth", "tags": "auth"},
		Provenance: &memory.Provenance{SessionID: "auth", Tags: []string{"auth"}},
	}))
	require.NoError(t, mem.Add(memory.Document{
		ID:         "session/docs/message/0",
		Text:       "OAuth docs note",
		Metadata:   map[string]string{"session_id": "docs", "tags": "docs"},
		Provenance: &memory.Provenance{SessionID: "docs", Tags: []string{"docs"}},
	}))
	require.NoError(t, mem.Save(storePath))

	built, err := buildMemoryStore(session.NewStore(filepath.Join(dir, "sessions")), cliOptions{
		memoryStorePath: storePath,
		memoryScope:     memoryScopeStore,
		memoryTags:      stringListFlag{"auth"},
		memorySearch:    "oauth",
	})
	require.NoError(t, err)
	assert.Equal(t, memoryScopeStore, built.Corpus.Scope)
	assert.Equal(t, []string{"auth"}, built.Corpus.SessionIDs)

	results, err := built.Search("oauth", 10)
	require.NoError(t, err)
	require.Len(t, results, 1)
	assert.Equal(t, "session/auth/message/0", results[0].Document.ID)
}

func TestBuildMemoryStore_StoreScopeCanFilterBySessionRef(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	storePath := filepath.Join(dir, "memory.json")

	mem := memory.NewStore()
	require.NoError(t, mem.Add(memory.Document{
		ID:         "session/auth-review/message/0",
		Text:       "OAuth auth review note",
		Metadata:   map[string]string{"session_id": "auth-review"},
		Provenance: &memory.Provenance{SessionID: "auth-review"},
	}))
	require.NoError(t, mem.Add(memory.Document{
		ID:         "session/docs-review/message/0",
		Text:       "OAuth docs review note",
		Metadata:   map[string]string{"session_id": "docs-review"},
		Provenance: &memory.Provenance{SessionID: "docs-review"},
	}))
	require.NoError(t, mem.Save(storePath))

	built, err := buildMemoryStore(session.NewStore(filepath.Join(dir, "sessions")), cliOptions{
		memoryStorePath:  storePath,
		memoryScope:      memoryScopeStore,
		memorySessionRef: "auth-review",
		memorySearch:     "oauth",
	})
	require.NoError(t, err)
	assert.Equal(t, memoryScopeStore, built.Corpus.Scope)
	assert.Equal(t, []string{"auth-review"}, built.Corpus.SessionIDs)

	results, err := built.Search("oauth", 10)
	require.NoError(t, err)
	require.Len(t, results, 1)
	assert.Equal(t, "session/auth-review/message/0", results[0].Document.ID)
}

//nolint:paralleltest // Captures process-wide stdout.
func TestRunMemoryCommandStoreScopeSurvivesSaveBeforeSearch(t *testing.T) {
	dir := t.TempDir()
	storePath := filepath.Join(dir, "memory.json")
	now := time.Now().UTC()
	oldTime := now.AddDate(0, 0, -60)

	mem := memory.NewStore()
	require.NoError(t, mem.Add(memory.Document{
		ID:         "session/recent/message/0",
		Text:       "OAuth recent stored memory",
		Metadata:   map[string]string{"session_id": "recent", "updated_at": now.Format(time.RFC3339)},
		Provenance: &memory.Provenance{SessionID: "recent", UpdatedAt: now.Format(time.RFC3339)},
	}))
	require.NoError(t, mem.Add(memory.Document{
		ID:         "session/old/message/0",
		Text:       "OAuth old stored memory",
		Metadata:   map[string]string{"session_id": "old", "updated_at": oldTime.Format(time.RFC3339)},
		Provenance: &memory.Provenance{SessionID: "old", UpdatedAt: oldTime.Format(time.RFC3339)},
	}))
	require.NoError(t, mem.Save(storePath))

	output := captureMemoryStdout(t, func() error {
		return runMemoryCommand(session.NewStore(filepath.Join(dir, "sessions")), cliOptions{
			memoryStorePath:     storePath,
			memoryScope:         memoryScopeStore,
			memorySearch:        "OAuth",
			memoryRetentionDays: positiveIntFlag{value: 30, set: true},
		})
	})
	assert.Contains(t, output, "Searched corpus:")
	assert.Contains(t, output, "scope=store")
	assert.Contains(t, output, "session/recent/message/0")
	assert.NotContains(t, output, "session/old/message/0")

	loaded, err := memory.Load(storePath)
	require.NoError(t, err)
	assert.Equal(t, memory.ScopeStore, loaded.Corpus.Scope)
	assert.Equal(t, []string{"recent"}, loaded.Corpus.SessionIDs)
}

//nolint:paralleltest // Uses t.Chdir and captures process-wide stdout.
func TestRunMemoryCommandStoreSearchWithRetentionUsesRepoScopedSearchView(t *testing.T) {
	dir := t.TempDir()
	t.Chdir(dir)

	storePath := filepath.Join(dir, "memory.json")
	otherRepo := filepath.Join(t.TempDir(), "other-repo")
	now := time.Now().UTC()

	mem := memory.NewStore()
	require.NoError(t, mem.Add(memory.Document{
		ID:         "session/local/message/0",
		Text:       "OAuth local stored memory",
		Metadata:   map[string]string{"session_id": localMemorySessionID, "repo_path": dir, "updated_at": now.Format(time.RFC3339)},
		Provenance: &memory.Provenance{SessionID: localMemorySessionID, RepoPath: dir, UpdatedAt: now.Format(time.RFC3339)},
	}))
	require.NoError(t, mem.Add(memory.Document{
		ID:         "session/other/message/0",
		Text:       "OAuth other stored memory",
		Metadata:   map[string]string{"session_id": otherMemoryName, "repo_path": otherRepo, "updated_at": now.Format(time.RFC3339)},
		Provenance: &memory.Provenance{SessionID: otherMemoryName, RepoPath: otherRepo, UpdatedAt: now.Format(time.RFC3339)},
	}))
	require.NoError(t, mem.Save(storePath))

	output := captureMemoryStdout(t, func() error {
		return runMemoryCommand(session.NewStore(filepath.Join(dir, "sessions")), cliOptions{
			memoryStorePath:     storePath,
			memoryRepoPath:      dir,
			memorySearch:        "OAuth",
			memoryRetentionDays: positiveIntFlag{value: 30, set: true},
		})
	})
	assert.Contains(t, output, "Searched corpus:")
	assert.Contains(t, output, "scope=repo")
	assert.Contains(t, output, "session/local/message/0")
	assert.NotContains(t, output, "session/other/message/0")

	loaded, err := memory.Load(storePath)
	require.NoError(t, err)
	assert.Len(t, loaded.Documents, 2)
}

//nolint:paralleltest // Uses t.Chdir to verify relative session IDs are not treated as repo paths.
func TestBuildMemoryStore_RepoScopeExcludesStoreSessionsWithoutRepoProvenance(t *testing.T) {
	dir := t.TempDir()
	t.Chdir(dir)

	storePath := filepath.Join(dir, "memory.json")
	mem := memory.NewStore()
	require.NoError(t, mem.Add(memory.Document{
		ID:   "session/legacy/message/0",
		Path: "legacy",
		Text: "OAuth legacy session without repo provenance",
	}))
	require.NoError(t, mem.Add(memory.Document{
		ID:         "session/local/message/0",
		Text:       "OAuth local repo session",
		Metadata:   map[string]string{"session_id": localMemorySessionID, "repo_path": dir},
		Provenance: &memory.Provenance{SessionID: localMemorySessionID, RepoPath: dir},
	}))
	require.NoError(t, mem.Save(storePath))

	built, err := buildMemoryStore(session.NewStore(filepath.Join(dir, "sessions")), cliOptions{
		memoryStorePath: storePath,
		memoryRepoPath:  dir,
		memorySearch:    "oauth",
	})
	require.NoError(t, err)
	assert.Equal(t, []string{localMemorySessionID}, built.Corpus.SessionIDs)

	results, err := built.Search("oauth", 10)
	require.NoError(t, err)
	require.Len(t, results, 1)
	assert.Equal(t, "session/local/message/0", results[0].Document.ID)
}

func TestBuildMemoryStore_SessionScopeCanFilterStoreWithoutSavedSession(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	storePath := filepath.Join(dir, "memory.json")

	mem := memory.NewStore()
	require.NoError(t, mem.Add(memory.Document{
		ID:         "session/local/message/0",
		Text:       "OAuth local session note",
		Metadata:   map[string]string{"session_id": localMemorySessionID},
		Provenance: &memory.Provenance{SessionID: localMemorySessionID},
	}))
	require.NoError(t, mem.Add(memory.Document{
		ID:         "session/other/message/0",
		Text:       "OAuth other session note",
		Metadata:   map[string]string{"session_id": otherMemoryName},
		Provenance: &memory.Provenance{SessionID: otherMemoryName},
	}))
	require.NoError(t, mem.Save(storePath))

	built, err := buildMemoryStore(session.NewStore(filepath.Join(dir, "missing-sessions")), cliOptions{
		memoryStorePath:  storePath,
		memorySessionRef: localMemorySessionID,
		memorySearch:     "oauth",
	})
	require.NoError(t, err)
	assert.Equal(t, memory.ScopeSession, built.Corpus.Scope)
	assert.Equal(t, []string{localMemorySessionID}, built.Corpus.SessionIDs)

	results, err := built.Search("oauth", 10)
	require.NoError(t, err)
	require.Len(t, results, 1)
	assert.Equal(t, "session/local/message/0", results[0].Document.ID)
}

func TestBuildMemoryStore_SessionScopeFiltersRedactedStoredSessionID(t *testing.T) {
	t.Parallel()

	const secretSessionID = "sk-1234567890abcdefSECRET"

	dir := t.TempDir()
	storePath := filepath.Join(dir, "memory.json")

	mem := memory.NewStore()
	require.NoError(t, mem.Add(memory.Document{
		ID:         "session/" + secretSessionID + "/message/0",
		Text:       "OAuth secret session note",
		Metadata:   map[string]string{"session_id": secretSessionID},
		Provenance: &memory.Provenance{SessionID: secretSessionID},
	}))
	require.NoError(t, mem.Save(storePath))

	built, err := buildMemoryStore(session.NewStore(filepath.Join(dir, "missing-sessions")), cliOptions{
		memoryStorePath:  storePath,
		memorySessionRef: secretSessionID,
		memorySearch:     "oauth",
	})
	require.NoError(t, err)
	assert.Equal(t, memory.ScopeSession, built.Corpus.Scope)
	require.Len(t, built.Documents, 1)
	assert.NotContains(t, built.Documents[0].ID, secretSessionID)

	results, err := built.Search("oauth", 10)
	require.NoError(t, err)
	require.Len(t, results, 1)
	assert.NotContains(t, results[0].Document.ID, secretSessionID)
}

func TestBuildMemoryStore_LegacySessionRefFiltersStoredMemory(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	storePath := filepath.Join(dir, "memory.json")

	mem := memory.NewStore()
	require.NoError(t, mem.Add(memory.Document{
		ID:         "session/local/message/0",
		Text:       "OAuth local session note",
		Metadata:   map[string]string{"session_id": localMemorySessionID},
		Provenance: &memory.Provenance{SessionID: localMemorySessionID},
	}))
	require.NoError(t, mem.Add(memory.Document{
		ID:         "session/other/message/0",
		Text:       "OAuth other session note",
		Metadata:   map[string]string{"session_id": otherMemoryName},
		Provenance: &memory.Provenance{SessionID: otherMemoryName},
	}))
	require.NoError(t, mem.Save(storePath))

	built, err := buildMemoryStore(session.NewStore(filepath.Join(dir, "missing-sessions")), cliOptions{
		memoryStorePath: storePath,
		sessionRef:      localMemorySessionID,
		memorySearch:    "oauth",
	})
	require.NoError(t, err)
	assert.Equal(t, memory.ScopeSession, built.Corpus.Scope)
	assert.Equal(t, []string{localMemorySessionID}, built.Corpus.SessionIDs)

	results, err := built.Search("oauth", 10)
	require.NoError(t, err)
	require.Len(t, results, 1)
	assert.Equal(t, "session/local/message/0", results[0].Document.ID)
}

func TestFormatMemoryCorpusStatementReportsDateRangeAndRetention(t *testing.T) {
	t.Parallel()

	redactor, err := memory.NewRedactor()
	require.NoError(t, err)

	mem := memory.NewStore()
	mem.Corpus = memory.CorpusMetadata{
		Scope:     memory.ScopeDateRange,
		DateStart: "2026-05-01T00:00:00Z",
		DateEnd:   "2026-05-02T23:59:59Z",
		Retention: "30 days",
	}

	storePath := filepath.Join(t.TempDir(), "memory.json")
	got := formatMemoryCorpusStatement(mem, redactor, storePath)
	assert.Contains(t, got, "scope=date_range")
	assert.Contains(t, got, "store="+filepath.Clean(storePath))
	assert.Contains(t, got, "date_range=2026-05-01T00:00:00Z..2026-05-02T23:59:59Z")
	assert.Contains(t, got, "retention=30 days")
}

func TestFormatMemoryCorpusStatementRedactsStoreAndSelectors(t *testing.T) {
	t.Parallel()

	const secret = "ACME-12345"

	redactor, err := memory.NewRedactor(`ACME-[0-9]+`)
	require.NoError(t, err)

	mem := memory.NewStore()
	mem.Corpus = memory.CorpusMetadata{
		Scope:      memory.ScopeRepo,
		RepoPath:   filepath.Join(t.TempDir(), secret),
		Agent:      secret,
		Tags:       []string{secret},
		SessionIDs: []string{secret},
	}

	storePath := filepath.Join(t.TempDir(), secret, "memory.json")
	got := formatMemoryCorpusStatement(mem, redactor, storePath)
	assert.Contains(t, got, "Searched corpus:")
	assert.Contains(t, got, "[REDACTED:custom_1]")
	assert.NotContains(t, got, secret)
}

func TestMemoryPlan_DateRangeScope(t *testing.T) {
	t.Parallel()

	plan, err := memoryPlan(cliOptions{memorySearch: "note", memorySince: "2026-05-01", memoryUntil: "2026-05-02"})
	require.NoError(t, err)
	assert.Equal(t, memory.ScopeDateRange, plan.scope)
	assert.True(t, plan.hasSince)
	assert.True(t, plan.hasUntil)
	assert.Equal(t, "2026-05-01T00:00:00Z", plan.since.Format(time.RFC3339))
	assert.Equal(t, "2026-05-02T23:59:59Z", plan.until.Format(time.RFC3339))
}

func TestMemoryPlan_AgentScopeDefaultsToSelectedAgent(t *testing.T) {
	t.Parallel()

	plan, err := memoryPlan(cliOptions{memorySearch: "note", memoryScope: "agent", agentName: testReviewerName})
	require.NoError(t, err)
	assert.Equal(t, memory.ScopeAgent, plan.scope)
	assert.Equal(t, testReviewerName, plan.agent)
}

func TestMemoryPlan_AgentScopeErrorMentionsSelectedAgentFallback(t *testing.T) {
	t.Parallel()

	_, err := memoryPlan(cliOptions{memorySearch: "note", memoryScope: "agent"})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "--memory-agent or --agent")
}

func TestNormalizeMemoryScope_AcceptsIssueVocabularyAliases(t *testing.T) {
	t.Parallel()

	tests := map[string]string{
		"current-session-only": memory.ScopeSession,
		"current-repo-only":    memory.ScopeRepo,
		"tagged-sessions":      memory.ScopeTags,
		"date-ranges":          memory.ScopeDateRange,
		"agent-memory":         memory.ScopeAgent,
		"opt-in-global":        memory.ScopeGlobal,
	}
	for input, want := range tests {
		assert.Equal(t, want, normalizeMemoryScope(input), input)
	}
}

func TestMemoryPlan_RejectsIncompleteExplicitScopes(t *testing.T) {
	t.Parallel()

	tests := []cliOptions{
		{memoryScope: "tags"},
		{memoryScope: "date-range"},
		{memoryScope: "agent"},
		{memoryScope: "unknown"},
	}
	for _, opts := range tests {
		_, err := memoryPlan(opts)
		require.Error(t, err)
	}
}

func TestMemoryPlan_RejectsInvertedDateRange(t *testing.T) {
	t.Parallel()

	_, err := memoryPlan(cliOptions{
		memorySearch: "note",
		memorySince:  "2026-05-03",
		memoryUntil:  "2026-05-01",
	})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "--memory-since")
	assert.Contains(t, err.Error(), "after --memory-until")
}

//nolint:paralleltest // Captures process-wide stdout and uses t.Chdir.
func TestRunMemoryCommandReportsCorpusAndRedactsSnippet(t *testing.T) {
	const secret = "sk-1234567890abcdefSECRET"

	dir := t.TempDir()
	t.Chdir(dir)

	store := session.NewStore(filepath.Join(dir, "sessions"))
	saved := session.New("gpt-test", []llm.Message{{Role: llm.RoleUser, Content: "Rotate OAuth token " + secret}})
	saved.ID = "demo"
	saved.WorktreePath = dir
	require.NoError(t, store.Save(saved))

	output := captureMemoryStdout(t, func() error {
		return runMemoryCommand(store, cliOptions{memorySearch: "rotate oauth", memoryRepoPath: dir})
	})

	assert.Contains(t, output, "Searched corpus:")
	assert.Contains(t, output, "scope=repo")
	assert.NotContains(t, output, secret)
	assert.Contains(t, output, "[REDACTED:")
}

//nolint:paralleltest // Captures process-wide stdout.
func TestRunMemoryCommandReportsSessionIDInCorpus(t *testing.T) {
	dir := t.TempDir()

	store := session.NewStore(filepath.Join(dir, "sessions"))
	saved := session.New("gpt-test", []llm.Message{{Role: llm.RoleUser, Content: "OAuth scoped session note"}})
	saved.ID = "demo-session"
	require.NoError(t, store.Save(saved))

	output := captureMemoryStdout(t, func() error {
		return runMemoryCommand(store, cliOptions{memorySearch: "oauth", memorySessionRef: "demo-session"})
	})

	assert.Contains(t, output, "Searched corpus:")
	assert.Contains(t, output, "scope=session")
	assert.Contains(t, output, "session_ids=demo-session")
}

//nolint:paralleltest // Captures process-wide stdout.
func TestRunMemoryCommandReportsSelectedSessionForEmptyStoredScope(t *testing.T) {
	dir := t.TempDir()
	storePath := filepath.Join(dir, "memory.json")

	mem := memory.NewStore()
	require.NoError(t, mem.Add(memory.Document{
		ID:         "session/other/message/0",
		Text:       "OAuth other memory",
		Metadata:   map[string]string{"session_id": otherMemoryName},
		Provenance: &memory.Provenance{SessionID: otherMemoryName},
	}))
	require.NoError(t, mem.Save(storePath))

	output := captureMemoryStdout(t, func() error {
		return runMemoryCommand(session.NewStore(filepath.Join(dir, "missing-sessions")), cliOptions{
			memoryStorePath:  storePath,
			memorySessionRef: "missing",
			memorySearch:     "oauth",
		})
	})

	assert.Contains(t, output, "Searched corpus:")
	assert.Contains(t, output, "scope=session")
	assert.Contains(t, output, "session_ids=missing")
	assert.Contains(t, output, "No memory results found.")
}

//nolint:paralleltest // Captures process-wide stdout.
func TestRunMemoryCommandRedactsLegacyStoreBeforePrinting(t *testing.T) {
	const secret = "sk-abcdef1234567890SECRET"

	dir := t.TempDir()
	storePath := filepath.Join(dir, "memory.json")
	require.NoError(t, os.WriteFile(storePath, []byte(`{
  "documents": [
    {"id": "legacy", "text": "legacy raw token `+secret+`"}
  ]
}`), 0o600))

	output := captureMemoryStdout(t, func() error {
		return runMemoryCommand(session.NewStore(filepath.Join(dir, "sessions")), cliOptions{
			memoryStorePath: storePath,
			memoryScope:     memoryScopeStore,
			memorySearch:    "legacy raw token",
		})
	})

	assert.Contains(t, output, "Searched corpus:")
	assert.Contains(t, output, "scope=store")
	assert.Contains(t, output, "store="+filepath.Clean(storePath))
	assert.Contains(t, output, "legacy")
	assert.NotContains(t, output, secret)
	assert.Contains(t, output, "[REDACTED:")
}

//nolint:paralleltest // Captures process-wide stdout.
func TestRunMemoryCommandAppliesCustomRedactionBeforePrinting(t *testing.T) {
	const secret = "ACME-12345"

	dir := t.TempDir()
	storePath := filepath.Join(dir, "memory.json")
	require.NoError(t, os.WriteFile(storePath, []byte(`{
  "documents": [
    {"id": "legacy", "text": "customer `+secret+` hidden note"}
  ]
}`), 0o600))

	output := captureMemoryStdout(t, func() error {
		return runMemoryCommand(session.NewStore(filepath.Join(dir, "sessions")), cliOptions{
			memoryStorePath:   storePath,
			memoryScope:       memoryScopeStore,
			memorySearch:      "customer hidden",
			memoryRedactRules: rawStringListFlag{`ACME-[0-9]+`},
		})
	})

	assert.Contains(t, output, "scope=store")
	assert.Contains(t, output, "legacy")
	assert.NotContains(t, output, secret)
	assert.Contains(t, output, "[REDACTED:custom_1]")
}

//nolint:paralleltest // Captures process-wide stdout.
func TestRunMemoryCommandListCorpusRedactsCorpusMetadata(t *testing.T) {
	const secret = "ACME-12345"

	dir := t.TempDir()
	storePath := filepath.Join(dir, "memory.json")
	mem := memory.NewStore()
	mem.Corpus = memory.CorpusMetadata{Scope: memory.ScopeRepo, RepoPath: filepath.Join(dir, secret), Tags: []string{secret}}
	require.NoError(t, mem.Add(memory.Document{
		ID:   "session/" + secret + "/message/0",
		Text: "OAuth corpus note",
		Metadata: map[string]string{
			"session_id": secret,
			"repo_path":  filepath.Join(dir, secret),
			"tags":       secret,
		},
		Provenance: &memory.Provenance{SessionID: secret, RepoPath: filepath.Join(dir, secret), Tags: []string{secret}},
	}))
	require.NoError(t, mem.Save(storePath))

	output := captureMemoryStdout(t, func() error {
		return runMemoryCommand(session.NewStore(filepath.Join(dir, "sessions")), cliOptions{
			memoryStorePath:   storePath,
			memoryListCorpus:  true,
			memoryRedactRules: rawStringListFlag{`ACME-[0-9]+`},
		})
	})

	assert.Contains(t, output, "Memory corpus:")
	assert.NotContains(t, output, secret)
	assert.Contains(t, output, "[REDACTED:custom_1]")
}

//nolint:paralleltest // Captures process-wide stdout.
func TestRunMemoryCommandListCorpusWithScopeDoesNotLoadSessions(t *testing.T) {
	dir := t.TempDir()
	storePath := filepath.Join(dir, "memory.json")

	mem := memory.NewStore()
	require.NoError(t, mem.Add(memory.Document{
		ID:         "session/stored/message/0",
		Text:       "OAuth stored memory",
		Metadata:   map[string]string{"session_id": "stored", "repo_path": dir},
		Provenance: &memory.Provenance{SessionID: "stored", RepoPath: dir},
	}))
	require.NoError(t, mem.Save(storePath))

	store := session.NewStore(filepath.Join(dir, "sessions"))
	require.NoError(t, os.MkdirAll(store.Dir(), 0o750))
	malformed, err := json.Marshal(map[string]any{
		"id":            "malformed-local",
		"created_at":    time.Now().UTC(),
		"updated_at":    time.Now().UTC(),
		"worktree_path": dir,
		"messages": []map[string]any{
			{"role": "user", "content": 123},
		},
	})
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(store.Path("malformed-local"), append(malformed, '\n'), 0o600))

	output := captureMemoryStdout(t, func() error {
		return runMemoryCommand(store, cliOptions{
			memoryStorePath:  storePath,
			memoryListCorpus: true,
			memoryRepoPath:   dir,
		})
	})

	assert.Contains(t, output, "Memory corpus:")
	assert.Contains(t, output, "sessions=stored")
}

//nolint:paralleltest // Captures process-wide stdout.
func TestRunMemoryCommandRedactsStatusStorePath(t *testing.T) {
	const secret = "ACME-12345"

	dir := t.TempDir()
	notePath := filepath.Join(dir, "note.txt")
	require.NoError(t, os.WriteFile(notePath, []byte("OAuth callback notes\n"), 0o600))

	storePath := filepath.Join(dir, secret, "memory.json")
	output := captureMemoryStdout(t, func() error {
		return runMemoryCommand(session.NewStore(filepath.Join(dir, "sessions")), cliOptions{
			memoryStorePath:   storePath,
			memoryIndexFiles:  stringListFlag{notePath},
			memoryRedactRules: rawStringListFlag{`ACME-[0-9]+`},
		})
	})

	assert.Contains(t, output, "Indexed")
	assert.NotContains(t, output, secret)
	assert.Contains(t, output, "[REDACTED:custom_1]")
}

func TestRunMemoryCommandIndexWithoutSearchRequiresStoreBeforeSessionLoad(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	notePath := filepath.Join(dir, "note.txt")
	require.NoError(t, os.WriteFile(notePath, []byte("OAuth callback notes\n"), 0o600))

	store := session.NewStore(filepath.Join(dir, "sessions"))
	require.NoError(t, os.MkdirAll(store.Dir(), 0o750))
	require.NoError(t, os.WriteFile(filepath.Join(store.Dir(), "malformed.json"), []byte("{not-json"), 0o600))

	for _, opts := range []cliOptions{
		{memoryIndexFiles: stringListFlag{notePath}},
		{memoryIndexFiles: stringListFlag{notePath}, memoryListCorpus: true},
	} {
		err := runMemoryCommand(store, opts)

		require.Error(t, err)
		assert.Contains(t, err.Error(), "--memory-store is required")
		assert.NotContains(t, err.Error(), "malformed")
		assert.NotContains(t, err.Error(), "parse")
	}
}

//nolint:paralleltest // Exercises store maintenance command flow.
func TestRunMemoryCommandIndexOnlyWithScopeDoesNotLoadSessions(t *testing.T) {
	dir := t.TempDir()
	notePath := filepath.Join(dir, "note.txt")
	storePath := filepath.Join(dir, "memory.json")
	require.NoError(t, os.WriteFile(notePath, []byte("OAuth callback notes\n"), 0o600))

	store := session.NewStore(filepath.Join(dir, "sessions"))
	require.NoError(t, os.MkdirAll(store.Dir(), 0o750))
	malformed, err := json.Marshal(map[string]any{
		"id":            "malformed-local",
		"created_at":    time.Now().UTC(),
		"updated_at":    time.Now().UTC(),
		"worktree_path": dir,
		"messages": []map[string]any{
			{"role": "user", "content": 123},
		},
	})
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(store.Path("malformed-local"), append(malformed, '\n'), 0o600))

	output := captureMemoryStdout(t, func() error {
		return runMemoryCommand(store, cliOptions{
			memoryStorePath:  storePath,
			memoryIndexFiles: stringListFlag{notePath},
			memoryRepoPath:   dir,
		})
	})
	assert.Contains(t, output, "Indexed 1 document")

	loaded, err := memory.Load(storePath)
	require.NoError(t, err)
	require.Len(t, loaded.Documents, 1)
	assert.Empty(t, loaded.Corpus.SessionIDs)
	assert.Equal(t, memory.ScopeRepo, loaded.Corpus.Scope)
}

//nolint:paralleltest // Captures process-wide stdout.
func TestRunMemoryCommandIndexOnlyReportsNewIndexCount(t *testing.T) {
	dir := t.TempDir()
	existingPath := filepath.Join(dir, "existing.txt")
	notePath := filepath.Join(dir, "note.txt")
	storePath := filepath.Join(dir, "memory.json")
	require.NoError(t, os.WriteFile(existingPath, []byte("Existing OAuth callback notes\n"), 0o600))
	require.NoError(t, os.WriteFile(notePath, []byte("New OAuth callback notes\n"), 0o600))

	mem := memory.NewStore()
	require.NoError(t, mem.AddFile(existingPath))
	require.NoError(t, mem.Save(storePath))

	output := captureMemoryStdout(t, func() error {
		return runMemoryCommand(session.NewStore(filepath.Join(dir, "sessions")), cliOptions{
			memoryStorePath:  storePath,
			memoryIndexFiles: stringListFlag{notePath},
		})
	})
	assert.Contains(t, output, "Indexed 1 document(s)")
	assert.NotContains(t, output, "Indexed 2 document(s)")

	loaded, err := memory.Load(storePath)
	require.NoError(t, err)
	require.Len(t, loaded.Documents, 2)
}

//nolint:paralleltest // Uses t.Chdir and captures process-wide stdout.
func TestRunMemoryCommandSearchIncludesExplicitIndexFilesWithStore(t *testing.T) {
	repoDir := t.TempDir()
	require.NoError(t, os.Mkdir(filepath.Join(repoDir, ".git"), 0o750))
	t.Chdir(repoDir)

	externalDir := t.TempDir()
	notePath := filepath.Join(externalDir, "note.txt")
	storePath := filepath.Join(repoDir, "memory.json")
	require.NoError(t, os.WriteFile(notePath, []byte("External OAuth callback notes\n"), 0o600))

	otherRepo := filepath.Join(t.TempDir(), "other-repo")
	mem := memory.NewStore()
	require.NoError(t, mem.Add(memory.Document{
		ID:         "session/other/message/0",
		Text:       "External OAuth other stored memory",
		Metadata:   map[string]string{"session_id": otherMemoryName, "repo_path": otherRepo},
		Provenance: &memory.Provenance{SessionID: otherMemoryName, RepoPath: otherRepo},
	}))
	require.NoError(t, mem.Save(storePath))

	output := captureMemoryStdout(t, func() error {
		return runMemoryCommand(session.NewStore(filepath.Join(repoDir, "sessions")), cliOptions{
			memoryStorePath:  storePath,
			memoryIndexFiles: stringListFlag{notePath},
			memoryRepoPath:   repoDir,
			memorySearch:     "external oauth",
		})
	})

	assert.Contains(t, output, "Searched corpus:")
	assert.Contains(t, output, filepath.Clean(notePath))
	assert.NotContains(t, output, "session/other/message/0")
	assert.NotContains(t, output, "No memory results found.")

	loaded, err := memory.Load(storePath)
	require.NoError(t, err)
	require.Len(t, loaded.Documents, 2)
	assert.NotNil(t, findMemoryDocumentByPath(loaded, filepath.Clean(notePath)))
}

//nolint:paralleltest // Exercises process-level command error formatting.
func TestRunMemoryCommandRedactsErrorMessages(t *testing.T) {
	const secret = "ACME-12345"

	dir := t.TempDir()
	storePath := filepath.Join(dir, secret, "memory.json")
	require.NoError(t, os.MkdirAll(filepath.Dir(storePath), 0o750))
	require.NoError(t, os.WriteFile(storePath, []byte("{not-json"), 0o600))

	err := runMemoryCommand(session.NewStore(filepath.Join(dir, "sessions")), cliOptions{
		memoryStorePath:   storePath,
		memorySearch:      "oauth",
		memoryRedactRules: rawStringListFlag{`ACME-[0-9]+`},
	})

	require.Error(t, err)
	assert.NotContains(t, err.Error(), secret)
	assert.Contains(t, err.Error(), "[REDACTED:custom_1]")
}

func TestRunMemoryCommandRedactsInvalidRedactionRuleErrors(t *testing.T) {
	t.Parallel()

	const secret = "sk-1234567890abcdefSECRET"

	err := runMemoryCommand(session.NewStore(filepath.Join(t.TempDir(), "sessions")), cliOptions{
		memorySearch:      "oauth",
		memoryRedactRules: rawStringListFlag{"(" + secret},
	})

	require.Error(t, err)
	assert.NotContains(t, err.Error(), secret)
	assert.Contains(t, err.Error(), "[REDACTED:openai_api_key]")
}

func TestRedactMemoryCommandErrorPreservesCause(t *testing.T) {
	t.Parallel()

	const secret = "ACME-12345"
	cause := errors.New("failed for " + secret)
	redactor, err := memory.NewRedactor(`ACME-[0-9]+`)
	require.NoError(t, err)

	redacted := redactMemoryCommandError(redactor, fmt.Errorf("memory: load store: %w", cause))

	require.Error(t, redacted)
	require.ErrorIs(t, redacted, cause)
	assert.NotContains(t, redacted.Error(), secret)
	assert.Contains(t, redacted.Error(), "[REDACTED:custom_1]")
}

//nolint:paralleltest // Captures process-wide stdout.
func TestRunMemoryCommandRebuildListCorpusAndPurge(t *testing.T) {
	dir := t.TempDir()
	storePath := filepath.Join(dir, "memory.json")
	store := session.NewStore(filepath.Join(dir, "sessions"))
	saved := session.Session{
		ID:           "demo",
		CreatedAt:    time.Date(2026, 5, 1, 12, 0, 0, 0, time.UTC),
		UpdatedAt:    time.Date(2026, 5, 1, 12, 1, 0, 0, time.UTC),
		DefaultAgent: testReviewerName,
		WorktreePath: dir,
		Tags:         []string{"auth"},
		Messages:     []llm.Message{{Role: llm.RoleUser, Content: "OAuth rebuild note"}},
	}
	require.NoError(t, store.Save(saved))

	rebuildOutput := captureMemoryStdout(t, func() error {
		return runMemoryCommand(store, cliOptions{
			memoryStorePath: storePath,
			memoryRebuild:   true,
			memoryRepoPath:  dir,
			memorySearch:    "OAuth",
		})
	})
	assert.Contains(t, rebuildOutput, "Rebuilt memory store")
	assert.Contains(t, rebuildOutput, "Searched corpus:")
	assert.Contains(t, rebuildOutput, "session/demo/message/0")

	loaded, err := memory.Load(storePath)
	require.NoError(t, err)
	assert.Equal(t, memory.SchemaVersion, loaded.SchemaVersion)
	assert.Equal(t, memory.ScopeRepo, loaded.Corpus.Scope)
	assert.False(t, loaded.CreatedAt.IsZero())
	assert.False(t, loaded.UpdatedAt.IsZero())
	require.NotEmpty(t, loaded.Documents)

	listOutput := captureMemoryStdout(t, func() error {
		return runMemoryCommand(store, cliOptions{memoryStorePath: storePath, memoryListCorpus: true})
	})
	assert.Contains(t, listOutput, "Memory corpus:")
	assert.Contains(t, listOutput, "scope=repo")
	assert.Contains(t, listOutput, "created_from=sessions")

	purgeOutput := captureMemoryStdout(t, func() error {
		return runMemoryCommand(store, cliOptions{memoryStorePath: storePath, memoryPurgeSpec: "session:demo"})
	})
	assert.Contains(t, purgeOutput, "Purged")

	loaded, err = memory.Load(storePath)
	require.NoError(t, err)
	assert.Empty(t, loaded.Documents)
}

//nolint:paralleltest // Captures process-wide stdout.
func TestRunMemoryCommandRebuildPersistsEmptySelectedCorpus(t *testing.T) {
	dir := t.TempDir()
	expectedRepo := cleanMemoryPath(dir)
	storePath := filepath.Join(dir, "memory.json")

	output := captureMemoryStdout(t, func() error {
		return runMemoryCommand(session.NewStore(filepath.Join(dir, "sessions")), cliOptions{
			memoryStorePath: storePath,
			memoryRebuild:   true,
			memoryRepoPath:  dir,
		})
	})
	assert.Contains(t, output, "Rebuilt memory store")
	assert.Contains(t, output, "scope=repo")
	assert.Contains(t, output, "repo="+expectedRepo)

	loaded, err := memory.Load(storePath)
	require.NoError(t, err)
	assert.Equal(t, memory.ScopeRepo, loaded.Corpus.Scope)
	assert.Equal(t, expectedRepo, loaded.Corpus.RepoPath)
	assert.Zero(t, loaded.Corpus.DocumentCount)
	assert.Empty(t, loaded.Documents)
}

//nolint:paralleltest // Captures process-wide stdout.
func TestRunMemoryCommandPurgeTagRepoAndAllSelectors(t *testing.T) {
	dir := t.TempDir()
	storePath := filepath.Join(dir, "memory.json")
	repoDocs := filepath.Join(dir, "docs-repo")
	repoMisc := filepath.Join(dir, "misc-repo")

	mem := memory.NewStore()
	require.NoError(t, mem.Add(memory.Document{
		ID:         "session/auth/message/0",
		Text:       "OAuth auth memory",
		Metadata:   map[string]string{"session_id": "auth", "tags": "auth", "repo_path": filepath.Join(dir, "auth-repo")},
		Provenance: &memory.Provenance{SessionID: "auth", Tags: []string{"auth"}, RepoPath: filepath.Join(dir, "auth-repo")},
	}))
	require.NoError(t, mem.Add(memory.Document{
		ID:         "session/docs/message/0",
		Text:       "OAuth docs memory",
		Metadata:   map[string]string{"session_id": "docs", "tags": "docs", "repo_path": repoDocs},
		Provenance: &memory.Provenance{SessionID: "docs", Tags: []string{"docs"}, RepoPath: repoDocs},
	}))
	require.NoError(t, mem.Add(memory.Document{
		ID:         filepath.Join(repoDocs, "note.txt"),
		Path:       filepath.Join(repoDocs, "note.txt"),
		Text:       "OAuth docs file",
		Metadata:   map[string]string{"source_type": memory.ScopeFile, "kind": memory.ScopeFile, "path": filepath.Join(repoDocs, "note.txt")},
		Provenance: &memory.Provenance{SourceType: memory.ScopeFile, Path: filepath.Join(repoDocs, "note.txt")},
	}))
	require.NoError(t, mem.Add(memory.Document{
		ID:         "session/misc/message/0",
		Text:       "OAuth misc memory",
		Metadata:   map[string]string{"session_id": "misc", "tags": "misc", "repo_path": repoMisc},
		Provenance: &memory.Provenance{SessionID: "misc", Tags: []string{"misc"}, RepoPath: repoMisc},
	}))
	require.NoError(t, mem.Save(storePath))

	tagOutput := captureMemoryStdout(t, func() error {
		return runMemoryCommand(session.NewStore(filepath.Join(dir, "sessions")), cliOptions{
			memoryStorePath: storePath,
			memoryPurgeSpec: "tag:auth",
		})
	})
	assert.Contains(t, tagOutput, "Purged 1 memory document")

	loaded, err := memory.Load(storePath)
	require.NoError(t, err)
	assert.Len(t, loaded.Documents, 3)
	assert.NotContains(t, loaded.Corpus.SessionIDs, "auth")

	repoOutput := captureMemoryStdout(t, func() error {
		return runMemoryCommand(session.NewStore(filepath.Join(dir, "sessions")), cliOptions{
			memoryStorePath: storePath,
			memoryPurgeSpec: "repo:" + repoDocs,
		})
	})
	assert.Contains(t, repoOutput, "Purged 2 memory document")

	loaded, err = memory.Load(storePath)
	require.NoError(t, err)
	assert.Len(t, loaded.Documents, 1)
	assert.Equal(t, []string{"misc"}, loaded.Corpus.SessionIDs)

	allOutput := captureMemoryStdout(t, func() error {
		return runMemoryCommand(session.NewStore(filepath.Join(dir, "sessions")), cliOptions{
			memoryStorePath: storePath,
			memoryPurgeSpec: "all",
		})
	})
	assert.Contains(t, allOutput, "Purged 1 memory document")

	loaded, err = memory.Load(storePath)
	require.NoError(t, err)
	assert.Empty(t, loaded.Documents)
}

//nolint:paralleltest // Captures process-wide stdout.
func TestRunMemoryCommandPurgeRepoNormalizesSubdirToGitRoot(t *testing.T) {
	dir := t.TempDir()
	require.NoError(t, os.Mkdir(filepath.Join(dir, ".git"), 0o750))
	subdir := filepath.Join(dir, "cmd", "atteler")
	require.NoError(t, os.MkdirAll(subdir, 0o750))

	storePath := filepath.Join(dir, "memory.json")
	mem := memory.NewStore()
	require.NoError(t, mem.Add(memory.Document{
		ID:         "session/local/message/0",
		Text:       "OAuth local repo memory",
		Metadata:   map[string]string{"session_id": localMemorySessionID, "repo_path": dir},
		Provenance: &memory.Provenance{SessionID: localMemorySessionID, RepoPath: dir},
	}))
	require.NoError(t, mem.Save(storePath))

	output := captureMemoryStdout(t, func() error {
		return runMemoryCommand(session.NewStore(filepath.Join(dir, "sessions")), cliOptions{
			memoryStorePath: storePath,
			memoryPurgeSpec: "repo:" + subdir,
		})
	})
	assert.Contains(t, output, "Purged 1 memory document")

	loaded, err := memory.Load(storePath)
	require.NoError(t, err)
	assert.Empty(t, loaded.Documents)
}

//nolint:paralleltest // Captures process-wide stdout.
func TestRunMemoryCommandPurgeRepoMatchesLegacyFileIDPath(t *testing.T) {
	dir := t.TempDir()
	notePath := filepath.Join(dir, "note.txt")
	storePath := filepath.Join(dir, "memory.json")

	legacyStore, err := json.Marshal(map[string]any{
		"documents": []map[string]any{
			{
				"id":       notePath,
				"text":     "legacy OAuth file memory",
				"metadata": map[string]string{"source_type": memory.ScopeFile, "kind": memory.ScopeFile},
			},
		},
	})
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(storePath, append(legacyStore, '\n'), 0o600))

	output := captureMemoryStdout(t, func() error {
		return runMemoryCommand(session.NewStore(filepath.Join(dir, "sessions")), cliOptions{
			memoryStorePath: storePath,
			memoryPurgeSpec: "repo:" + dir,
		})
	})
	assert.Contains(t, output, "Purged 1 memory document")

	loaded, err := memory.Load(storePath)
	require.NoError(t, err)
	assert.Empty(t, loaded.Documents)
}

func TestRunMemoryCommandRejectsPurgeWithRebuild(t *testing.T) {
	t.Parallel()

	err := runMemoryCommand(session.NewStore(filepath.Join(t.TempDir(), "sessions")), cliOptions{
		memoryStorePath: filepath.Join(t.TempDir(), "memory.json"),
		memoryPurgeSpec: "session:demo",
		memoryRebuild:   true,
	})

	require.Error(t, err)
	assert.Contains(t, err.Error(), "cannot be combined")
}

//nolint:paralleltest // Captures process-wide stdout.
func TestRunMemoryCommandAppliesRetentionToPersistedStore(t *testing.T) {
	dir := t.TempDir()
	storePath := filepath.Join(dir, "memory.json")

	mem := memory.NewStore()
	oldTime := time.Now().UTC().AddDate(0, 0, -60)
	newTime := time.Now().UTC()
	mem.Corpus = memory.CorpusMetadata{
		Scope:       memory.ScopeDateRange,
		DateStart:   oldTime.AddDate(0, 0, -1).Format(time.RFC3339),
		DateEnd:     oldTime.AddDate(0, 0, 1).Format(time.RFC3339),
		Description: "scope=date_range date_range=stale..stale",
	}
	require.NoError(t, mem.Add(memory.Document{
		ID:         "session/old/message/0",
		Text:       "old OAuth memory",
		Provenance: &memory.Provenance{SessionID: "old", UpdatedAt: oldTime.Format(time.RFC3339)},
		Metadata:   map[string]string{"session_id": "old", "updated_at": oldTime.Format(time.RFC3339)},
	}))
	require.NoError(t, mem.Add(memory.Document{
		ID:         "session/new/message/0",
		Text:       "new OAuth memory",
		Provenance: &memory.Provenance{SessionID: "new", UpdatedAt: newTime.Format(time.RFC3339)},
		Metadata:   map[string]string{"session_id": "new", "updated_at": newTime.Format(time.RFC3339)},
	}))
	require.NoError(t, mem.Save(storePath))

	output := captureMemoryStdout(t, func() error {
		return runMemoryCommand(session.NewStore(filepath.Join(dir, "sessions")), cliOptions{
			memoryStorePath:     storePath,
			memoryRetentionDays: positiveIntFlag{value: 30, set: true},
		})
	})
	assert.Contains(t, output, "Applied 30 days memory retention")

	loaded, err := memory.Load(storePath)
	require.NoError(t, err)
	require.Len(t, loaded.Documents, 1)
	assert.Equal(t, "session/new/message/0", loaded.Documents[0].ID)
	require.NotNil(t, loaded.Documents[0].Policy)
	assert.Equal(t, "30 days", loaded.Documents[0].Policy.Retention)
	assert.Equal(t, "30 days", loaded.Corpus.Retention)
	assert.Empty(t, loaded.Corpus.DateEnd)
	assert.NotContains(t, loaded.Corpus.Description, "stale")
	assert.Contains(t, loaded.Corpus.Description, "retention=30 days")
}

//nolint:paralleltest // Captures process-wide stdout.
func TestRunMemoryCommandRetentionPersistsEmptyCorpusPolicy(t *testing.T) {
	dir := t.TempDir()
	storePath := filepath.Join(dir, "memory.json")
	oldTime := time.Now().UTC().AddDate(0, 0, -60)

	mem := memory.NewStore()
	require.NoError(t, mem.Add(memory.Document{
		ID:         "session/old/message/0",
		Text:       "old OAuth memory",
		Metadata:   map[string]string{"session_id": "old", "updated_at": oldTime.Format(time.RFC3339)},
		Provenance: &memory.Provenance{SessionID: "old", UpdatedAt: oldTime.Format(time.RFC3339)},
	}))
	require.NoError(t, mem.Save(storePath))

	output := captureMemoryStdout(t, func() error {
		return runMemoryCommand(session.NewStore(filepath.Join(dir, "sessions")), cliOptions{
			memoryStorePath:     storePath,
			memoryRetentionDays: positiveIntFlag{value: 30, set: true},
		})
	})
	assert.Contains(t, output, "Applied 30 days memory retention")

	loaded, err := memory.Load(storePath)
	require.NoError(t, err)
	assert.Empty(t, loaded.Documents)
	assert.Equal(t, memory.ScopeManual, loaded.Corpus.Scope)
	assert.Equal(t, "30 days", loaded.Corpus.Retention)
	assert.NotEmpty(t, loaded.Corpus.DateStart)
	assert.Contains(t, loaded.Corpus.Description, "retention=30 days")
}

//nolint:paralleltest // Captures process-wide stdout.
func TestRunMemoryCommandCombinesPurgeAndRetention(t *testing.T) {
	dir := t.TempDir()
	storePath := filepath.Join(dir, "memory.json")
	oldTime := time.Now().UTC().AddDate(0, 0, -60)
	newTime := time.Now().UTC()

	mem := memory.NewStore()
	require.NoError(t, mem.Add(memory.Document{
		ID:         "session/tagged/message/0",
		Text:       "tagged OAuth memory",
		Metadata:   map[string]string{"session_id": "tagged", "tags": "auth", "updated_at": newTime.Format(time.RFC3339)},
		Provenance: &memory.Provenance{SessionID: "tagged", Tags: []string{"auth"}, UpdatedAt: newTime.Format(time.RFC3339)},
	}))
	require.NoError(t, mem.Add(memory.Document{
		ID:         "session/old/message/0",
		Text:       "old OAuth memory",
		Metadata:   map[string]string{"session_id": "old", "updated_at": oldTime.Format(time.RFC3339)},
		Provenance: &memory.Provenance{SessionID: "old", UpdatedAt: oldTime.Format(time.RFC3339)},
	}))
	require.NoError(t, mem.Add(memory.Document{
		ID:         "session/new/message/0",
		Text:       "new OAuth memory",
		Metadata:   map[string]string{"session_id": "new", "updated_at": newTime.Format(time.RFC3339)},
		Provenance: &memory.Provenance{SessionID: "new", UpdatedAt: newTime.Format(time.RFC3339)},
	}))
	require.NoError(t, mem.Save(storePath))

	output := captureMemoryStdout(t, func() error {
		return runMemoryCommand(session.NewStore(filepath.Join(dir, "sessions")), cliOptions{
			memoryStorePath:     storePath,
			memoryPurgeSpec:     "tag:auth",
			memoryRetentionDays: positiveIntFlag{value: 30, set: true},
		})
	})
	assert.Contains(t, output, "Purged 1 memory document")
	assert.Contains(t, output, "Applied 30 days memory retention")

	loaded, err := memory.Load(storePath)
	require.NoError(t, err)
	require.Len(t, loaded.Documents, 1)
	assert.Equal(t, "session/new/message/0", loaded.Documents[0].ID)
	assert.Equal(t, []string{"new"}, loaded.Corpus.SessionIDs)
}

//nolint:paralleltest // Captures process-wide stdout.
func TestRunMemoryCommandPurgeWithExplicitScopeDoesNotReindexSession(t *testing.T) {
	dir := t.TempDir()
	storePath := filepath.Join(dir, "memory.json")

	now := time.Now().UTC()
	mem := memory.NewStore()
	require.NoError(t, mem.Add(memory.Document{
		ID:         "session/local/message/0",
		Text:       "stored OAuth memory",
		Metadata:   map[string]string{"session_id": localMemorySessionID, "updated_at": now.Format(time.RFC3339)},
		Provenance: &memory.Provenance{SessionID: localMemorySessionID, UpdatedAt: now.Format(time.RFC3339)},
	}))
	require.NoError(t, mem.Save(storePath))

	store := session.NewStore(filepath.Join(dir, "sessions"))
	saved := session.New("gpt-test", []llm.Message{{Role: llm.RoleUser, Content: "saved OAuth memory should not return after purge"}})
	saved.ID = localMemorySessionID
	require.NoError(t, store.Save(saved))

	output := captureMemoryStdout(t, func() error {
		return runMemoryCommand(store, cliOptions{
			memoryStorePath:     storePath,
			memoryPurgeSpec:     "session:local",
			memorySessionRef:    localMemorySessionID,
			memoryRetentionDays: positiveIntFlag{value: 30, set: true},
		})
	})
	assert.Contains(t, output, "Purged 1 memory document")
	assert.Contains(t, output, "Applied 30 days memory retention")

	loaded, err := memory.Load(storePath)
	require.NoError(t, err)
	assert.Empty(t, loaded.Documents)
	assert.Empty(t, loaded.Corpus.SessionIDs)
}

func TestRedactedMemoryMatchPlanDoesNotMutateOriginalTags(t *testing.T) {
	t.Parallel()

	redactor, err := memory.NewRedactor(`ACME-[0-9]+`)
	require.NoError(t, err)

	plan := memoryCorpusPlan{tags: []string{"ACME-12345"}}
	redacted := redactedMemoryMatchPlan(plan, redactor)

	assert.Equal(t, []string{"ACME-12345"}, plan.tags)
	require.Len(t, redacted.tags, 1)
	assert.Contains(t, redacted.tags[0], "[REDACTED:custom_1]")
	assert.NotContains(t, redacted.tags[0], "ACME-12345")
}

//nolint:paralleltest // Captures process-wide stdout.
func TestRunMemoryCommandRetentionOnlyDoesNotIndexOrConstrainSessions(t *testing.T) {
	dir := t.TempDir()
	storePath := filepath.Join(dir, "memory.json")
	store := session.NewStore(filepath.Join(dir, "sessions"))

	mem := memory.NewStore()
	now := time.Now().UTC()
	require.NoError(t, mem.Add(memory.Document{
		ID:         "stored-other-repo",
		Text:       "stored OAuth memory",
		Provenance: &memory.Provenance{RepoPath: filepath.Join(dir, otherMemoryName), UpdatedAt: now.Format(time.RFC3339)},
		Metadata:   map[string]string{"repo_path": filepath.Join(dir, otherMemoryName), "updated_at": now.Format(time.RFC3339)},
	}))
	require.NoError(t, mem.Save(storePath))

	local := session.New("gpt-test", []llm.Message{{Role: llm.RoleUser, Content: "local OAuth session should not be added"}})
	local.ID = "local-session"
	local.WorktreePath = dir
	require.NoError(t, store.Save(local))

	output := captureMemoryStdout(t, func() error {
		return runMemoryCommand(store, cliOptions{
			memoryStorePath:     storePath,
			memoryRepoPath:      dir,
			memoryRetentionDays: positiveIntFlag{value: 30, set: true},
		})
	})
	assert.Contains(t, output, "Applied 30 days memory retention")

	loaded, err := memory.Load(storePath)
	require.NoError(t, err)
	require.Len(t, loaded.Documents, 1)
	assert.Equal(t, "stored-other-repo", loaded.Documents[0].ID)
	assert.Empty(t, loaded.Corpus.SessionIDs)
}

func TestBuildMemoryStore_RetentionAppliesAfterSessionIndexing(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	store := session.NewStore(filepath.Join(dir, "sessions"))
	oldTime := time.Now().UTC().AddDate(0, 0, -60)
	old := session.Session{
		ID:           "old-session",
		CreatedAt:    oldTime,
		UpdatedAt:    oldTime,
		DefaultModel: "gpt-test",
		Messages:     []llm.Message{{Role: llm.RoleUser, Content: "expired OAuth memory"}},
	}
	writeMemorySessionFixture(t, store, old)

	mem, err := buildMemoryStore(store, cliOptions{
		memorySessionRef:    "old-session",
		memoryRetentionDays: positiveIntFlag{value: 30, set: true},
	})
	require.NoError(t, err)

	assert.Empty(t, mem.Documents)
	assert.Empty(t, mem.Corpus.SessionIDs)
	assert.Equal(t, "30 days", mem.Corpus.Retention)
	assert.Contains(t, mem.Corpus.Description, "retention=30 days")
}

func writeMemorySessionFixture(t *testing.T, store *session.Store, saved session.Session) {
	t.Helper()

	data, err := json.Marshal(saved)
	require.NoError(t, err)
	require.NoError(t, os.MkdirAll(store.Dir(), 0o750))
	require.NoError(t, os.WriteFile(filepath.Join(store.Dir(), saved.ID+".json"), data, 0o600))
}

func findMemoryDocumentByPath(store *memory.Store, path string) *memory.Document {
	if store == nil {
		return nil
	}

	for i := range store.Documents {
		if store.Documents[i].Path == path {
			return &store.Documents[i]
		}
	}

	return nil
}

func captureMemoryStdout(t *testing.T, fn func() error) string {
	t.Helper()

	oldStdout := os.Stdout
	reader, writer, err := os.Pipe()
	require.NoError(t, err)
	os.Stdout = writer

	runErr := fn()
	require.NoError(t, writer.Close())
	os.Stdout = oldStdout

	out, readErr := io.ReadAll(reader)
	require.NoError(t, readErr)
	require.NoError(t, runErr)

	return strings.TrimSpace(string(out))
}
