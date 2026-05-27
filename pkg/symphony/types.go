// Package symphony implements the Symphony scheduler/runner service contract.
package symphony

import (
	"context"
	"encoding/json"
	"log/slog"
	"time"
)

const (
	// DefaultWorkflowFile is used when the caller does not provide an explicit
	// workflow path.
	DefaultWorkflowFile = "WORKFLOW.md"

	defaultLinearEndpoint    = "https://api.linear.app/graphql"
	defaultGitHubEndpoint    = "https://api.github.com"
	trackerKindLinear        = "linear"
	trackerKindGitHub        = "github"
	defaultPollInterval      = 30 * time.Second
	defaultHookTimeout       = time.Minute
	defaultMaxConcurrent     = 10
	defaultMaxTurns          = 20
	defaultMaxRetryBackoff   = 5 * time.Minute
	defaultCodexCommand      = "codex app-server"
	defaultCodexTurnTimeout  = time.Hour
	defaultCodexReadTimeout  = 5 * time.Second
	defaultCodexStallTimeout = 5 * time.Minute
	continuationRetryDelay   = time.Second
	defaultLinearPageSize    = 50
	defaultLinearHTTPTimeout = 30 * time.Second
	defaultPromptWhenEmpty   = "You are working on an issue from the configured tracker."
	defaultDebugAddress      = "127.0.0.1:34000"
	defaultDebugEventLimit   = 200
	defaultPRCheckInterval   = 30 * time.Second
	defaultMaxPRRework       = 3
	defaultNoChecksPolicy    = PullRequestNoChecksPass
	linearAPIKeyEnv          = "LINEAR_API_KEY" //nolint:gosec // Environment variable name, not a credential value.
	githubTokenEnv           = "GITHUB_TOKEN"   //nolint:gosec // Environment variable name, not a credential value.
	githubCLITokenEnv        = "GH_TOKEN"
	symphonyServiceName      = "atteler-symphony"
	workspaceDirPermissions  = 0o750
	workflowFilePermissions  = 0o600
	maxAppServerProtocolLine = 16 * 1024 * 1024
)

// Issue is the normalized tracker issue model used by Symphony.
type Issue struct {
	CreatedAt   *time.Time     `json:"created_at,omitempty"`
	UpdatedAt   *time.Time     `json:"updated_at,omitempty"`
	Description *string        `json:"description,omitempty"`
	Priority    *int           `json:"priority,omitempty"`
	BranchName  *string        `json:"branch_name,omitempty"`
	URL         *string        `json:"url,omitempty"`
	ID          string         `json:"id"`
	Identifier  string         `json:"identifier"`
	Title       string         `json:"title"`
	State       string         `json:"state"`
	Labels      []string       `json:"labels"`
	BlockedBy   []BlockerRef   `json:"blocked_by"`
	Comments    []IssueComment `json:"comments,omitempty"`
}

// IssueComment is a normalized tracker discussion comment.
type IssueComment struct {
	CreatedAt         *time.Time `json:"created_at,omitempty"`
	UpdatedAt         *time.Time `json:"updated_at,omitempty"`
	URL               *string    `json:"url,omitempty"`
	Author            string     `json:"author,omitempty"`
	AuthorAssociation string     `json:"author_association,omitempty"`
	Body              string     `json:"body,omitempty"`
}

// BlockerRef identifies an issue that blocks a candidate issue.
type BlockerRef struct {
	ID         *string `json:"id,omitempty"`
	Identifier *string `json:"identifier,omitempty"`
	State      *string `json:"state,omitempty"`
}

// WorkflowDefinition is the parsed WORKFLOW.md payload.
type WorkflowDefinition struct {
	Config         map[string]any
	PromptTemplate string
}

// Config is the typed, resolved service configuration derived from workflow
// front matter.
//
//nolint:govet // Keep logical config grouping instead of fieldalignment's memory-optimal order.
type Config struct {
	WorkflowPath string
	WorkflowDir  string
	Tracker      TrackerConfig
	Polling      PollingConfig
	Workspace    WorkspaceConfig
	Publish      PublishConfig
	Debug        DebugConfig
	Hooks        HooksConfig
	Agent        AgentConfig
	Codex        CodexConfig
}

