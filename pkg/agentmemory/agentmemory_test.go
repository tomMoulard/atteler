package agentmemory

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

	"github.com/tommoulard/atteler/pkg/privacy"
	"github.com/tommoulard/atteler/pkg/retrieval"
	"github.com/tommoulard/atteler/pkg/vector"
)

const (
	staleHashModel     = "stale-hash-model"
	sensitiveAgentName = "reviewer/access_token=agent123/team"
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
	assert.Equal(t, "direct", docs[0].Provenance["source_type"])
	assert.Equal(t, privacy.RedactionPolicyVersion, docs[0].Provenance["privacy_policy"])

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
	require.NoError(t, os.WriteFile(path, []byte("lexical memory from file"), 0o600))

	store, err := NewStore(16)
	require.NoError(t, err)
	require.NoError(t, store.AddFile("agent", path))

	docs := store.Documents("agent")
	require.Len(t, docs, 1)
	assert.Equal(t, filepath.Clean(path), docs[0].ID)
	assert.Equal(t, filepath.Clean(path), docs[0].Path)
	assert.Equal(t, "lexical memory from file", docs[0].Text)
	assert.Equal(t, "file", docs[0].Provenance["source_type"])
	assert.Equal(t, filepath.Clean(path), docs[0].Provenance["path"])

	badPath := filepath.Join(dir, "bad.bin")
	require.NoError(t, os.WriteFile(badPath, []byte{0xff, 0xfe}, 0o600))
	err = store.AddFile("agent", badPath)
	require.ErrorIs(t, err, ErrInvalidUTF8)
}

func TestStore_AddFileWithOptionsExpiresAndRecordsProvenance(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	path := filepath.Join(dir, "memory.txt")
	require.NoError(t, os.WriteFile(path, []byte("short lived file memory"), 0o600))

	expiresAt := time.Now().UTC().Add(time.Hour)
	store, err := NewStore(16)
	require.NoError(t, err)
	require.NoError(t, store.AddFileWithOptions("agent", path,
		WithExpiresAt(expiresAt),
		WithProvenance(map[string]string{"source": "file", "api_key": "abc123"}),
	))

	docs := store.Documents("agent")
	require.Len(t, docs, 1)
	require.NotNil(t, docs[0].ExpiresAt)
	assert.Equal(t, expiresAt, *docs[0].ExpiresAt)
	assert.Equal(t, "file", docs[0].Provenance["source"])
	assert.Equal(t, "[REDACTED]", docs[0].Provenance["api_key"])

	assert.Equal(t, 1, store.Compact(expiresAt.Add(time.Nanosecond)))
	assert.Empty(t, store.Documents("agent"))
}

func TestStore_SaveLoadJSONRoundTrip(t *testing.T) {
	t.Parallel()

	store, err := NewStore(24)
	require.NoError(t, err)
	require.NoError(t, store.Add("alice", Document{
		ID:       "design",
		Text:     "Keep the lexical memory package simple and persistent.",
		Metadata: map[string]string{"kind": "note"},
	}))
	require.NoError(t, store.AddText("bob", "unrelated", "Bob owns a separate memory namespace."))

	path := filepath.Join(t.TempDir(), "agent-memory.json")
	require.NoError(t, store.Save(path))

	loaded, err := Load(path)
	require.NoError(t, err)
	assert.Equal(t, StoreSchemaVersion, loaded.SchemaVersion)
	assert.Equal(t, store.Dimensions, loaded.Dimensions)
	assert.True(t, loaded.Vectorizer.CompatibleWith(vector.TextVectorizerSpec(loaded.Dimensions)))
	assert.False(t, loaded.CreatedAt.IsZero())
	assert.False(t, loaded.UpdatedAt.IsZero())

	results, err := loaded.Search("alice", "simple persistent", 1)
	require.NoError(t, err)
	require.Len(t, results, 1)
	assert.Equal(t, "design", results[0].Document.ID)
	assert.Equal(t, map[string]string{"kind": "note"}, results[0].Document.Metadata)
	assert.NotEmpty(t, results[0].Document.Vector)
	assert.NotEmpty(t, results[0].Document.SourceHash)
	assert.False(t, results[0].Document.CreatedAt.IsZero())
	assert.False(t, results[0].Document.UpdatedAt.IsZero())
	assert.True(t, results[0].Document.Vectorizer.CompatibleWith(loaded.Vectorizer))

	bobResults, err := loaded.Search("bob", "separate namespace", 1)
	require.NoError(t, err)
	require.Len(t, bobResults, 1)
	assert.Equal(t, "unrelated", bobResults[0].Document.ID)
}

func TestStore_SaveTightensExistingFilePermissions(t *testing.T) {
	t.Parallel()

	store, err := NewStore(16)
	require.NoError(t, err)
	require.NoError(t, store.AddText("agent", "note", "private agent memory"))

	path := filepath.Join(t.TempDir(), "agent-memory.json")
	//nolint:gosec // Intentionally start with loose permissions to prove Save tightens persisted agent memory stores.
	require.NoError(t, os.WriteFile(path, []byte("{}"), 0o644))
	require.NoError(t, store.Save(path))

	info, err := os.Stat(path)
	require.NoError(t, err)
	assert.Equal(t, os.FileMode(0o600), info.Mode().Perm())
}

func TestStore_RedactsSensitiveTextAndMetadataBeforePersistence(t *testing.T) {
	t.Parallel()

	store, err := NewStore(16)
	require.NoError(t, err)

	require.NoError(t, store.Add("agent", Document{
		ID:       "secret",
		Path:     "docs/secret.md?access_token=artifact123",
		Text:     "deploy password=hunter2 with api_key=abc123",
		Metadata: map[string]string{"auth_token": "abc123", "kind": "note"},
	}))

	docs := store.Documents("agent")
	require.Len(t, docs, 1)
	assert.NotContains(t, docs[0].Text, "hunter2")
	assert.NotContains(t, docs[0].Text, "abc123")
	assert.NotContains(t, docs[0].Path, "artifact123")
	assert.Equal(t, "[REDACTED]", docs[0].Metadata["auth_token"])
	assert.NotEmpty(t, docs[0].SourceHash)
	assert.True(t, docs[0].Vectorizer.CompatibleWith(store.Vectorizer))
}

func TestStore_RedactsSensitiveProvenanceBeforePersistence(t *testing.T) {
	t.Parallel()

	store, err := NewStore(16)
	require.NoError(t, err)

	require.NoError(t, store.AddTextWithOptions("agent", "note", "safe memory",
		WithProvenance(map[string]string{
			"access_token": "abc123",
			"source":       "captured password=hunter2 from notes",
		}),
	))

	docs := store.Documents("agent")
	require.Len(t, docs, 1)
	assert.Equal(t, "[REDACTED]", docs[0].Provenance["access_token"])
	assert.NotContains(t, docs[0].Provenance["source"], "hunter2")
}

func TestStore_RedactsSensitiveIDBeforePersistenceAndDelete(t *testing.T) {
	t.Parallel()

	store, err := NewStore(16)
	require.NoError(t, err)

	rawID := "docs/secret.md?access_token=artifact123"
	require.NoError(t, store.AddText("agent", rawID, "safe memory"))

	docs := store.Documents("agent")
	require.Len(t, docs, 1)
	assert.NotContains(t, docs[0].ID, "artifact123")
	assert.True(t, store.Delete("agent", rawID))
	assert.Empty(t, store.Documents("agent"))
}

