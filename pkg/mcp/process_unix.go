//go:build !windows

package mcp

import (
	"fmt"
	"os"
	"os/exec"
	"syscall"
)

// configureProcessGroup starts the MCP server in its own process group so
// shutdown signals also reach children spawned by wrapper commands
// (sh, npx, uvx) instead of only the direct child.
func configureProcessGroup(cmd *exec.Cmd) {
	if cmd == nil {
		return
	}

	if cmd.SysProcAttr == nil {
		cmd.SysProcAttr = &syscall.SysProcAttr{}
	}

	cmd.SysProcAttr.Setpgid = true
}

func terminateProcess(process *os.Process) error {
	if process == nil {
		return nil
	}

	if signalProcessGroup(process, syscall.SIGTERM) == nil {
		return nil
	}

	if err := process.Signal(syscall.SIGTERM); err != nil {
		return fmt.Errorf("signal SIGTERM: %w", err)
	}

	return nil
}

func killProcess(process *os.Process) error {
	if process == nil {
		return nil
	}

	if signalProcessGroup(process, syscall.SIGKILL) == nil {
		return nil
	}

	if err := process.Kill(); err != nil {
		return fmt.Errorf("kill process: %w", err)
	}

	return nil
}

// signalProcessGroup signals the whole process group created via Setpgid; the
// group ID equals the direct child's PID. Callers fall back to signaling the
// direct child when the group is already gone or was never created.
func signalProcessGroup(process *os.Process, signal syscall.Signal) error {
	if process.Pid <= 0 {
		return fmt.Errorf("invalid process group id %d", process.Pid)
	}

	if err := syscall.Kill(-process.Pid, signal); err != nil {
		return fmt.Errorf("signal process group %d: %w", process.Pid, err)
	}

	return nil
}
