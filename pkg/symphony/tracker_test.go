package symphony

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/tommoulard/atteler/pkg/permission"
	"github.com/tommoulard/atteler/pkg/watch"
)

const (
	testGitHubIssuesPath       = "/repos/owner/repo/issues"
	testGitHubPullRequestPath  = "/repos/owner/repo/pulls/31"
	testGitHubCheckRunsPath    = "/repos/owner/repo/commits/abc123/check-runs"
	testGitHubCommitStatusPath = "/repos/owner/repo/commits/abc123/status"
	testGitHubProtectionPath   = "/repos/owner/repo/branches/main/protection/required_status_checks"
	testGitHubRulesPath        = "/repos/owner/repo/rules/branches/main"
)

func TestTrackerClients_RequireActiveContext(t *testing.T) {
	t.Parallel()

	linear := NewLinearClient(TrackerConfig{
		Endpoint:     "http://127.0.0.1:1/graphql",
		APIKey:       "token",
		ProjectSlug:  "project",
		ActiveStates: []string{"Todo"},
	})

	_, err := linear.FetchCandidateIssues(nil) //nolint:staticcheck // Verify nil contexts are rejected instead of panicking.
	require.Error(t, err)
	require.Contains(t, err.Error(), "context is required")

	github := NewGitHubClient(TrackerConfig{
		Endpoint:     "http://127.0.0.1:1",
		APIKey:       "token",
		Owner:        "owner",
		Repo:         "repo",
		ActiveStates: []string{"OPEN"},
	})

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	_, err = github.FetchCandidateIssues(ctx)
	require.Error(t, err)
	require.ErrorIs(t, err, context.Canceled)
}

func TestLinearClient_PermissionPolicyDeniesCredentialAccessBeforeRequest(t *testing.T) {
	t.Parallel()

	policy := permission.DefaultPolicy()
	policy.SetMode(permission.OperationCredentialAccess, permission.ModeDeny)
	ctx := permission.ContextWithPolicy(t.Context(), &policy)
	ctx = permission.ContextWithAuditDir(ctx, t.TempDir())

	client := NewLinearClient(TrackerConfig{
		Endpoint:     "http://127.0.0.1:1/graphql",
		APIKey:       "token",
		ProjectSlug:  "project",
		ActiveStates: []string{"Todo"},
	})

	_, err := client.FetchCandidateIssues(ctx)
	require.Error(t, err)
	require.True(t, permission.ErrDenied(err))
	assert.Contains(t, err.Error(), "permission.credential_access.deny")
}

func TestGitHubClient_PermissionPolicyDeniesNetworkBeforeRequest(t *testing.T) {
	t.Parallel()

	policy := permission.DefaultPolicy()
	policy.SetMode(permission.OperationNetwork, permission.ModeDeny)
	ctx := permission.ContextWithPolicy(t.Context(), &policy)
	ctx = permission.ContextWithAuditDir(ctx, t.TempDir())

	client := NewGitHubClient(TrackerConfig{
		Endpoint:     "http://127.0.0.1:1",
		APIKey:       "token",
		Owner:        "owner",
		Repo:         "repo",
		ActiveStates: []string{"open"},
	})

	_, err := client.FetchCandidateIssues(ctx)
	require.Error(t, err)
	require.True(t, permission.ErrDenied(err))
	assert.Contains(t, err.Error(), "permission.network.deny")
}

func TestGitHubClient_PermissionPolicyDeniesWriteBeforeCreateIssue(t *testing.T) {
	t.Parallel()

	policy := permission.DefaultPolicy()
	policy.SetMode(permission.OperationWrite, permission.ModeDeny)
	ctx := permission.ContextWithPolicy(t.Context(), &policy)
	ctx = permission.ContextWithAuditDir(ctx, t.TempDir())

	client := NewGitHubClient(TrackerConfig{
		Endpoint: "http://127.0.0.1:1",
		APIKey:   "token",
		Owner:    "owner",
		Repo:     "repo",
	})

	_, err := client.CreateIssue(ctx, watch.IssueDraft{
		Title:       "denied",
		Body:        "should not make a network request",
		Fingerprint: "permission-denied",
	})
	require.Error(t, err)
	require.True(t, permission.ErrDenied(err))
	assert.Contains(t, err.Error(), "permission.write.deny")
}

func TestGitHubClient_FetchPullRequestChecksClassifiesFailure(t *testing.T) {
	t.Parallel()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, "Bearer token", r.Header.Get("Authorization"))
		assert.Equal(t, githubAPIVersion, r.Header.Get("X-GitHub-Api-Version"))

		switch {
		case r.Method == http.MethodGet && r.URL.Path == testGitHubPullRequestPath:
			writeTestResponse(t, w, `{"number":31,"html_url":"https://github.com/owner/repo/pull/31","state":"open","head":{"ref":"symphony/GH-2","sha":"abc123"}}`)
		case r.Method == http.MethodGet && r.URL.Path == testGitHubCheckRunsPath:
			writeTestResponse(t, w, `{"check_runs":[{"name":"test","status":"completed","conclusion":"failure","details_url":"https://ci.example/test"},{"name":"lint","status":"completed","conclusion":"success"}]}`)
		case r.Method == http.MethodGet && r.URL.Path == testGitHubCommitStatusPath:
			writeTestResponse(t, w, `{"state":"success","statuses":[]}`)
		default:
			t.Errorf("unexpected GitHub request: %s %s", r.Method, r.URL.String())
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	client := NewGitHubClient(TrackerConfig{
		Endpoint: server.URL,
		APIKey:   "token",
		Owner:    "owner",
		Repo:     "repo",
	})

	checks, err := client.FetchPullRequestChecks(t.Context(), 31)
	require.NoError(t, err)

	assert.Equal(t, PullRequestChecksFailed, checks.State)
	assert.Equal(t, "abc123", checks.HeadSHA)
	assert.Equal(t, []string{"test"}, checks.FailedCheckNames)
	assert.Contains(t, checks.Summary, "test")
}

func TestGitHubClient_FetchPullRequestChecksPassesNoCheckRepoByPolicy(t *testing.T) {
	t.Parallel()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, "Bearer token", r.Header.Get("Authorization"))

		switch {
		case r.Method == http.MethodGet && r.URL.Path == testGitHubPullRequestPath:
			writeTestResponse(t, w, `{"number":31,"html_url":"https://github.com/owner/repo/pull/31","state":"open","head":{"ref":"symphony/GH-2","sha":"abc123"},"base":{"ref":"main","sha":"base123"}}`)
		case r.Method == http.MethodGet && r.URL.Path == testGitHubCheckRunsPath:
			writeTestResponse(t, w, `{"check_runs":[]}`)
		case r.Method == http.MethodGet && r.URL.Path == testGitHubCommitStatusPath:
			writeTestResponse(t, w, `{"state":"success","statuses":[]}`)
		default:
			t.Errorf("unexpected GitHub request: %s %s", r.Method, r.URL.String())
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	client := NewGitHubClient(TrackerConfig{
		Endpoint: server.URL,
		APIKey:   "token",
		Owner:    "owner",
		Repo:     "repo",
	})

	checks, err := client.FetchPullRequestChecksWithPolicy(t.Context(), 31, PullRequestCheckPolicy{
		NoChecksPolicy: PullRequestNoChecksPass,
	})
	require.NoError(t, err)

	assert.Equal(t, PullRequestChecksPassed, checks.State)
	assert.Equal(t, "none", checks.RequirementSource)
	assert.Equal(t, string(PullRequestNoChecksPass), checks.NoChecksPolicy)
	assert.Empty(t, checks.FailedCheckNames)
	assert.Contains(t, checks.Summary, "no required checks")
}

