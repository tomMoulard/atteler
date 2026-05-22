package watch

import (
	"context"
	"errors"
	"fmt"
	"time"
)

const (
	// DefaultRunInterval is the default delay between background scan iterations.
	DefaultRunInterval = time.Minute
)

// ScanFunc is the scanner signature used by RunOptions for tests and custom runners.
type ScanFunc func(context.Context, string, Options) ([]Finding, error)

// RunOptions configures the continuous watch runner.
//
//nolint:govet // Public field order groups scan settings before test hooks.
type RunOptions struct {
	// ScanOptions are passed to each scan iteration.
	ScanOptions Options
	// Baseline is an optional branch-point or acknowledged-debt snapshot used
	// to classify new, fixed, unchanged, and suppressed findings.
	Baseline *Baseline
	// Gate optionally evaluates new findings against a severity threshold.
	Gate GateOptions
	// IssueTracker optionally creates or updates tracker issues for new,
	// unsuppressed findings that meet IssueOptions.MinSeverity. Without a
	// Baseline, all current findings are treated as new.
	IssueTracker IssueTracker
	// IssueOptions controls which new findings become issue upserts when
	// IssueTracker is configured.
	IssueOptions IssueOptions
	// StopOnGateFailure stops the loop after recording the first failed gate.
	StopOnGateFailure bool
	// Interval is the delay between iterations. Zero uses DefaultRunInterval.
	Interval time.Duration
	// MaxIterations limits the number of scans. Zero means run until context cancellation.
	MaxIterations int

	// Scan overrides the scan implementation. Nil uses ScanWithOptions.
	Scan ScanFunc
	// Now overrides the time source. Nil uses time.Now.
	Now func() time.Time
	// Wait overrides interval waiting. Nil uses a context-aware timer.
	Wait func(context.Context, time.Duration) error
}

// IterationResult describes one completed watch runner scan iteration.
//
//nolint:govet // Public field order follows iteration lifecycle.
type IterationResult struct {
	Comparison *Comparison
	Gate       *GateResult
	Iteration  int
	StartedAt  time.Time
	FinishedAt time.Time
	Duration   time.Duration
	Findings   []Finding
	Issues     []IssueUpsertResult
}

// Run repeatedly scans root until context cancellation, scan failure, or the
// configured maximum iteration count is reached.
func Run(ctx context.Context, root string, options RunOptions) ([]IterationResult, error) {
	options, err := normalizeRunOptions(ctx, root, options)
	if err != nil {
		return nil, err
	}

	results := make([]IterationResult, 0, resultCapacity(options.MaxIterations))
	trends := runTrendTracker{}

	for iteration := 1; options.MaxIterations == 0 || iteration <= options.MaxIterations; iteration++ {
		if err := ctx.Err(); err != nil {
			return results, fmt.Errorf("watch run: context before iteration %d: %w", iteration, err)
		}

		result, err := runWatchIteration(ctx, root, options, &trends, iteration)

		results = append(results, result)
		if err != nil {
			return results, err
		}

		if result.Gate != nil && !result.Gate.Passed && options.StopOnGateFailure {
			return results, fmt.Errorf("watch gate %q failed on iteration %d: %s", result.Gate.Name, iteration, result.Gate.Reason)
		}

		if err := ctx.Err(); err != nil {
			return results, fmt.Errorf("watch run: context after iteration %d: %w", iteration, err)
		}

		if options.MaxIterations > 0 && iteration == options.MaxIterations {
			return results, nil
		}

		if err := options.Wait(ctx, options.Interval); err != nil {
			if ctxErr := ctx.Err(); ctxErr != nil {
				return results, fmt.Errorf("watch run: context while waiting after iteration %d: %w", iteration, ctxErr)
			}

			return results, fmt.Errorf("wait for next watch iteration: %w", err)
		}
	}

	return results, nil
}

func runWatchIteration(ctx context.Context, root string, options RunOptions, trends *runTrendTracker, iteration int) (IterationResult, error) {
	startedAt := options.Now()
	findings, err := options.Scan(ctx, root, options.ScanOptions)
	finishedAt := options.Now()
	comparison, gate := compareRunFindings(options, findings)
	trends.mark(comparison, findings)

	if comparison != nil && options.Gate.Enabled {
		updatedGate := EvaluateGate(*comparison, options.Gate)
		gate = &updatedGate
	}

	result := IterationResult{
		Comparison: comparison,
		Gate:       gate,
		Iteration:  iteration,
		StartedAt:  startedAt,
		FinishedAt: finishedAt,
		Duration:   finishedAt.Sub(startedAt),
		Findings:   append([]Finding(nil), findings...),
	}
	if err != nil {
		return result, fmt.Errorf("watch iteration %d: %w", iteration, err)
	}

	if err := addIssueUpserts(ctx, options, comparison, &result); err != nil {
		return result, fmt.Errorf("watch iteration %d issue upsert: %w", iteration, err)
	}

	return result, nil
}

