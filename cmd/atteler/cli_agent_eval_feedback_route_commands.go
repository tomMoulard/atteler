package main

import (
	"context"
	"errors"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/tommoulard/atteler/pkg/agent"
	appconfig "github.com/tommoulard/atteler/pkg/config"
	atteval "github.com/tommoulard/atteler/pkg/eval"
	"github.com/tommoulard/atteler/pkg/feedback"
	"github.com/tommoulard/atteler/pkg/modelroute"
	"github.com/tommoulard/atteler/pkg/promptcomplete"
	"github.com/tommoulard/atteler/pkg/session"
	attskill "github.com/tommoulard/atteler/pkg/skill"
)

func planAgents(registry *agent.Registry, prompt string, requested []string, maxAgents int, recentAgentNames ...[]string) error {
	request := agent.OrchestrationRequest{
		Prompt:          prompt,
		RequestedNames:  requested,
		MaxParticipants: maxAgents,
	}
	if len(recentAgentNames) > 0 {
		request.RecentAgentNames = append([]string(nil), recentAgentNames[0]...)
	}

	return planAgentsWithRequest(registry, request)
}

func planAgentsWithRequest(registry *agent.Registry, request agent.OrchestrationRequest) error {
	plan, err := registry.PlanOrchestration(request)
	if err != nil {
		return fmt.Errorf("plan agents: %w", err)
	}

	if len(plan.Participants) == 0 {
		fmt.Println("No agents matched.")
	} else {
		for i := range plan.Participants {
			fmt.Println(formatAgentPlanParticipant(&plan.Participants[i]))
		}
	}

	fmt.Println(formatAgentPlanComposition(plan.Composition))
	fmt.Println(formatAgentPlanMetadata(plan.Metadata))

	for i := range plan.Composition.Roles {
		fmt.Println(formatAgentPlanRole(plan.Composition.Roles[i]))
	}

	for i := range plan.Composition.Dependencies {
		fmt.Println(formatAgentPlanDependency(plan.Composition.Dependencies[i]))
	}

	for i := range plan.Ambiguities {
		fmt.Println(formatAgentPlanAmbiguity(plan.Ambiguities[i]))
	}

	for i := range plan.Candidates {
		fmt.Println(formatAgentPlanCandidate(&plan.Candidates[i]))
	}

	return nil
}

func recentAgentNamesForPlan(state appState) []string {
	return recentAgentNamesForSelection(state.selectedAgent, state.sessionState)
}

func recentAgentNamesForSelection(selectedAgent string, sessionState session.Session) []string {
	names := make([]string, 0, 4)
	names = appendPlanAgentName(names, selectedAgent)
	names = appendPlanAgentName(names, sessionState.DefaultAgent)

	for i := len(sessionState.Evaluations) - 1; i >= 0; i-- {
		names = appendPlanAgentName(names, sessionState.Evaluations[i].Agent)
	}

	for i := len(sessionState.Artifacts) - 1; i >= 0; i-- {
		names = appendPlanAgentName(names, sessionState.Artifacts[i].SourceAgent)
	}

	return names
}

func appendPlanAgentName(names []string, name string) []string {
	name = strings.TrimSpace(name)
	if name == "" || containsString(names, name) {
		return names
	}

	return append(names, name)
}

func formatAgentPlanParticipant(participant *agent.Participant) string {
	parts := []string{participant.Agent.Name, "source=" + participant.Source}
	if participant.Pattern != "" {
		parts = append(parts, "match="+participant.Pattern)
	}

	parts = append(parts, fmt.Sprintf("score=%.1f", participant.Score))

	if len(participant.Roles) > 0 {
		parts = append(parts, "roles="+strings.Join(participant.Roles, ","))
	}

	if participant.Rationale != "" {
		parts = append(parts, "rationale="+singleLineAgentPlanField(participant.Rationale))
	}

	if toolSummary := formatToolConstraints(participant.ToolConstraints); toolSummary != "" {
		parts = append(parts, "tools="+toolSummary)
	}

	if len(participant.Agent.Capabilities) > 0 {
		parts = append(parts, "capabilities="+strings.Join(participant.Agent.Capabilities, ","))
	}

	if participant.Agent.Model != "" {
		parts = append(parts, "model="+participant.Agent.Model)
	}

	return strings.Join(parts, "\t")
}

func formatAgentPlanComposition(composition agent.PlanComposition) string {
	parts := []string{
		"composition",
		fmt.Sprintf("max_concurrency=%d", composition.MaxConcurrency),
		"stop=" + emptyPlanValue(composition.StopReason),
		fmt.Sprintf("selected=%d", composition.Budget.SelectedCount),
		fmt.Sprintf("candidates=%d", composition.Budget.CandidateCount),
	}
	if composition.Budget.MaxParticipants > 0 {
		parts = append(parts, fmt.Sprintf("max_participants=%d", composition.Budget.MaxParticipants))
	}

	if len(composition.Budget.Truncated) > 0 {
		parts = append(parts, "truncated="+strings.Join(composition.Budget.Truncated, ","))
	}

	if len(composition.RequiredTools) > 0 {
		parts = append(parts, "required_tools="+strings.Join(composition.RequiredTools, ","))
	}

	return strings.Join(parts, "\t")
}

func formatAgentPlanMetadata(metadata agent.PlanMetadata) string {
	parts := []string{
		"metadata",
		"scoring_version=" + emptyPlanValue(metadata.ScoringVersion),
	}

	if len(metadata.RequestedRoles) > 0 {
		parts = append(parts, "requested_roles="+strings.Join(metadata.RequestedRoles, ","))
	}

	if len(metadata.RequestedNames) > 0 {
		parts = append(parts, "requested_agents="+strings.Join(metadata.RequestedNames, ","))
	}

	if len(metadata.RecentAgentNames) > 0 {
		parts = append(parts, "recent_agents="+strings.Join(metadata.RecentAgentNames, ","))
	}

	if len(metadata.RequiredTools) > 0 {
		parts = append(parts, "required_tools="+strings.Join(metadata.RequiredTools, ","))
	}

	if len(metadata.SelectedNames) > 0 {
		parts = append(parts, "selected_agents="+strings.Join(metadata.SelectedNames, ","))
	}

	if len(metadata.CandidateNames) > 0 {
		parts = append(parts, "candidate_agents="+strings.Join(metadata.CandidateNames, ","))
	}

	if len(metadata.RejectedNames) > 0 {
		parts = append(parts, "rejected_agents="+strings.Join(metadata.RejectedNames, ","))
	}

	if len(metadata.ToolOverrideNames) > 0 {
		parts = append(parts, "tool_override_agents="+strings.Join(metadata.ToolOverrideNames, ","))
	}

	if metadata.SelectionThreshold > 0 {
		parts = append(parts, fmt.Sprintf("selection_threshold=%.1f", metadata.SelectionThreshold))
	}

	if metadata.AmbiguityScoreWindow > 0 {
		parts = append(parts, fmt.Sprintf("ambiguity_window=%.1f", metadata.AmbiguityScoreWindow))
	}

	if metadata.AmbiguityCount > 0 {
		parts = append(parts, fmt.Sprintf("ambiguity_count=%d", metadata.AmbiguityCount))
	}

	if len(metadata.PromptTokens) > 0 {
		parts = append(parts, "prompt_tokens="+strings.Join(metadata.PromptTokens, ","))
	}

	return strings.Join(parts, "\t")
}

