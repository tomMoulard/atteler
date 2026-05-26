//go:build windows

package llm

import (
	"fmt"
	"os"
)

func defaultOllamaProcessAlive(pid int) bool {
	if pid <= 0 {
		return false
	}

	_, err := os.FindProcess(pid)

	return err == nil
}

func defaultOllamaTerminateProcess(pid int) error {
	return defaultOllamaKillProcess(pid)
}

func defaultOllamaKillProcess(pid int) error {
	process, err := os.FindProcess(pid)
	if err != nil {
		return fmt.Errorf("ollama: find PID %d: %w", pid, err)
	}

	if err := process.Kill(); err != nil {
		return fmt.Errorf("ollama: kill PID %d: %w", pid, err)
	}

	return nil
}

func defaultOllamaProcessMatchesOwnership(ownership *OllamaDaemonOwnership) bool {
	return ownership != nil && ownership.PID > 0 && ollamaServeCommandRecorded(ownership.Command)
}
