//nolint:wsl_v5 // The command summary printer is intentionally linear and compact.
package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"strings"
	"time"

	"github.com/tommoulard/atteler/pkg/issuewatch"
	"github.com/tommoulard/atteler/pkg/permission"
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
		{
			name:            "issue-watch",
			tier:            tierProviderless,
			match:           func(o cliOptions) bool { return o.issueWatch || strings.TrimSpace(o.issueWatchRunRef) != "" },
			runProviderless: runIssueWatchCommand,
		},
	}
}

func runIssueImplementCommand(ctx context.Context, opts cliOptions, _ *session.Store) error {
	if opts.issueOpenPR && issueWatchLocalOnlyEnvironment() {
		return errors.New("issue implement: --open-pr is disabled inside atteler issue watch local commands; publish later from an explicit workflow")
	}

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

func issueWatchLocalOnlyEnvironment() bool {
	return strings.TrimSpace(os.Getenv("ATTELER_ISSUE_WATCH")) == "1"
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

func runIssueWatchCommand(ctx context.Context, opts cliOptions, _ *session.Store) error {
	cwd, err := os.Getwd()
	if err != nil {
		return fmt.Errorf("issue watch: locate working directory: %w", err)
	}

	interval := time.Minute
	if opts.issueWatchIntervalSeconds.set {
		interval = time.Duration(opts.issueWatchIntervalSeconds.value) * time.Second
	}

	for {
		result, err := runIssueWatchIteration(ctx, cwd, opts)
		printIssueWatchResult(result)
		if err != nil {
			return err
		}

		if opts.issueWatchOnce || opts.issueWatchDryRun || strings.TrimSpace(opts.issueWatchRunRef) != "" {
			return nil
		}

		timer := time.NewTimer(interval)
		select {
		case <-ctx.Done():
			timer.Stop()
			return fmt.Errorf("issue watch: context canceled: %w", ctx.Err())
		case <-timer.C:
		}
	}
}

func runIssueWatchIteration(ctx context.Context, root string, opts cliOptions) (issuewatch.Result, error) {
	runRef := strings.TrimSpace(opts.issueWatchRunRef)
	trackerConfig, err := issueWatchGitHubTrackerConfig(ctx, opts, runRef == "")
	if err != nil {
		return issuewatch.Result{}, err
	}

	githubTracker := symphony.NewGitHubClient(trackerConfig)
	tracker := issuewatch.Tracker(githubTracker)
	if runRef != "" {
		tracker = issueWatchRunTracker{tracker: githubTracker, ref: runRef}
	}

	result, err := issuewatch.RunOnce(ctx, issuewatch.Options{
		Root:               root,
		Repository:         opts.issueWatchGitHub,
		Command:            opts.issueWatchCommand,
		Labels:             []string(opts.issueWatchLabels),
		ValidationCommands: []string(opts.issueWatchValidationCommands),
		CommandTimeout:     issueWatchCommandTimeout(opts),
		DryRun:             opts.issueWatchDryRun,
		AllowEmptyLabels:   runRef != "",
		IgnoreState:        runRef != "",
		Tracker:            tracker,
	})
	if err != nil {
		return result, fmt.Errorf("run issue-watch iteration: %w", err)
	}
	if runRef != "" && len(result.Candidates) == 0 {
		return result, fmt.Errorf("issue watch run: no eligible issue found for %s", runRef)
	}

	return result, nil
}

func issueWatchCommandTimeout(opts cliOptions) time.Duration {
	if opts.issueWatchCommandTimeout.set {
		return time.Duration(opts.issueWatchCommandTimeout.value) * time.Second
	}

	return 0
}

type issueWatchIssueFetcher interface {
	FetchIssueStatesByIDs(context.Context, []string) ([]symphony.Issue, error)
}

type issueWatchRunTracker struct {
	tracker issueWatchIssueFetcher
	ref     string
}

func (t issueWatchRunTracker) FetchCandidateIssues(ctx context.Context) ([]symphony.Issue, error) {
	issues, err := t.tracker.FetchIssueStatesByIDs(ctx, []string{t.ref})
	if err != nil {
		return nil, fmt.Errorf("fetch issue %s for issue watch run: %w", t.ref, err)
	}
	if len(issues) == 0 {
		return nil, fmt.Errorf("issue watch run: no issue found for %s", t.ref)
	}

	return issues, nil
}

func issueWatchGitHubTrackerConfig(ctx context.Context, opts cliOptions, requireLabel bool) (symphony.TrackerConfig, error) {
	owner, repo := splitGitHubRepository(opts.issueWatchGitHub)
	if owner == "" || repo == "" {
		return symphony.TrackerConfig{}, errors.New("issue watch requires --github owner/repo")
	}

	labels := []string(opts.issueWatchLabels)
	if len(labels) == 0 && requireLabel {
		return symphony.TrackerConfig{}, errors.New("issue watch requires at least one --label")
	}

	endpoint := strings.TrimSpace(opts.issueWatchGitHubEndpoint)
	if endpoint == "" {
		endpoint = defaultGitHubAPIEndpoint
	}

	token, err := issueWatchGitHubToken(ctx, opts.issueWatchGitHubToken)
	if err != nil {
		return symphony.TrackerConfig{}, err
	}

	return symphony.TrackerConfig{
		Kind:         "github",
		Endpoint:     endpoint,
		APIKey:       token,
		Repository:   strings.TrimSpace(opts.issueWatchGitHub),
		Owner:        owner,
		Repo:         repo,
		ActiveStates: []string{"OPEN"},
		Labels:       labels,
	}, nil
}

func issueWatchGitHubToken(ctx context.Context, explicit string) (string, error) {
	explicit = strings.TrimSpace(explicit)
	if explicit != "" {
		if err := authorizeIssueWatchGitHubTokenAccess(ctx); err != nil {
			return "", err
		}

		return explicit, nil
	}

	if err := authorizeIssueWatchGitHubTokenAccess(ctx); err != nil {
		if permission.ErrDenied(err) {
			return "", nil
		}

		return "", err
	}

	return firstNonEmpty(os.Getenv("GITHUB_TOKEN"), os.Getenv("GH_TOKEN")), nil
}

func authorizeIssueWatchGitHubTokenAccess(ctx context.Context) error {
	decision := permission.Evaluate(ctx, nil, permission.Request{
		Action: "resolve issue-watch GitHub token",
		Source: "atteler.issue_watch.github",
		Target: "--issue-watch-github-token/GITHUB_TOKEN/GH_TOKEN",
		Operations: []permission.Operation{{
			Kind:   permission.OperationCredentialAccess,
			Action: "resolve issue-watch GitHub token",
			Source: "atteler.issue_watch.github",
			Target: "--issue-watch-github-token/GITHUB_TOKEN/GH_TOKEN",
		}},
	})
	if decision.Allowed {
		return nil
	}

	return &permission.Error{Decision: decision}
}

func printIssueWatchResult(result issuewatch.Result) {
	if result.StartedAt.IsZero() && len(result.Candidates) == 0 && len(result.Runs) == 0 {
		return
	}

	mode := "iteration"
	if result.DryRun {
		mode = issueWatchDryRunModeName
	}
	fmt.Printf("issue watch: %s candidates=%d runs=%d\n", mode, len(result.Candidates), len(result.Runs))
	if strings.TrimSpace(result.StatePath) != "" && !result.DryRun {
		fmt.Printf("state: %s\n", result.StatePath)
	}

	if result.DryRun {
		for i := range result.Candidates {
			candidate := &result.Candidates[i]
			fmt.Printf("candidate: %s\t%s\n", firstNonEmpty(candidate.Identifier, candidate.ID), candidate.Title)
		}

		return
	}

	for i := range result.Runs {
		run := &result.Runs[i]
		fmt.Printf("run: %s\t%s\t%s\n", firstNonEmpty(run.IssueIdentifier, run.IssueID), run.RunID, run.Status)
		fmt.Printf("worktree: %s\n", run.WorktreePath)
		fmt.Printf("artifacts: %s\n", run.Artifacts.Dir)
		if run.Workflow.Run {
			fmt.Printf("workflow: %s\n", run.Workflow.Summary)
		}
		fmt.Printf("validation: %s\n", run.Validation.Summary)
		fmt.Println("publish: skipped (local-only issue watch)")
	}
}
