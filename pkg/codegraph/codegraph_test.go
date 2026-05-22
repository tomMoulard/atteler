//nolint:wsl_v5 // Existing tests and query builders use compact assertion/evidence blocks.
package codegraph

import (
	"errors"
	"reflect"
	"strings"
	"testing"
)

func TestGraph_AddNodesEdgesAndDeterministicQueries(t *testing.T) {
	t.Parallel()

	var graph Graph
	graph.AddEdge("pkg/b", "pkg/c")
	graph.AddEdge("pkg/a", "pkg/c")
	graph.AddEdge("pkg/a", "pkg/b")
	graph.AddEdge("pkg/a", "pkg/b")
	graph.AddNode("pkg/d")

	assertNodes(t, graph.Nodes(), []NodeID{"pkg/a", "pkg/b", "pkg/c", "pkg/d"})
	assertEdges(t, graph.Edges(), []Edge{
		{From: "pkg/a", To: "pkg/b"},
		{From: "pkg/a", To: "pkg/c"},
		{From: "pkg/b", To: "pkg/c"},
	})
	assertNodes(t, graph.Neighbors("pkg/a"), []NodeID{"pkg/b", "pkg/c"})
	assertNodes(t, graph.ReverseDependencies("pkg/c"), []NodeID{"pkg/a", "pkg/b"})

	if !graph.HasNode("pkg/d") {
		t.Fatal("HasNode(pkg/d) = false, want true")
	}

	if !graph.HasEdge("pkg/a", "pkg/b") {
		t.Fatal("HasEdge(pkg/a, pkg/b) = false, want true")
	}

	if graph.HasEdge("pkg/c", "pkg/a") {
		t.Fatal("HasEdge(pkg/c, pkg/a) = true, want false")
	}
}

func TestGraph_ReachableFromFollowsOutgoingEdges(t *testing.T) {
	t.Parallel()

	graph := New()
	graph.AddEdge("root", "b")
	graph.AddEdge("root", "a")
	graph.AddEdge("a", "c")
	graph.AddEdge("c", "root")
	graph.AddEdge("z", "root")
	graph.AddNode("isolated")

	assertNodes(t, graph.ReachableFrom("root"), []NodeID{"a", "b", "c", "root"})
	assertNodes(t, graph.ReachableFrom("missing"), nil)
	assertNodes(t, graph.ReachableFrom("isolated"), nil)
}

func TestGraph_ImpactSetFollowsReverseEdges(t *testing.T) {
	t.Parallel()

	graph := New()
	graph.AddEdge("cli", "service")
	graph.AddEdge("service", "storage")
	graph.AddEdge("tests", "service")
	graph.AddEdge("docs", "cli")
	graph.AddNode("unrelated")

	assertNodes(t, graph.ImpactSet("storage"), []NodeID{"cli", "docs", "service", "tests"})
	assertNodes(t, graph.ImpactSet("service"), []NodeID{"cli", "docs", "tests"})
	assertNodes(t, graph.ImpactSet("unrelated"), nil)
}

func TestGraph_TopologicalLayersReturnsStableLayers(t *testing.T) {
	t.Parallel()

	graph := New()
	graph.AddEdge("parse", "index")
	graph.AddEdge("scan", "index")
	graph.AddEdge("index", "rank")
	graph.AddEdge("rank", "render")
	graph.AddNode("config")

	layers, err := graph.TopologicalLayers()
	if err != nil {
		t.Fatalf("TopologicalLayers() error = %v", err)
	}

	want := [][]NodeID{
		{"config", "parse", "scan"},
		{"index"},
		{"rank"},
		{"render"},
	}
	if !reflect.DeepEqual(layers, want) {
		t.Fatalf("TopologicalLayers() = %#v, want %#v", layers, want)
	}
}

