package vector

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/tommoulard/atteler/pkg/privacy"
	"github.com/tommoulard/atteler/pkg/retrieval"
)

const (
	duplicateID    = "same"
	staleHashModel = "stale-hash-model"
)

func TestStore_SearchCosineRanking(t *testing.T) {
	t.Parallel()

	store, err := NewStore(2)
	if err != nil {
		t.Fatalf("NewStore() error = %v", err)
	}

	for _, doc := range []Document{
		{ID: "mixed", Text: "partly related", Vector: Vector{0.5, 0.5}},
		{ID: "exact", Text: "directly related", Vector: Vector{1, 0}},
		{ID: "orthogonal", Text: "unrelated", Vector: Vector{0, 1}},
	} {
		addErr := store.Add(doc)
		if addErr != nil {
			t.Fatalf("Add(%q) error = %v", doc.ID, addErr)
		}
	}

	results, err := store.Search(Vector{1, 0}, 0)
	if err != nil {
		t.Fatalf("Search() error = %v", err)
	}

	got := resultIDs(results)

	want := []string{"exact", "mixed"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("Search() IDs = %v, want %v", got, want)
	}

	if results[0].Score <= results[1].Score {
		t.Fatalf("Search() scores = %v, want descending cosine ranking", scores(results))
	}
}

func TestStore_SearchTieBreaksByID(t *testing.T) {
	t.Parallel()

	store, err := NewStore(2)
	if err != nil {
		t.Fatalf("NewStore() error = %v", err)
	}

	for _, doc := range []Document{
		{ID: "b", SourceHash: sourceHash("b"), Vector: Vector{1, 0}},
		{ID: "a", SourceHash: sourceHash("a"), Vector: Vector{1, 0}},
	} {
		addErr := store.Add(doc)
		if addErr != nil {
			t.Fatalf("Add(%q) error = %v", doc.ID, addErr)
		}
	}

	results, err := store.Search(Vector{1, 0}, 0)
	if err != nil {
		t.Fatalf("Search() error = %v", err)
	}

	got := resultIDs(results)

	want := []string{"a", "b"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("Search() IDs = %v, want %v", got, want)
	}
}

func TestStore_DimensionErrors(t *testing.T) {
	t.Parallel()

	store, err := NewStore(3)
	if err != nil {
		t.Fatalf("NewStore() error = %v", err)
	}

	addErr := store.Add(Document{ID: "bad", SourceHash: sourceHash("bad"), Vector: Vector{1, 0}})
	if !errors.Is(addErr, ErrDimensionMismatch) {
		t.Fatalf("Add(dimension mismatch) error = %v, want ErrDimensionMismatch", addErr)
	}

	_, searchErr := store.Search(Vector{1, 0}, 0)
	if !errors.Is(searchErr, ErrDimensionMismatch) {
		t.Fatalf("Search(dimension mismatch) error = %v, want ErrDimensionMismatch", searchErr)
	}

	adoptingStore, err := NewStore(0)
	if err != nil {
		t.Fatalf("NewStore(0) error = %v", err)
	}

	addErr = adoptingStore.Add(Document{ID: "ok", SourceHash: sourceHash("ok"), Vector: Vector{1, 0}})
	if addErr != nil {
		t.Fatalf("Add(first vector) error = %v", addErr)
	}

	if adoptingStore.Dimensions != 2 {
		t.Fatalf("Dimensions = %d, want adopted dimension 2", adoptingStore.Dimensions)
	}

	addErr = adoptingStore.Add(Document{ID: "bad", SourceHash: sourceHash("bad"), Vector: Vector{1, 0, 0}})
	if !errors.Is(addErr, ErrDimensionMismatch) {
		t.Fatalf("Add(second dimension mismatch) error = %v, want ErrDimensionMismatch", addErr)
	}

	emptyStore, err := NewStore(0)
	if err != nil {
		t.Fatalf("NewStore(0) error = %v", err)
	}

	_, searchErr = emptyStore.Search(Vector{1, 0}, 0)
	if searchErr != nil {
		t.Fatalf("Search(empty adopting store) error = %v", searchErr)
	}

	if emptyStore.Dimensions != 0 {
		t.Fatalf("Dimensions after empty Search = %d, want 0", emptyStore.Dimensions)
	}
}

func TestStore_RejectsIncompatibleVectorizerMetadata(t *testing.T) {
	t.Parallel()

	spec := TextVectorizerSpec(2)
	store, err := NewStoreWithVectorizer(spec)
	require.NoError(t, err)

	err = store.Add(Document{
		ID:         "ok",
		SourceHash: sourceHash("ok"),
		Vector:     Vector{1, 0},
		Vectorizer: spec,
	})
	require.NoError(t, err)

	stale := spec
	stale.Model = "old-hash-model"
	err = store.Add(Document{
		ID:         "stale",
		SourceHash: sourceHash("stale"),
		Vector:     Vector{1, 0},
		Vectorizer: stale,
	})
	require.ErrorIs(t, err, ErrVectorizerMismatch)

	_, err = store.SearchWithVectorizer(Vector{1, 0}, stale, 1)
	require.ErrorIs(t, err, ErrVectorizerMismatch)
}

func TestStore_RejectsEmbeddingModelMismatchUntilRebuilt(t *testing.T) {
	t.Parallel()

	oldSpec := VectorizerSpec{
		ID:            "ollama-compatible-embedding",
		Model:         "nomic-embed-text",
		Normalization: "trim-space-v1",
		Version:       "1",
		Dimensions:    2,
	}

	newSpec := oldSpec
	newSpec.Model = "mxbai-embed-large"

	store, err := NewStoreWithVectorizer(oldSpec)
	require.NoError(t, err)
	require.NoError(t, store.Add(Document{
		ID:         "doc",
		Text:       "embedding model memory",
		Vector:     Vector{1, 0},
		Vectorizer: oldSpec,
	}))

	_, err = store.SearchWithVectorizer(Vector{1, 0}, newSpec, 1)
	require.ErrorIs(t, err, ErrVectorizerMismatch)

	err = store.Add(Document{
		ID:         "new-model-doc",
		Text:       "new embedding model memory",
		Vector:     Vector{1, 0},
		Vectorizer: newSpec,
	})
	require.ErrorIs(t, err, ErrVectorizerMismatch)

	require.NoError(t, store.Rebuild(newSpec, func(Document) (Vector, error) {
		return Vector{1, 0}, nil
	}))

	results, err := store.SearchWithVectorizer(Vector{1, 0}, newSpec, 1)
	require.NoError(t, err)

	if assert.Len(t, results, 1) {
		assert.Equal(t, "doc", results[0].Document.ID)
		assert.True(t, results[0].Document.Vectorizer.CompatibleWith(newSpec))
	}
}

func TestStore_RejectsEmbeddingVectorizerVersionMismatchUntilRebuilt(t *testing.T) {
	t.Parallel()

	oldSpec := VectorizerSpec{
		ID:            "ollama-compatible-embedding",
		Model:         "nomic-embed-text",
		Normalization: "trim-space-v1",
		Version:       "1",
		Dimensions:    2,
	}

	newSpec := oldSpec
	newSpec.Version = "2"

	store, err := NewStoreWithVectorizer(oldSpec)
	require.NoError(t, err)
	require.NoError(t, store.Add(Document{
		ID:         "doc",
		Text:       "embedding version memory",
		Vector:     Vector{1, 0},
		Vectorizer: oldSpec,
	}))

	_, err = store.SearchWithVectorizer(Vector{1, 0}, newSpec, 1)
	require.ErrorIs(t, err, ErrVectorizerMismatch)

	err = store.Add(Document{
		ID:         "new-version-doc",
		Text:       "new embedding version memory",
		Vector:     Vector{1, 0},
		Vectorizer: newSpec,
	})
	require.ErrorIs(t, err, ErrVectorizerMismatch)

	require.NoError(t, store.Rebuild(newSpec, func(Document) (Vector, error) {
		return Vector{1, 0}, nil
	}))

	results, err := store.SearchWithVectorizer(Vector{1, 0}, newSpec, 1)
	require.NoError(t, err)

	if assert.Len(t, results, 1) {
		assert.Equal(t, "doc", results[0].Document.ID)
		assert.True(t, results[0].Document.Vectorizer.CompatibleWith(newSpec))
	}
}

func TestStore_RejectsIncompleteVectorizerMetadata(t *testing.T) {
	t.Parallel()

	partial := VectorizerSpec{ID: TextHashVectorizerID, Dimensions: 2}
	_, err := NewStoreWithVectorizer(partial)
	require.ErrorIs(t, err, ErrVectorizerMismatch)

	store, err := NewStore(2)
	require.NoError(t, err)
	err = store.Add(Document{ID: "partial", SourceHash: sourceHash("partial"), Vector: Vector{1, 0}, Vectorizer: partial})
	require.ErrorIs(t, err, ErrVectorizerMismatch)

	negativeDimensions := TextVectorizerSpec(-1)
	err = store.Add(Document{
		ID:         "negative-vectorizer-dimensions",
		SourceHash: sourceHash("negative-vectorizer-dimensions"),
		Vector:     Vector{1, 0},
		Vectorizer: negativeDimensions,
	})
	require.ErrorIs(t, err, ErrInvalidDimensions)

	spec := TextVectorizerSpec(2)
	pinned, err := NewStoreWithVectorizer(spec)
	require.NoError(t, err)
	require.NoError(t, pinned.Add(Document{ID: "doc", SourceHash: sourceHash("doc"), Vector: Vector{1, 0}, Vectorizer: spec}))

	_, err = pinned.SearchWithVectorizer(Vector{1, 0}, partial, 1)
	require.ErrorIs(t, err, ErrVectorizerMismatch)

	err = pinned.Rebuild(partial, func(Document) (Vector, error) {
		return Vector{1, 0}, nil
	})
	require.ErrorIs(t, err, ErrVectorizerMismatch)
}

func TestStore_StampsDimensionlessPinnedVectorizer(t *testing.T) {
	t.Parallel()

	spec := TextVectorizerSpec(0)
	store, err := NewStoreWithVectorizer(spec)
	require.NoError(t, err)

	err = store.Add(Document{
		ID:         "adopt",
		SourceHash: sourceHash("adopt"),
		Vector:     Vector{1, 0},
	})
	require.NoError(t, err)

	assert.Equal(t, 2, store.Dimensions)
	assert.Equal(t, 2, store.Vectorizer.Dimensions)
	require.Len(t, store.Documents, 1)
	assert.Equal(t, 2, store.Documents[0].Vectorizer.Dimensions)
	assert.True(t, store.Vectorizer.CompatibleWith(store.Documents[0].Vectorizer))
}

func TestStore_SearchRejectsTamperedDocumentVectorizer(t *testing.T) {
	t.Parallel()

	spec := TextVectorizerSpec(2)
	store, err := NewStoreWithVectorizer(spec)
	require.NoError(t, err)
	require.NoError(t, store.Add(Document{ID: "ok", SourceHash: sourceHash("ok"), Vector: Vector{1, 0}, Vectorizer: spec}))

	stale := spec
	stale.Normalization = "old-normalization"
	store.Documents[0].Vectorizer = stale

	_, err = store.SearchWithVectorizer(Vector{1, 0}, spec, 1)
	require.ErrorIs(t, err, ErrVectorizerMismatch)

	store.Documents[0].Vectorizer = VectorizerSpec{}
	_, err = store.SearchWithVectorizer(Vector{1, 0}, spec, 1)
	require.ErrorIs(t, err, ErrVectorizerMismatch)

	dimensionless := spec
	dimensionless.Dimensions = 0
	store.Documents[0].Vectorizer = dimensionless
	_, err = store.SearchWithVectorizer(Vector{1, 0}, spec, 1)
	require.ErrorIs(t, err, ErrDimensionMismatch)

	wrongDimensions := spec
	wrongDimensions.Dimensions = 3
	store.Documents[0].Vectorizer = wrongDimensions
	_, err = store.SearchWithVectorizer(Vector{1, 0}, spec, 1)
	require.ErrorIs(t, err, ErrDimensionMismatch)
}

func TestStore_SearchRejectsMissingOrDuplicateIDs(t *testing.T) {
	t.Parallel()

	spec := TextVectorizerSpec(2)
	store, err := NewStoreWithVectorizer(spec)
	require.NoError(t, err)
	require.NoError(t, store.Add(Document{ID: "first", SourceHash: sourceHash("first"), Vector: Vector{1, 0}, Vectorizer: spec}))
	require.NoError(t, store.Add(Document{ID: "second", SourceHash: sourceHash("second"), Vector: Vector{1, 0}, Vectorizer: spec}))

	store.Documents[0].ID = " "
	_, err = store.SearchWithVectorizer(Vector{1, 0}, spec, 10)
	require.ErrorIs(t, err, ErrMissingID)

	store.Documents[0].ID = duplicateID
	store.Documents[1].ID = duplicateID
	_, err = store.SearchWithVectorizer(Vector{1, 0}, spec, 10)
	require.ErrorIs(t, err, ErrDuplicateID)
}

