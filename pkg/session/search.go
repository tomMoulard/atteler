package session

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"math"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode"

	"github.com/tommoulard/atteler/pkg/llm"
	"github.com/tommoulard/atteler/pkg/retrieval"
)

const (
	maxSnippetsPerSession = 3
	snippetRadius         = 80
)

// SearchField names a field family in the saved-session search index.
type SearchField string

const (
	// SearchFieldTranscript matches chat transcript messages.
	SearchFieldTranscript SearchField = "transcript"
	// SearchFieldTags matches session tags.
	SearchFieldTags SearchField = "tags"
	// SearchFieldEvaluations matches agent evaluation records.
	SearchFieldEvaluations SearchField = "evaluations"
	// SearchFieldFailures matches negative-knowledge / failed-approach records.
	SearchFieldFailures SearchField = "failures"
	// SearchFieldArtifacts matches recorded artifact metadata.
	SearchFieldArtifacts SearchField = "artifacts"
	// SearchFieldAgent matches agent metadata and per-record source agents.
	SearchFieldAgent SearchField = "agent"
	// SearchFieldModel matches model metadata.
	SearchFieldModel SearchField = "model"
	// SearchFieldDate matches indexed created/updated timestamps.
	SearchFieldDate SearchField = "date"
	// SearchFieldRepo matches worktree/repository metadata.
	SearchFieldRepo SearchField = "repo"
	// SearchFieldSession matches the stable session ID.
	SearchFieldSession SearchField = "session"
	// SearchFieldTitle matches the session title.
	SearchFieldTitle SearchField = "title"
)

// SearchOptions scopes saved-session search without requiring callers to load
// each session file. Empty filters search all indexed fields and sessions.
type SearchOptions struct {
	// DateFrom keeps sessions whose latest indexed activity is on or after this time.
	DateFrom time.Time
	// DateTo keeps sessions whose latest indexed activity is on or before this time.
	DateTo time.Time
	// Agent keeps sessions indexed for this default or per-record source agent.
	Agent string
	// Model keeps sessions indexed for this default model.
	Model string
	// Repo keeps sessions whose worktree path basename matches this repo name.
	Repo string
	// Fields restricts matching evidence to the listed indexed field families.
	Fields []SearchField
	// Tags keeps sessions that contain every non-empty tag in this list.
	Tags []string
	// SessionIDs keeps sessions whose stable ID is in this list, when session identity is indexed.
	SessionIDs []string
	// Limit caps returned results after ranking; non-positive values return all results.
	Limit int
}

// SearchResult is one matching saved session plus ranked evidence.
type SearchResult struct {
	Snippets []SearchSnippet
	Matches  []SearchMatch
	Summary  Summary
	Score    float64
}

// SearchMatch is field-level evidence for a search hit.
type SearchMatch struct {
	Role  llm.Role
	Field SearchField
	// Label is a stable field path within the saved session, such as messages[1].content.
	Label string
	// Text is a compact excerpt from the indexed field value.
	Text string
	// Offset and End are rune offsets into the indexed field value, not the excerpt.
	Offset      int
	End         int
	ExactPhrase bool
	Score       float64
}

// SearchSnippet is a backward-compatible matching excerpt. Field, Label, Range
// and offsets identify stable evidence in the indexed field text.
//
//nolint:govet // Layout prioritizes API readability over pointer-byte packing.
type SearchSnippet struct {
	Range retrieval.Range
	Role  llm.Role
	Field SearchField
	Label string
	Kind  string
	Text  string
	Index int
	// Offset and End are rune offsets into the indexed field value, not the excerpt.
	Offset int
	End    int
}

type normalizedSearchQuery struct {
	normalized string
	tokens     []string
}

type fieldMatch struct {
	matchedTokens []string
	offset        int
	end           int
	occurrences   int
	exactPhrase   bool
}

// Search returns saved sessions whose indexed metadata or transcript contains query.
func (s *Store) Search(query string) ([]SearchResult, error) {
	return s.SearchWithOptions(query, SearchOptions{})
}