func TestStore_RedactsSensitiveAgentNameBeforePersistence(t *testing.T) {
	t.Parallel()

	store, err := NewStore(16)
	require.NoError(t, err)

	rawAgent := sensitiveAgentName
	require.NoError(t, store.AddText(rawAgent, "note", "safe agent memory"))

	var persistedAgent string
	for agent := range store.Agents {
		persistedAgent = agent
	}

	assert.NotContains(t, persistedAgent, "agent123")
	assert.Contains(t, persistedAgent, "[REDACTED]")
	assert.Contains(t, persistedAgent, "/team")
	require.Len(t, store.Documents(rawAgent), 1)

	results, err := store.Search(rawAgent, "safe", 1)
	require.NoError(t, err)
	require.Len(t, results, 1)

	assert.True(t, store.Delete(rawAgent, "note"))
	assert.Empty(t, store.Documents(rawAgent))
}

func TestStore_RedactsSensitiveIDWithoutCollapsingPathSuffix(t *testing.T) {
	t.Parallel()

	store, err := NewStore(16)
	require.NoError(t, err)

	firstID := "tenant/access_token=artifact123/first"
	secondID := "tenant/access_token=artifact123/second"

	require.NoError(t, store.AddText("agent", firstID, "first safe memory"))
	require.NoError(t, store.AddText("agent", secondID, "second safe memory"))

	docs := store.Documents("agent")
	require.Len(t, docs, 2)
	assert.NotContains(t, docs[0].ID, "artifact123")
	assert.NotContains(t, docs[1].ID, "artifact123")
	assert.Regexp(t, `/first$`, docs[0].ID)
	assert.Regexp(t, `/second$`, docs[1].ID)

	assert.True(t, store.Delete("agent", firstID))

	docs = store.Documents("agent")
	require.Len(t, docs, 1)
	assert.Regexp(t, `/second$`, docs[0].ID)
}

func TestStore_DeleteAndTTLRemovePersistedContent(t *testing.T) {
	t.Parallel()

	now := time.Now().UTC()
	expired := now.Add(-time.Second)
	store, err := NewStore(16)
	require.NoError(t, err)
	require.NoError(t, store.AddTextWithOptions("agent", "expired", "temporary callback memory", WithExpiresAt(expired)))
	require.NoError(t, store.AddText("agent", "keep", "durable callback memory"))

	results, err := store.Search("agent", "temporary", 1)
	require.NoError(t, err)
	assert.Empty(t, results)

	assert.Equal(t, 1, store.Compact(now))
	backing := store.Agents["agent"][:cap(store.Agents["agent"])]
	assertAgentBackingOmitsText(t, backing, "temporary callback memory")
	assert.False(t, store.Delete("agent", "expired"))
	assert.True(t, store.Delete("agent", "keep"))
	assertAgentBackingOmitsText(t, backing, "durable callback memory")

	path := filepath.Join(t.TempDir(), "agent-memory.json")
	require.NoError(t, store.Save(path))
	data, err := os.ReadFile(path)
	require.NoError(t, err)
	assert.NotContains(t, string(data), "temporary callback memory")
	assert.NotContains(t, string(data), "durable callback memory")
}

func TestStore_DeleteRemovesOneAgentMemoryAndPreservesSiblings(t *testing.T) {
	t.Parallel()

	store, err := NewStore(16)
	require.NoError(t, err)
	require.NoError(t, store.AddText("agent", "delete-me", "obsolete agent memory"))
	require.NoError(t, store.AddText("agent", "keep-me", "durable agent memory"))
	require.NoError(t, store.AddText("other", "delete-me", "other agent memory"))

	assert.True(t, store.Delete("agent", "delete-me"))

	docs := store.Documents("agent")
	require.Len(t, docs, 1)
	assert.Equal(t, "keep-me", docs[0].ID)

	otherDocs := store.Documents("other")
	require.Len(t, otherDocs, 1)
	assert.Equal(t, "delete-me", otherDocs[0].ID)

	results, err := store.Search("agent", "obsolete", 10)
	require.NoError(t, err)

	for _, result := range results {
		assert.NotEqual(t, "delete-me", result.Document.ID)
	}

	results, err = store.Search("agent", "durable", 10)
	require.NoError(t, err)
	require.Len(t, results, 1)
	assert.Equal(t, "keep-me", results[0].Document.ID)

	results, err = store.Search("other", "other", 10)
	require.NoError(t, err)
	require.Len(t, results, 1)
	assert.Equal(t, "delete-me", results[0].Document.ID)

	path := filepath.Join(t.TempDir(), "agent-memory.json")
	require.NoError(t, store.Save(path))

	data, err := os.ReadFile(path)
	require.NoError(t, err)
	assert.NotContains(t, string(data), "obsolete agent memory")
	assert.Contains(t, string(data), "durable agent memory")
	assert.Contains(t, string(data), "other agent memory")
}

func TestStore_MigrateCompactsExpiredDocuments(t *testing.T) {
	t.Parallel()

	expired := time.Now().UTC().Add(-time.Second)
	store, err := NewStore(16)
	require.NoError(t, err)
	require.NoError(t, store.AddTextWithOptions("agent", "expired", "expired callback memory", WithExpiresAt(expired)))
	require.NoError(t, store.AddText("agent", "active", "active callback memory"))

	store.SchemaVersion = 0
	require.NoError(t, store.Migrate())

	docs := store.Documents("agent")
	require.Len(t, docs, 1)
	assert.Equal(t, "active", docs[0].ID)
}

func TestStore_MigrateCompactsExpiredNegativeDimensionsBeforeValidation(t *testing.T) {
	t.Parallel()

	expired := time.Now().UTC().Add(-time.Second)
	store, err := NewStore(16)
	require.NoError(t, err)
	require.NoError(t, store.AddTextWithOptions("agent", "expired", "expired negative-dimension callback memory", WithExpiresAt(expired)))

	store.SchemaVersion = 0
	store.Dimensions = -1
	store.Vectorizer.Dimensions = -1
	store.Agents["agent"][0].Vectorizer = store.Vectorizer

	require.NoError(t, store.Migrate())
	assert.Empty(t, store.Documents("agent"))
	assert.Positive(t, store.Dimensions)
	assert.True(t, store.Vectorizer.CompatibleWith(vector.TextVectorizerSpec(store.Dimensions)))
}

func TestStore_DeleteRemovesAllDuplicateIDsBeforePersistence(t *testing.T) {
	t.Parallel()

	store, err := NewStore(16)
	require.NoError(t, err)
	require.NoError(t, store.AddText("agent", "same", "first duplicate agent memory"))
	require.NoError(t, store.AddText("agent", "other", "second duplicate agent memory"))
	store.Agents["agent"][1].ID = "same"

	assert.True(t, store.Delete("agent", "same"))
	assert.Empty(t, store.Documents("agent"))

	path := filepath.Join(t.TempDir(), "agent-memory.json")
	require.NoError(t, store.Save(path))

	data, err := os.ReadFile(path)
	require.NoError(t, err)
	assert.NotContains(t, string(data), "first duplicate agent memory")
	assert.NotContains(t, string(data), "second duplicate agent memory")
}

