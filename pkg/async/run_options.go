//nolint:wsl_v5,modernize // Ledger orchestration keeps related cancellation and persistence steps together.
package async

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

const (
	// StatusSucceeded means a task completed without error.
	StatusSucceeded = "succeeded"
	// StatusFailed means a task returned an error.
	StatusFailed = "failed"
	// StatusDenied means a task was denied before execution by admission policy.
	StatusDenied = "denied"
	// StatusCanceled means a task was canceled before completion.
	StatusCanceled = "canceled"
	// StatusTimedOut means a task exceeded its per-task timeout.
	StatusTimedOut = "timed_out"
	// StatusBudgetExhausted means a task exceeded a budget before or during execution.
	StatusBudgetExhausted = "budget_exhausted"
	// StatusSkipped means a task was not rerun because a previous success was resumed.
	StatusSkipped = "skipped"
	// StatusRunning is written to the ledger while an attempt is in progress.
	StatusRunning = "running"

	defaultMaxConcurrency = 4
	defaultMaxAttempts    = 1
)

// RetryPolicy controls retry behavior for failed task attempts.
type RetryPolicy struct {
	Backoff     time.Duration `json:"backoff,omitempty"`
	MaxAttempts int           `json:"max_attempts,omitempty"`
}

// Budget describes aggregate async-run ceilings. Token and cost limits use
// caller-provided or estimated prompt costs before execution and runner-reported
// usage after completion when that usage is available.
type Budget struct {
	MaxOutputBytes  int64 `json:"max_output_bytes,omitempty"`
	MaxCostMicros   int64 `json:"max_cost_micros,omitempty"`
	MaxPromptTokens int   `json:"max_prompt_tokens,omitempty"`
}

// Usage records budget consumption attributed to a task attempt.
type Usage struct {
	PromptTokens        int   `json:"prompt_tokens,omitempty"`
	OutputBytes         int64 `json:"output_bytes,omitempty"`
	EstimatedCostMicros int64 `json:"estimated_cost_micros,omitempty"`
}

// TaskRunOutput captures process-level output returned by a DetailedTaskRunner.
type TaskRunOutput struct {
	Stdout              string   `json:"stdout,omitempty"`
	Stderr              string   `json:"stderr,omitempty"`
	Artifacts           []string `json:"artifacts,omitempty"`
	ExitStatus          int      `json:"exit_status,omitempty"`
	PromptTokens        int      `json:"prompt_tokens,omitempty"`
	EstimatedCostMicros int64    `json:"estimated_cost_micros,omitempty"`
	BudgetExhausted     bool     `json:"budget_exhausted,omitempty"`
}

// DetailedTaskRunner runs one task and returns auditable output metadata.
type DetailedTaskRunner func(context.Context, Task) (TaskRunOutput, error)

// RunOptions controls concurrency, cancellation, retry, budget, and recovery behavior.
//
//nolint:govet // Field order groups user-facing options for CLI/ledger readability.
type RunOptions struct {
	Timeout           time.Duration `json:"timeout,omitempty"`
	RetryPolicy       RetryPolicy   `json:"retry_policy,omitempty"`
	Budget            Budget        `json:"budget,omitempty"`
	LedgerPath        string        `json:"ledger_path,omitempty"`
	WorkspaceID       string        `json:"workspace_id,omitempty"`
	AllowedWriteScope string        `json:"allowed_write_scope,omitempty"`
	Model             string        `json:"model,omitempty"`
	Provider          string        `json:"provider,omitempty"`
	Autonomy          string        `json:"autonomy,omitempty"`
	MaxConcurrency    int           `json:"max_concurrency,omitempty"`
	CancelOnFailure   bool          `json:"cancel_on_failure,omitempty"`
	Resume            bool          `json:"resume,omitempty"`
}

// TaskAttempt captures one durable task attempt.
//
//nolint:govet // Field order keeps lifecycle metadata before task payload for ledger readability.
type TaskAttempt struct {
	StartedAt      time.Time     `json:"started_at"`
	FinishedAt     time.Time     `json:"finished_at,omitempty"`
	Duration       time.Duration `json:"duration,omitempty"`
	Wave           int           `json:"wave"`
	Order          int           `json:"order"`
	Attempt        int           `json:"attempt"`
	Task           Task          `json:"task"`
	Output         string        `json:"output,omitempty"`
	Stderr         string        `json:"stderr,omitempty"`
	Error          string        `json:"error,omitempty"`
	Status         string        `json:"status"`
	AdmissionID    string        `json:"admission_id,omitempty"`
	StopID         string        `json:"stop_id,omitempty"`
	TranscriptPath string        `json:"transcript_path,omitempty"`
	Artifacts      []string      `json:"artifacts,omitempty"`
	ExitStatus     int           `json:"exit_status,omitempty"`
	Usage          Usage         `json:"usage,omitempty"`
}

// Admission captures the durable decision boundary before an async child task
// is allowed to run or denied by resource/cancellation policy.
//
//nolint:govet // Field order keeps identity and policy before the decision.
type Admission struct {
	RecordedAt        time.Time     `json:"recorded_at"`
	AdmissionID       string        `json:"admission_id"`
	ChildID           string        `json:"child_id"`
	ParentRunID       string        `json:"parent_run_id"`
	Wave              int           `json:"wave"`
	Order             int           `json:"order"`
	WorkspaceID       string        `json:"workspace_id,omitempty"`
	AllowedWriteScope string        `json:"allowed_write_scope,omitempty"`
	Model             string        `json:"model,omitempty"`
	Provider          string        `json:"provider,omitempty"`
	Autonomy          string        `json:"autonomy,omitempty"`
	Timeout           time.Duration `json:"timeout,omitempty"`
	Budget            Budget        `json:"budget,omitempty"`
	RetryPolicy       RetryPolicy   `json:"retry_policy,omitempty"`
	Attempt           int           `json:"attempt,omitempty"`
	Admitted          bool          `json:"admitted"`
	DenyReason        string        `json:"deny_reason,omitempty"`
}

// StopReceipt captures the durable halt boundary for an admitted async task.
//
//nolint:govet // Field order keeps identity before terminal reason metadata.
type StopReceipt struct {
	RecordedAt  time.Time `json:"recorded_at"`
	StopID      string    `json:"stop_id"`
	AdmissionID string    `json:"admission_id"`
	ChildID     string    `json:"child_id"`
	ParentRunID string    `json:"parent_run_id"`
	Wave        int       `json:"wave"`
	Order       int       `json:"order"`
	Attempt     int       `json:"attempt"`
	Status      string    `json:"status"`
	Reason      string    `json:"reason,omitempty"`
}

