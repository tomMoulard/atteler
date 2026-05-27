// Package vector provides dependency-free vector retrieval primitives for
// small prose and ADR search workflows.
package vector

import (
	"bytes"
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"hash/fnv"
	"maps"
	"math"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/tommoulard/atteler/pkg/privacy"
	"github.com/tommoulard/atteler/pkg/retrieval"
)

const defaultVectorizerDimensions = 128

const (
	// StoreSchemaVersion is the current JSON-compatible vector store schema.
	StoreSchemaVersion = 1
	// TextHashVectorizerID identifies the dependency-free hashed token vectorizer.
	TextHashVectorizerID = "text-hash-fnv64a"
	// TextHashVectorizerModel names the local hashed token-frequency model.
	TextHashVectorizerModel = "local-token-frequency"
	// TextHashVectorizerNormalization describes TextVectorizer's token normalization.
	TextHashVectorizerNormalization = "unicode-letter-digit-lowercase-v1"
	vectorizerSpecVersion           = "1"
)

// Vector is an embedding or lexical feature vector.
type Vector []float64

// VectorizerSpec identifies how a vector was produced. Persisted stores use it
// to reject stale embeddings from a different model, normalization pipeline, or
// dimensionality before retrieval can trust them.
type VectorizerSpec struct {
	ID            string `json:"id,omitempty"`
	Model         string `json:"model,omitempty"`
	Normalization string `json:"normalization,omitempty"`
	Version       string `json:"version,omitempty"`
	Dimensions    int    `json:"dimensions,omitempty"`
}

// Document is a text item indexed by Store.
type Document struct {
	Metadata   map[string]string `json:"metadata,omitempty"`
	Provenance map[string]string `json:"provenance,omitempty"`
	ExpiresAt  *time.Time        `json:"expires_at,omitempty"`
	CreatedAt  time.Time         `json:"created_at,omitzero"`
	UpdatedAt  time.Time         `json:"updated_at,omitzero"`
	Vectorizer VectorizerSpec    `json:"vectorizer,omitzero"`
	ID         string            `json:"id"`
	Text       string            `json:"text,omitempty"`
	SourceHash string            `json:"source_hash,omitempty"`
	Vector     Vector            `json:"vector"`
}

// Result is a cosine-ranked search result.
type Result struct {
	Document Document `json:"document"`
	Score    float64  `json:"score"`
}

// Searcher adapts a Store plus text vectorizer to the shared retrieval
// contract.
type Searcher struct {
	Store      *Store
	Vectorizer Vectorizer
	Source     retrieval.Source
	ScorerName string
}

// Store is an in-memory vector document store. All exported methods are safe
// for concurrent use.
//
//nolint:govet // Layout prioritizes JSON/API readability over pointer-byte packing.
type Store struct {
	Documents     []Document     `json:"documents"`
	Vectorizer    VectorizerSpec `json:"vectorizer,omitzero"`
	CreatedAt     time.Time      `json:"created_at,omitzero"`
	UpdatedAt     time.Time      `json:"updated_at,omitzero"`
	mu            sync.RWMutex   `json:"-"`
	SchemaVersion int            `json:"schema_version"`
	Dimensions    int            `json:"dimensions"`
}

//nolint:govet // Matches Store's JSON field order without carrying its mutex.
type storeJSON struct {
	Documents     []Document     `json:"documents"`
	Vectorizer    VectorizerSpec `json:"vectorizer,omitzero"`
	CreatedAt     time.Time      `json:"created_at,omitzero"`
	UpdatedAt     time.Time      `json:"updated_at,omitzero"`
	SchemaVersion int            `json:"schema_version"`
	Dimensions    int            `json:"dimensions"`
}

// LoadOptions controls persisted vector store loading.
//
//nolint:govet // Keep the public option fields grouped by behavior.
type LoadOptions struct {
	// Migrate rebuilds persisted vectors from their redacted text instead of
	// trusting missing or stale vectorizer metadata.
	Migrate bool
	// Vectorize rebuilds a vector from a redacted persisted document when
	// Migrate is true.
	Vectorize func(Document) (Vector, error)
	// Vectorizer identifies the replacement vectorizer used when Migrate is true.
	Vectorizer VectorizerSpec
}

// NewStore returns an empty vector store. When dimensions is zero, the store
// adopts the first added document vector's dimensions.
func NewStore(dimensions int) (*Store, error) {
	if dimensions < 0 {
		return nil, ErrInvalidDimensions
	}

	now := time.Now().UTC()

	return &Store{
		SchemaVersion: StoreSchemaVersion,
		Dimensions:    dimensions,
		CreatedAt:     now,
		UpdatedAt:     now,
	}, nil
}

// NewStoreWithVectorizer returns an empty vector store pinned to spec. Added
// documents must either declare the same spec or omit it so the store can stamp
// the current identity.
func NewStoreWithVectorizer(spec VectorizerSpec) (*Store, error) {
	spec = normalizeVectorizerSpec(spec)
	if err := validateVectorizerIdentity(spec); err != nil {
		return nil, err
	}

	if spec.Dimensions < 0 {
		return nil, ErrInvalidDimensions
	}

	store, err := NewStore(spec.Dimensions)
	if err != nil {
		return nil, err
	}

	store.Vectorizer = spec

	return store, nil
}

