package main

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/tommoulard/atteler/pkg/agent"
	"github.com/tommoulard/atteler/pkg/config"
	"github.com/tommoulard/atteler/pkg/llm"
	"github.com/tommoulard/atteler/pkg/modelroute"
	"github.com/tommoulard/atteler/pkg/permission"
	"github.com/tommoulard/atteler/pkg/session"
)

type routeFakeProvider struct {
	verified *bool
	fetch    func(context.Context) ([]string, error)
	name     string
	models   []string
	fetched  []string
}

func (p routeFakeProvider) Name() string { return p.name }

func (p routeFakeProvider) Models() []string { return append([]string(nil), p.models...) }

func (p routeFakeProvider) FetchModels(ctx context.Context) ([]string, error) {
	if p.fetch != nil {
		return p.fetch(ctx)
	}

	if len(p.fetched) > 0 {
		return append([]string(nil), p.fetched...), nil
	}

	return p.Models(), nil
}

func (p routeFakeProvider) ProviderModelsVerified() bool {
	if p.verified == nil {
		return true
	}

	return *p.verified
}

func (p routeFakeProvider) HealthCheck(context.Context) error { return nil }

func (p routeFakeProvider) Complete(context.Context, llm.CompleteParams) (*llm.Response, error) {
	return &llm.Response{}, nil
}

func (p routeFakeProvider) ModelContextWindow(string) int { return 0 }

func TestResolveAgent_InlineOverridesSelected(t *testing.T) {
	t.Parallel()

	registry := agent.NewRegistry(map[string]config.AgentConfig{
		"default":        {SystemPrompt: "default"},
		testReviewerName: {SystemPrompt: "review"},
	})

	selected, prompt, err := resolveAgent(registry, "default", "@reviewer check this")
	if err != nil {
		require.NoError(t, err)
	}

	if selected.name != testReviewerName {
		assert.Failf(t, "assertion failed", "agent = %q, want reviewer", selected.name)
	}

	if prompt != "check this" {
		assert.Failf(t, "assertion failed", "prompt = %q, want check this", prompt)
	}
}

func TestResolveAgent_Unknown(t *testing.T) {
	t.Parallel()

	_, _, err := resolveAgent(agent.NewRegistry(nil), "", "@missing hi")
	if err == nil {
		require.FailNow(t, "expected unknown agent error")
	}
}

func TestResolveAgent_AmbiguousPromptProceedsWithWinner(t *testing.T) {
	t.Parallel()

	registry := agent.NewRegistry(map[string]config.AgentConfig{
		"auth-a": {Triggers: []string{"auth"}},
		"auth-b": {Triggers: []string{"auth"}},
	})

	selected, prompt, err := resolveAgent(registry, "", "review auth permissions")
	require.NoError(t, err)

	assert.Equal(t, "review auth permissions", prompt)
	// The planner picks a deterministic winner rather than erroring; either
	// tied candidate is acceptable so long as one is selected.
	assert.True(t, selected.ok)
	assert.Contains(t, []string{"auth-a", "auth-b"}, selected.name)

	// A non-fatal notice records the ambiguity and the override path.
	assert.NotEmpty(t, selected.notice)
	assert.Contains(t, selected.notice, "ambiguous agent match")
	assert.Contains(t, selected.notice, "override with @agent or --agent")
	assert.Contains(t, selected.notice, selected.name)
	assert.Contains(t, selected.notice, "auth-a")
	assert.Contains(t, selected.notice, "auth-b")
}

func TestResolveAgent_ExplicitOverrideBypassesAmbiguousPrompt(t *testing.T) {
	t.Parallel()

	registry := agent.NewRegistry(map[string]config.AgentConfig{
		"auth-a": {Triggers: []string{"auth"}},
		"auth-b": {Triggers: []string{"auth"}},
	})

	selected, prompt, err := resolveAgent(registry, "", "@auth-b review auth permissions")
	require.NoError(t, err)

	assert.Equal(t, "auth-b", selected.name)
	assert.Equal(t, "review auth permissions", prompt)
}

func TestResolveAgent_RecentSessionContextBreaksAmbiguousTie(t *testing.T) {
	t.Parallel()

	registry := agent.NewRegistry(map[string]config.AgentConfig{
		"alpha-reviewer": {Triggers: []string{"review"}},
		"beta-reviewer":  {Triggers: []string{"review"}},
	})

	selected, prompt, err := resolveAgent(registry, "", "review this", []string{"beta-reviewer"})
	require.NoError(t, err)

	assert.Equal(t, "beta-reviewer", selected.name)
	assert.Equal(t, "review this", prompt)
}

func TestResolveAgent_IneligibleToolMatchRequiresOverride(t *testing.T) {
	t.Parallel()

	registry := agent.NewRegistry(map[string]config.AgentConfig{
		"blocked-runner": {
			Triggers:        []string{"run tests"},
			ToolPermissions: map[string]bool{"bash": false},
		},
	})

	_, prompt, err := resolveAgent(registry, "", "run tests")
	require.Error(t, err)

	assert.Equal(t, "run tests", prompt)
	require.ErrorContains(t, err, "no eligible agent match")
	require.ErrorContains(t, err, "use @agent or --agent to override")
	require.ErrorContains(t, err, "blocked-runner")
	require.ErrorContains(t, err, "missing required tool permission")
}

func TestRequestModelAndFallbacks_AppliesAgentRoutingPolicy(t *testing.T) {
	t.Parallel()

	requestModel, fallbackModels, routeDecision, err := requestModelAndFallbacks("", false, nil, agentSelection{
		ok: true,
		agent: agent.Agent{
			Model:          "openai/gpt-4.1-mini",
			FallbackModels: []string{"anthropic/claude-sonnet-4-20250514", "ollama/llama3.2"},
			RoutingPolicy: modelroute.Policy{
				PreferredProviders: []string{"anthropic"},
				BannedProviders:    []string{"ollama"},
			},
		},
	}, modelroute.RequestProfile{}, nil)

	require.NoError(t, err)
	require.NotNil(t, routeDecision)
	assert.Equal(t, "anthropic/claude-sonnet-4-20250514", requestModel)
	assert.Equal(t, []string{"openai/gpt-4.1-mini"}, fallbackModels)
}

func TestRequestModelAndFallbacks_RejectsUnknownAgentRouteCapability(t *testing.T) {
	t.Parallel()

	requestModel, fallbackModels, routeDecision, err := requestModelAndFallbacks("", false, nil, agentSelection{
		ok: true,
		agent: agent.Agent{
			Model: "openai/gpt-4.1-mini",
			RoutingPolicy: modelroute.Policy{
				RequiredCapabilities: []string{"teleport"},
			},
		},
	}, modelroute.RequestProfile{}, nil)

	require.Error(t, err)
	require.NotNil(t, routeDecision)
	assert.Empty(t, requestModel)
	assert.Nil(t, fallbackModels)
	assert.Contains(t, err.Error(), `unknown capability "teleport"`)
	assert.Equal(t, []string{"teleport"}, routeDecision.Policy.RequiredCapabilities)
	assert.Contains(t, routeDecision.Constraints, modelroute.ConstraintRequiredCapabilities)
	assert.Empty(t, routeDecision.FallbackOrder)
}

func TestRequestModelAndFallbacks_RejectsInvalidAgentRoutePolicyLimit(t *testing.T) {
	t.Parallel()

	requestModel, fallbackModels, routeDecision, err := requestModelAndFallbacks("", false, nil, agentSelection{
		ok: true,
		agent: agent.Agent{
			Model: "openai/gpt-4.1-mini",
			RoutingPolicy: modelroute.Policy{
				MaxBudget: -0.01,
			},
		},
	}, modelroute.RequestProfile{}, nil)

	require.Error(t, err)
	require.NotNil(t, routeDecision)
	assert.Empty(t, requestModel)
	assert.Nil(t, fallbackModels)
	assert.Contains(t, err.Error(), "agent routing_policy.max_budget must be >= 0")
	assert.InDelta(t, -0.01, routeDecision.Policy.MaxBudget, 0.000000001)
	assert.Contains(t, routeDecision.Constraints, modelroute.ConstraintBudget)
	assert.Empty(t, routeDecision.FallbackOrder)
}

func TestRequestModelAndFallbacks_IncludesSelectedModelWhenAgentHasOnlyFallbacks(t *testing.T) {
	t.Parallel()

	requestModel, fallbackModels, routeDecision, err := requestModelAndFallbacks("openai/gpt-4.1-mini", false, nil, agentSelection{
		ok: true,
		agent: agent.Agent{
			FallbackModels: []string{"openai/gpt-4.1-nano"},
			RoutingPolicy: modelroute.Policy{
				BannedModels: []string{"openai/gpt-4.1-nano"},
			},
		},
	}, modelroute.RequestProfile{}, nil)

	require.NoError(t, err)
	require.NotNil(t, routeDecision)
	assert.Equal(t, "openai/gpt-4.1-mini", requestModel)
	assert.Empty(t, fallbackModels)
	assert.Equal(t, []string{"openai/gpt-4.1-mini"}, routeDecision.FallbackOrder)
	assertRejectionContainsCommand(t, *routeDecision, "openai/gpt-4.1-nano", modelroute.ReasonModelBanned)
}

