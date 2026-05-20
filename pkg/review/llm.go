package review

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"sync"
)

const reviewJudgeName = "review-judge"

// CrossReviewNote captures one reviewer's challenge of another reviewer's
// independent report.
type CrossReviewNote struct {
	Reviewer         string
	ReviewedReviewer string
	Notes            string
}

// Session captures the artifacts produced by a three-round review-agent run.
type Session struct {
	Plan         Plan
	Reports      []Report
	CrossReviews []CrossReviewNote
	Verdict      Report
}

// Result summarizes a completed review-agent run.
type Result struct {
	Report  Report
	Session Session
}

// ReportRunner produces one reviewer's independent structured report.
type ReportRunner func(ctx context.Context, reviewer Reviewer) (Report, error)

// CrossReviewRunner produces notes for one reviewer challenging another
// reviewer's report.
type CrossReviewRunner func(ctx context.Context, assignment CrossReview, report Report) (string, error)

// ReportAggregator consolidates independent reports and cross-review notes
// into the final structured verdict report.
type ReportAggregator func(ctx context.Context, session Session) (Report, error)

// Runner contains the caller-supplied functions used to execute each
// review-agent round.
type Runner struct {
	Review      ReportRunner
	CrossReview CrossReviewRunner
	Aggregate   ReportAggregator
}

// LLMCompleter is the interface used by RunWithLLM to call an LLM provider.
type LLMCompleter interface {
	Complete(ctx context.Context, reviewer string, systemPrompt string, userPrompt string) (string, error)
}

// Run executes the review-agent workflow: concurrent independent reviews,
// concurrent cross-reviews, then one aggregate verdict. It returns partial
// session data alongside any error encountered.
func Run(ctx context.Context, plan Plan, runner Runner) (Result, error) {
	if ctx == nil {
		return Result{}, errors.New("context is required")
	}

	if err := validateRunnablePlan(plan); err != nil {
		return Result{}, err
	}

	if runner.Review == nil {
		return Result{}, errors.New("review runner is required")
	}

	if runner.CrossReview == nil {
		return Result{}, errors.New("cross-review runner is required")
	}

	if runner.Aggregate == nil {
		return Result{}, errors.New("aggregator is required")
	}

	session := Session{Plan: plan}

	reports, err := runIndependentReports(ctx, plan.Reviewers(), runner.Review)
	session.Reports = reports

	if err != nil {
		return Result{Session: session}, err
	}

	crossReviews, err := runCrossReviews(ctx, plan.CrossReviews(), reports, runner.CrossReview)
	session.CrossReviews = crossReviews

	if err != nil {
		return Result{Session: session}, err
	}

	verdict, err := runner.Aggregate(ctx, session)
	session.Verdict = verdict

	if err != nil {
		return Result{Session: session}, fmt.Errorf("aggregate: %w", err)
	}

	if err := ValidateReport(verdict, plan.RequiredGates()); err != nil {
		return Result{Session: session}, err
	}

	return Result{Report: verdict, Session: session}, nil
}

// RunWithLLM executes the full three-round review-agent pipeline using real LLM
// calls through the provided completer. reviewContext should contain the code
// surface, diff, or files being reviewed plus any human instructions.
func RunWithLLM(ctx context.Context, plan Plan, completer LLMCompleter, reviewContext string) (Result, error) {
	if completer == nil {
		return Result{}, errors.New("LLM completer is required")
	}

	reviewContext = strings.TrimSpace(reviewContext)
	if reviewContext == "" {
		return Result{}, errors.New("review context is required")
	}

	runner := Runner{
		Review:      llmIndependentReviewer(completer, plan, reviewContext),
		CrossReview: llmCrossReviewer(completer),
		Aggregate:   llmReviewAggregator(completer, plan, reviewContext),
	}

	return Run(ctx, plan, runner)
}

func runIndependentReports(ctx context.Context, reviewers []Reviewer, review ReportRunner) ([]Report, error) {
	reports := make([]Report, len(reviewers))
	errs := make([]error, len(reviewers))

	var wg sync.WaitGroup
	wg.Add(len(reviewers))

	for i, reviewer := range reviewers {
		go func(i int, reviewer Reviewer) {
			defer wg.Done()

			report, err := review(ctx, reviewer)
			if err != nil {
				errs[i] = fmt.Errorf("review %q: %w", reviewer.Name, err)
				return
			}

			if strings.TrimSpace(report.Reviewer) == "" {
				report.Reviewer = reviewer.Name
			}

			report.Findings = SortedFindings(report.Findings)
			reports[i] = report
		}(i, reviewer)
	}

	wg.Wait()

	if err := firstError(errs); err != nil {
		return reports, err
	}

	return reports, nil
}

