// Package tasklist provides a JSON-backed coordination task list for agents.
package tasklist

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Status is the lifecycle state of a task.
type Status string

const (
	// StatusReady means the task can be claimed by a worker.
	StatusReady Status = "ready"
	// StatusPending is the legacy ready state accepted from older task files.
	StatusPending Status = "pending"
	// StatusBlocked means dependencies or an explicit blocker prevent work.
	StatusBlocked Status = "blocked"
	// StatusInProgress means the task has an owner and an active or expired lease.
	StatusInProgress Status = "in_progress"
	// StatusAssigned is the legacy in-progress state accepted from older task files.
	StatusAssigned Status = "assigned"
	// StatusReview means implementation is complete enough for review.
	StatusReview Status = "review"
	// StatusFailed means work stopped unsuccessfully and may need a retry.
	StatusFailed Status = "failed"
	// StatusCanceled means work was intentionally stopped.
	StatusCanceled Status = "canceled"
	// StatusReopened means a completed, failed, canceled, or review task was reopened.
	StatusReopened Status = "reopened"
	// StatusCompleted means the task has been checked off.
	StatusCompleted Status = "completed"
)

// ReviewStatus captures lightweight human or agent review state for a task.
type ReviewStatus string

const (
	// ReviewStatusNone means no review state has been requested.
	ReviewStatusNone ReviewStatus = ""
	// ReviewStatusPending means the task is waiting for review.
	ReviewStatusPending ReviewStatus = "pending"
	// ReviewStatusApproved means the task passed review.
	ReviewStatusApproved ReviewStatus = "approved"
	// ReviewStatusChangesRequested means review found follow-up work.
	ReviewStatusChangesRequested ReviewStatus = "changes_requested"
)

// HistoryAction describes the operation recorded in a history entry.
type HistoryAction string

const (
	// HistoryAdded means a task was created.
	HistoryAdded HistoryAction = "added"
	// HistoryAssigned means a task was assigned to an agent.
	HistoryAssigned HistoryAction = "assigned"
	// HistoryUpdated means mutable task fields were changed.
	HistoryUpdated HistoryAction = "updated"
	// HistoryHeartbeat means an owner renewed an assigned task lease.
	HistoryHeartbeat HistoryAction = "heartbeat"
	// HistoryReconciled means derived coordination state was repaired.
	HistoryReconciled HistoryAction = "reconciled"
	// HistoryCompleted means a task was checked off.
	HistoryCompleted HistoryAction = "completed"
	// HistoryReviewRequested means a task moved to review.
	HistoryReviewRequested HistoryAction = "review_requested"
	// HistoryFailed means a task was marked failed.
	HistoryFailed HistoryAction = "failed"
	// HistoryCanceled means a task was canceled.
	HistoryCanceled HistoryAction = "canceled"
	// HistoryReopened means a task was reopened for another pass.
	HistoryReopened HistoryAction = "reopened"
	// HistoryRepaired means a corrupt or conflicted task file was repaired.
	HistoryRepaired HistoryAction = "repaired"
)

const (
	// DefaultLeaseDuration is used when callers assign or claim a task without an explicit lease duration.
	DefaultLeaseDuration = 30 * time.Minute
	fileLockRetryDelay   = 10 * time.Millisecond
)

var (
	// ErrTaskNotFound is returned when a task ID does not exist in the store.
	ErrTaskNotFound = errors.New("tasklist: task not found")
	// ErrTaskCompleted is returned when an operation cannot modify a completed task.
	ErrTaskCompleted = errors.New("tasklist: task is completed")
	// ErrTaskCanceled is returned when an operation cannot modify a canceled task.
	ErrTaskCanceled = errors.New("tasklist: task is canceled")
	// ErrTaskFailed is returned when a failed task must be reopened before more work.
	ErrTaskFailed = errors.New("tasklist: task is failed")
	// ErrTaskBlocked is returned when dependencies prevent a task from being assigned or completed.
	ErrTaskBlocked = errors.New("tasklist: task is blocked")
	// ErrTaskLeased is returned when another owner still has a non-expired task lease.
	ErrTaskLeased = errors.New("tasklist: task lease is held by another owner")
	// ErrTaskLeaseExpired is returned when a heartbeat is attempted after the task lease expired.
	ErrTaskLeaseExpired = errors.New("tasklist: task lease expired")
	// ErrRevisionConflict is returned when a caller's expected task revision is stale.
	ErrRevisionConflict = errors.New("tasklist: revision conflict")
	// ErrDependencyCycle is returned when task dependencies would create a cycle.
	ErrDependencyCycle = errors.New("tasklist: dependency cycle")
)

// Lease records temporary ownership of an assigned task.
type Lease struct {
	AcquiredAt      time.Time `json:"acquired_at"`
	LastHeartbeatAt time.Time `json:"last_heartbeat_at"`
	ExpiresAt       time.Time `json:"expires_at"`
	Owner           string    `json:"owner"`
	SessionID       string    `json:"session_id,omitempty"`
	RunID           string    `json:"run_id,omitempty"`
}

// Task is a durable TODO item.
//
//nolint:govet // Field order keeps related coordination data readable in JSON.
type Task struct {
	CreatedAt     time.Time         `json:"created_at"`
	UpdatedAt     time.Time         `json:"updated_at"`
	CompletedAt   *time.Time        `json:"completed_at,omitempty"`
	Lease         *Lease            `json:"lease,omitempty"`
	Metadata      map[string]string `json:"metadata,omitempty"`
	Dependencies  []string          `json:"dependencies,omitempty"`
	Revision      int64             `json:"revision,omitempty"`
	Priority      int               `json:"priority,omitempty"`
	AttemptCount  int               `json:"attempt_count,omitempty"`
	RetryCount    int               `json:"retry_count,omitempty"`
	ID            string            `json:"id"`
	Title         string            `json:"title"`
	Status        Status            `json:"status"`
	Agent         string            `json:"agent,omitempty"`
	Risk          string            `json:"risk,omitempty"`
	BlockerReason string            `json:"blocker_reason,omitempty"`
	FailureReason string            `json:"failure_reason,omitempty"`
	ReviewStatus  ReviewStatus      `json:"review_status,omitempty"`
}

// HistoryEntry records a task mutation.
//
//nolint:govet // Field order keeps related history data readable in JSON.
type HistoryEntry struct {
	At            time.Time     `json:"at"`
	Before        *Task         `json:"before,omitempty"`
	After         *Task         `json:"after,omitempty"`
	Seq           int64         `json:"seq"`
	StateRevision int64         `json:"state_revision,omitempty"`
	Action        HistoryAction `json:"action"`
	TaskID        string        `json:"task_id"`
	Actor         string        `json:"actor"`
	Agent         string        `json:"agent,omitempty"`
	Message       string        `json:"message,omitempty"`
}

// AddRequest contains fields for a new task.
//
//nolint:govet // Field order follows the user-facing add request shape.
type AddRequest struct {
	ID            string
	Title         string
	Agent         string
	Actor         string
	SessionID     string
	RunID         string
	Message       string
	Metadata      map[string]string
	Dependencies  []string
	Priority      int
	Risk          string
	BlockerReason string
	ReviewStatus  ReviewStatus
	LeaseDuration time.Duration
}

// AssignRequest contains fields for claiming or administratively assigning a task.
//
// Claim rejects live leases held by a different owner. Assign uses the same
// shape but is intentionally administrative and can replace an owner.
type AssignRequest struct {
	Agent            string
	Actor            string
	SessionID        string
	RunID            string
	Message          string
	LeaseDuration    time.Duration
	ExpectedRevision int64
}

// HeartbeatRequest contains fields for renewing a task lease.
type HeartbeatRequest struct {
	Agent            string
	Actor            string
	SessionID        string
	RunID            string
	Message          string
	LeaseDuration    time.Duration
	ExpectedRevision int64
}

// CompleteRequest contains fields for completing a task with conflict checks and audit context.
type CompleteRequest struct {
	Agent            string
	Actor            string
	SessionID        string
	RunID            string
	Message          string
	ExpectedRevision int64
}

// ReviewRequest contains fields for moving a task into review.
type ReviewRequest struct {
	Agent            string
	Actor            string
	SessionID        string
	RunID            string
	Message          string
	ExpectedRevision int64
}

// FailRequest contains fields for marking a task failed.
type FailRequest struct {
	Agent            string
	Actor            string
	SessionID        string
	RunID            string
	Reason           string
	Message          string
	ExpectedRevision int64
}

// CancelRequest contains fields for canceling a task.
type CancelRequest struct {
	Agent            string
	Actor            string
	SessionID        string
	RunID            string
	Reason           string
	Message          string
	ExpectedRevision int64
}

