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
	return opts.taskAddTitle != "" || opts.taskList || opts.taskAssignSpec != "" || opts.taskCompleteID != "" || opts.taskHeartbeatID != "" || opts.taskUpdateID != "" ||
		opts.taskReviewID != "" || opts.taskFailID != "" || opts.taskCancelID != "" || opts.taskReopenID != "" || opts.taskReconcile || opts.taskRepair
}

//nolint:govet // Field order follows the user-facing task command shape.
type taskCommandInput struct {
	FilePath          string
	AddTitle          string
	AddID             string
	Agent             string
	AssignSpec        string
	CompleteID        string
	HeartbeatID       string
	UpdateID          string
	ReviewID          string
	FailID            string
	CancelID          string
	ReopenID          string
	Title             string
	Message           string
	Reason            string
	SessionID         string
	RunID             string
	Dependencies      []string
	Risk              string
	BlockerReason     string
	ExpectedRevision  int64
	LeaseDuration     time.Duration
	Priority          nonNegativeIntFlag
	List              bool
	Reconcile         bool
	Repair            bool
	ClearBlocker      bool
	ClearDependencies bool
	ClearRisk         bool
}

type taskListAction struct {
	run       func(context.Context, *tasklist.Store, taskCommandInput) error
	name      string
	operation []permission.OperationKind
}

func taskCommandInputFromOptions(opts cliOptions) taskCommandInput {
	return taskCommandInput{
		FilePath:          opts.taskFilePath,
		AddTitle:          opts.taskAddTitle,
		AddID:             opts.taskAddID,
		Agent:             opts.taskAgent,
		AssignSpec:        opts.taskAssignSpec,
		CompleteID:        opts.taskCompleteID,
		HeartbeatID:       opts.taskHeartbeatID,
		UpdateID:          opts.taskUpdateID,
		ReviewID:          opts.taskReviewID,
		FailID:            opts.taskFailID,
		CancelID:          opts.taskCancelID,
		ReopenID:          opts.taskReopenID,
		Title:             opts.taskTitle,
		Message:           opts.taskMessage,
		Reason:            opts.taskReason,
		SessionID:         opts.taskSessionID,
		RunID:             opts.taskRunID,
		Dependencies:      append([]string(nil), opts.taskDependencies...),
		Risk:              opts.taskRisk,
		BlockerReason:     opts.taskBlockerReason,
		ExpectedRevision:  taskExpectedRevisionFromOptions(opts),
		LeaseDuration:     taskLeaseDurationFromOptions(opts),
		Priority:          opts.taskPriority,
		List:              opts.taskList,
		Reconcile:         opts.taskReconcile,
		Repair:            opts.taskRepair,
		ClearBlocker:      opts.taskClearBlocker,
		ClearDependencies: opts.taskClearDependencies,
		ClearRisk:         opts.taskClearRisk,
	}
}

func taskExpectedRevisionFromOptions(opts cliOptions) int64 {
	if !opts.taskExpectedRevision.set {
		return 0
	}

	return int64(opts.taskExpectedRevision.value)
}

