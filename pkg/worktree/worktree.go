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
	worktreeManifestDir  = "atteler/worktrees"
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

// MergeStrategy is the explicit strategy used to bring a session branch back.
type MergeStrategy string

const (
	// MergeStrategyMerge uses git merge --no-ff after a dry-run merge check.
	MergeStrategyMerge MergeStrategy = "merge"
)

// MergeOptions is the explicit safety policy for merging a session worktree.
type MergeOptions struct {
	// Strategy is the explicit merge-back strategy. Use MergeStrategyMerge for
	// the currently supported reviewed merge transaction.
	Strategy MergeStrategy
	// Provenance adds optional caller-provided context to auto-commit messages.
	Provenance []string
	// AutoCommit permits atteler to stage and commit dirty worktree files before
	// merging. If false, MergeWithOptionsContext refuses dirty worktrees.
	//
	// AutoCommit only takes effect when ReviewedAutoCommit is also true. This
	// keeps legacy callers from silently manufacturing unreviewed commits.
	AutoCommit bool
	// ReviewedAutoCommit confirms the caller has reviewed the worktree diff and
	// intentionally permits atteler to create the generated session commit.
	ReviewedAutoCommit bool
	// AutoMerge permits atteler to run git merge in the base repository. This
	// makes auto-merge call sites opt in instead of reaching the merge path by
	// accident.
	AutoMerge bool
	// AllowBaseBranchMismatch permits merging even when the main worktree is not
	// checked out to Info.BaseBranch. Keep false for normal session finalization.
	AllowBaseBranchMismatch bool
}

// RemoveOptions controls destructive cleanup of a session worktree.
type RemoveOptions struct {
	// Force permits removing dirty or unmerged worktrees and force-deleting the
	// branch. Keep false for normal cleanup so failed transactions remain
	// recoverable.
	Force bool
}

