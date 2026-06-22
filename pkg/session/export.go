package session

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"html"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/tommoulard/atteler/pkg/llm"
)

// ExportProfile controls how much session data is included and whether it is redacted.
type ExportProfile string

const (
	// ExportProfileShareable is the default safe-to-share export profile.
	ExportProfileShareable ExportProfile = "redacted-shareable"
	// ExportProfilePrivate preserves full-fidelity content and is explicitly marked private.
	ExportProfilePrivate ExportProfile = "private-full"
	// ExportProfileIssue renders an issue/PR-ready summary without transcript bodies.
	ExportProfileIssue ExportProfile = "issue-pr-summary"
)

const (
	// DefaultMaxContentRunes limits each exported untrusted text field in safe profiles.
	DefaultMaxContentRunes = 12_000
	// DefaultIssueMaxContentRunes keeps summary exports compact.
	DefaultIssueMaxContentRunes = 2_000
	// DefaultMaxTranscriptMessages limits safe transcript exports to a reviewable size.
	DefaultMaxTranscriptMessages = 200
)

// ExportOptions configures Markdown and machine-readable session exports.
type ExportOptions struct {
	// ExportedAt overrides the manifest export time. Zero uses time.Now().UTC().
	ExportedAt time.Time
	// Profile selects the export redaction/omission behavior. Zero uses ExportProfileShareable.
	Profile ExportProfile
	// ExcludedFields omits field families from exports, such as SearchFieldTranscript or SearchFieldFailures.
	ExcludedFields []SearchField
	// SensitiveFields adds field names to redact, such as "tenant_secret".
	SensitiveFields []string
	// MaxContentRunes limits each text field. Zero uses the profile default; negative disables the limit.
	MaxContentRunes int
	// MaxTranscriptMessages limits exported transcript messages. Zero uses the profile default; negative disables the limit.
	MaxTranscriptMessages int
}

// ExportManifest records how an export was produced so reviewers can reason about provenance.
type ExportManifest struct {
	ExportedAt       time.Time         `json:"exported_at,omitzero"`
	ContentHashes    map[string]string `json:"content_hashes"`
	SessionID        string            `json:"session_id"`
	RedactionProfile ExportProfile     `json:"redaction_profile"`
	PrivacyNotice    string            `json:"privacy_notice,omitempty"`
	OmittedSections  []string          `json:"omitted_sections"`
}

// MachineReadableExport is the structured, redaction-aware session export payload.
//
//nolint:govet // JSON field order keeps provenance first; padding is not performance-sensitive.
type MachineReadableExport struct {
	Manifest          ExportManifest            `json:"manifest"`
	Session           ExportSessionMetadata     `json:"session"`
	Provenance        ExportProvenance          `json:"provenance"`
	NegativeKnowledge []ExportNegativeKnowledge `json:"negative_knowledge,omitempty"`
	Evaluations       []ExportAgentEvaluation   `json:"evaluations,omitempty"`
	Artifacts         []ExportArtifact          `json:"artifacts,omitempty"`
	MultiAgentRuns    []ExportMultiAgentRun     `json:"multi_agent_runs,omitempty"`
	Messages          []ExportMessage           `json:"messages,omitempty"`
}

// ExportSessionMetadata contains safe session-level metadata for exports.
//
//nolint:govet // JSON field order keeps stable export metadata; padding is not performance-sensitive.
type ExportSessionMetadata struct {
	CreatedAt              time.Time           `json:"created_at,omitzero"`
	UpdatedAt              time.Time           `json:"updated_at,omitzero"`
	ID                     string              `json:"id"`
	Title                  string              `json:"title,omitempty"`
	DefaultModel           string              `json:"default_model,omitempty"`
	DefaultReasoningLevel  string              `json:"default_reasoning_level,omitempty"`
	DefaultModelMode       string              `json:"default_model_mode,omitempty"`
	DefaultAgent           string              `json:"default_agent,omitempty"`
	Autonomy               string              `json:"autonomy,omitempty"`
	AgentLoopBudget        llm.AgentLoopBudget `json:"agent_loop_budget,omitzero"`
	WorktreePath           string              `json:"worktree_path,omitempty"`
	WorktreeBranch         string              `json:"worktree_branch,omitempty"`
	WorktreeBase           string              `json:"worktree_base,omitempty"`
	Tags                   []string            `json:"tags,omitempty"`
	MessageCount           int                 `json:"message_count"`
	ExportedMessageCount   int                 `json:"exported_message_count"`
	NegativeKnowledgeCount int                 `json:"negative_knowledge_count"`
	EvaluationCount        int                 `json:"evaluation_count"`
	ArtifactCount          int                 `json:"artifact_count"`
	MultiAgentRunCount     int                 `json:"multi_agent_run_count"`
	SchemaVersion          int                 `json:"schema_version,omitempty"`
	EventSchemaVersion     int                 `json:"event_schema_version,omitempty"`
}

// ExportProvenance summarizes the replay inputs that explain how a session
// result was produced without requiring consumers to parse the full transcript.
//
//nolint:govet // JSON field order keeps high-level provenance before lists.
type ExportProvenance struct {
	EventLog          *ExportEventLogProvenance `json:"event_log,omitempty"`
	TokenUsage        ExportTokenUsage          `json:"token_usage,omitzero"`
	ConfigHash        string                    `json:"config_hash"`
	Providers         []string                  `json:"providers,omitempty"`
	Models            []string                  `json:"models,omitempty"`
	ProviderCalls     []ExportProviderCall      `json:"provider_calls,omitempty"`
	ReferencedFiles   []ExportFileReference     `json:"referenced_files,omitempty"`
	VerificationGates []ExportVerificationGate  `json:"verification_gates,omitempty"`
}

// ExportEventLogProvenance identifies the append-only log backing an export.
type ExportEventLogProvenance struct {
	Path          string `json:"path,omitempty"`
	LastHash      string `json:"last_hash,omitempty"`
	SchemaVersion int    `json:"schema_version,omitempty"`
	EventCount    int    `json:"event_count,omitempty"`
	LastSequence  int64  `json:"last_sequence,omitempty"`
	TruncatedTail bool   `json:"truncated_tail,omitempty"`
}

// ExportTokenUsage aggregates observed and estimated token/cost provenance.
type ExportTokenUsage struct {
	ModelCalls            int   `json:"model_calls,omitempty"`
	InputTokens           int   `json:"input_tokens,omitempty"`
	CachedInputTokens     int   `json:"cached_input_tokens,omitempty"`
	CacheWriteInputTokens int   `json:"cache_write_input_tokens,omitempty"`
	OutputTokens          int   `json:"output_tokens,omitempty"`
	TotalTokens           int   `json:"total_tokens,omitempty"`
	EstimatedInputTokens  int   `json:"estimated_input_tokens,omitempty"`
	EstimatedOutputTokens int   `json:"estimated_output_tokens,omitempty"`
	EstimatedTotalTokens  int   `json:"estimated_total_tokens,omitempty"`
	EstimatedCostMicros   int64 `json:"estimated_cost_micros,omitempty"`
}

// ExportProviderCall records one provider request/response pair that influenced replay.
//
// Raw request messages are included only for private full-fidelity exports; safe
// profiles keep prompt/config/response hashes plus counts so consumers can still
// correlate the export with the append-only audit log without leaking injected
// file or tool-result content.
type ExportProviderCall struct {
	StartedAt               time.Time             `json:"started_at,omitzero"`
	CompletedAt             time.Time             `json:"completed_at,omitzero"`
	ID                      string                `json:"id"`
	Source                  string                `json:"source,omitempty"`
	Provider                string                `json:"provider,omitempty"`
	RequestedModel          string                `json:"requested_model,omitempty"`
	ResponseModel           string                `json:"response_model,omitempty"`
	ModelMode               string                `json:"model_mode,omitempty"`
	ReasoningLevel          string                `json:"reasoning_level,omitempty"`
	PromptHash              string                `json:"prompt_hash,omitempty"`
	ConfigHash              string                `json:"config_hash,omitempty"`
	ResponseHash            string                `json:"response_hash,omitempty"`
	StopReason              llm.StopReason        `json:"stop_reason,omitempty"`
	RequestMessages         []ExportMessage       `json:"request_messages,omitempty"`
	FallbackModels          []string              `json:"fallback_models,omitempty"`
	ReferencedFiles         []ExportFileReference `json:"referenced_files,omitempty"`
	InputTokens             int                   `json:"input_tokens,omitempty"`
	CachedInputTokens       int                   `json:"cached_input_tokens,omitempty"`
	CacheWriteInputTokens   int                   `json:"cache_write_input_tokens,omitempty"`
	OutputTokens            int                   `json:"output_tokens,omitempty"`
	TotalTokens             int                   `json:"total_tokens,omitempty"`
	EstimatedInputTokens    int                   `json:"estimated_input_tokens,omitempty"`
	EstimatedOutputTokens   int                   `json:"estimated_output_tokens,omitempty"`
	MaxOutputTokens         int                   `json:"max_output_tokens,omitempty"`
	RequestMessageCount     int                   `json:"request_message_count,omitempty"`
	RequestToolCount        int                   `json:"request_tool_count,omitempty"`
	RequestToolCallCount    int                   `json:"request_tool_call_count,omitempty"`
	RequestToolResultCount  int                   `json:"request_tool_result_count,omitempty"`
	ResponseToolCallCount   int                   `json:"response_tool_call_count,omitempty"`
	DurationMillis          int64                 `json:"duration_millis,omitempty"`
	FirstTokenLatencyMillis int64                 `json:"first_token_latency_millis,omitempty"`
}

// ExportFileReference records a file or artifact path that influenced replay.
//
//nolint:govet // JSON field order keeps path identity before provenance metadata.
type ExportFileReference struct {
	Path            string `json:"path"`
	LogicalPath     string `json:"logical_path,omitempty"`
	Kind            string `json:"kind,omitempty"`
	Source          string `json:"source,omitempty"`
	SourceAgent     string `json:"source_agent,omitempty"`
	SourceSessionID string `json:"source_session_id,omitempty"`
	SHA256          string `json:"sha256,omitempty"`
	SizeBytes       int64  `json:"size_bytes,omitempty"`
	WorktreeBranch  string `json:"worktree_branch,omitempty"`
	WorktreeBase    string `json:"worktree_base,omitempty"`
}

// ExportVerificationGate records a replay-affecting gate decision.
type ExportVerificationGate struct {
	RunID  string `json:"run_id,omitempty"`
	Name   string `json:"name"`
	Phase  string `json:"phase,omitempty"`
	Agent  string `json:"agent,omitempty"`
	Notes  string `json:"notes,omitempty"`
	Passed bool   `json:"passed"`
}

// ExportMessage is a redaction-aware exported transcript message.
type ExportMessage struct {
	ToolResult        *llm.ToolResult `json:"tool_result,omitempty"`
	Role              llm.Role        `json:"role"`
	Content           string          `json:"content"`
	ToolCalls         []llm.ToolCall  `json:"tool_calls,omitempty"`
	Index             int             `json:"index"`
	ToolCallCount     int             `json:"tool_call_count,omitempty"`
	ToolResultOmitted bool            `json:"tool_result_omitted,omitempty"`
}

// ExportNegativeKnowledge is a redaction-aware exported failed approach record.
type ExportNegativeKnowledge struct {
	CreatedAt time.Time `json:"created_at,omitzero"`
	Approach  string    `json:"approach"`
	Reason    string    `json:"reason,omitempty"`
	Commit    string    `json:"commit,omitempty"`
	Agent     string    `json:"agent,omitempty"`
	TaskType  string    `json:"task_type,omitempty"`
	Severity  string    `json:"severity,omitempty"`
}

// ExportAgentEvaluation is a redaction-aware exported evaluation record.
//
//nolint:govet // JSON field order keeps exported evaluation metadata readable and stable.
type ExportAgentEvaluation struct {
	CreatedAt       time.Time `json:"created_at,omitzero"`
	Agent           string    `json:"agent"`
	Outcome         string    `json:"outcome,omitempty"`
	Notes           string    `json:"notes,omitempty"`
	Reference       string    `json:"reference,omitempty"`
	Source          string    `json:"source,omitempty"`
	Evaluator       string    `json:"evaluator,omitempty"`
	RubricVersion   string    `json:"rubric_version,omitempty"`
	TaskType        string    `json:"task_type,omitempty"`
	Difficulty      string    `json:"difficulty,omitempty"`
	ExpectedOutcome string    `json:"expected_outcome,omitempty"`
	Provider        string    `json:"provider,omitempty"`
	Model           string    `json:"model,omitempty"`
	FixtureVersion  string    `json:"fixture_version,omitempty"`
	AgentVersion    string    `json:"agent_version,omitempty"`
	SchemaVersion   int       `json:"schema_version,omitempty"`
	Score           int       `json:"score,omitempty"`
	PassRate        *float64  `json:"pass_rate,omitempty"`
	FlakeCount      int       `json:"flake_count,omitempty"`
	DurationMillis  int64     `json:"duration_millis,omitempty"`
	InputTokens     int       `json:"input_tokens,omitempty"`
	OutputTokens    int       `json:"output_tokens,omitempty"`
	TotalTokens     int       `json:"total_tokens,omitempty"`
	Cost            float64   `json:"cost,omitempty"`
	Confidence      float64   `json:"confidence,omitempty"`
}

// ExportArtifact is a redaction-aware exported artifact record.
type ExportArtifact struct {
	CreatedAt       time.Time  `json:"created_at,omitzero"`
	ConsumedAt      *time.Time `json:"consumed_at,omitempty"`
	Path            string     `json:"path"`
	LogicalPath     string     `json:"logical_path,omitempty"`
	Kind            string     `json:"kind,omitempty"`
	Summary         string     `json:"summary,omitempty"`
	SourceAgent     string     `json:"source_agent,omitempty"`
	SourceSessionID string     `json:"source_session_id,omitempty"`
	SourceCommand   string     `json:"source_command,omitempty"`
	SourceTool      string     `json:"source_tool,omitempty"`
	SourceCommit    string     `json:"source_commit,omitempty"`
	WorktreePath    string     `json:"worktree_path,omitempty"`
	WorktreeBranch  string     `json:"worktree_branch,omitempty"`
	WorktreeBase    string     `json:"worktree_base,omitempty"`
	SHA256          string     `json:"sha256,omitempty"`
	ReviewStatus    string     `json:"review_status,omitempty"`
	SizeBytes       int64      `json:"size_bytes,omitempty"`
	SourceTurn      int        `json:"source_turn,omitempty"`
	WorktreeDirty   bool       `json:"worktree_dirty,omitempty"`
}

// ExportMultiAgentRun is a redaction-aware exported review/speculation run.
//
//nolint:govet // JSON field order mirrors session.MultiAgentRun for audit readability.
type ExportMultiAgentRun struct {
	StartedAt          time.Time                         `json:"started_at,omitzero"`
	UpdatedAt          time.Time                         `json:"updated_at,omitzero"`
	CompletedAt        *time.Time                        `json:"completed_at,omitempty"`
	ID                 string                            `json:"id"`
	ReceiptID          string                            `json:"receipt_id,omitempty"`
	Kind               string                            `json:"kind"`
	Status             MultiAgentRunStatus               `json:"status"`
	Prompt             string                            `json:"prompt,omitempty"`
	Model              string                            `json:"model,omitempty"`
	FallbackModels     []string                          `json:"fallback_models,omitempty"`
	Budget             MultiAgentRunBudget               `json:"budget,omitzero"`
	Usage              MultiAgentRunUsage                `json:"usage,omitzero"`
	Calls              []ExportMultiAgentRunCall         `json:"calls,omitempty"`
	Branches           []ExportMultiAgentRunBranch       `json:"branches,omitempty"`
	Reviewers          []ExportMultiAgentRunReviewer     `json:"reviewers,omitempty"`
	Artifacts          []ExportMultiAgentRunArtifact     `json:"artifacts,omitempty"`
	Gates              []ExportMultiAgentRunGate         `json:"gates,omitempty"`
	Disagreements      []ExportMultiAgentRunDisagreement `json:"disagreements,omitempty"`
	Errors             []ExportMultiAgentRunError        `json:"errors,omitempty"`
	Decisions          []ExportMultiAgentRunDecision     `json:"decisions,omitempty"`
	Summary            ExportMultiAgentRunSummary        `json:"summary,omitzero"`
	CancellationReason string                            `json:"cancellation_reason,omitempty"`
	ResumeReason       string                            `json:"resume_reason,omitempty"`
	Error              string                            `json:"error,omitempty"`
}

