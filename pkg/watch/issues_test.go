package watch

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestUpsertIssues_CreatesThenUpdatesByFingerprint(t *testing.T) {
	t.Parallel()

	tracker := newFakeIssueTracker()
	finding := testFinding("pkg/new.go", KindConventionDrift, SeverityHigh)
	comparison := CompareFindings(nil, []Finding{finding})
	options := IssueOptions{
		TitlePrefix: "Quality gate",
		MinSeverity: SeverityHigh,
		Labels:      []string{"quality", "watch"},
	}

	created, err := UpsertIssues(context.Background(), tracker, comparison, options)
	require.NoError(t, err)
	require.Len(t, created, 1)
	assert.Equal(t, IssueActionCreated, created[0].Action)
	assert.Equal(t, 1, tracker.createCalls)
	assert.Equal(t, 0, tracker.updateCalls)
	assert.Contains(t, tracker.drafts[finding.Fingerprint].Body, "<!-- atteler-watch:fingerprint="+finding.Fingerprint+" -->")
	assert.Equal(t, []string{"quality", "watch"}, tracker.drafts[finding.Fingerprint].Labels)

	updated, err := UpsertIssues(context.Background(), tracker, comparison, options)
	require.NoError(t, err)
	require.Len(t, updated, 1)
	assert.Equal(t, IssueActionUpdated, updated[0].Action)
	assert.Equal(t, 1, tracker.createCalls)
	assert.Equal(t, 1, tracker.updateCalls)
}

func TestIssueDraftIncludesConfiguredOwner(t *testing.T) {
	t.Parallel()

	finding := testFinding("pkg/new.go", KindConventionDrift, SeverityHigh)
	finding.Owner = "platform-quality"

	draft := issueDraft(finding, IssueOptions{})

	assert.Contains(t, draft.Body, "- Owner: platform-quality")
	assert.Contains(t, draft.Body, "- Rule description:")
	assert.Contains(t, draft.Body, "- Finding ID: `"+finding.ID+"`")
	assert.Contains(t, draft.Body, "- Fingerprint: `"+finding.Fingerprint+"`")
	assert.Contains(t, draft.Body, "Suggested remediation: fix it")
}

func TestIssueDraftUsesFallbackRemediationWhenRuleHelpMissing(t *testing.T) {
	t.Parallel()

	finding := testFinding("pkg/new.go", KindConventionDrift, SeverityHigh)
	finding.Help = ""

	draft := issueDraft(finding, IssueOptions{})

	assert.Contains(t, draft.Body, "Suggested remediation: Review the finding evidence")
}

func TestUpsertIssues_IgnoresSuppressedLowSeverityAndExistingFindings(t *testing.T) {
	t.Parallel()

	tracker := newFakeIssueTracker()
	existingHigh := testFinding("pkg/existing.go", KindConventionDrift, SeverityHigh)
	newWarning := testFinding("pkg/warning.go", KindLargeFile, SeverityWarning)
	suppressedHigh := testFinding("pkg/suppressed.go", KindConventionDrift, SeverityHigh)
	suppressedHigh.Suppressed = true
	suppressedHigh.SuppressionReason = "owned elsewhere"

	comparison := CompareFindings(
		[]Finding{existingHigh},
		[]Finding{existingHigh, newWarning, suppressedHigh},
	)

	results, err := UpsertIssues(context.Background(), tracker, comparison, IssueOptions{})
	require.NoError(t, err)

	assert.Empty(t, results)
	assert.Equal(t, 0, tracker.createCalls)
	assert.Equal(t, 0, tracker.updateCalls)
}

func TestUpsertIssues_HonorsConfiguredSeverityThreshold(t *testing.T) {
	t.Parallel()

	tracker := newFakeIssueTracker()
	warningFinding := testFinding("pkg/warning.go", KindLargeFile, SeverityWarning)
	comparison := CompareFindings(nil, []Finding{warningFinding})

	results, err := UpsertIssues(context.Background(), tracker, comparison, IssueOptions{MinSeverity: SeverityWarning})
	require.NoError(t, err)
	require.Len(t, results, 1)

	assert.Equal(t, IssueActionCreated, results[0].Action)
	assert.Equal(t, warningFinding.Fingerprint, results[0].Finding.Fingerprint)
	assert.Equal(t, 1, tracker.createCalls)
	assert.Equal(t, 0, tracker.updateCalls)
}

