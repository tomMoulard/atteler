package review_test

import (
	"fmt"

	"github.com/tommoulard/atteler/pkg/review"
)

func ExampleNewRunPlan() {
	plan, err := review.NewRunPlan(review.RunPlanOptions{
		Reviewers: []review.Reviewer{
			{Name: "quality-reviewer", Categories: []review.Category{review.CategoryCorrectness}},
			{Name: "test-engineer", Categories: []review.Category{review.CategoryTests}},
		},
		Paths: []string{"pkg/sdk"},
	})
	if err != nil {
		panic(err)
	}

	rounds := plan.Rounds()
	for i := range rounds {
		round := rounds[i]
		fmt.Println(round.Number, round.Kind)
	}

	// Output:
	// 1 independent-review
	// 2 cross-review
	// 3 aggregate-verdict
}

func ExampleFormatReport() {
	report := review.Report{
		Reviewer: "quality-reviewer",
		Findings: []review.Finding{{
			Severity: review.SeverityMedium,
			Category: review.CategoryTests,
			Path:     "pkg/sdk/sdk.go",
			Line:     42,
			Message:  "SDK contract needs an example-backed test",
		}},
		GateChecks: []review.GateCheck{{
			Name:   "tests pass",
			Passed: true,
			Proof:  "go test ./pkg/sdk",
		}},
	}

	fmt.Print(review.FormatReport(report))

	// Output:
	// reviewer: quality-reviewer
	// summary: critical=0 high=0 medium=1 low=0 info=0 total=1
	// gate_checks:
	//   - name=tests pass	passed=true	proof=go test ./pkg/sdk
	// findings:
	//   - severity=medium	category=tests	path=pkg/sdk/sdk.go	line=42	message=SDK contract needs an example-backed test
	// gates:
	//   - tests pass: PASS
}
