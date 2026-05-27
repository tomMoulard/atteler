//nolint:wsl_v5 // Tests intentionally group setup/assertions for scenario readability.
package async

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const (
	doneOutput         = "done"
	lateSuccessOutput  = "late success"
	partialOutput      = "partial"
	failingID          = "fail"
	recoveredOutput    = "recovered"
	shouldNotRunOutput = "should not run"
	slowID             = "slow"
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

func TestPlan_RunUsesDefaultBoundedConcurrency(t *testing.T) {
	t.Parallel()

	tasks := make([]Task, defaultMaxConcurrency*3)
	for i := range tasks {
		id := fmt.Sprintf("task-%02d", i)
		tasks[i] = Task{ID: id, Agent: "executor", Prompt: id}
	}

	plan, err := NewPlan(tasks)
	require.NoError(t, err)

	var current atomic.Int32
	var maxSeen atomic.Int32
	results, err := plan.Run(t.Context(), func(context.Context, Task) (string, error) {
		active := current.Add(1)
		for {
			seen := maxSeen.Load()
			if active <= seen || maxSeen.CompareAndSwap(seen, active) {
				break
			}
		}

		time.Sleep(10 * time.Millisecond)
		current.Add(-1)

		return doneOutput, nil
	})

	require.NoError(t, err)
	require.Len(t, results, len(tasks))
	assert.LessOrEqual(t, maxSeen.Load(), int32(defaultMaxConcurrency))
}

func TestPlan_RunPreservesWaveOrderDespiteCompletionOrder(t *testing.T) {
	t.Parallel()

	plan, err := NewPlan([]Task{
		{ID: slowID, Agent: "executor", Prompt: slowID},
		{ID: "fast", Agent: "executor", Prompt: "fast"},
		{ID: "after", Agent: "reviewer", Prompt: "after", DependsOn: []string{slowID, "fast"}},
	})
	require.NoError(t, err)

	results, err := plan.Run(context.Background(), func(_ context.Context, task Task) (string, error) {
		if task.ID == slowID {
			time.Sleep(25 * time.Millisecond)
		}

		return task.ID + "-output", nil
	})

	require.NoError(t, err)
	require.Len(t, results, 3)
	assert.Equal(t, []string{slowID, "fast", "after"}, resultIDs(results))
	assert.Equal(t, []int{0, 0, 1}, resultWaves(results))
	assert.Equal(t, []int{0, 1, 0}, resultOrders(results))
}

func TestPlan_RunSkipsDownstreamWavesAfterFailure(t *testing.T) {
	t.Parallel()

	plan, err := NewPlan([]Task{
		{ID: failingID, Agent: "executor", Prompt: failingID},
		{ID: "peer", Agent: "executor", Prompt: "peer"},
		{ID: "downstream", Agent: "reviewer", Prompt: "downstream", DependsOn: []string{failingID, "peer"}},
	})
	require.NoError(t, err)

	results, err := plan.Run(context.Background(), func(_ context.Context, task Task) (string, error) {
		if task.ID == failingID {
			return partialOutput, errors.New("boom")
		}

		return task.ID + "-output", nil
	})

	require.Error(t, err)
	assert.Contains(t, err.Error(), `task "fail" failed: boom`)
	require.Len(t, results, 2)
	assert.Equal(t, []string{failingID, "peer"}, resultIDs(results))
	assert.Equal(t, "boom", results[0].Error)
	assert.Equal(t, partialOutput, results[0].Output)
	assert.Empty(t, results[1].Error)
}

func TestPlan_RunWithOptions_PersistsDownstreamAdmissionDenialsAfterFailure(t *testing.T) {
	t.Parallel()

	plan, err := NewPlan([]Task{
		{ID: failingID, Agent: "executor", Prompt: failingID},
		{ID: "peer", Agent: "executor", Prompt: "peer"},
		{ID: "downstream", Agent: "reviewer", Prompt: "downstream", DependsOn: []string{"peer"}},
	})
	require.NoError(t, err)

	ledgerPath := filepath.Join(t.TempDir(), "async-ledger.json")
	results, err := plan.RunWithOptions(t.Context(), func(_ context.Context, task Task) (string, error) {
		if task.ID == failingID {
			return partialOutput, errors.New("boom")
		}

		return task.ID + "-output", nil
	}, RunOptions{LedgerPath: ledgerPath})

	require.Error(t, err)
	assert.Contains(t, err.Error(), `task "fail" failed: boom`)
	require.Len(t, results, 3)
	assert.Equal(t, []string{failingID, "peer", "downstream"}, resultIDs(results))
	assert.Equal(t, StatusFailed, results[0].Status)
	assert.Equal(t, StatusSucceeded, results[1].Status)
	assert.Equal(t, StatusDenied, results[2].Status)
	assert.Contains(t, results[2].Error, `upstream wave halted after task "fail" failed`)
	assert.NotEmpty(t, results[2].AdmissionID)

	var ledger Ledger
	data, readErr := os.ReadFile(ledgerPath)
	require.NoError(t, readErr)
	require.NoError(t, json.Unmarshal(data, &ledger))
	require.Len(t, ledger.Admissions, 3)
	require.Len(t, ledger.Attempts, 2)
	require.Len(t, ledger.Results, 3)

	admissions := map[string]Admission{}
	for _, admission := range ledger.Admissions {
		admissions[admission.ChildID] = admission
	}
	require.Contains(t, admissions, "downstream")
	assert.False(t, admissions["downstream"].Admitted)
	assert.Contains(t, admissions["downstream"].DenyReason, `upstream wave halted after task "fail" failed`)
	assert.Equal(t, admissions["downstream"].AdmissionID, results[2].AdmissionID)
	assert.Equal(t, admissions["downstream"].AdmissionID, ledger.Results[2].AdmissionID)
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
		for j := range batch {
			ids[i][j] = batch[j].ID
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

func TestPlan_RunWithOptions_CancelOnFailureCancelsSibling(t *testing.T) {
	t.Parallel()

	plan, err := NewPlan([]Task{
		{ID: failingID, Agent: "executor", Prompt: failingID},
		{ID: slowID, Agent: "executor", Prompt: slowID},
	})
	require.NoError(t, err)

	slowStarted := make(chan struct{})
	releaseFailure := make(chan struct{})

	results, err := plan.RunWithOptions(t.Context(), func(ctx context.Context, task Task) (string, error) {
		switch task.ID {
		case failingID:
			<-slowStarted
			close(releaseFailure)
			return partialOutput, errors.New("boom")
		case slowID:
			close(slowStarted)
			<-releaseFailure
			<-ctx.Done()
			return "", ctx.Err()
		default:
			return "", nil
		}
	}, RunOptions{MaxConcurrency: 2, CancelOnFailure: true})

	require.Error(t, err)
	require.Len(t, results, 2)
	assert.Equal(t, StatusFailed, results[0].Status)
	assert.Equal(t, StatusCanceled, results[1].Status)
	assert.Contains(t, results[1].Error, `sibling cancellation after task "fail" failed`)
}

func TestPlan_RunWithOptions_CancelOnFailureMarksSiblingCanceledWhenRunnerIgnoresContext(t *testing.T) {
	t.Parallel()

	plan, err := NewPlan([]Task{
		{ID: failingID, Agent: "executor", Prompt: failingID},
		{ID: slowID, Agent: "executor", Prompt: slowID},
	})
	require.NoError(t, err)

	slowStarted := make(chan struct{})
	releaseFailure := make(chan struct{})

	results, err := plan.RunWithOptions(t.Context(), func(ctx context.Context, task Task) (string, error) {
		switch task.ID {
		case failingID:
			<-slowStarted
			close(releaseFailure)
			return partialOutput, errors.New("boom")
		case slowID:
			close(slowStarted)
			<-releaseFailure
			<-ctx.Done()
			return lateSuccessOutput, nil
		default:
			return "", nil
		}
	}, RunOptions{MaxConcurrency: 2, CancelOnFailure: true})

	require.Error(t, err)
	require.Len(t, results, 2)
	assert.Equal(t, StatusFailed, results[0].Status)
	assert.Equal(t, StatusCanceled, results[1].Status)
	assert.Contains(t, results[1].Error, context.Canceled.Error())
	assert.Contains(t, results[1].Error, `sibling cancellation after task "fail" failed`)
}

func TestPlan_RunWithOptions_CancelOnFailureBeatsTimeoutWhenSiblingIgnoresContext(t *testing.T) {
	t.Parallel()

	plan, err := NewPlan([]Task{
		{ID: failingID, Agent: "executor", Prompt: failingID},
		{ID: slowID, Agent: "executor", Prompt: slowID},
	})
	require.NoError(t, err)

	slowStarted := make(chan struct{})
	releaseFailure := make(chan struct{})

	results, err := plan.RunWithOptions(t.Context(), func(_ context.Context, task Task) (string, error) {
		switch task.ID {
		case failingID:
			<-slowStarted
			close(releaseFailure)
			return partialOutput, errors.New("boom")
		case slowID:
			close(slowStarted)
			<-releaseFailure
			time.Sleep(20 * time.Millisecond)
			return lateSuccessOutput, nil
		default:
			return "", nil
		}
	}, RunOptions{MaxConcurrency: 2, CancelOnFailure: true, Timeout: 5 * time.Millisecond})

	require.Error(t, err)
	require.Len(t, results, 2)
	assert.Equal(t, StatusFailed, results[0].Status)
	assert.Equal(t, StatusCanceled, results[1].Status)
	assert.Contains(t, results[1].Error, `sibling cancellation after task "fail" failed`)
	assert.NotContains(t, results[1].Error, context.DeadlineExceeded.Error())
}

func TestPlan_RunWithOptions_ParentCancellationMarksDownstreamCanceled(t *testing.T) {
	t.Parallel()

	plan, err := NewPlan([]Task{
		{ID: "running", Agent: "executor", Prompt: "running"},
		{ID: "after", Agent: "reviewer", Prompt: "after", DependsOn: []string{"running"}},
	})
	require.NoError(t, err)

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()

	ledgerPath := filepath.Join(t.TempDir(), "async-ledger.json")
	results, err := plan.RunWithOptions(ctx, func(ctx context.Context, task Task) (string, error) {
		if task.ID == "running" {
			cancel()
			<-ctx.Done()

			return "", ctx.Err()
		}

		return shouldNotRunOutput, nil
	}, RunOptions{LedgerPath: ledgerPath})

	require.Error(t, err)
	assert.Contains(t, err.Error(), "context canceled after wave 0")
	require.Len(t, results, 2)
	assert.Equal(t, StatusCanceled, results[0].Status)
	assert.Equal(t, StatusCanceled, results[1].Status)
	assert.NotEmpty(t, results[1].AdmissionID)

	var ledger Ledger
	data, readErr := os.ReadFile(ledgerPath)
	require.NoError(t, readErr)
	require.NoError(t, json.Unmarshal(data, &ledger))
	require.Len(t, ledger.Admissions, 2)
	require.Len(t, ledger.Attempts, 1)
	require.Len(t, ledger.Results, 2)
	assert.True(t, ledger.Admissions[0].Admitted)
	assert.False(t, ledger.Admissions[1].Admitted)
	assert.Equal(t, "after", ledger.Admissions[1].ChildID)
	assert.Contains(t, ledger.Admissions[1].DenyReason, context.Canceled.Error())
	assert.Equal(t, StatusCanceled, ledger.Results[1].Status)
	assert.Equal(t, ledger.Admissions[1].AdmissionID, ledger.Results[1].AdmissionID)
}

func TestPlan_RunWithOptions_ParentCancellationPreservesCauseInDownstreamAdmission(t *testing.T) {
	t.Parallel()

	plan, err := NewPlan([]Task{
		{ID: "running", Agent: "executor", Prompt: "running"},
		{ID: "after", Agent: "reviewer", Prompt: "after", DependsOn: []string{"running"}},
	})
	require.NoError(t, err)

	cancelCause := errors.New("operator stopped async run")
	ctx, cancel := context.WithCancelCause(t.Context())
	defer cancel(nil)

	ledgerPath := filepath.Join(t.TempDir(), "async-ledger.json")
	results, err := plan.RunWithOptions(ctx, func(ctx context.Context, task Task) (string, error) {
		if task.ID == "running" {
			cancel(cancelCause)
			<-ctx.Done()

			return "", ctx.Err()
		}

		return shouldNotRunOutput, nil
	}, RunOptions{LedgerPath: ledgerPath})

	require.Error(t, err)
	assert.Contains(t, err.Error(), "context canceled after wave 0")
	assert.Contains(t, err.Error(), cancelCause.Error())
	require.Len(t, results, 2)
	assert.Equal(t, StatusCanceled, results[0].Status)
	assert.Contains(t, results[0].Error, cancelCause.Error())
	assert.Equal(t, StatusCanceled, results[1].Status)
	assert.Contains(t, results[1].Error, cancelCause.Error())

	var ledger Ledger
	data, readErr := os.ReadFile(ledgerPath)
	require.NoError(t, readErr)
	require.NoError(t, json.Unmarshal(data, &ledger))
	require.Len(t, ledger.Admissions, 2)
	assert.True(t, ledger.Admissions[0].Admitted)
	assert.False(t, ledger.Admissions[1].Admitted)
	assert.Equal(t, "after", ledger.Admissions[1].ChildID)
	assert.Contains(t, ledger.Admissions[1].DenyReason, cancelCause.Error())
}

func TestPlan_RunWithOptions_RetriesPersistsAndResumes(t *testing.T) {
	t.Parallel()

	plan, err := NewPlan([]Task{
		{ID: "ok", Agent: "executor", Prompt: "ok"},
		{ID: "flaky", Agent: "executor", Prompt: "flaky"},
		{ID: "after", Agent: "reviewer", Prompt: "after", DependsOn: []string{"ok", "flaky"}},
	})
	require.NoError(t, err)

	ledgerPath := filepath.Join(t.TempDir(), "async-ledger.json")
	var mu sync.Mutex
	calls := map[string]int{}
	runner := func(_ context.Context, task Task) (string, error) {
		mu.Lock()
		calls[task.ID]++
		call := calls[task.ID]
		mu.Unlock()

		if task.ID == "flaky" && call == 1 {
			return partialOutput, errors.New("try again")
		}

		return task.ID + "-done", nil
	}

	results, err := plan.RunWithOptions(t.Context(), runner, RunOptions{
		LedgerPath:  ledgerPath,
		RetryPolicy: RetryPolicy{MaxAttempts: 2},
		Model:       "codex/gpt-test",
		WorkspaceID: "parent-run-a",
	})
	require.NoError(t, err)
	require.Len(t, results, 3)
	assert.Equal(t, StatusSucceeded, results[1].Status)
	assert.Equal(t, 2, results[1].Attempts)

	var ledger Ledger
	data, err := os.ReadFile(ledgerPath)
	require.NoError(t, err)
	require.NoError(t, json.Unmarshal(data, &ledger))
	require.Len(t, ledger.Results, 3)
	require.Len(t, ledger.Attempts, 4)
	assert.Equal(t, "codex/gpt-test", ledger.Attempts[0].Task.Model)
	assert.NotEmpty(t, ledger.Attempts[0].Task.WorkspaceID)
	assert.NotEqual(t, ledger.Attempts[0].Task.WorkspaceID, ledger.Attempts[1].Task.WorkspaceID)

	mu.Lock()
	calls = map[string]int{}
	mu.Unlock()

	resumed, err := plan.RunWithOptions(t.Context(), runner, RunOptions{
		LedgerPath:  ledgerPath,
		Resume:      true,
		Model:       "codex/gpt-test",
		WorkspaceID: "parent-run-b",
	})
	require.NoError(t, err)
	require.Len(t, resumed, 3)
	assert.Equal(t, []string{StatusSkipped, StatusSkipped, StatusSkipped}, []string{resumed[0].Status, resumed[1].Status, resumed[2].Status})
	assert.True(t, resumed[0].Resumed)
	assert.Equal(t, "parent-run-b/ok", resumed[0].Task.WorkspaceID)
	assert.Equal(t, "after-done", resumed[2].Output)

	mu.Lock()
	assert.Empty(t, calls)
	calls = map[string]int{}
	mu.Unlock()

	changedPlan, err := NewPlan([]Task{
		{ID: "ok", Agent: "executor", Prompt: "changed"},
		{ID: "flaky", Agent: "executor", Prompt: "flaky"},
		{ID: "after", Agent: "reviewer", Prompt: "after", DependsOn: []string{"ok", "flaky"}},
	})
	require.NoError(t, err)

	changed, err := changedPlan.RunWithOptions(t.Context(), runner, RunOptions{
		LedgerPath:  ledgerPath,
		Resume:      true,
		Model:       "codex/gpt-test",
		WorkspaceID: "parent-run-b",
	})
	require.NoError(t, err)
	require.Len(t, changed, 3)
	assert.Equal(t, StatusSucceeded, changed[0].Status)
	assert.False(t, changed[0].Resumed)
	assert.Equal(t, StatusSkipped, changed[1].Status)
	assert.True(t, changed[1].Resumed)
	assert.Equal(t, StatusSucceeded, changed[2].Status)
	assert.False(t, changed[2].Resumed)

	mu.Lock()
	defer mu.Unlock()
	assert.Equal(t, 1, calls["ok"])
	assert.Zero(t, calls["flaky"])
	assert.Equal(t, 1, calls["after"])
}

func TestPlan_RunWithOptions_ResumesAfterFailedWave(t *testing.T) {
	t.Parallel()

	plan, err := NewPlan([]Task{
		{ID: "ok", Agent: "executor", Prompt: "ok"},
		{ID: failingID, Agent: "executor", Prompt: failingID},
		{ID: "after", Agent: "reviewer", Prompt: "after", DependsOn: []string{failingID}},
	})
	require.NoError(t, err)

	ledgerPath := filepath.Join(t.TempDir(), "async-ledger.json")
	var failing atomic.Bool
	failing.Store(true)
	var mu sync.Mutex
	calls := map[string]int{}
	runner := func(_ context.Context, task Task) (string, error) {
		mu.Lock()
		calls[task.ID]++
		mu.Unlock()

		if task.ID == failingID && failing.Load() {
			return partialOutput, errors.New("boom")
		}

		return task.ID + "-done", nil
	}

	results, err := plan.RunWithOptions(t.Context(), runner, RunOptions{LedgerPath: ledgerPath})
	require.Error(t, err)
	require.Len(t, results, 3)
	assert.Equal(t, StatusSucceeded, results[0].Status)
	assert.Equal(t, StatusFailed, results[1].Status)
	assert.Equal(t, StatusDenied, results[2].Status)
	assert.Contains(t, results[2].Error, `upstream wave halted after task "fail" failed`)

	failing.Store(false)
	mu.Lock()
	calls = map[string]int{}
	mu.Unlock()

	resumed, err := plan.RunWithOptions(t.Context(), runner, RunOptions{LedgerPath: ledgerPath, Resume: true})
	require.NoError(t, err)
	require.Len(t, resumed, 3)
	assert.Equal(t, StatusSkipped, resumed[0].Status)
	assert.True(t, resumed[0].Resumed)
	assert.Equal(t, StatusSucceeded, resumed[1].Status)
	assert.False(t, resumed[1].Resumed)
	assert.Equal(t, StatusSucceeded, resumed[2].Status)
	assert.False(t, resumed[2].Resumed)

	mu.Lock()
	defer mu.Unlock()
	assert.Zero(t, calls["ok"])
	assert.Equal(t, 1, calls[failingID])
	assert.Equal(t, 1, calls["after"])
}

func TestPlan_RunWithOptions_RecordsAdmissionDenialWhenRetryBackoffCanceled(t *testing.T) {
	t.Parallel()

	plan, err := NewPlan([]Task{{ID: "flaky", Agent: "executor", Prompt: "flaky"}})
	require.NoError(t, err)

	ledgerPath := filepath.Join(t.TempDir(), "async-ledger.json")
	parentCtx, stopTimeout := context.WithTimeout(t.Context(), 2*time.Second)
	defer stopTimeout()
	ctx, cancel := context.WithCancelCause(parentCtx)
	defer cancel(nil)
	cancelCause := errors.New("operator stopped async retry")
	go cancelAfterFailedAttempt(ctx, func() { cancel(cancelCause) }, ledgerPath)

	var calls atomic.Int32
	results, err := plan.RunWithOptions(ctx, func(context.Context, Task) (string, error) {
		if calls.Add(1) == 1 {
			return partialOutput, errors.New("try again")
		}

		return shouldNotRunOutput, nil
	}, RunOptions{
		LedgerPath: ledgerPath,
		RetryPolicy: RetryPolicy{
			MaxAttempts: 2,
			Backoff:     time.Hour,
		},
	})

	require.Error(t, err)
	require.Len(t, results, 1)
	assert.Equal(t, StatusCanceled, results[0].Status)
	assert.Equal(t, 1, results[0].Attempts)
	assert.Contains(t, results[0].Error, cancelCause.Error())
	assert.Equal(t, int32(1), calls.Load())

	var ledger Ledger
	data, readErr := os.ReadFile(ledgerPath)
	require.NoError(t, readErr)
	require.NoError(t, json.Unmarshal(data, &ledger))
	require.Len(t, ledger.Admissions, 2)
	assert.True(t, ledger.Admissions[0].Admitted)
	assert.Equal(t, 1, ledger.Admissions[0].Attempt)
	assert.False(t, ledger.Admissions[1].Admitted)
	assert.Equal(t, 2, ledger.Admissions[1].Attempt)
	assert.Contains(t, ledger.Admissions[1].DenyReason, "retry backoff canceled")
	assert.Contains(t, ledger.Admissions[1].DenyReason, cancelCause.Error())
	assert.Equal(t, ledger.Admissions[1].AdmissionID, results[0].AdmissionID)
	require.Len(t, ledger.Attempts, 1)
	assert.Equal(t, StatusFailed, ledger.Attempts[0].Status)
	assert.Equal(t, ledger.Admissions[0].AdmissionID, ledger.Attempts[0].AdmissionID)
	require.Len(t, ledger.Results, 1)
	assert.Equal(t, StatusCanceled, ledger.Results[0].Status)
	assert.Equal(t, ledger.Admissions[1].AdmissionID, ledger.Results[0].AdmissionID)
}

func cancelAfterFailedAttempt(ctx context.Context, cancel context.CancelFunc, ledgerPath string) {
	ticker := time.NewTicker(time.Millisecond)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			data, err := os.ReadFile(ledgerPath)
			if err != nil {
				continue
			}

			var ledger Ledger
			if err := json.Unmarshal(data, &ledger); err != nil {
				continue
			}

			for i := range ledger.Attempts {
				if ledger.Attempts[i].Status == StatusFailed {
					cancel()
					return
				}
			}
		}
	}
}

func TestPlan_RunWithOptions_ResumeRerunsAfterLatestMatchingFailure(t *testing.T) {
	t.Parallel()

	plan, err := NewPlan([]Task{{ID: "child", Agent: "executor", Prompt: "child"}})
	require.NoError(t, err)

	ledgerPath := filepath.Join(t.TempDir(), "async-ledger.json")
	results, err := plan.RunWithOptions(t.Context(), func(context.Context, Task) (string, error) {
		return "first success", nil
	}, RunOptions{LedgerPath: ledgerPath})
	require.NoError(t, err)
	require.Len(t, results, 1)
	assert.Equal(t, StatusSucceeded, results[0].Status)

	results, err = plan.RunWithOptions(t.Context(), func(context.Context, Task) (string, error) {
		return partialOutput, errors.New("latest failure")
	}, RunOptions{LedgerPath: ledgerPath})
	require.Error(t, err)
	require.Len(t, results, 1)
	assert.Equal(t, StatusFailed, results[0].Status)

	var calls atomic.Int32
	resumed, err := plan.RunWithOptions(t.Context(), func(context.Context, Task) (string, error) {
		calls.Add(1)
		return recoveredOutput, nil
	}, RunOptions{LedgerPath: ledgerPath, Resume: true})
	require.NoError(t, err)
	require.Len(t, resumed, 1)
	assert.Equal(t, StatusSucceeded, resumed[0].Status)
	assert.False(t, resumed[0].Resumed)
	assert.Equal(t, recoveredOutput, resumed[0].Output)
	assert.Equal(t, int32(1), calls.Load())
}

func TestPlan_RunWithOptions_ResumeRerunsAfterCanceledResultWithoutAttempt(t *testing.T) {
	t.Parallel()

	ledgerPath := filepath.Join(t.TempDir(), "async-ledger.json")
	initial, err := NewPlan([]Task{{ID: "child", Agent: "executor", Prompt: "child"}})
	require.NoError(t, err)
	results, err := initial.RunWithOptions(t.Context(), func(context.Context, Task) (string, error) {
		return "first success", nil
	}, RunOptions{LedgerPath: ledgerPath})
	require.NoError(t, err)
	require.Len(t, results, 1)
	assert.Equal(t, StatusSucceeded, results[0].Status)

	canceled, err := NewPlan([]Task{
		{ID: failingID, Agent: "executor", Prompt: failingID},
		{ID: "child", Agent: "executor", Prompt: "child"},
	})
	require.NoError(t, err)
	results, err = canceled.RunWithOptions(t.Context(), func(context.Context, Task) (string, error) {
		return partialOutput, errors.New("cancel sibling")
	}, RunOptions{LedgerPath: ledgerPath, MaxConcurrency: 1, CancelOnFailure: true})
	require.Error(t, err)
	require.Len(t, results, 2)
	assert.Equal(t, StatusFailed, results[0].Status)
	assert.Equal(t, StatusCanceled, results[1].Status)
	assert.Zero(t, results[1].Attempts)

	var calls atomic.Int32
	resumePlan, err := NewPlan([]Task{{ID: "child", Agent: "executor", Prompt: "child"}})
	require.NoError(t, err)
	resumed, err := resumePlan.RunWithOptions(t.Context(), func(context.Context, Task) (string, error) {
		calls.Add(1)
		return recoveredOutput, nil
	}, RunOptions{LedgerPath: ledgerPath, Resume: true})
	require.NoError(t, err)
	require.Len(t, resumed, 1)
	assert.Equal(t, StatusSucceeded, resumed[0].Status)
	assert.False(t, resumed[0].Resumed)
	assert.Equal(t, recoveredOutput, resumed[0].Output)
	assert.Equal(t, int32(1), calls.Load())
}

func TestPlan_RunWithOptions_ResumesFromSuccessfulAttemptWhenResultMissing(t *testing.T) {
	t.Parallel()

	plan, err := NewPlan([]Task{{ID: "ok", Agent: "executor", Prompt: "ok"}})
	require.NoError(t, err)

	ledgerPath := filepath.Join(t.TempDir(), "async-ledger.json")
	var calls atomic.Int32
	results, err := plan.RunWithOptions(t.Context(), func(context.Context, Task) (string, error) {
		calls.Add(1)
		return "ok-done", nil
	}, RunOptions{LedgerPath: ledgerPath})
	require.NoError(t, err)
	require.Len(t, results, 1)

	var ledger Ledger
	data, err := os.ReadFile(ledgerPath)
	require.NoError(t, err)
	require.NoError(t, json.Unmarshal(data, &ledger))
	require.NotEmpty(t, ledger.Attempts)
	ledger.Results = nil
	data, err = json.MarshalIndent(ledger, "", "  ")
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(ledgerPath, append(data, '\n'), 0o600))

	calls.Store(0)
	resumed, err := plan.RunWithOptions(t.Context(), func(context.Context, Task) (string, error) {
		calls.Add(1)
		return "rerun", nil
	}, RunOptions{LedgerPath: ledgerPath, Resume: true})

	require.NoError(t, err)
	require.Len(t, resumed, 1)
	assert.Equal(t, StatusSkipped, resumed[0].Status)
	assert.True(t, resumed[0].Resumed)
	assert.Equal(t, "ok-done", resumed[0].Output)
	assert.Equal(t, int32(0), calls.Load())

	data, err = os.ReadFile(ledgerPath)
	require.NoError(t, err)
	require.NoError(t, json.Unmarshal(data, &ledger))
	require.Len(t, ledger.Results, 1)
	assert.Equal(t, StatusSkipped, ledger.Results[0].Status)
	assert.True(t, ledger.Results[0].Resumed)
}

