package vector

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// SourceIndexOptions controls persistence and incremental refresh for an
// explicit local RAG source set such as sessions, git history, ADRs, or any
// caller-discovered corpus.
//
//nolint:govet // Public option field order keeps lifecycle groups readable.
type SourceIndexOptions struct {
	Now                func() time.Time
	Vectorizer         Vectorizer
	VectorizerMetadata VectorizerMetadata
	Chunk              ChunkOptions
	Sources            []Source
	IndexPath          string
}

// SourceIndexRefreshResult summarizes RefreshSourceIndex.
type SourceIndexRefreshResult struct {
	Index       *Index
	IndexPath   string
	Added       int
	Updated     int
	Deleted     int
	Unchanged   int
	Documents   int
	Rebuilt     bool
	Refreshed   bool
	Initialized bool
}

// SourceIndexAsyncResult is returned by RefreshSourceIndexAsync.
type SourceIndexAsyncResult struct {
	Err     error
	Refresh SourceIndexRefreshResult
}

// RefreshSourceIndexAsync starts the same incremental source-index lifecycle
// as RefreshSourceIndex in a goroutine and returns exactly one result. It is
// the source-corpus counterpart to RefreshWorkspaceIndexAsync for callers that
// want session, git-history, ADR, or other non-file RAG sources to sync in the
// background without changing invalidation behavior.
func RefreshSourceIndexAsync(ctx context.Context, opts SourceIndexOptions) <-chan SourceIndexAsyncResult {
	results := make(chan SourceIndexAsyncResult, 1)

	go func() {
		defer close(results)

		refresh, err := RefreshSourceIndex(ctx, opts)
		results <- SourceIndexAsyncResult{Refresh: refresh, Err: err}
	}()

	return results
}

// RefreshSourceIndex loads an existing persisted vector index for opts.Sources,
// re-vectorizes only added or changed sources, removes deleted sources, and
// saves the fresh index. It is the generic lifecycle primitive for non-file
// local RAG sources while RefreshWorkspaceIndex owns filesystem discovery.
func RefreshSourceIndex(ctx context.Context, opts SourceIndexOptions) (SourceIndexRefreshResult, error) {
	if ctx == nil {
		return SourceIndexRefreshResult{}, ErrContextRequired
	}

	if err := ctx.Err(); err != nil {
		return SourceIndexRefreshResult{}, fmt.Errorf("source vector index: refresh canceled: %w", err)
	}

	opts, err := normalizeSourceIndexOptions(opts)
	if err != nil {
		return SourceIndexRefreshResult{}, err
	}

	result := SourceIndexRefreshResult{IndexPath: opts.IndexPath}
	if len(opts.Sources) == 0 {
		return clearSourceIndexForNoSources(opts, result)
	}

	existing, loadErr := loadIndex(opts.IndexPath, refreshIndexValidationOptions())
	switch {
	case loadErr == nil:
		refreshed, reused, reuseErr := refreshReusableSourceIndex(ctx, existing, opts, result)
		if reuseErr != nil {
			return SourceIndexRefreshResult{}, reuseErr
		}

		if reused {
			return refreshed, nil
		}
	case errors.Is(loadErr, os.ErrNotExist):
	case workspaceIndexRequiresRebuild(loadErr):
		if rebuildErr := validateSourceIndexPathMayBeRebuilt(opts.IndexPath); rebuildErr != nil {
			return SourceIndexRefreshResult{}, rebuildErr
		}
	default:
		return SourceIndexRefreshResult{}, fmt.Errorf("source vector index: load: %w", loadErr)
	}

	index, err := BuildIndex(ctx, opts.Sources, opts.Vectorizer, opts.VectorizerMetadata, opts.Chunk, sourceIndexNow(opts))
	if err != nil {
		return SourceIndexRefreshResult{}, fmt.Errorf("source vector index: build: %w", err)
	}

	if err := index.Save(opts.IndexPath); err != nil {
		return SourceIndexRefreshResult{}, fmt.Errorf("source vector index: save: %w", err)
	}

	result.Index = index
	result.Documents = len(index.Documents)
	result.Added = len(index.Sources)
	result.Initialized = errors.Is(loadErr, os.ErrNotExist)
	result.Rebuilt = !result.Initialized
	result.Refreshed = true

	return result, nil
}

func validateSourceIndexPathMayBeRebuilt(indexPath string) error {
	if workspaceIndexFileLooksManaged(indexPath) {
		return nil
	}

	return fmt.Errorf("source vector index: refusing to overwrite existing non-index file %s", indexPath)
}

