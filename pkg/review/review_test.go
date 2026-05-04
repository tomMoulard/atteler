package review

import (
	"reflect"
	"strings"
	"testing"
)

func TestValidateReviewer_AcceptsReviewerWithCategories(t *testing.T) {
	t.Parallel()

	err := ValidateReviewer(Reviewer{
		Name:       "quality-reviewer",
		Categories: []Category{CategoryCorrectness, CategoryTests},
	})
	if err != nil {
		t.Fatalf("ValidateReviewer() error = %v", err)
	}
}

func TestValidateReviewer_RejectsDuplicateCategory(t *testing.T) {
	t.Parallel()

	err := ValidateReviewer(Reviewer{
		Name:       "quality-reviewer",
		Categories: []Category{CategoryTests, CategoryTests},
	})
	if err == nil || !strings.Contains(err.Error(), "duplicate reviewer category") {
		t.Fatalf("ValidateReviewer() error = %v, want duplicate category error", err)
	}
}

func TestValidateRequest_AcceptsChangedFilesOrPaths(t *testing.T) {
	t.Parallel()

	request := Request{
		ChangedFiles: []ChangedFile{{Path: "pkg/review/review.go", Status: "added"}},
		Paths:        []string{"pkg/review/review_test.go"},
	}

	if err := ValidateRequest(request); err != nil {
		t.Fatalf("ValidateRequest() error = %v", err)
	}
}

func TestValidateRequest_RejectsEmptyAndDuplicateTargets(t *testing.T) {
	t.Parallel()

	//nolint:govet // Test table order follows readability.
	tests := []struct {
		name    string
		request Request
		want    string
	}{
		{
			name:    "empty request",
			request: Request{},
			want:    "at least one review path is required",
		},
		{
			name: "empty changed file path",
			request: Request{
				ChangedFiles: []ChangedFile{{Path: " ", Status: "added"}},
			},
			want: "review path is required",
		},
		{
			name: "duplicate path across fields",
			request: Request{
				ChangedFiles: []ChangedFile{{Path: "pkg/review/review.go", Status: "added"}},
				Paths:        []string{"pkg/review/review.go"},
			},
			want: `duplicate review path "pkg/review/review.go"`,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			err := ValidateRequest(test.request)
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("ValidateRequest() error = %v, want %q", err, test.want)
			}
		})
	}
}

func TestValidateFinding_RequiresActionableMetadata(t *testing.T) {
	t.Parallel()

	valid := Finding{
		Severity:   SeverityHigh,
		Category:   CategoryCorrectness,
		Path:       "pkg/review/review.go",
		Line:       42,
		Message:    "nil report can panic",
		Suggestion: "guard the nil case",
	}
	if err := ValidateFinding(valid); err != nil {
		t.Fatalf("ValidateFinding() error = %v", err)
	}

	tests := []struct {
		name    string
		finding Finding
		want    string
	}{
		{
			name:    "invalid severity",
			finding: withSeverity(valid, "urgent"),
			want:    `invalid severity "urgent"`,
		},
		{
			name:    "missing category",
			finding: withCategory(valid, ""),
			want:    "finding category is required",
		},
		{
			name:    "missing path",
			finding: withPath(valid, " "),
			want:    "finding path is required",
		},
		{
			name:    "negative line",
			finding: withLine(valid, -1),
			want:    "finding line must be non-negative",
		},
		{
			name:    "missing message",
			finding: withMessage(valid, "\t"),
			want:    "finding message is required",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			err := ValidateFinding(test.finding)
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("ValidateFinding() error = %v, want %q", err, test.want)
			}
		})
	}
}

func TestValidateGateChecks_RequiresKnownPassingGates(t *testing.T) {
	t.Parallel()

	required := []string{"tests pass", "lint pass"}
	checks := []GateCheck{
		{Name: "tests pass", Passed: true},
		{Name: "lint pass", Passed: true},
	}

	if err := ValidateGateChecks(required, checks); err != nil {
		t.Fatalf("ValidateGateChecks() error = %v", err)
	}
}

