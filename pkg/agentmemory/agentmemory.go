// Package agentmemory provides per-agent vector memory with JSON persistence.
package agentmemory

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/tommoulard/atteler/pkg/privacy"
	"github.com/tommoulard/atteler/pkg/retrieval"
	"github.com/tommoulard/atteler/pkg/vector"
)

const (
	// StoreSchemaVersion is the current JSON persistence schema for agent memory.
	StoreSchemaVersion = 1
)

// Document is a vectorized text item stored for one agent.
type Document struct {
	Metadata   map[string]string     `json:"metadata,omitempty"`
	Provenance map[string]string     `json:"provenance,omitempty"`
	ExpiresAt  *time.Time            `json:"expires_at,omitempty"`
	CreatedAt  time.Time             `json:"created_at,omitzero"`
	UpdatedAt  time.Time             `json:"updated_at,omitzero"`
	Vectorizer vector.VectorizerSpec `json:"vectorizer"`
	ID         string                `json:"id"`
	Path       string                `json:"path,omitempty"`
	Text       string                `json:"text"`
	SourceHash string                `json:"source_hash"`
	Vector     vector.Vector         `json:"vector"`
}

// Result is a vector-ranked agent memory search result.
type Result struct {
	Document Document `json:"document"`
	Score    float64  `json:"score"`
}

// Searcher adapts one agent namespace to the shared retrieval contract.
type Searcher struct {
	Store *Store
	Agent string
}

// Store keeps vectorized memories partitioned by agent name. The persisted JSON
// records vectorizer identity; model-backed vectorizers are process-local and
// must be supplied again after Load before adding/searching.
//
//nolint:govet // Layout prioritizes JSON/API readability over pointer-byte packing.
type Store struct {
	Agents        map[string][]Document `json:"agents"`
	Vectorizer    vector.VectorizerSpec `json:"vectorizer"`
	CreatedAt     time.Time             `json:"created_at,omitzero"`
	UpdatedAt     time.Time             `json:"updated_at,omitzero"`
	vectorizer    vector.Vectorizer     `json:"-"`
	SchemaVersion int                   `json:"schema_version"`
	Dimensions    int                   `json:"dimensions"`
}

// LoadOptions controls persisted store loading.
type LoadOptions struct {
	// Migrate re-embeds legacy or stale documents from their redacted text instead
	// of trusting persisted vectors with missing/incompatible metadata.
	Migrate bool
}

type addOptions struct {
	provenance map[string]string
	expiresAt  *time.Time
	ttl        time.Duration
}

type (
	vectorizeTextFunc   func(string) (vector.Vector, error)
	reembedDocumentFunc func(Document) (Document, error)
	contextErrFunc      func() error
)

// AddOption configures AddWithOptions and AddTextWithOptions.
type AddOption func(*addOptions)

// WithTTL expires the memory after ttl. Non-positive TTLs are ignored.
func WithTTL(ttl time.Duration) AddOption {
	return func(opts *addOptions) {
		opts.ttl = ttl
	}
}

// WithExpiresAt expires the memory at t.
func WithExpiresAt(t time.Time) AddOption {
	return func(opts *addOptions) {
		t = t.UTC()
		opts.expiresAt = &t
	}
}

// WithProvenance records non-sensitive source metadata for the memory.
func WithProvenance(provenance map[string]string) AddOption {
	return func(opts *addOptions) {
		opts.provenance = cleanMetadata(provenance)
	}
}

// NewStore returns an empty per-agent memory store. A zero dimension value uses
// pkg/vector's default hashed lexical vectorizer dimensions.
func NewStore(dimensions int) (*Store, error) {
	vectorizer, err := vector.NewTextVectorizer(dimensions)
	if err != nil {
		return nil, fmt.Errorf("agent memory: create vectorizer: %w", err)
	}

	now := time.Now().UTC()

	return &Store{
		Agents:        make(map[string][]Document),
		Vectorizer:    vectorizer.Spec(),
		vectorizer:    vectorizer,
		SchemaVersion: StoreSchemaVersion,
		Dimensions:    vectorizer.Dimensions,
		CreatedAt:     now,
		UpdatedAt:     now,
	}, nil
}

// NewStoreWithVectorizer returns an empty per-agent memory store pinned to a
// caller-provided vectorizer identity. Use it for embedding-backed agent memory;
// context-aware vectorizers require the *Context methods for add/search/reembed.
func NewStoreWithVectorizer(spec vector.VectorizerSpec, vectorizer vector.Vectorizer) (*Store, error) {
	now := time.Now().UTC()

	store := &Store{
		Agents:        make(map[string][]Document),
		SchemaVersion: StoreSchemaVersion,
		CreatedAt:     now,
		UpdatedAt:     now,
	}

	if err := store.SetVectorizer(spec, vectorizer); err != nil {
		return nil, err
	}

	return store, nil
}

// SetVectorizer binds a process-local vectorizer to a loaded store. Existing
// documents must already declare a compatible persisted vectorizer identity; it
// intentionally refuses silent cross-model searches instead of re-embedding.
func (s *Store) SetVectorizer(spec vector.VectorizerSpec, vectorizer vector.Vectorizer) error {
	if vectorizer == nil {
		return fmt.Errorf("%w: agent memory vectorizer is required", vector.ErrVectorizerMismatch)
	}

	spec = cleanVectorizerSpec(spec)
	if spec.IsZero() {
		return fmt.Errorf("%w: agent memory vectorizer metadata is required", vector.ErrVectorizerMismatch)
	}

	if err := validatePersistedVectorizerSpecPrivacy(spec); err != nil {
		return err
	}

	if s.hasDocuments() {
		if err := s.validateSetVectorizerForExistingDocuments(&spec); err != nil {
			return err
		}
	} else if spec.Dimensions > 0 {
		s.Dimensions = spec.Dimensions
	}

	if s.Dimensions > 0 && spec.Dimensions == 0 {
		spec.Dimensions = s.Dimensions
	}

	s.Vectorizer = spec
	s.vectorizer = vectorizer

	return s.ensureVectorizer()
}

func (s *Store) validateSetVectorizerForExistingDocuments(spec *vector.VectorizerSpec) error {
	if s.Dimensions <= 0 {
		return vector.ErrInvalidDimensions
	}

	if spec.Dimensions == 0 {
		spec.Dimensions = s.Dimensions
	}

	if spec.Dimensions != s.Dimensions {
		return fmt.Errorf("%w: vectorizer dimensions got %d, want %d",
			vector.ErrDimensionMismatch, spec.Dimensions, s.Dimensions)
	}

	if !s.Vectorizer.IsZero() && !s.Vectorizer.CompatibleWith(*spec) {
		return fmt.Errorf("%w: store uses %s/%s, want %s/%s",
			vector.ErrVectorizerMismatch, s.Vectorizer.ID, s.Vectorizer.Model, spec.ID, spec.Model)
	}

	return nil
}

// AddText vectorizes and stores text for agent under id. Existing documents for
// the same agent and id are replaced.
func (s *Store) AddText(agent, id, text string) error {
	return s.Add(agent, Document{ID: id, Text: text})
}

// AddTextContext is AddText with caller-provided cancellation for
// context-aware vectorizers.
func (s *Store) AddTextContext(ctx context.Context, agent, id, text string) error {
	return s.AddContext(ctx, agent, Document{ID: id, Text: text})
}

// AddTextWithOptions is AddText with TTL/provenance support.
func (s *Store) AddTextWithOptions(agent, id, text string, opts ...AddOption) error {
	return s.AddWithOptions(agent, Document{ID: id, Text: text}, opts...)
}

