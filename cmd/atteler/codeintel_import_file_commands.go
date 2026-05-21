package main

import (
	"fmt"
	"sort"
	"strconv"
	"strings"

	"github.com/tommoulard/atteler/pkg/codeintel"
)

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
