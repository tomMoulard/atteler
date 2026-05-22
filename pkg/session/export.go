package session

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"html"
	"regexp"
	"sort"
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
	NegativeKnowledge []ExportNegativeKnowledge `json:"negative_knowledge,omitempty"`
	Evaluations       []ExportAgentEvaluation   `json:"evaluations,omitempty"`
	Artifacts         []ExportArtifact          `json:"artifacts,omitempty"`
	Messages          []ExportMessage           `json:"messages,omitempty"`
}

// ExportSessionMetadata contains safe session-level metadata for exports.
type ExportSessionMetadata struct {
	CreatedAt              time.Time `json:"created_at,omitzero"`
	UpdatedAt              time.Time `json:"updated_at,omitzero"`
	ID                     string    `json:"id"`
	Title                  string    `json:"title,omitempty"`
	DefaultModel           string    `json:"default_model,omitempty"`
	DefaultReasoningLevel  string    `json:"default_reasoning_level,omitempty"`
	DefaultAgent           string    `json:"default_agent,omitempty"`
	WorktreePath           string    `json:"worktree_path,omitempty"`
	WorktreeBranch         string    `json:"worktree_branch,omitempty"`
	WorktreeBase           string    `json:"worktree_base,omitempty"`
	Tags                   []string  `json:"tags,omitempty"`
	MessageCount           int       `json:"message_count"`
	ExportedMessageCount   int       `json:"exported_message_count"`
	NegativeKnowledgeCount int       `json:"negative_knowledge_count"`
	EvaluationCount        int       `json:"evaluation_count"`
	ArtifactCount          int       `json:"artifact_count"`
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
	Model           string    `json:"model,omitempty"`
	AgentVersion    string    `json:"agent_version,omitempty"`
	SchemaVersion   int       `json:"schema_version,omitempty"`
	Score           int       `json:"score,omitempty"`
	DurationMillis  int64     `json:"duration_millis,omitempty"`
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
			WorktreePath:           builder.exportString("session.worktree_path", SearchFieldRepo, session.WorktreePath),
			WorktreeBranch:         builder.exportString("session.worktree_branch", SearchFieldRepo, session.WorktreeBranch),
			WorktreeBase:           builder.exportString("session.worktree_base", SearchFieldRepo, session.WorktreeBase),
			Tags:                   builder.exportSlice("session.tags", SearchFieldTags, session.Tags),
			MessageCount:           len(session.Messages),
			NegativeKnowledgeCount: len(session.NegativeKnowledge),
			EvaluationCount:        len(session.Evaluations),
			ArtifactCount:          len(session.Artifacts),
		},
	}

	export.NegativeKnowledge = builder.exportNegativeKnowledge(session.NegativeKnowledge)
	export.Evaluations = builder.exportEvaluations(session.Evaluations)
	export.Artifacts = builder.exportArtifacts(session.Artifacts)
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
			Model:           builder.exportString(prefix+".model", SearchFieldModel, entry.Model),
			AgentVersion:    builder.exportString(prefix+".agent_version", SearchFieldEvaluations, entry.AgentVersion),
			SchemaVersion:   entry.SchemaVersion,
			Score:           entry.Score,
			DurationMillis:  entry.DurationMillis,
			Cost:            entry.Cost,
			Confidence:      entry.Confidence,
		})
	}

	return exported
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
	writeMetadataString(&b, "Worktree", export.Session.WorktreePath)
	writeMetadataString(&b, "Branch", export.Session.WorktreeBranch)
	writeMetadataString(&b, "Base", export.Session.WorktreeBase)
	writeMetadataString(&b, "Tags", strings.Join(export.Session.Tags, ", "))

	writeManifest(&b, export.Manifest)

	if export.Manifest.RedactionProfile == ExportProfileIssue {
		writeIssueSummary(&b, export)
		return b.String()
	}

	writeNegativeKnowledge(&b, export.NegativeKnowledge)
	writeEvaluations(&b, export.Evaluations)
	writeArtifacts(&b, export.Artifacts)
	writeTranscript(&b, export)

	return b.String()
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

	writeNegativeKnowledge(b, export.NegativeKnowledge)
	writeEvaluations(b, export.Evaluations)
	writeArtifacts(b, export.Artifacts)
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
	writeIndentedMetadataString(b, "Model", entry.Model)
	writeIndentedMetadataString(b, "Agent Version", entry.AgentVersion)

	if entry.SchemaVersion != 0 {
		fmt.Fprintf(b, "  - **Schema Version:** %d\n", entry.SchemaVersion)
	}

	if entry.DurationMillis != 0 {
		fmt.Fprintf(b, "  - **Duration Millis:** %d\n", entry.DurationMillis)
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

func exportContentHashes(export MachineReadableExport) map[string]string {
	hashes := map[string]string{
		"session": hashJSON(export.Session),
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

	return hashes
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
