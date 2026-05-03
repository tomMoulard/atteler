package session

import (
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestStore_SaveLoadListHeadlessRuns(t *testing.T) {
	t.Parallel()

	store := NewStore(t.TempDir())
	completedAt := time.Date(2026, 5, 3, 12, 0, 0, 0, time.UTC)

	first := HeadlessRun{
		ID:          "run-one",
		SessionID:   "session-one",
		SessionPath: store.Path("session-one"),
		Prompt:      "first prompt",
		Model:       "gpt-5.5",
		Agent:       "executor",
		Status:      HeadlessStatusRunning,
	}
	require.NoError(t, store.SaveHeadlessRun(first))

	// Save timestamps are assigned at write time, so ensure deterministic newest-first ordering.
	time.Sleep(2 * time.Millisecond)

	second := HeadlessRun{
		ID:          "run-two",
		SessionID:   "session-two",
		SessionPath: store.Path("session-two"),
		Prompt:      "second prompt",
		Model:       "gpt-5.5-mini",
		Agent:       "reviewer",
		Status:      HeadlessStatusCompleted,
		CompletedAt: &completedAt,
	}
	require.NoError(t, store.SaveHeadlessRun(second))

	loaded, err := store.LoadHeadlessRun(first.ID)
	require.NoError(t, err)
	assert.Equal(t, first.ID, loaded.ID)
	assert.Equal(t, first.SessionID, loaded.SessionID)
	assert.Equal(t, first.SessionPath, loaded.SessionPath)
	assert.Equal(t, first.Prompt, loaded.Prompt)
	assert.Equal(t, first.Model, loaded.Model)
	assert.Equal(t, first.Agent, loaded.Agent)
	assert.Equal(t, HeadlessStatusRunning, loaded.Status)
	assert.Equal(t, filepath.Join(store.Dir(), "headless", first.ID+".log"), loaded.LogPath)
	assert.False(t, loaded.StartedAt.IsZero())
	assert.False(t, loaded.UpdatedAt.IsZero())

	runs, err := store.ListHeadlessRuns()
	require.NoError(t, err)
	require.Len(t, runs, 2)
	assert.Equal(t, []string{"run-two", "run-one"}, []string{runs[0].ID, runs[1].ID})
	assert.Equal(t, HeadlessStatusCompleted, runs[0].Status)
	assert.Equal(t, HeadlessStatusRunning, runs[1].Status)
	require.NotNil(t, runs[0].CompletedAt)
	assert.True(t, completedAt.Equal(*runs[0].CompletedAt))
}

func TestStore_HeadlessLogAppendRead(t *testing.T) {
	t.Parallel()

	store := NewStore(t.TempDir())

	require.NoError(t, store.AppendHeadlessLog("run-log", "first line\n"))
	require.NoError(t, store.AppendHeadlessLog("run-log", "second line\n"))

	log, err := store.ReadHeadlessLog("run-log")
	require.NoError(t, err)
	assert.Equal(t, "first line\nsecond line\n", log)
}

func TestStore_HeadlessRejectsBlankIDs(t *testing.T) {
	t.Parallel()

	store := NewStore(t.TempDir())

	require.ErrorContains(t, store.SaveHeadlessRun(HeadlessRun{}), "headless id is required")
	require.ErrorContains(t, store.AppendHeadlessLog(" \t ", "log"), "headless id is required")
	_, err := store.LoadHeadlessRun("")
	require.ErrorContains(t, err, "headless id is required")
	_, err = store.ReadHeadlessLog(" ")
	require.ErrorContains(t, err, "headless id is required")
}
