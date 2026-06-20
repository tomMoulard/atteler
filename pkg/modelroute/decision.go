package modelroute

import (
	"fmt"
	"maps"
	"math"
	"slices"
	"sort"
	"strings"
	"time"
)

const (
	defaultRateLimitCooldown = time.Minute

	// StatusSelected means the candidate is the chosen primary model.
	StatusSelected = "selected"
	// StatusFallback means the candidate is eligible after the primary model.
	StatusFallback = "fallback"
	// StatusRejected means the candidate failed one or more route constraints.
	StatusRejected = "rejected"

	// ReasonMetadataStale means the route used a catalog past its freshness horizon.
	ReasonMetadataStale = "metadata stale"
	// ReasonUnknownMetadata means no catalog entry exists for a requested model.
	ReasonUnknownMetadata = "unknown model metadata"
	// ReasonAmbiguousMetadata means an unqualified model id matched multiple catalog entries.
	ReasonAmbiguousMetadata = "ambiguous model metadata"
	// ReasonProviderBanned means the candidate provider is blocked by policy.
	ReasonProviderBanned = "provider banned by policy"
	// ReasonModelBanned means the candidate model is blocked by policy.
	ReasonModelBanned = "model banned by policy"
	// ReasonMissingCapability means the candidate lacks a policy-required capability.
	ReasonMissingCapability = "required capability missing"
	// ReasonContextOverflow means the estimated input or output exceeds a model limit.
	ReasonContextOverflow = "context overflow"
	// ReasonOverBudget means the estimated cost exceeds the effective route budget.
	ReasonOverBudget = "over budget"
	// ReasonCostUnknown means a budget was configured but no price metadata exists.
	ReasonCostUnknown = "cost metadata unavailable"
	// ReasonProviderUnavailable means the runtime registry cannot serve this provider.
	ReasonProviderUnavailable = "provider unavailable"
	// ReasonModelUnavailable means the runtime registry cannot resolve this model.
	ReasonModelUnavailable = "model unavailable"
	// ReasonModelUnverified means the runtime registry can route the model by provider but has not indexed that model id.
	ReasonModelUnverified = "model availability not verified"
	// ReasonRateLimited means recent telemetry observed a provider rate limit.
	ReasonRateLimited = "recent rate limit"
	// ReasonLatencyExceeded means observed or expected end-to-end latency exceeded policy.
	ReasonLatencyExceeded = "latency limit exceeded"
	// ReasonTTFTExceeded means observed or expected time-to-first-token exceeded policy.
	ReasonTTFTExceeded = "ttft limit exceeded"

	// ConstraintCatalogMetadata records that candidates were resolved through a maintained catalog.
	ConstraintCatalogMetadata = "catalog_metadata"
	// ConstraintMetadataFreshness records that the catalog freshness horizon was evaluated.
	ConstraintMetadataFreshness = "metadata_freshness"
	// ConstraintContextWindow records that estimated input tokens were checked against context windows.
	ConstraintContextWindow = "context_window"
	// ConstraintOutputLimit records that estimated output tokens were checked against generation limits.
	ConstraintOutputLimit = "output_limit"
	// ConstraintEstimatedCost records that request cost estimates influenced the decision.
	ConstraintEstimatedCost = "estimated_cost"
	// ConstraintBudget records that a request or policy budget cap was applied.
	ConstraintBudget = "budget"
	// ConstraintRoutingPolicy records that at least one per-agent routing policy field was applied.
	ConstraintRoutingPolicy = "routing_policy"
	// ConstraintRequiredCapabilities records that policy-required model capabilities were checked.
	ConstraintRequiredCapabilities = "required_capabilities"
	// ConstraintProviderPreference records that preferred providers influenced ranking.
	ConstraintProviderPreference = "provider_preference"
	// ConstraintObservedTelemetry records that observed route telemetry influenced ranking or rejection.
	ConstraintObservedTelemetry = "observed_telemetry"
	// ConstraintRuntimeAvailability records that the live registry constrained provider/model choices.
	ConstraintRuntimeAvailability = "runtime_availability"
	// ConstraintLatency records that expected or observed latency influenced ranking.
	ConstraintLatency = "latency"
	// ConstraintTTFT records that time-to-first-token influenced interactive ranking.
	ConstraintTTFT = "ttft"
)

// Policy captures agent/request routing preferences that do not belong in CLI
// arithmetic flags. Empty fields are disabled.
type Policy struct {
	// PreferredProviders ranks providers before cost/latency tie-breakers.
	PreferredProviders []string `json:"preferred_providers,omitempty"`
	// BannedProviders rejects every candidate from these providers.
	BannedProviders []string `json:"banned_providers,omitempty"`
	// BannedModels rejects provider-qualified ids or provider-local model names.
	BannedModels []string `json:"banned_models,omitempty"`
	// RequiredCapabilities rejects candidates that do not advertise every listed capability.
	RequiredCapabilities []string `json:"required_capabilities,omitempty"`
	// MaxBudget caps the effective request budget, even when the CLI/caller did not set one.
	MaxBudget float64 `json:"max_budget,omitempty"`
	// MaxLatencyMS rejects candidates with observed or expected end-to-end latency above this limit.
	MaxLatencyMS int `json:"max_latency_ms,omitempty"`
	// MaxTTFTMS rejects candidates with observed or expected time-to-first-token above this limit.
	MaxTTFTMS int `json:"max_ttft_ms,omitempty"`
	// RequireFreshMetadata rejects catalog-backed routes when the metadata snapshot is stale.
	RequireFreshMetadata bool `json:"require_fresh_metadata,omitempty"`
}

