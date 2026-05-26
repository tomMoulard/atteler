package review

import (
	"context"
	"encoding/json"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const testReviewContext = `Review instructions:
focus on auth safety

<configured_references>
<file source="pkg/auth.go">
package auth
func token() string { return "" }
func useToken() {}
</file>
<file source="pkg/auth_test.go">
package auth
func TestToken(t *testing.T) {}
</file>
</configured_references>

Command output:
go test ./... PASS
make lint PASS
`

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
		return llmReportJSON(
			"aggregate-verdict",
			[]map[string]any{
				validLLMFinding("quality", "pkg/auth.go", 2, SeverityHigh, CategoryCorrectness),
			},
			[]map[string]any{
				validLLMGate("tests pass", true, "go test ./... PASS", ""),
			},
		), nil
	case strings.Contains(systemPrompt, "cross-reviewing"):
		return llmCrossReviewJSON(
			"keep the panic finding and require a regression test",
			[]map[string]any{
				validLLMCrossReviewChallenge("pkg/auth.go:2 nil token can panic", "support"),
			},
		), nil
	default:
		return llmReportJSON(
			reviewer,
			[]map[string]any{
				validLLMFinding(reviewer, "pkg/auth_test.go", 2, SeverityMedium, CategoryTests),
			},
			[]map[string]any{
				validLLMGate("tests pass", true, "go test ./... PASS", ""),
			},
		), nil
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
	result, err := RunWithLLM(t.Context(), plan, completer, testReviewContext)
	require.NoError(t, err)

	assert.Equal(t, "aggregate-verdict", result.Report.Reviewer)
	require.Len(t, result.Report.Findings, 1)
	assert.Equal(t, SeverityHigh, result.Report.Findings[0].Severity)
	assert.Equal(t, "pkg/auth.go", result.Report.Findings[0].Path)
	assert.Equal(t, 2, result.Report.Findings[0].Line)
	assert.Equal(t, 2, result.Report.Findings[0].EndLine)
	assert.NotEmpty(t, result.Report.Findings[0].Evidence)
	assert.NotEmpty(t, result.Report.Findings[0].SeverityRationale)
	assert.NotEmpty(t, result.Report.Findings[0].SuggestedVerification)
	assert.NotEmpty(t, result.Report.Findings[0].Provenance)
	assert.Len(t, result.Session.Reports, 2)
	assert.Len(t, result.Session.CrossReviews, 2)
	assert.Empty(t, result.Session.Errors)
	assert.Equal(t, "tests pass", result.Report.GateChecks[0].Name)
	assert.True(t, result.Report.GateChecks[0].Passed)
	assert.NotEmpty(t, result.Report.GateChecks[0].Proof)
	assert.NotEmpty(t, result.Report.GateChecks[0].Provenance)

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
		proof := "go test ./... failed"
		if reviewer == reviewJudgeName {
			return llmReportJSON(
				"aggregate-verdict",
				nil,
				[]map[string]any{validLLMGate("tests pass", false, proof, "")},
			)
		}

		return llmReportJSON(
			reviewer,
			nil,
			[]map[string]any{validLLMGate("tests pass", false, proof, "")},
		)
	})

	result, err := RunWithLLM(t.Context(), plan, completer, strings.Replace(testReviewContext, "go test ./... PASS", "go test ./... failed", 1))
	require.Error(t, err)
	assert.Contains(t, err.Error(), `gate check "tests pass" failed`)
	assert.Len(t, result.Session.Reports, 1)
	assert.Equal(t, "aggregate-verdict", result.Session.Verdict.Reviewer)
	require.Len(t, result.Session.Errors, 1)
	assert.Equal(t, string(RoundAggregateVerdict), result.Session.Errors[0].Stage)
	assert.Contains(t, result.Session.Errors[0].Message, `gate check "tests pass" failed`)
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
			return llmReportJSON("aggregate-verdict", nil, nil)
		}

		return llmReportJSON(
			reviewer,
			nil,
			[]map[string]any{validLLMGate("tests pass", true, "go test ./... PASS", "")},
		)
	})

	result, err := RunWithLLM(t.Context(), plan, completer, testReviewContext)
	require.Error(t, err)
	assert.Contains(t, err.Error(), `missing gate check "tests pass"`)
	assert.Len(t, result.Session.Reports, 1)
	assert.Equal(t, "aggregate-verdict", result.Session.Verdict.Reviewer)
	assert.Empty(t, result.Session.Verdict.GateChecks)
	require.Len(t, result.Session.Errors, 1)
	assert.Contains(t, result.Session.Errors[0].Message, `missing gate check "tests pass"`)
}

func TestRunWithLLM_MalformedReviewResponseFailsAndIsRecorded(t *testing.T) {
	t.Parallel()

	plan, err := NewPlan(
		[]Reviewer{{Name: "quality", Categories: []Category{CategoryCorrectness}}},
		[]string{"pkg/auth.go"},
		[]string{"tests pass"},
	)
	require.NoError(t, err)

	result, err := RunWithLLM(t.Context(), plan, staticReviewCompleter(func(string) string {
		return "FINDING: high|correctness|pkg/auth.go|2|bad|fix"
	}), testReviewContext)

	require.Error(t, err)
	assert.Contains(t, err.Error(), "parse review JSON")
	require.Len(t, result.Session.Errors, 1)
	assert.Equal(t, string(RoundIndependentReview), result.Session.Errors[0].Stage)
	assert.Equal(t, "quality", result.Session.Errors[0].Reviewer)
	assert.Contains(t, result.Session.Errors[0].Message, "parse review JSON")
	require.Len(t, result.Session.Reports, 1)
	assert.Equal(t, "quality", result.Session.Reports[0].Reviewer)
}

