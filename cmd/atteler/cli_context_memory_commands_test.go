package main

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/tommoulard/atteler/pkg/agentmemory"
	"github.com/tommoulard/atteler/pkg/llm"
	"github.com/tommoulard/atteler/pkg/memory"
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

func TestAddVectorFileRecordsProvenance(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	path := filepath.Join(dir, "vector-note.txt")
	require.NoError(t, os.WriteFile(path, []byte("vector provenance memory"), 0o600))

	vectorizer, err := vector.NewTextVectorizer(16)
	require.NoError(t, err)

	store, err := vector.NewStoreWithVectorizer(vectorizer.Spec())
	require.NoError(t, err)
	require.NoError(t, addVectorFile(store, vectorizer, path))

	require.Len(t, store.Documents, 1)
	assert.Equal(t, "file", store.Documents[0].Provenance["source_type"])
	assert.Equal(t, filepath.Clean(path), store.Documents[0].Provenance["path"])
}

func TestRunMemoryCommandIndexesWithTTL(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()

	note := filepath.Join(dir, "ttl-memory.txt")
	require.NoError(t, os.WriteFile(note, []byte("short lived lexical memory"), 0o600))

	storePath := filepath.Join(dir, "memory.json")
	start := time.Now().UTC()

	err := runMemoryCommand(nil, memoryCommandInput{
		StorePath:  storePath,
		IndexFiles: []string{note},
		TTLSeconds: 60,
	})
	require.NoError(t, err)

	loaded, err := memory.Load(storePath)
	require.NoError(t, err)
	require.Len(t, loaded.Documents, 1)
	require.NotNil(t, loaded.Documents[0].ExpiresAt)
	assert.True(t, loaded.Documents[0].ExpiresAt.After(start))
	assert.Equal(t, "file", loaded.Documents[0].Provenance["source_type"])
	assert.Equal(t, filepath.Clean(note), loaded.Documents[0].Provenance["path"])

	assert.Equal(t, 1, loaded.Compact(loaded.Documents[0].ExpiresAt.Add(time.Nanosecond)))
	require.NoError(t, loaded.Save(storePath))

	persisted, err := os.ReadFile(storePath)
	require.NoError(t, err)
	assert.NotContains(t, string(persisted), "short lived lexical memory")
}

func TestRunMemoryCommandDeletesAndCompactsPersistedStore(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	storePath := filepath.Join(dir, "memory.json")
	expired := time.Now().UTC().Add(-time.Hour)

	store := memory.NewStore()
	require.NoError(t, store.AddText("delete-me", "obsolete lexical memory api_key=api-key-test-value"))
	require.NoError(t, store.Add(memory.Document{ID: "expired-me", Text: "expired lexical memory", ExpiresAt: &expired}))
	require.NoError(t, store.AddText("keep-me", "durable lexical memory"))

	data, err := json.MarshalIndent(store, "", "  ")
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(storePath, append(data, '\n'), 0o600))

	err = runMemoryCommand(nil, memoryCommandInput{
		StorePath: storePath,
		DeleteID:  "delete-me",
	})
	require.NoError(t, err)

	loaded, err := memory.Load(storePath)
	require.NoError(t, err)

	results, err := loaded.Search("obsolete", 10)
	require.NoError(t, err)
	assert.Empty(t, results)

	err = runMemoryCommand(nil, memoryCommandInput{
		StorePath: storePath,
		Compact:   true,
	})
	require.NoError(t, err)

	persisted, err := os.ReadFile(storePath)
	require.NoError(t, err)
	assert.NotContains(t, string(persisted), "delete-me")
	assert.NotContains(t, string(persisted), "obsolete lexical")
	assert.NotContains(t, string(persisted), "api-key-test-value")
	assert.NotContains(t, string(persisted), "expired-me")
	assert.NotContains(t, string(persisted), "expired lexical")
	assert.Contains(t, string(persisted), "keep-me")
}