// Ledger is an auditable JSON document for a dependency-aware async run.
//
//nolint:govet // Field order keeps lifecycle metadata before detailed records.
type Ledger struct {
	StartedAt    time.Time     `json:"started_at"`
	UpdatedAt    time.Time     `json:"updated_at"`
	RunID        string        `json:"run_id"`
	Options      RunOptions    `json:"options"`
	Tasks        []Task        `json:"tasks"`
	Admissions   []Admission   `json:"admissions,omitempty"`
	StopReceipts []StopReceipt `json:"stop_receipts,omitempty"`
	Attempts     []TaskAttempt `json:"attempts,omitempty"`
	Results      []TaskResult  `json:"results,omitempty"`
}

// RunWithOptions executes ready batches in order under explicit budgets,
// retries, cancellation behavior, and an optional durable ledger. It returns
// results in wave/order order. If a task fails, downstream waves are skipped;
// ledger-backed runs also persist those never-started tasks as denied
// admissions/results so resume has a durable boundary.
func (p *Plan) RunWithOptions(ctx context.Context, run TaskRunner, opts RunOptions) ([]TaskResult, error) {
	if run == nil {
		return nil, errors.New("task runner is nil")
	}

	return p.RunDetailedWithOptions(ctx, func(ctx context.Context, task Task) (TaskRunOutput, error) {
		output, err := run(ctx, task)

		return TaskRunOutput{Stdout: output}, err
	}, opts)
}

// RunDetailedWithOptions executes ready batches with a runner that returns
// stdout/stderr, exit status, artifacts, and usage metadata for the ledger.
// It uses the same downstream-denial persistence semantics as RunWithOptions.
//
//nolint:gocognit // Wave orchestration keeps resume, cancellation, failure, and ledger ordering visible.
func (p *Plan) RunDetailedWithOptions(ctx context.Context, run DetailedTaskRunner, opts RunOptions) ([]TaskResult, error) {
	if run == nil {
		return nil, errors.New("task runner is nil")
	}

	if p == nil {
		return nil, nil
	}

	if ctx == nil {
		return nil, errors.New("context is nil")
	}

	opts = normalizeRunOptions(opts, len(p.tasks))
	batches := p.ReadyBatches()
	ledgerTasks := applyTaskDefaults(p.Tasks(), opts)
	ledger, err := openRunLedger(opts, ledgerTasks)
	if err != nil {
		return nil, err
	}

	budget := newRunBudgetTracker(opts.Budget, ledger)
	stableResumed := make(map[string]struct{}, len(p.tasks))
	results := make([]TaskResult, 0, len(p.tasks))

	for wave, batch := range batches {
		if err := ctx.Err(); err != nil {
			cause := runContextCauseOrErr(ctx, err)
			if ledger != nil {
				results = append(results, recordRemainingWavesCanceledBeforeStart(wave, batches, opts, ledger, cause)...)
			}

			return results, errors.Join(fmt.Errorf("context canceled before wave %d: %w", wave, cause), ledger.ledgerError())
		}

		batch = applyTaskDefaults(batch, opts)
		waveResults := runBatchWithOptions(ctx, wave, batch, run, opts, budget, ledger, stableResumed)
		results = append(results, waveResults...)
		for i := range waveResults {
			if waveResults[i].Resumed {
				stableResumed[waveResults[i].Task.ID] = struct{}{}
			}
		}

		if err := ctx.Err(); err != nil {
			cause := runContextCauseOrErr(ctx, err)
			if ledger != nil {
				results = append(results, recordRemainingWavesCanceledBeforeStart(wave+1, batches, opts, ledger, cause)...)
			}

			return results, errors.Join(fmt.Errorf("context canceled after wave %d: %w", wave, cause), ledger.ledgerError())
		}

		if result, ok := firstFailedResult(waveResults); ok {
			if ledger != nil {
				results = append(results, recordRemainingWavesDeniedAfterFailure(wave+1, batches, opts, ledger, result)...)
			}

			return results, errors.Join(taskRunError(result), ledger.ledgerError())
		}
	}

	return results, ledger.ledgerError()
}

func recordRemainingWavesCanceledBeforeStart(
	startWave int,
	batches [][]Task,
	opts RunOptions,
	ledger *runLedgerStore,
	err error,
) []TaskResult {
	results := make([]TaskResult, 0)
	for wave := startWave; wave < len(batches); wave++ {
		batch := applyTaskDefaults(batches[wave], opts)
		waveResults := make([]TaskResult, len(batch))
		for i := range batch {
			recordTaskCanceledBeforeStart(wave, i, batch[i], waveResults, ledger, opts, err)
		}

		results = append(results, waveResults...)
	}

	return results
}

func recordRemainingWavesDeniedAfterFailure(
	startWave int,
	batches [][]Task,
	opts RunOptions,
	ledger *runLedgerStore,
	failed TaskResult,
) []TaskResult {
	if startWave >= len(batches) {
		return nil
	}

	err := fmt.Errorf("upstream wave halted after task %q %s: %s", failed.Task.ID, taskRunErrorStatus(failed.Status), failed.Error)
	results := make([]TaskResult, 0)
	for wave := startWave; wave < len(batches); wave++ {
		batch := applyTaskDefaults(batches[wave], opts)
		waveResults := make([]TaskResult, len(batch))
		for i := range batch {
			recordTaskDeniedBeforeStart(wave, i, batch[i], waveResults, ledger, opts, err)
		}

		results = append(results, waveResults...)
	}

	return results
}

func taskRunError(result TaskResult) error {
	return fmt.Errorf("task %q %s: %s", result.Task.ID, taskRunErrorStatus(result.Status), result.Error)
}

func taskRunErrorStatus(status string) string {
	switch status {
	case StatusBudgetExhausted:
		return "budget exhausted"
	case StatusCanceled:
		return "canceled"
	case StatusDenied:
		return "denied"
	case StatusTimedOut:
		return "timed out"
	default:
		return "failed"
	}
}

