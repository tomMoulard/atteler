// Package githistory provides git-backed history collection plus parsing and
// explainable search over collected commits.
//
//nolint:cyclop,govet,wsl_v5 // Retrieval result assembly and exported structs prioritize contract readability.
package githistory

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"
	"unicode"

	"github.com/tommoulard/atteler/pkg/retrieval"
)

const (
	fieldSeparator  = "\x1f"
	recordSeparator = "\x1e"
	maxSnippets     = 3
	snippetRadius   = 80
)

// Commit is one collected git history entry.
type Commit struct {
	Date          time.Time
	Hash          string
	AuthorName    string
	AuthorEmail   string
	Subject       string
	Body          string
	Files         []string
	Changes       []ChangedFile
	Relations     CommitRelations
	Diff          string
	DiffTruncated bool
	Refs          []string
}

// ChangedFile describes one path touched by a commit.
type ChangedFile struct {
	Path    string
	OldPath string
	Status  string
	Added   int
	Deleted int
	Binary  bool
	Renamed bool
}

// CommitRelations captures history relationships inferred from commit
// metadata and messages.
type CommitRelations struct {
	Reverts   []string
	IssueRefs []string
	PRRefs    []string
	Fixup     bool
	Squash    bool
}

// Snippet is a representative matching excerpt from a commit.
type Snippet struct {
	Field string
	Text  string
}

// MatchEvidence records why a commit matched a query.
type MatchEvidence struct {
	Field  string
	Term   string
	Text   string
	Weight int
}

// Result is one ranked commit search match.
type Result struct {
	Commit       Commit
	Snippets     []Snippet
	Matches      []MatchEvidence
	RangeContext string
	Confidence   float64
	Score        int
}

// Index is an in-memory explainable index over collected commits.
type Index struct {
	commits []indexedCommit
}

type indexedCommit struct {
	commit Commit
	order  int
}