func TestClassifyPullRequestChecksHonorsNoChecksPolicies(t *testing.T) {
	t.Parallel()

	tests := []struct {
		policy       PullRequestNoChecksPolicy
		expected     PullRequestCheckState
		failedChecks []string
	}{
		{
			policy:   PullRequestNoChecksPass,
			expected: PullRequestChecksPassed,
		},
		{
			policy:   PullRequestNoChecksPending,
			expected: PullRequestChecksPending,
		},
		{
			policy:       PullRequestNoChecksFail,
			expected:     PullRequestChecksFailed,
			failedChecks: []string{"no reported checks"},
		},
	}

	for _, test := range tests {
		t.Run(string(test.policy), func(t *testing.T) {
			t.Parallel()

			checks := PullRequestCheckSnapshot{}
			classifyPullRequestChecks(&checks, PullRequestCheckPolicy{
				NoChecksPolicy: test.policy,
			})

			assert.Equal(t, test.expected, checks.State)
			assert.Equal(t, string(test.policy), checks.NoChecksPolicy)
			assert.Equal(t, test.failedChecks, checks.FailedCheckNames)
		})
	}
}

func TestClassifyPullRequestChecksHonorsNoChecksPoliciesWhenOnlyOptionalChecksReported(t *testing.T) {
	t.Parallel()

	tests := []struct {
		policy       PullRequestNoChecksPolicy
		expected     PullRequestCheckState
		failedChecks []string
	}{
		{
			policy:   PullRequestNoChecksPass,
			expected: PullRequestChecksPassed,
		},
		{
			policy:   PullRequestNoChecksPending,
			expected: PullRequestChecksPending,
		},
		{
			policy:       PullRequestNoChecksFail,
			expected:     PullRequestChecksFailed,
			failedChecks: []string{"no required checks"},
		},
	}

	for _, test := range tests {
		t.Run(string(test.policy), func(t *testing.T) {
			t.Parallel()

			checks := PullRequestCheckSnapshot{
				CheckRuns: []PullRequestCheckRun{{
					Name:       "optional-lint",
					Status:     "completed",
					Conclusion: "failure",
				}},
			}
			classifyPullRequestChecks(&checks, PullRequestCheckPolicy{
				NoChecksPolicy: test.policy,
			})

			assert.Equal(t, test.expected, checks.State)
			assert.Equal(t, string(test.policy), checks.NoChecksPolicy)
			assert.Equal(t, test.failedChecks, checks.FailedCheckNames)
			assert.Equal(t, []string{"optional-lint"}, checks.OptionalFailedCheckNames)
			assert.Equal(t, "none", checks.RequirementSource)
		})
	}
}

func TestClassifyPullRequestChecksTreatAllReportedUsesNoChecksPolicyWhenNoneReported(t *testing.T) {
	t.Parallel()

	checks := PullRequestCheckSnapshot{}
	classifyPullRequestChecks(&checks, PullRequestCheckPolicy{
		NoChecksPolicy:             PullRequestNoChecksPending,
		TreatAllReportedAsRequired: true,
	})

	assert.Equal(t, PullRequestChecksPending, checks.State)
	assert.Empty(t, checks.MissingRequiredCheckNames)
	assert.Equal(t, "no check runs or status contexts have been reported yet", checks.Summary)
}

func TestClassifyPullRequestChecksMarksConfiguredRequiredChecksMissingWhenNoneReported(t *testing.T) {
	t.Parallel()

	checks := PullRequestCheckSnapshot{}
	classifyPullRequestChecks(&checks, PullRequestCheckPolicy{
		NoChecksPolicy:     PullRequestNoChecksPass,
		RequiredCheckNames: []string{"test"},
	})

	assert.Equal(t, PullRequestChecksPending, checks.State)
	assert.Equal(t, []string{"test"}, checks.MissingRequiredCheckNames)
	assert.Contains(t, checks.Summary, "pending required checks")
}

func TestClassifyPullRequestChecksMarksConfiguredRequiredPatternsMissingWhenNoneReported(t *testing.T) {
	t.Parallel()

	checks := PullRequestCheckSnapshot{}
	classifyPullRequestChecks(&checks, PullRequestCheckPolicy{
		NoChecksPolicy:        PullRequestNoChecksPass,
		RequiredCheckPatterns: []string{"ci/*"},
	})

	assert.Equal(t, PullRequestChecksPending, checks.State)
	assert.Equal(t, []string{"pattern:ci/*"}, checks.MissingRequiredCheckNames)
	assert.Contains(t, checks.Summary, "pending required checks")
}

func TestGitHubClient_FetchPullRequestChecksReportsOptionalFailuresWithoutFailing(t *testing.T) {
	t.Parallel()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, "Bearer token", r.Header.Get("Authorization"))

		switch {
		case r.Method == http.MethodGet && r.URL.Path == testGitHubPullRequestPath:
			writeTestResponse(t, w, `{"number":31,"html_url":"https://github.com/owner/repo/pull/31","state":"open","head":{"ref":"symphony/GH-2","sha":"abc123"},"base":{"ref":"main","sha":"base123"}}`)
		case r.Method == http.MethodGet && r.URL.Path == testGitHubCheckRunsPath:
			writeTestResponse(t, w, `{"check_runs":[{"name":"optional-lint","status":"completed","conclusion":"failure"}]}`)
		case r.Method == http.MethodGet && r.URL.Path == testGitHubCommitStatusPath:
			writeTestResponse(t, w, `{"state":"success","statuses":[]}`)
		default:
			t.Errorf("unexpected GitHub request: %s %s", r.Method, r.URL.String())
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	client := NewGitHubClient(TrackerConfig{
		Endpoint: server.URL,
		APIKey:   "token",
		Owner:    "owner",
		Repo:     "repo",
	})

	checks, err := client.FetchPullRequestChecksWithPolicy(t.Context(), 31, PullRequestCheckPolicy{
		NoChecksPolicy: PullRequestNoChecksPass,
	})
	require.NoError(t, err)

	assert.Equal(t, PullRequestChecksPassed, checks.State)
	assert.Empty(t, checks.FailedCheckNames)
	assert.Equal(t, []string{"optional-lint"}, checks.OptionalFailedCheckNames)
	require.Len(t, checks.CheckRuns, 1)
	assert.False(t, checks.CheckRuns[0].Required)
	assert.Equal(t, "optional", checks.CheckRuns[0].RequirementSource)
	assert.Contains(t, checks.Summary, "optional failing checks")
}

func TestClassifyPullRequestChecksCanReworkOptionalFailuresByPolicy(t *testing.T) {
	t.Parallel()

	checks := PullRequestCheckSnapshot{
		CheckRuns: []PullRequestCheckRun{{
			Name:       "optional-lint",
			Status:     "completed",
			Conclusion: "failure",
		}},
	}

	classifyPullRequestChecks(&checks, PullRequestCheckPolicy{
		NoChecksPolicy:       PullRequestNoChecksPass,
		ReworkOptionalChecks: true,
	})

	assert.Equal(t, PullRequestChecksFailed, checks.State)
	assert.Equal(t, []string{"optional-lint"}, checks.FailedCheckNames)
	assert.Equal(t, []string{"optional-lint"}, checks.OptionalFailedCheckNames)
	assert.Empty(t, checks.RequiredFailedCheckNames)
	assert.Contains(t, checks.Summary, "optional checks configured for rework")
}

