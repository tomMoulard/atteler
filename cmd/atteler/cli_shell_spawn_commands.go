package main

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/tommoulard/atteler/pkg/events"
	attshell "github.com/tommoulard/atteler/pkg/shell"
	"github.com/tommoulard/atteler/pkg/subagent"
)

type bashCommandInput struct {
	Command        string
	Dir            string
	TimeoutSeconds int
}

//nolint:govet // field order follows CLI flag grouping instead of byte packing.
type spawnAgentsCommandInput struct {
	Specs          []string
	Binary         string
	TimeoutSeconds int
	DryRun         bool
	Execution      childExecutionCommandInput
}

func bashCommandInputFromOptions(opts cliOptions) bashCommandInput {
	return bashCommandInput{
		Command:        opts.bashCommand,
		Dir:            opts.bashDir,
		TimeoutSeconds: opts.bashTimeout.value,
	}
}

func spawnAgentsCommandInputFromOptions(opts cliOptions) spawnAgentsCommandInput {
	return spawnAgentsCommandInput{
		Specs:          append([]string(nil), opts.spawnAgentSpecs...),
		Binary:         opts.spawnBinary,
		TimeoutSeconds: opts.spawnTimeout.value,
		DryRun:         opts.spawnDryRun,
		Execution:      childExecutionCommandInputFromOptions(opts),
	}
}

func runBashCommand(ctx context.Context, state appState, input bashCommandInput) error {
	// Default to 120s for the CLI --bash command (builds, tests, etc. can be
	// long-running). The shell package has its own 30s default for interactive
	// TUI commands which is intentionally shorter.
	const defaultBashCLITimeout = 120

	timeoutSeconds := input.TimeoutSeconds
	if timeoutSeconds == 0 {
		timeoutSeconds = defaultBashCLITimeout
	}

	timeout := time.Duration(timeoutSeconds) * time.Second

	dir := strings.TrimSpace(input.Dir)
	if dir == "" {
		dir = state.cwd
	}

	emitHookWarning(ctx, state.hookRunner, events.Event{
		Type:        events.CommandExecute,
		SessionID:   state.sessionState.ID,
		SessionPath: state.sessionStore.Path(state.sessionState.ID),
		Agent:       state.selectedAgent,
		Model:       state.selectedModel,
		Content:     input.Command,
		Metadata: map[string]string{
			"command": input.Command,
			"cwd":     dir,
			"input":   input.Command,
			"source":  "cli",
		},
	})

	result, err := attshell.RunBash(ctx, attshell.Options{
		Command: input.Command,
		Dir:     dir,
		Timeout: timeout,
		Audit: attshell.AuditContext{
			Caller:      "atteler.cli.bash",
			SessionID:   state.sessionState.ID,
			SessionPath: state.sessionStore.Path(state.sessionState.ID),
		},
		OutputCallback: func(chunk attshell.OutputChunk) {
			switch chunk.Stream {
			case attshell.OutputStreamStderr:
				_, _ = os.Stderr.Write(chunk.Data)
			default:
				_, _ = os.Stdout.Write(chunk.Data)
			}

			emitHookWarning(ctx, state.hookRunner, events.Event{
				Type:        events.CommandOutput,
				SessionID:   state.sessionState.ID,
				SessionPath: state.sessionStore.Path(state.sessionState.ID),
				Agent:       state.selectedAgent,
				Model:       state.selectedModel,
				Content:     string(chunk.Data),
				Metadata: map[string]string{
					"command":  input.Command,
					"cwd":      dir,
					"partial":  "true",
					"sequence": strconv.FormatInt(chunk.Sequence, 10),
					"source":   "cli",
					"stream":   string(chunk.Stream),
				},
			})
		},
	})

	output := formatShellContext(shellResultMsg{
		command: input.Command,
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
		input.Command,
		output,
		err,
		map[string]string{"source": "cli"},
	))

	if err != nil {
		return fmt.Errorf("run bash: %w", err)
	}

	return nil
}

