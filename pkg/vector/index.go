package vector

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/tommoulard/atteler/internal/atomicfile"
	"github.com/tommoulard/atteler/pkg/privacy"
	"github.com/tommoulard/atteler/pkg/retrieval"
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

	// SourceKindFile identifies file/workspace sources.
	SourceKindFile = "file"
	// SourceKindSession identifies persisted session transcript/artifact
	// sources.
	SourceKindSession = "session"
	// SourceKindGitHistory identifies git history sources.
	SourceKindGitHistory = "git_history"
	// SourceKindADR identifies architecture decision record sources.
	SourceKindADR = "adr"

	provenanceSourceTypeKey         = "source_type"
	vectorizerMetadataRedactedValue = "[REDACTED]"
)

var vectorizerMetadataUserinfoPattern = regexp.MustCompile(`(^|//)([^/?#\s:@]+):([^/?#\s@]+)@`)

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
	m.Provider = normalizeProviderToken(privacy.RedactIdentifier(m.Provider))
	m.Model = privacy.RedactIdentifier(strings.TrimSpace(m.Model))
	m.BaseURL = redactVectorizerMetadataURL(m.BaseURL)

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

func redactVectorizerMetadataURL(raw string) string {
	raw = strings.TrimRight(strings.TrimSpace(raw), "/")
	if raw == "" {
		return ""
	}

	parsed, err := url.Parse(raw)
	if err != nil {
		return privacy.RedactIdentifier(redactVectorizerMetadataUserinfo(raw))
	}

	if parsed.User != nil {
		if _, hasPassword := parsed.User.Password(); hasPassword {
			parsed.User = url.UserPassword(vectorizerMetadataRedactedValue, vectorizerMetadataRedactedValue)
		} else {
			parsed.User = url.User(vectorizerMetadataRedactedValue)
		}
	} else if redacted := redactVectorizerMetadataUserinfo(raw); redacted != raw {
		reparsed, parseErr := url.Parse(redacted)
		if parseErr != nil {
			return privacy.RedactIdentifier(redacted)
		}

		parsed = reparsed
	}

	if query, redacted := redactVectorizerMetadataQuery(parsed.Query()); redacted {
		parsed.RawQuery = query
	}

	return privacy.RedactIdentifier(parsed.String())
}

func redactVectorizerMetadataUserinfo(raw string) string {
	return vectorizerMetadataUserinfoPattern.ReplaceAllString(
		raw,
		"$1"+vectorizerMetadataRedactedValue+":"+vectorizerMetadataRedactedValue+"@",
	)
}

func redactVectorizerMetadataQuery(query url.Values) (string, bool) {
	redacted := false

	for key := range query {
		if privacy.IsSensitiveKey(key) {
			redacted = true

			query.Set(key, vectorizerMetadataRedactedValue)
		}
	}

	if !redacted {
		return "", false
	}

	return query.Encode(), true
}

func validateVectorizerMetadataPrivacy(metadata VectorizerMetadata) error {
	if vectorizerMetadataSecretValue(metadata.Provider) ||
		vectorizerMetadataSecretValue(metadata.Model) ||
		vectorizerMetadataURLSecretValue(metadata.BaseURL) {
		return ErrPrivacyPolicy
	}

	return nil
}

func vectorizerMetadataSecretValue(value string) bool {
	value = strings.TrimSpace(value)

	return value != privacy.RedactIdentifier(value)
}

func vectorizerMetadataURLSecretValue(raw string) bool {
	raw = strings.TrimRight(strings.TrimSpace(raw), "/")
	if raw == "" {
		return false
	}

	if vectorizerMetadataUserinfoSecretValue(raw) {
		return true
	}

	if raw != privacy.RedactIdentifier(raw) {
		return true
	}

	parsed, err := url.Parse(raw)
	if err != nil {
		return false
	}

	if parsed.User != nil {
		username := parsed.User.Username()
		if !vectorizerMetadataValueRedacted(username) {
			return true
		}

		password, hasPassword := parsed.User.Password()
		if hasPassword && !vectorizerMetadataValueRedacted(password) {
			return true
		}
	}

	for key, values := range parsed.Query() {
		if !privacy.IsSensitiveKey(key) {
			continue
		}

		for _, value := range values {
			if !vectorizerMetadataValueRedacted(value) {
				return true
			}
		}
	}

	return false
}

