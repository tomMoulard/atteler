// Package codeintel provides Go code intelligence primitives backed by package
// loading, type information, and provenance-aware graph metadata.
//
//nolint:govet,wsl_v5 // Public metadata structs prioritize stable/readable field order; wsl is noisy for evidence-building code.
package codeintel

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io/fs"
	"maps"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/tommoulard/atteler/pkg/codegraph"
)

// Symbol describes a named declaration in a Go source file.
//
// Symbol intentionally keeps the legacy small shape used by existing CLI
// summaries. Rich definition metadata lives in Declaration.
type Symbol struct {
	Name string
	Kind string
	File string
	Line int
}

// File summarizes one parsed Go source file.
//
// File intentionally keeps the legacy summary shape used by existing CLI
// commands. Rich file metadata lives in SourceFile.
type File struct {
	Path    string
	Package string
	Imports []string
	Symbols []Symbol
}

// ImportEdge describes one file-level import relationship.
//
// ImportEdge intentionally keeps the legacy small shape used by existing import
// graph commands. Rich import metadata lives in Import.
type ImportEdge struct {
	From   string
	Import string
}

const (
	kindFunc    = "func"
	kindMethod  = "method"
	kindBuiltin = "builtin"
)

// SourceRange identifies a source span in a file.
type SourceRange struct {
	File        string
	StartLine   int
	StartColumn int
	EndLine     int
	EndColumn   int
}

// BuildContext records the build settings that made a file or edge visible.
type BuildContext struct {
	GOOS        string
	GOARCH      string
	Tags        []string
	Test        bool
	Generated   bool
	PackageID   string
	PackagePath string
	ModulePath  string
}

// String returns a stable compact build-context description for graph evidence.
func (ctx BuildContext) String() string {
	parts := []string{
		"goos=" + ctx.GOOS,
		"goarch=" + ctx.GOARCH,
		"test=" + strconv.FormatBool(ctx.Test),
		"generated=" + strconv.FormatBool(ctx.Generated),
	}
	if len(ctx.Tags) > 0 {
		parts = append(parts, "tags="+strings.Join(ctx.Tags, ","))
	}
	if ctx.PackagePath != "" {
		parts = append(parts, "package="+ctx.PackagePath)
	}
	if ctx.PackageID != "" && ctx.PackageID != ctx.PackagePath {
		parts = append(parts, "package_id="+ctx.PackageID)
	}
	if ctx.ModulePath != "" {
		parts = append(parts, "module="+ctx.ModulePath)
	}

	return strings.Join(parts, " ")
}

// Provenance records why an index item exists.
type Provenance struct {
	Source     string
	Range      SourceRange
	Build      BuildContext
	Confidence string
}

// Diagnostic records package loader errors that did not prevent a partial index.
type Diagnostic struct {
	PackageID string
	Position  string
	Message   string
	Kind      string
}

// PackageInfo describes one loaded package or package test variant.
type PackageInfo struct {
	ID         string
	Name       string
	Path       string
	ModulePath string
	Files      []string
	Test       bool
}

// SourceFile describes one loaded source file with build and cache metadata.
type SourceFile struct {
	Path        string
	PackageName string
	PackageID   string
	PackagePath string
	ModulePath  string
	Generated   bool
	Test        bool
	BuildTags   []string
	Range       SourceRange
	ContentHash string
	ModTime     time.Time
	Build       BuildContext
	Provenance  Provenance
}

// Declaration describes a top-level API or implementation declaration.
type Declaration struct {
	ID          string
	Name        string
	Kind        string
	PackageName string
	PackageID   string
	PackagePath string
	Receiver    string
	File        string
	Range       SourceRange
	Exported    bool
	Build       BuildContext
	Provenance  Provenance
}

// Import describes one source import with evidence and build context.
type Import struct {
	ID                  string
	Path                string
	Alias               string
	File                string
	PackageID           string
	PackagePath         string
	ResolvedPackagePath string
	Range               SourceRange
	Build               BuildContext
	Provenance          Provenance
}

