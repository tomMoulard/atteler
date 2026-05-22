package review

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
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

// RunError records a failed review-agent stage so malformed model output and
// gate failures remain auditable in partial sessions.
type RunError struct {
	Stage            string
	Reviewer         string
	ReviewedReviewer string
	Message          string
}

// Session captures the artifacts produced by a three-round review-agent run.
type Session struct {
	Plan         Plan
	Reports      []Report
	CrossReviews []CrossReviewNote
	Verdict      Report
	Errors       []RunError
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
	if err := requireRunContext(ctx); err != nil {
		return Result{}, err
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

	reports, runErrors, err := runIndependentReports(ctx, plan.Reviewers(), runner.Review)
	session.Reports = reports
	session.Errors = append(session.Errors, runErrors...)

	if err != nil {
		return Result{Session: session}, err
	}

	if ctxErr := requireRunContext(ctx); ctxErr != nil {
		return Result{Session: session}, ctxErr
	}

	crossReviews, runErrors, err := runCrossReviews(ctx, plan.CrossReviews(), reports, runner.CrossReview)
	session.CrossReviews = crossReviews
	session.Errors = append(session.Errors, runErrors...)

	if err != nil {
		return Result{Session: session}, err
	}

	if ctxErr := requireRunContext(ctx); ctxErr != nil {
		return Result{Session: session}, ctxErr
	}

	verdict, err := runner.Aggregate(ctx, session)
	session.Verdict = verdict

	if err != nil {
		session.Errors = append(session.Errors, RunError{
			Stage:    string(RoundAggregateVerdict),
			Reviewer: reviewJudgeName,
			Message:  err.Error(),
		})

		return Result{Session: session}, fmt.Errorf("aggregate: %w", err)
	}

	if err := ValidateReport(verdict, plan.RequiredGates()); err != nil {
		session.Errors = append(session.Errors, RunError{
			Stage:    string(RoundAggregateVerdict),
			Reviewer: verdict.Reviewer,
			Message:  err.Error(),
		})

		return Result{Session: session}, err
	}

	return Result{Report: verdict, Session: session}, nil
}

func requireRunContext(ctx context.Context) error {
	if ctx == nil {
		return errors.New("review: context is required")
	}

	if err := ctx.Err(); err != nil {
		return fmt.Errorf("review: context already done: %w", err)
	}

	return nil
}

// RunWithLLM executes the full three-round review-agent pipeline using real LLM
// calls through the provided completer. reviewContext must include the reviewed
// file snapshot (for example contextref.FormatReferences output) plus any
// human instructions, so model findings can be validated against real paths and
// line ranges.
func RunWithLLM(ctx context.Context, plan Plan, completer LLMCompleter, reviewContext string) (Result, error) {
	if completer == nil {
		return Result{}, errors.New("LLM completer is required")
	}

	reviewContext = strings.TrimSpace(reviewContext)
	if reviewContext == "" {
		return Result{}, errors.New("review context is required")
	}

	snapshot, err := newReviewSnapshotFromContext(reviewContext)
	if err != nil {
		return Result{}, err
	}

	runner := Runner{
		Review:      llmIndependentReviewer(completer, plan, reviewContext, snapshot),
		CrossReview: llmCrossReviewer(completer),
		Aggregate:   llmReviewAggregator(completer, plan, reviewContext, snapshot),
	}

	return Run(ctx, plan, runner)
}

func runIndependentReports(ctx context.Context, reviewers []Reviewer, review ReportRunner) ([]Report, []RunError, error) {
	reports := make([]Report, len(reviewers))
	errs := make([]error, len(reviewers))
	runErrors := make([]RunError, len(reviewers))

	var wg sync.WaitGroup
	wg.Add(len(reviewers))

	for i, reviewer := range reviewers {
		go func(i int, reviewer Reviewer) {
			defer wg.Done()

			report, err := review(ctx, reviewer)
			if strings.TrimSpace(report.Reviewer) == "" {
				report.Reviewer = reviewer.Name
			}

			report.Findings = SortedFindings(report.Findings)
			reports[i] = report

			if err != nil {
				errs[i] = fmt.Errorf("review %q: %w", reviewer.Name, err)
				runErrors[i] = RunError{
					Stage:    string(RoundIndependentReview),
					Reviewer: reviewer.Name,
					Message:  err.Error(),
				}

				return
			}
		}(i, reviewer)
	}

	wg.Wait()

	if err := firstError(errs); err != nil {
		return reports, compactRunErrors(runErrors), err
	}

	return reports, nil, nil
}

func runCrossReviews(
	ctx context.Context,
	assignments []CrossReview,
	reports []Report,
	crossReview CrossReviewRunner,
) ([]CrossReviewNote, []RunError, error) {
	reportByReviewer := make(map[string]Report, len(reports))
	for _, report := range reports {
		reportByReviewer[report.Reviewer] = report
	}

	notes := make([]CrossReviewNote, len(assignments))
	errs := make([]error, len(assignments))
	runErrors := make([]RunError, len(assignments))

	var wg sync.WaitGroup
	wg.Add(len(assignments))

	for i, assignment := range assignments {
		go func(i int, assignment CrossReview) {
			defer wg.Done()

			targetReport, ok := reportByReviewer[assignment.ReviewedReviewer]
			if !ok {
				errs[i] = fmt.Errorf("review %q -> %q: missing target report", assignment.Reviewer, assignment.ReviewedReviewer)
				runErrors[i] = RunError{
					Stage:            string(RoundCrossReview),
					Reviewer:         assignment.Reviewer,
					ReviewedReviewer: assignment.ReviewedReviewer,
					Message:          "missing target report",
				}

				return
			}

			content, err := crossReview(ctx, assignment, targetReport)
			if err != nil {
				errs[i] = fmt.Errorf("review %q -> %q: %w", assignment.Reviewer, assignment.ReviewedReviewer, err)
				runErrors[i] = RunError{
					Stage:            string(RoundCrossReview),
					Reviewer:         assignment.Reviewer,
					ReviewedReviewer: assignment.ReviewedReviewer,
					Message:          err.Error(),
				}

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
		return notes, compactRunErrors(runErrors), err
	}

	return notes, nil, nil
}

func compactRunErrors(runErrors []RunError) []RunError {
	compacted := make([]RunError, 0, len(runErrors))
	for _, runError := range runErrors {
		if strings.TrimSpace(runError.Message) != "" {
			compacted = append(compacted, runError)
		}
	}

	return compacted
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

type llmReviewResponse struct {
	Reviewer   *string       `json:"reviewer"`
	Findings   *[]llmFinding `json:"findings"`
	GateChecks *[]llmGate    `json:"gate_checks"`
}

type llmFinding struct {
	Severity              *Severity            `json:"severity"`
	Category              *Category            `json:"category"`
	Path                  *string              `json:"path"`
	LineStart             *int                 `json:"line_start"`
	LineEnd               *int                 `json:"line_end"`
	Message               *string              `json:"message"`
	Evidence              *string              `json:"evidence"`
	SeverityRationale     *string              `json:"severity_rationale"`
	Suggestion            *string              `json:"suggestion"`
	SuggestedVerification *string              `json:"suggested_verification"`
	Provenance            *[]llmEvidenceSource `json:"provenance"`
	Dissent               *[]llmEvidenceSource `json:"dissent"`
	Confidence            *string              `json:"confidence"`
}

type llmGate struct {
	Name         *string              `json:"name"`
	Passed       *bool                `json:"passed"`
	Notes        *string              `json:"notes"`
	Proof        *string              `json:"proof"`
	NotRunReason *string              `json:"not_run_reason"`
	Provenance   *[]llmEvidenceSource `json:"provenance"`
}

type llmEvidenceSource struct {
	Type    *EvidenceType `json:"type"`
	Source  *string       `json:"source"`
	Summary *string       `json:"summary"`
}

type llmCrossReviewResponse struct {
	Notes      *string                    `json:"notes"`
	Challenges *[]llmCrossReviewChallenge `json:"challenges"`
}

type llmCrossReviewChallenge struct {
	Finding               *string `json:"finding"`
	Position              *string `json:"position"`
	Rationale             *string `json:"rationale"`
	SuggestedVerification *string `json:"suggested_verification"`
}

func llmIndependentReviewer(completer LLMCompleter, plan Plan, reviewContext string, snapshot reviewSnapshot) ReportRunner {
	return func(ctx context.Context, reviewer Reviewer) (Report, error) {
		systemPrompt := "You are reviewer " + reviewer.Name + " participating in a three-round code review workflow.\n" +
			"Perform an independent review of the supplied code surface. Be specific, actionable, and avoid speculation.\n" +
			"Focus categories: " + categoryList(reviewer.Categories) + ".\n" +
			structuredReviewFormatInstructions(reviewer.Name)

		userPrompt := "Review paths: " + strings.Join(plan.Paths(), ", ") + "\n" +
			"Required gates: " + strings.Join(plan.RequiredGates(), ", ") + "\n\n" +
			"Review context:\n" + reviewContext + "\n\n" +
			"Produce your independent evidence-backed JSON review."

		content, err := completer.Complete(ctx, reviewer.Name, systemPrompt, userPrompt)
		if err != nil {
			return Report{}, fmt.Errorf("independent review LLM call: %w", err)
		}

		return parseReportFromLLM(content, reviewer.Name, plan.RequiredGates(), snapshot)
	}
}

func llmCrossReviewer(completer LLMCompleter) CrossReviewRunner {
	return func(ctx context.Context, assignment CrossReview, report Report) (string, error) {
		systemPrompt := "You are reviewer " + assignment.Reviewer + " cross-reviewing " + assignment.ReviewedReviewer + "'s findings.\n" +
			"Challenge false positives, identify missing risks, and propose sharper wording or gates.\n" +
			"Respond with strict JSON only: {\"notes\":\"concise summary\",\"challenges\":[]}. " +
			"Each challenge requires finding, position, rationale, and suggested_verification. " +
			"position must be support, dispute, uncertain, or missing."

		userPrompt := "Report from " + assignment.ReviewedReviewer + ":\n" + formatReportForPrompt(report) + "\n\n" +
			"Provide concise evidence-aware cross-review JSON."

		content, err := completer.Complete(ctx, assignment.Reviewer, systemPrompt, userPrompt)
		if err != nil {
			return "", fmt.Errorf("cross-review LLM call: %w", err)
		}

		return parseCrossReviewFromLLM(content)
	}
}

func llmReviewAggregator(completer LLMCompleter, plan Plan, reviewContext string, snapshot reviewSnapshot) ReportAggregator {
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
		prompt.WriteString("Deduplicate findings, keep only actionable issues, and evaluate every required gate. ")
		prompt.WriteString("Preserve dissent and uncertainty in finding dissent/provenance fields instead of flattening disagreement.")

		systemPrompt := "You are the review judge aggregating a three-round code review.\n" +
			"Use reviewer findings and cross-review challenges as evidence, not as authority.\n" +
			"Original reviewer model-judgment support sources must be one of: " + strings.Join(reviewerNames(plan.Reviewers()), ", ") + ".\n" +
			structuredReviewFormatInstructions("aggregate-verdict")

		content, err := completer.Complete(ctx, reviewJudgeName, systemPrompt, prompt.String())
		if err != nil {
			return Report{}, fmt.Errorf("aggregator LLM call: %w", err)
		}

		return parseAggregateReportFromLLM(content, "aggregate-verdict", plan.RequiredGates(), reviewerNames(plan.Reviewers()), snapshot)
	}
}

func structuredReviewFormatInstructions(reviewer string) string {
	instructions := "Respond with strict JSON only. Do not wrap it in Markdown and do not emit pipe-delimited text.\n" +
		"Unknown or duplicate JSON fields are invalid. The required top-level object is:\n" +
		"{\"reviewer\":\"" + reviewer + "\",\"findings\":[],\"gate_checks\":[]}.\n" +
		"Each finding object requires severity, category, path, line_start, line_end, message, evidence, severity_rationale, suggested_verification, provenance, and confidence. " +
		"severity must be one of critical, high, medium, low, info. category must be one of correctness, security, tests, performance, maintainability, style. " +
		"path and line range must refer to the reviewed file snapshot. evidence must include a verbatim code excerpt from that range and may add a short summary. confidence must be high, medium, or low. " +
		"Use optional suggestion for the fix and optional dissent for reviewer disagreement.\n" +
		"Each provenance or dissent entry requires type, source, and summary; type must be model-judgment, review-context, or command-output. " +
		"Each finding provenance must include model-judgment from the reviewer plus a review-context source for the cited path and overlapping line range using path:line_start-line_end. " +
		"Use command-output only when both the command/output source and summary are present in a Command output section after the reviewed snapshot; otherwise use model-judgment or review-context.\n" +
		"Each gate_checks object requires name, passed, notes, proof, not_run_reason, and provenance. Emit one gate object for every required gate and no unknown gates. " +
		"Set proof to concrete command/test/lint/typecheck output when available; if a gate was not run, set passed=false and explain why in not_run_reason. " +
		"Every not_run_reason must include model-judgment provenance from the reviewer reporting that the gate was not run. " +
		"Any gate proof must cite review-context or command-output provenance; model-judgment alone is not enough. Command-output gate proof must include the provenance source command and summary output. Review-context gate proof must quote text from the cited source range. " +
		"Test, typecheck, lint, and flake gate proof must include command-output evidence from the supplied context."

	if isAggregateReviewer(reviewer) {
		instructions += " Aggregate finding provenance must include at least one supporting original reviewer or command-output source; the judge's own model-judgment alone is not enough."
	}

	return instructions
}

func parseCrossReviewFromLLM(content string) (string, error) {
	if err := rejectDuplicateJSONFields(content); err != nil {
		return "", fmt.Errorf("parse cross-review JSON: %w", err)
	}

	decoder := json.NewDecoder(strings.NewReader(strings.TrimSpace(content)))
	decoder.DisallowUnknownFields()

	var payload llmCrossReviewResponse
	if err := decoder.Decode(&payload); err != nil {
		return "", fmt.Errorf("parse cross-review JSON: %w", err)
	}

	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		return "", errors.New("parse cross-review JSON: response must contain exactly one JSON object")
	}

	if payload.Notes == nil {
		return "", errors.New("parse cross-review JSON: notes is required")
	}

	if payload.Challenges == nil {
		return "", errors.New("parse cross-review JSON: challenges array is required")
	}

	return formatCrossReviewResponse(payload)
}

func formatCrossReviewResponse(payload llmCrossReviewResponse) (string, error) {
	notes := strings.TrimSpace(derefString(payload.Notes))
	if notes == "" {
		return "", errors.New("cross-review notes are required")
	}

	var b strings.Builder
	b.WriteString(notes)

	challenges := derefChallenges(payload.Challenges)
	for i, challenge := range challenges {
		formatted, err := formatCrossReviewChallenge(challenge)
		if err != nil {
			return "", fmt.Errorf("challenge %d: %w", i, err)
		}

		b.WriteString("\n- ")
		b.WriteString(formatted)
	}

	return b.String(), nil
}

func formatCrossReviewChallenge(challenge llmCrossReviewChallenge) (string, error) {
	var missing []string
	requiredField(&missing, challenge.Finding, "finding")
	requiredField(&missing, challenge.Position, "position")
	requiredField(&missing, challenge.Rationale, "rationale")
	requiredField(&missing, challenge.SuggestedVerification, "suggested_verification")

	if len(missing) > 0 {
		return "", fmt.Errorf("missing required field(s): %s", strings.Join(missing, ", "))
	}

	position := strings.TrimSpace(derefString(challenge.Position))
	if !validCrossReviewPosition(position) {
		return "", fmt.Errorf("invalid position %q", position)
	}

	return fmt.Sprintf(
		"%s %s: %s verification=%s",
		position,
		strings.TrimSpace(derefString(challenge.Finding)),
		strings.TrimSpace(derefString(challenge.Rationale)),
		strings.TrimSpace(derefString(challenge.SuggestedVerification)),
	), nil
}

func validCrossReviewPosition(position string) bool {
	switch position {
	case "support", "dispute", "uncertain", "missing":
		return true
	default:
		return false
	}
}

func parseReportFromLLM(content, expectedReviewer string, requiredGates []string, snapshot reviewSnapshot) (Report, error) {
	return parseReportFromLLMWithOptions(content, expectedReviewer, requiredGates, snapshot, evidenceValidationOptions{})
}

func parseAggregateReportFromLLM(
	content,
	expectedReviewer string,
	requiredGates,
	aggregateSupporters []string,
	snapshot reviewSnapshot,
) (Report, error) {
	return parseReportFromLLMWithOptions(
		content,
		expectedReviewer,
		requiredGates,
		snapshot,
		evidenceValidationOptions{aggregateSupporters: stringSet(aggregateSupporters)},
	)
}

func parseReportFromLLMWithOptions(
	content,
	expectedReviewer string,
	requiredGates []string,
	snapshot reviewSnapshot,
	options evidenceValidationOptions,
) (Report, error) {
	payload, err := decodeStrictLLMReview(content)
	if err != nil {
		return Report{Reviewer: strings.TrimSpace(expectedReviewer)}, err
	}

	report, err := payload.Report(expectedReviewer)
	if err != nil {
		return report, err
	}

	report.Findings = SortedFindings(report.Findings)
	if err := validateEvidenceBackedReport(report, requiredGates, snapshot, options); err != nil {
		return report, err
	}

	return report, nil
}

func stringSet(values []string) map[string]struct{} {
	out := make(map[string]struct{}, len(values))
	for _, value := range values {
		value = strings.TrimSpace(value)
		if value != "" {
			out[value] = struct{}{}
		}
	}

	return out
}

func decodeStrictLLMReview(content string) (llmReviewResponse, error) {
	if err := rejectDuplicateJSONFields(content); err != nil {
		return llmReviewResponse{}, fmt.Errorf("parse review JSON: %w", err)
	}

	decoder := json.NewDecoder(strings.NewReader(strings.TrimSpace(content)))
	decoder.DisallowUnknownFields()

	var payload llmReviewResponse
	if err := decoder.Decode(&payload); err != nil {
		return llmReviewResponse{}, fmt.Errorf("parse review JSON: %w", err)
	}

	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		return llmReviewResponse{}, errors.New("parse review JSON: response must contain exactly one JSON object")
	}

	if payload.Reviewer == nil {
		return llmReviewResponse{}, errors.New("parse review JSON: reviewer is required")
	}

	if payload.Findings == nil {
		return llmReviewResponse{}, errors.New("parse review JSON: findings array is required")
	}

	if payload.GateChecks == nil {
		return llmReviewResponse{}, errors.New("parse review JSON: gate_checks array is required")
	}

	return payload, nil
}

func rejectDuplicateJSONFields(content string) error {
	decoder := json.NewDecoder(strings.NewReader(strings.TrimSpace(content)))

	if err := scanJSONValue(decoder); err != nil {
		return err
	}

	if _, err := decoder.Token(); err == nil {
		return errors.New("response must contain exactly one JSON value")
	} else if !errors.Is(err, io.EOF) {
		return fmt.Errorf("scan trailing JSON token: %w", err)
	}

	return nil
}

func scanJSONValue(decoder *json.Decoder) error {
	token, err := decoder.Token()
	if errors.Is(err, io.EOF) {
		return errors.New("empty JSON response")
	}

	if err != nil {
		return fmt.Errorf("scan JSON token: %w", err)
	}

	delim, ok := token.(json.Delim)
	if !ok {
		return nil
	}

	switch delim {
	case '{':
		return scanJSONObject(decoder)
	case '[':
		return scanJSONArray(decoder)
	default:
		return fmt.Errorf("unexpected JSON delimiter %q", delim)
	}
}

func scanJSONObject(decoder *json.Decoder) error {
	seen := make(map[string]struct{})

	for decoder.More() {
		token, err := decoder.Token()
		if err != nil {
			return fmt.Errorf("scan JSON object key: %w", err)
		}

		key, ok := token.(string)
		if !ok {
			return errors.New("object key must be a string")
		}

		if _, exists := seen[key]; exists {
			return fmt.Errorf("duplicate JSON field %q", key)
		}

		seen[key] = struct{}{}

		if err := scanJSONValue(decoder); err != nil {
			return err
		}
	}

	return consumeJSONDelimiter(decoder, '}')
}

func scanJSONArray(decoder *json.Decoder) error {
	for decoder.More() {
		if err := scanJSONValue(decoder); err != nil {
			return err
		}
	}

	return consumeJSONDelimiter(decoder, ']')
}

func consumeJSONDelimiter(decoder *json.Decoder, want json.Delim) error {
	token, err := decoder.Token()
	if err != nil {
		return fmt.Errorf("scan JSON delimiter: %w", err)
	}

	got, ok := token.(json.Delim)
	if !ok || got != want {
		return fmt.Errorf("expected JSON delimiter %q", want)
	}

	return nil
}

func (payload llmReviewResponse) Report(expectedReviewer string) (Report, error) {
	report := Report{Reviewer: strings.TrimSpace(derefString(payload.Reviewer))}
	expectedReviewer = strings.TrimSpace(expectedReviewer)

	if report.Reviewer == "" {
		return report, errors.New("reviewer is required")
	}

	if expectedReviewer != "" && report.Reviewer != expectedReviewer {
		return report, fmt.Errorf("reviewer %q does not match expected reviewer %q", report.Reviewer, expectedReviewer)
	}

	findings := derefFindings(payload.Findings)

	report.Findings = make([]Finding, 0, len(findings))
	for i, rawFinding := range findings {
		finding, err := rawFinding.Finding()
		if err != nil {
			return report, fmt.Errorf("finding %d: %w", i, err)
		}

		report.Findings = append(report.Findings, finding)
	}

	gates := derefGates(payload.GateChecks)

	report.GateChecks = make([]GateCheck, 0, len(gates))
	for i, rawGate := range gates {
		gate, err := rawGate.GateCheck()
		if err != nil {
			return report, fmt.Errorf("gate check %d: %w", i, err)
		}

		report.GateChecks = append(report.GateChecks, gate)
	}

	return report, nil
}

func (raw llmFinding) Finding() (Finding, error) {
	var missing []string
	requiredField(&missing, raw.Severity, "severity")
	requiredField(&missing, raw.Category, "category")
	requiredField(&missing, raw.Path, "path")
	requiredField(&missing, raw.LineStart, "line_start")
	requiredField(&missing, raw.LineEnd, "line_end")
	requiredField(&missing, raw.Message, "message")
	requiredField(&missing, raw.Evidence, "evidence")
	requiredField(&missing, raw.SeverityRationale, "severity_rationale")
	requiredField(&missing, raw.SuggestedVerification, "suggested_verification")
	requiredField(&missing, raw.Provenance, "provenance")
	requiredField(&missing, raw.Confidence, "confidence")

	if len(missing) > 0 {
		return Finding{}, fmt.Errorf("missing required field(s): %s", strings.Join(missing, ", "))
	}

	provenance, err := evidenceSources(*raw.Provenance)
	if err != nil {
		return Finding{}, fmt.Errorf("provenance: %w", err)
	}

	dissent, err := optionalEvidenceSources(raw.Dissent)
	if err != nil {
		return Finding{}, fmt.Errorf("dissent: %w", err)
	}

	finding := Finding{
		Severity:              Severity(strings.TrimSpace(string(*raw.Severity))),
		Category:              Category(strings.TrimSpace(string(*raw.Category))),
		Path:                  cleanReviewPath(derefString(raw.Path)),
		Line:                  derefInt(raw.LineStart),
		EndLine:               derefInt(raw.LineEnd),
		Message:               strings.TrimSpace(derefString(raw.Message)),
		Evidence:              strings.TrimSpace(derefString(raw.Evidence)),
		SeverityRationale:     strings.TrimSpace(derefString(raw.SeverityRationale)),
		Suggestion:            strings.TrimSpace(derefString(raw.Suggestion)),
		SuggestedVerification: strings.TrimSpace(derefString(raw.SuggestedVerification)),
		Provenance:            provenance,
		Dissent:               dissent,
		Confidence:            strings.TrimSpace(derefString(raw.Confidence)),
	}

	return finding, nil
}

func (raw llmGate) GateCheck() (GateCheck, error) {
	var missing []string
	requiredField(&missing, raw.Name, "name")
	requiredField(&missing, raw.Passed, "passed")
	requiredField(&missing, raw.Notes, "notes")
	requiredField(&missing, raw.Proof, "proof")
	requiredField(&missing, raw.NotRunReason, "not_run_reason")
	requiredField(&missing, raw.Provenance, "provenance")

	if len(missing) > 0 {
		return GateCheck{}, fmt.Errorf("missing required field(s): %s", strings.Join(missing, ", "))
	}

	provenance, err := evidenceSources(*raw.Provenance)
	if err != nil {
		return GateCheck{}, fmt.Errorf("provenance: %w", err)
	}

	return GateCheck{
		Name:         strings.TrimSpace(derefString(raw.Name)),
		Passed:       derefBool(raw.Passed),
		Notes:        strings.TrimSpace(derefString(raw.Notes)),
		Proof:        strings.TrimSpace(derefString(raw.Proof)),
		NotRunReason: strings.TrimSpace(derefString(raw.NotRunReason)),
		Provenance:   provenance,
	}, nil
}

func evidenceSources(raw []llmEvidenceSource) ([]EvidenceSource, error) {
	out := make([]EvidenceSource, 0, len(raw))
	for i, source := range raw {
		var missing []string
		requiredField(&missing, source.Type, "type")
		requiredField(&missing, source.Source, "source")
		requiredField(&missing, source.Summary, "summary")

		if len(missing) > 0 {
			return nil, fmt.Errorf("entry %d missing required field(s): %s", i, strings.Join(missing, ", "))
		}

		out = append(out, EvidenceSource{
			Type:    EvidenceType(strings.TrimSpace(string(*source.Type))),
			Source:  strings.TrimSpace(derefString(source.Source)),
			Summary: strings.TrimSpace(derefString(source.Summary)),
		})
	}

	return out, nil
}

func optionalEvidenceSources(raw *[]llmEvidenceSource) ([]EvidenceSource, error) {
	if raw == nil {
		return nil, nil
	}

	return evidenceSources(*raw)
}

func requiredField[T any](missing *[]string, value *T, name string) {
	if value == nil {
		*missing = append(*missing, name)
	}
}

func derefString(value *string) string {
	if value == nil {
		return ""
	}

	return *value
}

func derefInt(value *int) int {
	if value == nil {
		return 0
	}

	return *value
}

func derefBool(value *bool) bool {
	if value == nil {
		return false
	}

	return *value
}

func derefFindings(value *[]llmFinding) []llmFinding {
	if value == nil {
		return nil
	}

	return *value
}

func derefGates(value *[]llmGate) []llmGate {
	if value == nil {
		return nil
	}

	return *value
}

func derefChallenges(value *[]llmCrossReviewChallenge) []llmCrossReviewChallenge {
	if value == nil {
		return nil
	}

	return *value
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
	formatFindingsForPrompt(&b, report.SortedFindings())
	formatGateChecksForPrompt(&b, report.GateChecks)

	return b.String()
}

func formatFindingsForPrompt(b *strings.Builder, findings []Finding) {
	if len(findings) == 0 {
		b.WriteString("Findings: none\n")
		return
	}

	b.WriteString("Findings:\n")

	for i := range findings {
		formatFindingForPrompt(b, findings[i])
	}
}

func formatFindingForPrompt(b *strings.Builder, finding Finding) {
	fmt.Fprintf(
		b,
		"- %s %s %s:%s %s",
		finding.Severity,
		finding.Category,
		finding.Path,
		formatFindingLineRange(finding),
		finding.Message,
	)

	appendPromptField(b, " evidence=", finding.Evidence)
	appendPromptField(b, " rationale=", finding.SeverityRationale)
	appendPromptField(b, " suggestion=", finding.Suggestion)
	appendPromptField(b, " verification=", finding.SuggestedVerification)
	appendPromptField(b, " confidence=", finding.Confidence)

	if len(finding.Provenance) > 0 {
		fmt.Fprintf(b, " provenance=%s", formatEvidenceSourcesForPrompt(finding.Provenance))
	}

	if len(finding.Dissent) > 0 {
		fmt.Fprintf(b, " dissent=%s", formatEvidenceSourcesForPrompt(finding.Dissent))
	}

	b.WriteByte('\n')
}

func formatGateChecksForPrompt(b *strings.Builder, checks []GateCheck) {
	if len(checks) == 0 {
		return
	}

	b.WriteString("Gates:\n")

	for i := range checks {
		formatGateCheckForPrompt(b, checks[i])
	}
}

func formatGateCheckForPrompt(b *strings.Builder, check GateCheck) {
	status := "FAIL"
	if check.Passed {
		status = "PASS"
	}

	fmt.Fprintf(b, "- %s: %s %s", check.Name, status, check.Notes)

	appendPromptField(b, " proof=", check.Proof)
	appendPromptField(b, " not_run_reason=", check.NotRunReason)

	if len(check.Provenance) > 0 {
		fmt.Fprintf(b, " provenance=%s", formatEvidenceSourcesForPrompt(check.Provenance))
	}

	b.WriteByte('\n')
}

func formatFindingLineRange(finding Finding) string {
	lineRange := strconv.Itoa(finding.Line)
	if finding.EndLine > finding.Line {
		lineRange += "-" + strconv.Itoa(finding.EndLine)
	}

	return lineRange
}

func appendPromptField(b *strings.Builder, prefix, value string) {
	if value != "" {
		b.WriteString(prefix)
		b.WriteString(value)
	}
}

func formatEvidenceSourcesForPrompt(sources []EvidenceSource) string {
	parts := make([]string, 0, len(sources))
	for _, source := range sources {
		parts = append(parts, string(source.Type)+":"+source.Source+":"+source.Summary)
	}

	return strings.Join(parts, "; ")
}