func runBatchWithOptions(
	ctx context.Context,
	wave int,
	batch []Task,
	run DetailedTaskRunner,
	opts RunOptions,
	budget *runBudgetTracker,
	ledger *runLedgerStore,
	stableResumed map[string]struct{},
) []TaskResult {
	results := make([]TaskResult, len(batch))
	skipped := seedResumedTaskResults(results, wave, batch, ledger, opts, stableResumed)
	ctx, cancel := context.WithCancelCause(ctx)
	defer cancel(nil)

	jobs := make(chan int)
	var wg sync.WaitGroup

	for range opts.MaxConcurrency {
		wg.Add(1)
		go func() {
			defer wg.Done()

			for {
				index, ok := nextTaskIndex(ctx, jobs)
				if !ok {
					return
				}

				result := runTaskWithRetries(ctx, wave, index, batch[index], run, opts, budget, ledger)
				results[index] = result
				if result.Error != "" && (opts.CancelOnFailure || result.Status == StatusBudgetExhausted || result.Status == StatusDenied) {
					cancel(taskSiblingCancelCause(result))
				}
			}
		}()
	}

	submitTaskJobs(ctx, wave, batch, skipped, jobs, results, ledger, opts)

	close(jobs)
	wg.Wait()

	return results
}

func submitTaskJobs(
	ctx context.Context,
	wave int,
	batch []Task,
	skipped []bool,
	jobs chan<- int,
	results []TaskResult,
	ledger *runLedgerStore,
	opts RunOptions,
) {
	for i := range batch {
		if skipped[i] {
			continue
		}

		if err := ctx.Err(); err != nil {
			recordTaskCanceledBeforeStart(wave, i, batch[i], results, ledger, opts, runContextCauseOrErr(ctx, err))
			continue
		}

		select {
		case <-ctx.Done():
			recordTaskCanceledBeforeStart(wave, i, batch[i], results, ledger, opts, runContextCauseOrErr(ctx, ctx.Err()))
		case jobs <- i:
		}
	}
}

func recordTaskCanceledBeforeStart(
	wave int,
	order int,
	task Task,
	results []TaskResult,
	ledger *runLedgerStore,
	opts RunOptions,
	err error,
) {
	admission := admissionForTask(wave, order, task, 0, false, err, ledger, opts)
	ignoreRunLedgerError(ledger.recordAdmission(admission))
	results[order] = taskResultFromError(wave, order, task, ledger, StatusCanceled, 0, err)
	results[order].AdmissionID = admission.AdmissionID
	ignoreRunLedgerError(ledger.recordResult(results[order]))
}

func recordTaskDeniedBeforeStart(
	wave int,
	order int,
	task Task,
	results []TaskResult,
	ledger *runLedgerStore,
	opts RunOptions,
	err error,
) {
	admission := admissionForTask(wave, order, task, 0, false, err, ledger, opts)
	ignoreRunLedgerError(ledger.recordAdmission(admission))
	results[order] = taskResultFromError(wave, order, task, ledger, StatusDenied, 0, err)
	results[order].AdmissionID = admission.AdmissionID
	ignoreRunLedgerError(ledger.recordResult(results[order]))
}

func nextTaskIndex(ctx context.Context, jobs <-chan int) (int, bool) {
	select {
	case <-ctx.Done():
		return 0, false
	default:
	}

	select {
	case <-ctx.Done():
		return 0, false
	case index, ok := <-jobs:
		return index, ok
	}
}

//nolint:gocognit // Retry, admission, budget, timeout, and ledger ordering must stay explicit.
func runTaskWithRetries(
	ctx context.Context,
	wave int,
	order int,
	task Task,
	run DetailedTaskRunner,
	opts RunOptions,
	budget *runBudgetTracker,
	ledger *runLedgerStore,
) TaskResult {
	maxAttempts := opts.RetryPolicy.MaxAttempts
	var last TaskResult

	for attempt := 1; attempt <= maxAttempts; attempt++ {
		if err := ctx.Err(); err != nil {
			cause := runContextCauseOrErr(ctx, err)
			admission := admissionForTask(wave, order, task, attempt, false, cause, ledger, opts)
			ignoreRunLedgerError(ledger.recordAdmission(admission))
			last = taskResultFromError(wave, order, task, ledger, StatusCanceled, attempt-1, cause)
			last.AdmissionID = admission.AdmissionID
			ignoreRunLedgerError(ledger.recordResult(last))

			return last
		}

		if err := validateRunAllowedWriteScope(task.AllowedWriteScope, opts.AllowedWriteScope); err != nil {
			admission := admissionForTask(wave, order, task, attempt, false, err, ledger, opts)
			ignoreRunLedgerError(ledger.recordAdmission(admission))
			last = taskResultFromError(wave, order, task, ledger, StatusDenied, attempt-1, err)
			last.AdmissionID = admission.AdmissionID
			ignoreRunLedgerError(ledger.recordResult(last))

			return last
		}

		usage, err := budget.reserve(task)
		if err != nil {
			admission := admissionForTask(wave, order, task, attempt, false, err, ledger, opts)
			ignoreRunLedgerError(ledger.recordAdmission(admission))
			last = taskResultFromError(wave, order, task, ledger, StatusBudgetExhausted, attempt-1, err)
			last.AdmissionID = admission.AdmissionID
			ignoreRunLedgerError(ledger.recordResult(last))

			return last
		}

		admission := admissionForTask(wave, order, task, attempt, true, nil, ledger, opts)
		admitErr := ledger.recordAdmission(admission)
		if admitErr != nil {
			admissionErr := fmt.Errorf("async: task %q admission failed: %w", task.ID, admitErr)
			last = taskResultFromError(wave, order, task, ledger, StatusFailed, attempt-1, admissionErr)
			last.AdmissionID = admission.AdmissionID
			ignoreRunLedgerError(ledger.recordResult(last))

			return last
		}

		attemptCtx := ctx
		var cancel context.CancelFunc = func() {}
		if opts.Timeout > 0 {
			attemptCtx, cancel = context.WithTimeout(ctx, opts.Timeout)
		}
		attemptCtx = withRunRemainingOutputBudget(attemptCtx, budget)

		startedClock := time.Now()
		started := startedClock.UTC()
		attemptIndex := ledger.startAttempt(TaskAttempt{
			StartedAt:   started,
			Wave:        wave,
			Order:       order,
			Attempt:     attempt,
			Task:        task,
			Status:      StatusRunning,
			Usage:       usage,
			AdmissionID: admission.AdmissionID,
		})

		out, err := run(attemptCtx, task)
		finishedClock := time.Now()
		finished := finishedClock.UTC()
		duration := finishedClock.Sub(startedClock)
		attemptErr := attemptCtx.Err()
		parentErr := ctx.Err()
		timeoutExceeded := opts.Timeout > 0 && duration >= opts.Timeout
		cancel()

		status := taskStatusForAttempt(attemptErr, parentErr, err, timeoutExceeded)
		err = taskErrorForStatus(attemptCtx, ctx, status, err)
		reserved := usage
		usage = mergeTaskUsage(usage, out)
		if budgetErr := budget.recordActual(reserved, usage); budgetErr != nil {
			status = StatusBudgetExhausted
			err = errors.Join(err, budgetErr)
		}
		status, err = taskStatusAfterRunnerBudgetExhaustion(status, err, out)
		stopID := ""
		if taskStatusNeedsStopReceipt(status) {
			receipt := stopReceiptForTask(wave, order, task, attempt, admission.AdmissionID, status, err, ledger)
			stopID = receipt.StopID
			ignoreRunLedgerError(ledger.recordStopReceipt(receipt))
		}
		transcriptPath := ledger.writeTranscript(task.ID, attempt, out, err)
		attemptRecord := TaskAttempt{
			StartedAt:      started,
			FinishedAt:     finished,
			Duration:       duration,
			Wave:           wave,
			Order:          order,
			Attempt:        attempt,
			Task:           task,
			Output:         out.Stdout,
			Stderr:         out.Stderr,
			Status:         status,
			AdmissionID:    admission.AdmissionID,
			StopID:         stopID,
			TranscriptPath: transcriptPath,
			Artifacts:      append([]string(nil), out.Artifacts...),
			ExitStatus:     out.ExitStatus,
			Usage:          usage,
		}
		if err != nil {
			attemptRecord.Error = err.Error()
		}
		ignoreRunLedgerError(ledger.finishAttempt(attemptIndex, attemptRecord))

		last = TaskResult{
			StartedAt:      started,
			FinishedAt:     finished,
			Duration:       duration,
			Wave:           wave,
			Order:          order,
			Attempts:       attempt,
			Task:           task,
			Output:         out.Stdout,
			Stderr:         out.Stderr,
			Status:         status,
			LedgerPath:     ledger.path(),
			AdmissionID:    admission.AdmissionID,
			StopID:         stopID,
			TranscriptPath: transcriptPath,
			Artifacts:      append([]string(nil), out.Artifacts...),
			ExitStatus:     out.ExitStatus,
			Usage:          usage,
		}
		if err != nil {
			last.Error = err.Error()
		} else {
			ignoreRunLedgerError(ledger.recordResult(last))

			return last
		}

		if !retryTaskStatus(status, attempt, maxAttempts) {
			break
		}

		if err := waitTaskRetryBackoff(ctx, opts.RetryPolicy.Backoff); err != nil {
			admission := admissionForTask(wave, order, task, attempt+1, false, err, ledger, opts)
			ignoreRunLedgerError(ledger.recordAdmission(admission))
			last = taskResultFromError(wave, order, task, ledger, StatusCanceled, attempt, err)
			last.AdmissionID = admission.AdmissionID
			break
		}
	}

	ignoreRunLedgerError(ledger.recordResult(last))

	return last
}

