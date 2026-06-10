package vector

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/tommoulard/atteler/pkg/privacy"
	"github.com/tommoulard/atteler/pkg/retrieval"
)

const (
	// DefaultWorkspaceIndexPath is the per-workspace persisted vector datastore
	// path used when callers do not configure a custom location. ANN buckets are
	// derived in memory from this JSON index when searching.
	DefaultWorkspaceIndexPath = ".atteler/workspace-vector-index.json"
	// DefaultWorkspaceMaxFileBytes prevents accidentally embedding huge files.
	DefaultWorkspaceMaxFileBytes = 256 * 1024
	// DefaultWorkspaceMaxFiles prevents pathological repository walks from
	// blocking the harness indefinitely.
	DefaultWorkspaceMaxFiles = 5000
)

// WorkspaceOptions controls workspace discovery and index persistence.
//
// Root is the current folder where atteler was launched. Paths persisted into
// the index are relative to Root so local indexes do not leak absolute private
// filesystem locations.
//
//nolint:govet // Public option field order keeps behavior groups readable.
type WorkspaceOptions struct {
	Now                func() time.Time
	Vectorizer         Vectorizer
	VectorizerMetadata VectorizerMetadata
	Chunk              ChunkOptions
	Root               string
	IndexPath          string
	IncludePatterns    []string
	ExcludePatterns    []string
	MaxFileBytes       int64
	MaxFiles           int
}

// WorkspaceSkip records why a path was not considered indexable.
type WorkspaceSkip struct {
	Path   string
	Reason string
}

// WorkspaceRefreshResult summarizes a load/refresh operation.
//
//nolint:govet // Public result field order keeps status counters grouped.
type WorkspaceRefreshResult struct {
	Index       *Index
	Skipped     []WorkspaceSkip
	IndexPath   string
	Added       int
	Updated     int
	Deleted     int
	Unchanged   int
	Indexed     int
	Documents   int
	Rebuilt     bool
	Refreshed   bool
	Initialized bool
}

// WorkspaceAsyncResult is returned by RefreshWorkspaceIndexAsync.
type WorkspaceAsyncResult struct {
	Err     error
	Refresh WorkspaceRefreshResult
}

// RefreshWorkspaceIndexAsync starts the same incremental index lifecycle as
// RefreshWorkspaceIndex in a goroutine and returns exactly one result. This is
// intentionally a small primitive so callers can schedule background local RAG
// sync without changing the persistence, invalidation, and privacy behavior
// that the synchronous path already tests.
func RefreshWorkspaceIndexAsync(ctx context.Context, opts WorkspaceOptions) <-chan WorkspaceAsyncResult {
	results := make(chan WorkspaceAsyncResult, 1)

	go func() {
		defer close(results)

		refresh, err := RefreshWorkspaceIndex(ctx, opts)
		results <- WorkspaceAsyncResult{Refresh: refresh, Err: err}
	}()

	return results
}

// RefreshWorkspaceIndex discovers supported files under Root, loads any
// compatible existing datastore, refreshes only changed/deleted/added sources,
// persists the result, and returns the fresh index.
func RefreshWorkspaceIndex(ctx context.Context, opts WorkspaceOptions) (WorkspaceRefreshResult, error) {
	if ctx == nil {
		return WorkspaceRefreshResult{}, ErrContextRequired
	}

	if err := ctx.Err(); err != nil {
		return WorkspaceRefreshResult{}, fmt.Errorf("workspace vector index: refresh canceled: %w", err)
	}

	opts, err := normalizeWorkspaceOptions(opts)
	if err != nil {
		return WorkspaceRefreshResult{}, err
	}

	sources, skipped, err := DiscoverWorkspaceSources(ctx, opts)
	if err != nil {
		return WorkspaceRefreshResult{}, err
	}

	result := WorkspaceRefreshResult{
		Skipped:   skipped,
		IndexPath: opts.IndexPath,
		Indexed:   len(sources),
	}

	if len(sources) == 0 {
		return clearWorkspaceIndexForNoSources(opts, result)
	}

	existing, loadErr := loadWorkspaceRefreshIndex(opts.IndexPath)
	switch {
	case loadErr == nil:
		refreshed, reused, reuseErr := refreshReusableWorkspaceIndex(ctx, existing, sources, opts, result)
		if reuseErr != nil {
			return WorkspaceRefreshResult{}, reuseErr
		}

		if reused {
			return refreshed, nil
		}
	case errors.Is(loadErr, os.ErrNotExist):
	case workspaceIndexRequiresRebuild(loadErr):
		if rebuildErr := validateWorkspaceIndexPathMayBeRebuilt(opts.Root, opts.IndexPath); rebuildErr != nil {
			return WorkspaceRefreshResult{}, rebuildErr
		}
	default:
		return WorkspaceRefreshResult{}, fmt.Errorf("workspace vector index: load: %w", loadErr)
	}

	index, err := BuildIndex(ctx, sources, opts.Vectorizer, opts.VectorizerMetadata, opts.Chunk, workspaceNow(opts))
	if err != nil {
		return WorkspaceRefreshResult{}, fmt.Errorf("workspace vector index: build: %w", err)
	}

	if err := index.Save(opts.IndexPath); err != nil {
		return WorkspaceRefreshResult{}, fmt.Errorf("workspace vector index: save: %w", err)
	}

	result.Index = index
	result.Documents = len(index.Documents)
	result.Added = len(index.Sources)
	result.Initialized = errors.Is(loadErr, os.ErrNotExist)
	result.Rebuilt = !result.Initialized
	result.Refreshed = true

	return result, nil
}

