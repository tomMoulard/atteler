package agent

import (
	"errors"
	"reflect"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/tommoulard/atteler/pkg/config"
)

func TestRegistry_PlanOrchestration_OrdersRequestedTriggersAndCapabilities(t *testing.T) {
	t.Parallel()

	registry := NewRegistry(map[string]config.AgentConfig{
		"architect": {Capabilities: []string{"design"}},
		"reviewer":  {Capabilities: []string{"security"}, Triggers: []string{"review"}},
		"writer":    {Triggers: []string{"docs"}},
	})

	plan, err := registry.PlanOrchestration(OrchestrationRequest{
		Prompt:         "Please review the design and docs for security.",
		RequestedNames: []string{"writer"},
	})
	require.NoError(t, err)

	assertParticipantNames(t, plan, []string{"writer", "reviewer", "architect"})
	assertParticipantSources(t, plan, []string{
		ParticipantSourceRequested,
		ParticipantSourceTrigger,
		ParticipantSourceCapability,
	})

	if plan.Participants[1].Pattern != "review" || plan.Participants[2].Pattern != "design" {
		assert.Failf(t, "assertion failed", "patterns = %q/%q", plan.Participants[1].Pattern, plan.Participants[2].Pattern)
	}

	assert.Equal(t, plannerScoringVersion, plan.Metadata.ScoringVersion)
	assert.NotEmpty(t, plan.Participants[1].Evidence)
	assert.NotEmpty(t, plan.Participants[1].Rationale)
}

func TestRegistry_PlanOrchestration_MaxParticipantsCapsStableSelection(t *testing.T) {
	t.Parallel()

	registry := NewRegistry(map[string]config.AgentConfig{
		"alpha":   {Triggers: []string{"ship"}},
		"beta":    {Triggers: []string{"ship"}},
		"charlie": {Capabilities: []string{"ship"}},
	})

	plan, err := registry.PlanOrchestration(OrchestrationRequest{
		Prompt:          "ship it",
		RequestedNames:  []string{"charlie"},
		MaxParticipants: 2,
	})
	require.NoError(t, err)

	assertParticipantNames(t, plan, []string{"charlie", "alpha"})
}

func TestRegistry_PlanOrchestration_OverlappingTriggersPreferSpecificIntent(t *testing.T) {
	t.Parallel()

	registry := NewRegistry(map[string]config.AgentConfig{
		"aaa-general": {Triggers: []string{"go"}},
		"zzz-tester":  {Triggers: []string{"go test"}},
	})

	plan, err := registry.PlanOrchestration(OrchestrationRequest{
		Prompt:          "please go test the package",
		MaxParticipants: 1,
	})
	require.NoError(t, err)

	assertParticipantNames(t, plan, []string{"zzz-tester"})
	assert.Equal(t, "go test", plan.Participants[0].Pattern)
	assert.Greater(t, plan.Participants[0].Score, candidateScore(t, plan, "aaa-general"))
	assert.Equal(t, "max_participants", plan.Composition.StopReason)
	assert.Contains(t, plan.Composition.Budget.Truncated, "aaa-general")
}

func TestRegistry_PlanOrchestration_OverlappingCapabilitiesPreferSpecificIntent(t *testing.T) {
	t.Parallel()

	registry := NewRegistry(map[string]config.AgentConfig{
		"aaa-general":    {Capabilities: []string{"test"}},
		"zzz-specialist": {Capabilities: []string{"integration test"}},
	})

	plan, err := registry.PlanOrchestration(OrchestrationRequest{
		Prompt:          "run the integration test suite",
		MaxParticipants: 1,
	})
	require.NoError(t, err)

	assertParticipantNames(t, plan, []string{"zzz-specialist"})
	assert.Equal(t, ParticipantSourceCapability, plan.Participants[0].Source)
	assert.Equal(t, "integration test", plan.Participants[0].Pattern)
	assert.Greater(t, plan.Participants[0].Score, candidateScore(t, plan, "aaa-general"))
}

