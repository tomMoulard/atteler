package main

import (
	"errors"
	"fmt"
	"strings"
	"time"

	appconfig "github.com/tommoulard/atteler/pkg/config"
	"github.com/tommoulard/atteler/pkg/llm"
	"github.com/tommoulard/atteler/pkg/modelroute"
	"github.com/tommoulard/atteler/pkg/session"
)

var errIdleSuggestionBudget = errors.New("idle suggestion budget")

func defaultIdleSuggestionBudget() idleSuggestionBudget {
	return idleSuggestionBudget{
		MaxRequestsPerSession: idleSuggestionMaxRequestsPerSession,
		MaxRequestsPerMinute:  idleSuggestionMaxRequestsPerMinute,
		MaxInputTokens:        idleSuggestionMaxInputTokens,
		MaxOutputTokens:       idleSuggestionMaxOutputTokens,
		MaxSessionTokens:      idleSuggestionMaxSessionTokens,
		MaxEstimatedCostUSD:   idleSuggestionMaxEstimatedCostUSD,
	}
}

func idleSuggestionBudgetStateFromSession(
	sessionState session.Session,
) (usage tokenUsage, requests, estimatedTokens int, estimatedCostUSD float64) {
	if sessionState.BackgroundSuggestions == nil {
		return usage, requests, estimatedTokens, estimatedCostUSD
	}

	background := sessionState.BackgroundSuggestions
	usage = tokenUsage{
		InputTokens:           background.InputTokens,
		CachedInputTokens:     background.CachedInputTokens,
		CacheWriteInputTokens: background.CacheWriteInputTokens,
		OutputTokens:          background.OutputTokens,
		Responses:             background.Responses,
	}

	estimatedTokens = max(
		background.EstimatedInputTokens+background.EstimatedOutputTokens,
		background.InputTokens+background.OutputTokens,
	)

	return usage, background.Requests, estimatedTokens, background.EstimatedCostUSD
}

func normalizeIdleSuggestionBudget(budget idleSuggestionBudget) idleSuggestionBudget {
	defaults := defaultIdleSuggestionBudget()

	if budget.MaxRequestsPerSession <= 0 {
		budget.MaxRequestsPerSession = defaults.MaxRequestsPerSession
	}

	if budget.MaxRequestsPerMinute <= 0 {
		budget.MaxRequestsPerMinute = defaults.MaxRequestsPerMinute
	}

	if budget.MaxInputTokens <= 0 {
		budget.MaxInputTokens = defaults.MaxInputTokens
	}

	if budget.MaxOutputTokens <= 0 {
		budget.MaxOutputTokens = defaults.MaxOutputTokens
	}

	if budget.MaxSessionTokens <= 0 {
		budget.MaxSessionTokens = defaults.MaxSessionTokens
	}

	if budget.MaxEstimatedCostUSD <= 0 {
		budget.MaxEstimatedCostUSD = defaults.MaxEstimatedCostUSD
	}

	return budget
}

func effectiveIdleSuggestionMaxInputTokens(maxInputTokens int, budget idleSuggestionBudget) int {
	budget = normalizeIdleSuggestionBudget(budget)

	if budget.MaxInputTokens > 0 && (maxInputTokens <= 0 || budget.MaxInputTokens < maxInputTokens) {
		return budget.MaxInputTokens
	}

	return maxInputTokens
}

func promptSuggestionConsentFromPreferences(
	promptLocalOnly bool,
	sessionPreference string,
	statePreference appconfig.PreferenceResolution,
) promptSuggestionConsent {
	if promptLocalOnly {
		return promptSuggestionConsentLocalOnly
	}

	switch appconfig.NormalizePromptSuggestionPreference(sessionPreference) {
	case appconfig.PromptSuggestionPreferenceModelBacked:
		return promptSuggestionConsentSession
	case appconfig.PromptSuggestionPreferenceLocalOnly:
		return promptSuggestionConsentLocalOnly
	}

	switch appconfig.NormalizePromptSuggestionPreference(statePreference.Value) {
	case appconfig.PromptSuggestionPreferenceModelBacked:
		switch statePreference.Scope {
		case appconfig.ModelScopeFolder:
			return promptSuggestionConsentFolder
		case appconfig.ModelScopeGlobal:
			return promptSuggestionConsentGlobal
		default:
			return promptSuggestionConsentSession
		}
	case appconfig.PromptSuggestionPreferenceLocalOnly:
		return promptSuggestionConsentLocalOnly
	default:
		return promptSuggestionConsentLocalOnly
	}
}

