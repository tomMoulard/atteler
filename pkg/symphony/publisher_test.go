//nolint:wsl_v5 // The table-like HTTP and git fixtures are clearer kept compact.
package symphony

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/tommoulard/atteler/pkg/autonomy"
	"github.com/tommoulard/atteler/pkg/shell"
)

const (
	githubPullsPath         = "/repos/owner/repo/pulls"
	githubIssueLabelPath    = "/repos/owner/repo/issues/12/labels/symphony"
	githubIssueCommentsPath = "/repos/owner/repo/issues/12/comments"
	gitCheckoutBranch       = "checkout -B symphony/GH-12"
	gitCheckoutRemoteBranch = "checkout -B symphony/GH-12 origin/symphony/GH-12"
	gitStatusPorcelain      = "status --porcelain"
	gitRemoteGetOrigin      = "remote get-url origin"
	gitRevParseHead         = "rev-parse HEAD"
	gitRevListMainHead      = "rev-list --count main..HEAD"
	gitDiffMainHead         = "diff --name-only --diff-filter=ACDMRT main..HEAD"
	gitPushBranch           = "push -u origin symphony/GH-12"
	gitConfigUserName       = "config user.name Atteler Symphony"
	gitConfigUserEmail      = "config user.email symphony@users.noreply.github.com"
	gitAddAll               = "add -A"
)

func publisherTestConfig(endpoint string) Config {
	return Config{
		Tracker: TrackerConfig{
			Kind:     trackerKindGitHub,
			Endpoint: endpoint,
			APIKey:   "token",
			Owner:    "owner",
			Repo:     "repo",
			Labels:   []string{"symphony"},
		},
		Publish: PublishConfig{
			Enabled:                    true,
			Remote:                     "origin",
			BaseBranch:                 "main",
			BranchPrefix:               "symphony",
			RemoveLabels:               []string{"symphony"},
			GitUserName:                "Atteler Symphony",
			GitUserEmail:               "symphony@users.noreply.github.com",
			DraftOnFailedValidation:    true,
			VerificationOutputMaxBytes: defaultPRGateOutputBytes,
		},
	}
}

func successfulPublishGitRunner(t *testing.T, remoteURL string, commands *[]string) gitCommandRunner {
	t.Helper()

	var statusCalls int
	return func(_ context.Context, _ string, env []string, _ shell.AuditContext, args ...string) ([]byte, error) {
		joined := strings.Join(args, " ")
		if commands != nil {
			*commands = append(*commands, joined)
		}

		switch {
		case joined == gitCheckoutBranch:
			return nil, nil
		case joined == gitStatusPorcelain:
			statusCalls++
			if statusCalls > 2 {
				return nil, nil
			}

			return []byte(" M file.go\n"), nil
		case joined == gitConfigUserName:
			return nil, nil
		case joined == gitConfigUserEmail:
			return nil, nil
		case joined == gitAddAll:
			return nil, nil
		case strings.HasPrefix(joined, "commit -F "):
			return []byte("[symphony/GH-12 abc123] publish\n"), nil
		case joined == gitRevParseHead:
			return []byte("abc123\n"), nil
		case joined == gitRevListMainHead:
			return []byte("1\n"), nil
		case joined == gitDiffMainHead:
			return []byte("file.go\n"), nil
		case joined == gitRemoteGetOrigin:
			return []byte("/local/origin\n"), nil
		case joined == "remote set-url origin "+remoteURL+"/owner/repo.git":
			return nil, nil
		case joined == gitPushBranch:
			assert.Contains(t, env, "GIT_TERMINAL_PROMPT=0")
			assert.Contains(t, env, "GITHUB_TOKEN=token")
			return nil, nil
		default:
			t.Fatalf("unexpected git command: %s", joined)
			return nil, nil
		}
	}
}

func verificationRunnerFor(report VerificationReport) func(context.Context, Config, Issue, Workspace) (VerificationReport, error) {
	return func(context.Context, Config, Issue, Workspace) (VerificationReport, error) {
		return report, nil
	}
}

func passingVerificationReport() VerificationReport {
	return VerificationReport{
		Configured: true,
		Passed:     true,
		Gates: []VerificationGateResult{{
			Name:     "unit",
			Command:  "go test ./...",
			Status:   VerificationPassed,
			Stdout:   "gate-ok",
			Required: true,
		}},
	}
}

func failingVerificationReport() VerificationReport {
	return VerificationReport{
		Configured:     true,
		Passed:         false,
		FailedRequired: []string{"unit"},
		Gates: []VerificationGateResult{{
			Name:     "unit",
			Command:  "go test ./...",
			Status:   VerificationFailed,
			Error:    "exit status 1",
			Required: true,
		}},
	}
}

func TestParseGitStatusChangedFiles(t *testing.T) {
	t.Parallel()

	got := parseGitStatusChangedFiles(" M file.go\nA  new.go\nR  old.go -> renamed.go\n?? docs/guide.md\n M file.go\n")

	assert.Equal(t, []string{"file.go", "new.go", "renamed.go", "docs/guide.md"}, got)
}

func TestParseGitNameOnlyFiles(t *testing.T) {
	t.Parallel()

	got := parseGitNameOnlyFiles("file.go\n\n docs/guide.md \nfile.go\n")

	assert.Equal(t, []string{"file.go", "docs/guide.md"}, got)
}

func TestPublishReviewerFocusMapsIssueLabels(t *testing.T) {
	t.Parallel()

	got := publishReviewerFocus(Issue{
		Labels: []string{"security", "auth", "architecture", "quality", "docs", "ux", "roadmap"},
	})

	assert.Equal(t, "security, architecture/backend, quality/testing, documentation, UX/frontend", got)
}

func TestPublishSuggestedReviewersMapsIssueLabelsAndValidation(t *testing.T) {
	t.Parallel()

	got := publishSuggestedReviewers(Issue{
		Labels: []string{"security", "backend", "testing", "docs", "ux", "security"},
	}, VerificationReport{
		FailedRequired: []string{"unit"},
	})

	assert.Equal(t, "security reviewer, backend/architecture reviewer, test/quality reviewer, documentation reviewer, UX/frontend reviewer, CI/build owner for failed validation evidence", got)
}

func TestPublishSuggestedReviewersFallsBackToMaintainer(t *testing.T) {
	t.Parallel()

	assert.Equal(t, "maintainer familiar with the changed area", publishSuggestedReviewers(Issue{}, VerificationReport{}))
}

func TestFormatVerificationReportCallsOutOptionalFailures(t *testing.T) {
	t.Parallel()

	report := VerificationReport{
		Configured: true,
		Passed:     true,
		Gates: []VerificationGateResult{
			{
				Name:     "unit",
				Command:  "go test ./...",
				Status:   VerificationPassed,
				Required: true,
			},
			{
				Name:     "security-scan",
				Command:  "scanner --local",
				Status:   VerificationFailed,
				Required: false,
			},
		},
	}

	assert.Contains(t, formatVerificationReport(report), "Optional verification gate(s) failed: security-scan")
	assert.Contains(t, publishRiskAssessment(Issue{}, report), "Medium: optional local verification failed")
}

func TestFormatVerificationReportReportsPassingRequiredGatesOnce(t *testing.T) {
	t.Parallel()

	body := formatVerificationReport(VerificationReport{
		Configured: true,
		Passed:     true,
		Gates: []VerificationGateResult{{
			Name:     "unit",
			Command:  "go test ./...",
			Status:   VerificationPassed,
			Required: true,
		}},
	})

	assert.Equal(t, 1, strings.Count(body, "All required local verification gates passed."))
	assert.NotContains(t, body, "Required verification gate(s) failed")
}

func TestPublishPRBodyRedactsVerificationEvidence(t *testing.T) {
	t.Parallel()

	issueURL := "https://example.test/issues/12?api_key=url-secret"
	issue := Issue{
		Identifier: "GH-12",
		Title:      "api_key=title-secret",
		URL:        &issueURL,
		Labels:     []string{"api_key=label-secret"},
	}

	body := publishPRBody(issue, VerificationReport{
		Configured:     true,
		Passed:         false,
		FailedRequired: []string{"api_key=required-secret"},
		Gates: []VerificationGateResult{{
			Name:     "api_key=gate-secret",
			Command:  "printf api_key=command-secret",
			Status:   VerificationFailed,
			Stdout:   "api_key=stdout-secret",
			Stderr:   "api_key=stderr-secret",
			Error:    "api_key=error-secret",
			Required: true,
		}},
	}, true, []string{"secrets/api_key=file-secret.txt"})

	for _, secret := range []string{"title-secret", "url-secret", "label-secret", "required-secret", "gate-secret", "command-secret", "stdout-secret", "stderr-secret", "error-secret", "file-secret"} {
		assert.NotContains(t, body, secret)
		assert.NotContains(t, publishPRTitle(issue), secret)
	}
	assert.Contains(t, body, "[REDACTED]")
}

func TestPublishCommitMessageRedactsIssueTitle(t *testing.T) {
	t.Parallel()

	message := publishCommitMessage(Issue{
		Identifier: "GH-12",
		Title:      "api_key=title-secret",
	})

	assert.NotContains(t, message, "title-secret")
	assert.Contains(t, message, "[REDACTED]")
}