// Add indexes doc. Existing documents with the same ID are replaced. When Text
// is present, callers must pass text that was redacted before vectorization so
// the store never keeps a vector derived from raw sensitive content.
func (s *Store) Add(doc Document) error {
	now := time.Now().UTC()

	doc, hasCreatedAt, err := prepareAddedDocument(doc, now)
	if err != nil {
		return err
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	if err := s.prepareAddLocked(&doc, now); err != nil {
		return err
	}

	if s.replaceDocumentLocked(doc, hasCreatedAt) {
		return nil
	}

	s.Documents = append(s.Documents, doc)

	return nil
}

func prepareAddedDocument(doc Document, now time.Time) (Document, bool, error) {
	id, err := redactDocumentID(doc.ID)
	if err != nil {
		return Document{}, false, err
	}

	doc.ID = id

	if err := validateVector(doc.Vector); err != nil {
		return Document{}, false, err
	}

	if doc.Text != "" && !utf8.ValidString(doc.Text) {
		return Document{}, false, ErrInvalidUTF8
	}

	doc.Vector = cloneVector(doc.Vector)
	if redactedText := privacy.RedactText(doc.Text); redactedText != doc.Text {
		return Document{}, false, ErrPrivacyPolicy
	}

	doc.Metadata = privacy.RedactMetadata(doc.Metadata)
	doc.Provenance = ensureProvenance(privacy.RedactMetadata(doc.Provenance), "direct")
	doc.ExpiresAt = cloneTimePtr(doc.ExpiresAt)
	doc.SourceHash = strings.TrimSpace(doc.SourceHash)

	if doc.Text != "" {
		doc.SourceHash = sourceHash(doc.Text)
	}

	hasCreatedAt := !doc.CreatedAt.IsZero()
	if doc.CreatedAt.IsZero() {
		doc.CreatedAt = now
	}

	doc.UpdatedAt = now

	return doc, hasCreatedAt, nil
}

func (s *Store) prepareAddLocked(doc *Document, now time.Time) error {
	if err := s.ensureSchemaLocked(); err != nil {
		return err
	}

	if doc.SourceHash == "" {
		return fmt.Errorf("%w: document %q has no source hash", ErrSourceHashMismatch, doc.ID)
	}

	if !validSourceHash(doc.SourceHash) {
		return fmt.Errorf("%w: document %q has malformed source hash", ErrSourceHashMismatch, doc.ID)
	}

	if err := validateOptionalVectorizerIdentity(s.Vectorizer); err != nil {
		return err
	}

	if err := s.adoptDimensionsLocked(doc.Vector); err != nil {
		return err
	}

	s.stampVectorizerDimensionsLocked()

	doc.Vectorizer = normalizeVectorizerSpec(doc.Vectorizer)
	if !doc.Vectorizer.IsZero() && doc.Vectorizer.Dimensions == 0 && s.Dimensions > 0 {
		doc.Vectorizer.Dimensions = s.Dimensions
	}

	if err := s.adoptVectorizerLocked(doc.Vectorizer); err != nil {
		return err
	}

	s.stampVectorizerDimensionsLocked()

	if doc.Vectorizer.IsZero() && !s.Vectorizer.IsZero() {
		doc.Vectorizer = s.Vectorizer
	}

	if err := checkDocumentTextHashVector(*doc); err != nil {
		return err
	}

	if s.CreatedAt.IsZero() {
		s.CreatedAt = now
	}

	s.UpdatedAt = now

	return nil
}

func (s *Store) replaceDocumentLocked(doc Document, hasCreatedAt bool) bool {
	for i := range s.Documents {
		if s.Documents[i].ID != doc.ID {
			continue
		}

		if !hasCreatedAt {
			doc.CreatedAt = s.Documents[i].CreatedAt
		}

		s.Documents[i] = doc

		return true
	}

	return false
}

// Search ranks documents by cosine similarity to query and returns up to limit
// results. A limit less than one returns every document with a non-zero score.
func (s *Store) Search(query Vector, limit int) ([]Result, error) {
	return s.SearchWithVectorizer(query, VectorizerSpec{}, limit)
}

// SearchWithVectorizer ranks documents using query and refuses searches from an
// incompatible vectorizer identity when the store is pinned to one.
func (s *Store) SearchWithVectorizer(query Vector, spec VectorizerSpec, limit int) ([]Result, error) {
	if err := validateVector(query); err != nil {
		return nil, err
	}

	results, err := s.searchLocked(query, normalizeVectorizerSpec(spec))
	if err != nil {
		return nil, err
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

// SearchRetrieval vectorizes query text and returns results using the shared
// retrieval contract.
func (s Searcher) SearchRetrieval(ctx context.Context, query retrieval.Query) ([]retrieval.Result, error) {
	if ctx == nil {
		return nil, ErrContextRequired
	}

	if err := ctx.Err(); err != nil {
		return nil, fmt.Errorf("vector retrieval: %w", err)
	}

	if s.Store == nil {
		return nil, nil
	}

	if s.Vectorizer == nil {
		vectorizer, err := NewTextVectorizer(s.Store.Dimensions)
		if err != nil {
			return nil, fmt.Errorf("vector retrieval: create vectorizer: %w", err)
		}

		s.Vectorizer = vectorizer
	}

	queryVector, err := vectorizeContext(ctx, s.Vectorizer, privacy.RedactText(query.Text))
	if err != nil {
		return nil, fmt.Errorf("vector retrieval: vectorize query: %w", err)
	}

	spec := s.Store.Vectorizer
	if spec.IsZero() {
		switch vectorizer := s.Vectorizer.(type) {
		case *TextVectorizer:
			spec = vectorizer.Spec()
		case TextVectorizer:
			spec = vectorizer.Spec()
		}
	}

	results, err := s.Store.SearchWithVectorizer(queryVector, spec, query.Limit)
	if err != nil {
		return nil, fmt.Errorf("vector retrieval: search store: %w", err)
	}

	out := make([]retrieval.Result, 0, len(results))
	for i := range results {
		out = append(out, s.retrievalResult(results[i], query))
	}

	return out, nil
}

// Delete removes documents by ID and reports whether anything was removed.
func (s *Store) Delete(id string) bool {
	id, err := redactDocumentID(id)
	if err != nil {
		return false
	}

	s.mu.Lock()
	defer s.mu.Unlock()

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
		s.clearVectorizerIfEmptyLocked()
		s.UpdatedAt = time.Now().UTC()

		return true
	}

	return false
}

// Compact removes expired documents and returns the number of purged entries.
func (s *Store) Compact(now time.Time) int {
	now = now.UTC()

	s.mu.Lock()
	defer s.mu.Unlock()

	removed := s.compactExpiredLocked(now)
	if removed > 0 {
		s.UpdatedAt = time.Now().UTC()
	}

	return removed
}

func (s *Store) compactExpiredLocked(now time.Time) int {
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
		s.clearVectorizerIfEmptyLocked()
	}

	return removed
}

// Save writes the vector store as pretty-printed JSON after compacting expired
// documents and validating that persisted vectors carry complete provenance and
// vectorizer metadata.
func (s *Store) Save(path string) error {
	snapshot, err := s.snapshotForSave(time.Now().UTC())
	if err != nil {
		return err
	}

	data, err := json.MarshalIndent(snapshot, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal vector store: %w", err)
	}

	data = append(data, '\n')

	if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
		return fmt.Errorf("create vector store dir: %w", err)
	}

	if err := os.WriteFile(path, data, 0o600); err != nil {
		return fmt.Errorf("write vector store %q: %w", path, err)
	}

	if err := os.Chmod(path, 0o600); err != nil {
		return fmt.Errorf("chmod vector store %q: %w", path, err)
	}

	return nil
}

// Load reads a JSON vector store saved by Save and refuses stale or incomplete
// persisted vector metadata.
func Load(path string) (*Store, error) {
	return LoadWithOptions(path, LoadOptions{})
}

// LoadWithOptions reads a JSON vector store and optionally migrates stale vector
// data by rebuilding vectors from redacted persisted text.
func LoadWithOptions(path string, opts LoadOptions) (*Store, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read vector store %q: %w", path, err)
	}

	var store Store
	if err := json.Unmarshal(data, &store); err != nil {
		return nil, fmt.Errorf("decode vector store %q: %w", path, err)
	}

	if opts.Migrate {
		if err := store.Migrate(opts.Vectorizer, opts.Vectorize); err != nil {
			return nil, fmt.Errorf("migrate vector store %q: %w", path, err)
		}

		return &store, nil
	}

	store.Compact(time.Now().UTC())

	if err := store.validateLoaded(); err != nil {
		return nil, fmt.Errorf("validate vector store %q: %w", path, err)
	}

	return &store, nil
}

// Migrate rebuilds all persisted vectors with spec. It is the explicit escape
// hatch for stale or legacy stores that Load refuses to trust by default.
func (s *Store) Migrate(spec VectorizerSpec, vectorize func(Document) (Vector, error)) error {
	if err := s.Rebuild(spec, vectorize); err != nil {
		return err
	}

	return s.validateLoaded()
}