func (c promptSuggestionConsent) allowsModelBacked() bool {
	return c == promptSuggestionConsentSession ||
		c == promptSuggestionConsentFolder ||
		c == promptSuggestionConsentGlobal
}

func (m model) modelBackedIdleSuggestionsEnabled() bool {
	return !m.promptLocalOnly && m.promptSuggestionConsent.allowsModelBacked()
}

func (m model) idleSuggestionBudgetBeforeRequestError() error {
	budget := normalizeIdleSuggestionBudget(m.idleSuggestionBudget)
	if budget.MaxRequestsPerSession > 0 && m.idleSuggestionRequests >= budget.MaxRequestsPerSession {
		return fmt.Errorf("%w: request limit reached (%d/%d)", errIdleSuggestionBudget, m.idleSuggestionRequests, budget.MaxRequestsPerSession)
	}

	recentRequests := idleSuggestionRecentRequestCount(m.idleSuggestionTimes, time.Now())
	if budget.MaxRequestsPerMinute > 0 && recentRequests >= budget.MaxRequestsPerMinute {
		return fmt.Errorf("%w: rate limit reached (%d/%d per minute)", errIdleSuggestionBudget, recentRequests, budget.MaxRequestsPerMinute)
	}

	usedTokens := max(m.idleSuggestionUsage.InputTokens+m.idleSuggestionUsage.OutputTokens, m.idleSuggestionTokens)
	if budget.MaxSessionTokens > 0 && usedTokens >= budget.MaxSessionTokens {
		return fmt.Errorf("%w: token limit reached (%d/%d)", errIdleSuggestionBudget, usedTokens, budget.MaxSessionTokens)
	}

	if budget.MaxEstimatedCostUSD > 0 && m.idleSuggestionCostUSD >= budget.MaxEstimatedCostUSD {
		return fmt.Errorf("%w: cost limit reached (%.6f/%.6f)", errIdleSuggestionBudget, m.idleSuggestionCostUSD, budget.MaxEstimatedCostUSD)
	}

	return nil
}

func idleSuggestionRecentRequestCount(requestTimes []time.Time, now time.Time) int {
	if now.IsZero() {
		now = time.Now()
	}

	cutoff := now.Add(-time.Minute)
	count := 0

	for _, requestedAt := range requestTimes {
		if requestedAt.After(cutoff) {
			count++
		}
	}

	return count
}

func appendIdleSuggestionRequestTime(requestTimes []time.Time, requestedAt time.Time) []time.Time {
	if requestedAt.IsZero() {
		requestedAt = time.Now()
	}

	cutoff := requestedAt.Add(-time.Minute)

	out := make([]time.Time, 0, len(requestTimes)+1)
	for _, previous := range requestTimes {
		if previous.After(cutoff) {
			out = append(out, previous)
		}
	}

	return append(out, requestedAt)
}

func validateIdleSuggestionRequestBudget(
	reg *llm.Registry,
	modelName string,
	fallbackModels []string,
	messages []llm.Message,
	maxInputTokens int,
	budget idleSuggestionBudget,
	currentUsage tokenUsage,
	currentEstimatedTokens int,
	currentCostUSD float64,
) (estimatedInputTokens int, estimatedCostUSD float64, err error) {
	budget = normalizeIdleSuggestionBudget(budget)
	effectiveMaxInputTokens := effectiveIdleSuggestionMaxInputTokens(maxInputTokens, budget)

	if err := validateRequestBudgetWithFallbacks(reg, modelName, fallbackModels, messages, effectiveMaxInputTokens); err != nil {
		return 0, 0, fmt.Errorf("%w: %w", errIdleSuggestionBudget, err)
	}

	estimatedInputTokens = estimateIdleSuggestionInputTokensWithFallbacks(reg, modelName, fallbackModels, messages)

	if budget.MaxSessionTokens > 0 {
		used := max(currentUsage.InputTokens+currentUsage.OutputTokens, currentEstimatedTokens) + estimatedInputTokens + budget.MaxOutputTokens
		if used > budget.MaxSessionTokens {
			return estimatedInputTokens, 0, fmt.Errorf("%w: token limit would be exceeded (%d/%d)", errIdleSuggestionBudget, used, budget.MaxSessionTokens)
		}
	}

	estimatedCostUSD = estimateIdleSuggestionCostWithFallbacks(reg, modelName, fallbackModels, estimatedInputTokens, budget.MaxOutputTokens)
	if budget.MaxEstimatedCostUSD > 0 && estimatedCostUSD > 0 && currentCostUSD+estimatedCostUSD > budget.MaxEstimatedCostUSD {
		return estimatedInputTokens, estimatedCostUSD, fmt.Errorf(
			"%w: estimated cost would be %.6f > %.6f",
			errIdleSuggestionBudget,
			currentCostUSD+estimatedCostUSD,
			budget.MaxEstimatedCostUSD,
		)
	}

	return estimatedInputTokens, estimatedCostUSD, nil
}

