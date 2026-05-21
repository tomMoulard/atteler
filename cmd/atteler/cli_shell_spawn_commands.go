package main

import (
	"context"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/tommoulard/atteler/pkg/events"
	attshell "github.com/tommoulard/atteler/pkg/shell"
	"github.com/tommoulard/atteler/pkg/subagent"
)

func runBashCommand(ctx context.Context, state appState, opts cliOptions) error {
	// Default to 120s for the CLI --bash command (builds, tests, etc. can be
	// long-running). The shell package has its own 30s default for interactive
	// TUI commands which is intentionally shorter.
	const defaultBashCLITimeout = 120

	timeoutSeconds := opts.bashTimeout.value
	if timeoutSeconds == 0 {
		timeoutSeconds = defaultBashCLITimeout
	}

	timeout := time.Duration(timeoutSeconds) * time.Second

	dir := strings.TrimSpace(opts.bashDir)
	if dir == "" {
		dir = state.cwd
	}

	emitHookWarning(ctx, state.hookRunner, events.Event{
		Type:        events.CommandExecute,
		SessionID:   state.sessionState.ID,
		SessionPath: state.sessionStore.Path(state.sessionState.ID),
		Agent:       state.selectedAgent,
		Model:       state.selectedModel,
		Content:     opts.bashCommand,
		Metadata: map[string]string{
			"command": opts.bashCommand,
			"cwd":     dir,
			"input":   opts.bashCommand,
			"source":  "cli",
		},
	})

	result, err := attshell.RunBash(ctx, attshell.Options{
		Command: opts.bashCommand,
		Dir:     dir,
		Timeout: timeout,
	})
	if result.Stdout != "" {
		fmt.Print(result.Stdout)
	}

	if result.Stderr != "" {
		fmt.Fprint(os.Stderr, result.Stderr)
	}

	output := formatShellContext(shellResultMsg{
		command: opts.bashCommand,
		stdout:  result.Stdout,
		stderr:  result.Stderr,
		err:     err,
	})
	emitHookWarning(ctx, state.hookRunner, commandOutputEvent(
		state.sessionState.ID,
		state.sessionStore.Path(state.sessionState.ID),
		state.selectedAgent,
		state.selectedModel,
		dir,
		opts.bashCommand,
		output,
		err,
		map[string]string{"source": "cli"},
	))

	if err != nil {
		return fmt.Errorf("run bash: %w", err)
	}

	return nil
}

func runSpawnAgents(ctx context.Context, state appState, opts cliOptions) error {
	requests, err := parseSpawnAgentSpecs(opts.spawnAgentSpecs)
	if err != nil {
		return err
	}

	if opts.spawnDryRun {
		fmt.Print(formatSpawnDryRun(requests))
		return nil
	}

	if opts.spawnTimeout.value > 0 {
		var cancel context.CancelFunc

		ctx, cancel = context.WithTimeout(ctx, time.Duration(opts.spawnTimeout.value)*time.Second)
		defer cancel()
	}

	emitHookWarning(ctx, state.hookRunner, events.Event{
		Type:        events.CommandExecute,
		SessionID:   state.sessionState.ID,
		SessionPath: state.sessionStore.Path(state.sessionState.ID),
		Agent:       state.selectedAgent,
		Model:       state.selectedModel,
		Metadata: map[string]string{
			"command": "spawn-agent",
			"count":   strconv.Itoa(len(requests)),
		},
	})

	results, runErr := subagent.SpawnAll(ctx, requests, subagent.AttelerCommandWithOptions(subagent.CommandOptions{
		Binary: resolveSpawnBinary(opts.spawnBinary),
		Dir:    state.cwd,
	}))
	fmt.Print(formatSpawnResults(results))

	if runErr != nil {
		return fmt.Errorf("spawn agents: %w", runErr)
	}

	return nil
}

func resolveSpawnBinary(explicit string) string {
	if binary := strings.TrimSpace(explicit); binary != "" {
		return binary
	}

	binary, err := os.Executable()
	if err != nil || strings.TrimSpace(binary) == "" {
		return os.Args[0]
	}

	return binary
}

func subagentCommandArgs(state appState) []string {
	var args []string
	if strings.TrimSpace(state.selectedModel) != "" {
		args = append(args, "--model", state.selectedModel)
	}

	if state.sessionStore != nil && strings.TrimSpace(state.sessionStore.Dir()) != "" {
		args = append(args, "--session-dir", state.sessionStore.Dir())
	}

	return args
}

func parseSpawnAgentSpecs(specs rawStringListFlag) ([]subagent.Request, error) {
	requests := make([]subagent.Request, 0, len(specs))
	for i, raw := range specs {
		request, err := parseSpawnAgentSpec(raw, i+1)
		if err != nil {
			return nil, err
		}

		requests = append(requests, request)
	}

	if err := validateSpawnRequests(requests); err != nil {
		return nil, err
	}

	return requests, nil
}

func parseSpawnAgentSpec(raw string, index int) (subagent.Request, error) {
	parts := strings.SplitN(strings.TrimSpace(raw), "|", 3)
	switch len(parts) {
	case 2:
		return subagent.Request{
			ID:     fmt.Sprintf("child-%d", index),
			Agent:  strings.TrimSpace(parts[0]),
			Prompt: strings.TrimSpace(parts[1]),
		}, nil
	case 3:
		return subagent.Request{
			ID:     strings.TrimSpace(parts[0]),
			Agent:  strings.TrimSpace(parts[1]),
			Prompt: strings.TrimSpace(parts[2]),
		}, nil
	default:
		return subagent.Request{}, fmt.Errorf("spawn agent spec %q: expected agent|prompt or id|agent|prompt", raw)
	}
}

func validateSpawnRequests(requests []subagent.Request) error {
	seen := make(map[string]struct{}, len(requests))
	for i, request := range requests {
		if strings.TrimSpace(request.ID) == "" {
			return fmt.Errorf("spawn agent request %d: id is required", i)
		}

		if strings.TrimSpace(request.Agent) == "" {
			return fmt.Errorf("spawn agent request %q: agent is required", request.ID)
		}

		if strings.TrimSpace(request.Prompt) == "" {
			return fmt.Errorf("spawn agent request %q: prompt is required", request.ID)
		}

		if _, ok := seen[request.ID]; ok {
			return fmt.Errorf("spawn agent: duplicate request id %q", request.ID)
		}

		seen[request.ID] = struct{}{}
	}

	return nil
}

func formatSpawnDryRun(requests []subagent.Request) string {
	var b strings.Builder
	fmt.Fprintf(&b, "Would spawn %d sub-agent(s).\n", len(requests))

	for _, request := range requests {
		fmt.Fprintf(&b, "id=%s\tagent=%s\tprompt=%s\n", request.ID, request.Agent, request.Prompt)
	}

	return b.String()
}

func formatSpawnResults(results []subagent.Result) string {
	if len(results) == 0 {
		return ""
	}

	var b strings.Builder

	for _, result := range results {
		status := "ok"
		if result.Error != "" {
			status = statusError
		}

		fmt.Fprintf(
			&b,
			"id=%s\tagent=%s\tstatus=%s\tduration=%s\n",
			result.Request.ID,
			result.Request.Agent,
			status,
			result.Duration.Round(time.Millisecond),
		)

		if strings.TrimSpace(result.Output) != "" {
			fmt.Fprintf(&b, "output=%s\n", strings.TrimSpace(result.Output))
		}

		if result.Error != "" {
			fmt.Fprintf(&b, "%s=%s\n", statusError, result.Error)
		}
	}

	return b.String()
}
