package main

import (
	"fmt"
	"sort"
	"strings"

	"github.com/tommoulard/atteler/pkg/codeintel"
)

func listCodeFileImports(root, target string) error {
	idx, err := codeintel.IndexDir(root)
	if err != nil {
		return fmt.Errorf("code file imports: index %s: %w", root, err)
	}

	file, ok := findCodeFile(root, idx, target)
	if !ok {
		fmt.Println("No Go code file found.")
		return nil
	}

	imports := codeFileImports(file)
	if len(imports) == 0 {
		fmt.Println("No Go code file imports found.")
		return nil
	}

	for _, imp := range imports {
		fmt.Println("import=" + imp)
	}

	return nil
}

func codeFileImports(file codeintel.File) []string {
	if len(file.Imports) == 0 {
		return nil
	}

	imports := append([]string(nil), file.Imports...)
	sort.Strings(imports)

	return imports
}

func listCodeFileImportPath(root, spec string) error {
	target, importPath, err := parseCodeFileSymbolFilterSpec(spec, "code file import path", "path:import")
	if err != nil {
		return err
	}

	idx, err := codeintel.IndexDir(root)
	if err != nil {
		return fmt.Errorf("code file import path: index %s: %w", root, err)
	}

	file, ok := findCodeFile(root, idx, target)
	if !ok {
		fmt.Println("No Go code file found.")
		return nil
	}

	imports := codeFileImportsForPath(file, importPath)
	if len(imports) == 0 {
		fmt.Println("No Go code file imports found.")
		return nil
	}

	for _, imp := range imports {
		fmt.Println("import=" + imp)
	}

	return nil
}

func codeFileImportsForPath(file codeintel.File, importPath string) []string {
	importPath = strings.TrimSpace(importPath)
	if importPath == "" {
		return nil
	}

	imports := make([]string, 0, 1)

	for _, imp := range file.Imports {
		if imp == importPath {
			imports = append(imports, imp)
		}
	}

	sort.Strings(imports)

	return imports
}

func listCodeFileImportPrefix(root, spec string) error {
	target, prefix, err := parseCodeFileSymbolFilterSpec(spec, "code file import prefix", "path:prefix")
	if err != nil {
		return err
	}

	idx, err := codeintel.IndexDir(root)
	if err != nil {
		return fmt.Errorf("code file import prefix: index %s: %w", root, err)
	}

	file, ok := findCodeFile(root, idx, target)
	if !ok {
		fmt.Println("No Go code file found.")
		return nil
	}

	imports := codeFileImportsWithPrefix(file, prefix)
	if len(imports) == 0 {
		fmt.Println("No Go code file imports found.")
		return nil
	}

	for _, imp := range imports {
		fmt.Println("import=" + imp)
	}

	return nil
}

func codeFileImportsWithPrefix(file codeintel.File, prefix string) []string {
	prefix = strings.TrimSpace(prefix)
	if prefix == "" {
		return nil
	}

	imports := make([]string, 0)

	for _, imp := range file.Imports {
		if strings.HasPrefix(imp, prefix) {
			imports = append(imports, imp)
		}
	}

	sort.Strings(imports)

	return imports
}
