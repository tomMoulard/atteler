package modelroute

import (
	"maps"
	"sync"
	"time"
)

const (
	// RateLimitScopeProvider marks a rate limit as affecting the whole provider.
	RateLimitScopeProvider = "provider"
	// RateLimitScopeModel marks a rate limit as affecting only the observed model.
	RateLimitScopeModel = "model"
)

// ActualUsage captures provider-reported usage and locally observed timing for
// a completed model call.
type ActualUsage struct {
	Latency           time.Duration
	TTFT              time.Duration
	InputTokens       int
	CachedInputTokens int
	CacheWriteTokens  int
	OutputTokens      int
}

// Failure captures an unsuccessful provider call that routing can learn from.
//
//nolint:govet // Field order keeps timing/error facts grouped for callers.
type Failure struct {
	RetryAfter     time.Duration
	Error          string
	Kind           string
	RateLimitScope string
	Retryable      bool
	RateLimited    bool
}

// Observation is the aggregate telemetry retained for one provider/model.
//
//nolint:govet // Field order matches the external JSON telemetry artifact shape.
type Observation struct {
	UpdatedAt                 time.Time `json:"updated_at"`
	LastFailureAt             time.Time `json:"last_failure_at,omitzero"`
	LastSuccessAt             time.Time `json:"last_success_at,omitzero"`
	Provider                  string    `json:"provider"`
	Model                     string    `json:"model"`
	ModelID                   string    `json:"model_id"`
	Count                     int       `json:"count"`
	FailureCount              int       `json:"failure_count,omitempty"`
	RateLimitCount            int       `json:"rate_limit_count,omitempty"`
	TokenUsageCount           int       `json:"token_usage_count,omitempty"`
	LatencySamples            int       `json:"latency_samples,omitempty"`
	TTFTSamples               int       `json:"ttft_samples,omitempty"`
	InputTokens               int       `json:"input_tokens"`
	CachedInputTokens         int       `json:"cached_input_tokens"`
	CacheWriteTokens          int       `json:"cache_write_tokens"`
	OutputTokens              int       `json:"output_tokens"`
	LastLatencyMS             int       `json:"last_latency_ms"`
	AvgLatencyMS              int       `json:"avg_latency_ms"`
	LastTTFTMS                int       `json:"last_ttft_ms,omitempty"`
	AvgTTFTMS                 int       `json:"avg_ttft_ms,omitempty"`
	LastCacheHitRate          float64   `json:"last_cache_hit_rate"`
	LastCost                  float64   `json:"last_cost"`
	TotalCost                 float64   `json:"total_cost"`
	LastEstimatedDeltaUSD     float64   `json:"last_estimated_delta_usd,omitempty"`
	LastError                 string    `json:"last_error,omitempty"`
	LastFailureKind           string    `json:"last_failure_kind,omitempty"`
	LastFailureRateLimitScope string    `json:"last_failure_rate_limit_scope,omitempty"`
	LastRetryAfterMS          int       `json:"last_retry_after_ms,omitempty"`
	LastFailureRetryable      bool      `json:"last_failure_retryable,omitempty"`
	LastFailureRateLimited    bool      `json:"last_failure_rate_limited,omitempty"`
}

// Telemetry stores in-memory route observations. It is safe for concurrent use.
type Telemetry struct {
	observations map[string]Observation
	mu           sync.RWMutex
}

// NewTelemetry creates an empty route telemetry store.
func NewTelemetry() *Telemetry {
	return &Telemetry{observations: make(map[string]Observation)}
}

