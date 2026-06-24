//go:build darwin || linux || freebsd || netbsd || openbsd || dragonfly

package llm

import (
	"fmt"
	"os"
	"syscall"
)

func lockCredentialFile(file *os.File) error {
	//nolint:gosec // os.File descriptors are OS-provided small integers used by flock.
	if err := syscall.Flock(int(file.Fd()), syscall.LOCK_EX); err != nil {
		return fmt.Errorf("credential lock flock %s: %w", redactCredentialLocation(file.Name()), err)
	}

	return nil
}

func unlockCredentialFile(file *os.File) error {
	//nolint:gosec // os.File descriptors are OS-provided small integers used by flock.
	if err := syscall.Flock(int(file.Fd()), syscall.LOCK_UN); err != nil {
		return fmt.Errorf("credential lock unlock %s: %w", redactCredentialLocation(file.Name()), err)
	}

	return nil
}

func syncCredentialDir(dir string) error {
	file, err := os.Open(dir)
	if err != nil {
		return fmt.Errorf("credential sync dir %s: %s", redactCredentialLocation(dir), redactCredentialPathError(err))
	}
	defer file.Close()

	if err := file.Sync(); err != nil {
		return fmt.Errorf("credential sync dir %s: %s", redactCredentialLocation(dir), redactCredentialPathError(err))
	}

	return nil
}
