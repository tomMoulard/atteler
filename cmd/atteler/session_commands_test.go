package main

import (
	"context"
	"encoding/json"
	"flag"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/tommoulard/atteler/pkg/permission"
	"github.com/tommoulard/atteler/pkg/session"
)

func TestRunHeadlessCommandRejectsMultipleLifecycleActions(t *testing.T) {
	t.Parallel()

	store := session.NewStore(t.TempDir())
	err := runHeadlessCommand(context.Background(), cliOptions{
		listHeadless:     true,
		cancelHeadlessID: "run-123",
	}, store)
	require.ErrorContains(t, err, "choose only one")
}

func TestRunHeadlessCommandPermissionPolicyDeniesCancelBeforeStateMutation(t *testing.T) {
	t.Parallel()

	store := session.NewStore(t.TempDir())
	require.NoError(t, store.SaveHeadlessRun(session.HeadlessRun{
		ID:              "run-cancel-denied",
		LastHeartbeatAt: time.Now().UTC(),
		Hostname:        "foreign-host",
		PID:             os.Getpid(),
		Status:          session.HeadlessStatusRunning,
	}))

	policy := permission.DefaultPolicy()
	policy.SetMode(permission.OperationExecute, permission.ModeDeny)
	ctx := permission.ContextWithPolicy(t.Context(), &policy)
	ctx = permission.ContextWithAuditDir(ctx, t.TempDir())

	err := runHeadlessCommand(ctx, cliOptions{cancelHeadlessID: "run-cancel-denied"}, store)
	require.Error(t, err)
	assert.True(t, permission.ErrDenied(err))
	assert.Contains(t, err.Error(), "permission.execute.deny")

	loaded, loadErr := store.LoadHeadlessRun("run-cancel-denied")
	require.NoError(t, loadErr)
	assert.Equal(t, session.HeadlessStatusRunning, loaded.Status)
	assert.Empty(t, loaded.CancellationReason)
}

func TestRunHeadlessCommandPermissionPolicyDeniesRecoverBeforeStateMutation(t *testing.T) {
	t.Parallel()

	store := session.NewStore(t.TempDir())
	require.NoError(t, store.SaveHeadlessRun(session.HeadlessRun{
		ID:              "run-recover-denied",
		LastHeartbeatAt: time.Now().UTC(),
		Hostname:        "foreign-host",
		PID:             1 << 30,
		Status:          session.HeadlessStatusRunning,
	}))

	policy := permission.DefaultPolicy()
	policy.SetMode(permission.OperationWrite, permission.ModeDeny)
	ctx := permission.ContextWithPolicy(t.Context(), &policy)
	ctx = permission.ContextWithAuditDir(ctx, t.TempDir())

	err := runHeadlessCommand(ctx, cliOptions{recoverHeadless: true}, store)
	require.Error(t, err)
	assert.True(t, permission.ErrDenied(err))
	assert.Contains(t, err.Error(), "permission.write.deny")

	loaded, loadErr := store.LoadHeadlessRun("run-recover-denied")
	require.NoError(t, loadErr)
	assert.Equal(t, session.HeadlessStatusRunning, loaded.Status)
	assert.Empty(t, loaded.StaleReason)
}

func TestListSessionsPermissionPolicyDeniesRead(t *testing.T) {
	t.Parallel()

	store := session.NewStore(t.TempDir())
	require.NoError(t, store.Save(session.New("gpt-test", nil)))

	policy := permission.DefaultPolicy()
	policy.SetMode(permission.OperationRead, permission.ModeDeny)

	auditDir := t.TempDir()
	ctx := permission.ContextWithPolicy(t.Context(), &policy)
	ctx = permission.ContextWithAuditDir(ctx, auditDir)

	err := listSessions(ctx, store, "")
	require.Error(t, err)
	require.True(t, permission.ErrDenied(err))
	assert.Contains(t, err.Error(), "permission.read.deny")

	auditData, readErr := os.ReadFile(filepath.Join(auditDir, "side_effects.jsonl"))
	require.NoError(t, readErr)
	assert.Contains(t, string(auditData), "list sessions")
	assert.Contains(t, string(auditData), "permission.read.deny")
}

func TestSearchSessionsPermissionPolicyDeniesIndexWrite(t *testing.T) {
	t.Parallel()

	store := session.NewStore(t.TempDir())
	saved := session.New("gpt-test", nil)
	require.NoError(t, store.Save(saved))
	require.NoError(t, os.Remove(filepath.Join(store.Dir(), ".session-search-index")))

	policy := permission.DefaultPolicy()
	policy.SetMode(permission.OperationWrite, permission.ModeDeny)

	auditDir := t.TempDir()
	ctx := permission.ContextWithPolicy(t.Context(), &policy)
	ctx = permission.ContextWithAuditDir(ctx, auditDir)

	err := searchSessions(ctx, store, "OAuth")
	require.Error(t, err)
	require.True(t, permission.ErrDenied(err))
	assert.Contains(t, err.Error(), "permission.write.deny")

	_, statErr := os.Stat(filepath.Join(store.Dir(), ".session-search-index"))
	require.ErrorIs(t, statErr, os.ErrNotExist)

	auditData, readErr := os.ReadFile(filepath.Join(auditDir, "side_effects.jsonl"))
	require.NoError(t, readErr)
	assert.Contains(t, string(auditData), "update session search index")
	assert.Contains(t, string(auditData), "permission.write.deny")
}

func TestStatusHeadlessRunPermissionPolicyDeniesRead(t *testing.T) {
	t.Parallel()

	store := session.NewStore(t.TempDir())
	require.NoError(t, store.SaveHeadlessRun(session.HeadlessRun{
		ID:     "run-status-denied",
		Status: session.HeadlessStatusRunning,
	}))

	policy := permission.DefaultPolicy()
	policy.SetMode(permission.OperationRead, permission.ModeDeny)

	ctx := permission.ContextWithPolicy(t.Context(), &policy)
	ctx = permission.ContextWithAuditDir(ctx, t.TempDir())

	err := statusHeadlessRun(ctx, store, "run-status-denied")
	require.Error(t, err)
	require.True(t, permission.ErrDenied(err))
	assert.Contains(t, err.Error(), "permission.read.deny")
}

func TestStatusHeadlessRunRejectsWhitespacePaddedID(t *testing.T) {
	t.Parallel()

	store := session.NewStore(t.TempDir())
	err := statusHeadlessRun(t.Context(), store, " run-123 ")

	require.ErrorContains(t, err, "headless id must not have leading or trailing whitespace")
}

