//nolint:wsl_v5 // Search normalization keeps related policy/safety checks compact.
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

	"github.com/tommoulard/atteler/pkg/sourcepolicy"
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

		merged = appendSelectedResults(merged, results, selected, query.Filters, query.SourcePolicy)
	}

	merged = Deduplicate(merged)
	merged = filterDeniedSourceResults(merged)
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
	if query.Limit <= 0 || (len(selected) == 0 && query.IncludeUnsafe && len(query.Filters) == 0 && !sourcePolicyMayAffectResults(query.SourcePolicy)) {
		return query
	}

	// Apply the public limit after source/policy filtering so one filtered top
	// hit cannot hide a lower-ranked allowed result from the same backend.
	query.Limit = 0

	return query
}

func sourcePolicyMayFilter(policy sourcepolicy.Policy) bool {
	if len(policy.DeniedDomains) > 0 {
		return true
	}
	return policy.AllowLowTrustSources != nil && !*policy.AllowLowTrustSources
}

func sourcePolicyMayAffectResults(policy sourcepolicy.Policy) bool {
	return sourcePolicyMayFilter(policy) ||
		len(policy.TrustedDomains) > 0 ||
		len(policy.PreferSourceTypes) > 0
}

func appendSelectedResults(out, results []Result, selected map[SourceType]struct{}, filters map[string]string, policy sourcepolicy.Policy) []Result {
	for i := range results {
		result := NormalizeResultWithSourcePolicy(results[i], policy)
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
	return NormalizeResultWithSourcePolicy(result, sourcepolicy.Policy{})
}

// NormalizeResultWithSourcePolicy fills derived contract metadata, source
// quality, and clamps the comparable score.
func NormalizeResultWithSourcePolicy(result Result, policy sourcepolicy.Policy) Result {
	if result.Metadata == nil {
		result.Metadata = make(map[string]string)
	} else {
		result.Metadata = maps.Clone(result.Metadata)
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

	evaluation := sourceEvaluationForResult(result, policy)
	result.Quality = evaluation.Quality
	result.Metadata = MergeSourceQualityMetadata(result.Metadata, result.Quality)
	if len(evaluation.Warnings) > 0 {
		result.Metadata[MetadataSourceQualityWarnings] = strings.Join(evaluation.Warnings, ";")
	}
	result.Score = ClampScore(result.Score)

	return result
}

// MergeSourceQualityMetadata stores source-quality fields in result metadata.
func MergeSourceQualityMetadata(metadata map[string]string, quality SourceQuality) map[string]string {
	if metadata == nil {
		metadata = make(map[string]string)
	}

	if quality.Domain != "" {
		metadata[MetadataSourceQualityDomain] = quality.Domain
	}
	if quality.SourceType != "" {
		metadata[MetadataSourceQualityType] = quality.SourceType
	}
	if quality.TrustLevel != "" {
		metadata[MetadataSourceQualityTrustLevel] = quality.TrustLevel
	}
	if quality.TrustScore != 0 {
		metadata[MetadataSourceQualityTrustScore] = strconv.FormatFloat(quality.TrustScore, 'f', 3, 64)
	}
	if quality.PolicyMatch != "" {
		metadata[MetadataSourceQualityPolicyMatch] = quality.PolicyMatch
	}

	return metadata
}

func sourceEvaluationForResult(result Result, policy sourcepolicy.Policy) sourcepolicy.Evaluation {
	if sourcePolicyEmpty(policy) && sourceQualityAssessed(result.Quality) {
		return sourcepolicy.Evaluation{Quality: result.Quality, Allowed: true}
	}

	sourceURI := ""
	if strings.HasPrefix(result.Source.URI, "http://") || strings.HasPrefix(result.Source.URI, "https://") {
		sourceURI = result.Source.URI
	}
	if sourceURI == "" && result.Quality.Domain != "" {
		sourceURI = "https://" + sourcepolicy.NormalizeDomain(result.Quality.Domain)
	}

	source := sourcepolicy.Source{
		URL:        sourceURI,
		Path:       retrievalResultPath(result),
		Title:      result.DocumentID,
		SourceType: retrievalSourcePolicyType(result),
	}
	if result.Quality.SourceType != "" {
		source.SourceType = result.Quality.SourceType
	}
	evaluation := sourcepolicy.Evaluate(source, policy)

	return evaluation
}

func sourcePolicyEmpty(policy sourcepolicy.Policy) bool {
	return len(policy.TrustedDomains) == 0 &&
		len(policy.DeniedDomains) == 0 &&
		len(policy.PreferSourceTypes) == 0 &&
		policy.AllowLowTrustSources == nil &&
		policy.WarnOnLowTrustSources == nil &&
		policy.RequireEvidenceForHighImpactClaims == nil
}

func sourceQualityAssessed(quality SourceQuality) bool {
	return quality.TrustLevel != "" || quality.TrustScore != 0 || quality.PolicyMatch != ""
}

func retrievalResultPath(result Result) string {
	for _, key := range []string{"path", "file", "filename"} {
		if value := strings.TrimSpace(result.Metadata[key]); value != "" {
			return value
		}
	}

	if result.Source.URI != "" && !strings.HasPrefix(result.Source.URI, "http://") && !strings.HasPrefix(result.Source.URI, "https://") {
		return result.Source.URI
	}

	switch result.Source.Type {
	case SourceFile, SourceVector, SourceADR, SourceGitHistory:
		return result.DocumentID
	default:
		return ""
	}
}

func retrievalSourcePolicyType(result Result) string {
	if strings.HasPrefix(result.Source.URI, "http://") || strings.HasPrefix(result.Source.URI, "https://") {
		return sourcepolicy.SourceTypeUnknown
	}

	switch result.Source.Type {
	case SourceFile, SourceVector, SourceADR, SourceGitHistory:
		return sourcepolicy.SourceTypeSourceCode
	default:
		return sourcepolicy.SourceTypeUnknown
	}
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

func filterDeniedSourceResults(results []Result) []Result {
	filtered := results[:0]
	for i := range results {
		result := results[i]
		if result.Quality.TrustLevel == sourcepolicy.TrustLevelDenied {
			continue
		}
		if result.Quality.PolicyMatch == sourcepolicy.PolicyMatchLowTrustDisallowed {
			continue
		}
		filtered = append(filtered, result)
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
	winner.Quality = stricterSourceQuality(existing.Quality, candidate.Quality)

	if shouldPreferSaferPayload(loser, Result{Safety: winnerSafety}) {
		winner.Snippet = loser.Snippet
		winner.Metadata = mergeMetadataPreferLeft(loser.Metadata, winner.Metadata)
	} else {
		winner.Metadata = mergeMetadataPreferLeft(winner.Metadata, loser.Metadata)
	}

	if !IsDefaultSafety(winner.Safety) {
		winner.Metadata = MergeSafetyMetadata(winner.Metadata, winner.Safety)
	}
	winner.Metadata = MergeSourceQualityMetadata(winner.Metadata, winner.Quality)

	return winner
}

func stricterSourceQuality(left, right SourceQuality) SourceQuality {
	if sourceTrustRank(right.TrustLevel) > sourceTrustRank(left.TrustLevel) {
		return right
	}
	if sourceTrustRank(right.TrustLevel) == sourceTrustRank(left.TrustLevel) && right.TrustScore < left.TrustScore {
		return right
	}

	return left
}

func sourceTrustRank(level string) int {
	switch level {
	case sourcepolicy.TrustLevelDenied:
		return 4
	case sourcepolicy.TrustLevelLow:
		return 3
	case sourcepolicy.TrustLevelUnknown:
		return 2
	case sourcepolicy.TrustLevelMedium:
		return 1
	default:
		return 0
	}
}

// SortResults applies the shared ranking tie-breakers.
func SortResults(results []Result) {
	sort.SliceStable(results, func(i, j int) bool {
		left, right := results[i], results[j]
		if left.Score != right.Score {
			return left.Score > right.Score
		}

		if leftQuality, rightQuality := sourceQualitySortScore(left.Quality), sourceQualitySortScore(right.Quality); leftQuality != rightQuality {
			return leftQuality > rightQuality
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

func sourceQualitySortScore(quality SourceQuality) float64 {
	if quality.TrustScore > 0 {
		return quality.TrustScore
	}

	switch quality.TrustLevel {
	case sourcepolicy.TrustLevelHigh:
		return 0.80
	case sourcepolicy.TrustLevelMedium:
		return 0.60
	case sourcepolicy.TrustLevelLow:
		return 0.40
	default:
		return 0
	}
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