func TestGitHubClient_FetchPullRequestChecksFailsRequiredChecksOnly(t *testing.T) {
	t.Parallel()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, "Bearer token", r.Header.Get("Authorization"))

		switch {
		case r.Method == http.MethodGet && r.URL.Path == testGitHubPullRequestPath:
			writeTestResponse(t, w, `{"number":31,"html_url":"https://github.com/owner/repo/pull/31","state":"open","head":{"ref":"symphony/GH-2","sha":"abc123"},"base":{"ref":"main","sha":"base123"}}`)
		case r.Method == http.MethodGet && r.URL.Path == testGitHubCheckRunsPath:
			writeTestResponse(t, w, `{"check_runs":[{"name":"test","status":"completed","conclusion":"failure"},{"name":"optional-lint","status":"completed","conclusion":"failure"}]}`)
		case r.Method == http.MethodGet && r.URL.Path == testGitHubCommitStatusPath:
			writeTestResponse(t, w, `{"state":"success","statuses":[]}`)
		default:
			t.Errorf("unexpected GitHub request: %s %s", r.Method, r.URL.String())
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	client := NewGitHubClient(TrackerConfig{
		Endpoint: server.URL,
		APIKey:   "token",
		Owner:    "owner",
		Repo:     "repo",
	})

	checks, err := client.FetchPullRequestChecksWithPolicy(t.Context(), 31, PullRequestCheckPolicy{
		NoChecksPolicy:     PullRequestNoChecksPass,
		RequiredCheckNames: []string{"test"},
	})
	require.NoError(t, err)

	assert.Equal(t, PullRequestChecksFailed, checks.State)
	assert.Equal(t, []string{"test"}, checks.FailedCheckNames)
	assert.Equal(t, []string{"test"}, checks.RequiredFailedCheckNames)
	assert.Equal(t, []string{"optional-lint"}, checks.OptionalFailedCheckNames)
	require.Len(t, checks.CheckRuns, 2)
	assert.True(t, checks.CheckRuns[0].Required)
	assert.Equal(t, "config", checks.CheckRuns[0].RequirementSource)
	assert.False(t, checks.CheckRuns[1].Required)
}

func TestClassifyPullRequestChecksClassifiesLegacyStatusContexts(t *testing.T) {
	t.Parallel()

	checks := PullRequestCheckSnapshot{
		StatusContexts: []PullRequestStatus{
			{Context: "status/test", State: "failure"},
			{Context: "status/optional", State: "failure"},
		},
	}

	classifyPullRequestChecks(&checks, PullRequestCheckPolicy{
		NoChecksPolicy:     PullRequestNoChecksPass,
		RequiredCheckNames: []string{"status/test"},
	})

	assert.Equal(t, PullRequestChecksFailed, checks.State)
	assert.Equal(t, []string{"status/test"}, checks.FailedCheckNames)
	assert.Equal(t, []string{"status/test"}, checks.RequiredFailedCheckNames)
	assert.Equal(t, []string{"status/optional"}, checks.OptionalFailedCheckNames)
	require.Len(t, checks.StatusContexts, 2)
	assert.True(t, checks.StatusContexts[0].Required)
	assert.Equal(t, "config", checks.StatusContexts[0].RequirementSource)
	assert.False(t, checks.StatusContexts[1].Required)
	assert.Equal(t, "optional", checks.StatusContexts[1].RequirementSource)
}

func TestClassifyPullRequestChecksSupportsRequiredCheckPatterns(t *testing.T) {
	t.Parallel()

	checks := PullRequestCheckSnapshot{
		CheckRuns: []PullRequestCheckRun{{
			Name:       "ci/test",
			Status:     "completed",
			Conclusion: "failure",
		}},
	}

	classifyPullRequestChecks(&checks, PullRequestCheckPolicy{
		NoChecksPolicy:        PullRequestNoChecksPass,
		RequiredCheckPatterns: []string{"ci/*"},
	})

	assert.Equal(t, PullRequestChecksFailed, checks.State)
	assert.Equal(t, []string{"ci/test"}, checks.RequiredFailedCheckNames)
	assert.Equal(t, []string{"ci/*"}, checks.RequiredCheckPatterns)
	require.Len(t, checks.CheckRuns, 1)
	assert.True(t, checks.CheckRuns[0].Required)
	assert.Equal(t, "config_pattern", checks.CheckRuns[0].RequirementSource)
}

func TestGitHubClient_FetchPullRequestChecksTreatsNeutralAndSkippedAsPassing(t *testing.T) {
	t.Parallel()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, "Bearer token", r.Header.Get("Authorization"))

		switch {
		case r.Method == http.MethodGet && r.URL.Path == testGitHubPullRequestPath:
			writeTestResponse(t, w, `{"number":31,"html_url":"https://github.com/owner/repo/pull/31","state":"open","head":{"ref":"symphony/GH-2","sha":"abc123"},"base":{"ref":"main","sha":"base123"}}`)
		case r.Method == http.MethodGet && r.URL.Path == testGitHubCheckRunsPath:
			writeTestResponse(t, w, `{"check_runs":[{"name":"lint","status":"completed","conclusion":"neutral"},{"name":"docs","status":"completed","conclusion":"skipped"}]}`)
		case r.Method == http.MethodGet && r.URL.Path == testGitHubCommitStatusPath:
			writeTestResponse(t, w, `{"state":"success","statuses":[]}`)
		default:
			t.Errorf("unexpected GitHub request: %s %s", r.Method, r.URL.String())
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	client := NewGitHubClient(TrackerConfig{
		Endpoint: server.URL,
		APIKey:   "token",
		Owner:    "owner",
		Repo:     "repo",
	})

	checks, err := client.FetchPullRequestChecksWithPolicy(t.Context(), 31, PullRequestCheckPolicy{
		NoChecksPolicy:     PullRequestNoChecksPass,
		RequiredCheckNames: []string{"lint", "docs"},
	})
	require.NoError(t, err)

	assert.Equal(t, PullRequestChecksPassed, checks.State)
	assert.Empty(t, checks.FailedCheckNames)
	assert.Empty(t, checks.PendingRequiredCheckNames)
	assert.Contains(t, checks.Summary, "all required checks")
}

func TestClassifyPullRequestChecksTreatsUnknownConclusionsAsFailing(t *testing.T) {
	t.Parallel()

	checks := PullRequestCheckSnapshot{
		CheckRuns: []PullRequestCheckRun{{
			Name:       "experimental",
			Status:     "completed",
			Conclusion: "unexpected_new_conclusion",
		}},
	}

	classifyPullRequestChecks(&checks, PullRequestCheckPolicy{
		NoChecksPolicy:     PullRequestNoChecksPass,
		RequiredCheckNames: []string{"experimental"},
	})

	assert.Equal(t, PullRequestChecksFailed, checks.State)
	assert.Equal(t, []string{"experimental"}, checks.RequiredFailedCheckNames)
	assert.Contains(t, checks.Summary, "failing required checks")
}

func TestClassifyCheckRunStateAppliesDocumentedConclusionPolicy(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name       string
		status     string
		conclusion string
		expected   pullRequestObservedCheckState
	}{
		{name: "success passes", status: "completed", conclusion: "success", expected: pullRequestObservedCheckPassed},
		{name: "neutral passes", status: "completed", conclusion: "neutral", expected: pullRequestObservedCheckPassed},
		{name: "skipped passes", status: "completed", conclusion: "skipped", expected: pullRequestObservedCheckPassed},
		{name: "failure fails", status: "completed", conclusion: "failure", expected: pullRequestObservedCheckFailed},
		{
			name:       "GitHub cancellation conclusion fails",
			status:     "completed",
			conclusion: "cancelled", //nolint:misspell // Exact GitHub Checks API token.
			expected:   pullRequestObservedCheckFailed,
		},
		{name: "canceled alias fails", status: "completed", conclusion: "canceled", expected: pullRequestObservedCheckFailed},
		{name: "timed out fails", status: "completed", conclusion: "timed_out", expected: pullRequestObservedCheckFailed},
		{name: "action required fails", status: "completed", conclusion: "action_required", expected: pullRequestObservedCheckFailed},
		{name: "blank completed is pending", status: "completed", expected: pullRequestObservedCheckPending},
		{name: "in progress is pending", status: "in_progress", expected: pullRequestObservedCheckPending},
		{name: "unknown fails closed", status: "completed", conclusion: "new_conclusion", expected: pullRequestObservedCheckFailed},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			actual := classifyCheckRunState(PullRequestCheckRun{Status: test.status, Conclusion: test.conclusion})

			assert.Equal(t, test.expected, actual)
		})
	}
}

