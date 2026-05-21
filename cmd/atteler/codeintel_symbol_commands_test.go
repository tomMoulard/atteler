package main

import (
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/tommoulard/atteler/pkg/codeintel"
)

func TestSummarizeAndFormatCodeSymbolFiles(t *testing.T) {
	t.Parallel()

	root := filepath.Join("tmp", "repo")
	idx := codeintel.Index{Files: []codeintel.File{
		{Path: filepath.Join(root, "pkg", "b.go"), Package: "pkg", Symbols: []codeintel.Symbol{{Name: "B"}}},
		{Path: filepath.Join(root, "cmd", "a.go"), Package: "main", Symbols: []codeintel.Symbol{{Name: "A"}, {Name: "Run"}}},
		{Path: filepath.Join(root, "pkg", "empty.go"), Package: "pkg"},
		{Path: filepath.Join(root, "pkg", "c.go"), Package: "pkg", Symbols: []codeintel.Symbol{{Name: "C"}, {Name: "Build"}}},
	}}
	summaries := summarizeCodeSymbolFiles(root, idx)

	want := []codeSymbolFileSummary{
		{Path: "cmd/a.go", Package: "main", Symbols: 2},
		{Path: "pkg/c.go", Package: "pkg", Symbols: 2},
		{Path: "pkg/b.go", Package: "pkg", Symbols: 1},
	}
	if !reflect.DeepEqual(summaries, want) {
		require.Failf(t, "unexpected symbol file summaries", "got %#v, want %#v", summaries, want)
	}

	got := formatCodeSymbolFileSummary(summaries[0])
	if got != "path=cmd/a.go	package=main	symbols=2" {
		require.Failf(t, "unexpected symbol file summary format", "got %q", got)
	}
}

func TestSummarizeAndFormatCodeSymbols(t *testing.T) {
	t.Parallel()

	idx := codeintel.Index{Symbols: []codeintel.Symbol{
		{Kind: "func"},
		{Kind: "type"},
		{Kind: "func"},
		{Kind: "const"},
		{Kind: ""},
	}}
	summaries := summarizeCodeSymbols(idx)

	want := []codeSymbolSummary{{Kind: "func", Count: 2}, {Kind: "const", Count: 1}, {Kind: "type", Count: 1}}
	if !reflect.DeepEqual(summaries, want) {
		require.Failf(t, "unexpected symbol summaries", "got %#v, want %#v", summaries, want)
	}

	got := formatCodeSymbolSummary(summaries[0])
	if got != "kind=func	symbols=2" {
		require.Failf(t, "unexpected symbol summary format", "got %q", got)
	}
}

func TestSummarizeCodeSymbolKindFiles(t *testing.T) {
	t.Parallel()

	root := filepath.Join("tmp", "repo")
	idx := codeintel.Index{Files: []codeintel.File{
		{Path: filepath.Join(root, "pkg", "b.go"), Package: "pkg", Symbols: []codeintel.Symbol{{Kind: "func"}, {Kind: "type"}}},
		{Path: filepath.Join(root, "cmd", "a.go"), Package: "main", Symbols: []codeintel.Symbol{{Kind: "func"}, {Kind: "FUNC"}}},
		{Path: filepath.Join(root, "pkg", "c.go"), Package: "pkg", Symbols: []codeintel.Symbol{{Kind: "const"}}},
	}}
	summaries := summarizeCodeSymbolKindFiles(root, idx, "func")

	want := []codeSymbolFileSummary{
		{Path: "cmd/a.go", Package: "main", Symbols: 2},
		{Path: "pkg/b.go", Package: "pkg", Symbols: 1},
	}
	if !reflect.DeepEqual(summaries, want) {
		require.Failf(t, "unexpected symbol kind file summaries", "got %#v, want %#v", summaries, want)
	}

	if got := summarizeCodeSymbolKindFiles(root, idx, "missing"); len(got) != 0 {
		require.Failf(t, "missing kind should return empty", "got %#v", got)
	}

	if got := summarizeCodeSymbolKindFiles(root, idx, " "); got != nil {
		require.Failf(t, "blank kind should return nil", "got %#v", got)
	}
}