func taskStatusAfterRunnerBudgetExhaustion(status string, err error, out TaskRunOutput) (string, error) {
	if !out.BudgetExhausted {
		return status, err
	}

	if err == nil {
		err = errors.New("async: budget exhausted by runner")
	}

	return StatusBudgetExhausted, err
}

func retryTaskStatus(status string, attempt, maxAttempts int) bool {
	if attempt >= maxAttempts {
		return false
	}

	return status == StatusFailed || status == StatusTimedOut
}

func waitTaskRetryBackoff(ctx context.Context, backoff time.Duration) error {
	if backoff <= 0 {
		return nil
	}

	timer := time.NewTimer(backoff)
	defer timer.Stop()

	select {
	case <-timer.C:
		return nil
	case <-ctx.Done():
		return fmt.Errorf("retry backoff canceled: %w", runContextCauseOrErr(ctx, ctx.Err()))
	}
}

func taskStatusForAttempt(attemptErr, parentErr, err error, timeoutExceeded bool) string {
	if errors.Is(parentErr, context.Canceled) {
		return StatusCanceled
	}

	if errors.Is(attemptErr, context.DeadlineExceeded) {
		return StatusTimedOut
	}

	if errors.Is(attemptErr, context.Canceled) {
		return StatusCanceled
	}

	if timeoutExceeded {
		return StatusTimedOut
	}

	if err == nil {
		return StatusSucceeded
	}

	return StatusFailed
}

func taskErrorForStatus(attemptCtx, parentCtx context.Context, status string, err error) error {
	if status == StatusTimedOut {
		cause := runContextCauseOrErr(attemptCtx, context.DeadlineExceeded)
		if err == nil {
			return cause
		}

		if errors.Is(err, context.DeadlineExceeded) {
			return err
		}

		return errors.Join(err, cause)
	}

	if status == StatusCanceled {
		cause := runContextCauseOrErr(attemptCtx, context.Canceled)
		if parentCtx != nil && errors.Is(parentCtx.Err(), context.Canceled) {
			cause = runContextCauseOrErr(parentCtx, context.Canceled)
		} else if errors.Is(cause, context.Canceled) && parentCtx != nil {
			cause = runContextCauseOrErr(parentCtx, context.Canceled)
		}

		if err == nil || errors.Is(err, context.Canceled) {
			return cause
		}

		return errors.Join(err, cause)
	}

	return err
}

func runContextCauseOrErr(ctx context.Context, fallback error) error {
	if ctx == nil {
		return fallback
	}

	cause := context.Cause(ctx)
	if cause != nil {
		return fmt.Errorf("%w", cause)
	}

	return fallback
}

func taskSiblingCancelCause(result TaskResult) error {
	status := taskRunErrorStatus(result.Status)
	if strings.TrimSpace(status) == "" {
		status = "failed"
	}

	if detail := strings.TrimSpace(result.Error); detail != "" {
		return fmt.Errorf("async: sibling cancellation after task %q %s: %s: %w", result.Task.ID, status, detail, context.Canceled)
	}

	return fmt.Errorf("async: sibling cancellation after task %q %s: %w", result.Task.ID, status, context.Canceled)
}

func taskResultFromError(wave, order int, task Task, ledger *runLedgerStore, status string, attempts int, err error) TaskResult {
	now := time.Now().UTC()

	return TaskResult{
		StartedAt:  now,
		FinishedAt: now,
		Wave:       wave,
		Order:      order,
		Attempts:   attempts,
		Task:       task,
		Error:      err.Error(),
		Status:     status,
		LedgerPath: ledger.path(),
	}
}

