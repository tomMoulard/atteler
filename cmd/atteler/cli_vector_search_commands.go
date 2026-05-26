package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	appconfig "github.com/tommoulard/atteler/pkg/config"
	"github.com/tommoulard/atteler/pkg/vector"
)

type vectorSearchSettings struct {
	IndexPath      string
	Vectorizer     string
	Provider       string
	Model          string
	BaseURL        string
	FallbackPolicy string
	Timeout        time.Duration
	Chunk          vector.ChunkOptions
	Limit          int
}

const (
	vectorDefaultBaseURL        = "http://127.0.0.1:11434"
	vectorDefaultModel          = "nomic-embed-text"
	vectorDefaultProvider       = "ollama"
	vectorDefaultTimeoutSeconds = "30"
	vectorFallbackPolicyFail    = "fail"
)

func runVectorSearch(ctx context.Context, cwd string, cfg appconfig.VectorConfig, opts cliOptions) error {
	settings, err := vectorSearchSettingsFromOptions(cwd, cfg, opts)
	if err != nil {
		return err
	}

	err = runVectorSearchOnce(ctx, settings, opts.vectorSearch, opts.vectorIndexFiles)
	if err == nil {
		return nil
	}

	if settings.Vectorizer != vector.VectorizerKindEmbedding ||
		settings.FallbackPolicy != vector.VectorizerKindLexical ||
		!vectorSearchEmbeddingFailure(err) {
		return err
	}

	fmt.Fprintln(os.Stderr, "warning: embedding vector search failed; falling back to lexical hashed token-frequency retrieval: "+err.Error())

	fallbackPaths := append([]string(nil), opts.vectorIndexFiles...)
	if len(fallbackPaths) == 0 {
		fallbackPaths = vectorIndexSourcePaths(settings.IndexPath)
	}

	settings.Vectorizer = vector.VectorizerKindLexical
	settings.Provider = ""
	settings.BaseURL = ""
	settings.Model = vector.LexicalFallbackModel
	settings.FallbackPolicy = vectorFallbackPolicyFail
	settings.IndexPath = lexicalFallbackIndexPath(settings.IndexPath)

	return runVectorSearchOnce(ctx, settings, opts.vectorSearch, fallbackPaths)
}

func runVectorSearchOnce(ctx context.Context, settings vectorSearchSettings, query string, paths []string) error {
	query = strings.TrimSpace(query)
	if query == "" && len(paths) == 0 {
		return errors.New("vector search: --vector-search or --vector-index is required")
	}

	vectorizer, metadata, err := newVectorSearchVectorizer(settings)
	if err != nil {
		return err
	}

	queryVector, metadata, err := prepareVectorSearchQuery(ctx, settings, paths, vectorizer, metadata, query)
	if err != nil {
		return err
	}

	idx, rebuilt, err := loadOrBuildVectorIndex(ctx, settings, paths, vectorizer, metadata)
	if err != nil {
		return err
	}

	if rebuilt {
		if saveErr := idx.Save(settings.IndexPath); saveErr != nil {
			return fmt.Errorf("vector search: save index: %w", saveErr)
		}
	}

	if query == "" {
		fmt.Printf(
			"Indexed %d chunk(s) from %d source file(s) with %s into %s\n",
			len(idx.Documents),
			len(idx.Sources),
			formatVectorizerMetadata(idx.Vectorizer),
			settings.IndexPath,
		)

		return nil
	}

	if len(queryVector) != idx.Dimensions {
		return fmt.Errorf(
			"vector search: reusable index %s is invalid: %w: query has %d dimensions, index has %d; pass --vector-index to rebuild",
			settings.IndexPath,
			vector.ErrDimensionMismatch,
			len(queryVector),
			idx.Dimensions,
		)
	}

	store, err := idx.Store()
	if err != nil {
		return fmt.Errorf("vector search: load index: %w", err)
	}

	results, err := store.Search(queryVector, settings.Limit)
	if err != nil {
		return fmt.Errorf("vector search failed: %w", err)
	}

	fmt.Println(formatVectorSearchHeader(idx, settings.IndexPath, rebuilt))

	if len(results) == 0 {
		fmt.Println("No vector results found.")
		return nil
	}

	for i := range results {
		fmt.Println(formatVectorResult(results[i]))
	}

	return nil
}