func TestRegistry_PlanOrchestration_DoesNotMatchInsideWords(t *testing.T) {
	t.Parallel()

	registry := NewRegistry(map[string]config.AgentConfig{
		"go-agent": {Triggers: []string{"go"}},
	})

	plan, err := registry.PlanOrchestration(OrchestrationRequest{Prompt: "golang migration"})
	require.NoError(t, err)

	assert.Empty(t, plan.Participants)
	assert.Empty(t, plan.Candidates)
	assert.Equal(t, "no_matching_candidates", plan.Composition.StopReason)
}

func TestRegistry_PlanOrchestration_AmbiguousPromptReportsCloseCandidates(t *testing.T) {
	t.Parallel()

	registry := NewRegistry(map[string]config.AgentConfig{
		"auth-a": {Triggers: []string{"auth"}},
		"auth-b": {Triggers: []string{"auth"}},
	})

	plan, err := registry.PlanOrchestration(OrchestrationRequest{
		Prompt:          "review auth permissions",
		MaxParticipants: 1,
	})
	require.NoError(t, err)

	require.True(t, plan.Ambiguous())
	require.NotEmpty(t, plan.Ambiguities)
	assert.Equal(t, "security", plan.Ambiguities[0].Role)
	assert.Len(t, plan.Ambiguities[0].Candidates, 2)
	assert.Contains(t, plan.Ambiguities[0].Reason, "deterministic tie-breakers")
}

func TestRegistry_PlanOrchestration_ShortTriggerCollisionReportsAmbiguity(t *testing.T) {
	t.Parallel()

	registry := NewRegistry(map[string]config.AgentConfig{
		"aaa-go": {Triggers: []string{"go"}},
		"zzz-go": {Triggers: []string{"go"}},
	})

	plan, err := registry.PlanOrchestration(OrchestrationRequest{
		Prompt:          "go",
		MaxParticipants: 1,
	})
	require.NoError(t, err)

	assertParticipantNames(t, plan, []string{"aaa-go"})
	require.True(t, plan.Ambiguous())
	require.NotEmpty(t, plan.Ambiguities)
	assert.Equal(t, 1, plan.Metadata.AmbiguityCount)
	assert.Equal(t, "general", plan.Ambiguities[0].Role)
	assert.Len(t, plan.Ambiguities[0].Candidates, 2)
	assert.Equal(t, "go", plan.Participants[0].Pattern)
}

func TestRegistry_PlanOrchestration_RecentSessionContextBreaksCloseTie(t *testing.T) {
	t.Parallel()

	registry := NewRegistry(map[string]config.AgentConfig{
		"alpha-reviewer": {Triggers: []string{"review"}},
		"beta-reviewer":  {Triggers: []string{"review"}},
	})

	plan, err := registry.PlanOrchestration(OrchestrationRequest{
		Prompt:           "review this change",
		RecentAgentNames: []string{"beta-reviewer"},
		MaxParticipants:  1,
	})
	require.NoError(t, err)

	assertParticipantNames(t, plan, []string{"beta-reviewer"})
	assert.Greater(t, candidateScore(t, plan, "beta-reviewer"), candidateScore(t, plan, "alpha-reviewer"))
	assert.Contains(t, evidenceKinds(plan.Participants[0].Evidence), ParticipantSourceRecency)
	assert.Equal(t, []string{"beta-reviewer"}, plan.Metadata.RecentAgentNames)
	assert.Equal(t, []string{"beta-reviewer"}, plan.Metadata.SelectedNames)
	assert.Equal(t, []string{"beta-reviewer", "alpha-reviewer"}, plan.Metadata.CandidateNames)
	assert.InDelta(t, selectionScoreThreshold, plan.Metadata.SelectionThreshold, 0.000000001)
	assert.InDelta(t, ambiguityScoreWindow, plan.Metadata.AmbiguityScoreWindow, 0.000000001)
}

func TestRegistry_PlanOrchestration_AmbiguityReasonExplainsScoreMargin(t *testing.T) {
	t.Parallel()

	registry := NewRegistry(map[string]config.AgentConfig{
		"alpha-reviewer": {Triggers: []string{"review"}},
		"beta-reviewer":  {Triggers: []string{"review"}},
	})

	plan, err := registry.PlanOrchestration(OrchestrationRequest{
		Prompt:           "review this change",
		RecentAgentNames: []string{"other-agent", "beta-reviewer"},
		MaxParticipants:  1,
	})
	require.NoError(t, err)

	assertParticipantNames(t, plan, []string{"beta-reviewer"})
	require.True(t, plan.Ambiguous())
	require.NotEmpty(t, plan.Ambiguities)
	assert.Equal(t, "beta-reviewer", plan.Ambiguities[0].Winner)
	assert.Contains(t, plan.Ambiguities[0].Reason, "top score by 6.0 point(s)")
}