// ReopenRequest contains fields for reopening terminal or review work.
type ReopenRequest struct {
	Agent              string
	Actor              string
	Message            string
	ClearFailureReason bool
	ClearBlockerReason bool
	ExpectedRevision   int64
}

// UpdateRequest contains mutable task fields. Empty values leave fields unchanged.
//
// Use ReplaceDependencies, SetPriority, ClearRisk, ClearBlockerReason,
// ClearFailureReason, or ExpectedRevision when an empty value carries meaning.
//
//nolint:govet // Field order keeps older fields first for compatibility.
type UpdateRequest struct {
	Title               string
	Agent               string
	Actor               string
	Status              Status
	ReviewStatus        ReviewStatus
	FailureReason       string
	Metadata            map[string]string
	Dependencies        []string
	Message             string
	Risk                string
	BlockerReason       string
	Priority            int
	SetPriority         bool
	ClearRisk           bool
	ClearBlockerReason  bool
	ReplaceDependencies bool
	ClearFailureReason  bool
	ExpectedRevision    int64
}

// RepairRequest identifies the actor repairing an unreadable or conflicted task file.
type RepairRequest struct {
	Actor   string
	Message string
}

// RepairResult summarizes recoverable file repair work.
type RepairResult struct {
	BackupPath     string
	StateRevision  int64
	TasksRecovered int
	TasksDropped   int
	HistoryEntries int
	Repaired       bool
}

// ReconcileRequest identifies the actor repairing derived task state.
type ReconcileRequest struct {
	Actor   string
	Message string
}

// ReconcileResult summarizes stale lease and dependency repairs.
type ReconcileResult struct {
	Tasks          []Task
	ExpiredLeases  int
	Blocked        int
	Unblocked      int
	StateRevision  int64
	HistoryEntries int
}

// State is the JSON document persisted by Store.
//
//nolint:govet // Field order keeps the revision header first in persisted JSON.
type State struct {
	Revision int64          `json:"revision,omitempty"`
	Tasks    []Task         `json:"tasks"`
	History  []HistoryEntry `json:"history"`
}

// Store persists a task list to one JSON file.
//
//nolint:govet // Field order keeps path and synchronization fields easy to scan.
type Store struct {
	path string
	mu   sync.Mutex
	now  func() time.Time
}

// NewStore creates a task list store bound to path.
func NewStore(path string) *Store {
	return &Store{path: path, now: func() time.Time { return time.Now().UTC() }}
}

// Path returns the JSON file path used by the store.
func (s *Store) Path() string {
	return s.path
}

