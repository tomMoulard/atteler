package tasklist

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestStore_AddListAndPersistTasksDeterministically(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "tasks", "todo.json")
	store := NewStore(path)
	clock := newTestClock(time.Date(2026, 5, 5, 12, 0, 0, 0, time.UTC))
	store.now = clock.now

	trimmedKey := " priority "
	second, err := store.Add(ctx, AddRequest{ID: "b", Title: " second task ", Metadata: map[string]string{trimmedKey: "high", "": "ignored"}})
	require.NoError(t, err)

	firstTime := second.CreatedAt

	clock.advanceMinute()

	first, err := store.Add(ctx, AddRequest{ID: "a", Title: "first task", Agent: "agent-1"})
	require.NoError(t, err)

	assert.Equal(t, "second task", second.Title)
	assert.Equal(t, map[string]string{"priority": "high"}, second.Metadata)
	assert.Equal(t, StatusReady, second.Status)
	assert.Equal(t, StatusInProgress, first.Status)
	assert.Equal(t, "agent-1", first.Agent)

	listed, err := store.List(ctx)
	require.NoError(t, err)
	require.Len(t, listed, 2)
	assert.Equal(t, []string{"b", "a"}, []string{listed[0].ID, listed[1].ID})
	assert.Equal(t, firstTime, listed[0].CreatedAt)

	reloaded := NewStore(path)
	loaded, err := reloaded.List(ctx)
	require.NoError(t, err)
	assert.Equal(t, listed, loaded)

	var persisted State

	data, err := os.ReadFile(path)
	require.NoError(t, err)
	require.NoError(t, json.Unmarshal(data, &persisted))
	assert.Len(t, persisted.Tasks, 2)
	assert.Len(t, persisted.History, 2)
	assert.Equal(t, []HistoryAction{HistoryAdded, HistoryAdded}, []HistoryAction{persisted.History[0].Action, persisted.History[1].Action})
}

func TestStore_AssignUpdateAndCompleteRecordHistory(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	store := NewStore(filepath.Join(t.TempDir(), "tasks.json"))
	clock := newTestClock(time.Date(2026, 5, 5, 13, 0, 0, 0, time.UTC))
	store.now = clock.now

	task, err := store.Add(ctx, AddRequest{ID: "todo-1", Title: "draft SDK", Message: "initial package task"})
	require.NoError(t, err)

	clock.advanceMinute()

	assigned, err := store.Assign(ctx, task.ID, "agent-a")
	require.NoError(t, err)
	assert.Equal(t, StatusInProgress, assigned.Status)
	assert.Equal(t, "agent-a", assigned.Agent)
	require.NotNil(t, assigned.Lease)
	assert.Equal(t, "agent-a", assigned.Lease.Owner)
	assert.Equal(t, assigned.UpdatedAt.Add(DefaultLeaseDuration), assigned.Lease.ExpiresAt)
	assert.Equal(t, 1, assigned.AttemptCount)
	assert.Greater(t, assigned.Revision, task.Revision)
	assert.True(t, assigned.UpdatedAt.After(assigned.CreatedAt))

	clock.advanceMinute()

	updated, err := store.Update(ctx, task.ID, UpdateRequest{
		Title:    "draft package SDK",
		Metadata: map[string]string{"scope": "pkg/tasklist"},
		Message:  "clarified scope",
	})
	require.NoError(t, err)
	assert.Equal(t, "draft package SDK", updated.Title)
	assert.Equal(t, map[string]string{"scope": "pkg/tasklist"}, updated.Metadata)
	assert.Nil(t, updated.CompletedAt)

	clock.advanceMinute()

	completed, err := store.Complete(ctx, task.ID, "agent-a")
	require.NoError(t, err)
	assert.Equal(t, StatusCompleted, completed.Status)
	assert.Equal(t, "agent-a", completed.Agent)
	require.NotNil(t, completed.CompletedAt)
	assert.Equal(t, completed.UpdatedAt, *completed.CompletedAt)

	history, err := store.History(ctx)
	require.NoError(t, err)
	require.Len(t, history, 4)
	assert.Equal(t, []HistoryAction{HistoryAdded, HistoryAssigned, HistoryUpdated, HistoryCompleted}, historyActions(history))
	assert.Equal(t, []int64{1, 2, 3, 4}, historySeqs(history))
	assert.Equal(t, []int64{1, 2, 3, 4}, historyStateRevisions(history))
	assert.Equal(t, "initial package task", history[0].Message)
	assert.Equal(t, "agent-a", history[1].Actor)
	require.NotNil(t, history[2].Before)
	require.NotNil(t, history[2].After)
	assert.Equal(t, "draft SDK", history[2].Before.Title)
	assert.Equal(t, "draft package SDK", history[2].After.Title)
	assert.Equal(t, "clarified scope", history[2].Message)
	assert.Equal(t, "agent-a", history[3].Agent)
}

func TestStore_ConcurrentStoresSerializeFileMutations(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "tasks.json")
	seed := NewStore(path)
	seed.now = newTestClock(time.Date(2026, 5, 6, 9, 0, 0, 0, time.UTC)).now

	task, err := seed.Add(ctx, AddRequest{ID: "shared", Title: "initial"})
	require.NoError(t, err)

	start := make(chan struct{})
	errCh := make(chan error, 2)

	var wg sync.WaitGroup

	wg.Add(2)

	go func() {
		defer wg.Done()

		<-start

		_, updateErr := NewStore(path).Update(ctx, task.ID, UpdateRequest{Title: "renamed by first store"})
		errCh <- updateErr
	}()

	go func() {
		defer wg.Done()

		<-start

		_, updateErr := NewStore(path).Update(ctx, task.ID, UpdateRequest{Metadata: map[string]string{"owner": "second-store"}})
		errCh <- updateErr
	}()

	close(start)
	wg.Wait()
	close(errCh)

	for updateErr := range errCh {
		require.NoError(t, updateErr)
	}

	loaded, err := NewStore(path).List(ctx)
	require.NoError(t, err)
	require.Len(t, loaded, 1)
	assert.Equal(t, "renamed by first store", loaded[0].Title)
	assert.Equal(t, map[string]string{"owner": "second-store"}, loaded[0].Metadata)

	state, err := NewStore(path).Load(ctx)
	require.NoError(t, err)
	assert.Equal(t, int64(3), state.Revision)
	assert.Len(t, state.History, 3)
}

func TestStore_ConcurrentStoresDoNotDropAddedTasks(t *testing.T) {
	t.Parallel()

	const writers = 24

	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "tasks.json")

	errCh := make(chan error, writers)

	var wg sync.WaitGroup

	for i := range writers {
		wg.Add(1)

		go func(i int) {
			defer wg.Done()

			_, err := NewStore(path).Add(ctx, AddRequest{
				ID:    fmt.Sprintf("task-%02d", i),
				Title: fmt.Sprintf("task %02d", i),
			})
			errCh <- err
		}(i)
	}

	wg.Wait()
	close(errCh)

	for err := range errCh {
		require.NoError(t, err)
	}

	state, err := NewStore(path).Load(ctx)
	require.NoError(t, err)
	assert.Len(t, state.Tasks, writers)
	assert.Len(t, state.History, writers)
	assert.Equal(t, int64(writers), state.Revision)
}