func TestStore_DeleteClearsBackingCapacity(t *testing.T) {
	t.Parallel()

	store, err := NewStore(16)
	require.NoError(t, err)

	backing := []Document{
		{ID: "delete-me", Text: "visible deleted agent memory"},
		{ID: "ghost", Text: "hidden stale agent memory"},
	}
	store.Agents["agent"] = backing[:1]
	fullBacking := store.Agents["agent"][:cap(store.Agents["agent"])]

	assert.True(t, store.Delete("agent", "delete-me"))
	assertAgentBackingOmitsText(t, fullBacking, "agent memory")
}

func TestStore_SearchRejectsDuplicateIDs(t *testing.T) {
	t.Parallel()

	store, err := NewStore(16)
	require.NoError(t, err)
	require.NoError(t, store.AddText("agent", "same", "first duplicate agent memory"))
	require.NoError(t, store.AddText("agent", "other", "second duplicate agent memory"))
	store.Agents["agent"][1].ID = "same"

	_, err = store.Search("agent", "duplicate", 10)
	require.ErrorIs(t, err, ErrDuplicateID)
}

func TestStore_SearchDoesNotReturnExpiredDuplicateContent(t *testing.T) {
	t.Parallel()

	const duplicateID = "expired-duplicate"

	expired := time.Now().UTC().Add(-time.Second)
	store, err := NewStore(16)
	require.NoError(t, err)
	require.NoError(t, store.AddTextWithOptions("agent", duplicateID, "expired duplicate agent memory", WithExpiresAt(expired)))
	require.NoError(t, store.AddText("agent", "active", "active duplicate agent memory"))
	store.Agents["agent"][1].ID = duplicateID

	results, err := store.Search("agent", "active duplicate", 1)
	require.NoError(t, err)
	require.Len(t, results, 1)
	assert.Equal(t, duplicateID, results[0].Document.ID)
	assert.Equal(t, "active duplicate agent memory", results[0].Document.Text)
	assert.NotContains(t, results[0].Document.Text, "expired")
}

func TestStore_SearchRejectsMissingProvenance(t *testing.T) {
	t.Parallel()

	store, err := NewStore(16)
	require.NoError(t, err)
	require.NoError(t, store.AddText("agent", "note", "safe searchable agent memory"))
	store.Agents["agent"][0].Provenance = nil

	_, err = store.Search("agent", "safe", 1)
	require.ErrorIs(t, err, ErrProvenanceMissing)
}

func TestStore_SaveRejectsMissingProvenance(t *testing.T) {
	t.Parallel()

	store, err := NewStore(16)
	require.NoError(t, err)
	require.NoError(t, store.AddText("agent", "note", "safe persistent agent memory"))
	store.Agents["agent"][0].Provenance = nil

	path := filepath.Join(t.TempDir(), "agent-memory.json")
	err = store.Save(path)
	require.ErrorIs(t, err, ErrProvenanceMissing)
}

func TestStore_SaveCompactsExpiredContent(t *testing.T) {
	t.Parallel()

	expired := time.Now().UTC().Add(-time.Second)
	store, err := NewStore(16)
	require.NoError(t, err)
	require.NoError(t, store.AddTextWithOptions("agent", "expired", "temporary save-only agent memory", WithExpiresAt(expired)))

	path := filepath.Join(t.TempDir(), "agent-memory.json")
	require.NoError(t, store.Save(path))

	data, err := os.ReadFile(path)
	require.NoError(t, err)
	assert.NotContains(t, string(data), "temporary save-only agent memory")

	loaded, err := Load(path)
	require.NoError(t, err)
	assert.Empty(t, loaded.Documents("agent"))
}

func TestStore_SaveCompactsExpiredStaleContentBeforeVectorizerValidation(t *testing.T) {
	t.Parallel()

	expired := time.Now().UTC().Add(-time.Second)
	store, err := NewStore(16)
	require.NoError(t, err)
	require.NoError(t, store.AddTextWithOptions("agent", "expired", "expired stale agent memory", WithExpiresAt(expired)))
	store.SchemaVersion = 0
	store.Vectorizer.Model = staleHashModel
	store.Agents["agent"][0].Vectorizer = store.Vectorizer

	path := filepath.Join(t.TempDir(), "agent-memory.json")
	require.NoError(t, store.Save(path))

	persisted, err := os.ReadFile(path)
	require.NoError(t, err)
	assert.NotContains(t, string(persisted), "expired stale agent memory")

	loaded, err := Load(path)
	require.NoError(t, err)
	assert.Empty(t, loaded.Documents("agent"))
	assert.True(t, loaded.Vectorizer.CompatibleWith(vector.TextVectorizerSpec(loaded.Dimensions)))
}

func TestStore_SaveCompactsExpiredContentBeforeDimensionValidation(t *testing.T) {
	t.Parallel()

	expired := time.Now().UTC().Add(-time.Second)
	store, err := NewStore(16)
	require.NoError(t, err)
	require.NoError(t, store.AddTextWithOptions("agent", "expired", "expired invalid-dimension agent memory", WithExpiresAt(expired)))
	store.Dimensions = -1
	store.Vectorizer.Dimensions = -1
	store.Agents["agent"][0].Vectorizer = store.Vectorizer

	path := filepath.Join(t.TempDir(), "agent-memory.json")
	require.NoError(t, store.Save(path))

	loaded, err := Load(path)
	require.NoError(t, err)
	assert.Empty(t, loaded.Documents("agent"))
	assert.True(t, loaded.Vectorizer.CompatibleWith(vector.TextVectorizerSpec(loaded.Dimensions)))
}

func TestLoadCompactsExpiredStaleContentBeforeValidation(t *testing.T) {
	t.Parallel()

	expired := time.Now().UTC().Add(-time.Second)
	store, err := NewStore(16)
	require.NoError(t, err)
	require.NoError(t, store.AddTextWithOptions("agent", "expired", "expired stale agent memory", WithExpiresAt(expired)))
	store.Vectorizer.Model = staleHashModel
	store.Agents["agent"][0].Vectorizer = store.Vectorizer
	store.Agents["agent"][0].Provenance = nil

	path := filepath.Join(t.TempDir(), "agent-memory.json")
	data, err := json.Marshal(store)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(path, data, 0o600))

	loaded, err := Load(path)
	require.NoError(t, err)
	assert.Empty(t, loaded.Documents("agent"))
	assert.True(t, loaded.Vectorizer.CompatibleWith(vector.TextVectorizerSpec(loaded.Dimensions)))
}

func TestLoadAndCompactReportsExpiredDocumentsBeforeValidation(t *testing.T) {
	t.Parallel()

	expired := time.Now().UTC().Add(-time.Second)
	store, err := NewStore(16)
	require.NoError(t, err)
	require.NoError(t, store.AddTextWithOptions("agent", "expired", "expired stale agent memory", WithExpiresAt(expired)))
	store.Vectorizer.Model = staleHashModel
	store.Agents["agent"][0].Vectorizer = store.Vectorizer
	store.Agents["agent"][0].Provenance = nil

	path := filepath.Join(t.TempDir(), "agent-memory.json")
	data, err := json.Marshal(store)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(path, data, 0o600))

	loaded, removed, err := LoadAndCompact(path, time.Now().UTC())
	require.NoError(t, err)
	assert.Equal(t, 1, removed)
	assert.Empty(t, loaded.Documents("agent"))
	assert.True(t, loaded.Vectorizer.CompatibleWith(vector.TextVectorizerSpec(loaded.Dimensions)))
}

