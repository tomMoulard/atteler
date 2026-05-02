package async

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
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

func TestPlan_RunExecutesSameWaveConcurrently(t *testing.T) {
	t.Parallel()

	plan, err := NewPlan([]Task{
		{ID: "a", Agent: "executor", Prompt: "a"},
		{ID: "b", Agent: "executor", Prompt: "b"},
		{ID: "c", Agent: "executor", Prompt: "c"},
	})
	require.NoError(t, err)

	started := make(chan string, 3)
	release := make(chan struct{})
	done := make(chan error, 1)
	var results []TaskResult

	go func() {
		var err error
		results, err = plan.Run(context.Background(), func(ctx context.Context, task Task) (string, error) {
			started <- task.ID
			select {
			case <-release:
				return task.ID + "-done", nil
			case <-ctx.Done():
				return "", ctx.Err()
			}
		})
		done <- err
	}()

	gotStarted := make([]string, 0, 3)
	for range 3 {
		select {
		case id := <-started:
			gotStarted = append(gotStarted, id)
		case <-time.After(500 * time.Millisecond):
			t.Fatalf("Run did not start every same-wave task concurrently; started %v", gotStarted)
		}
	}

	close(release)

	select {
	case err := <-done:
		require.NoError(t, err)
		require.Len(t, results, 3)
		assert.Equal(t, []string{"a", "b", "c"}, resultIDs(results))
		for i := range results {
			result := results[i]
			assert.Equal(t, 0, result.Wave)
			assert.Equal(t, i, result.Order)
			assert.Equal(t, result.Task.ID+"-done", result.Output)
			assert.Empty(t, result.Error)
			assert.Positive(t, result.Duration)
		}
	case <-time.After(500 * time.Millisecond):
		t.Fatal("Run did not finish after same-wave tasks were released")
	}
}

func TestPlan_RunPreservesWaveOrderDespiteCompletionOrder(t *testing.T) {
	t.Parallel()

	plan, err := NewPlan([]Task{
		{ID: "slow", Agent: "executor", Prompt: "slow"},
		{ID: "fast", Agent: "executor", Prompt: "fast"},
		{ID: "after", Agent: "reviewer", Prompt: "after", DependsOn: []string{"slow", "fast"}},
	})
	require.NoError(t, err)

	results, err := plan.Run(context.Background(), func(_ context.Context, task Task) (string, error) {
		if task.ID == "slow" {
			time.Sleep(25 * time.Millisecond)
		}
		return task.ID + "-output", nil
	})

	require.NoError(t, err)
	require.Len(t, results, 3)
	assert.Equal(t, []string{"slow", "fast", "after"}, resultIDs(results))
	assert.Equal(t, []int{0, 0, 1}, resultWaves(results))
	assert.Equal(t, []int{0, 1, 0}, resultOrders(results))
}

func TestPlan_RunSkipsDownstreamWavesAfterFailure(t *testing.T) {
	t.Parallel()

	plan, err := NewPlan([]Task{
		{ID: "fail", Agent: "executor", Prompt: "fail"},
		{ID: "peer", Agent: "executor", Prompt: "peer"},
		{ID: "downstream", Agent: "reviewer", Prompt: "downstream", DependsOn: []string{"fail", "peer"}},
	})
	require.NoError(t, err)

	results, err := plan.Run(context.Background(), func(_ context.Context, task Task) (string, error) {
		if task.ID == "fail" {
			return "partial", errors.New("boom")
		}
		return task.ID + "-output", nil
	})

	require.Error(t, err)
	assert.Contains(t, err.Error(), `task "fail" failed: boom`)
	require.Len(t, results, 2)
	assert.Equal(t, []string{"fail", "peer"}, resultIDs(results))
	assert.Equal(t, "boom", results[0].Error)
	assert.Equal(t, "partial", results[0].Output)
	assert.Empty(t, results[1].Error)
}

func TestPlan_RunRejectsNilRunner(t *testing.T) {
	t.Parallel()

	plan, err := NewPlan([]Task{{ID: "task"}})
	require.NoError(t, err)

	results, err := plan.Run(context.Background(), nil)
	require.Error(t, err)
	assert.Nil(t, results)
	assert.Contains(t, err.Error(), "task runner is nil")
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

func resultIDs(results []TaskResult) []string {
	ids := make([]string, len(results))
	for i := range results {
		ids[i] = results[i].Task.ID
	}
	return ids
}

func resultWaves(results []TaskResult) []int {
	waves := make([]int, len(results))
	for i := range results {
		waves[i] = results[i].Wave
	}
	return waves
}

func resultOrders(results []TaskResult) []int {
	orders := make([]int, len(results))
	for i := range results {
		orders[i] = results[i].Order
	}
	return orders
}
