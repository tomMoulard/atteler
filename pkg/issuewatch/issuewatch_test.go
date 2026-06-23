package issuewatch

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/tommoulard/atteler/pkg/permission"
	"github.com/tommoulard/atteler/pkg/symphony"
	"github.com/tommoulard/atteler/pkg/worktree"
)

func TestSelectCandidatesFiltersLabelsAndState(t *testing.T) {
	t.Parallel()

	state := State{Runs: map[string]StateRun{
		"node-processed": {RunID: "20260620T120000Z-GH-3", Status: statusNeedsHuman},
	}}
	issues := []symphony.Issue{
		{ID: "node-1", Identifier: "GH-1", Title: "ready", Labels: []string{"Atteler-Agent", "bug"}},
		{ID: "node-2", Identifier: "GH-2", Title: "wrong label", Labels: []string{"bug"}},
		{ID: "node-processed", Identifier: "GH-3", Title: "already processed", Labels: []string{"atteler-agent"}},
		{ID: "node-closed", Identifier: "GH-4", Title: "closed", State: "CLOSED", Labels: []string{"atteler-agent"}},
		{Identifier: "", Title: "missing key", Labels: []string{"atteler-agent"}},
	}

	candidates := SelectCandidates(issues, []string{"atteler-agent"}, state)

	require.Len(t, candidates, 1)
	assert.Equal(t, "GH-1", candidates[0].Identifier)
	assert.Equal(t, "node-1", candidates[0].StateKey)
	assert.Equal(t, "GH-1", candidates[0].IssueIDPath)
}

func TestSelectCandidatesDeduplicatesAfterEligibility(t *testing.T) {
	t.Parallel()

	issues := []symphony.Issue{
		{ID: "node-1", Identifier: "GH-1", Title: "first wrong label", State: "OPEN", Labels: []string{"bug"}},
		{ID: "node-1", Identifier: "GH-1", Title: "ready duplicate", State: "OPEN", Labels: []string{"atteler-agent"}},
		{ID: "node-2", Identifier: "GH-2", Title: "first closed", State: "CLOSED", Labels: []string{"atteler-agent"}},
		{ID: "node-2", Identifier: "GH-2", Title: "ready after closed", State: "OPEN", Labels: []string{"atteler-agent"}},
		{ID: "node-2", Identifier: "GH-2", Title: "extra eligible duplicate", State: "OPEN", Labels: []string{"atteler-agent"}},
	}

	candidates := SelectCandidates(issues, []string{"atteler-agent"}, emptyState())

	require.Len(t, candidates, 2)
	assert.Equal(t, "ready duplicate", candidates[0].Title)
	assert.Equal(t, "ready after closed", candidates[1].Title)
}

func TestSelectCandidatesAllowsNewRunWhenIssueUpdatedAfterState(t *testing.T) {
	t.Parallel()

	firstSeen := time.Date(2026, 6, 20, 9, 0, 0, 0, time.UTC)
	updatedLater := time.Date(2026, 6, 20, 10, 0, 0, 0, time.UTC)
	state := State{Runs: map[string]StateRun{
		"node-unchanged": {
			IssueUpdatedAt: &firstSeen,
			RunID:          "20260620T090000Z-GH-1",
			Status:         statusNeedsHuman,
		},
		"node-updated": {
			IssueUpdatedAt: &firstSeen,
			RunID:          "20260620T090000Z-GH-2",
			Status:         statusNeedsHuman,
		},
		"node-legacy": {
			RunID:  "20260620T090000Z-GH-3",
			Status: statusNeedsHuman,
		},
	}}
	issues := []symphony.Issue{
		{UpdatedAt: &firstSeen, ID: "node-unchanged", Identifier: "GH-1", Title: "same", Labels: []string{"atteler-agent"}},
		{UpdatedAt: &updatedLater, ID: "node-updated", Identifier: "GH-2", Title: "changed", Labels: []string{"atteler-agent"}},
		{UpdatedAt: &updatedLater, ID: "node-legacy", Identifier: "GH-3", Title: "legacy state", Labels: []string{"atteler-agent"}},
	}

	candidates := SelectCandidates(issues, []string{"atteler-agent"}, state)

	require.Len(t, candidates, 1)
	assert.Equal(t, "GH-2", candidates[0].Identifier)
}

