package feedback

import (
	"fmt"
	"strings"

	"github.com/tommoulard/atteler/pkg/config"
)

const feedbackGuidanceHeader = "Feedback-derived guidance:"

// HistoryEntry records one feedback proposal applied to an agent configuration.
type HistoryEntry struct {
	Agent      string
	Action     string
	Reason     string
	Evidence   []string
	Confidence float64
}

// ApplyProposals applies feedback proposals to configured agents and returns a
// copied agent map plus stable history entries for newly applied guidance.
//
// Proposals for agents not present in agents are ignored. Reapplying the same
// proposal is idempotent: an existing guidance block is not appended again and
// no duplicate history entry is returned.
func ApplyProposals(agents map[string]config.AgentConfig, proposals []Proposal) (map[string]config.AgentConfig, []HistoryEntry) {
	updated := copyAgents(agents)
	if len(updated) == 0 || len(proposals) == 0 {
		return updated, nil
	}

	entries := make([]HistoryEntry, 0, len(proposals))
	for _, proposal := range proposals {
		agentName, ok := configuredAgentName(updated, proposal.Agent)
		if !ok {
			continue
		}

		guidance := proposalGuidance(proposal)
		if guidance == "" {
			continue
		}

		agent := updated[agentName]
		if strings.Contains(agent.SystemPrompt, guidance) {
			continue
		}

		agent.SystemPrompt = appendSystemPromptGuidance(agent.SystemPrompt, guidance)
		updated[agentName] = agent

		entries = append(entries, HistoryEntry{
			Agent:      agentName,
			Action:     strings.TrimSpace(proposal.Action),
			Reason:     strings.TrimSpace(proposal.Reason),
			Evidence:   cleanStrings(proposal.Evidence),
			Confidence: proposal.Confidence,
		})
	}

	return updated, entries
}

// FormatHistoryEntry formats a stable, human-readable feedback history entry.
func FormatHistoryEntry(entry HistoryEntry) string {
	var b strings.Builder
	fmt.Fprintf(&b, "agent: %s\n", strings.TrimSpace(entry.Agent))
	fmt.Fprintf(&b, "confidence: %.2f\n", entry.Confidence)

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

	return b.String()
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

func proposalGuidance(proposal Proposal) string {
	var lines []string

	lines = append(lines, feedbackGuidanceHeader)
	if action := strings.TrimSpace(proposal.Action); action != "" {
		lines = append(lines, "- Action: "+action)
	}

	if reason := strings.TrimSpace(proposal.Reason); reason != "" {
		lines = append(lines, "- Reason: "+reason)
	}

	for _, evidence := range cleanStrings(proposal.Evidence) {
		lines = append(lines, "- Evidence: "+evidence)
	}

	if len(lines) == 1 {
		return ""
	}

	return strings.Join(lines, "\n")
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
