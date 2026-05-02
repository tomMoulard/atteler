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