func prepareVectorSearchQuery(
	ctx context.Context,
	settings vectorSearchSettings,
	paths []string,
	vectorizer vector.Vectorizer,
	metadata vector.VectorizerMetadata,
	query string,
) (vector.Vector, vector.VectorizerMetadata, error) {
	if query == "" {
		return nil, metadata, nil
	}

	if len(paths) == 0 {
		if validateErr := validateReusableVectorIndexForQuery(settings, metadata); validateErr != nil {
			return nil, metadata, validateErr
		}
	}

	queryVector, err := vectorizeSearchText(ctx, vectorizer, query)
	if err != nil {
		return nil, metadata, fmt.Errorf("vector search: vectorize query: %w", err)
	}

	metadata.Dimensions = len(queryVector)

	return queryVector, metadata, nil
}

func validateReusableVectorIndexForQuery(settings vectorSearchSettings, metadata vector.VectorizerMetadata) error {
	idx, err := vector.LoadIndex(settings.IndexPath)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("vector search: no reusable index at %s; pass --vector-index to build one", settings.IndexPath)
		}

		return fmt.Errorf("vector search: load index: %w", err)
	}

	_, _, err = reusableVectorIndex(idx, metadata, settings.Chunk, settings.IndexPath, nil)
	if err != nil {
		return err
	}

	return nil
}

func vectorSearchSettingsFromOptions(cwd string, cfg appconfig.VectorConfig, opts cliOptions) (vectorSearchSettings, error) {
	settings := vectorSearchSettings{
		Vectorizer:     firstNonEmpty(opts.vectorizer, cfg.Vectorizer, vector.VectorizerKindLexical),
		Provider:       firstNonEmpty(opts.vectorProvider, cfg.Provider, vectorDefaultProvider),
		Model:          firstNonEmpty(opts.vectorModel, cfg.Model),
		BaseURL:        firstNonEmpty(opts.vectorBaseURL, cfg.BaseURL),
		FallbackPolicy: firstNonEmpty(opts.vectorFallbackPolicy, cfg.FallbackPolicy, vectorFallbackPolicyFail),
		IndexPath:      firstNonEmpty(opts.vectorStorePath, cfg.IndexPath),
		Limit:          opts.vectorLimit.value,
		Chunk: vector.ChunkOptions{
			MaxRunes:     cfg.ChunkMaxRunes,
			OverlapRunes: cfg.ChunkOverlapRunes,
		},
	}

	if settings.Limit == 0 {
		settings.Limit = 5
	}

	if settings.IndexPath == "" {
		settings.IndexPath = filepath.Join(cwd, ".atteler", "vector-index.json")
	}

	if opts.vectorTimeout.set {
		settings.Timeout = time.Duration(opts.vectorTimeout.value) * time.Second
	} else if cfg.TimeoutSeconds > 0 {
		settings.Timeout = time.Duration(cfg.TimeoutSeconds) * time.Second
	}

	if opts.vectorChunkMaxRunes.set {
		settings.Chunk.MaxRunes = opts.vectorChunkMaxRunes.value
	}

	if opts.vectorChunkOverlapRunes.set {
		settings.Chunk.OverlapRunes = opts.vectorChunkOverlapRunes.value
	}

	settings.Chunk = settings.Chunk.Normalize()

	var err error

	settings.Vectorizer, err = normalizeVectorizerKind(settings.Vectorizer)
	if err != nil {
		return vectorSearchSettings{}, err
	}

	settings.FallbackPolicy, err = normalizeVectorFallbackPolicy(settings.FallbackPolicy)
	if err != nil {
		return vectorSearchSettings{}, err
	}

	return settings, nil
}

func newVectorSearchVectorizer(settings vectorSearchSettings) (vector.Vectorizer, vector.VectorizerMetadata, error) {
	switch settings.Vectorizer {
	case vector.VectorizerKindLexical:
		vectorizer, err := vector.NewTextVectorizer(0)
		if err != nil {
			return nil, vector.VectorizerMetadata{}, fmt.Errorf("vector search: create lexical fallback vectorizer: %w", err)
		}

		return vectorizer, vectorizer.Metadata(), nil
	case vector.VectorizerKindEmbedding:
		provider, err := normalizeEmbeddingProvider(settings.Provider)
		if err != nil {
			return nil, vector.VectorizerMetadata{}, err
		}

		if provider == "" {
			provider = vectorDefaultProvider
		}

		settings.Provider = provider

		options := []vector.EmbeddingOption{
			vector.WithEmbeddingProvider(settings.Provider),
			vector.WithEmbeddingModel(settings.Model),
			vector.WithEmbeddingBaseURL(settings.BaseURL),
		}
		if settings.Timeout > 0 {
			options = append(options, vector.WithEmbeddingTimeout(settings.Timeout))
		}

		vectorizer := vector.NewEmbeddingVectorizer(options...)
		metadata := vectorizer.Metadata(0)

		return vectorizer, metadata, nil
	default:
		return nil, vector.VectorizerMetadata{}, fmt.Errorf("vector search: unsupported vectorizer %q", settings.Vectorizer)
	}
}

