// Package llm defines the provider-agnostic interface for calling large language models.
package llm

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"sort"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/tommoulard/atteler/pkg/events"
	"github.com/tommoulard/atteler/pkg/modelroute"
)

const (
	providerAnthropic  = "anthropic"
	providerClaudeCode = "claude-code"
	providerCodex      = "codex"
	providerOpenAI     = "openai"
	providerOllama     = "ollama"
)

// Role represents who authored a message.
type Role string

// Supported message roles.
const (
	RoleSystem    Role = "system"
	RoleUser      Role = "user"
	RoleAssistant Role = "assistant"
	RoleTool      Role = "tool"
)

// StopReason indicates why the model stopped generating.
type StopReason string

// Known stop reasons.
const (
	StopEndTurn StopReason = "end_turn" // Model finished normally.
	StopToolUse StopReason = "tool_use" // Model wants to call a tool.
	StopMaxToks StopReason = "max_tokens"
	StopUnknown StopReason = ""
)

// ToolDefinition describes a tool the model can call.
type ToolDefinition struct {
	Parameters  map[string]any `json:"parameters"`
	Name        string         `json:"name"`
	Description string         `json:"description"`
}

const (
	// ResponseFormatText leaves provider output unconstrained.
	ResponseFormatText = "text"

	// ResponseFormatJSONObject asks providers to return syntactically valid
	// JSON without enforcing a particular schema.
	ResponseFormatJSONObject = "json_object"

	// ResponseFormatJSONSchema asks providers to constrain output to Schema.
	ResponseFormatJSONSchema = "json_schema"
)

// ResponseFormat describes provider-agnostic structured-output constraints.
// Providers that cannot safely honor the requested format reject it instead of
// silently falling back to unconstrained text.
type ResponseFormat struct {
	Schema map[string]any `json:"schema,omitempty"`
	Type   string         `json:"type,omitempty"`
	Name   string         `json:"name,omitempty"`
	Strict bool           `json:"strict,omitempty"`
}

// ToolCall is a tool invocation requested by the model.
type ToolCall struct {
	Input map[string]any `json:"input"`
	ID    string         `json:"id"`
	Name  string         `json:"name"`
}

// ToolResult is the outcome of executing a tool call.
type ToolResult struct {
	ToolCallID string `json:"tool_call_id"`
	Content    string `json:"content"`
	IsError    bool   `json:"is_error,omitempty"`
}

// Message is a single turn in a conversation.
type Message struct {
	ToolResult *ToolResult `json:"tool_result,omitempty"`
	Role       Role        `json:"role"`
	Content    string      `json:"content"`
	ToolCalls  []ToolCall  `json:"tool_calls,omitempty"`
}

// CompleteParams groups the knobs for a completion call.
type CompleteParams struct {
	Temperature    *float64
	TopP           *float64
	Seed           *int
	ResponseFormat *ResponseFormat
	Model          string
	ModelMode      string
	ReasoningLevel string
	Messages       []Message
	Stop           []string
	Tools          []ToolDefinition
	MaxTokens      int
}

// Response is the provider-normalised result of a completion.
type Response struct {
	Content               string
	Provider              string // Provider that produced the response.
	Model                 string // Model that actually answered.
	StopReason            StopReason
	ToolCalls             []ToolCall
	Latency               time.Duration
	FirstTokenLatency     time.Duration
	InputTokens           int
	CachedInputTokens     int
	CacheWriteInputTokens int
	OutputTokens          int
}

// WantsToolUse returns true if the model stopped because it wants to call tools.
func (r *Response) WantsToolUse() bool {
	return r.StopReason == StopToolUse || len(r.ToolCalls) > 0
}

// Provider is the interface every LLM backend must implement.
type Provider interface {
	// Name returns a human-readable provider name (e.g. "anthropic").
	Name() string

	// Models lists the models this provider can serve.
	// It returns the offline/static catalog used by --list-known-models.
	Models() []string

	// FetchModels returns the configured provider's current model list. Some
	// providers hit the network or local daemon; others return a local catalog.
	// Callers that need an offline fallback should use Models().
	FetchModels(ctx context.Context) ([]string, error)

	// HealthCheck verifies provider readiness. Some providers hit the network;
	// others only validate local credentials or daemon reachability. It returns
	// nil when the provider is healthy.
	HealthCheck(ctx context.Context) error

	// Complete performs a chat completion.
	Complete(ctx context.Context, params CompleteParams) (*Response, error)

	// ModelContextWindow returns the context window size (in tokens) for the
	// given model. If the model is unknown the provider returns 0.
	ModelContextWindow(model string) int
}

// ModelProvenance records how the registry learned about a model claim.
type ModelProvenance string

// Model claim provenance values.
const (
	ModelProvenanceStatic          ModelProvenance = "static"
	ModelProvenanceFetchedLive     ModelProvenance = "fetched_live"
	ModelProvenanceConfiguredAlias ModelProvenance = "configured_alias"
	ModelProvenanceUserOverride    ModelProvenance = "user_override"
)

// ModelResolutionCandidate explains one provider claim considered during
// registry model resolution.
type ModelResolutionCandidate struct {
	ProviderName string
	Model        string
	Provenance   ModelProvenance
	Stale        bool
}

// ModelResolutionDiagnostic explains how a request model resolves, or why it
// cannot resolve safely.
//
//nolint:govet // Field order keeps diagnostic output fields grouped for callers.
type ModelResolutionDiagnostic struct {
	Error                     error
	RequestedModel            string
	ProviderName              string
	ProviderModel             string
	Reason                    string
	Provenance                ModelProvenance
	Candidates                []ModelResolutionCandidate
	DefaultProvider           string
	DefaultProviderConfigured bool
	Stale                     bool
}

// Resolved reports whether the diagnostic selected a concrete provider/model.
func (d ModelResolutionDiagnostic) Resolved() bool {
	return d.Error == nil && d.ProviderName != ""
}

type modelClaim struct {
	providerName string
	model        string
	provenance   ModelProvenance
}

// ---------------------------------------------------------------------------
// Registry
// ---------------------------------------------------------------------------

// Registry holds the set of available providers and resolves model -> provider.
//
//nolint:govet // Field order keeps registry state grouped by purpose.
type Registry struct {
	providers          map[string]Provider
	models             map[string]map[string]modelClaim
	catalogs           map[string]ProviderModelCatalog
	healthCache        map[string]providerHealthCacheEntry
	providerModels     map[string]map[string]bool
	modelProvenance    map[string]map[string]ModelProvenance
	catalogProvenance  map[string]map[string]ModelProvenance
	modelOverrides     map[string]map[string]bool
	providerRetries    map[string]retryConfig
	modelRoles         map[string]ModelRole
	providerModelsLive map[string]bool
	fallback           string
	defaultModel       string
	defaultConfigured  bool
	defaultProviderSet bool
	defaultQualified   bool
	readiness          ProviderReadinessReport
	routeTelemetry     *modelroute.Telemetry
	mu                 sync.RWMutex
	retry              retryConfig
}

// NewRegistry creates an empty registry with default retry settings.
func NewRegistry() *Registry {
	return &Registry{
		providers:          make(map[string]Provider),
		models:             make(map[string]map[string]modelClaim),
		catalogs:           make(map[string]ProviderModelCatalog),
		healthCache:        make(map[string]providerHealthCacheEntry),
		providerModels:     make(map[string]map[string]bool),
		modelProvenance:    make(map[string]map[string]ModelProvenance),
		catalogProvenance:  make(map[string]map[string]ModelProvenance),
		modelOverrides:     make(map[string]map[string]bool),
		providerRetries:    make(map[string]retryConfig),
		modelRoles:         make(map[string]ModelRole),
		providerModelsLive: make(map[string]bool),
		retry:              defaultRetryConfig(),
		routeTelemetry:     modelroute.NewTelemetry(),
	}
}