// AddTextWithOptionsContext is AddTextWithOptions with caller-provided
// cancellation for context-aware vectorizers.
func (s *Store) AddTextWithOptionsContext(ctx context.Context, agent, id, text string, opts ...AddOption) error {
	return s.AddWithOptionsContext(ctx, agent, Document{ID: id, Text: text}, opts...)
}

// AddFile reads and stores a UTF-8 text file for agent. The cleaned filepath is
// used as the document ID and Path.
func (s *Store) AddFile(agent, path string) error {
	return s.AddFileWithOptions(agent, path)
}

// AddFileWithOptions is AddFile with TTL/provenance support.
func (s *Store) AddFileWithOptions(agent, path string, opts ...AddOption) error {
	doc, err := agentMemoryDocumentFromFile(path)
	if err != nil {
		return err
	}

	return s.AddWithOptions(agent, doc, opts...)
}

// AddFileWithOptionsContext is AddFileWithOptions with caller-provided
// cancellation for context-aware vectorizers.
func (s *Store) AddFileWithOptionsContext(ctx context.Context, agent, path string, opts ...AddOption) error {
	if ctx == nil {
		return vector.ErrContextRequired
	}

	doc, err := agentMemoryDocumentFromFile(path)
	if err != nil {
		return err
	}

	return s.AddWithOptionsContext(ctx, agent, doc, opts...)
}

func agentMemoryDocumentFromFile(path string) (Document, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return Document{}, fmt.Errorf("read agent memory file %q: %w", path, err)
	}

	if !utf8.Valid(data) {
		return Document{}, fmt.Errorf("read agent memory file %q: %w", path, ErrInvalidUTF8)
	}

	clean := filepath.Clean(path)

	metadata := map[string]string{"path": clean}
	if info, statErr := os.Stat(path); statErr == nil && !info.ModTime().IsZero() {
		metadata[retrieval.MetadataSourceUpdatedAt] = info.ModTime().UTC().Format(time.RFC3339Nano)
	}

	return Document{
		ID:         clean,
		Path:       clean,
		Text:       string(data),
		Metadata:   metadata,
		Provenance: fileProvenance(clean),
	}, nil
}

// Add vectorizes and stores doc for agent. Existing documents for the same
// agent and document ID are replaced.
func (s *Store) Add(agent string, doc Document) error {
	return s.AddWithOptions(agent, doc)
}

// AddContext is Add with caller-provided cancellation for context-aware
// vectorizers.
func (s *Store) AddContext(ctx context.Context, agent string, doc Document) error {
	return s.AddWithOptionsContext(ctx, agent, doc)
}

// AddWithOptions vectorizes and stores doc with TTL/provenance support.
func (s *Store) AddWithOptions(agent string, doc Document, opts ...AddOption) error {
	return s.addWithOptions(agent, doc, s.vectorize, opts...)
}

// AddWithOptionsContext vectorizes and stores doc with TTL/provenance support
// using caller-provided cancellation for context-aware vectorizers.
func (s *Store) AddWithOptionsContext(ctx context.Context, agent string, doc Document, opts ...AddOption) error {
	if ctx == nil {
		return vector.ErrContextRequired
	}

	return s.addWithOptions(agent, doc, func(text string) (vector.Vector, error) {
		return s.vectorizeContext(ctx, text)
	}, opts...)
}

func (s *Store) addWithOptions(agent string, doc Document, vectorize vectorizeTextFunc, opts ...AddOption) error {
	agent, doc, hasCreatedAt, err := s.prepareAddDocument(agent, doc, opts...)
	if err != nil {
		return err
	}

	vec, err := vectorize(doc.Text)
	if err != nil {
		return err
	}

	if err := s.adoptVectorDimensions(len(vec)); err != nil {
		return err
	}

	doc.Vector = cloneVector(vec)
	doc.Vectorizer = s.Vectorizer

	s.storePreparedDocument(agent, doc, hasCreatedAt)

	return nil
}

func (s *Store) prepareAddDocument(
	rawAgent string,
	doc Document,
	opts ...AddOption,
) (agent string, prepared Document, hasCreatedAt bool, err error) {
	agent, agentErr := redactAgentName(rawAgent)
	if agentErr != nil {
		return "", Document{}, false, agentErr
	}

	id, idErr := redactDocumentID(doc.ID)
	if idErr != nil {
		return "", Document{}, false, idErr
	}

	doc.ID = id

	if !utf8.ValidString(doc.Text) {
		return "", Document{}, false, ErrInvalidUTF8
	}

	if err := s.ensureVectorizer(); err != nil {
		return "", Document{}, false, err
	}

	applied := applyAddOptions(opts...)
	if applied.ttl > 0 {
		expiresAt := time.Now().UTC().Add(applied.ttl)
		applied.expiresAt = &expiresAt
	}

	if applied.expiresAt != nil {
		doc.ExpiresAt = cloneTimePtr(applied.expiresAt)
	}

	if len(applied.provenance) > 0 {
		doc.Provenance = mergeMetadata(doc.Provenance, applied.provenance)
	}

	doc.Text = privacy.RedactText(doc.Text)
	doc.Path = privacy.RedactIdentifier(strings.TrimSpace(doc.Path))
	doc.Metadata = privacy.RedactMetadata(doc.Metadata)
	doc.Provenance = ensureProvenance(cleanMetadata(doc.Provenance), "direct")
	doc.SourceHash = privacy.SourceHash(doc.Text)

	hasCreatedAt = !doc.CreatedAt.IsZero()

	return agent, doc, hasCreatedAt, nil
}

func (s *Store) storePreparedDocument(agent string, doc Document, hasCreatedAt bool) {
	now := time.Now().UTC()

	if doc.CreatedAt.IsZero() {
		doc.CreatedAt = now
	}

	doc.UpdatedAt = now

	if s.Agents == nil {
		s.Agents = make(map[string][]Document)
	}

	if s.CreatedAt.IsZero() {
		s.CreatedAt = now
	}

	s.UpdatedAt = now

	docs := s.Agents[agent]
	for i := range docs {
		if docs[i].ID == doc.ID {
			if !hasCreatedAt {
				doc.CreatedAt = docs[i].CreatedAt
			}

			docs[i] = cloneDocument(doc)
			s.Agents[agent] = docs

			return
		}
	}

	s.Agents[agent] = append(docs, cloneDocument(doc))
}

// Search vectorizes query and returns results from only agent's documents. A
// limit less than one returns every non-zero match.
func (s *Store) Search(agent, query string, limit int) ([]Result, error) {
	return s.searchWithVectorize(agent, query, limit, s.vectorize)
}

// SearchContext is Search with caller-provided cancellation for context-aware
// vectorizers.
func (s *Store) SearchContext(ctx context.Context, agent, query string, limit int) ([]Result, error) {
	if ctx == nil {
		return nil, vector.ErrContextRequired
	}

	return s.searchWithVectorize(agent, query, limit, func(text string) (vector.Vector, error) {
		return s.vectorizeContext(ctx, text)
	})
}

