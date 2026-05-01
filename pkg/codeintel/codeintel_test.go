package codeintel

import (
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"sort"
	"strings"
	"testing"
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
