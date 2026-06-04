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
	"github.com/tommoulard/atteler/pkg/permission"
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

type taskListAction struct {
	run       func(context.Context, *tasklist.Store, taskCommandInput) error
	name      string
	operation []permission.OperationKind
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

	action, ok := taskListActionForInput(input)
	if !ok {
		return errors.New("task list: no task operation requested")
	}

	if taskCommandWritesFiles(input) && !autonomy.Normalize(level).Allows(autonomy.ActionFileWrite) {
		return fmt.Errorf("%s", autonomy.DenialMessage(level, autonomy.ActionFileWrite, taskCommandAutonomyContext(input)))
	}

	path := taskListPath(sessionStore, input.FilePath)
	store := tasklist.NewStore(path)

	if err := authorizeTaskListPermission(ctx, action.name, path, action.operation...); err != nil {
		return fmt.Errorf("task list: %w", err)
	}

	return action.run(ctx, store, input)
}

func taskListActionForInput(input taskCommandInput) (taskListAction, bool) {
	switch {
	case input.AddTitle != "":
		return taskListAction{name: "add task", operation: taskListWriteOperations(), run: runTaskListAdd}, true
	case input.AssignSpec != "":
		return taskListAction{name: "assign task", operation: taskListWriteOperations(), run: runTaskListAssign}, true
	case input.CompleteID != "":
		return taskListAction{name: "complete task", operation: taskListWriteOperations(), run: runTaskListComplete}, true
	case input.List:
		return taskListAction{name: "list tasks", operation: []permission.OperationKind{permission.OperationRead}, run: runTaskListList}, true
	default:
		return taskListAction{}, false
	}
}

func taskListWriteOperations() []permission.OperationKind {
	return []permission.OperationKind{permission.OperationRead, permission.OperationWrite}
}

func runTaskListAdd(ctx context.Context, store *tasklist.Store, input taskCommandInput) error {
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
}

func runTaskListAssign(ctx context.Context, store *tasklist.Store, input taskCommandInput) error {
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
}

func runTaskListComplete(ctx context.Context, store *tasklist.Store, input taskCommandInput) error {
	task, err := store.Complete(ctx, input.CompleteID, input.Agent)
	if err != nil {
		return fmt.Errorf("task complete: %w", err)
	}

	fmt.Println(formatTaskListItem(task))

	return nil
}

func runTaskListList(ctx context.Context, store *tasklist.Store, _ taskCommandInput) error {
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
}

func authorizeTaskListPermission(ctx context.Context, action, target string, kinds ...permission.OperationKind) error {
	operations := make([]permission.Operation, 0, len(kinds))
	for _, kind := range kinds {
		operations = append(operations, permission.Operation{
			Kind:   kind,
			Action: action,
			Source: "atteler.task_list",
			Target: target,
		})
	}

	decision := permission.Evaluate(ctx, nil, permission.Request{
		Action:     action,
		Source:     "atteler.task_list",
		Target:     target,
		Operations: operations,
	})
	if decision.Allowed {
		return nil
	}

	return &permission.Error{Decision: decision}
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
