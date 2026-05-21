package main

import (
	"errors"
	"fmt"
	"sort"
	"strings"

	"github.com/tommoulard/atteler/pkg/codeintel"
)

func listCodePackageSymbolFileSummary(root, packageName string) error {
	idx, err := codeintel.IndexDir(root)
	if err != nil {
		return fmt.Errorf("code package symbol file summary: index %s: %w", root, err)
	}

	summaries := summarizeCodePackageSymbolFiles(root, idx, packageName)
	if len(summaries) == 0 {
		fmt.Println("No Go package symbols found.")
		return nil
	}

	for i := range summaries {
		fmt.Println(formatCodeSymbolFileSummary(summaries[i]))
	}

	return nil
}

func summarizeCodePackageSymbolFiles(root string, idx codeintel.Index, packageName string) []codeSymbolFileSummary {
	packageName = strings.TrimSpace(packageName)
	if packageName == "" {
		return nil
	}

	summaries := make([]codeSymbolFileSummary, 0)

	for i := range idx.Files {
		file := idx.Files[i]
		if file.Package != packageName || len(file.Symbols) == 0 {
			continue
		}

		summaries = append(summaries, codeSymbolFileSummary{
			Path:    relativeCodePath(root, file.Path),
			Package: file.Package,
			Symbols: len(file.Symbols),
		})
	}

	sort.Slice(summaries, func(i, j int) bool {
		if summaries[i].Symbols != summaries[j].Symbols {
			return summaries[i].Symbols > summaries[j].Symbols
		}

		return summaries[i].Path < summaries[j].Path
	})

	return summaries
}

func listCodePackageSymbol(root, spec string) error {
	packageName, name, err := parseCodeFileSymbolFilterSpec(spec, "code package symbol", "package:name")
	if err != nil {
		return err
	}

	idx, err := codeintel.IndexDir(root)
	if err != nil {
		return fmt.Errorf("code package symbol: index %s: %w", root, err)
	}

	symbols := codePackageSymbolsByName(idx, packageName, name)
	if len(symbols) == 0 {
		fmt.Println("No Go package symbols found.")
		return nil
	}

	for i := range symbols {
		fmt.Println(formatCodeSymbol(root, symbols[i]))
	}

	return nil
}

func codePackageSymbolsByName(idx codeintel.Index, packageName, name string) []codeintel.Symbol {
	symbols := codePackageSymbols(idx, packageName)
	if len(symbols) == 0 {
		return nil
	}

	name = strings.TrimSpace(name)
	if name == "" {
		return nil
	}

	filtered := make([]codeintel.Symbol, 0, len(symbols))
	for i := range symbols {
		if symbols[i].Name == name {
			filtered = append(filtered, symbols[i])
		}
	}

	return filtered
}

func listCodePackageSymbolNameFileSummary(root, spec string) error {
	packageName, name, err := parseCodeFileSymbolFilterSpec(spec, "code package symbol name file summary", "package:name")
	if err != nil {
		return err
	}

	idx, err := codeintel.IndexDir(root)
	if err != nil {
		return fmt.Errorf("code package symbol name file summary: index %s: %w", root, err)
	}

	summaries := summarizeCodePackageSymbolNameFiles(root, idx, packageName, name)
	if len(summaries) == 0 {
		fmt.Println("No Go package symbols found.")
		return nil
	}

	for i := range summaries {
		fmt.Println(formatCodeSymbolFileSummary(summaries[i]))
	}

	return nil
}

func summarizeCodePackageSymbolNameFiles(root string, idx codeintel.Index, packageName, name string) []codeSymbolFileSummary {
	packageName = strings.TrimSpace(packageName)

	name = strings.TrimSpace(name)
	if packageName == "" || name == "" {
		return nil
	}

	summaries := make([]codeSymbolFileSummary, 0)

	for i := range idx.Files {
		file := idx.Files[i]
		if file.Package != packageName {
			continue
		}

		count := 0

		for j := range file.Symbols {
			if file.Symbols[j].Name == name {
				count++
			}
		}

		if count == 0 {
			continue
		}

		summaries = append(summaries, codeSymbolFileSummary{
			Path:    relativeCodePath(root, file.Path),
			Package: file.Package,
			Symbols: count,
		})
	}

	sort.Slice(summaries, func(i, j int) bool {
		if summaries[i].Symbols != summaries[j].Symbols {
			return summaries[i].Symbols > summaries[j].Symbols
		}

		return summaries[i].Path < summaries[j].Path
	})

	return summaries
}