// TrackerConfig configures issue tracker access.
type TrackerConfig struct {
	Kind           string
	Endpoint       string
	APIKey         string
	ProjectSlug    string
	Repository     string
	Owner          string
	Repo           string
	ActiveStates   []string
	TerminalStates []string
	Labels         []string
}

// PollingConfig configures the scheduler cadence.
type PollingConfig struct {
	Interval time.Duration
}

// WorkspaceConfig configures per-issue workspace location.
type WorkspaceConfig struct {
	Root string
}

// PublishConfig configures the explicit local commit, push, PR, and tracker
// finalization path after a worker leaves a successful workspace.
type PublishConfig struct {
	Remote                 string
	RemoteURL              string
	BaseBranch             string
	BranchPrefix           string
	GitUserName            string
	GitUserEmail           string
	NoChecksPolicy         PullRequestNoChecksPolicy
	RemoveLabels           []string
	RequiredCheckNames     []string
	RequiredCheckPatterns  []string
	CheckInterval          time.Duration
	MaxCheckReworkAttempts int
	Enabled                bool
	Draft                  bool
	MonitorChecks          bool
	DiscoverRequiredChecks bool
	ReworkOptionalChecks   bool
}

// DebugConfig configures the local HTTP status/debug API.
type DebugConfig struct {
	Address    string
	EventLimit int
	Enabled    bool
}

// HooksConfig configures workspace lifecycle hooks.
type HooksConfig struct {
	AfterCreate  string
	BeforeRun    string
	AfterRun     string
	BeforeRemove string
	Timeout      time.Duration
}

// AgentConfig configures orchestration limits and retry behavior.
//
//nolint:govet // Keep scalar limits near their state-specific override map.
type AgentConfig struct {
	MaxConcurrentAgents        int
	MaxTurns                   int
	MaxRetryBackoff            time.Duration
	MaxConcurrentAgentsByState map[string]int
}

// CodexConfig configures the Codex app-server runner.
type CodexConfig struct {
	ApprovalPolicy    any
	ThreadSandbox     any
	TurnSandboxPolicy any
	ExtraConfig       map[string]any
	Command           string
	TurnTimeout       time.Duration
	ReadTimeout       time.Duration
	StallTimeout      time.Duration
}

// Workspace describes the filesystem workspace assigned to an issue.
type Workspace struct {
	Path         string
	WorkspaceKey string
	CreatedNow   bool
}

// RunRequest is one agent worker attempt.
//
//nolint:govet // Keep request fields in the order callers reason about the run lifecycle.
type RunRequest struct {
	Config   Config
	Workflow WorkflowDefinition
	Issue    Issue
	Attempt  *int
	Context  *RunContext
}

// RunResult records one worker attempt outcome.
type RunResult struct {
	Publish       *PublishResult
	WorkspacePath string
	StartedAt     time.Time
	CompletedAt   time.Time
	Status        AttemptStatus
	Error         string
}

// PublishResult records the optional publish/finalization result for a worker
// attempt.
type PublishResult struct {
	Branch              string
	CommitSHA           string
	PullRequestURL      string
	SkippedReason       string
	RemovedLabels       []string
	PullRequestNumber   int
	Published           bool
	ExistingPullRequest bool
}

// RunContext carries orchestration context that is not part of the original
// tracker issue, such as PR CI remediation instructions.
type RunContext struct {
	PullRequest *PullRequestReworkContext
	Kind        RunKind
}

// RunKind identifies why Symphony launched a worker.
type RunKind string

const (
	// RunKindIssue launches a normal issue implementation worker.
	RunKindIssue RunKind = "issue"
	// RunKindPullRequestRework launches a worker to repair an existing PR.
	RunKindPullRequestRework RunKind = "pull_request_rework"
)

// PullRequestReworkContext tells a worker why it is re-entering a published PR
// branch and which checks need attention.
type PullRequestReworkContext struct {
	URL                  string
	Branch               string
	HeadSHA              string
	Summary              string
	FailedChecks         []string
	RequiredFailedChecks []string
	OptionalFailedChecks []string
	Number               int
	ReworkAttempt        int
}

// PullRequestCheckState is Symphony's normalized view of GitHub PR checks.
type PullRequestCheckState string