func TestSummarizeCodeSymbolKindPackages(t *testing.T) {
	t.Parallel()

	idx := codeintel.Index{Files: []codeintel.File{
		{Path: "pkg/b.go", Package: "pkg", Symbols: []codeintel.Symbol{{Kind: "func"}, {Kind: "type"}}},
		{Path: "cmd/a.go", Package: "main", Symbols: []codeintel.Symbol{{Kind: "func"}, {Kind: "FUNC"}}},
		{Path: "pkg/c.go", Package: "pkg", Symbols: []codeintel.Symbol{{Kind: "func"}, {Kind: "FUNC"}, {Kind: "const"}}},
		{Path: "empty.go", Symbols: []codeintel.Symbol{{Kind: "func"}}},
	}}
	summaries := summarizeCodeSymbolKindPackages(idx, "func")

	want := []codePackageSummary{
		{Name: "pkg", Files: 2, Symbols: 3},
		{Name: "main", Files: 1, Symbols: 2},
	}
	if !reflect.DeepEqual(summaries, want) {
		require.Failf(t, "unexpected symbol kind package summaries", "got %#v, want %#v", summaries, want)
	}

	if got := summarizeCodeSymbolKindPackages(idx, "missing"); len(got) != 0 {
		require.Failf(t, "missing kind should return empty", "got %#v", got)
	}

	if got := summarizeCodeSymbolKindPackages(idx, " "); got != nil {
		require.Failf(t, "blank kind should return nil", "got %#v", got)
	}
}

func TestCodeSymbolsByKind(t *testing.T) {
	t.Parallel()

	idx := codeintel.Index{Symbols: []codeintel.Symbol{
		{Name: "Run", Kind: "func", File: "b.go", Line: 20},
		{Name: "Client", Kind: "type", File: "a.go", Line: 1},
		{Name: "Build", Kind: "func", File: "a.go", Line: 10},
		{Name: "Count", Kind: "var", File: "c.go", Line: 3},
	}}
	matches := codeSymbolsByKind(idx, " FUNC ")

	want := []codeintel.Symbol{
		{Name: "Build", Kind: "func", File: "a.go", Line: 10},
		{Name: "Run", Kind: "func", File: "b.go", Line: 20},
	}
	if !reflect.DeepEqual(matches, want) {
		require.Failf(t, "unexpected kind matches", "got %#v, want %#v", matches, want)
	}

	if got := codeSymbolsByKind(idx, " "); got != nil {
		require.Failf(t, "blank kind should return nil", "got %#v", got)
	}
}

func TestSummarizeCodeSymbolNameFiles(t *testing.T) {
	t.Parallel()

	root := filepath.Join("tmp", "repo")
	idx := codeintel.Index{Files: []codeintel.File{
		{Path: filepath.Join(root, "pkg", "b.go"), Package: "pkg", Symbols: []codeintel.Symbol{{Name: "Run"}, {Name: "Build"}}},
		{Path: filepath.Join(root, "cmd", "a.go"), Package: "main", Symbols: []codeintel.Symbol{{Name: "Run"}, {Name: "Run"}}},
		{Path: filepath.Join(root, "pkg", "c.go"), Package: "pkg", Symbols: []codeintel.Symbol{{Name: "Client"}}},
	}}
	summaries := summarizeCodeSymbolNameFiles(root, idx, "Run")

	want := []codeSymbolFileSummary{
		{Path: "cmd/a.go", Package: "main", Symbols: 2},
		{Path: "pkg/b.go", Package: "pkg", Symbols: 1},
	}
	if !reflect.DeepEqual(summaries, want) {
		require.Failf(t, "unexpected symbol name file summaries", "got %#v, want %#v", summaries, want)
	}

	if got := summarizeCodeSymbolNameFiles(root, idx, "missing"); len(got) != 0 {
		require.Failf(t, "missing name should return empty", "got %#v", got)
	}

	if got := summarizeCodeSymbolNameFiles(root, idx, " "); got != nil {
		require.Failf(t, "blank name should return nil", "got %#v", got)
	}
}

func TestSummarizeCodeSymbolNamePackages(t *testing.T) {
	t.Parallel()

	idx := codeintel.Index{Files: []codeintel.File{
		{Path: "pkg/b.go", Package: "pkg", Symbols: []codeintel.Symbol{{Name: "Run"}, {Name: "Build"}}},
		{Path: "cmd/a.go", Package: "main", Symbols: []codeintel.Symbol{{Name: "Run"}, {Name: "Run"}}},
		{Path: "pkg/c.go", Package: "pkg", Symbols: []codeintel.Symbol{{Name: "Run"}, {Name: "Run"}, {Name: "Client"}}},
		{Path: "empty.go", Symbols: []codeintel.Symbol{{Name: "Run"}}},
	}}
	summaries := summarizeCodeSymbolNamePackages(idx, "Run")

	want := []codePackageSummary{
		{Name: "pkg", Files: 2, Symbols: 3},
		{Name: "main", Files: 1, Symbols: 2},
	}
	if !reflect.DeepEqual(summaries, want) {
		require.Failf(t, "unexpected symbol name package summaries", "got %#v, want %#v", summaries, want)
	}

	if got := summarizeCodeSymbolNamePackages(idx, "missing"); len(got) != 0 {
		require.Failf(t, "missing name should return empty", "got %#v", got)
	}

	if got := summarizeCodeSymbolNamePackages(idx, " "); got != nil {
		require.Failf(t, "blank name should return nil", "got %#v", got)
	}
}