func formatAgentPlanRole(role agent.PlannedRole) string {
	parts := []string{
		"role",
		"name=" + role.Name,
		fmt.Sprintf("covered=%t", role.Covered),
		fmt.Sprintf("required=%t", role.Required),
	}
	if role.Agent != "" {
		parts = append(parts, "agent="+role.Agent)
	}

	return strings.Join(parts, "\t")
}

func formatAgentPlanDependency(dependency agent.PlanDependency) string {
	return strings.Join([]string{
		"dependency",
		"before=" + dependency.Before,
		"after=" + dependency.After,
		"reason=" + singleLineAgentPlanField(dependency.Reason),
	}, "\t")
}

func formatAgentPlanAmbiguity(ambiguity agent.Ambiguity) string {
	candidates := make([]string, 0, len(ambiguity.Candidates))
	evidence := make([]string, 0, len(ambiguity.Candidates))

	for _, candidate := range ambiguity.Candidates {
		candidates = append(candidates, fmt.Sprintf("%s=%.1f", candidate.Name, candidate.Score))
		if formatted := formatMatchEvidence(candidate.Evidence); formatted != "" {
			evidence = append(evidence, candidate.Name+"["+formatted+"]")
		}
	}

	parts := []string{
		"ambiguity",
		"role=" + ambiguity.Role,
		"winner=" + ambiguity.Winner,
		"action=override_required",
		"candidates=" + strings.Join(candidates, ","),
		"reason=" + singleLineAgentPlanField(ambiguity.Reason),
	}

	if len(evidence) > 0 {
		parts = append(parts, "evidence="+strings.Join(evidence, ","))
	}

	return strings.Join(parts, "\t")
}

func formatAgentPlanCandidate(candidate *agent.Candidate) string {
	parts := []string{
		"candidate",
		"name=" + candidate.Agent.Name,
		fmt.Sprintf("score=%.1f", candidate.Score),
		fmt.Sprintf("eligible=%t", candidate.Eligible),
	}
	if candidate.Source != "" {
		parts = append(parts, "source="+candidate.Source)
	}

	if candidate.Pattern != "" {
		parts = append(parts, "match="+candidate.Pattern)
	}

	if len(candidate.Roles) > 0 {
		parts = append(parts, "roles="+strings.Join(candidate.Roles, ","))
	}

	if evidence := formatMatchEvidence(candidate.Evidence); evidence != "" {
		parts = append(parts, "evidence="+evidence)
	}

	if candidate.RejectedReason != "" {
		parts = append(parts, "rejected="+singleLineAgentPlanField(candidate.RejectedReason))
	}

	if candidate.Rationale != "" {
		parts = append(parts, "rationale="+singleLineAgentPlanField(candidate.Rationale))
	}

	if toolSummary := formatToolConstraints(candidate.ToolConstraints); toolSummary != "" {
		parts = append(parts, "tools="+toolSummary)
	}

	return strings.Join(parts, "\t")
}

func formatMatchEvidence(evidence []agent.MatchEvidence) string {
	if len(evidence) == 0 {
		return ""
	}

	parts := make([]string, 0, len(evidence))
	for _, item := range evidence {
		label := item.Kind
		if item.Pattern != "" {
			label += ":" + item.Pattern
		}

		value := fmt.Sprintf("%s=%.1f", label, item.Score)
		if item.Kind == agent.ParticipantSourceTrigger || item.Kind == agent.ParticipantSourceCapability {
			value += fmt.Sprintf("@%d", item.TokenIndex)
		}

		parts = append(parts, value)
	}

	return strings.Join(parts, ",")
}

func formatToolConstraints(constraints []agent.ToolConstraint) string {
	if len(constraints) == 0 {
		return ""
	}

	parts := make([]string, 0, len(constraints))
	for _, constraint := range constraints {
		status := "denied"
		if constraint.Allowed {
			status = "allowed"
		}

		parts = append(parts, constraint.Tool+":"+status)
	}

	return strings.Join(parts, ",")
}

func singleLineAgentPlanField(value string) string {
	value = strings.ReplaceAll(value, "\t", " ")
	value = strings.ReplaceAll(value, "\n", " ")

	return strings.TrimSpace(value)
}

func emptyPlanValue(value string) string {
	if value == "" {
		return "-"
	}

	return value
}

func evalOutput(actualPath, expectedText, expectedPath string, mode atteval.MatchMode) error {
	actual, err := os.ReadFile(actualPath)
	if err != nil {
		return fmt.Errorf("eval output: read actual %s: %w", actualPath, err)
	}

	expected, err := expectedEvalText(expectedText, expectedPath)
	if err != nil {
		return err
	}

	result := atteval.Check(string(actual), expected, mode)
	if result.Passed {
		fmt.Printf("PASS\tmode=%s\tactual=%s\n", result.Mode, actualPath)
		return nil
	}

	report := result.Failure()
	if report == "" {
		report = result.Summary
	}

	fmt.Printf("FAIL\tmode=%s\tactual=%s\n%s\n", result.Mode, actualPath, report)

	return errors.New("eval output failed")
}

func evalCommandRequested(opts cliOptions) bool {
	return opts.evalOutputPath != "" ||
		opts.evalAssertionsPath != "" ||
		opts.evalFixtureDir != "" ||
		opts.evalExpected != "" ||
		opts.evalExpectedPath != "" ||
		opts.evalJSON ||
		opts.evalReportPath != "" ||
		opts.evalUpdateGolden ||
		opts.evalApproveGoldenUpdate ||
		opts.evalExitCode.set
}

func evalOutputCommand(opts cliOptions) error {
	if structuredEvalRequested(opts) || opts.evalJSON || opts.evalReportPath != "" {
		report, err := evalReportForCommand(opts)
		if err != nil {
			return err
		}

		if err := emitEvalReport(report, opts); err != nil {
			return err
		}

		if !report.Passed {
			return errors.New("eval output failed")
		}

		return nil
	}

	return evalOutput(opts.evalOutputPath, opts.evalExpected, opts.evalExpectedPath, atteval.MatchMode(opts.evalMode))
}

func structuredEvalRequested(opts cliOptions) bool {
	return opts.evalAssertionsPath != "" ||
		opts.evalFixtureDir != "" ||
		opts.evalUpdateGolden ||
		opts.evalApproveGoldenUpdate
}

func evalReportForCommand(opts cliOptions) (atteval.Report, error) {
	if opts.evalAssertionsPath != "" && opts.evalFixtureDir != "" {
		return atteval.Report{}, errors.New("eval output: pass either --eval-assertions or --eval-fixture-dir, not both")
	}

	if opts.evalOutputPath == "" && opts.evalAssertionsPath == "" && opts.evalFixtureDir == "" {
		return atteval.Report{}, errors.New("eval output: pass --eval-output, --eval-assertions, or --eval-fixture-dir")
	}

	runOptions := evalRunOptions(opts)
	switch {
	case opts.evalFixtureDir != "":
		report, err := atteval.RunFixtureDir(opts.evalFixtureDir, runOptions)
		if err != nil {
			return atteval.Report{}, fmt.Errorf("run eval fixture dir: %w", err)
		}

		return report, nil
	case opts.evalAssertionsPath != "":
		report, err := atteval.RunSuiteFile(opts.evalAssertionsPath, runOptions)
		if err != nil {
			return atteval.Report{}, fmt.Errorf("run eval assertions: %w", err)
		}

		return report, nil
	case opts.evalUpdateGolden || opts.evalApproveGoldenUpdate:
		return atteval.Report{}, errors.New("eval output: golden update flags require --eval-assertions or --eval-fixture-dir")
	default:
		return simpleEvalReport(opts)
	}
}

