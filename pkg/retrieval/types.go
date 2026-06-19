// Package retrieval defines the shared result contract used by Atteler search
// backends.
package retrieval

import (
	"time"

	"github.com/tommoulard/atteler/pkg/sourcepolicy"
)

const (
	// SourceMemory identifies the lexical memory store.
	SourceMemory SourceType = "memory"
	// SourceSession identifies saved session transcripts and metadata.
	SourceSession SourceType = "session"
	// SourceGitHistory identifies parsed git history.
	SourceGitHistory SourceType = "git_history"
	// SourceAgentMemory identifies per-agent vector memory.
	SourceAgentMemory SourceType = "agent_memory"
	// SourceVector identifies local vector indexes.
	SourceVector SourceType = "vector"
	// SourceFile identifies file-backed memory documents.
	SourceFile SourceType = "file"
	// SourceADR identifies architecture decision record sources.
	SourceADR SourceType = "adr"
)

const (
	// RangeUnitRuneOffset means Range boundaries are zero-based rune offsets in
	// the source text. Rune offsets are stable across UTF-8 byte widths and easy
	// for agents to cite without re-reading binary offsets.
	RangeUnitRuneOffset = "rune_offset"
)

// SourceType names a retrieval backend or source family.
type SourceType string

// Source describes the backend and logical collection that produced a result.
type Source struct {
	Type SourceType `json:"type"`
	Name string     `json:"name,omitempty"`
	URI  string     `json:"uri,omitempty"`
}

// Range identifies the span of source text represented by a result chunk.
type Range struct {
	Unit  string `json:"unit"`
	Start int    `json:"start"`
	End   int    `json:"end"`
}

// Chunk identifies the indexed text slice that matched a query.
//
//nolint:govet // Layout prioritizes JSON/API readability over pointer-byte packing.
type Chunk struct {
	Range       Range  `json:"range"`
	ID          string `json:"id"`
	ContentHash string `json:"content_hash,omitempty"`
	Index       int    `json:"index"`
}

// Scorer explains how a backend ranked a result. Result.Score is normalized to
// the [0,1] range for cross-source comparison; Raw preserves the backend-native
// value for debugging and regression tests.
type Scorer struct {
	Details     map[string]float64 `json:"details,omitempty"`
	Name        string             `json:"name"`
	Explanation []string           `json:"explanation,omitempty"`
	Raw         float64            `json:"raw"`
}

// Freshness records whether the result reflects the current source revision.
type Freshness struct {
	IndexedAt       time.Time `json:"indexed_at,omitzero"`
	SourceUpdatedAt time.Time `json:"source_updated_at,omitzero"`
	Status          string    `json:"status,omitempty"`
	Deleted         bool      `json:"deleted,omitempty"`
}

// Safety summarizes whether result text is safe to inject into model prompts.
type Safety struct {
	Reasons       []string `json:"reasons,omitempty"`
	InjectAllowed bool     `json:"inject_allowed"`
	Redacted      bool     `json:"redacted,omitempty"`
	Private       bool     `json:"private,omitempty"`
	Sensitive     bool     `json:"sensitive,omitempty"`
}

// SourceQuality describes source trust metadata attached to a retrieval result.
type SourceQuality = sourcepolicy.Quality

// Result is the common retrieval contract returned by all search backends.
//
//nolint:govet // Layout prioritizes JSON/API readability over pointer-byte packing.
type Result struct {
	Metadata   map[string]string `json:"metadata,omitempty"`
	Source     Source            `json:"source"`
	Quality    SourceQuality     `json:"source_quality,omitzero"`
	Chunk      Chunk             `json:"chunk"`
	Scorer     Scorer            `json:"scorer"`
	Freshness  Freshness         `json:"freshness,omitzero"`
	Safety     Safety            `json:"safety"`
	DocumentID string            `json:"document_id"`
	Snippet    string            `json:"snippet"`
	Score      float64           `json:"score"`
}

// Query configures a retrieval search across one or more sources.
//
//nolint:govet // Layout prioritizes JSON/API readability over pointer-byte packing.
type Query struct {
	Now           time.Time           `json:"now,omitzero"`
	Filters       map[string]string   `json:"filters,omitempty"`
	SourcePolicy  sourcepolicy.Policy `json:"source_policy,omitzero"`
	Sources       []SourceType        `json:"sources,omitempty"`
	Text          string              `json:"text"`
	Limit         int                 `json:"limit,omitempty"`
	Explain       bool                `json:"explain,omitempty"`
	IncludeUnsafe bool                `json:"include_unsafe,omitempty"`
}
