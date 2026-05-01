// Package modelroute provides dependency-free primitives for ranking model
// candidates against request cost, context, and latency constraints.
package modelroute

import "sort"

// Candidate describes a routable model and the metadata needed to estimate
// whether it can serve a request.
type Candidate struct {
	// Name is the provider-local model name, for example "gpt-4.1-mini".
	Name string
	// Provider identifies the model provider, for example "openai".
	Provider string

	// InputTokenCost is the cost per one input token.
	InputTokenCost float64
	// OutputTokenCost is the cost per one output token.
	OutputTokenCost float64

	// Priority is an operator-defined preference. Lower values are preferred.
	Priority int
	// MaxInputTokens is the model context/input limit. Zero means unknown or unlimited.
	MaxInputTokens int
	// ExpectedLatencyMS is the expected end-to-end latency in milliseconds.
	ExpectedLatencyMS int
	// ExpectedTTFTMS is the expected time-to-first-token in milliseconds.
	ExpectedTTFTMS int
}

// RequestProfile describes the request shape used for cost and fit estimates.
type RequestProfile struct {
	// EstimatedInputTokens is the estimated prompt/input token count.
	EstimatedInputTokens int
	// EstimatedOutputTokens is the estimated completion/output token count.
	EstimatedOutputTokens int
	// Budget is the maximum acceptable estimated request cost. Zero means no budget limit.
	Budget float64
	// Interactive marks latency-sensitive requests where TTFT should influence ranking.
	Interactive bool
	// Batch marks throughput-oriented requests where total latency matters less than cost.
	Batch bool
	// PromptCacheReuseEstimate is the estimated share of input tokens that will be
	// reused from prompt cache. Values outside [0,1] are clamped.
	PromptCacheReuseEstimate float64
}

// ID returns a stable provider/name identifier for a candidate.
func (c Candidate) ID() string {
	if c.Provider == "" {
		return c.Name
	}
	if c.Name == "" {
		return c.Provider
	}

	return c.Provider + "/" + c.Name
}

// EstimateCost returns the estimated request cost for candidate and profile.
// Prompt cache reuse reduces estimated billable input tokens only.
func EstimateCost(candidate Candidate, profile RequestProfile) float64 {
	billableInputTokens := float64(nonNegative(profile.EstimatedInputTokens)) * (1 - clamp01(profile.PromptCacheReuseEstimate))
	outputTokens := float64(nonNegative(profile.EstimatedOutputTokens))

	return billableInputTokens*candidate.InputTokenCost + outputTokens*candidate.OutputTokenCost
}

// FitsContext reports whether candidate has enough declared input context for profile.
func FitsContext(candidate Candidate, profile RequestProfile) bool {
	return candidate.MaxInputTokens <= 0 || nonNegative(profile.EstimatedInputTokens) <= candidate.MaxInputTokens
}

// FitsBudget reports whether candidate is within the request budget. A zero or
// negative budget means no budget limit is applied.
func FitsBudget(candidate Candidate, profile RequestProfile) bool {
	return profile.Budget <= 0 || EstimateCost(candidate, profile) <= profile.Budget
}

// Filter returns candidates that fit both budget and input context constraints.
// Candidate order is preserved.
func Filter(candidates []Candidate, profile RequestProfile) []Candidate {
	filtered := make([]Candidate, 0, len(candidates))
	for _, candidate := range candidates {
		if FitsContext(candidate, profile) && FitsBudget(candidate, profile) {
			filtered = append(filtered, candidate)
		}
	}

	return filtered
}

// SelectBest returns the best candidate that fits budget and context constraints.
// Selection is stable: if candidates compare equally, the earlier input candidate wins.
func SelectBest(candidates []Candidate, profile RequestProfile) (Candidate, bool) {
	chain := FallbackChain(candidates, profile)
	if len(chain) == 0 {
		return Candidate{}, false
	}

	return chain[0], true
}

// FallbackChain returns all candidates that fit budget and context constraints,
// ordered from most to least preferred. Ordering is stable for ties.
func FallbackChain(candidates []Candidate, profile RequestProfile) []Candidate {
	chain := Filter(candidates, profile)
	sort.SliceStable(chain, func(i, j int) bool {
		return better(chain[i], chain[j], profile)
	})

	return chain
}

func better(left, right Candidate, profile RequestProfile) bool {
	if left.Priority != right.Priority {
		return left.Priority < right.Priority
	}

	leftCost := EstimateCost(left, profile)
	rightCost := EstimateCost(right, profile)
	if leftCost != rightCost {
		return leftCost < rightCost
	}

	if profile.Interactive && left.ExpectedTTFTMS != right.ExpectedTTFTMS {
		return normalizedLatency(left.ExpectedTTFTMS) < normalizedLatency(right.ExpectedTTFTMS)
	}

	if !profile.Batch && left.ExpectedLatencyMS != right.ExpectedLatencyMS {
		return normalizedLatency(left.ExpectedLatencyMS) < normalizedLatency(right.ExpectedLatencyMS)
	}

	return false
}

func nonNegative(value int) int {
	if value < 0 {
		return 0
	}

	return value
}

func clamp01(value float64) float64 {
	if value < 0 {
		return 0
	}
	if value > 1 {
		return 1
	}

	return value
}

func normalizedLatency(value int) int {
	if value <= 0 {
		return int(^uint(0) >> 1)
	}

	return value
}
