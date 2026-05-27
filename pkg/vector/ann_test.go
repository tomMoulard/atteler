package vector

import (
	"fmt"
	"math/rand"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestANNIndexCandidateIndexesProbesUntilMinimumCandidates(t *testing.T) {
	t.Parallel()

	const (
		bucketBits    = 4
		dimensions    = 16
		minCandidates = 4
	)

	docs := documentsWithDistinctANNSignatures(t, bucketBits, dimensions, minCandidates+2)
	ann, err := NewANNIndex(docs, dimensions, ANNOptions{
		BucketBits:          bucketBits,
		Probes:              1,
		MinCandidates:       minCandidates,
		CandidateMultiplier: 1,
	})
	require.NoError(t, err)

	candidates := ann.candidateIndexes(docs[0].Vector, 1)
	assert.GreaterOrEqual(t, len(candidates), minCandidates)
}

func TestANNIndexCandidateIndexesWithoutLimitScansAllDocuments(t *testing.T) {
	t.Parallel()

	const (
		bucketBits = 8
		dimensions = 16
		docCount   = defaultANNMinCandidates + 10
	)

	docs := documentsWithDistinctANNSignatures(t, bucketBits, dimensions, docCount)
	ann, err := NewANNIndex(docs, dimensions, ANNOptions{
		BucketBits:    bucketBits,
		Probes:        1,
		MinCandidates: 1,
	})
	require.NoError(t, err)

	assert.Len(t, ann.candidateIndexes(docs[0].Vector, 0), docCount)
}

func TestANNIndexRejectsUnsafePersistedDocument(t *testing.T) {
	t.Parallel()

	_, err := NewANNIndex([]Document{{
		ID:         "secret",
		Text:       "api_key=super-secret-token",
		SourceHash: sourceHash("api_key=super-secret-token"),
		Vector:     Vector{1, 0},
		Provenance: ensureProvenance(map[string]string{"source_type": "file"}, "file"),
	}}, 2, ANNOptions{})
	require.Error(t, err)
	assert.ErrorIs(t, err, ErrPrivacyPolicy)
}

func TestANNOptionsNormalizeLargeLimitDoesNotOverflow(t *testing.T) {
	t.Parallel()

	got := (ANNOptions{
		BucketBits:          4,
		Probes:              1,
		MinCandidates:       1,
		CandidateMultiplier: 8,
	}).Normalize(100, int(^uint(0)>>1))

	assert.Equal(t, 100, got.MinCandidates)
}

func documentsWithDistinctANNSignatures(t *testing.T, bucketBits, dimensions, count int) []Document {
	t.Helper()

	seen := make(map[uint64]struct{}, count)
	docs := make([]Document, 0, count)

	rng := rand.New(rand.NewSource(1)) //nolint:gosec // Deterministic test fixture, not security-sensitive.

	for range 10_000 {
		vec := make(Vector, dimensions)
		for dim := range dimensions {
			vec[dim] = (rng.Float64() * 2) - 1
		}

		signature := annSignature(vec, bucketBits)
		if _, ok := seen[signature]; ok {
			continue
		}

		seen[signature] = struct{}{}

		id := fmt.Sprintf("doc-%02d", len(docs))
		text := "ann probe fixture"
		docs = append(docs, Document{
			ID:         id,
			Text:       text,
			SourceHash: sourceHash(text),
			Vector:     vec,
			Provenance: ensureProvenance(map[string]string{"source_type": "test"}, "test"),
		})

		if len(docs) == count {
			return docs
		}
	}

	require.Len(t, docs, count)

	return docs
}
