//nolint:wsl_v5 // The integration-style review-fix test keeps git setup and artifact assertions grouped.
package main

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/tommoulard/atteler/pkg/autonomy"
	"github.com/tommoulard/atteler/pkg/reviewfix"
	"github.com/tommoulard/atteler/pkg/session"
	"github.com/tommoulard/atteler/pkg/worktree"
)

const reviewFixExamplePatch = "diff --git a/example.txt b/example.txt\n--- a/example.txt\n+++ b/example.txt\n@@ -1 +1 @@\n-old\n+new\n"

func TestRunReviewFix_AppliesSuggestedDiffAndWritesArtifacts(t *testing.T) { //nolint:paralleltest // captures process-wide stdout/stderr from command helpers.
	root := t.TempDir()
	runGitForReviewFixTest(t, root, "init")
	require.NoError(t, os.WriteFile(filepath.Join(root, "example.txt"), []byte("old\n"), 0o600))
	runGitForReviewFixTest(t, root, "add", "example.txt")
	require.NoError(t, os.WriteFile(filepath.Join(root, "AGENTS.md"), []byte("follow local test rules\n"), 0o600))

	writeReviewFixFindingInput(t, root, reviewFixExamplePatch)

	store := session.NewStore(filepath.Join(root, "sessions"))
	state := appState{
		cwd:          root,
		sessionStore: store,
		sessionState: session.New("gpt-test", nil),
		autonomy:     autonomy.Medium,
	}

	err := runReviewFix(t.Context(), state, reviewFixCommandInput{From: "review.json"})
	require.NoError(t, err)

	content, err := os.ReadFile(filepath.Join(root, "example.txt"))
	require.NoError(t, err)
	assert.Equal(t, "new\n", string(content))

	runs, err := filepath.Glob(filepath.Join(root, ".atteler", "runs", "review-fix", "*"))
	require.NoError(t, err)
	require.Len(t, runs, 1)
	for _, name := range []string{"findings.input.json", "fix-plan.md", "changes.md", "validation.log", "patch.diff", "run.json"} {
		require.FileExists(t, filepath.Join(runs[0], name))
	}

	patch, err := os.ReadFile(filepath.Join(runs[0], "patch.diff"))
	require.NoError(t, err)
	assert.Contains(t, string(patch), "+new")

	plan, err := os.ReadFile(filepath.Join(runs[0], "fix-plan.md"))
	require.NoError(t, err)
	assert.Contains(t, string(plan), "AGENTS.md")
}

func TestRunReviewFix_WritesValidationFailureArtifacts(t *testing.T) { //nolint:paralleltest // captures process-wide stdout/stderr from command helpers.
	root := t.TempDir()
	runGitForReviewFixTest(t, root, "init")
	require.NoError(t, os.WriteFile(filepath.Join(root, "example.txt"), []byte("old\n"), 0o600))
	runGitForReviewFixTest(t, root, "add", "example.txt")

	writeReviewFixFindingInput(t, root, reviewFixExamplePatch)

	store := session.NewStore(filepath.Join(root, "sessions"))
	state := appState{
		cwd:          root,
		sessionStore: store,
		sessionState: session.New("gpt-test", nil),
		autonomy:     autonomy.Medium,
	}

	err := runReviewFix(t.Context(), state, reviewFixCommandInput{
		From:               "review.json",
		ValidationCommands: []string{"printf validation-failed >&2; exit 7"},
	})
	require.ErrorContains(t, err, "review fix validation failed")

	runs, err := filepath.Glob(filepath.Join(root, ".atteler", "runs", "review-fix", "*"))
	require.NoError(t, err)
	require.Len(t, runs, 1)

	validationLog, err := os.ReadFile(filepath.Join(runs[0], "validation.log"))
	require.NoError(t, err)
	assert.Contains(t, string(validationLog), "status: failed")
	assert.Contains(t, string(validationLog), "validation-failed")

	runData, err := os.ReadFile(filepath.Join(runs[0], "run.json"))
	require.NoError(t, err)
	var record reviewfix.RunRecord
	require.NoError(t, json.Unmarshal(runData, &record))
	require.Len(t, record.Validation, 1)
	assert.Equal(t, reviewFixValidationFailed, record.Validation[0].Status)
	assert.False(t, record.RemotePublishing)
}

