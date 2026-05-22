// Package llm defines the provider-agnostic interface for calling large language models.
package llm

import (
	"context"
	"errors"
	"fmt"
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
	Model          string
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
	// It returns a static fallback list; use FetchModels for a live API query.
	Models() []string

	// FetchModels queries the provider's API for the list of available models.
	// If the API call fails it falls back to Models().
	FetchModels(ctx context.Context) ([]string, error)

	// HealthCheck pings the provider to verify that credentials are valid and
	// the service is reachable. It returns nil when the provider is healthy.
	HealthCheck(ctx context.Context) error

	// Complete performs a chat completion.
	Complete(ctx context.Context, params CompleteParams) (*Response, error)

	// ModelContextWindow returns the context window size (in tokens) for the
	// given model. If the model is unknown the provider returns 0.
	ModelContextWindow(model string) int
}

// ---------------------------------------------------------------------------
// Registry
// ---------------------------------------------------------------------------

// Registry holds the set of available providers and resolves model -> provider.
//
//nolint:govet // Field order keeps registry state grouped by purpose.
type Registry struct {
	providers          map[string]Provider
	models             map[string]Provider
	providerModels     map[string]map[string]bool
	providerModelsLive map[string]bool
	fallback           string
	defaultModel       string
	routeTelemetry     *modelroute.Telemetry
	mu                 sync.RWMutex
	retry              retryConfig
}

// NewRegistry creates an empty registry with default retry settings.
func NewRegistry() *Registry {
	return &Registry{
		providers:          make(map[string]Provider),
		models:             make(map[string]Provider),
		providerModels:     make(map[string]map[string]bool),
		providerModelsLive: make(map[string]bool),
		retry:              defaultRetryConfig(),
		routeTelemetry:     modelroute.NewTelemetry(),
	}
}

// SetRetry overrides the default retry policy. Pass a zero MaxAttempts to
// disable retries entirely, which is useful for fast test runs.
func (r *Registry) SetRetry(cfg retryConfig) {
	r.mu.Lock()
	defer r.mu.Unlock()

	r.retry = cfg
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
	r.indexProviderModelsLocked(providerName, p.Models())

	// First registered provider becomes the default.
	if r.fallback == "" {
		r.fallback = providerName
	}
}

// SetDefault changes the fallback provider used when the model is empty.
func (r *Registry) SetDefault(name string) error {
	r.mu.Lock()
	defer r.mu.Unlock()

	if _, ok := r.providers[name]; !ok {
		return fmt.Errorf("llm: unknown provider %q", name)
	}

	r.fallback = name
	r.defaultModel = ""

	return nil
}

// SetDefaultModel changes the fallback model used when CompleteParams.Model is
// empty. The model must already be indexed by the registry.
func (r *Registry) SetDefaultModel(model string) error {
	r.mu.Lock()
	defer r.mu.Unlock()

	if providerName, providerModel, ok := splitProviderModel(model); ok {
		if _, ok := r.providers[providerName]; !ok {
			return fmt.Errorf("llm: unknown provider %q", providerName)
		}

		r.indexProviderModelLocked(providerName, providerModel)
		r.fallback = providerName
		r.defaultModel = providerModel

		return nil
	}

	p, ok := r.models[model]
	if !ok {
		return fmt.Errorf("llm: unknown model %q", model)
	}

	r.fallback = p.Name()
	r.defaultModel = model

	return nil
}

