package llm

import (
	"log"
	"strings"
)

type providerFactory func() (Provider, error)

// ProviderInfo describes a built-in provider without requiring credentials.
type ProviderInfo struct {
	Name   string
	Models []string
}

// AutoRegisterConfig configures provider auto-registration and fallback
// selection.
type AutoRegisterConfig struct {
	Providers       map[string]ProviderConfig
	DefaultProvider string
	DefaultModel    string
}

// ProviderConfig configures one LLM provider.
type ProviderConfig struct {
	BaseURL  string
	Disabled bool
}

// AutoRegister tries to create every known provider and registers the ones
// whose credentials are available. It returns a ready-to-use Registry.
// Providers that fail to initialize (missing credentials) are silently skipped.
func AutoRegister() *Registry {
	return AutoRegisterWithConfig(AutoRegisterConfig{})
}

// KnownProviders returns built-in provider model catalogs without network or
// credential access.
func KnownProviders() []ProviderInfo {
	providers := []Provider{
		&AnthropicProvider{},
		&ClaudeCodeProvider{},
		&CodexProvider{},
		&OpenAIProvider{},
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
	r := NewRegistry()

	registerConfiguredProvider(r, cfg, providerAnthropic, func() (Provider, error) {
		return NewAnthropicProviderWithConfig(providerConfig(cfg, providerAnthropic))
	})
	registerConfiguredProvider(r, cfg, providerOpenAI, func() (Provider, error) {
		return NewOpenAIProviderWithConfig(providerConfig(cfg, providerOpenAI))
	})
	registerConfiguredProvider(r, cfg, providerClaudeCode, func() (Provider, error) {
		return NewClaudeCodeProvider()
	})
	registerConfiguredProvider(r, cfg, providerCodex, func() (Provider, error) {
		return NewCodexProvider()
	})

	applyDefaultSelection(r, cfg)

	return r
}

func registerConfiguredProvider(r *Registry, cfg AutoRegisterConfig, providerName string, factory providerFactory) {
	if providerConfig(cfg, providerName).Disabled {
		log.Printf("llm: %s skipped: disabled by config", providerName)
		return
	}
	p, err := factory()
	if err != nil {
		logProviderSkip(providerName, err)
		return
	}
	r.Register(p)
}

func applyDefaultSelection(r *Registry, cfg AutoRegisterConfig) {
	if cfg.DefaultProvider != "" {
		if err := r.SetDefault(cfg.DefaultProvider); err != nil {
			log.Printf("llm: default provider ignored: %v", err)
		}
	}

	if cfg.DefaultModel != "" {
		err := r.SetDefaultModel(cfg.DefaultModel)
		if err != nil && cfg.DefaultProvider != "" {
			err = r.SetDefaultProviderModel(cfg.DefaultProvider, cfg.DefaultModel)
		}
		if err != nil {
			log.Printf("llm: default model ignored: %v", err)
		}
	}
}

func logProviderSkip(providerName string, err error) {
	if isMissingCredentialError(err) {
		return
	}
	log.Printf("llm: %s skipped: %v", providerName, err)
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
