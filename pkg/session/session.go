// Package session persists atteler chat sessions for replay and continuation.
package session

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/tommoulard/atteler/pkg/llm"
)

const (
	// EnvDir overrides the default session storage directory.
	EnvDir = "ATTELER_SESSION_DIR"

	sessionFileExt = ".json"

	// AgentEvaluationSchemaVersion is the current persisted evaluation metadata schema.
	AgentEvaluationSchemaVersion = 2

	// EvaluationSourceHuman marks an evaluation entered by a person.
	EvaluationSourceHuman = "human"
	// EvaluationSourceHarness marks an evaluation emitted by a repeatable eval harness.
	EvaluationSourceHarness = "harness"
	// EvaluationSourceCI marks an evaluation emitted by continuous integration.
	EvaluationSourceCI = "ci"
	// EvaluationSourceLegacy marks records that predate provenance metadata.
	EvaluationSourceLegacy = "legacy"

	// RubricVersionLegacy is used when older records have scores but no rubric.
	RubricVersionLegacy = "legacy-unversioned"
)

// Session is a durable chat transcript.
//
//nolint:govet // Field order keeps the on-disk JSON/YAML schema stable and readable.
type Session struct {
	CreatedAt             time.Time           `json:"created_at"`
	UpdatedAt             time.Time           `json:"updated_at"`
	ID                    string              `json:"id"`
	Title                 string              `json:"title,omitempty"`
	DefaultModel          string              `json:"default_model,omitempty"`
	DefaultReasoningLevel string              `json:"default_reasoning_level,omitempty"`
	DefaultModelMode      string              `json:"default_model_mode,omitempty"`
	DefaultAgent          string              `json:"default_agent,omitempty"`
	Autonomy              string              `json:"autonomy,omitempty" yaml:"autonomy,omitempty"`
	AgentLoopBudget       llm.AgentLoopBudget `json:"agent_loop_budget,omitzero"`
	// PromptSuggestions stores the session-scoped background prompt suggestion
	// preference. Empty means no session override; callers should fall back to
	// folder/global state or the safe local-only default.
	PromptSuggestions     string                     `json:"prompt_suggestions,omitempty" yaml:"prompt_suggestions,omitempty"`
	WorktreePath          string                     `json:"worktree_path,omitempty"`
	WorktreeBranch        string                     `json:"worktree_branch,omitempty"`
	WorktreeBase          string                     `json:"worktree_base,omitempty"`
	Tags                  []string                   `json:"tags,omitempty"`
	Messages              []llm.Message              `json:"messages"`
	NegativeKnowledge     []NegativeKnowledge        `json:"negative_knowledge,omitempty" yaml:"negative_knowledge,omitempty"`
	Evaluations           []AgentEvaluation          `json:"evaluations,omitempty" yaml:"evaluations,omitempty"`
	Artifacts             []Artifact                 `json:"artifacts,omitempty" yaml:"artifacts,omitempty"`
	MultiAgentRuns        []MultiAgentRun            `json:"multi_agent_runs,omitempty" yaml:"multi_agent_runs,omitempty"`
	BackgroundSuggestions *BackgroundSuggestionUsage `json:"background_suggestions,omitempty" yaml:"background_suggestions,omitempty"`
}

// BackgroundSuggestionUsage stores usage for background prompt suggestion calls
// separately from user-submitted chat completions.
type BackgroundSuggestionUsage struct {
	UpdatedAt             time.Time `json:"updated_at,omitzero" yaml:"updated_at,omitempty"`
	LastProvider          string    `json:"last_provider,omitempty" yaml:"last_provider,omitempty"`
	LastModel             string    `json:"last_model,omitempty" yaml:"last_model,omitempty"`
	LastStatus            string    `json:"last_status,omitempty" yaml:"last_status,omitempty"`
	LastContextSummary    string    `json:"last_context_summary,omitempty" yaml:"last_context_summary,omitempty"`
	EstimatedCostUSD      float64   `json:"estimated_cost_usd,omitempty" yaml:"estimated_cost_usd,omitempty"`
	Requests              int       `json:"requests,omitempty" yaml:"requests,omitempty"`
	ProviderCalls         int       `json:"provider_calls,omitempty" yaml:"provider_calls,omitempty"`
	Responses             int       `json:"responses,omitempty" yaml:"responses,omitempty"`
	Errors                int       `json:"errors,omitempty" yaml:"errors,omitempty"`
	Rejected              int       `json:"rejected,omitempty" yaml:"rejected,omitempty"`
	InputTokens           int       `json:"input_tokens,omitempty" yaml:"input_tokens,omitempty"`
	CachedInputTokens     int       `json:"cached_input_tokens,omitempty" yaml:"cached_input_tokens,omitempty"`
	CacheWriteInputTokens int       `json:"cache_write_input_tokens,omitempty" yaml:"cache_write_input_tokens,omitempty"`
	OutputTokens          int       `json:"output_tokens,omitempty" yaml:"output_tokens,omitempty"`
	EstimatedInputTokens  int       `json:"estimated_input_tokens,omitempty" yaml:"estimated_input_tokens,omitempty"`
	EstimatedOutputTokens int       `json:"estimated_output_tokens,omitempty" yaml:"estimated_output_tokens,omitempty"`
}

// BackgroundSuggestionRecord describes one background prompt suggestion
// lifecycle event to fold into BackgroundSuggestionUsage.
type BackgroundSuggestionRecord struct {
	UpdatedAt             time.Time
	Provider              string
	Model                 string
	Status                string
	ContextSummary        string
	EstimatedCostUSD      float64
	ProviderCall          bool
	Response              bool
	Error                 bool
	Rejected              bool
	InputTokens           int
	CachedInputTokens     int
	CacheWriteInputTokens int
	OutputTokens          int
	EstimatedInputTokens  int
	EstimatedOutputTokens int
}

// NegativeKnowledge records a failed approach so future agents can avoid repeating it.
type NegativeKnowledge struct {
	CreatedAt time.Time `json:"created_at" yaml:"created_at"`
	Approach  string    `json:"approach" yaml:"approach"`
	Reason    string    `json:"reason" yaml:"reason"`
	Commit    string    `json:"commit,omitempty" yaml:"commit,omitempty"`
	Agent     string    `json:"agent,omitempty" yaml:"agent,omitempty"`
	TaskType  string    `json:"task_type,omitempty" yaml:"task_type,omitempty"`
	Severity  string    `json:"severity,omitempty" yaml:"severity,omitempty"`
}

// AgentEvaluation records a human, harness, or CI assessment for an agent output.
type AgentEvaluation struct {
	CreatedAt       time.Time `json:"created_at" yaml:"created_at"`
	Agent           string    `json:"agent" yaml:"agent"`
	Outcome         string    `json:"outcome" yaml:"outcome"`
	Notes           string    `json:"notes,omitempty" yaml:"notes,omitempty"`
	Reference       string    `json:"reference,omitempty" yaml:"reference,omitempty"`
	Source          string    `json:"source,omitempty" yaml:"source,omitempty"`
	Evaluator       string    `json:"evaluator,omitempty" yaml:"evaluator,omitempty"`
	RubricVersion   string    `json:"rubric_version,omitempty" yaml:"rubric_version,omitempty"`
	TaskType        string    `json:"task_type,omitempty" yaml:"task_type,omitempty"`
	Difficulty      string    `json:"difficulty,omitempty" yaml:"difficulty,omitempty"`
	ExpectedOutcome string    `json:"expected_outcome,omitempty" yaml:"expected_outcome,omitempty"`
	Provider        string    `json:"provider,omitempty" yaml:"provider,omitempty"`
	Model           string    `json:"model,omitempty" yaml:"model,omitempty"`
	FixtureVersion  string    `json:"fixture_version,omitempty" yaml:"fixture_version,omitempty"`
	AgentVersion    string    `json:"agent_version,omitempty" yaml:"agent_version,omitempty"`
	SchemaVersion   int       `json:"schema_version,omitempty" yaml:"schema_version,omitempty"`
	Score           int       `json:"score,omitempty" yaml:"score,omitempty"`
	PassRate        float64   `json:"pass_rate,omitempty" yaml:"pass_rate,omitempty"`
	HasPassRate     bool      `json:"has_pass_rate,omitempty" yaml:"has_pass_rate,omitempty"`
	FlakeCount      int       `json:"flake_count,omitempty" yaml:"flake_count,omitempty"`
	DurationMillis  int64     `json:"duration_millis,omitempty" yaml:"duration_millis,omitempty"`
	InputTokens     int       `json:"input_tokens,omitempty" yaml:"input_tokens,omitempty"`
	OutputTokens    int       `json:"output_tokens,omitempty" yaml:"output_tokens,omitempty"`
	TotalTokens     int       `json:"total_tokens,omitempty" yaml:"total_tokens,omitempty"`
	Cost            float64   `json:"cost,omitempty" yaml:"cost,omitempty"`
	Confidence      float64   `json:"confidence,omitempty" yaml:"confidence,omitempty"`
}