func TestUpsertIssues_IgnoresUnstableNewFindings(t *testing.T) {
	t.Parallel()

	tracker := newFakeIssueTracker()
	flakyHigh := testFinding("pkg/flaky.go", KindConventionDrift, SeverityHigh)
	comparison := Comparison{
		NewFindings:      []Finding{flakyHigh},
		UnstableFindings: []Finding{flakyHigh},
		Metrics:          TrendMetrics{New: 1, Unstable: 1},
	}

	results, err := UpsertIssues(context.Background(), tracker, comparison, IssueOptions{})
	require.NoError(t, err)

	assert.Empty(t, results)
	assert.Equal(t, 0, tracker.createCalls)
	assert.Equal(t, 0, tracker.updateCalls)
}

func TestUpsertIssues_DeduplicatesRepeatedNewFingerprints(t *testing.T) {
	t.Parallel()

	tracker := newFakeIssueTracker()
	finding := testFinding("pkg/repeated.go", KindConventionDrift, SeverityHigh)
	duplicate := finding
	duplicate.Message = "same finding surfaced twice by caller"
	comparison := Comparison{
		NewFindings: []Finding{finding, duplicate},
		Metrics:     TrendMetrics{New: 2},
	}

	results, err := UpsertIssues(context.Background(), tracker, comparison, IssueOptions{})
	require.NoError(t, err)

	require.Len(t, results, 1)
	assert.Equal(t, IssueActionCreated, results[0].Action)
	assert.Equal(t, finding.Fingerprint, results[0].Finding.Fingerprint)
	assert.Equal(t, 1, tracker.createCalls)
	assert.Equal(t, 0, tracker.updateCalls)
}

func TestUpsertIssues_RejectsInvalidSeverityThreshold(t *testing.T) {
	t.Parallel()

	tracker := newFakeIssueTracker()
	finding := testFinding("pkg/new.go", KindConventionDrift, SeverityHigh)
	comparison := CompareFindings(nil, []Finding{finding})

	results, err := UpsertIssues(context.Background(), tracker, comparison, IssueOptions{MinSeverity: "typo"})
	require.Error(t, err)

	assert.Nil(t, results)
	assert.Contains(t, err.Error(), "invalid min severity")
	assert.Equal(t, 0, tracker.createCalls)
	assert.Equal(t, 0, tracker.updateCalls)
}

type fakeIssueTracker struct {
	issues      map[string]IssueRef
	drafts      map[string]IssueDraft
	createCalls int
	updateCalls int
}

func newFakeIssueTracker() *fakeIssueTracker {
	return &fakeIssueTracker{
		issues: make(map[string]IssueRef),
		drafts: make(map[string]IssueDraft),
	}
}

func (t *fakeIssueTracker) FindIssueByFingerprint(_ context.Context, fingerprint string) (*IssueRef, error) {
	issue, ok := t.issues[fingerprint]
	if !ok {
		return nil, nil
	}

	return &issue, nil
}

func (t *fakeIssueTracker) CreateIssue(_ context.Context, draft IssueDraft) (IssueRef, error) {
	t.createCalls++
	t.drafts[draft.Fingerprint] = draft
	issue := IssueRef{
		ID:          "issue-" + draft.Fingerprint,
		URL:         "https://github.example/issues/" + draft.Fingerprint,
		Fingerprint: draft.Fingerprint,
		Number:      t.createCalls,
	}
	t.issues[draft.Fingerprint] = issue

	return issue, nil
}

func (t *fakeIssueTracker) UpdateIssue(_ context.Context, issue IssueRef, draft IssueDraft) (IssueRef, error) {
	t.updateCalls++
	t.drafts[draft.Fingerprint] = draft
	issue.Fingerprint = draft.Fingerprint
	t.issues[draft.Fingerprint] = issue

	return issue, nil
}
