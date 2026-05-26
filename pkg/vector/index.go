package vector

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"
)

const (
	// IndexVersion is the on-disk vector index schema version.
	IndexVersion = 1

	// VectorizerKindLexical names the deterministic hashed token-frequency
	// fallback. It is lexical retrieval, not semantic embedding retrieval.
	VectorizerKindLexical = "lexical"
	// VectorizerKindEmbedding names model-backed embedding retrieval.
	VectorizerKindEmbedding = "embedding"

	// LexicalFallbackModel is the model metadata label used for TextVectorizer.
	LexicalFallbackModel = "hashed-token-frequency"

	// DefaultChunkMaxRunes bounds each indexed chunk. The value is conservative
	// enough for local embedding endpoints while avoiding one-vector-per-file
	// muddiness for multi-topic documents.
	DefaultChunkMaxRunes = 1200
	// DefaultChunkOverlapRunes preserves some local context across chunk
	// boundaries without producing excessive duplicate chunks.
	DefaultChunkOverlapRunes = 120
)

// VectorizerMetadata records the vectorizer that produced an index. Persisting
// it prevents accidental reuse of hashed lexical vectors as model embeddings
// or vice versa.
type VectorizerMetadata struct {
	Kind       string `json:"kind"`
	Provider   string `json:"provider,omitempty"`
	Model      string `json:"model,omitempty"`
	BaseURL    string `json:"base_url,omitempty"`
	Dimensions int    `json:"dimensions,omitempty"`
}

// NewLexicalMetadata returns normalized metadata for the lexical fallback.
func NewLexicalMetadata(dimensions int) VectorizerMetadata {
	return VectorizerMetadata{
		Kind:       VectorizerKindLexical,
		Model:      LexicalFallbackModel,
		Dimensions: dimensions,
	}.Normalize()
}

// NewEmbeddingMetadata returns normalized metadata for an embedding vectorizer.
func NewEmbeddingMetadata(provider, model, baseURL string, dimensions int) VectorizerMetadata {
	return VectorizerMetadata{
		Kind:       VectorizerKindEmbedding,
		Provider:   provider,
		Model:      model,
		BaseURL:    baseURL,
		Dimensions: dimensions,
	}.Normalize()
}

// Normalize trims metadata fields and fills stable labels for known vectorizer
// kinds.
func (m VectorizerMetadata) Normalize() VectorizerMetadata {
	m.Kind = normalizeMetadataToken(m.Kind)
	m.Provider = normalizeProviderToken(m.Provider)
	m.Model = strings.TrimSpace(m.Model)
	m.BaseURL = strings.TrimRight(strings.TrimSpace(m.BaseURL), "/")

	if m.Kind == VectorizerKindLexical {
		m.Provider = ""
		m.BaseURL = ""

		if m.Model == "" {
			m.Model = LexicalFallbackModel
		}
	}

	if m.Kind == VectorizerKindEmbedding && m.Provider == "" {
		m.Provider = defaultEmbeddingProvider
	}

	return m
}

// Label returns a compact human-readable vectorizer description.
func (m VectorizerMetadata) Label() string {
	m = m.Normalize()
	switch m.Kind {
	case VectorizerKindLexical:
		return "lexical-fallback"
	case VectorizerKindEmbedding:
		if m.Provider != "" {
			return "embedding/" + m.Provider
		}

		return VectorizerKindEmbedding
	default:
		if m.Kind == "" {
			return "unknown"
		}

		return m.Kind
	}
}

// CompatibleWith reports whether actual persisted metadata may satisfy an
// expected search request. A zero expected dimension means "unknown until the
// first vector is produced"; all other non-derived metadata must match exactly.
func (m VectorizerMetadata) CompatibleWith(expected VectorizerMetadata) error {
	actual := m.Normalize()
	expected = expected.Normalize()

	if actual.Kind != expected.Kind {
		return fmt.Errorf("%w: kind %q != %q", ErrMetadataMismatch, actual.Kind, expected.Kind)
	}

	if actual.Provider != expected.Provider {
		return fmt.Errorf("%w: provider %q != %q", ErrMetadataMismatch, actual.Provider, expected.Provider)
	}

	if actual.Model != expected.Model {
		return fmt.Errorf("%w: model %q != %q", ErrMetadataMismatch, actual.Model, expected.Model)
	}

	if actual.BaseURL != expected.BaseURL {
		return fmt.Errorf("%w: base_url %q != %q", ErrMetadataMismatch, actual.BaseURL, expected.BaseURL)
	}

	if expected.Dimensions > 0 && actual.Dimensions != expected.Dimensions {
		return fmt.Errorf("%w: got %d, want %d", ErrDimensionMismatch, actual.Dimensions, expected.Dimensions)
	}

	return nil
}

