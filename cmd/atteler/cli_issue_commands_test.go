package main

import (
	"context"
	"flag"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/tommoulard/atteler/pkg/permission"
	attshell "github.com/tommoulard/atteler/pkg/shell"
	"github.com/tommoulard/atteler/pkg/symphony"
	"github.com/tommoulard/atteler/pkg/worktree"
)

const (
	testIssueWatchIssuesPath           = "/repos/owner/repo/issues"
	testIssueWatchIssue232Path         = "/repos/owner/repo/issues/232"
	testIssueWatchIssue232CommentsPath = testIssueWatchIssue232Path + "/comments"
)

func TestIssueImplementLegacyFlagTracksExplicitEmptyValue(t *testing.T) {
	t.Parallel()

	var opts cliOptions

	fs := flag.NewFlagSet("test", flag.ContinueOnError)
	registerCLIFlagsWithFlagSet(fs, &opts)

	require.NoError(t, fs.Parse([]string{"--issue-implement", ""}))

	assert.True(t, opts.issueImplementRequested)
	assert.Empty(t, opts.issueImplementRef)
	require.NotEmpty(t, providerlessIssueCommands())
	assert.True(t, providerlessIssueCommands()[0].match(opts))
}

func TestIssueImplementRejectsOpenPRInsideIssueWatchLocalCommand(t *testing.T) {
	t.Setenv("ATTELER_ISSUE_WATCH", "1")

	err := runIssueImplementCommand(t.Context(), cliOptions{
		issueImplementRef: "GH-232",
		issueOpenPR:       true,
	}, nil)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "--open-pr is disabled inside atteler issue watch local commands")
}

func TestRunOnceBashCommandBlocksShellNetworkInsideIssueWatchLocalCommand(t *testing.T) {
	t.Setenv("ATTELER_ISSUE_WATCH", "1")

	permissionPolicy := permission.DefaultPolicy()
	root := t.TempDir()

	result, err := runOnceBashCommand(
		t.Context(),
		root,
		"curl https://example.com/publish",
		nil,
		attshell.AuditContext{Caller: "issue-watch-test"},
		&permissionPolicy,
		"tool-call",
	)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "network.deny")
	assert.Empty(t, result.Stdout)

	result, err = runOnceBashCommand(
		t.Context(),
		root,
		"printf local-only",
		nil,
		attshell.AuditContext{Caller: "issue-watch-test"},
		&permissionPolicy,
		"tool-call-local",
	)
	require.NoError(t, err)
	assert.Equal(t, "local-only", result.Stdout)
}

func TestIssueImplementValidationSummary(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name   string
		report *symphony.VerificationReport
		want   []string
	}{
		{
			name:   "nil report",
			report: nil,
			want:   nil,
		},
		{
			name: "not configured",
			report: &symphony.VerificationReport{
				Configured: false,
			},
			want: []string{"validation: no local gates configured"},
		},
		{
			name: "passed",
			report: &symphony.VerificationReport{
				Configured: true,
				Passed:     true,
				Gates:      []symphony.VerificationGateResult{{Name: "unit"}},
			},
			want: []string{"validation: passed (1 gate(s))"},
		},
		{
			name: "optional failed",
			report: &symphony.VerificationReport{
				Configured: true,
				Passed:     true,
				Gates: []symphony.VerificationGateResult{
					{Name: "unit", Status: symphony.VerificationPassed, Required: true},
					{Name: "api_key=optional-secret", Status: symphony.VerificationFailed},
				},
			},
			want: []string{
				"validation: passed (2 gate(s))",
				"failed_optional_gates: api_key=[REDACTED]",
			},
		},
		{
			name: "failed required",
			report: &symphony.VerificationReport{
				Configured:     true,
				Passed:         false,
				FailedRequired: []string{"unit", "api_key=gate-secret", " "},
				Gates:          []symphony.VerificationGateResult{{Name: "unit"}},
			},
			want: []string{
				"validation: failed (1 gate(s))",
				"failed_required_gates: unit, api_key=[REDACTED]",
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			assert.Equal(t, tt.want, issueImplementValidationSummary(tt.report))
		})
	}
}