// PassRateRecorded reports whether PassRate was intentionally recorded. A zero
// pass rate is meaningful for failed eval suites, so callers must not infer
// presence from PassRate != 0 alone.
func (evaluation AgentEvaluation) PassRateRecorded() bool {
	return evaluation.HasPassRate || evaluation.PassRate != 0
}

// Artifact records a useful file or research artifact produced during a session.
//
//nolint:govet // Field order keeps artifact identity and provenance grouped in serialized sessions.
type Artifact struct {
	Path            string     `json:"path" yaml:"path"`
	LogicalPath     string     `json:"logical_path,omitempty" yaml:"logical_path,omitempty"`
	Kind            string     `json:"kind" yaml:"kind"`
	Summary         string     `json:"summary,omitempty" yaml:"summary,omitempty"`
	SourceAgent     string     `json:"source_agent,omitempty" yaml:"source_agent,omitempty"`
	SourceSessionID string     `json:"source_session_id,omitempty" yaml:"source_session_id,omitempty"`
	SourceCommand   string     `json:"source_command,omitempty" yaml:"source_command,omitempty"`
	SourceTool      string     `json:"source_tool,omitempty" yaml:"source_tool,omitempty"`
	SourceCommit    string     `json:"source_commit,omitempty" yaml:"source_commit,omitempty"`
	WorktreePath    string     `json:"worktree_path,omitempty" yaml:"worktree_path,omitempty"`
	WorktreeBranch  string     `json:"worktree_branch,omitempty" yaml:"worktree_branch,omitempty"`
	WorktreeBase    string     `json:"worktree_base,omitempty" yaml:"worktree_base,omitempty"`
	SHA256          string     `json:"sha256,omitempty" yaml:"sha256,omitempty"`
	ReviewStatus    string     `json:"review_status,omitempty" yaml:"review_status,omitempty"`
	CreatedAt       time.Time  `json:"created_at" yaml:"created_at"`
	ConsumedAt      *time.Time `json:"consumed_at,omitempty" yaml:"consumed_at,omitempty"`
	SizeBytes       int64      `json:"size_bytes,omitempty" yaml:"size_bytes,omitempty"`
	SourceTurn      int        `json:"source_turn,omitempty" yaml:"source_turn,omitempty"`
	WorktreeDirty   bool       `json:"worktree_dirty,omitempty" yaml:"worktree_dirty,omitempty"`
}

// MultiAgentRunStatus is the durable lifecycle state for a review/speculation run.
type MultiAgentRunStatus string

const (
	// MultiAgentRunStatusRunning means at least one child call may still be active.
	MultiAgentRunStatusRunning MultiAgentRunStatus = "running"
	// MultiAgentRunStatusCompleted means all workflow gates passed.
	MultiAgentRunStatusCompleted MultiAgentRunStatus = "completed"
	// MultiAgentRunStatusError means the run ended with a provider, parsing, or validation error.
	MultiAgentRunStatusError MultiAgentRunStatus = "error"
	// MultiAgentRunStatusBudgetExhausted means the run stopped because a configured budget was exceeded.
	MultiAgentRunStatusBudgetExhausted MultiAgentRunStatus = "budget_exhausted"
	// MultiAgentRunStatusCancelled matches the issue terminal-state spelling.
	MultiAgentRunStatusCancelled MultiAgentRunStatus = "cancelled" //nolint:misspell // Public receipt schema uses the issue's terminal-state spelling.
	// MultiAgentRunStatusCanceled is the US-spelling alias.
	MultiAgentRunStatusCanceled MultiAgentRunStatus = MultiAgentRunStatusCancelled
	// MultiAgentRunStatusFailed is retained as a compatibility alias for error terminal states.
	MultiAgentRunStatusFailed MultiAgentRunStatus = MultiAgentRunStatusError
)

const (
	// MultiAgentRunKindSpeculation records a speculative execution workflow.
	MultiAgentRunKindSpeculation = "speculation"
	// MultiAgentRunKindReview records a review-agent workflow.
	MultiAgentRunKindReview = "review"
)

const (
	multiAgentRunArtifactKindVerdict     = "verdict"
	multiAgentRunDecisionOutcomeAccepted = "accepted"
)

// MultiAgentRun captures replayable artifacts for one review/speculation workflow.
//
//nolint:govet // Field order follows lifecycle, request, budget, evidence, then result for readable JSON.
type MultiAgentRun struct {
	StartedAt          time.Time                   `json:"started_at" yaml:"started_at"`
	UpdatedAt          time.Time                   `json:"updated_at" yaml:"updated_at"`
	CompletedAt        *time.Time                  `json:"completed_at,omitempty" yaml:"completed_at,omitempty"`
	ID                 string                      `json:"id" yaml:"id"`
	ReceiptID          string                      `json:"receipt_id,omitempty" yaml:"receipt_id,omitempty"`
	Kind               string                      `json:"kind" yaml:"kind"`
	Status             MultiAgentRunStatus         `json:"status" yaml:"status"`
	Prompt             string                      `json:"prompt,omitempty" yaml:"prompt,omitempty"`
	Model              string                      `json:"model,omitempty" yaml:"model,omitempty"`
	FallbackModels     []string                    `json:"fallback_models,omitempty" yaml:"fallback_models,omitempty"`
	Budget             MultiAgentRunBudget         `json:"budget,omitzero" yaml:"budget,omitempty"`
	Usage              MultiAgentRunUsage          `json:"usage,omitzero" yaml:"usage,omitempty"`
	Calls              []MultiAgentRunCall         `json:"calls,omitempty" yaml:"calls,omitempty"`
	Branches           []MultiAgentRunBranch       `json:"branches,omitempty" yaml:"branches,omitempty"`
	Reviewers          []MultiAgentRunReviewer     `json:"reviewers,omitempty" yaml:"reviewers,omitempty"`
	Artifacts          []MultiAgentRunArtifact     `json:"artifacts,omitempty" yaml:"artifacts,omitempty"`
	Gates              []MultiAgentRunGate         `json:"gates,omitempty" yaml:"gates,omitempty"`
	Disagreements      []MultiAgentRunDisagreement `json:"disagreements,omitempty" yaml:"disagreements,omitempty"`
	Errors             []MultiAgentRunError        `json:"errors,omitempty" yaml:"errors,omitempty"`
	Decisions          []MultiAgentRunDecision     `json:"decisions,omitempty" yaml:"decisions,omitempty"`
	Summary            MultiAgentRunSummary        `json:"summary,omitzero" yaml:"summary,omitempty"`
	CancellationReason string                      `json:"cancellation_reason,omitempty" yaml:"cancellation_reason,omitempty"`
	ResumeReason       string                      `json:"resume_reason,omitempty" yaml:"resume_reason,omitempty"`
	Error              string                      `json:"error,omitempty" yaml:"error,omitempty"`
}