func TestPlan_RunWithOptions_ResumePreservesResultOnlySuccessForChangedTaskID(t *testing.T) {
	t.Parallel()

	ledgerPath := filepath.Join(t.TempDir(), "async-ledger.json")
	original, err := NewPlan([]Task{{ID: "child", Agent: "executor", Prompt: "child"}})
	require.NoError(t, err)

	results, err := original.RunWithOptions(t.Context(), func(context.Context, Task) (string, error) {
		return "original success", nil
	}, RunOptions{LedgerPath: ledgerPath})
	require.NoError(t, err)
	require.Len(t, results, 1)
	assert.Equal(t, StatusSucceeded, results[0].Status)

	var ledger Ledger
	data, err := os.ReadFile(ledgerPath)
	require.NoError(t, err)
	require.NoError(t, json.Unmarshal(data, &ledger))
	ledger.Attempts = nil
	data, err = json.MarshalIndent(ledger, "", "  ")
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(ledgerPath, append(data, '\n'), 0o600))

	changedCanceled, err := NewPlan([]Task{
		{ID: failingID, Agent: "executor", Prompt: failingID},
		{ID: "child", Agent: "executor", Prompt: "changed"},
	})
	require.NoError(t, err)
	results, err = changedCanceled.RunWithOptions(t.Context(), func(context.Context, Task) (string, error) {
		return partialOutput, errors.New("cancel sibling")
	}, RunOptions{LedgerPath: ledgerPath, Resume: true, MaxConcurrency: 1, CancelOnFailure: true})
	require.Error(t, err)
	require.Len(t, results, 2)
	assert.Equal(t, StatusCanceled, results[1].Status)
	assert.Zero(t, results[1].Attempts)

	var calls atomic.Int32
	resumed, err := original.RunWithOptions(t.Context(), func(context.Context, Task) (string, error) {
		calls.Add(1)
		return recoveredOutput, nil
	}, RunOptions{LedgerPath: ledgerPath, Resume: true})
	require.NoError(t, err)
	require.Len(t, resumed, 1)
	assert.Equal(t, StatusSkipped, resumed[0].Status)
	assert.True(t, resumed[0].Resumed)
	assert.Equal(t, "original success", resumed[0].Output)
	assert.Equal(t, int32(0), calls.Load())

	data, err = os.ReadFile(ledgerPath)
	require.NoError(t, err)
	require.NoError(t, json.Unmarshal(data, &ledger))
	assert.Len(t, ledger.Results, 3)
}

