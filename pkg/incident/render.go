//nolint:wsl_v5 // Report rendering is easier to read with section-by-section blocks.
package incident

import (
	"fmt"
	"sort"
	"strings"
	"time"
)

const maxRenderedObservations = 3

// RenderText renders a human-readable diagnosis report.
func RenderText(analysis Analysis) string {
	analysis = RedactAnalysis(analysis)

	var b strings.Builder

	writeTextIncidentSummary(&b, analysis)

	b.WriteString("\nDiagnosis:\n")
	writeDiagnosisBullets(&b, analysis)

	b.WriteString("\nObservability context:\n")
	writeObservabilityContext(&b, analysis.Incident)

	b.WriteString("\nReproduction:\n")
	writeCommandResult(&b, analysis.Reproduction)

	b.WriteString("\nTest plan:\n")
	writePlan(&b, analysis.TestPlan)
	writeTestChanges(&b, analysis.WorktreeChanges)

	b.WriteString("\nCode changes:\n")
	writeCodeChanges(&b, analysis.WorktreeChanges, "- Diagnose does not mutate source by itself; include the regression test and fix diff before opening the PR.")

	b.WriteString("\nFix plan:\n")
	writePlan(&b, analysis.FixPlan)

	b.WriteString("\nValidation:\n")
	if len(analysis.Validation) == 0 {
		b.WriteString("- not run: no validation command supplied yet\n")
	} else {
		for i := range analysis.Validation {
			writeCommandResult(&b, analysis.Validation[i])
		}
	}

	b.WriteString("\nRisk:\n")
	fmt.Fprintf(&b, "- %s: %s\n", analysis.Risk.Level, analysis.Risk.Rationale)
	if len(analysis.Risk.SuggestedReviewers) > 0 {
		fmt.Fprintf(&b, "- Suggested reviewers: %s\n", strings.Join(analysis.Risk.SuggestedReviewers, ", "))
	}

	if len(analysis.Warnings) > 0 {
		b.WriteString("\nWarnings:\n")
		for _, warning := range analysis.Warnings {
			fmt.Fprintf(&b, "- %s\n", warning)
		}
	}

	b.WriteString("\nPrivacy:\n")
	fmt.Fprintf(&b, "- Redaction policy: %s\n", analysis.RedactionPolicy)
	b.WriteString("- Do not paste raw production secrets, tokens, cookies, user IDs, emails, or payloads into tests or PRs.\n")

	if analysis.FixPrompt != "" {
		b.WriteString("\nHarness fix prompt:\n")
		b.WriteString(analysis.FixPrompt)
		if !strings.HasSuffix(analysis.FixPrompt, "\n") {
			b.WriteByte('\n')
		}
	}

	return b.String()
}

func writeTextIncidentSummary(b *strings.Builder, analysis Analysis) {
	inc := analysis.Incident
	fmt.Fprintf(b, "Incident: %s %s\n", labelOrUnknown(inc.Source), labelOrUnknown(inc.Reference))
	if inc.Service != "" {
		fmt.Fprintf(b, "Service: %s\n", inc.Service)
	}
	if inc.Environment != "" {
		fmt.Fprintf(b, "Environment: %s\n", inc.Environment)
	}
	if inc.Title != "" {
		fmt.Fprintf(b, "Title: %s\n", inc.Title)
	}
	if inc.Message != "" || inc.ErrorType != "" {
		fmt.Fprintf(b, "Error: %s\n", errorSummary(inc.ErrorType, inc.Message))
	}
	if !inc.FirstSeen.IsZero() {
		fmt.Fprintf(b, "First seen: %s\n", inc.FirstSeen.Format(timeFormatRFC3339))
	}
	if inc.Release != "" {
		fmt.Fprintf(b, "Release: %s\n", inc.Release)
	}
	if summary := likelyRegressionSummary(analysis); summary != "" {
		fmt.Fprintf(b, "Likely regression: %s\n", summary)
	}
}

