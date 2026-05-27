package llm

import (
	"context"
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/tommoulard/atteler/pkg/modelroute"
)

type agentLoopCostProvider struct {
	response *Response
	name     string
	models   []string
	calls    int
}

func (p *agentLoopCostProvider) Name() string { return p.name }

func (p *agentLoopCostProvider) Models() []string { return append([]string(nil), p.models...) }

func (p *agentLoopCostProvider) FetchModels(_ context.Context) ([]string, error) {
	return p.Models(), nil
}

func (p *agentLoopCostProvider) HealthCheck(_ context.Context) error { return nil }

func (p *agentLoopCostProvider) ModelContextWindow(_ string) int { return 128_000 }

func (p *agentLoopCostProvider) Complete(_ context.Context, _ CompleteParams) (*Response, error) {
	if p.response == nil {
		return nil, errors.New("missing response")
	}

	p.calls++

	return p.response, nil
}

func TestRegistry_AgentLoopCostEstimatorRequiresPricingMetadata(t *testing.T) {
	t.Parallel()

	reg := NewRegistry()
	reg.Register(&agentLoopCostProvider{name: "ollama", models: []string{"llama3.2"}})

	_, err := reg.AgentLoopCostEstimator("ollama/llama3.2", nil)
	require.ErrorIs(t, err, ErrAgentLoopCostPricingUnavailable)
	assert.Contains(t, err.Error(), "ollama/llama3.2")
}

func TestRegistry_AgentLoopCostEstimatorRejectsAmbiguousUnqualifiedConfiguredModel(t *testing.T) {
	t.Parallel()

	_, err := NewRegistry().AgentLoopCostEstimator("gpt-5.5", nil)
	require.ErrorIs(t, err, ErrAgentLoopCostPricingUnavailable)
	assert.Contains(t, err.Error(), "gpt-5.5")
}

func TestRegistry_AgentLoopCostEstimatorUsesProviderModelPricing(t *testing.T) {
	t.Parallel()

	reg := NewRegistry()
	reg.Register(&agentLoopCostProvider{name: "openai", models: []string{"gpt-4.1-mini"}})

	estimator, err := reg.AgentLoopCostEstimator("openai/gpt-4.1-mini", nil)
	require.NoError(t, err)

	costMicros, err := estimator(&Response{
		Provider:     "openai",
		Model:        "gpt-4.1-mini",
		InputTokens:  1,
		OutputTokens: 1,
	})
	require.NoError(t, err)
	assert.EqualValues(t, 2, costMicros)
}

func TestRegistry_AgentLoopCostEstimatorUsesDefaultModelPricing(t *testing.T) {
	t.Parallel()

	reg := NewRegistry()
	reg.Register(&agentLoopCostProvider{name: "openai", models: []string{"gpt-4.1-mini"}})

	estimator, err := reg.AgentLoopCostEstimator("", nil)
	require.NoError(t, err)

	costMicros, err := estimator(&Response{
		Provider:     "openai",
		Model:        "gpt-4.1-mini",
		InputTokens:  1,
		OutputTokens: 1,
	})
	require.NoError(t, err)
	assert.EqualValues(t, 2, costMicros)
}

func TestRegistry_AgentLoopCostEstimatorUsesFallbackPricingWhenPrimaryOmitted(t *testing.T) {
	t.Parallel()

	reg := NewRegistry()
	reg.Register(&agentLoopCostProvider{name: "openai", models: []string{"gpt-4.1", "gpt-4.1-mini"}})

	estimator, err := reg.AgentLoopCostEstimator("", []string{"openai/gpt-4.1-mini"})
	require.NoError(t, err)

	costMicros, err := estimator(&Response{
		Provider:     "openai",
		Model:        "gpt-4.1-mini",
		InputTokens:  1,
		OutputTokens: 1,
	})
	require.NoError(t, err)
	assert.EqualValues(t, 2, costMicros)

	_, err = estimator(&Response{
		Provider:     "openai",
		Model:        "gpt-4.1",
		InputTokens:  1,
		OutputTokens: 1,
	})
	require.ErrorIs(t, err, ErrAgentLoopCostPricingUnavailable)
	assert.Contains(t, err.Error(), "openai/gpt-4.1")
}

func TestRegistry_AgentLoopCostEstimatorAllowsSingleConfiguredModelWhenResponseOmitsModel(t *testing.T) {
	t.Parallel()

	reg := NewRegistry()
	reg.Register(&agentLoopCostProvider{name: "openai", models: []string{"gpt-4.1-mini"}})

	estimator, err := reg.AgentLoopCostEstimator("openai/gpt-4.1-mini", nil)
	require.NoError(t, err)

	costMicros, err := estimator(&Response{
		Provider:     "openai",
		InputTokens:  1,
		OutputTokens: 1,
	})
	require.NoError(t, err)
	assert.EqualValues(t, 2, costMicros)
}

