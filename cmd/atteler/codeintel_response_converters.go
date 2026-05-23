package main

import (
	"github.com/tommoulard/atteler/pkg/codegraph"
	"github.com/tommoulard/atteler/pkg/codeintel"
	"github.com/tommoulard/atteler/pkg/lsp"
)

func codeIntelFileFromDetail(root string, file codeintel.File) codeIntelFile {
	out := codeIntelFile{
		Path:        relativeCodePath(root, file.Path),
		Package:     file.Package,
		ImportCount: new(len(file.Imports)),
		SymbolCount: new(len(file.Symbols)),
		Imports:     append([]string(nil), file.Imports...),
		Symbols:     codeIntelSymbolsFromSymbols(root, file.Symbols),
	}

	return out
}

func codeIntelFilesFromPackageFiles(files []codePackageFile) []codeIntelFile {
	out := make([]codeIntelFile, 0, len(files))
	for i := range files {
		out = append(out, codeIntelFile{
			Path:        files[i].Path,
			Package:     files[i].Package,
			ImportCount: new(files[i].Imports),
			SymbolCount: new(files[i].Symbols),
		})
	}

	return out
}

func codeIntelFilesFromSymbolFileSummaries(files []codeSymbolFileSummary) []codeIntelFile {
	out := make([]codeIntelFile, 0, len(files))
	for i := range files {
		out = append(out, codeIntelFile{
			Path:        files[i].Path,
			Package:     files[i].Package,
			SymbolCount: new(files[i].Symbols),
		})
	}

	return out
}

func codeIntelFilesFromImportFileSummaries(files []codeImportFileSummary) []codeIntelFile {
	out := make([]codeIntelFile, 0, len(files))
	for i := range files {
		out = append(out, codeIntelFile{
			Path:        files[i].Path,
			Package:     files[i].Package,
			ImportCount: new(files[i].Imports),
		})
	}

	return out
}

func codeIntelPackagesFromSummaries(packages []codePackageSummary) []codeIntelPackage {
	out := make([]codeIntelPackage, 0, len(packages))
	for i := range packages {
		out = append(out, codeIntelPackage{
			Name:    packages[i].Name,
			Files:   new(packages[i].Files),
			Symbols: new(packages[i].Symbols),
		})
	}

	return out
}

func codeIntelPackagesFromImportSummaries(packages []codePackageImportSummary) []codeIntelPackage {
	out := make([]codeIntelPackage, 0, len(packages))
	for i := range packages {
		out = append(out, codeIntelPackage{
			Name:          packages[i].Name,
			Files:         new(packages[i].Files),
			Imports:       new(packages[i].Imports),
			UniqueImports: new(packages[i].UniqueImports),
		})
	}

	return out
}

func codeIntelPackagesFromImportMatches(packages []codePackageImportMatchSummary) []codeIntelPackage {
	out := make([]codeIntelPackage, 0, len(packages))
	for i := range packages {
		pkg := codeIntelPackage{
			Name:  packages[i].Name,
			Files: new(packages[i].Files),
		}
		if packages[i].Imports > 0 {
			pkg.Imports = new(packages[i].Imports)
		}

		out = append(out, pkg)
	}

	return out
}

func codeIntelSymbolsFromSymbols(root string, symbols []codeintel.Symbol) []codeIntelSymbol {
	out := make([]codeIntelSymbol, 0, len(symbols))
	for i := range symbols {
		out = append(out, codeIntelSymbol{
			Name: symbols[i].Name,
			Kind: symbols[i].Kind,
			Path: relativeCodePath(root, symbols[i].File),
			Line: symbols[i].Line,
		})
	}

	return out
}

func codeIntelSymbolsFromSummaries(symbols []codeSymbolSummary) []codeIntelSymbol {
	out := make([]codeIntelSymbol, 0, len(symbols))
	for i := range symbols {
		out = append(out, codeIntelSymbol{Kind: symbols[i].Kind, Count: symbols[i].Count})
	}

	return out
}

func codeIntelImportsFromSummaries(imports []codeImportSummary) []codeIntelImport {
	out := make([]codeIntelImport, 0, len(imports))
	for i := range imports {
		out = append(out, codeIntelImport{Path: imports[i].Path, Files: imports[i].Files})
	}

	return out
}

func codeIntelImportsFromPaths(imports []string) []codeIntelImport {
	out := make([]codeIntelImport, 0, len(imports))
	for _, imp := range imports {
		out = append(out, codeIntelImport{Path: imp})
	}

	return out
}

func codeIntelEdgesFromImportEdges(root string, edges []codeintel.ImportEdge) []codeIntelEdge {
	out := make([]codeIntelEdge, 0, len(edges))
	for i := range edges {
		out = append(out, codeIntelEdge{Path: relativeCodePath(root, edges[i].From), Import: edges[i].Import})
	}

	return out
}

func codeIntelEdgesFromFiles(files []string, importPath string) []codeIntelEdge {
	out := make([]codeIntelEdge, 0, len(files))
	for _, file := range files {
		out = append(out, codeIntelEdge{Path: file, Import: importPath})
	}

	return out
}

func codeIntelNodesFromGraph(nodes []codegraph.NodeID) []codeIntelNode {
	out := make([]codeIntelNode, 0, len(nodes))
	for _, node := range nodes {
		out = append(out, codeIntelNode{Path: string(node)})
	}

	return out
}

func codeIntelCyclesFromGraph(cycles [][]codegraph.NodeID) []codeIntelCycle {
	out := make([]codeIntelCycle, 0, len(cycles))
	for i := range cycles {
		out = append(out, codeIntelCycle{Index: i + 1, Nodes: codeIntelNodeLabels(cycles[i])})
	}

	return out
}

func codeIntelLayersFromGraph(layers [][]codegraph.NodeID) []codeIntelLayer {
	out := make([]codeIntelLayer, 0, len(layers))
	for i := range layers {
		out = append(out, codeIntelLayer{Index: i + 1, Nodes: codeIntelNodeLabels(layers[i])})
	}

	return out
}

func codeIntelLSPSymbolsFromLSP(symbols []lsp.Symbol) []codeIntelLSPSymbol {
	out := make([]codeIntelLSPSymbol, 0, len(symbols))
	for i := range symbols {
		out = append(out, codeIntelLSPSymbolFromLSP(symbols[i]))
	}

	return out
}

func codeIntelLSPSymbolFromLSP(symbol lsp.Symbol) codeIntelLSPSymbol {
	return codeIntelLSPSymbol{
		Name:           symbol.Name,
		Kind:           symbol.Kind,
		Detail:         symbol.Detail,
		Container:      symbol.ContainerName,
		URI:            symbol.URI,
		Range:          codeIntelLSPRangeFromLSP(symbol.Range),
		SelectionRange: codeIntelLSPRangeFromLSP(symbol.SelectionRange),
		Children:       codeIntelLSPSymbolsFromLSP(symbol.Children),
	}
}

func codeIntelLSPRangeFromLSP(lspRange lsp.Range) codeIntelLSPRange {
	return codeIntelLSPRange{
		Start: codeIntelLSPPosition{
			Line:      lspRange.Start.Line,
			Character: lspRange.Start.Character,
		},
		End: codeIntelLSPPosition{
			Line:      lspRange.End.Line,
			Character: lspRange.End.Character,
		},
	}
}

func codeIntelNodeLabels(nodes []codegraph.NodeID) []string {
	labels := make([]string, 0, len(nodes))
	for _, node := range nodes {
		labels = append(labels, string(node))
	}

	return labels
}