func TestRunOnceIgnoreStateSelectsAlreadyProcessedIssue(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	statePath := filepath.Join(root, defaultStatePath)
	state := State{Runs: map[string]StateRun{
		"node-1": {RunID: "20260620T110000Z-GH-1", Status: statusNeedsHuman},
	}}
	require.NoError(t, SaveState(statePath, state))

	result, err := RunOnce(t.Context(), Options{
		Root:        root,
		StatePath:   statePath,
		Repository:  "owner/repo",
		Labels:      []string{"atteler-agent"},
		Tracker:     fakeTracker{issues: []symphony.Issue{{ID: "node-1", Identifier: "GH-1", Title: "rerun", Labels: []string{"atteler-agent"}}}},
		DryRun:      true,
		IgnoreState: true,
		Now:         fixedIssueWatchNow,
	})
	require.NoError(t, err)
	require.Len(t, result.Candidates, 1)
	assert.Equal(t, "GH-1", result.Candidates[0].Identifier)
}

func TestRunOnceDryRunDoesNotCreateArtifactsOrState(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	createCalled := false
	tracker := fakeTracker{issues: []symphony.Issue{{
		ID:         "node-1",
		Identifier: "GH-1",
		Title:      "ready",
		State:      "OPEN",
		Labels:     []string{"atteler-agent"},
	}}}

	result, err := RunOnce(t.Context(), Options{
		Root:       root,
		Repository: "owner/repo",
		Labels:     []string{"atteler-agent"},
		Tracker:    tracker,
		CreateWorktree: func(context.Context, string, string) (*worktree.Info, error) {
			createCalled = true
			return &worktree.Info{}, nil
		},
		DryRun: true,
		Now:    fixedIssueWatchNow,
	})
	require.NoError(t, err)

	require.Len(t, result.Candidates, 1)
	assert.True(t, result.DryRun)
	assert.Empty(t, result.Runs)
	assert.False(t, createCalled)
	assert.NoFileExists(t, filepath.Join(root, defaultStatePath))
	assert.NoDirExists(t, filepath.Join(root, defaultRunsRoot))
}

func TestRunOnceRespectsLocalWritePermissionBeforeArtifacts(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	tracker := fakeTracker{issues: []symphony.Issue{{
		ID:         "node-1",
		Identifier: "GH-1",
		Title:      "ready",
		State:      "OPEN",
		Labels:     []string{"atteler-agent"},
	}}}
	policy := permission.DefaultPolicy()
	policy.SetMode(permission.OperationWrite, permission.ModeDeny)
	ctx := permission.ContextWithPolicy(t.Context(), &policy)
	createCalled := false

	result, err := RunOnce(ctx, Options{
		Root:       root,
		Repository: "owner/repo",
		Labels:     []string{"atteler-agent"},
		Tracker:    tracker,
		CreateWorktree: func(context.Context, string, string) (*worktree.Info, error) {
			createCalled = true
			return &worktree.Info{}, nil
		},
		Now: fixedIssueWatchNow,
	})
	require.Error(t, err)
	assert.True(t, permission.ErrDenied(err))
	assert.False(t, createCalled)
	assert.Len(t, result.Candidates, 1)
	assert.Empty(t, result.Runs)
	assert.NoFileExists(t, filepath.Join(root, defaultStatePath))
	assert.NoDirExists(t, filepath.Join(root, defaultRunsRoot))
}

func TestRunOnceResolvesCustomRelativeStateAndRunsRootUnderRepository(t *testing.T) {
	root := initIssueWatchRepo(t)
	worktreeRoot := filepath.Join(t.TempDir(), "worktrees")
	t.Setenv(worktree.EnvDir, worktreeRoot)

	tracker := fakeTracker{issues: []symphony.Issue{{
		ID:         "node-7",
		Identifier: "GH-7",
		Title:      "custom dirs",
		State:      "OPEN",
		Labels:     []string{"atteler-agent"},
	}}}

	result, err := RunOnce(t.Context(), Options{
		Root:       root,
		StatePath:  "local/state.json",
		RunsRoot:   "local/runs",
		Repository: "owner/repo",
		Labels:     []string{"atteler-agent"},
		Tracker:    tracker,
		Now:        fixedIssueWatchNow,
	})
	require.NoError(t, err)
	require.Len(t, result.Runs, 1)

	assert.Equal(t, filepath.Join(root, "local", "state.json"), result.StatePath)
	assert.Equal(t, filepath.Join(root, "local", "runs", "GH-7", result.Runs[0].RunID), result.Runs[0].Artifacts.Dir)
	assert.FileExists(t, filepath.Join(root, "local", "state.json"))

	excludeData, err := os.ReadFile(filepath.Join(root, ".git", "info", "exclude"))
	require.NoError(t, err)
	assert.Contains(t, string(excludeData), "/local/")
}