func (s *Store) searchWithVectorize(agent, query string, limit int, vectorize vectorizeTextFunc) ([]Result, error) {
	agent, agentErr := redactAgentName(agent)
	if agentErr != nil {
		return nil, agentErr
	}

	if !utf8.ValidString(query) {
		return nil, ErrInvalidUTF8
	}

	if err := s.ensureVectorizer(); err != nil {
		return nil, err
	}

	queryVector, err := vectorize(privacy.RedactText(query))
	if err != nil {
		return nil, err
	}

	docs := s.Agents[agent]
	if len(docs) == 0 {
		return nil, nil
	}

	store, err := vector.NewStoreWithVectorizer(s.Vectorizer)
	if err != nil {
		return nil, fmt.Errorf("agent memory: create vector store: %w", err)
	}

	now := time.Now().UTC()
	seen := make(map[string]struct{}, len(docs))
	indexedDocs := make([]Document, 0, len(docs))

	for i := range docs {
		doc := docs[i]
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

		if validateErr := s.validateSearchDocument(doc); validateErr != nil {
			return nil, fmt.Errorf("agent memory: validate search document %q: %w", doc.ID, validateErr)
		}

		indexedDocs = append(indexedDocs, cloneDocument(doc))

		if addErr := store.Add(vector.Document{
			ID:         doc.ID,
			Text:       doc.Text,
			Metadata:   doc.Metadata,
			Provenance: doc.Provenance,
			Vector:     doc.Vector,
			Vectorizer: doc.Vectorizer,
			SourceHash: doc.SourceHash,
			CreatedAt:  doc.CreatedAt,
			UpdatedAt:  doc.UpdatedAt,
			ExpiresAt:  doc.ExpiresAt,
		}); addErr != nil {
			return nil, fmt.Errorf("index agent memory document %q: %w", doc.ID, addErr)
		}
	}

	vectorResults, err := store.SearchWithVectorizer(queryVector, s.Vectorizer, limit)
	if err != nil {
		return nil, fmt.Errorf("agent memory: search vector store: %w", err)
	}

	results := make([]Result, 0, len(vectorResults))
	for i := range vectorResults {
		result := vectorResults[i]
		results = append(results, Result{
			Document: documentFromVector(result.Document, indexedDocs),
			Score:    result.Score,
		})
	}

	return results, nil
}

// SearchRetrieval returns agent memory hits using the shared retrieval
// contract.
func (s *Store) SearchRetrieval(ctx context.Context, agent string, query retrieval.Query) ([]retrieval.Result, error) {
	searcher := Searcher{Store: s, Agent: agent}

	return searcher.SearchRetrieval(ctx, query)
}

// SearchRetrieval implements retrieval.Searcher for one agent namespace.
func (s Searcher) SearchRetrieval(ctx context.Context, query retrieval.Query) ([]retrieval.Result, error) {
	if ctx == nil {
		return nil, vector.ErrContextRequired
	}

	if err := ctx.Err(); err != nil {
		return nil, fmt.Errorf("agent memory retrieval: %w", err)
	}

	if s.Store == nil {
		return nil, nil
	}

	results, err := s.Store.SearchContext(ctx, s.Agent, query.Text, query.Limit)
	if err != nil {
		return nil, err
	}

	out := make([]retrieval.Result, 0, len(results))
	for i := range results {
		out = append(out, agentRetrievalResult(s.Agent, results[i], query))
	}

	return out, nil
}

// Delete removes memories for agent and reports whether anything was removed.
func (s *Store) Delete(agent, id string) bool {
	redactedAgent, agentErr := redactAgentName(agent)

	redactedID, idErr := redactDocumentID(id)
	if agentErr != nil || idErr != nil {
		return false
	}

	agent = redactedAgent
	id = redactedID

	docs := s.Agents[agent]
	kept := docs[:0]
	removed := false

	for i := range docs {
		if docs[i].ID == id {
			removed = true
			continue
		}

		kept = append(kept, docs[i])
	}

	if removed {
		clear(docs[len(kept):cap(docs)])

		if len(kept) == 0 {
			delete(s.Agents, agent)
		} else {
			s.Agents[agent] = kept
		}

		s.UpdatedAt = time.Now().UTC()

		return true
	}

	return false
}

// Compact removes expired memories and returns the number of purged entries.
func (s *Store) Compact(now time.Time) int {
	now = now.UTC()

	removed := 0

	for agent, docs := range s.Agents {
		kept := docs[:0]
		for i := range docs {
			if isExpired(docs[i], now) {
				removed++
				continue
			}

			kept = append(kept, docs[i])
		}

		clear(docs[len(kept):cap(docs)])

		if len(kept) == 0 {
			delete(s.Agents, agent)
			continue
		}

		s.Agents[agent] = kept
	}

	if removed > 0 {
		s.UpdatedAt = time.Now().UTC()
	}

	return removed
}

// Save writes the store as pretty-printed JSON.
func (s *Store) Save(path string) error {
	s.Compact(time.Now().UTC())

	if err := s.ensureVectorizer(); err != nil {
		return err
	}

	if err := s.validateLoaded(); err != nil {
		return err
	}

	data, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal agent memory store: %w", err)
	}

	data = append(data, '\n')

	if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
		return fmt.Errorf("create agent memory store dir: %w", err)
	}

	if err := os.WriteFile(path, data, 0o600); err != nil {
		return fmt.Errorf("write agent memory store %q: %w", path, err)
	}

	if err := os.Chmod(path, 0o600); err != nil {
		return fmt.Errorf("chmod agent memory store %q: %w", path, err)
	}

	return nil
}

// Load reads a JSON store saved by Save.
func Load(path string) (*Store, error) {
	return LoadWithOptions(path, LoadOptions{})
}

// LoadWithOptions reads a JSON store and optionally migrates stale vector data
// by rebuilding vectors from redacted persisted text.
func LoadWithOptions(path string, opts LoadOptions) (*Store, error) {
	return loadWithOptions(path, opts, func(store *Store) error {
		return store.Migrate()
	})
}

// LoadWithOptionsContext is LoadWithOptions with caller-provided cancellation
// for migrations that rebuild vectors from persisted text.
func LoadWithOptionsContext(ctx context.Context, path string, opts LoadOptions) (*Store, error) {
	if ctx == nil {
		return nil, vector.ErrContextRequired
	}

	if err := ctx.Err(); err != nil {
		return nil, fmt.Errorf("agent memory: load canceled: %w", err)
	}

	return loadWithOptions(path, opts, func(store *Store) error {
		return store.MigrateContext(ctx)
	})
}

func loadWithOptions(path string, opts LoadOptions, migrate func(*Store) error) (*Store, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read agent memory store %q: %w", path, err)
	}

	var store Store
	if err := json.Unmarshal(data, &store); err != nil {
		return nil, fmt.Errorf("decode agent memory store %q: %w", path, err)
	}

	if opts.Migrate {
		if err := migrate(&store); err != nil {
			return nil, fmt.Errorf("migrate agent memory store %q: %w", path, err)
		}

		return &store, nil
	}

	store.Compact(time.Now().UTC())

	if err := store.validateLoaded(); err != nil {
		return nil, fmt.Errorf("validate agent memory store %q: %w", path, err)
	}

	return &store, nil
}

// Migrate updates a loaded legacy/stale store to the current schema by
// redacting text/metadata and rebuilding every vector with the current local
// text-hash vectorizer.
func (s *Store) Migrate() error {
	return s.migrate(s.reembedDocument, noContextErr)
}

// MigrateContext is Migrate with caller-provided cancellation between
// documents.
func (s *Store) MigrateContext(ctx context.Context) error {
	if ctx == nil {
		return vector.ErrContextRequired
	}

	return s.migrate(func(doc Document) (Document, error) {
		return s.reembedDocumentContext(ctx, doc)
	}, ctx.Err)
}