func TestRunReviewFixStateful_FinalizesPreservedWorktree(t *testing.T) { //nolint:paralleltest // captures process-wide stdout/stderr.
	cwd := t.TempDir()
	worktreeRoot := t.TempDir()
	runGitForReviewFixTest(t, worktreeRoot, "init")
	require.NoError(t, os.WriteFile(filepath.Join(worktreeRoot, "example.txt"), []byte("old\n"), 0o600))
	runGitForReviewFixTest(t, worktreeRoot, "add", "example.txt")

	writeReviewFixFindingInput(t, cwd, reviewFixExamplePatch)

	store := session.NewStore(filepath.Join(cwd, "sessions"))
	state := appState{
		cwd:          cwd,
		sessionStore: store,
		sessionState: session.New("gpt-test", nil),
		autonomy:     autonomy.Medium,
		worktreeInfo: &worktree.Info{
			Path:       worktreeRoot,
			Branch:     "atteler/test-review-fix",
			BaseBranch: "main",
			SessionID:  "review-fix-session",
		},
	}

	var runErr error
	stderr := captureStderr(t, func() {
		_ = captureStdoutForStateDiagnostics(t, func() {
			runErr = runReviewFixStateful(t.Context(), state, reviewFixCommandInput{From: "review.json", Worktree: true})
		})
	})
	require.NoError(t, runErr)
	assert.Contains(t, stderr, "worktree: session files are in "+worktreeRoot)
	assert.Contains(t, stderr, "worktree: merge with: atteler --merge-worktree")

	saved, err := store.Load(state.sessionState.ID)
	require.NoError(t, err)
	assert.Equal(t, worktreeRoot, saved.WorktreePath)
	assert.Equal(t, "atteler/test-review-fix", saved.WorktreeBranch)
	assert.Equal(t, "main", saved.WorktreeBase)

	patches, err := filepath.Glob(filepath.Join(worktreeRoot, ".atteler", "runs", "review-fix", "*", "patch.diff"))
	require.NoError(t, err)
	require.Len(t, patches, 1)

	content, err := os.ReadFile(filepath.Join(worktreeRoot, "example.txt"))
	require.NoError(t, err)
	assert.Equal(t, "new\n", string(content))
}

func TestReviewFixSuggestedDiffForPlan_RequiresEveryFindingToBePatchable(t *testing.T) {
	t.Parallel()

	diff := "--- a/example.txt\n+++ b/example.txt\n@@ -1 +1 @@\n-old\n+new\n"
	_, ok := reviewFixSuggestedDiffForPlan([]reviewfix.Finding{
		{SuggestedFix: diff},
		{SuggestedFix: "Please add a regression test too."},
	})

	assert.False(t, ok)

	combined, ok := reviewFixSuggestedDiffForPlan([]reviewfix.Finding{{SuggestedFix: diff}})
	require.True(t, ok)
	assert.Contains(t, combined, "+new")
}

func TestReviewFixLocalAutonomy_ClampsPublishingLevels(t *testing.T) {
	t.Parallel()

	assert.Equal(t, autonomy.Low, reviewFixLocalAutonomy(autonomy.Low))
	assert.Equal(t, autonomy.Medium, reviewFixLocalAutonomy(autonomy.Medium))
	assert.Equal(t, autonomy.Medium, reviewFixLocalAutonomy(autonomy.High))
	assert.Equal(t, autonomy.Medium, reviewFixLocalAutonomy(autonomy.Full))
}