func TestPublishFailureCommitMessageRedactsEvidence(t *testing.T) {
	t.Parallel()

	message := publishFailureCommitMessage(Issue{
		Identifier: "GH-12",
		Title:      "api_key=title-secret",
	}, errors.New("api_key=worker-secret"))

	assert.Contains(t, message, "incomplete Symphony draft")
	assert.NotContains(t, message, "title-secret")
	assert.NotContains(t, message, "worker-secret")
	assert.Contains(t, message, "[REDACTED]")
}

func TestPublishFailureDraftSkipsWhenDraftFallbackDisabled(t *testing.T) {
	t.Parallel()

	cfg := publisherTestConfig("https://example.test")
	cfg.Publish.DraftOnFailedValidation = false

	result, err := PublishFailureDraft(t.Context(), cfg, Issue{
		Identifier: "GH-12",
		Title:      "Run failed",
	}, Workspace{Path: t.TempDir()}, assert.AnError, loggerOrDefault(nil))

	require.NoError(t, err)
	assert.Nil(t, result)
}

func TestPublishFailurePRBodyOnlyClaimsEmptyCommitWhenCreated(t *testing.T) {
	t.Parallel()

	body := publishFailurePRBody(Issue{Identifier: "GH-12", Title: "Run failed"}, "worker failed", nil, false)

	assert.Contains(t, body, "No changed files were captured before the incomplete run ended.")
	assert.NotContains(t, body, "empty draft commit")
}

func TestPublishFailurePRBodyReportsPartialChangedFiles(t *testing.T) {
	t.Parallel()

	body := publishFailurePRBody(Issue{Identifier: "GH-12", Title: "Run failed"}, "worker failed", []string{"partial.go"}, false)

	assert.Contains(t, body, "Partial implementation changes were committed")
	assert.Contains(t, body, "Changed file: `partial.go`")
	assert.Equal(t, 1, strings.Count(body, "## What changed"))
	assert.NotContains(t, body, "empty draft commit")
	assert.Contains(t, body, "Related to #12")
}

func TestPublishPRBodyUsesRelatedIssueWhenRequiredValidationFails(t *testing.T) {
	t.Parallel()

	body := publishPRBody(Issue{Identifier: "GH-12", Title: "Run failed"}, failingVerificationReport(), true, []string{"partial.go"})

	assert.Contains(t, body, "Required verification gate(s) failed: unit")
	assert.Contains(t, body, "Related to #12")
	assert.NotContains(t, body, "Closes #12")
}

func TestPublishFailurePRBodyRedactsEvidence(t *testing.T) {
	t.Parallel()

	issueURL := "https://example.test/issues/12?api_key=url-secret"
	issue := Issue{
		Identifier: "GH-12",
		Title:      "api_key=title-secret",
		URL:        &issueURL,
		Labels:     []string{"api_key=label-secret"},
	}

	body := publishFailurePRBody(issue, "api_key=worker-secret", []string{"partial/api_key=file-secret.go"}, false)

	for _, secret := range []string{"title-secret", "url-secret", "label-secret", "worker-secret", "file-secret"} {
		assert.NotContains(t, body, secret)
	}
	assert.Contains(t, body, "[REDACTED]")
}

func TestPublishIssueCommentsRedactRemovedLabels(t *testing.T) {
	t.Parallel()

	labels := []string{"symphony", "api_key=label-secret"}

	for _, body := range []string{
		publishIssueComment(GitHubPullRequest{Number: 7}, labels),
		publishFailureIssueComment(GitHubPullRequest{Number: 7}, labels),
		publishValidationFailureIssueComment(GitHubPullRequest{Number: 7}, labels),
	} {
		assert.Contains(t, body, "symphony")
		assert.NotContains(t, body, "label-secret")
		assert.Contains(t, body, "[REDACTED]")
	}
}

func TestPublishFailureIssueCommentDocumentsIncompleteDraft(t *testing.T) {
	t.Parallel()

	body := publishFailureIssueComment(GitHubPullRequest{Number: 7}, []string{"symphony"})

	assert.Contains(t, body, "draft pull request #7")
	assert.Contains(t, body, "could not complete before verification")
	assert.Contains(t, body, "draft PR documents the failure")
	assert.NotContains(t, body, "Symphony published pull request #7")
}

func TestPublishValidationFailureIssueCommentDocumentsDraftGateFailure(t *testing.T) {
	t.Parallel()

	body := publishValidationFailureIssueComment(GitHubPullRequest{Number: 7}, []string{"symphony"})

	assert.Contains(t, body, "draft pull request #7")
	assert.Contains(t, body, "required local verification failed")
	assert.Contains(t, body, "draft PR documents failed verification")
	assert.NotContains(t, body, "could not complete before verification")
}

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
		case r.Method == http.MethodDelete && r.URL.Path == githubIssueLabelPath:
			w.WriteHeader(http.StatusOK)
			writeTestResponse(t, w, `[]`)
		case r.Method == http.MethodPost && r.URL.Path == githubIssueCommentsPath:
			var body map[string]string
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
				t.Errorf("decode issue comment body: %v", err)
				http.Error(w, err.Error(), http.StatusBadRequest)
				return
			}
			assert.Contains(t, body["body"], "Symphony published pull request #7")
			assert.NotContains(t, body["body"], "worker failed before verification")
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
	var statusCalls int
	runner := func(_ context.Context, _ string, env []string, audit shell.AuditContext, args ...string) ([]byte, error) {
		joined := strings.Join(args, " ")
		commands = append(commands, joined)
		audits = append(audits, audit)
		switch {
		case joined == gitCheckoutBranch:
			return nil, nil
		case joined == gitStatusPorcelain:
			statusCalls++
			if statusCalls > 2 {
				return nil, nil
			}

			return []byte(" M file.go\n"), nil
		case joined == gitConfigUserName:
			return nil, nil
		case joined == gitConfigUserEmail:
			return nil, nil
		case joined == gitAddAll:
			return nil, nil
		case strings.HasPrefix(joined, "commit -F "):
			return []byte("[symphony/GH-12 abc123] publish\n"), nil
		case joined == gitRevParseHead:
			return []byte("abc123\n"), nil
		case joined == gitRevListMainHead:
			return []byte("1\n"), nil
		case joined == gitDiffMainHead:
			return []byte("file.go\n"), nil
		case joined == gitRemoteGetOrigin:
			return []byte("/local/origin\n"), nil
		case joined == "remote set-url origin "+server.URL+"/owner/repo.git":
			return nil, nil
		case joined == gitPushBranch:
			pushEnv = append([]string(nil), env...)
			return nil, nil
		default:
			t.Fatalf("unexpected git command: %s", joined)
			return nil, nil
		}
	}

	cfg := Config{
		Autonomy: autonomy.High,
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
	assert.Contains(t, commands, gitPushBranch)
	assert.Contains(t, pushEnv, "GIT_TERMINAL_PROMPT=0")
	assert.Contains(t, pushEnv, "GITHUB_TOKEN=token")
	require.NotEmpty(t, audits)
	for _, audit := range audits {
		assert.Equal(t, "symphony.git", audit.Caller)
		assert.Equal(t, "node", audit.IssueID)
		assert.Equal(t, "GH-12", audit.IssueIdentifier)
		assert.Equal(t, "high", audit.Autonomy)
	}
	requestMu.Lock()
	defer requestMu.Unlock()
	assert.Contains(t, requests, "DELETE "+githubIssueLabelPath+"?")
}

func TestGitHubPublisher_ReturnsPublishedResultWhenFinalizationFails(t *testing.T) {
	t.Parallel()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == githubPullsPath:
			writeTestResponse(t, w, `[]`)
		case r.Method == http.MethodPost && r.URL.Path == githubPullsPath:
			w.WriteHeader(http.StatusCreated)
			writeTestResponse(t, w, `{"number":7,"html_url":"https://github.com/owner/repo/pull/7"}`)
		case r.Method == http.MethodDelete && r.URL.Path == githubIssueLabelPath:
			http.Error(w, "label service unavailable", http.StatusInternalServerError)
		default:
			t.Errorf("unexpected GitHub request: %s %s", r.Method, r.URL.String())
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	cfg := publisherTestConfig(server.URL)
	publisher := &githubPublisher{
		cfg:             cfg,
		client:          NewGitHubClient(cfg.Tracker),
		runGit:          successfulPublishGitRunner(t, server.URL, nil),
		runVerification: verificationRunnerFor(passingVerificationReport()),
		logger:          loggerOrDefault(nil),
	}

	result, err := publisher.Publish(t.Context(), Issue{
		ID:         "node",
		Identifier: "GH-12",
		Title:      "Make the CLI smaller",
	}, Workspace{Path: t.TempDir()})
	require.Error(t, err)
	require.NotNil(t, result)
	assert.Contains(t, err.Error(), "remove issue label")
	assert.True(t, result.Published)
	assert.Equal(t, 7, result.PullRequestNumber)
	assert.Equal(t, "https://github.com/owner/repo/pull/7", result.PullRequestURL)
}

