//go:build darwin || linux || freebsd || netbsd || openbsd || dragonfly

package session

import (
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestStore_CancelHeadlessRunTerminatesRecordedProcessGroup(t *testing.T) {
	t.Parallel()

	store := NewStore(t.TempDir())
	childPIDPath := store.Path("child.pid")

	cmd := exec.CommandContext(t.Context(), "sh", "-c", "sleep 30 & echo $! > \"$1\"; wait", "sh", childPIDPath)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	require.NoError(t, cmd.Start())

	var (
		processGroupID int
		waited         bool
	)

	done := make(chan error, 1)

	go func() {
		done <- cmd.Wait()
	}()

	defer func() {
		if waited {
			return
		}

		if processGroupID > 0 {
			if err := syscall.Kill(-processGroupID, syscall.SIGKILL); err != nil {
				t.Logf("cleanup kill process group %d: %v", processGroupID, err)
			}
		} else if err := cmd.Process.Kill(); err != nil {
			t.Logf("cleanup kill pid %d: %v", cmd.Process.Pid, err)
		}

		select {
		case <-done:
		case <-time.After(time.Second):
		}
	}()

	childPID := waitForHeadlessTestPIDFile(t, childPIDPath)
	processGroupID = headlessProcessGroupID(cmd.Process.Pid)
	require.NotZero(t, processGroupID)
	assert.Equal(t, processGroupID, headlessProcessGroupID(childPID))

	hostname, err := os.Hostname()
	require.NoError(t, err)
	require.NoError(t, store.SaveHeadlessRun(HeadlessRun{
		ID:             "run-cancel-process-group",
		Hostname:       hostname,
		PID:            cmd.Process.Pid,
		ProcessGroupID: processGroupID,
		Status:         HeadlessStatusRunning,
	}))

	canceled, err := store.CancelHeadlessRun("run-cancel-process-group", "operator requested stop")
	require.NoError(t, err)
	assert.Equal(t, HeadlessStatusCanceled, canceled.Status)

	select {
	case <-done:
		waited = true
	case <-time.After(2 * time.Second):
		t.Fatalf("cancel did not terminate process group %d", processGroupID)
	}

	require.Eventually(t, func() bool {
		return !headlessProcessAlive(childPID)
	}, 2*time.Second, 25*time.Millisecond, "child pid %d should exit with the recorded process group", childPID)
}

func TestSignalHeadlessProcessGroupKillsRemainingMembersAfterLeaderExits(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	childPIDPath := filepath.Join(dir, "child.pid")
	executable, executableErr := os.Executable()
	require.NoError(t, executableErr)

	cmd := exec.CommandContext(
		t.Context(),
		"sh",
		"-c",
		"\"$2\" -test.run=TestHeadlessSIGTERMHelperProcess & echo $! > \"$1\"; wait",
		"sh",
		childPIDPath,
		executable,
	)

	cmd.Env = append(os.Environ(), "ATTELER_HEADLESS_TEST_HELPER=ignore-sigterm")
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	require.NoError(t, cmd.Start())

	var (
		processGroupID int
		waited         bool
	)

	done := make(chan error, 1)

	go func() {
		done <- cmd.Wait()
	}()

	defer func() {
		if waited {
			return
		}

		if processGroupID > 0 {
			if err := syscall.Kill(-processGroupID, syscall.SIGKILL); err != nil {
				t.Logf("cleanup kill process group %d: %v", processGroupID, err)
			}
		} else if err := cmd.Process.Kill(); err != nil {
			t.Logf("cleanup kill pid %d: %v", cmd.Process.Pid, err)
		}

		select {
		case <-done:
		case <-time.After(time.Second):
		}
	}()

	childPID := waitForHeadlessTestPIDFile(t, childPIDPath)
	processGroupID = headlessProcessGroupID(cmd.Process.Pid)
	require.NotZero(t, processGroupID)
	assert.Equal(t, processGroupID, headlessProcessGroupID(childPID))

	require.NoError(t, signalHeadlessProcessGroup(cmd.Process.Pid, processGroupID))

	select {
	case <-done:
		waited = true
	case <-time.After(2 * time.Second):
		t.Fatalf("process group leader pid %d did not exit", cmd.Process.Pid)
	}

	require.Eventually(t, func() bool {
		return !headlessProcessAlive(childPID)
	}, 2*time.Second, 25*time.Millisecond, "SIGTERM-ignoring child pid %d should be killed with the recorded process group", childPID)
}

func TestStore_CancelHeadlessRunTerminatesRecordedProcessGroupAfterLeaderExits(t *testing.T) {
	t.Parallel()

	store := NewStore(t.TempDir())
	childPIDPath := store.Path("child.pid")
	executable, executableErr := os.Executable()
	require.NoError(t, executableErr)

	cmd := exec.CommandContext(
		t.Context(),
		"sh",
		"-c",
		"\"$2\" -test.run=TestHeadlessSIGTERMHelperProcess & echo $! > \"$1\"; exit 0",
		"sh",
		childPIDPath,
		executable,
	)

	cmd.Env = append(os.Environ(), "ATTELER_HEADLESS_TEST_HELPER=ignore-sigterm")
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	require.NoError(t, cmd.Start())

	leaderPID := cmd.Process.Pid
	processGroupID := leaderPID
	childPID := waitForHeadlessTestPIDFile(t, childPIDPath)

	require.Eventually(t, func() bool {
		return headlessProcessGroupID(childPID) == processGroupID
	}, time.Second, 10*time.Millisecond, "child pid %d should inherit process group %d", childPID, processGroupID)

	defer func() {
		if !headlessProcessAlive(childPID) {
			return
		}

		if err := syscall.Kill(-processGroupID, syscall.SIGKILL); err != nil {
			t.Logf("cleanup kill process group %d: %v", processGroupID, err)
		}
	}()

	require.NoError(t, cmd.Wait())
	require.False(t, headlessProcessAlive(leaderPID), "leader pid %d should be reaped before cancellation", leaderPID)

	hostname, err := os.Hostname()
	require.NoError(t, err)
	require.NoError(t, store.SaveHeadlessRun(HeadlessRun{
		ID:             "run-cancel-dead-leader-process-group",
		Hostname:       hostname,
		PID:            leaderPID,
		ProcessGroupID: processGroupID,
		Status:         HeadlessStatusRunning,
	}))

	status, err := store.HeadlessRunStatus("run-cancel-dead-leader-process-group")
	require.NoError(t, err)
	assert.Equal(t, HeadlessStatusOrphaned, status.Status)
	assert.Contains(t, status.OrphanedReason, "process group")

	canceled, err := store.CancelHeadlessRun("run-cancel-dead-leader-process-group", "operator requested stop")
	require.NoError(t, err)
	assert.Equal(t, HeadlessStatusCanceled, canceled.Status)

	require.Eventually(t, func() bool {
		return !headlessProcessAlive(childPID)
	}, 2*time.Second, 25*time.Millisecond, "child pid %d should exit when cancel signals the recorded process group", childPID)

	loaded, err := store.LoadHeadlessRun("run-cancel-dead-leader-process-group")
	require.NoError(t, err)
	assert.Equal(t, HeadlessStatusCanceled, loaded.Status)
}

func TestTerminateHeadlessRunProcessFallsBackToPIDForNonLeaderProcessGroup(t *testing.T) {
	t.Parallel()

	store := NewStore(t.TempDir())
	childPIDPath := store.Path("child.pid")

	cmd := exec.CommandContext(t.Context(), "sh", "-c", "sleep 30 & echo $! > \"$1\"; sleep 30", "sh", childPIDPath)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	require.NoError(t, cmd.Start())

	var (
		processGroupID int
		waited         bool
	)

	done := make(chan error, 1)

	go func() {
		done <- cmd.Wait()
	}()

	defer func() {
		if waited {
			return
		}

		if processGroupID > 0 {
			if err := syscall.Kill(-processGroupID, syscall.SIGKILL); err != nil {
				t.Logf("cleanup kill process group %d: %v", processGroupID, err)
			}
		} else if err := cmd.Process.Kill(); err != nil {
			t.Logf("cleanup kill pid %d: %v", cmd.Process.Pid, err)
		}

		select {
		case <-done:
		case <-time.After(time.Second):
		}
	}()

	childPID := waitForHeadlessTestPIDFile(t, childPIDPath)
	processGroupID = headlessProcessGroupID(cmd.Process.Pid)
	require.NotZero(t, processGroupID)
	require.NotEqual(t, childPID, processGroupID)
	assert.Equal(t, processGroupID, headlessProcessGroupID(childPID))

	hostname, err := os.Hostname()
	require.NoError(t, err)
	require.NoError(t, terminateHeadlessRunProcess(HeadlessRun{
		ID:             "run-cancel-non-leader-process-group",
		Hostname:       hostname,
		PID:            childPID,
		ProcessGroupID: processGroupID,
		Status:         HeadlessStatusRunning,
	}))

	time.Sleep(100 * time.Millisecond)
	assert.True(t, headlessProcessAlive(cmd.Process.Pid), "shell group leader should survive PID-only fallback")
}

func TestSignalHeadlessProcessIgnoresAlreadyExitedPID(t *testing.T) {
	t.Parallel()

	deadPID := 99999999
	if headlessProcessAlive(deadPID) {
		t.Skipf("test pid %d unexpectedly exists", deadPID)
	}

	require.NoError(t, signalHeadlessProcess(deadPID))
}

func TestTerminateHeadlessRunProcessKillsProcessThatHandlesSIGTERM(t *testing.T) {
	t.Parallel()

	readyPath := filepath.Join(t.TempDir(), "ready")
	executable, executableErr := os.Executable()
	require.NoError(t, executableErr)

	cmd := exec.CommandContext(t.Context(), executable, "-test.run=TestHeadlessSIGTERMHelperProcess")

	cmd.Env = append(
		os.Environ(),
		"ATTELER_HEADLESS_TEST_HELPER=ignore-sigterm",
		"ATTELER_HEADLESS_TEST_READY="+readyPath,
	)
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

		if killErr := cmd.Process.Kill(); killErr != nil {
			t.Logf("cleanup kill pid %d: %v", cmd.Process.Pid, killErr)
		}

		select {
		case <-done:
		case <-time.After(time.Second):
		}
	}()

	require.Eventually(t, func() bool {
		_, statErr := os.Stat(readyPath)

		return statErr == nil
	}, time.Second, 10*time.Millisecond)

	hostname, err := os.Hostname()
	require.NoError(t, err)
	require.NoError(t, terminateHeadlessRunProcess(HeadlessRun{
		ID:       "run-kill-sigterm-ignoring-process",
		Hostname: hostname,
		PID:      cmd.Process.Pid,
		Status:   HeadlessStatusRunning,
	}))

	select {
	case waitErr := <-done:
		waited = true

		require.Error(t, waitErr)
	case <-time.After(2 * time.Second):
		t.Fatalf("cancel did not kill SIGTERM-ignoring pid %d", cmd.Process.Pid)
	}
}

//nolint:paralleltest // helper process owns process-global signal handling and blocks until killed.
func TestHeadlessSIGTERMHelperProcess(t *testing.T) {
	if os.Getenv("ATTELER_HEADLESS_TEST_HELPER") != "ignore-sigterm" {
		return
	}

	signals := make(chan os.Signal, 1)
	signal.Notify(signals, syscall.SIGTERM)

	if readyPath := os.Getenv("ATTELER_HEADLESS_TEST_READY"); readyPath != "" {
		//nolint:gosec // test helper writes to a temp path provided by its parent test.
		require.NoError(t, os.WriteFile(readyPath, []byte("ready\n"), 0o600))
	}

	select {}
}

func waitForHeadlessTestPIDFile(t *testing.T, path string) int {
	t.Helper()

	var raw string

	require.Eventually(t, func() bool {
		data, err := os.ReadFile(path)
		if err != nil {
			return false
		}

		raw = strings.TrimSpace(string(data))

		return raw != ""
	}, time.Second, 10*time.Millisecond)

	pid, err := strconv.Atoi(raw)
	require.NoError(t, err)

	return pid
}
