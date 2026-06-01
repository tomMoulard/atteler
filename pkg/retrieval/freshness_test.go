package retrieval_test

import (
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"

	"github.com/tommoulard/atteler/pkg/retrieval"
)

func TestFreshnessFromMetadataDoesNotStatSyntheticSourceKinds(t *testing.T) {
	t.Parallel()

	sourceUpdatedAt := time.Date(2026, 6, 1, 10, 30, 0, 0, time.UTC)

	freshness := retrieval.FreshnessFromMetadata(map[string]string{
		retrieval.MetadataSourceUpdatedAt: sourceUpdatedAt.Format(time.RFC3339Nano),
		"path":                            "git/abc123",
		"source_kind":                     "git_history",
	})

	assert.Equal(t, "current", freshness.Status)
	assert.False(t, freshness.Deleted)
	assert.Equal(t, sourceUpdatedAt, freshness.SourceUpdatedAt)
}

func TestFreshnessFromMetadataStillChecksFileSources(t *testing.T) {
	t.Parallel()

	sourceUpdatedAt := time.Date(2026, 6, 1, 10, 30, 0, 0, time.UTC)
	missingPath := filepath.Join(t.TempDir(), "deleted.md")

	freshness := retrieval.FreshnessFromMetadata(map[string]string{
		retrieval.MetadataSourceUpdatedAt: sourceUpdatedAt.Format(time.RFC3339Nano),
		"path":                            missingPath,
		"source_kind":                     "file",
	})

	assert.Equal(t, "deleted", freshness.Status)
	assert.True(t, freshness.Deleted)
}
