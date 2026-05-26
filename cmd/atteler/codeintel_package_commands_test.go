package main

import (
	"path/filepath"
	"reflect"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/tommoulard/atteler/pkg/codeintel"
)

func TestSummarizeCodePackageSymbolFiles(t *testing.T) {
	t.Parallel()

	root := filepath.Join("tmp", "repo")
	idx := codeintel.Index{Files: []codeintel.File{
		{Path: filepath.Join(root, "pkg", "b.go"), Package: "pkg", Symbols: []codeintel.Symbol{{Name: "B"}}},
		{Path: filepath.Join(root, "cmd", "a.go"), Package: "main", Symbols: []codeintel.Symbol{{Name: "A"}, {Name: "Run"}}},
		{Path: filepath.Join(root, "pkg", "empty.go"), Package: "pkg"},
		{Path: filepath.Join(root, "pkg", "c.go"), Package: "pkg", Symbols: []codeintel.Symbol{{Name: "C"}, {Name: "Build"}}},
	}}
	summaries := summarizeCodePackageSymbolFiles(root, idx, "pkg")

	want := []codeSymbolFileSummary{
		{Path: "pkg/c.go", Package: "pkg", Symbols: 2},
		{Path: "pkg/b.go", Package: "pkg", Symbols: 1},
	}
	if !reflect.DeepEqual(summaries, want) {
		require.Failf(t, "unexpected package symbol file summaries", "got %#v, want %#v", summaries, want)
	}

	if got := summarizeCodePackageSymbolFiles(root, idx, "missing"); len(got) != 0 {
		require.Failf(t, "missing package should return empty", "got %#v", got)
	}

	if got := summarizeCodePackageSymbolFiles(root, idx, " "); got != nil {
		require.Failf(t, "blank package should return nil", "got %#v", got)
	}
}

func TestCodePackageSymbolsByName(t *testing.T) {
	t.Parallel()

	const testPackageName = "core"

	packageName, name, err := parseCodeFileSymbolFilterSpec(testPackageName+":Run", "code package symbol", "package:name")
	if err != nil {
		require.NoError(t, err)
	}

	if packageName != testPackageName || name != "Run" {
		require.Failf(t, "unexpected parsed spec", "package=%q name=%q", packageName, name)
	}

	if _, _, err := parseCodeFileSymbolFilterSpec(testPackageName, "code package symbol", "package:name"); err == nil {
		require.Fail(t, "expected parse error for missing symbol name")
	}

	idx := codeintel.Index{Files: []codeintel.File{
		{Package: testPackageName, Symbols: []codeintel.Symbol{{Name: "Run", Kind: "func", File: "b.go", Line: 2}, {Name: "Client", Kind: "type", File: "a.go", Line: 1}}},
		{Package: "main", Symbols: []codeintel.Symbol{{Name: "Run", Kind: "func", File: "main.go", Line: 1}}},
		{Package: testPackageName, Symbols: []codeintel.Symbol{{Name: "Run", Kind: "method", File: "a.go", Line: 3}, {Name: "Build", Kind: "func", File: "c.go", Line: 4}}},
	}}
	symbols := codePackageSymbolsByName(idx, testPackageName, "Run")

	want := []codeintel.Symbol{
		{Name: "Run", Kind: "method", File: "a.go", Line: 3},
		{Name: "Run", Kind: "func", File: "b.go", Line: 2},
	}
	if !reflect.DeepEqual(symbols, want) {
		require.Failf(t, "unexpected package symbols by name", "got %#v, want %#v", symbols, want)
	}

	if got := codePackageSymbolsByName(idx, testPackageName, " "); got != nil {
		require.Failf(t, "blank symbol name should return nil", "got %#v", got)
	}

	if got := codePackageSymbolsByName(idx, "missing", "Run"); got != nil {
		require.Failf(t, "missing package should return nil", "got %#v", got)
	}
}

