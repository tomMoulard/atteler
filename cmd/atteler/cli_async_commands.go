package main

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	attasync "github.com/tommoulard/atteler/pkg/async"
	"github.com/tommoulard/atteler/pkg/events"
	"github.com/tommoulard/atteler/pkg/subagent"
)

func runAsyncPlan(specs []string) error {
	plan, err := asyncPlanFromSpecs(specs)
	if err != nil {
		return fmt.Errorf("async plan: %w", err)
	}

	fmt.Print(formatAsyncPlanBatches(plan.ReadyBatches()))

	return nil
}

func runAsyncTasks(ctx context.Context, state appState, opts cliOptions) error {
	plan, err := asyncPlanFromSpecs(opts.asyncTaskSpecs)
	if err != nil {
		return fmt.Errorf("async run: %w", err)
	}

	tasks := plan.Tasks()
	if err := validateAsyncRunTasks(tasks); err != nil {
		return fmt.Errorf("async run: %w", err)
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
			"command": "async-run",
			"count":   strconv.Itoa(len(tasks)),
			"waves":   strconv.Itoa(len(plan.ReadyBatches())),
		},
	})

	runner := subagent.AttelerCommandWithOptions(subagent.CommandOptions{
		Args:   subagentCommandArgs(state),
		Binary: resolveSpawnBinary(opts.spawnBinary),
		Dir:    state.cwd,
	})
	results, runErr := plan.Run(ctx, func(ctx context.Context, task attasync.Task) (string, error) {
		return runner(ctx, subagent.Request{
			ID:     task.ID,
			Agent:  task.Agent,
			Prompt: task.Prompt,
		})
	})

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
	for _, task := range tasks {
		if strings.TrimSpace(task.Agent) == "" {
			return fmt.Errorf("task %q agent is required for --async-run", task.ID)
		}

		if strings.TrimSpace(task.Prompt) == "" {
			return fmt.Errorf("task %q prompt is required for --async-run", task.ID)
		}
	}

	return nil
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

		status := "ok"
		if result.Error != "" {
			status = statusError
		}

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

		if strings.TrimSpace(result.Output) != "" {
			fmt.Fprintf(&b, "output=%s\n", strings.TrimSpace(result.Output))
		}

		if result.Error != "" {
			fmt.Fprintf(&b, "%s=%s\n", statusError, result.Error)
		}
	}

	return b.String()
}
