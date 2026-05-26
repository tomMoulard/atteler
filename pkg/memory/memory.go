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

	"github.com/tommoulard/atteler/pkg/privacy"
	"github.com/tommoulard/atteler/pkg/retrieval"
)

const (
	defaultSnippetRunes = 160
	// StoreSchemaVersion is the current JSON persistence schema for lexical memory.
	StoreSchemaVersion = 1
	// StoreTextNormalization identifies the token normalization used by lexical search.
	StoreTextNormalization = "unicode-letter-digit-lowercase-v1"
)

// Document is a text item indexed by Store.
type Document struct {
	Metadata   map[string]string `json:"metadata,omitempty"`
	Provenance map[string]string `json:"provenance,omitempty"`
	ExpiresAt  *time.Time        `json:"expires_at,omitempty"`
	CreatedAt  time.Time         `json:"created_at,omitzero"`
	UpdatedAt  time.Time         `json:"updated_at,omitzero"`
	ID         string            `json:"id"`
	Path       string            `json:"path,omitempty"`
	Text       string            `json:"text"`
	SourceHash string            `json:"source_hash,omitempty"`
}

// Store is a JSON-serializable collection of text documents.
//
//nolint:govet // Layout prioritizes JSON/API readability over pointer-byte packing.
type Store struct {
	Documents     []Document `json:"documents"`
	CreatedAt     time.Time  `json:"created_at,omitzero"`
	UpdatedAt     time.Time  `json:"updated_at,omitzero"`
	Normalization string     `json:"normalization,omitempty"`
	SchemaVersion int        `json:"schema_version"`
}

// LoadOptions controls persisted store loading.
type LoadOptions struct {
	// Migrate redacts legacy persisted content and recomputes source hashes
	// instead of trusting or silently normalizing stale JSON.
	Migrate bool
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
	now := time.Now().UTC()

	return &Store{
		SchemaVersion: StoreSchemaVersion,
		Normalization: StoreTextNormalization,
		CreatedAt:     now,
		UpdatedAt:     now,
	}
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

	return s.Add(Document{
		ID:         clean,
		Path:       clean,
		Text:       string(data),
		Metadata:   metadata,
		Provenance: fileProvenance(clean),
	})
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
	if err := s.ensureSchema(); err != nil {
		return err
	}

	id, err := redactDocumentID(doc.ID)
	if err != nil {
		return err
	}

	doc.ID = id

	if !utf8.ValidString(doc.Text) {
		return ErrInvalidUTF8
	}

	doc.Text = privacy.RedactText(doc.Text)
	doc.Path = privacy.RedactIdentifier(strings.TrimSpace(doc.Path))
	doc.Metadata = privacy.RedactMetadata(doc.Metadata)
	doc.Provenance = ensureProvenance(cleanMetadata(doc.Provenance), "direct")
	doc.ExpiresAt = cloneTimePtr(doc.ExpiresAt)
	doc.SourceHash = privacy.SourceHash(doc.Text)

	now := time.Now().UTC()

	hasCreatedAt := !doc.CreatedAt.IsZero()
	if doc.CreatedAt.IsZero() {
		doc.CreatedAt = now
	}

	doc.UpdatedAt = now

	if s.CreatedAt.IsZero() {
		s.CreatedAt = now
	}

	s.UpdatedAt = now

	for i := range s.Documents {
		if s.Documents[i].ID == doc.ID {
			if !hasCreatedAt {
				doc.CreatedAt = s.Documents[i].CreatedAt
			}

			s.Documents[i] = doc

			return nil
		}
	}

	s.Documents = append(s.Documents, doc)

	return nil
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
	for i := range results {
		if err := ctx.Err(); err != nil {
			return nil, fmt.Errorf("memory retrieval: %w", err)
		}

		out = append(out, retrievalResult(results[i], query))
	}

	return out, nil
}