// SearchWithOptions returns indexed saved-session search results using field,
// date, agent/model, repo and session scopes from options.
func (s *Store) SearchWithOptions(query string, options SearchOptions) ([]SearchResult, error) {
	normalizedQuery, err := normalizeSearchQuery(query)
	if err != nil {
		return nil, err
	}

	index, err := s.ensureSearchIndex()
	if err != nil {
		return nil, err
	}

	documents := searchCandidateSessions(index, normalizedQuery.tokens)
	fieldFilter := normalizedFieldFilter(options.Fields)
	now := time.Now().UTC()

	results := make([]SearchResult, 0, len(documents))
	for _, document := range documents {
		if !searchDocumentMatchesFilters(document, options) {
			continue
		}

		result, ok := searchDocument(document, normalizedQuery, fieldFilter, now)
		if ok {
			result.Summary.Path = s.indexedSessionPath(document)
			results = append(results, result)
		}
	}

	sort.SliceStable(results, func(i, j int) bool {
		if math.Abs(results[i].Score-results[j].Score) > 0.000001 {
			return results[i].Score > results[j].Score
		}

		if !results[i].Summary.UpdatedAt.Equal(results[j].Summary.UpdatedAt) {
			return results[i].Summary.UpdatedAt.After(results[j].Summary.UpdatedAt)
		}

		return results[i].Summary.ID < results[j].Summary.ID
	})

	if options.Limit > 0 && len(results) > options.Limit {
		results = results[:options.Limit]
	}

	return results, nil
}

// SearchRetrieval returns saved session matches using the shared retrieval
// contract.
func (s *Store) SearchRetrieval(ctx context.Context, query retrieval.Query) ([]retrieval.Result, error) {
	if err := ctx.Err(); err != nil {
		return nil, fmt.Errorf("session retrieval: %w", err)
	}

	results, err := s.Search(query.Text)
	if err != nil {
		return nil, err
	}

	out := make([]retrieval.Result, 0, len(results))
	for i := range results {
		if err := ctx.Err(); err != nil {
			return nil, fmt.Errorf("session retrieval: %w", err)
		}

		result := results[i]
		out = append(out, sessionRetrievalResults(result, query)...)

		if query.Limit > 0 && len(out) >= query.Limit {
			return out[:query.Limit], nil
		}
	}

	return out, nil
}

func sessionRetrievalResults(result SearchResult, query retrieval.Query) []retrieval.Result {
	documentID := "session/" + result.Summary.ID
	rawScore := result.Score

	if rawScore <= 0 {
		rawScore = 1 + float64(len(result.Snippets))
	}

	baseMetadata := map[string]string{
		"session_id": result.Summary.ID,
	}

	if result.Summary.Title != "" {
		baseMetadata["session_title"] = result.Summary.Title
	}

	if result.Summary.DefaultAgent != "" {
		baseMetadata["default_agent"] = result.Summary.DefaultAgent
	}

	if result.Summary.DefaultModel != "" {
		baseMetadata["default_model"] = result.Summary.DefaultModel
	}

	if len(result.Snippets) == 0 {
		text := strings.TrimSpace(result.Summary.Title)
		if text == "" {
			text = result.Summary.ID
		}

		metadata := cloneStringMap(baseMetadata)
		metadata["kind"] = "metadata"

		return []retrieval.Result{sessionRetrievalResult(
			documentID,
			0,
			text,
			retrieval.Range{Unit: retrieval.RangeUnitRuneOffset, Start: 0, End: len([]rune(text))},
			metadata,
			result.Summary,
			rawScore,
			query,
		)}
	}

	out := make([]retrieval.Result, 0, len(result.Snippets))
	for i := range result.Snippets {
		snippet := &result.Snippets[i]
		metadata := cloneStringMap(baseMetadata)

		metadata["role"] = string(snippet.Role)
		metadata["field"] = string(snippet.Field)
		metadata["label"] = snippet.Label

		if snippet.Kind != "" {
			metadata["kind"] = snippet.Kind
		}

		if snippet.Index >= 0 {
			metadata["index"] = strconv.Itoa(snippet.Index)
		}

		sourceRange := snippet.Range
		if sourceRange.Unit == "" && snippet.End > snippet.Offset {
			sourceRange = retrieval.Range{Unit: retrieval.RangeUnitRuneOffset, Start: snippet.Offset, End: snippet.End}
		}

		out = append(out, sessionRetrievalResult(documentID, i, snippet.Text, sourceRange, metadata, result.Summary, rawScore, query))
	}

	return out
}