func TestIssueWatchGitHubTrackerConfigRequiresRepositoryAndLabel(t *testing.T) {
	t.Parallel()

	_, err := issueWatchGitHubTrackerConfig(t.Context(), cliOptions{
		issueWatchGitHub: "owner/repo",
	}, true)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "at least one --label")

	_, err = issueWatchGitHubTrackerConfig(t.Context(), cliOptions{
		issueWatchGitHub: "owner",
		issueWatchLabels: stringListFlag{"atteler-agent"},
	}, true)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "--github owner/repo")
}

func TestIssueWatchIntervalFlagParsesWithNamedValidationError(t *testing.T) {
	t.Parallel()

	opts, fs := newCLIOptionsAndFlagSetForTest(t)
	require.NoError(t, fs.Parse([]string{"--issue-watch-interval-seconds", "5"}))
	assert.True(t, opts.issueWatchIntervalSeconds.set)
	assert.Equal(t, 5, opts.issueWatchIntervalSeconds.value)

	_, fs = newCLIOptionsAndFlagSetForTest(t)
	err := fs.Parse([]string{"--issue-watch-interval-seconds", "0"})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "issue-watch-interval-seconds must be > 0")
}

func TestIssueWatchGitHubTrackerConfigUsesOpenIssuesAndLabels(t *testing.T) {
	t.Parallel()

	cfg, err := issueWatchGitHubTrackerConfig(t.Context(), cliOptions{
		issueWatchGitHub:         "owner/repo",
		issueWatchGitHubEndpoint: "https://github.example/api/v3",
		issueWatchGitHubToken:    "token",
		issueWatchLabels:         stringListFlag{"atteler-agent", "ready-for-ai"},
	}, true)
	require.NoError(t, err)

	assert.Equal(t, "github", cfg.Kind)
	assert.Equal(t, "https://github.example/api/v3", cfg.Endpoint)
	assert.Equal(t, "token", cfg.APIKey)
	assert.Equal(t, "owner", cfg.Owner)
	assert.Equal(t, "repo", cfg.Repo)
	assert.Equal(t, []string{"OPEN"}, cfg.ActiveStates)
	assert.Equal(t, []string{"atteler-agent", "ready-for-ai"}, cfg.Labels)
}

func TestIssueWatchGitHubTrackerConfigRespectsCredentialPolicy(t *testing.T) {
	t.Parallel()

	policy := permission.DefaultPolicy()
	policy.SetMode(permission.OperationCredentialAccess, permission.ModeDeny)
	ctx := permission.ContextWithPolicy(t.Context(), &policy)

	_, err := issueWatchGitHubTrackerConfig(ctx, cliOptions{
		issueWatchGitHub:      "owner/repo",
		issueWatchGitHubToken: "token",
		issueWatchLabels:      stringListFlag{"atteler-agent"},
	}, true)
	require.Error(t, err)
	assert.True(t, permission.ErrDenied(err))
}

func TestIssueWatchGitHubTrackerConfigSkipsEnvTokenWhenCredentialsDenied(t *testing.T) {
	t.Setenv("GITHUB_TOKEN", "env-token")

	policy := permission.DefaultPolicy()
	policy.SetMode(permission.OperationCredentialAccess, permission.ModeDeny)
	ctx := permission.ContextWithPolicy(t.Context(), &policy)

	cfg, err := issueWatchGitHubTrackerConfig(ctx, cliOptions{
		issueWatchGitHub: "owner/repo",
		issueWatchLabels: stringListFlag{"atteler-agent"},
	}, true)
	require.NoError(t, err)
	assert.Empty(t, cfg.APIKey)
}

func writeIssueWatchTestJSON(t *testing.T, w http.ResponseWriter, payload string) {
	t.Helper()

	_, err := w.Write([]byte(payload))
	require.NoError(t, err)
}

func TestIssueWatchGitHubTrackerConfigAllowsRunRefWithoutLabel(t *testing.T) {
	t.Parallel()

	cfg, err := issueWatchGitHubTrackerConfig(t.Context(), cliOptions{
		issueWatchGitHub: "owner/repo",
	}, false)
	require.NoError(t, err)
	assert.Equal(t, "owner", cfg.Owner)
	assert.Equal(t, "repo", cfg.Repo)
	assert.Empty(t, cfg.Labels)
}