// Decision is the inspectable route artifact. It records every candidate that
// was considered, why candidates were rejected, and the final fallback order.
//
//nolint:govet // Field order matches the external JSON artifact shape.
type Decision struct {
	CatalogVersion string              `json:"catalog_version,omitempty"`
	CatalogStale   bool                `json:"catalog_stale,omitempty"`
	Warnings       []string            `json:"warnings,omitempty"`
	Availability   *Availability       `json:"availability,omitempty"`
	ModelRole      string              `json:"model_role,omitempty"`
	Profile        RequestProfile      `json:"profile"`
	Policy         Policy              `json:"policy,omitzero"`
	Constraints    []string            `json:"constraints_applied,omitempty"`
	Candidates     []CandidateDecision `json:"candidates"`
	Selected       string              `json:"selected,omitempty"`
	ActualSelected string              `json:"actual_selected,omitempty"`
	FallbackOrder  []string            `json:"fallback_order,omitempty"`
}

// CandidateDecision explains one candidate's outcome inside a Decision.
//
//nolint:govet // Field order matches the external JSON artifact shape.
type CandidateDecision struct {
	Candidate                 Candidate `json:"candidate,omitzero"`
	ID                        string    `json:"id"`
	Status                    string    `json:"status"`
	Rejected                  []string  `json:"rejected,omitempty"`
	EstimatedCost             float64   `json:"estimated_cost"`
	ActualUsageRecorded       bool      `json:"actual_usage_recorded,omitempty"`
	ActualCost                float64   `json:"actual_cost,omitempty"`
	ActualCostDelta           float64   `json:"actual_cost_delta,omitempty"`
	ActualInputTokens         int       `json:"actual_input_tokens,omitempty"`
	ActualCachedTokens        int       `json:"actual_cached_input_tokens,omitempty"`
	ActualCacheWrites         int       `json:"actual_cache_write_tokens,omitempty"`
	ActualCacheHitRate        float64   `json:"actual_cache_hit_rate,omitempty"`
	ActualOutputTokens        int       `json:"actual_output_tokens,omitempty"`
	ExpectedLatencyMS         int       `json:"expected_latency_ms,omitempty"`
	ExpectedTTFTMS            int       `json:"expected_ttft_ms,omitempty"`
	ObservedLatencyMS         int       `json:"observed_latency_ms,omitempty"`
	ObservedTTFTMS            int       `json:"observed_ttft_ms,omitempty"`
	TelemetryCount            int       `json:"telemetry_count,omitempty"`
	FailureCount              int       `json:"failure_count,omitempty"`
	RateLimitCount            int       `json:"rate_limit_count,omitempty"`
	LastRetryAfterMS          int       `json:"last_retry_after_ms,omitempty"`
	RateLimitUntil            string    `json:"rate_limit_until,omitempty"`
	LastError                 string    `json:"last_error,omitempty"`
	LastFailureKind           string    `json:"last_failure_kind,omitempty"`
	LastFailureRateLimitScope string    `json:"last_failure_rate_limit_scope,omitempty"`
	Rank                      int       `json:"rank,omitempty"`
}

// Availability records runtime provider/model evidence used to reject candidates
// that the configured registry cannot currently resolve.
//
//nolint:govet // Field order follows the external JSON artifact shape.
type Availability struct {
	Checked                bool                `json:"checked,omitempty"`
	RefreshAttempted       bool                `json:"refresh_attempted,omitempty"`
	RefreshTimeoutMS       int                 `json:"refresh_timeout_ms,omitempty"`
	Providers              []string            `json:"providers,omitempty"`
	Models                 []string            `json:"models,omitempty"`
	ProviderModels         map[string][]string `json:"provider_models,omitempty"`
	ProviderModelsVerified map[string]bool     `json:"provider_models_verified,omitempty"`
	Unavailable            map[string]string   `json:"unavailable,omitempty"`
	Unverified             map[string]string   `json:"unverified,omitempty"`
}

// Decide ranks candidates and returns an inspectable decision artifact.
func Decide(candidates []Candidate, profile RequestProfile, policy Policy, telemetry *Telemetry) Decision {
	return DecideAt(candidates, profile, policy, telemetry, time.Now().UTC())
}