func TestLoadCompactsExpiredLegacySchemaContentBeforeValidation(t *testing.T) {
	t.Parallel()

	expired := time.Now().UTC().Add(-time.Second)
	store, err := NewStore(16)
	require.NoError(t, err)
	require.NoError(t, store.AddTextWithOptions("agent", "expired", "expired legacy agent memory", WithExpiresAt(expired)))
	store.SchemaVersion = 0
	store.Vectorizer.Model = staleHashModel
	store.Agents["agent"][0].Vectorizer = store.Vectorizer
	store.Agents["agent"][0].Provenance = nil

	path := filepath.Join(t.TempDir(), "agent-memory.json")
	data, err := json.Marshal(store)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(path, data, 0o600))

	loaded, err := Load(path)
	require.NoError(t, err)
	assert.Equal(t, StoreSchemaVersion, loaded.SchemaVersion)
	assert.Empty(t, loaded.Documents("agent"))
	assert.True(t, loaded.Vectorizer.CompatibleWith(vector.TextVectorizerSpec(loaded.Dimensions)))
}

func TestLoadCompactsExpiredNegativeDimensionsBeforeValidation(t *testing.T) {
	t.Parallel()

	expired := time.Now().UTC().Add(-time.Second)
	store, err := NewStore(16)
	require.NoError(t, err)
	require.NoError(t, store.AddTextWithOptions("agent", "expired", "expired negative-dimension agent memory", WithExpiresAt(expired)))
	store.Dimensions = -1
	store.Vectorizer.Dimensions = -1
	store.Agents["agent"][0].Vectorizer = store.Vectorizer

	path := filepath.Join(t.TempDir(), "agent-memory.json")
	data, err := json.Marshal(store)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(path, data, 0o600))

	loaded, err := Load(path)
	require.NoError(t, err)
	assert.Empty(t, loaded.Documents("agent"))
	assert.True(t, loaded.Vectorizer.CompatibleWith(vector.TextVectorizerSpec(loaded.Dimensions)))
}

func TestLoadCompactsExpiredDuplicateBeforeValidation(t *testing.T) {
	t.Parallel()

	expired := time.Now().UTC().Add(-time.Second)
	store, err := NewStore(16)
	require.NoError(t, err)
	require.NoError(t, store.AddText("agent", "same", "active duplicate agent memory"))
	store.Agents["agent"] = append(store.Agents["agent"], Document{
		ID:        "same",
		Text:      "expired duplicate agent memory",
		ExpiresAt: &expired,
	})

	path := filepath.Join(t.TempDir(), "agent-memory.json")
	data, err := json.Marshal(store)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(path, data, 0o600))

	loaded, err := Load(path)
	require.NoError(t, err)

	docs := loaded.Documents("agent")
	require.Len(t, docs, 1)
	assert.Equal(t, "active duplicate agent memory", docs[0].Text)
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

	store, err := NewStore(8)
	require.NoError(t, err)
	require.NoError(t, store.AddText("agent", "dup", "alpha"))
	require.NoError(t, store.AddText("agent", "other", "beta"))
	store.Agents["agent"][1].ID = "dup"

	path := filepath.Join(t.TempDir(), "bad.json")
	data, err := json.Marshal(store)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(path, data, 0o600))

	_, err = Load(path)
	require.ErrorIs(t, err, ErrDuplicateID)
}

func TestLoadRefusesSourceHashMismatch(t *testing.T) {
	t.Parallel()

	store, err := NewStore(16)
	require.NoError(t, err)
	require.NoError(t, store.AddText("agent", "note", "original memory text"))
	store.Agents["agent"][0].Text = "tampered memory text"

	path := filepath.Join(t.TempDir(), "tampered.json")
	data, err := json.Marshal(store)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(path, data, 0o600))

	_, err = Load(path)
	require.ErrorIs(t, err, ErrSourceHashMismatch)
}

func TestLoadAndSearchRefuseMissingSourceHash(t *testing.T) {
	t.Parallel()

	store, err := NewStore(16)
	require.NoError(t, err)
	require.NoError(t, store.AddText("agent", "note", "durable memory text"))
	store.Agents["agent"][0].SourceHash = ""

	_, err = store.Search("agent", "durable", 1)
	require.ErrorIs(t, err, ErrSourceHashMismatch)

	path := filepath.Join(t.TempDir(), "missing-source-hash.json")
	data, err := json.Marshal(store)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(path, data, 0o600))

	_, err = Load(path)
	require.ErrorIs(t, err, ErrSourceHashMismatch)

	migrated, err := LoadWithOptions(path, LoadOptions{Migrate: true})
	require.NoError(t, err)

	docs := migrated.Documents("agent")
	require.Len(t, docs, 1)
	assert.NotEmpty(t, docs[0].SourceHash)
}

func TestLoadAndSearchRefuseUnvectorizableText(t *testing.T) {
	t.Parallel()

	store, err := NewStore(16)
	require.NoError(t, err)
	require.NoError(t, store.AddText("agent", "note", "durable memory text"))
	store.Agents["agent"][0].Text = "!!!"
	store.Agents["agent"][0].SourceHash = privacy.SourceHash("!!!")

	_, err = store.Search("agent", "durable", 1)
	require.ErrorIs(t, err, vector.ErrEmptyText)

	path := filepath.Join(t.TempDir(), "unvectorizable-text.json")
	data, err := json.Marshal(store)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(path, data, 0o600))

	_, err = Load(path)
	require.ErrorIs(t, err, vector.ErrEmptyText)

	_, err = LoadWithOptions(path, LoadOptions{Migrate: true})
	require.ErrorIs(t, err, vector.ErrEmptyText)
}

func TestLoadRefusesStaleVectorizerUnlessMigrated(t *testing.T) {
	t.Parallel()

	store, err := NewStore(16)
	require.NoError(t, err)
	require.NoError(t, store.AddText("agent", "note", "OAuth token refresh retries"))
	store.Agents["agent"][0].Vectorizer.Model = staleHashModel

	path := filepath.Join(t.TempDir(), "stale.json")
	data, err := json.Marshal(store)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(path, data, 0o600))

	_, err = Load(path)
	require.ErrorIs(t, err, vector.ErrVectorizerMismatch)

	migrated, err := LoadWithOptions(path, LoadOptions{Migrate: true})
	require.NoError(t, err)

	docs := migrated.Documents("agent")
	require.Len(t, docs, 1)
	assert.True(t, docs[0].Vectorizer.CompatibleWith(migrated.Vectorizer))
	assert.NotEqual(t, staleHashModel, docs[0].Vectorizer.Model)

	results, err := migrated.Search("agent", "token refresh", 1)
	require.NoError(t, err)
	require.Len(t, results, 1)
	assert.Equal(t, "note", results[0].Document.ID)
}

