//nolint:gocritic,govet,wsl_v5,modernize,misspell // Tracker adapters mirror third-party payloads and state names.
package symphony

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"
)

// NewTrackerClient builds the tracker adapter selected by config.
func NewTrackerClient(cfg Config) (TrackerClient, error) {
	switch normalizeState(cfg.Tracker.Kind) {
	case trackerKindLinear:
		return NewLinearClient(cfg.Tracker), nil
	case trackerKindGitHub:
		return NewGitHubClient(cfg.Tracker), nil
	default:
		return nil, fmt.Errorf("unsupported_tracker_kind: %s", cfg.Tracker.Kind)
	}
}

// LinearClient is a Linear GraphQL tracker adapter.
type LinearClient struct {
	httpClient *http.Client
	cfg        TrackerConfig
}

// NewLinearClient creates a Linear GraphQL client.
func NewLinearClient(cfg TrackerConfig) *LinearClient {
	return &LinearClient{
		cfg: cfg,
		httpClient: &http.Client{
			Timeout: defaultLinearHTTPTimeout,
		},
	}
}

// FetchCandidateIssues fetches active Linear issues for the configured project.
func (c *LinearClient) FetchCandidateIssues(ctx context.Context) ([]Issue, error) {
	return c.fetchIssues(ctx, c.cfg.ActiveStates, linearCandidateIssuesQuery)
}

// FetchIssuesByStates fetches Linear issues in the provided states.
func (c *LinearClient) FetchIssuesByStates(ctx context.Context, states []string) ([]Issue, error) {
	return c.fetchIssues(ctx, states, linearCandidateIssuesQuery)
}

// FetchIssueStatesByIDs refreshes Linear issues by GraphQL issue ID.
func (c *LinearClient) FetchIssueStatesByIDs(ctx context.Context, ids []string) ([]Issue, error) {
	if len(ids) == 0 {
		return nil, nil
	}

	var response linearIssuesResponse
	if err := c.graphql(ctx, linearIssuesByIDsQuery, map[string]any{"ids": ids}, &response); err != nil {
		return nil, err
	}

	return normalizeLinearIssueNodes(response.Data.Issues.Nodes), nil
}

func (c *LinearClient) fetchIssues(ctx context.Context, states []string, query string) ([]Issue, error) {
	states = trimNonEmptyStrings(states)
	if len(states) == 0 {
		return nil, nil
	}

	var issues []Issue
	var after *string

	for {
		var response linearIssuesResponse
		variables := map[string]any{
			"projectSlug": c.cfg.ProjectSlug,
			"stateNames":  states,
			"first":       defaultLinearPageSize,
			"after":       after,
		}

		if err := c.graphql(ctx, query, variables, &response); err != nil {
			return nil, err
		}

		issues = append(issues, normalizeLinearIssueNodes(response.Data.Issues.Nodes)...)
		if !response.Data.Issues.PageInfo.HasNextPage {
			break
		}

		if response.Data.Issues.PageInfo.EndCursor == nil || *response.Data.Issues.PageInfo.EndCursor == "" {
			return nil, errors.New("linear_missing_end_cursor")
		}

		after = response.Data.Issues.PageInfo.EndCursor
	}

	return issues, nil
}

func (c *LinearClient) graphql(ctx context.Context, query string, variables map[string]any, out any) error {
	body, err := json.Marshal(map[string]any{
		"query":     query,
		"variables": variables,
	})
	if err != nil {
		return fmt.Errorf("linear_api_request: marshal request: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.cfg.Endpoint, bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("linear_api_request: build request: %w", err)
	}

	req.Header.Set("Authorization", c.cfg.APIKey)
	req.Header.Set("Content-Type", "application/json")

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("linear_api_request: %w", err)
	}
	defer resp.Body.Close()

	data, err := io.ReadAll(io.LimitReader(resp.Body, 8*1024*1024))
	if err != nil {
		return fmt.Errorf("linear_api_request: read response: %w", err)
	}

	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("linear_api_status: HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(data)))
	}

	var envelope struct {
		Errors []map[string]any `json:"errors"`
	}
	if err := json.Unmarshal(data, &envelope); err != nil {
		return fmt.Errorf("linear_unknown_payload: %w", err)
	}

	if len(envelope.Errors) > 0 {
		return fmt.Errorf("linear_graphql_errors: %s", strings.TrimSpace(string(data)))
	}

	if err := json.Unmarshal(data, out); err != nil {
		return fmt.Errorf("linear_unknown_payload: %w", err)
	}

	return nil
}

