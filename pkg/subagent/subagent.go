// Package subagent provides small concurrent spawning primitives for child agent work.
package subagent

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/tommoulard/atteler/pkg/shell"
)

// Request describes one child agent invocation.
type Request struct {
	ID     string
	Agent  string
	Prompt string
}

// Runner executes one child agent request and returns its text output.
type Runner func(context.Context, Request) (string, error)

// Result captures the outcome and timing for one child agent request.
//
//nolint:govet // Field order keeps lifecycle metadata before request payload for readability.
type Result struct {
	StartedAt time.Time
	Duration  time.Duration
	Request   Request
	Output    string
	Error     string
}

// CommandOptions controls AttelerCommand invocations.
type CommandOptions struct {
	Env    map[string]string
	Binary string
	Dir    string
	Args   []string
}

// SpawnAll runs all requests concurrently and returns results in the same order
// as the input requests. Every started request is allowed to finish; request
// failures are recorded in their Result and returned as a joined error.
func SpawnAll(ctx context.Context, requests []Request, run Runner) ([]Result, error) {
	if err := requireContext(ctx); err != nil {
		return nil, err
	}

	if run == nil {
		return nil, errors.New("subagent: runner is required")
	}

	if err := validateRequests(requests); err != nil {
		return nil, err
	}

	results := make([]Result, len(requests))
	errs := make([]error, len(requests))

	var wg sync.WaitGroup
	wg.Add(len(requests))

	for i, request := range requests {
		go func() {
			defer wg.Done()

			started := time.Now().UTC()
			output, err := run(ctx, request)

			results[i] = Result{
				StartedAt: started,
				Duration:  time.Since(started),
				Request:   request,
				Output:    output,
			}
			if err != nil {
				results[i].Error = err.Error()
				errs[i] = fmt.Errorf("subagent: request %q failed: %w", request.ID, err)
			}
		}()
	}

	wg.Wait()

	return results, errors.Join(errs...)
}

// AttelerCommand returns a Runner that invokes an atteler binary once per
// request with --agent and --once arguments.
func AttelerCommand(binary string) Runner {
	return AttelerCommandWithOptions(CommandOptions{Binary: binary})
}

// AttelerCommandWithOptions returns a Runner that invokes an atteler binary once
// per request with --agent and --once arguments.
func AttelerCommandWithOptions(opts CommandOptions) Runner {
	return func(ctx context.Context, request Request) (string, error) {
		if err := requireContext(ctx); err != nil {
			return "", err
		}

		binary := strings.TrimSpace(opts.Binary)
		if binary == "" {
			return "", errors.New("subagent: atteler binary is required")
		}

		args := append([]string(nil), opts.Args...)
		args = append(args, "--agent", request.Agent, "--once", request.Prompt)

		var stdout, stderr bytes.Buffer

		cmd, invocation, err := shell.CommandContext(ctx, shell.CommandOptions{
			Program: binary,
			Args:    args,
			Dir:     opts.Dir,
			Env:     opts.Env,
			Mode:    shell.ModeCaptured,
			Stdout:  &stdout,
			Stderr:  &stderr,
			Audit: shell.AuditContext{
				Caller: "atteler.subagent",
			},
		})
		if err != nil {
			return "", fmt.Errorf("subagent: authorize atteler command: %w", err)
		}

		if err := cmd.Run(); err != nil {
			if finishErr := invocation.Finish(shell.FinishOptions{
				Stdout:        stdout.String(),
				Stderr:        stderr.String(),
				Error:         err,
				OutputCapture: shell.OutputCaptured,
			}); finishErr != nil {
				return stdout.String(), fmt.Errorf("subagent: audit atteler command: %w", finishErr)
			}

			message := strings.TrimSpace(stderr.String())
			if message == "" {
				message = err.Error()
			}

			return stdout.String(), fmt.Errorf("subagent: atteler command failed: %s: %w", message, err)
		}

		if err := invocation.Finish(shell.FinishOptions{
			Stdout:        stdout.String(),
			Stderr:        stderr.String(),
			OutputCapture: shell.OutputCaptured,
		}); err != nil {
			return stdout.String(), err
		}

		return stdout.String(), nil
	}
}

func requireContext(ctx context.Context) error {
	if ctx == nil {
		return errors.New("subagent: context is required")
	}

	if err := ctx.Err(); err != nil {
		return fmt.Errorf("subagent: context already done: %w", err)
	}

	return nil
}

func validateRequests(requests []Request) error {
	seen := make(map[string]struct{}, len(requests))
	for i, request := range requests {
		id := strings.TrimSpace(request.ID)
		if id == "" {
			return fmt.Errorf("subagent: request %d ID is required", i)
		}

		if strings.TrimSpace(request.Agent) == "" {
			return fmt.Errorf("subagent: request %q agent is required", request.ID)
		}

		if strings.TrimSpace(request.Prompt) == "" {
			return fmt.Errorf("subagent: request %q prompt is required", request.ID)
		}

		if _, exists := seen[id]; exists {
			return fmt.Errorf("subagent: duplicate request ID %q", id)
		}

		seen[id] = struct{}{}
	}

	return nil
}
