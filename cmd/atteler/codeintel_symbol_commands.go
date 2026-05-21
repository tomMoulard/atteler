package main

import (
	"fmt"
	"sort"
	"strconv"
	"strings"

	"github.com/tommoulard/atteler/pkg/codeintel"
)

func findCodeSymbol(root, name string) error {
	idx, err := codeintel.IndexDir(root)
	if err != nil {
		return fmt.Errorf("code symbol: index %s: %w", root, err)
	}

	matches := idx.FindSymbol(name)
	if len(matches) == 0 {
		fmt.Println("No code symbols found.")
		return nil
	}

	for i := range matches {
		fmt.Println(formatCodeSymbol(root, matches[i]))
	}

	return nil
}

type codeSymbolFileSummary struct {
	Path    string
	Package string
	Symbols int
}

func listCodeSymbolFileSummary(root string) error {
	idx, err := codeintel.IndexDir(root)
	if err != nil {
		return fmt.Errorf("code symbol file summary: index %s: %w", root, err)
	}

	summaries := summarizeCodeSymbolFiles(root, idx)
	if len(summaries) == 0 {
		fmt.Println("No code symbols found.")
		return nil
	}

	for i := range summaries {
		fmt.Println(formatCodeSymbolFileSummary(summaries[i]))
	}

	return nil
}