const linearIssueFields = `
nodes {
  id
  identifier
  title
  description
  priority
  branchName
  url
  createdAt
  updatedAt
  state { name }
  labels { nodes { name } }
  inverseRelations { nodes { type relatedIssue { id identifier state { name } } } }
}`

const linearCandidateIssuesQuery = `
query SymphonyCandidateIssues($projectSlug: String!, $stateNames: [String!], $first: Int!, $after: String) {
  issues(
    first: $first,
    after: $after,
    filter: {
      project: { slugId: { eq: $projectSlug } },
      state: { name: { in: $stateNames } }
    }
  ) {
    ` + linearIssueFields + `
    pageInfo { hasNextPage endCursor }
  }
}`

const linearIssuesByIDsQuery = `
query SymphonyIssueStates($ids: [ID!]!) {
  issues(filter: { id: { in: $ids } }) {
    ` + linearIssueFields + `
    pageInfo { hasNextPage endCursor }
  }
}`

type linearIssuesResponse struct {
	Data struct {
		Issues struct {
			Nodes    []linearIssueNode `json:"nodes"`
			PageInfo struct {
				EndCursor   *string `json:"endCursor"`
				HasNextPage bool    `json:"hasNextPage"`
			} `json:"pageInfo"`
		} `json:"issues"`
	} `json:"data"`
}

type linearIssueNode struct {
	Description *string `json:"description"`
	BranchName  *string `json:"branchName"`
	URL         *string `json:"url"`
	ID          string  `json:"id"`
	Identifier  string  `json:"identifier"`
	Title       string  `json:"title"`
	Priority    any     `json:"priority"`
	CreatedAt   string  `json:"createdAt"`
	UpdatedAt   string  `json:"updatedAt"`
	State       struct {
		Name string `json:"name"`
	} `json:"state"`
	Labels struct {
		Nodes []struct {
			Name string `json:"name"`
		} `json:"nodes"`
	} `json:"labels"`
	InverseRelations struct {
		Nodes []struct {
			Type         string `json:"type"`
			RelatedIssue struct {
				ID         *string `json:"id"`
				Identifier *string `json:"identifier"`
				State      *struct {
					Name string `json:"name"`
				} `json:"state"`
			} `json:"relatedIssue"`
		} `json:"nodes"`
	} `json:"inverseRelations"`
}

func normalizeLinearIssueNodes(nodes []linearIssueNode) []Issue {
	issues := make([]Issue, 0, len(nodes))
	for _, node := range nodes {
		issue := Issue{
			ID:          node.ID,
			Identifier:  node.Identifier,
			Title:       node.Title,
			Description: node.Description,
			Priority:    parsePriority(node.Priority),
			State:       node.State.Name,
			BranchName:  node.BranchName,
			URL:         node.URL,
			CreatedAt:   parseTimePointer(node.CreatedAt),
			UpdatedAt:   parseTimePointer(node.UpdatedAt),
		}

		for _, label := range node.Labels.Nodes {
			if label.Name != "" {
				issue.Labels = append(issue.Labels, strings.ToLower(label.Name))
			}
		}

		for _, relation := range node.InverseRelations.Nodes {
			if normalizeState(relation.Type) != "blocks" {
				continue
			}

			blocker := BlockerRef{
				ID:         relation.RelatedIssue.ID,
				Identifier: relation.RelatedIssue.Identifier,
			}
			if relation.RelatedIssue.State != nil {
				state := relation.RelatedIssue.State.Name
				blocker.State = &state
			}

			issue.BlockedBy = append(issue.BlockedBy, blocker)
		}

		issues = append(issues, issue)
	}

	return issues
}

// GitHubClient is a GitHub Issues REST tracker adapter. It is an Atteler
// extension over the Linear-only Symphony draft tracker.
type GitHubClient struct {
	httpClient *http.Client
	cfg        TrackerConfig
}