func TestGitHubClient_FetchPullRequestChecksUsesBranchProtectionRequiredChecks(t *testing.T) {
	t.Parallel()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, "Bearer token", r.Header.Get("Authorization"))

		switch {
		case r.Method == http.MethodGet && r.URL.Path == testGitHubPullRequestPath:
			writeTestResponse(t, w, `{"number":31,"html_url":"https://github.com/owner/repo/pull/31","state":"open","head":{"ref":"symphony/GH-2","sha":"abc123"},"base":{"ref":"main","sha":"base123"}}`)
		case r.Method == http.MethodGet && r.URL.Path == testGitHubCheckRunsPath:
			writeTestResponse(t, w, `{"check_runs":[{"name":"test","status":"completed","conclusion":"failure"}]}`)
		case r.Method == http.MethodGet && r.URL.Path == testGitHubCommitStatusPath:
			writeTestResponse(t, w, `{"state":"success","statuses":[]}`)
		case r.Method == http.MethodGet && r.URL.Path == testGitHubProtectionPath:
			writeTestResponse(t, w, `{"contexts":["test"],"checks":[]}`)
		case r.Method == http.MethodGet && r.URL.Path == testGitHubRulesPath:
			writeTestResponse(t, w, `[]`)
		default:
			t.Errorf("unexpected GitHub request: %s %s", r.Method, r.URL.String())
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	client := NewGitHubClient(TrackerConfig{
		Endpoint: server.URL,
		APIKey:   "token",
		Owner:    "owner",
		Repo:     "repo",
	})

	checks, err := client.FetchPullRequestChecksWithPolicy(t.Context(), 31, PullRequestCheckPolicy{
		NoChecksPolicy:         PullRequestNoChecksPass,
		DiscoverRequiredChecks: true,
	})
	require.NoError(t, err)

	assert.Equal(t, PullRequestChecksFailed, checks.State)
	assert.Equal(t, "branch_protection", checks.RequirementSource)
	assert.Equal(t, []string{"test"}, checks.RequiredCheckNames)
	assert.Equal(t, []string{"test"}, checks.RequiredFailedCheckNames)
	require.Len(t, checks.CheckRuns, 1)
	assert.True(t, checks.CheckRuns[0].Required)
	assert.Equal(t, "branch_protection", checks.CheckRuns[0].RequirementSource)
}

func TestGitHubRequiredStatusChecksContextsIncludesChecksArray(t *testing.T) {
	t.Parallel()

	payload := githubRequiredStatusChecks{
		Contexts: []string{"legacy-status"},
		Checks: []githubRequiredStatusCheck{{
			Context: "github-actions",
		}},
	}

	assert.Equal(t, []string{"github-actions", "legacy-status"}, payload.contexts())
}

func TestGitHubClient_FetchPullRequestChecksWaitsForDiscoveredRequiredChecks(t *testing.T) {
	t.Parallel()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, "Bearer token", r.Header.Get("Authorization"))

		switch {
		case r.Method == http.MethodGet && r.URL.Path == testGitHubPullRequestPath:
			writeTestResponse(t, w, `{"number":31,"html_url":"https://github.com/owner/repo/pull/31","state":"open","head":{"ref":"symphony/GH-2","sha":"abc123"},"base":{"ref":"main","sha":"base123"}}`)
		case r.Method == http.MethodGet && r.URL.Path == testGitHubCheckRunsPath:
			writeTestResponse(t, w, `{"check_runs":[]}`)
		case r.Method == http.MethodGet && r.URL.Path == testGitHubCommitStatusPath:
			writeTestResponse(t, w, `{"state":"success","statuses":[]}`)
		case r.Method == http.MethodGet && r.URL.Path == testGitHubProtectionPath:
			writeTestResponse(t, w, `{"contexts":["test"],"checks":[]}`)
		case r.Method == http.MethodGet && r.URL.Path == testGitHubRulesPath:
			writeTestResponse(t, w, `[]`)
		default:
			t.Errorf("unexpected GitHub request: %s %s", r.Method, r.URL.String())
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	client := NewGitHubClient(TrackerConfig{
		Endpoint: server.URL,
		APIKey:   "token",
		Owner:    "owner",
		Repo:     "repo",
	})

	checks, err := client.FetchPullRequestChecksWithPolicy(t.Context(), 31, PullRequestCheckPolicy{
		NoChecksPolicy:         PullRequestNoChecksPass,
		DiscoverRequiredChecks: true,
	})
	require.NoError(t, err)

	assert.Equal(t, PullRequestChecksPending, checks.State)
	assert.Equal(t, "branch_protection", checks.RequirementSource)
	assert.Equal(t, []string{"test"}, checks.RequiredCheckNames)
	assert.Equal(t, []string{"test"}, checks.MissingRequiredCheckNames)
	assert.Empty(t, checks.FailedCheckNames)
	assert.Contains(t, checks.Summary, "pending required checks")
}

func TestGitHubClient_FetchPullRequestChecksUsesRulesetRequiredChecks(t *testing.T) {
	t.Parallel()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, "Bearer token", r.Header.Get("Authorization"))

		switch {
		case r.Method == http.MethodGet && r.URL.Path == testGitHubPullRequestPath:
			writeTestResponse(t, w, `{"number":31,"html_url":"https://github.com/owner/repo/pull/31","state":"open","head":{"ref":"symphony/GH-2","sha":"abc123"},"base":{"ref":"main","sha":"base123"}}`)
		case r.Method == http.MethodGet && r.URL.Path == testGitHubCheckRunsPath:
			writeTestResponse(t, w, `{"check_runs":[{"name":"test","status":"completed","conclusion":"failure"}]}`)
		case r.Method == http.MethodGet && r.URL.Path == testGitHubCommitStatusPath:
			writeTestResponse(t, w, `{"state":"success","statuses":[]}`)
		case r.Method == http.MethodGet && r.URL.Path == testGitHubProtectionPath:
			http.NotFound(w, r)
		case r.Method == http.MethodGet && r.URL.Path == testGitHubRulesPath:
			writeTestResponse(t, w, `[{"type":"required_status_checks","parameters":{"required_status_checks":[{"context":"test"}]}}]`)
		default:
			t.Errorf("unexpected GitHub request: %s %s", r.Method, r.URL.String())
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	client := NewGitHubClient(TrackerConfig{
		Endpoint: server.URL,
		APIKey:   "token",
		Owner:    "owner",
		Repo:     "repo",
	})

	checks, err := client.FetchPullRequestChecksWithPolicy(t.Context(), 31, PullRequestCheckPolicy{
		NoChecksPolicy:         PullRequestNoChecksPass,
		DiscoverRequiredChecks: true,
	})
	require.NoError(t, err)

	assert.Equal(t, PullRequestChecksFailed, checks.State)
	assert.Equal(t, "ruleset", checks.RequirementSource)
	assert.Equal(t, []string{"test"}, checks.RequiredCheckNames)
	assert.Equal(t, []string{"test"}, checks.RequiredFailedCheckNames)
	require.Len(t, checks.CheckRuns, 1)
	assert.True(t, checks.CheckRuns[0].Required)
	assert.Equal(t, "ruleset", checks.CheckRuns[0].RequirementSource)
}

