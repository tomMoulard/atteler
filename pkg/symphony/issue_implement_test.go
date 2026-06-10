package symphony

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const testGitHubIssueURL218 = "https://github.com/owner/repo/issues/218"

var testGitHubIssueURL218Value = testGitHubIssueURL218

type issueLookupTracker struct {
	noopTracker
	byID     []Issue
	byIDErr  error
	byStates []Issue
	ids      [][]string
	states   [][]string
}

func (t *issueLookupTracker) FetchIssueStatesByIDs(_ context.Context, ids []string) ([]Issue, error) {
	t.ids = append(t.ids, append([]string(nil), ids...))
	if t.byIDErr != nil {
		return nil, t.byIDErr
	}

	return t.byID, nil
}

func (t *issueLookupTracker) FetchIssuesByStates(_ context.Context, states []string) ([]Issue, error) {
	t.states = append(t.states, append([]string(nil), states...))

	return t.byStates, nil
}

func TestImplementIssueRejectsMissingRequiredInputs(t *testing.T) {
	t.Parallel()

	_, err := ImplementIssue(nil, IssueImplementOptions{IssueRef: "GH-1"}) //nolint:staticcheck // Verify nil contexts are rejected instead of panicking.
	require.Error(t, err)
	assert.Contains(t, err.Error(), "context is required")

	_, err = ImplementIssue(t.Context(), IssueImplementOptions{IssueRef: " "})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "issue reference is required")
}

func TestIssueReferenceCandidates(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		ref  string
		want []string
	}{
		{name: "github identifier", ref: "GH-218", want: []string{"GH-218"}},
		{name: "lowercase github identifier", ref: "gh-218", want: []string{"gh-218"}},
		{name: "hash issue number", ref: "#218", want: []string{"#218", "GH-218"}},
		{name: "padded hash issue number", ref: "#0218", want: []string{"#0218", "GH-0218", "GH-218"}},
		{name: "bare issue number", ref: "218", want: []string{"218", "GH-218"}},
		{name: "padded bare issue number", ref: "0218", want: []string{"0218", "GH-0218", "GH-218"}},
		{name: "github issue url", ref: "https://github.com/owner/repo/issues/218", want: []string{"https://github.com/owner/repo/issues/218", "GH-218"}},
		{name: "empty", ref: " ", want: nil},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			assert.Equal(t, tt.want, issueReferenceCandidates(tt.ref))
		})
	}
}

func TestFetchIssueForImplementationUsesReferenceCandidates(t *testing.T) {
	t.Parallel()

	tracker := &issueLookupTracker{
		byID: []Issue{{
			Identifier: "GH-218",
			Title:      "Autonomous PR agent",
		}},
	}

	issue, err := fetchIssueForImplementation(t.Context(), tracker, Config{}, "#218")
	require.NoError(t, err)

	assert.Equal(t, "GH-218", issue.Identifier)
	assert.Equal(t, "Autonomous PR agent", issue.Title)
	assert.Equal(t, [][]string{{"#218", "GH-218"}}, tracker.ids)
	assert.Empty(t, tracker.states)
}

func TestFetchIssueForImplementationMatchesIssueURL(t *testing.T) {
	t.Parallel()

	tracker := &issueLookupTracker{
		byID: []Issue{{
			ID:         "node-218",
			Identifier: "external-issue-key",
			Title:      "Autonomous PR agent",
			URL:        &testGitHubIssueURL218Value,
		}},
	}

	issue, err := fetchIssueForImplementation(t.Context(), tracker, Config{}, testGitHubIssueURL218)
	require.NoError(t, err)

	assert.Equal(t, "node-218", issue.ID)
	assert.Equal(t, "external-issue-key", issue.Identifier)
	assert.Equal(t, [][]string{{testGitHubIssueURL218, "GH-218"}}, tracker.ids)
	assert.Empty(t, tracker.states)
}

func TestFetchIssueForImplementationKeepsGitHubRefreshIDDirect(t *testing.T) {
	t.Parallel()

	tracker := &issueLookupTracker{
		byID: []Issue{{
			ID:         "opaque-github-node-id",
			Identifier: "GH-218",
			Title:      "Autonomous PR agent",
			URL:        &testGitHubIssueURL218Value,
		}},
	}

	issue, err := fetchIssueForImplementation(t.Context(), tracker, Config{
		Tracker: TrackerConfig{
			Kind: trackerKindGitHub,
		},
	}, "GH-218")
	require.NoError(t, err)

	assert.Equal(t, "GH-218", issue.ID)
	assert.Equal(t, "GH-218", issue.Identifier)
	assert.Equal(t, [][]string{{"GH-218"}}, tracker.ids)
	assert.Empty(t, tracker.states)
}

