package main

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	attasync "github.com/tommoulard/atteler/pkg/async"
	"github.com/tommoulard/atteler/pkg/review"
	"github.com/tommoulard/atteler/pkg/session"
	"github.com/tommoulard/atteler/pkg/speculate"
	"github.com/tommoulard/atteler/pkg/tasklist"
	"github.com/tommoulard/atteler/pkg/watch"
)

func TestFormatWatchFinding(t *testing.T) {
	t.Parallel()

	got := formatWatchFinding(watch.Finding{
		Path:            "pkg/example/example.go",
		Kind:            watch.KindMissingTest,
		Severity:        watch.SeverityInfo,
		Message:         "missing _test.go companion",
		RuleID:          "watch.missing_test",
		RuleDescription: "Flags production Go files without same-directory _test.go companions.",
	})

	want := strings.Join([]string{
		"path=pkg/example/example.go",
		"kind=missing_test",
		"severity=info",
		"message=missing _test.go companion",
		"rule_id=watch.missing_test",
		"rule_description=Flags production Go files without same-directory _test.go companions.",
	}, "\t")
	if got != want {
		require.Failf(t, "unexpected watch finding format", "got %q, want %q", got, want)
	}
}

func TestFormatWatchFindingWithStatus(t *testing.T) {
	t.Parallel()

	got := formatWatchFindingWithStatus("new", watch.Finding{
		Path:     "pkg/new.go",
		Kind:     watch.KindConventionDrift,
		Severity: watch.SeverityHigh,
		Message:  "uses context.Background() outside allowed entrypoints/tests",
	})

	want := strings.Join([]string{
		"status=new",
		"path=pkg/new.go",
		"kind=convention_drift",
		"severity=high",
		"message=uses context.Background() outside allowed entrypoints/tests",
	}, "\t")
	assert.Equal(t, want, got)
}

func TestFormatWatchIteration(t *testing.T) {
	t.Parallel()

	started := time.Date(2026, 5, 2, 9, 30, 0, 0, time.UTC)
	got := formatWatchIteration(watch.IterationResult{
		Iteration: 1,
		StartedAt: started,
		Duration:  2 * time.Second,
		Findings: []watch.Finding{
			{Path: "TODO.md", Kind: watch.KindStaleTODO},
			{Path: "pkg/example/example.go", Kind: watch.KindMissingTest},
		},
	})

	want := "iteration=1\tfindings=2\tstarted=2026-05-02T09:30:00Z\tduration=2s"
	if got != want {
		require.Failf(t, "unexpected watch iteration format", "got %q, want %q", got, want)
	}
}

func TestFormatWatchIterationWithComparisonAndGate(t *testing.T) {
	t.Parallel()

	got := formatWatchIteration(watch.IterationResult{
		Iteration: 1,
		Findings:  []watch.Finding{{Path: "new.go", Kind: watch.KindConventionDrift}},
		Issues:    []watch.IssueUpsertResult{{Action: watch.IssueActionCreated}},
		Comparison: &watch.Comparison{
			Metrics: watch.TrendMetrics{New: 1, Fixed: 2, Unchanged: 3, Suppressed: 4, Unstable: 5},
		},
		Gate: &watch.GateResult{Name: "watch-quality-gate", Passed: false},
	})

	want := "iteration=1\tfindings=1\tnew=1\tfixed=2\tunchanged=3\tsuppressed=4\tunstable=5\tgate=watch-quality-gate\tgate_passed=false\tissues=1"
	assert.Equal(t, want, got)
}

func TestFormatWatchComparisonAndGate(t *testing.T) {
	t.Parallel()

	comparison := watch.Comparison{Metrics: watch.TrendMetrics{New: 1, Fixed: 0, Unchanged: 2, Suppressed: 3, Unstable: 4}}
	assert.Equal(t, "watch_comparison\tnew=1\tfixed=0\tunchanged=2\tsuppressed=3\tunstable=4", formatWatchComparison(comparison))
	assert.Equal(
		t,
		"watch_baseline\tsource=git_merge_base\tfindings=7\tref=origin/main\tcommit=abc123",
		formatWatchBaseline(watchBaselineInfo{Source: "git_merge_base", Ref: "origin/main", Commit: "abc123", Findings: 7}),
	)

	gate := watch.GateResult{
		Name:             "watch-quality-gate",
		Reason:           "new findings meet or exceed high severity",
		BlockingFindings: []watch.Finding{{Path: "new.go"}},
		Passed:           false,
	}
	assert.Equal(t, "watch_gate\tname=watch-quality-gate\tpassed=false\treason=new findings meet or exceed high severity\tblocking_findings=1", formatWatchGate(gate))
}

func TestFormatWatchIssueUpsert(t *testing.T) {
	t.Parallel()

	got := formatWatchIssueUpsert(watch.IssueUpsertResult{
		Action: watch.IssueActionCreated,
		Issue: watch.IssueRef{
			URL:         "https://github.com/owner/repo/issues/12",
			Fingerprint: "abc123",
			Number:      12,
		},
		Finding: watch.Finding{ID: "watch.stale_todo:abc123"},
	})

	want := "watch_issue\taction=created\tnumber=12\turl=https://github.com/owner/repo/issues/12\tfingerprint=abc123\tfinding_id=watch.stale_todo:abc123"
	assert.Equal(t, want, got)
}