func (s *Store) migrate(reembed reembedDocumentFunc, checkContext contextErrFunc) error {
	if s.SchemaVersion < 0 || s.SchemaVersion > StoreSchemaVersion {
		return ErrIncompatibleSchema
	}

	now := time.Now().UTC()
	s.Compact(now)

	if s.Dimensions < 0 && !s.hasDocuments() {
		s.Dimensions = 0
	} else if s.Dimensions < 0 {
		return vector.ErrInvalidDimensions
	}

	if s.Dimensions == 0 {
		fresh, err := NewStore(0)
		if err != nil {
			return err
		}

		s.Dimensions = fresh.Dimensions
	}

	vectorizer, err := vector.NewTextVectorizer(s.Dimensions)
	if err != nil {
		return fmt.Errorf("agent memory: create migration vectorizer: %w", err)
	}

	s.SchemaVersion = StoreSchemaVersion
	s.Vectorizer = vectorizer.Spec()
	s.vectorizer = vectorizer

	if s.CreatedAt.IsZero() {
		s.CreatedAt = now
	}

	s.UpdatedAt = now

	if s.Agents == nil {
		s.Agents = make(map[string][]Document)
	}

	normalizedAgents := make(map[string][]Document, len(s.Agents))
	for agent, docs := range s.Agents {
		if err := checkContext(); err != nil {
			return fmt.Errorf("agent memory: migrate canceled: %w", err)
		}

		normalizedAgent, err := normalizeLoadedAgentName(agent, true)
		if err != nil {
			return err
		}

		normalizedDocs, err := s.migrateAgent(normalizedAgent, docs, reembed, checkContext)
		if err != nil {
			return err
		}

		normalizedAgents[normalizedAgent] = append(normalizedAgents[normalizedAgent], normalizedDocs...)
	}

	s.Agents = normalizedAgents

	return s.validateLoaded()
}

// ReembedAll rebuilds every vector with the store's current vectorizer.
func (s *Store) ReembedAll() error {
	return s.reembedAll(s.reembedDocument, noContextErr)
}

// ReembedAllContext rebuilds every vector with caller-provided cancellation for
// context-aware vectorizers.
func (s *Store) ReembedAllContext(ctx context.Context) error {
	if ctx == nil {
		return vector.ErrContextRequired
	}

	return s.reembedAll(func(doc Document) (Document, error) {
		return s.reembedDocumentContext(ctx, doc)
	}, ctx.Err)
}

func (s *Store) reembedAll(reembed reembedDocumentFunc, checkContext contextErrFunc) error {
	s.Compact(time.Now().UTC())

	if err := s.ensureVectorizer(); err != nil {
		return err
	}

	normalizedAgents := make(map[string][]Document, len(s.Agents))
	for agent, docs := range s.Agents {
		normalizedAgent, err := normalizeLoadedAgentName(agent, true)
		if err != nil {
			return err
		}

		for i := range docs {
			if err := checkContext(); err != nil {
				return fmt.Errorf("agent memory: re-embed canceled: %w", err)
			}

			updated, err := reembed(docs[i])
			if err != nil {
				return fmt.Errorf("agent memory: re-embed %s/%s: %w", agent, docs[i].ID, err)
			}

			docs[i] = updated
		}

		normalizedAgents[normalizedAgent] = append(normalizedAgents[normalizedAgent], docs...)
	}

	s.Agents = normalizedAgents
	s.UpdatedAt = time.Now().UTC()

	return s.validateLoaded()
}

// Reembed rebuilds one agent document vector with the store's current vectorizer.
func (s *Store) Reembed(agent, id string) error {
	return s.reembed(agent, id, s.reembedDocument)
}

// ReembedContext rebuilds one agent document vector with caller-provided
// cancellation for context-aware vectorizers.
func (s *Store) ReembedContext(ctx context.Context, agent, id string) error {
	if ctx == nil {
		return vector.ErrContextRequired
	}

	return s.reembed(agent, id, func(doc Document) (Document, error) {
		return s.reembedDocumentContext(ctx, doc)
	})
}

func (s *Store) reembed(agent, id string, reembed reembedDocumentFunc) error {
	agent, agentErr := redactAgentName(agent)
	if agentErr != nil {
		return agentErr
	}

	redactedID, idErr := redactDocumentID(id)
	if idErr != nil {
		return idErr
	}

	id = redactedID

	if err := s.ensureVectorizer(); err != nil {
		return err
	}

	docs := s.Agents[agent]
	for i := range docs {
		if docs[i].ID != id {
			continue
		}

		updated, err := reembed(docs[i])
		if err != nil {
			return err
		}

		docs[i] = updated
		s.Agents[agent] = docs
		s.UpdatedAt = time.Now().UTC()

		return nil
	}

	return fmt.Errorf("%w: %s", ErrMissingID, id)
}

// MigrateTextDimensions rebuilds the store using the built-in text hash
// vectorizer at dimensions.
func (s *Store) MigrateTextDimensions(dimensions int) error {
	vectorizer, err := vector.NewTextVectorizer(dimensions)
	if err != nil {
		return fmt.Errorf("agent memory: create vectorizer: %w", err)
	}

	s.Dimensions = vectorizer.Dimensions
	s.Vectorizer = vectorizer.Spec()
	s.vectorizer = vectorizer
	s.SchemaVersion = StoreSchemaVersion

	return s.ReembedAll()
}

// MigrateToVectorizerContext rebuilds every persisted document with a new
// caller-provided vectorizer. It is intended for controlled transitions from
// the lexical fallback store to an embedding-backed per-agent memory store.
func (s *Store) MigrateToVectorizerContext(ctx context.Context, spec vector.VectorizerSpec, vectorizer vector.Vectorizer) error {
	if ctx == nil {
		return vector.ErrContextRequired
	}

	if vectorizer == nil {
		return fmt.Errorf("%w: agent memory vectorizer is required", vector.ErrVectorizerMismatch)
	}

	if err := ctx.Err(); err != nil {
		return fmt.Errorf("agent memory: migrate vectorizer canceled: %w", err)
	}

	spec = cleanVectorizerSpec(spec)
	if spec.IsZero() {
		return fmt.Errorf("%w: agent memory vectorizer metadata is required", vector.ErrVectorizerMismatch)
	}

	if err := validatePersistedVectorizerSpecPrivacy(spec); err != nil {
		return err
	}

	migrated := cloneStoreForVectorizerMigration(s)
	migrated.SchemaVersion = StoreSchemaVersion
	migrated.Vectorizer = spec
	migrated.vectorizer = vectorizer
	migrated.Dimensions = spec.Dimensions

	if err := migrated.migrateToCurrentVectorizer(ctx, vectorizer); err != nil {
		return err
	}

	*s = migrated

	return nil
}

func (s *Store) migrateToCurrentVectorizer(ctx context.Context, vectorizer vector.Vectorizer) error {
	if s.SchemaVersion < 0 || s.SchemaVersion > StoreSchemaVersion {
		return ErrIncompatibleSchema
	}

	now := time.Now().UTC()
	s.Compact(now)

	if s.CreatedAt.IsZero() {
		s.CreatedAt = now
	}

	s.UpdatedAt = now

	if s.Agents == nil {
		s.Agents = make(map[string][]Document)
	}

	normalizedAgents := make(map[string][]Document, len(s.Agents))
	for agent, docs := range s.Agents {
		if err := ctx.Err(); err != nil {
			return fmt.Errorf("agent memory: migrate vectorizer canceled: %w", err)
		}

		normalizedAgent, err := normalizeLoadedAgentName(agent, true)
		if err != nil {
			return err
		}

		normalizedDocs, err := s.reembedAgentWithCurrentVectorizer(ctx, vectorizer, docs)
		if err != nil {
			return fmt.Errorf("agent memory: migrate vectorizer %s: %w", normalizedAgent, err)
		}

		normalizedAgents[normalizedAgent] = append(normalizedAgents[normalizedAgent], normalizedDocs...)
	}

	s.Agents = normalizedAgents

	return s.validateLoaded()
}

