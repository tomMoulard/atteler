//nolint:cyclop,gocognit,gosec,nestif,wsl_v5 // Publishing coordinates local git, GitHub API, and token-backed push intentionally.
package symphony

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/tommoulard/atteler/pkg/privacy"
	"github.com/tommoulard/atteler/pkg/shell"
)

const (
	publishLabelAPI            = "api"
	publishLabelArchitecture   = "architecture"
	publishLabelAuth           = "auth"
	publishLabelAuthentication = "authentication"
	publishLabelBackend        = "backend"
	publishLabelDocs           = "docs"
	publishLabelDocumentation  = "documentation"
	publishLabelFrontend       = "frontend"
	publishLabelQuality        = "quality"
	publishLabelSecurity       = "security"
	publishLabelTest           = "test"
	publishLabelTesting        = "testing"
	publishLabelUI             = "ui"
	publishLabelUX             = "ux"
)

type gitCommandRunner func(context.Context, string, []string, shell.AuditContext, ...string) ([]byte, error)

// PublishWorkspace commits a successful worker workspace, pushes it to GitHub,
// opens or updates a pull request, then removes dispatch labels from the source
// issue so the scheduler stops re-queueing it.
func PublishWorkspace(ctx context.Context, cfg Config, issue Issue, workspace Workspace, logger *slog.Logger) (*PublishResult, error) {
	if !cfg.Publish.Enabled {
		return nil, nil
	}

	if ctx == nil {
		return nil, errors.New("publish: context is required")
	}

	if normalizeState(cfg.Tracker.Kind) != trackerKindGitHub {
		return nil, errors.New("publish: only github tracker publishing is supported")
	}

	publisher := &githubPublisher{
		cfg:    cfg,
		client: NewGitHubClient(cfg.Tracker),
		runGit: defaultGitCommandRunner,
		logger: loggerOrDefault(logger),
	}

	return publisher.Publish(ctx, issue, workspace)
}

// PublishFailureDraft creates or refreshes a draft pull request for a one-shot
// issue implementation run that could not complete before normal verification
// and publication.
func PublishFailureDraft(ctx context.Context, cfg Config, issue Issue, workspace Workspace, runErr error, logger *slog.Logger) (*PublishResult, error) {
	if !cfg.Publish.Enabled || !cfg.Publish.DraftOnFailedValidation {
		return nil, nil
	}

	if ctx == nil {
		return nil, errors.New("publish: context is required")
	}

	if normalizeState(cfg.Tracker.Kind) != trackerKindGitHub {
		return nil, errors.New("publish: only github tracker publishing is supported")
	}

	publisher := &githubPublisher{
		cfg:    cfg,
		client: NewGitHubClient(cfg.Tracker),
		runGit: defaultGitCommandRunner,
		logger: loggerOrDefault(logger),
	}

	return publisher.PublishFailureDraft(ctx, issue, workspace, runErr)
}

// UpdatePullRequestBranch refreshes a monitored PR branch from its base branch
// and pushes the rebased branch back to GitHub. Rebase conflicts are returned
// to the orchestrator so it can dispatch a PR rework worker on the same branch.
func UpdatePullRequestBranch(ctx context.Context, cfg Config, issue Issue, branch string, logger *slog.Logger) (string, error) {
	if ctx == nil {
		return "", errors.New("publish: context is required")
	}

	if normalizeState(cfg.Tracker.Kind) != trackerKindGitHub {
		return "", errors.New("publish: only github tracker publishing is supported")
	}

	branch = strings.TrimSpace(branch)
	if branch == "" {
		return "", errors.New("publish: pull request branch is required")
	}

	workspaces := NewWorkspaceManager(logger)
	workspace, err := workspaces.Ensure(ctx, cfg, issue)
	if err != nil {
		return "", err
	}

	if err := ensureGitWorkspaceForBranchUpdate(ctx, cfg, issue, workspace); err != nil {
		return "", err
	}

	publisher := &githubPublisher{
		cfg:    cfg,
		client: NewGitHubClient(cfg.Tracker),
		runGit: defaultGitCommandRunner,
		logger: loggerOrDefault(logger),
		audit:  symphonyIssueAudit("symphony.git", issue),
	}

	return publisher.updatePullRequestBranch(ctx, workspace.Path, branch)
}

// PreparePullRequestReworkWorkspace puts a PR rework worker on the PR branch
// and, when possible, leaves any base-branch rebase conflict in the workspace
// for Codex to resolve.
func PreparePullRequestReworkWorkspace(ctx context.Context, cfg Config, pullRequest *PullRequestReworkContext, workspace Workspace, logger *slog.Logger) error {
	if pullRequest == nil || strings.TrimSpace(pullRequest.Branch) == "" {
		return nil
	}

	if normalizeState(cfg.Tracker.Kind) != trackerKindGitHub {
		return nil
	}

	hasGit, err := workspaceHasGitCheckout(workspace)
	if err != nil {
		return err
	}
	if !hasGit {
		return fmt.Errorf("publish: workspace %s is not a git checkout", workspace.Path)
	}

	publisher := &githubPublisher{
		cfg:    cfg,
		client: NewGitHubClient(cfg.Tracker),
		runGit: defaultGitCommandRunner,
		logger: loggerOrDefault(logger),
	}

	return publisher.preparePullRequestReworkWorkspace(ctx, workspace.Path, pullRequest.Branch)
}

func ensureGitWorkspaceForBranchUpdate(ctx context.Context, cfg Config, issue Issue, workspace Workspace) error {
	hasGit, err := workspaceHasGitCheckout(workspace)
	if err == nil && hasGit {
		return nil
	}
	if err != nil {
		return err
	}

	if strings.TrimSpace(cfg.Hooks.BeforeRun) == "" {
		return fmt.Errorf("publish: workspace %s is not a git checkout", workspace.Path)
	}

	if hookErr := RunHook(ctx, cfg, issue, workspace, "before_run", cfg.Hooks.BeforeRun); hookErr != nil {
		return fmt.Errorf("publish: before_run hook failed before branch update: %w", hookErr)
	}

	hasGit, err = workspaceHasGitCheckout(workspace)
	if err != nil {
		return err
	}
	if !hasGit {
		return fmt.Errorf("publish: workspace %s is not a git checkout after before_run hook", workspace.Path)
	}

	return nil
}

func workspaceHasGitCheckout(workspace Workspace) (bool, error) {
	gitPath := filepath.Join(workspace.Path, ".git")
	_, err := os.Stat(gitPath)
	if err == nil {
		return true, nil
	}
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}

	return false, fmt.Errorf("publish: inspect git checkout %s: %w", gitPath, err)
}

//nolint:govet // Keep the heavyweight config first; this struct is not allocated in hot paths.
type githubPublisher struct {
	cfg             Config
	client          *GitHubClient
	runGit          gitCommandRunner
	runVerification func(context.Context, Config, Issue, Workspace) (VerificationReport, error)
	logger          *slog.Logger
	audit           shell.AuditContext
}