// DecideAt ranks candidates using now for time-bound telemetry decisions.
func DecideAt(candidates []Candidate, profile RequestProfile, policy Policy, telemetry *Telemetry, now time.Time) Decision {
	if now.IsZero() {
		now = time.Now().UTC()
	}

	policy = normalizePolicy(policy)
	effectiveProfile := policy.applyBudget(profile)
	decision := Decision{
		Profile:     effectiveProfile,
		Policy:      policy,
		Constraints: baseConstraints(effectiveProfile, policy),
	}

	type acceptedCandidate struct {
		candidate Candidate
		index     int
	}

	accepted := make([]acceptedCandidate, 0, len(candidates))
	observedAny := false
	latencyEvidence := false
	ttftEvidence := false

	for i := range candidates {
		evaluated := evaluateCandidate(candidates[i], effectiveProfile, policy, telemetry, now)
		observedAny = observedAny || evaluated.observed
		latencyEvidence = latencyEvidence || evaluated.latencyEvidence
		ttftEvidence = ttftEvidence || evaluated.ttftEvidence

		if len(evaluated.decision.Rejected) == 0 {
			accepted = append(accepted, acceptedCandidate{candidate: evaluated.candidate, index: i})
		}

		decision.Candidates = append(decision.Candidates, evaluated.decision)
	}

	decision.Constraints = appendEvidenceConstraints(decision.Constraints, effectiveProfile, observedAny, latencyEvidence, ttftEvidence)

	sort.SliceStable(accepted, func(i, j int) bool {
		return betterWithPolicy(accepted[i].candidate, accepted[j].candidate, effectiveProfile, policy)
	})

	for rank := range accepted {
		acceptedCandidate := accepted[rank]
		id := acceptedCandidate.candidate.ID()

		decision.FallbackOrder = append(decision.FallbackOrder, id)
		if rank == 0 {
			decision.Selected = id
		}

		cd := &decision.Candidates[acceptedCandidate.index]

		cd.Rank = rank + 1
		if rank == 0 {
			cd.Status = StatusSelected
			continue
		}

		cd.Status = StatusFallback
	}

	return decision
}

//nolint:govet // Field order follows the decision-building data flow.
type evaluatedCandidate struct {
	candidate       Candidate
	decision        CandidateDecision
	observed        bool
	latencyEvidence bool
	ttftEvidence    bool
}

func evaluateCandidate(candidate Candidate, profile RequestProfile, policy Policy, telemetry *Telemetry, now time.Time) evaluatedCandidate {
	obs, observed := telemetry.Snapshot(candidate.ID())
	providerRateLimitObs, providerRateLimited := telemetry.ProviderRateLimitObservation(candidate.Provider, now)
	cd := CandidateDecision{
		Candidate:         candidate,
		ID:                candidate.ID(),
		Status:            StatusRejected,
		EstimatedCost:     EstimateCost(candidate, profile),
		ExpectedLatencyMS: candidate.ExpectedLatencyMS,
		ExpectedTTFTMS:    candidate.ExpectedTTFTMS,
	}
	latencyEvidence := candidate.ExpectedLatencyMS > 0
	ttftEvidence := candidate.ExpectedTTFTMS > 0

	if observed {
		latencyEvidence = latencyEvidence || obs.AvgLatencyMS > 0
		ttftEvidence = ttftEvidence || obs.AvgTTFTMS > 0
		applyObservation(&cd, obs)

		candidate = candidateWithObservation(candidate, obs)
	}

	cd.Rejected = rejectionReasons(candidate, profile, policy)

	if rateLimitObs, rateLimited := activeRateLimitObservation(obs, observed, providerRateLimitObs, providerRateLimited, now); rateLimited {
		observed = true

		applyFailureObservation(&cd, rateLimitObs)
		cd.RateLimitUntil = rateLimitObs.RateLimitUntil().Format(time.RFC3339)
		cd.Rejected = append(cd.Rejected, rateLimitReason(rateLimitObs))
	}

	return evaluatedCandidate{
		candidate:       candidate,
		decision:        cd,
		observed:        observed,
		latencyEvidence: latencyEvidence,
		ttftEvidence:    ttftEvidence,
	}
}

func activeRateLimitObservation(
	modelObs Observation,
	modelObserved bool,
	providerObs Observation,
	providerObserved bool,
	now time.Time,
) (Observation, bool) {
	var selected Observation
	if modelObserved && modelObs.RateLimitActive(now) {
		selected = modelObs
	}

	if providerObserved && providerObs.RateLimitActive(now) && selected.RateLimitUntil().Before(providerObs.RateLimitUntil()) {
		selected = providerObs
	}

	return selected, !selected.RateLimitUntil().IsZero()
}

func applyFailureObservation(candidate *CandidateDecision, obs Observation) {
	candidate.FailureCount = obs.FailureCount
	candidate.RateLimitCount = obs.RateLimitCount
	candidate.LastRetryAfterMS = obs.LastRetryAfterMS
	candidate.LastError = obs.LastError
	candidate.LastFailureKind = obs.LastFailureKind
	candidate.LastFailureRateLimitScope = obs.LastFailureRateLimitScope
}

