package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"os"
	"sort"
	"strconv"
	"strings"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/tommoulard/atteler/pkg/agent"
	appconfig "github.com/tommoulard/atteler/pkg/config"
	"github.com/tommoulard/atteler/pkg/contextpack"
	"github.com/tommoulard/atteler/pkg/contextref"
	"github.com/tommoulard/atteler/pkg/events"
	"github.com/tommoulard/atteler/pkg/llm"
	"github.com/tommoulard/atteler/pkg/session"
	"github.com/tommoulard/atteler/pkg/worktree"
)

type agentDescription struct {
	Temperature    *float64 `yaml:"temperature,omitempty"`
	TopP           *float64 `yaml:"top_p,omitempty"`
	Seed           *int     `yaml:"seed,omitempty"`
	Name           string   `yaml:"name"`
	ReasoningLevel string   `yaml:"reasoning_level,omitempty"`
	Model          string   `yaml:"model,omitempty"`
	Description    string   `yaml:"description,omitempty"`
	Personality    string   `yaml:"personality,omitempty"`
	SystemPrompt   string   `yaml:"system_prompt,omitempty"`
	FallbackModels []string `yaml:"fallback_models,omitempty"`
	Capabilities   []string `yaml:"capabilities,omitempty"`
	Triggers       []string `yaml:"triggers,omitempty"`
	MaxTokens      int      `yaml:"max_tokens,omitempty"`
}

func describeAgent(agents *agent.Registry, name string) error {
	activeAgent, ok := agents.Get(name)
	if !ok {
		return fmt.Errorf("unknown agent %q", name)
	}

	out, err := formatAgentDescription(activeAgent)
	if err != nil {
		return fmt.Errorf("format agent %q: %w", name, err)
	}

	fmt.Print(out)

	return nil
}

func formatAgentDescription(activeAgent agent.Agent) (string, error) {
	out, err := yaml.Marshal(agentDescription{
		Name:           activeAgent.Name,
		Model:          activeAgent.Model,
		Description:    activeAgent.Description,
		Personality:    activeAgent.Personality,
		SystemPrompt:   activeAgent.SystemPrompt,
		FallbackModels: activeAgent.FallbackModels,
		Capabilities:   activeAgent.Capabilities,
		Temperature:    activeAgent.Temperature,
		TopP:           activeAgent.TopP,
		Seed:           activeAgent.Seed,
		ReasoningLevel: activeAgent.ReasoningLevel,
		Triggers:       activeAgent.Triggers,
		MaxTokens:      activeAgent.MaxTokens,
	})
	if err != nil {
		return "", fmt.Errorf("marshal agent description: %w", err)
	}

	return string(out), nil
}

func doctor(ctx context.Context, state appState) error {
	fmt.Println("Atteler doctor")

	providers := state.registry.ListProviders()
	sort.Strings(providers)

	printDoctorOverview(state, providers)
	fmt.Println(formatOllamaDoctorLine(ctx, state.config))

	// Health check every registered provider and list their models.
	fmt.Println()

	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()

	report := state.registry.CheckReadiness(ctx, 0)
	readinessHealthy := printProviderReadinessReport(report)

	fmt.Println()

	results := providerHealthResults(ctx, state, providers)
	adapterHealthy := 0

	for i := range results {
		result := &results[i]
		if result.Healthy {
			fmt.Printf("  [ok] %s%s\n", result.Name, doctorAdapterSuffix(result.Contract))

			adapterHealthy++
		} else {
			fmt.Printf("  [FAIL] %s%s: %v\n", result.Name, doctorAdapterSuffix(result.Contract), result.Error)
		}

		printDoctorAdapterDetails(result)

		metadataProvider := doctorMetadataProvider(state, result.Name)

		for _, m := range result.Models {
			fmt.Printf("         - %s%s\n", m, doctorModelMetadataSuffix(metadataProvider, m))
		}

		printDoctorRuntimeDetails(result.Name)
	}

	if readinessHealthy == 0 && adapterHealthy == 0 {
		if len(report.Providers) == 0 && len(results) == 0 {
			return errors.New("doctor: no providers registered; set provider credentials or config")
		}

		return errors.New("doctor: all providers failed their health check")
	}

	return nil
}