func (p *githubPublisher) Publish(ctx context.Context, issue Issue, workspace Workspace) (*PublishResult, error) {
	if strings.TrimSpace(workspace.Path) == "" {
		return nil, errors.New("publish: workspace path is required")
	}

	issueNumber, err := githubIssueNumber(issue)
	if err != nil {
		return nil, err
	}

	p.audit = symphonyIssueAudit("symphony.git", issue)

	branch := publishBranchName(p.cfg.Publish, issue)
	result := &PublishResult{Branch: branch}

	if checkoutErr := p.checkoutBranch(ctx, workspace.Path, branch); checkoutErr != nil {
		return nil, checkoutErr
	}

	hasChanges, err := p.hasChanges(ctx, workspace.Path)
	if err != nil {
		return nil, err
	}

	if hasChanges {
		changedFiles, filesErr := p.changedFiles(ctx, workspace.Path)
		if filesErr != nil {
			return nil, filesErr
		}
		result.ChangedFiles = changedFiles

		commitSHA, commitErr := p.commit(ctx, workspace.Path, issue)
		if commitErr != nil {
			return nil, commitErr
		}

		result.CommitSHA = commitSHA
	}

	hasCommits, err := p.hasCommitsAhead(ctx, workspace.Path)
	if err != nil {
		return nil, err
	}
	if hasCommits {
		if filesErr := p.refreshCommittedChangedFiles(ctx, workspace.Path, result); filesErr != nil {
			return nil, filesErr
		}
	}

	existingPR, err := p.client.FetchOpenPullRequestByHead(ctx, branch)
	if err != nil {
		return nil, err
	}

	if !hasCommits {
		result.ExistingPullRequest = existingPR != nil
		result.SkippedReason = "workspace has no changes to publish"
		return result, nil
	}

	verificationRunner := p.runVerification
	if verificationRunner == nil {
		verificationRunner = runVerificationGates
	}

	verificationReport, err := verificationRunner(ctx, p.cfg, issue, workspace)
	if err != nil {
		result.Verification = verificationReportPointer(verificationReport)
		return result, err
	}
	result.Verification = &verificationReport
	verificationReport, err = p.failVerificationIfWorkspaceDirty(ctx, workspace.Path, verificationReport)
	if err != nil {
		return result, err
	}
	result.Verification = &verificationReport
	if !verificationReport.Passed {
		if !p.cfg.Publish.DraftOnFailedValidation {
			return result, &VerificationGateError{Report: verificationReport}
		}
		result.DraftDueToFailedVerification = true
	}

	if hasCommits {
		if result.CommitSHA == "" {
			result.CommitSHA, err = p.currentCommit(ctx, workspace.Path)
			if err != nil {
				return result, err
			}
		}

		if remoteErr := p.setRemote(ctx, workspace.Path); remoteErr != nil {
			return result, remoteErr
		}

		if pushErr := p.push(ctx, workspace.Path, branch); pushErr != nil {
			return result, pushErr
		}
	}

	pr := existingPR
	if pr == nil {
		created, reusedExisting, createErr := p.createPullRequest(ctx, issue, branch, verificationReport, result.DraftDueToFailedVerification, result.ChangedFiles)
		if createErr != nil {
			result.ExistingPullRequest = reusedExisting
			attachPullRequestResult(result, created)
			return result, createErr
		}

		pr = &created
		result.ExistingPullRequest = reusedExisting
	} else {
		result.ExistingPullRequest = true
		attachPullRequestResult(result, *pr)
		if updateErr := p.updatePullRequestReport(ctx, *pr, issue, verificationReport, result.DraftDueToFailedVerification, result.ChangedFiles); updateErr != nil {
			return result, updateErr
		}
	}

	attachPullRequestResult(result, *pr)

	removed, finalizeErr := p.finalizeIssue(ctx, issueNumber, issue, *pr, result.DraftDueToFailedVerification)
	if finalizeErr != nil {
		return result, finalizeErr
	}

	result.RemovedLabels = removed
	return result, nil
}

func (p *githubPublisher) PublishFailureDraft(ctx context.Context, issue Issue, workspace Workspace, runErr error) (*PublishResult, error) {
	if strings.TrimSpace(workspace.Path) == "" {
		return nil, errors.New("publish: workspace path is required")
	}

	issueNumber, err := githubIssueNumber(issue)
	if err != nil {
		return nil, err
	}

	p.audit = symphonyIssueAudit("symphony.git", issue)

	branch := publishBranchName(p.cfg.Publish, issue)
	result := &PublishResult{
		Branch:               branch,
		DraftDueToRunFailure: true,
		FailureReason:        redactedRunFailure(runErr),
		Verification:         failureVerificationReport(runErr),
	}

	if checkoutErr := p.checkoutBranch(ctx, workspace.Path, branch); checkoutErr != nil {
		return result, checkoutErr
	}

	hasChanges, err := p.hasChanges(ctx, workspace.Path)
	if err != nil {
		return result, err
	}

	if hasChanges {
		changedFiles, filesErr := p.changedFiles(ctx, workspace.Path)
		if filesErr != nil {
			return result, filesErr
		}
		result.ChangedFiles = changedFiles

		commitSHA, commitErr := p.commitFailure(ctx, workspace.Path, issue, runErr, false)
		if commitErr != nil {
			return result, commitErr
		}
		result.CommitSHA = commitSHA
	}

	hasCommits, err := p.hasCommitsAhead(ctx, workspace.Path)
	if err != nil {
		return result, err
	}
	if hasCommits {
		if filesErr := p.refreshCommittedChangedFiles(ctx, workspace.Path, result); filesErr != nil {
			return result, filesErr
		}
	}

	existingPR, err := p.client.FetchOpenPullRequestByHead(ctx, branch)
	if err != nil {
		return result, err
	}

	emptyFailureCommit := false
	if !hasCommits && existingPR == nil {
		commitSHA, commitErr := p.commitFailure(ctx, workspace.Path, issue, runErr, true)
		if commitErr != nil {
			return result, commitErr
		}
		result.CommitSHA = commitSHA
		hasCommits = true
		emptyFailureCommit = true
	}

	if hasCommits {
		if result.CommitSHA == "" {
			result.CommitSHA, err = p.currentCommit(ctx, workspace.Path)
			if err != nil {
				return result, err
			}
		}

		if remoteErr := p.setRemote(ctx, workspace.Path); remoteErr != nil {
			return result, remoteErr
		}

		if pushErr := p.push(ctx, workspace.Path, branch); pushErr != nil {
			return result, pushErr
		}
	}

	pr := existingPR
	if pr == nil {
		created, reusedExisting, createErr := p.createFailurePullRequest(ctx, issue, branch, result.FailureReason, result.ChangedFiles, emptyFailureCommit)
		if createErr != nil {
			result.ExistingPullRequest = reusedExisting
			attachPullRequestResult(result, created)
			return result, createErr
		}

		pr = &created
		result.ExistingPullRequest = reusedExisting
	} else {
		result.ExistingPullRequest = true
		attachPullRequestResult(result, *pr)
		if updateErr := p.updateFailurePullRequestReport(ctx, *pr, issue, result.FailureReason, result.ChangedFiles, emptyFailureCommit); updateErr != nil {
			return result, updateErr
		}
	}

	attachPullRequestResult(result, *pr)

	removed, finalizeErr := p.finalizeFailureIssue(ctx, issueNumber, issue, *pr)
	if finalizeErr != nil {
		return result, finalizeErr
	}

	result.RemovedLabels = removed
	return result, nil
}

func (p *githubPublisher) checkoutBranch(ctx context.Context, dir, branch string) error {
	_, err := p.git(ctx, dir, nil, "checkout", "-B", branch)
	return err
}

func attachPullRequestResult(result *PublishResult, pr GitHubPullRequest) {
	if result == nil {
		return
	}

	if pr.Number > 0 {
		result.PullRequestNumber = pr.Number
	}

	if strings.TrimSpace(pr.HTMLURL) != "" {
		result.PullRequestURL = pr.HTMLURL
	}

	result.Published = result.PullRequestNumber > 0 || strings.TrimSpace(result.PullRequestURL) != ""
}

