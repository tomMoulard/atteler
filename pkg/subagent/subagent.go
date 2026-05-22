// Package subagent provides bounded concurrent spawning primitives for child agent work.
//
//nolint:wsl_v5,modernize // Spawn orchestration keeps related cancellation and persistence steps together.
package subagent

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

const (
	// StatusSucceeded means a child request completed without error.
	StatusSucceeded = "succeeded"
	// StatusFailed means a child request returned an error.
	StatusFailed = "failed"
	// StatusCanceled means a child request was canceled before completion.
	StatusCanceled = "canceled"
	// StatusTimedOut means a child request exceeded its per-child timeout.
	StatusTimedOut = "timed_out"
	// StatusBudgetExhausted means a child request exceeded a budget before or during execution.
	StatusBudgetExhausted = "budget_exhausted"
	// StatusSkipped means a child request was not started because a prior result was resumed.
	StatusSkipped = "skipped"
	// StatusRunning is recorded in the ledger while a child attempt is in progress.
	StatusRunning = "running"

	defaultMaxConcurrency = 4
	defaultMaxAttempts    = 1
)

// Request describes one child agent invocation.
type Request struct {
	ID                    string `json:"id"`
	Agent                 string `json:"agent"`
	Prompt                string `json:"prompt"`
	WorkspaceID           string `json:"workspace_id,omitempty"`
	AllowedWriteScope     string `json:"allowed_write_scope,omitempty"`
	Model                 string `json:"model,omitempty"`
	Provider              string `json:"provider,omitempty"`
	EstimatedPromptTokens int    `json:"estimated_prompt_tokens,omitempty"`
	EstimatedCostMicros   int64  `json:"estimated_cost_micros,omitempty"`
}

// Runner executes one child agent request and returns its text output.
type Runner func(context.Context, Request) (string, error)

// DetailedRunner executes one child agent request and returns auditable output metadata.
type DetailedRunner func(context.Context, Request) (RunOutput, error)

// RunOutput captures process-level output returned by a DetailedRunner.
type RunOutput struct {
	Stdout              string   `json:"stdout,omitempty"`
	Stderr              string   `json:"stderr,omitempty"`
	Artifacts           []string `json:"artifacts,omitempty"`
	ExitStatus          int      `json:"exit_status,omitempty"`
	PromptTokens        int      `json:"prompt_tokens,omitempty"`
	EstimatedCostMicros int64    `json:"estimated_cost_micros,omitempty"`
	BudgetExhausted     bool     `json:"budget_exhausted,omitempty"`
}

// Usage records budget consumption attributed to a child attempt.
type Usage struct {
	PromptTokens        int   `json:"prompt_tokens,omitempty"`
	OutputBytes         int64 `json:"output_bytes,omitempty"`
	EstimatedCostMicros int64 `json:"estimated_cost_micros,omitempty"`
}

// RetryPolicy controls retry behavior for failed child requests.
type RetryPolicy struct {
	Backoff     time.Duration `json:"backoff,omitempty"`
	MaxAttempts int           `json:"max_attempts,omitempty"`
}

// Budget describes aggregate spawn-run ceilings. Token and cost limits use
// request estimates before spawn and runner-reported usage after completion
// when that usage is available.
type Budget struct {
	MaxOutputBytes  int64 `json:"max_output_bytes,omitempty"`
	MaxCostMicros   int64 `json:"max_cost_micros,omitempty"`
	MaxPromptTokens int   `json:"max_prompt_tokens,omitempty"`
}

// Options controls concurrency, cancellation, retry, budget, and recovery behavior.
//
//nolint:govet // Field order groups user-facing options for CLI/ledger readability.
type Options struct {
	Timeout           time.Duration `json:"timeout,omitempty"`
	RetryPolicy       RetryPolicy   `json:"retry_policy,omitempty"`
	Budget            Budget        `json:"budget,omitempty"`
	LedgerPath        string        `json:"ledger_path,omitempty"`
	WorkspaceID       string        `json:"workspace_id,omitempty"`
	AllowedWriteScope string        `json:"allowed_write_scope,omitempty"`
	Model             string        `json:"model,omitempty"`
	Provider          string        `json:"provider,omitempty"`
	MaxConcurrency    int           `json:"max_concurrency,omitempty"`
	CancelOnFailure   bool          `json:"cancel_on_failure,omitempty"`
	Resume            bool          `json:"resume,omitempty"`
}