func TestIssueImplementTrackerKeepsGitHubRefreshIDDirectAcrossTurns(t *testing.T) {
	t.Parallel()

	inner := &issueLookupTracker{
		byID: []Issue{{
			ID:         "opaque-github-node-id",
			Identifier: "GH-218",
			Title:      "Autonomous PR agent",
			URL:        &testGitHubIssueURL218Value,
		}},
	}
	tracker := issueImplementTracker(Config{
		Tracker: TrackerConfig{
			Kind: trackerKindGitHub,
		},
	}, inner)

	firstRefresh, err := tracker.FetchIssueStatesByIDs(t.Context(), []string{"GH-218"})
	require.NoError(t, err)
	require.Len(t, firstRefresh, 1)
	assert.Equal(t, "GH-218", firstRefresh[0].ID)

	secondRefresh, err := tracker.FetchIssueStatesByIDs(t.Context(), []string{firstRefresh[0].ID})
	require.NoError(t, err)
	require.Len(t, secondRefresh, 1)
	assert.Equal(t, "GH-218", secondRefresh[0].ID)
	assert.Equal(t, [][]string{{"GH-218"}, {"GH-218"}}, inner.ids)
}

func TestFetchIssueForImplementationFallsBackToConfiguredStates(t *testing.T) {
	t.Parallel()

	tracker := &issueLookupTracker{
		byStates: []Issue{{
			ID:         "node-218",
			Identifier: "GH-218",
			Title:      "Autonomous PR agent",
		}},
	}

	issue, err := fetchIssueForImplementation(t.Context(), tracker, Config{
		Tracker: TrackerConfig{
			ActiveStates:   []string{"OPEN"},
			TerminalStates: []string{"CLOSED", "DONE"},
		},
	}, "218")
	require.NoError(t, err)

	assert.Equal(t, "node-218", issue.ID)
	assert.Equal(t, "GH-218", issue.Identifier)
	assert.Equal(t, [][]string{{"218", "GH-218"}}, tracker.ids)
	assert.Equal(t, [][]string{{"OPEN", "CLOSED", "DONE"}}, tracker.states)
}

func TestFetchIssueForImplementationFallsBackToStatesAfterReferenceLookupError(t *testing.T) {
	t.Parallel()

	tracker := &issueLookupTracker{
		byIDErr: assert.AnError,
		byStates: []Issue{{
			ID:         "linear-node-218",
			Identifier: "ENG-218",
			Title:      "Autonomous PR agent",
		}},
	}

	issue, err := fetchIssueForImplementation(t.Context(), tracker, Config{
		Tracker: TrackerConfig{
			ActiveStates:   []string{"Todo"},
			TerminalStates: []string{"Done"},
		},
	}, "ENG-218")
	require.NoError(t, err)

	assert.Equal(t, "linear-node-218", issue.ID)
	assert.Equal(t, "ENG-218", issue.Identifier)
	assert.Equal(t, [][]string{{"ENG-218"}}, tracker.ids)
	assert.Equal(t, [][]string{{"Todo", "Done"}}, tracker.states)
}

func TestFetchIssueForImplementationFallsBackToStatesAndMatchesIssueURL(t *testing.T) {
	t.Parallel()

	issueURL := "https://linear.app/acme/issue/ENG-218/autonomous-pr-agent"
	tracker := &issueLookupTracker{
		byIDErr: assert.AnError,
		byStates: []Issue{{
			ID:         "linear-node-218",
			Identifier: "ENG-218",
			Title:      "Autonomous PR agent",
			URL:        &issueURL,
		}},
	}

	issue, err := fetchIssueForImplementation(t.Context(), tracker, Config{
		Tracker: TrackerConfig{
			ActiveStates: []string{"Todo"},
		},
	}, issueURL+"/")
	require.NoError(t, err)

	assert.Equal(t, "linear-node-218", issue.ID)
	assert.Equal(t, "ENG-218", issue.Identifier)
	assert.Equal(t, [][]string{{issueURL + "/"}}, tracker.ids)
	assert.Equal(t, [][]string{{"Todo"}}, tracker.states)
}

