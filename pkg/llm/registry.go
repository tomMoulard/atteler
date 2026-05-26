package llm

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"slices"
	"strings"
	"time"

	"github.com/tommoulard/atteler/pkg/modelroute"
)

type providerFactory func() (Provider, error)

type providerRegistration struct {
	factory      providerFactory
	name         string
	staticModels []string
}

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
//
//nolint:govet // Field order keeps caller-facing config groups readable.
type AutoRegisterConfig struct {
	// Logger receives registration progress messages. When nil, slog.Default() is used.
	Logger *slog.Logger

	Providers      map[string]ProviderConfig
	FallbackModels []string

	DefaultProvider string
	DefaultModel    string
	SelectedModel   string

	// ReadinessCacheTTL controls the in-process cache for provider readiness
	// checks. A zero value uses DefaultReadinessCacheTTL.
	ReadinessCacheTTL time.Duration

	// ReadinessCheckTimeout bounds each auto-registration readiness probe. A
	// zero value uses DefaultReadinessCheckTimeout.
	ReadinessCheckTimeout time.Duration

	// DisableReadinessChecks skips network health checks during auto-registration
	// while still reporting registration, disabled providers, and credential
	// failures.
	DisableReadinessChecks bool
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

// AutoRegisterReport is AutoRegister plus an empty provider readiness report.
// It is kept for source compatibility and does not perform credential or
// network work without a caller-provided context.
//
// Deprecated: use AutoRegisterContextReport.
func AutoRegisterReport() (*Registry, ProviderReadinessReport) {
	return AutoRegisterWithConfigReport(AutoRegisterConfig{})
}

// AutoRegisterContext is AutoRegister with caller-provided cancellation.
func AutoRegisterContext(ctx context.Context) *Registry {
	return AutoRegisterWithConfigContext(ctx, AutoRegisterConfig{})
}

// AutoRegisterContextReport is AutoRegisterContext plus the provider readiness
// report collected during auto-registration.
func AutoRegisterContextReport(ctx context.Context) (*Registry, ProviderReadinessReport) {
	return AutoRegisterWithConfigContextReport(ctx, AutoRegisterConfig{})
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
	report := applyDefaultSelection(r, cfg)
	r.mu.Lock()
	r.readiness.Default = report
	r.mu.Unlock()

	return r
}

// AutoRegisterWithConfigReport is AutoRegisterWithConfig plus an empty provider
// readiness report. It does not perform credential or network work without a
// caller-provided context.
//
// Deprecated: use AutoRegisterWithConfigContextReport.
func AutoRegisterWithConfigReport(cfg AutoRegisterConfig) (*Registry, ProviderReadinessReport) {
	r := AutoRegisterWithConfig(cfg)
	report := r.ReadinessReport()

	return r, report
}

// AutoRegisterWithConfigContext is AutoRegisterWithConfig with caller-provided
// cancellation for credential discovery and provider readiness checks.
func AutoRegisterWithConfigContext(ctx context.Context, cfg AutoRegisterConfig) *Registry {
	r, _ := AutoRegisterWithConfigContextReport(ctx, cfg)

	return r
}

// AutoRegisterWithConfigContextReport is AutoRegisterWithConfigContext plus the
// provider readiness report collected during auto-registration.
func AutoRegisterWithConfigContextReport(ctx context.Context, cfg AutoRegisterConfig) (*Registry, ProviderReadinessReport) {
	r := NewRegistry()

	if err := requireCredentialContext(ctx); err != nil {
		logProviderSkip(cfg.logger(), "all", err, false)
		defaultReport := applyDefaultSelection(r, cfg)
		r.mu.Lock()
		r.readiness.Default = defaultReport
		r.mu.Unlock()

		report := r.ReadinessReport()

		return r, report
	}

	r = autoRegisterWithFactoriesContext(ctx, cfg, builtinProviderRegistrations(ctx, cfg))
	report := r.ReadinessReport()

	return r, report
}

func builtinProviderRegistrations(ctx context.Context, cfg AutoRegisterConfig) []providerRegistration {
	return []providerRegistration{
		{
			name:         providerAnthropic,
			staticModels: (&AnthropicProvider{}).Models(),
			factory: func() (Provider, error) {
				return NewAnthropicProviderWithConfigContext(ctx, providerConfig(cfg, providerAnthropic))
			},
		},
		{
			name:         providerOpenAI,
			staticModels: (&OpenAIProvider{}).Models(),
			factory: func() (Provider, error) {
				return NewOpenAIProviderWithConfigContext(ctx, providerConfig(cfg, providerOpenAI))
			},
		},
		{
			name:         providerOllama,
			staticModels: (&OllamaProvider{}).Models(),
			factory: func() (Provider, error) {
				ollamaConfig := providerConfig(cfg, providerOllama)
				ollamaConfig.AutoStart = ollamaConfig.AutoStart || shouldAutoStartOllama(cfg)

				return NewOllamaProviderWithConfigContext(ctx, ollamaConfig)
			},
		},
		{
			name:         providerClaudeCode,
			staticModels: defaultClaudeCodeModels(),
			factory: func() (Provider, error) {
				return NewClaudeCodeProviderWithConfigContext(ctx, providerConfig(cfg, providerClaudeCode))
			},
		},
		{
			name:         providerCodex,
			staticModels: defaultCodexModels(),
			factory: func() (Provider, error) {
				return NewCodexProviderWithConfigContext(ctx, providerConfig(cfg, providerCodex))
			},
		},
	}
}

func autoRegisterWithFactoriesContext(ctx context.Context, cfg AutoRegisterConfig, registrations []providerRegistration) *Registry {
	r := NewRegistry()

	for _, registration := range registrations {
		registerProviderWithReadiness(ctx, r, cfg, registration)
	}

	defaultReport := applyDefaultSelection(r, cfg)
	r.mu.Lock()
	r.readiness.Default = defaultReport
	r.mu.Unlock()

	return r
}

func registerProviderWithReadiness(
	ctx context.Context,
	r *Registry,
	cfg AutoRegisterConfig,
	registration providerRegistration,
) {
	providerName := registration.name
	providerCfg := providerConfig(cfg, providerName)
	entry := ProviderReadiness{
		Name:               providerName,
		Status:             ProviderStatusRegistered,
		Configured:         providerConfigured(cfg, providerName),
		Requested:          providerRequested(cfg, providerName),
		ModelCatalogSource: ModelCatalogSourceStatic,
		Models:             cleanModelList(registration.staticModels),
		StaticModels:       cleanModelList(registration.staticModels),
	}

	if providerCfg.Disabled || privateAdapterDisabled(providerName, providerCfg) {
		entry.Status = ProviderStatusDisabled

		r.mu.Lock()
		r.upsertReadinessProviderLocked(entry)
		r.mu.Unlock()

		cfg.logger().Debug("llm provider skipped: disabled by config", "provider", providerName)

		return
	}

	r.mu.Lock()
	r.upsertReadinessProviderLocked(entry)
	r.mu.Unlock()

	p, err := registration.factory()
	if err != nil {
		entry.Error = err
		if isMissingCredentialError(err) {
			entry.Status = ProviderStatusMissingCredential
		} else {
			entry.Status = ProviderStatusFailed
		}

		r.mu.Lock()
		r.upsertReadinessProviderLocked(entry)
		r.mu.Unlock()

		logProviderSkip(cfg.logger(), providerName, err, entry.Configured || entry.Requested)

		return
	}

	r.Register(p)

	r.mu.Lock()
	if registered, ok := r.readinessProviderLocked(providerName); ok {
		entry = registered
	}

	entry.Name = providerName
	entry.Status = ProviderStatusRegistered
	entry.Registered = true
	entry.Configured = providerConfigured(cfg, providerName)
	entry.Requested = providerRequested(cfg, providerName)
	entry.ModelCatalogSource = ModelCatalogSourceStatic
	entry.Models = cleanModelList(p.Models())
	entry.StaticModels = cleanModelList(p.Models())
	r.upsertReadinessProviderLocked(entry)
	r.mu.Unlock()

	if shouldCheckProviderReadiness(cfg, entry) {
		checkCtx, cancel := context.WithTimeout(ctx, readinessCheckTimeout(cfg))
		defer cancel()

		r.checkProviderHealth(checkCtx, providerName, readinessCacheTTL(cfg))
	}
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
		Name:         providerName,
		Checks:       append([]ReadinessCheck(nil), diagnostics.Checks...),
		Warnings:     append([]string(nil), diagnostics.Warnings...),
		Models:       append([]string(nil), diagnostics.Models...),
		StaticModels: append([]string(nil), diagnostics.Models...),
		ModelSource:  ModelCatalogSourceStatic,
		Healthy:      diagnostics.Healthy(),
		Error:        diagnostics.Error(),
	}

	if diagnostics.Contract.AdapterVersion != "" {
		contract := diagnostics.Contract
		health.Contract = &contract
	}

	return health
}

