package session

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const testWindowsGOOS = "windows"

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
	assert.Equal(t, filepath.Join(store.Dir(), "headless", first.ID+".events.jsonl"), loaded.EventsPath)
	assert.NotEmpty(t, loaded.ArtifactDir)
	assert.NotEmpty(t, loaded.StartedCommand)
	assert.NotEmpty(t, loaded.CommandArgs)
	assert.NotEmpty(t, loaded.CWD)
	assert.Equal(t, "foreground", loaded.StartMethod)
	assert.NotZero(t, loaded.PID)
	assert.NotZero(t, loaded.ParentPID)
	assert.Equal(t, defaultHeadlessLogMaxChunkBytes, loaded.LogMaxChunkBytes)
	assert.Equal(t, defaultHeadlessLogMaxChunks, loaded.LogMaxChunks)
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

func TestStore_SaveHeadlessRunDoesNotInventLocalParentForRecordedPID(t *testing.T) {
	t.Parallel()

	store := NewStore(t.TempDir())

	require.NoError(t, store.SaveHeadlessRun(HeadlessRun{
		ID:     "run-recorded-pid",
		PID:    99999999,
		Status: HeadlessStatusRunning,
	}))

	recorded, err := store.LoadHeadlessRun("run-recorded-pid")
	require.NoError(t, err)
	assert.Equal(t, 99999999, recorded.PID)
	assert.Zero(t, recorded.ParentPID)

	require.NoError(t, store.SaveHeadlessRun(HeadlessRun{
		ID:       "run-foreign-no-pid",
		Hostname: "foreign-host",
		Status:   HeadlessStatusRunning,
	}))

	foreign, err := store.LoadHeadlessRun("run-foreign-no-pid")
	require.NoError(t, err)
	assert.Zero(t, foreign.PID)
	assert.Zero(t, foreign.ParentPID)
	assert.Zero(t, foreign.ProcessGroupID)
	assert.Empty(t, foreign.Owner)
	assert.Empty(t, foreign.StartedCommand)
	assert.Empty(t, foreign.CommandArgs)
	assert.Empty(t, foreign.CWD)
	assert.Empty(t, foreign.StartMethod)
}

func TestStore_SaveHeadlessRunRejectsInvalidRelationships(t *testing.T) {
	t.Parallel()

	store := NewStore(t.TempDir())

	tests := []struct {
		name    string
		wantErr string
		run     HeadlessRun
	}{
		{
			name: "whitespace parent",
			run: HeadlessRun{
				ID:          "run-invalid-parent",
				ParentRunID: " parent ",
				Status:      HeadlessStatusRunning,
			},
			wantErr: "invalid parent headless id",
		},
		{
			name: "self parent",
			run: HeadlessRun{
				ID:          "run-self-parent",
				ParentRunID: "run-self-parent",
				Status:      HeadlessStatusRunning,
			},
			wantErr: "cannot be its own parent",
		},
		{
			name: "whitespace child",
			run: HeadlessRun{
				ID:          "run-invalid-child",
				ChildRunIDs: []string{" child "},
				Status:      HeadlessStatusRunning,
			},
			wantErr: "invalid child headless id",
		},
		{
			name: "self child",
			run: HeadlessRun{
				ID:          "run-self-child",
				ChildRunIDs: []string{"run-self-child"},
				Status:      HeadlessStatusRunning,
			},
			wantErr: "cannot be its own child",
		},
		{
			name: "duplicate child",
			run: HeadlessRun{
				ID:          "run-duplicate-child",
				ChildRunIDs: []string{"run-child", "run-child"},
				Status:      HeadlessStatusRunning,
			},
			wantErr: "duplicate child headless id",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			err := store.SaveHeadlessRun(tt.run)
			require.ErrorContains(t, err, tt.wantErr)
		})
	}
}

func TestStore_SaveNewHeadlessRunRejectsExistingMetadata(t *testing.T) {
	t.Parallel()

	store := NewStore(t.TempDir())
	require.NoError(t, store.SaveNewHeadlessRun(HeadlessRun{
		ID:        "run-new-once",
		SessionID: "session-original",
		Status:    HeadlessStatusCompleted,
	}))

	err := store.SaveNewHeadlessRun(HeadlessRun{
		ID:        "run-new-once",
		SessionID: "session-reused",
		Status:    HeadlessStatusRunning,
	})
	require.ErrorContains(t, err, "already exists")

	loaded, err := store.LoadHeadlessRun("run-new-once")
	require.NoError(t, err)
	assert.Equal(t, "session-original", loaded.SessionID)
	assert.Equal(t, HeadlessStatusCompleted, loaded.Status)
}

func TestStore_SaveNewHeadlessRunRejectsCorruptExistingMetadata(t *testing.T) {
	t.Parallel()

	store := NewStore(t.TempDir())
	require.NoError(t, os.MkdirAll(filepath.Dir(store.headlessJSONPath("run-corrupt-existing")), 0o750))
	require.NoError(t, os.WriteFile(store.headlessJSONPath("run-corrupt-existing"), []byte("{not-json"), 0o600))

	err := store.SaveNewHeadlessRun(HeadlessRun{
		ID:     "run-corrupt-existing",
		Status: HeadlessStatusRunning,
	})
	require.ErrorContains(t, err, "already exists")

	data, err := os.ReadFile(store.headlessJSONPath("run-corrupt-existing"))
	require.NoError(t, err)
	assert.Equal(t, "{not-json", string(data))
}

func TestStore_SaveNewHeadlessRunRejectsRetainedArtifacts(t *testing.T) {
	t.Parallel()

	tests := []struct {
		setup func(*testing.T, *Store, string)
		name  string
	}{
		{
			name: "events",
			setup: func(t *testing.T, store *Store, id string) {
				t.Helper()
				require.NoError(t, os.MkdirAll(store.headlessDir(), 0o750))
				require.NoError(t, os.WriteFile(store.headlessEventsPath(id), []byte("{}\n"), 0o600))
			},
		},
		{
			name: "legacy-log",
			setup: func(t *testing.T, store *Store, id string) {
				t.Helper()
				require.NoError(t, os.MkdirAll(store.headlessDir(), 0o750))
				require.NoError(t, os.WriteFile(store.headlessLogPath(id), []byte("old log\n"), 0o600))
			},
		},
		{
			name: "log-chunk",
			setup: func(t *testing.T, store *Store, id string) {
				t.Helper()
				require.NoError(t, os.MkdirAll(store.headlessDir(), 0o750))
				require.NoError(t, os.WriteFile(store.headlessLogChunkPath(id, 1), []byte("old chunk\n"), 0o600))
			},
		},
		{
			name: "artifact-dir",
			setup: func(t *testing.T, store *Store, id string) {
				t.Helper()
				require.NoError(t, os.MkdirAll(store.headlessArtifactDir(id), 0o750))
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			store := NewStore(t.TempDir())
			id := "run-existing-" + tt.name
			tt.setup(t, store, id)

			err := store.SaveNewHeadlessRun(HeadlessRun{
				ID:     id,
				Status: HeadlessStatusRunning,
			})
			require.ErrorContains(t, err, "already exists")

			_, loadErr := store.LoadHeadlessRun(id)
			require.ErrorIs(t, loadErr, os.ErrNotExist)
		})
	}
}

func TestStore_SaveHeadlessRunRedactsSensitiveCommandArgs(t *testing.T) {
	t.Parallel()

	store := NewStore(t.TempDir())
	require.NoError(t, store.SaveHeadlessRun(HeadlessRun{
		ID:             "run-secret-args",
		StartedCommand: "atteler --api-key secret-value-123456 --token=ghp_" + strings.Repeat("r", 20),
		CommandArgs: []string{
			"atteler",
			"--api-key",
			"secret-value-123456",
			"--token=ghp_" + strings.Repeat("r", 20),
			"--model",
			"gpt-test",
		},
		Status: HeadlessStatusRunning,
	}))

	loaded, err := store.LoadHeadlessRun("run-secret-args")
	require.NoError(t, err)
	assert.Equal(t, "atteler", loaded.CommandArgs[0])
	assert.Equal(t, "--api-key", loaded.CommandArgs[1])
	assert.Equal(t, "[REDACTED]", loaded.CommandArgs[2])
	assert.Equal(t, "--token=[REDACTED]", loaded.CommandArgs[3])
	assert.Equal(t, "--model", loaded.CommandArgs[4])
	assert.Equal(t, "gpt-test", loaded.CommandArgs[5])
	assert.NotContains(t, loaded.StartedCommand, "secret-value-123456")
	assert.NotContains(t, loaded.StartedCommand, "ghp_"+strings.Repeat("r", 20))
	assert.Contains(t, loaded.StartedCommand, "--api-key [REDACTED]")

	require.NoError(t, store.SaveHeadlessRun(HeadlessRun{
		ID:             "run-secret-command-only",
		StartedCommand: "atteler --api-key secret-value-123456 --model gpt-test",
		Status:         HeadlessStatusCompleted,
	}))

	commandOnly, err := store.LoadHeadlessRun("run-secret-command-only")
	require.NoError(t, err)
	assert.NotContains(t, commandOnly.StartedCommand, "secret-value-123456")
	assert.Contains(t, commandOnly.StartedCommand, "--api-key [REDACTED]")
}

func TestStore_LinkHeadlessChildRunRecordsRelationship(t *testing.T) {
	t.Parallel()

	store := NewStore(t.TempDir())
	require.NoError(t, store.SaveHeadlessRun(HeadlessRun{
		ID:     "run-parent",
		Status: HeadlessStatusRunning,
	}))
	require.NoError(t, store.SaveHeadlessRun(HeadlessRun{
		ID:          "run-child",
		ParentRunID: "run-parent",
		Status:      HeadlessStatusRunning,
	}))

	require.NoError(t, store.LinkHeadlessChildRun("run-parent", "run-child"))
	require.NoError(t, store.LinkHeadlessChildRun("run-parent", "run-child"))

	parent, err := store.LoadHeadlessRun("run-parent")
	require.NoError(t, err)
	assert.Equal(t, []string{"run-child"}, parent.ChildRunIDs)

	child, err := store.LoadHeadlessRun("run-child")
	require.NoError(t, err)
	assert.Equal(t, "run-parent", child.ParentRunID)
}

func TestStore_LinkHeadlessChildRunDoesNotInventParentProcessMetadata(t *testing.T) {
	t.Parallel()

	store := NewStore(t.TempDir())
	require.NoError(t, os.MkdirAll(store.headlessDir(), 0o750))
	require.NoError(t, os.WriteFile(
		store.headlessJSONPath("run-minimal-parent"),
		[]byte(`{"id":"run-minimal-parent","status":"running","pid":12345,"hostname":"foreign-host"}`),
		0o600,
	))
	require.NoError(t, store.SaveHeadlessRun(HeadlessRun{
		ID:          "run-minimal-child",
		ParentRunID: "run-minimal-parent",
		Status:      HeadlessStatusRunning,
	}))

	require.NoError(t, store.LinkHeadlessChildRun("run-minimal-parent", "run-minimal-child"))

	parent, err := store.LoadHeadlessRun("run-minimal-parent")
	require.NoError(t, err)
	assert.Equal(t, []string{"run-minimal-child"}, parent.ChildRunIDs)
	assert.Equal(t, 12345, parent.PID)
	assert.Equal(t, "foreign-host", parent.Hostname)
	assert.Zero(t, parent.ParentPID)
	assert.Zero(t, parent.ProcessGroupID)
	assert.Empty(t, parent.StartedCommand)
	assert.Empty(t, parent.CommandArgs)
	assert.Empty(t, parent.CWD)
	assert.Empty(t, parent.StartMethod)
}