func vectorizerMetadataUserinfoSecretValue(raw string) bool {
	for _, match := range vectorizerMetadataUserinfoPattern.FindAllStringSubmatch(raw, -1) {
		if len(match) < 4 {
			continue
		}

		if !vectorizerMetadataValueRedacted(match[2]) || !vectorizerMetadataValueRedacted(match[3]) {
			return true
		}
	}

	return false
}

func vectorizerMetadataValueRedacted(value string) bool {
	value = strings.TrimSpace(value)
	if value == "" || value == vectorizerMetadataRedactedValue {
		return true
	}

	unescaped, err := url.QueryUnescape(value)
	if err != nil {
		return false
	}

	return unescaped == vectorizerMetadataRedactedValue
}

type vectorizerMetadataProvider interface {
	Metadata() VectorizerMetadata
}

type vectorizerDimensionMetadataProvider interface {
	Metadata(int) VectorizerMetadata
}

func vectorizerMetadata(vectorizer Vectorizer) VectorizerMetadata {
	if provider, ok := vectorizer.(vectorizerDimensionMetadataProvider); ok {
		return provider.Metadata(0)
	}

	if provider, ok := vectorizer.(vectorizerMetadataProvider); ok {
		return provider.Metadata()
	}

	return VectorizerMetadata{}
}