func clearWorkspaceIndexForNoSources(opts WorkspaceOptions, result WorkspaceRefreshResult) (WorkspaceRefreshResult, error) {
	existing, err := LoadIndex(opts.IndexPath)
	if err != nil {
		requiresCleanup := errors.Is(err, os.ErrNotExist) || workspaceIndexRequiresRebuild(err)
		if !requiresCleanup {
			return WorkspaceRefreshResult{}, fmt.Errorf("workspace vector index: load: %w", err)
		}

		removed, removeErr := removeWorkspaceIndexArtifacts(opts.Root, opts.IndexPath)
		if removeErr != nil {
			return WorkspaceRefreshResult{}, removeErr
		}

		result.Refreshed = removed

		return result, ErrNoSources
	}

	result.Deleted = len(existing.Sources)

	removed, removeErr := removeWorkspaceIndexArtifacts(opts.Root, opts.IndexPath)
	if removeErr != nil {
		return WorkspaceRefreshResult{}, removeErr
	}

	result.Refreshed = result.Deleted > 0 || len(existing.Documents) > 0 || removed

	return result, ErrNoSources
}

func removeWorkspaceIndexArtifacts(root, indexPath string) (bool, error) {
	removed := false

	for path := range workspaceIndexArtifactPaths(root, indexPath) {
		if !workspaceIndexArtifactMayBeRemoved(root, path) {
			continue
		}

		if err := os.Remove(path); err != nil {
			if errors.Is(err, os.ErrNotExist) {
				continue
			}

			return removed, fmt.Errorf("workspace vector index: remove empty workspace index artifact %s: %w", path, err)
		}

		removed = true
	}

	return removed, nil
}

func validateWorkspaceIndexPathMayBeRebuilt(root, indexPath string) error {
	if workspaceIndexPathManagedByAtteler(root, indexPath) || workspaceIndexFileLooksManaged(indexPath) {
		return nil
	}

	return fmt.Errorf("workspace vector index: refusing to overwrite existing non-index file %s", indexPath)
}

func workspaceIndexArtifactMayBeRemoved(root, path string) bool {
	if _, err := os.Stat(path); err != nil {
		return errors.Is(err, os.ErrNotExist)
	}

	return workspaceIndexPathManagedByAtteler(root, path) || workspaceIndexFileLooksManaged(path)
}

func workspaceIndexPathManagedByAtteler(root, path string) bool {
	rel, err := filepath.Rel(root, path)
	if err != nil || rel == "." || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return false
	}

	rel = filepath.ToSlash(filepath.Clean(rel))
	if rel == filepath.ToSlash(DefaultWorkspaceIndexPath) {
		return true
	}

	if !strings.HasPrefix(rel, ".atteler/") {
		return false
	}

	name := strings.ToLower(filepath.Base(rel))

	return strings.HasSuffix(name, ".json") &&
		strings.Contains(name, "index") &&
		(strings.Contains(name, "workspace") || strings.Contains(name, "vector"))
}

func workspaceIndexFileLooksManaged(path string) bool {
	data, err := os.ReadFile(path)
	if err != nil {
		return errors.Is(err, os.ErrNotExist)
	}

	var fields map[string]json.RawMessage
	if err := json.Unmarshal(data, &fields); err != nil {
		return false
	}

	var version int
	if err := json.Unmarshal(fields["version"], &version); err != nil || version <= 0 {
		return false
	}

	return workspaceIndexJSONFieldIsObject(fields["vectorizer"]) &&
		workspaceIndexJSONFieldIsObject(fields["chunk"]) &&
		workspaceIndexJSONFieldIsArray(fields["sources"]) &&
		workspaceIndexJSONFieldIsArray(fields["documents"])
}

func workspaceIndexJSONFieldIsObject(raw json.RawMessage) bool {
	var value map[string]json.RawMessage
	if err := json.Unmarshal(raw, &value); err != nil {
		return false
	}

	return value != nil
}

func workspaceIndexJSONFieldIsArray(raw json.RawMessage) bool {
	var value []json.RawMessage

	return json.Unmarshal(raw, &value) == nil
}

func refreshReusableWorkspaceIndex(
	ctx context.Context,
	existing *Index,
	sources []Source,
	opts WorkspaceOptions,
	result WorkspaceRefreshResult,
) (WorkspaceRefreshResult, bool, error) {
	index, changed, err := refreshExistingWorkspaceIndex(ctx, existing, sources, opts, &result)
	if err != nil {
		if workspaceIndexRequiresRebuild(err) {
			return result, false, nil
		}

		return WorkspaceRefreshResult{}, false, err
	}

	result.Index = index
	result.Documents = len(index.Documents)
	result.Refreshed = changed

	if !changed {
		if err := tightenWorkspaceIndexPermissions(opts.IndexPath); err != nil {
			return WorkspaceRefreshResult{}, false, err
		}

		return result, true, nil
	}

	if saveErr := index.Save(opts.IndexPath); saveErr != nil {
		return WorkspaceRefreshResult{}, false, fmt.Errorf("workspace vector index: save: %w", saveErr)
	}

	return result, true, nil
}

func tightenWorkspaceIndexPermissions(indexPath string) error {
	if err := os.Chmod(indexPath, 0o600); err != nil {
		return fmt.Errorf("workspace vector index: chmod reusable index %s: %w", indexPath, err)
	}

	return nil
}