// Reference describes one type-resolved identifier reference.
type Reference struct {
	ID                string
	Name              string
	Kind              string
	File              string
	FromPackageID     string
	FromDeclarationID string
	ToDeclarationID   string
	Range             SourceRange
	Build             BuildContext
	Provenance        Provenance
}

// CallEdge describes one type-resolved call relationship between declarations.
type CallEdge struct {
	CallerID   string
	CalleeID   string
	File       string
	Range      SourceRange
	Build      BuildContext
	Provenance Provenance
}

// IndexStats describes cache behavior for an indexing run.
type IndexStats struct {
	CacheHit     bool
	FilesScanned int
	FilesReused  int
	FilesChanged int
}

// Index contains legacy summaries plus semantic code intelligence data.
type Index struct {
	Files       []File
	Symbols     []Symbol
	ImportEdges []ImportEdge

	Packages     []PackageInfo
	FileDetails  []SourceFile
	Declarations []Declaration
	Imports      []Import
	References   []Reference
	CallEdges    []CallEdge
	Graph        *codegraph.EvidenceGraph
	Diagnostics  []Diagnostic
	Stats        IndexStats
}

// IndexOptions controls package-aware indexing.
type IndexOptions struct {
	// Tags are passed as -tags to the Go package loader.
	Tags []string
	// Env augments or overrides the process environment used by the package loader.
	Env []string
	// ExcludeTests omits _test.go files and package test variants.
	ExcludeTests bool
	// ExcludeGenerated omits files matching Go's generated-code convention.
	ExcludeGenerated bool
}

// Indexer caches whole-index results by file mtimes and content hashes so
// repeated indexing of unchanged workspaces avoids package reloads.
type Indexer struct {
	mu    sync.Mutex
	cache map[string]cachedIndex
}

type cachedIndex struct {
	fingerprint  string
	fingerprints map[string]fileFingerprint
	index        Index
}

type fileFingerprint struct {
	Path    string
	Hash    string
	ModTime time.Time
	Size    int64
}

// NewIndexer returns an incremental package-aware indexer.
func NewIndexer() *Indexer {
	return &Indexer{cache: make(map[string]cachedIndex)}
}

// IndexDir parses all active Go source files under root.
func IndexDir(root string) (Index, error) {
	return NewIndexer().IndexDir(root)
}

// IndexDirContext parses all active Go source files under root using ctx for
// package-loading cancellation.
func IndexDirContext(ctx context.Context, root string) (Index, error) {
	return NewIndexer().IndexDirContext(ctx, root)
}

// IndexDir parses all active Go source files under root using this indexer's cache.
func (idxr *Indexer) IndexDir(root string) (Index, error) {
	return idxr.IndexDirWithOptions(root, IndexOptions{})
}

// IndexDirContext parses all active Go source files under root using this
// indexer's cache and ctx for package-loading cancellation.
func (idxr *Indexer) IndexDirContext(ctx context.Context, root string) (Index, error) {
	return idxr.IndexDirWithOptionsContext(ctx, root, IndexOptions{})
}

// IndexDirWithOptions parses all active Go source files under root using options.
func (idxr *Indexer) IndexDirWithOptions(root string, opts IndexOptions) (Index, error) {
	return idxr.IndexDirWithOptionsContext(context.TODO(), root, opts)
}

// IndexDirWithOptionsContext parses all active Go source files under root using
// options and ctx for package-loading cancellation.
func (idxr *Indexer) IndexDirWithOptionsContext(ctx context.Context, root string, opts IndexOptions) (Index, error) {
	paths, err := goFilePaths(root)
	if err != nil {
		return Index{}, err
	}

	fingerprints, err := fingerprintFiles(paths)
	if err != nil {
		return Index{}, err
	}

	fingerprint := fingerprintKey(fingerprints)
	cacheKey := "dir\x00" + cleanPath(root) + "\x00" + optionsKey(opts)
	if cached, ok := idxr.cached(cacheKey, fingerprint); ok {
		cached.Stats = IndexStats{CacheHit: true, FilesScanned: cached.Stats.FilesScanned, FilesReused: cached.Stats.FilesScanned}
		return cached, nil
	}
	reused := idxr.reusedFileCount(cacheKey, fingerprints)

	loadDir, patterns := packageLoadTarget(root, paths)
	index, err := loadIndex(loadRequest{
		Context:      ctx,
		Dir:          loadDir,
		Patterns:     patterns,
		Options:      opts,
		Fingerprints: fingerprints,
	})
	if err != nil {
		return Index{}, err
	}

	index.Stats = IndexStats{FilesScanned: len(paths), FilesReused: reused, FilesChanged: len(paths) - reused}
	idxr.store(cacheKey, fingerprint, fingerprints, index)

	return index, nil
}