func estimateIdleSuggestionInputTokensWithFallbacks(
	reg *llm.Registry,
	modelName string,
	fallbackModels []string,
	messages []llm.Message,
) int {
	models := requestBudgetModels(modelName, fallbackModels)
	if len(models) == 0 {
		estimate, _ := estimateMessagesForModel(reg, "", messages)

		return estimate.UpperBoundTokens
	}

	maxEstimatedTokens := 0

	for _, model := range models {
		estimate, _ := estimateMessagesForModel(reg, model, messages)
		maxEstimatedTokens = max(maxEstimatedTokens, estimate.UpperBoundTokens)
	}

	return maxEstimatedTokens
}

func estimateIdleSuggestionCostWithFallbacks(
	reg *llm.Registry,
	modelName string,
	fallbackModels []string,
	inputTokens, outputTokens int,
) float64 {
	estimatedCostUSD := estimateIdleSuggestionCost(reg, modelName, inputTokens, outputTokens)
	for _, fallbackModel := range fallbackModels {
		if fallbackCostUSD := estimateIdleSuggestionCost(reg, fallbackModel, inputTokens, outputTokens); fallbackCostUSD > estimatedCostUSD {
			estimatedCostUSD = fallbackCostUSD
		}
	}

	return estimatedCostUSD
}

func estimateIdleSuggestionCost(reg *llm.Registry, modelName string, inputTokens, outputTokens int) float64 {
	modelID := idleSuggestionCatalogModelID(reg, modelName)
	if idleSuggestionModelIsLocal(modelID) {
		return 0
	}

	candidate, ok := modelroute.BuiltinCatalog().Candidate(modelID)
	if !ok {
		return conservativeIdleSuggestionCost(inputTokens, outputTokens)
	}

	return modelroute.EstimateCost(candidate, modelroute.RequestProfile{
		EstimatedInputTokens:  inputTokens,
		EstimatedOutputTokens: outputTokens,
	})
}

func idleSuggestionModelIsLocal(modelID string) bool {
	provider, _, ok := strings.Cut(strings.TrimSpace(modelID), "/")
	if !ok {
		return false
	}

	return strings.EqualFold(provider, "ollama")
}

func conservativeIdleSuggestionCost(inputTokens, outputTokens int) float64 {
	var maxInputCost, maxOutputCost float64

	models := modelroute.BuiltinCatalog().Models
	for i := range models {
		metadata := &models[i]
		if metadata.InputTokenCost > maxInputCost {
			maxInputCost = metadata.InputTokenCost
		}

		if metadata.OutputTokenCost > maxOutputCost {
			maxOutputCost = metadata.OutputTokenCost
		}
	}

	if maxInputCost <= 0 && maxOutputCost <= 0 {
		return 0
	}

	return float64(max(inputTokens, 0))*maxInputCost + float64(max(outputTokens, 0))*maxOutputCost
}

func idleSuggestionCatalogModelID(reg *llm.Registry, modelName string) string {
	modelID := strings.TrimSpace(modelName)
	if modelID == "" {
		provider, model, ok := resolveRegistryModel(reg, "")
		if !ok {
			return ""
		}

		return providerQualifiedModelID(provider, model)
	}

	if strings.Contains(modelID, "/") {
		return modelID
	}

	return providerQualifiedModelID(providerNameForModel(reg, modelID), modelID)
}

func providerQualifiedModelID(provider, model string) string {
	model = strings.TrimSpace(model)
	if model == "" {
		return ""
	}

	provider = strings.TrimSpace(provider)
	if provider != "" && !strings.Contains(model, "/") {
		return provider + "/" + model
	}

	return model
}
