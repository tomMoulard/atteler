package session

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
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
		ModelMode:   "fast",
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
	assert.Equal(t, first.ModelMode, loaded.ModelMode)
	assert.Equal(t, first.Agent, loaded.Agent)
	assert.Equal(t, HeadlessStatusRunning, loaded.Status)
	assert.Equal(t, filepath.Join(store.Dir(), "headless", first.ID+".log"), loaded.LogPath)
	assert.NotEmpty(t, loaded.ArtifactDir)
	assert.NotEmpty(t, loaded.StartedCommand)
	assert.NotZero(t, loaded.PID)
	assert.False(t, loaded.StartedAt.IsZero())
	assert.False(t, loaded.UpdatedAt.IsZero())
	assert.False(t, loaded.LastHeartbeatAt.IsZero())

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

func TestStore_HeadlessLogRotatesAndTailsByOffset(t *testing.T) {
	t.Parallel()

	store := NewStore(t.TempDir())
	policy := HeadlessLogPolicy{MaxChunkBytes: 10, MaxChunks: 10}

	require.NoError(t, store.AppendHeadlessLogWithOptions("run-tail", "abcdefghij", HeadlessLogWriteOptions{Policy: policy, Private: true}))
	require.NoError(t, store.AppendHeadlessLogWithOptions("run-tail", "klmnop", HeadlessLogWriteOptions{Policy: policy, Private: true}))

	chunks := headlessLogChunkFiles(t, store, "run-tail")
	assert.Equal(t, []string{"run-tail.log.000001", "run-tail.log.000002"}, chunks)

	first, err := store.TailHeadlessLog("run-tail", HeadlessLogTailOptions{MaxBytes: 4})
	require.NoError(t, err)
	assert.Equal(t, "abcd", first.Text)
	assert.Equal(t, HeadlessLogOffset{Chunk: 1, Byte: 4}, first.NextOffset)

	second, err := store.TailHeadlessLog("run-tail", HeadlessLogTailOptions{Offset: first.NextOffset, MaxBytes: 20})
	require.NoError(t, err)
	assert.Equal(t, "efghijklmnop", second.Text)
	assert.Equal(t, HeadlessLogOffset{Chunk: 2, Byte: 6}, second.NextOffset)
}

func TestStore_HeadlessLogTailReportsTruncatedOffset(t *testing.T) {
	t.Parallel()

	store := NewStore(t.TempDir())
	policy := HeadlessLogPolicy{MaxChunkBytes: 4, MaxChunks: 2}

	require.NoError(t, store.AppendHeadlessLogWithOptions("run-tail-truncated", "aaaabbbbcccc", HeadlessLogWriteOptions{
		Policy:  policy,
		Private: true,
	}))

	tail, err := store.TailHeadlessLog("run-tail-truncated", HeadlessLogTailOptions{
		Offset:   HeadlessLogOffset{Chunk: 1, Byte: 2},
		MaxBytes: 16,
	})
	require.NoError(t, err)
	assert.Equal(t, "bbbbcccc", tail.Text)
	assert.Equal(t, HeadlessLogOffset{Chunk: 2, Byte: 0}, tail.RetainedOffset)
	assert.Equal(t, HeadlessLogOffset{Chunk: 3, Byte: 4}, tail.NextOffset)
	assert.True(t, tail.Truncated)
}

func TestStore_HeadlessLogRetentionCleanup(t *testing.T) {
	t.Parallel()

	store := NewStore(t.TempDir())
	policy := HeadlessLogPolicy{MaxChunkBytes: 5, MaxChunks: 2}

	require.NoError(t, store.AppendHeadlessLogWithOptions("run-retain", "aaaaa", HeadlessLogWriteOptions{Policy: policy, Private: true}))
	require.NoError(t, store.AppendHeadlessLogWithOptions("run-retain", "bbbbb", HeadlessLogWriteOptions{Policy: policy, Private: true}))
	require.NoError(t, store.AppendHeadlessLogWithOptions("run-retain", "ccccc", HeadlessLogWriteOptions{Policy: policy, Private: true}))

	chunks := headlessLogChunkFiles(t, store, "run-retain")
	assert.Equal(t, []string{"run-retain.log.000002", "run-retain.log.000003"}, chunks)

	log, err := store.ReadHeadlessLog("run-retain")
	require.NoError(t, err)
	assert.Equal(t, "bbbbbccccc", log)
}

func TestStore_HeadlessLogMaxAgeRetentionCleanup(t *testing.T) {
	t.Parallel()

	store := NewStore(t.TempDir())
	policy := HeadlessLogPolicy{MaxChunkBytes: 5, MaxChunks: 4}

	require.NoError(t, store.AppendHeadlessLogWithOptions("run-retain-age", "aaaaabbbbbccccc", HeadlessLogWriteOptions{
		Policy:  policy,
		Private: true,
	}))

	oldTime := time.Now().Add(-time.Hour)

	for _, name := range headlessLogChunkFiles(t, store, "run-retain-age") {
		path := filepath.Join(store.Dir(), "headless", name)
		require.NoError(t, os.Chtimes(path, oldTime, oldTime))
	}

	require.NoError(t, store.CleanupHeadlessLogs("run-retain-age", HeadlessLogPolicy{
		MaxAge:        time.Millisecond,
		MaxChunkBytes: 5,
		MaxChunks:     4,
	}))

	assert.Empty(t, headlessLogChunkFiles(t, store, "run-retain-age"))
}

