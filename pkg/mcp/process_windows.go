//go:build windows

package mcp

import (
	"fmt"
	"os"
	"os/exec"
)

// configureProcessGroup is a no-op on Windows; process termination relies on
// killing the direct child only.
func configureProcessGroup(_ *exec.Cmd) {}

func terminateProcess(process *os.Process) error {
	if process == nil {
		return nil
	}

	if err := process.Kill(); err != nil {
		return fmt.Errorf("kill process: %w", err)
	}

	return nil
}

func killProcess(process *os.Process) error {
	if process == nil {
		return nil
	}

	if err := process.Kill(); err != nil {
		return fmt.Errorf("kill process: %w", err)
	}

	return nil
}
