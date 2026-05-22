//nolint:govet,wsl_v5 // Public metadata structs prioritize stable/readable field order; wsl is noisy for evidence-building code.
package codegraph

import (
	"slices"
	"sort"
	"strings"
)

// Node describes a typed graph node. Kind is intentionally a string so callers
// can define domain-specific node taxonomies without coupling codegraph to a
// language model.
type Node struct {
	ID   NodeID
	Kind string
	Name string
}

// Provenance records why a graph relationship exists. It is deliberately small
// and serializable so review tools can cite evidence rather than trusting naked
// node IDs.
type Provenance struct {
	Source       string
	File         string
	StartLine    int
	StartColumn  int
	EndLine      int
	EndColumn    int
	BuildContext string
	Confidence   string
}

// Relationship describes one typed edge with provenance.
type Relationship struct {
	From       NodeID
	To         NodeID
	Kind       string
	Provenance []Provenance
}

// QueryResult returns graph nodes with the evidence and uncertainty collected
// while answering a query.
type QueryResult struct {
	Node        Node
	Evidence    []Relationship
	Uncertainty []string
}

// EvidenceGraph wraps Graph with typed nodes and provenance-bearing edges while
// preserving deterministic traversal semantics.
type EvidenceGraph struct {
	graph *Graph
	nodes map[NodeID]Node
	edges map[relationshipKey]Relationship
}

type relationshipKey struct {
	from NodeID
	to   NodeID
	kind string
}

// NewEvidence returns an empty provenance-aware graph.
func NewEvidence() *EvidenceGraph {
	return &EvidenceGraph{
		graph: New(),
		nodes: make(map[NodeID]Node),
		edges: make(map[relationshipKey]Relationship),
	}
}

// AddNode adds or updates a typed node. Empty IDs are ignored.
func (g *EvidenceGraph) AddNode(node Node) {
	if node.ID == "" {
		return
	}

	g.ensure()
	g.nodes[node.ID] = node
	g.graph.AddNode(node.ID)
}

// AddRelationship adds a typed relationship with optional provenance. Missing
// endpoint nodes are added with only their IDs populated.
func (g *EvidenceGraph) AddRelationship(edge Relationship) {
	if edge.From == "" || edge.To == "" || edge.Kind == "" {
		return
	}

	g.ensure()
	if _, ok := g.nodes[edge.From]; !ok {
		g.AddNode(Node{ID: edge.From})
	}
	if _, ok := g.nodes[edge.To]; !ok {
		g.AddNode(Node{ID: edge.To})
	}

	g.graph.AddEdge(edge.From, edge.To)

	key := relationshipKey{from: edge.From, to: edge.To, kind: edge.Kind}
	existing := g.edges[key]
	if existing.Kind == "" {
		existing = Relationship{From: edge.From, To: edge.To, Kind: edge.Kind}
	}
	existing.Provenance = append(existing.Provenance, edge.Provenance...)
	existing.Provenance = sortProvenance(existing.Provenance)
	g.edges[key] = existing
}

// Clone returns a deep copy of the evidence graph metadata and underlying graph.
func (g *EvidenceGraph) Clone() *EvidenceGraph {
	if g == nil {
		return nil
	}

	clone := NewEvidence()
	for _, node := range g.Nodes() {
		clone.AddNode(node)
	}
	for _, edge := range g.Relationships() {
		provenance := append([]Provenance(nil), edge.Provenance...)
		clone.AddRelationship(Relationship{
			From:       edge.From,
			To:         edge.To,
			Kind:       edge.Kind,
			Provenance: provenance,
		})
	}

	return clone
}

// Graph returns the underlying deterministic directed graph. Callers must not
// mutate it if they need EvidenceGraph metadata to remain consistent.
func (g *EvidenceGraph) Graph() *Graph {
	if g == nil {
		return nil
	}

	return g.graph
}

// Nodes returns typed nodes sorted by ID.
func (g *EvidenceGraph) Nodes() []Node {
	if g == nil {
		return nil
	}

	ids := sortedKeys(g.nodes)
	nodes := make([]Node, 0, len(ids))
	for _, id := range ids {
		nodes = append(nodes, g.nodes[id])
	}

	return nodes
}

// Relationships returns typed relationships sorted by from, to, then kind.
func (g *EvidenceGraph) Relationships() []Relationship {
	if g == nil {
		return nil
	}

	edges := make([]Relationship, 0, len(g.edges))
	for _, edge := range g.edges {
		edges = append(edges, edge)
	}

	sortRelationships(edges)

	return edges
}

// RelationshipsBetween returns all typed relationships from -> to sorted by kind.
func (g *EvidenceGraph) RelationshipsBetween(from, to NodeID) []Relationship {
	if g == nil {
		return nil
	}

	var edges []Relationship
	for _, edge := range g.edges {
		if edge.From == from && edge.To == to {
			edges = append(edges, edge)
		}
	}

	sortRelationships(edges)

	return edges
}

// NeighborsWithEvidence returns outgoing neighbors and the relationships that
// prove each direct edge. Missing node metadata is reported as uncertainty.
func (g *EvidenceGraph) NeighborsWithEvidence(id NodeID) []QueryResult {
	if g == nil || g.graph == nil {
		return nil
	}

	neighbors := g.graph.Neighbors(id)
	results := make([]QueryResult, 0, len(neighbors))
	for _, neighbor := range neighbors {
		result := QueryResult{
			Node:     g.nodeOrUnknown(neighbor),
			Evidence: g.RelationshipsBetween(id, neighbor),
		}
		if result.Node.Kind == "" {
			result.Uncertainty = append(result.Uncertainty, "neighbor node has no typed metadata")
		}
		if len(result.Evidence) == 0 {
			result.Uncertainty = append(result.Uncertainty, "edge has no recorded provenance")
		}
		results = append(results, result)
	}

	return results
}