func TestRegistry_AgentLoopCostEstimatorDeduplicatesConfiguredAliases(t *testing.T) {
	t.Parallel()

	reg := NewRegistry()
	reg.Register(&agentLoopCostProvider{name: "openai", models: []string{"gpt-4.1-mini", "gpt-4.1-mini-2025-04-14"}})

	estimator, err := reg.AgentLoopCostEstimator("openai/gpt-4.1-mini", []string{"openai/gpt-4.1-mini-2025-04-14"})
	require.NoError(t, err)

	costMicros, err := estimator(&Response{
		Provider:     "openai",
		InputTokens:  1,
		OutputTokens: 1,
	})
	require.NoError(t, err)
	assert.EqualValues(t, 2, costMicros)
}

func TestRegistry_AgentLoopCostEstimatorRequiresFallbackPricingMetadata(t *testing.T) {
	t.Parallel()

	reg := NewRegistry()
	reg.Register(&agentLoopCostProvider{name: "openai", models: []string{"gpt-4.1-mini"}})
	reg.Register(&agentLoopCostProvider{name: "ollama", models: []string{"llama3.2"}})

	_, err := reg.AgentLoopCostEstimator("openai/gpt-4.1-mini", []string{"ollama/llama3.2"})
	require.ErrorIs(t, err, ErrAgentLoopCostPricingUnavailable)
	assert.Contains(t, err.Error(), "ollama/llama3.2")
}

func TestRegistry_AgentLoopCostEstimatorUsesActualResponseModelPricing(t *testing.T) {
	t.Parallel()

	reg := NewRegistry()
	reg.Register(&agentLoopCostProvider{name: "openai", models: []string{"gpt-4.1-mini", "gpt-4.1"}})

	estimator, err := reg.AgentLoopCostEstimator("openai/gpt-4.1-mini", []string{"openai/gpt-4.1"})
	require.NoError(t, err)

	costMicros, err := estimator(&Response{
		Provider:     "openai",
		Model:        "gpt-4.1",
		InputTokens:  1,
		OutputTokens: 1,
	})
	require.NoError(t, err)
	assert.EqualValues(t, 10, costMicros)
}

func TestRegistry_AgentLoopCostEstimatorUsesConfiguredFallbackWhenResponseOmitsProvider(t *testing.T) {
	t.Parallel()

	reg := NewRegistry()
	reg.Register(&agentLoopCostProvider{name: "openai", models: []string{"gpt-4.1-mini", "gpt-4.1"}})

	estimator, err := reg.AgentLoopCostEstimator("openai/gpt-4.1-mini", []string{"openai/gpt-4.1"})
	require.NoError(t, err)

	costMicros, err := estimator(&Response{
		Model:        "gpt-4.1",
		InputTokens:  1,
		OutputTokens: 1,
	})
	require.NoError(t, err)
	assert.EqualValues(t, 10, costMicros)
}

func TestRegistry_AgentLoopCostEstimatorUsesConfiguredQualifiedFallbackWhenResponseOmitsProvider(t *testing.T) {
	t.Parallel()

	reg := NewRegistry()
	reg.Register(&agentLoopCostProvider{name: "openai", models: []string{"gpt-4.1-mini", "gpt-4.1"}})

	estimator, err := reg.AgentLoopCostEstimator("openai/gpt-4.1-mini", []string{"openai/gpt-4.1"})
	require.NoError(t, err)

	costMicros, err := estimator(&Response{
		Model:        "openai/gpt-4.1",
		InputTokens:  1,
		OutputTokens: 1,
	})
	require.NoError(t, err)
	assert.EqualValues(t, 10, costMicros)
}

func TestRegistry_AgentLoopCostEstimatorRejectsUnexpectedUnqualifiedResponseModel(t *testing.T) {
	t.Parallel()

	reg := NewRegistry()
	reg.Register(&agentLoopCostProvider{name: "openai", models: []string{"gpt-4.1-mini", "gpt-4.1"}})

	estimator, err := reg.AgentLoopCostEstimator("openai/gpt-4.1-mini", nil)
	require.NoError(t, err)

	_, err = estimator(&Response{
		Model:        "gpt-4.1",
		InputTokens:  1,
		OutputTokens: 1,
	})
	require.ErrorIs(t, err, ErrAgentLoopCostPricingUnavailable)
	assert.Contains(t, err.Error(), "gpt-4.1")
}