func (s *Store) reembedAgentWithCurrentVectorizer(
	ctx context.Context,
	vectorizer vector.Vectorizer,
	docs []Document,
) ([]Document, error) {
	seen := make(map[string]struct{}, len(docs))
	for i := range docs {
		if err := ctx.Err(); err != nil {
			return nil, fmt.Errorf("migrate canceled: %w", err)
		}

		normalized, err := s.normalizeLoadedDocument(docs[i], seen, true)
		if err != nil {
			return nil, err
		}

		updated, err := s.reembedDocumentWithVectorize(normalized, func(text string) (vector.Vector, error) {
			return vectorizeAgentMemoryWithContext(ctx, vectorizer, text)
		})
		if err != nil {
			return nil, fmt.Errorf("re-embed %s: %w", normalized.ID, err)
		}

		docs[i] = updated
	}

	return docs, nil
}

// Documents returns a defensive copy of agent's documents.
func (s *Store) Documents(agent string) []Document {
	agent, err := redactAgentName(agent)
	if err != nil {
		return nil
	}

	docs := s.Agents[agent]

	out := make([]Document, 0, len(docs))
	for i := range docs {
		out = append(out, cloneDocument(docs[i]))
	}

	return out
}

func (s *Store) runtimeVectorizer() (vector.Vectorizer, error) {
	if err := s.ensureVectorizer(); err != nil {
		return nil, err
	}

	vectorizer := s.vectorizer
	if vectorizer == nil {
		if !s.usesTextHashVectorizer() {
			return nil, fmt.Errorf("%w: agent memory runtime vectorizer is required for %s/%s",
				vector.ErrVectorizerMismatch, s.Vectorizer.ID, s.Vectorizer.Model)
		}

		textVectorizer, err := vector.NewTextVectorizer(s.Dimensions)
		if err != nil {
			return nil, fmt.Errorf("agent memory: create vectorizer: %w", err)
		}

		vectorizer = textVectorizer
	}

	return vectorizer, nil
}

func (s *Store) vectorize(text string) (vector.Vector, error) {
	vectorizer, err := s.runtimeVectorizer()
	if err != nil {
		return nil, err
	}

	vec, err := vectorizer.Vectorize(text)
	if err != nil {
		return nil, fmt.Errorf("agent memory: vectorize text: %w", err)
	}

	return vec, nil
}

func vectorizeAgentMemoryWithContext(ctx context.Context, vectorizer vector.Vectorizer, text string) (vector.Vector, error) {
	if ctx == nil {
		return nil, vector.ErrContextRequired
	}

	if err := ctx.Err(); err != nil {
		return nil, fmt.Errorf("agent memory: vectorize context: %w", err)
	}

	if contextual, ok := vectorizer.(vector.VectorizerContext); ok {
		vec, vectorizeErr := contextual.VectorizeContext(ctx, text)
		if vectorizeErr != nil {
			return nil, fmt.Errorf("agent memory: vectorize text: %w", vectorizeErr)
		}

		return vec, nil
	}

	vec, err := vectorizer.Vectorize(text)
	if err != nil {
		return nil, fmt.Errorf("agent memory: vectorize text: %w", err)
	}

	return vec, nil
}

func (s *Store) vectorizeContext(ctx context.Context, text string) (vector.Vector, error) {
	if ctx == nil {
		return nil, vector.ErrContextRequired
	}

	vectorizer, err := s.runtimeVectorizer()
	if err != nil {
		return nil, err
	}

	return vectorizeAgentMemoryWithContext(ctx, vectorizer, text)
}

func (s *Store) ensureVectorizer() error {
	if s.SchemaVersion == 0 {
		if s.hasDocuments() {
			return ErrIncompatibleSchema
		}

		s.SchemaVersion = StoreSchemaVersion
	}

	if s.SchemaVersion != StoreSchemaVersion {
		return ErrIncompatibleSchema
	}

	hasDocuments := s.hasDocuments()
	if s.hasCustomVectorizer() {
		return s.ensureCustomVectorizer(hasDocuments)
	}

	if err := s.ensureVectorizerDimensions(hasDocuments); err != nil {
		return err
	}

	wanted := vector.TextVectorizerSpec(s.Dimensions)

	return s.ensureVectorizerSpec(wanted, hasDocuments)
}

func (s *Store) ensureCustomVectorizer(hasDocuments bool) error {
	if err := s.validateCustomVectorizerIdentity(); err != nil {
		return err
	}

	return s.normalizeCustomVectorizerDimensions(hasDocuments)
}

func (s *Store) validateCustomVectorizerIdentity() error {
	if s.Dimensions < 0 || s.Vectorizer.Dimensions < 0 {
		return vector.ErrInvalidDimensions
	}

	if privacyErr := validatePersistedVectorizerSpecPrivacy(s.Vectorizer); privacyErr != nil {
		return privacyErr
	}

	s.Vectorizer = cleanVectorizerSpec(s.Vectorizer)
	if s.Vectorizer.IsZero() {
		return vector.ErrVectorizerMismatch
	}

	if _, err := vector.NewStoreWithVectorizer(s.Vectorizer); err != nil {
		return fmt.Errorf("agent memory: validate custom vectorizer: %w", err)
	}

	return nil
}

func (s *Store) normalizeCustomVectorizerDimensions(hasDocuments bool) error {
	if s.Dimensions == 0 && s.Vectorizer.Dimensions > 0 {
		s.Dimensions = s.Vectorizer.Dimensions
	}

	if s.Vectorizer.Dimensions == 0 && s.Dimensions > 0 {
		s.Vectorizer.Dimensions = s.Dimensions
	}

	if hasDocuments && s.Dimensions <= 0 {
		return vector.ErrInvalidDimensions
	}

	if hasDocuments && s.Vectorizer.Dimensions <= 0 {
		return fmt.Errorf("%w: store vectorizer dimensions got 0, want %d", vector.ErrDimensionMismatch, s.Dimensions)
	}

	if s.Dimensions > 0 && s.Vectorizer.Dimensions > 0 && s.Vectorizer.Dimensions != s.Dimensions {
		return fmt.Errorf("%w: store vectorizer dimensions got %d, want %d",
			vector.ErrDimensionMismatch, s.Vectorizer.Dimensions, s.Dimensions)
	}

	return nil
}

func (s *Store) adoptVectorDimensions(dimensions int) error {
	if dimensions <= 0 {
		return vector.ErrInvalidDimensions
	}

	if s.Dimensions == 0 {
		s.Dimensions = dimensions
	}

	if s.Dimensions != dimensions {
		return fmt.Errorf("%w: vector dimensions got %d, want %d", vector.ErrDimensionMismatch, dimensions, s.Dimensions)
	}

	if s.Vectorizer.Dimensions == 0 {
		s.Vectorizer.Dimensions = dimensions
	}

	if s.Vectorizer.Dimensions != dimensions {
		return fmt.Errorf("%w: vectorizer dimensions got %d, want %d",
			vector.ErrDimensionMismatch, s.Vectorizer.Dimensions, dimensions)
	}

	return nil
}

