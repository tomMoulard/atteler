package main

import (
	"context"
	"encoding/json"
	"net/url"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/tommoulard/atteler/pkg/contextpack"
	"github.com/tommoulard/atteler/pkg/contextref"
	"github.com/tommoulard/atteler/pkg/events"
	"github.com/tommoulard/atteler/pkg/llm"
)

type contextManifestBudgetProvider struct {
	name   string
	models []string
	window int
}

func (p contextManifestBudgetProvider) Name() string { return p.name }

func (p contextManifestBudgetProvider) Models() []string { return p.models }

func (p contextManifestBudgetProvider) FetchModels(context.Context) ([]string, error) {
	return p.models, nil
}

func (p contextManifestBudgetProvider) HealthCheck(context.Context) error { return nil }

func (p contextManifestBudgetProvider) Complete(_ context.Context, params llm.CompleteParams) (*llm.Response, error) {
	return &llm.Response{Content: "ok", Model: params.Model}, nil
}

func (p contextManifestBudgetProvider) ModelContextWindow(string) int { return p.window }

func TestRequestContextManifestIncludesInlineAndConfiguredReferences(t *testing.T) {
	t.Parallel()

	estimator := contextpack.NewEstimator("openai", "gpt-4.1")
	inlineEstimate := estimator.EstimateMessage(llm.Message{Role: llm.RoleSystem, Content: "inline"})
	configuredEstimate := estimator.EstimateMessage(llm.Message{Role: llm.RoleSystem, Content: "configured"})
	configured := contextref.BuildReferenceManifest([]contextref.ReferenceEvent{
		{
			Source:         "https://docs.example.com/ref.md",
			Kind:           "url",
			Scope:          contextref.ReferenceScopeGlobal,
			Location:       "remote",
			Bytes:          10,
			PolicyDecision: contextref.ReferenceDecisionLoaded,
			PolicyReason:   "allowed by policy",
			TokenEstimate:  configuredEstimate,
			TokenEstimator: contextEstimatorSummary(estimator.Profile()),
		},
	})

	manifest := newRequestContextManifest(
		"openai",
		"gpt-4.1",
		[]llm.Message{{Role: llm.RoleUser, Content: "summarize"}},
		1000,
		2000,
		[]contextref.Reference{
			{
				Path:           "README.md",
				Kind:           "file",
				DigestSHA256:   strings.Repeat("a", 64),
				Bytes:          6,
				TokenEstimate:  inlineEstimate,
				TokenEstimator: contextEstimatorSummary(estimator.Profile()),
			},
		},
		configured,
	)

	assert.Equal(t, 1, manifest.SchemaVersion)
	assert.Equal(t, 1, manifest.InlineReferenceCount)
	assert.Equal(t, 1, manifest.ConfiguredReferenceEntryCount)
	assert.Equal(t, 2, manifest.IncludedReferenceCount)
	assert.Equal(t, 16, manifest.ReferenceBytes)
	assert.Equal(t, inlineEstimate.Tokens+configuredEstimate.Tokens, manifest.ReferenceEstimatedTokens)
	assert.Equal(t, inlineEstimate.ErrorBoundTokens+configuredEstimate.ErrorBoundTokens, manifest.ReferenceEstimatedErrorBound)
	assert.Equal(t, inlineEstimate.UpperBoundTokens+configuredEstimate.UpperBoundTokens, manifest.ReferenceEstimatedUpperBound)
	assert.True(t, manifest.InputFitsConfiguredTokenBudget)
	assert.True(t, manifest.InputFitsModelContextWindow)
	assert.Contains(t, manifest.TokenEstimator, "openai-calibrated")
	assert.Contains(t, manifest.TokenEstimator, "calibration=provider-message-overhead-v1")
	require.Len(t, manifest.InlineReferences, 1)
	assert.Equal(t, contextref.ReferenceScopeInline, manifest.InlineReferences[0].Scope)
	assert.Equal(t, contextref.ReferenceDecisionLoaded, manifest.InlineReferences[0].PolicyDecision)
	assert.Equal(t, strings.Repeat("a", 64), manifest.InlineReferences[0].DigestSHA256)
}

