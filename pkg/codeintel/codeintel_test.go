//nolint:wsl_v5 // Existing tests and query builders use compact assertion/evidence blocks.
package codeintel

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"sort"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/tommoulard/atteler/pkg/codegraph"
)

func TestIndexFiles_SummarizesPackagesImportsAndSymbols(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	path := writeGoFile(t, dir, "alpha.go", `package alpha

import (
	"context"
	"fmt"
)

const Answer = 42
var Name = "atteler"

type Runner struct{}

func NewRunner() Runner {
	return Runner{}
}

func (Runner) Run(context.Context) error {
	return nil
}
`)

	idx, err := IndexFiles([]string{path})
	if err != nil {
		t.Fatalf("IndexFiles() error = %v", err)
	}

	if len(idx.Files) != 1 {
		t.Fatalf("len(Files) = %d, want 1", len(idx.Files))
	}

	file := idx.Files[0]
	if file.Path != path {
		t.Fatalf("Path = %q, want %q", file.Path, path)
	}

	if file.Package != "alpha" {
		t.Fatalf("Package = %q, want alpha", file.Package)
	}

	if !reflect.DeepEqual(file.Imports, []string{"context", "fmt"}) {
		t.Fatalf("Imports = %#v, want context/fmt", file.Imports)
	}

	assertSymbol(t, file.Symbols, Symbol{Name: "Answer", Kind: "const", File: path, Line: 8})
	assertSymbol(t, file.Symbols, Symbol{Name: "Name", Kind: "var", File: path, Line: 9})
	assertSymbol(t, file.Symbols, Symbol{Name: "Runner", Kind: "type", File: path, Line: 11})
	assertSymbol(t, file.Symbols, Symbol{Name: "NewRunner", Kind: "func", File: path, Line: 13})
	assertSymbol(t, file.Symbols, Symbol{Name: "Run", Kind: "method", File: path, Line: 17})

	if len(idx.ImportEdges) != 2 {
		t.Fatalf("len(ImportEdges) = %d, want 2", len(idx.ImportEdges))
	}
}

func TestIndexDir_WalksGoFilesAndFindsSymbols(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	rootFile := writeGoFile(t, dir, "root.go", `package sample

import "strings"

func Root() {}
`)
	nestedFile := writeGoFile(t, dir, "internal/nested.go", `package internal

type Nested struct{}
`)
	writeGoFile(t, dir, "vendor/ignored.go", `package ignored

func Ignored() {}
`)

	if err := os.WriteFile(filepath.Join(dir, "notes.txt"), []byte("not go"), 0o600); err != nil {
		t.Fatalf("write notes: %v", err)
	}

	idx, err := IndexDir(dir)
	if err != nil {
		t.Fatalf("IndexDir() error = %v", err)
	}

	files := make([]string, 0, len(idx.Files))
	for _, file := range idx.Files {
		files = append(files, file.Path)
	}

	if !reflect.DeepEqual(files, []string{nestedFile, rootFile}) {
		t.Fatalf("Files = %#v, want root and nested files", files)
	}

	rootMatches := idx.FindSymbol("Root")
	if !reflect.DeepEqual(rootMatches, []Symbol{{Name: "Root", Kind: "func", File: rootFile, Line: 5}}) {
		t.Fatalf("FindSymbol(Root) = %#v", rootMatches)
	}

	nestedMatches := idx.FindSymbol("Nested")
	if !reflect.DeepEqual(nestedMatches, []Symbol{{Name: "Nested", Kind: "type", File: nestedFile, Line: 3}}) {
		t.Fatalf("FindSymbol(Nested) = %#v", nestedMatches)
	}

	if matches := idx.FindSymbol("Ignored"); len(matches) != 0 {
		t.Fatalf("FindSymbol(Ignored) = %#v, want none", matches)
	}
}

func TestIndexFiles_ReturnsParseErrorWithPath(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	path := writeGoFile(t, dir, "broken.go", "package broken\nfunc nope(\n")

	_, err := IndexFiles([]string{path})
	if err == nil {
		t.Fatal("IndexFiles() error = nil, want parse error")
	}

	if got := err.Error(); !strings.Contains(got, "parse") || !strings.Contains(got, path) {
		t.Fatalf("error = %q, want parse error with path", got)
	}
}

func assertSymbol(t *testing.T, symbols []Symbol, want Symbol) {
	t.Helper()

	if slices.Contains(symbols, want) {
		return
	}

	t.Fatalf("symbols missing %#v in %#v", want, symbols)
}

func writeGoFile(t *testing.T, dir, name, content string) string {
	t.Helper()

	path := filepath.Join(dir, name)
	if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
		t.Fatalf("mkdir: %v", err)
	}

	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("write file: %v", err)
	}

	return path
}

func writeTextFile(t *testing.T, dir, name, content string) string {
	t.Helper()

	path := filepath.Join(dir, name)
	require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o750))
	require.NoError(t, os.WriteFile(path, []byte(content), 0o600))

	return path
}

func TestIndex_SymbolsAreSortedForStableLookup(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	bFile := writeGoFile(t, dir, "b.go", `package stable

func Zebra() {}
`)
	aFile := writeGoFile(t, dir, "a.go", `package stable

func Alpha() {}
`)

	idx, err := IndexFiles([]string{bFile, aFile})
	if err != nil {
		t.Fatalf("IndexFiles() error = %v", err)
	}

	var names []string
	for _, sym := range idx.Symbols {
		names = append(names, sym.Name)
	}

	sorted := append([]string(nil), names...)
	sort.Strings(sorted)

	if !reflect.DeepEqual(names, sorted) {
		t.Fatalf("Symbols order = %#v, want sorted by name", names)
	}
}

