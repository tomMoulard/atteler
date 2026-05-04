// Package memory provides dependency-free local text indexing and lexical search
// primitives for small retrieval-augmented workflows.
package memory

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"unicode"
	"unicode/utf8"
)

const defaultSnippetRunes = 160

// Document is a text item indexed by Store.
type Document struct {
	Metadata map[string]string `json:"metadata,omitempty"`
	ID       string            `json:"id"`
	Path     string            `json:"path,omitempty"`
	Text     string            `json:"text"`
}

// Store is a JSON-serializable collection of text documents.
type Store struct {
	Documents []Document `json:"documents"`
}

// Result is a ranked lexical match returned by Search.
//
//nolint:govet // Layout prioritizes JSON/API readability over pointer-byte packing.
type Result struct {
	Score    float64  `json:"score"`
	Document Document `json:"document"`
	Matches  []string `json:"matches"`
	Snippet  string   `json:"snippet"`
}

// NewStore returns an empty in-memory document store.
func NewStore() *Store {
	return &Store{}
}

// Tokenize splits text into lowercase unicode letter/digit tokens.
func Tokenize(text string) []string {
	return tokenize(text)
}

// AddText indexes text under id. Existing documents with the same id are replaced.
func (s *Store) AddText(id, text string) error {
	return s.Add(Document{ID: id, Text: text})
}

// IndexText is an alias for AddText.
func (s *Store) IndexText(id, text string) error {
	return s.AddText(id, text)
}

// AddFile reads and indexes a UTF-8 text file. The filepath is used as both ID
// and Path so callers can round-trip search results back to the source file.
func (s *Store) AddFile(path string) error {
	data, err := os.ReadFile(path)
	if err != nil {
		return fmt.Errorf("read memory file %q: %w", path, err)
	}

	if !utf8.Valid(data) {
		return fmt.Errorf("read memory file %q: %w", path, ErrInvalidUTF8)
	}

	clean := filepath.Clean(path)

	return s.Add(Document{ID: clean, Path: clean, Text: string(data)})
}

// AddFiles reads and indexes each path in order.
func (s *Store) AddFiles(paths ...string) error {
	for _, path := range paths {
		if err := s.AddFile(path); err != nil {
			return err
		}
	}

	return nil
}

// IndexFile is an alias for AddFile.
func (s *Store) IndexFile(path string) error {
	return s.AddFile(path)
}

// IndexFiles is an alias for AddFiles.
func (s *Store) IndexFiles(paths ...string) error {
	return s.AddFiles(paths...)
}

// Add indexes doc. Existing documents with the same id are replaced.
func (s *Store) Add(doc Document) error {
	doc.ID = strings.TrimSpace(doc.ID)
	if doc.ID == "" {
		return ErrMissingID
	}

	if doc.Metadata != nil && len(doc.Metadata) == 0 {
		doc.Metadata = nil
	}

	for i, existing := range s.Documents {
		if existing.ID == doc.ID {
			s.Documents[i] = doc
			return nil
		}
	}

	s.Documents = append(s.Documents, doc)

	return nil
}

// Index is an alias for Add.
func (s *Store) Index(doc Document) error {
	return s.Add(doc)
}

// Search ranks documents by lexical overlap with query and returns up to limit
// results. A limit less than one returns every matching document.
func (s *Store) Search(query string, limit int) ([]Result, error) {
	queryTerms := uniqueTokens(query)
	if len(queryTerms) == 0 {
		return nil, ErrEmptyQuery
	}

	querySet := make(map[string]struct{}, len(queryTerms))
	for _, term := range queryTerms {
		querySet[term] = struct{}{}
	}

	results := make([]Result, 0, len(s.Documents))
	for _, doc := range s.Documents {
		tokens := tokenize(doc.Text)
		if len(tokens) == 0 {
			continue
		}

		counts := make(map[string]int)

		for _, token := range tokens {
			if _, ok := querySet[token]; ok {
				counts[token]++
			}
		}

		if len(counts) == 0 {
			continue
		}

		matches := sortedKeys(counts)
		results = append(results, Result{
			Document: doc,
			Score:    score(counts, len(queryTerms), len(tokens)),
			Snippet:  snippet(doc.Text, matches, defaultSnippetRunes),
			Matches:  matches,
		})
	}

	sort.SliceStable(results, func(i, j int) bool {
		if results[i].Score != results[j].Score {
			return results[i].Score > results[j].Score
		}

		return results[i].Document.ID < results[j].Document.ID
	})

	if limit > 0 && len(results) > limit {
		results = results[:limit]
	}

	return results, nil
}

