package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"

	"github.com/tommoulard/atteler/pkg/agent"
	"github.com/tommoulard/atteler/pkg/contextref"
	"github.com/tommoulard/atteler/pkg/llm"
	"github.com/tommoulard/atteler/pkg/review"
	"github.com/tommoulard/atteler/pkg/watch"
)

func runReviewPlan(input reviewPlanCommandInput) error {
	plan, err := review.NewPlan(reviewPlanReviewers(input.Agents), reviewPlanPaths(input.Paths), input.Gates)
	if err != nil {
		return fmt.Errorf("review plan: %w", err)
	}

	fmt.Print(formatReviewPlan(plan))

	return nil
}

func reviewPlanReviewers(names []string) []review.Reviewer {
	if len(names) == 0 {
		return []review.Reviewer{
			{Name: "quality-reviewer", Categories: []review.Category{review.CategoryCorrectness, review.CategoryMaintainability}},
			{Name: "test-engineer", Categories: []review.Category{review.CategoryTests}},
		}
	}

	reviewers := make([]review.Reviewer, 0, len(names))
	for _, name := range names {
		reviewers = append(reviewers, review.Reviewer{Name: strings.TrimSpace(name)})
	}

	return reviewers
}

func reviewPlanPaths(paths []string) []string {
	if len(paths) == 0 {
		return []string{"."}
	}

	return append([]string(nil), paths...)
}