func listCodePackageSymbolKind(root, spec string) error {
	packageName, kind, err := parseCodePackageSymbolKindSpec(spec)
	if err != nil {
		return err
	}

	idx, err := codeintel.IndexDir(root)
	if err != nil {
		return fmt.Errorf("code package symbol kind: index %s: %w", root, err)
	}

	symbols := codePackageSymbolsByKind(idx, packageName, kind)
	if len(symbols) == 0 {
		fmt.Println("No Go package symbols found.")
		return nil
	}

	for i := range symbols {
		fmt.Println(formatCodeSymbol(root, symbols[i]))
	}

	return nil
}

func parseCodePackageSymbolKindSpec(spec string) (packageName, kind string, err error) {
	parts := strings.SplitN(spec, ":", 2)
	if len(parts) != 2 {
		return "", "", errors.New("code package symbol kind: expected package:kind")
	}

	packageName = strings.TrimSpace(parts[0])

	kind = strings.TrimSpace(parts[1])
	if packageName == "" || kind == "" {
		return "", "", errors.New("code package symbol kind: package and kind are required")
	}

	return packageName, kind, nil
}

func codePackageSymbolsByKind(idx codeintel.Index, packageName, kind string) []codeintel.Symbol {
	symbols := codePackageSymbols(idx, packageName)
	if len(symbols) == 0 {
		return nil
	}

	kind = strings.TrimSpace(kind)
	if kind == "" {
		return nil
	}

	filtered := make([]codeintel.Symbol, 0, len(symbols))
	for i := range symbols {
		if strings.EqualFold(symbols[i].Kind, kind) {
			filtered = append(filtered, symbols[i])
		}
	}

	return filtered
}

func listCodePackageSymbolPrefixFileSummary(root, spec string) error {
	packageName, prefix, err := parseCodeFileSymbolFilterSpec(spec, "code package symbol prefix file summary", "package:prefix")
	if err != nil {
		return err
	}

	idx, err := codeintel.IndexDir(root)
	if err != nil {
		return fmt.Errorf("code package symbol prefix file summary: index %s: %w", root, err)
	}

	summaries := summarizeCodePackageSymbolPrefixFiles(root, idx, packageName, prefix)
	if len(summaries) == 0 {
		fmt.Println("No Go package symbols found.")
		return nil
	}

	for i := range summaries {
		fmt.Println(formatCodeSymbolFileSummary(summaries[i]))
	}

	return nil
}

func summarizeCodePackageSymbolPrefixFiles(root string, idx codeintel.Index, packageName, prefix string) []codeSymbolFileSummary {
	packageName = strings.TrimSpace(packageName)

	prefix = strings.TrimSpace(prefix)
	if packageName == "" || prefix == "" {
		return nil
	}

	summaries := make([]codeSymbolFileSummary, 0)

	for i := range idx.Files {
		file := idx.Files[i]
		if file.Package != packageName {
			continue
		}

		count := 0

		for j := range file.Symbols {
			if strings.HasPrefix(file.Symbols[j].Name, prefix) {
				count++
			}
		}

		if count == 0 {
			continue
		}

		summaries = append(summaries, codeSymbolFileSummary{
			Path:    relativeCodePath(root, file.Path),
			Package: file.Package,
			Symbols: count,
		})
	}

	sort.Slice(summaries, func(i, j int) bool {
		if summaries[i].Symbols != summaries[j].Symbols {
			return summaries[i].Symbols > summaries[j].Symbols
		}

		return summaries[i].Path < summaries[j].Path
	})

	return summaries
}

func listCodePackageSymbolKindFileSummary(root, spec string) error {
	packageName, kind, err := parseCodePackageSymbolKindSpec(spec)
	if err != nil {
		return err
	}

	idx, err := codeintel.IndexDir(root)
	if err != nil {
		return fmt.Errorf("code package symbol kind file summary: index %s: %w", root, err)
	}

	summaries := summarizeCodePackageSymbolKindFiles(root, idx, packageName, kind)
	if len(summaries) == 0 {
		fmt.Println("No Go package symbols found.")
		return nil
	}

	for i := range summaries {
		fmt.Println(formatCodeSymbolFileSummary(summaries[i]))
	}

	return nil
}