// Delete removes documents by id and reports whether anything was removed.
func (s *Store) Delete(id string) bool {
	id, err := redactDocumentID(id)
	if err != nil {
		return false
	}

	kept := s.Documents[:0]
	removed := false

	for i := range s.Documents {
		if s.Documents[i].ID == id {
			removed = true
			continue
		}

		kept = append(kept, s.Documents[i])
	}

	if removed {
		clear(s.Documents[len(kept):cap(s.Documents)])
		s.Documents = kept
		s.UpdatedAt = time.Now().UTC()

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
	for i := range s.Documents {
		doc := s.Documents[i]
		if doc.Metadata["source_type"] != string(retrieval.SourceFile) {
			filtered = append(filtered, doc)
			continue
		}

		if _, ok := keep[doc.ID]; ok {
			filtered = append(filtered, doc)
		}
	}

	if len(filtered) != len(s.Documents) {
		clear(s.Documents[len(filtered):cap(s.Documents)])
		s.UpdatedAt = time.Now().UTC()
	}

	s.Documents = filtered

	return nil
}

// Compact removes expired documents and returns the number of purged entries.
func (s *Store) Compact(now time.Time) int {
	now = now.UTC()

	kept := s.Documents[:0]
	removed := 0

	for i := range s.Documents {
		if isExpired(s.Documents[i], now) {
			removed++
			continue
		}

		kept = append(kept, s.Documents[i])
	}

	if removed > 0 {
		clear(s.Documents[len(kept):cap(s.Documents)])
		s.Documents = kept
		s.UpdatedAt = time.Now().UTC()
	}

	return removed
}

// Index is an alias for Add.
func (s *Store) Index(doc Document) error {
	return s.Add(doc)
}

// Search ranks documents by lexical overlap with query and returns up to limit
// results. A limit less than one returns every matching document.
func (s *Store) Search(query string, limit int) ([]Result, error) {
	if err := s.ensureSchema(); err != nil {
		return nil, err
	}

	queryTerms := uniqueTokens(query)
	if len(queryTerms) == 0 {
		return nil, ErrEmptyQuery
	}

	querySet := make(map[string]struct{}, len(queryTerms))
	for _, term := range queryTerms {
		querySet[term] = struct{}{}
	}

	results := make([]Result, 0, len(s.Documents))
	now := time.Now().UTC()
	seen := make(map[string]struct{}, len(s.Documents))

	for i := range s.Documents {
		doc := s.Documents[i]
		if isExpired(doc, now) {
			continue
		}

		doc.ID = strings.TrimSpace(doc.ID)
		if doc.ID == "" {
			return nil, ErrMissingID
		}

		if err := rememberDocumentID(seen, doc.ID); err != nil {
			return nil, err
		}

		result, ok, err := searchDocument(doc, queryTerms, querySet)
		if err != nil {
			return nil, err
		}

		if !ok {
			continue
		}

		results = append(results, result)
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
	s.Compact(time.Now().UTC())

	if err := s.ensureSchema(); err != nil {
		return err
	}

	if err := s.prepareForSave(); err != nil {
		return err
	}

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

	if err := os.Chmod(path, 0o600); err != nil {
		return fmt.Errorf("chmod memory store %q: %w", path, err)
	}

	return nil
}

func (s *Store) prepareForSave() error {
	now := time.Now().UTC()
	if s.CreatedAt.IsZero() {
		s.CreatedAt = now
	}

	if s.UpdatedAt.IsZero() {
		s.UpdatedAt = now
	}

	seen := make(map[string]struct{}, len(s.Documents))
	for i := range s.Documents {
		doc := s.Documents[i]

		normalized, err := s.prepareDocumentForSave(doc)
		if err != nil {
			return err
		}

		if err := rememberDocumentID(seen, normalized.ID); err != nil {
			return err
		}

		s.Documents[i] = normalized
	}

	return nil
}

func (s *Store) prepareDocumentForSave(doc Document) (Document, error) {
	doc.ID = strings.TrimSpace(doc.ID)
	if doc.ID == "" {
		return Document{}, ErrMissingID
	}

	if privacy.RedactIdentifier(doc.ID) != doc.ID {
		return Document{}, fmt.Errorf("%w: %s", ErrPrivacyPolicy, doc.ID)
	}

	if !utf8.ValidString(doc.Text) {
		return Document{}, ErrInvalidUTF8
	}

	redactedText := privacy.RedactText(doc.Text)
	if redactedText != doc.Text {
		return Document{}, fmt.Errorf("%w: %s", ErrPrivacyPolicy, doc.ID)
	}

	doc.Path = strings.TrimSpace(doc.Path)
	if redactedPath := privacy.RedactIdentifier(doc.Path); redactedPath != doc.Path {
		return Document{}, fmt.Errorf("%w: %s", ErrPrivacyPolicy, doc.ID)
	}

	redactedMetadata := privacy.RedactMetadata(doc.Metadata)
	if !maps.Equal(doc.Metadata, redactedMetadata) {
		return Document{}, fmt.Errorf("%w: %s", ErrPrivacyPolicy, doc.ID)
	}

	redactedProvenance := cleanMetadata(doc.Provenance)
	if !maps.Equal(doc.Provenance, redactedProvenance) {
		return Document{}, fmt.Errorf("%w: %s", ErrPrivacyPolicy, doc.ID)
	}

	doc.Metadata = redactedMetadata
	doc.Provenance = redactedProvenance
	doc.ExpiresAt = cloneTimePtr(doc.ExpiresAt)

	if doc.CreatedAt.IsZero() {
		doc.CreatedAt = s.CreatedAt
	}

	if doc.UpdatedAt.IsZero() {
		doc.UpdatedAt = doc.CreatedAt
	}

	hash := privacy.SourceHash(doc.Text)
	if doc.SourceHash == "" {
		return Document{}, fmt.Errorf("%w: %s", ErrSourceHashMismatch, doc.ID)
	}

	if doc.SourceHash != hash {
		return Document{}, fmt.Errorf("%w: %s", ErrSourceHashMismatch, doc.ID)
	}

	if err := validateProvenance(doc.Provenance); err != nil {
		return Document{}, fmt.Errorf("%w: %s", err, doc.ID)
	}

	return doc, nil
}

// Load reads a JSON store saved by Save.
func Load(path string) (*Store, error) {
	return LoadWithOptions(path, LoadOptions{})
}

// LoadWithOptions reads a JSON store and optionally migrates legacy persisted
// content by redacting text/metadata/provenance and recomputing source hashes.
func LoadWithOptions(path string, opts LoadOptions) (*Store, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read memory store %q: %w", path, err)
	}

	var store Store
	if err := json.Unmarshal(data, &store); err != nil {
		return nil, fmt.Errorf("decode memory store %q: %w", path, err)
	}

	if opts.Migrate {
		if err := store.Migrate(); err != nil {
			return nil, fmt.Errorf("migrate memory store %q: %w", path, err)
		}

		return &store, nil
	}

	store.Compact(time.Now().UTC())

	if err := store.validateLoaded(); err != nil {
		return nil, fmt.Errorf("validate memory store %q: %w", path, err)
	}

	return &store, nil
}

// Migrate updates a loaded legacy store to the current schema by redacting
// text/metadata/provenance, filling timestamps, and recomputing source hashes.
func (s *Store) Migrate() error {
	if s.SchemaVersion < 0 || s.SchemaVersion > StoreSchemaVersion {
		return ErrIncompatibleSchema
	}

	s.SchemaVersion = StoreSchemaVersion
	s.Normalization = StoreTextNormalization

	now := time.Now().UTC()
	if s.CreatedAt.IsZero() {
		s.CreatedAt = now
	}

	s.UpdatedAt = now
	s.Compact(now)

	seen := make(map[string]struct{}, len(s.Documents))
	for i := range s.Documents {
		doc := s.Documents[i]

		normalized, err := s.migrateDocument(doc)
		if err != nil {
			return err
		}

		if err := rememberDocumentID(seen, normalized.ID); err != nil {
			return err
		}

		s.Documents[i] = normalized
	}

	return s.validateLoaded()
}

func (s *Store) migrateDocument(doc Document) (Document, error) {
	id, err := redactDocumentID(doc.ID)
	if err != nil {
		return Document{}, ErrMissingID
	}

	doc.ID = id

	if !utf8.ValidString(doc.Text) {
		return Document{}, ErrInvalidUTF8
	}

	doc.Text = privacy.RedactText(doc.Text)
	doc.Path = privacy.RedactIdentifier(strings.TrimSpace(doc.Path))
	doc.Metadata = privacy.RedactMetadata(doc.Metadata)
	doc.Provenance = ensureProvenance(cleanMetadata(doc.Provenance), "legacy")
	doc.ExpiresAt = cloneTimePtr(doc.ExpiresAt)

	if doc.CreatedAt.IsZero() {
		doc.CreatedAt = s.CreatedAt
	}

	if doc.UpdatedAt.IsZero() {
		doc.UpdatedAt = doc.CreatedAt
	}

	doc.SourceHash = privacy.SourceHash(doc.Text)

	return doc, nil
}

func (s *Store) ensureSchema() error {
	if s.SchemaVersion == 0 {
		if len(s.Documents) > 0 {
			return ErrIncompatibleSchema
		}

		s.SchemaVersion = StoreSchemaVersion
	}

	if s.SchemaVersion != StoreSchemaVersion {
		return ErrIncompatibleSchema
	}

	normalization := strings.TrimSpace(s.Normalization)
	if normalization == "" {
		if len(s.Documents) > 0 {
			return ErrNormalizationMismatch
		}

		s.Normalization = StoreTextNormalization
		normalization = StoreTextNormalization
	}

	if normalization != StoreTextNormalization {
		if len(s.Documents) == 0 {
			s.Normalization = StoreTextNormalization

			return nil
		}

		return ErrNormalizationMismatch
	}

	return nil
}

var (
	// ErrMissingID is returned when indexing a document without an ID.
	ErrMissingID = errors.New("memory document id is required")
	// ErrDuplicateID is returned when persisted JSON has duplicate document IDs.
	ErrDuplicateID = errors.New("memory duplicate document id")
	// ErrEmptyQuery is returned when a search query has no tokens.
	ErrEmptyQuery = errors.New("memory search query is empty")
	// ErrInvalidUTF8 is returned when AddFile reads non-UTF-8 content.
	ErrInvalidUTF8 = errors.New("memory file is not valid UTF-8")
	// ErrIncompatibleSchema is returned when a persisted store is newer than this code.
	ErrIncompatibleSchema = errors.New("memory store schema version is incompatible")
	// ErrSourceHashMismatch is returned when persisted text and source hash disagree.
	ErrSourceHashMismatch = errors.New("memory source hash mismatch")
	// ErrPrivacyPolicy is returned when a search document contains unredacted sensitive content.
	ErrPrivacyPolicy = errors.New("memory violates privacy policy")
	// ErrNormalizationMismatch is returned when persisted lexical normalization metadata is missing or stale.
	ErrNormalizationMismatch = errors.New("memory normalization mismatch")
	// ErrProvenanceMissing is returned when persisted memory lacks source provenance metadata.
	ErrProvenanceMissing = errors.New("memory provenance metadata is required")
)

func (s *Store) validateLoaded() error {
	switch {
	case s.SchemaVersion == 0:
		if len(s.Documents) > 0 {
			return ErrIncompatibleSchema
		}

		s.SchemaVersion = StoreSchemaVersion
	case s.SchemaVersion != StoreSchemaVersion:
		return ErrIncompatibleSchema
	}

	s.Normalization = strings.TrimSpace(s.Normalization)
	if s.Normalization != StoreTextNormalization {
		if len(s.Documents) == 0 {
			s.Normalization = StoreTextNormalization
		} else {
			return ErrNormalizationMismatch
		}
	}

	now := time.Now().UTC()
	if s.CreatedAt.IsZero() {
		s.CreatedAt = now
	}

	if s.UpdatedAt.IsZero() {
		s.UpdatedAt = s.CreatedAt
	}

	seen := make(map[string]struct{}, len(s.Documents))
	for i := range s.Documents {
		doc, err := s.prepareDocumentForSave(s.Documents[i])
		if err != nil {
			return err
		}

		if err := rememberDocumentID(seen, doc.ID); err != nil {
			return err
		}

		s.Documents[i] = doc
	}

	return nil
}

func rememberDocumentID(seen map[string]struct{}, id string) error {
	if _, ok := seen[id]; ok {
		return fmt.Errorf("%w: %s", ErrDuplicateID, id)
	}

	seen[id] = struct{}{}

	return nil
}

func searchDocument(doc Document, queryTerms []string, querySet map[string]struct{}) (Result, bool, error) {
	if err := validateSearchDocument(doc); err != nil {
		return Result{}, false, fmt.Errorf("memory: validate document %q: %w", doc.ID, err)
	}

	tokens := tokenize(doc.Text)
	if len(tokens) == 0 {
		return Result{}, false, nil
	}

	counts := make(map[string]int)

	for _, token := range tokens {
		if _, ok := querySet[token]; ok {
			counts[token]++
		}
	}

	if len(counts) == 0 {
		return Result{}, false, nil
	}

	matches := sortedKeys(counts)

	return Result{
		Document: cloneDocument(doc),
		Score:    score(counts, len(queryTerms), len(tokens)),
		Snippet:  snippet(doc.Text, matches, defaultSnippetRunes),
		Matches:  matches,
	}, true, nil
}

func validateSearchDocument(doc Document) error {
	if id := strings.TrimSpace(doc.ID); id == "" || privacy.RedactIdentifier(id) != id {
		if id == "" {
			return ErrMissingID
		}

		return ErrPrivacyPolicy
	}

	if !utf8.ValidString(doc.Text) {
		return ErrInvalidUTF8
	}

	redactedText := privacy.RedactText(doc.Text)
	if redactedText != doc.Text {
		return ErrPrivacyPolicy
	}

	if path := strings.TrimSpace(doc.Path); privacy.RedactIdentifier(path) != path {
		return ErrPrivacyPolicy
	}

	if !maps.Equal(doc.Metadata, privacy.RedactMetadata(doc.Metadata)) {
		return ErrPrivacyPolicy
	}

	if !maps.Equal(doc.Provenance, cleanMetadata(doc.Provenance)) {
		return ErrPrivacyPolicy
	}

	if doc.SourceHash == "" {
		return ErrSourceHashMismatch
	}

	if doc.SourceHash != privacy.SourceHash(doc.Text) {
		return ErrSourceHashMismatch
	}

	if err := validateProvenance(doc.Provenance); err != nil {
		return err
	}

	return nil
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

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			return value
		}
	}

	return ""
}