// Rebuild re-vectorizes every stored document and pins the store to spec. This
// is the explicit migration path after dimensions, model, or normalization
// change: stale vectors are replaced before later searches can trust them.
func (s *Store) Rebuild(spec VectorizerSpec, vectorize func(Document) (Vector, error)) error {
	spec = normalizeVectorizerSpec(spec)
	if spec.IsZero() {
		return fmt.Errorf("%w: rebuild vectorizer metadata is required", ErrVectorizerMismatch)
	}

	if err := validateVectorizerIdentity(spec); err != nil {
		return err
	}

	if spec.Dimensions < 0 {
		return ErrInvalidDimensions
	}

	if vectorize == nil {
		return ErrVectorizerRequired
	}

	s.mu.RLock()

	if s.SchemaVersion < 0 || s.SchemaVersion > StoreSchemaVersion {
		s.mu.RUnlock()
		return ErrIncompatibleSchema
	}

	docs := make([]Document, 0, len(s.Documents))
	for i := range s.Documents {
		docs = append(docs, cloneDocument(s.Documents[i]))
	}

	s.mu.RUnlock()

	now := time.Now().UTC()

	rebuilt, dimensions, err := rebuildDocuments(docs, spec, vectorize, now)
	if err != nil {
		return err
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	s.SchemaVersion = StoreSchemaVersion

	if len(rebuilt) == 0 {
		dimensions = 0
		spec = VectorizerSpec{}
	} else {
		spec.Dimensions = dimensions
	}

	s.Dimensions = dimensions
	s.Vectorizer = spec

	clear(s.Documents[:cap(s.Documents)])
	s.Documents = rebuilt

	if s.CreatedAt.IsZero() {
		s.CreatedAt = now
	}

	s.UpdatedAt = now

	return nil
}

func rebuildDocuments(
	docs []Document,
	spec VectorizerSpec,
	vectorize func(Document) (Vector, error),
	now time.Time,
) ([]Document, int, error) {
	dimensions := spec.Dimensions
	rebuilt := make([]Document, 0, len(docs))
	seen := make(map[string]struct{}, len(docs))

	for i := range docs {
		if isExpired(docs[i], now) {
			continue
		}

		doc, err := rebuildDocument(docs[i], spec, vectorize, now, &dimensions, seen)
		if err != nil {
			return nil, 0, err
		}

		rebuilt = append(rebuilt, doc)
	}

	return rebuilt, dimensions, nil
}

func rebuildDocument(
	doc Document,
	spec VectorizerSpec,
	vectorize func(Document) (Vector, error),
	now time.Time,
	dimensions *int,
	seen map[string]struct{},
) (Document, error) {
	id, err := redactDocumentID(doc.ID)
	if err != nil {
		return Document{}, err
	}

	doc.ID = id

	if _, ok := seen[doc.ID]; ok {
		return Document{}, fmt.Errorf("%w: %s", ErrDuplicateID, doc.ID)
	}

	seen[doc.ID] = struct{}{}

	if doc.Text != "" && !utf8.ValidString(doc.Text) {
		return Document{}, ErrInvalidUTF8
	}

	if strings.TrimSpace(doc.Text) == "" {
		return Document{}, ErrEmptyText
	}

	doc.Text = privacy.RedactText(doc.Text)
	doc.Metadata = privacy.RedactMetadata(doc.Metadata)
	doc.Provenance = ensureProvenance(privacy.RedactMetadata(doc.Provenance), "legacy")
	doc.SourceHash = strings.TrimSpace(doc.SourceHash)

	if doc.Text != "" {
		doc.SourceHash = sourceHash(doc.Text)
	}

	if doc.SourceHash == "" {
		return Document{}, fmt.Errorf("%w: document %q has no source hash", ErrSourceHashMismatch, doc.ID)
	}

	if !validSourceHash(doc.SourceHash) {
		return Document{}, fmt.Errorf("%w: document %q has malformed source hash", ErrSourceHashMismatch, doc.ID)
	}

	source := cloneDocument(doc)
	source.Vector = nil
	source.Vectorizer = VectorizerSpec{}

	vec, err := vectorize(source)
	if err != nil {
		return Document{}, fmt.Errorf("rebuild vector %q: %w", doc.ID, err)
	}

	if err := validateVector(vec); err != nil {
		return Document{}, fmt.Errorf("rebuild vector %q: %w", doc.ID, err)
	}

	if *dimensions == 0 {
		*dimensions = len(vec)
	}

	if len(vec) != *dimensions {
		return Document{}, fmt.Errorf("%w: rebuild vector %q got %d, want %d", ErrDimensionMismatch, doc.ID, len(vec), *dimensions)
	}

	doc.Vector = cloneVector(vec)
	doc.Vectorizer = spec
	doc.Vectorizer.Dimensions = *dimensions

	if err := checkDocumentTextHashVector(doc); err != nil {
		return Document{}, err
	}

	if doc.CreatedAt.IsZero() {
		doc.CreatedAt = now
	}

	doc.UpdatedAt = now

	return doc, nil
}

func (s *Store) snapshotForSave(now time.Time) (storeJSON, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	removedExpired := s.compactExpiredLocked(now)
	if removedExpired > 0 {
		s.UpdatedAt = now
	}

	if err := s.ensureSchemaLocked(); err != nil {
		return storeJSON{}, err
	}

	hasDocuments := len(s.Documents) > 0

	dimensions, err := normalizeStoreDimensions(s.Dimensions, hasDocuments)
	if err != nil {
		return storeJSON{}, ErrInvalidDimensions
	}

	s.Dimensions = dimensions

	if s.CreatedAt.IsZero() {
		s.CreatedAt = now
	}

	if s.UpdatedAt.IsZero() {
		s.UpdatedAt = now
	}

	if privacyErr := validatePersistedVectorizerSpecPrivacy(s.Vectorizer); privacyErr != nil {
		return storeJSON{}, privacyErr
	}

	originalVectorizer := normalizeVectorizerSpec(s.Vectorizer)

	s.Vectorizer, err = normalizeStoreVectorizerForDocuments(s.Vectorizer, hasDocuments)
	if err != nil {
		return storeJSON{}, err
	}

	if !hasDocuments && !originalVectorizer.IsZero() {
		s.Dimensions = 0
	}

	kept := s.Documents[:0]
	seen := make(map[string]struct{}, len(s.Documents))

	for i := range s.Documents {
		doc := s.Documents[i]

		normalized, err := s.normalizePersistedDocumentLocked(doc)
		if err != nil {
			return storeJSON{}, err
		}

		if _, ok := seen[normalized.ID]; ok {
			return storeJSON{}, fmt.Errorf("%w: %s", ErrDuplicateID, normalized.ID)
		}

		seen[normalized.ID] = struct{}{}
		kept = append(kept, normalized)
	}

	s.Documents = kept

	if hasDocuments {
		if err := validateCompleteVectorizerSpec(s.Vectorizer, s.Dimensions); err != nil {
			return storeJSON{}, err
		}
	}

	return storeJSON{
		Documents:     cloneDocuments(s.Documents),
		Vectorizer:    s.Vectorizer,
		CreatedAt:     s.CreatedAt,
		UpdatedAt:     s.UpdatedAt,
		SchemaVersion: s.SchemaVersion,
		Dimensions:    s.Dimensions,
	}, nil
}

func (s *Store) validateLoaded() error {
	if s.SchemaVersion == 0 {
		if len(s.Documents) > 0 {
			return ErrIncompatibleSchema
		}

		s.SchemaVersion = StoreSchemaVersion
	}

	if s.SchemaVersion != StoreSchemaVersion {
		return ErrIncompatibleSchema
	}

	hasDocuments := len(s.Documents) > 0

	dimensions, err := normalizeStoreDimensions(s.Dimensions, hasDocuments)
	if err != nil {
		return ErrInvalidDimensions
	}

	s.Dimensions = dimensions

	now := time.Now().UTC()
	if s.CreatedAt.IsZero() {
		s.CreatedAt = now
	}

	if s.UpdatedAt.IsZero() {
		s.UpdatedAt = s.CreatedAt
	}

	if privacyErr := validatePersistedVectorizerSpecPrivacy(s.Vectorizer); privacyErr != nil {
		return privacyErr
	}

	originalVectorizer := normalizeVectorizerSpec(s.Vectorizer)

	s.Vectorizer, err = normalizeStoreVectorizerForDocuments(s.Vectorizer, hasDocuments)
	if err != nil {
		return err
	}

	if !hasDocuments && !originalVectorizer.IsZero() {
		s.Dimensions = 0
	}

	if hasDocuments {
		if err := validateCompleteVectorizerSpec(s.Vectorizer, s.Dimensions); err != nil {
			return err
		}
	}

	seen := make(map[string]struct{}, len(s.Documents))
	for i := range s.Documents {
		normalized, err := s.normalizeLoadedDocument(s.Documents[i], seen)
		if err != nil {
			return err
		}

		s.Documents[i] = normalized
	}

	return nil
}

func normalizeStoreDimensions(dimensions int, hasDocuments bool) (int, error) {
	if dimensions >= 0 {
		return dimensions, nil
	}

	if hasDocuments {
		return 0, ErrInvalidDimensions
	}

	return 0, nil
}

func normalizeStoreVectorizerForDocuments(spec VectorizerSpec, hasDocuments bool) (VectorizerSpec, error) {
	spec = normalizeVectorizerSpec(spec)

	if !hasDocuments {
		return VectorizerSpec{}, nil
	}

	if spec.Dimensions < 0 {
		return VectorizerSpec{}, ErrInvalidDimensions
	}

	if err := validateOptionalVectorizerIdentity(spec); err != nil {
		return VectorizerSpec{}, err
	}

	return spec, nil
}

func (s *Store) normalizeLoadedDocument(doc Document, seen map[string]struct{}) (Document, error) {
	doc.ID = strings.TrimSpace(doc.ID)
	if doc.ID == "" {
		return Document{}, ErrMissingID
	}

	if _, ok := seen[doc.ID]; ok {
		return Document{}, fmt.Errorf("%w: %s", ErrDuplicateID, doc.ID)
	}

	seen[doc.ID] = struct{}{}

	if s.Dimensions == 0 {
		s.Dimensions = len(doc.Vector)
		if !s.Vectorizer.IsZero() && s.Vectorizer.Dimensions == 0 {
			s.Vectorizer.Dimensions = s.Dimensions
		}
	}

	return s.normalizePersistedDocumentLocked(doc)
}

func (s *Store) normalizePersistedDocumentLocked(doc Document) (Document, error) {
	doc.ID = strings.TrimSpace(doc.ID)
	if doc.ID == "" {
		return Document{}, ErrMissingID
	}

	if err := validatePersistedVectorizerSpecPrivacy(doc.Vectorizer); err != nil {
		return Document{}, err
	}

	doc.Vectorizer = normalizeVectorizerSpec(doc.Vectorizer)
	if err := s.validatePersistedDocumentLocked(doc); err != nil {
		return Document{}, err
	}

	doc.Vector = cloneVector(doc.Vector)
	doc.Metadata = cloneMetadata(doc.Metadata)
	doc.Provenance = cloneMetadata(doc.Provenance)
	doc.ExpiresAt = cloneTimePtr(doc.ExpiresAt)

	if doc.CreatedAt.IsZero() {
		doc.CreatedAt = s.CreatedAt
	}

	if doc.UpdatedAt.IsZero() {
		doc.UpdatedAt = doc.CreatedAt
	}

	return doc, nil
}

func (s *Store) validatePersistedDocumentLocked(doc Document) error {
	if err := validateVector(doc.Vector); err != nil {
		return err
	}

	if err := s.checkDimensionsLocked(doc.Vector); err != nil {
		return err
	}

	if err := checkDocumentPrivacy(doc); err != nil {
		return err
	}

	if err := validateCompleteVectorizerSpec(doc.Vectorizer, s.Dimensions); err != nil {
		return err
	}

	if !s.Vectorizer.CompatibleWith(doc.Vectorizer) {
		return fmt.Errorf("%w: document %q uses %s/%s, want %s/%s",
			ErrVectorizerMismatch, doc.ID, doc.Vectorizer.ID, doc.Vectorizer.Model, s.Vectorizer.ID, s.Vectorizer.Model)
	}

	if err := checkDocumentSourceHash(doc); err != nil {
		return err
	}

	if err := checkDocumentProvenance(doc); err != nil {
		return err
	}

	if err := checkDocumentTextHashVector(doc); err != nil {
		return err
	}

	return nil
}

// searchLocked performs the read-locked portion of Search, cloning results
// so callers can sort/slice without holding the lock.
func (s *Store) searchLocked(query Vector, spec VectorizerSpec) ([]Result, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	if err := s.checkSchemaLocked(); err != nil {
		return nil, err
	}

	if err := s.checkDimensionsLocked(query); err != nil {
		return nil, err
	}

	if err := s.checkSearchVectorizerLocked(spec, len(query)); err != nil {
		return nil, err
	}

	now := time.Now().UTC()

	results := make([]Result, 0, len(s.Documents))
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

		if _, ok := seen[doc.ID]; ok {
			return nil, fmt.Errorf("%w: %s", ErrDuplicateID, doc.ID)
		}

		seen[doc.ID] = struct{}{}

		result, ok, err := s.scoreDocumentLocked(query, doc)
		if err != nil {
			return nil, err
		}

		if !ok {
			continue
		}

		results = append(results, result)
	}

	return results, nil
}