// SetRetry overrides the default retry policy. Pass a zero MaxAttempts to
// disable retries entirely, which is useful for fast test runs.
func (r *Registry) SetRetry(cfg RetryPolicy) {
	r.mu.Lock()
	defer r.mu.Unlock()

	r.retry = cfg
}

// SetProviderRetry overrides the retry policy for one provider. Pass a zero
// MaxAttempts to disable retries for that provider.
func (r *Registry) SetProviderRetry(providerName string, cfg RetryPolicy) {
	r.mu.Lock()
	defer r.mu.Unlock()

	if r.providerRetries == nil {
		r.providerRetries = make(map[string]retryConfig)
	}

	r.providerRetries[providerName] = cfg
}

func (r *Registry) applyProviderRetryConfig(providerName string, cfg RetryPolicyConfig) {
	if !cfg.hasOverrides() {
		return
	}

	r.mu.Lock()
	defer r.mu.Unlock()

	if r.providerRetries == nil {
		r.providerRetries = make(map[string]retryConfig)
	}

	r.providerRetries[providerName] = cfg.apply(r.retryPolicyForProviderLocked(providerName))
}

// RetryPolicyForProvider returns the active retry policy for diagnostics.
func (r *Registry) RetryPolicyForProvider(providerName string) RetryPolicyInfo {
	r.mu.RLock()
	defer r.mu.RUnlock()

	return r.retryPolicyForProviderLocked(providerName).info()
}

func (r *Registry) retryPolicyForProviderLocked(providerName string) retryConfig {
	if cfg, ok := r.providerRetries[providerName]; ok {
		return cfg
	}

	return r.retry
}

func contextualizeProviderError(providerName string, err error) error {
	var providerErr *ProviderError
	if errors.As(err, &providerErr) {
		if providerErr.Provider == "" {
			return fmt.Errorf("llm: %s: %w", providerName, err)
		}

		return err
	}

	return fmt.Errorf("llm: %s: %w", providerName, err)
}

// RouteTelemetry returns the registry-owned route telemetry store.
func (r *Registry) RouteTelemetry() *modelroute.Telemetry {
	r.mu.RLock()
	defer r.mu.RUnlock()

	return r.routeTelemetry
}

// SetRouteTelemetry replaces the registry-owned route telemetry store. Passing
// nil disables telemetry recording.
func (r *Registry) SetRouteTelemetry(telemetry *modelroute.Telemetry) {
	r.mu.Lock()
	defer r.mu.Unlock()

	r.routeTelemetry = telemetry
}

// Register adds a provider and indexes all of its models.
func (r *Registry) Register(p Provider) {
	r.mu.Lock()
	defer r.mu.Unlock()

	providerName := p.Name()
	r.providers[providerName] = p
	r.providerModelsLive[providerName] = false
	r.indexProviderModelsLocked(providerName, p.Models(), ModelProvenanceStatic)
	r.setStaticProviderCatalogLocked(providerName, p.Models())
	r.markProviderRegisteredLocked(providerName, p.Models())

	// First registered provider becomes the default.
	if r.fallback == "" {
		r.fallback = providerName
	}
}

// SetDefault changes the fallback provider used when the model is empty.
func (r *Registry) SetDefault(name string) error {
	name = strings.TrimSpace(name)

	r.mu.Lock()
	defer r.mu.Unlock()

	if _, ok := r.providers[name]; !ok {
		return fmt.Errorf("llm: unknown provider %q", name)
	}

	r.fallback = name
	r.defaultModel = ""
	r.defaultConfigured = true
	r.defaultProviderSet = true
	r.defaultQualified = false

	return nil
}

// SetDefaultModel changes the fallback model used when CompleteParams.Model is
// empty. Bare models must resolve through an exact claim or configured alias;
// provider-qualified models may introduce a user override for private model IDs.
func (r *Registry) SetDefaultModel(model string) error {
	model = strings.TrimSpace(model)
	if model == "" {
		return errors.New("llm: default model cannot be empty")
	}

	r.mu.Lock()
	defer r.mu.Unlock()

	if _, ok := r.modelRoles[model]; ok {
		r.defaultModel = model
		r.defaultConfigured = true
		r.defaultProviderSet = false
		r.defaultQualified = false

		return nil
	}

	if providerName, providerModel, ok := splitProviderModel(model); ok {
		if _, ok := r.providers[providerName]; ok {
			if r.defaultProviderSet && !strings.EqualFold(providerName, r.fallback) {
				return defaultModelProviderMismatchError(r.fallback, providerName, model)
			}

			r.indexProviderModelLocked(providerName, providerModel, ModelProvenanceUserOverride)
			r.recordProviderModelOverrideLocked(providerName, providerModel)
			r.fallback = providerName
			r.defaultModel = providerModel
			r.defaultConfigured = true
			r.defaultProviderSet = true
			r.defaultQualified = true

			return nil
		}
	}

	diagnostic := r.explainModelResolutionLocked(model)
	if diagnostic.Error != nil {
		if r.defaultProviderSet && len(diagnostic.Candidates) > 0 {
			return defaultModelProviderMismatchError(
				r.fallback,
				strings.Join(modelResolutionCandidateProviderNames(diagnostic.Candidates), ", "),
				model,
			)
		}

		return diagnostic.Error
	}

	if r.defaultProviderSet && diagnostic.ProviderName != r.fallback {
		return defaultModelProviderMismatchError(r.fallback, diagnostic.ProviderName, model)
	}

	r.fallback = diagnostic.ProviderName
	r.defaultModel = model
	r.defaultConfigured = true
	r.defaultQualified = false

	return nil
}

// SetDefaultProviderModel changes the fallback provider/model pair. It can be
// used for configured live-only model IDs before ProviderModels has fetched and
// indexed the provider's live model list.
func (r *Registry) SetDefaultProviderModel(providerName, model string) error {
	providerName = strings.TrimSpace(providerName)
	model = strings.TrimSpace(model)

	if model == "" {
		return errors.New("llm: default model cannot be empty")
	}

	r.mu.Lock()
	defer r.mu.Unlock()

	if _, ok := r.providers[providerName]; !ok {
		return fmt.Errorf("llm: unknown provider %q", providerName)
	}

	r.indexProviderModelLocked(providerName, model, ModelProvenanceUserOverride)
	r.recordProviderModelOverrideLocked(providerName, model)
	r.fallback = providerName
	r.defaultModel = model
	r.defaultConfigured = true
	r.defaultProviderSet = true
	r.defaultQualified = true

	return nil
}

// SetModelAlias maps a bare alias to a provider-local model. Aliases are
// explicit user configuration and take part in collision-safe bare model
// resolution just like provider catalog entries.
func (r *Registry) SetModelAlias(alias, providerName, providerModel string) error {
	alias = strings.TrimSpace(alias)
	providerName = strings.TrimSpace(providerName)
	providerModel = strings.TrimSpace(providerModel)

	if alias == "" {
		return errors.New("llm: model alias cannot be empty")
	}

	if providerModel == "" {
		return errors.New("llm: alias target model cannot be empty")
	}

	if strings.Contains(alias, "/") {
		return fmt.Errorf("llm: model alias %q must be a bare model name", alias)
	}

	r.mu.Lock()
	defer r.mu.Unlock()

	if _, ok := r.providers[providerName]; !ok {
		return fmt.Errorf("llm: unknown provider %q", providerName)
	}

	r.indexModelClaimLocked(providerName, alias, providerModel, ModelProvenanceConfiguredAlias)

	return nil
}

