package retrieval

import (
	"context"
	"fmt"
	"maps"
	"math"
	"slices"
	"sort"
	"strconv"
	"strings"
)

// Searcher is implemented by retrieval backends that can return the shared
// contract directly.
type Searcher interface {
	SearchRetrieval(context.Context, Query) ([]Result, error)
}

// Search queries selected sources, removes unsafe results unless requested,
// deduplicates equivalent chunks, and returns cross-source comparable results.
func Search(ctx context.Context, query Query, searchers ...Searcher) ([]Result, error) {
	selected := selectedSources(query.Sources)
	merged := make([]Result, 0)

	for _, searcher := range searchers {
		if err := ctx.Err(); err != nil {
			return nil, fmt.Errorf("retrieval search: %w", err)
		}

		if searcher == nil {
			continue
		}

		results, err := searcher.SearchRetrieval(ctx, backendQueryFor(query, selected))
		if err != nil {
			return nil, fmt.Errorf("retrieval searcher: %w", err)
		}

		merged = appendSelectedResults(merged, results, selected, query.Filters)
	}

	merged = Deduplicate(merged)
	if !query.IncludeUnsafe {
		merged = filterUnsafeResults(merged)
	}

	SortResults(merged)

	if query.Limit > 0 && len(merged) > query.Limit {
		merged = merged[:query.Limit]
	}

	return merged, nil
}

func backendQueryFor(query Query, selected map[SourceType]struct{}) Query {
	if query.Limit <= 0 || (len(selected) == 0 && query.IncludeUnsafe && len(query.Filters) == 0) {
		return query
	}

	// Apply the public limit after source/policy filtering so one filtered top
	// hit cannot hide a lower-ranked allowed result from the same backend.
	query.Limit = 0

	return query
}

func appendSelectedResults(out, results []Result, selected map[SourceType]struct{}, filters map[string]string) []Result {
	for i := range results {
		result := NormalizeResult(results[i])
		if !sourceSelected(result.Source.Type, selected) {
			continue
		}

		if !matchesFilters(result, filters) {
			continue
		}

		out = append(out, result)
	}

	return out
}

// NormalizeResult fills derived contract metadata and clamps the comparable
// score. Backends may call it before returning direct SearchRetrieval results;
// Search also applies it defensively before cross-source filtering/ranking.
func NormalizeResult(result Result) Result {
	if result.Metadata == nil {
		result.Metadata = make(map[string]string)
	}

	if IsZeroSafety(result.Safety) {
		result.Safety = SafetyFromMetadata(result.Metadata)
	} else {
		result.Safety = MergeSafety(NormalizeSafety(result.Safety), SafetyFromMetadata(result.Metadata))
	}

	if result.Metadata[MetadataStableID] == "" && result.DocumentID != "" {
		result.Metadata[MetadataStableID] = StableDocumentID(result.Source, result.DocumentID)
	}

	if result.Chunk.ContentHash == "" {
		if contentHash := result.Metadata[MetadataContentHash]; contentHash != "" {
			result.Chunk.ContentHash = contentHash
		} else if result.Snippet != "" {
			result.Chunk.ContentHash = TextHash(result.Snippet)
		}
	}

	if result.Metadata[MetadataContentHash] == "" && result.Chunk.ContentHash != "" {
		result.Metadata[MetadataContentHash] = result.Chunk.ContentHash
	}

	result.Score = ClampScore(result.Score)

	return result
}

func matchesFilters(result Result, filters map[string]string) bool {
	if len(filters) == 0 {
		return true
	}

	for key, want := range filters {
		if !matchesFilter(result, key, want) {
			return false
		}
	}

	return true
}

func matchesFilter(result Result, key, want string) bool {
	switch key {
	case "source.type":
		return string(result.Source.Type) == want
	case "source.name":
		return result.Source.Name == want
	case "source.uri":
		return result.Source.URI == want
	case "document_id":
		return result.DocumentID == want
	case "safety.inject_allowed":
		return strconv.FormatBool(result.Safety.InjectAllowed) == want
	default:
		return result.Metadata[key] == want
	}
}

func filterUnsafeResults(results []Result) []Result {
	filtered := results[:0]
	for i := range results {
		result := results[i]
		if result.Safety.InjectAllowed {
			filtered = append(filtered, result)
		}
	}

	return filtered
}