// MultiAgentRunBudget records the limits enforced for one multi-agent run.
type MultiAgentRunBudget struct {
	PerCallMaxInputTokens  int   `json:"per_call_max_input_tokens,omitempty" yaml:"per_call_max_input_tokens,omitempty"`
	PerCallMaxOutputTokens int   `json:"per_call_max_output_tokens,omitempty" yaml:"per_call_max_output_tokens,omitempty"`
	MaxRunInputTokens      int   `json:"max_run_input_tokens,omitempty" yaml:"max_run_input_tokens,omitempty"`
	MaxRunOutputTokens     int   `json:"max_run_output_tokens,omitempty" yaml:"max_run_output_tokens,omitempty"`
	MaxRunTotalTokens      int   `json:"max_run_total_tokens,omitempty" yaml:"max_run_total_tokens,omitempty"`
	MaxModelCalls          int   `json:"max_model_calls,omitempty" yaml:"max_model_calls,omitempty"`
	MaxRunCostMicros       int64 `json:"max_run_cost_micros,omitempty" yaml:"max_run_cost_micros,omitempty"`
	MaxRunWallTimeMS       int64 `json:"max_run_wall_time_ms,omitempty" yaml:"max_run_wall_time_ms,omitempty"`
}

// MultiAgentRunUsage records estimated and observed usage for audit and budget replay.
type MultiAgentRunUsage struct {
	ModelCalls            int   `json:"model_calls,omitempty" yaml:"model_calls,omitempty"`
	EstimatedInputTokens  int   `json:"estimated_input_tokens,omitempty" yaml:"estimated_input_tokens,omitempty"`
	EstimatedOutputTokens int   `json:"estimated_output_tokens,omitempty" yaml:"estimated_output_tokens,omitempty"`
	EstimatedCostMicros   int64 `json:"estimated_cost_micros,omitempty" yaml:"estimated_cost_micros,omitempty"`
	InputTokens           int   `json:"input_tokens,omitempty" yaml:"input_tokens,omitempty"`
	CachedInputTokens     int   `json:"cached_input_tokens,omitempty" yaml:"cached_input_tokens,omitempty"`
	OutputTokens          int   `json:"output_tokens,omitempty" yaml:"output_tokens,omitempty"`
	TotalTokens           int   `json:"total_tokens,omitempty" yaml:"total_tokens,omitempty"`
	EstimatedTotalTokens  int   `json:"estimated_total_tokens,omitempty" yaml:"estimated_total_tokens,omitempty"`
	DurationMS            int64 `json:"duration_ms,omitempty" yaml:"duration_ms,omitempty"`
	BudgetRejectedCalls   int   `json:"budget_rejected_calls,omitempty" yaml:"budget_rejected_calls,omitempty"`
	ProviderFailedCalls   int   `json:"provider_failed_calls,omitempty" yaml:"provider_failed_calls,omitempty"`
	CanceledCalls         int   `json:"canceled_calls,omitempty" yaml:"canceled_calls,omitempty"`
	CompletedCalls        int   `json:"completed_calls,omitempty" yaml:"completed_calls,omitempty"`
}

// MultiAgentRunCall records one provider call or preflight budget rejection.
//
//nolint:govet // Prompt and response fields are intentionally grouped for audit readability.
type MultiAgentRunCall struct {
	StartedAt            time.Time           `json:"started_at" yaml:"started_at"`
	CompletedAt          *time.Time          `json:"completed_at,omitempty" yaml:"completed_at,omitempty"`
	ID                   string              `json:"id" yaml:"id"`
	Phase                string              `json:"phase" yaml:"phase"`
	Agent                string              `json:"agent,omitempty" yaml:"agent,omitempty"`
	TargetAgent          string              `json:"target_agent,omitempty" yaml:"target_agent,omitempty"`
	Status               MultiAgentRunStatus `json:"status" yaml:"status"`
	RequestedModel       string              `json:"requested_model,omitempty" yaml:"requested_model,omitempty"`
	ResponseModel        string              `json:"response_model,omitempty" yaml:"response_model,omitempty"`
	FallbackModels       []string            `json:"fallback_models,omitempty" yaml:"fallback_models,omitempty"`
	PromptHash           string              `json:"prompt_hash,omitempty" yaml:"prompt_hash,omitempty"`
	SystemPrompt         string              `json:"system_prompt,omitempty" yaml:"system_prompt,omitempty"`
	UserPrompt           string              `json:"user_prompt,omitempty" yaml:"user_prompt,omitempty"`
	Response             string              `json:"response,omitempty" yaml:"response,omitempty"`
	Error                string              `json:"error,omitempty" yaml:"error,omitempty"`
	InputTokenEstimate   int                 `json:"input_token_estimate,omitempty" yaml:"input_token_estimate,omitempty"`
	OutputTokenEstimate  int                 `json:"output_token_estimate,omitempty" yaml:"output_token_estimate,omitempty"`
	ContextWindow        int                 `json:"context_window,omitempty" yaml:"context_window,omitempty"`
	MaxOutputTokens      int                 `json:"max_output_tokens,omitempty" yaml:"max_output_tokens,omitempty"`
	InputTokens          int                 `json:"input_tokens,omitempty" yaml:"input_tokens,omitempty"`
	CachedInputTokens    int                 `json:"cached_input_tokens,omitempty" yaml:"cached_input_tokens,omitempty"`
	OutputTokens         int                 `json:"output_tokens,omitempty" yaml:"output_tokens,omitempty"`
	TotalTokens          int                 `json:"total_tokens,omitempty" yaml:"total_tokens,omitempty"`
	EstimatedCostMicros  int64               `json:"estimated_cost_micros,omitempty" yaml:"estimated_cost_micros,omitempty"`
	DurationMS           int64               `json:"duration_ms,omitempty" yaml:"duration_ms,omitempty"`
	BudgetRejectionRule  string              `json:"budget_rejection_rule,omitempty" yaml:"budget_rejection_rule,omitempty"`
	BudgetRejectionUsage int                 `json:"budget_rejection_usage,omitempty" yaml:"budget_rejection_usage,omitempty"`
	BudgetRejectionLimit int                 `json:"budget_rejection_limit,omitempty" yaml:"budget_rejection_limit,omitempty"`
}

// MultiAgentRunBranch records per-branch provenance and budget usage.
//
// Branches are derived from candidate-producing provider calls, such as
// speculation proposals and independent review reports.
//
//nolint:govet // Field order mirrors the persisted audit schema for readable JSON/YAML.
type MultiAgentRunBranch struct {
	Name                 string              `json:"name" yaml:"name"`
	Role                 string              `json:"role,omitempty" yaml:"role,omitempty"`
	Provenance           string              `json:"provenance,omitempty" yaml:"provenance,omitempty"`
	Model                string              `json:"model,omitempty" yaml:"model,omitempty"`
	PromptHash           string              `json:"prompt_hash,omitempty" yaml:"prompt_hash,omitempty"`
	Error                string              `json:"error,omitempty" yaml:"error,omitempty"`
	Status               MultiAgentRunStatus `json:"status" yaml:"status"`
	InputTokenEstimate   int                 `json:"input_token_estimate,omitempty" yaml:"input_token_estimate,omitempty"`
	OutputTokenEstimate  int                 `json:"output_token_estimate,omitempty" yaml:"output_token_estimate,omitempty"`
	ContextWindow        int                 `json:"context_window,omitempty" yaml:"context_window,omitempty"`
	MaxOutputTokens      int                 `json:"max_output_tokens,omitempty" yaml:"max_output_tokens,omitempty"`
	InputTokens          int                 `json:"input_tokens,omitempty" yaml:"input_tokens,omitempty"`
	CachedInputTokens    int                 `json:"cached_input_tokens,omitempty" yaml:"cached_input_tokens,omitempty"`
	OutputTokens         int                 `json:"output_tokens,omitempty" yaml:"output_tokens,omitempty"`
	TotalTokens          int                 `json:"total_tokens,omitempty" yaml:"total_tokens,omitempty"`
	EstimatedCostMicros  int64               `json:"estimated_cost_micros,omitempty" yaml:"estimated_cost_micros,omitempty"`
	DurationMS           int64               `json:"duration_ms,omitempty" yaml:"duration_ms,omitempty"`
	BudgetRejectionRule  string              `json:"budget_rejection_rule,omitempty" yaml:"budget_rejection_rule,omitempty"`
	BudgetRejectionUsage int                 `json:"budget_rejection_usage,omitempty" yaml:"budget_rejection_usage,omitempty"`
	BudgetRejectionLimit int                 `json:"budget_rejection_limit,omitempty" yaml:"budget_rejection_limit,omitempty"`
}