func evalRunOptions(opts cliOptions) atteval.RunOptions {
	runOptions := atteval.RunOptions{
		ActualPath:          opts.evalOutputPath,
		UpdateGolden:        opts.evalUpdateGolden,
		ApproveGoldenUpdate: opts.evalApproveGoldenUpdate,
	}
	if opts.evalExitCode.set {
		runOptions.ExitCode = &opts.evalExitCode.value
	}

	return runOptions
}

func simpleEvalReport(opts cliOptions) (atteval.Report, error) {
	actual, err := os.ReadFile(opts.evalOutputPath)
	if err != nil {
		return atteval.Report{}, fmt.Errorf("eval output: read actual %s: %w", opts.evalOutputPath, err)
	}

	expected, err := expectedEvalText(opts.evalExpected, opts.evalExpectedPath)
	if err != nil {
		return atteval.Report{}, err
	}

	result := atteval.Check(string(actual), expected, atteval.MatchMode(opts.evalMode))
	status := "pass"
	evidence := "output matched"

	if !result.Passed {
		status = "fail"
		evidence = result.Summary
	}

	assertion := atteval.AssertionResult{
		ID:              "output",
		Type:            atteval.AssertionType(result.Mode),
		Severity:        atteval.SeverityError,
		Status:          status,
		Passed:          result.Passed,
		Evidence:        atteval.Redact(evidence),
		ExpectedSnippet: evalReportSnippet(expected),
		ActualSnippet:   evalReportSnippet(string(actual)),
		Error:           atteval.Redact(result.Diff),
	}
	if result.Passed {
		assertion.ExpectedSnippet = ""
		assertion.ActualSnippet = ""
	}

	summary := atteval.ReportSummary{Total: 1}
	if result.Passed {
		summary.Passed = 1
		summary.PassRate = 1
	} else {
		summary.Failed = 1
	}

	return atteval.Report{
		Version:   1,
		Passed:    result.Passed,
		RunAt:     time.Now().UTC().Format(time.RFC3339),
		Summary:   summary,
		Results:   []atteval.AssertionResult{assertion},
		ActualRef: opts.evalOutputPath,
	}, nil
}

func emitEvalReport(report atteval.Report, opts cliOptions) error {
	data, err := report.JSON()
	if err != nil {
		return fmt.Errorf("eval output: encode report: %w", err)
	}

	if opts.evalReportPath != "" {
		if err := writeEvalReport(opts.evalReportPath, data); err != nil {
			return err
		}
	}

	if opts.evalJSON {
		fmt.Println(string(data))
		return nil
	}

	printEvalReportText(report, opts.evalReportPath)

	return nil
}

func writeEvalReport(path string, data []byte) error {
	if dir := filepath.Dir(path); dir != "." && dir != "" {
		if err := os.MkdirAll(dir, 0o750); err != nil {
			return fmt.Errorf("eval output: create report directory: %w", err)
		}
	}

	if err := os.WriteFile(path, data, 0o600); err != nil {
		return fmt.Errorf("eval output: write report %s: %w", path, err)
	}

	return nil
}

//nolint:gocognit,wsl_v5 // Text output keeps assertion details grouped for readable CLI failures.
func printEvalReportText(report atteval.Report, reportPath string) {
	status := "PASS"
	if !report.Passed {
		status = "FAIL"
	}

	fmt.Printf("%s\ttotal=%d\tpassed=%d\tfailed=%d\tpass_rate=%.2f", status, report.Summary.Total, report.Summary.Passed, report.Summary.Failed, report.Summary.PassRate)
	if report.Summary.FlakeCount != 0 {
		fmt.Printf("\tflake_count=%d", report.Summary.FlakeCount)
	}
	fmt.Println()
	printEvalReportMetadata(report)
	printEvalReportMetrics(report.Metrics)
	for i := range report.Results {
		result := &report.Results[i]
		fmt.Printf("%s\tid=%s\ttype=%s\tseverity=%s", strings.ToUpper(result.Status), result.ID, result.Type, result.Severity)
		if result.Suite != "" {
			fmt.Printf("\tsuite=%s", result.Suite)
		}
		if result.Evidence != "" {
			fmt.Printf("\tevidence=%s", result.Evidence)
		}
		fmt.Println()

		if !result.Passed {
			if result.Error != "" {
				fmt.Printf("  error: %s\n", result.Error)
			}
			if result.ExpectedSnippet != "" {
				fmt.Printf("  expected: %q\n", result.ExpectedSnippet)
			}
			if result.ActualSnippet != "" {
				fmt.Printf("  actual: %q\n", result.ActualSnippet)
			}
			if result.Remediation != "" {
				fmt.Printf("  remediation: %s\n", result.Remediation)
			}
		}
	}
	if reportPath != "" {
		fmt.Printf("report=%s\n", reportPath)
	}
}

func printEvalReportMetadata(report atteval.Report) {
	var parts []string
	if report.Metadata.Provider != "" {
		parts = append(parts, "provider="+report.Metadata.Provider)
	}

	if report.Metadata.Model != "" {
		parts = append(parts, "model="+report.Metadata.Model)
	}

	if report.Metadata.FixtureVersion != "" {
		parts = append(parts, "fixture_version="+report.Metadata.FixtureVersion)
	}

	if len(parts) > 0 {
		fmt.Println("metadata\t" + strings.Join(parts, "\t"))
	}
}

func printEvalReportMetrics(metrics atteval.ReportMetrics) {
	var parts []string
	if metrics.LatencyMillis != 0 {
		parts = append(parts, "latency_millis="+strconv.FormatInt(metrics.LatencyMillis, 10))
	}

	if metrics.InputTokens != 0 {
		parts = append(parts, "input_tokens="+strconv.Itoa(metrics.InputTokens))
	}

	if metrics.OutputTokens != 0 {
		parts = append(parts, "output_tokens="+strconv.Itoa(metrics.OutputTokens))
	}

	if metrics.TotalTokens != 0 {
		parts = append(parts, "total_tokens="+strconv.Itoa(metrics.TotalTokens))
	}

	if metrics.Cost != 0 {
		parts = append(parts, fmt.Sprintf("cost=%.6f", metrics.Cost))
	}

	if len(parts) > 0 {
		fmt.Println("metrics\t" + strings.Join(parts, "\t"))
	}
}

func evalReportSnippet(value string) string {
	const limit = 160

	value = strings.Join(strings.Fields(atteval.Redact(value)), " ")
	runes := []rune(value)

	if len(runes) <= limit {
		return value
	}

	return string(runes[:limit-1]) + "…"
}

func expectedEvalText(expectedText, expectedPath string) (string, error) {
	switch {
	case expectedText != "" && expectedPath != "":
		return "", errors.New("eval output: pass either --eval-expected or --eval-expected-file, not both")
	case expectedText != "":
		return expectedText, nil
	case expectedPath != "":
		data, err := os.ReadFile(expectedPath)
		if err != nil {
			return "", fmt.Errorf("eval output: read expected %s: %w", expectedPath, err)
		}

		return string(data), nil
	default:
		return "", errors.New("eval output: expected text is required")
	}
}