func TestSummarizeCodeSymbolPrefixFiles(t *testing.T) {
	t.Parallel()

	root := filepath.Join("tmp", "repo")
	idx := codeintel.Index{Files: []codeintel.File{
		{Path: filepath.Join(root, "pkg", "b.go"), Package: "pkg", Symbols: []codeintel.Symbol{{Name: "Run"}, {Name: "Build"}}},
		{Path: filepath.Join(root, "cmd", "a.go"), Package: "main", Symbols: []codeintel.Symbol{{Name: "Render"}, {Name: "Run"}}},
		{Path: filepath.Join(root, "pkg", "c.go"), Package: "pkg", Symbols: []codeintel.Symbol{{Name: "Client"}}},
	}}
	summaries := summarizeCodeSymbolPrefixFiles(root, idx, "R")

	want := []codeSymbolFileSummary{
		{Path: "cmd/a.go", Package: "main", Symbols: 2},
		{Path: "pkg/b.go", Package: "pkg", Symbols: 1},
	}
	if !reflect.DeepEqual(summaries, want) {
		require.Failf(t, "unexpected symbol prefix file summaries", "got %#v, want %#v", summaries, want)
	}

	if got := summarizeCodeSymbolPrefixFiles(root, idx, "missing"); len(got) != 0 {
		require.Failf(t, "missing prefix should return empty", "got %#v", got)
	}

	if got := summarizeCodeSymbolPrefixFiles(root, idx, " "); got != nil {
		require.Failf(t, "blank prefix should return nil", "got %#v", got)
	}
}

func TestSummarizeCodeSymbolPrefixPackages(t *testing.T) {
	t.Parallel()

	idx := codeintel.Index{Files: []codeintel.File{
		{Path: "pkg/b.go", Package: "pkg", Symbols: []codeintel.Symbol{{Name: "Run"}, {Name: "Build"}}},
		{Path: "cmd/a.go", Package: "main", Symbols: []codeintel.Symbol{{Name: "Render"}, {Name: "Run"}}},
		{Path: "pkg/c.go", Package: "pkg", Symbols: []codeintel.Symbol{{Name: "Render"}, {Name: "Run"}, {Name: "Client"}}},
		{Path: "empty.go", Symbols: []codeintel.Symbol{{Name: "Run"}}},
	}}
	summaries := summarizeCodeSymbolPrefixPackages(idx, "R")

	want := []codePackageSummary{
		{Name: "pkg", Files: 2, Symbols: 3},
		{Name: "main", Files: 1, Symbols: 2},
	}
	if !reflect.DeepEqual(summaries, want) {
		require.Failf(t, "unexpected symbol prefix package summaries", "got %#v, want %#v", summaries, want)
	}

	if got := summarizeCodeSymbolPrefixPackages(idx, "missing"); len(got) != 0 {
		require.Failf(t, "missing prefix should return empty", "got %#v", got)
	}

	if got := summarizeCodeSymbolPrefixPackages(idx, " "); got != nil {
		require.Failf(t, "blank prefix should return nil", "got %#v", got)
	}
}

func TestCodeSymbolsWithPrefix(t *testing.T) {
	t.Parallel()

	idx := codeintel.Index{Symbols: []codeintel.Symbol{
		{Name: "RunOnce", File: "b.go", Line: 20},
		{Name: "Build", File: "a.go", Line: 1},
		{Name: "Run", File: "a.go", Line: 10},
		{Name: "Run", File: "a.go", Line: 8},
	}}
	matches := codeSymbolsWithPrefix(idx, "Run")

	want := []codeintel.Symbol{
		{Name: "Run", File: "a.go", Line: 8},
		{Name: "Run", File: "a.go", Line: 10},
		{Name: "RunOnce", File: "b.go", Line: 20},
	}
	if !reflect.DeepEqual(matches, want) {
		require.Failf(t, "unexpected prefix matches", "got %#v, want %#v", matches, want)
	}

	if got := codeSymbolsWithPrefix(idx, " "); got != nil {
		require.Failf(t, "blank prefix should return nil", "got %#v", got)
	}
}

func TestFormatCodeSymbol(t *testing.T) {
	t.Parallel()

	root := filepath.Join("tmp", "repo")
	got := formatCodeSymbol(root, codeintel.Symbol{
		Name: "Run",
		Kind: "method",
		File: filepath.Join(root, "pkg", "runner.go"),
		Line: 42,
	})

	want := strings.Join([]string{"Run", "kind=method", "path=pkg/runner.go", "line=42"}, "\t")
	if got != want {
		require.Failf(t, "unexpected code symbol format", "got %q, want %q", got, want)
	}
}
