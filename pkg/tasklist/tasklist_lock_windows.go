//go:build windows

package tasklist

import (
	"context"
	"errors"
	"fmt"
	"os"
	"time"

	"golang.org/x/sys/windows"
)

func lockTaskFile(ctx context.Context, file *os.File) error {
	for {
		var overlapped windows.Overlapped
		err := windows.LockFileEx(
			windows.Handle(file.Fd()),
			windows.LOCKFILE_EXCLUSIVE_LOCK|windows.LOCKFILE_FAIL_IMMEDIATELY,
			0,
			1,
			0,
			&overlapped,
		)
		if err == nil {
			return nil
		}
		if !errors.Is(err, windows.ERROR_LOCK_VIOLATION) {
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
	var overlapped windows.Overlapped
	if err := windows.UnlockFileEx(
		windows.Handle(file.Fd()),
		0,
		1,
		0,
		&overlapped,
	); err != nil {
		return fmt.Errorf("tasklist: unlock %s: %w", file.Name(), err)
	}

	return nil
}

func syncTaskDir(_ string) error {
	return nil
}