func TestGitHubPublisher_IncludesVerificationReportInPullRequest(t *testing.T) {
	t.Parallel()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == githubPullsPath:
			writeTestResponse(t, w, `[]`)
		case r.Method == http.MethodPost && r.URL.Path == githubPullsPath:
			var body map[string]any
			if !assert.NoError(t, json.NewDecoder(r.Body).Decode(&body)) {
				http.Error(w, "decode pull request body", http.StatusBadRequest)
				return
			}

			assert.Equal(t, false, body["draft"])
			assert.Contains(t, body["body"], "## What changed")
			assert.Contains(t, body["body"], "## Validation")
			assert.Contains(t, body["body"], "## Risk")
			assert.Contains(t, body["body"], "## Reviewer notes")
			assert.Contains(t, body["body"], "## Linked issue")
			assert.Contains(t, body["body"], "Changed file: `file.go`")
			assert.Contains(t, body["body"], "PASS `unit`")
			assert.Contains(t, body["body"], "stdout: gate-ok")
			assert.Contains(t, body["body"], "All required local verification gates passed")
			assert.Contains(t, body["body"], "Suggested reviewer focus: security, architecture/backend, quality/testing")
			assert.Contains(t, body["body"], "Suggested reviewers: security reviewer, backend/architecture reviewer, test/quality reviewer")
			assert.Contains(t, body["body"], "Issue: https://github.com/owner/repo/issues/12")
			assert.Contains(t, body["body"], "Closes #12")
			w.WriteHeader(http.StatusCreated)
			writeTestResponse(t, w, `{"number":7,"html_url":"https://github.com/owner/repo/pull/7"}`)
		case r.Method == http.MethodDelete && r.URL.Path == githubIssueLabelPath:
			w.WriteHeader(http.StatusOK)
			writeTestResponse(t, w, `[]`)
		case r.Method == http.MethodPost && r.URL.Path == githubIssueCommentsPath:
			var body map[string]string
			if !assert.NoError(t, json.NewDecoder(r.Body).Decode(&body)) {
				http.Error(w, "decode issue comment body", http.StatusBadRequest)
				return
			}

			assert.Contains(t, body["body"], "Symphony published pull request #7")
			assert.NotContains(t, body["body"], "worker failed before verification")
			w.WriteHeader(http.StatusCreated)
			writeTestResponse(t, w, `{}`)
		default:
			t.Errorf("unexpected GitHub request: %s %s", r.Method, r.URL.String())
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	runner := successfulPublishGitRunner(t, server.URL, nil)
	cfg := publisherTestConfig(server.URL)
	publisher := &githubPublisher{
		cfg:             cfg,
		client:          NewGitHubClient(cfg.Tracker),
		runGit:          runner,
		runVerification: verificationRunnerFor(passingVerificationReport()),
		logger:          loggerOrDefault(nil),
	}

	issueURL := "https://github.com/owner/repo/issues/12"
	result, err := publisher.Publish(t.Context(), Issue{
		ID:         "node",
		Identifier: "GH-12",
		Title:      "Make the CLI smaller",
		URL:        &issueURL,
		Labels:     []string{"security", "architecture", "quality"},
	}, Workspace{Path: t.TempDir()})
	require.NoError(t, err)

	require.NotNil(t, result.Verification)
	assert.True(t, result.Verification.Passed)
	assert.False(t, result.DraftDueToFailedVerification)
	assert.Equal(t, []string{"file.go"}, result.ChangedFiles)
}

func TestGitHubPublisher_UpdatesExistingPullRequestWithCommittedChangedFiles(t *testing.T) {
	t.Parallel()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == githubPullsPath:
			writeTestResponse(t, w, `[{"number":7,"html_url":"https://github.com/owner/repo/pull/7","state":"open","head":{"ref":"symphony/GH-12","sha":"abc123"}}]`)
		case r.Method == http.MethodPatch && r.URL.Path == githubPullsPath+"/7":
			var body map[string]any
			if !assert.NoError(t, json.NewDecoder(r.Body).Decode(&body)) {
				http.Error(w, "decode pull request body", http.StatusBadRequest)
				return
			}

			assert.Contains(t, body["body"], "Changed file: `committed.go`")
			assert.Contains(t, body["body"], "Changed file: `deleted.go`")
			w.WriteHeader(http.StatusOK)
			writeTestResponse(t, w, `{"number":7,"html_url":"https://github.com/owner/repo/pull/7"}`)
		case r.Method == http.MethodDelete && r.URL.Path == githubIssueLabelPath:
			w.WriteHeader(http.StatusOK)
			writeTestResponse(t, w, `[]`)
		case r.Method == http.MethodPost && r.URL.Path == githubIssueCommentsPath:
			var body map[string]string
			if !assert.NoError(t, json.NewDecoder(r.Body).Decode(&body)) {
				http.Error(w, "decode issue comment body", http.StatusBadRequest)
				return
			}

			assert.Contains(t, body["body"], "Symphony published pull request #7")
			assert.NotContains(t, body["body"], "worker failed before verification")
			w.WriteHeader(http.StatusCreated)
			writeTestResponse(t, w, `{}`)
		default:
			t.Errorf("unexpected GitHub request: %s %s", r.Method, r.URL.String())
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	var commands []string
	runner := func(_ context.Context, _ string, env []string, _ shell.AuditContext, args ...string) ([]byte, error) {
		joined := strings.Join(args, " ")
		commands = append(commands, joined)

		switch joined {
		case gitCheckoutBranch:
			return nil, nil
		case gitStatusPorcelain:
			return nil, nil
		case gitRevListMainHead:
			return []byte("1\n"), nil
		case gitDiffMainHead:
			return []byte("committed.go\ndeleted.go\n"), nil
		case gitRevParseHead:
			return []byte("abc123\n"), nil
		case gitRemoteGetOrigin:
			return []byte("/local/origin\n"), nil
		case "remote set-url origin " + server.URL + "/owner/repo.git":
			return nil, nil
		case gitPushBranch:
			assert.Contains(t, env, "GITHUB_TOKEN=token")
			return nil, nil
		default:
			t.Fatalf("unexpected git command: %s", joined)
			return nil, nil
		}
	}

	cfg := publisherTestConfig(server.URL)
	publisher := &githubPublisher{
		cfg:             cfg,
		client:          NewGitHubClient(cfg.Tracker),
		runGit:          runner,
		runVerification: verificationRunnerFor(passingVerificationReport()),
		logger:          loggerOrDefault(nil),
	}

	result, err := publisher.Publish(t.Context(), Issue{
		ID:         "node",
		Identifier: "GH-12",
		Title:      "Make the CLI smaller",
	}, Workspace{Path: t.TempDir()})
	require.NoError(t, err)

	assert.True(t, result.ExistingPullRequest)
	assert.Equal(t, []string{"committed.go", "deleted.go"}, result.ChangedFiles)
	assert.Contains(t, commands, gitDiffMainHead)
}

func TestGitHubPublisher_ReportsCommittedDiffAfterCommittingWorkspace(t *testing.T) {
	t.Parallel()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == githubPullsPath:
			writeTestResponse(t, w, `[]`)
		case r.Method == http.MethodPost && r.URL.Path == githubPullsPath:
			var body map[string]any
			if !assert.NoError(t, json.NewDecoder(r.Body).Decode(&body)) {
				http.Error(w, "decode pull request body", http.StatusBadRequest)
				return
			}

			assert.Contains(t, body["body"], "Changed file: `old.go`")
			assert.Contains(t, body["body"], "Changed file: `new.go`")
			w.WriteHeader(http.StatusCreated)
			writeTestResponse(t, w, `{"number":7,"html_url":"https://github.com/owner/repo/pull/7"}`)
		case r.Method == http.MethodDelete && r.URL.Path == githubIssueLabelPath:
			w.WriteHeader(http.StatusOK)
			writeTestResponse(t, w, `[]`)
		case r.Method == http.MethodPost && r.URL.Path == githubIssueCommentsPath:
			w.WriteHeader(http.StatusCreated)
			writeTestResponse(t, w, `{}`)
		default:
			t.Errorf("unexpected GitHub request: %s %s", r.Method, r.URL.String())
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	var statusCalls int
	runner := func(_ context.Context, _ string, env []string, _ shell.AuditContext, args ...string) ([]byte, error) {
		joined := strings.Join(args, " ")

		switch {
		case joined == gitCheckoutBranch:
			return nil, nil
		case joined == gitStatusPorcelain:
			statusCalls++
			if statusCalls > 2 {
				return nil, nil
			}

			return []byte(" M new.go\n"), nil
		case joined == gitConfigUserName:
			return nil, nil
		case joined == gitConfigUserEmail:
			return nil, nil
		case joined == gitAddAll:
			return nil, nil
		case strings.HasPrefix(joined, "commit -F "):
			return []byte("[symphony/GH-12 abc123] publish\n"), nil
		case joined == gitRevParseHead:
			return []byte("abc123\n"), nil
		case joined == gitRevListMainHead:
			return []byte("2\n"), nil
		case joined == gitDiffMainHead:
			return []byte("old.go\nnew.go\n"), nil
		case joined == gitRemoteGetOrigin:
			return []byte("/local/origin\n"), nil
		case joined == "remote set-url origin "+server.URL+"/owner/repo.git":
			return nil, nil
		case joined == gitPushBranch:
			assert.Contains(t, env, "GITHUB_TOKEN=token")
			return nil, nil
		default:
			t.Fatalf("unexpected git command: %s", joined)
			return nil, nil
		}
	}

	cfg := publisherTestConfig(server.URL)
	publisher := &githubPublisher{
		cfg:             cfg,
		client:          NewGitHubClient(cfg.Tracker),
		runGit:          runner,
		runVerification: verificationRunnerFor(passingVerificationReport()),
		logger:          loggerOrDefault(nil),
	}

	result, err := publisher.Publish(t.Context(), Issue{
		ID:         "node",
		Identifier: "GH-12",
		Title:      "Make the CLI smaller",
	}, Workspace{Path: t.TempDir()})
	require.NoError(t, err)

	assert.Equal(t, []string{"old.go", "new.go"}, result.ChangedFiles)
}

