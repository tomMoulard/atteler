package watch

import (
	"context"
	"errors"
	"reflect"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestRun_RepeatsScansWithIterationMetadata(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	started := time.Date(2026, 5, 2, 9, 0, 0, 0, time.UTC)
	nowCalls := 0
	scanCalls := 0

	var waited []time.Duration

	results, err := Run(context.Background(), root, RunOptions{
		Interval:      5 * time.Second,
		MaxIterations: 2,
		Scan: func(ctx context.Context, gotRoot string, options Options) ([]Finding, error) {
			require.NoError(t, ctx.Err())
			assert.Equal(t, root, gotRoot)
			assert.Equal(t, int64(128), options.LargeFileBytes)

			scanCalls++

			return []Finding{{Path: "file.go", Kind: KindMissingTest, Severity: SeverityInfo}}, nil
		},
		ScanOptions: Options{LargeFileBytes: 128},
		Now: func() time.Time {
			defer func() { nowCalls++ }()
			return started.Add(time.Duration(nowCalls) * time.Second)
		},
		Wait: func(ctx context.Context, interval time.Duration) error {
			waited = append(waited, interval)
			return ctx.Err()
		},
	})

	require.NoError(t, err)
	require.Len(t, results, 2)
	assert.Equal(t, 2, scanCalls)
	assert.Equal(t, []time.Duration{5 * time.Second}, waited)
	assert.Equal(t, 1, results[0].Iteration)
	assert.Equal(t, 2, results[1].Iteration)
	assert.Equal(t, started, results[0].StartedAt)
	assert.Equal(t, started.Add(time.Second), results[0].FinishedAt)
	assert.Equal(t, time.Second, results[0].Duration)
	assert.Equal(t, time.Second, results[1].Duration)
	assert.Equal(t, KindMissingTest, results[0].Findings[0].Kind)
}

func TestRun_StopsWhenContextCanceledDuringWait(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithCancel(context.Background())

	var scanCalls int

	results, err := Run(ctx, t.TempDir(), RunOptions{
		MaxIterations: 0,
		Scan: func(context.Context, string, Options) ([]Finding, error) {
			scanCalls++
			return nil, nil
		},
		Wait: func(context.Context, time.Duration) error {
			cancel()
			return context.Canceled
		},
	})

	require.ErrorIs(t, err, context.Canceled)
	assert.Len(t, results, 1)
	assert.Equal(t, 1, scanCalls)
}

func TestRun_ReturnsPartialResultsWhenScanFails(t *testing.T) {
	t.Parallel()

	scanErr := errors.New("boom")
	results, err := Run(context.Background(), t.TempDir(), RunOptions{
		MaxIterations: 3,
		Scan: func(context.Context, string, Options) ([]Finding, error) {
			return []Finding{{Path: "bad.go", Kind: KindMissingTest}}, scanErr
		},
	})

	require.ErrorIs(t, err, scanErr)
	require.Len(t, results, 1)
	assert.Equal(t, 1, results[0].Iteration)
	assert.Equal(t, "bad.go", results[0].Findings[0].Path)
}

func TestRun_AttachesBaselineComparisonAndGate(t *testing.T) {
	t.Parallel()

	existing := testFinding("pkg/existing.go", KindConventionDrift, SeverityHigh)
	newFinding := testFinding("pkg/new.go", KindConventionDrift, SeverityHigh)

	results, err := Run(context.Background(), t.TempDir(), RunOptions{
		MaxIterations: 1,
		Baseline:      &Baseline{Findings: []Finding{existing}},
		Gate:          GateOptions{Enabled: true},
		Scan: func(context.Context, string, Options) ([]Finding, error) {
			return []Finding{existing, newFinding}, nil
		},
	})
	require.NoError(t, err)
	require.Len(t, results, 1)

	require.NotNil(t, results[0].Comparison)
	assert.Equal(t, TrendMetrics{New: 1, Fixed: 0, Unchanged: 1, Suppressed: 0, Unstable: 0}, results[0].Comparison.Metrics)
	require.NotNil(t, results[0].Gate)
	assert.False(t, results[0].Gate.Passed)
	assert.Equal(t, []string{"pkg/new.go"}, findingPaths(results[0].Gate.BlockingFindings))
}

func TestRun_StopsOnGateFailureWhenConfigured(t *testing.T) {
	t.Parallel()

	newFinding := testFinding("pkg/new.go", KindConventionDrift, SeverityHigh)

	results, err := Run(context.Background(), t.TempDir(), RunOptions{
		MaxIterations:     3,
		Gate:              GateOptions{Enabled: true},
		StopOnGateFailure: true,
		Scan: func(context.Context, string, Options) ([]Finding, error) {
			return []Finding{newFinding}, nil
		},
	})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "watch gate")
	require.Len(t, results, 1)
	require.NotNil(t, results[0].Gate)
	assert.False(t, results[0].Gate.Passed)
}