func TestStore_SearchRequiresQueryVectorizerForPinnedStore(t *testing.T) {
	t.Parallel()

	spec := TextVectorizerSpec(2)
	store, err := NewStoreWithVectorizer(spec)
	require.NoError(t, err)
	require.NoError(t, store.Add(Document{ID: "doc", SourceHash: sourceHash("doc"), Vector: Vector{1, 0}, Vectorizer: spec}))

	_, err = store.Search(Vector{1, 0}, 1)
	require.ErrorIs(t, err, ErrVectorizerRequired)

	legacy, err := NewStore(2)
	require.NoError(t, err)
	require.NoError(t, legacy.Add(Document{ID: "doc", SourceHash: sourceHash("doc"), Vector: Vector{1, 0}}))
	_, err = legacy.SearchWithVectorizer(Vector{1, 0}, spec, 1)
	require.ErrorIs(t, err, ErrVectorizerMismatch)
}

func TestStore_JSONUsesVersionedSchemaFields(t *testing.T) {
	t.Parallel()

	spec := TextVectorizerSpec(2)
	store, err := NewStoreWithVectorizer(spec)
	require.NoError(t, err)
	require.NoError(t, store.Add(Document{
		ID:         "doc",
		Text:       "memory",
		Vector:     Vector{1, 0},
		Vectorizer: spec,
		Provenance: map[string]string{"source_type": "test"},
	}))

	data, err := json.Marshal(store)
	require.NoError(t, err)
	assert.Contains(t, string(data), `"schema_version":`)
	assert.Contains(t, string(data), `"created_at":`)
	assert.Contains(t, string(data), `"updated_at":`)
	assert.Contains(t, string(data), `"source_hash":`)
	assert.Contains(t, string(data), `"normalization":`)
	assert.Contains(t, string(data), `"provenance":`)
	assert.NotContains(t, string(data), `"SchemaVersion"`)

	var decoded Store
	require.NoError(t, json.Unmarshal(data, &decoded))
	assert.Equal(t, StoreSchemaVersion, decoded.SchemaVersion)
	assert.Equal(t, 2, decoded.Dimensions)
	assert.Equal(t, spec.ID, decoded.Vectorizer.ID)
	assert.Equal(t, spec.Model, decoded.Vectorizer.Model)
	assert.Equal(t, spec.Normalization, decoded.Vectorizer.Normalization)
	assert.Equal(t, spec.Version, decoded.Vectorizer.Version)
	assert.False(t, decoded.CreatedAt.IsZero())
	assert.False(t, decoded.UpdatedAt.IsZero())
	require.Len(t, decoded.Documents, 1)
	assert.Equal(t, sourceHash("memory"), decoded.Documents[0].SourceHash)
	assert.Equal(t, "test", decoded.Documents[0].Provenance["source_type"])
	assert.False(t, decoded.Documents[0].CreatedAt.IsZero())
	assert.False(t, decoded.Documents[0].UpdatedAt.IsZero())
}

func TestStore_SaveTightensExistingFilePermissions(t *testing.T) {
	t.Parallel()

	spec := TextVectorizerSpec(2)
	store, err := NewStoreWithVectorizer(spec)
	require.NoError(t, err)
	require.NoError(t, store.Add(Document{
		ID:         "doc",
		Text:       "private vector memory",
		Vector:     textHashVector(t, spec, "private vector memory"),
		Vectorizer: spec,
	}))

	path := filepath.Join(t.TempDir(), "vectors.json")
	//nolint:gosec // Intentionally start with loose permissions to prove Save tightens persisted vector stores.
	require.NoError(t, os.WriteFile(path, []byte("{}"), 0o644))
	require.NoError(t, store.Save(path))

	info, err := os.Stat(path)
	require.NoError(t, err)
	assert.Equal(t, os.FileMode(0o600), info.Mode().Perm())
}

func TestStore_AddRecordsDirectProvenance(t *testing.T) {
	t.Parallel()

	store, err := NewStore(2)
	require.NoError(t, err)
	require.NoError(t, store.Add(Document{ID: "doc", Text: "direct vector provenance", Vector: Vector{1, 0}}))

	require.Len(t, store.Documents, 1)
	assert.Equal(t, "direct", store.Documents[0].Provenance["source_type"])
	assert.Equal(t, privacy.RedactionPolicyVersion, store.Documents[0].Provenance["privacy_policy"])
}

func TestStore_RedactsSensitiveIDBeforePersistenceAndDelete(t *testing.T) {
	t.Parallel()

	store, err := NewStore(2)
	require.NoError(t, err)

	rawID := "docs/secret.md?access_token=artifact123"
	require.NoError(t, store.Add(Document{
		ID:         rawID,
		SourceHash: sourceHash("external source"),
		Vector:     Vector{1, 0},
	}))

	require.Len(t, store.Documents, 1)
	assert.NotContains(t, store.Documents[0].ID, "artifact123")
	assert.True(t, store.Delete(rawID))
	assert.Empty(t, store.Documents)
}

func TestStore_RedactsSensitiveIDWithoutCollapsingPathSuffix(t *testing.T) {
	t.Parallel()

	store, err := NewStore(2)
	require.NoError(t, err)

	firstID := "tenant/access_token=artifact123/first"
	secondID := "tenant/access_token=artifact123/second"

	require.NoError(t, store.Add(Document{ID: firstID, SourceHash: sourceHash("first"), Vector: Vector{1, 0}}))
	require.NoError(t, store.Add(Document{ID: secondID, SourceHash: sourceHash("second"), Vector: Vector{0, 1}}))

	require.Len(t, store.Documents, 2)
	assert.NotContains(t, store.Documents[0].ID, "artifact123")
	assert.NotContains(t, store.Documents[1].ID, "artifact123")
	assert.True(t, strings.HasSuffix(store.Documents[0].ID, "/first"))
	assert.True(t, strings.HasSuffix(store.Documents[1].ID, "/second"))

	assert.True(t, store.Delete(firstID))
	require.Len(t, store.Documents, 1)
	assert.True(t, strings.HasSuffix(store.Documents[0].ID, "/second"))
}

func TestStore_RedactsSensitiveVectorizerMetadata(t *testing.T) {
	t.Parallel()

	spec := VectorizerSpec{
		ID:            "ollama-compatible-embedding",
		Model:         "tenant-embed?api_key=abc123",
		Normalization: "trim-space-v1",
		Version:       "1",
		Dimensions:    2,
	}

	store, err := NewStoreWithVectorizer(spec)
	require.NoError(t, err)
	assert.NotContains(t, store.Vectorizer.Model, "abc123")

	require.NoError(t, store.Add(Document{
		ID:         "doc",
		SourceHash: sourceHash("external source"),
		Vector:     Vector{1, 0},
		Vectorizer: spec,
	}))
	assert.NotContains(t, store.Documents[0].Vectorizer.Model, "abc123")

	results, err := store.SearchWithVectorizer(Vector{1, 0}, spec, 1)
	require.NoError(t, err)
	require.Len(t, results, 1)

	path := filepath.Join(t.TempDir(), "vectors.json")
	require.NoError(t, store.Save(path))

	data, err := os.ReadFile(path)
	require.NoError(t, err)
	assert.NotContains(t, string(data), "abc123")
	assert.Contains(t, string(data), "[REDACTED]")
}

func TestVectorizerSpecRedactionPreservesVersionSuffix(t *testing.T) {
	t.Parallel()

	v1 := VectorizerSpec{
		ID:            "ollama-compatible-embedding",
		Model:         "tenant-embed?api_key=abc123/v1",
		Normalization: "trim-space-v1",
		Version:       "1",
		Dimensions:    2,
	}
	v2 := v1
	v2.Model = "tenant-embed?api_key=abc123/v2"

	v1Store, err := NewStoreWithVectorizer(v1)
	require.NoError(t, err)

	v2Store, err := NewStoreWithVectorizer(v2)
	require.NoError(t, err)

	assert.NotContains(t, v1Store.Vectorizer.Model, "abc123")
	assert.NotContains(t, v2Store.Vectorizer.Model, "abc123")
	assert.Contains(t, v1Store.Vectorizer.Model, "/v1")
	assert.Contains(t, v2Store.Vectorizer.Model, "/v2")
	assert.False(t, v1Store.Vectorizer.CompatibleWith(v2Store.Vectorizer))
}

func TestLoadRejectsUnredactedPersistedIDUnlessMigrated(t *testing.T) {
	t.Parallel()

	vectorizer, err := NewTextVectorizer(8)
	require.NoError(t, err)

	text := "safe vector memory"
	vec, err := vectorizer.Vectorize(text)
	require.NoError(t, err)

	store := &Store{
		SchemaVersion: StoreSchemaVersion,
		Dimensions:    vectorizer.Dimensions,
		Vectorizer:    vectorizer.Spec(),
		Documents: []Document{{
			ID:         "doc.md?access_token=id123",
			Text:       text,
			Vector:     vec,
			Vectorizer: vectorizer.Spec(),
			SourceHash: sourceHash(text),
			Provenance: map[string]string{"source_type": "test"},
		}},
	}

	path := filepath.Join(t.TempDir(), "unredacted-id.json")
	writeVectorStoreJSON(t, path, store)

	_, err = Load(path)
	require.ErrorIs(t, err, ErrPrivacyPolicy)

	migrated, err := LoadWithOptions(path, LoadOptions{
		Migrate:    true,
		Vectorizer: vectorizer.Spec(),
		Vectorize: func(doc Document) (Vector, error) {
			return vectorizer.Vectorize(doc.Text)
		},
	})
	require.NoError(t, err)
	require.Len(t, migrated.Documents, 1)
	assert.NotContains(t, migrated.Documents[0].ID, "id123")
}

func TestLoadRejectsPersistedStoreDimensionMismatchUnlessMigrated(t *testing.T) {
	t.Parallel()

	vectorizer, err := NewTextVectorizer(8)
	require.NoError(t, err)

	text := "dimension mismatch memory"
	vec, err := vectorizer.Vectorize(text)
	require.NoError(t, err)

	store := &Store{
		SchemaVersion: StoreSchemaVersion,
		Dimensions:    vectorizer.Dimensions - 1,
		Vectorizer:    vectorizer.Spec(),
		Documents: []Document{{
			ID:         "doc",
			Text:       text,
			Vector:     vec,
			Vectorizer: vectorizer.Spec(),
			SourceHash: sourceHash(text),
			Provenance: map[string]string{"source_type": "test"},
		}},
	}

	path := filepath.Join(t.TempDir(), "dimension-mismatch.json")
	writeVectorStoreJSON(t, path, store)

	_, err = Load(path)
	require.ErrorIs(t, err, ErrDimensionMismatch)

	replacement, err := NewTextVectorizer(4)
	require.NoError(t, err)

	migrated, err := LoadWithOptions(path, LoadOptions{
		Migrate:    true,
		Vectorizer: replacement.Spec(),
		Vectorize: func(doc Document) (Vector, error) {
			return replacement.Vectorize(doc.Text)
		},
	})
	require.NoError(t, err)
	require.Len(t, migrated.Documents, 1)
	assert.Equal(t, replacement.Dimensions, migrated.Dimensions)
	assert.Equal(t, replacement.Spec(), migrated.Vectorizer)
	assert.Len(t, migrated.Documents[0].Vector, replacement.Dimensions)
}

func TestLoadRejectsUnredactedPersistedVectorizerMetadataUnlessMigrated(t *testing.T) {
	t.Parallel()

	text := "safe vectorizer metadata memory"
	rawSpec := VectorizerSpec{
		ID:            "ollama-compatible-embedding",
		Model:         "tenant-embed?api_key=model123/v1",
		Normalization: "trim-space-v1",
		Version:       "1",
		Dimensions:    2,
	}

	store := &Store{
		SchemaVersion: StoreSchemaVersion,
		Dimensions:    rawSpec.Dimensions,
		Vectorizer:    rawSpec,
		Documents: []Document{{
			ID:         "doc",
			Text:       text,
			Vector:     Vector{1, 0},
			Vectorizer: rawSpec,
			SourceHash: sourceHash(text),
			Provenance: map[string]string{"source_type": "test"},
		}},
	}

	path := filepath.Join(t.TempDir(), "unredacted-vectorizer.json")
	writeVectorStoreJSON(t, path, store)

	_, err := Load(path)
	require.ErrorIs(t, err, ErrPrivacyPolicy)

	vectorizer, err := NewTextVectorizer(8)
	require.NoError(t, err)

	migrated, err := LoadWithOptions(path, LoadOptions{
		Migrate:    true,
		Vectorizer: vectorizer.Spec(),
		Vectorize: func(doc Document) (Vector, error) {
			return vectorizer.Vectorize(doc.Text)
		},
	})
	require.NoError(t, err)
	assert.True(t, migrated.Vectorizer.CompatibleWith(vectorizer.Spec()))

	data, err := json.Marshal(migrated)
	require.NoError(t, err)
	assert.NotContains(t, string(data), "model123")
}

