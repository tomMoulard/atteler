//nolint:cyclop,gocognit,govet,wrapcheck,wsl_v5 // The runner follows the ordered attempt lifecycle from the Symphony spec.
package symphony

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"
)

// DefaultAgentRunner prepares the workspace, renders prompts, and drives one
// Codex app-server worker lifetime.
type DefaultAgentRunner struct {
	workspaces *WorkspaceManager
	tracker    TrackerClient
	logger     *slog.Logger
}

// NewDefaultAgentRunner creates the production worker runner.
func NewDefaultAgentRunner(tracker TrackerClient, logger *slog.Logger) *DefaultAgentRunner {
	return &DefaultAgentRunner{
		workspaces: NewWorkspaceManager(logger),
		tracker:    tracker,
		logger:     loggerOrDefault(logger),
	}
}

// Run executes one issue attempt.
func (r *DefaultAgentRunner) Run(ctx context.Context, req RunRequest, emit func(CodexEvent)) (RunResult, error) {
	result := RunResult{
		StartedAt: time.Now().UTC(),
		Status:    AttemptFailed,
	}

	workspace, err := r.workspaces.Ensure(ctx, req.Config, req.Issue)
	if err != nil {
		result.CompletedAt = time.Now().UTC()
		result.Error = err.Error()
		return result, err
	}

	result.WorkspacePath = workspace.Path

	afterRunNeeded := true
	defer func() {
		if !afterRunNeeded || strings.TrimSpace(req.Config.Hooks.AfterRun) == "" {
			return
		}

		if err := RunHook(ctx, req.Config, req.Issue, workspace, "after_run", req.Config.Hooks.AfterRun); err != nil {
			r.logger.Warn(
				"symphony hook failed; ignoring",
				"hook", "after_run",
				"issue_id", req.Issue.ID,
				"issue_identifier", req.Issue.Identifier,
				"error", err,
			)
		}
	}()

	if shouldRunBeforeRunHook(req, workspace) {
		if err := RunHook(ctx, req.Config, req.Issue, workspace, "before_run", req.Config.Hooks.BeforeRun); err != nil {
			result.CompletedAt = time.Now().UTC()
			result.Error = err.Error()
			return result, err
		}
	}

	if req.Context != nil && req.Context.Kind == RunKindPullRequestRework {
		if err := PreparePullRequestReworkWorkspace(ctx, req.Config, req.Context.PullRequest, workspace, r.logger); err != nil {
			result.CompletedAt = time.Now().UTC()
			result.Error = err.Error()
			return result, err
		}
	}

	client, err := StartAppServerForIssue(ctx, req.Config.Codex, req.Issue, workspace.Path, emit)
	if err != nil {
		result.CompletedAt = time.Now().UTC()
		result.Error = err.Error()
		return result, err
	}
	defer client.Close()

	threadID, err := client.StartThread(ctx, req.Config, req.Issue, workspace.Path)
	if err != nil {
		result.CompletedAt = time.Now().UTC()
		result.Error = err.Error()
		return result, err
	}

	issue := req.Issue
	for turn := 1; turn <= req.Config.Agent.MaxTurns; turn++ {
		prompt, err := turnPrompt(req.Workflow, issue, req.Attempt, req.Context, turn, req.Config.Agent.MaxTurns)
		if err != nil {
			result.CompletedAt = time.Now().UTC()
			result.Error = err.Error()
			return result, err
		}

		if err := client.RunTurn(ctx, req.Config, threadID, prompt, workspace.Path); err != nil {
			result.CompletedAt = time.Now().UTC()
			result.Error = err.Error()
			result.Status = statusForRunError(err)
			return result, err
		}

		refreshed, err := r.tracker.FetchIssueStatesByIDs(ctx, []string{issue.ID})
		if err != nil {
			result.CompletedAt = time.Now().UTC()
			result.Error = err.Error()
			return result, err
		}

		if len(refreshed) > 0 {
			issue = refreshed[0]
		}

		if !isActiveState(issue.State, req.Config) || isTerminalState(issue.State, req.Config) {
			break
		}
	}

	publishResult, err := PublishWorkspace(ctx, req.Config, issue, workspace, r.logger)
	if err != nil {
		result.CompletedAt = time.Now().UTC()
		result.Error = err.Error()
		return result, err
	}

	result.Publish = publishResult
	result.CompletedAt = time.Now().UTC()
	result.Status = AttemptSucceeded
	afterRunNeeded = true

	return result, nil
}

