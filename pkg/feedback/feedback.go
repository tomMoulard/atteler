// Package feedback derives audited agent improvement proposals from recorded session feedback.
package feedback

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/tommoulard/atteler/pkg/session"
)

func evidenceReference(kind, description string) string {
	kind = strings.TrimSpace(kind)
	if kind == "" {
		kind = "evidence"
	}

	sum := sha256.Sum256([]byte(kind + "\n" + strings.TrimSpace(description)))

	return kind + ":" + hex.EncodeToString(sum[:8])
}

const unknownAgent = "unknown"

// RootCauseClassification explains why an agent prompt change is being proposed.
type RootCauseClassification struct {
	Category string
	Summary  string
	Signals  []string
}

// RejectedAlternative records an approach that was considered and explicitly
// ruled out before changing an agent prompt.
type RejectedAlternative struct {
	Alternative string
	Reason      string
}

// EvidenceLink points to source material that supports a feedback proposal.
type EvidenceLink struct {
	Kind        string
	Reference   string
	Description string
}

// VerificationRecord records an eval or fixture used to prove a feedback patch.
type VerificationRecord struct {
	Kind      string
	Phase     string
	Name      string
	Outcome   string
	Reference string
	Notes     string
	Score     int
	Passed    bool
}

const (
	// VerificationKindEval records a session.AgentEvaluation proof.
	VerificationKindEval = "eval"
	// VerificationKindFixture records a passing fixture artifact proof.
	VerificationKindFixture = "fixture"

	// VerificationPhaseBefore records baseline failure evidence.
	VerificationPhaseBefore = "before"
	// VerificationPhaseAfter records post-change or acceptance proof evidence.
	VerificationPhaseAfter = "after"
)

// Proposal describes a focused agent improvement suggested by session feedback.
type Proposal struct {
	ExpiresAt            *time.Time
	ID                   string
	Agent                string
	Action               string
	Reason               string
	SourceRun            string
	Reviewer             string
	RootCause            RootCauseClassification
	TargetBehavior       string
	RejectedAlternatives []RejectedAlternative
	Evidence             []string
	LinkedEvidence       []EvidenceLink
	Verification         []VerificationRecord
	Confidence           float64
}

type proposalGroup struct {
	agent              string
	negative           []session.NegativeKnowledge
	failedEvaluations  []session.AgentEvaluation
	passingEvaluations []session.AgentEvaluation
}

// FromSession derives agent improvement proposals from a saved session.
func FromSession(saved session.Session) []Proposal {
	proposals := Proposals(saved.Evaluations, saved.NegativeKnowledge)
	attachFixtureProofs(proposals, saved.Artifacts, latestSignalTimes(saved.Evaluations, saved.NegativeKnowledge))

	for i := range proposals {
		proposals[i].SourceRun = strings.TrimSpace(saved.ID)
	}

	return proposals
}

// Proposals derives agent improvement proposals from evaluation and negative-knowledge records.
//
// Proposals are grouped by agent, ordered by strongest evidence first, and omit agents
// that only have positive or empty feedback.
func Proposals(evaluations []session.AgentEvaluation, negativeKnowledge []session.NegativeKnowledge) []Proposal {
	groups := make(map[string]*proposalGroup)
	passingEvaluations := make(map[string][]session.AgentEvaluation)

	for _, entry := range negativeKnowledge {
		approach := strings.TrimSpace(entry.Approach)

		reason := strings.TrimSpace(entry.Reason)
		if approach == "" && reason == "" {
			continue
		}

		group := groupFor(groups, entry.Agent)
		group.negative = append(group.negative, entry)
	}

	for i := range evaluations {
		entry := evaluations[i]
		if !isImprovementSignal(entry) {
			if isPassingEvaluation(entry) {
				key := agentKey(entry.Agent)
				if key != "" {
					passingEvaluations[key] = append(passingEvaluations[key], entry)
				}
			}

			continue
		}

		group := groupFor(groups, entry.Agent)
		group.failedEvaluations = append(group.failedEvaluations, entry)
	}

	proposals := make([]Proposal, 0, len(groups))
	for _, group := range groups {
		group.passingEvaluations = afterPassingEvaluations(passingEvaluations[agentKey(group.agent)], group.latestSignalTime())

		evidence := proposalEvidence(group)

		if len(evidence) == 0 {
			continue
		}

		proposals = append(proposals, Proposal{
			Agent:                group.agent,
			Action:               proposalAction(group),
			Reason:               proposalReason(group),
			RootCause:            proposalRootCause(group),
			TargetBehavior:       proposalTargetBehavior(group),
			RejectedAlternatives: proposalRejectedAlternatives(group),
			Evidence:             evidence,
			LinkedEvidence:       proposalEvidenceLinks(group),
			Verification:         proposalVerification(group),
			Confidence:           proposalConfidence(group),
		})
	}

	sort.SliceStable(proposals, func(i, j int) bool {
		leftPriority := proposalPriority(proposals[i])

		rightPriority := proposalPriority(proposals[j])
		if leftPriority != rightPriority {
			return leftPriority > rightPriority
		}

		return proposals[i].Agent < proposals[j].Agent
	})

	return proposals
}