func applyObservation(candidate *CandidateDecision, obs Observation) {
	if obs.TokenUsageCount > 0 {
		candidate.ActualUsageRecorded = true
		candidate.ActualCostDelta = obs.LastCost - candidate.EstimatedCost
		candidate.ActualInputTokens = obs.InputTokens
		candidate.ActualCachedTokens = obs.CachedInputTokens
		candidate.ActualCacheWrites = obs.CacheWriteTokens
		candidate.ActualCacheHitRate = obs.LastCacheHitRate
		candidate.ActualOutputTokens = obs.OutputTokens
	}

	candidate.ActualCost = obs.LastCost
	candidate.ObservedLatencyMS = obs.AvgLatencyMS
	candidate.ObservedTTFTMS = obs.AvgTTFTMS
	candidate.TelemetryCount = obs.Count
	applyFailureObservation(candidate, obs)

	if rateLimitUntil := obs.RateLimitUntil(); !rateLimitUntil.IsZero() {
		candidate.RateLimitUntil = rateLimitUntil.Format(time.RFC3339)
	}
}

func rateLimitReason(obs Observation) string {
	if obs.LastRetryAfterMS > 0 {
		return fmt.Sprintf("%s: retry_after_ms=%d until=%s", ReasonRateLimited, obs.LastRetryAfterMS, obs.RateLimitUntil().Format(time.RFC3339))
	}

	return fmt.Sprintf("%s: until=%s", ReasonRateLimited, obs.RateLimitUntil().Format(time.RFC3339))
}

// DecisionWithAvailability returns a decision annotated with runtime provider
// availability and rejects candidates listed in availability.Unavailable. When
// availability marks a non-catalog id, provider-reported alias, or
// provider-local model name unavailable, that evidence is matched back to the
// canonical candidate id where possible.
func DecisionWithAvailability(decision Decision, availability Availability) Decision {
	decision = cloneDecision(decision)
	if !availability.Checked {
		return decision
	}

	availability = cloneAvailability(availability)
	decision.Availability = &availability

	decision.Constraints = appendUnique(decision.Constraints, ConstraintRuntimeAvailability)
	if len(availability.Unavailable) == 0 {
		return decision
	}

	bareIDCounts := candidateBareAvailabilityIDCounts(decision.Candidates)

	for i := range decision.Candidates {
		candidate := &decision.Candidates[i]

		reason, ok := availabilityRejectionReason(*candidate, availability.Unavailable, bareIDCounts)
		if !ok {
			continue
		}

		candidate.Status = StatusRejected
		candidate.Rank = 0
		candidate.Rejected = appendUnique(candidate.Rejected, reason)
	}

	return rerankDecision(decision)
}

func availabilityRejectionReason(candidate CandidateDecision, unavailable map[string]string, bareIDCounts map[string]int) (string, bool) {
	for _, id := range candidateAvailabilityIDs(candidate, bareIDCounts) {
		if reason, ok := unavailable[id]; ok {
			return reason, true
		}
	}

	return "", false
}

func candidateAvailabilityIDs(candidate CandidateDecision, bareIDCounts map[string]int) []string {
	ids := []string{
		strings.TrimSpace(candidate.ID),
		strings.TrimSpace(candidate.Candidate.ID()),
	}

	if unambiguousBareAvailabilityID(candidate.Candidate.Name, bareIDCounts) {
		ids = append(ids, strings.TrimSpace(candidate.Candidate.Name))
	}

	for _, alias := range candidate.Candidate.Aliases {
		alias = strings.TrimSpace(alias)
		if alias == "" {
			continue
		}

		if candidate.Candidate.Provider != "" && !strings.Contains(alias, "/") {
			ids = append(ids, candidate.Candidate.Provider+"/"+alias)
		}

		if strings.Contains(alias, "/") || unambiguousBareAvailabilityID(alias, bareIDCounts) {
			ids = append(ids, alias)
		}
	}

	return uniqueNonEmptyStrings(ids)
}

func candidateBareAvailabilityIDCounts(candidates []CandidateDecision) map[string]int {
	counts := make(map[string]int)

	for i := range candidates {
		for _, id := range candidateBareAvailabilityIDs(candidates[i]) {
			counts[normalizeModelID(id)]++
		}
	}

	return counts
}

func candidateBareAvailabilityIDs(candidate CandidateDecision) []string {
	ids := []string{candidate.Candidate.Name}
	for _, alias := range candidate.Candidate.Aliases {
		if strings.Contains(alias, "/") {
			continue
		}

		ids = append(ids, alias)
	}

	return uniqueNonEmptyStrings(ids)
}

func unambiguousBareAvailabilityID(id string, counts map[string]int) bool {
	id = normalizeModelID(id)
	if id == "" {
		return false
	}

	return counts[id] == 1
}

func rerankDecision(decision Decision) Decision {
	previousOrder := append([]string(nil), decision.FallbackOrder...)
	decision.Selected = ""
	decision.FallbackOrder = nil

	rank := 1

	for _, id := range previousOrder {
		candidate := candidateDecisionByID(decision.Candidates, id)
		if candidate == nil || candidate.Status == StatusRejected || len(candidate.Rejected) > 0 {
			continue
		}

		candidate.Rank = rank
		candidate.Status = StatusFallback

		decision.FallbackOrder = append(decision.FallbackOrder, id)

		if rank == 1 {
			candidate.Status = StatusSelected
			decision.Selected = id
		}

		rank++
	}

	return decision
}