// RenderMarkdown renders the diagnosis as a PR-ready Markdown body.
func RenderMarkdown(analysis Analysis) string {
	analysis = RedactAnalysis(analysis)

	var b strings.Builder

	inc := analysis.Incident
	b.WriteString("# Production incident diagnosis\n\n")
	fmt.Fprintf(&b, "- **Incident:** %s %s\n", labelOrUnknown(inc.Source), labelOrUnknown(inc.Reference))
	if inc.URL != "" {
		fmt.Fprintf(&b, "- **Link:** %s\n", inc.URL)
	}
	if inc.Service != "" {
		fmt.Fprintf(&b, "- **Service:** %s\n", inc.Service)
	}
	if inc.Environment != "" {
		fmt.Fprintf(&b, "- **Environment:** %s\n", inc.Environment)
	}
	if inc.Title != "" {
		fmt.Fprintf(&b, "- **Title:** %s\n", inc.Title)
	}
	if inc.Message != "" || inc.ErrorType != "" {
		fmt.Fprintf(&b, "- **Error:** %s\n", errorSummary(inc.ErrorType, inc.Message))
	}
	if !inc.FirstSeen.IsZero() {
		fmt.Fprintf(&b, "- **First seen:** %s\n", inc.FirstSeen.Format(timeFormatRFC3339))
	}
	if inc.Release != "" {
		fmt.Fprintf(&b, "- **Release:** %s\n", inc.Release)
	}
	if summary := likelyRegressionSummary(analysis); summary != "" {
		fmt.Fprintf(&b, "- **Likely regression:** %s\n", summary)
	}

	b.WriteString("\n## Diagnosis\n\n")
	writeDiagnosisBullets(&b, analysis)

	b.WriteString("\n## Observability context\n\n")
	writeObservabilityContext(&b, inc)

	b.WriteString("\n## Reproduction\n\n")
	writeCommandResult(&b, analysis.Reproduction)

	b.WriteString("\n## Tests\n\n")
	writePlan(&b, analysis.TestPlan)
	writeTestChanges(&b, analysis.WorktreeChanges)

	b.WriteString("\n## Code changes\n\n")
	writeCodeChanges(&b, analysis.WorktreeChanges, "- See this PR diff for the regression test and fix. The diagnose step itself only prepares a redacted, test-first repair plan.")

	b.WriteString("\n## Fix plan\n\n")
	writePlan(&b, analysis.FixPlan)

	b.WriteString("\n## Validation\n\n")
	if len(analysis.Validation) == 0 {
		b.WriteString("- not run: no validation command supplied yet\n")
	} else {
		for i := range analysis.Validation {
			writeCommandResult(&b, analysis.Validation[i])
		}
	}

	b.WriteString("\n## Risk\n\n")
	fmt.Fprintf(&b, "- **%s:** %s\n", analysis.Risk.Level, analysis.Risk.Rationale)
	if len(analysis.Risk.SuggestedReviewers) > 0 {
		fmt.Fprintf(&b, "- **Suggested reviewers:** %s\n", strings.Join(analysis.Risk.SuggestedReviewers, ", "))
	}

	writeMarkdownPRReadiness(&b, analysis.PRPlan)

	if len(analysis.Warnings) > 0 {
		b.WriteString("\n## Warnings\n\n")
		for _, warning := range analysis.Warnings {
			fmt.Fprintf(&b, "- %s\n", warning)
		}
	}

	b.WriteString("\n## Privacy\n\n")
	fmt.Fprintf(&b, "- Redaction policy: `%s`\n", analysis.RedactionPolicy)
	b.WriteString("- Production secrets, tokens, cookies, user identifiers, email addresses, and sensitive payloads are redacted by default.\n")

	return b.String()
}

func writeMarkdownPRReadiness(b *strings.Builder, plan PRPlan) {
	if strings.TrimSpace(plan.Summary) == "" && len(plan.BodySections) == 0 && len(plan.SuggestedReviewers) == 0 {
		return
	}

	b.WriteString("\n## PR readiness\n\n")
	if plan.Summary != "" {
		fmt.Fprintf(b, "- %s\n", plan.Summary)
	}
	if len(plan.BodySections) > 0 {
		fmt.Fprintf(b, "- PR body sections: %s\n", strings.Join(plan.BodySections, ", "))
	}
	if len(plan.SuggestedReviewers) > 0 {
		fmt.Fprintf(b, "- Suggested reviewers: %s\n", strings.Join(plan.SuggestedReviewers, ", "))
	}
}