// Result captures the outcome and timing for one child agent request.
//
//nolint:govet // Field order keeps lifecycle metadata before request payload for readability.
type Result struct {
	StartedAt      time.Time     `json:"started_at,omitempty"`
	FinishedAt     time.Time     `json:"finished_at,omitempty"`
	Duration       time.Duration `json:"duration,omitempty"`
	Request        Request       `json:"request"`
	Output         string        `json:"output,omitempty"`
	Stderr         string        `json:"stderr,omitempty"`
	Error          string        `json:"error,omitempty"`
	Status         string        `json:"status"`
	LedgerPath     string        `json:"ledger_path,omitempty"`
	TranscriptPath string        `json:"transcript_path,omitempty"`
	Artifacts      []string      `json:"artifacts,omitempty"`
	Usage          Usage         `json:"usage,omitempty"`
	Attempts       int           `json:"attempts,omitempty"`
	ExitStatus     int           `json:"exit_status,omitempty"`
	Resumed        bool          `json:"resumed,omitempty"`
}

// Attempt captures one durable attempt for a child request.
//
//nolint:govet // Field order mirrors Result for ledger readability.
type Attempt struct {
	StartedAt      time.Time     `json:"started_at"`
	FinishedAt     time.Time     `json:"finished_at,omitempty"`
	Duration       time.Duration `json:"duration,omitempty"`
	Request        Request       `json:"request"`
	Attempt        int           `json:"attempt"`
	Status         string        `json:"status"`
	Stdout         string        `json:"stdout,omitempty"`
	Stderr         string        `json:"stderr,omitempty"`
	Error          string        `json:"error,omitempty"`
	ExitStatus     int           `json:"exit_status,omitempty"`
	TranscriptPath string        `json:"transcript_path,omitempty"`
	Artifacts      []string      `json:"artifacts,omitempty"`
	Usage          Usage         `json:"usage,omitempty"`
}

// Ledger is an auditable JSON document for a spawn run.
//
//nolint:govet // Field order keeps lifecycle metadata before detailed records.
type Ledger struct {
	StartedAt time.Time `json:"started_at"`
	UpdatedAt time.Time `json:"updated_at"`
	RunID     string    `json:"run_id"`
	Options   Options   `json:"options"`
	Requests  []Request `json:"requests"`
	Attempts  []Attempt `json:"attempts,omitempty"`
	Results   []Result  `json:"results,omitempty"`
}

// CommandOptions controls AttelerCommand invocations.
type CommandOptions struct {
	Env            map[string]string
	Binary         string
	Dir            string
	Args           []string
	MaxOutputBytes int64
}

// SpawnAll runs requests with default bounded concurrency and returns results in
// the same order as the input requests. Every started request is allowed to
// finish; request failures are recorded in their Result and returned as a joined
// error.
func SpawnAll(ctx context.Context, requests []Request, run Runner) ([]Result, error) {
	return SpawnAllWithOptions(ctx, requests, run, Options{})
}

// SpawnAllWithOptions runs requests under explicit budgets, retry policy,
// cancellation behavior, and an optional durable ledger.
func SpawnAllWithOptions(ctx context.Context, requests []Request, run Runner, opts Options) ([]Result, error) {
	if run == nil {
		return nil, errors.New("subagent: runner is required")
	}

	return SpawnAllDetailed(ctx, requests, func(ctx context.Context, request Request) (RunOutput, error) {
		output, err := run(ctx, request)

		return RunOutput{Stdout: output}, err
	}, opts)
}

// SpawnAllDetailed runs requests with a runner that returns stdout/stderr,
// artifact, exit-status, and usage metadata for the ledger.
//
//nolint:gocognit // The orchestration path is kept linear so cancellation semantics stay visible.
func SpawnAllDetailed(ctx context.Context, requests []Request, run DetailedRunner, opts Options) ([]Result, error) {
	if ctx == nil {
		return nil, errors.New("subagent: context is required")
	}

	if run == nil {
		return nil, errors.New("subagent: runner is required")
	}

	if err := validateRequests(requests); err != nil {
		return nil, err
	}

	if err := ctx.Err(); err != nil {
		return nil, fmt.Errorf("subagent: context canceled before spawning: %w", err)
	}

	opts = normalizeOptions(opts, len(requests))
	requests = applyRequestDefaults(requests, opts)

	ledger, err := openLedger(opts, requests)
	if err != nil {
		return nil, err
	}

	results := make([]Result, len(requests))
	skipped := seedResumedResults(results, requests, ledger, opts)
	budget := newBudgetTracker(opts.Budget, ledger)
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	errs := make([]error, len(requests))
	jobs := make(chan int)
	var wg sync.WaitGroup

	for range opts.MaxConcurrency {
		wg.Add(1)
		go func() {
			defer wg.Done()

			for {
				index, ok := nextRequestIndex(ctx, jobs)
				if !ok {
					return
				}

				result, err := runRequest(ctx, requests[index], run, opts, budget, ledger)
				results[index] = result
				if err != nil {
					errs[index] = err
					if opts.CancelOnFailure || result.Status == StatusBudgetExhausted {
						cancel()
					}
				}
			}
		}()
	}

	submitRequestJobs(ctx, requests, skipped, jobs, results, errs, ledger)

	close(jobs)
	wg.Wait()

	return results, errors.Join(errors.Join(errs...), ledger.ledgerError())
}

