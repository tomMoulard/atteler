//go:build darwin || linux || freebsd || netbsd || openbsd || dragonfly

package config

import (
	"fmt"
	"os"
	"syscall"
)

func lockStateFile(file *os.File) error {
	//nolint:gosec // os.File descriptors are OS-provided small integers used by flock.
	if err := syscall.Flock(int(file.Fd()), syscall.LOCK_EX); err != nil {
		return fmt.Errorf("flock %s: %w", file.Name(), err)
	}

	return nil
}

func unlockStateFile(file *os.File) error {
	//nolint:gosec // os.File descriptors are OS-provided small integers used by flock.
	if err := syscall.Flock(int(file.Fd()), syscall.LOCK_UN); err != nil {
		return fmt.Errorf("unlock %s: %w", file.Name(), err)
	}

	return nil
}

func syncDir(dir string) error {
	file, err := os.Open(dir)
	if err != nil {
		return fmt.Errorf("open dir %s: %w", dir, err)
	}
	defer file.Close()

	if err := file.Sync(); err != nil {
		return fmt.Errorf("sync dir %s: %w", dir, err)
	}

	return nil
}