func TestRun_UpsertsIssueBeforeStoppingOnGateFailure(t *testing.T) {
	t.Parallel()

	tracker := newFakeIssueTracker()
	newFinding := testFinding("pkg/new.go", KindConventionDrift, SeverityHigh)

	results, err := Run(context.Background(), t.TempDir(), RunOptions{
		MaxIterations:     3,
		Gate:              GateOptions{Enabled: true},
		IssueTracker:      tracker,
		StopOnGateFailure: true,
		Scan: func(context.Context, string, Options) ([]Finding, error) {
			return []Finding{newFinding}, nil
		},
	})
	require.Error(t, err)
	require.Len(t, results, 1)

	require.NotNil(t, results[0].Gate)
	assert.False(t, results[0].Gate.Passed)
	require.Len(t, results[0].Issues, 1)
	assert.Equal(t, IssueActionCreated, results[0].Issues[0].Action)
	assert.Equal(t, newFinding.Fingerprint, results[0].Issues[0].Finding.Fingerprint)
	assert.Equal(t, 1, tracker.createCalls)
	assert.Equal(t, 0, tracker.updateCalls)
}

func TestRun_UpsertsIssuesForNewActionableFindings(t *testing.T) {
	t.Parallel()

	tracker := newFakeIssueTracker()
	existing := testFinding("pkg/existing.go", KindConventionDrift, SeverityHigh)
	newFinding := testFinding("pkg/new.go", KindConventionDrift, SeverityHigh)

	results, err := Run(context.Background(), t.TempDir(), RunOptions{
		MaxIterations: 2,
		Baseline:      &Baseline{Findings: []Finding{existing}},
		IssueTracker:  tracker,
		IssueOptions: IssueOptions{
			Labels: []string{"quality", "watch"},
		},
		Scan: func(context.Context, string, Options) ([]Finding, error) {
			return []Finding{existing, newFinding}, nil
		},
		Wait: func(context.Context, time.Duration) error {
			return nil
		},
	})
	require.NoError(t, err)
	require.Len(t, results, 2)

	require.Len(t, results[0].Issues, 1)
	assert.Equal(t, IssueActionCreated, results[0].Issues[0].Action)
	assert.Equal(t, newFinding.Fingerprint, results[0].Issues[0].Finding.Fingerprint)
	assert.Equal(t, []string{"quality", "watch"}, tracker.drafts[newFinding.Fingerprint].Labels)

	require.Len(t, results[1].Issues, 1)
	assert.Equal(t, IssueActionUpdated, results[1].Issues[0].Action)
	assert.Equal(t, 1, tracker.createCalls)
	assert.Equal(t, 1, tracker.updateCalls)
}

func TestRun_TracksUnstableFindingsAcrossIterations(t *testing.T) {
	t.Parallel()

	flaky := testFinding("pkg/flaky.go", KindConventionDrift, SeverityHigh)
	scans := [][]Finding{
		{flaky},
		nil,
		{flaky},
	}
	scanCalls := 0

	results, err := Run(context.Background(), t.TempDir(), RunOptions{
		MaxIterations: 3,
		Baseline:      &Baseline{},
		Scan: func(context.Context, string, Options) ([]Finding, error) {
			defer func() { scanCalls++ }()
			return scans[scanCalls], nil
		},
		Wait: func(context.Context, time.Duration) error {
			return nil
		},
	})
	require.NoError(t, err)
	require.Len(t, results, 3)

	assert.Equal(t, 0, results[0].Comparison.Metrics.Unstable)
	assert.Equal(t, 0, results[1].Comparison.Metrics.Unstable)
	assert.Empty(t, results[1].Comparison.UnstableFindings)
	assert.Equal(t, 1, results[2].Comparison.Metrics.Unstable)
	assert.Equal(t, []string{"pkg/flaky.go"}, findingPaths(results[2].Comparison.UnstableFindings))
}

