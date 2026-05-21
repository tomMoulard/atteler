package feedback

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"maps"
	"sort"
	"strings"
	"time"
	"unicode"

	"github.com/tommoulard/atteler/pkg/config"
)

const (
	feedbackGuidanceHeader = "Feedback-derived guidance:"
	defaultReviewer        = "feedback-apply"
	unknownSourceRun       = "unknown"
)

const (
	// GuidanceStatusPending records reviewed guidance that is not yet active.
	GuidanceStatusPending = "pending"
	// GuidanceStatusApproved records guidance that can be rendered into prompts.
	GuidanceStatusApproved = "approved"
	// GuidanceStatusQuarantined records guidance rejected before activation.
	GuidanceStatusQuarantined = "quarantined"
	// GuidanceStatusRolledBack records guidance that was explicitly deactivated.
	GuidanceStatusRolledBack = "rolled_back"
)

const (
	auditActionApproved    = "approved"
	auditActionPending     = "pending"
	auditActionQuarantined = "quarantined"
	auditActionRolledBack  = "rolled_back"
)

// ApplyOptions captures provenance and lifecycle defaults for feedback guidance.
//
//nolint:govet // Field order keeps optional provenance defaults grouped for callers.
type ApplyOptions struct {
	ExpiresAt *time.Time
	SourceRun string
	Reviewer  string
	Status    string
	Now       time.Time
}

// HistoryEntry records one feedback guidance lifecycle decision for an agent.
type HistoryEntry struct {
	ExpiresAt      *time.Time
	CreatedAt      time.Time
	UpdatedAt      time.Time
	ID             string
	Agent          string
	Status         string
	Action         string
	Reason         string
	SourceRun      string
	Reviewer       string
	RollbackReason string
	Evidence       []string
	ConflictsWith  []string
	Confidence     float64
}

// ApplyProposals stores feedback proposals as pending structured guidance
// records and returns a copied agent map plus stable history entries for newly
// recorded decisions.
//
// Proposals for agents not present in agents are ignored. Reapplying the same
// proposal is idempotent by stable guidance ID: the existing record is left
// untouched and no duplicate history entry is returned. Guidance recorded
// through this compatibility helper requires explicit approval before
// RenderSystemPrompt can render it into a runtime prompt.
func ApplyProposals(agents map[string]config.AgentConfig, proposals []Proposal) (map[string]config.AgentConfig, []HistoryEntry) {
	return ApplyProposalsWithOptions(agents, proposals, ApplyOptions{})
}

// ProposalID returns the stable guidance ID that would be used for proposal.
func ProposalID(proposal Proposal) string {
	return guidanceID(proposal)
}

// ApplyProposalsWithOptions stores proposals using explicit provenance and
// lifecycle options. Status defaults to pending; pass GuidanceStatusApproved
// only after an external review has already approved the proposal.
func ApplyProposalsWithOptions(
	agents map[string]config.AgentConfig,
	proposals []Proposal,
	options ApplyOptions,
) (map[string]config.AgentConfig, []HistoryEntry) {
	updated := copyAgents(agents)
	if len(updated) == 0 || len(proposals) == 0 {
		return updated, nil
	}

	now := normalizeTime(options.Now)
	entries := make([]HistoryEntry, 0, len(proposals))

	for i := range proposals {
		proposal := proposals[i]

		agentName, ok := configuredAgentName(updated, proposal.Agent)
		if !ok {
			continue
		}

		record := guidanceRecordFromProposal(proposal, options, now)
		if guidanceRecordEmpty(record) {
			continue
		}

		agent := updated[agentName]
		if guidanceIndexByID(agent.FeedbackGuidance, record.ID) >= 0 {
			continue
		}

		if reason := applyBlocker(record, now); reason != "" {
			record = quarantineGuidance(record, now, record.Reviewer, reason)
		} else if conflicts := contradictoryGuidanceIDs(agent.FeedbackGuidance, record, now); len(conflicts) > 0 {
			record.ConflictWith = conflicts
			record = quarantineGuidance(record, now, record.Reviewer, "contradicts active guidance")
		}

		agent.FeedbackGuidance = append(agent.FeedbackGuidance, record)
		updated[agentName] = agent
		entries = append(entries, historyEntryFromRecord(agentName, record))
	}

	return updated, entries
}