func TestRunWithLLM_MalformedCrossReviewResponseFailsAndIsRecorded(t *testing.T) {
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

	result, err := RunWithLLM(t.Context(), plan, promptReviewCompleter(func(reviewer, systemPrompt string) string {
		if reviewer == reviewJudgeName {
			return llmReportJSON("aggregate-verdict", nil, []map[string]any{validLLMGate("tests pass", true, "go test ./... PASS", "")})
		}

		if !strings.Contains(systemPrompt, "cross-reviewing") {
			return llmReportJSON(reviewer, nil, []map[string]any{validLLMGate("tests pass", true, "go test ./... PASS", "")})
		}

		return "free-form cross review"
	}), testReviewContext)

	require.Error(t, err)
	assert.Contains(t, err.Error(), "parse cross-review JSON")
	require.NotEmpty(t, result.Session.Errors)
	assert.Equal(t, string(RoundCrossReview), result.Session.Errors[0].Stage)
	assert.Contains(t, result.Session.Errors[0].Message, "parse cross-review JSON")
}

func TestRunWithLLM_MalformedAggregateResponseFailsAndIsRecorded(t *testing.T) {
	t.Parallel()

	plan, err := NewPlan(
		[]Reviewer{{Name: "quality", Categories: []Category{CategoryCorrectness}}},
		[]string{"pkg/auth.go"},
		[]string{"tests pass"},
	)
	require.NoError(t, err)

	result, err := RunWithLLM(t.Context(), plan, promptReviewCompleter(func(reviewer, _ string) string {
		if reviewer == reviewJudgeName {
			return "VERDICT: ship it"
		}

		return llmReportJSON(reviewer, nil, []map[string]any{validLLMGate("tests pass", true, "go test ./... PASS", "")})
	}), testReviewContext)

	require.Error(t, err)
	assert.Contains(t, err.Error(), "aggregate: parse review JSON")
	assert.Len(t, result.Session.Reports, 1)
	require.Len(t, result.Session.Errors, 1)
	assert.Equal(t, string(RoundAggregateVerdict), result.Session.Errors[0].Stage)
	assert.Equal(t, reviewJudgeName, result.Session.Errors[0].Reviewer)
	assert.Contains(t, result.Session.Errors[0].Message, "parse review JSON")
}

func TestParseCrossReviewFromLLM_RejectsInvalidSchema(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		content string
		want    string
	}{
		{
			name:    "malformed json",
			content: "looks good to me",
			want:    "parse cross-review JSON",
		},
		{
			name: "unknown top level field",
			content: llmJSON(map[string]any{
				"notes":      "keep the finding",
				"challenges": []map[string]any{},
				"extra":      true,
			}),
			want: `unknown field "extra"`,
		},
		{
			name: "duplicate challenge field",
			content: `{"notes":"keep the finding","challenges":[{` +
				`"finding":"pkg/auth.go:2 nil token can panic",` +
				`"finding":"pkg/auth.go:3 missing regression test",` +
				`"position":"support",` +
				`"rationale":"matches the cited evidence",` +
				`"suggested_verification":"keep the regression test gate"` +
				`}]}`,
			want: `duplicate JSON field "finding"`,
		},
		{
			name: "missing challenges",
			content: llmJSON(map[string]any{
				"notes": "keep the finding",
			}),
			want: "challenges array is required",
		},
		{
			name: "missing challenge field",
			content: llmCrossReviewJSON(
				"keep the finding",
				[]map[string]any{withoutField(validLLMCrossReviewChallenge("pkg/auth.go:2 nil token can panic", "support"), "rationale")},
			),
			want: "missing required field(s): rationale",
		},
		{
			name: "invalid challenge position",
			content: llmCrossReviewJSON(
				"keep the finding",
				[]map[string]any{validLLMCrossReviewChallenge("pkg/auth.go:2 nil token can panic", "maybe")},
			),
			want: `invalid position "maybe"`,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			_, err := parseCrossReviewFromLLM(test.content)
			require.Error(t, err)
			assert.Contains(t, err.Error(), test.want)
		})
	}
}

func TestRunWithLLM_RequiresCompleterContextAndSnapshot(t *testing.T) {
	t.Parallel()

	plan, err := NewPlan([]Reviewer{{Name: "quality"}}, []string{"pkg/auth.go"}, []string{"tests pass"})
	require.NoError(t, err)

	_, err = RunWithLLM(t.Context(), plan, nil, testReviewContext)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "LLM completer is required")

	_, err = RunWithLLM(t.Context(), plan, staticReviewCompleter(func(string) string { return "" }), " ")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "review context is required")

	_, err = RunWithLLM(t.Context(), plan, staticReviewCompleter(func(string) string { return "" }), "diff -- pkg/auth.go")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "configured file references")
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