func TestMemoryLifecycleMessagesRedactSensitiveIdentifiers(t *testing.T) {
	t.Parallel()

	rawID := "docs/access_token=doc123/note.md"
	rawStorePath := filepath.Join(t.TempDir(), "tenant", "access_token=store123", "memory.json")

	store := memory.NewStore()
	require.NoError(t, store.AddText(rawID, "sensitive lifecycle message memory"))

	changed, messages, err := mutateMemoryStore(store, memoryCommandInput{
		StorePath: rawStorePath,
		DeleteID:  rawID,
		Compact:   true,
		Migrate:   true,
	})
	require.NoError(t, err)
	assert.True(t, changed)

	got := strings.Join(messages, "\n")
	assert.NotContains(t, got, "doc123")
	assert.NotContains(t, got, "store123")
	assert.Contains(t, got, "[REDACTED]")
}

func TestRunMemoryCommandMigratesLegacyPersistedStore(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	storePath := filepath.Join(dir, "legacy-memory.json")

	store := memory.Store{
		Documents: []memory.Document{{
			ID:         "legacy-secret",
			Text:       "deploy password=hunter2 with api_key=abc123",
			Metadata:   map[string]string{"auth_token": "tok123"},
			Provenance: map[string]string{"source": "Authorization: Bearer openai-secret-value"},
		}},
	}

	data, err := json.MarshalIndent(store, "", "  ")
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(storePath, append(data, '\n'), 0o600))

	err = runMemoryCommand(nil, memoryCommandInput{
		StorePath: storePath,
		Migrate:   true,
	})
	require.NoError(t, err)

	loaded, err := memory.Load(storePath)
	require.NoError(t, err)
	require.Len(t, loaded.Documents, 1)
	assert.Equal(t, memory.StoreSchemaVersion, loaded.SchemaVersion)
	assert.NotEmpty(t, loaded.Documents[0].SourceHash)

	persisted, err := os.ReadFile(storePath)
	require.NoError(t, err)

	for _, raw := range []string{"hunter2", "abc123", "tok123", "openai-secret-value"} {
		assert.NotContains(t, string(persisted), raw)
	}
}

func TestRunMemoryCommandMigrateRequiresExistingStore(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	missingStore := filepath.Join(dir, "missing-memory.json")

	err := runMemoryCommand(nil, memoryCommandInput{
		StorePath: missingStore,
		Migrate:   true,
	})
	require.ErrorIs(t, err, os.ErrNotExist)

	_, statErr := os.Stat(missingStore)
	require.ErrorIs(t, statErr, os.ErrNotExist)
}

func TestMemoryCommandInputFromOptionsMapsPrivacyOptIns(t *testing.T) {
	t.Parallel()

	got := memoryCommandInputFromOptions(cliOptions{
		memorySearch:                  "callback retry",
		memoryMigrate:                 true,
		memoryIncludeSessionMessages:  true,
		memoryIncludeWorktreeMetadata: true,
	})

	assert.Equal(t, "callback retry", got.Search)
	assert.True(t, got.Migrate)
	assert.True(t, got.IncludeSessionMessages)
	assert.True(t, got.IncludeWorktreeMetadata)
}