func TestGitHubClient_FetchPullRequestChecksCombinesRequirementSources(t *testing.T) {
	t.Parallel()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, "Bearer token", r.Header.Get("Authorization"))

		switch {
		case r.Method == http.MethodGet && r.URL.Path == testGitHubPullRequestPath:
			writeTestResponse(t, w, `{"number":31,"html_url":"https://github.com/owner/repo/pull/31","state":"open","head":{"ref":"symphony/GH-2","sha":"abc123"},"base":{"ref":"main","sha":"base123"}}`)
		case r.Method == http.MethodGet && r.URL.Path == testGitHubCheckRunsPath:
			writeTestResponse(t, w, `{"check_runs":[{"name":"test","status":"completed","conclusion":"failure"}]}`)
		case r.Method == http.MethodGet && r.URL.Path == testGitHubCommitStatusPath:
			writeTestResponse(t, w, `{"state":"success","statuses":[]}`)
		case r.Method == http.MethodGet && r.URL.Path == testGitHubProtectionPath:
			writeTestResponse(t, w, `{"contexts":["test"],"checks":[]}`)
		case r.Method == http.MethodGet && r.URL.Path == testGitHubRulesPath:
			writeTestResponse(t, w, `[{"type":"required_status_checks","parameters":{"required_status_checks":[{"context":"test"}]}}]`)
		default:
			t.Errorf("unexpected GitHub request: %s %s", r.Method, r.URL.String())
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	client := NewGitHubClient(TrackerConfig{
		Endpoint: server.URL,
		APIKey:   "token",
		Owner:    "owner",
		Repo:     "repo",
	})

	checks, err := client.FetchPullRequestChecksWithPolicy(t.Context(), 31, PullRequestCheckPolicy{
		NoChecksPolicy:         PullRequestNoChecksPass,
		RequiredCheckNames:     []string{"test"},
		DiscoverRequiredChecks: true,
	})
	require.NoError(t, err)

	assert.Equal(t, PullRequestChecksFailed, checks.State)
	assert.Equal(t, "branch_protection,config,ruleset", checks.RequirementSource)
	assert.Equal(t, []string{"test"}, checks.RequiredCheckNames)
	require.Len(t, checks.CheckRuns, 1)
	assert.True(t, checks.CheckRuns[0].Required)
	assert.Equal(t, "branch_protection,config,ruleset", checks.CheckRuns[0].RequirementSource)
}

func TestGitHubClient_FetchPullRequestChecksPaginatesRulesetRequiredChecks(t *testing.T) {
	t.Parallel()

	rulesRequests := 0

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, "Bearer token", r.Header.Get("Authorization"))

		switch {
		case r.Method == http.MethodGet && r.URL.Path == testGitHubPullRequestPath:
			writeTestResponse(t, w, `{"number":31,"html_url":"https://github.com/owner/repo/pull/31","state":"open","head":{"ref":"symphony/GH-2","sha":"abc123"},"base":{"ref":"main","sha":"base123"}}`)
		case r.Method == http.MethodGet && r.URL.Path == testGitHubCheckRunsPath:
			writeTestResponse(t, w, `{"check_runs":[{"name":"late-required","status":"completed","conclusion":"failure"}]}`)
		case r.Method == http.MethodGet && r.URL.Path == testGitHubCommitStatusPath:
			writeTestResponse(t, w, `{"state":"success","statuses":[]}`)
		case r.Method == http.MethodGet && r.URL.Path == testGitHubProtectionPath:
			http.NotFound(w, r)
		case r.Method == http.MethodGet && r.URL.Path == testGitHubRulesPath:
			rulesRequests++

			assert.Equal(t, "100", r.URL.Query().Get("per_page"))

			switch r.URL.Query().Get("page") {
			case "1":
				writeTestResponse(t, w, testGitHubRulesPayload(100, "pull_request", "ignored"))
			case "2":
				writeTestResponse(t, w, testGitHubRulesPayload(1, "required_status_checks", "late-required"))
			default:
				t.Errorf("unexpected rules page: %s", r.URL.RawQuery)
				http.NotFound(w, r)
			}
		default:
			t.Errorf("unexpected GitHub request: %s %s", r.Method, r.URL.String())
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	client := NewGitHubClient(TrackerConfig{
		Endpoint: server.URL,
		APIKey:   "token",
		Owner:    "owner",
		Repo:     "repo",
	})

	checks, err := client.FetchPullRequestChecksWithPolicy(t.Context(), 31, PullRequestCheckPolicy{
		NoChecksPolicy:         PullRequestNoChecksPass,
		DiscoverRequiredChecks: true,
	})
	require.NoError(t, err)

	assert.Equal(t, 2, rulesRequests)
	assert.Equal(t, PullRequestChecksFailed, checks.State)
	assert.Equal(t, "ruleset", checks.RequirementSource)
	assert.Equal(t, []string{"late-required"}, checks.RequiredCheckNames)
	assert.Equal(t, []string{"late-required"}, checks.RequiredFailedCheckNames)
}

func TestGitHubClient_FetchPullRequestChecksPaginatesCheckRuns(t *testing.T) {
	t.Parallel()

	checkRunRequests := 0

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, "Bearer token", r.Header.Get("Authorization"))

		switch {
		case r.Method == http.MethodGet && r.URL.Path == testGitHubPullRequestPath:
			writeTestResponse(t, w, `{"number":31,"html_url":"https://github.com/owner/repo/pull/31","state":"open","head":{"ref":"symphony/GH-2","sha":"abc123"},"base":{"ref":"main","sha":"base123"}}`)
		case r.Method == http.MethodGet && r.URL.Path == testGitHubCheckRunsPath:
			checkRunRequests++

			assert.Equal(t, "100", r.URL.Query().Get("per_page"))

			switch r.URL.Query().Get("page") {
			case "", "1":
				writeTestResponse(t, w, testGitHubCheckRunsPage(130, 1, 100, "run-130"))
			case "2":
				writeTestResponse(t, w, testGitHubCheckRunsPage(130, 101, 30, "run-130"))
			default:
				assert.Failf(t, "unexpected check-runs page", "query: %s", r.URL.RawQuery)
				http.NotFound(w, r)
			}
		case r.Method == http.MethodGet && r.URL.Path == testGitHubCommitStatusPath:
			writeTestResponse(t, w, `{"state":"success","total_count":0,"statuses":[]}`)
		default:
			assert.Failf(t, "unexpected GitHub request", "%s %s", r.Method, r.URL.String())
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	client := NewGitHubClient(TrackerConfig{
		Endpoint: server.URL,
		APIKey:   "token",
		Owner:    "owner",
		Repo:     "repo",
	})

	checks, err := client.FetchPullRequestChecksWithPolicy(t.Context(), 31, PullRequestCheckPolicy{
		TreatAllReportedAsRequired: true,
	})
	require.NoError(t, err)

	assert.Equal(t, 2, checkRunRequests)
	assert.Len(t, checks.CheckRuns, 130)
	assert.Equal(t, PullRequestChecksFailed, checks.State)
	assert.Equal(t, []string{"run-130"}, checks.RequiredFailedCheckNames)
	assert.Equal(t, []string{"run-130"}, checks.FailedCheckNames)
}

