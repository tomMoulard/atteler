package worktree

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// initTestRepo creates a temporary git repository with one commit and
// returns its path. The caller should defer os.RemoveAll on the returned
// directory.
func initTestRepo(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()

	cmds := [][]string{
		{"git", "init"},
		{"git", "config", "user.email", "test@test.com"},
		{"git", "config", "user.name", "Test"},
		{"git", "config", "commit.gpgsign", "false"},
		{"git", "config", "core.excludesFile", os.DevNull},
		{"git", "commit", "--allow-empty", "-m", "init"},
	}
	for _, args := range cmds {
		cmd := exec.CommandContext(t.Context(), args[0], args[1:]...) //nolint:gosec // Test helper runs static git commands.

		cmd.Dir = dir
		if out, err := cmd.CombinedOutput(); err != nil {
			require.Failf(t, "unexpected failure", "setup %v: %s: %v", args, out, err)
		}
	}

	return dir
}

func runGit(t *testing.T, dir string, args ...string) {
	t.Helper()

	cmd := exec.CommandContext(t.Context(), "git", args...)
	cmd.Dir = dir

	if out, err := cmd.CombinedOutput(); err != nil {
		require.Failf(t, "unexpected failure", "git %v: %s: %v", args, out, err)
	}
}

func runGitExpectError(t *testing.T, dir string, args ...string) string {
	t.Helper()

	cmd := exec.CommandContext(t.Context(), "git", args...)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	require.Error(t, err)

	return string(out)
}

func gitTestOutput(t *testing.T, dir string, args ...string) string {
	t.Helper()

	cmd := exec.CommandContext(t.Context(), "git", args...)
	cmd.Dir = dir

	out, err := cmd.CombinedOutput()
	if err != nil {
		require.Failf(t, "unexpected failure", "git %v: %s: %v", args, out, err)
	}

	return string(out)
}

func writeFile(t *testing.T, path, content string) {
	t.Helper()

	require.NoError(t, os.WriteFile(path, []byte(content), 0o600))
}

func commitConflictFile(t *testing.T, repo, content, message string) {
	t.Helper()

	writeFile(t, filepath.Join(repo, "conflict.txt"), content)
	runGit(t, repo, "add", "conflict.txt")
	runGit(t, repo, "commit", "-m", message)
}

func commitAll(t *testing.T, repo, message string) {
	t.Helper()

	runGit(t, repo, "add", "-A")
	runGit(t, repo, "commit", "-m", message)
}

func mergeOptions() MergeOptions {
	return MergeOptions{
		AutoMerge: true,
		Strategy:  MergeStrategyMerge,
	}
}

func reviewedAutoCommitMergeOptions() MergeOptions {
	return MergeOptions{
		AutoCommit:         true,
		ReviewedAutoCommit: true,
		AutoMerge:          true,
		Strategy:           MergeStrategyMerge,
	}
}

func TestCreateAndRemove(t *testing.T) {
	t.Parallel()
	repo := initTestRepo(t)

	info, err := Create(repo, "test-session-1")
	if err != nil {
		require.Failf(t, "unexpected failure", "Create: %v", err)
	}

	if info.SessionID != "test-session-1" {
		assert.Failf(t, "assertion failed", "SessionID = %q, want %q", info.SessionID, "test-session-1")
	}

	if info.Branch != "atteler/test-session-1" {
		assert.Failf(t, "assertion failed", "Branch = %q, want %q", info.Branch, "atteler/test-session-1")
	}

	if info.BaseBranch == "" {
		assert.Fail(t, "BaseBranch is empty")
	}

	if _, err := os.Stat(info.Path); err != nil {
		assert.Failf(t, "assertion failed", "worktree path does not exist: %v", err)
	}

	// Remove should clean up.
	if err := Remove(repo, info); err != nil {
		require.Failf(t, "unexpected failure", "Remove: %v", err)
	}

	if _, err := os.Stat(info.Path); !os.IsNotExist(err) {
		assert.Failf(t, "assertion failed", "worktree path still exists after Remove")
	}

	require.NoError(t, Remove(repo, info))
}

func TestCreateIdempotent(t *testing.T) {
	t.Parallel()
	repo := initTestRepo(t)

	info1, err := Create(repo, "idempotent-1")
	if err != nil {
		require.Failf(t, "unexpected failure", "first Create: %v", err)
	}

	writeFile(t, filepath.Join(repo, "dirty-after-create.txt"), "dirty\n")

	// Second create for the same session should succeed (join).
	info2, err := Create(repo, "idempotent-1")
	if err != nil {
		require.Failf(t, "unexpected failure", "second Create: %v", err)
	}

	if info2.Path != info1.Path {
		assert.Failf(t, "assertion failed", "paths differ: %s vs %s", info2.Path, info1.Path)
	}

	if err := Remove(repo, info1); err != nil {
		require.Failf(t, "unexpected failure", "Remove: %v", err)
	}
}

