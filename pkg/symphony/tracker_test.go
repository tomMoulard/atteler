package symphony

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/tommoulard/atteler/pkg/watch"
)

const testGitHubIssuesPath = "/repos/owner/repo/issues"

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

func TestGitHubClient_FetchPullRequestChecksClassifiesFailure(t *testing.T) {
	t.Parallel()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, "Bearer token", r.Header.Get("Authorization"))

		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/repos/owner/repo/pulls/31":
			writeTestResponse(t, w, `{"number":31,"html_url":"https://github.com/owner/repo/pull/31","state":"open","head":{"ref":"symphony/GH-2","sha":"abc123"}}`)
		case r.Method == http.MethodGet && r.URL.Path == "/repos/owner/repo/commits/abc123/check-runs":
			writeTestResponse(t, w, `{"check_runs":[{"name":"test","status":"completed","conclusion":"failure","details_url":"https://ci.example/test"},{"name":"lint","status":"completed","conclusion":"success"}]}`)
		case r.Method == http.MethodGet && r.URL.Path == "/repos/owner/repo/commits/abc123/status":
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

func TestGitHubClient_FetchPullRequestChecksFlagsBranchUpdate(t *testing.T) {
	t.Parallel()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, "Bearer token", r.Header.Get("Authorization"))

		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/repos/owner/repo/pulls/31":
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