func TestIndexDir_LoadsSemanticDefinitionsReferencesImportsAndImpact(t *testing.T) {
	t.Parallel()
	opts := packageLoaderOptions(t)

	dir := writeSemanticModule(t)
	apiFile := filepath.Join(dir, "api.go")

	idx, err := NewIndexer().IndexDirWithOptions(dir, opts)
	require.NoError(t, err)

	definitions := idx.FindDefinitions("NewRunner")
	require.Len(t, definitions, 1)
	definition := definitions[0]
	require.Equal(t, "func", definition.Kind)
	require.Equal(t, "example.com/sem", definition.PackagePath)
	require.Equal(t, apiFile, definition.File)
	require.True(t, definition.Exported)
	require.Equal(t, "types:def", definition.Provenance.Source)
	require.NotZero(t, definition.Range.StartLine)
	require.NotEmpty(t, definition.Build.String())
	require.Len(t, idx.FindDefinitions("sem.NewRunner"), 1)
	require.Len(t, idx.FindDefinitions("example.com/sem.NewRunner"), 1)
	require.NotEmpty(t, idx.FindReferences("example.com/sem.NewRunner"))

	references := idx.FindReferences("NewRunner")
	require.NotEmpty(t, references)
	require.Equal(t, "types:use", references[0].Provenance.Source)
	require.Equal(t, definition.ID, references[0].ToDeclarationID)
	require.NotZero(t, references[0].Range.StartColumn)

	methodDefinitions := idx.FindDefinitions("Runner.Run")
	require.Len(t, methodDefinitions, 1)
	require.Equal(t, "method", methodDefinitions[0].Kind)
	require.Equal(t, "Runner", methodDefinitions[0].Receiver)
	require.True(t, methodDefinitions[0].Exported)
	require.Len(t, idx.FindDefinitions("sem.Runner.Run"), 1)
	require.Len(t, idx.FindDefinitions("example.com/sem.Runner.Run"), 1)
	require.NotEmpty(t, idx.FindReferences("Runner.Run"))

	unexportedReceiverMethods := idx.FindDefinitions("hidden.Visible")
	require.Len(t, unexportedReceiverMethods, 1)
	require.False(t, unexportedReceiverMethods[0].Exported)
	require.Empty(t, idx.AnalyzeImpact(ImpactQuery{SymbolName: "hidden.Visible"}).PublicAPIDeclarations)

	genericReceiverMethods := idx.FindDefinitions("Box.Get")
	require.Len(t, genericReceiverMethods, 1)
	require.Equal(t, "Box", genericReceiverMethods[0].Receiver)
	require.True(t, genericReceiverMethods[0].Exported)
	require.NotEmpty(t, idx.FindReferences("Box.Get"))

	publicAPI := idx.PublicAPI("example.com/sem")
	require.Contains(t, declarationNames(publicAPI), "NewRunner")
	require.Contains(t, declarationQualifiedNames(publicAPI), "Runner.Run")
	require.Contains(t, declarationQualifiedNames(publicAPI), "Box.Get")
	require.NotContains(t, declarationQualifiedNames(publicAPI), "hidden.Visible")
	require.NotEmpty(t, exportRelationships(idx.Graph.Relationships()))
	require.Contains(t, nodeKinds(idx.Graph.Nodes()), "api_boundary")
	require.Contains(t, nodeKinds(idx.Graph.Nodes()), "external_declaration")
	require.Contains(t, relationshipKinds(idx.Graph.Relationships()), "has_api_boundary")

	imports := idx.FindImports("context")
	require.Len(t, imports, 1)
	require.Equal(t, apiFile, imports[0].File)
	require.Equal(t, "parser:import", imports[0].Provenance.Source)
	require.NotZero(t, imports[0].Range.StartLine)

	require.NotEmpty(t, idx.CallEdges)
	require.Contains(t, callTargets(idx.CallEdges), definition.ID)
	callers := idx.FindCallers("NewRunner")
	require.NotEmpty(t, callers)
	require.Equal(t, "types:call", callers[0].Provenance.Source)
	require.Contains(t, callTargets(idx.FindCallees("Use")), definition.ID)
	require.NotEmpty(t, idx.FindCallers("Runner.Run"))
	require.Contains(t, callTargets(idx.FindCallees("Use")), genericReceiverMethods[0].ID)
	require.NotEmpty(t, idx.FindCallers("Box.Get"))
	useDefinitions := idx.FindDefinitions("Use")
	require.Len(t, useDefinitions, 1)
	testUseReferences := referencesWhere(idx.FindReferences("Use"), func(reference Reference) bool {
		return strings.HasSuffix(reference.File, "api_test.go")
	})
	require.NotEmpty(t, testUseReferences)
	require.Equal(t, useDefinitions[0].ID, testUseReferences[0].ToDeclarationID)
	require.Contains(t, callTargets(idx.FindCallees("TestUse")), useDefinitions[0].ID)

	impact := idx.AnalyzeImpact(ImpactQuery{File: "api.go", ImportPath: "context", SymbolName: "NewRunner"})
	require.Len(t, impact.DirectImports, 1)
	require.Len(t, impact.ReverseImports, 1)
	require.NotEmpty(t, impact.References)
	require.NotEmpty(t, impact.Callers)
	require.Len(t, impact.PublicAPIDeclarations, 1)
	require.NotEmpty(t, impact.Evidence)

	externalImpact := idx.AnalyzeImpact(ImpactQuery{SymbolName: "new"})
	require.NotEmpty(t, externalImpact.References)
	require.True(t, uncertaintyContains(externalImpact.Uncertainty, "medium confidence"), "uncertainty=%#v", externalImpact.Uncertainty)

	fileNode := fileNodeID(apiFile)
	neighbors := idx.Graph.NeighborsWithEvidence(fileNode)
	require.NotEmpty(t, neighbors)
	var sawImportEvidence bool
	for _, neighbor := range neighbors {
		for _, evidence := range neighbor.Evidence {
			if evidence.Kind == "imports" && len(evidence.Provenance) > 0 && evidence.Provenance[0].Source == "parser:import" {
				sawImportEvidence = true
			}
		}
	}
	require.True(t, sawImportEvidence, "expected import graph evidence for %s", apiFile)
	require.Contains(t, relationshipKinds(idx.Graph.Relationships()), "resolves_to")
	require.Contains(t, relationshipKinds(idx.Graph.Relationships()), "imports_package")
}

