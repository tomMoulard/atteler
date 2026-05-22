package review

import (
	"context"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type fakeReviewCompleter struct {
	calls []string
	mu    sync.Mutex
}

func (f *fakeReviewCompleter) Complete(_ context.Context, reviewer, systemPrompt, _ string) (string, error) {
	f.mu.Lock()
	f.calls = append(f.calls, reviewer)
	f.mu.Unlock()

	switch {
	case reviewer == reviewJudgeName:
		return "FINDING: high|correctness|pkg/auth.go|12|nil token can panic|guard token before use" + "\n" +
			"GATE tests pass: PASS covered by regression", nil
	case strings.Contains(systemPrompt, "cross-reviewing"):
		return "challenge: keep the panic finding and require a regression test", nil
	default:
		return "FINDING: medium|tests|pkg/auth_test.go|0|missing nil token coverage|add a regression test" + "\n" +
			"GATE tests pass: PASS reviewer checked tests", nil
	}
}

func TestRunWithLLM_ExecutesReviewRoundsAndAggregatesVerdict(t *testing.T) {
	t.Parallel()

	plan, err := NewPlan(
		[]Reviewer{
			{Name: "quality", Categories: []Category{CategoryCorrectness}},
			{Name: "tests", Categories: []Category{CategoryTests}},
		},
		[]string{"pkg/auth.go"},
		[]string{"tests pass"},
	)
	require.NoError(t, err)

	completer := &fakeReviewCompleter{}
	result, err := RunWithLLM(t.Context(), plan, completer, "diff -- pkg/auth.go")
	require.NoError(t, err)

	assert.Equal(t, "aggregate-verdict", result.Report.Reviewer)
	require.Len(t, result.Report.Findings, 1)
	assert.Equal(t, SeverityHigh, result.Report.Findings[0].Severity)
	assert.Equal(t, "pkg/auth.go", result.Report.Findings[0].Path)
	assert.Len(t, result.Session.Reports, 2)
	assert.Len(t, result.Session.CrossReviews, 2)
	assert.Equal(t, "tests pass", result.Report.GateChecks[0].Name)
	assert.True(t, result.Report.GateChecks[0].Passed)

	completer.mu.Lock()
	defer completer.mu.Unlock()

	assert.Contains(t, completer.calls, "quality")
	assert.Contains(t, completer.calls, "tests")
	assert.Contains(t, completer.calls, reviewJudgeName)
}

func TestRunWithLLM_ReturnsPartialSessionWhenVerdictGateFails(t *testing.T) {
	t.Parallel()

	plan, err := NewPlan(
		[]Reviewer{{Name: "quality", Categories: []Category{CategoryCorrectness}}},
		[]string{"pkg/auth.go"},
		[]string{"tests pass"},
	)
	require.NoError(t, err)

	completer := staticReviewCompleter(func(reviewer string) string {
		if reviewer == reviewJudgeName {
			return "GATE tests pass: FAIL tests were not run"
		}

		return "GATE tests pass: PASS independent reviewer"
	})

	result, err := RunWithLLM(t.Context(), plan, completer, "diff -- pkg/auth.go")
	require.Error(t, err)
	assert.Contains(t, err.Error(), `gate check "tests pass" failed`)
	assert.Len(t, result.Session.Reports, 1)
	assert.Equal(t, "aggregate-verdict", result.Session.Verdict.Reviewer)
}

func TestRunWithLLM_ReturnsPartialSessionWhenVerdictOmitsRequiredGate(t *testing.T) {
	t.Parallel()

	plan, err := NewPlan(
		[]Reviewer{{Name: "quality", Categories: []Category{CategoryCorrectness}}},
		[]string{"pkg/auth.go"},
		[]string{"tests pass"},
	)
	require.NoError(t, err)

	completer := staticReviewCompleter(func(reviewer string) string {
		if reviewer == reviewJudgeName {
			return "FINDING: high|correctness|pkg/auth.go|12|nil token can panic|guard token before use"
		}

		return "GATE tests pass: PASS independent reviewer"
	})

	result, err := RunWithLLM(t.Context(), plan, completer, "diff -- pkg/auth.go")
	require.Error(t, err)
	assert.Contains(t, err.Error(), `missing gate check "tests pass"`)
	assert.Len(t, result.Session.Reports, 1)
	assert.Equal(t, "aggregate-verdict", result.Session.Verdict.Reviewer)
	assert.Empty(t, result.Session.Verdict.GateChecks)
}

func TestRunWithLLM_RequiresCompleterAndContext(t *testing.T) {
	t.Parallel()

	plan, err := NewPlan([]Reviewer{{Name: "quality"}}, []string{"pkg/auth.go"}, []string{"tests pass"})
	require.NoError(t, err)

	_, err = RunWithLLM(t.Context(), plan, nil, "diff")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "LLM completer is required")

	_, err = RunWithLLM(t.Context(), plan, staticReviewCompleter(func(string) string { return "" }), " ")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "review context is required")
}