func TestRunOnceCreatesLocalRunArtifactsAndWorktree(t *testing.T) {
	root := initIssueWatchRepo(t)
	worktreeRoot := filepath.Join(t.TempDir(), "worktrees")
	t.Setenv(worktree.EnvDir, worktreeRoot)

	issueURL := "https://github.com/owner/repo/issues/232"
	commentURL := "https://github.com/owner/repo/issues/232#issuecomment-1"
	description := "Implement an issue watch command."
	updatedAt := time.Date(2026, 6, 20, 9, 30, 0, 0, time.UTC)
	commentCreatedAt := time.Date(2026, 6, 20, 10, 0, 0, 0, time.UTC)
	tracker := fakeTracker{issues: []symphony.Issue{{
		UpdatedAt:   &updatedAt,
		Description: &description,
		URL:         &issueURL,
		ID:          "node-232",
		Identifier:  "GH-232",
		Title:       "Add issue watch",
		State:       "OPEN",
		Labels:      []string{"atteler-agent", "enhancement"},
		Comments: []symphony.IssueComment{{
			CreatedAt: &commentCreatedAt,
			URL:       &commentURL,
			Author:    "maintainer",
			Body:      "Please keep the watcher local-only and evidence-first.",
		}},
	}}}

	result, err := RunOnce(t.Context(), Options{
		Root:       root,
		Repository: "owner/repo",
		Labels:     []string{"atteler-agent"},
		Tracker:    tracker,
		Now:        fixedIssueWatchNow,
	})
	require.NoError(t, err)

	require.Len(t, result.Runs, 1)
	run := result.Runs[0]
	assert.Equal(t, RunSchemaVersion, run.SchemaVersion)
	assert.Equal(t, statusNeedsHuman, run.Status)
	assert.Equal(t, "GH-232", run.IssueIdentifier)
	assert.Equal(t, "atteler/issue-GH-232-20260620T120000Z", run.WorktreeBranch)
	require.DirExists(t, run.WorktreePath)
	assert.FileExists(t, filepath.Join(run.WorktreePath, "AGENTS.md"))

	for _, path := range []string{
		run.Artifacts.IssueJSON,
		run.Artifacts.Plan,
		run.Artifacts.Implementation,
		run.Artifacts.ValidationLog,
		run.Artifacts.Patch,
		run.Artifacts.RunJSON,
	} {
		assert.FileExists(t, path)
	}

	planData, err := os.ReadFile(run.Artifacts.Plan)
	require.NoError(t, err)

	plan := string(planData)
	assert.Contains(t, plan, "AGENTS.md")
	assert.Contains(t, plan, ".cursor/rules/style.mdc")
	assert.Contains(t, plan, "Implement an issue watch command.")
	assert.Contains(t, plan, "Comment 1 by maintainer")
	assert.Contains(t, plan, "Please keep the watcher local-only and evidence-first.")
	assert.Contains(t, plan, "local-only")
	assert.Contains(t, plan, "does not push branches, open pull requests, or post tracker comments")
	assert.Contains(t, plan, "Atteler refreshes implementation.md, validation.log, patch.diff, and run.json")
	assert.NotContains(t, plan, "append command evidence to validation.log")

	validationData, err := os.ReadFile(run.Artifacts.ValidationLog)
	require.NoError(t, err)
	assert.Contains(t, string(validationData), "validation_run: false")

	implementationData, err := os.ReadFile(run.Artifacts.Implementation)
	require.NoError(t, err)

	implementation := string(implementationData)
	assert.Contains(t, implementation, "## Validation")
	assert.Contains(t, implementation, "validation not run")
	assert.Contains(t, implementation, "## Evidence checklist")
	assert.Contains(t, implementation, "Files changed")
	assert.Contains(t, implementation, "Tests run")

	runData, err := os.ReadFile(run.Artifacts.RunJSON)
	require.NoError(t, err)

	var stored RunMetadata
	require.NoError(t, json.Unmarshal(runData, &stored))
	assert.Equal(t, run.RunID, stored.RunID)
	require.NotNil(t, stored.IssueURL)
	assert.Len(t, stored.Guidance, 2)
	assert.Equal(t, run.WorktreePath, stored.WorktreePath)
	assert.Equal(t, "validation not run; issue watch MVP prepared a local worktree and artifacts only", stored.Validation.Summary)

	issueData, err := os.ReadFile(run.Artifacts.IssueJSON)
	require.NoError(t, err)

	var storedIssue Candidate
	require.NoError(t, json.Unmarshal(issueData, &storedIssue))
	require.NotNil(t, storedIssue.Description)
	assert.Equal(t, description, *storedIssue.Description)

	state, err := LoadState(filepath.Join(root, defaultStatePath))
	require.NoError(t, err)

	recorded := state.Runs["node-232"]
	assert.Equal(t, run.RunID, recorded.RunID)
	assert.Equal(t, statusNeedsHuman, recorded.Status)
	require.NotNil(t, recorded.IssueUpdatedAt)
	assert.Equal(t, updatedAt, recorded.IssueUpdatedAt.UTC())

	excludeData, err := os.ReadFile(filepath.Join(root, ".git", "info", "exclude"))
	require.NoError(t, err)
	assert.Contains(t, string(excludeData), "/.atteler/issue-watch/")
	assert.Contains(t, string(excludeData), "/.atteler/runs/issues/")

	second, err := RunOnce(t.Context(), Options{
		Root:       root,
		Repository: "owner/repo",
		Labels:     []string{"atteler-agent"},
		Tracker:    tracker,
		Now:        fixedIssueWatchNow,
	})
	require.NoError(t, err)
	assert.Empty(t, second.Candidates)
	assert.Empty(t, second.Runs)

	laterUpdatedAt := updatedAt.Add(time.Hour)
	updatedDescription := "Updated issue instructions."
	updatedTracker := fakeTracker{issues: []symphony.Issue{{
		UpdatedAt:   &laterUpdatedAt,
		Description: &updatedDescription,
		URL:         &issueURL,
		ID:          "node-232",
		Identifier:  "GH-232",
		Title:       "Add issue watch",
		State:       "OPEN",
		Labels:      []string{"atteler-agent", "enhancement"},
	}}}

	third, err := RunOnce(t.Context(), Options{
		Root:       root,
		Repository: "owner/repo",
		Labels:     []string{"atteler-agent"},
		Tracker:    updatedTracker,
		Now: func() time.Time {
			return fixedIssueWatchNow().Add(time.Hour)
		},
	})
	require.NoError(t, err)
	require.Len(t, third.Candidates, 1)
	require.Len(t, third.Runs, 1)
	assert.Equal(t, "20260620T130000Z-GH-232", third.Runs[0].RunID)

	state, err = LoadState(filepath.Join(root, defaultStatePath))
	require.NoError(t, err)

	recorded = state.Runs["node-232"]
	assert.Equal(t, third.Runs[0].RunID, recorded.RunID)
	require.NotNil(t, recorded.IssueUpdatedAt)
	assert.Equal(t, laterUpdatedAt, recorded.IssueUpdatedAt.UTC())
}