func TestRequestModelRoleAndFallbacks_ResolvesRoleWithDecision(t *testing.T) {
	t.Parallel()

	registry := llm.NewRegistry()
	registry.Register(routeFakeProvider{name: "openai", models: []string{"gpt-4.1", "gpt-4.1-mini"}})
	require.NoError(t, registry.SetModelRole("planner", llm.ModelRole{
		Preferred:            "openai/gpt-4.1",
		FallbackModels:       []string{"openai/gpt-4.1-mini"},
		RequiredCapabilities: []string{modelroute.CapabilityJSONSchema},
		MaxCostUSD:           0.00005,
	}))

	requestModel, fallbackModels, routeDecision, routed, err := requestModelRoleAndFallbacks(
		context.Background(),
		registry,
		"planner",
		nil,
		llm.CompleteParams{
			Messages:  []llm.Message{{Role: llm.RoleUser, Content: "plan this"}},
			MaxTokens: 10,
		},
	)

	require.NoError(t, err)
	require.True(t, routed)
	require.NotNil(t, routeDecision)
	assert.Equal(t, "planner", routeDecision.ModelRole)
	assert.Equal(t, "openai/gpt-4.1-mini", requestModel)
	assert.Empty(t, fallbackModels)
	assert.Equal(t, []string{"openai/gpt-4.1-mini"}, routeDecision.FallbackOrder)
	assert.Contains(t, routeDecision.Constraints, modelroute.ConstraintRequiredCapabilities)
	assert.Contains(t, routeDecision.Constraints, modelroute.ConstraintBudget)
	assert.Contains(t, routeDecision.Constraints, modelroute.ConstraintRuntimeAvailability)
	require.NotNil(t, routeDecision.Availability)
	assert.True(t, routeDecision.Availability.RefreshAttempted)
	assert.Equal(t, int(routeAvailabilityRefreshTimeout/time.Millisecond), routeDecision.Availability.RefreshTimeoutMS)
	assertRejectionContainsCommand(t, *routeDecision, "openai/gpt-4.1", modelroute.ReasonOverBudget)
}

func TestRequestModelRoleAndFallbacks_UsesDefaultModelRole(t *testing.T) {
	t.Parallel()

	registry := llm.NewRegistry()
	registry.Register(routeFakeProvider{name: "openai", models: []string{"gpt-4.1-mini"}})
	require.NoError(t, registry.SetModelRole("planner", llm.ModelRole{
		Preferred: "openai/gpt-4.1-mini",
	}))
	require.NoError(t, registry.SetDefaultModel("planner"))

	requestModel, fallbackModels, routeDecision, routed, err := requestModelRoleAndFallbacks(
		context.Background(),
		registry,
		"",
		nil,
		llm.CompleteParams{
			Messages: []llm.Message{{Role: llm.RoleUser, Content: "plan this"}},
		},
	)

	require.NoError(t, err)
	require.True(t, routed)
	require.NotNil(t, routeDecision)
	assert.Equal(t, "openai/gpt-4.1-mini", requestModel)
	assert.Empty(t, fallbackModels)
	assert.Equal(t, []string{"openai/gpt-4.1-mini"}, routeDecision.FallbackOrder)
}

func TestRequestModelRoutingAndFallbacks_AppliesAgentPolicyToModelRole(t *testing.T) {
	t.Parallel()

	registry := llm.NewRegistry()
	registry.Register(routeFakeProvider{name: "openai", models: []string{"gpt-4.1-mini"}})
	registry.Register(routeFakeProvider{name: "codex", models: []string{"gpt-5.4-mini"}})
	require.NoError(t, registry.SetModelRole("planner", llm.ModelRole{
		Preferred:      "openai/gpt-4.1-mini",
		FallbackModels: []string{"codex/gpt-5.4-mini"},
	}))

	requestModel, fallbackModels, routeDecision, err := requestModelRoutingAndFallbacks(
		context.Background(),
		registry,
		"",
		false,
		nil,
		agentSelection{
			ok: true,
			agent: agent.Agent{
				Model: "planner",
				RoutingPolicy: modelroute.Policy{
					BannedProviders: []string{"openai"},
				},
			},
		},
		"planner",
		nil,
		llm.CompleteParams{
			Model:    "planner",
			Messages: []llm.Message{{Role: llm.RoleUser, Content: "plan this"}},
		},
		modelroute.RequestProfile{},
		routeTelemetryFromRegistry(registry),
		modelroute.Availability{},
	)

	require.NoError(t, err)
	require.NotNil(t, routeDecision)
	assert.Equal(t, "codex/gpt-5.4-mini", requestModel)
	assert.Empty(t, fallbackModels)
	assert.Contains(t, routeDecision.Policy.BannedProviders, "openai")
	assertRejectionContainsCommand(t, *routeDecision, "openai/gpt-4.1-mini", modelroute.ReasonProviderBanned)
}

func TestRequestModelRoleAndFallbacks_RefreshesNestedRoleProviders(t *testing.T) {
	t.Parallel()

	var anthropicFetches atomic.Int32

	registry := llm.NewRegistry()
	registry.Register(routeFakeProvider{name: "openai", models: []string{"gpt-4.1-mini"}})
	registry.Register(routeFakeProvider{
		name: "anthropic",
		fetch: func(context.Context) ([]string, error) {
			anthropicFetches.Add(1)

			return []string{"claude-sonnet-4-20250514"}, nil
		},
	})
	require.NoError(t, registry.SetModelRole("planner", llm.ModelRole{
		Preferred:      "openai/gpt-4.1-mini",
		FallbackModels: []string{"writer"},
	}))
	require.NoError(t, registry.SetModelRole("writer", llm.ModelRole{
		Preferred: "anthropic/claude-sonnet-4-20250514",
	}))

	requestModel, fallbackModels, routeDecision, routed, err := requestModelRoleAndFallbacks(
		context.Background(),
		registry,
		"planner",
		nil,
		llm.CompleteParams{
			Messages: []llm.Message{{Role: llm.RoleUser, Content: "plan this"}},
		},
	)

	require.NoError(t, err)
	require.True(t, routed)
	require.NotNil(t, routeDecision)
	assert.Equal(t, "openai/gpt-4.1-mini", requestModel)
	assert.Equal(t, []string{"anthropic/claude-sonnet-4-20250514"}, fallbackModels)
	assert.Equal(t, int32(1), anthropicFetches.Load())
	require.NotNil(t, routeDecision.Availability)
	assert.True(t, routeDecision.Availability.ProviderModelsVerified["anthropic"])
	assert.Contains(t, routeDecision.Availability.ProviderModels["anthropic"], "claude-sonnet-4-20250514")
	assert.Empty(t, routeDecision.Availability.Unverified)
}

func TestRequestModelRoleAndFallbacks_RefreshesAmbiguousBareRoleProviders(t *testing.T) {
	t.Parallel()

	var anthropicFetches atomic.Int32

	registry := llm.NewRegistry()
	registry.Register(routeFakeProvider{name: "openai", models: []string{"gpt-5.4-mini"}})
	registry.Register(routeFakeProvider{
		name:   "anthropic",
		models: []string{"gpt-5.4-mini"},
		fetch: func(context.Context) ([]string, error) {
			anthropicFetches.Add(1)

			return []string{"gpt-5.4-mini"}, nil
		},
	})
	require.NoError(t, registry.SetModelRole("planner", llm.ModelRole{
		Preferred:          "gpt-5.4-mini",
		PreferredProviders: []string{"anthropic"},
	}))

	requestModel, fallbackModels, routeDecision, routed, err := requestModelRoleAndFallbacks(
		context.Background(),
		registry,
		"planner",
		nil,
		llm.CompleteParams{
			Messages: []llm.Message{{Role: llm.RoleUser, Content: "plan this"}},
		},
	)

	require.NoError(t, err)
	require.True(t, routed)
	require.NotNil(t, routeDecision)
	assert.Equal(t, "anthropic/gpt-5.4-mini", requestModel)
	assert.Empty(t, fallbackModels)
	assert.Equal(t, int32(1), anthropicFetches.Load())
	require.NotNil(t, routeDecision.Availability)
	assert.True(t, routeDecision.Availability.RefreshAttempted)
	assert.True(t, routeDecision.Availability.ProviderModelsVerified["anthropic"])
	assert.Contains(t, routeDecision.Availability.ProviderModels["anthropic"], "gpt-5.4-mini")
}

func TestRequestModelAndFallbacks_PreservesAgentOrderWithoutRoutingPolicy(t *testing.T) {
	t.Parallel()

	requestModel, fallbackModels, routeDecision, err := requestModelAndFallbacks("", false, nil, agentSelection{
		ok: true,
		agent: agent.Agent{
			Model:          "openai/gpt-4.1",
			FallbackModels: []string{"openai/gpt-4.1-mini"},
		},
	}, modelroute.RequestProfile{}, nil)

	require.NoError(t, err)
	assert.Nil(t, routeDecision)
	assert.Equal(t, "openai/gpt-4.1", requestModel)
	assert.Equal(t, []string{"openai/gpt-4.1-mini"}, fallbackModels)
}

func TestRequestMessagesForBudgetIncludesReferenceContext(t *testing.T) {
	t.Parallel()

	const referenceContext = "Configured references:\nlarge design notes"

	messages := requestMessagesForBudget(
		"openai/gpt-4.1-mini",
		[]llm.Message{{Role: llm.RoleUser, Content: "summarize"}},
		agentSelection{},
		generationSettings{},
		referenceContext,
		false,
	)

	require.Len(t, messages, 2)
	assert.Equal(t, llm.RoleSystem, messages[0].Role)
	assert.Equal(t, referenceContext, messages[0].Content)
	assert.Equal(t, llm.RoleUser, messages[1].Role)
	assert.Equal(t, "summarize", messages[1].Content)
}