func runCrossReviews(
	ctx context.Context,
	assignments []CrossReview,
	reports []Report,
	crossReview CrossReviewRunner,
) ([]CrossReviewNote, error) {
	reportByReviewer := make(map[string]Report, len(reports))
	for _, report := range reports {
		reportByReviewer[report.Reviewer] = report
	}

	notes := make([]CrossReviewNote, len(assignments))
	errs := make([]error, len(assignments))

	var wg sync.WaitGroup
	wg.Add(len(assignments))

	for i, assignment := range assignments {
		go func(i int, assignment CrossReview) {
			defer wg.Done()

			targetReport, ok := reportByReviewer[assignment.ReviewedReviewer]
			if !ok {
				errs[i] = fmt.Errorf("review %q -> %q: missing target report", assignment.Reviewer, assignment.ReviewedReviewer)
				return
			}

			content, err := crossReview(ctx, assignment, targetReport)
			if err != nil {
				errs[i] = fmt.Errorf("review %q -> %q: %w", assignment.Reviewer, assignment.ReviewedReviewer, err)
				return
			}

			notes[i] = CrossReviewNote{
				Reviewer:         assignment.Reviewer,
				ReviewedReviewer: assignment.ReviewedReviewer,
				Notes:            strings.TrimSpace(content),
			}
		}(i, assignment)
	}

	wg.Wait()

	if err := firstError(errs); err != nil {
		return notes, err
	}

	return notes, nil
}

func validateRunnablePlan(plan Plan) error {
	reviewers := plan.Reviewers()
	if len(reviewers) == 0 {
		return errors.New("at least one reviewer is required")
	}

	for i, reviewer := range reviewers {
		if err := ValidateReviewer(reviewer); err != nil {
			return fmt.Errorf("reviewer %d: %w", i, err)
		}
	}

	if err := ValidateRequest(Request{Paths: plan.Paths()}); err != nil {
		return err
	}

	if _, err := normalizeUnique("required gate", plan.RequiredGates()); err != nil {
		return err
	}

	rounds := plan.Rounds()
	if len(rounds) != 3 ||
		rounds[0].Kind != RoundIndependentReview ||
		rounds[1].Kind != RoundCrossReview ||
		rounds[2].Kind != RoundAggregateVerdict {
		return errors.New("plan must use the fixed three-round review workflow")
	}

	return nil
}

func firstError(errs []error) error {
	for _, err := range errs {
		if err != nil {
			return err
		}
	}

	return nil
}

func llmIndependentReviewer(completer LLMCompleter, plan Plan, reviewContext string) ReportRunner {
	return func(ctx context.Context, reviewer Reviewer) (Report, error) {
		systemPrompt := "You are reviewer " + reviewer.Name + " participating in a three-round code review workflow.\n" +
			"Perform an independent review of the supplied code surface. Be specific, actionable, and avoid speculation.\n" +
			"Focus categories: " + categoryList(reviewer.Categories) + ".\n" +
			structuredReviewFormatInstructions()

		userPrompt := "Review paths: " + strings.Join(plan.Paths(), ", ") + "\n" +
			"Required gates: " + strings.Join(plan.RequiredGates(), ", ") + "\n\n" +
			"Review context:\n" + reviewContext + "\n\n" +
			"Produce your independent structured review."

		content, err := completer.Complete(ctx, reviewer.Name, systemPrompt, userPrompt)
		if err != nil {
			return Report{}, fmt.Errorf("independent review LLM call: %w", err)
		}

		return parseReportFromLLM(content, reviewer.Name), nil
	}
}

func llmCrossReviewer(completer LLMCompleter) CrossReviewRunner {
	return func(ctx context.Context, assignment CrossReview, report Report) (string, error) {
		systemPrompt := "You are reviewer " + assignment.Reviewer + " cross-reviewing " + assignment.ReviewedReviewer + "'s findings.\n" +
			"Challenge false positives, identify missing risks, and propose sharper wording or gates."

		userPrompt := "Report from " + assignment.ReviewedReviewer + ":\n" + formatReportForPrompt(report) + "\n\n" +
			"Provide concise cross-review notes."

		return completer.Complete(ctx, assignment.Reviewer, systemPrompt, userPrompt)
	}
}

func llmReviewAggregator(completer LLMCompleter, plan Plan, reviewContext string) ReportAggregator {
	return func(ctx context.Context, session Session) (Report, error) {
		var prompt strings.Builder

		prompt.WriteString("Review paths: ")
		prompt.WriteString(strings.Join(plan.Paths(), ", "))
		prompt.WriteString("\nRequired gates: ")
		prompt.WriteString(strings.Join(plan.RequiredGates(), ", "))
		prompt.WriteString("\n\nReview context:\n")
		prompt.WriteString(reviewContext)
		prompt.WriteString("\n\nIndependent reports:\n")

		for _, report := range session.Reports {
			fmt.Fprintf(&prompt, "\n--- %s ---\n%s\n", report.Reviewer, formatReportForPrompt(report))
		}

		prompt.WriteString("\nCross-review notes:\n")

		for _, note := range session.CrossReviews {
			fmt.Fprintf(&prompt, "\n--- %s reviewing %s ---\n%s\n", note.Reviewer, note.ReviewedReviewer, note.Notes)
		}

		prompt.WriteString("\nAggregate the evidence into the final structured review verdict. ")
		prompt.WriteString("Deduplicate findings, keep only actionable issues, and evaluate every required gate.")

		systemPrompt := "You are the review judge aggregating a three-round code review.\n" +
			"Use reviewer findings and cross-review challenges as evidence, not as authority.\n" +
			structuredReviewFormatInstructions()

		content, err := completer.Complete(ctx, reviewJudgeName, systemPrompt, prompt.String())
		if err != nil {
			return Report{}, fmt.Errorf("aggregator LLM call: %w", err)
		}

		return parseReportFromLLM(content, "aggregate-verdict"), nil
	}
}