func printDoctorOverview(state appState, providers []string) {
	if len(state.loadedConfigPaths) == 0 {
		fmt.Println("config: no config files loaded")
	} else {
		fmt.Println("config: " + strings.Join(state.loadedConfigPaths, ", "))
	}

	printConfigStateDoctorSummary()
	fmt.Println("sessions: " + state.sessionStore.Dir() + " (" + pathStatus(state.sessionStore.Dir()) + ")")

	if len(providers) == 0 {
		fmt.Println("providers: none registered")
	} else {
		fmt.Println("providers: " + strings.Join(providers, ", "))
	}

	agents := state.agentRegistry.List()
	if len(agents) == 0 {
		fmt.Println("agents: none configured")
	} else {
		fmt.Println("agents: " + strings.Join(agents, ", "))
	}

	if state.worktreeInfo != nil {
		fmt.Println("worktree: " + worktree.Status(state.worktreeInfo))
	}
}

func providerHealthResults(ctx context.Context, state appState, registeredProviders []string) []llm.ProviderHealth {
	results := state.registry.CheckHealthWithTTL(ctx, llm.DefaultReadinessCacheTTL)
	diagnosticConfig := privateAdapterDiagnosticConfig(state, registeredProviders)
	results = append(results, llm.PrivateAdapterDiagnostics(ctx, diagnosticConfig)...)

	sort.SliceStable(results, func(i, j int) bool {
		return results[i].Name < results[j].Name
	})

	return results
}

func privateAdapterDiagnosticConfig(state appState, registeredProviders []string) llm.AutoRegisterConfig {
	diagnosticConfig := llmConfig(state.config, state.selectedModel, state.fallbackModels, state.sessionState.ID, os.Args)
	if diagnosticConfig.Providers == nil {
		diagnosticConfig.Providers = make(map[string]llm.ProviderConfig)
	}

	for _, providerName := range registeredProviders {
		providerConfig := diagnosticConfig.Providers[providerName]
		providerConfig.Disabled = true
		diagnosticConfig.Providers[providerName] = providerConfig
	}

	return diagnosticConfig
}

func doctorAdapterSuffix(contract *llm.AdapterContract) string {
	if contract == nil || contract.AdapterVersion == "" {
		return ""
	}

	return " adapter=" + contract.AdapterVersion
}

func printDoctorRuntimeDetails(providerName string) {
	if runtime, ok := llm.ProviderRuntime(providerName); ok {
		fmt.Printf("         runtime: %s\n", runtime.ExecutionPath)
		fmt.Printf("         health: %s\n", runtime.HealthCheck)
	}
}

func doctorMetadataProvider(state appState, providerName string) llm.ModelMetadataProvider {
	if provider, found := state.registry.Provider(providerName); found {
		if typedProvider, hasMetadata := provider.(llm.ModelMetadataProvider); hasMetadata {
			return typedProvider
		}
	}

	switch providerName {
	case "codex":
		return &llm.CodexProvider{}
	case "claude-code":
		return &llm.ClaudeCodeProvider{}
	default:
		return nil
	}
}

func printDoctorAdapterDetails(result *llm.ProviderHealth) {
	if result == nil {
		return
	}

	if result.Contract != nil {
		fmt.Printf("         adapter_contract: %s\n", doctorAdapterContractStatus(result))
		fmt.Printf("         contract: source=%s; source_cli_version=%s; protocol=%s; reviewed=%s; review_after=%s\n",
			result.Contract.SourceCLI,
			result.Contract.SourceCLIVersion,
			result.Contract.Protocol,
			result.Contract.ReviewedAt,
			result.Contract.ReviewAfter,
		)
		fmt.Printf("         credentials: %s\n", result.Contract.Credential)

		if len(result.Contract.KillSwitches) > 0 {
			fmt.Printf("         kill_switches: %s\n", strings.Join(result.Contract.KillSwitches, ", "))
		}
	}

	for _, check := range result.Checks {
		fmt.Printf("         [%s] %s: %s\n", check.Status, check.Name, check.Detail)
	}

	for _, warning := range result.Warnings {
		fmt.Printf("         warning: %s\n", warning)
	}
}

func doctorAdapterContractStatus(result *llm.ProviderHealth) string {
	if result == nil {
		return "failed"
	}

	if result.Healthy && result.Error == nil {
		return "passed"
	}

	if result.Error == nil {
		return "failed"
	}

	return "failed: " + result.Error.Error()
}