func TestRequestModelAndFallbacks_UsesTelemetryWithoutExplicitRoutingPolicy(t *testing.T) {
	t.Parallel()

	catalog := modelroute.BuiltinCatalog()
	primary, ok := catalog.Candidate("openai/gpt-4.1-mini")
	require.True(t, ok)

	telemetry := modelroute.NewTelemetry()
	observedAt := time.Now().UTC()
	telemetry.RecordFailure(primary, modelroute.Failure{
		RetryAfter:  time.Hour,
		Error:       "openai: HTTP 429: rate limited",
		Kind:        "transient_rate_limit",
		Retryable:   true,
		RateLimited: true,
	}, observedAt)

	requestModel, fallbackModels, routeDecision, err := requestModelAndFallbacks("", false, nil, agentSelection{
		ok: true,
		agent: agent.Agent{
			Model:          "openai/gpt-4.1-mini",
			FallbackModels: []string{"openai/gpt-4.1-nano", "anthropic/claude-sonnet-4-20250514"},
		},
	}, modelroute.RequestProfile{}, telemetry)

	require.NoError(t, err)
	require.NotNil(t, routeDecision)
	assert.Equal(t, "anthropic/claude-sonnet-4-20250514", requestModel)
	assert.Empty(t, fallbackModels)
	assert.Contains(t, routeDecision.Constraints, modelroute.ConstraintObservedTelemetry)
	assertRejectionContainsCommand(t, *routeDecision, "openai/gpt-4.1-mini", modelroute.ReasonRateLimited)
	assertRejectionContainsCommand(t, *routeDecision, "openai/gpt-4.1-nano", modelroute.ReasonRateLimited)
}

func TestRequestModelAndFallbacks_UsesProviderRateLimitTelemetryForSibling(t *testing.T) {
	t.Parallel()

	catalog := modelroute.BuiltinCatalog()
	observedSibling, ok := catalog.Candidate("openai/gpt-4.1-mini")
	require.True(t, ok)

	telemetry := modelroute.NewTelemetry()
	observedAt := time.Now().UTC()
	telemetry.RecordFailure(observedSibling, modelroute.Failure{
		RetryAfter:  time.Hour,
		Error:       "openai: HTTP 429: rate limited",
		Kind:        "transient_rate_limit",
		Retryable:   true,
		RateLimited: true,
	}, observedAt)

	requestModel, fallbackModels, routeDecision, err := requestModelAndFallbacks("", false, nil, agentSelection{
		ok: true,
		agent: agent.Agent{
			Model:          "openai/gpt-4.1-nano",
			FallbackModels: []string{"anthropic/claude-sonnet-4-20250514"},
		},
	}, modelroute.RequestProfile{}, telemetry)

	require.NoError(t, err)
	require.NotNil(t, routeDecision)
	assert.Equal(t, "anthropic/claude-sonnet-4-20250514", requestModel)
	assert.Empty(t, fallbackModels)
	assert.Contains(t, routeDecision.Constraints, modelroute.ConstraintObservedTelemetry)
	assertRejectionContainsCommand(t, *routeDecision, "openai/gpt-4.1-nano", modelroute.ReasonRateLimited)
}

func TestRequestModelAndFallbacks_UsesPassiveTelemetryWithoutExplicitRoutingPolicy(t *testing.T) {
	t.Parallel()

	catalog := modelroute.BuiltinCatalog()
	fallback, ok := catalog.Candidate("openai/gpt-4.1-mini")
	require.True(t, ok)

	telemetry := modelroute.NewTelemetry()
	telemetry.Record(fallback, modelroute.ActualUsage{
		Latency:     50 * time.Millisecond,
		InputTokens: 10,
	}, time.Now().UTC())

	requestModel, fallbackModels, routeDecision, err := requestModelAndFallbacks("", false, nil, agentSelection{
		ok: true,
		agent: agent.Agent{
			Model:          "openai/gpt-4.1-nano",
			FallbackModels: []string{"openai/gpt-4.1-mini"},
		},
	}, modelroute.RequestProfile{}, telemetry)

	require.NoError(t, err)
	require.NotNil(t, routeDecision)
	assert.Equal(t, "openai/gpt-4.1-mini", requestModel)
	assert.Equal(t, []string{"openai/gpt-4.1-nano"}, fallbackModels)
	assert.Contains(t, routeDecision.Constraints, modelroute.ConstraintObservedTelemetry)
	assert.Equal(t, 1, findCommandCandidateDecision(t, *routeDecision, "openai/gpt-4.1-mini").TelemetryCount)
}

func TestRequestModelAndFallbacks_IgnoresNonRateFailureTelemetryWithoutExplicitRoutingPolicy(t *testing.T) {
	t.Parallel()

	catalog := modelroute.BuiltinCatalog()
	primary, ok := catalog.Candidate("openai/gpt-4.1-mini")
	require.True(t, ok)

	telemetry := modelroute.NewTelemetry()
	telemetry.RecordFailure(primary, modelroute.Failure{
		Error:     "openai: HTTP 500: unavailable",
		Retryable: true,
	}, time.Now().UTC())

	requestModel, fallbackModels, routeDecision, err := requestModelAndFallbacks("", false, nil, agentSelection{
		ok: true,
		agent: agent.Agent{
			Model:          "openai/gpt-4.1-mini",
			FallbackModels: []string{"openai/gpt-4.1-nano"},
		},
	}, modelroute.RequestProfile{}, telemetry)

	require.NoError(t, err)
	assert.Nil(t, routeDecision)
	assert.Equal(t, "openai/gpt-4.1-mini", requestModel)
	assert.Equal(t, []string{"openai/gpt-4.1-nano"}, fallbackModels)
}

func TestRequestModelAndFallbacks_UsesCatalogContextOverflowWithoutExplicitRoutingPolicy(t *testing.T) {
	t.Parallel()

	requestModel, fallbackModels, routeDecision, err := requestModelAndFallbacks("", false, nil, agentSelection{
		ok: true,
		agent: agent.Agent{
			Model:          "openai/gpt-5.4-mini",
			FallbackModels: []string{"openai/gpt-5.4"},
		},
	}, modelroute.RequestProfile{
		EstimatedInputTokens: 500_000,
	}, nil)

	require.NoError(t, err)
	require.NotNil(t, routeDecision)
	assert.Equal(t, "openai/gpt-5.4", requestModel)
	assert.Empty(t, fallbackModels)
	assert.Contains(t, routeDecision.Constraints, modelroute.ConstraintContextWindow)
	assertRejectionContainsCommand(t, *routeDecision, "openai/gpt-5.4-mini", modelroute.ReasonContextOverflow)
}

func TestRequestModelAndFallbacks_UsesCatalogBudgetWithoutExplicitRoutingPolicy(t *testing.T) {
	t.Parallel()

	requestModel, fallbackModels, routeDecision, err := requestModelAndFallbacks("", false, nil, agentSelection{
		ok: true,
		agent: agent.Agent{
			Model:          "openai/gpt-4.1",
			FallbackModels: []string{"openai/gpt-4.1-nano"},
		},
	}, modelroute.RequestProfile{
		EstimatedInputTokens:  1000,
		EstimatedOutputTokens: 100,
		Budget:                0.0002,
	}, nil)

	require.NoError(t, err)
	require.NotNil(t, routeDecision)
	assert.Equal(t, "openai/gpt-4.1-nano", requestModel)
	assert.Empty(t, fallbackModels)
	assert.Contains(t, routeDecision.Constraints, modelroute.ConstraintBudget)
	assertRejectionContainsCommand(t, *routeDecision, "openai/gpt-4.1", modelroute.ReasonOverBudget)
}

func TestRequestModelAndFallbacks_ErrorsWhenCatalogConstraintsRejectAllWithoutPolicy(t *testing.T) {
	t.Parallel()

	_, _, routeDecision, err := requestModelAndFallbacks("", false, nil, agentSelection{
		ok: true,
		agent: agent.Agent{
			Model:          "openai/gpt-5.4",
			FallbackModels: []string{"openai/gpt-4.1"},
		},
	}, modelroute.RequestProfile{
		EstimatedInputTokens:  1000,
		EstimatedOutputTokens: 200_000,
	}, nil)

	require.Error(t, err)
	require.NotNil(t, routeDecision)
	assert.Contains(t, err.Error(), "agent routing rejected all model candidates")
	assertRejectionContainsCommand(t, *routeDecision, "openai/gpt-5.4", modelroute.ReasonContextOverflow)
	assertRejectionContainsCommand(t, *routeDecision, "openai/gpt-4.1", modelroute.ReasonContextOverflow)
}

func TestRequestModelAndFallbacks_UsesAvailabilityWithoutExplicitRoutingPolicy(t *testing.T) {
	t.Parallel()

	registry := llm.NewRegistry()
	registry.Register(routeFakeProvider{name: "openai", models: []string{"gpt-4.1-mini"}})

	activeAgent := agentSelection{
		ok: true,
		agent: agent.Agent{
			Model:          "anthropic/claude-sonnet-4-20250514",
			FallbackModels: []string{"openai/gpt-4.1-mini"},
		},
	}

	requestModel, fallbackModels, routeDecision, err := requestModelAndFallbacks(
		"",
		false,
		nil,
		activeAgent,
		modelroute.RequestProfile{},
		routeTelemetryFromRegistry(registry),
		routeAvailabilityFromRegistry(registry, activeAgent.agent.ModelChain()),
	)

	require.NoError(t, err)
	require.NotNil(t, routeDecision)
	assert.Equal(t, "openai/gpt-4.1-mini", requestModel)
	assert.Empty(t, fallbackModels)
	assert.Contains(t, routeDecision.Constraints, modelroute.ConstraintRuntimeAvailability)
	assertRejectionContainsCommand(t, *routeDecision, "anthropic/claude-sonnet-4-20250514", modelroute.ReasonProviderUnavailable)
}