// MultiAgentRunReviewer records one reviewer role invocation with model and prompt provenance.
type MultiAgentRunReviewer struct {
	Name        string `json:"name" yaml:"name"`
	Role        string `json:"role,omitempty" yaml:"role,omitempty"`
	TargetAgent string `json:"target_agent,omitempty" yaml:"target_agent,omitempty"`
	Model       string `json:"model,omitempty" yaml:"model,omitempty"`
	PromptHash  string `json:"prompt_hash,omitempty" yaml:"prompt_hash,omitempty"`
	CallID      string `json:"call_id,omitempty" yaml:"call_id,omitempty"`
}

// MultiAgentRunArtifact stores a replayable workflow artifact.
//
//nolint:govet // JSON/YAML field order keeps audit metadata grouped for reviewability.
type MultiAgentRunArtifact struct {
	CreatedAt   time.Time         `json:"created_at" yaml:"created_at"`
	Kind        string            `json:"kind" yaml:"kind"`
	Phase       string            `json:"phase,omitempty" yaml:"phase,omitempty"`
	Agent       string            `json:"agent,omitempty" yaml:"agent,omitempty"`
	TargetAgent string            `json:"target_agent,omitempty" yaml:"target_agent,omitempty"`
	Content     string            `json:"content,omitempty" yaml:"content,omitempty"`
	Metadata    map[string]string `json:"metadata,omitempty" yaml:"metadata,omitempty"`
	Index       int               `json:"index,omitempty" yaml:"index,omitempty"`
}

// MultiAgentRunGate records one gate decision tied to a reviewer, agent, or verdict.
type MultiAgentRunGate struct {
	Name   string `json:"name" yaml:"name"`
	Phase  string `json:"phase,omitempty" yaml:"phase,omitempty"`
	Agent  string `json:"agent,omitempty" yaml:"agent,omitempty"`
	Notes  string `json:"notes,omitempty" yaml:"notes,omitempty"`
	Passed bool   `json:"passed" yaml:"passed"`
}

// MultiAgentRunDisagreement records cross-review challenges or divergent gate decisions.
type MultiAgentRunDisagreement struct {
	Phase       string `json:"phase,omitempty" yaml:"phase,omitempty"`
	Reviewer    string `json:"reviewer,omitempty" yaml:"reviewer,omitempty"`
	TargetAgent string `json:"target_agent,omitempty" yaml:"target_agent,omitempty"`
	Subject     string `json:"subject,omitempty" yaml:"subject,omitempty"`
	Notes       string `json:"notes,omitempty" yaml:"notes,omitempty"`
	Index       int    `json:"index,omitempty" yaml:"index,omitempty"`
}

// MultiAgentRunError records workflow-level errors that are not provider-call
// failures, such as structured-output parse or validation failures.
type MultiAgentRunError struct {
	Stage       string `json:"stage,omitempty" yaml:"stage,omitempty"`
	Reviewer    string `json:"reviewer,omitempty" yaml:"reviewer,omitempty"`
	TargetAgent string `json:"target_agent,omitempty" yaml:"target_agent,omitempty"`
	Message     string `json:"message" yaml:"message"`
}

// MultiAgentRunDecision records accepted/rejected workflow outputs and rationale.
type MultiAgentRunDecision struct {
	Kind        string `json:"kind" yaml:"kind"`
	Phase       string `json:"phase,omitempty" yaml:"phase,omitempty"`
	Agent       string `json:"agent,omitempty" yaml:"agent,omitempty"`
	TargetAgent string `json:"target_agent,omitempty" yaml:"target_agent,omitempty"`
	Outcome     string `json:"outcome" yaml:"outcome"`
	Rationale   string `json:"rationale,omitempty" yaml:"rationale,omitempty"`
	Index       int    `json:"index,omitempty" yaml:"index,omitempty"`
}

// MultiAgentRunSummary captures compact replay metadata for list and replay commands.
type MultiAgentRunSummary struct {
	Winner          string `json:"winner,omitempty" yaml:"winner,omitempty"`
	Reason          string `json:"reason,omitempty" yaml:"reason,omitempty"`
	VerdictReviewer string `json:"verdict_reviewer,omitempty" yaml:"verdict_reviewer,omitempty"`
	Findings        int    `json:"findings,omitempty" yaml:"findings,omitempty"`
}

// Store reads and writes sessions under a directory.
type Store struct {
	dir         string
	indexPolicy SearchIndexPolicy
}

// Summary is lightweight session metadata for listing.
//
//nolint:govet // JSON/API field readability is preferred over pointer packing.
type Summary struct {
	UpdatedAt       time.Time           `json:"updated_at,omitzero"`
	CreatedAt       time.Time           `json:"created_at,omitzero"`
	AgentNames      []string            `json:"agent_names,omitempty"`
	Path            string              `json:"path"`
	ID              string              `json:"id"`
	Title           string              `json:"title,omitempty"`
	DefaultModel    string              `json:"default_model,omitempty"`
	DefaultAgent    string              `json:"default_agent,omitempty"`
	Autonomy        string              `json:"autonomy,omitempty" yaml:"autonomy,omitempty"`
	AgentLoopBudget llm.AgentLoopBudget `json:"agent_loop_budget,omitzero"`
	WorktreePath    string              `json:"worktree_path,omitempty"`
	WorktreeBranch  string              `json:"worktree_branch,omitempty"`
	WorktreeBase    string              `json:"worktree_base,omitempty"`
	Tags            []string            `json:"tags,omitempty"`
	Messages        int                 `json:"messages"`
}

// sessionSummaryFile mirrors lightweight session JSON fields used for listing.
// It intentionally does not decode message content so summary scans do not
// materialize historical transcripts.
//
//nolint:govet // JSON/API field readability is preferred over pointer packing.
type sessionSummaryFile struct {
	CreatedAt       time.Time
	UpdatedAt       time.Time
	AgentNames      []string
	Tags            []string
	ID              string
	Title           string
	DefaultModel    string
	DefaultAgent    string
	Autonomy        string
	AgentLoopBudget llm.AgentLoopBudget
	WorktreePath    string
	WorktreeBranch  string
	WorktreeBase    string
	Messages        int
}

// TagSummary counts how many saved sessions use a tag.
type TagSummary struct {
	Tag      string
	Sessions int
}

// NewStore creates a session store. If dir is empty, DefaultDir is used.
func NewStore(dir string) *Store {
	return NewStoreWithSearchIndexPolicy(dir, SearchIndexPolicy{})
}

// NewStoreWithSearchIndexPolicy creates a session store using policy for the
// saved-session search index. The zero policy preserves safe defaults.
func NewStoreWithSearchIndexPolicy(dir string, policy SearchIndexPolicy) *Store {
	if dir == "" {
		dir = DefaultDir()
	}

	return &Store{dir: dir, indexPolicy: policy}
}

// DefaultDir returns the default session storage directory.
func DefaultDir() string {
	if dir := os.Getenv(EnvDir); dir != "" {
		return dir
	}

	if cwd, err := os.Getwd(); err == nil {
		return filepath.Join(cwd, ".atteler", "sessions")
	}

	return filepath.Join(os.TempDir(), "atteler", "sessions")
}

// Dir returns the store directory.
func (s *Store) Dir() string {
	return s.dir
}

// New creates a new unsaved session.
func New(defaultModel string, messages []llm.Message) Session {
	now := time.Now().UTC()

	copied := append([]llm.Message(nil), messages...)

	return Session{
		ID:           newID(now),
		CreatedAt:    now,
		UpdatedAt:    now,
		DefaultModel: defaultModel,
		Messages:     copied,
	}
}

// Load reads a session by ID or path.
func (s *Store) Load(ref string) (Session, error) {
	path := s.path(ref)

	data, err := os.ReadFile(path)
	if err != nil {
		return Session{}, fmt.Errorf("session: read %s: %w", path, err)
	}

	var session Session
	if err := json.Unmarshal(data, &session); err != nil {
		return Session{}, fmt.Errorf("session: parse %s: %w", path, err)
	}

	if session.ID == "" {
		session.ID = idFromPath(path)
	}

	return session, nil
}