func TestStore_LinkHeadlessChildRunRejectsInvalidIDs(t *testing.T) {
	t.Parallel()

	store := NewStore(t.TempDir())
	require.NoError(t, store.SaveHeadlessRun(HeadlessRun{
		ID:     "run-parent-link",
		Status: HeadlessStatusRunning,
	}))

	err := store.LinkHeadlessChildRun(" run-parent-link ", "run-child")
	require.ErrorContains(t, err, "headless id must not have leading or trailing whitespace")

	err = store.LinkHeadlessChildRun("run-parent-link", "run/child")
	require.ErrorContains(t, err, "headless id must be a file name")
}

func TestStore_SaveFinishedHeadlessRunPreservesLinkedChildRuns(t *testing.T) {
	t.Parallel()

	store := NewStore(t.TempDir())
	require.NoError(t, store.SaveHeadlessRun(HeadlessRun{
		ID:           "run-parent-finish",
		ChildRunIDs:  []string{"child-existing"},
		PrivateLogs:  true,
		Status:       HeadlessStatusRunning,
		CommandArgs:  []string{"atteler", "--headless"},
		StartMethod:  "headless",
		Hostname:     "host-a",
		LogMaxChunks: 3,
	}))

	inMemory, err := store.LoadHeadlessRun("run-parent-finish")
	require.NoError(t, err)

	inMemory.ChildRunIDs = []string{"child-existing", "child-in-memory"}

	require.NoError(t, store.LinkHeadlessChildRun("run-parent-finish", "child-linked"))

	completedAt := time.Now().UTC()
	exitCode := 0
	inMemory.Status = HeadlessStatusCompleted
	inMemory.CompletedAt = &completedAt
	inMemory.ExitCode = &exitCode
	inMemory.TerminalReason = "completed"

	finished, wrote, err := store.SaveFinishedHeadlessRun(inMemory)
	require.NoError(t, err)
	require.True(t, wrote)
	assert.Equal(t, HeadlessStatusCompleted, finished.Status)
	assert.ElementsMatch(t, []string{"child-existing", "child-linked", "child-in-memory"}, finished.ChildRunIDs)
	assert.True(t, finished.PrivateLogs)
	assert.Equal(t, "headless", finished.StartMethod)
	assert.Equal(t, []string{"atteler", "--headless"}, finished.CommandArgs)
	assert.Equal(t, 3, finished.LogMaxChunks)
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

func TestStore_HeadlessLogAppendRevivesOrphanedRun(t *testing.T) {
	t.Parallel()

	store := NewStore(t.TempDir())
	orphanedAt := time.Now().Add(-time.Hour).UTC()
	require.NoError(t, store.SaveHeadlessRun(HeadlessRun{
		ID:              "run-log-revives-orphaned",
		LastHeartbeatAt: orphanedAt,
		OrphanedReason:  "no heartbeat since " + orphanedAt.Format(time.RFC3339),
		Status:          HeadlessStatusOrphaned,
		Stale:           true,
	}))

	require.NoError(t, store.AppendHeadlessLog("run-log-revives-orphaned", "still alive\n"))

	loaded, err := store.LoadHeadlessRun("run-log-revives-orphaned")
	require.NoError(t, err)
	assert.Equal(t, HeadlessStatusRunning, loaded.Status)
	assert.False(t, loaded.Stale)
	assert.Empty(t, loaded.OrphanedReason)
	assert.True(t, loaded.LastHeartbeatAt.After(orphanedAt))
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

func TestStore_TailHeadlessLogMigratesLegacyLog(t *testing.T) {
	t.Parallel()

	store := NewStore(t.TempDir())
	require.NoError(t, os.MkdirAll(filepath.Join(store.Dir(), "headless"), 0o750))

	legacyPath := store.headlessLogPath("run-legacy-tail")
	require.NoError(t, os.WriteFile(legacyPath, []byte("legacy log tail\n"), 0o600))

	tail, err := store.TailHeadlessLog("run-legacy-tail", HeadlessLogTailOptions{MaxBytes: 7})
	require.NoError(t, err)
	assert.Equal(t, "legacy ", tail.Text)
	assert.Equal(t, HeadlessLogOffset{Chunk: 1, Byte: 7}, tail.NextOffset)

	_, err = os.Stat(legacyPath)
	require.ErrorIs(t, err, os.ErrNotExist)
	assert.Equal(t, []string{"run-legacy-tail.log.000001"}, headlessLogChunkFiles(t, store, "run-legacy-tail"))
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

func TestStore_TailHeadlessLogAppliesRecordedRetentionToExistingChunks(t *testing.T) {
	t.Parallel()

	store := NewStore(t.TempDir())
	id := "run-tail-retain-existing"
	require.NoError(t, store.SaveHeadlessRun(HeadlessRun{
		ID:               id,
		LogMaxChunkBytes: 8,
		LogMaxChunks:     2,
		Status:           HeadlessStatusCompleted,
	}))

	require.NoError(t, os.WriteFile(store.headlessLogChunkPath(id, 1), []byte("drop-001\n"), 0o600))
	require.NoError(t, os.WriteFile(store.headlessLogChunkPath(id, 2), []byte("keep-002\n"), 0o600))
	require.NoError(t, os.WriteFile(store.headlessLogChunkPath(id, 3), []byte("keep-003\n"), 0o600))

	tail, err := store.TailHeadlessLog(id, HeadlessLogTailOptions{MaxBytes: 64})
	require.NoError(t, err)
	assert.Equal(t, "keep-002\nkeep-003\n", tail.Text)
	assert.Equal(t, HeadlessLogOffset{Chunk: 2}, tail.RetainedOffset)
	assert.Equal(t, []string{
		"run-tail-retain-existing.log.000002",
		"run-tail-retain-existing.log.000003",
	}, headlessLogChunkFiles(t, store, id))
}

func TestStore_HeadlessLogAppendUsesRecordedPolicy(t *testing.T) {
	t.Parallel()

	store := NewStore(t.TempDir())
	require.NoError(t, store.SaveHeadlessRun(HeadlessRun{
		ID:               "run-recorded-log-policy",
		LogMaxChunkBytes: 5,
		LogMaxChunks:     2,
		Status:           HeadlessStatusCompleted,
	}))

	require.NoError(t, store.AppendHeadlessLog("run-recorded-log-policy", "aaaaabbbbbccccc"))

	chunks := headlessLogChunkFiles(t, store, "run-recorded-log-policy")
	assert.Equal(t, []string{"run-recorded-log-policy.log.000002", "run-recorded-log-policy.log.000003"}, chunks)

	log, err := store.ReadHeadlessLog("run-recorded-log-policy")
	require.NoError(t, err)
	assert.Equal(t, "bbbbbccccc", log)
}

func TestStore_HeadlessLogReadMigratesLegacyWithRecordedPolicy(t *testing.T) {
	t.Parallel()

	store := NewStore(t.TempDir())
	require.NoError(t, store.SaveHeadlessRun(HeadlessRun{
		ID:               "run-read-legacy-policy",
		LogMaxChunkBytes: 5,
		LogMaxChunks:     2,
		PrivateLogs:      true,
		Status:           HeadlessStatusCompleted,
	}))

	require.NoError(t, os.WriteFile(
		store.headlessLogPath("run-read-legacy-policy"),
		[]byte("aaaaabbbbbccccc"),
		0o600,
	))

	log, err := store.ReadHeadlessLog("run-read-legacy-policy")
	require.NoError(t, err)
	assert.Equal(t, "bbbbbccccc", log)
	assert.Equal(t, []string{
		"run-read-legacy-policy.log.000001",
		"run-read-legacy-policy.log.000002",
	}, headlessLogChunkFiles(t, store, "run-read-legacy-policy"))
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

func TestStore_HeadlessLogMigratesLegacyPrivateLogWithoutRedaction(t *testing.T) {
	t.Parallel()

	store := NewStore(t.TempDir())
	policy := HeadlessLogPolicy{MaxChunkBytes: 128, MaxChunks: 1}
	secret := "sk-" + strings.Repeat("p", 20)
	legacyID := "run-legacy-private"

	require.NoError(t, store.SaveHeadlessRun(HeadlessRun{
		ID:          legacyID,
		PrivateLogs: true,
		Status:      HeadlessStatusCompleted,
	}))
	require.NoError(t, os.WriteFile(
		store.headlessLogPath(legacyID),
		[]byte("legacy token="+secret+"\n"),
		0o600,
	))

	require.NoError(t, store.AppendHeadlessLogWithOptions(
		legacyID,
		"new token="+secret+"\n",
		HeadlessLogWriteOptions{Policy: policy},
	))

	log, err := store.ReadHeadlessLog(legacyID)
	require.NoError(t, err)
	assert.Contains(t, log, "legacy token="+secret)
	assert.Contains(t, log, "new token="+secret)
	assert.NotContains(t, log, headlessLogRedactedValue)
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
	rawSessionPath := filepath.Join(t.TempDir(), envValue, "session.json")
	rawLogPath := filepath.Join(t.TempDir(), envValue, "run.log")
	rawEventsPath := filepath.Join(t.TempDir(), envValue, "run.events.jsonl")
	rawArtifactDir := filepath.Join(t.TempDir(), envValue, "artifacts")
	rawCWD := filepath.Join(t.TempDir(), envValue)
	require.NoError(t, store.SaveHeadlessRun(HeadlessRun{
		ID:          "run-redacted",
		SessionPath: rawSessionPath,
		LogPath:     rawLogPath,
		EventsPath:  rawEventsPath,
		ArtifactDir: rawArtifactDir,
		CWD:         rawCWD,
		Prompt:      rawPrompt,
		Error:       rawError,
		Status:      HeadlessStatusFailed,
	}))

	loaded, err := store.LoadHeadlessRun("run-redacted")
	require.NoError(t, err)
	assert.NotContains(t, loaded.Prompt, envValue)
	assert.NotContains(t, loaded.Error, strings.Repeat("a", 16))
	assert.NotContains(t, loaded.SessionPath, envValue)
	assert.NotContains(t, loaded.LogPath, envValue)
	assert.NotContains(t, loaded.EventsPath, envValue)
	assert.NotContains(t, loaded.ArtifactDir, envValue)
	assert.NotContains(t, loaded.CWD, envValue)
	assert.Contains(t, loaded.Prompt, headlessLogRedactedValue)
	assert.Contains(t, loaded.Error, headlessLogRedactedValue)
	assert.Contains(t, loaded.SessionPath, headlessLogRedactedValue)
	assert.Contains(t, loaded.LogPath, headlessLogRedactedValue)
	assert.Contains(t, loaded.EventsPath, headlessLogRedactedValue)
	assert.Contains(t, loaded.ArtifactDir, headlessLogRedactedValue)
	assert.Contains(t, loaded.CWD, headlessLogRedactedValue)
	require.DirExists(t, rawArtifactDir)

	_, err = os.Stat(loaded.ArtifactDir)
	require.ErrorIs(t, err, os.ErrNotExist)

	patternSecret := "sk-" + strings.Repeat("b", 20)
	require.NoError(t, store.AppendHeadlessLog("run-redacted", "api_key="+patternSecret+" env="+envValue+"\n"))
	log, err := store.ReadHeadlessLog("run-redacted")
	require.NoError(t, err)
	assert.NotContains(t, log, patternSecret)
	assert.NotContains(t, log, envValue)
	assert.Contains(t, log, headlessLogRedactedValue)

	require.NoError(t, store.AppendHeadlessEvent("run-redacted", HeadlessEvent{
		Type:           HeadlessEventFailed,
		SessionPath:    filepath.Join(t.TempDir(), envValue, "session.json"),
		Message:        "token=" + envValue,
		Error:          "Authorization: Bearer " + strings.Repeat("c", 16),
		CWD:            filepath.Join(t.TempDir(), envValue),
		StartedCommand: "atteler --api-key " + envValue,
		TerminalReason: "terminal token=" + envValue,
		CancelReason:   "cancel token=" + envValue,
		StaleReason:    "stale token=" + envValue,
		OrphanedReason: "orphan token=" + envValue,
		CommandArgs:    []string{"atteler", "--api-key", envValue},
		Metadata: map[string]string{
			"token": envValue,
		},
	}))
	events, err := store.ReadHeadlessEvents("run-redacted")
	require.NoError(t, err)
	require.Len(t, events, 1)
	assert.NotContains(t, events[0].Message, envValue)
	assert.NotContains(t, events[0].Error, strings.Repeat("c", 16))
	assert.NotContains(t, events[0].SessionPath, envValue)
	assert.NotContains(t, events[0].CWD, envValue)
	assert.NotContains(t, events[0].StartedCommand, envValue)
	assert.NotContains(t, events[0].TerminalReason, envValue)
	assert.NotContains(t, events[0].CancelReason, envValue)
	assert.NotContains(t, events[0].StaleReason, envValue)
	assert.NotContains(t, events[0].OrphanedReason, envValue)
	require.Len(t, events[0].CommandArgs, 3)
	assert.Equal(t, headlessLogRedactedValue, events[0].CommandArgs[2])
	assert.NotContains(t, events[0].Metadata["token"], envValue)
	assert.Contains(t, events[0].Message, headlessLogRedactedValue)
	assert.Contains(t, events[0].Error, headlessLogRedactedValue)
	assert.Contains(t, events[0].SessionPath, headlessLogRedactedValue)
	assert.Contains(t, events[0].CWD, headlessLogRedactedValue)
	assert.Contains(t, events[0].StartedCommand, headlessLogRedactedValue)
	assert.Contains(t, events[0].TerminalReason, headlessLogRedactedValue)
	assert.Contains(t, events[0].CancelReason, headlessLogRedactedValue)
	assert.Contains(t, events[0].StaleReason, headlessLogRedactedValue)
	assert.Contains(t, events[0].OrphanedReason, headlessLogRedactedValue)
	assert.Contains(t, events[0].Metadata["token"], headlessLogRedactedValue)

	require.NoError(t, store.SaveHeadlessRun(HeadlessRun{
		ID:          "run-private",
		SessionPath: filepath.Join(t.TempDir(), envValue, "session.json"),
		LogPath:     filepath.Join(t.TempDir(), envValue, "run.log"),
		EventsPath:  filepath.Join(t.TempDir(), envValue, "run.events.jsonl"),
		ArtifactDir: filepath.Join(t.TempDir(), envValue, "artifacts"),
		Prompt:      rawPrompt,
		PrivateLogs: true,
		Status:      HeadlessStatusRunning,
	}))
	require.NoError(t, store.AppendHeadlessLog("run-private", "env="+envValue+"\n"))

	privateRun, err := store.LoadHeadlessRun("run-private")
	require.NoError(t, err)
	assert.Contains(t, privateRun.Prompt, envValue)
	assert.Contains(t, privateRun.SessionPath, envValue)
	assert.Contains(t, privateRun.LogPath, envValue)
	assert.Contains(t, privateRun.EventsPath, envValue)
	assert.Contains(t, privateRun.ArtifactDir, envValue)

	privateLog, err := store.ReadHeadlessLog("run-private")
	require.NoError(t, err)
	assert.Contains(t, privateLog, envValue)

	require.NoError(t, store.AppendHeadlessEvent("run-private", HeadlessEvent{
		Type:           HeadlessEventFailed,
		SessionPath:    filepath.Join(t.TempDir(), envValue, "session.json"),
		Message:        "token=" + envValue,
		Error:          "Authorization: Bearer " + strings.Repeat("d", 16),
		CWD:            filepath.Join(t.TempDir(), envValue),
		StartedCommand: "atteler --api-key " + envValue,
		TerminalReason: "terminal token=" + envValue,
		CancelReason:   "cancel token=" + envValue,
		StaleReason:    "stale token=" + envValue,
		OrphanedReason: "orphan token=" + envValue,
		CommandArgs:    []string{"atteler", "--api-key", envValue},
		Metadata: map[string]string{
			"token": envValue,
		},
	}))
	privateEvents, err := store.ReadHeadlessEvents("run-private")
	require.NoError(t, err)
	require.Len(t, privateEvents, 1)
	assert.Contains(t, privateEvents[0].Message, envValue)
	assert.Contains(t, privateEvents[0].Error, strings.Repeat("d", 16))
	assert.Contains(t, privateEvents[0].SessionPath, envValue)
	assert.Contains(t, privateEvents[0].CWD, envValue)
	assert.Contains(t, privateEvents[0].StartedCommand, envValue)
	assert.Contains(t, privateEvents[0].TerminalReason, envValue)
	assert.Contains(t, privateEvents[0].CancelReason, envValue)
	assert.Contains(t, privateEvents[0].StaleReason, envValue)
	assert.Contains(t, privateEvents[0].OrphanedReason, envValue)
	require.Len(t, privateEvents[0].CommandArgs, 3)
	assert.Equal(t, envValue, privateEvents[0].CommandArgs[2])
	assert.Equal(t, envValue, privateEvents[0].Metadata["token"])

	require.NoError(t, store.SaveHeadlessRun(HeadlessRun{
		ID:       "run-cancel-redacted",
		Hostname: "foreign-host",
		PID:      os.Getpid(),
		Status:   HeadlessStatusRunning,
	}))
	_, err = store.CancelHeadlessRun("run-cancel-redacted", "token="+envValue)
	require.NoError(t, err)
	cancelLog, err := store.ReadHeadlessLog("run-cancel-redacted")
	require.NoError(t, err)
	assert.NotContains(t, cancelLog, envValue)
	assert.Contains(t, cancelLog, headlessLogRedactedValue)

	cancelEvents, err := store.ReadHeadlessEvents("run-cancel-redacted")
	require.NoError(t, err)
	require.Len(t, cancelEvents, 1)
	assert.NotContains(t, cancelEvents[0].Message, envValue)
	assert.Contains(t, cancelEvents[0].Message, headlessLogRedactedValue)

	require.NoError(t, store.SaveHeadlessRun(HeadlessRun{
		ID:          "run-cancel-private",
		Hostname:    "foreign-host",
		PID:         os.Getpid(),
		Status:      HeadlessStatusRunning,
		PrivateLogs: true,
	}))
	_, err = store.CancelHeadlessRun("run-cancel-private", "token="+envValue)
	require.NoError(t, err)
	privateCancelLog, err := store.ReadHeadlessLog("run-cancel-private")
	require.NoError(t, err)
	assert.Contains(t, privateCancelLog, envValue)

	privateCancelEvents, err := store.ReadHeadlessEvents("run-cancel-private")
	require.NoError(t, err)
	require.Len(t, privateCancelEvents, 1)
	assert.Contains(t, privateCancelEvents[0].Message, envValue)
}

func TestRedactHeadlessTextRedactsOverlappingEnvSecrets(t *testing.T) {
	shortSecret := "overlap-secret-123456"
	longSecret := shortSecret + "-789"
	t.Setenv("ATTELER_SHORT_TOKEN", shortSecret)
	t.Setenv("ATTELER_LONG_TOKEN", longSecret)

	redacted := RedactHeadlessText("saw " + longSecret + " and " + shortSecret)

	assert.Equal(t, "saw "+headlessLogRedactedValue+" and "+headlessLogRedactedValue, redacted)
	assert.NotContains(t, redacted, shortSecret)
	assert.NotContains(t, redacted, longSecret)
	assert.NotContains(t, redacted, "-789")
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
		Hostname:        "foreign-host",
		ID:              "run-stale",
		PID:             os.Getpid(),
		Status:          HeadlessStatusRunning,
	}))

	recovered, err := store.RecoverStaleHeadlessRuns(time.Millisecond)
	require.NoError(t, err)
	require.Len(t, recovered, 1)
	assert.Equal(t, HeadlessStatusStale, recovered[0].Status)
	assert.Contains(t, recovered[0].StaleReason, "no heartbeat")
	assert.Contains(t, recovered[0].TerminalReason, "no heartbeat")
	assert.Empty(t, recovered[0].CancellationReason)
	require.NotNil(t, recovered[0].ExitCode)
	assert.Equal(t, 1, *recovered[0].ExitCode)

	loaded, err := store.LoadHeadlessRun("run-stale")
	require.NoError(t, err)
	assert.Equal(t, HeadlessStatusStale, loaded.Status)
	assert.True(t, loaded.Stale)

	log, err := store.ReadHeadlessLog("run-stale")
	require.NoError(t, err)
	assert.Contains(t, log, "stale")

	events, err := store.ReadHeadlessEvents("run-stale")
	require.NoError(t, err)
	require.Len(t, events, 1)
	assert.Equal(t, HeadlessEventStale, events[0].Type)
	assert.Contains(t, events[0].StaleReason, "no heartbeat")
	assert.Contains(t, events[0].TerminalReason, "no heartbeat")
}

func TestStore_RecoverStaleHeadlessRunsIsIdempotent(t *testing.T) {
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
		ID:              "run-recover-idempotent",
		PID:             os.Getpid(),
		Status:          HeadlessStatusRunning,
	}))

	first, err := store.RecoverStaleHeadlessRuns(time.Millisecond)
	require.NoError(t, err)
	require.Len(t, first, 1)
	assert.Equal(t, HeadlessStatusStale, first[0].Status)

	events, err := store.ReadHeadlessEvents("run-recover-idempotent")
	require.NoError(t, err)
	require.Len(t, events, 1)
	assert.Equal(t, HeadlessEventStale, events[0].Type)

	log, err := store.ReadHeadlessLog("run-recover-idempotent")
	require.NoError(t, err)
	assert.Equal(t, 1, strings.Count(log, "stale\t"))

	second, err := store.RecoverStaleHeadlessRuns(time.Millisecond)
	require.NoError(t, err)
	assert.Empty(t, second)

	events, err = store.ReadHeadlessEvents("run-recover-idempotent")
	require.NoError(t, err)
	require.Len(t, events, 1)

	log, err = store.ReadHeadlessLog("run-recover-idempotent")
	require.NoError(t, err)
	assert.Equal(t, 1, strings.Count(log, "stale\t"))
}