func TestRegistry_PlanOrchestration_HiddenAgentsAreExcludedUnlessRequested(t *testing.T) {
	t.Parallel()

	registry := NewRegistry(map[string]config.AgentConfig{
		"hidden-reviewer": {
			Triggers: []string{"review"},
			Hidden:   true,
		},
		"visible-reviewer": {Triggers: []string{"review"}},
	})

	plan, err := registry.PlanOrchestration(OrchestrationRequest{Prompt: "review this"})
	require.NoError(t, err)

	assertParticipantNames(t, plan, []string{"visible-reviewer"})
	assert.Nil(t, findCandidate(plan, "hidden-reviewer"))

	plan, err = registry.PlanOrchestration(OrchestrationRequest{
		Prompt:         "review this",
		RequestedNames: []string{"hidden-reviewer"},
	})
	require.NoError(t, err)

	assertParticipantNames(t, plan, []string{"hidden-reviewer"})
	assert.Equal(t, ParticipantSourceRequested, plan.Participants[0].Source)
	visible := findCandidate(plan, "visible-reviewer")
	require.NotNil(t, visible)
	assert.Contains(t, visible.RejectedReason, "role coverage already satisfied")
	assert.Contains(t, visible.Rationale, "not selected")
	assert.Equal(t, []string{"visible-reviewer"}, plan.Metadata.RejectedNames)
	assert.Equal(t, "role_coverage_satisfied", plan.Composition.StopReason)
}

func TestRegistry_PlanOrchestration_StopReasonDoesNotClaimUncoveredRolesSatisfied(t *testing.T) {
	t.Parallel()

	registry := NewRegistry(map[string]config.AgentConfig{
		"auth-a": {Triggers: []string{"auth"}},
		"auth-b": {Triggers: []string{"auth"}},
	})

	plan, err := registry.PlanOrchestration(OrchestrationRequest{
		Prompt:          "review auth permissions",
		MaxParticipants: 2,
	})
	require.NoError(t, err)

	assertParticipantNames(t, plan, []string{"auth-a"})
	assert.Equal(t, "no_more_candidates", plan.Composition.StopReason)
	assertPlannedRoleUncovered(t, plan, "review")
	assertPlannedRole(t, plan, "security", "auth-a")
}

func TestRegistry_PlanOrchestration_MaxParticipantTruncationRecordsDiagnostics(t *testing.T) {
	t.Parallel()

	registry := NewRegistry(map[string]config.AgentConfig{
		"alpha":   {Triggers: []string{"ship"}},
		"beta":    {Triggers: []string{"ship"}},
		"charlie": {Triggers: []string{"ship"}},
	})

	plan, err := registry.PlanOrchestration(OrchestrationRequest{
		Prompt:          "ship this",
		MaxParticipants: 2,
	})
	require.NoError(t, err)

	assertParticipantNames(t, plan, []string{"alpha", "beta"})
	assert.Equal(t, "max_participants", plan.Composition.StopReason)
	assert.Equal(t, 3, plan.Composition.Budget.CandidateCount)
	assert.Equal(t, 2, plan.Composition.Budget.SelectedCount)
	assert.Equal(t, []string{"charlie"}, plan.Composition.Budget.Truncated)
	assert.Equal(t, []string{"charlie"}, plan.Metadata.RejectedNames)

	charlie := findCandidate(plan, "charlie")
	require.NotNil(t, charlie)
	assert.Contains(t, charlie.RejectedReason, "max participants")
	assert.Contains(t, charlie.Rationale, "not selected")
}