func candidateDecisionByID(candidates []CandidateDecision, id string) *CandidateDecision {
	for i := range candidates {
		if candidates[i].ID == id {
			return &candidates[i]
		}
	}

	return nil
}

func candidateWithObservation(candidate Candidate, obs Observation) Candidate {
	if obs.AvgLatencyMS > 0 {
		candidate.ExpectedLatencyMS = obs.AvgLatencyMS
	}

	if obs.AvgTTFTMS > 0 {
		candidate.ExpectedTTFTMS = obs.AvgTTFTMS
	}

	return candidate
}

// DecisionWithTelemetry returns a copy of decision refreshed with the latest
// telemetry evidence for every candidate. It intentionally does not rerank the
// decision: callers use this for post-call artifacts where the original
// estimated selection and fallback order should remain inspectable.
func DecisionWithTelemetry(decision Decision, telemetry *Telemetry) Decision {
	decision = cloneDecision(decision)
	if telemetry == nil {
		return decision
	}

	now := time.Now().UTC()
	observedAny := false
	latencyEvidence := false
	ttftEvidence := false

	for i := range decision.Candidates {
		candidate := &decision.Candidates[i]

		obs, ok := telemetry.Snapshot(candidate.ID)
		providerObs, providerOK := telemetry.ProviderRateLimitObservation(candidate.Candidate.Provider, now)

		if ok {
			observedAny = true
			latencyEvidence = latencyEvidence || obs.AvgLatencyMS > 0
			ttftEvidence = ttftEvidence || obs.AvgTTFTMS > 0

			applyObservation(candidate, obs)
		}

		if rateLimitObs, rateLimited := activeRateLimitObservation(obs, ok, providerObs, providerOK, now); rateLimited {
			observedAny = true

			applyFailureObservation(candidate, rateLimitObs)
			candidate.RateLimitUntil = rateLimitObs.RateLimitUntil().Format(time.RFC3339)
		}
	}

	decision.Constraints = appendEvidenceConstraints(decision.Constraints, decision.Profile, observedAny, latencyEvidence, ttftEvidence)

	return decision
}

// DecisionWithActualUsage returns a copy of decision annotated with actual
// provider-reported usage for the selected model. modelID may be either the
// route candidate id or the provider-reported response model; an empty value
// falls back to decision.Selected.
func DecisionWithActualUsage(decision Decision, modelID string, usage ActualUsage) Decision {
	decision = cloneDecision(decision)

	fallbackToPlannedSelection := modelID == ""
	if modelID == "" {
		modelID = decision.Selected
	}

	modelID = normalizeModelID(modelID)
	allowBareMatch := actualUsageAllowsBareModelMatch(decision.Candidates, modelID)

	for i := range decision.Candidates {
		candidate := &decision.Candidates[i]
		if !candidateMatches(candidate, modelID, fallbackToPlannedSelection, allowBareMatch) {
			continue
		}

		annotateActualUsage(&decision, candidate, usage)

		return decision
	}

	if !fallbackToPlannedSelection {
		if candidate := uniqueProviderCandidate(decision.Candidates, modelID); candidate != nil {
			annotateActualUsage(&decision, candidate, usage)

			return decision
		}
	}

	return decision
}

func actualUsageAllowsBareModelMatch(candidates []CandidateDecision, modelID string) bool {
	if strings.Contains(modelID, "/") {
		return true
	}

	return unambiguousBareAvailabilityID(modelID, candidateBareAvailabilityIDCounts(candidates))
}

func annotateActualUsage(decision *Decision, candidate *CandidateDecision, usage ActualUsage) {
	decision.ActualSelected = candidate.ID

	if usageHasTokenCounts(usage) {
		candidate.ActualUsageRecorded = true
		candidate.ActualCost = EstimateActualCost(candidate.Candidate, usage)
		candidate.ActualCostDelta = candidate.ActualCost - candidate.EstimatedCost
		candidate.ActualInputTokens = usage.InputTokens
		candidate.ActualCachedTokens = usage.CachedInputTokens
		candidate.ActualCacheWrites = usage.CacheWriteTokens
		candidate.ActualCacheHitRate = cacheHitRate(usage)
		candidate.ActualOutputTokens = usage.OutputTokens
	}

	if latencyMS := durationMS(usage.Latency); latencyMS > 0 {
		candidate.ObservedLatencyMS = latencyMS
	}

	if ttftMS := durationMS(usage.TTFT); ttftMS > 0 {
		candidate.ObservedTTFTMS = ttftMS
	}
}

func candidateMatches(candidate *CandidateDecision, modelID string, fallbackToPlannedSelection, allowBareMatch bool) bool {
	if fallbackToPlannedSelection {
		return candidate.Status == StatusSelected
	}

	if normalizeModelID(candidate.ID) == modelID ||
		normalizeModelID(candidate.Candidate.ID()) == modelID {
		return true
	}

	if allowBareMatch && normalizeModelID(candidate.Candidate.Name) == modelID {
		return true
	}

	for _, alias := range candidate.Candidate.Aliases {
		if (strings.Contains(alias, "/") || allowBareMatch) && normalizeModelID(alias) == modelID {
			return true
		}

		if candidate.Candidate.Provider != "" &&
			!strings.Contains(alias, "/") &&
			normalizeModelID(candidate.Candidate.Provider+"/"+alias) == modelID {
			return true
		}
	}

	return false
}