func TestStore_ListHeadlessRunsReconcilesStaleRuns(t *testing.T) {
	t.Parallel()

	store := NewStore(t.TempDir())
	staleAt := time.Now().Add(-time.Hour).UTC()

	require.NoError(t, store.SaveHeadlessRun(HeadlessRun{
		StartedAt:       staleAt,
		UpdatedAt:       staleAt,
		LastHeartbeatAt: staleAt,
		Hostname:        "foreign-host",
		ID:              "run-list-stale",
		PID:             os.Getpid(),
		Status:          HeadlessStatusRunning,
	}))

	runs, err := store.ListHeadlessRuns()
	require.NoError(t, err)
	require.Len(t, runs, 1)
	assert.Equal(t, HeadlessStatusStale, runs[0].Status)
	assert.Contains(t, runs[0].StaleReason, "no heartbeat")

	loaded, err := store.LoadHeadlessRun("run-list-stale")
	require.NoError(t, err)
	assert.Equal(t, HeadlessStatusStale, loaded.Status)

	log, err := store.ReadHeadlessLog("run-list-stale")
	require.NoError(t, err)
	assert.Contains(t, log, "stale")
}

func TestStore_HeadlessRunStatusReconcilesStaleRun(t *testing.T) {
	t.Parallel()

	store := NewStore(t.TempDir())
	staleAt := time.Now().Add(-time.Hour).UTC()

	require.NoError(t, store.SaveHeadlessRun(HeadlessRun{
		StartedAt:       staleAt,
		UpdatedAt:       staleAt,
		LastHeartbeatAt: staleAt,
		Hostname:        "foreign-host",
		ID:              "run-status-stale",
		PID:             os.Getpid(),
		Status:          HeadlessStatusRunning,
	}))

	run, err := store.HeadlessRunStatus("run-status-stale")
	require.NoError(t, err)
	assert.Equal(t, HeadlessStatusStale, run.Status)
	assert.Contains(t, run.StaleReason, "no heartbeat")

	loaded, err := store.LoadHeadlessRun("run-status-stale")
	require.NoError(t, err)
	assert.Equal(t, HeadlessStatusStale, loaded.Status)

	events, err := store.ReadHeadlessEvents("run-status-stale")
	require.NoError(t, err)
	require.Len(t, events, 1)
	assert.Equal(t, HeadlessEventStale, events[0].Type)
}