func TestPlan_RunWithOptions_ResumeIgnoresDependencyOrder(t *testing.T) {
	t.Parallel()

	ledgerPath := filepath.Join(t.TempDir(), "async-ledger.json")
	initial, err := NewPlan([]Task{
		{ID: "a", Agent: "executor", Prompt: "a"},
		{ID: "b", Agent: "executor", Prompt: "b"},
		{ID: "child", Agent: "executor", Prompt: "child", DependsOn: []string{"a", "b"}},
	})
	require.NoError(t, err)

	results, err := initial.RunWithOptions(t.Context(), func(context.Context, Task) (string, error) {
		return doneOutput, nil
	}, RunOptions{LedgerPath: ledgerPath})
	require.NoError(t, err)
	require.Len(t, results, 3)

	reordered, err := NewPlan([]Task{
		{ID: "a", Agent: "executor", Prompt: "a"},
		{ID: "b", Agent: "executor", Prompt: "b"},
		{ID: "child", Agent: "executor", Prompt: "child", DependsOn: []string{"b", "a"}},
	})
	require.NoError(t, err)

	var calls atomic.Int32
	resumed, err := reordered.RunWithOptions(t.Context(), func(context.Context, Task) (string, error) {
		calls.Add(1)
		return "rerun", nil
	}, RunOptions{LedgerPath: ledgerPath, Resume: true})
	require.NoError(t, err)
	require.Len(t, resumed, 3)
	assert.Equal(t, []string{StatusSkipped, StatusSkipped, StatusSkipped}, []string{resumed[0].Status, resumed[1].Status, resumed[2].Status})
	assert.True(t, resumed[2].Resumed)
	assert.Equal(t, int32(0), calls.Load())
}