func TestParseReportFromLLM_RequiresEvidenceBackedJSON(t *testing.T) {
	t.Parallel()

	report, err := parseReportFromLLM(
		llmReportJSON(
			"judge",
			[]map[string]any{validLLMFinding("judge", "pkg/auth.go", 2, SeverityHigh, CategoryCorrectness)},
			[]map[string]any{validLLMGate("tests pass", true, "go test ./... PASS", "")},
		),
		"judge",
		[]string{"tests pass"},
		validReviewSnapshot(t),
	)
	require.NoError(t, err)

	require.Len(t, report.Findings, 1)
	assert.Equal(t, SeverityHigh, report.Findings[0].Severity)
	assert.Equal(t, CategoryCorrectness, report.Findings[0].Category)
	assert.Equal(t, "pkg/auth.go", report.Findings[0].Path)
	assert.Equal(t, "high", report.Findings[0].Confidence)
	assert.Equal(t, []GateCheck{
		{
			Name:   "tests pass",
			Passed: true,
			Notes:  "gate evaluated",
			Proof:  "go test ./... PASS",
			Provenance: []EvidenceSource{
				{Type: EvidenceCommandOutput, Source: "go test ./...", Summary: "go test ./... PASS"},
			},
		},
	}, report.GateChecks)
}

func TestParseReportFromLLM_AllowsExplicitGateNotRunReason(t *testing.T) {
	t.Parallel()

	report, err := parseReportFromLLM(
		llmReportJSON(
			"judge",
			nil,
			[]map[string]any{validLLMGateNotRun("tests pass", "judge", "tests were not run in the supplied context")},
		),
		"judge",
		[]string{"tests pass"},
		validReviewSnapshot(t),
	)
	require.NoError(t, err)

	require.Len(t, report.GateChecks, 1)
	assert.False(t, report.GateChecks[0].Passed)
	assert.Equal(t, "tests were not run in the supplied context", report.GateChecks[0].NotRunReason)
}

func TestParseReportFromLLM_PreservesDissentAndUncertainty(t *testing.T) {
	t.Parallel()

	finding := validLLMFinding("judge", "pkg/auth.go", 2, SeverityMedium, CategoryCorrectness)
	finding["confidence"] = "medium"
	finding["dissent"] = []map[string]any{
		{
			"type":    string(EvidenceModelJudgment),
			"source":  "test-engineer",
			"summary": "uncertain until a nil-token regression test is added",
		},
	}

	report, err := parseReportFromLLM(
		llmReportJSON(
			"judge",
			[]map[string]any{finding},
			[]map[string]any{validLLMGate("tests pass", true, "go test ./... PASS", "")},
		),
		"judge",
		[]string{"tests pass"},
		validReviewSnapshot(t),
	)
	require.NoError(t, err)

	require.Len(t, report.Findings, 1)
	assert.Equal(t, "medium", report.Findings[0].Confidence)
	assert.Equal(t, []EvidenceSource{
		{
			Type:    EvidenceModelJudgment,
			Source:  "test-engineer",
			Summary: "uncertain until a nil-token regression test is added",
		},
	}, report.Findings[0].Dissent)
}