// Add creates and persists a new task.
func (s *Store) Add(ctx context.Context, req AddRequest) (Task, error) {
	if err := ctxErr(ctx); err != nil {
		return Task{}, err
	}

	title := strings.TrimSpace(req.Title)
	if title == "" {
		return Task{}, errors.New("tasklist: title is required")
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	var created Task

	err := s.withFileLock(ctx, func() error {
		state, err := s.loadFile(ctx)
		if err != nil {
			return err
		}

		now := s.now().UTC()

		created, err = newTaskFromAddRequest(state, req, title, now)
		if err != nil {
			return err
		}

		state.Revision = nextStateRevision(state)
		created.Revision = state.Revision
		state.Tasks = append(state.Tasks, created)
		state.History = appendHistory(state.History, historyRecord{
			at:            now,
			action:        HistoryAdded,
			taskID:        created.ID,
			agent:         created.Agent,
			actor:         actorIdentity(req.Actor, created.Agent),
			message:       req.Message,
			stateRevision: state.Revision,
			after:         &created,
		})

		if err := s.saveFile(ctx, state); err != nil {
			return err
		}

		return nil
	})
	if err != nil {
		return Task{}, err
	}

	return cloneTask(created), nil
}

func newTaskFromAddRequest(state State, req AddRequest, title string, now time.Time) (Task, error) {
	id := strings.TrimSpace(req.ID)
	if id == "" {
		id = newID()
	}

	if findTask(state.Tasks, id) >= 0 {
		return Task{}, fmt.Errorf("tasklist: duplicate task id %q", id)
	}

	dependencies, err := sanitizeDependencies(req.Dependencies, id)
	if err != nil {
		return Task{}, err
	}

	if err := validateReviewStatus(req.ReviewStatus); err != nil {
		return Task{}, err
	}

	var (
		agent         = strings.TrimSpace(req.Agent)
		lease         *Lease
		status        = StatusReady
		attemptCount  int
		blockerReason = strings.TrimSpace(req.BlockerReason)
	)

	unmet := unmetDependencies(state, dependencies)
	if blockerReason == "" {
		blockerReason = dependencyBlockerReason(unmet)
	}

	if len(unmet) > 0 || blockerReason != "" {
		if agent != "" {
			return Task{}, fmt.Errorf("%w: dependencies are not completed", ErrTaskBlocked)
		}

		status = StatusBlocked
	} else if agent != "" {
		status = StatusInProgress
		attemptCount = 1
		lease = newLease(agent, req.SessionID, req.RunID, now, req.LeaseDuration)
	}

	task := Task{
		ID:            id,
		Title:         title,
		Status:        status,
		Agent:         agent,
		Lease:         lease,
		CreatedAt:     now,
		UpdatedAt:     now,
		Metadata:      copyMetadata(req.Metadata),
		Dependencies:  dependencies,
		Priority:      req.Priority,
		Risk:          strings.TrimSpace(req.Risk),
		BlockerReason: blockerReason,
		AttemptCount:  attemptCount,
		ReviewStatus:  req.ReviewStatus,
	}

	if dependencyCycleForTask(stateWithTask(state, task), task.ID) {
		return Task{}, fmt.Errorf("%w: task %q", ErrDependencyCycle, task.ID)
	}

	return task, nil
}

// List returns tasks sorted by creation time and then ID for deterministic output.
func (s *Store) List(ctx context.Context) ([]Task, error) {
	state, err := s.Load(ctx)
	if err != nil {
		return nil, err
	}

	return cloneTasks(state.Tasks), nil
}

// Load reads the whole task list state. Missing files return an empty state.
func (s *Store) Load(ctx context.Context) (State, error) {
	if err := ctxErr(ctx); err != nil {
		return State{}, err
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	var state State

	err := s.withFileLock(ctx, func() error {
		loaded, err := s.loadFile(ctx)
		if err != nil {
			return err
		}

		state = loaded

		return nil
	})
	if err != nil {
		return State{}, err
	}

	return cloneState(state), nil
}

// History returns history entries sorted by sequence.
func (s *Store) History(ctx context.Context) ([]HistoryEntry, error) {
	state, err := s.Load(ctx)
	if err != nil {
		return nil, err
	}

	return cloneHistory(state.History), nil
}

// Assign records an agent owner for an incomplete task.
//
// Assign is administrative: it always writes the requested owner under the
// cross-process file lock. Agents that need lease-preserving coordination should
// use Claim, which rejects tasks held by another live lease.
func (s *Store) Assign(ctx context.Context, id, agent string) (Task, error) {
	return s.AssignWithLease(ctx, id, AssignRequest{Agent: agent})
}

// AssignWithLease records an agent owner and lease for an incomplete task.
func (s *Store) AssignWithLease(ctx context.Context, id string, req AssignRequest) (Task, error) {
	return s.assign(ctx, id, req, true)
}

// Claim assigns a task only when no other owner holds a non-expired lease.
func (s *Store) Claim(ctx context.Context, id string, req AssignRequest) (Task, error) {
	return s.assign(ctx, id, req, false)
}

// Heartbeat renews the active owner lease for an assigned task.
func (s *Store) Heartbeat(ctx context.Context, id string, req HeartbeatRequest) (Task, error) {
	agent := strings.TrimSpace(req.Agent)
	if agent == "" {
		return Task{}, errors.New("tasklist: agent is required")
	}

	return s.updateTask(ctx, id, req.ExpectedRevision, req.Actor, func(_ *State, task *Task, now time.Time) (HistoryAction, string, error) {
		if task.Status == StatusCompleted {
			return "", "", ErrTaskCompleted
		}

		if task.Status == StatusCanceled {
			return "", "", ErrTaskCanceled
		}

		if !isInProgressStatus(task.Status) || task.Lease == nil {
			return "", "", ErrTaskLeaseExpired
		}

		if task.Lease.Owner != agent || !sameLeaseScope(task.Lease, req.SessionID, req.RunID) {
			return "", "", ErrTaskLeased
		}

		if !task.Lease.ExpiresAt.After(now) {
			return "", "", ErrTaskLeaseExpired
		}

		task.Lease.LastHeartbeatAt = now
		task.Lease.ExpiresAt = now.Add(leaseDuration(req.LeaseDuration))
		task.Status = StatusInProgress
		task.UpdatedAt = now

		return HistoryHeartbeat, strings.TrimSpace(req.Message), nil
	})
}

// Complete checks off a task and records the completing agent when provided.
func (s *Store) Complete(ctx context.Context, id, agent string) (Task, error) {
	return s.CompleteWithOptions(ctx, id, CompleteRequest{Agent: agent})
}

// CompleteWithOptions checks off a task with conflict detection and audit metadata.
func (s *Store) CompleteWithOptions(ctx context.Context, id string, req CompleteRequest) (Task, error) {
	return s.updateTask(ctx, id, req.ExpectedRevision, req.Actor, func(state *State, task *Task, now time.Time) (HistoryAction, string, error) {
		if task.Status == StatusCompleted {
			return "", "", ErrTaskCompleted
		}

		if task.Status == StatusCanceled {
			return "", "", ErrTaskCanceled
		}

		if task.Status == StatusFailed {
			return "", "", ErrTaskFailed
		}

		if taskIsBlocked(*state, *task) {
			return "", "", fmt.Errorf("%w: dependencies are not completed", ErrTaskBlocked)
		}

		agent := strings.TrimSpace(req.Agent)
		if taskLeaseHeldByAnotherOwner(task, agent, req.SessionID, req.RunID, now) {
			return "", "", ErrTaskLeased
		}

		if agent != "" {
			task.Agent = agent
		}

		task.Status = StatusCompleted
		task.Lease = nil
		task.BlockerReason = ""
		task.ReviewStatus = ReviewStatusNone
		task.UpdatedAt = now
		task.CompletedAt = &now

		return HistoryCompleted, strings.TrimSpace(req.Message), nil
	})
}

func taskLeaseHeldByAnotherOwner(task *Task, agent, sessionID, runID string, now time.Time) bool {
	return task.Lease != nil &&
		!leaseIsExpired(task.Lease, now) &&
		(agent == "" || !sameLeaseOwner(task.Lease, agent, sessionID, runID))
}

// RequestReview marks a task as waiting for review and releases its active lease.
func (s *Store) RequestReview(ctx context.Context, id string, req ReviewRequest) (Task, error) {
	return s.updateTask(ctx, id, req.ExpectedRevision, req.Actor, func(state *State, task *Task, now time.Time) (HistoryAction, string, error) {
		if task.Status == StatusCompleted {
			return "", "", ErrTaskCompleted
		}

		if task.Status == StatusCanceled {
			return "", "", ErrTaskCanceled
		}

		if task.Status == StatusFailed {
			return "", "", ErrTaskFailed
		}

		if taskIsBlocked(*state, *task) {
			return "", "", fmt.Errorf("%w: dependencies are not completed", ErrTaskBlocked)
		}

		agent := strings.TrimSpace(req.Agent)
		if taskLeaseHeldByAnotherOwner(task, agent, req.SessionID, req.RunID, now) {
			return "", "", ErrTaskLeased
		}

		if agent != "" {
			task.Agent = agent
		}

		task.Status = StatusReview
		task.Lease = nil
		task.ReviewStatus = ReviewStatusPending
		task.UpdatedAt = now

		return HistoryReviewRequested, strings.TrimSpace(req.Message), nil
	})
}

// Fail marks a task as failed, records the reason, and releases its active lease.
func (s *Store) Fail(ctx context.Context, id string, req FailRequest) (Task, error) {
	return s.updateTask(ctx, id, req.ExpectedRevision, req.Actor, func(_ *State, task *Task, now time.Time) (HistoryAction, string, error) {
		if task.Status == StatusCompleted {
			return "", "", ErrTaskCompleted
		}

		if task.Status == StatusCanceled {
			return "", "", ErrTaskCanceled
		}

		agent := strings.TrimSpace(req.Agent)
		if taskLeaseHeldByAnotherOwner(task, agent, req.SessionID, req.RunID, now) {
			return "", "", ErrTaskLeased
		}

		if agent != "" {
			task.Agent = agent
		}

		task.Status = StatusFailed
		task.Lease = nil
		task.ReviewStatus = ReviewStatusNone
		task.FailureReason = strings.TrimSpace(req.Reason)
		task.UpdatedAt = now

		return HistoryFailed, strings.TrimSpace(req.Message), nil
	})
}

// Cancel marks a task as canceled and releases its active lease.
func (s *Store) Cancel(ctx context.Context, id string, req CancelRequest) (Task, error) {
	return s.updateTask(ctx, id, req.ExpectedRevision, req.Actor, func(_ *State, task *Task, now time.Time) (HistoryAction, string, error) {
		if task.Status == StatusCompleted {
			return "", "", ErrTaskCompleted
		}

		if task.Status == StatusCanceled {
			return "", "", ErrTaskCanceled
		}

		agent := strings.TrimSpace(req.Agent)
		if taskLeaseHeldByAnotherOwner(task, agent, req.SessionID, req.RunID, now) {
			return "", "", ErrTaskLeased
		}

		if agent != "" {
			task.Agent = agent
		}

		task.Status = StatusCanceled
		task.Lease = nil
		task.ReviewStatus = ReviewStatusNone

		if reason := strings.TrimSpace(req.Reason); reason != "" {
			task.FailureReason = reason
		}

		task.UpdatedAt = now

		return HistoryCanceled, strings.TrimSpace(req.Message), nil
	})
}

// Reopen moves a completed, failed, canceled, or review task back into active planning.
func (s *Store) Reopen(ctx context.Context, id string, req ReopenRequest) (Task, error) {
	return s.updateTask(ctx, id, req.ExpectedRevision, req.Actor, func(state *State, task *Task, now time.Time) (HistoryAction, string, error) {
		if !canReopenStatus(task.Status) {
			return "", "", fmt.Errorf("tasklist: cannot reopen task with status %q", task.Status)
		}

		if task.Status != StatusReopened {
			task.RetryCount++
		}

		if agent := strings.TrimSpace(req.Agent); agent != "" {
			task.Agent = agent
		} else {
			task.Agent = ""
		}

		task.Status = StatusReopened
		task.Lease = nil
		task.ReviewStatus = ReviewStatusNone
		task.CompletedAt = nil

		if req.ClearFailureReason || task.FailureReason != "" {
			task.FailureReason = ""
		}

		if req.ClearBlockerReason {
			task.BlockerReason = ""
		}

		task.UpdatedAt = now
		applyDependencyStatus(state, task)

		return HistoryReopened, strings.TrimSpace(req.Message), nil
	})
}

// Update changes mutable fields on an incomplete task.
func (s *Store) Update(ctx context.Context, id string, req UpdateRequest) (Task, error) {
	return s.updateTask(ctx, id, req.ExpectedRevision, req.Actor, func(state *State, task *Task, now time.Time) (HistoryAction, string, error) {
		if task.Status == StatusCompleted {
			return "", "", ErrTaskCompleted
		}

		if err := applyUpdateRequest(state, task, req, now); err != nil {
			return "", "", err
		}

		task.UpdatedAt = now

		return HistoryUpdated, strings.TrimSpace(req.Message), nil
	})
}

func applyUpdateRequest(state *State, task *Task, req UpdateRequest, now time.Time) error {
	if err := validateUpdateWorkflowRequest(*task, req); err != nil {
		return err
	}

	if title := strings.TrimSpace(req.Title); title != "" {
		task.Title = title
	}

	if req.Metadata != nil {
		task.Metadata = copyMetadata(req.Metadata)
	}

	if err := applyUpdateDependencies(task, req); err != nil {
		return err
	}

	if req.ReplaceDependencies && dependencyCycleForTask(*state, task.ID) {
		return fmt.Errorf("%w: task %q", ErrDependencyCycle, task.ID)
	}

	if req.SetPriority {
		task.Priority = req.Priority
	}

	applyUpdateRisk(task, req)
	applyUpdateBlockerReason(task, req)

	if err := applyUpdateReviewStatus(task, req); err != nil {
		return err
	}

	applyUpdateFailureReason(task, req)

	if len(unmetDependencies(*state, task.Dependencies)) > 0 && updateRequestsAssignment(req) {
		return fmt.Errorf("%w: dependencies are not completed", ErrTaskBlocked)
	}

	if err := applyUpdateOwner(task, req, now); err != nil {
		return err
	}

	applyDependencyStatus(state, task)

	return ensureAssignedLease(task, now)
}

func validateUpdateWorkflowRequest(task Task, req UpdateRequest) error {
	if req.Status != "" {
		requested := canonicalStatusForWrite(req.Status)
		if requested == StatusReview || requested == StatusFailed || requested == StatusCanceled {
			return fmt.Errorf("tasklist: use dedicated workflow method for status %q", requested)
		}
	}

	if workflowStatusRequiresReopen(task.Status) && updateRequestsAssignment(req) {
		return fmt.Errorf("tasklist: task status %q requires Reopen before assignment", task.Status)
	}

	if workflowStatusRequiresReopen(task.Status) && req.Status != "" && canonicalStatusForWrite(req.Status) != task.Status {
		return fmt.Errorf("tasklist: task status %q requires Reopen before transition", task.Status)
	}

	return nil
}

func workflowStatusRequiresReopen(status Status) bool {
	switch status {
	case StatusReview, StatusFailed, StatusCanceled:
		return true
	default:
		return false
	}
}

func updateRequestsAssignment(req UpdateRequest) bool {
	return strings.TrimSpace(req.Agent) != "" || isInProgressStatus(req.Status)
}

func applyUpdateOwner(task *Task, req UpdateRequest, now time.Time) error {
	agent := strings.TrimSpace(req.Agent)
	statusRequested := req.Status != ""

	if statusRequested {
		if err := applyRequestedOwnerStatus(task, req.Status); err != nil {
			return err
		}
	}

	if agent == "" {
		return nil
	}

	if statusRequested && !isInProgressStatus(req.Status) {
		applyNonProgressAgent(task, agent)

		return nil
	}

	assignTaskOwner(task, agent, now)

	return nil
}

func applyRequestedOwnerStatus(task *Task, status Status) error {
	if err := validateIncompleteStatus(status); err != nil {
		return err
	}

	task.Status = canonicalStatusForWrite(status)
	switch {
	case isInProgressStatus(task.Status):
	case task.Status == StatusReview || task.Status == StatusFailed || task.Status == StatusCanceled:
		task.Lease = nil
	default:
		clearTaskOwner(task)
	}

	return nil
}

func applyNonProgressAgent(task *Task, agent string) {
	if task.Status == StatusReview || task.Status == StatusFailed || task.Status == StatusCanceled {
		task.Agent = agent
	}
}

func assignTaskOwner(task *Task, agent string, now time.Time) {
	task.Agent = agent
	task.Status = StatusInProgress
	task.AttemptCount++
	task.Lease = newLease(agent, "", "", now, 0)
}

func applyUpdateDependencies(task *Task, req UpdateRequest) error {
	if !req.ReplaceDependencies {
		return nil
	}

	dependencies, err := sanitizeDependencies(req.Dependencies, task.ID)
	if err != nil {
		return err
	}

	task.Dependencies = dependencies

	return nil
}

func applyUpdateReviewStatus(task *Task, req UpdateRequest) error {
	if req.ReviewStatus == "" {
		return nil
	}

	if err := validateReviewStatus(req.ReviewStatus); err != nil {
		return err
	}

	task.ReviewStatus = req.ReviewStatus

	return nil
}

func applyUpdateRisk(task *Task, req UpdateRequest) {
	if req.ClearRisk {
		task.Risk = ""
		return
	}

	if risk := strings.TrimSpace(req.Risk); risk != "" {
		task.Risk = risk
	}
}

func applyUpdateBlockerReason(task *Task, req UpdateRequest) {
	if req.ClearBlockerReason {
		task.BlockerReason = ""
		return
	}

	if blockerReason := strings.TrimSpace(req.BlockerReason); blockerReason != "" {
		task.BlockerReason = blockerReason
		task.Status = StatusBlocked
		clearTaskOwner(task)
	}
}

func applyUpdateFailureReason(task *Task, req UpdateRequest) {
	if req.ClearFailureReason {
		task.FailureReason = ""
		return
	}

	if failureReason := strings.TrimSpace(req.FailureReason); failureReason != "" {
		task.FailureReason = failureReason
	}
}

func applyDependencyStatus(state *State, task *Task) {
	unmet := unmetDependencies(*state, task.Dependencies)
	if len(unmet) > 0 {
		task.Status = StatusBlocked
		task.BlockerReason = dependencyBlockerReason(unmet)
		clearTaskOwner(task)

		return
	}

	if isDependencyBlockerReason(task.BlockerReason) {
		task.BlockerReason = ""
	}

	if task.Status == StatusBlocked && strings.TrimSpace(task.BlockerReason) == "" {
		task.Status = StatusReady
		clearTaskOwner(task)
	}
}

func ensureAssignedLease(task *Task, now time.Time) error {
	if !isInProgressStatus(task.Status) || task.Lease != nil {
		return nil
	}

	if strings.TrimSpace(task.Agent) == "" {
		return errors.New("tasklist: agent is required for assigned tasks")
	}

	task.AttemptCount++
	task.Status = StatusInProgress
	task.Lease = newLease(task.Agent, "", "", now, 0)

	return nil
}

// Reconcile repairs stale derived coordination state in the task file.
//
// It expires leases, marks tasks with incomplete dependencies as blocked, and
// unblocks tasks whose dependencies are now completed. Each repair is recorded
// as an append-only history entry with before/after snapshots.
func (s *Store) Reconcile(ctx context.Context, req ReconcileRequest) (ReconcileResult, error) {
	if err := ctxErr(ctx); err != nil {
		return ReconcileResult{}, err
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	var result ReconcileResult

	err := s.withFileLock(ctx, func() error {
		state, err := s.loadFile(ctx)
		if err != nil {
			return err
		}

		now := s.now().UTC()
		actor := actorIdentity(req.Actor, "")
		message := strings.TrimSpace(req.Message)

		result = reconcileState(&state, now, actor, message)
		if result.HistoryEntries == 0 {
			return nil
		}

		if err := s.saveFile(ctx, state); err != nil {
			return err
		}

		return nil
	})
	if err != nil {
		return ReconcileResult{}, err
	}

	result.Tasks = cloneTasks(result.Tasks)

	return result, nil
}

// Repair backs up and rewrites an unreadable or internally conflicted task file.
//
// Repair is intentionally conservative: it only rewrites when the current file
// cannot be loaded as valid state or when parseable JSON contains conflicts
// that can be normalized without inventing task content. The original bytes are
// copied to BackupPath before the repaired state replaces the task file.
func (s *Store) Repair(ctx context.Context, req RepairRequest) (RepairResult, error) {
	if err := ctxErr(ctx); err != nil {
		return RepairResult{}, err
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	var result RepairResult

	err := s.withFileLock(ctx, func() error {
		repairResult, err := s.repairLocked(ctx, req)
		if err != nil {
			return err
		}

		result = repairResult

		return nil
	})
	if err != nil {
		return RepairResult{}, err
	}

	return result, nil
}

func (s *Store) repairLocked(ctx context.Context, req RepairRequest) (RepairResult, error) {
	// #nosec G703 -- task list paths are explicit user/session configuration.
	raw, err := os.ReadFile(s.path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return RepairResult{}, nil
		}

		return RepairResult{}, fmt.Errorf("tasklist: read %s: %w", s.path, err)
	}

	now := s.now().UTC()
	actor := actorIdentity(req.Actor, "")
	message := strings.TrimSpace(req.Message)

	var state State

	parseErr := json.Unmarshal(raw, &state)
	if parseErr != nil {
		return s.repairMalformedState(ctx, raw, now, actor, message, parseErr)
	}

	return s.repairParsedState(ctx, raw, state, now, actor, message)
}

func (s *Store) repairMalformedState(ctx context.Context, raw []byte, now time.Time, actor, message string, parseErr error) (RepairResult, error) {
	backupPath, err := s.backupFile(raw, now)
	if err != nil {
		return RepairResult{}, err
	}

	state := State{}
	appendRepairHistory(&state, now, actor, repairMessage(message, fmt.Sprintf("malformed JSON: %v", parseErr)))

	if saveErr := s.saveFile(ctx, state); saveErr != nil {
		return RepairResult{}, saveErr
	}

	return RepairResult{
		BackupPath:     backupPath,
		StateRevision:  state.Revision,
		HistoryEntries: 1,
		Repaired:       true,
	}, nil
}

func (s *Store) repairParsedState(ctx context.Context, raw []byte, state State, now time.Time, actor, message string) (RepairResult, error) {
	loadedErr := validateState(state)
	repaired, recovered, dropped := repairLoadedState(&state, now)

	if loadedErr == nil && !repaired {
		return RepairResult{
			StateRevision:  state.Revision,
			TasksRecovered: len(state.Tasks),
		}, nil
	}

	backupPath, err := s.backupFile(raw, now)
	if err != nil {
		return RepairResult{}, err
	}

	reason := "normalized conflicted task state"
	if loadedErr != nil {
		reason = loadedErr.Error()
	}

	appendRepairHistory(&state, now, actor, repairMessage(message, reason))

	if saveErr := s.saveFile(ctx, state); saveErr != nil {
		return RepairResult{}, saveErr
	}

	return RepairResult{
		BackupPath:     backupPath,
		StateRevision:  state.Revision,
		TasksRecovered: recovered,
		TasksDropped:   dropped,
		HistoryEntries: 1,
		Repaired:       true,
	}, nil
}

func (s *Store) backupFile(raw []byte, now time.Time) (string, error) {
	backupPath := fmt.Sprintf("%s.repair-%s.bak", s.path, now.Format("20060102T150405.000000000Z"))
	// #nosec G306,G703 -- backup path is derived from the explicit task-list path.
	if err := os.WriteFile(backupPath, raw, 0o600); err != nil {
		return "", fmt.Errorf("tasklist: backup %s: %w", backupPath, err)
	}

	return backupPath, nil
}

func appendRepairHistory(state *State, now time.Time, actor, message string) {
	state.Revision = nextStateRevision(*state)
	state.History = appendHistory(state.History, historyRecord{
		at:            now,
		action:        HistoryRepaired,
		taskID:        "__state__",
		actor:         actor,
		message:       message,
		stateRevision: state.Revision,
	})
}

func repairMessage(message, reason string) string {
	message = strings.TrimSpace(message)

	reason = strings.TrimSpace(reason)

	if message == "" {
		return reason
	}

	if reason == "" {
		return message
	}

	return message + ": " + reason
}

func repairLoadedState(state *State, now time.Time) (repaired bool, recovered, dropped int) {
	repaired = repairHistory(state, now) || repaired

	seen := make(map[string]struct{}, len(state.Tasks))

	tasks := make([]Task, 0, len(state.Tasks))
	for i := range state.Tasks {
		task, changed, ok := repairTask(state.Tasks[i], now)
		if !ok {
			dropped++
			repaired = true

			continue
		}

		if _, exists := seen[task.ID]; exists {
			dropped++
			repaired = true

			continue
		}

		seen[task.ID] = struct{}{}
		tasks = append(tasks, task)
		recovered++
		repaired = changed || repaired
	}

	state.Tasks = tasks
	for i := range state.Tasks {
		before := cloneTask(state.Tasks[i])
		applyDependencyStatus(state, &state.Tasks[i])

		if !tasksEquivalentForRepair(before, state.Tasks[i]) {
			repaired = true
		}
	}

	return repaired, recovered, dropped
}

//nolint:gocognit,cyclop // Repair normalization intentionally handles independent corrupt field cases.
func repairTask(task Task, now time.Time) (Task, bool, bool) {
	changed := false

	trimmedID := strings.TrimSpace(task.ID)

	trimmedTitle := strings.TrimSpace(task.Title)
	if trimmedID == "" || trimmedTitle == "" {
		return Task{}, true, false
	}

	if task.ID != trimmedID {
		task.ID = trimmedID
		changed = true
	}

	if task.Title != trimmedTitle {
		task.Title = trimmedTitle
		changed = true
	}

	if task.CreatedAt.IsZero() {
		task.CreatedAt = now
		changed = true
	}

	if task.UpdatedAt.IsZero() {
		task.UpdatedAt = task.CreatedAt
		changed = true
	}

	status, statusChanged := repairStatus(task.Status)
	task.Status = status
	changed = statusChanged || changed

	dependencies, depsChanged := repairDependencies(task.Dependencies, task.ID)
	task.Dependencies = dependencies
	changed = depsChanged || changed

	task.Metadata = copyMetadata(task.Metadata)

	if risk := strings.TrimSpace(task.Risk); risk != task.Risk {
		task.Risk = risk
		changed = true
	}

	if blockerReason := strings.TrimSpace(task.BlockerReason); blockerReason != task.BlockerReason {
		task.BlockerReason = blockerReason
		changed = true
	}

	if failureReason := strings.TrimSpace(task.FailureReason); failureReason != task.FailureReason {
		task.FailureReason = failureReason
		changed = true
	}

	if task.AttemptCount < 0 {
		task.AttemptCount = 0
		changed = true
	}

	if task.RetryCount < 0 {
		task.RetryCount = 0
		changed = true
	}

	if task.Status == StatusCompleted && task.CompletedAt == nil {
		completedAt := task.UpdatedAt
		task.CompletedAt = &completedAt
		changed = true
	}

	if task.Status != StatusCompleted && task.CompletedAt != nil {
		task.CompletedAt = nil
		changed = true
	}

	if task.Lease != nil && !validLease(*task.Lease) {
		task.Lease = nil
		changed = true
	}

	if task.Lease != nil && !isInProgressStatus(task.Status) {
		task.Lease = nil
		if !statusRetainsHistoricalAgent(task.Status) && task.Agent != "" {
			task.Agent = ""
		}

		changed = true
	}

	if task.Lease == nil && isInProgressStatus(task.Status) {
		task.Status = StatusReady
		task.Agent = ""
		changed = true
	}

	if task.Lease != nil && task.Agent != task.Lease.Owner {
		task.Agent = task.Lease.Owner
		changed = true
	}

	return task, changed, true
}

func repairStatus(status Status) (Status, bool) {
	switch status {
	case StatusReady, StatusBlocked, StatusInProgress, StatusReview, StatusFailed, StatusCanceled, StatusReopened, StatusCompleted:
		return status, false
	case StatusPending:
		return StatusReady, true
	case StatusAssigned:
		return StatusInProgress, true
	default:
		return StatusFailed, true
	}
}

func repairDependencies(dependencies []string, selfID string) ([]string, bool) {
	repaired, err := sanitizeDependencies(dependencies, selfID)
	if err == nil {
		return repaired, !stringSlicesEqual(repaired, dependencies)
	}

	out := make([]string, 0, len(dependencies))

	seen := make(map[string]struct{}, len(dependencies))
	for _, dependency := range dependencies {
		dependency = strings.TrimSpace(dependency)
		if dependency == "" || dependency == strings.TrimSpace(selfID) {
			continue
		}

		if _, ok := seen[dependency]; ok {
			continue
		}

		seen[dependency] = struct{}{}
		out = append(out, dependency)
	}

	sort.Strings(out)

	return out, true
}

func repairHistory(state *State, now time.Time) bool {
	repaired := false

	history := make([]HistoryEntry, 0, len(state.History))
	seenSeq := make(map[int64]struct{}, len(state.History))
	nextSeq := int64(1)

	for i := range state.History {
		entry := state.History[i]
		if strings.TrimSpace(entry.TaskID) == "" || !isValidHistoryAction(entry.Action) {
			repaired = true
			continue
		}

		entry.Actor = actorIdentity(entry.Actor, "")
		entry.Agent = strings.TrimSpace(entry.Agent)
		entry.Message = strings.TrimSpace(entry.Message)

		if entry.At.IsZero() {
			entry.At = now
			repaired = true
		}

		if entry.Seq <= 0 {
			entry.Seq = nextSeq
			repaired = true
		}

		if _, exists := seenSeq[entry.Seq]; exists {
			entry.Seq = nextSeq
			repaired = true
		}

		seenSeq[entry.Seq] = struct{}{}
		if entry.Seq >= nextSeq {
			nextSeq = entry.Seq + 1
		}

		history = append(history, entry)
	}

	state.History = history

	return repaired
}

func validLease(lease Lease) bool {
	return strings.TrimSpace(lease.Owner) != "" &&
		!lease.AcquiredAt.IsZero() &&
		!lease.LastHeartbeatAt.IsZero() &&
		!lease.ExpiresAt.IsZero()
}

func tasksEquivalentForRepair(a, b Task) bool {
	return a.ID == b.ID &&
		a.Title == b.Title &&
		a.Status == b.Status &&
		a.Agent == b.Agent &&
		a.BlockerReason == b.BlockerReason &&
		stringSlicesEqual(a.Dependencies, b.Dependencies)
}

func reconcileState(state *State, now time.Time, actor, message string) ReconcileResult {
	var result ReconcileResult

	for idx := range state.Tasks {
		change, changed := reconcileTaskState(*state, &state.Tasks[idx], now)
		if !changed {
			continue
		}

		after := appendReconcileHistory(state, idx, change, now, actor, message)
		addReconcileResult(&result, change, after)
	}

	result.StateRevision = state.Revision

	return result
}

func appendReconcileHistory(state *State, idx int, change reconcileChange, now time.Time, actor, message string) Task {
	state.Revision = nextStateRevision(*state)
	state.Tasks[idx].Revision = state.Revision
	state.Tasks[idx].UpdatedAt = now

	after := cloneTask(state.Tasks[idx])
	state.History = appendHistory(state.History, historyRecord{
		at:            now,
		action:        HistoryReconciled,
		taskID:        after.ID,
		agent:         after.Agent,
		actor:         actor,
		message:       message,
		stateRevision: state.Revision,
		before:        &change.before,
		after:         &after,
	})

	return after
}

func addReconcileResult(result *ReconcileResult, change reconcileChange, after Task) {
	if change.expiredLease {
		result.ExpiredLeases++
	}

	if change.before.Status != StatusBlocked && after.Status == StatusBlocked {
		result.Blocked++
	}

	if change.before.Status == StatusBlocked && after.Status != StatusBlocked {
		result.Unblocked++
	}

	result.Tasks = append(result.Tasks, after)
	result.HistoryEntries++
}

type reconcileChange struct {
	before       Task
	expiredLease bool
}

func reconcileTaskState(state State, task *Task, now time.Time) (reconcileChange, bool) {
	if task.Status == StatusCompleted {
		return reconcileChange{}, false
	}

	change := reconcileChange{before: cloneTask(*task)}
	changed := repairAssignedLease(task, now, &change)
	unmet := unmetDependencies(state, task.Dependencies)

	if reconcileDependencyBlock(task, unmet) {
		return change, true
	}

	if reconcileManualBlock(task, len(unmet) > 0) {
		return change, true
	}

	if clearReconcileStaleOwner(task) {
		changed = true
	}

	return change, changed
}

func reconcileDependencyBlock(task *Task, unmet []string) bool {
	if len(unmet) == 0 || isDependencyBlockerReason(task.BlockerReason) {
		return false
	}

	task.BlockerReason = dependencyBlockerReason(unmet)
	task.Status = StatusBlocked
	clearTaskOwner(task)

	return true
}

func reconcileManualBlock(task *Task, dependencyBlocked bool) bool {
	blocked := dependencyBlocked || hasManualBlocker(task.BlockerReason, dependencyBlocked)
	switch {
	case blocked && task.Status != StatusBlocked:
		task.Status = StatusBlocked
		clearTaskOwner(task)

		return true
	case !blocked && isDependencyBlockerReason(task.BlockerReason):
		task.BlockerReason = ""
		if task.Status == StatusBlocked {
			task.Status = StatusReady
			clearTaskOwner(task)
		}

		return true
	case !blocked && task.Status == StatusBlocked:
		task.Status = StatusReady
		clearTaskOwner(task)

		return true
	default:
		return false
	}
}

func hasManualBlocker(reason string, dependencyBlocked bool) bool {
	reason = strings.TrimSpace(reason)

	return reason != "" && (!isDependencyBlockerReason(reason) || dependencyBlocked)
}

func clearReconcileStaleOwner(task *Task) bool {
	if isInProgressStatus(task.Status) || !hasTaskOwner(task) || statusRetainsHistoricalAgent(task.Status) {
		return false
	}

	clearTaskOwner(task)

	return true
}

func repairAssignedLease(task *Task, now time.Time, change *reconcileChange) bool {
	if !isInProgressStatus(task.Status) {
		return false
	}

	legacyAssigned := task.Status == StatusAssigned

	if leaseIsExpired(task.Lease, now) {
		clearTaskOwner(task)
		task.Status = StatusReady
		change.expiredLease = true

		return true
	}

	if task.Status == StatusAssigned {
		task.Status = StatusInProgress
	}

	if task.Agent != task.Lease.Owner {
		task.Agent = task.Lease.Owner

		return true
	}

	return legacyAssigned
}

func clearTaskOwner(task *Task) {
	task.Agent = ""
	task.Lease = nil
}

func hasTaskOwner(task *Task) bool {
	return strings.TrimSpace(task.Agent) != "" || task.Lease != nil
}

func statusRetainsHistoricalAgent(status Status) bool {
	switch status {
	case StatusCompleted, StatusReview, StatusFailed, StatusCanceled:
		return true
	default:
		return false
	}
}

func (s *Store) assign(ctx context.Context, id string, req AssignRequest, replaceLiveLease bool) (Task, error) {
	agent := strings.TrimSpace(req.Agent)
	if agent == "" {
		return Task{}, errors.New("tasklist: agent is required")
	}

	return s.updateTask(ctx, id, req.ExpectedRevision, req.Actor, func(state *State, task *Task, now time.Time) (HistoryAction, string, error) {
		if task.Status == StatusCompleted {
			return "", "", ErrTaskCompleted
		}

		if task.Status == StatusCanceled {
			return "", "", ErrTaskCanceled
		}

		if task.Status == StatusFailed {
			return "", "", ErrTaskFailed
		}

		if task.Status == StatusReview {
			return "", "", errors.New("tasklist: task is in review")
		}

		if taskIsBlocked(*state, *task) {
			return "", "", fmt.Errorf("%w: dependencies are not completed", ErrTaskBlocked)
		}

		if !replaceLiveLease && isInProgressStatus(task.Status) && !leaseIsExpired(task.Lease, now) && !sameLeaseOwner(task.Lease, agent, req.SessionID, req.RunID) {
			return "", "", ErrTaskLeased
		}

		if shouldCountAssignmentAttempt(task, agent, req.SessionID, req.RunID, now) {
			task.AttemptCount++
		}

		task.Agent = agent
		task.Status = StatusInProgress
		task.Lease = newLease(agent, req.SessionID, req.RunID, now, req.LeaseDuration)
		task.UpdatedAt = now

		return HistoryAssigned, strings.TrimSpace(req.Message), nil
	})
}

func shouldCountAssignmentAttempt(task *Task, agent, sessionID, runID string, now time.Time) bool {
	return !isInProgressStatus(task.Status) || leaseIsExpired(task.Lease, now) || !sameLeaseOwner(task.Lease, agent, sessionID, runID)
}

func (s *Store) updateTask(ctx context.Context, id string, expectedRevision int64, actor string, mutate func(*State, *Task, time.Time) (HistoryAction, string, error)) (Task, error) {
	if err := ctxErr(ctx); err != nil {
		return Task{}, err
	}

	id = strings.TrimSpace(id)
	if id == "" {
		return Task{}, errors.New("tasklist: id is required")
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	var updated Task

	err := s.withFileLock(ctx, func() error {
		state, err := s.loadFile(ctx)
		if err != nil {
			return err
		}

		idx := findTask(state.Tasks, id)
		if idx < 0 {
			return ErrTaskNotFound
		}

		if expectedRevision > 0 && state.Tasks[idx].Revision != expectedRevision {
			return ErrRevisionConflict
		}

		now := s.now().UTC()
		before := cloneTask(state.Tasks[idx])

		action, message, err := mutate(&state, &state.Tasks[idx], now)
		if err != nil {
			return err
		}

		state.Revision = nextStateRevision(state)
		state.Tasks[idx].Revision = state.Revision
		updated = cloneTask(state.Tasks[idx])
		entryActor := actorIdentity(actor, historyActor(action, updated, before))
		state.History = appendHistory(state.History, historyRecord{
			at:            now,
			action:        action,
			taskID:        updated.ID,
			agent:         updated.Agent,
			actor:         entryActor,
			message:       message,
			stateRevision: state.Revision,
			before:        &before,
			after:         &updated,
		})

		if err := s.saveFile(ctx, state); err != nil {
			return err
		}

		return nil
	})
	if err != nil {
		return Task{}, err
	}

	return cloneTask(updated), nil
}

func (s *Store) withFileLock(ctx context.Context, fn func() error) error {
	if s.path == "" {
		return errors.New("tasklist: path is required")
	}

	if err := ctxErr(ctx); err != nil {
		return err
	}

	dir := filepath.Dir(s.path)
	// #nosec G703 -- task list paths are explicit user/session configuration.
	if err := os.MkdirAll(dir, 0o750); err != nil {
		return fmt.Errorf("tasklist: create dir: %w", err)
	}

	lockPath := filepath.Join(dir, "."+filepath.Base(s.path)+".lock")
	// #nosec G703 -- task list paths are explicit user/session configuration.
	file, err := os.OpenFile(lockPath, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return fmt.Errorf("tasklist: open lock: %w", err)
	}
	defer file.Close()

	if err := lockTaskFile(ctx, file); err != nil {
		return err
	}
	defer unlockTaskFile(file) //nolint:errcheck // Best-effort unlock before closing the descriptor.

	return fn()
}

func (s *Store) loadFile(ctx context.Context) (State, error) {
	if s.path == "" {
		return State{}, errors.New("tasklist: path is required")
	}

	if err := ctxErr(ctx); err != nil {
		return State{}, err
	}

	// #nosec G703 -- task list paths are explicit user/session configuration.
	data, err := os.ReadFile(s.path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return State{}, nil
		}

		return State{}, fmt.Errorf("tasklist: read %s: %w", s.path, err)
	}

	if err := ctxErr(ctx); err != nil {
		return State{}, err
	}

	var state State
	if err := json.Unmarshal(data, &state); err != nil {
		return State{}, fmt.Errorf("tasklist: parse %s: %w", s.path, err)
	}

	if err := validateState(state); err != nil {
		return State{}, err
	}

	sortState(&state)

	return state, nil
}

func (s *Store) saveFile(ctx context.Context, state State) error {
	if s.path == "" {
		return errors.New("tasklist: path is required")
	}

	if err := validateState(state); err != nil {
		return err
	}

	if err := ctxErr(ctx); err != nil {
		return err
	}

	sortState(&state)

	data, err := json.MarshalIndent(state, "", "  ")
	if err != nil {
		return fmt.Errorf("tasklist: marshal: %w", err)
	}

	data = append(data, '\n')

	dir := filepath.Dir(s.path)

	// #nosec G703 -- task list paths are explicit user/session configuration.
	if mkdirErr := os.MkdirAll(dir, 0o750); mkdirErr != nil {
		return fmt.Errorf("tasklist: create dir: %w", mkdirErr)
	}

	if contextErr := ctxErr(ctx); contextErr != nil {
		return contextErr
	}

	tmp, err := os.CreateTemp(dir, ".tasklist-*.json")
	if err != nil {
		return fmt.Errorf("tasklist: create temp: %w", err)
	}

	tmpPath := tmp.Name()
	defer os.Remove(tmpPath)

	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("tasklist: write temp: %w", err)
	}

	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("tasklist: sync temp: %w", err)
	}

	if err := tmp.Close(); err != nil {
		return fmt.Errorf("tasklist: close temp: %w", err)
	}

	if err := ctxErr(ctx); err != nil {
		return err
	}

	// #nosec G703 -- task list paths are explicit user/session configuration.
	if err := os.Rename(tmpPath, s.path); err != nil {
		return fmt.Errorf("tasklist: replace %s: %w", s.path, err)
	}

	if err := syncTaskDir(dir); err != nil {
		return err
	}

	return nil
}

//nolint:govet // Field order follows appendHistory call sites.
type historyRecord struct {
	at            time.Time
	action        HistoryAction
	taskID        string
	agent         string
	actor         string
	message       string
	stateRevision int64
	before        *Task
	after         *Task
}

func appendHistory(history []HistoryEntry, record historyRecord) []HistoryEntry {
	return append(history, HistoryEntry{
		Seq:           nextSeq(history),
		At:            record.at,
		Action:        record.action,
		TaskID:        record.taskID,
		Actor:         actorIdentity(record.actor, record.agent),
		Agent:         strings.TrimSpace(record.agent),
		Message:       strings.TrimSpace(record.message),
		StateRevision: record.stateRevision,
		Before:        cloneTaskPtr(record.before),
		After:         cloneTaskPtr(record.after),
	})
}

func nextSeq(history []HistoryEntry) int64 {
	var seq int64
	for i := range history {
		if history[i].Seq > seq {
			seq = history[i].Seq
		}
	}

	return seq + 1
}

func nextStateRevision(state State) int64 {
	revision := state.Revision
	for i := range state.Tasks {
		if state.Tasks[i].Revision > revision {
			revision = state.Tasks[i].Revision
		}
	}

	for i := range state.History {
		if state.History[i].StateRevision > revision {
			revision = state.History[i].StateRevision
		}
	}

	return revision + 1
}

func newID() string {
	var buf [8]byte
	if _, err := rand.Read(buf[:]); err == nil {
		return hex.EncodeToString(buf[:])
	}

	return strconv.FormatInt(time.Now().UTC().UnixNano(), 10)
}

func validateState(state State) error {
	seen := make(map[string]struct{}, len(state.Tasks))
	for i := range state.Tasks {
		if err := validateTask(state.Tasks[i]); err != nil {
			return err
		}

		if _, ok := seen[state.Tasks[i].ID]; ok {
			return fmt.Errorf("tasklist: duplicate task id %q", state.Tasks[i].ID)
		}

		seen[state.Tasks[i].ID] = struct{}{}
	}

	for i := range state.History {
		if state.History[i].Seq <= 0 {
			return errors.New("tasklist: history seq is required")
		}

		if state.History[i].At.IsZero() {
			return errors.New("tasklist: history at is required")
		}

		if strings.TrimSpace(state.History[i].TaskID) == "" {
			return errors.New("tasklist: history task id is required")
		}

		if !isValidHistoryAction(state.History[i].Action) {
			return fmt.Errorf("tasklist: invalid history action %q", state.History[i].Action)
		}

		if strings.TrimSpace(state.History[i].Actor) == "" {
			return errors.New("tasklist: history actor is required")
		}
	}

	seenHistorySeq := make(map[int64]struct{}, len(state.History))
	for i := range state.History {
		if _, ok := seenHistorySeq[state.History[i].Seq]; ok {
			return fmt.Errorf("tasklist: duplicate history seq %d", state.History[i].Seq)
		}

		seenHistorySeq[state.History[i].Seq] = struct{}{}
	}

	return nil
}

func validateTask(task Task) error {
	for _, validate := range []func(Task) error{
		validateTaskRequiredFields,
		validateTaskStatusFields,
		validateTaskCompletion,
		validateTaskLease,
		validateTaskDependencies,
		validateTaskCounters,
	} {
		if err := validate(task); err != nil {
			return err
		}
	}

	return nil
}

func validateTaskRequiredFields(task Task) error {
	if strings.TrimSpace(task.ID) == "" {
		return errors.New("tasklist: id is required")
	}

	if strings.TrimSpace(task.Title) == "" {
		return errors.New("tasklist: title is required")
	}

	if task.CreatedAt.IsZero() {
		return errors.New("tasklist: created_at is required")
	}

	if task.UpdatedAt.IsZero() {
		return errors.New("tasklist: updated_at is required")
	}

	return nil
}

func validateTaskStatusFields(task Task) error {
	if err := validateStatus(task.Status); err != nil {
		return err
	}

	if err := validateReviewStatus(task.ReviewStatus); err != nil {
		return err
	}

	return nil
}

func validateTaskCompletion(task Task) error {
	if task.Status == StatusCompleted && task.CompletedAt == nil {
		return errors.New("tasklist: completed_at is required for completed tasks")
	}

	if task.Status != StatusCompleted && task.CompletedAt != nil {
		return errors.New("tasklist: completed_at is only valid for completed tasks")
	}

	return nil
}

func validateTaskLease(task Task) error {
	if task.Lease == nil {
		return nil
	}

	// Blocked tasks may carry stale leases from manual edits; keep them loadable
	// so Reconcile can clear the lease before the task becomes claimable.
	if task.Status != StatusBlocked && !isInProgressStatus(task.Status) {
		return errors.New("tasklist: lease is only valid for in-progress tasks")
	}

	if strings.TrimSpace(task.Lease.Owner) == "" {
		return errors.New("tasklist: lease owner is required")
	}

	if task.Lease.AcquiredAt.IsZero() {
		return errors.New("tasklist: lease acquired_at is required")
	}

	if task.Lease.LastHeartbeatAt.IsZero() {
		return errors.New("tasklist: lease last_heartbeat_at is required")
	}

	if task.Lease.ExpiresAt.IsZero() {
		return errors.New("tasklist: lease expires_at is required")
	}

	return nil
}

func validateTaskDependencies(task Task) error {
	if _, err := sanitizeDependencies(task.Dependencies, task.ID); err != nil {
		return err
	}

	return nil
}

func validateTaskCounters(task Task) error {
	if task.AttemptCount < 0 {
		return errors.New("tasklist: attempt_count cannot be negative")
	}

	if task.RetryCount < 0 {
		return errors.New("tasklist: retry_count cannot be negative")
	}

	return nil
}

func validateStatus(status Status) error {
	switch status {
	case StatusReady, StatusPending, StatusBlocked, StatusInProgress, StatusAssigned, StatusReview, StatusFailed, StatusCanceled, StatusReopened, StatusCompleted:
		return nil
	default:
		return fmt.Errorf("tasklist: invalid status %q", status)
	}
}

func isValidHistoryAction(action HistoryAction) bool {
	switch action {
	case HistoryAdded,
		HistoryAssigned,
		HistoryUpdated,
		HistoryHeartbeat,
		HistoryReconciled,
		HistoryCompleted,
		HistoryReviewRequested,
		HistoryFailed,
		HistoryCanceled,
		HistoryReopened,
		HistoryRepaired:
		return true
	default:
		return false
	}
}

func validateIncompleteStatus(status Status) error {
	if status == StatusCompleted {
		return errors.New("tasklist: use Complete to complete a task")
	}

	return validateStatus(status)
}

func canonicalStatusForWrite(status Status) Status {
	switch status {
	case StatusPending:
		return StatusReady
	case StatusAssigned:
		return StatusInProgress
	default:
		return status
	}
}

func isInProgressStatus(status Status) bool {
	return status == StatusInProgress || status == StatusAssigned
}

func canReopenStatus(status Status) bool {
	switch status {
	case StatusCompleted, StatusReview, StatusFailed, StatusCanceled, StatusReopened:
		return true
	default:
		return false
	}
}

func validateReviewStatus(status ReviewStatus) error {
	switch status {
	case ReviewStatusNone, ReviewStatusPending, ReviewStatusApproved, ReviewStatusChangesRequested:
		return nil
	default:
		return fmt.Errorf("tasklist: invalid review status %q", status)
	}
}

func findTask(tasks []Task, id string) int {
	for i := range tasks {
		if tasks[i].ID == id {
			return i
		}
	}

	return -1
}

func sortState(state *State) {
	sort.SliceStable(state.Tasks, func(i, j int) bool {
		if !state.Tasks[i].CreatedAt.Equal(state.Tasks[j].CreatedAt) {
			return state.Tasks[i].CreatedAt.Before(state.Tasks[j].CreatedAt)
		}

		return state.Tasks[i].ID < state.Tasks[j].ID
	})
	sort.SliceStable(state.History, func(i, j int) bool {
		if state.History[i].Seq != state.History[j].Seq {
			return state.History[i].Seq < state.History[j].Seq
		}

		if !state.History[i].At.Equal(state.History[j].At) {
			return state.History[i].At.Before(state.History[j].At)
		}

		return state.History[i].TaskID < state.History[j].TaskID
	})
}

func copyMetadata(metadata map[string]string) map[string]string {
	if len(metadata) == 0 {
		return nil
	}

	copied := make(map[string]string, len(metadata))
	for key, value := range metadata {
		key = strings.TrimSpace(key)
		if key == "" {
			continue
		}

		copied[key] = value
	}

	if len(copied) == 0 {
		return nil
	}

	return copied
}

func sanitizeDependencies(dependencies []string, selfID string) ([]string, error) {
	if len(dependencies) == 0 {
		return nil, nil
	}

	seen := make(map[string]struct{}, len(dependencies))
	out := make([]string, 0, len(dependencies))

	selfID = strings.TrimSpace(selfID)

	for _, dependency := range dependencies {
		dependency = strings.TrimSpace(dependency)
		if dependency == "" {
			continue
		}

		if dependency == selfID {
			return nil, fmt.Errorf("tasklist: task %q cannot depend on itself", selfID)
		}

		if _, ok := seen[dependency]; ok {
			continue
		}

		seen[dependency] = struct{}{}
		out = append(out, dependency)
	}

	if len(out) == 0 {
		return nil, nil
	}

	sort.Strings(out)

	return out, nil
}

func stringSlicesEqual(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}

	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}

	return true
}