// SetProviderModelOverride records a provider-local model explicitly selected
// by user configuration without changing the registry default.
func (r *Registry) SetProviderModelOverride(providerName, model string) error {
	providerName = strings.TrimSpace(providerName)
	model = strings.TrimSpace(model)

	if model == "" {
		return errors.New("llm: model override cannot be empty")
	}

	r.mu.Lock()
	defer r.mu.Unlock()

	if _, ok := r.providers[providerName]; !ok {
		return fmt.Errorf("llm: unknown provider %q", providerName)
	}

	r.indexProviderModelLocked(providerName, model, ModelProvenanceUserOverride)
	r.recordProviderModelOverrideLocked(providerName, model)

	return nil
}

// Complete resolves the provider for params.Model and calls it.
// If Model is empty the default provider is used with its first listed model.
// Transient errors (429, 5xx) are retried according to the registry's retry
// configuration.
func (r *Registry) Complete(ctx context.Context, params CompleteParams) (*Response, error) {
	if err := requireCredentialContext(ctx); err != nil {
		return nil, err
	}

	if routedParams, routedFallbacks, routed, err := r.routeModelRoleRequest(params, nil); err != nil {
		return nil, err
	} else if routed {
		params = routedParams
		if len(routedFallbacks) > 0 {
			return r.completeResolvedWithFallback(ctx, params, routedFallbacks)
		}
	}

	return r.completeResolved(ctx, params)
}

func (r *Registry) completeResolved(ctx context.Context, params CompleteParams) (*Response, error) {
	p, params, err := r.resolve(params)
	if err != nil {
		return nil, err
	}

	params, adjustments, err := prepareRoutedCompleteParamsForProviderCapabilities(p.Name(), ProviderCapabilitiesFor(p), params)
	if err != nil {
		return nil, err
	}

	if validateErr := validateCompleteParamsAgainstDeclaredCapabilities(p, params); validateErr != nil {
		return nil, validateErr
	}

	// Snapshot retry config under the lock so concurrent SetRetry calls are safe.
	r.mu.RLock()
	retryCfg := r.retryPolicyForProviderLocked(p.Name())
	r.mu.RUnlock()

	emitToolExecute(ctx, p, params, adjustments)

	startedAt := time.Now()

	resp, err := completeWithRetry(ctx, retryCfg, retryMetadata{provider: p.Name(), model: params.Model}, func(ctx context.Context) (*Response, error) {
		providerResp, completeErr := p.Complete(ctx, params)
		if completeErr != nil {
			wrappedErr := contextualizeProviderError(p.Name(), completeErr)
			r.recordRouteFailure(p.Name(), params.Model, wrappedErr)

			return nil, wrappedErr
		}

		return providerResp, nil
	})
	if err != nil {
		return nil, err
	}

	latency := time.Since(startedAt)
	if resp.Latency <= 0 {
		resp.Latency = latency
	}

	if resp.Provider == "" {
		resp.Provider = p.Name()
	}

	if resp.Model == "" {
		resp.Model = params.Model
	}

	r.recordRouteObservation(p.Name(), params.Model, resp, resp.Latency)

	return resp, nil
}

func (r *Registry) completeResolvedWithFallback(
	ctx context.Context,
	params CompleteParams,
	fallbackModels []string,
) (*Response, error) {
	models := modelFallbackChain(params.Model, fallbackModels)
	if len(models) == 0 {
		return r.completeResolved(ctx, params)
	}

	var failures []error

	for _, model := range models {
		if err := ctx.Err(); err != nil {
			return nil, fmt.Errorf("llm: fallback canceled: %w", err)
		}

		next := params
		next.Model = model

		resp, err := r.completeResolved(ctx, next)
		if err == nil {
			return resp, nil
		}

		failures = append(failures, fmt.Errorf("%s via %s: %w", model, r.modelResolutionLabel(model), err))
	}

	return nil, r.withReadinessContext(fmt.Errorf("llm: all fallback models failed: %w", joinFallbackFailures(failures)))
}

// CompleteWithFallback tries params.Model followed by fallbackModels until one
// completion succeeds. If neither params.Model nor fallbackModels are set, it
// behaves like Complete.
func (r *Registry) CompleteWithFallback(
	ctx context.Context,
	params CompleteParams,
	fallbackModels []string,
) (*Response, error) {
	if err := requireCredentialContext(ctx); err != nil {
		return nil, err
	}

	if routedParams, routedFallbacks, routed, err := r.routeModelRoleRequest(params, fallbackModels); err != nil {
		return nil, err
	} else if routed {
		params = routedParams
		fallbackModels = routedFallbacks
	}

	models := modelFallbackChain(params.Model, fallbackModels)
	if len(models) == 0 {
		return r.Complete(ctx, params)
	}

	var failures []fallbackAttemptFailure

	rateLimitedProviders := make(map[string]fallbackAttemptFailure)

	for _, model := range models {
		if err := ctx.Err(); err != nil {
			return nil, fmt.Errorf("llm: fallback canceled: %w", err)
		}

		target := r.fallbackAttemptTarget(model)
		if prior, ok := rateLimitedProviders[target.providerName]; ok && target.resolved && target.providerName != "" {
			skipped := skippedRateLimitFailure(model, target, prior)
			failures = append(failures, skipped)
			r.recordRouteFailureWithScope(
				target.providerName,
				fallbackObservationModel(model, target),
				skipped.err,
				skipped.rateLimitScope,
			)

			continue
		}

		if skipped, ok := r.providerCooldownFailure(model, target); ok {
			failures = append(failures, skipped)
			r.recordRouteFailureWithScope(
				target.providerName,
				fallbackObservationModel(model, target),
				skipped.err,
				skipped.rateLimitScope,
			)

			continue
		}

		next := params
		next.Model = model

		resp, err := r.Complete(ctx, next)
		if err == nil {
			return resp, nil
		}

		failure := fallbackAttemptFailure{
			err:            err,
			classification: classifyProviderFailure(err),
			model:          model,
			providerName:   target.providerName,
			label:          target.label,
		}
		if failure.classification.RateLimited {
			failure.rateLimitScope = modelroute.RateLimitScopeProvider
		}

		failures = append(failures, failure)

		if failure.classification.RateLimited && target.providerName != "" {
			rateLimitedProviders[target.providerName] = failure
		}
	}

	r.mu.RLock()
	readiness := r.readinessReportLocked()
	r.mu.RUnlock()

	return nil, newFallbackError(failures, readiness)
}

func fallbackObservationModel(model string, target fallbackAttemptTarget) string {
	if target.providerModel != "" {
		return target.providerModel
	}

	return model
}

func (r *Registry) providerCooldownFailure(model string, target fallbackAttemptTarget) (fallbackAttemptFailure, bool) {
	if !target.resolved || target.providerName == "" {
		return fallbackAttemptFailure{}, false
	}

	r.mu.RLock()
	telemetry := r.routeTelemetry
	r.mu.RUnlock()

	if telemetry == nil {
		return fallbackAttemptFailure{}, false
	}

	now := time.Now().UTC()

	var (
		cooldownObs   modelroute.Observation
		cooldownScope string
	)

	if obs, ok := telemetry.ProviderRateLimitObservation(target.providerName, now); ok {
		cooldownObs = obs
		cooldownScope = modelroute.RateLimitScopeProvider
	}

	cooldownModel := model
	if target.providerModel != "" {
		cooldownModel = target.providerModel
	}

	candidate, _ := routeObservationCandidate(target.providerName, cooldownModel)

	if obs, ok := telemetry.Snapshot(candidate.ID()); ok && obs.RateLimitActive(now) {
		if cooldownScope != "" && !cooldownObs.RateLimitUntil().Before(obs.RateLimitUntil()) {
			return skippedProviderCooldownFailure(
				model,
				target,
				cooldownObs.RateLimitUntil(),
				cooldownObs.LastError,
				cooldownScope,
			), true
		}

		return skippedProviderCooldownFailure(
			model,
			target,
			obs.RateLimitUntil(),
			obs.LastError,
			modelroute.RateLimitScopeModel,
		), true
	}

	if cooldownScope != "" {
		return skippedProviderCooldownFailure(
			model,
			target,
			cooldownObs.RateLimitUntil(),
			cooldownObs.LastError,
			cooldownScope,
		), true
	}

	return fallbackAttemptFailure{}, false
}