func TestCreateRejoinsExistingManifestFromDetachedHead(t *testing.T) {
	t.Parallel()
	repo := initTestRepo(t)

	info1, err := Create(repo, "detached-rejoin")
	require.NoError(t, err)

	runGit(t, repo, "checkout", "--detach")

	info2, err := Create(repo, "detached-rejoin")
	require.NoError(t, err)
	assert.Equal(t, info1.Path, info2.Path)
	assert.Equal(t, info1.BaseBranch, info2.BaseBranch)

	require.NoError(t, Remove(repo, info1))
}

func TestCreateRejectsNewSessionFromDetachedHead(t *testing.T) {
	t.Parallel()
	repo := initTestRepo(t)

	runGit(t, repo, "checkout", "--detach")

	_, err := Create(repo, "new-detached")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "detached HEAD")

	_, ok, manifestErr := loadWorktreeManifest(t.Context(), repo, "new-detached")
	require.NoError(t, manifestErr)
	assert.False(t, ok)
}

func TestCreateWritesManifestAndRejoinsWithRecordedBase(t *testing.T) {
	t.Parallel()
	repo := initTestRepo(t)

	info1, err := Create(repo, "manifest-rejoin")
	require.NoError(t, err)

	manifest, ok, err := loadWorktreeManifest(t.Context(), repo, "manifest-rejoin")
	require.NoError(t, err)
	require.True(t, ok)
	assert.Equal(t, manifestStateActive, manifest.State)
	assert.Equal(t, info1.Branch, manifest.Branch)
	assert.Equal(t, info1.Path, manifest.WorktreePath)
	assert.NotEmpty(t, manifest.BaseHEAD)

	runGit(t, repo, "checkout", "-b", "other")

	info2, err := Create(repo, "manifest-rejoin")
	require.NoError(t, err)
	assert.Equal(t, info1.Path, info2.Path)
	assert.Equal(t, info1.BaseBranch, info2.BaseBranch)

	require.NoError(t, Remove(repo, info1))
}

func TestCreateRefusesDirtyBaseBeforeManifest(t *testing.T) {
	t.Parallel()
	repo := initTestRepo(t)

	writeFile(t, filepath.Join(repo, "dirty.txt"), "dirty\n")

	_, err := Create(repo, "dirty-create")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "before isolation")
	assert.Contains(t, err.Error(), "dirty.txt")

	branchExists, branchErr := gitBranchExists(t.Context(), repo, "atteler/dirty-create")
	require.NoError(t, branchErr)
	assert.False(t, branchExists)

	_, ok, manifestErr := loadWorktreeManifest(t.Context(), repo, "dirty-create")
	require.NoError(t, manifestErr)
	assert.False(t, ok)
}

func TestCreateResumesStaleManifestWithBranch(t *testing.T) {
	t.Parallel()
	repo := initTestRepo(t)

	sessionID := "resume-stale"
	branch := "atteler/" + sessionID
	repoRoot, err := gitRepoRoot(t.Context(), repo)
	require.NoError(t, err)
	baseBranch, err := gitCurrentBranch(t.Context(), repoRoot)
	require.NoError(t, err)
	baseHEAD, err := gitRevParse(t.Context(), repoRoot, "HEAD")
	require.NoError(t, err)

	wtDir := worktreeDir(repoRoot, sessionID)

	manifest := newWorktreeManifest(sessionID, branch, baseBranch, baseHEAD, repoRoot, wtDir)
	manifest.State = manifestStateBranchCreated
	require.NoError(t, writeWorktreeManifest(t.Context(), repoRoot, &manifest, "test-stale", "branch exists without worktree"))
	runGit(t, repoRoot, "branch", branch, baseHEAD)

	info, err := Create(repo, sessionID)
	require.NoError(t, err)
	assert.Equal(t, wtDir, info.Path)

	_, statErr := os.Stat(info.Path)
	require.NoError(t, statErr)

	loaded, ok, err := loadWorktreeManifest(t.Context(), repo, sessionID)
	require.NoError(t, err)
	require.True(t, ok)
	assert.Equal(t, manifestStateActive, loaded.State)

	require.NoError(t, Remove(repo, info))
}

func TestCreateRepairsMissingRegisteredWorktreePath(t *testing.T) {
	t.Parallel()
	repo := initTestRepo(t)

	info, err := Create(repo, "missing-registered-path")
	require.NoError(t, err)
	require.NoError(t, os.RemoveAll(info.Path))

	rejoined, err := Create(repo, "missing-registered-path")
	require.NoError(t, err)
	assert.Equal(t, info.Path, rejoined.Path)

	exists, existsErr := pathExists(rejoined.Path)
	require.NoError(t, existsErr)
	assert.True(t, exists)

	require.NoError(t, Remove(repo, rejoined))
}