// ParseLog parses the legacy caller-captured git log format. New code should
// prefer Collect so git range, path, author, date, rename, and diff semantics are
// owned by this package instead of reconstructed by callers.
func ParseLog(text string) ([]Commit, error) {
	text = strings.ReplaceAll(text, "\r\n", "\n")
	text = strings.ReplaceAll(text, recordSeparator, "\n")

	var (
		commits []Commit
		current *Commit
	)

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

// Search ranks commits matching query across subject, body, files, author,
// relations, and optional bounded diff context.
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

		score, snippets, matches := scoreCommit(entry.commit, terms, normalizedQuery, query)
		if score == 0 {
			continue
		}

		results = append(results, rankedResult{
			result: Result{
				Commit:       cloneCommit(entry.commit),
				Snippets:     snippets,
				Matches:      matches,
				RangeContext: rangeContext(entry.commit),
				Confidence:   confidenceForScore(score, matches),
				Score:        score,
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

// SearchRetrieval returns git history matches using the shared retrieval
// contract.
func (idx *Index) SearchRetrieval(ctx context.Context, query retrieval.Query) ([]retrieval.Result, error) {
	if err := ctx.Err(); err != nil {
		return nil, fmt.Errorf("githistory retrieval: %w", err)
	}

	results := idx.Search(query.Text, query.Limit)
	out := make([]retrieval.Result, 0, len(results))

	for i := range results {
		if err := ctx.Err(); err != nil {
			return nil, fmt.Errorf("githistory retrieval: %w", err)
		}

		result := results[i]
		out = append(out, gitRetrievalResult(result, query))
	}

	return out, nil
}

type rankedResult struct {
	result Result
	order  int
}

func gitRetrievalResult(result Result, query retrieval.Query) retrieval.Result {
	commit := result.Commit
	documentID := commit.Hash
	source := retrieval.Source{Type: retrieval.SourceGitHistory, Name: "git log"}
	text, safety := retrieval.Sanitize(commitText(commit), retrieval.PolicyContext{
		Source:     source,
		DocumentID: documentID,
	})
	chunk := retrieval.BestChunkForTerms(documentID, text, tokenize(query.Text), retrieval.ChunkOptions{})

	snippet := retrieval.Snippet(chunk.Text, snippetRadius*2)
	if len(result.Snippets) > 0 && result.Snippets[0].Text != "" {
		var snippetSafety retrieval.Safety

		snippet, snippetSafety = retrieval.Sanitize(result.Snippets[0].Text, retrieval.PolicyContext{
			Source:     source,
			DocumentID: documentID,
		})
		safety = retrieval.MergeSafety(safety, snippetSafety)
	}

	metadata := map[string]string{
		"hash": commit.Hash,
	}

	if subject := sanitizedGitMetadata(commit.Subject, source, documentID, "subject", &safety); subject != "" {
		metadata["subject"] = subject
	}

	if commit.AuthorName != "" {
		metadata["author_name"] = sanitizedGitMetadata(commit.AuthorName, source, documentID, "author_name", &safety)
	}

	if commit.AuthorEmail != "" {
		metadata["author_email"] = sanitizedGitMetadata(commit.AuthorEmail, source, documentID, "author_email", &safety)
	}

	if len(commit.Files) > 0 {
		metadata["files"] = sanitizedGitMetadata(strings.Join(commit.Files, "\n"), source, documentID, "files", &safety)
	}
	if len(commit.Refs) > 0 {
		metadata["refs"] = sanitizedGitMetadata(strings.Join(commit.Refs, "\n"), source, documentID, "refs", &safety)
	}
	if commit.DiffTruncated {
		metadata["diff_truncated"] = "true"
	}

	if len(result.Snippets) > 0 {
		metadata["matched_field"] = result.Snippets[0].Field
	}
	if fields := matchedFields(result.Matches); fields != "" {
		metadata["matched_fields"] = fields
	}
	if result.RangeContext != "" {
		metadata["range_context"] = result.RangeContext
	}
	if result.Confidence > 0 {
		metadata["confidence"] = fmt.Sprintf("%.2f", result.Confidence)
	}
	if len(commit.Changes) > 0 {
		metadata["changed_files"] = sanitizedGitMetadata(changedFilesMetadata(commit.Changes), source, documentID, "changed_files", &safety)
	}
	if len(commit.Relations.Reverts) > 0 {
		metadata["reverts"] = strings.Join(commit.Relations.Reverts, "\n")
	}
	if len(commit.Relations.IssueRefs) > 0 {
		metadata["issue_refs"] = strings.Join(commit.Relations.IssueRefs, "\n")
	}
	if len(commit.Relations.PRRefs) > 0 {
		metadata["pr_refs"] = strings.Join(commit.Relations.PRRefs, "\n")
	}

	var metadataSafety retrieval.Safety

	metadata, metadataSafety = retrieval.SanitizeMetadata(metadata, retrieval.PolicyContext{
		Source:     source,
		Metadata:   metadata,
		DocumentID: documentID,
	})
	safety = retrieval.MergeSafety(safety, metadataSafety)

	if !retrieval.IsDefaultSafety(safety) {
		metadata = retrieval.MergeSafetyMetadata(metadata, safety)
	}

	scorer := retrieval.Scorer{
		Name: "git-history-explainable-weighted",
		Raw:  float64(result.Score),
		Details: map[string]float64{
			"weighted_score": float64(result.Score),
			"confidence":     result.Confidence,
		},
	}
	if query.Explain {
		scorer.Explanation = rankingExplanation(result)
	}

	return retrieval.NormalizeResult(retrieval.Result{
		Source:     source,
		DocumentID: documentID,
		Chunk:      chunk.Chunk,
		Score:      retrieval.NormalizeRawScore(float64(result.Score)),
		Scorer:     scorer,
		Snippet:    snippet,
		Metadata:   metadata,
		Freshness: retrieval.Freshness{
			SourceUpdatedAt: commit.Date,
			Status:          "current",
		},
		Safety: safety,
	})
}

func sanitizedGitMetadata(value string, source retrieval.Source, documentID, field string, safety *retrieval.Safety) string {
	sanitized, fieldSafety := retrieval.Sanitize(value, retrieval.PolicyContext{
		Source:     source,
		Metadata:   map[string]string{"field": field},
		DocumentID: documentID,
	})
	*safety = retrieval.MergeSafety(*safety, fieldSafety)

	return sanitized
}

func commitText(commit Commit) string {
	var parts []string
	if commit.Subject != "" {
		parts = append(parts, commit.Subject)
	}

	if commit.Body != "" {
		parts = append(parts, commit.Body)
	}

	if len(commit.Files) > 0 {
		parts = append(parts, strings.Join(commit.Files, "\n"))
	}
	if len(commit.Changes) > 0 {
		parts = append(parts, changedFilesMetadata(commit.Changes))
	}
	if commit.Diff != "" {
		parts = append(parts, commit.Diff)
	}
	if relations := relationsText(commit.Relations); relations != "" {
		parts = append(parts, relations)
	}

	if commit.AuthorName != "" || commit.AuthorEmail != "" {
		parts = append(parts, commit.AuthorName+" "+commit.AuthorEmail)
	}

	if commit.Hash != "" {
		parts = append(parts, commit.Hash)
	}

	return strings.Join(parts, "\n")
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

func scoreCommit(commit Commit, terms []string, normalizedQuery, originalQuery string) (int, []Snippet, []MatchEvidence) {
	fields := []searchField{
		{name: "subject", text: commit.Subject, weight: 40},
		{name: "body", text: commit.Body, weight: 30},
		{name: "files", text: strings.Join(commit.Files, "\n") + "\n" + changedFilesMetadata(commit.Changes), weight: 25},
		{name: "relations", text: relationsText(commit.Relations), weight: 35},
		{name: "diff", text: commit.Diff, weight: 15},
		{name: "author", text: commit.AuthorName + " " + commit.AuthorEmail, weight: 15},
		{name: "hash", text: commit.Hash, weight: 5},
	}

	var score int

	snippets := make([]Snippet, 0, maxSnippets)
	matches := make([]MatchEvidence, 0)

	for _, field := range fields {
		normalizedText := normalize(field.text)
		if normalizedText == "" {
			continue
		}

		fieldScore := 0
		if strings.Contains(normalizedText, normalizedQuery) {
			fieldScore += field.weight * 2
			matches = append(matches, MatchEvidence{
				Field:  field.name,
				Term:   originalQuery,
				Text:   makeSnippet(field.text, originalQuery, terms),
				Weight: field.weight * 2,
			})
		}

		for _, term := range terms {
			if strings.Contains(normalizedText, term) {
				fieldScore += field.weight
				matches = append(matches, MatchEvidence{
					Field:  field.name,
					Term:   term,
					Text:   makeSnippet(field.text, term, []string{term}),
					Weight: field.weight,
				})
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

	return score, snippets, matches
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
	commit.Changes = append([]ChangedFile(nil), commit.Changes...)
	commit.Relations.Reverts = append([]string(nil), commit.Relations.Reverts...)
	commit.Relations.IssueRefs = append([]string(nil), commit.Relations.IssueRefs...)
	commit.Relations.PRRefs = append([]string(nil), commit.Relations.PRRefs...)
	commit.Refs = append([]string(nil), commit.Refs...)
	return commit
}