func (r *Registry) resolve(params CompleteParams) (Provider, CompleteParams, error) {
	r.mu.RLock()
	defer r.mu.RUnlock()

	if params.Model != "" {
		return r.resolveExplicitModelLocked(params)
	}

	if r.defaultModel != "" {
		diagnostic := r.explainDefaultModelLocked()
		if diagnostic.Error != nil {
			return nil, params, r.resolutionErrorForModelLocked(
				diagnostic.Error,
				r.defaultModel,
			)
		}

		p, ok := r.providers[diagnostic.ProviderName]
		if !ok {
			return nil, params, r.resolutionErrorForModelLocked(
				fmt.Errorf("llm: unknown provider %q", diagnostic.ProviderName),
				r.defaultModel,
			)
		}

		params.Model = diagnostic.ProviderModel

		return p, params, nil
	}

	p, ok := r.providers[r.fallback]
	if !ok {
		return nil, params, r.resolutionErrorLocked(errors.New("llm: no providers registered"))
	}

	if models := r.defaultProviderModelsLocked(r.fallback, p); len(models) > 0 {
		params.Model = models[0]
	}

	return p, params, nil
}

func emitToolExecute(ctx context.Context, provider Provider, params CompleteParams, adjustments []completeParamAdjustment) {
	metadata := map[string]string{
		"provider": provider.Name(),
		"tool":     "llm.complete",
	}
	if level := normalizeReasoningLevel(params.ReasoningLevel); level != "" {
		metadata["reasoning_level"] = level
	}

	if mode := normalizeModelMode(params.ModelMode); mode != "" {
		metadata["model_mode"] = mode
		if tier := openAIServiceTierForProviderModelMode(provider.Name(), mode); tier != "" {
			metadata["service_tier"] = tier
		}
	}

	if len(adjustments) > 0 {
		metadata["option_adjustments"] = formatCompleteParamAdjustments(adjustments)
	}

	emitActivity(ctx, events.Event{
		Type:     events.ToolExecute,
		Model:    params.Model,
		Metadata: metadata,
	})
}

func openAIServiceTierForProviderModelMode(providerName, mode string) string {
	switch providerName {
	case providerOpenAI, providerCodex:
		return openAIServiceTierForModelMode(mode)
	default:
		return ""
	}
}

func formatCompleteParamAdjustments(adjustments []completeParamAdjustment) string {
	parts := make([]string, 0, len(adjustments))
	for _, adjustment := range adjustments {
		part := adjustment.Name + " " + adjustment.Action
		if adjustment.Reason != "" {
			part += ": " + adjustment.Reason
		}

		parts = append(parts, part)
	}

	return strings.Join(parts, "; ")
}

func (r *Registry) recordRouteFailure(providerName, requestedModel string, err error) {
	r.recordRouteFailureWithScope(providerName, requestedModel, err, "")
}

func (r *Registry) recordRouteFailureWithScope(providerName, requestedModel string, err error, rateLimitScope string) {
	if err == nil {
		return
	}

	r.mu.RLock()
	telemetry := r.routeTelemetry
	r.mu.RUnlock()

	if telemetry == nil {
		return
	}

	candidate, _ := routeObservationCandidate(providerName, requestedModel)
	decision := retryDecisionForError(err)
	classification := classifyProviderFailure(err)

	telemetry.RecordFailure(candidate, modelroute.Failure{
		RetryAfter:     decision.retryAfter,
		Error:          err.Error(),
		Kind:           string(classification.Kind),
		RateLimitScope: rateLimitScope,
		Retryable:      decision.retryable,
		RateLimited:    classification.RateLimited,
	}, time.Now().UTC())
}

func (r *Registry) recordRouteObservation(providerName, requestedModel string, resp *Response, latency time.Duration) {
	if resp == nil {
		return
	}

	r.mu.RLock()
	telemetry := r.routeTelemetry
	r.mu.RUnlock()

	if telemetry == nil {
		return
	}

	model := strings.TrimSpace(resp.Model)
	if model == "" {
		model = requestedModel
	}

	candidate := routeObservationCandidateForResponse(providerName, requestedModel, model)

	recordRouteObservationForTelemetry(telemetry, candidate, resp, latency)
}

func recordRouteObservationForTelemetry(telemetry *modelroute.Telemetry, candidate modelroute.Candidate, resp *Response, latency time.Duration) {
	telemetry.Record(candidate, modelroute.ActualUsage{
		Latency:           latency,
		TTFT:              resp.FirstTokenLatency,
		InputTokens:       resp.InputTokens,
		CachedInputTokens: resp.CachedInputTokens,
		CacheWriteTokens:  resp.CacheWriteInputTokens,
		OutputTokens:      resp.OutputTokens,
	}, time.Now().UTC())
}

func routeObservationCandidateForResponse(providerName, requestedModel, responseModel string) modelroute.Candidate {
	candidate, catalogBacked := routeObservationCandidate(providerName, responseModel)
	if !catalogBacked && responseModel != requestedModel {
		if requestedCandidate, ok := catalogRouteObservationCandidate(providerName, requestedModel); ok {
			return requestedCandidate
		}
	}

	return candidate
}

func routeObservationCandidate(providerName, model string) (modelroute.Candidate, bool) {
	if candidate, ok := catalogRouteObservationCandidate(providerName, model); ok {
		return candidate, true
	}

	if qualifiedProvider, providerModel, ok := splitProviderModel(model); ok {
		providerName = qualifiedProvider
		model = providerModel
	}

	return modelroute.Candidate{Provider: providerName, Name: model}, false
}

func catalogRouteObservationCandidate(providerName, model string) (modelroute.Candidate, bool) {
	if qualifiedProvider, providerModel, ok := splitProviderModel(model); ok {
		providerName = qualifiedProvider
		model = providerModel
	}

	catalog := modelroute.BuiltinCatalog()
	metadata, ok := catalog.Lookup(providerName, model)

	if ok {
		candidate := metadata.Candidate(0)
		candidate.MetadataVersion = catalog.Version

		return candidate, true
	}

	return modelroute.Candidate{}, false
}

func (r *Registry) resolveExplicitModelLocked(params CompleteParams) (Provider, CompleteParams, error) {
	diagnostic := r.explainModelResolutionLocked(params.Model)
	if diagnostic.Error != nil {
		return nil, params, r.resolutionErrorForModelLocked(diagnostic.Error, params.Model)
	}

	p, ok := r.providers[diagnostic.ProviderName]
	if !ok {
		return nil, params, r.resolutionErrorForModelLocked(
			fmt.Errorf("llm: unknown provider %q", diagnostic.ProviderName),
			params.Model,
		)
	}

	params.Model = diagnostic.ProviderModel

	return p, params, nil
}

// ExplainModelResolution returns a diagnostic for the provider/model that the
// registry would use for model without making a provider call.
func (r *Registry) ExplainModelResolution(model string) ModelResolutionDiagnostic {
	r.mu.RLock()
	defer r.mu.RUnlock()

	return r.explainModelResolutionLocked(model)
}