func TestRunIssueWatchIterationDryRunFetchesGitHubCandidatesByLabel(t *testing.T) {
	t.Parallel()

	var issueListSeen atomic.Bool

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")

		switch r.URL.Path {
		case testIssueWatchIssuesPath:
			issueListSeen.Store(true)
			assert.Equal(t, http.MethodGet, r.Method)
			assert.Equal(t, "open", r.URL.Query().Get("state"))
			assert.Equal(t, "atteler-agent", r.URL.Query().Get("labels"))
			assert.Empty(t, r.Header.Get("Authorization"))
			writeIssueWatchTestJSON(t, w, `[{"node_id":"node-232","number":232,"title":"Ready for agent","state":"open","body":"Implement the requested change.","html_url":"https://github.com/owner/repo/issues/232","created_at":"2026-06-20T10:00:00Z","updated_at":"2026-06-20T10:05:00Z","labels":[{"name":"atteler-agent"}]}]`)
		case testIssueWatchIssue232CommentsPath:
			assert.Equal(t, http.MethodGet, r.Method)
			writeIssueWatchTestJSON(t, w, `[]`)
		default:
			t.Errorf("unexpected GitHub test request %s", r.URL.String())
			w.WriteHeader(http.StatusNotFound)
			writeIssueWatchTestJSON(t, w, `{"message":"not found"}`)
		}
	}))
	t.Cleanup(server.Close)

	root := t.TempDir()
	result, err := runIssueWatchIteration(t.Context(), root, cliOptions{
		issueWatchGitHub:         "owner/repo",
		issueWatchGitHubEndpoint: server.URL,
		issueWatchLabels:         stringListFlag{"atteler-agent"},
		issueWatchDryRun:         true,
	})
	require.NoError(t, err)

	assert.True(t, issueListSeen.Load())
	require.Len(t, result.Candidates, 1)
	assert.Equal(t, "GH-232", result.Candidates[0].Identifier)
	assert.Equal(t, "Ready for agent", result.Candidates[0].Title)
	assert.Empty(t, result.Runs)
	assert.NoFileExists(t, filepath.Join(root, ".atteler", "issue-watch", "state.json"))
}

func TestRunIssueWatchIterationDryRunFetchesSingleGitHubIssueWithoutLabel(t *testing.T) {
	t.Parallel()

	var issueSeen atomic.Bool

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")

		switch r.URL.Path {
		case testIssueWatchIssue232Path:
			issueSeen.Store(true)
			assert.Equal(t, http.MethodGet, r.Method)
			assert.Empty(t, r.Header.Get("Authorization"))
			writeIssueWatchTestJSON(t, w, `{"node_id":"node-232","number":232,"title":"Run directly","state":"open","body":"Direct issue run.","html_url":"https://github.com/owner/repo/issues/232","created_at":"2026-06-20T10:00:00Z","updated_at":"2026-06-20T10:05:00Z","labels":[]}`)
		case testIssueWatchIssue232CommentsPath:
			assert.Equal(t, http.MethodGet, r.Method)
			writeIssueWatchTestJSON(t, w, `[]`)
		default:
			t.Errorf("unexpected GitHub test request %s", r.URL.String())
			w.WriteHeader(http.StatusNotFound)
			writeIssueWatchTestJSON(t, w, `{"message":"not found"}`)
		}
	}))
	t.Cleanup(server.Close)

	result, err := runIssueWatchIteration(t.Context(), t.TempDir(), cliOptions{
		issueWatchGitHub:         "owner/repo",
		issueWatchGitHubEndpoint: server.URL,
		issueWatchRunRef:         "232",
		issueWatchDryRun:         true,
	})
	require.NoError(t, err)

	assert.True(t, issueSeen.Load())
	require.Len(t, result.Candidates, 1)
	assert.Equal(t, "GH-232", result.Candidates[0].Identifier)
	assert.Equal(t, "Run directly", result.Candidates[0].Title)
	assert.Empty(t, result.Runs)
}

