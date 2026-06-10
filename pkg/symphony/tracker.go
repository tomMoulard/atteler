//nolint:gocritic,govet,wsl_v5,misspell // Tracker adapters mirror third-party payloads and state names.
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
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/tommoulard/atteler/pkg/watch"
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
	if err := requireTrackerContext(ctx); err != nil {
		return err
	}

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

const githubConvertPullRequestToDraftMutation = `
mutation SymphonyConvertPullRequestToDraft($pullRequestId: ID!) {
  convertPullRequestToDraft(input: { pullRequestId: $pullRequestId }) {
    pullRequest { id isDraft }
  }
}`

const githubMarkPullRequestReadyForReviewMutation = `
mutation SymphonyMarkPullRequestReadyForReview($pullRequestId: ID!) {
  markPullRequestReadyForReview(input: { pullRequestId: $pullRequestId }) {
    pullRequest { id isDraft }
  }
}`

const (
	// GitHub rejects made-up REST API version headers with HTTP 400, so keep
	// this pinned to a documented supported version.
	githubAPIVersion  = "2022-11-28"
	githubGraphQLPath = "/graphql"
)

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

var _ watch.IssueTracker = (*GitHubClient)(nil)

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
	seenNumbers := make(map[int]struct{}, len(ids))
	for _, id := range ids {
		id = strings.TrimSpace(id)
		if id == "" {
			continue
		}

		if number, ok := githubIssueNumberReference(id); ok {
			if _, seen := seenNumbers[number]; !seen {
				numbers = append(numbers, number)
				seenNumbers[number] = struct{}{}
			}

			continue
		}

		want[id] = struct{}{}
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

func githubIssueNumberReference(ref string) (int, bool) {
	ref = strings.TrimSpace(ref)
	if ref == "" {
		return 0, false
	}

	if parsedURL, err := url.Parse(ref); err == nil && parsedURL.Scheme != "" && parsedURL.Host != "" {
		parts := strings.Split(strings.Trim(parsedURL.Path, "/"), "/")
		for i := 0; i+1 < len(parts); i++ {
			if !strings.EqualFold(parts[i], "issues") {
				continue
			}

			parsed, err := strconv.Atoi(parts[i+1])
			if err == nil && parsed > 0 {
				return parsed, true
			}
		}

		return 0, false
	}

	if number, ok := strings.CutPrefix(ref, "#"); ok {
		ref = number
	} else if number, ok := strings.CutPrefix(strings.ToUpper(ref), "GH-"); ok {
		ref = number
	}

	parsed, err := strconv.Atoi(ref)
	if err != nil || parsed <= 0 {
		return 0, false
	}

	return parsed, true
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

			normalized, err := c.normalizeGitHubIssueWithComments(ctx, issue)
			if err != nil {
				return nil, err
			}

			all = append(all, normalized)
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
	if payload.PullRequest != nil {
		return Issue{}, fmt.Errorf("github issue %d is a pull request, not an issue", number)
	}

	return c.normalizeGitHubIssueWithComments(ctx, payload)
}

func (c *GitHubClient) normalizeGitHubIssueWithComments(ctx context.Context, payload githubIssue) (Issue, error) {
	issue := normalizeGitHubIssue(payload)
	if payload.Number <= 0 {
		return issue, nil
	}

	comments, err := c.fetchIssueComments(ctx, payload.Number)
	if err != nil {
		return Issue{}, err
	}
	issue.Comments = comments

	return issue, nil
}

func (c *GitHubClient) fetchIssueComments(ctx context.Context, number int) ([]IssueComment, error) {
	var comments []IssueComment
	for page := 1; ; page++ {
		values := url.Values{}
		values.Set("per_page", "100")
		values.Set("page", strconv.Itoa(page))

		var payload []githubIssueComment
		path := fmt.Sprintf(
			"/repos/%s/%s/issues/%d/comments?%s",
			url.PathEscape(c.cfg.Owner),
			url.PathEscape(c.cfg.Repo),
			number,
			values.Encode(),
		)
		if err := c.get(ctx, path, &payload); err != nil {
			return nil, err
		}

		for _, comment := range payload {
			comments = append(comments, normalizeGitHubIssueComment(comment))
		}

		if len(payload) < 100 {
			break
		}
	}

	return comments, nil
}

func (c *GitHubClient) get(ctx context.Context, path string, out any) error {
	return c.do(ctx, http.MethodGet, path, nil, out)
}

func (c *GitHubClient) post(ctx context.Context, path string, in any, out any) error {
	return c.do(ctx, http.MethodPost, path, in, out)
}

func (c *GitHubClient) patch(ctx context.Context, path string, in any, out any) error {
	return c.do(ctx, http.MethodPatch, path, in, out)
}

func (c *GitHubClient) graphql(ctx context.Context, query string, variables map[string]any, out any) error {
	if err := requireTrackerContext(ctx); err != nil {
		return err
	}

	body, err := json.Marshal(map[string]any{
		"query":     query,
		"variables": variables,
	})
	if err != nil {
		return fmt.Errorf("github_graphql_request: marshal request: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, githubGraphQLEndpoint(c.cfg.Endpoint), bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("github_graphql_request: build request: %w", err)
	}

	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("Authorization", "Bearer "+c.cfg.APIKey)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-GitHub-Api-Version", githubAPIVersion)

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("github_graphql_request: %w", err)
	}
	defer resp.Body.Close()

	data, err := io.ReadAll(io.LimitReader(resp.Body, 8*1024*1024))
	if err != nil {
		return fmt.Errorf("github_graphql_request: read response: %w", err)
	}

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return &githubAPIStatusError{
			StatusCode: resp.StatusCode,
			Body:       strings.TrimSpace(string(data)),
		}
	}

	var envelope struct {
		Errors []map[string]any `json:"errors"`
	}
	if err := json.Unmarshal(data, &envelope); err != nil {
		return fmt.Errorf("github_graphql_unknown_payload: %w", err)
	}

	if len(envelope.Errors) > 0 {
		return fmt.Errorf("github_graphql_errors: %s", strings.TrimSpace(string(data)))
	}

	if out == nil || len(data) == 0 {
		return nil
	}

	if err := json.Unmarshal(data, out); err != nil {
		return fmt.Errorf("github_graphql_unknown_payload: %w", err)
	}

	return nil
}