func doctorModelMetadataSuffix(provider llm.ModelMetadataProvider, model string) string {
	if provider == nil {
		return ""
	}

	metadata, ok := provider.ModelMetadata(model)
	if !ok {
		return ""
	}

	parts := make([]string, 0, 3)
	if metadata.ContextWindow > 0 {
		parts = append(parts, fmt.Sprintf("context=%d", metadata.ContextWindow))
	} else {
		parts = append(parts, "context=unknown")
	}

	if metadata.Provenance != "" {
		parts = append(parts, "provenance="+metadata.Provenance)
	}

	if metadata.ReviewedAt != "" {
		parts = append(parts, "reviewed="+metadata.ReviewedAt)
	}

	if metadata.ReviewAfter != "" {
		parts = append(parts, "review_after="+metadata.ReviewAfter)
	}

	if metadata.Notes != "" {
		parts = append(parts, "notes="+metadata.Notes)
	}

	if len(parts) == 0 {
		return ""
	}

	return " (" + strings.Join(parts, "; ") + ")"
}

func printProviderReadinessReport(report llm.ProviderReadinessReport) int {
	if report.Default.Provider != "" || report.Default.Model != "" {
		fmt.Println("default_selection:")

		if report.Default.Provider != "" {
			printDefaultSelectionLine("provider", report.Default.Provider, report.Default.ProviderError)
		}

		if report.Default.Model != "" {
			printDefaultSelectionLine("model", report.Default.Model, report.Default.ModelError)
		}

		fmt.Println()
	}

	if len(report.Providers) == 0 {
		fmt.Println("providers: none")

		return 0
	}

	fmt.Println("provider_readiness:")

	healthy := 0

	for i := range report.Providers {
		provider := &report.Providers[i]
		label := providerReadinessLabel(provider)
		fmt.Printf("  [%s] %s", label, provider.Name)

		if provider.Configured {
			fmt.Print(" configured")
		}

		if provider.Requested {
			fmt.Print(" requested")
		}

		if provider.HealthCached {
			fmt.Print(" cached")
		}

		fmt.Println()

		if provider.Healthy {
			healthy++
		}

		printProviderReadinessReason(provider)
		printProviderReadinessModels(provider)
	}

	return healthy
}

func printDefaultSelectionLine(kind, value string, err error) {
	if err != nil {
		fmt.Printf("  [%s] %s: %s (%v)\n", statusWarn, kind, value, err)

		return
	}

	fmt.Printf("  [ok] %s: %s\n", kind, value)
}

func providerReadinessLabel(provider *llm.ProviderReadiness) string {
	switch provider.Status {
	case llm.ProviderStatusRegistered:
		if provider.HealthChecked && !provider.Healthy {
			return statusFail
		}

		if provider.ModelFetchError != nil {
			return statusWarn
		}

		return "ok"
	case llm.ProviderStatusDisabled:
		return "skip"
	case llm.ProviderStatusMissingCredential:
		return "auth"
	default:
		return statusFail
	}
}

func printProviderReadinessReason(provider *llm.ProviderReadiness) {
	if provider.Status != "" {
		fmt.Printf("         status: %s\n", provider.Status)
	}

	if provider.Error != nil {
		fmt.Printf("         reason: %v\n", provider.Error)
	}

	if provider.HealthError != nil && errorString(provider.HealthError) != errorString(provider.Error) {
		fmt.Printf("         health: %v\n", provider.HealthError)
	}

	if provider.ModelFetchError != nil && errorString(provider.ModelFetchError) != errorString(provider.Error) {
		fmt.Printf("         model_fetch: %v\n", provider.ModelFetchError)
	}
}

func errorString(err error) string {
	if err == nil {
		return ""
	}

	return err.Error()
}

func printProviderReadinessModels(provider *llm.ProviderReadiness) {
	models := append([]string(nil), provider.Models...)
	sort.Strings(models)

	source := provider.ModelCatalogSource
	if source == "" {
		source = llm.ModelCatalogSourceStatic
	}

	switch source {
	case llm.ModelCatalogSourceLive:
		fmt.Println("         models: live")
	default:
		if provider.ModelsStale {
			fmt.Println("         models: static fallback (stale)")
		} else {
			fmt.Println("         models: static fallback")
		}
	}

	for _, model := range models {
		fmt.Printf("         - %s\n", model)
	}
}