func TestWatchQualityInputsLoadsBaselineSuppressionsAndGate(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	baselinePath := filepath.Join(root, "baseline.json")
	rulesPath := filepath.Join(root, "rules.json")
	suppressionsPath := filepath.Join(root, "suppressions.json")

	require.NoError(t, os.WriteFile(baselinePath, []byte(`{"findings":[{"path":"old.go","kind":"stale_todo","severity":"maintenance","rule_id":"watch.stale_todo"}]}`), 0o600))
	require.NoError(t, os.WriteFile(rulesPath, []byte(`{"ignore_paths":["ignored.txt"," "],"rules":[{"rule_id":"watch.large_file","severity":"high","help":"move artifact out of git","owner":"platform-quality"},{"rule_id":"watch.stale_todo","disabled":true}]}`), 0o600))
	require.NoError(t, os.WriteFile(suppressionsPath, []byte(`[{"id":"watch.stale_todo:abc123","reason":"tracked in GH-123"}]`), 0o600))

	scanOptions, baseline, baselineInfo, gate, err := watchQualityInputs(context.Background(), root, watchCLIOptions{
		BaselinePath:     baselinePath,
		RulesPath:        rulesPath,
		SuppressionsPath: suppressionsPath,
		GateMinSeverity:  watch.SeverityWarning,
		LargeFileBytes:   128,
	})
	require.NoError(t, err)

	assert.Equal(t, int64(128), scanOptions.LargeFileBytes)
	assert.Equal(t, []string{"ignored.txt"}, scanOptions.IgnorePaths)
	require.Len(t, scanOptions.Rules, 2)
	assert.Equal(t, watch.SeverityHigh, scanOptions.Rules[0].Severity)
	assert.Equal(t, "platform-quality", scanOptions.Rules[0].Owner)
	assert.True(t, scanOptions.Rules[1].Disabled)
	require.Len(t, scanOptions.Suppressions, 1)
	assert.Equal(t, "tracked in GH-123", scanOptions.Suppressions[0].Reason)
	require.NotNil(t, baseline)
	assert.Equal(t, "old.go", baseline.Findings[0].Path)
	require.NotNil(t, baselineInfo)
	assert.Equal(t, "file", baselineInfo.Source)
	assert.Equal(t, baselinePath, baselineInfo.Path)
	assert.Equal(t, 1, baselineInfo.Findings)
	assert.True(t, gate.Enabled)
	assert.Equal(t, watch.SeverityWarning, gate.MinSeverity)
}

func TestReadWatchBaselineAcceptsWatchJSONOutputAndArrayPayload(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	watchJSONPath := filepath.Join(root, "watch-output.json")
	arrayPath := filepath.Join(root, "baseline-array.json")
	finding := watch.Finding{
		Path:     "old.go",
		Kind:     watch.KindStaleTODO,
		Severity: watch.SeverityMaintenance,
		RuleID:   "watch.stale_todo",
	}

	watchJSON, err := json.Marshal(watchScanOutput{
		Comparison: &watch.Comparison{Metrics: watch.TrendMetrics{Unchanged: 1}},
		Gate:       &watch.GateResult{Name: watch.DefaultQualityGateName, Passed: true},
		Findings:   []watch.Finding{finding},
	})
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(watchJSONPath, watchJSON, 0o600))

	arrayJSON, err := json.Marshal([]watch.Finding{finding})
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(arrayPath, arrayJSON, 0o600))

	fromWatchJSON, err := readWatchBaseline(watchJSONPath)
	require.NoError(t, err)
	require.Len(t, fromWatchJSON.Findings, 1)
	assert.Equal(t, "old.go", fromWatchJSON.Findings[0].Path)

	fromArray, err := readWatchBaseline(arrayPath)
	require.NoError(t, err)
	require.Len(t, fromArray.Findings, 1)
	assert.Equal(t, "old.go", fromArray.Findings[0].Path)
}

func TestReadWatchRulesAcceptsArrayPayload(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), "rules.json")
	require.NoError(t, os.WriteFile(path, []byte(`[{"rule_id":"watch.large_file","severity":"high"}]`), 0o600))

	rules, err := readWatchRules(path)
	require.NoError(t, err)

	require.Len(t, rules, 1)
	assert.Equal(t, "watch.large_file", rules[0].RuleID)
	assert.Equal(t, watch.SeverityHigh, rules[0].Severity)
}

func TestWatchIssueOptionsDefaultsLabelsAndValidatesSeverity(t *testing.T) {
	t.Parallel()

	options, err := watchIssueOptions(watchCLIOptions{})
	require.NoError(t, err)

	assert.Empty(t, options.MinSeverity)
	assert.Equal(t, []string{"quality", "watch"}, options.Labels)

	_, err = watchIssueOptions(watchCLIOptions{IssueMinSeverity: "typo"})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "watch issue min severity")
}

func TestWatchQualityInputsLoadsBaselineFromGitBranchPoint(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	runGitForWatchTest(t, root, "init")
	runGitForWatchTest(t, root, "config", "user.email", "watch@example.test")
	runGitForWatchTest(t, root, "config", "user.name", "Watch Test")
	runGitForWatchTest(t, root, "config", "commit.gpgsign", "false")
	require.NoError(t, os.WriteFile(filepath.Join(root, "existing.txt"), []byte("TODO: existing debt\n"), 0o600))
	runGitForWatchTest(t, root, "add", ".")
	runGitForWatchTest(t, root, "commit", "-m", "baseline")
	runGitForWatchTest(t, root, "branch", "-M", "main")
	runGitForWatchTest(t, root, "switch", "-c", "feature")
	require.NoError(t, os.WriteFile(filepath.Join(root, "new.txt"), []byte("TODO: new branch debt\n"), 0o600))

	scanOptions, baseline, baselineInfo, _, err := watchQualityInputs(context.Background(), root, watchCLIOptions{
		BaselineRef:    "main",
		LargeFileBytes: 1024,
	})
	require.NoError(t, err)
	require.NotNil(t, baseline)
	require.NotNil(t, baselineInfo)
	assert.Equal(t, "git_merge_base", baselineInfo.Source)
	assert.Equal(t, "main", baselineInfo.Ref)
	assert.NotEmpty(t, baselineInfo.Commit)

	current, err := watch.ScanWithOptions(root, scanOptions)
	require.NoError(t, err)

	comparison := watch.CompareFindings(baseline.Findings, current)
	assert.Equal(t, []string{"new.txt"}, watchFindingPaths(comparison.NewFindings))
	assert.Equal(t, []string{"existing.txt"}, watchFindingPaths(comparison.UnchangedFindings))
}