func TestRequestModelAndFallbacks_RecordsUnverifiedAvailabilityWithoutExplicitRoutingPolicy(t *testing.T) {
	t.Parallel()

	registry := llm.NewRegistry()
	registry.Register(routeFakeProvider{name: "openai", models: []string{"gpt-4.1-mini"}})

	activeAgent := agentSelection{
		ok: true,
		agent: agent.Agent{
			Model:          "openai/gpt-future",
			FallbackModels: []string{"openai/gpt-4.1-mini"},
		},
	}

	requestModel, fallbackModels, routeDecision, err := requestModelAndFallbacks(
		"",
		false,
		nil,
		activeAgent,
		modelroute.RequestProfile{},
		routeTelemetryFromRegistry(registry),
		routeAvailabilityFromRegistry(registry, activeAgent.agent.ModelChain()),
	)

	require.NoError(t, err)
	require.NotNil(t, routeDecision)
	assert.Equal(t, "openai/gpt-4.1-mini", requestModel)
	assert.Empty(t, fallbackModels)
	assert.Contains(t, routeDecision.Constraints, modelroute.ConstraintRuntimeAvailability)
	require.NotNil(t, routeDecision.Availability)
	require.Contains(t, routeDecision.Availability.Unverified, "openai/gpt-future")
	assertRejectionContainsCommand(t, *routeDecision, "openai/gpt-future", modelroute.ReasonUnknownMetadata)
}

func TestRequestModelAndFallbacks_AppliesAgentRoutingBudgetCap(t *testing.T) {
	t.Parallel()

	requestModel, fallbackModels, routeDecision, err := requestModelAndFallbacks("", false, nil, agentSelection{
		ok: true,
		agent: agent.Agent{
			Model:          "openai/gpt-4.1",
			FallbackModels: []string{"openai/gpt-4.1-mini", "openai/gpt-4.1-nano"},
			RoutingPolicy: modelroute.Policy{
				MaxBudget: 0.0002,
			},
		},
	}, modelroute.RequestProfile{
		EstimatedInputTokens:  1000,
		EstimatedOutputTokens: 100,
	}, nil)

	require.NoError(t, err)
	require.NotNil(t, routeDecision)
	assert.Equal(t, "openai/gpt-4.1-nano", requestModel)
	assert.Empty(t, fallbackModels)
}

func TestRequestModelAndFallbacks_ErrorsWhenRoutingPolicyRejectsAllCandidates(t *testing.T) {
	t.Parallel()

	_, _, routeDecision, err := requestModelAndFallbacks("", false, nil, agentSelection{
		ok: true,
		agent: agent.Agent{
			Model:          "openai/gpt-4.1",
			FallbackModels: []string{"openai/gpt-4.1-mini"},
			RoutingPolicy: modelroute.Policy{
				BannedProviders: []string{"openai"},
			},
		},
	}, modelroute.RequestProfile{}, nil)

	require.Error(t, err)
	require.NotNil(t, routeDecision)
	assert.Contains(t, err.Error(), "routing policy rejected all")
	assert.Contains(t, err.Error(), "provider banned")
}

func TestRequestModelAndFallbacks_UsesRouteTelemetry(t *testing.T) {
	t.Parallel()

	catalog := modelroute.BuiltinCatalog()
	openai, ok := catalog.Candidate("openai/gpt-5.5")
	require.True(t, ok)
	codex, ok := catalog.Candidate("codex/gpt-5.5")
	require.True(t, ok)

	telemetry := modelroute.NewTelemetry()
	observedAt := time.Date(2026, time.May, 22, 12, 0, 0, 0, time.UTC)
	telemetry.Record(openai, modelroute.ActualUsage{Latency: 200 * time.Millisecond, TTFT: 100 * time.Millisecond}, observedAt)
	telemetry.Record(codex, modelroute.ActualUsage{Latency: 20 * time.Millisecond, TTFT: 10 * time.Millisecond}, observedAt)

	requestModel, fallbackModels, routeDecision, err := requestModelAndFallbacks("", false, nil, agentSelection{
		ok: true,
		agent: agent.Agent{
			Model:          "openai/gpt-5.5",
			FallbackModels: []string{"codex/gpt-5.5"},
			RoutingPolicy: modelroute.Policy{
				RequiredCapabilities: []string{"text"},
			},
		},
	}, modelroute.RequestProfile{Interactive: true}, telemetry)

	require.NoError(t, err)
	require.NotNil(t, routeDecision)
	assert.Equal(t, "codex/gpt-5.5", requestModel)
	assert.Equal(t, []string{"openai/gpt-5.5"}, fallbackModels)
}

func TestRequestModelAndFallbacks_RejectsUnavailablePreferredProvider(t *testing.T) {
	t.Parallel()

	registry := llm.NewRegistry()
	registry.Register(routeFakeProvider{name: "openai", models: []string{"gpt-4.1-mini"}})

	activeAgent := agentSelection{
		ok: true,
		agent: agent.Agent{
			Model:          "anthropic/claude-sonnet-4-20250514",
			FallbackModels: []string{"openai/gpt-4.1-mini"},
			RoutingPolicy: modelroute.Policy{
				PreferredProviders: []string{"anthropic"},
				RequiredCapabilities: []string{
					"text",
				},
			},
		},
	}

	requestModel, fallbackModels, routeDecision, err := requestModelAndFallbacks(
		"",
		false,
		nil,
		activeAgent,
		modelroute.RequestProfile{},
		routeTelemetryFromRegistry(registry),
		routeAvailabilityFromRegistry(registry, activeAgent.agent.ModelChain()),
	)

	require.NoError(t, err)
	require.NotNil(t, routeDecision)
	assert.Equal(t, "openai/gpt-4.1-mini", requestModel)
	assert.Empty(t, fallbackModels)
	require.NotNil(t, routeDecision.Availability)
	assert.True(t, routeDecision.Availability.Checked)
	assertRejectionContainsCommand(t, *routeDecision, "anthropic/claude-sonnet-4-20250514", modelroute.ReasonProviderUnavailable)
}

func TestRouteAvailabilityFromRegistryMarksProviderQualifiedModelUnverified(t *testing.T) {
	t.Parallel()

	registry := llm.NewRegistry()
	registry.Register(routeFakeProvider{name: "openai", models: []string{"gpt-4.1-mini"}})

	availability := routeAvailabilityFromRegistry(registry, []string{
		"openai/gpt-future",
		"gpt-prefix-future",
		"openai/gpt-4.1-mini",
		"anthropic/claude-sonnet-4-20250514",
	})

	assert.True(t, availability.Checked)
	assert.NotContains(t, availability.Unavailable, "openai/gpt-future")
	require.Contains(t, availability.Unverified, "openai/gpt-future")
	assert.Contains(t, availability.Unverified["openai/gpt-future"], modelroute.ReasonModelUnverified)
	require.Contains(t, availability.Unavailable, "gpt-prefix-future")
	assert.Contains(t, availability.Unavailable["gpt-prefix-future"], modelroute.ReasonModelUnavailable)
	assert.NotContains(t, availability.Unverified, "openai/gpt-4.1-mini")
	assert.Equal(t, []string{"gpt-4.1-mini"}, availability.ProviderModels["openai"])
	assert.False(t, availability.ProviderModelsVerified["openai"])
	assert.Contains(t, availability.Unavailable["anthropic/claude-sonnet-4-20250514"], modelroute.ReasonProviderUnavailable)
}

func TestRouteAvailabilityFromRegistryDoesNotMarkAmbiguousCatalogModelUnavailable(t *testing.T) {
	t.Parallel()

	registry := llm.NewRegistry()
	registry.Register(routeFakeProvider{name: "openai", models: []string{"gpt-5.4-mini"}})
	registry.Register(routeFakeProvider{name: "codex", models: []string{"gpt-5.4-mini"}})

	availability := routeAvailabilityFromRegistry(registry, []string{"gpt-5.4-mini"})

	assert.True(t, availability.Checked)
	assert.NotContains(t, availability.Unavailable, "gpt-5.4-mini")
	assert.NotContains(t, availability.Unverified, "gpt-5.4-mini")
}

func TestRouteAvailabilityFromRegistryAcceptsExactSlashModelID(t *testing.T) {
	t.Parallel()

	registry := llm.NewRegistry()
	registry.Register(routeFakeProvider{name: "openai", models: []string{"namespace/model"}})

	availability := routeAvailabilityFromRegistry(registry, []string{"namespace/model"})

	assert.True(t, availability.Checked)
	assert.NotContains(t, availability.Unavailable, "namespace/model")
	assert.NotContains(t, availability.Unverified, "namespace/model")
	assert.Equal(t, []string{"namespace/model"}, availability.ProviderModels["openai"])
	assert.Contains(t, availability.Models, "namespace/model")
}

func TestRouteAvailabilityFromRegistryDoesNotTreatConfiguredAliasAsProviderQualifiedModel(t *testing.T) {
	t.Parallel()

	registry := llm.NewRegistry()
	registry.Register(routeFakeProvider{name: "openai", models: []string{"gpt-4.1-mini"}})
	require.NoError(t, registry.SetModelAlias("fast", "openai", "gpt-4.1-mini"))

	availability := routeAvailabilityFromRegistry(registry, []string{
		"fast",
		"openai/fast",
	})

	assert.NotContains(t, availability.Unverified, "fast")
	assert.NotContains(t, availability.Unavailable, "fast")
	require.Contains(t, availability.Unverified, "openai/fast")
	assert.Contains(t, availability.Unverified["openai/fast"], modelroute.ReasonModelUnverified)
}