func unmetDependencies(state State, dependencies []string) []string {
	if len(dependencies) == 0 {
		return nil
	}

	unmet := make([]string, 0, len(dependencies))
	for _, dependency := range dependencies {
		idx := findTask(state.Tasks, dependency)
		if idx < 0 || state.Tasks[idx].Status != StatusCompleted {
			unmet = append(unmet, dependency)
		}
	}

	return unmet
}

const dependencyBlockerPrefix = "waiting on dependencies: "

func taskIsBlocked(state State, task Task) bool {
	return len(unmetDependencies(state, task.Dependencies)) > 0 || strings.TrimSpace(task.BlockerReason) != "" || task.Status == StatusBlocked
}

func dependencyBlockerReason(dependencies []string) string {
	if len(dependencies) == 0 {
		return ""
	}

	dependencies = append([]string(nil), dependencies...)
	sort.Strings(dependencies)

	return dependencyBlockerPrefix + strings.Join(dependencies, ",")
}

func isDependencyBlockerReason(reason string) bool {
	return strings.HasPrefix(strings.TrimSpace(reason), dependencyBlockerPrefix)
}

func stateWithTask(state State, task Task) State {
	tasks := append([]Task(nil), state.Tasks...)

	idx := findTask(tasks, task.ID)
	if idx >= 0 {
		tasks[idx] = task
	} else {
		tasks = append(tasks, task)
	}

	state.Tasks = tasks

	return state
}