func suggestSkill(steps []string, maxSteps, minOccurrences int, saveDir string, reviewOnly bool) error {
	suggestion, ok := attskill.SuggestWithOptions(steps, attskill.Options{
		MaxSteps:       maxSteps,
		MinOccurrences: minOccurrences,
	})
	if !ok {
		fmt.Println("No repeated multi-step skill candidate found.")
		return nil
	}

	fmt.Print(formatSkillSuggestion(suggestion))

	if strings.TrimSpace(saveDir) == "" {
		if reviewOnly {
			return errors.New("skill review: --skill-review-only requires --skill-save-dir")
		}

		return nil
	}

	review, err := attskill.BuildReview(saveDir, suggestion)
	if err != nil {
		return fmt.Errorf("review skill suggestion: %w", err)
	}

	fmt.Print(review.Diff)
	fmt.Print(formatSkillTriggerResults(review.TriggerResults))

	if reviewOnly {
		fmt.Println("review-only: no files written")
		return nil
	}

	path, err := attskill.PersistReview(review)
	if err != nil {
		return fmt.Errorf("save skill suggestion: %w", err)
	}

	fmt.Println("saved: " + path)

	return nil
}

func formatSkillSuggestion(suggestion attskill.Suggestion) string {
	var b strings.Builder
	fmt.Fprintf(&b, "name: %s\n", suggestion.Name)
	fmt.Fprintf(&b, "slug: %s\n", suggestion.Slug)
	fmt.Fprintf(&b, "occurrences: %d\n", suggestion.Occurrences)
	b.WriteString("steps:\n")

	for _, step := range suggestion.Steps {
		fmt.Fprintf(&b, "  - %s\n", step)
	}

	if len(suggestion.Parameters) > 0 {
		b.WriteString("parameters:\n")

		for _, parameter := range suggestion.Parameters {
			fmt.Fprintf(&b, "  - %s=%s", parameter.Name, parameter.Placeholder)

			if len(parameter.Examples) > 0 {
				fmt.Fprintf(&b, " examples=%s", strings.Join(parameter.Examples, ","))
			}

			b.WriteByte('\n')
		}
	}

	if suggestion.Rationale != "" {
		fmt.Fprintf(&b, "rationale: %s\n", suggestion.Rationale)
	}

	return b.String()
}

func formatSkillTriggerResults(results []attskill.TriggerEvalResult) string {
	if len(results) == 0 {
		return ""
	}

	var b strings.Builder
	fmt.Fprintf(&b, "trigger-evals: pass %d cases\n", len(results))

	for _, result := range results {
		label := "reject"
		if result.Expected {
			label = "trigger"
		}

		fmt.Fprintf(&b, "  - %s: %s\n", label, result.Prompt)
	}

	return b.String()
}

func promptComplete(ctx context.Context, state appState, input string, limit int) {
	contextResult := promptCompletionContextWithFreshness(ctx, state, input, true)
	fmt.Print(formatPromptContextSources(contextResult.Sources))

	suggestions := promptcomplete.SuggestAll(contextResult.Context, promptcomplete.Options{Limit: limit})
	if len(suggestions) == 0 {
		fmt.Println("No prompt completion found.")
		return
	}

	fmt.Print(formatPromptSuggestions(suggestions))
}

func promptAgentCandidates(registry *agent.Registry) []promptcomplete.Candidate {
	if registry == nil {
		return nil
	}

	names := registry.List()

	out := make([]promptcomplete.Candidate, 0, len(names))
	for _, name := range names {
		configuredAgent, _ := registry.Get(name)
		out = append(out, promptcomplete.Candidate{
			Text:        name,
			Kind:        "agent",
			Description: configuredAgent.Description,
			Tokens:      append([]string(nil), configuredAgent.Capabilities...),
		})
	}

	return out
}

func promptToolCandidates() []promptcomplete.Candidate {
	return []promptcomplete.Candidate{
		{Text: "memory-search", Kind: "tool", Description: "search privacy-scoped local memory with corpus reporting"},
		{Text: "plan-agents", Kind: "tool", Description: "preview agent orchestration"},
		{Text: "review", Kind: "tool", Description: "run a structured code review"},
		{Text: "test", Kind: "tool", Description: "run verification tests"},
	}
}

func promptTemplateCandidates() []promptcomplete.Candidate {
	return []promptcomplete.Candidate{
		{
			Text:        "review this change for correctness, tests, and regressions",
			Kind:        "template",
			Description: "code review prompt",
		},
		{
			Text:        "summarize this session with changed files and verification evidence",
			Kind:        "template",
			Description: "session summary prompt",
		},
		{
			Text:        "plan agents for this task and list the verification gates",
			Kind:        "template",
			Description: "agent orchestration prompt",
		},
	}
}

func formatPromptSuggestions(suggestions []promptcomplete.Suggestion) string {
	var b strings.Builder

	for i := range suggestions {
		suggestion := &suggestions[i]
		fmt.Fprintf(&b, "text: %s\n", suggestion.Text)
		fmt.Fprintf(&b, "suffix: %s\n", suggestion.Suffix)
		fmt.Fprintf(&b, "kind: %s\n", suggestion.Candidate.Kind)

		if suggestion.Source != "" {
			fmt.Fprintf(&b, "source: %s\n", suggestion.Source)
		}

		fmt.Fprintf(&b, "score: %d\n", suggestion.Score)
		fmt.Fprintf(&b, "replace: %d:%d\n", suggestion.ReplacementStart, suggestion.ReplacementEnd)

		if suggestion.Explanation != "" {
			fmt.Fprintf(&b, "explanation: %s\n", suggestion.Explanation)
		}

		if len(suggestion.RankSignals) > 0 {
			b.WriteString("rank:\n")

			for _, signal := range suggestion.RankSignals {
				fmt.Fprintf(&b, "  - %s %+d", signal.Name, signal.Score)

				if signal.Detail != "" {
					fmt.Fprintf(&b, ": %s", signal.Detail)
				}

				b.WriteByte('\n')
			}
		}

		b.WriteByte('\n')
	}

	return b.String()
}

func printFeedbackProposals(saved session.Session) {
	proposals := feedback.FromSession(saved)
	if len(proposals) == 0 {
		fmt.Println("No feedback proposals found.")
		return
	}

	for i := range proposals {
		fmt.Print(formatFeedbackProposal(proposals[i]))
	}
}

func formatFeedbackProposal(proposal feedback.Proposal) string {
	var b strings.Builder
	fmt.Fprintf(&b, "id: %s\n", feedback.ProposalID(proposal))
	writeFeedbackProposalHeader(&b, proposal)

	if sourceRun := strings.TrimSpace(proposal.SourceRun); sourceRun != "" {
		fmt.Fprintf(&b, "source_run: %s\n", sourceRun)
	}

	if reviewer := strings.TrimSpace(proposal.Reviewer); reviewer != "" {
		fmt.Fprintf(&b, "reviewer: %s\n", reviewer)
	}

	if proposal.ExpiresAt != nil {
		fmt.Fprintf(&b, "expires_at: %s\n", proposal.ExpiresAt.UTC().Format(time.RFC3339))
	}

	writeFeedbackRejectedAlternatives(&b, proposal.RejectedAlternatives)
	writeFeedbackEvidence(&b, proposal.Evidence)
	writeFeedbackLinkedEvidence(&b, proposal.LinkedEvidence)
	writeFeedbackVerification(&b, proposal.Verification)

	b.WriteByte('\n')

	return b.String()
}

func writeFeedbackProposalHeader(b *strings.Builder, proposal feedback.Proposal) {
	fmt.Fprintf(b, "agent: %s\n", proposal.Agent)
	fmt.Fprintf(b, "confidence: %.2f\n", proposal.Confidence)

	if rootCause := formatFeedbackRootCause(proposal.RootCause); rootCause != "" {
		fmt.Fprintf(b, "root_cause: %s\n", rootCause)
	}

	if target := strings.TrimSpace(proposal.TargetBehavior); target != "" {
		fmt.Fprintf(b, "target_behavior: %s\n", target)
	}

	fmt.Fprintf(b, "action: %s\n", proposal.Action)
	fmt.Fprintf(b, "reason: %s\n", proposal.Reason)
}

