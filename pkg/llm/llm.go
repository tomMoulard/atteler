// Package llm defines the provider-agnostic interface for calling large language models.
package llm

import (
	"context"
	"errors"
	"fmt"
	"sync"
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
	Model       string
	Messages    []Message
	Stop        []string
	Temperature float64
	TopP        float64
	MaxTokens   int
}

// Response is the provider-normalised result of a completion.
type Response struct {
	Content      string
	Model        string // Model that actually answered.
	InputTokens  int
	OutputTokens int
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

	// Complete performs a chat completion.
	Complete(ctx context.Context, params CompleteParams) (*Response, error)
}

// ---------------------------------------------------------------------------
// Registry
// ---------------------------------------------------------------------------

// Registry holds the set of available providers and resolves model -> provider.
type Registry struct {
	providers map[string]Provider
	models    map[string]Provider
	fallback  string
	mu        sync.RWMutex
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

	r.providers[p.Name()] = p
	for _, m := range p.Models() {
		r.models[m] = p
	}
	// First registered provider becomes the default.
	if r.fallback == "" {
		r.fallback = p.Name()
	}
}

// SetDefault changes the fallback provider used when the model is empty.
func (r *Registry) SetDefault(name string) error {
	r.mu.RLock()
	defer r.mu.RUnlock()
	if _, ok := r.providers[name]; !ok {
		return fmt.Errorf("llm: unknown provider %q", name)
	}
	r.fallback = name
	return nil
}

// Complete resolves the provider for params.Model and calls it.
// If Model is empty the default provider is used with its first listed model.
func (r *Registry) Complete(ctx context.Context, params CompleteParams) (*Response, error) {
	r.mu.RLock()
	defer r.mu.RUnlock()

	if params.Model != "" {
		if p, ok := r.models[params.Model]; ok {
			resp, err := p.Complete(ctx, params)
			if err != nil {
				return nil, fmt.Errorf("llm: %s: %w", p.Name(), err)
			}
			return resp, nil
		}
		return nil, fmt.Errorf("llm: unknown model %q", params.Model)
	}

	p, ok := r.providers[r.fallback]
	if !ok {
		return nil, errors.New("llm: no providers registered")
	}
	if models := p.Models(); len(models) > 0 {
		params.Model = models[0]
	}

	resp, err := p.Complete(ctx, params)
	if err != nil {
		return nil, fmt.Errorf("llm: %s: %w", p.Name(), err)
	}
	return resp, nil
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
		return models, nil
	}
	return p.Models(), nil
}