func TestRequestContextManifestEventCarriesJSONAndSummaryCounts(t *testing.T) {
	t.Parallel()

	manifest := newRequestContextManifest(
		"",
		"gpt-test",
		[]llm.Message{{Role: llm.RoleUser, Content: strings.Repeat("x", 12)}},
		2,
		3,
		nil,
		contextref.ReferenceManifest{},
	)

	event := requestContextManifestEvent(manifest)

	require.Equal(t, events.ContextManifest, event.Type)
	assert.Equal(t, "gpt-test", event.Model)
	assert.Equal(t, "1", event.Metadata["schema_version"])
	assert.Equal(t, "1", event.Metadata["message_count"])
	assert.Equal(t, "true", event.Metadata["input_budget_checked"])
	assert.Equal(t, "false", event.Metadata["fits_configured_token_budget"])
	assert.Equal(t, "true", event.Metadata["model_context_window_checked"])
	assert.Equal(t, "false", event.Metadata["fits_model_context_window"])
	assert.Equal(t, "3", event.Metadata["model_context_window"])
	assert.Contains(t, event.Metadata, "reference_estimated_tokens")
	assert.Contains(t, event.Metadata, "reference_estimated_token_error_bound")
	assert.Contains(t, event.Metadata, "reference_estimated_token_upper_bound")

	manifestJSON := []byte(event.Metadata["context_manifest"])

	var raw map[string]any
	require.NoError(t, json.Unmarshal(manifestJSON, &raw))
	assert.Equal(t, false, raw["input_fits_configured_token_budget"])
	assert.Equal(t, false, raw["input_fits_model_context_window"])

	var decoded requestContextManifest
	require.NoError(t, json.Unmarshal(manifestJSON, &decoded))
	assert.Equal(t, manifest.InputEstimate.UpperBoundTokens, decoded.InputEstimate.UpperBoundTokens)
	assert.Equal(t, manifest.InputBudgetChecked, decoded.InputBudgetChecked)
	assert.Equal(t, manifest.InputFitsConfiguredTokenBudget, decoded.InputFitsConfiguredTokenBudget)
	assert.Equal(t, manifest.ModelContextWindowChecked, decoded.ModelContextWindowChecked)
	assert.Equal(t, manifest.InputFitsModelContextWindow, decoded.InputFitsModelContextWindow)
}

func TestRequestContextManifestSanitizesConfiguredReferenceManifest(t *testing.T) {
	t.Parallel()

	parsed := url.URL{
		Scheme: "https",
		Host:   "docs.example.com",
		Path:   "/style.md",
	}
	parsed.User = url.UserPassword("token-user", "password-secret")
	query := parsed.Query()
	query.Set("access_token", "query-secret")
	query.Set("topic", "context")
	parsed.RawQuery = query.Encode()
	rawURL := parsed.String()

	manifest := newRequestContextManifest(
		"",
		"gpt-test",
		[]llm.Message{{Role: llm.RoleUser, Content: "hello"}},
		0,
		0,
		nil,
		contextref.ReferenceManifest{
			TokenEstimator: "test-estimator",
			Entries: []contextref.ReferenceEvent{
				{
					Source:         rawURL,
					ResolvedSource: rawURL,
					Kind:           "url",
					PolicyDecision: contextref.ReferenceDecisionRejected,
					PolicyReason:   "fetch failed for " + rawURL,
				},
			},
		},
	)
	require.Len(t, manifest.ConfiguredReferences.Entries, 1)

	event := requestContextManifestEvent(manifest)
	manifestJSON := event.Metadata["context_manifest"]

	for _, got := range []string{
		manifest.ConfiguredReferences.Entries[0].Source,
		manifest.ConfiguredReferences.Entries[0].ResolvedSource,
		manifest.ConfiguredReferences.Entries[0].PolicyReason,
		manifestJSON,
	} {
		assert.NotContains(t, got, "token-user")
		assert.NotContains(t, got, "password-secret")
		assert.NotContains(t, got, "query-secret")
	}

	assert.Contains(t, manifestJSON, "REDACTED@docs.example.com")
	assert.Contains(t, manifestJSON, "access_token=REDACTED")
	assert.Equal(t, 1, manifest.ConfiguredReferences.SchemaVersion)
	assert.Contains(t, manifestJSON, "test-estimator")
}