func TestCreateRejectsManifestBranchWithWrongBase(t *testing.T) {
	t.Parallel()
	repo := initTestRepo(t)

	sessionID := "branch-base-mismatch"
	branch := "atteler/" + sessionID
	repoRoot, err := gitRepoRoot(t.Context(), repo)
	require.NoError(t, err)
	baseBranch, err := gitCurrentBranch(t.Context(), repoRoot)
	require.NoError(t, err)
	baseHEAD, err := gitRevParse(t.Context(), repoRoot, "HEAD")
	require.NoError(t, err)

	manifest := newWorktreeManifest(sessionID, branch, baseBranch, baseHEAD, repoRoot, worktreeDir(repoRoot, sessionID))
	manifest.State = manifestStateBranchCreated
	require.NoError(t, writeWorktreeManifest(t.Context(), repoRoot, &manifest, "test-branch-collision", "branch points at unrelated history"))

	runGit(t, repoRoot, "checkout", "--orphan", "unrelated-base")
	writeFile(t, filepath.Join(repoRoot, "unrelated.txt"), "unrelated\n")
	runGit(t, repoRoot, "add", "-A")
	runGit(t, repoRoot, "commit", "-m", "unrelated")
	unrelatedHead, err := gitRevParse(t.Context(), repoRoot, "HEAD")
	require.NoError(t, err)
	runGit(t, repoRoot, "checkout", baseBranch)
	runGit(t, repoRoot, "branch", branch, unrelatedHead)

	_, err = Create(repo, sessionID)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "does not descend from recorded base HEAD")
}

func TestCreateRejectsRemovedManifest(t *testing.T) {
	t.Parallel()
	repo := initTestRepo(t)

	info, err := Create(repo, "removed-session")
	require.NoError(t, err)
	require.NoError(t, Remove(repo, info))

	_, err = Create(repo, "removed-session")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "already merged")
}

func TestMergeWithExplicitPolicy(t *testing.T) {
	t.Parallel()
	repo := initTestRepo(t)

	info, err := Create(repo, "merge-session")
	if err != nil {
		require.Failf(t, "unexpected failure", "Create: %v", err)
	}

	// Write a file in the worktree and commit it.
	testFile := filepath.Join(info.Path, "hello.txt")
	if writeErr := os.WriteFile(testFile, []byte("hello\n"), 0o600); writeErr != nil {
		require.Failf(t, "unexpected failure", "write test file: %v", writeErr)
	}

	commitAll(t, info.Path, "session change")

	if mergeErr := MergeWithOptionsContext(t.Context(), repo, info, mergeOptions()); mergeErr != nil {
		require.Failf(t, "unexpected failure", "Merge: %v", mergeErr)
	}

	// The file should now exist in the main repo.
	mainFile := filepath.Join(repo, "hello.txt")

	data, err := os.ReadFile(mainFile)
	if err != nil {
		require.Failf(t, "unexpected failure", "read merged file: %v", err)
	}

	if string(data) != "hello\n" {
		assert.Failf(t, "assertion failed", "merged content = %q, want %q", string(data), "hello\n")
	}

	// Worktree directory should be gone.
	if _, err := os.Stat(info.Path); !os.IsNotExist(err) {
		assert.Failf(t, "assertion failed", "worktree path still exists after Merge")
	}
}

func TestMergeRequiresExplicitAutoMergePolicy(t *testing.T) {
	t.Parallel()
	repo := initTestRepo(t)

	info, err := Create(repo, "merge-default-policy")
	require.NoError(t, err)

	err = Merge(repo, info)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "auto-merge policy")

	manifest, ok, manifestErr := loadWorktreeManifest(t.Context(), repo, info.SessionID)
	require.NoError(t, manifestErr)
	require.True(t, ok)
	assert.Equal(t, manifestStateFailed, manifest.State)
	assert.Contains(t, manifest.LastError, "auto-merge policy")

	require.NoError(t, Remove(repo, info))
}

func TestCreateRejectsUnsafeSessionIDs(t *testing.T) {
	t.Parallel()
	repo := initTestRepo(t)

	for _, sessionID := range []string{
		"../escape",
		"nested/session",
		"has space",
		".hidden",
		"two..dots",
		"bad.lock",
		"bad@{ref}",
		"-leading-dash",
	} {
		t.Run(sessionID, func(t *testing.T) {
			t.Parallel()

			_, err := Create(repo, sessionID)
			require.Error(t, err)
			assert.Contains(t, err.Error(), "unsafe session ID")
		})
	}
}

func TestCreateRejectsBranchAlreadyCheckedOutElsewhere(t *testing.T) {
	t.Parallel()
	repo := initTestRepo(t)

	runGit(t, repo, "branch", "atteler/checked-out")
	runGit(t, repo, "checkout", "atteler/checked-out")

	_, err := Create(repo, "checked-out")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "already exists without ownership metadata")
	assert.Contains(t, err.Error(), "atteler/checked-out")
}

func TestCreateRejectsExistingBranchWithoutMatchingWorktree(t *testing.T) {
	t.Parallel()
	repo := initTestRepo(t)

	runGit(t, repo, "branch", "atteler/stale")

	_, err := Create(repo, "stale")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "already exists without ownership metadata")
	assert.Contains(t, err.Error(), "atteler/stale")
}

func TestCreateRejectsExistingPathWithoutOwnershipMetadata(t *testing.T) {
	t.Parallel()
	repo := initTestRepo(t)

	blockingDir := filepath.Join(repo, ".atteler", "worktrees", "add-fail")
	require.NoError(t, os.MkdirAll(blockingDir, 0o700))
	writeFile(t, filepath.Join(blockingDir, "blocking.txt"), "blocking\n")

	_, err := Create(repo, "add-fail")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "already exists without ownership metadata")

	branchExists, branchErr := gitBranchExists(t.Context(), repo, "atteler/add-fail")
	require.NoError(t, branchErr)
	assert.False(t, branchExists)
}