func TestReconcileHeadlessRunsAtStartupMarksStaleRuns(t *testing.T) {
	t.Parallel()

	store := session.NewStore(t.TempDir())
	staleAt := time.Now().Add(-time.Hour).UTC()
	hostname, err := os.Hostname()
	require.NoError(t, err)

	require.NoError(t, store.SaveHeadlessRun(session.HeadlessRun{
		ID:              "run-startup-stale",
		StartedAt:       staleAt,
		UpdatedAt:       staleAt,
		LastHeartbeatAt: staleAt,
		Hostname:        hostname,
		PID:             os.Getpid(),
		Status:          session.HeadlessStatusRunning,
	}))

	reconcileHeadlessRunsAtStartup(t.Context(), cliOptions{}, store)

	loaded, err := store.LoadHeadlessRun("run-startup-stale")
	require.NoError(t, err)
	assert.Equal(t, session.HeadlessStatusOrphaned, loaded.Status)
	assert.Contains(t, loaded.OrphanedReason, "no heartbeat since")
}

func TestReconcileHeadlessRunsAtStartupMarksDeadLocalPIDStale(t *testing.T) {
	t.Parallel()

	store := session.NewStore(t.TempDir())
	hostname, err := os.Hostname()
	require.NoError(t, err)

	require.NoError(t, store.SaveHeadlessRun(session.HeadlessRun{
		ID:              "run-startup-dead-pid",
		LastHeartbeatAt: time.Now().UTC(),
		Hostname:        hostname,
		PID:             1 << 30,
		Status:          session.HeadlessStatusRunning,
	}))

	reconcileHeadlessRunsAtStartup(t.Context(), cliOptions{}, store)

	loaded, err := store.LoadHeadlessRun("run-startup-dead-pid")
	require.NoError(t, err)
	assert.Equal(t, session.HeadlessStatusStale, loaded.Status)
	assert.Contains(t, loaded.StaleReason, "is not running")
	assert.NotNil(t, loaded.CompletedAt)
	require.NotNil(t, loaded.ExitCode)
	assert.Equal(t, 1, *loaded.ExitCode)
}

func TestReconcileHeadlessRunsAtStartupPermissionPolicyDeniesWrite(t *testing.T) {
	t.Parallel()

	store := session.NewStore(t.TempDir())
	staleAt := time.Now().Add(-time.Hour).UTC()
	hostname, err := os.Hostname()
	require.NoError(t, err)

	require.NoError(t, store.SaveHeadlessRun(session.HeadlessRun{
		ID:              "run-startup-denied",
		StartedAt:       staleAt,
		UpdatedAt:       staleAt,
		LastHeartbeatAt: staleAt,
		Hostname:        hostname,
		PID:             os.Getpid(),
		Status:          session.HeadlessStatusRunning,
	}))

	policy := permission.DefaultPolicy()
	policy.SetMode(permission.OperationWrite, permission.ModeDeny)

	auditDir := t.TempDir()
	ctx := permission.ContextWithPolicy(t.Context(), &policy)
	ctx = permission.ContextWithAuditDir(ctx, auditDir)

	reconcileHeadlessRunsAtStartup(ctx, cliOptions{}, store)

	loaded, err := store.LoadHeadlessRun("run-startup-denied")
	require.NoError(t, err)
	assert.Equal(t, session.HeadlessStatusRunning, loaded.Status)

	auditData, readErr := os.ReadFile(filepath.Join(auditDir, "side_effects.jsonl"))
	require.NoError(t, readErr)
	assert.Contains(t, string(auditData), "reconcile headless runs at startup")
	assert.Contains(t, string(auditData), "permission.write.deny")
}

func TestRun_HeadlessRecordsLoadStateFailure(t *testing.T) { //nolint:paralleltest // mutates process-global os.Args and flag.CommandLine.
	oldArgs := os.Args
	oldCommandLine := flag.CommandLine

	defer func() {
		os.Args = oldArgs
		flag.CommandLine = oldCommandLine
	}()

	sessionDir := filepath.Join(t.TempDir(), "sessions")
	headlessID := "run-load-state-failure"
	agentName := "missing-agent-gh82-load-state-failure"

	flag.CommandLine = flag.NewFlagSet("atteler", flag.ContinueOnError)
	flag.CommandLine.SetOutput(io.Discard)

	os.Args = []string{
		"atteler",
		"--session-dir", sessionDir,
		"--headless",
		"--headless-id", headlessID,
		"--agent", agentName,
		"hello",
	}

	err := run(context.Background())
	require.ErrorContains(t, err, "unknown agent")

	loaded, loadErr := session.NewStore(sessionDir).LoadHeadlessRun(headlessID)
	require.NoError(t, loadErr)
	assert.Equal(t, session.HeadlessStatusFailed, loaded.Status)
	assert.Equal(t, agentName, loaded.Agent)
	assert.Contains(t, loaded.Error, "unknown agent")
	assert.Equal(t, loaded.Error, loaded.TerminalReason)
	require.NotNil(t, loaded.ExitCode)
	assert.Equal(t, 1, *loaded.ExitCode)

	events, eventsErr := session.NewStore(sessionDir).ReadHeadlessEvents(headlessID)
	require.NoError(t, eventsErr)
	require.Len(t, events, 3)
	assert.Equal(t, session.HeadlessEventStarted, events[0].Type)
	assert.Equal(t, session.HeadlessEventUserMessage, events[1].Type)
	assert.Equal(t, session.HeadlessEventFailed, events[2].Type)
	assert.Contains(t, events[2].Error, "unknown agent")
}

func TestRunRecoverHeadlessPrintsNewlyRecoveredRun(t *testing.T) { //nolint:paralleltest // mutates process-global os.Args, flag.CommandLine, stdout.
	sessionDir := t.TempDir()
	store := session.NewStore(sessionDir)
	hostname, err := os.Hostname()
	require.NoError(t, err)

	require.NoError(t, store.SaveHeadlessRun(session.HeadlessRun{
		ID:              "run-cli-recover",
		LastHeartbeatAt: time.Now().UTC(),
		Hostname:        hostname,
		PID:             1 << 30,
		Status:          session.HeadlessStatusRunning,
	}))

	oldArgs := os.Args
	oldCommandLine := flag.CommandLine

	defer func() {
		os.Args = oldArgs
		flag.CommandLine = oldCommandLine
	}()

	flag.CommandLine = flag.NewFlagSet("atteler", flag.ContinueOnError)
	flag.CommandLine.SetOutput(io.Discard)

	os.Args = []string{"atteler", "--session-dir", sessionDir, "session", "recover-headless"}

	out := captureSessionCommandStdout(t, func() {
		require.NoError(t, run(context.Background()))
	})

	assert.Contains(t, out, "run-cli-recover")
	assert.Contains(t, out, "status=stale")
	assert.Contains(t, out, "process pid")
	assert.NotContains(t, out, "No recoverable")
}