func (s *Store) checkSearchVectorizerLocked(spec VectorizerSpec, queryDimensions int) error {
	if s.Vectorizer.IsZero() {
		if !spec.IsZero() && len(s.Documents) > 0 {
			return fmt.Errorf("%w: store has no vectorizer metadata for query %s/%s",
				ErrVectorizerMismatch, spec.ID, spec.Model)
		}

		return nil
	}

	if spec.IsZero() {
		return ErrVectorizerRequired
	}

	if err := validateCompleteVectorizerSpec(s.Vectorizer, s.Dimensions); err != nil {
		return err
	}

	if err := validateCompleteVectorizerSpec(spec, queryDimensions); err != nil {
		return err
	}

	if !s.Vectorizer.CompatibleWith(spec) {
		return fmt.Errorf("%w: query %s/%s cannot search store %s/%s",
			ErrVectorizerMismatch, spec.ID, spec.Model, s.Vectorizer.ID, s.Vectorizer.Model)
	}

	return nil
}

func (s *Store) scoreDocumentLocked(query Vector, doc Document) (Result, bool, error) {
	if err := checkDocumentPrivacy(doc); err != nil {
		return Result{}, false, err
	}

	if err := checkDocumentSourceHash(doc); err != nil {
		return Result{}, false, err
	}

	if err := checkDocumentProvenance(doc); err != nil {
		return Result{}, false, err
	}

	if err := s.checkDocumentVectorizerLocked(doc); err != nil {
		return Result{}, false, err
	}

	if err := checkDocumentTextHashVector(doc); err != nil {
		return Result{}, false, err
	}

	score, err := CosineSimilarity(query, doc.Vector)
	if err != nil {
		return Result{}, false, err
	}

	if score == 0 {
		return Result{}, false, nil
	}

	return Result{
		Document: cloneDocument(doc),
		Score:    score,
	}, true, nil
}

// CosineSimilarity returns the cosine similarity between two vectors.
func CosineSimilarity(a, b Vector) (float64, error) {
	if len(a) == 0 || len(b) == 0 {
		return 0, ErrEmptyVector
	}

	if len(a) != len(b) {
		return 0, fmt.Errorf("%w: got %d and %d", ErrDimensionMismatch, len(a), len(b))
	}

	var dot, normA, normB float64

	for i := range a {
		if !isFinite(a[i]) || !isFinite(b[i]) {
			return 0, ErrInvalidVector
		}

		dot += a[i] * b[i]
		normA += a[i] * a[i]
		normB += b[i] * b[i]
	}

	if normA == 0 || normB == 0 {
		return 0, ErrZeroVector
	}

	return dot / (math.Sqrt(normA) * math.Sqrt(normB)), nil
}