// ExportMultiAgentRunCall is a sanitized provider-call audit record.
//
//nolint:govet // JSON field order mirrors the durable call audit schema.
type ExportMultiAgentRunCall struct {
	StartedAt            time.Time           `json:"started_at,omitzero"`
	CompletedAt          *time.Time          `json:"completed_at,omitempty"`
	ID                   string              `json:"id"`
	Phase                string              `json:"phase"`
	Agent                string              `json:"agent,omitempty"`
	TargetAgent          string              `json:"target_agent,omitempty"`
	Status               MultiAgentRunStatus `json:"status"`
	RequestedModel       string              `json:"requested_model,omitempty"`
	ResponseModel        string              `json:"response_model,omitempty"`
	FallbackModels       []string            `json:"fallback_models,omitempty"`
	PromptHash           string              `json:"prompt_hash,omitempty"`
	SystemPrompt         string              `json:"system_prompt,omitempty"`
	UserPrompt           string              `json:"user_prompt,omitempty"`
	Response             string              `json:"response,omitempty"`
	Error                string              `json:"error,omitempty"`
	InputTokenEstimate   int                 `json:"input_token_estimate,omitempty"`
	OutputTokenEstimate  int                 `json:"output_token_estimate,omitempty"`
	ContextWindow        int                 `json:"context_window,omitempty"`
	MaxOutputTokens      int                 `json:"max_output_tokens,omitempty"`
	InputTokens          int                 `json:"input_tokens,omitempty"`
	CachedInputTokens    int                 `json:"cached_input_tokens,omitempty"`
	OutputTokens         int                 `json:"output_tokens,omitempty"`
	TotalTokens          int                 `json:"total_tokens,omitempty"`
	EstimatedCostMicros  int64               `json:"estimated_cost_micros,omitempty"`
	DurationMS           int64               `json:"duration_ms,omitempty"`
	BudgetRejectionRule  string              `json:"budget_rejection_rule,omitempty"`
	BudgetRejectionUsage int                 `json:"budget_rejection_usage,omitempty"`
	BudgetRejectionLimit int                 `json:"budget_rejection_limit,omitempty"`
}

// ExportMultiAgentRunBranch is a sanitized per-branch provenance and budget record.
//
//nolint:govet // JSON field order mirrors the durable branch audit schema.
type ExportMultiAgentRunBranch struct {
	Name                 string              `json:"name"`
	Role                 string              `json:"role,omitempty"`
	Provenance           string              `json:"provenance,omitempty"`
	Model                string              `json:"model,omitempty"`
	PromptHash           string              `json:"prompt_hash,omitempty"`
	Error                string              `json:"error,omitempty"`
	Status               MultiAgentRunStatus `json:"status"`
	InputTokenEstimate   int                 `json:"input_token_estimate,omitempty"`
	OutputTokenEstimate  int                 `json:"output_token_estimate,omitempty"`
	ContextWindow        int                 `json:"context_window,omitempty"`
	MaxOutputTokens      int                 `json:"max_output_tokens,omitempty"`
	InputTokens          int                 `json:"input_tokens,omitempty"`
	CachedInputTokens    int                 `json:"cached_input_tokens,omitempty"`
	OutputTokens         int                 `json:"output_tokens,omitempty"`
	TotalTokens          int                 `json:"total_tokens,omitempty"`
	EstimatedCostMicros  int64               `json:"estimated_cost_micros,omitempty"`
	DurationMS           int64               `json:"duration_ms,omitempty"`
	BudgetRejectionRule  string              `json:"budget_rejection_rule,omitempty"`
	BudgetRejectionUsage int                 `json:"budget_rejection_usage,omitempty"`
	BudgetRejectionLimit int                 `json:"budget_rejection_limit,omitempty"`
}

// ExportMultiAgentRunReviewer is a sanitized reviewer role invocation record.
type ExportMultiAgentRunReviewer struct {
	Name        string `json:"name"`
	Role        string `json:"role,omitempty"`
	TargetAgent string `json:"target_agent,omitempty"`
	Model       string `json:"model,omitempty"`
	PromptHash  string `json:"prompt_hash,omitempty"`
	CallID      string `json:"call_id,omitempty"`
}

// ExportMultiAgentRunArtifact is a sanitized workflow artifact.
type ExportMultiAgentRunArtifact struct {
	CreatedAt   time.Time         `json:"created_at,omitzero"`
	Metadata    map[string]string `json:"metadata,omitempty"`
	Kind        string            `json:"kind"`
	Phase       string            `json:"phase,omitempty"`
	Agent       string            `json:"agent,omitempty"`
	TargetAgent string            `json:"target_agent,omitempty"`
	Content     string            `json:"content,omitempty"`
	Index       int               `json:"index,omitempty"`
}

// ExportMultiAgentRunGate is a sanitized gate result.
type ExportMultiAgentRunGate struct {
	Name   string `json:"name"`
	Phase  string `json:"phase,omitempty"`
	Agent  string `json:"agent,omitempty"`
	Notes  string `json:"notes,omitempty"`
	Passed bool   `json:"passed"`
}

// ExportMultiAgentRunDisagreement is a sanitized cross-review or divergent-gate record.
type ExportMultiAgentRunDisagreement struct {
	Phase       string `json:"phase,omitempty"`
	Reviewer    string `json:"reviewer,omitempty"`
	TargetAgent string `json:"target_agent,omitempty"`
	Subject     string `json:"subject,omitempty"`
	Notes       string `json:"notes,omitempty"`
	Index       int    `json:"index,omitempty"`
}

// ExportMultiAgentRunError is a sanitized workflow-level error.
type ExportMultiAgentRunError struct {
	Stage       string `json:"stage,omitempty"`
	Reviewer    string `json:"reviewer,omitempty"`
	TargetAgent string `json:"target_agent,omitempty"`
	Message     string `json:"message"`
}

// ExportMultiAgentRunDecision is a sanitized accepted/rejected output decision.
type ExportMultiAgentRunDecision struct {
	Kind        string `json:"kind"`
	Phase       string `json:"phase,omitempty"`
	Agent       string `json:"agent,omitempty"`
	TargetAgent string `json:"target_agent,omitempty"`
	Outcome     string `json:"outcome"`
	Rationale   string `json:"rationale,omitempty"`
	Index       int    `json:"index,omitempty"`
}

// ExportMultiAgentRunSummary is a sanitized compact run result.
type ExportMultiAgentRunSummary struct {
	Winner          string `json:"winner,omitempty"`
	Reason          string `json:"reason,omitempty"`
	VerdictReviewer string `json:"verdict_reviewer,omitempty"`
	Findings        int    `json:"findings,omitempty"`
}

type normalizedExportOptions struct {
	exportedAt            time.Time
	profile               ExportProfile
	excludedFields        map[SearchField]struct{}
	sensitiveFields       []string
	maxContentRunes       int
	maxTranscriptMessages int
	redact                bool
}

type exportBuilder struct {
	omitSeen map[string]struct{}
	omitted  []string
	options  normalizedExportOptions
}

var (
	privateKeyBlockRE = regexp.MustCompile(`(?s)-----BEGIN [A-Z0-9 ]*PRIVATE KEY-----.*?-----END [A-Z0-9 ]*PRIVATE KEY-----`)
	urlCredentialsRE  = regexp.MustCompile(`(?i)\b([a-z][a-z0-9+.-]*://)[^/\s:@]+:[^@\s/]+@`)
	cookieHeaderRE    = regexp.MustCompile(`(?i)\b((?:set-cookie|cookie)\s*[:=]\s*)[^\r\n]+`)
	authorizationRE   = regexp.MustCompile(`(?i)\b(authorization\s*[:=]\s*)(?:bearer|basic|token)?\s*[A-Za-z0-9._~+/=-]{8,}`)
	bearerTokenRE     = regexp.MustCompile(`(?i)\bBearer\s+[A-Za-z0-9._~+/=-]{12,}`)
	openAIKeyRE       = regexp.MustCompile(`\bsk-(?:proj-)?[A-Za-z0-9_-]{8,}\b`)
	anthropicKeyRE    = regexp.MustCompile(`\bsk-ant-[A-Za-z0-9_-]{8,}\b`)
	githubTokenRE     = regexp.MustCompile(`\b(?:gh[pousr]_[A-Za-z0-9_]{20,}|github_pat_[A-Za-z0-9_]{20,})\b`)
	slackTokenRE      = regexp.MustCompile(`\bxox[baprs]-[A-Za-z0-9-]{10,}\b`)
	awsAccessKeyRE    = regexp.MustCompile(`\b(?:AKIA|ASIA)[A-Z0-9]{16}\b`)
	jwtRE             = regexp.MustCompile(`\beyJ[A-Za-z0-9_-]{10,}\.[A-Za-z0-9_-]{10,}\.[A-Za-z0-9_-]{10,}\b`)
	fileURIPathRE     = regexp.MustCompile(`(?i)\bfile:///(?:[^ \t\r\n"'<>)}\],;]+/)+[^ \t\r\n"'<>)}\],;]*`)
	quotedPathFieldRE = regexp.MustCompile(`(?i)\b((?:path|file|dir|directory|cwd|worktree|root)\s*[:=]\s*["'])(?:/[^"'\r\n]+|~[/\\][^"'\r\n]+|[A-Z]:\\[^"'\r\n]+|\\\\[^"'\r\n]+)(["'])`)
	posixPathFieldRE  = regexp.MustCompile(`(?i)\b((?:path|file|dir|directory|cwd|worktree|root)\s*[:=]\s*)(/[^ \t\r\n"'<>)}\],;]+(?:/[^ \t\r\n"'<>)}\],;]*)*)`)
	tildePathRE       = regexp.MustCompile(`(^|[\s"'({\[=])(~[/\\][^ \t\r\n"'<>)}\],;]*(?:[/\\][^ \t\r\n"'<>)}\],;]*)*)`)
	uncAbsPathRE      = regexp.MustCompile(`\\\\[^ \t\r\n"'<>|\\]+\\[^ \t\r\n"'<>|\\]+(?:\\[^ \t\r\n"'<>|\\]+)*`)
	windowsAbsPathRE  = regexp.MustCompile(`(?i)\b[A-Z]:\\[^ \t\r\n"'<>|]+(?:\\[^ \t\r\n"'<>|]+)*`)
	posixAbsPathRE    = regexp.MustCompile(`(^|[\s"'({\[=])(/[^ \t\r\n"'<>)}\],;]+(?:/[^ \t\r\n"'<>)}\],;]*)*)`)
)

var defaultSensitiveFieldPatterns = []string{
	`(?:[a-z0-9]+[-_])?api[-_]?key`,
	`apiKey`,
	`x[-_]?api[-_]?key`,
	`authorization`,
	`auth`,
	`bearer`,
	`token`,
	`(?:[a-z0-9]+[-_])*token(?:[-_][a-z0-9]+)*`,
	`access[-_]?token`,
	`refresh[-_]?token`,
	`id[-_]?token`,
	`password`,
	`(?:[a-z0-9]+[-_])*password(?:[-_][a-z0-9]+)*`,
	`passwd`,
	`secret`,
	`(?:[a-z0-9]+[-_])*secret(?:[-_][a-z0-9]+)*`,
	`client[-_]?secret`,
	`private[-_]?key`,
	`session[-_]?cookie`,
	`cookie`,
}

// Markdown renders a session using the default redacted shareable profile.
func Markdown(session Session) string {
	return MarkdownWithOptions(session, ExportOptions{Profile: ExportProfileShareable})
}

// PrivateMarkdown renders a full-fidelity Markdown export that is explicitly marked private.
func PrivateMarkdown(session Session) string {
	return MarkdownWithOptions(session, ExportOptions{Profile: ExportProfilePrivate})
}

// IssueMarkdown renders a compact issue/PR-ready Markdown summary without transcript bodies.
func IssueMarkdown(session Session) string {
	return MarkdownWithOptions(session, ExportOptions{Profile: ExportProfileIssue})
}

// MarkdownWithOptions renders a session according to the selected export profile.
func MarkdownWithOptions(session Session, options ExportOptions) string {
	export := BuildMachineReadableExport(session, options)
	return renderMarkdown(export)
}

// JSON renders a session as redacted machine-readable JSON.
func JSON(session Session) ([]byte, error) {
	return JSONWithOptions(session, ExportOptions{Profile: ExportProfileShareable})
}

// JSONWithOptions renders a session as machine-readable JSON using the selected profile.
func JSONWithOptions(session Session, options ExportOptions) ([]byte, error) {
	export := BuildMachineReadableExport(session, options)

	data, err := json.MarshalIndent(export, "", "  ")
	if err != nil {
		return nil, fmt.Errorf("session: marshal export json: %w", err)
	}

	return append(data, '\n'), nil
}

// BuildMachineReadableExport builds the structured payload shared by Markdown and JSON exports.
func BuildMachineReadableExport(session Session, options ExportOptions) MachineReadableExport {
	builder := &exportBuilder{options: normalizeExportOptions(options), omitSeen: make(map[string]struct{})}

	export := MachineReadableExport{
		Manifest: ExportManifest{
			SessionID:        builder.exportString("manifest.session_id", SearchFieldSession, fallback(session.ID, "untitled")),
			ExportedAt:       builder.exportTime("manifest.exported_at", builder.options.exportedAt),
			RedactionProfile: builder.options.profile,
			PrivacyNotice:    privacyNotice(builder.options),
		},
		Session: ExportSessionMetadata{
			ID:                     builder.exportString("session.id", SearchFieldSession, fallback(session.ID, "untitled")),
			Title:                  builder.exportString("session.title", SearchFieldTitle, session.Title),
			CreatedAt:              builder.exportTime("session.created_at", session.CreatedAt),
			UpdatedAt:              builder.exportTime("session.updated_at", session.UpdatedAt),
			DefaultAgent:           builder.exportString("session.default_agent", SearchFieldAgent, session.DefaultAgent),
			DefaultModel:           builder.exportString("session.default_model", SearchFieldModel, session.DefaultModel),
			DefaultReasoningLevel:  builder.exportString("session.default_reasoning_level", SearchFieldModel, session.DefaultReasoningLevel),
			DefaultModelMode:       builder.exportString("session.default_model_mode", SearchFieldModel, session.DefaultModelMode),
			Autonomy:               builder.exportString("session.autonomy", SearchFieldSession, session.Autonomy),
			AgentLoopBudget:        session.AgentLoopBudget,
			WorktreePath:           builder.exportString("session.worktree_path", SearchFieldRepo, session.WorktreePath),
			WorktreeBranch:         builder.exportString("session.worktree_branch", SearchFieldRepo, session.WorktreeBranch),
			WorktreeBase:           builder.exportString("session.worktree_base", SearchFieldRepo, session.WorktreeBase),
			Tags:                   builder.exportSlice("session.tags", SearchFieldTags, session.Tags),
			MessageCount:           len(session.Messages),
			NegativeKnowledgeCount: len(session.NegativeKnowledge),
			EvaluationCount:        len(session.Evaluations),
			ArtifactCount:          len(session.Artifacts),
			MultiAgentRunCount:     len(session.MultiAgentRuns),
			SchemaVersion:          normalizedSessionSchemaVersion(session.SchemaVersion),
			EventSchemaVersion:     SessionEventSchemaVersion,
		},
		Provenance: builder.exportProvenance(session),
	}

	export.NegativeKnowledge = builder.exportNegativeKnowledge(session.NegativeKnowledge)
	export.Evaluations = builder.exportEvaluations(session.Evaluations)
	export.Artifacts = builder.exportArtifacts(session.Artifacts)
	export.MultiAgentRuns = builder.exportMultiAgentRuns(session.MultiAgentRuns)
	export.Messages = builder.exportMessages(session.Messages)
	export.Session.ExportedMessageCount = len(export.Messages)

	export.Manifest.OmittedSections = append([]string{}, builder.omitted...)
	export.Manifest.ContentHashes = exportContentHashes(export)

	return export
}