func TestRecoverHeadlessRunsPrintsExpiredLease(t *testing.T) { //nolint:paralleltest // captures process-global stdout.
	store := session.NewStore(t.TempDir())
	now := time.Now().UTC()
	writeHeadlessRunMetadataForCLITest(t, store, session.HeadlessRun{
		ID:              "run-cli-recover-expired",
		StartedAt:       now.Add(-time.Minute),
		UpdatedAt:       now,
		LastHeartbeatAt: now,
		LeaseExpiresAt:  now.Add(-time.Second),
		Status:          session.HeadlessStatusRunning,
	})

	out := captureSessionCommandStdout(t, func() {
		require.NoError(t, recoverHeadlessRuns(t.Context(), store))
	})

	assert.Contains(t, out, "run-cli-recover-expired")
	assert.Contains(t, out, "status=expired")
	assert.Contains(t, out, "terminal_reason=lease expired at")
	assert.NotContains(t, out, "No recoverable")
}

func TestRunStatusHeadlessPrintsRequestedRun(t *testing.T) { //nolint:paralleltest // mutates process-global os.Args, flag.CommandLine, stdout.
	sessionDir := t.TempDir()
	store := session.NewStore(sessionDir)
	completedAt := time.Now().UTC()
	require.NoError(t, store.SaveHeadlessRun(session.HeadlessRun{
		ID:          "run-cli-status",
		SessionID:   "session-cli-status",
		CompletedAt: &completedAt,
		Status:      session.HeadlessStatusCompleted,
	}))

	out := runSessionCommandWithArgs(t, "--session-dir", sessionDir, "session", "status-headless", "run-cli-status")

	assert.Contains(t, out, "run-cli-status")
	assert.Contains(t, out, "status=completed")
	assert.Contains(t, out, "session=session-cli-status")
}

func TestRunCancelHeadlessPrintsAndPersistsCanceledStatus(t *testing.T) { //nolint:paralleltest // mutates process-global os.Args, flag.CommandLine, stdout.
	sessionDir := t.TempDir()
	store := session.NewStore(sessionDir)
	require.NoError(t, store.SaveHeadlessRun(session.HeadlessRun{
		ID:              "run-cli-cancel",
		LastHeartbeatAt: time.Now().UTC(),
		Hostname:        "foreign-host",
		PID:             os.Getpid(),
		Status:          session.HeadlessStatusRunning,
	}))

	out := runSessionCommandWithArgs(t, "--session-dir", sessionDir, "session", "cancel-headless", "run-cli-cancel")

	assert.Contains(t, out, "run-cli-cancel")
	assert.Contains(t, out, "status=canceled")
	assert.Contains(t, out, "cancellation_reason=canceled by atteler session cancel-headless")

	loaded, err := store.LoadHeadlessRun("run-cli-cancel")
	require.NoError(t, err)
	assert.Equal(t, session.HeadlessStatusCanceled, loaded.Status)
	assert.Equal(t, "canceled by atteler session cancel-headless", loaded.CancellationReason)
}

func TestStatusHeadlessRunPrintsReconciledOrphanedStatus(t *testing.T) { //nolint:paralleltest // captures process-global stdout.
	store := session.NewStore(t.TempDir())
	staleAt := time.Now().Add(-time.Hour).UTC()
	hostname, err := os.Hostname()
	require.NoError(t, err)

	require.NoError(t, store.SaveHeadlessRun(session.HeadlessRun{
		ID:              "run-status",
		StartedAt:       staleAt,
		UpdatedAt:       staleAt,
		LastHeartbeatAt: staleAt,
		Hostname:        hostname,
		PID:             os.Getpid(),
		Status:          session.HeadlessStatusRunning,
	}))

	out := captureSessionCommandStdout(t, func() {
		require.NoError(t, statusHeadlessRun(t.Context(), store, "run-status"))
	})

	assert.Contains(t, out, "run-status")
	assert.Contains(t, out, "status=orphaned")
	assert.Contains(t, out, "orphaned_reason=no heartbeat since")
}

func TestStatusHeadlessRunPrintsCorruptMetadata(t *testing.T) { //nolint:paralleltest // captures process-global stdout.
	store := session.NewStore(t.TempDir())
	require.NoError(t, os.MkdirAll(filepath.Join(store.Dir(), "headless"), 0o750))
	require.NoError(t, os.WriteFile(filepath.Join(store.Dir(), "headless", "run-corrupt.json"), []byte("{not-json"), 0o600))

	out := captureSessionCommandStdout(t, func() {
		require.NoError(t, statusHeadlessRun(t.Context(), store, "run-corrupt"))
	})

	assert.Contains(t, out, "run-corrupt")
	assert.Contains(t, out, "status=corrupt")
	assert.Contains(t, out, "parse headless")
}

func TestListHeadlessRunsPrintsOrphanedAndCorruptRuns(t *testing.T) { //nolint:paralleltest // captures process-global stdout.
	store := session.NewStore(t.TempDir())
	require.NoError(t, os.MkdirAll(filepath.Join(store.Dir(), "headless"), 0o750))

	hostname, err := os.Hostname()
	require.NoError(t, err)

	require.NoError(t, store.SaveHeadlessRun(session.HeadlessRun{
		ID:              "run-orphaned",
		LastHeartbeatAt: time.Now().Add(-time.Hour).UTC(),
		Hostname:        hostname,
		OrphanedReason:  "no heartbeat since test",
		PID:             os.Getpid(),
		Status:          session.HeadlessStatusOrphaned,
	}))
	require.NoError(t, os.WriteFile(filepath.Join(store.Dir(), "headless", "run-corrupt.json"), []byte("{not-json"), 0o600))

	out := captureSessionCommandStdout(t, func() {
		require.NoError(t, listHeadlessRuns(t.Context(), store, "", ""))
	})

	assert.Contains(t, out, "run-orphaned")
	assert.Contains(t, out, "status=orphaned")
	assert.Contains(t, out, "orphaned_reason=no heartbeat since test")
	assert.Contains(t, out, "run-corrupt")
	assert.Contains(t, out, "status=corrupt")
}

