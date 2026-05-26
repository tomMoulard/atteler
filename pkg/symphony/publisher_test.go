//nolint:wsl_v5 // The table-like HTTP and git fixtures are clearer kept compact.
package symphony

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/tommoulard/atteler/pkg/shell"
)

const (
	githubPullsPath    = "/repos/owner/repo/pulls"
	gitStatusPorcelain = "status --porcelain"
	gitRemoteGetOrigin = "remote get-url origin"
)

func TestGitHubPublisher_CommitsPushesCreatesPRAndFinalizesIssue(t *testing.T) {
	t.Parallel()

	var requestMu sync.Mutex
	var requests []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requestMu.Lock()
		requests = append(requests, r.Method+" "+r.URL.Path+"?"+r.URL.RawQuery)
		requestMu.Unlock()
		assert.Equal(t, "Bearer token", r.Header.Get("Authorization"))

		switch {
		case r.Method == http.MethodGet && r.URL.Path == githubPullsPath:
			assert.Equal(t, "open", r.URL.Query().Get("state"))
			assert.Equal(t, "owner:symphony/GH-12", r.URL.Query().Get("head"))
			writeTestResponse(t, w, `[]`)
		case r.Method == http.MethodPost && r.URL.Path == githubPullsPath:
			var body map[string]any
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
				t.Errorf("decode pull request body: %v", err)
				http.Error(w, err.Error(), http.StatusBadRequest)
				return
			}
			assert.Equal(t, "GH-12: Make the CLI smaller", body["title"])
			assert.Equal(t, "symphony/GH-12", body["head"])
			assert.Equal(t, "main", body["base"])
			assert.Contains(t, body["body"], "Closes #12")
			w.WriteHeader(http.StatusCreated)
			writeTestResponse(t, w, `{"number":7,"html_url":"https://github.com/owner/repo/pull/7"}`)
		case r.Method == http.MethodDelete && r.URL.Path == "/repos/owner/repo/issues/12/labels/symphony":
			w.WriteHeader(http.StatusOK)
			writeTestResponse(t, w, `[]`)
		case r.Method == http.MethodPost && r.URL.Path == "/repos/owner/repo/issues/12/comments":
			var body map[string]string
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
				t.Errorf("decode issue comment body: %v", err)
				http.Error(w, err.Error(), http.StatusBadRequest)
				return
			}
			w.WriteHeader(http.StatusCreated)
			writeTestResponse(t, w, `{}`)
		default:
			t.Errorf("unexpected GitHub request: %s %s", r.Method, r.URL.String())
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	var commands []string
	var pushEnv []string
	var audits []shell.AuditContext
	runner := func(_ context.Context, _ string, env []string, audit shell.AuditContext, args ...string) ([]byte, error) {
		joined := strings.Join(args, " ")
		commands = append(commands, joined)
		audits = append(audits, audit)
		switch {
		case joined == "checkout -B symphony/GH-12":
			return nil, nil
		case joined == gitStatusPorcelain:
			return []byte(" M file.go\n"), nil
		case joined == "config user.name Atteler Symphony":
			return nil, nil
		case joined == "config user.email symphony@users.noreply.github.com":
			return nil, nil
		case joined == "add -A":
			return nil, nil
		case strings.HasPrefix(joined, "commit -F "):
			return []byte("[symphony/GH-12 abc123] publish\n"), nil
		case joined == "rev-parse HEAD":
			return []byte("abc123\n"), nil
		case joined == "rev-list --count main..HEAD":
			return []byte("1\n"), nil
		case joined == gitRemoteGetOrigin:
			return []byte("/local/origin\n"), nil
		case joined == "remote set-url origin "+server.URL+"/owner/repo.git":
			return nil, nil
		case joined == "push -u origin symphony/GH-12":
			pushEnv = append([]string(nil), env...)
			return nil, nil
		default:
			t.Fatalf("unexpected git command: %s", joined)
			return nil, nil
		}
	}

	cfg := Config{
		Tracker: TrackerConfig{
			Kind:     trackerKindGitHub,
			Endpoint: server.URL,
			APIKey:   "token",
			Owner:    "owner",
			Repo:     "repo",
			Labels:   []string{"symphony"},
		},
		Publish: PublishConfig{
			Enabled:      true,
			Remote:       "origin",
			BaseBranch:   "main",
			BranchPrefix: "symphony",
			RemoveLabels: []string{"symphony"},
			GitUserName:  "Atteler Symphony",
			GitUserEmail: "symphony@users.noreply.github.com",
		},
	}
	issueURL := "https://github.com/owner/repo/issues/12"
	publisher := &githubPublisher{
		cfg:    cfg,
		client: NewGitHubClient(cfg.Tracker),
		runGit: runner,
		logger: loggerOrDefault(nil),
	}

	result, err := publisher.Publish(t.Context(), Issue{
		ID:         "node",
		Identifier: "GH-12",
		Title:      "Make the CLI smaller",
		URL:        &issueURL,
	}, Workspace{Path: t.TempDir()})
	require.NoError(t, err)

	assert.True(t, result.Published)
	assert.Equal(t, "symphony/GH-12", result.Branch)
	assert.Equal(t, "abc123", result.CommitSHA)
	assert.Equal(t, 7, result.PullRequestNumber)
	assert.Equal(t, "https://github.com/owner/repo/pull/7", result.PullRequestURL)
	assert.Equal(t, []string{"symphony"}, result.RemovedLabels)
	assert.Contains(t, commands, "push -u origin symphony/GH-12")
	assert.Contains(t, pushEnv, "GIT_TERMINAL_PROMPT=0")
	assert.Contains(t, pushEnv, "GITHUB_TOKEN=token")
	require.NotEmpty(t, audits)
	for _, audit := range audits {
		assert.Equal(t, "symphony.git", audit.Caller)
		assert.Equal(t, "node", audit.IssueID)
		assert.Equal(t, "GH-12", audit.IssueIdentifier)
	}
	requestMu.Lock()
	defer requestMu.Unlock()
	assert.Contains(t, requests, "DELETE /repos/owner/repo/issues/12/labels/symphony?")
}