func TestRun_StopsBeforeStartingWhenContextCanceled(t *testing.T) {
	t.Parallel()

	plan, err := NewPlan([]Reviewer{{Name: "quality"}}, []string{"pkg/auth.go"}, []string{"tests pass"})
	require.NoError(t, err)

	ctx, cancel := context.WithCancel(t.Context())
	cancel()

	var called atomic.Bool

	_, err = Run(ctx, plan, Runner{
		Review: func(context.Context, Reviewer) (Report, error) {
			called.Store(true)
			return Report{}, nil
		},
		CrossReview: func(context.Context, CrossReview, Report) (string, error) {
			called.Store(true)
			return "", nil
		},
		Aggregate: func(context.Context, Session) (Report, error) {
			called.Store(true)
			return Report{}, nil
		},
	})

	require.Error(t, err)
	require.ErrorIs(t, err, context.Canceled)
	assert.False(t, called.Load())
}

func TestRun_StopsBeforeCrossReviewsWhenContextCanceledAfterReports(t *testing.T) {
	t.Parallel()

	plan, err := NewPlan(
		[]Reviewer{{Name: "quality"}, {Name: "tests"}},
		[]string{"pkg/auth.go"},
		[]string{"tests pass"},
	)
	require.NoError(t, err)

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()

	var (
		crossReviewCalled atomic.Bool
		aggregateCalled   atomic.Bool
	)

	result, err := Run(ctx, plan, Runner{
		Review: func(_ context.Context, reviewer Reviewer) (Report, error) {
			cancel()

			return Report{
				Reviewer: reviewer.Name,
				GateChecks: []GateCheck{{
					Name:   "tests pass",
					Passed: true,
				}},
			}, nil
		},
		CrossReview: func(context.Context, CrossReview, Report) (string, error) {
			crossReviewCalled.Store(true)
			return "should not run", nil
		},
		Aggregate: func(context.Context, Session) (Report, error) {
			aggregateCalled.Store(true)
			return Report{}, nil
		},
	})

	require.Error(t, err)
	require.ErrorIs(t, err, context.Canceled)
	require.Len(t, result.Session.Reports, 2)
	assert.False(t, crossReviewCalled.Load())
	assert.False(t, aggregateCalled.Load())
}

func TestParseReportFromLLM_NormalizesFindingsAndPreservesOnlyExplicitGates(t *testing.T) {
	t.Parallel()

	report := parseReportFromLLM(strings.Join([]string{
		"FINDING: urgent|unknown|pkg/auth.go|abc|message|suggestion",
		"FINDING: high|tests||1|missing path|skip",
		"GATE lint pass: PASS clean",
	}, "\n"), "judge")

	require.Len(t, report.Findings, 1)
	assert.Equal(t, SeverityInfo, report.Findings[0].Severity)
	assert.Equal(t, CategoryMaintainability, report.Findings[0].Category)
	assert.Zero(t, report.Findings[0].Line)
	assert.Equal(t, []GateCheck{
		{Name: "lint pass", Passed: true, Notes: "clean"},
	}, report.GateChecks)
}

type staticReviewCompleter func(reviewer string) string

func (f staticReviewCompleter) Complete(_ context.Context, reviewer, _, _ string) (string, error) {
	return f(reviewer), nil
}
