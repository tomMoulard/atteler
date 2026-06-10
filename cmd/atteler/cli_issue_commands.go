//nolint:wsl_v5 // The command summary printer is intentionally linear and compact.
package main

import (
	"context"
	"fmt"
	"log/slog"
	"strings"

	"github.com/tommoulard/atteler/pkg/privacy"
	"github.com/tommoulard/atteler/pkg/session"
	"github.com/tommoulard/atteler/pkg/symphony"
)

func providerlessIssueCommands() []command {
	return []command{
		{
			name:            "issue-implement",
			tier:            tierProviderless,
			match:           func(o cliOptions) bool { return o.issueImplementRequested },
			runProviderless: runIssueImplementCommand,
		},
	}
}

func runIssueImplementCommand(ctx context.Context, opts cliOptions, _ *session.Store) error {
	result, err := symphony.ImplementIssue(ctx, symphony.IssueImplementOptions{
		Logger:          slog.Default(),
		WorkflowPath:    opts.issueWorkflowPath,
		IssueRef:        opts.issueImplementRef,
		BaseBranch:      opts.issueBaseBranch,
		OpenPullRequest: opts.issueOpenPR,
		RunTests:        opts.issueRunTests,
		RunLint:         opts.issueRunLint,
		UpdateDocs:      opts.issueUpdateDocs,
		UpdateChangelog: opts.issueUpdateChangelog,
	})
	if err != nil {
		if shouldPrintIssueImplementResult(result) {
			printIssueImplementResult(result)
		}

		return fmt.Errorf("issue implement: %w", err)
	}

	printIssueImplementResult(result)
	return nil
}

func shouldPrintIssueImplementResult(result symphony.RunResult) bool {
	return !result.StartedAt.IsZero() || strings.TrimSpace(result.WorkspacePath) != "" || result.Publish != nil
}

func printIssueImplementResult(result symphony.RunResult) {
	fmt.Printf("issue implementation: %s\n", result.Status)
	if strings.TrimSpace(result.WorkspacePath) != "" {
		fmt.Printf("workspace: %s\n", result.WorkspacePath)
	}

	if result.Publish == nil {
		return
	}

	if result.Publish.SkippedReason != "" {
		fmt.Printf("publish: skipped (%s)\n", result.Publish.SkippedReason)
		return
	}

	if result.Publish.Branch != "" {
		fmt.Printf("branch: %s\n", result.Publish.Branch)
	}
	if result.Publish.CommitSHA != "" {
		fmt.Printf("commit: %s\n", result.Publish.CommitSHA)
	}
	if result.Publish.PullRequestURL != "" {
		fmt.Printf("pull_request: %s\n", result.Publish.PullRequestURL)
	}
	for _, line := range issueImplementValidationSummary(result.Publish.Verification) {
		fmt.Println(line)
	}
	if result.Publish.DraftDueToFailedVerification {
		fmt.Println("draft_reason: required verification gate failed")
	}
	if result.Publish.DraftDueToRunFailure {
		fmt.Println("draft_reason: implementation incomplete")
	}
}

func issueImplementValidationSummary(report *symphony.VerificationReport) []string {
	if report == nil {
		return nil
	}

	if !report.Configured {
		return []string{"validation: no local gates configured"}
	}

	status := string(symphony.VerificationFailed)
	if report.Passed {
		status = string(symphony.VerificationPassed)
	}

	lines := []string{fmt.Sprintf("validation: %s (%d gate(s))", status, len(report.Gates))}
	failedRequired := trimIssueImplementSummaryValues(report.FailedRequired)
	if len(failedRequired) > 0 {
		lines = append(lines, "failed_required_gates: "+strings.Join(failedRequired, ", "))
	}
	failedOptional := issueImplementOptionalFailureNames(report)
	if len(failedOptional) > 0 {
		lines = append(lines, "failed_optional_gates: "+strings.Join(failedOptional, ", "))
	}

	return lines
}

func issueImplementOptionalFailureNames(report *symphony.VerificationReport) []string {
	if report == nil {
		return nil
	}

	var failed []string
	for i := range report.Gates {
		gate := report.Gates[i]
		if gate.Required || gate.Status != symphony.VerificationFailed {
			continue
		}

		failed = append(failed, gate.Name)
	}

	return trimIssueImplementSummaryValues(failed)
}

func trimIssueImplementSummaryValues(values []string) []string {
	out := make([]string, 0, len(values))
	for _, value := range values {
		value = strings.TrimSpace(privacy.RedactText(value))
		if value != "" {
			out = append(out, value)
		}
	}

	return out
}