func TestRegistry_AgentLoopCostEstimatorRejectsUnexpectedQualifiedResponseModel(t *testing.T) {
	t.Parallel()

	reg := NewRegistry()
	reg.Register(&agentLoopCostProvider{name: "openai", models: []string{"gpt-4.1-mini", "gpt-4.1"}})

	estimator, err := reg.AgentLoopCostEstimator("openai/gpt-4.1-mini", nil)
	require.NoError(t, err)

	_, err = estimator(&Response{
		Model:        "openai/gpt-4.1",
		InputTokens:  1,
		OutputTokens: 1,
	})
	require.ErrorIs(t, err, ErrAgentLoopCostPricingUnavailable)
	assert.Contains(t, err.Error(), "openai/gpt-4.1")
}

func TestRegistry_AgentLoopCostEstimatorRejectsConflictingProviderQualifiedResponseModel(t *testing.T) {
	t.Parallel()

	reg := NewRegistry()
	reg.Register(&agentLoopCostProvider{name: "openai", models: []string{"gpt-5.5"}})
	reg.Register(&agentLoopCostProvider{name: "codex", models: []string{"gpt-5.5"}})

	estimator, err := reg.AgentLoopCostEstimator("openai/gpt-5.5", nil)
	require.NoError(t, err)

	_, err = estimator(&Response{
		Provider:     "openai",
		Model:        "codex/gpt-5.5",
		InputTokens:  1,
		OutputTokens: 1,
	})
	require.ErrorIs(t, err, ErrAgentLoopCostPricingUnavailable)
	assert.Contains(t, err.Error(), "openai/codex/gpt-5.5")
}

func TestRegistry_AgentLoopCostEstimatorRejectsAmbiguousUnqualifiedResponseModel(t *testing.T) {
	t.Parallel()

	reg := NewRegistry()
	reg.Register(&agentLoopCostProvider{name: "openai", models: []string{"gpt-5.5"}})
	reg.Register(&agentLoopCostProvider{name: "codex", models: []string{"gpt-5.5"}})

	estimator, err := reg.AgentLoopCostEstimator("openai/gpt-5.5", []string{"codex/gpt-5.5"})
	require.NoError(t, err)

	_, err = estimator(&Response{
		Model:        "gpt-5.5",
		InputTokens:  1,
		OutputTokens: 1,
	})
	require.ErrorIs(t, err, ErrAgentLoopCostPricingUnavailable)
	assert.Contains(t, err.Error(), "gpt-5.5")
}

func TestRegistry_AgentLoopCostEstimatorRejectsUnexpectedProviderResponseModel(t *testing.T) {
	t.Parallel()

	reg := NewRegistry()
	reg.Register(&agentLoopCostProvider{name: "openai", models: []string{"gpt-4.1-mini", "gpt-4.1"}})

	estimator, err := reg.AgentLoopCostEstimator("openai/gpt-4.1-mini", nil)
	require.NoError(t, err)

	_, err = estimator(&Response{
		Provider:     "openai",
		Model:        "gpt-4.1",
		InputTokens:  1,
		OutputTokens: 1,
	})
	require.ErrorIs(t, err, ErrAgentLoopCostPricingUnavailable)
	assert.Contains(t, err.Error(), "openai/gpt-4.1")
}

func TestRegistry_AgentLoopCostEstimatorRejectsAmbiguousResponseModel(t *testing.T) {
	t.Parallel()

	reg := NewRegistry()
	reg.Register(&agentLoopCostProvider{name: "openai", models: []string{"gpt-4.1-mini", "gpt-4.1"}})

	estimator, err := reg.AgentLoopCostEstimator("openai/gpt-4.1-mini", []string{"openai/gpt-4.1"})
	require.NoError(t, err)

	_, err = estimator(&Response{
		Provider:     "openai",
		InputTokens:  1,
		OutputTokens: 1,
	})
	require.ErrorIs(t, err, ErrAgentLoopCostPricingUnavailable)
	assert.Contains(t, err.Error(), "openai")
}

func TestRegistry_AgentLoopCostEstimatorRequiresUsageMetadata(t *testing.T) {
	t.Parallel()

	reg := NewRegistry()
	reg.Register(&agentLoopCostProvider{name: "openai", models: []string{"gpt-4.1-mini"}})

	estimator, err := reg.AgentLoopCostEstimator("openai/gpt-4.1-mini", nil)
	require.NoError(t, err)

	_, err = estimator(&Response{
		Provider: "openai",
		Model:    "gpt-4.1-mini",
	})
	require.ErrorIs(t, err, ErrAgentLoopCostUsageUnavailable)
	assert.Contains(t, err.Error(), "openai/gpt-4.1-mini")

	_, err = estimator(&Response{
		Content:      "partial usage",
		Provider:     "openai",
		Model:        "gpt-4.1-mini",
		OutputTokens: 1,
	})
	require.ErrorIs(t, err, ErrAgentLoopCostUsageUnavailable)
	assert.Contains(t, err.Error(), "input token usage unavailable")

	_, err = estimator(&Response{
		Content:     "partial usage",
		Provider:    "openai",
		Model:       "gpt-4.1-mini",
		InputTokens: 1,
	})
	require.ErrorIs(t, err, ErrAgentLoopCostUsageUnavailable)
	assert.Contains(t, err.Error(), "output token usage unavailable")
}

