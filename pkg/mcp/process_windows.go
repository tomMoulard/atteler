//go:build windows

package mcp

import (
	"fmt"
	"os"
)

func terminateProcess(process *os.Process) error {
	if process == nil {
		return nil
	}

	if err := process.Kill(); err != nil {
		return fmt.Errorf("kill process: %w", err)
	}

	return nil
}