func TestStore_SaveLoadRoundTripValidatesPersistedMetadata(t *testing.T) {
	t.Parallel()

	vectorizer, err := NewTextVectorizer(8)
	require.NoError(t, err)

	store, err := NewStoreWithVectorizer(vectorizer.Spec())
	require.NoError(t, err)

	activeVector, err := vectorizer.Vectorize("durable persisted vector memory")
	require.NoError(t, err)
	require.NoError(t, store.Add(Document{
		ID:         "active",
		Text:       "durable persisted vector memory",
		Vector:     activeVector,
		Vectorizer: vectorizer.Spec(),
		Provenance: map[string]string{"source": "fixture"},
	}))

	expiredAt := time.Now().UTC().Add(-time.Second)
	expiredVector, err := vectorizer.Vectorize("temporary vector memory")
	require.NoError(t, err)
	require.NoError(t, store.Add(Document{
		ID:         "expired",
		Text:       "temporary vector memory",
		Vector:     expiredVector,
		Vectorizer: vectorizer.Spec(),
		ExpiresAt:  &expiredAt,
	}))

	path := filepath.Join(t.TempDir(), "nested", "vectors.json")
	require.NoError(t, store.Save(path))

	data, err := os.ReadFile(path)
	require.NoError(t, err)
	assert.NotContains(t, string(data), "temporary vector memory")

	loaded, err := Load(path)
	require.NoError(t, err)
	assert.True(t, loaded.Vectorizer.CompatibleWith(vectorizer.Spec()))
	require.Len(t, loaded.Documents, 1)
	assert.Equal(t, "active", loaded.Documents[0].ID)
	assert.Equal(t, sourceHash("durable persisted vector memory"), loaded.Documents[0].SourceHash)

	query, err := vectorizer.Vectorize("persisted memory")
	require.NoError(t, err)
	results, err := loaded.SearchWithVectorizer(query, vectorizer.Spec(), 1)
	require.NoError(t, err)
	require.Len(t, results, 1)
	assert.Equal(t, "active", results[0].Document.ID)
}

func TestStore_SaveCompactsExpiredBeforeRequiringVectorizerMetadata(t *testing.T) {
	t.Parallel()

	store, err := NewStore(2)
	require.NoError(t, err)

	expiredAt := time.Now().UTC().Add(-time.Second)
	require.NoError(t, store.Add(Document{
		ID:        "expired",
		Text:      "expired unpinned vector memory",
		Vector:    Vector{1, 0},
		ExpiresAt: &expiredAt,
	}))
	store.Vectorizer = TextVectorizerSpec(2)
	store.Vectorizer.Model = staleHashModel
	store.Documents[0].Vectorizer = store.Vectorizer

	path := filepath.Join(t.TempDir(), "vectors.json")
	require.NoError(t, store.Save(path))

	persisted, err := os.ReadFile(path)
	require.NoError(t, err)
	assert.NotContains(t, string(persisted), "expired unpinned vector memory")

	loaded, err := Load(path)
	require.NoError(t, err)
	assert.Empty(t, loaded.Documents)
	assert.True(t, loaded.Vectorizer.IsZero())
}

func TestStore_SaveCompactsExpiredBeforeDimensionValidation(t *testing.T) {
	t.Parallel()

	expiredAt := time.Now().UTC().Add(-time.Second)
	store := Store{
		SchemaVersion: StoreSchemaVersion,
		Dimensions:    -1,
		Vectorizer:    TextVectorizerSpec(-1),
		Documents: []Document{{
			ID:        "expired",
			Text:      "expired invalid-dimension vector memory",
			Vector:    Vector{1, 0},
			ExpiresAt: &expiredAt,
		}},
	}

	path := filepath.Join(t.TempDir(), "vectors.json")
	require.NoError(t, store.Save(path))

	loaded, err := Load(path)
	require.NoError(t, err)
	assert.Equal(t, 0, loaded.Dimensions)
	assert.True(t, loaded.Vectorizer.IsZero())
	assert.Empty(t, loaded.Documents)
}

func TestLoadRejectsStaleOrIncompleteVectorizerMetadata(t *testing.T) {
	t.Parallel()

	spec := TextVectorizerSpec(2)
	store, err := NewStoreWithVectorizer(spec)
	require.NoError(t, err)
	require.NoError(t, store.Add(Document{ID: "doc", Text: "memory", Vector: Vector{1, 0}, Vectorizer: spec}))

	path := filepath.Join(t.TempDir(), "stale.json")
	store.Documents[0].Vectorizer.Model = staleHashModel
	writeVectorStoreJSON(t, path, store)

	_, err = Load(path)
	require.ErrorIs(t, err, ErrVectorizerMismatch)

	store.Documents[0].Vectorizer = spec
	store.Documents[0].Vectorizer.Normalization = "legacy-token-normalization"
	writeVectorStoreJSON(t, path, store)

	_, err = Load(path)
	require.ErrorIs(t, err, ErrVectorizerMismatch)

	store.Documents[0].Vectorizer = VectorizerSpec{}
	writeVectorStoreJSON(t, path, store)

	_, err = Load(path)
	require.ErrorIs(t, err, ErrVectorizerMismatch)
}

func TestLoadAndSaveRefuseMissingStoreVectorizerUnlessMigrated(t *testing.T) {
	t.Parallel()

	store, err := NewStore(2)
	require.NoError(t, err)
	require.NoError(t, store.Add(Document{
		ID:     "legacy",
		Text:   "legacy vector memory",
		Vector: Vector{1, 0},
	}))

	path := filepath.Join(t.TempDir(), "missing-vectorizer.json")
	err = store.Save(path)
	require.ErrorIs(t, err, ErrVectorizerMismatch)

	writeVectorStoreJSON(t, path, store)

	_, err = Load(path)
	require.ErrorIs(t, err, ErrVectorizerMismatch)

	vectorizer, err := NewTextVectorizer(4)
	require.NoError(t, err)

	migrated, err := LoadWithOptions(path, LoadOptions{
		Migrate:    true,
		Vectorizer: vectorizer.Spec(),
		Vectorize: func(doc Document) (Vector, error) {
			return vectorizer.Vectorize(doc.Text)
		},
	})
	require.NoError(t, err)
	assert.True(t, migrated.Vectorizer.CompatibleWith(vectorizer.Spec()))
	require.Len(t, migrated.Documents, 1)
	assert.True(t, migrated.Documents[0].Vectorizer.CompatibleWith(vectorizer.Spec()))
}

func TestLoadRefusesLegacySchemaUnlessMigrated(t *testing.T) {
	t.Parallel()

	spec := TextVectorizerSpec(2)
	store, err := NewStoreWithVectorizer(spec)
	require.NoError(t, err)
	require.NoError(t, store.Add(Document{
		ID:         "doc",
		Text:       "legacy vector memory",
		Vector:     textHashVector(t, spec, "legacy vector memory"),
		Vectorizer: spec,
	}))
	store.SchemaVersion = 0

	path := filepath.Join(t.TempDir(), "legacy-schema.json")
	writeVectorStoreJSON(t, path, store)

	_, err = Load(path)
	require.ErrorIs(t, err, ErrIncompatibleSchema)

	vectorizer, err := NewTextVectorizer(4)
	require.NoError(t, err)

	migrated, err := LoadWithOptions(path, LoadOptions{
		Migrate:    true,
		Vectorizer: vectorizer.Spec(),
		Vectorize: func(doc Document) (Vector, error) {
			return vectorizer.Vectorize(doc.Text)
		},
	})
	require.NoError(t, err)
	assert.Equal(t, StoreSchemaVersion, migrated.SchemaVersion)
	assert.True(t, migrated.Vectorizer.CompatibleWith(vectorizer.Spec()))
	require.Len(t, migrated.Documents, 1)
	assert.True(t, migrated.Documents[0].Vectorizer.CompatibleWith(vectorizer.Spec()))
}

func TestStore_SaveAndSearchRefuseMissingSchemaUnlessRebuilt(t *testing.T) {
	t.Parallel()

	spec := TextVectorizerSpec(2)
	store, err := NewStoreWithVectorizer(spec)
	require.NoError(t, err)
	require.NoError(t, store.Add(Document{
		ID:         "doc",
		Text:       "legacy vector memory",
		Vector:     textHashVector(t, spec, "legacy vector memory"),
		Vectorizer: spec,
	}))
	store.SchemaVersion = 0

	path := filepath.Join(t.TempDir(), "legacy-schema.json")
	err = store.Save(path)
	require.ErrorIs(t, err, ErrIncompatibleSchema)

	_, err = store.SearchWithVectorizer(Vector{1, 0}, spec, 1)
	require.ErrorIs(t, err, ErrIncompatibleSchema)

	vectorizer, err := NewTextVectorizer(4)
	require.NoError(t, err)
	require.NoError(t, store.Rebuild(vectorizer.Spec(), func(doc Document) (Vector, error) {
		return vectorizer.Vectorize(doc.Text)
	}))

	require.NoError(t, store.Save(path))
	assert.Equal(t, StoreSchemaVersion, store.SchemaVersion)
	assert.True(t, store.Vectorizer.CompatibleWith(vectorizer.Spec()))
}

func TestLoadRejectsSelfConsistentStaleTextHashVectorizer(t *testing.T) {
	t.Parallel()

	staleSpec := TextVectorizerSpec(2)
	staleSpec.Model = staleHashModel

	store, err := NewStoreWithVectorizer(TextVectorizerSpec(2))
	require.NoError(t, err)
	require.NoError(t, store.Add(Document{
		ID:         "doc",
		Text:       "token refresh memory",
		Vector:     textHashVector(t, TextVectorizerSpec(2), "token refresh memory"),
		Vectorizer: TextVectorizerSpec(2),
	}))

	store.Vectorizer = staleSpec
	store.Documents[0].Vectorizer = staleSpec

	path := filepath.Join(t.TempDir(), "stale-built-in.json")
	writeVectorStoreJSON(t, path, store)

	_, err = Load(path)
	require.ErrorIs(t, err, ErrVectorizerMismatch)

	err = store.Save(path)
	require.ErrorIs(t, err, ErrVectorizerMismatch)

	vectorizer, err := NewTextVectorizer(4)
	require.NoError(t, err)

	migrated, err := LoadWithOptions(path, LoadOptions{
		Migrate:    true,
		Vectorizer: vectorizer.Spec(),
		Vectorize: func(doc Document) (Vector, error) {
			return vectorizer.Vectorize(doc.Text)
		},
	})
	require.NoError(t, err)
	assert.True(t, migrated.Vectorizer.CompatibleWith(vectorizer.Spec()))
	require.Len(t, migrated.Documents, 1)
	assert.True(t, migrated.Documents[0].Vectorizer.CompatibleWith(vectorizer.Spec()))
}

func TestStore_SaveRefusesStaleVectorizerUntilRebuilt(t *testing.T) {
	t.Parallel()

	vectorizer, err := NewTextVectorizer(8)
	require.NoError(t, err)

	store, err := NewStoreWithVectorizer(vectorizer.Spec())
	require.NoError(t, err)

	vec, err := vectorizer.Vectorize("token refresh memory")
	require.NoError(t, err)
	require.NoError(t, store.Add(Document{
		ID:         "doc",
		Text:       "token refresh memory",
		Vector:     vec,
		Vectorizer: vectorizer.Spec(),
	}))

	store.Documents[0].Vectorizer.Model = staleHashModel

	path := filepath.Join(t.TempDir(), "vectors.json")
	err = store.Save(path)
	require.ErrorIs(t, err, ErrVectorizerMismatch)

	require.NoError(t, store.Rebuild(vectorizer.Spec(), func(doc Document) (Vector, error) {
		return vectorizer.Vectorize(doc.Text)
	}))
	require.NoError(t, store.Save(path))

	loaded, err := Load(path)
	require.NoError(t, err)
	require.Len(t, loaded.Documents, 1)
	assert.True(t, loaded.Documents[0].Vectorizer.CompatibleWith(vectorizer.Spec()))
}

