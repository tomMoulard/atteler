// Package feedback derives agent improvement proposals from recorded session feedback.
package feedback

import (
	"fmt"
	"sort"
	"strings"

	"github.com/tommoulard/atteler/pkg/session"
)

const unknownAgent = "unknown"

// Proposal describes a focused agent improvement suggested by session feedback.
type Proposal struct {
	Agent      string
	Action     string
	Reason     string
	Evidence   []string
	Confidence float64
}

type proposalGroup struct {
	agent              string
	negativeEvidence   []string
	evaluationEvidence []string
}

// FromSession derives agent improvement proposals from a saved session.
func FromSession(saved session.Session) []Proposal {
	return Proposals(saved.Evaluations, saved.NegativeKnowledge)
}

// Proposals derives agent improvement proposals from evaluation and negative-knowledge records.
//
// Proposals are grouped by agent, ordered by strongest evidence first, and omit agents
// that only have positive or empty feedback.
func Proposals(evaluations []session.AgentEvaluation, negativeKnowledge []session.NegativeKnowledge) []Proposal {
	groups := make(map[string]*proposalGroup)

	for _, entry := range negativeKnowledge {
		approach := strings.TrimSpace(entry.Approach)
		reason := strings.TrimSpace(entry.Reason)
		if approach == "" && reason == "" {
			continue
		}
		group := groupFor(groups, entry.Agent)
		group.negativeEvidence = append(group.negativeEvidence, negativeKnowledgeEvidence(entry))
	}

	for _, entry := range evaluations {
		if !isImprovementSignal(entry) {
			continue
		}
		group := groupFor(groups, entry.Agent)
		group.evaluationEvidence = append(group.evaluationEvidence, evaluationEvidence(entry))
	}

	proposals := make([]Proposal, 0, len(groups))
	for _, group := range groups {
		evidence := make([]string, 0, len(group.negativeEvidence)+len(group.evaluationEvidence))
		evidence = append(evidence, group.negativeEvidence...)
		evidence = append(evidence, group.evaluationEvidence...)
		if len(evidence) == 0 {
			continue
		}
		proposals = append(proposals, Proposal{
			Agent:      group.agent,
			Action:     proposalAction(group),
			Reason:     proposalReason(group),
			Evidence:   evidence,
			Confidence: proposalConfidence(group),
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
	key := strings.ToLower(displayAgent)
	group, ok := groups[key]
	if !ok {
		group = &proposalGroup{agent: displayAgent}
		groups[key] = group
	}
	return group
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

func proposalAction(group *proposalGroup) string {
	if len(group.negativeEvidence) > 0 {
		return "Revise the agent instructions to avoid repeated failed approaches and add explicit fallback guidance."
	}
	return "Revise the agent instructions to address failed evaluation patterns."
}

func proposalReason(group *proposalGroup) string {
	switch {
	case len(group.negativeEvidence) > 0 && len(group.evaluationEvidence) > 0:
		return "Recorded negative knowledge and failed evaluations indicate recurring improvement opportunities."
	case len(group.negativeEvidence) > 0:
		return "Recorded negative knowledge shows approaches this agent should avoid repeating."
	default:
		return "Failed or low-scoring evaluations show behavior this agent should improve."
	}
}

func proposalConfidence(group *proposalGroup) float64 {
	signals := len(group.negativeEvidence)*2 + len(group.evaluationEvidence)
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