//nolint:unparam // error return kept for consistency with other command handlers.
func doctorOffline(opts cliOptions) error {
	cfg, loadedConfigPaths, err := appconfig.Load()
	if err != nil {
		fmt.Println("config_error: " + err.Error())
	}

	fmt.Println("Atteler offline doctor")

	if len(loadedConfigPaths) == 0 {
		fmt.Println("config: no config files loaded")
	} else {
		fmt.Println("config: " + strings.Join(loadedConfigPaths, ", "))
	}

	printConfigStateDoctorSummary()

	store := session.NewStore(opts.sessionDir)
	fmt.Println("sessions: " + store.Dir() + " (" + pathStatus(store.Dir()) + ")")

	providerNames := make([]string, 0)
	for _, provider := range llm.KnownProviders() {
		providerNames = append(providerNames, provider.Name)
	}

	sort.Strings(providerNames)

	if len(providerNames) == 0 {
		fmt.Println("known_providers: none")
	} else {
		fmt.Println("known_providers: " + strings.Join(providerNames, ", "))
	}

	agents := agent.NewRegistry(cfg.Agents).List()
	if len(agents) == 0 {
		fmt.Println("agents: none configured")
	} else {
		fmt.Println("agents: " + strings.Join(agents, ", "))
	}

	fmt.Println("hook_events: " + strconv.Itoa(len(events.SupportedEventTypes())))

	if len(cfg.Plugins.Paths) == 0 {
		fmt.Println("plugins: none configured")
	} else {
		fmt.Println("plugins: " + strings.Join(cfg.Plugins.Paths, ", "))
	}

	return nil
}

type configDoctorDiagnosticSummary struct {
	errors   int
	warnings int
	info     int
}

func printConfigStateDoctorSummary() {
	fmt.Printf(
		"schema: config=%d state=%d\n",
		appconfig.ConfigSchemaVersion,
		appconfig.StateSchemaVersion,
	)

	configSummary := summarizeSourceDiagnostics(appconfig.InspectPathSources(appconfig.DefaultPathSources()))
	fmt.Printf(
		"config_diagnostics: errors=%d warnings=%d info=%d\n",
		configSummary.errors,
		configSummary.warnings,
		configSummary.info,
	)

	stateReport := appconfig.InspectStatePath(appconfig.DefaultStatePath())

	stateStatus := stateReport.Status
	if stateReport.Version > 0 {
		stateStatus += fmt.Sprintf(", version=%d", stateReport.Version)
	}

	if stateReport.Revision > 0 {
		stateStatus += fmt.Sprintf(", revision=%d", stateReport.Revision)
	}

	stateSummary := summarizeDiagnostics(stateReport.Diagnostics)
	fmt.Printf("state: %s (%s)\n", stateReport.Path, stateStatus)
	fmt.Printf(
		"state_diagnostics: errors=%d warnings=%d info=%d\n",
		stateSummary.errors,
		stateSummary.warnings,
		stateSummary.info,
	)
}

func summarizeSourceDiagnostics(sources []appconfig.SourceDiagnostic) configDoctorDiagnosticSummary {
	var summary configDoctorDiagnosticSummary
	for _, source := range sources {
		summary.add(source.Diagnostics)
	}

	return summary
}

func summarizeDiagnostics(diagnostics []appconfig.Diagnostic) configDoctorDiagnosticSummary {
	var summary configDoctorDiagnosticSummary
	summary.add(diagnostics)

	return summary
}

func (s *configDoctorDiagnosticSummary) add(diagnostics []appconfig.Diagnostic) {
	if s == nil {
		return
	}

	for _, diagnostic := range diagnostics {
		switch diagnostic.Severity {
		case appconfig.DiagnosticError:
			s.errors++
		case appconfig.DiagnosticWarning:
			s.warnings++
		case appconfig.DiagnosticInfo:
			s.info++
		}
	}
}

func pathStatus(path string) string {
	info, err := os.Stat(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return "will be created on first save"
		}

		return "error: " + err.Error()
	}

	if !info.IsDir() {
		return "not a directory"
	}

	return "ok"
}