func TestFetchIssueForImplementationReturnsReferenceLookupErrorWhenFallbackMisses(t *testing.T) {
	t.Parallel()

	tracker := &issueLookupTracker{
		byIDErr: assert.AnError,
		byStates: []Issue{{
			Identifier: "GH-999",
		}},
	}

	_, err := fetchIssueForImplementation(t.Context(), tracker, Config{
		Tracker: TrackerConfig{
			ActiveStates: []string{"OPEN"},
		},
	}, "GH-218")
	require.Error(t, err)

	require.ErrorIs(t, err, assert.AnError)
	assert.Contains(t, err.Error(), "fetch issue by reference")
	assert.Equal(t, [][]string{{"GH-218"}}, tracker.ids)
	assert.Equal(t, [][]string{{"OPEN"}}, tracker.states)
}

func TestApplyIssueImplementOverridesAppendsVerificationGates(t *testing.T) {
	t.Parallel()

	cfg := Config{
		Publish: PublishConfig{
			BaseBranch:                "main",
			VerificationAllowCommands: []string{"go"},
			VerificationGates: []VerificationGateConfig{{
				Name:     "go_test",
				Command:  "go test ./...",
				Timeout:  defaultPRGateTimeout,
				Required: true,
			}},
		},
	}

	applyIssueImplementOverrides(&cfg, IssueImplementOptions{
		BaseBranch:      "release/next",
		OpenPullRequest: true,
		RunTests:        true,
		RunLint:         true,
	})

	assert.True(t, cfg.Publish.Enabled)
	assert.Equal(t, "release/next", cfg.Publish.BaseBranch)
	require.Len(t, cfg.Publish.VerificationGates, 2)
	assert.Equal(t, "go_test", cfg.Publish.VerificationGates[0].Name)
	assert.Equal(t, "golangci_lint", cfg.Publish.VerificationGates[1].Name)
	assert.Equal(t, "make lint", cfg.Publish.VerificationGates[1].Command)
	assert.Equal(t, []string{"go", "make"}, cfg.Publish.VerificationAllowCommands)
}

func TestApplyIssueImplementOverridesSeedsAllowListForFlagOnlyGates(t *testing.T) {
	t.Parallel()

	cfg := Config{}

	applyIssueImplementOverrides(&cfg, IssueImplementOptions{
		OpenPullRequest: true,
		RunTests:        true,
		RunLint:         true,
	})

	require.Len(t, cfg.Publish.VerificationGates, 2)
	assert.Equal(t, "go_test", cfg.Publish.VerificationGates[0].Name)
	assert.Equal(t, "golangci_lint", cfg.Publish.VerificationGates[1].Name)
	assert.Equal(t, []string{"go", "make"}, cfg.Publish.VerificationAllowCommands)
}

func TestApplyIssueImplementOverridesDoesNotRestrictExistingGatesWithoutAllowList(t *testing.T) {
	t.Parallel()

	cfg := Config{
		Publish: PublishConfig{
			VerificationGates: []VerificationGateConfig{{
				Name:     "custom",
				Command:  "test -f README.md",
				Timeout:  defaultPRGateTimeout,
				Required: true,
			}},
		},
	}

	applyIssueImplementOverrides(&cfg, IssueImplementOptions{
		OpenPullRequest: true,
		RunTests:        true,
	})

	require.Len(t, cfg.Publish.VerificationGates, 2)
	assert.Equal(t, "custom", cfg.Publish.VerificationGates[0].Name)
	assert.Equal(t, "go_test", cfg.Publish.VerificationGates[1].Name)
	assert.Empty(t, cfg.Publish.VerificationAllowCommands)
}

func TestApplyIssueImplementOverridesDisablesPublishUnlessOpenPR(t *testing.T) {
	t.Parallel()

	cfg := Config{
		Publish: PublishConfig{
			Enabled: true,
		},
	}

	applyIssueImplementOverrides(&cfg, IssueImplementOptions{})

	assert.False(t, cfg.Publish.Enabled)
}

func TestLoadIssueImplementWorkflowAppliesOpenPROverrideAfterSchedulerValidation(t *testing.T) {
	t.Setenv(githubTokenEnv, "token")
	dir := t.TempDir()
	workflowPath := filepath.Join(dir, "WORKFLOW.md")
	require.NoError(t, os.WriteFile(workflowPath, []byte(`---
tracker:
  kind: github
  repository: owner/repo
publish:
  enabled: true
---
Implement {{ issue.identifier }}.
`), 0o600))

	snapshot, err := loadIssueImplementWorkflow(t.Context(), IssueImplementOptions{
		WorkflowPath: workflowPath,
	})
	require.NoError(t, err)
	assert.False(t, snapshot.Config.Publish.Enabled)

	openPROpts := IssueImplementOptions{
		WorkflowPath:    workflowPath,
		OpenPullRequest: true,
	}
	snapshot, err = loadIssueImplementWorkflow(t.Context(), openPROpts)
	require.NoError(t, err)
	applyIssueImplementOverrides(&snapshot.Config, openPROpts)
	require.NoError(t, snapshot.Config.validateIssueImplementPublishConfig())
	assert.True(t, snapshot.Config.Publish.Enabled)
	assert.Empty(t, snapshot.Config.Publish.RemoveLabels)
}