// TextVectorizer creates deterministic lexical vectors from text using feature
// hashing. It is intended as a simple fallback when model embeddings are not
// available.
type TextVectorizer struct {
	Dimensions int
}

// NewTextVectorizer returns a text vectorizer. A zero dimension value uses the
// default dimension count.
func NewTextVectorizer(dimensions int) (*TextVectorizer, error) {
	if dimensions < 0 {
		return nil, ErrInvalidDimensions
	}

	if dimensions == 0 {
		dimensions = defaultVectorizerDimensions
	}

	return &TextVectorizer{Dimensions: dimensions}, nil
}

// Vectorize converts text into a deterministic hashed token-frequency vector.
func (v TextVectorizer) Vectorize(text string) (Vector, error) {
	if v.Dimensions <= 0 {
		return nil, ErrInvalidDimensions
	}

	tokens := tokenize(text)
	if len(tokens) == 0 {
		return nil, ErrEmptyText
	}

	out := make(Vector, v.Dimensions)
	for _, token := range tokens {
		out[hashToken(token)%uint64(v.Dimensions)]++
	}

	return out, nil
}

// Spec returns the stable identity stamped on vectors created by TextVectorizer.
func (v TextVectorizer) Spec() VectorizerSpec {
	return TextVectorizerSpec(v.Dimensions)
}

// TextVectorizerSpec returns the persisted identity for the built-in hashed
// token vectorizer.
func TextVectorizerSpec(dimensions int) VectorizerSpec {
	return VectorizerSpec{
		ID:            TextHashVectorizerID,
		Model:         TextHashVectorizerModel,
		Dimensions:    dimensions,
		Normalization: TextHashVectorizerNormalization,
		Version:       vectorizerSpecVersion,
	}
}

// Metadata describes the lexical fallback vectorizer for persisted indexes
// and CLI output.
func (v TextVectorizer) Metadata() VectorizerMetadata {
	return NewLexicalMetadata(v.Dimensions)
}

// Vectorizer abstracts text-to-vector conversion so callers can swap between
// the zero-dependency TextVectorizer and a model-backed EmbeddingVectorizer.
//
// Model-backed implementations should expose VectorizerContext and may reject
// Vectorize calls that would otherwise need a hidden root context.
type Vectorizer interface {
	Vectorize(text string) (Vector, error)
}

// VectorizerContext extends Vectorizer with context-aware vectorization.
// Implementations that make network calls should implement this interface.
type VectorizerContext interface {
	Vectorizer
	VectorizeContext(ctx context.Context, text string) (Vector, error)
}

// EmbeddingVectorizer calls an Ollama-compatible embedding API to produce
// dense vectors from text. It is the recommended vectorizer when search
// quality matters and a model endpoint is available.
type EmbeddingVectorizer struct {
	client   *http.Client
	baseURL  string
	model    string
	provider string
}

const (
	defaultEmbeddingProvider = "ollama"
	defaultEmbeddingBaseURL  = "http://127.0.0.1:11434"
	defaultEmbeddingModel    = "nomic-embed-text"
	embeddingTimeout         = 30 * time.Second
)

// EmbeddingOption configures an EmbeddingVectorizer.
type EmbeddingOption func(*EmbeddingVectorizer)

// WithEmbeddingProvider labels the embedding backend in persisted metadata.
// The implementation currently uses an Ollama-compatible /api/embed endpoint.
func WithEmbeddingProvider(provider string) EmbeddingOption {
	return func(v *EmbeddingVectorizer) {
		if p := strings.TrimSpace(provider); p != "" {
			v.provider = p
		}
	}
}

// WithEmbeddingBaseURL sets the API endpoint. Default is the local Ollama URL.
func WithEmbeddingBaseURL(baseURL string) EmbeddingOption {
	return func(v *EmbeddingVectorizer) {
		if u := strings.TrimSpace(baseURL); u != "" {
			v.baseURL = u
		}
	}
}

// WithEmbeddingModel sets the model name. Default is "nomic-embed-text".
func WithEmbeddingModel(model string) EmbeddingOption {
	return func(v *EmbeddingVectorizer) {
		if m := strings.TrimSpace(model); m != "" {
			v.model = m
		}
	}
}

// WithEmbeddingHTTPClient overrides the default HTTP client.
func WithEmbeddingHTTPClient(client *http.Client) EmbeddingOption {
	return func(v *EmbeddingVectorizer) {
		if client != nil {
			v.client = client
		}
	}
}

// WithEmbeddingTimeout sets the request timeout on the vectorizer's HTTP
// client. It preserves other client fields when a custom client is already set.
func WithEmbeddingTimeout(timeout time.Duration) EmbeddingOption {
	return func(v *EmbeddingVectorizer) {
		if timeout <= 0 {
			return
		}

		if v.client == nil {
			v.client = &http.Client{Timeout: timeout}
			return
		}

		client := *v.client
		client.Timeout = timeout
		v.client = &client
	}
}

// NewEmbeddingVectorizer returns a vectorizer that calls an Ollama-compatible
// /api/embed endpoint. The zero-value options use the local Ollama default URL
// and "nomic-embed-text" as the model.
func NewEmbeddingVectorizer(opts ...EmbeddingOption) *EmbeddingVectorizer {
	v := &EmbeddingVectorizer{
		baseURL:  defaultEmbeddingBaseURL,
		model:    defaultEmbeddingModel,
		provider: defaultEmbeddingProvider,
		client:   &http.Client{Timeout: embeddingTimeout},
	}

	for _, opt := range opts {
		opt(v)
	}

	return v
}

// Metadata describes the embedding vectorizer for persisted indexes and CLI
// output. Pass zero dimensions when the model's vector size is not known yet.
func (v *EmbeddingVectorizer) Metadata(dimensions int) VectorizerMetadata {
	if v == nil {
		return NewEmbeddingMetadata("", "", "", dimensions)
	}

	return NewEmbeddingMetadata(v.provider, v.model, v.baseURL, dimensions)
}

// Provider returns the metadata provider label for this vectorizer.
func (v *EmbeddingVectorizer) Provider() string {
	if v == nil {
		return ""
	}

	return v.provider
}

// Model returns the embedding model name.
func (v *EmbeddingVectorizer) Model() string {
	if v == nil {
		return ""
	}

	return v.model
}

// BaseURL returns the embedding API base URL.
func (v *EmbeddingVectorizer) BaseURL() string {
	if v == nil {
		return ""
	}

	return v.baseURL
}

// Vectorize is kept for source compatibility only.
//
// Deprecated: use VectorizeContext so embedding HTTP requests inherit caller
// cancellation and deadlines.
func (v *EmbeddingVectorizer) Vectorize(text string) (Vector, error) {
	if strings.TrimSpace(text) == "" {
		return nil, ErrEmptyText
	}

	return nil, ErrContextRequired
}

// VectorizeContext is Vectorize with caller-provided cancellation.
func (v *EmbeddingVectorizer) VectorizeContext(ctx context.Context, text string) (Vector, error) {
	if ctx == nil {
		return nil, ErrContextRequired
	}

	if err := ctx.Err(); err != nil {
		return nil, fmt.Errorf("embedding context: %w", err)
	}

	text = strings.TrimSpace(text)
	if text == "" {
		return nil, ErrEmptyText
	}

	baseURL, model, client := v.embeddingSettings()

	body, err := json.Marshal(ollamaEmbedRequest{
		Model: model,
		Input: text,
	})
	if err != nil {
		return nil, fmt.Errorf("embedding: marshal request: %w", err)
	}

	endpoint := strings.TrimRight(baseURL, "/") + "/api/embed"

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("embedding: create request: %w", err)
	}

	req.Header.Set("Content-Type", "application/json")

	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("embedding: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("embedding: unexpected status %d", resp.StatusCode)
	}

	var result ollamaEmbedResponse
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return nil, fmt.Errorf("embedding: decode response: %w", err)
	}

	if len(result.Embeddings) == 0 {
		return nil, fmt.Errorf("embedding: empty response from %s", model)
	}

	if len(result.Embeddings[0]) == 0 {
		return nil, fmt.Errorf("embedding: empty vector response from %s", model)
	}

	return Vector(result.Embeddings[0]), nil
}