func llmConfig(
	cfg appconfig.Config,
	selectedModel string,
	fallbackModels []string,
	sessionID string,
	commandLine []string,
) llm.AutoRegisterConfig {
	providers := make(map[string]llm.ProviderConfig, len(cfg.Providers))
	for name, provider := range cfg.Providers {
		providers[name] = llm.ProviderConfig{
			Disabled:              provider.Disabled,
			AutoStart:             provider.AutoStart,
			DisablePrivateAdapter: provider.DisablePrivateAdapter,
			BaseURL:               provider.BaseURL,
			TimeoutSeconds:        provider.TimeoutSeconds,
		}
	}

	if len(providers) == 0 {
		providers = nil
	}

	return llm.AutoRegisterConfig{
		DefaultProvider: cfg.DefaultProvider,
		DefaultModel:    cfg.DefaultModel,
		ModelAliases:    cloneStringMap(cfg.ModelAliases),
		SelectedModel:   selectedModel,
		FallbackModels:  append([]string(nil), fallbackModels...),
		SessionID:       sessionID,
		CommandLine:     append([]string(nil), commandLine...),
		Providers:       providers,
	}
}

func cloneStringMap(in map[string]string) map[string]string {
	if len(in) == 0 {
		return nil
	}

	out := make(map[string]string, len(in))
	maps.Copy(out, in)

	return out
}

func generationFromConfig(cfg appconfig.Config) generationSettings {
	return generationSettings{
		Temperature:    cfg.Generation.Temperature,
		TopP:           cfg.Generation.TopP,
		Seed:           cfg.Generation.Seed,
		ReasoningLevel: strings.TrimSpace(cfg.Generation.ReasoningLevel),
		MaxTokens:      cfg.Generation.MaxTokens,
	}
}

func generationFromOptions(opts cliOptions) generationSettings {
	var generation generationSettings
	if opts.temperature.set {
		generation.Temperature = &opts.temperature.value
	}

	if opts.topP.set {
		generation.TopP = &opts.topP.value
	}

	if opts.seed.set {
		generation.Seed = &opts.seed.value
	}

	if opts.maxTokens.set {
		generation.MaxTokens = opts.maxTokens.value
	}

	if strings.TrimSpace(opts.reasoningLevel) != "" {
		generation.ReasoningLevel = strings.TrimSpace(opts.reasoningLevel)
	}

	return generation
}

func generationForRequest(
	defaults generationSettings,
	overrides generationSettings,
	activeAgent agentSelection,
) generationSettings {
	generation := defaults
	if activeAgent.ok {
		generation = mergeGenerationSettings(generation, generationSettings{
			Temperature:    activeAgent.agent.Temperature,
			TopP:           activeAgent.agent.TopP,
			Seed:           activeAgent.agent.Seed,
			ReasoningLevel: activeAgent.agent.ReasoningLevel,
			MaxTokens:      activeAgent.agent.MaxTokens,
		})
	}

	return mergeGenerationSettings(generation, overrides)
}

func mergeGenerationSettings(base, override generationSettings) generationSettings {
	if override.Temperature != nil {
		base.Temperature = override.Temperature
	}

	if override.TopP != nil {
		base.TopP = override.TopP
	}

	if override.Seed != nil {
		base.Seed = override.Seed
	}

	if override.ReasoningLevel != "" {
		base.ReasoningLevel = strings.TrimSpace(override.ReasoningLevel)
	}

	if override.MaxTokens > 0 {
		base.MaxTokens = override.MaxTokens
	}

	return base
}

func applyGenerationParams(params *llm.CompleteParams, generation generationSettings) {
	params.Temperature = generation.Temperature
	params.TopP = generation.TopP
	params.Seed = generation.Seed

	params.ReasoningLevel = generation.ReasoningLevel
	if generation.MaxTokens > 0 {
		params.MaxTokens = generation.MaxTokens
	}
}

func mergeTags(existing, next []string) []string {
	out := make([]string, 0, len(existing)+len(next))

	seen := make(map[string]bool, len(existing)+len(next))
	for _, tag := range append(append([]string(nil), existing...), next...) {
		tag = strings.TrimSpace(tag)

		tagKey := strings.ToLower(tag)
		if tag == "" || seen[tagKey] {
			continue
		}

		seen[tagKey] = true

		out = append(out, tag)
	}

	return out
}

