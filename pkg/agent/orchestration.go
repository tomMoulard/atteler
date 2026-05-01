package agent

import (
	"fmt"
	"strings"
)

const (
	// ParticipantSourceRequested identifies an explicitly requested agent.
	ParticipantSourceRequested = "requested"
	// ParticipantSourceTrigger identifies an agent selected by a trigger phrase.
	ParticipantSourceTrigger = "trigger"
	// ParticipantSourceCapability identifies an agent selected by a capability phrase.
	ParticipantSourceCapability = "capability"
)

// OrchestrationRequest describes the inputs used to select agents for a task.
type OrchestrationRequest struct {
	Prompt          string
	RequestedNames  []string
	MaxParticipants int
}

// OrchestrationPlan is a stable, ordered set of agents selected for a task.
type OrchestrationPlan struct {
	Participants []Participant
}

// Agents returns the selected agents without match metadata.
func (p OrchestrationPlan) Agents() []Agent {
	agents := make([]Agent, 0, len(p.Participants))
	for i := range p.Participants {
		agents = append(agents, p.Participants[i].Agent)
	}
	return agents
}

// Participant describes a selected agent and why it was selected.
//
//nolint:govet // Field order keeps the selected agent before reason metadata.
type Participant struct {
	Agent   Agent
	Source  string
	Pattern string
}

// UnknownAgentsError reports explicitly requested agents that are not registered.
type UnknownAgentsError struct {
	Names []string
	Known []string
}

// Error returns a useful unknown-agent diagnostic with known alternatives.
func (e UnknownAgentsError) Error() string {
	if len(e.Known) == 0 {
		return fmt.Sprintf("unknown agent(s): %s; no agents are registered", strings.Join(e.Names, ", "))
	}
	return fmt.Sprintf("unknown agent(s): %s; known agents: %s", strings.Join(e.Names, ", "), strings.Join(e.Known, ", "))
}

// PlanAgents is a convenience wrapper around PlanOrchestration.
func (r *Registry) PlanAgents(prompt string, requestedNames []string, maxParticipants int) (OrchestrationPlan, error) {
	return r.PlanOrchestration(OrchestrationRequest{
		Prompt:          prompt,
		RequestedNames:  requestedNames,
		MaxParticipants: maxParticipants,
	})
}

// PlanOrchestration selects a stable ordered set of agents for a task.
//
// Explicit requested names are selected first in request order, followed by
// trigger matches and then capability matches in sorted registry-name order.
// Duplicate agents are included once, and MaxParticipants caps the final set
// when greater than zero.
func (r *Registry) PlanOrchestration(request OrchestrationRequest) (OrchestrationPlan, error) {
	if r == nil {
		return OrchestrationPlan{}, nil
	}

	known := r.List()
	unknown := unknownRequestedNames(r, request.RequestedNames)
	if len(unknown) > 0 {
		return OrchestrationPlan{}, UnknownAgentsError{Names: unknown, Known: known}
	}

	planner := orchestrationPlanner{
		prompt:          strings.ToLower(request.Prompt),
		selected:        make(map[string]bool),
		registry:        r,
		maxParticipants: request.MaxParticipants,
	}

	if !planner.addRequested(request.RequestedNames) {
		return planner.plan, nil
	}
	if !planner.addPhraseMatches(known, ParticipantSourceTrigger) {
		return planner.plan, nil
	}
	planner.addPhraseMatches(known, ParticipantSourceCapability)

	return planner.plan, nil
}

type orchestrationPlanner struct {
	prompt          string
	selected        map[string]bool
	registry        *Registry
	plan            OrchestrationPlan
	maxParticipants int
}

func (p *orchestrationPlanner) addRequested(names []string) bool {
	for _, name := range names {
		name = strings.TrimSpace(name)
		if name == "" {
			continue
		}
		agent, _ := p.registry.Get(name)
		if !p.add(agent, ParticipantSourceRequested, "") {
			return false
		}
	}
	return true
}

func (p *orchestrationPlanner) addPhraseMatches(names []string, source string) bool {
	for _, name := range names {
		agent := p.registry.agents[name]
		if pattern, ok := p.firstMatch(agent, source); ok && !p.add(agent, source, pattern) {
			return false
		}
	}
	return true
}

func (p *orchestrationPlanner) firstMatch(agent Agent, source string) (string, bool) {
	patterns := agent.Capabilities
	if source == ParticipantSourceTrigger {
		patterns = agent.Triggers
	}
	for _, pattern := range patterns {
		if p.matches(pattern) {
			return pattern, true
		}
	}
	return "", false
}

func (p *orchestrationPlanner) add(agent Agent, source, pattern string) bool {
	if p.selected[agent.Name] {
		return true
	}
	if p.maxParticipants > 0 && len(p.plan.Participants) >= p.maxParticipants {
		return false
	}
	p.plan.Participants = append(p.plan.Participants, Participant{
		Agent:   agent,
		Source:  source,
		Pattern: pattern,
	})
	p.selected[agent.Name] = true
	return true
}

func (p *orchestrationPlanner) matches(pattern string) bool {
	return pattern != "" && strings.Contains(p.prompt, pattern)
}

func unknownRequestedNames(r *Registry, names []string) []string {
	seen := make(map[string]bool, len(names))
	unknown := make([]string, 0)
	for _, name := range names {
		name = strings.TrimSpace(name)
		if name == "" || seen[name] {
			continue
		}
		seen[name] = true
		if _, ok := r.Get(name); !ok {
			unknown = append(unknown, name)
		}
	}
	return unknown
}