func TestUpsertWatchScanIssuesWhenEnabled(t *testing.T) {
	t.Parallel()

	tracker := newFakeWatchIssueTracker()
	finding := watch.Finding{
		Path:     "todo.txt",
		Kind:     watch.KindStaleTODO,
		Severity: watch.SeverityMaintenance,
		RuleID:   "watch." + watch.KindStaleTODO,
		Message:  "contains stale TODO/FIXME marker",
	}
	output := buildWatchScanOutput([]watch.Finding{finding}, nil, nil, watch.GateOptions{})

	err := upsertWatchScanIssues(context.Background(), watchCLIOptions{
		IssueMinSeverity: watch.SeverityMaintenance,
		IssueUpsert:      true,
	}, tracker, &output)
	require.NoError(t, err)

	require.NotNil(t, output.Comparison)
	assert.Equal(t, watch.TrendMetrics{New: 1, Fixed: 0, Unchanged: 0, Suppressed: 0, Unstable: 0}, output.Comparison.Metrics)
	require.Len(t, output.Issues, 1)
	assert.Equal(t, watch.IssueActionCreated, output.Issues[0].Action)
	assert.Equal(t, 1, tracker.createCalls)
	assert.Equal(t, 0, tracker.updateCalls)
}

func TestWatchQualityInputsRejectsInvalidGateSeverity(t *testing.T) {
	t.Parallel()

	_, _, _, _, err := watchQualityInputs(context.Background(), t.TempDir(), watchCLIOptions{GateMinSeverity: "typo"})

	require.Error(t, err)
	assert.Contains(t, err.Error(), "watch gate min severity")
}

func TestWatchQualityInputsRejectsConflictingBaselines(t *testing.T) {
	t.Parallel()

	_, _, _, _, err := watchQualityInputs(context.Background(), t.TempDir(), watchCLIOptions{
		BaselinePath: "baseline.json",
		BaselineRef:  "origin/main",
	})

	require.Error(t, err)
	assert.Contains(t, err.Error(), "use only one of --watch-baseline or --watch-baseline-ref")
}

func TestBuildWatchScanOutputComparesAndEvaluatesGate(t *testing.T) {
	t.Parallel()

	existing := watch.Finding{Path: "old.go", Kind: watch.KindStaleTODO, Severity: watch.SeverityMaintenance, RuleID: "watch.stale_todo"}
	current := watch.Finding{Path: "new.go", Kind: watch.KindConventionDrift, Severity: watch.SeverityHigh, RuleID: "watch.convention_drift"}

	output := buildWatchScanOutput(
		[]watch.Finding{current},
		&watch.Baseline{Findings: []watch.Finding{existing}},
		&watchBaselineInfo{Source: "file", Path: "baseline.json", Findings: 1},
		watch.GateOptions{Enabled: true},
	)

	require.NotNil(t, output.Baseline)
	assert.Equal(t, "baseline.json", output.Baseline.Path)
	require.NotNil(t, output.Comparison)
	assert.Equal(t, watch.TrendMetrics{New: 1, Fixed: 1, Unchanged: 0, Suppressed: 0, Unstable: 0}, output.Comparison.Metrics)
	require.NotNil(t, output.Gate)
	assert.False(t, output.Gate.Passed)
	require.Error(t, watchGateError(output.Gate))
}

func TestRunWatchScanJSONReportsBaselineComparisonAndGate(t *testing.T) { //nolint:paralleltest // captures process-global stdout.
	root := t.TempDir()
	baselineDir := t.TempDir()
	writeWatchScanTestFile(t, root, "existing.txt", "TODO: existing debt\n")

	baselineFindings, err := watch.ScanWithOptions(root, watch.Options{LargeFileBytes: 1024})
	require.NoError(t, err)
	require.Len(t, baselineFindings, 1)

	baselinePath := filepath.Join(baselineDir, "baseline.json")
	baselineData, err := json.Marshal(watch.Baseline{Findings: baselineFindings})
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(baselinePath, baselineData, 0o600))

	writeWatchScanTestFile(t, root, "new.txt", "TODO: new branch regression\n")

	var runErr error

	stdout := captureStdoutForStateDiagnostics(t, func() {
		runErr = runWatchScan(context.Background(), root, watchCLIOptions{
			BaselinePath:    baselinePath,
			GateMinSeverity: watch.SeverityMaintenance,
			LargeFileBytes:  1024,
			JSONOutput:      true,
		})
	})
	require.Error(t, runErr)
	assert.Contains(t, runErr.Error(), "watch gate")

	var output watchScanOutput
	require.NoError(t, json.Unmarshal([]byte(stdout), &output))
	require.NotNil(t, output.Baseline)
	assert.Equal(t, "file", output.Baseline.Source)
	assert.Equal(t, baselinePath, output.Baseline.Path)
	assert.Equal(t, 1, output.Baseline.Findings)
	require.NotNil(t, output.Comparison)
	assert.Equal(t, watch.TrendMetrics{New: 1, Fixed: 0, Unchanged: 1, Suppressed: 0, Unstable: 0}, output.Comparison.Metrics)
	require.NotNil(t, output.Gate)
	assert.False(t, output.Gate.Passed)
	assert.Len(t, output.Gate.BlockingFindings, 1)
	assert.Len(t, output.Findings, 2)
}

