package feedback

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"maps"
	"sort"
	"strings"

	"github.com/tommoulard/atteler/pkg/config"
)

const (
	feedbackGuidanceHeader = "Feedback-derived guidance:"
	learningIDLabel        = "- Learning ID: "
	rootCauseLabel         = "- Root cause: "
	targetBehaviorLabel    = "- Target behavior: "

	// PatchStatusAccepted means an audited prompt patch has passing eval or fixture proof.
	PatchStatusAccepted = "accepted"
)

// ApplyOptions captures the approval boundary metadata for audited feedback patches.
type ApplyOptions struct {
	Author string
	Source string
}

// HistoryEntry records one audited feedback proposal applied to an agent configuration.
//
//nolint:govet // Field order follows audit-log readability instead of pointer packing.
type HistoryEntry struct {
	Agent                string
	Action               string
	Reason               string
	RootCause            RootCauseClassification
	TargetBehavior       string
	RejectedAlternatives []RejectedAlternative
	Evidence             []string
	LinkedEvidence       []EvidenceLink
	Verification         []VerificationRecord
	Confidence           float64
	Author               string
	Source               string
	Status               string
	LearningID           string
	BeforePromptHash     string
	AfterPromptHash      string
	Diff                 string
	RollbackDiff         string
	RollbackInstructions string

	// BeforePrompt is kept in memory so callers can perform a safe rollback after
	// verifying the current prompt still matches AfterPromptHash. It is not emitted
	// by FormatHistoryEntry to avoid duplicating the full prompt in audit logs.
	BeforePrompt string
}

type guidanceBlock struct {
	RootCauseCategory string
	TargetBehavior    string
	LearningID        string
}

// ApplyProposals applies feedback proposals to configured agents and returns a
// copied agent map plus stable history entries for newly applied guidance.
//
// A proposal is only marked accepted and applied when it carries at least one
// passing after-phase eval or fixture. Proposals for agents not present in agents
// are ignored. Reapplying the same audited learning ID is idempotent.
func ApplyProposals(agents map[string]config.AgentConfig, proposals []Proposal) (map[string]config.AgentConfig, []HistoryEntry) {
	return ApplyProposalsWithOptions(agents, proposals, ApplyOptions{})
}

// ApplyProposalsWithOptions is ApplyProposals with explicit audit metadata.
func ApplyProposalsWithOptions(
	agents map[string]config.AgentConfig,
	proposals []Proposal,
	options ApplyOptions,
) (map[string]config.AgentConfig, []HistoryEntry) {
	updated := copyAgents(agents)
	if len(updated) == 0 || len(proposals) == 0 {
		return updated, nil
	}

	options = normalizeApplyOptions(options)

	entries := make([]HistoryEntry, 0, len(proposals))
	for i := range proposals {
		proposal := proposals[i]

		agentName, ok := configuredAgentName(updated, proposal.Agent)
		if !ok {
			continue
		}

		proposal = normalizeProposalForApply(agentName, proposal)
		if !hasPassingProof(proposal.Verification) {
			continue
		}

		guidance := proposalGuidance(proposal)
		if guidance == "" {
			continue
		}

		agent := updated[agentName]
		if hasDuplicateGuidance(agent.SystemPrompt, proposal) || hasContradictoryGuidance(agent.SystemPrompt, proposal) {
			continue
		}

		before := agent.SystemPrompt
		after := appendSystemPromptGuidance(before, guidance)

		if before == after {
			continue
		}

		agent.SystemPrompt = after
		updated[agentName] = agent

		beforeHash := PromptHash(before)
		afterHash := PromptHash(after)
		entries = append(entries, HistoryEntry{
			Agent:                agentName,
			Action:               strings.TrimSpace(proposal.Action),
			Reason:               strings.TrimSpace(proposal.Reason),
			RootCause:            cleanRootCause(proposal.RootCause),
			TargetBehavior:       strings.TrimSpace(proposal.TargetBehavior),
			RejectedAlternatives: cleanRejectedAlternatives(proposal.RejectedAlternatives),
			Evidence:             cleanStrings(proposal.Evidence),
			LinkedEvidence:       cleanEvidenceLinks(proposal.LinkedEvidence),
			Verification:         cleanVerificationRecords(proposal.Verification),
			Confidence:           proposal.Confidence,
			Author:               options.Author,
			Source:               options.Source,
			Status:               PatchStatusAccepted,
			LearningID:           proposalLearningID(proposal),
			BeforePromptHash:     beforeHash,
			AfterPromptHash:      afterHash,
			Diff:                 promptDiff(agentName, before, after),
			RollbackDiff:         promptDiff(agentName, after, before),
			RollbackInstructions: rollbackInstructions(agentName, beforeHash, afterHash),
			BeforePrompt:         before,
		})
	}

	return updated, entries
}