func TestEmptyStoreSaveClearsStaleVectorizerMetadata(t *testing.T) {
	t.Parallel()

	staleSpec := TextVectorizerSpec(2)
	staleSpec.Model = staleHashModel

	store, err := NewStore(2)
	require.NoError(t, err)

	store.Vectorizer = staleSpec

	path := filepath.Join(t.TempDir(), "empty-stale-vectorizer.json")
	err = store.Save(path)
	require.NoError(t, err)
	assert.True(t, store.Vectorizer.IsZero())
	assert.Equal(t, 0, store.Dimensions)

	loaded, err := Load(path)
	require.NoError(t, err)
	assert.True(t, loaded.Vectorizer.IsZero())
	assert.Equal(t, 0, loaded.Dimensions)

	store.Vectorizer = staleSpec
	writeVectorStoreJSON(t, path, store)

	loaded, err = Load(path)
	require.NoError(t, err)
	assert.True(t, loaded.Vectorizer.IsZero())
	assert.Equal(t, 0, loaded.Dimensions)

	err = store.Add(Document{ID: "doc", Text: "new memory", Vector: Vector{1, 0}})
	require.ErrorIs(t, err, ErrVectorizerMismatch)
}

func TestEmptyStoreSaveClearsDimensionlessVectorizerMetadata(t *testing.T) {
	t.Parallel()

	store, err := NewStore(0)
	require.NoError(t, err)

	store.Vectorizer = VectorizerSpec{
		ID:            "ollama-compatible-embedding",
		Model:         "nomic-embed-text",
		Normalization: "trim-space-v1",
		Version:       vectorizerSpecVersion,
	}

	path := filepath.Join(t.TempDir(), "dimensionless-empty-vectorizer.json")
	require.NoError(t, store.Save(path))
	assert.True(t, store.Vectorizer.IsZero())
	assert.Equal(t, 0, store.Dimensions)

	loaded, err := Load(path)
	require.NoError(t, err)
	assert.True(t, loaded.Vectorizer.IsZero())
	assert.Equal(t, 0, loaded.Dimensions)

	results, err := loaded.SearchWithVectorizer(Vector{1, 0}, TextVectorizerSpec(2), 1)
	require.NoError(t, err)
	assert.Empty(t, results)
}

func TestEmptyStoreSaveClearsCompleteVectorizerMetadata(t *testing.T) {
	t.Parallel()

	spec := TextVectorizerSpec(2)
	store, err := NewStoreWithVectorizer(spec)
	require.NoError(t, err)

	path := filepath.Join(t.TempDir(), "empty-pinned-vectorizer.json")
	require.NoError(t, store.Save(path))
	assert.True(t, store.Vectorizer.IsZero())
	assert.Equal(t, 0, store.Dimensions)

	loaded, err := Load(path)
	require.NoError(t, err)
	assert.True(t, loaded.Vectorizer.IsZero())
	assert.Equal(t, 0, loaded.Dimensions)

	require.NoError(t, loaded.Add(Document{
		ID:         "doc",
		SourceHash: sourceHash("doc"),
		Vector:     Vector{1, 0, 0},
		Vectorizer: VectorizerSpec{
			ID:            "ollama-compatible-embedding",
			Model:         "other-embed",
			Normalization: "trim-space-v1",
			Version:       vectorizerSpecVersion,
			Dimensions:    3,
		},
	}))
	assert.Equal(t, 3, loaded.Dimensions)
}

func TestEmptyStoreSavePreservesExplicitDimensionsWithoutVectorizer(t *testing.T) {
	t.Parallel()

	store, err := NewStore(2)
	require.NoError(t, err)

	path := filepath.Join(t.TempDir(), "empty-explicit-dimensions.json")
	require.NoError(t, store.Save(path))
	assert.True(t, store.Vectorizer.IsZero())
	assert.Equal(t, 2, store.Dimensions)

	loaded, err := Load(path)
	require.NoError(t, err)
	assert.True(t, loaded.Vectorizer.IsZero())
	assert.Equal(t, 2, loaded.Dimensions)

	err = loaded.Add(Document{
		ID:         "doc",
		SourceHash: sourceHash("doc"),
		Vector:     Vector{1, 0, 0},
	})
	require.ErrorIs(t, err, ErrDimensionMismatch)
}

func TestLoadCompactsExpiredStaleVectorContentBeforeValidation(t *testing.T) {
	t.Parallel()

	staleSpec := TextVectorizerSpec(2)
	staleSpec.Model = staleHashModel

	expiredAt := time.Now().UTC().Add(-time.Second)
	store := Store{
		SchemaVersion: StoreSchemaVersion,
		Dimensions:    2,
		Vectorizer:    staleSpec,
		Documents: []Document{{
			ID:         "expired",
			Text:       "expired stale vector memory",
			Vector:     Vector{1, 0},
			Vectorizer: staleSpec,
			ExpiresAt:  &expiredAt,
		}},
	}

	path := filepath.Join(t.TempDir(), "expired-stale-vector.json")
	writeVectorStoreJSON(t, path, &store)

	loaded, err := Load(path)
	require.NoError(t, err)
	assert.Empty(t, loaded.Documents)
	assert.True(t, loaded.Vectorizer.IsZero())
}

func TestLoadCompactsExpiredLegacySchemaVectorContentBeforeValidation(t *testing.T) {
	t.Parallel()

	staleSpec := TextVectorizerSpec(2)
	staleSpec.Model = staleHashModel

	expiredAt := time.Now().UTC().Add(-time.Second)
	store := Store{
		Dimensions: 2,
		Vectorizer: staleSpec,
		Documents: []Document{{
			ID:         "expired",
			Text:       "expired legacy vector memory",
			Vector:     Vector{1, 0},
			Vectorizer: staleSpec,
			ExpiresAt:  &expiredAt,
		}},
	}

	path := filepath.Join(t.TempDir(), "expired-legacy-vector.json")
	writeVectorStoreJSON(t, path, &store)

	loaded, err := Load(path)
	require.NoError(t, err)
	assert.Equal(t, StoreSchemaVersion, loaded.SchemaVersion)
	assert.Empty(t, loaded.Documents)
	assert.True(t, loaded.Vectorizer.IsZero())
}

func TestLoadCompactsExpiredNegativeDimensionsBeforeValidation(t *testing.T) {
	t.Parallel()

	expiredAt := time.Now().UTC().Add(-time.Second)
	store := Store{
		SchemaVersion: StoreSchemaVersion,
		Dimensions:    -1,
		Vectorizer:    TextVectorizerSpec(-1),
		Documents: []Document{{
			ID:        "expired",
			Text:      "expired negative-dimension vector memory",
			Vector:    Vector{1, 0},
			ExpiresAt: &expiredAt,
		}},
	}

	path := filepath.Join(t.TempDir(), "expired-negative-dimensions-vector.json")
	writeVectorStoreJSON(t, path, &store)

	loaded, err := Load(path)
	require.NoError(t, err)
	assert.Equal(t, 0, loaded.Dimensions)
	assert.True(t, loaded.Vectorizer.IsZero())
	assert.Empty(t, loaded.Documents)
}

func TestLoadCompactsExpiredDuplicateVectorBeforeValidation(t *testing.T) {
	t.Parallel()

	vectorizer, err := NewTextVectorizer(8)
	require.NoError(t, err)

	store, err := NewStoreWithVectorizer(vectorizer.Spec())
	require.NoError(t, err)

	vec, err := vectorizer.Vectorize("active duplicate vector memory")
	require.NoError(t, err)
	require.NoError(t, store.Add(Document{
		ID:         duplicateID,
		Text:       "active duplicate vector memory",
		Vector:     vec,
		Vectorizer: vectorizer.Spec(),
	}))

	expiredAt := time.Now().UTC().Add(-time.Second)
	store.Documents = append(store.Documents, Document{
		ID:        duplicateID,
		Text:      "expired duplicate vector memory",
		Vector:    Vector{1, 0},
		ExpiresAt: &expiredAt,
	})

	path := filepath.Join(t.TempDir(), "expired-duplicate-vector.json")
	writeVectorStoreJSON(t, path, store)

	loaded, err := Load(path)
	require.NoError(t, err)
	require.Len(t, loaded.Documents, 1)
	assert.Equal(t, "active duplicate vector memory", loaded.Documents[0].Text)
}

func TestLoadRejectsMissingSourceHashUnlessMigrated(t *testing.T) {
	t.Parallel()

	spec := TextVectorizerSpec(2)
	store, err := NewStoreWithVectorizer(spec)
	require.NoError(t, err)
	require.NoError(t, store.Add(Document{ID: "doc", Text: "memory", Vector: Vector{1, 0}, Vectorizer: spec}))
	store.Documents[0].SourceHash = ""

	path := filepath.Join(t.TempDir(), "missing-source-hash.json")
	writeVectorStoreJSON(t, path, store)

	_, err = Load(path)
	require.ErrorIs(t, err, ErrSourceHashMismatch)

	vectorizer, err := NewTextVectorizer(2)
	require.NoError(t, err)

	migrated, err := LoadWithOptions(path, LoadOptions{
		Migrate:    true,
		Vectorizer: vectorizer.Spec(),
		Vectorize: func(doc Document) (Vector, error) {
			return vectorizer.Vectorize(doc.Text)
		},
	})
	require.NoError(t, err)
	require.Len(t, migrated.Documents, 1)
	assert.Equal(t, sourceHash("memory"), migrated.Documents[0].SourceHash)
}

func TestLoadRejectsMalformedVectorOnlySourceHash(t *testing.T) {
	t.Parallel()

	spec := TextVectorizerSpec(2)
	store, err := NewStoreWithVectorizer(spec)
	require.NoError(t, err)

	store.Documents = append(store.Documents, Document{
		ID:         "vector-only",
		SourceHash: "not-a-source-hash",
		Vector:     Vector{1, 0},
		Vectorizer: spec,
		Provenance: map[string]string{"source_type": "external"},
	})

	path := filepath.Join(t.TempDir(), "malformed-source-hash.json")
	writeVectorStoreJSON(t, path, store)

	_, err = Load(path)
	require.ErrorIs(t, err, ErrSourceHashMismatch)
}

func TestLoadWithOptionsMigratesStaleVectorizer(t *testing.T) {
	t.Parallel()

	oldSpec := TextVectorizerSpec(2)
	store, err := NewStoreWithVectorizer(oldSpec)
	require.NoError(t, err)
	require.NoError(t, store.Add(Document{
		ID:         "doc",
		Text:       "token refresh memory",
		Vector:     textHashVector(t, oldSpec, "token refresh memory"),
		Vectorizer: oldSpec,
	}))
	store.Documents[0].Vectorizer.Model = staleHashModel

	path := filepath.Join(t.TempDir(), "stale.json")
	writeVectorStoreJSON(t, path, store)

	_, err = Load(path)
	require.ErrorIs(t, err, ErrVectorizerMismatch)

	vectorizer, err := NewTextVectorizer(8)
	require.NoError(t, err)

	migrated, err := LoadWithOptions(path, LoadOptions{
		Migrate:    true,
		Vectorizer: vectorizer.Spec(),
		Vectorize: func(doc Document) (Vector, error) {
			return vectorizer.Vectorize(doc.Text)
		},
	})
	require.NoError(t, err)
	assert.True(t, migrated.Vectorizer.CompatibleWith(vectorizer.Spec()))
	require.Len(t, migrated.Documents, 1)
	assert.Len(t, migrated.Documents[0].Vector, 8)
	assert.True(t, migrated.Documents[0].Vectorizer.CompatibleWith(vectorizer.Spec()))

	query, err := vectorizer.Vectorize("refresh")
	require.NoError(t, err)
	results, err := migrated.SearchWithVectorizer(query, vectorizer.Spec(), 1)
	require.NoError(t, err)
	require.Len(t, results, 1)
	assert.Equal(t, "doc", results[0].Document.ID)
}

func TestLoadWithOptionsMigratesByRedactingBeforeVectorize(t *testing.T) {
	t.Parallel()

	spec := TextVectorizerSpec(2)
	store := Store{
		SchemaVersion: StoreSchemaVersion,
		Dimensions:    2,
		Vectorizer:    spec,
		Documents: []Document{{
			ID:         "secret",
			Text:       "safe memory password=hunter2",
			SourceHash: sourceHash("safe memory password=hunter2"),
			Vector:     Vector{1, 0},
			Vectorizer: spec,
			Metadata:   map[string]string{"api_key": "abc123"},
		}},
	}

	path := filepath.Join(t.TempDir(), "secret.json")
	writeVectorStoreJSON(t, path, &store)

	_, err := Load(path)
	require.ErrorIs(t, err, ErrPrivacyPolicy)

	vectorizer, err := NewTextVectorizer(4)
	require.NoError(t, err)

	migrated, err := LoadWithOptions(path, LoadOptions{
		Migrate:    true,
		Vectorizer: vectorizer.Spec(),
		Vectorize: func(doc Document) (Vector, error) {
			assert.NotContains(t, doc.Text, "hunter2")
			assert.Equal(t, "[REDACTED]", doc.Metadata["api_key"])

			return vectorizer.Vectorize(doc.Text)
		},
	})
	require.NoError(t, err)
	require.Len(t, migrated.Documents, 1)
	assert.NotContains(t, migrated.Documents[0].Text, "hunter2")
	assert.Equal(t, "[REDACTED]", migrated.Documents[0].Metadata["api_key"])
	assert.True(t, migrated.Documents[0].Vectorizer.CompatibleWith(vectorizer.Spec()))
}