func taskLeaseDurationFromOptions(opts cliOptions) time.Duration {
	if !opts.taskLeaseSeconds.set {
		return 0
	}

	return time.Duration(opts.taskLeaseSeconds.value) * time.Second
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
		return taskListAction{name: "claim task", operation: taskListWriteOperations(), run: runTaskListAssign}, true
	case input.CompleteID != "":
		return taskListAction{name: "complete task", operation: taskListWriteOperations(), run: runTaskListComplete}, true
	case input.HeartbeatID != "":
		return taskListAction{name: "heartbeat task", operation: taskListWriteOperations(), run: runTaskListHeartbeat}, true
	case input.UpdateID != "":
		return taskListAction{name: "update task", operation: taskListWriteOperations(), run: runTaskListUpdate}, true
	case input.ReviewID != "":
		return taskListAction{name: "review task", operation: taskListWriteOperations(), run: runTaskListReview}, true
	case input.FailID != "":
		return taskListAction{name: "fail task", operation: taskListWriteOperations(), run: runTaskListFail}, true
	case input.CancelID != "":
		return taskListAction{name: "cancel task", operation: taskListWriteOperations(), run: runTaskListCancel}, true
	case input.ReopenID != "":
		return taskListAction{name: "reopen task", operation: taskListWriteOperations(), run: runTaskListReopen}, true
	case input.Reconcile:
		return taskListAction{name: "reconcile tasks", operation: taskListWriteOperations(), run: runTaskListReconcile}, true
	case input.Repair:
		return taskListAction{name: "repair tasks", operation: taskListWriteOperations(), run: runTaskListRepair}, true
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
		ID:            input.AddID,
		Title:         input.AddTitle,
		Agent:         input.Agent,
		Actor:         input.Agent,
		SessionID:     input.SessionID,
		RunID:         input.RunID,
		Message:       input.Message,
		Dependencies:  input.Dependencies,
		Priority:      input.Priority.value,
		Risk:          input.Risk,
		BlockerReason: input.BlockerReason,
		LeaseDuration: input.LeaseDuration,
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

	actor := strings.TrimSpace(input.Agent)
	if actor == "" {
		actor = agentName
	}

	task, err := store.Claim(ctx, id, tasklist.AssignRequest{
		Agent:            agentName,
		Actor:            actor,
		SessionID:        input.SessionID,
		RunID:            input.RunID,
		Message:          input.Message,
		LeaseDuration:    input.LeaseDuration,
		ExpectedRevision: input.ExpectedRevision,
	})
	if err != nil {
		return fmt.Errorf("task assign: %w", err)
	}

	fmt.Println(formatTaskListItem(task))

	return nil
}

func runTaskListComplete(ctx context.Context, store *tasklist.Store, input taskCommandInput) error {
	task, err := store.CompleteWithOptions(ctx, input.CompleteID, tasklist.CompleteRequest{
		Agent:            input.Agent,
		Actor:            input.Agent,
		SessionID:        input.SessionID,
		RunID:            input.RunID,
		Message:          input.Message,
		ExpectedRevision: input.ExpectedRevision,
	})
	if err != nil {
		return fmt.Errorf("task complete: %w", err)
	}

	fmt.Println(formatTaskListItem(task))

	return nil
}

func runTaskListHeartbeat(ctx context.Context, store *tasklist.Store, input taskCommandInput) error {
	task, err := store.Heartbeat(ctx, input.HeartbeatID, tasklist.HeartbeatRequest{
		Agent:            input.Agent,
		Actor:            input.Agent,
		SessionID:        input.SessionID,
		RunID:            input.RunID,
		Message:          input.Message,
		LeaseDuration:    input.LeaseDuration,
		ExpectedRevision: input.ExpectedRevision,
	})
	if err != nil {
		return fmt.Errorf("task heartbeat: %w", err)
	}

	fmt.Println(formatTaskListItem(task))

	return nil
}

func runTaskListUpdate(ctx context.Context, store *tasklist.Store, input taskCommandInput) error {
	task, err := store.Update(ctx, input.UpdateID, tasklist.UpdateRequest{
		Title:               input.Title,
		Actor:               input.Agent,
		Dependencies:        input.Dependencies,
		Message:             input.Message,
		Risk:                input.Risk,
		BlockerReason:       input.BlockerReason,
		Priority:            input.Priority.value,
		SetPriority:         input.Priority.set,
		ClearRisk:           input.ClearRisk,
		ClearBlockerReason:  input.ClearBlocker,
		ReplaceDependencies: len(input.Dependencies) > 0 || input.ClearDependencies,
		ExpectedRevision:    input.ExpectedRevision,
	})
	if err != nil {
		return fmt.Errorf("task update: %w", err)
	}

	fmt.Println(formatTaskListItem(task))

	return nil
}

