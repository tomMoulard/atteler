//nolint:wsl_v5 // Adapter parsing keeps compact scanner branches.
package codeintel

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io/fs"
	"maps"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"

	"github.com/tommoulard/atteler/pkg/codegraph"
)

var (
	pythonClassPattern      = regexp.MustCompile(`^\s*class\s+([A-Za-z_][A-Za-z0-9_]*)\b[^:]*:\s*(?:#.*)?$`)
	pythonFuncPattern       = regexp.MustCompile(`^\s*def\s+([A-Za-z_][A-Za-z0-9_]*)\s*\([^)]*\)\s*(?:->\s*[^:]+)?\s*:\s*(?:#.*)?$`)
	pythonFromImportPattern = regexp.MustCompile(`^\s*from\s+([A-Za-z_][A-Za-z0-9_.]*)\s+import\b`)
	pythonImportPattern     = regexp.MustCompile(`^\s*import\s+(.+)$`)
	pythonModuleNamePattern = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_.]*$`)
)

// WorkspaceIndexOptions configures the multi-language code-intelligence
// subsystem. Go controls the existing package-aware Go adapter. CachePath, when
// set, stores a deterministic JSON snapshot that can be reused across runs.
type WorkspaceIndexOptions struct {
	CachePath string
	Go        IndexOptions
}

// WorkspaceIndexer coordinates language adapters, persistence, and invalidation
// for a workspace-level code-intelligence index.
type WorkspaceIndexer struct {
	goIndexer *Indexer
	goLoader  goIndexLoader
	snapshot  *persistedWorkspaceIndex
	adapters  []languageAdapter
	options   WorkspaceIndexOptions
	mu        sync.Mutex
}

type workspaceIndexRequest struct {
	ctx context.Context
}

type goIndexLoader func(*Indexer, string, IndexOptions, workspaceIndexRequest) (Index, error)

type languageAdapter interface {
	Language() string
	Extensions() []string
	IndexFiles(context.Context, []adapterFile, map[string]persistedFileModel, *codegraph.EvidenceGraph) (Model, map[string]persistedFileModel, error)
}

type adapterFile struct {
	Path        string
	Fingerprint fileFingerprint
}

type pythonAdapter struct{}

// NewWorkspaceIndexer returns a multi-language workspace indexer.
func NewWorkspaceIndexer(options WorkspaceIndexOptions) *WorkspaceIndexer {
	return &WorkspaceIndexer{goIndexer: NewIndexer(), goLoader: loadGoWorkspaceIndex, adapters: defaultLanguageAdapters(), options: options}
}

// IndexDir indexes a workspace without a caller-supplied deadline. Prefer
// IndexDirContext when callers already have a request context.
func (idxr *WorkspaceIndexer) IndexDir(root string) (Index, error) {
	return idxr.indexDir(root, workspaceIndexRequest{})
}

// IndexDirContext indexes supported languages under root, reusing a persisted
// snapshot when every supported file fingerprint is unchanged.
func (idxr *WorkspaceIndexer) IndexDirContext(ctx context.Context, root string) (Index, error) {
	if ctx == nil {
		return Index{}, errors.New("codeintel workspace index: context is required")
	}

	return idxr.indexDir(root, workspaceIndexRequest{ctx: ctx})
}

func (idxr *WorkspaceIndexer) indexDir(root string, req workspaceIndexRequest) (Index, error) {
	if idxr == nil {
		idxr = NewWorkspaceIndexer(WorkspaceIndexOptions{})
	}
	if idxr.goIndexer == nil {
		idxr.goIndexer = NewIndexer()
	}
	if idxr.goLoader == nil {
		idxr.goLoader = loadGoWorkspaceIndex
	}
	if len(idxr.adapters) == 0 {
		idxr.adapters = defaultLanguageAdapters()
	}

	root, paths, fingerprints, fingerprint, err := workspaceFingerprints(req.ctx, root, idxr.supportedExtensions())
	if err != nil {
		return Index{}, err
	}
	optsKey := workspaceOptionsKey(idxr.options)

	persisted, persistedOK, err := idxr.readSnapshot()
	if err != nil {
		return Index{}, err
	}
	if ctxErr := contextError(req.ctx); ctxErr != nil {
		return Index{}, ctxErr
	}
	if persistedOK && persisted.Root != root {
		persisted = persistedWorkspaceIndex{}
		persistedOK = false
	}
	if persistedOK && persistedIndexMatches(persisted, root, optsKey, fingerprint) {
		index := restoreIndex(persisted.Index)
		index.Stats = IndexStats{CacheHit: true, FilesScanned: index.Stats.FilesScanned, FilesReused: index.Stats.FilesScanned}
		index.Model.Stats = index.Stats

		return index, nil
	}

	reused, deleted := fileChangeCountsFromSnapshot(persisted, persistedOK, fingerprints)
	index, fileModels, err := idxr.indexWorkspace(root, fingerprints, persisted, persistedOK, req)
	if err != nil {
		return Index{}, err
	}

	index.Stats = IndexStats{FilesScanned: len(paths), FilesReused: reused, FilesChanged: len(paths) - reused, FilesDeleted: deleted}
	index.Model.Stats = index.Stats

	if ctxErr := contextError(req.ctx); ctxErr != nil {
		return Index{}, ctxErr
	}
	snapshot := newPersistedWorkspaceIndex(root, optsKey, fingerprint, fingerprints, fileModels, index)
	if err := idxr.writeSnapshot(snapshot); err != nil {
		return Index{}, err
	}

	return index, nil
}

func workspaceFingerprints(
	ctx context.Context,
	root string,
	extensions map[string]struct{},
) (cleanRoot string, paths []string, fingerprints map[string]fileFingerprint, fingerprint string, err error) {
	if ctxErr := contextError(ctx); ctxErr != nil {
		return "", nil, nil, "", ctxErr
	}

	cleanRoot = cleanPath(root)
	paths, err = workspaceFilePaths(cleanRoot, extensions)
	if err != nil {
		return "", nil, nil, "", err
	}
	if ctxErr := contextError(ctx); ctxErr != nil {
		return "", nil, nil, "", ctxErr
	}

	fingerprints, err = fingerprintFiles(paths)
	if err != nil {
		return "", nil, nil, "", err
	}
	if ctxErr := contextError(ctx); ctxErr != nil {
		return "", nil, nil, "", ctxErr
	}

	return cleanRoot, paths, fingerprints, fingerprintKey(fingerprints), nil
}

func (idxr *WorkspaceIndexer) readSnapshot() (persistedWorkspaceIndex, bool, error) {
	if idxr == nil {
		return persistedWorkspaceIndex{}, false, nil
	}

	if idxr.options.CachePath != "" {
		snapshot, ok, err := readPersistedWorkspaceIndex(idxr.options.CachePath)
		if err != nil {
			return persistedWorkspaceIndex{}, false, err
		}
		if ok {
			idxr.storeMemorySnapshot(snapshot)

			return snapshot, true, nil
		}
	}

	idxr.mu.Lock()
	defer idxr.mu.Unlock()

	if idxr.snapshot == nil {
		return persistedWorkspaceIndex{}, false, nil
	}

	return clonePersistedWorkspaceIndex(*idxr.snapshot), true, nil
}

func (idxr *WorkspaceIndexer) writeSnapshot(snapshot persistedWorkspaceIndex) error {
	if idxr == nil {
		return nil
	}

	if err := writePersistedWorkspaceIndex(idxr.options.CachePath, snapshot); err != nil {
		return err
	}

	idxr.storeMemorySnapshot(snapshot)

	return nil
}

func (idxr *WorkspaceIndexer) storeMemorySnapshot(snapshot persistedWorkspaceIndex) {
	idxr.mu.Lock()
	defer idxr.mu.Unlock()

	cloned := clonePersistedWorkspaceIndex(snapshot)
	idxr.snapshot = &cloned
}

func (idxr *WorkspaceIndexer) indexWorkspace(
	root string,
	fingerprints map[string]fileFingerprint,
	persisted persistedWorkspaceIndex,
	persistedOK bool,
	req workspaceIndexRequest,
) (Index, map[string]persistedFileModel, error) {
	goIndex, err := idxr.indexGoWorkspace(root, fingerprints, persisted, persistedOK, req)
	if err != nil {
		return Index{}, nil, err
	}
	if goIndex.Graph == nil {
		goIndex.Graph = codegraph.NewEvidence()
	}
	if len(goIndex.Model.Files) == 0 && len(goIndex.FileDetails) > 0 {
		goIndex.Model = modelFromGoIndex(goIndex)
	}

	adapterModel, fileModels, err := idxr.indexAdapterFiles(req.ctx, fingerprints, persisted.FileModels, goIndex.Graph)
	if err != nil {
		return Index{}, nil, err
	}
	goIndex.Model = mergeModels(goIndex.Model, adapterModel)

	return goIndex, fileModels, nil
}

func (idxr *WorkspaceIndexer) indexGoWorkspace(
	root string,
	fingerprints map[string]fileFingerprint,
	persisted persistedWorkspaceIndex,
	persistedOK bool,
	req workspaceIndexRequest,
) (Index, error) {
	if persistedOK && persisted.OptionsKey == workspaceOptionsKey(idxr.options) && goFingerprintsUnchanged(persisted.Fingerprints, fingerprints) {
		return restoreGoIndex(persisted), nil
	}

	return idxr.goLoader(idxr.goIndexer, root, idxr.options.Go, req)
}

func loadGoWorkspaceIndex(idxr *Indexer, root string, opts IndexOptions, req workspaceIndexRequest) (Index, error) {
	if idxr == nil {
		idxr = NewIndexer()
	}
	if req.ctx == nil {
		return idxr.IndexDirWithOptions(root, opts)
	}

	return idxr.IndexDirWithOptionsContext(req.ctx, root, opts)
}

func workspaceOptionsKey(options WorkspaceIndexOptions) string {
	return "go=" + optionsKey(options.Go)
}

func workspaceFilePaths(root string, extensions map[string]struct{}) ([]string, error) {
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
		if !entry.Type().IsRegular() {
			return nil
		}
		if supportedWorkspaceFile(entry.Name(), extensions) {
			files = append(files, cleanPath(path))
		}

		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("index workspace %s: %w", root, err)
	}

	sort.Strings(files)

	return files, nil
}

func supportedWorkspaceFile(name string, extensions map[string]struct{}) bool {
	_, ok := extensions[strings.ToLower(filepath.Ext(name))]
	return ok
}

func defaultLanguageAdapters() []languageAdapter {
	return []languageAdapter{pythonAdapter{}}
}

func goFingerprintsUnchanged(previous, current map[string]fileFingerprint) bool {
	for path, fingerprint := range current {
		if !isGoFile(path) {
			continue
		}
		if previousFingerprint, ok := previous[path]; !ok || !sameFileFingerprint(previousFingerprint, fingerprint) {
			return false
		}
	}
	for path := range previous {
		if !isGoFile(path) {
			continue
		}
		if _, ok := current[path]; !ok {
			return false
		}
	}

	return true
}

func isGoFile(path string) bool {
	return strings.EqualFold(filepath.Ext(path), ".go")
}

func restoreGoIndex(snapshot persistedWorkspaceIndex) Index {
	index := restoreIndex(snapshot.Index)
	index.Graph = graphWithoutPersistedFileModels(snapshot.Index, snapshot.FileModels)
	index.Model = modelFromGoIndex(index)

	return index
}

func graphWithoutPersistedFileModels(snapshot persistedIndex, fileModels map[string]persistedFileModel) *codegraph.EvidenceGraph {
	staleNodes, staleRelationships := persistedFileModelGraphItems(fileModels)
	graph := codegraph.NewEvidence()

	for i := range snapshot.GraphNodes {
		node := snapshot.GraphNodes[i]
		if _, stale := staleNodes[node.ID]; stale {
			continue
		}
		graph.AddNode(node)
	}
	for i := range snapshot.GraphRelationships {
		relationship := snapshot.GraphRelationships[i]
		key := graphRelationshipKey{
			from: string(relationship.From),
			to:   string(relationship.To),
			kind: relationship.Kind,
		}
		if _, stale := staleRelationships[key]; stale {
			continue
		}
		if _, stale := staleNodes[relationship.From]; stale {
			continue
		}
		if _, stale := staleNodes[relationship.To]; stale {
			continue
		}
		graph.AddRelationship(relationship)
	}

	return graph
}

type graphRelationshipKey struct {
	from string
	to   string
	kind string
}

func persistedFileModelGraphItems(fileModels map[string]persistedFileModel) (nodes map[codegraph.NodeID]struct{}, relationships map[graphRelationshipKey]struct{}) {
	nodes = make(map[codegraph.NodeID]struct{})
	relationships = make(map[graphRelationshipKey]struct{})
	for path := range fileModels {
		fileModel := fileModels[path]
		model := fileModel.Model
		for i := range model.Files {
			nodes[codegraph.NodeID(model.Files[i].ID)] = struct{}{}
		}
		for i := range model.Symbols {
			nodes[codegraph.NodeID(model.Symbols[i].ID)] = struct{}{}
		}
		for i := range model.Definitions {
			nodes[codegraph.NodeID(model.Definitions[i].ID)] = struct{}{}
		}
		for i := range model.References {
			nodes[codegraph.NodeID(model.References[i].ID)] = struct{}{}
		}
		for i := range model.Relationships {
			relationship := model.Relationships[i]
			if adapterOwnedGraphID(relationship.FromID, fileModel.Language) {
				nodes[codegraph.NodeID(relationship.FromID)] = struct{}{}
			}
			if adapterOwnedGraphID(relationship.ToID, fileModel.Language) {
				nodes[codegraph.NodeID(relationship.ToID)] = struct{}{}
			}
			relationships[graphRelationshipKey{
				from: relationship.FromID,
				to:   relationship.ToID,
				kind: relationship.Kind,
			}] = struct{}{}
		}
	}

	return nodes, relationships
}

func adapterOwnedGraphID(id, language string) bool {
	if id == "" {
		return false
	}

	return strings.HasPrefix(id, "import:"+language+":") ||
		strings.HasPrefix(id, "decl:"+language+":") ||
		strings.HasPrefix(id, "ref:"+language+":")
}

func (idxr *WorkspaceIndexer) supportedExtensions() map[string]struct{} {
	extensions := map[string]struct{}{".go": {}}
	for _, adapter := range idxr.adapters {
		for _, ext := range adapter.Extensions() {
			extensions[strings.ToLower(ext)] = struct{}{}
		}
	}

	return extensions
}

func (idxr *WorkspaceIndexer) indexAdapterFiles(
	ctx context.Context,
	fingerprints map[string]fileFingerprint,
	previous map[string]persistedFileModel,
	graph *codegraph.EvidenceGraph,
) (Model, map[string]persistedFileModel, error) {
	var model Model
	fileModels := make(map[string]persistedFileModel)
	for _, adapter := range idxr.adapters {
		files := adapterFilesForExtensions(fingerprints, adapter.Extensions())
		adapterModel, adapterFileModels, err := adapter.IndexFiles(ctx, files, previous, graph)
		if err != nil {
			return Model{}, nil, fmt.Errorf("index %s files: %w", adapter.Language(), err)
		}
		model = mergeModels(model, adapterModel)
		maps.Copy(fileModels, adapterFileModels)
	}
	model.sort()

	return model, fileModels, nil
}

func adapterFilesForExtensions(fingerprints map[string]fileFingerprint, extensions []string) []adapterFile {
	extensionSet := make(map[string]struct{}, len(extensions))
	for _, ext := range extensions {
		extensionSet[strings.ToLower(ext)] = struct{}{}
	}

	paths := make([]string, 0, len(fingerprints))
	for path := range fingerprints {
		if _, ok := extensionSet[strings.ToLower(filepath.Ext(path))]; ok {
			paths = append(paths, path)
		}
	}
	sort.Strings(paths)

	files := make([]adapterFile, 0, len(paths))
	for _, path := range paths {
		files = append(files, adapterFile{Path: path, Fingerprint: fingerprints[path]})
	}

	return files
}

func (pythonAdapter) Language() string {
	return LanguagePython
}

func (pythonAdapter) Extensions() []string {
	return []string{".py"}
}

func (adapter pythonAdapter) IndexFiles(
	ctx context.Context,
	files []adapterFile,
	previous map[string]persistedFileModel,
	graph *codegraph.EvidenceGraph,
) (Model, map[string]persistedFileModel, error) {
	var model Model
	fileModels := make(map[string]persistedFileModel, len(files))
	for _, file := range files {
		if err := contextError(ctx); err != nil {
			return Model{}, nil, err
		}

		fileModel, reused := adapter.reusedFileModel(file, previous, graph)
		if !reused {
			var err error
			fileModel, err = indexPythonFile(file.Path, file.Fingerprint, graph)
			if err != nil {
				return Model{}, nil, err
			}
		}

		model = mergeModels(model, fileModel)
		fileModels[file.Path] = persistedFileModel{
			Language:    adapter.Language(),
			Fingerprint: cloneFileFingerprint(file.Fingerprint),
			Model:       cloneModel(fileModel),
		}
	}
	model.sort()

	return model, fileModels, nil
}

func (adapter pythonAdapter) reusedFileModel(file adapterFile, previous map[string]persistedFileModel, graph *codegraph.EvidenceGraph) (Model, bool) {
	if len(previous) == 0 {
		return Model{}, false
	}

	persisted, ok := previous[file.Path]
	if !ok || persisted.Language != adapter.Language() || !sameFileFingerprint(persisted.Fingerprint, file.Fingerprint) {
		return Model{}, false
	}

	model := cloneModel(persisted.Model)
	addModelToGraph(model, graph)

	return model, true
}

func indexPythonFile(path string, fingerprint fileFingerprint, graph *codegraph.EvidenceGraph) (Model, error) {
	file, err := os.Open(path)
	if err != nil {
		return Model{}, fmt.Errorf("open python source %s: %w", path, err)
	}
	defer func() { _ = file.Close() }()

	fileID := string(fileNodeID(path))
	provenance := Provenance{
		Source:     "python:scanner",
		Range:      SourceRange{File: path, StartLine: 1, StartColumn: 1},
		Confidence: "medium",
	}
	model := Model{Files: []CodeFile{{
		ID:          fileID,
		Path:        path,
		Language:    LanguagePython,
		ContentHash: fingerprint.Hash,
		Size:        fingerprint.Size,
		ModTime:     fingerprint.ModTime,
		Provenance:  provenance,
	}}}
	if graph != nil {
		graph.AddNode(codegraph.Node{ID: codegraph.NodeID(fileID), Kind: "file", Name: filepath.ToSlash(path)})
	}

	scanner := bufio.NewScanner(file)
	lineNumber := 0
	for scanner.Scan() {
		lineNumber++
		line := scanner.Text()
		if diagnostic := pythonParseDiagnostic(path, line, lineNumber); diagnostic.ID != "" {
			model.Diagnostics = append(model.Diagnostics, diagnostic)
			continue
		}
		if match := pythonClassPattern.FindStringSubmatch(line); match != nil {
			addPythonDefinition(&model, graph, path, fileID, match[1], "class", line, lineNumber)
			continue
		}
		if match := pythonFuncPattern.FindStringSubmatch(line); match != nil {
			addPythonDefinition(&model, graph, path, fileID, match[1], "function", line, lineNumber)
			continue
		}
		for _, module := range pythonImportModules(line) {
			addPythonImport(&model, graph, path, fileID, module, line, lineNumber)
		}
	}
	if err := scanner.Err(); err != nil {
		return Model{}, fmt.Errorf("scan python source %s: %w", path, err)
	}
	model.Files[0].Range = SourceRange{File: path, StartLine: 1, StartColumn: 1, EndLine: max(lineNumber, 1), EndColumn: 1}
	model.sort()

	return model, nil
}

func pythonImportModules(line string) []string {
	if match := pythonFromImportPattern.FindStringSubmatch(line); match != nil {
		return []string{match[1]}
	}

	match := pythonImportPattern.FindStringSubmatch(line)
	if match == nil {
		return nil
	}

	spec := strings.Split(match[1], "#")[0]
	parts := strings.Split(spec, ",")
	modules := make([]string, 0, len(parts))
	for _, part := range parts {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}

		module, _, _ := strings.Cut(part, " as ")
		module = strings.TrimSpace(module)
		if pythonModuleNamePattern.MatchString(module) {
			modules = append(modules, module)
		}
	}

	return modules
}

func pythonParseDiagnostic(path, line string, lineNumber int) CodeDiagnostic {
	trimmed := strings.TrimSpace(line)
	if trimmed == "" || strings.HasPrefix(trimmed, "#") {
		return CodeDiagnostic{}
	}

	var message string
	switch {
	case strings.HasPrefix(trimmed, "def ") && !pythonFuncPattern.MatchString(line):
		message = "unable to parse Python function definition"
	case strings.HasPrefix(trimmed, "class ") && !pythonClassPattern.MatchString(line):
		message = "unable to parse Python class definition"
	default:
		return CodeDiagnostic{}
	}

	rangeInfo := SourceRange{File: path, StartLine: lineNumber, StartColumn: 1, EndLine: lineNumber, EndColumn: len(line) + 1}
	return CodeDiagnostic{
		ID:       diagnosticID(LanguagePython, "parse", fmt.Sprintf("%s:%d", path, lineNumber), message),
		Language: LanguagePython,
		File:     path,
		Source:   "python:scanner",
		Severity: "parse",
		Message:  message,
		Range:    rangeInfo,
	}
}

func addPythonDefinition(model *Model, graph *codegraph.EvidenceGraph, path, fileID, name, kind, line string, lineNumber int) {
	rangeInfo := pythonNameRange(path, line, name, lineNumber)
	id := pythonDefinitionID(path, kind, name, lineNumber)
	provenance := Provenance{Source: "python:scanner", Range: rangeInfo, Confidence: "medium"}
	definition := CodeDefinition{
		ID:         id,
		Name:       name,
		Kind:       kind,
		Language:   LanguagePython,
		File:       path,
		Range:      rangeInfo,
		Exported:   !strings.HasPrefix(name, "_"),
		Provenance: provenance,
	}
	model.Definitions = append(model.Definitions, definition)
	model.Symbols = append(model.Symbols, CodeSymbol{
		ID:           id,
		Name:         name,
		Kind:         kind,
		Language:     LanguagePython,
		File:         path,
		DefinitionID: id,
		Range:        rangeInfo,
		Provenance:   provenance,
	})
	model.Relationships = append(model.Relationships, CodeRelationship{
		FromID:     fileID,
		ToID:       id,
		Kind:       "declares",
		Language:   LanguagePython,
		File:       path,
		Range:      rangeInfo,
		Provenance: []Provenance{provenance},
	})
	if graph != nil {
		graph.AddNode(codegraph.Node{ID: codegraph.NodeID(id), Kind: "declaration", Name: name})
		graph.AddRelationship(codegraph.Relationship{
			From:       codegraph.NodeID(fileID),
			To:         codegraph.NodeID(id),
			Kind:       "declares",
			Provenance: []codegraph.Provenance{graphProvenance(provenance)},
		})
	}
}

func addPythonImport(model *Model, graph *codegraph.EvidenceGraph, path, fileID, module, line string, lineNumber int) {
	if module == "" {
		return
	}

	rangeInfo := pythonNameRange(path, line, module, lineNumber)
	id := pythonImportID(path, module, lineNumber)
	provenance := Provenance{Source: "python:scanner", Range: rangeInfo, Confidence: "low"}
	model.References = append(model.References, CodeReference{
		ID:         id,
		Name:       module,
		Kind:       "import",
		Language:   LanguagePython,
		File:       path,
		FromID:     fileID,
		ToID:       id,
		Range:      rangeInfo,
		Provenance: provenance,
	})
	model.Relationships = append(model.Relationships, CodeRelationship{
		FromID:     fileID,
		ToID:       id,
		Kind:       "imports",
		Language:   LanguagePython,
		File:       path,
		Range:      rangeInfo,
		Provenance: []Provenance{provenance},
	})
	if graph != nil {
		graph.AddNode(codegraph.Node{ID: codegraph.NodeID(id), Kind: "import", Name: module})
		graph.AddRelationship(codegraph.Relationship{
			From:       codegraph.NodeID(fileID),
			To:         codegraph.NodeID(id),
			Kind:       "imports",
			Provenance: []codegraph.Provenance{graphProvenance(provenance)},
		})
	}
}

func pythonNameRange(path, line, name string, lineNumber int) SourceRange {
	column := strings.Index(line, name)
	column = max(column, 0)
	start := column + 1

	return SourceRange{File: path, StartLine: lineNumber, StartColumn: start, EndLine: lineNumber, EndColumn: start + len(name)}
}

func pythonDefinitionID(path, kind, name string, lineNumber int) string {
	return strings.Join([]string{"decl", LanguagePython, filepath.ToSlash(path), kind, name, strconv.Itoa(lineNumber)}, ":")
}

func pythonImportID(path, module string, lineNumber int) string {
	return strings.Join([]string{"import", LanguagePython, filepath.ToSlash(path), module, strconv.Itoa(lineNumber)}, ":")
}

func fileChangeCountsFromSnapshot(snapshot persistedWorkspaceIndex, ok bool, current map[string]fileFingerprint) (reused, deleted int) {
	if !ok {
		return 0, 0
	}

	for path, fingerprint := range current {
		if previous, ok := snapshot.Fingerprints[path]; ok && sameFileFingerprint(previous, fingerprint) {
			reused++
		}
	}
	for path := range snapshot.Fingerprints {
		if _, ok := current[path]; !ok {
			deleted++
		}
	}

	return reused, deleted
}

func addModelToGraph(model Model, graph *codegraph.EvidenceGraph) {
	if graph == nil {
		return
	}

	added := make(map[codegraph.NodeID]struct{})
	for i := range model.Files {
		file := model.Files[i]
		addModelGraphNode(graph, added, codegraph.Node{ID: codegraph.NodeID(file.ID), Kind: "file", Name: filepath.ToSlash(file.Path)})
	}
	for i := range model.Definitions {
		definition := model.Definitions[i]
		addModelGraphNode(graph, added, codegraph.Node{ID: codegraph.NodeID(definition.ID), Kind: "declaration", Name: definition.Name})
	}
	for i := range model.Symbols {
		symbol := model.Symbols[i]
		addModelGraphNode(graph, added, codegraph.Node{ID: codegraph.NodeID(symbol.ID), Kind: "symbol", Name: symbol.Name})
	}
	for i := range model.References {
		reference := model.References[i]
		kind := "reference"
		if reference.Kind == "import" {
			kind = "import"
		}
		addModelGraphNode(graph, added, codegraph.Node{ID: codegraph.NodeID(reference.ID), Kind: kind, Name: reference.Name})
	}
	for i := range model.Relationships {
		relationship := model.Relationships[i]
		graph.AddRelationship(codegraph.Relationship{
			From:       codegraph.NodeID(relationship.FromID),
			To:         codegraph.NodeID(relationship.ToID),
			Kind:       relationship.Kind,
			Provenance: graphProvenanceList(relationship.Provenance),
		})
	}
}

func addModelGraphNode(graph *codegraph.EvidenceGraph, added map[codegraph.NodeID]struct{}, node codegraph.Node) {
	if node.ID == "" {
		return
	}
	if _, ok := added[node.ID]; ok {
		return
	}

	added[node.ID] = struct{}{}
	graph.AddNode(node)
}

func graphProvenanceList(provenance []Provenance) []codegraph.Provenance {
	out := make([]codegraph.Provenance, 0, len(provenance))
	for i := range provenance {
		out = append(out, graphProvenance(provenance[i]))
	}

	return out
}

func contextError(ctx context.Context) error {
	if ctx == nil {
		return nil
	}

	if err := ctx.Err(); err != nil {
		return fmt.Errorf("code-intelligence context: %w", err)
	}

	return nil
}