func structuredReviewFormatInstructions() string {
	return "Respond using only these line formats:\n" +
		"FINDING: <critical|high|medium|low|info>|<correctness|security|tests|performance|maintainability|style>|<path>|<line>|<message>|<suggestion>\n" +
		"GATE <gate name>: PASS|FAIL <notes>\n" +
		"Emit one explicit GATE line for every required gate. Omit FINDING lines when there are no actionable findings."
}

func parseReportFromLLM(content, reviewer string) Report {
	report := Report{Reviewer: strings.TrimSpace(reviewer)}

	for rawLine := range strings.SplitSeq(content, "\n") {
		line := strings.TrimSpace(rawLine)
		upper := strings.ToUpper(line)

		switch {
		case strings.HasPrefix(upper, "FINDING:"):
			finding, ok := parseFindingLine(line[len("FINDING:"):])
			if ok {
				report.Findings = append(report.Findings, finding)
			}
		case strings.HasPrefix(upper, "GATE "):
			check := parseGateCheckLine(line[len("GATE "):])
			if check.Name != "" {
				report.GateChecks = append(report.GateChecks, check)
			}
		}
	}

	report.Findings = SortedFindings(report.Findings)

	return report
}

func parseFindingLine(line string) (Finding, bool) {
	parts := strings.SplitN(strings.TrimSpace(line), "|", 6)
	if len(parts) < 5 {
		return Finding{}, false
	}

	path, message := strings.TrimSpace(parts[2]), strings.TrimSpace(parts[4])

	if path == "" || message == "" {
		return Finding{}, false
	}

	lineNumber, err := strconv.Atoi(strings.TrimSpace(parts[3]))
	if err != nil {
		lineNumber = 0
	}

	if lineNumber < 0 {
		lineNumber = 0
	}

	finding := Finding{
		Severity: normalizeSeverity(parts[0]),
		Category: normalizeCategory(parts[1]),
		Path:     path,
		Line:     lineNumber,
		Message:  message,
	}

	if len(parts) == 6 {
		finding.Suggestion = strings.TrimSpace(parts[5])
	}

	return finding, true
}

func parseGateCheckLine(line string) GateCheck {
	name, rest, found := strings.Cut(line, ":")
	if !found {
		return GateCheck{}
	}

	name = strings.TrimSpace(name)
	rest = strings.TrimSpace(rest)
	upper := strings.ToUpper(rest)

	check := GateCheck{Name: name}

	switch {
	case strings.HasPrefix(upper, "PASS"):
		check.Passed = true
		check.Notes = strings.TrimSpace(rest[len("PASS"):])
	case strings.HasPrefix(upper, "FAIL"):
		check.Passed = false
		check.Notes = strings.TrimSpace(rest[len("FAIL"):])
	default:
		check.Notes = rest
	}

	return check
}

func normalizeSeverity(value string) Severity {
	severity := Severity(strings.ToLower(strings.TrimSpace(value)))
	if validSeverity(severity) {
		return severity
	}

	return SeverityInfo
}

func normalizeCategory(value string) Category {
	category := Category(strings.ToLower(strings.TrimSpace(value)))
	switch category {
	case CategoryCorrectness, CategorySecurity, CategoryTests, CategoryPerformance, CategoryMaintainability, CategoryStyle:
		return category
	default:
		return CategoryMaintainability
	}
}

func categoryList(categories []Category) string {
	if len(categories) == 0 {
		return "correctness, security, tests, performance, maintainability, style"
	}

	out := make([]string, 0, len(categories))
	for _, category := range categories {
		out = append(out, string(category))
	}

	return strings.Join(out, ", ")
}

func formatReportForPrompt(report Report) string {
	var b strings.Builder

	fmt.Fprintf(&b, "Reviewer: %s\n", report.Reviewer)

	if len(report.Findings) == 0 {
		b.WriteString("Findings: none\n")
	} else {
		b.WriteString("Findings:\n")

		for _, finding := range report.SortedFindings() {
			fmt.Fprintf(
				&b,
				"- %s %s %s:%d %s",
				finding.Severity,
				finding.Category,
				finding.Path,
				finding.Line,
				finding.Message,
			)

			if finding.Suggestion != "" {
				fmt.Fprintf(&b, " suggestion=%s", finding.Suggestion)
			}

			b.WriteByte('\n')
		}
	}

	if len(report.GateChecks) > 0 {
		b.WriteString("Gates:\n")

		for _, check := range report.GateChecks {
			status := "FAIL"
			if check.Passed {
				status = "PASS"
			}

			fmt.Fprintf(&b, "- %s: %s %s\n", check.Name, status, check.Notes)
		}
	}

	return b.String()
}