func uniqueProviderCandidate(candidates []CandidateDecision, modelID string) *CandidateDecision {
	provider, model := splitID(modelID)
	if provider == "" || model == "" {
		return nil
	}

	provider = normalize(provider)
	if provider == "" {
		return nil
	}

	var match *CandidateDecision

	for i := range candidates {
		if normalize(candidates[i].Candidate.Provider) != provider {
			continue
		}

		if match != nil {
			return nil
		}

		match = &candidates[i]
	}

	return match
}

func cloneDecision(decision Decision) Decision {
	decision.Warnings = append([]string(nil), decision.Warnings...)
	decision.Constraints = append([]string(nil), decision.Constraints...)
	decision.FallbackOrder = append([]string(nil), decision.FallbackOrder...)
	decision.Policy = clonePolicy(decision.Policy)

	decision.Candidates = append([]CandidateDecision(nil), decision.Candidates...)
	for i := range decision.Candidates {
		decision.Candidates[i].Rejected = append([]string(nil), decision.Candidates[i].Rejected...)
		decision.Candidates[i].Candidate = cloneCandidate(decision.Candidates[i].Candidate)
	}

	if decision.Availability != nil {
		availability := cloneAvailability(*decision.Availability)
		decision.Availability = &availability
	}

	return decision
}

func clonePolicy(policy Policy) Policy {
	policy.PreferredProviders = append([]string(nil), policy.PreferredProviders...)
	policy.BannedProviders = append([]string(nil), policy.BannedProviders...)
	policy.BannedModels = append([]string(nil), policy.BannedModels...)
	policy.RequiredCapabilities = append([]string(nil), policy.RequiredCapabilities...)

	return policy
}

func cloneCandidate(candidate Candidate) Candidate {
	candidate.Capabilities = append([]string(nil), candidate.Capabilities...)
	candidate.Aliases = append([]string(nil), candidate.Aliases...)

	return candidate
}

func cloneAvailability(availability Availability) Availability {
	availability.Providers = append([]string(nil), availability.Providers...)
	availability.Models = append([]string(nil), availability.Models...)
	availability.ProviderModels = cloneStringSlicesMap(availability.ProviderModels)
	availability.ProviderModelsVerified = cloneBoolMap(availability.ProviderModelsVerified)
	availability.Unavailable = cloneStringMap(availability.Unavailable)
	availability.Unverified = cloneStringMap(availability.Unverified)

	return availability
}

func cloneStringMap(in map[string]string) map[string]string {
	if len(in) == 0 {
		return nil
	}

	out := make(map[string]string, len(in))
	maps.Copy(out, in)

	return out
}

func cloneBoolMap(in map[string]bool) map[string]bool {
	if len(in) == 0 {
		return nil
	}

	out := make(map[string]bool, len(in))
	maps.Copy(out, in)

	return out
}

func cloneStringSlicesMap(in map[string][]string) map[string][]string {
	if len(in) == 0 {
		return nil
	}

	out := make(map[string][]string, len(in))
	for key, values := range in {
		out[key] = append([]string(nil), values...)
	}

	return out
}

// DecideFromCatalog resolves ids through catalog before ranking. Unknown ids are
// preserved in the artifact as rejected candidates so the operator can see
// exactly which requested models were missing maintained metadata.
func DecideFromCatalog(catalog Catalog, ids []string, profile RequestProfile, policy Policy, telemetry *Telemetry, now time.Time) Decision {
	candidates, unknown := catalog.Candidates(ids)
	decision := DecideAt(candidates, profile, policy, telemetry, now)

	decision.CatalogVersion = catalog.Version
	decision.Constraints = appendUnique(decision.Constraints, ConstraintCatalogMetadata)
	decision.Constraints = appendUnique(decision.Constraints, ConstraintMetadataFreshness)

	if catalog.IsStale(now) {
		decision.CatalogStale = true
		decision.Warnings = append(decision.Warnings, fmt.Sprintf("%s: catalog %s stale after %s", ReasonMetadataStale, catalog.Version, catalog.StaleAfter.Format(time.RFC3339)))

		if policy.RequireFreshMetadata {
			decision.Selected = ""
			decision.FallbackOrder = nil

			for i := range decision.Candidates {
				decision.Candidates[i].Status = StatusRejected
				decision.Candidates[i].Rejected = appendUnique(decision.Candidates[i].Rejected, ReasonMetadataStale)
				decision.Candidates[i].Rank = 0
			}
		}
	}

	for _, id := range unknown {
		decision.Candidates = append(decision.Candidates, CandidateDecision{
			ID:       strings.TrimSpace(id),
			Status:   StatusRejected,
			Rejected: []string{catalog.candidateFailureReason(id)},
		})
	}

	return decision
}