// DiscoverWorkspaceSources walks Root and returns supported UTF-8 text/code
// sources that are not ignored by default rules, .gitignore, .attelerignore, or
// caller-provided include/exclude patterns.
func DiscoverWorkspaceSources(ctx context.Context, opts WorkspaceOptions) ([]Source, []WorkspaceSkip, error) {
	if ctx == nil {
		return nil, nil, ErrContextRequired
	}

	if err := ctx.Err(); err != nil {
		return nil, nil, fmt.Errorf("workspace vector index: discover canceled: %w", err)
	}

	opts, err := normalizeWorkspaceOptions(opts)
	if err != nil {
		return nil, nil, err
	}

	walker := workspaceWalker{
		opts:              opts,
		root:              opts.Root,
		indexArtifactsAbs: workspaceIndexArtifactPaths(opts.Root, opts.IndexPath),
	}

	if err := walker.walk(ctx, opts.Root, "", defaultWorkspaceIgnoreRules()); err != nil {
		return nil, nil, err
	}

	sort.SliceStable(walker.sources, func(i, j int) bool {
		return walker.sources[i].Path < walker.sources[j].Path
	})

	filteredSources, collisionSkips := workspaceSourcesWithUniquePersistedPaths(walker.sources)
	walker.sources = filteredSources
	walker.skipped = append(walker.skipped, collisionSkips...)

	return walker.sources, walker.skipped, nil
}