func (p *githubPublisher) hasChanges(ctx context.Context, dir string) (bool, error) {
	status, err := p.workspaceStatus(ctx, dir)
	if err != nil {
		return false, err
	}

	return status != "", nil
}

func (p *githubPublisher) workspaceStatus(ctx context.Context, dir string) (string, error) {
	output, err := p.git(ctx, dir, nil, "status", "--porcelain")
	if err != nil {
		return "", err
	}

	return strings.TrimSpace(string(output)), nil
}

func (p *githubPublisher) changedFiles(ctx context.Context, dir string) ([]string, error) {
	output, err := p.git(ctx, dir, nil, "status", "--porcelain")
	if err != nil {
		return nil, err
	}

	return parseGitStatusChangedFiles(string(output)), nil
}

func parseGitStatusChangedFiles(output string) []string {
	var files []string
	seen := make(map[string]struct{})

	for line := range strings.SplitSeq(output, "\n") {
		if strings.TrimSpace(line) == "" {
			continue
		}

		path := strings.TrimSpace(line)
		if len(line) > 3 {
			path = strings.TrimSpace(line[3:])
		}
		if strings.Contains(path, " -> ") {
			parts := strings.Split(path, " -> ")
			path = strings.TrimSpace(parts[len(parts)-1])
		}
		if path == "" {
			continue
		}

		if _, ok := seen[path]; ok {
			continue
		}
		seen[path] = struct{}{}
		files = append(files, path)
	}

	return files
}

func (p *githubPublisher) committedChangedFiles(ctx context.Context, dir string) ([]string, error) {
	base := strings.TrimSpace(p.cfg.Publish.BaseBranch)
	output, err := p.git(ctx, dir, nil, "diff", "--name-only", "--diff-filter=ACDMRT", base+"..HEAD")
	if err != nil {
		return nil, err
	}

	return parseGitNameOnlyFiles(string(output)), nil
}

func (p *githubPublisher) refreshCommittedChangedFiles(ctx context.Context, dir string, result *PublishResult) error {
	if result == nil {
		return nil
	}

	changedFiles, err := p.committedChangedFiles(ctx, dir)
	if err != nil {
		return err
	}
	if len(changedFiles) > 0 {
		result.ChangedFiles = changedFiles
	}

	return nil
}

func parseGitNameOnlyFiles(output string) []string {
	var files []string
	seen := make(map[string]struct{})

	for line := range strings.SplitSeq(output, "\n") {
		path := strings.TrimSpace(line)
		if path == "" {
			continue
		}

		if _, ok := seen[path]; ok {
			continue
		}
		seen[path] = struct{}{}
		files = append(files, path)
	}

	return files
}

func (p *githubPublisher) hasCommitsAhead(ctx context.Context, dir string) (bool, error) {
	base := strings.TrimSpace(p.cfg.Publish.BaseBranch)
	output, err := p.git(ctx, dir, nil, "rev-list", "--count", base+"..HEAD")
	if err != nil {
		return false, err
	}

	count, err := strconv.Atoi(strings.TrimSpace(string(output)))
	if err != nil {
		return false, fmt.Errorf("publish: parse commits ahead of %s: %w", base, err)
	}

	return count > 0, nil
}

func (p *githubPublisher) commit(ctx context.Context, dir string, issue Issue) (string, error) {
	return p.commitWithMessage(ctx, dir, publishCommitMessage(issue), false)
}

func (p *githubPublisher) commitFailure(ctx context.Context, dir string, issue Issue, runErr error, allowEmpty bool) (string, error) {
	return p.commitWithMessage(ctx, dir, publishFailureCommitMessage(issue, runErr), allowEmpty)
}

func (p *githubPublisher) commitWithMessage(ctx context.Context, dir, message string, allowEmpty bool) (string, error) {
	if _, configNameErr := p.git(ctx, dir, nil, "config", "user.name", p.cfg.Publish.GitUserName); configNameErr != nil {
		return "", configNameErr
	}

	if _, configEmailErr := p.git(ctx, dir, nil, "config", "user.email", p.cfg.Publish.GitUserEmail); configEmailErr != nil {
		return "", configEmailErr
	}

	if _, addErr := p.git(ctx, dir, nil, "add", "-A"); addErr != nil {
		return "", addErr
	}

	messageFile, err := os.CreateTemp("", "symphony-commit-*.txt")
	if err != nil {
		return "", fmt.Errorf("publish: create commit message file: %w", err)
	}
	messagePath := messageFile.Name()
	defer func() {
		_ = os.Remove(messagePath)
	}()

	if _, writeErr := messageFile.WriteString(message); writeErr != nil {
		_ = messageFile.Close()
		return "", fmt.Errorf("publish: write commit message: %w", writeErr)
	}

	if closeErr := messageFile.Close(); closeErr != nil {
		return "", fmt.Errorf("publish: close commit message: %w", closeErr)
	}

	args := []string{"commit"}
	if allowEmpty {
		args = append(args, "--allow-empty")
	}
	args = append(args, "-F", messagePath)

	if _, commitErr := p.git(ctx, dir, nil, args...); commitErr != nil {
		return "", commitErr
	}

	return p.currentCommit(ctx, dir)
}

func (p *githubPublisher) currentCommit(ctx context.Context, dir string) (string, error) {
	output, err := p.git(ctx, dir, nil, "rev-parse", "HEAD")
	if err != nil {
		return "", err
	}

	return strings.TrimSpace(string(output)), nil
}

func (p *githubPublisher) failVerificationIfWorkspaceDirty(ctx context.Context, dir string, report VerificationReport) (VerificationReport, error) {
	if !report.Configured {
		return report, nil
	}

	status, err := p.workspaceStatus(ctx, dir)
	if err != nil {
		return report, err
	}

	if strings.TrimSpace(status) == "" {
		return report, nil
	}

	now := time.Now().UTC()
	report.Configured = true
	report.Passed = false
	report.CompletedAt = now
	report.FailedRequired = appendMissingVerificationName(report.FailedRequired, "workspace_clean")
	report.Gates = append(report.Gates, VerificationGateResult{
		StartedAt:   now,
		CompletedAt: now,
		Name:        "workspace_clean",
		Command:     "git status --porcelain",
		Status:      VerificationFailed,
		Stdout:      truncateOneLine(privacy.RedactText(status)),
		Error:       "workspace has uncommitted changes after verification gates; commit or revert generated files and rerun verification",
		Required:    true,
	})

	return report, nil
}

func verificationReportPointer(report VerificationReport) *VerificationReport {
	if !report.Configured && len(report.Gates) == 0 && len(report.FailedRequired) == 0 && report.StartedAt.IsZero() && report.CompletedAt.IsZero() {
		return nil
	}

	return &report
}

func appendMissingVerificationName(names []string, name string) []string {
	name = strings.TrimSpace(name)
	if name == "" {
		return names
	}

	for _, existing := range names {
		if strings.EqualFold(strings.TrimSpace(existing), name) {
			return names
		}
	}

	return append(names, name)
}

func (p *githubPublisher) setRemote(ctx context.Context, dir string) error {
	remote := strings.TrimSpace(p.cfg.Publish.Remote)
	remoteURL := publishRemoteURL(p.cfg)
	if _, getErr := p.git(ctx, dir, nil, "remote", "get-url", remote); getErr != nil {
		_, addErr := p.git(ctx, dir, nil, "remote", "add", remote, remoteURL)
		return addErr
	}

	_, err := p.git(ctx, dir, nil, "remote", "set-url", remote, remoteURL)
	return err
}