func rejectionReasons(candidate Candidate, profile RequestProfile, policy Policy) []string {
	var reasons []string
	if containsFold(policy.BannedProviders, candidate.Provider) {
		reasons = append(reasons, ReasonProviderBanned)
	}

	if containsModel(policy.BannedModels, candidate) {
		reasons = append(reasons, ReasonModelBanned)
	}

	if missing := missingCapabilities(candidate.Capabilities, policy.RequiredCapabilities); len(missing) > 0 {
		reasons = append(reasons, ReasonMissingCapability+": "+strings.Join(missing, ","))
	}

	reasons = append(reasons, contextRejectionReasons(candidate, profile)...)
	reasons = append(reasons, latencyRejectionReasons(candidate, policy)...)

	if profile.Budget > 0 && !HasCostEstimateForProfile(candidate, profile) {
		reasons = append(reasons, ReasonCostUnknown)
	} else if !FitsBudget(candidate, profile) {
		reasons = append(reasons, fmt.Sprintf("%s: estimated cost %.6f > budget %.6f", ReasonOverBudget, EstimateCost(candidate, profile), profile.Budget))
	}

	return reasons
}

func latencyRejectionReasons(candidate Candidate, policy Policy) []string {
	var reasons []string

	if policy.MaxLatencyMS > 0 && candidate.ExpectedLatencyMS > policy.MaxLatencyMS {
		reasons = append(reasons, fmt.Sprintf(
			"%s: %dms > %dms",
			ReasonLatencyExceeded,
			candidate.ExpectedLatencyMS,
			policy.MaxLatencyMS,
		))
	}

	if policy.MaxTTFTMS > 0 && candidate.ExpectedTTFTMS > policy.MaxTTFTMS {
		reasons = append(reasons, fmt.Sprintf(
			"%s: %dms > %dms",
			ReasonTTFTExceeded,
			candidate.ExpectedTTFTMS,
			policy.MaxTTFTMS,
		))
	}

	return reasons
}

func contextRejectionReasons(candidate Candidate, profile RequestProfile) []string {
	var reasons []string

	inputTokens := nonNegative(profile.EstimatedInputTokens)

	outputTokens := nonNegative(profile.EstimatedOutputTokens)

	if candidate.MaxInputTokens > 0 && inputTokens > candidate.MaxInputTokens {
		reasons = append(reasons, fmt.Sprintf(
			"%s: estimated input %d > input limit %d",
			ReasonContextOverflow,
			inputTokens,
			candidate.MaxInputTokens,
		))
	} else if candidate.MaxInputTokens > 0 && inputTokens+outputTokens > candidate.MaxInputTokens {
		reasons = append(reasons, fmt.Sprintf(
			"%s: estimated input+output %d > context limit %d",
			ReasonContextOverflow,
			inputTokens+outputTokens,
			candidate.MaxInputTokens,
		))
	}

	if candidate.MaxOutputTokens > 0 && outputTokens > candidate.MaxOutputTokens {
		reasons = append(reasons, fmt.Sprintf(
			"%s: estimated output %d > output limit %d",
			ReasonContextOverflow,
			outputTokens,
			candidate.MaxOutputTokens,
		))
	}

	return reasons
}

func baseConstraints(profile RequestProfile, policy Policy) []string {
	constraints := []string{
		ConstraintContextWindow,
	}

	if nonNegative(profile.EstimatedOutputTokens) > 0 {
		constraints = append(constraints, ConstraintOutputLimit)
	}

	constraints = append(constraints, ConstraintEstimatedCost)

	if profile.Budget > 0 {
		constraints = append(constraints, ConstraintBudget)
	}

	if routePolicyHasConstraints(policy) {
		constraints = append(constraints, ConstraintRoutingPolicy)
	}

	if len(policy.RequiredCapabilities) > 0 {
		constraints = append(constraints, ConstraintRequiredCapabilities)
	}

	if len(policy.PreferredProviders) > 0 {
		constraints = append(constraints, ConstraintProviderPreference)
	}

	if policy.MaxLatencyMS > 0 {
		constraints = appendUnique(constraints, ConstraintLatency)
	}

	if policy.MaxTTFTMS > 0 {
		constraints = appendUnique(constraints, ConstraintTTFT)
	}

	return constraints
}

func appendEvidenceConstraints(constraints []string, profile RequestProfile, observedAny, latencyEvidence, ttftEvidence bool) []string {
	if observedAny {
		constraints = appendUnique(constraints, ConstraintObservedTelemetry)
	}

	if profile.Interactive && ttftEvidence {
		constraints = appendUnique(constraints, ConstraintTTFT)
	}

	if !profile.Batch && latencyEvidence {
		constraints = appendUnique(constraints, ConstraintLatency)
	}

	return constraints
}

func routePolicyHasConstraints(policy Policy) bool {
	return len(policy.PreferredProviders) > 0 ||
		len(policy.BannedProviders) > 0 ||
		len(policy.BannedModels) > 0 ||
		len(policy.RequiredCapabilities) > 0 ||
		policy.MaxBudget > 0 ||
		policy.MaxLatencyMS > 0 ||
		policy.MaxTTFTMS > 0 ||
		policy.RequireFreshMetadata
}