func TestReviewFixPatchDiff_IncludesUntrackedFilesButSkipsArtifacts(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	runGitForReviewFixTest(t, root, "init")
	runGitForReviewFixTest(t, root, "config", "user.email", "atteler@example.test")
	runGitForReviewFixTest(t, root, "config", "user.name", "Atteler Test")
	runGitForReviewFixTest(t, root, "config", "commit.gpgsign", "false")
	runDir := filepath.Join(root, ".atteler", "runs", "review-fix", "run-1")
	require.NoError(t, os.MkdirAll(runDir, 0o750))
	require.NoError(t, os.WriteFile(filepath.Join(root, "tracked.txt"), []byte("old\n"), 0o600))
	require.NoError(t, os.WriteFile(filepath.Join(runDir, "run.json"), []byte("{}\n"), 0o600))
	require.NoError(t, os.WriteFile(filepath.Join(root, "review.json"), []byte("{}\n"), 0o600))
	runGitForReviewFixTest(t, root, "add", ".")
	runGitForReviewFixTest(t, root, "commit", "-m", "seed")

	require.NoError(t, os.WriteFile(filepath.Join(root, "tracked.txt"), []byte("new\n"), 0o600))
	runGitForReviewFixTest(t, root, "add", "tracked.txt")
	require.NoError(t, os.WriteFile(filepath.Join(root, "new_test.go"), []byte("package main\n"), 0o600))
	require.NoError(t, os.WriteFile(filepath.Join(runDir, "run.json"), []byte("{\"changed\":true}\n"), 0o600))
	require.NoError(t, os.WriteFile(filepath.Join(root, "review.json"), []byte("{\"changed\":true}\n"), 0o600))

	patch, err := reviewFixPatchDiff(t.Context(), root, runDir, filepath.Join(root, "review.json"))
	require.NoError(t, err)
	assert.Contains(t, patch, "tracked.txt")
	assert.Contains(t, patch, "new_test.go")
	assert.NotContains(t, patch, "\"changed\":true")
	assert.NotContains(t, patch, "review.json")
	assert.NotContains(t, patch, "run.json")

	changed, err := reviewFixChangedFiles(t.Context(), root, runDir, filepath.Join(root, "review.json"))
	require.NoError(t, err)
	assert.ElementsMatch(t, []reviewfix.ChangedFile{
		{Status: "M", Path: "tracked.txt"},
		{Status: "??", Path: "new_test.go"},
	}, changed)
}

func TestReviewFixPatchDiff_IgnoresExcludePathsOutsideWorkspace(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	runGitForReviewFixTest(t, root, "init")
	runGitForReviewFixTest(t, root, "config", "user.email", "atteler@example.test")
	runGitForReviewFixTest(t, root, "config", "user.name", "Atteler Test")
	runGitForReviewFixTest(t, root, "config", "commit.gpgsign", "false")
	require.NoError(t, os.WriteFile(filepath.Join(root, "tracked.txt"), []byte("old\n"), 0o600))
	runGitForReviewFixTest(t, root, "add", "tracked.txt")
	runGitForReviewFixTest(t, root, "commit", "-m", "seed")
	require.NoError(t, os.WriteFile(filepath.Join(root, "tracked.txt"), []byte("new\n"), 0o600))

	outsideRoot := t.TempDir()
	outsideInput := filepath.Join(outsideRoot, "review.json")
	require.NoError(t, os.WriteFile(outsideInput, []byte("{}\n"), 0o600))

	patch, err := reviewFixPatchDiff(t.Context(), root, outsideInput)
	require.NoError(t, err)
	assert.Contains(t, patch, "tracked.txt")
	assert.Empty(t, reviewFixRelativePath(root, outsideInput))
}

func writeReviewFixFindingInput(t *testing.T, root, diff string) {
	t.Helper()

	input := map[string]any{
		"reviewer": "unit-reviewer",
		"findings": []map[string]any{{
			"id":            "f-1",
			"severity":      "important",
			"file":          "example.txt",
			"line":          1,
			"message":       "replace old content",
			"suggested_fix": diff,
		}},
	}
	inputData, err := json.Marshal(input)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(filepath.Join(root, "review.json"), inputData, 0o600))
}

func runGitForReviewFixTest(t *testing.T, dir string, args ...string) {
	t.Helper()

	cmd := exec.CommandContext(t.Context(), "git", args...)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	require.NoErrorf(t, err, "git %v: %s", args, string(out))
}