func loadOrBuildVectorIndex(
	ctx context.Context,
	settings vectorSearchSettings,
	paths []string,
	vectorizer vector.Vectorizer,
	metadata vector.VectorizerMetadata,
) (*vector.Index, bool, error) {
	idx, loadErr := vector.LoadIndex(settings.IndexPath)
	if loadErr == nil {
		reuse, rebuildPaths, reuseErr := reusableVectorIndex(idx, metadata, settings.Chunk, settings.IndexPath, paths)
		if reuseErr != nil {
			return nil, false, reuseErr
		}

		if reuse {
			return idx, false, nil
		}

		if len(rebuildPaths) > 0 {
			paths = rebuildPaths
		}
	} else if !errors.Is(loadErr, os.ErrNotExist) && len(paths) == 0 {
		return nil, false, fmt.Errorf("vector search: load index: %w", loadErr)
	}

	if len(paths) == 0 {
		return nil, false, fmt.Errorf("vector search: no reusable index at %s; pass --vector-index to build one", settings.IndexPath)
	}

	sources, err := vectorSourcesFromFiles(paths)
	if err != nil {
		return nil, false, err
	}

	idx, err = vector.BuildIndex(ctx, sources, vectorizer, metadata, settings.Chunk, time.Now().UTC())
	if err != nil {
		return nil, false, fmt.Errorf("vector search: build index: %w", err)
	}

	return idx, true, nil
}

func reusableVectorIndex(
	idx *vector.Index,
	metadata vector.VectorizerMetadata,
	chunk vector.ChunkOptions,
	indexPath string,
	paths []string,
) (reuse bool, rebuildPaths []string, err error) {
	currentPaths := mergeVectorSourcePaths(indexSourcePaths(idx), paths)

	currentSources, sourceErr := vectorSourceMetadata(currentPaths)
	if sourceErr != nil {
		if len(paths) == 0 {
			return false, nil, fmt.Errorf("vector search: validate index sources: %w", sourceErr)
		}

		return false, nil, nil
	}

	validateErr := idx.ValidateFor(metadata, currentSources, chunk)
	if validateErr == nil {
		return true, nil, nil
	}

	if len(paths) == 0 {
		return false, nil, fmt.Errorf("vector search: reusable index %s is invalid: %w; pass --vector-index to rebuild", indexPath, validateErr)
	}

	return false, currentPaths, nil
}

func vectorSourcesFromFiles(paths []string) ([]vector.Source, error) {
	sources := make([]vector.Source, 0, len(paths))
	for _, path := range paths {
		source, err := vector.SourceFromFile(path)
		if err != nil {
			return nil, fmt.Errorf("vector search: read %s: %w", path, err)
		}

		sources = append(sources, source)
	}

	return sources, nil
}

func vectorSourceMetadata(paths []string) ([]vector.SourceMetadata, error) {
	sources := make([]vector.SourceMetadata, 0, len(paths))
	for _, path := range paths {
		source, err := vector.SourceMetadataFromFile(path)
		if err != nil {
			return nil, fmt.Errorf("vector source metadata %s: %w", path, err)
		}

		sources = append(sources, source)
	}

	return sources, nil
}