// ApproveGuidance activates a pending guidance record when it does not conflict
// with current active guidance. The existing record remains in place when the ID
// is unknown, already rolled back, expired, or contradictory.
func ApproveGuidance(
	agents map[string]config.AgentConfig,
	agentName string,
	guidanceID string,
	reviewer string,
	now time.Time,
) (map[string]config.AgentConfig, HistoryEntry, bool) {
	updated := copyAgents(agents)

	resolvedName, ok := configuredAgentName(updated, agentName)
	if !ok {
		return updated, HistoryEntry{}, false
	}

	agent := updated[resolvedName]

	idx := guidanceIndexByID(agent.FeedbackGuidance, guidanceID)
	if idx < 0 {
		return updated, HistoryEntry{}, false
	}

	now = normalizeTime(now)

	record := agent.FeedbackGuidance[idx]
	if record.Status != GuidanceStatusPending {
		return updated, HistoryEntry{}, false
	}

	approver := reviewerOrDefault(reviewer)
	if reason := approvalBlocker(record, now); reason != "" {
		record = quarantineGuidance(record, now, approver, reason)
		agent.FeedbackGuidance[idx] = record
		updated[resolvedName] = agent

		return updated, historyEntryFromRecord(resolvedName, record), true
	}

	if conflicts := contradictoryGuidanceIDsExcluding(agent.FeedbackGuidance, record, now, record.ID); len(conflicts) > 0 {
		record.ConflictWith = conflicts
		record = quarantineGuidance(record, now, approver, "contradicts active guidance")
		agent.FeedbackGuidance[idx] = record
		updated[resolvedName] = agent

		return updated, historyEntryFromRecord(resolvedName, record), true
	}

	record.Status = GuidanceStatusApproved
	record.Reviewer = approver
	record.UpdatedAt = now
	record.Audit = append(record.Audit, auditEvent(now, approver, auditActionApproved, "approved for runtime prompt rendering"))
	agent.FeedbackGuidance[idx] = record
	updated[resolvedName] = agent

	return updated, historyEntryFromRecord(resolvedName, record), true
}

// RollbackGuidance deactivates a guidance record without removing its audit trail.
func RollbackGuidance(
	agents map[string]config.AgentConfig,
	agentName string,
	guidanceID string,
	reviewer string,
	reason string,
	now time.Time,
) (map[string]config.AgentConfig, HistoryEntry, bool) {
	updated := copyAgents(agents)

	resolvedName, ok := configuredAgentName(updated, agentName)
	if !ok {
		return updated, HistoryEntry{}, false
	}

	agent := updated[resolvedName]

	idx := guidanceIndexByID(agent.FeedbackGuidance, guidanceID)
	if idx < 0 {
		return updated, HistoryEntry{}, false
	}

	record := agent.FeedbackGuidance[idx]
	if record.Status == GuidanceStatusRolledBack {
		return updated, HistoryEntry{}, false
	}

	now = normalizeTime(now)

	record.Status = GuidanceStatusRolledBack
	record.UpdatedAt = now
	record.RollbackReason = strings.TrimSpace(reason)
	record.Audit = append(record.Audit, auditEvent(now, reviewerOrDefault(reviewer), auditActionRolledBack, record.RollbackReason))
	agent.FeedbackGuidance[idx] = record
	updated[resolvedName] = agent

	return updated, historyEntryFromRecord(resolvedName, record), true
}

// RenderSystemPrompt returns systemPrompt with currently approved guidance
// appended in deterministic order. Pending, quarantined, rolled-back, expired,
// or evidence-free records are omitted.
func RenderSystemPrompt(systemPrompt string, records []config.FeedbackGuidance, now time.Time) string {
	block := renderGuidanceBlock(records, now)
	if block == "" {
		return systemPrompt
	}

	return appendSystemPromptGuidance(systemPrompt, block)
}