func TestSummarizeCodePackageSymbolNameFiles(t *testing.T) {
	t.Parallel()

	root := filepath.Join("tmp", "repo")
	idx := codeintel.Index{Files: []codeintel.File{
		{Path: filepath.Join(root, "pkg", "b.go"), Package: "pkg", Symbols: []codeintel.Symbol{{Name: "Run"}, {Name: "Build"}}},
		{Path: filepath.Join(root, "cmd", "a.go"), Package: "main", Symbols: []codeintel.Symbol{{Name: "Run"}, {Name: "Run"}}},
		{Path: filepath.Join(root, "pkg", "c.go"), Package: "pkg", Symbols: []codeintel.Symbol{{Name: "Run"}, {Name: "Run"}, {Name: "Client"}}},
	}}
	summaries := summarizeCodePackageSymbolNameFiles(root, idx, "pkg", "Run")

	want := []codeSymbolFileSummary{
		{Path: "pkg/c.go", Package: "pkg", Symbols: 2},
		{Path: "pkg/b.go", Package: "pkg", Symbols: 1},
	}
	if !reflect.DeepEqual(summaries, want) {
		require.Failf(t, "unexpected package symbol name file summaries", "got %#v, want %#v", summaries, want)
	}

	if got := summarizeCodePackageSymbolNameFiles(root, idx, "pkg", "missing"); len(got) != 0 {
		require.Failf(t, "missing name should return empty", "got %#v", got)
	}

	if got := summarizeCodePackageSymbolNameFiles(root, idx, "pkg", " "); got != nil {
		require.Failf(t, "blank name should return nil", "got %#v", got)
	}

	if got := summarizeCodePackageSymbolNameFiles(root, idx, "missing", "Run"); len(got) != 0 {
		require.Failf(t, "missing package should return empty", "got %#v", got)
	}

	if got := summarizeCodePackageSymbolNameFiles(root, idx, " ", "Run"); got != nil {
		require.Failf(t, "blank package should return nil", "got %#v", got)
	}
}

func TestCodePackageSymbolsByKindAndParseSpec(t *testing.T) {
	t.Parallel()

	packageName, kind, err := parseCodePackageSymbolKindSpec("llm:func")
	if err != nil {
		require.NoError(t, err)
	}

	if packageName != "llm" || kind != "func" {
		require.Failf(t, "unexpected parsed spec", "package=%q kind=%q", packageName, kind)
	}

	if _, _, err := parseCodePackageSymbolKindSpec("llm"); err == nil {
		require.Fail(t, "expected parse error for missing kind")
	}

	idx := codeintel.Index{Files: []codeintel.File{
		{Package: "llm", Symbols: []codeintel.Symbol{{Name: "Run", Kind: "func", File: "b.go", Line: 2}, {Name: "Client", Kind: "type", File: "a.go", Line: 1}}},
		{Package: "llm", Symbols: []codeintel.Symbol{{Name: "Build", Kind: "func", File: "c.go", Line: 3}}},
	}}
	symbols := codePackageSymbolsByKind(idx, "llm", "FUNC")

	want := []codeintel.Symbol{
		{Name: "Build", Kind: "func", File: "c.go", Line: 3},
		{Name: "Run", Kind: "func", File: "b.go", Line: 2},
	}
	if !reflect.DeepEqual(symbols, want) {
		require.Failf(t, "unexpected package symbols by kind", "got %#v, want %#v", symbols, want)
	}
}