func (r *Registry) explainModelResolutionLocked(model string) ModelResolutionDiagnostic {
	model = strings.TrimSpace(model)
	if model == "" {
		return r.explainDefaultModelLocked()
	}

	diagnostic := ModelResolutionDiagnostic{
		RequestedModel:            model,
		DefaultProvider:           r.fallback,
		DefaultProviderConfigured: r.defaultConfigured,
	}

	if role, ok := r.modelRoles[model]; ok {
		return r.explainModelRoleLocked(diagnostic, role)
	}

	providerName, providerModel, providerQualified := splitProviderModel(model)
	if qualifiedDiagnostic, ok := r.explainProviderQualifiedModelLocked(
		diagnostic,
		providerName,
		providerModel,
		providerQualified,
	); ok {
		return qualifiedDiagnostic
	}

	candidates := r.modelResolutionCandidatesLocked(model)
	diagnostic.Candidates = candidates

	if len(candidates) == 0 {
		if providerQualified {
			diagnostic.Error = fmt.Errorf("llm: unknown provider %q", providerName)
			diagnostic.Reason = "provider-qualified model names require a registered provider"

			return diagnostic
		}

		diagnostic.Error = fmt.Errorf("llm: unknown model %q", model)
		diagnostic.Reason = "no registered provider catalog, live fetch, configured alias, or user override claims this bare model"

		return diagnostic
	}

	if len(candidates) == 1 {
		candidate := candidates[0]
		diagnostic.ProviderName = candidate.ProviderName
		diagnostic.ProviderModel = candidate.Model
		diagnostic.Provenance = candidate.Provenance
		diagnostic.Stale = candidate.Stale
		diagnostic.Reason = exactModelMatchReason(providerQualified)

		return diagnostic
	}

	if r.defaultConfigured {
		for _, candidate := range candidates {
			if candidate.ProviderName != r.fallback {
				continue
			}

			diagnostic.ProviderName = candidate.ProviderName
			diagnostic.ProviderModel = candidate.Model
			diagnostic.Provenance = candidate.Provenance
			diagnostic.Stale = candidate.Stale
			diagnostic.Reason = fmt.Sprintf(
				"%s is claimed by multiple providers; configured default provider %q selected a deterministic match",
				modelResolutionRequestKind(providerQualified),
				r.fallback,
			)

			return diagnostic
		}
	}

	diagnostic.Error = fmt.Errorf(
		"llm: ambiguous model %q claimed by providers: %s",
		model,
		strings.Join(modelResolutionCandidateProviderNames(candidates), ", "),
	)
	diagnostic.Reason = modelResolutionRequestKind(providerQualified) +
		" is ambiguous; use provider/model or configure a default provider"

	return diagnostic
}

func (r *Registry) explainModelRoleLocked(
	diagnostic ModelResolutionDiagnostic,
	role ModelRole,
) ModelResolutionDiagnostic {
	diagnostic.Reason = "model role selected the first resolvable preferred/fallback model"

	for _, model := range r.expandedModelRoleChainLocked(diagnostic.RequestedModel, role, nil) {
		if strings.TrimSpace(model) == "" || strings.TrimSpace(model) == diagnostic.RequestedModel {
			continue
		}

		candidate := r.explainModelResolutionLocked(model)
		if candidate.Error != nil {
			diagnostic.Candidates = append(diagnostic.Candidates, candidate.Candidates...)

			continue
		}

		diagnostic.ProviderName = candidate.ProviderName
		diagnostic.ProviderModel = candidate.ProviderModel
		diagnostic.Provenance = candidate.Provenance
		diagnostic.Stale = candidate.Stale
		diagnostic.Candidates = candidate.Candidates

		return diagnostic
	}

	diagnostic.Error = fmt.Errorf("llm: model role %q has no resolvable candidates", diagnostic.RequestedModel)

	return diagnostic
}

func (r *Registry) explainProviderQualifiedModelLocked(
	diagnostic ModelResolutionDiagnostic,
	providerName string,
	providerModel string,
	providerQualified bool,
) (ModelResolutionDiagnostic, bool) {
	if !providerQualified {
		return diagnostic, false
	}

	p, ok := r.providers[providerName]
	if !ok {
		return diagnostic, false
	}

	diagnostic.ProviderName = p.Name()
	diagnostic.ProviderModel = providerModel
	diagnostic.Provenance = r.providerQualifiedModelProvenanceLocked(providerName, providerModel)
	diagnostic.Stale = r.modelClaimStaleLocked(providerName, providerModel, diagnostic.Provenance)
	diagnostic.Candidates = []ModelResolutionCandidate{{
		ProviderName: providerName,
		Model:        diagnostic.ProviderModel,
		Provenance:   diagnostic.Provenance,
		Stale:        diagnostic.Stale,
	}}
	diagnostic.Reason = "provider-qualified model selected provider directly"

	return diagnostic, true
}

func (r *Registry) providerQualifiedModelProvenanceLocked(providerName, providerModel string) ModelProvenance {
	if claim, ok := r.models[providerModel][providerName]; ok &&
		claim.model == providerModel &&
		claim.provenance != ModelProvenanceConfiguredAlias {
		return claim.provenance
	}

	if provenance, ok := r.providerModelCatalogProvenanceLocked(providerName, providerModel); ok {
		return provenance
	}

	return ModelProvenanceUserOverride
}

func exactModelMatchReason(providerQualified bool) string {
	if providerQualified {
		return "provider prefix was not registered; full model ID matched exactly one registered provider claim"
	}

	return "bare model matched exactly one registered provider claim"
}

func modelResolutionRequestKind(providerQualified bool) string {
	if providerQualified {
		return "model ID"
	}

	return "bare model"
}

//nolint:nestif // Keeps default-model diagnostic branches together for explain output.
func (r *Registry) explainDefaultModelLocked() ModelResolutionDiagnostic {
	diagnostic := ModelResolutionDiagnostic{
		DefaultProvider:           r.fallback,
		DefaultProviderConfigured: r.defaultConfigured,
	}

	if r.defaultModel != "" {
		if role, ok := r.modelRoles[r.defaultModel]; ok {
			return r.explainDefaultModelRoleLocked(diagnostic, role)
		}

		p, ok := r.providers[r.fallback]
		if !ok {
			diagnostic.RequestedModel = r.defaultModel
			diagnostic.Error = fmt.Errorf("llm: unknown default model %q", r.defaultModel)
			diagnostic.Reason = "configured default model points at an unavailable provider"

			return diagnostic
		}

		diagnostic.RequestedModel = r.defaultModel
		diagnostic.ProviderName = p.Name()
		diagnostic.ProviderModel = r.defaultModel

		diagnostic.Provenance = r.providerModelProvenanceLocked(p.Name(), r.defaultModel)
		if r.defaultQualified {
			diagnostic.Provenance = ModelProvenanceUserOverride
		} else if claim, ok := r.models[r.defaultModel][p.Name()]; ok {
			diagnostic.ProviderModel = claim.model
			diagnostic.Provenance = claim.provenance
		}

		if diagnostic.Provenance == "" {
			diagnostic.Provenance = ModelProvenanceUserOverride
		}

		diagnostic.Stale = r.modelClaimStaleLocked(p.Name(), diagnostic.ProviderModel, diagnostic.Provenance)

		diagnostic.Candidates = []ModelResolutionCandidate{{
			ProviderName: p.Name(),
			Model:        diagnostic.ProviderModel,
			Provenance:   diagnostic.Provenance,
			Stale:        diagnostic.Stale,
		}}
		diagnostic.Reason = "empty request used configured default model"

		return diagnostic
	}

	p, ok := r.providers[r.fallback]
	if !ok {
		diagnostic.Error = errors.New("llm: no providers registered")
		diagnostic.Reason = "empty request has no registered fallback provider"

		return diagnostic
	}

	diagnostic.ProviderName = p.Name()

	diagnostic.Reason = "empty request used default provider"
	if models := r.defaultProviderModelsLocked(p.Name(), p); len(models) > 0 {
		diagnostic.RequestedModel = models[0]
		diagnostic.ProviderModel = models[0]

		diagnostic.Provenance = r.providerModelProvenanceLocked(p.Name(), models[0])
		if diagnostic.Provenance == "" {
			diagnostic.Provenance = ModelProvenanceStatic
		}

		diagnostic.Stale = r.modelClaimStaleLocked(p.Name(), models[0], diagnostic.Provenance)

		diagnostic.Candidates = []ModelResolutionCandidate{{
			ProviderName: p.Name(),
			Model:        models[0],
			Provenance:   diagnostic.Provenance,
			Stale:        diagnostic.Stale,
		}}
	}

	return diagnostic
}