func TestMergeRequiresExplicitPolicies(t *testing.T) {
	t.Parallel()
	repo := initTestRepo(t)

	info, err := Create(repo, "policy")
	require.NoError(t, err)

	err = MergeWithOptionsContext(t.Context(), repo, info, MergeOptions{})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "auto-merge policy")

	writeFile(t, filepath.Join(info.Path, "policy.txt"), "policy\n")

	err = MergeWithOptionsContext(t.Context(), repo, info, MergeOptions{AutoMerge: true})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "explicit strategy")

	err = MergeWithOptionsContext(t.Context(), repo, info, MergeOptions{AutoMerge: true, Strategy: MergeStrategyMerge})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "auto-commit policy")

	require.NoError(t, RemoveWithOptionsContext(t.Context(), repo, info, RemoveOptions{Force: true}))
}

func TestMergeRefusesUnreviewedAutoCommitAndPreservesWork(t *testing.T) {
	t.Parallel()
	repo := initTestRepo(t)

	info, err := Create(repo, "unreviewed-auto-commit")
	require.NoError(t, err)
	writeFile(t, filepath.Join(info.Path, "unreviewed.txt"), "unreviewed\n")

	opts := mergeOptions()
	opts.AutoCommit = true

	err = MergeWithOptionsContext(t.Context(), repo, info, opts)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "auto-commit review")
	assert.Contains(t, err.Error(), "not marked reviewed")

	_, readErr := os.ReadFile(filepath.Join(repo, "unreviewed.txt"))
	assert.True(t, os.IsNotExist(readErr))

	exists, existsErr := pathExists(info.Path)
	require.NoError(t, existsErr)
	assert.True(t, exists)
	branchExists, branchErr := gitBranchExists(t.Context(), repo, info.Branch)
	require.NoError(t, branchErr)
	assert.True(t, branchExists)

	require.NoError(t, RemoveWithOptionsContext(t.Context(), repo, info, RemoveOptions{Force: true}))
}

func TestMergeInterruptedAutoCommitPreservesBranchAndWorktree(t *testing.T) {
	t.Parallel()
	repo := initTestRepo(t)

	info, err := Create(repo, "interrupted-auto-commit")
	require.NoError(t, err)
	writeFile(t, filepath.Join(info.Path, "blocked.txt"), "blocked\n")

	lockPath, err := gitPath(t.Context(), info.Path, "index.lock")
	require.NoError(t, err)
	writeFile(t, lockPath, "locked\n")

	err = MergeWithOptionsContext(t.Context(), repo, info, reviewedAutoCommitMergeOptions())
	require.Error(t, err)
	assert.Contains(t, err.Error(), "auto-commit")

	exists, existsErr := pathExists(info.Path)
	require.NoError(t, existsErr)
	assert.True(t, exists)
	branchExists, branchErr := gitBranchExists(t.Context(), repo, info.Branch)
	require.NoError(t, branchErr)
	assert.True(t, branchExists)

	require.NoError(t, os.Remove(lockPath))
	require.NoError(t, RemoveWithOptionsContext(t.Context(), repo, info, RemoveOptions{Force: true}))
}

func TestRemoveRefusesUnmergedBranchWithoutForce(t *testing.T) {
	t.Parallel()
	repo := initTestRepo(t)

	info, err := Create(repo, "remove-unmerged")
	require.NoError(t, err)
	writeFile(t, filepath.Join(info.Path, "unmerged.txt"), "unmerged\n")
	commitAll(t, info.Path, "unmerged")

	err = Remove(repo, info)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "refusing to remove unmerged branch")

	exists, existsErr := pathExists(info.Path)
	require.NoError(t, existsErr)
	assert.True(t, exists)
	branchExists, branchErr := gitBranchExists(t.Context(), repo, info.Branch)
	require.NoError(t, branchErr)
	assert.True(t, branchExists)

	require.NoError(t, RemoveWithOptionsContext(t.Context(), repo, info, RemoveOptions{Force: true}))
}

func TestRemoveReportsFailedBranchDeletionAndPreservesBranch(t *testing.T) {
	repo := initTestRepo(t)

	info, err := Create(repo, "delete-fails")
	require.NoError(t, err)

	realGit, err := exec.LookPath("git")
	require.NoError(t, err)

	fakeBin := t.TempDir()
	fakeGit := filepath.Join(fakeBin, "git")
	marker := filepath.Join(fakeBin, "fail-once")
	writeFile(t, marker, "fail\n")
	script := "#!/bin/sh\n" +
		"if [ \"$1\" = \"branch\" ] && [ \"$2\" = \"-d\" ] && [ \"$3\" = \"atteler/delete-fails\" ] && [ -f " + marker + " ]; then\n" +
		"  rm " + marker + "\n" +
		"  echo simulated branch delete failure >&2\n" +
		"  exit 1\n" +
		"fi\n" +
		"exec " + realGit + " \"$@\"\n"
	require.NoError(t, os.WriteFile(fakeGit, []byte(script), 0o600))
	require.NoError(t, os.Chmod(fakeGit, 0o700)) //nolint:gosec // Test creates an executable fake git wrapper.
	t.Setenv("PATH", fakeBin+string(os.PathListSeparator)+os.Getenv("PATH"))

	err = Remove(repo, info)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "delete branch")
	assert.Contains(t, err.Error(), "simulated branch delete failure")

	exists, existsErr := pathExists(info.Path)
	require.NoError(t, existsErr)
	assert.False(t, exists)

	branchExists, branchErr := gitBranchExists(t.Context(), repo, info.Branch)
	require.NoError(t, branchErr)
	assert.True(t, branchExists)

	require.NoError(t, Remove(repo, info))

	branchExists, branchErr = gitBranchExists(t.Context(), repo, info.Branch)
	require.NoError(t, branchErr)
	assert.False(t, branchExists)
}