func TestRun_KeepsBaselineFixesOutOfUnstableTrend(t *testing.T) {
	t.Parallel()

	existing := testFinding("pkg/existing.go", KindConventionDrift, SeverityHigh)
	scans := [][]Finding{
		{existing},
		nil,
	}
	scanCalls := 0

	results, err := Run(context.Background(), t.TempDir(), RunOptions{
		MaxIterations: 2,
		Baseline:      &Baseline{Findings: []Finding{existing}},
		Scan: func(context.Context, string, Options) ([]Finding, error) {
			defer func() { scanCalls++ }()
			return scans[scanCalls], nil
		},
		Wait: func(context.Context, time.Duration) error {
			return nil
		},
	})
	require.NoError(t, err)
	require.Len(t, results, 2)

	assert.Equal(t, TrendMetrics{New: 0, Fixed: 0, Unchanged: 1, Suppressed: 0, Unstable: 0}, results[0].Comparison.Metrics)
	assert.Equal(t, TrendMetrics{New: 0, Fixed: 1, Unchanged: 0, Suppressed: 0, Unstable: 0}, results[1].Comparison.Metrics)
	assert.Equal(t, []string{"pkg/existing.go"}, findingPaths(results[1].Comparison.FixedFindings))
	assert.Empty(t, results[1].Comparison.UnstableFindings)
}

func TestRun_ReevaluatesGateAfterUnstableTrendTracking(t *testing.T) {
	t.Parallel()

	flaky := testFinding("pkg/flaky.go", KindConventionDrift, SeverityHigh)
	scans := [][]Finding{
		{flaky},
		nil,
		{flaky},
	}
	scanCalls := 0

	results, err := Run(context.Background(), t.TempDir(), RunOptions{
		MaxIterations: 3,
		Gate:          GateOptions{Enabled: true},
		Scan: func(context.Context, string, Options) ([]Finding, error) {
			defer func() { scanCalls++ }()
			return scans[scanCalls], nil
		},
		Wait: func(context.Context, time.Duration) error {
			return nil
		},
	})
	require.NoError(t, err)
	require.Len(t, results, 3)

	require.NotNil(t, results[0].Gate)
	assert.False(t, results[0].Gate.Passed)
	require.NotNil(t, results[2].Comparison)
	assert.Equal(t, TrendMetrics{New: 0, Fixed: 0, Unchanged: 0, Suppressed: 0, Unstable: 1}, results[2].Comparison.Metrics)
	require.NotNil(t, results[2].Gate)
	assert.True(t, results[2].Gate.Passed)
	assert.Empty(t, results[2].Gate.BlockingFindings)
}

func TestRun_ValidatesOptions(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		ctx     context.Context
		root    string
		options RunOptions
		wantErr string
	}{
		{
			name:    "nil context",
			root:    t.TempDir(),
			wantErr: "nil context",
		},
		{
			name:    "empty root",
			ctx:     context.Background(),
			wantErr: "root is required",
		},
		{
			name:    "negative interval",
			ctx:     context.Background(),
			root:    t.TempDir(),
			options: RunOptions{Interval: -time.Second},
			wantErr: "interval must be non-negative",
		},
		{
			name:    "negative max iterations",
			ctx:     context.Background(),
			root:    t.TempDir(),
			options: RunOptions{MaxIterations: -1},
			wantErr: "max iterations must be non-negative",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			_, err := Run(test.ctx, test.root, test.options)
			require.Error(t, err)
			assert.Contains(t, err.Error(), test.wantErr)
		})
	}
}

func TestRun_DefaultScannerUsesScanWithOptions(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	writeFile(t, root, "todo.txt", "TODO: scan me\n")

	results, err := Run(context.Background(), root, RunOptions{MaxIterations: 1})

	require.NoError(t, err)
	require.Len(t, results, 1)
	got := findingKeys(results[0].Findings)

	want := []string{"todo.txt|stale_todo|maintenance"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("findings = %#v, want %#v", got, want)
	}
}