// Save writes a session atomically enough for local CLI use.
func (s *Store) Save(session Session) error {
	if session.ID == "" {
		return errors.New("session: id is required")
	}

	now := time.Now().UTC()
	if session.CreatedAt.IsZero() {
		session.CreatedAt = now
	}

	session.UpdatedAt = now

	if err := os.MkdirAll(s.dir, 0o750); err != nil {
		return fmt.Errorf("session: create dir: %w", err)
	}

	data, err := json.MarshalIndent(session, "", "  ")
	if err != nil {
		return fmt.Errorf("session: marshal: %w", err)
	}

	data = append(data, '\n')

	path := s.path(session.ID)

	tmp, err := os.CreateTemp(s.dir, ".session-*.json")
	if err != nil {
		return fmt.Errorf("session: create temp: %w", err)
	}

	tmpPath := tmp.Name()
	defer os.Remove(tmpPath)

	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("session: write temp: %w", err)
	}

	if err := tmp.Close(); err != nil {
		return fmt.Errorf("session: close temp: %w", err)
	}

	if err := os.Rename(tmpPath, path); err != nil {
		return fmt.Errorf("session: replace %s: %w", path, err)
	}

	if err := s.indexSavedSession(path); err != nil {
		return fmt.Errorf("session: update search index: %w", err)
	}

	return nil
}

// List returns saved sessions sorted by most recently updated first.
func (s *Store) List() ([]Summary, error) {
	entries, err := os.ReadDir(s.dir)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil
		}

		return nil, fmt.Errorf("session: list %s: %w", s.dir, err)
	}

	summaries := make([]Summary, 0, len(entries))
	for _, entry := range entries {
		if entry.IsDir() || filepath.Ext(entry.Name()) != sessionFileExt {
			continue
		}

		path := filepath.Join(s.dir, entry.Name())

		summary, err := readSummary(path)
		if err != nil {
			return nil, err
		}

		summaries = append(summaries, summary)
	}

	sort.Slice(summaries, func(i, j int) bool {
		return summaries[i].UpdatedAt.After(summaries[j].UpdatedAt)
	})

	return summaries, nil
}

// ListByTag returns saved sessions containing tag, sorted by most recently updated first.
func (s *Store) ListByTag(tag string) ([]Summary, error) {
	key := normalizeTagKey(tag)
	if key == "" {
		return nil, errors.New("session: tag is required")
	}

	summaries, err := s.List()
	if err != nil {
		return nil, err
	}

	filtered := make([]Summary, 0, len(summaries))
	for i := range summaries {
		if summaryHasTag(summaries[i], key) {
			filtered = append(filtered, summaries[i])
		}
	}

	return filtered, nil
}

// Tags returns saved session tags sorted by descending use count, then name.
func (s *Store) Tags() ([]TagSummary, error) {
	summaries, err := s.List()
	if err != nil {
		return nil, err
	}

	counts := make(map[string]int)
	display := make(map[string]string)

	for i := range summaries {
		summary := &summaries[i]

		seen := make(map[string]bool, len(summary.Tags))
		for _, tag := range summary.Tags {
			key := normalizeTagKey(tag)
			if key == "" || seen[key] {
				continue
			}

			seen[key] = true

			counts[key]++
			if _, ok := display[key]; !ok {
				display[key] = strings.TrimSpace(tag)
			}
		}
	}

	tags := make([]TagSummary, 0, len(counts))
	for key, count := range counts {
		tags = append(tags, TagSummary{Tag: display[key], Sessions: count})
	}

	sort.Slice(tags, func(i, j int) bool {
		if tags[i].Sessions == tags[j].Sessions {
			return strings.ToLower(tags[i].Tag) < strings.ToLower(tags[j].Tag)
		}

		return tags[i].Sessions > tags[j].Sessions
	})

	return tags, nil
}

func summaryHasTag(summary Summary, want string) bool {
	for _, tag := range summary.Tags {
		if normalizeTagKey(tag) == want {
			return true
		}
	}

	return false
}

func normalizeTagKey(tag string) string {
	return strings.ToLower(strings.TrimSpace(tag))
}

// Path returns the path for a session reference.
func (s *Store) Path(ref string) string {
	return s.path(ref)
}

// Append adds a message to the session.
func (s *Session) Append(role llm.Role, content string) {
	s.Messages = append(s.Messages, llm.Message{Role: role, Content: content})
}

// RecordBackgroundSuggestionUsage records one background suggestion attempt
// without mixing it into chat token totals.
func (s *Session) RecordBackgroundSuggestionUsage(record BackgroundSuggestionRecord) {
	if s == nil {
		return
	}

	if s.BackgroundSuggestions == nil {
		s.BackgroundSuggestions = &BackgroundSuggestionUsage{}
	}

	usage := s.BackgroundSuggestions

	if record.UpdatedAt.IsZero() {
		record.UpdatedAt = time.Now().UTC()
	}

	usage.UpdatedAt = record.UpdatedAt
	usage.LastProvider = strings.TrimSpace(record.Provider)
	usage.LastModel = strings.TrimSpace(record.Model)
	usage.LastStatus = strings.TrimSpace(record.Status)
	usage.LastContextSummary = strings.TrimSpace(record.ContextSummary)
	usage.EstimatedCostUSD += record.EstimatedCostUSD
	usage.Requests++

	if record.ProviderCall {
		usage.ProviderCalls++
	}

	if record.Response {
		usage.Responses++
	}

	if record.Error {
		usage.Errors++
	}

	if record.Rejected {
		usage.Rejected++
	}

	usage.InputTokens += max(record.InputTokens, 0)
	usage.CachedInputTokens += max(record.CachedInputTokens, 0)
	usage.CacheWriteInputTokens += max(record.CacheWriteInputTokens, 0)
	usage.OutputTokens += max(record.OutputTokens, 0)
	usage.EstimatedInputTokens += max(record.EstimatedInputTokens, 0)
	usage.EstimatedOutputTokens += max(record.EstimatedOutputTokens, 0)
}

// RecordNegativeKnowledge records a failed approach unless the same approach and reason already exist.
func (s *Session) RecordNegativeKnowledge(approach, reason, commit, agent string) bool {
	return s.RecordNegativeKnowledgeDetails(NegativeKnowledge{
		Approach: approach,
		Reason:   reason,
		Commit:   commit,
		Agent:    agent,
	})
}

// RecordNegativeKnowledgeDetails records categorized negative knowledge unless it is a duplicate.
func (s *Session) RecordNegativeKnowledgeDetails(entry NegativeKnowledge) bool {
	entry.Approach = strings.TrimSpace(entry.Approach)

	entry.Reason = strings.TrimSpace(entry.Reason)
	if entry.Approach == "" || entry.Reason == "" {
		return false
	}

	approachKey := normalizeNegativeKnowledgeKey(entry.Approach)

	reasonKey := normalizeNegativeKnowledgeKey(entry.Reason)
	for _, existing := range s.NegativeKnowledge {
		if normalizeNegativeKnowledgeKey(existing.Approach) == approachKey &&
			normalizeNegativeKnowledgeKey(existing.Reason) == reasonKey {
			return false
		}
	}

	entry.Commit = strings.TrimSpace(entry.Commit)
	entry.Agent = strings.TrimSpace(entry.Agent)
	entry.TaskType = strings.TrimSpace(entry.TaskType)

	entry.Severity = strings.TrimSpace(entry.Severity)
	if entry.CreatedAt.IsZero() {
		entry.CreatedAt = time.Now().UTC()
	}

	s.NegativeKnowledge = append(s.NegativeKnowledge, entry)

	return true
}

func normalizeNegativeKnowledgeKey(value string) string {
	return strings.ToLower(strings.Join(strings.Fields(value), " "))
}

// RecordEvaluation appends an agent evaluation when required fields are valid.
func (s *Session) RecordEvaluation(agentName, outcome, notes, reference string, score int) bool {
	return s.RecordEvaluationDetails(AgentEvaluation{
		Agent:         agentName,
		Outcome:       outcome,
		Notes:         notes,
		Reference:     reference,
		Score:         score,
		Source:        EvaluationSourceHuman,
		RubricVersion: RubricVersionLegacy,
		SchemaVersion: AgentEvaluationSchemaVersion,
	})
}