// MergeError describes a failed merge transaction and how to recover it.
type MergeError struct {
	Err            error
	Step           string
	Branch         string
	BaseBranch     string
	SessionID      string
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

	if e.WorktreePath != "" {
		b.WriteString("\nrecovery: inspect with: git -C " + e.WorktreePath + " status --short")
	}

	if e.BaseBranch != "" && e.Branch != "" {
		b.WriteString("\nrecovery: review diff with: git diff --stat " + e.BaseBranch + "..." + e.Branch)
	}

	if e.SessionID != "" {
		b.WriteString("\nrecovery: retry with: atteler --merge-worktree " + e.SessionID)
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

	plan, err := planCreateWorktree(ctx, repoDir, sessionID)
	if err != nil {
		return nil, err
	}

	if plan.hasExistingState() {
		return joinOrResumeExistingWorktree(ctx, plan)
	}

	return createNewWorktree(ctx, plan)
}

type createPlan struct {
	manifest      *worktreeManifest
	repoRoot      string
	sessionID     string
	branch        string
	baseBranch    string
	baseHEAD      string
	wtDir         string
	manifestFound bool
	branchExists  bool
	wtPathExists  bool
}

func (p createPlan) hasExistingState() bool {
	return p.branchExists || p.wtPathExists || p.manifestFound
}

func planCreateWorktree(ctx context.Context, repoDir, sessionID string) (createPlan, error) {
	branch, err := branchForSessionID(sessionID)
	if err != nil {
		return createPlan{}, err
	}

	repoRoot, err := gitRepoRoot(ctx, repoDir)
	if err != nil {
		return createPlan{}, fmt.Errorf("worktree: locate repo root: %w", err)
	}

	baseBranch, err := gitCurrentBranch(ctx, repoRoot)
	if err != nil {
		return createPlan{}, fmt.Errorf("worktree: detect current branch: %w", err)
	}

	baseHEAD, err := gitRevParse(ctx, repoRoot, "HEAD")
	if err != nil {
		return createPlan{}, fmt.Errorf("worktree: detect current HEAD: %w", err)
	}

	wtDir := worktreeDir(repoRoot, sessionID)

	manifest, manifestExists, err := loadWorktreeManifest(ctx, repoRoot, sessionID)
	if err != nil {
		return createPlan{}, fmt.Errorf("worktree: load ownership manifest: %w", err)
	}

	if manifestExists {
		validateErr := validateManifestOwnership(repoRoot, manifest, sessionID, branch, wtDir)
		if validateErr != nil {
			return createPlan{}, fmt.Errorf("worktree: invalid ownership manifest: %w", validateErr)
		}
	}

	branchExists, err := gitBranchExists(ctx, repoRoot, branch)
	if err != nil {
		return createPlan{}, fmt.Errorf("worktree: check branch %s: %w", branch, err)
	}

	wtPathExists, err := pathExists(wtDir)
	if err != nil {
		return createPlan{}, fmt.Errorf("worktree: check path %s: %w", wtDir, err)
	}

	// Dirty base worktrees are only unsafe before creating new ownership.
	// Existing manifests make Create idempotent: rejoin/resume uses the
	// recorded BaseHEAD instead of guessing from the caller's current dirt.
	if !manifestExists && !branchExists && !wtPathExists {
		if baseBranch == "HEAD" {
			return createPlan{}, errors.New("worktree: cannot create worktree from detached HEAD")
		}

		preflightErr := preflightCreateCleanState(ctx, repoRoot, wtDir)
		if preflightErr != nil {
			return createPlan{}, preflightErr
		}
	}

	return createPlan{
		manifest:      manifest,
		repoRoot:      repoRoot,
		sessionID:     sessionID,
		branch:        branch,
		baseBranch:    baseBranch,
		baseHEAD:      baseHEAD,
		wtDir:         wtDir,
		manifestFound: manifestExists,
		branchExists:  branchExists,
		wtPathExists:  wtPathExists,
	}, nil
}

func createNewWorktree(ctx context.Context, plan createPlan) (*Info, error) {
	created := newWorktreeManifest(plan.sessionID, plan.branch, plan.baseBranch, plan.baseHEAD, plan.repoRoot, plan.wtDir)

	err := writeWorktreeManifest(ctx, plan.repoRoot, &created, "create-preflight", "recorded ownership before branch creation")
	if err != nil {
		return nil, fmt.Errorf("worktree: write ownership manifest: %w", err)
	}

	if err := gitRun(ctx, plan.repoRoot, "branch", plan.branch, plan.baseHEAD); err != nil {
		cause := fmt.Errorf("worktree: create branch %s: %w", plan.branch, err)
		return nil, markManifestFailed(ctx, plan.repoRoot, &created, "create-branch-failed", cause)
	}

	created.State = manifestStateBranchCreated
	if err := writeWorktreeManifest(ctx, plan.repoRoot, &created, "branch-created", plan.branch); err != nil {
		return nil, rollbackCreatedBranch(ctx, plan.repoRoot, plan.branch, fmt.Errorf("worktree: update ownership manifest: %w", err))
	}

	if err := gitRun(ctx, plan.repoRoot, "worktree", "add", plan.wtDir, plan.branch); err != nil {
		addErr := fmt.Errorf("worktree: add %s: %w", plan.wtDir, err)
		manifestErr := markManifestFailed(ctx, plan.repoRoot, &created, "worktree-add-failed", addErr)

		return nil, rollbackCreatedBranch(ctx, plan.repoRoot, plan.branch, manifestErr)
	}

	created.State = manifestStateActive
	if err := writeWorktreeManifest(ctx, plan.repoRoot, &created, "active", plan.wtDir); err != nil {
		return nil, fmt.Errorf("worktree: update ownership manifest: %w", err)
	}

	return manifestInfo(&created), nil
}

// Merge refuses to merge without an explicit MergeOptions policy.
//
// Use MergeWithOptionsContext when a caller intentionally permits reviewed
// auto-commit and auto-merge behavior.
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

	repoRoot, manifest, log, err := beginMergeTransaction(ctx, repoDir, info, opts)
	if err != nil {
		return err
	}

	if err := runMergeTransaction(ctx, repoRoot, info, opts, manifest, log); err != nil {
		if markErr := markMergeFailed(ctx, repoRoot, manifest, err); markErr != nil {
			return errors.Join(err, markErr)
		}

		return err
	}

	return completeMergeTransaction(ctx, repoRoot, info, manifest, log)
}

func runMergeTransaction(
	ctx context.Context,
	repoRoot string,
	info *Info,
	opts MergeOptions,
	manifest *worktreeManifest,
	log *transactionLog,
) error {
	if policyErr := requireAutoMergePolicy(opts); policyErr != nil {
		return mergeFailure("auto-merge policy", info, log, policyErr)
	}

	if preflightErr := preflightMainRepo(ctx, repoRoot, info, opts, manifest); preflightErr != nil {
		return mergeFailure("preflight main repository", info, log, preflightErr)
	}

	if appendErr := log.append("preflight main repository", "ok"); appendErr != nil {
		return mergeFailure("write transaction log", info, log, appendErr)
	}

	if commitErr := autoCommitIfNeeded(ctx, info, opts, log); commitErr != nil {
		return commitErr
	}

	if dryRunErr := dryRunMerge(ctx, repoRoot, info, log); dryRunErr != nil {
		return dryRunErr
	}

	if mergeErr := mergeBranch(ctx, repoRoot, info, log, manifest); mergeErr != nil {
		return mergeErr
	}

	if cleanupErr := cleanupMergedWorktree(ctx, repoRoot, info, log, manifest); cleanupErr != nil {
		return cleanupErr
	}

	return nil
}