func TestRouteAvailabilityFromRegistryAcceptsConfiguredAliasToPrivateModel(t *testing.T) {
	t.Parallel()

	registry := llm.NewRegistry()
	registry.Register(routeFakeProvider{name: "openai", models: []string{"gpt-4.1-mini"}})
	require.NoError(t, registry.SetModelAlias("fast", "openai", "private-deployment"))

	availability := routeAvailabilityFromRegistry(registry, []string{"fast"})

	assert.NotContains(t, availability.Unverified, "fast")
	assert.NotContains(t, availability.Unavailable, "fast")
	assert.Contains(t, availability.Models, "fast")
	assert.Contains(t, availability.ProviderModels["openai"], "fast")
}

func TestRouteAvailabilityFromRegistryAllowsProviderQualifiedCatalogModelBesideAlias(t *testing.T) {
	t.Parallel()

	registry := llm.NewRegistry()
	registry.Register(routeFakeProvider{name: "openai", models: []string{"fast"}, fetched: []string{"fast"}})
	require.NoError(t, registry.SetModelAlias("fast", "openai", "fast"))
	_, err := registry.ProviderModelCatalog(context.Background(), "openai")
	require.NoError(t, err)

	availability := routeAvailabilityFromRegistry(registry, []string{
		"fast",
		"openai/fast",
	})

	assert.True(t, availability.ProviderModelsVerified["openai"])
	assert.NotContains(t, availability.Unverified, "fast")
	assert.NotContains(t, availability.Unavailable, "fast")
	assert.NotContains(t, availability.Unverified, "openai/fast")
	assert.NotContains(t, availability.Unavailable, "openai/fast")
}

func TestRouteAvailabilityFromRegistryAllowsProviderQualifiedOverrideBesideAlias(t *testing.T) {
	t.Parallel()

	registry := llm.NewRegistry()
	registry.Register(routeFakeProvider{name: "openai", models: []string{"gpt-4.1-mini"}})
	require.NoError(t, registry.SetModelAlias("fast", "openai", "gpt-4.1-mini"))
	require.NoError(t, registry.SetProviderModelOverride("openai", "fast"))

	availability := routeAvailabilityFromRegistry(registry, []string{
		"fast",
		"openai/fast",
	})

	assert.NotContains(t, availability.Unverified, "fast")
	assert.NotContains(t, availability.Unavailable, "fast")
	assert.NotContains(t, availability.Unverified, "openai/fast")
	assert.NotContains(t, availability.Unavailable, "openai/fast")
}

func TestRouteAvailabilityFromRegistryAcceptsProviderQualifiedUserOverride(t *testing.T) {
	t.Parallel()

	registry := llm.NewRegistry()
	registry.Register(routeFakeProvider{name: "openai", models: []string{"gpt-4.1-mini"}})
	require.NoError(t, registry.SetProviderModelOverride("openai", "private-deployment"))

	availability := routeAvailabilityFromRegistry(registry, []string{
		"openai/private-deployment",
	})

	assert.NotContains(t, availability.Unverified, "openai/private-deployment")
	assert.NotContains(t, availability.Unavailable, "openai/private-deployment")
}

func TestRouteAvailabilityFromRegistryMarksMissingVerifiedModelUnavailable(t *testing.T) {
	t.Parallel()

	registry := llm.NewRegistry()
	registry.Register(routeFakeProvider{name: "openai", models: []string{"gpt-4.1-mini"}})
	_, err := registry.ProviderModels(context.Background(), "openai")

	require.NoError(t, err)

	availability := routeAvailabilityFromRegistry(registry, []string{
		"openai/gpt-future",
		"gpt-prefix-future",
	})

	assert.True(t, availability.ProviderModelsVerified["openai"])
	require.Contains(t, availability.Unavailable, "openai/gpt-future")
	assert.Contains(t, availability.Unavailable["openai/gpt-future"], modelroute.ReasonModelUnavailable)
	require.Contains(t, availability.Unavailable, "gpt-prefix-future")
	assert.Contains(t, availability.Unavailable["gpt-prefix-future"], modelroute.ReasonModelUnavailable)
	assert.NotContains(t, availability.Unverified, "openai/gpt-future")
	assert.NotContains(t, availability.Unverified, "gpt-prefix-future")
}

func TestRouteAvailabilityFromRegistryKeepsMissingStaticFetchUnverified(t *testing.T) {
	t.Parallel()

	verified := false
	registry := llm.NewRegistry()
	registry.Register(routeFakeProvider{name: "claude-code", models: []string{"claude-sonnet-4-6"}, verified: &verified})
	_, err := registry.ProviderModels(context.Background(), "claude-code")

	require.NoError(t, err)

	availability := routeAvailabilityFromRegistry(registry, []string{
		"claude-code/claude-future",
	})

	assert.False(t, availability.ProviderModelsVerified["claude-code"])
	assert.NotContains(t, availability.Unavailable, "claude-code/claude-future")
	require.Contains(t, availability.Unverified, "claude-code/claude-future")
	assert.Contains(t, availability.Unverified["claude-code/claude-future"], modelroute.ReasonModelUnverified)
}

func TestRouteAvailabilityFromRegistryUsesProviderSpecificModelIndex(t *testing.T) {
	t.Parallel()

	registry := llm.NewRegistry()
	registry.Register(routeFakeProvider{name: "openai", models: []string{"shared"}})
	registry.Register(routeFakeProvider{name: "anthropic", models: []string{"claude-sonnet-4-20250514"}})

	availability := routeAvailabilityFromRegistry(registry, []string{
		"openai/shared",
		"anthropic/shared",
	})

	assert.NotContains(t, availability.Unverified, "openai/shared")
	require.Contains(t, availability.Unverified, "anthropic/shared")
	assert.Contains(t, availability.Unverified["anthropic/shared"], modelroute.ReasonModelUnverified)
}

func TestRouteAvailabilityFromRegistryTreatsProviderReportedAliasAsIndexed(t *testing.T) {
	t.Parallel()

	registry := llm.NewRegistry()
	registry.Register(routeFakeProvider{name: "openai", models: []string{"gpt-4.1-mini-2025-04-14"}})

	availability := routeAvailabilityFromRegistry(registry, []string{
		"openai/gpt-4.1-mini",
	})

	assert.NotContains(t, availability.Unverified, "openai/gpt-4.1-mini")
	assert.Equal(t, []string{"gpt-4.1-mini-2025-04-14"}, availability.ProviderModels["openai"])
}

func TestRouteAvailabilityFromRegistryWithRefreshUsesLiveProviderModels(t *testing.T) {
	t.Parallel()

	registry := llm.NewRegistry()
	registry.Register(routeFakeProvider{
		name:    "openai",
		models:  []string{"gpt-static"},
		fetched: []string{"gpt-live"},
	})

	availability := routeAvailabilityFromRegistryWithRefresh(context.Background(), registry, []string{
		"openai/gpt-live",
		"openai/gpt-static",
	})

	assert.True(t, availability.ProviderModelsVerified["openai"])
	assert.Equal(t, []string{"gpt-live"}, availability.ProviderModels["openai"])
	assert.NotContains(t, availability.Unavailable, "openai/gpt-live")
	require.Contains(t, availability.Unavailable, "openai/gpt-static")
	assert.Contains(t, availability.Unavailable["openai/gpt-static"], modelroute.ReasonModelUnavailable)
}

func TestRouteAvailabilityFromRegistryWithRefreshBoundsLiveFetch(t *testing.T) {
	t.Parallel()

	deadlines := make(chan time.Time, 1)
	registry := llm.NewRegistry()
	registry.Register(routeFakeProvider{
		name:   "openai",
		models: []string{"gpt-static"},
		fetch: func(ctx context.Context) ([]string, error) {
			deadline, ok := ctx.Deadline()
			require.True(t, ok)

			deadlines <- deadline

			return []string{"gpt-live"}, nil
		},
	})

	availability := routeAvailabilityFromRegistryWithRefresh(context.Background(), registry, []string{"openai/gpt-live"})

	require.True(t, availability.ProviderModelsVerified["openai"])

	select {
	case deadline := <-deadlines:
		remaining := time.Until(deadline)
		assert.Greater(t, remaining, time.Duration(0))
		assert.LessOrEqual(t, remaining, routeAvailabilityRefreshTimeout)
	default:
		t.Fatal("expected provider fetch to receive a deadline-bound context")
	}
}

func TestRouteAvailabilityFromRegistryWithRefreshFallsBackToStaticModelsOnFetchError(t *testing.T) {
	t.Parallel()

	registry := llm.NewRegistry()
	registry.Register(routeFakeProvider{
		name:   "openai",
		models: []string{"gpt-static"},
		fetch: func(context.Context) ([]string, error) {
			return nil, errors.New("models endpoint unavailable")
		},
	})

	availability := routeAvailabilityFromRegistryWithRefresh(context.Background(), registry, []string{
		"openai/gpt-static",
		"openai/gpt-live",
	})

	assert.False(t, availability.ProviderModelsVerified["openai"])
	assert.True(t, availability.RefreshAttempted)
	assert.Equal(t, int(routeAvailabilityRefreshTimeout/time.Millisecond), availability.RefreshTimeoutMS)
	assert.Equal(t, []string{"gpt-static"}, availability.ProviderModels["openai"])
	assert.NotContains(t, availability.Unavailable, "openai/gpt-live")
	require.Contains(t, availability.Unverified, "openai/gpt-live")
	assert.Contains(t, availability.Unverified["openai/gpt-live"], modelroute.ReasonModelUnverified)
}