func normalizeSourceIndexOptions(opts SourceIndexOptions) (SourceIndexOptions, error) {
	if strings.TrimSpace(opts.IndexPath) == "" {
		return SourceIndexOptions{}, errors.New("source vector index: index path is required")
	}

	opts.IndexPath = filepath.Clean(opts.IndexPath)

	if opts.Vectorizer == nil {
		vectorizer, err := NewTextVectorizer(0)
		if err != nil {
			return SourceIndexOptions{}, err
		}

		opts.Vectorizer = vectorizer
		opts.VectorizerMetadata = vectorizer.Metadata()
	}

	opts.VectorizerMetadata = normalizeVectorizerMetadataForIndex(opts.Vectorizer, opts.VectorizerMetadata)
	if opts.VectorizerMetadata.Kind == "" {
		return SourceIndexOptions{}, fmt.Errorf("%w: source vectorizer metadata is required", ErrMetadataMismatch)
	}

	opts.Chunk = opts.Chunk.Normalize()

	opts.Sources = cloneSources(opts.Sources)
	if err := validateUniqueSourceIndexSources(opts.Sources); err != nil {
		return SourceIndexOptions{}, err
	}

	return opts, nil
}

func validateUniqueSourceIndexSources(sources []Source) error {
	if len(sources) < 2 {
		return nil
	}

	seen := make(map[string]struct{}, len(sources))
	for _, source := range sources {
		meta := sourceMetadataForSource(source)
		if meta.Path == "" {
			continue
		}

		if _, ok := seen[meta.Path]; ok {
			return fmt.Errorf("%w: duplicate source path %q", ErrSourceStale, meta.Path)
		}

		seen[meta.Path] = struct{}{}
	}

	return nil
}

func cloneSources(sources []Source) []Source {
	if len(sources) == 0 {
		return nil
	}

	out := make([]Source, 0, len(sources))
	for _, source := range sources {
		out = append(out, Source{
			Metadata:   cloneMetadata(source.Metadata),
			Provenance: cloneMetadata(source.Provenance),
			Kind:       source.Kind,
			Path:       source.Path,
			Text:       source.Text,
		})
	}

	return out
}

func clearSourceIndexForNoSources(
	opts SourceIndexOptions,
	result SourceIndexRefreshResult,
) (SourceIndexRefreshResult, error) {
	existing, err := LoadIndex(opts.IndexPath)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return result, ErrNoSources
		}

		if workspaceIndexRequiresRebuild(err) && workspaceIndexFileLooksManaged(opts.IndexPath) {
			if removeErr := os.Remove(opts.IndexPath); removeErr != nil && !errors.Is(removeErr, os.ErrNotExist) {
				return SourceIndexRefreshResult{}, fmt.Errorf("source vector index: remove stale empty source index %s: %w", opts.IndexPath, removeErr)
			}

			result.Refreshed = true

			return result, ErrNoSources
		}

		return SourceIndexRefreshResult{}, fmt.Errorf("source vector index: load: %w", err)
	}

	result.Deleted = len(existing.Sources)

	if removeErr := os.Remove(opts.IndexPath); removeErr != nil && !errors.Is(removeErr, os.ErrNotExist) {
		return SourceIndexRefreshResult{}, fmt.Errorf("source vector index: remove empty source index %s: %w", opts.IndexPath, removeErr)
	}

	result.Refreshed = result.Deleted > 0 || len(existing.Documents) > 0

	return result, ErrNoSources
}

func refreshReusableSourceIndex(
	ctx context.Context,
	existing *Index,
	opts SourceIndexOptions,
	result SourceIndexRefreshResult,
) (SourceIndexRefreshResult, bool, error) {
	index, changed, err := refreshExistingSourceIndex(ctx, existing, opts, &result)
	if err != nil {
		if workspaceIndexRequiresRebuild(err) {
			return result, false, nil
		}

		return SourceIndexRefreshResult{}, false, err
	}

	result.Index = index
	result.Documents = len(index.Documents)
	result.Refreshed = changed

	if !changed {
		if err := os.Chmod(opts.IndexPath, 0o600); err != nil {
			return SourceIndexRefreshResult{}, false, fmt.Errorf("source vector index: chmod reusable index %s: %w", opts.IndexPath, err)
		}

		return result, true, nil
	}

	if saveErr := index.Save(opts.IndexPath); saveErr != nil {
		return SourceIndexRefreshResult{}, false, fmt.Errorf("source vector index: save: %w", saveErr)
	}

	return result, true, nil
}