func TestRunOnceExecutesLocalWorkflowInWorktreeAndCapturesEvidence(t *testing.T) {
	root := initIssueWatchRepo(t)
	t.Setenv(worktree.EnvDir, filepath.Join(t.TempDir(), "worktrees"))

	tracker := fakeTracker{issues: []symphony.Issue{{
		ID:         "node-8",
		Identifier: "GH-8",
		Title:      "workflow",
		State:      "OPEN",
		Labels:     []string{"atteler-agent"},
	}}}

	result, err := RunOnce(t.Context(), Options{
		Root:               root,
		Repository:         "owner/repo",
		Labels:             []string{"atteler-agent"},
		Tracker:            tracker,
		Command:            "printf 'impl-out\\n'; printf '\\nissue-watch-change\\n' >> README.md; mkdir -p notes; printf 'new evidence\\n' > notes/evidence.txt",
		ValidationCommands: []string{"printf 'validation-out\\n'; grep -q issue-watch-change README.md; grep -q 'new evidence' notes/evidence.txt"},
		Now:                fixedIssueWatchNow,
	})
	require.NoError(t, err)
	require.Len(t, result.Runs, 1)

	run := result.Runs[0]
	assert.Equal(t, statusSucceeded, run.Status)
	assert.True(t, run.Workflow.Run)
	assert.True(t, run.Workflow.Passed)
	assert.True(t, run.Validation.Run)
	assert.True(t, run.Validation.Passed)
	assert.Contains(t, run.ChangedFiles, "M README.md")
	assert.Contains(t, run.ChangedFiles, "?? notes/evidence.txt")

	patchData, err := os.ReadFile(run.Artifacts.Patch)
	require.NoError(t, err)
	assert.Contains(t, string(patchData), "+issue-watch-change")
	assert.Contains(t, string(patchData), "+++ b/notes/evidence.txt")
	assert.Contains(t, string(patchData), "+new evidence")

	logData, err := os.ReadFile(run.Artifacts.ValidationLog)
	require.NoError(t, err)

	log := string(logData)
	assert.Contains(t, log, "impl-out")
	assert.Contains(t, log, "validation-out")

	implementationData, err := os.ReadFile(run.Artifacts.Implementation)
	require.NoError(t, err)

	implementation := string(implementationData)
	assert.Contains(t, implementation, "implementation command passed")
	assert.Contains(t, implementation, "validation passed")
	assert.Contains(t, implementation, "M README.md")
	assert.Contains(t, implementation, "?? notes/evidence.txt")

	runData, err := os.ReadFile(run.Artifacts.RunJSON)
	require.NoError(t, err)

	var stored RunMetadata
	require.NoError(t, json.Unmarshal(runData, &stored))
	assert.Equal(t, statusSucceeded, stored.Status)
	assert.True(t, stored.Workflow.Run)
	assert.True(t, stored.Workflow.Passed)
	assert.True(t, stored.Validation.Run)
	assert.True(t, stored.Validation.Passed)
	assert.Contains(t, stored.ChangedFiles, "M README.md")
	assert.Contains(t, stored.ChangedFiles, "?? notes/evidence.txt")
	require.Len(t, stored.Validation.Results, 1)
	assert.Contains(t, stored.Validation.Results[0].Stdout, "validation-out")

	state, err := LoadState(filepath.Join(root, defaultStatePath))
	require.NoError(t, err)
	assert.Equal(t, statusSucceeded, state.Runs["node-8"].Status)
}