func TestGitHubPublisher_RebasesAndForcePushesPullRequestBranch(t *testing.T) {
	t.Parallel()

	var commands []string
	var fetchEnv []string
	var pushEnv []string
	runner := func(_ context.Context, _ string, env []string, _ shell.AuditContext, args ...string) ([]byte, error) {
		joined := strings.Join(args, " ")
		commands = append(commands, joined)
		switch joined {
		case gitRemoteGetOrigin:
			return []byte("/local/origin\n"), nil
		case "remote set-url origin https://github.example/owner/repo.git":
			return nil, nil
		case "fetch origin +refs/heads/main:refs/remotes/origin/main +refs/heads/symphony/GH-12:refs/remotes/origin/symphony/GH-12":
			fetchEnv = append([]string(nil), env...)
			return nil, nil
		case "checkout -B symphony/GH-12 origin/symphony/GH-12":
			return nil, nil
		case gitStatusPorcelain:
			return nil, nil
		case "rebase origin/main":
			return nil, nil
		case "rev-parse HEAD":
			return []byte("def456\n"), nil
		case "push --force-with-lease origin symphony/GH-12":
			pushEnv = append([]string(nil), env...)
			return nil, nil
		default:
			t.Fatalf("unexpected git command: %s", joined)
			return nil, nil
		}
	}

	cfg := Config{
		Tracker: TrackerConfig{
			Kind:   trackerKindGitHub,
			APIKey: "token",
			Owner:  "owner",
			Repo:   "repo",
		},
		Publish: PublishConfig{
			Remote:     "origin",
			RemoteURL:  "https://github.example/owner/repo.git",
			BaseBranch: "main",
		},
	}
	publisher := &githubPublisher{
		cfg:    cfg,
		client: NewGitHubClient(cfg.Tracker),
		runGit: runner,
		logger: loggerOrDefault(nil),
	}

	commitSHA, err := publisher.updatePullRequestBranch(t.Context(), t.TempDir(), "symphony/GH-12")
	require.NoError(t, err)

	assert.Equal(t, "def456", commitSHA)
	assert.Contains(t, commands, "rebase origin/main")
	assert.Contains(t, commands, "push --force-with-lease origin symphony/GH-12")
	assert.Contains(t, fetchEnv, "GIT_TERMINAL_PROMPT=0")
	assert.Contains(t, fetchEnv, "GITHUB_TOKEN=token")
	assert.Contains(t, pushEnv, "GIT_TERMINAL_PROMPT=0")
	assert.Contains(t, pushEnv, "GITHUB_TOKEN=token")
}

