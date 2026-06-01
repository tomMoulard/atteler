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
	vectorSearchVectorStore     = "vector-search"
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

	ann, err := vector.NewANNIndex(idx.Documents, idx.Dimensions, vector.ANNOptions{})
	if err != nil {
		return fmt.Errorf("vector search: load ANN index: %w", err)
	}

	results, err := ann.Search(queryVector, settings.Limit)
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
	resolved := cfg.ResolveVectorizerConfig(appconfig.VectorScope{
		Store:  vectorSearchVectorStore,
		Source: vector.SourceKindFile,
	})
	settings := vectorSearchSettings{
		Vectorizer:     firstNonEmpty(opts.vectorizer, resolved.Vectorizer, vector.VectorizerKindLexical),
		Provider:       firstNonEmpty(opts.vectorProvider, resolved.Provider, vectorDefaultProvider),
		Model:          firstNonEmpty(opts.vectorModel, resolved.Model),
		BaseURL:        firstNonEmpty(opts.vectorBaseURL, resolved.BaseURL),
		FallbackPolicy: firstNonEmpty(opts.vectorFallbackPolicy, resolved.FallbackPolicy, vectorFallbackPolicyFail),
		IndexPath:      firstNonEmpty(opts.vectorStorePath, resolved.IndexPath),
		Limit:          opts.vectorLimit.value,
		Chunk: vector.ChunkOptions{
			MaxRunes:     resolved.ChunkMaxRunes,
			OverlapRunes: resolved.ChunkOverlapRunes,
		},
	}

	if settings.Limit == 0 {
		settings.Limit = 5
	}

	if settings.IndexPath == "" {
		settings.IndexPath = filepath.Join(cwd, ".atteler", "vector-index.json")
	} else {
		settings.IndexPath = rootRelativePath(cwd, settings.IndexPath)
	}

	if opts.vectorTimeout.set {
		settings.Timeout = time.Duration(opts.vectorTimeout.value) * time.Second
	} else if resolved.TimeoutSeconds > 0 {
		settings.Timeout = time.Duration(resolved.TimeoutSeconds) * time.Second
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

func scopedVectorizerConfig(scopes map[string]appconfig.VectorizerConfig, key string) appconfig.VectorizerConfig {
	key = strings.TrimSpace(key)
	if key == "" || len(scopes) == 0 {
		return appconfig.VectorizerConfig{}
	}

	if scoped, ok := scopes[key]; ok {
		return scoped
	}

	lowerKey := strings.ToLower(key)
	for name, scoped := range scopes {
		if strings.ToLower(strings.TrimSpace(name)) == lowerKey {
			return scoped
		}
	}

	normalizedKey := normalizeVectorScopeKey(key)
	for name, scoped := range scopes {
		if normalizeVectorScopeKey(name) == normalizedKey {
			return scoped
		}
	}

	return appconfig.VectorizerConfig{}
}

func normalizeVectorScopeKey(key string) string {
	key = strings.ToLower(strings.TrimSpace(key))
	key = strings.ReplaceAll(key, "_", "-")
	key = strings.ReplaceAll(key, " ", "-")

	return key
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
		loaded, err := reuseOrRefreshLoadedVectorIndex(ctx, idx, settings, paths, vectorizer, metadata)
		if loaded.handled || err != nil {
			return loaded.index, loaded.rebuilt, err
		}
	} else if !errors.Is(loadErr, os.ErrNotExist) && len(paths) == 0 {
		return nil, false, fmt.Errorf("vector search: load index: %w", loadErr)
	}

	if len(paths) == 0 {
		return nil, false, fmt.Errorf("vector search: no reusable index at %s; pass --vector-index to build one", settings.IndexPath)
	}

	refreshed, err := refreshFileVectorIndex(ctx, settings, paths, vectorizer, metadata)
	if err != nil {
		return nil, false, err
	}

	return refreshed, true, nil
}

type vectorIndexLoadResult struct {
	index   *vector.Index
	rebuilt bool
	handled bool
}