// errPullRequestBranchUpdateSkipped marks a branch update that was deliberately
// not performed to protect local work in the workspace. The orchestrator
// reschedules the next check instead of dispatching rework for it.
var errPullRequestBranchUpdateSkipped = errors.New("publish: pull request branch update skipped")

func (p *githubPublisher) updatePullRequestBranch(ctx context.Context, dir, branch string) (string, error) {
	branch = strings.TrimSpace(branch)
	base := strings.TrimSpace(p.cfg.Publish.BaseBranch)
	remote := strings.TrimSpace(p.cfg.Publish.Remote)
	if branch == "" {
		return "", errors.New("publish: pull request branch is required")
	}
	if base == "" {
		return "", errors.New("publish: base branch is required")
	}
	if remote == "" {
		return "", errors.New("publish: git remote is required")
	}

	if err := p.setRemote(ctx, dir); err != nil {
		return "", err
	}

	baseRemote := remote + "/" + base
	branchRemote := remote + "/" + branch
	if err := p.fetchBranchUpdateRefs(ctx, dir, remote, base, branch); err != nil {
		return "", err
	}

	// Both safety checks must run before `checkout -B`, which force-resets the
	// local branch to the remote ref and would silently discard local work.
	dirty, err := p.hasChanges(ctx, dir)
	if err != nil {
		return "", err
	}
	if dirty {
		return "", errors.New("publish: workspace has uncommitted changes before branch update")
	}

	unpushed, err := p.hasUnpushedCommits(ctx, dir, branch, branchRemote)
	if err != nil {
		return "", err
	}
	if unpushed {
		p.logger.Warn(
			"symphony pull request branch update skipped; local branch has commits not pushed to the remote",
			"branch", branch,
			"remote_ref", branchRemote,
		)
		return "", fmt.Errorf("branch %s has local commits not on %s: %w", branch, branchRemote, errPullRequestBranchUpdateSkipped)
	}

	if _, checkoutErr := p.git(ctx, dir, nil, "checkout", "-B", branch, branchRemote); checkoutErr != nil {
		return "", fmt.Errorf("publish: checkout pull request branch %s: %w", branch, checkoutErr)
	}

	if _, rebaseErr := p.git(ctx, dir, nil, "rebase", baseRemote); rebaseErr != nil {
		// Best-effort cleanup: when rebase exits with a merge conflict ctx is
		// still alive and the abort succeeds. If ctx was already canceled the
		// abort will fail too, but the workspace is being torn down anyway.
		if _, abortErr := p.git(ctx, dir, nil, "rebase", "--abort"); abortErr != nil {
			return "", fmt.Errorf("publish: rebase %s onto %s failed and abort failed: %w; abort: %w", branch, baseRemote, rebaseErr, abortErr)
		}
		return "", fmt.Errorf("publish: rebase %s onto %s: %w", branch, baseRemote, rebaseErr)
	}

	commitSHA, err := p.currentCommit(ctx, dir)
	if err != nil {
		return "", err
	}

	if err := p.pushForceWithLease(ctx, dir, branch); err != nil {
		return "", err
	}

	return commitSHA, nil
}

// hasUnpushedCommits reports whether the local branch carries commits that are
// not on its remote counterpart, e.g. a worker committed but failed to push.
func (p *githubPublisher) hasUnpushedCommits(ctx context.Context, dir, branch, branchRemote string) (bool, error) {
	if _, err := p.git(ctx, dir, nil, "rev-parse", "--verify", "--quiet", "refs/heads/"+branch); err != nil {
		// rev-parse --verify exits non-zero when the local branch does not
		// exist yet, in which case checkout -B cannot discard local commits.
		return false, nil //nolint:nilerr // a missing local branch means there is nothing to protect
	}

	output, err := p.git(ctx, dir, nil, "rev-list", branchRemote+".."+branch)
	if err != nil {
		return false, err
	}

	return strings.TrimSpace(string(output)) != "", nil
}

func (p *githubPublisher) preparePullRequestReworkWorkspace(ctx context.Context, dir, branch string) error {
	branch = strings.TrimSpace(branch)
	base := strings.TrimSpace(p.cfg.Publish.BaseBranch)
	remote := strings.TrimSpace(p.cfg.Publish.Remote)
	if branch == "" || base == "" || remote == "" {
		return nil
	}

	inRebase, err := p.rebaseInProgress(ctx, dir)
	if err != nil {
		return err
	}
	if inRebase {
		p.logger.Info("symphony PR rework workspace already has a rebase in progress", "branch", branch)
		return nil
	}

	dirty, err := p.hasChanges(ctx, dir)
	if err != nil {
		return err
	}
	if dirty {
		p.logger.Info("symphony PR rework workspace has local changes; preserving them for rework", "branch", branch)
		return nil
	}

	if err := p.setRemote(ctx, dir); err != nil {
		return err
	}

	baseRemote := remote + "/" + base
	branchRemote := remote + "/" + branch
	if err := p.fetchBranchUpdateRefs(ctx, dir, remote, base, branch); err != nil {
		return err
	}

	if _, err := p.git(ctx, dir, nil, "checkout", "-B", branch, branchRemote); err != nil {
		return fmt.Errorf("publish: checkout pull request branch %s for rework: %w", branch, err)
	}

	if _, err := p.git(ctx, dir, nil, "rebase", baseRemote); err != nil {
		p.logger.Warn(
			"symphony PR rework workspace rebase needs conflict resolution",
			"branch", branch,
			"base", baseRemote,
			"error", err,
		)
		return nil
	}

	return nil
}

func (p *githubPublisher) rebaseInProgress(ctx context.Context, dir string) (bool, error) {
	for _, name := range []string{"rebase-merge", "rebase-apply"} {
		output, err := p.git(ctx, dir, nil, "rev-parse", "--git-path", name)
		if err != nil {
			return false, err
		}

		path := strings.TrimSpace(string(output))
		if path == "" {
			continue
		}
		if !filepath.IsAbs(path) {
			path = filepath.Join(dir, path)
		}

		if _, statErr := os.Stat(path); statErr == nil {
			return true, nil
		} else if !errors.Is(statErr, os.ErrNotExist) {
			return false, fmt.Errorf("publish: inspect rebase state %s: %w", path, statErr)
		}
	}

	return false, nil
}

func (p *githubPublisher) fetchBranchUpdateRefs(ctx context.Context, dir, remote, base, branch string) error {
	baseSpec := "+refs/heads/" + base + ":refs/remotes/" + remote + "/" + base
	branchSpec := "+refs/heads/" + branch + ":refs/remotes/" + remote + "/" + branch
	return p.gitWithAuth(ctx, dir, "fetch", remote, baseSpec, branchSpec)
}

func (p *githubPublisher) push(ctx context.Context, dir, branch string) error {
	return p.gitWithAuth(ctx, dir, "push", "-u", p.cfg.Publish.Remote, branch)
}

func (p *githubPublisher) pushForceWithLease(ctx context.Context, dir, branch string) error {
	return p.gitWithAuth(ctx, dir, "push", "--force-with-lease", p.cfg.Publish.Remote, branch)
}