func privacyNotice(options normalizedExportOptions) string {
	if options.profile != ExportProfilePrivate {
		return ""
	}

	if options.redact {
		return "Private export with sensitive-field redaction. Tool attachments are omitted to avoid leaking raw sensitive content."
	}

	return "Private full-fidelity export. Do not share unless recipients are allowed to see raw session content."
}

// ParseExportProfile maps user-facing profile names to export profiles.
func ParseExportProfile(value string) (ExportProfile, error) {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case "", "markdown", "md", "share", "shareable", "redacted", "redacted-shareable":
		return ExportProfileShareable, nil
	case "private", "private-full", "full", "raw", "raw-full":
		return ExportProfilePrivate, nil
	case "issue", "pr", "summary", "issue-pr", "issue-pr-summary":
		return ExportProfileIssue, nil
	default:
		return "", fmt.Errorf("unsupported session export profile %q", value)
	}
}

func normalizeExportOptions(options ExportOptions) normalizedExportOptions {
	profile, err := ParseExportProfile(string(options.Profile))
	if err != nil {
		profile = ExportProfileShareable
	}

	exportedAt := options.ExportedAt
	if exportedAt.IsZero() {
		exportedAt = time.Now().UTC()
	} else {
		exportedAt = exportedAt.UTC()
	}

	sensitiveFields := normalizeStringList(options.SensitiveFields)
	normalized := normalizedExportOptions{
		profile:         profile,
		exportedAt:      exportedAt,
		excludedFields:  normalizedExportFieldSet(options.ExcludedFields),
		sensitiveFields: sensitiveFields,
		redact:          profile != ExportProfilePrivate,
	}

	switch profile {
	case ExportProfilePrivate:
		normalized.maxContentRunes = -1
		normalized.maxTranscriptMessages = -1
	case ExportProfileIssue:
		normalized.maxContentRunes = DefaultIssueMaxContentRunes
		normalized.maxTranscriptMessages = 0
	default:
		normalized.profile = ExportProfileShareable
		normalized.redact = true
		normalized.maxContentRunes = DefaultMaxContentRunes
		normalized.maxTranscriptMessages = DefaultMaxTranscriptMessages
	}

	if options.MaxContentRunes != 0 {
		normalized.maxContentRunes = options.MaxContentRunes
	}

	if options.MaxTranscriptMessages != 0 {
		normalized.maxTranscriptMessages = options.MaxTranscriptMessages
	}

	if len(normalized.sensitiveFields) > 0 {
		normalized.redact = true
	}

	return normalized
}

func normalizedExportFieldSet(fields []SearchField) map[SearchField]struct{} {
	if len(fields) == 0 {
		return nil
	}

	set := make(map[SearchField]struct{}, len(fields))
	for _, field := range fields {
		if field == "" {
			continue
		}

		set[field] = struct{}{}
	}

	return set
}

func (builder *exportBuilder) exportNegativeKnowledge(entries []NegativeKnowledge) []ExportNegativeKnowledge {
	if len(entries) == 0 {
		return nil
	}

	if builder.excludes(SearchFieldFailures) {
		builder.omit("negative knowledge omitted by export field policy")

		return nil
	}

	exported := make([]ExportNegativeKnowledge, 0, len(entries))
	for index, entry := range entries {
		if entry.Approach == "" && entry.Reason == "" {
			continue
		}

		prefix := fmt.Sprintf("negative_knowledge[%d]", index+1)
		exported = append(exported, ExportNegativeKnowledge{
			CreatedAt: builder.exportTime(prefix+".created_at", entry.CreatedAt),
			Approach:  builder.exportString(prefix+".approach", SearchFieldFailures, entry.Approach),
			Reason:    builder.exportString(prefix+".reason", SearchFieldFailures, entry.Reason),
			Commit:    builder.exportString(prefix+".commit", SearchFieldFailures, entry.Commit),
			Agent:     builder.exportString(prefix+".agent", SearchFieldAgent, entry.Agent),
			TaskType:  builder.exportString(prefix+".task_type", SearchFieldFailures, entry.TaskType),
			Severity:  builder.exportString(prefix+".severity", SearchFieldFailures, entry.Severity),
		})
	}

	return exported
}

func (builder *exportBuilder) exportEvaluations(entries []AgentEvaluation) []ExportAgentEvaluation {
	if len(entries) == 0 {
		return nil
	}

	if builder.excludes(SearchFieldEvaluations) {
		builder.omit("evaluations omitted by export field policy")

		return nil
	}

	exported := make([]ExportAgentEvaluation, 0, len(entries))
	for index := range entries {
		entry := &entries[index]
		if entry.Agent == "" && entry.Outcome == "" {
			continue
		}

		prefix := fmt.Sprintf("evaluations[%d]", index+1)
		exported = append(exported, ExportAgentEvaluation{
			CreatedAt:       builder.exportTime(prefix+".created_at", entry.CreatedAt),
			Agent:           builder.exportString(prefix+".agent", SearchFieldAgent, entry.Agent),
			Outcome:         builder.exportString(prefix+".outcome", SearchFieldEvaluations, entry.Outcome),
			Notes:           builder.exportString(prefix+".notes", SearchFieldEvaluations, entry.Notes),
			Reference:       builder.exportString(prefix+".reference", SearchFieldEvaluations, entry.Reference),
			Source:          builder.exportString(prefix+".source", SearchFieldEvaluations, entry.Source),
			Evaluator:       builder.exportString(prefix+".evaluator", SearchFieldEvaluations, entry.Evaluator),
			RubricVersion:   builder.exportString(prefix+".rubric_version", SearchFieldEvaluations, entry.RubricVersion),
			TaskType:        builder.exportString(prefix+".task_type", SearchFieldEvaluations, entry.TaskType),
			Difficulty:      builder.exportString(prefix+".difficulty", SearchFieldEvaluations, entry.Difficulty),
			ExpectedOutcome: builder.exportString(prefix+".expected_outcome", SearchFieldEvaluations, entry.ExpectedOutcome),
			Provider:        builder.exportString(prefix+".provider", SearchFieldModel, entry.Provider),
			Model:           builder.exportString(prefix+".model", SearchFieldModel, entry.Model),
			FixtureVersion:  builder.exportString(prefix+".fixture_version", SearchFieldEvaluations, entry.FixtureVersion),
			AgentVersion:    builder.exportString(prefix+".agent_version", SearchFieldEvaluations, entry.AgentVersion),
			SchemaVersion:   entry.SchemaVersion,
			Score:           entry.Score,
			PassRate:        exportPassRate(entry),
			FlakeCount:      entry.FlakeCount,
			DurationMillis:  entry.DurationMillis,
			InputTokens:     entry.InputTokens,
			OutputTokens:    entry.OutputTokens,
			TotalTokens:     entry.TotalTokens,
			Cost:            entry.Cost,
			Confidence:      entry.Confidence,
		})
	}

	return exported
}

func exportPassRate(entry *AgentEvaluation) *float64 {
	if entry == nil || !entry.PassRateRecorded() {
		return nil
	}

	passRate := entry.PassRate

	return &passRate
}

func (builder *exportBuilder) exportArtifacts(entries []Artifact) []ExportArtifact {
	if len(entries) == 0 {
		return nil
	}

	if builder.excludes(SearchFieldArtifacts) {
		builder.omit("artifacts omitted by export field policy")

		return nil
	}

	exported := make([]ExportArtifact, 0, len(entries))
	for index := range entries {
		entry := &entries[index]

		if entry.Path == "" && entry.Kind == "" {
			continue
		}

		var consumedAt *time.Time

		if entry.ConsumedAt != nil && !entry.ConsumedAt.IsZero() {
			copied := entry.ConsumedAt.UTC()
			consumedAt = &copied
		}

		prefix := fmt.Sprintf("artifacts[%d]", index+1)
		exported = append(exported, ExportArtifact{
			CreatedAt:       builder.exportTime(prefix+".created_at", entry.CreatedAt),
			ConsumedAt:      consumedAt,
			Path:            builder.exportString(prefix+".path", SearchFieldArtifacts, entry.Path),
			LogicalPath:     builder.exportString(prefix+".logical_path", SearchFieldArtifacts, entry.LogicalPath),
			Kind:            builder.exportString(prefix+".kind", SearchFieldArtifacts, entry.Kind),
			Summary:         builder.exportString(prefix+".summary", SearchFieldArtifacts, entry.Summary),
			SourceAgent:     builder.exportString(prefix+".source_agent", SearchFieldAgent, entry.SourceAgent),
			SourceSessionID: builder.exportString(prefix+".source_session_id", SearchFieldSession, entry.SourceSessionID),
			SourceCommand:   builder.exportString(prefix+".source_command", SearchFieldArtifacts, entry.SourceCommand),
			SourceTool:      builder.exportString(prefix+".source_tool", SearchFieldArtifacts, entry.SourceTool),
			SourceCommit:    builder.exportString(prefix+".source_commit", SearchFieldArtifacts, entry.SourceCommit),
			WorktreePath:    builder.exportString(prefix+".worktree_path", SearchFieldRepo, entry.WorktreePath),
			WorktreeBranch:  builder.exportString(prefix+".worktree_branch", SearchFieldRepo, entry.WorktreeBranch),
			WorktreeBase:    builder.exportString(prefix+".worktree_base", SearchFieldRepo, entry.WorktreeBase),
			SHA256:          builder.exportString(prefix+".sha256", SearchFieldArtifacts, entry.SHA256),
			ReviewStatus:    builder.exportString(prefix+".review_status", SearchFieldArtifacts, entry.ReviewStatus),
			SizeBytes:       entry.SizeBytes,
			SourceTurn:      entry.SourceTurn,
			WorktreeDirty:   entry.WorktreeDirty,
		})
	}

	return exported
}

func (builder *exportBuilder) exportMultiAgentRuns(entries []MultiAgentRun) []ExportMultiAgentRun {
	if len(entries) == 0 {
		return nil
	}

	exported := make([]ExportMultiAgentRun, 0, len(entries))
	for index := range entries {
		entry := entries[index]
		if entry.ID == "" && entry.Kind == "" {
			continue
		}

		prefix := fmt.Sprintf("multi_agent_runs[%d]", index+1)
		summary := builder.exportMultiAgentRunSummary(prefix, entry)
		exported = append(exported, ExportMultiAgentRun{
			StartedAt:   entry.StartedAt,
			UpdatedAt:   entry.UpdatedAt,
			CompletedAt: entry.CompletedAt,
			ID:          builder.sanitize(prefix+".id", entry.ID),
			ReceiptID:   builder.sanitize(prefix+".receipt_id", entry.ReceiptID),
			Kind:        builder.sanitize(prefix+".kind", entry.Kind),
			Status:      entry.Status,
			Prompt: builder.sanitizeIssueOmittable(
				prefix+".prompt",
				entry.Prompt,
				"multi-agent run prompts omitted by issue/PR summary profile",
			),
			Model:              builder.sanitize(prefix+".model", entry.Model),
			FallbackModels:     builder.sanitizeSlice(prefix+".fallback_models", entry.FallbackModels),
			Budget:             entry.Budget,
			Usage:              entry.Usage,
			Calls:              builder.exportMultiAgentRunCalls(prefix, entry.Calls),
			Branches:           builder.exportMultiAgentRunBranches(prefix, entry.Branches),
			Reviewers:          builder.exportMultiAgentRunReviewers(prefix, entry.Reviewers),
			Artifacts:          builder.exportMultiAgentRunArtifacts(prefix, entry.Artifacts),
			Gates:              builder.exportMultiAgentRunGates(prefix, entry.Gates),
			Disagreements:      builder.exportMultiAgentRunDisagreements(prefix, entry.Disagreements),
			Errors:             builder.exportMultiAgentRunErrors(prefix, entry.Errors),
			Decisions:          builder.exportMultiAgentRunDecisions(prefix, entry.Decisions),
			Summary:            summary,
			CancellationReason: builder.sanitize(prefix+".cancellation_reason", entry.CancellationReason),
			ResumeReason:       builder.sanitize(prefix+".resume_reason", entry.ResumeReason),
			Error:              builder.sanitize(prefix+".error", entry.Error),
		})
	}

	return exported
}

func (builder *exportBuilder) exportMultiAgentRunSummary(
	prefix string,
	entry MultiAgentRun,
) ExportMultiAgentRunSummary {
	if !multiAgentRunHasAcceptedOutput(entry) {
		return ExportMultiAgentRunSummary{}
	}

	return ExportMultiAgentRunSummary{
		Winner:          builder.sanitize(prefix+".summary.winner", entry.Summary.Winner),
		Reason:          builder.sanitize(prefix+".summary.reason", entry.Summary.Reason),
		VerdictReviewer: builder.sanitize(prefix+".summary.verdict_reviewer", entry.Summary.VerdictReviewer),
		Findings:        entry.Summary.Findings,
	}
}

func (builder *exportBuilder) exportMultiAgentRunCalls(prefix string, calls []MultiAgentRunCall) []ExportMultiAgentRunCall {
	if len(calls) == 0 {
		return nil
	}

	exported := make([]ExportMultiAgentRunCall, 0, len(calls))
	for index := range calls {
		call := calls[index]
		callPrefix := fmt.Sprintf("%s.calls[%d]", prefix, index+1)
		exported = append(exported, ExportMultiAgentRunCall{
			StartedAt:      call.StartedAt,
			CompletedAt:    call.CompletedAt,
			ID:             builder.sanitize(callPrefix+".id", call.ID),
			Phase:          builder.sanitize(callPrefix+".phase", call.Phase),
			Agent:          builder.sanitize(callPrefix+".agent", call.Agent),
			TargetAgent:    builder.sanitize(callPrefix+".target_agent", call.TargetAgent),
			Status:         call.Status,
			RequestedModel: builder.sanitize(callPrefix+".requested_model", call.RequestedModel),
			ResponseModel:  builder.sanitize(callPrefix+".response_model", call.ResponseModel),
			FallbackModels: builder.sanitizeSlice(callPrefix+".fallback_models", call.FallbackModels),
			PromptHash:     builder.sanitize(callPrefix+".prompt_hash", call.PromptHash),
			SystemPrompt: builder.sanitizeIssueOmittable(
				callPrefix+".system_prompt",
				call.SystemPrompt,
				"multi-agent run provider-call prompts/responses omitted by issue/PR summary profile",
			),
			UserPrompt: builder.sanitizeIssueOmittable(
				callPrefix+".user_prompt",
				call.UserPrompt,
				"multi-agent run provider-call prompts/responses omitted by issue/PR summary profile",
			),
			Response: builder.sanitizeIssueOmittable(
				callPrefix+".response",
				call.Response,
				"multi-agent run provider-call prompts/responses omitted by issue/PR summary profile",
			),
			Error:                builder.sanitize(callPrefix+".error", call.Error),
			InputTokenEstimate:   call.InputTokenEstimate,
			OutputTokenEstimate:  call.OutputTokenEstimate,
			ContextWindow:        call.ContextWindow,
			MaxOutputTokens:      call.MaxOutputTokens,
			InputTokens:          call.InputTokens,
			CachedInputTokens:    call.CachedInputTokens,
			OutputTokens:         call.OutputTokens,
			TotalTokens:          call.TotalTokens,
			EstimatedCostMicros:  call.EstimatedCostMicros,
			DurationMS:           call.DurationMS,
			BudgetRejectionRule:  builder.sanitize(callPrefix+".budget_rejection_rule", call.BudgetRejectionRule),
			BudgetRejectionUsage: call.BudgetRejectionUsage,
			BudgetRejectionLimit: call.BudgetRejectionLimit,
		})
	}

	return exported
}