func TestRouteDecisionEvent_EmbedsInspectableArtifact(t *testing.T) {
	t.Parallel()

	decision := modelroute.DecideFromCatalog(
		modelroute.BuiltinCatalog(),
		[]string{"openai/gpt-4.1-mini", "ollama/llama3.2"},
		modelroute.RequestProfile{EstimatedInputTokens: 10},
		modelroute.Policy{BannedProviders: []string{"ollama"}},
		nil,
		time.Date(2026, time.May, 22, 12, 0, 0, 0, time.UTC),
	)
	decision = modelroute.DecisionWithAvailability(decision, modelroute.Availability{
		Checked:                true,
		ProviderModelsVerified: map[string]bool{"openai": true, "ollama": false},
	})

	event, ok := routeDecisionEvent("session-1", "/tmp/session.json", "reviewer", decision.Selected, &decision)

	require.True(t, ok)
	assert.Equal(t, "route_decision", event.Type)
	assert.Equal(t, decision.Selected, event.Metadata["selected"])
	assert.Equal(t, "openai", event.Metadata["selected_provider"])
	assert.Equal(t, "gpt-4.1-mini", event.Metadata["selected_model"])
	assert.Equal(t, "0", event.Metadata["fallback_count"])
	assert.Equal(t, modelroute.BuiltinCatalogVersion, event.Metadata["catalog_version"])
	assert.Equal(t, "2", event.Metadata["candidate_count"])
	assert.Equal(t, "1", event.Metadata["rejected_count"])
	assert.Contains(t, event.Metadata["constraints"], modelroute.ConstraintCatalogMetadata)
	assert.Contains(t, event.Metadata["constraints"], modelroute.ConstraintRuntimeAvailability)
	assert.Equal(t, "10", event.Metadata["estimated_input_tokens"])
	assert.Equal(t, "1", event.Metadata["verified_provider_model_count"])

	var artifact modelroute.Decision
	require.NoError(t, json.Unmarshal([]byte(event.Content), &artifact))
	assert.Equal(t, decision.Selected, artifact.Selected)
	assertRejectionContainsCommand(t, artifact, "ollama/llama3.2", modelroute.ReasonProviderBanned)
}

func TestRouteDecisionEvent_MetadataIncludesRequestProfileEvidence(t *testing.T) {
	t.Parallel()

	decision := modelroute.Decide(
		[]modelroute.Candidate{{
			Name:                 "primary",
			Provider:             "openai",
			InputTokenCost:       0.000001,
			CachedInputTokenCost: 0.0000005,
			CacheWriteTokenCost:  0.000001,
			OutputTokenCost:      0.000004,
		}},
		modelroute.RequestProfile{
			EstimatedInputTokens:      100,
			EstimatedOutputTokens:     50,
			EstimatedCacheWriteTokens: 20,
			PromptCacheReuseEstimate:  0.5,
			Budget:                    0.25,
			Interactive:               true,
			Batch:                     true,
		},
		modelroute.Policy{},
		nil,
	)

	event, ok := routeDecisionEvent("session-1", "/tmp/session.json", "reviewer", decision.Selected, &decision)

	require.True(t, ok)
	assert.Equal(t, "100", event.Metadata["estimated_input_tokens"])
	assert.Equal(t, "50", event.Metadata["estimated_output_tokens"])
	assert.Equal(t, "20", event.Metadata["estimated_cache_write_tokens"])
	assert.Equal(t, "0.5", event.Metadata["prompt_cache_reuse_estimate"])
	assert.Equal(t, "0.250000", event.Metadata["budget"])
	assert.Equal(t, "true", event.Metadata["interactive"])
	assert.Equal(t, "true", event.Metadata["batch"])
	assert.Equal(t, "0.000275", event.Metadata["estimated_cost"])
}

func TestRouteDecisionEvent_MetadataIncludesAvailabilityEvidence(t *testing.T) {
	t.Parallel()

	decision := modelroute.Decide(
		[]modelroute.Candidate{{Name: "primary", Provider: "openai"}},
		modelroute.RequestProfile{},
		modelroute.Policy{},
		nil,
	)
	decision = modelroute.DecisionWithAvailability(decision, modelroute.Availability{
		Checked:          true,
		RefreshAttempted: true,
		RefreshTimeoutMS: 5000,
		Providers: []string{
			"anthropic",
			"openai",
		},
		Models: []string{
			"claude-sonnet-4-20250514",
			"gpt-4.1-mini",
		},
		ProviderModels: map[string][]string{
			"anthropic": {"claude-sonnet-4-20250514"},
			"openai":    {"gpt-4.1-mini", "gpt-4.1-nano"},
		},
		ProviderModelsVerified: map[string]bool{
			"anthropic": false,
			"openai":    true,
		},
		Unavailable: map[string]string{
			"openai/primary": modelroute.ReasonProviderUnavailable,
		},
		Unverified: map[string]string{
			"openai/gpt-future": modelroute.ReasonModelUnverified,
		},
	})

	event, ok := routeDecisionEvent("session-1", "/tmp/session.json", "reviewer", decision.Selected, &decision)

	require.True(t, ok)
	assert.Equal(t, "true", event.Metadata["availability_checked"])
	assert.Equal(t, "true", event.Metadata["availability_refresh_attempted"])
	assert.Equal(t, "5000", event.Metadata["availability_refresh_timeout_ms"])
	assert.Equal(t, "2", event.Metadata["provider_count"])
	assert.Equal(t, "2", event.Metadata["model_count"])
	assert.Equal(t, "3", event.Metadata["provider_model_count"])
	assert.Equal(t, "1", event.Metadata["unavailable_count"])
	assert.Equal(t, "1", event.Metadata["unverified_count"])
	assert.Equal(t, "1", event.Metadata["verified_provider_model_count"])
}

func TestRouteDecisionEvent_MetadataIncludesWarnings(t *testing.T) {
	t.Parallel()

	decision := modelroute.Decision{
		CatalogStale: true,
		Warnings:     []string{modelroute.ReasonMetadataStale},
		Candidates: []modelroute.CandidateDecision{
			{ID: "openai/gpt-test", Status: modelroute.StatusSelected},
		},
		Selected:      "openai/gpt-test",
		FallbackOrder: []string{"openai/gpt-test"},
	}

	event, ok := routeDecisionEvent("session-1", "/tmp/session.json", "reviewer", decision.Selected, &decision)

	require.True(t, ok)
	assert.Equal(t, "true", event.Metadata["catalog_stale"])
	assert.Equal(t, "1", event.Metadata["warning_count"])
}

func TestRouteDecisionWithResponseAnnotatesActualCost(t *testing.T) {
	t.Parallel()

	decision := modelroute.Decide(
		[]modelroute.Candidate{
			{Name: "gpt-test", Provider: "openai", InputTokenCost: 0.000001, OutputTokenCost: 0.000004},
			{Name: "fallback", Provider: "openai", InputTokenCost: 0.000002, OutputTokenCost: 0.000004},
		},
		modelroute.RequestProfile{EstimatedInputTokens: 100},
		modelroute.Policy{},
		nil,
	)

	annotated := routeDecisionWithResponse(&decision, &llm.Response{
		Latency:           31 * time.Millisecond,
		Model:             "gpt-test",
		FirstTokenLatency: 9 * time.Millisecond,
		InputTokens:       100,
		OutputTokens:      10,
	})
	event, ok := routeDecisionEvent("session-1", "/tmp/session.json", "reviewer", "gpt-test", annotated)

	require.True(t, ok)
	assert.Equal(t, "actual", event.Metadata["phase"])
	assert.Equal(t, "0.000100", event.Metadata["estimated_cost"])
	assert.Equal(t, "0.000140", event.Metadata["actual_cost"])
	assert.Equal(t, "0.000040", event.Metadata["actual_cost_delta"])
	assert.Equal(t, "100", event.Metadata["actual_input_tokens"])
	assert.Equal(t, "10", event.Metadata["actual_output_tokens"])
	assert.Equal(t, "31", event.Metadata["actual_latency_ms"])
	assert.Equal(t, "9", event.Metadata["actual_ttft_ms"])

	var artifact modelroute.Decision
	require.NoError(t, json.Unmarshal([]byte(event.Content), &artifact))
	selected := findCommandCandidateDecision(t, artifact, "openai/gpt-test")
	assert.True(t, selected.ActualUsageRecorded)
	assert.Equal(t, 100, selected.ActualInputTokens)
	assert.Equal(t, 10, selected.ActualOutputTokens)
	assert.InDelta(t, 0.00014, selected.ActualCost, 0.000000001)
	assert.InDelta(t, 0.00004, selected.ActualCostDelta, 0.000000001)
	assert.Equal(t, 31, selected.ObservedLatencyMS)
	assert.Equal(t, 9, selected.ObservedTTFTMS)
}

func TestRouteDecisionWithResponseMetadataIncludesLatencyWithoutTokenUsage(t *testing.T) {
	t.Parallel()

	decision := modelroute.Decide(
		[]modelroute.Candidate{
			{Name: "gpt-test", Provider: "openai", InputTokenCost: 0.000001, OutputTokenCost: 0.000004},
		},
		modelroute.RequestProfile{EstimatedInputTokens: 100},
		modelroute.Policy{},
		nil,
	)

	annotated := routeDecisionWithResponse(&decision, &llm.Response{
		Latency:           42 * time.Millisecond,
		Model:             "gpt-test",
		FirstTokenLatency: 7 * time.Millisecond,
	})
	event, ok := routeDecisionEvent("session-1", "/tmp/session.json", "reviewer", "gpt-test", annotated)

	require.True(t, ok)
	assert.Equal(t, "actual", event.Metadata["phase"])
	assert.Equal(t, "42", event.Metadata["actual_latency_ms"])
	assert.Equal(t, "7", event.Metadata["actual_ttft_ms"])
	assert.NotContains(t, event.Metadata, "actual_cost")
	assert.NotContains(t, event.Metadata, "actual_input_tokens")

	var artifact modelroute.Decision
	require.NoError(t, json.Unmarshal([]byte(event.Content), &artifact))
	selected := findCommandCandidateDecision(t, artifact, "openai/gpt-test")
	assert.False(t, selected.ActualUsageRecorded)
	assert.Equal(t, 42, selected.ObservedLatencyMS)
	assert.Equal(t, 7, selected.ObservedTTFTMS)
}