func (p *githubPublisher) gitWithAuth(ctx context.Context, dir string, args ...string) error {
	askPassDir, err := os.MkdirTemp("", "symphony-git-askpass-*")
	if err != nil {
		return fmt.Errorf("publish: create git askpass directory: %w", err)
	}
	defer func() {
		_ = os.RemoveAll(askPassDir)
	}()

	askPassPath := askPassDir + "/askpass.sh"
	writeErr := os.WriteFile(askPassPath, []byte(`#!/bin/sh
case "$1" in
  *Username*) printf '%s\n' 'x-access-token' ;;
  *Password*) printf '%s\n' "$GITHUB_TOKEN" ;;
  *) printf '\n' ;;
esac
`), 0o700)
	if writeErr != nil {
		return fmt.Errorf("publish: write git askpass script: %w", writeErr)
	}

	env := []string{
		"GIT_ASKPASS=" + askPassPath,
		"GIT_TERMINAL_PROMPT=0",
		"GITHUB_TOKEN=" + p.cfg.Tracker.APIKey,
	}
	_, err = p.git(ctx, dir, env, args...)
	return err
}

func (p *githubPublisher) createPullRequest(ctx context.Context, issue Issue, branch string, report VerificationReport, draftDueToFailedValidation bool, changedFiles []string) (GitHubPullRequest, bool, error) {
	shouldBeDraft := p.cfg.Publish.Draft || draftDueToFailedValidation
	pr, err := p.client.CreatePullRequest(
		ctx,
		branch,
		p.cfg.Publish.BaseBranch,
		publishPRTitle(issue),
		publishPRBody(issue, report, draftDueToFailedValidation, changedFiles),
		shouldBeDraft,
	)
	if err == nil {
		if draftErr := p.ensurePullRequestDraftState(ctx, pr, shouldBeDraft); draftErr != nil {
			return pr, false, draftErr
		}

		return pr, false, nil
	}

	var statusErr *githubAPIStatusError
	if !errors.As(err, &statusErr) || statusErr.StatusCode != httpStatusValidationFailed {
		return GitHubPullRequest{}, false, err
	}

	existing, findErr := p.client.FetchOpenPullRequestByHead(ctx, branch)
	if findErr != nil {
		return GitHubPullRequest{}, false, findErr
	}

	if existing == nil {
		return GitHubPullRequest{}, false, err
	}

	updated, updateErr := p.client.UpdatePullRequest(ctx, existing.Number, publishPRTitle(issue), publishPRBody(issue, report, draftDueToFailedValidation, changedFiles))
	if updateErr != nil {
		return GitHubPullRequest{}, false, updateErr
	}
	updated = mergePullRequestIdentity(updated, *existing)
	if draftErr := p.ensurePullRequestDraftState(ctx, updated, shouldBeDraft); draftErr != nil {
		return updated, true, draftErr
	}

	return updated, true, nil
}

func (p *githubPublisher) updatePullRequestReport(ctx context.Context, pr GitHubPullRequest, issue Issue, report VerificationReport, draftDueToFailedValidation bool, changedFiles []string) error {
	if pr.Number <= 0 {
		return nil
	}

	updated, err := p.client.UpdatePullRequest(ctx, pr.Number, publishPRTitle(issue), publishPRBody(issue, report, draftDueToFailedValidation, changedFiles))
	if err != nil {
		return err
	}

	return p.ensurePullRequestDraftState(ctx, mergePullRequestIdentity(updated, pr), p.cfg.Publish.Draft || draftDueToFailedValidation)
}

func (p *githubPublisher) createFailurePullRequest(ctx context.Context, issue Issue, branch, failureReason string, changedFiles []string, emptyFailureCommit bool) (GitHubPullRequest, bool, error) {
	pr, err := p.client.CreatePullRequest(
		ctx,
		branch,
		p.cfg.Publish.BaseBranch,
		publishPRTitle(issue),
		publishFailurePRBody(issue, failureReason, changedFiles, emptyFailureCommit),
		true,
	)
	if err == nil {
		if draftErr := p.ensurePullRequestDraftState(ctx, pr, true); draftErr != nil {
			return pr, false, draftErr
		}

		return pr, false, nil
	}

	var statusErr *githubAPIStatusError
	if !errors.As(err, &statusErr) || statusErr.StatusCode != httpStatusValidationFailed {
		return GitHubPullRequest{}, false, err
	}

	existing, findErr := p.client.FetchOpenPullRequestByHead(ctx, branch)
	if findErr != nil {
		return GitHubPullRequest{}, false, findErr
	}

	if existing == nil {
		return GitHubPullRequest{}, false, err
	}

	updated, updateErr := p.client.UpdatePullRequest(ctx, existing.Number, publishPRTitle(issue), publishFailurePRBody(issue, failureReason, changedFiles, emptyFailureCommit))
	if updateErr != nil {
		return GitHubPullRequest{}, false, updateErr
	}
	updated = mergePullRequestIdentity(updated, *existing)
	if draftErr := p.ensurePullRequestDraftState(ctx, updated, true); draftErr != nil {
		return updated, true, draftErr
	}

	return updated, true, nil
}

func (p *githubPublisher) updateFailurePullRequestReport(ctx context.Context, pr GitHubPullRequest, issue Issue, failureReason string, changedFiles []string, emptyFailureCommit bool) error {
	if pr.Number <= 0 {
		return nil
	}

	updated, err := p.client.UpdatePullRequest(ctx, pr.Number, publishPRTitle(issue), publishFailurePRBody(issue, failureReason, changedFiles, emptyFailureCommit))
	if err != nil {
		return err
	}

	return p.ensurePullRequestDraftState(ctx, mergePullRequestIdentity(updated, pr), true)
}

func (p *githubPublisher) ensurePullRequestDraftState(ctx context.Context, pr GitHubPullRequest, shouldBeDraft bool) error {
	if pr.Draft == shouldBeDraft {
		return nil
	}

	nodeID := strings.TrimSpace(pr.NodeID)
	if nodeID == "" {
		action := "convert"
		if !shouldBeDraft {
			action = "mark ready for review"
		}

		return fmt.Errorf("publish: %s pull request #%d: GitHub node_id is missing", action, pr.Number)
	}

	if shouldBeDraft {
		if err := p.client.ConvertPullRequestToDraft(ctx, nodeID); err != nil {
			return fmt.Errorf("publish: convert pull request #%d to draft: %w", pr.Number, err)
		}

		return nil
	}

	if err := p.client.MarkPullRequestReadyForReview(ctx, nodeID); err != nil {
		return fmt.Errorf("publish: mark pull request #%d ready for review: %w", pr.Number, err)
	}

	return nil
}

func mergePullRequestIdentity(updated, existing GitHubPullRequest) GitHubPullRequest {
	if updated.Number == 0 {
		updated.Number = existing.Number
	}
	if strings.TrimSpace(updated.HTMLURL) == "" {
		updated.HTMLURL = existing.HTMLURL
	}
	if strings.TrimSpace(updated.NodeID) == "" {
		updated.NodeID = existing.NodeID
	}
	if updated.Head.Ref == "" {
		updated.Head = existing.Head
	}
	if updated.Base.Ref == "" {
		updated.Base = existing.Base
	}
	if !updated.DraftKnown && existing.DraftKnown {
		updated.Draft = existing.Draft
		updated.DraftKnown = true
	}

	return updated
}

func (p *githubPublisher) finalizeIssue(ctx context.Context, issueNumber int, issue Issue, pr GitHubPullRequest, draftDueToFailedValidation bool) ([]string, error) {
	if draftDueToFailedValidation {
		return p.finalizeIssueWithComment(ctx, issueNumber, issue, pr, publishValidationFailureIssueComment)
	}

	return p.finalizeIssueWithComment(ctx, issueNumber, issue, pr, publishIssueComment)
}