// SetDefaultProviderModel changes the fallback provider/model pair. It can be
// used for configured live-only model IDs before ProviderModels has fetched and
// indexed the provider's live model list.
func (r *Registry) SetDefaultProviderModel(providerName, model string) error {
	if model == "" {
		return errors.New("llm: default model cannot be empty")
	}

	r.mu.Lock()
	defer r.mu.Unlock()

	if _, ok := r.providers[providerName]; !ok {
		return fmt.Errorf("llm: unknown provider %q", providerName)
	}

	r.indexProviderModelLocked(providerName, model)
	r.fallback = providerName
	r.defaultModel = model

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

	p, params, err := r.resolve(params)
	if err != nil {
		return nil, err
	}

	// Snapshot retry config under the lock so concurrent SetRetry calls are safe.
	r.mu.RLock()
	retryCfg := r.retry
	r.mu.RUnlock()

	emitToolExecute(ctx, p, params.Model)

	startedAt := time.Now()

	resp, err := completeWithRetry(ctx, retryCfg, func(ctx context.Context) (*Response, error) {
		providerResp, completeErr := p.Complete(ctx, params)
		if completeErr != nil {
			wrappedErr := fmt.Errorf("llm: %s: %w", p.Name(), completeErr)
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

	models := modelFallbackChain(params.Model, fallbackModels)
	if len(models) == 0 {
		return r.Complete(ctx, params)
	}

	var failures []error

	for _, model := range models {
		if err := ctx.Err(); err != nil {
			return nil, fmt.Errorf("llm: fallback canceled: %w", err)
		}

		next := params
		next.Model = model

		resp, err := r.Complete(ctx, next)
		if err == nil {
			return resp, nil
		}

		failures = append(failures, fmt.Errorf("%s: %w", model, err))
	}

	return nil, fmt.Errorf("llm: all fallback models failed: %w", errors.Join(failures...))
}

func (r *Registry) resolve(params CompleteParams) (Provider, CompleteParams, error) {
	r.mu.RLock()
	defer r.mu.RUnlock()

	if params.Model != "" {
		return r.resolveExplicitModelLocked(params)
	}

	if r.defaultModel != "" {
		p, ok := r.models[r.defaultModel]
		if !ok {
			return nil, params, fmt.Errorf("llm: unknown default model %q", r.defaultModel)
		}

		params.Model = r.defaultModel

		return p, params, nil
	}

	p, ok := r.providers[r.fallback]
	if !ok {
		return nil, params, errors.New("llm: no providers registered")
	}

	if models := p.Models(); len(models) > 0 {
		params.Model = models[0]
	}

	return p, params, nil
}

func emitToolExecute(ctx context.Context, provider Provider, model string) {
	emitActivity(ctx, events.Event{
		Type:  events.ToolExecute,
		Model: model,
		Metadata: map[string]string{
			"provider": provider.Name(),
			"tool":     "llm.complete",
		},
	})
}

func (r *Registry) recordRouteFailure(providerName, requestedModel string, err error) {
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
	retryAfter, retryable := isRetryable(err)

	telemetry.RecordFailure(candidate, modelroute.Failure{
		RetryAfter:  retryAfter,
		Error:       err.Error(),
		Retryable:   retryable,
		RateLimited: isRateLimitError(err),
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
	if providerName, providerModel, ok := splitProviderModel(params.Model); ok {
		p, ok := r.providers[providerName]
		if !ok {
			return nil, params, fmt.Errorf("llm: unknown provider %q", providerName)
		}

		params.Model = providerModel

		return p, params, nil
	}

	if p, ok := r.models[params.Model]; ok {
		return p, params, nil
	}

	if p, ok := r.providerForModelPrefixLocked(params.Model); ok {
		return p, params, nil
	}

	return nil, params, fmt.Errorf("llm: unknown model %q", params.Model)
}

func (r *Registry) providerForModelPrefixLocked(model string) (Provider, bool) {
	providerName := r.providerNameForModelPrefixLocked(model)
	if providerName == "" {
		return nil, false
	}

	p, ok := r.providers[providerName]

	return p, ok
}

func (r *Registry) providerNameForModelPrefixLocked(model string) string {
	model = strings.ToLower(strings.TrimSpace(model))
	switch {
	case strings.HasPrefix(model, "claude"):
		if _, ok := r.providers[providerClaudeCode]; ok {
			return providerClaudeCode
		}

		return providerAnthropic
	case strings.HasPrefix(model, "gpt"), strings.HasPrefix(model, "o1"), strings.HasPrefix(model, "o3"), strings.HasPrefix(model, "o4"):
		return providerOpenAI
	default:
		return ""
	}
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

// ProviderForModel returns the provider name currently indexed for a model.
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

	p, providerModel, ok := r.resolveModelLocked(model)
	if !ok {
		return "", "", false
	}

	return p.Name(), providerModel, true
}

// ProviderHasModel reports whether providerName has model in the registry's
// static or fetched model index. It does not make network requests.
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

// IndexedProviderModels returns a copy of the registry's provider-specific
// static/fetched model index. It does not make network requests.
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
// indexed provider model names, and known provider model prefixes.
func (r *Registry) CanResolveModel(model string) (string, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()

	if providerName, _, ok := splitProviderModel(model); ok {
		if _, ok := r.providers[providerName]; ok {
			return providerName, true
		}

		return "", false
	}

	if p, ok := r.models[model]; ok {
		return p.Name(), true
	}

	if p, ok := r.providerForModelPrefixLocked(model); ok {
		return p.Name(), true
	}

	return "", false
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
// live API first (FetchModels) and falling back to the static list.
func (r *Registry) ProviderModels(ctx context.Context, providerName string) ([]string, error) {
	if err := requireCredentialContext(ctx); err != nil {
		return nil, err
	}

	r.mu.RLock()
	p, ok := r.providers[providerName]
	r.mu.RUnlock()

	if !ok {
		return nil, fmt.Errorf("llm: unknown provider %q", providerName)
	}

	models, err := p.FetchModels(ctx)
	if err == nil && len(models) > 0 {
		r.storeFetchedProviderModels(providerName, models, providerModelsVerified(p))

		return models, nil
	}

	r.markProviderModelsUnverified(providerName, p.Models())

	return p.Models(), nil
}

func (r *Registry) indexProviderModelsLocked(providerName string, models []string) {
	for _, m := range models {
		r.indexProviderModelLocked(providerName, m)
	}
}

func (r *Registry) storeFetchedProviderModels(providerName string, models []string, verified bool) {
	r.mu.Lock()
	defer r.mu.Unlock()

	r.storeFetchedProviderModelsLocked(providerName, models, verified)
}

func (r *Registry) storeFetchedProviderModelsLocked(providerName string, models []string, verified bool) {
	if verified {
		r.replaceProviderModelsLocked(providerName, models)

		return
	}

	r.indexProviderModelsLocked(providerName, models)
	r.providerModelsLive[providerName] = false
}

func (r *Registry) replaceProviderModelsLocked(providerName string, models []string) {
	for model := range r.providerModels[providerName] {
		r.removeProviderModelLocked(providerName, model)
	}

	delete(r.providerModels, providerName)
	r.indexProviderModelsLocked(providerName, models)
	r.providerModelsLive[providerName] = true
}

func (r *Registry) markProviderModelsUnverified(providerName string, fallbackModels []string) {
	r.mu.Lock()
	defer r.mu.Unlock()

	r.indexProviderModelsLocked(providerName, fallbackModels)
	r.markProviderModelsUnverifiedLocked(providerName)
}

func (r *Registry) markProviderModelsUnverifiedLocked(providerName string) {
	if _, ok := r.providers[providerName]; ok {
		r.providerModelsLive[providerName] = false
	}
}

func (r *Registry) removeProviderModelLocked(providerName, model string) {
	current, ok := r.models[model]
	if !ok || current.Name() != providerName {
		return
	}

	delete(r.models, model)

	for otherProvider, indexedModels := range r.providerModels {
		if otherProvider == providerName || !indexedModels[model] {
			continue
		}

		if replacement, ok := r.providers[otherProvider]; ok {
			r.models[model] = replacement

			return
		}
	}
}

func (r *Registry) indexProviderModelLocked(providerName, model string) {
	model = strings.TrimSpace(model)
	if model == "" {
		return
	}

	r.models[model] = r.providers[providerName]

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

// ProviderHealth describes the outcome of a single provider health check.
type ProviderHealth struct {
	Contract *AdapterContract
	Error    error
	Name     string
	Models   []string
	Checks   []ReadinessCheck
	Warnings []string
	Healthy  bool
}

// CheckHealth returns one ProviderHealth entry per provider, sorted by name.
// Providers with structured diagnostics report their adapter contract and
// readiness dimensions; other providers are pinged via HealthCheck and then
// queried for models.
func (r *Registry) CheckHealth(ctx context.Context) []ProviderHealth {
	ctxErr := requireCredentialContext(ctx)

	r.mu.RLock()

	names := make([]string, 0, len(r.providers))
	for name := range r.providers {
		names = append(names, name)
	}

	r.mu.RUnlock()

	sort.Strings(names)

	results := make([]ProviderHealth, 0, len(names))
	for _, name := range names {
		r.mu.RLock()
		p := r.providers[name]
		r.mu.RUnlock()

		ph := ProviderHealth{Name: name}

		if ctxErr != nil {
			ph.Error = ctxErr
			ph.Models = p.Models()
			ph.Warnings = appendProviderWarnings(ph.Warnings, p)

			r.markProviderModelsUnverified(name, ph.Models)
			results = append(results, ph)

			continue
		}

		if diagnosticProvider, ok := p.(DiagnosticsProvider); ok {
			ph = providerHealthFromDiagnostics(name, diagnosticProvider.AdapterDiagnostics())
			if len(ph.Models) == 0 {
				ph.Models = p.Models()
			}

			ph.Warnings = appendProviderWarnings(ph.Warnings, p)

			r.markProviderModelsUnverified(name, ph.Models)
			results = append(results, ph)

			continue
		}

		ph.Warnings = appendProviderWarnings(ph.Warnings, p)

		if err := p.HealthCheck(ctx); err != nil {
			ph.Error = err
			ph.Models = p.Models()

			r.markProviderModelsUnverified(name, ph.Models)
		} else {
			ph.Healthy = true

			models, fetchErr := p.FetchModels(ctx)
			if fetchErr != nil || len(models) == 0 {
				models = p.Models()

				r.markProviderModelsUnverified(name, models)
			} else {
				r.storeFetchedProviderModels(name, models, providerModelsVerified(p))
			}

			ph.Models = models
		}

		results = append(results, ph)
	}

	return results
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
	model = strings.TrimSpace(model)
	if model == "" {
		return r.resolveDefaultModelLocked()
	}

	if providerName, providerModel, ok := splitProviderModel(model); ok {
		p, ok := r.providers[providerName]
		if !ok {
			return nil, "", false
		}

		return p, providerModel, true
	}

	if p, ok := r.models[model]; ok {
		return p, model, true
	}

	if p, ok := r.providerForModelPrefixLocked(model); ok {
		return p, model, true
	}

	return nil, "", false
}

func (r *Registry) resolveDefaultModelLocked() (Provider, string, bool) {
	if r.defaultModel != "" {
		p, ok := r.models[r.defaultModel]
		if !ok {
			return nil, "", false
		}

		return p, r.defaultModel, true
	}

	p, ok := r.providers[r.fallback]
	if !ok {
		return nil, "", false
	}

	models := p.Models()
	if len(models) == 0 {
		return p, "", true
	}

	return p, models[0], true
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