func TestRegistry_AgentLoopCostEstimatorRejectsInconsistentUsageMetadata(t *testing.T) {
	t.Parallel()

	reg := NewRegistry()
	reg.Register(&agentLoopCostProvider{name: "openai", models: []string{"gpt-4.1-mini"}})

	estimator, err := reg.AgentLoopCostEstimator("openai/gpt-4.1-mini", nil)
	require.NoError(t, err)

	_, err = estimator(&Response{
		Provider:              "openai",
		Model:                 "gpt-4.1-mini",
		InputTokens:           1,
		CachedInputTokens:     1,
		CacheWriteInputTokens: 1,
	})
	require.ErrorIs(t, err, ErrAgentLoopCostUsageUnavailable)
	assert.Contains(t, err.Error(), "cache token usage exceeds input tokens")

	_, err = estimator(&Response{
		Provider:     "openai",
		Model:        "gpt-4.1-mini",
		InputTokens:  -1,
		OutputTokens: 1,
	})
	require.ErrorIs(t, err, ErrAgentLoopCostUsageUnavailable)
	assert.Contains(t, err.Error(), "negative token usage")
}

func TestAgentLoopResponseCostMicrosRequiresUsageSpecificPricing(t *testing.T) {
	t.Parallel()

	_, err := agentLoopResponseCostMicros(modelroute.ModelMetadata{
		Provider:       "test",
		Name:           "partial-output",
		InputTokenCost: 0.000001,
	}, &Response{
		InputTokens:  1,
		OutputTokens: 1,
	})
	require.ErrorIs(t, err, ErrAgentLoopCostPricingUnavailable)
	assert.Contains(t, err.Error(), "output token price unavailable")

	_, err = agentLoopResponseCostMicros(modelroute.ModelMetadata{
		Provider:        "test",
		Name:            "partial-cache",
		InputTokenCost:  0.000001,
		OutputTokenCost: 0.000004,
	}, &Response{
		InputTokens:       1,
		CachedInputTokens: 1,
	})
	require.ErrorIs(t, err, ErrAgentLoopCostPricingUnavailable)
	assert.Contains(t, err.Error(), "cached input token price unavailable")
}

func TestAgentLoop_CostBudgetPassesWithCatalogPricing(t *testing.T) {
	t.Parallel()

	provider := &agentLoopCostProvider{
		name:   "openai",
		models: []string{"gpt-4.1-mini"},
		response: &Response{
			Content:      "priced answer",
			Provider:     "openai",
			Model:        "gpt-4.1-mini",
			StopReason:   StopEndTurn,
			InputTokens:  1,
			OutputTokens: 1,
		},
	}
	reg := NewRegistry()
	reg.Register(provider)

	estimator, err := reg.AgentLoopCostEstimator("openai/gpt-4.1-mini", nil)
	require.NoError(t, err)

	resp, _, err := AgentLoop(context.Background(), reg, CompleteParams{Model: "openai/gpt-4.1-mini"}, nil, nil, AgentLoopConfig{
		Budget:             AgentLoopBudget{MaxCostMicros: 10},
		EstimateCostMicros: estimator,
	})
	require.NoError(t, err)
	assert.Equal(t, "priced answer", resp.Content)
	assert.Equal(t, 1, provider.calls)
}

func TestAgentLoop_CostBudgetFailsClosedWithoutUsageMetadata(t *testing.T) {
	t.Parallel()

	provider := &agentLoopCostProvider{
		name:   "openai",
		models: []string{"gpt-4.1-mini"},
		response: &Response{
			Content:    "unmetered answer",
			Provider:   "openai",
			Model:      "gpt-4.1-mini",
			StopReason: StopEndTurn,
		},
	}
	reg := NewRegistry()
	reg.Register(provider)

	estimator, err := reg.AgentLoopCostEstimator("openai/gpt-4.1-mini", nil)
	require.NoError(t, err)

	_, _, err = AgentLoop(context.Background(), reg, CompleteParams{Model: "openai/gpt-4.1-mini"}, nil, nil, AgentLoopConfig{
		Budget:             AgentLoopBudget{MaxCostMicros: 10},
		EstimateCostMicros: estimator,
	})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "cost budget could not be enforced")
	assert.Contains(t, err.Error(), ErrAgentLoopCostUsageUnavailable.Error())
	assert.Equal(t, 1, provider.calls)
}