// Spec returns the stable identity for this embedding endpoint/model. The
// dimensions value is set after a caller knows the model response width.
func (v *EmbeddingVectorizer) Spec(dimensions int) VectorizerSpec {
	_, model, _ := v.embeddingSettings()

	return normalizeVectorizerSpec(VectorizerSpec{
		ID:            "ollama-compatible-embedding",
		Model:         model,
		Dimensions:    dimensions,
		Normalization: "trim-space-v1",
		Version:       vectorizerSpecVersion,
	})
}

func (v *EmbeddingVectorizer) embeddingSettings() (baseURL, model string, client *http.Client) {
	baseURL = defaultEmbeddingBaseURL
	model = defaultEmbeddingModel
	client = &http.Client{Timeout: embeddingTimeout}

	if v == nil {
		return baseURL, model, client
	}

	if strings.TrimSpace(v.baseURL) != "" {
		baseURL = v.baseURL
	}

	if strings.TrimSpace(v.model) != "" {
		model = v.model
	}

	if v.client != nil {
		client = v.client
	}

	return baseURL, model, client
}

// ollamaEmbedRequest is the Ollama /api/embed request format.
type ollamaEmbedRequest struct {
	Input any    `json:"input"`
	Model string `json:"model"`
}

// ollamaEmbedResponse is the Ollama /api/embed response format.
type ollamaEmbedResponse struct {
	Embeddings [][]float64 `json:"embeddings"`
}

// adoptDimensionsLocked sets the store dimensions from the first added vector.
// Caller must hold s.mu (write lock).
func (s *Store) adoptDimensionsLocked(vec Vector) error {
	if s.Dimensions == 0 {
		s.Dimensions = len(vec)
	}

	if len(vec) != s.Dimensions {
		return fmt.Errorf("%w: got %d, want %d", ErrDimensionMismatch, len(vec), s.Dimensions)
	}

	return nil
}

// adoptVectorizerLocked pins the store to the first document vectorizer that
// declares an identity. Caller must hold s.mu (write lock).
func (s *Store) adoptVectorizerLocked(spec VectorizerSpec) error {
	if spec.IsZero() {
		return nil
	}

	if err := validateVectorizerIdentity(spec); err != nil {
		return err
	}

	if spec.Dimensions > 0 && s.Dimensions > 0 && spec.Dimensions != s.Dimensions {
		return fmt.Errorf("%w: vectorizer dimensions got %d, want %d", ErrDimensionMismatch, spec.Dimensions, s.Dimensions)
	}

	if s.Vectorizer.IsZero() {
		s.Vectorizer = spec
		if s.Vectorizer.Dimensions == 0 {
			s.Vectorizer.Dimensions = s.Dimensions
		}

		return nil
	}

	if !s.Vectorizer.CompatibleWith(spec) {
		return fmt.Errorf("%w: document %s/%s cannot be added to store %s/%s",
			ErrVectorizerMismatch, spec.ID, spec.Model, s.Vectorizer.ID, s.Vectorizer.Model)
	}

	if s.Vectorizer.Dimensions == 0 && spec.Dimensions > 0 {
		s.Vectorizer.Dimensions = spec.Dimensions
	}

	return nil
}

// stampVectorizerDimensionsLocked fills a dimensionless pinned vectorizer after
// the store adopts vector width from its first document. Caller must hold s.mu.
func (s *Store) stampVectorizerDimensionsLocked() {
	if !s.Vectorizer.IsZero() && s.Vectorizer.Dimensions == 0 && s.Dimensions > 0 {
		s.Vectorizer.Dimensions = s.Dimensions
	}
}

// clearVectorizerIfEmptyLocked drops vectorizer-derived dimensions once the
// last vectorized document is deleted or expired. Explicit dimension-only stores
// keep their configured width because there is no vectorizer metadata to unpin.
func (s *Store) clearVectorizerIfEmptyLocked() {
	if len(s.Documents) == 0 && !s.Vectorizer.IsZero() {
		s.Vectorizer = VectorizerSpec{}
		s.Dimensions = 0
	}
}

// ensureSchemaLocked normalizes legacy zero-value stores and rejects stores
// written by a newer incompatible schema. Caller must hold s.mu.
func (s *Store) ensureSchemaLocked() error {
	if s.SchemaVersion == 0 {
		if len(s.Documents) > 0 {
			return ErrIncompatibleSchema
		}

		s.SchemaVersion = StoreSchemaVersion
	}

	return s.checkSchemaLocked()
}

// checkSchemaLocked rejects stores written by a newer incompatible schema.
// A zero value is treated as a legacy in-memory store for read-only callers.
// Caller must hold s.mu.
func (s *Store) checkSchemaLocked() error {
	if s.SchemaVersion == 0 {
		if len(s.Documents) > 0 {
			return ErrIncompatibleSchema
		}

		return nil
	}

	if s.SchemaVersion == StoreSchemaVersion {
		return nil
	}

	return ErrIncompatibleSchema
}

// checkDocumentVectorizerLocked validates per-document vectorizer metadata for
// stores that declare a vectorizer identity. Caller must hold s.mu.
func (s *Store) checkDocumentVectorizerLocked(doc Document) error {
	if s.Vectorizer.IsZero() {
		return nil
	}

	if doc.Vectorizer.IsZero() {
		return fmt.Errorf("%w: document %q has no vectorizer metadata", ErrVectorizerMismatch, doc.ID)
	}

	if err := validateCompleteVectorizerSpec(doc.Vectorizer, s.Dimensions); err != nil {
		return fmt.Errorf("document %q vectorizer metadata: %w", doc.ID, err)
	}

	if !s.Vectorizer.CompatibleWith(doc.Vectorizer) {
		return fmt.Errorf("%w: document %q uses %s/%s, want %s/%s",
			ErrVectorizerMismatch, doc.ID, doc.Vectorizer.ID, doc.Vectorizer.Model, s.Vectorizer.ID, s.Vectorizer.Model)
	}

	return nil
}

// checkDimensionsLocked validates query dimensions. Caller must hold s.mu
// (at least read lock).
func (s *Store) checkDimensionsLocked(vec Vector) error {
	if s.Dimensions > 0 && len(vec) != s.Dimensions {
		return fmt.Errorf("%w: got %d, want %d", ErrDimensionMismatch, len(vec), s.Dimensions)
	}

	return nil
}

func validateVector(vec Vector) error {
	if len(vec) == 0 {
		return ErrEmptyVector
	}

	var norm float64

	for _, value := range vec {
		if !isFinite(value) {
			return ErrInvalidVector
		}

		norm += value * value
	}

	if norm == 0 {
		return ErrZeroVector
	}

	return nil
}

func isFinite(value float64) bool {
	return !math.IsNaN(value) && !math.IsInf(value, 0)
}

func cloneDocument(doc Document) Document {
	doc.Vector = cloneVector(doc.Vector)
	doc.Metadata = cloneMetadata(doc.Metadata)
	doc.Provenance = cloneMetadata(doc.Provenance)
	doc.ExpiresAt = cloneTimePtr(doc.ExpiresAt)

	return doc
}

