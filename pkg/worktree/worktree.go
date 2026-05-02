// Package worktree manages git worktrees for session isolation.
//
// When multiple atteler sessions run in the same repository, each session
// can operate in its own git worktree so that file changes do not collide.
// A session creates a branch and a linked worktree; when the session ends
// (or the user explicitly requests it) the branch is merged back into the
// original branch and the worktree is removed.
package worktree

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

// EnvDir overrides the parent directory for worktree directories.
const EnvDir = "ATTELER_WORKTREE_DIR"

var defaultContextFactory = context.Background

func defaultCommandContext() context.Context {
	return defaultContextFactory()
}

func nonNilCommandContext(ctx context.Context) context.Context {
	if ctx != nil {
		return ctx
	}
	return defaultCommandContext()
}

// Info describes an active session worktree.
type Info struct {
	// Path is the absolute filesystem path to the worktree directory.
	Path string
	// Branch is the git branch checked out in the worktree.
	Branch string
	// BaseBranch is the branch from which the worktree branch was created.
	BaseBranch string
	// SessionID ties the worktree back to an atteler session.
	SessionID string
}

// Create sets up a new git worktree for the given session.
//
// It creates a new branch named "atteler/<sessionID>" from the current HEAD,
// then adds a git worktree at a well-known path derived from the session ID.
// The returned Info contains all details needed to use and later clean up
// the worktree.
//
// repoDir must be the root of a git repository (or anywhere inside one).
func Create(repoDir, sessionID string) (*Info, error) {
	return CreateContext(defaultCommandContext(), repoDir, sessionID)
}

// CreateContext is Create with caller-provided cancellation for git commands.
func CreateContext(ctx context.Context, repoDir, sessionID string) (*Info, error) {
	ctx = nonNilCommandContext(ctx)
	if sessionID == "" {
		return nil, errors.New("worktree: session ID is required")
	}

	repoRoot, err := gitRepoRoot(ctx, repoDir)
	if err != nil {
		return nil, fmt.Errorf("worktree: locate repo root: %w", err)
	}

	baseBranch, err := gitCurrentBranch(ctx, repoRoot)
	if err != nil {
		return nil, fmt.Errorf("worktree: detect current branch: %w", err)
	}

	branch := "atteler/" + sessionID
	wtDir := worktreeDir(repoRoot, sessionID)

	// Create the branch from current HEAD.
	if err := gitRun(ctx, repoRoot, "branch", branch); err != nil {
		// If the branch already exists, that's fine (e.g. re-joining a session).
		if !strings.Contains(err.Error(), "already exists") {
			return nil, fmt.Errorf("worktree: create branch %s: %w", branch, err)
		}
	}

	// Add the worktree.
	if err := gitRun(ctx, repoRoot, "worktree", "add", wtDir, branch); err != nil {
		// If it already exists, treat as success (join).
		if !strings.Contains(err.Error(), "already exists") &&
			!strings.Contains(err.Error(), "is already checked out") {
			return nil, fmt.Errorf("worktree: add %s: %w", wtDir, err)
		}
	}

	return &Info{
		Path:       wtDir,
		Branch:     branch,
		BaseBranch: baseBranch,
		SessionID:  sessionID,
	}, nil
}

// Merge merges the worktree branch back into the base branch and removes
// the worktree. The merge uses --no-ff to keep the branch history visible.
// If there are uncommitted changes in the worktree, they are committed first
// with a default message.
func Merge(repoDir string, info *Info) error {
	return MergeContext(defaultCommandContext(), repoDir, info)
}

// MergeContext is Merge with caller-provided cancellation for git commands.
func MergeContext(ctx context.Context, repoDir string, info *Info) error {
	ctx = nonNilCommandContext(ctx)
	if info == nil {
		return errors.New("worktree: nil info")
	}

	repoRoot, err := gitRepoRoot(ctx, repoDir)
	if err != nil {
		return fmt.Errorf("worktree: locate repo root: %w", err)
	}

	// Auto-commit any uncommitted changes in the worktree.
	if err := autoCommit(ctx, info.Path, info.SessionID); err != nil {
		return fmt.Errorf("worktree: auto-commit: %w", err)
	}

	// Merge branch into base from the main repo.
	mergeMsg := "atteler: merge session " + info.SessionID
	if err := gitRun(ctx, repoRoot, "merge", "--no-ff", "-m", mergeMsg, info.Branch); err != nil {
		return fmt.Errorf("worktree: merge %s into %s: %w", info.Branch, info.BaseBranch, err)
	}

	return RemoveContext(ctx, repoDir, info)
}

// Remove deletes the worktree and its branch without merging.
func Remove(repoDir string, info *Info) error {
	return RemoveContext(defaultCommandContext(), repoDir, info)
}

// RemoveContext is Remove with caller-provided cancellation for git commands.
func RemoveContext(ctx context.Context, repoDir string, info *Info) error {
	ctx = nonNilCommandContext(ctx)
	if info == nil {
		return errors.New("worktree: nil info")
	}

	repoRoot, err := gitRepoRoot(ctx, repoDir)
	if err != nil {
		return fmt.Errorf("worktree: locate repo root: %w", err)
	}

	errs := []error{
		gitRun(ctx, repoRoot, "worktree", "remove", "--force", info.Path),
		gitRun(ctx, repoRoot, "worktree", "prune"),
		gitRun(ctx, repoRoot, "branch", "-D", info.Branch),
	}
	if err := errors.Join(errs...); err != nil {
		return fmt.Errorf("worktree: remove: %w", err)
	}
	return nil
}