// BuildFixPrompt returns the prompt that can be handed to Atteler's existing
// one-shot/agent loop after diagnosis has gathered safe incident context.
func BuildFixPrompt(analysis Analysis) string {
	analysis = RedactAnalysis(analysis)

	var b strings.Builder

	b.WriteString("Diagnose and fix this production incident using a safe local workflow.\n\n")
	b.WriteString("Rules:\n")
	b.WriteString("- Use only the redacted incident context below; do not request or persist raw production secrets or PII.\n")
	b.WriteString("- Inspect likely source files first, then attempt a safe local reproduction if feasible.\n")
	b.WriteString("- Prefer creating a failing regression test before editing production code.\n")
	b.WriteString("- Implement the smallest fix, run targeted validation, then broader validation.\n")
	b.WriteString("- Do not run destructive, expensive, production-impacting, or credentialed actions without explicit user approval.\n")
	b.WriteString("- Prepare a PR summary with linked incident context, diagnosis, code changes, tests, validation, risk, and reviewers.\n\n")

	b.WriteString("Incident summary:\n")
	fmt.Fprintf(&b, "- Source: %s\n", labelOrUnknown(analysis.Incident.Source))
	fmt.Fprintf(&b, "- Reference: %s\n", labelOrUnknown(analysis.Incident.Reference))
	if analysis.Incident.Service != "" {
		fmt.Fprintf(&b, "- Service: %s\n", analysis.Incident.Service)
	}
	if analysis.Incident.Title != "" {
		fmt.Fprintf(&b, "- Title: %s\n", analysis.Incident.Title)
	}
	if analysis.Incident.Message != "" || analysis.Incident.ErrorType != "" {
		fmt.Fprintf(&b, "- Error: %s\n", errorSummary(analysis.Incident.ErrorType, analysis.Incident.Message))
	}
	writeFixPromptObservability(&b, analysis.Incident)
	writeFixPromptCommandSection(&b, "Reproduction result", []CommandResult{analysis.Reproduction})

	if len(analysis.CodeCandidates) > 0 {
		b.WriteString("\nLikely source files:\n")
		for i := range analysis.CodeCandidates {
			candidate := analysis.CodeCandidates[i]
			fmt.Fprintf(&b, "- %s:%d (%s, confidence=%s)\n", candidate.Path, candidate.Line, candidateFunction(candidate), candidate.Confidence)
			if len(candidate.Owners) > 0 {
				fmt.Fprintf(&b, "  Owners: %s\n", strings.Join(candidate.Owners, ", "))
			}
		}
	}

	if len(analysis.RecentChanges) > 0 {
		b.WriteString("\nCorrelated recent changes:\n")
		for i := range analysis.RecentChanges {
			change := analysis.RecentChanges[i]
			fmt.Fprintf(&b, "- %s %s", shortHash(change.Hash), change.Subject)
			if change.Match != "" {
				fmt.Fprintf(&b, " %s", change.Match)
			}
			if len(change.PullRequests) > 0 {
				fmt.Fprintf(&b, " PRs: %s", strings.Join(change.PullRequests, ", "))
			}
			b.WriteByte('\n')
		}
	}

	writeFixPromptPlanSection(&b, "Test-first plan", analysis.TestPlan)
	writeFixPromptPlanSection(&b, "Fix plan", analysis.FixPlan)
	writeFixPromptCommandSection(&b, "Validation already run", analysis.Validation)

	if len(analysis.WorktreeChanges) > 0 {
		b.WriteString("\nCurrent local changes:\n")
		writeCodeChanges(&b, analysis.WorktreeChanges, "")
	}

	return b.String()
}

func writeFixPromptPlanSection(b *strings.Builder, heading string, plan Plan) {
	if plan.Summary == "" && plan.NotRunReason == "" && len(plan.Steps) == 0 {
		return
	}

	fmt.Fprintf(b, "\n%s:\n", heading)
	writePlan(b, plan)
}

func writeFixPromptCommandSection(b *strings.Builder, heading string, results []CommandResult) {
	results = nonEmptyCommandResults(results)
	if len(results) == 0 {
		return
	}

	fmt.Fprintf(b, "\n%s:\n", heading)
	for i := range results {
		writeCommandResult(b, results[i])
	}
}

func nonEmptyCommandResults(results []CommandResult) []CommandResult {
	out := make([]CommandResult, 0, len(results))
	for i := range results {
		result := results[i]
		if result.Command == "" && result.Status == "" && result.NotRunReason == "" && result.Stdout == "" && result.Stderr == "" && result.Error == "" {
			continue
		}

		out = append(out, result)
	}

	return out
}

func writeCodeChanges(b *strings.Builder, changes []WorktreeChange, emptyMessage string) {
	if len(changes) == 0 {
		if emptyMessage != "" {
			b.WriteString(emptyMessage)
			if !strings.HasSuffix(emptyMessage, "\n") {
				b.WriteByte('\n')
			}
		}

		return
	}

	for i := range changes {
		change := changes[i]
		fmt.Fprintf(b, "- %s %s\n", labelOrUnknown(change.Status), change.Path)
	}
}

