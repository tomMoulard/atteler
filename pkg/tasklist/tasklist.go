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
	// StatusPending means the task has not been started.
	StatusPending Status = "pending"
	// StatusAssigned means the task has an owner but is not complete.
	StatusAssigned Status = "assigned"
	// StatusCompleted means the task has been checked off.
	StatusCompleted Status = "completed"
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
	// HistoryCompleted means a task was checked off.
	HistoryCompleted HistoryAction = "completed"
)

var (
	// ErrTaskNotFound is returned when a task ID does not exist in the store.
	ErrTaskNotFound = errors.New("tasklist: task not found")
	// ErrTaskCompleted is returned when an operation cannot modify a completed task.
	ErrTaskCompleted = errors.New("tasklist: task is completed")
)

// Task is a durable TODO item.
type Task struct {
	CreatedAt   time.Time         `json:"created_at"`
	UpdatedAt   time.Time         `json:"updated_at"`
	CompletedAt *time.Time        `json:"completed_at,omitempty"`
	Metadata    map[string]string `json:"metadata,omitempty"`
	ID          string            `json:"id"`
	Title       string            `json:"title"`
	Status      Status            `json:"status"`
	Agent       string            `json:"agent,omitempty"`
}

// HistoryEntry records a task mutation.
//
//nolint:govet // Field order keeps related history data readable in JSON.
type HistoryEntry struct {
	At      time.Time     `json:"at"`
	Seq     int64         `json:"seq"`
	Action  HistoryAction `json:"action"`
	TaskID  string        `json:"task_id"`
	Agent   string        `json:"agent,omitempty"`
	Message string        `json:"message,omitempty"`
}

// AddRequest contains fields for a new task.
//
//nolint:govet // Field order follows the user-facing add request shape.
type AddRequest struct {
	ID       string
	Title    string
	Agent    string
	Metadata map[string]string
}

// UpdateRequest contains mutable task fields. Empty values leave fields unchanged.
type UpdateRequest struct {
	Title    string
	Agent    string
	Status   Status
	Metadata map[string]string
	Message  string
}

// State is the JSON document persisted by Store.
type State struct {
	Tasks   []Task         `json:"tasks"`
	History []HistoryEntry `json:"history"`
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

	state, err := s.loadLocked(ctx)
	if err != nil {
		return Task{}, err
	}

	id := strings.TrimSpace(req.ID)
	if id == "" {
		id = newID()
	}

	if findTask(state.Tasks, id) >= 0 {
		return Task{}, fmt.Errorf("tasklist: duplicate task id %q", id)
	}

	now := s.now().UTC()
	task := Task{
		ID:        id,
		Title:     title,
		Status:    StatusPending,
		Agent:     strings.TrimSpace(req.Agent),
		CreatedAt: now,
		UpdatedAt: now,
		Metadata:  copyMetadata(req.Metadata),
	}

	if task.Agent != "" {
		task.Status = StatusAssigned
	}

	state.Tasks = append(state.Tasks, task)
	state.History = appendHistory(state.History, now, HistoryAdded, task.ID, task.Agent, "")

	if err := s.saveLocked(ctx, state); err != nil {
		return Task{}, err
	}

	return cloneTask(task), nil
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

	return s.loadLocked(ctx)
}

// History returns history entries sorted by sequence.
func (s *Store) History(ctx context.Context) ([]HistoryEntry, error) {
	state, err := s.Load(ctx)
	if err != nil {
		return nil, err
	}

	return append([]HistoryEntry(nil), state.History...), nil
}

// Assign records an agent owner for an incomplete task.
func (s *Store) Assign(ctx context.Context, id, agent string) (Task, error) {
	agent = strings.TrimSpace(agent)
	if agent == "" {
		return Task{}, errors.New("tasklist: agent is required")
	}

	return s.updateTask(ctx, id, func(task *Task, now time.Time) (HistoryAction, string, error) {
		if task.Status == StatusCompleted {
			return "", "", ErrTaskCompleted
		}

		task.Agent = agent
		task.Status = StatusAssigned
		task.UpdatedAt = now

		return HistoryAssigned, "", nil
	})
}

