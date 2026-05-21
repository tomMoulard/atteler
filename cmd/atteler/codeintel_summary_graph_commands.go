package main

import (
	"fmt"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/tommoulard/atteler/pkg/codegraph"
	"github.com/tommoulard/atteler/pkg/codeintel"
)

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