func TestRunOnceRecordsFailedValidationWithImplementationEvidence(t *testing.T) {
	root := initIssueWatchRepo(t)
	t.Setenv(worktree.EnvDir, filepath.Join(t.TempDir(), "worktrees"))

	tracker := fakeTracker{issues: []symphony.Issue{{
		ID:         "node-81",
		Identifier: "GH-81",
		Title:      "validation fail",
		State:      "OPEN",
		Labels:     []string{"atteler-agent"},
	}}}

	result, err := RunOnce(t.Context(), Options{
		Root:               root,
		Repository:         "owner/repo",
		Labels:             []string{"atteler-agent"},
		Tracker:            tracker,
		Command:            "printf '\\nvalidation-failure-change\\n' >> README.md",
		ValidationCommands: []string{"printf 'validation-fail\\n' >&2; exit 4"},
		Now:                fixedIssueWatchNow,
	})
	require.NoError(t, err)
	require.Len(t, result.Runs, 1)

	run := result.Runs[0]
	assert.Equal(t, statusFailed, run.Status)
	assert.True(t, run.Workflow.Run)
	assert.True(t, run.Workflow.Passed)
	assert.True(t, run.Validation.Run)
	assert.False(t, run.Validation.Passed)
	assert.Contains(t, run.Validation.Summary, "validation failed")
	assert.Contains(t, run.ChangedFiles, "M README.md")
	require.Len(t, run.Validation.Results, 1)
	assert.False(t, run.Validation.Results[0].Passed)

	patchData, err := os.ReadFile(run.Artifacts.Patch)
	require.NoError(t, err)
	assert.Contains(t, string(patchData), "+validation-failure-change")

	logData, err := os.ReadFile(run.Artifacts.ValidationLog)
	require.NoError(t, err)

	log := string(logData)
	assert.Contains(t, log, "validation-fail")
	assert.Contains(t, log, "validation_passed: false")

	state, err := LoadState(filepath.Join(root, defaultStatePath))
	require.NoError(t, err)
	assert.Equal(t, statusFailed, state.Runs["node-81"].Status)
}

