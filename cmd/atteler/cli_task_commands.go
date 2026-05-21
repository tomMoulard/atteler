package main

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/tommoulard/atteler/pkg/session"
	"github.com/tommoulard/atteler/pkg/tasklist"
)

func taskCommandRequested(opts cliOptions) bool {
	return opts.taskAddTitle != "" || opts.taskList || opts.taskAssignSpec != "" || opts.taskCompleteID != ""
}

func runTaskListCommand(ctx context.Context, sessionStore *session.Store, opts cliOptions) error {
	if err := validateSingleTaskOperation(opts); err != nil {
		return err
	}

	store := tasklist.NewStore(taskListPath(sessionStore, opts.taskFilePath))
	switch {
	case opts.taskAddTitle != "":
		task, err := store.Add(ctx, tasklist.AddRequest{
			ID:    opts.taskAddID,
			Title: opts.taskAddTitle,
			Agent: opts.taskAgent,
		})
		if err != nil {
			return fmt.Errorf("task add: %w", err)
		}

		fmt.Println(formatTaskListItem(task))

		return nil
	case opts.taskAssignSpec != "":
		id, agentName, err := parseTaskAssignmentSpec(opts.taskAssignSpec)
		if err != nil {
			return err
		}

		task, err := store.Assign(ctx, id, agentName)
		if err != nil {
			return fmt.Errorf("task assign: %w", err)
		}

		fmt.Println(formatTaskListItem(task))

		return nil
	case opts.taskCompleteID != "":
		task, err := store.Complete(ctx, opts.taskCompleteID, opts.taskAgent)
		if err != nil {
			return fmt.Errorf("task complete: %w", err)
		}

		fmt.Println(formatTaskListItem(task))

		return nil
	case opts.taskList:
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

func validateSingleTaskOperation(opts cliOptions) error {
	operations := 0

	for _, requested := range []bool{
		opts.taskAddTitle != "",
		opts.taskList,
		opts.taskAssignSpec != "",
		opts.taskCompleteID != "",
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
