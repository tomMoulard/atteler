// Package llm defines the provider-agnostic interface for calling large language models.
package llm

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"

	"github.com/tommoulard/atteler/pkg/events"
)

const (
	providerAnthropic  = "anthropic"
	providerClaudeCode = "claude-code"
	providerCodex      = "codex"
	providerOpenAI     = "openai"
)

// Role represents who authored a message.
type Role string

// Supported message roles.
const (
	RoleSystem    Role = "system"
	RoleUser      Role = "user"
	RoleAssistant Role = "assistant"
)

// Message is a single turn in a conversation.
type Message struct {
	Role    Role   `json:"role"`
	Content string `json:"content"`
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
	MaxTokens      int
}

// Response is the provider-normalised result of a completion.
type Response struct {
	Content           string
	Model             string // Model that actually answered.
	InputTokens       int
	CachedInputTokens int
	OutputTokens      int
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
type Registry struct {
	providers    map[string]Provider
	models       map[string]Provider
	fallback     string
	defaultModel string
	mu           sync.RWMutex
}

// NewRegistry creates an empty registry.
func NewRegistry() *Registry {
	return &Registry{
		providers: make(map[string]Provider),
		models:    make(map[string]Provider),
	}
}

// Register adds a provider and indexes all of its models.
func (r *Registry) Register(p Provider) {
	r.mu.Lock()
	defer r.mu.Unlock()

	providerName := p.Name()
	r.providers[providerName] = p
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
		p, ok := r.providers[providerName]
		if !ok {
			return fmt.Errorf("llm: unknown provider %q", providerName)
		}
		r.models[providerModel] = p
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

	p, ok := r.providers[providerName]
	if !ok {
		return fmt.Errorf("llm: unknown provider %q", providerName)
	}

	r.models[model] = p
	r.fallback = providerName
	r.defaultModel = model
	return nil
}

// Complete resolves the provider for params.Model and calls it.
// If Model is empty the default provider is used with its first listed model.
func (r *Registry) Complete(ctx context.Context, params CompleteParams) (*Response, error) {
	p, params, err := r.resolve(params)
	if err != nil {
		return nil, err
	}

	emitToolExecute(ctx, p, params.Model)
	resp, err := p.Complete(ctx, params)
	if err != nil {
		return nil, fmt.Errorf("llm: %s: %w", p.Name(), err)
	}
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
	r.mu.RLock()
	defer r.mu.RUnlock()

	if providerName, _, ok := splitProviderModel(model); ok {
		if _, ok := r.providers[providerName]; ok {
			return providerName, true
		}
		return "", false
	}

	p, ok := r.models[model]
	if !ok {
		return "", false
	}
	return p.Name(), true
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
	r.mu.RLock()
	p, ok := r.providers[providerName]
	r.mu.RUnlock()
	if !ok {
		return nil, fmt.Errorf("llm: unknown provider %q", providerName)
	}

	models, err := p.FetchModels(ctx)
	if err == nil && len(models) > 0 {
		r.mu.Lock()
		r.indexProviderModelsLocked(providerName, models)
		r.mu.Unlock()
		return models, nil
	}
	return p.Models(), nil
}

func (r *Registry) indexProviderModelsLocked(providerName string, models []string) {
	p := r.providers[providerName]
	for _, m := range models {
		if m != "" {
			r.models[m] = p
		}
	}
}

// ProviderHealth describes the outcome of a single provider health check.
type ProviderHealth struct {
	Error   error
	Name    string
	Models  []string
	Healthy bool
}

// CheckHealth pings every registered provider and fetches its models. It
// returns one ProviderHealth entry per provider, sorted by name.
func (r *Registry) CheckHealth(ctx context.Context) []ProviderHealth {
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

		if err := p.HealthCheck(ctx); err != nil {
			ph.Error = err
			ph.Models = p.Models()
		} else {
			ph.Healthy = true
			models, fetchErr := p.FetchModels(ctx)
			if fetchErr != nil || len(models) == 0 {
				models = p.Models()
			}
			ph.Models = models
		}

		results = append(results, ph)
	}

	return results
}

// ContextWindow returns the context window size (in tokens) for a model,
// resolving the provider via the same logic as Complete. Returns 0 when the
// model or provider is unknown.
func (r *Registry) ContextWindow(model string) int {
	r.mu.RLock()
	defer r.mu.RUnlock()

	p := r.providerForModelLocked(model)
	if p == nil {
		return 0
	}

	// Strip provider prefix if present (e.g. "openai/gpt-4.1" -> "gpt-4.1").
	if _, providerModel, ok := splitProviderModel(model); ok {
		return p.ModelContextWindow(providerModel)
	}
	return p.ModelContextWindow(model)
}

func (r *Registry) providerForModelLocked(model string) Provider {
	if providerName, _, ok := splitProviderModel(model); ok {
		if p, ok := r.providers[providerName]; ok {
			return p
		}
		return nil
	}
	if p, ok := r.models[model]; ok {
		return p
	}
	if p, ok := r.providerForModelPrefixLocked(model); ok {
		return p
	}
	return nil
}

// EstimateTokens returns a rough token count for a slice of messages using
// the ~4 characters per token heuristic. This is intentionally a fast
// approximation; provider-specific tokenizers can refine it later.
func EstimateTokens(messages []Message) int {
	var chars int
	for i := range messages {
		chars += len(messages[i].Content)
	}
	// ~4 characters per token is the widely used GPT/Claude approximation.
	const charsPerToken = 4
	return (chars + charsPerToken - 1) / charsPerToken
}