func TestPlan_RunWithOptions_ResumeRequiresExistingLedger(t *testing.T) {
	t.Parallel()

	plan, err := NewPlan([]Task{{ID: "ok", Agent: "executor", Prompt: "ok"}})
	require.NoError(t, err)

	var calls atomic.Int32
	runner := func(context.Context, Task) (string, error) {
		calls.Add(1)
		return shouldNotRunOutput, nil
	}

	results, err := plan.RunWithOptions(t.Context(), runner, RunOptions{Resume: true})
	require.Error(t, err)
	assert.Nil(t, results)
	assert.Contains(t, err.Error(), "resume requires ledger path")

	missing := filepath.Join(t.TempDir(), "missing-ledger.json")
	results, err = plan.RunWithOptions(t.Context(), runner, RunOptions{LedgerPath: missing, Resume: true})
	require.Error(t, err)
	assert.Nil(t, results)
	assert.Contains(t, err.Error(), "resume ledger")
	assert.Equal(t, int32(0), calls.Load())
}

func TestPlan_RunWithOptions_ResumeRecoversRunningAttempt(t *testing.T) {
	t.Parallel()

	task := Task{ID: "child", Agent: "executor", Prompt: "child"}
	plan, err := NewPlan([]Task{task})
	require.NoError(t, err)

	started := time.Now().UTC().Add(-time.Minute)
	admissionID := "admission-before-crash"
	ledgerPath := filepath.Join(t.TempDir(), "async-ledger.json")
	data, err := json.MarshalIndent(Ledger{
		StartedAt: started,
		UpdatedAt: started,
		RunID:     "run-before-crash",
		Tasks:     []Task{task},
		Admissions: []Admission{{
			RecordedAt:  started,
			AdmissionID: admissionID,
			ChildID:     task.ID,
			ParentRunID: "run-before-crash",
			Admitted:    true,
			Attempt:     1,
		}},
		Attempts: []TaskAttempt{{
			StartedAt:   started,
			Wave:        0,
			Order:       0,
			Attempt:     1,
			Task:        task,
			Status:      StatusRunning,
			AdmissionID: admissionID,
			Usage:       Usage{PromptTokens: 1},
		}},
	}, "", "  ")
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(ledgerPath, append(data, '\n'), 0o600))

	var calls atomic.Int32
	results, err := plan.RunWithOptions(t.Context(), func(context.Context, Task) (string, error) {
		calls.Add(1)
		return recoveredOutput, nil
	}, RunOptions{LedgerPath: ledgerPath, Resume: true})

	require.NoError(t, err)
	require.Len(t, results, 1)
	assert.Equal(t, StatusSucceeded, results[0].Status)
	assert.False(t, results[0].Resumed)
	assert.Equal(t, recoveredOutput, results[0].Output)
	assert.Equal(t, int32(1), calls.Load())

	var ledger Ledger
	data, err = os.ReadFile(ledgerPath)
	require.NoError(t, err)
	require.NoError(t, json.Unmarshal(data, &ledger))
	require.Len(t, ledger.Attempts, 2)
	assert.Equal(t, StatusCanceled, ledger.Attempts[0].Status)
	assert.Contains(t, ledger.Attempts[0].Error, "recovered running attempt")
	assert.False(t, ledger.Attempts[0].FinishedAt.IsZero())
	require.Len(t, ledger.StopReceipts, 1)
	assert.Equal(t, ledger.Attempts[0].StopID, ledger.StopReceipts[0].StopID)
	assert.Equal(t, admissionID, ledger.StopReceipts[0].AdmissionID)
	assert.Equal(t, task.ID, ledger.StopReceipts[0].ChildID)
	assert.Equal(t, StatusCanceled, ledger.StopReceipts[0].Status)
	assert.Contains(t, ledger.StopReceipts[0].Reason, "recovered running attempt")
	assert.Equal(t, StatusSucceeded, ledger.Attempts[1].Status)
}