func TestRegistry_PlanOrchestration_MaxParticipantOverflowDeduplicatesRequestedCandidates(t *testing.T) {
	t.Parallel()

	registry := NewRegistry(map[string]config.AgentConfig{
		"alpha": {Triggers: []string{"ship"}},
		"beta":  {Triggers: []string{"ship"}},
	})

	plan, err := registry.PlanOrchestration(OrchestrationRequest{
		Prompt:          "ship this",
		RequestedNames:  []string{"alpha", "beta", "beta"},
		MaxParticipants: 1,
	})
	require.NoError(t, err)

	assertParticipantNames(t, plan, []string{"alpha"})
	assert.Equal(t, "max_participants", plan.Composition.StopReason)
	assert.Equal(t, 2, plan.Composition.Budget.CandidateCount)
	assert.Equal(t, []string{"beta"}, plan.Composition.Budget.Truncated)
	assert.Equal(t, 1, countCandidates(plan, "beta"))
	assert.Equal(t, []string{"alpha", "beta"}, plan.Metadata.RequestedNames)

	beta := findCandidate(plan, "beta")
	require.NotNil(t, beta)
	assert.Equal(t, ParticipantSourceRequested, beta.Source)
	assert.Contains(t, beta.RejectedReason, "max participants")
	assert.Contains(t, beta.Rationale, "not selected")
}

func TestRegistry_PlanOrchestration_MaxConcurrencyRecordsCompositionCap(t *testing.T) {
	t.Parallel()

	registry := NewRegistry(map[string]config.AgentConfig{
		"alpha":   {Triggers: []string{"ship"}},
		"beta":    {Triggers: []string{"ship"}},
		"charlie": {Triggers: []string{"ship"}},
	})

	plan, err := registry.PlanOrchestration(OrchestrationRequest{
		Prompt:         "ship this",
		MaxConcurrency: 2,
	})
	require.NoError(t, err)

	assertParticipantNames(t, plan, []string{"alpha", "beta", "charlie"})
	assert.Equal(t, 2, plan.Composition.MaxConcurrency)
	assert.Equal(t, "no_more_candidates", plan.Composition.StopReason)

	plan, err = registry.PlanOrchestration(OrchestrationRequest{
		Prompt:          "ship this",
		MaxParticipants: 2,
		MaxConcurrency:  99,
	})
	require.NoError(t, err)

	assertParticipantNames(t, plan, []string{"alpha", "beta"})
	assert.Equal(t, 2, plan.Composition.MaxConcurrency)
	assert.Equal(t, "max_participants", plan.Composition.StopReason)
}

func TestRegistry_PlanOrchestration_ToolConstraintsExcludeIneligibleAgents(t *testing.T) {
	t.Parallel()

	registry := NewRegistry(map[string]config.AgentConfig{
		"blocked-runner": {
			Triggers:        []string{"run tests"},
			ToolPermissions: map[string]bool{"bash": false},
		},
		"capable-runner": {
			Capabilities:    []string{"tests"},
			ToolPermissions: map[string]bool{"bash": true},
		},
	})

	plan, err := registry.PlanOrchestration(OrchestrationRequest{Prompt: "run tests"})
	require.NoError(t, err)

	assertParticipantNames(t, plan, []string{"capable-runner"})
	blocked := findCandidate(plan, "blocked-runner")
	require.NotNil(t, blocked)
	assert.False(t, blocked.Eligible)
	assert.Contains(t, blocked.RejectedReason, "bash")
	assert.Equal(t, []string{"bash"}, plan.Composition.RequiredTools)
}

func TestRegistry_PlanOrchestration_NormalizesToolPermissionNames(t *testing.T) {
	t.Parallel()

	registry := NewRegistry(map[string]config.AgentConfig{
		"capable-runner": {
			Triggers:        []string{"run tests"},
			ToolPermissions: map[string]bool{" BASH ": true},
		},
	})

	plan, err := registry.PlanOrchestration(OrchestrationRequest{Prompt: "run tests"})
	require.NoError(t, err)

	assertParticipantNames(t, plan, []string{"capable-runner"})
	assert.Equal(t, []string{"bash"}, plan.Composition.RequiredTools)
	require.Len(t, plan.Participants[0].ToolConstraints, 1)
	assert.True(t, plan.Participants[0].ToolConstraints[0].Allowed)
	assert.Empty(t, plan.Metadata.RejectedNames)
}