func TestIndexDir_HandlesGeneratedTestsBuildTagsAndVendorIntentionally(t *testing.T) {
	t.Parallel()
	opts := packageLoaderOptions(t)

	dir := writeSemanticModule(t)
	idx, err := NewIndexer().IndexDirWithOptions(dir, opts)
	require.NoError(t, err)

	require.NotEmpty(t, sourceFilesWhere(idx.FileDetails, func(file SourceFile) bool { return file.Test && strings.HasSuffix(file.Path, "api_test.go") }))
	require.NotEmpty(t, sourceFilesWhere(idx.FileDetails, func(file SourceFile) bool { return file.Generated && strings.HasSuffix(file.Path, "generated.go") }))
	require.Empty(t, sourceFilesWhere(idx.FileDetails, func(file SourceFile) bool { return strings.HasSuffix(file.Path, "tagged.go") }))
	require.Empty(t, idx.FindDefinitions("VendorIgnored"))

	idxr := NewIndexer()
	withoutGeneratedOpts := opts
	withoutGeneratedOpts.ExcludeGenerated = true
	withoutGenerated, err := idxr.IndexDirWithOptions(dir, withoutGeneratedOpts)
	require.NoError(t, err)
	require.Empty(t, withoutGenerated.FindDefinitions("Generated"))

	withoutTestsOpts := opts
	withoutTestsOpts.ExcludeTests = true
	withoutTests, err := idxr.IndexDirWithOptions(dir, withoutTestsOpts)
	require.NoError(t, err)
	require.Empty(t, withoutTests.FindDefinitions("TestUse"))

	withTagOpts := opts
	withTagOpts.Tags = []string{"integration"}
	withTag, err := idxr.IndexDirWithOptions(dir, withTagOpts)
	require.NoError(t, err)
	require.Len(t, withTag.FindDefinitions("Tagged"), 1)
	taggedFiles := sourceFilesWhere(withTag.FileDetails, func(file SourceFile) bool { return strings.HasSuffix(file.Path, "tagged.go") })
	require.Len(t, taggedFiles, 1)
	require.Equal(t, []string{"integration"}, taggedFiles[0].Build.Tags)
	require.Equal(t, []string{"integration"}, taggedFiles[0].BuildTags)
}

func TestIndexDir_UsesAncestorModuleForSubdirectory(t *testing.T) {
	t.Parallel()
	opts := packageLoaderOptions(t)

	dir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(dir, "go.mod"), []byte("module example.com/root\n\ngo 1.26.2\n"), 0o600))
	writeGoFile(t, dir, "root.go", `package root

func Root() {}
`)
	subFile := writeGoFile(t, dir, "sub/sub.go", `package sub

func Sub() {}
`)

	idx, err := NewIndexer().IndexDirWithOptions(filepath.Join(dir, "sub"), opts)
	require.NoError(t, err)
	require.Len(t, idx.Files, 1)
	require.Equal(t, subFile, idx.Files[0].Path)

	definitions := idx.FindDefinitions("Sub")
	require.Len(t, definitions, 1)
	require.Equal(t, "example.com/root/sub", definitions[0].PackagePath)
	require.Empty(t, idx.FindDefinitions("Root"))
}

func TestIndexDir_ReturnsPartialIndexWithTypeDiagnostics(t *testing.T) {
	t.Parallel()
	opts := packageLoaderOptions(t)

	dir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(dir, "go.mod"), []byte("module example.com/broken\n\ngo 1.26.2\n"), 0o600))
	writeGoFile(t, dir, "broken.go", `package broken

func Broken() {
	Missing()
}
`)

	idx, err := NewIndexer().IndexDirWithOptions(dir, opts)
	require.NoError(t, err)
	require.Len(t, idx.FindDefinitions("Broken"), 1)
	require.NotEmpty(t, idx.Diagnostics)
	require.True(t, diagnosticsContain(idx.Diagnostics, "Missing"), "diagnostics=%#v", idx.Diagnostics)
}

func TestIndexer_ReusesUnchangedFingerprintCache(t *testing.T) {
	t.Parallel()
	opts := packageLoaderOptions(t)

	dir := writeSemanticModule(t)
	idxr := NewIndexer()

	first, err := idxr.IndexDirWithOptions(dir, opts)
	require.NoError(t, err)
	require.False(t, first.Stats.CacheHit)
	require.Positive(t, first.Stats.FilesScanned)
	require.Zero(t, first.Stats.FilesReused)
	require.Equal(t, first.Stats.FilesScanned, first.Stats.FilesChanged)

	second, err := idxr.IndexDirWithOptions(dir, opts)
	require.NoError(t, err)
	require.True(t, second.Stats.CacheHit)
	require.Equal(t, first.Stats.FilesScanned, second.Stats.FilesScanned)
	require.Equal(t, first.Stats.FilesScanned, second.Stats.FilesReused)
	require.Zero(t, second.Stats.FilesChanged)

	writeGoFile(t, dir, "extra.go", `package sem

func Extra() {}
`)
	third, err := idxr.IndexDirWithOptions(dir, opts)
	require.NoError(t, err)
	require.False(t, third.Stats.CacheHit)
	require.Equal(t, first.Stats.FilesScanned, third.Stats.FilesReused)
	require.Equal(t, 1, third.Stats.FilesChanged)
	require.Len(t, third.FindDefinitions("Extra"), 1)
}