func reuseOrRefreshLoadedVectorIndex(
	ctx context.Context,
	idx *vector.Index,
	settings vectorSearchSettings,
	paths []string,
	vectorizer vector.Vectorizer,
	metadata vector.VectorizerMetadata,
) (vectorIndexLoadResult, error) {
	reuse, rebuildPaths, err := reusableVectorIndex(idx, metadata, settings.Chunk, settings.IndexPath, paths)
	if err != nil {
		return vectorIndexLoadResult{handled: true}, err
	}

	if reuse {
		return vectorIndexLoadResult{index: idx, handled: true}, nil
	}

	if len(rebuildPaths) == 0 {
		return vectorIndexLoadResult{}, nil
	}

	refreshed, err := refreshFileVectorIndex(ctx, settings, rebuildPaths, vectorizer, metadata)
	if err != nil {
		return vectorIndexLoadResult{handled: true}, err
	}

	return vectorIndexLoadResult{index: refreshed, rebuilt: true, handled: true}, nil
}

func refreshFileVectorIndex(
	ctx context.Context,
	settings vectorSearchSettings,
	paths []string,
	vectorizer vector.Vectorizer,
	metadata vector.VectorizerMetadata,
) (*vector.Index, error) {
	sources, err := vectorSourcesFromFiles(paths)
	if err != nil {
		return nil, err
	}

	if validateErr := validateSourceVectorIndexMayBeRefreshed(settings.IndexPath, vector.SourceKindFile); validateErr != nil {
		return nil, validateErr
	}

	refresh, err := vector.RefreshSourceIndex(ctx, vector.SourceIndexOptions{
		IndexPath:          settings.IndexPath,
		Sources:            sources,
		Vectorizer:         vectorizer,
		VectorizerMetadata: metadata,
		Chunk:              settings.Chunk,
	})
	if err != nil {
		return nil, fmt.Errorf("vector search: refresh file index: %w", err)
	}

	return refresh.Index, nil
}

func reusableVectorIndex(
	idx *vector.Index,
	metadata vector.VectorizerMetadata,
	chunk vector.ChunkOptions,
	indexPath string,
	paths []string,
) (reuse bool, rebuildPaths []string, err error) {
	currentPaths := mergeVectorSourcePaths(indexSourcePaths(idx), paths)

	currentSources, presentPaths, sourceErr := vectorSourceMetadataForReusableIndex(currentPaths, paths)
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

	return false, presentPaths, nil
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

func vectorSourceMetadataForReusableIndex(
	currentPaths []string,
	requestedPaths []string,
) ([]vector.SourceMetadata, []string, error) {
	requested := cleanedPathSet(requestedPaths)
	sources := make([]vector.SourceMetadata, 0, len(currentPaths))
	presentPaths := make([]string, 0, len(currentPaths))

	for _, path := range currentPaths {
		source, err := vector.SourceMetadataFromFile(path)
		if err != nil {
			clean := filepath.Clean(strings.TrimSpace(path))
			if len(requestedPaths) > 0 && !requested[clean] && errors.Is(err, os.ErrNotExist) {
				continue
			}

			return nil, nil, fmt.Errorf("vector source metadata %s: %w", path, err)
		}

		sources = append(sources, source)
		presentPaths = append(presentPaths, path)
	}

	return sources, presentPaths, nil
}

func cleanedPathSet(paths []string) map[string]bool {
	out := make(map[string]bool, len(paths))
	for _, path := range paths {
		clean := filepath.Clean(strings.TrimSpace(path))
		if clean == "" || clean == "." {
			continue
		}

		out[clean] = true
	}

	return out
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

	if strings.HasSuffix(indexPath, ".lexical") {
		return indexPath
	}

	extension := filepath.Ext(indexPath)
	if extension == "" {
		return indexPath + ".lexical"
	}

	stem := strings.TrimSuffix(indexPath, extension)
	if strings.HasSuffix(stem, ".lexical") {
		return indexPath
	}

	return stem + ".lexical" + extension
}

func rootRelativePath(root, path string) string {
	path = strings.TrimSpace(path)
	if path == "" {
		return ""
	}

	if filepath.IsAbs(path) || strings.TrimSpace(root) == "" {
		return filepath.Clean(path)
	}

	return filepath.Join(root, path)
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