func TestListHeadlessRunsPrintsAllRunsAndAppliesStatusFilter(t *testing.T) { //nolint:paralleltest // captures process-global stdout.
	store := session.NewStore(t.TempDir())
	completedAt := time.Now().UTC()

	require.NoError(t, store.SaveHeadlessRun(session.HeadlessRun{
		ID:          "run-list-completed",
		CompletedAt: &completedAt,
		Status:      session.HeadlessStatusCompleted,
	}))
	require.NoError(t, store.SaveHeadlessRun(session.HeadlessRun{
		ID:     "run-list-failed",
		Status: session.HeadlessStatusFailed,
		Error:  "provider failed",
	}))

	allOut := captureSessionCommandStdout(t, func() {
		require.NoError(t, listHeadlessRuns(t.Context(), store, "", ""))
	})

	assert.Contains(t, allOut, "run-list-completed")
	assert.Contains(t, allOut, "status=completed")
	assert.Contains(t, allOut, "run-list-failed")
	assert.Contains(t, allOut, "status=failed")

	failedOut := captureSessionCommandStdout(t, func() {
		require.NoError(t, listHeadlessRuns(t.Context(), store, "failed", ""))
	})

	assert.NotContains(t, failedOut, "run-list-completed")
	assert.Contains(t, failedOut, "run-list-failed")
}

func TestParseHeadlessListFilterNormalizesStatus(t *testing.T) {
	t.Parallel()

	filter, err := parseHeadlessListFilter(" Failed ", "")
	require.NoError(t, err)

	assert.True(t, filter.matches(session.HeadlessRun{Status: session.HeadlessStatusFailed}))
	assert.False(t, filter.matches(session.HeadlessRun{Status: session.HeadlessStatusCompleted}))
}

func TestListHeadlessRunsAppliesMaxAgeFilter(t *testing.T) { //nolint:paralleltest // writes raw timestamps and captures process-global stdout.
	store := session.NewStore(t.TempDir())
	now := time.Now().UTC()
	oldCompletedAt := now.Add(-48 * time.Hour)

	writeHeadlessRunMetadataForCLITest(t, store, session.HeadlessRun{
		ID:        "run-list-old",
		StartedAt: now.Add(-49 * time.Hour),
		UpdatedAt: now.Add(-48 * time.Hour),
		Status:    session.HeadlessStatusCompleted,
	})
	writeHeadlessRunMetadataForCLITest(t, store, session.HeadlessRun{
		ID:        "run-list-recent",
		StartedAt: now.Add(-2 * time.Hour),
		UpdatedAt: now.Add(-time.Hour),
		Status:    session.HeadlessStatusCompleted,
	})
	require.NoError(t, store.SaveHeadlessRun(session.HeadlessRun{
		ID:          "run-list-old-completed-recent-metadata",
		CompletedAt: &oldCompletedAt,
		Status:      session.HeadlessStatusCompleted,
	}))

	out := captureSessionCommandStdout(t, func() {
		require.NoError(t, listHeadlessRuns(t.Context(), store, "", "24h"))
	})

	assert.NotContains(t, out, "run-list-old")
	assert.NotContains(t, out, "run-list-old-completed-recent-metadata")
	assert.Contains(t, out, "run-list-recent")
}

func TestCleanupHeadlessRunsPrintsAndRemovesExpiredTerminalRun(t *testing.T) { //nolint:paralleltest // captures process-global stdout.
	store := session.NewStore(t.TempDir())
	oldCompletedAt := time.Now().Add(-48 * time.Hour).UTC()
	recentCompletedAt := time.Now().UTC()

	require.NoError(t, store.SaveHeadlessRun(session.HeadlessRun{
		ID:          "run-cleanup-cli-old",
		CompletedAt: &oldCompletedAt,
		Status:      session.HeadlessStatusCompleted,
	}))
	require.NoError(t, store.AppendHeadlessLog("run-cleanup-cli-old", "old log\n"))
	require.NoError(t, store.SaveHeadlessRun(session.HeadlessRun{
		ID:          "run-cleanup-cli-recent",
		CompletedAt: &recentCompletedAt,
		Status:      session.HeadlessStatusCompleted,
	}))

	out := captureSessionCommandStdout(t, func() {
		require.NoError(t, cleanupHeadlessRuns(t.Context(), store, "24h"))
	})

	assert.Contains(t, out, "run-cleanup-cli-old")
	assert.Contains(t, out, "status=expired")
	assert.NotContains(t, out, "run-cleanup-cli-recent")

	_, err := store.LoadHeadlessRun("run-cleanup-cli-old")
	require.ErrorIs(t, err, os.ErrNotExist)
	_, err = store.LoadHeadlessRun("run-cleanup-cli-recent")
	require.NoError(t, err)
}

func TestCancelHeadlessRunPrintsAndPersistsCanceledStatus(t *testing.T) { //nolint:paralleltest // captures process-global stdout.
	store := session.NewStore(t.TempDir())
	heartbeat := time.Now().UTC()
	require.NoError(t, store.SaveHeadlessRun(session.HeadlessRun{
		ID:              "run-cancel-command",
		LastHeartbeatAt: heartbeat,
		Hostname:        "foreign-host",
		PID:             os.Getpid(),
		Status:          session.HeadlessStatusRunning,
	}))

	out := captureSessionCommandStdout(t, func() {
		require.NoError(t, cancelHeadlessRun(t.Context(), store, "run-cancel-command"))
	})

	assert.Contains(t, out, "run-cancel-command")
	assert.Contains(t, out, "status=canceled")
	assert.Contains(t, out, "cancellation_reason=canceled by atteler session cancel-headless")

	loaded, err := store.LoadHeadlessRun("run-cancel-command")
	require.NoError(t, err)
	assert.Equal(t, session.HeadlessStatusCanceled, loaded.Status)
	assert.Equal(t, "canceled by atteler session cancel-headless", loaded.CancellationReason)
}

func TestCancelHeadlessRunReturnsErrorForCorruptMetadata(t *testing.T) {
	t.Parallel()

	store := session.NewStore(t.TempDir())
	require.NoError(t, os.MkdirAll(filepath.Join(store.Dir(), "headless"), 0o750))
	require.NoError(t, os.WriteFile(filepath.Join(store.Dir(), "headless", "run-corrupt-cancel.json"), []byte("{not-json"), 0o600))

	err := cancelHeadlessRun(t.Context(), store, "run-corrupt-cancel")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "parse headless")
}