func summarizeCodeSymbolFiles(root string, idx codeintel.Index) []codeSymbolFileSummary {
	summaries := make([]codeSymbolFileSummary, 0)

	for i := range idx.Files {
		file := idx.Files[i]
		if len(file.Symbols) == 0 {
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

func formatCodeSymbolFileSummary(summary codeSymbolFileSummary) string {
	return "path=" + summary.Path + "	package=" + summary.Package + "	symbols=" + strconv.Itoa(summary.Symbols)
}

type codeSymbolSummary struct {
	Kind  string
	Count int
}

func listCodeSymbolSummary(root string) error {
	idx, err := codeintel.IndexDir(root)
	if err != nil {
		return fmt.Errorf("code symbol summary: index %s: %w", root, err)
	}

	summaries := summarizeCodeSymbols(idx)
	if len(summaries) == 0 {
		fmt.Println("No code symbols found.")
		return nil
	}

	for i := range summaries {
		fmt.Println(formatCodeSymbolSummary(summaries[i]))
	}

	return nil
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

	sort.Slice(summaries, func(i, j int) bool {
		if summaries[i].Count != summaries[j].Count {
			return summaries[i].Count > summaries[j].Count
		}

		return summaries[i].Kind < summaries[j].Kind
	})

	return summaries
}

func formatCodeSymbolSummary(summary codeSymbolSummary) string {
	return "kind=" + summary.Kind + "	symbols=" + strconv.Itoa(summary.Count)
}

func listCodeSymbolKindFileSummary(root, kind string) error {
	idx, err := codeintel.IndexDir(root)
	if err != nil {
		return fmt.Errorf("code symbol kind file summary: index %s: %w", root, err)
	}

	summaries := summarizeCodeSymbolKindFiles(root, idx, kind)
	if len(summaries) == 0 {
		fmt.Println("No code symbols found.")
		return nil
	}

	for i := range summaries {
		fmt.Println(formatCodeSymbolFileSummary(summaries[i]))
	}

	return nil
}

func summarizeCodeSymbolKindFiles(root string, idx codeintel.Index, kind string) []codeSymbolFileSummary {
	kind = strings.TrimSpace(kind)
	if kind == "" {
		return nil
	}

	summaries := make([]codeSymbolFileSummary, 0)

	for i := range idx.Files {
		file := idx.Files[i]
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

func listCodeSymbolKindPackageSummary(root, kind string) error {
	idx, err := codeintel.IndexDir(root)
	if err != nil {
		return fmt.Errorf("code symbol kind package summary: index %s: %w", root, err)
	}

	summaries := summarizeCodeSymbolKindPackages(idx, kind)
	if len(summaries) == 0 {
		fmt.Println("No code symbols found.")
		return nil
	}

	for i := range summaries {
		fmt.Println(formatCodePackageSummary(summaries[i]))
	}

	return nil
}

func summarizeCodeSymbolKindPackages(idx codeintel.Index, kind string) []codePackageSummary {
	kind = strings.TrimSpace(kind)
	if kind == "" {
		return nil
	}

	byPackage := make(map[string]*codePackageSummary)
	filesByPackage := make(map[string]map[string]struct{})

	for i := range idx.Files {
		file := idx.Files[i]
		if file.Package == "" {
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

	sort.Slice(summaries, func(i, j int) bool {
		if summaries[i].Symbols != summaries[j].Symbols {
			return summaries[i].Symbols > summaries[j].Symbols
		}

		if summaries[i].Files != summaries[j].Files {
			return summaries[i].Files > summaries[j].Files
		}

		return summaries[i].Name < summaries[j].Name
	})

	return summaries
}

func findCodeSymbolsByKind(root, kind string) error {
	idx, err := codeintel.IndexDir(root)
	if err != nil {
		return fmt.Errorf("code symbol kind: index %s: %w", root, err)
	}

	matches := codeSymbolsByKind(idx, kind)
	if len(matches) == 0 {
		fmt.Println("No code symbols found.")
		return nil
	}

	for i := range matches {
		fmt.Println(formatCodeSymbol(root, matches[i]))
	}

	return nil
}

func codeSymbolsByKind(idx codeintel.Index, kind string) []codeintel.Symbol {
	kind = strings.TrimSpace(strings.ToLower(kind))
	if kind == "" {
		return nil
	}

	matches := make([]codeintel.Symbol, 0)

	for i := range idx.Symbols {
		if strings.EqualFold(idx.Symbols[i].Kind, kind) {
			matches = append(matches, idx.Symbols[i])
		}
	}

	sortCodeSymbols(matches)

	return matches
}

func listCodeSymbolPrefixFileSummary(root, prefix string) error {
	idx, err := codeintel.IndexDir(root)
	if err != nil {
		return fmt.Errorf("code symbol prefix file summary: index %s: %w", root, err)
	}

	summaries := summarizeCodeSymbolPrefixFiles(root, idx, prefix)
	if len(summaries) == 0 {
		fmt.Println("No code symbols found.")
		return nil
	}

	for i := range summaries {
		fmt.Println(formatCodeSymbolFileSummary(summaries[i]))
	}

	return nil
}

func summarizeCodeSymbolPrefixFiles(root string, idx codeintel.Index, prefix string) []codeSymbolFileSummary {
	prefix = strings.TrimSpace(prefix)
	if prefix == "" {
		return nil
	}

	summaries := make([]codeSymbolFileSummary, 0)

	for i := range idx.Files {
		file := idx.Files[i]
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

func listCodeSymbolPrefixPackageSummary(root, prefix string) error {
	idx, err := codeintel.IndexDir(root)
	if err != nil {
		return fmt.Errorf("code symbol prefix package summary: index %s: %w", root, err)
	}

	summaries := summarizeCodeSymbolPrefixPackages(idx, prefix)
	if len(summaries) == 0 {
		fmt.Println("No code symbols found.")
		return nil
	}

	for i := range summaries {
		fmt.Println(formatCodePackageSummary(summaries[i]))
	}

	return nil
}

func summarizeCodeSymbolPrefixPackages(idx codeintel.Index, prefix string) []codePackageSummary {
	prefix = strings.TrimSpace(prefix)
	if prefix == "" {
		return nil
	}

	byPackage := make(map[string]*codePackageSummary)
	filesByPackage := make(map[string]map[string]struct{})

	for i := range idx.Files {
		file := idx.Files[i]
		if file.Package == "" {
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

	sort.Slice(summaries, func(i, j int) bool {
		if summaries[i].Symbols != summaries[j].Symbols {
			return summaries[i].Symbols > summaries[j].Symbols
		}

		if summaries[i].Files != summaries[j].Files {
			return summaries[i].Files > summaries[j].Files
		}

		return summaries[i].Name < summaries[j].Name
	})

	return summaries
}

func listCodeSymbolNameFileSummary(root, name string) error {
	idx, err := codeintel.IndexDir(root)
	if err != nil {
		return fmt.Errorf("code symbol name file summary: index %s: %w", root, err)
	}

	summaries := summarizeCodeSymbolNameFiles(root, idx, name)
	if len(summaries) == 0 {
		fmt.Println("No code symbols found.")
		return nil
	}

	for i := range summaries {
		fmt.Println(formatCodeSymbolFileSummary(summaries[i]))
	}

	return nil
}

func summarizeCodeSymbolNameFiles(root string, idx codeintel.Index, name string) []codeSymbolFileSummary {
	name = strings.TrimSpace(name)
	if name == "" {
		return nil
	}

	summaries := make([]codeSymbolFileSummary, 0)

	for i := range idx.Files {
		file := idx.Files[i]
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

func listCodeSymbolNamePackageSummary(root, name string) error {
	idx, err := codeintel.IndexDir(root)
	if err != nil {
		return fmt.Errorf("code symbol name package summary: index %s: %w", root, err)
	}

	summaries := summarizeCodeSymbolNamePackages(idx, name)
	if len(summaries) == 0 {
		fmt.Println("No code symbols found.")
		return nil
	}

	for i := range summaries {
		fmt.Println(formatCodePackageSummary(summaries[i]))
	}

	return nil
}

func summarizeCodeSymbolNamePackages(idx codeintel.Index, name string) []codePackageSummary {
	name = strings.TrimSpace(name)
	if name == "" {
		return nil
	}

	byPackage := make(map[string]*codePackageSummary)
	filesByPackage := make(map[string]map[string]struct{})

	for i := range idx.Files {
		file := idx.Files[i]
		if file.Package == "" {
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

	sort.Slice(summaries, func(i, j int) bool {
		if summaries[i].Symbols != summaries[j].Symbols {
			return summaries[i].Symbols > summaries[j].Symbols
		}

		if summaries[i].Files != summaries[j].Files {
			return summaries[i].Files > summaries[j].Files
		}

		return summaries[i].Name < summaries[j].Name
	})

	return summaries
}

func findCodeSymbolPrefix(root, prefix string) error {
	idx, err := codeintel.IndexDir(root)
	if err != nil {
		return fmt.Errorf("code symbol prefix: index %s: %w", root, err)
	}

	matches := codeSymbolsWithPrefix(idx, prefix)
	if len(matches) == 0 {
		fmt.Println("No code symbols found.")
		return nil
	}

	for i := range matches {
		fmt.Println(formatCodeSymbol(root, matches[i]))
	}

	return nil
}

func codeSymbolsWithPrefix(idx codeintel.Index, prefix string) []codeintel.Symbol {
	prefix = strings.TrimSpace(prefix)
	if prefix == "" {
		return nil
	}

	matches := make([]codeintel.Symbol, 0)

	for i := range idx.Symbols {
		if strings.HasPrefix(idx.Symbols[i].Name, prefix) {
			matches = append(matches, idx.Symbols[i])
		}
	}

	sortCodeSymbols(matches)

	return matches
}

func sortCodeSymbols(symbols []codeintel.Symbol) {
	sort.Slice(symbols, func(i, j int) bool {
		if symbols[i].Name != symbols[j].Name {
			return symbols[i].Name < symbols[j].Name
		}

		if symbols[i].File != symbols[j].File {
			return symbols[i].File < symbols[j].File
		}

		return symbols[i].Line < symbols[j].Line
	})
}

func formatCodeSymbol(root string, symbol codeintel.Symbol) string {
	path := relativeCodePath(root, symbol.File)

	return strings.Join([]string{
		symbol.Name,
		"kind=" + symbol.Kind,
		"path=" + path,
		"line=" + strconv.Itoa(symbol.Line),
	}, "\t")
}