func TestRemoveRepairsStaleManifestAfterCompletedGitCleanup(t *testing.T) {
	t.Parallel()
	repo := initTestRepo(t)

	info, err := Create(repo, "stale-remove-manifest")
	require.NoError(t, err)
	require.NoError(t, Remove(repo, info))

	manifest, ok, err := loadWorktreeManifest(t.Context(), repo, info.SessionID)
	require.NoError(t, err)
	require.True(t, ok)

	manifest.State = manifestStateActive
	require.NoError(t, writeWorktreeManifest(t.Context(), repo, manifest, "test-stale-remove", "simulate interrupted manifest update"))

	require.NoError(t, Remove(repo, info))

	manifest, ok, err = loadWorktreeManifest(t.Context(), repo, info.SessionID)
	require.NoError(t, err)
	require.True(t, ok)
	assert.Equal(t, manifestStateRemoved, manifest.State)
}

func TestRemoveRequiresOwnershipMetadataEvenWhenGitStateIsGone(t *testing.T) {
	t.Parallel()
	repo := initTestRepo(t)

	info := &Info{
		Path:       filepath.Join(repo, ".atteler", "worktrees", "missing-owned-state"),
		Branch:     "atteler/missing-owned-state",
		BaseBranch: "main",
		SessionID:  "missing-owned-state",
	}

	err := Remove(repo, info)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "missing ownership metadata")

	require.NoError(t, RemoveWithOptionsContext(t.Context(), repo, info, RemoveOptions{Force: true}))
}

func TestForceRemoveCleansPathWhenBranchAlreadyMissing(t *testing.T) {
	t.Parallel()
	repo := initTestRepo(t)

	info, err := Create(repo, "missing-branch-force")
	require.NoError(t, err)
	runGit(t, repo, "update-ref", "-d", "refs/heads/"+info.Branch)

	require.NoError(t, RemoveWithOptionsContext(t.Context(), repo, info, RemoveOptions{Force: true}))

	exists, existsErr := pathExists(info.Path)
	require.NoError(t, existsErr)
	assert.False(t, exists)
}

func TestMergeRefusesDirtyBaseWorktree(t *testing.T) {
	t.Parallel()
	repo := initTestRepo(t)

	info, err := Create(repo, "dirty-base")
	require.NoError(t, err)

	writeFile(t, filepath.Join(repo, "untracked.txt"), "dirty\n")

	err = MergeWithOptionsContext(t.Context(), repo, info, mergeOptions())
	require.Error(t, err)
	assert.Contains(t, err.Error(), "preflight main repository")
	assert.Contains(t, err.Error(), "uncommitted or untracked")
	assert.Contains(t, err.Error(), "untracked.txt")

	require.NoError(t, os.Remove(filepath.Join(repo, "untracked.txt")))
	require.NoError(t, Remove(repo, info))
}

func TestMergeRefusesModifiedBaseWorktree(t *testing.T) {
	t.Parallel()
	repo := initTestRepo(t)

	writeFile(t, filepath.Join(repo, "tracked.txt"), "clean\n")
	runGit(t, repo, "add", "tracked.txt")
	runGit(t, repo, "commit", "-m", "track file")

	info, err := Create(repo, "modified-base")
	require.NoError(t, err)

	writeFile(t, filepath.Join(repo, "tracked.txt"), "dirty\n")

	err = MergeWithOptionsContext(t.Context(), repo, info, mergeOptions())
	require.Error(t, err)
	assert.Contains(t, err.Error(), "uncommitted or untracked")
	assert.Contains(t, err.Error(), "tracked.txt")

	runGit(t, repo, "checkout", "--", "tracked.txt")
	require.NoError(t, Remove(repo, info))
}

func TestGitStatusSummaryExcludesManagedWorktreePath(t *testing.T) {
	t.Parallel()
	repo := initTestRepo(t)

	info, err := Create(repo, "status-exclude")
	require.NoError(t, err)
	repoRoot, err := gitRepoRoot(t.Context(), repo)
	require.NoError(t, err)

	writeFile(t, filepath.Join(info.Path, "worktree.txt"), "worktree\n")
	writeFile(t, filepath.Join(repoRoot, ".atteler", "main.txt"), "main\n")

	summary, err := gitStatusSummary(t.Context(), repoRoot, info.Path)
	require.NoError(t, err)

	assert.Contains(t, summary.String(), ".atteler/main.txt")
	assert.NotContains(t, summary.String(), ".atteler/worktrees/status-exclude")

	require.NoError(t, os.Remove(filepath.Join(repoRoot, ".atteler", "main.txt")))
	require.NoError(t, RemoveWithOptionsContext(t.Context(), repo, info, RemoveOptions{Force: true}))
}