// NewGitHubClient creates a GitHub Issues client.
func NewGitHubClient(cfg TrackerConfig) *GitHubClient {
	return &GitHubClient{
		cfg: cfg,
		httpClient: &http.Client{
			Timeout: defaultLinearHTTPTimeout,
		},
	}
}

// FetchCandidateIssues fetches GitHub issues in configured active states.
func (c *GitHubClient) FetchCandidateIssues(ctx context.Context) ([]Issue, error) {
	return c.fetchByStates(ctx, c.cfg.ActiveStates)
}

// FetchIssuesByStates fetches GitHub issues in the requested states.
func (c *GitHubClient) FetchIssuesByStates(ctx context.Context, states []string) ([]Issue, error) {
	return c.fetchByStates(ctx, states)
}

// FetchIssueStatesByIDs refreshes GitHub issues by node ID. Identifiers in the
// GH-123 fallback shape are also accepted for testability and recovery.
func (c *GitHubClient) FetchIssueStatesByIDs(ctx context.Context, ids []string) ([]Issue, error) {
	if len(ids) == 0 {
		return nil, nil
	}

	want := make(map[string]struct{}, len(ids))
	numbers := make([]int, 0, len(ids))
	for _, id := range ids {
		id = strings.TrimSpace(id)
		if id == "" {
			continue
		}

		want[id] = struct{}{}
		if strings.HasPrefix(id, "GH-") {
			if number, err := strconv.Atoi(strings.TrimPrefix(id, "GH-")); err == nil {
				numbers = append(numbers, number)
			}
		}
	}

	var issues []Issue
	for _, number := range numbers {
		issue, err := c.fetchOne(ctx, number)
		if err != nil {
			return nil, err
		}

		issues = append(issues, issue)
		delete(want, issue.ID)
	}

	if len(want) > 0 {
		all, err := c.fetchByStates(ctx, []string{"OPEN", "CLOSED"})
		if err != nil {
			return nil, err
		}

		for _, issue := range all {
			if _, ok := want[issue.ID]; ok {
				issues = append(issues, issue)
			}
		}
	}

	return issues, nil
}

func (c *GitHubClient) fetchByStates(ctx context.Context, states []string) ([]Issue, error) {
	states = trimNonEmptyStrings(states)
	if len(states) == 0 {
		return nil, nil
	}

	var issues []Issue
	for _, state := range githubRESTStates(states) {
		next, err := c.fetchList(ctx, state)
		if err != nil {
			return nil, err
		}

		issues = append(issues, next...)
	}

	return uniqueIssues(issues), nil
}

func (c *GitHubClient) fetchList(ctx context.Context, state string) ([]Issue, error) {
	var all []Issue
	for page := 1; ; page++ {
		values := url.Values{}
		values.Set("state", state)
		values.Set("per_page", "100")
		values.Set("page", strconv.Itoa(page))
		values.Set("sort", "created")
		values.Set("direction", "asc")
		if len(c.cfg.Labels) > 0 {
			values.Set("labels", strings.Join(c.cfg.Labels, ","))
		}

		var payload []githubIssue
		if err := c.get(ctx, "/repos/"+url.PathEscape(c.cfg.Owner)+"/"+url.PathEscape(c.cfg.Repo)+"/issues?"+values.Encode(), &payload); err != nil {
			return nil, err
		}

		if len(payload) == 0 {
			break
		}

		for _, issue := range payload {
			if issue.PullRequest != nil {
				continue
			}

			all = append(all, normalizeGitHubIssue(issue))
		}

		if len(payload) < 100 {
			break
		}
	}

	return all, nil
}

func (c *GitHubClient) fetchOne(ctx context.Context, number int) (Issue, error) {
	var payload githubIssue
	if err := c.get(ctx, fmt.Sprintf("/repos/%s/%s/issues/%d", url.PathEscape(c.cfg.Owner), url.PathEscape(c.cfg.Repo), number), &payload); err != nil {
		return Issue{}, err
	}

	return normalizeGitHubIssue(payload), nil
}

func (c *GitHubClient) get(ctx context.Context, path string, out any) error {
	return c.do(ctx, http.MethodGet, path, nil, out)
}