func addIssueUpserts(ctx context.Context, options RunOptions, comparison *Comparison, result *IterationResult) error {
	if comparison == nil || options.IssueTracker == nil {
		return nil
	}

	issues, err := UpsertIssues(ctx, options.IssueTracker, *comparison, options.IssueOptions)
	result.Issues = append([]IssueUpsertResult(nil), issues...)

	return err
}

func normalizeRunOptions(ctx context.Context, root string, options RunOptions) (RunOptions, error) {
	if ctx == nil {
		return RunOptions{}, errors.New("watch run: nil context")
	}

	if root == "" {
		return RunOptions{}, errors.New("watch run: root is required")
	}

	if options.Interval < 0 {
		return RunOptions{}, fmt.Errorf("watch run: interval must be non-negative: %s", options.Interval)
	}

	if options.Interval == 0 {
		options.Interval = DefaultRunInterval
	}

	if options.MaxIterations < 0 {
		return RunOptions{}, fmt.Errorf("watch run: max iterations must be non-negative: %d", options.MaxIterations)
	}

	if options.Scan == nil {
		options.Scan = defaultScanFunc
	}

	if options.Now == nil {
		options.Now = time.Now
	}

	if options.Wait == nil {
		options.Wait = waitWithContext
	}

	return options, nil
}

func defaultScanFunc(ctx context.Context, root string, options Options) ([]Finding, error) {
	if err := ctx.Err(); err != nil {
		return nil, fmt.Errorf("watch scan: context before scan: %w", err)
	}

	findings, err := ScanWithOptions(root, options)
	if err != nil {
		return nil, err
	}

	if err := ctx.Err(); err != nil {
		return nil, fmt.Errorf("watch scan: context after scan: %w", err)
	}

	return findings, nil
}

func compareRunFindings(options RunOptions, findings []Finding) (*Comparison, *GateResult) {
	if options.Baseline == nil && !options.Gate.Enabled && options.IssueTracker == nil {
		return nil, nil
	}

	var baseline []Finding
	if options.Baseline != nil {
		baseline = options.Baseline.Findings
	}

	comparison := CompareFindings(baseline, findings)
	if !options.Gate.Enabled {
		return &comparison, nil
	}

	gate := EvaluateGate(comparison, options.Gate)

	return &comparison, &gate
}

type runTrendTracker struct {
	previous    map[string]Finding
	everSeen    map[string]Finding
	unstable    map[string]Finding
	initialized bool
}

func (t *runTrendTracker) mark(comparison *Comparison, findings []Finding) {
	if comparison == nil {
		return
	}

	current, _ := indexFindings(findings)
	if !t.initialized {
		t.previous = current
		t.everSeen = cloneFindingIndex(current)
		t.unstable = make(map[string]Finding)
		t.initialized = true

		return
	}

	t.markReappearedFindings(current)
	t.previous = current
	t.mergeInto(comparison)
}

func (t *runTrendTracker) markReappearedFindings(current map[string]Finding) {
	for fingerprint := range current {
		finding := current[fingerprint]
		if _, ok := t.previous[fingerprint]; !ok {
			if _, seen := t.everSeen[fingerprint]; seen {
				t.unstable[fingerprint] = finding
			}
		}

		t.everSeen[fingerprint] = finding
	}
}

func (t *runTrendTracker) mergeInto(comparison *Comparison) {
	seen := make(map[string]struct{}, len(comparison.UnstableFindings)+len(t.unstable))
	merged := make([]Finding, 0, len(comparison.UnstableFindings)+len(t.unstable))

	for i := range comparison.UnstableFindings {
		finding := completeFindingIdentity(comparison.UnstableFindings[i])
		merged = append(merged, finding)
		seen[finding.Fingerprint] = struct{}{}
	}

	for fingerprint := range t.unstable {
		if _, ok := seen[fingerprint]; ok {
			continue
		}

		merged = append(merged, t.unstable[fingerprint])
	}

	sortFindings(merged)
	comparison.UnstableFindings = merged
	comparison.removeUnstableFromClassifiedFindings()
	comparison.recalculateMetrics()
}

func cloneFindingIndex(index map[string]Finding) map[string]Finding {
	clone := make(map[string]Finding, len(index))
	for fingerprint := range index {
		clone[fingerprint] = index[fingerprint]
	}

	return clone
}

func waitWithContext(ctx context.Context, interval time.Duration) error {
	timer := time.NewTimer(interval)
	defer timer.Stop()

	select {
	case <-ctx.Done():
		return fmt.Errorf("watch wait: %w", ctx.Err())
	case <-timer.C:
		return nil
	}
}

func resultCapacity(maxIterations int) int {
	if maxIterations > 0 {
		return maxIterations
	}

	return 0
}
