package main

import (
	"strings"

	"github.com/tommoulard/atteler/pkg/codeintel"
)

func filterCodeImportEdges(edges []codeintel.ImportEdge, keep func(codeintel.ImportEdge) bool) []codeintel.ImportEdge {
	filtered := filterCodeIntelSlice(edges, keep)
	sortCodeImportEdges(filtered)

	return filtered
}

func summarizeCodeImportPackagesFromEdges(idx codeintel.Index, edges []codeintel.ImportEdge, includeImportCount bool) []codePackageImportMatchSummary {
	if len(edges) == 0 {
		return nil
	}

	packagesByFile := codeFilePackagesByPath(idx)
	filesByPackage := make(map[string]map[string]struct{})
	importsByPackage := make(map[string]map[string]struct{})

	for i := range edges {
		edge := edges[i]

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
		summary := codePackageImportMatchSummary{
			Name:  packageName,
			Files: len(files),
		}
		if includeImportCount {
			summary.Imports = len(importsByPackage[packageName])
		}

		summaries = append(summaries, summary)
	}

	sortCodePackageImportMatchSummaries(summaries)

	return summaries
}

type codeImportFileSummary struct {
	Path    string
	Package string
	Imports int
}

func sortCodeImportFileSummaries(summaries []codeImportFileSummary) {
	sortCodeIntelByCountDescNameAsc(summaries,
		func(summary codeImportFileSummary) int { return summary.Imports },
		func(summary codeImportFileSummary) string { return summary.Path },
	)
}

func sortCodeImportFileSummariesByPath(summaries []codeImportFileSummary) {
	sortCodeIntelByNameAsc(summaries, func(summary codeImportFileSummary) string { return summary.Path })
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

	sortCodeImportFileSummaries(summaries)

	return summaries
}

type codeImportSummary struct {
	Path  string
	Files int
}

func sortCodeImportSummaries(summaries []codeImportSummary) {
	sortCodeIntelByCountDescNameAsc(summaries,
		func(summary codeImportSummary) int { return summary.Files },
		func(summary codeImportSummary) string { return summary.Path },
	)
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

	sortCodeImportSummaries(summaries)

	return summaries
}

func summarizeCodeImportPrefix(idx codeintel.Index, prefix string) []codeImportSummary {
	edges := codeImportEdgesWithPrefix(idx, prefix)
	if len(edges) == 0 {
		return nil
	}

	return summarizeCodeImports(codeintel.Index{ImportEdges: edges})
}

func summarizeCodeImportPrefixFiles(root string, idx codeintel.Index, prefix string) []codeImportFileSummary {
	return edgesToCodeImportFileSummaries(root, idx, codeImportEdgesWithPrefix(idx, prefix))
}

func summarizeCodeImportPrefixPackages(idx codeintel.Index, prefix string) []codePackageImportMatchSummary {
	return summarizeCodeImportPackagesFromEdges(idx, codeImportEdgesWithPrefix(idx, prefix), true)
}

func codeImportEdgesWithPrefix(idx codeintel.Index, prefix string) []codeintel.ImportEdge {
	prefix = strings.TrimSpace(prefix)
	if prefix == "" {
		return nil
	}

	return filterCodeImportEdges(idx.ImportEdges, func(edge codeintel.ImportEdge) bool {
		return strings.HasPrefix(edge.Import, prefix)
	})
}

func codeImportEdgesForPath(idx codeintel.Index, importPath string) []codeintel.ImportEdge {
	importPath = strings.TrimSpace(importPath)
	if importPath == "" {
		return nil
	}

	return filterCodeImportEdges(idx.ImportEdges, func(edge codeintel.ImportEdge) bool {
		return edge.Import == importPath
	})
}

func summarizeCodeImportPath(idx codeintel.Index, importPath string) []codeImportSummary {
	edges := codeImportEdgesForPath(idx, importPath)
	if len(edges) == 0 {
		return nil
	}

	return summarizeCodeImports(codeintel.Index{ImportEdges: edges})
}

func summarizeCodeImportPathFiles(root string, idx codeintel.Index, importPath string) []codeImportFileSummary {
	summaries := edgesToCodeImportFileSummaries(root, idx, codeImportEdgesForPath(idx, importPath))
	sortCodeImportFileSummariesByPath(summaries)

	return summaries
}

func summarizeCodeImportPathPackages(idx codeintel.Index, importPath string) []codePackageImportMatchSummary {
	return summarizeCodeImportPackagesFromEdges(idx, codeImportEdgesForPath(idx, importPath), false)
}

func sortCodeImportEdges(edges []codeintel.ImportEdge) {
	sortCodeIntelByNamesAsc(edges,
		func(edge codeintel.ImportEdge) string { return edge.From },
		func(edge codeintel.ImportEdge) string { return edge.Import },
	)
}