func TestRegistry_PlanOrchestration_NormalizesExplicitRequiredTools(t *testing.T) {
	t.Parallel()

	registry := NewRegistry(map[string]config.AgentConfig{
		"capable-runner": {
			Triggers:        []string{"run tests"},
			ToolPermissions: map[string]bool{"bash": true},
		},
	})

	plan, err := registry.PlanOrchestration(OrchestrationRequest{
		Prompt:        "run tests",
		RequiredTools: []string{" Bash ", "bash"},
	})
	require.NoError(t, err)

	assertParticipantNames(t, plan, []string{"capable-runner"})
	assert.Equal(t, []string{"bash"}, plan.Composition.RequiredTools)
	assert.Equal(t, []string{"bash"}, plan.Metadata.RequiredTools)
	require.Len(t, plan.Participants[0].ToolConstraints, 1)
	assert.True(t, plan.Participants[0].ToolConstraints[0].Allowed)
}

func TestRegistry_PlanOrchestration_ExplicitRequiredToolsExcludeIneligibleAgents(t *testing.T) {
	t.Parallel()

	registry := NewRegistry(map[string]config.AgentConfig{
		"blocked-reader": {
			Triggers:        []string{"analyze auth"},
			ToolPermissions: map[string]bool{"read": false},
		},
		"capable-reader": {
			Capabilities:    []string{"analyze auth"},
			ToolPermissions: map[string]bool{"read": true},
		},
	})

	plan, err := registry.PlanOrchestration(OrchestrationRequest{
		Prompt:        "analyze auth",
		RequiredTools: []string{"read"},
	})
	require.NoError(t, err)

	assertParticipantNames(t, plan, []string{"capable-reader"})
	assert.Equal(t, []string{"read"}, plan.Metadata.RequiredTools)
	assert.Equal(t, []string{"read"}, plan.Composition.RequiredTools)

	blocked := findCandidate(plan, "blocked-reader")
	require.NotNil(t, blocked)
	assert.False(t, blocked.Eligible)
	assert.Contains(t, blocked.RejectedReason, "read")
}

func TestRegistry_PlanOrchestration_RequestedAgentOverridesToolConstraintWithRationale(t *testing.T) {
	t.Parallel()

	registry := NewRegistry(map[string]config.AgentConfig{
		"blocked-runner": {
			Triggers:        []string{"run tests"},
			ToolPermissions: map[string]bool{"bash": false},
		},
	})

	plan, err := registry.PlanOrchestration(OrchestrationRequest{
		Prompt:         "run tests",
		RequestedNames: []string{"blocked-runner"},
		RequiredTools:  []string{"bash"},
	})
	require.NoError(t, err)

	assertParticipantNames(t, plan, []string{"blocked-runner"})
	require.Len(t, plan.Participants[0].ToolConstraints, 1)
	assert.False(t, plan.Participants[0].ToolConstraints[0].Allowed)
	assert.Contains(t, plan.Participants[0].Rationale, "explicitly requested")
	assert.Contains(t, plan.Participants[0].Rationale, "tool constraint override")
	assert.Equal(t, []string{"blocked-runner"}, plan.Metadata.SelectedNames)
	assert.Equal(t, []string{"blocked-runner"}, plan.Metadata.ToolOverrideNames)
}

func TestRegistry_PlanOrchestration_ToolConstraintsExplainNoEligibleAgents(t *testing.T) {
	t.Parallel()

	registry := NewRegistry(map[string]config.AgentConfig{
		"blocked-runner": {
			Triggers:        []string{"run tests"},
			ToolPermissions: map[string]bool{"bash": false},
		},
	})

	plan, err := registry.PlanOrchestration(OrchestrationRequest{Prompt: "run tests"})
	require.NoError(t, err)

	assert.Empty(t, plan.Participants)
	assert.Equal(t, "no_eligible_candidates", plan.Composition.StopReason)
	assert.Equal(t, []string{"bash"}, plan.Composition.RequiredTools)

	blocked := findCandidate(plan, "blocked-runner")
	require.NotNil(t, blocked)
	assert.False(t, blocked.Eligible)
	assert.Contains(t, blocked.RejectedReason, "missing required tool permission")
}

