package main

import (
	"reflect"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/tommoulard/atteler/pkg/codeintel"
)

func TestCodeFileImports(t *testing.T) {
	t.Parallel()

	file := codeintel.File{Imports: []string{"fmt", "context", "errors"}}
	imports := codeFileImports(file)

	want := []string{"context", "errors", "fmt"}
	if !reflect.DeepEqual(imports, want) {
		require.Failf(t, "unexpected code file imports", "got %#v, want %#v", imports, want)
	}

	if got := codeFileImports(codeintel.File{}); got != nil {
		require.Failf(t, "empty imports should return nil", "got %#v", got)
	}
}

func TestCodeFileImportsForPath(t *testing.T) {
	t.Parallel()

	file := codeintel.File{Imports: []string{"context", "fmt", "context"}}
	imports := codeFileImportsForPath(file, "context")

	want := []string{"context", "context"}
	if !reflect.DeepEqual(imports, want) {
		require.Failf(t, "unexpected file imports by path", "got %#v, want %#v", imports, want)
	}

	if got := codeFileImportsForPath(file, " "); got != nil {
		require.Failf(t, "blank import path should return nil", "got %#v", got)
	}
}

func TestCodeFileImportsWithPrefix(t *testing.T) {
	t.Parallel()

	file := codeintel.File{Imports: []string{
		"github.com/tommoulard/atteler/pkg/llm",
		"context",
		"github.com/tommoulard/atteler/pkg/agent",
	}}
	imports := codeFileImportsWithPrefix(file, "github.com/tommoulard/atteler/pkg/")

	want := []string{"github.com/tommoulard/atteler/pkg/agent", "github.com/tommoulard/atteler/pkg/llm"}
	if !reflect.DeepEqual(imports, want) {
		require.Failf(t, "unexpected file imports by prefix", "got %#v, want %#v", imports, want)
	}

	if got := codeFileImportsWithPrefix(file, " "); got != nil {
		require.Failf(t, "blank prefix should return nil", "got %#v", got)
	}
}

func TestSummarizeCodeFileSymbols(t *testing.T) {
	t.Parallel()

	file := codeintel.File{Symbols: []codeintel.Symbol{
		{Kind: "func"},
		{Kind: "type"},
		{Kind: "func"},
		{Kind: "const"},
		{Kind: ""},
	}}
	summaries := summarizeCodeFileSymbols(file)

	want := []codeSymbolSummary{{Kind: "func", Count: 2}, {Kind: "const", Count: 1}, {Kind: "type", Count: 1}}
	if !reflect.DeepEqual(summaries, want) {
		require.Failf(t, "unexpected file symbol summaries", "got %#v, want %#v", summaries, want)
	}

	if got := summarizeCodeFileSymbols(codeintel.File{}); len(got) != 0 {
		require.Failf(t, "empty file should return empty summaries", "got %#v", got)
	}
}

func TestCodeFileSymbols(t *testing.T) {
	t.Parallel()

	file := codeintel.File{Symbols: []codeintel.Symbol{
		{Name: "Run", Kind: "func", File: "b.go", Line: 2},
		{Name: "Client", Kind: "type", File: "a.go", Line: 1},
		{Name: "Build", Kind: "func", File: "c.go", Line: 3},
	}}
	symbols := codeFileSymbols(file)

	want := []codeintel.Symbol{
		{Name: "Build", Kind: "func", File: "c.go", Line: 3},
		{Name: "Client", Kind: "type", File: "a.go", Line: 1},
		{Name: "Run", Kind: "func", File: "b.go", Line: 2},
	}
	if !reflect.DeepEqual(symbols, want) {
		require.Failf(t, "unexpected code file symbols", "got %#v, want %#v", symbols, want)
	}

	if got := codeFileSymbols(codeintel.File{}); got != nil {
		require.Failf(t, "empty symbols should return nil", "got %#v", got)
	}
}