func TestLoadRejectsMissingProvenanceUnlessMigrated(t *testing.T) {
	t.Parallel()

	store, err := NewStore(16)
	require.NoError(t, err)
	require.NoError(t, store.AddText("agent", "note", "OAuth token refresh retries"))
	store.Agents["agent"][0].Provenance = nil

	path := filepath.Join(t.TempDir(), "missing-provenance.json")
	data, err := json.Marshal(store)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(path, data, 0o600))

	_, err = Load(path)
	require.ErrorIs(t, err, ErrProvenanceMissing)

	migrated, err := LoadWithOptions(path, LoadOptions{Migrate: true})
	require.NoError(t, err)

	docs := migrated.Documents("agent")
	require.Len(t, docs, 1)
	assert.Equal(t, "legacy", docs[0].Provenance["source_type"])
}

func TestLoadRejectsMissingPrivacyPolicyUnlessMigrated(t *testing.T) {
	t.Parallel()

	store, err := NewStore(16)
	require.NoError(t, err)
	require.NoError(t, store.AddText("agent", "note", "safe memory"))
	delete(store.Agents["agent"][0].Provenance, "privacy_policy")

	path := filepath.Join(t.TempDir(), "missing-privacy-policy.json")
	data, err := json.Marshal(store)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(path, data, 0o600))

	_, err = Load(path)
	require.ErrorIs(t, err, ErrPrivacyPolicy)

	migrated, err := LoadWithOptions(path, LoadOptions{Migrate: true})
	require.NoError(t, err)

	docs := migrated.Documents("agent")
	require.Len(t, docs, 1)
	assert.Equal(t, privacy.RedactionPolicyVersion, docs[0].Provenance["privacy_policy"])
}

func TestStore_MigratePreservesExistingProvenance(t *testing.T) {
	t.Parallel()

	store, err := NewStore(16)
	require.NoError(t, err)

	store.SchemaVersion = 0
	store.Agents["agent"] = []Document{{
		ID:   "file-doc",
		Text: "safe file-backed agent memory",
		Provenance: map[string]string{
			"source_type": "file",
			"path":        "docs/agent-memory-note.md",
			"api_key":     "abc123",
		},
	}}

	require.NoError(t, store.Migrate())

	docs := store.Documents("agent")
	require.Len(t, docs, 1)

	wantRedacted := privacy.RedactMetadata(map[string]string{"api_key": "abc123"})["api_key"]

	assert.Equal(t, "file", docs[0].Provenance["source_type"])
	assert.Equal(t, wantRedacted, docs[0].Provenance["api_key"])
	assert.True(t, docs[0].Vectorizer.CompatibleWith(store.Vectorizer))
	assert.NotEmpty(t, docs[0].SourceHash)
}

func TestLoadAndSaveRefuseMissingVectorizerMetadataUnlessMigrated(t *testing.T) {
	t.Parallel()

	store, err := NewStore(16)
	require.NoError(t, err)
	require.NoError(t, store.AddText("agent", "note", "OAuth token refresh retries"))

	store.Vectorizer = vector.VectorizerSpec{}

	path := filepath.Join(t.TempDir(), "missing-vectorizer.json")
	err = store.Save(path)
	require.ErrorIs(t, err, vector.ErrVectorizerMismatch)

	_, err = store.Search("agent", "token refresh", 1)
	require.ErrorIs(t, err, vector.ErrVectorizerMismatch)

	store.Agents["agent"][0].Vectorizer = vector.VectorizerSpec{}

	data, err := json.Marshal(store)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(path, data, 0o600))

	_, err = Load(path)
	require.ErrorIs(t, err, vector.ErrVectorizerMismatch)

	migrated, err := LoadWithOptions(path, LoadOptions{Migrate: true})
	require.NoError(t, err)
	assert.True(t, migrated.Vectorizer.CompatibleWith(vector.TextVectorizerSpec(migrated.Dimensions)))

	results, err := migrated.Search("agent", "token refresh", 1)
	require.NoError(t, err)
	require.Len(t, results, 1)
	assert.Equal(t, "note", results[0].Document.ID)
}

func TestLoadAndSaveRefuseMissingVectorizerDimensionsUnlessMigrated(t *testing.T) {
	t.Parallel()

	store, err := NewStore(16)
	require.NoError(t, err)
	require.NoError(t, store.AddText("agent", "note", "OAuth token refresh retries"))

	store.Vectorizer.Dimensions = 0
	store.Agents["agent"][0].Vectorizer.Dimensions = 0

	path := filepath.Join(t.TempDir(), "missing-vectorizer-dimensions.json")
	err = store.Save(path)
	require.ErrorIs(t, err, vector.ErrDimensionMismatch)

	data, err := json.Marshal(store)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(path, data, 0o600))

	_, err = Load(path)
	require.ErrorIs(t, err, vector.ErrDimensionMismatch)

	migrated, err := LoadWithOptions(path, LoadOptions{Migrate: true})
	require.NoError(t, err)
	assert.Equal(t, migrated.Dimensions, migrated.Vectorizer.Dimensions)

	docs := migrated.Documents("agent")
	require.Len(t, docs, 1)
	assert.Equal(t, migrated.Dimensions, docs[0].Vectorizer.Dimensions)

	results, err := migrated.Search("agent", "token refresh", 1)
	require.NoError(t, err)
	require.Len(t, results, 1)
	assert.Equal(t, "note", results[0].Document.ID)
}

func TestLoadRefusesLegacySchemaUnlessMigrated(t *testing.T) {
	t.Parallel()

	store, err := NewStore(16)
	require.NoError(t, err)
	require.NoError(t, store.AddText("agent", "note", "OAuth token refresh retries"))
	store.SchemaVersion = 0

	path := filepath.Join(t.TempDir(), "legacy-schema.json")
	data, err := json.Marshal(store)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(path, data, 0o600))

	_, err = Load(path)
	require.ErrorIs(t, err, ErrIncompatibleSchema)

	migrated, err := LoadWithOptions(path, LoadOptions{Migrate: true})
	require.NoError(t, err)
	assert.Equal(t, StoreSchemaVersion, migrated.SchemaVersion)
	assert.True(t, migrated.Vectorizer.CompatibleWith(vector.TextVectorizerSpec(migrated.Dimensions)))

	results, err := migrated.Search("agent", "token refresh", 1)
	require.NoError(t, err)
	require.Len(t, results, 1)
	assert.Equal(t, "note", results[0].Document.ID)
}

func TestStore_MigrateRejectsMalformedSchemaVersion(t *testing.T) {
	t.Parallel()

	store, err := NewStore(16)
	require.NoError(t, err)
	require.NoError(t, store.AddText("agent", "note", "OAuth token refresh retries"))
	store.SchemaVersion = -1

	require.ErrorIs(t, store.Migrate(), ErrIncompatibleSchema)
}

func TestStore_OperationsRejectLegacySchemaWithDocumentsUnlessMigrated(t *testing.T) {
	t.Parallel()

	store, err := NewStore(16)
	require.NoError(t, err)
	require.NoError(t, store.AddText("agent", "note", "OAuth token refresh retries"))
	store.SchemaVersion = 0

	_, err = store.Search("agent", "token refresh", 1)
	require.ErrorIs(t, err, ErrIncompatibleSchema)

	require.ErrorIs(t, store.AddText("agent", "next", "new callback memory"), ErrIncompatibleSchema)
	require.ErrorIs(t, store.Save(filepath.Join(t.TempDir(), "legacy.json")), ErrIncompatibleSchema)

	require.NoError(t, store.Migrate())

	results, err := store.Search("agent", "token refresh", 1)
	require.NoError(t, err)
	require.Len(t, results, 1)
	assert.Equal(t, "note", results[0].Document.ID)
}