func TestLoadRejectsMissingProvenanceUnlessMigrated(t *testing.T) {
	t.Parallel()

	spec := TextVectorizerSpec(2)
	store := Store{
		SchemaVersion: StoreSchemaVersion,
		Dimensions:    2,
		Vectorizer:    spec,
		Documents: []Document{{
			ID:         "doc",
			Text:       "safe vector memory",
			SourceHash: sourceHash("safe vector memory"),
			Vector:     Vector{1, 0},
			Vectorizer: spec,
		}},
	}

	path := filepath.Join(t.TempDir(), "missing-provenance.json")
	writeVectorStoreJSON(t, path, &store)

	_, err := Load(path)
	require.ErrorIs(t, err, ErrProvenanceMissing)

	vectorizer, err := NewTextVectorizer(4)
	require.NoError(t, err)

	migrated, err := LoadWithOptions(path, LoadOptions{
		Migrate:    true,
		Vectorizer: vectorizer.Spec(),
		Vectorize: func(doc Document) (Vector, error) {
			assert.Equal(t, "legacy", doc.Provenance["source_type"])

			return vectorizer.Vectorize(doc.Text)
		},
	})
	require.NoError(t, err)
	require.Len(t, migrated.Documents, 1)
	assert.Equal(t, "legacy", migrated.Documents[0].Provenance["source_type"])
}

func TestLoadRejectsMissingPrivacyPolicyUnlessMigrated(t *testing.T) {
	t.Parallel()

	text := "safe vector memory"
	vectorizer, err := NewTextVectorizer(4)
	require.NoError(t, err)
	vec, err := vectorizer.Vectorize(text)
	require.NoError(t, err)

	store, err := NewStoreWithVectorizer(vectorizer.Spec())
	require.NoError(t, err)
	require.NoError(t, store.Add(Document{
		ID:         "safe",
		Text:       text,
		Vector:     vec,
		Vectorizer: vectorizer.Spec(),
	}))

	delete(store.Documents[0].Provenance, "privacy_policy")

	path := filepath.Join(t.TempDir(), "missing-privacy-policy.json")
	writeVectorStoreJSON(t, path, store)

	_, err = Load(path)
	require.ErrorIs(t, err, ErrPrivacyPolicy)

	migrated, err := LoadWithOptions(path, LoadOptions{
		Migrate:    true,
		Vectorizer: vectorizer.Spec(),
		Vectorize: func(doc Document) (Vector, error) {
			return vectorizer.Vectorize(doc.Text)
		},
	})
	require.NoError(t, err)

	assert.Equal(t, privacy.RedactionPolicyVersion, migrated.Documents[0].Provenance["privacy_policy"])
}

func TestStore_RebuildPreservesExistingProvenance(t *testing.T) {
	t.Parallel()

	oldSpec := TextVectorizerSpec(2)
	store := Store{
		Documents: []Document{{
			ID:         "file-doc",
			Text:       "safe file-backed vector memory",
			Vector:     Vector{1, 0},
			Vectorizer: oldSpec,
			SourceHash: sourceHash("safe file-backed vector memory"),
			Provenance: map[string]string{
				"source_type": "file",
				"path":        "docs/vector-note.md",
				"api_key":     "abc123",
			},
		}},
		Vectorizer: oldSpec,
		Dimensions: 2,
	}

	vectorizer, err := NewTextVectorizer(4)
	require.NoError(t, err)

	wantRedacted := privacy.RedactMetadata(map[string]string{"api_key": "abc123"})["api_key"]
	err = store.Rebuild(vectorizer.Spec(), func(doc Document) (Vector, error) {
		assert.Equal(t, "file", doc.Provenance["source_type"])
		assert.Equal(t, wantRedacted, doc.Provenance["api_key"])

		return vectorizer.Vectorize(doc.Text)
	})
	require.NoError(t, err)

	require.Len(t, store.Documents, 1)
	assert.Equal(t, "file", store.Documents[0].Provenance["source_type"])
	assert.Equal(t, wantRedacted, store.Documents[0].Provenance["api_key"])
	assert.True(t, store.Documents[0].Vectorizer.CompatibleWith(vectorizer.Spec()))
}

func TestLoadRejectsUnredactedPersistedVectorText(t *testing.T) {
	t.Parallel()

	spec := TextVectorizerSpec(2)
	store := Store{
		SchemaVersion: StoreSchemaVersion,
		Dimensions:    2,
		Vectorizer:    spec,
		Documents: []Document{{
			ID:         "secret",
			Text:       "password=hunter2",
			SourceHash: sourceHash("password=hunter2"),
			Vector:     Vector{1, 0},
			Vectorizer: spec,
		}},
	}

	path := filepath.Join(t.TempDir(), "secret.json")
	writeVectorStoreJSON(t, path, &store)

	_, err := Load(path)
	require.ErrorIs(t, err, ErrPrivacyPolicy)
}

func TestStore_RejectsIncompatibleSchemaVersion(t *testing.T) {
	t.Parallel()

	spec := TextVectorizerSpec(2)
	store, err := NewStoreWithVectorizer(spec)
	require.NoError(t, err)
	require.NoError(t, store.Add(Document{ID: "doc", SourceHash: sourceHash("doc"), Vector: Vector{1, 0}, Vectorizer: spec}))

	store.SchemaVersion = StoreSchemaVersion + 1
	_, err = store.SearchWithVectorizer(Vector{1, 0}, spec, 1)
	require.ErrorIs(t, err, ErrIncompatibleSchema)

	err = store.Add(Document{ID: "new", SourceHash: sourceHash("new"), Vector: Vector{1, 0}, Vectorizer: spec})
	require.ErrorIs(t, err, ErrIncompatibleSchema)
}

func TestStore_RebuildRejectsMalformedSchemaVersion(t *testing.T) {
	t.Parallel()

	spec := TextVectorizerSpec(2)
	store, err := NewStoreWithVectorizer(spec)
	require.NoError(t, err)
	require.NoError(t, store.Add(Document{
		ID:         "doc",
		Text:       "stable vector text",
		Vector:     textHashVector(t, spec, "stable vector text"),
		Vectorizer: spec,
	}))

	store.SchemaVersion = -1

	err = store.Rebuild(spec, func(doc Document) (Vector, error) {
		return textHashVector(t, spec, doc.Text), nil
	})
	require.ErrorIs(t, err, ErrIncompatibleSchema)
}

func TestStore_RebuildMigratesVectorizerAndDimensions(t *testing.T) {
	t.Parallel()

	oldSpec := TextVectorizerSpec(2)
	store, err := NewStoreWithVectorizer(oldSpec)
	require.NoError(t, err)
	require.NoError(t, store.Add(Document{
		ID:         "auth",
		Text:       "token refresh retry",
		Vector:     textHashVector(t, oldSpec, "token refresh retry"),
		Vectorizer: oldSpec,
	}))

	newVectorizer, err := NewTextVectorizer(8)
	require.NoError(t, err)

	newSpec := newVectorizer.Spec()
	query, err := newVectorizer.Vectorize("refresh retry")
	require.NoError(t, err)

	_, err = store.SearchWithVectorizer(query, newSpec, 1)
	require.ErrorIs(t, err, ErrDimensionMismatch)

	err = store.Rebuild(newSpec, func(doc Document) (Vector, error) {
		return newVectorizer.Vectorize(doc.Text)
	})
	require.NoError(t, err)

	assert.Equal(t, 8, store.Dimensions)
	assert.True(t, store.Vectorizer.CompatibleWith(newSpec))
	require.Len(t, store.Documents, 1)
	assert.True(t, store.Documents[0].Vectorizer.CompatibleWith(newSpec))
	assert.Len(t, store.Documents[0].Vector, 8)

	results, err := store.SearchWithVectorizer(query, newSpec, 1)
	require.NoError(t, err)
	require.Len(t, results, 1)
	assert.Equal(t, "auth", results[0].Document.ID)
}

func TestStore_RebuildRedactsBeforeVectorizing(t *testing.T) {
	t.Parallel()

	oldSpec := TextVectorizerSpec(2)
	store, err := NewStoreWithVectorizer(oldSpec)
	require.NoError(t, err)
	require.NoError(t, store.Add(Document{
		ID:         "secret",
		Text:       "safe memory",
		Vector:     textHashVector(t, oldSpec, "safe memory"),
		Vectorizer: oldSpec,
	}))
	store.Documents[0].Text = "safe memory password=hunter2"
	store.Documents[0].Metadata = map[string]string{"api_key": "abc123"}
	store.Documents[0].SourceHash = sourceHash("safe memory password=hunter2")

	newVectorizer, err := NewTextVectorizer(4)
	require.NoError(t, err)

	err = store.Rebuild(newVectorizer.Spec(), func(doc Document) (Vector, error) {
		assert.NotContains(t, doc.Text, "hunter2")
		assert.Equal(t, "[REDACTED]", doc.Metadata["api_key"])
		assert.Equal(t, sourceHash("safe memory password=[REDACTED]"), doc.SourceHash)
		assert.Empty(t, doc.Vector)
		assert.True(t, doc.Vectorizer.IsZero())

		return newVectorizer.Vectorize(doc.Text)
	})
	require.NoError(t, err)

	require.Len(t, store.Documents, 1)
	assert.NotContains(t, store.Documents[0].Text, "hunter2")
	assert.Equal(t, "[REDACTED]", store.Documents[0].Metadata["api_key"])
	assert.Len(t, store.Documents[0].Vector, 4)
}

func TestStore_RebuildRejectsInvalidUTF8BeforeVectorizing(t *testing.T) {
	t.Parallel()

	oldSpec := TextVectorizerSpec(2)
	store, err := NewStoreWithVectorizer(oldSpec)
	require.NoError(t, err)
	require.NoError(t, store.Add(Document{
		ID:         "invalid",
		Text:       "safe memory",
		Vector:     textHashVector(t, oldSpec, "safe memory"),
		Vectorizer: oldSpec,
	}))

	invalidText := string([]byte{0xff})
	store.Documents[0].Text = invalidText
	store.Documents[0].SourceHash = sourceHash(invalidText)

	newVectorizer, err := NewTextVectorizer(4)
	require.NoError(t, err)

	err = store.Rebuild(newVectorizer.Spec(), func(Document) (Vector, error) {
		t.Fatal("vectorize callback should not be called for invalid UTF-8 text")

		return nil, nil
	})
	require.ErrorIs(t, err, ErrInvalidUTF8)
}

func TestStore_RebuildSkipsExpiredDocuments(t *testing.T) {
	t.Parallel()

	oldSpec := TextVectorizerSpec(2)
	store, err := NewStoreWithVectorizer(oldSpec)
	require.NoError(t, err)
	require.NoError(t, store.Add(Document{
		ID:         "active",
		Text:       "active vector memory",
		Vector:     textHashVector(t, oldSpec, "active vector memory"),
		Vectorizer: oldSpec,
	}))

	expiredAt := time.Now().UTC().Add(-time.Second)
	require.NoError(t, store.Add(Document{
		ID:         "expired",
		Text:       "expired vector memory",
		Vector:     textHashVector(t, oldSpec, "expired vector memory"),
		Vectorizer: oldSpec,
		ExpiresAt:  &expiredAt,
	}))

	newVectorizer, err := NewTextVectorizer(4)
	require.NoError(t, err)

	err = store.Rebuild(newVectorizer.Spec(), func(doc Document) (Vector, error) {
		assert.NotEqual(t, "expired", doc.ID)

		return newVectorizer.Vectorize(doc.Text)
	})
	require.NoError(t, err)

	require.Len(t, store.Documents, 1)
	assert.Equal(t, "active", store.Documents[0].ID)
}

func TestStore_RebuildClearsDimensionlessVectorizerWhenNoDocuments(t *testing.T) {
	t.Parallel()

	store, err := NewStore(0)
	require.NoError(t, err)

	dimensionlessSpec := VectorizerSpec{
		ID:            "ollama-compatible-embedding",
		Model:         "nomic-embed-text",
		Normalization: "trim-space-v1",
		Version:       vectorizerSpecVersion,
	}

	called := false
	err = store.Rebuild(dimensionlessSpec, func(Document) (Vector, error) {
		called = true

		return Vector{1, 0}, nil
	})
	require.NoError(t, err)
	assert.False(t, called)
	assert.True(t, store.Vectorizer.IsZero())

	results, err := store.SearchWithVectorizer(Vector{1, 0}, TextVectorizerSpec(2), 1)
	require.NoError(t, err)
	assert.Empty(t, results)
}