func TestRetryHeadlessRunStartsRetryAndMarksParentRetried(t *testing.T) { //nolint:paralleltest // mutates package retry launcher and captures stdout.
	store := session.NewStore(t.TempDir())
	completedAt := time.Now().UTC()
	require.NoError(t, store.SaveHeadlessRun(session.HeadlessRun{
		ID:          "run-retry-parent",
		CompletedAt: &completedAt,
		Prompt:      "try again",
		Model:       "gpt-test",
		Agent:       "executor",
		Status:      session.HeadlessStatusFailed,
		Error:       "provider failed",
	}))

	oldStarter := startHeadlessRetryProcess
	startHeadlessRetryProcess = func(_ context.Context, gotStore *session.Store, parent session.HeadlessRun, newID string) (headlessRetryProcess, error) {
		require.Equal(t, store.Dir(), gotStore.Dir())
		require.Equal(t, "run-retry-parent", parent.ID)
		require.Equal(t, "run-retry-child", newID)

		args, err := headlessRetryArgs(gotStore, parent, newID)
		require.NoError(t, err)
		assert.Contains(t, args, "--headless")
		assert.Contains(t, args, "--headless-id")
		assert.Contains(t, args, "run-retry-child")
		assert.Contains(t, args, "try again")

		return headlessRetryProcess{
			Args:       args,
			ID:         newID,
			Executable: "/tmp/atteler",
			PID:        4242,
		}, nil
	}

	defer func() { startHeadlessRetryProcess = oldStarter }()

	out := captureSessionCommandStdout(t, func() {
		require.NoError(t, retryHeadlessRun(t.Context(), store, "run-retry-parent", "run-retry-child"))
	})

	assert.Contains(t, out, "run-retry-parent")
	assert.Contains(t, out, "status=retried")
	assert.Contains(t, out, "superseded_by=run-retry-child")
	assert.Contains(t, out, "retry_run=run-retry-child")
	assert.Contains(t, out, "pid=4242")

	loaded, err := store.LoadHeadlessRun("run-retry-parent")
	require.NoError(t, err)
	assert.Equal(t, session.HeadlessStatusRetried, loaded.Status)
	assert.Equal(t, "run-retry-child", loaded.SupersededByRunID)
	assert.Equal(t, []string{"run-retry-child"}, loaded.ChildRunIDs)
	require.NotNil(t, loaded.RetriedAt)

	events, err := store.ReadHeadlessEvents("run-retry-parent")
	require.NoError(t, err)
	require.Len(t, events, 1)
	assert.Equal(t, session.HeadlessEventRetried, events[0].Type)
	assert.Equal(t, session.HeadlessStatusRetried, events[0].Status)
	assert.Equal(t, "run-retry-child", events[0].SupersededByRunID)
	assert.Equal(t, "run-retry-child", events[0].Metadata["child_run_id"])
}

func TestRetryHeadlessRunGeneratesRetryIDWhenNotProvided(t *testing.T) { //nolint:paralleltest // mutates package retry launcher and captures stdout.
	store := session.NewStore(t.TempDir())
	completedAt := time.Now().UTC()
	require.NoError(t, store.SaveHeadlessRun(session.HeadlessRun{
		ID:          "run-retry-parent-default-id",
		CompletedAt: &completedAt,
		Prompt:      "try again",
		Status:      session.HeadlessStatusFailed,
	}))

	oldStarter := startHeadlessRetryProcess

	var generatedID string

	startHeadlessRetryProcess = func(_ context.Context, _ *session.Store, parent session.HeadlessRun, newID string) (headlessRetryProcess, error) {
		require.Equal(t, "run-retry-parent-default-id", parent.ID)
		require.NoError(t, session.ValidateHeadlessID(newID))
		assert.True(t, strings.HasPrefix(newID, "run-retry-parent-default-id-retry-"))

		generatedID = newID

		return headlessRetryProcess{
			Args:       []string{"--headless-id", newID},
			ID:         newID,
			Executable: "/tmp/atteler",
			PID:        4243,
		}, nil
	}

	defer func() { startHeadlessRetryProcess = oldStarter }()

	out := captureSessionCommandStdout(t, func() {
		require.NoError(t, retryHeadlessRun(t.Context(), store, "run-retry-parent-default-id", ""))
	})

	require.NotEmpty(t, generatedID)
	assert.Contains(t, out, "retry_run="+generatedID)
	assert.Contains(t, out, "superseded_by="+generatedID)

	loaded, err := store.LoadHeadlessRun("run-retry-parent-default-id")
	require.NoError(t, err)
	assert.Equal(t, session.HeadlessStatusRetried, loaded.Status)
	assert.Equal(t, generatedID, loaded.SupersededByRunID)
	assert.Equal(t, []string{generatedID}, loaded.ChildRunIDs)
}

func TestRetryHeadlessRunRejectsExistingRetryIDBeforeStartingProcess(t *testing.T) { //nolint:paralleltest // mutates package retry launcher.
	store := session.NewStore(t.TempDir())
	completedAt := time.Now().UTC()
	require.NoError(t, store.SaveHeadlessRun(session.HeadlessRun{
		ID:          "run-retry-parent",
		CompletedAt: &completedAt,
		Prompt:      "try again",
		Status:      session.HeadlessStatusFailed,
	}))
	require.NoError(t, store.SaveHeadlessRun(session.HeadlessRun{
		ID:     "run-retry-child",
		Status: session.HeadlessStatusCompleted,
	}))

	oldStarter := startHeadlessRetryProcess
	started := false
	startHeadlessRetryProcess = func(context.Context, *session.Store, session.HeadlessRun, string) (headlessRetryProcess, error) {
		started = true
		return headlessRetryProcess{}, nil
	}

	defer func() { startHeadlessRetryProcess = oldStarter }()

	err := retryHeadlessRun(t.Context(), store, "run-retry-parent", "run-retry-child")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "already exists")
	assert.False(t, started)

	loaded, err := store.LoadHeadlessRun("run-retry-parent")
	require.NoError(t, err)
	assert.Equal(t, session.HeadlessStatusFailed, loaded.Status)
}