func TestSummarizeCodePackageSymbolPrefixFiles(t *testing.T) {
	t.Parallel()

	root := filepath.Join("tmp", "repo")
	idx := codeintel.Index{Files: []codeintel.File{
		{Path: filepath.Join(root, "pkg", "b.go"), Package: "pkg", Symbols: []codeintel.Symbol{{Name: "Run"}, {Name: "Build"}}},
		{Path: filepath.Join(root, "cmd", "a.go"), Package: "main", Symbols: []codeintel.Symbol{{Name: "Render"}, {Name: "Run"}}},
		{Path: filepath.Join(root, "pkg", "c.go"), Package: "pkg", Symbols: []codeintel.Symbol{{Name: "Render"}, {Name: "Run"}, {Name: "Client"}}},
	}}
	summaries := summarizeCodePackageSymbolPrefixFiles(root, idx, "pkg", "R")

	want := []codeSymbolFileSummary{
		{Path: "pkg/c.go", Package: "pkg", Symbols: 2},
		{Path: "pkg/b.go", Package: "pkg", Symbols: 1},
	}
	if !reflect.DeepEqual(summaries, want) {
		require.Failf(t, "unexpected package symbol prefix file summaries", "got %#v, want %#v", summaries, want)
	}

	if got := summarizeCodePackageSymbolPrefixFiles(root, idx, "pkg", "missing"); len(got) != 0 {
		require.Failf(t, "missing prefix should return empty", "got %#v", got)
	}

	if got := summarizeCodePackageSymbolPrefixFiles(root, idx, "pkg", " "); got != nil {
		require.Failf(t, "blank prefix should return nil", "got %#v", got)
	}

	if got := summarizeCodePackageSymbolPrefixFiles(root, idx, "missing", "R"); len(got) != 0 {
		require.Failf(t, "missing package should return empty", "got %#v", got)
	}
}

func TestSummarizeCodePackageSymbolKindFiles(t *testing.T) {
	t.Parallel()

	root := filepath.Join("tmp", "repo")
	idx := codeintel.Index{Files: []codeintel.File{
		{Path: filepath.Join(root, "pkg", "b.go"), Package: "pkg", Symbols: []codeintel.Symbol{{Kind: "func"}, {Kind: "type"}}},
		{Path: filepath.Join(root, "cmd", "a.go"), Package: "main", Symbols: []codeintel.Symbol{{Kind: "func"}, {Kind: "FUNC"}}},
		{Path: filepath.Join(root, "pkg", "c.go"), Package: "pkg", Symbols: []codeintel.Symbol{{Kind: "func"}, {Kind: "FUNC"}, {Kind: "const"}}},
	}}
	summaries := summarizeCodePackageSymbolKindFiles(root, idx, "pkg", "func")

	want := []codeSymbolFileSummary{
		{Path: "pkg/c.go", Package: "pkg", Symbols: 2},
		{Path: "pkg/b.go", Package: "pkg", Symbols: 1},
	}
	if !reflect.DeepEqual(summaries, want) {
		require.Failf(t, "unexpected package symbol kind file summaries", "got %#v, want %#v", summaries, want)
	}

	if got := summarizeCodePackageSymbolKindFiles(root, idx, "pkg", "missing"); len(got) != 0 {
		require.Failf(t, "missing kind should return empty", "got %#v", got)
	}

	if got := summarizeCodePackageSymbolKindFiles(root, idx, "pkg", " "); got != nil {
		require.Failf(t, "blank kind should return nil", "got %#v", got)
	}

	if got := summarizeCodePackageSymbolKindFiles(root, idx, "missing", "func"); len(got) != 0 {
		require.Failf(t, "missing package should return empty", "got %#v", got)
	}
}