func normalizeVectorizerMetadataForIndex(vectorizer Vectorizer, metadata VectorizerMetadata) VectorizerMetadata {
	inferred := vectorizerMetadata(vectorizer)
	if strings.TrimSpace(inferred.Kind) != "" {
		inferred = inferred.Normalize()
	}

	if strings.TrimSpace(metadata.Kind) == "" {
		metadata.Kind = inferred.Kind
	}

	if strings.TrimSpace(metadata.Kind) == "" {
		return VectorizerMetadata{}
	}

	if normalizeMetadataToken(metadata.Kind) != inferred.Kind {
		return metadata.Normalize()
	}

	if strings.TrimSpace(metadata.Provider) == "" {
		metadata.Provider = inferred.Provider
	}

	if strings.TrimSpace(metadata.Model) == "" {
		metadata.Model = inferred.Model
	}

	if strings.TrimSpace(metadata.BaseURL) == "" {
		metadata.BaseURL = inferred.BaseURL
	}

	if metadata.Dimensions == 0 {
		metadata.Dimensions = inferred.Dimensions
	}

	return metadata.Normalize()
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

// Source is a UTF-8 text source that can be chunked and indexed. Kind and
// metadata let one persisted Index represent files, sessions, git history, ADRs
// or other local corpora while keeping the same digest-based invalidation
// lifecycle.
type Source struct {
	Metadata   map[string]string
	Provenance map[string]string
	Kind       string
	Path       string
	Text       string
}

// SourceMetadata records the digest used to decide whether persisted chunks
// are still fresh for a source.
type SourceMetadata struct {
	Path   string `json:"path"`
	Digest string `json:"digest"`
	Kind   string `json:"kind,omitempty"`
	Bytes  int    `json:"bytes"`
}

// Index is a JSON-serializable persisted vector index.
type Index struct {
	Vectorizer VectorizerMetadata `json:"vectorizer"`
	CreatedAt  time.Time          `json:"created_at"`
	UpdatedAt  time.Time          `json:"updated_at,omitzero"`
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

	info, err := os.Stat(path)
	if err != nil {
		return Source{}, fmt.Errorf("stat vector source %q: %w", path, err)
	}

	metadata := make(map[string]string, 1)
	if updatedAt := info.ModTime().UTC(); !updatedAt.IsZero() {
		metadata[retrieval.MetadataSourceUpdatedAt] = updatedAt.Format(time.RFC3339Nano)
	}

	return Source{Path: filepath.Clean(path), Text: string(data), Metadata: metadata}, nil
}

// SourceMetadataForText returns digest metadata for source text. Digests and
// byte counts are derived from redacted text so persisted indexes do not retain
// fingerprints of raw credential values.
func SourceMetadataForText(path, text string) SourceMetadata {
	return SourceMetadataForTextWithKind(path, text, SourceKindFile)
}

// SourceMetadataForTextWithKind returns digest metadata for a typed source.
// Digests and byte counts are derived from redacted text so persisted indexes
// do not retain fingerprints of raw credential values.
func SourceMetadataForTextWithKind(path, text, kind string) SourceMetadata {
	path = redactSourcePath(path)
	text = privacy.RedactText(text)

	return SourceMetadata{
		Path:   path,
		Digest: DigestText(text),
		Kind:   normalizeSourceKind(kind),
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
	if ctx == nil {
		return nil, ErrContextRequired
	}

	if len(sources) == 0 {
		return nil, ErrNoSources
	}

	if vectorizer == nil {
		return nil, errors.New("vectorizer is required")
	}

	if err := validateUniqueSourceIndexSources(sources); err != nil {
		return nil, err
	}

	if createdAt.IsZero() {
		createdAt = time.Now().UTC()
	}

	createdAt = createdAt.UTC()

	metadata = normalizeVectorizerMetadataForIndex(vectorizer, metadata)
	if metadata.Kind == "" {
		return nil, fmt.Errorf("%w: vectorizer metadata is required", ErrMetadataMismatch)
	}

	idx := &Index{
		Version:    IndexVersion,
		CreatedAt:  createdAt,
		UpdatedAt:  createdAt,
		Vectorizer: metadata,
		Chunk:      chunkOptions.Normalize(),
		Sources:    make([]SourceMetadata, 0, len(sources)),
	}

	for _, source := range sources {
		if err := ctx.Err(); err != nil {
			return nil, fmt.Errorf("build vector index: %w", err)
		}

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

	if err := idx.validate(indexValidationOptions{RequireDocumentPersistenceSafety: true}); err != nil {
		return err
	}

	data, err := json.MarshalIndent(idx, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal vector index: %w", err)
	}

	data = append(data, '\n')

	if err := atomicfile.WriteFile(path, data, 0o600, ".vector-index-*.tmp"); err != nil {
		return fmt.Errorf("write vector index %q atomically: %w", path, err)
	}

	return nil
}

// LoadIndex reads and validates an index saved by Save.
func LoadIndex(path string) (*Index, error) {
	return loadIndex(path, indexValidationOptions{})
}

func loadIndex(path string, validation indexValidationOptions) (*Index, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read vector index %q: %w", path, err)
	}

	var idx Index
	if err := json.Unmarshal(data, &idx); err != nil {
		return nil, fmt.Errorf("%w: decode vector index %q: %w", ErrIndexCorrupt, path, err)
	}

	if err := idx.validate(validation); err != nil {
		return nil, fmt.Errorf("validate vector index %q: %w", path, err)
	}

	return &idx, nil
}

func redactSourcePath(path string) string {
	path = strings.TrimSpace(path)
	if path == "" {
		return ""
	}

	return privacy.RedactIdentifier(filepath.Clean(path))
}

// Validate checks the internal consistency of idx.
func (idx *Index) Validate() error {
	return idx.validate(indexValidationOptions{})
}

type indexValidationOptions struct {
	AllowStaleTextHashVector         bool
	RequireDocumentPersistenceSafety bool
}

func (idx *Index) validate(opts indexValidationOptions) error {
	if idx == nil {
		return errors.New("vector index is nil")
	}

	if idx.Version != IndexVersion {
		return fmt.Errorf("unsupported vector index version %d", idx.Version)
	}

	idx.normalizeTimestamps()

	if idx.Dimensions <= 0 {
		return ErrInvalidDimensions
	}

	if err := idx.validateVectorizerMetadata(); err != nil {
		return err
	}

	seen := make(map[string]struct{}, len(idx.Documents))
	for i := range idx.Documents {
		if err := validateIndexDocument(&idx.Documents[i], idx.Vectorizer, idx.Dimensions, opts, seen); err != nil {
			return err
		}
	}

	seenSources := make(map[string]struct{}, len(idx.Sources))
	for _, source := range idx.Sources {
		if err := validateIndexSourceMetadata(source, seenSources); err != nil {
			return err
		}
	}

	return nil
}

func validateIndexSourceMetadata(source SourceMetadata, seen map[string]struct{}) error {
	sourcePath := strings.TrimSpace(source.Path)
	sourceDigest := strings.TrimSpace(source.Digest)
	sourceKind := normalizeSourceKind(source.Kind)

	if sourcePath == "" || sourceDigest == "" {
		return fmt.Errorf("%w: source metadata missing path or digest", ErrSourceStale)
	}

	cleanPath := filepath.Clean(sourcePath)
	if sourcePath != cleanPath {
		return fmt.Errorf("%w: source metadata path %q is not clean", ErrSourceStale, cleanPath)
	}

	redactedSourcePath := redactSourcePath(cleanPath)
	if cleanPath != redactedSourcePath {
		return fmt.Errorf("%w: source metadata path %q", ErrPrivacyPolicy, redactedSourcePath)
	}

	if !validSourceDigest(sourceDigest) || source.Bytes < 0 {
		return fmt.Errorf("%w: source metadata %q has invalid digest or byte count", ErrSourceStale, cleanPath)
	}

	if sourceKind == "" {
		return fmt.Errorf("%w: source metadata %q has invalid source kind", ErrSourceStale, cleanPath)
	}

	if _, ok := seen[cleanPath]; ok {
		return fmt.Errorf("%w: duplicate source metadata path %q", ErrSourceStale, cleanPath)
	}

	seen[cleanPath] = struct{}{}

	return nil
}

func validSourceDigest(digest string) bool {
	if len(digest) != sha256.Size*2 {
		return false
	}

	_, err := hex.DecodeString(digest)

	return err == nil
}

func normalizeSourceKind(kind string) string {
	kind = strings.ToLower(strings.TrimSpace(privacy.RedactIdentifier(kind)))
	kind = strings.ReplaceAll(kind, "-", "_")
	kind = strings.ReplaceAll(kind, " ", "_")

	if kind == "" {
		return SourceKindFile
	}

	return kind
}

func validateIndexDocument(
	doc *Document,
	metadata VectorizerMetadata,
	dimensions int,
	opts indexValidationOptions,
	seen map[string]struct{},
) error {
	if strings.TrimSpace(doc.ID) == "" {
		return ErrMissingID
	}

	if _, ok := seen[doc.ID]; ok {
		return fmt.Errorf("%w: %q", ErrDuplicateID, doc.ID)
	}

	seen[doc.ID] = struct{}{}
	if len(doc.Vector) != dimensions {
		return fmt.Errorf("%w: document %q has %d, want %d", ErrDimensionMismatch, doc.ID, len(doc.Vector), dimensions)
	}

	if err := validateVector(doc.Vector); err != nil {
		return fmt.Errorf("validate vector document %q: %w", doc.ID, err)
	}

	if err := validateIndexDocumentVectorizer(*doc, dimensions, metadata); err != nil {
		return fmt.Errorf("validate vector document %q vectorizer: %w", doc.ID, err)
	}

	if opts.RequireDocumentPersistenceSafety {
		if err := checkDocumentPrivacy(*doc); err != nil {
			return fmt.Errorf("validate vector document %q privacy: %w", doc.ID, err)
		}

		if err := checkDocumentSourceHash(*doc); err != nil {
			return fmt.Errorf("validate vector document %q source hash: %w", doc.ID, err)
		}

		if err := checkDocumentProvenance(*doc); err != nil {
			return fmt.Errorf("validate vector document %q provenance: %w", doc.ID, err)
		}
	}

	if opts.AllowStaleTextHashVector {
		return nil
	}

	if err := checkDocumentTextHashVector(*doc); err != nil {
		return fmt.Errorf("validate vector document %q vector: %w", doc.ID, err)
	}

	return nil
}

func validateIndexDocumentVectorizer(doc Document, dimensions int, metadata VectorizerMetadata) error {
	if doc.Vectorizer.IsZero() {
		return nil
	}

	if err := validatePersistedVectorizerSpecPrivacy(doc.Vectorizer); err != nil {
		return err
	}

	spec := normalizeVectorizerSpec(doc.Vectorizer)
	if err := validateVectorizerIdentity(spec); err != nil {
		return err
	}

	if spec.Dimensions != 0 && spec.Dimensions != dimensions {
		return fmt.Errorf("%w: vectorizer dimensions got %d, want %d", ErrDimensionMismatch, spec.Dimensions, dimensions)
	}

	if err := validateDocumentVectorizerMatchesIndex(spec, metadata); err != nil {
		return err
	}

	return nil
}

func validateDocumentVectorizerMatchesIndex(spec VectorizerSpec, metadata VectorizerMetadata) error {
	metadata = metadata.Normalize()

	switch metadata.Kind {
	case "":
		return nil
	case VectorizerKindLexical:
		if spec.ID != TextHashVectorizerID {
			return ErrVectorizerMismatch
		}
	case VectorizerKindEmbedding:
		if metadata.Provider != "" && spec.Provider != "" && spec.Provider != metadata.Provider {
			return ErrVectorizerMismatch
		}

		if metadata.Model != "" && spec.Model != "" && spec.Model != metadata.Model {
			return ErrVectorizerMismatch
		}

		if metadata.BaseURL != "" && spec.BaseURL != "" && spec.BaseURL != metadata.BaseURL {
			return ErrVectorizerMismatch
		}
	}

	return nil
}

func (idx *Index) validateVectorizerMetadata() error {
	if idx.Vectorizer.Kind == "" {
		return fmt.Errorf("%w: missing vectorizer kind", ErrMetadataMismatch)
	}

	if err := validateVectorizerMetadataPrivacy(idx.Vectorizer); err != nil {
		return err
	}

	idx.Vectorizer = idx.Vectorizer.Normalize()
	if idx.Vectorizer.Dimensions == 0 {
		idx.Vectorizer.Dimensions = idx.Dimensions
	}

	if idx.Vectorizer.Dimensions != idx.Dimensions {
		return fmt.Errorf("%w: metadata has %d, index has %d", ErrDimensionMismatch, idx.Vectorizer.Dimensions, idx.Dimensions)
	}

	return nil
}

func (idx *Index) normalizeTimestamps() {
	if idx.CreatedAt.IsZero() && !idx.UpdatedAt.IsZero() {
		idx.CreatedAt = idx.UpdatedAt
	}

	if idx.UpdatedAt.IsZero() && !idx.CreatedAt.IsZero() {
		idx.UpdatedAt = idx.CreatedAt
	}

	if !idx.CreatedAt.IsZero() {
		idx.CreatedAt = idx.CreatedAt.UTC()
	}

	if !idx.UpdatedAt.IsZero() {
		idx.UpdatedAt = idx.UpdatedAt.UTC()
	}
}

// ValidateFor checks whether idx can be reused for expected metadata and the
// currently observed source digests.
func (idx *Index) ValidateFor(expected VectorizerMetadata, currentSources []SourceMetadata, expectedChunk ...ChunkOptions) error {
	return idx.validateFor(expected, currentSources, indexValidationOptions{}, expectedChunk...)
}

func (idx *Index) validateFor(
	expected VectorizerMetadata,
	currentSources []SourceMetadata,
	validation indexValidationOptions,
	expectedChunk ...ChunkOptions,
) error {
	if err := idx.validate(validation); err != nil {
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

	seenCurrent := make(map[string]struct{}, len(currentSources))
	for _, current := range currentSources {
		path := filepath.Clean(current.Path)

		previous, ok := indexed[path]
		if !ok {
			return fmt.Errorf("%w: %s was not indexed", ErrSourceStale, path)
		}

		if previous.Digest != current.Digest {
			return fmt.Errorf("%w: %s digest changed", ErrSourceStale, path)
		}

		if normalizeSourceKind(previous.Kind) != normalizeSourceKind(current.Kind) {
			return fmt.Errorf("%w: %s source kind changed", ErrSourceStale, path)
		}

		seenCurrent[path] = struct{}{}
	}

	for _, source := range idx.Sources {
		path := filepath.Clean(source.Path)
		if _, ok := seenCurrent[path]; !ok {
			return fmt.Errorf("%w: %s was deleted", ErrSourceStale, path)
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
		doc := idx.Documents[i]
		// Index.ValidateFor already enforces index-level vectorizer metadata.
		// Keep the legacy Store adapter unpinned so callers with a query vector
		// can continue using Store.Search without also providing vectorizer
		// identity; ANN search still validates per-document specs directly.
		doc.Vectorizer = VectorizerSpec{}

		if err := store.Add(doc); err != nil {
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

	rawPath := filepath.Clean(source.Path)

	if !utf8.ValidString(source.Text) {
		return fmt.Errorf("vector source %s: %w", rawPath, ErrInvalidUTF8)
	}

	sourceKind := normalizeSourceKind(source.Kind)
	sourceMetadata := SourceMetadataForTextWithKind(rawPath, source.Text, sourceKind)
	documentPath := sourceMetadata.Path

	if documentPath == "" {
		return ErrMissingID
	}

	text := privacy.RedactText(source.Text)

	chunks, err := ChunkText(documentPath, text, idx.Chunk)
	if err != nil {
		return fmt.Errorf("chunk vector source %s: %w", rawPath, err)
	}

	idx.Sources = append(idx.Sources, sourceMetadata)

	for _, chunk := range chunks {
		text, metadata := chunkDocumentPayload(source, documentPath, sourceMetadata, chunk)

		vec, err := vectorizeWithContext(ctx, vectorizer, text)
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

		indexedAt := idxDocumentTimestamp(idx)
		idx.Documents = append(idx.Documents, Document{
			ID:         chunk.ID,
			Text:       text,
			SourceHash: sourceHash(text),
			Vector:     cloneVector(vec),
			Vectorizer: documentVectorizerSpec(vectorizer, len(vec)),
			Metadata:   metadata,
			Provenance: sourceProvenance(source, sourceKind, documentPath),
			CreatedAt:  indexedAt,
			UpdatedAt:  indexedAt,
		})
	}

	return nil
}

func idxDocumentTimestamp(idx *Index) time.Time {
	if idx == nil {
		return time.Time{}
	}

	if !idx.UpdatedAt.IsZero() {
		return idx.UpdatedAt.UTC()
	}

	if !idx.CreatedAt.IsZero() {
		return idx.CreatedAt.UTC()
	}

	return time.Time{}
}

func chunkDocumentPayload(source Source, documentPath string, sourceMetadata SourceMetadata, chunk Chunk) (text string, metadata map[string]string) {
	metadata = map[string]string{
		"path":             documentPath,
		"source_kind":      normalizeSourceKind(source.Kind),
		"chunk_index":      strconv.Itoa(chunk.Index),
		"chunk_start_rune": strconv.Itoa(chunk.StartRune),
		"chunk_end_rune":   strconv.Itoa(chunk.EndRune),
		"source_digest":    sourceMetadata.Digest,
	}
	for key, value := range privacy.RedactMetadata(source.Metadata) {
		key = strings.TrimSpace(key)
		value = strings.TrimSpace(value)

		if key == "" || value == "" {
			continue
		}

		if _, reserved := metadata[key]; reserved {
			continue
		}

		metadata[key] = value
	}

	policyContext := retrieval.PolicyContext{
		Source:     retrieval.Source{Type: retrieval.SourceVector, Name: documentPath, URI: documentPath},
		Metadata:   metadata,
		DocumentID: chunk.ID,
		Path:       documentPath,
	}
	text, textSafety := retrieval.Sanitize(chunk.Text, policyContext)
	metadata, metadataSafety := retrieval.SanitizeMetadata(metadata, policyContext)
	// Keep the persisted path aligned with SourceMetadata so incremental
	// refresh can match retained chunks after safety redaction.
	metadata["path"] = documentPath

	safety := retrieval.MergeSafety(textSafety, metadataSafety)
	if strings.Contains(text, "[REDACTED]") {
		safety = retrieval.MergeSafety(safety, retrieval.Safety{
			InjectAllowed: false,
			Redacted:      true,
			Sensitive:     true,
			Reasons:       []string{"privacy redaction before indexing"},
		})
	}

	if !retrieval.IsDefaultSafety(safety) {
		metadata = retrieval.MergeSafetyMetadata(metadata, safety)
	}

	return text, metadata
}

func sourceProvenance(source Source, sourceKind, documentPath string) map[string]string {
	provenance := map[string]string{
		provenanceSourceTypeKey: sourceKind,
		"path":                  documentPath,
	}

	for key, value := range privacy.RedactMetadata(source.Provenance) {
		key = strings.TrimSpace(key)
		value = strings.TrimSpace(value)

		if key == "" || value == "" {
			continue
		}

		if _, reserved := provenance[key]; reserved {
			continue
		}

		provenance[key] = value
	}

	return ensureProvenance(provenance, sourceKind)
}

type vectorizerDimensionSpecProvider interface {
	Spec(int) VectorizerSpec
}

type vectorizerSpecProvider interface {
	Spec() VectorizerSpec
}

func documentVectorizerSpec(vectorizer Vectorizer, dimensions int) VectorizerSpec {
	if provider, ok := vectorizer.(vectorizerDimensionSpecProvider); ok {
		return provider.Spec(dimensions)
	}

	if provider, ok := vectorizer.(vectorizerSpecProvider); ok {
		return provider.Spec()
	}

	return VectorizerSpec{}
}

func vectorizeWithContext(ctx context.Context, vectorizer Vectorizer, text string) (Vector, error) {
	if ctx == nil {
		return nil, ErrContextRequired
	}

	if err := ctx.Err(); err != nil {
		return nil, fmt.Errorf("vectorize context: %w", err)
	}

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