func TestStore_RebuildClearsCompleteVectorizerWhenNoDocuments(t *testing.T) {
	t.Parallel()

	store, err := NewStoreWithVectorizer(TextVectorizerSpec(2))
	require.NoError(t, err)

	err = store.Rebuild(TextVectorizerSpec(2), func(Document) (Vector, error) {
		return Vector{1, 0}, nil
	})
	require.NoError(t, err)
	assert.True(t, store.Vectorizer.IsZero())
	assert.Equal(t, 0, store.Dimensions)
}

func TestStore_RebuildRejectsVectorOnlyDocumentWithoutSourceText(t *testing.T) {
	t.Parallel()

	oldSpec := TextVectorizerSpec(2)
	store := &Store{
		SchemaVersion: StoreSchemaVersion,
		Dimensions:    2,
		Vectorizer:    oldSpec,
		Documents: []Document{{
			ID:         "vector-only",
			SourceHash: sourceHash("redacted source elsewhere"),
			Vector:     Vector{1, 0},
			Vectorizer: oldSpec,
			Provenance: map[string]string{"source_type": "external"},
		}},
	}

	called := false
	err := store.Rebuild(TextVectorizerSpec(4), func(Document) (Vector, error) {
		called = true

		return Vector{1, 0, 0, 0}, nil
	})
	require.ErrorIs(t, err, ErrEmptyText)
	assert.False(t, called)
}

func TestStore_RefusesStaleTextHashVectorUntilRebuilt(t *testing.T) {
	t.Parallel()

	vectorizer, err := NewTextVectorizer(32)
	require.NoError(t, err)

	store, err := NewStoreWithVectorizer(vectorizer.Spec())
	require.NoError(t, err)

	currentText := "OAuth token refresh retries"
	currentVector, err := vectorizer.Vectorize(currentText)
	require.NoError(t, err)
	staleVector, err := vectorizer.Vectorize("documentation screenshots changelog")
	require.NoError(t, err)
	require.False(t, vectorsEqual(currentVector, staleVector))

	err = store.Add(Document{
		ID:         "stale",
		Text:       currentText,
		Vector:     staleVector,
		Vectorizer: vectorizer.Spec(),
	})
	require.ErrorIs(t, err, ErrVectorMismatch)

	require.NoError(t, store.Add(Document{
		ID:         "auth",
		Text:       currentText,
		Vector:     currentVector,
		Vectorizer: vectorizer.Spec(),
	}))
	store.Documents[0].Vector = staleVector

	_, err = store.SearchWithVectorizer(currentVector, vectorizer.Spec(), 1)
	require.ErrorIs(t, err, ErrVectorMismatch)

	path := filepath.Join(t.TempDir(), "stale-vector.json")
	writeVectorStoreJSON(t, path, store)

	_, err = Load(path)
	require.ErrorIs(t, err, ErrVectorMismatch)

	err = store.Save(path)
	require.ErrorIs(t, err, ErrVectorMismatch)

	migrated, err := LoadWithOptions(path, LoadOptions{
		Migrate:    true,
		Vectorizer: vectorizer.Spec(),
		Vectorize: func(doc Document) (Vector, error) {
			return vectorizer.Vectorize(doc.Text)
		},
	})
	require.NoError(t, err)

	results, err := migrated.SearchWithVectorizer(currentVector, vectorizer.Spec(), 1)
	require.NoError(t, err)
	require.Len(t, results, 1)
	assert.Equal(t, "auth", results[0].Document.ID)
}

func TestStore_RebuildRejectsStaleTextHashVectorCallback(t *testing.T) {
	t.Parallel()

	vectorizer, err := NewTextVectorizer(32)
	require.NoError(t, err)

	store, err := NewStoreWithVectorizer(vectorizer.Spec())
	require.NoError(t, err)

	currentText := "OAuth token refresh retries"
	currentVector, err := vectorizer.Vectorize(currentText)
	require.NoError(t, err)
	staleVector, err := vectorizer.Vectorize("documentation screenshots changelog")
	require.NoError(t, err)
	require.False(t, vectorsEqual(currentVector, staleVector))

	require.NoError(t, store.Add(Document{
		ID:         "auth",
		Text:       currentText,
		Vector:     currentVector,
		Vectorizer: vectorizer.Spec(),
	}))

	err = store.Rebuild(vectorizer.Spec(), func(Document) (Vector, error) {
		return staleVector, nil
	})
	require.ErrorIs(t, err, ErrVectorMismatch)
}

func TestStore_SearchRejectsSourceHashMismatch(t *testing.T) {
	t.Parallel()

	spec := TextVectorizerSpec(2)
	store, err := NewStoreWithVectorizer(spec)
	require.NoError(t, err)
	require.NoError(t, store.Add(Document{
		ID:         "doc",
		Text:       "original text",
		Vector:     textHashVector(t, spec, "original text"),
		Vectorizer: spec,
	}))
	require.NotEmpty(t, store.Documents[0].SourceHash)

	store.Documents[0].Text = "tampered text"

	_, err = store.SearchWithVectorizer(Vector{1, 0}, spec, 1)
	require.ErrorIs(t, err, ErrSourceHashMismatch)
}

func TestStore_SearchRejectsMissingSourceHashForTextDocument(t *testing.T) {
	t.Parallel()

	spec := TextVectorizerSpec(2)
	store, err := NewStoreWithVectorizer(spec)
	require.NoError(t, err)
	require.NoError(t, store.Add(Document{
		ID:         "doc",
		Text:       "original text",
		Vector:     textHashVector(t, spec, "original text"),
		Vectorizer: spec,
	}))
	require.NotEmpty(t, store.Documents[0].SourceHash)

	store.Documents[0].SourceHash = ""

	_, err = store.SearchWithVectorizer(Vector{1, 0}, spec, 1)
	require.ErrorIs(t, err, ErrSourceHashMismatch)
}

func TestStore_SearchRejectsMissingSourceHashForVectorOnlyDocument(t *testing.T) {
	t.Parallel()

	spec := TextVectorizerSpec(2)
	store, err := NewStoreWithVectorizer(spec)
	require.NoError(t, err)
	require.NoError(t, store.Add(Document{ID: "doc", SourceHash: sourceHash("doc"), Vector: Vector{1, 0}, Vectorizer: spec}))
	store.Documents[0].SourceHash = ""

	_, err = store.SearchWithVectorizer(Vector{1, 0}, spec, 1)
	require.ErrorIs(t, err, ErrSourceHashMismatch)
}

func TestStore_SearchRejectsMissingProvenance(t *testing.T) {
	t.Parallel()

	store, err := NewStore(2)
	require.NoError(t, err)
	require.NoError(t, store.Add(Document{ID: "doc", Text: "safe vector memory", Vector: Vector{1, 0}}))
	store.Documents[0].Provenance = nil

	_, err = store.Search(Vector{1, 0}, 1)
	require.ErrorIs(t, err, ErrProvenanceMissing)
}

func TestStore_SaveRejectsMissingProvenance(t *testing.T) {
	t.Parallel()

	vectorizer, err := NewTextVectorizer(8)
	require.NoError(t, err)

	store, err := NewStoreWithVectorizer(vectorizer.Spec())
	require.NoError(t, err)

	vec, err := vectorizer.Vectorize("safe persistent vector memory")
	require.NoError(t, err)
	require.NoError(t, store.Add(Document{
		ID:         "safe",
		Text:       "safe persistent vector memory",
		Vector:     vec,
		Vectorizer: vectorizer.Spec(),
	}))

	store.Documents[0].Provenance = nil

	path := filepath.Join(t.TempDir(), "vectors.json")
	err = store.Save(path)
	require.ErrorIs(t, err, ErrProvenanceMissing)
}

func TestStore_AddRejectsVectorOnlyDocumentWithoutSourceHash(t *testing.T) {
	t.Parallel()

	store, err := NewStore(0)
	require.NoError(t, err)

	err = store.Add(Document{ID: "doc", Vector: Vector{1, 0}})
	require.ErrorIs(t, err, ErrSourceHashMismatch)

	err = store.Add(Document{ID: "doc", SourceHash: "not-a-source-hash", Vector: Vector{1, 0}})
	require.ErrorIs(t, err, ErrSourceHashMismatch)

	assert.Empty(t, store.Documents)
	assert.Equal(t, 0, store.Dimensions)
	assert.True(t, store.Vectorizer.IsZero())
}

func TestStore_AddRejectsInvalidUTF8Text(t *testing.T) {
	t.Parallel()

	store, err := NewStore(2)
	require.NoError(t, err)

	err = store.Add(Document{
		ID:     "invalid",
		Text:   string([]byte{0xff}),
		Vector: Vector{1, 0},
	})
	require.ErrorIs(t, err, ErrInvalidUTF8)
	assert.Empty(t, store.Documents)
}

func TestStore_RejectsUnredactedTextBeforePersistence(t *testing.T) {
	t.Parallel()

	store, err := NewStore(2)
	require.NoError(t, err)

	err = store.Add(Document{
		ID:         "raw-secret",
		Text:       "deploy password=hunter2 with api_key=abc123",
		Vector:     Vector{1, 0},
		Metadata:   map[string]string{"auth_token": "abc123", "kind": "note"},
		Provenance: map[string]string{"source": "Authorization: Bearer abc123"},
	})
	require.ErrorIs(t, err, ErrPrivacyPolicy)
	assert.Empty(t, store.Documents)

	redacted := privacy.RedactText("deploy password=hunter2 with api_key=abc123")
	require.NoError(t, store.Add(Document{
		ID:         "secret",
		Text:       redacted,
		Vector:     Vector{1, 0},
		Metadata:   map[string]string{"auth_token": "abc123", "kind": "note"},
		Provenance: map[string]string{"source": "Authorization: Bearer abc123"},
	}))

	require.Len(t, store.Documents, 1)
	doc := store.Documents[0]
	assert.Equal(t, redacted, doc.Text)
	assert.NotContains(t, doc.Text, "hunter2")
	assert.NotContains(t, doc.Text, "abc123")
	assert.Equal(t, "[REDACTED]", doc.Metadata["auth_token"])
	assert.NotContains(t, doc.Provenance["source"], "abc123")
	assert.Equal(t, sourceHash(doc.Text), doc.SourceHash)
}

func TestStore_SearchRejectsUnredactedTamperedDocument(t *testing.T) {
	t.Parallel()

	store, err := NewStore(2)
	require.NoError(t, err)
	require.NoError(t, store.Add(Document{ID: "safe", Text: "safe vector memory", Vector: Vector{1, 0}}))

	store.Documents[0].Text = "safe vector memory password=hunter2"
	store.Documents[0].SourceHash = sourceHash(store.Documents[0].Text)

	_, err = store.Search(Vector{1, 0}, 1)
	require.ErrorIs(t, err, ErrPrivacyPolicy)
}

func TestStore_RebuildRequiresVectorizer(t *testing.T) {
	t.Parallel()

	store, err := NewStore(2)
	require.NoError(t, err)

	err = store.Rebuild(VectorizerSpec{}, func(Document) (Vector, error) {
		return Vector{1, 0}, nil
	})
	require.ErrorIs(t, err, ErrVectorizerMismatch)

	err = store.Rebuild(TextVectorizerSpec(2), nil)
	require.ErrorIs(t, err, ErrVectorizerRequired)
}

func TestStore_DeleteAndCompactExpiredDocuments(t *testing.T) {
	t.Parallel()

	store, err := NewStore(2)
	require.NoError(t, err)

	expired := time.Now().UTC().Add(-time.Second)
	require.NoError(t, store.Add(Document{ID: "expired", Text: "temporary vector memory", Vector: Vector{1, 0}, ExpiresAt: &expired}))
	require.NoError(t, store.Add(Document{ID: "active", Text: "durable vector memory", Vector: Vector{1, 0}}))

	results, err := store.Search(Vector{1, 0}, 0)
	require.NoError(t, err)
	assert.Equal(t, []string{"active"}, resultIDs(results))

	assert.Equal(t, 1, store.Compact(expired.Add(time.Second)))
	assertStoreBackingOmitsText(t, store, "temporary vector memory")
	assert.False(t, store.Delete("expired"))
	assert.True(t, store.Delete("active"))
	assert.Empty(t, store.Documents)
	assertStoreBackingOmitsText(t, store, "durable vector memory")

	data, err := json.Marshal(store)
	require.NoError(t, err)
	assert.NotContains(t, string(data), "temporary vector memory")
	assert.NotContains(t, string(data), "durable vector memory")
}