func TestGraph_TopologicalLayersReturnsCycleError(t *testing.T) {
	t.Parallel()

	graph := New()
	graph.AddEdge("a", "b")
	graph.AddEdge("b", "c")
	graph.AddEdge("c", "a")
	graph.AddEdge("d", "d")

	layers, err := graph.TopologicalLayers()
	if err == nil {
		t.Fatal("TopologicalLayers() error = nil, want cycle error")
	}

	if layers != nil {
		t.Fatalf("TopologicalLayers() layers = %#v, want nil", layers)
	}

	var cycleErr CycleError
	if !errors.As(err, &cycleErr) {
		t.Fatalf("TopologicalLayers() error = %T, want CycleError", err)
	}

	assertCycles(t, cycleErr.Cycles, [][]NodeID{
		{"a", "b", "c", "a"},
		{"d", "d"},
	})

	if !strings.Contains(err.Error(), "a -> b -> c -> a") {
		t.Fatalf("CycleError.Error() = %q, want cycle details", err.Error())
	}
}

func TestGraph_CyclesDetectsNoneSimpleAndSelfCycles(t *testing.T) {
	t.Parallel()

	acyclic := New()
	acyclic.AddEdge("a", "b")
	acyclic.AddEdge("b", "c")

	if acyclic.HasCycle() {
		t.Fatal("acyclic.HasCycle() = true, want false")
	}

	assertCycles(t, acyclic.Cycles(), nil)

	cyclic := New()
	cyclic.AddEdge("x", "y")
	cyclic.AddEdge("y", "x")
	cyclic.AddEdge("self", "self")

	if !cyclic.HasCycle() {
		t.Fatal("cyclic.HasCycle() = false, want true")
	}

	assertCycles(t, cyclic.Cycles(), [][]NodeID{
		{"self", "self"},
		{"x", "y", "x"},
	})
}

func TestNilGraphQueriesAreSafe(t *testing.T) {
	t.Parallel()

	var graph *Graph
	assertNodes(t, graph.Nodes(), nil)
	assertEdges(t, graph.Edges(), nil)
	assertNodes(t, graph.Neighbors("x"), nil)
	assertNodes(t, graph.ReverseDependencies("x"), nil)
	assertNodes(t, graph.ReachableFrom("x"), nil)
	assertNodes(t, graph.ImpactSet("x"), nil)
	assertCycles(t, graph.Cycles(), nil)

	layers, err := graph.TopologicalLayers()
	if err != nil {
		t.Fatalf("TopologicalLayers() error = %v, want nil", err)
	}

	if layers != nil {
		t.Fatalf("TopologicalLayers() = %#v, want nil", layers)
	}
}

func assertNodes(t *testing.T, got, want []NodeID) {
	t.Helper()

	if !reflect.DeepEqual(got, want) {
		t.Fatalf("nodes = %#v, want %#v", got, want)
	}
}

func assertEdges(t *testing.T, got, want []Edge) {
	t.Helper()

	if !reflect.DeepEqual(got, want) {
		t.Fatalf("edges = %#v, want %#v", got, want)
	}
}

func assertCycles(t *testing.T, got, want [][]NodeID) {
	t.Helper()

	if !reflect.DeepEqual(got, want) {
		t.Fatalf("cycles = %#v, want %#v", got, want)
	}
}

func TestEvidenceGraph_ReturnsTypedEvidenceAndUncertainty(t *testing.T) {
	t.Parallel()

	graph := NewEvidence()
	graph.AddNode(Node{ID: "file:a.go", Kind: "file", Name: "a.go"})
	graph.AddNode(Node{ID: "import:fmt", Kind: "import", Name: "fmt"})
	graph.AddRelationship(Relationship{
		From: "file:a.go",
		To:   "import:fmt",
		Kind: "imports",
		Provenance: []Provenance{{
			Source:       "parser:import",
			File:         "a.go",
			StartLine:    3,
			StartColumn:  8,
			EndLine:      3,
			EndColumn:    13,
			BuildContext: "goos=darwin goarch=arm64",
			Confidence:   "high",
		}},
	})
	graph.AddRelationship(Relationship{From: "file:a.go", To: "missing", Kind: "declares"})

	results := graph.NeighborsWithEvidence("file:a.go")
	if len(results) != 2 {
		t.Fatalf("NeighborsWithEvidence() len = %d, want 2", len(results))
	}

	if results[0].Node.ID != "import:fmt" || results[0].Node.Kind != "import" {
		t.Fatalf("first neighbor = %#v, want typed import node", results[0].Node)
	}
	if len(results[0].Evidence) != 1 || results[0].Evidence[0].Kind != "imports" {
		t.Fatalf("first evidence = %#v, want imports relationship", results[0].Evidence)
	}
	if len(results[0].Uncertainty) != 0 {
		t.Fatalf("first uncertainty = %#v, want none", results[0].Uncertainty)
	}

	if results[1].Node.ID != "missing" || len(results[1].Uncertainty) == 0 {
		t.Fatalf("missing neighbor result = %#v, want uncertainty", results[1])
	}

	reverse := graph.ReverseDependenciesWithEvidence("import:fmt")
	if len(reverse) != 1 || reverse[0].Node.ID != "file:a.go" || reverse[0].Evidence[0].Kind != "imports" {
		t.Fatalf("ReverseDependenciesWithEvidence() = %#v, want file import evidence", reverse)
	}
}

