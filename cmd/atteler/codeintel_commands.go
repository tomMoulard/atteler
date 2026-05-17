package main

import (
	"errors"
	"fmt"
	"path/filepath"
	"sort"
	"strconv"
	"strings"

	"github.com/tommoulard/atteler/pkg/codegraph"
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

type codeImportFileSummary struct {
	Path    string
	Package string
	Imports int
}

func listCodeImportFileSummary(root string) error {
	idx, err := codeintel.IndexDir(root)
	if err != nil {
		return fmt.Errorf("code import file summary: index %s: %w", root, err)
	}

	summaries := summarizeCodeImportFiles(root, idx)
	if len(summaries) == 0 {
		fmt.Println("No code imports found.")
		return nil
	}

	for i := range summaries {
		fmt.Println(formatCodeImportFileSummary(summaries[i]))
	}

	return nil
}

func summarizeCodeImportFiles(root string, idx codeintel.Index) []codeImportFileSummary {
	summaries := make([]codeImportFileSummary, 0)

	for i := range idx.Files {
		file := idx.Files[i]
		if len(file.Imports) == 0 {
			continue
		}

		summaries = append(summaries, codeImportFileSummary{
			Path:    relativeCodePath(root, file.Path),
			Package: file.Package,
			Imports: len(file.Imports),
		})
	}

	sort.Slice(summaries, func(i, j int) bool {
		if summaries[i].Imports != summaries[j].Imports {
			return summaries[i].Imports > summaries[j].Imports
		}

		return summaries[i].Path < summaries[j].Path
	})

	return summaries
}

func formatCodeImportFileSummary(summary codeImportFileSummary) string {
	return "path=" + summary.Path + "	package=" + summary.Package + "	imports=" + strconv.Itoa(summary.Imports)
}

type codeImportSummary struct {
	Path  string
	Files int
}

func listCodeImportSummary(root string) error {
	idx, err := codeintel.IndexDir(root)
	if err != nil {
		return fmt.Errorf("code import summary: index %s: %w", root, err)
	}

	summaries := summarizeCodeImports(idx)
	if len(summaries) == 0 {
		fmt.Println("No code imports found.")
		return nil
	}

	for i := range summaries {
		fmt.Println(formatCodeImportSummary(summaries[i]))
	}

	return nil
}

func summarizeCodeImports(idx codeintel.Index) []codeImportSummary {
	filesByImport := make(map[string]map[string]struct{})

	for i := range idx.ImportEdges {
		edge := idx.ImportEdges[i]
		if edge.Import == "" {
			continue
		}

		if _, ok := filesByImport[edge.Import]; !ok {
			filesByImport[edge.Import] = make(map[string]struct{})
		}

		filesByImport[edge.Import][edge.From] = struct{}{}
	}

	summaries := make([]codeImportSummary, 0, len(filesByImport))
	for importPath, files := range filesByImport {
		summaries = append(summaries, codeImportSummary{Path: importPath, Files: len(files)})
	}

	sort.Slice(summaries, func(i, j int) bool {
		if summaries[i].Files != summaries[j].Files {
			return summaries[i].Files > summaries[j].Files
		}

		return summaries[i].Path < summaries[j].Path
	})

	return summaries
}

func formatCodeImportSummary(summary codeImportSummary) string {
	return "import=" + summary.Path + "	files=" + strconv.Itoa(summary.Files)
}

func listCodeImportPrefixSummary(root, prefix string) error {
	idx, err := codeintel.IndexDir(root)
	if err != nil {
		return fmt.Errorf("code import prefix summary: index %s: %w", root, err)
	}

	summaries := summarizeCodeImportPrefix(idx, prefix)
	if len(summaries) == 0 {
		fmt.Println("No code imports found.")
		return nil
	}

	for i := range summaries {
		fmt.Println(formatCodeImportSummary(summaries[i]))
	}

	return nil
}

func summarizeCodeImportPrefix(idx codeintel.Index, prefix string) []codeImportSummary {
	prefix = strings.TrimSpace(prefix)
	if prefix == "" {
		return nil
	}

	filtered := codeintel.Index{ImportEdges: make([]codeintel.ImportEdge, 0)}
	for i := range idx.ImportEdges {
		if strings.HasPrefix(idx.ImportEdges[i].Import, prefix) {
			filtered.ImportEdges = append(filtered.ImportEdges, idx.ImportEdges[i])
		}
	}

	return summarizeCodeImports(filtered)
}

func listCodeImportPrefixFileSummary(root, prefix string) error {
	idx, err := codeintel.IndexDir(root)
	if err != nil {
		return fmt.Errorf("code import prefix file summary: index %s: %w", root, err)
	}

	summaries := summarizeCodeImportPrefixFiles(root, idx, prefix)
	if len(summaries) == 0 {
		fmt.Println("No code imports found.")
		return nil
	}

	for i := range summaries {
		fmt.Println(formatCodeImportFileSummary(summaries[i]))
	}

	return nil
}

func summarizeCodeImportPrefixFiles(root string, idx codeintel.Index, prefix string) []codeImportFileSummary {
	prefix = strings.TrimSpace(prefix)
	if prefix == "" {
		return nil
	}

	packagesByFile := make(map[string]string, len(idx.Files))
	for i := range idx.Files {
		packagesByFile[idx.Files[i].Path] = idx.Files[i].Package
	}

	importsByFile := make(map[string]map[string]struct{})

	for i := range idx.ImportEdges {
		edge := idx.ImportEdges[i]
		if !strings.HasPrefix(edge.Import, prefix) {
			continue
		}

		if _, ok := importsByFile[edge.From]; !ok {
			importsByFile[edge.From] = make(map[string]struct{})
		}

		importsByFile[edge.From][edge.Import] = struct{}{}
	}

	summaries := make([]codeImportFileSummary, 0, len(importsByFile))
	for file, imports := range importsByFile {
		summaries = append(summaries, codeImportFileSummary{
			Path:    relativeCodePath(root, file),
			Package: packagesByFile[file],
			Imports: len(imports),
		})
	}

	sort.Slice(summaries, func(i, j int) bool {
		if summaries[i].Imports != summaries[j].Imports {
			return summaries[i].Imports > summaries[j].Imports
		}

		return summaries[i].Path < summaries[j].Path
	})

	return summaries
}

func listCodeImportPrefixPackageSummary(root, prefix string) error {
	idx, err := codeintel.IndexDir(root)
	if err != nil {
		return fmt.Errorf("code import prefix package summary: index %s: %w", root, err)
	}

	summaries := summarizeCodeImportPrefixPackages(idx, prefix)
	if len(summaries) == 0 {
		fmt.Println("No code imports found.")
		return nil
	}

	for i := range summaries {
		fmt.Println(formatCodePackageImportMatchSummary(summaries[i]))
	}

	return nil
}

func summarizeCodeImportPrefixPackages(idx codeintel.Index, prefix string) []codePackageImportMatchSummary {
	prefix = strings.TrimSpace(prefix)
	if prefix == "" {
		return nil
	}

	packagesByFile := make(map[string]string, len(idx.Files))
	for i := range idx.Files {
		packagesByFile[idx.Files[i].Path] = idx.Files[i].Package
	}

	filesByPackage := make(map[string]map[string]struct{})
	importsByPackage := make(map[string]map[string]struct{})

	for i := range idx.ImportEdges {
		edge := idx.ImportEdges[i]
		if !strings.HasPrefix(edge.Import, prefix) {
			continue
		}

		packageName := packagesByFile[edge.From]
		if packageName == "" {
			continue
		}

		if _, ok := filesByPackage[packageName]; !ok {
			filesByPackage[packageName] = make(map[string]struct{})
			importsByPackage[packageName] = make(map[string]struct{})
		}

		filesByPackage[packageName][edge.From] = struct{}{}
		importsByPackage[packageName][edge.Import] = struct{}{}
	}

	summaries := make([]codePackageImportMatchSummary, 0, len(filesByPackage))
	for packageName, files := range filesByPackage {
		summaries = append(summaries, codePackageImportMatchSummary{
			Name:    packageName,
			Files:   len(files),
			Imports: len(importsByPackage[packageName]),
		})
	}

	sortCodePackageImportMatchSummaries(summaries)

	return summaries
}

func listCodeImportPrefix(root, prefix string) error {
	idx, err := codeintel.IndexDir(root)
	if err != nil {
		return fmt.Errorf("code import prefix: index %s: %w", root, err)
	}

	edges := codeImportEdgesWithPrefix(idx, prefix)
	if len(edges) == 0 {
		fmt.Println("No code imports found.")
		return nil
	}

	for i := range edges {
		fmt.Println(formatCodeImportEdge(root, edges[i]))
	}

	return nil
}

func codeImportEdgesWithPrefix(idx codeintel.Index, prefix string) []codeintel.ImportEdge {
	prefix = strings.TrimSpace(prefix)
	if prefix == "" {
		return nil
	}

	edges := make([]codeintel.ImportEdge, 0)

	for i := range idx.ImportEdges {
		if strings.HasPrefix(idx.ImportEdges[i].Import, prefix) {
			edges = append(edges, idx.ImportEdges[i])
		}
	}

	sortCodeImportEdges(edges)

	return edges
}

func listCodeImportPath(root, importPath string) error {
	idx, err := codeintel.IndexDir(root)
	if err != nil {
		return fmt.Errorf("code import path: index %s: %w", root, err)
	}

	edges := codeImportEdgesForPath(idx, importPath)
	if len(edges) == 0 {
		fmt.Println("No code imports found.")
		return nil
	}

	for i := range edges {
		fmt.Println(formatCodeImportEdge(root, edges[i]))
	}

	return nil
}

func codeImportEdgesForPath(idx codeintel.Index, importPath string) []codeintel.ImportEdge {
	importPath = strings.TrimSpace(importPath)
	if importPath == "" {
		return nil
	}

	edges := make([]codeintel.ImportEdge, 0)

	for i := range idx.ImportEdges {
		if idx.ImportEdges[i].Import == importPath {
			edges = append(edges, idx.ImportEdges[i])
		}
	}

	sortCodeImportEdges(edges)

	return edges
}

func listCodeImportPathSummary(root, importPath string) error {
	idx, err := codeintel.IndexDir(root)
	if err != nil {
		return fmt.Errorf("code import path summary: index %s: %w", root, err)
	}

	summaries := summarizeCodeImportPath(idx, importPath)
	if len(summaries) == 0 {
		fmt.Println("No code imports found.")
		return nil
	}

	for i := range summaries {
		fmt.Println(formatCodeImportSummary(summaries[i]))
	}

	return nil
}

func summarizeCodeImportPath(idx codeintel.Index, importPath string) []codeImportSummary {
	edges := codeImportEdgesForPath(idx, importPath)
	if len(edges) == 0 {
		return nil
	}

	return summarizeCodeImports(codeintel.Index{ImportEdges: edges})
}

func listCodeImportPathFileSummary(root, importPath string) error {
	idx, err := codeintel.IndexDir(root)
	if err != nil {
		return fmt.Errorf("code import path file summary: index %s: %w", root, err)
	}

	summaries := summarizeCodeImportPathFiles(root, idx, importPath)
	if len(summaries) == 0 {
		fmt.Println("No code imports found.")
		return nil
	}

	for i := range summaries {
		fmt.Println(formatCodeImportFileSummary(summaries[i]))
	}

	return nil
}

func summarizeCodeImportPathFiles(root string, idx codeintel.Index, importPath string) []codeImportFileSummary {
	importPath = strings.TrimSpace(importPath)
	if importPath == "" {
		return nil
	}

	packagesByFile := make(map[string]string, len(idx.Files))
	for i := range idx.Files {
		packagesByFile[idx.Files[i].Path] = idx.Files[i].Package
	}

	files := make(map[string]struct{})

	for i := range idx.ImportEdges {
		edge := idx.ImportEdges[i]
		if edge.Import == importPath {
			files[edge.From] = struct{}{}
		}
	}

	summaries := make([]codeImportFileSummary, 0, len(files))
	for file := range files {
		summaries = append(summaries, codeImportFileSummary{
			Path:    relativeCodePath(root, file),
			Package: packagesByFile[file],
			Imports: 1,
		})
	}

	sort.Slice(summaries, func(i, j int) bool {
		return summaries[i].Path < summaries[j].Path
	})

	return summaries
}

func listCodeImportPathPackageSummary(root, importPath string) error {
	idx, err := codeintel.IndexDir(root)
	if err != nil {
		return fmt.Errorf("code import path package summary: index %s: %w", root, err)
	}

	summaries := summarizeCodeImportPathPackages(idx, importPath)
	if len(summaries) == 0 {
		fmt.Println("No code imports found.")
		return nil
	}

	for i := range summaries {
		fmt.Println(formatCodePackageImportMatchSummary(summaries[i]))
	}

	return nil
}

func summarizeCodeImportPathPackages(idx codeintel.Index, importPath string) []codePackageImportMatchSummary {
	importPath = strings.TrimSpace(importPath)
	if importPath == "" {
		return nil
	}

	packagesByFile := make(map[string]string, len(idx.Files))
	for i := range idx.Files {
		packagesByFile[idx.Files[i].Path] = idx.Files[i].Package
	}

	filesByPackage := make(map[string]map[string]struct{})

	for i := range idx.ImportEdges {
		edge := idx.ImportEdges[i]
		if edge.Import != importPath {
			continue
		}

		packageName := packagesByFile[edge.From]
		if packageName == "" {
			continue
		}

		if _, ok := filesByPackage[packageName]; !ok {
			filesByPackage[packageName] = make(map[string]struct{})
		}

		filesByPackage[packageName][edge.From] = struct{}{}
	}

	summaries := make([]codePackageImportMatchSummary, 0, len(filesByPackage))
	for packageName, files := range filesByPackage {
		summaries = append(summaries, codePackageImportMatchSummary{
			Name:  packageName,
			Files: len(files),
		})
	}

	sortCodePackageImportMatchSummaries(summaries)

	return summaries
}

func sortCodeImportEdges(edges []codeintel.ImportEdge) {
	sort.Slice(edges, func(i, j int) bool {
		if edges[i].From != edges[j].From {
			return edges[i].From < edges[j].From
		}

		return edges[i].Import < edges[j].Import
	})
}

func listCodeImports(root string) error {
	idx, err := codeintel.IndexDir(root)
	if err != nil {
		return fmt.Errorf("code imports: index %s: %w", root, err)
	}

	if len(idx.ImportEdges) == 0 {
		fmt.Println("No code imports found.")
		return nil
	}

	for i := range idx.ImportEdges {
		fmt.Println(formatCodeImportEdge(root, idx.ImportEdges[i]))
	}

	return nil
}

func listCodeImpact(root, target string) error {
	graph, err := importGraph(root)
	if err != nil {
		return fmt.Errorf("code impact: %w", err)
	}

	impact := graph.ImpactSet(normalizeCodeGraphTarget(root, target))
	if len(impact) == 0 {
		fmt.Println("No code impact found.")
		return nil
	}

	for _, node := range impact {
		fmt.Println("path=" + string(node))
	}

	return nil
}

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

func showCodeFile(root, target string) error {
	idx, err := codeintel.IndexDir(root)
	if err != nil {
		return fmt.Errorf("code file: index %s: %w", root, err)
	}

	file, ok := findCodeFile(root, idx, target)
	if !ok {
		fmt.Println("No Go code file found.")
		return nil
	}

	printCodeFile(root, file)

	return nil
}

func findCodeFile(root string, idx codeintel.Index, target string) (codeintel.File, bool) {
	target = filepath.ToSlash(strings.TrimSpace(target))

	for i := range idx.Files {
		rel := relativeCodePath(root, idx.Files[i].Path)

		abs := filepath.ToSlash(idx.Files[i].Path)
		if rel == target || abs == target {
			return idx.Files[i], true
		}
	}

	return codeintel.File{}, false
}

func printCodeFile(root string, file codeintel.File) {
	fmt.Println(formatCodeFile(root, file))

	if len(file.Imports) > 0 {
		fmt.Println("imports:")

		for _, imp := range file.Imports {
			fmt.Println("  - " + imp)
		}
	}

	if len(file.Symbols) > 0 {
		fmt.Println("symbols:")

		for i := range file.Symbols {
			fmt.Println("  - " + formatCodeFileSymbol(file.Symbols[i]))
		}
	}
}

func formatCodeFile(root string, file codeintel.File) string {
	return "path=" + relativeCodePath(root, file.Path) + "	package=" + file.Package + "	imports=" + strconv.Itoa(len(file.Imports)) + "	symbols=" + strconv.Itoa(len(file.Symbols))
}

func formatCodeFileSymbol(symbol codeintel.Symbol) string {
	return symbol.Name + "	kind=" + symbol.Kind + "	line=" + strconv.Itoa(symbol.Line)
}

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

func listCodePackageImportFileSummary(root, packageName string) error {
	idx, err := codeintel.IndexDir(root)
	if err != nil {
		return fmt.Errorf("code package import file summary: index %s: %w", root, err)
	}

	summaries := summarizeCodePackageImportFiles(root, idx, packageName)
	if len(summaries) == 0 {
		fmt.Println("No Go package imports found.")
		return nil
	}

	for i := range summaries {
		fmt.Println(formatCodeImportFileSummary(summaries[i]))
	}

	return nil
}

func summarizeCodePackageImportFiles(root string, idx codeintel.Index, packageName string) []codeImportFileSummary {
	packageName = strings.TrimSpace(packageName)
	if packageName == "" {
		return nil
	}

	summaries := make([]codeImportFileSummary, 0)

	for i := range idx.Files {
		file := idx.Files[i]
		if file.Package != packageName || len(file.Imports) == 0 {
			continue
		}

		summaries = append(summaries, codeImportFileSummary{
			Path:    relativeCodePath(root, file.Path),
			Package: file.Package,
			Imports: len(file.Imports),
		})
	}

	sort.Slice(summaries, func(i, j int) bool {
		if summaries[i].Imports != summaries[j].Imports {
			return summaries[i].Imports > summaries[j].Imports
		}

		return summaries[i].Path < summaries[j].Path
	})

	return summaries
}

func listCodePackageImportPrefixFileSummary(root, spec string) error {
	packageName, prefix, err := parseCodeFileSymbolFilterSpec(spec, "code package import prefix file summary", "package:prefix")
	if err != nil {
		return err
	}

	idx, err := codeintel.IndexDir(root)
	if err != nil {
		return fmt.Errorf("code package import prefix file summary: index %s: %w", root, err)
	}

	summaries := summarizeCodePackageImportPrefixFiles(root, idx, packageName, prefix)
	if len(summaries) == 0 {
		fmt.Println("No Go package import files found.")
		return nil
	}

	for i := range summaries {
		fmt.Println(formatCodeImportFileSummary(summaries[i]))
	}

	return nil
}

func summarizeCodePackageImportPrefixFiles(root string, idx codeintel.Index, packageName, prefix string) []codeImportFileSummary {
	edges := codePackageImportPrefixFiles(idx, packageName, prefix)
	if len(edges) == 0 {
		return edgesToCodeImportFileSummaries(root, idx, nil)
	}

	return edgesToCodeImportFileSummaries(root, idx, edges)
}

func edgesToCodeImportFileSummaries(root string, idx codeintel.Index, edges []codeintel.ImportEdge) []codeImportFileSummary {
	if len(edges) == 0 {
		return nil
	}

	packagesByFile := make(map[string]string, len(idx.Files))
	for i := range idx.Files {
		packagesByFile[idx.Files[i].Path] = idx.Files[i].Package
	}

	importsByFile := make(map[string]map[string]struct{})

	for i := range edges {
		edge := edges[i]
		if _, ok := importsByFile[edge.From]; !ok {
			importsByFile[edge.From] = make(map[string]struct{})
		}

		importsByFile[edge.From][edge.Import] = struct{}{}
	}

	summaries := make([]codeImportFileSummary, 0, len(importsByFile))
	for file, imports := range importsByFile {
		summaries = append(summaries, codeImportFileSummary{
			Path:    relativeCodePath(root, file),
			Package: packagesByFile[file],
			Imports: len(imports),
		})
	}

	sort.Slice(summaries, func(i, j int) bool {
		if summaries[i].Imports != summaries[j].Imports {
			return summaries[i].Imports > summaries[j].Imports
		}

		return summaries[i].Path < summaries[j].Path
	})

	return summaries
}

func listCodePackageImportPrefixFiles(root, spec string) error {
	packageName, prefix, err := parseCodeFileSymbolFilterSpec(spec, "code package import prefix files", "package:prefix")
	if err != nil {
		return err
	}

	idx, err := codeintel.IndexDir(root)
	if err != nil {
		return fmt.Errorf("code package import prefix files: index %s: %w", root, err)
	}

	edges := codePackageImportPrefixFiles(idx, packageName, prefix)
	if len(edges) == 0 {
		fmt.Println("No Go package import files found.")
		return nil
	}

	for i := range edges {
		fmt.Println(formatCodeImportEdge(root, edges[i]))
	}

	return nil
}

func codePackageImportPrefixFiles(idx codeintel.Index, packageName, prefix string) []codeintel.ImportEdge {
	packageName = strings.TrimSpace(packageName)

	prefix = strings.TrimSpace(prefix)
	if packageName == "" || prefix == "" {
		return nil
	}

	packageFiles := make(map[string]struct{})

	for i := range idx.Files {
		if idx.Files[i].Package == packageName {
			packageFiles[idx.Files[i].Path] = struct{}{}
		}
	}

	if len(packageFiles) == 0 {
		return nil
	}

	seen := make(map[string]struct{})
	edges := make([]codeintel.ImportEdge, 0)

	for i := range idx.ImportEdges {
		edge := idx.ImportEdges[i]
		if !strings.HasPrefix(edge.Import, prefix) {
			continue
		}

		if _, ok := packageFiles[edge.From]; !ok {
			continue
		}

		seenKey := edge.From + "\x00" + edge.Import
		if _, ok := seen[seenKey]; ok {
			continue
		}

		seen[seenKey] = struct{}{}

		edges = append(edges, edge)
	}

	sortCodeImportEdges(edges)

	return edges
}

func listCodePackageImportFiles(root, spec string) error {
	packageName, importPath, err := parseCodeFileSymbolFilterSpec(spec, "code package import files", "package:import")
	if err != nil {
		return err
	}

	idx, err := codeintel.IndexDir(root)
	if err != nil {
		return fmt.Errorf("code package import files: index %s: %w", root, err)
	}

	files := codePackageImportFiles(root, idx, packageName, importPath)
	if len(files) == 0 {
		fmt.Println("No Go package import files found.")
		return nil
	}

	for _, file := range files {
		fmt.Println("path=" + file + "	import=" + importPath)
	}

	return nil
}

func codePackageImportFiles(root string, idx codeintel.Index, packageName, importPath string) []string {
	packageName = strings.TrimSpace(packageName)

	importPath = strings.TrimSpace(importPath)
	if packageName == "" || importPath == "" {
		return nil
	}

	packageFiles := make(map[string]struct{})

	for i := range idx.Files {
		if idx.Files[i].Package == packageName {
			packageFiles[idx.Files[i].Path] = struct{}{}
		}
	}

	if len(packageFiles) == 0 {
		return nil
	}

	seen := make(map[string]struct{})
	files := make([]string, 0)

	for i := range idx.ImportEdges {
		edge := idx.ImportEdges[i]
		if edge.Import != importPath {
			continue
		}

		if _, ok := packageFiles[edge.From]; !ok {
			continue
		}

		rel := relativeCodePath(root, edge.From)
		if _, ok := seen[rel]; ok {
			continue
		}

		seen[rel] = struct{}{}
		files = append(files, rel)
	}

	sort.Strings(files)

	return files
}

func listCodePackageImportPathFileSummary(root, spec string) error {
	packageName, importPath, err := parseCodeFileSymbolFilterSpec(spec, "code package import path file summary", "package:import")
	if err != nil {
		return err
	}

	idx, err := codeintel.IndexDir(root)
	if err != nil {
		return fmt.Errorf("code package import path file summary: index %s: %w", root, err)
	}

	summaries := summarizeCodePackageImportPathFiles(root, idx, packageName, importPath)
	if len(summaries) == 0 {
		fmt.Println("No Go package import files found.")
		return nil
	}

	for i := range summaries {
		fmt.Println(formatCodeImportFileSummary(summaries[i]))
	}

	return nil
}

func summarizeCodePackageImportPathFiles(root string, idx codeintel.Index, packageName, importPath string) []codeImportFileSummary {
	packageName = strings.TrimSpace(packageName)

	importPath = strings.TrimSpace(importPath)
	if packageName == "" || importPath == "" {
		return nil
	}

	packageFiles := make(map[string]struct{})

	for i := range idx.Files {
		if idx.Files[i].Package == packageName {
			packageFiles[idx.Files[i].Path] = struct{}{}
		}
	}

	if len(packageFiles) == 0 {
		return nil
	}

	files := make(map[string]struct{})

	for i := range idx.ImportEdges {
		edge := idx.ImportEdges[i]
		if edge.Import != importPath {
			continue
		}

		if _, ok := packageFiles[edge.From]; !ok {
			continue
		}

		files[edge.From] = struct{}{}
	}

	summaries := make([]codeImportFileSummary, 0, len(files))
	for file := range files {
		summaries = append(summaries, codeImportFileSummary{
			Path:    relativeCodePath(root, file),
			Package: packageName,
			Imports: 1,
		})
	}

	sort.Slice(summaries, func(i, j int) bool {
		return summaries[i].Path < summaries[j].Path
	})

	return summaries
}

func listCodePackageImportPath(root, spec string) error {
	packageName, importPath, err := parseCodeFileSymbolFilterSpec(spec, "code package import path", "package:import")
	if err != nil {
		return err
	}

	idx, err := codeintel.IndexDir(root)
	if err != nil {
		return fmt.Errorf("code package import path: index %s: %w", root, err)
	}

	summaries := summarizeCodePackageImportPath(idx, packageName, importPath)
	if len(summaries) == 0 {
		fmt.Println("No Go package imports found.")
		return nil
	}

	for i := range summaries {
		fmt.Println(formatCodeImportSummary(summaries[i]))
	}

	return nil
}

func summarizeCodePackageImportPath(idx codeintel.Index, packageName, importPath string) []codeImportSummary {
	packageName = strings.TrimSpace(packageName)

	importPath = strings.TrimSpace(importPath)
	if packageName == "" || importPath == "" {
		return nil
	}

	all := summarizeCodePackageImports(idx, packageName)
	filtered := make([]codeImportSummary, 0, 1)

	for i := range all {
		if all[i].Path == importPath {
			filtered = append(filtered, all[i])
		}
	}

	return filtered
}

func listCodePackageImportPrefix(root, spec string) error {
	packageName, prefix, err := parseCodeFileSymbolFilterSpec(spec, "code package import prefix", "package:prefix")
	if err != nil {
		return err
	}

	idx, err := codeintel.IndexDir(root)
	if err != nil {
		return fmt.Errorf("code package import prefix: index %s: %w", root, err)
	}

	summaries := summarizeCodePackageImportPrefix(idx, packageName, prefix)
	if len(summaries) == 0 {
		fmt.Println("No Go package imports found.")
		return nil
	}

	for i := range summaries {
		fmt.Println(formatCodeImportSummary(summaries[i]))
	}

	return nil
}

func summarizeCodePackageImportPrefix(idx codeintel.Index, packageName, prefix string) []codeImportSummary {
	packageName = strings.TrimSpace(packageName)

	prefix = strings.TrimSpace(prefix)
	if packageName == "" || prefix == "" {
		return nil
	}

	all := summarizeCodePackageImports(idx, packageName)

	filtered := make([]codeImportSummary, 0, len(all))
	for i := range all {
		if strings.HasPrefix(all[i].Path, prefix) {
			filtered = append(filtered, all[i])
		}
	}

	return filtered
}

func listCodePackageImports(root, packageName string) error {
	idx, err := codeintel.IndexDir(root)
	if err != nil {
		return fmt.Errorf("code package imports: index %s: %w", root, err)
	}

	summaries := summarizeCodePackageImports(idx, packageName)
	if len(summaries) == 0 {
		fmt.Println("No Go package imports found.")
		return nil
	}

	for i := range summaries {
		fmt.Println(formatCodeImportSummary(summaries[i]))
	}

	return nil
}

func summarizeCodePackageImports(idx codeintel.Index, packageName string) []codeImportSummary {
	packageName = strings.TrimSpace(packageName)
	if packageName == "" {
		return nil
	}

	files := make(map[string]struct{})

	for i := range idx.Files {
		if idx.Files[i].Package == packageName {
			files[idx.Files[i].Path] = struct{}{}
		}
	}

	if len(files) == 0 {
		return nil
	}

	filtered := codeintel.Index{ImportEdges: make([]codeintel.ImportEdge, 0)}
	for i := range idx.ImportEdges {
		if _, ok := files[idx.ImportEdges[i].From]; ok {
			filtered.ImportEdges = append(filtered.ImportEdges, idx.ImportEdges[i])
		}
	}

	return summarizeCodeImports(filtered)
}

func listCodeFiles(root string) error {
	idx, err := codeintel.IndexDir(root)
	if err != nil {
		return fmt.Errorf("code files: index %s: %w", root, err)
	}

	files := summarizeCodeFiles(root, idx)
	if len(files) == 0 {
		fmt.Println("No Go files found.")
		return nil
	}

	for i := range files {
		fmt.Println(formatCodePackageFile(files[i]))
	}

	return nil
}

func summarizeCodeFiles(root string, idx codeintel.Index) []codePackageFile {
	files := make([]codePackageFile, 0, len(idx.Files))
	for i := range idx.Files {
		file := idx.Files[i]
		files = append(files, codePackageFile{
			Path:    relativeCodePath(root, file.Path),
			Package: file.Package,
			Symbols: len(file.Symbols),
			Imports: len(file.Imports),
		})
	}

	sort.Slice(files, func(i, j int) bool { return files[i].Path < files[j].Path })

	return files
}

func listCodePackageFiles(root, name string) error {
	idx, err := codeintel.IndexDir(root)
	if err != nil {
		return fmt.Errorf("code package: index %s: %w", root, err)
	}

	files := summarizeCodePackageFiles(root, idx, name)
	if len(files) == 0 {
		fmt.Println("No Go package files found.")
		return nil
	}

	for i := range files {
		fmt.Println(formatCodePackageFile(files[i]))
	}

	return nil
}

type codePackageFile struct {
	Path    string
	Package string
	Symbols int
	Imports int
}

func summarizeCodePackageFiles(root string, idx codeintel.Index, name string) []codePackageFile {
	name = strings.TrimSpace(name)
	files := make([]codePackageFile, 0)

	for i := range idx.Files {
		file := idx.Files[i]
		if file.Package != name {
			continue
		}

		files = append(files, codePackageFile{
			Path:    relativeCodePath(root, file.Path),
			Package: file.Package,
			Symbols: len(file.Symbols),
			Imports: len(file.Imports),
		})
	}

	sort.Slice(files, func(i, j int) bool { return files[i].Path < files[j].Path })

	return files
}

func formatCodePackageFile(file codePackageFile) string {
	return "path=" + file.Path + "	package=" + file.Package + "	symbols=" + strconv.Itoa(file.Symbols) + "	imports=" + strconv.Itoa(file.Imports)
}

type codePackageImportSummary struct {
	Name          string
	Files         int
	Imports       int
	UniqueImports int
}

type codePackageImportMatchSummary struct {
	Name    string
	Files   int
	Imports int
}

func formatCodePackageImportMatchSummary(summary codePackageImportMatchSummary) string {
	parts := []string{
		"package=" + summary.Name,
		"files=" + strconv.Itoa(summary.Files),
	}
	if summary.Imports > 0 {
		parts = append(parts, "imports="+strconv.Itoa(summary.Imports))
	}

	return strings.Join(parts, "\t")
}

func sortCodePackageImportMatchSummaries(summaries []codePackageImportMatchSummary) {
	sort.Slice(summaries, func(i, j int) bool {
		if summaries[i].Imports != summaries[j].Imports {
			return summaries[i].Imports > summaries[j].Imports
		}

		if summaries[i].Files != summaries[j].Files {
			return summaries[i].Files > summaries[j].Files
		}

		return summaries[i].Name < summaries[j].Name
	})
}

func listCodePackageImportSummary(root string) error {
	idx, err := codeintel.IndexDir(root)
	if err != nil {
		return fmt.Errorf("code package import summary: index %s: %w", root, err)
	}

	summaries := summarizeCodePackageImportCounts(idx)
	if len(summaries) == 0 {
		fmt.Println("No Go package imports found.")
		return nil
	}

	for i := range summaries {
		fmt.Println(formatCodePackageImportSummary(summaries[i]))
	}

	return nil
}

func summarizeCodePackageImportCounts(idx codeintel.Index) []codePackageImportSummary {
	byPackage := make(map[string]*codePackageImportSummary)
	uniqueByPackage := make(map[string]map[string]struct{})

	for i := range idx.Files {
		file := idx.Files[i]
		if file.Package == "" {
			continue
		}

		summary, ok := byPackage[file.Package]
		if !ok {
			summary = &codePackageImportSummary{Name: file.Package}
			byPackage[file.Package] = summary
			uniqueByPackage[file.Package] = make(map[string]struct{})
		}

		summary.Files++

		summary.Imports += len(file.Imports)
		for _, imp := range file.Imports {
			if imp != "" {
				uniqueByPackage[file.Package][imp] = struct{}{}
			}
		}
	}

	summaries := make([]codePackageImportSummary, 0, len(byPackage))
	for name, summary := range byPackage {
		summary.UniqueImports = len(uniqueByPackage[name])
		if summary.Imports == 0 {
			continue
		}

		summaries = append(summaries, *summary)
	}

	sort.Slice(summaries, func(i, j int) bool {
		if summaries[i].Imports != summaries[j].Imports {
			return summaries[i].Imports > summaries[j].Imports
		}

		return summaries[i].Name < summaries[j].Name
	})

	return summaries
}

func formatCodePackageImportSummary(summary codePackageImportSummary) string {
	return "package=" + summary.Name + "	files=" + strconv.Itoa(summary.Files) + "	imports=" + strconv.Itoa(summary.Imports) + "	unique_imports=" + strconv.Itoa(summary.UniqueImports)
}

type codePackageSummary struct {
	Name    string
	Files   int
	Symbols int
}

func listCodePackages(root string) error {
	idx, err := codeintel.IndexDir(root)
	if err != nil {
		return fmt.Errorf("code packages: index %s: %w", root, err)
	}

	packages := summarizeCodePackages(idx)
	if len(packages) == 0 {
		fmt.Println("No Go packages found.")
		return nil
	}

	for i := range packages {
		fmt.Println(formatCodePackageSummary(packages[i]))
	}

	return nil
}

func summarizeCodePackages(idx codeintel.Index) []codePackageSummary {
	byPackage := make(map[string]*codePackageSummary)

	for i := range idx.Files {
		name := idx.Files[i].Package
		if name == "" {
			continue
		}

		summary, ok := byPackage[name]
		if !ok {
			summary = &codePackageSummary{Name: name}
			byPackage[name] = summary
		}

		summary.Files++
		summary.Symbols += len(idx.Files[i].Symbols)
	}

	packages := make([]codePackageSummary, 0, len(byPackage))
	for _, summary := range byPackage {
		packages = append(packages, *summary)
	}

	sort.Slice(packages, func(i, j int) bool {
		if packages[i].Name != packages[j].Name {
			return packages[i].Name < packages[j].Name
		}

		return packages[i].Files < packages[j].Files
	})

	return packages
}

func formatCodePackageSummary(summary codePackageSummary) string {
	return "package=" + summary.Name + "	files=" + strconv.Itoa(summary.Files) + "	symbols=" + strconv.Itoa(summary.Symbols)
}

type codeSummary struct {
	Files    int
	Packages int
	Symbols  int
	Imports  int
	Nodes    int
	Edges    int
	Cycles   int
	Layers   int
}

func printCodeSummary(root string) error {
	idx, err := codeintel.IndexDir(root)
	if err != nil {
		return fmt.Errorf("code summary: index %s: %w", root, err)
	}

	graph := importGraphFromIndex(root, idx)
	layers, layerErr := graph.TopologicalLayers()

	summary := codeSummary{
		Files:    len(idx.Files),
		Packages: countPackages(idx.Files),
		Symbols:  len(idx.Symbols),
		Imports:  len(idx.ImportEdges),
		Nodes:    len(graph.Nodes()),
		Edges:    len(graph.Edges()),
		Cycles:   len(graph.Cycles()),
	}
	if layerErr == nil {
		summary.Layers = len(layers)
	}

	fmt.Println(formatCodeSummary(summary))

	return nil
}

func countPackages(files []codeintel.File) int {
	seen := make(map[string]struct{})

	for i := range files {
		if files[i].Package != "" {
			seen[files[i].Package] = struct{}{}
		}
	}

	return len(seen)
}

func formatCodeSummary(summary codeSummary) string {
	return strings.Join([]string{
		"files=" + strconv.Itoa(summary.Files),
		"packages=" + strconv.Itoa(summary.Packages),
		"symbols=" + strconv.Itoa(summary.Symbols),
		"imports=" + strconv.Itoa(summary.Imports),
		"nodes=" + strconv.Itoa(summary.Nodes),
		"edges=" + strconv.Itoa(summary.Edges),
		"cycles=" + strconv.Itoa(summary.Cycles),
		"layers=" + strconv.Itoa(summary.Layers),
	}, "	")
}

func listCodeCycles(root string) error {
	graph, err := importGraph(root)
	if err != nil {
		return fmt.Errorf("code cycles: %w", err)
	}

	cycles := graph.Cycles()
	if len(cycles) == 0 {
		fmt.Println("No code graph cycles found.")
		return nil
	}

	for i := range cycles {
		fmt.Println(formatCodeCycle(i+1, cycles[i]))
	}

	return nil
}

func formatCodeCycle(index int, cycle []codegraph.NodeID) string {
	labels := make([]string, 0, len(cycle))
	for _, node := range cycle {
		labels = append(labels, string(node))
	}

	return "cycle=" + strconv.Itoa(index) + "	nodes=" + strings.Join(labels, " -> ")
}

func listCodeLayers(root string) error {
	graph, err := importGraph(root)
	if err != nil {
		return fmt.Errorf("code layers: %w", err)
	}

	layers, err := graph.TopologicalLayers()
	if err != nil {
		return fmt.Errorf("code layers: %w", err)
	}

	if len(layers) == 0 {
		fmt.Println("No code graph layers found.")
		return nil
	}

	for i := range layers {
		fmt.Println(formatCodeLayer(i+1, layers[i]))
	}

	return nil
}

func formatCodeLayer(index int, nodes []codegraph.NodeID) string {
	labels := make([]string, 0, len(nodes))
	for _, node := range nodes {
		labels = append(labels, string(node))
	}

	return "layer=" + strconv.Itoa(index) + "	nodes=" + strings.Join(labels, ",")
}

func listCodeDeps(root, target string) error {
	graph, err := importGraph(root)
	if err != nil {
		return fmt.Errorf("code deps: %w", err)
	}

	deps := codeGraphDependencies(graph, root, target)
	if len(deps) == 0 {
		fmt.Println("No direct code dependencies found.")
		return nil
	}

	for _, node := range deps {
		fmt.Println("node=" + string(node))
	}

	return nil
}

func listCodeReverseDeps(root, target string) error {
	graph, err := importGraph(root)
	if err != nil {
		return fmt.Errorf("code rdeps: %w", err)
	}

	rdeps := codeGraphReverseDependencies(graph, root, target)
	if len(rdeps) == 0 {
		fmt.Println("No direct code reverse dependencies found.")
		return nil
	}

	for _, node := range rdeps {
		fmt.Println("node=" + string(node))
	}

	return nil
}

func codeGraphDependencies(graph *codegraph.Graph, root, target string) []codegraph.NodeID {
	return graph.Neighbors(normalizeCodeGraphTarget(root, target))
}

func codeGraphReverseDependencies(graph *codegraph.Graph, root, target string) []codegraph.NodeID {
	return graph.ReverseDependencies(normalizeCodeGraphTarget(root, target))
}

func listCodeReachable(root, target string) error {
	graph, err := importGraph(root)
	if err != nil {
		return fmt.Errorf("code reachable: %w", err)
	}

	reachable := graph.ReachableFrom(normalizeCodeGraphTarget(root, target))
	if len(reachable) == 0 {
		fmt.Println("No reachable code graph nodes found.")
		return nil
	}

	for _, node := range reachable {
		fmt.Println("node=" + string(node))
	}

	return nil
}

func importGraph(root string) (*codegraph.Graph, error) {
	idx, err := codeintel.IndexDir(root)
	if err != nil {
		return nil, fmt.Errorf("index %s: %w", root, err)
	}

	return importGraphFromIndex(root, idx), nil
}

func importGraphFromIndex(root string, idx codeintel.Index) *codegraph.Graph {
	graph := codegraph.New()
	for i := range idx.Files {
		graph.AddNode(codegraph.NodeID(relativeCodePath(root, idx.Files[i].Path)))
	}

	for i := range idx.ImportEdges {
		edge := idx.ImportEdges[i]
		from := codegraph.NodeID(relativeCodePath(root, edge.From))
		graph.AddEdge(from, codegraph.NodeID(edge.Import))
	}

	return graph
}

func normalizeCodeGraphTarget(root, target string) codegraph.NodeID {
	target = strings.TrimSpace(target)
	if target == "" {
		return ""
	}

	if filepath.IsAbs(target) {
		return codegraph.NodeID(relativeCodePath(root, target))
	}

	return codegraph.NodeID(filepath.ToSlash(target))
}

func relativeCodePath(root, path string) string {
	if relativePath, err := filepath.Rel(root, path); err == nil {
		return filepath.ToSlash(relativePath)
	}

	return filepath.ToSlash(path)
}

func formatCodeImportEdge(root string, edge codeintel.ImportEdge) string {
	path := relativeCodePath(root, edge.From)
	return "path=" + path + "\timport=" + edge.Import
}