func (builder *exportBuilder) exportMultiAgentRunBranches(prefix string, branches []MultiAgentRunBranch) []ExportMultiAgentRunBranch {
	if len(branches) == 0 {
		return nil
	}

	exported := make([]ExportMultiAgentRunBranch, 0, len(branches))
	for index := range branches {
		branch := &branches[index]
		branchPrefix := fmt.Sprintf("%s.branches[%d]", prefix, index+1)
		exported = append(exported, ExportMultiAgentRunBranch{
			Name:                 builder.sanitize(branchPrefix+".name", branch.Name),
			Role:                 builder.sanitize(branchPrefix+".role", branch.Role),
			Provenance:           builder.sanitize(branchPrefix+".provenance", branch.Provenance),
			Model:                builder.sanitize(branchPrefix+".model", branch.Model),
			PromptHash:           builder.sanitize(branchPrefix+".prompt_hash", branch.PromptHash),
			Error:                builder.sanitize(branchPrefix+".error", branch.Error),
			Status:               branch.Status,
			InputTokenEstimate:   branch.InputTokenEstimate,
			OutputTokenEstimate:  branch.OutputTokenEstimate,
			ContextWindow:        branch.ContextWindow,
			MaxOutputTokens:      branch.MaxOutputTokens,
			InputTokens:          branch.InputTokens,
			CachedInputTokens:    branch.CachedInputTokens,
			OutputTokens:         branch.OutputTokens,
			TotalTokens:          branch.TotalTokens,
			EstimatedCostMicros:  branch.EstimatedCostMicros,
			DurationMS:           branch.DurationMS,
			BudgetRejectionRule:  builder.sanitize(branchPrefix+".budget_rejection_rule", branch.BudgetRejectionRule),
			BudgetRejectionUsage: branch.BudgetRejectionUsage,
			BudgetRejectionLimit: branch.BudgetRejectionLimit,
		})
	}

	return exported
}

func (builder *exportBuilder) exportMultiAgentRunReviewers(prefix string, reviewers []MultiAgentRunReviewer) []ExportMultiAgentRunReviewer {
	if len(reviewers) == 0 {
		return nil
	}

	exported := make([]ExportMultiAgentRunReviewer, 0, len(reviewers))
	for index, reviewer := range reviewers {
		reviewerPrefix := fmt.Sprintf("%s.reviewers[%d]", prefix, index+1)
		exported = append(exported, ExportMultiAgentRunReviewer{
			Name:        builder.sanitize(reviewerPrefix+".name", reviewer.Name),
			Role:        builder.sanitize(reviewerPrefix+".role", reviewer.Role),
			TargetAgent: builder.sanitize(reviewerPrefix+".target_agent", reviewer.TargetAgent),
			Model:       builder.sanitize(reviewerPrefix+".model", reviewer.Model),
			PromptHash:  builder.sanitize(reviewerPrefix+".prompt_hash", reviewer.PromptHash),
			CallID:      builder.sanitize(reviewerPrefix+".call_id", reviewer.CallID),
		})
	}

	return exported
}

func (builder *exportBuilder) exportMultiAgentRunArtifacts(prefix string, artifacts []MultiAgentRunArtifact) []ExportMultiAgentRunArtifact {
	if len(artifacts) == 0 {
		return nil
	}

	exported := make([]ExportMultiAgentRunArtifact, 0, len(artifacts))
	for index, artifact := range artifacts {
		artifactPrefix := fmt.Sprintf("%s.artifacts[%d]", prefix, index+1)
		exported = append(exported, ExportMultiAgentRunArtifact{
			CreatedAt:   artifact.CreatedAt,
			Kind:        builder.sanitize(artifactPrefix+".kind", artifact.Kind),
			Phase:       builder.sanitize(artifactPrefix+".phase", artifact.Phase),
			Agent:       builder.sanitize(artifactPrefix+".agent", artifact.Agent),
			TargetAgent: builder.sanitize(artifactPrefix+".target_agent", artifact.TargetAgent),
			Content: builder.sanitizeIssueOmittable(
				artifactPrefix+".content",
				artifact.Content,
				"multi-agent run artifact contents omitted by issue/PR summary profile",
			),
			Metadata: builder.sanitizeMap(artifactPrefix+".metadata", artifact.Metadata),
			Index:    artifact.Index,
		})
	}

	return exported
}

func (builder *exportBuilder) exportMultiAgentRunGates(prefix string, gates []MultiAgentRunGate) []ExportMultiAgentRunGate {
	if len(gates) == 0 {
		return nil
	}

	exported := make([]ExportMultiAgentRunGate, 0, len(gates))
	for index, gate := range gates {
		gatePrefix := fmt.Sprintf("%s.gates[%d]", prefix, index+1)
		exported = append(exported, ExportMultiAgentRunGate{
			Name:   builder.sanitize(gatePrefix+".name", gate.Name),
			Phase:  builder.sanitize(gatePrefix+".phase", gate.Phase),
			Agent:  builder.sanitize(gatePrefix+".agent", gate.Agent),
			Notes:  builder.sanitize(gatePrefix+".notes", gate.Notes),
			Passed: gate.Passed,
		})
	}

	return exported
}

func (builder *exportBuilder) exportMultiAgentRunDisagreements(prefix string, disagreements []MultiAgentRunDisagreement) []ExportMultiAgentRunDisagreement {
	if len(disagreements) == 0 {
		return nil
	}

	exported := make([]ExportMultiAgentRunDisagreement, 0, len(disagreements))
	for index, disagreement := range disagreements {
		disagreementPrefix := fmt.Sprintf("%s.disagreements[%d]", prefix, index+1)
		exported = append(exported, ExportMultiAgentRunDisagreement{
			Phase:       builder.sanitize(disagreementPrefix+".phase", disagreement.Phase),
			Reviewer:    builder.sanitize(disagreementPrefix+".reviewer", disagreement.Reviewer),
			TargetAgent: builder.sanitize(disagreementPrefix+".target_agent", disagreement.TargetAgent),
			Subject:     builder.sanitize(disagreementPrefix+".subject", disagreement.Subject),
			Notes: builder.sanitizeIssueOmittable(
				disagreementPrefix+".notes",
				disagreement.Notes,
				"multi-agent run disagreement notes omitted by issue/PR summary profile",
			),
			Index: disagreement.Index,
		})
	}

	return exported
}

func (builder *exportBuilder) exportMultiAgentRunErrors(prefix string, runErrors []MultiAgentRunError) []ExportMultiAgentRunError {
	if len(runErrors) == 0 {
		return nil
	}

	exported := make([]ExportMultiAgentRunError, 0, len(runErrors))
	for index, runError := range runErrors {
		errorPrefix := fmt.Sprintf("%s.errors[%d]", prefix, index+1)
		exported = append(exported, ExportMultiAgentRunError{
			Stage:       builder.sanitize(errorPrefix+".stage", runError.Stage),
			Reviewer:    builder.sanitize(errorPrefix+".reviewer", runError.Reviewer),
			TargetAgent: builder.sanitize(errorPrefix+".target_agent", runError.TargetAgent),
			Message:     builder.sanitize(errorPrefix+".message", runError.Message),
		})
	}

	return exported
}

func (builder *exportBuilder) exportMultiAgentRunDecisions(prefix string, decisions []MultiAgentRunDecision) []ExportMultiAgentRunDecision {
	if len(decisions) == 0 {
		return nil
	}

	exported := make([]ExportMultiAgentRunDecision, 0, len(decisions))
	for index, decision := range decisions {
		decisionPrefix := fmt.Sprintf("%s.decisions[%d]", prefix, index+1)
		exported = append(exported, ExportMultiAgentRunDecision{
			Kind:        builder.sanitize(decisionPrefix+".kind", decision.Kind),
			Phase:       builder.sanitize(decisionPrefix+".phase", decision.Phase),
			Agent:       builder.sanitize(decisionPrefix+".agent", decision.Agent),
			TargetAgent: builder.sanitize(decisionPrefix+".target_agent", decision.TargetAgent),
			Outcome:     builder.sanitize(decisionPrefix+".outcome", decision.Outcome),
			Rationale:   builder.sanitize(decisionPrefix+".rationale", decision.Rationale),
			Index:       decision.Index,
		})
	}

	return exported
}

func (builder *exportBuilder) exportProvenance(sessionState Session) ExportProvenance {
	provenance := ExportProvenance{
		ConfigHash:        sessionConfigHash(sessionState),
		Providers:         builder.sanitizeSlice("provenance.providers", sessionProviders(sessionState)),
		Models:            builder.sanitizeSlice("session.default_model", sessionModels(sessionState)),
		TokenUsage:        sessionTokenUsage(sessionState),
		ProviderCalls:     builder.exportProviderCalls(sessionState.ProviderCalls),
		ReferencedFiles:   builder.exportFileReferences(sessionFileReferences(sessionState)),
		VerificationGates: builder.exportVerificationGates(sessionVerificationGates(sessionState)),
	}

	if sessionState.EventLog != nil {
		provenance.EventLog = &ExportEventLogProvenance{
			Path:          builder.sanitize("provenance.event_log.path", sessionState.EventLog.Path),
			LastHash:      builder.sanitize("provenance.event_log.last_hash", sessionState.EventLog.LastHash),
			SchemaVersion: sessionState.EventLog.SchemaVersion,
			EventCount:    sessionState.EventLog.EventCount,
			LastSequence:  sessionState.EventLog.LastSequence,
			TruncatedTail: sessionState.EventLog.TruncatedTail,
		}
	}

	return provenance
}

func (builder *exportBuilder) exportProviderCalls(calls []ProviderCall) []ExportProviderCall {
	if len(calls) == 0 {
		return nil
	}

	out := make([]ExportProviderCall, 0, len(calls))
	for index := range calls {
		call := exportProviderCallProjection(calls[index])
		if !providerCallHasEvidence(call) {
			continue
		}

		prefix := fmt.Sprintf("provenance.provider_calls[%d]", len(out)+1)
		requestToolCallCount, requestToolResultCount := requestToolEventCounts(call.RequestMessages)
		exported := ExportProviderCall{
			StartedAt:               builder.exportTime(prefix+".started_at", call.StartedAt),
			CompletedAt:             builder.exportTime(prefix+".completed_at", call.CompletedAt),
			ID:                      builder.sanitize(prefix+".id", call.ID),
			Source:                  builder.sanitize(prefix+".source", call.Source),
			Provider:                builder.sanitize(prefix+".provider", call.Provider),
			RequestedModel:          builder.sanitize(prefix+".requested_model", call.RequestedModel),
			ResponseModel:           builder.sanitize(prefix+".response_model", call.ResponseModel),
			ModelMode:               builder.sanitize(prefix+".model_mode", call.ModelMode),
			ReasoningLevel:          builder.sanitize(prefix+".reasoning_level", call.ReasoningLevel),
			PromptHash:              builder.sanitize(prefix+".prompt_hash", call.PromptHash),
			ConfigHash:              builder.sanitize(prefix+".config_hash", call.ConfigHash),
			ResponseHash:            builder.sanitize(prefix+".response_hash", call.ResponseHash),
			StopReason:              call.StopReason,
			RequestMessages:         builder.exportProviderCallRequestMessages(prefix, call.RequestMessages),
			FallbackModels:          builder.sanitizeSlice(prefix+".fallback_models", call.FallbackModels),
			ReferencedFiles:         builder.exportFileReferencesForPrefix(prefix+".referenced_files", fileReferencesFromProviderCall(call)),
			InputTokens:             call.InputTokens,
			CachedInputTokens:       call.CachedInputTokens,
			CacheWriteInputTokens:   call.CacheWriteInputTokens,
			OutputTokens:            call.OutputTokens,
			TotalTokens:             call.TotalTokens,
			EstimatedInputTokens:    call.EstimatedInputTokens,
			EstimatedOutputTokens:   call.EstimatedOutputTokens,
			MaxOutputTokens:         call.MaxOutputTokens,
			RequestMessageCount:     call.RequestMessageCount,
			RequestToolCount:        call.RequestToolCount,
			RequestToolCallCount:    requestToolCallCount,
			RequestToolResultCount:  requestToolResultCount,
			ResponseToolCallCount:   call.ResponseToolCallCount,
			DurationMillis:          call.DurationMillis,
			FirstTokenLatencyMillis: call.FirstTokenLatencyMillis,
		}

		out = append(out, exported)
	}

	return out
}

func exportProviderCallProjection(call ProviderCall) ProviderCall {
	if !call.StartedAt.IsZero() {
		call.StartedAt = call.StartedAt.UTC()
	}

	if !call.CompletedAt.IsZero() {
		call.CompletedAt = call.CompletedAt.UTC()
	}

	call.ID = strings.TrimSpace(call.ID)
	call.Source = strings.TrimSpace(call.Source)
	call.Provider = strings.TrimSpace(call.Provider)
	call.RequestedModel = strings.TrimSpace(call.RequestedModel)
	call.ResponseModel = strings.TrimSpace(call.ResponseModel)
	call.ModelMode = strings.TrimSpace(call.ModelMode)
	call.ReasoningLevel = strings.TrimSpace(call.ReasoningLevel)
	call.PromptHash = strings.TrimSpace(call.PromptHash)
	call.ConfigHash = strings.TrimSpace(call.ConfigHash)
	call.ResponseHash = strings.TrimSpace(call.ResponseHash)

	if call.Provider == "" {
		call.Provider = providerFromModel(firstNonEmptyString(call.ResponseModel, call.RequestedModel))
	}

	if call.PromptHash == "" && len(call.RequestMessages) > 0 {
		call.PromptHash = hashJSON(call.RequestMessages)
	}

	if call.RequestMessageCount == 0 {
		call.RequestMessageCount = len(call.RequestMessages)
	}

	call.FallbackModels = compactStringSlice(call.FallbackModels)
	call.ReferencedFiles = cloneFileReferences(call.ReferencedFiles)
	call.RequestMessages = append([]llm.Message(nil), call.RequestMessages...)
	call.InputTokens = max(call.InputTokens, 0)
	call.CachedInputTokens = max(call.CachedInputTokens, 0)
	call.CacheWriteInputTokens = max(call.CacheWriteInputTokens, 0)
	call.OutputTokens = max(call.OutputTokens, 0)
	call.TotalTokens = max(call.TotalTokens, 0)

	if call.TotalTokens == 0 {
		call.TotalTokens = call.InputTokens + call.OutputTokens
	}

	call.EstimatedInputTokens = max(call.EstimatedInputTokens, 0)
	call.EstimatedOutputTokens = max(call.EstimatedOutputTokens, 0)
	call.MaxOutputTokens = max(call.MaxOutputTokens, 0)
	call.RequestToolCount = max(call.RequestToolCount, 0)
	call.ResponseToolCallCount = max(call.ResponseToolCallCount, 0)
	call.DurationMillis = nonNegativeInt64(call.DurationMillis)
	call.FirstTokenLatencyMillis = nonNegativeInt64(call.FirstTokenLatencyMillis)

	return call
}

