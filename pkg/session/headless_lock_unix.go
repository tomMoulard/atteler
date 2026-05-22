//go:build darwin || linux || freebsd || netbsd || openbsd || dragonfly

package session

import (
	"errors"
	"fmt"
	"os"
	"syscall"
)

func lockHeadlessFile(file *os.File) error {
	//nolint:gosec // os.File descriptors are OS-provided small integers used by flock.
	if err := syscall.Flock(int(file.Fd()), syscall.LOCK_EX); err != nil {
		return fmt.Errorf("session: flock headless lock %s: %w", file.Name(), err)
	}

	return nil
}

func unlockHeadlessFile(file *os.File) error {
	//nolint:gosec // os.File descriptors are OS-provided small integers used by flock.
	if err := syscall.Flock(int(file.Fd()), syscall.LOCK_UN); err != nil {
		return fmt.Errorf("session: unlock headless lock %s: %w", file.Name(), err)
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