func TestValidateGateChecks_RejectsGateFailures(t *testing.T) {
	t.Parallel()

	//nolint:govet // Test table order follows readability.
	tests := []struct {
		name     string
		required []string
		checks   []GateCheck
		want     string
	}{
		{
			name:     "missing gate",
			required: []string{"tests pass", "lint pass"},
			checks:   []GateCheck{{Name: "tests pass", Passed: true}},
			want:     `missing gate check "lint pass"`,
		},
		{
			name:     "failed gate",
			required: []string{"tests pass"},
			checks:   []GateCheck{{Name: "tests pass", Passed: false}},
			want:     `gate check "tests pass" failed`,
		},
		{
			name:     "unknown gate",
			required: []string{"tests pass"},
			checks:   []GateCheck{{Name: "tests pass", Passed: true}, {Name: "deploy complete", Passed: true}},
			want:     `unknown gate check "deploy complete"`,
		},
		{
			name:     "duplicate gate",
			required: []string{"tests pass"},
			checks:   []GateCheck{{Name: "tests pass", Passed: true}, {Name: "tests pass", Passed: true}},
			want:     `duplicate gate check "tests pass"`,
		},
		{
			name:     "duplicate required gate",
			required: []string{"tests pass", "tests pass"},
			checks:   []GateCheck{{Name: "tests pass", Passed: true}},
			want:     `duplicate required gate check "tests pass"`,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			err := ValidateGateChecks(test.required, test.checks)
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("ValidateGateChecks() error = %v, want %q", err, test.want)
			}
		})
	}
}

func TestValidateReport_ValidatesReviewerFindingsAndGates(t *testing.T) {
	t.Parallel()

	report := Report{
		Reviewer: "reviewer",
		Findings: []Finding{
			{
				Severity: SeverityMedium,
				Category: CategoryTests,
				Path:     "pkg/review/review.go",
				Line:     12,
				Message:  "missing edge-case test",
			},
		},
		GateChecks: []GateCheck{{Name: "tests pass", Passed: true}},
	}

	if err := ValidateReport(report, []string{"tests pass"}); err != nil {
		t.Fatalf("ValidateReport() error = %v", err)
	}

	report.Findings[0].Message = ""

	err := ValidateReport(report, []string{"tests pass"})
	if err == nil || !strings.Contains(err.Error(), "finding 0: finding message is required") {
		t.Fatalf("ValidateReport() error = %v, want wrapped finding error", err)
	}
}

func TestSortedFindings_IsDeterministicAndDoesNotMutateInput(t *testing.T) {
	t.Parallel()

	input := []Finding{
		{Severity: SeverityLow, Category: CategoryStyle, Path: "z.go", Line: 9, Message: "style"},
		{Severity: SeverityHigh, Category: CategorySecurity, Path: "b.go", Line: 4, Message: "auth"},
		{Severity: SeverityHigh, Category: CategoryTests, Path: "a.go", Line: 8, Message: "missing test"},
		{Severity: SeverityCritical, Category: CategoryCorrectness, Path: "a.go", Line: 3, Message: "panic"},
		{Severity: SeverityHigh, Category: CategoryCorrectness, Path: "a.go", Line: 2, Message: "wrong branch"},
	}
	original := append([]Finding(nil), input...)

	got := SortedFindings(input)
	want := []Finding{
		{Severity: SeverityCritical, Category: CategoryCorrectness, Path: "a.go", Line: 3, Message: "panic"},
		{Severity: SeverityHigh, Category: CategoryCorrectness, Path: "a.go", Line: 2, Message: "wrong branch"},
		{Severity: SeverityHigh, Category: CategoryTests, Path: "a.go", Line: 8, Message: "missing test"},
		{Severity: SeverityHigh, Category: CategorySecurity, Path: "b.go", Line: 4, Message: "auth"},
		{Severity: SeverityLow, Category: CategoryStyle, Path: "z.go", Line: 9, Message: "style"},
	}

	if !reflect.DeepEqual(got, want) {
		t.Fatalf("SortedFindings() = %#v, want %#v", got, want)
	}

	if !reflect.DeepEqual(input, original) {
		t.Fatalf("SortedFindings() mutated input: %#v", input)
	}
}

