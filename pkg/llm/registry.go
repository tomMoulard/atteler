package llm

import (
	"context"
	"log/slog"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/tommoulard/atteler/pkg/modelroute"
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
	Capabilities ProviderCapabilities
	Name         string
	Models       []string
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
	BaseURL               string
	Disabled              bool
	AutoStart             bool
	DisablePrivateAdapter bool
	TimeoutSeconds        int
}

// AutoRegister is kept for source compatibility only and does not perform
// credential or network work without a caller-provided context.
//
// Deprecated: use AutoRegisterContext.
func AutoRegister() *Registry {
	return NewRegistry()
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
	catalogModels := catalogModelsByProvider()

	out := make([]ProviderInfo, 0, len(providers))
	for _, provider := range providers {
		out = append(out, ProviderInfo{
			Capabilities: ProviderCapabilitiesFor(provider),
			Name:         provider.Name(),
			Models:       mergeModelLists(provider.Models(), catalogModels[provider.Name()]),
		})
	}

	return out
}

func catalogModelsByProvider() map[string][]string {
	catalog := modelroute.BuiltinCatalog()
	models := make(map[string][]string, len(catalog.Models))

	for i := range catalog.Models {
		metadata := catalog.Models[i]
		models[metadata.Provider] = append(models[metadata.Provider], metadata.Name)
	}

	return models
}

func mergeModelLists(lists ...[]string) []string {
	var out []string

	seen := make(map[string]bool)

	for _, list := range lists {
		for _, model := range list {
			model = strings.TrimSpace(model)
			if model == "" || seen[model] {
				continue
			}

			seen[model] = true
			out = append(out, model)
		}
	}

	return out
}

// AutoRegisterWithConfig is kept for source compatibility only and does not
// perform credential or network work without a caller-provided context.
//
// Deprecated: use AutoRegisterWithConfigContext.
func AutoRegisterWithConfig(cfg AutoRegisterConfig) *Registry {
	r := NewRegistry()
	applyDefaultSelection(r, cfg)

	return r
}

// AutoRegisterWithConfigContext is AutoRegisterWithConfig with caller-provided
// cancellation for credential discovery and provider readiness checks.
func AutoRegisterWithConfigContext(ctx context.Context, cfg AutoRegisterConfig) *Registry {
	r := NewRegistry()

	if err := requireCredentialContext(ctx); err != nil {
		logProviderSkip(cfg.logger(), "all", err)
		applyDefaultSelection(r, cfg)

		return r
	}

	registerConfiguredProvider(r, cfg, providerAnthropic, func() (Provider, error) {
		return NewAnthropicProviderWithConfigContext(ctx, providerConfig(cfg, providerAnthropic))
	})
	registerConfiguredProvider(r, cfg, providerOpenAI, func() (Provider, error) {
		return NewOpenAIProviderWithConfigContext(ctx, providerConfig(cfg, providerOpenAI))
	})
	registerConfiguredProvider(r, cfg, providerOllama, func() (Provider, error) {
		ollamaConfig := providerConfig(cfg, providerOllama)
		ollamaConfig.AutoStart = ollamaConfig.AutoStart || shouldAutoStartOllama(cfg)

		return NewOllamaProviderWithConfigContext(ctx, ollamaConfig)
	})
	registerConfiguredProvider(r, cfg, providerClaudeCode, func() (Provider, error) {
		return NewClaudeCodeProviderWithConfigContext(ctx, providerConfig(cfg, providerClaudeCode))
	})
	registerConfiguredProvider(r, cfg, providerCodex, func() (Provider, error) {
		return NewCodexProviderWithConfigContext(ctx, providerConfig(cfg, providerCodex))
	})

	applyDefaultSelection(r, cfg)

	return r
}

// PrivateAdapterDiagnostics reports readiness for private/borrowed-credential
// adapters without requiring them to be registered. Disabled private adapters
// are omitted so global/provider kill switches do not produce noisy failures.
func PrivateAdapterDiagnostics(ctx context.Context, cfg AutoRegisterConfig) []ProviderHealth {
	ctxErr := requireCredentialContext(ctx)

	names := []string{providerClaudeCode, providerCodex}
	results := make([]ProviderHealth, 0, len(names))

	for _, providerName := range names {
		providerCfg := providerConfig(cfg, providerName)
		if providerCfg.Disabled || privateAdapterDisabled(providerName, providerCfg) {
			continue
		}

		if ctxErr != nil {
			results = append(results, privateAdapterCredentialFailure(
				providerName,
				privateAdapterContract(providerName),
				privateAdapterModels(providerName),
				ctxErr,
			))

			continue
		}

		results = append(results, privateAdapterHealth(ctx, providerName, providerCfg))
	}

	return results
}