func likelyRegressionSummary(analysis Analysis) string {
	change, ok := likelyRegressionChange(analysis)
	if !ok {
		return ""
	}

	parts := make([]string, 0, 3)
	if len(change.PullRequests) == 1 {
		parts = append(parts, "PR "+change.PullRequests[0])
	} else if len(change.PullRequests) > 1 {
		parts = append(parts, "PRs "+strings.Join(change.PullRequests, ", "))
	}
	if change.Hash != "" {
		parts = append(parts, "commit "+shortHash(change.Hash))
	}
	if change.Subject != "" {
		parts = append(parts, change.Subject)
	}

	return strings.Join(parts, " / ")
}

func likelyRegressionChange(analysis Analysis) (Change, bool) {
	if len(analysis.RecentChanges) == 0 {
		return Change{}, false
	}

	for i := range analysis.Incident.Deployments {
		deployCommit := analysis.Incident.Deployments[i].Commit
		for j := range analysis.RecentChanges {
			if hashReferencesSameCommit(deployCommit, analysis.RecentChanges[j].Hash) {
				return analysis.RecentChanges[j], true
			}
		}
	}

	return analysis.RecentChanges[0], true
}

func hashReferencesSameCommit(left, right string) bool {
	left = strings.TrimSpace(left)
	right = strings.TrimSpace(right)
	if len(left) < 7 || len(right) < 7 {
		return false
	}

	return strings.HasPrefix(left, right) || strings.HasPrefix(right, left)
}

func writeTestChanges(b *strings.Builder, changes []WorktreeChange) {
	tests := testWorktreeChanges(changes)
	if len(tests) == 0 {
		return
	}

	b.WriteString("- Tests added/changed:\n")
	for i := range tests {
		change := tests[i]
		fmt.Fprintf(b, "  - %s %s\n", labelOrUnknown(change.Status), change.Path)
	}
}

func testWorktreeChanges(changes []WorktreeChange) []WorktreeChange {
	out := make([]WorktreeChange, 0)
	for i := range changes {
		if isTestPath(changes[i].Path) {
			out = append(out, changes[i])
		}
	}

	return out
}

func isTestPath(path string) bool {
	path = strings.ToLower(strings.TrimSpace(path))
	if path == "" {
		return false
	}

	base := strings.ToLower(pathpkgBase(path))
	if strings.HasPrefix(path, "test/") ||
		strings.HasPrefix(path, "tests/") ||
		strings.Contains(path, "/test/") ||
		strings.Contains(path, "/tests/") ||
		strings.Contains(path, "__tests__") {
		return true
	}

	for _, suffix := range testFileSuffixes {
		if strings.HasSuffix(base, suffix) {
			return true
		}
	}

	return false
}

var testFileSuffixes = []string{
	"_test.go",
	".test.go",
	"_test.py",
	"_spec.rb",
	".spec.ts",
	".spec.tsx",
	".test.ts",
	".test.tsx",
	".spec.js",
	".test.js",
	".spec.jsx",
	".test.jsx",
}

func pathpkgBase(path string) string {
	path = strings.TrimRight(strings.ReplaceAll(path, "\\", "/"), "/")
	if path == "" {
		return ""
	}
	if slash := strings.LastIndexByte(path, '/'); slash >= 0 {
		return path[slash+1:]
	}

	return path
}