func beginMergeTransaction(
	ctx context.Context,
	repoDir string,
	info *Info,
	opts MergeOptions,
) (string, *worktreeManifest, *transactionLog, error) {
	if info == nil {
		return "", nil, nil, errors.New("worktree: nil info")
	}

	if err := validateInfo(info, true); err != nil {
		return "", nil, nil, err
	}

	repoRoot, rootErr := gitRepoRoot(ctx, repoDir)
	if rootErr != nil {
		return "", nil, nil, fmt.Errorf("worktree: locate repo root: %w", rootErr)
	}

	manifest, manifestErr := requireOwnedManifest(ctx, repoRoot, info)
	if manifestErr != nil {
		return "", nil, nil, manifestErr
	}

	log, startErr := startMergeTransaction(ctx, repoRoot, info, opts)
	if startErr != nil {
		return "", nil, nil, startErr
	}

	manifest.State = manifestStateMerging
	manifest.LastTransaction = transactionLogPath(log)
	manifest.LastError = ""

	writeErr := writeWorktreeManifest(ctx, repoRoot, manifest, "merge-start", "transaction="+transactionLogPath(log))
	if writeErr != nil {
		return "", nil, nil, mergeFailure("write ownership manifest", info, log, writeErr)
	}

	return repoRoot, manifest, log, nil
}

func markMergeFailed(ctx context.Context, repoRoot string, manifest *worktreeManifest, cause error) error {
	if manifest == nil || cause == nil {
		return nil
	}

	manifest.State = manifestStateFailed
	manifest.LastError = cause.Error()

	return writeWorktreeManifest(ctx, repoRoot, manifest, "merge-failed", cause.Error())
}

func completeMergeTransaction(
	ctx context.Context,
	repoRoot string,
	info *Info,
	manifest *worktreeManifest,
	log *transactionLog,
) error {
	manifest.State = manifestStateMerged
	manifest.LastError = ""

	if writeErr := writeWorktreeManifest(ctx, repoRoot, manifest, "merge-complete", "merged and cleaned up"); writeErr != nil {
		return mergeFailure("write ownership manifest", info, log, writeErr)
	}

	if appendErr := log.append("complete", "ok"); appendErr != nil {
		return mergeFailure("write transaction log", info, log, appendErr)
	}

	return nil
}

// Remove deletes a clean, already-merged worktree and its branch.
//
// Use RemoveWithOptionsContext with Force=true for explicitly destructive
// cleanup of dirty or unmerged session work.
func Remove(repoDir string, info *Info) error {
	return RemoveContext(defaultCommandContext(), repoDir, info)
}

// RemoveContext is Remove with caller-provided cancellation for git commands.
func RemoveContext(ctx context.Context, repoDir string, info *Info) error {
	return RemoveWithOptionsContext(ctx, repoDir, info, RemoveOptions{})
}

// RemoveWithOptionsContext removes a session worktree with an explicit
// destructive-cleanup policy.
func RemoveWithOptionsContext(ctx context.Context, repoDir string, info *Info, opts RemoveOptions) error {
	ctx = nonNilCommandContext(ctx)

	plan, err := prepareRemove(ctx, repoDir, info, opts)
	if err != nil {
		return err
	}

	if plan.alreadyRemoved {
		return nil
	}

	if err := removeWorktreePath(ctx, plan.repoRoot, info, opts); err != nil {
		return err
	}

	if err := deleteWorktreeBranch(ctx, plan.repoRoot, info, opts); err != nil {
		return err
	}

	if err := verifyWorktreeRemoved(ctx, plan.repoRoot, info); err != nil {
		return err
	}

	return markManifestRemoved(ctx, plan.repoRoot, info)
}

type removePlan struct {
	repoRoot       string
	alreadyRemoved bool
}

func prepareRemove(ctx context.Context, repoDir string, info *Info, opts RemoveOptions) (removePlan, error) {
	if info == nil {
		return removePlan{}, errors.New("worktree: nil info")
	}

	if err := validateInfo(info, false); err != nil {
		return removePlan{}, err
	}

	repoRoot, rootErr := gitRepoRoot(ctx, repoDir)
	if rootErr != nil {
		return removePlan{}, fmt.Errorf("worktree: locate repo root: %w", rootErr)
	}

	if !opts.Force {
		if _, err := requireOwnedManifest(ctx, repoRoot, info); err != nil {
			return removePlan{}, err
		}
	}

	gitStateRemoved, gitStateErr := removedGitState(ctx, repoRoot, info)
	if gitStateErr != nil {
		return removePlan{}, gitStateErr
	}

	if gitStateRemoved {
		if err := markManifestRemoved(ctx, repoRoot, info); err != nil {
			return removePlan{}, err
		}

		return removePlan{repoRoot: repoRoot, alreadyRemoved: true}, nil
	}

	removed, removedErr := alreadyRemoved(ctx, repoRoot, info)
	if removedErr != nil {
		return removePlan{}, removedErr
	}

	if removed {
		return removePlan{repoRoot: repoRoot, alreadyRemoved: true}, nil
	}

	if !opts.Force {
		if err := preflightSafeRemove(ctx, repoRoot, info); err != nil {
			return removePlan{}, err
		}
	}

	return removePlan{repoRoot: repoRoot}, nil
}