func TestStore_SubprocessWritersDoNotDropAddedTasks(t *testing.T) {
	t.Parallel()

	const writers = 8

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	path := filepath.Join(t.TempDir(), "tasks.json")

	exe, err := os.Executable()
	require.NoError(t, err)

	cmds := make([]*exec.Cmd, 0, writers)
	outputs := make([]*bytes.Buffer, 0, writers)

	for i := range writers {
		cmd := exec.CommandContext(ctx, exe, "-test.run", "^TestStore_SubprocessAddHelper$", "-test.v")

		cmd.Env = append(os.Environ(),
			"TASKLIST_SUBPROCESS_HELPER=1",
			"TASKLIST_SUBPROCESS_PATH="+path,
			fmt.Sprintf("TASKLIST_SUBPROCESS_ID=child-%02d", i),
			fmt.Sprintf("TASKLIST_SUBPROCESS_TITLE=child task %02d", i),
		)

		output := &bytes.Buffer{}

		cmd.Stdout = output
		cmd.Stderr = output
		require.NoError(t, cmd.Start())

		cmds = append(cmds, cmd)
		outputs = append(outputs, output)
	}

	for i, cmd := range cmds {
		require.NoErrorf(t, cmd.Wait(), "child %d output:\n%s", i, outputs[i].String())
	}

	state, err := NewStore(path).Load(ctx)
	require.NoError(t, err)
	assert.Len(t, state.Tasks, writers)
	assert.Len(t, state.History, writers)
	assert.Equal(t, int64(writers), state.Revision)
}

func TestStore_SubprocessAddHelper(t *testing.T) {
	t.Parallel()

	if os.Getenv("TASKLIST_SUBPROCESS_HELPER") != "1" {
		t.Skip("helper process only")
	}

	path := os.Getenv("TASKLIST_SUBPROCESS_PATH")
	id := os.Getenv("TASKLIST_SUBPROCESS_ID")
	title := os.Getenv("TASKLIST_SUBPROCESS_TITLE")

	_, err := NewStore(path).Add(context.Background(), AddRequest{
		ID:    id,
		Title: title,
		Actor: "subprocess-test",
	})
	require.NoError(t, err)
}

func TestStore_ConcurrentStoresClaimAndCompleteSeparateTasks(t *testing.T) {
	t.Parallel()

	const workers = 12

	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "tasks.json")
	seed := NewStore(path)

	for i := range workers {
		_, err := seed.Add(ctx, AddRequest{
			ID:    fmt.Sprintf("work-%02d", i),
			Title: fmt.Sprintf("work %02d", i),
			Actor: "planner",
		})
		require.NoError(t, err)
	}

	start := make(chan struct{})
	errCh := make(chan error, workers)

	var wg sync.WaitGroup

	for i := range workers {
		wg.Add(1)

		go func(i int) {
			defer wg.Done()

			<-start

			store := NewStore(path)
			id := fmt.Sprintf("work-%02d", i)
			agent := fmt.Sprintf("agent-%02d", i)

			claimed, err := store.Claim(ctx, id, AssignRequest{Agent: agent, Actor: agent})
			if err != nil {
				errCh <- err
				return
			}

			_, err = store.CompleteWithOptions(ctx, id, CompleteRequest{
				Agent:            agent,
				Actor:            agent,
				Message:          "done",
				ExpectedRevision: claimed.Revision,
			})
			errCh <- err
		}(i)
	}

	close(start)
	wg.Wait()
	close(errCh)

	for err := range errCh {
		require.NoError(t, err)
	}

	state, err := NewStore(path).Load(ctx)
	require.NoError(t, err)
	require.Len(t, state.Tasks, workers)
	assert.Len(t, state.History, workers*3)

	for i := range state.Tasks {
		assert.Equal(t, StatusCompleted, state.Tasks[i].Status)
		assert.Nil(t, state.Tasks[i].Lease)
		assert.NotNil(t, state.Tasks[i].CompletedAt)
	}
}

func TestStore_ConcurrentClaimsAllowOnlyOneLiveOwner(t *testing.T) {
	t.Parallel()

	const contenders = 8

	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "tasks.json")
	_, err := NewStore(path).Add(ctx, AddRequest{ID: "shared", Title: "shared work"})
	require.NoError(t, err)

	start := make(chan struct{})
	results := make(chan error, contenders)

	var wg sync.WaitGroup

	for i := range contenders {
		wg.Add(1)

		go func(i int) {
			defer wg.Done()

			<-start

			_, claimErr := NewStore(path).Claim(ctx, "shared", AssignRequest{
				Agent:         fmt.Sprintf("agent-%02d", i),
				SessionID:     fmt.Sprintf("session-%02d", i),
				LeaseDuration: time.Hour,
			})
			results <- claimErr
		}(i)
	}

	close(start)
	wg.Wait()
	close(results)

	successes := 0
	leased := 0

	for err := range results {
		switch {
		case err == nil:
			successes++
		case errors.Is(err, ErrTaskLeased):
			leased++
		default:
			require.NoError(t, err)
		}
	}

	assert.Equal(t, 1, successes)
	assert.Equal(t, contenders-1, leased)

	state, err := NewStore(path).Load(ctx)
	require.NoError(t, err)
	require.Len(t, state.Tasks, 1)
	assert.Equal(t, StatusInProgress, state.Tasks[0].Status)
	assert.NotNil(t, state.Tasks[0].Lease)
	assert.Len(t, state.History, 2)
}

func TestStore_ClaimHeartbeatAndReconcileLeases(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "tasks.json")
	clock := newTestClock(time.Date(2026, 5, 6, 10, 0, 0, 0, time.UTC))
	store := NewStore(path)
	store.now = clock.now

	task, err := store.Add(ctx, AddRequest{ID: "lease", Title: "lease-aware task"})
	require.NoError(t, err)

	claimed, err := store.Claim(ctx, task.ID, AssignRequest{
		Agent:         "agent-a",
		Actor:         "session-a",
		SessionID:     "session-1",
		RunID:         "run-1",
		LeaseDuration: time.Minute,
	})
	require.NoError(t, err)
	assert.Equal(t, StatusInProgress, claimed.Status)
	assert.Equal(t, "agent-a", claimed.Agent)
	require.NotNil(t, claimed.Lease)
	assert.Equal(t, "agent-a", claimed.Lease.Owner)
	assert.Equal(t, "session-1", claimed.Lease.SessionID)
	assert.Equal(t, "run-1", claimed.Lease.RunID)
	assert.Equal(t, clock.current.Add(time.Minute), claimed.Lease.ExpiresAt)
	assert.Equal(t, 1, claimed.AttemptCount)

	otherStore := NewStore(path)
	otherStore.now = clock.now
	_, err = otherStore.Claim(ctx, task.ID, AssignRequest{Agent: "agent-b", LeaseDuration: time.Minute})
	require.ErrorIs(t, err, ErrTaskLeased)

	clock.advance(30 * time.Second)

	renewed, err := store.Heartbeat(ctx, task.ID, HeartbeatRequest{
		Agent:         "agent-a",
		Actor:         "session-a",
		SessionID:     "session-1",
		RunID:         "run-1",
		LeaseDuration: 2 * time.Minute,
	})
	require.NoError(t, err)
	require.NotNil(t, renewed.Lease)
	assert.Equal(t, clock.current.Add(2*time.Minute), renewed.Lease.ExpiresAt)
	assert.Equal(t, clock.current, renewed.Lease.LastHeartbeatAt)

	clock.advance(3 * time.Minute)

	_, err = store.Heartbeat(ctx, task.ID, HeartbeatRequest{Agent: "agent-a", SessionID: "session-1", RunID: "run-1"})
	require.ErrorIs(t, err, ErrTaskLeaseExpired)

	result, err := store.Reconcile(ctx, ReconcileRequest{Actor: "scheduler", Message: "expire stale leases"})
	require.NoError(t, err)
	assert.Equal(t, 1, result.ExpiredLeases)
	assert.Equal(t, 1, result.HistoryEntries)
	require.Len(t, result.Tasks, 1)
	assert.Equal(t, StatusReady, result.Tasks[0].Status)
	assert.Nil(t, result.Tasks[0].Lease)

	reclaimed, err := otherStore.Claim(ctx, task.ID, AssignRequest{Agent: "agent-b", LeaseDuration: time.Minute})
	require.NoError(t, err)
	assert.Equal(t, "agent-b", reclaimed.Agent)
	assert.Equal(t, 2, reclaimed.AttemptCount)
}

