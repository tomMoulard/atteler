package llm

import (
	"context"
	"log/slog"
	"net/http"
	"strings"
	"time"
)

type providerFactory func() (Provider, error)

// defaultHTTPTimeout is the timeout applied to provider HTTP clients when no
// explicit timeout is configured. LLM completion calls can be long-running, so
// the default is generous.
const defaultHTTPTimeout = 120 * time.Second

// providerHTTPClient returns an *http.Client with a timeout derived from the
// provider config. A zero or negative TimeoutSeconds uses defaultHTTPTimeout.
func providerHTTPClient(cfg ProviderConfig) *http.Client {
	timeout := defaultHTTPTimeout
	if cfg.TimeoutSeconds > 0 {
		timeout = time.Duration(cfg.TimeoutSeconds) * time.Second
	}

	return &http.Client{Timeout: timeout}
}

// ProviderInfo describes a built-in provider without requiring credentials.
type ProviderInfo struct {
	Name   string
	Models []string
}

// AutoRegisterConfig configures provider auto-registration and fallback
// selection.
type AutoRegisterConfig struct {
	// Logger receives registration progress messages. When nil, slog.Default() is used.
	Logger          *slog.Logger
	Providers       map[string]ProviderConfig
	DefaultProvider string
	DefaultModel    string
	SelectedModel   string
}

// logger returns the configured logger, falling back to the standard default.
func (c AutoRegisterConfig) logger() *slog.Logger {
	if c.Logger != nil {
		return c.Logger
	}

	return slog.Default()
}

// ProviderConfig configures one LLM provider.
type ProviderConfig struct {
	BaseURL        string
	Disabled       bool
	AutoStart      bool
	TimeoutSeconds int
}

// AutoRegister tries to create every known provider and registers the ones
// whose credentials are available. It returns a ready-to-use Registry.
// Providers that fail to initialize (missing credentials) are silently skipped.
func AutoRegister() *Registry {
	return AutoRegisterContext(defaultCredentialContext())
}

// AutoRegisterContext is AutoRegister with caller-provided cancellation.
func AutoRegisterContext(ctx context.Context) *Registry {
	return AutoRegisterWithConfigContext(ctx, AutoRegisterConfig{})
}

// KnownProviders returns built-in provider model catalogs without network or
// credential access.
func KnownProviders() []ProviderInfo {
	providers := []Provider{
		&AnthropicProvider{},
		&ClaudeCodeProvider{},
		&CodexProvider{},
		&OpenAIProvider{},
		&OllamaProvider{},
	}

	out := make([]ProviderInfo, 0, len(providers))
	for _, provider := range providers {
		out = append(out, ProviderInfo{
			Name:   provider.Name(),
			Models: append([]string(nil), provider.Models()...),
		})
	}

	return out
}

// AutoRegisterWithConfig tries to create every known provider and applies the
// configured fallback provider/model after registration.
func AutoRegisterWithConfig(cfg AutoRegisterConfig) *Registry {
	return AutoRegisterWithConfigContext(defaultCredentialContext(), cfg)
}

// AutoRegisterWithConfigContext is AutoRegisterWithConfig with caller-provided
// cancellation for credential discovery and provider readiness checks.
func AutoRegisterWithConfigContext(ctx context.Context, cfg AutoRegisterConfig) *Registry {
	ctx = nonNilCredentialContext(ctx)
	r := NewRegistry()

	registerConfiguredProvider(r, cfg, providerAnthropic, func() (Provider, error) {
		return NewAnthropicProviderWithConfigContext(ctx, providerConfig(cfg, providerAnthropic))
	})
	registerConfiguredProvider(r, cfg, providerOpenAI, func() (Provider, error) {
		return NewOpenAIProviderWithConfig(providerConfig(cfg, providerOpenAI))
	})
	registerConfiguredProvider(r, cfg, providerOllama, func() (Provider, error) {
		ollamaConfig := providerConfig(cfg, providerOllama)
		ollamaConfig.AutoStart = ollamaConfig.AutoStart || shouldAutoStartOllama(cfg)

		return NewOllamaProviderWithConfigContext(ctx, ollamaConfig)
	})
	registerConfiguredProvider(r, cfg, providerClaudeCode, func() (Provider, error) {
		return NewClaudeCodeProviderContext(ctx)
	})
	registerConfiguredProvider(r, cfg, providerCodex, func() (Provider, error) {
		return NewCodexProvider()
	})

	applyDefaultSelection(r, cfg)

	return r
}

func registerConfiguredProvider(r *Registry, cfg AutoRegisterConfig, providerName string, factory providerFactory) {
	if providerConfig(cfg, providerName).Disabled {
		cfg.logger().Debug("llm provider skipped: disabled by config", "provider", providerName)
		return
	}

	p, err := factory()
	if err != nil {
		logProviderSkip(cfg.logger(), providerName, err)
		return
	}

	r.Register(p)
}

func applyDefaultSelection(r *Registry, cfg AutoRegisterConfig) {
	logger := cfg.logger()

	if cfg.DefaultProvider != "" {
		if err := r.SetDefault(cfg.DefaultProvider); err != nil {
			logger.Warn("llm default provider ignored", "provider", cfg.DefaultProvider, "error", err)
		}
	}

	if cfg.DefaultModel != "" {
		err := r.SetDefaultModel(cfg.DefaultModel)
		if err != nil && cfg.DefaultProvider != "" {
			err = r.SetDefaultProviderModel(cfg.DefaultProvider, cfg.DefaultModel)
		}

		if err != nil {
			logger.Warn("llm default model ignored", "model", cfg.DefaultModel, "error", err)
		}
	}
}

func logProviderSkip(logger *slog.Logger, providerName string, err error) {
	if isMissingCredentialError(err) {
		return
	}

	logger.Debug("llm provider skipped", "provider", providerName, "error", err)
}

func isMissingCredentialError(err error) bool {
	if err == nil {
		return false
	}

	msg := strings.ToLower(err.Error())

	return strings.Contains(msg, "no ") &&
		(strings.Contains(msg, "credentials") || strings.Contains(msg, "api key found"))
}

func providerConfig(cfg AutoRegisterConfig, name string) ProviderConfig {
	if cfg.Providers == nil {
		return ProviderConfig{}
	}

	return cfg.Providers[name]
}

func shouldAutoStartOllama(cfg AutoRegisterConfig) bool {
	if strings.EqualFold(cfg.DefaultProvider, providerOllama) {
		return true
	}

	return modelNamesProvider(cfg.DefaultModel, providerOllama) ||
		modelNamesProvider(cfg.SelectedModel, providerOllama) ||
		isKnownOllamaModelName(cfg.DefaultModel) ||
		isKnownOllamaModelName(cfg.SelectedModel)
}

func modelNamesProvider(model, provider string) bool {
	model = strings.TrimSpace(model)
	if model == "" {
		return false
	}

	prefix, _, ok := strings.Cut(model, "/")

	return ok && strings.EqualFold(prefix, provider)
}

func isKnownOllamaModelName(model string) bool {
	model = strings.ToLower(strings.TrimSpace(model))

	model, _, _ = strings.Cut(model, ":")
	if model == "" {
		return false
	}

	for _, known := range (&OllamaProvider{}).Models() {
		if strings.EqualFold(model, known) {
			return true
		}
	}

	return false
}
