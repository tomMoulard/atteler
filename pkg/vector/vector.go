// Package vector provides dependency-free vector retrieval primitives for
// small prose and ADR search workflows.
package vector

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"hash/fnv"
	"maps"
	"math"
	"net/http"
	"sort"
	"strings"
	"sync"
	"time"
	"unicode"
)

const defaultVectorizerDimensions = 128

// Vector is an embedding or lexical feature vector.
type Vector []float64

// Document is a text item indexed by Store.
type Document struct {
	Metadata map[string]string
	ID       string
	Text     string
	Vector   Vector
}

// Result is a cosine-ranked search result.
type Result struct {
	Document Document
	Score    float64
}

// Store is an in-memory vector document store. All exported methods are safe
// for concurrent use.
type Store struct {
	Documents  []Document
	mu         sync.RWMutex
	Dimensions int
}

// NewStore returns an empty vector store. When dimensions is zero, the store
// adopts the first added document vector's dimensions.
func NewStore(dimensions int) (*Store, error) {
	if dimensions < 0 {
		return nil, ErrInvalidDimensions
	}

	return &Store{Dimensions: dimensions}, nil
}

// Add indexes doc. Existing documents with the same ID are replaced.
func (s *Store) Add(doc Document) error {
	doc.ID = strings.TrimSpace(doc.ID)
	if doc.ID == "" {
		return ErrMissingID
	}

	if err := validateVector(doc.Vector); err != nil {
		return err
	}

	doc.Vector = cloneVector(doc.Vector)
	doc.Metadata = cloneMetadata(doc.Metadata)

	s.mu.Lock()
	defer s.mu.Unlock()

	if err := s.adoptDimensionsLocked(doc.Vector); err != nil {
		return err
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

// Search ranks documents by cosine similarity to query and returns up to limit
// results. A limit less than one returns every document with a non-zero score.
func (s *Store) Search(query Vector, limit int) ([]Result, error) {
	if err := validateVector(query); err != nil {
		return nil, err
	}

	results, err := s.searchLocked(query)
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

// searchLocked performs the read-locked portion of Search, cloning results
// so callers can sort/slice without holding the lock.
func (s *Store) searchLocked(query Vector) ([]Result, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	if err := s.checkDimensionsLocked(query); err != nil {
		return nil, err
	}

	results := make([]Result, 0, len(s.Documents))
	for _, doc := range s.Documents {
		score, err := CosineSimilarity(query, doc.Vector)
		if err != nil {
			return nil, err
		}

		if score == 0 {
			continue
		}

		results = append(results, Result{
			Document: cloneDocument(doc),
			Score:    score,
		})
	}

	return results, nil
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

// Vectorizer abstracts text-to-vector conversion so callers can swap between
// the zero-dependency TextVectorizer and a model-backed EmbeddingVectorizer.
//
// Prefer VectorizerContext when a context is available; the plain Vectorize
// method exists for callers that do not have a context handy.
type Vectorizer interface {
	Vectorize(text string) (Vector, error)
}

// VectorizerContext extends Vectorizer with context-aware vectorization.
// Implementations that make network calls should implement this interface.
type VectorizerContext interface {
	Vectorizer
	VectorizeContext(ctx context.Context, text string) (Vector, error)
}

// EmbeddingVectorizer calls an external embedding API (e.g. Ollama or OpenAI)
// to produce dense vectors from text. It is the recommended vectorizer when
// search quality matters and a model endpoint is available.
type EmbeddingVectorizer struct {
	client  *http.Client
	baseURL string
	model   string
}

const (
	defaultEmbeddingBaseURL = "http://127.0.0.1:11434"
	defaultEmbeddingModel   = "nomic-embed-text"
	embeddingTimeout        = 30 * time.Second
)

// EmbeddingOption configures an EmbeddingVectorizer.
type EmbeddingOption func(*EmbeddingVectorizer)

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

// NewEmbeddingVectorizer returns a vectorizer that calls an Ollama-compatible
// /api/embed endpoint. The zero-value options use the local Ollama default URL
// and "nomic-embed-text" as the model.
func NewEmbeddingVectorizer(opts ...EmbeddingOption) *EmbeddingVectorizer {
	v := &EmbeddingVectorizer{
		baseURL: defaultEmbeddingBaseURL,
		model:   defaultEmbeddingModel,
		client:  &http.Client{Timeout: embeddingTimeout},
	}

	for _, opt := range opts {
		opt(v)
	}

	return v
}

// Vectorize sends text to the embedding API and returns the resulting vector.
func (v *EmbeddingVectorizer) Vectorize(text string) (Vector, error) {
	return v.VectorizeContext(context.TODO(), text)
}

// VectorizeContext is Vectorize with caller-provided cancellation.
func (v *EmbeddingVectorizer) VectorizeContext(ctx context.Context, text string) (Vector, error) {
	text = strings.TrimSpace(text)
	if text == "" {
		return nil, ErrEmptyText
	}

	body, err := json.Marshal(ollamaEmbedRequest{
		Model: v.model,
		Input: text,
	})
	if err != nil {
		return nil, fmt.Errorf("embedding: marshal request: %w", err)
	}

	endpoint := strings.TrimRight(v.baseURL, "/") + "/api/embed"

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("embedding: create request: %w", err)
	}

	req.Header.Set("Content-Type", "application/json")

	resp, err := v.client.Do(req)
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
		return nil, fmt.Errorf("embedding: empty response from %s", v.model)
	}

	if len(result.Embeddings[0]) == 0 {
		return nil, fmt.Errorf("embedding: empty vector response from %s", v.model)
	}

	return Vector(result.Embeddings[0]), nil
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

	return doc
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
	// ErrEmptyText is returned when text vectorization has no tokens.
	ErrEmptyText = errors.New("vector text has no tokens")
)