func TestStore_LeaseScopeMustMatchForHeartbeatOrClaim(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "tasks.json")
	clock := newTestClock(time.Date(2026, 5, 6, 10, 30, 0, 0, time.UTC))
	store := NewStore(path)
	store.now = clock.now

	task, err := store.Add(ctx, AddRequest{ID: "scoped", Title: "scoped lease"})
	require.NoError(t, err)

	claimed, err := store.Claim(ctx, task.ID, AssignRequest{
		Agent:         "agent-a",
		SessionID:     "session-1",
		RunID:         "run-1",
		LeaseDuration: time.Minute,
	})
	require.NoError(t, err)
	assert.Equal(t, 1, claimed.AttemptCount)

	_, err = store.Heartbeat(ctx, task.ID, HeartbeatRequest{Agent: "agent-a"})
	require.ErrorIs(t, err, ErrTaskLeased)

	_, err = store.Heartbeat(ctx, task.ID, HeartbeatRequest{
		Agent:     "agent-a",
		SessionID: "session-1",
		RunID:     "wrong-run",
	})
	require.ErrorIs(t, err, ErrTaskLeased)

	_, err = store.Claim(ctx, task.ID, AssignRequest{Agent: "agent-a"})
	require.ErrorIs(t, err, ErrTaskLeased)

	renewed, err := store.Claim(ctx, task.ID, AssignRequest{
		Agent:         "agent-a",
		SessionID:     "session-1",
		RunID:         "run-1",
		LeaseDuration: 2 * time.Minute,
	})
	require.NoError(t, err)
	assert.Equal(t, 1, renewed.AttemptCount)
	require.NotNil(t, renewed.Lease)
	assert.Equal(t, clock.current.Add(2*time.Minute), renewed.Lease.ExpiresAt)

	clock.advance(30 * time.Second)

	heartbeat, err := store.Heartbeat(ctx, task.ID, HeartbeatRequest{
		Agent:         "agent-a",
		SessionID:     "session-1",
		RunID:         "run-1",
		LeaseDuration: time.Minute,
	})
	require.NoError(t, err)
	require.NotNil(t, heartbeat.Lease)
	assert.Equal(t, clock.current.Add(time.Minute), heartbeat.Lease.ExpiresAt)

	adminReassigned, err := store.AssignWithLease(ctx, task.ID, AssignRequest{
		Agent:         "agent-a",
		SessionID:     "session-2",
		RunID:         "run-2",
		LeaseDuration: time.Minute,
	})
	require.NoError(t, err)
	assert.Equal(t, 2, adminReassigned.AttemptCount)
	require.NotNil(t, adminReassigned.Lease)
	assert.Equal(t, "session-2", adminReassigned.Lease.SessionID)
	assert.Equal(t, "run-2", adminReassigned.Lease.RunID)
}

func TestStore_CompleteRequiresLiveLeaseOwner(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "tasks.json")
	clock := newTestClock(time.Date(2026, 5, 6, 10, 40, 0, 0, time.UTC))
	store := NewStore(path)
	store.now = clock.now

	task, err := store.Add(ctx, AddRequest{ID: "owned", Title: "owned task"})
	require.NoError(t, err)

	claimed, err := store.Claim(ctx, task.ID, AssignRequest{
		Agent:         "agent-a",
		SessionID:     "session-a",
		RunID:         "run-a",
		LeaseDuration: time.Minute,
	})
	require.NoError(t, err)
	require.NotNil(t, claimed.Lease)

	_, err = store.CompleteWithOptions(ctx, task.ID, CompleteRequest{
		Agent:     "agent-b",
		SessionID: "session-b",
		RunID:     "run-b",
	})
	require.ErrorIs(t, err, ErrTaskLeased)

	_, err = store.CompleteWithOptions(ctx, task.ID, CompleteRequest{Agent: "agent-a"})
	require.ErrorIs(t, err, ErrTaskLeased)

	completed, err := store.CompleteWithOptions(ctx, task.ID, CompleteRequest{
		Agent:     "agent-a",
		SessionID: "session-a",
		RunID:     "run-a",
		Message:   "owner finished",
	})
	require.NoError(t, err)
	assert.Equal(t, StatusCompleted, completed.Status)
	assert.Equal(t, "agent-a", completed.Agent)

	history, err := store.History(ctx)
	require.NoError(t, err)
	require.Len(t, history, 3)
	assert.Equal(t, HistoryCompleted, history[2].Action)
	assert.Equal(t, "owner finished", history[2].Message)
}

func TestStore_ReviewFailCancelRequireLiveLeaseOwner(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "tasks.json")
	store := NewStore(path)

	task, err := store.Add(ctx, AddRequest{ID: "owned", Title: "owned task"})
	require.NoError(t, err)

	_, err = store.Claim(ctx, task.ID, AssignRequest{
		Agent:         "agent-a",
		SessionID:     "session-a",
		RunID:         "run-a",
		LeaseDuration: time.Hour,
	})
	require.NoError(t, err)

	_, err = store.RequestReview(ctx, task.ID, ReviewRequest{
		Agent:     "agent-b",
		SessionID: "session-b",
		RunID:     "run-b",
	})
	require.ErrorIs(t, err, ErrTaskLeased)

	_, err = store.Fail(ctx, task.ID, FailRequest{
		Agent:     "agent-b",
		SessionID: "session-b",
		RunID:     "run-b",
		Reason:    "cannot finish",
	})
	require.ErrorIs(t, err, ErrTaskLeased)

	_, err = store.Cancel(ctx, task.ID, CancelRequest{
		Agent:     "agent-b",
		SessionID: "session-b",
		RunID:     "run-b",
		Reason:    "superseded",
	})
	require.ErrorIs(t, err, ErrTaskLeased)

	review, err := store.RequestReview(ctx, task.ID, ReviewRequest{
		Agent:     "agent-a",
		SessionID: "session-a",
		RunID:     "run-a",
		Message:   "owner ready for review",
	})
	require.NoError(t, err)
	assert.Equal(t, StatusReview, review.Status)
	assert.Nil(t, review.Lease)
}