func TestIndexer_CachedIndexesAreIndependent(t *testing.T) {
	t.Parallel()
	opts := packageLoaderOptions(t)

	dir := writeSemanticModule(t)
	idxr := NewIndexer()

	first, err := idxr.IndexDirWithOptions(dir, opts)
	require.NoError(t, err)
	require.NotEmpty(t, first.Files)
	require.NotNil(t, first.Graph)

	first.Files[0].Imports[0] = "mutated"
	first.Packages[0].Files[0] = "mutated.go"
	first.FileDetails[0].Build.Tags = []string{"mutated"}
	first.Graph.AddNode(codegraph.Node{ID: "mutated", Kind: "test"})

	second, err := idxr.IndexDirWithOptions(dir, opts)
	require.NoError(t, err)
	require.True(t, second.Stats.CacheHit)
	require.NotEqual(t, "mutated", second.Files[0].Imports[0])
	require.NotEqual(t, "mutated.go", second.Packages[0].Files[0])
	require.NotEqual(t, []string{"mutated"}, second.FileDetails[0].Build.Tags)
	require.False(t, second.Graph.Graph().HasNode("mutated"))
}

func TestWorkspaceIndexer_PersistsPythonIndexAndInvalidatesChangedDeletedFiles(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	cachePath := filepath.Join(dir, ".atteler", "codeintel.json")
	pythonFile := writeTextFile(t, dir, "worker.py", `import os

class Worker:
	pass

def run():
	return os.name
`)

	indexer := NewWorkspaceIndexer(WorkspaceIndexOptions{CachePath: cachePath})
	first, err := indexer.IndexDirContext(t.Context(), dir)
	require.NoError(t, err)
	require.False(t, first.Stats.CacheHit)
	require.Equal(t, 1, first.Stats.FilesScanned)
	require.Equal(t, 1, first.Stats.FilesChanged)
	require.Len(t, first.Query(Query{Kind: QueryDefinitions, Name: "run", Language: LanguagePython}).Definitions, 1)
	require.Len(t, first.Query(Query{Kind: QueryDefinitions, Name: "Worker", Language: LanguagePython}).Definitions, 1)
	require.Len(t, first.Query(Query{Kind: QueryFiles, File: "worker.py", Language: LanguagePython}).Files, 1)
	require.Len(t, first.Query(Query{Kind: QueryRelationships, RelationshipKind: "imports", Language: LanguagePython}).Relationships, 1)
	firstSnapshot, ok, err := readPersistedWorkspaceIndex(cachePath)
	require.NoError(t, err)
	require.True(t, ok)
	require.Contains(t, firstSnapshot.FileModels, pythonFile)

	secondIndexer := NewWorkspaceIndexer(WorkspaceIndexOptions{CachePath: cachePath})
	second, err := secondIndexer.IndexDirContext(t.Context(), dir)
	require.NoError(t, err)
	require.True(t, second.Stats.CacheHit)
	require.Equal(t, 1, second.Stats.FilesReused)
	require.Len(t, second.Query(Query{Kind: QueryDefinitions, Name: "run", Language: LanguagePython}).Definitions, 1)

	writeTextFile(t, dir, "worker.py", `class Worker:
	pass

def changed():
	return "changed"
`)
	third, err := secondIndexer.IndexDirContext(t.Context(), dir)
	require.NoError(t, err)
	require.False(t, third.Stats.CacheHit)
	require.Zero(t, third.Stats.FilesReused)
	require.Equal(t, 1, third.Stats.FilesChanged)
	require.Empty(t, third.Query(Query{Kind: QueryDefinitions, Name: "run", Language: LanguagePython}).Definitions)
	require.Len(t, third.Query(Query{Kind: QueryDefinitions, Name: "changed", Language: LanguagePython}).Definitions, 1)

	require.NoError(t, os.Remove(pythonFile))
	fourth, err := secondIndexer.IndexDirContext(t.Context(), dir)
	require.NoError(t, err)
	require.False(t, fourth.Stats.CacheHit)
	require.Zero(t, fourth.Stats.FilesScanned)
	require.Equal(t, 1, fourth.Stats.FilesDeleted)
	require.Empty(t, fourth.Query(Query{Kind: QueryDefinitions, Language: LanguagePython}).Definitions)
	fourthSnapshot, ok, err := readPersistedWorkspaceIndex(cachePath)
	require.NoError(t, err)
	require.True(t, ok)
	require.NotContains(t, fourthSnapshot.FileModels, pythonFile)
}

func TestWorkspaceIndexer_ReusesInMemorySnapshotWithoutCachePath(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	alphaFile := writeTextFile(t, dir, "alpha.py", `def alpha():
	return "alpha"
`)
	writeTextFile(t, dir, "beta.py", `def beta():
	return "beta"
`)

	indexer := NewWorkspaceIndexer(WorkspaceIndexOptions{})
	first, err := indexer.IndexDirContext(t.Context(), dir)
	require.NoError(t, err)
	require.False(t, first.Stats.CacheHit)
	require.Equal(t, 2, first.Stats.FilesChanged)

	second, err := indexer.IndexDirContext(t.Context(), dir)
	require.NoError(t, err)
	require.True(t, second.Stats.CacheHit)
	require.Equal(t, 2, second.Stats.FilesReused)
	require.Len(t, second.Query(Query{Kind: QueryDefinitions, Name: "alpha", Language: LanguagePython}).Definitions, 1)

	writeTextFile(t, dir, "beta.py", `def changed():
	return "changed"
`)
	third, err := indexer.IndexDirContext(t.Context(), dir)
	require.NoError(t, err)
	require.False(t, third.Stats.CacheHit)
	require.Equal(t, 1, third.Stats.FilesReused)
	require.Equal(t, 1, third.Stats.FilesChanged)
	require.Len(t, third.Query(Query{Kind: QueryDefinitions, Name: "alpha", Language: LanguagePython}).Definitions, 1)
	require.Empty(t, third.Query(Query{Kind: QueryDefinitions, Name: "beta", Language: LanguagePython}).Definitions)
	require.Len(t, third.Query(Query{Kind: QueryDefinitions, Name: "changed", Language: LanguagePython}).Definitions, 1)

	require.NoError(t, os.Remove(alphaFile))
	fourth, err := indexer.IndexDirContext(t.Context(), dir)
	require.NoError(t, err)
	require.False(t, fourth.Stats.CacheHit)
	require.Equal(t, 1, fourth.Stats.FilesReused)
	require.Equal(t, 1, fourth.Stats.FilesDeleted)
	require.Empty(t, fourth.Query(Query{Kind: QueryDefinitions, Name: "alpha", Language: LanguagePython}).Definitions)
	require.Len(t, fourth.Query(Query{Kind: QueryDefinitions, Name: "changed", Language: LanguagePython}).Definitions, 1)
}