func (c *GitHubClient) post(ctx context.Context, path string, in any, out any) error {
	return c.do(ctx, http.MethodPost, path, in, out)
}

func (c *GitHubClient) delete(ctx context.Context, path string, out any) error {
	return c.do(ctx, http.MethodDelete, path, nil, out)
}

func (c *GitHubClient) do(ctx context.Context, method, path string, in any, out any) error {
	endpoint := strings.TrimRight(c.cfg.Endpoint, "/") + path
	var body io.Reader
	if in != nil {
		data, err := json.Marshal(in)
		if err != nil {
			return fmt.Errorf("github_api_request: marshal request: %w", err)
		}

		body = bytes.NewReader(data)
	}

	req, err := http.NewRequestWithContext(ctx, method, endpoint, body)
	if err != nil {
		return fmt.Errorf("github_api_request: build request: %w", err)
	}

	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("Authorization", "Bearer "+c.cfg.APIKey)
	req.Header.Set("X-GitHub-Api-Version", "2026-03-10")
	if in != nil {
		req.Header.Set("Content-Type", "application/json")
	}

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("github_api_request: %w", err)
	}
	defer resp.Body.Close()

	data, err := io.ReadAll(io.LimitReader(resp.Body, 8*1024*1024))
	if err != nil {
		return fmt.Errorf("github_api_request: read response: %w", err)
	}

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return &githubAPIStatusError{
			StatusCode: resp.StatusCode,
			Body:       strings.TrimSpace(string(data)),
		}
	}

	if out == nil || len(data) == 0 {
		return nil
	}

	if err := json.Unmarshal(data, out); err != nil {
		return fmt.Errorf("github_unknown_payload: %w", err)
	}

	return nil
}

// FetchOpenPullRequestByHead returns the open PR for owner:branch, if any.
func (c *GitHubClient) FetchOpenPullRequestByHead(ctx context.Context, branch string) (*GitHubPullRequest, error) {
	branch = strings.TrimSpace(branch)
	if branch == "" {
		return nil, errors.New("github pull request head branch is required")
	}

	values := url.Values{}
	values.Set("state", "open")
	values.Set("head", c.cfg.Owner+":"+branch)

	var payload []GitHubPullRequest
	if err := c.get(ctx, "/repos/"+url.PathEscape(c.cfg.Owner)+"/"+url.PathEscape(c.cfg.Repo)+"/pulls?"+values.Encode(), &payload); err != nil {
		return nil, err
	}

	if len(payload) == 0 {
		return nil, nil
	}

	return &payload[0], nil
}

// FetchOpenPullRequestsByHeadPrefix returns open PRs whose head branch follows
// the Symphony branch prefix convention, along with the source issue inferred
// from that branch.
func (c *GitHubClient) FetchOpenPullRequestsByHeadPrefix(ctx context.Context, branchPrefix string) ([]MonitoredPullRequest, error) {
	prefix := strings.Trim(strings.TrimSpace(branchPrefix), "/")
	if prefix == "" {
		return nil, nil
	}

	var out []MonitoredPullRequest
	for page := 1; ; page++ {
		values := url.Values{}
		values.Set("state", "open")
		values.Set("per_page", "100")
		values.Set("page", strconv.Itoa(page))

		var payload []GitHubPullRequest
		if err := c.get(ctx, "/repos/"+url.PathEscape(c.cfg.Owner)+"/"+url.PathEscape(c.cfg.Repo)+"/pulls?"+values.Encode(), &payload); err != nil {
			return nil, err
		}

		if len(payload) == 0 {
			break
		}

		for _, pr := range payload {
			branch := strings.TrimSpace(pr.Head.Ref)
			identifier, ok := strings.CutPrefix(branch, prefix+"/")
			if !ok || strings.TrimSpace(identifier) == "" {
				continue
			}

			number, err := githubIssueNumber(Issue{Identifier: identifier})
			if err != nil {
				continue
			}

			issue, err := c.fetchOne(ctx, number)
			if err != nil {
				return nil, err
			}

			out = append(out, MonitoredPullRequest{
				Issue:       issue,
				PullRequest: pr,
				Branch:      branch,
			})
		}

		if len(payload) < 100 {
			break
		}
	}

	return out, nil
}

