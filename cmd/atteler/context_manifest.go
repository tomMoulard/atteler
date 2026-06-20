package main

import (
	"encoding/json"
	"fmt"
	"strconv"
	"strings"

	"github.com/tommoulard/atteler/pkg/contextpack"
	"github.com/tommoulard/atteler/pkg/contextref"
	atteval "github.com/tommoulard/atteler/pkg/eval"
	"github.com/tommoulard/atteler/pkg/events"
	"github.com/tommoulard/atteler/pkg/llm"
)

const contextManifestSchemaVersion = 1

// requestContextManifest is the per-request audit record for what context was
// prepared for a model call. It intentionally records estimates and upper
// bounds, not exact provider token counts.
//
//nolint:govet // Field order keeps manifest JSON groups readable.
type requestContextManifest struct {
	InputEstimate                  contextpack.TokenEstimate     `json:"input_estimate"`
	ConfiguredReferences           contextref.ReferenceManifest  `json:"configured_references"`
	InlineReferences               []contextref.ReferenceEvent   `json:"inline_references,omitempty"`
	FallbackModelEstimates         []requestModelContextEstimate `json:"fallback_model_estimates,omitempty"`
	Model                          string                        `json:"model,omitempty"`
	TokenEstimator                 string                        `json:"token_estimator"`
	MessageCount                   int                           `json:"message_count"`
	MaxInputTokens                 int                           `json:"max_input_tokens,omitempty"`
	ModelContextWindow             int                           `json:"model_context_window,omitempty"`
	ReferenceBytes                 int                           `json:"reference_bytes"`
	ReferenceEstimatedTokens       int                           `json:"reference_estimated_tokens"`
	ReferenceEstimatedErrorBound   int                           `json:"reference_estimated_token_error_bound"`
	ReferenceEstimatedUpperBound   int                           `json:"reference_estimated_token_upper_bound"`
	SchemaVersion                  int                           `json:"schema_version"`
	ConfiguredReferenceEntryCount  int                           `json:"configured_reference_entry_count"`
	InlineReferenceCount           int                           `json:"inline_reference_count"`
	IncludedReferenceCount         int                           `json:"included_reference_count"`
	TruncatedReferenceCount        int                           `json:"truncated_reference_count"`
	OmittedReferenceCount          int                           `json:"omitted_reference_count"`
	SkippedReferenceCount          int                           `json:"skipped_reference_count"`
	RejectedReferenceCount         int                           `json:"rejected_reference_count"`
	InputBudgetChecked             bool                          `json:"input_budget_checked"`
	InputFitsConfiguredTokenBudget bool                          `json:"input_fits_configured_token_budget"`
	ModelContextWindowChecked      bool                          `json:"model_context_window_checked"`
	InputFitsModelContextWindow    bool                          `json:"input_fits_model_context_window"`
}

// requestModelContextEstimate records how the same prepared prompt budgets
// against a possible fallback model. Fallbacks can be different providers with
// different overheads/context windows, so the primary model estimate alone is
// not enough to audit a CompleteWithFallback request.
type requestModelContextEstimate struct {
	Model                          string                    `json:"model,omitempty"`
	TokenEstimator                 string                    `json:"token_estimator"`
	InputEstimate                  contextpack.TokenEstimate `json:"input_estimate"`
	MaxInputTokens                 int                       `json:"max_input_tokens,omitempty"`
	ModelContextWindow             int                       `json:"model_context_window,omitempty"`
	InputBudgetChecked             bool                      `json:"input_budget_checked"`
	InputFitsConfiguredTokenBudget bool                      `json:"input_fits_configured_token_budget"`
	ModelContextWindowChecked      bool                      `json:"model_context_window_checked"`
	InputFitsModelContextWindow    bool                      `json:"input_fits_model_context_window"`
}

func newRequestContextManifest(
	providerName string,
	model string,
	messages []llm.Message,
	maxInputTokens int,
	modelContextWindow int,
	inlineRefs []contextref.Reference,
	configuredManifest contextref.ReferenceManifest,
) requestContextManifest {
	return newRequestContextManifestWithInlineEvents(
		providerName,
		model,
		messages,
		maxInputTokens,
		modelContextWindow,
		inlineReferenceEvents(inlineRefs),
		configuredManifest,
	)
}