func groupFor(groups map[string]*proposalGroup, agent string) *proposalGroup {
	displayAgent := strings.TrimSpace(agent)
	if displayAgent == "" {
		displayAgent = unknownAgent
	}

	key := agentKey(displayAgent)

	group, ok := groups[key]
	if !ok {
		group = &proposalGroup{agent: displayAgent}
		groups[key] = group
	}

	return group
}

func agentKey(agent string) string {
	return strings.ToLower(strings.TrimSpace(agent))
}

func (group *proposalGroup) latestSignalTime() time.Time {
	var latest time.Time
	for _, entry := range group.negative {
		latest = maxTime(latest, entry.CreatedAt)
	}

	for i := range group.failedEvaluations {
		entry := group.failedEvaluations[i]
		latest = maxTime(latest, entry.CreatedAt)
	}

	return latest
}

func maxTime(left, right time.Time) time.Time {
	if right.After(left) {
		return right
	}

	return left
}

func latestSignalTimes(
	evaluations []session.AgentEvaluation,
	negativeKnowledge []session.NegativeKnowledge,
) map[string]time.Time {
	latest := make(map[string]time.Time)

	for _, entry := range negativeKnowledge {
		approach := strings.TrimSpace(entry.Approach)

		reason := strings.TrimSpace(entry.Reason)
		if approach == "" && reason == "" {
			continue
		}

		key := agentKey(entry.Agent)
		latest[key] = maxTime(latest[key], entry.CreatedAt)
	}

	for i := range evaluations {
		entry := evaluations[i]
		if !isImprovementSignal(entry) {
			continue
		}

		key := agentKey(entry.Agent)
		latest[key] = maxTime(latest[key], entry.CreatedAt)
	}

	return latest
}

func afterPassingEvaluations(evaluations []session.AgentEvaluation, latestSignal time.Time) []session.AgentEvaluation {
	if latestSignal.IsZero() || len(evaluations) == 0 {
		return evaluations
	}

	filtered := make([]session.AgentEvaluation, 0, len(evaluations))
	for i := range evaluations {
		evaluation := evaluations[i]
		if evaluation.CreatedAt.IsZero() || !evaluation.CreatedAt.Before(latestSignal) {
			filtered = append(filtered, evaluation)
		}
	}

	return filtered
}

func isImprovementSignal(entry session.AgentEvaluation) bool {
	outcome := strings.TrimSpace(entry.Outcome)
	if outcome == "" {
		return false
	}

	if entry.Score > 0 && entry.Score <= 2 {
		return true
	}

	normalized := strings.ToLower(outcome)

	failureTerms := []string{"fail", "error", "reject", "regress", "block", "timeout", "flaky", "incomplete"}
	for _, term := range failureTerms {
		if strings.Contains(normalized, term) {
			return true
		}
	}

	positiveOutcomes := map[string]bool{
		"pass":      true,
		"passed":    true,
		"success":   true,
		"succeeded": true,
		"ok":        true,
		"accepted":  true,
		"approved":  true,
	}

	return !positiveOutcomes[normalized] && strings.TrimSpace(entry.Notes) != ""
}

func isPassingEvaluation(entry session.AgentEvaluation) bool {
	outcome := strings.TrimSpace(entry.Outcome)
	if outcome == "" || isImprovementSignal(entry) {
		return false
	}

	normalized := strings.ToLower(outcome)

	positiveOutcomes := map[string]bool{
		"pass":      true,
		"passed":    true,
		"success":   true,
		"succeeded": true,
		"ok":        true,
		"accepted":  true,
		"approved":  true,
	}
	if positiveOutcomes[normalized] {
		return true
	}

	return entry.Score > 2
}

func negativeKnowledgeEvidence(entry session.NegativeKnowledge) string {
	approach := strings.TrimSpace(entry.Approach)

	reason := strings.TrimSpace(entry.Reason)
	switch {
	case approach != "" && reason != "":
		return fmt.Sprintf("negative knowledge: %s -> %s", approach, reason)
	case approach != "":
		return "negative knowledge: " + approach
	default:
		return "negative knowledge: " + reason
	}
}