// List returns all atteler-managed worktrees found in the repository.
func List(repoDir string) ([]Info, error) {
	return ListContext(defaultCommandContext(), repoDir)
}

// ListContext is List with caller-provided cancellation for git commands.
func ListContext(ctx context.Context, repoDir string) ([]Info, error) {
	ctx = nonNilCommandContext(ctx)
	repoRoot, err := gitRepoRoot(ctx, repoDir)
	if err != nil {
		return nil, fmt.Errorf("worktree: locate repo root: %w", err)
	}

	out, err := gitOutput(ctx, repoRoot, "worktree", "list", "--porcelain")
	if err != nil {
		return nil, fmt.Errorf("worktree: list: %w", err)
	}

	return parseWorktreeListContext(ctx, out, repoRoot), nil
}

// Status returns a human-readable summary of an active worktree.
func Status(info *Info) string {
	if info == nil {
		return "no worktree"
	}
	return fmt.Sprintf("worktree: %s (branch %s, base %s)", info.Path, info.Branch, info.BaseBranch)
}

// IsGitRepo reports whether dir is inside a git repository.
func IsGitRepo(dir string) bool {
	return IsGitRepoContext(defaultCommandContext(), dir)
}

// IsGitRepoContext reports whether dir is inside a git repository using ctx.
func IsGitRepoContext(ctx context.Context, dir string) bool {
	_, err := gitRepoRoot(nonNilCommandContext(ctx), dir)
	return err == nil
}

// ---------------------------------------------------------------------------
// Internal helpers
// ---------------------------------------------------------------------------

// worktreeDir returns the directory path for a session worktree.
func worktreeDir(repoRoot, sessionID string) string {
	if dir := os.Getenv(EnvDir); dir != "" {
		return filepath.Join(dir, "atteler-"+sessionID)
	}
	return filepath.Join(repoRoot, ".atteler", "worktrees", sessionID)
}

// autoCommit stages and commits any dirty files in the worktree.
func autoCommit(ctx context.Context, wtDir, sessionID string) error {
	ctx = nonNilCommandContext(ctx)
	// Check if there are changes.
	out, err := gitOutput(ctx, wtDir, "status", "--porcelain")
	if err != nil {
		return err
	}
	if strings.TrimSpace(out) == "" {
		return nil // nothing to commit
	}

	if err := gitRun(ctx, wtDir, "add", "-A"); err != nil {
		return fmt.Errorf("stage changes: %w", err)
	}

	msg := fmt.Sprintf("atteler: auto-commit session %s at %s",
		sessionID, time.Now().UTC().Format(time.RFC3339))
	return gitRun(ctx, wtDir, "commit", "-m", msg)
}

// gitRepoRoot returns the top-level directory of the git repository
// containing dir.
func gitRepoRoot(ctx context.Context, dir string) (string, error) {
	out, err := gitOutput(nonNilCommandContext(ctx), dir, "rev-parse", "--show-toplevel")
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(out), nil
}

// gitCurrentBranch returns the current branch name. If HEAD is detached
// it returns "HEAD".
func gitCurrentBranch(ctx context.Context, dir string) (string, error) {
	out, err := gitOutput(nonNilCommandContext(ctx), dir, "rev-parse", "--abbrev-ref", "HEAD")
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(out), nil
}

// gitRun executes a git command in dir and returns any error.
func gitRun(ctx context.Context, dir string, args ...string) error {
	cmd := exec.CommandContext(nonNilCommandContext(ctx), "git", args...)
	cmd.Dir = dir
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("git %s: %s: %w", strings.Join(args, " "), strings.TrimSpace(stderr.String()), err)
	}
	return nil
}

// gitOutput executes a git command in dir and returns its stdout.
func gitOutput(ctx context.Context, dir string, args ...string) (string, error) {
	cmd := exec.CommandContext(nonNilCommandContext(ctx), "git", args...)
	cmd.Dir = dir
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return "", fmt.Errorf("git %s: %s: %w", strings.Join(args, " "), strings.TrimSpace(stderr.String()), err)
	}
	return stdout.String(), nil
}

// parseWorktreeList parses the porcelain output of `git worktree list`
// and returns only atteler-managed entries (branches starting with "atteler/").
func parseWorktreeList(output, repoRoot string) []Info {
	return parseWorktreeListContext(defaultCommandContext(), output, repoRoot)
}

func parseWorktreeListContext(ctx context.Context, output, repoRoot string) []Info {
	ctx = nonNilCommandContext(ctx)
	var results []Info
	var current Info

	for line := range strings.SplitSeq(output, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			if current.Branch != "" && strings.HasPrefix(current.Branch, "atteler/") {
				current.SessionID = strings.TrimPrefix(current.Branch, "atteler/")
				results = append(results, current)
			}
			current = Info{}
			continue
		}

		if path, ok := strings.CutPrefix(line, "worktree "); ok {
			current.Path = path
		}
		if branch, ok := strings.CutPrefix(line, "branch refs/heads/"); ok {
			current.Branch = branch
		}
	}

	// Flush last entry.
	if current.Branch != "" && strings.HasPrefix(current.Branch, "atteler/") {
		current.SessionID = strings.TrimPrefix(current.Branch, "atteler/")
		results = append(results, current)
	}

	// Try to fill in BaseBranch from the main worktree's branch.
	if len(results) > 0 {
		if base, err := gitCurrentBranch(ctx, repoRoot); err == nil {
			for i := range results {
				results[i].BaseBranch = base
			}
		}
	}

	return results
}