func removeWorktreePath(ctx context.Context, repoRoot string, info *Info, opts RemoveOptions) error {
	exists, existsErr := pathExists(info.Path)
	if existsErr != nil {
		return fmt.Errorf("worktree: check path %s before remove: %w", info.Path, existsErr)
	}

	if !exists {
		if pruneErr := gitRun(ctx, repoRoot, "worktree", "prune"); pruneErr != nil {
			return fmt.Errorf("worktree: prune: %w", pruneErr)
		}

		return nil
	}

	removeArgs := []string{"worktree", "remove"}
	if opts.Force {
		removeArgs = append(removeArgs, "--force")
	}

	removeArgs = append(removeArgs, info.Path)
	if removeErr := gitRun(ctx, repoRoot, removeArgs...); removeErr != nil {
		return fmt.Errorf("worktree: remove path %s: %w", info.Path, removeErr)
	}

	if pruneErr := gitRun(ctx, repoRoot, "worktree", "prune"); pruneErr != nil {
		return fmt.Errorf("worktree: prune: %w", pruneErr)
	}

	return nil
}

func removedGitState(ctx context.Context, repoRoot string, info *Info) (bool, error) {
	pathExists, pathErr := pathExists(info.Path)
	if pathErr != nil {
		return false, fmt.Errorf("worktree: check path %s: %w", info.Path, pathErr)
	}

	branchExists, branchErr := gitBranchExists(ctx, repoRoot, info.Branch)
	if branchErr != nil {
		return false, fmt.Errorf("worktree: check branch %s: %w", info.Branch, branchErr)
	}

	return !pathExists && !branchExists, nil
}

func alreadyRemoved(ctx context.Context, repoRoot string, info *Info) (bool, error) {
	manifest, ok, err := loadWorktreeManifest(ctx, repoRoot, info.SessionID)
	if err != nil {
		return false, fmt.Errorf("worktree: load ownership manifest: %w", err)
	}

	if !ok || (manifest.State != manifestStateRemoved && manifest.State != manifestStateMerged) {
		return false, nil
	}

	pathExists, pathErr := pathExists(info.Path)
	if pathErr != nil {
		return false, fmt.Errorf("worktree: check path %s: %w", info.Path, pathErr)
	}

	branchExists, branchErr := gitBranchExists(ctx, repoRoot, info.Branch)
	if branchErr != nil {
		return false, fmt.Errorf("worktree: check branch %s: %w", info.Branch, branchErr)
	}

	return !pathExists && !branchExists, nil
}

func markManifestRemoved(ctx context.Context, repoRoot string, info *Info) error {
	manifest, ok, err := loadWorktreeManifest(ctx, repoRoot, info.SessionID)
	if err != nil {
		return fmt.Errorf("worktree: load ownership manifest: %w", err)
	}

	if !ok {
		return nil
	}

	manifest.State = manifestStateRemoved
	manifest.LastError = ""

	if err := writeWorktreeManifest(ctx, repoRoot, manifest, "remove-complete", "removed worktree and branch"); err != nil {
		return fmt.Errorf("worktree: update ownership manifest: %w", err)
	}

	return nil
}

func deleteWorktreeBranch(ctx context.Context, repoRoot string, info *Info, opts RemoveOptions) error {
	exists, existsErr := gitBranchExists(ctx, repoRoot, info.Branch)
	if existsErr != nil {
		return fmt.Errorf("worktree: check branch %s before delete: %w", info.Branch, existsErr)
	}

	if !exists {
		return nil
	}

	branchDeleteFlag := "-d"
	if opts.Force {
		branchDeleteFlag = "-D"
	}

	if branchErr := gitRun(ctx, repoRoot, "branch", branchDeleteFlag, info.Branch); branchErr != nil {
		return fmt.Errorf("worktree: delete branch %s: %w", info.Branch, branchErr)
	}

	return nil
}

func verifyWorktreeRemoved(ctx context.Context, repoRoot string, info *Info) error {
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

	if opts.Strategy == "" {
		return errors.New("merge policy must choose an explicit strategy")
	}

	if opts.Strategy != MergeStrategyMerge {
		return fmt.Errorf("merge strategy %q is not supported", opts.Strategy)
	}

	return nil
}

func rollbackCreatedBranch(ctx context.Context, repoRoot, branch string, cause error) error {
	if err := gitRun(ctx, repoRoot, "branch", "-D", branch); err != nil {
		return errors.Join(cause, fmt.Errorf("rollback created branch %s: %w", branch, err))
	}

	return cause
}

func preflightCreateCleanState(ctx context.Context, repoRoot, wtDir string) error {
	excludes, err := managedWorktreePaths(ctx, repoRoot)
	if err != nil {
		return fmt.Errorf("worktree: list managed worktrees for create preflight: %w", err)
	}

	excludes = append(excludes, wtDir)

	summary, err := gitStatusSummary(ctx, repoRoot, excludes...)
	if err != nil {
		return fmt.Errorf("worktree: read main worktree status: %w", err)
	}

	if !summary.empty() {
		return fmt.Errorf("worktree: main worktree has uncommitted or untracked files before isolation:\n%s", summary.String())
	}

	return nil
}