func TestRunWatchScanTextReportsBaselineComparisonStatuses(t *testing.T) { //nolint:paralleltest // captures process-global stdout.
	root := t.TempDir()
	baselineDir := t.TempDir()
	writeWatchScanTestFile(t, root, "existing.txt", "TODO: existing debt\n")

	baselineFindings, err := watch.ScanWithOptions(root, watch.Options{LargeFileBytes: 1024})
	require.NoError(t, err)
	require.Len(t, baselineFindings, 1)

	baselinePath := filepath.Join(baselineDir, "baseline.json")
	baselineData, err := json.Marshal(watch.Baseline{Findings: baselineFindings})
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(baselinePath, baselineData, 0o600))

	writeWatchScanTestFile(t, root, "new.txt", "TODO: new branch regression\n")

	var runErr error

	stdout := captureStdoutForStateDiagnostics(t, func() {
		runErr = runWatchScan(context.Background(), root, watchCLIOptions{
			BaselinePath:   baselinePath,
			LargeFileBytes: 1024,
		})
	})
	require.NoError(t, runErr)

	assert.Contains(t, stdout, "watch_comparison\tnew=1\tfixed=0\tunchanged=1\tsuppressed=0\tunstable=0\n")
	assert.Contains(t, stdout, "watch_baseline\tsource=file\tfindings=1\tpath="+baselinePath+"\n")
	assert.Contains(t, stdout, "status=new\tpath=new.txt\tkind=stale_todo\tseverity=maintenance")
	assert.Contains(t, stdout, "status=unchanged\tpath=existing.txt\tkind=stale_todo\tseverity=maintenance")
}

func TestRunWatchScanIssueUpsertCreatesThenUpdatesByFingerprint(t *testing.T) { //nolint:paralleltest // captures process-global stdout.
	root := t.TempDir()
	tracker := newFakeWatchIssueTracker()

	writeWatchScanTestFile(t, root, "todo.txt", "TODO: track this once\n")

	options := watchCLIOptions{
		IssueMinSeverity: watch.SeverityMaintenance,
		IssueUpsert:      true,
		LargeFileBytes:   1024,
	}

	var firstErr error

	firstStdout := captureStdoutForStateDiagnostics(t, func() {
		firstErr = runWatchScanWithIssueTracker(context.Background(), root, options, tracker)
	})
	require.NoError(t, firstErr)
	assert.Equal(t, 1, tracker.createCalls)
	assert.Equal(t, 0, tracker.updateCalls)
	assert.Contains(t, firstStdout, "watch_issue\taction=created")

	var secondErr error

	secondStdout := captureStdoutForStateDiagnostics(t, func() {
		secondErr = runWatchScanWithIssueTracker(context.Background(), root, options, tracker)
	})
	require.NoError(t, secondErr)
	assert.Equal(t, 1, tracker.createCalls)
	assert.Equal(t, 1, tracker.updateCalls)
	assert.Contains(t, secondStdout, "watch_issue\taction=updated")
}

func TestRunWatchScanJSONIncludesIssueUpserts(t *testing.T) { //nolint:paralleltest // captures process-global stdout.
	root := t.TempDir()
	tracker := newFakeWatchIssueTracker()

	writeWatchScanTestFile(t, root, "todo.txt", "TODO: track this through JSON output\n")

	var runErr error

	stdout := captureStdoutForStateDiagnostics(t, func() {
		runErr = runWatchScanWithIssueTracker(context.Background(), root, watchCLIOptions{
			IssueMinSeverity: watch.SeverityMaintenance,
			IssueUpsert:      true,
			LargeFileBytes:   1024,
			JSONOutput:       true,
		}, tracker)
	})
	require.NoError(t, runErr)

	var output watchScanOutput
	require.NoError(t, json.Unmarshal([]byte(stdout), &output))
	require.NotNil(t, output.Comparison)
	assert.Equal(t, watch.TrendMetrics{New: 1, Fixed: 0, Unchanged: 0, Suppressed: 0, Unstable: 0}, output.Comparison.Metrics)
	require.Len(t, output.Issues, 1)
	assert.Equal(t, watch.IssueActionCreated, output.Issues[0].Action)
	assert.Equal(t, 1, tracker.createCalls)
	assert.Equal(t, 0, tracker.updateCalls)
	assert.Len(t, output.Findings, 1)
}

func TestRunWatchScanGateIgnoresSuppressedHighSeverityFindings(t *testing.T) { //nolint:paralleltest // captures process-global stdout.
	root := t.TempDir()
	rulesPath := filepath.Join(t.TempDir(), "rules.json")
	suppressionsPath := filepath.Join(t.TempDir(), "suppressions.json")

	writeWatchScanTestFile(t, root, "todo.txt", "TODO: acknowledged high severity debt\n")
	require.NoError(t, os.WriteFile(rulesPath, []byte(`[{"rule_id":"watch.stale_todo","severity":"high"}]`), 0o600))
	require.NoError(t, os.WriteFile(suppressionsPath, []byte(`[{"rule_id":"watch.stale_todo","path":"todo.txt","reason":"tracked in GH-123"}]`), 0o600))

	var runErr error

	stdout := captureStdoutForStateDiagnostics(t, func() {
		runErr = runWatchScan(context.Background(), root, watchCLIOptions{
			RulesPath:        rulesPath,
			SuppressionsPath: suppressionsPath,
			GateEnabled:      true,
			LargeFileBytes:   1024,
		})
	})
	require.NoError(t, runErr)

	assert.Contains(t, stdout, "watch_comparison\tnew=0\tfixed=0\tunchanged=0\tsuppressed=1\tunstable=0\n")
	assert.Contains(t, stdout, "watch_gate\tname=watch-quality-gate\tpassed=true")
	assert.Contains(t, stdout, "status=suppressed\tpath=todo.txt\tkind=stale_todo\tseverity=high")
	assert.Contains(t, stdout, "suppression_reason=tracked in GH-123")
}