func writeDiagnosisBullets(b *strings.Builder, analysis Analysis) {
	if len(analysis.CodeCandidates) == 0 {
		b.WriteString("- No local source candidate was identified from the incident stack trace.\n")
	} else {
		for i := range analysis.CodeCandidates {
			candidate := analysis.CodeCandidates[i]
			fmt.Fprintf(b, "- Stack trace points to %s:%d (%s, confidence=%s): %s",
				candidate.Path,
				candidate.Line,
				candidateFunction(candidate),
				candidate.Confidence,
				candidate.Reason,
			)
			if len(candidate.Owners) > 0 {
				fmt.Fprintf(b, " Owners: %s", strings.Join(candidate.Owners, ", "))
			}
			b.WriteString(".\n")
		}
	}

	if len(analysis.RecentChanges) == 0 {
		b.WriteString("- No recent local git history correlation was found or git history was unavailable.\n")
	} else {
		for i := range analysis.RecentChanges {
			change := analysis.RecentChanges[i]
			fmt.Fprintf(b, "- Related recent change: %s %s", shortHash(change.Hash), change.Subject)
			if change.Match != "" {
				fmt.Fprintf(b, " (%s)", change.Match)
			}
			if len(change.PullRequests) > 0 {
				fmt.Fprintf(b, " PRs: %s", strings.Join(change.PullRequests, ", "))
			}
			b.WriteString(".\n")
		}
	}

	if len(analysis.Incident.Traces) > 0 {
		fmt.Fprintf(b, "- Trace context available: %d trace observation(s).\n", len(analysis.Incident.Traces))
	}
	if len(analysis.Incident.Logs) > 0 {
		fmt.Fprintf(b, "- Logs/breadcrumbs available: %d observation(s).\n", len(analysis.Incident.Logs))
	}
	if len(analysis.Incident.Metrics) > 0 {
		fmt.Fprintf(b, "- Metrics available: %d observation(s).\n", len(analysis.Incident.Metrics))
	}
	if len(analysis.Incident.Deployments) > 0 {
		fmt.Fprintf(b, "- Deploy/release metadata available: %d record(s).\n", len(analysis.Incident.Deployments))
	}
}

func writeObservabilityContext(b *strings.Builder, inc Context) {
	wrote := false
	if hasRequestContext(inc.Request) {
		writeRequestContext(b, inc.Request)
		wrote = true
	}

	wrote = writeTagContext(b, inc.Tags) || wrote
	wrote = writeObservationList(b, "Trace", inc.Traces) || wrote
	wrote = writeObservationList(b, "Log/breadcrumb", inc.Logs) || wrote
	wrote = writeObservationList(b, "Metric", inc.Metrics) || wrote
	wrote = writeDeploymentList(b, inc.Deployments) || wrote

	if !wrote {
		b.WriteString("- no request, tag, log, trace, metric, or deployment details were available in the redacted incident payload\n")
	}
}

func hasRequestContext(request Request) bool {
	return request.Method != "" ||
		request.URL != "" ||
		request.Data != "" ||
		len(request.Headers) > 0 ||
		len(request.Metadata) > 0
}

func writeRequestContext(b *strings.Builder, request Request) {
	if request.Method != "" || request.URL != "" {
		fmt.Fprintf(b, "- Request: %s %s\n", labelOrUnknown(request.Method), labelOrUnknown(request.URL))
	}
	if keys := sortedKeys(request.Metadata); len(keys) > 0 {
		fmt.Fprintf(b, "- Request metadata present: %s\n", strings.Join(keys, ", "))
	}
	if keys := sortedKeys(request.Headers); len(keys) > 0 {
		fmt.Fprintf(b, "- Request headers present with values redacted: %s\n", strings.Join(keys, ", "))
	}
	if request.Data != "" {
		fmt.Fprintf(b, "- Request payload captured and redacted (%d bytes after redaction).\n", len(request.Data))
	}
}

func writeTagContext(b *strings.Builder, tags map[string]string) bool {
	keys := sortedKeys(tags)
	if len(keys) == 0 {
		return false
	}

	if len(keys) > maxRenderedObservations*3 {
		fmt.Fprintf(b, "- Tags/context present with sensitive values redacted: %s (+%d more)\n", strings.Join(keys[:maxRenderedObservations*3], ", "), len(keys)-maxRenderedObservations*3)
		return true
	}

	fmt.Fprintf(b, "- Tags/context present with sensitive values redacted: %s\n", strings.Join(keys, ", "))
	return true
}

func writeObservationList(b *strings.Builder, label string, observations []Observation) bool {
	if len(observations) == 0 {
		return false
	}

	limit := min(len(observations), maxRenderedObservations)
	for i := range limit {
		observation := observations[i]
		fmt.Fprintf(b, "- %s: %s", label, observationSummary(observation))
		if keys := sortedKeys(observation.Fields); len(keys) > 0 {
			fmt.Fprintf(b, " (fields: %s)", strings.Join(keys, ", "))
		}
		b.WriteByte('\n')
	}
	if len(observations) > limit {
		fmt.Fprintf(b, "- %s: %d additional observation(s) omitted from the report.\n", label, len(observations)-limit)
	}

	return true
}