func TestParseReportFromLLM_RejectsInvalidEvidenceAndSchema(t *testing.T) {
	t.Parallel()

	snapshot := validReviewSnapshot(t)

	tests := []struct {
		name    string
		content string
		want    string
	}{
		{
			name:    "malformed json",
			content: "GATE tests pass: PASS",
			want:    "parse review JSON",
		},
		{
			name: "unknown top level field",
			content: llmJSON(map[string]any{
				"reviewer":    "judge",
				"findings":    []map[string]any{},
				"gate_checks": []map[string]any{validLLMGate("tests pass", true, "go test ./... PASS", "")},
				"extra":       true,
			}),
			want: `unknown field "extra"`,
		},
		{
			name: "duplicate top level field",
			content: `{"reviewer":"judge","reviewer":"other",` +
				`"findings":[],"gate_checks":[]}`,
			want: `duplicate JSON field "reviewer"`,
		},
		{
			name: "unknown finding field",
			content: llmReportJSON(
				"judge",
				[]map[string]any{withFindingField(validLLMFinding("judge", "pkg/auth.go", 2, SeverityHigh, CategoryCorrectness), "extra", true)},
				[]map[string]any{validLLMGate("tests pass", true, "go test ./... PASS", "")},
			),
			want: `unknown field "extra"`,
		},
		{
			name: "unknown gate field",
			content: llmReportJSON(
				"judge",
				nil,
				[]map[string]any{withFindingField(validLLMGate("tests pass", true, "go test ./... PASS", ""), "extra", true)},
			),
			want: `unknown field "extra"`,
		},
		{
			name: "invalid path",
			content: llmReportJSON(
				"judge",
				[]map[string]any{validLLMFinding("judge", "pkg/missing.go", 1, SeverityHigh, CategoryCorrectness)},
				[]map[string]any{validLLMGate("tests pass", true, "go test ./... PASS", "")},
			),
			want: `finding path "pkg/missing.go" was not in reviewed snapshot`,
		},
		{
			name: "invalid line range",
			content: llmReportJSON(
				"judge",
				[]map[string]any{validLLMFinding("judge", "pkg/auth.go", 99, SeverityHigh, CategoryCorrectness)},
				[]map[string]any{validLLMGate("tests pass", true, "go test ./... PASS", "")},
			),
			want: "exceeds",
		},
		{
			name: "unknown gate",
			content: llmReportJSON(
				"judge",
				nil,
				[]map[string]any{validLLMGate("deploy complete", true, "deploy ok", "")},
			),
			want: `unknown gate check "deploy complete"`,
		},
		{
			name: "duplicate gate",
			content: llmReportJSON(
				"judge",
				nil,
				[]map[string]any{
					validLLMGate("tests pass", true, "go test ./... PASS", ""),
					validLLMGate("tests pass", true, "go test ./... PASS", ""),
				},
			),
			want: `duplicate gate check "tests pass"`,
		},
		{
			name: "duplicate finding",
			content: llmReportJSON(
				"judge",
				[]map[string]any{
					validLLMFinding("judge", "pkg/auth.go", 2, SeverityHigh, CategoryCorrectness),
					validLLMFinding("judge", "pkg/auth.go", 2, SeverityHigh, CategoryCorrectness),
				},
				[]map[string]any{validLLMGate("tests pass", true, "go test ./... PASS", "")},
			),
			want: "duplicate finding",
		},
		{
			name: "missing evidence",
			content: llmReportJSON(
				"judge",
				[]map[string]any{withFindingField(validLLMFinding("judge", "pkg/auth.go", 2, SeverityHigh, CategoryCorrectness), "evidence", "")},
				[]map[string]any{validLLMGate("tests pass", true, "go test ./... PASS", "")},
			),
			want: "finding evidence is required",
		},
		{
			name: "evidence not in cited range",
			content: llmReportJSON(
				"judge",
				[]map[string]any{withFindingField(validLLMFinding("judge", "pkg/auth.go", 3, SeverityHigh, CategoryCorrectness), "evidence", "func token() string { return \"\" }")},
				[]map[string]any{validLLMGate("tests pass", true, "go test ./... PASS", "")},
			),
			want: "finding evidence must quote reviewed snapshot range pkg/auth.go:3-3",
		},
		{
			name: "finding without review context provenance",
			content: llmReportJSON(
				"judge",
				[]map[string]any{withFindingField(
					validLLMFinding("judge", "pkg/auth.go", 2, SeverityHigh, CategoryCorrectness),
					"provenance",
					[]map[string]any{{"type": string(EvidenceModelJudgment), "source": "judge", "summary": "model identified the risk"}},
				)},
				[]map[string]any{validLLMGate("tests pass", true, "go test ./... PASS", "")},
			),
			want: `finding provenance must include review-context source for pkg/auth.go:2-2`,
		},
		{
			name: "finding without model judgment provenance",
			content: llmReportJSON(
				"judge",
				[]map[string]any{withFindingField(
					validLLMFinding("judge", "pkg/auth.go", 2, SeverityHigh, CategoryCorrectness),
					"provenance",
					[]map[string]any{{"type": string(EvidenceReviewContext), "source": "pkg/auth.go:2", "summary": "reviewed file line supports the finding"}},
				)},
				[]map[string]any{validLLMGate("tests pass", true, "go test ./... PASS", "")},
			),
			want: "finding provenance must include model-judgment source",
		},
		{
			name: "finding model judgment from wrong reviewer",
			content: llmReportJSON(
				"judge",
				[]map[string]any{withFindingField(
					validLLMFinding("other-reviewer", "pkg/auth.go", 2, SeverityHigh, CategoryCorrectness),
					"provenance",
					[]map[string]any{
						{"type": string(EvidenceModelJudgment), "source": "other-reviewer", "summary": "another reviewer identified the risk"},
						{"type": string(EvidenceReviewContext), "source": "pkg/auth.go:2", "summary": "reviewed file line supports the finding"},
					},
				)},
				[]map[string]any{validLLMGate("tests pass", true, "go test ./... PASS", "")},
			),
			want: `finding provenance 0 model-judgment source "other-reviewer" does not match reviewer "judge"`,
		},
		{
			name: "finding review context provenance without line range",
			content: llmReportJSON(
				"judge",
				[]map[string]any{withFindingField(
					validLLMFinding("judge", "pkg/auth.go", 2, SeverityHigh, CategoryCorrectness),
					"provenance",
					[]map[string]any{
						{"type": string(EvidenceModelJudgment), "source": "judge", "summary": "model identified the risk"},
						{"type": string(EvidenceReviewContext), "source": "pkg/auth.go", "summary": "file was reviewed"},
					},
				)},
				[]map[string]any{validLLMGate("tests pass", true, "go test ./... PASS", "")},
			),
			want: `finding provenance must include review-context source for pkg/auth.go:2-2`,
		},
		{
			name: "finding review context provenance outside finding range",
			content: llmReportJSON(
				"judge",
				[]map[string]any{withFindingField(
					validLLMFinding("judge", "pkg/auth.go", 2, SeverityHigh, CategoryCorrectness),
					"provenance",
					[]map[string]any{
						{"type": string(EvidenceModelJudgment), "source": "judge", "summary": "model identified the risk"},
						{"type": string(EvidenceReviewContext), "source": "pkg/auth.go:3", "summary": "wrong line was cited"},
					},
				)},
				[]map[string]any{validLLMGate("tests pass", true, "go test ./... PASS", "")},
			),
			want: `finding provenance must include review-context source for pkg/auth.go:2-2`,
		},
		{
			name: "finding review context provenance range outside snapshot",
			content: llmReportJSON(
				"judge",
				[]map[string]any{withFindingField(
					validLLMFinding("judge", "pkg/auth.go", 2, SeverityHigh, CategoryCorrectness),
					"provenance",
					[]map[string]any{
						{"type": string(EvidenceModelJudgment), "source": "judge", "summary": "model identified the risk"},
						{"type": string(EvidenceReviewContext), "source": "pkg/auth.go:2-99", "summary": "line range exceeds the reviewed file"},
					},
				)},
				[]map[string]any{validLLMGate("tests pass", true, "go test ./... PASS", "")},
			),
			want: "finding provenance 1 review-context source",
		},
		{
			name: "finding command output provenance not in context",
			content: llmReportJSON(
				"judge",
				[]map[string]any{withFindingField(
					validLLMFinding("judge", "pkg/auth.go", 2, SeverityHigh, CategoryCorrectness),
					"provenance",
					[]map[string]any{
						{"type": string(EvidenceModelJudgment), "source": "judge", "summary": "model identified the risk"},
						{"type": string(EvidenceReviewContext), "source": "pkg/auth.go:2", "summary": "reviewed file line supports the finding"},
						{"type": string(EvidenceCommandOutput), "source": "go test ./pkg/auth", "summary": "invented failing test output"},
					},
				)},
				[]map[string]any{validLLMGate("tests pass", true, "go test ./... PASS", "")},
			),
			want: "finding provenance 2 command-output source was not found in review context",
		},
		{
			name: "finding command output command alone is not hard evidence",
			content: llmReportJSON(
				"judge",
				[]map[string]any{withFindingField(
					validLLMFinding("judge", "pkg/auth.go", 2, SeverityHigh, CategoryCorrectness),
					"provenance",
					[]map[string]any{
						{"type": string(EvidenceModelJudgment), "source": "judge", "summary": "model identified the risk"},
						{"type": string(EvidenceReviewContext), "source": "pkg/auth.go:2", "summary": "reviewed file line supports the finding"},
						{"type": string(EvidenceCommandOutput), "source": "go test ./...", "summary": "invented failing test output"},
					},
				)},
				[]map[string]any{validLLMGate("tests pass", true, "go test ./... PASS", "")},
			),
			want: "finding provenance 2 command-output source was not found in review context",
		},
		{
			name: "missing required finding field",
			content: llmReportJSON(
				"judge",
				[]map[string]any{withoutField(validLLMFinding("judge", "pkg/auth.go", 2, SeverityHigh, CategoryCorrectness), "severity_rationale")},
				[]map[string]any{validLLMGate("tests pass", true, "go test ./... PASS", "")},
			),
			want: "missing required field(s): severity_rationale",
		},
		{
			name: "gate without proof or not run reason",
			content: llmReportJSON(
				"judge",
				nil,
				[]map[string]any{validLLMGate("tests pass", false, "", "")},
			),
			want: `gate check "tests pass" requires proof or not_run_reason`,
		},
		{
			name: "gate proof not in review context",
			content: llmReportJSON(
				"judge",
				nil,
				[]map[string]any{validLLMGate("tests pass", true, "go test ./pkg/other PASS", "")},
			),
			want: `gate check "tests pass": gate check provenance 0 command-output source was not found in review context`,
		},
		{
			name: "gate model judgment from wrong reviewer",
			content: llmReportJSON(
				"judge",
				nil,
				[]map[string]any{withGateProvenance(
					validLLMGate("tests pass", false, "", "tests were not run"),
					[]map[string]any{{"type": string(EvidenceModelJudgment), "source": "other-reviewer", "summary": "tests were not run"}},
				)},
			),
			want: `gate check "tests pass": gate check provenance 0 model-judgment source "other-reviewer" does not match reviewer "judge"`,
		},
		{
			name: "gate not run reason without model judgment provenance",
			content: llmReportJSON(
				"judge",
				nil,
				[]map[string]any{withGateProvenance(
					validLLMGate("tests pass", false, "", "tests were not run"),
					[]map[string]any{{"type": string(EvidenceReviewContext), "source": "pkg/auth.go:2", "summary": "reviewed source but not command output"}},
				)},
			),
			want: `gate check "tests pass" not_run_reason requires model-judgment provenance`,
		},
		{
			name: "gate command output provenance not in context",
			content: llmReportJSON(
				"judge",
				nil,
				[]map[string]any{withGateProvenance(
					validLLMGate("tests pass", false, "", "tests were not run"),
					[]map[string]any{{"type": string(EvidenceCommandOutput), "source": "go test ./pkg/auth", "summary": "invented failing test output"}},
				)},
			),
			want: `gate check "tests pass": gate check provenance 0 command-output source was not found in review context`,
		},
		{
			name: "gate with proof and not run reason",
			content: llmReportJSON(
				"judge",
				nil,
				[]map[string]any{validLLMGate("tests pass", false, "go test ./... failed", "tests were skipped")},
			),
			want: `gate check "tests pass" cannot include both proof and not_run_reason`,
		},
		{
			name: "test gate without command output provenance",
			content: llmReportJSON(
				"judge",
				nil,
				[]map[string]any{withGateProvenance(
					validLLMGate("tests pass", true, "go test ./... PASS", ""),
					[]map[string]any{{"type": string(EvidenceModelJudgment), "source": "judge", "summary": "model says tests pass"}},
				)},
			),
			want: `gate check "tests pass" requires command-output provenance`,
		},
		{
			name: "test gate command output provenance must support proof",
			content: llmReportJSON(
				"judge",
				nil,
				[]map[string]any{withGateProvenance(
					validLLMGate("tests pass", true, "go test ./... PASS", ""),
					[]map[string]any{{"type": string(EvidenceCommandOutput), "source": "make lint", "summary": "make lint PASS"}},
				)},
			),
			want: `gate check "tests pass" command-output provenance must support proof`,
		},
		{
			name: "test gate proof must include command source and output summary",
			content: llmReportJSON(
				"judge",
				nil,
				[]map[string]any{withGateProvenance(
					validLLMGate("tests pass", true, "PASS", ""),
					[]map[string]any{{"type": string(EvidenceCommandOutput), "source": "go test ./...", "summary": "PASS"}},
				)},
			),
			want: `gate check "tests pass" command-output provenance must support proof`,
		},
		{
			name: "test gate command output command alone is not enough",
			content: llmReportJSON(
				"judge",
				nil,
				[]map[string]any{withGateProvenance(
					validLLMGate("tests pass", true, "go test ./... PASS", ""),
					[]map[string]any{{"type": string(EvidenceCommandOutput), "source": "go test ./...", "summary": "go test ./... FAIL"}},
				)},
			),
			want: `gate check "tests pass": gate check provenance 0 command-output source was not found in review context`,
		},
		{
			name: "gate review context provenance invalid path",
			content: llmReportJSON(
				"judge",
				nil,
				[]map[string]any{withGateProvenance(
					validLLMGate("tests pass", true, "go test ./... PASS", ""),
					[]map[string]any{
						{"type": string(EvidenceCommandOutput), "source": "go test ./...", "summary": "go test ./... PASS"},
						{"type": string(EvidenceReviewContext), "source": "pkg/missing.go:1", "summary": "gate references a missing file"},
					},
				)},
			),
			want: `gate check "tests pass": gate check provenance 1 review-context source`,
		},
		{
			name: "invalid category",
			content: llmReportJSON(
				"judge",
				[]map[string]any{validLLMFinding("judge", "pkg/auth.go", 2, SeverityHigh, Category("unknown"))},
				[]map[string]any{validLLMGate("tests pass", true, "go test ./... PASS", "")},
			),
			want: `invalid category "unknown"`,
		},
		{
			name: "invalid severity",
			content: llmReportJSON(
				"judge",
				[]map[string]any{validLLMFinding("judge", "pkg/auth.go", 2, Severity("urgent"), CategoryCorrectness)},
				[]map[string]any{validLLMGate("tests pass", true, "go test ./... PASS", "")},
			),
			want: `invalid severity "urgent"`,
		},
		{
			name: "uppercase enum is rejected",
			content: llmReportJSON(
				"judge",
				[]map[string]any{validLLMFinding("judge", "pkg/auth.go", 2, Severity("High"), CategoryCorrectness)},
				[]map[string]any{validLLMGate("tests pass", true, "go test ./... PASS", "")},
			),
			want: `invalid severity "High"`,
		},
		{
			name: "invalid confidence",
			content: llmReportJSON(
				"judge",
				[]map[string]any{withFindingField(validLLMFinding("judge", "pkg/auth.go", 2, SeverityHigh, CategoryCorrectness), "confidence", "certain")},
				[]map[string]any{validLLMGate("tests pass", true, "go test ./... PASS", "")},
			),
			want: `finding confidence must be one of high, medium, or low, got "certain"`,
		},
		{
			name: "invalid provenance type",
			content: llmReportJSON(
				"judge",
				[]map[string]any{withFindingField(
					validLLMFinding("judge", "pkg/auth.go", 2, SeverityHigh, CategoryCorrectness),
					"provenance",
					[]map[string]any{
						{"type": "hearsay", "source": "judge", "summary": "model identified the risk"},
						{"type": string(EvidenceReviewContext), "source": "pkg/auth.go:2", "summary": "reviewed file line supports the finding"},
					},
				)},
				[]map[string]any{validLLMGate("tests pass", true, "go test ./... PASS", "")},
			),
			want: `finding provenance 0 has invalid type "hearsay"`,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			_, err := parseReportFromLLM(test.content, "judge", []string{"tests pass"}, snapshot)
			require.Error(t, err)
			assert.Contains(t, err.Error(), test.want)
		})
	}
}