func TestRunIssueWatchIterationDirectRunCreatesLocalArtifactsWithoutLabel(t *testing.T) {
	var (
		issueSeen atomic.Bool
		listSeen  atomic.Bool
	)

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")

		switch r.URL.Path {
		case testIssueWatchIssue232Path:
			issueSeen.Store(true)
			assert.Equal(t, http.MethodGet, r.Method)
			assert.Empty(t, r.URL.Query().Get("labels"))
			writeIssueWatchTestJSON(t, w, `{"node_id":"node-232","number":232,"title":"Run directly","state":"open","body":"Direct issue run.","html_url":"https://github.com/owner/repo/issues/232","created_at":"2026-06-20T10:00:00Z","updated_at":"2026-06-20T10:05:00Z","labels":[]}`)
		case testIssueWatchIssue232CommentsPath:
			assert.Equal(t, http.MethodGet, r.Method)
			writeIssueWatchTestJSON(t, w, `[]`)
		case testIssueWatchIssuesPath:
			listSeen.Store(true)
			w.WriteHeader(http.StatusInternalServerError)
			writeIssueWatchTestJSON(t, w, `{"message":"direct run should not list candidates"}`)
		default:
			t.Errorf("unexpected GitHub test request %s", r.URL.String())
			w.WriteHeader(http.StatusNotFound)
			writeIssueWatchTestJSON(t, w, `{"message":"not found"}`)
		}
	}))
	t.Cleanup(server.Close)

	root := initIssueWatchCLIRepo(t)
	t.Setenv(worktree.EnvDir, filepath.Join(t.TempDir(), "worktrees"))

	result, err := runIssueWatchIteration(t.Context(), root, cliOptions{
		issueWatchGitHub:         "owner/repo",
		issueWatchGitHubEndpoint: server.URL,
		issueWatchRunRef:         "232",
	})
	require.NoError(t, err)

	assert.True(t, issueSeen.Load())
	assert.False(t, listSeen.Load())
	require.Len(t, result.Candidates, 1)
	require.Len(t, result.Runs, 1)
	run := result.Runs[0]
	assert.Equal(t, "GH-232", run.IssueIdentifier)
	assert.FileExists(t, run.Artifacts.IssueJSON)
	assert.FileExists(t, run.Artifacts.Plan)
	assert.FileExists(t, run.Artifacts.RunJSON)
	assert.DirExists(t, run.WorktreePath)
}

func TestRunIssueWatchCommandOnceCreatesLocalRunAndExits(t *testing.T) {
	var issueListRequests atomic.Int32

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")

		switch r.URL.Path {
		case testIssueWatchIssuesPath:
			issueListRequests.Add(1)
			assert.Equal(t, http.MethodGet, r.Method)
			assert.Equal(t, "open", r.URL.Query().Get("state"))
			assert.Equal(t, "atteler-agent", r.URL.Query().Get("labels"))
			writeIssueWatchTestJSON(t, w, `[{"node_id":"node-232","number":232,"title":"Once run","state":"open","body":"Run one watch iteration.","html_url":"https://github.com/owner/repo/issues/232","created_at":"2026-06-20T10:00:00Z","updated_at":"2026-06-20T10:05:00Z","labels":[{"name":"atteler-agent"}]}]`)
		case testIssueWatchIssue232CommentsPath:
			assert.Equal(t, http.MethodGet, r.Method)
			writeIssueWatchTestJSON(t, w, `[]`)
		default:
			t.Errorf("unexpected GitHub test request %s", r.URL.String())
			w.WriteHeader(http.StatusNotFound)
			writeIssueWatchTestJSON(t, w, `{"message":"not found"}`)
		}
	}))
	t.Cleanup(server.Close)

	root := initIssueWatchCLIRepo(t)
	t.Chdir(root)
	t.Setenv(worktree.EnvDir, filepath.Join(t.TempDir(), "worktrees"))

	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Second)
	defer cancel()

	err := runIssueWatchCommand(ctx, cliOptions{
		issueWatchGitHub:         "owner/repo",
		issueWatchGitHubEndpoint: server.URL,
		issueWatchLabels:         stringListFlag{"atteler-agent"},
		issueWatchOnce:           true,
		issueWatchIntervalSeconds: positiveIntFlag{
			name:  "issue-watch-interval-seconds",
			value: 1,
			set:   true,
		},
	}, nil)
	require.NoError(t, err)

	assert.Equal(t, int32(1), issueListRequests.Load())
	assert.FileExists(t, filepath.Join(root, ".atteler", "issue-watch", "state.json"))

	matches, err := filepath.Glob(filepath.Join(root, ".atteler", "runs", "issues", "GH-232", "*", "run.json"))
	require.NoError(t, err)
	require.Len(t, matches, 1)
}