func dependencyCycleForTask(state State, taskID string) bool {
	taskID = strings.TrimSpace(taskID)
	if taskID == "" {
		return false
	}

	graph := make(map[string][]string, len(state.Tasks))
	for i := range state.Tasks {
		graph[state.Tasks[i].ID] = state.Tasks[i].Dependencies
	}

	for _, dependency := range graph[taskID] {
		if dependencyReachesTask(graph, dependency, taskID, make(map[string]struct{})) {
			return true
		}
	}

	return false
}

func dependencyReachesTask(graph map[string][]string, current, target string, visited map[string]struct{}) bool {
	if current == target {
		return true
	}

	if _, ok := visited[current]; ok {
		return false
	}

	visited[current] = struct{}{}
	for _, dependency := range graph[current] {
		if dependencyReachesTask(graph, dependency, target, visited) {
			return true
		}
	}

	return false
}

func newLease(owner, sessionID, runID string, now time.Time, duration time.Duration) *Lease {
	return &Lease{
		Owner:           strings.TrimSpace(owner),
		SessionID:       strings.TrimSpace(sessionID),
		RunID:           strings.TrimSpace(runID),
		AcquiredAt:      now,
		LastHeartbeatAt: now,
		ExpiresAt:       now.Add(leaseDuration(duration)),
	}
}