func TestMergeRefusesDetachedHead(t *testing.T) {
	t.Parallel()
	repo := initTestRepo(t)

	info, err := Create(repo, "detached")
	require.NoError(t, err)
	runGit(t, repo, "checkout", "--detach")

	err = MergeWithOptionsContext(t.Context(), repo, info, mergeOptions())
	require.Error(t, err)
	assert.Contains(t, err.Error(), "detached HEAD")
	assert.Contains(t, err.Error(), "recovery: failed step: preflight main repository")
	assert.Contains(t, err.Error(), "recovery: branch: "+info.Branch)
	assert.Contains(t, err.Error(), "recovery: worktree path: "+info.Path)

	require.NoError(t, Remove(repo, info))
}

func TestMergeRefusesPendingMerge(t *testing.T) {
	t.Parallel()
	repo := initTestRepo(t)

	commitConflictFile(t, repo, "base\n", "base")
	info, err := Create(repo, "pending-merge")
	require.NoError(t, err)

	runGit(t, repo, "checkout", "-b", "incoming")
	commitConflictFile(t, repo, "incoming\n", "incoming")
	runGit(t, repo, "checkout", info.BaseBranch)
	commitConflictFile(t, repo, "main\n", "main")
	runGitExpectError(t, repo, "merge", "incoming")

	err = MergeWithOptionsContext(t.Context(), repo, info, mergeOptions())
	require.Error(t, err)
	assert.Contains(t, err.Error(), "pending merge")

	runGit(t, repo, "merge", "--abort")
	require.NoError(t, Remove(repo, info))
}

func TestMergeRefusesPendingCherryPickAndRebase(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		gitPath string
		want    string
		isDir   bool
	}{
		{name: "cherry-pick", gitPath: "CHERRY_PICK_HEAD", want: "pending cherry-pick"},
		{name: "rebase", gitPath: "rebase-merge", want: "pending rebase", isDir: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			repo := initTestRepo(t)

			info, err := Create(repo, "pending-"+tt.name)
			require.NoError(t, err)

			pendingPath, err := gitPath(t.Context(), repo, tt.gitPath)
			require.NoError(t, err)

			if tt.isDir {
				require.NoError(t, os.MkdirAll(pendingPath, 0o700))
			} else {
				writeFile(t, pendingPath, "pending\n")
			}

			err = MergeWithOptionsContext(t.Context(), repo, info, mergeOptions())
			require.Error(t, err)
			assert.Contains(t, err.Error(), tt.want)

			if tt.isDir {
				require.NoError(t, os.RemoveAll(pendingPath))
			} else {
				require.NoError(t, os.Remove(pendingPath))
			}

			require.NoError(t, Remove(repo, info))
		})
	}
}

func TestMergeRefusesBaseBranchMismatchUnlessOverride(t *testing.T) {
	t.Parallel()
	repo := initTestRepo(t)

	info, err := Create(repo, "base-mismatch")
	require.NoError(t, err)
	writeFile(t, filepath.Join(info.Path, "override.txt"), "override\n")
	commitAll(t, info.Path, "override")
	runGit(t, repo, "checkout", "-b", "other")

	err = MergeWithOptionsContext(t.Context(), repo, info, mergeOptions())
	require.Error(t, err)
	assert.Contains(t, err.Error(), "expected recorded base branch")

	opts := mergeOptions()
	opts.AllowBaseBranchMismatch = true
	require.NoError(t, MergeWithOptionsContext(t.Context(), repo, info, opts))

	data, err := os.ReadFile(filepath.Join(repo, "override.txt"))
	require.NoError(t, err)
	assert.Equal(t, "override\n", string(data))
}

func TestMergeRefusesCurrentHeadThatLostRecordedBase(t *testing.T) {
	t.Parallel()
	repo := initTestRepo(t)

	writeFile(t, filepath.Join(repo, "base.txt"), "base\n")
	commitAll(t, repo, "recorded base")

	info, err := Create(repo, "base-head-lost")
	require.NoError(t, err)
	writeFile(t, filepath.Join(info.Path, "branch.txt"), "branch\n")
	commitAll(t, info.Path, "branch")

	runGit(t, repo, "reset", "--hard", "HEAD~1")

	err = MergeWithOptionsContext(t.Context(), repo, info, mergeOptions())
	require.Error(t, err)
	assert.Contains(t, err.Error(), "recorded base HEAD")
	assert.Contains(t, err.Error(), "current HEAD")

	exists, existsErr := pathExists(info.Path)
	require.NoError(t, existsErr)
	assert.True(t, exists)
	branchExists, branchErr := gitBranchExists(t.Context(), repo, info.Branch)
	require.NoError(t, branchErr)
	assert.True(t, branchExists)

	require.NoError(t, RemoveWithOptionsContext(t.Context(), repo, info, RemoveOptions{Force: true}))
}