func TestRunIssueWatchIterationLocalRunUsesReadOnlyGitHubRequests(t *testing.T) {
	var (
		mutationRequests atomic.Int32
		issueListSeen    atomic.Bool
	)

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")

		if r.Method != http.MethodGet {
			mutationRequests.Add(1)
			w.WriteHeader(http.StatusMethodNotAllowed)
			writeIssueWatchTestJSON(t, w, `{"message":"mutations are not allowed"}`)

			return
		}

		switch r.URL.Path {
		case testIssueWatchIssuesPath:
			issueListSeen.Store(true)
			assert.Equal(t, "open", r.URL.Query().Get("state"))
			assert.Equal(t, "atteler-agent", r.URL.Query().Get("labels"))
			writeIssueWatchTestJSON(t, w, `[{"node_id":"node-232","number":232,"title":"Prepare local run","state":"open","body":"No publishing by default.","html_url":"https://github.com/owner/repo/issues/232","created_at":"2026-06-20T10:00:00Z","updated_at":"2026-06-20T10:05:00Z","labels":[{"name":"atteler-agent"}]}]`)
		case testIssueWatchIssue232CommentsPath:
			writeIssueWatchTestJSON(t, w, `[]`)
		default:
			t.Errorf("unexpected GitHub test request %s", r.URL.String())
			w.WriteHeader(http.StatusNotFound)
			writeIssueWatchTestJSON(t, w, `{"message":"not found"}`)
		}
	}))
	t.Cleanup(server.Close)

	root := initIssueWatchCLIRepo(t)
	t.Setenv(worktree.EnvDir, filepath.Join(t.TempDir(), "worktrees"))

	result, err := runIssueWatchIteration(t.Context(), root, cliOptions{
		issueWatchGitHub:         "owner/repo",
		issueWatchGitHubEndpoint: server.URL,
		issueWatchLabels:         stringListFlag{"atteler-agent"},
	})
	require.NoError(t, err)

	assert.True(t, issueListSeen.Load())
	assert.Zero(t, mutationRequests.Load())
	require.Len(t, result.Runs, 1)
	run := result.Runs[0]
	assert.Contains(t, run.Safety, "no push")
	assert.Contains(t, run.Safety, "pull request")
	assert.Contains(t, run.Safety, "tracker comment")
	assert.FileExists(t, filepath.Join(root, ".atteler", "issue-watch", "state.json"))
	assert.FileExists(t, run.Artifacts.RunJSON)
	assert.DirExists(t, run.WorktreePath)
}

func TestRunIssueWatchIterationErrorsWhenDirectIssueIsNotEligible(t *testing.T) {
	t.Parallel()

	var issueSeen atomic.Bool

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")

		switch r.URL.Path {
		case testIssueWatchIssue232Path:
			issueSeen.Store(true)
			assert.Equal(t, http.MethodGet, r.Method)
			writeIssueWatchTestJSON(t, w, `{"node_id":"node-232","number":232,"title":"Already closed","state":"closed","body":"No longer eligible.","html_url":"https://github.com/owner/repo/issues/232","labels":[]}`)
		case testIssueWatchIssue232CommentsPath:
			assert.Equal(t, http.MethodGet, r.Method)
			writeIssueWatchTestJSON(t, w, `[]`)
		default:
			t.Errorf("unexpected GitHub test request %s", r.URL.String())
			w.WriteHeader(http.StatusNotFound)
			writeIssueWatchTestJSON(t, w, `{"message":"not found"}`)
		}
	}))
	t.Cleanup(server.Close)

	result, err := runIssueWatchIteration(t.Context(), t.TempDir(), cliOptions{
		issueWatchGitHub:         "owner/repo",
		issueWatchGitHubEndpoint: server.URL,
		issueWatchRunRef:         "232",
		issueWatchDryRun:         true,
	})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "no eligible issue found for 232")
	assert.True(t, issueSeen.Load())
	assert.Empty(t, result.Candidates)
	assert.Empty(t, result.Runs)
}

func TestIssueWatchRunTrackerErrorsWhenIssueReferenceIsMissing(t *testing.T) {
	t.Parallel()

	tracker := issueWatchRunTracker{
		tracker: emptyIssueWatchIssueFetcher{},
		ref:     "node-missing",
	}

	_, err := tracker.FetchCandidateIssues(t.Context())
	require.Error(t, err)
	assert.Contains(t, err.Error(), "no issue found for node-missing")
}

