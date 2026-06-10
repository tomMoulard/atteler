package main

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/tommoulard/atteler/pkg/autonomy"
	"github.com/tommoulard/atteler/pkg/session"
	"github.com/tommoulard/atteler/pkg/tasklist"
)

func taskCommandRequested(opts cliOptions) bool {
	return opts.taskAddTitle != "" || opts.taskList || opts.taskAssignSpec != "" || opts.taskCompleteID != ""
}

type taskCommandInput struct {
	FilePath   string
	AddTitle   string
	AddID      string
	Agent      string
	AssignSpec string
	CompleteID string
	List       bool
}

func taskCommandInputFromOptions(opts cliOptions) taskCommandInput {
	return taskCommandInput{
		FilePath:   opts.taskFilePath,
		AddTitle:   opts.taskAddTitle,
		AddID:      opts.taskAddID,
		Agent:      opts.taskAgent,
		AssignSpec: opts.taskAssignSpec,
		CompleteID: opts.taskCompleteID,
		List:       opts.taskList,
	}
}

func runTaskListCommand(ctx context.Context, sessionStore *session.Store, input taskCommandInput) error {
	return runTaskListCommandWithAutonomy(ctx, sessionStore, input, autonomy.DefaultLevel)
}

func runTaskListCommandWithAutonomy(ctx context.Context, sessionStore *session.Store, input taskCommandInput, level autonomy.Level) error {
	if err := validateSingleTaskOperation(input); err != nil {
		return err
	}

	if taskCommandWritesFiles(input) && !autonomy.Normalize(level).Allows(autonomy.ActionFileWrite) {
		return fmt.Errorf("%s", autonomy.DenialMessage(level, autonomy.ActionFileWrite, taskCommandAutonomyContext(input)))
	}

	return runTaskListCommandValidated(ctx, sessionStore, input)
}

func runTaskListCommandValidated(ctx context.Context, sessionStore *session.Store, input taskCommandInput) error {
	store := tasklist.NewStore(taskListPath(sessionStore, input.FilePath))
	switch {
	case input.AddTitle != "":
		task, err := store.Add(ctx, tasklist.AddRequest{
			ID:    input.AddID,
			Title: input.AddTitle,
			Agent: input.Agent,
		})
		if err != nil {
			return fmt.Errorf("task add: %w", err)
		}

		fmt.Println(formatTaskListItem(task))

		return nil
	case input.AssignSpec != "":
		id, agentName, err := parseTaskAssignmentSpec(input.AssignSpec)
		if err != nil {
			return err
		}

		task, err := store.Assign(ctx, id, agentName)
		if err != nil {
			return fmt.Errorf("task assign: %w", err)
		}

		fmt.Println(formatTaskListItem(task))

		return nil
	case input.CompleteID != "":
		task, err := store.Complete(ctx, input.CompleteID, input.Agent)
		if err != nil {
			return fmt.Errorf("task complete: %w", err)
		}

		fmt.Println(formatTaskListItem(task))

		return nil
	case input.List:
		tasks, err := store.List(ctx)
		if err != nil {
			return fmt.Errorf("task list: %w", err)
		}

		if len(tasks) == 0 {
			fmt.Println("No tasks found.")
			return nil
		}

		for i := range tasks {
			fmt.Println(formatTaskListItem(tasks[i]))
		}

		return nil
	default:
		return errors.New("task list: no task operation requested")
	}
}

func taskCommandWritesFiles(input taskCommandInput) bool {
	return input.AddTitle != "" || input.AssignSpec != "" || input.CompleteID != ""
}

func taskCommandAutonomyContext(input taskCommandInput) string {
	switch {
	case input.AddTitle != "":
		return "--task-add"
	case input.AssignSpec != "":
		return "--task-assign"
	case input.CompleteID != "":
		return "--task-complete"
	default:
		return "task command"
	}
}

func validateSingleTaskOperation(input taskCommandInput) error {
	operations := 0

	for _, requested := range []bool{
		input.AddTitle != "",
		input.List,
		input.AssignSpec != "",
		input.CompleteID != "",
	} {
		if requested {
			operations++
		}
	}

	if operations > 1 {
		return errors.New("task list: choose only one of --task-add, --task-list, --task-assign, or --task-complete")
	}

	return nil
}

func taskListPath(sessionStore *session.Store, explicit string) string {
	if path := strings.TrimSpace(explicit); path != "" {
		return path
	}

	if sessionStore == nil || strings.TrimSpace(sessionStore.Dir()) == "" {
		return filepath.Join(".atteler", "tasks.json")
	}

	return filepath.Join(filepath.Dir(sessionStore.Dir()), "tasks.json")
}

func parseTaskAssignmentSpec(raw string) (id, agentName string, err error) {
	id, agentName, ok := strings.Cut(raw, ":")
	if !ok {
		return "", "", fmt.Errorf("task assign %q: expected id:agent", raw)
	}

	id = strings.TrimSpace(id)
	agentName = strings.TrimSpace(agentName)

	if id == "" {
		return "", "", fmt.Errorf("task assign %q: id is required", raw)
	}

	if agentName == "" {
		return "", "", fmt.Errorf("task assign %q: agent is required", raw)
	}

	return id, agentName, nil
}

func formatTaskListItem(task tasklist.Task) string {
	parts := []string{
		"id=" + task.ID,
		"status=" + string(task.Status),
		"title=" + task.Title,
	}

	if task.Agent != "" {
		parts = append(parts, "agent="+task.Agent)
	}

	if !task.CreatedAt.IsZero() {
		parts = append(parts, "created_at="+task.CreatedAt.UTC().Format(time.RFC3339))
	}

	if !task.UpdatedAt.IsZero() {
		parts = append(parts, "updated_at="+task.UpdatedAt.UTC().Format(time.RFC3339))
	}

	if task.CompletedAt != nil && !task.CompletedAt.IsZero() {
		parts = append(parts, "completed_at="+task.CompletedAt.UTC().Format(time.RFC3339))
	}

	if metadata := formatTaskMetadata(task.Metadata); metadata != "" {
		parts = append(parts, "metadata="+metadata)
	}

	return strings.Join(parts, "	")
}

func formatTaskMetadata(metadata map[string]string) string {
	if len(metadata) == 0 {
		return ""
	}

	keys := make([]string, 0, len(metadata))
	for key := range metadata {
		keys = append(keys, key)
	}

	sort.Strings(keys)

	parts := make([]string, 0, len(keys))
	for _, key := range keys {
		parts = append(parts, key+":"+metadata[key])
	}

	return strings.Join(parts, ",")
}
