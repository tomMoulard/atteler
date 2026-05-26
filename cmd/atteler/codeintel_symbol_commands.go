package main

import (
	"strings"

	"github.com/tommoulard/atteler/pkg/codeintel"
)

type codeSymbolFileSummary struct {
	Path    string
	Package string
	Symbols int
}

func sortCodeSymbolFileSummaries(summaries []codeSymbolFileSummary) {
	sortCodeIntelByCountDescNameAsc(summaries,
		func(summary codeSymbolFileSummary) int { return summary.Symbols },
		func(summary codeSymbolFileSummary) string { return summary.Path },
	)
}

func sortCodePackageSummariesBySymbolCounts(summaries []codePackageSummary) {
	sortCodeIntelByCountsDescNameAsc(summaries,
		func(summary codePackageSummary) int { return summary.Symbols },
		func(summary codePackageSummary) int { return summary.Files },
		func(summary codePackageSummary) string { return summary.Name },
	)
}

func filterCodeSymbols(symbols []codeintel.Symbol, keep func(codeintel.Symbol) bool) []codeintel.Symbol {
	matches := filterCodeIntelSlice(symbols, keep)
	sortCodeSymbols(matches)

	return matches
}

func countCodeSymbols(symbols []codeintel.Symbol, keep func(codeintel.Symbol) bool) int {
	count := 0

	for i := range symbols {
		if keep(symbols[i]) {
			count++
		}
	}

	return count
}

func summarizeCodeSymbolFilesByPredicate(root string, files []codeintel.File, keep func(codeintel.Symbol) bool) []codeSymbolFileSummary {
	summaries := make([]codeSymbolFileSummary, 0)

	for i := range files {
		file := files[i]

		count := countCodeSymbols(file.Symbols, keep)
		if count == 0 {
			continue
		}

		summaries = append(summaries, codeSymbolFileSummary{
			Path:    relativeCodePath(root, file.Path),
			Package: file.Package,
			Symbols: count,
		})
	}

	sortCodeSymbolFileSummaries(summaries)

	return summaries
}

func summarizeCodeSymbolPackagesByPredicate(files []codeintel.File, keep func(codeintel.Symbol) bool) []codePackageSummary {
	byPackage := make(map[string]*codePackageSummary)
	filesByPackage := make(map[string]map[string]struct{})

	for i := range files {
		file := files[i]
		if file.Package == "" {
			continue
		}

		count := countCodeSymbols(file.Symbols, keep)
		if count == 0 {
			continue
		}

		summary, ok := byPackage[file.Package]
		if !ok {
			summary = &codePackageSummary{Name: file.Package}
			byPackage[file.Package] = summary
			filesByPackage[file.Package] = make(map[string]struct{})
		}

		summary.Symbols += count
		filesByPackage[file.Package][file.Path] = struct{}{}
	}

	summaries := make([]codePackageSummary, 0, len(byPackage))
	for packageName, summary := range byPackage {
		summary.Files = len(filesByPackage[packageName])
		summaries = append(summaries, *summary)
	}

	sortCodePackageSummariesBySymbolCounts(summaries)

	return summaries
}

func summarizeCodeSymbolFiles(root string, idx codeintel.Index) []codeSymbolFileSummary {
	return summarizeCodeSymbolFilesByPredicate(root, idx.Files, func(_ codeintel.Symbol) bool {
		return true
	})
}

type codeSymbolSummary struct {
	Kind  string
	Count int
}

func summarizeCodeSymbols(idx codeintel.Index) []codeSymbolSummary {
	counts := make(map[string]int)

	for i := range idx.Symbols {
		if idx.Symbols[i].Kind != "" {
			counts[idx.Symbols[i].Kind]++
		}
	}

	summaries := make([]codeSymbolSummary, 0, len(counts))
	for kind, count := range counts {
		summaries = append(summaries, codeSymbolSummary{Kind: kind, Count: count})
	}

	sortCodeIntelByCountDescNameAsc(summaries,
		func(summary codeSymbolSummary) int { return summary.Count },
		func(summary codeSymbolSummary) string { return summary.Kind },
	)

	return summaries
}

func summarizeCodeSymbolKindFiles(root string, idx codeintel.Index, kind string) []codeSymbolFileSummary {
	kind = strings.TrimSpace(kind)
	if kind == "" {
		return nil
	}

	return summarizeCodeSymbolFilesByPredicate(root, idx.Files, func(symbol codeintel.Symbol) bool {
		return strings.EqualFold(symbol.Kind, kind)
	})
}

func summarizeCodeSymbolKindPackages(idx codeintel.Index, kind string) []codePackageSummary {
	kind = strings.TrimSpace(kind)
	if kind == "" {
		return nil
	}

	return summarizeCodeSymbolPackagesByPredicate(idx.Files, func(symbol codeintel.Symbol) bool {
		return strings.EqualFold(symbol.Kind, kind)
	})
}

func codeSymbolsByKind(idx codeintel.Index, kind string) []codeintel.Symbol {
	kind = strings.TrimSpace(kind)
	if kind == "" {
		return nil
	}

	return filterCodeSymbols(idx.Symbols, func(symbol codeintel.Symbol) bool {
		return strings.EqualFold(symbol.Kind, kind)
	})
}

func summarizeCodeSymbolPrefixFiles(root string, idx codeintel.Index, prefix string) []codeSymbolFileSummary {
	prefix = strings.TrimSpace(prefix)
	if prefix == "" {
		return nil
	}

	return summarizeCodeSymbolFilesByPredicate(root, idx.Files, func(symbol codeintel.Symbol) bool {
		return strings.HasPrefix(symbol.Name, prefix)
	})
}

func summarizeCodeSymbolPrefixPackages(idx codeintel.Index, prefix string) []codePackageSummary {
	prefix = strings.TrimSpace(prefix)
	if prefix == "" {
		return nil
	}

	return summarizeCodeSymbolPackagesByPredicate(idx.Files, func(symbol codeintel.Symbol) bool {
		return strings.HasPrefix(symbol.Name, prefix)
	})
}

func summarizeCodeSymbolNameFiles(root string, idx codeintel.Index, name string) []codeSymbolFileSummary {
	name = strings.TrimSpace(name)
	if name == "" {
		return nil
	}

	return summarizeCodeSymbolFilesByPredicate(root, idx.Files, func(symbol codeintel.Symbol) bool {
		return symbol.Name == name
	})
}

func summarizeCodeSymbolNamePackages(idx codeintel.Index, name string) []codePackageSummary {
	name = strings.TrimSpace(name)
	if name == "" {
		return nil
	}

	return summarizeCodeSymbolPackagesByPredicate(idx.Files, func(symbol codeintel.Symbol) bool {
		return symbol.Name == name
	})
}

func codeSymbolsWithPrefix(idx codeintel.Index, prefix string) []codeintel.Symbol {
	prefix = strings.TrimSpace(prefix)
	if prefix == "" {
		return nil
	}

	return filterCodeSymbols(idx.Symbols, func(symbol codeintel.Symbol) bool {
		return strings.HasPrefix(symbol.Name, prefix)
	})
}

func sortCodeSymbols(symbols []codeintel.Symbol) {
	sortCodeIntelByNamesAscLineAsc(symbols,
		func(symbol codeintel.Symbol) string { return symbol.Name },
		func(symbol codeintel.Symbol) string { return symbol.File },
		func(symbol codeintel.Symbol) int { return symbol.Line },
	)
}