func TestCodeFileSymbolsByName(t *testing.T) {
	t.Parallel()

	target, name, err := parseCodeFileSymbolFilterSpec("pkg/llm/client.go:Run", "code file symbol", "path:name")
	if err != nil {
		require.NoError(t, err)
	}

	if target != "pkg/llm/client.go" || name != "Run" {
		require.Failf(t, "unexpected parsed spec", "target=%q name=%q", target, name)
	}

	if _, _, err := parseCodeFileSymbolFilterSpec("pkg/llm/client.go", "code file symbol", "path:name"); err == nil {
		require.Fail(t, "expected parse error for missing name")
	}

	file := codeintel.File{Symbols: []codeintel.Symbol{
		{Name: "Run", Kind: "method", File: "b.go", Line: 2},
		{Name: "Client", Kind: "type", File: "a.go", Line: 1},
		{Name: "Run", Kind: "func", File: "a.go", Line: 3},
	}}
	symbols := codeFileSymbolsByName(file, "Run")

	want := []codeintel.Symbol{
		{Name: "Run", Kind: "func", File: "a.go", Line: 3},
		{Name: "Run", Kind: "method", File: "b.go", Line: 2},
	}
	if !reflect.DeepEqual(symbols, want) {
		require.Failf(t, "unexpected file symbols by name", "got %#v, want %#v", symbols, want)
	}

	if got := codeFileSymbolsByName(file, " "); got != nil {
		require.Failf(t, "blank name should return nil", "got %#v", got)
	}
}

func TestCodeFileSymbolsWithPrefix(t *testing.T) {
	t.Parallel()

	file := codeintel.File{Symbols: []codeintel.Symbol{
		{Name: "NewClient", Kind: "func", File: "b.go", Line: 2},
		{Name: "Client", Kind: "type", File: "a.go", Line: 1},
		{Name: "NewRegistry", Kind: "func", File: "c.go", Line: 3},
	}}
	symbols := codeFileSymbolsWithPrefix(file, "New")

	want := []codeintel.Symbol{
		{Name: "NewClient", Kind: "func", File: "b.go", Line: 2},
		{Name: "NewRegistry", Kind: "func", File: "c.go", Line: 3},
	}
	if !reflect.DeepEqual(symbols, want) {
		require.Failf(t, "unexpected file symbols by prefix", "got %#v, want %#v", symbols, want)
	}

	if got := codeFileSymbolsWithPrefix(file, " "); got != nil {
		require.Failf(t, "blank prefix should return nil", "got %#v", got)
	}
}

func TestCodeFileSymbolsByKindAndParseSpec(t *testing.T) {
	t.Parallel()

	target, kind, err := parseCodeFileSymbolKindSpec("pkg/llm/llm.go:func")
	if err != nil {
		require.NoError(t, err)
	}

	if target != "pkg/llm/llm.go" || kind != "func" {
		require.Failf(t, "unexpected parsed file symbol kind spec", "target=%q kind=%q", target, kind)
	}

	if _, _, err := parseCodeFileSymbolKindSpec("pkg/llm/llm.go"); err == nil {
		require.Fail(t, "expected parse error for missing kind")
	}

	file := codeintel.File{Symbols: []codeintel.Symbol{
		{Name: "Run", Kind: "func", File: "b.go", Line: 2},
		{Name: "Client", Kind: "type", File: "a.go", Line: 1},
		{Name: "Build", Kind: "func", File: "c.go", Line: 3},
	}}
	symbols := codeFileSymbolsByKind(file, "FUNC")

	want := []codeintel.Symbol{
		{Name: "Build", Kind: "func", File: "c.go", Line: 3},
		{Name: "Run", Kind: "func", File: "b.go", Line: 2},
	}
	if !reflect.DeepEqual(symbols, want) {
		require.Failf(t, "unexpected file symbols by kind", "got %#v, want %#v", symbols, want)
	}
}