func sessionRetrievalResult(
	documentID string,
	index int,
	text string,
	sourceRange retrieval.Range,
	metadata map[string]string,
	summary Summary,
	rawScore float64,
	query retrieval.Query,
) retrieval.Result {
	source := retrieval.Source{Type: retrieval.SourceSession, Name: summary.ID, URI: summary.Path}

	policyContext := retrieval.PolicyContext{
		Source:     source,
		Metadata:   metadata,
		DocumentID: documentID,
		Path:       summary.Path,
	}
	text, safety := retrieval.Sanitize(text, policyContext)

	var metadataSafety retrieval.Safety

	metadata, metadataSafety = retrieval.SanitizeMetadata(metadata, policyContext)
	safety = retrieval.MergeSafety(safety, metadataSafety)

	if !retrieval.IsDefaultSafety(safety) {
		metadata = retrieval.MergeSafetyMetadata(metadata, safety)
	}

	if sourceRange.Unit == "" {
		sourceRange = retrieval.Range{Unit: retrieval.RangeUnitRuneOffset, Start: 0, End: len([]rune(text))}
	}

	chunk := retrieval.Chunk{
		ID:          retrieval.StableChunkID(documentID, index, sourceRange.Start, sourceRange.End, text),
		Index:       index,
		Range:       sourceRange,
		ContentHash: retrieval.TextHash(text),
	}

	scorer := retrieval.Scorer{
		Name: "session-index-lexical-recency",
		Raw:  rawScore,
		Details: map[string]float64{
			"matches": float64(max(0, len(metadata)-len(baseSessionRetrievalMetadataKeys()))),
		},
	}
	if query.Explain {
		scorer.Explanation = []string{"session search matched indexed metadata or transcript text and ranks by field weight plus recency"}
	}

	return retrieval.NormalizeResult(retrieval.Result{
		Source:     source,
		DocumentID: documentID,
		Chunk:      chunk,
		Score:      retrieval.NormalizeRawScore(rawScore),
		Scorer:     scorer,
		Snippet:    retrieval.Snippet(text, snippetRadius*2),
		Metadata:   metadata,
		Freshness: retrieval.Freshness{
			SourceUpdatedAt: summary.UpdatedAt,
			Status:          "current",
		},
		Safety: retrieval.SafetyFromMetadata(metadata),
	})
}

func baseSessionRetrievalMetadataKeys() []string {
	return []string{"session_id", "session_title", "default_agent", "default_model"}
}

func cloneStringMap(in map[string]string) map[string]string {
	if len(in) == 0 {
		return nil
	}

	out := make(map[string]string, len(in))
	maps.Copy(out, in)

	return out
}

func normalizeSearchQuery(query string) (normalizedSearchQuery, error) {
	query = strings.TrimSpace(query)
	if query == "" {
		return normalizedSearchQuery{}, errors.New("session: search query is required")
	}

	tokens := tokenizeSearchText(query)
	if len(tokens) == 0 {
		return normalizedSearchQuery{}, errors.New("session: search query is required")
	}

	return normalizedSearchQuery{
		normalized: strings.ToLower(query),
		tokens:     tokens,
	}, nil
}

func searchCandidateSessions(index sessionSearchIndex, tokens []string) []*indexedSession {
	if len(tokens) == 0 {
		return nil
	}

	counts := make(map[string]int)

	for _, token := range tokens {
		ids := matchingSessionIDsForToken(index.Terms, token)

		seen := make(map[string]struct{}, len(ids))
		for _, id := range ids {
			if _, ok := seen[id]; ok {
				continue
			}

			seen[id] = struct{}{}
			counts[id]++
		}
	}

	byKey := make(map[string]*indexedSession, len(index.Sessions))
	for i := range index.Sessions {
		document := &index.Sessions[i]
		byKey[document.Key] = document
	}

	documents := make([]*indexedSession, 0, len(counts))
	for key, count := range counts {
		if count != len(tokens) {
			continue
		}

		document, ok := byKey[key]
		if ok {
			documents = append(documents, document)
		}
	}

	sort.Slice(documents, func(i, j int) bool {
		if !documents[i].Summary.UpdatedAt.Equal(documents[j].Summary.UpdatedAt) {
			return documents[i].Summary.UpdatedAt.After(documents[j].Summary.UpdatedAt)
		}

		if documents[i].Summary.ID != documents[j].Summary.ID {
			return documents[i].Summary.ID < documents[j].Summary.ID
		}

		return documents[i].Key < documents[j].Key
	})

	return documents
}