// FormatHistoryEntry formats a stable, human-readable feedback history entry.
func FormatHistoryEntry(entry HistoryEntry) string {
	var b strings.Builder
	fmt.Fprintf(&b, "id: %s\n", strings.TrimSpace(entry.ID))
	fmt.Fprintf(&b, "agent: %s\n", strings.TrimSpace(entry.Agent))
	fmt.Fprintf(&b, "status: %s\n", strings.TrimSpace(entry.Status))
	fmt.Fprintf(&b, "confidence: %.2f\n", entry.Confidence)

	if sourceRun := strings.TrimSpace(entry.SourceRun); sourceRun != "" {
		fmt.Fprintf(&b, "source_run: %s\n", sourceRun)
	}

	if reviewer := strings.TrimSpace(entry.Reviewer); reviewer != "" {
		fmt.Fprintf(&b, "reviewer: %s\n", reviewer)
	}

	if !entry.CreatedAt.IsZero() {
		fmt.Fprintf(&b, "created_at: %s\n", entry.CreatedAt.UTC().Format(time.RFC3339))
	}

	if !entry.UpdatedAt.IsZero() {
		fmt.Fprintf(&b, "updated_at: %s\n", entry.UpdatedAt.UTC().Format(time.RFC3339))
	}

	if entry.ExpiresAt != nil && !entry.ExpiresAt.IsZero() {
		fmt.Fprintf(&b, "expires_at: %s\n", entry.ExpiresAt.UTC().Format(time.RFC3339))
	}

	if action := strings.TrimSpace(entry.Action); action != "" {
		fmt.Fprintf(&b, "action: %s\n", action)
	}

	if reason := strings.TrimSpace(entry.Reason); reason != "" {
		fmt.Fprintf(&b, "reason: %s\n", reason)
	}

	evidence := cleanStrings(entry.Evidence)
	if len(evidence) > 0 {
		b.WriteString("evidence:\n")

		for _, item := range evidence {
			fmt.Fprintf(&b, "  - %s\n", item)
		}
	}

	conflicts := cleanStrings(entry.ConflictsWith)
	if len(conflicts) > 0 {
		b.WriteString("conflicts_with:\n")

		for _, item := range conflicts {
			fmt.Fprintf(&b, "  - %s\n", item)
		}
	}

	if rollbackReason := strings.TrimSpace(entry.RollbackReason); rollbackReason != "" {
		fmt.Fprintf(&b, "rollback_reason: %s\n", rollbackReason)
	}

	return b.String()
}

func guidanceRecordFromProposal(proposal Proposal, options ApplyOptions, now time.Time) config.FeedbackGuidance {
	expiresAt := options.ExpiresAt
	if proposal.ExpiresAt != nil {
		expiresAt = proposal.ExpiresAt
	}

	idProposal := proposal
	if strings.TrimSpace(idProposal.SourceRun) == "" {
		idProposal.SourceRun = options.SourceRun
	}

	record := config.FeedbackGuidance{
		ID:         guidanceID(idProposal),
		Status:     normalizeApplyStatus(options.Status),
		SourceRun:  sourceRunOrDefault(proposal.SourceRun, options.SourceRun),
		Action:     strings.TrimSpace(proposal.Action),
		Reason:     strings.TrimSpace(proposal.Reason),
		Evidence:   cleanStrings(proposal.Evidence),
		Confidence: proposal.Confidence,
		Reviewer:   reviewerOrDefault(firstNonEmpty(proposal.Reviewer, options.Reviewer)),
		CreatedAt:  now,
		UpdatedAt:  now,
	}

	if expiresAt != nil {
		expiresAtValue := expiresAt.UTC()
		record.ExpiresAt = &expiresAtValue
	}

	action := auditActionApproved
	if record.Status == GuidanceStatusPending {
		action = auditActionPending
	}

	record.Audit = append(record.Audit, auditEvent(now, record.Reviewer, action, "recorded feedback-derived guidance"))

	return record
}

func guidanceID(proposal Proposal) string {
	if id := strings.TrimSpace(proposal.ID); id != "" {
		return id
	}

	hash := sha256.Sum256([]byte(strings.Join([]string{
		strings.ToLower(strings.TrimSpace(proposal.Agent)),
		strings.TrimSpace(proposal.SourceRun),
		normalizeWords(proposal.Action),
		normalizeWords(proposal.Reason),
		strings.Join(normalizeStringSlice(proposal.Evidence), "\x00"),
	}, "\x1f")))

	return "fg_" + hex.EncodeToString(hash[:])[:16]
}

func guidanceRecordEmpty(record config.FeedbackGuidance) bool {
	return strings.TrimSpace(record.Action) == "" &&
		strings.TrimSpace(record.Reason) == "" &&
		len(cleanStrings(record.Evidence)) == 0
}