func TestRegistry_PlanOrchestration_ToolInferenceRequiresEditForRefactor(t *testing.T) {
	t.Parallel()

	registry := NewRegistry(map[string]config.AgentConfig{
		"blocked-editor": {
			Triggers:        []string{"refactor auth"},
			ToolPermissions: map[string]bool{"edit": false},
		},
		"capable-editor": {
			Capabilities:    []string{"refactor"},
			ToolPermissions: map[string]bool{"edit": true},
		},
	})

	plan, err := registry.PlanOrchestration(OrchestrationRequest{Prompt: "refactor auth"})
	require.NoError(t, err)

	assertParticipantNames(t, plan, []string{"capable-editor"})
	assert.Equal(t, []string{"edit"}, plan.Composition.RequiredTools)
	assert.Equal(t, []string{"edit"}, plan.Metadata.RequiredTools)
	assert.Equal(t, []string{"blocked-editor"}, plan.Metadata.RejectedNames)

	blocked := findCandidate(plan, "blocked-editor")
	require.NotNil(t, blocked)
	assert.False(t, blocked.Eligible)
	assert.Contains(t, blocked.RejectedReason, "edit")
}

func TestRegistry_PlanOrchestration_ToolInferenceUsesSpecificSearchTool(t *testing.T) {
	t.Parallel()

	registry := NewRegistry(map[string]config.AgentConfig{
		"shell-searcher": {
			Triggers:        []string{"grep auth"},
			ToolPermissions: map[string]bool{"bash": true},
		},
		"grep-searcher": {
			Capabilities:    []string{"grep"},
			ToolPermissions: map[string]bool{"grep": true},
		},
	})

	plan, err := registry.PlanOrchestration(OrchestrationRequest{Prompt: "grep auth"})
	require.NoError(t, err)

	assertParticipantNames(t, plan, []string{"grep-searcher"})
	assert.Equal(t, []string{"grep"}, plan.Composition.RequiredTools)
	assert.NotContains(t, plan.Composition.RequiredTools, "bash")

	blocked := findCandidate(plan, "shell-searcher")
	require.NotNil(t, blocked)
	assert.False(t, blocked.Eligible)
	assert.Contains(t, blocked.RejectedReason, "grep")
}

func TestRegistry_PlanOrchestration_ToolInferenceDoesNotTreatChangeAsEdit(t *testing.T) {
	t.Parallel()

	registry := NewRegistry(map[string]config.AgentConfig{
		"reviewer": {
			Triggers:        []string{"review"},
			ToolPermissions: map[string]bool{"read": true},
		},
	})

	plan, err := registry.PlanOrchestration(OrchestrationRequest{Prompt: "review this auth change"})
	require.NoError(t, err)

	assertParticipantNames(t, plan, []string{"reviewer"})
	assert.NotContains(t, plan.Composition.RequiredTools, "edit")
	assert.NotContains(t, plan.Metadata.RequiredTools, "edit")
}

func TestRegistry_PlanOrchestration_ToolInferenceDoesNotTreatRunAsBashByItself(t *testing.T) {
	t.Parallel()

	registry := NewRegistry(map[string]config.AgentConfig{
		"reviewer": {
			Triggers:        []string{"review"},
			ToolPermissions: map[string]bool{"read": true},
		},
	})

	plan, err := registry.PlanOrchestration(OrchestrationRequest{Prompt: "run a review of auth"})
	require.NoError(t, err)

	assertParticipantNames(t, plan, []string{"reviewer"})
	assert.NotContains(t, plan.Composition.RequiredTools, "bash")
	assert.NotContains(t, plan.Metadata.RequiredTools, "bash")
}

func TestRegistry_PlanOrchestration_ToolInferenceDoesNotRequireBashForTestStrategy(t *testing.T) {
	t.Parallel()

	registry := NewRegistry(map[string]config.AgentConfig{
		"test-strategist": {
			Triggers:        []string{"test strategy"},
			ToolPermissions: map[string]bool{"read": true},
		},
	})

	plan, err := registry.PlanOrchestration(OrchestrationRequest{Prompt: "draft a test strategy"})
	require.NoError(t, err)

	assertParticipantNames(t, plan, []string{"test-strategist"})
	assert.NotContains(t, plan.Composition.RequiredTools, "bash")
	assert.NotContains(t, plan.Metadata.RequiredTools, "bash")

	candidate := findCandidate(plan, "test-strategist")
	require.NotNil(t, candidate)
	assert.True(t, candidate.Eligible)
}