func admissionForTask(
	wave int,
	order int,
	task Task,
	attempt int,
	admitted bool,
	denyErr error,
	ledger *runLedgerStore,
	opts RunOptions,
) Admission {
	now := time.Now().UTC()
	admission := Admission{
		RecordedAt:        now,
		AdmissionID:       "admission-" + newRunID(now),
		ChildID:           task.ID,
		ParentRunID:       ledger.runID(),
		Wave:              wave,
		Order:             order,
		WorkspaceID:       task.WorkspaceID,
		AllowedWriteScope: task.AllowedWriteScope,
		Model:             task.Model,
		Provider:          task.Provider,
		Autonomy:          strings.TrimSpace(opts.Autonomy),
		Timeout:           opts.Timeout,
		Budget:            opts.Budget,
		RetryPolicy:       opts.RetryPolicy,
		Attempt:           attempt,
		Admitted:          admitted,
	}
	if denyErr != nil {
		admission.DenyReason = denyErr.Error()
	}

	return admission
}

func taskStatusNeedsStopReceipt(status string) bool {
	return status == StatusCanceled || status == StatusTimedOut || status == StatusBudgetExhausted
}

func stopReceiptForTask(
	wave int,
	order int,
	task Task,
	attempt int,
	admissionID string,
	status string,
	err error,
	ledger *runLedgerStore,
) StopReceipt {
	now := time.Now().UTC()
	receipt := StopReceipt{
		RecordedAt:  now,
		StopID:      "stop-" + newRunID(now),
		AdmissionID: admissionID,
		ChildID:     task.ID,
		ParentRunID: ledger.runID(),
		Wave:        wave,
		Order:       order,
		Attempt:     attempt,
		Status:      status,
	}
	if err != nil {
		receipt.Reason = err.Error()
	}

	return receipt
}

func seedResumedTaskResults(
	results []TaskResult,
	wave int,
	batch []Task,
	ledger *runLedgerStore,
	opts RunOptions,
	stableResumed map[string]struct{},
) []bool {
	skipped := make([]bool, len(batch))
	if !opts.Resume || ledger == nil {
		return skipped
	}

	for i := range batch {
		if !resumableDependencies(batch[i], stableResumed) {
			continue
		}

		result, ok := ledger.succeeded(batch[i])
		if !ok {
			continue
		}

		result.Task = batch[i]
		result.Wave = wave
		result.Order = i
		result.Resumed = true
		result.Status = StatusSkipped
		result.LedgerPath = ledger.path()
		results[i] = result
		skipped[i] = true
		ignoreRunLedgerError(ledger.recordResult(result))
	}

	return skipped
}

func resumableDependencies(task Task, stableResumed map[string]struct{}) bool {
	for _, dep := range task.DependsOn {
		if _, ok := stableResumed[dep]; !ok {
			return false
		}
	}

	return true
}

func normalizeRunOptions(opts RunOptions, taskCount int) RunOptions {
	if opts.MaxConcurrency <= 0 {
		opts.MaxConcurrency = defaultMaxConcurrency
	}

	if taskCount > 0 && opts.MaxConcurrency > taskCount {
		opts.MaxConcurrency = taskCount
	}

	if opts.Budget.MaxOutputBytes > 0 && opts.MaxConcurrency > 1 {
		opts.MaxConcurrency = 1
	}

	if opts.MaxConcurrency <= 0 {
		opts.MaxConcurrency = 1
	}

	if opts.RetryPolicy.MaxAttempts <= 0 {
		opts.RetryPolicy.MaxAttempts = defaultMaxAttempts
	}

	if opts.RetryPolicy.Backoff < 0 {
		opts.RetryPolicy.Backoff = 0
	}

	return opts
}

func applyTaskDefaults(tasks []Task, opts RunOptions) []Task {
	out := make([]Task, len(tasks))
	needsIdentity := opts.LedgerPath != "" || opts.WorkspaceID != "" || opts.AllowedWriteScope != "" || opts.Model != "" || opts.Provider != ""
	needsEstimate := opts.LedgerPath != "" || opts.Budget.MaxPromptTokens > 0

	for i := range tasks {
		task := cloneTask(tasks[i])
		if needsIdentity && strings.TrimSpace(task.WorkspaceID) == "" {
			task.WorkspaceID = taskWorkspaceID(opts.WorkspaceID, task.ID)
		}

		if strings.TrimSpace(task.AllowedWriteScope) == "" {
			task.AllowedWriteScope = opts.AllowedWriteScope
		}

		if strings.TrimSpace(task.Model) == "" {
			task.Model = opts.Model
		}

		if strings.TrimSpace(task.Provider) == "" {
			task.Provider = opts.Provider
		}

		if needsEstimate && task.EstimatedPromptTokens <= 0 {
			task.EstimatedPromptTokens = estimateRunPromptTokens(task.Prompt)
		}

		out[i] = task
	}

	return out
}

func taskWorkspaceID(parentWorkspaceID, taskID string) string {
	parentWorkspaceID = strings.TrimSpace(parentWorkspaceID)
	taskID = strings.TrimSpace(taskID)
	if parentWorkspaceID == "" {
		return taskID
	}

	if taskID == "" {
		return parentWorkspaceID
	}

	return parentWorkspaceID + "/" + taskID
}

func validateRunAllowedWriteScope(childScope, parentScope string) error {
	childScope = strings.TrimSpace(childScope)
	parentScope = strings.TrimSpace(parentScope)
	if childScope == "" || parentScope == "" {
		return nil
	}

	parentAbs, err := resolveRunAllowedWriteScopePath(parentScope)
	if err != nil {
		return fmt.Errorf("resolve parent allowed write scope %q: %w", parentScope, err)
	}

	childAbs, err := resolveRunAllowedWriteScopePath(childScope)
	if err != nil {
		return fmt.Errorf("resolve child allowed write scope %q: %w", childScope, err)
	}

	rel, err := filepath.Rel(parentAbs, childAbs)
	if err != nil {
		return fmt.Errorf("compare child allowed write scope %q to parent scope %q: %w", childScope, parentScope, err)
	}

	if rel == "." || (!filepath.IsAbs(rel) && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))) {
		return nil
	}

	return fmt.Errorf("allowed write scope %q escapes parent scope %q", childScope, parentScope)
}

func resolveRunAllowedWriteScopePath(path string) (string, error) {
	abs, err := filepath.Abs(path)
	if err != nil {
		return "", fmt.Errorf("absolute scope path: %w", err)
	}

	clean := filepath.Clean(abs)
	resolved, err := filepath.EvalSymlinks(clean)
	if err == nil {
		return filepath.Clean(resolved), nil
	}

	if !errors.Is(err, os.ErrNotExist) {
		return "", fmt.Errorf("evaluate scope symlinks %q: %w", clean, err)
	}

	return resolveRunAllowedWriteScopeExistingAncestor(clean)
}