func guidanceIndexByID(records []config.FeedbackGuidance, guidanceID string) int {
	guidanceID = strings.TrimSpace(guidanceID)
	if guidanceID == "" {
		return -1
	}

	for i := range records {
		if strings.TrimSpace(records[i].ID) == guidanceID {
			return i
		}
	}

	return -1
}

func quarantineGuidance(record config.FeedbackGuidance, now time.Time, actor, reason string) config.FeedbackGuidance {
	actor = reviewerOrDefault(actor)
	record.Status = GuidanceStatusQuarantined
	record.Reviewer = actor
	record.UpdatedAt = now
	record.Audit = append(record.Audit, auditEvent(now, actor, auditActionQuarantined, reason))

	return record
}

func historyEntryFromRecord(agent string, record config.FeedbackGuidance) HistoryEntry {
	return HistoryEntry{
		ID:             strings.TrimSpace(record.ID),
		Agent:          strings.TrimSpace(agent),
		Status:         strings.TrimSpace(record.Status),
		Action:         strings.TrimSpace(record.Action),
		Reason:         strings.TrimSpace(record.Reason),
		SourceRun:      strings.TrimSpace(record.SourceRun),
		Reviewer:       strings.TrimSpace(record.Reviewer),
		Evidence:       cleanStrings(record.Evidence),
		Confidence:     record.Confidence,
		CreatedAt:      record.CreatedAt,
		UpdatedAt:      record.UpdatedAt,
		ExpiresAt:      copyTimePtr(record.ExpiresAt),
		ConflictsWith:  cleanStrings(record.ConflictWith),
		RollbackReason: strings.TrimSpace(record.RollbackReason),
	}
}

func renderGuidanceBlock(records []config.FeedbackGuidance, now time.Time) string {
	active := activeGuidance(records, now)
	if len(active) == 0 {
		return ""
	}

	var lines []string

	lines = append(lines, feedbackGuidanceHeader)

	for i := range active {
		record := active[i]

		action := strings.TrimSpace(record.Action)
		if action == "" {
			action = strings.TrimSpace(record.Reason)
		}

		if action == "" && len(record.Evidence) > 0 {
			action = record.Evidence[0]
		}

		lines = append(lines, "- Action: "+action)

		if id := strings.TrimSpace(record.ID); id != "" {
			lines = append(lines, "  ID: "+id)
		}

		if sourceRun := strings.TrimSpace(record.SourceRun); sourceRun != "" {
			lines = append(lines, "  Source run: "+sourceRun)
		}

		if reviewer := strings.TrimSpace(record.Reviewer); reviewer != "" {
			lines = append(lines, "  Reviewer: "+reviewer)
		}

		if record.Confidence != 0 {
			lines = append(lines, fmt.Sprintf("  Confidence: %.2f", record.Confidence))
		}

		if reason := strings.TrimSpace(record.Reason); reason != "" && reason != action {
			lines = append(lines, "  Reason: "+reason)
		}

		for _, evidence := range cleanStrings(record.Evidence) {
			lines = append(lines, "  Evidence: "+evidence)
		}
	}

	return strings.Join(lines, "\n")
}

func activeGuidance(records []config.FeedbackGuidance, now time.Time) []config.FeedbackGuidance {
	active := make([]config.FeedbackGuidance, 0, len(records))

	for i := range records {
		record := records[i]
		if !guidanceActive(record, now) {
			continue
		}

		active = append(active, cloneFeedbackGuidanceRecord(record))
	}

	sort.SliceStable(active, func(i, j int) bool {
		left := active[i]

		right := active[j]
		if !left.CreatedAt.Equal(right.CreatedAt) {
			return left.CreatedAt.Before(right.CreatedAt)
		}

		return strings.TrimSpace(left.ID) < strings.TrimSpace(right.ID)
	})

	return active
}

func guidanceActive(record config.FeedbackGuidance, now time.Time) bool {
	if strings.TrimSpace(record.Status) != GuidanceStatusApproved {
		return false
	}

	if approvalBlocker(record, now) != "" {
		return false
	}

	return strings.TrimSpace(record.Action) != "" || strings.TrimSpace(record.Reason) != ""
}