func TestStore_UpdateToPendingClearsOwnerAndLease(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "tasks.json")
	clock := newTestClock(time.Date(2026, 5, 6, 10, 45, 0, 0, time.UTC))
	store := NewStore(path)
	store.now = clock.now

	task, err := store.Add(ctx, AddRequest{ID: "reset", Title: "reset ownership"})
	require.NoError(t, err)

	claimed, err := store.Claim(ctx, task.ID, AssignRequest{Agent: "agent-a", LeaseDuration: time.Minute})
	require.NoError(t, err)
	require.NotNil(t, claimed.Lease)

	clock.advanceMinute()

	reset, err := store.Update(ctx, task.ID, UpdateRequest{
		Agent:   "ignored-agent",
		Status:  StatusReady,
		Actor:   "scheduler",
		Message: "release task",
	})
	require.NoError(t, err)
	assert.Equal(t, StatusReady, reset.Status)
	assert.Empty(t, reset.Agent)
	assert.Nil(t, reset.Lease)
	assert.Equal(t, claimed.AttemptCount, reset.AttemptCount)

	history, err := store.History(ctx)
	require.NoError(t, err)
	require.Len(t, history, 3)
	require.NotNil(t, history[2].Before)
	require.NotNil(t, history[2].After)
	assert.Equal(t, StatusInProgress, history[2].Before.Status)
	assert.Equal(t, StatusReady, history[2].After.Status)
	assert.Equal(t, "scheduler", history[2].Actor)
	assert.Equal(t, "release task", history[2].Message)
}

func TestStore_DependenciesBlockAssignmentUntilCompleted(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	store := NewStore(filepath.Join(t.TempDir(), "tasks.json"))
	store.now = newTestClock(time.Date(2026, 5, 6, 11, 0, 0, 0, time.UTC)).now

	dependency, err := store.Add(ctx, AddRequest{ID: "setup", Title: "set up workspace"})
	require.NoError(t, err)

	dependent, err := store.Add(ctx, AddRequest{
		ID:           "implement",
		Title:        "implement feature",
		Dependencies: []string{dependency.ID},
		Priority:     10,
	})
	require.NoError(t, err)
	assert.Equal(t, StatusBlocked, dependent.Status)
	assert.Equal(t, []string{dependency.ID}, dependent.Dependencies)
	assert.Equal(t, 10, dependent.Priority)

	_, err = store.Claim(ctx, dependent.ID, AssignRequest{Agent: "agent-a"})
	require.ErrorIs(t, err, ErrTaskBlocked)
	_, err = store.Update(ctx, dependent.ID, UpdateRequest{Agent: "agent-a"})
	require.ErrorIs(t, err, ErrTaskBlocked)
	_, err = store.Complete(ctx, dependent.ID, "agent-a")
	require.ErrorIs(t, err, ErrTaskBlocked)

	_, err = store.Complete(ctx, dependency.ID, "agent-a")
	require.NoError(t, err)

	result, err := store.Reconcile(ctx, ReconcileRequest{Actor: "scheduler"})
	require.NoError(t, err)
	assert.Equal(t, 1, result.Unblocked)
	require.Len(t, result.Tasks, 1)
	assert.Equal(t, StatusReady, result.Tasks[0].Status)

	claimed, err := store.Claim(ctx, dependent.ID, AssignRequest{Agent: "agent-b"})
	require.NoError(t, err)
	assert.Equal(t, StatusInProgress, claimed.Status)
	assert.Equal(t, "agent-b", claimed.Agent)
}

func TestStore_RejectsDependencyCycles(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	store := NewStore(filepath.Join(t.TempDir(), "tasks.json"))
	store.now = newTestClock(time.Date(2026, 5, 6, 11, 15, 0, 0, time.UTC)).now

	root, err := store.Add(ctx, AddRequest{ID: "root", Title: "root"})
	require.NoError(t, err)

	child, err := store.Add(ctx, AddRequest{
		ID:           "child",
		Title:        "child",
		Dependencies: []string{root.ID},
	})
	require.NoError(t, err)

	_, err = store.Update(ctx, root.ID, UpdateRequest{
		ReplaceDependencies: true,
		Dependencies:        []string{child.ID},
	})
	require.ErrorIs(t, err, ErrDependencyCycle)

	reloaded, err := store.Load(ctx)
	require.NoError(t, err)

	rootIdx := findTask(reloaded.Tasks, root.ID)
	require.NotEqual(t, -1, rootIdx)
	assert.Empty(t, reloaded.Tasks[rootIdx].Dependencies)

	missingFuture, err := store.Add(ctx, AddRequest{
		ID:           "waiting",
		Title:        "waiting on future",
		Dependencies: []string{"future"},
	})
	require.NoError(t, err)
	assert.Equal(t, StatusBlocked, missingFuture.Status)

	_, err = store.Add(ctx, AddRequest{
		ID:           "future",
		Title:        "future task",
		Dependencies: []string{missingFuture.ID},
	})
	require.ErrorIs(t, err, ErrDependencyCycle)
}

func TestStore_UpdatePersistsCoordinationMetadata(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	store := NewStore(filepath.Join(t.TempDir(), "tasks.json"))
	store.now = newTestClock(time.Date(2026, 5, 6, 11, 30, 0, 0, time.UTC)).now

	task, err := store.Add(ctx, AddRequest{ID: "review", Title: "review me"})
	require.NoError(t, err)

	updated, err := store.Update(ctx, task.ID, UpdateRequest{
		Actor:         "reviewer",
		ReviewStatus:  ReviewStatusChangesRequested,
		FailureReason: "needs tests",
		Priority:      42,
		SetPriority:   true,
		Message:       "record review findings",
	})
	require.NoError(t, err)
	assert.Equal(t, ReviewStatusChangesRequested, updated.ReviewStatus)
	assert.Equal(t, "needs tests", updated.FailureReason)
	assert.Equal(t, 42, updated.Priority)

	cleared, err := store.Update(ctx, task.ID, UpdateRequest{
		Actor:              "reviewer",
		ReviewStatus:       ReviewStatusApproved,
		ClearFailureReason: true,
	})
	require.NoError(t, err)
	assert.Equal(t, ReviewStatusApproved, cleared.ReviewStatus)
	assert.Empty(t, cleared.FailureReason)

	history, err := store.History(ctx)
	require.NoError(t, err)
	require.Len(t, history, 3)
	assert.Equal(t, "reviewer", history[1].Actor)
	require.NotNil(t, history[1].Before)
	require.NotNil(t, history[1].After)
	assert.Empty(t, history[1].Before.FailureReason)
	assert.Equal(t, "needs tests", history[1].After.FailureReason)
	assert.Equal(t, "record review findings", history[1].Message)
}