func (s *Store) ensureVectorizerDimensions(hasDocuments bool) error {
	if s.Dimensions < 0 && hasDocuments {
		return vector.ErrInvalidDimensions
	}

	if s.Dimensions < 0 {
		s.Dimensions = 0
	}

	if s.Dimensions != 0 {
		return nil
	}

	vectorizer, err := vector.NewTextVectorizer(0)
	if err != nil {
		return fmt.Errorf("agent memory: create default vectorizer: %w", err)
	}

	s.Dimensions = vectorizer.Dimensions

	return nil
}

func (s *Store) ensureVectorizerSpec(wanted vector.VectorizerSpec, hasDocuments bool) error {
	if privacyErr := validatePersistedVectorizerSpecPrivacy(s.Vectorizer); privacyErr != nil {
		return privacyErr
	}

	s.Vectorizer = cleanVectorizerSpec(s.Vectorizer)

	if s.Vectorizer.IsZero() {
		if hasDocuments {
			return vector.ErrVectorizerMismatch
		}

		s.Vectorizer = wanted
	}

	if !s.Vectorizer.CompatibleWith(wanted) {
		if hasDocuments {
			return fmt.Errorf("%w: store uses %s/%s, want %s/%s",
				vector.ErrVectorizerMismatch, s.Vectorizer.ID, s.Vectorizer.Model, wanted.ID, wanted.Model)
		}

		s.Vectorizer = wanted
	}

	if s.Vectorizer.Dimensions == 0 {
		if hasDocuments {
			return fmt.Errorf("%w: store vectorizer dimensions got 0, want %d", vector.ErrDimensionMismatch, s.Dimensions)
		}

		s.Vectorizer.Dimensions = s.Dimensions
	}

	if s.Vectorizer.Dimensions != s.Dimensions {
		return fmt.Errorf("%w: store vectorizer dimensions got %d, want %d",
			vector.ErrDimensionMismatch, s.Vectorizer.Dimensions, s.Dimensions)
	}

	return nil
}

func (s *Store) validateLoaded() error {
	if s.SchemaVersion == 0 {
		if s.hasDocuments() {
			return ErrIncompatibleSchema
		}

		s.SchemaVersion = StoreSchemaVersion
	}

	if s.SchemaVersion != StoreSchemaVersion {
		return ErrIncompatibleSchema
	}

	if err := s.ensureLoadedVectorizer(); err != nil {
		return err
	}

	now := time.Now().UTC()
	if s.CreatedAt.IsZero() {
		s.CreatedAt = now
	}

	if s.UpdatedAt.IsZero() {
		s.UpdatedAt = s.CreatedAt
	}

	if s.Agents == nil {
		s.Agents = make(map[string][]Document)
	}

	for agent, docs := range s.Agents {
		normalizedDocs, err := s.validateLoadedAgent(agent, docs)
		if err != nil {
			return err
		}

		s.Agents[agent] = normalizedDocs
	}

	return nil
}

func (s *Store) ensureLoadedVectorizer() error {
	hasDocuments := s.hasDocuments()
	if s.hasCustomVectorizer() {
		return s.ensureCustomVectorizer(hasDocuments)
	}

	if s.Dimensions <= 0 {
		if hasDocuments {
			return vector.ErrInvalidDimensions
		}

		vectorizer, err := vector.NewTextVectorizer(0)
		if err != nil {
			return fmt.Errorf("agent memory: create default vectorizer: %w", err)
		}

		s.Dimensions = vectorizer.Dimensions
	}

	if s.Dimensions <= 0 {
		return vector.ErrInvalidDimensions
	}

	wanted := vector.TextVectorizerSpec(s.Dimensions)
	if privacyErr := validatePersistedVectorizerSpecPrivacy(s.Vectorizer); privacyErr != nil {
		return privacyErr
	}

	s.Vectorizer = cleanVectorizerSpec(s.Vectorizer)

	if s.Vectorizer.IsZero() || !s.Vectorizer.CompatibleWith(wanted) {
		if hasDocuments {
			return vector.ErrVectorizerMismatch
		}

		s.Vectorizer = wanted
	}

	if s.Vectorizer.Dimensions != s.Dimensions {
		if hasDocuments {
			return fmt.Errorf("%w: store vectorizer dimensions got %d, want %d",
				vector.ErrDimensionMismatch, s.Vectorizer.Dimensions, s.Dimensions)
		}

		s.Vectorizer = wanted
	}

	return nil
}

func (s *Store) usesTextHashVectorizer() bool {
	spec := cleanVectorizerSpec(s.Vectorizer)

	return spec.IsZero() || spec.ID == vector.TextHashVectorizerID
}

func (s *Store) hasCustomVectorizer() bool {
	spec := cleanVectorizerSpec(s.Vectorizer)

	return !spec.IsZero() && spec.ID != vector.TextHashVectorizerID
}