func TestStore_HeadlessRunStatusDoesNotReconcileTerminalRuns(t *testing.T) {
	t.Parallel()

	store := NewStore(t.TempDir())
	staleAt := time.Now().Add(-time.Hour).UTC()

	deadPID := 99999999
	if headlessProcessAlive(deadPID) {
		t.Skipf("test pid %d unexpectedly exists", deadPID)
	}

	tests := []struct {
		status HeadlessStatus
		name   string
	}{
		{name: "completed", status: HeadlessStatusCompleted},
		{name: "failed", status: HeadlessStatusFailed},
		{name: "canceled", status: HeadlessStatusCanceled},
		{name: "timed_out", status: HeadlessStatusTimedOut},
		{name: "superseded", status: HeadlessStatusSuperseded},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			id := "run-terminal-status-" + tt.name
			require.NoError(t, store.SaveHeadlessRun(HeadlessRun{
				ID:              id,
				LastHeartbeatAt: staleAt,
				Hostname:        "foreign-host",
				PID:             deadPID,
				Status:          tt.status,
			}))

			status, err := store.HeadlessRunStatus(id)
			require.NoError(t, err)
			assert.Equal(t, tt.status, status.Status)
			assert.Empty(t, status.StaleReason)
			assert.Empty(t, status.OrphanedReason)

			loaded, err := store.LoadHeadlessRun(id)
			require.NoError(t, err)
			assert.Equal(t, tt.status, loaded.Status)

			events, err := store.ReadHeadlessEvents(id)
			require.NoError(t, err)
			assert.Empty(t, events)
		})
	}
}

func TestStore_ListHeadlessRunsMarksMissingActivityStale(t *testing.T) {
	t.Parallel()

	store := NewStore(t.TempDir())
	require.NoError(t, os.MkdirAll(filepath.Join(store.Dir(), "headless"), 0o750))
	require.NoError(t, os.WriteFile(
		store.headlessJSONPath("run-missing-activity"),
		[]byte(`{"id":"run-missing-activity","status":"running"}`+"\n"),
		0o600,
	))

	runs, err := store.ListHeadlessRuns()
	require.NoError(t, err)
	require.Len(t, runs, 1)
	assert.Equal(t, HeadlessStatusStale, runs[0].Status)
	assert.Equal(t, "no process pid recorded", runs[0].StaleReason)
}

func TestStore_ListHeadlessRunsMarksDeadLocalPIDStale(t *testing.T) {
	t.Parallel()

	store := NewStore(t.TempDir())
	hostname, err := os.Hostname()
	require.NoError(t, err)

	deadPID := 99999999
	if headlessProcessAlive(deadPID) {
		t.Skipf("test pid %d unexpectedly exists", deadPID)
	}

	require.NoError(t, store.SaveHeadlessRun(HeadlessRun{
		ID:        "run-dead-pid",
		Hostname:  hostname,
		PID:       deadPID,
		Status:    HeadlessStatusRunning,
		StartedAt: time.Now().UTC(),
	}))

	runs, err := store.ListHeadlessRuns()
	require.NoError(t, err)
	require.Len(t, runs, 1)
	assert.Equal(t, HeadlessStatusStale, runs[0].Status)
	assert.Contains(t, runs[0].StaleReason, "process pid")
}

func TestStore_ListHeadlessRunsMarksPIDWithMismatchedProcessGroupStale(t *testing.T) {
	t.Parallel()

	processGroupID := headlessProcessGroupID(os.Getpid())
	if processGroupID == 0 {
		t.Skip("process group lookup is unavailable on this platform")
	}

	store := NewStore(t.TempDir())
	hostname, err := os.Hostname()
	require.NoError(t, err)

	require.NoError(t, store.SaveHeadlessRun(HeadlessRun{
		ID:              "run-pgid-mismatch",
		Hostname:        hostname,
		PID:             os.Getpid(),
		ProcessGroupID:  processGroupID + 999999,
		LastHeartbeatAt: time.Now().UTC(),
		Status:          HeadlessStatusRunning,
	}))

	runs, err := store.ListHeadlessRuns()
	require.NoError(t, err)
	require.Len(t, runs, 1)
	assert.Equal(t, HeadlessStatusStale, runs[0].Status)
	assert.Contains(t, runs[0].StaleReason, "process group")
	assert.Empty(t, runs[0].CancellationReason)
}

func TestStore_ListHeadlessRunsMarksAlivePIDWithStaleHeartbeatOrphaned(t *testing.T) {
	t.Parallel()

	store := NewStore(t.TempDir())
	hostname, err := os.Hostname()
	require.NoError(t, err)

	staleAt := time.Now().Add(-time.Hour).UTC()
	require.NoError(t, store.SaveHeadlessRun(HeadlessRun{
		ID:              "run-alive-stale-heartbeat",
		StartedAt:       staleAt,
		UpdatedAt:       staleAt,
		LastHeartbeatAt: staleAt,
		Hostname:        hostname,
		PID:             os.Getpid(),
		Status:          HeadlessStatusRunning,
	}))

	runs, err := store.ListHeadlessRuns()
	require.NoError(t, err)
	require.Len(t, runs, 1)
	assert.Equal(t, HeadlessStatusOrphaned, runs[0].Status)
	assert.Contains(t, runs[0].OrphanedReason, "no heartbeat since")
}

func TestStore_HeartbeatHeadlessRunUpdatesRunningRun(t *testing.T) {
	t.Parallel()

	store := NewStore(t.TempDir())
	oldHeartbeat := time.Now().Add(-time.Hour).UTC()
	require.NoError(t, store.SaveHeadlessRun(HeadlessRun{
		ID:              "run-heartbeat",
		LastHeartbeatAt: oldHeartbeat,
		Status:          HeadlessStatusRunning,
	}))

	require.NoError(t, store.HeartbeatHeadlessRun("run-heartbeat"))

	loaded, err := store.LoadHeadlessRun("run-heartbeat")
	require.NoError(t, err)
	assert.Equal(t, HeadlessStatusRunning, loaded.Status)
	assert.True(t, loaded.LastHeartbeatAt.After(oldHeartbeat))
}

func TestStore_HeartbeatHeadlessRunDoesNotUpdateTerminalRun(t *testing.T) {
	t.Parallel()

	store := NewStore(t.TempDir())
	oldHeartbeat := time.Now().Add(-time.Hour).UTC()
	require.NoError(t, store.SaveHeadlessRun(HeadlessRun{
		ID:              "run-terminal-heartbeat",
		LastHeartbeatAt: oldHeartbeat,
		Status:          HeadlessStatusCompleted,
	}))

	require.NoError(t, store.HeartbeatHeadlessRun("run-terminal-heartbeat"))

	loaded, err := store.LoadHeadlessRun("run-terminal-heartbeat")
	require.NoError(t, err)
	assert.Equal(t, oldHeartbeat, loaded.LastHeartbeatAt)
}

func TestStore_SaveHeadlessRunClearsCancellationFieldsForNonCanceledRuns(t *testing.T) {
	t.Parallel()

	store := NewStore(t.TempDir())
	canceledAt := time.Now().UTC()
	require.NoError(t, store.SaveHeadlessRun(HeadlessRun{
		ID:                 "run-non-canceled-reason",
		Status:             HeadlessStatusFailed,
		CanceledAt:         &canceledAt,
		CancellationReason: "stale legacy reason",
	}))

	loaded, err := store.LoadHeadlessRun("run-non-canceled-reason")
	require.NoError(t, err)
	assert.Nil(t, loaded.CanceledAt)
	assert.Empty(t, loaded.CancellationReason)
}

func TestStore_SaveHeadlessRunClearsTerminalFieldsForNonTerminalRuns(t *testing.T) {
	t.Parallel()

	store := NewStore(t.TempDir())
	completedAt := time.Now().Add(-time.Minute).UTC()
	exitCode := 1

	for _, tc := range []struct {
		name   string
		status HeadlessStatus
	}{
		{name: "running", status: HeadlessStatusRunning},
		{name: "orphaned", status: HeadlessStatusOrphaned},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			require.NoError(t, store.SaveHeadlessRun(HeadlessRun{
				ID:                 "run-clear-terminal-" + tc.name,
				Status:             tc.status,
				CompletedAt:        &completedAt,
				CanceledAt:         &completedAt,
				ExitCode:           &exitCode,
				Error:              "previous failure",
				TerminalReason:     "previous terminal reason",
				CancellationReason: "previous cancellation",
				StaleReason:        "previous stale reason",
				OrphanedReason:     "previous orphaned reason",
			}))

			loaded, err := store.LoadHeadlessRun("run-clear-terminal-" + tc.name)
			require.NoError(t, err)
			assert.Nil(t, loaded.CompletedAt)
			assert.Nil(t, loaded.CanceledAt)
			assert.Nil(t, loaded.ExitCode)
			assert.Empty(t, loaded.Error)
			assert.Empty(t, loaded.TerminalReason)
			assert.Empty(t, loaded.CancellationReason)
			assert.Empty(t, loaded.StaleReason)

			if tc.status == HeadlessStatusOrphaned {
				assert.True(t, loaded.Stale)
				assert.Equal(t, "previous orphaned reason", loaded.OrphanedReason)
			} else {
				assert.False(t, loaded.Stale)
				assert.Empty(t, loaded.OrphanedReason)
			}
		})
	}
}