const (
	// PullRequestChecksPending means at least one required check is not complete.
	PullRequestChecksPending PullRequestCheckState = "pending"
	// PullRequestChecksPassed means required checks passed or the configured
	// no-check policy completed monitoring.
	PullRequestChecksPassed PullRequestCheckState = "passed"
	// PullRequestChecksFailed means at least one required check failed, or
	// optional/no-check failures are configured to fail monitoring.
	PullRequestChecksFailed PullRequestCheckState = "failed"
)

// PullRequestNoChecksPolicy controls what the PR monitor does when no required
// checks are configured, discovered, or reported for a repository.
type PullRequestNoChecksPolicy string

const (
	// PullRequestNoChecksPass completes PR monitoring when no required checks
	// exist for the head commit.
	PullRequestNoChecksPass PullRequestNoChecksPolicy = "pass"
	// PullRequestNoChecksPending preserves the legacy wait-for-CI behavior.
	PullRequestNoChecksPending PullRequestNoChecksPolicy = "pending"
	// PullRequestNoChecksFail dispatches rework when a workflow requires checks
	// to exist but none are configured, discovered, or reported.
	PullRequestNoChecksFail PullRequestNoChecksPolicy = "fail"
)

// PullRequestCheckPolicy configures how GitHub check runs and legacy commit
// status contexts are separated into required and optional evidence.
//
// RequiredCheckPatterns use a small glob syntax where '*' matches any
// substring and '?' matches one rune. The exported policy type lets the
// orchestrator pass publish-monitor config without making GitHub tracker config
// responsible for publish behavior.
type PullRequestCheckPolicy struct {
	NoChecksPolicy             PullRequestNoChecksPolicy
	RequiredCheckNames         []string
	RequiredCheckPatterns      []string
	BranchProtectionCheckNames []string
	RulesetCheckNames          []string
	DiscoverRequiredChecks     bool
	ReworkOptionalChecks       bool
	TreatAllReportedAsRequired bool
}

// PullRequestCheckSnapshot is the GitHub evidence used by the PR remediation
// lane and debug API.
//
//nolint:govet // Field order keeps timestamps and PR identity before check details for JSON/debug readability.
type PullRequestCheckSnapshot struct {
	CheckedAt                 time.Time             `json:"checked_at"`
	PullRequestURL            string                `json:"pull_request_url,omitempty"`
	HeadRef                   string                `json:"head_ref,omitempty"`
	HeadSHA                   string                `json:"head_sha,omitempty"`
	BaseRef                   string                `json:"base_ref,omitempty"`
	BaseSHA                   string                `json:"base_sha,omitempty"`
	MergeableState            string                `json:"mergeable_state,omitempty"`
	Summary                   string                `json:"summary,omitempty"`
	BranchUpdateReason        string                `json:"branch_update_reason,omitempty"`
	RequirementSource         string                `json:"requirement_source,omitempty"`
	NoChecksPolicy            string                `json:"no_checks_policy,omitempty"`
	BranchProtectionError     string                `json:"branch_protection_error,omitempty"`
	RulesetError              string                `json:"ruleset_error,omitempty"`
	RequiredCheckNames        []string              `json:"required_check_names,omitempty"`
	RequiredCheckPatterns     []string              `json:"required_check_patterns,omitempty"`
	FailedCheckNames          []string              `json:"failed_check_names,omitempty"`
	RequiredFailedCheckNames  []string              `json:"required_failed_check_names,omitempty"`
	OptionalFailedCheckNames  []string              `json:"optional_failed_check_names,omitempty"`
	PendingRequiredCheckNames []string              `json:"pending_required_check_names,omitempty"`
	MissingRequiredCheckNames []string              `json:"missing_required_check_names,omitempty"`
	PendingOptionalCheckNames []string              `json:"pending_optional_check_names,omitempty"`
	CheckRuns                 []PullRequestCheckRun `json:"check_runs,omitempty"`
	StatusContexts            []PullRequestStatus   `json:"status_contexts,omitempty"`
	PullRequestNumber         int                   `json:"pull_request_number"`
	State                     PullRequestCheckState `json:"state"`
	PullRequestClosed         bool                  `json:"pull_request_closed"`
	NeedsBranchUpdate         bool                  `json:"needs_branch_update"`
}