func TestRunWatchScanIssueUpsertSkipsSuppressedHighSeverityFindings(t *testing.T) { //nolint:paralleltest // captures process-global stdout.
	root := t.TempDir()
	tracker := newFakeWatchIssueTracker()
	rulesPath := filepath.Join(t.TempDir(), "rules.json")
	suppressionsPath := filepath.Join(t.TempDir(), "suppressions.json")

	writeWatchScanTestFile(t, root, "todo.txt", "TODO: acknowledged high severity debt\n")
	require.NoError(t, os.WriteFile(rulesPath, []byte(`[{"rule_id":"watch.stale_todo","severity":"high"}]`), 0o600))
	require.NoError(t, os.WriteFile(suppressionsPath, []byte(`[{"rule_id":"watch.stale_todo","path":"todo.txt","reason":"tracked in GH-123"}]`), 0o600))

	var runErr error

	stdout := captureStdoutForStateDiagnostics(t, func() {
		runErr = runWatchScanWithIssueTracker(context.Background(), root, watchCLIOptions{
			RulesPath:        rulesPath,
			SuppressionsPath: suppressionsPath,
			IssueUpsert:      true,
			LargeFileBytes:   1024,
		}, tracker)
	})
	require.NoError(t, runErr)

	assert.Equal(t, 0, tracker.createCalls)
	assert.Equal(t, 0, tracker.updateCalls)
	assert.NotContains(t, stdout, "watch_issue\t")
	assert.Contains(t, stdout, "status=suppressed\tpath=todo.txt\tkind=stale_todo\tseverity=high")
}

func TestRunWatchScanGateAndIssueUpsertIgnoreBaselineHighSeverityDebt(t *testing.T) { //nolint:paralleltest // captures process-global stdout.
	root := t.TempDir()
	baselineDir := t.TempDir()
	rulesPath := filepath.Join(t.TempDir(), "rules.json")
	tracker := newFakeWatchIssueTracker()

	writeWatchScanTestFile(t, root, "todo.txt", "TODO: existing high severity debt\n")
	require.NoError(t, os.WriteFile(rulesPath, []byte(`[{"rule_id":"watch.stale_todo","severity":"high"}]`), 0o600))

	baselineFindings, err := watch.ScanWithOptions(root, watch.Options{
		LargeFileBytes: 1024,
		Rules:          []watch.RuleConfig{{RuleID: "watch.stale_todo", Severity: watch.SeverityHigh}},
	})
	require.NoError(t, err)
	require.Len(t, baselineFindings, 1)
	require.Equal(t, watch.SeverityHigh, baselineFindings[0].Severity)

	baselinePath := filepath.Join(baselineDir, "baseline.json")
	baselineData, err := json.Marshal(watch.Baseline{Findings: baselineFindings})
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(baselinePath, baselineData, 0o600))

	var runErr error

	stdout := captureStdoutForStateDiagnostics(t, func() {
		runErr = runWatchScanWithIssueTracker(context.Background(), root, watchCLIOptions{
			BaselinePath:   baselinePath,
			RulesPath:      rulesPath,
			GateEnabled:    true,
			IssueUpsert:    true,
			LargeFileBytes: 1024,
		}, tracker)
	})
	require.NoError(t, runErr)

	assert.Equal(t, 0, tracker.createCalls)
	assert.Equal(t, 0, tracker.updateCalls)
	assert.Contains(t, stdout, "watch_comparison\tnew=0\tfixed=0\tunchanged=1\tsuppressed=0\tunstable=0\n")
	assert.Contains(t, stdout, "watch_gate\tname=watch-quality-gate\tpassed=true")
	assert.NotContains(t, stdout, "watch_issue\t")
	assert.Contains(t, stdout, "status=unchanged\tpath=todo.txt\tkind=stale_todo\tseverity=high")
}

func TestRunWatchLoopReportsBaselineComparisonStatuses(t *testing.T) { //nolint:paralleltest // captures process-global stdout.
	root := t.TempDir()
	baselineDir := t.TempDir()

	writeWatchScanTestFile(t, root, "existing.txt", "TODO: existing debt\n")

	baselineFindings, err := watch.ScanWithOptions(root, watch.Options{LargeFileBytes: 1024})
	require.NoError(t, err)
	require.Len(t, baselineFindings, 1)

	baselinePath := filepath.Join(baselineDir, "baseline.json")
	baselineData, err := json.Marshal(watch.Baseline{Findings: baselineFindings})
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(baselinePath, baselineData, 0o600))

	writeWatchScanTestFile(t, root, "new.txt", "TODO: new branch regression\n")

	var runErr error

	stdout := captureStdoutForStateDiagnostics(t, func() {
		runErr = runWatchLoop(context.Background(), root, watchCLIOptions{
			BaselinePath:   baselinePath,
			LargeFileBytes: 1024,
		}, 1, 1)
	})
	require.NoError(t, runErr)

	assert.Contains(t, stdout, "watch_baseline\tsource=file\tfindings=1\tpath="+baselinePath+"\n")
	assert.Contains(t, stdout, "iteration=1\tfindings=2")
	assert.Contains(t, stdout, "\tnew=1\tfixed=0\tunchanged=1\tsuppressed=0\tunstable=0")
	assert.Contains(t, stdout, "status=new\tpath=new.txt\tkind=stale_todo\tseverity=maintenance")
	assert.Contains(t, stdout, "status=unchanged\tpath=existing.txt\tkind=stale_todo\tseverity=maintenance")
}

func TestRunReviewScanEmitsWatchGateCheck(t *testing.T) { //nolint:paralleltest // captures process-global stdout.
	root := t.TempDir()
	baselineDir := t.TempDir()
	writeWatchScanTestFile(t, root, "existing.txt", "TODO: existing debt\n")

	baselineFindings, err := watch.ScanWithOptions(root, watch.Options{LargeFileBytes: 1024})
	require.NoError(t, err)
	require.Len(t, baselineFindings, 1)

	baselinePath := filepath.Join(baselineDir, "baseline.json")
	baselineData, err := json.Marshal(watch.Baseline{Findings: baselineFindings})
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(baselinePath, baselineData, 0o600))

	writeWatchScanTestFile(t, root, "new.txt", "TODO: new branch regression\n")

	var runErr error

	stdout := captureStdoutForStateDiagnostics(t, func() {
		runErr = runReviewScan(context.Background(), root, watchCLIOptions{
			BaselinePath:    baselinePath,
			GateMinSeverity: watch.SeverityMaintenance,
			LargeFileBytes:  1024,
		})
	})
	require.Error(t, runErr)
	assert.Contains(t, runErr.Error(), "watch gate")
	assert.Contains(t, stdout, "reviewer: watch-scan\n")
	assert.Contains(t, stdout, "gate_checks:\n")
	assert.Contains(t, stdout, "name=watch-quality-gate\tpassed=false")
	assert.Contains(t, stdout, "blocking_findings=1")
}