func TestStore_HeartbeatHeadlessRunRevivesOrphanedRun(t *testing.T) {
	t.Parallel()

	store := NewStore(t.TempDir())
	oldHeartbeat := time.Now().Add(-time.Hour).UTC()
	require.NoError(t, store.SaveHeadlessRun(HeadlessRun{
		ID:              "run-orphaned-heartbeat",
		LastHeartbeatAt: oldHeartbeat,
		OrphanedReason:  "no heartbeat since " + oldHeartbeat.Format(time.RFC3339),
		Status:          HeadlessStatusOrphaned,
		Stale:           true,
	}))

	require.NoError(t, store.HeartbeatHeadlessRun("run-orphaned-heartbeat"))

	loaded, err := store.LoadHeadlessRun("run-orphaned-heartbeat")
	require.NoError(t, err)
	assert.Equal(t, HeadlessStatusRunning, loaded.Status)
	assert.False(t, loaded.Stale)
	assert.Empty(t, loaded.OrphanedReason)
	assert.True(t, loaded.LastHeartbeatAt.After(oldHeartbeat))
}

func TestStore_ListHeadlessRunsMarksDeadOrphanedPIDStale(t *testing.T) {
	t.Parallel()

	store := NewStore(t.TempDir())
	hostname, err := os.Hostname()
	require.NoError(t, err)

	deadPID := 99999999
	if headlessProcessAlive(deadPID) {
		t.Skipf("test pid %d unexpectedly exists", deadPID)
	}

	orphanedAt := time.Now().Add(-time.Hour).UTC()
	require.NoError(t, store.SaveHeadlessRun(HeadlessRun{
		ID:              "run-dead-orphaned-pid",
		StartedAt:       orphanedAt,
		LastHeartbeatAt: orphanedAt,
		Hostname:        hostname,
		PID:             deadPID,
		OrphanedReason:  "no heartbeat since " + orphanedAt.Format(time.RFC3339),
		Status:          HeadlessStatusOrphaned,
		Stale:           true,
	}))

	runs, err := store.ListHeadlessRuns()
	require.NoError(t, err)
	require.Len(t, runs, 1)
	assert.Equal(t, HeadlessStatusStale, runs[0].Status)
	assert.Contains(t, runs[0].StaleReason, "process pid")
	assert.Empty(t, runs[0].OrphanedReason)

	events, err := store.ReadHeadlessEvents("run-dead-orphaned-pid")
	require.NoError(t, err)
	require.Len(t, events, 1)
	assert.Equal(t, HeadlessEventStale, events[0].Type)
	assert.Empty(t, events[0].OrphanedReason)
}

func TestStore_RecoverStaleHeadlessRunsMarksDeadOrphanedPIDStale(t *testing.T) {
	t.Parallel()

	store := NewStore(t.TempDir())
	hostname, err := os.Hostname()
	require.NoError(t, err)

	deadPID := 99999999
	if headlessProcessAlive(deadPID) {
		t.Skipf("test pid %d unexpectedly exists", deadPID)
	}

	orphanedAt := time.Now().Add(-time.Hour).UTC()
	require.NoError(t, store.SaveHeadlessRun(HeadlessRun{
		ID:              "run-recover-dead-orphaned-pid",
		StartedAt:       orphanedAt,
		LastHeartbeatAt: orphanedAt,
		Hostname:        hostname,
		PID:             deadPID,
		OrphanedReason:  "no heartbeat since " + orphanedAt.Format(time.RFC3339),
		Status:          HeadlessStatusOrphaned,
		Stale:           true,
	}))

	recovered, err := store.RecoverStaleHeadlessRuns(time.Millisecond)
	require.NoError(t, err)
	require.Len(t, recovered, 1)
	assert.Equal(t, HeadlessStatusStale, recovered[0].Status)
	assert.Contains(t, recovered[0].StaleReason, "process pid")
	assert.Empty(t, recovered[0].OrphanedReason)

	loaded, err := store.LoadHeadlessRun("run-recover-dead-orphaned-pid")
	require.NoError(t, err)
	assert.Equal(t, HeadlessStatusStale, loaded.Status)

	events, err := store.ReadHeadlessEvents("run-recover-dead-orphaned-pid")
	require.NoError(t, err)
	require.Len(t, events, 1)
	assert.Equal(t, HeadlessEventStale, events[0].Type)
}

func TestStore_SaveFinishedHeadlessRunPreservesCanceledStatus(t *testing.T) {
	t.Parallel()

	store := NewStore(t.TempDir())
	canceledAt := time.Now().UTC()
	exitCode := 130
	require.NoError(t, store.SaveHeadlessRun(HeadlessRun{
		ID:                 "run-finish-after-cancel",
		CompletedAt:        &canceledAt,
		CanceledAt:         &canceledAt,
		ExitCode:           &exitCode,
		CancellationReason: "operator requested stop",
		TerminalReason:     "operator requested stop",
		Status:             HeadlessStatusCanceled,
	}))

	completedCode := 0
	saved, wrote, err := store.SaveFinishedHeadlessRun(HeadlessRun{
		ID:       "run-finish-after-cancel",
		ExitCode: &completedCode,
		Status:   HeadlessStatusCompleted,
	})
	require.NoError(t, err)
	assert.False(t, wrote)
	assert.Equal(t, HeadlessStatusCanceled, saved.Status)
	assert.Equal(t, "operator requested stop", saved.CancellationReason)

	loaded, err := store.LoadHeadlessRun("run-finish-after-cancel")
	require.NoError(t, err)
	assert.Equal(t, HeadlessStatusCanceled, loaded.Status)
	assert.Equal(t, "operator requested stop", loaded.TerminalReason)
	require.NotNil(t, loaded.ExitCode)
	assert.Equal(t, 130, *loaded.ExitCode)
}

func TestStore_SaveFinishedHeadlessRunRejectsNonTerminalStatus(t *testing.T) {
	t.Parallel()

	store := NewStore(t.TempDir())

	_, wrote, err := store.SaveFinishedHeadlessRun(HeadlessRun{
		ID:     "run-finish-non-terminal",
		Status: HeadlessStatusRunning,
	})
	require.ErrorContains(t, err, "must have a terminal status")
	assert.False(t, wrote)

	_, err = store.LoadHeadlessRun("run-finish-non-terminal")
	require.ErrorIs(t, err, os.ErrNotExist)
}

func TestStore_SaveFinishedHeadlessRunIgnoresDifferentExecution(t *testing.T) {
	t.Parallel()

	store := NewStore(t.TempDir())
	currentStarted := time.Now().UTC()
	require.NoError(t, store.SaveHeadlessRun(HeadlessRun{
		ID:             "run-reused-id",
		StartedAt:      currentStarted,
		SessionID:      "session-current",
		Hostname:       "foreign-host",
		PID:            222,
		ProcessGroupID: 22,
		Status:         HeadlessStatusRunning,
	}))

	completedCode := 0
	saved, wrote, err := store.SaveFinishedHeadlessRun(HeadlessRun{
		ID:             "run-reused-id",
		StartedAt:      currentStarted.Add(-time.Hour),
		SessionID:      "session-previous",
		Hostname:       "foreign-host",
		PID:            111,
		ProcessGroupID: 11,
		ExitCode:       &completedCode,
		Status:         HeadlessStatusCompleted,
	})
	require.NoError(t, err)
	assert.False(t, wrote)
	assert.Equal(t, HeadlessStatusRunning, saved.Status)
	assert.Equal(t, "session-current", saved.SessionID)
	assert.Equal(t, 222, saved.PID)
	assert.Equal(t, 22, saved.ProcessGroupID)

	loaded, err := store.LoadHeadlessRun("run-reused-id")
	require.NoError(t, err)
	assert.Equal(t, HeadlessStatusRunning, loaded.Status)
	assert.Equal(t, "session-current", loaded.SessionID)
	assert.Equal(t, 222, loaded.PID)
	assert.Equal(t, 22, loaded.ProcessGroupID)
}

func TestStore_SaveFinishedHeadlessRunCanCompleteOrphanedStatus(t *testing.T) {
	t.Parallel()

	store := NewStore(t.TempDir())
	orphanedAt := time.Now().UTC()
	require.NoError(t, store.SaveHeadlessRun(HeadlessRun{
		ID:              "run-finish-after-orphaned",
		StartedAt:       orphanedAt,
		LastHeartbeatAt: orphanedAt,
		OrphanedReason:  "no heartbeat since " + orphanedAt.Format(time.RFC3339),
		Status:          HeadlessStatusOrphaned,
		Stale:           true,
	}))

	completedCode := 0
	saved, wrote, err := store.SaveFinishedHeadlessRun(HeadlessRun{
		ID:       "run-finish-after-orphaned",
		ExitCode: &completedCode,
		Status:   HeadlessStatusCompleted,
	})
	require.NoError(t, err)
	assert.True(t, wrote)
	assert.Equal(t, HeadlessStatusCompleted, saved.Status)
	require.NotNil(t, saved.ExitCode)
	assert.Equal(t, 0, *saved.ExitCode)

	loaded, err := store.LoadHeadlessRun("run-finish-after-orphaned")
	require.NoError(t, err)
	assert.Equal(t, HeadlessStatusCompleted, loaded.Status)
	assert.False(t, loaded.Stale)
	assert.Empty(t, loaded.OrphanedReason)
}

func TestStore_SaveFinishedHeadlessRunPreservesStaleStatus(t *testing.T) {
	t.Parallel()

	store := NewStore(t.TempDir())
	staleAt := time.Now().UTC()
	exitCode := 1
	require.NoError(t, store.SaveHeadlessRun(HeadlessRun{
		ID:             "run-finish-after-stale",
		CompletedAt:    &staleAt,
		ExitCode:       &exitCode,
		TerminalReason: "process pid 99999999 is not running",
		StaleReason:    "process pid 99999999 is not running",
		Status:         HeadlessStatusStale,
		Stale:          true,
	}))

	completedCode := 0
	saved, wrote, err := store.SaveFinishedHeadlessRun(HeadlessRun{
		ID:       "run-finish-after-stale",
		ExitCode: &completedCode,
		Status:   HeadlessStatusCompleted,
	})
	require.NoError(t, err)
	assert.False(t, wrote)
	assert.Equal(t, HeadlessStatusStale, saved.Status)
	assert.Equal(t, "process pid 99999999 is not running", saved.StaleReason)

	loaded, err := store.LoadHeadlessRun("run-finish-after-stale")
	require.NoError(t, err)
	assert.Equal(t, HeadlessStatusStale, loaded.Status)
	require.NotNil(t, loaded.ExitCode)
	assert.Equal(t, 1, *loaded.ExitCode)
}