func markManifestFailed(ctx context.Context, repoRoot string, manifest *worktreeManifest, event string, cause error) error {
	manifest.State = manifestStateFailed
	manifest.LastError = cause.Error()

	writeErr := writeWorktreeManifest(ctx, repoRoot, manifest, event, cause.Error())
	if writeErr != nil {
		return errors.Join(cause, fmt.Errorf("worktree: update ownership manifest: %w", writeErr))
	}

	return cause
}

func joinOrResumeExistingWorktree(ctx context.Context, plan createPlan) (*Info, error) {
	if !plan.manifestFound {
		if plan.branchExists {
			return nil, fmt.Errorf("worktree: branch %s already exists without ownership metadata", plan.branch)
		}

		return nil, fmt.Errorf("worktree: path %s already exists without ownership metadata", plan.wtDir)
	}

	if plan.manifest.State == manifestStateMerged || plan.manifest.State == manifestStateRemoved {
		return nil, fmt.Errorf("worktree: session %s was already merged; start a new session instead", plan.sessionID)
	}

	if err := preflightManifestBranch(ctx, plan); err != nil {
		return nil, err
	}

	actualPath, checkedOut, err := gitWorktreePathForBranch(ctx, plan.repoRoot, plan.branch)
	if err != nil {
		return nil, fmt.Errorf("worktree: check worktree checkout: %w", err)
	}

	if plan.branchExists && checkedOut {
		return rejoinCheckedOutWorktree(ctx, plan, actualPath)
	}

	if plan.wtPathExists {
		return nil, fmt.Errorf("worktree: path %s exists but is not the owned checkout for branch %s", plan.wtDir, plan.branch)
	}

	if !plan.branchExists {
		if err := resumeMissingBranch(ctx, plan); err != nil {
			return nil, err
		}
	}

	return resumeWorktreeCheckout(ctx, plan)
}

func preflightManifestBranch(ctx context.Context, plan createPlan) error {
	if !plan.branchExists {
		return nil
	}

	baseIsAncestor, err := gitIsAncestor(ctx, plan.repoRoot, plan.manifest.BaseHEAD, plan.branch)
	if err != nil {
		return fmt.Errorf("worktree: verify branch %s against ownership manifest: %w", plan.branch, err)
	}

	if !baseIsAncestor {
		return fmt.Errorf("worktree: branch %s does not descend from recorded base HEAD %s in ownership manifest", plan.branch, plan.manifest.BaseHEAD)
	}

	return nil
}

func rejoinCheckedOutWorktree(ctx context.Context, plan createPlan, actualPath string) (*Info, error) {
	if !samePath(actualPath, plan.wtDir) {
		return nil, fmt.Errorf("worktree: branch %s is checked out at %s, ownership manifest expects %s", plan.branch, actualPath, plan.wtDir)
	}

	if !plan.wtPathExists {
		if err := gitRun(ctx, plan.repoRoot, "worktree", "prune"); err != nil {
			cause := fmt.Errorf("worktree: prune missing checkout %s: %w", plan.wtDir, err)
			return nil, markManifestFailed(ctx, plan.repoRoot, plan.manifest, "rejoin-prune-failed", cause)
		}

		plan.manifest.State = manifestStateBranchCreated
		if err := writeWorktreeManifest(ctx, plan.repoRoot, plan.manifest, "rejoin-pruned-missing", plan.wtDir); err != nil {
			return nil, fmt.Errorf("worktree: update ownership manifest: %w", err)
		}

		return resumeWorktreeCheckout(ctx, plan)
	}

	plan.manifest.State = manifestStateActive

	err := writeWorktreeManifest(ctx, plan.repoRoot, plan.manifest, "rejoin-active", "verified existing branch and worktree")
	if err != nil {
		return nil, fmt.Errorf("worktree: update ownership manifest: %w", err)
	}

	return manifestInfo(plan.manifest), nil
}

func resumeMissingBranch(ctx context.Context, plan createPlan) error {
	if plan.manifest.State != manifestStateCreating &&
		plan.manifest.State != manifestStateBranchCreated &&
		plan.manifest.State != manifestStateFailed {
		return fmt.Errorf("worktree: ownership manifest says session %s is %s but branch %s is missing", plan.sessionID, plan.manifest.State, plan.branch)
	}

	if err := gitRun(ctx, plan.repoRoot, "branch", plan.branch, plan.manifest.BaseHEAD); err != nil {
		cause := fmt.Errorf("worktree: resume branch %s: %w", plan.branch, err)
		return markManifestFailed(ctx, plan.repoRoot, plan.manifest, "resume-branch-failed", cause)
	}

	plan.manifest.State = manifestStateBranchCreated
	if err := writeWorktreeManifest(ctx, plan.repoRoot, plan.manifest, "resume-branch-created", plan.branch); err != nil {
		return rollbackCreatedBranch(ctx, plan.repoRoot, plan.branch, fmt.Errorf("worktree: update ownership manifest: %w", err))
	}

	return nil
}