//nolint:cyclop // Incremental refresh must account for add/update/delete/retain counters explicitly.
func refreshExistingWorkspaceIndex(
	ctx context.Context,
	existing *Index,
	sources []Source,
	opts WorkspaceOptions,
	result *WorkspaceRefreshResult,
) (*Index, bool, error) {
	if err := validateReusableWorkspaceRefreshIndex(existing, opts); err != nil {
		return nil, false, err
	}

	currentMeta := make([]SourceMetadata, 0, len(sources))
	currentByPath := make(map[string]Source, len(sources))

	for _, source := range sources {
		meta := sourceMetadataForSource(source)
		currentMeta = append(currentMeta, meta)
		currentByPath[meta.Path] = source
	}

	existingMeta := make(map[string]SourceMetadata, len(existing.Sources))
	for _, meta := range existing.Sources {
		existingMeta[filepath.Clean(meta.Path)] = meta
	}

	documentsByPath := indexDocumentsByPath(existing.Documents)
	changedSources := make([]Source, 0)
	retainedDocuments := make([]Document, 0, len(existing.Documents))
	retainedSources := make([]SourceMetadata, 0, len(sources))
	changed := false

	for _, meta := range currentMeta {
		previous, ok := existingMeta[meta.Path]
		switch {
		case !ok:
			result.Added++
			changed = true

			changedSources = append(changedSources, currentByPath[meta.Path])
		case previous.Digest != meta.Digest:
			result.Updated++
			changed = true

			changedSources = append(changedSources, currentByPath[meta.Path])
		default:
			docs := documentsByPath[meta.Path]
			if indexDocumentsNeedRefresh(docs, existing.Vectorizer, currentByPath[meta.Path], opts.Chunk) {
				result.Updated++
				changed = true

				changedSources = append(changedSources, currentByPath[meta.Path])

				continue
			}

			retainedSources = append(retainedSources, meta)
			docs, metadataChanged := retainWorkspaceSourceDocuments(
				result,
				docs,
				currentByPath[meta.Path],
				opts.Chunk,
			)
			changed = changed || metadataChanged

			retainedDocuments = append(retainedDocuments, docs...)
		}
	}

	changed = markDeletedWorkspaceSources(result, existing.Sources, currentByPath) || changed

	if !changed {
		return existing, false, nil
	}

	refreshedAt := workspaceNow(opts)

	index := &Index{
		Version:    IndexVersion,
		CreatedAt:  existing.CreatedAt,
		UpdatedAt:  refreshedAt,
		Vectorizer: existing.Vectorizer.Normalize(),
		Chunk:      opts.Chunk.Normalize(),
		Dimensions: existing.Dimensions,
		Sources:    retainedSources,
		Documents:  retainedDocuments,
	}
	if index.CreatedAt.IsZero() {
		index.CreatedAt = refreshedAt
	}

	if len(retainedDocuments) == 0 {
		index.Dimensions = 0
		index.Vectorizer = opts.VectorizerMetadata.Normalize()
	}

	for _, source := range changedSources {
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

func markDeletedWorkspaceSources(
	result *WorkspaceRefreshResult,
	sources []SourceMetadata,
	currentByPath map[string]Source,
) bool {
	changed := false

	for _, meta := range sources {
		if _, ok := currentByPath[filepath.Clean(meta.Path)]; !ok {
			result.Deleted++
			changed = true
		}
	}

	return changed
}

func retainWorkspaceSourceDocuments(
	result *WorkspaceRefreshResult,
	docs []Document,
	source Source,
	chunk ChunkOptions,
) ([]Document, bool) {
	result.Unchanged++

	retained, metadataChanged := refreshRetainedDocumentFreshnessMetadata(docs, source, chunk)
	if !metadataChanged {
		return retained, false
	}

	result.Updated++
	result.Unchanged--

	return retained, true
}

func loadWorkspaceRefreshIndex(path string) (*Index, error) {
	return loadIndex(path, refreshIndexValidationOptions())
}

func validateReusableWorkspaceRefreshIndex(existing *Index, opts WorkspaceOptions) error {
	if err := existing.validateFor(opts.VectorizerMetadata, nil, refreshIndexValidationOptions(), opts.Chunk); err != nil {
		return err
	}

	return validateIndexSourceCoverage(existing)
}

func validateIndexSourceCoverage(idx *Index) error {
	sources := make(map[string]struct{}, len(idx.Sources))
	for _, source := range idx.Sources {
		sources[filepath.Clean(source.Path)] = struct{}{}
	}

	documentCounts := make(map[string]int, len(sources))

	for i := range idx.Documents {
		doc := &idx.Documents[i]

		path := filepath.Clean(strings.TrimSpace(doc.Metadata["path"]))
		if path == "" || path == "." {
			return fmt.Errorf("%w: document %q is missing source path metadata", ErrSourceStale, doc.ID)
		}

		if _, ok := sources[path]; !ok {
			return fmt.Errorf("%w: document %q references unindexed source %q", ErrSourceStale, doc.ID, path)
		}

		documentCounts[path]++
	}

	for path := range sources {
		if documentCounts[path] == 0 {
			return fmt.Errorf("%w: source %q has no indexed documents", ErrSourceStale, path)
		}
	}

	return nil
}

func refreshIndexValidationOptions() indexValidationOptions {
	return indexValidationOptions{AllowStaleTextHashVector: true}
}

var workspaceIndexRebuildErrors = [...]error{
	ErrMetadataMismatch,
	ErrDimensionMismatch,
	ErrInvalidDimensions,
	ErrMissingID,
	ErrDuplicateID,
	ErrEmptyVector,
	ErrZeroVector,
	ErrInvalidVector,
	ErrVectorizerMismatch,
	ErrVectorMismatch,
	ErrSourceHashMismatch,
	ErrProvenanceMissing,
	ErrSourceStale,
	ErrPrivacyPolicy,
	ErrIndexCorrupt,
	ErrEmptyText,
}

func workspaceIndexRequiresRebuild(err error) bool {
	for _, rebuildErr := range workspaceIndexRebuildErrors {
		if errors.Is(err, rebuildErr) {
			return true
		}
	}

	return false
}

func normalizeWorkspaceOptions(opts WorkspaceOptions) (WorkspaceOptions, error) {
	root := strings.TrimSpace(opts.Root)
	if root == "" {
		root = "."
	}

	absRoot, err := filepath.Abs(root)
	if err != nil {
		return WorkspaceOptions{}, fmt.Errorf("workspace vector index: resolve root: %w", err)
	}

	info, err := os.Stat(absRoot)
	if err != nil {
		return WorkspaceOptions{}, fmt.Errorf("workspace vector index: stat root: %w", err)
	}

	if !info.IsDir() {
		return WorkspaceOptions{}, fmt.Errorf("workspace vector index: root %s is not a directory", absRoot)
	}

	if opts.Vectorizer == nil {
		vectorizer, vectorizerErr := NewTextVectorizer(0)
		if vectorizerErr != nil {
			return WorkspaceOptions{}, vectorizerErr
		}

		opts.Vectorizer = vectorizer
		opts.VectorizerMetadata = vectorizer.Metadata()
	}

	opts.VectorizerMetadata = normalizeVectorizerMetadataForIndex(opts.Vectorizer, opts.VectorizerMetadata)

	if opts.VectorizerMetadata.Kind == "" {
		return WorkspaceOptions{}, fmt.Errorf("%w: workspace vectorizer metadata is required", ErrMetadataMismatch)
	}

	opts.Root = filepath.Clean(absRoot)

	opts.Chunk = opts.Chunk.Normalize()

	opts.IndexPath, err = normalizeWorkspaceIndexPath(opts.Root, opts.IndexPath)
	if err != nil {
		return WorkspaceOptions{}, err
	}

	if opts.MaxFileBytes <= 0 {
		opts.MaxFileBytes = DefaultWorkspaceMaxFileBytes
	}

	if opts.MaxFiles <= 0 {
		opts.MaxFiles = DefaultWorkspaceMaxFiles
	}

	return opts, nil
}

func normalizeWorkspaceIndexPath(root, indexPath string) (string, error) {
	indexPath = strings.TrimSpace(indexPath)
	if indexPath == "" {
		indexPath = filepath.Join(root, DefaultWorkspaceIndexPath)
	} else if !filepath.IsAbs(indexPath) {
		indexPath = filepath.Join(root, indexPath)
	}

	indexPath = filepath.Clean(indexPath)
	if err := validateWorkspaceIndexPath(root, indexPath); err != nil {
		return "", err
	}

	return indexPath, nil
}

func validateWorkspaceIndexPath(root, indexPath string) error {
	rel, err := filepath.Rel(root, indexPath)
	if err != nil || rel == "." || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return fmt.Errorf("workspace vector index: index path %s must be inside workspace root %s", indexPath, root)
	}

	if err := validateWorkspaceIndexPathNoSymlinkEscape(root, rel, indexPath); err != nil {
		return err
	}

	return nil
}

func validateWorkspaceIndexPathNoSymlinkEscape(root, rel, indexPath string) error {
	current := root

	for part := range strings.SplitSeq(filepath.Clean(rel), string(filepath.Separator)) {
		if part == "" || part == "." {
			continue
		}

		current = filepath.Join(current, part)

		info, err := os.Lstat(current)
		if err != nil {
			if errors.Is(err, os.ErrNotExist) {
				return nil
			}

			return fmt.Errorf("workspace vector index: inspect index path %s: %w", current, err)
		}

		if info.Mode()&os.ModeSymlink != 0 {
			return fmt.Errorf("workspace vector index: index path %s must not traverse symlink %s", indexPath, current)
		}
	}

	return nil
}

//nolint:govet // Internal walker keeps immutable options before accumulated state.
type workspaceWalker struct {
	opts              WorkspaceOptions
	root              string
	indexArtifactsAbs map[string]struct{}
	sources           []Source
	skipped           []WorkspaceSkip
}

func (w *workspaceWalker) walk(ctx context.Context, dir, relDir string, inherited []workspaceIgnoreRule) error {
	if err := ctx.Err(); err != nil {
		return fmt.Errorf("workspace vector index: walk canceled: %w", err)
	}

	rules := append([]workspaceIgnoreRule(nil), inherited...)
	rules = append(rules, readWorkspaceIgnoreRules(filepath.Join(dir, ".gitignore"), relDir)...)
	rules = append(rules, readWorkspaceIgnoreRules(filepath.Join(dir, ".attelerignore"), relDir)...)

	entries, err := os.ReadDir(dir)
	if err != nil {
		if relDir != "" {
			w.skip(relDir, "read dir failed")

			return nil
		}

		return fmt.Errorf("workspace vector index: read dir %s: %w", dir, err)
	}

	for _, entry := range entries {
		name := entry.Name()
		rel := filepath.ToSlash(filepath.Join(relDir, name))
		abs := filepath.Join(dir, name)
		isDir := entry.IsDir()

		if entry.Type()&os.ModeSymlink != 0 {
			w.skip(rel, "symlink")
			continue
		}

		if w.ignored(rel, isDir, rules) {
			w.skip(rel, "ignored")
			continue
		}

		if len(w.sources) >= w.opts.MaxFiles {
			w.skip(rel, "workspace file limit reached")
			continue
		}

		if isDir {
			if err := w.walk(ctx, abs, rel, rules); err != nil {
				return err
			}

			continue
		}

		source, ok := w.sourceFromFile(abs, rel, entry)
		if ok {
			w.sources = append(w.sources, source)
		}
	}

	return nil
}

func (w *workspaceWalker) ignored(rel string, isDir bool, rules []workspaceIgnoreRule) bool {
	rel = filepath.ToSlash(rel)

	abs := absoluteWorkspacePath(w.root, rel)
	if _, ok := w.indexArtifactsAbs[abs]; ok {
		return true
	}

	if matchesDefaultWorkspaceIgnore(rel, isDir) {
		return true
	}

	if len(w.opts.IncludePatterns) > 0 && !matchesAnyWorkspaceIncludePattern(rel, isDir, w.opts.IncludePatterns) {
		return true
	}

	ignored := false

	for _, rule := range rules {
		if rule.matches(rel, isDir) {
			ignored = !rule.negated
		}
	}

	if matchesAnyWorkspacePattern(rel, isDir, w.opts.ExcludePatterns) {
		ignored = true
	}

	return ignored
}

func (w *workspaceWalker) sourceFromFile(abs, rel string, entry fs.DirEntry) (Source, bool) {
	if !workspaceFileSupported(rel) {
		w.skip(rel, "unsupported file type")
		return Source{}, false
	}

	info, err := entry.Info()
	if err != nil {
		w.skip(rel, "stat failed")
		return Source{}, false
	}

	if !info.Mode().IsRegular() {
		w.skip(rel, "non-regular file")
		return Source{}, false
	}

	if w.opts.MaxFileBytes > 0 && info.Size() > w.opts.MaxFileBytes {
		w.skip(rel, "too large")
		return Source{}, false
	}

	data, tooLarge, err := readWorkspaceFileWithinLimit(abs, w.opts.MaxFileBytes)
	if err != nil {
		w.skip(rel, "read failed")
		return Source{}, false
	}

	if tooLarge {
		w.skip(rel, "too large")
		return Source{}, false
	}

	if len(data) == 0 {
		w.skip(rel, "empty")
		return Source{}, false
	}

	if !utf8.Valid(data) || hasNUL(data) {
		w.skip(rel, "binary")
		return Source{}, false
	}

	text := string(data)
	if strings.TrimSpace(text) == "" {
		w.skip(rel, "empty")
		return Source{}, false
	}

	if w.opts.VectorizerMetadata.Normalize().Kind == VectorizerKindLexical && len(tokenize(text)) == 0 {
		w.skip(rel, "no indexable text")
		return Source{}, false
	}

	metadata := make(map[string]string, 1)
	if updatedAt := info.ModTime().UTC(); !updatedAt.IsZero() {
		metadata[retrieval.MetadataSourceUpdatedAt] = updatedAt.Format(time.RFC3339Nano)
	}

	return Source{Path: rel, Text: text, Metadata: metadata}, true
}

func readWorkspaceFileWithinLimit(path string, maxBytes int64) (data []byte, tooLarge bool, err error) {
	if maxBytes <= 0 {
		data, err = os.ReadFile(path)
		if err != nil {
			return nil, false, fmt.Errorf("read workspace file %s: %w", path, err)
		}

		return data, false, nil
	}

	file, err := os.Open(path)
	if err != nil {
		return nil, false, fmt.Errorf("open workspace file %s: %w", path, err)
	}
	defer file.Close()

	data, err = io.ReadAll(io.LimitReader(file, maxBytes+1))
	if err != nil {
		return nil, false, fmt.Errorf("read workspace file %s: %w", path, err)
	}

	if int64(len(data)) > maxBytes {
		return nil, true, nil
	}

	return data, false, nil
}

func (w *workspaceWalker) skip(path, reason string) {
	w.skipped = append(w.skipped, WorkspaceSkip{Path: redactWorkspaceRelativePath(path), Reason: reason})
}

func workspaceSourcesWithUniquePersistedPaths(sources []Source) ([]Source, []WorkspaceSkip) {
	if len(sources) == 0 {
		return nil, nil
	}

	seen := make(map[string]struct{}, len(sources))
	out := make([]Source, 0, len(sources))
	skipped := make([]WorkspaceSkip, 0)

	for _, source := range sources {
		persistedPath := redactSourcePath(source.Path)
		if _, ok := seen[persistedPath]; ok {
			skipped = append(skipped, WorkspaceSkip{
				Path:   redactWorkspaceRelativePath(source.Path),
				Reason: "redacted path collision",
			})

			continue
		}

		seen[persistedPath] = struct{}{}

		out = append(out, source)
	}

	return out, skipped
}

func redactWorkspaceRelativePath(path string) string {
	path = redactSourcePath(path)
	if path == "" {
		return ""
	}

	return filepath.ToSlash(path)
}

type workspaceIgnoreRule struct {
	base     string
	pattern  string
	negated  bool
	dirOnly  bool
	anchored bool
}

func defaultWorkspaceIgnoreRules() []workspaceIgnoreRule {
	patterns := []string{
		".git/",
		".atteler/",
		"node_modules/",
		"vendor/",
		"dist/",
		"build/",
		"target/",
		"coverage/",
		"generated/",
		"gen/",
		"out/",
		".aws/",
		".cache/",
		".docker/",
		".gnupg/",
		".kube/",
		".next/",
		".nuxt/",
		".mypy_cache/",
		".parcel-cache/",
		".pytest_cache/",
		".ruff_cache/",
		".ssh/",
		".turbo/",
		".venv/",
		"venv/",
		"__pycache__/",
		"*.generated.*",
		"*.gen.*",
		"*.pb.go",
		"*.min.js",
		"*.map",
		"*.lock",
		"*.lockb",
		"package-lock.json",
		"npm-shrinkwrap.json",
		"pnpm-lock.yaml",
		"go.sum",
		".env",
		".env.*",
		".envrc",
		"*secret*",
		"*credential*",
		"*token*",
		"*password*",
		"*passwd*",
		"*api_key*",
		"*apikey*",
		"*authorization*",
		"id_rsa*",
		"id_dsa*",
		"id_ecdsa*",
		"id_ed25519*",
		"*.pem",
		"*.key",
	}

	rules := make([]workspaceIgnoreRule, 0, len(patterns))
	for _, pattern := range patterns {
		rules = append(rules, parseWorkspaceIgnoreRule(pattern, ""))
	}

	return rules
}

func matchesDefaultWorkspaceIgnore(rel string, isDir bool) bool {
	rules := defaultWorkspaceIgnoreRules()
	if matchesAnyWorkspaceRule(rel, isDir, rules) {
		return true
	}

	return matchesAnyWorkspaceRule(strings.ToLower(rel), isDir, rules)
}

func readWorkspaceIgnoreRules(path, base string) []workspaceIgnoreRule {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil
	}

	var rules []workspaceIgnoreRule

	for line := range strings.SplitSeq(string(data), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}

		rules = append(rules, parseWorkspaceIgnoreRule(line, base))
	}

	return rules
}

