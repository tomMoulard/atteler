// Package agentmemory provides dependency-free per-agent vector memory with
// JSON persistence.
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

	"github.com/tommoulard/atteler/pkg/retrieval"
	"github.com/tommoulard/atteler/pkg/vector"
)

// Document is a vectorized text item stored for one agent.
type Document struct {
	Metadata map[string]string `json:"metadata,omitempty"`
	ID       string            `json:"id"`
	Path     string            `json:"path,omitempty"`
	Text     string            `json:"text"`
	Vector   vector.Vector     `json:"vector"`
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

// Store keeps vector memories partitioned by agent name.
type Store struct {
	Agents     map[string][]Document `json:"agents"`
	indexes    map[string]*vector.Store
	Dimensions int `json:"dimensions"`
}

// NewStore returns an empty per-agent memory store. A zero dimension value uses
// pkg/vector's default text vectorizer dimensions.
func NewStore(dimensions int) (*Store, error) {
	vectorizer, err := vector.NewTextVectorizer(dimensions)
	if err != nil {
		return nil, fmt.Errorf("agent memory: create vectorizer: %w", err)
	}

	return &Store{
		Agents:     make(map[string][]Document),
		Dimensions: vectorizer.Dimensions,
		indexes:    make(map[string]*vector.Store),
	}, nil
}

// AddText vectorizes and stores text for agent under id. Existing documents for
// the same agent and id are replaced.
func (s *Store) AddText(agent, id, text string) error {
	return s.Add(agent, Document{ID: id, Text: text})
}

// AddFile reads and stores a UTF-8 text file for agent. The cleaned filepath is
// used as the document ID and Path.
func (s *Store) AddFile(agent, path string) error {
	data, err := os.ReadFile(path)
	if err != nil {
		return fmt.Errorf("read agent memory file %q: %w", path, err)
	}

	if !utf8.Valid(data) {
		return fmt.Errorf("read agent memory file %q: %w", path, ErrInvalidUTF8)
	}

	clean := filepath.Clean(path)

	metadata := map[string]string{
		"path": clean,
	}
	if info, statErr := os.Stat(path); statErr == nil && !info.ModTime().IsZero() {
		metadata[retrieval.MetadataSourceUpdatedAt] = info.ModTime().UTC().Format(time.RFC3339Nano)
	}

	return s.Add(agent, Document{
		ID:       clean,
		Path:     clean,
		Text:     string(data),
		Metadata: metadata,
	})
}

// Add vectorizes and stores doc for agent. Existing documents for the same
// agent and document ID are replaced.
func (s *Store) Add(agent string, doc Document) error {
	agent = strings.TrimSpace(agent)
	if agent == "" {
		return ErrMissingAgent
	}

	doc.ID = strings.TrimSpace(doc.ID)
	if doc.ID == "" {
		return ErrMissingID
	}

	if !utf8.ValidString(doc.Text) {
		return ErrInvalidUTF8
	}

	if doc.Metadata != nil && len(doc.Metadata) == 0 {
		doc.Metadata = nil
	}

	doc, _ = s.prepareDocument(agent, doc)

	vec, err := s.vectorize(doc.Text)
	if err != nil {
		return err
	}

	doc.Vector = cloneVector(vec)
	doc.Metadata = cloneMetadata(doc.Metadata)

	if s.Agents == nil {
		s.Agents = make(map[string][]Document)
	}

	docs := s.Agents[agent]
	for i, existing := range docs {
		if existing.ID == doc.ID {
			docs[i] = doc
			s.Agents[agent] = docs

			return s.indexDocument(agent, doc)
		}
	}

	s.Agents[agent] = append(docs, doc)

	return s.indexDocument(agent, doc)
}

// Search vectorizes query and returns results from only agent's documents. A
// limit less than one returns every non-zero match.
func (s *Store) Search(agent, query string, limit int) ([]Result, error) {
	agent = strings.TrimSpace(agent)
	if agent == "" {
		return nil, ErrMissingAgent
	}

	if !utf8.ValidString(query) {
		return nil, ErrInvalidUTF8
	}

	queryVector, err := s.vectorize(query)
	if err != nil {
		return nil, err
	}

	docs := s.Agents[agent]
	if len(docs) == 0 {
		return nil, nil
	}

	store, err := s.indexForAgent(agent)
	if err != nil {
		return nil, err
	}

	vectorResults, err := store.Search(queryVector, limit)
	if err != nil {
		return nil, fmt.Errorf("agent memory: search vector store: %w", err)
	}

	results := make([]Result, 0, len(vectorResults))
	for _, result := range vectorResults {
		results = append(results, Result{
			Document: documentFromVector(result.Document, docs),
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
	if err := ctx.Err(); err != nil {
		return nil, fmt.Errorf("agent memory retrieval: %w", err)
	}

	if s.Store == nil {
		return nil, nil
	}

	results, err := s.Store.Search(s.Agent, query.Text, query.Limit)
	if err != nil {
		return nil, err
	}

	out := make([]retrieval.Result, 0, len(results))
	for _, result := range results {
		out = append(out, agentRetrievalResult(s.Agent, result, query))
	}

	return out, nil
}

// Delete removes one document from an agent namespace.
func (s *Store) Delete(agent, id string) bool {
	agent = strings.TrimSpace(agent)

	id = strings.TrimSpace(id)
	if agent == "" || id == "" {
		return false
	}

	docs := s.Agents[agent]
	for i, doc := range docs {
		if doc.ID != id {
			continue
		}

		s.Agents[agent] = append(docs[:i], docs[i+1:]...)
		if len(s.Agents[agent]) == 0 {
			delete(s.Agents, agent)
		}

		if s.indexes != nil && s.indexes[agent] != nil {
			s.indexes[agent].Delete(id)
		}

		return true
	}

	return false
}

// Save writes the store as pretty-printed JSON.
func (s *Store) Save(path string) error {
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

	return nil
}

// Load reads a JSON store saved by Save.
func Load(path string) (*Store, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read agent memory store %q: %w", path, err)
	}

	var store Store
	if err := json.Unmarshal(data, &store); err != nil {
		return nil, fmt.Errorf("decode agent memory store %q: %w", path, err)
	}

	if err := store.validateLoaded(); err != nil {
		return nil, fmt.Errorf("validate agent memory store %q: %w", path, err)
	}

	return &store, nil
}

// Documents returns a defensive copy of agent's documents.
func (s *Store) Documents(agent string) []Document {
	docs := s.Agents[strings.TrimSpace(agent)]

	out := make([]Document, 0, len(docs))
	for _, doc := range docs {
		out = append(out, cloneDocument(doc))
	}

	return out
}

func (s *Store) prepareDocument(agent string, doc Document) (Document, bool) {
	source := retrieval.Source{Type: retrieval.SourceAgentMemory, Name: agent, URI: firstNonEmpty(doc.Path, doc.Metadata["path"])}
	originalText := doc.Text
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

	if !retrieval.IsDefaultSafety(safety) {
		doc.Metadata = retrieval.MergeSafetyMetadata(doc.Metadata, safety)
	}

	return doc, originalText != doc.Text
}

func (s *Store) indexForAgent(agent string) (*vector.Store, error) {
	if s.indexes == nil {
		s.indexes = make(map[string]*vector.Store)
	}

	if store := s.indexes[agent]; store != nil {
		return store, nil
	}

	if err := s.rebuildAgentIndex(agent); err != nil {
		return nil, err
	}

	return s.indexes[agent], nil
}

func (s *Store) indexDocument(agent string, doc Document) error {
	store, err := s.indexForAgent(agent)
	if err != nil {
		return err
	}

	if err := store.Add(vector.Document{
		ID:       doc.ID,
		Text:     doc.Text,
		Metadata: doc.Metadata,
		Vector:   doc.Vector,
	}); err != nil {
		return fmt.Errorf("index agent memory document %q: %w", doc.ID, err)
	}

	return nil
}

func (s *Store) rebuildAgentIndex(agent string) error {
	if s.indexes == nil {
		s.indexes = make(map[string]*vector.Store)
	}

	store, err := vector.NewStore(s.Dimensions)
	if err != nil {
		return fmt.Errorf("agent memory: create vector store: %w", err)
	}

	for _, doc := range s.Agents[agent] {
		if addErr := store.Add(vector.Document{
			ID:       doc.ID,
			Text:     doc.Text,
			Metadata: doc.Metadata,
			Vector:   doc.Vector,
		}); addErr != nil {
			return fmt.Errorf("index agent memory document %q: %w", doc.ID, addErr)
		}
	}

	s.indexes[agent] = store

	return nil
}

func (s *Store) vectorize(text string) (vector.Vector, error) {
	if s.Dimensions <= 0 {
		vectorizer, err := vector.NewTextVectorizer(0)
		if err != nil {
			return nil, fmt.Errorf("agent memory: create default vectorizer: %w", err)
		}

		s.Dimensions = vectorizer.Dimensions
	}

	vectorizer, err := vector.NewTextVectorizer(s.Dimensions)
	if err != nil {
		return nil, fmt.Errorf("agent memory: create vectorizer: %w", err)
	}

	vec, err := vectorizer.Vectorize(text)
	if err != nil {
		return nil, fmt.Errorf("agent memory: vectorize text: %w", err)
	}

	return vec, nil
}

func (s *Store) validateLoaded() error {
	if s.Dimensions < 0 {
		return vector.ErrInvalidDimensions
	}

	if s.Dimensions == 0 {
		fresh, err := NewStore(0)
		if err != nil {
			return err
		}

		s.Dimensions = fresh.Dimensions
	}

	if s.Agents == nil {
		s.Agents = make(map[string][]Document)
	}

	s.indexes = make(map[string]*vector.Store, len(s.Agents))

	for agent, docs := range s.Agents {
		normalizedDocs, err := s.validateLoadedAgent(agent, docs)
		if err != nil {
			return err
		}

		s.Agents[agent] = normalizedDocs
		if err := s.rebuildAgentIndex(agent); err != nil {
			return err
		}
	}

	return nil
}

func (s *Store) validateLoadedAgent(agent string, docs []Document) ([]Document, error) {
	if strings.TrimSpace(agent) == "" {
		return nil, ErrMissingAgent
	}

	seen := make(map[string]struct{}, len(docs))

	for i, doc := range docs {
		normalized, normalizeErr := s.normalizeLoadedDocument(agent, doc, seen)
		if normalizeErr != nil {
			return nil, normalizeErr
		}

		docs[i] = normalized
	}

	return docs, nil
}

func (s *Store) normalizeLoadedDocument(agent string, doc Document, seen map[string]struct{}) (Document, error) {
	doc.ID = strings.TrimSpace(doc.ID)
	if doc.ID == "" {
		return Document{}, ErrMissingID
	}

	if _, ok := seen[doc.ID]; ok {
		return Document{}, fmt.Errorf("%w: %s", ErrDuplicateID, doc.ID)
	}

	seen[doc.ID] = struct{}{}
	if !utf8.ValidString(doc.Text) {
		return Document{}, ErrInvalidUTF8
	}

	var redacted bool

	doc, redacted = s.prepareDocument(agent, doc)

	if len(doc.Vector) == 0 || redacted {
		vec, err := s.vectorize(doc.Text)
		if err != nil {
			return Document{}, err
		}

		doc.Vector = vec
	}

	doc.Vector = cloneVector(doc.Vector)
	doc.Metadata = cloneMetadata(doc.Metadata)

	return doc, nil
}

func documentFromVector(doc vector.Document, originals []Document) Document {
	for _, original := range originals {
		if original.ID == doc.ID {
			return cloneDocument(original)
		}
	}

	return Document{
		ID:       doc.ID,
		Text:     doc.Text,
		Metadata: cloneMetadata(doc.Metadata),
		Vector:   cloneVector(doc.Vector),
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
		Name: "agent-memory-hashed-vector-cosine",
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

func cloneDocument(doc Document) Document {
	doc.Vector = cloneVector(doc.Vector)
	doc.Metadata = cloneMetadata(doc.Metadata)

	return doc
}

func cloneVector(vec vector.Vector) vector.Vector {
	out := make(vector.Vector, len(vec))
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

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			return value
		}
	}

	return ""
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
)