func (r *Registry) explainDefaultModelRoleLocked(
	diagnostic ModelResolutionDiagnostic,
	role ModelRole,
) ModelResolutionDiagnostic {
	diagnostic.RequestedModel = r.defaultModel
	diagnostic.Reason = "empty request used configured model role"

	for _, model := range r.expandedModelRoleChainLocked(r.defaultModel, role, nil) {
		if strings.TrimSpace(model) == "" || strings.TrimSpace(model) == r.defaultModel {
			continue
		}

		candidate := r.explainModelResolutionLocked(model)
		if candidate.Error != nil {
			diagnostic.Candidates = append(diagnostic.Candidates, candidate.Candidates...)

			continue
		}

		diagnostic.ProviderName = candidate.ProviderName
		diagnostic.ProviderModel = candidate.ProviderModel
		diagnostic.Provenance = candidate.Provenance
		diagnostic.Stale = candidate.Stale
		diagnostic.Candidates = candidate.Candidates

		return diagnostic
	}

	diagnostic.Error = fmt.Errorf("llm: model role %q has no resolvable candidates", r.defaultModel)

	return diagnostic
}

func (r *Registry) defaultProviderModelsLocked(providerName string, p Provider) []string {
	if catalog, ok := r.catalogs[providerName]; ok {
		if len(catalog.LiveModels) > 0 {
			return append([]string(nil), catalog.LiveModels...)
		}

		if len(catalog.StaticModels) > 0 {
			return append([]string(nil), catalog.StaticModels...)
		}

		if len(catalog.Models) > 0 {
			return append([]string(nil), catalog.Models...)
		}
	}

	return p.Models()
}

func (r *Registry) modelResolutionCandidatesLocked(model string) []ModelResolutionCandidate {
	claims := r.models[model]
	if len(claims) == 0 {
		return nil
	}

	providers := make([]string, 0, len(claims))
	for providerName := range claims {
		providers = append(providers, providerName)
	}

	sort.Strings(providers)

	candidates := make([]ModelResolutionCandidate, 0, len(providers))
	for _, providerName := range providers {
		claim := claims[providerName]
		candidates = append(candidates, ModelResolutionCandidate{
			ProviderName: claim.providerName,
			Model:        claim.model,
			Provenance:   claim.provenance,
			Stale:        r.modelClaimStaleLocked(claim.providerName, claim.model, claim.provenance),
		})
	}

	return candidates
}

func (r *Registry) modelClaimStaleLocked(providerName, model string, provenance ModelProvenance) bool {
	if !isCatalogProvenance(provenance) && provenance != ModelProvenanceConfiguredAlias {
		return false
	}

	// Configured aliases are fresh user intent, but an alias target that only
	// appears in a stale static fallback should still disclose that catalog age.
	catalog, ok := r.catalogs[providerName]
	if !ok || !catalog.Stale {
		return false
	}

	catalogProvenance, ok := r.providerModelCatalogProvenanceLocked(providerName, model)

	return ok && isCatalogProvenance(catalogProvenance)
}

func modelResolutionCandidateProviderNames(candidates []ModelResolutionCandidate) []string {
	names := make([]string, 0, len(candidates))
	for _, candidate := range candidates {
		names = append(names, candidate.ProviderName)
	}

	return names
}

func splitProviderModel(model string) (providerName, providerModel string, ok bool) {
	providerName, providerModel, ok = strings.Cut(strings.TrimSpace(model), "/")
	if !ok {
		return "", "", false
	}

	providerName = strings.TrimSpace(providerName)

	providerModel = strings.TrimSpace(providerModel)
	if providerName == "" || providerModel == "" {
		return "", "", false
	}

	return providerName, providerModel, true
}

func modelFallbackChain(primary string, fallbacks []string) []string {
	var out []string

	seen := make(map[string]bool, len(fallbacks)+1)
	for _, model := range append([]string{primary}, fallbacks...) {
		if model != "" && !seen[model] {
			out = append(out, model)
			seen[model] = true
		}
	}

	return out
}

// ListModels returns every model across all registered providers.
func (r *Registry) ListModels() []string {
	r.mu.RLock()
	defer r.mu.RUnlock()

	out := make([]string, 0, len(r.models))
	for m := range r.models {
		out = append(out, m)
	}

	return out
}

// Provider returns a provider by name.
func (r *Registry) Provider(name string) (Provider, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()

	p, ok := r.providers[name]

	return p, ok
}

// ProviderForModel returns the provider name that would handle model.
func (r *Registry) ProviderForModel(model string) (string, bool) {
	providerName, _, ok := r.ResolveModel(model)

	return providerName, ok
}

// ResolveModel returns the provider and provider-local model that would be used
// for a request model. An empty model resolves through the same default-provider
// path as Complete, which lets callers preflight budget and context-window
// checks before making a provider call.
func (r *Registry) ResolveModel(model string) (providerName, providerModel string, ok bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()

	diagnostic := r.explainModelResolutionLocked(model)
	if diagnostic.Error != nil {
		return "", "", false
	}

	return diagnostic.ProviderName, diagnostic.ProviderModel, true
}

// ProviderHasModel reports whether providerName has model in the registry's
// static, fetched, alias, or override model index. It does not make network
// requests.
func (r *Registry) ProviderHasModel(providerName, model string) bool {
	r.mu.RLock()
	defer r.mu.RUnlock()

	providerName = strings.TrimSpace(providerName)
	model = strings.TrimSpace(model)

	if providerName == "" || model == "" {
		return false
	}

	models := r.providerModels[providerName]

	return models != nil && models[model]
}

// ProviderModelProvenance reports how providerName's claim for model was
// learned. It does not make network requests.
func (r *Registry) ProviderModelProvenance(providerName, model string) (ModelProvenance, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()

	providerName = strings.TrimSpace(providerName)
	model = strings.TrimSpace(model)

	if providerName == "" || model == "" || r.modelProvenance == nil {
		return "", false
	}

	provenance, ok := r.modelProvenance[providerName][model]

	return provenance, ok
}

// ProviderModelCatalogProvenance reports the static/live catalog provenance for
// providerName/model, even when a configured alias with the same local model
// name owns bare-model resolution.
func (r *Registry) ProviderModelCatalogProvenance(providerName, model string) (ModelProvenance, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()

	providerName = strings.TrimSpace(providerName)
	model = strings.TrimSpace(model)

	return r.providerModelCatalogProvenanceLocked(providerName, model)
}

func (r *Registry) providerModelCatalogProvenanceLocked(providerName, model string) (ModelProvenance, bool) {
	if providerName == "" || model == "" || r.catalogProvenance == nil {
		return "", false
	}

	provenance, ok := r.catalogProvenance[providerName][model]

	return provenance, ok
}

