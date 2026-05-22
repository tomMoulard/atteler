package main

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/tommoulard/atteler/pkg/agentmemory"
	"github.com/tommoulard/atteler/pkg/llm"
	"github.com/tommoulard/atteler/pkg/retrieval"
	"github.com/tommoulard/atteler/pkg/session"
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

func TestParseContextPackMessagesMapsDeveloperAndToolRoles(t *testing.T) {
	t.Parallel()

	messages, metadata := parseContextPackMessagesWithMetadata("developer[2026-05-22T10:00:00Z]: keep secret policy\ntool: grep output\nuser: continue\n")

	require.Len(t, messages, 3)
	require.Len(t, metadata, 3)
	assert.Equal(t, llm.RoleSystem, messages[0].Role)
	assert.Equal(t, "keep secret policy", messages[0].Content)
	assert.Equal(t, "2026-05-22T10:00:00Z", metadata[0].Timestamp)
	assert.Equal(t, llm.RoleTool, messages[1].Role)
	assert.Equal(t, "grep output", messages[1].Content)
	assert.Empty(t, metadata[1].Timestamp)
	assert.Equal(t, llm.RoleUser, messages[2].Role)
}

func TestParseContextPackMessagesDoesNotMistakeContentBracketForTimestamp(t *testing.T) {
	t.Parallel()

	messages, metadata := parseContextPackMessagesWithMetadata("user: keep literal ]: marker in content\n")

	require.Len(t, messages, 1)
	require.Len(t, metadata, 1)
	assert.Equal(t, llm.RoleUser, messages[0].Role)
	assert.Equal(t, "keep literal ]: marker in content", messages[0].Content)
	assert.Empty(t, metadata[0].Timestamp)
}

func TestParseContextPackTimestampWithoutSpaceKeepsContent(t *testing.T) {
	t.Parallel()

	messages, metadata := parseContextPackMessagesWithMetadata("user[2026-05-22T10:00:00Z]:continue without losing first byte\n")

	require.Len(t, messages, 1)
	require.Len(t, metadata, 1)
	assert.Equal(t, llm.RoleUser, messages[0].Role)
	assert.Equal(t, "continue without losing first byte", messages[0].Content)
	assert.Equal(t, "2026-05-22T10:00:00Z", metadata[0].Timestamp)
}

func TestAddVectorFileRecordsRetrievalFreshness(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	path := filepath.Join(dir, "vector-note.txt")
	require.NoError(t, os.WriteFile(path, []byte("OAuth callback vector context"), 0o600))

	vectorizer, err := vector.NewTextVectorizer(32)
	require.NoError(t, err)
	store, err := vector.NewStore(vectorizer.Dimensions)
	require.NoError(t, err)
	require.NoError(t, addVectorFile(store, vectorizer, path))

	searcher := vector.Searcher{
		Store:      store,
		Vectorizer: vectorizer,
		Source:     retrieval.Source{Type: retrieval.SourceVector, Name: "fixture"},
	}

	results, err := searcher.SearchRetrieval(context.Background(), retrieval.Query{Text: "oauth callback", Limit: 1})
	require.NoError(t, err)
	require.Len(t, results, 1)
	assert.Equal(t, "current", results[0].Freshness.Status)
	assert.False(t, results[0].Freshness.Deleted)

	future := time.Now().Add(2 * time.Hour).UTC().Truncate(time.Second)
	require.NoError(t, os.Chtimes(path, future, future))

	results, err = searcher.SearchRetrieval(context.Background(), retrieval.Query{Text: "oauth callback", Limit: 1})
	require.NoError(t, err)
	require.Len(t, results, 1)
	assert.Equal(t, "stale", results[0].Freshness.Status)
	assert.False(t, results[0].Freshness.Deleted)

	require.NoError(t, os.Remove(path))

	results, err = searcher.SearchRetrieval(context.Background(), retrieval.Query{Text: "oauth callback", Limit: 1})
	require.NoError(t, err)
	require.Len(t, results, 1)
	assert.Equal(t, "deleted", results[0].Freshness.Status)
	assert.True(t, results[0].Freshness.Deleted)
}