func contextOptionsFromConfig(cfg appconfig.Config) contextref.Options {
	opts := contextref.Options{
		MaxFileBytes:    cfg.Context.MaxFileBytes,
		MaxTotalBytes:   cfg.Context.MaxTotalBytes,
		ReferencePolicy: referencePolicyFromConfig(cfg.Context.ReferencePolicy),
	}
	if cwd, err := os.Getwd(); err == nil {
		opts.Root = cwd
	}

	return opts
}

func contextOptionsForProviderModel(opts contextref.Options, providerName, model string) contextref.Options {
	opts.TokenEstimator = contextpack.NewEstimator(providerName, model)

	return opts
}

func contextOptionsForRequestModels(opts contextref.Options, reg *llm.Registry, model string, fallbackModels []string) contextref.Options {
	providerName, estimatorModel := requestManifestModelIdentity(reg, model, fallbackModels)

	return contextOptionsForProviderModel(opts, providerName, estimatorModel)
}

func referencePolicyFromConfig(policy appconfig.ReferencePolicyConfig) contextref.ReferencePolicy {
	return contextref.ReferencePolicy{
		AllowedSchemes:       append([]string(nil), policy.AllowedSchemes...),
		AllowedHosts:         append([]string(nil), policy.AllowedHosts...),
		LocalRoots:           append([]string(nil), policy.LocalRoots...),
		MaxRedirects:         policy.MaxRedirects,
		ContentTypes:         append([]string(nil), policy.ContentTypes...),
		AllowPrivateNetworks: policy.AllowPrivateNetworks,
	}
}

// loadConfiguredReferences resolves the configured reference paths/URLs at
// startup and returns a pre-rendered reference block that can be injected into
// every LLM request as additional context. Errors are logged and fail closed for
// the configured-reference block so rejected entries do not silently leave a
// partial context behind.
func loadConfiguredReferences(ctx context.Context, refs []string, opts contextref.Options) string {
	return loadConfiguredReferenceContext(ctx, refs, opts).Content
}

//nolint:govet // Field order keeps manifest before rendered content in reports.
type configuredReferenceContext struct {
	Manifest  contextref.ReferenceManifest
	Content   string
	Estimator string
}

func loadConfiguredReferenceContext(ctx context.Context, refs []string, opts contextref.Options) configuredReferenceContext {
	if opts.ReferenceScope == "" {
		opts.ReferenceScope = contextref.ReferenceScopeGlobal
	}

	return loadConfiguredReferenceContextForScope(ctx, refs, opts)
}

func loadConfiguredReferenceContextForScope(ctx context.Context, refs []string, opts contextref.Options) configuredReferenceContext {
	estimatorSummary := estimatorSummaryForContextOptions(opts)
	if len(refs) == 0 {
		return configuredReferenceContext{Estimator: estimatorSummary}
	}

	loaded, referenceEvents, err := contextref.LoadReferencesWithReport(ctx, refs, opts)
	manifest := withReferenceManifestEstimator(contextref.BuildReferenceManifest(referenceEvents), estimatorSummary)

	for i := range referenceEvents {
		fmt.Fprintln(os.Stderr, formatReferenceEvent(referenceEvents[i]))
	}

	if err != nil {
		omittedEvents := omitLoadedConfiguredReferenceEvents(referenceEvents, "configured reference block omitted because loading failed")
		for i := range omittedEvents {
			if omittedEvents[i].PolicyDecision == contextref.ReferenceDecisionOmitted {
				fmt.Fprintln(os.Stderr, formatReferenceEvent(omittedEvents[i]))
			}
		}

		manifest = withReferenceManifestEstimator(contextref.BuildReferenceManifest(omittedEvents), estimatorSummary)
		if len(referenceEvents) > 0 {
			fmt.Fprintln(os.Stderr, formatReferenceManifest(manifest))
		}

		fmt.Fprintf(os.Stderr, "warning: loading configured references failed; omitting configured reference context: %v\n", err)

		return configuredReferenceContext{Manifest: manifest, Estimator: estimatorSummary}
	}

	if len(referenceEvents) > 0 {
		fmt.Fprintln(os.Stderr, formatReferenceManifest(manifest))
	}

	return configuredReferenceContext{
		Content:   contextref.FormatReferences(loaded),
		Manifest:  manifest,
		Estimator: estimatorSummary,
	}
}

