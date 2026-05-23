//go:build darwin || linux || freebsd || netbsd || openbsd || dragonfly

package skill

import (
	"fmt"
	"os"
	"syscall"
)

func lockLearningFile(file *os.File) error {
	//nolint:gosec // os.File descriptors are OS-provided small integers used by flock.
	if err := syscall.Flock(int(file.Fd()), syscall.LOCK_EX); err != nil {
		return fmt.Errorf("skill learning: lock state %s: %w", file.Name(), err)
	}

	return nil
}

func unlockLearningFile(file *os.File) error {
	//nolint:gosec // os.File descriptors are OS-provided small integers used by flock.
	if err := syscall.Flock(int(file.Fd()), syscall.LOCK_UN); err != nil {
		return fmt.Errorf("skill learning: unlock state %s: %w", file.Name(), err)
	}

	return nil
}