func formatReviewPlan(plan review.Plan) string {
	var b strings.Builder
	b.WriteString("reviewers:\n")

	for _, reviewer := range plan.Reviewers() {
		fmt.Fprintf(&b, "  - %s\n", formatReviewPlanReviewer(reviewer))
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

func formatReviewPlanReviewer(reviewer review.Reviewer) string {
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

func runReviewScan(root string, largeFileBytes int) error {
	findings, err := watch.ScanWithOptions(root, watch.Options{LargeFileBytes: int64(largeFileBytes)})
	if err != nil {
		return fmt.Errorf("review scan: %w", err)
	}

	report := review.Report{
		Reviewer: "watch-scan",
		Findings: watchFindingsToReview(findings),
	}
	fmt.Print(formatReviewReport(report))

	return nil
}

func watchFindingsToReview(findings []watch.Finding) []review.Finding {
	out := make([]review.Finding, 0, len(findings))
	for i := range findings {
		finding := findings[i]
		out = append(out, review.Finding{
			Severity:   reviewSeverity(finding.Severity),
			Category:   reviewCategory(finding.Kind),
			Path:       finding.Path,
			Message:    finding.Message,
			Suggestion: finding.Help,
		})
	}

	return review.SortedFindings(out)
}

func reviewSeverity(severity string) review.Severity {
	switch severity {
	case watch.SeverityWarning:
		return review.SeverityMedium
	case watch.SeverityMaintenance:
		return review.SeverityLow
	default:
		return review.SeverityInfo
	}
}

func reviewCategory(kind string) review.Category {
	switch kind {
	case watch.KindMissingTest:
		return review.CategoryTests
	case watch.KindConventionDrift:
		return review.CategoryMaintainability
	default:
		return review.CategoryMaintainability
	}
}

func formatReviewReport(report review.Report) string {
	var b strings.Builder
	fmt.Fprintf(&b, "reviewer: %s\n", report.Reviewer)
	summary := report.SeveritySummary()
	fmt.Fprintf(&b, "summary: critical=%d high=%d medium=%d low=%d info=%d total=%d\n", summary.Critical, summary.High, summary.Medium, summary.Low, summary.Info, summary.Total())

	findings := report.SortedFindings()
	if len(findings) == 0 {
		b.WriteString("findings: none\n")
		return b.String()
	}

	b.WriteString("findings:\n")

	for i := range findings {
		fmt.Fprintf(&b, "  - %s\n", formatReviewFinding(findings[i]))
	}

	return b.String()
}

func formatReviewFinding(finding review.Finding) string {
	parts := []string{
		"severity=" + string(finding.Severity),
		"category=" + string(finding.Category),
		"path=" + finding.Path,
	}
	if finding.Line > 0 {
		parts = append(parts, "line="+strconv.Itoa(finding.Line))
	}

	if finding.Message != "" {
		parts = append(parts, "message="+finding.Message)
	}

	if finding.Suggestion != "" {
		parts = append(parts, "suggestion="+finding.Suggestion)
	}

	return strings.Join(parts, "\t")
}

type reviewCompleter struct {
	registry       *llm.Registry
	agents         *agent.Registry
	selectedModel  string
	fallbackModels []string
	generationBase generationSettings
	generationOver generationSettings
	modelLocked    bool
}

func (rc *reviewCompleter) Complete(ctx context.Context, reviewer, systemPrompt, userPrompt string) (string, error) {
	activeAgent := agentSelection{name: reviewer}
	if configuredAgent, ok := rc.agents.Get(reviewer); ok {
		activeAgent = agentSelection{name: reviewer, agent: configuredAgent, ok: true}
	}

	generation := generationForRequest(rc.generationBase, rc.generationOver, activeAgent)

	messages := []llm.Message{
		{Role: llm.RoleSystem, Content: systemPrompt},
		{Role: llm.RoleUser, Content: userPrompt},
	}

	requestModel, fallbackModels, _, err := requestModelAndFallbacks(
		rc.selectedModel,
		rc.modelLocked,
		rc.fallbackModels,
		activeAgent,
		routeProfileForMessages(requestMessagesForBudget(rc.selectedModel, messages, activeAgent, generation, ""), generation),
		routeTelemetryFromRegistry(rc.registry),
		routeAvailabilityFromRegistryWithRefresh(ctx, rc.registry, effectiveRouteCandidateChain(rc.selectedModel, rc.fallbackModels, activeAgent, rc.modelLocked)),
	)
	if err != nil {
		return "", err
	}

	params := llm.CompleteParams{
		Model:    requestModel,
		Messages: messages,
	}
	if activeAgent.ok {
		params = activeAgent.agent.CompleteParams(requestModel, messages)
	}

	applyGenerationParams(&params, generation)

	resp, err := rc.registry.CompleteWithFallback(ctx, params, fallbackModels)
	if err != nil {
		return "", fmt.Errorf("review LLM complete: %w", err)
	}

	return resp.Content, nil
}

func runReviewExecution(ctx context.Context, state appState, input reviewRunCommandInput) error {
	plan, err := review.NewPlan(reviewPlanReviewers(input.Agents), reviewPlanPaths(input.Paths), input.Gates)
	if err != nil {
		return fmt.Errorf("review-run: %w", err)
	}

	reviewContext, err := buildReviewContext(ctx, plan.Paths(), input.Prompt, state.contextOptions)
	if err != nil {
		return fmt.Errorf("review-run context: %w", err)
	}

	completer := &reviewCompleter{
		registry:       state.registry,
		agents:         state.agentRegistry,
		selectedModel:  state.selectedModel,
		fallbackModels: state.fallbackModels,
		generationBase: state.generationDefaults,
		generationOver: state.generationOverrides,
		modelLocked:    state.modelLocked,
	}

	reviewerNames := make([]string, 0, len(plan.Reviewers()))
	for _, reviewer := range plan.Reviewers() {
		reviewerNames = append(reviewerNames, reviewer.Name)
	}

	fmt.Fprintln(os.Stderr, "review: running three-round pipeline with "+strings.Join(reviewerNames, ", ")+"...")

	result, err := review.RunWithLLM(ctx, plan, completer, reviewContext)
	if err != nil {
		if len(result.Session.Reports) > 0 || result.Session.Verdict.Reviewer != "" {
			fmt.Print(formatReviewRunResult(result))
		}

		return fmt.Errorf("review-run: %w", err)
	}

	fmt.Print(formatReviewRunResult(result))

	return nil
}

func buildReviewContext(ctx context.Context, paths []string, prompt string, opts contextref.Options) (string, error) {
	opts.ReferenceScope = contextref.ReferenceScopeReview

	refs, err := contextref.LoadReferences(ctx, paths, opts)
	if err != nil {
		return "", fmt.Errorf("load review references: %w", err)
	}

	var b strings.Builder

	prompt = strings.TrimSpace(prompt)
	if prompt != "" {
		b.WriteString("Review instructions:\n")
		b.WriteString(prompt)
		b.WriteString("\n\n")
	}

	if formatted := contextref.FormatReferences(refs); formatted != "" {
		b.WriteString(formatted)
	}

	if strings.TrimSpace(b.String()) == "" {
		return "", errors.New("no review context loaded")
	}

	return b.String(), nil
}

func formatReviewRunResult(result review.Result) string {
	var b strings.Builder

	if len(result.Session.Reports) > 0 {
		b.WriteString("independent_reports:\n")

		for _, report := range result.Session.Reports {
			summary := report.SeveritySummary()
			fmt.Fprintf(
				&b,
				"  - reviewer=%s\tfindings=%d\tcritical=%d\thigh=%d\tmedium=%d\tlow=%d\tinfo=%d\n",
				report.Reviewer,
				summary.Total(),
				summary.Critical,
				summary.High,
				summary.Medium,
				summary.Low,
				summary.Info,
			)
		}
	}

	if len(result.Session.CrossReviews) > 0 {
		b.WriteString("cross_reviews:\n")

		for _, note := range result.Session.CrossReviews {
			fmt.Fprintf(
				&b,
				"  - %s -> %s\tnotes=%s\n",
				note.Reviewer,
				note.ReviewedReviewer,
				truncatePreview(note.Notes, 160),
			)
		}
	}

	report := result.Report
	if report.Reviewer == "" {
		report = result.Session.Verdict
	}

	if report.Reviewer != "" {
		b.WriteString("aggregate_report:\n")
		b.WriteString(formatReviewReport(report))
	}

	return b.String()
}