func sourceSelected(source SourceType, selected map[SourceType]struct{}) bool {
	if len(selected) == 0 {
		return true
	}

	_, ok := selected[source]

	return ok
}

// Deduplicate collapses repeated source/document/chunk hits, keeping the
// highest normalized score and merging explanation strings.
func Deduplicate(results []Result) []Result {
	seen := make(map[string]int, len(results))
	out := make([]Result, 0, len(results))

	for i := range results {
		result := results[i]

		key := dedupeKey(result)
		if existingIndex, ok := seen[key]; ok {
			out[existingIndex] = mergeDuplicate(out[existingIndex], result)

			continue
		}

		seen[key] = len(out)
		out = append(out, result)
	}

	return out
}

func mergeDuplicate(existing, candidate Result) Result {
	winner, loser := existing, candidate
	if candidate.Score > existing.Score {
		winner, loser = candidate, existing
	}

	winnerSafety := winner.Safety
	winner.Scorer.Explanation = mergeExplanations(existing.Scorer.Explanation, candidate.Scorer.Explanation)
	winner.Safety = MergeSafety(existing.Safety, candidate.Safety)

	if shouldPreferSaferPayload(loser, Result{Safety: winnerSafety}) {
		winner.Snippet = loser.Snippet
		winner.Metadata = mergeMetadataPreferLeft(loser.Metadata, winner.Metadata)
	} else {
		winner.Metadata = mergeMetadataPreferLeft(winner.Metadata, loser.Metadata)
	}

	if !IsDefaultSafety(winner.Safety) {
		winner.Metadata = MergeSafetyMetadata(winner.Metadata, winner.Safety)
	}

	return winner
}

// SortResults applies the shared ranking tie-breakers.
func SortResults(results []Result) {
	sort.SliceStable(results, func(i, j int) bool {
		left, right := results[i], results[j]
		if left.Score != right.Score {
			return left.Score > right.Score
		}

		if left.Source.Type != right.Source.Type {
			return left.Source.Type < right.Source.Type
		}

		if left.Source.Name != right.Source.Name {
			return left.Source.Name < right.Source.Name
		}

		if left.DocumentID != right.DocumentID {
			return left.DocumentID < right.DocumentID
		}

		return left.Chunk.ID < right.Chunk.ID
	})
}

// NormalizeRawScore converts an unbounded positive backend score to [0,1).
func NormalizeRawScore(raw float64) float64 {
	if raw <= 0 || math.IsNaN(raw) || math.IsInf(raw, 0) {
		return 0
	}

	return raw / (raw + 1)
}

// ClampScore clamps native comparable scores (for example cosine similarity)
// to [0,1].
func ClampScore(score float64) float64 {
	if math.IsNaN(score) || score < 0 {
		return 0
	}

	if math.IsInf(score, 1) || score > 1 {
		return 1
	}

	return score
}

func selectedSources(sources []SourceType) map[SourceType]struct{} {
	if len(sources) == 0 {
		return nil
	}

	selected := make(map[SourceType]struct{}, len(sources))
	for _, source := range sources {
		if source != "" {
			selected[source] = struct{}{}
		}
	}

	return selected
}

func dedupeKey(result Result) string {
	chunkID := result.Chunk.ID
	if chunkID == "" {
		chunkID = result.Chunk.ContentHash
	}

	return strings.Join([]string{string(result.Source.Type), result.Source.Name, result.DocumentID, chunkID}, "\x00")
}

func mergeExplanations(left, right []string) []string {
	out := append([]string(nil), left...)

	for _, candidate := range right {
		candidate = strings.TrimSpace(candidate)
		if candidate == "" {
			continue
		}

		if !slices.Contains(out, candidate) {
			out = append(out, candidate)
		}
	}

	return out
}

func shouldPreferSaferPayload(candidate, current Result) bool {
	if candidate.Snippet == "" && len(candidate.Metadata) == 0 {
		return false
	}

	if candidate.Safety.Redacted && !current.Safety.Redacted {
		return true
	}

	if !candidate.Safety.InjectAllowed && current.Safety.InjectAllowed {
		return true
	}

	return false
}

func mergeMetadataPreferLeft(left, right map[string]string) map[string]string {
	if len(left) == 0 && len(right) == 0 {
		return nil
	}

	out := make(map[string]string, len(left)+len(right))
	maps.Copy(out, right)
	maps.Copy(out, left)

	return out
}