func TestGitHubPublisher_UpdatesPullRequestWhenCreateDiscoversRace(t *testing.T) {
	t.Parallel()

	var pullListRequests int
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == githubPullsPath:
			pullListRequests++
			if pullListRequests == 1 {
				writeTestResponse(t, w, `[]`)
				return
			}

			writeTestResponse(t, w, `[{"number":7,"html_url":"https://github.com/owner/repo/pull/7","state":"open","head":{"ref":"symphony/GH-12","sha":"abc123"}}]`)
		case r.Method == http.MethodPost && r.URL.Path == githubPullsPath:
			w.WriteHeader(httpStatusValidationFailed)
			writeTestResponse(t, w, `{"message":"Validation Failed"}`)
		case r.Method == http.MethodPatch && r.URL.Path == githubPullsPath+"/7":
			var body map[string]any
			if !assert.NoError(t, json.NewDecoder(r.Body).Decode(&body)) {
				http.Error(w, "decode pull request body", http.StatusBadRequest)
				return
			}

			assert.Contains(t, body["body"], "PASS `unit`")
			assert.Contains(t, body["body"], "Changed file: `file.go`")
			w.WriteHeader(http.StatusOK)
			writeTestResponse(t, w, `{"number":7,"html_url":"https://github.com/owner/repo/pull/7"}`)
		case r.Method == http.MethodDelete && r.URL.Path == githubIssueLabelPath:
			w.WriteHeader(http.StatusOK)
			writeTestResponse(t, w, `[]`)
		case r.Method == http.MethodPost && r.URL.Path == githubIssueCommentsPath:
			var body map[string]string
			if !assert.NoError(t, json.NewDecoder(r.Body).Decode(&body)) {
				http.Error(w, "decode issue comment body", http.StatusBadRequest)
				return
			}

			assert.Contains(t, body["body"], "Symphony published pull request #7")
			assert.NotContains(t, body["body"], "worker failed before verification")
			w.WriteHeader(http.StatusCreated)
			writeTestResponse(t, w, `{}`)
		default:
			t.Errorf("unexpected GitHub request: %s %s", r.Method, r.URL.String())
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	cfg := publisherTestConfig(server.URL)
	publisher := &githubPublisher{
		cfg:             cfg,
		client:          NewGitHubClient(cfg.Tracker),
		runGit:          successfulPublishGitRunner(t, server.URL, nil),
		runVerification: verificationRunnerFor(passingVerificationReport()),
		logger:          loggerOrDefault(nil),
	}

	result, err := publisher.Publish(t.Context(), Issue{
		ID:         "node",
		Identifier: "GH-12",
		Title:      "Make the CLI smaller",
	}, Workspace{Path: t.TempDir()})
	require.NoError(t, err)

	assert.Equal(t, 2, pullListRequests)
	assert.Equal(t, 7, result.PullRequestNumber)
	assert.Equal(t, "https://github.com/owner/repo/pull/7", result.PullRequestURL)
	assert.True(t, result.ExistingPullRequest)
}

func TestGitHubPublisher_OpensDraftPullRequestWhenRequiredVerificationFails(t *testing.T) {
	t.Parallel()

	var converted atomic.Bool
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == githubPullsPath:
			writeTestResponse(t, w, `[]`)
		case r.Method == http.MethodPost && r.URL.Path == githubPullsPath:
			var body map[string]any
			if !assert.NoError(t, json.NewDecoder(r.Body).Decode(&body)) {
				http.Error(w, "decode pull request body", http.StatusBadRequest)
				return
			}

			assert.Equal(t, true, body["draft"])
			assert.Contains(t, body["body"], "FAIL `unit`")
			assert.Contains(t, body["body"], "Required verification gate(s) failed: unit")
			assert.Contains(t, body["body"], "not ready")
			w.WriteHeader(http.StatusCreated)
			writeTestResponse(t, w, `{"number":7,"node_id":"PR_node","draft":false,"html_url":"https://github.com/owner/repo/pull/7"}`)
		case r.Method == http.MethodPost && r.URL.Path == githubGraphQLPath:
			converted.Store(true)
			writeTestResponse(t, w, `{"data":{"convertPullRequestToDraft":{"pullRequest":{"id":"PR_node","isDraft":true}}}}`)
		case r.Method == http.MethodDelete && r.URL.Path == githubIssueLabelPath:
			w.WriteHeader(http.StatusOK)
			writeTestResponse(t, w, `[]`)
		case r.Method == http.MethodPost && r.URL.Path == githubIssueCommentsPath:
			var body map[string]string
			if !assert.NoError(t, json.NewDecoder(r.Body).Decode(&body)) {
				http.Error(w, "decode issue comment body", http.StatusBadRequest)
				return
			}

			assert.Contains(t, body["body"], "draft pull request #7")
			assert.Contains(t, body["body"], "required local verification failed")
			assert.Contains(t, body["body"], "draft PR documents failed verification")
			assert.NotContains(t, body["body"], "could not complete before verification")
			w.WriteHeader(http.StatusCreated)
			writeTestResponse(t, w, `{}`)
		default:
			t.Errorf("unexpected GitHub request: %s %s", r.Method, r.URL.String())
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	cfg := publisherTestConfig(server.URL)
	publisher := &githubPublisher{
		cfg:             cfg,
		client:          NewGitHubClient(cfg.Tracker),
		runGit:          successfulPublishGitRunner(t, server.URL, nil),
		runVerification: verificationRunnerFor(failingVerificationReport()),
		logger:          loggerOrDefault(nil),
	}

	result, err := publisher.Publish(t.Context(), Issue{
		ID:         "node",
		Identifier: "GH-12",
		Title:      "Make the CLI smaller",
	}, Workspace{Path: t.TempDir()})
	require.NoError(t, err)

	require.NotNil(t, result.Verification)
	assert.False(t, result.Verification.Passed)
	assert.Equal(t, []string{"unit"}, result.Verification.FailedRequired)
	assert.True(t, result.DraftDueToFailedVerification)
	assert.True(t, converted.Load())
}

func TestGitHubPublisher_ReturnsPartialPullRequestWhenDraftConversionFails(t *testing.T) {
	t.Parallel()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == githubPullsPath:
			writeTestResponse(t, w, `[]`)
		case r.Method == http.MethodPost && r.URL.Path == githubPullsPath:
			w.WriteHeader(http.StatusCreated)
			writeTestResponse(t, w, `{"number":7,"node_id":"PR_node","draft":false,"html_url":"https://github.com/owner/repo/pull/7"}`)
		case r.Method == http.MethodPost && r.URL.Path == githubGraphQLPath:
			http.Error(w, "draft conversion unavailable", http.StatusInternalServerError)
		default:
			t.Errorf("unexpected GitHub request after draft conversion failure: %s %s", r.Method, r.URL.String())
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	cfg := publisherTestConfig(server.URL)
	publisher := &githubPublisher{
		cfg:             cfg,
		client:          NewGitHubClient(cfg.Tracker),
		runGit:          successfulPublishGitRunner(t, server.URL, nil),
		runVerification: verificationRunnerFor(failingVerificationReport()),
		logger:          loggerOrDefault(nil),
	}

	result, err := publisher.Publish(t.Context(), Issue{
		ID:         "node",
		Identifier: "GH-12",
		Title:      "Make the CLI smaller",
	}, Workspace{Path: t.TempDir()})
	require.Error(t, err)
	require.NotNil(t, result)
	require.NotNil(t, result.Verification)
	assert.True(t, result.Published)
	assert.True(t, result.DraftDueToFailedVerification)
	assert.Equal(t, 7, result.PullRequestNumber)
	assert.Equal(t, "https://github.com/owner/repo/pull/7", result.PullRequestURL)
	assert.Equal(t, []string{"unit"}, result.Verification.FailedRequired)
}

