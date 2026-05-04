// Package speculate provides dependency-free planning primitives for
// speculative parallel execution workflows.
package speculate

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"
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

// BranchPrompt identifies the full prompt for one speculative branch.
//
// Branch names are normalized by EstimatePromptCacheReuse, and output metadata
// is sorted by branch name for deterministic callers and tests.
type BranchPrompt struct {
	Branch string
	Prompt string
}

// PromptBranchCacheMetadata summarizes cacheable prompt-prefix reuse for one
// speculative branch. Byte counts are used as a dependency-free proxy when no
// provider tokenizer is available.
type PromptBranchCacheMetadata struct {
	Branch            string
	PromptBytes       int
	SharedPrefixBytes int
	ReuseRatio        float64
}

// PromptCacheReuseEstimate summarizes prompt-prefix reuse across speculative
// branches. ReusablePromptBytes counts the shared prefix once per branch.
//
//nolint:govet // Public field order keeps aggregate summary before per-branch details.
type PromptCacheReuseEstimate struct {
	SharedPrefix        string
	SharedPrefixBytes   int
	TotalPromptBytes    int
	ReusablePromptBytes int
	ReuseRatio          float64
	Branches            []PromptBranchCacheMetadata
}

// Session captures the data produced by a speculative three-round run.
type Session struct {
	Plan      Plan
	Proposals []Proposal
	Reviews   []Review
	Verdict   Verdict
}

// Result summarizes a completed speculative session.
type Result struct {
	Winner  string
	Reason  string
	Session Session
}

// ProposalRunner produces one agent's independent round 1 proposal.
type ProposalRunner func(ctx context.Context, agent string) (string, error)

// ReviewRunner produces one round 2 cross-review for a target proposal.
type ReviewRunner func(ctx context.Context, assignment Review, proposal Proposal) (string, error)

// Aggregator produces the round 3 verdict from the completed proposal and
// review outputs.
type Aggregator func(ctx context.Context, session Session) (Verdict, error)

