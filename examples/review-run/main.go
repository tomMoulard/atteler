// Package main demonstrates building a reusable review-run contract.
package main

import (
	"fmt"
	"log"

	"github.com/tommoulard/atteler/pkg/review"
	"github.com/tommoulard/atteler/pkg/sdk"
)

func main() {
	run, err := sdk.NewReviewRun(sdk.ReviewRunOptions{
		Reviewers: []review.Reviewer{
			{Name: "quality-reviewer", Categories: []review.Category{review.CategoryCorrectness}},
			{Name: "test-engineer", Categories: []review.Category{review.CategoryTests}},
		},
		Paths: []string{"pkg/sdk"},
	})
	if err != nil {
		log.Fatal(err)
	}

	fmt.Print(review.FormatPlan(run.Plan))
}
