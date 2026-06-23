package main

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"sync"
	"time"

	attasync "github.com/tommoulard/atteler/pkg/async"
	"github.com/tommoulard/atteler/pkg/autonomy"
	"github.com/tommoulard/atteler/pkg/events"
	"github.com/tommoulard/atteler/pkg/subagent"
)

func runAsyncPlan(input asyncPlanCommandInput) error {
	plan, err := asyncPlanFromSpecs(input.TaskSpecs)
	if err != nil {
		return fmt.Errorf("async plan: %w", err)
	}

	fmt.Print(formatAsyncPlanBatches(plan.ReadyBatches()))

	return nil
}

func runAsyncTasks(ctx context.Context, state appState, input asyncRunCommandInput) error {
	plan, err := asyncPlanFromSpecs(input.TaskSpecs)
	if err != nil {
		return fmt.Errorf("async run: %w", err)
	}

	tasks := plan.Tasks()
	if validateErr := validateAsyncRunTasks(tasks); validateErr != nil {
		return fmt.Errorf("async run: %w", validateErr)
	}

	if !autonomy.Normalize(state.autonomy).Allows(autonomy.ActionMutatingShell) {
		return fmt.Errorf("%s", autonomy.DenialMessage(state.autonomy, autonomy.ActionMutatingShell, "--async-run"))
	}

	if input.TimeoutSeconds > 0 {
		var cancel context.CancelFunc

		ctx, cancel = context.WithTimeout(ctx, time.Duration(input.TimeoutSeconds)*time.Second)
		defer cancel()
	}

	var startEventOnce sync.Once

	commandEvent := events.Event{
		Type:        events.CommandExecute,
		SessionID:   state.sessionState.ID,
		SessionPath: state.sessionStore.Path(state.sessionState.ID),
		Agent:       state.selectedAgent,
		Model:       state.selectedModel,
		Metadata: map[string]string{
			"autonomy": state.autonomy.String(),
			"command":  "async-run",
			"count":    strconv.Itoa(len(tasks)),
			"waves":    strconv.Itoa(len(plan.ReadyBatches())),
		},
	}

	asyncOpts, err := asyncRunOptionsFromInput(state, input.Execution)
	if err != nil {
		return err
	}

	if err := authorizeChildExecutionSideEffects(ctx, state.permissionPolicy, "run async child tasks", "atteler.async", asyncOpts.LedgerPath, asyncOpts.Resume); err != nil {
		return fmt.Errorf("async run: %w", err)
	}

	runner := subagent.AttelerCommandDetailedWithOptions(subagent.CommandOptions{
		Args:           subagentCommandArgs(state),
		Autonomy:       state.autonomy.String(),
		Binary:         resolveSpawnBinary(input.SpawnBinary),
		Dir:            state.cwd,
		MaxOutputBytes: int64(input.Execution.OutputBudgetBytes),
		StartCallback: func() {
			startEventOnce.Do(func() {
				emitHookWarning(ctx, state.hookRunner, commandEvent)
			})
		},
	})
	results, runErr := plan.RunDetailedWithOptions(ctx, func(ctx context.Context, task attasync.Task) (attasync.TaskRunOutput, error) {
		if outputBytesRemaining := attasync.OutputByteLimit(ctx); outputBytesRemaining > 0 {
			ctx = subagent.WithOutputByteLimit(ctx, outputBytesRemaining)
		}

		out, err := runner(ctx, subagent.Request{
			ID:                    task.ID,
			Agent:                 task.Agent,
			Prompt:                task.Prompt,
			WorkspaceID:           task.WorkspaceID,
			AllowedWriteScope:     task.AllowedWriteScope,
			Model:                 task.Model,
			Provider:              task.Provider,
			EstimatedPromptTokens: task.EstimatedPromptTokens,
			EstimatedCostMicros:   task.EstimatedCostMicros,
		})

		return attasync.TaskRunOutput{
			Stdout:              out.Stdout,
			Stderr:              out.Stderr,
			Artifacts:           out.Artifacts,
			ExitStatus:          out.ExitStatus,
			PromptTokens:        out.PromptTokens,
			EstimatedCostMicros: out.EstimatedCostMicros,
			BudgetExhausted:     out.BudgetExhausted,
		}, err
	}, asyncOpts)

	fmt.Print(formatAsyncRunResults(results))

	if runErr != nil {
		return fmt.Errorf("async run: %w", runErr)
	}

	return nil
}

func asyncPlanFromSpecs(specs []string) (*attasync.Plan, error) {
	if len(specs) == 0 {
		return nil, errors.New("at least one --async-task is required")
	}

	tasks := make([]attasync.Task, 0, len(specs))
	for _, spec := range specs {
		task, err := parseAsyncTaskSpec(spec)
		if err != nil {
			return nil, err
		}

		tasks = append(tasks, task)
	}

	plan, err := attasync.NewPlan(tasks)
	if err != nil {
		return nil, fmt.Errorf("new async plan: %w", err)
	}

	return plan, nil
}

func validateAsyncRunTasks(tasks []attasync.Task) error {
	for i := range tasks {
		task := tasks[i]
		if strings.TrimSpace(task.Agent) == "" {
			return fmt.Errorf("task %q agent is required for --async-run", task.ID)
		}

		if strings.TrimSpace(task.Prompt) == "" {
			return fmt.Errorf("task %q prompt is required for --async-run", task.ID)
		}
	}

	return nil
}

