// Package githistory provides dependency-free parsing and lexical search for
// git history text captured by callers.
package githistory

import (
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"
	"unicode"
)

const (
	fieldSeparator  = "\x1f"
	recordSeparator = "\x1e"
	maxSnippets     = 3
	snippetRadius   = 80
)

// Commit is one git history entry parsed from git log text.
type Commit struct {
	Date        time.Time
	Hash        string
	AuthorName  string
	AuthorEmail string
	Subject     string
	Body        string
	Files       []string
}

// Snippet is a representative matching excerpt from a commit.
type Snippet struct {
	Field string
	Text  string
}

// Result is one ranked commit search match.
type Result struct {
	Commit   Commit
	Snippets []Snippet
	Score    int
}

// Index is an in-memory lexical index over parsed commits.
type Index struct {
	commits []indexedCommit
}

type indexedCommit struct {
	commit Commit
	order  int
}

// ParseLog parses text produced by:
//
//	git log --name-only --date=iso-strict --pretty=format:%H%x1f%an%x1f%ae%x1f%aI%x1f%s%x1e
//
// The package never shells out to git; callers provide captured log text.
func ParseLog(text string) ([]Commit, error) {
	text = strings.ReplaceAll(text, "\r\n", "\n")
	text = strings.ReplaceAll(text, recordSeparator, "\n")

	var commits []Commit
	var current *Commit
	for lineNumber, rawLine := range strings.Split(text, "\n") {
		line := strings.TrimRight(rawLine, "\r")
		if strings.TrimSpace(line) == "" {
			continue
		}

		if isHeaderLine(line) {
			if current != nil {
				commits = append(commits, cloneCommit(*current))
			}

			commit, err := parseHeader(line)
			if err != nil {
				return nil, fmt.Errorf("githistory: parse line %d: %w", lineNumber+1, err)
			}
			current = &commit
			continue
		}

		if current == nil {
			return nil, fmt.Errorf("githistory: parse line %d: file listed before commit header", lineNumber+1)
		}
		current.Files = append(current.Files, strings.TrimSpace(line))
	}

	if current != nil {
		commits = append(commits, cloneCommit(*current))
	}
	return commits, nil
}

// NewIndex returns an in-memory search index over commits.
func NewIndex(commits []Commit) *Index {
	idx := &Index{commits: make([]indexedCommit, 0, len(commits))}
	for i := range commits {
		idx.commits = append(idx.commits, indexedCommit{
			commit: cloneCommit(commits[i]),
			order:  i,
		})
	}
	return idx
}

// Search ranks commits matching query across subject, body, files, and author.
// A limit less than one returns every match.
func (idx *Index) Search(query string, limit int) []Result {
	query = strings.TrimSpace(query)
	if idx == nil || query == "" {
		return nil
	}

	terms := tokenize(query)
	if len(terms) == 0 {
		return nil
	}
	normalizedQuery := normalize(query)

	results := make([]rankedResult, 0, len(idx.commits))
	for i := range idx.commits {
		entry := &idx.commits[i]
		score, snippets := scoreCommit(entry.commit, terms, normalizedQuery, query)
		if score == 0 {
			continue
		}
		results = append(results, rankedResult{
			result: Result{
				Commit:   cloneCommit(entry.commit),
				Snippets: snippets,
				Score:    score,
			},
			order: entry.order,
		})
	}

	sort.SliceStable(results, func(i, j int) bool {
		left, right := results[i], results[j]
		if left.result.Score != right.result.Score {
			return left.result.Score > right.result.Score
		}
		if !left.result.Commit.Date.Equal(right.result.Commit.Date) {
			return left.result.Commit.Date.After(right.result.Commit.Date)
		}
		if left.result.Commit.Hash != right.result.Commit.Hash {
			return left.result.Commit.Hash < right.result.Commit.Hash
		}
		return left.order < right.order
	})

	if limit > 0 && len(results) > limit {
		results = results[:limit]
	}

	out := make([]Result, 0, len(results))
	for i := range results {
		out = append(out, results[i].result)
	}
	return out
}