func TestPlan_RunWithOptions_TimeoutAndBudgetExhaustion(t *testing.T) {
	t.Parallel()

	t.Run("timeout", func(t *testing.T) {
		t.Parallel()

		plan, err := NewPlan([]Task{{ID: slowID, Agent: "executor", Prompt: slowID}})
		require.NoError(t, err)

		ledgerPath := filepath.Join(t.TempDir(), "async-ledger.json")
		results, err := plan.RunWithOptions(t.Context(), func(ctx context.Context, _ Task) (string, error) {
			<-ctx.Done()
			return "", ctx.Err()
		}, RunOptions{LedgerPath: ledgerPath, Timeout: 10 * time.Millisecond})

		require.Error(t, err)
		require.Len(t, results, 1)
		assert.Contains(t, err.Error(), `task "slow" timed out`)
		assert.Equal(t, StatusTimedOut, results[0].Status)
		assert.NotEmpty(t, results[0].AdmissionID)
		assert.NotEmpty(t, results[0].StopID)

		var ledger Ledger
		data, readErr := os.ReadFile(ledgerPath)
		require.NoError(t, readErr)
		require.NoError(t, json.Unmarshal(data, &ledger))
		require.Len(t, ledger.StopReceipts, 1)
		assert.Equal(t, results[0].AdmissionID, ledger.StopReceipts[0].AdmissionID)
		assert.Equal(t, results[0].StopID, ledger.StopReceipts[0].StopID)
		assert.Equal(t, slowID, ledger.StopReceipts[0].ChildID)
		assert.Equal(t, StatusTimedOut, ledger.StopReceipts[0].Status)
		assert.Equal(t, 0, ledger.StopReceipts[0].Wave)
		assert.Equal(t, 0, ledger.StopReceipts[0].Order)
	})

	t.Run("timeout even when runner returns success after deadline", func(t *testing.T) {
		t.Parallel()

		plan, err := NewPlan([]Task{{ID: slowID, Agent: "executor", Prompt: slowID}})
		require.NoError(t, err)

		results, err := plan.RunWithOptions(t.Context(), func(context.Context, Task) (string, error) {
			time.Sleep(20 * time.Millisecond)
			return lateSuccessOutput, nil
		}, RunOptions{Timeout: 5 * time.Millisecond})

		require.Error(t, err)
		require.Len(t, results, 1)
		assert.Equal(t, StatusTimedOut, results[0].Status)
		assert.Contains(t, results[0].Error, context.DeadlineExceeded.Error())
	})

	t.Run("timeout records deadline when runner returns custom error after deadline", func(t *testing.T) {
		t.Parallel()

		plan, err := NewPlan([]Task{{ID: slowID, Agent: "executor", Prompt: slowID}})
		require.NoError(t, err)

		ledgerPath := filepath.Join(t.TempDir(), "async-ledger.json")
		results, err := plan.RunWithOptions(t.Context(), func(context.Context, Task) (string, error) {
			time.Sleep(20 * time.Millisecond)
			return "", errors.New("late cleanup failed")
		}, RunOptions{LedgerPath: ledgerPath, Timeout: 5 * time.Millisecond})

		require.Error(t, err)
		require.Len(t, results, 1)
		assert.Equal(t, StatusTimedOut, results[0].Status)
		assert.Contains(t, results[0].Error, "late cleanup failed")
		assert.Contains(t, results[0].Error, context.DeadlineExceeded.Error())

		var ledger Ledger
		data, readErr := os.ReadFile(ledgerPath)
		require.NoError(t, readErr)
		require.NoError(t, json.Unmarshal(data, &ledger))
		require.Len(t, ledger.Attempts, 1)
		assert.Contains(t, ledger.Attempts[0].Error, "late cleanup failed")
		assert.Contains(t, ledger.Attempts[0].Error, context.DeadlineExceeded.Error())
		require.Len(t, ledger.StopReceipts, 1)
		assert.Contains(t, ledger.StopReceipts[0].Reason, "late cleanup failed")
		assert.Contains(t, ledger.StopReceipts[0].Reason, context.DeadlineExceeded.Error())
	})

	t.Run("budget", func(t *testing.T) {
		t.Parallel()

		plan, err := NewPlan([]Task{
			{ID: "first", Agent: "executor", Prompt: "first", EstimatedPromptTokens: 4},
			{ID: "second", Agent: "executor", Prompt: "second", EstimatedPromptTokens: 4},
		})
		require.NoError(t, err)

		var calls atomic.Int32
		results, err := plan.RunWithOptions(t.Context(), func(_ context.Context, task Task) (string, error) {
			calls.Add(1)
			return task.ID + "-done", nil
		}, RunOptions{MaxConcurrency: 1, Budget: Budget{MaxPromptTokens: 5}})

		require.Error(t, err)
		require.Len(t, results, 2)
		assert.Equal(t, StatusSucceeded, results[0].Status)
		assert.Equal(t, StatusBudgetExhausted, results[1].Status)
		assert.Equal(t, int32(1), calls.Load())
	})

	t.Run("output budget", func(t *testing.T) {
		t.Parallel()

		plan, err := NewPlan([]Task{
			{ID: "chatty", Agent: "executor", Prompt: "chatty"},
			{ID: "queued", Agent: "executor", Prompt: "queued"},
		})
		require.NoError(t, err)

		var calls atomic.Int32
		results, err := plan.RunWithOptions(t.Context(), func(context.Context, Task) (string, error) {
			calls.Add(1)
			return "too much output", nil
		}, RunOptions{MaxConcurrency: 1, Budget: Budget{MaxOutputBytes: 4}})

		require.Error(t, err)
		require.Len(t, results, 2)
		assert.Equal(t, StatusBudgetExhausted, results[0].Status)
		assert.Equal(t, StatusCanceled, results[1].Status)
		assert.Contains(t, results[0].Error, "output byte budget exceeded")
		assert.Equal(t, int32(1), calls.Load())
	})

	t.Run("output budget serializes accounting", func(t *testing.T) {
		t.Parallel()

		plan, err := NewPlan([]Task{
			{ID: "first", Agent: "executor", Prompt: "first"},
			{ID: "second", Agent: "executor", Prompt: "second"},
		})
		require.NoError(t, err)

		var current atomic.Int32
		var maxSeen atomic.Int32
		results, err := plan.RunWithOptions(t.Context(), func(context.Context, Task) (string, error) {
			active := current.Add(1)
			for {
				seen := maxSeen.Load()
				if active <= seen || maxSeen.CompareAndSwap(seen, active) {
					break
				}
			}

			time.Sleep(10 * time.Millisecond)
			current.Add(-1)

			return "ok", nil
		}, RunOptions{MaxConcurrency: 2, Budget: Budget{MaxOutputBytes: 10}})

		require.NoError(t, err)
		require.Len(t, results, 2)
		assert.LessOrEqual(t, maxSeen.Load(), int32(1))
	})

	t.Run("output budget is exposed to detailed runner", func(t *testing.T) {
		t.Parallel()

		plan, err := NewPlan([]Task{
			{ID: "first", Agent: "executor", Prompt: "first"},
			{ID: "second", Agent: "executor", Prompt: "second"},
		})
		require.NoError(t, err)

		var limitsMu sync.Mutex
		limits := make([]int64, 0, 2)
		results, err := plan.RunDetailedWithOptions(t.Context(), func(ctx context.Context, task Task) (TaskRunOutput, error) {
			limit := OutputByteLimit(ctx)
			limitsMu.Lock()
			limits = append(limits, limit)
			limitsMu.Unlock()

			switch task.ID {
			case "first":
				return TaskRunOutput{Stdout: "abcdef"}, nil
			default:
				return TaskRunOutput{Stdout: strings.Repeat("x", int(limit)), BudgetExhausted: true}, nil
			}
		}, RunOptions{MaxConcurrency: 2, Budget: Budget{MaxOutputBytes: 10}})

		require.Error(t, err)
		require.Len(t, results, 2)
		assert.Equal(t, StatusSucceeded, results[0].Status)
		assert.Equal(t, StatusBudgetExhausted, results[1].Status)
		assert.Contains(t, results[1].Error, "budget exhausted by runner")
		assert.Equal(t, []int64{10, 4}, limits)
	})

	t.Run("runner budget exhaustion cancels queued tasks", func(t *testing.T) {
		t.Parallel()

		plan, err := NewPlan([]Task{
			{ID: "chatty", Agent: "executor", Prompt: "chatty"},
			{ID: "queued", Agent: "executor", Prompt: "queued"},
		})
		require.NoError(t, err)

		ledgerPath := filepath.Join(t.TempDir(), "async-ledger.json")
		var calls atomic.Int32
		results, err := plan.RunDetailedWithOptions(t.Context(), func(context.Context, Task) (TaskRunOutput, error) {
			calls.Add(1)

			return TaskRunOutput{Stdout: "abcd", BudgetExhausted: true}, nil
		}, RunOptions{LedgerPath: ledgerPath, MaxConcurrency: 1})

		require.Error(t, err)
		require.Len(t, results, 2)
		assert.Equal(t, StatusBudgetExhausted, results[0].Status)
		assert.Equal(t, StatusCanceled, results[1].Status)
		assert.Equal(t, "abcd", results[0].Output)
		assert.Contains(t, results[0].Error, "budget exhausted by runner")
		assert.Equal(t, int32(1), calls.Load())

		var ledger Ledger
		data, readErr := os.ReadFile(ledgerPath)
		require.NoError(t, readErr)
		require.NoError(t, json.Unmarshal(data, &ledger))
		require.Len(t, ledger.Admissions, 2)
		admissions := map[string]Admission{}
		for _, admission := range ledger.Admissions {
			admissions[admission.ChildID] = admission
		}
		assert.True(t, admissions["chatty"].Admitted)
		assert.False(t, admissions["queued"].Admitted)
		assert.Contains(t, admissions["queued"].DenyReason, context.Canceled.Error())
		assert.Equal(t, admissions["queued"].AdmissionID, results[1].AdmissionID)
		require.Len(t, ledger.StopReceipts, 1)
		assert.Equal(t, results[0].AdmissionID, ledger.StopReceipts[0].AdmissionID)
		assert.Equal(t, results[0].StopID, ledger.StopReceipts[0].StopID)
		assert.Equal(t, "chatty", ledger.StopReceipts[0].ChildID)
		assert.Equal(t, StatusBudgetExhausted, ledger.StopReceipts[0].Status)
		assert.Contains(t, ledger.StopReceipts[0].Reason, "budget exhausted by runner")
		require.Len(t, ledger.Attempts, 1)
		assert.Equal(t, "chatty", ledger.Attempts[0].Task.ID)
	})

	t.Run("resume keeps exhausted output budget closed", func(t *testing.T) {
		t.Parallel()

		plan, err := NewPlan([]Task{{ID: "chatty", Agent: "executor", Prompt: "chatty"}})
		require.NoError(t, err)

		ledgerPath := filepath.Join(t.TempDir(), "async-ledger.json")
		var calls atomic.Int32
		results, err := plan.RunWithOptions(t.Context(), func(context.Context, Task) (string, error) {
			calls.Add(1)
			return "too much output", nil
		}, RunOptions{LedgerPath: ledgerPath, Budget: Budget{MaxOutputBytes: 4}})

		require.Error(t, err)
		require.Len(t, results, 1)
		assert.Equal(t, StatusBudgetExhausted, results[0].Status)
		assert.Equal(t, int32(1), calls.Load())

		calls.Store(0)
		resumed, err := plan.RunWithOptions(t.Context(), func(context.Context, Task) (string, error) {
			calls.Add(1)
			return shouldNotRunOutput, nil
		}, RunOptions{LedgerPath: ledgerPath, Resume: true, Budget: Budget{MaxOutputBytes: 4}})

		require.Error(t, err)
		require.Len(t, resumed, 1)
		assert.Equal(t, StatusBudgetExhausted, resumed[0].Status)
		assert.Contains(t, resumed[0].Error, "output byte budget exhausted")
		assert.Equal(t, int32(0), calls.Load())
	})

	t.Run("resume keeps exhausted token and cost budget closed", func(t *testing.T) {
		t.Parallel()

		plan, err := NewPlan([]Task{{ID: "actual", Agent: "executor", Prompt: "actual", EstimatedPromptTokens: 1, EstimatedCostMicros: 1}})
		require.NoError(t, err)

		ledgerPath := filepath.Join(t.TempDir(), "async-ledger.json")
		var calls atomic.Int32
		results, err := plan.RunDetailedWithOptions(t.Context(), func(context.Context, Task) (TaskRunOutput, error) {
			calls.Add(1)

			return TaskRunOutput{Stdout: "ok", PromptTokens: 10, EstimatedCostMicros: 10}, nil
		}, RunOptions{LedgerPath: ledgerPath, Budget: Budget{MaxPromptTokens: 5, MaxCostMicros: 5}})

		require.Error(t, err)
		require.Len(t, results, 1)
		assert.Equal(t, StatusBudgetExhausted, results[0].Status)
		assert.Equal(t, int32(1), calls.Load())

		calls.Store(0)
		resumed, err := plan.RunDetailedWithOptions(t.Context(), func(context.Context, Task) (TaskRunOutput, error) {
			calls.Add(1)

			return TaskRunOutput{Stdout: shouldNotRunOutput}, nil
		}, RunOptions{LedgerPath: ledgerPath, Resume: true, Budget: Budget{MaxPromptTokens: 5, MaxCostMicros: 5}})

		require.Error(t, err)
		require.Len(t, resumed, 1)
		assert.Equal(t, StatusBudgetExhausted, resumed[0].Status)
		assert.Contains(t, resumed[0].Error, "prompt token budget exceeded")
		assert.Contains(t, resumed[0].Error, "cost budget exceeded")
		assert.Equal(t, int32(0), calls.Load())
	})

	t.Run("actual token and cost budget", func(t *testing.T) {
		t.Parallel()

		plan, err := NewPlan([]Task{{ID: "actual", Agent: "executor", Prompt: "actual", EstimatedPromptTokens: 1, EstimatedCostMicros: 1}})
		require.NoError(t, err)

		results, err := plan.RunDetailedWithOptions(t.Context(), func(context.Context, Task) (TaskRunOutput, error) {
			return TaskRunOutput{Stdout: "ok", PromptTokens: 10, EstimatedCostMicros: 10}, nil
		}, RunOptions{Budget: Budget{MaxPromptTokens: 5, MaxCostMicros: 5}})

		require.Error(t, err)
		require.Len(t, results, 1)
		assert.Equal(t, StatusBudgetExhausted, results[0].Status)
		assert.Contains(t, results[0].Error, "prompt token budget exceeded")
		assert.Contains(t, results[0].Error, "cost budget exceeded")
		assert.Equal(t, 10, results[0].Usage.PromptTokens)
		assert.Equal(t, int64(10), results[0].Usage.EstimatedCostMicros)
	})
}