func TestMergeConflictRollsBackAndReportsRecovery(t *testing.T) {
	t.Parallel()
	repo := initTestRepo(t)

	commitConflictFile(t, repo, "base\n", "base")
	info, err := Create(repo, "merge-conflict")
	require.NoError(t, err)

	writeFile(t, filepath.Join(info.Path, "conflict.txt"), "branch\n")
	commitAll(t, info.Path, "branch")
	commitConflictFile(t, repo, "main\n", "main")

	err = MergeWithOptionsContext(t.Context(), repo, info, mergeOptions())
	require.Error(t, err)

	var mergeErr *MergeError
	require.ErrorAs(t, err, &mergeErr)
	assert.Equal(t, "dry-run merge", mergeErr.Step)
	assert.False(t, mergeErr.RolledBack)
	assert.Equal(t, info.Branch, mergeErr.Branch)
	assert.Equal(t, info.Path, mergeErr.WorktreePath)
	assert.NotEmpty(t, mergeErr.TransactionLog)
	assert.Contains(t, err.Error(), "recovery: branch: "+info.Branch)
	assert.Contains(t, err.Error(), "recovery: worktree path: "+info.Path)
	assert.Contains(t, err.Error(), "recovery: failed step: dry-run merge")
	assert.Contains(t, err.Error(), "merge dry-run reported conflicts")

	data, readErr := os.ReadFile(filepath.Join(repo, "conflict.txt"))
	require.NoError(t, readErr)
	assert.Equal(t, "main\n", string(data))

	pending, pendingErr := gitPendingOperation(t.Context(), repo)
	require.NoError(t, pendingErr)
	assert.Empty(t, pending)

	exists, existsErr := pathExists(info.Path)
	require.NoError(t, existsErr)
	assert.True(t, exists)
	branchExists, branchErr := gitBranchExists(t.Context(), repo, info.Branch)
	require.NoError(t, branchErr)
	assert.True(t, branchExists)
}

func TestMergeAutoCommitMessageIncludesProvenanceAndSummary(t *testing.T) {
	t.Parallel()
	repo := initTestRepo(t)

	info, err := Create(repo, "commit-message")
	require.NoError(t, err)
	writeFile(t, filepath.Join(info.Path, "hello.txt"), "hello\n")

	opts := reviewedAutoCommitMergeOptions()
	opts.Provenance = []string{"issue GH-83"}
	require.NoError(t, MergeWithOptionsContext(t.Context(), repo, info, opts))

	log := gitTestOutput(t, repo, "log", "--grep=atteler: auto-commit session commit-message", "--format=%B", "-n", "1")
	assert.Contains(t, log, "atteler: auto-commit session commit-message")
	assert.Contains(t, log, "Session: commit-message")
	assert.Contains(t, log, "Branch: atteler/commit-message")
	assert.Contains(t, log, "Provenance: issue GH-83")
	assert.Contains(t, log, "Changed files:")
	assert.Contains(t, log, "- ?? hello.txt")

	matches, globErr := filepath.Glob(filepath.Join(repo, ".git", "atteler", "worktree-transactions", "commit-message-*.log"))
	require.NoError(t, globErr)
	require.NotEmpty(t, matches)

	data, readErr := os.ReadFile(matches[0])
	require.NoError(t, readErr)
	assert.Contains(t, string(data), "diff summary")
	assert.Contains(t, string(data), "?? hello.txt")
}

func TestMergeReportsFailedCleanupAndPreservesWorktree(t *testing.T) {
	t.Parallel()
	repo := initTestRepo(t)

	info, err := Create(repo, "cleanup-failure")
	require.NoError(t, err)
	writeFile(t, filepath.Join(info.Path, "cleanup.txt"), "cleanup\n")
	commitAll(t, info.Path, "cleanup")
	runGit(t, repo, "worktree", "lock", info.Path)
	t.Cleanup(func() {
		if unlockErr := exec.CommandContext(defaultCommandContext(), "git", "-C", repo, "worktree", "unlock", info.Path).Run(); unlockErr != nil {
			t.Logf("unlock cleanup worktree: %v", unlockErr)
		}

		if removeErr := Remove(repo, info); removeErr != nil {
			t.Logf("remove cleanup worktree: %v", removeErr)
		}
	})

	err = MergeWithOptionsContext(t.Context(), repo, info, mergeOptions())
	require.Error(t, err)

	var mergeErr *MergeError
	require.ErrorAs(t, err, &mergeErr)
	assert.Equal(t, "cleanup worktree", mergeErr.Step)
	assert.Contains(t, err.Error(), "recovery: failed step: cleanup worktree")
	assert.Contains(t, err.Error(), "recovery: branch: "+info.Branch)
	assert.Contains(t, err.Error(), "recovery: worktree path: "+info.Path)

	data, readErr := os.ReadFile(filepath.Join(repo, "cleanup.txt"))
	require.NoError(t, readErr)
	assert.Equal(t, "cleanup\n", string(data))

	exists, existsErr := pathExists(info.Path)
	require.NoError(t, existsErr)
	assert.True(t, exists)
	branchExists, branchErr := gitBranchExists(t.Context(), repo, info.Branch)
	require.NoError(t, branchErr)
	assert.True(t, branchExists)

	runGit(t, repo, "worktree", "unlock", info.Path)
	require.NoError(t, MergeWithOptionsContext(t.Context(), repo, info, mergeOptions()))

	exists, existsErr = pathExists(info.Path)
	require.NoError(t, existsErr)
	assert.False(t, exists)
	branchExists, branchErr = gitBranchExists(t.Context(), repo, info.Branch)
	require.NoError(t, branchErr)
	assert.False(t, branchExists)
}

