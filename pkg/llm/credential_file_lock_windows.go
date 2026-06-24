//go:build windows

package llm

import (
	"fmt"
	"os"

	"golang.org/x/sys/windows"
)

func lockCredentialFile(file *os.File) error {
	var overlapped windows.Overlapped
	if err := windows.LockFileEx(
		windows.Handle(file.Fd()),
		windows.LOCKFILE_EXCLUSIVE_LOCK,
		0,
		1,
		0,
		&overlapped,
	); err != nil {
		return fmt.Errorf("credential lock %s: %w", redactCredentialLocation(file.Name()), err)
	}

	return nil
}

func unlockCredentialFile(file *os.File) error {
	var overlapped windows.Overlapped
	if err := windows.UnlockFileEx(
		windows.Handle(file.Fd()),
		0,
		1,
		0,
		&overlapped,
	); err != nil {
		return fmt.Errorf("credential unlock %s: %w", redactCredentialLocation(file.Name()), err)
	}

	return nil
}

func syncCredentialDir(_ string) error {
	return nil
}