// Save writes the store as pretty-printed JSON.
func (s *Store) Save(path string) error {
	data, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal memory store: %w", err)
	}

	data = append(data, '\n')

	if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
		return fmt.Errorf("create memory store dir: %w", err)
	}

	if err := os.WriteFile(path, data, 0o600); err != nil {
		return fmt.Errorf("write memory store %q: %w", path, err)
	}

	return nil
}

// Load reads a JSON store saved by Save.
func Load(path string) (*Store, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read memory store %q: %w", path, err)
	}

	var store Store
	if err := json.Unmarshal(data, &store); err != nil {
		return nil, fmt.Errorf("decode memory store %q: %w", path, err)
	}

	return &store, nil
}

var (
	// ErrMissingID is returned when indexing a document without an ID.
	ErrMissingID = errors.New("memory document id is required")
	// ErrEmptyQuery is returned when a search query has no tokens.
	ErrEmptyQuery = errors.New("memory search query is empty")
	// ErrInvalidUTF8 is returned when AddFile reads non-UTF-8 content.
	ErrInvalidUTF8 = errors.New("memory file is not valid UTF-8")
)

func tokenize(text string) []string {
	var (
		tokens []string
		b      strings.Builder
	)

	for _, r := range text {
		if unicode.IsLetter(r) || unicode.IsDigit(r) {
			b.WriteRune(unicode.ToLower(r))
			continue
		}

		if b.Len() > 0 {
			tokens = append(tokens, b.String())
			b.Reset()
		}
	}

	if b.Len() > 0 {
		tokens = append(tokens, b.String())
	}

	return tokens
}

func uniqueTokens(text string) []string {
	seen := make(map[string]struct{})

	var terms []string

	for _, token := range tokenize(text) {
		if _, ok := seen[token]; ok {
			continue
		}

		seen[token] = struct{}{}
		terms = append(terms, token)
	}

	return terms
}

func sortedKeys(counts map[string]int) []string {
	keys := make([]string, 0, len(counts))
	for key := range counts {
		keys = append(keys, key)
	}

	sort.Strings(keys)

	return keys
}

func score(counts map[string]int, queryTerms, documentTokens int) float64 {
	var frequency int
	for _, count := range counts {
		frequency += count
	}

	coverage := float64(len(counts)) / float64(queryTerms)
	density := float64(frequency) / float64(documentTokens)

	return coverage + density
}

func snippet(text string, terms []string, maxRunes int) string {
	clean := strings.Join(strings.Fields(text), " ")
	if clean == "" || maxRunes < 1 {
		return ""
	}

	firstRune := -1

	for _, term := range terms {
		idx := runeIndexFold(clean, term)
		if idx >= 0 && (firstRune < 0 || idx < firstRune) {
			firstRune = idx
		}
	}

	start := 0
	if firstRune >= 0 {
		start = max(0, firstRune-maxRunes/3)
	}

	runes := []rune(clean)
	if start > len(runes) {
		start = 0
	}

	end := min(start+maxRunes, len(runes))

	out := strings.TrimSpace(string(runes[start:end]))
	if start > 0 {
		out = "…" + out
	}

	if end < len(runes) {
		out += "…"
	}

	return out
}

func runeIndexFold(text, term string) int {
	textRunes := []rune(strings.ToLower(text))

	termRunes := []rune(strings.ToLower(term))
	if len(termRunes) == 0 || len(termRunes) > len(textRunes) {
		return -1
	}

	for i := 0; i <= len(textRunes)-len(termRunes); i++ {
		match := true

		for j, termRune := range termRunes {
			if textRunes[i+j] != termRune {
				match = false
				break
			}
		}

		if match {
			return i
		}
	}

	return -1
}