func resumeWorktreeCheckout(ctx context.Context, plan createPlan) (*Info, error) {
	if err := gitRun(ctx, plan.repoRoot, "worktree", "add", plan.wtDir, plan.branch); err != nil {
		cause := fmt.Errorf("worktree: resume worktree %s: %w", plan.wtDir, err)
		return nil, markManifestFailed(ctx, plan.repoRoot, plan.manifest, "resume-worktree-add-failed", cause)
	}

	plan.manifest.State = manifestStateActive
	plan.manifest.LastError = ""

	if err := writeWorktreeManifest(ctx, plan.repoRoot, plan.manifest, "resume-active", plan.wtDir); err != nil {
		return nil, fmt.Errorf("worktree: update ownership manifest: %w", err)
	}

	return manifestInfo(plan.manifest), nil
}

func preflightSafeRemove(ctx context.Context, repoRoot string, info *Info) error {
	exists, existsErr := pathExists(info.Path)
	if existsErr != nil {
		return fmt.Errorf("worktree: check path %s before remove: %w", info.Path, existsErr)
	}

	if exists {
		summary, err := gitStatusSummary(ctx, info.Path)
		if err != nil {
			return fmt.Errorf("worktree: read worktree status before remove: %w", err)
		}

		if !summary.empty() {
			return fmt.Errorf("worktree: refusing to remove dirty worktree without force:\n%s", summary.String())
		}
	}

	branchExists, err := gitBranchExists(ctx, repoRoot, info.Branch)
	if err != nil {
		return fmt.Errorf("worktree: check branch %s before remove: %w", info.Branch, err)
	}

	if !branchExists {
		return fmt.Errorf("worktree: branch %s is missing before remove", info.Branch)
	}

	merged, err := gitIsAncestor(ctx, repoRoot, info.Branch, "HEAD")
	if err != nil {
		return fmt.Errorf("worktree: verify branch %s merged before remove: %w", info.Branch, err)
	}

	if !merged {
		return fmt.Errorf("worktree: refusing to remove unmerged branch %s without force", info.Branch)
	}

	return nil
}

func requireOwnedManifest(ctx context.Context, repoRoot string, info *Info) (*worktreeManifest, error) {
	manifest, ok, err := loadWorktreeManifest(ctx, repoRoot, info.SessionID)
	if err != nil {
		return nil, fmt.Errorf("worktree: load ownership manifest: %w", err)
	}

	if !ok {
		return nil, fmt.Errorf("worktree: missing ownership metadata for session %s", info.SessionID)
	}

	if err := validateManifestOwnership(repoRoot, manifest, info.SessionID, info.Branch, info.Path); err != nil {
		return nil, fmt.Errorf("worktree: invalid ownership manifest: %w", err)
	}

	if info.BaseBranch != "" && manifest.BaseBranch != info.BaseBranch {
		return nil, fmt.Errorf("worktree: ownership manifest base branch %s does not match requested base %s", manifest.BaseBranch, info.BaseBranch)
	}

	return manifest, nil
}