// ChunkOptions controls deterministic source chunking.
type ChunkOptions struct {
	MaxRunes     int `json:"max_runes"`
	OverlapRunes int `json:"overlap_runes"`
}

// Normalize returns chunk options with safe defaults.
func (o ChunkOptions) Normalize() ChunkOptions {
	if o.MaxRunes <= 0 {
		o.MaxRunes = DefaultChunkMaxRunes
	}

	if o.OverlapRunes < 0 {
		o.OverlapRunes = 0
	} else if o.OverlapRunes == 0 {
		o.OverlapRunes = DefaultChunkOverlapRunes
	}

	if o.OverlapRunes >= o.MaxRunes {
		o.OverlapRunes = o.MaxRunes / 10
	}

	return o
}

// Chunk is a deterministic slice of a larger source document.
type Chunk struct {
	ID        string
	Text      string
	Index     int
	StartRune int
	EndRune   int
}

// Source is a UTF-8 text source that can be chunked and indexed.
type Source struct {
	Path string
	Text string
}

// SourceMetadata records the digest used to decide whether persisted chunks
// are still fresh for a source file.
type SourceMetadata struct {
	Path   string `json:"path"`
	Digest string `json:"digest"`
	Bytes  int    `json:"bytes"`
}

// Index is a JSON-serializable persisted vector index.
type Index struct {
	Vectorizer VectorizerMetadata `json:"vectorizer"`
	CreatedAt  time.Time          `json:"created_at"`
	Sources    []SourceMetadata   `json:"sources"`
	Documents  []Document         `json:"documents"`
	Chunk      ChunkOptions       `json:"chunk"`
	Version    int                `json:"version"`
	Dimensions int                `json:"dimensions"`
}

// ChunkText splits text into deterministic chunks with stable IDs.
func ChunkText(sourceID, text string, opts ChunkOptions) ([]Chunk, error) {
	sourceID = strings.TrimSpace(sourceID)
	if sourceID == "" {
		return nil, ErrMissingID
	}

	if !utf8.ValidString(text) {
		return nil, ErrInvalidUTF8
	}

	if strings.TrimSpace(text) == "" {
		return nil, ErrEmptyText
	}

	opts = opts.Normalize()
	runes := []rune(text)
	chunks := make([]Chunk, 0, (len(runes)/opts.MaxRunes)+1)

	for start := 0; start < len(runes); {
		end := min(start+opts.MaxRunes, len(runes))
		textStart, textEnd := trimChunkBounds(runes, start, end)

		if textStart < textEnd {
			index := len(chunks)
			chunks = append(chunks, Chunk{
				ID:        stableChunkID(sourceID, index),
				Text:      string(runes[textStart:textEnd]),
				Index:     index,
				StartRune: textStart,
				EndRune:   textEnd,
			})
		}

		if end == len(runes) {
			break
		}

		next := end - opts.OverlapRunes
		if next <= start {
			next = end
		}

		start = next
	}

	if len(chunks) == 0 {
		return nil, ErrEmptyText
	}

	return chunks, nil
}

// SourceFromFile reads a UTF-8 source from path.
func SourceFromFile(path string) (Source, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return Source{}, fmt.Errorf("read vector source %q: %w", path, err)
	}

	if !utf8.Valid(data) {
		return Source{}, fmt.Errorf("read vector source %q: %w", path, ErrInvalidUTF8)
	}

	return Source{Path: filepath.Clean(path), Text: string(data)}, nil
}

// SourceMetadataForText returns digest metadata for source text.
func SourceMetadataForText(path, text string) SourceMetadata {
	path = strings.TrimSpace(path)
	if path != "" {
		path = filepath.Clean(path)
	}

	return SourceMetadata{
		Path:   path,
		Digest: DigestText(text),
		Bytes:  len([]byte(text)),
	}
}

// SourceMetadataFromFile returns digest metadata for a UTF-8 source file.
func SourceMetadataFromFile(path string) (SourceMetadata, error) {
	source, err := SourceFromFile(path)
	if err != nil {
		return SourceMetadata{}, err
	}

	return SourceMetadataForText(source.Path, source.Text), nil
}

// DigestText returns the hex SHA-256 digest used for source invalidation.
func DigestText(text string) string {
	sum := sha256.Sum256([]byte(text))

	return hex.EncodeToString(sum[:])
}

