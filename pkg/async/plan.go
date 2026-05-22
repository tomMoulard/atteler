// Package async provides dependency-aware planning primitives for agent tasks.
package async

import (
	"context"
	"fmt"
	"time"
)

// Task describes a planning-only unit of work for an agent.
type Task struct {
	ID                    string   `json:"id"`
	Agent                 string   `json:"agent,omitempty"`
	Prompt                string   `json:"prompt,omitempty"`
	WorkspaceID           string   `json:"workspace_id,omitempty"`
	AllowedWriteScope     string   `json:"allowed_write_scope,omitempty"`
	Model                 string   `json:"model,omitempty"`
	Provider              string   `json:"provider,omitempty"`
	DependsOn             []string `json:"depends_on,omitempty"`
	EstimatedPromptTokens int      `json:"estimated_prompt_tokens,omitempty"`
	EstimatedCostMicros   int64    `json:"estimated_cost_micros,omitempty"`
}

// TaskRunner runs one task and returns caller-defined text for later display.
type TaskRunner func(context.Context, Task) (string, error)

// TaskResult records the outcome of one task execution.
//
//nolint:govet // Field order keeps execution metadata before task payload for CLI readability.
type TaskResult struct {
	StartedAt      time.Time     `json:"started_at,omitzero"`
	FinishedAt     time.Time     `json:"finished_at,omitzero"`
	Duration       time.Duration `json:"duration,omitempty"`
	Wave           int           `json:"wave"`
	Order          int           `json:"order"`
	Attempts       int           `json:"attempts,omitempty"`
	Task           Task          `json:"task"`
	Output         string        `json:"output,omitempty"`
	Stderr         string        `json:"stderr,omitempty"`
	Error          string        `json:"error,omitempty"`
	Status         string        `json:"status,omitempty"`
	LedgerPath     string        `json:"ledger_path,omitempty"`
	TranscriptPath string        `json:"transcript_path,omitempty"`
	Artifacts      []string      `json:"artifacts,omitempty"`
	ExitStatus     int           `json:"exit_status,omitempty"`
	Resumed        bool          `json:"resumed,omitempty"`
	Usage          Usage         `json:"usage,omitzero"`
}

// Spawn derives a child task from parent with an added dependency on parent.ID.
func (t Task) Spawn(id, agent, prompt string) Task {
	return Task{
		ID:        id,
		Agent:     agent,
		Prompt:    prompt,
		DependsOn: []string{t.ID},
	}
}

// Plan validates and batches dependency-aware agent tasks.
//
//nolint:govet // Field order keeps primary task list before lookup cache.
type Plan struct {
	tasks []Task
	byID  map[string]Task
}

// NewPlan returns a validated plan for tasks.
func NewPlan(tasks []Task) (*Plan, error) {
	plan := &Plan{
		tasks: make([]Task, len(tasks)),
		byID:  make(map[string]Task, len(tasks)),
	}

	for i := range tasks {
		task := tasks[i]
		if _, exists := plan.byID[task.ID]; exists {
			return nil, fmt.Errorf("duplicate task %q", task.ID)
		}

		plan.tasks[i] = cloneTask(task)
		plan.byID[task.ID] = plan.tasks[i]
	}

	for i := range plan.tasks {
		task := plan.tasks[i]
		for _, dep := range task.DependsOn {
			if _, exists := plan.byID[dep]; !exists {
				return nil, fmt.Errorf("task %q depends on missing task %q", task.ID, dep)
			}
		}
	}

	if err := plan.validateAcyclic(); err != nil {
		return nil, err
	}

	return plan, nil
}

// Spawn derives a child task from an existing parent task in the plan.
func (p *Plan) Spawn(parentID, id, agent, prompt string) (Task, error) {
	if p == nil {
		return Task{}, fmt.Errorf("parent task %q not found", parentID)
	}

	parent, ok := p.byID[parentID]
	if !ok {
		return Task{}, fmt.Errorf("parent task %q not found", parentID)
	}

	return parent.Spawn(id, agent, prompt), nil
}

// Tasks returns a defensive copy of the plan's tasks in input order.
func (p *Plan) Tasks() []Task {
	if p == nil {
		return nil
	}

	tasks := make([]Task, len(p.tasks))
	for i := range p.tasks {
		tasks[i] = cloneTask(p.tasks[i])
	}

	return tasks
}

// ReadyBatches returns dependency-ordered waves of tasks that may run in parallel.
func (p *Plan) ReadyBatches() [][]Task {
	if p == nil || len(p.tasks) == 0 {
		return nil
	}

	remaining := make(map[string]Task, len(p.tasks))
	for i := range p.tasks {
		task := p.tasks[i]
		remaining[task.ID] = task
	}

	completed := make(map[string]struct{}, len(p.tasks))
	batches := make([][]Task, 0)

	for len(remaining) > 0 {
		batch := make([]Task, 0)

		for i := range p.tasks {
			task := p.tasks[i]
			if _, ok := remaining[task.ID]; !ok {
				continue
			}

			if depsComplete(task, completed) {
				batch = append(batch, cloneTask(task))
			}
		}

		if len(batch) == 0 {
			return nil
		}

		for i := range batch {
			task := batch[i]
			delete(remaining, task.ID)
			completed[task.ID] = struct{}{}
		}

		batches = append(batches, batch)
	}

	return batches
}

// Run executes ready batches in order and runs tasks within each batch concurrently.
// It returns results in wave/order order. If a task fails, the current wave is
// allowed to finish, downstream waves are skipped, and the task error is returned.
func (p *Plan) Run(ctx context.Context, run TaskRunner) ([]TaskResult, error) {
	return p.RunWithOptions(ctx, run, RunOptions{})
}

func firstFailedResult(results []TaskResult) (TaskResult, bool) {
	for i := range results {
		if results[i].Error != "" && results[i].Status != StatusCanceled {
			return results[i], true
		}
	}

	for i := range results {
		if results[i].Error != "" {
			return results[i], true
		}
	}

	return TaskResult{}, false
}

func (p *Plan) validateAcyclic() error {
	const (
		unvisited = iota
		visiting
		visited
	)

	state := make(map[string]int, len(p.tasks))

	var visit func(Task) error

	visit = func(task Task) error {
		switch state[task.ID] {
		case visiting:
			return fmt.Errorf("cyclic dependency involving task %q", task.ID)
		case visited:
			return nil
		}

		state[task.ID] = visiting
		for _, dep := range task.DependsOn {
			if err := visit(p.byID[dep]); err != nil {
				return err
			}
		}

		state[task.ID] = visited

		return nil
	}

	for i := range p.tasks {
		task := p.tasks[i]
		if state[task.ID] == unvisited {
			if err := visit(task); err != nil {
				return err
			}
		}
	}

	return nil
}

func depsComplete(task Task, completed map[string]struct{}) bool {
	for _, dep := range task.DependsOn {
		if _, ok := completed[dep]; !ok {
			return false
		}
	}

	return true
}

func cloneTask(task Task) Task {
	if len(task.DependsOn) == 0 {
		return task
	}

	clone := task
	clone.DependsOn = append([]string(nil), task.DependsOn...)

	return clone
}