func writeFeedbackRejectedAlternatives(b *strings.Builder, rejected []feedback.RejectedAlternative) {
	if len(rejected) == 0 {
		return
	}

	b.WriteString("rejected_alternatives:\n")

	for _, item := range rejected {
		if alternative := strings.TrimSpace(item.Alternative); alternative != "" {
			fmt.Fprintf(b, "  - alternative: %s\n", alternative)
		}

		if reason := strings.TrimSpace(item.Reason); reason != "" {
			fmt.Fprintf(b, "    reason: %s\n", reason)
		}
	}
}

func writeFeedbackEvidence(b *strings.Builder, evidence []string) {
	if len(evidence) == 0 {
		return
	}

	b.WriteString("evidence:\n")

	for _, item := range evidence {
		fmt.Fprintf(b, "  - %s\n", item)
	}
}

func writeFeedbackLinkedEvidence(b *strings.Builder, links []feedback.EvidenceLink) {
	if len(links) == 0 {
		return
	}

	b.WriteString("linked_evidence:\n")

	for _, link := range links {
		fmt.Fprintf(b, "  - kind=%s\tref=%s", link.Kind, link.Reference)

		if description := strings.TrimSpace(link.Description); description != "" {
			fmt.Fprintf(b, "\tdescription=%s", description)
		}

		b.WriteByte('\n')
	}
}

func writeFeedbackVerification(b *strings.Builder, records []feedback.VerificationRecord) {
	if len(records) == 0 {
		return
	}

	b.WriteString("verification:\n")

	for _, record := range records {
		fmt.Fprintf(b, "  - phase=%s\tkind=%s\tpassed=%t", record.Phase, record.Kind, record.Passed)

		if record.Outcome != "" {
			fmt.Fprintf(b, "\toutcome=%s", record.Outcome)
		}

		if record.Score != 0 {
			fmt.Fprintf(b, "\tscore=%d", record.Score)
		}

		if record.Reference != "" {
			fmt.Fprintf(b, "\tref=%s", record.Reference)
		}

		b.WriteByte('\n')
	}
}

func formatFeedbackRootCause(rootCause feedback.RootCauseClassification) string {
	category := strings.TrimSpace(rootCause.Category)
	summary := strings.TrimSpace(rootCause.Summary)

	signals := cleanFeedbackStrings(rootCause.Signals)
	switch {
	case category == "" && summary == "":
		return ""
	case category == "":
		return summary
	case summary == "":
		return category
	case len(signals) == 0:
		return category + " — " + summary
	default:
		return fmt.Sprintf("%s — %s (signals: %s)", category, summary, strings.Join(signals, ", "))
	}
}

func cleanFeedbackStrings(values []string) []string {
	cleaned := make([]string, 0, len(values))
	for _, value := range values {
		if trimmed := strings.TrimSpace(value); trimmed != "" {
			cleaned = append(cleaned, trimmed)
		}
	}

	return cleaned
}

func applyFeedbackProposals(saved session.Session, configPath, historyPath string) error {
	configPath = strings.TrimSpace(configPath)
	if configPath == "" {
		return errors.New("feedback apply: config path is required")
	}

	cfg, loaded, err := appconfig.LoadFiles([]string{configPath})
	if err != nil {
		return fmt.Errorf("feedback apply: load config: %w", err)
	}

	if len(loaded) == 0 {
		return fmt.Errorf("feedback apply: config %s not found", configPath)
	}

	originalCfg := cfg
	appliedAt := time.Now().UTC()

	updatedAgents, history := feedback.ApplyProposalsWithOptions(cfg.Agents, feedback.FromSession(saved), feedback.ApplyOptions{
		SourceRun: saved.ID,
		Reviewer:  "feedback-apply",
		Status:    feedback.GuidanceStatusPending,
		Now:       appliedAt,
	})
	if len(history) == 0 {
		fmt.Println("No feedback proposals recorded.")
		return nil
	}

	cfg.Agents = updatedAgents
	if err := writeConfigFile(configPath, cfg); err != nil {
		return fmt.Errorf("feedback apply: %w", err)
	}

	historyPath = feedbackHistoryDefault(configPath, historyPath)
	if err := appendFeedbackHistory(historyPath, history, appliedAt); err != nil {
		if restoreErr := writeConfigFile(configPath, originalCfg); restoreErr != nil {
			return fmt.Errorf("feedback apply: append history failed and restore config failed: %w", errors.Join(err, restoreErr))
		}

		return fmt.Errorf("feedback apply: %w", err)
	}

	fmt.Printf("Recorded %d pending feedback guidance decision(s).\n", len(history))
	fmt.Println("config: " + configPath)
	fmt.Println("history: " + historyPath)

	return nil
}

func approveFeedbackGuidance(configPath, historyPath, agentName, guidanceID string) error {
	configPath = strings.TrimSpace(configPath)
	if configPath == "" {
		return errors.New("feedback approve: config path is required")
	}

	agentName = strings.TrimSpace(agentName)
	if agentName == "" {
		return errors.New("feedback approve: agent is required")
	}

	guidanceID = strings.TrimSpace(guidanceID)
	if guidanceID == "" {
		return errors.New("feedback approve: guidance id is required")
	}

	cfg, loaded, err := appconfig.LoadFiles([]string{configPath})
	if err != nil {
		return fmt.Errorf("feedback approve: load config: %w", err)
	}

	if len(loaded) == 0 {
		return fmt.Errorf("feedback approve: config %s not found", configPath)
	}

	approvedAt := time.Now().UTC()

	updatedAgents, history, ok := feedback.ApproveGuidance(
		cfg.Agents,
		agentName,
		guidanceID,
		"feedback-approve",
		approvedAt,
	)
	if !ok {
		return fmt.Errorf("feedback approve: guidance %s for agent %s was not found, pending, or approvable", guidanceID, agentName)
	}

	cfg.Agents = updatedAgents
	if err := writeConfigFile(configPath, cfg); err != nil {
		return fmt.Errorf("feedback approve: %w", err)
	}

	historyPath = feedbackHistoryDefault(configPath, historyPath)
	if err := appendFeedbackHistory(historyPath, []feedback.HistoryEntry{history}, approvedAt); err != nil {
		return fmt.Errorf("feedback approve: %w", err)
	}

	switch history.Status {
	case feedback.GuidanceStatusApproved:
		fmt.Printf("Approved feedback guidance %s for agent %s.\n", guidanceID, agentName)
	case feedback.GuidanceStatusQuarantined:
		fmt.Printf("Quarantined feedback guidance %s for agent %s.\n", guidanceID, agentName)
	default:
		fmt.Printf("Recorded feedback guidance %s for agent %s with status %s.\n", guidanceID, agentName, history.Status)
	}

	fmt.Println("config: " + configPath)
	fmt.Println("history: " + historyPath)

	return nil
}