func TestParseReportFromLLM_RejectsFindingCommandOutputSummaryWithoutCommand(t *testing.T) {
	t.Parallel()

	snapshot, err := newReviewSnapshotFromContext(`<configured_references>
<file source="pkg/auth.go">
package auth
func token() string { return "" }
</file>
</configured_references>

Command output:
PASS
`)
	require.NoError(t, err)

	finding := withFindingField(
		validLLMFinding("judge", "pkg/auth.go", 2, SeverityHigh, CategoryCorrectness),
		"provenance",
		[]map[string]any{
			{"type": string(EvidenceModelJudgment), "source": "judge", "summary": "model identified the risk"},
			{"type": string(EvidenceReviewContext), "source": "pkg/auth.go:2", "summary": "reviewed file line supports the finding"},
			{"type": string(EvidenceCommandOutput), "source": "go test ./...", "summary": "PASS"},
		},
	)

	_, err = parseReportFromLLM(
		llmReportJSON(
			"judge",
			[]map[string]any{finding},
			[]map[string]any{validLLMGateNotRun("tests pass", "judge", "tests were not run in the supplied context")},
		),
		"judge",
		[]string{"tests pass"},
		snapshot,
	)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "finding provenance 2 command-output source was not found in review context")
}

func TestParseReportFromLLM_AllowsReviewContextGateProof(t *testing.T) {
	t.Parallel()

	report, err := parseReportFromLLM(
		llmReportJSON(
			"judge",
			nil,
			[]map[string]any{withGateProvenance(
				validLLMGate("behavioral diff reviewed", true, "func token() string { return \"\" }", ""),
				[]map[string]any{{"type": string(EvidenceReviewContext), "source": "pkg/auth.go:2", "summary": "reviewed the changed code"}},
			)},
		),
		"judge",
		[]string{"behavioral diff reviewed"},
		validReviewSnapshot(t),
	)
	require.NoError(t, err)
	assert.Equal(t, "behavioral diff reviewed", report.GateChecks[0].Name)
}