func asyncRunOptions(state appState, opts cliOptions) (attasync.RunOptions, error) {
	return asyncRunOptionsFromInput(state, childExecutionCommandInputFromOptions(opts))
}

func asyncRunOptionsFromInput(state appState, input childExecutionCommandInput) (attasync.RunOptions, error) {
	ledgerPath, err := childExecutionLedgerPathFromInput(state, input, "async")
	if err != nil {
		return attasync.RunOptions{}, err
	}

	runOpts := attasync.RunOptions{
		LedgerPath:        ledgerPath,
		AllowedWriteScope: state.cwd,
		WorkspaceID:       state.sessionState.ID,
		Model:             state.selectedModel,
		Provider:          providerNameFromModel(state.selectedModel),
		Autonomy:          state.autonomy.String(),
		CancelOnFailure:   input.CancelOnFailure,
		Resume:            input.Resume,
	}

	if input.MaxConcurrency > 0 {
		runOpts.MaxConcurrency = input.MaxConcurrency
	}

	if input.TaskTimeoutSeconds > 0 {
		runOpts.Timeout = time.Duration(input.TaskTimeoutSeconds) * time.Second
	}

	if input.RetriesSet {
		runOpts.RetryPolicy.MaxAttempts = input.Retries + 1
	}

	if input.RetryBackoffSeconds > 0 {
		runOpts.RetryPolicy.Backoff = time.Duration(input.RetryBackoffSeconds) * time.Second
	}

	if input.TokenBudget > 0 {
		runOpts.Budget.MaxPromptTokens = input.TokenBudget
	}

	if input.CostBudgetMicros > 0 {
		runOpts.Budget.MaxCostMicros = int64(input.CostBudgetMicros)
	}

	if input.OutputBudgetBytes > 0 {
		runOpts.Budget.MaxOutputBytes = int64(input.OutputBudgetBytes)
	}

	return runOpts, nil
}

func parseAsyncTaskSpec(spec string) (attasync.Task, error) {
	parts := strings.SplitN(spec, "|", 4)
	if len(parts) < 3 {
		return attasync.Task{}, fmt.Errorf("async task %q: expected id|agent|prompt|dep1+dep2", spec)
	}

	task := attasync.Task{
		ID:     strings.TrimSpace(parts[0]),
		Agent:  strings.TrimSpace(parts[1]),
		Prompt: strings.TrimSpace(parts[2]),
	}
	if task.ID == "" {
		return attasync.Task{}, fmt.Errorf("async task %q: id is required", spec)
	}

	if len(parts) == 4 {
		task.DependsOn = parseAsyncDependencies(parts[3])
	}

	return task, nil
}

func parseAsyncDependencies(raw string) []string {
	fields := strings.FieldsFunc(raw, func(r rune) bool { return r == '+' || r == ';' })

	deps := make([]string, 0, len(fields))
	for _, field := range fields {
		dep := strings.TrimSpace(field)
		if dep != "" {
			deps = append(deps, dep)
		}
	}

	return deps
}

func formatAsyncPlanBatches(batches [][]attasync.Task) string {
	if len(batches) == 0 {
		return "waves: none\n"
	}

	var b strings.Builder
	for i, batch := range batches {
		fmt.Fprintf(&b, "wave %d:\n", i+1)

		for j := range batch {
			fmt.Fprintf(&b, "  - %s\n", formatAsyncTask(batch[j]))
		}
	}

	return b.String()
}

func formatAsyncTask(task attasync.Task) string {
	parts := []string{"id=" + task.ID}
	if task.Agent != "" {
		parts = append(parts, "agent="+task.Agent)
	}

	if len(task.DependsOn) > 0 {
		parts = append(parts, "depends="+strings.Join(task.DependsOn, "+"))
	}

	if task.Prompt != "" {
		parts = append(parts, "prompt="+task.Prompt)
	}

	return strings.Join(parts, "	")
}

func formatAsyncRunResults(results []attasync.TaskResult) string {
	if len(results) == 0 {
		return ""
	}

	var b strings.Builder

	for i := range results {
		result := results[i]

		status := childStatusForDisplay(result.Status, result.Error)

		fmt.Fprintf(
			&b,
			"wave=%d\torder=%d\tid=%s\tagent=%s\tstatus=%s\tduration=%s\n",
			result.Wave+1,
			result.Order+1,
			result.Task.ID,
			result.Task.Agent,
			status,
			result.Duration.Round(time.Millisecond),
		)

		if strings.TrimSpace(result.LedgerPath) != "" {
			fmt.Fprintf(&b, "ledger=%s\n", result.LedgerPath)
			b.WriteString(formatAttelerArtifactPrivacyHint(result.LedgerPath))
		}

		if strings.TrimSpace(result.AdmissionID) != "" {
			fmt.Fprintf(&b, "admission_id=%s\n", result.AdmissionID)
		}

		if strings.TrimSpace(result.StopID) != "" {
			fmt.Fprintf(&b, "stop_id=%s\n", result.StopID)
		}

		if strings.TrimSpace(result.TranscriptPath) != "" {
			fmt.Fprintf(&b, "transcript=%s\n", result.TranscriptPath)
			b.WriteString(formatAttelerArtifactPrivacyHint(result.TranscriptPath))
		}

		for _, artifact := range result.Artifacts {
			if strings.TrimSpace(artifact) != "" {
				fmt.Fprintf(&b, "artifact=%s\n", artifact)
				b.WriteString(formatAttelerArtifactPrivacyHint(artifact))
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