// ReachableFromWithEvidence follows outgoing edges and returns reachable nodes
// with the direct relationship evidence used to discover each node.
func (g *EvidenceGraph) ReachableFromWithEvidence(roots ...NodeID) []QueryResult {
	if g == nil || g.graph == nil {
		return nil
	}

	return g.walkWithEvidence(g.graph.out, false, roots...)
}

// ImpactSetWithEvidence follows reverse edges and returns impacted nodes with
// the direct relationship evidence used to discover each node.
func (g *EvidenceGraph) ImpactSetWithEvidence(changed ...NodeID) []QueryResult {
	if g == nil || g.graph == nil {
		return nil
	}

	return g.walkWithEvidence(g.graph.in, true, changed...)
}

// ReverseDependenciesWithEvidence returns incoming neighbors and the
// relationships that prove each direct reverse edge.
func (g *EvidenceGraph) ReverseDependenciesWithEvidence(id NodeID) []QueryResult {
	if g == nil || g.graph == nil {
		return nil
	}

	neighbors := g.graph.ReverseDependencies(id)
	results := make([]QueryResult, 0, len(neighbors))
	for _, neighbor := range neighbors {
		result := QueryResult{
			Node:     g.nodeOrUnknown(neighbor),
			Evidence: g.RelationshipsBetween(neighbor, id),
		}
		if result.Node.Kind == "" {
			result.Uncertainty = append(result.Uncertainty, "reverse dependency node has no typed metadata")
		}
		if len(result.Evidence) == 0 {
			result.Uncertainty = append(result.Uncertainty, "edge has no recorded provenance")
		}
		results = append(results, result)
	}

	return results
}

func (g *EvidenceGraph) walkWithEvidence(edges map[NodeID]map[NodeID]struct{}, reverse bool, roots ...NodeID) []QueryResult {
	frontier := make([]NodeID, 0, len(roots))
	for _, root := range roots {
		if g.graph.HasNode(root) {
			frontier = append(frontier, root)
		}
	}

	slices.Sort(frontier)

	evidenceByNode := make(map[NodeID][]Relationship)
	uncertaintyByNode := make(map[NodeID][]string)
	for len(frontier) > 0 {
		node := frontier[0]
		frontier = frontier[1:]

		for _, next := range sortedKeys(edges[node]) {
			if _, ok := evidenceByNode[next]; ok {
				continue
			}

			evidence := g.edgeEvidence(node, next, reverse)
			evidenceByNode[next] = evidence
			if len(evidence) == 0 {
				uncertaintyByNode[next] = append(uncertaintyByNode[next], "edge has no recorded provenance")
			}

			frontier = append(frontier, next)
		}
	}

	nodes := sortedKeys(evidenceByNode)
	results := make([]QueryResult, 0, len(nodes))
	for _, id := range nodes {
		result := QueryResult{
			Node:        g.nodeOrUnknown(id),
			Evidence:    evidenceByNode[id],
			Uncertainty: uncertaintyByNode[id],
		}
		if result.Node.Kind == "" {
			result.Uncertainty = append(result.Uncertainty, "node has no typed metadata")
		}

		results = append(results, result)
	}

	return results
}

func (g *EvidenceGraph) edgeEvidence(current, next NodeID, reverse bool) []Relationship {
	if reverse {
		return g.RelationshipsBetween(next, current)
	}

	return g.RelationshipsBetween(current, next)
}

func (g *EvidenceGraph) ensure() {
	if g.graph == nil {
		g.graph = New()
	}
	if g.nodes == nil {
		g.nodes = make(map[NodeID]Node)
	}
	if g.edges == nil {
		g.edges = make(map[relationshipKey]Relationship)
	}
}

func (g *EvidenceGraph) nodeOrUnknown(id NodeID) Node {
	if node, ok := g.nodes[id]; ok {
		return node
	}

	return Node{ID: id}
}

func sortRelationships(edges []Relationship) {
	sort.Slice(edges, func(i, j int) bool {
		if edges[i].From != edges[j].From {
			return edges[i].From < edges[j].From
		}
		if edges[i].To != edges[j].To {
			return edges[i].To < edges[j].To
		}

		return edges[i].Kind < edges[j].Kind
	})
}

func sortProvenance(provenance []Provenance) []Provenance {
	sort.Slice(provenance, func(i, j int) bool {
		left := provenanceKey(provenance[i])
		right := provenanceKey(provenance[j])

		return left < right
	})

	unique := provenance[:0]
	var last string
	for i, item := range provenance {
		key := provenanceKey(item)
		if i > 0 && key == last {
			continue
		}

		unique = append(unique, item)
		last = key
	}

	return unique
}

func provenanceKey(provenance Provenance) string {
	parts := []string{
		provenance.Source,
		provenance.File,
		provenance.BuildContext,
		provenance.Confidence,
	}
	parts = append(parts,
		intKey(provenance.StartLine),
		intKey(provenance.StartColumn),
		intKey(provenance.EndLine),
		intKey(provenance.EndColumn),
	)

	return strings.Join(parts, "\x00")
}

func intKey(value int) string {
	if value == 0 {
		return ""
	}

	// strconv would be fine here; avoiding another import keeps this wrapper tiny.
	const digits = "0123456789"
	var buf [20]byte
	pos := len(buf)
	for value > 0 {
		pos--
		buf[pos] = digits[value%10]
		value /= 10
	}

	return string(buf[pos:])
}
