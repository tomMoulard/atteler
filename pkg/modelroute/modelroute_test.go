package modelroute

import "testing"

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

func TestFilter_RemovesOverBudgetAndOverContextCandidates(t *testing.T) {
	t.Parallel()

	candidates := []Candidate{
		{Name: "too-small-context", MaxInputTokens: 1000, InputTokenCost: 0.01},
		{Name: "too-expensive", MaxInputTokens: 2000, InputTokenCost: 1},
		{Name: "fits", MaxInputTokens: 2000, InputTokenCost: 0.01},
	}
	profile := RequestProfile{EstimatedInputTokens: 1500, Budget: 20}

	got := Filter(candidates, profile)
	if len(got) != 1 || got[0].Name != "fits" {
		t.Fatalf("Filter() = %#v, want only fits", got)
	}
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