func approvalBlocker(record config.FeedbackGuidance, now time.Time) string {
	if guidanceExpired(record, now) {
		return "guidance expired before approval"
	}

	if strings.TrimSpace(record.ID) == "" ||
		strings.TrimSpace(record.SourceRun) == "" ||
		strings.TrimSpace(record.SourceRun) == unknownSourceRun ||
		strings.TrimSpace(record.Reviewer) == "" ||
		record.CreatedAt.IsZero() ||
		record.UpdatedAt.IsZero() ||
		len(record.Audit) == 0 {
		return "missing provenance"
	}

	if len(cleanStrings(record.Evidence)) == 0 {
		return "missing evidence"
	}

	return ""
}

func applyBlocker(record config.FeedbackGuidance, now time.Time) string {
	if len(cleanStrings(record.Evidence)) == 0 {
		return "missing evidence"
	}

	if guidanceExpired(record, now) {
		return "guidance expired before approval"
	}

	if strings.TrimSpace(record.Status) == GuidanceStatusApproved {
		return approvalBlocker(record, now)
	}

	return ""
}

func guidanceExpired(record config.FeedbackGuidance, now time.Time) bool {
	if record.ExpiresAt == nil || record.ExpiresAt.IsZero() {
		return false
	}

	now = normalizeTime(now)

	return !record.ExpiresAt.After(now)
}

func contradictoryGuidanceIDs(
	records []config.FeedbackGuidance,
	candidate config.FeedbackGuidance,
	now time.Time,
) []string {
	return contradictoryGuidanceIDsExcluding(records, candidate, now, "")
}

func contradictoryGuidanceIDsExcluding(
	records []config.FeedbackGuidance,
	candidate config.FeedbackGuidance,
	now time.Time,
	excludeID string,
) []string {
	conflicts := make([]string, 0)
	active := activeGuidance(records, now)

	for i := range active {
		existing := active[i]
		if strings.TrimSpace(existing.ID) == strings.TrimSpace(excludeID) {
			continue
		}

		if contradictoryActions(existing.Action, candidate.Action) {
			conflicts = append(conflicts, strings.TrimSpace(existing.ID))
		}
	}

	sort.Strings(conflicts)

	return conflicts
}

func contradictoryActions(left, right string) bool {
	leftKind, leftSubject := directiveKindAndSubject(left)
	rightKind, rightSubject := directiveKindAndSubject(right)

	if leftKind == "" || rightKind == "" || leftSubject == "" || rightSubject == "" {
		return false
	}

	return leftSubject == rightSubject && leftKind != rightKind
}

func directiveKindAndSubject(action string) (kind, subject string) {
	normalized := normalizeWords(action)
	if normalized == "" {
		return "", ""
	}

	prohibitivePrefixes := []string{"do not ", "don t ", "dont ", "never ", "avoid ", "skip ", "stop "}
	for _, prefix := range prohibitivePrefixes {
		if subject, ok := strings.CutPrefix(normalized, prefix); ok {
			return "prohibit", strings.TrimSpace(subject)
		}
	}

	mandatoryPrefixes := []string{"always ", "must ", "require ", "requires ", "required ", "do "}
	for _, prefix := range mandatoryPrefixes {
		if subject, ok := strings.CutPrefix(normalized, prefix); ok {
			return "require", strings.TrimSpace(subject)
		}
	}

	return "", ""
}

func copyAgents(agents map[string]config.AgentConfig) map[string]config.AgentConfig {
	if len(agents) == 0 {
		return nil
	}

	copied := make(map[string]config.AgentConfig, len(agents))
	for name := range agents {
		copied[name] = copyAgentConfig(agents[name])
	}

	return copied
}

func copyAgentConfig(agent config.AgentConfig) config.AgentConfig {
	agent.FallbackModels = append([]string(nil), agent.FallbackModels...)
	agent.Capabilities = append([]string(nil), agent.Capabilities...)
	agent.Triggers = append([]string(nil), agent.Triggers...)
	agent.References = append([]string(nil), agent.References...)
	agent.FeedbackGuidance = cloneFeedbackGuidance(agent.FeedbackGuidance)

	if agent.ToolPermissions != nil {
		agent.ToolPermissions = maps.Clone(agent.ToolPermissions)
	}

	if agent.Temperature != nil {
		value := *agent.Temperature
		agent.Temperature = &value
	}

	if agent.TopP != nil {
		value := *agent.TopP
		agent.TopP = &value
	}

	if agent.Seed != nil {
		value := *agent.Seed
		agent.Seed = &value
	}

	return agent
}

