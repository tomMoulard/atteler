//go:build windows

package session

import (
	"errors"
	"fmt"
	"os"

	"golang.org/x/sys/windows"
)

func lockHeadlessFile(file *os.File) error {
	return lockSessionFile(file, "headless lock")
}

func unlockHeadlessFile(file *os.File) error {
	return unlockSessionFile(file, "headless lock")
}

func lockSessionFile(file *os.File, label string) error {
	var overlapped windows.Overlapped
	if err := windows.LockFileEx(
		windows.Handle(file.Fd()),
		windows.LOCKFILE_EXCLUSIVE_LOCK,
		0,
		1,
		0,
		&overlapped,
	); err != nil {
		return fmt.Errorf("session: lock %s %s: %w", label, file.Name(), err)
	}

	return nil
}

func unlockSessionFile(file *os.File, label string) error {
	var overlapped windows.Overlapped
	if err := windows.UnlockFileEx(
		windows.Handle(file.Fd()),
		0,
		1,
		0,
		&overlapped,
	); err != nil {
		return fmt.Errorf("session: unlock %s %s: %w", label, file.Name(), err)
	}

	return nil
}

func headlessProcessAlive(pid int) bool {
	const stillActive = 259

	handle, err := windows.OpenProcess(windows.PROCESS_QUERY_LIMITED_INFORMATION, false, uint32(pid))
	if err != nil {
		return errors.Is(err, windows.ERROR_ACCESS_DENIED)
	}
	defer func() {
		_ = windows.CloseHandle(handle)
	}()

	var code uint32
	if err := windows.GetExitCodeProcess(handle, &code); err != nil {
		return false
	}

	return code == stillActive
}

func headlessProcessGroupID(_ int) int {
	return 0
}

func headlessProcessGroupAlive(_ int) bool {
	return false
}

func signalHeadlessProcess(pid int) error {
	process, err := os.FindProcess(pid)
	if err != nil {
		return fmt.Errorf("find process %d: %w", pid, err)
	}

	if err := process.Kill(); err != nil && !errors.Is(err, os.ErrProcessDone) {
		return fmt.Errorf("kill process %d: %w", pid, err)
	}

	return nil
}

func signalHeadlessProcessGroup(pid int, _ int) error {
	return signalHeadlessProcess(pid)
}