func TestBuildMemoryStoreRequiresOptInForSessionMessagesAndWorktreeMetadata(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	store := session.NewStore(filepath.Join(dir, "sessions"))
	saved := session.New("gpt-test", []llm.Message{{
		Role:    llm.RoleUser,
		Content: "Please remember the callback retry transcript detail.",
	}})
	saved.ID = "session-memory-opt-in"
	saved.Title = "Metadata only"
	saved.WorktreePath = "/tmp/customer-alpha-worktree"
	saved.WorktreeBranch = "feature/customer-alpha"
	require.NoError(t, store.Save(saved))

	defaultMemory, err := buildMemoryStore(store, memoryCommandInput{})
	require.NoError(t, err)

	messageResults, err := defaultMemory.Search("callback retry transcript", 1)
	require.NoError(t, err)
	assert.Empty(t, messageResults)

	worktreeResults, err := defaultMemory.Search("customer alpha worktree", 1)
	require.NoError(t, err)
	assert.Empty(t, worktreeResults)

	optInMemory, err := buildMemoryStore(store, memoryCommandInput{
		IncludeSessionMessages:  true,
		IncludeWorktreeMetadata: true,
	})
	require.NoError(t, err)

	messageResults, err = optInMemory.Search("callback retry transcript", 1)
	require.NoError(t, err)

	if assert.Len(t, messageResults, 1) {
		assert.Equal(t, "session/session-memory-opt-in/message/0", messageResults[0].Document.ID)
	}

	worktreeResults, err = optInMemory.Search("customer alpha worktree", 1)
	require.NoError(t, err)

	if assert.Len(t, worktreeResults, 1) {
		assert.Equal(t, "session/session-memory-opt-in/metadata", worktreeResults[0].Document.ID)
	}
}

func TestRunMemoryCommandPersistsDefaultSessionIndexIntoEmptyStore(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	sessionStore := session.NewStore(filepath.Join(dir, "sessions"))
	saved := session.New("gpt-test", nil)
	saved.ID = "persistent-session-memory"
	saved.Title = "Persistent searchable session title"
	require.NoError(t, sessionStore.Save(saved))

	storePath := filepath.Join(dir, "memory.json")
	err := runMemoryCommand(sessionStore, memoryCommandInput{
		StorePath: storePath,
		Search:    "persistent searchable title",
	})
	require.NoError(t, err)

	loaded, err := memory.Load(storePath)
	require.NoError(t, err)

	results, err := loaded.Search("persistent searchable title", 1)
	require.NoError(t, err)

	if assert.Len(t, results, 1) {
		assert.Equal(t, "session/persistent-session-memory/metadata", results[0].Document.ID)
	}
}

func TestRunMemoryCommandRedactsSessionSecretsBeforePersistence(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	sessionStore := session.NewStore(filepath.Join(dir, "sessions"))

	saved := session.New("tenant/embed?api_key=model123/v1", []llm.Message{{
		Role:    llm.RoleUser,
		Content: "transcript-only api_key=message123",
	}})
	saved.ID = "cli-secret-session"
	saved.Title = "CLI secret persistence"
	saved.DefaultAgent = "reviewer/access_token=agent123/team"
	saved.Artifacts = []session.Artifact{{
		Path:        "docs/secret.md?access_token=artifact123",
		Kind:        "note",
		Summary:     "artifact summary password=summary123",
		SourceAgent: "researcher/access_token=source123/team",
	}}
	require.NoError(t, sessionStore.Save(saved))

	storePath := filepath.Join(dir, "memory.json")
	err := runMemoryCommand(sessionStore, memoryCommandInput{
		StorePath: storePath,
		Search:    "cli secret persistence",
	})
	require.NoError(t, err)

	persisted, err := os.ReadFile(storePath)
	require.NoError(t, err)

	persistedText := string(persisted)
	assert.Contains(t, persistedText, "[REDACTED]")

	for _, raw := range []string{"model123", "agent123", "artifact123", "summary123", "source123", "message123"} {
		assert.NotContains(t, persistedText, raw)
	}

	assert.NotContains(t, persistedText, "transcript-only")

	loaded, err := memory.Load(storePath)
	require.NoError(t, err)

	for _, doc := range loaded.Documents {
		assert.NotEqual(t, "message", doc.Metadata["kind"])
	}
}