func TestWorkspaceIndexer_DoesNotReuseSnapshotAcrossRoots(t *testing.T) {
	t.Parallel()

	firstRoot := t.TempDir()
	secondRoot := t.TempDir()
	writeTextFile(t, firstRoot, "first.py", `def first():
	return "first"
`)
	writeTextFile(t, secondRoot, "second.py", `def second():
	return "second"
`)

	indexer := NewWorkspaceIndexer(WorkspaceIndexOptions{})
	first, err := indexer.IndexDirContext(t.Context(), firstRoot)
	require.NoError(t, err)
	require.Equal(t, 1, first.Stats.FilesChanged)
	require.Len(t, first.Query(Query{Kind: QueryDefinitions, Name: "first", Language: LanguagePython}).Definitions, 1)

	second, err := indexer.IndexDirContext(t.Context(), secondRoot)
	require.NoError(t, err)
	require.False(t, second.Stats.CacheHit)
	require.Equal(t, 1, second.Stats.FilesScanned)
	require.Equal(t, 1, second.Stats.FilesChanged)
	require.Zero(t, second.Stats.FilesDeleted)
	require.Empty(t, second.Query(Query{Kind: QueryDefinitions, Name: "first", Language: LanguagePython}).Definitions)
	require.Len(t, second.Query(Query{Kind: QueryDefinitions, Name: "second", Language: LanguagePython}).Definitions, 1)
}

func TestWorkspaceIndexer_RecordsAndInvalidatesPythonParseDiagnostics(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	cachePath := filepath.Join(dir, ".atteler", "codeintel.json")
	pythonFile := writeTextFile(t, dir, "broken.py", "def broken(\n\treturn 1\n")
	indexer := NewWorkspaceIndexer(WorkspaceIndexOptions{CachePath: cachePath})

	broken, err := indexer.IndexDirContext(t.Context(), dir)
	require.NoError(t, err)
	diagnostics := broken.Query(Query{Kind: QueryDiagnostics, Language: LanguagePython}).Diagnostics
	require.Len(t, diagnostics, 1)
	assert.Equal(t, pythonFile, diagnostics[0].File)
	assert.Contains(t, diagnostics[0].Message, "function definition")
	require.Empty(t, broken.Query(Query{Kind: QueryDefinitions, Name: "broken", Language: LanguagePython}).Definitions)

	writeTextFile(t, dir, "broken.py", "def broken():\n\treturn 1\n")
	fixed, err := indexer.IndexDirContext(t.Context(), dir)
	require.NoError(t, err)
	require.Empty(t, fixed.Query(Query{Kind: QueryDiagnostics, Language: LanguagePython}).Diagnostics)
	require.Len(t, fixed.Query(Query{Kind: QueryDefinitions, Name: "broken", Language: LanguagePython}).Definitions, 1)
}

func TestWorkspaceIndexer_IndexesPythonCommaImportsAsReferences(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	writeTextFile(t, dir, "imports.py", `import os, sys as system
from pathlib import Path

def helper():
	return Path(os.getcwd()).name + system.version
`)

	index, err := NewWorkspaceIndexer(WorkspaceIndexOptions{}).IndexDirContext(t.Context(), dir)
	require.NoError(t, err)

	references := index.Query(Query{Kind: QueryReferences, Language: LanguagePython}).References
	assert.Contains(t, codeReferenceNames(references), "os")
	assert.Contains(t, codeReferenceNames(references), "sys")
	assert.Contains(t, codeReferenceNames(references), "pathlib")

	imports := index.Query(Query{Kind: QueryRelationships, RelationshipKind: "imports", Language: LanguagePython}).Relationships
	require.Len(t, imports, 3)
	assert.True(t, codeRelationshipsSorted(imports), "relationships are not deterministic: %#v", imports)
}

func TestWorkspaceIndexer_StopsAdapterIndexingOnCanceledContext(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	cachePath := filepath.Join(dir, ".atteler", "codeintel.json")
	writeTextFile(t, dir, "worker.py", `def run():
	return "ok"
`)
	ctx, cancel := context.WithCancel(t.Context())
	cancel()

	indexer := NewWorkspaceIndexer(WorkspaceIndexOptions{CachePath: cachePath})
	goLoaderCalled := false
	indexer.goLoader = func(*Indexer, string, IndexOptions, workspaceIndexRequest) (Index, error) {
		goLoaderCalled = true

		return Index{}, errors.New("go loader should not run for an already-canceled context")
	}

	_, err := indexer.IndexDirContext(ctx, dir)
	require.Error(t, err)
	require.ErrorIs(t, err, context.Canceled)
	assert.False(t, goLoaderCalled)
	assert.NoFileExists(t, cachePath)
}