func TestCodePackageSymbolsWithPrefix(t *testing.T) {
	t.Parallel()

	packageName, prefix, err := parseCodeFileSymbolFilterSpec("llm:Ru", "code package symbol prefix", "package:prefix")
	if err != nil {
		require.NoError(t, err)
	}

	if packageName != "llm" || prefix != "Ru" {
		require.Failf(t, "unexpected parsed spec", "package=%q prefix=%q", packageName, prefix)
	}

	if _, _, err := parseCodeFileSymbolFilterSpec("llm", "code package symbol prefix", "package:prefix"); err == nil {
		require.Fail(t, "expected parse error for missing prefix")
	}

	idx := codeintel.Index{Files: []codeintel.File{
		{Package: "llm", Symbols: []codeintel.Symbol{{Name: "Run", File: "b.go", Line: 2}, {Name: "Client", File: "a.go", Line: 1}}},
		{Package: "main", Symbols: []codeintel.Symbol{{Name: "Main", File: "main.go", Line: 1}}},
		{Package: "llm", Symbols: []codeintel.Symbol{{Name: "Runtime", File: "c.go", Line: 3}, {Name: "Build", File: "c.go", Line: 4}}},
	}}
	symbols := codePackageSymbolsWithPrefix(idx, "llm", "Ru")

	want := []codeintel.Symbol{
		{Name: "Run", File: "b.go", Line: 2},
		{Name: "Runtime", File: "c.go", Line: 3},
	}
	if !reflect.DeepEqual(symbols, want) {
		require.Failf(t, "unexpected package symbols by prefix", "got %#v, want %#v", symbols, want)
	}

	if got := codePackageSymbolsWithPrefix(idx, "llm", " "); got != nil {
		require.Failf(t, "blank prefix should return nil", "got %#v", got)
	}

	if got := codePackageSymbolsWithPrefix(idx, "missing", "Ru"); got != nil {
		require.Failf(t, "missing package should return nil", "got %#v", got)
	}
}

func TestSummarizeAndFormatCodePackageImportCounts(t *testing.T) {
	t.Parallel()

	idx := codeintel.Index{Files: []codeintel.File{
		{Package: "pkg", Imports: []string{"fmt"}},
		{Package: "main", Imports: []string{"context", "fmt"}},
		{Package: "pkg"},
		{Package: "pkg", Imports: []string{"bytes", "fmt"}},
		{Package: "empty"},
	}}
	summaries := summarizeCodePackageImportCounts(idx)

	want := []codePackageImportSummary{
		{Name: "pkg", Files: 3, Imports: 3, UniqueImports: 2},
		{Name: "main", Files: 1, Imports: 2, UniqueImports: 2},
	}
	if !reflect.DeepEqual(summaries, want) {
		require.Failf(t, "unexpected package import summaries", "got %#v, want %#v", summaries, want)
	}

	got := formatCodeIntelPackageImportSummary(codeIntelPackagesFromImportSummaries(summaries)[0])
	if got != "package=pkg	files=3	imports=3	unique_imports=2" {
		require.Failf(t, "unexpected package import summary format", "got %q", got)
	}
}

func TestCodePackageSymbols(t *testing.T) {
	t.Parallel()

	idx := codeintel.Index{Files: []codeintel.File{
		{Package: "llm", Symbols: []codeintel.Symbol{{Name: "Run", File: "b.go", Line: 2}, {Name: "Client", File: "a.go", Line: 1}}},
		{Package: "main", Symbols: []codeintel.Symbol{{Name: "Main", File: "main.go", Line: 1}}},
		{Package: "llm", Symbols: []codeintel.Symbol{{Name: "Build", File: "c.go", Line: 3}}},
	}}
	symbols := codePackageSymbols(idx, "llm")

	want := []codeintel.Symbol{
		{Name: "Build", File: "c.go", Line: 3},
		{Name: "Client", File: "a.go", Line: 1},
		{Name: "Run", File: "b.go", Line: 2},
	}
	if !reflect.DeepEqual(symbols, want) {
		require.Failf(t, "unexpected package symbols", "got %#v, want %#v", symbols, want)
	}

	if got := codePackageSymbols(idx, " "); got != nil {
		require.Failf(t, "blank package should return nil", "got %#v", got)
	}
}