func TestStore_HeadlessLogMigratesLegacyBoundedRedactedTail(t *testing.T) {
	t.Parallel()

	store := NewStore(t.TempDir())
	policy := HeadlessLogPolicy{MaxChunkBytes: 32, MaxChunks: 2}
	secret := "sk-" + strings.Repeat("c", 20)
	legacyID := "run-legacy"

	require.NoError(t, os.MkdirAll(filepath.Join(store.Dir(), "headless"), 0o750))
	require.NoError(t, os.WriteFile(
		store.headlessLogPath(legacyID),
		[]byte("discard-me-"+strings.Repeat("x", 200)+"token="+secret+"\nlegacy tail\n"),
		0o600,
	))

	require.NoError(t, store.AppendHeadlessLogWithOptions(
		legacyID,
		"new token="+secret+"\n",
		HeadlessLogWriteOptions{Policy: policy},
	))

	_, err := os.Stat(store.headlessLogPath(legacyID))
	require.ErrorIs(t, err, os.ErrNotExist)

	chunks := headlessLogChunkFiles(t, store, legacyID)
	require.NotEmpty(t, chunks)
	assert.LessOrEqual(t, len(chunks), policy.MaxChunks)

	for _, name := range chunks {
		info, statErr := os.Stat(filepath.Join(store.Dir(), "headless", name))
		require.NoError(t, statErr)
		assert.LessOrEqual(t, info.Size(), policy.MaxChunkBytes)
	}

	log, err := store.ReadHeadlessLog(legacyID)
	require.NoError(t, err)
	assert.NotContains(t, log, secret)
	assert.NotContains(t, log, "discard-me")
	assert.Contains(t, log, headlessLogRedactedValue)
	assert.Contains(t, log, "legacy tail")
	assert.Contains(t, log, "new token=")
}

func TestStore_CleanupHeadlessLogsMigratesLegacyLog(t *testing.T) {
	t.Parallel()

	store := NewStore(t.TempDir())
	policy := HeadlessLogPolicy{MaxChunkBytes: 64, MaxChunks: 1}
	secret := "ghp_" + strings.Repeat("d", 20)
	legacyID := "run-legacy-cleanup"

	require.NoError(t, os.MkdirAll(filepath.Join(store.Dir(), "headless"), 0o750))
	require.NoError(t, os.WriteFile(
		store.headlessLogPath(legacyID),
		[]byte("legacy token="+secret+"\n"),
		0o600,
	))

	require.NoError(t, store.CleanupHeadlessLogs(legacyID, policy))

	_, err := os.Stat(store.headlessLogPath(legacyID))
	require.ErrorIs(t, err, os.ErrNotExist)

	chunks := headlessLogChunkFiles(t, store, legacyID)
	require.Len(t, chunks, 1)

	info, err := os.Stat(filepath.Join(store.Dir(), "headless", chunks[0]))
	require.NoError(t, err)
	assert.LessOrEqual(t, info.Size(), policy.MaxChunkBytes)

	log, err := store.ReadHeadlessLog(legacyID)
	require.NoError(t, err)
	assert.NotContains(t, log, secret)
	assert.Contains(t, log, headlessLogRedactedValue)
}

func TestStore_HeadlessRedactsByDefaultAndAllowsPrivateLogs(t *testing.T) {
	store := NewStore(t.TempDir())
	envValue := strings.Join([]string{"env", "value", "123456789"}, "-")
	t.Setenv("ATTELER_TEST_TOKEN", envValue)

	rawPrompt := "deploy with token=" + envValue
	rawError := "provider failed Authorization: Bearer " + strings.Repeat("a", 16)
	require.NoError(t, store.SaveHeadlessRun(HeadlessRun{
		ID:     "run-redacted",
		Prompt: rawPrompt,
		Error:  rawError,
		Status: HeadlessStatusFailed,
	}))

	loaded, err := store.LoadHeadlessRun("run-redacted")
	require.NoError(t, err)
	assert.NotContains(t, loaded.Prompt, envValue)
	assert.NotContains(t, loaded.Error, strings.Repeat("a", 16))
	assert.Contains(t, loaded.Prompt, headlessLogRedactedValue)
	assert.Contains(t, loaded.Error, headlessLogRedactedValue)

	patternSecret := "sk-" + strings.Repeat("b", 20)
	require.NoError(t, store.AppendHeadlessLog("run-redacted", "api_key="+patternSecret+" env="+envValue+"\n"))
	log, err := store.ReadHeadlessLog("run-redacted")
	require.NoError(t, err)
	assert.NotContains(t, log, patternSecret)
	assert.NotContains(t, log, envValue)
	assert.Contains(t, log, headlessLogRedactedValue)

	require.NoError(t, store.SaveHeadlessRun(HeadlessRun{
		ID:          "run-private",
		Prompt:      rawPrompt,
		PrivateLogs: true,
		Status:      HeadlessStatusRunning,
	}))
	require.NoError(t, store.AppendHeadlessLog("run-private", "env="+envValue+"\n"))

	privateRun, err := store.LoadHeadlessRun("run-private")
	require.NoError(t, err)
	assert.Contains(t, privateRun.Prompt, envValue)

	privateLog, err := store.ReadHeadlessLog("run-private")
	require.NoError(t, err)
	assert.Contains(t, privateLog, envValue)
}

