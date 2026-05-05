package tasklist

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
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
	assert.Equal(t, StatusPending, second.Status)
	assert.Equal(t, StatusAssigned, first.Status)
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

	task, err := store.Add(ctx, AddRequest{ID: "todo-1", Title: "draft SDK"})
	require.NoError(t, err)

	clock.advanceMinute()

	assigned, err := store.Assign(ctx, task.ID, "agent-a")
	require.NoError(t, err)
	assert.Equal(t, StatusAssigned, assigned.Status)
	assert.Equal(t, "agent-a", assigned.Agent)
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

	completed, err := store.Complete(ctx, task.ID, "agent-b")
	require.NoError(t, err)
	assert.Equal(t, StatusCompleted, completed.Status)
	assert.Equal(t, "agent-b", completed.Agent)
	require.NotNil(t, completed.CompletedAt)
	assert.Equal(t, completed.UpdatedAt, *completed.CompletedAt)

	history, err := store.History(ctx)
	require.NoError(t, err)
	require.Len(t, history, 4)
	assert.Equal(t, []HistoryAction{HistoryAdded, HistoryAssigned, HistoryUpdated, HistoryCompleted}, historyActions(history))
	assert.Equal(t, []int64{1, 2, 3, 4}, historySeqs(history))
	assert.Equal(t, "clarified scope", history[2].Message)
	assert.Equal(t, "agent-b", history[3].Agent)
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
			{ID: "later", Title: "later", Status: StatusPending, CreatedAt: now.Add(time.Hour), UpdatedAt: now.Add(time.Hour)},
			{ID: "earlier", Title: "earlier", Status: StatusPending, CreatedAt: now, UpdatedAt: now},
		},
		History: []HistoryEntry{
			{Seq: 2, At: now.Add(time.Minute), Action: HistoryUpdated, TaskID: "later"},
			{Seq: 1, At: now, Action: HistoryAdded, TaskID: "earlier"},
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

	invalidPath := filepath.Join(dir, "invalid.json")
	require.NoError(t, os.WriteFile(invalidPath, []byte(`not json`), 0o600))
	_, err = NewStore(invalidPath).Load(ctx)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "parse")
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
	c.current = c.current.Add(time.Minute)
}