func TestRunOnceRecordsFailedLocalWorkflowWithoutValidation(t *testing.T) {
	root := initIssueWatchRepo(t)
	t.Setenv(worktree.EnvDir, filepath.Join(t.TempDir(), "worktrees"))

	tracker := fakeTracker{issues: []symphony.Issue{{
		ID:         "node-9",
		Identifier: "GH-9",
		Title:      "workflow fail",
		State:      "OPEN",
		Labels:     []string{"atteler-agent"},
	}}}

	result, err := RunOnce(t.Context(), Options{
		Root:               root,
		Repository:         "owner/repo",
		Labels:             []string{"atteler-agent"},
		Tracker:            tracker,
		Command:            "printf 'boom\\n' >&2; exit 7",
		ValidationCommands: []string{"printf 'should-not-run\\n'"},
		Now:                fixedIssueWatchNow,
	})
	require.NoError(t, err)
	require.Len(t, result.Runs, 1)

	run := result.Runs[0]
	assert.Equal(t, statusFailed, run.Status)
	assert.True(t, run.Workflow.Run)
	assert.False(t, run.Workflow.Passed)
	assert.False(t, run.Validation.Run)
	assert.Contains(t, run.Validation.Summary, "validation skipped")

	logData, err := os.ReadFile(run.Artifacts.ValidationLog)
	require.NoError(t, err)

	log := string(logData)
	assert.Contains(t, log, "boom")
	assert.NotContains(t, log, "should-not-run")

	state, err := LoadState(filepath.Join(root, defaultStatePath))
	require.NoError(t, err)
	assert.Equal(t, statusFailed, state.Runs["node-9"].Status)
}

func TestRunOnceBlocksNetworkLocalWorkflowByDefault(t *testing.T) {
	root := initIssueWatchRepo(t)
	t.Setenv(worktree.EnvDir, filepath.Join(t.TempDir(), "worktrees"))

	tracker := fakeTracker{issues: []symphony.Issue{{
		ID:         "node-10",
		Identifier: "GH-10",
		Title:      "network safety",
		State:      "OPEN",
		Labels:     []string{"atteler-agent"},
	}}}

	result, err := RunOnce(t.Context(), Options{
		Root:               root,
		Repository:         "owner/repo",
		Labels:             []string{"atteler-agent"},
		Tracker:            tracker,
		Command:            "curl https://example.com/publish",
		ValidationCommands: []string{"printf 'should-not-run\\n'"},
		Now:                fixedIssueWatchNow,
	})
	require.NoError(t, err)
	require.Len(t, result.Runs, 1)

	run := result.Runs[0]
	assert.Equal(t, statusFailed, run.Status)
	assert.True(t, run.Workflow.Run)
	require.NotNil(t, run.Workflow.Result)
	assert.False(t, run.Workflow.Result.Passed)
	assert.Contains(t, run.Workflow.Result.Error, "permission.network.deny")
	assert.False(t, run.Validation.Run)

	logData, err := os.ReadFile(run.Artifacts.ValidationLog)
	require.NoError(t, err)

	log := string(logData)
	assert.Contains(t, log, "permission.network.deny")
	assert.NotContains(t, log, "should-not-run")

	state, err := LoadState(filepath.Join(root, defaultStatePath))
	require.NoError(t, err)
	assert.Equal(t, statusFailed, state.Runs["node-10"].Status)
}

func TestRunOnceBlocksGitPushLocalWorkflowByDefault(t *testing.T) {
	root := initIssueWatchRepo(t)
	t.Setenv(worktree.EnvDir, filepath.Join(t.TempDir(), "worktrees"))

	tracker := fakeTracker{issues: []symphony.Issue{{
		ID:         "node-11",
		Identifier: "GH-11",
		Title:      "push safety",
		State:      "OPEN",
		Labels:     []string{"atteler-agent"},
	}}}

	result, err := RunOnce(t.Context(), Options{
		Root:               root,
		Repository:         "owner/repo",
		Labels:             []string{"atteler-agent"},
		Tracker:            tracker,
		Command:            "git push origin HEAD > pushed",
		ValidationCommands: []string{"printf 'should-not-run\\n'"},
		Now:                fixedIssueWatchNow,
	})
	require.NoError(t, err)
	require.Len(t, result.Runs, 1)

	run := result.Runs[0]
	assert.Equal(t, statusFailed, run.Status)
	assert.True(t, run.Workflow.Run)
	require.NotNil(t, run.Workflow.Result)
	assert.False(t, run.Workflow.Result.Passed)
	assert.Contains(t, run.Workflow.Result.Error, "permission.")
	assert.False(t, run.Validation.Run)
	assert.NoFileExists(t, filepath.Join(run.WorktreePath, "pushed"))

	logData, err := os.ReadFile(run.Artifacts.ValidationLog)
	require.NoError(t, err)

	log := string(logData)
	assert.Contains(t, log, "permission.")
	assert.NotContains(t, log, "should-not-run")

	state, err := LoadState(filepath.Join(root, defaultStatePath))
	require.NoError(t, err)
	assert.Equal(t, statusFailed, state.Runs["node-11"].Status)
}