func betterWithPolicy(left, right Candidate, profile RequestProfile, policy Policy) bool {
	leftProviderPreference := providerPreference(policy, left.Provider)

	rightProviderPreference := providerPreference(policy, right.Provider)
	if leftProviderPreference != rightProviderPreference {
		return leftProviderPreference < rightProviderPreference
	}

	return better(left, right, profile)
}

func providerPreference(policy Policy, provider string) int {
	if len(policy.PreferredProviders) == 0 {
		return 0
	}

	provider = normalize(provider)
	for i, preferred := range policy.PreferredProviders {
		if normalize(preferred) == provider {
			return i
		}
	}

	return len(policy.PreferredProviders)
}

func (p Policy) applyBudget(profile RequestProfile) RequestProfile {
	profile = normalizeProfile(profile)

	if p.MaxBudget <= 0 {
		return profile
	}

	if profile.Budget <= 0 || p.MaxBudget < profile.Budget {
		profile.Budget = p.MaxBudget
	}

	return profile
}

func normalizeProfile(profile RequestProfile) RequestProfile {
	if profile.EstimatedInputTokens < 0 {
		profile.EstimatedInputTokens = 0
	}

	if profile.EstimatedOutputTokens < 0 {
		profile.EstimatedOutputTokens = 0
	}

	if profile.EstimatedCacheWriteTokens < 0 {
		profile.EstimatedCacheWriteTokens = 0
	}

	if !finiteFloat(profile.Budget) || profile.Budget < 0 {
		profile.Budget = 0
	}

	if !finiteFloat(profile.PromptCacheReuseEstimate) {
		profile.PromptCacheReuseEstimate = 0
	}

	profile.PromptCacheReuseEstimate = clamp01(profile.PromptCacheReuseEstimate)

	return profile
}

func normalizePolicy(policy Policy) Policy {
	policy.PreferredProviders = normalizeStrings(policy.PreferredProviders)
	policy.BannedProviders = normalizeStrings(policy.BannedProviders)
	policy.BannedModels = normalizeStrings(policy.BannedModels)
	policy.RequiredCapabilities = normalizeStrings(policy.RequiredCapabilities)

	if !finiteFloat(policy.MaxBudget) || policy.MaxBudget < 0 {
		policy.MaxBudget = 0
	}

	if policy.MaxLatencyMS < 0 {
		policy.MaxLatencyMS = 0
	}

	if policy.MaxTTFTMS < 0 {
		policy.MaxTTFTMS = 0
	}

	return policy
}

func finiteFloat(value float64) bool {
	return !math.IsNaN(value) && !math.IsInf(value, 0)
}

func normalizeStrings(in []string) []string {
	if in == nil {
		return nil
	}

	return uniqueNormalizedStrings(in)
}

func normalizeModelID(value string) string {
	return strings.ToLower(strings.TrimSpace(value))
}

func uniqueNonEmptyStrings(in []string) []string {
	out := make([]string, 0, len(in))
	seen := make(map[string]bool, len(in))

	for _, value := range in {
		value = strings.TrimSpace(value)
		if value == "" || seen[value] {
			continue
		}

		seen[value] = true
		out = append(out, value)
	}

	return out
}

func uniqueNormalizedStrings(in []string) []string {
	out := make([]string, 0, len(in))
	seen := make(map[string]bool, len(in))

	for _, value := range in {
		value = normalize(value)
		if value == "" || seen[value] {
			continue
		}

		seen[value] = true
		out = append(out, value)
	}

	return out
}

func containsFold(values []string, want string) bool {
	want = normalize(want)
	for _, value := range values {
		if normalize(value) == want {
			return true
		}
	}

	return false
}

func containsModel(values []string, candidate Candidate) bool {
	modelIDs := candidateModelPolicyIDs(candidate)

	for _, value := range values {
		value = normalize(value)
		if value != "" && slices.Contains(modelIDs, value) {
			return true
		}
	}

	return false
}

func candidateModelPolicyIDs(candidate Candidate) []string {
	ids := []string{
		normalize(candidate.ID()),
		normalize(candidate.Name),
	}

	for _, alias := range candidate.Aliases {
		alias = normalize(alias)
		if alias == "" {
			continue
		}

		ids = append(ids, alias)
		if candidate.Provider != "" && !strings.Contains(alias, "/") {
			ids = append(ids, normalize(candidate.Provider+"/"+alias))
		}
	}

	return ids
}

func missingCapabilities(candidateCaps, requiredCaps []string) []string {
	if len(requiredCaps) == 0 {
		return nil
	}

	candidateSet := make(map[string]bool, len(candidateCaps))
	for _, capability := range candidateCaps {
		candidateSet[normalize(capability)] = true
	}

	missing := make([]string, 0)

	for _, capability := range requiredCaps {
		capability = normalize(capability)
		if capability != "" && !candidateSet[capability] {
			missing = append(missing, capability)
		}
	}

	return missing
}

func appendUnique(values []string, next string) []string {
	if slices.Contains(values, next) {
		return values
	}

	return append(values, next)
}