func matchingSessionIDsForToken(terms map[string][]string, token string) []string {
	matches := make(map[string]struct{})

	for indexedToken, ids := range terms {
		if indexedToken != token && !strings.Contains(indexedToken, token) {
			continue
		}

		for _, id := range ids {
			matches[id] = struct{}{}
		}
	}

	out := make([]string, 0, len(matches))
	for id := range matches {
		out = append(out, id)
	}

	sort.Strings(out)

	return out
}

func searchDocument(
	document *indexedSession,
	query normalizedSearchQuery,
	fieldFilter map[SearchField]struct{},
	now time.Time,
) (SearchResult, bool) {
	matches := make([]SearchMatch, 0, len(document.Fields))
	completeMatches := make([]SearchMatch, 0, len(document.Fields))
	coveredTokens := make(map[string]struct{}, len(query.tokens))

	for _, field := range document.Fields {
		if !searchFieldAllowed(field.Field, fieldFilter) {
			continue
		}

		match, ok := matchIndexedField(field.Value, query)
		if !ok {
			continue
		}

		score := scoreFieldMatch(field, match)
		searchMatch := SearchMatch{
			Role:        field.Role,
			Field:       field.Field,
			Label:       field.Label,
			Text:        searchSnippetAt(field.Value, match.offset, match.end),
			Offset:      match.offset,
			End:         match.end,
			ExactPhrase: match.exactPhrase,
			Score:       score,
		}

		matches = append(matches, searchMatch)

		if coversAllSearchTokens(query.tokens, tokenSet(match.matchedTokens)) {
			completeMatches = append(completeMatches, searchMatch)
		}

		for _, token := range match.matchedTokens {
			coveredTokens[token] = struct{}{}
		}
	}

	if len(matches) == 0 || !coversAllSearchTokens(query.tokens, coveredTokens) {
		return SearchResult{}, false
	}

	if len(completeMatches) > 0 {
		matches = completeMatches
	}

	sort.SliceStable(matches, func(i, j int) bool {
		if math.Abs(matches[i].Score-matches[j].Score) > 0.000001 {
			return matches[i].Score > matches[j].Score
		}

		if matches[i].Field == matches[j].Field {
			if matches[i].Label != matches[j].Label {
				return matches[i].Label < matches[j].Label
			}

			return matches[i].Offset < matches[j].Offset
		}

		return matches[i].Field < matches[j].Field
	})

	score := recencyScore(document.Summary, now)
	for _, match := range matches {
		score += match.Score
	}

	result := SearchResult{
		Summary: document.Summary,
		Matches: matches,
		Score:   score,
	}
	result.Snippets = snippetsFromMatches(matches)

	return result, true
}

func matchIndexedField(value string, query normalizedSearchQuery) (fieldMatch, bool) {
	normalizedValue := strings.ToLower(value)

	queryRunes := []rune(query.normalized)
	if offset := runeIndex([]rune(normalizedValue), queryRunes); offset >= 0 {
		return fieldMatch{
			offset:        offset,
			end:           offset + len(queryRunes),
			occurrences:   countRuneOccurrences([]rune(normalizedValue), queryRunes),
			exactPhrase:   true,
			matchedTokens: append([]string(nil), query.tokens...),
		}, true
	}

	start := -1
	end := -1
	occurrences := 0
	matchedTokens := make([]string, 0, len(query.tokens))
	normalizedRunes := []rune(normalizedValue)

	for _, token := range query.tokens {
		tokenRunes := []rune(token)
		offset := runeIndex(normalizedRunes, tokenRunes)

		if offset < 0 {
			continue
		}

		if start < 0 || offset < start {
			start = offset
		}

		tokenEnd := offset + len(tokenRunes)
		if tokenEnd > end {
			end = tokenEnd
		}

		occurrences += countRuneOccurrences(normalizedRunes, tokenRunes)

		matchedTokens = append(matchedTokens, token)
	}

	if len(matchedTokens) == 0 {
		return fieldMatch{}, false
	}

	return fieldMatch{offset: start, end: end, occurrences: occurrences, matchedTokens: matchedTokens}, true
}