func TestRequestContextManifestSanitizesInlineReferenceEvents(t *testing.T) {
	t.Parallel()

	manifest := newRequestContextManifestWithInlineEvents(
		"",
		"gpt-test",
		[]llm.Message{{Role: llm.RoleUser, Content: "hello"}},
		0,
		0,
		[]contextref.ReferenceEvent{
			{
				Source:           "docs/api_key=inline-secret.md",
				Kind:             "file",
				Scope:            contextref.ReferenceScopeInline,
				Location:         "local",
				TokenEstimator:   "secret=inline-estimator-secret",
				DigestSHA256:     "sha\nsecret=inline-digest-secret",
				PolicyDecision:   contextref.ReferenceDecisionLoaded,
				PolicyReason:     "inline token=inline-reason-secret",
				PolicyReasonCode: "loaded.token=inline-code-secret",
			},
		},
		contextref.ReferenceManifest{},
	)

	require.Len(t, manifest.InlineReferences, 1)
	inlineRef := manifest.InlineReferences[0]
	assert.NotContains(t, inlineRef.Source, "inline-secret")
	assert.NotContains(t, inlineRef.TokenEstimator, "inline-estimator-secret")
	assert.NotContains(t, inlineRef.DigestSHA256, "inline-digest-secret")
	assert.NotContains(t, inlineRef.PolicyReason, "inline-reason-secret")
	assert.NotContains(t, inlineRef.PolicyReasonCode, "inline-code-secret")
	assert.NotContains(t, inlineRef.DigestSHA256, "\n")
	assert.Contains(t, inlineRef.Source, "api_key=[REDACTED]")

	event := requestContextManifestEvent(manifest)
	manifestJSON := event.Metadata["context_manifest"]
	assert.NotContains(t, manifestJSON, "inline-secret")
	assert.NotContains(t, manifestJSON, "inline-estimator-secret")
	assert.NotContains(t, manifestJSON, "inline-digest-secret")
	assert.NotContains(t, manifestJSON, "inline-reason-secret")
	assert.NotContains(t, manifestJSON, "inline-code-secret")
}

func TestRequestContextManifestSanitizesManifestLevelTokenEstimator(t *testing.T) {
	t.Parallel()

	manifest := newRequestContextManifest(
		"",
		"gpt-test",
		[]llm.Message{{Role: llm.RoleUser, Content: "hello"}},
		0,
		0,
		nil,
		contextref.ReferenceManifest{
			TokenEstimator: "provider=openai\nsecret=estimator-secret",
		},
	)

	require.Empty(t, manifest.ConfiguredReferences.Entries)
	assert.NotContains(t, manifest.ConfiguredReferences.TokenEstimator, "estimator-secret")
	assert.NotContains(t, manifest.ConfiguredReferences.TokenEstimator, "\n")
	assert.Contains(t, manifest.ConfiguredReferences.TokenEstimator, "secret=[REDACTED]")

	event := requestContextManifestEvent(manifest)
	manifestJSON := event.Metadata["context_manifest"]
	assert.NotContains(t, manifestJSON, "estimator-secret")
	assert.NotContains(t, manifestJSON, `\nsecret=estimator-secret`)
	assert.Contains(t, manifestJSON, "secret=[REDACTED]")
}

