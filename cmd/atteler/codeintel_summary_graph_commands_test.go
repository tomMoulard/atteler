package main

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/tommoulard/atteler/pkg/codegraph"
	"github.com/tommoulard/atteler/pkg/codeintel"
)

func TestSummarizeCodeFiles(t *testing.T) {
	t.Parallel()

	root := filepath.Join("tmp", "repo")
	idx := codeintel.Index{Files: []codeintel.File{
		{Path: filepath.Join(root, "pkg", "b.go"), Package: "pkg", Imports: []string{"fmt"}, Symbols: []codeintel.Symbol{{Name: "B"}}},
		{Path: filepath.Join(root, "cmd", "a.go"), Package: "main", Imports: []string{"context", "fmt"}, Symbols: []codeintel.Symbol{{Name: "A"}, {Name: "Run"}}},
	}}
	files := summarizeCodeFiles(root, idx)

	want := []codePackageFile{
		{Path: "cmd/a.go", Package: "main", Symbols: 2, Imports: 2},
		{Path: "pkg/b.go", Package: "pkg", Symbols: 1, Imports: 1},
	}
	if !reflect.DeepEqual(files, want) {
		require.Failf(t, "unexpected code file summaries", "got %#v, want %#v", files, want)
	}
}

func TestFindCodeFile(t *testing.T) {
	t.Parallel()

	root := filepath.Join("tmp", "repo")
	file := codeintel.File{
		Path:    filepath.Join(root, "pkg", "llm", "client.go"),
		Package: "llm",
		Imports: []string{"context", "fmt"},
		Symbols: []codeintel.Symbol{{Name: "Client", Kind: "type", Line: 12}},
	}
	idx := codeintel.Index{Files: []codeintel.File{file}}

	found, ok := findCodeFile(root, idx, "pkg/llm/client.go")
	if !ok || found.Path != file.Path {
		require.Failf(t, "expected to find code file", "found=%#v ok=%v", found, ok)
	}
}

func TestSummarizeAndFormatCodePackageFiles(t *testing.T) {
	t.Parallel()

	root := filepath.Join("tmp", "repo")
	idx := codeintel.Index{Files: []codeintel.File{
		{Path: filepath.Join(root, "pkg", "llm", "a.go"), Package: "llm", Symbols: []codeintel.Symbol{{Name: "A"}}, Imports: []string{"context", "fmt"}},
		{Path: filepath.Join(root, "pkg", "main", "main.go"), Package: "main", Symbols: []codeintel.Symbol{{Name: "Main"}}},
		{Path: filepath.Join(root, "pkg", "llm", "b.go"), Package: "llm", Symbols: []codeintel.Symbol{{Name: "B"}, {Name: "C"}}, Imports: []string{"errors"}},
	}}
	files := summarizeCodePackageFiles(root, idx, "llm")

	wantFiles := []codePackageFile{
		{Path: "pkg/llm/a.go", Package: "llm", Symbols: 1, Imports: 2},
		{Path: "pkg/llm/b.go", Package: "llm", Symbols: 2, Imports: 1},
	}
	if !reflect.DeepEqual(files, wantFiles) {
		require.Failf(t, "unexpected package files", "got %#v, want %#v", files, wantFiles)
	}

	got := formatCodeIntelFile(codeIntelFilesFromPackageFiles(files)[0])

	want := "path=pkg/llm/a.go	package=llm	symbols=1	imports=2"
	if got != want {
		require.Failf(t, "unexpected package file format", "got %q, want %q", got, want)
	}
}

func TestSummarizeAndFormatCodePackages(t *testing.T) {
	t.Parallel()

	packages := summarizeCodePackages(codeintel.Index{Files: []codeintel.File{
		{Package: "main", Symbols: []codeintel.Symbol{{Name: "Run"}, {Name: "Stop"}}},
		{Package: "llm", Symbols: []codeintel.Symbol{{Name: "Client"}}},
		{Package: "main", Symbols: []codeintel.Symbol{{Name: "Config"}}},
		{Package: ""},
	}})

	wantPackages := []codePackageSummary{
		{Name: "llm", Files: 1, Symbols: 1},
		{Name: "main", Files: 2, Symbols: 3},
	}
	if !reflect.DeepEqual(packages, wantPackages) {
		require.Failf(t, "unexpected package summaries", "got %#v, want %#v", packages, wantPackages)
	}

	got := formatCodeIntelPackage(codeIntelPackagesFromSummaries(packages)[1])

	want := "package=main	files=2	symbols=3"
	if got != want {
		require.Failf(t, "unexpected package summary format", "got %q, want %q", got, want)
	}
}