func submitRequestJobs(
	ctx context.Context,
	requests []Request,
	skipped []bool,
	jobs chan<- int,
	results []Result,
	errs []error,
	ledger *ledgerStore,
) {
	for i := range requests {
		if skipped[i] {
			continue
		}

		if err := ctx.Err(); err != nil {
			recordRequestCanceledBeforeStart(i, requests[i], results, errs, ledger, err)
			continue
		}

		select {
		case <-ctx.Done():
			recordRequestCanceledBeforeStart(i, requests[i], results, errs, ledger, ctx.Err())
		case jobs <- i:
		}
	}
}

func recordRequestCanceledBeforeStart(
	index int,
	request Request,
	results []Result,
	errs []error,
	ledger *ledgerStore,
	err error,
) {
	results[index] = canceledBeforeStartResult(request, ledger)
	errs[index] = fmt.Errorf("subagent: request %q canceled before start: %w", request.ID, err)
}

func nextRequestIndex(ctx context.Context, jobs <-chan int) (int, bool) {
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

// AttelerCommand returns a Runner that invokes an atteler binary once per
// request with --agent and --once arguments.
func AttelerCommand(binary string) Runner {
	return AttelerCommandWithOptions(CommandOptions{Binary: binary})
}

// AttelerCommandWithOptions returns a Runner that invokes an atteler binary once
// per request with --agent and --once arguments.
func AttelerCommandWithOptions(opts CommandOptions) Runner {
	detailed := AttelerCommandDetailedWithOptions(opts)

	return func(ctx context.Context, request Request) (string, error) {
		out, err := detailed(ctx, request)

		return out.Stdout, err
	}
}

// AttelerCommandDetailedWithOptions returns a DetailedRunner that invokes an
// atteler binary once per request with --agent and --once arguments.
func AttelerCommandDetailedWithOptions(opts CommandOptions) DetailedRunner {
	return func(ctx context.Context, request Request) (RunOutput, error) {
		if ctx == nil {
			return RunOutput{}, errors.New("subagent: context is required")
		}

		binary := strings.TrimSpace(opts.Binary)
		if binary == "" {
			return RunOutput{}, errors.New("subagent: atteler binary is required")
		}

		args := append([]string(nil), opts.Args...)
		args = append(args, "--agent", request.Agent, "--once", request.Prompt)

		cmdCtx, cancel := context.WithCancel(ctx)
		defer cancel()

		cmd := exec.CommandContext(cmdCtx, binary, args...)
		cmd.WaitDelay = commandCancelWaitDelay
		if strings.TrimSpace(opts.Dir) != "" {
			cmd.Dir = opts.Dir
		}

		cmd.Env = childEnv(opts.Env, request)

		maxOutputBytes := commandOutputByteLimit(ctx, opts.MaxOutputBytes)

		var stdout, stderr bytes.Buffer
		var limiter *commandOutputLimiter
		stdoutWriter := io.Writer(&stdout)
		stderrWriter := io.Writer(&stderr)
		if maxOutputBytes > 0 {
			limiter = &commandOutputLimiter{remaining: maxOutputBytes, cancel: cancel}
			stdoutWriter = commandOutputWriter{dst: &stdout, limiter: limiter}
			stderrWriter = commandOutputWriter{dst: &stderr, limiter: limiter}
		}

		cmd.Stdout = stdoutWriter
		cmd.Stderr = stderrWriter

		err := cmd.Run()
		out := RunOutput{
			Stdout:     stdout.String(),
			Stderr:     stderr.String(),
			ExitStatus: commandExitStatus(err),
		}
		if limiter != nil && limiter.exceededLimit() {
			out.BudgetExhausted = true
			if err == nil {
				return out, fmt.Errorf("subagent: atteler command output exceeded %d byte limit", maxOutputBytes)
			}

			return out, fmt.Errorf("subagent: atteler command output exceeded %d byte limit: %w", maxOutputBytes, err)
		}

		if err == nil {
			return out, nil
		}

		message := strings.TrimSpace(stderr.String())
		if message == "" {
			message = err.Error()
		}

		return out, fmt.Errorf("subagent: atteler command failed: %s: %w", message, err)
	}
}

type commandOutputWriter struct {
	dst     *bytes.Buffer
	limiter *commandOutputLimiter
}

func (w commandOutputWriter) Write(p []byte) (int, error) {
	if w.limiter == nil {
		n, _ := w.dst.Write(p)

		return n, nil
	}

	return w.limiter.write(w.dst, p)
}

//nolint:govet // Field order keeps lock next to the state and cancellation hook it protects.
type commandOutputLimiter struct {
	mu        sync.Mutex
	cancel    context.CancelFunc
	remaining int64
	exceeded  bool
}

func (l *commandOutputLimiter) write(dst *bytes.Buffer, p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()

	if l.remaining <= 0 {
		l.exceeded = true
		l.cancelCommand()

		return 0, errors.New(commandOutputLimitExceeded)
	}

	if int64(len(p)) > l.remaining {
		allowed := int(l.remaining)
		if allowed > 0 {
			_, _ = dst.Write(p[:allowed])
		}

		l.remaining = 0
		l.exceeded = true
		l.cancelCommand()

		return allowed, errors.New(commandOutputLimitExceeded)
	}

	n, _ := dst.Write(p)
	l.remaining -= int64(n)

	return n, nil
}

const commandOutputLimitExceeded = "command output byte limit exceeded"

const commandCancelWaitDelay = 250 * time.Millisecond

type outputByteLimitContextKey struct{}

// WithOutputByteLimit returns a context that caps AttelerCommand stdout/stderr
// capture to maxBytes for this invocation. It is intended for orchestrators that
// track an aggregate output budget and need the child command to enforce the
// remaining budget during execution.
func WithOutputByteLimit(ctx context.Context, maxBytes int64) context.Context {
	if ctx == nil || maxBytes <= 0 {
		return ctx
	}

	return context.WithValue(ctx, outputByteLimitContextKey{}, maxBytes)
}

func commandOutputByteLimit(ctx context.Context, configured int64) int64 {
	value := ctx.Value(outputByteLimitContextKey{})
	remaining, ok := value.(int64)
	if !ok {
		remaining = 0
	}

	switch {
	case remaining <= 0:
		return configured
	case configured <= 0 || remaining < configured:
		return remaining
	default:
		return configured
	}
}

func withRemainingOutputBudget(ctx context.Context, budget *budgetTracker) context.Context {
	outputBytesRemaining := budget.remainingOutputBytes()
	if outputBytesRemaining <= 0 {
		return ctx
	}

	return WithOutputByteLimit(ctx, outputBytesRemaining)
}

func (l *commandOutputLimiter) cancelCommand() {
	if l.cancel != nil {
		l.cancel()
	}
}

func (l *commandOutputLimiter) exceededLimit() bool {
	l.mu.Lock()
	defer l.mu.Unlock()

	return l.exceeded
}

func runRequest(
	ctx context.Context,
	request Request,
	run DetailedRunner,
	opts Options,
	budget *budgetTracker,
	ledger *ledgerStore,
) (Result, error) {
	maxAttempts := opts.RetryPolicy.MaxAttempts
	var last Result
	var lastErr error

	for attempt := 1; attempt <= maxAttempts; attempt++ {
		if err := ctx.Err(); err != nil {
			last = resultFromError(request, ledger, StatusCanceled, attempt-1, err)
			ignoreLedgerError(ledger.recordResult(last))

			return last, fmt.Errorf("subagent: request %q canceled: %w", request.ID, err)
		}

		usage, err := budget.reserve(request)
		if err != nil {
			last = resultFromError(request, ledger, StatusBudgetExhausted, attempt-1, err)
			ignoreLedgerError(ledger.recordResult(last))
			ignoreLedgerError(ledger.appendAttempt(Attempt{
				StartedAt:  last.StartedAt,
				FinishedAt: last.FinishedAt,
				Request:    request,
				Attempt:    attempt,
				Status:     StatusBudgetExhausted,
				Error:      err.Error(),
			}))

			return last, fmt.Errorf("subagent: request %q budget exhausted: %w", request.ID, err)
		}

		attemptCtx := ctx
		var cancel context.CancelFunc = func() {}
		if opts.Timeout > 0 {
			attemptCtx, cancel = context.WithTimeout(ctx, opts.Timeout)
		}
		attemptCtx = withRemainingOutputBudget(attemptCtx, budget)

		startedClock := time.Now()
		started := startedClock.UTC()
		attemptIndex := ledger.startAttempt(Attempt{
			StartedAt: started,
			Request:   request,
			Attempt:   attempt,
			Status:    StatusRunning,
			Usage:     usage,
		})

		out, err := run(attemptCtx, request)
		finishedClock := time.Now()
		finished := finishedClock.UTC()
		duration := finishedClock.Sub(startedClock)
		attemptErr := attemptCtx.Err()
		parentErr := ctx.Err()
		timeoutExceeded := opts.Timeout > 0 && duration >= opts.Timeout
		cancel()

		status := statusForAttempt(attemptErr, parentErr, err, timeoutExceeded)
		err = errorForStatus(status, err)
		reserved := usage
		usage = mergeUsage(usage, out)
		if budgetErr := budget.recordActual(reserved, usage); budgetErr != nil {
			status = StatusBudgetExhausted
			err = errors.Join(err, budgetErr)
		}
		status, err = statusAfterRunnerBudgetExhaustion(status, err, out)
		transcriptPath := ledger.writeTranscript(request.ID, attempt, out, err)
		ledgerAttempt := Attempt{
			StartedAt:      started,
			FinishedAt:     finished,
			Duration:       duration,
			Request:        request,
			Attempt:        attempt,
			Status:         status,
			Stdout:         out.Stdout,
			Stderr:         out.Stderr,
			ExitStatus:     out.ExitStatus,
			TranscriptPath: transcriptPath,
			Artifacts:      append([]string(nil), out.Artifacts...),
			Usage:          usage,
		}
		if err != nil {
			ledgerAttempt.Error = err.Error()
		}
		ignoreLedgerError(ledger.finishAttempt(attemptIndex, ledgerAttempt))

		last = Result{
			StartedAt:      started,
			FinishedAt:     finished,
			Duration:       duration,
			Request:        request,
			Output:         out.Stdout,
			Stderr:         out.Stderr,
			Status:         status,
			LedgerPath:     ledger.path(),
			TranscriptPath: transcriptPath,
			Artifacts:      append([]string(nil), out.Artifacts...),
			Usage:          usage,
			Attempts:       attempt,
			ExitStatus:     out.ExitStatus,
		}
		if err != nil {
			last.Error = err.Error()
			lastErr = err
		} else {
			ignoreLedgerError(ledger.recordResult(last))

			return last, nil
		}

		if !shouldRetry(status, attempt, maxAttempts) {
			break
		}

		if err := sleepBeforeRetry(ctx, opts.RetryPolicy.Backoff); err != nil {
			last.Status = StatusCanceled
			last.Error = err.Error()
			lastErr = err
			break
		}
	}

	ignoreLedgerError(ledger.recordResult(last))

	if last.Attempts <= 1 {
		return last, fmt.Errorf("subagent: request %q %s: %w", request.ID, last.Status, lastErr)
	}

	return last, fmt.Errorf("subagent: request %q %s after %d attempt(s): %w", request.ID, last.Status, last.Attempts, lastErr)
}

func statusAfterRunnerBudgetExhaustion(status string, err error, out RunOutput) (string, error) {
	if !out.BudgetExhausted {
		return status, err
	}

	if err == nil {
		err = errors.New("subagent: budget exhausted by runner")
	}

	return StatusBudgetExhausted, err
}

func sleepBeforeRetry(ctx context.Context, backoff time.Duration) error {
	if backoff <= 0 {
		return nil
	}

	timer := time.NewTimer(backoff)
	defer timer.Stop()

	select {
	case <-timer.C:
		return nil
	case <-ctx.Done():
		return fmt.Errorf("retry backoff canceled: %w", ctx.Err())
	}
}

func shouldRetry(status string, attempt, maxAttempts int) bool {
	if attempt >= maxAttempts {
		return false
	}

	return status == StatusFailed || status == StatusTimedOut
}

func statusForAttempt(attemptErr, parentErr, err error, timeoutExceeded bool) string {
	if errors.Is(attemptErr, context.DeadlineExceeded) || timeoutExceeded {
		return StatusTimedOut
	}

	if errors.Is(parentErr, context.Canceled) || errors.Is(attemptErr, context.Canceled) {
		return StatusCanceled
	}

	if err == nil {
		return StatusSucceeded
	}

	return StatusFailed
}

func errorForStatus(status string, err error) error {
	if status == StatusTimedOut && err == nil {
		return context.DeadlineExceeded
	}

	if status == StatusCanceled && err == nil {
		return context.Canceled
	}

	return err
}

func resultFromError(request Request, ledger *ledgerStore, status string, attempts int, err error) Result {
	now := time.Now().UTC()

	return Result{
		StartedAt:  now,
		FinishedAt: now,
		Request:    request,
		Error:      err.Error(),
		Status:     status,
		LedgerPath: ledger.path(),
		Attempts:   attempts,
	}
}

func canceledBeforeStartResult(request Request, ledger *ledgerStore) Result {
	result := resultFromError(request, ledger, StatusCanceled, 0, context.Canceled)
	ignoreLedgerError(ledger.recordResult(result))

	return result
}

func validateRequests(requests []Request) error {
	seen := make(map[string]struct{}, len(requests))
	for i := range requests {
		request := requests[i]
		id := strings.TrimSpace(request.ID)
		if id == "" {
			return fmt.Errorf("subagent: request %d ID is required", i)
		}

		if strings.TrimSpace(request.Agent) == "" {
			return fmt.Errorf("subagent: request %q agent is required", request.ID)
		}

		if strings.TrimSpace(request.Prompt) == "" {
			return fmt.Errorf("subagent: request %q prompt is required", request.ID)
		}

		if _, exists := seen[id]; exists {
			return fmt.Errorf("subagent: duplicate request ID %q", id)
		}

		seen[id] = struct{}{}
	}

	return nil
}

func normalizeOptions(opts Options, requestCount int) Options {
	if opts.MaxConcurrency <= 0 {
		opts.MaxConcurrency = defaultMaxConcurrency
	}

	if requestCount > 0 && opts.MaxConcurrency > requestCount {
		opts.MaxConcurrency = requestCount
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

func applyRequestDefaults(requests []Request, opts Options) []Request {
	out := make([]Request, len(requests))
	needsIdentity := opts.LedgerPath != "" || opts.WorkspaceID != "" || opts.AllowedWriteScope != "" || opts.Model != "" || opts.Provider != ""
	needsEstimate := opts.LedgerPath != "" || opts.Budget.MaxPromptTokens > 0

	for i := range requests {
		request := requests[i]
		if needsIdentity && strings.TrimSpace(request.WorkspaceID) == "" {
			request.WorkspaceID = requestWorkspaceID(opts.WorkspaceID, request.ID)
		}

		if strings.TrimSpace(request.AllowedWriteScope) == "" {
			request.AllowedWriteScope = opts.AllowedWriteScope
		}

		if strings.TrimSpace(request.Model) == "" {
			request.Model = opts.Model
		}

		if strings.TrimSpace(request.Provider) == "" {
			request.Provider = opts.Provider
		}

		if needsEstimate && request.EstimatedPromptTokens <= 0 {
			request.EstimatedPromptTokens = estimatePromptTokens(request.Prompt)
		}

		out[i] = request
	}

	return out
}

func seedResumedResults(results []Result, requests []Request, ledger *ledgerStore, opts Options) []bool {
	skipped := make([]bool, len(requests))
	if !opts.Resume || ledger == nil {
		return skipped
	}

	for i := range requests {
		result, ok := ledger.succeeded(requests[i])
		if !ok {
			continue
		}

		result.Request = requests[i]
		result.Resumed = true
		result.Status = StatusSkipped
		result.LedgerPath = ledger.path()
		results[i] = result
		skipped[i] = true
		ignoreLedgerError(ledger.recordResult(result))
	}

	return skipped
}

func requestWorkspaceID(parentWorkspaceID, requestID string) string {
	parentWorkspaceID = strings.TrimSpace(parentWorkspaceID)
	requestID = strings.TrimSpace(requestID)
	if parentWorkspaceID == "" {
		return requestID
	}

	if requestID == "" {
		return parentWorkspaceID
	}

	return parentWorkspaceID + "/" + requestID
}

func estimatePromptTokens(prompt string) int {
	prompt = strings.TrimSpace(prompt)
	if prompt == "" {
		return 0
	}

	fields := strings.Fields(prompt)
	byWords := len(fields)
	byBytes := (len(prompt) + 3) / 4
	if byBytes > byWords {
		return byBytes
	}

	return byWords
}

func mergeUsage(reserved Usage, out RunOutput) Usage {
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

func commandExitStatus(err error) int {
	if err == nil {
		return 0
	}

	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) {
		return exitErr.ExitCode()
	}

	return -1
}

func childEnv(extra map[string]string, request Request) []string {
	envValues := map[string]string{}
	for key, value := range extra {
		key = strings.TrimSpace(key)
		if key != "" {
			envValues[key] = value
		}
	}

	addEnvIfSet(envValues, "ATTELER_CHILD_ID", request.ID)
	addEnvIfSet(envValues, "ATTELER_CHILD_AGENT", request.Agent)
	addEnvIfSet(envValues, "ATTELER_CHILD_WORKSPACE_ID", request.WorkspaceID)
	addEnvIfSet(envValues, "ATTELER_ALLOWED_WRITE_SCOPE", request.AllowedWriteScope)
	addEnvIfSet(envValues, "ATTELER_CHILD_MODEL", request.Model)
	addEnvIfSet(envValues, "ATTELER_CHILD_PROVIDER", request.Provider)

	if len(envValues) == 0 {
		return nil
	}

	env := os.Environ()
	keys := make([]string, 0, len(envValues))
	for key := range envValues {
		keys = append(keys, key)
	}
	sort.Strings(keys)

	for _, key := range keys {
		env = withoutEnvKey(env, key)
	}

	for _, key := range keys {
		value := envValues[key]
		env = append(env, key+"="+value)
	}

	return env
}

func withoutEnvKey(env []string, key string) []string {
	prefix := key + "="
	filtered := env[:0]
	for _, entry := range env {
		if !strings.HasPrefix(entry, prefix) {
			filtered = append(filtered, entry)
		}
	}

	return filtered
}

func addEnvIfSet(env map[string]string, key, value string) {
	value = strings.TrimSpace(value)
	if value != "" {
		env[key] = value
	}
}

func ignoreLedgerError(_ error) {}

type budgetTracker struct {
	mu               sync.Mutex
	maxOutputBytes   int64
	usedOutputBytes  int64
	maxCostMicros    int64
	usedCostMicros   int64
	maxPromptTokens  int
	usedPromptTokens int
}

func newBudgetTracker(budget Budget, ledger *ledgerStore) *budgetTracker {
	tracker := &budgetTracker{
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

func (b *budgetTracker) reserve(request Request) (Usage, error) {
	usage := Usage{
		PromptTokens:        request.EstimatedPromptTokens,
		EstimatedCostMicros: request.EstimatedCostMicros,
	}
	if b == nil {
		return usage, nil
	}

	b.mu.Lock()
	defer b.mu.Unlock()

	if b.maxOutputBytes > 0 && b.usedOutputBytes >= b.maxOutputBytes {
		return usage, fmt.Errorf("output byte budget exhausted: used %d of %d", b.usedOutputBytes, b.maxOutputBytes)
	}

	if b.maxPromptTokens > 0 && b.usedPromptTokens+usage.PromptTokens > b.maxPromptTokens {
		return usage, fmt.Errorf("prompt token budget exceeded: requested %d, used %d of %d", usage.PromptTokens, b.usedPromptTokens, b.maxPromptTokens)
	}

	if b.maxCostMicros > 0 && b.usedCostMicros+usage.EstimatedCostMicros > b.maxCostMicros {
		return usage, fmt.Errorf("cost budget exceeded: requested %d micros, used %d of %d", usage.EstimatedCostMicros, b.usedCostMicros, b.maxCostMicros)
	}

	b.usedPromptTokens += usage.PromptTokens
	b.usedCostMicros += usage.EstimatedCostMicros

	return usage, nil
}

func (b *budgetTracker) recordActual(reserved, actual Usage) error {
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

func (b *budgetTracker) remainingOutputBytes() int64 {
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
type ledgerStore struct {
	mu      sync.Mutex
	pathTo  string
	ledger  Ledger
	lastErr error
}

func openLedger(opts Options, requests []Request) (*ledgerStore, error) {
	path := strings.TrimSpace(opts.LedgerPath)
	if path == "" {
		if opts.Resume {
			return nil, errors.New("subagent: resume requires ledger path")
		}

		return nil, nil
	}

	ledger := Ledger{}
	if opts.Resume {
		loaded, err := loadLedger(path)
		if err != nil {
			return nil, err
		}

		ledger = loaded
	}

	now := time.Now().UTC()
	if opts.Resume {
		recoverRunningAttempts(&ledger, now)
	}

	if ledger.RunID == "" {
		ledger.RunID = newRunID(now)
	}

	if ledger.StartedAt.IsZero() {
		ledger.StartedAt = now
	}

	ledger.UpdatedAt = now
	ledger.Options = opts
	ledger.Requests = append([]Request(nil), requests...)

	store := &ledgerStore{pathTo: path, ledger: ledger}
	if err := store.saveLocked(); err != nil {
		return nil, err
	}

	return store, nil
}

func loadLedger(path string) (Ledger, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return Ledger{}, fmt.Errorf("subagent: resume ledger %s does not exist: %w", path, err)
		}

		return Ledger{}, fmt.Errorf("subagent: read ledger %s: %w", path, err)
	}

	var ledger Ledger
	if err := json.Unmarshal(data, &ledger); err != nil {
		return Ledger{}, fmt.Errorf("subagent: parse ledger %s: %w", path, err)
	}

	return ledger, nil
}

func recoverRunningAttempts(ledger *Ledger, now time.Time) {
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
			ledger.Attempts[i].Error = "subagent: recovered running attempt during resume"
		}
	}
}

func (s *ledgerStore) path() string {
	if s == nil {
		return ""
	}

	return s.pathTo
}

func (s *ledgerStore) completedAttempts() []Attempt {
	if s == nil {
		return nil
	}

	attempts := make([]Attempt, 0, len(s.ledger.Attempts))
	for i := range s.ledger.Attempts {
		attempt := s.ledger.Attempts[i]
		if attempt.Status == StatusSucceeded || attempt.Status == StatusFailed || attempt.Status == StatusTimedOut ||
			attempt.Status == StatusCanceled || attempt.Status == StatusBudgetExhausted {
			attempts = append(attempts, attempt)
		}
	}

	return attempts
}

func (s *ledgerStore) succeeded(request Request) (Result, bool) {
	if s == nil {
		return Result{}, false
	}

	var latest Result
	found := false
	for i := len(s.ledger.Results) - 1; i >= 0; i-- {
		result := s.ledger.Results[i]
		if sameRequestForResume(result.Request, request) {
			latest = result
			found = true
			break
		}
	}

	for i := len(s.ledger.Attempts) - 1; i >= 0; i-- {
		attempt := s.ledger.Attempts[i]
		if sameRequestForResume(attempt.Request, request) {
			result := resultFromAttempt(attempt)
			if !found || resultTime(result).After(resultTime(latest)) {
				latest = result
				found = true
			}

			break
		}
	}

	if found && (latest.Status == StatusSucceeded || latest.Status == StatusSkipped) {
		return latest, true
	}

	return Result{}, false
}

func resultFromAttempt(attempt Attempt) Result {
	return Result{
		StartedAt:      attempt.StartedAt,
		FinishedAt:     attempt.FinishedAt,
		Duration:       attempt.Duration,
		Request:        attempt.Request,
		Output:         attempt.Stdout,
		Stderr:         attempt.Stderr,
		Error:          attempt.Error,
		Status:         attempt.Status,
		TranscriptPath: attempt.TranscriptPath,
		Artifacts:      append([]string(nil), attempt.Artifacts...),
		Usage:          attempt.Usage,
		Attempts:       attempt.Attempt,
		ExitStatus:     attempt.ExitStatus,
	}
}

func resultTime(result Result) time.Time {
	if !result.FinishedAt.IsZero() {
		return result.FinishedAt
	}

	return result.StartedAt
}

func sameRequestForResume(previous, current Request) bool {
	return previous.ID == current.ID &&
		previous.Agent == current.Agent &&
		previous.Prompt == current.Prompt &&
		previous.WorkspaceID == current.WorkspaceID &&
		previous.AllowedWriteScope == current.AllowedWriteScope &&
		previous.Model == current.Model &&
		previous.Provider == current.Provider &&
		previous.EstimatedPromptTokens == current.EstimatedPromptTokens &&
		previous.EstimatedCostMicros == current.EstimatedCostMicros
}

func (s *ledgerStore) startAttempt(attempt Attempt) int {
	if s == nil {
		return -1
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	s.ledger.Attempts = append(s.ledger.Attempts, attempt)
	index := len(s.ledger.Attempts) - 1
	ignoreLedgerError(s.saveLocked())

	return index
}

func (s *ledgerStore) appendAttempt(attempt Attempt) error {
	if s == nil {
		return nil
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	s.ledger.Attempts = append(s.ledger.Attempts, attempt)

	return s.saveLocked()
}

func (s *ledgerStore) finishAttempt(index int, attempt Attempt) error {
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

func (s *ledgerStore) recordResult(result Result) error {
	if s == nil {
		return nil
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	for i := range s.ledger.Results {
		if sameRequestForResume(s.ledger.Results[i].Request, result.Request) {
			s.ledger.Results[i] = result
			return s.saveLocked()
		}
	}

	s.ledger.Results = append(s.ledger.Results, result)

	return s.saveLocked()
}

func (s *ledgerStore) writeTranscript(id string, attempt int, out RunOutput, err error) string {
	if s == nil || s.pathTo == "" {
		return ""
	}

	dir := filepath.Join(filepath.Dir(s.pathTo), "transcripts")
	if mkdirErr := os.MkdirAll(dir, 0o750); mkdirErr != nil {
		s.recordError(fmt.Errorf("subagent: create transcript dir: %w", mkdirErr))
		return ""
	}

	path := filepath.Join(dir, safeFileName(id)+fmt.Sprintf("-attempt-%d-%s.txt", attempt, newRunID(time.Now().UTC())))
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
		s.recordError(fmt.Errorf("subagent: write transcript %s: %w", path, writeErr))
		return ""
	}

	return path
}

func (s *ledgerStore) recordError(err error) {
	if s == nil || err == nil {
		return
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	s.lastErr = errors.Join(s.lastErr, err)
}

func (s *ledgerStore) rememberLocked(err error) error {
	if err != nil {
		s.lastErr = errors.Join(s.lastErr, err)
	}

	return err
}

func (s *ledgerStore) ledgerError() error {
	if s == nil {
		return nil
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	return s.lastErr
}

func (s *ledgerStore) saveLocked() error {
	if s == nil || s.pathTo == "" {
		return nil
	}

	s.ledger.UpdatedAt = time.Now().UTC()

	if err := os.MkdirAll(filepath.Dir(s.pathTo), 0o750); err != nil {
		return s.rememberLocked(fmt.Errorf("subagent: create ledger dir: %w", err))
	}

	data, err := json.MarshalIndent(s.ledger, "", "  ")
	if err != nil {
		return s.rememberLocked(fmt.Errorf("subagent: marshal ledger: %w", err))
	}

	data = append(data, '\n')
	tmp, err := os.CreateTemp(filepath.Dir(s.pathTo), ".ledger-*.json")
	if err != nil {
		return s.rememberLocked(fmt.Errorf("subagent: create ledger temp: %w", err))
	}

	tmpPath := tmp.Name()
	defer os.Remove(tmpPath)

	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		return s.rememberLocked(fmt.Errorf("subagent: write ledger temp: %w", err))
	}

	if err := tmp.Close(); err != nil {
		return s.rememberLocked(fmt.Errorf("subagent: close ledger temp: %w", err))
	}

	if err := os.Rename(tmpPath, s.pathTo); err != nil {
		return s.rememberLocked(fmt.Errorf("subagent: replace ledger %s: %w", s.pathTo, err))
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

func safeFileName(value string) string {
	value = strings.TrimSpace(value)
	if value == "" {
		return "child"
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
