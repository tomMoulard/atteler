// Package codegraph provides dependency-free directed graph primitives for code relationships.
package codegraph

import (
	"slices"
	"sort"
	"strings"
)

// NodeID identifies a node in a graph.
type NodeID string

// Edge describes one directed relationship from one node to another.
type Edge struct {
	From NodeID
	To   NodeID
}

// Graph is a directed graph with deterministic query results.
type Graph struct {
	out map[NodeID]map[NodeID]struct{}
	in  map[NodeID]map[NodeID]struct{}
}

// CycleError reports that a topological operation could not complete because
// at least one directed cycle exists.
type CycleError struct {
	Cycles [][]NodeID
}

// Error returns a stable human-readable cycle error.
func (err CycleError) Error() string {
	if len(err.Cycles) == 0 {
		return "codegraph: graph contains a cycle"
	}

	parts := make([]string, 0, len(err.Cycles))
	for _, cycle := range err.Cycles {
		labels := make([]string, 0, len(cycle))
		for _, node := range cycle {
			labels = append(labels, string(node))
		}
		parts = append(parts, strings.Join(labels, " -> "))
	}
	return "codegraph: graph contains cycle(s): " + strings.Join(parts, "; ")
}

// New returns an empty directed graph.
func New() *Graph {
	return &Graph{
		out: make(map[NodeID]map[NodeID]struct{}),
		in:  make(map[NodeID]map[NodeID]struct{}),
	}
}

// AddNode adds id to the graph. It is safe to call repeatedly.
func (g *Graph) AddNode(id NodeID) {
	g.ensure()
	g.addNode(id)
}

// AddEdge adds a directed edge from -> to. Missing endpoint nodes are added.
func (g *Graph) AddEdge(from, to NodeID) {
	g.ensure()
	g.addNode(from)
	g.addNode(to)
	g.out[from][to] = struct{}{}
	g.in[to][from] = struct{}{}
}

// HasNode reports whether id exists in the graph.
func (g *Graph) HasNode(id NodeID) bool {
	if g == nil {
		return false
	}
	_, ok := g.out[id]
	return ok
}

// HasEdge reports whether from -> to exists in the graph.
func (g *Graph) HasEdge(from, to NodeID) bool {
	if g == nil {
		return false
	}
	neighbors, ok := g.out[from]
	if !ok {
		return false
	}
	_, ok = neighbors[to]
	return ok
}

// Nodes returns all graph nodes sorted by ID.
func (g *Graph) Nodes() []NodeID {
	if g == nil {
		return nil
	}
	return sortedKeys(g.out)
}

// Edges returns all graph edges sorted by from node and then to node.
func (g *Graph) Edges() []Edge {
	if g == nil {
		return nil
	}

	var edges []Edge
	for from, neighbors := range g.out {
		for to := range neighbors {
			edges = append(edges, Edge{From: from, To: to})
		}
	}
	sort.Slice(edges, func(i, j int) bool {
		if edges[i].From != edges[j].From {
			return edges[i].From < edges[j].From
		}
		return edges[i].To < edges[j].To
	})
	return edges
}

// Neighbors returns direct outgoing neighbors of id sorted by ID.
func (g *Graph) Neighbors(id NodeID) []NodeID {
	if g == nil {
		return nil
	}
	return sortedKeys(g.out[id])
}

// ReverseDependencies returns direct incoming neighbors of id sorted by ID.
func (g *Graph) ReverseDependencies(id NodeID) []NodeID {
	if g == nil {
		return nil
	}
	return sortedKeys(g.in[id])
}

// ReachableFrom returns nodes reachable from roots by following outgoing edges.
// Root nodes are not included unless they are reachable again through a cycle.
func (g *Graph) ReachableFrom(roots ...NodeID) []NodeID {
	if g == nil {
		return nil
	}
	return g.walk(g.out, roots...)
}

// ImpactSet returns nodes that can reach changed by following reverse edges.
// Changed nodes are not included unless they are impacted again through a cycle.
func (g *Graph) ImpactSet(changed ...NodeID) []NodeID {
	if g == nil {
		return nil
	}
	return g.walk(g.in, changed...)
}

// Cycles returns one directed cycle for each cyclic component in deterministic order.
func (g *Graph) Cycles() [][]NodeID {
	if g == nil {
		return nil
	}

	var cycles [][]NodeID
	for _, component := range g.stronglyConnectedComponents() {
		if len(component) == 1 && !g.HasEdge(component[0], component[0]) {
			continue
		}
		cycles = append(cycles, g.canonicalCycle(component))
	}

	sortCycles(cycles)
	return cycles
}

// HasCycle reports whether the graph contains at least one directed cycle.
func (g *Graph) HasCycle() bool {
	return len(g.Cycles()) > 0
}

// TopologicalLayers returns deterministic layers where every edge points from
// an earlier layer to the same or a later layer. Cyclic graphs return CycleError.
func (g *Graph) TopologicalLayers() ([][]NodeID, error) {
	if g == nil {
		return nil, nil
	}

	indegree := make(map[NodeID]int, len(g.out))
	for _, node := range g.Nodes() {
		indegree[node] = len(g.in[node])
	}

	current := zeroIndegreeNodes(indegree)
	var layers [][]NodeID
	visited := 0

	for len(current) > 0 {
		layer := append([]NodeID(nil), current...)
		layers = append(layers, layer)
		visited += len(layer)

		for _, from := range layer {
			for _, to := range g.Neighbors(from) {
				indegree[to]--
			}
			delete(indegree, from)
		}
		current = zeroIndegreeNodes(indegree)
	}

	if visited != len(g.out) {
		return nil, CycleError{Cycles: g.Cycles()}
	}
	return layers, nil
}