func (s Searcher) retrievalResult(result Result, query retrieval.Query) retrieval.Result {
	doc := result.Document

	source := s.Source
	if source.Type == "" {
		source = retrieval.Source{Type: retrieval.SourceVector}
	}

	if source.Name == "" {
		source.Name = doc.Metadata["path"]
	}

	if source.URI == "" {
		source.URI = doc.Metadata["path"]
	}

	metadata := cloneMetadata(doc.Metadata)
	policyContext := retrieval.PolicyContext{
		Source:     source,
		Metadata:   metadata,
		DocumentID: doc.ID,
		Path:       doc.Metadata["path"],
	}
	text, sanitizedSafety := retrieval.Sanitize(doc.Text, policyContext)
	metadata, metadataSafety := retrieval.SanitizeMetadata(metadata, policyContext)
	safety := retrieval.MergeSafety(retrieval.SafetyFromMetadata(metadata), sanitizedSafety)
	safety = retrieval.MergeSafety(safety, metadataSafety)

	if !retrieval.IsDefaultSafety(safety) {
		metadata = retrieval.MergeSafetyMetadata(metadata, safety)
	}

	chunk := retrieval.BestChunkForTerms(doc.ID, text, tokenize(query.Text), retrieval.ChunkOptions{})
	chunk = applySourceRuneOffset(chunk, metadata)

	scorerName := s.ScorerName
	if scorerName == "" {
		scorerName = scorerNameForVectorizer(s.Vectorizer)
	}

	scorer := retrieval.Scorer{
		Name: scorerName,
		Raw:  result.Score,
		Details: map[string]float64{
			"cosine_similarity": result.Score,
		},
	}
	if query.Explain {
		scorer.Explanation = []string{"ranked by cosine similarity between query vector and document vector"}
	}

	return retrieval.NormalizeResult(retrieval.Result{
		Source:     source,
		DocumentID: doc.ID,
		Chunk:      chunk.Chunk,
		Score:      retrieval.ClampScore(result.Score),
		Scorer:     scorer,
		Snippet:    retrieval.Snippet(chunk.Text, 160),
		Metadata:   metadata,
		Freshness:  retrieval.FreshnessFromMetadata(metadata),
		Safety:     safety,
	})
}

func applySourceRuneOffset(chunk retrieval.ChunkedText, metadata map[string]string) retrieval.ChunkedText {
	sourceStart, ok := parseNonNegativeMetadataInt(metadata["chunk_start_rune"])
	if !ok {
		return chunk
	}

	sourceEnd, hasSourceEnd := parseNonNegativeMetadataInt(metadata["chunk_end_rune"])

	chunk.Chunk.Range.Unit = retrieval.RangeUnitRuneOffset
	chunk.Chunk.Range.Start += sourceStart
	chunk.Chunk.Range.End += sourceStart

	if hasSourceEnd && chunk.Chunk.Range.End > sourceEnd {
		chunk.Chunk.Range.End = sourceEnd
	}

	if chunk.Chunk.Range.Start > chunk.Chunk.Range.End {
		chunk.Chunk.Range.Start = chunk.Chunk.Range.End
	}

	return chunk
}

func parseNonNegativeMetadataInt(raw string) (int, bool) {
	value, err := strconv.Atoi(strings.TrimSpace(raw))
	if err != nil || value < 0 {
		return 0, false
	}

	return value, true
}

func vectorizeContext(ctx context.Context, vectorizer Vectorizer, text string) (Vector, error) {
	if contextVectorizer, ok := vectorizer.(VectorizerContext); ok {
		vec, err := contextVectorizer.VectorizeContext(ctx, text)
		if err != nil {
			return nil, fmt.Errorf("vectorize with context: %w", err)
		}

		return vec, nil
	}

	if err := ctx.Err(); err != nil {
		return nil, fmt.Errorf("vectorize context: %w", err)
	}

	vec, err := vectorizer.Vectorize(text)
	if err != nil {
		return nil, fmt.Errorf("vectorize text: %w", err)
	}

	return vec, nil
}

func scorerNameForVectorizer(vectorizer Vectorizer) string {
	switch vectorizer.(type) {
	case *TextVectorizer, TextVectorizer:
		return "hashed-vector-cosine"
	default:
		return "embedding-cosine"
	}
}

func cloneDocuments(docs []Document) []Document {
	out := make([]Document, 0, len(docs))
	for i := range docs {
		out = append(out, cloneDocument(docs[i]))
	}

	return out
}

func cloneVector(vec Vector) Vector {
	out := make(Vector, len(vec))
	copy(out, vec)

	return out
}

func cloneMetadata(metadata map[string]string) map[string]string {
	if len(metadata) == 0 {
		return nil
	}

	out := make(map[string]string, len(metadata))
	maps.Copy(out, metadata)

	return out
}

func cloneTimePtr(value *time.Time) *time.Time {
	if value == nil {
		return nil
	}

	copied := value.UTC()

	return &copied
}

func isExpired(doc Document, now time.Time) bool {
	return doc.ExpiresAt != nil && !doc.ExpiresAt.IsZero() && !doc.ExpiresAt.After(now)
}

func redactDocumentID(id string) (string, error) {
	id = strings.TrimSpace(id)
	if id == "" {
		return "", ErrMissingID
	}

	return privacy.RedactIdentifier(id), nil
}

func validatePersistedDocumentID(id string) error {
	id = strings.TrimSpace(id)
	if id == "" {
		return ErrMissingID
	}

	if privacy.RedactIdentifier(id) != id {
		return fmt.Errorf("%w: %s", ErrPrivacyPolicy, id)
	}

	return nil
}

func checkDocumentSourceHash(doc Document) error {
	if doc.SourceHash == "" {
		return fmt.Errorf("%w: document %q", ErrSourceHashMismatch, doc.ID)
	}

	if !validSourceHash(doc.SourceHash) {
		return fmt.Errorf("%w: document %q", ErrSourceHashMismatch, doc.ID)
	}

	if doc.Text != "" && doc.SourceHash != sourceHash(doc.Text) {
		return fmt.Errorf("%w: document %q", ErrSourceHashMismatch, doc.ID)
	}

	return nil
}