func TestRunAgentMemoryCommandIndexesAndSearchesSelectedAgent(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()

	note := filepath.Join(dir, "note.txt")
	if err := os.WriteFile(note, []byte("OAuth callback retry memory"), 0o600); err != nil {
		require.NoError(t, err)
	}

	storePath := filepath.Join(dir, "agent-memory.json")

	err := runAgentMemoryCommand(dir, "reviewer", agentMemoryCommandInput{
		StorePath:  storePath,
		Search:     "callback retry",
		IndexFiles: []string{note},
		Limit:      1,
	})

	require.NoError(t, err)
	loaded, err := agentmemory.Load(storePath)
	require.NoError(t, err)
	results, err := loaded.Search("reviewer", "callback", 1)
	require.NoError(t, err)
	require.Len(t, results, 1)
	assert.Equal(t, filepath.Clean(note), results[0].Document.ID)
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

func TestSelectedRetrievalSources(t *testing.T) {
	t.Parallel()

	defaults, err := selectedRetrievalSources(retrievalCommandInput{})
	require.NoError(t, err)
	assert.Equal(t, []retrieval.SourceType{retrieval.SourceMemory, retrieval.SourceFile, retrieval.SourceSession}, defaults)

	got, err := selectedRetrievalSources(retrievalCommandInput{Sources: []string{"session", "git-history", "session"}})
	require.NoError(t, err)
	assert.Equal(t, []retrieval.SourceType{retrieval.SourceSession, retrieval.SourceGitHistory}, got)

	fileOnly, err := selectedRetrievalSources(retrievalCommandInput{Sources: []string{"file"}})
	require.NoError(t, err)
	assert.Equal(t, []retrieval.SourceType{retrieval.SourceFile}, fileOnly)

	all, err := selectedRetrievalSources(retrievalCommandInput{
		Sources:          []string{"all"},
		VectorIndexFiles: []string{"docs/a.md"},
		AgentName:        "reviewer",
	})
	require.NoError(t, err)
	assert.Contains(t, all, retrieval.SourceMemory)
	assert.Contains(t, all, retrieval.SourceFile)
	assert.Contains(t, all, retrieval.SourceSession)
	assert.Contains(t, all, retrieval.SourceGitHistory)
	assert.Contains(t, all, retrieval.SourceVector)
	assert.Contains(t, all, retrieval.SourceAgentMemory)

	_, err = selectedRetrievalSources(retrievalCommandInput{Sources: []string{"unknown"}})
	assert.Error(t, err)
}

func TestRetrievalFilters(t *testing.T) {
	t.Parallel()

	got, err := retrievalFilters([]string{"agent=reviewer", " source.name = gpt-review "})
	require.NoError(t, err)
	assert.Equal(t, map[string]string{"agent": "reviewer", "source.name": "gpt-review"}, got)

	got, err = retrievalFilters(nil)
	require.NoError(t, err)
	assert.Nil(t, got)

	_, err = retrievalFilters([]string{"missing-equals"})
	require.Error(t, err)

	_, err = retrievalFilters([]string{"=missing-key"})
	require.Error(t, err)
}

func TestRetrievalSearchersCanSearchSessionsOnceWithUnsafeOptIn(t *testing.T) {
	t.Parallel()

	store := session.NewStore(t.TempDir())
	saved := session.New("gpt-review", []llm.Message{{Role: llm.RoleUser, Content: "OAuth callback retry notes"}})
	saved.Title = "OAuth repair"
	require.NoError(t, store.Save(saved))

	input := retrievalCommandInput{Search: "oauth callback", Limit: 5}
	sources, err := selectedRetrievalSources(input)
	require.NoError(t, err)

	searchers, err := retrievalSearchers(context.Background(), appState{sessionStore: store, cwd: t.TempDir()}, input, sources)
	require.NoError(t, err)

	results, err := retrieval.Search(context.Background(), retrieval.Query{
		Text:          input.Search,
		Limit:         input.Limit,
		Sources:       sources,
		IncludeUnsafe: true,
	}, searchers...)
	require.NoError(t, err)
	require.Len(t, results, 1)
	assert.Equal(t, retrieval.SourceSession, results[0].Source.Type)
	assert.Equal(t, "session/"+saved.ID, results[0].DocumentID)
	assert.NotEmpty(t, results[0].Chunk.ID)
}

func TestRetrievalSearchersCanSearchAgentMemorySource(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	storePath := filepath.Join(dir, "agent-memory.json")

	store, err := agentmemory.NewStore(32)
	require.NoError(t, err)
	require.NoError(t, store.AddText("reviewer", "oauth-note", "OAuth callback retry guidance for reviewer memory"))
	require.NoError(t, store.Save(storePath))

	input := retrievalCommandInput{
		Search:               "oauth callback",
		Sources:              []string{"agent-memory"},
		AgentName:            "reviewer",
		AgentMemoryStorePath: storePath,
		Limit:                1,
	}
	sources, err := selectedRetrievalSources(input)
	require.NoError(t, err)

	searchers, err := retrievalSearchers(context.Background(), appState{sessionStore: session.NewStore(dir), cwd: dir}, input, sources)
	require.NoError(t, err)

	results, err := retrieval.Search(context.Background(), retrieval.Query{
		Text:    input.Search,
		Limit:   input.Limit,
		Sources: sources,
	}, searchers...)
	require.NoError(t, err)
	require.Len(t, results, 1)
	assert.Equal(t, retrieval.SourceAgentMemory, results[0].Source.Type)
	assert.Equal(t, "reviewer", results[0].Source.Name)
	assert.Equal(t, "oauth-note", results[0].DocumentID)
	assert.True(t, results[0].Safety.InjectAllowed)
	assert.NotEmpty(t, results[0].Metadata[retrieval.MetadataStableID])
}

func TestRetrievalSearchersAgentMemoryDefaultsToSelectedAgent(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	storePath := filepath.Join(dir, "agent-memory.json")

	store, err := agentmemory.NewStore(32)
	require.NoError(t, err)
	require.NoError(t, store.AddText("reviewer", "selected-note", "OAuth callback retry guidance for selected reviewer"))
	require.NoError(t, store.Save(storePath))

	input := retrievalCommandInput{
		Search:               "oauth callback",
		Sources:              []string{"agent-memory"},
		AgentMemoryStorePath: storePath,
		Limit:                1,
	}
	sources, err := selectedRetrievalSources(input)
	require.NoError(t, err)

	searchers, err := retrievalSearchers(context.Background(), appState{
		sessionStore:   session.NewStore(dir),
		cwd:            dir,
		selectedAgent:  "reviewer",
		selectedModel:  "gpt-test",
		fallbackModels: []string{"gpt-test"},
	}, input, sources)
	require.NoError(t, err)

	results, err := retrieval.Search(context.Background(), retrieval.Query{
		Text:    input.Search,
		Limit:   input.Limit,
		Sources: sources,
	}, searchers...)
	require.NoError(t, err)
	require.Len(t, results, 1)
	assert.Equal(t, retrieval.SourceAgentMemory, results[0].Source.Type)
	assert.Equal(t, "reviewer", results[0].Source.Name)
	assert.Equal(t, "selected-note", results[0].DocumentID)
}

func TestFormatRetrievalResultIncludesSafetyAndExplanation(t *testing.T) {
	t.Parallel()

	got := formatRetrievalResult(retrieval.Result{
		Source:     retrieval.Source{Type: retrieval.SourceSession, Name: "session-1", URI: "/tmp/session-1.json"},
		DocumentID: "session/session-1/message/0",
		Chunk: retrieval.Chunk{
			ID:    "chunk-1",
			Range: retrieval.Range{Unit: retrieval.RangeUnitRuneOffset, Start: 4, End: 42},
		},
		Score: 0.75,
		Scorer: retrieval.Scorer{
			Name:        "lexical-token-overlap",
			Explanation: []string{"matched terms: oauth"},
		},
		Snippet: "OAuth callback [REDACTED]",
		Safety: retrieval.Safety{
			InjectAllowed: false,
			Reasons:       []string{"private session transcript", "credential-shaped assignment"},
			Private:       true,
			Sensitive:     true,
			Redacted:      true,
		},
		Freshness: retrieval.Freshness{
			Status:          "stale",
			Deleted:         true,
			SourceUpdatedAt: time.Date(2026, 5, 21, 10, 11, 12, 0, time.UTC),
			IndexedAt:       time.Date(2026, 5, 20, 9, 8, 7, 0, time.UTC),
		},
	}, true)

	for _, want := range []string{
		"source=session",
		"source_name=session-1",
		"source_uri=/tmp/session-1.json",
		"document=session/session-1/message/0",
		"score=0.7500",
		"scorer=lexical-token-overlap",
		"chunk=chunk-1",
		"range=rune_offset:4-42",
		"inject_allowed=false",
		"private=true",
		"sensitive=true",
		"redacted=true",
		"safety_reasons=private session transcript;credential-shaped assignment",
		"freshness=stale",
		"deleted=true",
		"source_updated_at=2026-05-21T10:11:12Z",
		"indexed_at=2026-05-20T09:08:07Z",
		"snippet=OAuth callback [REDACTED]",
		"why=matched terms: oauth",
	} {
		assert.Contains(t, got, want)
	}
}