// CreatePullRequest opens a PR from branch into base.
func (c *GitHubClient) CreatePullRequest(ctx context.Context, branch, base, title, body string, draft bool) (GitHubPullRequest, error) {
	var payload GitHubPullRequest
	err := c.post(ctx, "/repos/"+url.PathEscape(c.cfg.Owner)+"/"+url.PathEscape(c.cfg.Repo)+"/pulls", map[string]any{
		"title": title,
		"head":  branch,
		"base":  base,
		"body":  body,
		"draft": draft,
	}, &payload)
	if err != nil {
		return GitHubPullRequest{}, err
	}

	return payload, nil
}

// RemoveIssueLabel removes a label from an issue. Missing labels are already
// absent from the dispatch filter, so GitHub 404 responses are treated as a
// successful no-op.
func (c *GitHubClient) RemoveIssueLabel(ctx context.Context, issueNumber int, label string) error {
	label = strings.TrimSpace(label)
	if issueNumber <= 0 || label == "" {
		return nil
	}

	err := c.delete(ctx, fmt.Sprintf(
		"/repos/%s/%s/issues/%d/labels/%s",
		url.PathEscape(c.cfg.Owner),
		url.PathEscape(c.cfg.Repo),
		issueNumber,
		url.PathEscape(label),
	), nil)
	if err == nil {
		return nil
	}

	var statusErr *githubAPIStatusError
	if errors.As(err, &statusErr) && statusErr.StatusCode == http.StatusNotFound {
		return nil
	}

	return err
}

// AddIssueComment writes a best-effort audit trail back to the source issue.
func (c *GitHubClient) AddIssueComment(ctx context.Context, issueNumber int, body string) error {
	body = strings.TrimSpace(body)
	if issueNumber <= 0 || body == "" {
		return nil
	}

	return c.post(ctx, fmt.Sprintf(
		"/repos/%s/%s/issues/%d/comments",
		url.PathEscape(c.cfg.Owner),
		url.PathEscape(c.cfg.Repo),
		issueNumber,
	), map[string]string{"body": body}, nil)
}

// FetchPullRequest returns the current GitHub PR payload needed by Symphony.
func (c *GitHubClient) FetchPullRequest(ctx context.Context, number int) (GitHubPullRequest, error) {
	if number <= 0 {
		return GitHubPullRequest{}, errors.New("github pull request number is required")
	}

	var payload GitHubPullRequest
	err := c.get(ctx, fmt.Sprintf(
		"/repos/%s/%s/pulls/%d",
		url.PathEscape(c.cfg.Owner),
		url.PathEscape(c.cfg.Repo),
		number,
	), &payload)
	if err != nil {
		return GitHubPullRequest{}, err
	}

	return payload, nil
}

// FetchPullRequestChecks returns a normalized snapshot of the current check
// runs and legacy commit statuses for a PR head commit.
func (c *GitHubClient) FetchPullRequestChecks(ctx context.Context, number int) (PullRequestCheckSnapshot, error) {
	pr, err := c.FetchPullRequest(ctx, number)
	if err != nil {
		return PullRequestCheckSnapshot{}, err
	}

	snapshot := PullRequestCheckSnapshot{
		CheckedAt:         time.Now().UTC(),
		PullRequestNumber: pr.Number,
		PullRequestURL:    pr.HTMLURL,
		HeadRef:           pr.Head.Ref,
		HeadSHA:           pr.Head.SHA,
		PullRequestClosed: strings.EqualFold(pr.State, "closed"),
	}
	if snapshot.PullRequestClosed {
		snapshot.State = PullRequestChecksPassed
		snapshot.Summary = "pull request is closed; no rework will be scheduled"
		return snapshot, nil
	}

	if strings.TrimSpace(pr.Head.SHA) == "" {
		snapshot.State = PullRequestChecksPending
		snapshot.Summary = "pull request head SHA is not available yet"
		return snapshot, nil
	}

	var checks githubCheckRunsResponse
	if err := c.get(ctx, fmt.Sprintf(
		"/repos/%s/%s/commits/%s/check-runs",
		url.PathEscape(c.cfg.Owner),
		url.PathEscape(c.cfg.Repo),
		url.PathEscape(pr.Head.SHA),
	), &checks); err != nil {
		return PullRequestCheckSnapshot{}, err
	}

	for _, run := range checks.CheckRuns {
		snapshot.CheckRuns = append(snapshot.CheckRuns, PullRequestCheckRun{
			Name:       run.Name,
			Status:     run.Status,
			Conclusion: run.Conclusion,
			DetailsURL: firstNonEmpty(run.DetailsURL, run.HTMLURL),
			HTMLURL:    run.HTMLURL,
		})
	}

	var statuses githubStatusesResponse
	if err := c.get(ctx, fmt.Sprintf(
		"/repos/%s/%s/commits/%s/status",
		url.PathEscape(c.cfg.Owner),
		url.PathEscape(c.cfg.Repo),
		url.PathEscape(pr.Head.SHA),
	), &statuses); err != nil {
		return PullRequestCheckSnapshot{}, err
	}

	for _, status := range statuses.Statuses {
		snapshot.StatusContexts = append(snapshot.StatusContexts, PullRequestStatus(status))
	}

	classifyPullRequestChecks(&snapshot)
	return snapshot, nil
}