func (p *githubPublisher) finalizeFailureIssue(ctx context.Context, issueNumber int, issue Issue, pr GitHubPullRequest) ([]string, error) {
	return p.finalizeIssueWithComment(ctx, issueNumber, issue, pr, publishFailureIssueComment)
}

func (p *githubPublisher) finalizeIssueWithComment(ctx context.Context, issueNumber int, issue Issue, pr GitHubPullRequest, commentFor func(GitHubPullRequest, []string) string) ([]string, error) {
	removed := trimNonEmptyStrings(p.cfg.Publish.RemoveLabels)
	for _, label := range removed {
		if labelErr := p.client.RemoveIssueLabel(ctx, issueNumber, label); labelErr != nil {
			return nil, fmt.Errorf("publish: remove issue label %q: %w", label, labelErr)
		}
	}

	comment := commentFor(pr, removed)
	if commentErr := p.client.AddIssueComment(ctx, issueNumber, comment); commentErr != nil {
		p.logger.Warn(
			"symphony issue comment failed after publish",
			"issue_id", issue.ID,
			"issue_identifier", issue.Identifier,
			"pull_request_url", pr.HTMLURL,
			"error", commentErr,
		)
	}

	return removed, nil
}

func (p *githubPublisher) git(ctx context.Context, dir string, env []string, args ...string) ([]byte, error) {
	if p.runGit == nil {
		p.runGit = defaultGitCommandRunner
	}

	audit := p.audit
	if strings.TrimSpace(audit.Caller) == "" {
		audit.Caller = "symphony.git"
	}

	return p.runGit(ctx, dir, env, audit, args...)
}

func defaultGitCommandRunner(ctx context.Context, dir string, env []string, audit shell.AuditContext, args ...string) ([]byte, error) {
	var output bytes.Buffer
	if strings.TrimSpace(audit.Caller) == "" {
		audit.Caller = "symphony.git"
	}

	cmd, invocation, err := shell.CommandContext(ctx, shell.CommandOptions{
		Program: "git",
		Args:    args,
		Dir:     dir,
		EnvList: env,
		Stdout:  &output,
		Stderr:  &output,
		Mode:    shell.ModeCaptured,
		Policy: &shell.Policy{
			AllowCredentialEnv: envNames(env),
		},
		Audit: audit,
	})
	if err != nil {
		return nil, fmt.Errorf("git %s authorize: %w", strings.Join(args, " "), err)
	}

	runErr := cmd.Run()
	if finishErr := invocation.Finish(shell.FinishOptions{
		Stdout:        output.String(),
		Error:         runErr,
		OutputCapture: shell.OutputCaptured,
	}); finishErr != nil {
		return output.Bytes(), fmt.Errorf("git %s audit: %w", strings.Join(args, " "), finishErr)
	}
	if runErr == nil {
		return output.Bytes(), nil
	}

	message := strings.TrimSpace(output.String())
	if message == "" {
		message = runErr.Error()
	}

	return output.Bytes(), fmt.Errorf("git %s: %s: %w", strings.Join(args, " "), message, runErr)
}

func envNames(env []string) []string {
	names := make([]string, 0, len(env))
	for _, pair := range env {
		name, _, ok := strings.Cut(pair, "=")
		if ok && strings.TrimSpace(name) != "" {
			names = append(names, name)
		}
	}

	return names
}

func symphonyIssueAudit(caller string, issue Issue) shell.AuditContext {
	return shell.AuditContext{
		Caller:          caller,
		IssueID:         issue.ID,
		IssueIdentifier: issue.Identifier,
	}
}

func publishBranchName(cfg PublishConfig, issue Issue) string {
	key := SanitizeWorkspaceKey(issue.Identifier)
	prefix := strings.Trim(strings.TrimSpace(cfg.BranchPrefix), "/")
	if prefix == "" {
		return key
	}

	return prefix + "/" + key
}

func publishRemoteURL(cfg Config) string {
	if strings.TrimSpace(cfg.Publish.RemoteURL) != "" {
		return strings.TrimSpace(cfg.Publish.RemoteURL)
	}

	apiURL, err := url.Parse(strings.TrimSpace(cfg.Tracker.Endpoint))
	if err == nil && apiURL.Scheme != "" && apiURL.Host != "" && apiURL.Host != "api.github.com" {
		basePath := strings.TrimSuffix(strings.TrimRight(apiURL.Path, "/"), "/api/v3")
		basePath = strings.TrimSuffix(strings.TrimRight(basePath, "/"), "/api")
		return fmt.Sprintf("%s://%s%s/%s/%s.git", apiURL.Scheme, apiURL.Host, basePath, cfg.Tracker.Owner, cfg.Tracker.Repo)
	}

	return fmt.Sprintf("https://github.com/%s/%s.git", cfg.Tracker.Owner, cfg.Tracker.Repo)
}

func githubIssueNumber(issue Issue) (int, error) {
	identifier := strings.TrimSpace(issue.Identifier)
	if value, ok := strings.CutPrefix(strings.ToUpper(identifier), "GH-"); ok {
		number, err := strconv.Atoi(value)
		if err == nil && number > 0 {
			return number, nil
		}
	}

	if issue.URL != nil {
		parts := strings.Split(strings.TrimRight(*issue.URL, "/"), "/")
		if len(parts) > 0 {
			number, err := strconv.Atoi(parts[len(parts)-1])
			if err == nil && number > 0 {
				return number, nil
			}
		}
	}

	return 0, fmt.Errorf("publish: cannot determine GitHub issue number from %q", issue.Identifier)
}

func publishCommitMessage(issue Issue) string {
	var body bytes.Buffer
	issueIdentifier := redactedIssueIdentifier(issue)
	fmt.Fprintf(&body, "chore: publish %s Symphony workspace\n\n", issueIdentifier)
	fmt.Fprintf(&body, "Symphony completed a worker run for %s: %s.\n\n", issueIdentifier, redactedIssueTitle(issue))
	fmt.Fprintln(&body, "Constraint: Publication is automated from the issue workspace")
	fmt.Fprintln(&body, "Confidence: medium")
	fmt.Fprintln(&body, "Scope-risk: moderate")
	fmt.Fprintln(&body, "Tested: See the Symphony pull request body and worker output")
	fmt.Fprintln(&body, "Not-tested: Human review pending")
	fmt.Fprintf(&body, "Related: %s\n", issueIdentifier)
	return body.String()
}

func publishFailureCommitMessage(issue Issue, runErr error) string {
	var body bytes.Buffer
	issueIdentifier := redactedIssueIdentifier(issue)
	fmt.Fprintf(&body, "chore: publish %s incomplete Symphony draft\n\n", issueIdentifier)
	fmt.Fprintf(&body, "Symphony could not complete the implementation for %s: %s.\n", issueIdentifier, redactedIssueTitle(issue))
	if reason := redactedRunFailure(runErr); reason != "" {
		fmt.Fprintf(&body, "Failure: %s.\n", oneLine(reason))
	}
	fmt.Fprintln(&body)
	fmt.Fprintln(&body, "Constraint: Publication is automated from an incomplete issue workspace")
	fmt.Fprintln(&body, "Confidence: medium")
	fmt.Fprintln(&body, "Scope-risk: moderate")
	fmt.Fprintln(&body, "Tested: Implementation did not reach normal verification gates")
	fmt.Fprintln(&body, "Not-tested: Implementation incomplete; human review pending")
	fmt.Fprintf(&body, "Related: %s\n", issueIdentifier)
	return body.String()
}

