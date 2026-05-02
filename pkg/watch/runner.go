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
	Iteration  int
	StartedAt  time.Time
	FinishedAt time.Time
	Duration   time.Duration
	Findings   []Finding
}

// Run repeatedly scans root until context cancellation, scan failure, or the
// configured maximum iteration count is reached.
func Run(ctx context.Context, root string, options RunOptions) ([]IterationResult, error) {
	options, err := normalizeRunOptions(ctx, root, options)
	if err != nil {
		return nil, err
	}

	results := make([]IterationResult, 0, resultCapacity(options.MaxIterations))
	for iteration := 1; options.MaxIterations == 0 || iteration <= options.MaxIterations; iteration++ {
		if err := ctx.Err(); err != nil {
			return results, fmt.Errorf("watch run: context before iteration %d: %w", iteration, err)
		}

		startedAt := options.Now()
		findings, err := options.Scan(ctx, root, options.ScanOptions)
		finishedAt := options.Now()
		results = append(results, IterationResult{
			Iteration:  iteration,
			StartedAt:  startedAt,
			FinishedAt: finishedAt,
			Duration:   finishedAt.Sub(startedAt),
			Findings:   append([]Finding(nil), findings...),
		})
		if err != nil {
			return results, fmt.Errorf("watch iteration %d: %w", iteration, err)
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