// ProviderModelUserOverride reports whether providerName/model was explicitly
// configured as a provider-qualified model ID. It is separate from
// ProviderModelProvenance so a configured alias can keep owning bare-model
// resolution while provider/model routing remains available for a private
// deployment with the same local name.
func (r *Registry) ProviderModelUserOverride(providerName, model string) bool {
	r.mu.RLock()
	defer r.mu.RUnlock()

	providerName = strings.TrimSpace(providerName)
	model = strings.TrimSpace(model)

	if providerName == "" || model == "" || r.modelOverrides == nil {
		return false
	}

	return r.modelOverrides[providerName][model]
}

// IndexedProviderModels returns a copy of the registry's provider-specific
// static/fetched/alias/override model index. It does not make network requests.
func (r *Registry) IndexedProviderModels() map[string][]string {
	r.mu.RLock()
	defer r.mu.RUnlock()

	out := make(map[string][]string, len(r.providerModels))
	for providerName, indexed := range r.providerModels {
		models := make([]string, 0, len(indexed))
		for model := range indexed {
			models = append(models, model)
		}

		sort.Strings(models)
		out[providerName] = models
	}

	return out
}

// ProviderModelsVerified reports whether the provider-specific model index
// came from a successful, provider-authoritative FetchModels call instead of
// only the static fallback list. A verified index can be used as evidence that
// absent models are currently unavailable for that provider/account.
func (r *Registry) ProviderModelsVerified(providerName string) bool {
	r.mu.RLock()
	defer r.mu.RUnlock()

	return r.providerModelsLive[strings.TrimSpace(providerName)]
}

// CanResolveModel reports whether the registry can route model to a provider
// without making a network request. It accepts provider-qualified model IDs,
// unambiguous indexed model names, and configured aliases.
func (r *Registry) CanResolveModel(model string) (string, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()

	diagnostic := r.explainModelResolutionLocked(model)
	if diagnostic.Error != nil {
		return "", false
	}

	return diagnostic.ProviderName, true
}

// ListProviders returns the names of all registered providers.
func (r *Registry) ListProviders() []string {
	r.mu.RLock()
	defer r.mu.RUnlock()

	out := make([]string, 0, len(r.providers))
	for name := range r.providers {
		out = append(out, name)
	}

	return out
}

// ProviderModels returns the model list for a specific provider, trying the
// live API first (FetchModels) and falling back to the static list. When the
// live fetch fails, it returns the stale static fallback together with an
// error so callers do not mistake the fallback for fresh provider data. Call
// ProviderModelCatalog when callers need structured stale-fallback or
// provenance details.
func (r *Registry) ProviderModels(ctx context.Context, providerName string) ([]string, error) {
	if err := requireCredentialContext(ctx); err != nil {
		return nil, err
	}

	catalog, err := r.ProviderModelCatalog(ctx, providerName)
	if err != nil {
		return nil, err
	}

	if catalog.Error != nil {
		return catalog.Models, fmt.Errorf(
			"llm: %s live model fetch failed; using stale static fallback: %w",
			providerName,
			catalog.Error,
		)
	}

	return catalog.Models, nil
}

func (r *Registry) indexProviderModelsLocked(providerName string, models []string, provenance ModelProvenance) {
	for _, m := range models {
		r.indexProviderModelLocked(providerName, m, provenance)
	}
}

func (r *Registry) replaceProviderModelsLocked(
	providerName string,
	models []string,
	provenance ModelProvenance,
	verified bool,
) {
	r.removeProviderCatalogModelsLocked(providerName)
	r.indexProviderModelsLocked(providerName, models, provenance)
	r.providerModelsLive[providerName] = verified
}

func (r *Registry) markProviderModelsUnverifiedLocked(providerName string) {
	if _, ok := r.providers[providerName]; ok {
		r.providerModelsLive[providerName] = false
	}
}

func (r *Registry) removeProviderCatalogModelsLocked(providerName string) {
	for model := range r.catalogProvenance[providerName] {
		r.removeCatalogModelClaimLocked(providerName, model)
	}
}

func isCatalogProvenance(provenance ModelProvenance) bool {
	return provenance == ModelProvenanceStatic || provenance == ModelProvenanceFetchedLive
}

func (r *Registry) removeModelClaimLocked(providerName, model string) {
	if byProvider := r.models[model]; byProvider != nil {
		delete(byProvider, providerName)

		if len(byProvider) == 0 {
			delete(r.models, model)
		}
	}

	if indexed := r.providerModels[providerName]; indexed != nil {
		delete(indexed, model)

		if len(indexed) == 0 {
			delete(r.providerModels, providerName)
		}
	}

	if provenanceByModel := r.modelProvenance[providerName]; provenanceByModel != nil {
		delete(provenanceByModel, model)

		if len(provenanceByModel) == 0 {
			delete(r.modelProvenance, providerName)
		}
	}

	if catalogProvenanceByModel := r.catalogProvenance[providerName]; catalogProvenanceByModel != nil {
		delete(catalogProvenanceByModel, model)

		if len(catalogProvenanceByModel) == 0 {
			delete(r.catalogProvenance, providerName)
		}
	}
}

func (r *Registry) removeCatalogModelClaimLocked(providerName, model string) {
	if catalogProvenanceByModel := r.catalogProvenance[providerName]; catalogProvenanceByModel != nil {
		delete(catalogProvenanceByModel, model)

		if len(catalogProvenanceByModel) == 0 {
			delete(r.catalogProvenance, providerName)
		}
	}

	if byProvider := r.models[model]; byProvider != nil {
		if claim, ok := byProvider[providerName]; ok && isCatalogProvenance(claim.provenance) {
			r.removeModelClaimLocked(providerName, model)

			return
		}
	}

	if provenanceByModel := r.modelProvenance[providerName]; provenanceByModel != nil {
		if isCatalogProvenance(provenanceByModel[model]) {
			delete(provenanceByModel, model)

			if len(provenanceByModel) == 0 {
				delete(r.modelProvenance, providerName)
			}
		}
	}

	if indexed := r.providerModels[providerName]; indexed != nil && !r.providerModelHasIntentionalClaimLocked(providerName, model) {
		delete(indexed, model)

		if len(indexed) == 0 {
			delete(r.providerModels, providerName)
		}
	}
}

func (r *Registry) providerModelHasIntentionalClaimLocked(providerName, model string) bool {
	if byProvider := r.models[model]; byProvider != nil {
		if claim, ok := byProvider[providerName]; ok && !isCatalogProvenance(claim.provenance) {
			return true
		}
	}

	return r.modelOverrides[providerName][model]
}

func (r *Registry) indexProviderModelLocked(providerName, model string, provenance ModelProvenance) {
	r.indexModelClaimLocked(providerName, model, model, provenance)
}

func (r *Registry) indexModelClaimLocked(providerName, model, providerModel string, provenance ModelProvenance) {
	model = strings.TrimSpace(model)

	providerModel = strings.TrimSpace(providerModel)
	if model == "" || providerModel == "" {
		return
	}

	if provenance == "" {
		provenance = ModelProvenanceStatic
	}

	if isCatalogProvenance(provenance) {
		r.recordCatalogModelProvenanceLocked(providerName, model, provenance)
	}

	if r.models == nil {
		r.models = make(map[string]map[string]modelClaim)
	}

	claims := r.models[model]
	if claims == nil {
		claims = make(map[string]modelClaim)
		r.models[model] = claims
	}

	if existing, ok := claims[providerName]; ok &&
		!isCatalogProvenance(existing.provenance) &&
		isCatalogProvenance(provenance) {
		// User-configured aliases and overrides are intentional routing claims.
		// A later static/live catalog refresh must not silently replace them
		// when the provider also exposes a model with the same bare name.
		r.addModelToProviderCatalogLocked(providerName, model, existing.provenance)

		return
	}

	if existing, ok := claims[providerName]; ok &&
		existing.provenance == ModelProvenanceConfiguredAlias &&
		provenance == ModelProvenanceUserOverride {
		// A provider-qualified user override such as openai/fast should make
		// that provider-local model available without stealing bare "fast"
		// away from an explicit configured alias.
		r.addModelToProviderCatalogLocked(providerName, model, existing.provenance)
		r.addProviderModelLocked(providerName, model)

		return
	}

	claims[providerName] = modelClaim{
		providerName: providerName,
		model:        providerModel,
		provenance:   provenance,
	}

	r.addProviderModelLocked(providerName, model)

	if r.modelProvenance == nil {
		r.modelProvenance = make(map[string]map[string]ModelProvenance)
	}

	provenanceByModel := r.modelProvenance[providerName]
	if provenanceByModel == nil {
		provenanceByModel = make(map[string]ModelProvenance)
		r.modelProvenance[providerName] = provenanceByModel
	}

	provenanceByModel[model] = provenance

	r.addModelToProviderCatalogLocked(providerName, model, provenance)
}