// PromptHash returns a stable SHA-256 digest for a prompt body.
func PromptHash(prompt string) string {
	sum := sha256.Sum256([]byte(prompt))

	return "sha256:" + hex.EncodeToString(sum[:])
}

// Rollback restores the prompt recorded by an audited history entry when the
// current prompt still matches the entry's after hash.
func Rollback(agents map[string]config.AgentConfig, entry HistoryEntry) (map[string]config.AgentConfig, error) {
	return RollbackHistoryEntry(agents, entry)
}

// RollbackHistoryEntry restores the prompt recorded by an audited history entry
// when the current prompt still matches the entry's after hash.
func RollbackHistoryEntry(agents map[string]config.AgentConfig, entry HistoryEntry) (map[string]config.AgentConfig, error) {
	updated := copyAgents(agents)
	if len(updated) == 0 {
		return updated, errors.New("feedback rollback: no configured agents")
	}

	agentName, ok := configuredAgentName(updated, entry.Agent)
	if !ok {
		return updated, fmt.Errorf("feedback rollback: unknown agent %q", entry.Agent)
	}

	agent := updated[agentName]
	if got := PromptHash(agent.SystemPrompt); got != strings.TrimSpace(entry.AfterPromptHash) {
		return updated, fmt.Errorf("feedback rollback: prompt hash mismatch for %s: got %s, want %s", agentName, got, entry.AfterPromptHash)
	}

	if PromptHash(entry.BeforePrompt) != strings.TrimSpace(entry.BeforePromptHash) {
		return updated, fmt.Errorf("feedback rollback: recorded before prompt hash mismatch for %s", agentName)
	}

	agent.SystemPrompt = entry.BeforePrompt
	updated[agentName] = agent

	return updated, nil
}

// FormatHistoryEntry formats a stable, human-readable feedback history entry.
func FormatHistoryEntry(entry HistoryEntry) string {
	var b strings.Builder
	writeHistoryMetadata(&b, entry)
	writeHistoryHashes(&b, entry)
	writeHistoryProposal(&b, entry)

	writeRejectedAlternatives(&b, entry.RejectedAlternatives)
	writeHistoryEvidence(&b, entry.Evidence)
	writeEvidenceLinks(&b, entry.LinkedEvidence)
	writeVerificationRecords(&b, entry.Verification)
	writeHistoryRollbackAndDiff(&b, entry)

	return b.String()
}

func writeHistoryMetadata(b *strings.Builder, entry HistoryEntry) {
	fmt.Fprintf(b, "agent: %s\n", strings.TrimSpace(entry.Agent))

	if status := strings.TrimSpace(entry.Status); status != "" {
		fmt.Fprintf(b, "status: %s\n", status)
	}

	if author := strings.TrimSpace(entry.Author); author != "" {
		fmt.Fprintf(b, "author: %s\n", author)
	}

	if source := strings.TrimSpace(entry.Source); source != "" {
		fmt.Fprintf(b, "source: %s\n", source)
	}

	if learningID := strings.TrimSpace(entry.LearningID); learningID != "" {
		fmt.Fprintf(b, "learning_id: %s\n", learningID)
	}

	fmt.Fprintf(b, "confidence: %.2f\n", entry.Confidence)
}

func writeHistoryHashes(b *strings.Builder, entry HistoryEntry) {
	if beforeHash := strings.TrimSpace(entry.BeforePromptHash); beforeHash != "" {
		fmt.Fprintf(b, "before_prompt_hash: %s\n", beforeHash)
	}

	if afterHash := strings.TrimSpace(entry.AfterPromptHash); afterHash != "" {
		fmt.Fprintf(b, "after_prompt_hash: %s\n", afterHash)
	}
}

