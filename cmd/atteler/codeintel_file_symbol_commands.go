package main

import (
	"fmt"
	"strings"

	"github.com/tommoulard/atteler/pkg/codeintel"
)

func listCodeFileSymbolSummary(root, target string) error {
	idx, err := codeintel.IndexDir(root)
	if err != nil {
		return fmt.Errorf("code file symbol summary: index %s: %w", root, err)
	}

	file, ok := findCodeFile(root, idx, target)
	if !ok {
		fmt.Println("No Go code file found.")
		return nil
	}

	summaries := summarizeCodeFileSymbols(file)
	if len(summaries) == 0 {
		fmt.Println("No Go code file symbols found.")
		return nil
	}

	for i := range summaries {
		fmt.Println(formatCodeSymbolSummary(summaries[i]))
	}

	return nil
}

func summarizeCodeFileSymbols(file codeintel.File) []codeSymbolSummary {
	return summarizeCodeSymbols(codeintel.Index{Symbols: file.Symbols})
}

func listCodeFileSymbols(root, target string) error {
	idx, err := codeintel.IndexDir(root)
	if err != nil {
		return fmt.Errorf("code file symbols: index %s: %w", root, err)
	}

	file, ok := findCodeFile(root, idx, target)
	if !ok {
		fmt.Println("No Go code file found.")
		return nil
	}

	symbols := codeFileSymbols(file)
	if len(symbols) == 0 {
		fmt.Println("No Go code file symbols found.")
		return nil
	}

	for i := range symbols {
		fmt.Println(formatCodeFileSymbol(symbols[i]))
	}

	return nil
}

func codeFileSymbols(file codeintel.File) []codeintel.Symbol {
	if len(file.Symbols) == 0 {
		return nil
	}

	symbols := append([]codeintel.Symbol(nil), file.Symbols...)
	sortCodeSymbols(symbols)

	return symbols
}

func listCodeFileSymbol(root, spec string) error {
	target, name, err := parseCodeFileSymbolFilterSpec(spec, "code file symbol", "path:name")
	if err != nil {
		return err
	}

	idx, err := codeintel.IndexDir(root)
	if err != nil {
		return fmt.Errorf("code file symbol: index %s: %w", root, err)
	}

	file, ok := findCodeFile(root, idx, target)
	if !ok {
		fmt.Println("No Go code file found.")
		return nil
	}

	symbols := codeFileSymbolsByName(file, name)
	if len(symbols) == 0 {
		fmt.Println("No Go code file symbols found.")
		return nil
	}

	for i := range symbols {
		fmt.Println(formatCodeFileSymbol(symbols[i]))
	}

	return nil
}

func codeFileSymbolsByName(file codeintel.File, name string) []codeintel.Symbol {
	name = strings.TrimSpace(name)
	if name == "" {
		return nil
	}

	symbols := make([]codeintel.Symbol, 0)

	for i := range file.Symbols {
		if file.Symbols[i].Name == name {
			symbols = append(symbols, file.Symbols[i])
		}
	}

	sortCodeSymbols(symbols)

	return symbols
}

func listCodeFileSymbolPrefix(root, spec string) error {
	target, prefix, err := parseCodeFileSymbolFilterSpec(spec, "code file symbol prefix", "path:prefix")
	if err != nil {
		return err
	}

	idx, err := codeintel.IndexDir(root)
	if err != nil {
		return fmt.Errorf("code file symbol prefix: index %s: %w", root, err)
	}

	file, ok := findCodeFile(root, idx, target)
	if !ok {
		fmt.Println("No Go code file found.")
		return nil
	}

	symbols := codeFileSymbolsWithPrefix(file, prefix)
	if len(symbols) == 0 {
		fmt.Println("No Go code file symbols found.")
		return nil
	}

	for i := range symbols {
		fmt.Println(formatCodeFileSymbol(symbols[i]))
	}

	return nil
}

func codeFileSymbolsWithPrefix(file codeintel.File, prefix string) []codeintel.Symbol {
	prefix = strings.TrimSpace(prefix)
	if prefix == "" {
		return nil
	}

	symbols := make([]codeintel.Symbol, 0)

	for i := range file.Symbols {
		if strings.HasPrefix(file.Symbols[i].Name, prefix) {
			symbols = append(symbols, file.Symbols[i])
		}
	}

	sortCodeSymbols(symbols)

	return symbols
}

func listCodeFileSymbolKind(root, spec string) error {
	target, kind, err := parseCodeFileSymbolKindSpec(spec)
	if err != nil {
		return err
	}

	idx, err := codeintel.IndexDir(root)
	if err != nil {
		return fmt.Errorf("code file symbol kind: index %s: %w", root, err)
	}

	file, ok := findCodeFile(root, idx, target)
	if !ok {
		fmt.Println("No Go code file found.")
		return nil
	}

	symbols := codeFileSymbolsByKind(file, kind)
	if len(symbols) == 0 {
		fmt.Println("No Go code file symbols found.")
		return nil
	}

	for i := range symbols {
		fmt.Println(formatCodeFileSymbol(symbols[i]))
	}

	return nil
}

func parseCodeFileSymbolKindSpec(spec string) (target, kind string, err error) {
	return parseCodeFileSymbolFilterSpec(spec, "code file symbol kind", "path:kind")
}

func parseCodeFileSymbolFilterSpec(spec, label, expected string) (target, value string, err error) {
	parts := strings.SplitN(spec, ":", 2)
	if len(parts) != 2 {
		return "", "", fmt.Errorf("%s: expected %s", label, expected)
	}

	target = strings.TrimSpace(parts[0])

	value = strings.TrimSpace(parts[1])
	if target == "" || value == "" {
		return "", "", fmt.Errorf("%s: path and value are required", label)
	}

	return target, value, nil
}

func codeFileSymbolsByKind(file codeintel.File, kind string) []codeintel.Symbol {
	kind = strings.TrimSpace(kind)
	if kind == "" {
		return nil
	}

	symbols := make([]codeintel.Symbol, 0)

	for i := range file.Symbols {
		if strings.EqualFold(file.Symbols[i].Kind, kind) {
			symbols = append(symbols, file.Symbols[i])
		}
	}

	sortCodeSymbols(symbols)

	return symbols
}