func TestRunMemoryCommandSearchesWithoutSessionStore(t *testing.T) {
	t.Parallel()

	err := runMemoryCommand(nil, memoryCommandInput{Search: "anything"})
	require.NoError(t, err)
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

func TestRunAgentMemoryCommandIndexesWithTTL(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()

	note := filepath.Join(dir, "ttl-note.txt")
	require.NoError(t, os.WriteFile(note, []byte("short lived callback memory"), 0o600))

	storePath := filepath.Join(dir, "agent-memory.json")
	start := time.Now().UTC()

	err := runAgentMemoryCommand(dir, "reviewer", agentMemoryCommandInput{
		StorePath:  storePath,
		IndexFiles: []string{note},
		TTLSeconds: 60,
	})
	require.NoError(t, err)

	loaded, err := agentmemory.Load(storePath)
	require.NoError(t, err)

	docs := loaded.Documents("reviewer")
	require.Len(t, docs, 1)
	require.NotNil(t, docs[0].ExpiresAt)
	assert.True(t, docs[0].ExpiresAt.After(start))

	assert.Equal(t, 1, loaded.Compact(docs[0].ExpiresAt.Add(time.Nanosecond)))
	require.NoError(t, loaded.Save(storePath))

	persisted, err := os.ReadFile(storePath)
	require.NoError(t, err)
	assert.NotContains(t, string(persisted), "short lived callback memory")
}

func TestRunAgentMemoryCommandDeletesAndPersists(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	storePath := filepath.Join(dir, "agent-memory.json")

	store, err := agentmemory.NewStore(0)
	require.NoError(t, err)
	require.NoError(t, store.AddText("reviewer", "delete-me", "obsolete callback memory api_key=api-key-test-value"))
	require.NoError(t, store.AddText("reviewer", "keep-me", "kept callback retry memory"))
	require.NoError(t, store.Save(storePath))

	err = runAgentMemoryCommand(dir, "reviewer", agentMemoryCommandInput{
		StorePath: storePath,
		DeleteID:  "delete-me",
	})
	require.NoError(t, err)

	loaded, err := agentmemory.Load(storePath)
	require.NoError(t, err)

	docs := loaded.Documents("reviewer")
	require.Len(t, docs, 1)
	assert.Equal(t, "keep-me", docs[0].ID)

	results, err := loaded.Search("reviewer", "obsolete", 10)
	require.NoError(t, err)
	assert.Empty(t, results)

	persisted, err := os.ReadFile(storePath)
	require.NoError(t, err)
	assert.NotContains(t, string(persisted), "delete-me")
	assert.NotContains(t, string(persisted), "obsolete callback")
	assert.NotContains(t, string(persisted), "api-key-test-value")
	assert.Contains(t, string(persisted), "keep-me")
}

func TestAgentMemoryLifecycleMessagesRedactSensitiveIdentifiers(t *testing.T) {
	t.Parallel()

	rawAgent := "reviewer/access_token=agent123/team"
	rawID := "notes/access_token=doc123/memory.md"
	rawStorePath := filepath.Join(t.TempDir(), "tenant", "access_token=store123", "agent-memory.json")

	store, err := agentmemory.NewStore(16)
	require.NoError(t, err)
	require.NoError(t, store.AddText(rawAgent, rawID, "sensitive agent lifecycle message memory"))

	changed, messages, err := mutateAgentMemoryStore(store, rawAgent, rawStorePath, agentMemoryCommandInput{
		DeleteID: rawID,
		Compact:  true,
		Migrate:  true,
	})
	require.NoError(t, err)
	assert.True(t, changed)

	got := strings.Join(messages, "\n")
	assert.NotContains(t, got, "agent123")
	assert.NotContains(t, got, "doc123")
	assert.NotContains(t, got, "store123")
	assert.Contains(t, got, "[REDACTED]")
}

func TestRunAgentMemoryCommandCompactsExpiredPersistedDocuments(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	storePath := filepath.Join(dir, "agent-memory.json")

	store, err := agentmemory.NewStore(0)
	require.NoError(t, err)
	require.NoError(t, store.AddTextWithOptions("reviewer", "expired-me", "expired callback memory", agentmemory.WithExpiresAt(time.Now().Add(-time.Hour))))
	require.NoError(t, store.AddTextWithOptions("reviewer", "fresh-me", "fresh callback memory", agentmemory.WithExpiresAt(time.Now().Add(time.Hour))))

	data, err := json.MarshalIndent(store, "", "  ")
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(storePath, append(data, '\n'), 0o600))

	err = runAgentMemoryCommand(dir, "reviewer", agentMemoryCommandInput{
		StorePath: storePath,
		Compact:   true,
	})
	require.NoError(t, err)

	loaded, err := agentmemory.Load(storePath)
	require.NoError(t, err)

	docs := loaded.Documents("reviewer")
	require.Len(t, docs, 1)
	assert.Equal(t, "fresh-me", docs[0].ID)

	persisted, err := os.ReadFile(storePath)
	require.NoError(t, err)
	assert.NotContains(t, string(persisted), "expired-me")
	assert.NotContains(t, string(persisted), "expired callback")
	assert.Contains(t, string(persisted), "fresh-me")
}

