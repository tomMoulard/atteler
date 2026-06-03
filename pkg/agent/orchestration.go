package agent

import (
	"fmt"
	"math"
	"slices"
	"sort"
	"strings"
	"unicode"
)

const (
	// ParticipantSourceRequested identifies an explicitly requested agent.
	ParticipantSourceRequested = "requested"
	// ParticipantSourceTrigger identifies an agent selected by a trigger phrase.
	ParticipantSourceTrigger = "trigger"
	// ParticipantSourceCapability identifies an agent selected by a capability phrase.
	ParticipantSourceCapability = "capability"
	// ParticipantSourceRole identifies an agent selected by requested role coverage.
	ParticipantSourceRole = "role"
	// ParticipantSourceRecency identifies an agent boosted by recent session context.
	ParticipantSourceRecency = "recency"

	plannerScoringVersion     = "agent-planner-v1"
	requestedEvidenceScore    = 1000.0
	roleCoverageScore         = 42.0
	recencyEvidenceScore      = 12.0
	selectionScoreThreshold   = 40.0
	ambiguityScoreWindow      = 8.0
	minAmbiguityScore         = selectionScoreThreshold
	defaultPlannerConcurrency = 2

	stopReasonMaxParticipants       = "max_participants"
	stopReasonNoEligibleCandidates  = "no_eligible_candidates"
	stopReasonNoMatchingCandidates  = "no_matching_candidates"
	stopReasonRoleCoverageSatisfied = "role_coverage_satisfied"
	stopReasonRequestedOnly         = "requested_only"
	stopReasonNoMoreCandidates      = "no_more_candidates"
)

// OrchestrationRequest describes the inputs used to select agents for a task.
type OrchestrationRequest struct {
	Prompt           string
	RequestedNames   []string
	RecentAgentNames []string
	RequiredTools    []string
	MaxParticipants  int
	MaxConcurrency   int
}

// OrchestrationPlan is a scored, evidence-backed set of agents selected for a
// task plus diagnostics for candidates that were close, truncated, or blocked.
type OrchestrationPlan struct {
	Participants []Participant
	Candidates   []Candidate
	Ambiguities  []Ambiguity
	Composition  PlanComposition
	Metadata     PlanMetadata
}

// Agents returns the selected agents without match metadata.
func (p OrchestrationPlan) Agents() []Agent {
	agents := make([]Agent, 0, len(p.Participants))
	for i := range p.Participants {
		agents = append(agents, p.Participants[i].Agent)
	}

	return agents
}

// Ambiguous reports whether the planner found close-scoring alternatives.
func (p OrchestrationPlan) Ambiguous() bool {
	return len(p.Ambiguities) > 0
}

// Participant describes a selected agent and why it was selected.
//
//nolint:govet // Field order keeps the selected agent before reason metadata.
type Participant struct {
	Agent           Agent
	Source          string
	Pattern         string
	Rationale       string
	Evidence        []MatchEvidence
	Roles           []string
	ToolConstraints []ToolConstraint
	Score           float64
}

// Candidate describes a scored agent considered by the planner.
//
//nolint:govet // Field order keeps agent identity before diagnostics.
type Candidate struct {
	Agent           Agent
	Source          string
	Pattern         string
	Rationale       string
	RejectedReason  string
	Evidence        []MatchEvidence
	Roles           []string
	ToolConstraints []ToolConstraint
	Score           float64
	Eligible        bool
}

// MatchEvidence records one scoring signal used to rank an agent.
type MatchEvidence struct {
	Kind    string
	Pattern string
	Detail  string
	Score   float64
}

// ToolConstraint records whether a candidate can use a required tool.
type ToolConstraint struct {
	Tool    string
	Reason  string
	Allowed bool
}

// Ambiguity reports close-scoring candidates for the same requested role.
type Ambiguity struct {
	Role       string
	Winner     string
	Reason     string
	Candidates []AmbiguityCandidate
}

// AmbiguityCandidate is the compact score/evidence view for ambiguity output.
type AmbiguityCandidate struct {
	Name     string
	Evidence []MatchEvidence
	Score    float64
}

// PlanComposition records the planner's multi-agent composition constraints.
//
//nolint:govet // Field order keeps composition groups readable.
type PlanComposition struct {
	Budget         PlanBudget
	Roles          []PlannedRole
	Dependencies   []PlanDependency
	RequiredTools  []string
	StopReason     string
	MaxConcurrency int
}

// PlanBudget records participant/candidate caps applied during planning.
type PlanBudget struct {
	Truncated       []string
	MaxParticipants int
	CandidateCount  int
	SelectedCount   int
}

// PlannedRole records whether a requested role is covered by the plan.
type PlannedRole struct {
	Name     string
	Agent    string
	Covered  bool
	Required bool
}