func TestGitHubPublisher_DraftsWhenVerificationLeavesWorkspaceDirty(t *testing.T) {
	t.Parallel()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == githubPullsPath:
			writeTestResponse(t, w, `[]`)
		case r.Method == http.MethodPost && r.URL.Path == githubPullsPath:
			var body map[string]any
			if !assert.NoError(t, json.NewDecoder(r.Body).Decode(&body)) {
				http.Error(w, "decode pull request body", http.StatusBadRequest)
				return
			}

			assert.Equal(t, true, body["draft"])
			assert.Contains(t, body["body"], "FAIL `workspace_clean`")
			assert.Contains(t, body["body"], "Required verification gate(s) failed: workspace_clean")
			assert.Contains(t, body["body"], "workspace has uncommitted changes after verification gates")
			assert.NotContains(t, body["body"], "generated-secret")
			w.WriteHeader(http.StatusCreated)
			writeTestResponse(t, w, `{"number":7,"draft":true,"html_url":"https://github.com/owner/repo/pull/7"}`)
		case r.Method == http.MethodDelete && r.URL.Path == githubIssueLabelPath:
			w.WriteHeader(http.StatusOK)
			writeTestResponse(t, w, `[]`)
		case r.Method == http.MethodPost && r.URL.Path == githubIssueCommentsPath:
			w.WriteHeader(http.StatusCreated)
			writeTestResponse(t, w, `{}`)
		default:
			t.Errorf("unexpected GitHub request: %s %s", r.Method, r.URL.String())
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	var statusCalls int
	runner := func(_ context.Context, _ string, env []string, _ shell.AuditContext, args ...string) ([]byte, error) {
		joined := strings.Join(args, " ")

		switch {
		case joined == gitCheckoutBranch:
			return nil, nil
		case joined == gitStatusPorcelain:
			statusCalls++
			switch statusCalls {
			case 1, 2:
				return []byte(" M file.go\n"), nil
			case 3:
				return []byte(" M api_key=generated-secret.go\n"), nil
			default:
				return nil, nil
			}
		case joined == gitConfigUserName:
			return nil, nil
		case joined == gitConfigUserEmail:
			return nil, nil
		case joined == gitAddAll:
			return nil, nil
		case strings.HasPrefix(joined, "commit -F "):
			return []byte("[symphony/GH-12 abc123] publish\n"), nil
		case joined == gitRevParseHead:
			return []byte("abc123\n"), nil
		case joined == gitRevListMainHead:
			return []byte("1\n"), nil
		case joined == gitDiffMainHead:
			return []byte("file.go\n"), nil
		case joined == gitRemoteGetOrigin:
			return []byte("/local/origin\n"), nil
		case joined == "remote set-url origin "+server.URL+"/owner/repo.git":
			return nil, nil
		case joined == gitPushBranch:
			assert.Contains(t, env, "GITHUB_TOKEN=token")
			return nil, nil
		default:
			t.Fatalf("unexpected git command: %s", joined)
			return nil, nil
		}
	}

	cfg := publisherTestConfig(server.URL)
	publisher := &githubPublisher{
		cfg:             cfg,
		client:          NewGitHubClient(cfg.Tracker),
		runGit:          runner,
		runVerification: verificationRunnerFor(passingVerificationReport()),
		logger:          loggerOrDefault(nil),
	}

	result, err := publisher.Publish(t.Context(), Issue{
		ID:         "node",
		Identifier: "GH-12",
		Title:      "Make the CLI smaller",
	}, Workspace{Path: t.TempDir()})
	require.NoError(t, err)

	require.NotNil(t, result.Verification)
	assert.False(t, result.Verification.Passed)
	assert.False(t, result.Verification.CompletedAt.IsZero())
	assert.Equal(t, []string{"workspace_clean"}, result.Verification.FailedRequired)
	assert.True(t, result.DraftDueToFailedVerification)
	assert.Equal(t, []string{"file.go"}, result.ChangedFiles)
	assert.Equal(t, 3, statusCalls)
}

func TestGitHubPublisher_ConvertsExistingPullRequestToDraftWhenRequiredVerificationFails(t *testing.T) {
	t.Parallel()

	var converted atomic.Bool
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == githubPullsPath:
			writeTestResponse(t, w, `[{"number":7,"node_id":"PR_node","draft":false,"html_url":"https://github.com/owner/repo/pull/7","state":"open","head":{"ref":"symphony/GH-12","sha":"abc123"}}]`)
		case r.Method == http.MethodPatch && r.URL.Path == githubPullsPath+"/7":
			var body map[string]any
			if !assert.NoError(t, json.NewDecoder(r.Body).Decode(&body)) {
				http.Error(w, "decode pull request body", http.StatusBadRequest)
				return
			}

			assert.Contains(t, body["body"], "FAIL `unit`")
			assert.Contains(t, body["body"], "not ready")
			w.WriteHeader(http.StatusOK)
			writeTestResponse(t, w, `{"number":7,"node_id":"PR_node","draft":false,"html_url":"https://github.com/owner/repo/pull/7"}`)
		case r.Method == http.MethodPost && r.URL.Path == githubGraphQLPath:
			var body map[string]any
			if !assert.NoError(t, json.NewDecoder(r.Body).Decode(&body)) {
				http.Error(w, "decode graphql body", http.StatusBadRequest)
				return
			}

			query, ok := body["query"].(string)
			if !assert.True(t, ok) {
				http.Error(w, "query is not a string", http.StatusBadRequest)
				return
			}

			variables, ok := body["variables"].(map[string]any)
			if !assert.True(t, ok) {
				http.Error(w, "variables are not an object", http.StatusBadRequest)
				return
			}

			assert.Contains(t, query, "convertPullRequestToDraft")
			assert.Equal(t, "PR_node", variables["pullRequestId"])
			converted.Store(true)
			writeTestResponse(t, w, `{"data":{"convertPullRequestToDraft":{"pullRequest":{"id":"PR_node","isDraft":true}}}}`)
		case r.Method == http.MethodDelete && r.URL.Path == githubIssueLabelPath:
			w.WriteHeader(http.StatusOK)
			writeTestResponse(t, w, `[]`)
		case r.Method == http.MethodPost && r.URL.Path == githubIssueCommentsPath:
			w.WriteHeader(http.StatusCreated)
			writeTestResponse(t, w, `{}`)
		default:
			t.Errorf("unexpected GitHub request: %s %s", r.Method, r.URL.String())
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	cfg := publisherTestConfig(server.URL)
	publisher := &githubPublisher{
		cfg:             cfg,
		client:          NewGitHubClient(cfg.Tracker),
		runGit:          successfulPublishGitRunner(t, server.URL, nil),
		runVerification: verificationRunnerFor(failingVerificationReport()),
		logger:          loggerOrDefault(nil),
	}

	result, err := publisher.Publish(t.Context(), Issue{
		ID:         "node",
		Identifier: "GH-12",
		Title:      "Make the CLI smaller",
	}, Workspace{Path: t.TempDir()})
	require.NoError(t, err)

	assert.True(t, converted.Load())
	assert.True(t, result.ExistingPullRequest)
	assert.True(t, result.DraftDueToFailedVerification)
}

func TestGitHubPublisher_MarksExistingDraftPullRequestReadyWhenVerificationPasses(t *testing.T) {
	t.Parallel()

	var markedReady atomic.Bool
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == githubPullsPath:
			writeTestResponse(t, w, `[{"number":7,"node_id":"PR_node","draft":true,"html_url":"https://github.com/owner/repo/pull/7","state":"open","head":{"ref":"symphony/GH-12","sha":"abc123"}}]`)
		case r.Method == http.MethodPatch && r.URL.Path == githubPullsPath+"/7":
			var body map[string]any
			if !assert.NoError(t, json.NewDecoder(r.Body).Decode(&body)) {
				http.Error(w, "decode pull request body", http.StatusBadRequest)
				return
			}

			assert.Contains(t, body["body"], "PASS `unit`")
			assert.NotContains(t, body["body"], "not ready")
			w.WriteHeader(http.StatusOK)
			writeTestResponse(t, w, `{"number":7,"node_id":"PR_node","draft":true,"html_url":"https://github.com/owner/repo/pull/7"}`)
		case r.Method == http.MethodPost && r.URL.Path == githubGraphQLPath:
			var body map[string]any
			if !assert.NoError(t, json.NewDecoder(r.Body).Decode(&body)) {
				http.Error(w, "decode graphql body", http.StatusBadRequest)
				return
			}

			query, ok := body["query"].(string)
			if !assert.True(t, ok) {
				http.Error(w, "query is not a string", http.StatusBadRequest)
				return
			}

			variables, ok := body["variables"].(map[string]any)
			if !assert.True(t, ok) {
				http.Error(w, "variables are not an object", http.StatusBadRequest)
				return
			}

			assert.Contains(t, query, "markPullRequestReadyForReview")
			assert.Equal(t, "PR_node", variables["pullRequestId"])
			markedReady.Store(true)
			writeTestResponse(t, w, `{"data":{"markPullRequestReadyForReview":{"pullRequest":{"id":"PR_node","isDraft":false}}}}`)
		case r.Method == http.MethodDelete && r.URL.Path == githubIssueLabelPath:
			w.WriteHeader(http.StatusOK)
			writeTestResponse(t, w, `[]`)
		case r.Method == http.MethodPost && r.URL.Path == githubIssueCommentsPath:
			w.WriteHeader(http.StatusCreated)
			writeTestResponse(t, w, `{}`)
		default:
			t.Errorf("unexpected GitHub request: %s %s", r.Method, r.URL.String())
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	cfg := publisherTestConfig(server.URL)
	publisher := &githubPublisher{
		cfg:             cfg,
		client:          NewGitHubClient(cfg.Tracker),
		runGit:          successfulPublishGitRunner(t, server.URL, nil),
		runVerification: verificationRunnerFor(passingVerificationReport()),
		logger:          loggerOrDefault(nil),
	}

	result, err := publisher.Publish(t.Context(), Issue{
		ID:         "node",
		Identifier: "GH-12",
		Title:      "Make the CLI smaller",
	}, Workspace{Path: t.TempDir()})
	require.NoError(t, err)

	assert.True(t, markedReady.Load())
	assert.True(t, result.ExistingPullRequest)
	assert.False(t, result.DraftDueToFailedVerification)
	require.NotNil(t, result.Verification)
	assert.True(t, result.Verification.Passed)
}