func applyDefaultSelection(r *Registry, cfg AutoRegisterConfig) DefaultSelectionReport {
	logger := cfg.logger()
	report := DefaultSelectionReport{
		Provider: cfg.DefaultProvider,
		Model:    cfg.DefaultModel,
	}

	if cfg.DefaultProvider != "" {
		if err := r.SetDefault(cfg.DefaultProvider); err != nil {
			report.ProviderError = err
			logger.Warn("llm default provider ignored", "provider", cfg.DefaultProvider, "error", err)
		}
	}

	if cfg.DefaultModel != "" {
		mismatchErr := defaultModelMismatch(cfg.DefaultProvider, cfg.DefaultModel)

		err := mismatchErr
		if err == nil {
			err = r.SetDefaultModel(cfg.DefaultModel)
		}

		if err != nil && cfg.DefaultProvider != "" && mismatchErr == nil {
			err = r.SetDefaultProviderModel(cfg.DefaultProvider, cfg.DefaultModel)
		}

		if err != nil {
			report.ModelError = err
			logger.Warn("llm default model ignored", "model", cfg.DefaultModel, "error", err)
		}
	}

	return report
}

func logProviderSkip(logger *slog.Logger, providerName string, err error, visible bool) {
	if isMissingCredentialError(err) && !visible {
		return
	}

	if visible {
		logger.Warn("llm provider unavailable", "provider", providerName, "error", err)

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

func providerConfigured(cfg AutoRegisterConfig, name string) bool {
	if cfg.Providers == nil {
		return false
	}

	_, ok := cfg.Providers[name]

	return ok
}

func providerRequested(cfg AutoRegisterConfig, name string) bool {
	if providerExplicitlyRequested(cfg, name) {
		return true
	}

	switch name {
	case providerOpenAI:
		return providerRequestedByModelPrefix(cfg, "gpt", "o1", "o3", "o4")
	case providerAnthropic, providerClaudeCode:
		return providerRequestedByModelPrefix(cfg, "claude")
	case providerOllama:
		return providerRequestedByKnownOllamaModel(cfg)
	default:
		return false
	}
}

func providerExplicitlyRequested(cfg AutoRegisterConfig, name string) bool {
	return strings.EqualFold(cfg.DefaultProvider, name) ||
		modelNamesProvider(cfg.DefaultModel, name) ||
		modelNamesProvider(cfg.SelectedModel, name) ||
		slices.ContainsFunc(cfg.FallbackModels, func(model string) bool {
			return modelNamesProvider(model, name)
		})
}

func providerRequestedByModelPrefix(cfg AutoRegisterConfig, prefixes ...string) bool {
	return unqualifiedModelHasAnyPrefix(cfg.DefaultModel, prefixes...) ||
		unqualifiedModelHasAnyPrefix(cfg.SelectedModel, prefixes...) ||
		anyUnqualifiedModelHasPrefix(cfg.FallbackModels, prefixes...)
}

func providerRequestedByKnownOllamaModel(cfg AutoRegisterConfig) bool {
	return isKnownOllamaModelName(cfg.DefaultModel) ||
		isKnownOllamaModelName(cfg.SelectedModel) ||
		slices.ContainsFunc(cfg.FallbackModels, isKnownOllamaModelName)
}

func shouldCheckProviderReadiness(cfg AutoRegisterConfig, entry ProviderReadiness) bool {
	if cfg.DisableReadinessChecks {
		return false
	}

	return entry.Registered && (entry.Configured || entry.Requested)
}

func readinessCacheTTL(cfg AutoRegisterConfig) time.Duration {
	if cfg.ReadinessCacheTTL > 0 {
		return cfg.ReadinessCacheTTL
	}

	return DefaultReadinessCacheTTL
}

func readinessCheckTimeout(cfg AutoRegisterConfig) time.Duration {
	if cfg.ReadinessCheckTimeout > 0 {
		return cfg.ReadinessCheckTimeout
	}

	return DefaultReadinessCheckTimeout
}

func defaultModelMismatch(providerName, model string) error {
	providerName = strings.ToLower(strings.TrimSpace(providerName))
	model = strings.TrimSpace(model)

	if providerName == "" || model == "" {
		return nil
	}

	if modelProvider, _, ok := splitProviderModel(model); ok {
		if !strings.EqualFold(modelProvider, providerName) {
			return defaultModelProviderMismatchError(providerName, modelProvider, model)
		}

		return nil
	}

	switch {
	case modelHasAnyPrefix(model, "claude"):
		if providerName != providerAnthropic && providerName != providerClaudeCode {
			return defaultModelProviderMismatchError(providerName, providerAnthropic, model)
		}
	case modelHasAnyPrefix(model, "gpt", "o1", "o3", "o4"):
		if providerName != providerOpenAI && providerName != providerCodex {
			return defaultModelProviderMismatchError(providerName, providerOpenAI, model)
		}
	case isKnownOllamaModelName(model):
		if providerName != providerOllama {
			return defaultModelProviderMismatchError(providerName, providerOllama, model)
		}
	}

	return nil
}

func defaultModelProviderMismatchError(defaultProvider, modelProvider, model string) error {
	return fmt.Errorf("llm: default model %q appears to belong to provider %q, not default provider %q",
		model, modelProvider, defaultProvider)
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
		slices.ContainsFunc(cfg.FallbackModels, func(model string) bool {
			return modelNamesProvider(model, providerOllama)
		}) ||
		isKnownOllamaModelName(cfg.DefaultModel) ||
		isKnownOllamaModelName(cfg.SelectedModel) ||
		slices.ContainsFunc(cfg.FallbackModels, isKnownOllamaModelName)
}

func modelNamesProvider(model, provider string) bool {
	model = strings.TrimSpace(model)
	if model == "" {
		return false
	}

	prefix, _, ok := strings.Cut(model, "/")

	return ok && strings.EqualFold(prefix, provider)
}

func modelHasAnyPrefix(model string, prefixes ...string) bool {
	model = strings.ToLower(strings.TrimSpace(model))
	if model == "" {
		return false
	}

	if _, providerModel, ok := splitProviderModel(model); ok {
		model = strings.ToLower(providerModel)
	}

	for _, prefix := range prefixes {
		if strings.HasPrefix(model, strings.ToLower(prefix)) {
			return true
		}
	}

	return false
}

func unqualifiedModelHasAnyPrefix(model string, prefixes ...string) bool {
	if _, _, ok := splitProviderModel(model); ok {
		return false
	}

	return modelHasAnyPrefix(model, prefixes...)
}

func anyUnqualifiedModelHasPrefix(models []string, prefixes ...string) bool {
	for _, model := range models {
		if unqualifiedModelHasAnyPrefix(model, prefixes...) {
			return true
		}
	}

	return false
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