func resolveRunAllowedWriteScopeExistingAncestor(path string) (string, error) {
	missing := make([]string, 0)
	current := path

	for {
		if _, err := os.Lstat(current); err == nil {
			resolved, evalErr := filepath.EvalSymlinks(current)
			if evalErr != nil {
				return "", fmt.Errorf("evaluate scope ancestor symlinks %q: %w", current, evalErr)
			}

			for i := len(missing) - 1; i >= 0; i-- {
				resolved = filepath.Join(resolved, missing[i])
			}

			return filepath.Clean(resolved), nil
		} else if !errors.Is(err, os.ErrNotExist) {
			return "", fmt.Errorf("stat scope ancestor %q: %w", current, err)
		}

		parent := filepath.Dir(current)
		if parent == current {
			return filepath.Clean(path), nil
		}

		missing = append(missing, filepath.Base(current))
		current = parent
	}
}

func estimateRunPromptTokens(prompt string) int {
	prompt = strings.TrimSpace(prompt)
	if prompt == "" {
		return 0
	}

	words := len(strings.Fields(prompt))
	bytesEstimate := (len(prompt) + 3) / 4
	if bytesEstimate > words {
		return bytesEstimate
	}

	return words
}

func mergeTaskUsage(reserved Usage, out TaskRunOutput) Usage {
	usage := reserved
	if out.PromptTokens > 0 {
		usage.PromptTokens = out.PromptTokens
	}

	if out.EstimatedCostMicros > 0 {
		usage.EstimatedCostMicros = out.EstimatedCostMicros
	}

	usage.OutputBytes = int64(len(out.Stdout) + len(out.Stderr))

	return usage
}

type outputByteLimitContextKey struct{}

// OutputByteLimit returns the remaining aggregate output-byte budget for the
// current attempt, when one is configured. DetailedTaskRunner implementations
// that can enforce output caps during execution should honor this value.
func OutputByteLimit(ctx context.Context) int64 {
	if ctx == nil {
		return 0
	}

	value := ctx.Value(outputByteLimitContextKey{})
	remaining, ok := value.(int64)
	if !ok || remaining <= 0 {
		return 0
	}

	return remaining
}

func withRunRemainingOutputBudget(ctx context.Context, budget *runBudgetTracker) context.Context {
	outputBytesRemaining := budget.remainingOutputBytes()
	if outputBytesRemaining <= 0 {
		return ctx
	}

	return context.WithValue(ctx, outputByteLimitContextKey{}, outputBytesRemaining)
}

func ignoreRunLedgerError(_ error) {}

type runBudgetTracker struct {
	mu               sync.Mutex
	maxOutputBytes   int64
	usedOutputBytes  int64
	maxCostMicros    int64
	usedCostMicros   int64
	maxPromptTokens  int
	usedPromptTokens int
}

func newRunBudgetTracker(budget Budget, ledger *runLedgerStore) *runBudgetTracker {
	tracker := &runBudgetTracker{
		maxOutputBytes:  budget.MaxOutputBytes,
		maxPromptTokens: budget.MaxPromptTokens,
		maxCostMicros:   budget.MaxCostMicros,
	}
	if ledger == nil {
		return tracker
	}

	attempts := ledger.completedAttempts()
	for i := range attempts {
		attempt := attempts[i]
		tracker.usedPromptTokens += attempt.Usage.PromptTokens
		tracker.usedCostMicros += attempt.Usage.EstimatedCostMicros
		tracker.usedOutputBytes += attempt.Usage.OutputBytes
	}

	return tracker
}

func (b *runBudgetTracker) reserve(task Task) (Usage, error) {
	usage := Usage{PromptTokens: task.EstimatedPromptTokens, EstimatedCostMicros: task.EstimatedCostMicros}
	if b == nil {
		return usage, nil
	}

	b.mu.Lock()
	defer b.mu.Unlock()

	var errs []error
	if b.maxOutputBytes > 0 && b.usedOutputBytes >= b.maxOutputBytes {
		errs = append(errs, fmt.Errorf("output byte budget exhausted: used %d of %d", b.usedOutputBytes, b.maxOutputBytes))
	}

	if b.maxPromptTokens > 0 && b.usedPromptTokens+usage.PromptTokens > b.maxPromptTokens {
		errs = append(errs, fmt.Errorf("prompt token budget exceeded: requested %d, used %d of %d", usage.PromptTokens, b.usedPromptTokens, b.maxPromptTokens))
	}

	if b.maxCostMicros > 0 && b.usedCostMicros+usage.EstimatedCostMicros > b.maxCostMicros {
		errs = append(errs, fmt.Errorf("cost budget exceeded: requested %d micros, used %d of %d", usage.EstimatedCostMicros, b.usedCostMicros, b.maxCostMicros))
	}

	if err := errors.Join(errs...); err != nil {
		return usage, err
	}

	b.usedPromptTokens += usage.PromptTokens
	b.usedCostMicros += usage.EstimatedCostMicros

	return usage, nil
}

func (b *runBudgetTracker) recordActual(reserved, actual Usage) error {
	if b == nil {
		return nil
	}

	b.mu.Lock()
	defer b.mu.Unlock()

	b.usedOutputBytes += actual.OutputBytes
	b.usedPromptTokens += actual.PromptTokens - reserved.PromptTokens
	b.usedCostMicros += actual.EstimatedCostMicros - reserved.EstimatedCostMicros

	var errs []error
	if b.maxPromptTokens > 0 && b.usedPromptTokens > b.maxPromptTokens {
		errs = append(errs, fmt.Errorf("prompt token budget exceeded: used %d of %d", b.usedPromptTokens, b.maxPromptTokens))
	}

	if b.maxCostMicros > 0 && b.usedCostMicros > b.maxCostMicros {
		errs = append(errs, fmt.Errorf("cost budget exceeded: used %d micros of %d", b.usedCostMicros, b.maxCostMicros))
	}

	if b.maxOutputBytes > 0 && b.usedOutputBytes > b.maxOutputBytes {
		errs = append(errs, fmt.Errorf("output byte budget exceeded: used %d of %d", b.usedOutputBytes, b.maxOutputBytes))
	}

	return errors.Join(errs...)
}

func (b *runBudgetTracker) remainingOutputBytes() int64 {
	if b == nil {
		return 0
	}

	b.mu.Lock()
	defer b.mu.Unlock()

	if b.maxOutputBytes <= 0 {
		return 0
	}

	remaining := b.maxOutputBytes - b.usedOutputBytes
	if remaining < 0 {
		return 0
	}

	return remaining
}