func runTaskListReview(ctx context.Context, store *tasklist.Store, input taskCommandInput) error {
	task, err := store.RequestReview(ctx, input.ReviewID, tasklist.ReviewRequest{
		Agent:            input.Agent,
		Actor:            input.Agent,
		SessionID:        input.SessionID,
		RunID:            input.RunID,
		Message:          input.Message,
		ExpectedRevision: input.ExpectedRevision,
	})
	if err != nil {
		return fmt.Errorf("task review: %w", err)
	}

	fmt.Println(formatTaskListItem(task))

	return nil
}

func runTaskListFail(ctx context.Context, store *tasklist.Store, input taskCommandInput) error {
	task, err := store.Fail(ctx, input.FailID, tasklist.FailRequest{
		Agent:            input.Agent,
		Actor:            input.Agent,
		SessionID:        input.SessionID,
		RunID:            input.RunID,
		Reason:           input.Reason,
		Message:          input.Message,
		ExpectedRevision: input.ExpectedRevision,
	})
	if err != nil {
		return fmt.Errorf("task fail: %w", err)
	}

	fmt.Println(formatTaskListItem(task))

	return nil
}

func runTaskListCancel(ctx context.Context, store *tasklist.Store, input taskCommandInput) error {
	task, err := store.Cancel(ctx, input.CancelID, tasklist.CancelRequest{
		Agent:            input.Agent,
		Actor:            input.Agent,
		SessionID:        input.SessionID,
		RunID:            input.RunID,
		Reason:           input.Reason,
		Message:          input.Message,
		ExpectedRevision: input.ExpectedRevision,
	})
	if err != nil {
		return fmt.Errorf("task cancel: %w", err)
	}

	fmt.Println(formatTaskListItem(task))

	return nil
}

func runTaskListReopen(ctx context.Context, store *tasklist.Store, input taskCommandInput) error {
	task, err := store.Reopen(ctx, input.ReopenID, tasklist.ReopenRequest{
		Agent:              input.Agent,
		Actor:              input.Agent,
		Message:            input.Message,
		ClearBlockerReason: input.ClearBlocker,
		ExpectedRevision:   input.ExpectedRevision,
	})
	if err != nil {
		return fmt.Errorf("task reopen: %w", err)
	}

	fmt.Println(formatTaskListItem(task))

	return nil
}

func runTaskListReconcile(ctx context.Context, store *tasklist.Store, input taskCommandInput) error {
	result, err := store.Reconcile(ctx, tasklist.ReconcileRequest{
		Actor:   input.Agent,
		Message: input.Message,
	})
	if err != nil {
		return fmt.Errorf("task reconcile: %w", err)
	}

	fmt.Println(formatTaskReconcileResult(result))

	return nil
}

func runTaskListRepair(ctx context.Context, store *tasklist.Store, input taskCommandInput) error {
	result, err := store.Repair(ctx, tasklist.RepairRequest{
		Actor:   input.Agent,
		Message: input.Message,
	})
	if err != nil {
		return fmt.Errorf("task repair: %w", err)
	}

	fmt.Println(formatTaskRepairResult(result))

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
	return input.AddTitle != "" || input.AssignSpec != "" || input.CompleteID != "" || input.HeartbeatID != "" || input.UpdateID != "" || input.ReviewID != "" ||
		input.FailID != "" || input.CancelID != "" || input.ReopenID != "" || input.Reconcile || input.Repair
}

