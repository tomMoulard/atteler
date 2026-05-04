// Package shell runs explicit local shell commands for Atteler workflows.
package shell

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"runtime"
	"strings"
	"time"
)

const defaultTimeout = 30 * time.Second

// Options controls one explicit shell command invocation.
type Options struct {
	Env     map[string]string
	Command string
	Dir     string
	Timeout time.Duration
}

// Result captures stdout, stderr, and timing for a command run.
//
//nolint:govet // Public field order groups lifecycle, timing, and captured output.
type Result struct {
	StartedAt time.Time
	Duration  time.Duration
	Stdout    string
	Stderr    string
	ExitError string
}

// RunBash runs Command through bash -lc with a timeout. The command string is
// intentionally caller-provided: this package is for explicit local CLI actions,
// not for executing model-suggested commands without user intent.
func RunBash(ctx context.Context, opts Options) (Result, error) {
	command := strings.TrimSpace(opts.Command)
	if command == "" {
		return Result{}, errors.New("shell: command is required")
	}

	if ctx == nil {
		return Result{}, errors.New("shell: context is required")
	}

	timeout := opts.Timeout
	if timeout <= 0 {
		timeout = defaultTimeout
	}

	runCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	bin, args, err := bashInvocation(command)
	if err != nil {
		return Result{}, err
	}

	started := time.Now().UTC()

	cmd := exec.CommandContext(runCtx, bin, args...)
	if strings.TrimSpace(opts.Dir) != "" {
		cmd.Dir = opts.Dir
	}

	cmd.Env = mergeEnv(opts.Env)

	var stdout, stderr bytes.Buffer

	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	runErr := cmd.Run()

	result := Result{
		StartedAt: started,
		Duration:  time.Since(started),
		Stdout:    stdout.String(),
		Stderr:    stderr.String(),
	}
	if runCtx.Err() != nil {
		return result, fmt.Errorf("shell: bash command timed out after %s: %w", timeout, runCtx.Err())
	}

	if runErr != nil {
		result.ExitError = runErr.Error()
		return result, fmt.Errorf("shell: bash command failed: %w", runErr)
	}

	return result, nil
}

func bashInvocation(command string) (bin string, args []string, err error) {
	if runtime.GOOS == "windows" {
		bin, err := exec.LookPath("bash")
		if err != nil {
			return "", nil, fmt.Errorf("shell: bash executable not found: %w", err)
		}

		return bin, []string{"-lc", command}, nil
	}

	return "bash", []string{"-lc", command}, nil
}

func mergeEnv(extra map[string]string) []string {
	env := os.Environ()

	for key, value := range extra {
		key = strings.TrimSpace(key)
		if key == "" {
			continue
		}

		env = append(env, key+"="+value)
	}

	return env
}