func TestStore_WorkflowStatesRiskBlockersAndRetries(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "tasks.json")
	clock := newTestClock(time.Date(2026, 5, 6, 12, 0, 0, 0, time.UTC))
	store := NewStore(path)
	store.now = clock.now

	blocked, err := store.Add(ctx, AddRequest{
		ID:            "workflow",
		Title:         "coordinate workflow",
		Priority:      7,
		Risk:          "high",
		BlockerReason: "waiting on design sign-off",
		Actor:         "planner",
	})
	require.NoError(t, err)
	assert.Equal(t, StatusBlocked, blocked.Status)
	assert.Equal(t, 7, blocked.Priority)
	assert.Equal(t, "high", blocked.Risk)
	assert.Equal(t, "waiting on design sign-off", blocked.BlockerReason)

	_, err = store.Reopen(ctx, blocked.ID, ReopenRequest{Actor: "scheduler"})
	require.Error(t, err)
	assert.Contains(t, err.Error(), `cannot reopen task with status "blocked"`)

	_, err = store.Claim(ctx, blocked.ID, AssignRequest{Agent: "agent-a"})
	require.ErrorIs(t, err, ErrTaskBlocked)

	clock.advanceMinute()

	ready, err := store.Update(ctx, blocked.ID, UpdateRequest{
		ClearBlockerReason: true,
		Risk:               "medium",
		Actor:              "planner",
		Message:            "design approved",
	})
	require.NoError(t, err)
	assert.Equal(t, StatusReady, ready.Status)
	assert.Empty(t, ready.BlockerReason)
	assert.Equal(t, "medium", ready.Risk)

	_, err = store.Update(ctx, blocked.ID, UpdateRequest{Status: StatusFailed})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "use dedicated workflow method")

	clock.advanceMinute()

	claimed, err := store.Claim(ctx, blocked.ID, AssignRequest{Agent: "agent-a", Actor: "agent-a"})
	require.NoError(t, err)
	assert.Equal(t, StatusInProgress, claimed.Status)
	require.NotNil(t, claimed.Lease)

	clock.advanceMinute()

	review, err := store.RequestReview(ctx, blocked.ID, ReviewRequest{
		Agent:   "agent-a",
		Actor:   "agent-a",
		Message: "ready for review",
	})
	require.NoError(t, err)
	assert.Equal(t, StatusReview, review.Status)
	assert.Equal(t, ReviewStatusPending, review.ReviewStatus)
	assert.Nil(t, review.Lease)

	_, err = store.Update(ctx, blocked.ID, UpdateRequest{Status: StatusReady})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "requires Reopen")

	_, err = store.Update(ctx, blocked.ID, UpdateRequest{Agent: "agent-b"})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "requires Reopen")

	_, err = store.Claim(ctx, blocked.ID, AssignRequest{Agent: "agent-b"})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "task is in review")

	clock.advanceMinute()

	reopenedFromReview, err := store.Reopen(ctx, blocked.ID, ReopenRequest{
		Actor:   "reviewer",
		Message: "changes requested",
	})
	require.NoError(t, err)
	assert.Equal(t, StatusReopened, reopenedFromReview.Status)
	assert.Equal(t, 1, reopenedFromReview.RetryCount)
	assert.Empty(t, reopenedFromReview.Agent)

	clock.advanceMinute()

	reclaimed, err := store.Claim(ctx, blocked.ID, AssignRequest{Agent: "agent-b", Actor: "agent-b"})
	require.NoError(t, err)
	assert.Equal(t, StatusInProgress, reclaimed.Status)
	assert.Equal(t, 2, reclaimed.AttemptCount)

	clock.advanceMinute()

	failed, err := store.Fail(ctx, blocked.ID, FailRequest{
		Agent:   "agent-b",
		Actor:   "agent-b",
		Reason:  "tests failed",
		Message: "implementation broke tests",
	})
	require.NoError(t, err)
	assert.Equal(t, StatusFailed, failed.Status)
	assert.Equal(t, "tests failed", failed.FailureReason)
	assert.Nil(t, failed.Lease)

	_, err = store.Claim(ctx, blocked.ID, AssignRequest{Agent: "agent-c"})
	require.ErrorIs(t, err, ErrTaskFailed)

	_, err = store.Complete(ctx, blocked.ID, "agent-c")
	require.ErrorIs(t, err, ErrTaskFailed)

	_, err = store.Update(ctx, blocked.ID, UpdateRequest{Status: StatusReady})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "requires Reopen")

	clock.advanceMinute()

	reopenedFromFailure, err := store.Reopen(ctx, blocked.ID, ReopenRequest{
		Actor:   "scheduler",
		Message: "retry failed work",
	})
	require.NoError(t, err)
	assert.Equal(t, StatusReopened, reopenedFromFailure.Status)
	assert.Equal(t, 2, reopenedFromFailure.RetryCount)
	assert.Empty(t, reopenedFromFailure.FailureReason)

	clock.advanceMinute()

	canceled, err := store.Cancel(ctx, blocked.ID, CancelRequest{
		Actor:   "scheduler",
		Reason:  "superseded",
		Message: "cancel obsolete work",
	})
	require.NoError(t, err)
	assert.Equal(t, StatusCanceled, canceled.Status)
	assert.Equal(t, "superseded", canceled.FailureReason)

	clock.advanceMinute()

	reopenedFromCancel, err := store.Reopen(ctx, blocked.ID, ReopenRequest{
		Actor:   "scheduler",
		Message: "bring back into plan",
	})
	require.NoError(t, err)
	assert.Equal(t, StatusReopened, reopenedFromCancel.Status)
	assert.Equal(t, 3, reopenedFromCancel.RetryCount)

	history, err := store.History(ctx)
	require.NoError(t, err)
	assert.Equal(t, []HistoryAction{
		HistoryAdded,
		HistoryUpdated,
		HistoryAssigned,
		HistoryReviewRequested,
		HistoryReopened,
		HistoryAssigned,
		HistoryFailed,
		HistoryReopened,
		HistoryCanceled,
		HistoryReopened,
	}, historyActions(history))
	require.NotNil(t, history[6].Before)
	require.NotNil(t, history[6].After)
	assert.Equal(t, StatusInProgress, history[6].Before.Status)
	assert.Equal(t, StatusFailed, history[6].After.Status)
	assert.Equal(t, "implementation broke tests", history[6].Message)
}

func TestStore_ReconcileRepairsManualAssignedTaskWithoutLease(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "tasks.json")
	now := time.Date(2026, 5, 6, 12, 30, 0, 0, time.UTC)
	writeState(t, path, State{
		Revision: 7,
		Tasks: []Task{
			{
				ID:        "manual",
				Title:     "manually edited assigned task",
				Status:    StatusInProgress,
				Agent:     "stale-agent",
				CreatedAt: now.Add(-time.Hour),
				UpdatedAt: now.Add(-time.Hour),
				Revision:  7,
			},
		},
	})

	store := NewStore(path)
	store.now = newTestClock(now).now

	result, err := store.Reconcile(ctx, ReconcileRequest{Actor: "scheduler", Message: "repair manual edit"})
	require.NoError(t, err)
	assert.Equal(t, 1, result.ExpiredLeases)
	assert.Equal(t, 1, result.HistoryEntries)
	assert.Equal(t, int64(8), result.StateRevision)
	require.Len(t, result.Tasks, 1)
	assert.Equal(t, StatusReady, result.Tasks[0].Status)
	assert.Empty(t, result.Tasks[0].Agent)
	assert.Nil(t, result.Tasks[0].Lease)

	history, err := store.History(ctx)
	require.NoError(t, err)
	require.Len(t, history, 1)
	assert.Equal(t, HistoryReconciled, history[0].Action)
	assert.Equal(t, "scheduler", history[0].Actor)
	assert.Equal(t, "repair manual edit", history[0].Message)
	require.NotNil(t, history[0].Before)
	require.NotNil(t, history[0].After)
	assert.Equal(t, StatusInProgress, history[0].Before.Status)
	assert.Equal(t, StatusReady, history[0].After.Status)

	claimed, err := store.Claim(ctx, "manual", AssignRequest{Agent: "fresh-agent"})
	require.NoError(t, err)
	assert.Equal(t, StatusInProgress, claimed.Status)
	assert.Equal(t, "fresh-agent", claimed.Agent)
	require.NotNil(t, claimed.Lease)
}