func TestDiscoverGuidanceFindsHarnessInstructionFiles(t *testing.T) {
	t.Parallel()

	root := t.TempDir()

	files := map[string]string{
		"AGENTS.md":                         "agent rules",
		"CLAUDE.md":                         "claude rules",
		"GEMINI.md":                         "gemini rules",
		"CODEX.md":                          "codex rules",
		".cursorrules":                      "cursor root rules",
		".cursor/rules/go.mdc":              "cursor project rules",
		".github/copilot-instructions.md":   "copilot rules",
		".atteler/runs/issues/AGENTS.md":    "generated run rules",
		"node_modules/example/CLAUDE.md":    "dependency rules",
		"vendor/example/.cursor/rules/x.md": "vendor rules",
	}

	for path, content := range files {
		fullPath := filepath.Join(root, filepath.FromSlash(path))
		require.NoError(t, os.MkdirAll(filepath.Dir(fullPath), 0o750))
		require.NoError(t, os.WriteFile(fullPath, []byte(content), 0o600))
	}

	guidance, err := DiscoverGuidance(root)
	require.NoError(t, err)

	kindsByPath := make(map[string]string, len(guidance))
	snippetsByPath := make(map[string]string, len(guidance))

	for _, item := range guidance {
		kindsByPath[item.Path] = item.Kind
		snippetsByPath[item.Path] = item.Snippet
	}

	assert.Equal(t, map[string]string{
		".cursor/rules/go.mdc":            "cursor_rules",
		".cursorrules":                    "cursor_rules",
		".github/copilot-instructions.md": "copilot_instructions",
		"AGENTS.md":                       "agents_instructions",
		"CLAUDE.md":                       "claude_instructions",
		"CODEX.md":                        "codex_instructions",
		"GEMINI.md":                       "gemini_instructions",
	}, kindsByPath)
	assert.Equal(t, "agent rules", snippetsByPath["AGENTS.md"])
	assert.NotContains(t, kindsByPath, ".atteler/runs/issues/AGENTS.md")
	assert.NotContains(t, kindsByPath, "node_modules/example/CLAUDE.md")
	assert.NotContains(t, kindsByPath, "vendor/example/.cursor/rules/x.md")
}

func TestDiscoverGuidanceIgnoresUnreadableHarnessFiles(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(root, "AGENTS.md"), []byte("valid guidance"), 0o600))
	require.NoError(t, os.WriteFile(filepath.Join(root, "CLAUDE.md"), []byte{0xff, 0xfe, 0xfd}, 0o600))

	guidance, err := DiscoverGuidance(root)
	require.NoError(t, err)

	require.Len(t, guidance, 1)
	assert.Equal(t, "AGENTS.md", guidance[0].Path)
	assert.Equal(t, "valid guidance", guidance[0].Snippet)
}

func TestDiscoverRunGuidanceMergesRootOnlyLocalGuidance(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	worktreePath := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(root, "AGENTS.md"), []byte("root guidance"), 0o600))
	require.NoError(t, os.WriteFile(filepath.Join(root, "CLAUDE.md"), []byte("root-only claude guidance"), 0o600))
	require.NoError(t, os.WriteFile(filepath.Join(worktreePath, "AGENTS.md"), []byte("worktree guidance"), 0o600))
	require.NoError(t, os.MkdirAll(filepath.Join(worktreePath, ".cursor", "rules"), 0o750))
	require.NoError(t, os.WriteFile(filepath.Join(worktreePath, ".cursor", "rules", "style.mdc"), []byte("worktree cursor rules"), 0o600))

	guidance, err := discoverRunGuidance(root, worktreePath)
	require.NoError(t, err)

	require.Len(t, guidance, 3)
	assert.Equal(t, ".cursor/rules/style.mdc", guidance[0].Path)
	assert.Equal(t, "AGENTS.md", guidance[1].Path)
	assert.Equal(t, "CLAUDE.md", guidance[2].Path)
	assert.Equal(t, "worktree guidance", guidance[1].Snippet)
	assert.Equal(t, "root-only claude guidance", guidance[2].Snippet)
}