// RecordEvaluationDetails appends a versioned agent evaluation when required fields are valid.
func (s *Session) RecordEvaluationDetails(evaluation AgentEvaluation) bool {
	evaluation, ok := normalizeEvaluationDetails(evaluation)
	if !ok {
		return false
	}

	s.Evaluations = append(s.Evaluations, evaluation)

	return true
}

func normalizeEvaluationDetails(evaluation AgentEvaluation) (AgentEvaluation, bool) {
	evaluation.Agent = strings.TrimSpace(evaluation.Agent)

	evaluation.Outcome = strings.TrimSpace(evaluation.Outcome)
	if evaluation.Agent == "" || evaluation.Outcome == "" {
		return AgentEvaluation{}, false
	}

	if invalidEvaluationCalibration(evaluation) {
		return AgentEvaluation{}, false
	}

	evaluation.Notes = strings.TrimSpace(evaluation.Notes)
	evaluation.Reference = strings.TrimSpace(evaluation.Reference)

	source, ok := normalizeEvaluationSourceForRecord(evaluation.Source)
	if !ok {
		return AgentEvaluation{}, false
	}

	evaluation.Source = source
	evaluation.Evaluator = strings.TrimSpace(evaluation.Evaluator)
	evaluation.RubricVersion = strings.TrimSpace(evaluation.RubricVersion)
	evaluation.TaskType = strings.TrimSpace(evaluation.TaskType)
	evaluation.Difficulty = strings.TrimSpace(evaluation.Difficulty)
	evaluation.ExpectedOutcome = strings.TrimSpace(evaluation.ExpectedOutcome)
	evaluation.Provider = strings.TrimSpace(evaluation.Provider)
	evaluation.Model = strings.TrimSpace(evaluation.Model)
	evaluation.FixtureVersion = strings.TrimSpace(evaluation.FixtureVersion)

	evaluation.AgentVersion = strings.TrimSpace(evaluation.AgentVersion)
	evaluation.SchemaVersion = normalizeEvaluationSchemaVersion(evaluation.SchemaVersion)

	if evaluation.PassRate != 0 {
		evaluation.HasPassRate = true
	}

	if evaluation.TotalTokens == 0 {
		evaluation.TotalTokens = evaluation.InputTokens + evaluation.OutputTokens
	}

	if evaluation.RubricVersion == "" {
		evaluation.RubricVersion = RubricVersionLegacy
	}

	if evaluation.CreatedAt.IsZero() {
		evaluation.CreatedAt = time.Now().UTC()
	}

	return evaluation, true
}

func invalidEvaluationCalibration(evaluation AgentEvaluation) bool {
	return evaluation.Confidence < 0 || evaluation.Confidence > 1 ||
		evaluation.PassRate < 0 || evaluation.PassRate > 1 ||
		evaluation.DurationMillis < 0 || evaluation.Cost < 0 ||
		evaluation.Score < 0 || evaluation.FlakeCount < 0 ||
		evaluation.InputTokens < 0 || evaluation.OutputTokens < 0 || evaluation.TotalTokens < 0 ||
		evaluation.SchemaVersion < 0 ||
		evaluation.SchemaVersion > AgentEvaluationSchemaVersion
}

func normalizeEvaluationSourceForRecord(source string) (string, bool) {
	sourceProvided := strings.TrimSpace(source) != ""
	source = normalizeEvaluationSourceName(source)

	if sourceProvided && source == "" {
		return "", false
	}

	if source == "" {
		source = EvaluationSourceHuman
	}

	return source, true
}

func normalizeEvaluationSchemaVersion(version int) int {
	if version == 0 {
		return AgentEvaluationSchemaVersion
	}

	return version
}

func knownEvaluationSource(source string) bool {
	return normalizeEvaluationSourceName(source) != ""
}

func normalizeEvaluationSourceName(source string) string {
	switch strings.ToLower(strings.TrimSpace(source)) {
	case EvaluationSourceHuman, EvaluationSourceHarness, EvaluationSourceCI:
		return strings.ToLower(strings.TrimSpace(source))
	default:
		return ""
	}
}

// AddArtifact appends a populated session artifact when the path and kind are valid.
func (s *Session) AddArtifact(artifact Artifact) bool {
	path := strings.TrimSpace(artifact.Path)

	kind := strings.TrimSpace(artifact.Kind)
	if path == "" || kind == "" {
		return false
	}

	artifact.Path = filepath.Clean(path)
	artifact.Kind = kind
	artifact.Summary = strings.TrimSpace(artifact.Summary)
	artifact.SourceAgent = strings.TrimSpace(artifact.SourceAgent)
	artifact.SourceSessionID = strings.TrimSpace(artifact.SourceSessionID)
	artifact.SourceCommand = strings.TrimSpace(artifact.SourceCommand)
	artifact.SourceTool = strings.TrimSpace(artifact.SourceTool)
	artifact.SourceCommit = strings.TrimSpace(artifact.SourceCommit)
	artifact.WorktreePath = strings.TrimSpace(artifact.WorktreePath)
	artifact.WorktreeBranch = strings.TrimSpace(artifact.WorktreeBranch)
	artifact.WorktreeBase = strings.TrimSpace(artifact.WorktreeBase)
	artifact.SHA256 = strings.ToLower(strings.TrimSpace(artifact.SHA256))
	artifact.ReviewStatus = strings.TrimSpace(artifact.ReviewStatus)

	if logicalPath := strings.TrimSpace(artifact.LogicalPath); logicalPath != "" {
		artifact.LogicalPath = filepath.Clean(logicalPath)
	} else {
		artifact.LogicalPath = artifact.Path
	}

	if artifact.SourceSessionID == "" {
		artifact.SourceSessionID = strings.TrimSpace(s.ID)
	}

	if artifact.SourceTurn == 0 && len(s.Messages) > 0 {
		artifact.SourceTurn = len(s.Messages)
	}

	if artifact.CreatedAt.IsZero() {
		artifact.CreatedAt = time.Now().UTC()
	}

	s.Artifacts = append(s.Artifacts, artifact)

	return true
}

// MarkArtifactsConsumed records when artifacts were consumed by a downstream merge/export.
func (s *Session) MarkArtifactsConsumed(paths []string, consumedAt time.Time) int {
	if len(paths) == 0 {
		return 0
	}

	if consumedAt.IsZero() {
		consumedAt = time.Now().UTC()
	} else {
		consumedAt = consumedAt.UTC()
	}

	wanted := make(map[string]struct{}, len(paths))
	for _, path := range paths {
		if normalized := normalizeArtifactPath(path); normalized != "" {
			wanted[normalized] = struct{}{}
		}
	}

	if len(wanted) == 0 {
		return 0
	}

	marked := 0

	for i := range s.Artifacts {
		artifact := &s.Artifacts[i]
		if _, ok := wanted[normalizeArtifactPath(artifact.Path)]; !ok {
			continue
		}

		if artifact.ConsumedAt != nil && !artifact.ConsumedAt.IsZero() {
			continue
		}

		copied := consumedAt
		artifact.ConsumedAt = &copied
		marked++
	}

	return marked
}

// RecordArtifact appends a session artifact when the path and kind are valid.
func (s *Session) RecordArtifact(path, kind, summary, sourceAgent string) bool {
	return s.AddArtifact(Artifact{
		Path:        path,
		Kind:        kind,
		Summary:     summary,
		SourceAgent: sourceAgent,
	})
}

func normalizeArtifactPath(path string) string {
	path = strings.TrimSpace(path)
	if path == "" {
		return ""
	}

	return filepath.ToSlash(filepath.Clean(path))
}

// NewMultiAgentRun creates a new running multi-agent workflow record.
func NewMultiAgentRun(kind, prompt, model string, fallbackModels []string, budget MultiAgentRunBudget) MultiAgentRun {
	now := time.Now().UTC()
	id := newID(now)

	return MultiAgentRun{
		ID:             id,
		ReceiptID:      id,
		Kind:           strings.TrimSpace(kind),
		Status:         MultiAgentRunStatusRunning,
		Prompt:         strings.TrimSpace(prompt),
		Model:          strings.TrimSpace(model),
		FallbackModels: append([]string(nil), fallbackModels...),
		Budget:         budget,
		StartedAt:      now,
		UpdatedAt:      now,
	}
}