func (s *Store) validateSearchDocument(doc Document) error {
	if _, err := normalizeLoadedDocumentID(doc.ID, false); err != nil {
		return err
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

	if err := s.validateTextVectorizable(redactedText); err != nil {
		return err
	}

	if !maps.Equal(doc.Metadata, privacy.RedactMetadata(doc.Metadata)) {
		return ErrPrivacyPolicy
	}

	if !maps.Equal(doc.Provenance, cleanMetadata(doc.Provenance)) {
		return ErrPrivacyPolicy
	}

	if err := validateProvenance(doc.Provenance); err != nil {
		return err
	}

	if doc.SourceHash != privacy.SourceHash(redactedText) {
		return ErrSourceHashMismatch
	}

	if _, err := s.validateDocumentVectorizer(doc); err != nil {
		return err
	}

	if err := s.validateDocumentVector(doc); err != nil {
		return err
	}

	return nil
}

func (s *Store) migrateAgent(
	agent string,
	docs []Document,
	reembed reembedDocumentFunc,
	checkContext contextErrFunc,
) ([]Document, error) {
	if strings.TrimSpace(agent) == "" {
		return nil, ErrMissingAgent
	}

	seen := make(map[string]struct{}, len(docs))
	for i := range docs {
		if err := checkContext(); err != nil {
			return nil, fmt.Errorf("agent memory: migrate canceled: %w", err)
		}

		normalized, err := s.normalizeLoadedDocument(docs[i], seen, true)
		if err != nil {
			return nil, err
		}

		updated, err := reembed(normalized)
		if err != nil {
			return nil, err
		}

		docs[i] = updated
	}

	return docs, nil
}

func (s *Store) validateLoadedAgent(agent string, docs []Document) ([]Document, error) {
	if _, err := normalizeLoadedAgentName(agent, false); err != nil {
		return nil, err
	}

	seen := make(map[string]struct{}, len(docs))

	store, err := vector.NewStoreWithVectorizer(s.Vectorizer)
	if err != nil {
		return nil, fmt.Errorf("agent memory: validate vector store: %w", err)
	}

	for i := range docs {
		normalized, normalizeErr := s.normalizeLoadedDocument(docs[i], seen, false)
		if normalizeErr != nil {
			return nil, normalizeErr
		}

		if addErr := store.Add(vector.Document{
			ID:         normalized.ID,
			Text:       normalized.Text,
			Metadata:   normalized.Metadata,
			Provenance: normalized.Provenance,
			Vector:     normalized.Vector,
			Vectorizer: normalized.Vectorizer,
			SourceHash: normalized.SourceHash,
			CreatedAt:  normalized.CreatedAt,
			UpdatedAt:  normalized.UpdatedAt,
			ExpiresAt:  normalized.ExpiresAt,
		}); addErr != nil {
			return nil, fmt.Errorf("agent memory: validate document %q: %w", normalized.ID, addErr)
		}

		docs[i] = normalized
	}

	return docs, nil
}

func (s *Store) normalizeLoadedDocument(doc Document, seen map[string]struct{}, migrating bool) (Document, error) {
	id, err := normalizeLoadedDocumentID(doc.ID, migrating)
	if err != nil {
		return Document{}, err
	}

	doc.ID = id

	if _, ok := seen[doc.ID]; ok {
		return Document{}, fmt.Errorf("%w: %s", ErrDuplicateID, doc.ID)
	}

	seen[doc.ID] = struct{}{}
	if !utf8.ValidString(doc.Text) {
		return Document{}, ErrInvalidUTF8
	}

	normalized, err := normalizeLoadedPrivacy(doc, migrating)
	if err != nil {
		return Document{}, err
	}

	doc = normalized
	if validateErr := s.validateTextVectorizable(doc.Text); validateErr != nil {
		return Document{}, validateErr
	}

	if doc.CreatedAt.IsZero() {
		doc.CreatedAt = s.CreatedAt
	}

	if doc.UpdatedAt.IsZero() {
		doc.UpdatedAt = doc.CreatedAt
	}

	if migrating {
		doc.SourceHash = privacy.SourceHash(doc.Text)
		doc.Vectorizer = s.Vectorizer

		return doc, nil
	}

	if doc.SourceHash != privacy.SourceHash(doc.Text) {
		return Document{}, fmt.Errorf("%w: %s", ErrSourceHashMismatch, doc.ID)
	}

	if provenanceErr := validateProvenance(doc.Provenance); provenanceErr != nil {
		return Document{}, fmt.Errorf("%w: %s", provenanceErr, doc.ID)
	}

	docVectorizer, err := s.validateDocumentVectorizer(doc)
	if err != nil {
		return Document{}, err
	}

	doc.Vectorizer = docVectorizer

	if err := s.validateDocumentVector(doc); err != nil {
		return Document{}, err
	}

	doc.Vector = cloneVector(doc.Vector)

	return doc, nil
}

func (s *Store) validateDocumentVectorizer(doc Document) (vector.VectorizerSpec, error) {
	if privacyErr := validatePersistedVectorizerSpecPrivacy(doc.Vectorizer); privacyErr != nil {
		return vector.VectorizerSpec{}, fmt.Errorf("%w: %s", privacyErr, doc.ID)
	}

	docVectorizer := cleanVectorizerSpec(doc.Vectorizer)
	if docVectorizer.IsZero() || !docVectorizer.CompatibleWith(s.Vectorizer) {
		return vector.VectorizerSpec{}, fmt.Errorf("%w: %s", vector.ErrVectorizerMismatch, doc.ID)
	}

	if docVectorizer.Dimensions != s.Dimensions {
		return vector.VectorizerSpec{}, fmt.Errorf("%w: document %q vectorizer dimensions got %d, want %d",
			vector.ErrDimensionMismatch, doc.ID, docVectorizer.Dimensions, s.Dimensions)
	}

	return docVectorizer, nil
}

func normalizeLoadedPrivacy(doc Document, migrating bool) (Document, error) {
	redactedText := privacy.RedactText(doc.Text)
	if !migrating && redactedText != doc.Text {
		return Document{}, fmt.Errorf("%w: %s", ErrPrivacyPolicy, doc.ID)
	}

	doc.Text = redactedText
	path := strings.TrimSpace(doc.Path)

	redactedPath := privacy.RedactIdentifier(path)
	if !migrating && redactedPath != path {
		return Document{}, fmt.Errorf("%w: %s", ErrPrivacyPolicy, doc.ID)
	}

	doc.Path = redactedPath

	redactedMetadata := privacy.RedactMetadata(doc.Metadata)
	if !migrating && !maps.Equal(doc.Metadata, redactedMetadata) {
		return Document{}, fmt.Errorf("%w: %s", ErrPrivacyPolicy, doc.ID)
	}

	doc.Metadata = redactedMetadata

	redactedProvenance := cleanMetadata(doc.Provenance)
	if !migrating && !maps.Equal(doc.Provenance, redactedProvenance) {
		return Document{}, fmt.Errorf("%w: %s", ErrPrivacyPolicy, doc.ID)
	}

	if migrating {
		redactedProvenance = ensureProvenance(redactedProvenance, "legacy")
	}

	doc.Provenance = redactedProvenance
	doc.ExpiresAt = cloneTimePtr(doc.ExpiresAt)

	return doc, nil
}

func (s *Store) reembedDocument(doc Document) (Document, error) {
	return s.reembedDocumentWithVectorize(doc, s.vectorize)
}

func (s *Store) reembedDocumentContext(ctx context.Context, doc Document) (Document, error) {
	if ctx == nil {
		return Document{}, vector.ErrContextRequired
	}

	return s.reembedDocumentWithVectorize(doc, func(text string) (vector.Vector, error) {
		return s.vectorizeContext(ctx, text)
	})
}

func (s *Store) reembedDocumentWithVectorize(doc Document, vectorize vectorizeTextFunc) (Document, error) {
	id, idErr := redactDocumentID(doc.ID)
	if idErr != nil {
		return Document{}, idErr
	}

	doc.ID = id

	if !utf8.ValidString(doc.Text) {
		return Document{}, ErrInvalidUTF8
	}

	doc.Text = privacy.RedactText(doc.Text)

	doc.Path = privacy.RedactIdentifier(strings.TrimSpace(doc.Path))
	if err := s.validateTextVectorizable(doc.Text); err != nil {
		return Document{}, err
	}

	doc.Metadata = privacy.RedactMetadata(doc.Metadata)
	doc.Provenance = ensureProvenance(cleanMetadata(doc.Provenance), "reembed")
	doc.SourceHash = privacy.SourceHash(doc.Text)

	vec, err := vectorize(doc.Text)
	if err != nil {
		return Document{}, err
	}

	if err := s.adoptVectorDimensions(len(vec)); err != nil {
		return Document{}, err
	}

	doc.Vector = cloneVector(vec)
	doc.Vectorizer = s.Vectorizer

	if doc.CreatedAt.IsZero() {
		doc.CreatedAt = time.Now().UTC()
	}

	doc.UpdatedAt = time.Now().UTC()

	return doc, nil
}

func documentFromVector(doc vector.Document, originals []Document) Document {
	for i := range originals {
		if originals[i].ID == doc.ID {
			return cloneDocument(originals[i])
		}
	}

	return Document{
		ID:         doc.ID,
		Text:       doc.Text,
		Metadata:   cloneMetadata(doc.Metadata),
		Provenance: cloneMetadata(doc.Provenance),
		Vector:     cloneVector(doc.Vector),
		Vectorizer: doc.Vectorizer,
		SourceHash: doc.SourceHash,
		CreatedAt:  doc.CreatedAt,
		UpdatedAt:  doc.UpdatedAt,
		ExpiresAt:  cloneTimePtr(doc.ExpiresAt),
	}
}

func agentRetrievalResult(agent string, result Result, query retrieval.Query) retrieval.Result {
	doc := result.Document
	source := retrieval.Source{Type: retrieval.SourceAgentMemory, Name: agent, URI: firstNonEmpty(doc.Path, doc.Metadata["path"])}
	metadata := cloneMetadata(doc.Metadata)
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

	if !retrieval.IsDefaultSafety(safety) {
		metadata = retrieval.MergeSafetyMetadata(metadata, safety)
	}

	chunk := retrieval.BestChunkForTerms(doc.ID, text, strings.Fields(strings.ToLower(query.Text)), retrieval.ChunkOptions{})

	scorer := retrieval.Scorer{
		Name: agentMemoryScorerName(doc.Vectorizer),
		Raw:  result.Score,
		Details: map[string]float64{
			"cosine_similarity": result.Score,
		},
	}
	if query.Explain {
		scorer.Explanation = []string{"ranked within one agent namespace by cosine similarity over persisted document vectors"}
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

func agentMemoryScorerName(spec vector.VectorizerSpec) string {
	if spec.ID == vector.TextHashVectorizerID || spec.IsZero() {
		return "agent-memory-lexical-vector-cosine"
	}

	return "agent-memory-embedding-vector-cosine"
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			return value
		}
	}

	return ""
}

func applyAddOptions(opts ...AddOption) addOptions {
	var out addOptions

	for _, opt := range opts {
		if opt != nil {
			opt(&out)
		}
	}

	return out
}

func noContextErr() error {
	return nil
}

func (s *Store) hasDocuments() bool {
	for _, docs := range s.Agents {
		if len(docs) > 0 {
			return true
		}
	}

	return false
}

func mergeMetadata(left, right map[string]string) map[string]string {
	out := cleanMetadata(left)

	right = cleanMetadata(right)
	if len(right) == 0 {
		return out
	}

	if out == nil {
		out = make(map[string]string, len(right))
	}

	for key, value := range right {
		key = strings.TrimSpace(key)
		value = strings.TrimSpace(value)

		if key == "" || value == "" {
			continue
		}

		out[key] = value
	}

	return out
}

func cleanMetadata(metadata map[string]string) map[string]string {
	return privacy.RedactMetadata(metadata)
}

func cleanVectorizerSpec(spec vector.VectorizerSpec) vector.VectorizerSpec {
	return vector.NormalizeVectorizerSpec(spec)
}

func validatePersistedVectorizerSpecPrivacy(spec vector.VectorizerSpec) error {
	if err := vector.ValidateVectorizerSpecPrivacy(spec); err != nil {
		return ErrPrivacyPolicy
	}

	return nil
}

func redactAgentName(agent string) (string, error) {
	return normalizeLoadedAgentName(agent, true)
}

func normalizeLoadedAgentName(agent string, migrating bool) (string, error) {
	trimmed := strings.TrimSpace(agent)
	if trimmed == "" {
		return "", ErrMissingAgent
	}

	redacted := privacy.RedactIdentifier(trimmed)
	if !migrating && (redacted != trimmed || trimmed != agent) {
		return "", fmt.Errorf("%w: %s", ErrPrivacyPolicy, agent)
	}

	return redacted, nil
}

func redactDocumentID(id string) (string, error) {
	id = strings.TrimSpace(id)
	if id == "" {
		return "", ErrMissingID
	}

	return privacy.RedactIdentifier(id), nil
}

func normalizeLoadedDocumentID(id string, migrating bool) (string, error) {
	id = strings.TrimSpace(id)
	if id == "" {
		return "", ErrMissingID
	}

	redacted := privacy.RedactIdentifier(id)
	if !migrating && redacted != id {
		return "", fmt.Errorf("%w: %s", ErrPrivacyPolicy, id)
	}

	return redacted, nil
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

func (s *Store) validateDocumentVector(doc Document) error {
	if len(doc.Vector) != s.Dimensions {
		return fmt.Errorf("%w: document %q vector dimensions got %d, want %d",
			vector.ErrDimensionMismatch, doc.ID, len(doc.Vector), s.Dimensions)
	}

	if !s.usesTextHashVectorizer() {
		return nil
	}

	vectorizer, err := vector.NewTextVectorizer(s.Dimensions)
	if err != nil {
		return fmt.Errorf("agent memory: create validation vectorizer: %w", err)
	}

	expected, err := vectorizer.Vectorize(doc.Text)
	if err != nil {
		return fmt.Errorf("agent memory: validate vector for %q: %w", doc.ID, err)
	}

	if !vectorsEqual(doc.Vector, expected) {
		return fmt.Errorf("%w: %s", ErrVectorMismatch, doc.ID)
	}

	return nil
}

func (s *Store) validateTextVectorizable(text string) error {
	if s.usesTextHashVectorizer() {
		return validateVectorizableText(text)
	}

	if !utf8.ValidString(text) {
		return ErrInvalidUTF8
	}

	if strings.TrimSpace(text) == "" {
		return vector.ErrEmptyText
	}

	return nil
}

func validateVectorizableText(text string) error {
	if !utf8.ValidString(text) {
		return ErrInvalidUTF8
	}

	vectorizer, err := vector.NewTextVectorizer(1)
	if err != nil {
		return fmt.Errorf("agent memory: create validation vectorizer: %w", err)
	}

	_, err = vectorizer.Vectorize(text)
	if err != nil {
		return fmt.Errorf("agent memory: validate vectorizable text: %w", err)
	}

	return nil
}

func fileProvenance(path string) map[string]string {
	return map[string]string{
		"source_type": "file",
		"path":        path,
	}
}

func cloneDocument(doc Document) Document {
	doc.Vector = cloneVector(doc.Vector)
	doc.Metadata = cloneMetadata(doc.Metadata)
	doc.Provenance = cloneMetadata(doc.Provenance)
	doc.ExpiresAt = cloneTimePtr(doc.ExpiresAt)

	return doc
}

func cloneStoreForVectorizerMigration(store *Store) Store {
	if store == nil {
		return Store{}
	}

	out := *store
	out.vectorizer = nil

	if store.Agents != nil {
		out.Agents = make(map[string][]Document, len(store.Agents))
		for agent, docs := range store.Agents {
			cloned := make([]Document, 0, len(docs))
			for i := range docs {
				cloned = append(cloned, cloneDocument(docs[i]))
			}

			out.Agents[agent] = cloned
		}
	}

	return out
}

func cloneVector(vec vector.Vector) vector.Vector {
	out := make(vector.Vector, len(vec))
	copy(out, vec)

	return out
}

func vectorsEqual(left, right vector.Vector) bool {
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

var (
	// ErrMissingAgent is returned when an agent name is required but empty.
	ErrMissingAgent = errors.New("agent memory agent name is required")
	// ErrMissingID is returned when indexing a document without an ID.
	ErrMissingID = errors.New("agent memory document id is required")
	// ErrInvalidUTF8 is returned for non-UTF-8 text or file content.
	ErrInvalidUTF8 = errors.New("agent memory text is not valid UTF-8")
	// ErrDuplicateID is returned when persisted JSON has duplicate IDs for one agent.
	ErrDuplicateID = errors.New("agent memory duplicate document id")
	// ErrIncompatibleSchema is returned when persisted JSON is not on the current schema.
	ErrIncompatibleSchema = errors.New("agent memory store schema version is incompatible")
	// ErrSourceHashMismatch is returned when persisted text and source hash disagree.
	ErrSourceHashMismatch = errors.New("agent memory source hash mismatch")
	// ErrPrivacyPolicy is returned when persisted JSON still contains text that must be redacted.
	ErrPrivacyPolicy = errors.New("agent memory violates privacy policy")
	// ErrProvenanceMissing is returned when persisted agent memory lacks source provenance metadata.
	ErrProvenanceMissing = errors.New("agent memory provenance metadata is required")
	// ErrVectorMismatch is returned when a persisted hashed vector no longer matches its text.
	ErrVectorMismatch = errors.New("agent memory vector mismatch")
)