func TestRunReviewScanOmitsAcknowledgedBaselineDebt(t *testing.T) { //nolint:paralleltest // captures process-global stdout.
	root := t.TempDir()
	baselineDir := t.TempDir()
	rulesPath := filepath.Join(t.TempDir(), "rules.json")

	writeWatchScanTestFile(t, root, "existing.txt", "TODO: acknowledged high severity debt\n")
	require.NoError(t, os.WriteFile(rulesPath, []byte(`[{"rule_id":"watch.stale_todo","severity":"high"}]`), 0o600))

	baselineFindings, err := watch.ScanWithOptions(root, watch.Options{
		LargeFileBytes: 1024,
		Rules:          []watch.RuleConfig{{RuleID: "watch.stale_todo", Severity: watch.SeverityHigh}},
	})
	require.NoError(t, err)
	require.Len(t, baselineFindings, 1)

	baselinePath := filepath.Join(baselineDir, "baseline.json")
	baselineData, err := json.Marshal(watch.Baseline{Findings: baselineFindings})
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(baselinePath, baselineData, 0o600))

	var runErr error

	stdout := captureStdoutForStateDiagnostics(t, func() {
		runErr = runReviewScan(context.Background(), root, watchCLIOptions{
			BaselinePath:   baselinePath,
			RulesPath:      rulesPath,
			GateEnabled:    true,
			LargeFileBytes: 1024,
		})
	})
	require.NoError(t, runErr)

	assert.Contains(t, stdout, "summary: critical=0 high=0 medium=0 low=0 info=0 total=0\n")
	assert.Contains(t, stdout, "name=watch-quality-gate\tpassed=true")
	assert.Contains(t, stdout, "findings: none\n")
	assert.NotContains(t, stdout, "path=existing.txt")
}

func TestParseAndFormatAsyncPlan(t *testing.T) {
	t.Parallel()

	task, err := parseAsyncTaskSpec("code|coder|implement feature|plan+review")
	if err != nil {
		require.NoError(t, err)
	}

	if task.ID != "code" || task.Agent != "coder" || task.Prompt != "implement feature" {
		require.Failf(t, "unexpected parsed async task", "task = %+v", task)
	}

	if !reflect.DeepEqual(task.DependsOn, []string{"plan", "review"}) {
		require.Failf(t, "unexpected parsed dependencies", "deps = %#v", task.DependsOn)
	}

	plan, err := attasync.NewPlan([]attasync.Task{
		{ID: "plan", Agent: "planner", Prompt: "draft plan"},
		{ID: "review", Agent: "reviewer", Prompt: "review plan", DependsOn: []string{"plan"}},
		{ID: "code", Agent: "coder", Prompt: "implement feature", DependsOn: []string{"plan", "review"}},
	})
	if err != nil {
		require.NoError(t, err)
	}

	got := formatAsyncPlanBatches(plan.ReadyBatches())
	for _, want := range []string{
		"wave 1:\n",
		"id=plan\tagent=planner\tprompt=draft plan",
		"wave 2:\n",
		"id=review\tagent=reviewer\tdepends=plan\tprompt=review plan",
		"wave 3:\n",
		"id=code\tagent=coder\tdepends=plan+review\tprompt=implement feature",
	} {
		if !strings.Contains(got, want) {
			require.Failf(t, "formatted async plan missing content", "missing %q in:\n%s", want, got)
		}
	}
}

func TestValidateAndFormatAsyncRun(t *testing.T) {
	t.Parallel()

	err := validateAsyncRunTasks([]attasync.Task{{ID: "plan", Prompt: "draft"}})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "agent is required")

	err = validateAsyncRunTasks([]attasync.Task{{ID: "plan", Agent: "planner"}})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "prompt is required")

	err = validateAsyncRunTasks([]attasync.Task{{ID: "plan", Agent: "planner", Prompt: "draft"}})
	require.NoError(t, err)

	got := formatAsyncRunResults([]attasync.TaskResult{{
		Wave:           0,
		Order:          0,
		Task:           attasync.Task{ID: "plan", Agent: "planner"},
		Output:         "done\n",
		Status:         attasync.StatusSucceeded,
		LedgerPath:     "/tmp/async-ledger.json",
		TranscriptPath: "/tmp/transcripts/plan.txt",
		Artifacts:      []string{"/tmp/artifacts/plan.patch"},
		Duration:       1500 * time.Millisecond,
	}, {
		Wave:     1,
		Order:    0,
		Task:     attasync.Task{ID: "code", Agent: "coder"},
		Error:    "boom",
		Status:   attasync.StatusFailed,
		Duration: time.Millisecond,
	}})

	assert.Contains(t, got, "wave=1\torder=1\tid=plan\tagent=planner\tstatus=ok\tduration=1.5s")
	assert.Contains(t, got, "ledger=/tmp/async-ledger.json")
	assert.Contains(t, got, "transcript=/tmp/transcripts/plan.txt")
	assert.Contains(t, got, "artifact=/tmp/artifacts/plan.patch")
	assert.Contains(t, got, "output=done")
	assert.Contains(t, got, "wave=2\torder=1\tid=code\tagent=coder\tstatus=error\tduration=1ms")
	assert.Contains(t, got, "error=boom")
}