func observationSummary(observation Observation) string {
	parts := make([]string, 0, 4)
	if !observation.Timestamp.IsZero() {
		parts = append(parts, observation.Timestamp.Format(timeFormatRFC3339))
	}
	if observation.Source != "" {
		parts = append(parts, observation.Source)
	}
	if observation.Name != "" {
		parts = append(parts, observation.Name)
	}
	if observation.Message != "" {
		parts = append(parts, truncateForTitle(observation.Message, 160))
	}
	if len(parts) == 0 {
		return "redacted observation metadata captured"
	}

	return strings.Join(parts, " | ")
}

func writeDeploymentList(b *strings.Builder, deployments []Deployment) bool {
	if len(deployments) == 0 {
		return false
	}

	limit := min(len(deployments), maxRenderedObservations)
	for i := range limit {
		fmt.Fprintf(b, "- Deployment: %s\n", deploymentSummary(deployments[i]))
	}
	if len(deployments) > limit {
		fmt.Fprintf(b, "- Deployment: %d additional record(s) omitted from the report.\n", len(deployments)-limit)
	}

	return true
}

func deploymentSummary(deployment Deployment) string {
	parts := make([]string, 0, 4)
	if deployment.Version != "" {
		parts = append(parts, "version="+deployment.Version)
	}
	if deployment.Commit != "" {
		parts = append(parts, "commit="+shortHash(deployment.Commit))
	}
	if deployment.Environment != "" {
		parts = append(parts, "environment="+deployment.Environment)
	}
	if !deployment.DeployedAt.IsZero() {
		parts = append(parts, "deployed_at="+deployment.DeployedAt.Format(timeFormatRFC3339))
	}
	if len(parts) == 0 {
		return "redacted deployment metadata captured"
	}

	return strings.Join(parts, " ")
}

func writeFixPromptObservability(b *strings.Builder, inc Context) {
	if !hasRequestContext(inc.Request) &&
		len(inc.Tags) == 0 &&
		len(inc.Traces) == 0 &&
		len(inc.Logs) == 0 &&
		len(inc.Metrics) == 0 &&
		len(inc.Deployments) == 0 {
		return
	}

	b.WriteString("\nRedacted observability context:\n")
	writeObservabilityContext(b, inc)
}

func sortedKeys(values map[string]string) []string {
	if len(values) == 0 {
		return nil
	}

	keys := make([]string, 0, len(values))
	for key := range values {
		if strings.TrimSpace(key) != "" {
			keys = append(keys, key)
		}
	}
	sort.Strings(keys)

	return keys
}

func errorSummary(errorType, message string) string {
	return strings.TrimSpace(errorType + " " + message)
}

func writeCommandResult(b *strings.Builder, result CommandResult) {
	status := result.Status
	if status == "" {
		status = commandStatusNotRun
	}
	if result.Command == "" {
		if result.NotRunReason != "" {
			fmt.Fprintf(b, "- %s: %s\n", status, result.NotRunReason)
		} else {
			fmt.Fprintf(b, "- %s\n", status)
		}

		return
	}

	fmt.Fprintf(b, "- `%s`: %s", result.Command, status)
	if result.Duration > 0 {
		fmt.Fprintf(b, " in %s", result.Duration.Round(time.Millisecond))
	}
	if result.Error != "" {
		fmt.Fprintf(b, " (%s)", result.Error)
	}
	b.WriteByte('\n')
	writeCommandOutputSnippet(b, "stdout", result.Stdout)
	writeCommandOutputSnippet(b, "stderr", result.Stderr)
}

func writeCommandOutputSnippet(b *strings.Builder, label, output string) {
	output = strings.TrimSpace(output)
	if output == "" {
		return
	}

	fmt.Fprintf(b, "  - %s: %s\n", label, truncateForTitle(singleLine(output), 240))
}

func singleLine(value string) string {
	return strings.Join(strings.Fields(value), " ")
}

func writePlan(b *strings.Builder, plan Plan) {
	if plan.Summary != "" {
		fmt.Fprintf(b, "- %s\n", plan.Summary)
	}
	if plan.NotRunReason != "" {
		fmt.Fprintf(b, "- not run: %s\n", plan.NotRunReason)
	}
	for _, step := range plan.Steps {
		fmt.Fprintf(b, "- %s\n", step)
	}
}

func labelOrUnknown(value string) string {
	if strings.TrimSpace(value) == "" {
		return "unknown"
	}

	return value
}

func shortHash(hash string) string {
	hash = strings.TrimSpace(hash)
	if len(hash) <= 12 {
		return hash
	}

	return hash[:12]
}

const timeFormatRFC3339 = "2006-01-02T15:04:05Z07:00"