func runSpawnAgents(ctx context.Context, state appState, input spawnAgentsCommandInput) error {
	requests, err := parseSpawnAgentSpecs(input.Specs)
	if err != nil {
		return err
	}

	if input.DryRun {
		fmt.Print(formatSpawnDryRun(requests))
		return nil
	}

	if input.TimeoutSeconds > 0 {
		var cancel context.CancelFunc

		ctx, cancel = context.WithTimeout(ctx, time.Duration(input.TimeoutSeconds)*time.Second)
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

	spawnOpts, err := subagentOptionsFromInput(state, input.Execution, "spawn")
	if err != nil {
		return err
	}

	results, runErr := subagent.SpawnAllDetailed(ctx, requests, subagent.AttelerCommandDetailedWithOptions(subagent.CommandOptions{
		Args:           subagentCommandArgs(state),
		Binary:         resolveSpawnBinary(input.Binary),
		Dir:            state.cwd,
		MaxOutputBytes: int64(input.Execution.OutputBudgetBytes),
	}), spawnOpts)
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

func subagentOptions(state appState, opts cliOptions, kind string) (subagent.Options, error) {
	return subagentOptionsFromInput(state, childExecutionCommandInputFromOptions(opts), kind)
}

func subagentOptionsFromInput(state appState, input childExecutionCommandInput, kind string) (subagent.Options, error) {
	ledgerPath, err := childExecutionLedgerPathFromInput(state, input, kind)
	if err != nil {
		return subagent.Options{}, err
	}

	spawnOpts := subagent.Options{
		LedgerPath:        ledgerPath,
		AllowedWriteScope: state.cwd,
		WorkspaceID:       state.sessionState.ID,
		Model:             state.selectedModel,
		Provider:          providerNameFromModel(state.selectedModel),
		CancelOnFailure:   input.CancelOnFailure,
		Resume:            input.Resume,
	}

	if input.MaxConcurrency > 0 {
		spawnOpts.MaxConcurrency = input.MaxConcurrency
	}

	if input.TaskTimeoutSeconds > 0 {
		spawnOpts.Timeout = time.Duration(input.TaskTimeoutSeconds) * time.Second
	}

	if input.RetriesSet {
		spawnOpts.RetryPolicy.MaxAttempts = input.Retries + 1
	}

	if input.RetryBackoffSeconds > 0 {
		spawnOpts.RetryPolicy.Backoff = time.Duration(input.RetryBackoffSeconds) * time.Second
	}

	if input.TokenBudget > 0 {
		spawnOpts.Budget.MaxPromptTokens = input.TokenBudget
	}

	if input.CostBudgetMicros > 0 {
		spawnOpts.Budget.MaxCostMicros = int64(input.CostBudgetMicros)
	}

	if input.OutputBudgetBytes > 0 {
		spawnOpts.Budget.MaxOutputBytes = int64(input.OutputBudgetBytes)
	}

	return spawnOpts, nil
}

func childExecutionLedgerPath(state appState, opts cliOptions, kind string) (string, error) {
	return childExecutionLedgerPathFromInput(state, childExecutionCommandInputFromOptions(opts), kind)
}

func childExecutionLedgerPathFromInput(state appState, input childExecutionCommandInput, kind string) (string, error) {
	if strings.TrimSpace(input.LedgerPath) != "" {
		return strings.TrimSpace(input.LedgerPath), nil
	}

	if input.Resume {
		return "", fmt.Errorf("%s resume requires --spawn-ledger", kind)
	}

	cwd := strings.TrimSpace(state.cwd)
	if cwd == "" {
		cwd = "."
	}

	runID := state.sessionState.ID
	if strings.TrimSpace(runID) == "" {
		runID = time.Now().UTC().Format("20060102-150405.000000000")
	}

	return filepath.Join(cwd, ".atteler", "runs", kind+"-"+runID+"-"+time.Now().UTC().Format("150405.000000000"), "ledger.json"), nil
}

func providerNameFromModel(model string) string {
	provider, _, ok := strings.Cut(strings.TrimSpace(model), "/")
	if !ok {
		return ""
	}

	return strings.TrimSpace(provider)
}

func childStatusForDisplay(status, errText string) string {
	if strings.TrimSpace(status) == "" {
		if errText != "" {
			return statusError
		}

		return "ok"
	}

	switch status {
	case subagent.StatusSucceeded:
		return "ok"
	case subagent.StatusFailed:
		return statusError
	default:
		return status
	}
}

func parseSpawnAgentSpecs(specs []string) ([]subagent.Request, error) {
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
	for i := range requests {
		request := requests[i]
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

	for i := range requests {
		request := requests[i]
		fmt.Fprintf(&b, "id=%s\tagent=%s\tprompt=%s\n", request.ID, request.Agent, request.Prompt)
	}

	return b.String()
}

func formatSpawnResults(results []subagent.Result) string {
	if len(results) == 0 {
		return ""
	}

	var b strings.Builder

	for i := range results {
		result := results[i]
		status := childStatusForDisplay(result.Status, result.Error)

		fmt.Fprintf(
			&b,
			"id=%s\tagent=%s\tstatus=%s\tduration=%s\n",
			result.Request.ID,
			result.Request.Agent,
			status,
			result.Duration.Round(time.Millisecond),
		)

		if strings.TrimSpace(result.LedgerPath) != "" {
			fmt.Fprintf(&b, "ledger=%s\n", result.LedgerPath)
		}

		if strings.TrimSpace(result.AdmissionID) != "" {
			fmt.Fprintf(&b, "admission_id=%s\n", result.AdmissionID)
		}

		if strings.TrimSpace(result.StopID) != "" {
			fmt.Fprintf(&b, "stop_id=%s\n", result.StopID)
		}

		if strings.TrimSpace(result.TranscriptPath) != "" {
			fmt.Fprintf(&b, "transcript=%s\n", result.TranscriptPath)
		}

		for _, artifact := range result.Artifacts {
			if strings.TrimSpace(artifact) != "" {
				fmt.Fprintf(&b, "artifact=%s\n", artifact)
			}
		}

		if strings.TrimSpace(result.Output) != "" {
			fmt.Fprintf(&b, "output=%s\n", strings.TrimSpace(result.Output))
		}

		if result.Error != "" {
			fmt.Fprintf(&b, "%s=%s\n", statusError, result.Error)
		}
	}

	return b.String()
}