func coversAllSearchTokens(tokens []string, covered map[string]struct{}) bool {
	for _, token := range tokens {
		if _, ok := covered[token]; !ok {
			return false
		}
	}

	return true
}

func tokenSet(tokens []string) map[string]struct{} {
	set := make(map[string]struct{}, len(tokens))
	for _, token := range tokens {
		set[token] = struct{}{}
	}

	return set
}

func scoreFieldMatch(field indexedField, match fieldMatch) float64 {
	weight := searchFieldWeight(field.Field)
	score := weight

	if match.exactPhrase {
		score += weight * 2
	}

	if match.occurrences > 1 {
		score += float64(match.occurrences-1) * (weight / 2)
	}

	if field.Field == SearchFieldFailures {
		score += 4 // Failed approaches are deliberately stronger than transcript echoes.
	}

	return score
}

func searchFieldWeight(field SearchField) float64 {
	switch field {
	case SearchFieldFailures:
		return 8
	case SearchFieldTags:
		return 6
	case SearchFieldTitle:
		return 5
	case SearchFieldEvaluations:
		return 5
	case SearchFieldArtifacts:
		return 4
	case SearchFieldAgent, SearchFieldModel, SearchFieldRepo, SearchFieldSession:
		return 3
	case SearchFieldDate:
		return 1
	case SearchFieldTranscript:
		return 2
	default:
		return 1
	}
}

func recencyScore(summary Summary, now time.Time) float64 {
	activity := fallbackActivity(summary.UpdatedAt, summary.CreatedAt)
	if activity.IsZero() {
		return 0
	}

	ageDays := now.Sub(activity).Hours() / 24
	if ageDays < 0 {
		ageDays = 0
	}

	return 2 / (1 + ageDays/30)
}

func snippetsFromMatches(matches []SearchMatch) []SearchSnippet {
	limit := min(maxSnippetsPerSession, len(matches))
	snippets := make([]SearchSnippet, 0, limit)

	for i := range limit {
		match := matches[i]
		kind, index := snippetKindIndex(match.Label)
		snippets = append(snippets, SearchSnippet{
			Range:  retrieval.Range{Unit: retrieval.RangeUnitRuneOffset, Start: match.Offset, End: match.End},
			Role:   match.Role,
			Field:  match.Field,
			Label:  match.Label,
			Kind:   kind,
			Text:   match.Text,
			Index:  index,
			Offset: match.Offset,
			End:    match.End,
		})
	}

	return snippets
}

func snippetKindIndex(label string) (kind string, index int) {
	for _, candidate := range []struct {
		prefix string
		kind   string
	}{
		{prefix: "messages[", kind: "message"},
		{prefix: "negative_knowledge[", kind: "negative_knowledge"},
		{prefix: "evaluations[", kind: "evaluation"},
		{prefix: "artifacts[", kind: "artifact"},
	} {
		if strings.HasPrefix(label, candidate.prefix) {
			return candidate.kind, parseOneBasedLabelIndex(label, len(candidate.prefix))
		}
	}

	return "metadata", -1
}

func parseOneBasedLabelIndex(label string, start int) int {
	end := strings.IndexByte(label[start:], ']')
	if end < 0 {
		return -1
	}

	index, err := strconv.Atoi(label[start : start+end])
	if err != nil || index <= 0 {
		return -1
	}

	return index - 1
}

func normalizedFieldFilter(fields []SearchField) map[SearchField]struct{} {
	if len(fields) == 0 {
		return nil
	}

	filter := make(map[SearchField]struct{}, len(fields))
	for _, field := range fields {
		if field == "" {
			continue
		}

		filter[field] = struct{}{}
	}

	return filter
}

func searchFieldAllowed(field SearchField, filter map[SearchField]struct{}) bool {
	if len(filter) == 0 {
		return true
	}

	_, ok := filter[field]

	return ok
}

func searchDocumentMatchesFilters(document *indexedSession, options SearchOptions) bool {
	if !sessionIDFilterMatches(document.Summary.ID, options.SessionIDs) {
		return false
	}

	if !stringSetFilterMatches(document.Agents, options.Agent) {
		return false
	}

	if !stringSetFilterMatches(document.Models, options.Model) {
		return false
	}

	if !repoFilterMatches(document.Repositories, options.Repo) {
		return false
	}

	if !tagFiltersMatch(document.Tags, options.Tags) {
		return false
	}

	return dateFilterMatches(document.Summary, options.DateFrom, options.DateTo)
}