//nolint:govet // Field order keeps the lock next to guarded state.
type runLedgerStore struct {
	mu      sync.Mutex
	pathTo  string
	ledger  Ledger
	lastErr error
}

func openRunLedger(opts RunOptions, tasks []Task) (*runLedgerStore, error) {
	path := strings.TrimSpace(opts.LedgerPath)
	if path == "" {
		if opts.Resume {
			return nil, errors.New("async: resume requires ledger path")
		}

		return nil, nil
	}

	ledger := Ledger{}
	if opts.Resume {
		loaded, err := loadRunLedger(path)
		if err != nil {
			return nil, err
		}

		ledger = loaded
	}

	now := time.Now().UTC()
	if opts.Resume {
		recoverRunningTaskAttempts(&ledger, now)
	}

	if ledger.RunID == "" {
		ledger.RunID = newRunID(now)
	}

	if ledger.StartedAt.IsZero() {
		ledger.StartedAt = now
	}

	ledger.UpdatedAt = now
	ledger.Options = opts
	ledger.Tasks = append([]Task(nil), tasks...)

	store := &runLedgerStore{pathTo: path, ledger: ledger}
	if err := store.saveLocked(); err != nil {
		return nil, err
	}

	return store, nil
}

func loadRunLedger(path string) (Ledger, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return Ledger{}, fmt.Errorf("async: resume ledger %s does not exist: %w", path, err)
		}

		return Ledger{}, fmt.Errorf("async: read ledger %s: %w", path, err)
	}

	var ledger Ledger
	if err := json.Unmarshal(data, &ledger); err != nil {
		return Ledger{}, fmt.Errorf("async: parse ledger %s: %w", path, err)
	}

	return ledger, nil
}

func recoverRunningTaskAttempts(ledger *Ledger, now time.Time) {
	if ledger == nil {
		return
	}

	for i := range ledger.Attempts {
		if ledger.Attempts[i].Status != StatusRunning {
			continue
		}

		ledger.Attempts[i].Status = StatusCanceled
		ledger.Attempts[i].FinishedAt = now
		if !ledger.Attempts[i].StartedAt.IsZero() {
			ledger.Attempts[i].Duration = now.Sub(ledger.Attempts[i].StartedAt)
		}

		if ledger.Attempts[i].Error == "" {
			ledger.Attempts[i].Error = "async: recovered running attempt during resume"
		}

		if ledger.Attempts[i].AdmissionID != "" {
			receipt := recoveredTaskStopReceipt(ledger, ledger.Attempts[i], now)
			ledger.Attempts[i].StopID = receipt.StopID
			ledger.StopReceipts = append(ledger.StopReceipts, receipt)
		}
	}
}

func recoveredTaskStopReceipt(ledger *Ledger, attempt TaskAttempt, now time.Time) StopReceipt {
	return StopReceipt{
		RecordedAt:  now,
		StopID:      "stop-" + newRunID(now),
		AdmissionID: attempt.AdmissionID,
		ChildID:     attempt.Task.ID,
		ParentRunID: ledger.RunID,
		Wave:        attempt.Wave,
		Order:       attempt.Order,
		Attempt:     attempt.Attempt,
		Status:      StatusCanceled,
		Reason:      attempt.Error,
	}
}

func (s *runLedgerStore) path() string {
	if s == nil {
		return ""
	}

	return s.pathTo
}