// IndexFiles parses the provided Go source files.
func IndexFiles(paths []string) (Index, error) {
	return NewIndexer().IndexFiles(paths)
}

// IndexFilesContext parses the provided Go source files using ctx for
// package-loading cancellation.
func IndexFilesContext(ctx context.Context, paths []string) (Index, error) {
	return NewIndexer().IndexFilesContext(ctx, paths)
}

// IndexFiles parses the provided Go source files using this indexer's cache.
func (idxr *Indexer) IndexFiles(paths []string) (Index, error) {
	return idxr.IndexFilesContext(context.TODO(), paths)
}

// IndexFilesContext parses the provided Go source files using this indexer's
// cache and ctx for package-loading cancellation.
func (idxr *Indexer) IndexFilesContext(ctx context.Context, paths []string) (Index, error) {
	paths = normalizedGoFiles(paths)
	if len(paths) == 0 {
		return Index{Graph: codegraph.NewEvidence()}, nil
	}

	fingerprints, err := fingerprintFiles(paths)
	if err != nil {
		return Index{}, err
	}

	fingerprint := fingerprintKey(fingerprints)
	cacheKey := "files\x00" + strings.Join(paths, "\x00")
	if cached, ok := idxr.cached(cacheKey, fingerprint); ok {
		cached.Stats = IndexStats{CacheHit: true, FilesScanned: cached.Stats.FilesScanned, FilesReused: cached.Stats.FilesScanned}
		return cached, nil
	}
	reused := idxr.reusedFileCount(cacheKey, fingerprints)

	root := commonDir(paths)
	patterns := make([]string, 0, len(paths))
	for _, path := range paths {
		patterns = append(patterns, "file="+path)
	}

	index, err := loadIndex(loadRequest{
		Context:      ctx,
		Dir:          root,
		Patterns:     patterns,
		Fingerprints: fingerprints,
	})
	if err != nil {
		return Index{}, err
	}

	index.Stats = IndexStats{FilesScanned: len(paths), FilesReused: reused, FilesChanged: len(paths) - reused}
	idxr.store(cacheKey, fingerprint, fingerprints, index)

	return index, nil
}

// FindSymbol returns symbols with the exact name.
func (idx Index) FindSymbol(name string) []Symbol {
	var matches []Symbol

	for _, sym := range idx.Symbols {
		if sym.Name == name {
			matches = append(matches, sym)
		}
	}

	return matches
}

func (idxr *Indexer) cached(key, fingerprint string) (Index, bool) {
	if idxr == nil {
		return Index{}, false
	}

	idxr.mu.Lock()
	defer idxr.mu.Unlock()

	cached, ok := idxr.cache[key]
	if !ok || cached.fingerprint != fingerprint {
		return Index{}, false
	}

	return cloneIndex(cached.index), true
}

func (idxr *Indexer) reusedFileCount(key string, fingerprints map[string]fileFingerprint) int {
	if idxr == nil {
		return 0
	}

	idxr.mu.Lock()
	defer idxr.mu.Unlock()

	cached, ok := idxr.cache[key]
	if !ok {
		return 0
	}

	var reused int
	for path, current := range fingerprints {
		if previous, ok := cached.fingerprints[path]; ok && sameFileFingerprint(previous, current) {
			reused++
		}
	}

	return reused
}

func (idxr *Indexer) store(key, fingerprint string, fingerprints map[string]fileFingerprint, index Index) {
	if idxr == nil {
		return
	}

	idxr.mu.Lock()
	defer idxr.mu.Unlock()

	idxr.cache[key] = cachedIndex{fingerprint: fingerprint, fingerprints: cloneFileFingerprints(fingerprints), index: cloneIndex(index)}
}

