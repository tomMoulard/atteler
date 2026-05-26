package main

import (
	"path/filepath"
	"strings"

	"github.com/tommoulard/atteler/pkg/codegraph"
	"github.com/tommoulard/atteler/pkg/codeintel"
)

func countPackages(files []codeintel.File) int {
	seen := make(map[string]struct{})

	for i := range files {
		if files[i].Package != "" {
			seen[files[i].Package] = struct{}{}
		}
	}

	return len(seen)
}

func codeGraphDependencies(graph *codegraph.Graph, root, target string) []codegraph.NodeID {
	return graph.Neighbors(normalizeCodeGraphTarget(root, target))
}

func codeGraphReverseDependencies(graph *codegraph.Graph, root, target string) []codegraph.NodeID {
	return graph.ReverseDependencies(normalizeCodeGraphTarget(root, target))
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