func TestRouteDecisionWithResponseDoesNotReportStaleTelemetryCostWithoutCurrentUsage(t *testing.T) {
	t.Parallel()

	candidate := modelroute.Candidate{
		Name:            "gpt-test",
		Provider:        "openai",
		InputTokenCost:  0.000001,
		OutputTokenCost: 0.000004,
	}
	decision := modelroute.Decide(
		[]modelroute.Candidate{candidate},
		modelroute.RequestProfile{EstimatedInputTokens: 100},
		modelroute.Policy{},
		nil,
	)
	telemetry := modelroute.NewTelemetry()
	telemetry.Record(candidate, modelroute.ActualUsage{
		InputTokens:  100,
		OutputTokens: 10,
	}, time.Date(2026, time.May, 22, 12, 0, 0, 0, time.UTC))

	annotated := routeDecisionWithResponse(&decision, &llm.Response{
		Latency: 42 * time.Millisecond,
		Model:   "gpt-test",
	}, telemetry)
	event, ok := routeDecisionEvent("session-1", "/tmp/session.json", "reviewer", "gpt-test", annotated)

	require.True(t, ok)
	assert.Equal(t, "actual", event.Metadata["phase"])
	assert.Equal(t, "42", event.Metadata["actual_latency_ms"])
	assert.NotContains(t, event.Metadata, "actual_cost")
	assert.NotContains(t, event.Metadata, "actual_input_tokens")

	var artifact modelroute.Decision
	require.NoError(t, json.Unmarshal([]byte(event.Content), &artifact))
	selected := findCommandCandidateDecision(t, artifact, "openai/gpt-test")
	assert.False(t, selected.ActualUsageRecorded)
	assert.Zero(t, selected.ActualCost)
	assert.Zero(t, selected.ActualInputTokens)
	assert.Equal(t, 42, selected.ObservedLatencyMS)
}

func TestRouteDecisionWithResponseAnnotatesActualFallback(t *testing.T) {
	t.Parallel()

	decision := modelroute.Decide(
		[]modelroute.Candidate{
			{Name: "primary", Provider: "openai", InputTokenCost: 0.000001, OutputTokenCost: 0.000004},
			{Name: "fallback", Provider: "openai", InputTokenCost: 0.000002, OutputTokenCost: 0.000004},
		},
		modelroute.RequestProfile{EstimatedInputTokens: 100},
		modelroute.Policy{},
		nil,
	)

	annotated := routeDecisionWithResponse(&decision, &llm.Response{
		Model:        "fallback",
		InputTokens:  100,
		OutputTokens: 10,
	})
	event, ok := routeDecisionEvent("session-1", "/tmp/session.json", "reviewer", "fallback", annotated)

	require.True(t, ok)
	assert.Equal(t, "actual", event.Metadata["phase"])
	assert.Equal(t, "openai/fallback", event.Metadata["actual_selected"])
	assert.Equal(t, "openai", event.Metadata["actual_provider"])
	assert.Equal(t, "fallback", event.Metadata["actual_model"])
	assert.Equal(t, "openai", event.Metadata["selected_provider"])
	assert.Equal(t, "primary", event.Metadata["selected_model"])
	assert.Equal(t, "1", event.Metadata["fallback_count"])
	assert.Equal(t, "0.000200", event.Metadata["estimated_cost"])
	assert.Equal(t, "0.000240", event.Metadata["actual_cost"])
	assert.Equal(t, "0.000040", event.Metadata["actual_cost_delta"])
	assert.Equal(t, "100", event.Metadata["actual_input_tokens"])
	assert.Equal(t, "10", event.Metadata["actual_output_tokens"])

	var artifact modelroute.Decision
	require.NoError(t, json.Unmarshal([]byte(event.Content), &artifact))
	assert.Equal(t, "openai/primary", artifact.Selected)
	assert.Equal(t, "openai/fallback", artifact.ActualSelected)
	actual := findCommandCandidateDecision(t, artifact, "openai/fallback")
	assert.True(t, actual.ActualUsageRecorded)
	assert.Equal(t, 100, actual.ActualInputTokens)
	assert.Equal(t, 10, actual.ActualOutputTokens)
	assert.InDelta(t, 0.00024, actual.ActualCost, 0.000000001)
}

func TestRouteDecisionWithResponseRefreshesTelemetryForFallbackArtifact(t *testing.T) {
	t.Parallel()

	primary := modelroute.Candidate{Name: "primary", Provider: "openai", InputTokenCost: 0.000001, OutputTokenCost: 0.000004}
	fallback := modelroute.Candidate{Name: "fallback", Provider: "anthropic", InputTokenCost: 0.000002, OutputTokenCost: 0.000004}
	decision := modelroute.Decide(
		[]modelroute.Candidate{primary, fallback},
		modelroute.RequestProfile{EstimatedInputTokens: 100},
		modelroute.Policy{},
		nil,
	)
	telemetry := modelroute.NewTelemetry()
	telemetry.RecordFailure(primary, modelroute.Failure{
		Error:       "openai: HTTP 429: rate limited",
		RetryAfter:  2 * time.Second,
		Retryable:   true,
		RateLimited: true,
	}, time.Date(2026, time.May, 22, 12, 0, 0, 0, time.UTC))

	annotated := routeDecisionWithResponse(&decision, &llm.Response{
		Provider:     "anthropic",
		Model:        "fallback",
		InputTokens:  100,
		OutputTokens: 10,
	}, telemetry)

	require.NotNil(t, annotated)
	assert.Equal(t, "openai/primary", annotated.Selected)
	assert.Equal(t, "anthropic/fallback", annotated.ActualSelected)
	assert.Contains(t, annotated.Constraints, modelroute.ConstraintObservedTelemetry)
	failedPrimary := findCommandCandidateDecision(t, *annotated, "openai/primary")
	assert.Equal(t, 1, failedPrimary.FailureCount)
	assert.Equal(t, 1, failedPrimary.RateLimitCount)
	assert.Contains(t, failedPrimary.LastError, "HTTP 429")
	actual := findCommandCandidateDecision(t, *annotated, "anthropic/fallback")
	assert.True(t, actual.ActualUsageRecorded)
}

func TestRouteDecisionWithResponseUsesProviderForAmbiguousModelNames(t *testing.T) {
	t.Parallel()

	decision := modelroute.Decide(
		[]modelroute.Candidate{
			{Name: "shared", Provider: "openai", InputTokenCost: 0.000001, OutputTokenCost: 0.000004},
			{Name: "shared", Provider: "anthropic", InputTokenCost: 0.000002, OutputTokenCost: 0.000004},
		},
		modelroute.RequestProfile{EstimatedInputTokens: 100},
		modelroute.Policy{},
		nil,
	)

	annotated := routeDecisionWithResponse(&decision, &llm.Response{
		Provider:     "anthropic",
		Model:        "shared",
		InputTokens:  100,
		OutputTokens: 10,
	})

	require.NotNil(t, annotated)
	assert.Equal(t, "openai/shared", annotated.Selected)
	assert.Equal(t, "anthropic/shared", annotated.ActualSelected)
	openAI := findCommandCandidateDecision(t, *annotated, "openai/shared")
	assert.False(t, openAI.ActualUsageRecorded)
	anthropic := findCommandCandidateDecision(t, *annotated, "anthropic/shared")
	assert.True(t, anthropic.ActualUsageRecorded)
}

func TestRouteResponseModelIDQualifiesProviderLocalModel(t *testing.T) {
	t.Parallel()

	assert.Equal(t, "anthropic/shared", routeResponseModelID("anthropic", "shared"))
	assert.Equal(t, "openai/shared", routeResponseModelID("anthropic", "openai/shared"))
	assert.Equal(t, "shared", routeResponseModelID("", "shared"))
}

func TestRouteDecisionWithResponseMarksZeroCostActuals(t *testing.T) {
	t.Parallel()

	decision := modelroute.Decide(
		[]modelroute.Candidate{{Name: "llama3.2", Provider: "ollama"}},
		modelroute.RequestProfile{EstimatedInputTokens: 100},
		modelroute.Policy{},
		nil,
	)

	annotated := routeDecisionWithResponse(&decision, &llm.Response{
		Model:        "llama3.2",
		InputTokens:  100,
		OutputTokens: 10,
	})
	event, ok := routeDecisionEvent("session-1", "/tmp/session.json", "reviewer", "llama3.2", annotated)

	require.True(t, ok)
	assert.Equal(t, "actual", event.Metadata["phase"])
	assert.Equal(t, "0.000000", event.Metadata["estimated_cost"])
	assert.Equal(t, "0.000000", event.Metadata["actual_cost"])
	assert.Equal(t, "0.000000", event.Metadata["actual_cost_delta"])
	assert.Equal(t, "100", event.Metadata["actual_input_tokens"])
	assert.Equal(t, "10", event.Metadata["actual_output_tokens"])

	var artifact modelroute.Decision
	require.NoError(t, json.Unmarshal([]byte(event.Content), &artifact))
	actual := findCommandCandidateDecision(t, artifact, "ollama/llama3.2")
	assert.True(t, actual.ActualUsageRecorded)
	assert.Zero(t, actual.ActualCost)
}