func cloneFileFingerprints(fingerprints map[string]fileFingerprint) map[string]fileFingerprint {
	out := make(map[string]fileFingerprint, len(fingerprints))
	maps.Copy(out, fingerprints)

	return out
}

func sameFileFingerprint(left, right fileFingerprint) bool {
	return left.Path == right.Path &&
		left.Hash == right.Hash &&
		left.Size == right.Size &&
		left.ModTime.Equal(right.ModTime)
}

func cloneIndex(index Index) Index {
	index.Files = cloneFiles(index.Files)
	index.Symbols = append([]Symbol(nil), index.Symbols...)
	index.ImportEdges = append([]ImportEdge(nil), index.ImportEdges...)
	index.Packages = clonePackages(index.Packages)
	index.FileDetails = cloneSourceFiles(index.FileDetails)
	index.Declarations = cloneDeclarations(index.Declarations)
	index.Imports = cloneImports(index.Imports)
	index.References = cloneReferences(index.References)
	index.CallEdges = cloneCallEdges(index.CallEdges)
	index.Diagnostics = append([]Diagnostic(nil), index.Diagnostics...)
	index.Graph = index.Graph.Clone()
	return index
}

func cloneFiles(files []File) []File {
	out := append([]File(nil), files...)
	for i := range out {
		out[i].Imports = append([]string(nil), out[i].Imports...)
		out[i].Symbols = append([]Symbol(nil), out[i].Symbols...)
	}

	return out
}

func clonePackages(packages []PackageInfo) []PackageInfo {
	out := append([]PackageInfo(nil), packages...)
	for i := range out {
		out[i].Files = append([]string(nil), out[i].Files...)
	}

	return out
}

func cloneSourceFiles(files []SourceFile) []SourceFile {
	out := append([]SourceFile(nil), files...)
	for i := range out {
		out[i].BuildTags = append([]string(nil), out[i].BuildTags...)
		out[i].Build.Tags = append([]string(nil), out[i].Build.Tags...)
		out[i].Provenance.Build.Tags = append([]string(nil), out[i].Provenance.Build.Tags...)
	}

	return out
}

func cloneDeclarations(declarations []Declaration) []Declaration {
	out := append([]Declaration(nil), declarations...)
	for i := range out {
		out[i].Build.Tags = append([]string(nil), out[i].Build.Tags...)
		out[i].Provenance.Build.Tags = append([]string(nil), out[i].Provenance.Build.Tags...)
	}

	return out
}

func cloneImports(imports []Import) []Import {
	out := append([]Import(nil), imports...)
	for i := range out {
		out[i].Build.Tags = append([]string(nil), out[i].Build.Tags...)
		out[i].Provenance.Build.Tags = append([]string(nil), out[i].Provenance.Build.Tags...)
	}

	return out
}

func cloneReferences(references []Reference) []Reference {
	out := append([]Reference(nil), references...)
	for i := range out {
		out[i].Build.Tags = append([]string(nil), out[i].Build.Tags...)
		out[i].Provenance.Build.Tags = append([]string(nil), out[i].Provenance.Build.Tags...)
	}

	return out
}

func cloneCallEdges(edges []CallEdge) []CallEdge {
	out := append([]CallEdge(nil), edges...)
	for i := range out {
		out[i].Build.Tags = append([]string(nil), out[i].Build.Tags...)
		out[i].Provenance.Build.Tags = append([]string(nil), out[i].Provenance.Build.Tags...)
	}

	return out
}

func goFilePaths(root string) ([]string, error) {
	var files []string

	err := filepath.WalkDir(root, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}

		if entry.IsDir() {
			if path != root && skipDir(entry.Name()) {
				return filepath.SkipDir
			}

			return nil
		}

		if entry.Type().IsRegular() && strings.HasSuffix(entry.Name(), ".go") {
			files = append(files, cleanPath(path))
		}

		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("index dir %s: %w", root, err)
	}

	sort.Strings(files)

	return files, nil
}

