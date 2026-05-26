//go:build windows

package skill

import (
	"fmt"
	"os"

	"golang.org/x/sys/windows"
)

func lockLearningFile(file *os.File) error {
	var overlapped windows.Overlapped
	if err := windows.LockFileEx(
		windows.Handle(file.Fd()),
		windows.LOCKFILE_EXCLUSIVE_LOCK,
		0,
		1,
		0,
		&overlapped,
	); err != nil {
		return fmt.Errorf("skill learning: lock state %s: %w", file.Name(), err)
	}

	return nil
}

func unlockLearningFile(file *os.File) error {
	var overlapped windows.Overlapped
	if err := windows.UnlockFileEx(
		windows.Handle(file.Fd()),
		0,
		1,
		0,
		&overlapped,
	); err != nil {
		return fmt.Errorf("skill learning: unlock state %s: %w", file.Name(), err)
	}

	return nil
}