func (builder *exportBuilder) exportProviderCallRequestMessages(prefix string, messages []llm.Message) []ExportMessage {
	if len(messages) == 0 {
		return nil
	}

	if !builder.exportsRawAttachments() {
		builder.omit(prefix + ".request_messages omitted by non-private export profile")

		return nil
	}

	out := make([]ExportMessage, 0, len(messages))
	for index := range messages {
		message := messages[index]
		messagePrefix := fmt.Sprintf("%s.request_messages[%d]", prefix, index+1)
		exported := ExportMessage{
			Index:   index + 1,
			Role:    message.Role,
			Content: builder.exportString(messagePrefix+".content", SearchFieldTranscript, message.Content),
		}

		if len(message.ToolCalls) > 0 {
			exported.ToolCalls = append([]llm.ToolCall(nil), message.ToolCalls...)
		}

		if message.ToolResult != nil {
			result := *message.ToolResult
			exported.ToolResult = &result
		}

		out = append(out, exported)
	}

	return out
}

func requestToolEventCounts(messages []llm.Message) (toolCalls, toolResults int) {
	for index := range messages {
		message := &messages[index]
		toolCalls += len(message.ToolCalls)

		if message.ToolResult != nil {
			toolResults++
		}
	}

	return toolCalls, toolResults
}

func fileReferencesFromProviderCall(call ProviderCall) []ExportFileReference {
	if len(call.ReferencedFiles) == 0 {
		return nil
	}

	refs := make([]ExportFileReference, 0, len(call.ReferencedFiles))
	for index := range call.ReferencedFiles {
		refs = append(refs, exportFileReferenceFromSessionReference(call.ReferencedFiles[index]))
	}

	return refs
}

func (builder *exportBuilder) exportFileReferences(entries []ExportFileReference) []ExportFileReference {
	return builder.exportFileReferencesForPrefix("provenance.referenced_files", entries)
}

func (builder *exportBuilder) exportFileReferencesForPrefix(prefix string, entries []ExportFileReference) []ExportFileReference {
	if len(entries) == 0 {
		return nil
	}

	if builder.excludes(SearchFieldArtifacts) {
		builder.omit("provenance file references omitted by export field policy")

		return nil
	}

	out := make([]ExportFileReference, 0, len(entries))
	for index := range entries {
		entry := &entries[index]
		entryPrefix := fmt.Sprintf("%s[%d]", prefix, index+1)
		out = append(out, ExportFileReference{
			Path:            builder.sanitize(entryPrefix+".path", entry.Path),
			LogicalPath:     builder.sanitize(entryPrefix+".logical_path", entry.LogicalPath),
			Kind:            builder.sanitize(entryPrefix+".kind", entry.Kind),
			Source:          builder.sanitize(entryPrefix+".source", entry.Source),
			SourceAgent:     builder.sanitize(entryPrefix+".source_agent", entry.SourceAgent),
			SourceSessionID: builder.sanitize(entryPrefix+".source_session_id", entry.SourceSessionID),
			SHA256:          builder.sanitize(entryPrefix+".sha256", entry.SHA256),
			SizeBytes:       entry.SizeBytes,
			WorktreeBranch:  builder.sanitize(entryPrefix+".worktree_branch", entry.WorktreeBranch),
			WorktreeBase:    builder.sanitize(entryPrefix+".worktree_base", entry.WorktreeBase),
		})
	}

	return out
}

func (builder *exportBuilder) exportVerificationGates(entries []ExportVerificationGate) []ExportVerificationGate {
	if len(entries) == 0 {
		return nil
	}

	out := make([]ExportVerificationGate, 0, len(entries))
	for index, entry := range entries {
		prefix := fmt.Sprintf("provenance.verification_gates[%d]", index+1)
		out = append(out, ExportVerificationGate{
			RunID:  builder.sanitize(prefix+".run_id", entry.RunID),
			Name:   builder.sanitize(prefix+".name", entry.Name),
			Phase:  builder.sanitize(prefix+".phase", entry.Phase),
			Agent:  builder.sanitize(prefix+".agent", entry.Agent),
			Notes:  builder.sanitize(prefix+".notes", entry.Notes),
			Passed: entry.Passed,
		})
	}

	return out
}

func (builder *exportBuilder) exportMessages(messages []llm.Message) []ExportMessage {
	if len(messages) == 0 {
		return nil
	}

	if builder.excludes(SearchFieldTranscript) {
		builder.omit("transcript omitted by export field policy")

		return nil
	}

	if builder.options.profile == ExportProfileIssue {
		builder.omit("transcript omitted by issue/PR summary profile")
		return nil
	}

	limit := len(messages)
	if builder.options.maxTranscriptMessages >= 0 && limit > builder.options.maxTranscriptMessages {
		limit = builder.options.maxTranscriptMessages
		builder.omit(fmt.Sprintf("transcript messages %d-%d omitted by message limit %d", limit+1, len(messages), builder.options.maxTranscriptMessages))
	}

	exported := make([]ExportMessage, 0, limit)
	for index := range limit {
		message := messages[index]
		exportedMessage := ExportMessage{
			Index:   index + 1,
			Role:    message.Role,
			Content: builder.exportString(fmt.Sprintf("messages[%d].content", index+1), SearchFieldTranscript, message.Content),
		}

		if len(message.ToolCalls) > 0 {
			if builder.exportsRawAttachments() {
				exportedMessage.ToolCalls = append([]llm.ToolCall(nil), message.ToolCalls...)
			} else {
				exportedMessage.ToolCallCount = len(message.ToolCalls)

				builder.omit(fmt.Sprintf("messages[%d].tool_calls omitted from %s", index+1, builder.attachmentOmissionScope()))
			}
		}

		if message.ToolResult != nil {
			if builder.exportsRawAttachments() {
				result := *message.ToolResult
				exportedMessage.ToolResult = &result
			} else {
				exportedMessage.ToolResultOmitted = true

				builder.omit(fmt.Sprintf("messages[%d].tool_result omitted from %s", index+1, builder.attachmentOmissionScope()))
			}
		}

		exported = append(exported, exportedMessage)
	}

	return exported
}

func (builder *exportBuilder) exportsRawAttachments() bool {
	return builder.options.profile == ExportProfilePrivate && !builder.options.redact
}

func (builder *exportBuilder) attachmentOmissionScope() string {
	if builder.options.profile == ExportProfilePrivate {
		return "redacted private export"
	}

	return "shareable export"
}

func (builder *exportBuilder) exportSlice(field string, searchField SearchField, values []string) []string {
	if len(values) == 0 {
		return nil
	}

	if builder.excludes(searchField) {
		builder.omit(field + " omitted by export field policy")

		return nil
	}

	out := make([]string, 0, len(values))
	for index, value := range values {
		out = append(out, builder.sanitize(fmt.Sprintf("%s[%d]", field, index+1), value))
	}

	return out
}

func (builder *exportBuilder) sanitizeSlice(field string, values []string) []string {
	if len(values) == 0 {
		return nil
	}

	out := make([]string, 0, len(values))
	for index, value := range values {
		out = append(out, builder.sanitize(fmt.Sprintf("%s[%d]", field, index+1), value))
	}

	return out
}

func (builder *exportBuilder) sanitizeMap(field string, values map[string]string) map[string]string {
	if len(values) == 0 {
		return nil
	}

	out := make(map[string]string, len(values))
	for key, value := range values {
		safeKey := builder.sanitize(field+".key", key)
		out[safeKey] = builder.sanitize(field+"."+key, value)
	}

	return out
}

func (builder *exportBuilder) sanitizeIssueOmittable(field, value, reason string) string {
	if value == "" {
		return ""
	}

	if builder.options.profile == ExportProfileIssue {
		builder.omit(reason)

		return ""
	}

	return builder.sanitize(field, value)
}

func (builder *exportBuilder) exportString(field string, searchField SearchField, value string) string {
	if value == "" {
		return ""
	}

	if builder.excludes(searchField) {
		builder.omit(field + " omitted by export field policy")

		return ""
	}

	return builder.sanitize(field, value)
}

func (builder *exportBuilder) exportTime(field string, value time.Time) time.Time {
	if value.IsZero() {
		return time.Time{}
	}

	if builder.excludes(SearchFieldDate) {
		builder.omit(field + " omitted by export field policy")

		return time.Time{}
	}

	return value
}

func (builder *exportBuilder) excludes(field SearchField) bool {
	_, ok := builder.options.excludedFields[field]

	return ok
}

func (builder *exportBuilder) sanitize(field, value string) string {
	if value == "" {
		return ""
	}

	if builder.options.redact {
		value = redactSensitiveField(field, value, builder.options.sensitiveFields)
	}

	return builder.limit(field, value)
}

func (builder *exportBuilder) limit(field, value string) string {
	limit := builder.options.maxContentRunes
	if limit < 0 {
		return value
	}

	runes := []rune(value)
	if len(runes) <= limit {
		return value
	}

	omitted := len(runes) - limit
	builder.omit(fmt.Sprintf("%s truncated by %d runes", field, omitted))

	return string(runes[:limit]) + fmt.Sprintf("\n\n[Truncated: omitted %d runes]", omitted)
}

func (builder *exportBuilder) omit(reason string) {
	if reason == "" {
		return
	}

	if _, ok := builder.omitSeen[reason]; ok {
		return
	}

	builder.omitSeen[reason] = struct{}{}
	builder.omitted = append(builder.omitted, reason)
}

func redactSensitive(value string, sensitiveFields []string) string {
	value = privateKeyBlockRE.ReplaceAllString(value, "[REDACTED_PRIVATE_KEY]")
	value = urlCredentialsRE.ReplaceAllString(value, "${1}[REDACTED]@")
	value = cookieHeaderRE.ReplaceAllString(value, "${1}[REDACTED]")
	value = authorizationRE.ReplaceAllString(value, "${1}[REDACTED]")
	value = bearerTokenRE.ReplaceAllString(value, "Bearer [REDACTED]")
	value = sensitiveFieldRE(sensitiveFields).ReplaceAllString(value, "${1}[REDACTED]")
	value = openAIKeyRE.ReplaceAllString(value, "[REDACTED_API_KEY]")
	value = anthropicKeyRE.ReplaceAllString(value, "[REDACTED_API_KEY]")
	value = githubTokenRE.ReplaceAllString(value, "[REDACTED_GITHUB_TOKEN]")
	value = slackTokenRE.ReplaceAllString(value, "[REDACTED_SLACK_TOKEN]")
	value = awsAccessKeyRE.ReplaceAllString(value, "[REDACTED_AWS_ACCESS_KEY]")
	value = jwtRE.ReplaceAllString(value, "[REDACTED_JWT]")
	value = fileURIPathRE.ReplaceAllString(value, "file://[REDACTED_PATH]")
	value = quotedPathFieldRE.ReplaceAllString(value, "${1}[REDACTED_PATH]${2}")
	value = posixPathFieldRE.ReplaceAllString(value, "${1}[REDACTED_PATH]")
	value = tildePathRE.ReplaceAllString(value, "${1}[REDACTED_PATH]")
	value = uncAbsPathRE.ReplaceAllString(value, "[REDACTED_PATH]")
	value = windowsAbsPathRE.ReplaceAllString(value, "[REDACTED_PATH]")
	value = posixAbsPathRE.ReplaceAllString(value, "${1}[REDACTED_PATH]")

	return value
}

func redactSensitiveField(field, value string, sensitiveFields []string) string {
	if strings.TrimSpace(value) == "" {
		return ""
	}

	if sensitiveFieldNameRE(sensitiveFields).MatchString(field) {
		return "[REDACTED]"
	}

	return redactSensitive(value, sensitiveFields)
}

func sensitiveFieldRE(extraFields []string) *regexp.Regexp {
	patterns := sensitiveFieldPatterns(extraFields)

	return regexp.MustCompile(`(?i)(["']?\b(?:` + strings.Join(patterns, "|") + `)\b["']?\s*[:=]\s*)(?:"[^"\r\n]*"|'[^'\r\n]*'|[^\s,;}]+)`)
}

func sensitiveFieldNameRE(extraFields []string) *regexp.Regexp {
	patterns := sensitiveFieldPatterns(extraFields)

	return regexp.MustCompile(`(?i)(?:^|[^a-z0-9])(?:` + strings.Join(patterns, "|") + `)(?:$|[^a-z0-9])`)
}

func sensitiveFieldPatterns(extraFields []string) []string {
	patterns := append([]string(nil), defaultSensitiveFieldPatterns...)

	for _, field := range extraFields {
		field = strings.TrimSpace(field)
		if field == "" {
			continue
		}

		patterns = append(patterns, regexp.QuoteMeta(field))
	}

	return patterns
}

func renderMarkdown(export MachineReadableExport) string {
	var b strings.Builder

	title := export.Session.Title
	if title != "" {
		fmt.Fprintf(&b, "# %s\n\n", markdownInline(title))
		writeMetadataString(&b, "Session", export.Session.ID)
	} else {
		fmt.Fprintf(&b, "# Atteler Session %s\n\n", markdownInline(fallback(export.Session.ID, "untitled")))
	}

	if export.Manifest.RedactionProfile == ExportProfilePrivate {
		fmt.Fprintf(&b, "> [!WARNING]\n> %s\n\n", markdownInline(export.Manifest.PrivacyNotice))
	}

	writeMetadata(&b, "Created", export.Session.CreatedAt)
	writeMetadata(&b, "Updated", export.Session.UpdatedAt)
	writeMetadataString(&b, "Agent", export.Session.DefaultAgent)
	writeMetadataString(&b, "Model", export.Session.DefaultModel)
	writeMetadataString(&b, "Effort", export.Session.DefaultReasoningLevel)
	writeMetadataString(&b, "Mode", export.Session.DefaultModelMode)
	writeMetadataString(&b, "Agent loop budget", formatAgentLoopBudgetCompact(export.Session.AgentLoopBudget))
	writeMetadataString(&b, "Worktree", export.Session.WorktreePath)
	writeMetadataString(&b, "Branch", export.Session.WorktreeBranch)
	writeMetadataString(&b, "Base", export.Session.WorktreeBase)
	writeMetadataString(&b, "Tags", strings.Join(export.Session.Tags, ", "))

	writeManifest(&b, export.Manifest)
	writeProvenance(&b, export.Provenance)

	if export.Manifest.RedactionProfile == ExportProfileIssue {
		writeIssueSummary(&b, export)
		return b.String()
	}

	writeNegativeKnowledge(&b, export.NegativeKnowledge)
	writeEvaluations(&b, export.Evaluations)
	writeArtifacts(&b, export.Artifacts)
	writeMultiAgentRuns(&b, export.MultiAgentRuns)
	writeTranscript(&b, export)

	return b.String()
}

//nolint:gocognit // Markdown provenance rendering mirrors the exported provenance sections.
func writeProvenance(b *strings.Builder, provenance ExportProvenance) {
	b.WriteString("\n## Provenance\n\n")
	writeMetadataString(b, "Config hash", provenance.ConfigHash)
	writeMetadataString(b, "Providers", strings.Join(provenance.Providers, ", "))
	writeMetadataString(b, "Models", strings.Join(provenance.Models, ", "))

	if provenance.EventLog != nil {
		writeMetadataString(b, "Event log", provenance.EventLog.Path)
		writeMetadataString(b, "Event log hash", provenance.EventLog.LastHash)

		if provenance.EventLog.EventCount > 0 {
			fmt.Fprintf(b, "- **Event count:** %d\n", provenance.EventLog.EventCount)
		}

		if provenance.EventLog.TruncatedTail {
			b.WriteString("- **Event log warning:** truncated tail ignored during replay\n")
		}
	}

	writeProvenanceTokenUsage(b, provenance.TokenUsage)
	writeProvenanceProviderCalls(b, provenance.ProviderCalls)

	if len(provenance.ReferencedFiles) > 0 {
		b.WriteString("- **Referenced files:**\n")

		for index := range provenance.ReferencedFiles {
			file := &provenance.ReferencedFiles[index]

			parts := []string{markdownInline(file.Path)}
			if file.Kind != "" {
				parts = append(parts, "kind="+markdownInline(file.Kind))
			}

			if file.Source != "" {
				parts = append(parts, "source="+markdownInline(file.Source))
			}

			if file.SHA256 != "" {
				parts = append(parts, "sha256="+markdownInline(file.SHA256))
			}

			fmt.Fprintf(b, "  - %s\n", strings.Join(parts, " "))
		}
	}

	if len(provenance.VerificationGates) > 0 {
		b.WriteString("- **Verification gates:**\n")

		for _, gate := range provenance.VerificationGates {
			status := "fail"
			if gate.Passed {
				status = "pass"
			}

			parts := []string{markdownInline(gate.Name), "status=" + status}
			if gate.RunID != "" {
				parts = append(parts, "run="+markdownInline(gate.RunID))
			}

			if gate.Phase != "" {
				parts = append(parts, "phase="+markdownInline(gate.Phase))
			}

			if gate.Agent != "" {
				parts = append(parts, "agent="+markdownInline(gate.Agent))
			}

			fmt.Fprintf(b, "  - %s\n", strings.Join(parts, " "))
		}
	}
}