func parseWorkspaceIgnoreRule(pattern, base string) workspaceIgnoreRule {
	pattern = strings.TrimSpace(pattern)
	rule := workspaceIgnoreRule{base: filepath.ToSlash(strings.Trim(base, "/"))}

	if strings.HasPrefix(pattern, "!") {
		rule.negated = true
		pattern = strings.TrimSpace(strings.TrimPrefix(pattern, "!"))
	}

	pattern = filepath.ToSlash(pattern)
	if strings.HasPrefix(pattern, "/") {
		rule.anchored = true
	}

	if strings.HasSuffix(pattern, "/") {
		rule.dirOnly = true
		pattern = strings.TrimSuffix(pattern, "/")
	}

	rule.pattern = strings.Trim(pattern, "/")

	return rule
}

func (r workspaceIgnoreRule) matches(rel string, isDir bool) bool {
	if r.pattern == "" {
		return false
	}

	rel = filepath.ToSlash(rel)

	local := rel
	if r.base != "" {
		if rel != r.base && !strings.HasPrefix(rel, r.base+"/") {
			return false
		}

		local = strings.TrimPrefix(rel, r.base+"/")
		if local == rel {
			local = ""
		}
	}

	if r.dirOnly && !isDir {
		return false
	}

	if r.anchored {
		return r.matchesAnchored(local, isDir)
	}

	if !strings.Contains(r.pattern, "/") {
		for part := range strings.SplitSeq(local, "/") {
			if workspacePatternMatch(r.pattern, part) {
				return true
			}
		}

		return false
	}

	if workspacePatternMatch(r.pattern, local) {
		return true
	}

	return isDir && strings.HasPrefix(local, r.pattern+"/")
}