func publishPRTitle(issue Issue) string {
	return strings.TrimSpace(redactedIssueIdentifier(issue) + ": " + redactedIssueTitle(issue))
}

func publishPRBody(issue Issue, report VerificationReport, draftDueToFailedValidation bool, changedFiles []string) string {
	var body bytes.Buffer
	issueIdentifier := redactedIssueIdentifier(issue)
	fmt.Fprintf(&body, "Automated Symphony publication for %s.\n\n", issueIdentifier)

	fmt.Fprintln(&body, "## What changed")
	fmt.Fprintf(&body, "- Prepared worker changes for %s: %s.\n", issueIdentifier, redactedIssueTitle(issue))
	fmt.Fprintln(&body, "- Committed the worker workspace to a deterministic Symphony branch.")
	for _, file := range changedFiles {
		fmt.Fprintf(&body, "- Changed file: `%s`\n", privacy.RedactIdentifier(file))
	}
	fmt.Fprintln(&body)

	fmt.Fprintln(&body, "## Validation")
	body.WriteString(formatVerificationReport(report))
	fmt.Fprintln(&body)

	fmt.Fprintln(&body, "## Risk")
	fmt.Fprintf(&body, "- %s\n", publishRiskAssessment(issue, report))
	if draftDueToFailedValidation {
		fmt.Fprintln(&body, "- This PR is not ready because at least one required local verification gate failed; keep it draft until resolved.")
	}
	fmt.Fprintln(&body)

	fmt.Fprintln(&body, "## Reviewer notes")
	fmt.Fprintln(&body, "- Review the worker commit, CI output, and the verification evidence above.")
	fmt.Fprintln(&body, "- Confirm generated claims against the diff before marking this PR ready.")
	if focus := publishReviewerFocus(issue); focus != "" {
		fmt.Fprintf(&body, "- Suggested reviewer focus: %s.\n", focus)
	}
	if reviewers := publishSuggestedReviewers(issue, report); reviewers != "" {
		fmt.Fprintf(&body, "- Suggested reviewers: %s.\n", reviewers)
	}
	if len(issue.Labels) > 0 {
		fmt.Fprintf(&body, "- Issue labels for reviewer routing: %s.\n", strings.Join(redactedIssueLabels(issue.Labels), ", "))
	}
	fmt.Fprintln(&body)

	fmt.Fprintln(&body, "## Linked issue")
	if issue.URL != nil && strings.TrimSpace(*issue.URL) != "" {
		fmt.Fprintf(&body, "- Issue: %s\n", privacy.RedactIdentifier(strings.TrimSpace(*issue.URL)))
	}
	if number, err := githubIssueNumber(issue); err == nil {
		if draftDueToFailedValidation || len(report.FailedRequired) > 0 {
			fmt.Fprintf(&body, "- Related to #%d\n", number)
		} else {
			fmt.Fprintf(&body, "- Closes #%d\n", number)
		}
	}

	return body.String()
}

func publishFailurePRBody(issue Issue, failureReason string, changedFiles []string, emptyFailureCommit bool) string {
	var body bytes.Buffer
	issueIdentifier := redactedIssueIdentifier(issue)
	failureReason = privacy.RedactText(strings.TrimSpace(failureReason))

	fmt.Fprintf(&body, "Automated Symphony draft for %s could not complete.\n\n", issueIdentifier)

	fmt.Fprintln(&body, "## What changed")
	fmt.Fprintf(&body, "- Symphony attempted to implement %s: %s.\n", issueIdentifier, redactedIssueTitle(issue))
	switch {
	case len(changedFiles) == 0 && emptyFailureCommit:
		fmt.Fprintln(&body, "- No changed files were captured before the incomplete run ended; an empty draft commit was published so the run is reviewable.")
	case len(changedFiles) == 0:
		fmt.Fprintln(&body, "- No changed files were captured before the incomplete run ended.")
	default:
		fmt.Fprintln(&body, "- Partial implementation changes were committed for reviewer inspection.")
		for _, file := range changedFiles {
			fmt.Fprintf(&body, "- Changed file: `%s`\n", privacy.RedactIdentifier(file))
		}
	}
	fmt.Fprintln(&body)

	fmt.Fprintln(&body, "## Validation")
	fmt.Fprintln(&body, "- FAIL `worker_run` (required): issue implementation did not complete.")
	if failureReason != "" {
		fmt.Fprintf(&body, "  - error: %s\n", oneLine(failureReason))
	}
	fmt.Fprintln(&body, "- Local verification gates were not run because the implementation did not reach the normal publish gate.")
	fmt.Fprintln(&body, "- Required verification gate(s) failed: worker_run.")
	fmt.Fprintln(&body)

	fmt.Fprintln(&body, "## Risk")
	fmt.Fprintln(&body, "- High: implementation is incomplete; do not merge until the incomplete run is resolved and verification gates pass.")
	fmt.Fprintln(&body, "- This PR is a draft because the autonomous implementation did not fully complete the issue.")
	fmt.Fprintln(&body)

	fmt.Fprintln(&body, "## Reviewer notes")
	fmt.Fprintln(&body, "- Review the failure evidence above and any partial diff before deciding whether to continue manually or rerun Symphony.")
	fmt.Fprintln(&body, "- Confirm no generated claim here is treated as proof of implementation completion.")
	if focus := publishReviewerFocus(issue); focus != "" {
		fmt.Fprintf(&body, "- Suggested reviewer focus: %s.\n", focus)
	}
	if reviewers := publishSuggestedReviewers(issue, VerificationReport{FailedRequired: []string{"worker_run"}}); reviewers != "" {
		fmt.Fprintf(&body, "- Suggested reviewers: %s.\n", reviewers)
	}
	if len(issue.Labels) > 0 {
		fmt.Fprintf(&body, "- Issue labels for reviewer routing: %s.\n", strings.Join(redactedIssueLabels(issue.Labels), ", "))
	}
	fmt.Fprintln(&body)

	fmt.Fprintln(&body, "## Linked issue")
	if issue.URL != nil && strings.TrimSpace(*issue.URL) != "" {
		fmt.Fprintf(&body, "- Issue: %s\n", privacy.RedactIdentifier(strings.TrimSpace(*issue.URL)))
	}
	if number, err := githubIssueNumber(issue); err == nil {
		fmt.Fprintf(&body, "- Related to #%d\n", number)
	}

	return body.String()
}

func redactedIssueIdentifier(issue Issue) string {
	return privacy.RedactIdentifier(strings.TrimSpace(issue.Identifier))
}

func redactedIssueTitle(issue Issue) string {
	return privacy.RedactText(strings.TrimSpace(issue.Title))
}

func redactedIssueLabels(labels []string) []string {
	out := make([]string, 0, len(labels))
	for _, label := range labels {
		label = strings.TrimSpace(privacy.RedactText(label))
		if label != "" {
			out = append(out, label)
		}
	}

	return out
}