func TestPlan_RunWithOptions_PersistsAdmissionDenialBeforeSpawn(t *testing.T) {
	t.Parallel()

	plan, err := NewPlan([]Task{{ID: "expensive", Agent: "executor", Prompt: "expensive", EstimatedPromptTokens: 10}})
	require.NoError(t, err)

	ledgerPath := filepath.Join(t.TempDir(), "async-ledger.json")
	var called atomic.Bool
	results, err := plan.RunWithOptions(t.Context(), func(context.Context, Task) (string, error) {
		called.Store(true)

		return shouldNotRunOutput, nil
	}, RunOptions{
		LedgerPath:        ledgerPath,
		WorkspaceID:       "parent-session",
		AllowedWriteScope: t.TempDir(),
		Model:             "codex/gpt-test",
		Provider:          "codex",
		Timeout:           time.Second,
		RetryPolicy:       RetryPolicy{MaxAttempts: 3},
		Budget:            Budget{MaxPromptTokens: 5},
	})

	require.Error(t, err)
	require.Len(t, results, 1)
	assert.Equal(t, StatusBudgetExhausted, results[0].Status)
	assert.Contains(t, err.Error(), `task "expensive" budget exhausted`)
	assert.False(t, called.Load())

	var ledger Ledger
	data, readErr := os.ReadFile(ledgerPath)
	require.NoError(t, readErr)
	require.NoError(t, json.Unmarshal(data, &ledger))
	require.Len(t, ledger.Admissions, 1)
	admission := ledger.Admissions[0]
	assert.NotEmpty(t, admission.AdmissionID)
	assert.Equal(t, "expensive", admission.ChildID)
	assert.Equal(t, ledger.RunID, admission.ParentRunID)
	assert.Equal(t, "parent-session/expensive", admission.WorkspaceID)
	assert.Equal(t, "codex/gpt-test", admission.Model)
	assert.Equal(t, "codex", admission.Provider)
	assert.Equal(t, time.Second, admission.Timeout)
	assert.Equal(t, Budget{MaxPromptTokens: 5}, admission.Budget)
	assert.Equal(t, RetryPolicy{MaxAttempts: 3}, admission.RetryPolicy)
	assert.False(t, admission.Admitted)
	assert.Contains(t, admission.DenyReason, "prompt token budget exceeded")
	assert.Equal(t, admission.AdmissionID, results[0].AdmissionID)
	require.Len(t, ledger.Results, 1)
	assert.Equal(t, admission.AdmissionID, ledger.Results[0].AdmissionID)
	assert.Empty(t, ledger.Attempts)
}