func TestSummarizeCodePackageSymbols(t *testing.T) {
	t.Parallel()

	idx := codeintel.Index{Files: []codeintel.File{
		{Package: "llm", Symbols: []codeintel.Symbol{{Kind: "func"}, {Kind: "type"}}},
		{Package: "main", Symbols: []codeintel.Symbol{{Kind: "func"}}},
		{Package: "llm", Symbols: []codeintel.Symbol{{Kind: "func"}, {Kind: "const"}}},
	}}
	summaries := summarizeCodePackageSymbols(idx, "llm")

	want := []codeSymbolSummary{{Kind: "func", Count: 2}, {Kind: "const", Count: 1}, {Kind: "type", Count: 1}}
	if !reflect.DeepEqual(summaries, want) {
		require.Failf(t, "unexpected package symbol summaries", "got %#v, want %#v", summaries, want)
	}

	if got := summarizeCodePackageSymbols(idx, "missing"); len(got) != 0 {
		require.Failf(t, "missing package should return empty", "got %#v", got)
	}
}

func TestSummarizeCodePackageImportFiles(t *testing.T) {
	t.Parallel()

	root := filepath.Join("tmp", "repo")
	idx := codeintel.Index{Files: []codeintel.File{
		{Path: filepath.Join(root, "pkg", "b.go"), Package: "pkg", Imports: []string{"fmt"}},
		{Path: filepath.Join(root, "cmd", "a.go"), Package: "main", Imports: []string{"context", "fmt"}},
		{Path: filepath.Join(root, "pkg", "empty.go"), Package: "pkg"},
		{Path: filepath.Join(root, "pkg", "c.go"), Package: "pkg", Imports: []string{"bytes", "errors"}},
	}}
	summaries := summarizeCodePackageImportFiles(root, idx, "pkg")

	want := []codeImportFileSummary{
		{Path: "pkg/c.go", Package: "pkg", Imports: 2},
		{Path: "pkg/b.go", Package: "pkg", Imports: 1},
	}
	if !reflect.DeepEqual(summaries, want) {
		require.Failf(t, "unexpected package import file summaries", "got %#v, want %#v", summaries, want)
	}

	if got := summarizeCodePackageImportFiles(root, idx, "missing"); len(got) != 0 {
		require.Failf(t, "missing package should return empty", "got %#v", got)
	}

	if got := summarizeCodePackageImportFiles(root, idx, " "); got != nil {
		require.Failf(t, "blank package should return nil", "got %#v", got)
	}
}

func TestSummarizeCodePackageImportPrefixFiles(t *testing.T) {
	t.Parallel()

	const testImportPackageName = "core"

	root := filepath.Join("tmp", "repo")
	idx := codeintel.Index{
		Files: []codeintel.File{
			{Path: filepath.Join(root, "pkg", "core", "b.go"), Package: testImportPackageName},
			{Path: filepath.Join(root, "pkg", "core", "a.go"), Package: testImportPackageName},
			{Path: filepath.Join(root, "cmd", "main.go"), Package: "main"},
		},
		ImportEdges: []codeintel.ImportEdge{
			{From: filepath.Join(root, "pkg", "core", "b.go"), Import: "github.com/example/beta"},
			{From: filepath.Join(root, "pkg", "core", "b.go"), Import: "github.com/example/beta"},
			{From: filepath.Join(root, "pkg", "core", "a.go"), Import: "github.com/example/alpha"},
			{From: filepath.Join(root, "pkg", "core", "a.go"), Import: "github.com/example/beta"},
			{From: filepath.Join(root, "cmd", "main.go"), Import: "github.com/example/alpha"},
			{From: filepath.Join(root, "pkg", "core", "a.go"), Import: "context"},
		},
	}
	summaries := summarizeCodePackageImportPrefixFiles(root, idx, testImportPackageName, "github.com/example/")

	want := []codeImportFileSummary{
		{Path: "pkg/core/a.go", Package: testImportPackageName, Imports: 2},
		{Path: "pkg/core/b.go", Package: testImportPackageName, Imports: 1},
	}
	if !reflect.DeepEqual(summaries, want) {
		require.Failf(t, "unexpected package import prefix file summaries", "got %#v, want %#v", summaries, want)
	}

	if got := summarizeCodePackageImportPrefixFiles(root, idx, testImportPackageName, "missing"); len(got) != 0 {
		require.Failf(t, "missing prefix should return empty", "got %#v", got)
	}

	if got := summarizeCodePackageImportPrefixFiles(root, idx, testImportPackageName, " "); got != nil {
		require.Failf(t, "blank prefix should return nil", "got %#v", got)
	}
}

