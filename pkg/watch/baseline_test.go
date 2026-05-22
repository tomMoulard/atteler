package watch

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestCompareFindings_DistinguishesNewFixedUnchangedAndSuppressed(t *testing.T) {
	t.Parallel()

	existing := testFinding("pkg/existing.go", KindConventionDrift, SeverityHigh)
	fixed := testFinding("pkg/fixed.go", KindConventionDrift, SeverityHigh)
	newHigh := testFinding("pkg/new.go", KindConventionDrift, SeverityHigh)
	newInfo := testFinding("pkg/info.go", KindMissingTest, SeverityInfo)
	suppressed := testFinding("pkg/suppressed.go", KindConventionDrift, SeverityHigh)
	suppressed.Suppressed = true
	suppressed.SuppressionReason = "tracked in GH-1"

	comparison := CompareFindings(
		[]Finding{existing, fixed},
		[]Finding{existing, newHigh, newInfo, suppressed},
	)

	require.Equal(t, TrendMetrics{
		New:        2,
		Fixed:      1,
		Unchanged:  1,
		Suppressed: 1,
		Unstable:   0,
	}, comparison.Metrics)
	assert.Equal(t, []string{"pkg/info.go", "pkg/new.go"}, findingPaths(comparison.NewFindings))
	assert.Equal(t, []string{"pkg/fixed.go"}, findingPaths(comparison.FixedFindings))
	assert.Equal(t, []string{"pkg/existing.go"}, findingPaths(comparison.UnchangedFindings))
	assert.Equal(t, []string{"pkg/suppressed.go"}, findingPaths(comparison.SuppressedFindings))
}

func TestEvaluateGate_FailsOnlyForNewUnsuppressedHighSeverityFindings(t *testing.T) {
	t.Parallel()

	existingHigh := testFinding("pkg/existing.go", KindConventionDrift, SeverityHigh)
	newHigh := testFinding("pkg/new.go", KindConventionDrift, SeverityHigh)
	newWarning := testFinding("pkg/warning.go", KindLargeFile, SeverityWarning)
	suppressedHigh := testFinding("pkg/suppressed.go", KindConventionDrift, SeverityHigh)
	suppressedHigh.Suppressed = true
	suppressedHigh.SuppressionReason = "accepted generated adapter debt"

	comparison := CompareFindings(
		[]Finding{existingHigh},
		[]Finding{existingHigh, newHigh, newWarning, suppressedHigh},
	)

	result := EvaluateGate(comparison, GateOptions{})

	require.False(t, result.Passed)
	require.Len(t, result.BlockingFindings, 1)
	assert.Equal(t, "pkg/new.go", result.BlockingFindings[0].Path)
	assert.Contains(t, result.Reason, SeverityHigh)
}

func TestEvaluateGate_PassesWhenHighSeverityFindingExistsOnlyInBaseline(t *testing.T) {
	t.Parallel()

	existingHigh := testFinding("pkg/existing.go", KindConventionDrift, SeverityHigh)
	comparison := CompareFindings([]Finding{existingHigh}, []Finding{existingHigh})

	result := EvaluateGate(comparison, GateOptions{})

	assert.True(t, result.Passed)
	assert.Empty(t, result.BlockingFindings)
}

func TestEvaluateGate_IgnoresUnstableNewFindings(t *testing.T) {
	t.Parallel()

	flakyHigh := testFinding("pkg/flaky.go", KindConventionDrift, SeverityHigh)
	comparison := Comparison{
		NewFindings:      []Finding{flakyHigh},
		UnstableFindings: []Finding{flakyHigh},
		Metrics:          TrendMetrics{New: 1, Unstable: 1},
	}

	result := EvaluateGate(comparison, GateOptions{})

	assert.True(t, result.Passed)
	assert.Empty(t, result.BlockingFindings)
	assert.Contains(t, result.Reason, "no new findings")
}

func TestEvaluateGate_FailsClosedForInvalidSeverityThreshold(t *testing.T) {
	t.Parallel()

	result := EvaluateGate(Comparison{}, GateOptions{MinSeverity: "typo"})

	assert.False(t, result.Passed)
	assert.Contains(t, result.Reason, "invalid minimum severity")
}

func TestCompareFindings_TracksDuplicateFingerprintsAsUnstable(t *testing.T) {
	t.Parallel()

	first := testFinding("pkg/dup.go", KindMissingTest, SeverityInfo)
	second := first
	second.Message = "same stable finding repeated by another scan path"

	comparison := CompareFindings(nil, []Finding{first, second})

	assert.Equal(t, TrendMetrics{New: 0, Fixed: 0, Unchanged: 0, Suppressed: 0, Unstable: 1}, comparison.Metrics)
	assert.Empty(t, comparison.NewFindings)
	assert.Equal(t, []string{"pkg/dup.go"}, findingPaths(comparison.UnstableFindings))
}

func TestCompareFindings_SortsEqualPathFindingsByStableIdentity(t *testing.T) {
	t.Parallel()

	laterRule := testFinding("pkg/same.go", KindConventionDrift, SeverityHigh)
	laterRule.RuleID = "watch.zzz"
	laterRule.ID = ""
	laterRule.Fingerprint = ""
	laterRule = completeFindingIdentity(laterRule)

	earlierRule := testFinding("pkg/same.go", KindConventionDrift, SeverityHigh)
	earlierRule.RuleID = "watch.aaa"
	earlierRule.ID = ""
	earlierRule.Fingerprint = ""
	earlierRule = completeFindingIdentity(earlierRule)

	comparison := CompareFindings(nil, []Finding{laterRule, earlierRule})

	require.Len(t, comparison.NewFindings, 2)
	assert.Equal(t, []string{"watch.aaa", "watch.zzz"}, findingRuleIDs(comparison.NewFindings))
}

func TestCompareFindings_ReturnsEmptySlicesForStableJSON(t *testing.T) {
	t.Parallel()

	comparison := CompareFindings(nil, nil)

	assert.NotNil(t, comparison.NewFindings)
	assert.NotNil(t, comparison.FixedFindings)
	assert.NotNil(t, comparison.UnchangedFindings)
	assert.NotNil(t, comparison.SuppressedFindings)
	assert.NotNil(t, comparison.UnstableFindings)
	assert.Equal(t, TrendMetrics{}, comparison.Metrics)
}

func testFinding(path, kind, severity string) Finding {
	return completeFindingIdentity(Finding{
		Path:     path,
		Kind:     kind,
		Message:  "synthetic finding",
		Severity: severity,
		RuleID:   "watch." + kind,
		Help:     "fix it",
	})
}

func findingPaths(findings []Finding) []string {
	paths := make([]string, 0, len(findings))

	for i := range findings {
		finding := findings[i]
		paths = append(paths, finding.Path)
	}

	return paths
}

func findingRuleIDs(findings []Finding) []string {
	ruleIDs := make([]string, 0, len(findings))

	for i := range findings {
		ruleIDs = append(ruleIDs, findings[i].RuleID)
	}

	return ruleIDs
}