func TestStore_ReconcileRepairsManualLeaseOwnerMismatch(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "tasks.json")
	now := time.Date(2026, 5, 6, 12, 40, 0, 0, time.UTC)
	writeState(t, path, State{
		Revision: 11,
		Tasks: []Task{
			{
				ID:        "mismatch",
				Title:     "manually mismatched lease owner",
				Status:    StatusInProgress,
				Agent:     "stale-agent",
				CreatedAt: now.Add(-time.Hour),
				UpdatedAt: now.Add(-time.Minute),
				Revision:  11,
				Lease: &Lease{
					Owner:           "lease-agent",
					SessionID:       "session-1",
					RunID:           "run-1",
					AcquiredAt:      now.Add(-time.Minute),
					LastHeartbeatAt: now.Add(-time.Minute),
					ExpiresAt:       now.Add(time.Minute),
				},
			},
		},
	})

	store := NewStore(path)
	store.now = newTestClock(now).now

	result, err := store.Reconcile(ctx, ReconcileRequest{Actor: "scheduler", Message: "repair owner mismatch"})
	require.NoError(t, err)
	assert.Equal(t, 0, result.ExpiredLeases)
	assert.Equal(t, 1, result.HistoryEntries)
	assert.Equal(t, int64(12), result.StateRevision)
	require.Len(t, result.Tasks, 1)
	assert.Equal(t, StatusInProgress, result.Tasks[0].Status)
	assert.Equal(t, "lease-agent", result.Tasks[0].Agent)
	require.NotNil(t, result.Tasks[0].Lease)
	assert.Equal(t, "lease-agent", result.Tasks[0].Lease.Owner)

	_, err = store.Heartbeat(ctx, "mismatch", HeartbeatRequest{
		Agent:     "stale-agent",
		SessionID: "session-1",
		RunID:     "run-1",
	})
	require.ErrorIs(t, err, ErrTaskLeased)

	renewed, err := store.Heartbeat(ctx, "mismatch", HeartbeatRequest{
		Agent:         "lease-agent",
		SessionID:     "session-1",
		RunID:         "run-1",
		LeaseDuration: 2 * time.Minute,
	})
	require.NoError(t, err)
	assert.Equal(t, "lease-agent", renewed.Agent)
	require.NotNil(t, renewed.Lease)
	assert.Equal(t, now.Add(2*time.Minute), renewed.Lease.ExpiresAt)
}

func TestStore_ReconcileClearsOwnerWhenUnblockingManualBlockedTask(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "tasks.json")
	now := time.Date(2026, 5, 6, 12, 45, 0, 0, time.UTC)
	writeState(t, path, State{
		Revision: 3,
		Tasks: []Task{
			{
				ID:        "manual-blocked",
				Title:     "manually edited blocked task",
				Status:    StatusBlocked,
				Agent:     "stale-agent",
				CreatedAt: now.Add(-time.Hour),
				UpdatedAt: now.Add(-time.Hour),
				Revision:  3,
			},
		},
	})

	store := NewStore(path)
	store.now = newTestClock(now).now

	result, err := store.Reconcile(ctx, ReconcileRequest{Actor: "scheduler"})
	require.NoError(t, err)
	assert.Equal(t, 1, result.Unblocked)
	assert.Equal(t, 1, result.HistoryEntries)
	require.Len(t, result.Tasks, 1)
	assert.Equal(t, StatusReady, result.Tasks[0].Status)
	assert.Empty(t, result.Tasks[0].Agent)
	assert.Nil(t, result.Tasks[0].Lease)
}

func TestStore_ReconcileClearsOwnerOnStillBlockedManualTask(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "tasks.json")
	now := time.Date(2026, 5, 6, 12, 50, 0, 0, time.UTC)
	writeState(t, path, State{
		Revision: 4,
		Tasks: []Task{
			{
				ID:        "dependency",
				Title:     "dependency",
				Status:    StatusReady,
				CreatedAt: now.Add(-2 * time.Hour),
				UpdatedAt: now.Add(-2 * time.Hour),
				Revision:  1,
			},
			{
				ID:           "manual-blocked",
				Title:        "manually edited blocked task",
				Status:       StatusBlocked,
				Agent:        "stale-agent",
				Dependencies: []string{"dependency"},
				CreatedAt:    now.Add(-time.Hour),
				UpdatedAt:    now.Add(-time.Hour),
				Revision:     4,
				Lease: &Lease{
					Owner:           "stale-agent",
					AcquiredAt:      now.Add(-time.Minute),
					LastHeartbeatAt: now.Add(-time.Minute),
					ExpiresAt:       now.Add(time.Minute),
				},
			},
		},
	})

	store := NewStore(path)
	store.now = newTestClock(now).now

	result, err := store.Reconcile(ctx, ReconcileRequest{Actor: "scheduler"})
	require.NoError(t, err)
	assert.Equal(t, 0, result.Blocked)
	assert.Equal(t, 0, result.Unblocked)
	assert.Equal(t, 1, result.HistoryEntries)
	require.Len(t, result.Tasks, 1)
	assert.Equal(t, StatusBlocked, result.Tasks[0].Status)
	assert.Empty(t, result.Tasks[0].Agent)
	assert.Nil(t, result.Tasks[0].Lease)
}

func TestStore_ExpectedRevisionRejectsStaleUpdates(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "tasks.json")
	storeA := NewStore(path)
	storeA.now = newTestClock(time.Date(2026, 5, 6, 12, 0, 0, 0, time.UTC)).now

	created, err := storeA.Add(ctx, AddRequest{ID: "conflict", Title: "initial", Actor: "planner"})
	require.NoError(t, err)

	updated, err := storeA.Update(ctx, created.ID, UpdateRequest{
		Title:            "latest title",
		Actor:            "editor-a",
		ExpectedRevision: created.Revision,
	})
	require.NoError(t, err)
	assert.Greater(t, updated.Revision, created.Revision)

	_, err = NewStore(path).Update(ctx, created.ID, UpdateRequest{
		Title:            "stale title",
		Actor:            "editor-b",
		ExpectedRevision: created.Revision,
	})
	require.ErrorIs(t, err, ErrRevisionConflict)

	history, err := storeA.History(ctx)
	require.NoError(t, err)
	require.Len(t, history, 2)
	assert.Equal(t, "planner", history[0].Actor)
	assert.Equal(t, "editor-a", history[1].Actor)
	require.NotNil(t, history[1].Before)
	require.NotNil(t, history[1].After)
	assert.Equal(t, "initial", history[1].Before.Title)
	assert.Equal(t, "latest title", history[1].After.Title)
}

func TestStore_RejectsInvalidOperations(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	store := NewStore(filepath.Join(t.TempDir(), "tasks.json"))
	store.now = newTestClock(time.Date(2026, 5, 5, 14, 0, 0, 0, time.UTC)).now

	_, err := store.Add(ctx, AddRequest{ID: "empty", Title: "  "})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "title is required")

	_, err = store.Add(ctx, AddRequest{ID: "same", Title: "one"})
	require.NoError(t, err)
	_, err = store.Add(ctx, AddRequest{ID: "same", Title: "two"})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "duplicate")

	_, err = store.Assign(ctx, "same", "")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "agent is required")

	_, err = store.Update(ctx, "missing", UpdateRequest{Title: "nope"})
	require.ErrorIs(t, err, ErrTaskNotFound)

	_, err = store.Update(ctx, "same", UpdateRequest{Status: StatusCompleted})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "use Complete")

	_, err = store.Complete(ctx, "same", "agent")
	require.NoError(t, err)
	_, err = store.Assign(ctx, "same", "agent")
	require.ErrorIs(t, err, ErrTaskCompleted)

	_, err = store.Complete(ctx, "same", "agent")
	require.ErrorIs(t, err, ErrTaskCompleted)
}