func (r workspaceIgnoreRule) matchesAnchored(local string, isDir bool) bool {
	local = strings.Trim(local, "/")
	if local == "" {
		return false
	}

	if workspacePatternMatch(r.pattern, local) {
		return true
	}

	return isDir && strings.HasPrefix(local, r.pattern+"/")
}

func (r workspaceIgnoreRule) matchesDescendant(rel string) bool {
	if !r.dirOnly || r.pattern == "" {
		return false
	}

	rel = filepath.ToSlash(rel)

	local := rel
	if r.base != "" {
		if rel != r.base && !strings.HasPrefix(rel, r.base+"/") {
			return false
		}

		local = strings.TrimPrefix(rel, r.base+"/")
		if local == rel {
			local = ""
		}
	}

	if !strings.Contains(r.pattern, "/") {
		parts := strings.Split(local, "/")
		for i, part := range parts {
			if r.anchored && i > 0 {
				return false
			}

			if workspacePatternMatch(r.pattern, part) && i < len(parts)-1 {
				return true
			}
		}

		return false
	}

	return strings.HasPrefix(local, r.pattern+"/")
}

func (r workspaceIgnoreRule) mayMatchIncludedDescendant(rel string) bool {
	if r.pattern == "" {
		return false
	}

	rel = filepath.ToSlash(strings.Trim(rel, "/"))
	if rel == "" {
		return true
	}

	if !strings.Contains(r.pattern, "/") {
		return !r.anchored
	}

	patternParts := strings.Split(r.pattern, "/")

	relParts := strings.Split(rel, "/")
	if len(relParts) > len(patternParts) {
		return false
	}

	for i, relPart := range relParts {
		patternPart := patternParts[i]
		if patternPart == "**" {
			return true
		}

		if !workspacePathPartMatch(patternPart, relPart) {
			return false
		}
	}

	return true
}