func TestStore_DeleteLastVectorizedDocumentClearsPinnedMetadata(t *testing.T) {
	t.Parallel()

	vectorizer, err := NewTextVectorizer(2)
	require.NoError(t, err)

	store, err := NewStoreWithVectorizer(vectorizer.Spec())
	require.NoError(t, err)

	vec, err := vectorizer.Vectorize("obsolete vector memory")
	require.NoError(t, err)
	require.NoError(t, store.Add(Document{
		ID:         "obsolete",
		Text:       "obsolete vector memory",
		Vector:     vec,
		Vectorizer: vectorizer.Spec(),
	}))

	require.True(t, store.Delete("obsolete"))
	assert.True(t, store.Vectorizer.IsZero())
	assert.Equal(t, 0, store.Dimensions)

	require.NoError(t, store.Add(Document{
		ID:         "replacement",
		SourceHash: sourceHash("replacement"),
		Vector:     Vector{1, 0, 0},
		Vectorizer: VectorizerSpec{
			ID:            "ollama-compatible-embedding",
			Model:         "fresh-embed",
			Normalization: "trim-space-v1",
			Version:       vectorizerSpecVersion,
			Dimensions:    3,
		},
	}))
	assert.Equal(t, 3, store.Dimensions)
}

func TestStore_CompactLastExpiredVectorizedDocumentClearsPinnedMetadata(t *testing.T) {
	t.Parallel()

	vectorizer, err := NewTextVectorizer(2)
	require.NoError(t, err)

	store, err := NewStoreWithVectorizer(vectorizer.Spec())
	require.NoError(t, err)

	expiredAt := time.Now().UTC().Add(-time.Second)
	vec, err := vectorizer.Vectorize("expired vector memory")
	require.NoError(t, err)
	require.NoError(t, store.Add(Document{
		ID:         "expired",
		Text:       "expired vector memory",
		Vector:     vec,
		Vectorizer: vectorizer.Spec(),
		ExpiresAt:  &expiredAt,
	}))

	assert.Equal(t, 1, store.Compact(time.Now().UTC()))
	assert.True(t, store.Vectorizer.IsZero())
	assert.Equal(t, 0, store.Dimensions)
}

func TestStore_DeleteRemovesPersistedVectorContent(t *testing.T) {
	t.Parallel()

	vectorizer, err := NewTextVectorizer(8)
	require.NoError(t, err)

	store, err := NewStoreWithVectorizer(vectorizer.Spec())
	require.NoError(t, err)

	deleteVector, err := vectorizer.Vectorize("obsolete vector memory")
	require.NoError(t, err)
	require.NoError(t, store.Add(Document{
		ID:         "delete-me",
		Text:       "obsolete vector memory",
		Vector:     deleteVector,
		Vectorizer: vectorizer.Spec(),
	}))

	keepVector, err := vectorizer.Vectorize("durable vector memory")
	require.NoError(t, err)
	require.NoError(t, store.Add(Document{
		ID:         "keep-me",
		Text:       "durable vector memory",
		Vector:     keepVector,
		Vectorizer: vectorizer.Spec(),
	}))

	assert.True(t, store.Delete("delete-me"))

	path := filepath.Join(t.TempDir(), "vectors.json")
	require.NoError(t, store.Save(path))

	persisted, err := os.ReadFile(path)
	require.NoError(t, err)
	assert.NotContains(t, string(persisted), "delete-me")
	assert.NotContains(t, string(persisted), "obsolete vector memory")
	assert.Contains(t, string(persisted), "keep-me")

	loaded, err := Load(path)
	require.NoError(t, err)
	require.Len(t, loaded.Documents, 1)
	assert.Equal(t, "keep-me", loaded.Documents[0].ID)

	query, err := vectorizer.Vectorize("obsolete")
	require.NoError(t, err)
	results, err := loaded.SearchWithVectorizer(query, vectorizer.Spec(), 10)
	require.NoError(t, err)
	assert.NotContains(t, resultIDs(results), "delete-me")
}

func TestStore_SaveAndLoadRejectDuplicateIDs(t *testing.T) {
	t.Parallel()

	vectorizer, err := NewTextVectorizer(8)
	require.NoError(t, err)

	store, err := NewStoreWithVectorizer(vectorizer.Spec())
	require.NoError(t, err)

	firstVector, err := vectorizer.Vectorize("first duplicate vector memory")
	require.NoError(t, err)
	require.NoError(t, store.Add(Document{
		ID:         duplicateID,
		Text:       "first duplicate vector memory",
		Vector:     firstVector,
		Vectorizer: vectorizer.Spec(),
	}))

	secondVector, err := vectorizer.Vectorize("second duplicate vector memory")
	require.NoError(t, err)
	require.NoError(t, store.Add(Document{
		ID:         "other",
		Text:       "second duplicate vector memory",
		Vector:     secondVector,
		Vectorizer: vectorizer.Spec(),
	}))

	store.Documents[1].ID = duplicateID

	path := filepath.Join(t.TempDir(), "vectors.json")
	err = store.Save(path)
	require.ErrorIs(t, err, ErrDuplicateID)

	writeVectorStoreJSON(t, path, store)

	_, err = Load(path)
	require.ErrorIs(t, err, ErrDuplicateID)
}

func TestStore_DeleteRemovesAllDuplicateIDsBeforePersistence(t *testing.T) {
	t.Parallel()

	vectorizer, err := NewTextVectorizer(8)
	require.NoError(t, err)

	store, err := NewStoreWithVectorizer(vectorizer.Spec())
	require.NoError(t, err)

	firstVector, err := vectorizer.Vectorize("first duplicate vector memory")
	require.NoError(t, err)
	require.NoError(t, store.Add(Document{
		ID:         duplicateID,
		Text:       "first duplicate vector memory",
		Vector:     firstVector,
		Vectorizer: vectorizer.Spec(),
	}))

	secondVector, err := vectorizer.Vectorize("second duplicate vector memory")
	require.NoError(t, err)
	require.NoError(t, store.Add(Document{
		ID:         "other",
		Text:       "second duplicate vector memory",
		Vector:     secondVector,
		Vectorizer: vectorizer.Spec(),
	}))

	store.Documents[1].ID = duplicateID

	assert.True(t, store.Delete(duplicateID))
	assert.Empty(t, store.Documents)

	path := filepath.Join(t.TempDir(), "vectors.json")
	require.NoError(t, store.Save(path))

	persisted, err := os.ReadFile(path)
	require.NoError(t, err)
	assert.NotContains(t, string(persisted), "first duplicate vector memory")
	assert.NotContains(t, string(persisted), "second duplicate vector memory")
}

func TestStore_DeleteClearsBackingCapacity(t *testing.T) {
	t.Parallel()

	store, err := NewStore(2)
	require.NoError(t, err)

	docs := []Document{
		{ID: "delete-me", Text: "visible deleted vector memory"},
		{ID: "ghost", Text: "hidden stale vector memory"},
	}
	store.Documents = docs[:1]

	assert.True(t, store.Delete("delete-me"))
	assertStoreBackingOmitsText(t, store, "vector memory")
}

func TestTextVectorizer_Stability(t *testing.T) {
	t.Parallel()

	vectorizer, err := NewTextVectorizer(16)
	if err != nil {
		t.Fatalf("NewTextVectorizer() error = %v", err)
	}

	first, err := vectorizer.Vectorize("ADR: Prefer local retrieval over remote indexing.")
	if err != nil {
		t.Fatalf("Vectorize(first) error = %v", err)
	}

	second, err := vectorizer.Vectorize("adr prefer local retrieval over remote indexing")
	if err != nil {
		t.Fatalf("Vectorize(second) error = %v", err)
	}

	if !reflect.DeepEqual(first, second) {
		t.Fatalf("Vectorize() = %v and %v, want stable lexical vector", first, second)
	}

	changed, err := vectorizer.Vectorize("ADR: Prefer encrypted archival notes.")
	if err != nil {
		t.Fatalf("Vectorize(changed) error = %v", err)
	}

	if reflect.DeepEqual(first, changed) {
		t.Fatalf("Vectorize(changed) = %v, want different vector for different text", changed)
	}
}

func TestTextVectorizer_RetrievalQualityRegression(t *testing.T) {
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

	vectorizer, err := NewTextVectorizer(128)
	require.NoError(t, err)
	store, err := NewStoreWithVectorizer(vectorizer.Spec())
	require.NoError(t, err)

	for _, doc := range fixture.Documents {
		vec, vecErr := vectorizer.Vectorize(doc.Text)
		require.NoError(t, vecErr)
		require.NoError(t, store.Add(Document{ID: doc.ID, Text: doc.Text, Vector: vec, Vectorizer: vectorizer.Spec()}))
	}

	for _, tc := range fixture.Cases {
		query, queryErr := vectorizer.Vectorize(tc.Query)
		require.NoError(t, queryErr)

		results, searchErr := store.SearchWithVectorizer(query, vectorizer.Spec(), 1)
		require.NoError(t, searchErr)

		if assert.Len(t, results, 1) {
			assert.Equal(t, tc.WantTop, results[0].Document.ID)
		}
	}
}

func resultIDs(results []Result) []string {
	ids := make([]string, 0, len(results))
	for i := range results {
		ids = append(ids, results[i].Document.ID)
	}

	return ids
}

func scores(results []Result) []float64 {
	scores := make([]float64, 0, len(results))
	for i := range results {
		scores = append(scores, results[i].Score)
	}

	return scores
}

func assertStoreBackingOmitsText(t *testing.T, store *Store, text string) {
	t.Helper()

	docs := store.Documents[:cap(store.Documents)]
	for i := range docs {
		assert.NotContains(t, docs[i].Text, text)
	}
}

func textHashVector(t *testing.T, spec VectorizerSpec, text string) Vector {
	t.Helper()

	vectorizer, err := NewTextVectorizer(spec.Dimensions)
	require.NoError(t, err)

	vec, err := vectorizer.Vectorize(text)
	require.NoError(t, err)

	return vec
}

func writeVectorStoreJSON(t *testing.T, path string, store *Store) {
	t.Helper()

	data, err := json.Marshal(store)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(path, data, 0o600))
}

// ---------------------------------------------------------------------------
// Vectorizer interface conformance
// ---------------------------------------------------------------------------

func TestTextVectorizer_ImplementsVectorizer(t *testing.T) {
	t.Parallel()

	tv, err := NewTextVectorizer(0)
	require.NoError(t, err)

	var _ Vectorizer = tv
}

func TestEmbeddingVectorizer_ImplementsVectorizer(t *testing.T) {
	t.Parallel()

	var _ Vectorizer = NewEmbeddingVectorizer()

	var _ VectorizerContext = NewEmbeddingVectorizer()
}

// ---------------------------------------------------------------------------
// EmbeddingVectorizer tests
// ---------------------------------------------------------------------------

func TestEmbeddingVectorizer_CallsAPI(t *testing.T) {
	t.Parallel()

	embedding := []float64{0.1, 0.2, 0.3, 0.4}

	client := embeddingTestClient(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, "/api/embed", r.URL.Path)
		assert.Equal(t, "application/json", r.Header.Get("Content-Type"))

		var req ollamaEmbedRequest

		assert.NoError(t, json.NewDecoder(r.Body).Decode(&req))
		assert.Equal(t, "nomic-embed-text", req.Model)
		assert.Equal(t, "hello world", req.Input)

		w.Header().Set("Content-Type", "application/json")

		resp := ollamaEmbedResponse{Embeddings: [][]float64{embedding}}

		assert.NoError(t, json.NewEncoder(w).Encode(resp))
	}))

	v := NewEmbeddingVectorizer(
		WithEmbeddingBaseURL("http://embedding.test"),
		WithEmbeddingHTTPClient(client),
	)

	vec, err := v.VectorizeContext(context.Background(), "hello world")
	require.NoError(t, err)
	assert.Equal(t, Vector(embedding), vec)
}

func TestEmbeddingVectorizer_VectorizeRequiresCallerContext(t *testing.T) {
	t.Parallel()

	v := NewEmbeddingVectorizer()

	_, err := v.Vectorize("hello world")
	require.ErrorIs(t, err, ErrContextRequired)

	var nilCtx context.Context

	_, err = v.VectorizeContext(nilCtx, "hello world")
	require.ErrorIs(t, err, ErrContextRequired)
}

func TestEmbeddingVectorizer_ZeroValueUsesDefaultsWithCallerContext(t *testing.T) {
	t.Parallel()

	var v EmbeddingVectorizer

	spec := v.Spec(384)
	assert.Equal(t, defaultEmbeddingModel, spec.Model)
	assert.Equal(t, 384, spec.Dimensions)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	_, err := v.VectorizeContext(ctx, "hello world")
	require.Error(t, err)
	assert.ErrorIs(t, err, context.Canceled)
}

func TestEmbeddingVectorizer_EmptyTextReturnsError(t *testing.T) {
	t.Parallel()

	v := NewEmbeddingVectorizer()

	_, err := v.Vectorize("  ")
	require.Error(t, err)
	assert.ErrorIs(t, err, ErrEmptyText)
}