func checkDocumentPrivacy(doc Document) error {
	if err := validatePersistedDocumentID(doc.ID); err != nil {
		return err
	}

	if doc.Text != "" && !utf8.ValidString(doc.Text) {
		return ErrInvalidUTF8
	}

	if doc.Text != privacy.RedactText(doc.Text) {
		return ErrPrivacyPolicy
	}

	if !maps.Equal(doc.Metadata, privacy.RedactMetadata(doc.Metadata)) {
		return ErrPrivacyPolicy
	}

	if !maps.Equal(doc.Provenance, privacy.RedactMetadata(doc.Provenance)) {
		return ErrPrivacyPolicy
	}

	return nil
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

func checkDocumentProvenance(doc Document) error {
	if strings.TrimSpace(doc.Provenance["source_type"]) == "" {
		return fmt.Errorf("%w: document %q", ErrProvenanceMissing, doc.ID)
	}

	if strings.TrimSpace(doc.Provenance["privacy_policy"]) != privacy.RedactionPolicyVersion {
		return fmt.Errorf("%w: document %q has stale privacy policy provenance", ErrPrivacyPolicy, doc.ID)
	}

	return nil
}

func checkDocumentTextHashVector(doc Document) error {
	if strings.TrimSpace(doc.Text) == "" {
		return nil
	}

	spec := normalizeVectorizerSpec(doc.Vectorizer)
	if spec.ID != TextHashVectorizerID {
		return nil
	}

	dimensions := spec.Dimensions
	if dimensions == 0 {
		dimensions = len(doc.Vector)
	}

	vectorizer, err := NewTextVectorizer(dimensions)
	if err != nil {
		return fmt.Errorf("validate text-hash vector %q: %w", doc.ID, err)
	}

	expected, err := vectorizer.Vectorize(doc.Text)
	if err != nil {
		return fmt.Errorf("validate text-hash vector %q: %w", doc.ID, err)
	}

	if !vectorsEqual(doc.Vector, expected) {
		return fmt.Errorf("%w: document %q", ErrVectorMismatch, doc.ID)
	}

	return nil
}

func sourceHash(text string) string {
	return privacy.SourceHash(text)
}

func validSourceHash(hash string) bool {
	value, ok := strings.CutPrefix(hash, "sha256:")
	if !ok || len(value) != 64 {
		return false
	}

	_, err := hex.DecodeString(value)

	return err == nil
}

// IsZero reports whether the vectorizer identity is absent.
func (s VectorizerSpec) IsZero() bool {
	return s.ID == "" && s.Model == "" && s.Normalization == "" && s.Version == "" && s.Dimensions == 0
}

// CompatibleWith reports whether two vectorizer identities can share a store.
// Empty specs are not considered compatible; callers use IsZero to decide when
// to stamp a missing identity.
func (s VectorizerSpec) CompatibleWith(other VectorizerSpec) bool {
	s = normalizeVectorizerSpec(s)
	other = normalizeVectorizerSpec(other)

	if s.IsZero() || other.IsZero() {
		return false
	}

	return s.ID == other.ID &&
		s.Model == other.Model &&
		s.Normalization == other.Normalization &&
		s.Version == other.Version &&
		(s.Dimensions == 0 || other.Dimensions == 0 || s.Dimensions == other.Dimensions)
}

func normalizeVectorizerSpec(spec VectorizerSpec) VectorizerSpec {
	spec.ID = privacy.RedactIdentifier(strings.TrimSpace(spec.ID))
	spec.Model = privacy.RedactIdentifier(strings.TrimSpace(spec.Model))
	spec.Normalization = privacy.RedactIdentifier(strings.TrimSpace(spec.Normalization))
	spec.Version = privacy.RedactIdentifier(strings.TrimSpace(spec.Version))

	return spec
}

func validatePersistedVectorizerSpecPrivacy(spec VectorizerSpec) error {
	if spec.IsZero() {
		return nil
	}

	if privacy.RedactIdentifier(strings.TrimSpace(spec.ID)) != strings.TrimSpace(spec.ID) ||
		privacy.RedactIdentifier(strings.TrimSpace(spec.Model)) != strings.TrimSpace(spec.Model) ||
		privacy.RedactIdentifier(strings.TrimSpace(spec.Normalization)) != strings.TrimSpace(spec.Normalization) ||
		privacy.RedactIdentifier(strings.TrimSpace(spec.Version)) != strings.TrimSpace(spec.Version) {
		return ErrPrivacyPolicy
	}

	return nil
}

func validateCompleteVectorizerSpec(spec VectorizerSpec, dimensions int) error {
	spec = normalizeVectorizerSpec(spec)
	if err := validateVectorizerIdentity(spec); err != nil {
		return err
	}

	if dimensions <= 0 {
		return ErrInvalidDimensions
	}

	if spec.Dimensions != dimensions {
		return fmt.Errorf("%w: vectorizer dimensions got %d, want %d", ErrDimensionMismatch, spec.Dimensions, dimensions)
	}

	return nil
}

func validateVectorizerIdentity(spec VectorizerSpec) error {
	spec = normalizeVectorizerSpec(spec)
	if spec.Dimensions < 0 {
		return ErrInvalidDimensions
	}

	if spec.ID == "" || spec.Model == "" || spec.Normalization == "" || spec.Version == "" {
		return ErrVectorizerMismatch
	}

	if spec.ID == TextHashVectorizerID &&
		(spec.Model != TextHashVectorizerModel ||
			spec.Normalization != TextHashVectorizerNormalization ||
			spec.Version != vectorizerSpecVersion) {
		return ErrVectorizerMismatch
	}

	return nil
}

func validateOptionalVectorizerIdentity(spec VectorizerSpec) error {
	spec = normalizeVectorizerSpec(spec)
	if spec.IsZero() {
		return nil
	}

	return validateVectorizerIdentity(spec)
}

func vectorsEqual(left, right Vector) bool {
	if len(left) != len(right) {
		return false
	}

	for i := range left {
		if left[i] != right[i] {
			return false
		}
	}

	return true
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

func hashToken(token string) uint64 {
	h := fnv.New64a()
	_, _ = h.Write([]byte(token))

	return h.Sum64()
}

var (
	// ErrMissingID is returned when indexing a document without an ID.
	ErrMissingID = errors.New("vector document id is required")
	// ErrDuplicateID is returned when persisted JSON has duplicate document IDs.
	ErrDuplicateID = errors.New("vector duplicate document id")
	// ErrInvalidDimensions is returned when a store or vectorizer dimension is invalid.
	ErrInvalidDimensions = errors.New("vector dimensions must be positive or zero for default")
	// ErrDimensionMismatch is returned when vector dimensions do not match.
	ErrDimensionMismatch = errors.New("vector dimension mismatch")
	// ErrEmptyVector is returned when a vector has no dimensions.
	ErrEmptyVector = errors.New("vector is empty")
	// ErrZeroVector is returned when cosine similarity cannot be computed.
	ErrZeroVector = errors.New("vector has zero magnitude")
	// ErrInvalidVector is returned when a vector contains NaN or infinity.
	ErrInvalidVector = errors.New("vector contains invalid value")
	// ErrInvalidUTF8 is returned when vector text is not valid UTF-8.
	ErrInvalidUTF8 = errors.New("vector text is not valid UTF-8")
	// ErrEmptyText is returned when text vectorization has no tokens.
	ErrEmptyText = errors.New("vector text has no tokens")
	// ErrVectorizerMismatch is returned when a store sees incompatible vectorizer metadata.
	ErrVectorizerMismatch = errors.New("vectorizer metadata mismatch")
	// ErrContextRequired is returned when a network vectorizer is called without caller context.
	ErrContextRequired = errors.New("vectorizer requires caller-provided context")
	// ErrIncompatibleSchema is returned when a vector store has an unsupported schema.
	ErrIncompatibleSchema = errors.New("vector store schema version is incompatible")
	// ErrVectorizerRequired is returned when rebuilding vectors without a vectorizer.
	ErrVectorizerRequired = errors.New("vectorizer is required")
	// ErrSourceHashMismatch is returned when text no longer matches source hash metadata.
	ErrSourceHashMismatch = errors.New("vector source hash mismatch")
	// ErrPrivacyPolicy is returned when persisted vector text/metadata still contains sensitive values.
	ErrPrivacyPolicy = errors.New("vector store violates privacy policy")
	// ErrProvenanceMissing is returned when persisted vectors lack source provenance metadata.
	ErrProvenanceMissing = errors.New("vector provenance metadata is required")
	// ErrVectorMismatch is returned when a deterministic text-hash vector no longer matches its text.
	ErrVectorMismatch = errors.New("vector does not match source text")
	// ErrNoSources is returned when building an index without source files.
	ErrNoSources = errors.New("vector index requires at least one source")
	// ErrIndexCorrupt is returned when a persisted vector index is not valid JSON.
	ErrIndexCorrupt = errors.New("vector index is corrupt")
	// ErrMetadataMismatch is returned when a persisted index was built with a
	// different vectorizer or model than the current search request.
	ErrMetadataMismatch = errors.New("vector index metadata mismatch")
	// ErrSourceStale is returned when a persisted index source digest no longer
	// matches the current file content.
	ErrSourceStale = errors.New("vector index source is stale")
)