func matchesAnyWorkspacePattern(rel string, isDir bool, patterns []string) bool {
	for _, pattern := range patterns {
		if parseWorkspaceIgnoreRule(pattern, "").matches(rel, isDir) {
			return true
		}
	}

	return false
}

func matchesAnyWorkspaceIncludePattern(rel string, isDir bool, patterns []string) bool {
	for _, pattern := range patterns {
		rule := parseWorkspaceIgnoreRule(pattern, "")
		if rule.matches(rel, isDir) ||
			rule.matchesDescendant(rel) ||
			(isDir && rule.mayMatchIncludedDescendant(rel)) {
			return true
		}
	}

	return false
}

func matchesAnyWorkspaceRule(rel string, isDir bool, rules []workspaceIgnoreRule) bool {
	for _, rule := range rules {
		if rule.matches(rel, isDir) {
			return true
		}
	}

	return false
}

func workspacePatternMatch(pattern, value string) bool {
	pattern = filepath.ToSlash(pattern)
	value = filepath.ToSlash(value)

	if strings.Contains(pattern, "**") {
		return workspaceDoubleStarPatternMatch(pattern, value)
	}

	matched, err := filepath.Match(pattern, value)
	if err == nil && matched {
		return true
	}

	return pattern == value
}

func workspaceDoubleStarPatternMatch(pattern, value string) bool {
	patternParts := strings.Split(strings.Trim(pattern, "/"), "/")
	valueParts := strings.Split(strings.Trim(value, "/"), "/")

	return workspaceDoubleStarPartsMatch(patternParts, valueParts)
}

func workspaceDoubleStarPartsMatch(patternParts, valueParts []string) bool {
	if len(patternParts) == 0 {
		return len(valueParts) == 0
	}

	if patternParts[0] == "**" {
		if len(patternParts) == 1 {
			return true
		}

		for i := range len(valueParts) + 1 {
			if workspaceDoubleStarPartsMatch(patternParts[1:], valueParts[i:]) {
				return true
			}
		}

		return false
	}

	if len(valueParts) == 0 {
		return false
	}

	if !workspacePathPartMatch(patternParts[0], valueParts[0]) {
		return false
	}

	return workspaceDoubleStarPartsMatch(patternParts[1:], valueParts[1:])
}

func workspacePathPartMatch(pattern, value string) bool {
	matched, err := filepath.Match(pattern, value)
	if err == nil && matched {
		return true
	}

	return pattern == value
}

func workspaceFileSupported(path string) bool {
	name := strings.ToLower(filepath.Base(path))
	switch name {
	case "dockerfile", "makefile", "license", "notice", "readme", "changelog", "go.mod", "go.sum":
		return true
	}

	ext := strings.ToLower(filepath.Ext(path))
	supported := map[string]struct{}{
		".c": {}, ".cc": {}, ".cfg": {}, ".conf": {}, ".cpp": {}, ".cs": {},
		".css": {}, ".csv": {}, ".dockerfile": {}, ".go": {},
		".graphql": {}, ".h": {}, ".hpp": {}, ".html": {}, ".java": {},
		".js": {}, ".json": {}, ".jsx": {}, ".kt": {}, ".lua": {}, ".md": {},
		".mdx": {}, ".php": {}, ".proto": {}, ".py": {}, ".rb": {}, ".rs": {},
		".sh": {}, ".sql": {}, ".svg": {}, ".swift": {}, ".toml": {}, ".ts": {},
		".tsx": {}, ".txt": {}, ".xml": {}, ".yaml": {}, ".yml": {},
	}

	_, ok := supported[ext]

	return ok
}

func indexDocumentsByPath(docs []Document) map[string][]Document {
	out := make(map[string][]Document)

	for i := range docs {
		doc := docs[i]

		path := filepath.Clean(doc.Metadata["path"])
		if path == "." || path == "" {
			continue
		}

		out[path] = append(out[path], cloneDocument(doc))
	}

	return out
}

func indexDocumentsNeedRefresh(docs []Document, vectorizer VectorizerMetadata, source Source, chunk ChunkOptions) bool {
	return len(docs) == 0 ||
		!indexDocumentsReusable(docs, vectorizer) ||
		!indexDocumentsMatchSource(docs, source, chunk)
}

func indexDocumentsReusable(docs []Document, vectorizer VectorizerMetadata) bool {
	vectorizer = vectorizer.Normalize()

	for i := range docs {
		doc := docs[i]
		if strings.TrimSpace(doc.Text) == "" {
			return false
		}

		if err := checkDocumentPrivacy(doc); err != nil {
			return false
		}

		if err := checkDocumentProvenance(doc); err != nil {
			return false
		}

		if err := checkDocumentSourceHash(doc); err != nil {
			return false
		}

		if vectorizer.Kind == VectorizerKindLexical && !workspaceLexicalVectorMatchesText(doc) {
			return false
		}
	}

	return true
}

