// Package subagent provides small concurrent spawning primitives for child agent work.
package subagent

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"sync"
	"time"
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
}

// SpawnAll runs all requests concurrently and returns results in the same order
// as the input requests. Every started request is allowed to finish; request
// failures are recorded in their Result and returned as a joined error.
func SpawnAll(ctx context.Context, requests []Request, run Runner) ([]Result, error) {
	if ctx == nil {
		return nil, errors.New("subagent: context is required")
	}
	if run == nil {
		return nil, errors.New("subagent: runner is required")
	}
	if err := validateRequests(requests); err != nil {
		return nil, err
	}
	if err := ctx.Err(); err != nil {
		return nil, fmt.Errorf("subagent: context canceled before spawning: %w", err)
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
		if ctx == nil {
			return "", errors.New("subagent: context is required")
		}
		binary := strings.TrimSpace(opts.Binary)
		if binary == "" {
			return "", errors.New("subagent: atteler binary is required")
		}

		cmd := exec.CommandContext(ctx, binary, "--agent", request.Agent, "--once", request.Prompt)
		if strings.TrimSpace(opts.Dir) != "" {
			cmd.Dir = opts.Dir
		}
		cmd.Env = mergeEnv(opts.Env)

		var stdout, stderr bytes.Buffer
		cmd.Stdout = &stdout
		cmd.Stderr = &stderr

		if err := cmd.Run(); err != nil {
			message := strings.TrimSpace(stderr.String())
			if message == "" {
				message = err.Error()
			}
			return stdout.String(), fmt.Errorf("subagent: atteler command failed: %s: %w", message, err)
		}

		return stdout.String(), nil
	}
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

func mergeEnv(extra map[string]string) []string {
	if len(extra) == 0 {
		return nil
	}
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