func withReferenceManifestEstimator(manifest contextref.ReferenceManifest, estimatorSummary string) contextref.ReferenceManifest {
	if manifest.TokenEstimator == "" {
		manifest.TokenEstimator = estimatorSummary
	}

	return manifest
}

func configuredReferenceContextForRequest(ctx context.Context, refs []string, current configuredReferenceContext, opts contextref.Options) configuredReferenceContext {
	if len(refs) == 0 {
		return current
	}

	if current.Estimator == estimatorSummaryForContextOptions(opts) {
		return current
	}

	return loadConfiguredReferenceContext(ctx, refs, opts)
}

func estimatorSummaryForContextOptions(opts contextref.Options) string {
	estimator := opts.TokenEstimator
	if estimator == nil {
		estimator = contextpack.DefaultEstimator()
	}

	return contextEstimatorSummary(estimator.Profile())
}

func omitLoadedConfiguredReferenceEvents(referenceEvents []contextref.ReferenceEvent, reason string) []contextref.ReferenceEvent {
	omittedEvents := append([]contextref.ReferenceEvent(nil), referenceEvents...)
	for i := range omittedEvents {
		switch omittedEvents[i].PolicyDecision {
		case contextref.ReferenceDecisionLoaded, contextref.ReferenceDecisionTruncated:
			omittedEvents[i].PolicyDecision = contextref.ReferenceDecisionOmitted
			omittedEvents[i].PolicyReason = reason
			omittedEvents[i].PolicyReasonCode = contextref.ReferenceReasonCode(contextref.ReferenceDecisionOmitted, reason)
		}
	}

	return omittedEvents
}

func omitIncludedReferenceManifestEntries(manifest contextref.ReferenceManifest, reason string) contextref.ReferenceManifest {
	return withReferenceManifestEstimator(
		contextref.BuildReferenceManifest(omitLoadedConfiguredReferenceEvents(manifest.Entries, reason)),
		manifest.TokenEstimator,
	)
}

func formatReferenceManifest(manifest contextref.ReferenceManifest) string {
	manifest = sanitizeReferenceManifestForAudit(manifest)

	data, err := json.Marshal(manifest)
	if err != nil {
		return fmt.Sprintf("reference manifest {\"error\":%q}", err.Error())
	}

	return "reference manifest " + string(data)
}

func formatReferenceEvent(event contextref.ReferenceEvent) string {
	event = sanitizeReferenceEventForDisplay(event)

	parts := []string{"reference", event.PolicyDecision}

	if event.Scope != "" {
		parts = append(parts, "scope="+event.Scope)
	}

	if event.Kind != "" {
		parts = append(parts, "kind="+event.Kind)
	}

	if event.Location != "" {
		parts = append(parts, "location="+event.Location)
	}

	if event.Source != "" {
		parts = append(parts, "source="+strconv.Quote(event.Source))
	}

	if event.ResolvedSource != "" {
		parts = append(parts, "resolved_source="+strconv.Quote(event.ResolvedSource))
	}

	if event.Bytes > 0 || event.PolicyDecision == contextref.ReferenceDecisionLoaded || event.PolicyDecision == contextref.ReferenceDecisionTruncated {
		parts = append(parts, fmt.Sprintf("bytes=%d", event.Bytes))
	}

	if event.TokenEstimate.Tokens > 0 || event.TokenEstimate.UpperBoundTokens > 0 {
		parts = append(parts,
			fmt.Sprintf("tokens=%d", event.TokenEstimate.Tokens),
			fmt.Sprintf("token_upper=%d", event.TokenEstimate.UpperBoundTokens),
		)
	}

	if event.TokenEstimator != "" {
		parts = append(parts, "token_estimator="+strconv.Quote(event.TokenEstimator))
	}

	if event.Truncated {
		parts = append(parts, "truncated=true")
	}

	if event.DigestSHA256 != "" {
		parts = append(parts, "sha256="+event.DigestSHA256)
	}

	if !event.FetchedAt.IsZero() {
		parts = append(parts, "fetched_at="+event.FetchedAt.UTC().Format(time.RFC3339))
	}

	parts = appendReferenceReasonFields(parts, event)

	return strings.Join(parts, " ")
}