func TestChildExecutionOptionsFromCLIFlags(t *testing.T) {
	t.Parallel()

	state := appState{
		cwd:           t.TempDir(),
		selectedModel: "openai/gpt-test",
		sessionState:  session.Session{ID: "session-1"},
	}
	ledgerPath := filepath.Join(t.TempDir(), "ledger.json")
	opts := cliOptions{
		spawnLedgerPath:      ledgerPath,
		spawnCancelOnFailure: true,
		spawnResume:          true,
	}
	opts.spawnMaxConcurrency = positiveIntFlag{value: 2}
	opts.spawnTaskTimeout = positiveIntFlag{value: 7}
	opts.spawnRetries = nonNegativeIntFlag{value: 2, set: true}
	opts.spawnRetryBackoff = positiveIntFlag{value: 4, set: true}
	opts.spawnTokenBudget = positiveIntFlag{value: 100}
	opts.spawnCostBudgetMicros = positiveIntFlag{value: 200}
	opts.spawnOutputBudgetBytes = positiveIntFlag{value: 300}

	spawnOpts, err := subagentOptions(state, opts, "spawn")
	require.NoError(t, err)
	assert.Equal(t, ledgerPath, spawnOpts.LedgerPath)
	assert.Equal(t, 2, spawnOpts.MaxConcurrency)
	assert.Equal(t, 7*time.Second, spawnOpts.Timeout)
	assert.Equal(t, 3, spawnOpts.RetryPolicy.MaxAttempts)
	assert.Equal(t, 4*time.Second, spawnOpts.RetryPolicy.Backoff)
	assert.Equal(t, 100, spawnOpts.Budget.MaxPromptTokens)
	assert.Equal(t, int64(200), spawnOpts.Budget.MaxCostMicros)
	assert.Equal(t, int64(300), spawnOpts.Budget.MaxOutputBytes)
	assert.Equal(t, "openai/gpt-test", spawnOpts.Model)
	assert.Equal(t, "openai", spawnOpts.Provider)
	assert.True(t, spawnOpts.CancelOnFailure)
	assert.True(t, spawnOpts.Resume)

	asyncOpts, err := asyncRunOptions(state, opts)
	require.NoError(t, err)
	assert.Equal(t, ledgerPath, asyncOpts.LedgerPath)
	assert.Equal(t, spawnOpts.MaxConcurrency, asyncOpts.MaxConcurrency)
	assert.Equal(t, spawnOpts.Timeout, asyncOpts.Timeout)
	assert.Equal(t, spawnOpts.RetryPolicy.MaxAttempts, asyncOpts.RetryPolicy.MaxAttempts)
	assert.Equal(t, spawnOpts.RetryPolicy.Backoff, asyncOpts.RetryPolicy.Backoff)
	assert.Equal(t, spawnOpts.Budget.MaxPromptTokens, asyncOpts.Budget.MaxPromptTokens)
}

func TestChildExecutionLedgerPathDefaultsUnderIgnoredRunsDir(t *testing.T) {
	t.Parallel()

	cwd := t.TempDir()
	state := appState{cwd: cwd, sessionState: session.Session{ID: "session-1"}}

	ledgerPath, err := childExecutionLedgerPath(state, cliOptions{}, "spawn")
	require.NoError(t, err)
	assert.True(t, strings.HasPrefix(ledgerPath, filepath.Join(cwd, ".atteler", "runs", "spawn-session-1-")))
	assert.True(t, strings.HasSuffix(ledgerPath, filepath.Join("", "ledger.json")))

	_, err = childExecutionLedgerPath(state, cliOptions{spawnResume: true}, "spawn")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "resume requires --spawn-ledger")
}

func TestTaskListHelpers(t *testing.T) {
	t.Parallel()

	assert.Equal(t, taskCommandInput{
		FilePath:   "tasks.json",
		AddTitle:   "write contract tests",
		AddID:      "todo-1",
		Agent:      "planner",
		AssignSpec: "todo-1:executor",
		CompleteID: "todo-1",
		List:       true,
	}, taskCommandInputFromOptions(cliOptions{
		taskFilePath:   "tasks.json",
		taskAddTitle:   "write contract tests",
		taskAddID:      "todo-1",
		taskAgent:      "planner",
		taskAssignSpec: "todo-1:executor",
		taskCompleteID: "todo-1",
		taskList:       true,
	}))

	id, agentName, err := parseTaskAssignmentSpec("todo-1:reviewer")
	require.NoError(t, err)
	assert.Equal(t, "todo-1", id)
	assert.Equal(t, "reviewer", agentName)

	_, _, err = parseTaskAssignmentSpec("todo-1")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "expected id:agent")

	completedAt := time.Date(2026, 5, 5, 12, 30, 0, 0, time.UTC)
	got := formatTaskListItem(tasklist.Task{
		ID:          "todo-1",
		Title:       "wire CLI",
		Status:      tasklist.StatusCompleted,
		Agent:       "reviewer",
		CreatedAt:   completedAt.Add(-time.Hour),
		UpdatedAt:   completedAt,
		CompletedAt: &completedAt,
		Metadata:    map[string]string{"priority": "high", "scope": "cmd"},
	})

	for _, want := range []string{
		"id=todo-1",
		"status=completed",
		"title=wire CLI",
		"agent=reviewer",
		"created_at=2026-05-05T11:30:00Z",
		"updated_at=2026-05-05T12:30:00Z",
		"completed_at=2026-05-05T12:30:00Z",
		"metadata=priority:high,scope:cmd",
	} {
		assert.Contains(t, got, want)
	}
}

func TestRunTaskListCommandPersistsTaskLifecycle(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	taskFile := filepath.Join(t.TempDir(), "tasks.json")
	store := session.NewStore(filepath.Join(t.TempDir(), "sessions"))

	err := runTaskListCommand(ctx, store, taskCommandInput{
		FilePath: taskFile,
		AddID:    "todo-1",
		AddTitle: "draft task package",
		Agent:    "planner",
	})
	require.NoError(t, err)

	err = runTaskListCommand(ctx, store, taskCommandInput{
		FilePath:   taskFile,
		AssignSpec: "todo-1:executor",
	})
	require.NoError(t, err)

	err = runTaskListCommand(ctx, store, taskCommandInput{
		FilePath:   taskFile,
		CompleteID: "todo-1",
		Agent:      "verifier",
	})
	require.NoError(t, err)

	tasks, err := tasklist.NewStore(taskFile).List(ctx)
	require.NoError(t, err)
	require.Len(t, tasks, 1)
	assert.Equal(t, tasklist.StatusCompleted, tasks[0].Status)
	assert.Equal(t, "verifier", tasks[0].Agent)

	history, err := tasklist.NewStore(taskFile).History(ctx)
	require.NoError(t, err)
	assert.Len(t, history, 3)

	err = runTaskListCommand(ctx, store, taskCommandInput{FilePath: taskFile, AddTitle: "new", List: true})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "choose only one")
}