func (g *Graph) ensure() {
	if g.out == nil {
		g.out = make(map[NodeID]map[NodeID]struct{})
	}
	if g.in == nil {
		g.in = make(map[NodeID]map[NodeID]struct{})
	}
}

func (g *Graph) addNode(id NodeID) {
	if _, ok := g.out[id]; !ok {
		g.out[id] = make(map[NodeID]struct{})
	}
	if _, ok := g.in[id]; !ok {
		g.in[id] = make(map[NodeID]struct{})
	}
}

func (g *Graph) walk(edges map[NodeID]map[NodeID]struct{}, roots ...NodeID) []NodeID {
	if g == nil {
		return nil
	}

	frontier := make([]NodeID, 0, len(roots))
	for _, root := range roots {
		if g.HasNode(root) {
			frontier = append(frontier, root)
		}
	}
	slices.Sort(frontier)

	seen := make(map[NodeID]struct{})
	for len(frontier) > 0 {
		node := frontier[0]
		frontier = frontier[1:]
		for _, next := range sortedKeys(edges[node]) {
			if _, ok := seen[next]; ok {
				continue
			}
			seen[next] = struct{}{}
			frontier = append(frontier, next)
		}
	}
	return sortedKeys(seen)
}

func (g *Graph) stronglyConnectedComponents() [][]NodeID {
	var (
		index      int
		stack      []NodeID
		onStack    = make(map[NodeID]bool)
		indexes    = make(map[NodeID]int)
		lowLinks   = make(map[NodeID]int)
		components [][]NodeID
	)

	var visit func(NodeID)
	visit = func(node NodeID) {
		indexes[node] = index
		lowLinks[node] = index
		index++
		stack = append(stack, node)
		onStack[node] = true

		for _, next := range g.Neighbors(node) {
			if _, ok := indexes[next]; !ok {
				visit(next)
				lowLinks[node] = min(lowLinks[node], lowLinks[next])
			} else if onStack[next] {
				lowLinks[node] = min(lowLinks[node], indexes[next])
			}
		}

		if lowLinks[node] != indexes[node] {
			return
		}

		var component []NodeID
		for {
			last := stack[len(stack)-1]
			stack = stack[:len(stack)-1]
			onStack[last] = false
			component = append(component, last)
			if last == node {
				break
			}
		}
		slices.Sort(component)
		components = append(components, component)
	}

	for _, node := range g.Nodes() {
		if _, ok := indexes[node]; !ok {
			visit(node)
		}
	}
	return components
}

func (g *Graph) canonicalCycle(component []NodeID) []NodeID {
	nodes := append([]NodeID(nil), component...)
	slices.Sort(nodes)

	if len(nodes) == 1 {
		return []NodeID{nodes[0], nodes[0]}
	}

	inComponent := make(map[NodeID]struct{}, len(nodes))
	for _, node := range nodes {
		inComponent[node] = struct{}{}
	}

	start := nodes[0]
	path := []NodeID{start}
	onPath := map[NodeID]struct{}{start: {}}
	if cycle, ok := g.findCyclePath(start, start, inComponent, path, onPath); ok {
		return cycle
	}

	// Tarjan only passes cyclic components here, so this is defensive.
	return append(nodes, nodes[0])
}

func (g *Graph) findCyclePath(start, current NodeID, inComponent map[NodeID]struct{}, path []NodeID, onPath map[NodeID]struct{}) ([]NodeID, bool) {
	for _, next := range g.Neighbors(current) {
		if _, ok := inComponent[next]; !ok {
			continue
		}
		if next == start && len(path) > 1 {
			return append(append([]NodeID(nil), path...), start), true
		}
		if _, ok := onPath[next]; ok {
			continue
		}

		onPath[next] = struct{}{}
		nextPath := append(append([]NodeID(nil), path...), next)
		if cycle, ok := g.findCyclePath(start, next, inComponent, nextPath, onPath); ok {
			return cycle, true
		}
		delete(onPath, next)
	}
	return nil, false
}

func sortedKeys[V any](m map[NodeID]V) []NodeID {
	if len(m) == 0 {
		return nil
	}
	keys := make([]NodeID, 0, len(m))
	for key := range m {
		keys = append(keys, key)
	}
	slices.Sort(keys)
	return keys
}

func zeroIndegreeNodes(indegree map[NodeID]int) []NodeID {
	var nodes []NodeID
	for node, degree := range indegree {
		if degree == 0 {
			nodes = append(nodes, node)
		}
	}
	slices.Sort(nodes)
	return nodes
}

func sortCycles(cycles [][]NodeID) {
	sort.Slice(cycles, func(i, j int) bool {
		left := cycleKey(cycles[i])
		right := cycleKey(cycles[j])
		return left < right
	})
}

func cycleKey(cycle []NodeID) string {
	parts := make([]string, 0, len(cycle))
	for _, node := range cycle {
		parts = append(parts, string(node))
	}
	return strings.Join(parts, "\x00")
}