// Record stores actual usage for candidate and returns the updated observation.
func (t *Telemetry) Record(candidate Candidate, usage ActualUsage, observedAt time.Time) Observation {
	if t == nil {
		return Observation{}
	}

	if observedAt.IsZero() {
		observedAt = time.Now().UTC()
	}

	id := candidate.ID()
	if id == "" {
		return Observation{}
	}

	latencyMS := durationMS(usage.Latency)
	ttftMS := durationMS(usage.TTFT)

	t.mu.Lock()
	defer t.mu.Unlock()

	if t.observations == nil {
		t.observations = make(map[string]Observation)
	}

	obs := t.observations[id]
	obs.Provider = candidate.Provider
	obs.Model = candidate.Name
	obs.ModelID = id
	obs.UpdatedAt = observedAt
	obs.LastSuccessAt = observedAt
	obs.Count++

	if usageHasTokenCounts(usage) {
		actualCost := EstimateActualCost(candidate, usage)

		obs.TokenUsageCount++
		obs.InputTokens = usage.InputTokens
		obs.CachedInputTokens = usage.CachedInputTokens
		obs.CacheWriteTokens = usage.CacheWriteTokens
		obs.OutputTokens = usage.OutputTokens
		obs.LastCost = actualCost
		obs.TotalCost += actualCost
		obs.LastCacheHitRate = cacheHitRate(usage)

		estimatedCost := EstimateCost(candidate, profileFromActualUsage(usage))
		obs.LastEstimatedDeltaUSD = actualCost - estimatedCost
	}

	if latencyMS > 0 {
		obs.LastLatencyMS = latencyMS
		obs.LatencySamples++
		obs.AvgLatencyMS = runningAverage(obs.AvgLatencyMS, latencyMS, obs.LatencySamples)
	}

	if ttftMS > 0 {
		obs.LastTTFTMS = ttftMS
		obs.TTFTSamples++
		obs.AvgTTFTMS = runningAverage(obs.AvgTTFTMS, ttftMS, obs.TTFTSamples)
	}

	obs.LastError = ""
	obs.LastFailureKind = ""
	obs.LastFailureRateLimitScope = ""
	obs.LastRetryAfterMS = 0
	obs.LastFailureRetryable = false
	obs.LastFailureRateLimited = false

	t.observations[id] = obs

	return obs
}

// RecordFailure stores a failed model call for future route decisions.
func (t *Telemetry) RecordFailure(candidate Candidate, failure Failure, observedAt time.Time) Observation {
	if t == nil {
		return Observation{}
	}

	if observedAt.IsZero() {
		observedAt = time.Now().UTC()
	}

	id := candidate.ID()
	if id == "" {
		return Observation{}
	}

	t.mu.Lock()
	defer t.mu.Unlock()

	if t.observations == nil {
		t.observations = make(map[string]Observation)
	}

	obs := t.observations[id]
	obs.Provider = candidate.Provider
	obs.Model = candidate.Name
	obs.ModelID = id
	obs.UpdatedAt = observedAt
	obs.LastFailureAt = observedAt
	obs.FailureCount++
	obs.LastError = failure.Error
	obs.LastFailureKind = failure.Kind
	obs.LastFailureRetryable = failure.Retryable
	obs.LastFailureRateLimited = failure.RateLimited
	obs.LastFailureRateLimitScope = ""
	obs.LastRetryAfterMS = durationMS(failure.RetryAfter)

	if failure.RateLimited {
		obs.RateLimitCount++
		obs.LastFailureRateLimitScope = normalizeRateLimitScope(failure.RateLimitScope)
	}

	t.observations[id] = obs

	return obs
}

// Snapshot returns the current observation for id.
func (t *Telemetry) Snapshot(id string) (Observation, bool) {
	if t == nil {
		return Observation{}, false
	}

	t.mu.RLock()
	defer t.mu.RUnlock()

	obs, ok := t.observations[id]

	return obs, ok
}

// ProviderRateLimitObservation returns the active rate-limit observation for a
// provider, if any. When multiple models from the same provider are limited, it
// returns the observation with the furthest cooldown deadline.
func (t *Telemetry) ProviderRateLimitObservation(provider string, now time.Time) (Observation, bool) {
	if t == nil || provider == "" {
		return Observation{}, false
	}

	t.mu.RLock()
	defer t.mu.RUnlock()

	var latestProviderSuccess time.Time

	for id := range t.observations {
		obs := t.observations[id]
		if obs.Provider != provider {
			continue
		}

		if obs.LastSuccessAt.After(latestProviderSuccess) {
			latestProviderSuccess = obs.LastSuccessAt
		}
	}

	var selected Observation

	for id := range t.observations {
		obs := t.observations[id]
		if obs.Provider != provider || !obs.providerRateLimitActive(now) {
			continue
		}

		if !latestProviderSuccess.IsZero() && !obs.LastFailureAt.After(latestProviderSuccess) {
			continue
		}

		if selected.RateLimitUntil().Before(obs.RateLimitUntil()) {
			selected = obs
		}
	}

	return selected, !selected.RateLimitUntil().IsZero()
}

// Snapshots returns a copy of all observations keyed by provider/model id.
func (t *Telemetry) Snapshots() map[string]Observation {
	if t == nil {
		return nil
	}

	t.mu.RLock()
	defer t.mu.RUnlock()

	out := make(map[string]Observation, len(t.observations))
	maps.Copy(out, t.observations)

	return out
}

