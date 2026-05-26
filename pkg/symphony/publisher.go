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
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
)

type gitCommandRunner func(context.Context, string, []string, ...string) ([]byte, error)

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
	cfg    Config
	client *GitHubClient
	runGit gitCommandRunner
	logger *slog.Logger
}

func (p *githubPublisher) Publish(ctx context.Context, issue Issue, workspace Workspace) (*PublishResult, error) {
	if strings.TrimSpace(workspace.Path) == "" {
		return nil, errors.New("publish: workspace path is required")
	}

	issueNumber, err := githubIssueNumber(issue)
	if err != nil {
		return nil, err
	}

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

	existingPR, err := p.client.FetchOpenPullRequestByHead(ctx, branch)
	if err != nil {
		return nil, err
	}

	if !hasCommits && existingPR == nil {
		result.SkippedReason = "workspace has no changes to publish"
		return result, nil
	}

	if hasCommits {
		if result.CommitSHA == "" {
			result.CommitSHA, err = p.currentCommit(ctx, workspace.Path)
			if err != nil {
				return nil, err
			}
		}

		if remoteErr := p.setRemote(ctx, workspace.Path); remoteErr != nil {
			return nil, remoteErr
		}

		if pushErr := p.push(ctx, workspace.Path, branch); pushErr != nil {
			return nil, pushErr
		}
	}

	pr := existingPR
	if pr == nil {
		created, createErr := p.createPullRequest(ctx, issue, branch)
		if createErr != nil {
			return nil, createErr
		}

		pr = &created
	} else {
		result.ExistingPullRequest = true
	}

	result.PullRequestNumber = pr.Number
	result.PullRequestURL = pr.HTMLURL
	result.Published = true

	removed, finalizeErr := p.finalizeIssue(ctx, issueNumber, issue, *pr)
	if finalizeErr != nil {
		return nil, finalizeErr
	}

	result.RemovedLabels = removed
	return result, nil
}

func (p *githubPublisher) checkoutBranch(ctx context.Context, dir, branch string) error {
	_, err := p.git(ctx, dir, nil, "checkout", "-B", branch)
	return err
}

func (p *githubPublisher) hasChanges(ctx context.Context, dir string) (bool, error) {
	output, err := p.git(ctx, dir, nil, "status", "--porcelain")
	if err != nil {
		return false, err
	}

	return strings.TrimSpace(string(output)) != "", nil
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

	if _, writeErr := messageFile.WriteString(publishCommitMessage(issue)); writeErr != nil {
		_ = messageFile.Close()
		return "", fmt.Errorf("publish: write commit message: %w", writeErr)
	}

	if closeErr := messageFile.Close(); closeErr != nil {
		return "", fmt.Errorf("publish: close commit message: %w", closeErr)
	}

	if _, commitErr := p.git(ctx, dir, nil, "commit", "-F", messagePath); commitErr != nil {
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

	if _, err := p.git(ctx, dir, nil, "checkout", "-B", branch, branchRemote); err != nil {
		return "", fmt.Errorf("publish: checkout pull request branch %s: %w", branch, err)
	}

	dirty, err := p.hasChanges(ctx, dir)
	if err != nil {
		return "", err
	}
	if dirty {
		return "", errors.New("publish: workspace has uncommitted changes before branch update")
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

func (p *githubPublisher) createPullRequest(ctx context.Context, issue Issue, branch string) (GitHubPullRequest, error) {
	pr, err := p.client.CreatePullRequest(
		ctx,
		branch,
		p.cfg.Publish.BaseBranch,
		publishPRTitle(issue),
		publishPRBody(issue),
		p.cfg.Publish.Draft,
	)
	if err == nil {
		return pr, nil
	}

	var statusErr *githubAPIStatusError
	if !errors.As(err, &statusErr) || statusErr.StatusCode != httpStatusValidationFailed {
		return GitHubPullRequest{}, err
	}

	existing, findErr := p.client.FetchOpenPullRequestByHead(ctx, branch)
	if findErr != nil {
		return GitHubPullRequest{}, findErr
	}

	if existing == nil {
		return GitHubPullRequest{}, err
	}

	return *existing, nil
}

func (p *githubPublisher) finalizeIssue(ctx context.Context, issueNumber int, issue Issue, pr GitHubPullRequest) ([]string, error) {
	removed := trimNonEmptyStrings(p.cfg.Publish.RemoveLabels)
	for _, label := range removed {
		if labelErr := p.client.RemoveIssueLabel(ctx, issueNumber, label); labelErr != nil {
			return nil, fmt.Errorf("publish: remove issue label %q: %w", label, labelErr)
		}
	}

	comment := publishIssueComment(pr, removed)
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

	return p.runGit(ctx, dir, env, args...)
}

func defaultGitCommandRunner(ctx context.Context, dir string, env []string, args ...string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, "git", args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), env...)

	output, err := cmd.CombinedOutput()
	if err == nil {
		return output, nil
	}

	message := strings.TrimSpace(string(output))
	if message == "" {
		message = err.Error()
	}

	return output, fmt.Errorf("git %s: %s: %w", strings.Join(args, " "), message, err)
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
	fmt.Fprintf(&body, "chore: publish %s Symphony workspace\n\n", issue.Identifier)
	fmt.Fprintf(&body, "Symphony completed a worker run for %s: %s.\n\n", issue.Identifier, issue.Title)
	fmt.Fprintln(&body, "Constraint: Publication is automated from the issue workspace")
	fmt.Fprintln(&body, "Confidence: medium")
	fmt.Fprintln(&body, "Scope-risk: moderate")
	fmt.Fprintln(&body, "Tested: See the Symphony pull request body and worker output")
	fmt.Fprintln(&body, "Not-tested: Human review pending")
	fmt.Fprintf(&body, "Related: %s\n", issue.Identifier)
	return body.String()
}

func publishPRTitle(issue Issue) string {
	return strings.TrimSpace(issue.Identifier + ": " + issue.Title)
}

func publishPRBody(issue Issue) string {
	var body bytes.Buffer
	fmt.Fprintf(&body, "Automated Symphony publication for %s.\n\n", issue.Identifier)
	if issue.URL != nil && strings.TrimSpace(*issue.URL) != "" {
		fmt.Fprintf(&body, "Issue: %s\n\n", strings.TrimSpace(*issue.URL))
	}

	if number, err := githubIssueNumber(issue); err == nil {
		fmt.Fprintf(&body, "Closes #%d\n\n", number)
	}

	fmt.Fprintln(&body, "## Verification")
	fmt.Fprintln(&body, "Review the worker commit, CI output, and any verification notes left by the Symphony run.")
	return body.String()
}

func publishIssueComment(pr GitHubPullRequest, removedLabels []string) string {
	var body bytes.Buffer
	fmt.Fprintf(&body, "Symphony opened pull request #%d\n\n", pr.Number)
	if len(removedLabels) > 0 {
		fmt.Fprintf(&body, "Removed dispatch label(s) `%s` so this issue stops redispatching while the PR is open.\n", strings.Join(removedLabels, "`, `"))
	}
	return body.String()
}

const httpStatusValidationFailed = 422