func classifyPullRequestChecks(snapshot *PullRequestCheckSnapshot) {
	if snapshot == nil {
		return
	}

	total := len(snapshot.CheckRuns) + len(snapshot.StatusContexts)
	if total == 0 {
		snapshot.State = PullRequestChecksPending
		snapshot.Summary = "no check runs or status contexts have been reported yet"
		return
	}

	var pending []string
	for _, run := range snapshot.CheckRuns {
		name := firstNonEmpty(run.Name, "unnamed check run")
		switch {
		case !strings.EqualFold(run.Status, "completed"):
			pending = append(pending, name)
		case isFailingCheckConclusion(run.Conclusion):
			snapshot.FailedCheckNames = append(snapshot.FailedCheckNames, name)
		case strings.TrimSpace(run.Conclusion) == "":
			pending = append(pending, name)
		}
	}

	for _, status := range snapshot.StatusContexts {
		name := firstNonEmpty(status.Context, "unnamed status context")
		switch normalizeState(status.State) {
		case "success":
		case "failure", "error":
			snapshot.FailedCheckNames = append(snapshot.FailedCheckNames, name)
		default:
			pending = append(pending, name)
		}
	}

	switch {
	case len(snapshot.FailedCheckNames) > 0:
		snapshot.State = PullRequestChecksFailed
		snapshot.Summary = "failing checks: " + strings.Join(snapshot.FailedCheckNames, ", ")
	case len(pending) > 0:
		snapshot.State = PullRequestChecksPending
		snapshot.Summary = "pending checks: " + strings.Join(pending, ", ")
	default:
		snapshot.State = PullRequestChecksPassed
		snapshot.Summary = "all reported checks have passed"
	}
}

func isFailingCheckConclusion(conclusion string) bool {
	switch normalizeState(conclusion) {
	case "failure", "cancelled", "canceled", "timed_out", "action_required", "startup_failure", "stale":
		return true
	default:
		return false
	}
}

type githubAPIStatusError struct {
	Body       string
	StatusCode int
}

func (e *githubAPIStatusError) Error() string {
	if e == nil {
		return "github_api_status"
	}

	if e.Body == "" {
		return fmt.Sprintf("github_api_status: HTTP %d", e.StatusCode)
	}

	return fmt.Sprintf("github_api_status: HTTP %d: %s", e.StatusCode, e.Body)
}

type githubIssue struct {
	PullRequest *struct{}     `json:"pull_request"`
	Body        *string       `json:"body"`
	HTMLURL     *string       `json:"html_url"`
	NodeID      string        `json:"node_id"`
	Title       string        `json:"title"`
	State       string        `json:"state"`
	CreatedAt   string        `json:"created_at"`
	UpdatedAt   string        `json:"updated_at"`
	Number      int           `json:"number"`
	Labels      []githubLabel `json:"labels"`
}

type githubLabel struct {
	Name string `json:"name"`
}