func TestRegistry_PlanOrchestration_CodeReviewDoesNotRequestImplementationRole(t *testing.T) {
	t.Parallel()

	registry := NewRegistry(map[string]config.AgentConfig{
		"executor": {Capabilities: []string{"implement"}},
		"reviewer": {Capabilities: []string{"review"}},
	})

	plan, err := registry.PlanOrchestration(OrchestrationRequest{Prompt: "review the code"})
	require.NoError(t, err)

	assertParticipantNames(t, plan, []string{"reviewer"})
	assert.Equal(t, []string{"review"}, plan.Metadata.RequestedRoles)
	assert.Nil(t, findCandidate(plan, "executor"))
}

func TestRegistry_PlanOrchestration_WriteDocsDoesNotRequestResearchRole(t *testing.T) {
	t.Parallel()

	registry := NewRegistry(map[string]config.AgentConfig{
		"researcher": {Capabilities: []string{"research"}},
		"writer":     {Capabilities: []string{"write docs"}},
	})

	plan, err := registry.PlanOrchestration(OrchestrationRequest{Prompt: "write docs"})
	require.NoError(t, err)

	assertParticipantNames(t, plan, []string{"writer"})
	assert.Equal(t, []string{"documentation"}, plan.Metadata.RequestedRoles)
	assert.Nil(t, findCandidate(plan, "researcher"))
}

func TestRegistry_PlanOrchestration_RoleCoverageSelectsWithoutPhraseMatch(t *testing.T) {
	t.Parallel()

	registry := NewRegistry(map[string]config.AgentConfig{
		"critic": {Description: "Code review specialist for maintainability feedback"},
		"writer": {Description: "Documentation specialist"},
	})

	plan, err := registry.PlanOrchestration(OrchestrationRequest{Prompt: "please review this change"})
	require.NoError(t, err)

	assertParticipantNames(t, plan, []string{"critic"})
	assert.Equal(t, ParticipantSourceRole, plan.Participants[0].Source)
	assert.Equal(t, "review", plan.Participants[0].Pattern)
	assert.Contains(t, plan.Participants[0].Rationale, "covers requested role")
	assertPlannedRole(t, plan, "review", "critic")

	candidate := findCandidate(plan, "critic")
	require.NotNil(t, candidate)
	assert.Contains(t, evidenceKinds(candidate.Evidence), ParticipantSourceRole)
}

func TestRegistry_PlanOrchestration_MultiRolePlanRecordsComposition(t *testing.T) {
	t.Parallel()

	registry := NewRegistry(map[string]config.AgentConfig{
		"executor": {Capabilities: []string{"implement"}},
		"reviewer": {Capabilities: []string{"review"}},
		"tester":   {Capabilities: []string{"test"}},
	})

	plan, err := registry.PlanOrchestration(OrchestrationRequest{
		Prompt: "implement the feature, test it, and review the code",
	})
	require.NoError(t, err)

	assertParticipantNameSet(t, plan, []string{"executor", "reviewer", "tester"})
	assertPlannedRole(t, plan, "implementation", "executor")
	assertPlannedRole(t, plan, "verification", "tester")
	assertPlannedRole(t, plan, "review", "reviewer")
	assert.Contains(t, plan.Composition.Dependencies, PlanDependency{
		Before: "executor",
		After:  "tester",
		Reason: "role order: implementation before verification",
	})
	assert.Contains(t, plan.Composition.Dependencies, PlanDependency{
		Before: "tester",
		After:  "reviewer",
		Reason: "role order: verification before review",
	})
}

func TestRegistry_PlanOrchestration_DeduplicatesSelection(t *testing.T) {
	t.Parallel()

	registry := NewRegistry(map[string]config.AgentConfig{
		"reviewer": {Capabilities: []string{"security"}, Triggers: []string{"review"}},
	})

	plan, err := registry.PlanOrchestration(OrchestrationRequest{
		Prompt:         "review security",
		RequestedNames: []string{"reviewer", "reviewer"},
	})
	require.NoError(t, err)

	assertParticipantNames(t, plan, []string{"reviewer"})
	assertParticipantSources(t, plan, []string{ParticipantSourceRequested})
}

