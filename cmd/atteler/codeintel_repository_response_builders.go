//nolint:wsl_v5 // Category builders keep related schema-field assignments compact and auditable.
package main

import (
	"fmt"

	"github.com/tommoulard/atteler/pkg/codeintel"
)

func buildCodeIntelRepositoryResponse(root string, idx codeintel.Index, input codeIntelCommandInput, response codeIntelResponse, commandName string) (codeIntelResponse, bool, error) {
	switch commandName {
	case "code-summary":
		graph := importGraphFromIndex(root, idx)
		layers, layerErr := graph.TopologicalLayers()
		summary := codeIntelSummary{
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
		response.Summary = &summary
	case "list-code-files":
		response.Files = codeIntelFilesFromPackageFiles(summarizeCodeFiles(root, idx))
	case "list-code-packages":
		response.Packages = codeIntelPackagesFromSummaries(summarizeCodePackages(idx))
	case "code-package-name":
		response.Query = codeIntelQuery("package", input.PackageName)
		response.Files = codeIntelFilesFromPackageFiles(summarizeCodePackageFiles(root, idx, input.PackageName))
	case "code-file-path":
		response.Query = codeIntelQuery("path", input.FilePath)
		if file, ok := findCodeFile(root, idx, input.FilePath); ok {
			response.Files = []codeIntelFile{codeIntelFileFromDetail(root, file)}
		}
	default:
		return response, false, nil
	}

	return response, true, nil
}

func buildCodeIntelGraphResponse(root string, idx codeintel.Index, input codeIntelCommandInput, response codeIntelResponse, commandName string) (codeIntelResponse, bool, error) {
	switch commandName {
	case "code-impact-target":
		response.Query = codeIntelQuery("target", input.ImpactTarget)
		response.ImpactSet = codeIntelNodesFromGraph(importGraphFromIndex(root, idx).ImpactSet(normalizeCodeGraphTarget(root, input.ImpactTarget)))
	case "code-reach-target":
		response.Query = codeIntelQuery("target", input.ReachTarget)
		response.Nodes = codeIntelNodesFromGraph(importGraphFromIndex(root, idx).ReachableFrom(normalizeCodeGraphTarget(root, input.ReachTarget)))
	case "code-deps-target":
		response.Query = codeIntelQuery("target", input.DepsTarget)
		response.Nodes = codeIntelNodesFromGraph(codeGraphDependencies(importGraphFromIndex(root, idx), root, input.DepsTarget))
	case "code-rdeps-target":
		response.Query = codeIntelQuery("target", input.RDepsTarget)
		response.Nodes = codeIntelNodesFromGraph(codeGraphReverseDependencies(importGraphFromIndex(root, idx), root, input.RDepsTarget))
	case codeIntelCyclesCommandName:
		response.Cycles = codeIntelCyclesFromGraph(importGraphFromIndex(root, idx).Cycles())
	case "list-code-layers":
		layers, err := importGraphFromIndex(root, idx).TopologicalLayers()
		if err != nil {
			return response, true, fmt.Errorf("code layers: %w", err)
		}
		response.Layers = codeIntelLayersFromGraph(layers)
	default:
		return response, false, nil
	}

	return response, true, nil
}