func shouldRunBeforeRunHook(req RunRequest, workspace Workspace) bool {
	if strings.TrimSpace(req.Config.Hooks.BeforeRun) == "" {
		return false
	}

	if req.Context == nil || req.Context.Kind != RunKindPullRequestRework {
		return true
	}

	hasGit, err := workspaceHasGitCheckout(workspace)
	return err != nil || !hasGit
}

func turnPrompt(def WorkflowDefinition, issue Issue, attempt *int, runContext *RunContext, turnNumber, maxTurns int) (string, error) {
	if turnNumber <= 1 {
		prompt, err := RenderPrompt(def.PromptTemplate, issue, attempt)
		if err != nil {
			return "", err
		}

		contextPrompt := runContextPrompt(runContext)
		if contextPrompt == "" {
			return prompt, nil
		}

		return prompt + "\n\n---\n\n" + contextPrompt, nil
	}

	return fmt.Sprintf(
		"Continue working on issue %s: %s.\n\nThis is continuation turn %d of %d in the same Symphony worker session. Do not repeat completed work; inspect the workspace and continue from the current state.",
		issue.Identifier,
		issue.Title,
		turnNumber,
		maxTurns,
	), nil
}

func runContextPrompt(runContext *RunContext) string {
	if runContext == nil || runContext.PullRequest == nil || runContext.Kind != RunKindPullRequestRework {
		return ""
	}

	pr := runContext.PullRequest
	var builder strings.Builder
	fmt.Fprintln(&builder, "Symphony PR rework context:")
	fmt.Fprintf(&builder, "- Pull request: #%d %s\n", pr.Number, pr.URL)
	fmt.Fprintf(&builder, "- Branch: %s\n", pr.Branch)
	if pr.HeadSHA != "" {
		fmt.Fprintf(&builder, "- Head SHA: %s\n", pr.HeadSHA)
	}
	fmt.Fprintf(&builder, "- Rework attempt: %d\n", pr.ReworkAttempt)
	if pr.Summary != "" {
		fmt.Fprintf(&builder, "- Check summary: %s\n", pr.Summary)
	}
	if len(pr.FailedChecks) > 0 {
		fmt.Fprintln(&builder, "- Failing checks:")
		for _, check := range pr.FailedChecks {
			fmt.Fprintf(&builder, "  - %s\n", check)
		}
	}

	fmt.Fprintln(&builder)
	fmt.Fprintln(&builder, "Inspect the existing PR workspace and make the smallest focused fix needed for the failing CI, branch update, or rebase conflict. Keep working on the same branch; Symphony will commit, push, and reuse the existing PR when this run succeeds.")
	fmt.Fprintln(&builder, "If the workspace is in a rebase conflict, resolve the conflicted files, stage them, and run `git rebase --continue` before finishing. If it is not already in a rebase, fetch the base branch, rebase this PR branch onto it, resolve any conflicts, and continue the rebase. Do not skip the Symphony commit unless the change is genuinely obsolete.")

	return strings.TrimSpace(builder.String())
}

func statusForRunError(err error) AttemptStatus {
	if err == nil {
		return AttemptSucceeded
	}

	switch {
	case errors.Is(err, context.Canceled):
		return AttemptCanceled
	case errors.Is(err, context.DeadlineExceeded):
		return AttemptTimedOut
	default:
		return AttemptFailed
	}
}