func TestCodePackageImportPrefixFiles(t *testing.T) {
	t.Parallel()

	const testImportPackageName = "core"

	root := filepath.Join("tmp", "repo")

	packageName, prefix, err := parseCodeFileSymbolFilterSpec(testImportPackageName+":github.com/example/", "code package import prefix files", "package:prefix")
	if err != nil {
		require.NoError(t, err)
	}

	if packageName != testImportPackageName || prefix != "github.com/example/" {
		require.Failf(t, "unexpected parsed spec", "package=%q prefix=%q", packageName, prefix)
	}

	if _, _, err := parseCodeFileSymbolFilterSpec(testImportPackageName, "code package import prefix files", "package:prefix"); err == nil {
		require.Fail(t, "expected parse error for missing prefix")
	}

	idx := codeintel.Index{
		Files: []codeintel.File{
			{Path: filepath.Join(root, "pkg", "core", "b.go"), Package: testImportPackageName},
			{Path: filepath.Join(root, "pkg", "core", "a.go"), Package: testImportPackageName},
			{Path: filepath.Join(root, "cmd", "main.go"), Package: "main"},
		},
		ImportEdges: []codeintel.ImportEdge{
			{From: filepath.Join(root, "pkg", "core", "b.go"), Import: "github.com/example/beta"},
			{From: filepath.Join(root, "pkg", "core", "b.go"), Import: "github.com/example/beta"},
			{From: filepath.Join(root, "pkg", "core", "a.go"), Import: "github.com/example/alpha"},
			{From: filepath.Join(root, "pkg", "core", "a.go"), Import: "github.com/example/beta"},
			{From: filepath.Join(root, "cmd", "main.go"), Import: "github.com/example/alpha"},
			{From: filepath.Join(root, "pkg", "core", "a.go"), Import: "context"},
		},
	}
	edges := codePackageImportPrefixFiles(idx, testImportPackageName, "github.com/example/")

	want := []codeintel.ImportEdge{
		{From: filepath.Join(root, "pkg", "core", "a.go"), Import: "github.com/example/alpha"},
		{From: filepath.Join(root, "pkg", "core", "a.go"), Import: "github.com/example/beta"},
		{From: filepath.Join(root, "pkg", "core", "b.go"), Import: "github.com/example/beta"},
	}
	if !reflect.DeepEqual(edges, want) {
		require.Failf(t, "unexpected package import prefix files", "got %#v, want %#v", edges, want)
	}

	if got := codePackageImportPrefixFiles(idx, testImportPackageName, "missing"); len(got) != 0 {
		require.Failf(t, "missing prefix should return empty", "got %#v", got)
	}

	if got := codePackageImportPrefixFiles(idx, testImportPackageName, " "); got != nil {
		require.Failf(t, "blank prefix should return nil", "got %#v", got)
	}
}