func rollbackFeedbackGuidance(configPath, historyPath, agentName, guidanceID, reason string) error {
	configPath = strings.TrimSpace(configPath)
	if configPath == "" {
		return errors.New("feedback rollback: config path is required")
	}

	agentName = strings.TrimSpace(agentName)
	if agentName == "" {
		return errors.New("feedback rollback: agent is required")
	}

	guidanceID = strings.TrimSpace(guidanceID)
	if guidanceID == "" {
		return errors.New("feedback rollback: guidance id is required")
	}

	cfg, loaded, err := appconfig.LoadFiles([]string{configPath})
	if err != nil {
		return fmt.Errorf("feedback rollback: load config: %w", err)
	}

	if len(loaded) == 0 {
		return fmt.Errorf("feedback rollback: config %s not found", configPath)
	}

	rolledBackAt := time.Now().UTC()

	updatedAgents, history, ok := feedback.RollbackGuidance(
		cfg.Agents,
		agentName,
		guidanceID,
		"feedback-rollback",
		reason,
		rolledBackAt,
	)
	if !ok {
		return fmt.Errorf("feedback rollback: guidance %s for agent %s was not found or is already rolled back", guidanceID, agentName)
	}

	cfg.Agents = updatedAgents
	if err := writeConfigFile(configPath, cfg); err != nil {
		return fmt.Errorf("feedback rollback: %w", err)
	}

	historyPath = feedbackHistoryDefault(configPath, historyPath)
	if err := appendFeedbackHistory(historyPath, []feedback.HistoryEntry{history}, rolledBackAt); err != nil {
		return fmt.Errorf("feedback rollback: %w", err)
	}

	fmt.Printf("Rolled back feedback guidance %s for agent %s.\n", guidanceID, agentName)
	fmt.Println("config: " + configPath)
	fmt.Println("history: " + historyPath)

	return nil
}

func feedbackHistoryDefault(configPath, historyPath string) string {
	historyPath = strings.TrimSpace(historyPath)
	if historyPath != "" {
		return historyPath
	}

	return configPath + ".feedback.md"
}

func writeConfigFile(path string, cfg appconfig.Config) error {
	data, err := yaml.Marshal(cfg)
	if err != nil {
		return fmt.Errorf("marshal config: %w", err)
	}

	if err := os.WriteFile(path, data, 0o600); err != nil {
		return fmt.Errorf("write config %s: %w", path, err)
	}

	return nil
}

func appendFeedbackHistory(path string, entries []feedback.HistoryEntry, appliedAt time.Time) error {
	dir := filepath.Dir(path)
	if dir != "." && dir != "" {
		if err := os.MkdirAll(dir, 0o750); err != nil {
			return fmt.Errorf("create feedback history dir %s: %w", dir, err)
		}
	}

	file, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return fmt.Errorf("open feedback history %s: %w", path, err)
	}

	if _, err := file.WriteString(formatFeedbackHistory(entries, appliedAt)); err != nil {
		_ = file.Close()
		return fmt.Errorf("write feedback history %s: %w", path, err)
	}

	if err := file.Close(); err != nil {
		return fmt.Errorf("close feedback history %s: %w", path, err)
	}

	return nil
}

func formatFeedbackHistory(entries []feedback.HistoryEntry, appliedAt time.Time) string {
	var b strings.Builder
	fmt.Fprintf(&b, "## Feedback guidance decisions %s\n\n", appliedAt.Format(time.RFC3339))

	for i := range entries {
		b.WriteString(feedback.FormatHistoryEntry(entries[i]))
		b.WriteByte('\n')
	}

	return b.String()
}

type routeModelsCommandInput struct {
	Candidates []string
	Policy     modelroute.Policy
	Profile    modelroute.RequestProfile
}

func routeModelsCommandInputFromOptions(opts cliOptions) routeModelsCommandInput {
	input := routeModelsCommandInput{
		Candidates: append([]string(nil), opts.routeCandidates...),
		Profile: modelroute.RequestProfile{
			EstimatedInputTokens:      opts.routeInputTokens.value,
			EstimatedOutputTokens:     opts.routeOutputTokens.value,
			EstimatedCacheWriteTokens: opts.routeCacheWriteTokens.value,
			Interactive:               opts.routeInteractive,
			Batch:                     opts.routeBatch,
		},
	}
	if opts.routeBudget.set {
		input.Profile.Budget = opts.routeBudget.value
	}

	if opts.routeCacheReuse.set {
		input.Profile.PromptCacheReuseEstimate = opts.routeCacheReuse.value
	}

	input.Policy.RequiredCapabilities = append([]string(nil), opts.routeRequiredCapabilities...)

	return input
}

func runRouteModels(input routeModelsCommandInput) error {
	if len(input.Candidates) == 0 {
		return errors.New("model route: at least one --route-candidate is required; run `atteler help providers`")
	}

	candidates, profile, err := routeCandidatesAndProfile(input)
	if err != nil {
		return err
	}

	if err := validateRouteModelPolicy(input.Policy); err != nil {
		return err
	}

	decision := decideRouteCandidatesWithPolicy(candidates, profile, input.Policy)
	if decision.Selected == "" {
		fmt.Println("No model route candidates fit.")

		if rendered := formatRouteDecision(decision); rendered != "" {
			fmt.Print(rendered)
		}

		return nil
	}

	fmt.Print(formatRouteDecision(decision))

	return nil
}

func routeCandidatesAndProfile(input routeModelsCommandInput) ([]modelroute.Candidate, modelroute.RequestProfile, error) {
	candidates := make([]modelroute.Candidate, 0, len(input.Candidates))
	for _, raw := range input.Candidates {
		candidate, err := parseRouteCandidate(raw)
		if err != nil {
			return nil, modelroute.RequestProfile{}, err
		}

		candidates = append(candidates, candidate)
	}

	return candidates, input.Profile, nil
}

func applyRouteSelection(input routeModelsCommandInput, state *selectionState) error {
	if len(input.Candidates) == 0 {
		return nil
	}

	candidates, profile, err := routeCandidatesAndProfile(input)
	if err != nil {
		return err
	}

	if err := validateRouteModelPolicy(input.Policy); err != nil {
		return err
	}

	decision := decideRouteCandidatesWithPolicy(candidates, profile, input.Policy)
	if len(decision.FallbackOrder) == 0 {
		return errors.New("model route: no candidates fit request budget/context")
	}

	state.selectedModel = decision.FallbackOrder[0]
	state.fallbackModels = append([]string(nil), decision.FallbackOrder[1:]...)
	state.modelLocked = true
	state.sessionState.DefaultModel = state.selectedModel

	return nil
}

func decideRouteCandidates(candidates []modelroute.Candidate, profile modelroute.RequestProfile) modelroute.Decision {
	return decideRouteCandidatesWithPolicy(candidates, profile, modelroute.Policy{})
}

func decideRouteCandidatesWithPolicy(
	candidates []modelroute.Candidate,
	profile modelroute.RequestProfile,
	policy modelroute.Policy,
) modelroute.Decision {
	return decideRouteCandidatesWithPolicyAt(candidates, profile, policy, time.Now().UTC())
}

func decideRouteCandidatesAt(candidates []modelroute.Candidate, profile modelroute.RequestProfile, now time.Time) modelroute.Decision {
	return decideRouteCandidatesWithPolicyAt(candidates, profile, modelroute.Policy{}, now)
}

func decideRouteCandidatesWithPolicyAt(
	candidates []modelroute.Candidate,
	profile modelroute.RequestProfile,
	policy modelroute.Policy,
	now time.Time,
) modelroute.Decision {
	decision := modelroute.Decide(candidates, profile, policy, nil)

	version, ok := routeCandidateCatalogVersion(candidates)
	if !ok {
		return decision
	}

	decision.CatalogVersion = version
	decision.Constraints = appendRouteConstraint(decision.Constraints, modelroute.ConstraintCatalogMetadata)
	decision.Constraints = appendRouteConstraint(decision.Constraints, modelroute.ConstraintMetadataFreshness)

	catalog := modelroute.BuiltinCatalog()
	if catalog.IsStale(now) {
		decision.CatalogStale = true
		decision.Warnings = append(decision.Warnings, fmt.Sprintf("%s: catalog %s stale after %s", modelroute.ReasonMetadataStale, catalog.Version, catalog.StaleAfter.Format(time.RFC3339)))
	}

	return decision
}