func TestLoadRefusesStaleStoreVectorizerNormalizationUnlessMigrated(t *testing.T) {
	t.Parallel()

	store, err := NewStore(16)
	require.NoError(t, err)
	require.NoError(t, store.AddText("agent", "note", "OAuth token refresh retries"))

	store.Vectorizer.Normalization = "legacy-token-normalization"
	store.Agents["agent"][0].Vectorizer = store.Vectorizer

	path := filepath.Join(t.TempDir(), "stale-normalization.json")
	data, err := json.Marshal(store)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(path, data, 0o600))

	_, err = Load(path)
	require.ErrorIs(t, err, vector.ErrVectorizerMismatch)

	migrated, err := LoadWithOptions(path, LoadOptions{Migrate: true})
	require.NoError(t, err)

	assert.True(t, migrated.Vectorizer.CompatibleWith(vector.TextVectorizerSpec(migrated.Dimensions)))
	docs := migrated.Documents("agent")
	require.Len(t, docs, 1)
	assert.True(t, docs[0].Vectorizer.CompatibleWith(migrated.Vectorizer))
}

func TestLoadRefusesDimensionMismatchUnlessMigrated(t *testing.T) {
	t.Parallel()

	store, err := NewStore(8)
	require.NoError(t, err)
	require.NoError(t, store.AddText("agent", "note", "OAuth token refresh retries"))

	store.Dimensions = 16
	store.Vectorizer = vector.TextVectorizerSpec(16)
	store.Agents["agent"][0].Vectorizer = store.Vectorizer

	path := filepath.Join(t.TempDir(), "stale-dimensions.json")
	data, err := json.Marshal(store)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(path, data, 0o600))

	_, err = Load(path)
	require.ErrorIs(t, err, vector.ErrDimensionMismatch)

	migrated, err := LoadWithOptions(path, LoadOptions{Migrate: true})
	require.NoError(t, err)
	assert.Equal(t, 16, migrated.Dimensions)

	docs := migrated.Documents("agent")
	require.Len(t, docs, 1)
	assert.Len(t, docs[0].Vector, 16)
	assert.True(t, docs[0].Vectorizer.CompatibleWith(migrated.Vectorizer))
}

func TestStore_MigrateTextDimensionsReembedsDocuments(t *testing.T) {
	t.Parallel()

	store, err := NewStore(8)
	require.NoError(t, err)
	require.NoError(t, store.AddText("agent", "note", "OAuth token refresh retries"))
	require.Len(t, store.Documents("agent")[0].Vector, 8)

	require.NoError(t, store.MigrateTextDimensions(32))

	docs := store.Documents("agent")
	require.Len(t, docs, 1)
	assert.Len(t, docs[0].Vector, 32)
	assert.True(t, docs[0].Vectorizer.CompatibleWith(vector.TextVectorizerSpec(32)))

	results, err := store.Search("agent", "token refresh", 1)
	require.NoError(t, err)
	require.Len(t, results, 1)
	assert.Equal(t, "note", results[0].Document.ID)
}

func TestStore_ReembedAllCompactsExpiredInvalidStateBeforeValidation(t *testing.T) {
	t.Parallel()

	expired := time.Now().UTC().Add(-time.Second)
	store, err := NewStore(16)
	require.NoError(t, err)
	require.NoError(t, store.AddTextWithOptions("agent", "expired", "expired invalid state memory", WithExpiresAt(expired)))

	store.SchemaVersion = 0
	store.Dimensions = -1
	store.Vectorizer = vector.TextVectorizerSpec(-1)
	store.Vectorizer.Model = "stale-hash-model"
	store.Agents["agent"][0].Vectorizer = store.Vectorizer

	require.NoError(t, store.ReembedAll())
	assert.Empty(t, store.Documents("agent"))
	assert.Equal(t, StoreSchemaVersion, store.SchemaVersion)
	assert.Positive(t, store.Dimensions)
	assert.True(t, store.Vectorizer.CompatibleWith(vector.TextVectorizerSpec(store.Dimensions)))
}

func TestStore_ReembedAllRedactsSensitiveAgentNames(t *testing.T) {
	t.Parallel()

	rawAgent := sensitiveAgentName
	store, err := NewStore(16)
	require.NoError(t, err)
	require.NoError(t, store.AddText("agent", "note", "safe reembed memory"))
	store.Agents[rawAgent] = store.Agents["agent"]
	delete(store.Agents, "agent")

	require.NoError(t, store.ReembedAll())

	docs := store.Documents(rawAgent)
	require.Len(t, docs, 1)
	assert.Equal(t, "note", docs[0].ID)

	persisted, err := json.Marshal(store)
	require.NoError(t, err)
	assert.NotContains(t, string(persisted), "agent123")
	assert.Contains(t, string(persisted), "[REDACTED]")
}

func TestStore_SearchRefusesTamperedTextUntilReembedded(t *testing.T) {
	t.Parallel()

	store, err := NewStore(16)
	require.NoError(t, err)
	require.NoError(t, store.AddText("agent", "note", "OAuth token refresh retries"))

	store.Agents["agent"][0].Text = "different topic"

	_, err = store.Search("agent", "different", 1)
	require.ErrorIs(t, err, ErrSourceHashMismatch)

	require.NoError(t, store.Reembed("agent", "note"))
	results, err := store.Search("agent", "different", 1)
	require.NoError(t, err)
	require.Len(t, results, 1)
	assert.Equal(t, "note", results[0].Document.ID)
}

func TestStore_SaveRefusesStaleVectorsUntilReembedded(t *testing.T) {
	t.Parallel()

	store, err := NewStore(16)
	require.NoError(t, err)
	require.NoError(t, store.AddText("agent", "note", "OAuth token refresh retries"))

	store.Agents["agent"][0].Vectorizer.Model = staleHashModel

	path := filepath.Join(t.TempDir(), "agent-memory.json")
	err = store.Save(path)
	require.ErrorIs(t, err, vector.ErrVectorizerMismatch)

	require.NoError(t, store.Reembed("agent", "note"))
	require.NoError(t, store.Save(path))

	loaded, err := Load(path)
	require.NoError(t, err)

	docs := loaded.Documents("agent")
	require.Len(t, docs, 1)
	assert.True(t, docs[0].Vectorizer.CompatibleWith(loaded.Vectorizer))
}