func TestGitHubPublisher_DoesNotMarkReadyWhenUpdateReportsPullRequestIsReady(t *testing.T) {
	t.Parallel()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == githubPullsPath:
			writeTestResponse(t, w, `[{"number":7,"node_id":"PR_node","draft":true,"html_url":"https://github.com/owner/repo/pull/7","state":"open","head":{"ref":"symphony/GH-12","sha":"abc123"}}]`)
		case r.Method == http.MethodPatch && r.URL.Path == githubPullsPath+"/7":
			w.WriteHeader(http.StatusOK)
			writeTestResponse(t, w, `{"number":7,"node_id":"PR_node","draft":false,"html_url":"https://github.com/owner/repo/pull/7"}`)
		case r.Method == http.MethodDelete && r.URL.Path == githubIssueLabelPath:
			w.WriteHeader(http.StatusOK)
			writeTestResponse(t, w, `[]`)
		case r.Method == http.MethodPost && r.URL.Path == githubIssueCommentsPath:
			w.WriteHeader(http.StatusCreated)
			writeTestResponse(t, w, `{}`)
		case r.Method == http.MethodPost && r.URL.Path == githubGraphQLPath:
			t.Errorf("unexpected draft-state GraphQL mutation after update reported ready PR")
			http.NotFound(w, r)
		default:
			t.Errorf("unexpected GitHub request: %s %s", r.Method, r.URL.String())
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	cfg := publisherTestConfig(server.URL)
	publisher := &githubPublisher{
		cfg:             cfg,
		client:          NewGitHubClient(cfg.Tracker),
		runGit:          successfulPublishGitRunner(t, server.URL, nil),
		runVerification: verificationRunnerFor(passingVerificationReport()),
		logger:          loggerOrDefault(nil),
	}

	result, err := publisher.Publish(t.Context(), Issue{
		ID:         "node",
		Identifier: "GH-12",
		Title:      "Make the CLI smaller",
	}, Workspace{Path: t.TempDir()})
	require.NoError(t, err)

	assert.True(t, result.ExistingPullRequest)
	assert.False(t, result.DraftDueToFailedVerification)
}

func TestGitHubPublisher_BlocksPublishWhenRequiredVerificationFailsWithoutDraftFallback(t *testing.T) {
	t.Parallel()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || r.URL.Path != githubPullsPath {
			t.Errorf("unexpected GitHub request after failed gate: %s %s", r.Method, r.URL.String())
			http.NotFound(w, r)
			return
		}

		writeTestResponse(t, w, `[]`)
	}))
	defer server.Close()

	var commands []string
	cfg := publisherTestConfig(server.URL)
	cfg.Publish.DraftOnFailedValidation = false
	publisher := &githubPublisher{
		cfg:             cfg,
		client:          NewGitHubClient(cfg.Tracker),
		runGit:          successfulPublishGitRunner(t, server.URL, &commands),
		runVerification: verificationRunnerFor(failingVerificationReport()),
		logger:          loggerOrDefault(nil),
	}

	result, err := publisher.Publish(t.Context(), Issue{
		ID:         "node",
		Identifier: "GH-12",
		Title:      "Make the CLI smaller",
	}, Workspace{Path: t.TempDir()})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "required verification gate(s) failed: unit")
	require.NotNil(t, result)
	require.NotNil(t, result.Verification)
	assert.False(t, result.Published)
	assert.False(t, result.Verification.Passed)
	assert.Equal(t, []string{"unit"}, result.Verification.FailedRequired)
	assert.NotContains(t, commands, gitPushBranch)
}

func TestGitHubPublisher_ReturnsPartialResultWhenVerificationRunnerErrors(t *testing.T) {
	t.Parallel()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || r.URL.Path != githubPullsPath {
			t.Errorf("unexpected GitHub request after verification error: %s %s", r.Method, r.URL.String())
			http.NotFound(w, r)
			return
		}

		writeTestResponse(t, w, `[]`)
	}))
	defer server.Close()

	var commands []string
	cfg := publisherTestConfig(server.URL)
	report := failingVerificationReport()
	publisher := &githubPublisher{
		cfg:    cfg,
		client: NewGitHubClient(cfg.Tracker),
		runGit: successfulPublishGitRunner(t, server.URL, &commands),
		runVerification: func(context.Context, Config, Issue, Workspace) (VerificationReport, error) {
			return report, context.Canceled
		},
		logger: loggerOrDefault(nil),
	}

	result, err := publisher.Publish(t.Context(), Issue{
		ID:         "node",
		Identifier: "GH-12",
		Title:      "Make the CLI smaller",
	}, Workspace{Path: t.TempDir()})
	require.ErrorIs(t, err, context.Canceled)
	require.NotNil(t, result)
	require.NotNil(t, result.Verification)
	assert.False(t, result.Published)
	assert.Equal(t, []string{"unit"}, result.Verification.FailedRequired)
	assert.NotContains(t, commands, gitPushBranch)
}

func TestGitHubPublisher_ReturnsPartialResultWhenPullRequestCreateFails(t *testing.T) {
	t.Parallel()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == githubPullsPath:
			writeTestResponse(t, w, `[]`)
		case r.Method == http.MethodPost && r.URL.Path == githubPullsPath:
			http.Error(w, "pull request service unavailable", http.StatusInternalServerError)
		default:
			t.Errorf("unexpected GitHub request after pull request create failure: %s %s", r.Method, r.URL.String())
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	var commands []string
	cfg := publisherTestConfig(server.URL)
	publisher := &githubPublisher{
		cfg:             cfg,
		client:          NewGitHubClient(cfg.Tracker),
		runGit:          successfulPublishGitRunner(t, server.URL, &commands),
		runVerification: verificationRunnerFor(passingVerificationReport()),
		logger:          loggerOrDefault(nil),
	}

	result, err := publisher.Publish(t.Context(), Issue{
		ID:         "node",
		Identifier: "GH-12",
		Title:      "Make the CLI smaller",
	}, Workspace{Path: t.TempDir()})
	require.Error(t, err)
	require.NotNil(t, result)
	require.NotNil(t, result.Verification)
	assert.False(t, result.Published)
	assert.Equal(t, "abc123", result.CommitSHA)
	assert.True(t, result.Verification.Passed)
	assert.Contains(t, commands, gitPushBranch)
}

func TestGitHubPublisher_ReturnsPartialFailureDraftResultWhenPullRequestCreateFails(t *testing.T) {
	t.Parallel()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == githubPullsPath:
			writeTestResponse(t, w, `[]`)
		case r.Method == http.MethodPost && r.URL.Path == githubPullsPath:
			http.Error(w, "pull request service unavailable", http.StatusInternalServerError)
		default:
			t.Errorf("unexpected GitHub request after failure draft create failure: %s %s", r.Method, r.URL.String())
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	var commands []string
	runner := func(_ context.Context, _ string, env []string, _ shell.AuditContext, args ...string) ([]byte, error) {
		joined := strings.Join(args, " ")
		commands = append(commands, joined)

		switch {
		case joined == gitCheckoutBranch:
			return nil, nil
		case joined == gitStatusPorcelain:
			return nil, nil
		case joined == gitRevListMainHead:
			return []byte("0\n"), nil
		case joined == gitConfigUserName:
			return nil, nil
		case joined == gitConfigUserEmail:
			return nil, nil
		case joined == gitAddAll:
			return nil, nil
		case strings.HasPrefix(joined, "commit --allow-empty -F "):
			return []byte("[symphony/GH-12 fail123] failed draft\n"), nil
		case joined == gitRevParseHead:
			return []byte("fail123\n"), nil
		case joined == gitRemoteGetOrigin:
			return []byte("/local/origin\n"), nil
		case joined == "remote set-url origin "+server.URL+"/owner/repo.git":
			return nil, nil
		case joined == gitPushBranch:
			assert.Contains(t, env, "GITHUB_TOKEN=token")
			return nil, nil
		default:
			t.Fatalf("unexpected git command: %s", joined)
			return nil, nil
		}
	}

	cfg := publisherTestConfig(server.URL)
	publisher := &githubPublisher{
		cfg:    cfg,
		client: NewGitHubClient(cfg.Tracker),
		runGit: runner,
		logger: loggerOrDefault(nil),
	}

	result, err := publisher.PublishFailureDraft(t.Context(), Issue{
		ID:         "node",
		Identifier: "GH-12",
		Title:      "Make the CLI smaller",
	}, Workspace{Path: t.TempDir()}, errors.New("api_key=worker-secret"))
	require.Error(t, err)
	require.NotNil(t, result)
	require.NotNil(t, result.Verification)
	assert.False(t, result.Published)
	assert.True(t, result.DraftDueToRunFailure)
	assert.Equal(t, "fail123", result.CommitSHA)
	assert.Equal(t, []string{"worker_run"}, result.Verification.FailedRequired)
	assert.NotContains(t, result.FailureReason, "worker-secret")
	assert.Contains(t, commands, gitPushBranch)
}