func TestLoadIssueImplementWorkflowRejectsMalformedPublishConfig(t *testing.T) {
	t.Setenv(githubTokenEnv, "token")
	dir := t.TempDir()
	workflowPath := filepath.Join(dir, "WORKFLOW.md")
	require.NoError(t, os.WriteFile(workflowPath, []byte(`---
tracker:
  kind: github
  repository: owner/repo
publish: true
---
Implement {{ issue.identifier }}.
`), 0o600))

	_, err := loadIssueImplementWorkflow(t.Context(), IssueImplementOptions{
		WorkflowPath: workflowPath,
	})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "publish config must be a map")
}

func TestImplementIssueValidatesOpenPRVerificationGatesAfterPreflight(t *testing.T) {
	t.Setenv(githubTokenEnv, "token")
	dir := t.TempDir()
	workflowPath := filepath.Join(dir, "WORKFLOW.md")
	require.NoError(t, os.WriteFile(workflowPath, []byte(`---
tracker:
  kind: github
  repository: owner/repo
publish:
  enabled: true
  verification_gates:
    - security_scan
---
Implement {{ issue.identifier }}.
`), 0o600))

	_, err := ImplementIssue(t.Context(), IssueImplementOptions{
		WorkflowPath:    workflowPath,
		IssueRef:        "GH-12",
		OpenPullRequest: true,
	})
	require.Error(t, err)
	assert.Contains(t, err.Error(), `publish.verification_gates "security_scan" requires command or a supported preset name`)
}

func TestIssueImplementPreflightConfigDoesNotMutateWorkflowConfig(t *testing.T) {
	t.Parallel()

	original := map[string]any{
		"publish": map[string]any{
			"enabled": true,
			"verification_gates": []any{
				map[string]any{"name": "unit"},
			},
		},
	}

	overridden, err := issueImplementPreflightConfig(original)
	require.NoError(t, err)

	overriddenPublish, ok := overridden["publish"].(map[string]any)
	require.True(t, ok)
	originalPublish, ok := original["publish"].(map[string]any)
	require.True(t, ok)
	overriddenEnabled, ok := overriddenPublish["enabled"].(bool)
	require.True(t, ok)
	originalEnabled, ok := originalPublish["enabled"].(bool)
	require.True(t, ok)
	assert.False(t, overriddenEnabled)
	assert.True(t, originalEnabled)

	overriddenGates, ok := overriddenPublish["verification_gates"].([]any)
	require.True(t, ok)
	require.NotEmpty(t, overriddenGates)
	overriddenGate, ok := overriddenGates[0].(map[string]any)
	require.True(t, ok)

	overriddenGate["name"] = "mutated"

	originalGates, ok := originalPublish["verification_gates"].([]any)
	require.True(t, ok)
	require.NotEmpty(t, originalGates)
	originalGate, ok := originalGates[0].(map[string]any)
	require.True(t, ok)
	assert.Equal(t, "unit", originalGate["name"])
}

func TestApplyIssueImplementOverridesPromotesExistingGateCommand(t *testing.T) {
	t.Parallel()

	cfg := Config{
		Publish: PublishConfig{
			VerificationGates: []VerificationGateConfig{{
				Name:     "unit",
				Command:  "go   test ./...",
				Timeout:  defaultPRGateTimeout,
				Required: false,
			}},
		},
	}

	applyIssueImplementOverrides(&cfg, IssueImplementOptions{
		OpenPullRequest: true,
		RunTests:        true,
	})

	require.Len(t, cfg.Publish.VerificationGates, 1)
	assert.Equal(t, "unit", cfg.Publish.VerificationGates[0].Name)
	assert.True(t, cfg.Publish.VerificationGates[0].Required)
}

