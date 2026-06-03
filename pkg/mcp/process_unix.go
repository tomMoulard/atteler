//go:build !windows

package mcp

import (
	"fmt"
	"os"
	"syscall"
)

func terminateProcess(process *os.Process) error {
	if process == nil {
		return nil
	}

	if err := process.Signal(syscall.SIGTERM); err != nil {
		return fmt.Errorf("signal SIGTERM: %w", err)
	}

	return nil
}