func TestCodePackageImportFiles(t *testing.T) {
	t.Parallel()

	root := filepath.Join("tmp", "repo")

	packageName, importPath, err := parseCodeFileSymbolFilterSpec("core:"+testContextImport, "code package import files", "package:import")
	if err != nil {
		require.NoError(t, err)
	}

	if packageName != "core" || importPath != testContextImport {
		require.Failf(t, "unexpected parsed spec", "package=%q import=%q", packageName, importPath)
	}

	if _, _, err := parseCodeFileSymbolFilterSpec("core", "code package import files", "package:import"); err == nil {
		require.Fail(t, "expected parse error for missing import path")
	}

	idx := codeintel.Index{
		Files: []codeintel.File{
			{Path: filepath.Join(root, "pkg", "core", "b.go"), Package: "core"},
			{Path: filepath.Join(root, "pkg", "core", "a.go"), Package: "core"},
			{Path: filepath.Join(root, "cmd", "main.go"), Package: "main"},
		},
		ImportEdges: []codeintel.ImportEdge{
			{From: filepath.Join(root, "pkg", "core", "b.go"), Import: "context"},
			{From: filepath.Join(root, "pkg", "core", "b.go"), Import: "context"},
			{From: filepath.Join(root, "pkg", "core", "a.go"), Import: "fmt"},
			{From: filepath.Join(root, "cmd", "main.go"), Import: "context"},
		},
	}
	files := codePackageImportFiles(root, idx, "core", "context")

	want := []string{"pkg/core/b.go"}
	if !reflect.DeepEqual(files, want) {
		require.Failf(t, "unexpected package import files", "got %#v, want %#v", files, want)
	}

	if got := codePackageImportFiles(root, idx, "core", "missing"); len(got) != 0 {
		require.Failf(t, "missing import should return empty", "got %#v", got)
	}

	if got := codePackageImportFiles(root, idx, "core", " "); got != nil {
		require.Failf(t, "blank import path should return nil", "got %#v", got)
	}
}

func TestSummarizeCodePackageImportPathFiles(t *testing.T) {
	t.Parallel()

	root := filepath.Join("tmp", "repo")
	idx := codeintel.Index{
		Files: []codeintel.File{
			{Path: filepath.Join(root, "pkg", "core", "b.go"), Package: "core"},
			{Path: filepath.Join(root, "pkg", "core", "a.go"), Package: "core"},
			{Path: filepath.Join(root, "cmd", "main.go"), Package: "main"},
		},
		ImportEdges: []codeintel.ImportEdge{
			{From: filepath.Join(root, "pkg", "core", "b.go"), Import: "context"},
			{From: filepath.Join(root, "pkg", "core", "b.go"), Import: "context"},
			{From: filepath.Join(root, "pkg", "core", "a.go"), Import: "fmt"},
			{From: filepath.Join(root, "cmd", "main.go"), Import: "context"},
		},
	}
	summaries := summarizeCodePackageImportPathFiles(root, idx, "core", "context")

	want := []codeImportFileSummary{{Path: "pkg/core/b.go", Package: "core", Imports: 1}}
	if !reflect.DeepEqual(summaries, want) {
		require.Failf(t, "unexpected package import path file summaries", "got %#v, want %#v", summaries, want)
	}

	if got := summarizeCodePackageImportPathFiles(root, idx, "core", "missing"); len(got) != 0 {
		require.Failf(t, "missing import should return empty", "got %#v", got)
	}

	if got := summarizeCodePackageImportPathFiles(root, idx, "core", " "); got != nil {
		require.Failf(t, "blank import path should return nil", "got %#v", got)
	}

	if got := summarizeCodePackageImportPathFiles(root, idx, "missing", "context"); len(got) != 0 {
		require.Failf(t, "missing package should return empty", "got %#v", got)
	}
}