func newRequestContextManifestWithInlineEvents(
	providerName string,
	model string,
	messages []llm.Message,
	maxInputTokens int,
	modelContextWindow int,
	inlineEvents []contextref.ReferenceEvent,
	configuredManifest contextref.ReferenceManifest,
) requestContextManifest {
	inlineEvents = sanitizeReferenceEventsForAudit(inlineEvents)
	configuredManifest = sanitizeReferenceManifestForAudit(configuredManifest)

	estimator := contextpack.NewEstimator(providerName, model)
	estimate := estimator.EstimateMessages(messages)

	manifest := requestContextManifest{
		SchemaVersion:                 contextManifestSchemaVersion,
		Model:                         model,
		TokenEstimator:                contextEstimatorSummary(estimator.Profile()),
		MessageCount:                  len(messages),
		MaxInputTokens:                maxInputTokens,
		ModelContextWindow:            modelContextWindow,
		InputEstimate:                 estimate,
		InlineReferences:              inlineEvents,
		InlineReferenceCount:          len(inlineEvents),
		ConfiguredReferences:          configuredManifest,
		ConfiguredReferenceEntryCount: len(configuredManifest.Entries),
	}
	if maxInputTokens > 0 {
		manifest.InputBudgetChecked = true
		manifest.InputFitsConfiguredTokenBudget = estimate.UpperBoundTokens <= maxInputTokens
	}

	if modelContextWindow > 0 {
		manifest.ModelContextWindowChecked = true
		manifest.InputFitsModelContextWindow = estimate.UpperBoundTokens <= modelContextWindow
	}

	for i := range inlineEvents {
		manifest.addReferenceEvent(inlineEvents[i])
	}

	for i := range configuredManifest.Entries {
		manifest.addReferenceEvent(configuredManifest.Entries[i])
	}

	return manifest
}

func requestModelEstimate(providerName, model string, messages []llm.Message, maxInputTokens, modelContextWindow int) requestModelContextEstimate {
	estimator := contextpack.NewEstimator(providerName, model)
	estimate := estimator.EstimateMessages(messages)

	modelEstimate := requestModelContextEstimate{
		Model:                          model,
		TokenEstimator:                 contextEstimatorSummary(estimator.Profile()),
		MaxInputTokens:                 maxInputTokens,
		ModelContextWindow:             modelContextWindow,
		InputEstimate:                  estimate,
		InputBudgetChecked:             maxInputTokens > 0,
		InputFitsConfiguredTokenBudget: maxInputTokens > 0 && estimate.UpperBoundTokens <= maxInputTokens,
		ModelContextWindowChecked:      modelContextWindow > 0,
		InputFitsModelContextWindow:    modelContextWindow > 0 && estimate.UpperBoundTokens <= modelContextWindow,
	}

	return modelEstimate
}

func (m *requestContextManifest) addReferenceEvent(event contextref.ReferenceEvent) {
	switch event.PolicyDecision {
	case contextref.ReferenceDecisionLoaded, contextref.ReferenceDecisionTruncated:
		m.ReferenceBytes += event.Bytes
		m.ReferenceEstimatedTokens += event.TokenEstimate.Tokens
		m.ReferenceEstimatedErrorBound += event.TokenEstimate.ErrorBoundTokens
		m.ReferenceEstimatedUpperBound += event.TokenEstimate.UpperBoundTokens

		if event.PolicyDecision == contextref.ReferenceDecisionTruncated || event.Truncated {
			m.TruncatedReferenceCount++
		}

		m.IncludedReferenceCount++
	case contextref.ReferenceDecisionSkipped:
		m.SkippedReferenceCount++
	case contextref.ReferenceDecisionOmitted:
		m.OmittedReferenceCount++
	case contextref.ReferenceDecisionRejected:
		m.RejectedReferenceCount++
	}
}

func inlineReferenceEvents(refs []contextref.Reference) []contextref.ReferenceEvent {
	referenceEvents := make([]contextref.ReferenceEvent, 0, len(refs))
	for _, ref := range refs {
		decision := contextref.ReferenceDecisionLoaded
		reason := "inline reference resolved inside root"

		if ref.Truncated {
			decision = contextref.ReferenceDecisionTruncated
			reason = "byte limit reached"
		}

		referenceEvents = append(referenceEvents, contextref.ReferenceEvent{
			Source:           ref.Path,
			Kind:             ref.Kind,
			Scope:            contextref.ReferenceScopeInline,
			Location:         "local",
			MediaType:        ref.MediaType,
			TokenEstimator:   ref.TokenEstimator,
			DigestSHA256:     ref.DigestSHA256,
			Bytes:            ref.Bytes,
			Truncated:        ref.Truncated,
			PolicyDecision:   decision,
			PolicyReason:     reason,
			PolicyReasonCode: contextref.ReferenceReasonCode(decision, reason),
			TokenEstimate:    ref.TokenEstimate,
		})
	}

	return referenceEvents
}