// GitHubPullRequest is the subset of GitHub's PR payload needed by Symphony.
type GitHubPullRequest struct {
	Body    *string               `json:"body,omitempty"`
	Head    githubPullRequestHead `json:"head"`
	HTMLURL string                `json:"html_url"`
	State   string                `json:"state"`
	Title   string                `json:"title,omitempty"`
	Number  int                   `json:"number"`
}

type githubPullRequestHead struct {
	Ref string `json:"ref"`
	SHA string `json:"sha"`
}

type githubCheckRunsResponse struct {
	CheckRuns []githubCheckRun `json:"check_runs"`
}

type githubCheckRun struct {
	Name       string `json:"name"`
	Status     string `json:"status"`
	Conclusion string `json:"conclusion"`
	DetailsURL string `json:"details_url"`
	HTMLURL    string `json:"html_url"`
}

type githubStatusesResponse struct {
	State    string                `json:"state"`
	Statuses []githubStatusContext `json:"statuses"`
}

type githubStatusContext struct {
	Context     string `json:"context"`
	State       string `json:"state"`
	Description string `json:"description"`
	TargetURL   string `json:"target_url"`
}

func normalizeGitHubIssue(issue githubIssue) Issue {
	state := strings.ToUpper(issue.State)
	identifier := fmt.Sprintf("GH-%d", issue.Number)
	priority := githubPriority(issue.Labels)

	out := Issue{
		ID:          firstNonEmpty(issue.NodeID, identifier),
		Identifier:  identifier,
		Title:       issue.Title,
		Description: issue.Body,
		Priority:    priority,
		State:       state,
		URL:         issue.HTMLURL,
		CreatedAt:   parseTimePointer(issue.CreatedAt),
		UpdatedAt:   parseTimePointer(issue.UpdatedAt),
	}

	for _, label := range issue.Labels {
		if label.Name != "" {
			out.Labels = append(out.Labels, strings.ToLower(label.Name))
		}
	}

	sort.Strings(out.Labels)

	return out
}

func githubRESTStates(states []string) []string {
	seen := map[string]struct{}{}
	for _, state := range states {
		switch normalizeState(state) {
		case "open", "todo", "in progress", "active":
			seen["open"] = struct{}{}
		case "closed", "done", "cancelled", "canceled", "duplicate":
			seen["closed"] = struct{}{}
		default:
			seen[strings.ToLower(strings.TrimSpace(state))] = struct{}{}
		}
	}

	out := make([]string, 0, len(seen))
	for state := range seen {
		out = append(out, state)
	}

	sort.Strings(out)

	return out
}

func githubPriority(labels []githubLabel) *int {
	for _, label := range labels {
		name := normalizeState(label.Name)
		for prefix, priority := range map[string]int{
			"p0": 0,
			"p1": 1,
			"p2": 2,
			"p3": 3,
			"p4": 4,
		} {
			if name == prefix || strings.HasPrefix(name, prefix+":") || strings.HasPrefix(name, "priority:"+prefix) {
				value := priority
				return &value
			}
		}
	}

	return nil
}

func uniqueIssues(issues []Issue) []Issue {
	seen := make(map[string]struct{}, len(issues))
	out := make([]Issue, 0, len(issues))
	for _, issue := range issues {
		key := firstNonEmpty(issue.ID, issue.Identifier)
		if key == "" {
			continue
		}

		if _, ok := seen[key]; ok {
			continue
		}

		seen[key] = struct{}{}
		out = append(out, issue)
	}

	return out
}

func parsePriority(value any) *int {
	switch typed := value.(type) {
	case nil:
		return nil
	case int:
		return &typed
	case float64:
		if typed != float64(int(typed)) {
			return nil
		}

		parsed := int(typed)
		return &parsed
	case json.Number:
		parsed, err := typed.Int64()
		if err != nil {
			return nil
		}

		value := int(parsed)
		return &value
	default:
		return nil
	}
}

func parseTimePointer(value string) *time.Time {
	if strings.TrimSpace(value) == "" {
		return nil
	}

	parsed, err := time.Parse(time.RFC3339, value)
	if err != nil {
		return nil
	}

	return &parsed
}