func TestRetryHeadlessRunRejectsWhitespacePaddedRetryIDBeforeStartingProcess(t *testing.T) { //nolint:paralleltest // mutates package retry launcher.
	store := session.NewStore(t.TempDir())
	completedAt := time.Now().UTC()
	require.NoError(t, store.SaveHeadlessRun(session.HeadlessRun{
		ID:          "run-retry-parent-whitespace",
		CompletedAt: &completedAt,
		Prompt:      "try again",
		Status:      session.HeadlessStatusFailed,
	}))

	oldStarter := startHeadlessRetryProcess
	started := false
	startHeadlessRetryProcess = func(context.Context, *session.Store, session.HeadlessRun, string) (headlessRetryProcess, error) {
		started = true
		return headlessRetryProcess{}, nil
	}

	defer func() { startHeadlessRetryProcess = oldStarter }()

	err := retryHeadlessRun(t.Context(), store, "run-retry-parent-whitespace", " run-retry-child ")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "headless id must not have leading or trailing whitespace")
	assert.False(t, started)

	loaded, err := store.LoadHeadlessRun("run-retry-parent-whitespace")
	require.NoError(t, err)
	assert.Equal(t, session.HeadlessStatusFailed, loaded.Status)
}

func TestRetryHeadlessRunRejectsNonRetryableStatusesBeforeStartingProcess(t *testing.T) { //nolint:paralleltest // mutates package retry launcher.
	store := session.NewStore(t.TempDir())
	now := time.Now().UTC()

	tests := []struct {
		status session.HeadlessStatus
		name   string
	}{
		{name: "running", status: session.HeadlessStatusRunning},
		{name: "orphaned", status: session.HeadlessStatusOrphaned},
		{name: "retried", status: session.HeadlessStatusRetried},
		{name: "superseded", status: session.HeadlessStatusSuperseded},
	}

	oldStarter := startHeadlessRetryProcess
	started := false
	startHeadlessRetryProcess = func(context.Context, *session.Store, session.HeadlessRun, string) (headlessRetryProcess, error) {
		started = true
		return headlessRetryProcess{}, nil
	}

	defer func() { startHeadlessRetryProcess = oldStarter }()

	for _, tt := range tests {
		id := "run-retry-nonretryable-" + tt.name
		require.NoError(t, store.SaveHeadlessRun(session.HeadlessRun{
			ID:              id,
			Status:          tt.status,
			TerminalReason:  string(tt.status),
			LastHeartbeatAt: now,
			Hostname:        "foreign-host",
			PID:             os.Getpid(),
		}), tt.name)

		err := retryHeadlessRun(t.Context(), store, id, id+"-child")
		require.Error(t, err, tt.name)
		assert.Contains(t, err.Error(), "only completed, failed, canceled, timed_out, stale, or expired runs can be retried", tt.name)
	}

	assert.False(t, started)
}

func TestHeadlessRetryArgsPreservesDashPrefixedPrompt(t *testing.T) {
	t.Parallel()

	store := session.NewStore(t.TempDir())
	args, err := headlessRetryArgs(store, session.HeadlessRun{
		ID:     "run-retry-dash-prompt",
		Prompt: "--explain this flag-like prompt",
		Status: session.HeadlessStatusFailed,
	}, "run-retry-dash-prompt-child")
	require.NoError(t, err)

	require.GreaterOrEqual(t, len(args), 2)
	assert.Equal(t, "--", args[len(args)-2])
	assert.Equal(t, "--explain this flag-like prompt", args[len(args)-1])
}

func TestHeadlessRetryArgsReusesRecordedCommandArgs(t *testing.T) {
	t.Parallel()

	store := session.NewStore(t.TempDir())
	args, err := headlessRetryArgs(store, session.HeadlessRun{
		ID:     "run-retry-recorded-command",
		Prompt: "fallback prompt",
		CommandArgs: []string{
			"/usr/local/bin/atteler",
			"--config", "atteler.yaml",
			"--session-dir", "/old/session-dir",
			"--headless",
			"--headless-id=run-retry-recorded-command",
			"--model", "gpt-test",
			"chat", "once", "--", "--headless-id", "prompt text",
		},
		Status: session.HeadlessStatusFailed,
	}, "run-retry-recorded-command-child")
	require.NoError(t, err)

	assert.NotContains(t, args, "/usr/local/bin/atteler")
	assert.Contains(t, args, "--config")
	assert.Contains(t, args, "atteler.yaml")
	assert.Contains(t, args, "--session-dir")
	assert.Contains(t, args, store.Dir())
	assert.NotContains(t, args, "/old/session-dir")
	assert.Contains(t, args, "--headless")
	assert.Contains(t, args, "--headless-id=run-retry-recorded-command-child")
	assert.NotContains(t, args, "--headless-id=run-retry-recorded-command")
	assert.Contains(t, args, "--model")
	assert.Contains(t, args, "gpt-test")
	assert.Equal(t, []string{"--", "--headless-id", "prompt text"}, args[len(args)-3:])
}

func TestHeadlessRetryArgsPreservesPrivateLogWhenReusingRecordedCommandArgs(t *testing.T) {
	t.Parallel()

	store := session.NewStore(t.TempDir())
	args, err := headlessRetryArgs(store, session.HeadlessRun{
		ID:          "run-retry-private-recorded-command",
		Prompt:      "fallback prompt",
		PrivateLogs: true,
		CommandArgs: []string{
			"/usr/local/bin/atteler",
			"--session-dir", "/old/session-dir",
			"--headless",
			"--headless-id", "run-retry-private-recorded-command",
			"chat", "once", "recorded prompt",
		},
		Status: session.HeadlessStatusFailed,
	}, "run-retry-private-recorded-command-child")
	require.NoError(t, err)

	assert.Contains(t, args, "--headless-private-log")
	assert.Contains(t, args, "recorded prompt")
}

func TestHeadlessRetryArgsRewritesRecordedBoolFlags(t *testing.T) {
	t.Parallel()

	store := session.NewStore(t.TempDir())
	args, err := headlessRetryArgs(store, session.HeadlessRun{
		ID:          "run-retry-bool-flags",
		Prompt:      "fallback prompt",
		PrivateLogs: true,
		CommandArgs: []string{
			"/usr/local/bin/atteler",
			"--headless=false",
			"--headless-private-log=false",
			"--headless-id", "run-retry-bool-flags",
			"chat", "once", "recorded prompt",
		},
		Status: session.HeadlessStatusFailed,
	}, "run-retry-bool-flags-child")
	require.NoError(t, err)

	assert.Contains(t, args, "--headless=true")
	assert.Contains(t, args, "--headless-private-log=true")
	assert.NotContains(t, args, "--headless=false")
	assert.NotContains(t, args, "--headless-private-log=false")
}