func TestRegistry_PlanOrchestration_UnknownRequestedNames(t *testing.T) {
	t.Parallel()

	registry := NewRegistry(map[string]config.AgentConfig{
		"reviewer": {},
		"writer":   {},
	})

	_, err := registry.PlanOrchestration(OrchestrationRequest{RequestedNames: []string{"missing", "writer", "ghost"}})
	require.Error(t, err)

	var unknown UnknownAgentsError
	if !errors.As(err, &unknown) {
		require.Failf(t, "unexpected error", "err = %T %[1]v", err)
	}

	if !reflect.DeepEqual(unknown.Names, []string{"missing", "ghost"}) {
		assert.Failf(t, "assertion failed", "unknown names = %v", unknown.Names)
	}

	message := err.Error()
	for _, want := range []string{"unknown agent(s): missing, ghost", "known agents: reviewer, writer"} {
		if !strings.Contains(message, want) {
			assert.Failf(t, "assertion failed", "error %q does not contain %q", message, want)
		}
	}
}

func TestRegistry_PlanOrchestration_NilRegistry(t *testing.T) {
	t.Parallel()

	var registry *Registry

	plan, err := registry.PlanOrchestration(OrchestrationRequest{Prompt: "review"})
	require.NoError(t, err)

	if len(plan.Participants) != 0 {
		assert.Failf(t, "assertion failed", "participants = %v", plan.Participants)
	}
}

func assertParticipantNames(t *testing.T, plan OrchestrationPlan, want []string) {
	t.Helper()

	agents := plan.Agents()

	got := make([]string, 0, len(agents))
	for i := range agents {
		got = append(got, agents[i].Name)
	}

	if !reflect.DeepEqual(got, want) {
		assert.Failf(t, "assertion failed", "agents = %v, want %v", got, want)
	}
}

func assertParticipantSources(t *testing.T, plan OrchestrationPlan, want []string) {
	t.Helper()

	got := make([]string, 0, len(plan.Participants))
	for i := range plan.Participants {
		got = append(got, plan.Participants[i].Source)
	}

	if !reflect.DeepEqual(got, want) {
		assert.Failf(t, "assertion failed", "sources = %v, want %v", got, want)
	}
}

func assertParticipantNameSet(t *testing.T, plan OrchestrationPlan, want []string) {
	t.Helper()

	got := make([]string, 0, len(plan.Participants))
	for i := range plan.Participants {
		got = append(got, plan.Participants[i].Agent.Name)
	}

	assert.ElementsMatch(t, want, got)
}

func assertPlannedRole(t *testing.T, plan OrchestrationPlan, roleName, agentName string) {
	t.Helper()

	for _, role := range plan.Composition.Roles {
		if role.Name == roleName {
			assert.True(t, role.Covered, "role %q should be covered", roleName)
			assert.True(t, role.Required, "role %q should be required", roleName)
			assert.Equal(t, agentName, role.Agent)

			return
		}
	}

	assert.Failf(t, "missing role", "role %q not found in %#v", roleName, plan.Composition.Roles)
}

func assertPlannedRoleUncovered(t *testing.T, plan OrchestrationPlan, roleName string) {
	t.Helper()

	for _, role := range plan.Composition.Roles {
		if role.Name == roleName {
			assert.False(t, role.Covered, "role %q should be uncovered", roleName)
			assert.True(t, role.Required, "role %q should be required", roleName)
			assert.Empty(t, role.Agent)

			return
		}
	}

	assert.Failf(t, "missing role", "role %q not found in %#v", roleName, plan.Composition.Roles)
}

func findCandidate(plan OrchestrationPlan, name string) *Candidate {
	for i := range plan.Candidates {
		if plan.Candidates[i].Agent.Name == name {
			return &plan.Candidates[i]
		}
	}

	return nil
}

func candidateScore(t *testing.T, plan OrchestrationPlan, name string) float64 {
	t.Helper()

	candidate := findCandidate(plan, name)
	require.NotNil(t, candidate)

	return candidate.Score
}

func countCandidates(plan OrchestrationPlan, name string) int {
	count := 0

	for i := range plan.Candidates {
		if plan.Candidates[i].Agent.Name == name {
			count++
		}
	}

	return count
}

func evidenceKinds(evidence []MatchEvidence) []string {
	kinds := make([]string, 0, len(evidence))
	for _, item := range evidence {
		kinds = append(kinds, item.Kind)
	}

	return kinds
}
