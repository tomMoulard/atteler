// Package speculate provides dependency-free planning primitives for
// speculative parallel execution workflows.
package speculate

import (
	"errors"
	"fmt"
	"slices"
	"strings"
)

const (
	// RoundProposal is the independent proposal phase.
	RoundProposal = 1
	// RoundReview is the cross-review phase.
	RoundReview = 2
	// RoundAggregate is the final aggregation and verdict phase.
	RoundAggregate = 3
)

// Proposal captures one agent's candidate answer for a speculative round.
//
//nolint:govet // Public field order follows semantic readability.
type Proposal struct {
	Agent   string
	Round   int
	Content string
}

// Review captures one reviewer's notes about another agent's proposal.
type Review struct {
	Reviewer    string
	TargetAgent string
	Notes       string
}

// Verdict captures the selected winner and the gate checks that justify it.
type Verdict struct {
	Winner     string
	Reason     string
	GateChecks []GateCheck
}

// GateCheck records a single pass/fail validation gate for a verdict.
//
//nolint:govet // Public field order follows semantic readability.
type GateCheck struct {
	Name   string
	Passed bool
	Notes  string
}

// Round describes one stage in the speculative planning workflow.
//
//nolint:govet // Public field order follows semantic readability.
type Round struct {
	Number  int
	Name    string
	Purpose string
}

// Plan describes the fixed three-round speculative workflow for a set of agents.
type Plan struct {
	Agents     []string
	Rounds     []Round
	GateChecks []string
}

// NewPlan creates a validated three-round speculative execution plan.
func NewPlan(agents, gateChecks []string) (Plan, error) {
	normalizedAgents, err := normalizeUnique("agent", agents)
	if err != nil {
		return Plan{}, err
	}
	if len(normalizedAgents) == 0 {
		return Plan{}, errors.New("at least one agent is required")
	}

	normalizedChecks, err := normalizeUnique("gate check", gateChecks)
	if err != nil {
		return Plan{}, err
	}

	return Plan{
		Agents:     normalizedAgents,
		Rounds:     WorkflowRounds(),
		GateChecks: normalizedChecks,
	}, nil
}

// WorkflowRounds returns the fixed three-round speculative workflow.
func WorkflowRounds() []Round {
	return []Round{
		{
			Number:  RoundProposal,
			Name:    "independent proposals",
			Purpose: "agents independently produce candidate plans or answers",
		},
		{
			Number:  RoundReview,
			Name:    "cross-review",
			Purpose: "agents review proposals from other agents",
		},
		{
			Number:  RoundAggregate,
			Name:    "aggregate",
			Purpose: "review evidence is aggregated into a gated verdict",
		},
	}
}

// CrossReviews maps proposal authors to cross-review assignments.
//
// Each unique proposal author reviews every other author's proposal. Self-review
// assignments are intentionally omitted.
func CrossReviews(proposals []Proposal) ([]Review, error) {
	agents := make([]string, 0, len(proposals))
	for _, proposal := range proposals {
		if proposal.Round != RoundProposal {
			return nil, fmt.Errorf("proposal from %q is in round %d, want round %d", proposal.Agent, proposal.Round, RoundProposal)
		}
		agents = append(agents, proposal.Agent)
	}

	agents, err := normalizeUnique("proposal agent", agents)
	if err != nil {
		return nil, err
	}

	reviews := make([]Review, 0, len(agents)*(len(agents)-1))
	for _, reviewer := range agents {
		for _, target := range agents {
			if reviewer == target {
				continue
			}
			reviews = append(reviews, Review{
				Reviewer:    reviewer,
				TargetAgent: target,
			})
		}
	}

	return reviews, nil
}

// ValidateGateChecks verifies that every configured gate has a matching passed
// verdict check and that no unknown checks are present.
func ValidateGateChecks(required []string, checks []GateCheck) error {
	required, err := normalizeUnique("required gate check", required)
	if err != nil {
		return err
	}

	seen := make(map[string]GateCheck, len(checks))
	for _, check := range checks {
		name := strings.TrimSpace(check.Name)
		if name == "" {
			return errors.New("gate check name is required")
		}
		if _, exists := seen[name]; exists {
			return fmt.Errorf("duplicate gate check %q", name)
		}
		check.Name = name
		seen[name] = check
	}

	for _, check := range checks {
		if !contains(required, check.Name) {
			return fmt.Errorf("unknown gate check %q", check.Name)
		}
	}

	for _, name := range required {
		check, ok := seen[name]
		if !ok {
			return fmt.Errorf("missing gate check %q", name)
		}
		if !check.Passed {
			return fmt.Errorf("gate check %q failed", name)
		}
	}

	return nil
}

// ValidateVerdict validates winner metadata and required gate checks.
func ValidateVerdict(verdict Verdict, requiredGateChecks []string) error {
	if strings.TrimSpace(verdict.Winner) == "" {
		return errors.New("verdict winner is required")
	}
	if strings.TrimSpace(verdict.Reason) == "" {
		return errors.New("verdict reason is required")
	}
	return ValidateGateChecks(requiredGateChecks, verdict.GateChecks)
}

func normalizeUnique(label string, values []string) ([]string, error) {
	normalized := make([]string, 0, len(values))
	seen := make(map[string]struct{}, len(values))
	for _, value := range values {
		value = strings.TrimSpace(value)
		if value == "" {
			return nil, fmt.Errorf("%s name is required", label)
		}
		if _, exists := seen[value]; exists {
			return nil, fmt.Errorf("duplicate %s %q", label, value)
		}
		seen[value] = struct{}{}
		normalized = append(normalized, value)
	}
	return normalized, nil
}

func contains(values []string, target string) bool {
	return slices.Contains(values, target)
}