func TestHeadlessRetryArgsDoesNotEnablePrivateLogsFromRecordedFalseFlag(t *testing.T) {
	t.Parallel()

	store := session.NewStore(t.TempDir())
	args, err := headlessRetryArgs(store, session.HeadlessRun{
		ID:     "run-retry-private-false-recorded-command",
		Prompt: "fallback prompt",
		CommandArgs: []string{
			"/usr/local/bin/atteler",
			"--headless",
			"--headless-private-log=false",
			"--headless-id", "run-retry-private-false-recorded-command",
			"chat", "once", "recorded prompt",
		},
		Status: session.HeadlessStatusFailed,
	}, "run-retry-private-false-recorded-command-child")
	require.NoError(t, err)

	assert.Contains(t, args, "--headless")
	assert.NotContains(t, args, "--headless-private-log")
	assert.NotContains(t, args, "--headless-private-log=true")
	assert.NotContains(t, args, "--headless-private-log=false")
	assert.Contains(t, args, "recorded prompt")
}

func TestHeadlessRetryArgsRewritesSingleDashRecordedFlags(t *testing.T) {
	t.Parallel()

	store := session.NewStore(t.TempDir())
	args, err := headlessRetryArgs(store, session.HeadlessRun{
		ID:     "run-retry-single-dash-command",
		Prompt: "fallback prompt",
		CommandArgs: []string{
			"/usr/local/bin/atteler",
			"-session-dir=/old/session-dir",
			"-headless",
			"-headless-id", "run-retry-single-dash-command",
			"chat", "once", "recorded prompt",
		},
		Status: session.HeadlessStatusFailed,
	}, "run-retry-single-dash-command-child")
	require.NoError(t, err)

	assert.Contains(t, args, "--session-dir="+store.Dir())
	assert.NotContains(t, args, "-session-dir=/old/session-dir")
	assert.Contains(t, args, "--headless")
	assert.Contains(t, args, "--headless-id")
	assert.Contains(t, args, "run-retry-single-dash-command-child")
	assert.NotContains(t, args, "run-retry-single-dash-command")
}

func TestHeadlessRetryArgsFallsBackWhenRecordedCommandWasRedacted(t *testing.T) {
	t.Parallel()

	store := session.NewStore(t.TempDir())
	args, err := headlessRetryArgs(store, session.HeadlessRun{
		ID:     "run-retry-redacted-command",
		Prompt: "fallback prompt",
		Model:  "gpt-test",
		Agent:  "executor",
		CommandArgs: []string{
			"/usr/local/bin/atteler",
			"--api-key", "[REDACTED]",
			"--session-dir", "/old/session-dir",
			"--headless-id", "run-retry-redacted-command",
			"chat", "once", "recorded prompt",
		},
		Status: session.HeadlessStatusFailed,
	}, "run-retry-redacted-command-child")
	require.NoError(t, err)

	assert.NotContains(t, args, "--api-key")
	assert.NotContains(t, args, "[REDACTED]")
	assert.Contains(t, args, "--session-dir")
	assert.Contains(t, args, store.Dir())
	assert.Contains(t, args, "--headless")
	assert.Contains(t, args, "run-retry-redacted-command-child")
	assert.Contains(t, args, "--model")
	assert.Contains(t, args, "gpt-test")
	assert.Contains(t, args, "--agent")
	assert.Contains(t, args, "executor")
	assert.Equal(t, []string{"--", "fallback prompt"}, args[len(args)-2:])
}

func TestHeadlessRetryExecutableFallsBackWhenRecordedPathWasRedacted(t *testing.T) {
	t.Parallel()

	executable, err := headlessRetryExecutable(session.HeadlessRun{
		Executable: filepath.Join(t.TempDir(), headlessRetryRedactedValue, "atteler"),
	})
	require.NoError(t, err)
	assert.NotEmpty(t, executable)
	assert.NotContains(t, executable, headlessRetryRedactedValue)
}

func TestHeadlessRetryExecutableFallsBackWhenRecordedPathIsMissing(t *testing.T) {
	t.Parallel()

	missing := filepath.Join(t.TempDir(), "missing-atteler")
	executable, err := headlessRetryExecutable(session.HeadlessRun{Executable: missing})
	require.NoError(t, err)
	assert.NotEmpty(t, executable)
	assert.NotEqual(t, missing, executable)
}

func TestHeadlessRetryWorkingDirIgnoresRedactedPath(t *testing.T) {
	t.Parallel()

	assert.Empty(t, headlessRetryWorkingDir(session.HeadlessRun{
		CWD: filepath.Join(t.TempDir(), headlessRetryRedactedValue),
	}))

	dir := t.TempDir()
	assert.Equal(t, dir, headlessRetryWorkingDir(session.HeadlessRun{CWD: dir}))
}

func TestHeadlessRetryWorkingDirIgnoresMissingPath(t *testing.T) {
	t.Parallel()

	assert.Empty(t, headlessRetryWorkingDir(session.HeadlessRun{
		CWD: filepath.Join(t.TempDir(), "missing-workdir"),
	}))
}

func TestHeadlessRetryEnvOverridesExistingRetryMetadata(t *testing.T) {
	t.Parallel()

	base := []string{
		"PATH=/bin",
		headlessParentRunIDEnv + "=stale-parent",
		headlessRetryOfRunIDEnv + "=stale-retry",
		headlessRetryCountEnv + "=99",
		headlessParentRunIDEnv + "=duplicate-stale-parent",
		"ATTELER_OTHER=value",
	}

	env := headlessRetryEnv(base, session.HeadlessRun{ID: "run-retry-parent", RetryCount: 2})

	assert.Contains(t, env, "PATH=/bin")
	assert.Contains(t, env, "ATTELER_OTHER=value")
	assert.Contains(t, env, headlessParentRunIDEnv+"=run-retry-parent")
	assert.Contains(t, env, headlessRetryOfRunIDEnv+"=run-retry-parent")
	assert.Contains(t, env, headlessRetryCountEnv+"=3")
	assert.NotContains(t, env, headlessParentRunIDEnv+"=stale-parent")
	assert.NotContains(t, env, headlessParentRunIDEnv+"=duplicate-stale-parent")
	assert.NotContains(t, env, headlessRetryOfRunIDEnv+"=stale-retry")
	assert.NotContains(t, env, headlessRetryCountEnv+"=99")
	assert.Equal(t, 1, countEnvKey(env, headlessParentRunIDEnv))
	assert.Equal(t, 1, countEnvKey(env, headlessRetryOfRunIDEnv))
	assert.Equal(t, 1, countEnvKey(env, headlessRetryCountEnv))
}