func TestStore_CancelHeadlessRunPreservesStaleStatus(t *testing.T) {
	t.Parallel()

	store := NewStore(t.TempDir())
	staleAt := time.Now().UTC()
	exitCode := 1
	hostname, err := os.Hostname()
	require.NoError(t, err)

	require.NoError(t, store.SaveHeadlessRun(HeadlessRun{
		ID:              "run-cancel-after-stale",
		CompletedAt:     &staleAt,
		ExitCode:        &exitCode,
		Hostname:        hostname,
		PID:             os.Getpid(),
		ProcessGroupID:  headlessProcessGroupID(os.Getpid()),
		TerminalReason:  "process pid 99999999 is not running",
		StaleReason:     "process pid 99999999 is not running",
		LastHeartbeatAt: staleAt.Add(-time.Hour),
		Status:          HeadlessStatusStale,
		Stale:           true,
	}))

	canceled, err := store.CancelHeadlessRun("run-cancel-after-stale", "operator requested stop")
	require.NoError(t, err)
	assert.Equal(t, HeadlessStatusStale, canceled.Status)
	assert.Empty(t, canceled.CancellationReason)
	assert.Equal(t, "process pid 99999999 is not running", canceled.TerminalReason)

	loaded, err := store.LoadHeadlessRun("run-cancel-after-stale")
	require.NoError(t, err)
	assert.Equal(t, HeadlessStatusStale, loaded.Status)
	assert.Empty(t, loaded.CancellationReason)
	require.NotNil(t, loaded.ExitCode)
	assert.Equal(t, 1, *loaded.ExitCode)

	events, err := store.ReadHeadlessEvents("run-cancel-after-stale")
	require.NoError(t, err)
	assert.Empty(t, events)
}

func TestStore_CancelHeadlessRunLeavesTerminalStatusesUnchanged(t *testing.T) {
	t.Parallel()

	store := NewStore(t.TempDir())
	completedAt := time.Now().UTC()

	deadPID := 99999999
	if headlessProcessAlive(deadPID) {
		t.Skipf("test pid %d unexpectedly exists", deadPID)
	}

	tests := []struct {
		status             HeadlessStatus
		name               string
		cancellationReason string
	}{
		{name: "completed", status: HeadlessStatusCompleted},
		{name: "failed", status: HeadlessStatusFailed},
		{name: "canceled", status: HeadlessStatusCanceled, cancellationReason: "already canceled"},
		{name: "timed_out", status: HeadlessStatusTimedOut},
		{name: "superseded", status: HeadlessStatusSuperseded},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			id := "run-cancel-terminal-" + tt.name
			require.NoError(t, store.SaveHeadlessRun(HeadlessRun{
				ID:                 id,
				CompletedAt:        &completedAt,
				CanceledAt:         canceledAtForStatus(tt.status, completedAt),
				CancellationReason: tt.cancellationReason,
				Hostname:           "foreign-host",
				PID:                deadPID,
				TerminalReason:     string(tt.status),
				Status:             tt.status,
			}))

			got, err := store.CancelHeadlessRun(id, "operator requested stop")
			require.NoError(t, err)
			assert.Equal(t, tt.status, got.Status)
			assert.Equal(t, tt.cancellationReason, got.CancellationReason)
			assert.Equal(t, string(tt.status), got.TerminalReason)

			loaded, err := store.LoadHeadlessRun(id)
			require.NoError(t, err)
			assert.Equal(t, tt.status, loaded.Status)
			assert.Equal(t, tt.cancellationReason, loaded.CancellationReason)

			events, err := store.ReadHeadlessEvents(id)
			require.NoError(t, err)
			assert.Empty(t, events)
		})
	}
}

func canceledAtForStatus(status HeadlessStatus, at time.Time) *time.Time {
	if status != HeadlessStatusCanceled {
		return nil
	}

	return &at
}

func TestStore_SaveFinishedHeadlessRunPreservesSupersededStatus(t *testing.T) {
	t.Parallel()

	store := NewStore(t.TempDir())
	supersededAt := time.Now().UTC()
	exitCode := 1
	require.NoError(t, store.SaveHeadlessRun(HeadlessRun{
		ID:             "run-finish-after-superseded",
		CompletedAt:    &supersededAt,
		ExitCode:       &exitCode,
		TerminalReason: "superseded by run-next",
		Status:         HeadlessStatusSuperseded,
	}))

	completedCode := 0
	saved, wrote, err := store.SaveFinishedHeadlessRun(HeadlessRun{
		ID:       "run-finish-after-superseded",
		ExitCode: &completedCode,
		Status:   HeadlessStatusCompleted,
	})
	require.NoError(t, err)
	assert.False(t, wrote)
	assert.Equal(t, HeadlessStatusSuperseded, saved.Status)
	assert.Equal(t, "superseded by run-next", saved.TerminalReason)

	loaded, err := store.LoadHeadlessRun("run-finish-after-superseded")
	require.NoError(t, err)
	assert.Equal(t, HeadlessStatusSuperseded, loaded.Status)
	require.NotNil(t, loaded.ExitCode)
	assert.Equal(t, 1, *loaded.ExitCode)
}

func TestStore_CancelHeadlessRunRecordsDurableCanceledStatus(t *testing.T) {
	t.Parallel()

	store := NewStore(t.TempDir())
	heartbeat := time.Now().UTC()
	require.NoError(t, store.SaveHeadlessRun(HeadlessRun{
		ID:              "run-cancel",
		LastHeartbeatAt: heartbeat,
		Hostname:        "foreign-host",
		PID:             os.Getpid(),
		Status:          HeadlessStatusRunning,
	}))

	canceled, err := store.CancelHeadlessRun("run-cancel", "operator requested stop")
	require.NoError(t, err)
	assert.Equal(t, HeadlessStatusCanceled, canceled.Status)
	assert.Equal(t, "operator requested stop", canceled.CancellationReason)
	assert.Contains(t, canceled.TerminalReason, "operator requested stop")
	require.NotNil(t, canceled.CanceledAt)
	require.NotNil(t, canceled.CompletedAt)
	require.NotNil(t, canceled.ExitCode)
	assert.Equal(t, 130, *canceled.ExitCode)

	loaded, err := store.LoadHeadlessRun("run-cancel")
	require.NoError(t, err)
	assert.Equal(t, HeadlessStatusCanceled, loaded.Status)

	log, err := store.ReadHeadlessLog("run-cancel")
	require.NoError(t, err)
	assert.Contains(t, log, "canceled")
	assert.Contains(t, log, "operator requested stop")

	events, err := store.ReadHeadlessEvents("run-cancel")
	require.NoError(t, err)
	require.Len(t, events, 1)
	assert.Equal(t, HeadlessEventCanceled, events[0].Type)
	assert.Equal(t, HeadlessStatusCanceled, events[0].Status)
	assert.Equal(t, "operator requested stop", events[0].CancelReason)
	assert.Contains(t, events[0].TerminalReason, "operator requested stop")
}

func TestStore_CancelHeadlessRunTerminatesLiveProcess(t *testing.T) {
	if runtime.GOOS == testWindowsGOOS {
		t.Skip("uses POSIX sleep process for signal verification")
	}

	t.Parallel()

	store := NewStore(t.TempDir())
	cmd := exec.CommandContext(t.Context(), "sleep", "30")
	require.NoError(t, cmd.Start())

	waited := false
	done := make(chan error, 1)

	go func() {
		done <- cmd.Wait()
	}()

	defer func() {
		if waited {
			return
		}

		if err := cmd.Process.Kill(); err != nil {
			t.Logf("cleanup kill pid %d: %v", cmd.Process.Pid, err)
		}

		select {
		case <-done:
		case <-time.After(time.Second):
		}
	}()

	hostname, err := os.Hostname()
	require.NoError(t, err)
	require.NoError(t, store.SaveHeadlessRun(HeadlessRun{
		ID:       "run-cancel-live",
		Hostname: hostname,
		PID:      cmd.Process.Pid,
		Status:   HeadlessStatusRunning,
	}))

	canceled, err := store.CancelHeadlessRun("run-cancel-live", "operator requested stop")
	require.NoError(t, err)
	assert.Equal(t, HeadlessStatusCanceled, canceled.Status)

	select {
	case <-done:
		waited = true
	case <-time.After(2 * time.Second):
		t.Fatalf("cancel did not terminate pid %d", cmd.Process.Pid)
	}

	loaded, err := store.LoadHeadlessRun("run-cancel-live")
	require.NoError(t, err)
	assert.Equal(t, HeadlessStatusCanceled, loaded.Status)

	events, err := store.ReadHeadlessEvents("run-cancel-live")
	require.NoError(t, err)
	require.Len(t, events, 1)
	assert.Equal(t, HeadlessEventCanceled, events[0].Type)
}

func TestStore_CancelHeadlessRunTerminatesLiveProcessAfterStaleReconciliation(t *testing.T) {
	if runtime.GOOS == testWindowsGOOS {
		t.Skip("uses POSIX sleep process for signal verification")
	}

	t.Parallel()

	store := NewStore(t.TempDir())
	cmd := exec.CommandContext(t.Context(), "sleep", "30")
	require.NoError(t, cmd.Start())

	waited := false
	done := make(chan error, 1)

	go func() {
		done <- cmd.Wait()
	}()

	defer func() {
		if waited {
			return
		}

		if err := cmd.Process.Kill(); err != nil {
			t.Logf("cleanup kill pid %d: %v", cmd.Process.Pid, err)
		}

		select {
		case <-done:
		case <-time.After(time.Second):
		}
	}()

	hostname, err := os.Hostname()
	require.NoError(t, err)

	staleAt := time.Now().Add(-time.Hour).UTC()
	require.NoError(t, store.SaveHeadlessRun(HeadlessRun{
		ID:              "run-cancel-reconciled-stale",
		StartedAt:       staleAt,
		UpdatedAt:       staleAt,
		LastHeartbeatAt: staleAt,
		Hostname:        hostname,
		PID:             cmd.Process.Pid,
		Status:          HeadlessStatusRunning,
	}))

	runs, err := store.ListHeadlessRuns()
	require.NoError(t, err)
	require.Len(t, runs, 1)
	assert.Equal(t, HeadlessStatusOrphaned, runs[0].Status)

	canceled, err := store.CancelHeadlessRun("run-cancel-reconciled-stale", "operator requested stop")
	require.NoError(t, err)
	assert.Equal(t, HeadlessStatusCanceled, canceled.Status)

	select {
	case <-done:
		waited = true
	case <-time.After(2 * time.Second):
		t.Fatalf("cancel did not terminate stale pid %d", cmd.Process.Pid)
	}

	loaded, err := store.LoadHeadlessRun("run-cancel-reconciled-stale")
	require.NoError(t, err)
	assert.Equal(t, HeadlessStatusCanceled, loaded.Status)
	assert.False(t, loaded.Stale)
	assert.Empty(t, loaded.OrphanedReason)

	events, err := store.ReadHeadlessEvents("run-cancel-reconciled-stale")
	require.NoError(t, err)
	require.Len(t, events, 2)
	assert.Equal(t, HeadlessEventOrphaned, events[0].Type)
	assert.Equal(t, HeadlessEventCanceled, events[1].Type)
}

