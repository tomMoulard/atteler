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
