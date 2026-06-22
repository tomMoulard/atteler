package review

import (
	"fmt"
	"strconv"
	"strings"
)

const (
	formatGateStatusFail = "FAIL"
	formatGateStatusPass = "PASS"
)

// FormatPlan renders a deterministic text representation of a review plan.
func FormatPlan(plan Plan) string {
	var b strings.Builder
	b.WriteString("reviewers:\n")

	for _, reviewer := range plan.Reviewers() {
		fmt.Fprintf(&b, "  - %s\n", formatPlanReviewer(reviewer))
	}

	b.WriteString("paths:\n")

	for _, path := range plan.Paths() {
		fmt.Fprintf(&b, "  - %s\n", path)
	}

	b.WriteString("rounds:\n")

	rounds := plan.Rounds()
	for i := range rounds {
		round := rounds[i]
		fmt.Fprintf(&b, "  - %d\t%s\t%s\treviewers=%s\n", round.Number, round.Kind, round.Name, strings.Join(round.Reviewers, ","))
	}

	if crossReviews := plan.CrossReviews(); len(crossReviews) > 0 {
		b.WriteString("cross_reviews:\n")

		for _, crossReview := range crossReviews {
			fmt.Fprintf(&b, "  - %s -> %s\n", crossReview.Reviewer, crossReview.ReviewedReviewer)
		}
	}

	b.WriteString("gates:\n")

	for _, gate := range plan.RequiredGates() {
		fmt.Fprintf(&b, "  - %s\n", gate)
	}

	return b.String()
}

// FormatReport renders a deterministic text representation of a review report.
func FormatReport(report Report) string {
	var b strings.Builder
	fmt.Fprintf(&b, "reviewer: %s\n", report.Reviewer)
	summary := report.SeveritySummary()
	fmt.Fprintf(&b, "summary: critical=%d high=%d medium=%d low=%d info=%d total=%d\n", summary.Critical, summary.High, summary.Medium, summary.Low, summary.Info, summary.Total())

	if len(report.GateChecks) > 0 {
		b.WriteString("gate_checks:\n")

		for i := range report.GateChecks {
			fmt.Fprintf(&b, "  - %s\n", formatGateCheck(report.GateChecks[i]))
		}
	}

	findings := report.SortedFindings()
	if len(findings) == 0 {
		b.WriteString("findings: none\n")
	} else {
		b.WriteString("findings:\n")

		for i := range findings {
			fmt.Fprintf(&b, "  - %s\n", formatFinding(findings[i]))
		}
	}

	if len(report.GateChecks) > 0 {
		b.WriteString("gates:\n")

		for _, gate := range report.GateChecks {
			status := formatGateStatusFail
			if gate.Passed {
				status = formatGateStatusPass
			}

			fmt.Fprintf(&b, "  - %s: %s %s\n", gate.Name, status, gate.Notes)
		}
	}

	return b.String()
}

func formatPlanReviewer(reviewer Reviewer) string {
	parts := []string{reviewer.Name}
	if len(reviewer.Categories) > 0 {
		categories := make([]string, 0, len(reviewer.Categories))
		for _, category := range reviewer.Categories {
			categories = append(categories, string(category))
		}

		parts = append(parts, "categories="+strings.Join(categories, ","))
	}

	return strings.Join(parts, "\t")
}

func formatGateCheck(check GateCheck) string {
	parts := []string{
		"name=" + check.Name,
		"passed=" + strconv.FormatBool(check.Passed),
	}

	if check.Notes != "" {
		parts = append(parts, "notes="+check.Notes)
	}

	if check.Proof != "" {
		parts = append(parts, "proof="+check.Proof)
	}

	if check.NotRunReason != "" {
		parts = append(parts, "not_run_reason="+check.NotRunReason)
	}

	if len(check.Provenance) > 0 {
		parts = append(parts, "provenance="+formatEvidenceSources(check.Provenance))
	}

	return strings.Join(parts, "\t")
}

func formatFinding(finding Finding) string {
	parts := []string{
		"severity=" + string(finding.Severity),
		"category=" + string(finding.Category),
		"path=" + finding.Path,
	}
	if finding.Line > 0 {
		line := strconv.Itoa(finding.Line)
		if finding.EndLine > finding.Line {
			line += "-" + strconv.Itoa(finding.EndLine)
		}

		parts = append(parts, "line="+line)
	}

	if finding.Message != "" {
		parts = append(parts, "message="+finding.Message)
	}

	if finding.Evidence != "" {
		parts = append(parts, "evidence="+finding.Evidence)
	}

	if finding.SeverityRationale != "" {
		parts = append(parts, "severity_rationale="+finding.SeverityRationale)
	}

	if finding.Suggestion != "" {
		parts = append(parts, "suggestion="+finding.Suggestion)
	}

	if finding.SuggestedVerification != "" {
		parts = append(parts, "suggested_verification="+finding.SuggestedVerification)
	}

	if finding.Confidence != "" {
		parts = append(parts, "confidence="+finding.Confidence)
	}

	if len(finding.Provenance) > 0 {
		parts = append(parts, "provenance="+formatEvidenceSources(finding.Provenance))
	}

	if len(finding.Dissent) > 0 {
		parts = append(parts, "dissent="+formatEvidenceSources(finding.Dissent))
	}

	return strings.Join(parts, "\t")
}

func formatEvidenceSources(sources []EvidenceSource) string {
	parts := make([]string, 0, len(sources))
	for _, source := range sources {
		parts = append(parts, string(source.Type)+":"+source.Source+":"+source.Summary)
	}

	return strings.Join(parts, ";")
}