func TestGitHubPublisher_OpensFailureDraftPullRequestWithEmptyCommit(t *testing.T) {
	t.Parallel()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == githubPullsPath:
			writeTestResponse(t, w, `[]`)
		case r.Method == http.MethodPost && r.URL.Path == githubPullsPath:
			var body map[string]any
			if !assert.NoError(t, json.NewDecoder(r.Body).Decode(&body)) {
				http.Error(w, "decode pull request body", http.StatusBadRequest)
				return
			}

			assert.Equal(t, true, body["draft"])
			assert.Contains(t, body["body"], "FAIL `worker_run`")
			assert.Contains(t, body["body"], "empty draft commit")
			assert.Contains(t, body["body"], "This PR is a draft")
			assert.NotContains(t, body["body"], "worker-secret")
			w.WriteHeader(http.StatusCreated)
			writeTestResponse(t, w, `{"number":7,"draft":true,"html_url":"https://github.com/owner/repo/pull/7"}`)
		case r.Method == http.MethodDelete && r.URL.Path == githubIssueLabelPath:
			w.WriteHeader(http.StatusOK)
			writeTestResponse(t, w, `[]`)
		case r.Method == http.MethodPost && r.URL.Path == githubIssueCommentsPath:
			var body map[string]string
			if !assert.NoError(t, json.NewDecoder(r.Body).Decode(&body)) {
				http.Error(w, "decode issue comment body", http.StatusBadRequest)
				return
			}

			assert.Contains(t, body["body"], "draft pull request #7 after the implementation could not complete before verification")
			assert.Contains(t, body["body"], "draft PR documents the failure")
			w.WriteHeader(http.StatusCreated)
			writeTestResponse(t, w, `{}`)
		default:
			t.Errorf("unexpected GitHub request: %s %s", r.Method, r.URL.String())
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	var commands []string
	runner := func(_ context.Context, _ string, env []string, _ shell.AuditContext, args ...string) ([]byte, error) {
		joined := strings.Join(args, " ")
		commands = append(commands, joined)

		switch {
		case joined == gitCheckoutBranch:
			return nil, nil
		case joined == gitStatusPorcelain:
			return nil, nil
		case joined == gitRevListMainHead:
			return []byte("0\n"), nil
		case joined == gitConfigUserName:
			return nil, nil
		case joined == gitConfigUserEmail:
			return nil, nil
		case joined == gitAddAll:
			return nil, nil
		case strings.HasPrefix(joined, "commit --allow-empty -F "):
			return []byte("[symphony/GH-12 fail123] failed draft\n"), nil
		case joined == gitRevParseHead:
			return []byte("fail123\n"), nil
		case joined == gitRemoteGetOrigin:
			return []byte("/local/origin\n"), nil
		case joined == "remote set-url origin "+server.URL+"/owner/repo.git":
			return nil, nil
		case joined == gitPushBranch:
			assert.Contains(t, env, "GITHUB_TOKEN=token")
			return nil, nil
		default:
			t.Fatalf("unexpected git command: %s", joined)
			return nil, nil
		}
	}

	cfg := publisherTestConfig(server.URL)
	publisher := &githubPublisher{
		cfg:    cfg,
		client: NewGitHubClient(cfg.Tracker),
		runGit: runner,
		logger: loggerOrDefault(nil),
	}

	result, err := publisher.PublishFailureDraft(t.Context(), Issue{
		ID:         "node",
		Identifier: "GH-12",
		Title:      "Make the CLI smaller",
	}, Workspace{Path: t.TempDir()}, errors.New("api_key=worker-secret"))
	require.NoError(t, err)

	require.NotNil(t, result.Verification)
	assert.False(t, result.Verification.Passed)
	assert.Equal(t, []string{"worker_run"}, result.Verification.FailedRequired)
	assert.True(t, result.DraftDueToRunFailure)
	assert.Equal(t, "fail123", result.CommitSHA)
	assert.Equal(t, 7, result.PullRequestNumber)
	assert.True(t, hasCommandPrefix(commands, "commit --allow-empty -F "))
	assert.Contains(t, commands, gitPushBranch)
	assert.NotContains(t, result.FailureReason, "worker-secret")
}

func TestGitHubPublisher_UpdatesExistingFailureDraftWithoutEmptyCommit(t *testing.T) {
	t.Parallel()

	var converted atomic.Bool
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == githubPullsPath:
			writeTestResponse(t, w, `[{"number":7,"node_id":"PR_node","draft":false,"html_url":"https://github.com/owner/repo/pull/7","head":{"ref":"symphony/GH-12","sha":"abc123"}}]`)
		case r.Method == http.MethodPatch && r.URL.Path == githubPullsPath+"/7":
			var body map[string]any
			if !assert.NoError(t, json.NewDecoder(r.Body).Decode(&body)) {
				http.Error(w, "decode pull request body", http.StatusBadRequest)
				return
			}

			bodyText, ok := body["body"].(string)
			if !assert.True(t, ok, "pull request body must be a string") {
				http.Error(w, "pull request body must be a string", http.StatusBadRequest)
				return
			}

			assert.Contains(t, bodyText, "FAIL `worker_run`")
			assert.Contains(t, bodyText, "No changed files were captured before the incomplete run ended.")
			assert.Contains(t, bodyText, "Related to #12")
			assert.NotContains(t, bodyText, "empty draft commit")
			assert.NotContains(t, bodyText, "worker-secret")
			writeTestResponse(t, w, `{"number":7,"node_id":"PR_node","draft":false,"html_url":"https://github.com/owner/repo/pull/7"}`)
		case r.Method == http.MethodPost && r.URL.Path == githubGraphQLPath:
			var body struct {
				Query string `json:"query"`
			}
			if !assert.NoError(t, json.NewDecoder(r.Body).Decode(&body)) {
				http.Error(w, "decode graphql body", http.StatusBadRequest)
				return
			}

			assert.Contains(t, body.Query, "convertPullRequestToDraft")
			converted.Store(true)
			writeTestResponse(t, w, `{"data":{"convertPullRequestToDraft":{"pullRequest":{"id":"PR_node","isDraft":true}}}}`)
		case r.Method == http.MethodDelete && r.URL.Path == githubIssueLabelPath:
			w.WriteHeader(http.StatusOK)
			writeTestResponse(t, w, `[]`)
		case r.Method == http.MethodPost && r.URL.Path == githubIssueCommentsPath:
			var body map[string]string
			if !assert.NoError(t, json.NewDecoder(r.Body).Decode(&body)) {
				http.Error(w, "decode issue comment body", http.StatusBadRequest)
				return
			}

			assert.Contains(t, body["body"], "draft pull request #7 after the implementation could not complete before verification")
			assert.Contains(t, body["body"], "draft PR documents the failure")
			w.WriteHeader(http.StatusCreated)
			writeTestResponse(t, w, `{}`)
		default:
			t.Errorf("unexpected GitHub request: %s %s", r.Method, r.URL.String())
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	var commands []string
	runner := func(_ context.Context, _ string, _ []string, _ shell.AuditContext, args ...string) ([]byte, error) {
		joined := strings.Join(args, " ")
		commands = append(commands, joined)

		switch joined {
		case gitCheckoutBranch:
			return nil, nil
		case gitStatusPorcelain:
			return nil, nil
		case gitRevListMainHead:
			return []byte("0\n"), nil
		default:
			t.Fatalf("unexpected git command: %s", joined)
			return nil, nil
		}
	}

	cfg := publisherTestConfig(server.URL)
	publisher := &githubPublisher{
		cfg:    cfg,
		client: NewGitHubClient(cfg.Tracker),
		runGit: runner,
		logger: loggerOrDefault(nil),
	}

	result, err := publisher.PublishFailureDraft(t.Context(), Issue{
		ID:         "node",
		Identifier: "GH-12",
		Title:      "Make the CLI smaller",
	}, Workspace{Path: t.TempDir()}, errors.New("api_key=worker-secret"))
	require.NoError(t, err)

	require.NotNil(t, result.Verification)
	assert.False(t, result.Verification.Passed)
	assert.Equal(t, []string{"worker_run"}, result.Verification.FailedRequired)
	assert.True(t, result.DraftDueToRunFailure)
	assert.True(t, result.ExistingPullRequest)
	assert.Empty(t, result.CommitSHA)
	assert.Equal(t, 7, result.PullRequestNumber)
	assert.True(t, converted.Load())
	assert.False(t, hasCommandPrefix(commands, "commit --allow-empty -F "))
	assert.NotContains(t, commands, gitPushBranch)
	assert.NotContains(t, result.FailureReason, "worker-secret")
}

func TestPublishWorkspaceBlocksBelowHighAutonomy(t *testing.T) {
	t.Parallel()

	_, err := PublishWorkspace(t.Context(), Config{
		Autonomy: autonomy.Medium,
		Tracker:  TrackerConfig{Kind: trackerKindGitHub},
		Publish:  PublishConfig{Enabled: true},
	}, Issue{ID: "node", Identifier: "GH-12"}, Workspace{Path: t.TempDir()}, nil)

	require.Error(t, err)
	assert.Contains(t, err.Error(), "autonomy medium blocks branch creation")
}

