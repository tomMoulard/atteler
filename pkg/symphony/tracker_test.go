package symphony

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
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