func TestParseReportFromLLM_AggregateFindingRequiresOriginalSupport(t *testing.T) {
	t.Parallel()

	_, err := parseReportFromLLM(
		llmReportJSON(
			string(RoundAggregateVerdict),
			[]map[string]any{validLLMFinding(string(RoundAggregateVerdict), "pkg/auth.go", 2, SeverityHigh, CategoryCorrectness)},
			[]map[string]any{validLLMGate("tests pass", true, "go test ./... PASS", "")},
		),
		string(RoundAggregateVerdict),
		[]string{"tests pass"},
		validReviewSnapshot(t),
	)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "aggregate finding provenance must include supporting reviewer or command-output source")
}

func TestParseAggregateReportFromLLM_RejectsUnknownReviewerSupport(t *testing.T) {
	t.Parallel()

	finding := validLLMFinding("unknown-reviewer", "pkg/auth.go", 2, SeverityHigh, CategoryCorrectness)

	_, err := parseAggregateReportFromLLM(
		llmReportJSON(
			string(RoundAggregateVerdict),
			[]map[string]any{finding},
			[]map[string]any{validLLMGate("tests pass", true, "go test ./... PASS", "")},
		),
		string(RoundAggregateVerdict),
		[]string{"tests pass"},
		[]string{"quality-reviewer", "test-engineer"},
		validReviewSnapshot(t),
	)
	require.Error(t, err)
	assert.Contains(t, err.Error(), `finding provenance 0 model-judgment source "unknown-reviewer" does not match reviewer "aggregate-verdict"`)
}

