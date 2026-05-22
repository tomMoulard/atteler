// Package modelroute provides dependency-free primitives for ranking model
// candidates against request cost, context, policy, and latency constraints.
package modelroute

import "sort"

// Candidate describes a routable model and the metadata needed to estimate
// whether it can serve a request.
//
//nolint:govet // Field order keeps route metadata grouped for callers and JSON artifacts.
type Candidate struct {
	// Name is the provider-local model name, for example "gpt-4.1-mini".
	Name string `json:"name,omitempty"`
	// Provider identifies the model provider, for example "openai".
	Provider string `json:"provider,omitempty"`
	// Capabilities describes route-relevant model features such as "tools" or
	// "reasoning". Routing policies can require capabilities before ranking.
	Capabilities []string `json:"capabilities,omitempty"`
	// Aliases are provider-reported model IDs that map back to this canonical
	// candidate, for example dated API snapshots.
	Aliases []string `json:"aliases,omitempty"`
	// MetadataVersion is the version of the metadata source that produced this
	// candidate. It is empty for legacy caller-supplied candidates.
	MetadataVersion string `json:"metadata_version,omitempty"`
	// MetadataSource is a human-readable source name or URL for the metadata.
	MetadataSource string `json:"metadata_source,omitempty"`
	// MetadataSourceURL links to the provider documentation used for this metadata.
	MetadataSourceURL string `json:"metadata_source_url,omitempty"`
	// MetadataPublished records the source snapshot/publication date when known.
	MetadataPublished string `json:"metadata_published,omitempty"`
	// Deprecated marks provider catalog entries that are available only for
	// compatibility or migration windows. Route artifacts expose this so
	// operators can spot version drift in configured model chains.
	Deprecated bool `json:"deprecated,omitempty"`

	// InputTokenCost is the cost per one uncached input token.
	InputTokenCost float64 `json:"input_token_cost,omitempty"`
	// CachedInputTokenCost is the cost per one cache-read input token. Zero keeps
	// the legacy behavior where cache reuse removes those tokens from the estimate.
	CachedInputTokenCost float64 `json:"cached_input_token_cost,omitempty"`
	// CacheWriteTokenCost is the cost per one cache-write input token. Zero falls
	// back to InputTokenCost when RequestProfile.EstimatedCacheWriteTokens is set.
	CacheWriteTokenCost float64 `json:"cache_write_token_cost,omitempty"`
	// OutputTokenCost is the cost per one output token.
	OutputTokenCost float64 `json:"output_token_cost,omitempty"`

	// Priority is an operator-defined preference. Lower values are preferred.
	Priority int `json:"priority,omitempty"`
	// MaxInputTokens is the model context/input window. Estimated output tokens
	// are also reserved against this window when provided. Zero means unknown or unlimited.
	MaxInputTokens int `json:"max_input_tokens,omitempty"`
	// MaxOutputTokens is the model generation/output limit. Zero means unknown or unlimited.
	MaxOutputTokens int `json:"max_output_tokens,omitempty"`
	// ExpectedLatencyMS is the expected end-to-end latency in milliseconds.
	ExpectedLatencyMS int `json:"expected_latency_ms,omitempty"`
	// ExpectedTTFTMS is the expected time-to-first-token in milliseconds.
	ExpectedTTFTMS int `json:"expected_ttft_ms,omitempty"`
}

// RequestProfile describes the request shape used for cost and fit estimates.
type RequestProfile struct {
	// EstimatedInputTokens is the estimated prompt/input token count.
	EstimatedInputTokens int `json:"estimated_input_tokens,omitempty"`
	// EstimatedOutputTokens is the estimated completion/output token count.
	EstimatedOutputTokens int `json:"estimated_output_tokens,omitempty"`
	// EstimatedCacheWriteTokens is the estimated count of input tokens that will
	// be written into a provider prompt cache. Zero means no cache-write charge.
	EstimatedCacheWriteTokens int `json:"estimated_cache_write_tokens,omitempty"`
	// Budget is the maximum acceptable estimated request cost. Zero means no budget limit.
	Budget float64 `json:"budget,omitempty"`
	// Interactive marks latency-sensitive requests where TTFT should influence ranking.
	Interactive bool `json:"interactive,omitempty"`
	// Batch marks throughput-oriented requests where total latency matters less than cost.
	Batch bool `json:"batch,omitempty"`
	// PromptCacheReuseEstimate is the estimated share of input tokens that will be
	// reused from prompt cache. Values outside [0,1] are clamped.
	PromptCacheReuseEstimate float64 `json:"prompt_cache_reuse_estimate,omitempty"`
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
// Prompt cache reuse applies cache-read pricing when the candidate declares it;
// otherwise it preserves the previous behavior of removing cache hits from the
// estimated billable input tokens.
func EstimateCost(candidate Candidate, profile RequestProfile) float64 {
	inputTokens := float64(nonNegative(profile.EstimatedInputTokens))
	cachedInputTokens := inputTokens * clamp01(profile.PromptCacheReuseEstimate)

	cacheWriteTokens := float64(nonNegative(profile.EstimatedCacheWriteTokens))
	if remaining := inputTokens - cachedInputTokens; cacheWriteTokens > remaining {
		cacheWriteTokens = remaining
	}

	uncachedInputTokens := inputTokens - cachedInputTokens - cacheWriteTokens
	if uncachedInputTokens < 0 {
		uncachedInputTokens = 0
	}

	outputTokens := float64(nonNegative(profile.EstimatedOutputTokens))

	cacheWriteCost := candidate.CacheWriteTokenCost
	if cacheWriteCost <= 0 {
		cacheWriteCost = candidate.InputTokenCost
	}

	return uncachedInputTokens*candidate.InputTokenCost +
		cachedInputTokens*candidate.CachedInputTokenCost +
		cacheWriteTokens*cacheWriteCost +
		outputTokens*candidate.OutputTokenCost
}

// FitsContext reports whether candidate has enough declared input and output limits for profile.
func FitsContext(candidate Candidate, profile RequestProfile) bool {
	inputTokens := nonNegative(profile.EstimatedInputTokens)
	outputTokens := nonNegative(profile.EstimatedOutputTokens)

	inputFits := candidate.MaxInputTokens <= 0 ||
		(inputTokens <= candidate.MaxInputTokens && inputTokens+outputTokens <= candidate.MaxInputTokens)
	outputFits := candidate.MaxOutputTokens <= 0 ||
		outputTokens <= candidate.MaxOutputTokens

	return inputFits && outputFits
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
	for i := range candidates {
		candidate := candidates[i]
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