func sessionIDFilterMatches(id string, ids []string) bool {
	if len(ids) == 0 {
		return true
	}

	hasFilter := false

	for _, want := range ids {
		want = strings.TrimSpace(want)
		if want == "" {
			continue
		}

		hasFilter = true

		if want == id {
			return true
		}
	}

	return !hasFilter
}

func stringSetFilterMatches(values []string, want string) bool {
	want = normalizeFilterValue(want)
	if want == "" {
		return true
	}

	for _, value := range values {
		if normalizeFilterValue(value) == want {
			return true
		}
	}

	return false
}

func repoFilterMatches(values []string, want string) bool {
	want = normalizeRepoFilterValue(want)
	if want == "" {
		return true
	}

	for _, value := range values {
		normalizedValue := normalizeFilterValue(value)
		if normalizedValue == want {
			return true
		}
	}

	return false
}

func normalizeRepoFilterValue(value string) string {
	value = strings.TrimSpace(value)
	if strings.ContainsAny(value, `/\`) {
		value = pathBase(value)
	}

	return normalizeFilterValue(value)
}

func tagFiltersMatch(tags, wantTags []string) bool {
	if len(wantTags) == 0 {
		return true
	}

	indexed := make(map[string]struct{}, len(tags))
	for _, tag := range tags {
		key := normalizeTagKey(tag)
		if key != "" {
			indexed[key] = struct{}{}
		}
	}

	for _, want := range wantTags {
		key := normalizeTagKey(want)
		if key == "" {
			continue
		}

		if _, ok := indexed[key]; !ok {
			return false
		}
	}

	return true
}

func dateFilterMatches(summary Summary, from, to time.Time) bool {
	activity := fallbackActivity(summary.UpdatedAt, summary.CreatedAt)
	if activity.IsZero() {
		return from.IsZero() && to.IsZero()
	}

	if !from.IsZero() && activity.Before(from.UTC()) {
		return false
	}

	if !to.IsZero() && activity.After(to.UTC()) {
		return false
	}

	return true
}

func normalizeFilterValue(value string) string {
	return strings.ToLower(strings.TrimSpace(value))
}

func tokenizeSearchText(value string) []string {
	tokens := make([]string, 0)
	seen := make(map[string]struct{})

	for _, token := range strings.FieldsFunc(strings.ToLower(value), func(r rune) bool {
		return !isSearchTokenRune(r)
	}) {
		if token == "" {
			continue
		}

		if _, ok := seen[token]; ok {
			continue
		}

		seen[token] = struct{}{}
		tokens = append(tokens, token)
	}

	return tokens
}

func isSearchTokenRune(r rune) bool {
	return unicode.IsLetter(r) || unicode.IsNumber(r)
}

func searchSnippetAt(content string, offset, end int) string {
	contentRunes := []rune(content)
	if offset < 0 || end < offset || offset >= len(contentRunes) {
		return compactWhitespace(content)
	}

	start := max(0, offset-snippetRadius)
	snippetEnd := min(len(contentRunes), end+snippetRadius)

	snippet := strings.TrimSpace(string(contentRunes[start:snippetEnd]))
	if start > 0 {
		snippet = "…" + snippet
	}

	if snippetEnd < len(contentRunes) {
		snippet += "…"
	}

	return compactWhitespace(snippet)
}

func runeIndex(haystack, needle []rune) int {
	if len(needle) == 0 || len(needle) > len(haystack) {
		return -1
	}

	for i := 0; i <= len(haystack)-len(needle); i++ {
		if slices.Equal(haystack[i:i+len(needle)], needle) {
			return i
		}
	}

	return -1
}

func countRuneOccurrences(haystack, needle []rune) int {
	if len(needle) == 0 || len(needle) > len(haystack) {
		return 0
	}

	count := 0

	for i := 0; i <= len(haystack)-len(needle); i++ {
		if slices.Equal(haystack[i:i+len(needle)], needle) {
			count++
		}
	}

	return count
}

func compactWhitespace(value string) string {
	return strings.Join(strings.Fields(value), " ")
}