type rankedResult struct {
	result Result
	order  int
}

func parseHeader(line string) (Commit, error) {
	fields := strings.SplitN(line, fieldSeparator, 6)
	if len(fields) < 5 {
		return Commit{}, errors.New("commit header requires hash, author name, author email, date, and subject")
	}

	date, err := time.Parse(time.RFC3339, strings.TrimSpace(fields[3]))
	if err != nil {
		return Commit{}, fmt.Errorf("invalid author date: %w", err)
	}

	commit := Commit{
		Hash:        strings.TrimSpace(fields[0]),
		AuthorName:  strings.TrimSpace(fields[1]),
		AuthorEmail: strings.TrimSpace(fields[2]),
		Date:        date,
		Subject:     strings.TrimSpace(fields[4]),
	}
	if len(fields) == 6 {
		commit.Body = strings.TrimSpace(fields[5])
	}
	if commit.Hash == "" {
		return Commit{}, errors.New("commit hash is required")
	}
	return commit, nil
}

func isHeaderLine(line string) bool {
	return strings.Count(line, fieldSeparator) >= 4
}

func scoreCommit(commit Commit, terms []string, normalizedQuery, originalQuery string) (int, []Snippet) {
	fields := []searchField{
		{name: "subject", text: commit.Subject, weight: 40},
		{name: "body", text: commit.Body, weight: 30},
		{name: "files", text: strings.Join(commit.Files, "\n"), weight: 20},
		{name: "author", text: commit.AuthorName + " " + commit.AuthorEmail, weight: 15},
		{name: "hash", text: commit.Hash, weight: 5},
	}

	var score int
	snippets := make([]Snippet, 0, maxSnippets)
	for _, field := range fields {
		normalizedText := normalize(field.text)
		if normalizedText == "" {
			continue
		}

		fieldScore := 0
		if strings.Contains(normalizedText, normalizedQuery) {
			fieldScore += field.weight * 2
		}
		for _, term := range terms {
			if strings.Contains(normalizedText, term) {
				fieldScore += field.weight
			}
		}

		if fieldScore == 0 {
			continue
		}
		score += fieldScore
		if len(snippets) < maxSnippets {
			snippets = append(snippets, Snippet{
				Field: field.name,
				Text:  makeSnippet(field.text, originalQuery, terms),
			})
		}
	}
	return score, snippets
}

type searchField struct {
	name   string
	text   string
	weight int
}

func makeSnippet(text, query string, terms []string) string {
	text = strings.TrimSpace(text)
	if text == "" {
		return ""
	}

	normalizedText := normalize(text)
	normalizedQuery := normalize(query)
	index := strings.Index(normalizedText, normalizedQuery)
	if index < 0 {
		for _, term := range terms {
			index = strings.Index(normalizedText, term)
			if index >= 0 {
				break
			}
		}
	}
	if index < 0 {
		if len(text) <= snippetRadius*2 {
			return text
		}
		return text[:snippetRadius*2] + "..."
	}

	start := max(index-snippetRadius, 0)
	end := min(index+len(query)+snippetRadius, len(text))

	snippet := strings.TrimSpace(text[start:end])
	if start > 0 {
		snippet = "..." + snippet
	}
	if end < len(text) {
		snippet += "..."
	}
	return snippet
}

func tokenize(text string) []string {
	seen := make(map[string]struct{})
	var terms []string
	for _, term := range strings.FieldsFunc(normalize(text), func(r rune) bool {
		return !unicode.IsLetter(r) && !unicode.IsDigit(r)
	}) {
		if term == "" {
			continue
		}
		if _, ok := seen[term]; ok {
			continue
		}
		seen[term] = struct{}{}
		terms = append(terms, term)
	}
	sort.Strings(terms)
	return terms
}

func normalize(text string) string {
	return strings.ToLower(strings.TrimSpace(text))
}

func cloneCommit(commit Commit) Commit {
	commit.Files = append([]string(nil), commit.Files...)
	return commit
}