func TestRequestContextManifestCountsOmittedReferencesOutsideIncludedBytes(t *testing.T) {
	t.Parallel()

	configured := contextref.BuildReferenceManifest([]contextref.ReferenceEvent{
		{
			Source:         "good.md",
			Kind:           "file",
			Scope:          contextref.ReferenceScopeGlobal,
			Location:       "local",
			Bytes:          12,
			PolicyDecision: contextref.ReferenceDecisionOmitted,
			PolicyReason:   "configured reference block omitted because loading failed",
			TokenEstimate:  contextpack.TokenEstimate{Tokens: 3, ErrorBoundTokens: 1, UpperBoundTokens: 4},
			TokenEstimator: "test-estimator",
		},
	})

	manifest := newRequestContextManifest(
		"",
		"gpt-test",
		[]llm.Message{{Role: llm.RoleUser, Content: "hello"}},
		1000,
		0,
		nil,
		configured,
	)

	assert.Equal(t, 1, manifest.OmittedReferenceCount)
	assert.Equal(t, 0, manifest.IncludedReferenceCount)
	assert.Equal(t, 0, manifest.ReferenceBytes)

	event := requestContextManifestEvent(manifest)
	assert.Equal(t, "1", event.Metadata["omitted_reference_count"])
	assert.Equal(t, "0", event.Metadata["included_reference_count"])
}

func TestRequestContextManifestForModelsIncludesFallbackEstimates(t *testing.T) {
	t.Parallel()

	registry := llm.NewRegistry()
	registry.Register(contextManifestBudgetProvider{name: "primary", models: []string{"large"}, window: 10_000})
	registry.Register(contextManifestBudgetProvider{name: "fallback", models: []string{"tiny"}, window: 3})

	messages := []llm.Message{{Role: llm.RoleUser, Content: strings.Repeat("fallback budget ", 10)}}
	manifest := newRequestContextManifestForModels(
		registry,
		"primary/large",
		[]string{"fallback/tiny", "fallback/tiny"},
		messages,
		1_000,
		contextref.ReferenceManifest{},
	)

	require.Len(t, manifest.FallbackModelEstimates, 1)
	fallback := manifest.FallbackModelEstimates[0]
	assert.Equal(t, "fallback/tiny", fallback.Model)
	assert.Equal(t, 3, fallback.ModelContextWindow)
	assert.True(t, fallback.InputBudgetChecked)
	assert.True(t, fallback.InputFitsConfiguredTokenBudget)
	assert.True(t, fallback.ModelContextWindowChecked)
	assert.False(t, fallback.InputFitsModelContextWindow)
	assert.Contains(t, fallback.TokenEstimator, "provider=fallback")
}

func TestRequestContextManifestForModelsResolvesDefaultModel(t *testing.T) {
	t.Parallel()

	registry := llm.NewRegistry()
	registry.Register(contextManifestBudgetProvider{name: "primary", models: []string{"tiny-default"}, window: 3})

	messages := []llm.Message{{Role: llm.RoleUser, Content: strings.Repeat("default budget ", 10)}}
	manifest := newRequestContextManifestForModels(
		registry,
		"",
		nil,
		messages,
		0,
		contextref.ReferenceManifest{},
	)

	assert.Equal(t, "tiny-default", manifest.Model)
	assert.Equal(t, 3, manifest.ModelContextWindow)
	assert.True(t, manifest.ModelContextWindowChecked)
	assert.False(t, manifest.InputFitsModelContextWindow)
	assert.Contains(t, manifest.TokenEstimator, "provider=primary")
	assert.Contains(t, manifest.TokenEstimator, "model=tiny-default")

	err := validateRequestBudgetWithFallbacks(registry, "", nil, messages, 0)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "tiny-default")
	assert.Contains(t, err.Error(), "context window")
}

