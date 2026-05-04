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