func TestWorkspaceIndexer_MergesGoAndPythonIntoDeterministicQueryModel(t *testing.T) {
	t.Parallel()
	opts := packageLoaderOptions(t)

	dir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(dir, "go.mod"), []byte("module example.com/mixed\n\ngo 1.26.2\n"), 0o600))
	goFile := writeGoFile(t, dir, "main.go", `package mixed

func GoThing() {}
`)
	pythonFile := writeTextFile(t, dir, "tools.py", `def helper():
	return "ok"
`)

	indexer := NewWorkspaceIndexer(WorkspaceIndexOptions{Go: opts})
	index, err := indexer.IndexDirContext(t.Context(), dir)
	require.NoError(t, err)

	goDefinitions := index.Query(Query{Kind: QueryDefinitions, Name: "GoThing", Language: LanguageGo}).Definitions
	pythonDefinitions := index.Query(Query{Kind: QueryDefinitions, Name: "helper", Language: LanguagePython}).Definitions
	require.Len(t, goDefinitions, 1)
	require.Len(t, pythonDefinitions, 1)
	assert.Equal(t, goFile, goDefinitions[0].File)
	assert.Equal(t, pythonFile, pythonDefinitions[0].File)

	files := index.Query(Query{Kind: QueryFiles}).Files
	require.Len(t, files, 2)
	assert.Equal(t, []string{LanguageGo, LanguagePython}, []string{files[0].Language, files[1].Language})

	relationships := index.Query(Query{Kind: QueryRelationships, RelationshipKind: "declares"}).Relationships
	require.NotEmpty(t, relationships)
	assert.True(t, codeRelationshipsSorted(relationships), "relationships are not deterministic: %#v", relationships)
	require.NotEmpty(t, relationshipWhere(relationships, func(relationship CodeRelationship) bool {
		return relationship.Language == LanguagePython && relationship.ToID == pythonDefinitions[0].ID
	}))
}

func TestWorkspaceIndexer_ReusesPersistedGoIndexWhenOnlyPythonChanges(t *testing.T) {
	t.Parallel()
	opts := packageLoaderOptions(t)

	dir := t.TempDir()
	cachePath := filepath.Join(dir, ".atteler", "codeintel.json")
	require.NoError(t, os.WriteFile(filepath.Join(dir, "go.mod"), []byte("module example.com/reuse\n\ngo 1.26.2\n"), 0o600))
	writeGoFile(t, dir, "main.go", `package reuse

func GoThing() {}
`)
	writeTextFile(t, dir, "tools.py", `def helper():
	return "ok"
`)

	firstIndexer := NewWorkspaceIndexer(WorkspaceIndexOptions{CachePath: cachePath, Go: opts})
	first, err := firstIndexer.IndexDirContext(t.Context(), dir)
	require.NoError(t, err)
	require.Len(t, first.Query(Query{Kind: QueryDefinitions, Name: "GoThing", Language: LanguageGo}).Definitions, 1)
	require.Len(t, first.Query(Query{Kind: QueryDefinitions, Name: "helper", Language: LanguagePython}).Definitions, 1)

	writeTextFile(t, dir, "tools.py", `def changed():
	return "changed"
`)
	errGoLoaderCalled := errors.New("go loader should not run for a python-only change")
	secondIndexer := NewWorkspaceIndexer(WorkspaceIndexOptions{CachePath: cachePath, Go: opts})
	secondIndexer.goLoader = func(*Indexer, string, IndexOptions, workspaceIndexRequest) (Index, error) {
		return Index{}, errGoLoaderCalled
	}
	second, err := secondIndexer.IndexDirContext(t.Context(), dir)
	require.NoError(t, err)
	require.False(t, second.Stats.CacheHit)
	require.Equal(t, 1, second.Stats.FilesReused)
	require.Equal(t, 1, second.Stats.FilesChanged)
	require.Len(t, second.Query(Query{Kind: QueryDefinitions, Name: "GoThing", Language: LanguageGo}).Definitions, 1)
	require.Empty(t, second.Query(Query{Kind: QueryDefinitions, Name: "helper", Language: LanguagePython}).Definitions)
	require.Len(t, second.Query(Query{Kind: QueryDefinitions, Name: "changed", Language: LanguagePython}).Definitions, 1)
}

func TestWorkspaceIndexer_ReindexesGoWhenOptionsChange(t *testing.T) {
	t.Parallel()
	opts := packageLoaderOptions(t)

	dir := t.TempDir()
	cachePath := filepath.Join(dir, ".atteler", "codeintel.json")
	require.NoError(t, os.WriteFile(filepath.Join(dir, "go.mod"), []byte("module example.com/options\n\ngo 1.26.2\n"), 0o600))
	writeGoFile(t, dir, "main.go", `package options

func Runtime() {}
`)
	writeGoFile(t, dir, "main_test.go", `package options

func TestOnly() {}
`)

	first, err := NewWorkspaceIndexer(WorkspaceIndexOptions{CachePath: cachePath, Go: opts}).IndexDirContext(t.Context(), dir)
	require.NoError(t, err)
	require.Len(t, first.Query(Query{Kind: QueryDefinitions, Name: "TestOnly", Language: LanguageGo}).Definitions, 1)

	changedOpts := opts
	changedOpts.ExcludeTests = true
	second, err := NewWorkspaceIndexer(WorkspaceIndexOptions{CachePath: cachePath, Go: changedOpts}).IndexDirContext(t.Context(), dir)
	require.NoError(t, err)
	require.False(t, second.Stats.CacheHit)
	require.Len(t, second.Query(Query{Kind: QueryDefinitions, Name: "Runtime", Language: LanguageGo}).Definitions, 1)
	require.Empty(t, second.Query(Query{Kind: QueryDefinitions, Name: "TestOnly", Language: LanguageGo}).Definitions)
}