// PullRequestCheckRun is a single GitHub Checks API run.
type PullRequestCheckRun struct {
	Name              string `json:"name"`
	Status            string `json:"status,omitempty"`
	Conclusion        string `json:"conclusion,omitempty"`
	DetailsURL        string `json:"details_url,omitempty"`
	HTMLURL           string `json:"html_url,omitempty"`
	RequirementSource string `json:"requirement_source,omitempty"`
	Required          bool   `json:"required"`
}

// PullRequestStatus is a single legacy GitHub commit-status context.
type PullRequestStatus struct {
	Context           string `json:"context"`
	State             string `json:"state,omitempty"`
	Description       string `json:"description,omitempty"`
	TargetURL         string `json:"target_url,omitempty"`
	RequirementSource string `json:"requirement_source,omitempty"`
	Required          bool   `json:"required"`
}

// MonitoredPullRequest ties an open GitHub PR back to the issue branch
// convention used by Symphony.
//
//nolint:govet // Keep Issue and PullRequest grouped for caller readability; this is not a hot-path allocation.
type MonitoredPullRequest struct {
	Issue       Issue
	PullRequest GitHubPullRequest
	Branch      string
}

// AttemptStatus is a normalized worker attempt terminal state.
type AttemptStatus string

// Attempt status values emitted by a worker attempt.
const (
	AttemptSucceeded AttemptStatus = "succeeded"
	AttemptFailed    AttemptStatus = "failed"
	AttemptTimedOut  AttemptStatus = "timed_out"
	AttemptStalled   AttemptStatus = "stalled"
	AttemptCanceled  AttemptStatus = "canceled_by_reconciliation"
)

// CodexEvent is emitted by the app-server client and consumed by the
// orchestrator state machine.
//
//nolint:govet // Keep event fields grouped by semantic meaning for log readers.
type CodexEvent struct {
	Timestamp    time.Time
	Payload      json.RawMessage
	Usage        *TokenUsage
	RateLimits   json.RawMessage
	Event        string
	ThreadID     string
	TurnID       string
	SessionID    string
	AppServerPID string
	Message      string
	CommandID    string
	ProcessID    string
	Command      string
	OutputStream string
	OutputChunk  string
	InputTokens  int64
	OutputTokens int64
	TotalTokens  int64
	ExitCode     int
	ExitCodeSet  bool
}

// TokenUsage captures the app-server token usage shape that Symphony tracks.
type TokenUsage struct {
	InputTokens  int64
	OutputTokens int64
	TotalTokens  int64
}

// TrackerClient is the tracker adapter contract required by the orchestrator.
type TrackerClient interface {
	FetchCandidateIssues(context.Context) ([]Issue, error)
	FetchIssuesByStates(context.Context, []string) ([]Issue, error)
	FetchIssueStatesByIDs(context.Context, []string) ([]Issue, error)
}

// AgentRunner executes one issue attempt in a prepared workspace.
type AgentRunner interface {
	Run(context.Context, RunRequest, func(CodexEvent)) (RunResult, error)
}

// Options configures a Symphony service run.
type Options struct {
	Logger       *slog.Logger
	WorkflowPath string
	WorkDir      string
}

// ErrorClass identifies typed errors required by the specification.
type ErrorClass string

// Error class values required by the Symphony contract.
const (
	ErrMissingWorkflowFile       ErrorClass = "missing_workflow_file"
	ErrWorkflowParse             ErrorClass = "workflow_parse_error"
	ErrWorkflowFrontMatterNotMap ErrorClass = "workflow_front_matter_not_a_map"
	ErrTemplateParse             ErrorClass = "template_parse_error"
	ErrTemplateRender            ErrorClass = "template_render_error"
)

// ClassedError is an error with a stable Symphony error class.
type ClassedError struct {
	Err   error
	Class ErrorClass
}

func (e *ClassedError) Error() string {
	if e == nil || e.Err == nil {
		return string(e.Class)
	}

	return string(e.Class) + ": " + e.Err.Error()
}

func (e *ClassedError) Unwrap() error {
	if e == nil {
		return nil
	}

	return e.Err
}