func formatVerificationReport(report VerificationReport) string {
	var body strings.Builder
	if !report.Configured {
		body.WriteString("- No local verification gates configured; review worker output and CI before merging.\n")
		return body.String()
	}

	for i := range report.Gates {
		gate := report.Gates[i]
		status := "FAIL"
		if gate.Status == VerificationPassed {
			status = "PASS"
		}
		required := "optional"
		if gate.Required {
			required = "required"
		}

		fmt.Fprintf(&body, "- %s `%s` (%s): `%s`", status, privacy.RedactText(gate.Name), required, privacy.RedactText(gate.Command))
		if gate.Duration > 0 {
			fmt.Fprintf(&body, " in %s", gate.Duration.Round(time.Millisecond))
		}
		body.WriteByte('\n')

		if gate.Error != "" {
			fmt.Fprintf(&body, "  - error: %s\n", oneLine(privacy.RedactText(gate.Error)))
		}
		if gate.Stdout != "" {
			fmt.Fprintf(&body, "  - stdout: %s\n", truncateOneLine(privacy.RedactText(gate.Stdout)))
		}
		if gate.Stderr != "" {
			fmt.Fprintf(&body, "  - stderr: %s\n", truncateOneLine(privacy.RedactText(gate.Stderr)))
		}
		if gate.OutputTruncated {
			body.WriteString("  - output was truncated by the configured capture limit\n")
		}
	}

	if len(report.FailedRequired) > 0 {
		fmt.Fprintf(&body, "- Required verification gate(s) failed: %s.\n", strings.Join(redactVerificationNames(report.FailedRequired), ", "))
	} else {
		body.WriteString("- All required local verification gates passed.\n")
	}
	if failedOptional := failedOptionalGateNames(report); len(failedOptional) > 0 {
		fmt.Fprintf(&body, "- Optional verification gate(s) failed: %s.\n", strings.Join(failedOptional, ", "))
	}

	return body.String()
}

func failureVerificationReport(runErr error) *VerificationReport {
	now := time.Now().UTC()
	reason := redactedRunFailure(runErr)

	return &VerificationReport{
		StartedAt:      now,
		CompletedAt:    now,
		Configured:     true,
		Passed:         false,
		FailedRequired: []string{"worker_run"},
		Gates: []VerificationGateResult{{
			StartedAt:   now,
			CompletedAt: now,
			Name:        "worker_run",
			Command:     "symphony worker run",
			Status:      VerificationFailed,
			Error:       reason,
			Required:    true,
		}},
	}
}

func publishRiskAssessment(issue Issue, report VerificationReport) string {
	if len(report.FailedRequired) > 0 {
		return "High: required local verification failed; do not merge until the failed gate output is resolved."
	}

	if len(failedOptionalGateNames(report)) > 0 {
		return "Medium: optional local verification failed; review the failed gate output before merging."
	}

	for _, label := range issue.Labels {
		switch normalizeState(label) {
		case publishLabelSecurity, publishLabelAuth, publishLabelAuthentication, publishLabelArchitecture:
			return "Medium: issue labels indicate a security- or architecture-sensitive change."
		}
	}

	if !report.Configured {
		return "Medium: no local verification gates were configured, so CI and human review remain the proof gates."
	}

	return "Low: configured local verification passed; human review and CI are still required before merge."
}

func failedOptionalGateNames(report VerificationReport) []string {
	var failed []string
	for i := range report.Gates {
		gate := report.Gates[i]
		if gate.Required || gate.Status == VerificationPassed {
			continue
		}

		name := strings.TrimSpace(gate.Name)
		if name != "" {
			failed = append(failed, privacy.RedactText(name))
		}
	}

	return failed
}

func redactVerificationNames(names []string) []string {
	redacted := make([]string, 0, len(names))
	for _, name := range names {
		name = strings.TrimSpace(privacy.RedactText(name))
		if name != "" {
			redacted = append(redacted, name)
		}
	}

	return redacted
}

func redactedRunFailure(runErr error) string {
	if runErr == nil {
		return "worker run failed before completion"
	}

	return privacy.RedactText(runErr.Error())
}

func publishReviewerFocus(issue Issue) string {
	seen := make(map[string]struct{})
	var focus []string

	add := func(value string) {
		if value == "" {
			return
		}

		if _, ok := seen[value]; ok {
			return
		}

		seen[value] = struct{}{}
		focus = append(focus, value)
	}

	for _, label := range issue.Labels {
		switch normalizeState(label) {
		case publishLabelSecurity, publishLabelAuth, publishLabelAuthentication:
			add("security")
		case publishLabelArchitecture, publishLabelAPI, publishLabelBackend:
			add("architecture/backend")
		case publishLabelQuality, publishLabelTesting, publishLabelTest:
			add("quality/testing")
		case publishLabelDocs, publishLabelDocumentation:
			add("documentation")
		case publishLabelUX, publishLabelUI, publishLabelFrontend:
			add("UX/frontend")
		}
	}

	return strings.Join(focus, ", ")
}

func publishSuggestedReviewers(issue Issue, report VerificationReport) string {
	seen := make(map[string]struct{})
	var reviewers []string

	add := func(value string) {
		if value == "" {
			return
		}

		if _, ok := seen[value]; ok {
			return
		}

		seen[value] = struct{}{}
		reviewers = append(reviewers, value)
	}

	for _, label := range issue.Labels {
		switch normalizeState(label) {
		case publishLabelSecurity, publishLabelAuth, publishLabelAuthentication:
			add("security reviewer")
		case publishLabelArchitecture, publishLabelAPI, publishLabelBackend:
			add("backend/architecture reviewer")
		case publishLabelQuality, publishLabelTesting, publishLabelTest:
			add("test/quality reviewer")
		case publishLabelDocs, publishLabelDocumentation:
			add("documentation reviewer")
		case publishLabelUX, publishLabelUI, publishLabelFrontend:
			add("UX/frontend reviewer")
		}
	}

	if len(report.FailedRequired) > 0 || len(failedOptionalGateNames(report)) > 0 {
		add("CI/build owner for failed validation evidence")
	}

	if len(reviewers) == 0 {
		add("maintainer familiar with the changed area")
	}

	return strings.Join(reviewers, ", ")
}

func oneLine(value string) string {
	return truncateOneLine(value)
}

func truncateOneLine(value string) string {
	const limit = 240

	value = strings.Join(strings.Fields(value), " ")
	if len(value) <= limit {
		return value
	}

	return value[:limit] + "..."
}

func publishIssueComment(pr GitHubPullRequest, removedLabels []string) string {
	var body bytes.Buffer
	fmt.Fprintf(&body, "Symphony published pull request #%d\n\n", pr.Number)
	labels := redactedIssueLabels(removedLabels)
	if len(labels) > 0 {
		fmt.Fprintf(&body, "Removed dispatch label(s) `%s` so this issue stops redispatching while the PR is open.\n", strings.Join(labels, "`, `"))
	}
	return body.String()
}

func publishFailureIssueComment(pr GitHubPullRequest, removedLabels []string) string {
	var body bytes.Buffer
	fmt.Fprintf(&body, "Symphony published draft pull request #%d after the implementation could not complete before verification.\n\n", pr.Number)
	labels := redactedIssueLabels(removedLabels)
	if len(labels) > 0 {
		fmt.Fprintf(&body, "Removed dispatch label(s) `%s` so this issue stops redispatching while the draft PR documents the failure.\n", strings.Join(labels, "`, `"))
	}
	return body.String()
}

func publishValidationFailureIssueComment(pr GitHubPullRequest, removedLabels []string) string {
	var body bytes.Buffer
	fmt.Fprintf(&body, "Symphony published draft pull request #%d after required local verification failed.\n\n", pr.Number)
	labels := redactedIssueLabels(removedLabels)
	if len(labels) > 0 {
		fmt.Fprintf(&body, "Removed dispatch label(s) `%s` so this issue stops redispatching while the draft PR documents failed verification.\n", strings.Join(labels, "`, `"))
	}
	return body.String()
}

const httpStatusValidationFailed = 422