func TestStore_RefusesStaleVectorContentUntilReembedded(t *testing.T) {
	t.Parallel()

	store, err := NewStore(32)
	require.NoError(t, err)
	require.NoError(t, store.AddText("agent", "note", "OAuth token refresh retries"))

	vectorizer, err := vector.NewTextVectorizer(store.Dimensions)
	require.NoError(t, err)
	currentVector, err := vectorizer.Vectorize(store.Agents["agent"][0].Text)
	require.NoError(t, err)
	staleVector, err := vectorizer.Vectorize("documentation screenshots changelog")
	require.NoError(t, err)
	require.False(t, vectorsEqual(currentVector, staleVector))

	store.Agents["agent"][0].Vector = staleVector

	_, err = store.Search("agent", "token refresh", 1)
	require.ErrorIs(t, err, ErrVectorMismatch)

	path := filepath.Join(t.TempDir(), "agent-memory.json")
	data, err := json.Marshal(store)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(path, data, 0o600))

	_, err = Load(path)
	require.ErrorIs(t, err, ErrVectorMismatch)

	err = store.Save(path)
	require.ErrorIs(t, err, ErrVectorMismatch)

	require.NoError(t, store.Reembed("agent", "note"))
	require.NoError(t, store.Save(path))

	loaded, err := Load(path)
	require.NoError(t, err)
	results, err := loaded.Search("agent", "token refresh", 1)
	require.NoError(t, err)
	require.Len(t, results, 1)
	assert.Equal(t, "note", results[0].Document.ID)
}

func TestStore_SearchRefusesUnredactedTamperedMetadata(t *testing.T) {
	t.Parallel()

	store, err := NewStore(16)
	require.NoError(t, err)
	require.NoError(t, store.AddText("agent", "note", "safe memory"))

	store.Agents["agent"][0].Metadata = map[string]string{"api_key": "abc123"}

	_, err = store.Search("agent", "safe", 1)
	require.ErrorIs(t, err, ErrPrivacyPolicy)
}

func TestStore_RetrievalQualityRegression(t *testing.T) {
	t.Parallel()

	data, err := os.ReadFile(filepath.Join("testdata", "retrieval_quality.json"))
	require.NoError(t, err)

	var fixture struct {
		Documents []struct {
			ID   string `json:"id"`
			Text string `json:"text"`
		} `json:"documents"`
		Cases []struct {
			Query   string `json:"query"`
			WantTop string `json:"want_top"`
		} `json:"cases"`
	}
	require.NoError(t, json.Unmarshal(data, &fixture))

	store, err := NewStore(64)
	require.NoError(t, err)

	for _, doc := range fixture.Documents {
		require.NoError(t, store.AddText("agent", doc.ID, doc.Text))
	}

	for _, tc := range fixture.Cases {
		results, err := store.Search("agent", tc.Query, 1)
		require.NoError(t, err)

		if assert.Len(t, results, 1) {
			assert.Equal(t, tc.WantTop, results[0].Document.ID)
		}
	}
}

func TestStore_EmbeddingVectorizerPersistsAndSearchesWithContext(t *testing.T) {
	t.Parallel()

	vectorizer := &agentMemoryEmbeddingVectorizer{}
	store, err := NewStoreWithVectorizer(agentMemoryEmbeddingSpec(0), vectorizer)
	require.NoError(t, err)

	err = store.AddText("agent", "no-context", "semantic retrieval memory")
	require.ErrorIs(t, err, vector.ErrContextRequired)

	require.NoError(t, store.AddTextContext(context.TODO(), "agent", "rag", "semantic retrieval memory for local rag"))
	require.NoError(t, store.AddTextContext(context.TODO(), "agent", "shell", "shell command output capture"))

	assert.Equal(t, 3, store.Dimensions)
	assert.True(t, store.Vectorizer.CompatibleWith(agentMemoryEmbeddingSpec(3)))

	results, err := store.SearchContext(context.TODO(), "agent", "semantic retrieval", 1)
	require.NoError(t, err)
	require.Len(t, results, 1)
	assert.Equal(t, "rag", results[0].Document.ID)

	path := filepath.Join(t.TempDir(), "embedding-agent-memory.json")
	require.NoError(t, store.Save(path))

	loaded, err := Load(path)
	require.NoError(t, err)
	assert.True(t, loaded.Vectorizer.CompatibleWith(agentMemoryEmbeddingSpec(3)))

	_, err = loaded.SearchContext(context.TODO(), "agent", "semantic retrieval", 1)
	require.ErrorIs(t, err, vector.ErrVectorizerMismatch)

	require.NoError(t, loaded.SetVectorizer(agentMemoryEmbeddingSpec(0), &agentMemoryEmbeddingVectorizer{}))

	retrievalResults, err := loaded.SearchRetrieval(context.TODO(), "agent", retrieval.Query{Text: "semantic retrieval", Limit: 1})
	require.NoError(t, err)
	require.Len(t, retrievalResults, 1)
	assert.Equal(t, "rag", retrievalResults[0].DocumentID)
	assert.Equal(t, "agent-memory-embedding-vector-cosine", retrievalResults[0].Scorer.Name)

	require.NoError(t, loaded.MigrateContext(context.TODO()))
	assert.Equal(t, vector.TextHashVectorizerID, loaded.Vectorizer.ID)

	migratedResults, err := loaded.Search("agent", "semantic retrieval", 1)
	require.NoError(t, err)
	require.Len(t, migratedResults, 1)
	assert.Equal(t, "rag", migratedResults[0].Document.ID)
}

func TestStore_SetVectorizerRejectsDifferentEmbeddingEndpoint(t *testing.T) {
	t.Parallel()

	vectorizer := &agentMemoryEmbeddingVectorizer{}
	spec := agentMemoryEmbeddingSpec(0)
	spec.Provider = "ollama"
	spec.BaseURL = "http://127.0.0.1:11434"
	store, err := NewStoreWithVectorizer(spec, vectorizer)
	require.NoError(t, err)
	require.NoError(t, store.AddTextContext(context.TODO(), "agent", "rag", "semantic retrieval memory for local rag"))

	otherEndpoint := spec
	otherEndpoint.BaseURL = "http://127.0.0.1:11435"
	err = store.SetVectorizer(otherEndpoint, vectorizer)
	require.ErrorIs(t, err, vector.ErrVectorizerMismatch)

	sameEndpoint := spec
	sameEndpoint.BaseURL = "http://127.0.0.1:11434/"
	require.NoError(t, store.SetVectorizer(sameEndpoint, vectorizer))
}

func TestStore_MigrateToVectorizerContextReembedsLexicalStore(t *testing.T) {
	t.Parallel()

	store, err := NewStore(16)
	require.NoError(t, err)
	require.NoError(t, store.AddText("agent", "rag", "semantic retrieval memory for local rag"))
	require.NoError(t, store.AddText("agent", "shell", "shell command output capture"))

	require.NoError(t, store.MigrateToVectorizerContext(
		context.TODO(),
		agentMemoryEmbeddingSpec(0),
		&agentMemoryEmbeddingVectorizer{},
	))

	assert.Equal(t, 3, store.Dimensions)
	assert.True(t, store.Vectorizer.CompatibleWith(agentMemoryEmbeddingSpec(3)))

	docs := store.Documents("agent")
	require.Len(t, docs, 2)

	for _, doc := range docs {
		assert.True(t, doc.Vectorizer.CompatibleWith(agentMemoryEmbeddingSpec(3)))
		assert.Len(t, doc.Vector, 3)
	}

	results, err := store.SearchContext(context.TODO(), "agent", "semantic retrieval", 1)
	require.NoError(t, err)
	require.Len(t, results, 1)
	assert.Equal(t, "rag", results[0].Document.ID)
}

