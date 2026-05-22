package retrieval

import (
	"errors"
	"os"
	"strings"
	"time"
)

// FreshnessFromMetadata reports whether a persisted file-backed result still
// reflects the source file revision recorded at index time. Non-file-backed
// results without MetadataSourceUpdatedAt are treated as current because there
// is no external source revision to compare.
func FreshnessFromMetadata(metadata map[string]string) Freshness {
	freshness := Freshness{Status: "current"}

	if value := strings.TrimSpace(metadata[MetadataSourceUpdatedAt]); value != "" {
		if ts, err := time.Parse(time.RFC3339Nano, value); err == nil {
			freshness.SourceUpdatedAt = ts
		}
	}

	if freshness.SourceUpdatedAt.IsZero() {
		return freshness
	}

	path := strings.TrimSpace(metadata["path"])
	if path == "" {
		return freshness
	}

	info, err := os.Stat(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			freshness.Status = "deleted"
			freshness.Deleted = true
		} else {
			freshness.Status = "unknown"
		}

		return freshness
	}

	current := info.ModTime().UTC()
	if current.After(freshness.SourceUpdatedAt) {
		freshness.Status = "stale"
		freshness.SourceUpdatedAt = current
	}

	return freshness
}