func assertRejectionContainsCommand(t *testing.T, decision modelroute.Decision, id, want string) {
	t.Helper()

	candidate := findCommandCandidateDecision(t, decision, id)
	for _, reason := range candidate.Rejected {
		if strings.Contains(reason, want) {
			return
		}
	}

	require.Failf(t, "rejection reason not found", "candidate %q rejected by %#v, want %q", id, candidate.Rejected, want)
}

func findCommandCandidateDecision(t *testing.T, decision modelroute.Decision, id string) modelroute.CandidateDecision {
	t.Helper()

	for i := range decision.Candidates {
		candidate := decision.Candidates[i]
		if candidate.ID == id {
			return candidate
		}
	}

	require.Failf(t, "candidate decision not found", "id %q in %#v", id, decision.Candidates)

	return modelroute.CandidateDecision{}
}

func TestResolveSelection_ExportSkipsUnknownSavedAgent(t *testing.T) {
	t.Parallel()

	const removedAgent = "removed-agent"

	store := session.NewStore(t.TempDir())
	saved := session.New("gpt-test", nil)

	saved.DefaultAgent = removedAgent
	if err := store.Save(saved); err != nil {
		require.NoError(t, err)
	}

	selection, err := resolveSelection(
		t.Context(),
		cliOptions{exportRef: saved.ID},
		config.Config{},
		"",
		agent.NewRegistry(nil),
		store,
	)
	if err != nil {
		require.NoError(t, err)
	}

	if selection.sessionState.DefaultAgent != removedAgent {
		require.Failf(t, "unexpected failure", "DefaultAgent = %q, want saved agent", selection.sessionState.DefaultAgent)
	}
}

func TestResolveSelection_ShowSkipsUnknownSavedAgent(t *testing.T) {
	t.Parallel()

	const removedAgent = "removed-agent"

	store := session.NewStore(t.TempDir())
	saved := session.New("gpt-test", nil)

	saved.DefaultAgent = removedAgent
	if err := store.Save(saved); err != nil {
		require.NoError(t, err)
	}

	selection, err := resolveSelection(
		t.Context(),
		cliOptions{showSessionRef: saved.ID},
		config.Config{},
		"",
		agent.NewRegistry(nil),
		store,
	)
	if err != nil {
		require.NoError(t, err)
	}

	if selection.sessionState.DefaultAgent != removedAgent {
		require.Failf(t, "unexpected failure", "DefaultAgent = %q, want saved agent", selection.sessionState.DefaultAgent)
	}
}

func TestResolveSelectionPermissionPolicyDeniesSessionRead(t *testing.T) {
	t.Parallel()

	store := session.NewStore(t.TempDir())
	saved := session.New("gpt-test", nil)
	require.NoError(t, store.Save(saved))

	policy := permission.DefaultPolicy()
	policy.SetMode(permission.OperationRead, permission.ModeDeny)

	auditDir := t.TempDir()
	ctx := permission.ContextWithPolicy(t.Context(), &policy)
	ctx = permission.ContextWithAuditDir(ctx, auditDir)

	_, err := resolveSelection(
		ctx,
		cliOptions{showSessionRef: saved.ID},
		config.Config{},
		"",
		agent.NewRegistry(nil),
		store,
	)
	require.Error(t, err)
	require.True(t, permission.ErrDenied(err))
	assert.Contains(t, err.Error(), "permission.read.deny")

	auditData, readErr := os.ReadFile(filepath.Join(auditDir, "side_effects.jsonl"))
	require.NoError(t, readErr)
	assert.Contains(t, string(auditData), "load requested session")
	assert.Contains(t, string(auditData), "permission.read.deny")
}

func TestResolveSelection_SessionUtilitiesSkipUnknownSavedAgent(t *testing.T) {
	t.Parallel()

	const removedAgent = "removed-agent"

	tests := map[string]func(string) cliOptions{
		"summary":            func(id string) cliOptions { return cliOptions{summarySessionRef: id} },
		"list messages":      func(id string) cliOptions { return cliOptions{sessionRef: id, listMessages: true} },
		"list artifacts":     func(id string) cliOptions { return cliOptions{sessionRef: id, listArtifacts: true} },
		"list evaluations":   func(id string) cliOptions { return cliOptions{sessionRef: id, listEvaluations: true} },
		"list failures":      func(id string) cliOptions { return cliOptions{sessionRef: id, listFailures: true} },
		"list runs":          func(id string) cliOptions { return cliOptions{sessionRef: id, listRuns: true} },
		"show run":           func(id string) cliOptions { return cliOptions{sessionRef: id, showRunRef: "latest"} },
		"export run":         func(id string) cliOptions { return cliOptions{sessionRef: id, exportRunRef: "latest"} },
		"replay run":         func(id string) cliOptions { return cliOptions{sessionRef: id, replayRunRef: "latest"} },
		"resume run":         func(id string) cliOptions { return cliOptions{sessionRef: id, resumeRunRef: "latest"} },
		"record failure":     func(id string) cliOptions { return cliOptions{sessionRef: id, recordFailure: "bad path"} },
		"record evaluation":  func(id string) cliOptions { return cliOptions{sessionRef: id, recordEvaluation: "reviewer"} },
		"record artifact":    func(id string) cliOptions { return cliOptions{sessionRef: id, recordArtifact: "artifact.md"} },
		"feedback proposals": func(id string) cliOptions { return cliOptions{sessionRef: id, feedbackProposals: true} },
		"merge artifacts":    func(id string) cliOptions { return cliOptions{sessionRef: id, mergeArtifactsPath: "-"} },
		"agent memory":       func(id string) cliOptions { return cliOptions{sessionRef: id, agentMemorySearch: "auth"} },
		"agent memory delete": func(id string) cliOptions {
			return cliOptions{sessionRef: id, agentMemoryDelete: "memory-id"}
		},
		"agent memory compact": func(id string) cliOptions {
			return cliOptions{sessionRef: id, agentMemoryCompact: true}
		},
		"agent memory migrate": func(id string) cliOptions {
			return cliOptions{sessionRef: id, agentMemoryMigrate: true}
		},
		"bash":      func(id string) cliOptions { return cliOptions{sessionRef: id, bashCommand: "echo ok"} },
		"async run": func(id string) cliOptions { return cliOptions{sessionRef: id, asyncRun: true} },
		"spawn agents": func(id string) cliOptions {
			return cliOptions{sessionRef: id, spawnAgentSpecs: []string{"reviewer|check"}}
		},
		"speculate run": func(id string) cliOptions { return cliOptions{sessionRef: id, speculateRun: true} },
		"review run":    func(id string) cliOptions { return cliOptions{sessionRef: id, reviewRun: true} },
		"list models":   func(id string) cliOptions { return cliOptions{sessionRef: id, listModels: true} },
		"doctor":        func(id string) cliOptions { return cliOptions{sessionRef: id, doctor: true} },
	}

	for name, optsForID := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			store := session.NewStore(t.TempDir())
			saved := session.New("gpt-test", nil)
			saved.DefaultAgent = removedAgent

			err := store.Save(saved)
			require.NoError(t, err)

			selection, err := resolveSelection(
				t.Context(),
				optsForID(saved.ID),
				config.Config{},
				"",
				agent.NewRegistry(nil),
				store,
			)
			require.NoError(t, err)
			assert.Equal(t, removedAgent, selection.sessionState.DefaultAgent)
		})
	}
}

func TestResolveSelection_ExplicitUnknownAgentStillErrorsForSessionUtilities(t *testing.T) {
	t.Parallel()

	store := session.NewStore(t.TempDir())
	saved := session.New("gpt-test", nil)
	err := store.Save(saved)
	require.NoError(t, err)

	_, err = resolveSelection(
		t.Context(),
		cliOptions{sessionRef: saved.ID, listMessages: true, agentName: "missing"},
		config.Config{},
		"",
		agent.NewRegistry(nil),
		store,
	)
	require.Error(t, err)
	assert.Contains(t, err.Error(), `unknown agent "missing"`)
}

func TestResolveSelection_UsesPersistedModelBeforeConfigDefault(t *testing.T) {
	t.Parallel()

	selection, err := resolveSelection(
		t.Context(),
		cliOptions{},
		config.Config{DefaultModel: "config-model"},
		testCodexModel,
		agent.NewRegistry(nil),
		session.NewStore(t.TempDir()),
	)
	if err != nil {
		require.NoError(t, err)
	}

	if selection.selectedModel != testCodexModel {
		require.Failf(t, "unexpected failure", "selectedModel = %q", selection.selectedModel)
	}
}

func TestResolveSelection_LoadedSessionWinsOverPersistedModel(t *testing.T) {
	t.Parallel()
	store := session.NewStore(t.TempDir())

	saved := session.New("session-model", nil)
	if err := store.Save(saved); err != nil {
		require.NoError(t, err)
	}

	selection, err := resolveSelection(
		t.Context(),
		cliOptions{sessionRef: saved.ID},
		config.Config{DefaultModel: "config-model"},
		"persisted-model",
		agent.NewRegistry(nil),
		store,
	)
	if err != nil {
		require.NoError(t, err)
	}

	if selection.selectedModel != "session-model" {
		require.Failf(t, "unexpected failure", "selectedModel = %q", selection.selectedModel)
	}
}
