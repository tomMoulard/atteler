//go:build darwin || linux || freebsd || netbsd || openbsd || dragonfly

package tasklist

import (
	"context"
	"errors"
	"fmt"
	"os"
	"syscall"
	"time"
)

func lockTaskFile(ctx context.Context, file *os.File) error {
	//nolint:gosec // os.File descriptors are OS-provided small integers used by flock.
	fd := int(file.Fd())
	for {
		if err := syscall.Flock(fd, syscall.LOCK_EX|syscall.LOCK_NB); err == nil {
			return nil
		} else if !errors.Is(err, syscall.EWOULDBLOCK) && !errors.Is(err, syscall.EAGAIN) {
			return fmt.Errorf("tasklist: lock %s: %w", file.Name(), err)
		}

		timer := time.NewTimer(fileLockRetryDelay)
		select {
		case <-ctx.Done():
			if !timer.Stop() {
				<-timer.C
			}

			return fmt.Errorf("tasklist: context: %w", ctx.Err())
		case <-timer.C:
		}
	}
}

func unlockTaskFile(file *os.File) error {
	//nolint:gosec // os.File descriptors are OS-provided small integers used by flock.
	if err := syscall.Flock(int(file.Fd()), syscall.LOCK_UN); err != nil {
		return fmt.Errorf("tasklist: unlock %s: %w", file.Name(), err)
	}

	return nil
}

func syncTaskDir(dir string) error {
	// #nosec G703 -- task list paths are explicit user/session configuration.
	file, err := os.Open(dir)
	if err != nil {
		return fmt.Errorf("tasklist: open dir %s: %w", dir, err)
	}
	defer file.Close()

	if err := file.Sync(); err != nil {
		return fmt.Errorf("tasklist: sync dir %s: %w", dir, err)
	}

	return nil
}