// Runner contains the caller-supplied functions used to execute each round.
type Runner struct {
	Propose   ProposalRunner
	Review    ReviewRunner
	Aggregate Aggregator
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

// SharedPromptPrefix returns the longest UTF-8-safe string prefix shared by all
// branch prompts. Empty input or any empty prompt has no reusable prefix.
func SharedPromptPrefix(prompts []string) string {
	if len(prompts) == 0 {
		return ""
	}

	prefix := prompts[0]
	for _, prompt := range prompts[1:] {
		prefix = sharedStringPrefix(prefix, prompt)
		if prefix == "" {
			return ""
		}
	}

	return prefix
}

// EstimatePromptCacheReuse estimates prompt-cache prefix reuse across named
// speculative branches. It is byte-based and tokenizer-free: ReuseRatio is the
// shared prefix bytes counted once per branch divided by total prompt bytes.
func EstimatePromptCacheReuse(branches []BranchPrompt) (PromptCacheReuseEstimate, error) {
	normalized, err := normalizeBranchPrompts(branches)
	if err != nil {
		return PromptCacheReuseEstimate{}, err
	}

	slices.SortFunc(normalized, func(left, right BranchPrompt) int {
		return strings.Compare(left.Branch, right.Branch)
	})

	prompts := make([]string, len(normalized))
	for i, branch := range normalized {
		prompts[i] = branch.Prompt
	}

	sharedPrefix := SharedPromptPrefix(prompts)
	sharedPrefixBytes := len(sharedPrefix)
	estimate := PromptCacheReuseEstimate{
		SharedPrefix:      sharedPrefix,
		SharedPrefixBytes: sharedPrefixBytes,
		Branches:          make([]PromptBranchCacheMetadata, len(normalized)),
	}

	for i, branch := range normalized {
		promptBytes := len(branch.Prompt)
		estimate.TotalPromptBytes += promptBytes
		estimate.ReusablePromptBytes += sharedPrefixBytes

		estimate.Branches[i] = PromptBranchCacheMetadata{
			Branch:            branch.Branch,
			PromptBytes:       promptBytes,
			SharedPrefixBytes: sharedPrefixBytes,
			ReuseRatio:        ratio(sharedPrefixBytes, promptBytes),
		}
	}

	estimate.ReuseRatio = ratio(estimate.ReusablePromptBytes, estimate.TotalPromptBytes)

	return estimate, nil
}

// Run executes the fixed speculative workflow: concurrent round 1 proposals,
// concurrent round 2 cross-reviews, then caller-supplied round 3 aggregation.
// It returns partial session data alongside any error encountered.
func Run(ctx context.Context, plan Plan, runner Runner) (Result, error) {
	if ctx == nil {
		return Result{}, errors.New("context is required")
	}

	if err := validateRunnablePlan(plan); err != nil {
		return Result{}, err
	}

	if runner.Propose == nil {
		return Result{}, errors.New("proposal runner is required")
	}

	if runner.Review == nil {
		return Result{}, errors.New("review runner is required")
	}

	if runner.Aggregate == nil {
		return Result{}, errors.New("aggregator is required")
	}

	session := Session{Plan: plan}

	proposals, err := runProposals(ctx, plan.Agents, runner.Propose)

	session.Proposals = proposals
	if err != nil {
		return Result{Session: session}, err
	}

	reviews, err := runReviews(ctx, proposals, runner.Review)

	session.Reviews = reviews
	if err != nil {
		return Result{Session: session}, err
	}

	verdict, err := runner.Aggregate(ctx, session)

	session.Verdict = verdict
	if err != nil {
		return Result{Session: session}, fmt.Errorf("aggregate: %w", err)
	}

	if err := ValidateVerdict(verdict, plan.GateChecks); err != nil {
		return Result{Session: session}, err
	}

	return Result{
		Session: session,
		Winner:  strings.TrimSpace(verdict.Winner),
		Reason:  strings.TrimSpace(verdict.Reason),
	}, nil
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

func runProposals(ctx context.Context, agents []string, propose ProposalRunner) ([]Proposal, error) {
	proposals := make([]Proposal, len(agents))
	errs := make([]error, len(agents))

	var wg sync.WaitGroup
	wg.Add(len(agents))

	for i, agent := range agents {
		go func(i int, agent string) {
			defer wg.Done()

			content, err := propose(ctx, agent)
			if err != nil {
				errs[i] = fmt.Errorf("proposal %q: %w", agent, err)
				return
			}

			proposals[i] = Proposal{Agent: agent, Round: RoundProposal, Content: content}
		}(i, agent)
	}

	wg.Wait()

	if err := firstError(errs); err != nil {
		return proposals, err
	}

	return proposals, nil
}

func runReviews(ctx context.Context, proposals []Proposal, review ReviewRunner) ([]Review, error) {
	assignments, err := CrossReviews(proposals)
	if err != nil {
		return nil, err
	}

	proposalByAgent := make(map[string]Proposal, len(proposals))
	for _, proposal := range proposals {
		proposalByAgent[proposal.Agent] = proposal
	}

	reviews := make([]Review, len(assignments))
	errs := make([]error, len(assignments))

	var wg sync.WaitGroup
	wg.Add(len(assignments))

	for i, assignment := range assignments {
		go func(i int, assignment Review) {
			defer wg.Done()

			targetProposal := proposalByAgent[assignment.TargetAgent]

			notes, err := review(ctx, assignment, targetProposal)
			if err != nil {
				errs[i] = fmt.Errorf("review %q -> %q: %w", assignment.Reviewer, assignment.TargetAgent, err)
				return
			}

			assignment.Notes = notes
			reviews[i] = assignment
		}(i, assignment)
	}

	wg.Wait()

	if err := firstError(errs); err != nil {
		return reviews, err
	}

	return reviews, nil
}

func validateRunnablePlan(plan Plan) error {
	if _, err := normalizeUnique("agent", plan.Agents); err != nil {
		return err
	}

	if len(plan.Agents) == 0 {
		return errors.New("at least one agent is required")
	}

	if _, err := normalizeUnique("gate check", plan.GateChecks); err != nil {
		return err
	}

	wantRounds := WorkflowRounds()
	if !slices.EqualFunc(plan.Rounds, wantRounds, func(got, want Round) bool {
		return got.Number == want.Number
	}) {
		return errors.New("plan must use the fixed three-round workflow")
	}

	return nil
}

func firstError(errs []error) error {
	for _, err := range errs {
		if err != nil {
			return err
		}
	}

	return nil
}

func normalizeBranchPrompts(branches []BranchPrompt) ([]BranchPrompt, error) {
	normalized := make([]BranchPrompt, 0, len(branches))

	seen := make(map[string]struct{}, len(branches))
	for _, branch := range branches {
		name := strings.TrimSpace(branch.Branch)
		if name == "" {
			return nil, errors.New("branch name is required")
		}

		if _, exists := seen[name]; exists {
			return nil, fmt.Errorf("duplicate branch %q", name)
		}

		seen[name] = struct{}{}
		normalized = append(normalized, BranchPrompt{
			Branch: name,
			Prompt: branch.Prompt,
		})
	}

	return normalized, nil
}

func sharedStringPrefix(left, right string) string {
	limit := min(len(left), len(right))
	lastMatch := 0

	for i := range left {
		if i > limit {
			break
		}

		if !strings.HasPrefix(right, left[:i]) {
			break
		}

		lastMatch = i
	}

	if limit == len(left) && strings.HasPrefix(right, left) {
		lastMatch = len(left)
	}

	if limit == len(right) && strings.HasPrefix(left, right) {
		lastMatch = len(right)
	}

	return left[:lastMatch]
}

func ratio(numerator, denominator int) float64 {
	if denominator == 0 {
		return 0
	}

	return float64(numerator) / float64(denominator)
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
