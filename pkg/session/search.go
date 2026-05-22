package session

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strconv"
	"strings"

	"github.com/tommoulard/atteler/pkg/llm"
	"github.com/tommoulard/atteler/pkg/retrieval"
)

const (
	maxSnippetsPerSession = 3
	snippetRadius         = 80
)

// SearchResult is one matching saved session plus representative snippets.
type SearchResult struct {
	Snippets []SearchSnippet
	Summary  Summary
}

// SearchSnippet is a matching transcript excerpt with enough provenance to cite
// the original session item instead of only the session file.
//
//nolint:govet // Layout prioritizes API readability over pointer-byte packing.
type SearchSnippet struct {
	Range retrieval.Range
	Role  llm.Role
	Kind  string
	Text  string
	Index int
}

// Search returns saved sessions whose metadata or transcript contains query.
func (s *Store) Search(query string) ([]SearchResult, error) {
	query = strings.TrimSpace(query)
	if query == "" {
		return nil, errors.New("session: search query is required")
	}

	entries, err := os.ReadDir(s.dir)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil
		}

		return nil, fmt.Errorf("session: search %s: %w", s.dir, err)
	}

	normalizedQuery := strings.ToLower(query)

	results := make([]SearchResult, 0, len(entries))
	for _, entry := range entries {
		if entry.IsDir() || filepath.Ext(entry.Name()) != sessionFileExt {
			continue
		}

		path := filepath.Join(s.dir, entry.Name())

		session, err := s.Load(path)
		if err != nil {
			return nil, err
		}

		result, ok := matchSession(summarize(path, session), session, query, normalizedQuery)
		if ok {
			results = append(results, result)
		}
	}

	sort.Slice(results, func(i, j int) bool {
		return results[i].Summary.UpdatedAt.After(results[j].Summary.UpdatedAt)
	})

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

func matchSession(summary Summary, session Session, query, normalizedQuery string) (SearchResult, bool) {
	result := SearchResult{Summary: summary}
	matched := strings.Contains(strings.ToLower(summary.ID), normalizedQuery) ||
		strings.Contains(strings.ToLower(summary.Title), normalizedQuery) ||
		strings.Contains(strings.ToLower(summary.DefaultAgent), normalizedQuery) ||
		strings.Contains(strings.ToLower(summary.DefaultModel), normalizedQuery) ||
		containsTag(summary.Tags, normalizedQuery)

	result.Snippets = append(result.Snippets, messageSnippets(session.Messages, query, normalizedQuery)...)
	result.Snippets = appendLimitedSnippets(result.Snippets, negativeKnowledgeSnippets(session.NegativeKnowledge, query, normalizedQuery)...)
	result.Snippets = appendLimitedSnippets(result.Snippets, evaluationSnippets(session.Evaluations, query, normalizedQuery)...)
	result.Snippets = appendLimitedSnippets(result.Snippets, artifactSnippets(session.Artifacts, query, normalizedQuery)...)

	return result, matched || len(result.Snippets) > 0
}