func normalizedGoFiles(paths []string) []string {
	files := make([]string, 0, len(paths))
	seen := make(map[string]struct{}, len(paths))
	for _, path := range paths {
		path = strings.TrimSpace(path)
		if path == "" || !strings.HasSuffix(path, ".go") {
			continue
		}

		path = cleanPath(path)
		if _, ok := seen[path]; ok {
			continue
		}

		seen[path] = struct{}{}
		files = append(files, path)
	}

	sort.Strings(files)

	return files
}

func fingerprintFiles(paths []string) (map[string]fileFingerprint, error) {
	fingerprints := make(map[string]fileFingerprint, len(paths))
	for _, path := range paths {
		info, err := os.Stat(path)
		if err != nil {
			return nil, fmt.Errorf("stat %s: %w", path, err)
		}

		content, err := os.ReadFile(path)
		if err != nil {
			return nil, fmt.Errorf("hash %s: %w", path, err)
		}

		sum := sha256.Sum256(content)
		fingerprints[path] = fileFingerprint{
			Path:    path,
			Hash:    hex.EncodeToString(sum[:]),
			ModTime: info.ModTime(),
			Size:    info.Size(),
		}
	}

	return fingerprints, nil
}

func fingerprintKey(fingerprints map[string]fileFingerprint) string {
	paths := make([]string, 0, len(fingerprints))
	for path := range fingerprints {
		paths = append(paths, path)
	}
	sort.Strings(paths)

	parts := make([]string, 0, len(paths))
	for _, path := range paths {
		fingerprint := fingerprints[path]
		parts = append(parts, strings.Join([]string{
			fingerprint.Path,
			fingerprint.ModTime.UTC().Format(time.RFC3339Nano),
			strconv.FormatInt(fingerprint.Size, 10),
			fingerprint.Hash,
		}, "\x1f"))
	}

	return strings.Join(parts, "\x1e")
}

func packageLoadTarget(root string, paths []string) (dir string, patterns []string) {
	if len(paths) == 0 {
		return root, nil
	}

	moduleRoot, ok := findModuleRoot(root)
	if !ok {
		patterns := make([]string, 0, len(paths))
		for _, path := range paths {
			patterns = append(patterns, "file="+path)
		}

		return root, patterns
	}

	rel, err := filepath.Rel(moduleRoot, root)
	if err != nil || rel == "." {
		return moduleRoot, []string{"./..."}
	}

	return moduleRoot, []string{"./" + filepath.ToSlash(rel) + "/..."}
}

func findModuleRoot(root string) (string, bool) {
	root = cleanPath(root)
	for {
		if hasGoModule(root) {
			return root, true
		}

		parent := filepath.Dir(root)
		if parent == root {
			return "", false
		}

		root = parent
	}
}

func hasGoModule(root string) bool {
	_, err := os.Stat(filepath.Join(root, "go.mod"))
	return err == nil
}

func optionsKey(opts IndexOptions) string {
	tags := append([]string(nil), opts.Tags...)
	sort.Strings(tags)
	env := append([]string(nil), opts.Env...)
	sort.Strings(env)

	return strings.Join([]string{
		"tags=" + strings.Join(tags, ","),
		"env=" + strings.Join(env, ","),
		"exclude_tests=" + strconv.FormatBool(opts.ExcludeTests),
		"exclude_generated=" + strconv.FormatBool(opts.ExcludeGenerated),
	}, "\x1f")
}

func commonDir(paths []string) string {
	if len(paths) == 0 {
		return "."
	}

	common := filepath.Dir(paths[0])
	for _, path := range paths[1:] {
		dir := filepath.Dir(path)
		for !sameOrChild(dir, common) && common != filepath.Dir(common) {
			common = filepath.Dir(common)
		}
	}

	return common
}

func sameOrChild(path, root string) bool {
	if path == root {
		return true
	}
	rel, err := filepath.Rel(root, path)
	return err == nil && rel != ".." && !strings.HasPrefix(rel, "../")
}

func cleanPath(path string) string {
	if abs, err := filepath.Abs(path); err == nil {
		return filepath.Clean(abs)
	}

	return filepath.Clean(path)
}

func skipDir(name string) bool {
	switch name {
	case ".git", ".hg", ".svn", "node_modules", "vendor":
		return true
	default:
		return false
	}
}