func TestWorkspaceIndexer_ReusesPythonFileModelsWithGraphMetadata(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	cachePath := filepath.Join(dir, ".atteler", "codeintel.json")
	writeTextFile(t, dir, "alpha.py", `import os

def alpha():
	return os.name
`)
	writeTextFile(t, dir, "beta.py", `import sys

def beta():
	return sys.version
`)

	indexer := NewWorkspaceIndexer(WorkspaceIndexOptions{CachePath: cachePath})
	first, err := indexer.IndexDirContext(t.Context(), dir)
	require.NoError(t, err)
	require.Len(t, first.Query(Query{Kind: QueryDefinitions, Name: "alpha", Language: LanguagePython}).Definitions, 1)
	require.Len(t, first.Query(Query{Kind: QueryReferences, Name: "os", Language: LanguagePython}).References, 1)

	writeTextFile(t, dir, "beta.py", `import sys

def changed():
	return sys.version
`)
	second, err := NewWorkspaceIndexer(WorkspaceIndexOptions{CachePath: cachePath}).IndexDirContext(t.Context(), dir)
	require.NoError(t, err)
	require.False(t, second.Stats.CacheHit)
	require.Equal(t, 1, second.Stats.FilesReused)
	require.Equal(t, 1, second.Stats.FilesChanged)
	require.Len(t, second.Query(Query{Kind: QueryDefinitions, Name: "alpha", Language: LanguagePython}).Definitions, 1)
	require.Empty(t, second.Query(Query{Kind: QueryDefinitions, Name: "beta", Language: LanguagePython}).Definitions)
	require.Len(t, second.Query(Query{Kind: QueryDefinitions, Name: "changed", Language: LanguagePython}).Definitions, 1)
	require.Len(t, second.Query(Query{Kind: QueryReferences, Name: "os", Language: LanguagePython}).References, 1)

	alphaNode, ok := graphNodeWhere(second.Graph.Nodes(), func(node codegraph.Node) bool {
		return node.Name == "alpha"
	})
	require.True(t, ok)
	assert.Equal(t, "declaration", alphaNode.Kind)

	osImportNode, ok := graphNodeWhere(second.Graph.Nodes(), func(node codegraph.Node) bool {
		return node.Name == "os"
	})
	require.True(t, ok)
	assert.Equal(t, "import", osImportNode.Kind)
}

func TestWorkspaceIndexer_SkipsAttelerStateDirectory(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	writeTextFile(t, dir, "visible.py", `def visible():
	return "visible"
`)
	writeTextFile(t, dir, filepath.Join(".atteler", "generated.py"), `def hidden():
	return "hidden"
`)

	index, err := NewWorkspaceIndexer(WorkspaceIndexOptions{
		CachePath: filepath.Join(dir, ".atteler", "codeintel.json"),
	}).IndexDirContext(t.Context(), dir)
	require.NoError(t, err)
	require.Len(t, index.Query(Query{Kind: QueryDefinitions, Name: "visible", Language: LanguagePython}).Definitions, 1)
	require.Empty(t, index.Query(Query{Kind: QueryDefinitions, Name: "hidden", Language: LanguagePython}).Definitions)
}

func TestWorkspaceIndexer_IndexesNonGoFixture(t *testing.T) {
	t.Parallel()

	root := filepath.Join("testdata", "multilang")
	index, err := NewWorkspaceIndexer(WorkspaceIndexOptions{}).IndexDirContext(t.Context(), root)
	require.NoError(t, err)

	definitions := index.Query(Query{Kind: QueryDefinitions, Language: LanguagePython}).Definitions
	assert.Contains(t, codeDefinitionNames(definitions), "Worker")
	assert.Contains(t, codeDefinitionNames(definitions), "helper")

	references := index.Query(Query{Kind: QueryReferences, Language: LanguagePython}).References
	assert.Contains(t, codeReferenceNames(references), "os")
	assert.Contains(t, codeReferenceNames(references), "pathlib")
	assert.Empty(t, index.Query(Query{Kind: QueryDefinitions, Name: "helper", Language: LanguageGo}).Definitions)
}

func TestModelQuery_NormalizesKindAndLanguage(t *testing.T) {
	t.Parallel()

	model := Model{
		Definitions: []CodeDefinition{{
			ID:       "decl:python:helper",
			Name:     "helper",
			Kind:     "function",
			Language: LanguagePython,
			File:     "worker.py",
		}},
		Relationships: []CodeRelationship{{
			FromID:   "file:worker.py",
			ToID:     "import:python:worker.py:os:1",
			Kind:     "imports",
			Language: LanguagePython,
			File:     "worker.py",
		}},
		Diagnostics: []CodeDiagnostic{{
			ID:       "diagnostic:python:parse:worker.py:1",
			Language: LanguagePython,
			File:     "worker.py",
			Source:   "python:scanner",
			Severity: "parse",
			Message:  "Unable to parse Python function definition",
		}},
	}

	result := model.Query(Query{Kind: QueryKind("Definitions"), Name: "helper", Language: "Python"})
	require.Len(t, result.Definitions, 1)
	assert.Empty(t, result.Uncertainty)

	result = model.Query(Query{Kind: QueryKind("Relationships"), RelationshipKind: "Imports", Language: "Python"})
	require.Len(t, result.Relationships, 1)
	assert.Empty(t, result.Uncertainty)

	result = model.Query(Query{Kind: QueryKind("Diagnostics"), Name: "PYTHON:SCANNER", Language: "Python"})
	require.Len(t, result.Diagnostics, 1)
	assert.Empty(t, result.Uncertainty)

	result = model.Query(Query{Kind: QueryDiagnostics, Name: "Parse", Language: LanguagePython})
	require.Len(t, result.Diagnostics, 1)
	assert.Empty(t, result.Uncertainty)
}