func leaseDuration(duration time.Duration) time.Duration {
	if duration <= 0 {
		return DefaultLeaseDuration
	}

	return duration
}

func leaseIsExpired(lease *Lease, now time.Time) bool {
	return lease == nil || !lease.ExpiresAt.After(now)
}

func sameLeaseScope(lease *Lease, sessionID, runID string) bool {
	if lease == nil {
		return false
	}

	return lease.SessionID == strings.TrimSpace(sessionID) && lease.RunID == strings.TrimSpace(runID)
}

func sameLeaseOwner(lease *Lease, owner, sessionID, runID string) bool {
	if lease == nil || lease.Owner != strings.TrimSpace(owner) {
		return false
	}

	return sameLeaseScope(lease, sessionID, runID)
}

func actorIdentity(actor, fallback string) string {
	if actor = strings.TrimSpace(actor); actor != "" {
		return actor
	}

	if fallback = strings.TrimSpace(fallback); fallback != "" {
		return fallback
	}

	return "system"
}

func historyActor(action HistoryAction, after, before Task) string {
	switch action {
	case HistoryCompleted, HistoryAssigned, HistoryHeartbeat:
		return actorIdentity(after.Agent, before.Agent)
	default:
		return actorIdentity(after.Agent, "")
	}
}

func cloneState(state State) State {
	state.Tasks = cloneTasks(state.Tasks)
	state.History = cloneHistory(state.History)

	return state
}