func summarizeCodePackageSymbolKindFiles(root string, idx codeintel.Index, packageName, kind string) []codeSymbolFileSummary {
	packageName = strings.TrimSpace(packageName)

	kind = strings.TrimSpace(kind)
	if packageName == "" || kind == "" {
		return nil
	}

	summaries := make([]codeSymbolFileSummary, 0)

	for i := range idx.Files {
		file := idx.Files[i]
		if file.Package != packageName {
			continue
		}

		count := 0

		for j := range file.Symbols {
			if strings.EqualFold(file.Symbols[j].Kind, kind) {
				count++
			}
		}

		if count == 0 {
			continue
		}

		summaries = append(summaries, codeSymbolFileSummary{
			Path:    relativeCodePath(root, file.Path),
			Package: file.Package,
			Symbols: count,
		})
	}

	sort.Slice(summaries, func(i, j int) bool {
		if summaries[i].Symbols != summaries[j].Symbols {
			return summaries[i].Symbols > summaries[j].Symbols
		}

		return summaries[i].Path < summaries[j].Path
	})

	return summaries
}

func listCodePackageSymbolPrefix(root, spec string) error {
	packageName, prefix, err := parseCodeFileSymbolFilterSpec(spec, "code package symbol prefix", "package:prefix")
	if err != nil {
		return err
	}

	idx, err := codeintel.IndexDir(root)
	if err != nil {
		return fmt.Errorf("code package symbol prefix: index %s: %w", root, err)
	}

	symbols := codePackageSymbolsWithPrefix(idx, packageName, prefix)
	if len(symbols) == 0 {
		fmt.Println("No Go package symbols found.")
		return nil
	}

	for i := range symbols {
		fmt.Println(formatCodeSymbol(root, symbols[i]))
	}

	return nil
}

func codePackageSymbolsWithPrefix(idx codeintel.Index, packageName, prefix string) []codeintel.Symbol {
	symbols := codePackageSymbols(idx, packageName)
	if len(symbols) == 0 {
		return nil
	}

	prefix = strings.TrimSpace(prefix)
	if prefix == "" {
		return nil
	}

	filtered := make([]codeintel.Symbol, 0, len(symbols))
	for i := range symbols {
		if strings.HasPrefix(symbols[i].Name, prefix) {
			filtered = append(filtered, symbols[i])
		}
	}

	return filtered
}

func listCodePackageSymbolList(root, packageName string) error {
	idx, err := codeintel.IndexDir(root)
	if err != nil {
		return fmt.Errorf("code package symbol list: index %s: %w", root, err)
	}

	symbols := codePackageSymbols(idx, packageName)
	if len(symbols) == 0 {
		fmt.Println("No Go package symbols found.")
		return nil
	}

	for i := range symbols {
		fmt.Println(formatCodeSymbol(root, symbols[i]))
	}

	return nil
}

func codePackageSymbols(idx codeintel.Index, packageName string) []codeintel.Symbol {
	packageName = strings.TrimSpace(packageName)
	if packageName == "" {
		return nil
	}

	symbols := make([]codeintel.Symbol, 0)

	for i := range idx.Files {
		if idx.Files[i].Package == packageName {
			symbols = append(symbols, idx.Files[i].Symbols...)
		}
	}

	sortCodeSymbols(symbols)

	return symbols
}

func listCodePackageSymbols(root, packageName string) error {
	idx, err := codeintel.IndexDir(root)
	if err != nil {
		return fmt.Errorf("code package symbols: index %s: %w", root, err)
	}

	summaries := summarizeCodePackageSymbols(idx, packageName)
	if len(summaries) == 0 {
		fmt.Println("No Go package symbols found.")
		return nil
	}

	for i := range summaries {
		fmt.Println(formatCodeSymbolSummary(summaries[i]))
	}

	return nil
}

func summarizeCodePackageSymbols(idx codeintel.Index, packageName string) []codeSymbolSummary {
	packageName = strings.TrimSpace(packageName)
	if packageName == "" {
		return nil
	}

	filtered := codeintel.Index{Symbols: make([]codeintel.Symbol, 0)}

	for i := range idx.Files {
		if idx.Files[i].Package == packageName {
			filtered.Symbols = append(filtered.Symbols, idx.Files[i].Symbols...)
		}
	}

	return summarizeCodeSymbols(filtered)
}