func TestPlan_RunWithOptions_PersistsAdmissionBeforeRunner(t *testing.T) {
	t.Parallel()

	plan, err := NewPlan([]Task{{ID: "preflight", Agent: "executor", Prompt: "preflight"}})
	require.NoError(t, err)

	ledgerPath := filepath.Join(t.TempDir(), "async-ledger.json")
	results, err := plan.RunWithOptions(t.Context(), func(context.Context, Task) (string, error) {
		data, readErr := os.ReadFile(ledgerPath)
		if readErr != nil {
			return "", fmt.Errorf("read ledger before runner body: %w", readErr)
		}

		var ledger Ledger
		if unmarshalErr := json.Unmarshal(data, &ledger); unmarshalErr != nil {
			return "", fmt.Errorf("unmarshal ledger before runner body: %w", unmarshalErr)
		}

		if len(ledger.Admissions) != 1 {
			return "", fmt.Errorf("expected one admission before runner body, got %d", len(ledger.Admissions))
		}

		admission := ledger.Admissions[0]
		if !admission.Admitted || admission.AdmissionID == "" || admission.ChildID != "preflight" || admission.ParentRunID == "" {
			return "", fmt.Errorf("unexpected admission before runner body: %+v", admission)
		}

		if len(ledger.Attempts) != 1 {
			return "", fmt.Errorf("expected one running attempt before runner body, got %d", len(ledger.Attempts))
		}

		attempt := ledger.Attempts[0]
		if attempt.Status != StatusRunning || attempt.AdmissionID != admission.AdmissionID {
			return "", fmt.Errorf("unexpected attempt before runner body: %+v", attempt)
		}

		return doneOutput, nil
	}, RunOptions{
		LedgerPath:        ledgerPath,
		WorkspaceID:       "parent-run",
		AllowedWriteScope: t.TempDir(),
		Timeout:           time.Second,
		Budget:            Budget{MaxPromptTokens: 10},
	})

	require.NoError(t, err)
	require.Len(t, results, 1)
	assert.Equal(t, StatusSucceeded, results[0].Status)
	assert.NotEmpty(t, results[0].AdmissionID)
}

func TestPlan_RunWithOptions_DeniesScopeViolationBeforeSpawn(t *testing.T) {
	t.Parallel()

	parentScope := t.TempDir()
	childScope := filepath.Join(parentScope, "..", "outside-scope")
	plan, err := NewPlan([]Task{{
		ID:                "escape",
		Agent:             "executor",
		Prompt:            "escape",
		AllowedWriteScope: childScope,
	}})
	require.NoError(t, err)

	ledgerPath := filepath.Join(t.TempDir(), "async-ledger.json")
	var called atomic.Bool
	results, err := plan.RunWithOptions(t.Context(), func(context.Context, Task) (string, error) {
		called.Store(true)

		return shouldNotRunOutput, nil
	}, RunOptions{LedgerPath: ledgerPath, AllowedWriteScope: parentScope})

	require.Error(t, err)
	require.Len(t, results, 1)
	assert.Equal(t, StatusDenied, results[0].Status)
	assert.Contains(t, err.Error(), `task "escape" denied`)
	assert.False(t, called.Load())

	var ledger Ledger
	data, readErr := os.ReadFile(ledgerPath)
	require.NoError(t, readErr)
	require.NoError(t, json.Unmarshal(data, &ledger))
	require.Len(t, ledger.Admissions, 1)
	admission := ledger.Admissions[0]
	assert.False(t, admission.Admitted)
	assert.Equal(t, childScope, admission.AllowedWriteScope)
	assert.Contains(t, admission.DenyReason, "escapes parent scope")
	assert.Equal(t, admission.AdmissionID, results[0].AdmissionID)
	require.Len(t, ledger.Results, 1)
	assert.Equal(t, StatusDenied, ledger.Results[0].Status)
	assert.Equal(t, admission.AdmissionID, ledger.Results[0].AdmissionID)
	assert.Empty(t, ledger.Attempts)
}

func TestPlan_RunWithOptions_DeniesSymlinkScopeEscapeBeforeSpawn(t *testing.T) {
	t.Parallel()

	parentScope := t.TempDir()
	outsideScope := t.TempDir()
	childScope := filepath.Join(parentScope, "linked-outside")
	requireSymlinkOrSkip(t, outsideScope, childScope)
	plan, err := NewPlan([]Task{{
		ID:                "escape",
		Agent:             "executor",
		Prompt:            "escape",
		AllowedWriteScope: childScope,
	}})
	require.NoError(t, err)

	ledgerPath := filepath.Join(t.TempDir(), "async-ledger.json")
	var called atomic.Bool
	results, err := plan.RunWithOptions(t.Context(), func(context.Context, Task) (string, error) {
		called.Store(true)

		return shouldNotRunOutput, nil
	}, RunOptions{LedgerPath: ledgerPath, AllowedWriteScope: parentScope})

	require.Error(t, err)
	require.Len(t, results, 1)
	assert.Equal(t, StatusDenied, results[0].Status)
	assert.Contains(t, err.Error(), `task "escape" denied`)
	assert.False(t, called.Load())

	var ledger Ledger
	data, readErr := os.ReadFile(ledgerPath)
	require.NoError(t, readErr)
	require.NoError(t, json.Unmarshal(data, &ledger))
	require.Len(t, ledger.Admissions, 1)
	admission := ledger.Admissions[0]
	assert.False(t, admission.Admitted)
	assert.Equal(t, childScope, admission.AllowedWriteScope)
	assert.Contains(t, admission.DenyReason, "escapes parent scope")
	assert.Equal(t, admission.AdmissionID, results[0].AdmissionID)
	assert.Empty(t, ledger.Attempts)
}

