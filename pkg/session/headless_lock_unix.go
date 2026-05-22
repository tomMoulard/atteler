//go:build darwin || linux || freebsd || netbsd || openbsd || dragonfly

package session

import (
	"errors"
	"fmt"
	"os"
	"syscall"
	"time"
)

func lockHeadlessFile(file *os.File) error {
	return lockSessionFile(file, "headless lock")
}

func unlockHeadlessFile(file *os.File) error {
	return unlockSessionFile(file, "headless lock")
}

func lockSessionFile(file *os.File, label string) error {
	//nolint:gosec // os.File descriptors are OS-provided small integers used by flock.
	if err := syscall.Flock(int(file.Fd()), syscall.LOCK_EX); err != nil {
		return fmt.Errorf("session: flock %s %s: %w", label, file.Name(), err)
	}

	return nil
}

func unlockSessionFile(file *os.File, label string) error {
	//nolint:gosec // os.File descriptors are OS-provided small integers used by flock.
	if err := syscall.Flock(int(file.Fd()), syscall.LOCK_UN); err != nil {
		return fmt.Errorf("session: unlock %s %s: %w", label, file.Name(), err)
	}

	return nil
}

func headlessProcessAlive(pid int) bool {
	process, err := os.FindProcess(pid)
	if err != nil {
		return false
	}

	err = process.Signal(syscall.Signal(0))

	return err == nil || errors.Is(err, syscall.EPERM)
}

func headlessProcessGroupID(pid int) int {
	pgid, err := syscall.Getpgid(pid)
	if err != nil {
		return 0
	}

	return pgid
}

func headlessProcessGroupAlive(processGroupID int) bool {
	if processGroupID <= 0 {
		return false
	}

	err := syscall.Kill(-processGroupID, 0)

	return err == nil || errors.Is(err, syscall.EPERM)
}

func signalHeadlessProcess(pid int) error {
	process, err := os.FindProcess(pid)
	if err != nil {
		return fmt.Errorf("find process %d: %w", pid, err)
	}

	if err := process.Signal(syscall.SIGTERM); err != nil &&
		!errors.Is(err, os.ErrProcessDone) &&
		!errors.Is(err, syscall.ESRCH) {
		return fmt.Errorf("signal process %d: %w", pid, err)
	}

	time.Sleep(defaultHeadlessCancelKillGrace)

	if !headlessProcessAlive(pid) {
		return nil
	}

	if err := process.Signal(syscall.SIGKILL); err != nil &&
		!errors.Is(err, os.ErrProcessDone) &&
		!errors.Is(err, syscall.ESRCH) {
		return fmt.Errorf("kill process %d: %w", pid, err)
	}

	return nil
}

func signalHeadlessProcessGroup(pid, processGroupID int) error {
	if processGroupID <= 0 {
		return signalHeadlessProcess(pid)
	}

	if err := syscall.Kill(-processGroupID, syscall.SIGTERM); err != nil && !errors.Is(err, syscall.ESRCH) {
		return fmt.Errorf("signal process group %d: %w", processGroupID, err)
	}

	time.Sleep(defaultHeadlessCancelKillGrace)

	if !headlessProcessGroupAlive(processGroupID) {
		return nil
	}

	if currentProcessGroupID := headlessProcessGroupID(pid); currentProcessGroupID > 0 && currentProcessGroupID != processGroupID {
		return nil
	}

	if err := syscall.Kill(-processGroupID, syscall.SIGKILL); err != nil && !errors.Is(err, syscall.ESRCH) {
		return fmt.Errorf("kill process group %d: %w", processGroupID, err)
	}

	return nil
}
