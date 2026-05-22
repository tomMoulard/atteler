package main

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"

	"github.com/tommoulard/atteler/pkg/events"
	"github.com/tommoulard/atteler/pkg/llm"
	"github.com/tommoulard/atteler/pkg/modelroute"
)

func emitRouteDecisionWarning(ctx context.Context, runner *events.Runner, sessionID, sessionPath, agentName, modelName string, decision *modelroute.Decision) {
	if event, ok := routeDecisionEvent(sessionID, sessionPath, agentName, modelName, decision); ok {
		emitHookWarning(ctx, runner, event)
	}
}

func routeDecisionEvent(sessionID, sessionPath, agentName, modelName string, decision *modelroute.Decision) (events.Event, bool) {
	if decision == nil {
		return events.Event{}, false
	}

	body, err := json.Marshal(decision)
	if err != nil {
		body = []byte(`{"error":"route decision marshal failed"}`)
	}

	return events.Event{
		Type:        events.RouteDecision,
		SessionID:   sessionID,
		SessionPath: sessionPath,
		Agent:       agentName,
		Model:       modelName,
		Content:     string(body),
		Metadata:    routeDecisionMetadata(decision),
	}, true
}

func routeDecisionMetadata(decision *modelroute.Decision) map[string]string {
	metadata := map[string]string{
		"selected":        decision.Selected,
		"fallback_order":  strings.Join(decision.FallbackOrder, ","),
		"candidate_count": strconv.Itoa(len(decision.Candidates)),
		"rejected_count":  strconv.Itoa(routeDecisionRejectedCount(decision)),
		"phase":           routeDecisionPhase(decision),
	}
	if len(decision.Constraints) > 0 {
		metadata["constraints"] = strings.Join(decision.Constraints, ",")
	}

	if len(decision.Warnings) > 0 {
		metadata["warning_count"] = strconv.Itoa(len(decision.Warnings))
	}

	if decision.ActualSelected != "" {
		metadata["actual_selected"] = decision.ActualSelected
	}

	if decision.CatalogVersion != "" {
		metadata["catalog_version"] = decision.CatalogVersion
	}

	addRouteProfileMetadata(metadata, decision.Profile)

	if decision.Availability != nil && decision.Availability.Checked {
		metadata["availability_checked"] = strconv.FormatBool(true)
		if decision.Availability.RefreshAttempted {
			metadata["availability_refresh_attempted"] = strconv.FormatBool(true)
		}

		if decision.Availability.RefreshTimeoutMS > 0 {
			metadata["availability_refresh_timeout_ms"] = strconv.Itoa(decision.Availability.RefreshTimeoutMS)
		}

		metadata["provider_count"] = strconv.Itoa(len(decision.Availability.Providers))
		metadata["model_count"] = strconv.Itoa(len(decision.Availability.Models))
		metadata["provider_model_count"] = strconv.Itoa(providerModelCount(decision.Availability.ProviderModels))
		metadata["unavailable_count"] = strconv.Itoa(len(decision.Availability.Unavailable))
		metadata["unverified_count"] = strconv.Itoa(len(decision.Availability.Unverified))
		metadata["verified_provider_model_count"] = strconv.Itoa(verifiedProviderModelCount(decision.Availability.ProviderModelsVerified))
	}

	if actual, ok := routeDecisionActualCandidate(decision); ok {
		addRouteActualMetadata(metadata, actual)
	}

	if decision.CatalogStale {
		metadata["catalog_stale"] = strconv.FormatBool(true)
	}

	return metadata
}

func addRouteActualMetadata(metadata map[string]string, actual modelroute.CandidateDecision) {
	if actual.ObservedLatencyMS > 0 {
		metadata["actual_latency_ms"] = strconv.Itoa(actual.ObservedLatencyMS)
	}

	if actual.ObservedTTFTMS > 0 {
		metadata["actual_ttft_ms"] = strconv.Itoa(actual.ObservedTTFTMS)
	}

	if !actual.ActualUsageRecorded {
		return
	}

	metadata["estimated_cost"] = fmt.Sprintf("%.6f", actual.EstimatedCost)
	metadata["actual_cost"] = fmt.Sprintf("%.6f", actual.ActualCost)
	metadata["actual_cost_delta"] = fmt.Sprintf("%.6f", actual.ActualCostDelta)
	metadata["actual_input_tokens"] = strconv.Itoa(actual.ActualInputTokens)
	metadata["actual_output_tokens"] = strconv.Itoa(actual.ActualOutputTokens)

	if actual.ActualCachedTokens > 0 {
		metadata["actual_cached_input_tokens"] = strconv.Itoa(actual.ActualCachedTokens)
	}

	if actual.ActualCacheWrites > 0 {
		metadata["actual_cache_write_tokens"] = strconv.Itoa(actual.ActualCacheWrites)
	}
}