func cleanMetadata(metadata map[string]string) map[string]string {
	return privacy.RedactMetadata(metadata)
}

func redactDocumentID(id string) (string, error) {
	id = strings.TrimSpace(id)
	if id == "" {
		return "", ErrMissingID
	}

	return privacy.RedactIdentifier(id), nil
}

func ensureProvenance(provenance map[string]string, sourceType string) map[string]string {
	if provenance == nil {
		provenance = make(map[string]string, 2)
	}

	if strings.TrimSpace(provenance["source_type"]) == "" {
		provenance["source_type"] = sourceType
	}

	provenance["privacy_policy"] = privacy.RedactionPolicyVersion

	return provenance
}

func validateProvenance(provenance map[string]string) error {
	if strings.TrimSpace(provenance["source_type"]) == "" {
		return ErrProvenanceMissing
	}

	if strings.TrimSpace(provenance["privacy_policy"]) != privacy.RedactionPolicyVersion {
		return ErrPrivacyPolicy
	}

	return nil
}

func fileProvenance(path string) map[string]string {
	return map[string]string{
		"source_type": "file",
		"path":        path,
	}
}

func cloneTimePtr(value *time.Time) *time.Time {
	if value == nil {
		return nil
	}

	copied := value.UTC()

	return &copied
}

func cloneDocument(doc Document) Document {
	doc.Metadata = cloneMetadata(doc.Metadata)
	doc.Provenance = cloneMetadata(doc.Provenance)
	doc.ExpiresAt = cloneTimePtr(doc.ExpiresAt)

	return doc
}

func cloneMetadata(metadata map[string]string) map[string]string {
	if len(metadata) == 0 {
		return nil
	}

	out := make(map[string]string, len(metadata))
	maps.Copy(out, metadata)

	return out
}

func isExpired(doc Document, now time.Time) bool {
	return doc.ExpiresAt != nil && !doc.ExpiresAt.IsZero() && !doc.ExpiresAt.After(now)
}

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
