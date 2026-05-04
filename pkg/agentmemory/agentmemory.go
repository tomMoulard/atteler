// Package agentmemory provides dependency-free per-agent vector memory with
// JSON persistence.
package agentmemory

import (
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"strings"
	"unicode/utf8"

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

// Store keeps vector memories partitioned by agent name.
type Store struct {
	Agents     map[string][]Document `json:"agents"`
	Dimensions int                   `json:"dimensions"`
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

	return s.Add(agent, Document{ID: clean, Path: clean, Text: string(data)})
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

			return nil
		}
	}

	s.Agents[agent] = append(docs, doc)

	return nil
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

	store, err := vector.NewStore(s.Dimensions)
	if err != nil {
		return nil, fmt.Errorf("agent memory: create vector store: %w", err)
	}

	for _, doc := range docs {
		if addErr := store.Add(vector.Document{
			ID:       doc.ID,
			Text:     doc.Text,
			Metadata: doc.Metadata,
			Vector:   doc.Vector,
		}); addErr != nil {
			return nil, fmt.Errorf("index agent memory document %q: %w", doc.ID, addErr)
		}
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

	for agent, docs := range s.Agents {
		normalizedDocs, err := s.validateLoadedAgent(agent, docs)
		if err != nil {
			return err
		}

		s.Agents[agent] = normalizedDocs
	}

	return nil
}

func (s *Store) validateLoadedAgent(agent string, docs []Document) ([]Document, error) {
	if strings.TrimSpace(agent) == "" {
		return nil, ErrMissingAgent
	}

	seen := make(map[string]struct{}, len(docs))

	store, err := vector.NewStore(s.Dimensions)
	if err != nil {
		return nil, fmt.Errorf("agent memory: validate vector store: %w", err)
	}

	for i, doc := range docs {
		normalized, normalizeErr := s.normalizeLoadedDocument(doc, seen)
		if normalizeErr != nil {
			return nil, normalizeErr
		}

		if addErr := store.Add(vector.Document{
			ID:       normalized.ID,
			Text:     normalized.Text,
			Metadata: normalized.Metadata,
			Vector:   normalized.Vector,
		}); addErr != nil {
			return nil, fmt.Errorf("agent memory: validate document %q: %w", normalized.ID, addErr)
		}

		docs[i] = normalized
	}

	return docs, nil
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
	if !utf8.ValidString(doc.Text) {
		return Document{}, ErrInvalidUTF8
	}

	if len(doc.Vector) == 0 {
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