// BuildIndex chunks, vectorizes, and records source metadata for sources.
func BuildIndex(
	ctx context.Context,
	sources []Source,
	vectorizer Vectorizer,
	metadata VectorizerMetadata,
	chunkOptions ChunkOptions,
	createdAt time.Time,
) (*Index, error) {
	if len(sources) == 0 {
		return nil, ErrNoSources
	}

	if vectorizer == nil {
		return nil, errors.New("vectorizer is required")
	}

	if createdAt.IsZero() {
		createdAt = time.Now().UTC()
	}

	idx := &Index{
		Version:    IndexVersion,
		CreatedAt:  createdAt.UTC(),
		Vectorizer: metadata.Normalize(),
		Chunk:      chunkOptions.Normalize(),
		Sources:    make([]SourceMetadata, 0, len(sources)),
	}

	for _, source := range sources {
		if err := idx.addSource(ctx, source, vectorizer); err != nil {
			return nil, err
		}
	}

	if idx.Dimensions == 0 || len(idx.Documents) == 0 {
		return nil, ErrEmptyText
	}

	idx.Vectorizer.Dimensions = idx.Dimensions

	return idx, nil
}

// Save writes idx as pretty-printed JSON.
func (idx *Index) Save(path string) error {
	if idx == nil {
		return errors.New("vector index is nil")
	}

	if err := idx.Validate(); err != nil {
		return err
	}

	data, err := json.MarshalIndent(idx, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal vector index: %w", err)
	}

	data = append(data, '\n')

	if dir := filepath.Dir(path); dir != "." {
		if err := os.MkdirAll(dir, 0o750); err != nil {
			return fmt.Errorf("create vector index dir: %w", err)
		}
	}

	if err := os.WriteFile(path, data, 0o600); err != nil {
		return fmt.Errorf("write vector index %q: %w", path, err)
	}

	return nil
}

// LoadIndex reads and validates an index saved by Save.
func LoadIndex(path string) (*Index, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read vector index %q: %w", path, err)
	}

	var idx Index
	if err := json.Unmarshal(data, &idx); err != nil {
		return nil, fmt.Errorf("decode vector index %q: %w", path, err)
	}

	if err := idx.Validate(); err != nil {
		return nil, fmt.Errorf("validate vector index %q: %w", path, err)
	}

	return &idx, nil
}

// Validate checks the internal consistency of idx.
func (idx *Index) Validate() error {
	if idx == nil {
		return errors.New("vector index is nil")
	}

	if idx.Version != IndexVersion {
		return fmt.Errorf("unsupported vector index version %d", idx.Version)
	}

	if idx.Dimensions <= 0 {
		return ErrInvalidDimensions
	}

	if idx.Vectorizer.Kind == "" {
		return fmt.Errorf("%w: missing vectorizer kind", ErrMetadataMismatch)
	}

	idx.Vectorizer = idx.Vectorizer.Normalize()
	if idx.Vectorizer.Dimensions == 0 {
		idx.Vectorizer.Dimensions = idx.Dimensions
	}

	if idx.Vectorizer.Dimensions != idx.Dimensions {
		return fmt.Errorf("%w: metadata has %d, index has %d", ErrDimensionMismatch, idx.Vectorizer.Dimensions, idx.Dimensions)
	}

	seen := make(map[string]struct{}, len(idx.Documents))
	for i := range idx.Documents {
		doc := &idx.Documents[i]
		if strings.TrimSpace(doc.ID) == "" {
			return ErrMissingID
		}

		if _, ok := seen[doc.ID]; ok {
			return fmt.Errorf("duplicate vector document id %q", doc.ID)
		}

		seen[doc.ID] = struct{}{}
		if len(doc.Vector) != idx.Dimensions {
			return fmt.Errorf("%w: document %q has %d, want %d", ErrDimensionMismatch, doc.ID, len(doc.Vector), idx.Dimensions)
		}

		if err := validateVector(doc.Vector); err != nil {
			return fmt.Errorf("validate vector document %q: %w", doc.ID, err)
		}
	}

	for _, source := range idx.Sources {
		if strings.TrimSpace(source.Path) == "" || strings.TrimSpace(source.Digest) == "" {
			return fmt.Errorf("%w: source metadata missing path or digest", ErrSourceStale)
		}
	}

	return nil
}

// ValidateFor checks whether idx can be reused for expected metadata and the
// currently observed source digests.
func (idx *Index) ValidateFor(expected VectorizerMetadata, currentSources []SourceMetadata, expectedChunk ...ChunkOptions) error {
	if err := idx.Validate(); err != nil {
		return err
	}

	if err := idx.Vectorizer.CompatibleWith(expected); err != nil {
		return err
	}

	if len(expectedChunk) > 0 {
		actual := idx.Chunk.Normalize()
		expected := expectedChunk[0].Normalize()

		if actual != expected {
			return fmt.Errorf("%w: chunk options %+v != %+v", ErrMetadataMismatch, actual, expected)
		}
	}

	if len(currentSources) == 0 {
		return nil
	}

	indexed := make(map[string]SourceMetadata, len(idx.Sources))
	for _, source := range idx.Sources {
		indexed[filepath.Clean(source.Path)] = source
	}

	for _, current := range currentSources {
		path := filepath.Clean(current.Path)

		previous, ok := indexed[path]
		if !ok {
			return fmt.Errorf("%w: %s was not indexed", ErrSourceStale, path)
		}

		if previous.Digest != current.Digest {
			return fmt.Errorf("%w: %s digest changed", ErrSourceStale, path)
		}
	}

	return nil
}