func writeHistoryProposal(b *strings.Builder, entry HistoryEntry) {
	if rootCause := formatRootCause(entry.RootCause); rootCause != "" {
		fmt.Fprintf(b, "root_cause: %s\n", rootCause)
	}

	if target := strings.TrimSpace(entry.TargetBehavior); target != "" {
		fmt.Fprintf(b, "target_behavior: %s\n", target)
	}

	if action := strings.TrimSpace(entry.Action); action != "" {
		fmt.Fprintf(b, "action: %s\n", action)
	}

	if reason := strings.TrimSpace(entry.Reason); reason != "" {
		fmt.Fprintf(b, "reason: %s\n", reason)
	}
}

func writeHistoryEvidence(b *strings.Builder, evidence []string) {
	evidence = cleanStrings(evidence)
	if len(evidence) == 0 {
		return
	}

	b.WriteString("evidence:\n")

	for _, item := range evidence {
		fmt.Fprintf(b, "  - %s\n", item)
	}
}

func writeHistoryRollbackAndDiff(b *strings.Builder, entry HistoryEntry) {
	if rollback := strings.TrimSpace(entry.RollbackInstructions); rollback != "" {
		fmt.Fprintf(b, "rollback: %s\n", rollback)
	}

	writeDiffBlock(b, "diff", entry.Diff)
	writeDiffBlock(b, "rollback_diff", entry.RollbackDiff)
}

func writeDiffBlock(b *strings.Builder, label, diff string) {
	diff = strings.TrimSpace(diff)
	if diff == "" {
		return
	}

	fmt.Fprintf(b, "%s:\n```diff\n", label)
	b.WriteString(diff)

	if !strings.HasSuffix(diff, "\n") {
		b.WriteByte('\n')
	}

	b.WriteString("```\n")
}