func TestList(t *testing.T) {
	t.Parallel()
	repo := initTestRepo(t)

	// No worktrees initially.
	infos, err := List(repo)
	if err != nil {
		require.Failf(t, "unexpected failure", "List: %v", err)
	}

	if len(infos) != 0 {
		assert.Failf(t, "assertion failed", "expected 0 worktrees, got %d", len(infos))
	}

	// Create two worktrees.
	info1, err := Create(repo, "list-1")
	if err != nil {
		require.Failf(t, "unexpected failure", "Create list-1: %v", err)
	}

	info2, err := Create(repo, "list-2")
	if err != nil {
		require.Failf(t, "unexpected failure", "Create list-2: %v", err)
	}

	infos, err = List(repo)
	if err != nil {
		require.Failf(t, "unexpected failure", "List: %v", err)
	}

	if len(infos) != 2 {
		assert.Failf(t, "assertion failed", "expected 2 worktrees, got %d", len(infos))
	}

	if err := Remove(repo, info1); err != nil {
		require.Failf(t, "unexpected failure", "Remove info1: %v", err)
	}

	if err := Remove(repo, info2); err != nil {
		require.Failf(t, "unexpected failure", "Remove info2: %v", err)
	}
}

func TestListContextCanceled(t *testing.T) {
	t.Parallel()
	repo := initTestRepo(t)
	ctx, cancel := context.WithCancel(t.Context())
	cancel()

	_, err := ListContext(ctx, repo)
	if err == nil {
		require.FailNow(t, "expected canceled context to stop git worktree listing")
	}
}

func TestStatus(t *testing.T) {
	t.Parallel()

	got := Status(nil)
	if got != "no worktree" {
		assert.Failf(t, "assertion failed", "Status(nil) = %q, want %q", got, "no worktree")
	}

	info := &Info{
		Path:       "/tmp/test",
		Branch:     "atteler/test",
		BaseBranch: "main",
	}

	got = Status(info)
	if !strings.Contains(got, "/tmp/test") {
		assert.Failf(t, "assertion failed", "Status missing path: %s", got)
	}

	if !strings.Contains(got, "atteler/test") {
		assert.Failf(t, "assertion failed", "Status missing branch: %s", got)
	}
}

func TestIsGitRepo(t *testing.T) {
	t.Parallel()

	repo := initTestRepo(t)
	if !IsGitRepo(repo) {
		assert.Fail(t, "IsGitRepo returned false for a git repo")
	}

	nonRepo := t.TempDir()
	if IsGitRepo(nonRepo) {
		assert.Fail(t, "IsGitRepo returned true for a non-git dir")
	}
}

func TestCreateEmptySessionID(t *testing.T) {
	t.Parallel()
	repo := initTestRepo(t)

	_, err := Create(repo, "")
	if err == nil {
		assert.Fail(t, "expected error for empty session ID")
	}
}

func TestCreateNotGitRepo(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()

	_, err := Create(dir, "test")
	if err == nil {
		assert.Fail(t, "expected error for non-git directory")
	}
}

func TestRemoveNilInfo(t *testing.T) {
	t.Parallel()

	err := Remove("/tmp", nil)
	if err == nil {
		assert.Fail(t, "expected error for nil info")
	}
}

func TestMergeNilInfo(t *testing.T) {
	t.Parallel()

	err := Merge("/tmp", nil)
	if err == nil {
		assert.Fail(t, "expected error for nil info")
	}
}

func TestParseWorktreeList(t *testing.T) {
	t.Parallel()

	input := `worktree /home/user/project
HEAD abc123
branch refs/heads/main

worktree /home/user/project/.atteler/worktrees/session-1
HEAD def456
branch refs/heads/atteler/session-1

worktree /home/user/project/.atteler/worktrees/session-2
HEAD ghi789
branch refs/heads/atteler/session-2

worktree /home/user/project/.atteler/worktrees/other
HEAD jkl012
branch refs/heads/feature/other

`

	// parseWorktreeList will try to call gitCurrentBranch which will fail
	// on a fake repo root, so we test the parsing logic directly.
	results := parseWorktreeList(input, t.TempDir())

	if len(results) != 2 {
		require.Failf(t, "unexpected failure", "expected 2 atteler worktrees, got %d", len(results))
	}

	if results[0].SessionID != "session-1" {
		assert.Failf(t, "assertion failed", "first session ID = %q, want %q", results[0].SessionID, "session-1")
	}

	if results[1].SessionID != "session-2" {
		assert.Failf(t, "assertion failed", "second session ID = %q, want %q", results[1].SessionID, "session-2")
	}

	if results[0].Branch != "atteler/session-1" {
		assert.Failf(t, "assertion failed", "first branch = %q, want %q", results[0].Branch, "atteler/session-1")
	}
}
