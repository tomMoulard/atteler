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
	"regexp"
	"strings"
	"time"
)

// EnvDir overrides the parent directory for worktree directories.
const EnvDir = "ATTELER_WORKTREE_DIR"

const (
	worktreeBranchPrefix = "atteler/"
	maxSessionIDLength   = 128
	transactionLogGitDir = "atteler/worktree-transactions"
)

var safeSessionIDPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]*$`)

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

// MergeOptions is the explicit safety policy for merging a session worktree.
type MergeOptions struct {
	// Provenance adds optional caller-provided context to auto-commit messages.
	Provenance []string
	// AutoCommit permits atteler to stage and commit dirty worktree files before
	// merging. If false, MergeWithOptionsContext refuses dirty worktrees.
	AutoCommit bool
	// AutoMerge permits atteler to run git merge in the base repository. This
	// makes auto-merge call sites opt in instead of reaching the merge path by
	// accident.
	AutoMerge bool
	// AllowBaseBranchMismatch permits merging even when the main worktree is not
	// checked out to Info.BaseBranch. Keep false for normal session finalization.
	AllowBaseBranchMismatch bool
}

// MergeError describes a failed merge transaction and how to recover it.
type MergeError struct {
	Err            error
	Step           string
	Branch         string
	WorktreePath   string
	TransactionLog string
	RolledBack     bool
}

func (e *MergeError) Error() string {
	var b strings.Builder

	fmt.Fprintf(&b, "worktree: %s failed for branch %s at %s: %v",
		e.Step, printable(e.Branch), printable(e.WorktreePath), e.Err)

	if e.RolledBack {
		b.WriteString("\nrecovery: failed merge was rolled back with git merge --abort")
	}

	b.WriteString("\nrecovery: failed step: " + printable(e.Step))
	b.WriteString("\nrecovery: branch: " + printable(e.Branch))
	b.WriteString("\nrecovery: worktree path: " + printable(e.WorktreePath))

	if e.TransactionLog != "" {
		b.WriteString("\nrecovery: transaction log: " + e.TransactionLog)
	}

	b.WriteString("\nrecovery: inspect the worktree, fix the failed step, then retry the merge")

	return b.String()
}

func (e *MergeError) Unwrap() error {
	return e.Err
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

	branch, err := branchForSessionID(sessionID)
	if err != nil {
		return nil, err
	}

	repoRoot, err := gitRepoRoot(ctx, repoDir)
	if err != nil {
		return nil, fmt.Errorf("worktree: locate repo root: %w", err)
	}

	baseBranch, err := gitCurrentBranch(ctx, repoRoot)
	if err != nil {
		return nil, fmt.Errorf("worktree: detect current branch: %w", err)
	}

	if baseBranch == "HEAD" {
		return nil, errors.New("worktree: cannot create worktree from detached HEAD")
	}

	wtDir := worktreeDir(repoRoot, sessionID)

	branchExists, err := gitBranchExists(ctx, repoRoot, branch)
	if err != nil {
		return nil, fmt.Errorf("worktree: check branch %s: %w", branch, err)
	}

	if branchExists {
		if err := verifyExistingWorktree(ctx, repoRoot, branch, wtDir); err != nil {
			return nil, fmt.Errorf("worktree: branch %s already exists without matching worktree: %w", branch, err)
		}

		return &Info{
			Path:       wtDir,
			Branch:     branch,
			BaseBranch: baseBranch,
			SessionID:  sessionID,
		}, nil
	} else if err := gitRun(ctx, repoRoot, "branch", branch); err != nil {
		return nil, fmt.Errorf("worktree: create branch %s: %w", branch, err)
	}

	// Add the worktree.
	if err := gitRun(ctx, repoRoot, "worktree", "add", wtDir, branch); err != nil {
		// If the same session worktree already exists, treat as success (join).
		if !strings.Contains(err.Error(), "already exists") &&
			!strings.Contains(err.Error(), "is already checked out") &&
			!strings.Contains(err.Error(), "is already used by worktree") {
			addErr := fmt.Errorf("worktree: add %s: %w", wtDir, err)

			return nil, rollbackCreatedBranch(ctx, repoRoot, branch, addErr)
		}

		if joinErr := verifyExistingWorktree(ctx, repoRoot, branch, wtDir); joinErr != nil {
			addErr := fmt.Errorf("worktree: add %s: %w", wtDir, errors.Join(err, joinErr))

			return nil, rollbackCreatedBranch(ctx, repoRoot, branch, addErr)
		}
	}

	return &Info{
		Path:       wtDir,
		Branch:     branch,
		BaseBranch: baseBranch,
		SessionID:  sessionID,
	}, nil
}

// Merge refuses to merge without an explicit MergeOptions policy.
//
// Use MergeWithOptionsContext when a caller intentionally permits auto-commit
// and auto-merge behavior.
func Merge(repoDir string, info *Info) error {
	return MergeContext(defaultCommandContext(), repoDir, info)
}

// MergeContext is Merge with caller-provided cancellation for git commands.
func MergeContext(ctx context.Context, repoDir string, info *Info) error {
	return MergeWithOptionsContext(ctx, repoDir, info, MergeOptions{})
}

// MergeWithOptionsContext merges the worktree using an explicit safety policy.
func MergeWithOptionsContext(ctx context.Context, repoDir string, info *Info, opts MergeOptions) error {
	ctx = nonNilCommandContext(ctx)

	if info == nil {
		return errors.New("worktree: nil info")
	}

	if err := validateInfo(info, true); err != nil {
		return err
	}

	repoRoot, rootErr := gitRepoRoot(ctx, repoDir)
	if rootErr != nil {
		return fmt.Errorf("worktree: locate repo root: %w", rootErr)
	}

	log, startErr := startMergeTransaction(ctx, repoRoot, info, opts)
	if startErr != nil {
		return startErr
	}

	if policyErr := requireAutoMergePolicy(opts); policyErr != nil {
		return mergeFailure("auto-merge policy", info, log, policyErr)
	}

	if preflightErr := preflightMainRepo(ctx, repoRoot, info, opts); preflightErr != nil {
		return mergeFailure("preflight main repository", info, log, preflightErr)
	}

	if appendErr := log.append("preflight main repository", "ok"); appendErr != nil {
		return mergeFailure("write transaction log", info, log, appendErr)
	}

	if commitErr := autoCommitIfNeeded(ctx, info, opts, log); commitErr != nil {
		return commitErr
	}

	if mergeErr := mergeBranch(ctx, repoRoot, info, log); mergeErr != nil {
		return mergeErr
	}

	if cleanupErr := cleanupMergedWorktree(ctx, repoDir, info, log); cleanupErr != nil {
		return cleanupErr
	}

	if appendErr := log.append("complete", "ok"); appendErr != nil {
		return mergeFailure("write transaction log", info, log, appendErr)
	}

	return nil
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

	if err := validateInfo(info, false); err != nil {
		return err
	}

	repoRoot, rootErr := gitRepoRoot(ctx, repoDir)
	if rootErr != nil {
		return fmt.Errorf("worktree: locate repo root: %w", rootErr)
	}

	if removeErr := gitRun(ctx, repoRoot, "worktree", "remove", "--force", info.Path); removeErr != nil {
		return fmt.Errorf("worktree: remove path %s: %w", info.Path, removeErr)
	}

	if pruneErr := gitRun(ctx, repoRoot, "worktree", "prune"); pruneErr != nil {
		return fmt.Errorf("worktree: prune: %w", pruneErr)
	}

	if branchErr := gitRun(ctx, repoRoot, "branch", "-D", info.Branch); branchErr != nil {
		return fmt.Errorf("worktree: delete branch %s: %w", info.Branch, branchErr)
	}

	if exists, err := pathExists(info.Path); err != nil {
		return fmt.Errorf("worktree: verify path cleanup %s: %w", info.Path, err)
	} else if exists {
		return fmt.Errorf("worktree: verify path cleanup %s: still exists", info.Path)
	}

	branchExists, err := gitBranchExists(ctx, repoRoot, info.Branch)
	if err != nil {
		return fmt.Errorf("worktree: verify branch cleanup %s: %w", info.Branch, err)
	}

	if branchExists {
		return fmt.Errorf("worktree: verify branch cleanup %s: still exists", info.Branch)
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

func branchForSessionID(sessionID string) (string, error) {
	if err := validateSessionID(sessionID); err != nil {
		return "", err
	}

	return worktreeBranchPrefix + sessionID, nil
}

func validateSessionID(sessionID string) error {
	if sessionID == "" {
		return errors.New("worktree: session ID is required")
	}

	if len(sessionID) > maxSessionIDLength {
		return fmt.Errorf("worktree: session ID %q is too long", sessionID)
	}

	if sessionID == "." || sessionID == ".." {
		return fmt.Errorf("worktree: unsafe session ID %q", sessionID)
	}

	if !safeSessionIDPattern.MatchString(sessionID) ||
		strings.Contains(sessionID, "..") ||
		strings.Contains(sessionID, "@{") ||
		strings.HasSuffix(sessionID, ".") ||
		strings.HasSuffix(sessionID, ".lock") {
		return fmt.Errorf("worktree: unsafe session ID %q: use only letters, numbers, dot, underscore, and dash", sessionID)
	}

	return nil
}

func validateInfo(info *Info, requireBase bool) error {
	if info == nil {
		return errors.New("worktree: nil info")
	}

	if err := validateSessionID(info.SessionID); err != nil {
		return err
	}

	wantBranch := worktreeBranchPrefix + info.SessionID
	if info.Branch != wantBranch {
		return fmt.Errorf("worktree: branch %q does not match session branch %q", info.Branch, wantBranch)
	}

	if strings.TrimSpace(info.Path) == "" {
		return errors.New("worktree: worktree path is required")
	}

	if requireBase && strings.TrimSpace(info.BaseBranch) == "" {
		return errors.New("worktree: base branch is required")
	}

	return nil
}

func startMergeTransaction(ctx context.Context, repoRoot string, info *Info, opts MergeOptions) (*transactionLog, error) {
	log, err := newTransactionLog(ctx, repoRoot, info.SessionID)
	if err != nil {
		return nil, mergeFailure("create transaction log", info, nil, err)
	}

	detail := fmt.Sprintf("session=%s branch=%s base=%s worktree=%s auto_commit=%t auto_merge=%t allow_base_mismatch=%t",
		info.SessionID, info.Branch, info.BaseBranch, info.Path, opts.AutoCommit, opts.AutoMerge, opts.AllowBaseBranchMismatch)
	if appendErr := log.append("start", detail); appendErr != nil {
		return nil, mergeFailure("write transaction log", info, log, appendErr)
	}

	return log, nil
}

func requireAutoMergePolicy(opts MergeOptions) error {
	if !opts.AutoMerge {
		return errors.New("merge policy does not permit running git merge")
	}

	return nil
}

func rollbackCreatedBranch(ctx context.Context, repoRoot, branch string, cause error) error {
	if err := gitRun(ctx, repoRoot, "branch", "-D", branch); err != nil {
		return errors.Join(cause, fmt.Errorf("rollback created branch %s: %w", branch, err))
	}

	return cause
}

func verifyExistingWorktree(ctx context.Context, repoRoot, branch, expectedPath string) error {
	actualPath, ok, err := gitWorktreePathForBranch(ctx, repoRoot, branch)
	if err != nil {
		return err
	}

	if !ok {
		return fmt.Errorf("branch %s is not checked out at %s", branch, expectedPath)
	}

	if !samePath(actualPath, expectedPath) {
		return fmt.Errorf("branch %s is already checked out at %s, expected %s", branch, actualPath, expectedPath)
	}

	if exists, err := pathExists(expectedPath); err != nil {
		return fmt.Errorf("check existing worktree path %s: %w", expectedPath, err)
	} else if !exists {
		return fmt.Errorf("branch %s is checked out at missing path %s", branch, expectedPath)
	}

	return nil
}

func preflightMainRepo(ctx context.Context, repoRoot string, info *Info, opts MergeOptions) error {
	if err := preflightCurrentBranch(ctx, repoRoot, info, opts); err != nil {
		return err
	}

	if err := preflightCleanState(ctx, repoRoot); err != nil {
		return err
	}

	if err := preflightWorktreeCheckout(ctx, repoRoot, info); err != nil {
		return err
	}

	return nil
}

func preflightCurrentBranch(ctx context.Context, repoRoot string, info *Info, opts MergeOptions) error {
	currentBranch, err := gitCurrentBranch(ctx, repoRoot)
	if err != nil {
		return fmt.Errorf("detect current branch: %w", err)
	}

	if currentBranch == "HEAD" {
		return errors.New("main worktree is in detached HEAD; check out the recorded base branch before merging")
	}

	if currentBranch != info.BaseBranch && !opts.AllowBaseBranchMismatch {
		return fmt.Errorf("main worktree is on branch %s, expected recorded base branch %s", currentBranch, info.BaseBranch)
	}

	return nil
}

func preflightCleanState(ctx context.Context, repoRoot string) error {
	pending, err := gitPendingOperation(ctx, repoRoot)
	if err != nil {
		return fmt.Errorf("detect pending git operation: %w", err)
	}

	if pending != "" {
		return fmt.Errorf("main worktree has pending %s; finish or abort it before merging", pending)
	}

	summary, err := gitStatusSummary(ctx, repoRoot)
	if err != nil {
		return fmt.Errorf("read main worktree status: %w", err)
	}

	if !summary.empty() {
		return fmt.Errorf("main worktree has uncommitted or untracked files:\n%s", summary.String())
	}

	return nil
}

func preflightWorktreeCheckout(ctx context.Context, repoRoot string, info *Info) error {
	branchExists, err := gitBranchExists(ctx, repoRoot, info.Branch)
	if err != nil {
		return fmt.Errorf("check worktree branch: %w", err)
	}

	if !branchExists {
		return fmt.Errorf("worktree branch %s does not exist", info.Branch)
	}

	actualPath, ok, err := gitWorktreePathForBranch(ctx, repoRoot, info.Branch)
	if err != nil {
		return fmt.Errorf("check worktree branch checkout: %w", err)
	}

	if !ok {
		return fmt.Errorf("worktree branch %s is not checked out in a worktree", info.Branch)
	}

	if !samePath(actualPath, info.Path) {
		return fmt.Errorf("worktree branch %s is checked out at %s, expected %s", info.Branch, actualPath, info.Path)
	}

	if exists, err := pathExists(info.Path); err != nil {
		return fmt.Errorf("check worktree path %s: %w", info.Path, err)
	} else if !exists {
		return fmt.Errorf("worktree path %s does not exist", info.Path)
	}

	return nil
}

func autoCommitIfNeeded(ctx context.Context, info *Info, opts MergeOptions, log *transactionLog) error {
	summary, err := gitStatusSummary(ctx, info.Path)
	if err != nil {
		return mergeFailure("diff summary", info, log, err)
	}

	if summary.empty() {
		if appendErr := log.append("diff summary", "clean"); appendErr != nil {
			return mergeFailure("write transaction log", info, log, appendErr)
		}

		return nil
	}

	if appendErr := log.append("diff summary", summary.String()); appendErr != nil {
		return mergeFailure("write transaction log", info, log, appendErr)
	}

	if !opts.AutoCommit {
		err := fmt.Errorf("worktree has uncommitted changes:\n%s", summary.String())

		return mergeFailure("auto-commit policy", info, log, err)
	}

	if err := autoCommit(ctx, info, summary, opts); err != nil {
		return mergeFailure("auto-commit", info, log, err)
	}

	if appendErr := log.append("auto-commit", "ok"); appendErr != nil {
		return mergeFailure("write transaction log", info, log, appendErr)
	}

	return nil
}

func mergeBranch(ctx context.Context, repoRoot string, info *Info, log *transactionLog) error {
	mergeMsg := "atteler: merge session " + info.SessionID

	detail := fmt.Sprintf("git merge --no-ff %s into %s", info.Branch, info.BaseBranch)
	if appendErr := log.append("merge branch", detail); appendErr != nil {
		return mergeFailure("write transaction log", info, log, appendErr)
	}

	if err := gitRun(ctx, repoRoot, "merge", "--no-ff", "-m", mergeMsg, info.Branch); err != nil {
		rolledBack, rollbackErr := rollbackFailedMerge(ctx, repoRoot, log)
		if rollbackErr != nil {
			err = errors.Join(err, fmt.Errorf("rollback failed: %w", rollbackErr))
		}

		return mergeFailureWithRollback("merge branch", info, log, err, rolledBack)
	}

	if appendErr := log.append("merge branch", "ok"); appendErr != nil {
		return mergeFailure("write transaction log", info, log, appendErr)
	}

	return nil
}

func cleanupMergedWorktree(ctx context.Context, repoDir string, info *Info, log *transactionLog) error {
	if appendErr := log.append("cleanup worktree", "start"); appendErr != nil {
		return mergeFailure("write transaction log", info, log, appendErr)
	}

	if err := RemoveContext(ctx, repoDir, info); err != nil {
		return mergeFailure("cleanup worktree", info, log, err)
	}

	if appendErr := log.append("cleanup worktree", "ok"); appendErr != nil {
		return mergeFailure("write transaction log", info, log, appendErr)
	}

	return nil
}

// autoCommit stages and commits any dirty files in the worktree.
func autoCommit(ctx context.Context, info *Info, summary statusSummary, opts MergeOptions) error {
	ctx = nonNilCommandContext(ctx)

	if summary.empty() {
		return nil // nothing to commit
	}

	if err := gitRun(ctx, info.Path, "add", "-A"); err != nil {
		return fmt.Errorf("stage changes: %w", err)
	}

	body := []string{
		"Session: " + info.SessionID,
		"Branch: " + info.Branch,
		"Base: " + info.BaseBranch,
		"Committed-at: " + time.Now().UTC().Format(time.RFC3339),
	}

	for _, provenance := range opts.Provenance {
		provenance = strings.TrimSpace(provenance)
		if provenance != "" {
			body = append(body, "Provenance: "+provenance)
		}
	}

	body = append(body, "", "Changed files:")

	for _, line := range summary.lines {
		body = append(body, "- "+line)
	}

	msg := "atteler: auto-commit session " + info.SessionID

	return gitRun(ctx, info.Path, "commit", "-m", msg, "-m", strings.Join(body, "\n"))
}

type statusSummary struct {
	lines []string
}

func (s statusSummary) empty() bool {
	return len(s.lines) == 0
}

func (s statusSummary) String() string {
	return strings.Join(s.lines, "\n")
}

func gitStatusSummary(ctx context.Context, dir string) (statusSummary, error) {
	out, err := gitOutput(ctx, dir, "status", "--porcelain")
	if err != nil {
		return statusSummary{}, err
	}

	var lines []string

	for line := range strings.SplitSeq(out, "\n") {
		line = strings.TrimRight(line, "\r")
		if strings.TrimSpace(line) == "" {
			continue
		}

		lines = append(lines, line)
	}

	return statusSummary{lines: lines}, nil
}

type transactionLog struct {
	path string
}

func newTransactionLog(ctx context.Context, repoRoot, sessionID string) (*transactionLog, error) {
	dir, err := gitPath(ctx, repoRoot, transactionLogGitDir)
	if err != nil {
		return nil, err
	}

	if mkdirErr := os.MkdirAll(dir, 0o700); mkdirErr != nil {
		return nil, fmt.Errorf("create transaction log dir %s: %w", dir, mkdirErr)
	}

	name := fmt.Sprintf("%s-%s.log", sessionID, time.Now().UTC().Format("20060102T150405.000000000Z"))
	path := filepath.Join(dir, name)

	file, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return nil, fmt.Errorf("create transaction log %s: %w", path, err)
	}
	defer file.Close()

	if _, err := fmt.Fprintf(file, "# atteler worktree merge transaction\n"); err != nil {
		return nil, fmt.Errorf("write transaction log header %s: %w", path, err)
	}

	return &transactionLog{path: path}, nil
}

func (l *transactionLog) append(step, detail string) error {
	if l == nil {
		return nil
	}

	file, err := os.OpenFile(l.path, os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return fmt.Errorf("open transaction log %s: %w", l.path, err)
	}
	defer file.Close()

	for line := range strings.SplitSeq(detail, "\n") {
		if _, err := fmt.Fprintf(file, "%s\t%s\t%s\n", time.Now().UTC().Format(time.RFC3339Nano), step, line); err != nil {
			return fmt.Errorf("append transaction log %s: %w", l.path, err)
		}
	}

	return nil
}

func mergeFailure(step string, info *Info, log *transactionLog, err error) error {
	return mergeFailureWithRollback(step, info, log, err, false)
}

func mergeFailureWithRollback(step string, info *Info, log *transactionLog, err error, rolledBack bool) error {
	if log != nil {
		if appendErr := log.append("failed: "+step, err.Error()); appendErr != nil {
			err = errors.Join(err, fmt.Errorf("write transaction log: %w", appendErr))
		}
	}

	return &MergeError{
		Err:            err,
		Step:           step,
		Branch:         info.Branch,
		WorktreePath:   info.Path,
		TransactionLog: transactionLogPath(log),
		RolledBack:     rolledBack,
	}
}

func transactionLogPath(log *transactionLog) string {
	if log == nil {
		return ""
	}

	return log.path
}

func rollbackFailedMerge(ctx context.Context, repoRoot string, log *transactionLog) (bool, error) {
	pending, err := gitPendingOperation(ctx, repoRoot)
	if err != nil {
		return false, err
	}

	if pending != "merge" {
		return false, nil
	}

	if err := log.append("rollback", "git merge --abort"); err != nil {
		return false, err
	}

	if err := gitRun(ctx, repoRoot, "merge", "--abort"); err != nil {
		return false, err
	}

	if err := log.append("rollback", "ok"); err != nil {
		return true, err
	}

	return true, nil
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

func gitPath(ctx context.Context, dir, name string) (string, error) {
	out, err := gitOutput(nonNilCommandContext(ctx), dir, "rev-parse", "--git-path", name)
	if err != nil {
		return "", err
	}

	path := strings.TrimSpace(out)
	if filepath.IsAbs(path) {
		return path, nil
	}

	return filepath.Join(dir, path), nil
}

func gitPendingOperation(ctx context.Context, dir string) (string, error) {
	checks := []struct {
		name string
		path string
	}{
		{name: "merge", path: "MERGE_HEAD"},
		{name: "cherry-pick", path: "CHERRY_PICK_HEAD"},
		{name: "revert", path: "REVERT_HEAD"},
		{name: "rebase", path: "rebase-merge"},
		{name: "rebase", path: "rebase-apply"},
	}

	for _, check := range checks {
		path, err := gitPath(ctx, dir, check.path)
		if err != nil {
			return "", err
		}

		exists, err := pathExists(path)
		if err != nil {
			return "", err
		}

		if exists {
			return check.name, nil
		}
	}

	return "", nil
}

func gitBranchExists(ctx context.Context, repoRoot, branch string) (bool, error) {
	out, err := gitOutput(ctx, repoRoot, "branch", "--list", branch)
	if err != nil {
		return false, err
	}

	return strings.TrimSpace(out) != "", nil
}

func gitWorktreePathForBranch(ctx context.Context, repoRoot, branch string) (path string, ok bool, err error) {
	out, err := gitOutput(ctx, repoRoot, "worktree", "list", "--porcelain")
	if err != nil {
		return "", false, err
	}

	var currentPath string

	for line := range strings.SplitSeq(out, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			currentPath = ""
			continue
		}

		if path, ok := strings.CutPrefix(line, "worktree "); ok {
			currentPath = path
			continue
		}

		if currentBranch, ok := strings.CutPrefix(line, "branch refs/heads/"); ok && currentBranch == branch {
			return currentPath, true, nil
		}
	}

	return "", false, nil
}

func pathExists(path string) (bool, error) {
	_, err := os.Stat(path)
	if err == nil {
		return true, nil
	}

	if os.IsNotExist(err) {
		return false, nil
	}

	return false, fmt.Errorf("stat %s: %w", path, err)
}

func samePath(left, right string) bool {
	leftAbs, leftErr := filepath.Abs(left)
	rightAbs, rightErr := filepath.Abs(right)

	if leftErr == nil {
		left = leftAbs
	}

	if rightErr == nil {
		right = rightAbs
	}

	return filepath.Clean(left) == filepath.Clean(right)
}

func printable(value string) string {
	if value == "" {
		return "<unknown>"
	}

	return value
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

	var (
		results []Info
		current Info
	)

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