func TestGitHubClient_FetchPullRequestChecksPaginatesCommitStatuses(t *testing.T) {
	t.Parallel()

	statusRequests := 0

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, "Bearer token", r.Header.Get("Authorization"))

		switch {
		case r.Method == http.MethodGet && r.URL.Path == testGitHubPullRequestPath:
			writeTestResponse(t, w, `{"number":31,"html_url":"https://github.com/owner/repo/pull/31","state":"open","head":{"ref":"symphony/GH-2","sha":"abc123"},"base":{"ref":"main","sha":"base123"}}`)
		case r.Method == http.MethodGet && r.URL.Path == testGitHubCheckRunsPath:
			writeTestResponse(t, w, `{"total_count":0,"check_runs":[]}`)
		case r.Method == http.MethodGet && r.URL.Path == testGitHubCommitStatusPath:
			statusRequests++

			assert.Equal(t, "100", r.URL.Query().Get("per_page"))

			switch r.URL.Query().Get("page") {
			case "", "1":
				writeTestResponse(t, w, testGitHubCommitStatusPage(130, 1, 100, "status-130"))
			case "2":
				writeTestResponse(t, w, testGitHubCommitStatusPage(130, 101, 30, "status-130"))
			default:
				assert.Failf(t, "unexpected commit-status page", "query: %s", r.URL.RawQuery)
				http.NotFound(w, r)
			}
		default:
			assert.Failf(t, "unexpected GitHub request", "%s %s", r.Method, r.URL.String())
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	client := NewGitHubClient(TrackerConfig{
		Endpoint: server.URL,
		APIKey:   "token",
		Owner:    "owner",
		Repo:     "repo",
	})

	checks, err := client.FetchPullRequestChecksWithPolicy(t.Context(), 31, PullRequestCheckPolicy{
		TreatAllReportedAsRequired: true,
	})
	require.NoError(t, err)

	assert.Equal(t, 2, statusRequests)
	assert.Len(t, checks.StatusContexts, 130)
	assert.Equal(t, PullRequestChecksFailed, checks.State)
	assert.Equal(t, []string{"status-130"}, checks.RequiredFailedCheckNames)
	assert.Equal(t, []string{"status-130"}, checks.FailedCheckNames)
}

func TestGitHubClient_FetchPullRequestChecksReportsBranchProtectionLookupErrors(t *testing.T) {
	t.Parallel()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, "Bearer token", r.Header.Get("Authorization"))

		switch {
		case r.Method == http.MethodGet && r.URL.Path == testGitHubPullRequestPath:
			writeTestResponse(t, w, `{"number":31,"html_url":"https://github.com/owner/repo/pull/31","state":"open","head":{"ref":"symphony/GH-2","sha":"abc123"},"base":{"ref":"main","sha":"base123"}}`)
		case r.Method == http.MethodGet && r.URL.Path == testGitHubCheckRunsPath:
			writeTestResponse(t, w, `{"check_runs":[]}`)
		case r.Method == http.MethodGet && r.URL.Path == testGitHubCommitStatusPath:
			writeTestResponse(t, w, `{"state":"success","statuses":[]}`)
		case r.Method == http.MethodGet && r.URL.Path == testGitHubProtectionPath:
			http.Error(w, `{"message":"Resource not accessible by integration"}`, http.StatusForbidden)
		case r.Method == http.MethodGet && r.URL.Path == testGitHubRulesPath:
			writeTestResponse(t, w, `[]`)
		default:
			t.Errorf("unexpected GitHub request: %s %s", r.Method, r.URL.String())
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	client := NewGitHubClient(TrackerConfig{
		Endpoint: server.URL,
		APIKey:   "token",
		Owner:    "owner",
		Repo:     "repo",
	})

	checks, err := client.FetchPullRequestChecksWithPolicy(t.Context(), 31, PullRequestCheckPolicy{
		NoChecksPolicy:         PullRequestNoChecksPass,
		DiscoverRequiredChecks: true,
	})
	require.NoError(t, err)

	assert.Equal(t, PullRequestChecksPassed, checks.State)
	assert.Equal(t, "none", checks.RequirementSource)
	assert.Contains(t, checks.BranchProtectionError, "HTTP 403")
	assert.Contains(t, checks.BranchProtectionError, "Resource not accessible")
	assert.Empty(t, checks.RulesetError)
}

func TestGitHubClient_FetchPullRequestChecksReportsRulesetLookupErrors(t *testing.T) {
	t.Parallel()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, "Bearer token", r.Header.Get("Authorization"))

		switch {
		case r.Method == http.MethodGet && r.URL.Path == testGitHubPullRequestPath:
			writeTestResponse(t, w, `{"number":31,"html_url":"https://github.com/owner/repo/pull/31","state":"open","head":{"ref":"symphony/GH-2","sha":"abc123"},"base":{"ref":"main","sha":"base123"}}`)
		case r.Method == http.MethodGet && r.URL.Path == testGitHubCheckRunsPath:
			writeTestResponse(t, w, `{"check_runs":[]}`)
		case r.Method == http.MethodGet && r.URL.Path == testGitHubCommitStatusPath:
			writeTestResponse(t, w, `{"state":"success","statuses":[]}`)
		case r.Method == http.MethodGet && r.URL.Path == testGitHubProtectionPath:
			http.NotFound(w, r)
		case r.Method == http.MethodGet && r.URL.Path == testGitHubRulesPath:
			http.Error(w, `{"message":"Resource not accessible by integration"}`, http.StatusForbidden)
		default:
			t.Errorf("unexpected GitHub request: %s %s", r.Method, r.URL.String())
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	client := NewGitHubClient(TrackerConfig{
		Endpoint: server.URL,
		APIKey:   "token",
		Owner:    "owner",
		Repo:     "repo",
	})

	checks, err := client.FetchPullRequestChecksWithPolicy(t.Context(), 31, PullRequestCheckPolicy{
		NoChecksPolicy:         PullRequestNoChecksPass,
		DiscoverRequiredChecks: true,
	})
	require.NoError(t, err)

	assert.Equal(t, PullRequestChecksPassed, checks.State)
	assert.Equal(t, "none", checks.RequirementSource)
	assert.Empty(t, checks.BranchProtectionError)
	assert.Contains(t, checks.RulesetError, "HTTP 403")
	assert.Contains(t, checks.RulesetError, "Resource not accessible")
}

func TestGitHubClient_FetchPullRequestChecksFlagsBranchUpdate(t *testing.T) {
	t.Parallel()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, "Bearer token", r.Header.Get("Authorization"))

		switch {
		case r.Method == http.MethodGet && r.URL.Path == testGitHubPullRequestPath:
			writeTestResponse(t, w, `{"number":31,"html_url":"https://github.com/owner/repo/pull/31","state":"open","mergeable_state":"behind","head":{"ref":"symphony/GH-2","sha":"abc123"},"base":{"ref":"main","sha":"base123"}}`)
		default:
			t.Errorf("unexpected GitHub request: %s %s", r.Method, r.URL.String())
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	client := NewGitHubClient(TrackerConfig{
		Endpoint: server.URL,
		APIKey:   "token",
		Owner:    "owner",
		Repo:     "repo",
	})

	checks, err := client.FetchPullRequestChecks(t.Context(), 31)
	require.NoError(t, err)

	assert.Equal(t, PullRequestChecksPending, checks.State)
	assert.True(t, checks.NeedsBranchUpdate)
	assert.Equal(t, "behind", checks.MergeableState)
	assert.Equal(t, "main", checks.BaseRef)
	assert.Equal(t, "base123", checks.BaseSHA)
	assert.Contains(t, checks.BranchUpdateReason, "behind main")
}