func TestParseAggregateReportFromLLM_AcceptsKnownReviewerSupport(t *testing.T) {
	t.Parallel()

	report, err := parseAggregateReportFromLLM(
		llmReportJSON(
			string(RoundAggregateVerdict),
			[]map[string]any{validLLMFinding("quality-reviewer", "pkg/auth.go", 2, SeverityHigh, CategoryCorrectness)},
			[]map[string]any{validLLMGate("tests pass", true, "go test ./... PASS", "")},
		),
		string(RoundAggregateVerdict),
		[]string{"tests pass"},
		[]string{"quality-reviewer", "test-engineer"},
		validReviewSnapshot(t),
	)
	require.NoError(t, err)
	require.Len(t, report.Findings, 1)
	assert.Equal(t, "quality-reviewer", report.Findings[0].Provenance[0].Source)
}

func TestParseReportFromLLM_AggregateFindingAcceptsCommandOutputSupport(t *testing.T) {
	t.Parallel()

	finding := validLLMFinding(string(RoundAggregateVerdict), "pkg/auth.go", 2, SeverityHigh, CategoryCorrectness)
	provenance, ok := finding["provenance"].([]map[string]any)
	require.True(t, ok)

	finding["provenance"] = append(provenance, map[string]any{
		"type":    string(EvidenceCommandOutput),
		"source":  "go test ./...",
		"summary": "go test ./... PASS",
	})

	report, err := parseReportFromLLM(
		llmReportJSON(
			string(RoundAggregateVerdict),
			[]map[string]any{finding},
			[]map[string]any{validLLMGate("tests pass", true, "go test ./... PASS", "")},
		),
		string(RoundAggregateVerdict),
		[]string{"tests pass"},
		validReviewSnapshot(t),
	)
	require.NoError(t, err)
	require.Len(t, report.Findings, 1)
	assert.Equal(t, string(RoundAggregateVerdict), report.Findings[0].Provenance[0].Source)
}

func TestParseReportFromLLM_RejectsReviewContextGateProofOutsideProvenanceRange(t *testing.T) {
	t.Parallel()

	_, err := parseReportFromLLM(
		llmReportJSON(
			"judge",
			nil,
			[]map[string]any{withGateProvenance(
				validLLMGate("behavioral diff reviewed", true, "func token() string { return \"\" }", ""),
				[]map[string]any{{"type": string(EvidenceReviewContext), "source": "pkg/auth.go:3", "summary": "reviewed the changed code"}},
			)},
		),
		"judge",
		[]string{"behavioral diff reviewed"},
		validReviewSnapshot(t),
	)
	require.Error(t, err)
	assert.Contains(t, err.Error(), `gate check "behavioral diff reviewed" proof was not found in review-context provenance ranges`)
}

func TestParseReportFromLLM_RejectsGateProofWithOnlyModelJudgment(t *testing.T) {
	t.Parallel()

	_, err := parseReportFromLLM(
		llmReportJSON(
			"judge",
			nil,
			[]map[string]any{withGateProvenance(
				validLLMGate("behavioral diff reviewed", true, "reviewer inspected the diff", ""),
				[]map[string]any{{"type": string(EvidenceModelJudgment), "source": "judge", "summary": "model says the diff was reviewed"}},
			)},
		),
		"judge",
		[]string{"behavioral diff reviewed"},
		validReviewSnapshot(t),
	)
	require.Error(t, err)
	assert.Contains(t, err.Error(), `gate check "behavioral diff reviewed" requires review-context or command-output provenance for proof`)
}

