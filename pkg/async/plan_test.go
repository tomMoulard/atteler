package async

import (
	"reflect"
	"strings"
	"testing"
)

func TestPlan_ReadyBatches(t *testing.T) {
	t.Parallel()

	plan, err := NewPlan([]Task{
		{ID: "design", Agent: "architect", Prompt: "design"},
		{ID: "docs", Agent: "writer", Prompt: "docs"},
		{ID: "build", Agent: "executor", Prompt: "build", DependsOn: []string{"design"}},
		{ID: "review", Agent: "reviewer", Prompt: "review", DependsOn: []string{"build", "docs"}},
	})
	if err != nil {
		t.Fatalf("NewPlan() error = %v", err)
	}

	got := batchIDs(plan.ReadyBatches())
	want := [][]string{{"design", "docs"}, {"build"}, {"review"}}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("ReadyBatches() = %v, want %v", got, want)
	}
}

func TestPlan_DuplicateTaskID(t *testing.T) {
	t.Parallel()

	_, err := NewPlan([]Task{{ID: "same"}, {ID: "same"}})
	if err == nil || !strings.Contains(err.Error(), "duplicate task") {
		t.Fatalf("NewPlan() error = %v, want duplicate task error", err)
	}
}

func TestPlan_MissingDependency(t *testing.T) {
	t.Parallel()

	_, err := NewPlan([]Task{{ID: "child", DependsOn: []string{"missing"}}})
	if err == nil || !strings.Contains(err.Error(), "missing task") {
		t.Fatalf("NewPlan() error = %v, want missing task error", err)
	}
}

func TestPlan_CyclicDependency(t *testing.T) {
	t.Parallel()

	_, err := NewPlan([]Task{
		{ID: "a", DependsOn: []string{"b"}},
		{ID: "b", DependsOn: []string{"c"}},
		{ID: "c", DependsOn: []string{"a"}},
	})
	if err == nil || !strings.Contains(err.Error(), "cyclic dependency") {
		t.Fatalf("NewPlan() error = %v, want cyclic dependency error", err)
	}
}

func TestPlan_SpawnDerivesChildFromExistingParent(t *testing.T) {
	t.Parallel()

	plan, err := NewPlan([]Task{{ID: "parent", Agent: "planner", Prompt: "plan"}})
	if err != nil {
		t.Fatalf("NewPlan() error = %v", err)
	}

	child, err := plan.Spawn("parent", "child", "executor", "implement")
	if err != nil {
		t.Fatalf("Spawn() error = %v", err)
	}

	want := Task{ID: "child", Agent: "executor", Prompt: "implement", DependsOn: []string{"parent"}}
	if !reflect.DeepEqual(child, want) {
		t.Fatalf("Spawn() = %+v, want %+v", child, want)
	}
}

func TestTask_SpawnDerivesChildMetadata(t *testing.T) {
	t.Parallel()

	parent := Task{ID: "parent", Agent: "planner", Prompt: "plan", DependsOn: []string{"root"}}
	child := parent.Spawn("child", "executor", "implement")

	want := Task{ID: "child", Agent: "executor", Prompt: "implement", DependsOn: []string{"parent"}}
	if !reflect.DeepEqual(child, want) {
		t.Fatalf("Spawn() = %+v, want %+v", child, want)
	}
}

func batchIDs(batches [][]Task) [][]string {
	ids := make([][]string, len(batches))
	for i, batch := range batches {
		ids[i] = make([]string, len(batch))
		for j, task := range batch {
			ids[i][j] = task.ID
		}
	}
	return ids
}