func TestCanPublishIssueImplementationFailure(t *testing.T) {
	t.Parallel()

	cfg := Config{Publish: PublishConfig{Enabled: true, DraftOnFailedValidation: true}}
	result := RunResult{WorkspacePath: t.TempDir()}

	assert.True(t, canPublishIssueImplementationFailure(cfg, result, assert.AnError))
	assert.False(t, canPublishIssueImplementationFailure(Config{}, result, assert.AnError))
	assert.False(t, canPublishIssueImplementationFailure(cfg, RunResult{}, assert.AnError))
	assert.False(t, canPublishIssueImplementationFailure(cfg, RunResult{WorkspacePath: result.WorkspacePath, Publish: &PublishResult{Published: true}}, assert.AnError))
	assert.False(t, canPublishIssueImplementationFailure(cfg, result, &VerificationGateError{}))
	assert.False(t, canPublishIssueImplementationFailure(cfg, result, context.Canceled))
	assert.False(t, canPublishIssueImplementationFailure(cfg, result, context.DeadlineExceeded))
}

func TestCanPublishIssueImplementationNoChangeDraft(t *testing.T) {
	t.Parallel()

	cfg := Config{Publish: PublishConfig{Enabled: true, DraftOnFailedValidation: true}}
	result := RunResult{
		WorkspacePath: t.TempDir(),
		Publish:       &PublishResult{SkippedReason: "workspace has no changes to publish"},
	}

	assert.True(t, canPublishIssueImplementationNoChangeDraft(cfg, result))
	assert.False(t, canPublishIssueImplementationNoChangeDraft(Config{}, result))
	assert.False(t, canPublishIssueImplementationNoChangeDraft(cfg, RunResult{Publish: result.Publish}))
	assert.False(t, canPublishIssueImplementationNoChangeDraft(cfg, RunResult{WorkspacePath: result.WorkspacePath}))
	assert.False(t, canPublishIssueImplementationNoChangeDraft(cfg, RunResult{
		WorkspacePath: result.WorkspacePath,
		Publish:       &PublishResult{Published: true, SkippedReason: "workspace has no changes to publish"},
	}))
}

func TestPublishNoChangeDraftIfNeededFailsWhenDraftFallbackDisabled(t *testing.T) {
	t.Parallel()

	result, err := publishNoChangeDraftIfNeeded(t.Context(), Config{
		Publish: PublishConfig{Enabled: true, DraftOnFailedValidation: false},
	}, Issue{Identifier: "GH-12"}, RunResult{
		Status:        AttemptSucceeded,
		WorkspacePath: t.TempDir(),
		Publish:       &PublishResult{SkippedReason: "workspace has no changes to publish"},
	}, loggerOrDefault(nil))

	require.Error(t, err)
	assert.Contains(t, err.Error(), "workspace has no changes to publish")
	assert.Equal(t, AttemptFailed, result.Status)
	assert.Equal(t, "symphony issue implement: workspace has no changes to publish", result.Error)
	require.NotNil(t, result.Publish)
	assert.Equal(t, "workspace has no changes to publish", result.Publish.SkippedReason)
}

func TestFailIssueImplementationValidationDraftIfNeededMarksRunFailed(t *testing.T) {
	t.Parallel()

	result, err := failIssueImplementationValidationDraftIfNeeded(RunResult{
		Status: AttemptSucceeded,
		Publish: &PublishResult{
			PullRequestURL:               "https://github.com/owner/repo/pull/7",
			DraftDueToFailedVerification: true,
			Verification: &VerificationReport{
				Configured:     true,
				Passed:         false,
				FailedRequired: []string{"unit"},
				Gates: []VerificationGateResult{{
					Name:     "unit",
					Status:   VerificationFailed,
					Required: true,
				}},
			},
		},
	})

	require.Error(t, err)
	assert.Equal(t, AttemptFailed, result.Status)
	assert.Contains(t, err.Error(), "draft pull request opened after failed verification")
	assert.Contains(t, err.Error(), "unit")
	assert.Equal(t, err.Error(), result.Error)
	require.NotNil(t, result.Publish)
	assert.Equal(t, "https://github.com/owner/repo/pull/7", result.Publish.PullRequestURL)
	assert.True(t, result.Publish.DraftDueToFailedVerification)
}

func TestIssueWithImplementationFlagsAddsWorkerInstructions(t *testing.T) {
	t.Parallel()

	description := "Existing issue body."
	issue := issueWithImplementationFlags(Issue{Description: &description}, IssueImplementOptions{
		UpdateDocs:      true,
		UpdateChangelog: true,
	})

	require.NotNil(t, issue.Description)
	assert.Contains(t, *issue.Description, "Existing issue body.")
	assert.Contains(t, *issue.Description, "Update documentation when relevant.")
	assert.Contains(t, *issue.Description, "Update the changelog when relevant.")
}
