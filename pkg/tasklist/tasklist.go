// Package tasklist provides a small JSON-backed TODO list for agents.
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
	// StatusPending means the task has not been started and is ready to claim.
	StatusPending Status = "pending"
	// StatusBlocked means the task has incomplete dependencies and cannot be assigned.
	StatusBlocked Status = "blocked"
	// StatusAssigned means the task has an owner and an active or expired lease.
	StatusAssigned Status = "assigned"
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
	ID            string            `json:"id"`
	Title         string            `json:"title"`
	Status        Status            `json:"status"`
	Agent         string            `json:"agent,omitempty"`
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
	Metadata      map[string]string
	Dependencies  []string
	Priority      int
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

// UpdateRequest contains mutable task fields. Empty values leave fields unchanged.
//
// Use ReplaceDependencies, SetPriority, ClearFailureReason, or ExpectedRevision
// when an empty value carries meaning.
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
	Priority            int
	SetPriority         bool
	ReplaceDependencies bool
	ClearFailureReason  bool
	ExpectedRevision    int64
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
		agent        = strings.TrimSpace(req.Agent)
		lease        *Lease
		status       = StatusPending
		attemptCount int
	)

	if len(unmetDependencies(state, dependencies)) > 0 {
		if agent != "" {
			return Task{}, fmt.Errorf("%w: dependencies are not completed", ErrTaskBlocked)
		}

		status = StatusBlocked
	} else if agent != "" {
		status = StatusAssigned
		attemptCount = 1
		lease = newLease(agent, req.SessionID, req.RunID, now, req.LeaseDuration)
	}

	task := Task{
		ID:           id,
		Title:        title,
		Status:       status,
		Agent:        agent,
		Lease:        lease,
		CreatedAt:    now,
		UpdatedAt:    now,
		Metadata:     copyMetadata(req.Metadata),
		Dependencies: dependencies,
		Priority:     req.Priority,
		AttemptCount: attemptCount,
		ReviewStatus: req.ReviewStatus,
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

		if task.Status != StatusAssigned || task.Lease == nil {
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
		task.UpdatedAt = now

		return HistoryHeartbeat, strings.TrimSpace(req.Message), nil
	})
}

// Complete checks off a task and records the completing agent when provided.
func (s *Store) Complete(ctx context.Context, id, agent string) (Task, error) {
	return s.updateTask(ctx, id, 0, agent, func(state *State, task *Task, now time.Time) (HistoryAction, string, error) {
		if task.Status == StatusCompleted {
			return "", "", ErrTaskCompleted
		}

		if len(unmetDependencies(*state, task.Dependencies)) > 0 {
			return "", "", fmt.Errorf("%w: dependencies are not completed", ErrTaskBlocked)
		}

		agent = strings.TrimSpace(agent)
		if agent != "" {
			task.Agent = agent
		}

		task.Status = StatusCompleted
		task.Lease = nil
		task.UpdatedAt = now
		task.CompletedAt = &now

		return HistoryCompleted, "", nil
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

func updateRequestsAssignment(req UpdateRequest) bool {
	return strings.TrimSpace(req.Agent) != "" || req.Status == StatusAssigned
}

func applyUpdateOwner(task *Task, req UpdateRequest, now time.Time) error {
	agent := strings.TrimSpace(req.Agent)
	if req.Status != "" {
		if err := validateIncompleteStatus(req.Status); err != nil {
			return err
		}

		task.Status = req.Status
		if req.Status != StatusAssigned {
			clearTaskOwner(task)

			return nil
		}
	}

	if agent == "" {
		return nil
	}

	task.Agent = agent
	task.Status = StatusAssigned
	task.AttemptCount++
	task.Lease = newLease(agent, "", "", now, 0)

	return nil
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
	if len(unmetDependencies(*state, task.Dependencies)) > 0 {
		task.Status = StatusBlocked
		clearTaskOwner(task)

		return
	}

	if task.Status == StatusBlocked {
		task.Status = StatusPending
		clearTaskOwner(task)
	}
}

func ensureAssignedLease(task *Task, now time.Time) error {
	if task.Status != StatusAssigned || task.Lease != nil {
		return nil
	}

	if strings.TrimSpace(task.Agent) == "" {
		return errors.New("tasklist: agent is required for assigned tasks")
	}

	task.AttemptCount++
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
	blocked := len(unmetDependencies(state, task.Dependencies)) > 0

	switch {
	case blocked && task.Status != StatusBlocked:
		task.Status = StatusBlocked
		clearTaskOwner(task)

		changed = true
	case !blocked && task.Status == StatusBlocked:
		task.Status = StatusPending
		clearTaskOwner(task)

		changed = true
	case task.Status != StatusAssigned && hasTaskOwner(task):
		clearTaskOwner(task)

		changed = true
	}

	return change, changed
}

func repairAssignedLease(task *Task, now time.Time, change *reconcileChange) bool {
	if task.Status != StatusAssigned {
		return false
	}

	if leaseIsExpired(task.Lease, now) {
		clearTaskOwner(task)
		task.Status = StatusPending
		change.expiredLease = true

		return true
	}

	if task.Agent != task.Lease.Owner {
		task.Agent = task.Lease.Owner

		return true
	}

	return false
}

func clearTaskOwner(task *Task) {
	task.Agent = ""
	task.Lease = nil
}

func hasTaskOwner(task *Task) bool {
	return strings.TrimSpace(task.Agent) != "" || task.Lease != nil
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

		if len(unmetDependencies(*state, task.Dependencies)) > 0 {
			return "", "", fmt.Errorf("%w: dependencies are not completed", ErrTaskBlocked)
		}

		if !replaceLiveLease && task.Status == StatusAssigned && !leaseIsExpired(task.Lease, now) && !sameLeaseOwner(task.Lease, agent, req.SessionID, req.RunID) {
			return "", "", ErrTaskLeased
		}

		if shouldCountAssignmentAttempt(task, agent, req.SessionID, req.RunID, now) {
			task.AttemptCount++
		}

		task.Agent = agent
		task.Status = StatusAssigned
		task.Lease = newLease(agent, req.SessionID, req.RunID, now, req.LeaseDuration)
		task.UpdatedAt = now

		return HistoryAssigned, strings.TrimSpace(req.Message), nil
	})
}

func shouldCountAssignmentAttempt(task *Task, agent, sessionID, runID string, now time.Time) bool {
	return task.Status != StatusAssigned || leaseIsExpired(task.Lease, now) || !sameLeaseOwner(task.Lease, agent, sessionID, runID)
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
		if strings.TrimSpace(state.History[i].TaskID) == "" {
			return errors.New("tasklist: history task id is required")
		}

		if state.History[i].Action == "" {
			return errors.New("tasklist: history action is required")
		}
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
	if task.Status == StatusCompleted && task.Lease != nil {
		return errors.New("tasklist: lease is not valid for completed tasks")
	}

	if task.Lease == nil {
		return nil
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

func validateStatus(status Status) error {
	switch status {
	case StatusPending, StatusBlocked, StatusAssigned, StatusCompleted:
		return nil
	default:
		return fmt.Errorf("tasklist: invalid status %q", status)
	}
}

func validateIncompleteStatus(status Status) error {
	if status == StatusCompleted {
		return errors.New("tasklist: use Complete to complete a task")
	}

	return validateStatus(status)
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
