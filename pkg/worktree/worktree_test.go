package worktree

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
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
		{"git", "commit", "--allow-empty", "-m", "init"},
	}
	for _, args := range cmds {
		cmd := exec.CommandContext(context.Background(), args[0], args[1:]...) //nolint:gosec // Test helper runs static git commands.
		cmd.Dir = dir
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("setup %v: %s: %v", args, out, err)
		}
	}
	return dir
}

func TestCreateAndRemove(t *testing.T) {
	repo := initTestRepo(t)

	info, err := Create(repo, "test-session-1")
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if info.SessionID != "test-session-1" {
		t.Errorf("SessionID = %q, want %q", info.SessionID, "test-session-1")
	}
	if info.Branch != "atteler/test-session-1" {
		t.Errorf("Branch = %q, want %q", info.Branch, "atteler/test-session-1")
	}
	if info.BaseBranch == "" {
		t.Error("BaseBranch is empty")
	}
	if _, err := os.Stat(info.Path); err != nil {
		t.Errorf("worktree path does not exist: %v", err)
	}

	// Remove should clean up.
	if err := Remove(repo, info); err != nil {
		t.Fatalf("Remove: %v", err)
	}
	if _, err := os.Stat(info.Path); !os.IsNotExist(err) {
		t.Errorf("worktree path still exists after Remove")
	}
}

func TestCreateIdempotent(t *testing.T) {
	repo := initTestRepo(t)

	info1, err := Create(repo, "idempotent-1")
	if err != nil {
		t.Fatalf("first Create: %v", err)
	}

	// Second create for the same session should succeed (join).
	info2, err := Create(repo, "idempotent-1")
	if err != nil {
		t.Fatalf("second Create: %v", err)
	}
	if info2.Path != info1.Path {
		t.Errorf("paths differ: %s vs %s", info2.Path, info1.Path)
	}

	if err := Remove(repo, info1); err != nil {
		t.Fatalf("Remove: %v", err)
	}
}

func TestMerge(t *testing.T) {
	repo := initTestRepo(t)

	info, err := Create(repo, "merge-session")
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	// Write a file in the worktree and commit it.
	testFile := filepath.Join(info.Path, "hello.txt")
	if writeErr := os.WriteFile(testFile, []byte("hello\n"), 0o600); writeErr != nil {
		t.Fatalf("write test file: %v", writeErr)
	}

	if mergeErr := Merge(repo, info); mergeErr != nil {
		t.Fatalf("Merge: %v", mergeErr)
	}

	// The file should now exist in the main repo.
	mainFile := filepath.Join(repo, "hello.txt")
	data, err := os.ReadFile(mainFile)
	if err != nil {
		t.Fatalf("read merged file: %v", err)
	}
	if string(data) != "hello\n" {
		t.Errorf("merged content = %q, want %q", string(data), "hello\n")
	}

	// Worktree directory should be gone.
	if _, err := os.Stat(info.Path); !os.IsNotExist(err) {
		t.Errorf("worktree path still exists after Merge")
	}
}

func TestList(t *testing.T) {
	repo := initTestRepo(t)

	// No worktrees initially.
	infos, err := List(repo)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(infos) != 0 {
		t.Errorf("expected 0 worktrees, got %d", len(infos))
	}

	// Create two worktrees.
	info1, err := Create(repo, "list-1")
	if err != nil {
		t.Fatalf("Create list-1: %v", err)
	}
	info2, err := Create(repo, "list-2")
	if err != nil {
		t.Fatalf("Create list-2: %v", err)
	}

	infos, err = List(repo)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(infos) != 2 {
		t.Errorf("expected 2 worktrees, got %d", len(infos))
	}

	if err := Remove(repo, info1); err != nil {
		t.Fatalf("Remove info1: %v", err)
	}
	if err := Remove(repo, info2); err != nil {
		t.Fatalf("Remove info2: %v", err)
	}
}

func TestStatus(t *testing.T) {
	got := Status(nil)
	if got != "no worktree" {
		t.Errorf("Status(nil) = %q, want %q", got, "no worktree")
	}

	info := &Info{
		Path:       "/tmp/test",
		Branch:     "atteler/test",
		BaseBranch: "main",
	}
	got = Status(info)
	if !strings.Contains(got, "/tmp/test") {
		t.Errorf("Status missing path: %s", got)
	}
	if !strings.Contains(got, "atteler/test") {
		t.Errorf("Status missing branch: %s", got)
	}
}

func TestIsGitRepo(t *testing.T) {
	repo := initTestRepo(t)
	if !IsGitRepo(repo) {
		t.Error("IsGitRepo returned false for a git repo")
	}

	nonRepo := t.TempDir()
	if IsGitRepo(nonRepo) {
		t.Error("IsGitRepo returned true for a non-git dir")
	}
}

func TestCreateEmptySessionID(t *testing.T) {
	repo := initTestRepo(t)
	_, err := Create(repo, "")
	if err == nil {
		t.Error("expected error for empty session ID")
	}
}

func TestCreateNotGitRepo(t *testing.T) {
	dir := t.TempDir()
	_, err := Create(dir, "test")
	if err == nil {
		t.Error("expected error for non-git directory")
	}
}

func TestRemoveNilInfo(t *testing.T) {
	err := Remove("/tmp", nil)
	if err == nil {
		t.Error("expected error for nil info")
	}
}

func TestMergeNilInfo(t *testing.T) {
	err := Merge("/tmp", nil)
	if err == nil {
		t.Error("expected error for nil info")
	}
}

func TestParseWorktreeList(t *testing.T) {
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
		t.Fatalf("expected 2 atteler worktrees, got %d", len(results))
	}
	if results[0].SessionID != "session-1" {
		t.Errorf("first session ID = %q, want %q", results[0].SessionID, "session-1")
	}
	if results[1].SessionID != "session-2" {
		t.Errorf("second session ID = %q, want %q", results[1].SessionID, "session-2")
	}
	if results[0].Branch != "atteler/session-1" {
		t.Errorf("first branch = %q, want %q", results[0].Branch, "atteler/session-1")
	}
}