func TestStore_CancelHeadlessRunMarksForeignOrphanedRunCanceled(t *testing.T) {
	t.Parallel()

	store := NewStore(t.TempDir())
	orphanedAt := time.Now().Add(-time.Hour).UTC()
	require.NoError(t, store.SaveHeadlessRun(HeadlessRun{
		ID:              "run-cancel-foreign-orphaned",
		LastHeartbeatAt: orphanedAt,
		Hostname:        "foreign-host",
		PID:             os.Getpid(),
		OrphanedReason:  "no heartbeat since " + orphanedAt.Format(time.RFC3339),
		Status:          HeadlessStatusOrphaned,
		Stale:           true,
	}))

	canceled, err := store.CancelHeadlessRun("run-cancel-foreign-orphaned", "operator requested stop")
	require.NoError(t, err)
	assert.Equal(t, HeadlessStatusCanceled, canceled.Status)
	assert.Equal(t, "operator requested stop", canceled.CancellationReason)
	assert.Contains(t, canceled.TerminalReason, "operator requested stop")
	assert.Contains(t, canceled.TerminalReason, "foreign-host")
	assert.False(t, canceled.Stale)
	assert.Empty(t, canceled.OrphanedReason)

	loaded, err := store.LoadHeadlessRun("run-cancel-foreign-orphaned")
	require.NoError(t, err)
	assert.Equal(t, HeadlessStatusCanceled, loaded.Status)
	assert.Empty(t, loaded.OrphanedReason)

	events, err := store.ReadHeadlessEvents("run-cancel-foreign-orphaned")
	require.NoError(t, err)
	require.Len(t, events, 1)
	assert.Equal(t, HeadlessEventCanceled, events[0].Type)
	assert.Contains(t, events[0].Error, "foreign-host")
}

func TestStore_CancelHeadlessRunMarksDeadOrphanedProcessStale(t *testing.T) {
	t.Parallel()

	store := NewStore(t.TempDir())
	hostname, err := os.Hostname()
	require.NoError(t, err)

	deadPID := 99999999
	if headlessProcessAlive(deadPID) {
		t.Skipf("test pid %d unexpectedly exists", deadPID)
	}

	orphanedAt := time.Now().Add(-time.Hour).UTC()
	require.NoError(t, store.SaveHeadlessRun(HeadlessRun{
		ID:              "run-cancel-dead-orphaned",
		StartedAt:       orphanedAt,
		LastHeartbeatAt: orphanedAt,
		Hostname:        hostname,
		PID:             deadPID,
		OrphanedReason:  "no heartbeat since " + orphanedAt.Format(time.RFC3339),
		Status:          HeadlessStatusOrphaned,
		Stale:           true,
	}))

	reconciled, err := store.CancelHeadlessRun("run-cancel-dead-orphaned", "operator requested stop")
	require.NoError(t, err)
	assert.Equal(t, HeadlessStatusStale, reconciled.Status)
	assert.Contains(t, reconciled.StaleReason, "process pid")

	loaded, err := store.LoadHeadlessRun("run-cancel-dead-orphaned")
	require.NoError(t, err)
	assert.Equal(t, HeadlessStatusStale, loaded.Status)
	assert.Empty(t, loaded.OrphanedReason)

	events, err := store.ReadHeadlessEvents("run-cancel-dead-orphaned")
	require.NoError(t, err)
	require.Len(t, events, 1)
	assert.Equal(t, HeadlessEventStale, events[0].Type)
	assert.Empty(t, events[0].OrphanedReason)
}

func TestStore_CancelHeadlessRunTreatsMismatchedProcessGroupAsStale(t *testing.T) {
	t.Parallel()

	processGroupID := headlessProcessGroupID(os.Getpid())
	if processGroupID == 0 {
		t.Skip("process group lookup is unavailable on this platform")
	}

	store := NewStore(t.TempDir())
	hostname, err := os.Hostname()
	require.NoError(t, err)

	require.NoError(t, store.SaveHeadlessRun(HeadlessRun{
		ID:              "run-cancel-pgid-mismatch",
		Hostname:        hostname,
		PID:             os.Getpid(),
		ProcessGroupID:  processGroupID + 999999,
		LastHeartbeatAt: time.Now().UTC(),
		Status:          HeadlessStatusRunning,
	}))

	reconciled, err := store.CancelHeadlessRun("run-cancel-pgid-mismatch", "operator requested stop")
	require.NoError(t, err)
	assert.Equal(t, HeadlessStatusStale, reconciled.Status)
	assert.Contains(t, reconciled.StaleReason, "process group")
	assert.Empty(t, reconciled.CancellationReason)

	events, err := store.ReadHeadlessEvents("run-cancel-pgid-mismatch")
	require.NoError(t, err)
	require.Len(t, events, 1)
	assert.Equal(t, HeadlessEventStale, events[0].Type)
}

func TestTerminateHeadlessRunProcessRefusesMismatchedProcessGroup(t *testing.T) {
	if runtime.GOOS == testWindowsGOOS {
		t.Skip("uses POSIX sleep process for process-group verification")
	}

	t.Parallel()

	cmd := exec.CommandContext(t.Context(), "sleep", "30")
	require.NoError(t, cmd.Start())

	waited := false
	done := make(chan error, 1)

	go func() {
		done <- cmd.Wait()
	}()

	defer func() {
		if waited {
			return
		}

		if err := cmd.Process.Kill(); err != nil {
			t.Logf("cleanup kill pid %d: %v", cmd.Process.Pid, err)
		}

		select {
		case <-done:
		case <-time.After(time.Second):
		}
	}()

	processGroupID := headlessProcessGroupID(cmd.Process.Pid)
	if processGroupID == 0 {
		t.Skip("process group lookup is unavailable on this platform")
	}

	hostname, err := os.Hostname()
	require.NoError(t, err)

	err = terminateHeadlessRunProcess(HeadlessRun{
		ID:             "run-signal-pgid-mismatch",
		Hostname:       hostname,
		PID:            cmd.Process.Pid,
		ProcessGroupID: processGroupID + 999999,
		Status:         HeadlessStatusRunning,
	})
	require.ErrorContains(t, err, "process group changed")

	select {
	case <-done:
		waited = true

		t.Fatal("process exited after refused process-group mismatch signal")
	case <-time.After(100 * time.Millisecond):
	}
}

func TestStore_CancelHeadlessRunRefusesToSignalCurrentProcess(t *testing.T) {
	t.Parallel()

	store := NewStore(t.TempDir())
	require.NoError(t, store.SaveHeadlessRun(HeadlessRun{
		ID:     "run-cancel-self",
		PID:    os.Getpid(),
		Status: HeadlessStatusRunning,
	}))

	canceled, err := store.CancelHeadlessRun("run-cancel-self", "operator requested stop")
	require.NoError(t, err)
	assert.Equal(t, HeadlessStatusCanceled, canceled.Status)
	assert.Contains(t, canceled.TerminalReason, "refusing to signal current process")

	events, err := store.ReadHeadlessEvents("run-cancel-self")
	require.NoError(t, err)
	require.Len(t, events, 1)
	assert.Contains(t, events[0].Error, "refusing to signal current process")
}

func TestStore_ReadHeadlessEventsAllowsMissingEventFile(t *testing.T) {
	t.Parallel()

	store := NewStore(t.TempDir())
	require.NoError(t, store.SaveHeadlessRun(HeadlessRun{
		ID:     "run-no-events",
		Status: HeadlessStatusCompleted,
	}))

	events, err := store.ReadHeadlessEvents("run-no-events")
	require.NoError(t, err)
	assert.Empty(t, events)
}

func TestStore_AppendHeadlessEventRejectsInvalidLifecycleFields(t *testing.T) {
	t.Parallel()

	store := NewStore(t.TempDir())
	require.NoError(t, store.SaveHeadlessRun(HeadlessRun{
		ID:     "run-event-validation",
		Status: HeadlessStatusCompleted,
	}))

	tests := []struct {
		event   HeadlessEvent
		name    string
		wantErr string
	}{
		{
			name:    "empty type",
			event:   HeadlessEvent{},
			wantErr: "invalid headless event type",
		},
		{
			name: "unknown type",
			event: HeadlessEvent{
				Type: HeadlessEventType("mystery"),
			},
			wantErr: "invalid headless event type",
		},
		{
			name: "mismatched run id",
			event: HeadlessEvent{
				Type:  HeadlessEventCompleted,
				RunID: "run-other",
			},
			wantErr: "does not match",
		},
		{
			name: "unknown status",
			event: HeadlessEvent{
				Type:   HeadlessEventCompleted,
				Status: HeadlessStatus("mystery"),
			},
			wantErr: "invalid headless event status",
		},
		{
			name: "mismatched status",
			event: HeadlessEvent{
				Type:   HeadlessEventCompleted,
				Status: HeadlessStatusFailed,
			},
			wantErr: "does not match status",
		},
		{
			name: "mismatched user message role",
			event: HeadlessEvent{
				Type: HeadlessEventUserMessage,
				Role: "assistant",
			},
			wantErr: "does not match role",
		},
		{
			name: "role on terminal event",
			event: HeadlessEvent{
				Type: HeadlessEventCompleted,
				Role: "assistant",
			},
			wantErr: "does not match role",
		},
		{
			name: "self parent",
			event: HeadlessEvent{
				Type:        HeadlessEventCompleted,
				ParentRunID: "run-event-validation",
			},
			wantErr: "cannot be its own parent",
		},
		{
			name: "invalid child id",
			event: HeadlessEvent{
				Type:        HeadlessEventCompleted,
				ChildRunIDs: []string{" child "},
			},
			wantErr: "invalid headless event child_run_id",
		},
		{
			name: "duplicate child id",
			event: HeadlessEvent{
				Type:        HeadlessEventCompleted,
				ChildRunIDs: []string{"run-child", "run-child"},
			},
			wantErr: "duplicate headless event child_run_id",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			err := store.AppendHeadlessEvent("run-event-validation", tt.event)
			require.ErrorContains(t, err, tt.wantErr)
		})
	}
}

func TestStore_ReadHeadlessEventsRejectsInvalidLifecycleFields(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		content string
		wantErr string
	}{
		{
			name:    "mismatched run id",
			content: `{"run_id":"run-other","type":"completed","status":"completed"}` + "\n",
			wantErr: "does not match",
		},
		{
			name:    "mismatched status",
			content: `{"run_id":"run-invalid-events","type":"completed","status":"failed"}` + "\n",
			wantErr: "does not match status",
		},
		{
			name:    "mismatched role",
			content: `{"run_id":"run-invalid-events","type":"user_message","role":"assistant"}` + "\n",
			wantErr: "does not match role",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			store := NewStore(t.TempDir())
			require.NoError(t, os.MkdirAll(store.headlessDir(), 0o750))
			require.NoError(t, os.WriteFile(
				store.headlessEventsPath("run-invalid-events"),
				[]byte(tt.content),
				0o600,
			))

			_, err := store.ReadHeadlessEvents("run-invalid-events")
			require.ErrorContains(t, err, tt.wantErr)
		})
	}
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

