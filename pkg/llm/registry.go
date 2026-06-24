package llm

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"net/http"
	"os"
	"slices"
	"sort"
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
//
//nolint:govet // Field order keeps provider capability metadata first for display callers.
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
	ModelAliases   map[string]string
	ModelRoles     map[string]ModelRole
	FallbackModels []string
	CommandLine    []string

	DefaultProvider string
	DefaultModel    string
	SessionID       string
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

	// DisableAutoStart prevents auto-registration from starting local daemons
	// for inspection-only commands.
	DisableAutoStart bool
}

// logger returns the configured logger, falling back to the standard default.
func (c AutoRegisterConfig) logger() *slog.Logger {
	if c.Logger != nil {
		return c.Logger
	}

	return slog.Default()
}

// ProviderConfig configures one LLM provider.
//
//nolint:govet // Field order keeps externally useful provider settings grouped.
type ProviderConfig struct {
	Retry                 RetryPolicyConfig
	CredentialPolicy      CredentialSourcePolicy
	BaseURL               string
	Type                  string
	APIKeyEnv             string
	APIKeyHeader          string
	APIKeyScheme          string
	ChatCompletionsPath   string
	EmbeddingsPath        string
	ModelsPath            string
	APIVersion            string
	OwnershipPath         string
	SessionID             string
	Models                []string
	Capabilities          []string
	CommandLine           []string
	DisablePrivateAdapter bool
	Disabled              bool
	Local                 bool
	AutoStart             bool
	TimeoutSeconds        int

	autoStartBlocked bool
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

// KnownProvidersContext returns built-in provider model catalogs while routing
// any local provider configuration inspection through ctx-scoped permission
// policy.
func KnownProvidersContext(ctx context.Context) ([]ProviderInfo, error) {
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
		models, err := knownProviderModelsContext(ctx, provider)
		if err != nil {
			return nil, err
		}

		out = append(out, ProviderInfo{
			Capabilities: ProviderCapabilitiesFor(provider),
			Name:         provider.Name(),
			Models:       mergeModelLists(models, catalogModels[provider.Name()]),
		})
	}

	return out, nil
}