func writeProvenanceProviderCalls(b *strings.Builder, calls []ExportProviderCall) {
	if len(calls) == 0 {
		return
	}

	b.WriteString("- **Provider calls:**\n")

	for index := range calls {
		call := &calls[index]
		parts := providerCallMarkdownParts(call)

		if len(parts) == 0 {
			continue
		}

		fmt.Fprintf(b, "  - %s\n", strings.Join(parts, " "))
	}
}

func providerCallMarkdownParts(call *ExportProviderCall) []string {
	parts := []string{}

	if call.ID != "" {
		parts = append(parts, "id="+markdownInline(call.ID))
	}

	if call.Source != "" {
		parts = append(parts, "source="+markdownInline(call.Source))
	}

	if call.Provider != "" {
		parts = append(parts, "provider="+markdownInline(call.Provider))
	}

	if call.RequestedModel != "" {
		parts = append(parts, "requested_model="+markdownInline(call.RequestedModel))
	}

	if call.ResponseModel != "" {
		parts = append(parts, "response_model="+markdownInline(call.ResponseModel))
	}

	if call.PromptHash != "" {
		parts = append(parts, "prompt_hash="+markdownInline(call.PromptHash))
	}

	if call.ConfigHash != "" {
		parts = append(parts, "config_hash="+markdownInline(call.ConfigHash))
	}

	if call.TotalTokens > 0 {
		parts = append(parts, "total_tokens="+strconv.Itoa(call.TotalTokens))
	}

	if call.RequestToolCallCount > 0 || call.RequestToolResultCount > 0 {
		parts = append(
			parts,
			"tool_calls="+strconv.Itoa(call.RequestToolCallCount),
			"tool_results="+strconv.Itoa(call.RequestToolResultCount),
		)
	}

	if len(call.ReferencedFiles) > 0 {
		parts = append(parts, "files="+strconv.Itoa(len(call.ReferencedFiles)))
	}

	return parts
}

func writeProvenanceTokenUsage(b *strings.Builder, usage ExportTokenUsage) {
	if usage.ModelCalls == 0 &&
		usage.InputTokens == 0 &&
		usage.CachedInputTokens == 0 &&
		usage.CacheWriteInputTokens == 0 &&
		usage.OutputTokens == 0 &&
		usage.TotalTokens == 0 &&
		usage.EstimatedInputTokens == 0 &&
		usage.EstimatedOutputTokens == 0 &&
		usage.EstimatedTotalTokens == 0 &&
		usage.EstimatedCostMicros == 0 {
		return
	}

	fmt.Fprintf(
		b,
		"- **Token usage:** model_calls=%d input=%d cached_input=%d cache_write_input=%d output=%d total=%d estimated_input=%d estimated_output=%d estimated_total=%d estimated_cost_micros=%d\n",
		usage.ModelCalls,
		usage.InputTokens,
		usage.CachedInputTokens,
		usage.CacheWriteInputTokens,
		usage.OutputTokens,
		usage.TotalTokens,
		usage.EstimatedInputTokens,
		usage.EstimatedOutputTokens,
		usage.EstimatedTotalTokens,
		usage.EstimatedCostMicros,
	)
}

func writeManifest(b *strings.Builder, manifest ExportManifest) {
	b.WriteString("\n## Export Manifest\n\n")
	writeMetadataString(b, "Session ID", manifest.SessionID)
	writeMetadata(b, "Exported", manifest.ExportedAt)
	writeMetadataString(b, "Redaction profile", string(manifest.RedactionProfile))
	writeMetadataString(b, "Privacy notice", manifest.PrivacyNotice)

	if len(manifest.OmittedSections) == 0 {
		b.WriteString("- **Omitted sections:** none\n")
	} else {
		b.WriteString("- **Omitted sections:**\n")

		for _, item := range manifest.OmittedSections {
			fmt.Fprintf(b, "  - %s\n", markdownInline(item))
		}
	}

	if len(manifest.ContentHashes) == 0 {
		return
	}

	b.WriteString("- **Content hashes:**\n")

	keys := make([]string, 0, len(manifest.ContentHashes))
	for key := range manifest.ContentHashes {
		keys = append(keys, key)
	}

	sort.Strings(keys)

	for _, key := range keys {
		fmt.Fprintf(b, "  - `%s`: `%s`\n", markdownInline(key), markdownInline(manifest.ContentHashes[key]))
	}
}

func writeIssueSummary(b *strings.Builder, export MachineReadableExport) {
	b.WriteString("\n## Issue/PR Summary\n\n")
	fmt.Fprintf(b, "- **Messages:** %d total, %d exported\n", export.Session.MessageCount, export.Session.ExportedMessageCount)
	fmt.Fprintf(b, "- **Negative knowledge records:** %d\n", export.Session.NegativeKnowledgeCount)
	fmt.Fprintf(b, "- **Evaluations:** %d\n", export.Session.EvaluationCount)
	fmt.Fprintf(b, "- **Artifacts:** %d\n", export.Session.ArtifactCount)
	fmt.Fprintf(b, "- **Multi-agent runs:** %d\n", export.Session.MultiAgentRunCount)

	writeNegativeKnowledge(b, export.NegativeKnowledge)
	writeEvaluations(b, export.Evaluations)
	writeArtifacts(b, export.Artifacts)
	writeMultiAgentRuns(b, export.MultiAgentRuns)
}

func writeEvaluations(b *strings.Builder, entries []ExportAgentEvaluation) {
	if len(entries) == 0 {
		return
	}

	b.WriteString("\n## Agent Evaluations\n\n")

	for i := range entries {
		entry := &entries[i]
		if entry.Agent == "" && entry.Outcome == "" {
			continue
		}

		fmt.Fprintf(b, "- **Agent:** %s\n", markdownInline(entry.Agent))

		if entry.Outcome != "" {
			fmt.Fprintf(b, "  - **Outcome:** %s\n", markdownInline(entry.Outcome))
		}

		if entry.Score != 0 {
			fmt.Fprintf(b, "  - **Score:** %d\n", entry.Score)
		}

		writeEvaluationMetadata(b, entry)

		if entry.Reference != "" {
			fmt.Fprintf(b, "  - **Reference:** %s\n", markdownInline(entry.Reference))
		}

		if entry.Notes != "" {
			fmt.Fprintf(b, "  - **Notes:** %s\n", markdownInline(entry.Notes))
		}

		if !entry.CreatedAt.IsZero() {
			fmt.Fprintf(b, "  - **Created:** %s\n", entry.CreatedAt.UTC().Format(time.RFC3339))
		}
	}
}

func writeEvaluationMetadata(b *strings.Builder, entry *ExportAgentEvaluation) {
	writeIndentedMetadataString(b, "Source", entry.Source)
	writeIndentedMetadataString(b, "Evaluator", entry.Evaluator)
	writeIndentedMetadataString(b, "Rubric Version", entry.RubricVersion)
	writeIndentedMetadataString(b, "Task Type", entry.TaskType)
	writeIndentedMetadataString(b, "Difficulty", entry.Difficulty)
	writeIndentedMetadataString(b, "Expected Outcome", entry.ExpectedOutcome)
	writeIndentedMetadataString(b, "Provider", entry.Provider)
	writeIndentedMetadataString(b, "Model", entry.Model)
	writeIndentedMetadataString(b, "Fixture Version", entry.FixtureVersion)
	writeIndentedMetadataString(b, "Agent Version", entry.AgentVersion)

	if entry.SchemaVersion != 0 {
		fmt.Fprintf(b, "  - **Schema Version:** %d\n", entry.SchemaVersion)
	}

	if entry.DurationMillis != 0 {
		fmt.Fprintf(b, "  - **Duration Millis:** %d\n", entry.DurationMillis)
	}

	if entry.PassRate != nil {
		fmt.Fprintf(b, "  - **Pass Rate:** %.2f\n", *entry.PassRate)
	}

	if entry.FlakeCount != 0 {
		fmt.Fprintf(b, "  - **Flake Count:** %d\n", entry.FlakeCount)
	}

	if entry.TotalTokens != 0 {
		fmt.Fprintf(b, "  - **Total Tokens:** %d\n", entry.TotalTokens)
	}

	if entry.InputTokens != 0 || entry.OutputTokens != 0 {
		fmt.Fprintf(b, "  - **Tokens:** input=%d output=%d\n", entry.InputTokens, entry.OutputTokens)
	}

	if entry.Cost != 0 {
		fmt.Fprintf(b, "  - **Cost:** %.6f\n", entry.Cost)
	}

	if entry.Confidence != 0 {
		fmt.Fprintf(b, "  - **Confidence:** %.2f\n", entry.Confidence)
	}
}

func writeArtifacts(b *strings.Builder, entries []ExportArtifact) {
	if len(entries) == 0 {
		return
	}

	b.WriteString("\n## Artifacts\n\n")

	for i := range entries {
		entry := &entries[i]
		if entry.Path == "" && entry.Kind == "" {
			continue
		}

		fmt.Fprintf(b, "- **Path:** %s\n", markdownInline(entry.Path))
		writeArtifactBasicMetadata(b, entry)
		writeArtifactSourceMetadata(b, entry)
		writeArtifactWorktreeMetadata(b, entry)

		if entry.ReviewStatus != "" {
			fmt.Fprintf(b, "  - **Review Status:** %s\n", markdownInline(entry.ReviewStatus))
		}

		if entry.ConsumedAt != nil && !entry.ConsumedAt.IsZero() {
			fmt.Fprintf(b, "  - **Consumed:** %s\n", entry.ConsumedAt.UTC().Format(time.RFC3339))
		}

		if !entry.CreatedAt.IsZero() {
			fmt.Fprintf(b, "  - **Created:** %s\n", entry.CreatedAt.UTC().Format(time.RFC3339))
		}
	}
}

func writeArtifactBasicMetadata(b *strings.Builder, entry *ExportArtifact) {
	writeIndentedMetadataString(b, "Kind", entry.Kind)
	writeIndentedMetadataString(b, "Summary", entry.Summary)

	if entry.LogicalPath != "" && entry.LogicalPath != entry.Path {
		writeIndentedMetadataString(b, "Logical Path", entry.LogicalPath)
	}

	writeIndentedMetadataString(b, "SHA-256", entry.SHA256)

	if entry.SizeBytes != 0 {
		fmt.Fprintf(b, "  - **Size:** %d bytes\n", entry.SizeBytes)
	}
}

func writeArtifactSourceMetadata(b *strings.Builder, entry *ExportArtifact) {
	writeIndentedMetadataString(b, "Source Agent", entry.SourceAgent)
	writeIndentedMetadataString(b, "Source Session", entry.SourceSessionID)

	if entry.SourceTurn != 0 {
		fmt.Fprintf(b, "  - **Source Turn:** %d\n", entry.SourceTurn)
	}

	writeIndentedMetadataString(b, "Source Command", entry.SourceCommand)
	writeIndentedMetadataString(b, "Source Tool", entry.SourceTool)
	writeIndentedMetadataString(b, "Source Commit", entry.SourceCommit)
}

func writeArtifactWorktreeMetadata(b *strings.Builder, entry *ExportArtifact) {
	writeIndentedMetadataString(b, "Worktree", entry.WorktreePath)
	writeIndentedMetadataString(b, "Worktree Branch", entry.WorktreeBranch)
	writeIndentedMetadataString(b, "Worktree Base", entry.WorktreeBase)

	if entry.WorktreeDirty {
		fmt.Fprintln(b, "  - **Worktree Dirty:** true")
	}
}