func sanitizeReferenceEventForDisplay(event contextref.ReferenceEvent) contextref.ReferenceEvent {
	manifest := contextref.BuildReferenceManifest([]contextref.ReferenceEvent{event})
	if len(manifest.Entries) == 0 {
		return event
	}

	return manifest.Entries[0]
}

func appendReferenceReasonFields(parts []string, event contextref.ReferenceEvent) []string {
	if event.PolicyReason != "" {
		parts = append(parts, "reason="+strconv.Quote(event.PolicyReason))
	}

	if event.PolicyReasonCode != "" {
		parts = append(parts, "reason_code="+strconv.Quote(event.PolicyReasonCode))
	}

	return parts
}

func buildReferenceContextWithManifest(ctx context.Context, globalRefCtx configuredReferenceContext, activeAgent agentSelection, opts contextref.Options) configuredReferenceContext {
	if !activeAgent.ok || len(activeAgent.agent.References) == 0 {
		return globalRefCtx
	}

	agentOpts := opts
	agentOpts.ReferenceScope = contextref.ReferenceScopeAgent

	if activeAgent.name != "" {
		agentOpts.ReferenceScope += ":" + activeAgent.name
	}

	agentRefCtx := loadConfiguredReferenceContextForScope(ctx, activeAgent.agent.References, agentOpts)
	mergedManifest := mergeReferenceManifests(globalRefCtx.Manifest, agentRefCtx.Manifest)
	estimatorSummary := estimatorSummaryForContextOptions(opts)

	if agentRefCtx.Content == "" {
		globalRefCtx.Manifest = mergedManifest
		if globalRefCtx.Estimator == "" {
			globalRefCtx.Estimator = estimatorSummary
		}

		return globalRefCtx
	}

	if globalRefCtx.Content == "" {
		return configuredReferenceContext{
			Content:   agentRefCtx.Content,
			Manifest:  mergedManifest,
			Estimator: estimatorSummary,
		}
	}

	return configuredReferenceContext{
		Content:   globalRefCtx.Content + "\n\n" + agentRefCtx.Content,
		Manifest:  mergedManifest,
		Estimator: estimatorSummary,
	}
}

func maxInputTokensFromConfigOptions(cfg appconfig.Config, opts cliOptions) int {
	if opts.maxInputTokens.set {
		return opts.maxInputTokens.value
	}

	return cfg.Context.MaxInputTokens
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if value != "" {
			return value
		}
	}

	return ""
}

// ---------------------------------------------------------------------------
// Worktree commands
// ---------------------------------------------------------------------------

// finalizeWorktree auto-merges the session worktree when enabled, or prints
// a reminder for manual merge.
func finalizeWorktree(ctx context.Context, state *appState) {
	if state.worktreeInfo == nil {
		return
	}

	if !state.autoMergeWorktree {
		fmt.Fprintln(os.Stderr, "worktree: session files are in "+state.worktreeInfo.Path)
		fmt.Fprintln(os.Stderr, "worktree: merge with: atteler --merge-worktree "+state.sessionState.ID)

		return
	}

	fmt.Fprintln(os.Stderr, "worktree: merging "+state.worktreeInfo.Branch+" into "+state.worktreeInfo.BaseBranch+"...")

	if err := worktree.MergeWithOptionsContext(ctx, state.cwd, state.worktreeInfo, worktree.MergeOptions{
		AutoMerge:  true,
		Strategy:   worktree.MergeStrategyMerge,
		Provenance: worktreeMergeProvenance(state.sessionState),
	}); err != nil {
		fmt.Fprintln(os.Stderr, "worktree: auto-merge failed: "+err.Error())
		fmt.Fprintln(os.Stderr, "worktree: files preserved in "+state.worktreeInfo.Path)
		fmt.Fprintln(os.Stderr, "worktree: retry with: atteler --merge-worktree "+state.sessionState.ID)

		return
	}

	state.sessionState.WorktreePath = ""
	state.sessionState.WorktreeBranch = ""

	state.sessionState.WorktreeBase = ""
	if saveErr := state.sessionStore.Save(state.sessionState); saveErr != nil {
		fmt.Fprintln(os.Stderr, "warning: could not update session after merge: "+saveErr.Error())
	}

	fmt.Fprintln(os.Stderr, "worktree: merged and cleaned up")
}