func cloneTasks(tasks []Task) []Task {
	if len(tasks) == 0 {
		return nil
	}

	cloned := make([]Task, len(tasks))
	for i := range tasks {
		cloned[i] = cloneTask(tasks[i])
	}

	return cloned
}

func cloneTask(task Task) Task {
	task.Metadata = copyMetadata(task.Metadata)
	task.Dependencies = append([]string(nil), task.Dependencies...)

	if task.CompletedAt != nil {
		completedAt := *task.CompletedAt
		task.CompletedAt = &completedAt
	}

	if task.Lease != nil {
		lease := *task.Lease
		task.Lease = &lease
	}

	return task
}

func cloneTaskPtr(task *Task) *Task {
	if task == nil {
		return nil
	}

	cloned := cloneTask(*task)

	return &cloned
}

func cloneHistory(history []HistoryEntry) []HistoryEntry {
	if len(history) == 0 {
		return nil
	}

	cloned := make([]HistoryEntry, len(history))
	for i := range history {
		cloned[i] = history[i]
		cloned[i].Before = cloneTaskPtr(history[i].Before)
		cloned[i].After = cloneTaskPtr(history[i].After)
	}

	return cloned
}

func ctxErr(ctx context.Context) error {
	if ctx == nil {
		return errors.New("tasklist: context is required")
	}

	select {
	case <-ctx.Done():
		return fmt.Errorf("tasklist: context: %w", ctx.Err())
	default:
		return nil
	}
}