func TestFormatSpeculatePlan(t *testing.T) {
	t.Parallel()

	plan, err := speculate.NewPlan([]string{"alpha", "beta"}, []string{"tests pass"})
	if err != nil {
		require.NoError(t, err)
	}

	got := formatSpeculatePlan(plan)
	for _, want := range []string{
		"agents: alpha,beta\n",
		"rounds:\n",
		"1\tindependent proposals",
		"cross_reviews:\n",
		"alpha -> beta",
		"beta -> alpha",
		"gates:\n  - tests pass\n",
	} {
		if !strings.Contains(got, want) {
			require.Failf(t, "formatted speculate plan missing content", "missing %q in:\n%s", want, got)
		}
	}
}

func TestFormatReviewPlan(t *testing.T) {
	t.Parallel()

	plan, err := review.NewPlan(
		[]review.Reviewer{{Name: "alpha"}, {Name: "beta"}},
		[]string{"pkg/auth.go"},
		[]string{"tests pass"},
	)
	if err != nil {
		require.NoError(t, err)
	}

	got := formatReviewPlan(plan)
	for _, want := range []string{
		"reviewers:\n",
		"  - alpha\n",
		"paths:\n  - pkg/auth.go\n",
		"rounds:\n",
		"1\tindependent-review\tIndependent review\treviewers=alpha,beta",
		"cross_reviews:\n",
		"alpha -> beta",
		"beta -> alpha",
		"gates:\n  - tests pass\n",
	} {
		if !strings.Contains(got, want) {
			require.Failf(t, "formatted review plan missing content", "missing %q in:\n%s", want, got)
		}
	}
}

func TestReviewPlanDefaults(t *testing.T) {
	t.Parallel()

	plan, err := review.NewPlan(reviewPlanReviewers(nil), reviewPlanPaths(nil), nil)
	if err != nil {
		require.NoError(t, err)
	}

	got := formatReviewPlan(plan)
	for _, want := range []string{
		"quality-reviewer\tcategories=correctness,maintainability",
		"test-engineer\tcategories=tests",
		"paths:\n  - .\n",
		"behavioral diff reviewed",
	} {
		if !strings.Contains(got, want) {
			require.Failf(t, "default review plan missing content", "missing %q in:\n%s", want, got)
		}
	}
}

func TestFormatSpeculatePromptCacheEstimate(t *testing.T) {
	t.Parallel()

	plan, err := speculate.NewPlan([]string{"alpha", "beta"}, []string{"tests pass"})
	if err != nil {
		require.NoError(t, err)
	}

	estimate, err := speculate.EstimatePromptCacheReuse(speculateBranchPrompts(plan, "implement auth flow"))
	if err != nil {
		require.NoError(t, err)
	}

	got := formatSpeculatePromptCacheEstimate(estimate)
	for _, want := range []string{
		"prompt_cache:\n",
		"shared_prefix_bytes:",
		"reusable_prompt_bytes:",
		"reuse_ratio:",
		"alpha\tprompt_bytes=",
		"beta\tprompt_bytes=",
	} {
		if !strings.Contains(got, want) {
			require.Failf(t, "formatted speculate prompt cache missing content", "missing %q in:\n%s", want, got)
		}
	}

	if estimate.SharedPrefixBytes == 0 {
		require.FailNow(t, "expected shared branch prompt prefix")
	}
}

type fakeWatchIssueTracker struct {
	issues      map[string]watch.IssueRef
	createCalls int
	updateCalls int
}

func newFakeWatchIssueTracker() *fakeWatchIssueTracker {
	return &fakeWatchIssueTracker{
		issues: make(map[string]watch.IssueRef),
	}
}

func (t *fakeWatchIssueTracker) FindIssueByFingerprint(_ context.Context, fingerprint string) (*watch.IssueRef, error) {
	issue, ok := t.issues[fingerprint]
	if !ok {
		return nil, nil
	}

	return &issue, nil
}

func (t *fakeWatchIssueTracker) CreateIssue(_ context.Context, draft watch.IssueDraft) (watch.IssueRef, error) {
	t.createCalls++
	issue := watch.IssueRef{
		URL:         "https://github.example/issues/" + draft.Fingerprint,
		Fingerprint: draft.Fingerprint,
		Number:      t.createCalls,
	}
	t.issues[draft.Fingerprint] = issue

	return issue, nil
}

func (t *fakeWatchIssueTracker) UpdateIssue(_ context.Context, issue watch.IssueRef, draft watch.IssueDraft) (watch.IssueRef, error) {
	t.updateCalls++
	issue.Fingerprint = draft.Fingerprint
	t.issues[draft.Fingerprint] = issue

	return issue, nil
}

func runGitForWatchTest(t *testing.T, root string, args ...string) {
	t.Helper()

	cmdArgs := append([]string{"-C", root}, args...)
	cmd := exec.CommandContext(t.Context(), "git", cmdArgs...)
	output, err := cmd.CombinedOutput()
	require.NoError(t, err, "git %s\n%s", strings.Join(args, " "), strings.TrimSpace(string(output)))
}

func watchFindingPaths(findings []watch.Finding) []string {
	paths := make([]string, 0, len(findings))
	for i := range findings {
		paths = append(paths, findings[i].Path)
	}

	return paths
}

func writeWatchScanTestFile(t *testing.T, root, path, content string) {
	t.Helper()

	fullPath := filepath.Join(root, path)
	require.NoError(t, os.MkdirAll(filepath.Dir(fullPath), 0o750))
	require.NoError(t, os.WriteFile(fullPath, []byte(content), 0o600))
}