func TestEvidenceGraph_CloneIsIndependent(t *testing.T) {
	t.Parallel()

	graph := NewEvidence()
	graph.AddNode(Node{ID: "a", Kind: "file"})
	graph.AddNode(Node{ID: "b", Kind: "import"})
	graph.AddRelationship(Relationship{From: "a", To: "b", Kind: "imports", Provenance: []Provenance{{Source: "parser"}}})

	clone := graph.Clone()
	clone.AddNode(Node{ID: "c", Kind: "file"})
	clone.AddRelationship(Relationship{From: "c", To: "b", Kind: "imports"})

	if graph.Graph().HasNode("c") {
		t.Fatal("original graph unexpectedly has cloned node c")
	}
	if len(graph.ReverseDependenciesWithEvidence("b")) != 1 {
		t.Fatalf("original reverse deps changed after clone mutation: %#v", graph.ReverseDependenciesWithEvidence("b"))
	}
}

func TestEvidenceGraph_TransitiveQueriesReturnEvidence(t *testing.T) {
	t.Parallel()

	const (
		apiNode     NodeID = "api"
		serviceNode NodeID = "service"
		storageNode NodeID = "storage"
		callKind           = "calls"
	)

	graph := NewEvidence()
	graph.AddNode(Node{ID: apiNode, Kind: "declaration"})
	graph.AddNode(Node{ID: serviceNode, Kind: "declaration"})
	graph.AddNode(Node{ID: storageNode, Kind: "declaration"})
	graph.AddRelationship(Relationship{From: apiNode, To: serviceNode, Kind: callKind, Provenance: []Provenance{{Source: "types:call", File: "api.go", StartLine: 10, Confidence: "high"}}})
	graph.AddRelationship(Relationship{From: serviceNode, To: storageNode, Kind: callKind, Provenance: []Provenance{{Source: "types:call", File: "service.go", StartLine: 20, Confidence: "high"}}})

	reachable := graph.ReachableFromWithEvidence(apiNode)
	if len(reachable) != 2 {
		t.Fatalf("ReachableFromWithEvidence() len = %d, want 2: %#v", len(reachable), reachable)
	}
	if reachable[0].Node.ID != serviceNode || reachable[0].Evidence[0].From != apiNode || reachable[0].Evidence[0].Kind != callKind {
		t.Fatalf("first reachable = %#v, want service call evidence", reachable[0])
	}
	if reachable[1].Node.ID != storageNode || reachable[1].Evidence[0].From != serviceNode || reachable[1].Evidence[0].Kind != callKind {
		t.Fatalf("second reachable = %#v, want storage call evidence", reachable[1])
	}

	impact := graph.ImpactSetWithEvidence(storageNode)
	if len(impact) != 2 {
		t.Fatalf("ImpactSetWithEvidence() len = %d, want 2: %#v", len(impact), impact)
	}
	if impact[0].Node.ID != apiNode || impact[0].Evidence[0].From != apiNode || impact[0].Evidence[0].To != serviceNode {
		t.Fatalf("first impact = %#v, want api->service evidence", impact[0])
	}
	if impact[1].Node.ID != serviceNode || impact[1].Evidence[0].From != serviceNode || impact[1].Evidence[0].To != storageNode {
		t.Fatalf("second impact = %#v, want service->storage evidence", impact[1])
	}
}