func sessionRetrievalResults(result SearchResult, query retrieval.Query) []retrieval.Result {
	documentID := "session/" + result.Summary.ID
	rawScore := 1 + float64(len(result.Snippets))

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
	for i, snippet := range result.Snippets {
		metadata := cloneStringMap(baseMetadata)

		metadata["role"] = string(snippet.Role)
		if snippet.Kind != "" {
			metadata["kind"] = snippet.Kind
			metadata["index"] = strconv.Itoa(snippet.Index)
		}

		out = append(out, sessionRetrievalResult(documentID, i, snippet.Text, snippet.Range, metadata, result.Summary, rawScore, query))
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
		Name: "session-recency-lexical",
		Raw:  rawScore,
		Details: map[string]float64{
			"snippets": max(0, rawScore-1),
		},
	}
	if query.Explain {
		scorer.Explanation = []string{"session matched metadata or transcript text; session search preserves newest-updated ordering"}
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

func cloneStringMap(in map[string]string) map[string]string {
	if len(in) == 0 {
		return nil
	}

	out := make(map[string]string, len(in))
	maps.Copy(out, in)

	return out
}

func messageSnippets(messages []llm.Message, query, normalizedQuery string) []SearchSnippet {
	snippets := make([]SearchSnippet, 0, maxSnippetsPerSession)

	for i, message := range messages {
		if !matchesMessage(message, normalizedQuery) {
			continue
		}

		snippet := searchSnippet(message.Content, query)

		snippets = append(snippets, SearchSnippet{
			Kind:  "message",
			Index: i,
			Role:  message.Role,
			Text:  snippet.Text,
			Range: snippet.Range,
		})
		if len(snippets) >= maxSnippetsPerSession {
			return snippets
		}
	}

	return snippets
}

func negativeKnowledgeSnippets(entries []NegativeKnowledge, query, normalizedQuery string) []SearchSnippet {
	snippets := make([]SearchSnippet, 0, maxSnippetsPerSession)

	for i, entry := range entries {
		if !matchesNegativeKnowledge(entry, normalizedQuery) {
			continue
		}

		snippet := searchSnippet(negativeKnowledgeSearchText(entry), query)

		snippets = append(snippets, SearchSnippet{
			Kind:  "negative_knowledge",
			Index: i,
			Role:  llm.Role("negative_knowledge"),
			Text:  snippet.Text,
			Range: snippet.Range,
		})
		if len(snippets) >= maxSnippetsPerSession {
			return snippets
		}
	}

	return snippets
}

func evaluationSnippets(entries []AgentEvaluation, query, normalizedQuery string) []SearchSnippet {
	snippets := make([]SearchSnippet, 0, maxSnippetsPerSession)

	for i := range entries {
		entry := &entries[i]
		if !matchesEvaluation(entry, normalizedQuery) {
			continue
		}

		snippet := searchSnippet(evaluationSearchText(entry), query)

		snippets = append(snippets, SearchSnippet{
			Kind:  "evaluation",
			Index: i,
			Role:  llm.Role("evaluation"),
			Text:  snippet.Text,
			Range: snippet.Range,
		})
		if len(snippets) >= maxSnippetsPerSession {
			return snippets
		}
	}

	return snippets
}

func artifactSnippets(entries []Artifact, query, normalizedQuery string) []SearchSnippet {
	snippets := make([]SearchSnippet, 0, maxSnippetsPerSession)

	for i := range entries {
		entry := &entries[i]
		if !matchesArtifact(entry, normalizedQuery) {
			continue
		}

		snippet := searchSnippet(artifactSearchText(entry), query)

		snippets = append(snippets, SearchSnippet{
			Kind:  "artifact",
			Index: i,
			Role:  llm.Role("artifact"),
			Text:  snippet.Text,
			Range: snippet.Range,
		})
		if len(snippets) >= maxSnippetsPerSession {
			return snippets
		}
	}

	return snippets
}

func appendLimitedSnippets(existing []SearchSnippet, candidates ...SearchSnippet) []SearchSnippet {
	if len(existing) >= maxSnippetsPerSession {
		return existing
	}

	remaining := maxSnippetsPerSession - len(existing)
	if len(candidates) > remaining {
		candidates = candidates[:remaining]
	}

	return append(existing, candidates...)
}

func containsTag(tags []string, normalizedQuery string) bool {
	for _, tag := range tags {
		if strings.Contains(strings.ToLower(tag), normalizedQuery) {
			return true
		}
	}

	return false
}

func matchesMessage(message llm.Message, normalizedQuery string) bool {
	return strings.Contains(strings.ToLower(string(message.Role)), normalizedQuery) ||
		strings.Contains(strings.ToLower(message.Content), normalizedQuery)
}

func matchesNegativeKnowledge(entry NegativeKnowledge, normalizedQuery string) bool {
	return strings.Contains(strings.ToLower(entry.Approach), normalizedQuery) ||
		strings.Contains(strings.ToLower(entry.Reason), normalizedQuery) ||
		strings.Contains(strings.ToLower(entry.Commit), normalizedQuery) ||
		strings.Contains(strings.ToLower(entry.Agent), normalizedQuery) ||
		strings.Contains(strings.ToLower(entry.TaskType), normalizedQuery) ||
		strings.Contains(strings.ToLower(entry.Severity), normalizedQuery)
}

func negativeKnowledgeSearchText(entry NegativeKnowledge) string {
	parts := []string{"Failed attempt: " + entry.Approach, "Reason: " + entry.Reason}
	if entry.Commit != "" {
		parts = append(parts, "Commit: "+entry.Commit)
	}

	if entry.Agent != "" {
		parts = append(parts, "Agent: "+entry.Agent)
	}

	if entry.TaskType != "" {
		parts = append(parts, "Task Type: "+entry.TaskType)
	}

	if entry.Severity != "" {
		parts = append(parts, "Severity: "+entry.Severity)
	}

	return strings.Join(parts, " | ")
}

func matchesEvaluation(entry *AgentEvaluation, normalizedQuery string) bool {
	return strings.Contains(strings.ToLower(evaluationSearchText(entry)), normalizedQuery)
}

func evaluationSearchText(entry *AgentEvaluation) string {
	parts := []string{"Evaluation: " + entry.Agent, "Outcome: " + entry.Outcome}
	if entry.Score != 0 {
		parts = append(parts, fmt.Sprintf("Score: %d", entry.Score))
	}

	if entry.Reference != "" {
		parts = append(parts, "Reference: "+entry.Reference)
	}

	parts = appendEvaluationSearchMetadata(parts, entry)

	if entry.Notes != "" {
		parts = append(parts, "Notes: "+entry.Notes)
	}

	return strings.Join(parts, " | ")
}

func appendEvaluationSearchMetadata(parts []string, entry *AgentEvaluation) []string {
	if entry.Source != "" {
		parts = append(parts, "Source: "+entry.Source)
	}

	if entry.Evaluator != "" {
		parts = append(parts, "Evaluator: "+entry.Evaluator)
	}

	if entry.RubricVersion != "" {
		parts = append(parts, "Rubric Version: "+entry.RubricVersion)
	}

	if entry.TaskType != "" {
		parts = append(parts, "Task Type: "+entry.TaskType)
	}

	if entry.Difficulty != "" {
		parts = append(parts, "Difficulty: "+entry.Difficulty)
	}

	if entry.ExpectedOutcome != "" {
		parts = append(parts, "Expected Outcome: "+entry.ExpectedOutcome)
	}

	if entry.Model != "" {
		parts = append(parts, "Model: "+entry.Model)
	}

	if entry.AgentVersion != "" {
		parts = append(parts, "Agent Version: "+entry.AgentVersion)
	}

	if entry.SchemaVersion != 0 {
		parts = append(parts, fmt.Sprintf("Schema Version: %d", entry.SchemaVersion))
	}

	if entry.DurationMillis != 0 {
		parts = append(parts, fmt.Sprintf("Duration Millis: %d", entry.DurationMillis))
	}

	if entry.Cost != 0 {
		parts = append(parts, fmt.Sprintf("Cost: %.6f", entry.Cost))
	}

	if entry.Confidence != 0 {
		parts = append(parts, fmt.Sprintf("Confidence: %.2f", entry.Confidence))
	}

	return parts
}

func matchesArtifact(entry *Artifact, normalizedQuery string) bool {
	return strings.Contains(strings.ToLower(entry.Path), normalizedQuery) ||
		strings.Contains(strings.ToLower(entry.LogicalPath), normalizedQuery) ||
		strings.Contains(strings.ToLower(entry.Kind), normalizedQuery) ||
		strings.Contains(strings.ToLower(entry.Summary), normalizedQuery) ||
		strings.Contains(strings.ToLower(entry.SourceAgent), normalizedQuery) ||
		strings.Contains(strings.ToLower(entry.SourceSessionID), normalizedQuery) ||
		strings.Contains(strings.ToLower(entry.SourceCommand), normalizedQuery) ||
		strings.Contains(strings.ToLower(entry.SourceTool), normalizedQuery) ||
		strings.Contains(strings.ToLower(entry.SourceCommit), normalizedQuery) ||
		strings.Contains(strings.ToLower(entry.WorktreePath), normalizedQuery) ||
		strings.Contains(strings.ToLower(entry.WorktreeBranch), normalizedQuery) ||
		strings.Contains(strings.ToLower(entry.WorktreeBase), normalizedQuery) ||
		strings.Contains(strings.ToLower(entry.SHA256), normalizedQuery) ||
		strings.Contains(strings.ToLower(entry.ReviewStatus), normalizedQuery)
}

func artifactSearchText(entry *Artifact) string {
	parts := []string{"Artifact: " + entry.Path, "Kind: " + entry.Kind}
	if entry.LogicalPath != "" && entry.LogicalPath != entry.Path {
		parts = append(parts, "Logical Path: "+entry.LogicalPath)
	}

	artifactTextFields := []struct {
		label string
		value string
	}{
		{label: "Summary", value: entry.Summary},
		{label: "Source Agent", value: entry.SourceAgent},
		{label: "Source Session", value: entry.SourceSessionID},
		{label: "Source Command", value: entry.SourceCommand},
		{label: "Source Tool", value: entry.SourceTool},
		{label: "Source Commit", value: entry.SourceCommit},
		{label: "Worktree", value: entry.WorktreePath},
		{label: "Worktree Branch", value: entry.WorktreeBranch},
		{label: "Worktree Base", value: entry.WorktreeBase},
		{label: "SHA256", value: entry.SHA256},
		{label: "Review Status", value: entry.ReviewStatus},
	}

	for _, field := range artifactTextFields {
		if field.value != "" {
			parts = append(parts, field.label+": "+field.value)
		}
	}

	return strings.Join(parts, " | ")
}

//nolint:govet // Layout prioritizes API readability over pointer-byte packing.
type rangedSnippet struct {
	Range retrieval.Range
	Text  string
}

func searchSnippet(content, query string) rangedSnippet {
	contentRunes := []rune(content)
	queryRunes := []rune(strings.ToLower(query))

	index := runeIndex([]rune(strings.ToLower(content)), queryRunes)
	if index < 0 {
		return rangedSnippet{
			Text:  compactWhitespace(content),
			Range: retrieval.Range{Unit: retrieval.RangeUnitRuneOffset, Start: 0, End: len(contentRunes)},
		}
	}

	start := max(0, index-snippetRadius)
	end := min(len(contentRunes), index+len(queryRunes)+snippetRadius)

	snippet := strings.TrimSpace(string(contentRunes[start:end]))
	if start > 0 {
		snippet = "…" + snippet
	}

	if end < len(contentRunes) {
		snippet += "…"
	}

	return rangedSnippet{
		Text:  compactWhitespace(snippet),
		Range: retrieval.Range{Unit: retrieval.RangeUnitRuneOffset, Start: start, End: end},
	}
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

func compactWhitespace(value string) string {
	return strings.Join(strings.Fields(value), " ")
}
