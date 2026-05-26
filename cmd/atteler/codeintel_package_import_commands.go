package main

import (
	"strings"

	"github.com/tommoulard/atteler/pkg/codeintel"
)

func codePackageImportEdges(idx codeintel.Index, packageName string, keep func(codeintel.ImportEdge) bool) []codeintel.ImportEdge {
	packageName = strings.TrimSpace(packageName)
	if packageName == "" {
		return nil
	}

	packageFiles := codePackageFileSet(idx, packageName)
	if len(packageFiles) == 0 {
		return nil
	}

	seen := make(map[string]struct{})
	edges := make([]codeintel.ImportEdge, 0)

	for i := range idx.ImportEdges {
		edge := idx.ImportEdges[i]
		if _, ok := packageFiles[edge.From]; !ok {
			continue
		}

		if !keep(edge) {
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

	sortCodeImportFileSummaries(summaries)

	return summaries
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

	packagesByFile := codeFilePackagesByPath(idx)
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

	sortCodeImportFileSummaries(summaries)

	return summaries
}

func codePackageImportPrefixFiles(idx codeintel.Index, packageName, prefix string) []codeintel.ImportEdge {
	prefix = strings.TrimSpace(prefix)
	if prefix == "" {
		return nil
	}

	return codePackageImportEdges(idx, packageName, func(edge codeintel.ImportEdge) bool {
		return strings.HasPrefix(edge.Import, prefix)
	})
}

func codePackageImportFiles(root string, idx codeintel.Index, packageName, importPath string) []string {
	importPath = strings.TrimSpace(importPath)
	if importPath == "" {
		return nil
	}

	edges := codePackageImportEdges(idx, packageName, func(edge codeintel.ImportEdge) bool {
		return edge.Import == importPath
	})
	if len(edges) == 0 {
		return nil
	}

	seen := make(map[string]struct{})
	files := make([]string, 0, len(edges))

	for i := range edges {
		rel := relativeCodePath(root, edges[i].From)
		if _, ok := seen[rel]; ok {
			continue
		}

		seen[rel] = struct{}{}
		files = append(files, rel)
	}

	sortCodeIntelStringsAsc(files)

	return files
}

func summarizeCodePackageImportPathFiles(root string, idx codeintel.Index, packageName, importPath string) []codeImportFileSummary {
	importPath = strings.TrimSpace(importPath)
	if importPath == "" {
		return nil
	}

	summaries := edgesToCodeImportFileSummaries(root, idx, codePackageImportEdges(idx, packageName, func(edge codeintel.ImportEdge) bool {
		return edge.Import == importPath
	}))
	sortCodeImportFileSummariesByPath(summaries)

	return summaries
}

func summarizeCodePackageImportPath(idx codeintel.Index, packageName, importPath string) []codeImportSummary {
	packageName = strings.TrimSpace(packageName)

	importPath = strings.TrimSpace(importPath)
	if packageName == "" || importPath == "" {
		return nil
	}

	all := summarizeCodePackageImports(idx, packageName)

	return filterCodeIntelSlice(all, func(summary codeImportSummary) bool {
		return summary.Path == importPath
	})
}

func summarizeCodePackageImportPrefix(idx codeintel.Index, packageName, prefix string) []codeImportSummary {
	packageName = strings.TrimSpace(packageName)

	prefix = strings.TrimSpace(prefix)
	if packageName == "" || prefix == "" {
		return nil
	}

	all := summarizeCodePackageImports(idx, packageName)

	return filterCodeIntelSlice(all, func(summary codeImportSummary) bool {
		return strings.HasPrefix(summary.Path, prefix)
	})
}

func summarizeCodePackageImports(idx codeintel.Index, packageName string) []codeImportSummary {
	edges := codePackageImportEdges(idx, packageName, func(_ codeintel.ImportEdge) bool {
		return true
	})
	if len(edges) == 0 {
		return nil
	}

	return summarizeCodeImports(codeintel.Index{ImportEdges: edges})
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

func sortCodePackageImportMatchSummaries(summaries []codePackageImportMatchSummary) {
	sortCodeIntelByCountsDescNameAsc(summaries,
		func(summary codePackageImportMatchSummary) int { return summary.Imports },
		func(summary codePackageImportMatchSummary) int { return summary.Files },
		func(summary codePackageImportMatchSummary) string { return summary.Name },
	)
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

	sortCodeIntelByCountDescNameAsc(summaries,
		func(summary codePackageImportSummary) int { return summary.Imports },
		func(summary codePackageImportSummary) string { return summary.Name },
	)

	return summaries
}