func evaluationEvidence(entry session.AgentEvaluation) string {
	parts := []string{"evaluation: " + strings.TrimSpace(entry.Outcome)}
	if entry.Score != 0 {
		parts = append(parts, fmt.Sprintf("score %d", entry.Score))
	}

	if notes := strings.TrimSpace(entry.Notes); notes != "" {
		parts = append(parts, notes)
	}

	if reference := strings.TrimSpace(entry.Reference); reference != "" {
		parts = append(parts, "ref "+reference)
	}

	return strings.Join(parts, "; ")
}

func proposalEvidence(group *proposalGroup) []string {
	evidence := make([]string, 0, len(group.negative)+len(group.failedEvaluations))
	for _, entry := range group.negative {
		evidence = append(evidence, negativeKnowledgeEvidence(entry))
	}

	for i := range group.failedEvaluations {
		entry := group.failedEvaluations[i]
		evidence = append(evidence, evaluationEvidence(entry))
	}

	return evidence
}

func proposalEvidenceLinks(group *proposalGroup) []EvidenceLink {
	links := make([]EvidenceLink, 0, len(group.negative)+len(group.failedEvaluations)+len(group.passingEvaluations))
	for _, entry := range group.negative {
		description := negativeKnowledgeEvidence(entry)
		if commit := strings.TrimSpace(entry.Commit); commit != "" {
			links = append(links, EvidenceLink{
				Kind:        "commit",
				Reference:   commit,
				Description: description,
			})

			continue
		}

		links = append(links, EvidenceLink{
			Kind:        "negative-knowledge",
			Reference:   evidenceReference("negative-knowledge", description),
			Description: description,
		})
	}

	for i := range group.failedEvaluations {
		entry := group.failedEvaluations[i]
		description := evaluationEvidence(entry)

		reference := strings.TrimSpace(entry.Reference)
		if reference == "" {
			reference = evidenceReference(VerificationKindEval, description)
		}

		links = append(links, EvidenceLink{
			Kind:        VerificationKindEval,
			Reference:   reference,
			Description: description,
		})
	}

	for i := range group.passingEvaluations {
		entry := group.passingEvaluations[i]
		description := evaluationEvidence(entry)

		reference := strings.TrimSpace(entry.Reference)
		if reference == "" {
			reference = evidenceReference(VerificationKindEval, description)
		}

		links = append(links, EvidenceLink{
			Kind:        VerificationKindEval,
			Reference:   reference,
			Description: description,
		})
	}

	return links
}

func proposalVerification(group *proposalGroup) []VerificationRecord {
	records := make([]VerificationRecord, 0, len(group.failedEvaluations)+len(group.passingEvaluations))
	for i := range group.failedEvaluations {
		entry := group.failedEvaluations[i]
		records = append(records, evaluationVerificationRecord(entry, VerificationPhaseBefore, false))
	}

	for i := range group.passingEvaluations {
		entry := group.passingEvaluations[i]
		records = append(records, evaluationVerificationRecord(entry, VerificationPhaseAfter, true))
	}

	return records
}

func evaluationVerificationRecord(entry session.AgentEvaluation, phase string, passed bool) VerificationRecord {
	return VerificationRecord{
		Kind:      VerificationKindEval,
		Phase:     phase,
		Name:      strings.TrimSpace(entry.Reference),
		Outcome:   strings.TrimSpace(entry.Outcome),
		Reference: strings.TrimSpace(entry.Reference),
		Notes:     strings.TrimSpace(entry.Notes),
		Score:     entry.Score,
		Passed:    passed,
	}
}

func proposalAction(group *proposalGroup) string {
	if len(group.negative) > 0 {
		return "Revise the agent instructions to avoid the recorded failed approach, require an alternative, and run a focused regression check before acceptance."
	}

	return "Revise the agent instructions to address the failed evaluation pattern and require evidence before claiming success."
}

func proposalReason(group *proposalGroup) string {
	switch {
	case len(group.negative) > 0 && len(group.failedEvaluations) > 0:
		return "Recorded negative knowledge and failed evaluations indicate recurring improvement opportunities."
	case len(group.negative) > 0:
		return "Recorded negative knowledge shows approaches this agent should avoid repeating."
	default:
		return "Failed or low-scoring evaluations show behavior this agent should improve."
	}
}

func proposalRootCause(group *proposalGroup) RootCauseClassification {
	signals := make([]string, 0, 2)
	if len(group.negative) > 0 {
		signals = append(signals, "negative-knowledge")
	}

	if len(group.failedEvaluations) > 0 {
		signals = append(signals, "failed-evaluation")
	}

	hasNegative := len(group.negative) > 0
	hasFailedEvaluation := len(group.failedEvaluations) > 0

	category := "evaluation-regression"
	if hasNegative && hasFailedEvaluation {
		category = "repeated-failed-approach-and-evaluation-regression"
	} else if hasNegative {
		category = "repeated-failed-approach"
	}

	return RootCauseClassification{
		Category: category,
		Summary:  proposalReason(group),
		Signals:  signals,
	}
}