func privateAdapterHealth(ctx context.Context, providerName string, cfg ProviderConfig) ProviderHealth {
	switch providerName {
	case providerCodex:
		provider, err := NewCodexProviderWithConfigContext(ctx, cfg)
		if err != nil {
			return privateAdapterCredentialFailure(providerCodex, codexAdapterContract(), codexModels(), err)
		}

		return providerHealthFromDiagnostics(providerCodex, provider.AdapterDiagnostics())
	case providerClaudeCode:
		provider, err := NewClaudeCodeProviderWithConfigContext(ctx, cfg)
		if err != nil {
			return privateAdapterCredentialFailure(providerClaudeCode, claudeCodeAdapterContract(), defaultClaudeCodeModels(), err)
		}

		return providerHealthFromDiagnostics(providerClaudeCode, provider.AdapterDiagnostics())
	default:
		return ProviderHealth{Name: providerName}
	}
}

func privateAdapterContract(providerName string) AdapterContract {
	switch providerName {
	case providerCodex:
		return codexAdapterContract()
	case providerClaudeCode:
		return claudeCodeAdapterContract()
	default:
		return AdapterContract{Provider: providerName}
	}
}

func privateAdapterModels(providerName string) []string {
	switch providerName {
	case providerCodex:
		return codexModels()
	case providerClaudeCode:
		return defaultClaudeCodeModels()
	default:
		return nil
	}
}

func privateAdapterCredentialFailure(
	providerName string,
	contract AdapterContract,
	models []string,
	err error,
) ProviderHealth {
	diagnostics := AdapterDiagnostics{
		Contract: contract,
		Checks: []ReadinessCheck{
			{Name: "local_credentials", Status: ReadinessFailed, Detail: err.Error()},
			{Name: "token_refresh", Status: ReadinessSkipped, Detail: "not checked because local credentials did not load"},
			{Name: "network_reachability", Status: ReadinessSkipped, Detail: "not probed during doctor; adapter did not pass local credential checks"},
			{Name: "model_availability", Status: ReadinessWarning, Detail: "static catalog only; model availability is not network verified"},
		},
		Warnings: []string{"private adapter is unavailable before any request because its borrowed credential contract failed"},
		Models:   append([]string(nil), models...),
	}

	return providerHealthFromDiagnostics(providerName, diagnostics)
}

func providerHealthFromDiagnostics(providerName string, diagnostics AdapterDiagnostics) ProviderHealth {
	health := ProviderHealth{
		Name:     providerName,
		Checks:   append([]ReadinessCheck(nil), diagnostics.Checks...),
		Warnings: append([]string(nil), diagnostics.Warnings...),
		Models:   append([]string(nil), diagnostics.Models...),
		Healthy:  diagnostics.Healthy(),
		Error:    diagnostics.Error(),
	}

	if diagnostics.Contract.AdapterVersion != "" {
		contract := diagnostics.Contract
		health.Contract = &contract
	}

	return health
}

func registerConfiguredProvider(r *Registry, cfg AutoRegisterConfig, providerName string, factory providerFactory) {
	providerCfg := providerConfig(cfg, providerName)
	if providerCfg.Disabled {
		cfg.logger().Debug("llm provider skipped: disabled by config", "provider", providerName)
		return
	}

	if privateAdapterDisabled(providerName, providerCfg) {
		cfg.logger().Debug("llm provider skipped: private adapter disabled", "provider", providerName)
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

func privateAdapterDisabled(providerName string, cfg ProviderConfig) bool {
	if !isPrivateAdapterProvider(providerName) {
		return false
	}

	return cfg.DisablePrivateAdapter ||
		envBool("ATTELER_DISABLE_PRIVATE_ADAPTERS") ||
		envBool("ATTELER_DISABLE_BORROWED_CREDENTIAL_ADAPTERS") ||
		envBool(privateAdapterProviderKillSwitch(providerName))
}

func isPrivateAdapterProvider(providerName string) bool {
	return providerName == providerCodex || providerName == providerClaudeCode
}

func privateAdapterProviderKillSwitch(providerName string) string {
	switch providerName {
	case providerCodex:
		return "ATTELER_DISABLE_CODEX_ADAPTER"
	case providerClaudeCode:
		return "ATTELER_DISABLE_CLAUDE_CODE_ADAPTER"
	default:
		return ""
	}
}

func envBool(name string) bool {
	if name == "" {
		return false
	}

	value, ok := os.LookupEnv(name)
	if !ok {
		return false
	}

	switch strings.ToLower(strings.TrimSpace(value)) {
	case "", "0", "false", "no", "off":
		return false
	default:
		return true
	}
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
