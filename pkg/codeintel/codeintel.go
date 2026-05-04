// Package codeintel provides small Go code intelligence primitives.
package codeintel

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
)

// Symbol describes a named declaration in a Go source file.
type Symbol struct {
	Name string
	Kind string
	File string
	Line int
}

// File summarizes one parsed Go source file.
type File struct {
	Path    string
	Package string
	Imports []string
	Symbols []Symbol
}

// ImportEdge describes one file-level import relationship.
type ImportEdge struct {
	From   string
	Import string
}

// Index contains parsed summaries and lookup data for a set of Go files.
type Index struct {
	Files       []File
	Symbols     []Symbol
	ImportEdges []ImportEdge
}

// IndexDir parses all Go source files under root.
func IndexDir(root string) (Index, error) {
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
			files = append(files, path)
		}

		return nil
	})
	if err != nil {
		return Index{}, fmt.Errorf("index dir %s: %w", root, err)
	}

	sort.Strings(files)

	return IndexFiles(files)
}

// IndexFiles parses the provided Go source files.
func IndexFiles(paths []string) (Index, error) {
	fset := token.NewFileSet()
	fileSummaries := make([]File, 0, len(paths))

	var (
		symbols []Symbol
		edges   []ImportEdge
	)

	for _, path := range paths {
		parsed, err := parser.ParseFile(fset, path, nil, 0)
		if err != nil {
			return Index{}, fmt.Errorf("parse %s: %w", path, err)
		}

		summary := summarizeFile(fset, path, parsed)
		fileSummaries = append(fileSummaries, summary)

		symbols = append(symbols, summary.Symbols...)
		for _, imp := range summary.Imports {
			edges = append(edges, ImportEdge{
				From:   path,
				Import: imp,
			})
		}
	}

	sort.Slice(symbols, func(i, j int) bool {
		if symbols[i].Name != symbols[j].Name {
			return symbols[i].Name < symbols[j].Name
		}

		if symbols[i].File != symbols[j].File {
			return symbols[i].File < symbols[j].File
		}

		return symbols[i].Line < symbols[j].Line
	})

	return Index{
		Files:       fileSummaries,
		Symbols:     symbols,
		ImportEdges: edges,
	}, nil
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

func summarizeFile(fset *token.FileSet, path string, file *ast.File) File {
	summary := File{
		Path:    path,
		Package: file.Name.Name,
	}

	for _, imp := range file.Imports {
		importPath, err := strconv.Unquote(imp.Path.Value)
		if err != nil {
			importPath = imp.Path.Value
		}

		summary.Imports = append(summary.Imports, importPath)
	}

	sort.Strings(summary.Imports)

	for _, decl := range file.Decls {
		switch decl := decl.(type) {
		case *ast.FuncDecl:
			summary.Symbols = append(summary.Symbols, funcSymbol(fset, path, decl))
		case *ast.GenDecl:
			summary.Symbols = append(summary.Symbols, genDeclSymbols(fset, path, decl)...)
		}
	}

	return summary
}

func funcSymbol(fset *token.FileSet, path string, decl *ast.FuncDecl) Symbol {
	kind := "func"
	if decl.Recv != nil {
		kind = "method"
	}

	return Symbol{
		Name: decl.Name.Name,
		Kind: kind,
		File: path,
		Line: fset.Position(decl.Name.Pos()).Line,
	}
}

func genDeclSymbols(fset *token.FileSet, path string, decl *ast.GenDecl) []Symbol {
	var symbols []Symbol

	for _, spec := range decl.Specs {
		switch spec := spec.(type) {
		case *ast.TypeSpec:
			symbols = append(symbols, Symbol{
				Name: spec.Name.Name,
				Kind: "type",
				File: path,
				Line: fset.Position(spec.Name.Pos()).Line,
			})
		case *ast.ValueSpec:
			kind := tokenKind(decl.Tok)

			for _, name := range spec.Names {
				if name.Name == "_" {
					continue
				}

				symbols = append(symbols, Symbol{
					Name: name.Name,
					Kind: kind,
					File: path,
					Line: fset.Position(name.Pos()).Line,
				})
			}
		}
	}

	return symbols
}

func tokenKind(tok token.Token) string {
	switch tok {
	case token.CONST:
		return "const"
	case token.VAR:
		return "var"
	default:
		return strings.ToLower(tok.String())
	}
}

func skipDir(name string) bool {
	switch name {
	case ".git", ".hg", ".svn", "node_modules", "vendor":
		return true
	default:
		return false
	}
}