func TestIndexQuery_BuildsModelFallbackFromGoSemanticFields(t *testing.T) {
	t.Parallel()

	graph := codegraph.NewEvidence()
	fileRange := SourceRange{File: "runner.go", StartLine: 1, StartColumn: 1, EndLine: 3, EndColumn: 1}
	definitionRange := SourceRange{File: "runner.go", StartLine: 3, StartColumn: 6, EndLine: 3, EndColumn: 9}
	graph.AddNode(codegraph.Node{ID: "file:runner.go", Kind: "file", Name: "runner.go"})
	graph.AddNode(codegraph.Node{ID: "decl:go:runner:Run", Kind: "declaration", Name: "Run"})
	graph.AddRelationship(codegraph.Relationship{
		From: "file:runner.go",
		To:   "decl:go:runner:Run",
		Kind: "declares",
		Provenance: []codegraph.Provenance{{
			Source:      "go/packages",
			File:        "runner.go",
			StartLine:   3,
			StartColumn: 6,
			EndLine:     3,
			EndColumn:   9,
			Confidence:  "high",
		}},
	})

	index := Index{
		FileDetails: []SourceFile{{
			Path:        "runner.go",
			ContentHash: "hash",
			Size:        42,
			Range:       fileRange,
		}},
		Declarations: []Declaration{{
			ID:       "decl:go:runner:Run",
			Name:     "Run",
			Kind:     kindFunc,
			File:     "runner.go",
			Range:    definitionRange,
			Exported: true,
		}},
		Graph: graph,
	}

	result := index.Query(Query{Kind: QueryDefinitions, Name: "Run", Language: LanguageGo})
	require.Len(t, result.Definitions, 1)
	assert.Equal(t, "Run", result.Definitions[0].Name)
	assert.Empty(t, result.Uncertainty)

	relationships := index.Query(Query{Kind: QueryRelationships, RelationshipKind: "declares", Language: LanguageGo}).Relationships
	require.Len(t, relationships, 1)
	assert.Equal(t, "file:runner.go", relationships[0].FromID)
	assert.Equal(t, "decl:go:runner:Run", relationships[0].ToID)
}

func writeSemanticModule(t *testing.T) string {
	t.Helper()

	dir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(dir, "go.mod"), []byte("module example.com/sem\n\ngo 1.26.2\n"), 0o600))
	writeGoFile(t, dir, "api.go", `package sem

import "context"

type Runner struct{}
type Box[T any] struct{ Value T }
type hidden struct{}

func NewRunner() Runner {
	return Runner{}
}

func (Runner) Run(ctx context.Context) error {
	return nil
}

func (hidden) Visible() {}
func (Box[T]) Get() T { return *new(T) }

func Use(ctx context.Context) {
	_ = NewRunner()
	_ = NewRunner().Run(ctx)
	box := Box[int]{Value: 1}
	_ = box.Get()
}
`)
	writeGoFile(t, dir, "api_test.go", `package sem

import "testing"

func TestUse(t *testing.T) {
	Use(nil)
}
`)
	writeGoFile(t, dir, "tagged.go", `//go:build integration

package sem

func Tagged() {}
`)
	writeGoFile(t, dir, "generated.go", `// Code generated by test. DO NOT EDIT.

package sem

func Generated() {}
`)
	writeGoFile(t, dir, "vendor/ignored.go", `package ignored

func VendorIgnored() {}
`)

	return dir
}

func packageLoaderOptions(t *testing.T) IndexOptions {
	t.Helper()

	return IndexOptions{Env: []string{
		"GOCACHE=" + filepath.Join(t.TempDir(), "gocache"),
		"GOWORK=off",
	}}
}

func declarationNames(declarations []Declaration) []string {
	out := make([]string, 0, len(declarations))
	for i := range declarations {
		out = append(out, declarations[i].Name)
	}

	return out
}

func declarationQualifiedNames(declarations []Declaration) []string {
	out := make([]string, 0, len(declarations))
	for i := range declarations {
		name := declarations[i].Name
		if declarations[i].Receiver != "" {
			name = declarations[i].Receiver + "." + name
		}
		out = append(out, name)
	}

	return out
}

func exportRelationships(edges []codegraph.Relationship) []codegraph.Relationship {
	var out []codegraph.Relationship
	for i := range edges {
		if edges[i].Kind == "exports" {
			out = append(out, edges[i])
		}
	}

	return out
}

func diagnosticsContain(diagnostics []Diagnostic, text string) bool {
	for i := range diagnostics {
		if strings.Contains(diagnostics[i].Message, text) {
			return true
		}
	}

	return false
}

func relationshipKinds(edges []codegraph.Relationship) []string {
	out := make([]string, 0, len(edges))
	for i := range edges {
		out = append(out, edges[i].Kind)
	}

	return out
}

func nodeKinds(nodes []codegraph.Node) []string {
	out := make([]string, 0, len(nodes))
	for i := range nodes {
		out = append(out, nodes[i].Kind)
	}

	return out
}

func callTargets(edges []CallEdge) []string {
	out := make([]string, 0, len(edges))
	for i := range edges {
		out = append(out, edges[i].CalleeID)
	}

	return out
}

func sourceFilesWhere(files []SourceFile, keep func(SourceFile) bool) []SourceFile {
	var out []SourceFile
	for i := range files {
		if keep(files[i]) {
			out = append(out, files[i])
		}
	}

	return out
}

func referencesWhere(references []Reference, keep func(Reference) bool) []Reference {
	var out []Reference
	for i := range references {
		if keep(references[i]) {
			out = append(out, references[i])
		}
	}

	return out
}

func uncertaintyContains(uncertainty []string, text string) bool {
	for i := range uncertainty {
		if strings.Contains(uncertainty[i], text) {
			return true
		}
	}

	return false
}

func codeRelationshipsSorted(relationships []CodeRelationship) bool {
	for i := 1; i < len(relationships); i++ {
		if codeRelationshipLess(relationships[i], relationships[i-1]) {
			return false
		}
	}

	return true
}

func relationshipWhere(relationships []CodeRelationship, keep func(CodeRelationship) bool) []CodeRelationship {
	var out []CodeRelationship
	for i := range relationships {
		if keep(relationships[i]) {
			out = append(out, relationships[i])
		}
	}

	return out
}

func codeDefinitionNames(definitions []CodeDefinition) []string {
	out := make([]string, 0, len(definitions))
	for i := range definitions {
		out = append(out, definitions[i].Name)
	}

	return out
}

func codeReferenceNames(references []CodeReference) []string {
	out := make([]string, 0, len(references))
	for i := range references {
		out = append(out, references[i].Name)
	}

	return out
}

func graphNodeWhere(nodes []codegraph.Node, keep func(codegraph.Node) bool) (codegraph.Node, bool) {
	for i := range nodes {
		if keep(nodes[i]) {
			return nodes[i], true
		}
	}

	return codegraph.Node{}, false
}