func TestGitHubClient_FetchOpenPullRequestsByHeadPrefixInfersIssue(t *testing.T) {
	t.Parallel()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, "Bearer token", r.Header.Get("Authorization"))

		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/repos/owner/repo/pulls":
			assert.Equal(t, "open", r.URL.Query().Get("state"))
			writeTestResponse(t, w, `[{"number":31,"html_url":"https://github.com/owner/repo/pull/31","state":"open","head":{"ref":"symphony/GH-2","sha":"abc123"}},{"number":32,"html_url":"https://github.com/owner/repo/pull/32","state":"open","head":{"ref":"other/GH-3","sha":"def456"}}]`)
		case r.Method == http.MethodGet && r.URL.Path == "/repos/owner/repo/issues/2":
			writeTestResponse(t, w, `{"node_id":"node-2","number":2,"title":"Fix CI","state":"open","html_url":"https://github.com/owner/repo/issues/2","labels":[]}`)
		case r.Method == http.MethodGet && r.URL.Path == "/repos/owner/repo/issues/2/comments":
			writeTestResponse(t, w, `[]`)
		default:
			t.Errorf("unexpected GitHub request: %s %s", r.Method, r.URL.String())
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	client := NewGitHubClient(TrackerConfig{
		Endpoint: server.URL,
		APIKey:   "token",
		Owner:    "owner",
		Repo:     "repo",
	})

	pullRequests, err := client.FetchOpenPullRequestsByHeadPrefix(t.Context(), "symphony")
	require.NoError(t, err)
	require.Len(t, pullRequests, 1)

	assert.Equal(t, 31, pullRequests[0].PullRequest.Number)
	assert.Equal(t, "symphony/GH-2", pullRequests[0].Branch)
	assert.Equal(t, "GH-2", pullRequests[0].Issue.Identifier)
	assert.Equal(t, "Fix CI", pullRequests[0].Issue.Title)
}

func TestGitHubClient_FetchCandidateIssuesIncludesDiscussionComments(t *testing.T) {
	t.Parallel()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, "Bearer token", r.Header.Get("Authorization"))

		switch {
		case r.Method == http.MethodGet && r.URL.Path == testGitHubIssuesPath:
			assert.Equal(t, "open", r.URL.Query().Get("state"))
			writeTestResponse(t, w, `[{"node_id":"node-32","number":32,"title":"Persist admission records","state":"open","html_url":"https://github.com/owner/repo/issues/32","body":"Add child run artifacts.","labels":[{"name":"symphony"}]}]`)
		case r.Method == http.MethodGet && r.URL.Path == "/repos/owner/repo/issues/32/comments":
			writeTestResponse(t, w, `[{"html_url":"https://github.com/owner/repo/issues/32#issuecomment-1","user":{"login":"maintainer"},"author_association":"OWNER","created_at":"2026-05-26T08:52:52Z","updated_at":"2026-05-26T08:52:52Z","body":"Add a fixture where a child is denied before spawn."},{"html_url":"https://github.com/owner/repo/issues/32#issuecomment-2","user":{"login":"reviewer"},"author_association":"NONE","created_at":"2026-05-26T09:00:00Z","updated_at":"2026-05-26T09:00:00Z","body":"Also cover admitted then halted."}]`)
		default:
			t.Errorf("unexpected GitHub request: %s %s", r.Method, r.URL.String())
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	client := NewGitHubClient(TrackerConfig{
		Endpoint:     server.URL,
		APIKey:       "token",
		Owner:        "owner",
		Repo:         "repo",
		ActiveStates: []string{"OPEN"},
		Labels:       []string{"symphony"},
	})

	issues, err := client.FetchCandidateIssues(t.Context())
	require.NoError(t, err)
	require.Len(t, issues, 1)
	require.Len(t, issues[0].Comments, 2)

	assert.Equal(t, "maintainer", issues[0].Comments[0].Author)
	assert.Equal(t, "OWNER", issues[0].Comments[0].AuthorAssociation)
	assert.Contains(t, issues[0].Comments[0].Body, "denied before spawn")
	assert.Contains(t, issues[0].Comments[1].Body, "admitted then halted")
}

func TestGitHubClient_FetchIssueStatesByIDsAcceptsIssueReference(t *testing.T) {
	t.Parallel()

	var (
		issueFetches  int
		requestedList bool
	)

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, "Bearer token", r.Header.Get("Authorization"))

		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/repos/owner/repo/issues/218":
			issueFetches++

			writeTestResponse(t, w, `{"node_id":"node-218","number":218,"title":"Autonomous PR agent","state":"open","html_url":"https://github.com/owner/repo/issues/218","body":"Implement issue-to-PR flow.","labels":[{"name":"symphony"}]}`)
		case r.Method == http.MethodGet && r.URL.Path == "/repos/owner/repo/issues/218/comments":
			writeTestResponse(t, w, `[{"html_url":"https://github.com/owner/repo/issues/218#issuecomment-1","user":{"login":"maintainer"},"author_association":"OWNER","created_at":"2026-05-26T08:52:52Z","updated_at":"2026-05-26T08:52:52Z","body":"Use draft PRs on failed validation."}]`)
		case r.Method == http.MethodGet && r.URL.Path == testGitHubIssuesPath:
			requestedList = true

			http.Error(w, "issue-reference lookup should fetch by number", http.StatusInternalServerError)
		default:
			t.Errorf("unexpected GitHub request: %s %s", r.Method, r.URL.String())
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	client := NewGitHubClient(TrackerConfig{
		Endpoint: server.URL,
		APIKey:   "token",
		Owner:    "owner",
		Repo:     "repo",
	})

	issues, err := client.FetchIssueStatesByIDs(t.Context(), []string{"218", "gh-218", "https://github.com/owner/repo/issues/218", "GH-218"})
	require.NoError(t, err)
	require.Len(t, issues, 1)

	assert.Equal(t, 1, issueFetches)
	assert.False(t, requestedList)
	assert.Equal(t, "node-218", issues[0].ID)
	assert.Equal(t, "GH-218", issues[0].Identifier)
	assert.Equal(t, "Autonomous PR agent", issues[0].Title)
	require.Len(t, issues[0].Comments, 1)
	assert.Contains(t, issues[0].Comments[0].Body, "draft PRs")
}

func TestGitHubClient_FetchIssueStatesByIDsRejectsPullRequestReference(t *testing.T) {
	t.Parallel()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, "Bearer token", r.Header.Get("Authorization"))

		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/repos/owner/repo/issues/218":
			writeTestResponse(t, w, `{"node_id":"PR_node","number":218,"title":"Existing PR","state":"open","html_url":"https://github.com/owner/repo/pull/218","pull_request":{}}`)
		default:
			t.Errorf("unexpected GitHub request: %s %s", r.Method, r.URL.String())
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	client := NewGitHubClient(TrackerConfig{
		Endpoint: server.URL,
		APIKey:   "token",
		Owner:    "owner",
		Repo:     "repo",
	})

	_, err := client.FetchIssueStatesByIDs(t.Context(), []string{"GH-218"})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "github issue 218 is a pull request, not an issue")
}