func preflightMainRepo(ctx context.Context, repoRoot string, info *Info, opts MergeOptions, manifest *worktreeManifest) error {
	if err := preflightCurrentBranch(ctx, repoRoot, info, opts); err != nil {
		return err
	}

	if err := preflightCleanState(ctx, repoRoot, info); err != nil {
		return err
	}

	if err := preflightWorktreeCheckout(ctx, repoRoot, info); err != nil {
		return err
	}

	if err := preflightMergeBase(ctx, repoRoot, info, manifest); err != nil {
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

func preflightCleanState(ctx context.Context, repoRoot string, info *Info) error {
	pending, err := gitPendingOperation(ctx, repoRoot)
	if err != nil {
		return fmt.Errorf("detect pending git operation: %w", err)
	}

	if pending != "" {
		return fmt.Errorf("main worktree has pending %s; finish or abort it before merging", pending)
	}

	excludes, err := managedWorktreePaths(ctx, repoRoot)
	if err != nil {
		return fmt.Errorf("list managed worktrees: %w", err)
	}

	excludes = append(excludes, info.Path)

	summary, err := gitStatusSummary(ctx, repoRoot, excludes...)
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

func preflightMergeBase(ctx context.Context, repoRoot string, info *Info, manifest *worktreeManifest) error {
	if manifest == nil {
		return errors.New("ownership manifest is required")
	}

	currentHead, err := gitRevParse(ctx, repoRoot, "HEAD")
	if err != nil {
		return fmt.Errorf("detect current HEAD: %w", err)
	}

	branchHead, err := gitRevParse(ctx, repoRoot, info.Branch)
	if err != nil {
		return fmt.Errorf("detect worktree branch HEAD: %w", err)
	}

	_, mergeBaseErr := gitMergeBase(ctx, repoRoot, currentHead, branchHead)
	if mergeBaseErr != nil {
		return fmt.Errorf("find merge base between %s and %s: %w", info.BaseBranch, info.Branch, mergeBaseErr)
	}

	baseIsAncestor, err := gitIsAncestor(ctx, repoRoot, manifest.BaseHEAD, branchHead)
	if err != nil {
		return fmt.Errorf("verify recorded base HEAD: %w", err)
	}

	if !baseIsAncestor {
		return fmt.Errorf("recorded base HEAD %s is not an ancestor of %s", manifest.BaseHEAD, info.Branch)
	}

	baseInCurrent, err := gitIsAncestor(ctx, repoRoot, manifest.BaseHEAD, currentHead)
	if err != nil {
		return fmt.Errorf("verify current HEAD contains recorded base HEAD: %w", err)
	}

	if !baseInCurrent {
		return fmt.Errorf("recorded base HEAD %s is not an ancestor of current HEAD %s; check out or restore recorded base branch %s before merging", manifest.BaseHEAD, currentHead, info.BaseBranch)
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

	if !opts.ReviewedAutoCommit {
		err := fmt.Errorf("worktree has uncommitted changes and auto-commit was not marked reviewed:\n%s", summary.String())

		return mergeFailure("auto-commit review", info, log, err)
	}

	if err := autoCommit(ctx, info, summary, opts); err != nil {
		return mergeFailure("auto-commit", info, log, err)
	}

	if appendErr := log.append("auto-commit", "ok"); appendErr != nil {
		return mergeFailure("write transaction log", info, log, appendErr)
	}

	return nil
}

func dryRunMerge(ctx context.Context, repoRoot string, info *Info, log *transactionLog) error {
	currentHead, err := gitRevParse(ctx, repoRoot, "HEAD")
	if err != nil {
		return mergeFailure("dry-run merge", info, log, fmt.Errorf("detect current HEAD: %w", err))
	}

	branchHead, err := gitRevParse(ctx, repoRoot, info.Branch)
	if err != nil {
		return mergeFailure("dry-run merge", info, log, fmt.Errorf("detect worktree branch HEAD: %w", err))
	}

	mergeBase, err := gitMergeBase(ctx, repoRoot, currentHead, branchHead)
	if err != nil {
		return mergeFailure("dry-run merge", info, log, fmt.Errorf("find merge base: %w", err))
	}

	diffSummary, err := gitDiffSummary(ctx, repoRoot, currentHead, info.Branch)
	if err != nil {
		return mergeFailure("dry-run merge", info, log, fmt.Errorf("build diff summary: %w", err))
	}

	if strings.TrimSpace(diffSummary) == "" {
		diffSummary = "no file changes"
	}

	detail := fmt.Sprintf("strategy=%s base=%s current=%s branch=%s\n%s", MergeStrategyMerge, mergeBase, currentHead, branchHead, strings.TrimSpace(diffSummary))
	if appendErr := log.append("dry-run merge", detail); appendErr != nil {
		return mergeFailure("write transaction log", info, log, appendErr)
	}

	output, err := gitCombinedOutput(ctx, repoRoot, "merge-tree", "--write-tree", currentHead, info.Branch)
	if err != nil {
		report := strings.TrimSpace(output)
		if report == "" {
			report = err.Error()
		}

		return mergeFailure("dry-run merge", info, log, fmt.Errorf("merge dry-run reported conflicts:\n%s", report))
	}

	if appendErr := log.append("dry-run merge", "ok"); appendErr != nil {
		return mergeFailure("write transaction log", info, log, appendErr)
	}

	return nil
}

func mergeBranch(ctx context.Context, repoRoot string, info *Info, log *transactionLog, manifest *worktreeManifest) error {
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

	merged, verifyErr := gitIsAncestor(ctx, repoRoot, info.Branch, "HEAD")
	if verifyErr != nil {
		return mergeFailure("verify merge", info, log, verifyErr)
	}

	if !merged {
		return mergeFailure("verify merge", info, log, fmt.Errorf("branch %s is not an ancestor of HEAD after merge", info.Branch))
	}

	mergeHead, headErr := gitRevParse(ctx, repoRoot, "HEAD")
	if headErr != nil {
		return mergeFailure("verify merge", info, log, fmt.Errorf("detect merged HEAD: %w", headErr))
	}

	if manifest != nil {
		manifest.LastError = ""
		if writeErr := writeWorktreeManifest(ctx, repoRoot, manifest, "merge-verified", "head="+mergeHead); writeErr != nil {
			return mergeFailure("write ownership manifest", info, log, writeErr)
		}
	}

	if appendErr := log.append("merge branch", "ok"); appendErr != nil {
		return mergeFailure("write transaction log", info, log, appendErr)
	}

	return nil
}

func cleanupMergedWorktree(ctx context.Context, repoRoot string, info *Info, log *transactionLog, manifest *worktreeManifest) error {
	if appendErr := log.append("cleanup worktree", "start"); appendErr != nil {
		return mergeFailure("write transaction log", info, log, appendErr)
	}

	if err := RemoveContext(ctx, repoRoot, info); err != nil {
		return mergeFailure("cleanup worktree", info, log, err)
	}

	if manifest != nil {
		manifest.State = manifestStateMerged
		if writeErr := writeWorktreeManifest(ctx, repoRoot, manifest, "cleanup-complete", "removed worktree and branch"); writeErr != nil {
			return mergeFailure("write ownership manifest", info, log, writeErr)
		}
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

func gitStatusSummary(ctx context.Context, dir string, excludePaths ...string) (statusSummary, error) {
	args := []string{"status", "--porcelain", "--untracked-files=all"}
	if pathspecs := statusPathspecs(dir, excludePaths); len(pathspecs) > 0 {
		args = append(args, "--")
		args = append(args, pathspecs...)
	}

	out, err := gitOutput(ctx, dir, args...)
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

func statusPathspecs(repoRoot string, excludePaths []string) []string {
	var pathspecs []string

	for _, path := range excludePaths {
		rel, ok := statusRelativePath(repoRoot, path)
		if !ok {
			continue
		}

		if len(pathspecs) == 0 {
			pathspecs = append(pathspecs, ".")
		}

		pathspecs = append(pathspecs, ":(exclude,literal)"+rel)
	}

	return pathspecs
}

func statusRelativePath(repoRoot, path string) (string, bool) {
	rel, err := filepath.Rel(repoRoot, path)
	if err != nil {
		return "", false
	}

	rel = filepath.ToSlash(filepath.Clean(rel))
	if rel == "." || rel == ".." || strings.HasPrefix(rel, "../") {
		return "", false
	}

	return rel, true
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
		BaseBranch:     info.BaseBranch,
		SessionID:      info.SessionID,
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

func gitRevParse(ctx context.Context, dir, rev string) (string, error) {
	out, err := gitOutput(nonNilCommandContext(ctx), dir, "rev-parse", "--verify", rev)
	if err != nil {
		return "", err
	}

	return strings.TrimSpace(out), nil
}

func gitMergeBase(ctx context.Context, dir, left, right string) (string, error) {
	out, err := gitOutput(nonNilCommandContext(ctx), dir, "merge-base", left, right)
	if err != nil {
		return "", err
	}

	return strings.TrimSpace(out), nil
}

func gitIsAncestor(ctx context.Context, dir, ancestor, descendant string) (bool, error) {
	cmd := exec.CommandContext(nonNilCommandContext(ctx), "git", "merge-base", "--is-ancestor", ancestor, descendant)
	cmd.Dir = dir

	var stderr bytes.Buffer

	cmd.Stderr = &stderr

	if err := cmd.Run(); err != nil {
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) && exitErr.ExitCode() == 1 {
			return false, nil
		}

		return false, fmt.Errorf("git merge-base --is-ancestor %s %s: %s: %w", ancestor, descendant, strings.TrimSpace(stderr.String()), err)
	}

	return true, nil
}

func gitDiffSummary(ctx context.Context, dir, base, branch string) (string, error) {
	return gitOutput(nonNilCommandContext(ctx), dir, "diff", "--stat", "--find-renames", base+"..."+branch)
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

func managedWorktreePaths(ctx context.Context, repoRoot string) ([]string, error) {
	out, err := gitOutput(ctx, repoRoot, "worktree", "list", "--porcelain")
	if err != nil {
		return nil, err
	}

	var (
		paths       []string
		currentPath string
	)

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

		if branch, ok := strings.CutPrefix(line, "branch refs/heads/"); ok && strings.HasPrefix(branch, worktreeBranchPrefix) && currentPath != "" {
			paths = append(paths, currentPath)
		}
	}

	return paths, nil
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

func gitCombinedOutput(ctx context.Context, dir string, args ...string) (string, error) {
	cmd := exec.CommandContext(nonNilCommandContext(ctx), "git", args...)
	cmd.Dir = dir

	out, err := cmd.CombinedOutput()
	if err != nil {
		return string(out), fmt.Errorf("git %s: %w", strings.Join(args, " "), err)
	}

	return string(out), nil
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

	results := parseWorktreeListEntries(output)
	hydrateWorktreeListFromManifests(ctx, repoRoot, results)

	return results
}

func parseWorktreeListEntries(output string) []Info {
	var (
		results []Info
		current Info
	)

	for line := range strings.SplitSeq(output, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			results = appendAttelerWorktree(results, current)

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
	results = appendAttelerWorktree(results, current)

	return results
}

func appendAttelerWorktree(results []Info, current Info) []Info {
	if current.Branch == "" || !strings.HasPrefix(current.Branch, worktreeBranchPrefix) {
		return results
	}

	current.SessionID = strings.TrimPrefix(current.Branch, worktreeBranchPrefix)

	return append(results, current)
}

func hydrateWorktreeListFromManifests(ctx context.Context, repoRoot string, results []Info) {
	if len(results) == 0 {
		return
	}

	base, baseErr := gitCurrentBranch(ctx, repoRoot)

	for i := range results {
		manifest, ok, err := loadWorktreeManifest(ctx, repoRoot, results[i].SessionID)
		if err == nil && ok {
			results[i].BaseBranch = manifest.BaseBranch
			results[i].Path = manifest.WorktreePath
			results[i].Branch = manifest.Branch

			continue
		}

		if baseErr == nil {
			results[i].BaseBranch = base
		}
	}
}