func taskCommandAutonomyContext(input taskCommandInput) string {
	switch {
	case input.AddTitle != "":
		return "--task-add"
	case input.AssignSpec != "":
		return "--task-assign"
	case input.CompleteID != "":
		return "--task-complete"
	case input.HeartbeatID != "":
		return "--task-heartbeat"
	case input.UpdateID != "":
		return "--task-update"
	case input.ReviewID != "":
		return "--task-review"
	case input.FailID != "":
		return "--task-fail"
	case input.CancelID != "":
		return "--task-cancel"
	case input.ReopenID != "":
		return "--task-reopen"
	case input.Reconcile:
		return "--task-reconcile"
	case input.Repair:
		return "--task-repair"
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
		input.HeartbeatID != "",
		input.UpdateID != "",
		input.ReviewID != "",
		input.FailID != "",
		input.CancelID != "",
		input.ReopenID != "",
		input.Reconcile,
		input.Repair,
	} {
		if requested {
			operations++
		}
	}

	if operations > 1 {
		return errors.New("task list: choose only one of --task-add, --task-list, --task-assign, --task-complete, --task-heartbeat, --task-update, --task-review, --task-fail, --task-cancel, --task-reopen, --task-reconcile, or --task-repair")
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

	if task.Revision > 0 {
		parts = append(parts, fmt.Sprintf("revision=%d", task.Revision))
	}

	if task.Agent != "" {
		parts = append(parts, "agent="+task.Agent)
	}

	parts = appendTaskCoordinationFields(parts, task)
	parts = appendTaskTimestamps(parts, task)

	if metadata := formatTaskMetadata(task.Metadata); metadata != "" {
		parts = append(parts, "metadata="+metadata)
	}

	return strings.Join(parts, "	")
}

func appendTaskCoordinationFields(parts []string, task tasklist.Task) []string {
	if task.Priority != 0 {
		parts = append(parts, fmt.Sprintf("priority=%d", task.Priority))
	}

	if task.Risk != "" {
		parts = append(parts, "risk="+task.Risk)
	}

	if task.BlockerReason != "" {
		parts = append(parts, "blocker_reason="+task.BlockerReason)
	}

	if task.FailureReason != "" {
		parts = append(parts, "failure_reason="+task.FailureReason)
	}

	if task.ReviewStatus != "" {
		parts = append(parts, "review_status="+string(task.ReviewStatus))
	}

	if len(task.Dependencies) > 0 {
		parts = append(parts, "dependencies="+strings.Join(task.Dependencies, ","))
	}

	if task.AttemptCount > 0 {
		parts = append(parts, fmt.Sprintf("attempt_count=%d", task.AttemptCount))
	}

	if task.RetryCount > 0 {
		parts = append(parts, fmt.Sprintf("retry_count=%d", task.RetryCount))
	}

	if task.Lease != nil {
		parts = append(parts,
			"lease_owner="+task.Lease.Owner,
			"lease_expires_at="+task.Lease.ExpiresAt.UTC().Format(time.RFC3339),
		)
	}

	return parts
}

func appendTaskTimestamps(parts []string, task tasklist.Task) []string {
	if !task.CreatedAt.IsZero() {
		parts = append(parts, "created_at="+task.CreatedAt.UTC().Format(time.RFC3339))
	}

	if !task.UpdatedAt.IsZero() {
		parts = append(parts, "updated_at="+task.UpdatedAt.UTC().Format(time.RFC3339))
	}

	if task.CompletedAt != nil && !task.CompletedAt.IsZero() {
		parts = append(parts, "completed_at="+task.CompletedAt.UTC().Format(time.RFC3339))
	}

	return parts
}

func formatTaskReconcileResult(result tasklist.ReconcileResult) string {
	parts := []string{
		"reconciled=true",
		fmt.Sprintf("expired_leases=%d", result.ExpiredLeases),
		fmt.Sprintf("blocked=%d", result.Blocked),
		fmt.Sprintf("unblocked=%d", result.Unblocked),
		fmt.Sprintf("history_entries=%d", result.HistoryEntries),
	}

	if result.StateRevision > 0 {
		parts = append(parts, fmt.Sprintf("state_revision=%d", result.StateRevision))
	}

	return strings.Join(parts, "	")
}

func formatTaskRepairResult(result tasklist.RepairResult) string {
	parts := []string{
		fmt.Sprintf("repaired=%t", result.Repaired),
		fmt.Sprintf("tasks_recovered=%d", result.TasksRecovered),
		fmt.Sprintf("tasks_dropped=%d", result.TasksDropped),
		fmt.Sprintf("history_entries=%d", result.HistoryEntries),
	}

	if result.StateRevision > 0 {
		parts = append(parts, fmt.Sprintf("state_revision=%d", result.StateRevision))
	}

	if result.BackupPath != "" {
		parts = append(parts, "backup_path="+result.BackupPath)
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
