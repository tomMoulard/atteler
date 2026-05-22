//go:build !windows

package llm

import (
	"errors"
	"fmt"
	"os"
	"runtime"
	"strings"
	"syscall"
)

func defaultOllamaProcessAlive(pid int) bool {
	if pid <= 0 {
		return false
	}

	err := syscall.Kill(pid, 0)

	return err == nil || errors.Is(err, syscall.EPERM)
}

func defaultOllamaTerminateProcess(pid int) error {
	process, err := os.FindProcess(pid)
	if err != nil {
		return fmt.Errorf("ollama: find PID %d: %w", pid, err)
	}

	if err := process.Signal(syscall.SIGTERM); err != nil && defaultOllamaProcessAlive(pid) {
		return fmt.Errorf("ollama: terminate PID %d: %w", pid, err)
	}

	return nil
}

func defaultOllamaKillProcess(pid int) error {
	process, err := os.FindProcess(pid)
	if err != nil {
		return fmt.Errorf("ollama: find PID %d: %w", pid, err)
	}

	if err := process.Kill(); err != nil && defaultOllamaProcessAlive(pid) {
		return fmt.Errorf("ollama: kill PID %d: %w", pid, err)
	}

	return nil
}

func defaultOllamaProcessMatchesOwnership(ownership *OllamaDaemonOwnership) bool {
	if ownership == nil || ownership.PID <= 0 {
		return false
	}

	if !ollamaServeCommandRecorded(ownership.Command) {
		return false
	}

	if runtime.GOOS != "linux" {
		return true
	}

	data, err := os.ReadFile(fmt.Sprintf("/proc/%d/cmdline", ownership.PID))
	if err != nil || len(data) == 0 {
		// Some Unix variants and restricted /proc mounts do not expose
		// argv. Keep the ownership record authoritative when verification is
		// unavailable; StopOwnedOllamaDaemon still validates the recorded
		// command and owner before signaling.
		return true
	}

	args := strings.Split(strings.TrimRight(string(data), "\x00"), "\x00")

	return ollamaServeCommandRecorded(args)
}