// UpsertMultiAgentRun records or replaces a durable multi-agent run by ID.
func (s *Session) UpsertMultiAgentRun(run MultiAgentRun) bool {
	run.ID = strings.TrimSpace(run.ID)
	if run.ID == "" {
		return false
	}

	if strings.TrimSpace(run.ReceiptID) == "" {
		run.ReceiptID = run.ID
	}

	if run.StartedAt.IsZero() {
		run.StartedAt = time.Now().UTC()
	}

	run.UpdatedAt = time.Now().UTC()

	for i := range s.MultiAgentRuns {
		if s.MultiAgentRuns[i].ID == run.ID {
			s.MultiAgentRuns[i] = run
			return true
		}
	}

	s.MultiAgentRuns = append(s.MultiAgentRuns, run)

	return true
}

// FindMultiAgentRun returns a run by ID, or the newest run when ref is "latest".
func (s *Session) FindMultiAgentRun(ref string) (MultiAgentRun, bool) {
	if s == nil {
		return MultiAgentRun{}, false
	}

	ref = strings.TrimSpace(ref)
	if ref == "" {
		return MultiAgentRun{}, false
	}

	if strings.EqualFold(ref, "latest") {
		return latestMultiAgentRun(s.MultiAgentRuns, "")
	}

	if strings.EqualFold(ref, MultiAgentRunKindReview) || strings.EqualFold(ref, MultiAgentRunKindSpeculation) {
		return latestMultiAgentRun(s.MultiAgentRuns, ref)
	}

	for i := range s.MultiAgentRuns {
		if s.MultiAgentRuns[i].ID == ref || s.MultiAgentRuns[i].ReceiptID == ref {
			return s.MultiAgentRuns[i], true
		}
	}

	return MultiAgentRun{}, false
}

func latestMultiAgentRun(runs []MultiAgentRun, kind string) (MultiAgentRun, bool) {
	var latest MultiAgentRun

	found := false

	for i := range runs {
		run := &runs[i]
		if kind != "" && !strings.EqualFold(run.Kind, kind) {
			continue
		}

		if !found || !multiAgentRunSortTime(*run).Before(multiAgentRunSortTime(latest)) {
			latest = *run
			found = true
		}
	}

	return latest, found
}

func multiAgentRunSortTime(run MultiAgentRun) time.Time {
	switch {
	case !run.UpdatedAt.IsZero():
		return run.UpdatedAt
	case run.CompletedAt != nil && !run.CompletedAt.IsZero():
		return *run.CompletedAt
	default:
		return run.StartedAt
	}
}

func multiAgentRunHasAcceptedOutput(run MultiAgentRun) bool {
	if run.Status != MultiAgentRunStatusCompleted {
		return false
	}

	artifact, ok := acceptedMultiAgentRunArtifact(run)
	if !ok {
		return false
	}

	return strings.TrimSpace(artifact.Content) != ""
}

func acceptedMultiAgentRunArtifact(run MultiAgentRun) (MultiAgentRunArtifact, bool) {
	accepted, ok := firstAcceptedMultiAgentRunDecision(run.Decisions)
	if !ok {
		return MultiAgentRunArtifact{}, false
	}

	var legacyMatch MultiAgentRunArtifact

	foundLegacyMatch := false

	for i := range run.Artifacts {
		artifact := run.Artifacts[i]
		if !multiAgentRunArtifactMatchesDecision(artifact, accepted) {
			continue
		}

		if accepted.Index > 0 && artifact.Index == 0 {
			legacyMatch = artifact
			foundLegacyMatch = true

			continue
		}

		if accepted.Index > 0 && artifact.Index != accepted.Index {
			continue
		}

		return artifact, true
	}

	return legacyMatch, foundLegacyMatch
}

func firstAcceptedMultiAgentRunDecision(decisions []MultiAgentRunDecision) (MultiAgentRunDecision, bool) {
	for _, decision := range decisions {
		if decision.Kind == multiAgentRunArtifactKindVerdict && decision.Outcome == multiAgentRunDecisionOutcomeAccepted {
			return decision, true
		}
	}

	return MultiAgentRunDecision{}, false
}

func multiAgentRunArtifactMatchesDecision(
	artifact MultiAgentRunArtifact,
	decision MultiAgentRunDecision,
) bool {
	if artifact.Kind != multiAgentRunArtifactKindVerdict {
		return false
	}

	if decision.Phase != "" && artifact.Phase != decision.Phase {
		return false
	}

	if decision.Agent != "" && artifact.Agent != decision.Agent {
		return false
	}

	if decision.TargetAgent != "" && artifact.TargetAgent != decision.TargetAgent {
		return false
	}

	return true
}

func (s *Store) path(ref string) string {
	if ref == "" {
		return ""
	}

	if filepath.IsAbs(ref) || strings.ContainsRune(ref, rune(os.PathSeparator)) {
		return ref
	}

	if strings.HasSuffix(ref, sessionFileExt) {
		return filepath.Join(s.dir, ref)
	}

	return filepath.Join(s.dir, ref+sessionFileExt)
}

func newID(now time.Time) string {
	var suffix [4]byte
	if _, err := rand.Read(suffix[:]); err != nil {
		return now.Format("20060102-150405")
	}

	return now.Format("20060102-150405") + "-" + hex.EncodeToString(suffix[:])
}

func idFromPath(path string) string {
	base := filepath.Base(path)
	return strings.TrimSuffix(base, filepath.Ext(base))
}

func readSummary(path string) (Summary, error) {
	reader, err := os.Open(path)
	if err != nil {
		return Summary{}, fmt.Errorf("session: read %s: %w", path, err)
	}
	defer reader.Close()

	decoder := json.NewDecoder(reader)

	file, err := readSummaryFile(decoder)
	if err != nil {
		return Summary{}, fmt.Errorf("session: parse %s: %w", path, err)
	}

	if file.ID == "" {
		file.ID = idFromPath(path)
	}

	return summarizeFile(path, file), nil
}

func summarizeFile(path string, file sessionSummaryFile) Summary {
	return Summary{
		ID:              file.ID,
		Title:           file.Title,
		Path:            path,
		CreatedAt:       file.CreatedAt,
		UpdatedAt:       file.UpdatedAt,
		AgentNames:      appendSummaryAgentNames([]string{file.DefaultAgent}, file.AgentNames...),
		DefaultModel:    file.DefaultModel,
		DefaultAgent:    file.DefaultAgent,
		Autonomy:        file.Autonomy,
		AgentLoopBudget: file.AgentLoopBudget,
		WorktreePath:    file.WorktreePath,
		Tags:            append([]string(nil), file.Tags...),
		Messages:        file.Messages,
	}
}

func readSummaryFile(decoder *json.Decoder) (sessionSummaryFile, error) {
	token, err := decoder.Token()
	if err != nil {
		return sessionSummaryFile{}, fmt.Errorf("read summary object: %w", err)
	}

	delim, ok := token.(json.Delim)
	if !ok || delim != '{' {
		return sessionSummaryFile{}, errors.New("read summary object: expected object")
	}

	var file sessionSummaryFile

	for decoder.More() {
		key, keyErr := readSummaryObjectKey(decoder)
		if keyErr != nil {
			return sessionSummaryFile{}, keyErr
		}

		if valueErr := readSummaryObjectValue(decoder, key, &file); valueErr != nil {
			return sessionSummaryFile{}, valueErr
		}
	}

	if err := expectSummaryJSONDelim(decoder, '}'); err != nil {
		return sessionSummaryFile{}, err
	}

	if err := expectSummaryJSONEOF(decoder); err != nil {
		return sessionSummaryFile{}, err
	}

	return file, nil
}

func readSummaryObjectKey(decoder *json.Decoder) (string, error) {
	token, err := decoder.Token()
	if err != nil {
		return "", fmt.Errorf("read summary key: %w", err)
	}

	key, ok := token.(string)
	if !ok {
		return "", errors.New("read summary key: expected string")
	}

	return key, nil
}