func (s *runLedgerStore) runID() string {
	if s == nil {
		return ""
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	return s.ledger.RunID
}

func (s *runLedgerStore) completedAttempts() []TaskAttempt {
	if s == nil {
		return nil
	}

	attempts := make([]TaskAttempt, 0, len(s.ledger.Attempts))
	for i := range s.ledger.Attempts {
		attempt := s.ledger.Attempts[i]
		if attempt.Status == StatusSucceeded || attempt.Status == StatusFailed || attempt.Status == StatusTimedOut ||
			attempt.Status == StatusCanceled || attempt.Status == StatusBudgetExhausted || attempt.Status == StatusDenied {
			attempts = append(attempts, attempt)
		}
	}

	return attempts
}

func (s *runLedgerStore) succeeded(task Task) (TaskResult, bool) {
	if s == nil {
		return TaskResult{}, false
	}

	var latest TaskResult
	found := false
	for i := len(s.ledger.Results) - 1; i >= 0; i-- {
		result := s.ledger.Results[i]
		if sameTaskForResume(result.Task, task) {
			latest = result
			found = true
			break
		}
	}

	for i := len(s.ledger.Attempts) - 1; i >= 0; i-- {
		attempt := s.ledger.Attempts[i]
		if sameTaskForResume(attempt.Task, task) {
			result := taskResultFromAttempt(attempt)
			if !found || taskResultTime(result).After(taskResultTime(latest)) {
				latest = result
				found = true
			}

			break
		}
	}

	if found && (latest.Status == StatusSucceeded || latest.Status == StatusSkipped) {
		return latest, true
	}

	return TaskResult{}, false
}

func taskResultFromAttempt(attempt TaskAttempt) TaskResult {
	return TaskResult{
		StartedAt:      attempt.StartedAt,
		FinishedAt:     attempt.FinishedAt,
		Duration:       attempt.Duration,
		Wave:           attempt.Wave,
		Order:          attempt.Order,
		Attempts:       attempt.Attempt,
		Task:           attempt.Task,
		Output:         attempt.Output,
		Stderr:         attempt.Stderr,
		Error:          attempt.Error,
		Status:         attempt.Status,
		AdmissionID:    attempt.AdmissionID,
		StopID:         attempt.StopID,
		TranscriptPath: attempt.TranscriptPath,
		Artifacts:      append([]string(nil), attempt.Artifacts...),
		ExitStatus:     attempt.ExitStatus,
		Usage:          attempt.Usage,
	}
}

func taskResultTime(result TaskResult) time.Time {
	if !result.FinishedAt.IsZero() {
		return result.FinishedAt
	}

	return result.StartedAt
}

func sameTaskForResume(previous, current Task) bool {
	return previous.ID == current.ID &&
		previous.Agent == current.Agent &&
		previous.Prompt == current.Prompt &&
		sameWorkspaceForResume(previous.WorkspaceID, current.WorkspaceID, current.ID) &&
		previous.AllowedWriteScope == current.AllowedWriteScope &&
		previous.Model == current.Model &&
		previous.Provider == current.Provider &&
		previous.EstimatedPromptTokens == current.EstimatedPromptTokens &&
		previous.EstimatedCostMicros == current.EstimatedCostMicros &&
		sameStringSet(previous.DependsOn, current.DependsOn)
}

func sameWorkspaceForResume(previous, current, childID string) bool {
	previous = strings.TrimSpace(previous)
	current = strings.TrimSpace(current)
	childID = strings.TrimSpace(childID)
	if previous == current {
		return true
	}

	// CLI utility runs create a fresh parent session ID for each invocation, but
	// the child boundary is still stable when both workspace IDs are derived by
	// appending the same task ID. Treat those derived child workspaces as
	// equivalent so --spawn-resume can skip already-completed children without
	// requiring operators to pin the transient parent session.
	return derivedChildWorkspaceID(previous, childID) && derivedChildWorkspaceID(current, childID)
}

func derivedChildWorkspaceID(workspaceID, childID string) bool {
	if workspaceID == "" || childID == "" {
		return false
	}

	return workspaceID == childID || strings.HasSuffix(workspaceID, "/"+childID)
}

func sameStringSet(left, right []string) bool {
	leftSet := make(map[string]struct{}, len(left))
	for _, value := range left {
		leftSet[value] = struct{}{}
	}

	rightSet := make(map[string]struct{}, len(right))
	for _, value := range right {
		rightSet[value] = struct{}{}
	}

	if len(leftSet) != len(rightSet) {
		return false
	}

	for value := range leftSet {
		if _, ok := rightSet[value]; !ok {
			return false
		}
	}

	return true
}

func (s *runLedgerStore) startAttempt(attempt TaskAttempt) int {
	if s == nil {
		return -1
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	s.ledger.Attempts = append(s.ledger.Attempts, attempt)
	index := len(s.ledger.Attempts) - 1
	ignoreRunLedgerError(s.saveLocked())

	return index
}

func (s *runLedgerStore) recordAdmission(admission Admission) error {
	if s == nil {
		return nil
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	s.ledger.Admissions = append(s.ledger.Admissions, admission)

	return s.saveLocked()
}

func (s *runLedgerStore) recordStopReceipt(receipt StopReceipt) error {
	if s == nil {
		return nil
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	s.ledger.StopReceipts = append(s.ledger.StopReceipts, receipt)

	return s.saveLocked()
}

func (s *runLedgerStore) finishAttempt(index int, attempt TaskAttempt) error {
	if s == nil {
		return nil
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	if index >= 0 && index < len(s.ledger.Attempts) {
		s.ledger.Attempts[index] = attempt
	} else {
		s.ledger.Attempts = append(s.ledger.Attempts, attempt)
	}

	return s.saveLocked()
}

func (s *runLedgerStore) recordResult(result TaskResult) error {
	if s == nil {
		return nil
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	for i := range s.ledger.Results {
		if sameTaskForResume(s.ledger.Results[i].Task, result.Task) {
			s.ledger.Results[i] = result
			return s.saveLocked()
		}
	}

	s.ledger.Results = append(s.ledger.Results, result)

	return s.saveLocked()
}

func (s *runLedgerStore) writeTranscript(id string, attempt int, out TaskRunOutput, err error) string {
	if s == nil || s.pathTo == "" {
		return ""
	}

	dir := filepath.Join(filepath.Dir(s.pathTo), "transcripts")
	if mkdirErr := os.MkdirAll(dir, 0o750); mkdirErr != nil {
		s.recordError(fmt.Errorf("async: create transcript dir: %w", mkdirErr))
		return ""
	}

	path := filepath.Join(dir, safeRunFileName(id)+fmt.Sprintf("-attempt-%d-%s.txt", attempt, newRunID(time.Now().UTC())))
	var b strings.Builder
	if out.Stdout != "" {
		fmt.Fprintf(&b, "# stdout\n%s\n", out.Stdout)
	}

	if out.Stderr != "" {
		fmt.Fprintf(&b, "# stderr\n%s\n", out.Stderr)
	}

	if err != nil {
		fmt.Fprintf(&b, "# error\n%s\n", err.Error())
	}

	if writeErr := os.WriteFile(path, []byte(b.String()), 0o600); writeErr != nil {
		s.recordError(fmt.Errorf("async: write transcript %s: %w", path, writeErr))
		return ""
	}

	return path
}

func (s *runLedgerStore) recordError(err error) {
	if s == nil || err == nil {
		return
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	s.lastErr = errors.Join(s.lastErr, err)
}

func (s *runLedgerStore) rememberLocked(err error) error {
	if err != nil {
		s.lastErr = errors.Join(s.lastErr, err)
	}

	return err
}

func (s *runLedgerStore) ledgerError() error {
	if s == nil {
		return nil
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	return s.lastErr
}

func (s *runLedgerStore) saveLocked() error {
	if s == nil || s.pathTo == "" {
		return nil
	}

	s.ledger.UpdatedAt = time.Now().UTC()

	if err := os.MkdirAll(filepath.Dir(s.pathTo), 0o750); err != nil {
		return s.rememberLocked(fmt.Errorf("async: create ledger dir: %w", err))
	}

	data, err := json.MarshalIndent(s.ledger, "", "  ")
	if err != nil {
		return s.rememberLocked(fmt.Errorf("async: marshal ledger: %w", err))
	}

	data = append(data, '\n')
	tmp, err := os.CreateTemp(filepath.Dir(s.pathTo), ".ledger-*.json")
	if err != nil {
		return s.rememberLocked(fmt.Errorf("async: create ledger temp: %w", err))
	}

	tmpPath := tmp.Name()
	defer os.Remove(tmpPath)

	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		return s.rememberLocked(fmt.Errorf("async: write ledger temp: %w", err))
	}

	if err := tmp.Close(); err != nil {
		return s.rememberLocked(fmt.Errorf("async: close ledger temp: %w", err))
	}

	if err := os.Rename(tmpPath, s.pathTo); err != nil {
		return s.rememberLocked(fmt.Errorf("async: replace ledger %s: %w", s.pathTo, err))
	}

	return nil
}

func newRunID(now time.Time) string {
	var b [4]byte
	if _, err := rand.Read(b[:]); err != nil {
		return now.Format("20060102-150405")
	}

	return now.Format("20060102-150405") + "-" + hex.EncodeToString(b[:])
}

func safeRunFileName(value string) string {
	value = strings.TrimSpace(value)
	if value == "" {
		return "task"
	}

	var b strings.Builder
	for _, r := range value {
		switch {
		case r >= 'a' && r <= 'z':
			b.WriteRune(r)
		case r >= 'A' && r <= 'Z':
			b.WriteRune(r)
		case r >= '0' && r <= '9':
			b.WriteRune(r)
		case r == '-' || r == '_' || r == '.':
			b.WriteRune(r)
		default:
			b.WriteRune('-')
		}
	}

	return b.String()
}
