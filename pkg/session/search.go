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
		if entry.IsDir() || filepath.Ext(entry.Name()) != ".json" {
			continue
		}

		path := filepath.Join(s.dir, entry.Name())
		session, err := s.Load(path)
		if err != nil {
			return nil, err
		}
		result, ok := matchSession(summarize(path, session), session.Messages, query, normalizedQuery)
		if ok {
			results = append(results, result)
		}
	}

	sort.Slice(results, func(i, j int) bool {
		return results[i].Summary.UpdatedAt.After(results[j].Summary.UpdatedAt)
	})
	return results, nil
}

func matchSession(summary Summary, messages []llm.Message, query, normalizedQuery string) (SearchResult, bool) {
	result := SearchResult{Summary: summary}
	matched := strings.Contains(strings.ToLower(summary.ID), normalizedQuery) ||
		strings.Contains(strings.ToLower(summary.Title), normalizedQuery) ||
		strings.Contains(strings.ToLower(summary.DefaultAgent), normalizedQuery) ||
		strings.Contains(strings.ToLower(summary.DefaultModel), normalizedQuery) ||
		containsTag(summary.Tags, normalizedQuery)

	for _, message := range messages {
		if !matchesMessage(message, normalizedQuery) {
			continue
		}
		matched = true
		if len(result.Snippets) < maxSnippetsPerSession {
			result.Snippets = append(result.Snippets, SearchSnippet{
				Role: message.Role,
				Text: searchSnippet(message.Content, query, snippetRadius),
			})
		}
	}
	return result, matched
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

func searchSnippet(content, query string, radius int) string {
	contentRunes := []rune(content)
	queryRunes := []rune(strings.ToLower(query))
	index := runeIndex([]rune(strings.ToLower(content)), queryRunes)
	if index < 0 {
		return compactWhitespace(content)
	}

	start := max(0, index-radius)
	end := min(len(contentRunes), index+len(queryRunes)+radius)
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