//nolint:gocognit,cyclop // Receipt export keeps sections in one stable order for reviewable Markdown.
func writeMultiAgentRuns(b *strings.Builder, entries []ExportMultiAgentRun) {
	if len(entries) == 0 {
		return
	}

	b.WriteString("\n## Multi-agent Runs\n\n")

	for i := range entries {
		entry := entries[i]
		fmt.Fprintf(
			b,
			"- **%s:** `%s` (%s)\n",
			markdownInline(fallback(entry.Kind, "run")),
			markdownInline(entry.ID),
			markdownInline(string(entry.Status)),
		)

		if entry.ReceiptID != "" && entry.ReceiptID != entry.ID {
			fmt.Fprintf(b, "  - **Receipt:** %s\n", markdownInline(entry.ReceiptID))
		}

		writeRunTimeMetadata(b, entry)

		if entry.Prompt != "" {
			fmt.Fprintf(b, "  - **Prompt:** %s\n", markdownInline(truncateForMarkdownLine(entry.Prompt, 240)))
		}

		writeRunSummary(b, entry.Summary)
		writeRunBudget(b, entry.Budget)
		writeRunUsage(b, entry.Usage)

		if entry.CancellationReason != "" {
			fmt.Fprintf(b, "  - **Cancellation reason:** %s\n", markdownInline(entry.CancellationReason))
		}

		if entry.ResumeReason != "" {
			fmt.Fprintf(b, "  - **Resume reason:** %s\n", markdownInline(entry.ResumeReason))
		}

		if entry.Error != "" {
			fmt.Fprintf(b, "  - **Error:** %s\n", markdownInline(entry.Error))
		}

		if len(entry.Branches) > 0 {
			b.WriteString("  - **Branches:**\n")

			for i := range entry.Branches {
				branch := &entry.Branches[i]
				fmt.Fprintf(
					b,
					"    - %s role=%s status=%s model=%s prompt_hash=%s input_estimate=%d output_estimate=%d context_window=%d max_output_tokens=%d input_tokens=%d cached_input_tokens=%d output_tokens=%d total_tokens=%d estimated_cost_micros=%d duration_ms=%d",
					markdownInline(branch.Name),
					markdownInline(branch.Role),
					markdownInline(string(branch.Status)),
					markdownInline(branch.Model),
					markdownInline(branch.PromptHash),
					branch.InputTokenEstimate,
					branch.OutputTokenEstimate,
					branch.ContextWindow,
					branch.MaxOutputTokens,
					branch.InputTokens,
					branch.CachedInputTokens,
					branch.OutputTokens,
					branch.TotalTokens,
					branch.EstimatedCostMicros,
					branch.DurationMS,
				)

				if branch.Provenance != "" {
					fmt.Fprintf(b, " provenance=%s", markdownInline(branch.Provenance))
				}

				if branch.Error != "" {
					fmt.Fprintf(b, " error=%s", markdownInline(branch.Error))
				}

				if branch.BudgetRejectionRule != "" {
					fmt.Fprintf(
						b,
						" budget_rejection=%s used=%d limit=%d",
						markdownInline(branch.BudgetRejectionRule),
						branch.BudgetRejectionUsage,
						branch.BudgetRejectionLimit,
					)
				}

				b.WriteByte('\n')
			}
		}

		if len(entry.Reviewers) > 0 {
			b.WriteString("  - **Reviewers:**\n")

			for _, reviewer := range entry.Reviewers {
				fmt.Fprintf(
					b,
					"    - %s role=%s target=%s model=%s prompt_hash=%s call=%s\n",
					markdownInline(reviewer.Name),
					markdownInline(reviewer.Role),
					markdownInline(reviewer.TargetAgent),
					markdownInline(reviewer.Model),
					markdownInline(reviewer.PromptHash),
					markdownInline(reviewer.CallID),
				)
			}
		}

		if len(entry.Gates) > 0 {
			b.WriteString("  - **Gates:**\n")

			for _, gate := range entry.Gates {
				status := "FAIL"
				if gate.Passed {
					status = "PASS"
				}

				fmt.Fprintf(b, "    - %s: %s", markdownInline(gate.Name), status)

				if gate.Phase != "" || gate.Agent != "" {
					fmt.Fprintf(b, " (%s %s)", markdownInline(gate.Phase), markdownInline(gate.Agent))
				}

				if gate.Notes != "" {
					fmt.Fprintf(b, " — %s", markdownInline(gate.Notes))
				}

				b.WriteByte('\n')
			}
		}

		if len(entry.Disagreements) > 0 {
			b.WriteString("  - **Disagreements:**\n")

			for _, disagreement := range entry.Disagreements {
				fmt.Fprintf(
					b,
					"    - %s reviewer=%s target=%s subject=%s",
					markdownInline(disagreement.Phase),
					markdownInline(disagreement.Reviewer),
					markdownInline(disagreement.TargetAgent),
					markdownInline(disagreement.Subject),
				)

				if disagreement.Index > 0 {
					fmt.Fprintf(b, " index=%d", disagreement.Index)
				}

				if disagreement.Notes != "" {
					fmt.Fprintf(b, " — %s", markdownInline(disagreement.Notes))
				}

				b.WriteByte('\n')
			}
		}

		if len(entry.Errors) > 0 {
			b.WriteString("  - **Workflow errors:**\n")

			for _, runError := range entry.Errors {
				fmt.Fprintf(
					b,
					"    - stage=%s reviewer=%s target=%s",
					markdownInline(runError.Stage),
					markdownInline(runError.Reviewer),
					markdownInline(runError.TargetAgent),
				)

				if runError.Message != "" {
					fmt.Fprintf(b, " — %s", markdownInline(runError.Message))
				}

				b.WriteByte('\n')
			}
		}

		if len(entry.Decisions) > 0 {
			b.WriteString("  - **Decisions:**\n")

			for _, decision := range entry.Decisions {
				fmt.Fprintf(
					b,
					"    - %s %s phase=%s agent=%s",
					markdownInline(decision.Kind),
					markdownInline(decision.Outcome),
					markdownInline(decision.Phase),
					markdownInline(decision.Agent),
				)

				if decision.TargetAgent != "" {
					fmt.Fprintf(b, " target=%s", markdownInline(decision.TargetAgent))
				}

				if decision.Index > 0 {
					fmt.Fprintf(b, " index=%d", decision.Index)
				}

				if decision.Rationale != "" {
					fmt.Fprintf(b, " — %s", markdownInline(decision.Rationale))
				}

				b.WriteByte('\n')
			}
		}

		if len(entry.Artifacts) > 0 {
			b.WriteString("  - **Artifacts:**\n")

			for _, artifact := range entry.Artifacts {
				fmt.Fprintf(
					b,
					"    - %s phase=%s agent=%s target=%s",
					markdownInline(artifact.Kind),
					markdownInline(artifact.Phase),
					markdownInline(artifact.Agent),
					markdownInline(artifact.TargetAgent),
				)

				if artifact.Index > 0 {
					fmt.Fprintf(b, " index=%d", artifact.Index)
				}

				if artifact.Content != "" {
					fmt.Fprintf(b, " content=%s", markdownInline(truncateForMarkdownLine(artifact.Content, 240)))
				}

				writeRunArtifactMetadata(b, artifact.Metadata)

				b.WriteByte('\n')
			}
		}

		writeRunCalls(b, entry.Calls)
	}
}

func writeRunArtifactMetadata(b *strings.Builder, metadata map[string]string) {
	if len(metadata) == 0 {
		return
	}

	keys := make([]string, 0, len(metadata))
	for key := range metadata {
		key = strings.TrimSpace(key)
		if key != "" {
			keys = append(keys, key)
		}
	}

	sort.Strings(keys)

	for _, key := range keys {
		fmt.Fprintf(b, " metadata.%s=%s", markdownInline(key), markdownInline(metadata[key]))
	}
}

func writeRunCalls(b *strings.Builder, calls []ExportMultiAgentRunCall) {
	if len(calls) == 0 {
		return
	}

	b.WriteString("  - **Calls:**\n")

	for i := range calls {
		writeRunCall(b, &calls[i])
	}
}

func writeRunCall(b *strings.Builder, call *ExportMultiAgentRunCall) {
	fmt.Fprintf(
		b,
		"    - %s agent=%s target=%s status=%s model=%s input_estimate=%d output_estimate=%d",
		markdownInline(call.Phase),
		markdownInline(call.Agent),
		markdownInline(call.TargetAgent),
		markdownInline(string(call.Status)),
		markdownInline(fallback(call.ResponseModel, call.RequestedModel)),
		call.InputTokenEstimate,
		call.OutputTokenEstimate,
	)

	if call.PromptHash != "" {
		fmt.Fprintf(b, " prompt_hash=%s", markdownInline(call.PromptHash))
	}

	if call.RequestedModel != "" {
		fmt.Fprintf(b, " requested_model=%s", markdownInline(call.RequestedModel))
	}

	if call.ResponseModel != "" {
		fmt.Fprintf(b, " response_model=%s", markdownInline(call.ResponseModel))
	}

	if len(call.FallbackModels) > 0 {
		fmt.Fprintf(b, " fallback_models=%s", markdownInline(strings.Join(call.FallbackModels, ",")))
	}

	if call.MaxOutputTokens > 0 {
		fmt.Fprintf(b, " max_output_tokens=%d", call.MaxOutputTokens)
	}

	if call.ContextWindow > 0 {
		fmt.Fprintf(b, " context_window=%d", call.ContextWindow)
	}

	if call.InputTokens > 0 || call.CachedInputTokens > 0 || call.OutputTokens > 0 || call.TotalTokens > 0 {
		fmt.Fprintf(
			b,
			" input_tokens=%d cached_input_tokens=%d output_tokens=%d total_tokens=%d",
			call.InputTokens,
			call.CachedInputTokens,
			call.OutputTokens,
			call.TotalTokens,
		)
	}

	if call.DurationMS > 0 {
		fmt.Fprintf(b, " duration_ms=%d", call.DurationMS)
	}

	if call.EstimatedCostMicros > 0 {
		fmt.Fprintf(b, " estimated_cost_micros=%d", call.EstimatedCostMicros)
	}

	if call.BudgetRejectionRule != "" {
		fmt.Fprintf(
			b,
			" budget_rejection=%s used=%d limit=%d",
			markdownInline(call.BudgetRejectionRule),
			call.BudgetRejectionUsage,
			call.BudgetRejectionLimit,
		)
	}

	if call.Error != "" {
		fmt.Fprintf(b, " error=%s", markdownInline(call.Error))
	}

	writeRunCallTextField(b, "system_prompt", call.SystemPrompt)
	writeRunCallTextField(b, "user_prompt", call.UserPrompt)
	writeRunCallTextField(b, "response", call.Response)

	b.WriteByte('\n')
}

func writeRunCallTextField(b *strings.Builder, name, value string) {
	if value == "" {
		return
	}

	fmt.Fprintf(b, " %s=%s", name, markdownInline(truncateForMarkdownLine(value, 160)))
}

func writeRunTimeMetadata(b *strings.Builder, entry ExportMultiAgentRun) {
	if !entry.StartedAt.IsZero() {
		fmt.Fprintf(b, "  - **Started:** %s\n", entry.StartedAt.UTC().Format(time.RFC3339))
	}

	if entry.CompletedAt != nil && !entry.CompletedAt.IsZero() {
		fmt.Fprintf(b, "  - **Completed:** %s\n", entry.CompletedAt.UTC().Format(time.RFC3339))
	}

	if entry.Model != "" {
		fmt.Fprintf(b, "  - **Model:** %s\n", markdownInline(entry.Model))
	}
}

func writeRunSummary(b *strings.Builder, summary ExportMultiAgentRunSummary) {
	if summary.Winner != "" {
		fmt.Fprintf(b, "  - **Winner:** %s\n", markdownInline(summary.Winner))
	}

	if summary.Reason != "" {
		fmt.Fprintf(b, "  - **Reason:** %s\n", markdownInline(summary.Reason))
	}

	if summary.VerdictReviewer != "" {
		fmt.Fprintf(b, "  - **Verdict reviewer:** %s\n", markdownInline(summary.VerdictReviewer))
	}

	if summary.Findings > 0 {
		fmt.Fprintf(b, "  - **Findings:** %d\n", summary.Findings)
	}
}

func writeRunBudget(b *strings.Builder, budget MultiAgentRunBudget) {
	if budget.PerCallMaxInputTokens == 0 &&
		budget.PerCallMaxOutputTokens == 0 &&
		budget.MaxRunInputTokens == 0 &&
		budget.MaxRunOutputTokens == 0 &&
		budget.MaxRunTotalTokens == 0 &&
		budget.MaxModelCalls == 0 &&
		budget.MaxRunCostMicros == 0 &&
		budget.MaxRunWallTimeMS == 0 {
		return
	}

	fmt.Fprintf(
		b,
		"  - **Budget:** per_call_max_input_tokens=%d per_call_max_output_tokens=%d max_run_input_tokens=%d max_run_output_tokens=%d max_run_total_tokens=%d max_model_calls=%d max_run_cost_micros=%d max_run_wall_time_ms=%d\n",
		budget.PerCallMaxInputTokens,
		budget.PerCallMaxOutputTokens,
		budget.MaxRunInputTokens,
		budget.MaxRunOutputTokens,
		budget.MaxRunTotalTokens,
		budget.MaxModelCalls,
		budget.MaxRunCostMicros,
		budget.MaxRunWallTimeMS,
	)
}

func writeRunUsage(b *strings.Builder, usage MultiAgentRunUsage) {
	if usage.ModelCalls == 0 && usage.EstimatedInputTokens == 0 && usage.EstimatedOutputTokens == 0 &&
		usage.EstimatedTotalTokens == 0 && usage.InputTokens == 0 && usage.CachedInputTokens == 0 &&
		usage.OutputTokens == 0 && usage.TotalTokens == 0 && usage.EstimatedCostMicros == 0 &&
		usage.CompletedCalls == 0 && usage.CanceledCalls == 0 && usage.ProviderFailedCalls == 0 &&
		usage.BudgetRejectedCalls == 0 &&
		usage.DurationMS == 0 {
		return
	}

	fmt.Fprintf(
		b,
		"  - **Usage:** model_calls=%d completed_calls=%d canceled_calls=%d provider_failed_calls=%d budget_rejected_calls=%d estimated_input_tokens=%d estimated_output_tokens=%d estimated_total_tokens=%d estimated_cost_micros=%d input_tokens=%d cached_input_tokens=%d output_tokens=%d total_tokens=%d duration_ms=%d\n",
		usage.ModelCalls,
		usage.CompletedCalls,
		usage.CanceledCalls,
		usage.ProviderFailedCalls,
		usage.BudgetRejectedCalls,
		usage.EstimatedInputTokens,
		usage.EstimatedOutputTokens,
		usage.EstimatedTotalTokens,
		usage.EstimatedCostMicros,
		usage.InputTokens,
		usage.CachedInputTokens,
		usage.OutputTokens,
		usage.TotalTokens,
		usage.DurationMS,
	)
}

func writeNegativeKnowledge(b *strings.Builder, entries []ExportNegativeKnowledge) {
	if len(entries) == 0 {
		return
	}

	b.WriteString("\n## Negative Knowledge\n\n")

	for _, entry := range entries {
		if entry.Approach == "" && entry.Reason == "" {
			continue
		}

		fmt.Fprintf(b, "- **Approach:** %s\n", markdownInline(entry.Approach))

		if entry.Reason != "" {
			fmt.Fprintf(b, "  - **Reason:** %s\n", markdownInline(entry.Reason))
		}

		if entry.Commit != "" {
			fmt.Fprintf(b, "  - **Commit:** %s\n", markdownInline(entry.Commit))
		}

		if entry.Agent != "" {
			fmt.Fprintf(b, "  - **Agent:** %s\n", markdownInline(entry.Agent))
		}

		if entry.TaskType != "" {
			fmt.Fprintf(b, "  - **Task Type:** %s\n", markdownInline(entry.TaskType))
		}

		if entry.Severity != "" {
			fmt.Fprintf(b, "  - **Severity:** %s\n", markdownInline(entry.Severity))
		}

		if !entry.CreatedAt.IsZero() {
			fmt.Fprintf(b, "  - **Created:** %s\n", entry.CreatedAt.UTC().Format(time.RFC3339))
		}
	}
}

func writeTranscript(b *strings.Builder, export MachineReadableExport) {
	if len(export.Messages) == 0 {
		b.WriteString("\n_No messages._\n")
		return
	}

	b.WriteString("\n## Transcript\n\n")

	for _, message := range export.Messages {
		fmt.Fprintf(b, "### %s\n\n", markdownInline(roleTitle(message.Role)))
		b.WriteString(fencedMarkdown(message.Content, "text"))
		b.WriteByte('\n')

		if len(message.ToolCalls) > 0 {
			b.WriteString("**Tool calls:**\n\n")
			b.WriteString(fencedJSON(message.ToolCalls))
			b.WriteByte('\n')
		}

		if message.ToolResult != nil {
			b.WriteString("**Tool result:**\n\n")
			b.WriteString(fencedJSON(message.ToolResult))
			b.WriteByte('\n')
		}
	}
}

func writeMetadata(b *strings.Builder, label string, value time.Time) {
	if value.IsZero() {
		return
	}

	fmt.Fprintf(b, "- **%s:** %s\n", label, value.UTC().Format(time.RFC3339))
}

func writeMetadataString(b *strings.Builder, label, value string) {
	if value == "" {
		return
	}

	fmt.Fprintf(b, "- **%s:** %s\n", label, markdownInline(value))
}

func formatAgentLoopBudgetCompact(budget llm.AgentLoopBudget) string {
	if budget.IsZero() {
		return ""
	}

	parts := make([]string, 0, 9)
	if budget.MaxIterations > 0 {
		parts = append(parts, "iter="+strconv.Itoa(budget.MaxIterations))
	}

	if budget.MaxModelCalls > 0 {
		parts = append(parts, "model="+strconv.Itoa(budget.MaxModelCalls))
	}

	if budget.MaxToolCalls > 0 {
		parts = append(parts, "tool="+strconv.Itoa(budget.MaxToolCalls))
	}

	if budget.MaxWallTime > 0 {
		parts = append(parts, "wall="+budget.MaxWallTime.String())
	}

	if budget.MaxInputTokens > 0 {
		parts = append(parts, "in="+formatTokenCount(budget.MaxInputTokens))
	}

	if budget.MaxOutputTokens > 0 {
		parts = append(parts, "out="+formatTokenCount(budget.MaxOutputTokens))
	}

	if budget.MaxTotalTokens > 0 {
		parts = append(parts, "total="+formatTokenCount(budget.MaxTotalTokens))
	}

	if budget.MaxOutputBytes > 0 {
		parts = append(parts, "bytes="+strconv.FormatInt(budget.MaxOutputBytes, 10))
	}

	if budget.MaxCostMicros > 0 {
		parts = append(parts, "costµ="+strconv.FormatInt(budget.MaxCostMicros, 10))
	}

	return strings.Join(parts, ",")
}