func proposalTargetBehavior(group *proposalGroup) string {
	if len(group.negative) > 0 {
		entry := group.negative[0]

		return fmt.Sprintf(
			"Avoid %q when it previously led to %q; choose a different approach and verify it with an eval or fixture before accepting the change.",
			strings.TrimSpace(entry.Approach),
			strings.TrimSpace(entry.Reason),
		)
	}

	if len(group.failedEvaluations) > 0 {
		entry := group.failedEvaluations[0]

		failure := strings.TrimSpace(entry.Notes)

		if failure == "" {
			failure = strings.TrimSpace(entry.Outcome)
		}

		return fmt.Sprintf("Address %q with an explicit regression check before reporting success.", failure)
	}

	return ""
}

func proposalRejectedAlternatives(group *proposalGroup) []RejectedAlternative {
	rejected := []RejectedAlternative{
		{
			Alternative: "Append generic feedback-derived guidance without a patch review",
			Reason:      "Generic prompt barnacles are not auditable and can become stale or contradictory.",
		},
		{
			Alternative: "Accept the prompt change without a passing eval or fixture",
			Reason:      "The workflow must prove at least one regression check before marking feedback accepted.",
		},
	}

	if len(group.negative) > 0 {
		entry := group.negative[0]
		rejected = append(rejected, RejectedAlternative{
			Alternative: "Repeat recorded failed approach: " + strings.TrimSpace(entry.Approach),
			Reason:      strings.TrimSpace(entry.Reason),
		})
	}

	return rejected
}

func proposalConfidence(group *proposalGroup) float64 {
	signals := len(group.negative)*2 + len(group.failedEvaluations)
	switch {
	case signals >= 4:
		return 0.9
	case signals >= 2:
		return 0.8
	default:
		return 0.65
	}
}

func proposalPriority(proposal Proposal) int {
	priority := 0

	for _, evidence := range proposal.Evidence {
		if strings.HasPrefix(evidence, "negative knowledge:") {
			priority += 2
			continue
		}

		priority++
	}

	return priority
}

func attachFixtureProofs(proposals []Proposal, artifacts []session.Artifact, latestSignals map[string]time.Time) {
	if len(proposals) == 0 || len(artifacts) == 0 {
		return
	}

	for i := range artifacts {
		artifact := &artifacts[i]
		if !isPassingFixture(artifact) {
			continue
		}

		key := agentKey(artifact.SourceAgent)
		if key == "" {
			continue
		}

		if !artifactIsAfterSignal(artifact, latestSignals[key]) {
			continue
		}

		record := fixtureVerificationRecord(artifact)
		link := EvidenceLink{
			Kind:        VerificationKindFixture,
			Reference:   record.Reference,
			Description: strings.TrimSpace(artifact.Summary),
		}

		for i := range proposals {
			if agentKey(proposals[i].Agent) != key {
				continue
			}

			proposals[i].Verification = append(proposals[i].Verification, record)
			if link.Reference != "" {
				proposals[i].LinkedEvidence = append(proposals[i].LinkedEvidence, link)
			}
		}
	}
}

func artifactIsAfterSignal(artifact *session.Artifact, latestSignal time.Time) bool {
	return latestSignal.IsZero() || artifact.CreatedAt.IsZero() || !artifact.CreatedAt.Before(latestSignal)
}

func isPassingFixture(artifact *session.Artifact) bool {
	kind := strings.ToLower(strings.TrimSpace(artifact.Kind))
	if kind == "" {
		return false
	}

	switch kind {
	case "passing-fixture", "fixture-pass", "eval-fixture-pass":
		return true
	}

	if !strings.Contains(kind, "fixture") {
		return false
	}

	return positiveProofText(artifact.Summary)
}

func positiveProofText(value string) bool {
	normalized := strings.ToLower(strings.Join(strings.Fields(value), " "))
	for _, term := range []string{"pass", "passed", "success", "succeeded", "accepted", "approved"} {
		if strings.Contains(normalized, term) {
			return true
		}
	}

	return false
}

func fixtureVerificationRecord(artifact *session.Artifact) VerificationRecord {
	return VerificationRecord{
		Kind:      VerificationKindFixture,
		Phase:     VerificationPhaseAfter,
		Name:      strings.TrimSpace(artifact.Path),
		Reference: strings.TrimSpace(artifact.Path),
		Notes:     strings.TrimSpace(artifact.Summary),
		Passed:    true,
	}
}