// Complete checks off a task and records the completing agent when provided.
func (s *Store) Complete(ctx context.Context, id, agent string) (Task, error) {
	return s.updateTask(ctx, id, func(task *Task, now time.Time) (HistoryAction, string, error) {
		if task.Status == StatusCompleted {
			return "", "", ErrTaskCompleted
		}

		agent = strings.TrimSpace(agent)
		if agent != "" {
			task.Agent = agent
		}

		task.Status = StatusCompleted
		task.UpdatedAt = now
		task.CompletedAt = &now

		return HistoryCompleted, "", nil
	})
}

// Update changes mutable fields on an incomplete task.
func (s *Store) Update(ctx context.Context, id string, req UpdateRequest) (Task, error) {
	return s.updateTask(ctx, id, func(task *Task, now time.Time) (HistoryAction, string, error) {
		if task.Status == StatusCompleted {
			return "", "", ErrTaskCompleted
		}

		if title := strings.TrimSpace(req.Title); title != "" {
			task.Title = title
		}

		if agent := strings.TrimSpace(req.Agent); agent != "" {
			task.Agent = agent
			if req.Status == "" {
				task.Status = StatusAssigned
			}
		}

		if req.Status != "" {
			if err := validateIncompleteStatus(req.Status); err != nil {
				return "", "", err
			}

			task.Status = req.Status
		}

		if req.Metadata != nil {
			task.Metadata = copyMetadata(req.Metadata)
		}

		task.UpdatedAt = now

		return HistoryUpdated, strings.TrimSpace(req.Message), nil
	})
}

func (s *Store) updateTask(ctx context.Context, id string, mutate func(*Task, time.Time) (HistoryAction, string, error)) (Task, error) {
	if err := ctxErr(ctx); err != nil {
		return Task{}, err
	}

	id = strings.TrimSpace(id)
	if id == "" {
		return Task{}, errors.New("tasklist: id is required")
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	state, err := s.loadLocked(ctx)
	if err != nil {
		return Task{}, err
	}

	idx := findTask(state.Tasks, id)
	if idx < 0 {
		return Task{}, ErrTaskNotFound
	}

	now := s.now().UTC()

	action, message, err := mutate(&state.Tasks[idx], now)
	if err != nil {
		return Task{}, err
	}

	state.History = appendHistory(state.History, now, action, state.Tasks[idx].ID, state.Tasks[idx].Agent, message)
	if err := s.saveLocked(ctx, state); err != nil {
		return Task{}, err
	}

	return cloneTask(state.Tasks[idx]), nil
}

func (s *Store) loadLocked(ctx context.Context) (State, error) {
	if s.path == "" {
		return State{}, errors.New("tasklist: path is required")
	}

	if err := ctxErr(ctx); err != nil {
		return State{}, err
	}

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

func (s *Store) saveLocked(ctx context.Context, state State) error {
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

	if err := os.Rename(tmpPath, s.path); err != nil {
		return fmt.Errorf("tasklist: replace %s: %w", s.path, err)
	}

	return nil
}

func appendHistory(history []HistoryEntry, at time.Time, action HistoryAction, taskID, agent, message string) []HistoryEntry {
	return append(history, HistoryEntry{
		Seq:     nextSeq(history),
		At:      at,
		Action:  action,
		TaskID:  taskID,
		Agent:   agent,
		Message: message,
	})
}

func nextSeq(history []HistoryEntry) int64 {
	var seq int64
	for _, entry := range history {
		if entry.Seq > seq {
			seq = entry.Seq
		}
	}

	return seq + 1
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
	if strings.TrimSpace(task.ID) == "" {
		return errors.New("tasklist: id is required")
	}

	if strings.TrimSpace(task.Title) == "" {
		return errors.New("tasklist: title is required")
	}

	if err := validateStatus(task.Status); err != nil {
		return err
	}

	if task.CreatedAt.IsZero() {
		return errors.New("tasklist: created_at is required")
	}

	if task.UpdatedAt.IsZero() {
		return errors.New("tasklist: updated_at is required")
	}

	if task.Status == StatusCompleted && task.CompletedAt == nil {
		return errors.New("tasklist: completed_at is required for completed tasks")
	}

	if task.Status != StatusCompleted && task.CompletedAt != nil {
		return errors.New("tasklist: completed_at is only valid for completed tasks")
	}

	return nil
}

func validateStatus(status Status) error {
	switch status {
	case StatusPending, StatusAssigned, StatusCompleted:
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
	if task.CompletedAt != nil {
		completedAt := *task.CompletedAt
		task.CompletedAt = &completedAt
	}

	return task
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
