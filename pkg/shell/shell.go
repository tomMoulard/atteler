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
	"sync"
	"time"
)

const defaultTimeout = 30 * time.Second

// Options controls one explicit shell command invocation.
type Options struct {
	Policy         *Policy
	Env            map[string]string
	Audit          AuditContext
	Command        string
	Dir            string
	Timeout        time.Duration
	MaxOutputBytes int64
}

// Result captures stdout, stderr, and timing for a command run.
//
//nolint:govet // Public field order groups lifecycle, timing, and captured output.
type Result struct {
	StartedAt       time.Time
	Duration        time.Duration
	Stdout          string
	Stderr          string
	ExitError       string
	OutputTruncated bool
}

// RunBash runs Command through bash -lc with a timeout. The command string is
// intentionally caller-provided: this package is for explicit local CLI actions,
// not for executing model-suggested commands without user intent.
func RunBash(ctx context.Context, opts Options) (Result, error) {
	if err := requireContext(ctx); err != nil {
		return Result{}, err
	}

	command := strings.TrimSpace(opts.Command)
	if command == "" {
		return Result{}, errors.New("shell: command is required")
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

	stdout, stderr, outputLimit := commandOutputWriters(opts.MaxOutputBytes)

	cmd, invocation, err := CommandContext(runCtx, CommandOptions{
		Program: bin,
		Args:    args,
		Command: command,
		Dir:     opts.Dir,
		Env:     opts.Env,
		Policy:  opts.Policy,
		Audit:   opts.Audit,
		Mode:    ModeCaptured,
		Stdout:  stdout,
		Stderr:  stderr,
	})
	if err != nil {
		return Result{}, err
	}

	runErr := cmd.Run()

	result := Result{
		StartedAt:       started,
		Duration:        time.Since(started),
		Stdout:          stdout.String(),
		Stderr:          stderr.String(),
		OutputTruncated: outputLimit.truncatedOutput(),
	}
	if runErr != nil {
		result.ExitError = runErr.Error()
	}

	finishErr := invocation.Finish(FinishOptions{
		Stdout:        result.Stdout,
		Stderr:        result.Stderr,
		Error:         runErr,
		OutputCapture: OutputCaptured,
	})
	if runCtx.Err() != nil {
		return result, fmt.Errorf("shell: bash command timed out after %s: %w", timeout, runCtx.Err())
	}

	if result.OutputTruncated {
		return result, fmt.Errorf("shell: bash command output exceeded %d bytes", opts.MaxOutputBytes)
	}

	if runErr != nil {
		return result, fmt.Errorf("shell: bash command failed: %w", runErr)
	}

	if finishErr != nil {
		return result, finishErr
	}

	return result, nil
}

type commandOutputLimiter struct {
	mu        sync.Mutex
	remaining int64
	limited   bool
	truncated bool
}

type limitedOutputBuffer struct {
	limiter *commandOutputLimiter
	buffer  bytes.Buffer
}

func commandOutputWriters(maxBytes int64) (stdout, stderr *limitedOutputBuffer, limiter *commandOutputLimiter) {
	limiter = &commandOutputLimiter{remaining: maxBytes, limited: maxBytes > 0}

	return &limitedOutputBuffer{limiter: limiter}, &limitedOutputBuffer{limiter: limiter}, limiter
}

func (w *limitedOutputBuffer) Write(p []byte) (int, error) {
	if w == nil || w.limiter == nil {
		return len(p), nil
	}

	w.limiter.mu.Lock()
	defer w.limiter.mu.Unlock()

	if !w.limiter.limited {
		_, _ = w.buffer.Write(p)

		return len(p), nil
	}

	if w.limiter.remaining <= 0 {
		w.limiter.truncated = true

		return len(p), nil
	}

	writeBytes := min(int64(len(p)), w.limiter.remaining)
	if writeBytes > 0 {
		_, _ = w.buffer.Write(p[:writeBytes])
		w.limiter.remaining -= writeBytes
	}

	if writeBytes < int64(len(p)) {
		w.limiter.truncated = true
	}

	return len(p), nil
}

func (w *limitedOutputBuffer) String() string {
	if w == nil {
		return ""
	}

	return w.buffer.String()
}

func (l *commandOutputLimiter) truncatedOutput() bool {
	if l == nil {
		return false
	}

	l.mu.Lock()
	defer l.mu.Unlock()

	return l.truncated
}

func bashInvocation(command string) (bin string, args []string, err error) {
	if runtime.GOOS == "windows" {
		bin, err := exec.LookPath("bash")
		if err != nil {
			return "", nil, fmt.Errorf("shell: bash executable not found: %w", err)
		}

		return bin, []string{"--noprofile", "--norc", "-lc", command}, nil
	}

	return "bash", []string{"--noprofile", "--norc", "-lc", command}, nil
}

// RunInteractive runs a command with stdin/stdout/stderr connected directly to
// the parent terminal so that interactive programs (vim, less, nested CLIs)
// work correctly. The caller's Bubble Tea program should be suspended before
// calling this function and resumed after it returns.
//
// Unlike RunBash, output is not captured -- it goes straight to the terminal.
// The returned Result only contains timing metadata.
func RunInteractive(ctx context.Context, opts Options) (Result, error) {
	if err := requireContext(ctx); err != nil {
		return Result{}, err
	}

	command := strings.TrimSpace(opts.Command)
	if command == "" {
		return Result{}, errors.New("shell: command is required")
	}

	bin, args, err := bashInvocation(command)
	if err != nil {
		return Result{}, err
	}

	started := time.Now().UTC()

	cmd, invocation, err := CommandContext(ctx, CommandOptions{
		Program: bin,
		Args:    args,
		Command: command,
		Dir:     opts.Dir,
		Env:     opts.Env,
		Policy:  opts.Policy,
		Audit:   opts.Audit,
		Mode:    ModeInteractive,
		Stdin:   os.Stdin,
		Stdout:  os.Stdout,
		Stderr:  os.Stderr,
	})
	if err != nil {
		return Result{}, err
	}

	runErr := cmd.Run()

	result := Result{
		StartedAt: started,
		Duration:  time.Since(started),
	}
	if runErr != nil {
		result.ExitError = runErr.Error()
	}

	finishErr := invocation.Finish(FinishOptions{
		Error:         runErr,
		OutputCapture: OutputNotCaptured,
		OutputNote:    "interactive terminal takeover; stdout/stderr not captured",
	})

	if ctx.Err() != nil {
		return result, fmt.Errorf("shell: interactive command canceled: %w", ctx.Err())
	}

	if runErr != nil {
		return result, fmt.Errorf("shell: interactive command failed: %w", runErr)
	}

	if finishErr != nil {
		return result, finishErr
	}

	return result, nil
}

func requireContext(ctx context.Context) error {
	if ctx == nil {
		return errors.New("shell: context is required")
	}

	if err := ctx.Err(); err != nil {
		return fmt.Errorf("shell: context already done: %w", err)
	}

	return nil
}