func requestContextManifestEvent(manifest requestContextManifest) events.Event {
	data, err := json.Marshal(manifest)
	if err != nil {
		data = fmt.Appendf(nil, `{"schema_version":%d,"error":%q}`, contextManifestSchemaVersion, err.Error())
	}

	metadata := map[string]string{
		"context_manifest":                      string(data),
		"schema_version":                        strconv.Itoa(contextManifestSchemaVersion),
		"message_count":                         strconv.Itoa(manifest.MessageCount),
		"estimated_tokens":                      strconv.Itoa(manifest.InputEstimate.Tokens),
		"estimated_token_upper_bound":           strconv.Itoa(manifest.InputEstimate.UpperBoundTokens),
		"estimated_token_error_bound":           strconv.Itoa(manifest.InputEstimate.ErrorBoundTokens),
		"reference_bytes":                       strconv.Itoa(manifest.ReferenceBytes),
		"reference_estimated_tokens":            strconv.Itoa(manifest.ReferenceEstimatedTokens),
		"reference_estimated_token_error_bound": strconv.Itoa(manifest.ReferenceEstimatedErrorBound),
		"reference_estimated_token_upper_bound": strconv.Itoa(manifest.ReferenceEstimatedUpperBound),
		"inline_reference_count":                strconv.Itoa(manifest.InlineReferenceCount),
		"configured_reference_entry_count":      strconv.Itoa(manifest.ConfiguredReferenceEntryCount),
		"fallback_model_count":                  strconv.Itoa(len(manifest.FallbackModelEstimates)),
		"included_reference_count":              strconv.Itoa(manifest.IncludedReferenceCount),
		"truncated_reference_count":             strconv.Itoa(manifest.TruncatedReferenceCount),
		"omitted_reference_count":               strconv.Itoa(manifest.OmittedReferenceCount),
		"skipped_reference_count":               strconv.Itoa(manifest.SkippedReferenceCount),
		"rejected_reference_count":              strconv.Itoa(manifest.RejectedReferenceCount),
		"token_estimator":                       manifest.TokenEstimator,
		"input_budget_checked":                  strconv.FormatBool(manifest.InputBudgetChecked),
		"fits_configured_token_budget":          strconv.FormatBool(manifest.InputFitsConfiguredTokenBudget),
		"model_context_window_checked":          strconv.FormatBool(manifest.ModelContextWindowChecked),
		"fits_model_context_window":             strconv.FormatBool(manifest.InputFitsModelContextWindow),
	}
	if manifest.MaxInputTokens > 0 {
		metadata["max_input_tokens"] = strconv.Itoa(manifest.MaxInputTokens)
	}

	if manifest.ModelContextWindow > 0 {
		metadata["model_context_window"] = strconv.Itoa(manifest.ModelContextWindow)
	}

	return events.Event{
		Type:     events.ContextManifest,
		Model:    manifest.Model,
		Metadata: metadata,
	}
}

func setExplicitContextManifestEventModel(event *events.Event, model string) {
	if event == nil || strings.TrimSpace(model) == "" {
		return
	}

	event.Model = model
}

func mergeReferenceManifests(manifests ...contextref.ReferenceManifest) contextref.ReferenceManifest {
	referenceEvents := make([]contextref.ReferenceEvent, 0)

	tokenEstimator := ""

	for i := range manifests {
		manifest := &manifests[i]
		if tokenEstimator == "" {
			tokenEstimator = sanitizeContextManifestText(manifest.TokenEstimator)
		}

		referenceEvents = append(referenceEvents, manifest.Entries...)
	}

	merged := contextref.BuildReferenceManifest(referenceEvents)
	if merged.TokenEstimator == "" {
		merged.TokenEstimator = tokenEstimator
	}

	return merged
}

func sanitizeReferenceManifestForAudit(manifest contextref.ReferenceManifest) contextref.ReferenceManifest {
	sanitized := contextref.BuildReferenceManifest(manifest.Entries)
	if sanitized.TokenEstimator == "" {
		sanitized.TokenEstimator = sanitizeContextManifestText(manifest.TokenEstimator)
	}

	return sanitized
}

func sanitizeReferenceEventsForAudit(referenceEvents []contextref.ReferenceEvent) []contextref.ReferenceEvent {
	return contextref.BuildReferenceManifest(referenceEvents).Entries
}

func sanitizeContextManifestText(value string) string {
	value = strings.ToValidUTF8(value, "�")
	value = strings.Map(func(r rune) rune {
		if r < 0x20 || r == 0x7f || (r >= 0x80 && r <= 0x9f) {
			return '�'
		}

		return r
	}, value)

	return atteval.Redact(value)
}