func TestFormatCodeSummary(t *testing.T) {
	t.Parallel()

	got := formatCodeIntelSummary(codeIntelSummary{
		Files:    3,
		Packages: 2,
		Symbols:  7,
		Imports:  5,
		Nodes:    6,
		Edges:    5,
		Cycles:   1,
		Layers:   4,
	})

	want := "files=3	packages=2	symbols=7	imports=5	nodes=6	edges=5	cycles=1	layers=4"
	if got != want {
		require.Failf(t, "unexpected code summary format", "got %q, want %q", got, want)
	}
}

func TestCountPackages(t *testing.T) {
	t.Parallel()

	got := countPackages([]codeintel.File{{Package: "main"}, {Package: "main"}, {Package: "llm"}, {Package: ""}})
	if got != 2 {
		require.Failf(t, "unexpected package count", "got %d", got)
	}
}

func TestFormatCodeCycle(t *testing.T) {
	t.Parallel()

	got := formatCodeIntelCycle(codeIntelCyclesFromGraph([][]codegraph.NodeID{{"pkg/a", "pkg/b", "pkg/a"}})[0])

	want := "cycle=1	nodes=pkg/a -> pkg/b -> pkg/a"
	if got != want {
		require.Failf(t, "unexpected code cycle format", "got %q, want %q", got, want)
	}
}

func TestFormatCodeLayer(t *testing.T) {
	t.Parallel()

	got := formatCodeIntelLayer(codeIntelLayer{Index: 2, Nodes: []string{"pkg/a", "pkg/b"}})

	want := "layer=2	nodes=pkg/a,pkg/b"
	if got != want {
		require.Failf(t, "unexpected code layer format", "got %q, want %q", got, want)
	}
}

func TestCodeGraphDirectDependencies(t *testing.T) {
	t.Parallel()

	root := filepath.Join("tmp", "repo")
	idx := codeintel.Index{
		Files: []codeintel.File{
			{Path: filepath.Join(root, "pkg", "runner.go")},
			{Path: filepath.Join(root, "pkg", "worker.go")},
		},
		ImportEdges: []codeintel.ImportEdge{
			{From: filepath.Join(root, "pkg", "runner.go"), Import: "context"},
			{From: filepath.Join(root, "pkg", "runner.go"), Import: "fmt"},
			{From: filepath.Join(root, "pkg", "worker.go"), Import: "context"},
		},
	}
	graph := importGraphFromIndex(root, idx)

	deps := codeGraphDependencies(graph, root, "pkg/runner.go")

	wantDeps := []codegraph.NodeID{"context", "fmt"}
	if !reflect.DeepEqual(deps, wantDeps) {
		require.Failf(t, "unexpected direct deps", "got %#v, want %#v", deps, wantDeps)
	}

	rdeps := codeGraphReverseDependencies(graph, root, "context")

	wantRdeps := []codegraph.NodeID{"pkg/runner.go", "pkg/worker.go"}
	if !reflect.DeepEqual(rdeps, wantRdeps) {
		require.Failf(t, "unexpected direct reverse deps", "got %#v, want %#v", rdeps, wantRdeps)
	}
}

func TestImportGraphReachableAndNormalizeTarget(t *testing.T) {
	t.Parallel()

	root := t.TempDir()

	file := filepath.Join(root, "pkg", "runner.go")
	if err := os.MkdirAll(filepath.Dir(file), 0o750); err != nil {
		require.NoError(t, err)
	}

	if err := os.WriteFile(file, []byte("package runner\nimport \"context\"\n"), 0o600); err != nil {
		require.NoError(t, err)
	}

	idx, err := codeintel.IndexDir(root)
	require.NoError(t, err)

	graph := importGraphFromIndex(root, idx)

	if got := normalizeCodeGraphTarget(root, file); got != "pkg/runner.go" {
		require.Failf(t, "unexpected normalized absolute target", "got %q", got)
	}

	if got := graph.ReachableFrom("pkg/runner.go"); !reflect.DeepEqual(got, []codegraph.NodeID{"context"}) {
		require.Failf(t, "unexpected reachable nodes", "got %#v", got)
	}
}