func vectorizeSearchText(ctx context.Context, vectorizer vector.Vectorizer, text string) (vector.Vector, error) {
	if contextual, ok := vectorizer.(vector.VectorizerContext); ok {
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

func normalizeVectorizerKind(kind string) (string, error) {
	kind = strings.ToLower(strings.TrimSpace(kind))
	kind = strings.ReplaceAll(kind, "_", "-")

	switch kind {
	case "", vector.VectorizerKindLexical, "lexical-fallback", "fallback", "text", "hashed", "hashed-token-frequency":
		return vector.VectorizerKindLexical, nil
	case vector.VectorizerKindEmbedding, "embed", "embeddings":
		return vector.VectorizerKindEmbedding, nil
	default:
		return "", fmt.Errorf("vector search: unsupported vectorizer %q (supported: lexical, embedding)", kind)
	}
}

func normalizeVectorFallbackPolicy(policy string) (string, error) {
	policy = strings.ToLower(strings.TrimSpace(policy))
	policy = strings.ReplaceAll(policy, "_", "-")

	switch policy {
	case "", vectorFallbackPolicyFail, "none":
		return vectorFallbackPolicyFail, nil
	case vector.VectorizerKindLexical, "lexical-fallback", "fallback":
		return vector.VectorizerKindLexical, nil
	default:
		return "", fmt.Errorf("vector search: unsupported vector fallback policy %q (supported: fail, lexical)", policy)
	}
}

func normalizeEmbeddingProvider(provider string) (string, error) {
	provider = strings.ToLower(strings.TrimSpace(provider))
	provider = strings.ReplaceAll(provider, "_", "-")

	switch provider {
	case "", vectorDefaultProvider, "ollama-compatible":
		return vectorDefaultProvider, nil
	default:
		return "", fmt.Errorf("vector search: unsupported embedding provider %q (supported: ollama-compatible)", provider)
	}
}

func vectorSearchEmbeddingFailure(err error) bool {
	return err != nil && strings.Contains(err.Error(), "embedding:")
}

func indexSourcePaths(idx *vector.Index) []string {
	if idx == nil {
		return nil
	}

	paths := make([]string, 0, len(idx.Sources))
	for _, source := range idx.Sources {
		if strings.TrimSpace(source.Path) != "" {
			paths = append(paths, source.Path)
		}
	}

	return paths
}

func mergeVectorSourcePaths(existing, requested []string) []string {
	seen := make(map[string]struct{}, len(existing)+len(requested))
	paths := make([]string, 0, len(existing)+len(requested))

	for _, path := range append(append([]string(nil), existing...), requested...) {
		path = filepath.Clean(strings.TrimSpace(path))
		if path == "." || path == "" {
			continue
		}

		if _, ok := seen[path]; ok {
			continue
		}

		seen[path] = struct{}{}
		paths = append(paths, path)
	}

	return paths
}

func vectorIndexSourcePaths(indexPath string) []string {
	idx, err := vector.LoadIndex(indexPath)
	if err != nil {
		return nil
	}

	return indexSourcePaths(idx)
}

func lexicalFallbackIndexPath(indexPath string) string {
	indexPath = strings.TrimSpace(indexPath)
	if indexPath == "" {
		return "vector-index.lexical.json"
	}

	extension := filepath.Ext(indexPath)
	if extension == "" {
		return indexPath + ".lexical"
	}

	return strings.TrimSuffix(indexPath, extension) + ".lexical" + extension
}

func formatVectorSearchHeader(idx *vector.Index, indexPath string, rebuilt bool) string {
	parts := []string{
		"vector_ranking",
		formatVectorizerMetadata(idx.Vectorizer),
		fmt.Sprintf("dimensions=%d", idx.Dimensions),
		fmt.Sprintf("chunks=%d", len(idx.Documents)),
		fmt.Sprintf("sources=%d", len(idx.Sources)),
		"index=" + indexPath,
	}
	if rebuilt {
		parts = append(parts, "rebuilt=true")
	}

	if !idx.CreatedAt.IsZero() {
		parts = append(parts, "created_at="+idx.CreatedAt.UTC().Format(time.RFC3339))
	}

	return strings.Join(parts, "\t")
}

func formatVectorizerMetadata(metadata vector.VectorizerMetadata) string {
	metadata = metadata.Normalize()
	parts := []string{
		"vectorizer=" + metadata.Label(),
		"model=" + metadata.Model,
	}

	if metadata.Provider != "" {
		parts = append(parts, "provider="+metadata.Provider)
	}

	if metadata.BaseURL != "" {
		parts = append(parts, "base_url="+metadata.BaseURL)
	}

	return strings.Join(parts, "\t")
}

func formatVectorResult(result vector.Result) string {
	parts := []string{
		result.Document.ID,
		fmt.Sprintf("score=%.4f", result.Score),
	}
	if path := result.Document.Metadata["path"]; path != "" {
		parts = append(parts, "path="+path)
	}

	if chunk := result.Document.Metadata["chunk_index"]; chunk != "" {
		parts = append(parts, "chunk="+chunk)
	}

	if start := result.Document.Metadata["chunk_start_rune"]; start != "" {
		end := result.Document.Metadata["chunk_end_rune"]
		if end != "" {
			parts = append(parts, "range="+start+"-"+end)
		}
	}

	return strings.Join(parts, "\t")
}