// HasObservations reports whether any route telemetry has been recorded.
func (t *Telemetry) HasObservations() bool {
	if t == nil {
		return false
	}

	t.mu.RLock()
	defer t.mu.RUnlock()

	return len(t.observations) > 0
}

// EstimateActualCost calculates cost from provider-reported token usage.
func EstimateActualCost(candidate Candidate, usage ActualUsage) float64 {
	inputTokens := nonNegative(usage.InputTokens)
	cachedTokens := nonNegative(usage.CachedInputTokens)
	cachedTokens = min(cachedTokens, inputTokens)

	cacheWriteTokens := nonNegative(usage.CacheWriteTokens)
	cacheWriteTokens = min(cacheWriteTokens, inputTokens-cachedTokens)

	uncachedInputTokens := max(inputTokens-cachedTokens-cacheWriteTokens, 0)

	cacheWriteCost := candidate.CacheWriteTokenCost
	if cacheWriteCost <= 0 {
		cacheWriteCost = candidate.InputTokenCost
	}

	return float64(uncachedInputTokens)*candidate.InputTokenCost +
		float64(cachedTokens)*candidate.CachedInputTokenCost +
		float64(cacheWriteTokens)*cacheWriteCost +
		float64(nonNegative(usage.OutputTokens))*candidate.OutputTokenCost
}

func usageHasTokenCounts(usage ActualUsage) bool {
	return nonNegative(usage.InputTokens) > 0 ||
		nonNegative(usage.CachedInputTokens) > 0 ||
		nonNegative(usage.CacheWriteTokens) > 0 ||
		nonNegative(usage.OutputTokens) > 0
}

func profileFromActualUsage(usage ActualUsage) RequestProfile {
	inputTokens := nonNegative(usage.InputTokens)
	cachedTokens := min(nonNegative(usage.CachedInputTokens), inputTokens)

	profile := RequestProfile{
		EstimatedInputTokens:      inputTokens,
		EstimatedOutputTokens:     nonNegative(usage.OutputTokens),
		EstimatedCacheWriteTokens: nonNegative(usage.CacheWriteTokens),
	}
	if inputTokens > 0 && cachedTokens > 0 {
		profile.PromptCacheReuseEstimate = float64(cachedTokens) / float64(inputTokens)
	}

	return profile
}

func cacheHitRate(usage ActualUsage) float64 {
	inputTokens := nonNegative(usage.InputTokens)
	if inputTokens == 0 {
		return 0
	}

	cachedTokens := min(nonNegative(usage.CachedInputTokens), inputTokens)

	return float64(cachedTokens) / float64(inputTokens)
}

func durationMS(d time.Duration) int {
	if d <= 0 {
		return 0
	}

	ms := d.Milliseconds()
	if ms <= 0 {
		return 1
	}

	return int(ms)
}

func runningAverage(previous, next, count int) int {
	if count <= 1 {
		return next
	}

	return (previous*(count-1) + next) / count
}

// RateLimitUntil returns the time until which the last observed rate limit
// should influence routing. Provider Retry-After wins; otherwise a conservative
// short cooldown prevents a permanent ban from one missing-header 429.
func (o Observation) RateLimitUntil() time.Time {
	if !o.LastFailureRateLimited || o.LastFailureAt.IsZero() {
		return time.Time{}
	}

	cooldown := defaultRateLimitCooldown
	if o.LastRetryAfterMS > 0 {
		cooldown = time.Duration(o.LastRetryAfterMS) * time.Millisecond
	}

	return o.LastFailureAt.Add(cooldown)
}

// RateLimitActive reports whether a previous rate limit should still reject the
// model at now.
func (o Observation) RateLimitActive(now time.Time) bool {
	until := o.RateLimitUntil()
	if until.IsZero() {
		return false
	}

	if now.IsZero() {
		now = time.Now().UTC()
	}

	return now.Before(until)
}

func (o Observation) providerRateLimitActive(now time.Time) bool {
	return o.RateLimitActive(now) && o.rateLimitScope() == RateLimitScopeProvider
}

func (o Observation) rateLimitScope() string {
	if !o.LastFailureRateLimited {
		return ""
	}

	if o.LastFailureRateLimitScope == RateLimitScopeModel {
		return RateLimitScopeModel
	}

	return RateLimitScopeProvider
}

func normalizeRateLimitScope(scope string) string {
	if scope == RateLimitScopeModel {
		return RateLimitScopeModel
	}

	return RateLimitScopeProvider
}