func githubGraphQLEndpoint(endpoint string) string {
	endpoint = strings.TrimSpace(endpoint)
	if endpoint == "" {
		endpoint = defaultGitHubEndpoint
	}

	parsed, err := url.Parse(endpoint)
	if err != nil || parsed.Scheme == "" || parsed.Host == "" {
		return strings.TrimRight(endpoint, "/") + "/graphql"
	}

	parsed.RawQuery = ""
	parsed.Fragment = ""

	path := strings.TrimRight(parsed.Path, "/")
	switch {
	case path == githubGraphQLPath ||
		strings.HasSuffix(path, "/api/graphql") ||
		strings.HasSuffix(path, githubGraphQLPath):
		parsed.Path = path
	case parsed.Host == "api.github.com" && path == "":
		parsed.Path = githubGraphQLPath
	case strings.HasSuffix(path, "/api/v3"):
		parsed.Path = strings.TrimSuffix(path, "/api/v3") + "/api/graphql"
	case path == "":
		parsed.Path = githubGraphQLPath
	default:
		parsed.Path = path + githubGraphQLPath
	}

	return parsed.String()
}

func (c *GitHubClient) delete(ctx context.Context, path string, out any) error {
	return c.do(ctx, http.MethodDelete, path, nil, out)
}

func (c *GitHubClient) do(ctx context.Context, method, path string, in any, out any) error {
	if err := requireTrackerContext(ctx); err != nil {
		return err
	}

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
	req.Header.Set("X-GitHub-Api-Version", githubAPIVersion)
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

func requireTrackerContext(ctx context.Context) error {
	if ctx == nil {
		return errors.New("tracker: context is required")
	}

	if err := ctx.Err(); err != nil {
		return fmt.Errorf("tracker: context already done: %w", err)
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

// UpdatePullRequest refreshes the title/body report for an existing PR.
func (c *GitHubClient) UpdatePullRequest(ctx context.Context, number int, title, body string) (GitHubPullRequest, error) {
	if number <= 0 {
		return GitHubPullRequest{}, errors.New("github pull request number is required")
	}

	var payload GitHubPullRequest
	err := c.patch(ctx, fmt.Sprintf(
		"/repos/%s/%s/pulls/%d",
		url.PathEscape(c.cfg.Owner),
		url.PathEscape(c.cfg.Repo),
		number,
	), map[string]any{
		"title": title,
		"body":  body,
	}, &payload)
	if err != nil {
		return GitHubPullRequest{}, err
	}

	return payload, nil
}

// ConvertPullRequestToDraft marks an existing PR as draft. GitHub exposes this
// transition through GraphQL, so callers need the REST node_id from the pull
// request payload.
func (c *GitHubClient) ConvertPullRequestToDraft(ctx context.Context, nodeID string) error {
	nodeID = strings.TrimSpace(nodeID)
	if nodeID == "" {
		return errors.New("github pull request node_id is required")
	}

	var response struct {
		Data struct {
			ConvertPullRequestToDraft struct {
				PullRequest struct {
					ID      string `json:"id"`
					IsDraft bool   `json:"isDraft"`
				} `json:"pullRequest"`
			} `json:"convertPullRequestToDraft"`
		} `json:"data"`
	}

	if err := c.graphql(ctx, githubConvertPullRequestToDraftMutation, map[string]any{"pullRequestId": nodeID}, &response); err != nil {
		return err
	}
	if !response.Data.ConvertPullRequestToDraft.PullRequest.IsDraft {
		return errors.New("github pull request was not converted to draft")
	}

	return nil
}

// MarkPullRequestReadyForReview marks an existing draft PR ready for review.
// GitHub exposes this transition through GraphQL, so callers need the REST
// node_id from the pull request payload.
func (c *GitHubClient) MarkPullRequestReadyForReview(ctx context.Context, nodeID string) error {
	nodeID = strings.TrimSpace(nodeID)
	if nodeID == "" {
		return errors.New("github pull request node_id is required")
	}

	var response struct {
		Data struct {
			MarkPullRequestReadyForReview struct {
				PullRequest struct {
					ID      string `json:"id"`
					IsDraft bool   `json:"isDraft"`
				} `json:"pullRequest"`
			} `json:"markPullRequestReadyForReview"`
		} `json:"data"`
	}

	if err := c.graphql(ctx, githubMarkPullRequestReadyForReviewMutation, map[string]any{"pullRequestId": nodeID}, &response); err != nil {
		return err
	}
	if response.Data.MarkPullRequestReadyForReview.PullRequest.IsDraft {
		return errors.New("github pull request was not marked ready for review")
	}

	return nil
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

// FindIssueByFingerprint locates an existing Atteler watch issue by the hidden
// fingerprint marker embedded in the issue body.
func (c *GitHubClient) FindIssueByFingerprint(ctx context.Context, fingerprint string) (*watch.IssueRef, error) {
	fingerprint = strings.TrimSpace(fingerprint)
	if fingerprint == "" {
		return nil, errors.New("watch issue fingerprint is required")
	}

	marker := watch.IssueFingerprintMarker(fingerprint)
	for page := 1; ; page++ {
		values := url.Values{}
		values.Set("state", "all")
		values.Set("per_page", "100")
		values.Set("page", strconv.Itoa(page))

		var payload []githubIssue
		if err := c.get(ctx, "/repos/"+url.PathEscape(c.cfg.Owner)+"/"+url.PathEscape(c.cfg.Repo)+"/issues?"+values.Encode(), &payload); err != nil {
			return nil, err
		}

		for _, issue := range payload {
			if issue.PullRequest != nil || issue.Body == nil || !strings.Contains(*issue.Body, marker) {
				continue
			}

			ref := watchIssueRef(issue, fingerprint)
			return &ref, nil
		}

		if len(payload) < 100 {
			return nil, nil
		}
	}
}

// CreateIssue opens a GitHub issue for an actionable Atteler watch finding.
func (c *GitHubClient) CreateIssue(ctx context.Context, draft watch.IssueDraft) (watch.IssueRef, error) {
	var payload githubIssue
	err := c.post(ctx, c.repositoryPath("issues"), watchIssueMutationPayload(draft), &payload)
	if err != nil {
		return watch.IssueRef{}, err
	}

	return watchIssueRef(payload, draft.Fingerprint), nil
}

// UpdateIssue refreshes an existing GitHub issue for an Atteler watch finding.
func (c *GitHubClient) UpdateIssue(ctx context.Context, ref watch.IssueRef, draft watch.IssueDraft) (watch.IssueRef, error) {
	if ref.Number <= 0 {
		return watch.IssueRef{}, errors.New("watch issue number is required")
	}

	var payload githubIssue
	mutation := watchIssueMutationPayload(draft)
	if strings.EqualFold(ref.State, "closed") {
		mutation["state"] = "open"
	}

	err := c.patch(ctx, c.repositoryPath(fmt.Sprintf("issues/%d", ref.Number)), mutation, &payload)
	if err != nil {
		return watch.IssueRef{}, err
	}

	return watchIssueRef(payload, draft.Fingerprint), nil
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
	return c.FetchPullRequestChecksWithPolicy(ctx, number, legacyPullRequestCheckPolicy())
}

// FetchPullRequestChecksWithPolicy returns a normalized snapshot of the current
// check runs and legacy commit statuses for a PR head commit using the supplied
// required-check policy.
func (c *GitHubClient) FetchPullRequestChecksWithPolicy(ctx context.Context, number int, policy PullRequestCheckPolicy) (PullRequestCheckSnapshot, error) {
	pr, err := c.FetchPullRequest(ctx, number)
	if err != nil {
		return PullRequestCheckSnapshot{}, err
	}

	policy = normalizePullRequestCheckPolicy(policy)
	snapshot := PullRequestCheckSnapshot{
		CheckedAt:         time.Now().UTC(),
		PullRequestNumber: pr.Number,
		PullRequestURL:    pr.HTMLURL,
		HeadRef:           pr.Head.Ref,
		HeadSHA:           pr.Head.SHA,
		BaseRef:           pr.Base.Ref,
		BaseSHA:           pr.Base.SHA,
		MergeableState:    pr.MergeableState,
		PullRequestClosed: strings.EqualFold(pr.State, "closed"),
		NoChecksPolicy:    string(policy.NoChecksPolicy),
	}
	if snapshot.PullRequestClosed {
		snapshot.State = PullRequestChecksPassed
		snapshot.Summary = "pull request is closed; no rework will be scheduled"
		return snapshot, nil
	}

	if reason := pullRequestBranchUpdateReason(pr); reason != "" {
		snapshot.NeedsBranchUpdate = true
		snapshot.BranchUpdateReason = reason
		snapshot.State = PullRequestChecksPending
		snapshot.Summary = reason
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
		snapshot.StatusContexts = append(snapshot.StatusContexts, PullRequestStatus{
			Context:     status.Context,
			State:       status.State,
			Description: status.Description,
			TargetURL:   status.TargetURL,
		})
	}

	if policy.DiscoverRequiredChecks {
		discovery := c.fetchGitHubRequiredChecks(ctx, pr.Base.Ref)
		policy.BranchProtectionCheckNames = append(policy.BranchProtectionCheckNames, discovery.branchProtectionChecks...)
		policy.RulesetCheckNames = append(policy.RulesetCheckNames, discovery.rulesetChecks...)
		if discovery.branchProtectionErr != nil {
			snapshot.BranchProtectionError = discovery.branchProtectionErr.Error()
		}
		if discovery.rulesetErr != nil {
			snapshot.RulesetError = discovery.rulesetErr.Error()
		}
	}

	classifyPullRequestChecks(&snapshot, policy)
	return snapshot, nil
}

func classifyPullRequestChecks(snapshot *PullRequestCheckSnapshot, policy PullRequestCheckPolicy) {
	if snapshot == nil {
		return
	}

	resetPullRequestCheckClassification(snapshot)
	policy = normalizePullRequestCheckPolicy(policy)
	matcher := newRequiredCheckMatcher(policy)
	snapshot.RequiredCheckNames = sortedStrings(matcher.exactNames())
	snapshot.RequiredCheckPatterns = sortedStrings(matcher.patterns())
	snapshot.RequirementSource = requirementSourceSummary(matcher.sources(), policy.TreatAllReportedAsRequired)
	snapshot.NoChecksPolicy = string(policy.NoChecksPolicy)

	total := len(snapshot.CheckRuns) + len(snapshot.StatusContexts)
	if total == 0 {
		classifyNoReportedPullRequestChecks(snapshot, matcher)
		return
	}

	evidence := collectPullRequestCheckEvidence(snapshot, matcher)
	snapshot.MissingRequiredCheckNames = matcher.missingRequired(evidence.observedRequired)
	snapshot.OptionalFailedCheckNames = sortedStrings(evidence.optionalFailures)
	snapshot.PendingOptionalCheckNames = sortedStrings(evidence.optionalPending)
	snapshot.RequiredFailedCheckNames = sortedStrings(snapshot.RequiredFailedCheckNames)
	snapshot.PendingRequiredCheckNames = sortedStrings(snapshot.PendingRequiredCheckNames)

	switch {
	case len(snapshot.RequiredFailedCheckNames) > 0:
		snapshot.FailedCheckNames = append([]string(nil), snapshot.RequiredFailedCheckNames...)
		snapshot.State = PullRequestChecksFailed
		snapshot.Summary = "failing required checks: " + strings.Join(snapshot.RequiredFailedCheckNames, ", ")
	case len(snapshot.PendingRequiredCheckNames) > 0 || len(snapshot.MissingRequiredCheckNames) > 0:
		snapshot.State = PullRequestChecksPending
		pending := append([]string(nil), snapshot.PendingRequiredCheckNames...)
		pending = append(pending, snapshot.MissingRequiredCheckNames...)
		snapshot.Summary = "pending required checks: " + strings.Join(sortedStrings(pending), ", ")
	case policy.ReworkOptionalChecks && len(snapshot.OptionalFailedCheckNames) > 0:
		snapshot.FailedCheckNames = append([]string(nil), snapshot.OptionalFailedCheckNames...)
		snapshot.State = PullRequestChecksFailed
		snapshot.Summary = "failing optional checks configured for rework: " + strings.Join(snapshot.OptionalFailedCheckNames, ", ")
	case !matcher.hasRequiredChecks():
		classifyNoRequiredPullRequestChecks(snapshot)
	case len(snapshot.OptionalFailedCheckNames) > 0:
		snapshot.State = PullRequestChecksPassed
		snapshot.Summary = "required checks passed; optional failing checks: " + strings.Join(snapshot.OptionalFailedCheckNames, ", ")
	case len(snapshot.PendingOptionalCheckNames) > 0:
		snapshot.State = PullRequestChecksPassed
		snapshot.Summary = "required checks passed; optional checks still pending: " + strings.Join(snapshot.PendingOptionalCheckNames, ", ")
	default:
		snapshot.State = PullRequestChecksPassed
		snapshot.Summary = "all required checks have passed"
	}
}

func resetPullRequestCheckClassification(snapshot *PullRequestCheckSnapshot) {
	snapshot.RequirementSource = ""
	snapshot.NoChecksPolicy = ""
	snapshot.RequiredCheckNames = nil
	snapshot.RequiredCheckPatterns = nil
	snapshot.FailedCheckNames = nil
	snapshot.RequiredFailedCheckNames = nil
	snapshot.OptionalFailedCheckNames = nil
	snapshot.PendingRequiredCheckNames = nil
	snapshot.MissingRequiredCheckNames = nil
	snapshot.PendingOptionalCheckNames = nil
}

type pullRequestCheckEvidence struct {
	observedRequired map[string]struct{}
	optionalFailures []string
	optionalPending  []string
}

func collectPullRequestCheckEvidence(snapshot *PullRequestCheckSnapshot, matcher requiredCheckMatcher) pullRequestCheckEvidence {
	evidence := pullRequestCheckEvidence{observedRequired: map[string]struct{}{}}

	for i := range snapshot.CheckRuns {
		evidence.addCheckRun(snapshot, &snapshot.CheckRuns[i], matcher)
	}

	for i := range snapshot.StatusContexts {
		evidence.addStatusContext(snapshot, &snapshot.StatusContexts[i], matcher)
	}

	return evidence
}

func (e *pullRequestCheckEvidence) addCheckRun(snapshot *PullRequestCheckSnapshot, run *PullRequestCheckRun, matcher requiredCheckMatcher) {
	name := firstNonEmpty(run.Name, "unnamed check run")
	required, source := matcher.match(name)
	run.Required = required
	run.RequirementSource = source
	if required {
		e.observedRequired[name] = struct{}{}
	}

	state := classifyCheckRunState(*run)
	switch {
	case required && state == pullRequestObservedCheckFailed:
		snapshot.RequiredFailedCheckNames = append(snapshot.RequiredFailedCheckNames, name)
	case required && state == pullRequestObservedCheckPending:
		snapshot.PendingRequiredCheckNames = append(snapshot.PendingRequiredCheckNames, name)
	case state == pullRequestObservedCheckFailed:
		e.optionalFailures = append(e.optionalFailures, name)
	case state == pullRequestObservedCheckPending:
		e.optionalPending = append(e.optionalPending, name)
	}
}

func (e *pullRequestCheckEvidence) addStatusContext(snapshot *PullRequestCheckSnapshot, status *PullRequestStatus, matcher requiredCheckMatcher) {
	name := firstNonEmpty(status.Context, "unnamed status context")
	required, source := matcher.match(name)
	status.Required = required
	status.RequirementSource = source
	if required {
		e.observedRequired[name] = struct{}{}
	}

	switch normalizeState(status.State) {
	case "success":
	case "failure", "error":
		if required {
			snapshot.RequiredFailedCheckNames = append(snapshot.RequiredFailedCheckNames, name)
			return
		}

		e.optionalFailures = append(e.optionalFailures, name)
	default:
		if required {
			snapshot.PendingRequiredCheckNames = append(snapshot.PendingRequiredCheckNames, name)
			return
		}

		e.optionalPending = append(e.optionalPending, name)
	}
}

func classifyNoReportedPullRequestChecks(snapshot *PullRequestCheckSnapshot, matcher requiredCheckMatcher) {
	if matcher.hasNamedRequiredChecks() {
		snapshot.MissingRequiredCheckNames = matcher.missingRequired(nil)
		snapshot.State = PullRequestChecksPending
		snapshot.Summary = "pending required checks: " + strings.Join(snapshot.MissingRequiredCheckNames, ", ")
		return
	}

	switch PullRequestNoChecksPolicy(snapshot.NoChecksPolicy) {
	case PullRequestNoChecksFail:
		snapshot.State = PullRequestChecksFailed
		snapshot.Summary = "no check runs or status contexts were reported and no-check policy is fail"
		snapshot.FailedCheckNames = []string{"no reported checks"}
	case PullRequestNoChecksPending:
		snapshot.State = PullRequestChecksPending
		snapshot.Summary = "no check runs or status contexts have been reported yet"
	default:
		snapshot.State = PullRequestChecksPassed
		snapshot.Summary = "no required checks configured or discovered; no-check policy is pass"
	}
}

func classifyNoRequiredPullRequestChecks(snapshot *PullRequestCheckSnapshot) {
	switch PullRequestNoChecksPolicy(snapshot.NoChecksPolicy) {
	case PullRequestNoChecksFail:
		snapshot.State = PullRequestChecksFailed
		snapshot.Summary = "reported checks are optional and no-check policy is fail"
		snapshot.FailedCheckNames = []string{"no required checks"}
	case PullRequestNoChecksPending:
		snapshot.State = PullRequestChecksPending
		snapshot.Summary = "reported checks are optional and no-check policy is pending"
	case PullRequestNoChecksPass:
		snapshot.State = PullRequestChecksPassed
		switch {
		case len(snapshot.OptionalFailedCheckNames) > 0:
			snapshot.Summary = "no required checks configured or discovered; optional failing checks: " + strings.Join(snapshot.OptionalFailedCheckNames, ", ")
		case len(snapshot.PendingOptionalCheckNames) > 0:
			snapshot.Summary = "no required checks configured or discovered; optional checks still pending: " + strings.Join(snapshot.PendingOptionalCheckNames, ", ")
		default:
			snapshot.Summary = "no required checks configured or discovered; reported checks are optional"
		}
	default:
		snapshot.State = PullRequestChecksPassed
		snapshot.Summary = "no required checks configured or discovered; reported checks are optional"
	}
}

type pullRequestObservedCheckState string

const (
	pullRequestObservedCheckPassed  pullRequestObservedCheckState = "passed"
	pullRequestObservedCheckPending pullRequestObservedCheckState = "pending"
	pullRequestObservedCheckFailed  pullRequestObservedCheckState = "failed"
)

func classifyCheckRunState(run PullRequestCheckRun) pullRequestObservedCheckState {
	if !strings.EqualFold(run.Status, "completed") {
		return pullRequestObservedCheckPending
	}

	switch normalizeState(run.Conclusion) {
	case "success", "neutral", "skipped":
		return pullRequestObservedCheckPassed
	case "failure", "cancelled", "canceled", "timed_out", "action_required", "startup_failure", "stale":
		return pullRequestObservedCheckFailed
	case "":
		return pullRequestObservedCheckPending
	default:
		return pullRequestObservedCheckFailed
	}
}

func legacyPullRequestCheckPolicy() PullRequestCheckPolicy {
	return PullRequestCheckPolicy{
		NoChecksPolicy:             PullRequestNoChecksPending,
		TreatAllReportedAsRequired: true,
	}
}

func normalizePullRequestCheckPolicy(policy PullRequestCheckPolicy) PullRequestCheckPolicy {
	policy.RequiredCheckNames = trimNonEmptyStrings(policy.RequiredCheckNames)
	policy.RequiredCheckPatterns = trimNonEmptyStrings(policy.RequiredCheckPatterns)
	policy.BranchProtectionCheckNames = trimNonEmptyStrings(policy.BranchProtectionCheckNames)
	policy.RulesetCheckNames = trimNonEmptyStrings(policy.RulesetCheckNames)
	if policy.NoChecksPolicy == "" {
		policy.NoChecksPolicy = defaultNoChecksPolicy
	}
	return policy
}

type requiredCheckMatcher struct {
	exact                    map[string][]string
	requiredPatterns         []requiredCheckPattern
	treatAllReportedRequired bool
}

type requiredCheckPattern struct {
	pattern string
	source  string
}

func newRequiredCheckMatcher(policy PullRequestCheckPolicy) requiredCheckMatcher {
	matcher := requiredCheckMatcher{
		exact:                    map[string][]string{},
		treatAllReportedRequired: policy.TreatAllReportedAsRequired,
	}

	for _, name := range trimNonEmptyStrings(policy.RequiredCheckNames) {
		matcher.addExact(name, "config")
	}

	for _, name := range trimNonEmptyStrings(policy.BranchProtectionCheckNames) {
		matcher.addExact(name, "branch_protection")
	}

	for _, name := range trimNonEmptyStrings(policy.RulesetCheckNames) {
		matcher.addExact(name, "ruleset")
	}

	for _, pattern := range trimNonEmptyStrings(policy.RequiredCheckPatterns) {
		matcher.requiredPatterns = append(matcher.requiredPatterns, requiredCheckPattern{pattern: pattern, source: "config_pattern"})
	}

	return matcher
}

func (m requiredCheckMatcher) addExact(name, source string) {
	if strings.TrimSpace(name) == "" {
		return
	}
	m.exact[name] = append(m.exact[name], source)
}

func (m requiredCheckMatcher) hasRequiredChecks() bool {
	return m.treatAllReportedRequired || len(m.exact) > 0 || len(m.requiredPatterns) > 0
}

func (m requiredCheckMatcher) hasNamedRequiredChecks() bool {
	return len(m.exact) > 0 || len(m.requiredPatterns) > 0
}

func (m requiredCheckMatcher) match(name string) (bool, string) {
	if m.treatAllReportedRequired {
		return true, "all_reported"
	}

	var sources []string
	if exactSources, ok := m.exact[name]; ok {
		sources = append(sources, exactSources...)
	}

	for _, pattern := range m.requiredPatterns {
		if wildcardMatch(pattern.pattern, name) {
			sources = append(sources, pattern.source)
		}
	}

	if len(sources) == 0 {
		return false, "optional"
	}

	return true, strings.Join(sortedStrings(sources), ",")
}

func (m requiredCheckMatcher) missingRequired(observed map[string]struct{}) []string {
	if m.treatAllReportedRequired {
		return nil
	}

	var missing []string
	for name := range m.exact {
		if _, ok := observed[name]; !ok {
			missing = append(missing, name)
		}
	}

	for _, pattern := range m.requiredPatterns {
		patternObserved := false
		for name := range observed {
			if wildcardMatch(pattern.pattern, name) {
				patternObserved = true
				break
			}
		}
		if !patternObserved {
			missing = append(missing, "pattern:"+pattern.pattern)
		}
	}

	return sortedStrings(missing)
}

func (m requiredCheckMatcher) exactNames() []string {
	names := make([]string, 0, len(m.exact))
	for name := range m.exact {
		names = append(names, name)
	}
	return names
}

func (m requiredCheckMatcher) patterns() []string {
	patterns := make([]string, 0, len(m.requiredPatterns))
	for _, pattern := range m.requiredPatterns {
		patterns = append(patterns, pattern.pattern)
	}
	return patterns
}

func (m requiredCheckMatcher) sources() []string {
	var sources []string
	for _, exactSources := range m.exact {
		sources = append(sources, exactSources...)
	}
	for _, pattern := range m.requiredPatterns {
		sources = append(sources, pattern.source)
	}
	return sources
}

func requirementSourceSummary(sources []string, treatAllReported bool) string {
	if treatAllReported {
		return "all_reported"
	}

	sources = sortedStrings(sources)
	if len(sources) == 0 {
		return "none"
	}

	return strings.Join(sources, ",")
}

func wildcardMatch(pattern, value string) bool {
	pattern = strings.TrimSpace(pattern)
	if pattern == "" {
		return false
	}

	return wildcardMatchRunes([]rune(pattern), []rune(value))
}

func wildcardMatchRunes(pattern, value []rune) bool {
	starIndex := -1
	matchIndex := 0
	patternIndex := 0
	valueIndex := 0

	for valueIndex < len(value) {
		switch {
		case patternIndex < len(pattern) && (pattern[patternIndex] == '?' || pattern[patternIndex] == value[valueIndex]):
			patternIndex++
			valueIndex++
		case patternIndex < len(pattern) && pattern[patternIndex] == '*':
			starIndex = patternIndex
			matchIndex = valueIndex
			patternIndex++
		case starIndex != -1:
			patternIndex = starIndex + 1
			matchIndex++
			valueIndex = matchIndex
		default:
			return false
		}
	}

	for patternIndex < len(pattern) && pattern[patternIndex] == '*' {
		patternIndex++
	}

	return patternIndex == len(pattern)
}

func sortedStrings(values []string) []string {
	values = trimNonEmptyStrings(values)
	sort.Strings(values)
	return slices.Compact(values)
}

type githubRequiredCheckDiscovery struct {
	branchProtectionErr    error
	rulesetErr             error
	branchProtectionChecks []string
	rulesetChecks          []string
}

func (c *GitHubClient) fetchGitHubRequiredChecks(ctx context.Context, branch string) githubRequiredCheckDiscovery {
	branchProtectionChecks, branchProtectionErr := c.fetchBranchProtectionRequiredChecks(ctx, branch)
	rulesetChecks, rulesetErr := c.fetchRulesetRequiredChecks(ctx, branch)
	return githubRequiredCheckDiscovery{
		branchProtectionErr:    branchProtectionErr,
		rulesetErr:             rulesetErr,
		branchProtectionChecks: branchProtectionChecks,
		rulesetChecks:          rulesetChecks,
	}
}

func (c *GitHubClient) fetchBranchProtectionRequiredChecks(ctx context.Context, branch string) ([]string, error) {
	branch = strings.TrimSpace(branch)
	if branch == "" {
		return nil, errors.New("branch protection lookup skipped: base branch is not available")
	}

	var payload githubRequiredStatusChecks
	if err := c.get(ctx, fmt.Sprintf(
		"/repos/%s/%s/branches/%s/protection/required_status_checks",
		url.PathEscape(c.cfg.Owner),
		url.PathEscape(c.cfg.Repo),
		url.PathEscape(branch),
	), &payload); err != nil {
		var statusErr *githubAPIStatusError
		if errors.As(err, &statusErr) && statusErr.StatusCode == http.StatusNotFound {
			return nil, nil
		}

		return nil, fmt.Errorf("branch protection required status checks: %w", err)
	}

	return payload.contexts(), nil
}

func (c *GitHubClient) fetchRulesetRequiredChecks(ctx context.Context, branch string) ([]string, error) {
	branch = strings.TrimSpace(branch)
	if branch == "" {
		return nil, errors.New("ruleset lookup skipped: base branch is not available")
	}

	var contexts []string
	for page := 1; ; page++ {
		values := url.Values{}
		values.Set("per_page", "100")
		values.Set("page", strconv.Itoa(page))

		var payload []githubBranchRule
		if err := c.get(ctx, fmt.Sprintf(
			"/repos/%s/%s/rules/branches/%s?%s",
			url.PathEscape(c.cfg.Owner),
			url.PathEscape(c.cfg.Repo),
			url.PathEscape(branch),
			values.Encode(),
		), &payload); err != nil {
			var statusErr *githubAPIStatusError
			if errors.As(err, &statusErr) && statusErr.StatusCode == http.StatusNotFound {
				return nil, nil
			}

			return nil, fmt.Errorf("ruleset required status checks: %w", err)
		}

		for _, rule := range payload {
			if rule.Type != "required_status_checks" {
				continue
			}

			contexts = append(contexts, rule.Parameters.contexts()...)
		}

		if len(payload) < 100 {
			break
		}
	}

	return sortedStrings(contexts), nil
}

func pullRequestBranchUpdateReason(pr GitHubPullRequest) string {
	base := firstNonEmpty(pr.Base.Ref, "base")
	switch normalizeState(pr.MergeableState) {
	case "behind":
		return "pull request branch is behind " + base
	case "dirty":
		return "pull request branch has merge conflicts with " + base
	default:
		return ""
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

type githubIssueComment struct {
	Body              *string     `json:"body"`
	HTMLURL           *string     `json:"html_url"`
	User              *githubUser `json:"user"`
	AuthorAssociation string      `json:"author_association"`
	CreatedAt         string      `json:"created_at"`
	UpdatedAt         string      `json:"updated_at"`
}

type githubUser struct {
	Login string `json:"login"`
}

type githubLabel struct {
	Name string `json:"name"`
}

// GitHubPullRequest is the subset of GitHub's PR payload needed by Symphony.
type GitHubPullRequest struct {
	Body           *string               `json:"body,omitempty"`
	Base           githubPullRequestBase `json:"base"`
	Head           githubPullRequestHead `json:"head"`
	HTMLURL        string                `json:"html_url"`
	MergeableState string                `json:"mergeable_state,omitempty"`
	NodeID         string                `json:"node_id,omitempty"`
	State          string                `json:"state"`
	Title          string                `json:"title,omitempty"`
	Number         int                   `json:"number"`
	Draft          bool                  `json:"draft,omitempty"`
	DraftKnown     bool                  `json:"-"`
}

// UnmarshalJSON records whether GitHub included the draft field so merge logic
// can distinguish an explicit false value from an omitted field.
func (pr *GitHubPullRequest) UnmarshalJSON(data []byte) error {
	type alias GitHubPullRequest

	var decoded alias
	if err := json.Unmarshal(data, &decoded); err != nil {
		return fmt.Errorf("github pull request: %w", err)
	}

	var fields map[string]json.RawMessage
	if err := json.Unmarshal(data, &fields); err != nil {
		return fmt.Errorf("github pull request fields: %w", err)
	}

	*pr = GitHubPullRequest(decoded)
	pr.DraftKnown = false
	if rawDraft, ok := fields["draft"]; ok {
		pr.DraftKnown = true
		if err := json.Unmarshal(rawDraft, &pr.Draft); err != nil {
			return fmt.Errorf("github pull request draft: %w", err)
		}
	}

	return nil
}

type githubPullRequestHead struct {
	Ref string `json:"ref"`
	SHA string `json:"sha"`
}

type githubPullRequestBase struct {
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

type githubRequiredStatusChecks struct {
	Contexts []string                    `json:"contexts"`
	Checks   []githubRequiredStatusCheck `json:"checks"`
}

type githubRequiredStatusCheck struct {
	Context string `json:"context"`
}

func (c githubRequiredStatusChecks) contexts() []string {
	out := append([]string(nil), c.Contexts...)
	for _, check := range c.Checks {
		out = append(out, check.Context)
	}
	return sortedStrings(out)
}

type githubBranchRule struct {
	Parameters githubBranchRuleParameters `json:"parameters"`
	Type       string                     `json:"type"`
}

type githubBranchRuleParameters struct {
	RequiredStatusChecks []githubRulesetRequiredStatusCheck `json:"required_status_checks"`
}

type githubRulesetRequiredStatusCheck struct {
	Context string `json:"context"`
}

func (p githubBranchRuleParameters) contexts() []string {
	contexts := make([]string, 0, len(p.RequiredStatusChecks))
	for _, check := range p.RequiredStatusChecks {
		contexts = append(contexts, check.Context)
	}

	return sortedStrings(contexts)
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

func normalizeGitHubIssueComment(comment githubIssueComment) IssueComment {
	var author string
	if comment.User != nil {
		author = comment.User.Login
	}
	var body string
	if comment.Body != nil {
		body = *comment.Body
	}

	return IssueComment{
		Author:            author,
		AuthorAssociation: comment.AuthorAssociation,
		Body:              strings.TrimSpace(body),
		URL:               comment.HTMLURL,
		CreatedAt:         parseTimePointer(comment.CreatedAt),
		UpdatedAt:         parseTimePointer(comment.UpdatedAt),
	}
}

func watchIssueRef(issue githubIssue, fingerprint string) watch.IssueRef {
	return watch.IssueRef{
		ID:          firstNonEmpty(issue.NodeID, fmt.Sprintf("GH-%d", issue.Number)),
		URL:         stringValue(issue.HTMLURL),
		Fingerprint: fingerprint,
		State:       issue.State,
		Number:      issue.Number,
	}
}

func watchIssueMutationPayload(draft watch.IssueDraft) map[string]any {
	payload := map[string]any{
		"title": draft.Title,
		"body":  draft.Body,
	}
	if len(draft.Labels) > 0 {
		payload["labels"] = draft.Labels
	}

	return payload
}

func (c *GitHubClient) repositoryPath(path string) string {
	return "/repos/" + url.PathEscape(c.cfg.Owner) + "/" + url.PathEscape(c.cfg.Repo) + "/" + strings.TrimPrefix(path, "/")
}

func stringValue(value *string) string {
	if value == nil {
		return ""
	}

	return *value
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
