//go:build windows

package session

import (
	"fmt"
	"os"

	"golang.org/x/sys/windows"
)

func lockHeadlessFile(file *os.File) error {
	var overlapped windows.Overlapped
	if err := windows.LockFileEx(
		windows.Handle(file.Fd()),
		windows.LOCKFILE_EXCLUSIVE_LOCK,
		0,
		1,
		0,
		&overlapped,
	); err != nil {
		return fmt.Errorf("session: lock headless lock %s: %w", file.Name(), err)
	}

	return nil
}

func unlockHeadlessFile(file *os.File) error {
	var overlapped windows.Overlapped
	if err := windows.UnlockFileEx(
		windows.Handle(file.Fd()),
		0,
		1,
		0,
		&overlapped,
	); err != nil {
		return fmt.Errorf("session: unlock headless lock %s: %w", file.Name(), err)
	}

	return nil
}

func headlessProcessAlive(pid int) bool {
	const stillActive = 259

	handle, err := windows.OpenProcess(windows.PROCESS_QUERY_LIMITED_INFORMATION, false, uint32(pid))
	if err != nil {
		return false
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