func formatTokenCount(n int) string {
	switch {
	case n >= 1_000_000:
		s := strconv.FormatFloat(float64(n)/1_000_000, 'f', 1, 64)

		return s + "M"
	case n >= 1_000:
		s := strconv.FormatFloat(float64(n)/1_000, 'f', 1, 64)
		s = strings.TrimSuffix(s, ".0")

		return s + "k"
	default:
		return strconv.Itoa(n)
	}
}

func fencedJSON(value any) string {
	data, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		return fencedMarkdown(fmt.Sprintf("failed to encode attachment: %v", err), "text")
	}

	return fencedMarkdown(string(data), "json")
}

func fencedMarkdown(content, language string) string {
	if content == "" {
		return "_Empty message._\n"
	}

	fence := markdownFence(content)
	language = strings.TrimSpace(language)

	if language != "" {
		return fmt.Sprintf("%s%s\n%s\n%s\n", fence, language, content, fence)
	}

	return fmt.Sprintf("%s\n%s\n%s\n", fence, content, fence)
}

func markdownFence(content string) string {
	maxRun := 2
	run := 0

	for _, r := range content {
		if r == '`' {
			run++
			if run > maxRun {
				maxRun = run
			}

			continue
		}

		run = 0
	}

	return strings.Repeat("`", maxRun+1)
}

func markdownInline(value string) string {
	value = strings.Join(strings.Fields(value), " ")
	if value == "" {
		return ""
	}

	value = html.EscapeString(value)
	replacer := strings.NewReplacer(
		"\\", "\\\\",
		"`", "\\`",
		"*", "\\*",
		"_", "\\_",
		"{", "\\{",
		"}", "\\}",
		"[", "\\[",
		"]", "\\]",
		"(", "\\(",
		")", "\\)",
		"#", "\\#",
		"!", "\\!",
		"|", "\\|",
	)

	return replacer.Replace(value)
}

func truncateForMarkdownLine(value string, maxRunes int) string {
	value = strings.Join(strings.Fields(value), " ")
	if maxRunes <= 0 {
		return value
	}

	runes := []rune(value)
	if len(runes) <= maxRunes {
		return value
	}

	return string(runes[:maxRunes]) + "..."
}

func exportContentHashes(export MachineReadableExport) map[string]string {
	hashes := map[string]string{
		"session": hashJSON(export.Session),
	}

	if export.Provenance.ConfigHash != "" {
		hashes["provenance"] = hashJSON(export.Provenance)
	}

	if len(export.Messages) > 0 {
		hashes["messages"] = hashJSON(export.Messages)
	}

	if len(export.NegativeKnowledge) > 0 {
		hashes["negative_knowledge"] = hashJSON(export.NegativeKnowledge)
	}

	if len(export.Evaluations) > 0 {
		hashes["evaluations"] = hashJSON(export.Evaluations)
	}

	if len(export.Artifacts) > 0 {
		hashes["artifacts"] = hashJSON(export.Artifacts)
	}

	if len(export.MultiAgentRuns) > 0 {
		hashes["multi_agent_runs"] = hashJSON(export.MultiAgentRuns)
	}

	return hashes
}

func normalizedSessionSchemaVersion(version int) int {
	if version == 0 {
		return SessionSchemaVersion
	}

	return version
}

func sessionConfigHash(sessionState Session) string {
	//nolint:govet // Hash field order is stable and human-readable in tests.
	config := struct {
		AgentLoopBudget       llm.AgentLoopBudget `json:"agent_loop_budget,omitzero"`
		Autonomy              string              `json:"autonomy,omitempty"`
		DefaultAgent          string              `json:"default_agent,omitempty"`
		DefaultModel          string              `json:"default_model,omitempty"`
		DefaultModelMode      string              `json:"default_model_mode,omitempty"`
		DefaultReasoningLevel string              `json:"default_reasoning_level,omitempty"`
		PromptSuggestions     string              `json:"prompt_suggestions,omitempty"`
		Tags                  []string            `json:"tags,omitempty"`
		WorktreeBase          string              `json:"worktree_base,omitempty"`
		WorktreeBranch        string              `json:"worktree_branch,omitempty"`
		WorktreePath          string              `json:"worktree_path,omitempty"`
	}{
		AgentLoopBudget:       sessionState.AgentLoopBudget,
		Autonomy:              sessionState.Autonomy,
		DefaultAgent:          sessionState.DefaultAgent,
		DefaultModel:          sessionState.DefaultModel,
		DefaultModelMode:      sessionState.DefaultModelMode,
		DefaultReasoningLevel: sessionState.DefaultReasoningLevel,
		PromptSuggestions:     sessionState.PromptSuggestions,
		Tags:                  append([]string(nil), sessionState.Tags...),
		WorktreeBase:          sessionState.WorktreeBase,
		WorktreeBranch:        sessionState.WorktreeBranch,
		WorktreePath:          sessionState.WorktreePath,
	}

	return hashJSON(config)
}

func sessionProviders(sessionState Session) []string {
	values := []string{
		providerFromModel(sessionState.DefaultModel),
	}

	for index := range sessionState.ProviderCalls {
		call := &sessionState.ProviderCalls[index]
		values = append(
			values,
			call.Provider,
			providerFromModel(call.RequestedModel),
			providerFromModel(call.ResponseModel),
		)
	}

	if sessionState.BackgroundSuggestions != nil {
		values = append(values, sessionState.BackgroundSuggestions.LastProvider)
	}

	for index := range sessionState.Evaluations {
		evaluation := &sessionState.Evaluations[index]
		values = append(values, evaluation.Provider, providerFromModel(evaluation.Model))
	}

	for runIndex := range sessionState.MultiAgentRuns {
		run := &sessionState.MultiAgentRuns[runIndex]
		values = append(values, providerFromModel(run.Model))

		for callIndex := range run.Calls {
			call := &run.Calls[callIndex]
			values = append(values, providerFromModel(call.RequestedModel), providerFromModel(call.ResponseModel))
		}

		for branchIndex := range run.Branches {
			branch := &run.Branches[branchIndex]
			values = append(values, providerFromModel(branch.Model))
		}

		for _, reviewer := range run.Reviewers {
			values = append(values, providerFromModel(reviewer.Model))
		}
	}

	return uniqueSortedStrings(values)
}

func sessionModels(sessionState Session) []string {
	values := []string{sessionState.DefaultModel}

	for index := range sessionState.ProviderCalls {
		call := &sessionState.ProviderCalls[index]
		values = append(values, call.RequestedModel, call.ResponseModel)
		values = append(values, call.FallbackModels...)
	}

	if sessionState.BackgroundSuggestions != nil {
		values = append(values, sessionState.BackgroundSuggestions.LastModel)
	}

	for index := range sessionState.Evaluations {
		values = append(values, sessionState.Evaluations[index].Model)
	}

	for runIndex := range sessionState.MultiAgentRuns {
		run := &sessionState.MultiAgentRuns[runIndex]
		values = append(values, run.Model)
		values = append(values, run.FallbackModels...)

		for callIndex := range run.Calls {
			call := &run.Calls[callIndex]
			values = append(values, call.RequestedModel, call.ResponseModel)
			values = append(values, call.FallbackModels...)
		}

		for branchIndex := range run.Branches {
			branch := &run.Branches[branchIndex]
			values = append(values, branch.Model)
		}

		for _, reviewer := range run.Reviewers {
			values = append(values, reviewer.Model)
		}
	}

	return uniqueSortedStrings(values)
}

func sessionTokenUsage(sessionState Session) ExportTokenUsage {
	var usage ExportTokenUsage

	for index := range sessionState.ProviderCalls {
		call := &sessionState.ProviderCalls[index]
		usage.ModelCalls++
		usage.InputTokens += call.InputTokens
		usage.CachedInputTokens += call.CachedInputTokens
		usage.CacheWriteInputTokens += call.CacheWriteInputTokens
		usage.OutputTokens += call.OutputTokens
		usage.TotalTokens += call.TotalTokens
		usage.EstimatedInputTokens += call.EstimatedInputTokens
		usage.EstimatedOutputTokens += call.EstimatedOutputTokens
	}

	if sessionState.BackgroundSuggestions != nil {
		usage.ModelCalls += sessionState.BackgroundSuggestions.ProviderCalls
		usage.InputTokens += sessionState.BackgroundSuggestions.InputTokens
		usage.CachedInputTokens += sessionState.BackgroundSuggestions.CachedInputTokens
		usage.CacheWriteInputTokens += sessionState.BackgroundSuggestions.CacheWriteInputTokens
		usage.OutputTokens += sessionState.BackgroundSuggestions.OutputTokens
		usage.EstimatedInputTokens += sessionState.BackgroundSuggestions.EstimatedInputTokens
		usage.EstimatedOutputTokens += sessionState.BackgroundSuggestions.EstimatedOutputTokens
	}

	for index := range sessionState.Evaluations {
		evaluation := &sessionState.Evaluations[index]
		usage.InputTokens += evaluation.InputTokens
		usage.OutputTokens += evaluation.OutputTokens
		usage.TotalTokens += evaluation.TotalTokens
	}

	for runIndex := range sessionState.MultiAgentRuns {
		run := &sessionState.MultiAgentRuns[runIndex]
		usage = addRunTokenUsage(usage, *run)
	}

	if usage.TotalTokens == 0 {
		usage.TotalTokens = usage.InputTokens + usage.OutputTokens
	}

	if usage.EstimatedTotalTokens == 0 {
		usage.EstimatedTotalTokens = usage.EstimatedInputTokens + usage.EstimatedOutputTokens
	}

	return usage
}

func addRunTokenUsage(total ExportTokenUsage, run MultiAgentRun) ExportTokenUsage {
	calls := providerCallTokenUsage(run.Calls)

	total.ModelCalls += preferRecordedInt(run.Usage.ModelCalls, calls.ModelCalls)
	total.InputTokens += preferRecordedInt(run.Usage.InputTokens, calls.InputTokens)
	total.CachedInputTokens += preferRecordedInt(run.Usage.CachedInputTokens, calls.CachedInputTokens)
	total.OutputTokens += preferRecordedInt(run.Usage.OutputTokens, calls.OutputTokens)
	total.TotalTokens += preferRecordedInt(run.Usage.TotalTokens, calls.TotalTokens)
	total.EstimatedInputTokens += run.Usage.EstimatedInputTokens
	total.EstimatedOutputTokens += run.Usage.EstimatedOutputTokens
	total.EstimatedTotalTokens += run.Usage.EstimatedTotalTokens
	total.EstimatedCostMicros += preferRecordedInt64(run.Usage.EstimatedCostMicros, calls.EstimatedCostMicros)

	return total
}

func providerCallTokenUsage(calls []MultiAgentRunCall) ExportTokenUsage {
	var usage ExportTokenUsage

	for index := range calls {
		call := &calls[index]
		usage.ModelCalls++
		usage.InputTokens += call.InputTokens
		usage.CachedInputTokens += call.CachedInputTokens
		usage.OutputTokens += call.OutputTokens
		usage.TotalTokens += call.TotalTokens
		usage.EstimatedCostMicros += call.EstimatedCostMicros
	}

	return usage
}

func preferRecordedInt(recorded, fallback int) int {
	if recorded != 0 {
		return recorded
	}

	return fallback
}

func preferRecordedInt64(recorded, fallback int64) int64 {
	if recorded != 0 {
		return recorded
	}

	return fallback
}

func sessionFileReferences(sessionState Session) []ExportFileReference {
	entries := make([]ExportFileReference, 0, len(sessionState.Artifacts)+len(sessionState.Evaluations)+1)

	if strings.TrimSpace(sessionState.WorktreePath) != "" {
		entries = append(entries, ExportFileReference{
			Path:           sessionState.WorktreePath,
			Kind:           "worktree",
			Source:         "session.worktree",
			WorktreeBranch: sessionState.WorktreeBranch,
			WorktreeBase:   sessionState.WorktreeBase,
		})
	}

	for callIndex := range sessionState.ProviderCalls {
		call := &sessionState.ProviderCalls[callIndex]
		for refIndex := range call.ReferencedFiles {
			entries = append(entries, exportFileReferenceFromSessionReference(call.ReferencedFiles[refIndex]))
		}
	}

	for index := range sessionState.Artifacts {
		artifact := &sessionState.Artifacts[index]
		if strings.TrimSpace(artifact.Path) == "" {
			continue
		}

		entries = append(entries, ExportFileReference{
			Path:            artifact.Path,
			LogicalPath:     artifact.LogicalPath,
			Kind:            artifact.Kind,
			Source:          "artifact",
			SourceAgent:     artifact.SourceAgent,
			SourceSessionID: artifact.SourceSessionID,
			SHA256:          artifact.SHA256,
			SizeBytes:       artifact.SizeBytes,
			WorktreeBranch:  artifact.WorktreeBranch,
			WorktreeBase:    artifact.WorktreeBase,
		})
	}

	for index := range sessionState.Evaluations {
		evaluation := &sessionState.Evaluations[index]
		if strings.TrimSpace(evaluation.Reference) == "" {
			continue
		}

		entries = append(entries, ExportFileReference{
			Path:        evaluation.Reference,
			Kind:        "evaluation",
			Source:      "evaluation.reference",
			SourceAgent: evaluation.Agent,
		})
	}

	return uniqueFileReferences(entries)
}

func exportFileReferenceFromSessionReference(ref FileReference) ExportFileReference {
	return ExportFileReference(ref)
}

func sessionVerificationGates(sessionState Session) []ExportVerificationGate {
	var gates []ExportVerificationGate

	for runIndex := range sessionState.MultiAgentRuns {
		run := &sessionState.MultiAgentRuns[runIndex]

		for _, gate := range run.Gates {
			gates = append(gates, ExportVerificationGate{
				RunID:  run.ID,
				Name:   gate.Name,
				Phase:  gate.Phase,
				Agent:  gate.Agent,
				Notes:  gate.Notes,
				Passed: gate.Passed,
			})
		}
	}

	return gates
}

func uniqueSortedStrings(values []string) []string {
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

func uniqueFileReferences(entries []ExportFileReference) []ExportFileReference {
	seen := make(map[string]struct{}, len(entries))
	out := make([]ExportFileReference, 0, len(entries))

	for index := range entries {
		entry := &entries[index]

		key := strings.Join([]string{
			entry.Path,
			entry.LogicalPath,
			entry.Kind,
			entry.Source,
			entry.SourceAgent,
			entry.SHA256,
		}, "\x00")
		if _, ok := seen[key]; ok {
			continue
		}

		seen[key] = struct{}{}

		out = append(out, *entry)
	}

	return out
}

func hashJSON(value any) string {
	data, err := json.Marshal(value)
	if err != nil {
		data = []byte(fmt.Sprintf("%#v", value))
	}

	sum := sha256.Sum256(data)

	return "sha256:" + hex.EncodeToString(sum[:])
}

func writeIndentedMetadataString(b *strings.Builder, label, value string) {
	value = strings.TrimSpace(value)
	if value == "" {
		return
	}

	fmt.Fprintf(b, "  - **%s:** %s\n", label, markdownInline(value))
}

func roleTitle(role llm.Role) string {
	switch role {
	case llm.RoleUser:
		return "User"
	case llm.RoleAssistant:
		return "Assistant"
	case llm.RoleSystem:
		return "System"
	case llm.RoleTool:
		return "Tool"
	default:
		value := strings.Join(strings.Fields(string(role)), " ")
		if value == "" {
			return "Unknown"
		}

		runes := []rune(value)

		return strings.ToUpper(string(runes[0])) + string(runes[1:])
	}
}

func fallback(value, fallbackValue string) string {
	if value == "" {
		return fallbackValue
	}

	return value
}