//nolint:paralleltest // Temporarily redirects process stdout.
func TestPrintIssueImplementResultReportsDraftValidationFailure(t *testing.T) {
	output := captureIssueImplementStdout(t, func() {
		printIssueImplementResult(symphony.RunResult{
			Status:        symphony.AttemptFailed,
			WorkspacePath: "/tmp/symphony/GH-12",
			Publish: &symphony.PublishResult{
				Branch:                       "symphony/GH-12",
				PullRequestURL:               "https://github.com/owner/repo/pull/7",
				DraftDueToFailedVerification: true,
				Verification: &symphony.VerificationReport{
					Configured:     true,
					Passed:         false,
					FailedRequired: []string{"unit"},
					Gates: []symphony.VerificationGateResult{{
						Name:     "unit",
						Status:   symphony.VerificationFailed,
						Required: true,
					}},
				},
			},
		})
	})

	assert.Contains(t, output, "issue implementation: failed")
	assert.Contains(t, output, "workspace: /tmp/symphony/GH-12")
	assert.Contains(t, output, "branch: symphony/GH-12")
	assert.Contains(t, output, "pull_request: https://github.com/owner/repo/pull/7")
	assert.Contains(t, output, "validation: failed (1 gate(s))")
	assert.Contains(t, output, "failed_required_gates: unit")
	assert.Contains(t, output, "draft_reason: required verification gate failed")
}

//nolint:paralleltest // Temporarily redirects process stdout.
func TestPrintIssueImplementResultReportsIncompleteDraft(t *testing.T) {
	output := captureIssueImplementStdout(t, func() {
		printIssueImplementResult(symphony.RunResult{
			Status:        symphony.AttemptFailed,
			WorkspacePath: "/tmp/symphony/GH-12",
			Publish: &symphony.PublishResult{
				Branch:               "symphony/GH-12",
				PullRequestURL:       "https://github.com/owner/repo/pull/7",
				DraftDueToRunFailure: true,
				Verification: &symphony.VerificationReport{
					Configured:     true,
					Passed:         false,
					FailedRequired: []string{"worker_run"},
					Gates: []symphony.VerificationGateResult{{
						Name:     "worker_run",
						Status:   symphony.VerificationFailed,
						Required: true,
					}},
				},
			},
		})
	})

	assert.Contains(t, output, "issue implementation: failed")
	assert.Contains(t, output, "pull_request: https://github.com/owner/repo/pull/7")
	assert.Contains(t, output, "validation: failed (1 gate(s))")
	assert.Contains(t, output, "failed_required_gates: worker_run")
	assert.Contains(t, output, "draft_reason: implementation incomplete")
}

func captureIssueImplementStdout(t *testing.T, fn func()) string {
	t.Helper()

	original := os.Stdout
	reader, writer, err := os.Pipe()
	require.NoError(t, err)

	os.Stdout = writer

	defer func() {
		os.Stdout = original
		_ = reader.Close()
		_ = writer.Close()
	}()

	fn()

	require.NoError(t, writer.Close())

	data, err := io.ReadAll(reader)
	require.NoError(t, err)

	return string(data)
}

type emptyIssueWatchIssueFetcher struct{}

func (emptyIssueWatchIssueFetcher) FetchIssueStatesByIDs(_ context.Context, _ []string) ([]symphony.Issue, error) {
	return nil, nil
}

func initIssueWatchCLIRepo(t *testing.T) string {
	t.Helper()

	root := t.TempDir()
	runGitForIssueWatchCLITest(t, root, "init")
	runGitForIssueWatchCLITest(t, root, "config", "user.email", "issuewatch-cli@example.test")
	runGitForIssueWatchCLITest(t, root, "config", "user.name", "Issue Watch CLI Test")
	runGitForIssueWatchCLITest(t, root, "config", "commit.gpgsign", "false")
	runGitForIssueWatchCLITest(t, root, "config", "core.excludesFile", os.DevNull)
	require.NoError(t, os.WriteFile(filepath.Join(root, "AGENTS.md"), []byte("# Rules\nKeep issue-watch local-only.\n"), 0o600))
	require.NoError(t, os.WriteFile(filepath.Join(root, "README.md"), []byte("# test repo\n"), 0o600))
	runGitForIssueWatchCLITest(t, root, "add", ".")
	runGitForIssueWatchCLITest(t, root, "commit", "-m", "init")

	return root
}

func runGitForIssueWatchCLITest(t *testing.T, dir string, args ...string) {
	t.Helper()

	cmd := exec.CommandContext(t.Context(), "git", args...)
	cmd.Dir = dir

	if out, err := cmd.CombinedOutput(); err != nil {
		require.Failf(t, "git command failed", "git %v: %s: %v", args, out, err)
	}
}