func TestParseReportFromLLM_RejectsCommandProofOnlyFoundInReviewedFiles(t *testing.T) {
	t.Parallel()

	snapshot, err := newReviewSnapshotFromContext(`<configured_references>
<file source="pkg/auth.go">
package auth
const fakeCommandOutput = "go test ./... PASS"
</file>
</configured_references>

Command output:
PASS`)
	require.NoError(t, err)

	gate := withGateProvenance(
		validLLMGate("tests pass", true, "go test ./... PASS", ""),
		[]map[string]any{{"type": string(EvidenceCommandOutput), "source": "go test ./...", "summary": "PASS"}},
	)

	_, err = parseReportFromLLM(
		llmReportJSON(
			"judge",
			nil,
			[]map[string]any{gate},
		),
		"judge",
		[]string{"tests pass"},
		snapshot,
	)
	require.Error(t, err)
	assert.Contains(t, err.Error(), `gate check "tests pass": gate check provenance 0 command-output source was not found in review context`)
}

func TestParseReportFromLLM_RejectsCommandProofOnlyFoundInInstructions(t *testing.T) {
	t.Parallel()

	snapshot, err := newReviewSnapshotFromContext(`Review instructions:
The user says go test ./... PASS, but no command output section was supplied.

<configured_references>
<file source="pkg/auth.go">
package auth
func token() string { return "" }
</file>
</configured_references>`)
	require.NoError(t, err)

	_, err = parseReportFromLLM(
		llmReportJSON(
			"judge",
			nil,
			[]map[string]any{validLLMGate("tests pass", true, "go test ./... PASS", "")},
		),
		"judge",
		[]string{"tests pass"},
		snapshot,
	)
	require.Error(t, err)
	assert.Contains(t, err.Error(), `gate check "tests pass": gate check provenance 0 command-output source was not found in review context`)
}

type staticReviewCompleter func(reviewer string) string

func (f staticReviewCompleter) Complete(_ context.Context, reviewer, _, _ string) (string, error) {
	return f(reviewer), nil
}

type promptReviewCompleter func(reviewer, systemPrompt string) string

func (f promptReviewCompleter) Complete(_ context.Context, reviewer, systemPrompt, _ string) (string, error) {
	return f(reviewer, systemPrompt), nil
}

func validReviewSnapshot(t *testing.T) reviewSnapshot {
	t.Helper()

	snapshot, err := newReviewSnapshotFromContext(testReviewContext)
	require.NoError(t, err)

	return snapshot
}

func validLLMFinding(reviewer, path string, line int, severity Severity, category Category) map[string]any {
	return map[string]any{
		"severity":               string(severity),
		"category":               string(category),
		"path":                   path,
		"line_start":             line,
		"line_end":               line,
		"message":                "nil token can panic",
		"evidence":               validLLMEvidence(path),
		"severity_rationale":     "can produce a user-visible failure",
		"suggestion":             "guard token before use",
		"suggested_verification": "add a regression test for nil token handling",
		"provenance": []map[string]any{
			{"type": string(EvidenceModelJudgment), "source": reviewer, "summary": "reviewer identified the risk"},
			{"type": string(EvidenceReviewContext), "source": path + ":" + strconv.Itoa(line), "summary": "reviewed file line supports the finding"},
		},
		"dissent":    []map[string]any{},
		"confidence": "high",
	}
}

func validLLMEvidence(path string) string {
	switch path {
	case "pkg/auth_test.go":
		return "func TestToken(t *testing.T) {}"
	default:
		return "func token() string { return \"\" }"
	}
}

func validLLMGate(name string, passed bool, proof, notRunReason string) map[string]any {
	evidenceType := EvidenceCommandOutput
	source := "go test ./..."

	if strings.TrimSpace(proof) == "" && strings.TrimSpace(notRunReason) != "" {
		evidenceType = EvidenceModelJudgment
		source = "reviewer"
	}

	return map[string]any{
		"name":           name,
		"passed":         passed,
		"notes":          "gate evaluated",
		"proof":          proof,
		"not_run_reason": notRunReason,
		"provenance": []map[string]any{
			{"type": string(evidenceType), "source": source, "summary": firstNonEmpty(proof, notRunReason)},
		},
	}
}

func validLLMGateNotRun(name, reviewer, notRunReason string) map[string]any {
	return withGateProvenance(
		validLLMGate(name, false, "", notRunReason),
		[]map[string]any{{"type": string(EvidenceModelJudgment), "source": reviewer, "summary": notRunReason}},
	)
}

func withFindingField(finding map[string]any, key string, value any) map[string]any {
	finding[key] = value
	return finding
}

func withGateProvenance(gate map[string]any, provenance []map[string]any) map[string]any {
	gate["provenance"] = provenance
	return gate
}

func withoutField(value map[string]any, key string) map[string]any {
	delete(value, key)
	return value
}

func llmReportJSON(reviewer string, findings, gates []map[string]any) string {
	if findings == nil {
		findings = []map[string]any{}
	}

	if gates == nil {
		gates = []map[string]any{}
	}

	return llmJSON(map[string]any{
		"reviewer":    reviewer,
		"findings":    findings,
		"gate_checks": gates,
	})
}

func llmCrossReviewJSON(notes string, challenges []map[string]any) string {
	if challenges == nil {
		challenges = []map[string]any{}
	}

	return llmJSON(map[string]any{
		"notes":      notes,
		"challenges": challenges,
	})
}

func validLLMCrossReviewChallenge(finding, position string) map[string]any {
	return map[string]any{
		"finding":                finding,
		"position":               position,
		"rationale":              "matches the cited code evidence",
		"suggested_verification": "keep the regression test gate",
	}
}

func llmJSON(value any) string {
	data, err := json.Marshal(value)
	if err != nil {
		panic(err)
	}

	return string(data)
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			return value
		}
	}

	return "not run"
}