func TestRunAgentMemoryCommandMigratesStalePersistedStore(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	storePath := filepath.Join(dir, "agent-memory.json")

	store, err := agentmemory.NewStore(16)
	require.NoError(t, err)
	require.NoError(t, store.AddText("reviewer", "stale-me", "OAuth token refresh retries"))
	store.Agents["reviewer"][0].Vectorizer.Model = "stale-hash-model"

	data, err := json.MarshalIndent(store, "", "  ")
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(storePath, append(data, '\n'), 0o600))

	err = runAgentMemoryCommand(dir, "", agentMemoryCommandInput{
		StorePath: storePath,
		Migrate:   true,
	})
	require.NoError(t, err)

	loaded, err := agentmemory.Load(storePath)
	require.NoError(t, err)

	docs := loaded.Documents("reviewer")
	require.Len(t, docs, 1)
	assert.NotEqual(t, "stale-hash-model", docs[0].Vectorizer.Model)
	assert.True(t, docs[0].Vectorizer.CompatibleWith(loaded.Vectorizer))

	results, err := loaded.Search("reviewer", "token refresh", 1)
	require.NoError(t, err)
	require.Len(t, results, 1)
	assert.Equal(t, "stale-me", results[0].Document.ID)
}

func TestRunAgentMemoryCommandMigrateRequiresExistingStore(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	missingStore := filepath.Join(dir, "missing-agent-memory.json")

	err := runAgentMemoryCommand(dir, "", agentMemoryCommandInput{
		StorePath: missingStore,
		Migrate:   true,
	})
	require.ErrorIs(t, err, os.ErrNotExist)

	_, statErr := os.Stat(missingStore)
	require.ErrorIs(t, statErr, os.ErrNotExist)
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

	source := retrieval.Source{Type: retrieval.SourceSession, Name: "session-1", URI: "/tmp/session-1.json"}
	documentID := "session/session-1/message/0"

	got := formatRetrievalResult(retrieval.Result{
		Source:     source,
		DocumentID: documentID,
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
		"stable_id=" + retrieval.StableDocumentID(source, documentID),
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

func TestFormatRetrievalResultDoesNotMutateMetadata(t *testing.T) {
	t.Parallel()

	metadata := map[string]string{"kind": "note"}
	got := formatRetrievalResult(retrieval.Result{
		Source:     retrieval.Source{Type: retrieval.SourceMemory, Name: "notes"},
		DocumentID: "doc-1",
		Score:      0.5,
		Scorer:     retrieval.Scorer{Name: "lexical-token-overlap"},
		Metadata:   metadata,
		Safety:     retrieval.Safety{InjectAllowed: true},
	}, false)

	assert.Contains(t, got, "stable_id="+retrieval.StableDocumentID(retrieval.Source{Type: retrieval.SourceMemory, Name: "notes"}, "doc-1"))
	assert.Equal(t, map[string]string{"kind": "note"}, metadata)
	assert.NotContains(t, metadata, retrieval.MetadataStableID)
}