func TestStore_HeadlessConcurrentAppendRead(t *testing.T) {
	t.Parallel()

	store := NewStore(t.TempDir())
	policy := HeadlessLogPolicy{MaxChunkBytes: 64, MaxChunks: 100}
	errs := make(chan error, 128)
	done := make(chan struct{})

	var reader sync.WaitGroup
	reader.Go(func() {
		offset := HeadlessLogOffset{}

		for {
			select {
			case <-done:
				return
			default:
			}

			tail, err := store.TailHeadlessLog("run-concurrent", HeadlessLogTailOptions{Offset: offset, MaxBytes: 32})
			if err != nil {
				errs <- err
				return
			}

			offset = tail.NextOffset
		}
	})

	var writers sync.WaitGroup

	for writer := range 8 {
		writers.Go(func() {
			for line := range 20 {
				text := fmt.Sprintf("writer-%d-line-%d\n", writer, line)
				options := HeadlessLogWriteOptions{Policy: policy, Private: true}

				if err := store.AppendHeadlessLogWithOptions("run-concurrent", text, options); err != nil {
					errs <- err
					return
				}
			}
		})
	}

	writers.Wait()
	close(done)
	reader.Wait()
	close(errs)

	for err := range errs {
		require.NoError(t, err)
	}

	log, err := store.ReadHeadlessLog("run-concurrent")
	require.NoError(t, err)
	assert.Equal(t, 8*20, strings.Count(log, "\n"))
}

func TestStore_RecoverStaleHeadlessRuns(t *testing.T) {
	t.Parallel()

	store := NewStore(t.TempDir())
	staleAt := time.Now().Add(-time.Hour).UTC()
	require.NoError(t, store.SaveHeadlessRun(HeadlessRun{
		StartedAt:       staleAt,
		UpdatedAt:       staleAt,
		LastHeartbeatAt: staleAt,
		ID:              "run-stale",
		PID:             -1,
		Status:          HeadlessStatusRunning,
	}))

	recovered, err := store.RecoverStaleHeadlessRuns(time.Millisecond)
	require.NoError(t, err)
	require.Len(t, recovered, 1)
	assert.Equal(t, HeadlessStatusStale, recovered[0].Status)
	assert.Contains(t, recovered[0].CancellationReason, "no heartbeat")
	require.NotNil(t, recovered[0].ExitCode)
	assert.Equal(t, 1, *recovered[0].ExitCode)

	loaded, err := store.LoadHeadlessRun("run-stale")
	require.NoError(t, err)
	assert.Equal(t, HeadlessStatusStale, loaded.Status)
	assert.True(t, loaded.Stale)

	log, err := store.ReadHeadlessLog("run-stale")
	require.NoError(t, err)
	assert.Contains(t, log, "stale")
}

func TestStore_RecoverStaleHeadlessRunsIgnoresForeignHostPID(t *testing.T) {
	t.Parallel()

	store := NewStore(t.TempDir())
	staleAt := time.Now().Add(-time.Hour).UTC()
	hostname, err := os.Hostname()
	require.NoError(t, err)

	require.NoError(t, store.SaveHeadlessRun(HeadlessRun{
		StartedAt:       staleAt,
		UpdatedAt:       staleAt,
		LastHeartbeatAt: staleAt,
		Hostname:        hostname + "-foreign",
		ID:              "run-foreign-host",
		PID:             os.Getpid(),
		Status:          HeadlessStatusRunning,
	}))

	recovered, err := store.RecoverStaleHeadlessRuns(time.Millisecond)
	require.NoError(t, err)
	require.Len(t, recovered, 1)
	assert.Equal(t, HeadlessStatusStale, recovered[0].Status)
	assert.Contains(t, recovered[0].StaleReason, "no heartbeat")
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

func headlessLogChunkFiles(t *testing.T, store *Store, id string) []string {
	t.Helper()

	entries, err := os.ReadDir(filepath.Join(store.Dir(), "headless"))
	require.NoError(t, err)

	prefix := id + headlessLogChunkPrefix
	files := make([]string, 0)

	for _, entry := range entries {
		if !entry.IsDir() && strings.HasPrefix(entry.Name(), prefix) {
			files = append(files, entry.Name())
		}
	}

	sort.Strings(files)

	return files
}