func TestRunOncePersistsSuccessfulRunBeforeLaterCandidateFailure(t *testing.T) {
	t.Parallel()

	root := initIssueWatchRepo(t)
	worktreeRoot := filepath.Join(t.TempDir(), "worktrees")
	tracker := fakeTracker{issues: []symphony.Issue{
		{
			ID:         "node-1",
			Identifier: "GH-1",
			Title:      "first",
			State:      "OPEN",
			Labels:     []string{"atteler-agent"},
		},
		{
			ID:         "node-2",
			Identifier: "GH-2",
			Title:      "second",
			State:      "OPEN",
			Labels:     []string{"atteler-agent"},
		},
	}}

	calls := 0
	createWorktree := func(_ context.Context, _ string, sessionID string) (*worktree.Info, error) {
		calls++
		if calls == 2 {
			return nil, errors.New("worktree unavailable")
		}

		path := filepath.Join(worktreeRoot, sessionID)
		require.NoError(t, os.MkdirAll(path, 0o750))

		return &worktree.Info{
			Path:       path,
			Branch:     "atteler/" + sessionID,
			BaseBranch: "main",
			SessionID:  sessionID,
		}, nil
	}

	result, err := RunOnce(t.Context(), Options{
		Root:           root,
		Repository:     "owner/repo",
		Labels:         []string{"atteler-agent"},
		Tracker:        tracker,
		CreateWorktree: createWorktree,
		Now:            fixedIssueWatchNow,
	})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "create worktree for GH-2")

	require.Len(t, result.Runs, 1)
	assert.Equal(t, "GH-1", result.Runs[0].IssueIdentifier)
	assert.FileExists(t, result.Runs[0].Artifacts.RunJSON)

	state, stateErr := LoadState(filepath.Join(root, defaultStatePath))
	require.NoError(t, stateErr)
	assert.Equal(t, result.Runs[0].RunID, state.Runs["node-1"].RunID)
	assert.NotContains(t, state.Runs, "node-2")
}

func fixedIssueWatchNow() time.Time {
	return time.Date(2026, 6, 20, 12, 0, 0, 0, time.UTC)
}

//nolint:govet // Test fixture keeps primary data before optional error.
type fakeTracker struct {
	issues []symphony.Issue
	err    error
}

func (t fakeTracker) FetchCandidateIssues(_ context.Context) ([]symphony.Issue, error) {
	return append([]symphony.Issue(nil), t.issues...), t.err
}

func initIssueWatchRepo(t *testing.T) string {
	t.Helper()

	root := t.TempDir()
	runGitForIssueWatchTest(t, root, "init")
	runGitForIssueWatchTest(t, root, "config", "user.email", "issuewatch@example.test")
	runGitForIssueWatchTest(t, root, "config", "user.name", "Issue Watch Test")
	runGitForIssueWatchTest(t, root, "config", "commit.gpgsign", "false")
	runGitForIssueWatchTest(t, root, "config", "core.excludesFile", os.DevNull)
	require.NoError(t, os.WriteFile(filepath.Join(root, "AGENTS.md"), []byte("# Rules\nKeep changes small and run tests.\n"), 0o600))
	require.NoError(t, os.MkdirAll(filepath.Join(root, ".cursor", "rules"), 0o750))
	require.NoError(t, os.WriteFile(filepath.Join(root, ".cursor", "rules", "style.mdc"), []byte("# Cursor style\nPrefer simple Go code.\n"), 0o600))
	require.NoError(t, os.WriteFile(filepath.Join(root, "README.md"), []byte("# test repo\n"), 0o600))
	runGitForIssueWatchTest(t, root, "add", ".")
	runGitForIssueWatchTest(t, root, "commit", "-m", "init")

	return root
}

func runGitForIssueWatchTest(t *testing.T, dir string, args ...string) {
	t.Helper()

	cmd := exec.CommandContext(t.Context(), "git", args...)

	cmd.Dir = dir
	if out, err := cmd.CombinedOutput(); err != nil {
		require.Failf(t, "git command failed", "git %v: %s: %v", args, out, err)
	}
}