func workspaceLexicalVectorMatchesText(doc Document) bool {
	vectorizer, err := NewTextVectorizer(len(doc.Vector))
	if err != nil {
		return false
	}

	expected, err := vectorizer.Vectorize(doc.Text)
	if err != nil {
		return false
	}

	return vectorsEqual(doc.Vector, expected)
}

func indexDocumentsMatchSource(docs []Document, source Source, chunk ChunkOptions) bool {
	sourceMetadata := sourceMetadataForSource(source)
	if sourceMetadata.Path == "" {
		return false
	}

	chunks, err := ChunkText(sourceMetadata.Path, privacy.RedactText(source.Text), chunk)
	if err != nil || len(chunks) != len(docs) {
		return false
	}

	docsByID := make(map[string]Document, len(docs))
	for i := range docs {
		doc := cloneDocument(docs[i])
		if doc.ID == "" {
			return false
		}

		if _, ok := docsByID[doc.ID]; ok {
			return false
		}

		docsByID[doc.ID] = doc
	}

	for _, chunk := range chunks {
		doc, ok := docsByID[chunk.ID]
		if !ok || !indexDocumentMatchesChunk(doc, source, sourceMetadata, chunk) {
			return false
		}
	}

	return true
}

func indexDocumentMatchesChunk(doc Document, source Source, sourceMetadata SourceMetadata, chunk Chunk) bool {
	expectedText, expectedMetadata := chunkDocumentPayload(source, sourceMetadata.Path, sourceMetadata, chunk)
	if doc.Text != expectedText {
		return false
	}

	if doc.SourceHash != sourceHash(expectedText) {
		return false
	}

	if !indexDocumentMetadataMatches(doc.Metadata, expectedMetadata) {
		return false
	}

	expectedProvenance := sourceProvenance(source, normalizeSourceKind(source.Kind), sourceMetadata.Path)

	return maps.Equal(doc.Provenance, expectedProvenance)
}

func indexDocumentMetadataMatches(actual, expected map[string]string) bool {
	actual = maps.Clone(actual)
	expected = maps.Clone(expected)

	delete(actual, retrieval.MetadataSourceUpdatedAt)
	delete(expected, retrieval.MetadataSourceUpdatedAt)

	return maps.Equal(actual, expected)
}

func refreshRetainedDocumentFreshnessMetadata(
	docs []Document,
	source Source,
	chunk ChunkOptions,
) ([]Document, bool) {
	retained := cloneDocuments(docs)

	sourceMetadata := sourceMetadataForSource(source)
	if sourceMetadata.Path == "" {
		return retained, false
	}

	chunks, err := ChunkText(sourceMetadata.Path, privacy.RedactText(source.Text), chunk)
	if err != nil {
		return retained, false
	}

	freshnessByChunkID := make(map[string]string, len(chunks))
	for _, chunk := range chunks {
		_, metadata := chunkDocumentPayload(source, sourceMetadata.Path, sourceMetadata, chunk)
		freshnessByChunkID[chunk.ID] = strings.TrimSpace(metadata[retrieval.MetadataSourceUpdatedAt])
	}

	changed := false

	for i := range retained {
		freshness, ok := freshnessByChunkID[retained[i].ID]
		if !ok || strings.TrimSpace(retained[i].Metadata[retrieval.MetadataSourceUpdatedAt]) == freshness {
			continue
		}

		if freshness == "" {
			delete(retained[i].Metadata, retrieval.MetadataSourceUpdatedAt)

			changed = true

			continue
		}

		if retained[i].Metadata == nil {
			retained[i].Metadata = make(map[string]string, 1)
		}

		retained[i].Metadata[retrieval.MetadataSourceUpdatedAt] = freshness
		changed = true
	}

	return retained, changed
}

func hasNUL(data []byte) bool {
	return slices.Contains(data, 0)
}

func absoluteWorkspacePath(root, path string) string {
	if strings.TrimSpace(path) == "" {
		return ""
	}

	if filepath.IsAbs(path) {
		return filepath.Clean(path)
	}

	return filepath.Clean(filepath.Join(root, path))
}

func workspaceIndexArtifactPaths(root, indexPath string) map[string]struct{} {
	primary := absoluteWorkspacePath(root, indexPath)
	if primary == "" {
		return nil
	}

	paths := map[string]struct{}{primary: {}}
	if original := workspacePrimaryIndexPathFromLexicalFallback(primary); original != primary {
		paths[original] = struct{}{}
	}

	if fallback := workspaceLexicalFallbackIndexPath(primary); fallback != primary {
		paths[fallback] = struct{}{}
	}

	return paths
}

func workspacePrimaryIndexPathFromLexicalFallback(indexPath string) string {
	extension := filepath.Ext(indexPath)
	if extension != "" {
		stem := strings.TrimSuffix(indexPath, extension)
		if primaryStem, ok := strings.CutSuffix(stem, ".lexical"); ok {
			return primaryStem + extension
		}
	}

	if primary, ok := strings.CutSuffix(indexPath, ".lexical"); ok {
		return primary
	}

	return indexPath
}

func workspaceLexicalFallbackIndexPath(indexPath string) string {
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

func workspaceNow(opts WorkspaceOptions) time.Time {
	if opts.Now != nil {
		return opts.Now().UTC()
	}

	return time.Now().UTC()
}
