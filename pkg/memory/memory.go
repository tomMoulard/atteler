// Package memory provides dependency-free local text indexing and lexical search
// primitives for small retrieval-augmented workflows.
package memory

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/tommoulard/atteler/pkg/retrieval"
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

	metadata := map[string]string{
		"source_type": string(retrieval.SourceFile),
		"path":        clean,
	}

	if info, statErr := os.Stat(path); statErr == nil && !info.ModTime().IsZero() {
		metadata[retrieval.MetadataSourceUpdatedAt] = info.ModTime().UTC().Format(time.RFC3339Nano)
	}

	return s.Add(Document{ID: clean, Path: clean, Text: string(data), Metadata: metadata})
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

	doc = prepareDocument(doc)
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

// SearchRetrieval returns lexical memory hits using the shared retrieval
// contract. It preserves Search's ranking while adding source, chunk/range,
// freshness, safety, and scorer explanation metadata.
func (s *Store) SearchRetrieval(ctx context.Context, query retrieval.Query) ([]retrieval.Result, error) {
	if err := ctx.Err(); err != nil {
		return nil, fmt.Errorf("memory retrieval: %w", err)
	}

	results, err := s.Search(query.Text, query.Limit)
	if err != nil {
		return nil, err
	}

	out := make([]retrieval.Result, 0, len(results))
	for _, result := range results {
		if err := ctx.Err(); err != nil {
			return nil, fmt.Errorf("memory retrieval: %w", err)
		}

		out = append(out, retrievalResult(result, query))
	}

	return out, nil
}

// Delete removes a document by ID and reports whether anything was removed.
func (s *Store) Delete(id string) bool {
	id = strings.TrimSpace(id)
	if id == "" {
		return false
	}

	for i, doc := range s.Documents {
		if doc.ID != id {
			continue
		}

		s.Documents = append(s.Documents[:i], s.Documents[i+1:]...)

		return true
	}

	return false
}

// SyncFiles incrementally indexes exactly paths and deletes file-backed memory
// documents that are no longer present in paths.
func (s *Store) SyncFiles(paths ...string) error {
	keep := make(map[string]struct{}, len(paths))
	for _, path := range paths {
		clean := filepath.Clean(path)
		keep[clean] = struct{}{}

		if err := s.AddFile(path); err != nil {
			return err
		}
	}

	filtered := s.Documents[:0]
	for _, doc := range s.Documents {
		if doc.Metadata["source_type"] != string(retrieval.SourceFile) {
			filtered = append(filtered, doc)
			continue
		}

		if _, ok := keep[doc.ID]; ok {
			filtered = append(filtered, doc)
		}
	}

	s.Documents = filtered

	return nil
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

	for i := range store.Documents {
		store.Documents[i] = prepareDocument(store.Documents[i])
	}

	return &store, nil
}

func prepareDocument(doc Document) Document {
	source := documentSource(doc)
	policyContext := retrieval.PolicyContext{
		Source:     source,
		Metadata:   doc.Metadata,
		DocumentID: doc.ID,
		Path:       firstNonEmpty(doc.Path, doc.Metadata["path"]),
	}
	text, textSafety := retrieval.Sanitize(doc.Text, policyContext)
	metadata, metadataSafety := retrieval.SanitizeMetadata(doc.Metadata, policyContext)
	safety := retrieval.MergeSafety(textSafety, metadataSafety)

	doc.Text = text
	doc.Metadata = metadata

	if doc.Metadata == nil {
		doc.Metadata = make(map[string]string)
	}

	if _, ok := doc.Metadata[retrieval.MetadataStableID]; !ok {
		doc.Metadata[retrieval.MetadataStableID] = retrieval.StableDocumentID(source, doc.ID)
	}

	doc.Metadata[retrieval.MetadataContentHash] = retrieval.TextHash(doc.Text)
	if !retrieval.IsDefaultSafety(safety) {
		doc.Metadata = retrieval.MergeSafetyMetadata(doc.Metadata, safety)
	}

	return doc
}

func retrievalResult(result Result, query retrieval.Query) retrieval.Result {
	doc := result.Document
	metadata := cloneMetadata(doc.Metadata)
	source := documentSource(doc)
	policyContext := retrieval.PolicyContext{
		Source:     source,
		Metadata:   metadata,
		DocumentID: doc.ID,
		Path:       firstNonEmpty(doc.Path, metadata["path"]),
	}
	text, textSafety := retrieval.Sanitize(doc.Text, policyContext)
	metadata, metadataSafety := retrieval.SanitizeMetadata(metadata, policyContext)
	safety := retrieval.MergeSafety(retrieval.SafetyFromMetadata(metadata), textSafety)
	safety = retrieval.MergeSafety(safety, metadataSafety)
	sanitized := text != doc.Text
	doc.Text = text
	doc.Metadata = metadata

	if !retrieval.IsDefaultSafety(safety) {
		doc.Metadata = retrieval.MergeSafetyMetadata(doc.Metadata, safety)
	}

	chunk := retrieval.BestChunkForTerms(doc.ID, doc.Text, result.Matches, retrieval.ChunkOptions{})

	snippet := result.Snippet
	if snippet == "" || sanitized {
		snippet = retrieval.Snippet(chunk.Text, defaultSnippetRunes)
	}

	scorer := retrieval.Scorer{
		Name: "lexical-token-overlap",
		Raw:  result.Score,
		Details: map[string]float64{
			"matches":     float64(len(result.Matches)),
			"query_terms": float64(len(uniqueTokens(query.Text))),
		},
	}
	if query.Explain {
		scorer.Explanation = []string{
			"ranked by query-token coverage plus in-document match density",
			"matched terms: " + strings.Join(result.Matches, ", "),
		}
	}

	return retrieval.NormalizeResult(retrieval.Result{
		Source:     source,
		DocumentID: doc.ID,
		Chunk:      chunk.Chunk,
		Score:      retrieval.NormalizeRawScore(result.Score),
		Scorer:     scorer,
		Snippet:    snippet,
		Metadata:   cloneMetadata(doc.Metadata),
		Freshness:  retrieval.FreshnessFromMetadata(doc.Metadata),
		Safety:     retrieval.SafetyFromMetadata(doc.Metadata),
	})
}

func documentSource(doc Document) retrieval.Source {
	sourceType := retrieval.SourceMemory
	switch retrieval.SourceType(strings.TrimSpace(doc.Metadata["source_type"])) {
	case retrieval.SourceSession:
		sourceType = retrieval.SourceSession
	case retrieval.SourceFile:
		sourceType = retrieval.SourceFile
	case retrieval.SourceMemory:
		sourceType = retrieval.SourceMemory
	}

	source := retrieval.Source{Type: sourceType}
	switch sourceType {
	case retrieval.SourceSession:
		source.Name = doc.Metadata["session_id"]
		source.URI = doc.Path
	case retrieval.SourceFile:
		source.Name = doc.Path
		source.URI = doc.Path
	default:
		source.Name = doc.Metadata["kind"]
		source.URI = doc.Path
	}

	return source
}

func cloneMetadata(metadata map[string]string) map[string]string {
	if len(metadata) == 0 {
		return nil
	}

	out := make(map[string]string, len(metadata))
	maps.Copy(out, metadata)

	return out
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			return value
		}
	}

	return ""
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