func configuredAgentName(agents map[string]config.AgentConfig, proposalAgent string) (string, bool) {
	trimmed := strings.TrimSpace(proposalAgent)
	if trimmed == "" {
		return "", false
	}

	if _, ok := agents[trimmed]; ok {
		return trimmed, true
	}

	normalized := strings.ToLower(trimmed)
	matches := make([]string, 0, 1)

	for name := range agents {
		if strings.ToLower(strings.TrimSpace(name)) == normalized {
			matches = append(matches, name)
		}
	}

	if len(matches) != 1 {
		return "", false
	}

	return matches[0], true
}

func appendSystemPromptGuidance(systemPrompt, guidance string) string {
	trimmedPrompt := strings.TrimSpace(systemPrompt)
	if trimmedPrompt == "" {
		return guidance
	}

	return trimmedPrompt + "\n\n" + guidance
}

func cleanStrings(values []string) []string {
	cleaned := make([]string, 0, len(values))
	for _, value := range values {
		if trimmed := strings.TrimSpace(value); trimmed != "" {
			cleaned = append(cleaned, trimmed)
		}
	}

	return cleaned
}

func normalizeStringSlice(values []string) []string {
	normalized := make([]string, 0, len(values))
	for _, value := range cleanStrings(values) {
		normalized = append(normalized, normalizeWords(value))
	}

	return normalized
}

func normalizeWords(value string) string {
	var b strings.Builder

	lastWasSpace := true

	for _, r := range strings.ToLower(strings.TrimSpace(value)) {
		if unicode.IsLetter(r) || unicode.IsDigit(r) {
			b.WriteRune(r)

			lastWasSpace = false

			continue
		}

		if !lastWasSpace {
			b.WriteByte(' ')

			lastWasSpace = true
		}
	}

	return strings.TrimSpace(b.String())
}

func normalizeApplyStatus(status string) string {
	switch strings.TrimSpace(status) {
	case GuidanceStatusPending:
		return GuidanceStatusPending
	case GuidanceStatusApproved:
		return GuidanceStatusApproved
	default:
		return GuidanceStatusPending
	}
}

func normalizeTime(value time.Time) time.Time {
	if value.IsZero() {
		return time.Now().UTC()
	}

	return value.UTC()
}

func auditEvent(at time.Time, actor, action, reason string) config.FeedbackGuidanceAuditEvent {
	return config.FeedbackGuidanceAuditEvent{
		At:     normalizeTime(at),
		Actor:  reviewerOrDefault(actor),
		Action: strings.TrimSpace(action),
		Reason: strings.TrimSpace(reason),
	}
}

func sourceRunOrDefault(values ...string) string {
	if value := firstNonEmpty(values...); value != "" {
		return value
	}

	return unknownSourceRun
}

func reviewerOrDefault(values ...string) string {
	if value := firstNonEmpty(values...); value != "" {
		return value
	}

	return defaultReviewer
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if trimmed := strings.TrimSpace(value); trimmed != "" {
			return trimmed
		}
	}

	return ""
}

func copyTimePtr(value *time.Time) *time.Time {
	if value == nil {
		return nil
	}

	copied := value.UTC()

	return &copied
}

func cloneFeedbackGuidance(records []config.FeedbackGuidance) []config.FeedbackGuidance {
	if len(records) == 0 {
		return nil
	}

	out := make([]config.FeedbackGuidance, 0, len(records))

	for i := range records {
		out = append(out, cloneFeedbackGuidanceRecord(records[i]))
	}

	return out
}

func cloneFeedbackGuidanceRecord(record config.FeedbackGuidance) config.FeedbackGuidance {
	next := record
	next.Evidence = append([]string(nil), record.Evidence...)
	next.ConflictWith = append([]string(nil), record.ConflictWith...)
	next.Audit = append([]config.FeedbackGuidanceAuditEvent(nil), record.Audit...)
	next.ExpiresAt = copyTimePtr(record.ExpiresAt)

	return next
}