func TestStore_ListAndStatusSurfaceCorruptHeadlessMetadata(t *testing.T) {
	t.Parallel()

	store := NewStore(t.TempDir())
	require.NoError(t, os.MkdirAll(filepath.Join(store.Dir(), "headless"), 0o750))
	require.NoError(t, os.WriteFile(store.headlessJSONPath("run-corrupt"), []byte("{not-json"), 0o600))

	runs, err := store.ListHeadlessRuns()
	require.NoError(t, err)
	require.Len(t, runs, 1)
	assert.Equal(t, "run-corrupt", runs[0].ID)
	assert.Equal(t, HeadlessStatusCorrupt, runs[0].Status)
	assert.Contains(t, runs[0].Error, "parse headless")

	status, err := store.HeadlessRunStatus("run-corrupt")
	require.NoError(t, err)
	assert.Equal(t, HeadlessStatusCorrupt, status.Status)

	_, err = store.LoadHeadlessRun("run-corrupt")
	require.ErrorIs(t, err, ErrCorruptHeadlessRun)
}

func TestStore_ListAndStatusSurfaceMismatchedHeadlessMetadataID(t *testing.T) {
	t.Parallel()

	store := NewStore(t.TempDir())
	require.NoError(t, os.MkdirAll(filepath.Join(store.Dir(), "headless"), 0o750))
	require.NoError(t, os.WriteFile(store.headlessJSONPath("run-mismatch"), []byte(`{"id":"run-other","status":"running"}`), 0o600))

	runs, err := store.ListHeadlessRuns()
	require.NoError(t, err)
	require.Len(t, runs, 1)
	assert.Equal(t, "run-mismatch", runs[0].ID)
	assert.Equal(t, HeadlessStatusCorrupt, runs[0].Status)
	assert.Contains(t, runs[0].Error, "metadata id")

	status, err := store.HeadlessRunStatus("run-mismatch")
	require.NoError(t, err)
	assert.Equal(t, "run-mismatch", status.ID)
	assert.Equal(t, HeadlessStatusCorrupt, status.Status)
	assert.Contains(t, status.Error, "metadata id")

	_, err = store.LoadHeadlessRun("run-mismatch")
	require.ErrorIs(t, err, ErrCorruptHeadlessRun)
}

func TestStore_ListAndStatusSurfaceUnknownHeadlessStatus(t *testing.T) {
	t.Parallel()

	store := NewStore(t.TempDir())
	require.NoError(t, os.MkdirAll(filepath.Join(store.Dir(), "headless"), 0o750))
	require.NoError(t, os.WriteFile(store.headlessJSONPath("run-unknown-status"), []byte(`{"id":"run-unknown-status","status":"mystery"}`), 0o600))

	runs, err := store.ListHeadlessRuns()
	require.NoError(t, err)
	require.Len(t, runs, 1)
	assert.Equal(t, "run-unknown-status", runs[0].ID)
	assert.Equal(t, HeadlessStatusCorrupt, runs[0].Status)
	assert.Contains(t, runs[0].Error, "invalid status")

	status, err := store.HeadlessRunStatus("run-unknown-status")
	require.NoError(t, err)
	assert.Equal(t, "run-unknown-status", status.ID)
	assert.Equal(t, HeadlessStatusCorrupt, status.Status)
	assert.Contains(t, status.Error, "invalid status")

	_, err = store.LoadHeadlessRun("run-unknown-status")
	require.ErrorIs(t, err, ErrCorruptHeadlessRun)
}

func TestStore_ListAndStatusSurfacePersistedCorruptHeadlessStatus(t *testing.T) {
	t.Parallel()

	store := NewStore(t.TempDir())
	require.NoError(t, os.MkdirAll(filepath.Join(store.Dir(), "headless"), 0o750))
	require.NoError(t, os.WriteFile(store.headlessJSONPath("run-persisted-corrupt-status"), []byte(`{"id":"run-persisted-corrupt-status","status":"corrupt"}`), 0o600))

	runs, err := store.ListHeadlessRuns()
	require.NoError(t, err)
	require.Len(t, runs, 1)
	assert.Equal(t, "run-persisted-corrupt-status", runs[0].ID)
	assert.Equal(t, HeadlessStatusCorrupt, runs[0].Status)
	assert.Contains(t, runs[0].Error, "invalid status")

	status, err := store.HeadlessRunStatus("run-persisted-corrupt-status")
	require.NoError(t, err)
	assert.Equal(t, "run-persisted-corrupt-status", status.ID)
	assert.Equal(t, HeadlessStatusCorrupt, status.Status)
	assert.Contains(t, status.Error, "invalid status")

	_, err = store.LoadHeadlessRun("run-persisted-corrupt-status")
	require.ErrorIs(t, err, ErrCorruptHeadlessRun)
}

func TestStore_ListAndStatusSurfaceInvalidHeadlessRelationships(t *testing.T) {
	t.Parallel()

	tests := []struct {
		content string
		name    string
		wantErr string
	}{
		{
			name:    "invalid parent",
			content: `{"id":"run-invalid-relationship","status":"running","parent_run_id":" parent "}`,
			wantErr: "invalid parent headless id",
		},
		{
			name:    "self child",
			content: `{"id":"run-invalid-relationship","status":"running","child_run_ids":["run-invalid-relationship"]}`,
			wantErr: "cannot be its own child",
		},
		{
			name:    "duplicate child",
			content: `{"id":"run-invalid-relationship","status":"running","child_run_ids":["run-child","run-child"]}`,
			wantErr: "duplicate child headless id",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			store := NewStore(t.TempDir())
			require.NoError(t, os.MkdirAll(filepath.Join(store.Dir(), "headless"), 0o750))
			require.NoError(t, os.WriteFile(store.headlessJSONPath("run-invalid-relationship"), []byte(tt.content), 0o600))

			runs, err := store.ListHeadlessRuns()
			require.NoError(t, err)
			require.Len(t, runs, 1)
			assert.Equal(t, "run-invalid-relationship", runs[0].ID)
			assert.Equal(t, HeadlessStatusCorrupt, runs[0].Status)
			assert.Contains(t, runs[0].Error, tt.wantErr)

			status, err := store.HeadlessRunStatus("run-invalid-relationship")
			require.NoError(t, err)
			assert.Equal(t, "run-invalid-relationship", status.ID)
			assert.Equal(t, HeadlessStatusCorrupt, status.Status)
			assert.Contains(t, status.Error, tt.wantErr)

			_, err = store.LoadHeadlessRun("run-invalid-relationship")
			require.ErrorIs(t, err, ErrCorruptHeadlessRun)
		})
	}
}

func TestStore_ListHeadlessRunsIgnoresTemporaryMetadataFiles(t *testing.T) {
	t.Parallel()

	store := NewStore(t.TempDir())
	require.NoError(t, os.MkdirAll(filepath.Join(store.Dir(), "headless"), 0o750))
	require.NoError(t, os.WriteFile(filepath.Join(store.Dir(), "headless", ".headless-crash.json"), []byte("{not-json"), 0o600))
	require.NoError(t, store.SaveHeadlessRun(HeadlessRun{
		ID:     "run-real",
		Status: HeadlessStatusCompleted,
	}))

	runs, err := store.ListHeadlessRuns()
	require.NoError(t, err)
	require.Len(t, runs, 1)
	assert.Equal(t, "run-real", runs[0].ID)

	recovered, err := store.RecoverStaleHeadlessRuns(time.Millisecond)
	require.NoError(t, err)
	assert.Empty(t, recovered)
}

func TestStore_ListHeadlessRunsIgnoresInvalidMetadataIDs(t *testing.T) {
	t.Parallel()

	store := NewStore(t.TempDir())
	require.NoError(t, os.MkdirAll(filepath.Join(store.Dir(), "headless"), 0o750))
	require.NoError(t, os.WriteFile(filepath.Join(store.Dir(), "headless", ".json"), []byte("{not-json"), 0o600))
	require.NoError(t, os.WriteFile(filepath.Join(store.Dir(), "headless", " run.json"), []byte("{not-json"), 0o600))
	require.NoError(t, store.SaveHeadlessRun(HeadlessRun{
		ID:     "run-real",
		Status: HeadlessStatusCompleted,
	}))

	runs, err := store.ListHeadlessRuns()
	require.NoError(t, err)
	require.Len(t, runs, 1)
	assert.Equal(t, "run-real", runs[0].ID)

	recovered, err := store.RecoverStaleHeadlessRuns(time.Millisecond)
	require.NoError(t, err)
	assert.Empty(t, recovered)
}

func TestStore_RecoverStaleHeadlessRunsSkipsCorruptMetadata(t *testing.T) {
	t.Parallel()

	store := NewStore(t.TempDir())
	staleAt := time.Now().Add(-time.Hour).UTC()
	require.NoError(t, store.SaveHeadlessRun(HeadlessRun{
		StartedAt:       staleAt,
		UpdatedAt:       staleAt,
		LastHeartbeatAt: staleAt,
		ID:              "run-recoverable",
		PID:             -1,
		Status:          HeadlessStatusRunning,
	}))
	require.NoError(t, os.WriteFile(store.headlessJSONPath("run-corrupt"), []byte("{not-json"), 0o600))

	recovered, err := store.RecoverStaleHeadlessRuns(time.Millisecond)
	require.NoError(t, err)
	require.Len(t, recovered, 1)
	assert.Equal(t, "run-recoverable", recovered[0].ID)
	assert.Equal(t, HeadlessStatusStale, recovered[0].Status)
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

func TestStore_HeadlessRejectsPathLikeIDs(t *testing.T) {
	t.Parallel()

	store := NewStore(t.TempDir())

	for _, id := range []string{
		".",
		"..",
		"parent/child",
		`parent\child`,
		"run:name",
		"run*name",
		"run\nname",
		" run",
		"run ",
	} {
		t.Run(strings.ReplaceAll(id, "\n", "\\n"), func(t *testing.T) {
			t.Parallel()

			require.Error(t, store.SaveHeadlessRun(HeadlessRun{
				ID:     id,
				Status: HeadlessStatusRunning,
			}))

			_, err := store.LoadHeadlessRun(id)
			require.Error(t, err)
		})
	}
}

func TestStore_HeadlessRejectsReservedTemporaryIDPrefix(t *testing.T) {
	t.Parallel()

	store := NewStore(t.TempDir())

	require.ErrorContains(t, store.SaveHeadlessRun(HeadlessRun{
		ID:     headlessTempFilePrefix + "run",
		Status: HeadlessStatusRunning,
	}), "reserved prefix")

	_, err := store.LoadHeadlessRun(headlessTempFilePrefix + "run")
	require.ErrorContains(t, err, "reserved prefix")
}

func TestStore_HeadlessRejectsUnknownStatusOnSave(t *testing.T) {
	t.Parallel()

	store := NewStore(t.TempDir())

	err := store.SaveHeadlessRun(HeadlessRun{
		ID:     "run-unknown-status",
		Status: "mystery",
	})
	require.ErrorContains(t, err, "invalid headless status")

	err = store.SaveHeadlessRun(HeadlessRun{
		ID:     "run-corrupt-status",
		Status: HeadlessStatusCorrupt,
	})
	require.ErrorContains(t, err, "invalid headless status")
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