func (r *Registry) recordCatalogModelProvenanceLocked(providerName, model string, provenance ModelProvenance) {
	if r.catalogProvenance == nil {
		r.catalogProvenance = make(map[string]map[string]ModelProvenance)
	}

	provenanceByModel := r.catalogProvenance[providerName]
	if provenanceByModel == nil {
		provenanceByModel = make(map[string]ModelProvenance)
		r.catalogProvenance[providerName] = provenanceByModel
	}

	provenanceByModel[model] = provenance
}

func (r *Registry) addProviderModelLocked(providerName, model string) {
	if r.providerModels == nil {
		r.providerModels = make(map[string]map[string]bool)
	}

	indexed := r.providerModels[providerName]
	if indexed == nil {
		indexed = make(map[string]bool)
		r.providerModels[providerName] = indexed
	}

	indexed[model] = true
}

func (r *Registry) recordProviderModelOverrideLocked(providerName, model string) {
	model = strings.TrimSpace(model)
	if providerName == "" || model == "" {
		return
	}

	if r.modelOverrides == nil {
		r.modelOverrides = make(map[string]map[string]bool)
	}

	overrides := r.modelOverrides[providerName]
	if overrides == nil {
		overrides = make(map[string]bool)
		r.modelOverrides[providerName] = overrides
	}

	overrides[model] = true
}

func (r *Registry) providerModelProvenanceLocked(providerName, model string) ModelProvenance {
	if r.modelProvenance == nil {
		return ""
	}

	return r.modelProvenance[providerName][strings.TrimSpace(model)]
}

func (r *Registry) addModelToProviderCatalogLocked(providerName, model string, provenance ModelProvenance) {
	if r.catalogs == nil {
		return
	}

	catalog, ok := r.catalogs[providerName]
	if !ok {
		return
	}

	if !containsModel(catalog.Models, model) {
		catalog.Models = append(catalog.Models, model)
		sort.Strings(catalog.Models)
	}

	if catalog.ModelProvenance == nil {
		catalog.ModelProvenance = make(map[string]ModelProvenance)
	}

	catalog.ModelProvenance[model] = provenance
	r.catalogs[providerName] = catalog
}

func (r *Registry) addProviderModelOverridesToCatalogLocked(providerName string) {
	for model, provenance := range r.modelProvenance[providerName] {
		if isCatalogProvenance(provenance) {
			continue
		}

		r.addModelToProviderCatalogLocked(providerName, model, provenance)
	}
}

func containsModel(models []string, want string) bool {
	return slices.Contains(models, want)
}

// ProviderHealth describes the outcome of a single provider health check.
//
//nolint:govet // Field order keeps diagnostics, errors, timing, model provenance, and booleans grouped.
type ProviderHealth struct {
	Contract        *AdapterContract
	Error           error
	ModelFetchError error
	CheckedAt       time.Time
	CacheTTL        time.Duration
	Name            string
	Models          []string
	StaticModels    []string
	LiveModels      []string
	Checks          []ReadinessCheck
	Warnings        []string
	RetryPolicy     RetryPolicyInfo
	ModelSource     ModelCatalogSource
	Healthy         bool
	Cached          bool
	ModelsStale     bool
}

// CheckHealth returns one ProviderHealth entry per provider, sorted by name.
// Providers with structured diagnostics report their adapter contract and
// readiness dimensions; other providers are pinged via HealthCheck and then
// queried for models. Some providers perform network requests here; others
// only check local credentials or daemon reachability.
func (r *Registry) CheckHealth(ctx context.Context) []ProviderHealth {
	return r.CheckHealthWithTTL(ctx, 0)
}

func appendProviderWarnings(current []string, p Provider) []string {
	warningProvider, ok := p.(WarningProvider)
	if !ok {
		return current
	}

	return append(current, warningProvider.ProviderWarnings()...)
}

type providerModelVerifier interface {
	ProviderModelsVerified() bool
}

func providerModelsVerified(provider Provider) bool {
	if verifier, ok := provider.(providerModelVerifier); ok {
		return verifier.ProviderModelsVerified()
	}

	return true
}

// ContextWindow returns the context window size (in tokens) for a model,
// resolving the provider via the same logic as Complete. Returns 0 when the
// model or provider is unknown.
func (r *Registry) ContextWindow(model string) int {
	r.mu.RLock()
	defer r.mu.RUnlock()

	p, providerModel, ok := r.resolveModelLocked(model)
	if !ok {
		return 0
	}

	if limit := catalogContextWindow(p.Name(), providerModel); limit > 0 {
		return limit
	}

	return p.ModelContextWindow(providerModel)
}

func catalogContextWindow(providerName, model string) int {
	metadata, ok := modelroute.BuiltinCatalog().Lookup(providerName, model)
	if !ok {
		return 0
	}

	return metadata.ContextWindow
}

func (r *Registry) resolveModelLocked(model string) (Provider, string, bool) {
	diagnostic := r.explainModelResolutionLocked(model)
	if diagnostic.Error != nil {
		return nil, "", false
	}

	p, ok := r.providers[diagnostic.ProviderName]
	if !ok {
		return nil, "", false
	}

	return p, diagnostic.ProviderModel, true
}

const (
	legacyEstimateCharsPerToken         = 3
	legacyEstimateErrorBoundPercent     = 25
	legacyEstimateMessageOverheadTokens = 6
)

// EstimateTokens returns a conservative provider-agnostic upper-bound estimate
// for a slice of messages. It is retained for legacy callers that only accept a
// single integer; provider/model-aware code should use contextpack.NewEstimator
// so the point estimate, error bound, and upper bound stay auditable.
func EstimateTokens(messages []Message) int {
	var total int

	for i := range messages {
		msg := messages[i]
		base := legacyEstimateMessageOverheadTokens +
			estimateLegacyTextTokens(string(msg.Role)) +
			estimateLegacyTextTokens(msg.Content)

		if len(msg.ToolCalls) > 0 {
			base += estimateLegacyTextTokens(fmt.Sprint(msg.ToolCalls))
		}

		if msg.ToolResult != nil {
			base += estimateLegacyTextTokens(fmt.Sprint(msg.ToolResult))
		}

		errorBound := ceilLegacyEstimateDiv(base*legacyEstimateErrorBoundPercent, 100)
		total += base + errorBound
	}

	return total
}

func estimateLegacyTextTokens(text string) int {
	text = strings.TrimSpace(text)
	if text == "" {
		return 0
	}

	return ceilLegacyEstimateDiv(utf8.RuneCountInString(text), legacyEstimateCharsPerToken)
}

func ceilLegacyEstimateDiv(value, divisor int) int {
	if value <= 0 {
		return 0
	}

	if divisor <= 0 {
		return value
	}

	return (value + divisor - 1) / divisor
}