func refreshExistingSourceIndex(
	ctx context.Context,
	existing *Index,
	opts SourceIndexOptions,
	result *SourceIndexRefreshResult,
) (*Index, bool, error) {
	if err := validateReusableSourceIndex(existing, opts); err != nil {
		return nil, false, err
	}

	plan := planSourceIndexRefresh(existing, opts, result)
	if !plan.changed {
		return existing, false, nil
	}

	refreshedAt := sourceIndexNow(opts)

	index := &Index{
		Version:    IndexVersion,
		CreatedAt:  existing.CreatedAt,
		UpdatedAt:  refreshedAt,
		Vectorizer: existing.Vectorizer.Normalize(),
		Chunk:      opts.Chunk.Normalize(),
		Dimensions: existing.Dimensions,
		Sources:    plan.retainedSources,
		Documents:  plan.retainedDocuments,
	}
	if index.CreatedAt.IsZero() {
		index.CreatedAt = refreshedAt
	}

	if len(plan.retainedDocuments) == 0 {
		index.Dimensions = 0
		index.Vectorizer = opts.VectorizerMetadata.Normalize()
	}

	for _, source := range plan.changedSources {
		if err := index.addSource(ctx, source, opts.Vectorizer); err != nil {
			return nil, false, err
		}
	}

	if index.Dimensions == 0 || len(index.Documents) == 0 {
		return nil, false, ErrEmptyText
	}

	index.Vectorizer = opts.VectorizerMetadata.Normalize()
	index.Vectorizer.Dimensions = index.Dimensions
	sort.SliceStable(index.Sources, func(i, j int) bool {
		return index.Sources[i].Path < index.Sources[j].Path
	})
	sort.SliceStable(index.Documents, func(i, j int) bool {
		return index.Documents[i].ID < index.Documents[j].ID
	})

	return index, true, nil
}

type sourceIndexRefreshPlan struct {
	changedSources    []Source
	retainedSources   []SourceMetadata
	retainedDocuments []Document
	changed           bool
}

func planSourceIndexRefresh(
	existing *Index,
	opts SourceIndexOptions,
	result *SourceIndexRefreshResult,
) sourceIndexRefreshPlan {
	currentMeta, currentByPath := sourceIndexCurrentMetadata(opts.Sources)
	existingMeta := sourceIndexExistingMetadata(existing.Sources)
	documentsByPath := indexDocumentsByPath(existing.Documents)

	plan := sourceIndexRefreshPlan{
		changedSources:    make([]Source, 0),
		retainedSources:   make([]SourceMetadata, 0, len(opts.Sources)),
		retainedDocuments: make([]Document, 0, len(existing.Documents)),
	}

	for _, meta := range currentMeta {
		planSourceIndexCurrentSource(&plan, result, existing, opts, documentsByPath, currentByPath, existingMeta, meta)
	}

	for _, meta := range existing.Sources {
		if _, ok := currentByPath[filepath.Clean(meta.Path)]; !ok {
			result.Deleted++
			plan.changed = true
		}
	}

	return plan
}

func sourceIndexCurrentMetadata(sources []Source) (currentMeta []SourceMetadata, currentByPath map[string]Source) {
	currentMeta = make([]SourceMetadata, 0, len(sources))
	currentByPath = make(map[string]Source, len(sources))

	for _, source := range sources {
		meta := sourceMetadataForSource(source)
		currentMeta = append(currentMeta, meta)
		currentByPath[meta.Path] = source
	}

	return currentMeta, currentByPath
}

func sourceIndexExistingMetadata(sources []SourceMetadata) map[string]SourceMetadata {
	existingMeta := make(map[string]SourceMetadata, len(sources))
	for _, meta := range sources {
		existingMeta[filepath.Clean(meta.Path)] = meta
	}

	return existingMeta
}

func planSourceIndexCurrentSource(
	plan *sourceIndexRefreshPlan,
	result *SourceIndexRefreshResult,
	existing *Index,
	opts SourceIndexOptions,
	documentsByPath map[string][]Document,
	currentByPath map[string]Source,
	existingMeta map[string]SourceMetadata,
	meta SourceMetadata,
) {
	source := currentByPath[meta.Path]
	previous, ok := existingMeta[meta.Path]

	switch {
	case !ok:
		result.Added++
		plan.changed = true

		plan.changedSources = append(plan.changedSources, source)
	case previous.Digest != meta.Digest || normalizeSourceKind(previous.Kind) != normalizeSourceKind(meta.Kind):
		result.Updated++
		plan.changed = true

		plan.changedSources = append(plan.changedSources, source)
	default:
		docs := documentsByPath[meta.Path]
		if indexDocumentsNeedRefresh(docs, existing.Vectorizer, source, opts.Chunk) {
			result.Updated++
			plan.changed = true

			plan.changedSources = append(plan.changedSources, source)

			return
		}

		result.Unchanged++

		plan.retainedSources = append(plan.retainedSources, meta)

		docs, metadataChanged := refreshRetainedDocumentFreshnessMetadata(docs, source, opts.Chunk)
		if metadataChanged {
			result.Updated++
			result.Unchanged--
			plan.changed = true
		}

		plan.retainedDocuments = append(plan.retainedDocuments, docs...)
	}
}

func validateReusableSourceIndex(existing *Index, opts SourceIndexOptions) error {
	if err := existing.validateFor(opts.VectorizerMetadata, nil, refreshIndexValidationOptions(), opts.Chunk); err != nil {
		return err
	}

	return validateIndexSourceCoverage(existing)
}

func sourceMetadataForSource(source Source) SourceMetadata {
	return SourceMetadataForTextWithKind(source.Path, source.Text, source.Kind)
}

func sourceIndexNow(opts SourceIndexOptions) time.Time {
	if opts.Now != nil {
		return opts.Now().UTC()
	}

	return time.Now().UTC()
}