func validateRouteModelPolicy(policy modelroute.Policy) error {
	for _, capability := range policy.RequiredCapabilities {
		capability = strings.ToLower(strings.TrimSpace(capability))
		if capability == "" || modelroute.IsKnownCapability(capability) {
			continue
		}

		return fmt.Errorf(
			"model route required capabilities contains unknown capability %q (valid: %s)",
			capability,
			strings.Join(modelroute.KnownCapabilities(), ","),
		)
	}

	return nil
}

func routeCandidateCatalogVersion(candidates []modelroute.Candidate) (string, bool) {
	version := ""

	for i := range candidates {
		candidateVersion := strings.TrimSpace(candidates[i].MetadataVersion)
		if candidateVersion == "" {
			continue
		}

		if version == "" {
			version = candidateVersion
			continue
		}

		if version != candidateVersion {
			return "mixed", true
		}
	}

	return version, version != ""
}

func appendRouteConstraint(constraints []string, constraint string) []string {
	if slices.Contains(constraints, constraint) {
		return constraints
	}

	return append(constraints, constraint)
}

func parseRouteCandidate(raw string) (modelroute.Candidate, error) {
	parts := strings.Split(raw, ",")
	if len(parts) == 0 || strings.TrimSpace(parts[0]) == "" {
		return modelroute.Candidate{}, errors.New("route candidate: model id is required")
	}

	candidate := modelroute.Candidate{}

	id := strings.TrimSpace(parts[0])
	if provider, name, ok := strings.Cut(id, "/"); ok {
		candidate.Provider = strings.TrimSpace(provider)
		candidate.Name = strings.TrimSpace(name)
	} else {
		candidate.Name = id
	}

	if candidate.Name == "" && candidate.Provider == "" {
		return modelroute.Candidate{}, errors.New("route candidate: model id is required")
	}

	if catalogCandidate, ok := modelroute.BuiltinCatalog().Candidate(candidate.ID()); ok {
		candidate = catalogCandidate
	} else if len(parts) == 1 {
		return modelroute.Candidate{}, fmt.Errorf("route candidate %q: unknown or ambiguous model metadata; use provider/model or supply explicit key=value metadata", id)
	}

	for _, part := range parts[1:] {
		field, value, ok := strings.Cut(strings.TrimSpace(part), "=")
		if !ok {
			return modelroute.Candidate{}, fmt.Errorf("route candidate %q: expected key=value", raw)
		}

		if err := applyRouteCandidateField(&candidate, strings.TrimSpace(field), strings.TrimSpace(value)); err != nil {
			return modelroute.Candidate{}, err
		}
	}

	return candidate, nil
}

//nolint:cyclop,gocognit // Flat CLI field parsing keeps each accepted spelling close to its validation.
func applyRouteCandidateField(candidate *modelroute.Candidate, field, value string) error {
	switch field {
	case "input", "input_cost":
		parsed, err := parseRouteCandidateNonNegativeFloat("input cost", value)
		if err != nil {
			return err
		}

		candidate.InputTokenCost = parsed
	case "output", "output_cost":
		parsed, err := parseRouteCandidateNonNegativeFloat("output cost", value)
		if err != nil {
			return err
		}

		candidate.OutputTokenCost = parsed
	case "cached", "cached_input", "cached_input_cost":
		parsed, err := parseRouteCandidateNonNegativeFloat("cached input cost", value)
		if err != nil {
			return err
		}

		candidate.CachedInputTokenCost = parsed
	case "cache_write", "cache_write_cost":
		parsed, err := parseRouteCandidateNonNegativeFloat("cache write cost", value)
		if err != nil {
			return err
		}

		candidate.CacheWriteTokenCost = parsed
	case "priority":
		parsed, err := strconv.Atoi(value)
		if err != nil {
			return fmt.Errorf("route candidate priority: %w", err)
		}

		candidate.Priority = parsed
	case "max", "max_input":
		parsed, err := parseRouteCandidateNonNegativeInt("max input", value)
		if err != nil {
			return err
		}

		candidate.MaxInputTokens = parsed
	case "max_output", "output_max":
		parsed, err := parseRouteCandidateNonNegativeInt("max output", value)
		if err != nil {
			return err
		}

		candidate.MaxOutputTokens = parsed
	case "latency":
		parsed, err := parseRouteCandidateNonNegativeInt("latency", value)
		if err != nil {
			return err
		}

		candidate.ExpectedLatencyMS = parsed
	case "ttft":
		parsed, err := parseRouteCandidateNonNegativeInt("ttft", value)
		if err != nil {
			return err
		}

		candidate.ExpectedTTFTMS = parsed
	case "capability", "capabilities", "caps":
		capabilities, err := parseRouteCandidateCapabilities(value)
		if err != nil {
			return err
		}

		candidate.Capabilities = capabilities
	default:
		return fmt.Errorf("route candidate: unknown field %q", field)
	}

	return nil
}

func parseRouteCandidateNonNegativeFloat(name, value string) (float64, error) {
	parsed, err := strconv.ParseFloat(value, 64)
	if err != nil {
		return 0, fmt.Errorf("route candidate %s: %w", name, err)
	}

	if math.IsNaN(parsed) || math.IsInf(parsed, 0) {
		return 0, fmt.Errorf("route candidate %s must be finite", name)
	}

	if parsed < 0 {
		return 0, fmt.Errorf("route candidate %s must be >= 0", name)
	}

	return parsed, nil
}

func parseRouteCandidateCapabilities(value string) ([]string, error) {
	values := strings.FieldsFunc(value, func(r rune) bool {
		return r == '|' || r == '+' || r == ';'
	})
	out := make([]string, 0, len(values))
	seen := make(map[string]bool, len(values))

	for _, capability := range values {
		capability = strings.ToLower(strings.TrimSpace(capability))
		if capability == "" || seen[capability] {
			continue
		}

		if !modelroute.IsKnownCapability(capability) {
			return nil, fmt.Errorf(
				"route candidate capabilities contains unknown capability %q (valid: %s)",
				capability,
				strings.Join(modelroute.KnownCapabilities(), ","),
			)
		}

		seen[capability] = true
		out = append(out, capability)
	}

	if len(out) == 0 {
		return nil, errors.New("route candidate capabilities must include at least one capability")
	}

	return out, nil
}

func parseRouteCandidateNonNegativeInt(name, value string) (int, error) {
	parsed, err := strconv.Atoi(value)
	if err != nil {
		return 0, fmt.Errorf("route candidate %s: %w", name, err)
	}

	if parsed < 0 {
		return 0, fmt.Errorf("route candidate %s must be >= 0", name)
	}

	return parsed, nil
}