// Store returns an in-memory search store for the persisted documents.
func (idx *Index) Store() (*Store, error) {
	if err := idx.Validate(); err != nil {
		return nil, err
	}

	store, err := NewStore(idx.Dimensions)
	if err != nil {
		return nil, err
	}

	for i := range idx.Documents {
		if err := store.Add(idx.Documents[i]); err != nil {
			return nil, fmt.Errorf("load vector document %q: %w", idx.Documents[i].ID, err)
		}
	}

	return store, nil
}

func (idx *Index) addSource(ctx context.Context, source Source, vectorizer Vectorizer) error {
	source.Path = strings.TrimSpace(source.Path)
	if source.Path == "" {
		return ErrMissingID
	}

	source.Path = filepath.Clean(source.Path)

	if !utf8.ValidString(source.Text) {
		return fmt.Errorf("vector source %s: %w", source.Path, ErrInvalidUTF8)
	}

	sourceMetadata := SourceMetadataForText(source.Path, source.Text)

	chunks, err := ChunkText(source.Path, source.Text, idx.Chunk)
	if err != nil {
		return fmt.Errorf("chunk vector source %s: %w", source.Path, err)
	}

	idx.Sources = append(idx.Sources, sourceMetadata)

	for _, chunk := range chunks {
		vec, err := vectorizeWithContext(ctx, vectorizer, chunk.Text)
		if err != nil {
			return fmt.Errorf("vectorize chunk %s: %w", chunk.ID, err)
		}

		if err := validateVector(vec); err != nil {
			return fmt.Errorf("validate chunk %s vector: %w", chunk.ID, err)
		}

		if idx.Vectorizer.Dimensions > 0 && len(vec) != idx.Vectorizer.Dimensions {
			return fmt.Errorf("%w: chunk %s has %d, metadata wants %d", ErrDimensionMismatch, chunk.ID, len(vec), idx.Vectorizer.Dimensions)
		}

		if idx.Dimensions == 0 {
			idx.Dimensions = len(vec)
		}

		if len(vec) != idx.Dimensions {
			return fmt.Errorf("%w: chunk %s has %d, want %d", ErrDimensionMismatch, chunk.ID, len(vec), idx.Dimensions)
		}

		idx.Documents = append(idx.Documents, Document{
			ID:     chunk.ID,
			Text:   chunk.Text,
			Vector: cloneVector(vec),
			Metadata: map[string]string{
				"path":             source.Path,
				"chunk_index":      strconv.Itoa(chunk.Index),
				"chunk_start_rune": strconv.Itoa(chunk.StartRune),
				"chunk_end_rune":   strconv.Itoa(chunk.EndRune),
				"source_digest":    sourceMetadata.Digest,
			},
		})
	}

	return nil
}

func vectorizeWithContext(ctx context.Context, vectorizer Vectorizer, text string) (Vector, error) {
	if contextual, ok := vectorizer.(VectorizerContext); ok {
		vec, err := contextual.VectorizeContext(ctx, text)
		if err != nil {
			return nil, fmt.Errorf("vectorize with context: %w", err)
		}

		return vec, nil
	}

	vec, err := vectorizer.Vectorize(text)
	if err != nil {
		return nil, fmt.Errorf("vectorize: %w", err)
	}

	return vec, nil
}

func stableChunkID(sourceID string, index int) string {
	return sourceID + "#chunk=" + fmt.Sprintf("%04d", index)
}

func trimChunkBounds(runes []rune, start, end int) (trimmedStart, trimmedEnd int) {
	for start < end && isChunkTrimRune(runes[start]) {
		start++
	}

	for end > start && isChunkTrimRune(runes[end-1]) {
		end--
	}

	return start, end
}

func isChunkTrimRune(r rune) bool {
	return r == ' ' || r == '\n' || r == '\t' || r == '\r'
}

func normalizeMetadataToken(value string) string {
	value = strings.ToLower(strings.TrimSpace(value))
	value = strings.ReplaceAll(value, "_", "-")

	switch value {
	case "", "lexical-fallback", "fallback", "text", "hashed", "hashed-token-frequency":
		return VectorizerKindLexical
	case "embed", "embeddings":
		return VectorizerKindEmbedding
	default:
		return value
	}
}

func normalizeProviderToken(value string) string {
	value = strings.ToLower(strings.TrimSpace(value))
	value = strings.ReplaceAll(value, "_", "-")

	switch value {
	case "ollama-compatible":
		return defaultEmbeddingProvider
	default:
		return value
	}
}