func TestPublishWorkspaceFullAutonomyRequiresCheckMonitoring(t *testing.T) {
	t.Parallel()

	_, err := PublishWorkspace(t.Context(), Config{
		Autonomy: autonomy.Full,
		Tracker:  TrackerConfig{Kind: trackerKindGitHub},
		Publish:  PublishConfig{Enabled: true},
	}, Issue{ID: "node", Identifier: "GH-12"}, Workspace{Path: t.TempDir()}, nil)

	require.Error(t, err)
	assert.Contains(t, err.Error(), "publish: autonomy full with publish.enabled requires publish.monitor_checks: true")
	assert.Contains(t, err.Error(), "use autonomy high")
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
		case gitCheckoutRemoteBranch:
			return nil, nil
		case gitStatusPorcelain:
			return nil, nil
		case "rev-parse --verify --quiet refs/heads/symphony/GH-12":
			return []byte("def456\n"), nil
		case "rev-list origin/symphony/GH-12..symphony/GH-12":
			return nil, nil
		case "rebase origin/main":
			return nil, nil
		case gitRevParseHead:
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
		Autonomy: autonomy.High,
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
	assert.Less(
		t,
		slices.Index(commands, gitStatusPorcelain),
		slices.Index(commands, "checkout -B symphony/GH-12 origin/symphony/GH-12"),
		"dirty-workspace check must run before the destructive checkout -B",
	)
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
		case gitCheckoutRemoteBranch:
			return nil, nil
		case "rebase origin/main":
			return nil, errors.New("conflict")
		default:
			t.Fatalf("unexpected git command: %s", joined)
			return nil, nil
		}
	}

	cfg := Config{
		Autonomy: autonomy.High,
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

	assert.Contains(t, commands, gitCheckoutRemoteBranch)
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
		case gitCheckoutBranch:
			return nil, nil
		case gitStatusPorcelain:
			return nil, nil
		case gitRevListMainHead:
			return []byte("0\n"), nil
		default:
			t.Fatalf("unexpected git command: %s", joined)
			return nil, nil
		}
	}

	cfg := Config{
		Autonomy: autonomy.High,
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
	assert.NotContains(t, commands, gitPushBranch)
}

func TestGitHubPublisher_SkipsCleanWorkspaceWithExistingPullRequest(t *testing.T) {
	t.Parallel()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || r.URL.Path != githubPullsPath {
			t.Errorf("unexpected GitHub request: %s %s", r.Method, r.URL.String())
			http.NotFound(w, r)
			return
		}

		writeTestResponse(t, w, `[{"number":7,"html_url":"https://github.com/owner/repo/pull/7","state":"open","head":{"ref":"symphony/GH-12","sha":"abc123"}}]`)
	}))
	defer server.Close()

	var commands []string
	runner := func(_ context.Context, _ string, _ []string, _ shell.AuditContext, args ...string) ([]byte, error) {
		joined := strings.Join(args, " ")
		commands = append(commands, joined)

		switch joined {
		case gitCheckoutBranch:
			return nil, nil
		case gitStatusPorcelain:
			return nil, nil
		case gitRevListMainHead:
			return []byte("0\n"), nil
		default:
			t.Fatalf("unexpected git command: %s", joined)
			return nil, nil
		}
	}

	cfg := publisherTestConfig(server.URL)
	publisher := &githubPublisher{
		cfg:             cfg,
		client:          NewGitHubClient(cfg.Tracker),
		runGit:          runner,
		runVerification: verificationRunnerFor(passingVerificationReport()),
		logger:          loggerOrDefault(nil),
	}

	result, err := publisher.Publish(t.Context(), Issue{
		ID:         "node",
		Identifier: "GH-12",
		Title:      "Make the CLI smaller",
	}, Workspace{Path: t.TempDir()})
	require.NoError(t, err)

	assert.False(t, result.Published)
	assert.True(t, result.ExistingPullRequest)
	assert.Equal(t, "workspace has no changes to publish", result.SkippedReason)
	assert.Nil(t, result.Verification)
	assert.NotContains(t, commands, gitPushBranch)
}

func TestGitHubPublisher_BranchUpdateSkipsAndPreservesUnpushedCommits(t *testing.T) {
	t.Parallel()

	workDir, remoteDir := initBranchUpdateGitRepos(t)

	writeTestFile(t, filepath.Join(workDir, "unpushed.txt"), "committed but not pushed\n")
	runTestGit(t, workDir, "add", "-A")
	runTestGit(t, workDir, "commit", "-m", "worker commit that failed to push")
	unpushedSHA := runTestGit(t, workDir, "rev-parse", "refs/heads/symphony/GH-12")

	publisher := newBranchUpdateTestPublisher(t, remoteDir)

	_, err := publisher.updatePullRequestBranch(t.Context(), workDir, "symphony/GH-12")
	require.ErrorIs(t, err, errPullRequestBranchUpdateSkipped)

	assert.Equal(
		t,
		unpushedSHA,
		runTestGit(t, workDir, "rev-parse", "refs/heads/symphony/GH-12"),
		"committed-but-unpushed worker commit must survive a skipped branch update",
	)
	assert.FileExists(t, filepath.Join(workDir, "unpushed.txt"))
}

func TestGitHubPublisher_BranchUpdateRefusesDirtyWorkspaceBeforeResetting(t *testing.T) {
	t.Parallel()

	workDir, remoteDir := initBranchUpdateGitRepos(t)

	writeTestFile(t, filepath.Join(workDir, "unpushed.txt"), "committed but not pushed\n")
	runTestGit(t, workDir, "add", "-A")
	runTestGit(t, workDir, "commit", "-m", "worker commit that failed to push")
	unpushedSHA := runTestGit(t, workDir, "rev-parse", "refs/heads/symphony/GH-12")

	// An untracked file makes the workspace dirty without blocking checkout -B,
	// so a reset performed before the guard would still "succeed" silently.
	writeTestFile(t, filepath.Join(workDir, "dirty.txt"), "uncommitted worker edit\n")

	publisher := newBranchUpdateTestPublisher(t, remoteDir)

	_, err := publisher.updatePullRequestBranch(t.Context(), workDir, "symphony/GH-12")
	require.ErrorContains(t, err, "uncommitted changes")

	assert.Equal(
		t,
		unpushedSHA,
		runTestGit(t, workDir, "rev-parse", "refs/heads/symphony/GH-12"),
		"dirty-workspace refusal must happen before the branch is reset",
	)
	assert.FileExists(t, filepath.Join(workDir, "dirty.txt"))
}

// initBranchUpdateGitRepos creates a bare "origin" repository plus a workspace
// clone with main and symphony/GH-12 pushed, mirroring a worker workspace.
func initBranchUpdateGitRepos(t *testing.T) (workDir, remoteDir string) {
	t.Helper()

	remoteDir = t.TempDir()
	runTestGit(t, remoteDir, "init", "--bare", "--initial-branch=main", ".")

	workDir = t.TempDir()
	runTestGit(t, workDir, "init", "--initial-branch=main", ".")
	runTestGit(t, workDir, "config", "user.name", "Symphony Test")
	runTestGit(t, workDir, "config", "user.email", "symphony-test@example.com")
	runTestGit(t, workDir, "remote", "add", "origin", remoteDir)

	writeTestFile(t, filepath.Join(workDir, "main.txt"), "base\n")
	runTestGit(t, workDir, "add", "-A")
	runTestGit(t, workDir, "commit", "-m", "base commit")
	runTestGit(t, workDir, "push", "origin", "main")

	runTestGit(t, workDir, "checkout", "-b", "symphony/GH-12")
	writeTestFile(t, filepath.Join(workDir, "feature.txt"), "feature\n")
	runTestGit(t, workDir, "add", "-A")
	runTestGit(t, workDir, "commit", "-m", "feature commit")
	runTestGit(t, workDir, "push", "origin", "symphony/GH-12")

	return workDir, remoteDir
}

func newBranchUpdateTestPublisher(t *testing.T, remoteDir string) *githubPublisher {
	t.Helper()

	cfg := Config{
		Tracker: TrackerConfig{
			Kind:   trackerKindGitHub,
			APIKey: "token",
			Owner:  "owner",
			Repo:   "repo",
		},
		Publish: PublishConfig{
			Remote:     "origin",
			RemoteURL:  remoteDir,
			BaseBranch: "main",
		},
	}

	return &githubPublisher{
		cfg:    cfg,
		client: NewGitHubClient(cfg.Tracker),
		runGit: defaultGitCommandRunner,
		logger: loggerOrDefault(nil),
		audit: shell.AuditContext{
			Caller:   "symphony.git",
			AuditDir: t.TempDir(),
		},
	}
}

func runTestGit(t *testing.T, dir string, args ...string) string {
	t.Helper()

	cmd := exec.CommandContext(t.Context(), "git", args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(),
		"GIT_CONFIG_GLOBAL=/dev/null",
		"GIT_CONFIG_SYSTEM=/dev/null",
		"GIT_TERMINAL_PROMPT=0",
	)
	output, err := cmd.CombinedOutput()
	require.NoError(t, err, "git %s: %s", strings.Join(args, " "), output)

	return strings.TrimSpace(string(output))
}

func writeTestFile(t *testing.T, path, content string) {
	t.Helper()

	require.NoError(t, os.WriteFile(path, []byte(content), 0o600))
}

func writeTestResponse(t *testing.T, w http.ResponseWriter, body string) {
	t.Helper()

	_, err := w.Write([]byte(body))
	assert.NoError(t, err)
}

func hasCommandPrefix(commands []string, prefix string) bool {
	for _, command := range commands {
		if strings.HasPrefix(command, prefix) {
			return true
		}
	}

	return false
}
