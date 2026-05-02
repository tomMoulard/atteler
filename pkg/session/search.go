package session

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"

	"github.com/tommoulard/atteler/pkg/llm"
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

// SearchSnippet is a matching transcript excerpt.
type SearchSnippet struct {
	Role llm.Role
	Text string
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

func messageSnippets(messages []llm.Message, query, normalizedQuery string) []SearchSnippet {
	snippets := make([]SearchSnippet, 0, maxSnippetsPerSession)
	for _, message := range messages {
		if !matchesMessage(message, normalizedQuery) {
			continue
		}
		snippets = append(snippets, SearchSnippet{
			Role: message.Role,
			Text: searchSnippet(message.Content, query),
		})
		if len(snippets) >= maxSnippetsPerSession {
			return snippets
		}
	}
	return snippets
}

func negativeKnowledgeSnippets(entries []NegativeKnowledge, query, normalizedQuery string) []SearchSnippet {
	snippets := make([]SearchSnippet, 0, maxSnippetsPerSession)
	for _, entry := range entries {
		if !matchesNegativeKnowledge(entry, normalizedQuery) {
			continue
		}
		snippets = append(snippets, SearchSnippet{
			Role: llm.Role("negative_knowledge"),
			Text: searchSnippet(negativeKnowledgeSearchText(entry), query),
		})
		if len(snippets) >= maxSnippetsPerSession {
			return snippets
		}
	}
	return snippets
}

func evaluationSnippets(entries []AgentEvaluation, query, normalizedQuery string) []SearchSnippet {
	snippets := make([]SearchSnippet, 0, maxSnippetsPerSession)
	for _, entry := range entries {
		if !matchesEvaluation(entry, normalizedQuery) {
			continue
		}
		snippets = append(snippets, SearchSnippet{
			Role: llm.Role("evaluation"),
			Text: searchSnippet(evaluationSearchText(entry), query),
		})
		if len(snippets) >= maxSnippetsPerSession {
			return snippets
		}
	}
	return snippets
}

func artifactSnippets(entries []Artifact, query, normalizedQuery string) []SearchSnippet {
	snippets := make([]SearchSnippet, 0, maxSnippetsPerSession)
	for _, entry := range entries {
		if !matchesArtifact(entry, normalizedQuery) {
			continue
		}
		snippets = append(snippets, SearchSnippet{
			Role: llm.Role("artifact"),
			Text: searchSnippet(artifactSearchText(entry), query),
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
		strings.Contains(strings.ToLower(entry.Agent), normalizedQuery)
}

func negativeKnowledgeSearchText(entry NegativeKnowledge) string {
	parts := []string{"Failed attempt: " + entry.Approach, "Reason: " + entry.Reason}
	if entry.Commit != "" {
		parts = append(parts, "Commit: "+entry.Commit)
	}
	if entry.Agent != "" {
		parts = append(parts, "Agent: "+entry.Agent)
	}
	return strings.Join(parts, " | ")
}

func matchesEvaluation(entry AgentEvaluation, normalizedQuery string) bool {
	return strings.Contains(strings.ToLower(entry.Agent), normalizedQuery) ||
		strings.Contains(strings.ToLower(entry.Outcome), normalizedQuery) ||
		strings.Contains(strings.ToLower(entry.Notes), normalizedQuery) ||
		strings.Contains(strings.ToLower(entry.Reference), normalizedQuery)
}

func evaluationSearchText(entry AgentEvaluation) string {
	parts := []string{"Evaluation: " + entry.Agent, "Outcome: " + entry.Outcome}
	if entry.Score != 0 {
		parts = append(parts, fmt.Sprintf("Score: %d", entry.Score))
	}
	if entry.Reference != "" {
		parts = append(parts, "Reference: "+entry.Reference)
	}
	if entry.Notes != "" {
		parts = append(parts, "Notes: "+entry.Notes)
	}
	return strings.Join(parts, " | ")
}

func matchesArtifact(entry Artifact, normalizedQuery string) bool {
	return strings.Contains(strings.ToLower(entry.Path), normalizedQuery) ||
		strings.Contains(strings.ToLower(entry.Kind), normalizedQuery) ||
		strings.Contains(strings.ToLower(entry.Summary), normalizedQuery) ||
		strings.Contains(strings.ToLower(entry.SourceAgent), normalizedQuery)
}

func artifactSearchText(entry Artifact) string {
	parts := []string{"Artifact: " + entry.Path, "Kind: " + entry.Kind}
	if entry.Summary != "" {
		parts = append(parts, "Summary: "+entry.Summary)
	}
	if entry.SourceAgent != "" {
		parts = append(parts, "Source Agent: "+entry.SourceAgent)
	}
	return strings.Join(parts, " | ")
}

func searchSnippet(content, query string) string {
	contentRunes := []rune(content)
	queryRunes := []rune(strings.ToLower(query))
	index := runeIndex([]rune(strings.ToLower(content)), queryRunes)
	if index < 0 {
		return compactWhitespace(content)
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

func compactWhitespace(value string) string {
	return strings.Join(strings.Fields(value), " ")
}