func readSummaryObjectValue(decoder *json.Decoder, key string, file *sessionSummaryFile) error {
	if agentField, ok := summaryAgentField(key); ok {
		return readSummaryObjectAgents(decoder, file, agentField)
	}

	switch key {
	case "id":
		return decodeSummaryJSONValue(decoder, key, &file.ID)
	case "title":
		return decodeSummaryJSONValue(decoder, key, &file.Title)
	case "default_model":
		return decodeSummaryJSONValue(decoder, key, &file.DefaultModel)
	case "default_agent":
		return decodeSummaryJSONValue(decoder, key, &file.DefaultAgent)
	case "autonomy":
		return decodeSummaryJSONValue(decoder, key, &file.Autonomy)
	case "agent_loop_budget":
		return decodeSummaryJSONValue(decoder, key, &file.AgentLoopBudget)
	case "worktree_path":
		return decodeSummaryJSONValue(decoder, key, &file.WorktreePath)
	case "created_at":
		return decodeSummaryJSONValue(decoder, key, &file.CreatedAt)
	case "updated_at":
		return decodeSummaryJSONValue(decoder, key, &file.UpdatedAt)
	case "tags":
		return decodeSummaryJSONValue(decoder, key, &file.Tags)
	case "messages":
		count, err := countSummaryMessages(decoder)
		if err != nil {
			return err
		}

		file.Messages = count

		return nil
	default:
		return skipSummaryJSONValue(decoder)
	}
}

func summaryAgentField(key string) (string, bool) {
	switch key {
	case "negative_knowledge", "evaluations":
		return "agent", true
	case "artifacts":
		return "source_agent", true
	default:
		return "", false
	}
}

func readSummaryObjectAgents(decoder *json.Decoder, file *sessionSummaryFile, agentField string) error {
	agents, err := readSummaryAgentArray(decoder, agentField)
	if err != nil {
		return err
	}

	file.AgentNames = appendSummaryAgentNames(file.AgentNames, agents...)

	return nil
}

func summaryAgentNames(values ...string) []string {
	seen := make(map[string]string, len(values))
	for _, value := range values {
		value = strings.TrimSpace(value)
		if value == "" {
			continue
		}

		key := strings.ToLower(value)
		if _, ok := seen[key]; ok {
			continue
		}

		seen[key] = value
	}

	out := make([]string, 0, len(seen))
	for _, value := range seen {
		out = append(out, value)
	}

	sort.Slice(out, func(i, j int) bool {
		return strings.ToLower(out[i]) < strings.ToLower(out[j])
	})

	return out
}

func appendSummaryAgentNames(existing []string, values ...string) []string {
	merged := make([]string, 0, len(existing)+len(values))
	merged = append(merged, existing...)
	merged = append(merged, values...)

	return summaryAgentNames(merged...)
}

func readSummaryAgentArray(decoder *json.Decoder, agentField string) ([]string, error) {
	token, err := decoder.Token()
	if err != nil {
		return nil, fmt.Errorf("read summary agents: %w", err)
	}

	if token == nil {
		return nil, nil
	}

	delim, ok := token.(json.Delim)
	if !ok || delim != '[' {
		if err := skipSummaryJSONValueAfterToken(decoder, token); err != nil {
			return nil, err
		}

		return nil, nil
	}

	var agents []string

	for decoder.More() {
		values, err := readSummaryAgentObject(decoder, agentField)
		if err != nil {
			return nil, err
		}

		agents = appendSummaryAgentNames(agents, values...)
	}

	if err := expectSummaryJSONDelim(decoder, ']'); err != nil {
		return nil, err
	}

	return agents, nil
}

func readSummaryAgentObject(decoder *json.Decoder, agentField string) ([]string, error) {
	token, err := decoder.Token()
	if err != nil {
		return nil, fmt.Errorf("read summary agent object: %w", err)
	}

	delim, ok := token.(json.Delim)
	if !ok || delim != '{' {
		return nil, skipSummaryJSONValueAfterToken(decoder, token)
	}

	var agents []string

	for decoder.More() {
		key, err := readSummaryObjectKey(decoder)
		if err != nil {
			return nil, err
		}

		if key != agentField {
			if skipErr := skipSummaryJSONValue(decoder); skipErr != nil {
				return nil, skipErr
			}

			continue
		}

		token, tokenErr := decoder.Token()
		if tokenErr != nil {
			return nil, fmt.Errorf("read summary agent %s: %w", agentField, tokenErr)
		}

		agent, ok := token.(string)
		if !ok {
			if skipErr := skipSummaryJSONValueAfterToken(decoder, token); skipErr != nil {
				return nil, skipErr
			}

			continue
		}

		agents = appendSummaryAgentNames(agents, agent)
	}

	if err := expectSummaryJSONDelim(decoder, '}'); err != nil {
		return nil, err
	}

	return agents, nil
}

func decodeSummaryJSONValue(decoder *json.Decoder, key string, target any) error {
	if err := decoder.Decode(target); err != nil {
		return fmt.Errorf("read summary %s: %w", key, err)
	}

	return nil
}

func countSummaryMessages(decoder *json.Decoder) (int, error) {
	token, err := decoder.Token()
	if err != nil {
		return 0, fmt.Errorf("read messages: %w", err)
	}

	if token == nil {
		return 0, nil
	}

	delim, ok := token.(json.Delim)
	if !ok || delim != '[' {
		return 0, errors.New("read messages: expected array")
	}

	count := 0

	for decoder.More() {
		if err := skipSummaryJSONValue(decoder); err != nil {
			return 0, err
		}

		count++
	}

	if err := expectSummaryJSONDelim(decoder, ']'); err != nil {
		return 0, err
	}

	return count, nil
}

func skipSummaryJSONValue(decoder *json.Decoder) error {
	token, err := decoder.Token()
	if err != nil {
		return fmt.Errorf("skip message value: %w", err)
	}

	return skipSummaryJSONValueAfterToken(decoder, token)
}

//nolint:wsl_v5 // Recursive token skipping is intentionally compact.
func skipSummaryJSONValueAfterToken(decoder *json.Decoder, token json.Token) error {
	delim, ok := token.(json.Delim)
	if !ok {
		return nil
	}

	switch delim {
	case '{':
		for decoder.More() {
			if _, err := decoder.Token(); err != nil {
				return fmt.Errorf("skip message key: %w", err)
			}
			if err := skipSummaryJSONValue(decoder); err != nil {
				return err
			}
		}

		return expectSummaryJSONDelim(decoder, '}')
	case '[':
		for decoder.More() {
			if err := skipSummaryJSONValue(decoder); err != nil {
				return err
			}
		}

		return expectSummaryJSONDelim(decoder, ']')
	default:
		return fmt.Errorf("skip message value: unexpected delimiter %q", delim)
	}
}

func expectSummaryJSONDelim(decoder *json.Decoder, want json.Delim) error {
	token, err := decoder.Token()
	if err != nil {
		return fmt.Errorf("skip message delimiter: %w", err)
	}

	got, ok := token.(json.Delim)
	if !ok || got != want {
		return fmt.Errorf("skip message delimiter: expected %q", want)
	}

	return nil
}

func expectSummaryJSONEOF(decoder *json.Decoder) error {
	token, err := decoder.Token()
	if errors.Is(err, io.EOF) {
		return nil
	}

	if err != nil {
		return fmt.Errorf("read summary trailing data: %w", err)
	}

	return fmt.Errorf("read summary trailing data: unexpected token %v", token)
}

func summarize(path string, session Session) Summary {
	return Summary{
		ID:             session.ID,
		Title:          session.Title,
		Path:           path,
		CreatedAt:      session.CreatedAt,
		UpdatedAt:      session.UpdatedAt,
		DefaultModel:   session.DefaultModel,
		DefaultAgent:   session.DefaultAgent,
		WorktreePath:   session.WorktreePath,
		WorktreeBranch: session.WorktreeBranch,
		WorktreeBase:   session.WorktreeBase,
		Tags:           append([]string(nil), session.Tags...),
		Messages:       len(session.Messages),
	}
}