func TestSummarizeCodePackageImportPath(t *testing.T) {
	t.Parallel()

	packageName, importPath, err := parseCodeFileSymbolFilterSpec("core:context", "code package import path", "package:import")
	if err != nil {
		require.NoError(t, err)
	}

	if packageName != "core" || importPath != "context" {
		require.Failf(t, "unexpected parsed spec", "package=%q import=%q", packageName, importPath)
	}

	if _, _, err := parseCodeFileSymbolFilterSpec("core", "code package import path", "package:import"); err == nil {
		require.Fail(t, "expected parse error for missing import path")
	}

	idx := codeintel.Index{
		Files: []codeintel.File{
			{Path: "pkg/core/a.go", Package: "core"},
			{Path: "pkg/core/b.go", Package: "core"},
			{Path: "cmd/main.go", Package: "main"},
		},
		ImportEdges: []codeintel.ImportEdge{
			{From: "pkg/core/a.go", Import: "context"},
			{From: "pkg/core/b.go", Import: "context"},
			{From: "pkg/core/b.go", Import: "fmt"},
			{From: "cmd/main.go", Import: "context"},
		},
	}
	summaries := summarizeCodePackageImportPath(idx, "core", "context")

	want := []codeImportSummary{{Path: "context", Files: 2}}
	if !reflect.DeepEqual(summaries, want) {
		require.Failf(t, "unexpected package import path summaries", "got %#v, want %#v", summaries, want)
	}

	if got := summarizeCodePackageImportPath(idx, "core", "missing"); len(got) != 0 {
		require.Failf(t, "missing import should return empty", "got %#v", got)
	}

	if got := summarizeCodePackageImportPath(idx, "core", " "); got != nil {
		require.Failf(t, "blank import path should return nil", "got %#v", got)
	}

	if got := summarizeCodePackageImportPath(idx, "missing", "context"); len(got) != 0 {
		require.Failf(t, "missing package should return empty", "got %#v", got)
	}
}

func TestSummarizeCodePackageImportPrefix(t *testing.T) {
	t.Parallel()

	idx := codeintel.Index{
		Files: []codeintel.File{
			{Path: "pkg/llm/a.go", Package: "llm"},
			{Path: "pkg/llm/b.go", Package: "llm"},
			{Path: "cmd/main.go", Package: "main"},
		},
		ImportEdges: []codeintel.ImportEdge{
			{From: "pkg/llm/a.go", Import: "github.com/tommoulard/atteler/pkg/events"},
			{From: "pkg/llm/b.go", Import: "github.com/tommoulard/atteler/pkg/events"},
			{From: "pkg/llm/b.go", Import: "context"},
			{From: "cmd/main.go", Import: "github.com/tommoulard/atteler/pkg/llm"},
		},
	}
	summaries := summarizeCodePackageImportPrefix(idx, "llm", "github.com/tommoulard/atteler/pkg/")

	want := []codeImportSummary{{Path: "github.com/tommoulard/atteler/pkg/events", Files: 2}}
	if !reflect.DeepEqual(summaries, want) {
		require.Failf(t, "unexpected package import prefix summaries", "got %#v, want %#v", summaries, want)
	}

	if got := summarizeCodePackageImportPrefix(idx, "llm", " "); got != nil {
		require.Failf(t, "blank prefix should return nil", "got %#v", got)
	}
}

func TestSummarizeCodePackageImports(t *testing.T) {
	t.Parallel()

	idx := codeintel.Index{
		Files: []codeintel.File{
			{Path: "pkg/llm/a.go", Package: "llm"},
			{Path: "pkg/llm/b.go", Package: "llm"},
			{Path: "cmd/main.go", Package: "main"},
		},
		ImportEdges: []codeintel.ImportEdge{
			{From: "pkg/llm/a.go", Import: "context"},
			{From: "pkg/llm/a.go", Import: "fmt"},
			{From: "pkg/llm/b.go", Import: "context"},
			{From: "cmd/main.go", Import: "context"},
		},
	}
	summaries := summarizeCodePackageImports(idx, "llm")

	want := []codeImportSummary{{Path: "context", Files: 2}, {Path: "fmt", Files: 1}}
	if !reflect.DeepEqual(summaries, want) {
		require.Failf(t, "unexpected package import summaries", "got %#v, want %#v", summaries, want)
	}

	if got := summarizeCodePackageImports(idx, "missing"); got != nil {
		require.Failf(t, "missing package should return nil", "got %#v", got)
	}
}