func TestStore_LoadSortsExistingFileAndRejectsCorruptState(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	dir := t.TempDir()
	path := filepath.Join(dir, "tasks.json")
	now := time.Date(2026, 5, 5, 15, 0, 0, 0, time.UTC)
	state := State{
		Tasks: []Task{
			{ID: "later", Title: "later", Status: StatusReady, CreatedAt: now.Add(time.Hour), UpdatedAt: now.Add(time.Hour)},
			{ID: "earlier", Title: "earlier", Status: StatusReady, CreatedAt: now, UpdatedAt: now},
		},
		History: []HistoryEntry{
			{Seq: 2, At: now.Add(time.Minute), Action: HistoryUpdated, TaskID: "later", Actor: "editor"},
			{Seq: 1, At: now, Action: HistoryAdded, TaskID: "earlier", Actor: "planner"},
		},
	}
	writeState(t, path, state)

	loaded, err := NewStore(path).Load(ctx)
	require.NoError(t, err)
	assert.Equal(t, []string{"earlier", "later"}, []string{loaded.Tasks[0].ID, loaded.Tasks[1].ID})
	assert.Equal(t, []int64{1, 2}, historySeqs(loaded.History))

	badPath := filepath.Join(dir, "bad.json")
	require.NoError(t, os.WriteFile(badPath, []byte(`{"tasks":[{"id":"x"}]}`), 0o600))
	_, err = NewStore(badPath).Load(ctx)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "title is required")

	badHistoryPath := filepath.Join(dir, "bad-history.json")
	writeState(t, badHistoryPath, State{
		Tasks: []Task{
			{ID: "task", Title: "task", Status: StatusReady, CreatedAt: now, UpdatedAt: now},
		},
		History: []HistoryEntry{
			{Seq: 1, At: now, Action: HistoryAction("unknown"), TaskID: "task", Actor: "planner"},
		},
	})
	_, err = NewStore(badHistoryPath).Load(ctx)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "invalid history action")

	badHistoryActorPath := filepath.Join(dir, "bad-history-actor.json")
	writeState(t, badHistoryActorPath, State{
		Tasks: []Task{
			{ID: "task", Title: "task", Status: StatusReady, CreatedAt: now, UpdatedAt: now},
		},
		History: []HistoryEntry{
			{Seq: 1, At: now, Action: HistoryAdded, TaskID: "task"},
		},
	})
	_, err = NewStore(badHistoryActorPath).Load(ctx)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "history actor")

	badLeasePath := filepath.Join(dir, "bad-lease.json")
	writeState(t, badLeasePath, State{
		Tasks: []Task{
			{
				ID:        "task",
				Title:     "task",
				Status:    StatusReady,
				CreatedAt: now,
				UpdatedAt: now,
				Lease: &Lease{
					Owner:           "agent",
					AcquiredAt:      now,
					LastHeartbeatAt: now,
					ExpiresAt:       now.Add(time.Minute),
				},
			},
		},
	})
	_, err = NewStore(badLeasePath).Load(ctx)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "lease is only valid")

	invalidPath := filepath.Join(dir, "invalid.json")
	require.NoError(t, os.WriteFile(invalidPath, []byte(`not json`), 0o600))
	_, err = NewStore(invalidPath).Load(ctx)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "parse")
}

func TestStore_RepairBacksUpMalformedTaskFile(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "tasks.json")
	require.NoError(t, os.WriteFile(path, []byte(`not json`), 0o600))

	store := NewStore(path)
	store.now = newTestClock(time.Date(2026, 5, 7, 9, 0, 0, 0, time.UTC)).now

	_, err := store.Load(ctx)
	require.Error(t, err)

	result, err := store.Repair(ctx, RepairRequest{Actor: "repairer", Message: "recover corrupt file"})
	require.NoError(t, err)
	assert.True(t, result.Repaired)
	assert.Equal(t, int64(1), result.StateRevision)
	assert.Equal(t, 1, result.HistoryEntries)
	assert.NotEmpty(t, result.BackupPath)

	backup, err := os.ReadFile(result.BackupPath)
	require.NoError(t, err)
	assert.Equal(t, "not json", string(backup))

	state, err := store.Load(ctx)
	require.NoError(t, err)
	assert.Empty(t, state.Tasks)
	require.Len(t, state.History, 1)
	assert.Equal(t, HistoryRepaired, state.History[0].Action)
	assert.Equal(t, "__state__", state.History[0].TaskID)
	assert.Equal(t, "repairer", state.History[0].Actor)
	assert.Contains(t, state.History[0].Message, "recover corrupt file")
	assert.Contains(t, state.History[0].Message, "malformed JSON")
}

func TestStore_RepairNormalizesConflictedTaskFile(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "tasks.json")
	now := time.Date(2026, 5, 7, 9, 30, 0, 0, time.UTC)
	completedAt := now.Add(-90 * time.Minute)
	writeState(t, path, State{
		Revision: 4,
		Tasks: []Task{
			{
				ID:           "dup",
				Title:        " keep ",
				Status:       StatusPending,
				CreatedAt:    now.Add(-time.Hour),
				UpdatedAt:    now.Add(-time.Hour),
				Dependencies: []string{"dep", "dep"},
				Revision:     2,
			},
			{
				ID:        "dup",
				Title:     "drop duplicate",
				Status:    StatusReady,
				CreatedAt: now.Add(-time.Minute),
				UpdatedAt: now.Add(-time.Minute),
				Revision:  3,
			},
			{
				ID:          "dep",
				Title:       "dependency",
				Status:      StatusCompleted,
				CreatedAt:   now.Add(-2 * time.Hour),
				UpdatedAt:   completedAt,
				CompletedAt: &completedAt,
				Revision:    1,
			},
		},
		History: []HistoryEntry{
			{Seq: 1, At: now, Action: HistoryAdded, TaskID: "dup", Actor: "planner"},
			{Seq: 2, At: now, Action: "", TaskID: "", Actor: "broken"},
		},
	})

	store := NewStore(path)
	store.now = newTestClock(now).now

	_, err := store.Load(ctx)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "duplicate task id")

	result, err := store.Repair(ctx, RepairRequest{Actor: "repairer", Message: "dedupe"})
	require.NoError(t, err)
	assert.True(t, result.Repaired)
	assert.Equal(t, 2, result.TasksRecovered)
	assert.Equal(t, 1, result.TasksDropped)
	assert.Equal(t, 1, result.HistoryEntries)
	assert.FileExists(t, result.BackupPath)

	state, err := store.Load(ctx)
	require.NoError(t, err)
	require.Len(t, state.Tasks, 2)

	dupIdx := findTask(state.Tasks, "dup")
	require.NotEqual(t, -1, dupIdx)
	assert.Equal(t, "keep", state.Tasks[dupIdx].Title)
	assert.Equal(t, StatusReady, state.Tasks[dupIdx].Status)
	assert.Equal(t, []string{"dep"}, state.Tasks[dupIdx].Dependencies)

	require.Len(t, state.History, 2)
	assert.Equal(t, HistoryRepaired, state.History[1].Action)
	assert.Equal(t, "repairer", state.History[1].Actor)
	assert.Contains(t, state.History[1].Message, "dedupe")
}