func formatRouteCandidate(candidate modelroute.Candidate, profile modelroute.RequestProfile) string {
	parts := []string{
		candidate.ID(),
		fmt.Sprintf("cost=%.6f", modelroute.EstimateCost(candidate, profile)),
	}
	if candidate.MetadataVersion != "" {
		parts = append(parts, "metadata_version="+candidate.MetadataVersion)
	}

	if candidate.MetadataSourceURL != "" {
		parts = append(parts, "metadata_source="+candidate.MetadataSourceURL)
	}

	if candidate.Deprecated {
		parts = append(parts, "deprecated=true")
	}

	if len(candidate.Capabilities) > 0 {
		parts = append(parts, "capabilities="+strings.Join(candidate.Capabilities, ","))
	}

	if candidate.Priority != 0 {
		parts = append(parts, "priority="+strconv.Itoa(candidate.Priority))
	}

	if candidate.MaxInputTokens > 0 {
		parts = append(parts, "max_input="+strconv.Itoa(candidate.MaxInputTokens))
	}

	if candidate.MaxOutputTokens > 0 {
		parts = append(parts, "max_output="+strconv.Itoa(candidate.MaxOutputTokens))
	}

	if candidate.ExpectedLatencyMS > 0 {
		parts = append(parts, "latency_ms="+strconv.Itoa(candidate.ExpectedLatencyMS))
	}

	if candidate.ExpectedTTFTMS > 0 {
		parts = append(parts, "ttft_ms="+strconv.Itoa(candidate.ExpectedTTFTMS))
	}

	return strings.Join(parts, "\t")
}

func formatRouteDecision(decision modelroute.Decision) string {
	if len(decision.Candidates) == 0 {
		return ""
	}

	var b strings.Builder

	b.WriteString(strings.Join(formatRouteDecisionHeader(decision), "\t"))
	b.WriteByte('\n')

	for i := range decision.Candidates {
		b.WriteString(strings.Join(formatCandidateDecision(decision.Candidates[i]), "\t"))
		b.WriteByte('\n')
	}

	return b.String()
}

func formatRouteDecisionHeader(decision modelroute.Decision) []string {
	header := []string{
		"route_decision",
		"selected=" + emptyRouteValue(decision.Selected),
		"fallback_order=" + strings.Join(decision.FallbackOrder, ","),
	}
	if decision.CatalogVersion != "" {
		header = append(header, "catalog_version="+decision.CatalogVersion)
	}

	if decision.CatalogStale {
		header = append(header, "catalog_stale=true")
	}

	header = appendRouteProfileEvidence(header, decision.Profile)

	if len(decision.Constraints) > 0 {
		header = append(header, "constraints="+strings.Join(decision.Constraints, ","))
	}

	if len(decision.Policy.RequiredCapabilities) > 0 {
		header = append(header, "required_capabilities="+strings.Join(decision.Policy.RequiredCapabilities, ","))
	}

	return header
}

func appendRouteProfileEvidence(parts []string, profile modelroute.RequestProfile) []string {
	if profile.Interactive {
		parts = append(parts, "interactive=true")
	}

	if profile.Batch {
		parts = append(parts, "batch=true")
	}

	if profile.EstimatedInputTokens > 0 {
		parts = append(parts, "estimated_input_tokens="+strconv.Itoa(profile.EstimatedInputTokens))
	}

	if profile.EstimatedOutputTokens > 0 {
		parts = append(parts, "estimated_output_tokens="+strconv.Itoa(profile.EstimatedOutputTokens))
	}

	if profile.EstimatedCacheWriteTokens > 0 {
		parts = append(parts, "estimated_cache_write_tokens="+strconv.Itoa(profile.EstimatedCacheWriteTokens))
	}

	if profile.PromptCacheReuseEstimate > 0 {
		parts = append(parts, "prompt_cache_reuse_estimate="+strconv.FormatFloat(profile.PromptCacheReuseEstimate, 'f', -1, 64))
	}

	if profile.Budget > 0 {
		parts = append(parts, fmt.Sprintf("budget=%.6f", profile.Budget))
	}

	return parts
}

func formatCandidateDecision(candidate modelroute.CandidateDecision) []string {
	parts := []string{
		"candidate",
		candidate.ID,
		"status=" + candidate.Status,
		fmt.Sprintf("estimated_cost=%.6f", candidate.EstimatedCost),
	}

	parts = appendCostAndRank(parts, candidate)
	parts = appendCandidateEvidence(parts, candidate)

	if len(candidate.Rejected) > 0 {
		parts = append(parts, "rejected="+strings.Join(candidate.Rejected, ";"))
	}

	return parts
}

func appendCostAndRank(parts []string, candidate modelroute.CandidateDecision) []string {
	if candidate.ActualUsageRecorded {
		parts = append(
			parts,
			fmt.Sprintf("actual_cost=%.6f", candidate.ActualCost),
			fmt.Sprintf("actual_cost_delta=%.6f", candidate.ActualCostDelta),
		)
		parts = appendActualUsage(parts, candidate)
	}

	if candidate.Rank > 0 {
		parts = append(parts, "rank="+strconv.Itoa(candidate.Rank))
	}

	if candidate.Candidate.MaxInputTokens > 0 {
		parts = append(parts, "max_input="+strconv.Itoa(candidate.Candidate.MaxInputTokens))
	}

	if candidate.Candidate.MaxOutputTokens > 0 {
		parts = append(parts, "max_output="+strconv.Itoa(candidate.Candidate.MaxOutputTokens))
	}

	return parts
}

func appendActualUsage(parts []string, candidate modelroute.CandidateDecision) []string {
	parts = append(parts, "actual_input_tokens="+strconv.Itoa(candidate.ActualInputTokens))

	if candidate.ActualCachedTokens > 0 {
		parts = append(parts, "actual_cached_input_tokens="+strconv.Itoa(candidate.ActualCachedTokens))
	}

	if candidate.ActualCacheWrites > 0 {
		parts = append(parts, "actual_cache_write_tokens="+strconv.Itoa(candidate.ActualCacheWrites))
	}

	if candidate.ActualOutputTokens > 0 {
		parts = append(parts, "actual_output_tokens="+strconv.Itoa(candidate.ActualOutputTokens))
	}

	return parts
}

func appendCandidateEvidence(parts []string, candidate modelroute.CandidateDecision) []string {
	if candidate.Candidate.MetadataVersion != "" {
		parts = append(parts, "metadata_version="+candidate.Candidate.MetadataVersion)
	}

	if candidate.Candidate.MetadataSourceURL != "" {
		parts = append(parts, "metadata_source="+candidate.Candidate.MetadataSourceURL)
	}

	if candidate.Candidate.Deprecated {
		parts = append(parts, "deprecated=true")
	}

	if len(candidate.Candidate.Capabilities) > 0 {
		parts = append(parts, "capabilities="+strings.Join(candidate.Candidate.Capabilities, ","))
	}

	if candidate.ExpectedLatencyMS > 0 {
		parts = append(parts, "expected_latency_ms="+strconv.Itoa(candidate.ExpectedLatencyMS))
	}

	if candidate.ExpectedTTFTMS > 0 {
		parts = append(parts, "expected_ttft_ms="+strconv.Itoa(candidate.ExpectedTTFTMS))
	}

	if candidate.ObservedLatencyMS > 0 {
		parts = append(parts, "observed_latency_ms="+strconv.Itoa(candidate.ObservedLatencyMS))
	}

	if candidate.ObservedTTFTMS > 0 {
		parts = append(parts, "observed_ttft_ms="+strconv.Itoa(candidate.ObservedTTFTMS))
	}

	if candidate.TelemetryCount > 0 || candidate.FailureCount > 0 {
		parts = append(parts, "telemetry_count="+strconv.Itoa(candidate.TelemetryCount))
	}

	if candidate.FailureCount > 0 {
		parts = append(parts, "failure_count="+strconv.Itoa(candidate.FailureCount))
	}

	if candidate.RateLimitCount > 0 {
		parts = append(parts, "rate_limit_count="+strconv.Itoa(candidate.RateLimitCount))
	}

	if candidate.RateLimitUntil != "" {
		parts = append(parts, "rate_limit_until="+candidate.RateLimitUntil)
	}

	return parts
}

func emptyRouteValue(value string) string {
	if value == "" {
		return "-"
	}

	return value
}