func TestGitHubPublisher_PreparesReworkWorkspaceAndLeavesRebaseConflict(t *testing.T) {
	t.Parallel()

	var commands []string
	runner := func(_ context.Context, _ string, _ []string, _ shell.AuditContext, args ...string) ([]byte, error) {
		joined := strings.Join(args, " ")
		commands = append(commands, joined)
		switch joined {
		case "rev-parse --git-path rebase-merge":
			return []byte(".git/rebase-merge\n"), nil
		case "rev-parse --git-path rebase-apply":
			return []byte(".git/rebase-apply\n"), nil
		case gitStatusPorcelain:
			return nil, nil
		case gitRemoteGetOrigin:
			return []byte("/local/origin\n"), nil
		case "remote set-url origin https://github.example/owner/repo.git":
			return nil, nil
		case "fetch origin +refs/heads/main:refs/remotes/origin/main +refs/heads/symphony/GH-12:refs/remotes/origin/symphony/GH-12":
			return nil, nil
		case "checkout -B symphony/GH-12 origin/symphony/GH-12":
			return nil, nil
		case "rebase origin/main":
			return nil, errors.New("conflict")
		default:
			t.Fatalf("unexpected git command: %s", joined)
			return nil, nil
		}
	}

	cfg := Config{
		Tracker: TrackerConfig{
			Kind:   trackerKindGitHub,
			APIKey: "token",
			Owner:  "owner",
			Repo:   "repo",
		},
		Publish: PublishConfig{
			Remote:     "origin",
			RemoteURL:  "https://github.example/owner/repo.git",
			BaseBranch: "main",
		},
	}
	publisher := &githubPublisher{
		cfg:    cfg,
		client: NewGitHubClient(cfg.Tracker),
		runGit: runner,
		logger: loggerOrDefault(nil),
	}

	err := publisher.preparePullRequestReworkWorkspace(t.Context(), t.TempDir(), "symphony/GH-12")
	require.NoError(t, err)

	assert.Contains(t, commands, "checkout -B symphony/GH-12 origin/symphony/GH-12")
	assert.Contains(t, commands, "rebase origin/main")
	assert.NotContains(t, commands, "rebase --abort")
}

func TestGitHubPublisher_SkipsCleanWorkspaceWithoutPullRequest(t *testing.T) {
	t.Parallel()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || r.URL.Path != githubPullsPath {
			t.Errorf("unexpected GitHub request: %s %s", r.Method, r.URL.String())
			http.NotFound(w, r)
			return
		}
		writeTestResponse(t, w, `[]`)
	}))
	defer server.Close()

	var commands []string
	runner := func(_ context.Context, _ string, _ []string, _ shell.AuditContext, args ...string) ([]byte, error) {
		joined := strings.Join(args, " ")
		commands = append(commands, joined)
		switch joined {
		case "checkout -B symphony/GH-12":
			return nil, nil
		case gitStatusPorcelain:
			return nil, nil
		case "rev-list --count main..HEAD":
			return []byte("0\n"), nil
		default:
			t.Fatalf("unexpected git command: %s", joined)
			return nil, nil
		}
	}

	cfg := Config{
		Tracker: TrackerConfig{
			Kind:     trackerKindGitHub,
			Endpoint: server.URL,
			APIKey:   "token",
			Owner:    "owner",
			Repo:     "repo",
		},
		Publish: PublishConfig{
			Enabled:      true,
			Remote:       "origin",
			BaseBranch:   "main",
			BranchPrefix: "symphony",
			RemoveLabels: []string{"symphony"},
		},
	}
	publisher := &githubPublisher{
		cfg:    cfg,
		client: NewGitHubClient(cfg.Tracker),
		runGit: runner,
		logger: loggerOrDefault(nil),
	}

	result, err := publisher.Publish(t.Context(), Issue{
		ID:         "node",
		Identifier: "GH-12",
		Title:      "Make the CLI smaller",
	}, Workspace{Path: t.TempDir()})
	require.NoError(t, err)

	assert.False(t, result.Published)
	assert.Equal(t, "workspace has no changes to publish", result.SkippedReason)
	assert.NotContains(t, commands, "push -u origin symphony/GH-12")
}

func writeTestResponse(t *testing.T, w http.ResponseWriter, body string) {
	t.Helper()

	_, err := w.Write([]byte(body))
	assert.NoError(t, err)
}