func TestPlan_RunWithOptions_PersistsAdmissionForHaltedSibling(t *testing.T) {
	t.Parallel()

	plan, err := NewPlan([]Task{
		{ID: failingID, Agent: "executor", Prompt: failingID},
		{ID: slowID, Agent: "executor", Prompt: slowID},
	})
	require.NoError(t, err)

	ledgerPath := filepath.Join(t.TempDir(), "async-ledger.json")
	slowStarted := make(chan struct{})
	releaseFailure := make(chan struct{})

	results, err := plan.RunWithOptions(t.Context(), func(ctx context.Context, task Task) (string, error) {
		switch task.ID {
		case failingID:
			<-slowStarted
			close(releaseFailure)

			return partialOutput, errors.New("boom")
		case slowID:
			close(slowStarted)
			<-releaseFailure
			<-ctx.Done()

			return "", ctx.Err()
		default:
			return "", nil
		}
	}, RunOptions{LedgerPath: ledgerPath, MaxConcurrency: 2, CancelOnFailure: true})

	require.Error(t, err)
	require.Len(t, results, 2)
	assert.Equal(t, StatusFailed, results[0].Status)
	assert.Equal(t, StatusCanceled, results[1].Status)

	var ledger Ledger
	data, readErr := os.ReadFile(ledgerPath)
	require.NoError(t, readErr)
	require.NoError(t, json.Unmarshal(data, &ledger))
	require.Len(t, ledger.Admissions, 2)
	admissions := map[string]Admission{}
	for _, admission := range ledger.Admissions {
		admissions[admission.ChildID] = admission
	}
	require.Contains(t, admissions, failingID)
	require.Contains(t, admissions, slowID)
	assert.NotEmpty(t, admissions[failingID].AdmissionID)
	assert.NotEmpty(t, admissions[slowID].AdmissionID)
	assert.NotEqual(t, admissions[failingID].AdmissionID, admissions[slowID].AdmissionID)
	assert.True(t, admissions[failingID].Admitted)
	assert.True(t, admissions[slowID].Admitted)
	assert.Empty(t, admissions[slowID].DenyReason)
	assert.Equal(t, ledger.RunID, admissions[slowID].ParentRunID)
	resultStatuses := map[string]string{}
	resultAdmissions := map[string]string{}
	resultStops := map[string]string{}
	resultErrors := map[string]string{}
	for _, result := range ledger.Results {
		resultStatuses[result.Task.ID] = result.Status
		resultAdmissions[result.Task.ID] = result.AdmissionID
		resultStops[result.Task.ID] = result.StopID
		resultErrors[result.Task.ID] = result.Error
	}
	attemptStatuses := map[string]string{}
	attemptAdmissions := map[string]string{}
	attemptStops := map[string]string{}
	attemptErrors := map[string]string{}
	for _, attempt := range ledger.Attempts {
		attemptStatuses[attempt.Task.ID] = attempt.Status
		attemptAdmissions[attempt.Task.ID] = attempt.AdmissionID
		attemptStops[attempt.Task.ID] = attempt.StopID
		attemptErrors[attempt.Task.ID] = attempt.Error
	}
	require.Len(t, ledger.StopReceipts, 1)
	stopReceipt := ledger.StopReceipts[0]
	assert.NotEmpty(t, stopReceipt.StopID)
	assert.Equal(t, slowID, stopReceipt.ChildID)
	assert.Equal(t, StatusCanceled, stopReceipt.Status)
	assert.Contains(t, stopReceipt.Reason, `sibling cancellation after task "fail" failed`)
	assert.Equal(t, admissions[slowID].AdmissionID, stopReceipt.AdmissionID)
	assert.Equal(t, StatusCanceled, resultStatuses[slowID])
	assert.Equal(t, StatusCanceled, attemptStatuses[slowID])
	assert.Contains(t, resultErrors[slowID], `sibling cancellation after task "fail" failed`)
	assert.Contains(t, attemptErrors[slowID], `sibling cancellation after task "fail" failed`)
	assert.Equal(t, admissions[slowID].AdmissionID, resultAdmissions[slowID])
	assert.Equal(t, admissions[slowID].AdmissionID, attemptAdmissions[slowID])
	assert.Equal(t, stopReceipt.StopID, resultStops[slowID])
	assert.Equal(t, stopReceipt.StopID, attemptStops[slowID])
}

func TestPlan_RunWithOptions_PreCanceledContextPersistsAdmissionDenials(t *testing.T) {
	t.Parallel()

	plan, err := NewPlan([]Task{
		{ID: "plan", Agent: "planner", Prompt: "plan"},
		{ID: "build", Agent: "executor", Prompt: "build", DependsOn: []string{"plan"}},
	})
	require.NoError(t, err)

	ctx, cancel := context.WithCancel(t.Context())
	cancel()

	ledgerPath := filepath.Join(t.TempDir(), "async-ledger.json")
	var ran atomic.Bool
	results, err := plan.RunWithOptions(ctx, func(context.Context, Task) (string, error) {
		ran.Store(true)

		return shouldNotRunOutput, nil
	}, RunOptions{LedgerPath: ledgerPath})

	require.Error(t, err)
	assert.Contains(t, err.Error(), "context canceled before wave 0")
	assert.False(t, ran.Load())
	require.Len(t, results, 2)
	assert.Equal(t, StatusCanceled, results[0].Status)
	assert.Equal(t, StatusCanceled, results[1].Status)
	assert.NotEmpty(t, results[0].AdmissionID)
	assert.NotEmpty(t, results[1].AdmissionID)

	var ledger Ledger
	data, readErr := os.ReadFile(ledgerPath)
	require.NoError(t, readErr)
	require.NoError(t, json.Unmarshal(data, &ledger))
	require.Len(t, ledger.Admissions, 2)
	require.Len(t, ledger.Results, 2)
	assert.Empty(t, ledger.Attempts)
	for _, admission := range ledger.Admissions {
		assert.False(t, admission.Admitted)
		assert.Contains(t, admission.DenyReason, context.Canceled.Error())
		assert.Equal(t, ledger.RunID, admission.ParentRunID)
	}
}

func requireSymlinkOrSkip(t *testing.T, target, link string) {
	t.Helper()

	if err := os.Symlink(target, link); err != nil {
		t.Skipf("symlink not available in this environment: %v", err)
	}
}

func TestPlan_RunDetailedWithOptions_PersistsTranscriptAndExitStatus(t *testing.T) {
	t.Parallel()

	plan, err := NewPlan([]Task{{ID: "child", Agent: "executor", Prompt: "run child"}})
	require.NoError(t, err)

	ledgerPath := filepath.Join(t.TempDir(), "async-ledger.json")
	results, err := plan.RunDetailedWithOptions(t.Context(), func(_ context.Context, task Task) (TaskRunOutput, error) {
		return TaskRunOutput{
			Stdout:     task.ID + " stdout",
			Stderr:     task.ID + " stderr",
			ExitStatus: 7,
			Artifacts:  []string{"artifact.txt"},
		}, nil
	}, RunOptions{LedgerPath: ledgerPath})

	require.NoError(t, err)
	require.Len(t, results, 1)
	assert.Equal(t, StatusSucceeded, results[0].Status)
	assert.Equal(t, "child stdout", results[0].Output)
	assert.Equal(t, "child stderr", results[0].Stderr)
	assert.Equal(t, 7, results[0].ExitStatus)
	assert.Equal(t, []string{"artifact.txt"}, results[0].Artifacts)
	assert.NotEmpty(t, results[0].TranscriptPath)

	transcript, err := os.ReadFile(results[0].TranscriptPath)
	require.NoError(t, err)
	assert.Contains(t, string(transcript), "# stdout")
	assert.Contains(t, string(transcript), "child stdout")
	assert.Contains(t, string(transcript), "# stderr")
	assert.Contains(t, string(transcript), "child stderr")

	var ledger Ledger
	data, err := os.ReadFile(ledgerPath)
	require.NoError(t, err)
	require.NoError(t, json.Unmarshal(data, &ledger))
	require.Len(t, ledger.Attempts, 1)
	assert.Equal(t, "child stderr", ledger.Attempts[0].Stderr)
	assert.Equal(t, 7, ledger.Attempts[0].ExitStatus)
	assert.Equal(t, results[0].TranscriptPath, ledger.Attempts[0].TranscriptPath)
}