func TestEmbeddingVectorizer_CustomModel(t *testing.T) {
	t.Parallel()

	var receivedModel string

	client := embeddingTestClient(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req ollamaEmbedRequest

		assert.NoError(t, json.NewDecoder(r.Body).Decode(&req))

		receivedModel = req.Model

		w.Header().Set("Content-Type", "application/json")
		assert.NoError(t, json.NewEncoder(w).Encode(ollamaEmbedResponse{Embeddings: [][]float64{{1.0}}}))
	}))

	v := NewEmbeddingVectorizer(
		WithEmbeddingBaseURL("http://embedding.test"),
		WithEmbeddingModel("mxbai-embed-large"),
		WithEmbeddingHTTPClient(client),
	)

	_, err := v.VectorizeContext(context.Background(), "test")
	require.NoError(t, err)
	assert.Equal(t, "mxbai-embed-large", receivedModel)
}

func TestEmbeddingVectorizer_SpecRecordsModelDimensionsAndNormalization(t *testing.T) {
	t.Parallel()

	v := NewEmbeddingVectorizer(WithEmbeddingModel("mxbai-embed-large"))

	spec := v.Spec(1024)

	assert.Equal(t, "ollama-compatible-embedding", spec.ID)
	assert.Equal(t, "mxbai-embed-large", spec.Model)
	assert.Equal(t, 1024, spec.Dimensions)
	assert.Equal(t, "trim-space-v1", spec.Normalization)
	assert.Equal(t, vectorizerSpecVersion, spec.Version)
	assert.True(t, spec.CompatibleWith(VectorizerSpec{
		ID:            "ollama-compatible-embedding",
		Model:         "mxbai-embed-large",
		Dimensions:    1024,
		Normalization: "trim-space-v1",
		Version:       vectorizerSpecVersion,
	}))
}

func TestEmbeddingVectorizerSpecRedactsSensitiveModelIdentity(t *testing.T) {
	t.Parallel()

	v := NewEmbeddingVectorizer(WithEmbeddingModel("tenant-embed?api_key=abc123/v1"))

	spec := v.Spec(1024)

	assert.NotContains(t, spec.Model, "abc123")
	assert.Contains(t, spec.Model, "[REDACTED]")
	assert.Contains(t, spec.Model, "/v1")
}

func TestEmbeddingVectorizer_ContextCancellation(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	v := NewEmbeddingVectorizer(WithEmbeddingBaseURL("http://127.0.0.1:1"))

	_, err := v.VectorizeContext(ctx, "test")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "context canceled")
}

func TestEmbeddingVectorizer_PropagatesInFlightCancellation(t *testing.T) {
	t.Parallel()

	started := make(chan struct{})

	client := &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
		close(started)
		<-req.Context().Done()

		return nil, req.Context().Err()
	})}

	v := NewEmbeddingVectorizer(
		WithEmbeddingBaseURL("http://embedding.test"),
		WithEmbeddingHTTPClient(client),
	)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	errCh := make(chan error, 1)

	go func() {
		_, err := v.VectorizeContext(ctx, "cancel in flight")
		errCh <- err
	}()

	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("embedding server was not called")
	}

	cancel()

	select {
	case err := <-errCh:
		require.Error(t, err)
		require.ErrorIs(t, err, context.Canceled)
	case <-time.After(time.Second):
		t.Fatal("VectorizeContext did not return after context cancellation")
	}
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(req *http.Request) (*http.Response, error) {
	return f(req)
}

func embeddingTestClient(handler http.Handler) *http.Client {
	return &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
		recorder := httptest.NewRecorder()
		handler.ServeHTTP(recorder, req)

		return recorder.Result(), nil
	})}
}

func TestEmbeddingVectorizer_ServerError(t *testing.T) {
	t.Parallel()

	client := embeddingTestClient(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))

	v := NewEmbeddingVectorizer(
		WithEmbeddingBaseURL("http://embedding.test"),
		WithEmbeddingHTTPClient(client),
	)

	_, err := v.VectorizeContext(context.Background(), "test")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "unexpected status 500")
}

func TestEmbeddingVectorizer_EmptyEmbeddingsResponse(t *testing.T) {
	t.Parallel()

	client := embeddingTestClient(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		assert.NoError(t, json.NewEncoder(w).Encode(ollamaEmbedResponse{Embeddings: [][]float64{}}))
	}))

	v := NewEmbeddingVectorizer(
		WithEmbeddingBaseURL("http://embedding.test"),
		WithEmbeddingHTTPClient(client),
	)

	_, err := v.VectorizeContext(context.Background(), "test")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "empty response")
}

func TestEmbeddingVectorizer_WithCustomHTTPClient(t *testing.T) {
	t.Parallel()

	customClient := embeddingTestClient(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		assert.NoError(t, json.NewEncoder(w).Encode(ollamaEmbedResponse{Embeddings: [][]float64{{0.5, 0.5}}}))
	}))

	v := NewEmbeddingVectorizer(
		WithEmbeddingBaseURL("http://embedding.test"),
		WithEmbeddingHTTPClient(customClient),
	)

	vec, err := v.VectorizeContext(context.Background(), "test")
	require.NoError(t, err)
	assert.Equal(t, Vector{0.5, 0.5}, vec)
}

func TestNewEmbeddingVectorizer_Defaults(t *testing.T) {
	t.Parallel()

	v := NewEmbeddingVectorizer()
	assert.Equal(t, defaultEmbeddingBaseURL, v.baseURL)
	assert.Equal(t, defaultEmbeddingModel, v.model)
	assert.Equal(t, defaultEmbeddingProvider, v.provider)
	assert.NotNil(t, v.client)
	assert.Equal(t, embeddingTimeout, v.client.Timeout)
}

func TestEmbeddingVectorizer_VectorizeRequiresContext(t *testing.T) {
	t.Parallel()

	v := NewEmbeddingVectorizer()

	_, err := v.Vectorize("hello world")
	require.Error(t, err)
	require.ErrorIs(t, err, ErrContextRequired)

	_, err = v.VectorizeContext(nil, "hello world") //nolint:staticcheck // Verify nil contexts are rejected instead of panicking.
	require.Error(t, err)
	require.ErrorIs(t, err, ErrContextRequired)
}

func TestEmbeddingVectorizer_VectorizeContextHonorsCancellation(t *testing.T) {
	t.Parallel()

	requestStarted := make(chan struct{})
	release := make(chan struct{})

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		close(requestStarted)

		select {
		case <-r.Context().Done():
			return
		case <-release:
			w.Header().Set("Content-Type", "application/json")
			assert.NoError(t, json.NewEncoder(w).Encode(ollamaEmbedResponse{Embeddings: [][]float64{{1.0}}}))
		}
	}))
	defer server.Close()
	defer close(release)

	v := NewEmbeddingVectorizer(WithEmbeddingBaseURL(server.URL))

	ctx, cancel := context.WithCancel(context.Background())
	errCh := make(chan error, 1)

	go func() {
		_, err := v.VectorizeContext(ctx, "hello world")
		errCh <- err
	}()

	<-requestStarted
	cancel()

	err := <-errCh
	require.Error(t, err)
	assert.ErrorIs(t, err, context.Canceled)
}

func TestEmbeddingVectorizer_VectorizeContextRejectsAlreadyCanceledContext(t *testing.T) {
	t.Parallel()

	v := NewEmbeddingVectorizer()

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	_, err := v.VectorizeContext(ctx, "  ")
	require.Error(t, err)
	require.ErrorIs(t, err, context.Canceled)
	require.NotErrorIs(t, err, ErrEmptyText)
}

func TestSearcher_SearchRetrievalRedactsUnsafeSnippets(t *testing.T) {
	t.Parallel()

	vectorizer, err := NewTextVectorizer(16)
	require.NoError(t, err)

	text := privacy.RedactText("oauth callback api_key=super-secret-token")
	vec, err := vectorizer.Vectorize(text)
	require.NoError(t, err)

	store, err := NewStoreWithVectorizer(vectorizer.Spec())
	require.NoError(t, err)
	require.NoError(t, store.Add(Document{ID: "secret", Text: text, Vector: vec, Metadata: map[string]string{"path": ".env"}}))

	results, err := Searcher{Store: store, Vectorizer: vectorizer, Source: retrieval.Source{Type: retrieval.SourceVector, Name: "fixture"}}.SearchRetrieval(context.Background(), retrieval.Query{Text: "oauth callback", IncludeUnsafe: true, Explain: true})
	require.NoError(t, err)
	require.Len(t, results, 1)
	assert.Equal(t, retrieval.SourceVector, results[0].Source.Type)
	assert.False(t, results[0].Safety.InjectAllowed)
	assert.True(t, results[0].Safety.Redacted)
	assert.NotContains(t, results[0].Snippet, "super-secret-token")
	assert.NotEmpty(t, results[0].Metadata[retrieval.MetadataStableID])
	assert.NotEmpty(t, results[0].Metadata[retrieval.MetadataContentHash])
	assert.Equal(t, "hashed-vector-cosine", results[0].Scorer.Name)
	assert.NotEmpty(t, results[0].Scorer.Explanation)
}

func TestSearcher_SearchRetrievalRedactsPreFlaggedRawText(t *testing.T) {
	t.Parallel()

	vectorizer, err := NewTextVectorizer(16)
	require.NoError(t, err)

	text := privacy.RedactText("oauth callback api_key=super-secret-token")
	vec, err := vectorizer.Vectorize(text)
	require.NoError(t, err)

	store, err := NewStoreWithVectorizer(vectorizer.Spec())
	require.NoError(t, err)
	require.NoError(t, store.Add(Document{
		ID:     "secret",
		Text:   text,
		Vector: vec,
		Metadata: map[string]string{
			retrieval.MetadataSafetyInjectAllowed: "false",
			retrieval.MetadataSafetySensitive:     "true",
			"api_key":                             "metadata-secret-token",
		},
	}))

	results, err := Searcher{Store: store, Vectorizer: vectorizer, Source: retrieval.Source{Type: retrieval.SourceVector, Name: "fixture"}}.SearchRetrieval(context.Background(), retrieval.Query{Text: "oauth callback", IncludeUnsafe: true})
	require.NoError(t, err)
	require.Len(t, results, 1)
	assert.False(t, results[0].Safety.InjectAllowed)
	assert.True(t, results[0].Safety.Redacted)
	assert.NotContains(t, results[0].Snippet, "super-secret-token")
	assert.Contains(t, results[0].Snippet, "[REDACTED]")
	assert.Equal(t, "[REDACTED]", results[0].Metadata["api_key"])
	assert.NotContains(t, results[0].Metadata["api_key"], "metadata-secret-token")
	assert.NotContains(t, results[0].Metadata[retrieval.MetadataSafetyReasons], ";;")
}

func TestSearcher_SearchRetrievalOffsetsWorkspaceChunkRange(t *testing.T) {
	t.Parallel()

	vectorizer, err := NewTextVectorizer(16)
	require.NoError(t, err)

	text := "OAuth callback retry near original file offset."
	vec, err := vectorizer.Vectorize(text)
	require.NoError(t, err)

	store, err := NewStoreWithVectorizer(vectorizer.Spec())
	require.NoError(t, err)
	require.NoError(t, store.Add(Document{
		ID:     "docs/auth.md#chunk=0001",
		Text:   text,
		Vector: vec,
		Metadata: map[string]string{
			"path":             "docs/auth.md",
			"chunk_start_rune": "120",
			"chunk_end_rune":   "180",
		},
	}))

	results, err := Searcher{
		Store:      store,
		Vectorizer: vectorizer,
		Source:     retrieval.Source{Type: retrieval.SourceVector, Name: "workspace"},
	}.SearchRetrieval(context.Background(), retrieval.Query{Text: "OAuth retry", IncludeUnsafe: true})
	require.NoError(t, err)
	require.Len(t, results, 1)

	assert.Equal(t, retrieval.RangeUnitRuneOffset, results[0].Chunk.Range.Unit)
	assert.Equal(t, 120, results[0].Chunk.Range.Start)
	assert.Equal(t, 120+len([]rune(text)), results[0].Chunk.Range.End)
	assert.LessOrEqual(t, results[0].Chunk.Range.End, 180)
}

func TestSearcher_SearchRetrievalRejectsNilContext(t *testing.T) {
	t.Parallel()

	_, err := Searcher{}.SearchRetrieval(nil, retrieval.Query{Text: "oauth callback"}) //nolint:staticcheck // Verify nil contexts are rejected instead of panicking.
	require.Error(t, err)
	require.ErrorIs(t, err, ErrContextRequired)
}

func TestEmbeddingVectorizer_WithTimeoutOverridesClientTimeout(t *testing.T) {
	t.Parallel()

	v := NewEmbeddingVectorizer(
		WithEmbeddingHTTPClient(&http.Client{}),
		WithEmbeddingTimeout(2*time.Second),
	)

	require.NotNil(t, v.client)
	assert.Equal(t, 2*time.Second, v.client.Timeout)
}