func TestStore_RepairNormalizesCorruptHistoryEvidence(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "tasks.json")
	now := time.Date(2026, 5, 7, 9, 45, 0, 0, time.UTC)
	writeState(t, path, State{
		Revision: 2,
		Tasks: []Task{
			{
				ID:        "task",
				Title:     "task",
				Status:    StatusReady,
				CreatedAt: now.Add(-time.Hour),
				UpdatedAt: now.Add(-time.Hour),
				Revision:  1,
			},
		},
		History: []HistoryEntry{
			{Seq: 0, Action: HistoryAdded, TaskID: "task"},
			{Seq: 1, At: now.Add(-time.Minute), Action: HistoryUpdated, TaskID: "task"},
			{Seq: 3, At: now.Add(-time.Minute), Action: HistoryAction("unknown"), TaskID: "task"},
		},
	})

	store := NewStore(path)
	store.now = newTestClock(now).now

	_, err := store.Load(ctx)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "history seq")

	result, err := store.Repair(ctx, RepairRequest{Actor: "repairer", Message: "restore history evidence"})
	require.NoError(t, err)
	assert.True(t, result.Repaired)
	assert.Equal(t, 1, result.TasksRecovered)
	assert.Equal(t, 1, result.HistoryEntries)
	assert.FileExists(t, result.BackupPath)

	state, err := store.Load(ctx)
	require.NoError(t, err)
	require.Len(t, state.History, 3)
	assert.Equal(t, []int64{1, 2, 3}, historySeqs(state.History))
	assert.Equal(t, "system", state.History[0].Actor)
	assert.Equal(t, now, state.History[0].At)
	assert.Equal(t, HistoryRepaired, state.History[2].Action)
	assert.Contains(t, state.History[2].Message, "restore history evidence")
}

func TestStore_RepairClearsLeaseFromNonProgressTask(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "tasks.json")
	now := time.Date(2026, 5, 7, 10, 0, 0, 0, time.UTC)
	writeState(t, path, State{
		Revision: 3,
		Tasks: []Task{
			{
				ID:        "ready",
				Title:     "ready but leased",
				Status:    StatusReady,
				Agent:     "stale-agent",
				CreatedAt: now.Add(-time.Hour),
				UpdatedAt: now.Add(-time.Minute),
				Revision:  3,
				Lease: &Lease{
					Owner:           "stale-agent",
					AcquiredAt:      now.Add(-time.Minute),
					LastHeartbeatAt: now.Add(-time.Minute),
					ExpiresAt:       now.Add(time.Hour),
				},
			},
		},
	})

	store := NewStore(path)
	store.now = newTestClock(now).now

	_, err := store.Load(ctx)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "lease is only valid")

	result, err := store.Repair(ctx, RepairRequest{Actor: "repairer", Message: "remove stale lease"})
	require.NoError(t, err)
	assert.True(t, result.Repaired)
	assert.Equal(t, 1, result.TasksRecovered)
	assert.Equal(t, 1, result.HistoryEntries)
	assert.FileExists(t, result.BackupPath)

	state, err := store.Load(ctx)
	require.NoError(t, err)
	require.Len(t, state.Tasks, 1)
	assert.Nil(t, state.Tasks[0].Lease)
	assert.Equal(t, StatusReady, state.Tasks[0].Status)
	assert.Empty(t, state.Tasks[0].Agent)
	require.Len(t, state.History, 1)
	assert.Equal(t, HistoryRepaired, state.History[0].Action)
	assert.Contains(t, state.History[0].Message, "remove stale lease")
}

func TestStore_UsesAtomicFileReplacementAndNoTempLeftovers(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	dir := t.TempDir()
	path := filepath.Join(dir, "nested", "tasks.json")
	store := NewStore(path)
	store.now = newTestClock(time.Date(2026, 5, 5, 16, 0, 0, 0, time.UTC)).now

	_, err := store.Add(ctx, AddRequest{ID: "one", Title: "one"})
	require.NoError(t, err)
	_, err = store.Update(ctx, "one", UpdateRequest{Title: "updated"})
	require.NoError(t, err)

	data, err := os.ReadFile(path)
	require.NoError(t, err)
	assert.True(t, json.Valid(data), "persisted file should remain valid JSON")

	matches, err := filepath.Glob(filepath.Join(filepath.Dir(path), ".tasklist-*.json"))
	require.NoError(t, err)
	assert.Empty(t, matches)
}

func TestStore_RespectsCanceledContextAndDefensivelyCopiesTasks(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	store := NewStore(filepath.Join(t.TempDir(), "tasks.json"))
	store.now = newTestClock(time.Date(2026, 5, 5, 17, 0, 0, 0, time.UTC)).now

	created, err := store.Add(ctx, AddRequest{ID: "copy", Title: "copy", Metadata: map[string]string{"k": "v"}})
	require.NoError(t, err)

	created.Metadata["k"] = "mutated"

	listed, err := store.List(ctx)
	require.NoError(t, err)
	require.Len(t, listed, 1)
	assert.Equal(t, "v", listed[0].Metadata["k"])
	listed[0].Metadata["k"] = "mutated-again"

	relisted, err := store.List(ctx)
	require.NoError(t, err)
	assert.Equal(t, "v", relisted[0].Metadata["k"])

	canceled, cancel := context.WithCancel(ctx)
	cancel()

	_, err = store.Add(canceled, AddRequest{Title: "blocked"})
	require.ErrorIs(t, err, context.Canceled)
}

func TestStore_EmptyMissingFileReturnsEmptyLists(t *testing.T) {
	t.Parallel()

	store := NewStore(filepath.Join(t.TempDir(), "missing.json"))
	state, err := store.Load(context.Background())
	require.NoError(t, err)
	assert.Empty(t, state.Tasks)
	assert.Empty(t, state.History)

	list, err := store.List(context.Background())
	require.NoError(t, err)
	assert.Empty(t, list)
}

func TestStore_ReturnsPathValidationErrors(t *testing.T) {
	t.Parallel()

	store := NewStore("")
	_, err := store.Add(context.Background(), AddRequest{Title: "no path"})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "path is required")

	_, err = store.Load(context.Background())
	require.Error(t, err)
	assert.Contains(t, err.Error(), "path is required")

	assert.Empty(t, store.Path())
}

func writeState(t *testing.T, path string, state State) {
	t.Helper()

	data, err := json.Marshal(state)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(path, data, 0o600))
}

func historyActions(history []HistoryEntry) []HistoryAction {
	actions := make([]HistoryAction, len(history))
	for i := range history {
		actions[i] = history[i].Action
	}

	return actions
}

func historySeqs(history []HistoryEntry) []int64 {
	seqs := make([]int64, len(history))
	for i := range history {
		seqs[i] = history[i].Seq
	}

	return seqs
}

func historyStateRevisions(history []HistoryEntry) []int64 {
	revisions := make([]int64, len(history))
	for i := range history {
		revisions[i] = history[i].StateRevision
	}

	return revisions
}

type testClock struct {
	current time.Time
}

func newTestClock(start time.Time) *testClock {
	return &testClock{current: start}
}

func (c *testClock) now() time.Time {
	return c.current
}

func (c *testClock) advanceMinute() {
	c.advance(time.Minute)
}

func (c *testClock) advance(duration time.Duration) {
	c.current = c.current.Add(duration)
}