func knownProviderModelsContext(ctx context.Context, provider Provider) ([]string, error) {
	if provider != nil && provider.Name() == providerCodex {
		return codexModelsContext(ctx)
	}

	return provider.Models(), nil
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
	applyModelRoles(r, cfg)
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
	registrations := []providerRegistration{
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
				ollamaConfig.autoStartBlocked = cfg.DisableAutoStart || !shouldAutoStartOllama(cfg)

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

	return append(registrations, openAICompatibleProviderRegistrations(ctx, cfg)...)
}

func openAICompatibleProviderRegistrations(ctx context.Context, cfg AutoRegisterConfig) []providerRegistration {
	names := make([]string, 0, len(cfg.Providers))
	for name := range cfg.Providers {
		if isBuiltinProviderName(name) {
			continue
		}

		if _, ok := openAICompatibleProviderConfig(cfg, name); !ok {
			continue
		}

		names = append(names, name)
	}

	sort.Strings(names)

	registrations := make([]providerRegistration, 0, len(names))
	for _, name := range names {
		providerName := name
		providerCfg, _ := openAICompatibleProviderConfig(cfg, providerName)
		registrations = append(registrations, providerRegistration{
			name:         providerName,
			staticModels: cleanModelList(providerCfg.Models),
			factory: func() (Provider, error) {
				return NewOpenAICompatibleProviderWithConfigContext(ctx, providerName, providerCfg)
			},
		})
	}

	return registrations
}

func openAICompatibleProviderConfig(cfg AutoRegisterConfig, name string) (ProviderConfig, bool) {
	providerCfg := providerConfig(cfg, name)
	if isOpenAICompatibleProviderType(providerCfg.Type) {
		return providerCfg, true
	}

	// Treat well-known OpenAI-compatible backend names such as "groq" and
	// "vllm" as type aliases when a base URL is configured. This keeps custom
	// endpoints first-class without forcing users to repeat
	// `type: openai_compatible` for common provider names.
	if strings.TrimSpace(providerCfg.Type) == "" &&
		strings.TrimSpace(providerCfg.BaseURL) != "" &&
		isOpenAICompatibleProviderType(name) {
		providerCfg.Type = name

		return providerCfg, true
	}

	return ProviderConfig{}, false
}

func isBuiltinProviderName(name string) bool {
	switch strings.ToLower(strings.TrimSpace(name)) {
	case providerAnthropic, providerOpenAI, providerOllama, providerClaudeCode, providerCodex:
		return true
	default:
		return false
	}
}

func isOpenAICompatibleProviderType(providerType string) bool {
	switch normalizeOpenAIProviderType(providerType) {
	case openAICompatibleType, azureOpenAIType:
		return true
	default:
		return false
	}
}

// IsOpenAICompatibleProviderType reports whether providerType is accepted for
// OpenAI-compatible endpoint auto-registration. It includes Azure/OpenAI path
// aliases and hosted/self-hosted provider aliases that expose the OpenAI wire
// shape.
func IsOpenAICompatibleProviderType(providerType string) bool {
	return isOpenAICompatibleProviderType(providerType)
}

func autoRegisterWithFactoriesContext(ctx context.Context, cfg AutoRegisterConfig, registrations []providerRegistration) *Registry {
	r := NewRegistry()

	for _, registration := range registrations {
		registerProviderWithReadiness(ctx, r, cfg, registration)
	}

	applyModelAliases(r, cfg)
	applyModelRoles(r, cfg)
	applyUserModelOverrides(r, cfg)

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
	r.applyProviderRetryConfig(providerName, providerCfg.Retry)

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
				privateAdapterModelsContext(ctx, providerName),
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
			return privateAdapterCredentialFailure(providerCodex, codexAdapterContract(), privateAdapterModelsContext(ctx, providerCodex), err)
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

func privateAdapterModelsContext(ctx context.Context, providerName string) []string {
	switch providerName {
	case providerCodex:
		models, err := codexModelsContext(ctx)
		if err != nil {
			return defaultCodexModels()
		}

		return models
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
			{Name: "local_credentials", Status: ReadinessFailed, Detail: RedactDiagnosticMessage(err.Error())},
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
		Checks:       redactReadinessChecks(diagnostics.Checks),
		Warnings:     redactDiagnosticStrings(diagnostics.Warnings),
		Models:       append([]string(nil), diagnostics.Models...),
		StaticModels: append([]string(nil), diagnostics.Models...),
		ModelSource:  ModelCatalogSourceStatic,
		Healthy:      diagnostics.Healthy(),
		Error:        redactDiagnosticError(diagnostics.Error()),
	}

	if diagnostics.Contract.AdapterVersion != "" {
		contract := redactAdapterContract(diagnostics.Contract)
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

	if err := applyDefaultModelSelection(r, cfg); err != nil {
		report.ModelError = err
		logger.Warn("llm default model ignored", "model", cfg.DefaultModel, "error", err)
	}

	return report
}

func applyDefaultModelSelection(r *Registry, cfg AutoRegisterConfig) error {
	if cfg.DefaultModel == "" {
		return nil
	}

	err := r.SetDefaultModel(cfg.DefaultModel)
	if err == nil || cfg.DefaultProvider == "" || isDefaultModelProviderMismatch(err) {
		return err
	}

	if mismatchErr := defaultModelMismatch(cfg.DefaultProvider, cfg.DefaultModel, cfg.ModelAliases); mismatchErr != nil {
		return mismatchErr
	}

	return r.SetDefaultProviderModel(cfg.DefaultProvider, cfg.DefaultModel)
}

func applyModelAliases(r *Registry, cfg AutoRegisterConfig) {
	logger := cfg.logger()

	aliases := make([]string, 0, len(cfg.ModelAliases))
	for alias := range cfg.ModelAliases {
		aliases = append(aliases, alias)
	}

	sort.Strings(aliases)

	for _, alias := range aliases {
		target := strings.TrimSpace(cfg.ModelAliases[alias])

		providerName, providerModel, ok := splitProviderModel(target)
		if !ok {
			logger.Warn(
				"llm model alias ignored",
				"alias",
				alias,
				"target",
				target,
				"error",
				"target must be provider/model",
			)

			continue
		}

		if err := r.SetModelAlias(alias, providerName, providerModel); err != nil {
			logger.Warn("llm model alias ignored", "alias", alias, "target", target, "error", err)
		}
	}
}

func applyModelRoles(r *Registry, cfg AutoRegisterConfig) {
	logger := cfg.logger()

	roles := make([]string, 0, len(cfg.ModelRoles))
	for role := range cfg.ModelRoles {
		roles = append(roles, role)
	}

	sort.Strings(roles)

	for _, role := range roles {
		if err := r.SetModelRole(role, cfg.ModelRoles[role]); err != nil {
			logger.Warn("llm model role ignored", "role", role, "error", err)
		}
	}
}

func applyUserModelOverrides(r *Registry, cfg AutoRegisterConfig) {
	logger := cfg.logger()

	for _, model := range providerQualifiedUserModels(cfg) {
		providerName, providerModel, ok := splitProviderModel(model)
		if !ok {
			continue
		}

		if err := r.SetProviderModelOverride(providerName, providerModel); err != nil {
			logger.Warn("llm model override ignored", "model", model, "error", err)
		}
	}
}

func providerQualifiedUserModels(cfg AutoRegisterConfig) []string {
	seen := make(map[string]bool)

	record := func(model string) {
		model = strings.TrimSpace(model)
		if model == "" {
			return
		}

		if _, _, ok := splitProviderModel(model); !ok {
			return
		}

		seen[model] = true
	}

	record(cfg.SelectedModel)

	for _, model := range cfg.FallbackModels {
		record(model)
	}

	models := make([]string, 0, len(seen))
	for model := range seen {
		models = append(models, model)
	}

	sort.Strings(models)

	return models
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
	var provider ProviderConfig
	if cfg.Providers == nil {
		provider = ProviderConfig{}
	} else {
		provider = cfg.Providers[name]
	}

	if provider.SessionID == "" {
		provider.SessionID = cfg.SessionID
	}

	if len(provider.CommandLine) == 0 {
		provider.CommandLine = append([]string(nil), cfg.CommandLine...)
	}

	provider.Models = append([]string(nil), provider.Models...)
	provider.Capabilities = append([]string(nil), provider.Capabilities...)
	provider.CredentialPolicy.AllowedProviders = append([]string(nil), provider.CredentialPolicy.AllowedProviders...)
	provider.CredentialPolicy.AllowedStores = append([]string(nil), provider.CredentialPolicy.AllowedStores...)

	return provider
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

	if providerRequestedByModelAlias(cfg, name) {
		return true
	}

	if providerRequestedByModelRole(cfg, name) {
		return true
	}

	// Compatibility-only readiness hint: legacy bare model prefixes can still
	// make provider diagnostics visible, but Registry resolution no longer uses
	// these prefixes to route completion requests. Keep this isolated to
	// auto-registration/readiness discovery; do not call it from model
	// resolution paths.
	switch name {
	case providerOpenAI:
		return providerRequestedByLegacyModelPrefix(cfg, "gpt", "o1", "o3", "o4")
	case providerAnthropic, providerClaudeCode:
		return providerRequestedByLegacyModelPrefix(cfg, "claude")
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

func providerRequestedByModelAlias(cfg AutoRegisterConfig, name string) bool {
	for alias, target := range cfg.ModelAliases {
		alias = strings.TrimSpace(alias)
		if alias == "" {
			continue
		}

		if !modelNamesProvider(target, name) {
			continue
		}

		if strings.TrimSpace(cfg.DefaultModel) == alias ||
			strings.TrimSpace(cfg.SelectedModel) == alias ||
			slices.ContainsFunc(cfg.FallbackModels, func(model string) bool {
				return strings.TrimSpace(model) == alias
			}) {
			return true
		}
	}

	return false
}

func providerRequestedByModelRole(cfg AutoRegisterConfig, name string) bool {
	for roleName := range cfg.ModelRoles {
		role := cfg.ModelRoles[roleName]
		if strings.TrimSpace(roleName) == "" {
			continue
		}

		if !modelRoleSelectedByConfig(cfg, roleName) {
			continue
		}

		if modelRoleReferencesProvider(cfg.ModelRoles, roleName, role, name, nil) {
			return true
		}
	}

	return false
}

func modelRoleSelectedByConfig(cfg AutoRegisterConfig, roleName string) bool {
	roleName = strings.TrimSpace(roleName)
	if roleName == "" {
		return false
	}

	return strings.TrimSpace(cfg.DefaultModel) == roleName ||
		strings.TrimSpace(cfg.SelectedModel) == roleName ||
		slices.ContainsFunc(cfg.FallbackModels, func(model string) bool {
			return strings.TrimSpace(model) == roleName
		})
}

func modelRoleReferencesProvider(
	roles map[string]ModelRole,
	roleName string,
	role ModelRole,
	providerName string,
	visited map[string]bool,
) bool {
	return modelRoleReferencesProviderWithPolicy(
		roles,
		roleName,
		role,
		providerName,
		visited,
		modelroute.Policy{},
	)
}

func modelRoleReferencesProviderWithPolicy(
	roles map[string]ModelRole,
	roleName string,
	role ModelRole,
	providerName string,
	visited map[string]bool,
	inheritedPolicy modelroute.Policy,
) bool {
	policy := mergeModelRoleRoutingPolicy(inheritedPolicy, role.policy())
	if routePolicyBansProvider(policy, providerName) {
		return false
	}

	roleName = strings.TrimSpace(roleName)
	if roleName != "" {
		if visited[roleName] {
			return false
		}

		nextVisited := make(map[string]bool, len(visited)+1)
		maps.Copy(nextVisited, visited)
		nextVisited[roleName] = true
		visited = nextVisited
	}

	if routePolicyPrefersProvider(policy, providerName) {
		return true
	}

	for _, model := range modelFallbackChain(role.Preferred, role.FallbackModels) {
		nestedRole, ok := roles[strings.TrimSpace(model)]
		if ok {
			if modelRoleReferencesProviderWithPolicy(roles, model, nestedRole, providerName, visited, policy) {
				return true
			}

			continue
		}

		if routePolicyBansModelForProvider(policy, model, providerName) {
			continue
		}

		if modelNamesProvider(model, providerName) || catalogModelNamesProvider(model, providerName) {
			return true
		}
	}

	return false
}

func routePolicyPrefersProvider(policy modelroute.Policy, providerName string) bool {
	providerName = strings.TrimSpace(providerName)
	if providerName == "" {
		return false
	}

	return slices.ContainsFunc(policy.PreferredProviders, func(candidate string) bool {
		return strings.EqualFold(strings.TrimSpace(candidate), providerName)
	})
}

func routePolicyBansProvider(policy modelroute.Policy, providerName string) bool {
	providerName = strings.TrimSpace(providerName)
	if providerName == "" {
		return false
	}

	return slices.ContainsFunc(policy.BannedProviders, func(candidate string) bool {
		return strings.EqualFold(strings.TrimSpace(candidate), providerName)
	})
}

func routePolicyBansModelForProvider(policy modelroute.Policy, model, providerName string) bool {
	ids := routePolicyModelIDsForProvider(model, providerName)
	if len(ids) == 0 {
		return false
	}

	return slices.ContainsFunc(policy.BannedModels, func(candidate string) bool {
		candidate = strings.ToLower(strings.TrimSpace(candidate))

		return candidate != "" && slices.Contains(ids, candidate)
	})
}

func routePolicyModelIDsForProvider(model, providerName string) []string {
	model = strings.TrimSpace(model)
	if model == "" {
		return nil
	}

	if provider, providerModel, ok := splitProviderModel(model); ok {
		return uniqueRoleStrings([]string{
			model,
			providerModel,
			provider + "/" + providerModel,
		})
	}

	providerName = strings.TrimSpace(providerName)

	return uniqueRoleStrings([]string{
		model,
		providerName + "/" + model,
	})
}

func catalogModelNamesProvider(model, providerName string) bool {
	providerName = strings.TrimSpace(providerName)
	if providerName == "" {
		return false
	}

	if provider, _, ok := splitProviderModel(model); ok {
		return strings.EqualFold(provider, providerName)
	}

	_, ok := modelroute.BuiltinCatalog().Lookup(providerName, model)

	return ok
}

func providerRequestedByLegacyModelPrefix(cfg AutoRegisterConfig, prefixes ...string) bool {
	return legacyUnqualifiedModelHasAnyPrefix(cfg.DefaultModel, prefixes...) ||
		legacyUnqualifiedModelHasAnyPrefix(cfg.SelectedModel, prefixes...) ||
		anyLegacyUnqualifiedModelHasPrefix(cfg.FallbackModels, prefixes...)
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

func defaultModelMismatch(providerName, model string, aliases map[string]string) error {
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

	if target, ok := configuredAliasTarget(aliases, model); ok {
		modelProvider, _, ok := splitProviderModel(target)
		if ok && !strings.EqualFold(modelProvider, providerName) {
			return defaultModelProviderMismatchError(providerName, modelProvider, model)
		}
	}

	return nil
}

func configuredAliasTarget(aliases map[string]string, alias string) (string, bool) {
	alias = strings.TrimSpace(alias)
	for configuredAlias, target := range aliases {
		if strings.TrimSpace(configuredAlias) == alias {
			return strings.TrimSpace(target), true
		}
	}

	return "", false
}

func defaultModelProviderMismatchError(defaultProvider, modelProvider, model string) error {
	return defaultModelProviderMismatch{
		defaultProvider: defaultProvider,
		modelProvider:   modelProvider,
		model:           model,
	}
}

type defaultModelProviderMismatch struct {
	defaultProvider string
	modelProvider   string
	model           string
}

func (e defaultModelProviderMismatch) Error() string {
	return fmt.Sprintf(
		"llm: default model %q appears to belong to provider %q, not default provider %q",
		e.model,
		e.modelProvider,
		e.defaultProvider,
	)
}

func isDefaultModelProviderMismatch(err error) bool {
	var mismatch defaultModelProviderMismatch

	return errors.As(err, &mismatch)
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
		modelRolesReferenceOllama(cfg) ||
		isKnownOllamaModelName(cfg.DefaultModel) ||
		isKnownOllamaModelName(cfg.SelectedModel) ||
		slices.ContainsFunc(cfg.FallbackModels, isKnownOllamaModelName)
}

func modelRolesReferenceOllama(cfg AutoRegisterConfig) bool {
	for roleName := range cfg.ModelRoles {
		role := cfg.ModelRoles[roleName]
		if strings.TrimSpace(roleName) == "" {
			continue
		}

		if !modelRoleSelectedByConfig(cfg, roleName) {
			continue
		}

		if modelRoleReferencesProvider(cfg.ModelRoles, roleName, role, providerOllama, nil) {
			return true
		}
	}

	return false
}

func modelNamesProvider(model, provider string) bool {
	model = strings.TrimSpace(model)
	if model == "" {
		return false
	}

	prefix, _, ok := strings.Cut(model, "/")

	return ok && strings.EqualFold(prefix, provider)
}

func legacyModelHasAnyPrefix(model string, prefixes ...string) bool {
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

func legacyUnqualifiedModelHasAnyPrefix(model string, prefixes ...string) bool {
	if _, _, ok := splitProviderModel(model); ok {
		return false
	}

	return legacyModelHasAnyPrefix(model, prefixes...)
}

func anyLegacyUnqualifiedModelHasPrefix(models []string, prefixes ...string) bool {
	for _, model := range models {
		if legacyUnqualifiedModelHasAnyPrefix(model, prefixes...) {
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
