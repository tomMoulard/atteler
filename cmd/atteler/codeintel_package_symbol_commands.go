package main

import (
	"strings"

	"github.com/tommoulard/atteler/pkg/codeintel"
)

func summarizeCodePackageSymbolFiles(root string, idx codeintel.Index, packageName string) []codeSymbolFileSummary {
	if strings.TrimSpace(packageName) == "" {
		return nil
	}

	files := codePackageFiles(idx, packageName)
	if len(files) == 0 {
		return nil
	}

	return summarizeCodeSymbolFilesByPredicate(root, files, func(_ codeintel.Symbol) bool {
		return true
	})
}

func codePackageSymbolsByName(idx codeintel.Index, packageName, name string) []codeintel.Symbol {
	name = strings.TrimSpace(name)
	if name == "" {
		return nil
	}

	symbols := codePackageSymbols(idx, packageName)
	if len(symbols) == 0 {
		return nil
	}

	return filterCodeSymbols(symbols, func(symbol codeintel.Symbol) bool {
		return symbol.Name == name
	})
}

func summarizeCodePackageSymbolNameFiles(root string, idx codeintel.Index, packageName, name string) []codeSymbolFileSummary {
	packageName = strings.TrimSpace(packageName)

	name = strings.TrimSpace(name)
	if packageName == "" || name == "" {
		return nil
	}

	return summarizeCodeSymbolFilesByPredicate(root, codePackageFiles(idx, packageName), func(symbol codeintel.Symbol) bool {
		return symbol.Name == name
	})
}

func parseCodePackageSymbolKindSpec(spec string) (packageName, kind string, err error) {
	return parseCodeIntelPairSpec(spec, "code package symbol kind", "package:kind", "package", "kind")
}

func codePackageSymbolsByKind(idx codeintel.Index, packageName, kind string) []codeintel.Symbol {
	kind = strings.TrimSpace(kind)
	if kind == "" {
		return nil
	}

	symbols := codePackageSymbols(idx, packageName)
	if len(symbols) == 0 {
		return nil
	}

	return filterCodeSymbols(symbols, func(symbol codeintel.Symbol) bool {
		return strings.EqualFold(symbol.Kind, kind)
	})
}

func summarizeCodePackageSymbolPrefixFiles(root string, idx codeintel.Index, packageName, prefix string) []codeSymbolFileSummary {
	packageName = strings.TrimSpace(packageName)

	prefix = strings.TrimSpace(prefix)
	if packageName == "" || prefix == "" {
		return nil
	}

	return summarizeCodeSymbolFilesByPredicate(root, codePackageFiles(idx, packageName), func(symbol codeintel.Symbol) bool {
		return strings.HasPrefix(symbol.Name, prefix)
	})
}

func summarizeCodePackageSymbolKindFiles(root string, idx codeintel.Index, packageName, kind string) []codeSymbolFileSummary {
	packageName = strings.TrimSpace(packageName)

	kind = strings.TrimSpace(kind)
	if packageName == "" || kind == "" {
		return nil
	}

	return summarizeCodeSymbolFilesByPredicate(root, codePackageFiles(idx, packageName), func(symbol codeintel.Symbol) bool {
		return strings.EqualFold(symbol.Kind, kind)
	})
}

func codePackageSymbolsWithPrefix(idx codeintel.Index, packageName, prefix string) []codeintel.Symbol {
	prefix = strings.TrimSpace(prefix)
	if prefix == "" {
		return nil
	}

	symbols := codePackageSymbols(idx, packageName)
	if len(symbols) == 0 {
		return nil
	}

	return filterCodeSymbols(symbols, func(symbol codeintel.Symbol) bool {
		return strings.HasPrefix(symbol.Name, prefix)
	})
}

func codePackageSymbols(idx codeintel.Index, packageName string) []codeintel.Symbol {
	files := codePackageFiles(idx, packageName)
	if len(files) == 0 {
		return nil
	}

	symbols := make([]codeintel.Symbol, 0)

	for _, file := range files {
		symbols = append(symbols, file.Symbols...)
	}

	sortCodeSymbols(symbols)

	return symbols
}

func codePackageFiles(idx codeintel.Index, packageName string) []codeintel.File {
	packageName = strings.TrimSpace(packageName)
	if packageName == "" {
		return nil
	}

	return filterCodeIntelSlice(idx.Files, func(file codeintel.File) bool {
		return file.Package == packageName
	})
}

func summarizeCodePackageSymbols(idx codeintel.Index, packageName string) []codeSymbolSummary {
	if strings.TrimSpace(packageName) == "" {
		return nil
	}

	return summarizeCodeSymbols(codeintel.Index{Symbols: codePackageSymbols(idx, packageName)})
}