func TestReport_SortedFindingsUsesReportFindings(t *testing.T) {
	t.Parallel()

	report := Report{
		Findings: []Finding{
			{Severity: SeverityLow, Category: CategoryStyle, Path: "z.go", Message: "style"},
			{Severity: SeverityHigh, Category: CategorySecurity, Path: "a.go", Message: "auth"},
		},
	}

	got := report.SortedFindings()

	want := []Finding{
		{Severity: SeverityHigh, Category: CategorySecurity, Path: "a.go", Message: "auth"},
		{Severity: SeverityLow, Category: CategoryStyle, Path: "z.go", Message: "style"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("SortedFindings() = %#v, want %#v", got, want)
	}
}

func TestGroupFindingsBySeverity_UsesSeverityOrder(t *testing.T) {
	t.Parallel()

	findings := []Finding{
		{Severity: SeverityLow, Category: CategoryStyle, Path: "c.go", Message: "style"},
		{Severity: SeverityCritical, Category: CategoryCorrectness, Path: "a.go", Message: "panic"},
		{Severity: SeverityHigh, Category: CategorySecurity, Path: "b.go", Message: "auth"},
		{Severity: SeverityHigh, Category: CategoryTests, Path: "a.go", Message: "test"},
	}

	groups := GroupFindingsBySeverity(findings)
	gotKeys := groupKeys(groups)

	wantKeys := []string{"critical", "high", "low"}
	if !reflect.DeepEqual(gotKeys, wantKeys) {
		t.Fatalf("GroupFindingsBySeverity() keys = %v, want %v", gotKeys, wantKeys)
	}

	if got := groups[1].Findings[0].Path; got != "a.go" {
		t.Fatalf("high group first path = %q, want a.go", got)
	}
}

func TestGroupFindingsByCategory_UsesCategoryNameOrder(t *testing.T) {
	t.Parallel()

	findings := []Finding{
		{Severity: SeverityLow, Category: CategoryStyle, Path: "c.go", Message: "style"},
		{Severity: SeverityCritical, Category: CategoryCorrectness, Path: "a.go", Message: "panic"},
		{Severity: SeverityHigh, Category: CategorySecurity, Path: "b.go", Message: "auth"},
	}

	groups := GroupFindingsByCategory(findings)
	gotKeys := groupKeys(groups)

	wantKeys := []string{"correctness", "security", "style"}
	if !reflect.DeepEqual(gotKeys, wantKeys) {
		t.Fatalf("GroupFindingsByCategory() keys = %v, want %v", gotKeys, wantKeys)
	}
}

func TestSummary_CountsSeveritiesAndTotal(t *testing.T) {
	t.Parallel()

	summary := Summary([]Finding{
		{Severity: SeverityCritical},
		{Severity: SeverityHigh},
		{Severity: SeverityHigh},
		{Severity: SeverityMedium},
		{Severity: SeverityLow},
		{Severity: SeverityInfo},
		{Severity: "unknown"},
	})

	want := SeveritySummary{Critical: 1, High: 2, Medium: 1, Low: 1, Info: 1}
	if summary != want {
		t.Fatalf("Summary() = %#v, want %#v", summary, want)
	}

	if got := summary.Total(); got != 6 {
		t.Fatalf("Total() = %d, want 6", got)
	}
}

func TestReport_SeveritySummaryUsesReportFindings(t *testing.T) {
	t.Parallel()

	report := Report{
		Findings: []Finding{
			{Severity: SeverityCritical},
			{Severity: SeverityMedium},
			{Severity: SeverityMedium},
		},
	}

	got := report.SeveritySummary()

	want := SeveritySummary{Critical: 1, Medium: 2}
	if got != want {
		t.Fatalf("SeveritySummary() = %#v, want %#v", got, want)
	}
}

func withSeverity(finding Finding, severity Severity) Finding {
	finding.Severity = severity
	return finding
}

func withCategory(finding Finding, category Category) Finding {
	finding.Category = category
	return finding
}

func withPath(finding Finding, path string) Finding {
	finding.Path = path
	return finding
}

func withLine(finding Finding, line int) Finding {
	finding.Line = line
	return finding
}

func withMessage(finding Finding, message string) Finding {
	finding.Message = message
	return finding
}

func groupKeys(groups []FindingGroup) []string {
	keys := make([]string, len(groups))
	for i, group := range groups {
		keys[i] = group.Key
	}

	return keys
}