// PlanDependency records an ordering rule between selected agents.
type PlanDependency struct {
	Before string
	After  string
	Reason string
}

// PlanMetadata records scoring inputs for later planner evaluation.
type PlanMetadata struct {
	ScoringVersion       string
	PromptTokens         []string
	RequestedNames       []string
	RequestedRoles       []string
	RecentAgentNames     []string
	RequiredTools        []string
	SelectedNames        []string
	CandidateNames       []string
	RejectedNames        []string
	ToolOverrideNames    []string
	SelectionThreshold   float64
	AmbiguityScoreWindow float64
	AmbiguityCount       int
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
// Explicit requested names are selected first in request order. Other agents
// are scored by phrase-boundary trigger/capability evidence, requested role
// coverage, tool constraints, and recent session hints. MaxParticipants caps
// the final set when greater than zero; the plan records candidates,
// ambiguities, truncation, and composition metadata for later evaluation.
func (r *Registry) PlanOrchestration(request OrchestrationRequest) (OrchestrationPlan, error) {
	if r == nil {
		return OrchestrationPlan{}, nil
	}

	known := r.List()

	unknown := unknownRequestedNames(r, request.RequestedNames)
	if len(unknown) > 0 {
		return OrchestrationPlan{}, UnknownAgentsError{Names: unknown, Known: known}
	}

	planner := newScoredOrchestrationPlanner(r, request, known)
	planner.addRequested(request.RequestedNames)
	planner.addAutomaticCandidates(known)
	planner.selectCandidates()
	planner.plan.Ambiguities = detectAmbiguities(planner.plan.Candidates)
	planner.plan.Composition = planner.composePlan()
	planner.recordEvaluationMetadata()

	return planner.plan, nil
}

//nolint:govet // Field order keeps planner inputs, state, and limits grouped.
type scoredOrchestrationPlanner struct {
	registry        *Registry
	considered      map[string]bool
	selected        map[string]bool
	recentRanks     map[string]int
	plan            OrchestrationPlan
	promptTokens    []string
	requestedRoles  []string
	requiredTools   []string
	truncated       []string
	maxParticipants int
	maxConcurrency  int
	stopReason      string
}

func newScoredOrchestrationPlanner(r *Registry, request OrchestrationRequest, known []string) *scoredOrchestrationPlanner {
	promptTokens := tokenizeWords(request.Prompt)
	requiredTools := requiredToolsForPrompt(request.RequiredTools, promptTokens)
	requestedRoles := requestedRolesForPrompt(promptTokens)

	return &scoredOrchestrationPlanner{
		registry:        r,
		considered:      make(map[string]bool, len(known)),
		selected:        make(map[string]bool, len(known)),
		recentRanks:     recentAgentRanks(request.RecentAgentNames),
		promptTokens:    promptTokens,
		requestedRoles:  requestedRoles,
		requiredTools:   requiredTools,
		maxParticipants: request.MaxParticipants,
		maxConcurrency:  request.MaxConcurrency,
		plan: OrchestrationPlan{
			Metadata: PlanMetadata{
				ScoringVersion:       plannerScoringVersion,
				PromptTokens:         append([]string(nil), promptTokens...),
				RequestedNames:       uniqueTrimmedNames(request.RequestedNames),
				RequestedRoles:       append([]string(nil), requestedRoles...),
				RecentAgentNames:     uniqueTrimmedNames(request.RecentAgentNames),
				RequiredTools:        append([]string(nil), requiredTools...),
				SelectionThreshold:   selectionScoreThreshold,
				AmbiguityScoreWindow: ambiguityScoreWindow,
			},
		},
	}
}

func (p *scoredOrchestrationPlanner) addRequested(names []string) {
	for _, name := range names {
		name = strings.TrimSpace(name)
		if name == "" || p.considered[name] {
			continue
		}

		agent, _ := p.registry.Get(name)
		candidate := p.scoreAgent(agent)
		candidate.Source = ParticipantSourceRequested
		candidate.Pattern = ""
		candidate.Score = roundScore(requestedEvidenceScore + math.Min(candidate.Score, 100))
		candidate.Eligible = true
		candidate.RejectedReason = ""
		candidate.Evidence = append([]MatchEvidence{{
			Kind:   ParticipantSourceRequested,
			Detail: "explicit agent override",
			Score:  requestedEvidenceScore,
		}}, candidate.Evidence...)
		candidate.Rationale = rationaleForCandidate(candidate)

		p.considered[name] = true
		if !p.canAddParticipant() {
			candidate.RejectedReason = "max participants reached before requested agent could be selected"
			candidate.Rationale = rationaleForCandidate(candidate)
			p.plan.Candidates = append(p.plan.Candidates, candidate)
			p.truncated = appendIfMissing(p.truncated, candidate.Agent.Name)
			p.stopReason = stopReasonMaxParticipants

			continue
		}

		p.plan.Candidates = append(p.plan.Candidates, candidate)
		p.addParticipant(candidate)
	}
}

func (p *scoredOrchestrationPlanner) addAutomaticCandidates(names []string) {
	for _, name := range names {
		if p.considered[name] {
			continue
		}

		candidate := p.scoreAgent(p.registry.agents[name])
		if candidate.Score <= 0 {
			continue
		}

		p.considered[name] = true
		p.plan.Candidates = append(p.plan.Candidates, candidate)
	}

	sortCandidates(p.plan.Candidates)
}

func (p *scoredOrchestrationPlanner) selectCandidates() {
	eligible := make([]Candidate, 0, len(p.plan.Candidates))
	for i := range p.plan.Candidates {
		candidate := p.plan.Candidates[i]
		if candidate.Source == ParticipantSourceRequested || p.selected[candidate.Agent.Name] || !candidate.Eligible {
			continue
		}

		eligible = append(eligible, candidate)
	}

	sortCandidates(eligible)

	coveredRoles := coveredRequestedRoles(p.requestedRoles, p.plan.Participants)
	skippedCoveredRoles := false

	for i := range eligible {
		if !p.shouldSelectCandidate(eligible[i], coveredRoles) {
			skippedCoveredRoles = true

			p.rejectCandidate(eligible[i].Agent.Name, "requested role coverage already satisfied")

			continue
		}

		if !p.canAddParticipant() {
			p.truncated = appendCandidateNames(p.truncated, eligible[i:])
			p.rejectCandidates(eligible[i:], "max participants reached before candidate could be selected")
			p.stopReason = stopReasonMaxParticipants

			return
		}

		p.addParticipant(eligible[i])
		addCoveredRoles(coveredRoles, p.requestedRoles, eligible[i].Roles)
	}

	if p.stopReason == "" {
		p.stopReason = p.selectionStopReason(eligible, coveredRoles, skippedCoveredRoles)
	}
}

func (p *scoredOrchestrationPlanner) selectionStopReason(
	eligible []Candidate,
	coveredRoles map[string]bool,
	skippedCoveredRoles bool,
) string {
	switch {
	case len(p.plan.Participants) == 0 && len(p.plan.Candidates) > 0:
		return stopReasonNoEligibleCandidates
	case len(p.plan.Participants) == 0:
		return stopReasonNoMatchingCandidates
	case skippedCoveredRoles && allRequestedRolesCovered(p.requestedRoles, coveredRoles):
		return stopReasonRoleCoverageSatisfied
	case len(eligible) == 0:
		return stopReasonRequestedOnly
	default:
		return stopReasonNoMoreCandidates
	}
}

func (p *scoredOrchestrationPlanner) shouldSelectCandidate(candidate Candidate, coveredRoles map[string]bool) bool {
	if len(p.requestedRoles) == 0 {
		return true
	}

	for _, role := range candidate.Roles {
		if containsString(p.requestedRoles, role) && !coveredRoles[role] {
			return true
		}
	}

	// If a direct phrase match does not map to any requested role, keep it: the
	// user may have used a domain-specific trigger that the generic role catalog
	// does not know about.
	return len(candidate.Roles) == 0 && hasDirectPhraseEvidence(candidate)
}

func (p *scoredOrchestrationPlanner) scoreAgent(agent Agent) Candidate {
	evidence := make([]MatchEvidence, 0)
	evidence = append(evidence, phraseEvidenceForPatterns(p.promptTokens, ParticipantSourceTrigger, agent.Triggers)...)
	evidence = append(evidence, phraseEvidenceForPatterns(p.promptTokens, ParticipantSourceCapability, agent.Capabilities)...)

	roles := coveredRolesForAgent(agent, p.requestedRoles)
	for _, role := range roles {
		evidence = append(evidence, MatchEvidence{
			Kind:    ParticipantSourceRole,
			Pattern: role,
			Detail:  "agent metadata covers requested role",
			Score:   roleCoverageScore,
		})
	}

	if rank, ok := p.recentRanks[agent.Name]; ok && len(evidence) > 0 {
		evidence = append(evidence, MatchEvidence{
			Kind:   ParticipantSourceRecency,
			Detail: fmt.Sprintf("recent session agent rank %d", rank+1),
			Score:  roundScore(recencyEvidenceScore / float64(rank+1)),
		})
	}

	sortEvidence(evidence)

	source, pattern := topEvidenceSource(evidence)
	constraints, toolsAllowed := toolConstraintsForAgent(agent, p.requiredTools)
	score := aggregateEvidenceScore(evidence)

	candidate := Candidate{
		Agent:           agent,
		Source:          source,
		Pattern:         pattern,
		Evidence:        evidence,
		Roles:           roles,
		ToolConstraints: constraints,
		Score:           score,
		Eligible:        score >= selectionScoreThreshold && toolsAllowed,
	}
	if score > 0 && score < selectionScoreThreshold {
		candidate.RejectedReason = fmt.Sprintf("score %.1f below selection threshold %.1f", score, selectionScoreThreshold)
	}

	if !toolsAllowed {
		candidate.RejectedReason = missingToolReason(constraints)
	}

	candidate.Rationale = rationaleForCandidate(candidate)

	return candidate
}

func (p *scoredOrchestrationPlanner) addParticipant(candidate Candidate) {
	if p.selected[candidate.Agent.Name] {
		return
	}

	if !p.canAddParticipant() {
		p.truncated = appendIfMissing(p.truncated, candidate.Agent.Name)
		p.stopReason = stopReasonMaxParticipants

		return
	}

	p.plan.Participants = append(p.plan.Participants, participantFromCandidate(candidate))
	p.selected[candidate.Agent.Name] = true
}

func (p *scoredOrchestrationPlanner) rejectCandidates(candidates []Candidate, reason string) {
	for i := range candidates {
		p.rejectCandidate(candidates[i].Agent.Name, reason)
	}
}

func (p *scoredOrchestrationPlanner) rejectCandidate(name, reason string) {
	for i := range p.plan.Candidates {
		if p.plan.Candidates[i].Agent.Name != name {
			continue
		}

		p.plan.Candidates[i].RejectedReason = reason
		p.plan.Candidates[i].Rationale = rationaleForCandidate(p.plan.Candidates[i])

		return
	}
}

func (p *scoredOrchestrationPlanner) canAddParticipant() bool {
	return p.maxParticipants <= 0 || len(p.plan.Participants) < p.maxParticipants
}

func (p *scoredOrchestrationPlanner) composePlan() PlanComposition {
	stopReason := p.stopReason
	if stopReason == "" {
		stopReason = stopReasonNoMoreCandidates
	}

	return PlanComposition{
		Budget: PlanBudget{
			Truncated:       append([]string(nil), p.truncated...),
			MaxParticipants: p.maxParticipants,
			CandidateCount:  len(p.plan.Candidates),
			SelectedCount:   len(p.plan.Participants),
		},
		Roles:          plannedRoles(p.requestedRoles, p.plan.Participants),
		Dependencies:   planDependencies(p.plan.Participants),
		RequiredTools:  append([]string(nil), p.requiredTools...),
		StopReason:     stopReason,
		MaxConcurrency: plannerMaxConcurrency(p.maxConcurrency, len(p.plan.Participants), p.maxParticipants),
	}
}

func (p *scoredOrchestrationPlanner) recordEvaluationMetadata() {
	p.plan.Metadata.SelectedNames = participantNames(p.plan.Participants)
	p.plan.Metadata.CandidateNames = candidateNames(p.plan.Candidates)
	p.plan.Metadata.RejectedNames = rejectedCandidateNames(p.plan.Candidates)
	p.plan.Metadata.ToolOverrideNames = toolOverrideParticipantNames(p.plan.Participants)
	p.plan.Metadata.AmbiguityCount = len(p.plan.Ambiguities)
}

func participantFromCandidate(candidate Candidate) Participant {
	return Participant{
		Agent:           candidate.Agent,
		Source:          candidate.Source,
		Pattern:         candidate.Pattern,
		Rationale:       candidate.Rationale,
		Evidence:        append([]MatchEvidence(nil), candidate.Evidence...),
		Roles:           append([]string(nil), candidate.Roles...),
		ToolConstraints: append([]ToolConstraint(nil), candidate.ToolConstraints...),
		Score:           candidate.Score,
	}
}

func participantNames(participants []Participant) []string {
	names := make([]string, 0, len(participants))

	for i := range participants {
		names = appendIfMissing(names, participants[i].Agent.Name)
	}

	return names
}

func candidateNames(candidates []Candidate) []string {
	names := make([]string, 0, len(candidates))

	for i := range candidates {
		names = appendIfMissing(names, candidates[i].Agent.Name)
	}

	return names
}

func rejectedCandidateNames(candidates []Candidate) []string {
	names := make([]string, 0)

	for i := range candidates {
		if candidates[i].Eligible && candidates[i].RejectedReason == "" {
			continue
		}

		names = appendIfMissing(names, candidates[i].Agent.Name)
	}

	return names
}

func toolOverrideParticipantNames(participants []Participant) []string {
	names := make([]string, 0)

	for i := range participants {
		if len(deniedToolConstraints(participants[i].ToolConstraints)) == 0 {
			continue
		}

		names = appendIfMissing(names, participants[i].Agent.Name)
	}

	return names
}

func phraseEvidenceForPatterns(promptTokens []string, kind string, patterns []string) []MatchEvidence {
	evidence := make([]MatchEvidence, 0, len(patterns))
	for _, pattern := range patterns {
		if match, ok := phraseEvidence(promptTokens, kind, pattern); ok {
			evidence = append(evidence, match)
		}
	}

	return evidence
}

func phraseEvidence(promptTokens []string, kind, pattern string) (MatchEvidence, bool) {
	phraseTokens := tokenizeWords(pattern)
	if len(phraseTokens) == 0 || !tokensContainPhrase(promptTokens, phraseTokens) {
		return MatchEvidence{}, false
	}

	base := 48.0
	if kind == ParticipantSourceTrigger {
		base = 70.0
	}

	specificity := float64(len(phraseTokens))*8 + math.Min(float64(joinedTokenLength(phraseTokens))/2, 16)
	score := base + specificity - shortPhrasePenalty(phraseTokens)

	return MatchEvidence{
		Kind:    kind,
		Pattern: pattern,
		Detail:  fmt.Sprintf("phrase-boundary match across %d token(s)", len(phraseTokens)),
		Score:   roundScore(score),
	}, true
}

func shortPhrasePenalty(tokens []string) float64 {
	if len(tokens) != 1 {
		return 0
	}

	switch l := len(tokens[0]); {
	case l <= 2:
		return 30
	case l == 3:
		return 12
	default:
		return 0
	}
}

func aggregateEvidenceScore(evidence []MatchEvidence) float64 {
	if len(evidence) == 0 {
		return 0
	}

	sortEvidence(evidence)

	score := evidence[0].Score
	for i := 1; i < len(evidence); i++ {
		weight := 0.35

		switch evidence[i].Kind {
		case ParticipantSourceRole:
			weight = 0.5
		case ParticipantSourceRecency:
			weight = 1
		case ParticipantSourceRequested:
			weight = 1
		}

		score += evidence[i].Score * weight
	}

	return roundScore(score)
}

func topEvidenceSource(evidence []MatchEvidence) (source, pattern string) {
	if len(evidence) == 0 {
		return "", ""
	}

	return evidence[0].Kind, evidence[0].Pattern
}

func sortEvidence(evidence []MatchEvidence) {
	sort.SliceStable(evidence, func(i, j int) bool {
		if evidence[i].Score != evidence[j].Score {
			return evidence[i].Score > evidence[j].Score
		}

		if evidencePriority(evidence[i].Kind) != evidencePriority(evidence[j].Kind) {
			return evidencePriority(evidence[i].Kind) > evidencePriority(evidence[j].Kind)
		}

		return evidence[i].Pattern < evidence[j].Pattern
	})
}

func evidencePriority(kind string) int {
	switch kind {
	case ParticipantSourceRequested:
		return 5
	case ParticipantSourceTrigger:
		return 4
	case ParticipantSourceCapability:
		return 3
	case ParticipantSourceRole:
		return 2
	case ParticipantSourceRecency:
		return 1
	default:
		return 0
	}
}

func sortCandidates(candidates []Candidate) {
	sort.SliceStable(candidates, func(i, j int) bool {
		if candidates[i].Score != candidates[j].Score {
			return candidates[i].Score > candidates[j].Score
		}

		if candidates[i].Eligible != candidates[j].Eligible {
			return candidates[i].Eligible
		}

		if len(candidates[i].Evidence) != len(candidates[j].Evidence) {
			return len(candidates[i].Evidence) > len(candidates[j].Evidence)
		}

		return candidates[i].Agent.Name < candidates[j].Agent.Name
	})
}

func rationaleForCandidate(candidate Candidate) string {
	if candidate.RejectedReason != "" {
		return "not selected: " + candidate.RejectedReason
	}

	if candidate.Source == ParticipantSourceRequested {
		if denied := deniedToolConstraints(candidate.ToolConstraints); len(denied) > 0 {
			return "selected because explicitly requested; tool constraint override: " + strings.Join(denied, ", ")
		}

		return "selected because explicitly requested"
	}

	if len(candidate.Evidence) == 0 {
		return "not selected: no trigger, capability, role, or context evidence"
	}

	top := candidate.Evidence[0]
	switch top.Kind {
	case ParticipantSourceTrigger, ParticipantSourceCapability:
		return fmt.Sprintf("%s %q matched with boundary-aware score %.1f", top.Kind, top.Pattern, candidate.Score)
	case ParticipantSourceRole:
		return fmt.Sprintf("covers requested role %q with score %.1f", top.Pattern, candidate.Score)
	default:
		return fmt.Sprintf("%s evidence scored %.1f", top.Kind, candidate.Score)
	}
}

func requiredToolsForPrompt(configured, promptTokens []string) []string {
	required := normalizePhrases(configured)

	if promptImpliesShellTool(promptTokens) {
		required = appendIfMissing(required, "bash")
	}

	for _, rule := range promptToolRules {
		if promptImpliesTool(promptTokens, rule.aliases) {
			required = appendIfMissing(required, rule.tool)
		}
	}

	return required
}

type promptToolRule struct {
	tool    string
	aliases []string
}

var promptToolRules = []promptToolRule{
	{
		tool:    "read",
		aliases: []string{"read", "inspect", "view", "open", "cat"},
	},
	{
		tool:    "grep",
		aliases: []string{"grep", "search", "find"},
	},
	{
		tool:    "glob",
		aliases: []string{"glob"},
	},
	{
		tool:    "edit",
		aliases: []string{"edit", "modify", "update", "patch", "refactor", "fix", "implement"},
	},
	{
		tool:    "write",
		aliases: []string{"write", "create", "add", "generate", "save"},
	},
}

var shellToolAliases = []string{
	"bash",
	"shell",
	"terminal",
	"run command",
	"run commands",
	"execute command",
	"execute commands",
	"go test",
	"make test",
	"npm test",
	"go build",
	"make build",
	"run build",
	"run lint",
}

var shellActionTokens = []string{"run", "execute", "exec"}

var shellObjectTokens = []string{"test", "tests", "testing", "suite", "build", "lint", "command", "commands"}

func promptImpliesShellTool(tokens []string) bool {
	if promptImpliesTool(tokens, shellToolAliases) {
		return true
	}

	for i, token := range tokens {
		if !containsString(shellActionTokens, token) {
			continue
		}

		for _, following := range tokensAfter(tokens, i, 4) {
			if containsString(shellObjectTokens, following) {
				return true
			}
		}
	}

	return false
}

func promptImpliesTool(tokens, aliases []string) bool {
	for _, alias := range aliases {
		if tokensContainPhrase(tokens, tokenizeWords(alias)) {
			return true
		}
	}

	return false
}

func tokensAfter(tokens []string, index, limit int) []string {
	if index+1 >= len(tokens) || limit <= 0 {
		return nil
	}

	end := min(index+1+limit, len(tokens))

	return tokens[index+1 : end]
}

func toolConstraintsForAgent(agent Agent, requiredTools []string) ([]ToolConstraint, bool) {
	if len(requiredTools) == 0 {
		return nil, true
	}

	constraints := make([]ToolConstraint, 0, len(requiredTools))
	allowed := true

	for _, tool := range requiredTools {
		toolAllowed := agent.HasToolPermission(tool)
		reason := "required tool is permitted"

		if !toolAllowed {
			allowed = false
			reason = "required tool is not permitted"
		}

		constraints = append(constraints, ToolConstraint{
			Tool:    tool,
			Reason:  reason,
			Allowed: toolAllowed,
		})
	}

	return constraints, allowed
}

func missingToolReason(constraints []ToolConstraint) string {
	denied := deniedToolConstraints(constraints)
	if len(denied) == 0 {
		return ""
	}

	return "missing required tool permission: " + strings.Join(denied, ", ")
}

func deniedToolConstraints(constraints []ToolConstraint) []string {
	denied := make([]string, 0)

	for _, constraint := range constraints {
		if !constraint.Allowed {
			denied = append(denied, constraint.Tool)
		}
	}

	return denied
}

type plannerRoleDefinition struct {
	Name    string
	Aliases []string
	Phase   int
}

var plannerRoles = []plannerRoleDefinition{
	{
		Name:    "planning",
		Aliases: []string{"plan", "planner", "planning", "architecture", "architect", "design", "strategy", "prd"},
		Phase:   10,
	},
	{
		Name:    "research",
		Aliases: []string{"research", "investigate", "lookup", "documentation reference", "reference"},
		Phase:   20,
	},
	{
		Name:    "debugging",
		Aliases: []string{"debug", "debugger", "diagnose", "troubleshoot", "root cause", "failure", "fix failing"},
		Phase:   25,
	},
	{
		Name:    "implementation",
		Aliases: []string{"implement", "implementation", "coding", "executor", "build", "feature", "fix", "refactor"},
		Phase:   30,
	},
	{
		Name:    "verification",
		Aliases: []string{"test", "tests", "testing", "verify", "verifier", "validate", "validation", "qa", "regression"},
		Phase:   40,
	},
	{
		Name:    "review",
		Aliases: []string{"review", "reviewer", "code review", "critic", "audit", "critique"},
		Phase:   50,
	},
	{
		Name:    "security",
		Aliases: []string{"security", "secure", "auth", "authentication", "authorization", "permission", "permissions", "vulnerability"},
		Phase:   50,
	},
	{
		Name:    "documentation",
		Aliases: []string{"doc", "docs", "document", "documentation", "write", "writer", "readme", "guide"},
		Phase:   60,
	},
}

func requestedRolesForPrompt(promptTokens []string) []string {
	roles := make([]string, 0)

	for _, role := range plannerRoles {
		if roleMatchesTokens(role, promptTokens) {
			roles = append(roles, role.Name)
		}
	}

	return roles
}

func roleMatchesTokens(role plannerRoleDefinition, tokens []string) bool {
	for _, alias := range role.Aliases {
		if tokensContainPhrase(tokens, tokenizeWords(alias)) {
			return true
		}
	}

	return false
}

func coveredRolesForAgent(agent Agent, requestedRoles []string) []string {
	if len(requestedRoles) == 0 {
		return nil
	}

	agentTokens := tokenizeWords(strings.Join(agentRoleText(agent), " "))
	covered := make([]string, 0, len(requestedRoles))

	for _, requestedRole := range requestedRoles {
		role, ok := plannerRoleByName(requestedRole)
		if !ok {
			continue
		}

		if roleMatchesTokens(role, agentTokens) {
			covered = append(covered, requestedRole)
		}
	}

	return covered
}

func agentRoleText(agent Agent) []string {
	parts := []string{
		agent.Name,
		agent.Mode,
		agent.Description,
		agent.Personality,
	}
	parts = append(parts, agent.Capabilities...)
	parts = append(parts, agent.Triggers...)

	return parts
}

func plannerRoleByName(name string) (plannerRoleDefinition, bool) {
	for _, role := range plannerRoles {
		if role.Name == name {
			return role, true
		}
	}

	return plannerRoleDefinition{}, false
}

func plannedRoles(requestedRoles []string, participants []Participant) []PlannedRole {
	roleNames := append([]string(nil), requestedRoles...)

	for i := range participants {
		for _, role := range participants[i].Roles {
			roleNames = appendIfMissing(roleNames, role)
		}
	}

	out := make([]PlannedRole, 0, len(roleNames))
	for _, role := range roleNames {
		planned := PlannedRole{
			Name:     role,
			Required: containsString(requestedRoles, role),
		}

		for i := range participants {
			if containsString(participants[i].Roles, role) {
				planned.Agent = participants[i].Agent.Name
				planned.Covered = true

				break
			}
		}

		out = append(out, planned)
	}

	return out
}

func coveredRequestedRoles(requestedRoles []string, participants []Participant) map[string]bool {
	covered := make(map[string]bool, len(requestedRoles))

	for i := range participants {
		addCoveredRoles(covered, requestedRoles, participants[i].Roles)
	}

	return covered
}

func addCoveredRoles(covered map[string]bool, requestedRoles, roles []string) {
	for _, role := range roles {
		if containsString(requestedRoles, role) {
			covered[role] = true
		}
	}
}

func allRequestedRolesCovered(requestedRoles []string, covered map[string]bool) bool {
	for _, role := range requestedRoles {
		if !covered[role] {
			return false
		}
	}

	return len(requestedRoles) > 0
}

func hasDirectPhraseEvidence(candidate Candidate) bool {
	for _, item := range candidate.Evidence {
		if item.Kind == ParticipantSourceTrigger || item.Kind == ParticipantSourceCapability {
			return true
		}
	}

	return false
}

func planDependencies(participants []Participant) []PlanDependency {
	type phasedParticipant struct {
		name  string
		role  string
		phase int
		index int
	}

	phased := make([]phasedParticipant, 0, len(participants))

	for i := range participants {
		role, phase := primaryParticipantPhase(&participants[i])
		if phase == 0 {
			continue
		}

		phased = append(phased, phasedParticipant{
			name:  participants[i].Agent.Name,
			role:  role,
			phase: phase,
			index: i,
		})
	}

	sort.SliceStable(phased, func(i, j int) bool {
		if phased[i].phase != phased[j].phase {
			return phased[i].phase < phased[j].phase
		}

		return phased[i].index < phased[j].index
	})

	dependencies := make([]PlanDependency, 0)

	for i := range len(phased) - 1 {
		if phased[i].phase >= phased[i+1].phase {
			continue
		}

		dependencies = append(dependencies, PlanDependency{
			Before: phased[i].name,
			After:  phased[i+1].name,
			Reason: fmt.Sprintf("role order: %s before %s", phased[i].role, phased[i+1].role),
		})
	}

	return dependencies
}

func primaryParticipantPhase(participant *Participant) (bestRole string, bestPhase int) {
	for _, roleName := range participant.Roles {
		role, ok := plannerRoleByName(roleName)
		if !ok {
			continue
		}

		if bestPhase == 0 || role.Phase < bestPhase {
			bestRole = role.Name
			bestPhase = role.Phase
		}
	}

	return bestRole, bestPhase
}

func plannerMaxConcurrency(requested, participantCount, maxParticipants int) int {
	if participantCount == 0 {
		return 0
	}

	maxConcurrency := requested
	if maxConcurrency <= 0 {
		maxConcurrency = defaultPlannerConcurrency
	}

	if maxParticipants > 0 && maxConcurrency > maxParticipants {
		maxConcurrency = maxParticipants
	}

	if maxConcurrency > participantCount {
		maxConcurrency = participantCount
	}

	if maxConcurrency < 1 {
		return 1
	}

	return maxConcurrency
}

func detectAmbiguities(candidates []Candidate) []Ambiguity {
	groups := make(map[string][]Candidate)

	for i := range candidates {
		candidate := candidates[i]
		if candidate.Source == ParticipantSourceRequested || !candidate.Eligible || candidate.Score < minAmbiguityScore {
			continue
		}

		keys := candidate.Roles
		if len(keys) == 0 {
			keys = []string{"general"}
		}

		for _, key := range keys {
			groups[key] = append(groups[key], candidate)
		}
	}

	ambiguities := make([]Ambiguity, 0)

	for role, group := range groups {
		if len(group) < 2 {
			continue
		}

		sortCandidates(group)

		top := group[0]
		closeCandidates := []AmbiguityCandidate{ambiguityCandidate(top)}

		for i := 1; i < len(group); i++ {
			if top.Score-group[i].Score > ambiguityScoreWindow {
				break
			}

			closeCandidates = append(closeCandidates, ambiguityCandidate(group[i]))
		}

		if len(closeCandidates) < 2 {
			continue
		}

		ambiguities = append(ambiguities, Ambiguity{
			Role:       role,
			Winner:     top.Agent.Name,
			Reason:     ambiguityReason(top, group[1], len(closeCandidates)),
			Candidates: closeCandidates,
		})
	}

	sort.SliceStable(ambiguities, func(i, j int) bool {
		return ambiguities[i].Role < ambiguities[j].Role
	})

	return ambiguities
}

func ambiguityReason(winner, runnerUp Candidate, closeCandidateCount int) string {
	margin := roundScore(winner.Score - runnerUp.Score)
	if margin > 0 {
		return fmt.Sprintf(
			"%d candidates scored within %.1f points; winner has top score by %.1f point(s)",
			closeCandidateCount,
			ambiguityScoreWindow,
			margin,
		)
	}

	return fmt.Sprintf(
		"%d candidates scored within %.1f points; scores tied, winner selected by deterministic tie-breakers",
		closeCandidateCount,
		ambiguityScoreWindow,
	)
}

func ambiguityCandidate(candidate Candidate) AmbiguityCandidate {
	evidence := candidate.Evidence
	if len(evidence) > 3 {
		evidence = evidence[:3]
	}

	return AmbiguityCandidate{
		Name:     candidate.Agent.Name,
		Score:    candidate.Score,
		Evidence: append([]MatchEvidence(nil), evidence...),
	}
}

func recentAgentRanks(names []string) map[string]int {
	ranks := make(map[string]int, len(names))
	for _, name := range names {
		name = strings.TrimSpace(name)
		if name == "" {
			continue
		}

		if _, ok := ranks[name]; !ok {
			ranks[name] = len(ranks)
		}
	}

	return ranks
}

func uniqueTrimmedNames(names []string) []string {
	out := make([]string, 0, len(names))
	for _, name := range names {
		out = appendIfMissing(out, strings.TrimSpace(name))
	}

	return out
}

func appendCandidateNames(names []string, candidates []Candidate) []string {
	for i := range candidates {
		names = appendIfMissing(names, candidates[i].Agent.Name)
	}

	return names
}

func appendIfMissing(values []string, value string) []string {
	if value == "" || containsString(values, value) {
		return values
	}

	return append(values, value)
}

func containsString(values []string, value string) bool {
	return slices.Contains(values, value)
}

func tokenizeWords(text string) []string {
	tokens := make([]string, 0)

	var builder strings.Builder

	flush := func() {
		if builder.Len() == 0 {
			return
		}

		tokens = append(tokens, builder.String())
		builder.Reset()
	}

	for _, r := range strings.ToLower(text) {
		if unicode.IsLetter(r) || unicode.IsDigit(r) {
			builder.WriteRune(r)
			continue
		}

		flush()
	}

	flush()

	return tokens
}

func tokensContainPhrase(tokens, phrase []string) bool {
	if len(tokens) == 0 || len(phrase) == 0 || len(phrase) > len(tokens) {
		return false
	}

	for i := 0; i <= len(tokens)-len(phrase); i++ {
		matched := true

		for j := range phrase {
			if tokens[i+j] != phrase[j] {
				matched = false
				break
			}
		}

		if matched {
			return true
		}
	}

	return false
}

func joinedTokenLength(tokens []string) int {
	n := 0
	for _, token := range tokens {
		n += len(token)
	}

	return n
}

func roundScore(score float64) float64 {
	return math.Round(score*10) / 10
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