func TestLoadRefusesUnredactedPersistedText(t *testing.T) {
	t.Parallel()

	store, err := NewStore(16)
	require.NoError(t, err)
	require.NoError(t, store.AddText("agent", "secret", "password=hunter2"))
	store.Agents["agent"][0].Text = "password=hunter2"

	path := filepath.Join(t.TempDir(), "unredacted.json")
	data, err := json.Marshal(store)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(path, data, 0o600))

	_, err = Load(path)
	require.ErrorIs(t, err, ErrPrivacyPolicy)

	migrated, err := LoadWithOptions(path, LoadOptions{Migrate: true})
	require.NoError(t, err)
	assert.NotContains(t, migrated.Documents("agent")[0].Text, "hunter2")
}

func TestLoadRefusesUnredactedPersistedMetadata(t *testing.T) {
	t.Parallel()

	store, err := NewStore(16)
	require.NoError(t, err)
	require.NoError(t, store.AddText("agent", "secret", "safe memory"))
	store.Agents["agent"][0].Metadata = map[string]string{"auth_token": "abc123"}

	path := filepath.Join(t.TempDir(), "unredacted-metadata.json")
	data, err := json.Marshal(store)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(path, data, 0o600))

	_, err = Load(path)
	require.ErrorIs(t, err, ErrPrivacyPolicy)

	migrated, err := LoadWithOptions(path, LoadOptions{Migrate: true})
	require.NoError(t, err)
	assert.Equal(t, "[REDACTED]", migrated.Documents("agent")[0].Metadata["auth_token"])
}

func TestLoadRefusesUnredactedPersistedPath(t *testing.T) {
	t.Parallel()

	store, err := NewStore(16)
	require.NoError(t, err)
	require.NoError(t, store.Add("agent", Document{
		ID:   "path-secret",
		Path: "docs/safe.md",
		Text: "safe memory",
	}))
	store.Agents["agent"][0].ID = "doc.md?access_token=id123"
	store.Agents["agent"][0].Path = "docs/secret.md?access_token=artifact123"

	path := filepath.Join(t.TempDir(), "unredacted-path.json")
	data, err := json.Marshal(store)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(path, data, 0o600))

	_, err = Load(path)
	require.ErrorIs(t, err, ErrPrivacyPolicy)

	migrated, err := LoadWithOptions(path, LoadOptions{Migrate: true})
	require.NoError(t, err)
	assert.NotContains(t, migrated.Documents("agent")[0].Path, "artifact123")
	assert.NotContains(t, migrated.Documents("agent")[0].ID, "id123")
}

func TestLoadRejectsUnredactedPersistedAgentNameUnlessMigrated(t *testing.T) {
	t.Parallel()

	rawAgent := sensitiveAgentName
	store, err := NewStore(16)
	require.NoError(t, err)
	require.NoError(t, store.AddText("agent", "note", "safe agent memory"))
	store.Agents[rawAgent] = store.Agents["agent"]
	delete(store.Agents, "agent")

	path := filepath.Join(t.TempDir(), "unredacted-agent.json")
	data, err := json.Marshal(store)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(path, data, 0o600))

	_, err = Load(path)
	require.ErrorIs(t, err, ErrPrivacyPolicy)

	migrated, err := LoadWithOptions(path, LoadOptions{Migrate: true})
	require.NoError(t, err)
	require.Empty(t, migrated.Documents(rawAgent+"-missing"))

	docs := migrated.Documents(rawAgent)
	require.Len(t, docs, 1)
	assert.Equal(t, "note", docs[0].ID)

	persisted, err := json.Marshal(migrated)
	require.NoError(t, err)
	assert.NotContains(t, string(persisted), "agent123")
	assert.Contains(t, string(persisted), "[REDACTED]")
}

func TestLoadRejectsUnredactedPersistedVectorizerMetadataUnlessMigrated(t *testing.T) {
	t.Parallel()

	run := func(t *testing.T, mutate func(*Store)) {
		t.Helper()

		store, err := NewStore(16)
		require.NoError(t, err)
		require.NoError(t, store.AddText("agent", "note", "safe vectorizer memory"))
		mutate(store)

		path := filepath.Join(t.TempDir(), "unredacted-vectorizer.json")
		data, err := json.Marshal(store)
		require.NoError(t, err)
		require.NoError(t, os.WriteFile(path, data, 0o600))

		_, err = Load(path)
		require.ErrorIs(t, err, ErrPrivacyPolicy)

		migrated, err := LoadWithOptions(path, LoadOptions{Migrate: true})
		require.NoError(t, err)

		docs := migrated.Documents("agent")
		require.Len(t, docs, 1)
		assert.True(t, docs[0].Vectorizer.CompatibleWith(migrated.Vectorizer))

		persisted, err := json.Marshal(migrated)
		require.NoError(t, err)
		assert.NotContains(t, string(persisted), "model123")
	}

	t.Run("store vectorizer", func(t *testing.T) {
		t.Parallel()

		run(t, func(store *Store) {
			store.Vectorizer.Model = "local-token-frequency?api_key=model123/v1"
		})
	})

	t.Run("document vectorizer", func(t *testing.T) {
		t.Parallel()

		run(t, func(store *Store) {
			store.Agents["agent"][0].Vectorizer.Model = "local-token-frequency?api_key=model123/v1"
		})
	})
}

func assertAgentBackingOmitsText(t *testing.T, docs []Document, text string) {
	t.Helper()

	for i := range docs {
		assert.NotContains(t, docs[i].Text, text)
	}
}

func TestStore_SearchRetrievalRejectsNilContext(t *testing.T) {
	t.Parallel()

	store, err := NewStore(16)
	require.NoError(t, err)

	_, err = store.SearchRetrieval(nil, "reviewer", retrieval.Query{Text: "oauth"}) //nolint:staticcheck // Verify nil contexts are rejected instead of panicking.
	require.ErrorIs(t, err, vector.ErrContextRequired)
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

	store, err := LoadWithOptions(path, LoadOptions{Migrate: true})
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

type agentMemoryEmbeddingVectorizer struct{}

func (agentMemoryEmbeddingVectorizer) Vectorize(text string) (vector.Vector, error) {
	if strings.TrimSpace(text) == "" {
		return nil, vector.ErrEmptyText
	}

	return nil, vector.ErrContextRequired
}

func (agentMemoryEmbeddingVectorizer) VectorizeContext(ctx context.Context, text string) (vector.Vector, error) {
	if ctx == nil {
		return nil, vector.ErrContextRequired
	}

	if err := ctx.Err(); err != nil {
		return nil, err
	}

	if strings.TrimSpace(text) == "" {
		return nil, vector.ErrEmptyText
	}

	out := vector.Vector{0, 0, 0}

	for token := range strings.FieldsSeq(strings.ToLower(text)) {
		switch strings.Trim(token, ".,:;!?") {
		case "semantic", "retrieval", "memory", "rag", "local":
			out[0]++
		case "shell", "command", "output", "capture":
			out[1]++
		default:
			out[2]++
		}
	}

	return out, nil
}

func agentMemoryEmbeddingSpec(dimensions int) vector.VectorizerSpec {
	return vector.VectorizerSpec{
		ID:            "test-agent-memory-embedding",
		Model:         "agent-memory-embed-test",
		Normalization: "lowercase-token-buckets-v1",
		Version:       "1",
		Dimensions:    dimensions,
	}
}