func normalizeApplyOptions(options ApplyOptions) ApplyOptions {
	options.Author = strings.TrimSpace(options.Author)
	if options.Author == "" {
		options.Author = "atteler feedback workflow"
	}

	options.Source = strings.TrimSpace(options.Source)
	if options.Source == "" {
		options.Source = "session feedback"
	}

	return options
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

	if agent.ToolPermissions != nil {
		toolPermissions := agent.ToolPermissions
		agent.ToolPermissions = make(map[string]bool, len(toolPermissions))
		maps.Copy(agent.ToolPermissions, toolPermissions)
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

func normalizeProposalForApply(agentName string, proposal Proposal) Proposal {
	proposal.Agent = strings.TrimSpace(agentName)
	proposal.Action = strings.TrimSpace(proposal.Action)
	proposal.Reason = strings.TrimSpace(proposal.Reason)
	proposal.RootCause = cleanRootCause(proposal.RootCause)
	proposal.TargetBehavior = strings.TrimSpace(proposal.TargetBehavior)
	proposal.RejectedAlternatives = cleanRejectedAlternatives(proposal.RejectedAlternatives)
	proposal.Evidence = cleanStrings(proposal.Evidence)
	proposal.LinkedEvidence = cleanEvidenceLinks(proposal.LinkedEvidence)
	proposal.Verification = cleanVerificationRecords(proposal.Verification)

	if len(proposal.LinkedEvidence) == 0 {
		proposal.LinkedEvidence = inferredEvidenceLinks(proposal)
	}

	if proposal.RootCause.Category == "" {
		proposal.RootCause.Category = "unclassified-feedback"
	}

	if proposal.RootCause.Summary == "" {
		proposal.RootCause.Summary = firstNonEmpty(proposal.Reason, proposal.Action, "Feedback indicated an agent behavior should change.")
	}

	if proposal.TargetBehavior == "" {
		proposal.TargetBehavior = firstNonEmpty(proposal.Action, "Change the agent behavior described by the linked feedback evidence.")
	}

	if len(proposal.RejectedAlternatives) == 0 {
		proposal.RejectedAlternatives = []RejectedAlternative{{
			Alternative: "Apply feedback without an audited patch",
			Reason:      "The learning workflow requires reviewable evidence and rollback metadata.",
		}}
	}

	return proposal
}

func proposalGuidance(proposal Proposal) string {
	lines := []string{
		feedbackGuidanceHeader,
		learningIDLabel + proposalLearningID(proposal),
	}

	if rootCause := formatRootCause(proposal.RootCause); rootCause != "" {
		lines = append(lines, rootCauseLabel+rootCause)
	}

	if target := strings.TrimSpace(proposal.TargetBehavior); target != "" {
		lines = append(lines, targetBehaviorLabel+target)
	}

	if action := strings.TrimSpace(proposal.Action); action != "" {
		lines = append(lines, "- Action: "+action)
	}

	if reason := strings.TrimSpace(proposal.Reason); reason != "" {
		lines = append(lines, "- Reason: "+reason)
	}

	if len(proposal.RejectedAlternatives) > 0 {
		lines = append(lines, "- Rejected alternatives:")

		for _, rejected := range proposal.RejectedAlternatives {
			line := "  - " + strings.TrimSpace(rejected.Alternative)
			if reason := strings.TrimSpace(rejected.Reason); reason != "" {
				line += " | " + reason
			}

			lines = append(lines, line)
		}
	}

	if len(proposal.Evidence) > 0 {
		lines = append(lines, "- Evidence:")
		for _, evidence := range cleanStrings(proposal.Evidence) {
			lines = append(lines, "  - "+evidence)
		}
	}

	if len(lines) == 1 {
		return ""
	}

	return strings.Join(lines, "\n")
}

func appendSystemPromptGuidance(systemPrompt, guidance string) string {
	trimmedPrompt := strings.TrimSpace(systemPrompt)
	if trimmedPrompt == "" {
		return strings.TrimSpace(guidance)
	}

	return trimmedPrompt + "\n\n" + strings.TrimSpace(guidance)
}

func hasPassingProof(records []VerificationRecord) bool {
	for _, record := range records {
		if record.Passed && strings.TrimSpace(record.Phase) == VerificationPhaseAfter && isAcceptedProofKind(record.Kind) {
			return true
		}
	}

	return false
}

func isAcceptedProofKind(kind string) bool {
	switch strings.TrimSpace(kind) {
	case VerificationKindEval, VerificationKindFixture:
		return true
	default:
		return false
	}
}

func hasDuplicateGuidance(systemPrompt string, proposal Proposal) bool {
	learningID := proposalLearningID(proposal)
	if strings.Contains(systemPrompt, learningIDLabel+learningID) {
		return true
	}

	return strings.Contains(systemPrompt, proposalGuidance(proposal))
}

func hasContradictoryGuidance(systemPrompt string, proposal Proposal) bool {
	category := strings.TrimSpace(proposal.RootCause.Category)
	target := normalizeGuidanceValue(proposal.TargetBehavior)

	if category == "" || target == "" {
		return false
	}

	for _, block := range parseGuidanceBlocks(systemPrompt) {
		if block.RootCauseCategory != category {
			continue
		}

		if normalizeGuidanceValue(block.TargetBehavior) != "" && normalizeGuidanceValue(block.TargetBehavior) != target {
			return true
		}
	}

	return false
}

func parseGuidanceBlocks(systemPrompt string) []guidanceBlock {
	parts := strings.Split(systemPrompt, feedbackGuidanceHeader)
	if len(parts) <= 1 {
		return nil
	}

	blocks := make([]guidanceBlock, 0, len(parts)-1)
	for _, part := range parts[1:] {
		block := guidanceBlock{}

		for line := range strings.SplitSeq(part, "\n") {
			line = strings.TrimSpace(line)
			switch {
			case strings.HasPrefix(line, strings.TrimSpace(learningIDLabel)):
				block.LearningID = strings.TrimSpace(strings.TrimPrefix(line, strings.TrimSpace(learningIDLabel)))
			case strings.HasPrefix(line, strings.TrimSpace(rootCauseLabel)):
				rootCause := strings.TrimSpace(strings.TrimPrefix(line, strings.TrimSpace(rootCauseLabel)))
				category, _, _ := strings.Cut(rootCause, " — ")
				block.RootCauseCategory = strings.TrimSpace(category)
			case strings.HasPrefix(line, strings.TrimSpace(targetBehaviorLabel)):
				block.TargetBehavior = strings.TrimSpace(strings.TrimPrefix(line, strings.TrimSpace(targetBehaviorLabel)))
			}
		}

		blocks = append(blocks, block)
	}

	return blocks
}

func proposalLearningID(proposal Proposal) string {
	parts := []string{
		strings.ToLower(strings.TrimSpace(proposal.Agent)),
		strings.ToLower(strings.TrimSpace(proposal.RootCause.Category)),
		strings.ToLower(strings.TrimSpace(proposal.TargetBehavior)),
		strings.ToLower(strings.TrimSpace(proposal.Action)),
	}
	sum := sha256.Sum256([]byte(strings.Join(parts, "\n")))

	return hex.EncodeToString(sum[:8])
}

func evidenceReference(kind, description string) string {
	kind = strings.TrimSpace(kind)
	if kind == "" {
		kind = "evidence"
	}

	sum := sha256.Sum256([]byte(kind + "\n" + strings.TrimSpace(description)))

	return kind + ":" + hex.EncodeToString(sum[:8])
}

func inferredEvidenceLinks(proposal Proposal) []EvidenceLink {
	links := make([]EvidenceLink, 0, len(proposal.Evidence)+len(proposal.Verification))
	for _, evidence := range proposal.Evidence {
		kind := evidenceKind(evidence)
		links = append(links, EvidenceLink{
			Kind:        kind,
			Reference:   evidenceReference(kind, evidence),
			Description: evidence,
		})
	}

	for _, record := range proposal.Verification {
		description := verificationDescription(record)
		if description == "" {
			continue
		}

		reference := strings.TrimSpace(record.Reference)
		if reference == "" {
			reference = evidenceReference(record.Kind, description)
		}

		links = append(links, EvidenceLink{
			Kind:        strings.TrimSpace(record.Kind),
			Reference:   reference,
			Description: description,
		})
	}

	return cleanEvidenceLinks(links)
}

func evidenceKind(evidence string) string {
	switch {
	case strings.HasPrefix(evidence, "negative knowledge:"):
		return "negative-knowledge"
	case strings.HasPrefix(evidence, "evaluation:"):
		return VerificationKindEval
	default:
		return "feedback-evidence"
	}
}

func verificationDescription(record VerificationRecord) string {
	parts := cleanStrings([]string{
		record.Phase,
		record.Kind,
		record.Outcome,
		record.Name,
		record.Notes,
	})
	if record.Score != 0 {
		parts = append(parts, fmt.Sprintf("score %d", record.Score))
	}

	return strings.Join(parts, "; ")
}

func promptDiff(agentName, before, after string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "--- a/agents/%s/system_prompt\n", agentName)
	fmt.Fprintf(&b, "+++ b/agents/%s/system_prompt\n", agentName)
	b.WriteString("@@\n")

	beforeLines := splitPromptLines(before)
	afterLines := splitPromptLines(after)
	prefix := commonLinePrefix(beforeLines, afterLines)
	suffix := commonLineSuffix(beforeLines[prefix:], afterLines[prefix:])

	for _, line := range beforeLines[prefix : len(beforeLines)-suffix] {
		fmt.Fprintf(&b, "-%s\n", line)
	}

	for _, line := range afterLines[prefix : len(afterLines)-suffix] {
		fmt.Fprintf(&b, "+%s\n", line)
	}

	return b.String()
}

func splitPromptLines(prompt string) []string {
	if prompt == "" {
		return nil
	}

	return strings.Split(prompt, "\n")
}

func commonLinePrefix(left, right []string) int {
	limit := min(len(left), len(right))
	for i := range limit {
		if left[i] != right[i] {
			return i
		}
	}

	return limit
}

func commonLineSuffix(left, right []string) int {
	limit := min(len(left), len(right))
	for i := range limit {
		if left[len(left)-1-i] != right[len(right)-1-i] {
			return i
		}
	}

	return limit
}

func rollbackInstructions(agentName, beforeHash, afterHash string) string {
	return fmt.Sprintf(
		"verify %s system prompt hash is %s, then apply rollback_diff to restore the prompt whose hash was %s or call feedback.RollbackHistoryEntry with this history entry",
		agentName,
		afterHash,
		beforeHash,
	)
}

func formatRootCause(rootCause RootCauseClassification) string {
	rootCause = cleanRootCause(rootCause)
	if rootCause.Category == "" && rootCause.Summary == "" {
		return ""
	}

	if rootCause.Summary == "" {
		return rootCause.Category
	}

	if rootCause.Category == "" {
		return rootCause.Summary
	}

	if len(rootCause.Signals) == 0 {
		return rootCause.Category + " — " + rootCause.Summary
	}

	return fmt.Sprintf("%s — %s (signals: %s)", rootCause.Category, rootCause.Summary, strings.Join(rootCause.Signals, ", "))
}

func writeRejectedAlternatives(b *strings.Builder, rejected []RejectedAlternative) {
	rejected = cleanRejectedAlternatives(rejected)
	if len(rejected) == 0 {
		return
	}

	b.WriteString("rejected_alternatives:\n")

	for _, item := range rejected {
		fmt.Fprintf(b, "  - alternative: %s\n", item.Alternative)

		if item.Reason != "" {
			fmt.Fprintf(b, "    reason: %s\n", item.Reason)
		}
	}
}

func writeEvidenceLinks(b *strings.Builder, links []EvidenceLink) {
	links = cleanEvidenceLinks(links)
	if len(links) == 0 {
		return
	}

	b.WriteString("linked_evidence:\n")

	for _, link := range links {
		parts := []string{"kind=" + link.Kind, "ref=" + link.Reference}
		if link.Description != "" {
			parts = append(parts, "description="+link.Description)
		}

		fmt.Fprintf(b, "  - %s\n", strings.Join(parts, "\t"))
	}
}

func writeVerificationRecords(b *strings.Builder, records []VerificationRecord) {
	records = cleanVerificationRecords(records)
	if len(records) == 0 {
		return
	}

	b.WriteString("verification:\n")

	for _, record := range records {
		parts := []string{
			"phase=" + record.Phase,
			"kind=" + record.Kind,
			fmt.Sprintf("passed=%t", record.Passed),
		}

		if record.Name != "" {
			parts = append(parts, "name="+record.Name)
		}

		if record.Outcome != "" {
			parts = append(parts, "outcome="+record.Outcome)
		}

		if record.Score != 0 {
			parts = append(parts, fmt.Sprintf("score=%d", record.Score))
		}

		if record.Reference != "" {
			parts = append(parts, "ref="+record.Reference)
		}

		if record.Notes != "" {
			parts = append(parts, "notes="+record.Notes)
		}

		fmt.Fprintf(b, "  - %s\n", strings.Join(parts, "\t"))
	}
}

func cleanRootCause(rootCause RootCauseClassification) RootCauseClassification {
	rootCause.Category = strings.TrimSpace(rootCause.Category)
	rootCause.Summary = strings.TrimSpace(rootCause.Summary)
	rootCause.Signals = cleanStrings(rootCause.Signals)
	sort.Strings(rootCause.Signals)

	return rootCause
}

func cleanRejectedAlternatives(values []RejectedAlternative) []RejectedAlternative {
	cleaned := make([]RejectedAlternative, 0, len(values))
	for _, value := range values {
		alternative := strings.TrimSpace(value.Alternative)

		reason := strings.TrimSpace(value.Reason)
		if alternative == "" && reason == "" {
			continue
		}

		cleaned = append(cleaned, RejectedAlternative{Alternative: alternative, Reason: reason})
	}

	return cleaned
}

func cleanEvidenceLinks(values []EvidenceLink) []EvidenceLink {
	cleaned := make([]EvidenceLink, 0, len(values))
	for _, value := range values {
		kind := strings.TrimSpace(value.Kind)
		reference := strings.TrimSpace(value.Reference)
		description := strings.TrimSpace(value.Description)

		if kind == "" || reference == "" {
			continue
		}

		cleaned = append(cleaned, EvidenceLink{Kind: kind, Reference: reference, Description: description})
	}

	return cleaned
}

func cleanVerificationRecords(values []VerificationRecord) []VerificationRecord {
	cleaned := make([]VerificationRecord, 0, len(values))
	for _, value := range values {
		record := VerificationRecord{
			Kind:      strings.TrimSpace(value.Kind),
			Phase:     strings.TrimSpace(value.Phase),
			Name:      strings.TrimSpace(value.Name),
			Outcome:   strings.TrimSpace(value.Outcome),
			Reference: strings.TrimSpace(value.Reference),
			Notes:     strings.TrimSpace(value.Notes),
			Score:     value.Score,
			Passed:    value.Passed,
		}
		if record.Kind == "" || record.Phase == "" {
			continue
		}

		cleaned = append(cleaned, record)
	}

	return cleaned
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

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if trimmed := strings.TrimSpace(value); trimmed != "" {
			return trimmed
		}
	}

	return ""
}

func normalizeGuidanceValue(value string) string {
	return strings.ToLower(strings.Join(strings.Fields(value), " "))
}