func TestGitHubClient_UpsertsWatchIssuesByFingerprint(t *testing.T) {
	t.Parallel()

	var (
		created    bool
		patchCalls int
		storedBody string
	)

	finding := watch.Finding{
		Path:     "pkg/service/service.go",
		Kind:     watch.KindConventionDrift,
		Severity: watch.SeverityHigh,
		RuleID:   "watch." + watch.KindConventionDrift,
		Message:  "uses context.Background() outside allowed entrypoints/tests",
		Help:     "propagate caller contexts",
	}
	comparison := watch.CompareFindings(nil, []watch.Finding{finding})
	require.Len(t, comparison.NewFindings, 1)

	fingerprint := comparison.NewFindings[0].Fingerprint
	marker := watch.IssueFingerprintMarker(fingerprint)

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, "Bearer token", r.Header.Get("Authorization"))

		switch {
		case r.Method == http.MethodGet && r.URL.Path == testGitHubIssuesPath:
			assert.Equal(t, "all", r.URL.Query().Get("state"))

			if !created {
				writeTestResponse(t, w, `[]`)
				return
			}

			writeGitHubIssueList(t, w, storedBody)
		case r.Method == http.MethodPost && r.URL.Path == testGitHubIssuesPath:
			var body struct {
				Title  string   `json:"title"`
				Body   string   `json:"body"`
				Labels []string `json:"labels"`
			}

			if err := json.NewDecoder(r.Body).Decode(&body); !assert.NoError(t, err) {
				http.Error(w, "bad request", http.StatusBadRequest)
				return
			}

			assert.Equal(t, []string{"quality", "watch"}, body.Labels)
			assert.Contains(t, body.Body, marker)

			created = true
			storedBody = body.Body
			writeGitHubIssue(t, w, storedBody)
		case r.Method == http.MethodPatch && r.URL.Path == "/repos/owner/repo/issues/44":
			var body struct {
				Body string `json:"body"`
			}

			if err := json.NewDecoder(r.Body).Decode(&body); !assert.NoError(t, err) {
				http.Error(w, "bad request", http.StatusBadRequest)
				return
			}

			assert.Contains(t, body.Body, marker)

			patchCalls++
			storedBody = body.Body + "\nupdated"
			writeGitHubIssue(t, w, storedBody)
		default:
			assert.Failf(t, "unexpected GitHub request", "%s %s", r.Method, r.URL.String())
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	client := NewGitHubClient(TrackerConfig{
		Endpoint: server.URL,
		APIKey:   "token",
		Owner:    "owner",
		Repo:     "repo",
	})

	first, err := watch.UpsertIssues(t.Context(), client, comparison, watch.IssueOptions{
		Labels: []string{"quality", "watch"},
	})
	require.NoError(t, err)
	require.Len(t, first, 1)
	assert.Equal(t, watch.IssueActionCreated, first[0].Action)
	assert.Equal(t, 44, first[0].Issue.Number)

	second, err := watch.UpsertIssues(t.Context(), client, comparison, watch.IssueOptions{
		Labels: []string{"quality", "watch"},
	})
	require.NoError(t, err)
	require.Len(t, second, 1)
	assert.Equal(t, watch.IssueActionUpdated, second[0].Action)
	assert.Equal(t, 1, patchCalls)
}

func TestGitHubClient_ReopensClosedWatchIssueByFingerprint(t *testing.T) {
	t.Parallel()

	finding := watch.Finding{
		Path:     "pkg/service/service.go",
		Kind:     watch.KindConventionDrift,
		Severity: watch.SeverityHigh,
		RuleID:   "watch." + watch.KindConventionDrift,
		Message:  "uses context.Background() outside allowed entrypoints/tests",
	}
	comparison := watch.CompareFindings(nil, []watch.Finding{finding})
	require.Len(t, comparison.NewFindings, 1)

	marker := watch.IssueFingerprintMarker(comparison.NewFindings[0].Fingerprint)

	var patchCalls int

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == testGitHubIssuesPath:
			writeGitHubIssueListWithState(t, w, marker, "closed")
		case r.Method == http.MethodPatch && r.URL.Path == "/repos/owner/repo/issues/44":
			var body struct {
				State string `json:"state"`
			}

			if err := json.NewDecoder(r.Body).Decode(&body); !assert.NoError(t, err) {
				http.Error(w, "bad request", http.StatusBadRequest)
				return
			}

			assert.Equal(t, "open", body.State)

			patchCalls++

			writeGitHubIssueWithState(t, w, marker, "open")
		default:
			assert.Failf(t, "unexpected GitHub request", "%s %s", r.Method, r.URL.String())
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	client := NewGitHubClient(TrackerConfig{
		Endpoint: server.URL,
		APIKey:   "token",
		Owner:    "owner",
		Repo:     "repo",
	})

	results, err := watch.UpsertIssues(t.Context(), client, comparison, watch.IssueOptions{})
	require.NoError(t, err)
	require.Len(t, results, 1)
	assert.Equal(t, watch.IssueActionUpdated, results[0].Action)
	assert.Equal(t, "open", results[0].Issue.State)
	assert.Equal(t, 1, patchCalls)
}

func TestWatchIssueMutationPayloadOmitsUnsetLabels(t *testing.T) {
	t.Parallel()

	payload := watchIssueMutationPayload(watch.IssueDraft{
		Title: "Watch finding",
		Body:  "finding body",
	})

	assert.Equal(t, "Watch finding", payload["title"])
	assert.Equal(t, "finding body", payload["body"])
	assert.NotContains(t, payload, "labels")
}

func writeGitHubIssueList(t *testing.T, w http.ResponseWriter, body string) {
	t.Helper()

	writeGitHubIssueListWithState(t, w, body, "open")
}

func writeGitHubIssue(t *testing.T, w http.ResponseWriter, body string) {
	t.Helper()

	writeGitHubIssueWithState(t, w, body, "open")
}

const (
	testGitHubCheckStateSuccess = "success"
	testGitHubCheckStateFailure = "failure"
)

func testGitHubCheckRunsPage(total, start, count int, failingName string) string {
	runs := make([]string, 0, count)
	for i := range count {
		name := fmt.Sprintf("run-%03d", start+i)

		conclusion := testGitHubCheckStateSuccess
		if name == failingName {
			conclusion = testGitHubCheckStateFailure
		}

		runs = append(runs, fmt.Sprintf(`{"name":%q,"status":"completed","conclusion":%q}`, name, conclusion))
	}

	return fmt.Sprintf(`{"total_count":%d,"check_runs":[%s]}`, total, strings.Join(runs, ","))
}

func testGitHubCommitStatusPage(total, start, count int, failingContext string) string {
	statuses := make([]string, 0, count)
	for i := range count {
		statusContext := fmt.Sprintf("status-%03d", start+i)

		state := testGitHubCheckStateSuccess
		if statusContext == failingContext {
			state = testGitHubCheckStateFailure
		}

		statuses = append(statuses, fmt.Sprintf(`{"context":%q,"state":%q}`, statusContext, state))
	}

	return fmt.Sprintf(`{"state":"pending","total_count":%d,"statuses":[%s]}`, total, strings.Join(statuses, ","))
}

func testGitHubRulesPayload(count int, ruleType, checkContext string) string {
	rules := make([]string, 0, count)
	for range count {
		rules = append(rules, fmt.Sprintf(`{"type":%q,"parameters":{"required_status_checks":[{"context":%q}]}}`, ruleType, checkContext))
	}

	return `[` + strings.Join(rules, ",") + `]`
}

func writeGitHubIssueListWithState(t *testing.T, w http.ResponseWriter, body, state string) {
	t.Helper()

	writeTestResponse(t, w, `[`+githubIssuePayloadWithState(t, body, state)+`]`)
}

func writeGitHubIssueWithState(t *testing.T, w http.ResponseWriter, body, state string) {
	t.Helper()

	writeTestResponse(t, w, githubIssuePayloadWithState(t, body, state))
}

func githubIssuePayloadWithState(t *testing.T, body, state string) string {
	t.Helper()

	bodyData, err := json.Marshal(body)
	require.NoError(t, err)

	stateData, err := json.Marshal(state)
	require.NoError(t, err)

	payload := strings.ReplaceAll(`{"node_id":"watch-node","number":44,"title":"Watch finding","state":STATE,"html_url":"https://github.com/owner/repo/issues/44","body":BODY,"labels":[{"name":"quality"},{"name":"watch"}]}`, "STATE", string(stateData))

	return strings.ReplaceAll(payload, "BODY", string(bodyData))
}
