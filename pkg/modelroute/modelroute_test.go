package modelroute

import (
	"math"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestEstimateCost_UsesOutputAndCachedInput(t *testing.T) {
	t.Parallel()

	candidate := Candidate{
		InputTokenCost:  0.000001,
		OutputTokenCost: 0.000004,
	}
	profile := RequestProfile{
		EstimatedInputTokens:     1000,
		EstimatedOutputTokens:    250,
		PromptCacheReuseEstimate: 0.25,
	}

	got := EstimateCost(candidate, profile)

	want := 0.00175 // 750 billable input tokens + 250 output tokens.
	if got != want {
		t.Fatalf("EstimateCost() = %v, want %v", got, want)
	}
}

func TestKnownCapabilitiesRecognizesNormalizedRouteCapabilities(t *testing.T) {
	t.Parallel()

	assert.ElementsMatch(t, []string{
		CapabilityText,
		CapabilityChat,
		CapabilityTools,
		CapabilityReasoning,
		CapabilityJSONSchema,
		CapabilityEmbeddings,
		CapabilityVision,
		CapabilityMultimodal,
		CapabilityBatch,
		CapabilityPromptCache,
		CapabilityStreaming,
		CapabilityRateLimits,
		CapabilityRetries,
		CapabilityFallback,
		CapabilityCostTracking,
		CapabilityLocal,
		CapabilityFastMode,
	}, KnownCapabilities())
	assert.True(t, IsKnownCapability(" Tools "))
	assert.True(t, IsKnownCapability(CapabilityJSONSchema))
	assert.True(t, IsKnownCapability(CapabilityFastMode))
	assert.False(t, IsKnownCapability(""))
	assert.False(t, IsKnownCapability("teleport"))
}

func TestEstimateCost_ClampsNegativeTokensAndCacheReuse(t *testing.T) {
	t.Parallel()

	candidate := Candidate{
		InputTokenCost:  1,
		OutputTokenCost: 2,
	}

	if got := EstimateCost(candidate, RequestProfile{EstimatedInputTokens: -10, EstimatedOutputTokens: -5}); got != 0 {
		t.Fatalf("EstimateCost() with negative tokens = %v, want 0", got)
	}

	if got := EstimateCost(candidate, RequestProfile{EstimatedInputTokens: 10, PromptCacheReuseEstimate: 2}); got != 0 {
		t.Fatalf("EstimateCost() with >100%% cache reuse = %v, want 0", got)
	}
}

func TestEstimateCost_AppliesCachedInputAndCacheWritePricing(t *testing.T) {
	t.Parallel()

	candidate := Candidate{
		InputTokenCost:       0.000001,
		CachedInputTokenCost: 0.0000001,
		CacheWriteTokenCost:  0.00000125,
		OutputTokenCost:      0.000004,
	}
	profile := RequestProfile{
		EstimatedInputTokens:      1000,
		EstimatedOutputTokens:     100,
		EstimatedCacheWriteTokens: 100,
		PromptCacheReuseEstimate:  0.5,
	}

	got := EstimateCost(candidate, profile)

	assert.InDelta(t, 0.000975, got, 0.000000001)
}

func TestFilter_RemovesOverBudgetAndOverContextCandidates(t *testing.T) {
	t.Parallel()

	candidates := []Candidate{
		{Name: "too-small-context", MaxInputTokens: 1000, InputTokenCost: 0.01, OutputTokenCost: 0.01},
		{Name: "too-small-output", MaxInputTokens: 2000, MaxOutputTokens: 10, InputTokenCost: 0.01, OutputTokenCost: 0.01},
		{Name: "too-expensive", MaxInputTokens: 2000, InputTokenCost: 1, OutputTokenCost: 1},
		{Name: "fits", MaxInputTokens: 2000, InputTokenCost: 0.01, OutputTokenCost: 0.01},
	}
	profile := RequestProfile{EstimatedInputTokens: 1500, EstimatedOutputTokens: 50, Budget: 20}

	got := Filter(candidates, profile)
	if len(got) != 1 || got[0].Name != "fits" {
		t.Fatalf("Filter() = %#v, want only fits", got)
	}
}

func TestDecide_RejectsUnpricedRemoteWhenBudgeted(t *testing.T) {
	t.Parallel()

	decision := Decide(
		[]Candidate{{Name: "live-only", Provider: "openai", Capabilities: []string{capabilityText}}},
		RequestProfile{EstimatedInputTokens: 100, Budget: 0.01},
		Policy{},
		nil,
	)

	assert.Empty(t, decision.FallbackOrder)
	assertRejectionContains(t, decision, "openai/live-only", ReasonCostUnknown)
}

func TestDecide_RejectsPartiallyPricedRemoteWhenBudgeted(t *testing.T) {
	t.Parallel()

	decision := Decide(
		[]Candidate{{Name: "input-only", Provider: "openai", InputTokenCost: 0.000001, Capabilities: []string{capabilityText}}},
		RequestProfile{EstimatedInputTokens: 100, EstimatedOutputTokens: 10, Budget: 0.01},
		Policy{},
		nil,
	)

	assert.Empty(t, decision.FallbackOrder)
	assertRejectionContains(t, decision, "openai/input-only", ReasonCostUnknown)
}

func TestFitsBudget_AllowsLocalZeroCostWhenBudgeted(t *testing.T) {
	t.Parallel()

	candidate := Candidate{
		Name:         "llama3.2",
		Provider:     "ollama",
		Capabilities: []string{capabilityLocal},
	}

	assert.True(t, FitsBudget(candidate, RequestProfile{EstimatedInputTokens: 100, Budget: 0.01}))
}

func TestFitsContext_ReservesEstimatedOutputInContextWindow(t *testing.T) {
	t.Parallel()

	candidate := Candidate{Name: "near-limit", MaxInputTokens: 1000, MaxOutputTokens: 500}
	profile := RequestProfile{EstimatedInputTokens: 800, EstimatedOutputTokens: 300}

	assert.False(t, FitsContext(candidate, profile))

	decision := Decide([]Candidate{candidate}, profile, Policy{}, nil)

	assert.Empty(t, decision.Selected)
	assertRejectionContains(t, decision, "near-limit", "estimated input+output 1100 > context limit 1000")
}

func TestSelectBest_PrefersPriorityThenCostThenInteractiveTTFT(t *testing.T) {
	t.Parallel()

	candidates := []Candidate{
		{Name: "higher-priority-number", Priority: 5, InputTokenCost: 0.01, ExpectedTTFTMS: 10},
		{Name: "lower-cost", Priority: 1, InputTokenCost: 0.02, ExpectedTTFTMS: 50},
		{Name: "best", Priority: 1, InputTokenCost: 0.01, ExpectedTTFTMS: 20},
		{Name: "same-cost-slower", Priority: 1, InputTokenCost: 0.01, ExpectedTTFTMS: 200},
	}
	profile := RequestProfile{EstimatedInputTokens: 100, Interactive: true}

	got, ok := SelectBest(candidates, profile)
	if !ok {
		t.Fatal("SelectBest() ok = false, want true")
	}

	if got.Name != "best" {
		t.Fatalf("SelectBest().Name = %q, want %q", got.Name, "best")
	}
}

func TestFallbackChain_IsStableForEqualCandidates(t *testing.T) {
	t.Parallel()

	candidates := []Candidate{
		{Name: "first", Provider: "provider", Priority: 1, InputTokenCost: 0.01, ExpectedLatencyMS: 100},
		{Name: "second", Provider: "provider", Priority: 1, InputTokenCost: 0.01, ExpectedLatencyMS: 100},
		{Name: "third", Provider: "provider", Priority: 1, InputTokenCost: 0.01, ExpectedLatencyMS: 100},
	}

	got := FallbackChain(candidates, RequestProfile{EstimatedInputTokens: 100})
	if len(got) != 3 {
		t.Fatalf("FallbackChain() len = %d, want 3", len(got))
	}

	for i, want := range []string{"first", "second", "third"} {
		if got[i].Name != want {
			t.Fatalf("FallbackChain()[%d].Name = %q, want %q", i, got[i].Name, want)
		}
	}
}

func TestFallbackChain_BatchSkipsLatencyTieBreaker(t *testing.T) {
	t.Parallel()

	candidates := []Candidate{
		{Name: "first-slower", Priority: 1, InputTokenCost: 0.01, ExpectedLatencyMS: 500},
		{Name: "second-faster", Priority: 1, InputTokenCost: 0.01, ExpectedLatencyMS: 50},
	}

	got := FallbackChain(candidates, RequestProfile{EstimatedInputTokens: 100, Batch: true})
	if got[0].Name != "first-slower" {
		t.Fatalf("FallbackChain()[0].Name = %q, want stable first candidate for batch", got[0].Name)
	}
}

func TestID(t *testing.T) {
	t.Parallel()

	if got := (Candidate{Provider: "openai", Name: "fast"}).ID(); got != "openai/fast" {
		t.Fatalf("ID() = %q, want openai/fast", got)
	}

	if got := (Candidate{Name: "fast"}).ID(); got != "fast" {
		t.Fatalf("ID() = %q, want fast", got)
	}

	if got := (Candidate{Provider: "openai"}).ID(); got != "openai" {
		t.Fatalf("ID() = %q, want openai", got)
	}
}

func TestBuiltinCatalog_ProvidesVersionedMetadata(t *testing.T) {
	t.Parallel()

	catalog := BuiltinCatalog()
	metadata, ok := catalog.Lookup("openai", "gpt-4.1-mini")

	require.True(t, ok)
	assert.Equal(t, BuiltinCatalogVersion, catalog.Version)
	assert.Equal(t, "openai/gpt-4.1-mini", metadata.ID())
	assert.Positive(t, metadata.ContextWindow)
	assert.Positive(t, metadata.MaxOutputTokens)
	assert.Greater(t, metadata.InputTokenCost, 0.0)
	assert.Greater(t, metadata.CachedInputTokenCost, 0.0)
	assert.Greater(t, metadata.OutputTokenCost, 0.0)
	assert.Contains(t, metadata.Capabilities, capabilityChat)
	assert.Contains(t, metadata.Capabilities, capabilityJSONSchema)
	assert.Contains(t, metadata.Capabilities, capabilityPromptCache)
	assert.Contains(t, metadata.Capabilities, capabilityFastMode)
	assert.Contains(t, metadata.Capabilities, capabilityStreaming)
	assert.Contains(t, metadata.Capabilities, capabilityCost)
	assert.Equal(t, "https://developers.openai.com/api/docs/pricing", metadata.SourceURL)

	fastMetadata, ok := catalog.Lookup("openai", "gpt-5.5")
	require.True(t, ok)
	assert.Contains(t, fastMetadata.Capabilities, capabilityFastMode)

	candidate := metadata.Candidate(0)
	assert.Equal(t, BuiltinCatalogVersion, candidate.MetadataVersion)
	assert.Equal(t, metadata.ContextWindow, candidate.MaxInputTokens)
	assert.Equal(t, metadata.MaxOutputTokens, candidate.MaxOutputTokens)
	assert.Equal(t, metadata.Source, candidate.MetadataSource)
	assert.Equal(t, metadata.SourceURL, candidate.MetadataSourceURL)
	assert.Equal(t, metadata.SourcePublished, candidate.MetadataPublished)
	assert.Equal(t, metadata.Deprecated, candidate.Deprecated)
}

func TestBuiltinCatalog_RecordsOfficialPricingSources(t *testing.T) {
	t.Parallel()

	catalog := BuiltinCatalog()

	codex, ok := catalog.Lookup("codex", "gpt-5.3-codex")
	require.True(t, ok)
	assert.Equal(t, "https://developers.openai.com/api/docs/pricing", codex.SourceURL)

	anthropic, ok := catalog.Lookup("anthropic", "claude-opus-4-7")
	require.True(t, ok)
	assert.Equal(t, "https://platform.claude.com/docs/en/about-claude/pricing", anthropic.SourceURL)
}

func TestBuiltinCatalog_IncludesEmbeddingMetadata(t *testing.T) {
	t.Parallel()

	catalog := BuiltinCatalog()

	small, ok := catalog.Lookup("openai", "text-embedding-3-small")
	require.True(t, ok)
	assert.Equal(t, 8192, small.ContextWindow)
	assert.Zero(t, small.MaxOutputTokens)
	assert.InDelta(t, 0.02/1_000_000, small.InputTokenCost, 0.000000000001)
	assert.Zero(t, small.OutputTokenCost)
	assert.Contains(t, small.Capabilities, capabilityEmbeddings)
	assert.Contains(t, small.Capabilities, capabilityBatch)
	assert.Contains(t, small.Capabilities, capabilityCost)
	assert.NotContains(t, small.Capabilities, capabilityChat)
	assert.Equal(t, "https://developers.openai.com/api/docs/models/text-embedding-3-small", small.SourceURL)

	large, ok := catalog.Lookup("openai", "text-embedding-3-large")
	require.True(t, ok)
	assert.InDelta(t, 0.13/1_000_000, large.InputTokenCost, 0.000000000001)

	local, ok := catalog.Lookup("ollama", "nomic-embed-text")
	require.True(t, ok)
	assert.Contains(t, local.Capabilities, capabilityEmbeddings)
	assert.Contains(t, local.Capabilities, capabilityLocal)
	assert.Zero(t, local.InputTokenCost)
}

func TestBuiltinCatalog_IncludesCurrentProviderLimits(t *testing.T) {
	t.Parallel()

	catalog := BuiltinCatalog()

	gptMini, ok := catalog.Lookup("openai", "gpt-5.4-mini")
	require.True(t, ok)
	assert.Equal(t, 400_000, gptMini.ContextWindow)
	assert.Equal(t, 128_000, gptMini.MaxOutputTokens)
	assert.InDelta(t, 0.75/1_000_000, gptMini.InputTokenCost, 0.000000000001)
	assert.InDelta(t, 0.075/1_000_000, gptMini.CachedInputTokenCost, 0.000000000001)
	assert.InDelta(t, 4.5/1_000_000, gptMini.OutputTokenCost, 0.000000000001)
	assert.Contains(t, gptMini.Capabilities, capabilityVision)

	opus, ok := catalog.Lookup("anthropic", "claude-opus-4-7")
	require.True(t, ok)
	assert.Equal(t, 1_000_000, opus.ContextWindow)
	assert.Equal(t, 128_000, opus.MaxOutputTokens)
	assert.InDelta(t, 5.0/1_000_000, opus.InputTokenCost, 0.000000000001)
	assert.InDelta(t, 0.5/1_000_000, opus.CachedInputTokenCost, 0.000000000001)
	assert.InDelta(t, 6.25/1_000_000, opus.CacheWriteTokenCost, 0.000000000001)
	assert.InDelta(t, 25.0/1_000_000, opus.OutputTokenCost, 0.000000000001)
	assert.Contains(t, opus.Capabilities, capabilityReasoning)
	assert.Contains(t, opus.Capabilities, capabilityVision)

	oldSonnet, ok := catalog.Lookup("anthropic", "claude-sonnet-4-20250514")
	require.True(t, ok)
	assert.True(t, oldSonnet.Deprecated)
	assert.True(t, oldSonnet.Candidate(0).Deprecated)
}

func TestBuiltinCatalog_MapsProviderReportedAliases(t *testing.T) {
	t.Parallel()

	catalog := BuiltinCatalog()
	metadata, ok := catalog.Lookup("openai", "gpt-4.1-mini-2025-04-14")

	require.True(t, ok)
	assert.Equal(t, "gpt-4.1-mini", metadata.Name)
	assert.Equal(t, "openai/gpt-4.1-mini", metadata.ID())

	candidate, ok := catalog.Candidate("openai/gpt-4.1-mini-2025-04-14")
	require.True(t, ok)
	assert.Equal(t, "openai/gpt-4.1-mini", candidate.ID())
	assert.Contains(t, candidate.Aliases, "gpt-4.1-mini-2025-04-14")
}

func TestBuiltinCatalog_RejectsAmbiguousProviderLocalModels(t *testing.T) {
	t.Parallel()

	catalog := BuiltinCatalog()

	_, ok := catalog.Candidate("gpt-5.5")
	assert.False(t, ok)

	candidate, ok := catalog.Candidate("codex/gpt-5.5")
	require.True(t, ok)
	assert.Equal(t, "codex/gpt-5.5", candidate.ID())

	decision := DecideFromCatalog(
		catalog,
		[]string{"gpt-5.5", "codex/gpt-5.5"},
		RequestProfile{EstimatedInputTokens: 10},
		Policy{},
		nil,
		time.Date(2026, time.May, 22, 0, 0, 0, 0, time.UTC),
	)

	assert.Equal(t, "codex/gpt-5.5", decision.Selected)
	ambiguous := findCandidateDecision(t, decision, "gpt-5.5")
	assert.Equal(t, StatusRejected, ambiguous.Status)
	assert.Contains(t, ambiguous.Rejected, ReasonAmbiguousMetadata)
}

func TestDecideFromCatalog_ReportsStaleMetadataAndUnknownModels(t *testing.T) {
	t.Parallel()

	catalog := BuiltinCatalog()
	catalog.StaleAfter = time.Date(2026, time.January, 1, 0, 0, 0, 0, time.UTC)

	decision := DecideFromCatalog(
		catalog,
		[]string{"openai/gpt-4.1-mini", "openai/not-real"},
		RequestProfile{EstimatedInputTokens: 10, EstimatedOutputTokens: 1},
		Policy{},
		nil,
		time.Date(2026, time.May, 22, 0, 0, 0, 0, time.UTC),
	)

	assert.True(t, decision.CatalogStale)
	assert.Contains(t, decision.Constraints, ConstraintCatalogMetadata)
	assert.Contains(t, decision.Constraints, ConstraintMetadataFreshness)
	require.NotEmpty(t, decision.Warnings)
	assert.Contains(t, decision.Warnings[0], ReasonMetadataStale)
	unknown := findCandidateDecision(t, decision, "openai/not-real")
	assert.Equal(t, StatusRejected, unknown.Status)
	assert.Contains(t, unknown.Rejected, ReasonUnknownMetadata)
}

func TestDecideFromCatalog_DeduplicatesCanonicalAliases(t *testing.T) {
	t.Parallel()

	decision := DecideFromCatalog(
		BuiltinCatalog(),
		[]string{
			"openai/gpt-4.1-mini-2025-04-14",
			"openai/gpt-4.1-mini",
			"openai/not-real",
			"openai/not-real",
		},
		RequestProfile{EstimatedInputTokens: 10, EstimatedOutputTokens: 1},
		Policy{},
		nil,
		time.Date(2026, time.May, 22, 0, 0, 0, 0, time.UTC),
	)

	assert.Equal(t, "openai/gpt-4.1-mini", decision.Selected)
	assert.Equal(t, []string{"openai/gpt-4.1-mini"}, decision.FallbackOrder)
	assert.Len(t, decision.Candidates, 2)

	candidate := findCandidateDecision(t, decision, "openai/gpt-4.1-mini")
	assert.Equal(t, StatusSelected, candidate.Status)

	unknown := findCandidateDecision(t, decision, "openai/not-real")
	assert.Equal(t, StatusRejected, unknown.Status)
	assert.Contains(t, unknown.Rejected, ReasonUnknownMetadata)
}

func TestDecideFromCatalog_RequireFreshMetadataRejectsStaleCatalog(t *testing.T) {
	t.Parallel()

	catalog := BuiltinCatalog()
	catalog.StaleAfter = time.Date(2026, time.January, 1, 0, 0, 0, 0, time.UTC)

	decision := DecideFromCatalog(
		catalog,
		[]string{"openai/gpt-4.1-mini", "openai/gpt-4.1-nano"},
		RequestProfile{EstimatedInputTokens: 10, EstimatedOutputTokens: 1},
		Policy{RequireFreshMetadata: true},
		nil,
		time.Date(2026, time.May, 22, 0, 0, 0, 0, time.UTC),
	)

	assert.True(t, decision.CatalogStale)
	assert.Empty(t, decision.Selected)
	assert.Empty(t, decision.FallbackOrder)
	assertRejectionContains(t, decision, "openai/gpt-4.1-mini", ReasonMetadataStale)
	assertRejectionContains(t, decision, "openai/gpt-4.1-nano", ReasonMetadataStale)
}

func TestDecide_AppliesPolicyBudgetContextAndFallbackOrder(t *testing.T) {
	t.Parallel()

	candidates := []Candidate{
		{Name: "too-small", Provider: "openai", MaxInputTokens: 100, InputTokenCost: 0.000001, OutputTokenCost: 0.000001, Capabilities: []string{capabilityText}},
		{Name: "too-small-output", Provider: "openai", MaxInputTokens: 10_000, MaxOutputTokens: 10, InputTokenCost: 0.000001, OutputTokenCost: 0.000001, Capabilities: []string{capabilityText, capabilityTools}},
		{Name: "too-expensive", Provider: "openai", MaxInputTokens: 10_000, InputTokenCost: 0.01, OutputTokenCost: 0.01, Capabilities: []string{capabilityText, capabilityTools}},
		{Name: "banned", Provider: "anthropic", MaxInputTokens: 10_000, InputTokenCost: 0.000001, OutputTokenCost: 0.000001, Capabilities: []string{capabilityText, capabilityTools}},
		{Name: "selected", Provider: "openai", MaxInputTokens: 10_000, InputTokenCost: 0.000001, OutputTokenCost: 0.000001, Priority: 1, Capabilities: []string{capabilityText, capabilityTools}},
		{Name: "fallback", Provider: "openai", MaxInputTokens: 10_000, InputTokenCost: 0.000002, OutputTokenCost: 0.000002, Priority: 2, Capabilities: []string{capabilityText, capabilityTools}},
	}

	decision := Decide(candidates, RequestProfile{EstimatedInputTokens: 500, EstimatedOutputTokens: 50}, Policy{
		BannedProviders:      []string{"anthropic"},
		RequiredCapabilities: []string{capabilityTools},
		MaxBudget:            0.002,
	}, nil)

	assert.InDelta(t, 0.002, decision.Profile.Budget, 0.000000001)
	assert.Contains(t, decision.Constraints, ConstraintContextWindow)
	assert.Contains(t, decision.Constraints, ConstraintOutputLimit)
	assert.Contains(t, decision.Constraints, ConstraintEstimatedCost)
	assert.Contains(t, decision.Constraints, ConstraintBudget)
	assert.Contains(t, decision.Constraints, ConstraintRoutingPolicy)
	assert.Contains(t, decision.Constraints, ConstraintRequiredCapabilities)
	assert.Equal(t, "openai/selected", decision.Selected)
	assert.Equal(t, []string{"openai/selected", "openai/fallback"}, decision.FallbackOrder)
	assertRejectionContains(t, decision, "openai/too-small", ReasonContextOverflow)
	assertRejectionContains(t, decision, "openai/too-small", ReasonMissingCapability)
	assertRejectionContains(t, decision, "openai/too-small-output", ReasonContextOverflow)
	assertRejectionContains(t, decision, "openai/too-expensive", ReasonOverBudget)
	assertRejectionContains(t, decision, "anthropic/banned", ReasonProviderBanned)
}

func TestDecide_NormalizesNegativeProfileAndPolicyLimits(t *testing.T) {
	t.Parallel()

	decision := Decide(
		[]Candidate{{Name: "selected", Provider: "openai", InputTokenCost: 0.000001, OutputTokenCost: 0.000001}},
		RequestProfile{
			EstimatedInputTokens:      -10,
			EstimatedOutputTokens:     -5,
			EstimatedCacheWriteTokens: -3,
			Budget:                    -1,
			PromptCacheReuseEstimate:  2,
		},
		Policy{
			MaxBudget:    -0.01,
			MaxLatencyMS: -1,
			MaxTTFTMS:    -1,
		},
		nil,
	)

	assert.Equal(t, "openai/selected", decision.Selected)
	assert.Zero(t, decision.Profile.EstimatedInputTokens)
	assert.Zero(t, decision.Profile.EstimatedOutputTokens)
	assert.Zero(t, decision.Profile.EstimatedCacheWriteTokens)
	assert.Zero(t, decision.Profile.Budget)
	assert.InDelta(t, 1.0, decision.Profile.PromptCacheReuseEstimate, 0.000000001)
	assert.Zero(t, decision.Policy.MaxBudget)
	assert.Zero(t, decision.Policy.MaxLatencyMS)
	assert.Zero(t, decision.Policy.MaxTTFTMS)
	assert.NotContains(t, decision.Constraints, ConstraintBudget)
	assert.NotContains(t, decision.Constraints, ConstraintLatency)
	assert.NotContains(t, decision.Constraints, ConstraintTTFT)
}

func TestDecide_NormalizesNonFiniteProfileAndPolicyLimits(t *testing.T) {
	t.Parallel()

	decision := Decide(
		[]Candidate{{Name: "selected", Provider: "openai", InputTokenCost: 0.000001}},
		RequestProfile{
			EstimatedInputTokens:     100,
			Budget:                   math.Inf(1),
			PromptCacheReuseEstimate: math.NaN(),
		},
		Policy{MaxBudget: math.NaN()},
		nil,
	)

	assert.Equal(t, "openai/selected", decision.Selected)
	assert.Zero(t, decision.Profile.Budget)
	assert.Zero(t, decision.Profile.PromptCacheReuseEstimate)
	assert.Zero(t, decision.Policy.MaxBudget)
	assert.NotContains(t, decision.Constraints, ConstraintBudget)
}

func TestDecide_UsesNormalizedPolicyForConstraintEvidence(t *testing.T) {
	t.Parallel()

	decision := Decide(
		[]Candidate{{Name: "selected", Provider: "openai", InputTokenCost: 0.000001}},
		RequestProfile{EstimatedInputTokens: 100},
		Policy{
			PreferredProviders:   []string{" "},
			BannedProviders:      []string{" "},
			BannedModels:         []string{" "},
			RequiredCapabilities: []string{" "},
			MaxBudget:            -0.01,
			MaxLatencyMS:         -1,
			MaxTTFTMS:            -1,
		},
		nil,
	)

	assert.Equal(t, "openai/selected", decision.Selected)
	assert.Empty(t, decision.Policy.PreferredProviders)
	assert.Empty(t, decision.Policy.BannedProviders)
	assert.Empty(t, decision.Policy.BannedModels)
	assert.Empty(t, decision.Policy.RequiredCapabilities)
	assert.NotContains(t, decision.Constraints, ConstraintRoutingPolicy)
	assert.NotContains(t, decision.Constraints, ConstraintRequiredCapabilities)
	assert.NotContains(t, decision.Constraints, ConstraintProviderPreference)
	assert.NotContains(t, decision.Constraints, ConstraintBudget)
}

func TestDecide_PrefersPolicyProviderBeforeCost(t *testing.T) {
	t.Parallel()

	candidates := []Candidate{
		{Name: "cheap", Provider: "openai", InputTokenCost: 0.000001},
		{Name: "preferred", Provider: "anthropic", InputTokenCost: 0.000002},
	}

	decision := Decide(candidates, RequestProfile{EstimatedInputTokens: 100}, Policy{PreferredProviders: []string{"anthropic"}}, nil)

	assert.Equal(t, "anthropic/preferred", decision.Selected)
}

func TestDecide_AppliesLatencyPolicyLimits(t *testing.T) {
	t.Parallel()

	candidates := []Candidate{
		{Name: "slow", Provider: "openai", InputTokenCost: 0.000001, ExpectedLatencyMS: 900, ExpectedTTFTMS: 80},
		{Name: "slow-ttft", Provider: "openai", InputTokenCost: 0.000001, ExpectedLatencyMS: 100, ExpectedTTFTMS: 500},
		{Name: "fast", Provider: "openai", InputTokenCost: 0.000001, ExpectedLatencyMS: 100, ExpectedTTFTMS: 80},
		{Name: "unknown", Provider: "openai", InputTokenCost: 0.000001},
	}

	decision := Decide(candidates, RequestProfile{Interactive: true}, Policy{
		MaxLatencyMS: 250,
		MaxTTFTMS:    150,
	}, nil)

	assert.Contains(t, decision.Constraints, ConstraintRoutingPolicy)
	assert.Contains(t, decision.Constraints, ConstraintLatency)
	assert.Contains(t, decision.Constraints, ConstraintTTFT)
	assert.Equal(t, "openai/fast", decision.Selected)
	assert.Equal(t, []string{"openai/fast", "openai/unknown"}, decision.FallbackOrder)
	assertRejectionContains(t, decision, "openai/slow", ReasonLatencyExceeded)
	assertRejectionContains(t, decision, "openai/slow-ttft", ReasonTTFTExceeded)
}

func TestDecide_AppliesObservedLatencyPolicyLimits(t *testing.T) {
	t.Parallel()

	candidate := Candidate{Name: "observed-slow", Provider: "openai", InputTokenCost: 0.000001}
	telemetry := NewTelemetry()
	telemetry.Record(candidate, ActualUsage{Latency: 900 * time.Millisecond, TTFT: 500 * time.Millisecond}, time.Now())

	decision := Decide([]Candidate{candidate}, RequestProfile{Interactive: true}, Policy{
		MaxLatencyMS: 250,
		MaxTTFTMS:    150,
	}, telemetry)

	assert.Empty(t, decision.Selected)
	assertRejectionContains(t, decision, "openai/observed-slow", ReasonLatencyExceeded)
	assertRejectionContains(t, decision, "openai/observed-slow", ReasonTTFTExceeded)
}

func TestDecide_BannedModelsMatchProviderReportedAliases(t *testing.T) {
	t.Parallel()

	catalog := BuiltinCatalog()
	candidate, ok := catalog.Candidate("openai/gpt-4.1-mini")
	require.True(t, ok)
	fallback, ok := catalog.Candidate("openai/gpt-4.1-nano")
	require.True(t, ok)

	for _, bannedModel := range []string{"gpt-4.1-mini-2025-04-14", "openai/gpt-4.1-mini-2025-04-14"} {
		decision := Decide(
			[]Candidate{candidate, fallback},
			RequestProfile{EstimatedInputTokens: 100},
			Policy{BannedModels: []string{bannedModel}},
			nil,
		)

		assert.Equal(t, "openai/gpt-4.1-nano", decision.Selected)
		assertRejectionContains(t, decision, "openai/gpt-4.1-mini", ReasonModelBanned)
	}
}

func TestCatalogCandidatesForModelReturnsAmbiguousMatches(t *testing.T) {
	t.Parallel()

	candidates := BuiltinCatalog().CandidatesForModel("gpt-5.4-mini")

	ids := make([]string, 0, len(candidates))
	for i := range candidates {
		ids = append(ids, candidates[i].ID())
	}

	assert.Contains(t, ids, "openai/gpt-5.4-mini")
	assert.Contains(t, ids, "codex/gpt-5.4-mini")
}

func TestDecisionWithAvailabilityRejectsUnavailableAndReranksFallback(t *testing.T) {
	t.Parallel()

	decision := Decide(
		[]Candidate{
			{Name: "primary", Provider: "anthropic", InputTokenCost: 0.000001},
			{Name: "fallback", Provider: "openai", InputTokenCost: 0.000002},
		},
		RequestProfile{EstimatedInputTokens: 100},
		Policy{PreferredProviders: []string{"anthropic"}},
		nil,
	)

	annotated := DecisionWithAvailability(decision, Availability{
		Checked: true,
		Providers: []string{
			"openai",
		},
		Unavailable: map[string]string{
			"anthropic/primary": ReasonProviderUnavailable + ": anthropic",
		},
	})

	assert.Equal(t, "openai/fallback", annotated.Selected)
	assert.Equal(t, []string{"openai/fallback"}, annotated.FallbackOrder)
	assert.Contains(t, annotated.Constraints, ConstraintRuntimeAvailability)
	assertRejectionContains(t, annotated, "anthropic/primary", ReasonProviderUnavailable)
	fallback := findCandidateDecision(t, annotated, "openai/fallback")
	assert.Equal(t, StatusSelected, fallback.Status)
	assert.Equal(t, 1, fallback.Rank)
}

func TestDecisionWithAvailabilityMatchesAliasesAndProviderLocalModels(t *testing.T) {
	t.Parallel()

	candidates := []Candidate{
		{
			Name:           "gpt-4.1-mini",
			Provider:       "openai",
			Aliases:        []string{"gpt-4.1-mini-2025-04-14"},
			InputTokenCost: 0.000001,
		},
		{
			Name:           "fallback",
			Provider:       "openai",
			InputTokenCost: 0.000002,
		},
	}
	decision := Decide(candidates, RequestProfile{EstimatedInputTokens: 100}, Policy{}, nil)

	annotated := DecisionWithAvailability(decision, Availability{
		Checked: true,
		Unavailable: map[string]string{
			"openai/gpt-4.1-mini-2025-04-14": ReasonModelUnavailable,
		},
	})

	assert.Equal(t, "openai/fallback", annotated.Selected)
	assert.Equal(t, []string{"openai/fallback"}, annotated.FallbackOrder)
	assertRejectionContains(t, annotated, "openai/gpt-4.1-mini", ReasonModelUnavailable)

	providerLocal := DecisionWithAvailability(decision, Availability{
		Checked: true,
		Unavailable: map[string]string{
			"gpt-4.1-mini": ReasonModelUnavailable,
		},
	})

	assert.Equal(t, "openai/fallback", providerLocal.Selected)
	assertRejectionContains(t, providerLocal, "openai/gpt-4.1-mini", ReasonModelUnavailable)
}

func TestDecisionWithAvailabilityDoesNotApplyAmbiguousBareModelUnavailable(t *testing.T) {
	t.Parallel()

	candidates := []Candidate{
		{Name: "shared", Provider: "openai", InputTokenCost: 0.000001},
		{Name: "shared", Provider: "anthropic", InputTokenCost: 0.000002},
	}
	decision := Decide(candidates, RequestProfile{EstimatedInputTokens: 100}, Policy{}, nil)

	annotated := DecisionWithAvailability(decision, Availability{
		Checked: true,
		Unavailable: map[string]string{
			"shared": ReasonModelUnavailable,
		},
	})

	assert.Equal(t, "openai/shared", annotated.Selected)
	assert.Equal(t, []string{"openai/shared", "anthropic/shared"}, annotated.FallbackOrder)
	assert.Empty(t, findCandidateDecision(t, annotated, "openai/shared").Rejected)
	assert.Empty(t, findCandidateDecision(t, annotated, "anthropic/shared").Rejected)
}

func TestDecide_UsesObservedLatencyForRanking(t *testing.T) {
	t.Parallel()

	candidates := []Candidate{
		{Name: "slow-observed", Provider: "openai", InputTokenCost: 0.000001, ExpectedLatencyMS: 10, ExpectedTTFTMS: 10},
		{Name: "fast-observed", Provider: "openai", InputTokenCost: 0.000001, ExpectedLatencyMS: 200, ExpectedTTFTMS: 200},
	}
	telemetry := NewTelemetry()
	observedAt := time.Date(2026, time.May, 22, 12, 0, 0, 0, time.UTC)

	telemetry.Record(candidates[0], ActualUsage{
		Latency: 100 * time.Millisecond,
		TTFT:    80 * time.Millisecond,
	}, observedAt)
	telemetry.Record(candidates[1], ActualUsage{
		Latency: 20 * time.Millisecond,
		TTFT:    5 * time.Millisecond,
	}, observedAt)

	decision := Decide(candidates, RequestProfile{EstimatedInputTokens: 100, Interactive: true}, Policy{}, telemetry)

	assert.Equal(t, "openai/fast-observed", decision.Selected)
	assert.Equal(t, []string{"openai/fast-observed", "openai/slow-observed"}, decision.FallbackOrder)
	assert.Contains(t, decision.Constraints, ConstraintObservedTelemetry)
	assert.Contains(t, decision.Constraints, ConstraintTTFT)
	assert.Contains(t, decision.Constraints, ConstraintLatency)

	fast := findCandidateDecision(t, decision, "openai/fast-observed")
	assert.False(t, fast.ActualUsageRecorded)
	assert.Equal(t, 20, fast.ObservedLatencyMS)
	assert.Equal(t, 5, fast.ObservedTTFTMS)
	assert.Equal(t, 200, fast.ExpectedLatencyMS)
	assert.Equal(t, 200, fast.ExpectedTTFTMS)
}

func TestDecide_OnlyReportsEvidenceBackedDynamicConstraints(t *testing.T) {
	t.Parallel()

	candidate := Candidate{Name: "primary", Provider: "openai", InputTokenCost: 0.000001}
	telemetry := NewTelemetry()
	telemetry.Record(
		Candidate{Name: "unrelated", Provider: "openai", InputTokenCost: 0.000001},
		ActualUsage{Latency: 10 * time.Millisecond},
		time.Date(2026, time.May, 22, 12, 0, 0, 0, time.UTC),
	)

	decision := Decide([]Candidate{candidate}, RequestProfile{EstimatedInputTokens: 100, Interactive: true}, Policy{}, telemetry)

	assert.NotContains(t, decision.Constraints, ConstraintObservedTelemetry)
	assert.NotContains(t, decision.Constraints, ConstraintTTFT)
	assert.NotContains(t, decision.Constraints, ConstraintLatency)
	assert.Contains(t, decision.Constraints, ConstraintContextWindow)
	assert.Contains(t, decision.Constraints, ConstraintEstimatedCost)
}

func TestTelemetry_RecordUpdatesActualCostAndDecisionArtifact(t *testing.T) {
	t.Parallel()

	candidate := Candidate{
		Name:                 "fast",
		Provider:             "openai",
		InputTokenCost:       0.000001,
		CachedInputTokenCost: 0.0000001,
		CacheWriteTokenCost:  0.00000125,
		OutputTokenCost:      0.000004,
	}
	telemetry := NewTelemetry()

	obs := telemetry.Record(candidate, ActualUsage{
		Latency:           25 * time.Millisecond,
		TTFT:              5 * time.Millisecond,
		InputTokens:       1000,
		CachedInputTokens: 200,
		CacheWriteTokens:  100,
		OutputTokens:      50,
	}, time.Date(2026, time.May, 22, 12, 0, 0, 0, time.UTC))

	assert.Equal(t, 1, obs.Count)
	assert.Equal(t, 1, obs.TokenUsageCount)
	assert.Equal(t, 1, obs.LatencySamples)
	assert.Equal(t, 1, obs.TTFTSamples)
	assert.Equal(t, 25, obs.AvgLatencyMS)
	assert.Equal(t, 5, obs.AvgTTFTMS)
	assert.Equal(t, 100, obs.CacheWriteTokens)
	assert.InDelta(t, 0.001045, obs.LastCost, 0.000000001)
	assert.InDelta(t, 0, obs.LastEstimatedDeltaUSD, 0.000000001)

	decision := Decide([]Candidate{candidate}, RequestProfile{EstimatedInputTokens: 900}, Policy{}, telemetry)
	cd := findCandidateDecision(t, decision, "openai/fast")
	assert.Equal(t, StatusSelected, cd.Status)
	assert.True(t, cd.ActualUsageRecorded)
	assert.Equal(t, 1000, cd.ActualInputTokens)
	assert.Equal(t, 200, cd.ActualCachedTokens)
	assert.Equal(t, 100, cd.ActualCacheWrites)
	assert.Equal(t, 50, cd.ActualOutputTokens)
	assert.Equal(t, 25, cd.ObservedLatencyMS)
	assert.InDelta(t, obs.LastCost, cd.ActualCost, 0.000000001)
	assert.InDelta(t, 0.000145, cd.ActualCostDelta, 0.000000001)
}

func TestTelemetry_ProfileFromActualUsageClampsCacheReads(t *testing.T) {
	t.Parallel()

	candidate := Candidate{
		Name:                 "fast",
		Provider:             "openai",
		InputTokenCost:       0.000001,
		CachedInputTokenCost: 0.0000001,
		CacheWriteTokenCost:  0.00000125,
		OutputTokenCost:      0.000004,
	}
	telemetry := NewTelemetry()

	obs := telemetry.Record(candidate, ActualUsage{
		InputTokens:       100,
		CachedInputTokens: 150,
		CacheWriteTokens:  50,
		OutputTokens:      10,
	}, time.Date(2026, time.May, 22, 12, 0, 0, 0, time.UTC))

	assert.InDelta(t, 0, obs.LastEstimatedDeltaUSD, 0.000000001)
	assert.InDelta(t, EstimateActualCost(candidate, ActualUsage{
		InputTokens:       100,
		CachedInputTokens: 150,
		CacheWriteTokens:  50,
		OutputTokens:      10,
	}), obs.LastCost, 0.000000001)
}

func TestTelemetry_RecordAveragesOnlyObservedTiming(t *testing.T) {
	t.Parallel()

	candidate := Candidate{Name: "fast", Provider: "openai"}
	telemetry := NewTelemetry()
	observedAt := time.Date(2026, time.May, 22, 12, 0, 0, 0, time.UTC)

	telemetry.Record(candidate, ActualUsage{InputTokens: 1}, observedAt)
	obs := telemetry.Record(candidate, ActualUsage{
		Latency: 20 * time.Millisecond,
		TTFT:    5 * time.Millisecond,
	}, observedAt.Add(time.Second))

	assert.Equal(t, 2, obs.Count)
	assert.Equal(t, 1, obs.TokenUsageCount)
	assert.Equal(t, 1, obs.LatencySamples)
	assert.Equal(t, 1, obs.TTFTSamples)
	assert.Equal(t, 20, obs.AvgLatencyMS)
	assert.Equal(t, 5, obs.AvgTTFTMS)
}

func TestTelemetry_RecordFailureRejectsRecentRateLimit(t *testing.T) {
	t.Parallel()

	primary := Candidate{Name: "primary", Provider: "openai", InputTokenCost: 0.000001}
	fallback := Candidate{Name: "fallback", Provider: "anthropic", InputTokenCost: 0.000002}
	telemetry := NewTelemetry()
	observedAt := time.Date(2026, time.May, 22, 12, 0, 0, 0, time.UTC)

	obs := telemetry.RecordFailure(primary, Failure{
		RetryAfter:  2 * time.Second,
		Error:       "openai: HTTP 429: rate limited",
		Kind:        "transient_rate_limit",
		Retryable:   true,
		RateLimited: true,
	}, observedAt)

	assert.Equal(t, 1, obs.FailureCount)
	assert.Equal(t, 1, obs.RateLimitCount)
	assert.Equal(t, "transient_rate_limit", obs.LastFailureKind)
	assert.True(t, obs.LastFailureRateLimited)
	assert.Equal(t, 2000, obs.LastRetryAfterMS)
	assert.Equal(t, observedAt.Add(2*time.Second), obs.RateLimitUntil())

	decision := DecideAt([]Candidate{primary, fallback}, RequestProfile{EstimatedInputTokens: 100}, Policy{
		PreferredProviders: []string{"openai"},
	}, telemetry, observedAt.Add(time.Second))

	assert.Equal(t, "anthropic/fallback", decision.Selected)
	rateLimited := findCandidateDecision(t, decision, "openai/primary")
	assert.Equal(t, StatusRejected, rateLimited.Status)
	assert.Equal(t, 1, rateLimited.FailureCount)
	assert.Equal(t, 1, rateLimited.RateLimitCount)
	assert.Equal(t, 2000, rateLimited.LastRetryAfterMS)
	assert.Equal(t, observedAt.Add(2*time.Second).Format(time.RFC3339), rateLimited.RateLimitUntil)
	assert.Contains(t, rateLimited.LastError, "HTTP 429")
	assertRejectionContains(t, decision, "openai/primary", ReasonRateLimited)

	expired := DecideAt([]Candidate{primary, fallback}, RequestProfile{EstimatedInputTokens: 100}, Policy{
		PreferredProviders: []string{"openai"},
	}, telemetry, observedAt.Add(3*time.Second))

	assert.Equal(t, "openai/primary", expired.Selected)

	telemetry.Record(primary, ActualUsage{InputTokens: 10}, observedAt.Add(time.Minute))
	recovered := DecideAt([]Candidate{primary, fallback}, RequestProfile{EstimatedInputTokens: 100}, Policy{
		PreferredProviders: []string{"openai"},
	}, telemetry, observedAt.Add(time.Minute))

	assert.Equal(t, "openai/primary", recovered.Selected)
}

func TestTelemetry_RateLimitRejectsSameProviderSiblings(t *testing.T) {
	t.Parallel()

	primary := Candidate{Name: "primary", Provider: "openai", InputTokenCost: 0.000001}
	sibling := Candidate{Name: "sibling", Provider: "openai", InputTokenCost: 0.000001}
	fallback := Candidate{Name: "fallback", Provider: "anthropic", InputTokenCost: 0.000002}
	telemetry := NewTelemetry()
	observedAt := time.Date(2026, time.May, 22, 12, 0, 0, 0, time.UTC)

	telemetry.RecordFailure(primary, Failure{
		RetryAfter:  2 * time.Second,
		Error:       "openai: HTTP 429: rate limited",
		Kind:        "transient_rate_limit",
		Retryable:   true,
		RateLimited: true,
	}, observedAt)

	decision := DecideAt(
		[]Candidate{primary, sibling, fallback},
		RequestProfile{EstimatedInputTokens: 100},
		Policy{PreferredProviders: []string{"openai"}},
		telemetry,
		observedAt.Add(time.Second),
	)

	assert.Equal(t, "anthropic/fallback", decision.Selected)
	assertRejectionContains(t, decision, "openai/primary", ReasonRateLimited)
	assertRejectionContains(t, decision, "openai/sibling", ReasonRateLimited)

	siblingDecision := findCandidateDecision(t, decision, "openai/sibling")
	assert.Equal(t, "transient_rate_limit", siblingDecision.LastFailureKind)
	assert.Equal(t, RateLimitScopeProvider, siblingDecision.LastFailureRateLimitScope)
	assert.Contains(t, siblingDecision.LastError, "HTTP 429")
	assert.Equal(t, observedAt.Add(2*time.Second).Format(time.RFC3339), siblingDecision.RateLimitUntil)
}

func TestTelemetry_RateLimitUsesLongestProviderCooldown(t *testing.T) {
	t.Parallel()

	primary := Candidate{Name: "primary", Provider: "openai", InputTokenCost: 0.000001}
	sibling := Candidate{Name: "sibling", Provider: "openai", InputTokenCost: 0.000001}
	fallback := Candidate{Name: "fallback", Provider: "anthropic", InputTokenCost: 0.000002}
	telemetry := NewTelemetry()
	observedAt := time.Date(2026, time.May, 22, 12, 0, 0, 0, time.UTC)

	telemetry.RecordFailure(primary, Failure{
		RetryAfter:  time.Hour,
		Error:       "openai: HTTP 429: rate limited",
		Kind:        "transient_rate_limit",
		Retryable:   true,
		RateLimited: true,
	}, observedAt)
	telemetry.RecordFailure(sibling, Failure{
		Error:       "provider openai temporarily rate limited",
		Kind:        "transient_rate_limit",
		Retryable:   true,
		RateLimited: true,
	}, observedAt.Add(time.Second))

	decision := DecideAt(
		[]Candidate{primary, sibling, fallback},
		RequestProfile{EstimatedInputTokens: 100},
		Policy{PreferredProviders: []string{"openai"}},
		telemetry,
		observedAt.Add(2*time.Second),
	)

	siblingDecision := findCandidateDecision(t, decision, "openai/sibling")
	assert.Equal(t, observedAt.Add(time.Hour).Format(time.RFC3339), siblingDecision.RateLimitUntil)
	assert.Contains(t, siblingDecision.LastError, "HTTP 429")
}

func TestTelemetry_ProviderRateLimitClearedByLaterProviderSuccess(t *testing.T) {
	t.Parallel()

	primary := Candidate{Name: "primary", Provider: "openai", InputTokenCost: 0.000001}
	sibling := Candidate{Name: "sibling", Provider: "openai", InputTokenCost: 0.000001}
	fallback := Candidate{Name: "fallback", Provider: "anthropic", InputTokenCost: 0.000002}
	telemetry := NewTelemetry()
	observedAt := time.Date(2026, time.May, 22, 12, 0, 0, 0, time.UTC)

	telemetry.RecordFailure(primary, Failure{
		RetryAfter:  time.Hour,
		Error:       "openai: HTTP 429: rate limited",
		Kind:        "transient_rate_limit",
		Retryable:   true,
		RateLimited: true,
	}, observedAt)
	telemetry.Record(sibling, ActualUsage{InputTokens: 10}, observedAt.Add(time.Second))

	_, ok := telemetry.ProviderRateLimitObservation("openai", observedAt.Add(2*time.Second))
	require.False(t, ok)

	decision := DecideAt(
		[]Candidate{primary, sibling, fallback},
		RequestProfile{EstimatedInputTokens: 100},
		Policy{PreferredProviders: []string{"openai"}},
		telemetry,
		observedAt.Add(2*time.Second),
	)

	assert.Equal(t, "openai/sibling", decision.Selected)
	assertRejectionContains(t, decision, "openai/primary", ReasonRateLimited)
	primaryDecision := findCandidateDecision(t, decision, "openai/primary")
	assert.Equal(t, RateLimitScopeProvider, primaryDecision.LastFailureRateLimitScope)
	siblingDecision := findCandidateDecision(t, decision, "openai/sibling")
	assert.NotContains(t, strings.Join(siblingDecision.Rejected, "; "), ReasonRateLimited)
}

func TestTelemetry_ModelScopedRateLimitDoesNotRejectProviderSiblings(t *testing.T) {
	t.Parallel()

	primary := Candidate{Name: "primary", Provider: "openai", InputTokenCost: 0.000001}
	sibling := Candidate{Name: "sibling", Provider: "openai", InputTokenCost: 0.000001}
	fallback := Candidate{Name: "fallback", Provider: "anthropic", InputTokenCost: 0.000002}
	telemetry := NewTelemetry()
	observedAt := time.Date(2026, time.May, 22, 12, 0, 0, 0, time.UTC)

	obs := telemetry.RecordFailure(primary, Failure{
		RetryAfter:     time.Hour,
		Error:          "openai: HTTP 429: rate limited",
		Kind:           "transient_rate_limit",
		RateLimitScope: RateLimitScopeModel,
		Retryable:      true,
		RateLimited:    true,
	}, observedAt)

	assert.Equal(t, RateLimitScopeModel, obs.LastFailureRateLimitScope)

	_, providerLimited := telemetry.ProviderRateLimitObservation("openai", observedAt.Add(time.Second))
	require.False(t, providerLimited)

	decision := DecideAt(
		[]Candidate{primary, sibling, fallback},
		RequestProfile{EstimatedInputTokens: 100},
		Policy{PreferredProviders: []string{"openai"}},
		telemetry,
		observedAt.Add(time.Second),
	)

	assert.Equal(t, "openai/sibling", decision.Selected)
	assertRejectionContains(t, decision, "openai/primary", ReasonRateLimited)
	primaryDecision := findCandidateDecision(t, decision, "openai/primary")
	assert.Equal(t, RateLimitScopeModel, primaryDecision.LastFailureRateLimitScope)
	siblingDecision := findCandidateDecision(t, decision, "openai/sibling")
	assert.NotContains(t, strings.Join(siblingDecision.Rejected, "; "), ReasonRateLimited)
}

func TestTelemetry_ProviderRateLimitUsesPostRecoveryFailure(t *testing.T) {
	t.Parallel()

	older := Candidate{Name: "older", Provider: "openai"}
	recovered := Candidate{Name: "recovered", Provider: "openai"}
	newer := Candidate{Name: "newer", Provider: "openai"}
	telemetry := NewTelemetry()
	observedAt := time.Date(2026, time.May, 22, 12, 0, 0, 0, time.UTC)

	telemetry.RecordFailure(older, Failure{
		RetryAfter:  time.Hour,
		Error:       "openai: HTTP 429: older limit",
		Kind:        "transient_rate_limit",
		Retryable:   true,
		RateLimited: true,
	}, observedAt)
	telemetry.Record(recovered, ActualUsage{InputTokens: 10}, observedAt.Add(time.Second))
	telemetry.RecordFailure(newer, Failure{
		RetryAfter:  10 * time.Minute,
		Error:       "openai: HTTP 429: newer limit",
		Kind:        "transient_rate_limit",
		Retryable:   true,
		RateLimited: true,
	}, observedAt.Add(2*time.Second))

	obs, ok := telemetry.ProviderRateLimitObservation("openai", observedAt.Add(3*time.Second))
	require.True(t, ok)
	assert.Equal(t, "openai/newer", obs.ModelID)
	assert.Equal(t, observedAt.Add(2*time.Second+10*time.Minute), obs.RateLimitUntil())
	assert.Contains(t, obs.LastError, "newer limit")
}

func TestTelemetry_ProviderRateLimitUsesFailureAfterSameModelSuccess(t *testing.T) {
	t.Parallel()

	primary := Candidate{Name: "primary", Provider: "openai", InputTokenCost: 0.000001}
	sibling := Candidate{Name: "sibling", Provider: "openai", InputTokenCost: 0.000001}
	fallback := Candidate{Name: "fallback", Provider: "anthropic", InputTokenCost: 0.000002}
	telemetry := NewTelemetry()
	observedAt := time.Date(2026, time.May, 22, 12, 0, 0, 0, time.UTC)

	telemetry.Record(primary, ActualUsage{InputTokens: 10}, observedAt)
	telemetry.RecordFailure(primary, Failure{
		RetryAfter:  time.Hour,
		Error:       "openai: HTTP 429: later limit",
		Kind:        "transient_rate_limit",
		Retryable:   true,
		RateLimited: true,
	}, observedAt.Add(time.Second))

	obs, ok := telemetry.ProviderRateLimitObservation("openai", observedAt.Add(2*time.Second))
	require.True(t, ok)
	assert.Equal(t, "openai/primary", obs.ModelID)
	assert.Contains(t, obs.LastError, "later limit")

	decision := DecideAt(
		[]Candidate{primary, sibling, fallback},
		RequestProfile{EstimatedInputTokens: 100},
		Policy{PreferredProviders: []string{"openai"}},
		telemetry,
		observedAt.Add(2*time.Second),
	)

	assert.Equal(t, "anthropic/fallback", decision.Selected)
	assertRejectionContains(t, decision, "openai/primary", ReasonRateLimited)
	assertRejectionContains(t, decision, "openai/sibling", ReasonRateLimited)
}

func TestDecisionWithActualUsageAnnotatesSelectedCandidate(t *testing.T) {
	t.Parallel()

	candidates := []Candidate{
		{Name: "selected", Provider: "openai", InputTokenCost: 0.000001, OutputTokenCost: 0.000004},
		{Name: "fallback", Provider: "openai", InputTokenCost: 0.000002, OutputTokenCost: 0.000004},
	}
	decision := Decide(candidates, RequestProfile{EstimatedInputTokens: 100}, Policy{}, nil)

	annotated := DecisionWithActualUsage(decision, "", ActualUsage{
		Latency:      30 * time.Millisecond,
		TTFT:         7 * time.Millisecond,
		InputTokens:  100,
		OutputTokens: 10,
	})

	selected := findCandidateDecision(t, annotated, "openai/selected")
	assert.Equal(t, "openai/selected", annotated.ActualSelected)
	assert.True(t, selected.ActualUsageRecorded)
	assert.Equal(t, 100, selected.ActualInputTokens)
	assert.Equal(t, 10, selected.ActualOutputTokens)
	assert.InDelta(t, 0.00014, selected.ActualCost, 0.000000001)
	assert.InDelta(t, 0.00004, selected.ActualCostDelta, 0.000000001)
	assert.Equal(t, 30, selected.ObservedLatencyMS)
	assert.Equal(t, 7, selected.ObservedTTFTMS)
	fallback := findCandidateDecision(t, annotated, "openai/fallback")
	assert.False(t, fallback.ActualUsageRecorded)
	assert.Zero(t, fallback.ActualCost)
}

func TestDecisionWithTelemetryRefreshesCandidateEvidenceWithoutReranking(t *testing.T) {
	t.Parallel()

	candidates := []Candidate{
		{Name: "primary", Provider: "openai", InputTokenCost: 0.000001},
		{Name: "fallback", Provider: "anthropic", InputTokenCost: 0.000002},
	}
	decision := Decide(candidates, RequestProfile{EstimatedInputTokens: 100, Interactive: true}, Policy{}, nil)

	telemetry := NewTelemetry()
	telemetry.RecordFailure(candidates[0], Failure{
		Error:       "openai: HTTP 429: rate limited",
		Kind:        "transient_rate_limit",
		RetryAfter:  2 * time.Second,
		Retryable:   true,
		RateLimited: true,
	}, time.Date(2026, time.May, 22, 12, 0, 0, 0, time.UTC))
	telemetry.Record(candidates[1], ActualUsage{
		Latency:     25 * time.Millisecond,
		TTFT:        5 * time.Millisecond,
		InputTokens: 10,
	}, time.Date(2026, time.May, 22, 12, 0, 1, 0, time.UTC))

	annotated := DecisionWithTelemetry(decision, telemetry)

	assert.Equal(t, "openai/primary", annotated.Selected)
	assert.Equal(t, []string{"openai/primary", "anthropic/fallback"}, annotated.FallbackOrder)
	assert.Contains(t, annotated.Constraints, ConstraintObservedTelemetry)
	assert.Contains(t, annotated.Constraints, ConstraintLatency)
	assert.Contains(t, annotated.Constraints, ConstraintTTFT)
	primary := findCandidateDecision(t, annotated, "openai/primary")
	assert.Equal(t, StatusSelected, primary.Status)
	assert.Equal(t, 1, primary.FailureCount)
	assert.Equal(t, 1, primary.RateLimitCount)
	assert.Equal(t, "transient_rate_limit", primary.LastFailureKind)
	assert.Equal(t, RateLimitScopeProvider, primary.LastFailureRateLimitScope)
	assert.Contains(t, primary.LastError, "HTTP 429")
	assert.Equal(t, "2026-05-22T12:00:02Z", primary.RateLimitUntil)
	fallback := findCandidateDecision(t, annotated, "anthropic/fallback")
	assert.Equal(t, StatusFallback, fallback.Status)
	assert.Equal(t, 25, fallback.ObservedLatencyMS)
	assert.Equal(t, 5, fallback.ObservedTTFTMS)
}

func TestDecisionWithTelemetryAnnotatesProviderRateLimitSiblings(t *testing.T) {
	t.Parallel()

	candidates := []Candidate{
		{Name: "primary", Provider: "openai", InputTokenCost: 0.000001},
		{Name: "sibling", Provider: "openai", InputTokenCost: 0.000001},
		{Name: "fallback", Provider: "anthropic", InputTokenCost: 0.000002},
	}
	decision := Decide(candidates, RequestProfile{EstimatedInputTokens: 100}, Policy{}, nil)

	telemetry := NewTelemetry()
	observedAt := time.Now().UTC()
	telemetry.RecordFailure(candidates[0], Failure{
		Error:       "openai: HTTP 429: rate limited",
		Kind:        "transient_rate_limit",
		RetryAfter:  time.Hour,
		Retryable:   true,
		RateLimited: true,
	}, observedAt)

	annotated := DecisionWithTelemetry(decision, telemetry)

	assert.Equal(t, "openai/primary", annotated.Selected)
	assert.Equal(t, []string{"openai/primary", "openai/sibling", "anthropic/fallback"}, annotated.FallbackOrder)
	assert.Contains(t, annotated.Constraints, ConstraintObservedTelemetry)

	sibling := findCandidateDecision(t, annotated, "openai/sibling")
	assert.Equal(t, StatusFallback, sibling.Status)
	assert.Equal(t, "transient_rate_limit", sibling.LastFailureKind)
	assert.Equal(t, RateLimitScopeProvider, sibling.LastFailureRateLimitScope)
	assert.Contains(t, sibling.LastError, "HTTP 429")
	assert.Equal(t, 1, sibling.RateLimitCount)
	assert.NotEmpty(t, sibling.RateLimitUntil)
}

func TestDecisionAnnotatorsDoNotMutateInputDecision(t *testing.T) {
	t.Parallel()

	candidates := []Candidate{
		{Name: "primary", Provider: "openai", InputTokenCost: 0.000001, OutputTokenCost: 0.000004},
		{Name: "fallback", Provider: "anthropic", InputTokenCost: 0.000002, OutputTokenCost: 0.000004},
	}
	decision := Decide(candidates, RequestProfile{EstimatedInputTokens: 100, Interactive: true}, Policy{}, nil)

	availability := Availability{
		Checked:     true,
		Unavailable: map[string]string{"openai/primary": ReasonModelUnavailable},
	}
	withAvailability := DecisionWithAvailability(decision, availability)

	assert.Equal(t, "openai/primary", decision.Selected)
	assert.Equal(t, StatusSelected, findCandidateDecision(t, decision, "openai/primary").Status)
	assert.Equal(t, "anthropic/fallback", withAvailability.Selected)
	assertRejectionContains(t, withAvailability, "openai/primary", ReasonModelUnavailable)

	telemetry := NewTelemetry()
	telemetry.Record(candidates[1], ActualUsage{
		Latency:     25 * time.Millisecond,
		TTFT:        5 * time.Millisecond,
		InputTokens: 10,
	}, time.Date(2026, time.May, 22, 12, 0, 0, 0, time.UTC))
	withTelemetry := DecisionWithTelemetry(decision, telemetry)

	assert.NotContains(t, decision.Constraints, ConstraintObservedTelemetry)
	assert.Zero(t, findCandidateDecision(t, decision, "anthropic/fallback").TelemetryCount)
	assert.Contains(t, withTelemetry.Constraints, ConstraintObservedTelemetry)
	assert.Equal(t, 1, findCandidateDecision(t, withTelemetry, "anthropic/fallback").TelemetryCount)

	withUsage := DecisionWithActualUsage(decision, "anthropic/fallback", ActualUsage{
		InputTokens:  100,
		OutputTokens: 10,
	})

	assert.Empty(t, decision.ActualSelected)
	assert.False(t, findCandidateDecision(t, decision, "anthropic/fallback").ActualUsageRecorded)
	assert.Equal(t, "anthropic/fallback", withUsage.ActualSelected)
	assert.True(t, findCandidateDecision(t, withUsage, "anthropic/fallback").ActualUsageRecorded)
}

func TestDecisionWithActualUsageAnnotatesFallbackCandidate(t *testing.T) {
	t.Parallel()

	candidates := []Candidate{
		{Name: "primary", Provider: "openai", InputTokenCost: 0.000001, OutputTokenCost: 0.000004},
		{Name: "fallback", Provider: "openai", InputTokenCost: 0.000002, OutputTokenCost: 0.000004},
	}
	decision := Decide(candidates, RequestProfile{EstimatedInputTokens: 100}, Policy{}, nil)

	annotated := DecisionWithActualUsage(decision, "fallback", ActualUsage{
		InputTokens:  100,
		OutputTokens: 10,
	})

	assert.Equal(t, "openai/primary", annotated.Selected)
	assert.Equal(t, "openai/fallback", annotated.ActualSelected)
	primary := findCandidateDecision(t, annotated, "openai/primary")
	assert.False(t, primary.ActualUsageRecorded)
	assert.Zero(t, primary.ActualCost)
	fallback := findCandidateDecision(t, annotated, "openai/fallback")
	assert.True(t, fallback.ActualUsageRecorded)
	assert.Equal(t, 100, fallback.ActualInputTokens)
	assert.Equal(t, 10, fallback.ActualOutputTokens)
	assert.InDelta(t, 0.00024, fallback.ActualCost, 0.000000001)
	assert.InDelta(t, 0.00004, fallback.ActualCostDelta, 0.000000001)
}

func TestDecisionWithActualUsageDoesNotGuessAmbiguousBareModel(t *testing.T) {
	t.Parallel()

	candidates := []Candidate{
		{Name: "shared", Provider: "openai", InputTokenCost: 0.000001, OutputTokenCost: 0.000004},
		{Name: "shared", Provider: "anthropic", InputTokenCost: 0.000002, OutputTokenCost: 0.000004},
	}
	decision := Decide(candidates, RequestProfile{EstimatedInputTokens: 100}, Policy{}, nil)

	annotated := DecisionWithActualUsage(decision, "shared", ActualUsage{
		InputTokens:  100,
		OutputTokens: 10,
	})

	assert.Empty(t, annotated.ActualSelected)
	assert.False(t, findCandidateDecision(t, annotated, "openai/shared").ActualUsageRecorded)
	assert.False(t, findCandidateDecision(t, annotated, "anthropic/shared").ActualUsageRecorded)
}

func TestDecisionWithActualUsageAnnotatesProviderReportedAlias(t *testing.T) {
	t.Parallel()

	catalog := BuiltinCatalog()
	candidate, ok := catalog.Candidate("openai/gpt-4.1-mini")
	require.True(t, ok)
	other, ok := catalog.Candidate("openai/gpt-4.1")
	require.True(t, ok)

	decision := Decide([]Candidate{candidate, other}, RequestProfile{EstimatedInputTokens: 100}, Policy{}, nil)
	annotated := DecisionWithActualUsage(decision, "openai/gpt-4.1-mini-2025-04-14", ActualUsage{
		InputTokens:  100,
		OutputTokens: 10,
	})

	assert.Equal(t, "openai/gpt-4.1-mini", annotated.ActualSelected)
	selected := findCandidateDecision(t, annotated, "openai/gpt-4.1-mini")
	assert.True(t, selected.ActualUsageRecorded)
	assert.Positive(t, selected.ActualCost)
	assert.False(t, findCandidateDecision(t, annotated, "openai/gpt-4.1").ActualUsageRecorded)
}

func TestDecisionWithActualUsageFallsBackToUniqueProviderForUnknownRevision(t *testing.T) {
	t.Parallel()

	candidates := []Candidate{
		{Name: "gpt-4.1-mini", Provider: "openai", InputTokenCost: 0.000001, OutputTokenCost: 0.000004},
		{Name: "claude-sonnet-4-20250514", Provider: "anthropic", InputTokenCost: 0.000003, OutputTokenCost: 0.000015},
	}
	decision := Decide(candidates, RequestProfile{EstimatedInputTokens: 100}, Policy{}, nil)

	annotated := DecisionWithActualUsage(decision, "openai/gpt-4.1-mini-2026-05-22", ActualUsage{
		InputTokens:  100,
		OutputTokens: 10,
	})

	assert.Equal(t, "openai/gpt-4.1-mini", annotated.ActualSelected)
	selected := findCandidateDecision(t, annotated, "openai/gpt-4.1-mini")
	assert.True(t, selected.ActualUsageRecorded)
	assert.InDelta(t, 0.00014, selected.ActualCost, 0.000000001)
}

func TestDecisionWithActualUsageDoesNotGuessAmbiguousProviderRevision(t *testing.T) {
	t.Parallel()

	candidates := []Candidate{
		{Name: "gpt-4.1", Provider: "openai", InputTokenCost: 0.000002, OutputTokenCost: 0.000008},
		{Name: "gpt-4.1-mini", Provider: "openai", InputTokenCost: 0.000001, OutputTokenCost: 0.000004},
	}
	decision := Decide(candidates, RequestProfile{EstimatedInputTokens: 100}, Policy{}, nil)

	annotated := DecisionWithActualUsage(decision, "openai/gpt-4.1-mini-2026-05-22", ActualUsage{
		InputTokens:  100,
		OutputTokens: 10,
	})

	assert.Empty(t, annotated.ActualSelected)
	assert.False(t, findCandidateDecision(t, annotated, "openai/gpt-4.1").ActualUsageRecorded)
	assert.False(t, findCandidateDecision(t, annotated, "openai/gpt-4.1-mini").ActualUsageRecorded)
}

func TestDecisionWithActualUsageMarksZeroCostActuals(t *testing.T) {
	t.Parallel()

	decision := Decide(
		[]Candidate{{Name: "llama3.2", Provider: "ollama"}},
		RequestProfile{EstimatedInputTokens: 100},
		Policy{},
		nil,
	)

	annotated := DecisionWithActualUsage(decision, "", ActualUsage{
		InputTokens:  100,
		OutputTokens: 10,
	})

	selected := findCandidateDecision(t, annotated, "ollama/llama3.2")
	assert.True(t, selected.ActualUsageRecorded)
	assert.Zero(t, selected.ActualCost)
	assert.Zero(t, selected.ActualCostDelta)
}

func findCandidateDecision(t *testing.T, decision Decision, id string) CandidateDecision {
	t.Helper()

	for i := range decision.Candidates {
		candidate := decision.Candidates[i]
		if candidate.ID == id {
			return candidate
		}
	}

	require.Failf(t, "candidate decision not found", "id %q in %#v", id, decision.Candidates)

	return CandidateDecision{}
}

func assertRejectionContains(t *testing.T, decision Decision, id, want string) {
	t.Helper()

	cd := findCandidateDecision(t, decision, id)
	require.Equal(t, StatusRejected, cd.Status)

	for _, reason := range cd.Rejected {
		if strings.Contains(reason, want) {
			return
		}
	}

	require.Failf(t, "rejection reason not found", "candidate %q rejected by %#v, want %q", id, cd.Rejected, want)
}
