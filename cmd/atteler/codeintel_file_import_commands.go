package main

import (
	"strings"

	"github.com/tommoulard/atteler/pkg/codeintel"
)

func codeFileImports(file codeintel.File) []string {
	if len(file.Imports) == 0 {
		return nil
	}

	imports := append([]string(nil), file.Imports...)
	sortCodeIntelStringsAsc(imports)

	return imports
}

func codeFileImportsForPath(file codeintel.File, importPath string) []string {
	importPath = strings.TrimSpace(importPath)
	if importPath == "" {
		return nil
	}

	imports := filterCodeIntelSlice(file.Imports, func(imp string) bool { return imp == importPath })
	sortCodeIntelStringsAsc(imports)

	return imports
}

func codeFileImportsWithPrefix(file codeintel.File, prefix string) []string {
	prefix = strings.TrimSpace(prefix)
	if prefix == "" {
		return nil
	}

	imports := filterCodeIntelSlice(file.Imports, func(imp string) bool { return strings.HasPrefix(imp, prefix) })
	sortCodeIntelStringsAsc(imports)

	return imports
}