func TestStreamHeadlessLogDrainsReconciliationLog(t *testing.T) { //nolint:paralleltest // captures process-global stdout.
	store := session.NewStore(t.TempDir())
	staleAt := time.Now().Add(-time.Hour).UTC()

	require.NoError(t, store.AppendHeadlessLog("run-stream-stale", "prelude\n"))
	require.NoError(t, store.SaveHeadlessRun(session.HeadlessRun{
		ID:              "run-stream-stale",
		StartedAt:       staleAt,
		UpdatedAt:       staleAt,
		LastHeartbeatAt: staleAt,
		Hostname:        "foreign-host",
		Status:          session.HeadlessStatusRunning,
	}))

	out := captureSessionCommandStdout(t, func() {
		require.NoError(t, streamHeadlessLog(context.Background(), store, "run-stream-stale"))
	})

	assert.Contains(t, out, "prelude")
	assert.Contains(t, out, "stale")
	assert.Contains(t, out, "no heartbeat since")
}

func TestStreamHeadlessLogDrainsLogWrittenAfterTerminalStatus(t *testing.T) { //nolint:paralleltest // captures process-global stdout.
	store := session.NewStore(t.TempDir())
	completedAt := time.Now().UTC()
	require.NoError(t, store.SaveHeadlessRun(session.HeadlessRun{
		ID:          "run-stream-late-terminal-log",
		CompletedAt: &completedAt,
		Status:      session.HeadlessStatusCompleted,
	}))

	appendErr := make(chan error, 1)

	go func() {
		time.Sleep(10 * time.Millisecond)

		appendErr <- store.AppendHeadlessLog("run-stream-late-terminal-log", "late terminal log\n")
	}()

	out := captureSessionCommandStdout(t, func() {
		require.NoError(t, streamHeadlessLog(context.Background(), store, "run-stream-late-terminal-log"))
	})

	require.NoError(t, <-appendErr)

	assert.Contains(t, out, "late terminal log")
}

func TestStreamHeadlessLogDrainsLargeTerminalBacklog(t *testing.T) { //nolint:paralleltest // captures process-global stdout.
	store := session.NewStore(t.TempDir())
	completedAt := time.Now().UTC()
	require.NoError(t, store.SaveHeadlessRun(session.HeadlessRun{
		ID:          "run-stream-large-terminal-backlog",
		CompletedAt: &completedAt,
		Status:      session.HeadlessStatusCompleted,
	}))
	require.NoError(t, store.AppendHeadlessLog(
		"run-stream-large-terminal-backlog",
		strings.Repeat("x", 220*1024)+"\ntail-marker\n",
	))

	out := captureSessionCommandStdout(t, func() {
		require.NoError(t, streamHeadlessLog(context.Background(), store, "run-stream-large-terminal-backlog"))
	})

	assert.Contains(t, out, "tail-marker")
}

func TestStreamHeadlessLogKeepsFollowingOrphanedLiveProcess(t *testing.T) { //nolint:paralleltest // captures process-global stdout.
	store := session.NewStore(t.TempDir())
	staleAt := time.Now().Add(-time.Hour).UTC()
	hostname, err := os.Hostname()
	require.NoError(t, err)

	run := session.HeadlessRun{
		ID:              "run-stream-orphaned",
		StartedAt:       staleAt,
		UpdatedAt:       staleAt,
		LastHeartbeatAt: staleAt,
		Hostname:        hostname,
		PID:             os.Getpid(),
		Status:          session.HeadlessStatusRunning,
	}
	require.NoError(t, store.SaveHeadlessRun(run))
	require.NoError(t, store.AppendHeadlessLog("run-stream-orphaned", "prelude\n"))
	require.NoError(t, store.SaveHeadlessRun(run))

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Millisecond)
	defer cancel()

	out := captureSessionCommandStdout(t, func() {
		require.ErrorIs(t, streamHeadlessLog(ctx, store, "run-stream-orphaned"), context.DeadlineExceeded)
	})

	assert.Contains(t, out, "prelude")

	loaded, err := store.LoadHeadlessRun("run-stream-orphaned")
	require.NoError(t, err)
	assert.Equal(t, session.HeadlessStatusOrphaned, loaded.Status)
}

func captureSessionCommandStdout(t *testing.T, fn func()) string {
	t.Helper()

	reader, writer, err := os.Pipe()
	require.NoError(t, err)

	oldStdout := os.Stdout
	os.Stdout = writer

	defer func() {
		os.Stdout = oldStdout
		_ = writer.Close()
		_ = reader.Close()
	}()

	readData := make(chan []byte, 1)
	readErrs := make(chan error, 1)

	go func() {
		data, err := io.ReadAll(reader)
		readData <- data

		readErrs <- err
	}()

	fn()

	require.NoError(t, writer.Close())

	data := <-readData
	readErr := <-readErrs

	require.NoError(t, readErr)
	require.NoError(t, reader.Close())

	return strings.TrimSpace(string(data))
}

func countEnvKey(env []string, key string) int {
	prefix := key + "="
	count := 0

	for _, entry := range env {
		if strings.HasPrefix(entry, prefix) {
			count++
		}
	}

	return count
}

func runSessionCommandWithArgs(t *testing.T, args ...string) string {
	t.Helper()

	oldArgs := os.Args
	oldCommandLine := flag.CommandLine

	defer func() {
		os.Args = oldArgs
		flag.CommandLine = oldCommandLine
	}()

	flag.CommandLine = flag.NewFlagSet("atteler", flag.ContinueOnError)
	flag.CommandLine.SetOutput(io.Discard)

	os.Args = append([]string{"atteler"}, args...)

	return captureSessionCommandStdout(t, func() {
		require.NoError(t, run(context.Background()))
	})
}

func writeHeadlessRunMetadataForCLITest(t *testing.T, store *session.Store, run session.HeadlessRun) {
	t.Helper()

	require.NoError(t, session.ValidateHeadlessID(run.ID))
	require.NoError(t, os.MkdirAll(filepath.Join(store.Dir(), "headless"), 0o750))

	data, err := json.Marshal(run)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(filepath.Join(store.Dir(), "headless", run.ID+".json"), append(data, '\n'), 0o600))
}
