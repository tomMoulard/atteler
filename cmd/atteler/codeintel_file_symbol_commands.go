package main

import (
	"fmt"
	"strings"

	"github.com/tommoulard/atteler/pkg/codeintel"
)

func summarizeCodeFileSymbols(file codeintel.File) []codeSymbolSummary {
	return summarizeCodeSymbols(codeintel.Index{Symbols: file.Symbols})
}

func codeFileSymbols(file codeintel.File) []codeintel.Symbol {
	if len(file.Symbols) == 0 {
		return nil
	}

	symbols := append([]codeintel.Symbol(nil), file.Symbols...)
	sortCodeSymbols(symbols)

	return symbols
}

func codeFileSymbolsByName(file codeintel.File, name string) []codeintel.Symbol {
	name = strings.TrimSpace(name)
	if name == "" {
		return nil
	}

	return filterCodeSymbols(file.Symbols, func(symbol codeintel.Symbol) bool {
		return symbol.Name == name
	})
}

func codeFileSymbolsWithPrefix(file codeintel.File, prefix string) []codeintel.Symbol {
	prefix = strings.TrimSpace(prefix)
	if prefix == "" {
		return nil
	}

	return filterCodeSymbols(file.Symbols, func(symbol codeintel.Symbol) bool {
		return strings.HasPrefix(symbol.Name, prefix)
	})
}

func parseCodeFileSymbolKindSpec(spec string) (target, kind string, err error) {
	return parseCodeIntelPairSpec(spec, "code file symbol kind", "path:kind", "path", "kind")
}

func parseCodeFileSymbolFilterSpec(spec, label, expected string) (target, value string, err error) {
	return parseCodeIntelPairSpec(spec, label, expected, "path", "value")
}

func parseCodeIntelPairSpec(spec, label, expected, targetName, valueName string) (target, value string, err error) {
	parts := strings.SplitN(spec, ":", 2)
	if len(parts) != 2 {
		return "", "", fmt.Errorf("%s: expected %s", label, expected)
	}

	target = strings.TrimSpace(parts[0])

	value = strings.TrimSpace(parts[1])
	if target == "" || value == "" {
		return "", "", fmt.Errorf("%s: %s and %s are required", label, targetName, valueName)
	}

	return target, value, nil
}

func codeFileSymbolsByKind(file codeintel.File, kind string) []codeintel.Symbol {
	kind = strings.TrimSpace(kind)
	if kind == "" {
		return nil
	}

	return filterCodeSymbols(file.Symbols, func(symbol codeintel.Symbol) bool {
		return strings.EqualFold(symbol.Kind, kind)
	})
}
