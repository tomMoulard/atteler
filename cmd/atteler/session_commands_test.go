package main

import (
	"context"
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
		require.NoError(t, listHeadlessRuns(t.Context(), store))
	})

	assert.Contains(t, out, "run-orphaned")
	assert.Contains(t, out, "status=orphaned")
	assert.Contains(t, out, "orphaned_reason=no heartbeat since test")
	assert.Contains(t, out, "run-corrupt")
	assert.Contains(t, out, "status=corrupt")
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

func TestStreamHeadlessLogDrainsReconciliationLog(t *testing.T) { //nolint:paralleltest // captures process-global stdout.
	store := session.NewStore(t.TempDir())
	staleAt := time.Now().Add(-time.Hour).UTC()
	require.NoError(t, store.SaveHeadlessRun(session.HeadlessRun{
		ID:              "run-stream-stale",
		StartedAt:       staleAt,
		UpdatedAt:       staleAt,
		LastHeartbeatAt: staleAt,
		Hostname:        "foreign-host",
		Status:          session.HeadlessStatusRunning,
	}))
	require.NoError(t, store.AppendHeadlessLog("run-stream-stale", "prelude\n"))

	out := captureSessionCommandStdout(t, func() {
		require.NoError(t, streamHeadlessLog(context.Background(), store, "run-stream-stale"))
	})

	assert.Contains(t, out, "prelude")
	assert.Contains(t, out, "stale")
	assert.Contains(t, out, "no process pid recorded")
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