func TestRequestContextManifestForModelsUsesKnownWindowWithoutRegistry(t *testing.T) {
	t.Parallel()

	messages := []llm.Message{{Role: llm.RoleUser, Content: strings.Repeat("window budget ", 3000)}}
	manifest := newRequestContextManifestForModels(
		nil,
		"openai/gpt-4",
		nil,
		messages,
		0,
		contextref.ReferenceManifest{},
	)

	assert.Equal(t, "openai/gpt-4", manifest.Model)
	assert.Equal(t, 8192, manifest.ModelContextWindow)
	assert.True(t, manifest.ModelContextWindowChecked)
	assert.False(t, manifest.InputFitsModelContextWindow)
	assert.Contains(t, manifest.TokenEstimator, "provider=openai")
	assert.Contains(t, manifest.TokenEstimator, "model=gpt-4")

	err := validateRequestBudgetWithFallbacks(nil, "openai/gpt-4", nil, messages, 0)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "openai/gpt-4")
	assert.Contains(t, err.Error(), "context window")
}

func TestRequestContextManifestForModelsUsesFirstFallbackWhenPrimaryEmpty(t *testing.T) {
	t.Parallel()

	registry := llm.NewRegistry()
	registry.Register(contextManifestBudgetProvider{name: "fallback", models: []string{"tiny", "backup"}, window: 10_000})

	manifest := newRequestContextManifestForModels(
		registry,
		"",
		[]string{"fallback/tiny", "fallback/backup"},
		[]llm.Message{{Role: llm.RoleUser, Content: "hello"}},
		0,
		contextref.ReferenceManifest{},
	)

	assert.Equal(t, "fallback/tiny", manifest.Model)
	assert.Contains(t, manifest.TokenEstimator, "provider=fallback")
	assert.Contains(t, manifest.TokenEstimator, "model=tiny")
	require.Len(t, manifest.FallbackModelEstimates, 1)
	assert.Equal(t, "fallback/backup", manifest.FallbackModelEstimates[0].Model)
}

func TestMergeReferenceManifestsPreservesManifestLevelEstimator(t *testing.T) {
	t.Parallel()

	merged := mergeReferenceManifests(
		contextref.ReferenceManifest{
			TokenEstimator: "openai-calibrated;provider=openai",
			Entries: []contextref.ReferenceEvent{
				{
					Source:         "../secret.md",
					Kind:           "file",
					Scope:          contextref.ReferenceScopeGlobal,
					Location:       "local",
					PolicyDecision: contextref.ReferenceDecisionRejected,
					PolicyReason:   "outside allowed local roots",
				},
			},
			RejectedCount: 1,
		},
		contextref.ReferenceManifest{
			TokenEstimator: "openai-calibrated;provider=openai",
			Entries: []contextref.ReferenceEvent{
				{
					Source:         "https://docs.example.com/style.md",
					Kind:           "url",
					Scope:          contextref.ReferenceScopeAgent,
					Location:       "remote",
					PolicyDecision: contextref.ReferenceDecisionSkipped,
					PolicyReason:   "max_total_bytes already reached",
				},
			},
			SkippedCount: 1,
		},
	)

	assert.Equal(t, "openai-calibrated;provider=openai", merged.TokenEstimator)
	assert.Equal(t, 1, merged.RejectedCount)
	assert.Equal(t, 1, merged.SkippedCount)
	require.Len(t, merged.Entries, 2)
	assert.Equal(t, contextref.ReferenceDecisionRejected, merged.Entries[0].PolicyDecision)
	assert.Equal(t, contextref.ReferenceDecisionSkipped, merged.Entries[1].PolicyDecision)
}

func TestValidateRequestBudgetWithFallbacksRejectsFallbackOverflow(t *testing.T) {
	t.Parallel()

	registry := llm.NewRegistry()
	registry.Register(contextManifestBudgetProvider{name: "primary", models: []string{"large"}, window: 10_000})
	registry.Register(contextManifestBudgetProvider{name: "fallback", models: []string{"tiny"}, window: 3})

	err := validateRequestBudgetWithFallbacks(
		registry,
		"primary/large",
		[]string{"fallback/tiny"},
		[]llm.Message{{Role: llm.RoleUser, Content: strings.Repeat("fallback budget ", 10)}},
		0,
	)

	require.Error(t, err)
	assert.Contains(t, err.Error(), "fallback/tiny")
	assert.Contains(t, err.Error(), "context window")
}