func addRouteProfileMetadata(metadata map[string]string, profile modelroute.RequestProfile) {
	if profile.EstimatedInputTokens > 0 {
		metadata["estimated_input_tokens"] = strconv.Itoa(profile.EstimatedInputTokens)
	}

	if profile.EstimatedOutputTokens > 0 {
		metadata["estimated_output_tokens"] = strconv.Itoa(profile.EstimatedOutputTokens)
	}

	if profile.EstimatedCacheWriteTokens > 0 {
		metadata["estimated_cache_write_tokens"] = strconv.Itoa(profile.EstimatedCacheWriteTokens)
	}

	if profile.PromptCacheReuseEstimate > 0 {
		metadata["prompt_cache_reuse_estimate"] = strconv.FormatFloat(profile.PromptCacheReuseEstimate, 'f', -1, 64)
	}

	if profile.Budget > 0 {
		metadata["budget"] = fmt.Sprintf("%.6f", profile.Budget)
	}
}

func routeDecisionWithResponse(decision *modelroute.Decision, resp *llm.Response, telemetry ...*modelroute.Telemetry) *modelroute.Decision {
	if decision == nil || resp == nil {
		return nil
	}

	annotated := *decision
	if len(telemetry) > 0 {
		annotated = modelroute.DecisionWithTelemetry(annotated, telemetry[0])
	}

	annotated = modelroute.DecisionWithActualUsage(annotated, routeResponseModelID(resp.Provider, resp.Model), modelroute.ActualUsage{
		Latency:           resp.Latency,
		TTFT:              resp.FirstTokenLatency,
		InputTokens:       resp.InputTokens,
		CachedInputTokens: resp.CachedInputTokens,
		CacheWriteTokens:  resp.CacheWriteInputTokens,
		OutputTokens:      resp.OutputTokens,
	})
	if !routeResponseHasTokenUsage(resp) {
		clearRouteDecisionActualUsage(&annotated)
	}

	return &annotated
}

func routeResponseHasTokenUsage(resp *llm.Response) bool {
	if resp == nil {
		return false
	}

	return resp.InputTokens > 0 ||
		resp.CachedInputTokens > 0 ||
		resp.CacheWriteInputTokens > 0 ||
		resp.OutputTokens > 0
}

func clearRouteDecisionActualUsage(decision *modelroute.Decision) {
	if decision == nil || decision.ActualSelected == "" {
		return
	}

	for i := range decision.Candidates {
		candidate := &decision.Candidates[i]
		if candidate.ID != decision.ActualSelected {
			continue
		}

		candidate.ActualUsageRecorded = false
		candidate.ActualCost = 0
		candidate.ActualCostDelta = 0
		candidate.ActualInputTokens = 0
		candidate.ActualCachedTokens = 0
		candidate.ActualCacheWrites = 0
		candidate.ActualOutputTokens = 0

		return
	}
}

func routeResponseModelID(provider, model string) string {
	modelID := strings.TrimSpace(model)

	provider = strings.TrimSpace(provider)
	if modelID == "" || provider == "" || strings.Contains(modelID, "/") {
		return modelID
	}

	return provider + "/" + modelID
}

func routeDecisionPhase(decision *modelroute.Decision) string {
	if decision.ActualSelected != "" {
		return "actual"
	}

	return "estimated"
}

func routeDecisionActualCandidate(decision *modelroute.Decision) (modelroute.CandidateDecision, bool) {
	if decision.ActualSelected == "" {
		return modelroute.CandidateDecision{}, false
	}

	for i := range decision.Candidates {
		candidate := decision.Candidates[i]
		if candidate.ID == decision.ActualSelected {
			return candidate, true
		}
	}

	return modelroute.CandidateDecision{}, false
}

func routeDecisionRejectedCount(decision *modelroute.Decision) int {
	count := 0

	for i := range decision.Candidates {
		if decision.Candidates[i].Status == modelroute.StatusRejected {
			count++
		}
	}

	return count
}

func verifiedProviderModelCount(providers map[string]bool) int {
	count := 0

	for _, verified := range providers {
		if verified {
			count++
		}
	}

	return count
}

func providerModelCount(providers map[string][]string) int {
	count := 0

	for _, models := range providers {
		count += len(models)
	}

	return count
}
