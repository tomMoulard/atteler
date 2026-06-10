package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
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
	"github.com/tommoulard/atteler/pkg/modelroute"
	"github.com/tommoulard/atteler/pkg/session"
	"github.com/tommoulard/atteler/pkg/worktree"
)

type agentDescription struct {
	Temperature    *float64 `yaml:"temperature,omitempty"`
	TopP           *float64 `yaml:"top_p,omitempty"`
	Seed           *int     `yaml:"seed,omitempty"`
	Name           string   `yaml:"name"`
	ModelMode      string   `yaml:"model_mode,omitempty"`
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

type doctorDiagnosticLevel struct {
	Severity string `json:"severity"`
	Meaning  string `json:"meaning"`
}

type doctorDiagnostic struct {
	Severity    string `json:"severity"`
	Message     string `json:"message"`
	Path        string `json:"path,omitempty"`
	Source      string `json:"source,omitempty"`
	Importer    string `json:"importer,omitempty"`
	Field       string `json:"field,omitempty"`
	Replacement string `json:"replacement,omitempty"`
}

type doctorDiagnosticCounts struct {
	Fatal    int `json:"fatal,omitempty"`
	Errors   int `json:"errors"`
	Warnings int `json:"warnings"`
	Info     int `json:"info"`
}

//nolint:govet // field order follows the user-facing JSON report grouping.
type doctorOfflineJSONReport struct {
	SchemaVersion     int                          `json:"schema_version"`
	Command           string                       `json:"command"`
	Status            string                       `json:"status"`
	DiagnosticLevels  []doctorDiagnosticLevel      `json:"diagnostic_levels"`
	Config            doctorOfflineConfigReport    `json:"config"`
	ConfigDiagnostics doctorDiagnosticCounts       `json:"config_diagnostics"`
	State             doctorOfflineStateReport     `json:"state"`
	StateDiagnostics  doctorDiagnosticCounts       `json:"state_diagnostics"`
	Sessions          doctorOfflinePathReport      `json:"sessions"`
	KnownProviders    []string                     `json:"known_providers"`
	Agents            []string                     `json:"agents"`
	HookEvents        int                          `json:"hook_events"`
	Plugins           []string                     `json:"plugins"`
	Diagnostics       []doctorDiagnostic           `json:"diagnostics,omitempty"`
	Sources           []appconfig.SourceDiagnostic `json:"sources,omitempty"`
}

//nolint:govet // field order follows the user-facing JSON report grouping.
type doctorOfflineConfigReport struct {
	Loaded     []string `json:"loaded,omitempty"`
	LoadError  string   `json:"load_error,omitempty"`
	FatalError string   `json:"fatal_error,omitempty"`
	Status     string   `json:"status"`
}

//nolint:govet // field order follows the user-facing JSON report grouping.
type doctorOfflineStateReport struct {
	Path     string `json:"path"`
	Status   string `json:"status"`
	Version  int    `json:"version,omitempty"`
	Revision int64  `json:"revision,omitempty"`
	Error    string `json:"error,omitempty"`
}

type doctorOfflinePathReport struct {
	Path   string `json:"path"`
	Status string `json:"status"`
}

const (
	doctorCommandNameOffline = "doctor-offline"
	doctorStatusFailed       = "failed"
	doctorStatusOK           = "ok"
	doctorSeverityFatal      = "fatal"
)

var configDoctorDiagnosticLevels = []doctorDiagnosticLevel{
	{
		Severity: doctorSeverityFatal,
		Meaning:  "selected Atteler config failed strict read, parse, schema, or migration checks; command exits non-zero",
	},
	{
		Severity: string(appconfig.DiagnosticWarning),
		Meaning:  "non-fatal best-effort diagnostics such as harness-import fallbacks or deprecated fields",
	},
	{
		Severity: string(appconfig.DiagnosticInfo),
		Meaning:  "informational context such as implicit defaults, schema notes, or missing optional files",
	},
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
		ModelMode:      activeAgent.ModelMode,
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

	if state.configLoadErr != nil {
		printConfigDoctorDiagnosticLevels(os.Stdout)
		fmt.Println("config_status: " + doctorStatusFailed)
		printFatalConfigDiagnostic(os.Stderr, state.configLoadErr)
		fmt.Println("doctor_status: " + doctorStatusFailed)

		return fmt.Errorf("config doctor: fatal config error: %w", state.configLoadErr)
	}

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

	printProviderCompatibilityMatrixSummary()

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
	case providerNameCodex:
		return &llm.CodexProvider{}
	case providerNameClaudeCode:
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

	fmt.Printf("         retry: %s\n", formatRetryPolicy(provider.RetryPolicy))

	for _, model := range models {
		fmt.Printf("         - %s\n", model)
	}
}

func formatRetryPolicy(policy llm.RetryPolicyInfo) string {
	return fmt.Sprintf(
		"max_retries=%d initial=%s max_backoff=%s budget=%s jitter=%.0f%%",
		policy.MaxAttempts,
		policy.InitialBackoff,
		policy.MaxBackoff,
		policy.MaxElapsedTime,
		policy.JitterFraction*100,
	)
}

func doctorOffline(opts cliOptions) error {
	cfg, loadedConfigPaths, _, diagnostics, loadErr := appconfig.LoadWithDiagnostics()
	sources := appconfig.InspectPathSources(appconfig.DefaultPathSources())
	fatalErr := configDoctorFatalError(loadErr, sources)

	outputFormat, err := structuredCommandOutputFormat(opts.jsonOutput, opts.outputFormat)
	if err != nil {
		return err
	}

	if outputFormat == outputFormatJSON {
		return printDoctorOfflineJSON(opts, cfg, loadedConfigPaths, diagnostics, sources, loadErr, fatalErr)
	}

	fmt.Println("Atteler offline doctor")
	printConfigDoctorDiagnosticLevels(os.Stdout)

	if fatalErr != nil {
		fmt.Println("config_status: " + doctorStatusFailed)
		printFatalConfigDiagnostic(os.Stderr, fatalErr)
	} else {
		fmt.Println("config_status: " + doctorStatusOK)
	}

	printDiagnostics(os.Stdout, diagnostics)

	switch {
	case len(loadedConfigPaths) == 0 && fatalErr != nil:
		fmt.Println("config: no config files loaded successfully")
	case len(loadedConfigPaths) == 0:
		fmt.Println("config: no config files loaded")
	default:
		fmt.Println("config: " + strings.Join(loadedConfigPaths, ", "))
	}

	printConfigStateDoctorSummaryWithDiagnostics(fatalErr, diagnostics)

	store := session.NewStore(opts.sessionDir)
	fmt.Println("sessions: " + store.Dir() + " (" + pathStatus(store.Dir()) + ")")

	providerNames := knownProviderNames()
	if len(providerNames) == 0 {
		fmt.Println("known_providers: none")
	} else {
		fmt.Println("known_providers: " + strings.Join(providerNames, ", "))
	}

	printDoctorOfflineProviderRetries(os.Stdout, cfg, providerNames)

	printProviderCompatibilityMatrixSummary()

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

	if fatalErr != nil {
		fmt.Println("doctor_status: " + doctorStatusFailed)

		return fmt.Errorf("config doctor-offline: fatal config error: %w", fatalErr)
	}

	fmt.Println("doctor_status: " + doctorStatusOK)

	return nil
}

func printDoctorOfflineJSON(
	opts cliOptions,
	cfg appconfig.Config,
	loadedConfigPaths []string,
	diagnostics []appconfig.Diagnostic,
	sources []appconfig.SourceDiagnostic,
	loadErr error,
	fatalErr error,
) error {
	report := newDoctorOfflineJSONReport(opts, cfg, loadedConfigPaths, diagnostics, sources, loadErr, fatalErr)
	if err := json.NewEncoder(os.Stdout).Encode(report); err != nil {
		return fmt.Errorf("config doctor-offline: encode JSON: %w", err)
	}

	if fatalErr != nil {
		return fmt.Errorf("config doctor-offline: fatal config error: %w", fatalErr)
	}

	return nil
}

func newDoctorOfflineJSONReport(
	opts cliOptions,
	cfg appconfig.Config,
	loadedConfigPaths []string,
	diagnostics []appconfig.Diagnostic,
	sources []appconfig.SourceDiagnostic,
	loadErr error,
	fatalErr error,
) doctorOfflineJSONReport {
	status := doctorStatusOK
	configStatus := doctorStatusOK

	if fatalErr != nil {
		status = doctorStatusFailed
		configStatus = doctorStatusFailed
	}

	state := appconfig.InspectStatePath(appconfig.DefaultStatePath())
	store := session.NewStore(opts.sessionDir)
	providerNames := knownProviderNames()

	agents := append([]string{}, agent.NewRegistry(cfg.Agents).List()...)
	plugins := append([]string{}, cfg.Plugins.Paths...)

	configSummary := summarizeSourceDiagnostics(sources)
	configSummary.add(diagnostics)
	configSummary.addFatal(fatalErr)

	stateSummary := summarizeDiagnostics(state.Diagnostics)

	reportDiagnostics := make([]doctorDiagnostic, 0, len(diagnostics)+1)
	if loadErr != nil {
		reportDiagnostics = append(reportDiagnostics, fatalDoctorDiagnostic(loadErr, sources))
	} else {
		reportDiagnostics = append(reportDiagnostics, fatalConfigSourceDiagnostics(sources)...)
	}

	for _, diagnostic := range diagnostics {
		reportDiagnostics = append(reportDiagnostics, doctorDiagnosticFromConfigDiagnostic(diagnostic))
	}

	return doctorOfflineJSONReport{
		SchemaVersion:    1,
		Command:          doctorCommandNameOffline,
		Status:           status,
		DiagnosticLevels: append([]doctorDiagnosticLevel(nil), configDoctorDiagnosticLevels...),
		Config: doctorOfflineConfigReport{
			Status:     configStatus,
			Loaded:     append([]string(nil), loadedConfigPaths...),
			LoadError:  loadErrorString(loadErr),
			FatalError: loadErrorString(fatalErr),
		},
		ConfigDiagnostics: doctorDiagnosticCountsFromSummary(configSummary),
		State: doctorOfflineStateReport{
			Path:     state.Path,
			Status:   state.Status,
			Version:  state.Version,
			Revision: state.Revision,
			Error:    state.Error,
		},
		StateDiagnostics: doctorDiagnosticCountsFromSummary(stateSummary),
		Sessions: doctorOfflinePathReport{
			Path:   store.Dir(),
			Status: pathStatus(store.Dir()),
		},
		KnownProviders: providerNames,
		Agents:         agents,
		HookEvents:     len(events.SupportedEventTypes()),
		Plugins:        plugins,
		Diagnostics:    reportDiagnostics,
		Sources:        sources,
	}
}

func printConfigDoctorDiagnosticLevels(w io.Writer) {
	fmt.Fprintln(w, "diagnostic_levels:")

	for _, level := range configDoctorDiagnosticLevels {
		fmt.Fprintf(w, "  - %s: %s\n", level.Severity, level.Meaning)
	}
}

func printFatalConfigDiagnostic(w io.Writer, err error) {
	if err == nil {
		return
	}

	fmt.Fprintln(w, "fatal:")

	lines := strings.Split(err.Error(), "\n")
	for i, line := range lines {
		if i == 0 {
			fmt.Fprintf(w, "  - %s\n", line)

			continue
		}

		fmt.Fprintf(w, "    %s\n", line)
	}
}

func knownProviderNames() []string {
	providers := llm.KnownProviders()
	providerNames := make([]string, 0, len(providers))

	for _, provider := range providers {
		providerNames = append(providerNames, provider.Name)
	}

	sort.Strings(providerNames)

	return providerNames
}

func printDoctorOfflineProviderRetries(w io.Writer, cfg appconfig.Config, providerNames []string) {
	fmt.Fprintln(w, "provider_retries:")

	for _, name := range doctorOfflineProviderRetryNames(cfg, providerNames) {
		fmt.Fprintf(w, "  %s: %s\n", name, formatRetryPolicy(retryPolicyInfoForConfig(name, cfg.Providers[name])))
	}
}

func doctorOfflineProviderRetryNames(cfg appconfig.Config, providerNames []string) []string {
	retryProviderSet := make(map[string]bool, len(providerNames))

	for _, name := range providerNames {
		retryProviderSet[name] = true
	}

	retryProviderNames := append([]string(nil), providerNames...)

	for name := range cfg.Providers {
		if !retryProviderSet[name] {
			retryProviderNames = append(retryProviderNames, name)
		}
	}

	sort.Strings(retryProviderNames)

	return retryProviderNames
}

func configDoctorFatalError(loadErr error, sources []appconfig.SourceDiagnostic) error {
	if loadErr != nil {
		return loadErr
	}

	fatalDiagnostics := fatalConfigSourceDiagnostics(sources)
	if len(fatalDiagnostics) == 0 {
		return nil
	}

	messages := make([]string, 0, len(fatalDiagnostics))
	for _, diagnostic := range fatalDiagnostics {
		messages = append(messages, diagnostic.Message)
	}

	if len(messages) == 1 {
		return errors.New(messages[0])
	}

	return fmt.Errorf("%d fatal config diagnostics: %s", len(messages), strings.Join(messages, "; "))
}

func fatalConfigSourceDiagnostics(sources []appconfig.SourceDiagnostic) []doctorDiagnostic {
	var out []doctorDiagnostic

	for _, source := range sources {
		for _, diagnostic := range source.Diagnostics {
			if diagnostic.Severity != appconfig.DiagnosticError {
				continue
			}

			fatalDiagnostic := doctorDiagnosticFromConfigDiagnostic(diagnostic)
			fatalDiagnostic.Severity = doctorSeverityFatal

			if fatalDiagnostic.Path == "" {
				fatalDiagnostic.Path = source.Path
			}

			out = append(out, fatalDiagnostic)
		}
	}

	return out
}

func fatalDoctorDiagnostic(loadErr error, sources []appconfig.SourceDiagnostic) doctorDiagnostic {
	diagnostic := doctorDiagnostic{
		Severity: doctorSeverityFatal,
		Message:  loadErr.Error(),
	}

	for _, source := range sources {
		if source.Status == "error" {
			diagnostic.Path = source.Path

			return diagnostic
		}

		for _, sourceDiagnostic := range source.Diagnostics {
			if sourceDiagnostic.Severity == appconfig.DiagnosticError {
				diagnostic.Path = source.Path

				return diagnostic
			}
		}
	}

	for _, source := range sources {
		if source.Path != "" && strings.Contains(loadErr.Error(), source.Path) {
			diagnostic.Path = source.Path

			return diagnostic
		}
	}

	return diagnostic
}

func doctorDiagnosticFromConfigDiagnostic(diagnostic appconfig.Diagnostic) doctorDiagnostic {
	severity := diagnostic.Severity
	if severity == "" {
		severity = appconfig.DiagnosticWarning
	}

	return doctorDiagnostic{
		Severity:    string(severity),
		Message:     diagnostic.String(),
		Path:        diagnostic.Path,
		Source:      diagnostic.Source,
		Importer:    diagnostic.Importer,
		Field:       diagnostic.Field,
		Replacement: diagnostic.Replacement,
	}
}

func doctorDiagnosticCountsFromSummary(summary configDoctorDiagnosticSummary) doctorDiagnosticCounts {
	return doctorDiagnosticCounts{
		Fatal:    summary.fatal,
		Errors:   summary.errors,
		Warnings: summary.warnings,
		Info:     summary.info,
	}
}

func loadErrorString(err error) string {
	if err == nil {
		return ""
	}

	return err.Error()
}

func printProviderCompatibilityMatrixSummary() {
	matrix := llm.ProviderCompatibilityMatrix()
	if len(matrix) == 0 {
		fmt.Println("compatibility_matrix: none")

		return
	}

	fmt.Println("compatibility_matrix:")

	for i := range matrix {
		row := &matrix[i]
		fmt.Printf("  - %s: %s\n", row.Provider, llm.ProviderCompatibilityStatusSummary(row))
	}
}

type configDoctorDiagnosticSummary struct {
	fatal    int
	errors   int
	warnings int
	info     int
}

func printConfigStateDoctorSummary() {
	printConfigStateDoctorSummaryWithFatal(nil)
}

func printConfigStateDoctorSummaryWithFatal(fatalErr error) {
	printConfigStateDoctorSummaryWithDiagnostics(fatalErr, nil)
}

func printConfigStateDoctorSummaryWithDiagnostics(fatalErr error, diagnostics []appconfig.Diagnostic) {
	fmt.Printf(
		"schema: config=%d state=%d\n",
		appconfig.ConfigSchemaVersion,
		appconfig.StateSchemaVersion,
	)

	configSummary := summarizeSourceDiagnostics(appconfig.InspectPathSources(appconfig.DefaultPathSources()))
	configSummary.add(diagnostics)
	configSummary.addFatal(fatalErr)
	fmt.Println("config_diagnostics: " + formatConfigDoctorDiagnosticSummary(configSummary))

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

func formatConfigDoctorDiagnosticSummary(summary configDoctorDiagnosticSummary) string {
	out := fmt.Sprintf(
		"errors=%d warnings=%d info=%d",
		summary.errors,
		summary.warnings,
		summary.info,
	)
	if summary.fatal > 0 {
		out += fmt.Sprintf(" fatal=%d", summary.fatal)
	}

	return out
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

func (s *configDoctorDiagnosticSummary) addFatal(err error) {
	if s == nil || err == nil {
		return
	}

	s.fatal++
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
	for name := range cfg.Providers {
		provider := cfg.Providers[name]

		providers[name] = llm.ProviderConfig{
			Type:                  provider.Type,
			APIKeyEnv:             provider.APIKeyEnv,
			APIKeyHeader:          provider.APIKeyHeader,
			APIKeyScheme:          provider.APIKeyScheme,
			ChatCompletionsPath:   provider.ChatCompletionsPath,
			EmbeddingsPath:        provider.EmbeddingsPath,
			ModelsPath:            provider.ModelsPath,
			APIVersion:            provider.APIVersion,
			Models:                append([]string(nil), provider.Models...),
			Capabilities:          append([]string(nil), provider.Capabilities...),
			Disabled:              provider.Disabled,
			Local:                 provider.Local,
			AutoStart:             provider.AutoStart,
			DisablePrivateAdapter: provider.DisablePrivateAdapter,
			BaseURL:               provider.BaseURL,
			Retry:                 llmRetryPolicyConfig(provider.Retry),
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
		ModelRoles:      llmModelRolesFromConfig(cfg.ModelRoles),
		SelectedModel:   selectedModel,
		FallbackModels:  append([]string(nil), fallbackModels...),
		SessionID:       sessionID,
		CommandLine:     append([]string(nil), commandLine...),
		Providers:       providers,
	}
}

func llmModelRolesFromConfig(roles map[string]appconfig.ModelRoleConfig) map[string]llm.ModelRole {
	if len(roles) == 0 {
		return nil
	}

	out := make(map[string]llm.ModelRole, len(roles))
	for name := range roles {
		role := roles[name]
		out[name] = llm.ModelRole{
			Preferred:            role.Preferred,
			FallbackModels:       append([]string(nil), role.FallbackModels...),
			RoutingPolicy:        routingPolicyFromConfig(role.RoutingPolicy),
			PreferredProviders:   append([]string(nil), role.PreferredProviders...),
			BannedProviders:      append([]string(nil), role.BannedProviders...),
			BannedModels:         append([]string(nil), role.BannedModels...),
			RequiredCapabilities: append([]string(nil), role.RequiredCapabilities...),
			MaxCostUSD:           role.MaxCostUSD,
			MaxLatencyMS:         role.MaxLatencyMS,
			MaxTTFTMS:            role.MaxTTFTMS,
			RequireFreshMetadata: role.RequireFreshMetadata,
			PreferLocal:          role.PreferLocal,
		}
	}

	return out
}

func routingPolicyFromConfig(policy appconfig.RoutingPolicyConfig) modelroute.Policy {
	return modelroute.Policy{
		PreferredProviders:   append([]string(nil), policy.PreferredProviders...),
		BannedProviders:      append([]string(nil), policy.BannedProviders...),
		BannedModels:         append([]string(nil), policy.BannedModels...),
		RequiredCapabilities: append([]string(nil), policy.RequiredCapabilities...),
		MaxBudget:            policy.MaxBudget,
		MaxLatencyMS:         policy.MaxLatencyMS,
		MaxTTFTMS:            policy.MaxTTFTMS,
		RequireFreshMetadata: policy.RequireFreshMetadata,
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

func llmRetryPolicyConfig(cfg appconfig.RetryConfig) llm.RetryPolicyConfig {
	return llm.RetryPolicyConfig{
		MaxAttempts:      cfg.MaxAttempts,
		InitialBackoffMS: cfg.InitialBackoffMS,
		MaxBackoffMS:     cfg.MaxBackoffMS,
		MaxElapsedMS:     cfg.MaxElapsedMS,
		JitterFraction:   cfg.JitterFraction,
	}
}

func retryPolicyInfoForConfig(providerName string, provider appconfig.ProviderConfig) llm.RetryPolicyInfo {
	r := llm.NewRegistry()
	policy := retryPolicyFromInfo(r.RetryPolicyForProvider(providerName))
	retry := provider.Retry

	if retry.MaxAttempts != nil {
		policy.MaxAttempts = *retry.MaxAttempts
	}

	if retry.InitialBackoffMS != nil {
		policy.InitialBackoff = time.Duration(*retry.InitialBackoffMS) * time.Millisecond
	}

	if retry.MaxBackoffMS != nil {
		policy.MaxBackoff = time.Duration(*retry.MaxBackoffMS) * time.Millisecond
	}

	if retry.MaxElapsedMS != nil {
		policy.MaxElapsedTime = time.Duration(*retry.MaxElapsedMS) * time.Millisecond
	}

	if retry.JitterFraction != nil {
		policy.JitterFraction = *retry.JitterFraction
	}

	r.SetProviderRetry(providerName, policy)

	return r.RetryPolicyForProvider(providerName)
}

func retryPolicyFromInfo(info llm.RetryPolicyInfo) llm.RetryPolicy {
	return llm.RetryPolicy{
		MaxAttempts:    info.MaxAttempts,
		InitialBackoff: info.InitialBackoff,
		MaxBackoff:     info.MaxBackoff,
		MaxElapsedTime: info.MaxElapsedTime,
		JitterFraction: info.JitterFraction,
	}
}

func generationFromConfig(cfg appconfig.Config) generationSettings {
	return generationSettings{
		Temperature:    cfg.Generation.Temperature,
		TopP:           cfg.Generation.TopP,
		Seed:           cfg.Generation.Seed,
		ModelMode:      strings.TrimSpace(cfg.Generation.ModelMode),
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

	if strings.TrimSpace(opts.modelMode) != "" {
		generation.ModelMode = strings.TrimSpace(opts.modelMode)
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
			ModelMode:      activeAgent.agent.ModelMode,
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

	if override.ModelMode != "" {
		base.ModelMode = strings.TrimSpace(override.ModelMode)
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

	params.ModelMode = generation.ModelMode
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
		AllowedSchemes:       cloneSlicePreserveEmpty(policy.AllowedSchemes),
		DeniedSchemes:        cloneSlicePreserveEmpty(policy.DeniedSchemes),
		AllowedHosts:         cloneSlicePreserveEmpty(policy.AllowedHosts),
		DeniedHosts:          cloneSlicePreserveEmpty(policy.DeniedHosts),
		AllowedPorts:         cloneSlicePreserveEmpty(policy.AllowedPorts),
		DeniedPorts:          cloneSlicePreserveEmpty(policy.DeniedPorts),
		LocalRoots:           cloneSlicePreserveEmpty(policy.LocalRoots),
		DeniedLocalRoots:     cloneSlicePreserveEmpty(policy.DeniedLocalRoots),
		AllowedGlobs:         cloneSlicePreserveEmpty(policy.AllowedGlobs),
		DeniedGlobs:          cloneSlicePreserveEmpty(policy.DeniedGlobs),
		MaxRedirects:         policy.MaxRedirects,
		MaxFiles:             policy.MaxFiles,
		ContentTypes:         cloneSlicePreserveEmpty(policy.ContentTypes),
		AllowAbsolutePaths:   policy.AllowAbsolutePaths,
		AllowPrivateNetworks: policy.AllowPrivateNetworks,
	}
}

func cloneSlicePreserveEmpty[T any](in []T) []T {
	if in == nil {
		return nil
	}

	out := make([]T, len(in))
	copy(out, in)

	return out
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
		return configuredReferenceContext{
			Manifest:  withReferenceManifestEstimator(contextref.BuildReferenceManifest(nil), estimatorSummary),
			Estimator: estimatorSummary,
		}
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

		if manifest.RejectedCount > 0 {
			fmt.Fprintf(os.Stderr, "warning: loading configured references rejected %d reference(s); omitting configured reference context\n", manifest.RejectedCount)
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
		manifest.TokenEstimator = sanitizeContextManifestText(estimatorSummary)
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

// finalizeWorktree preserves the session worktree by default, or runs a
// reviewed auto-merge transaction when explicitly configured.
func finalizeWorktree(ctx context.Context, state *appState) error {
	if state.worktreeInfo == nil {
		return nil
	}

	if !state.autoMergeWorktree {
		fmt.Fprintln(os.Stderr, "worktree: session files are in "+state.worktreeInfo.Path)
		fmt.Fprintln(os.Stderr, "worktree: merge with: atteler --merge-worktree "+state.sessionState.ID)

		return nil
	}

	fmt.Fprintln(os.Stderr, "worktree: merging "+state.worktreeInfo.Branch+" into "+state.worktreeInfo.BaseBranch+"...")

	result, err := worktree.MergeWithResultContext(ctx, state.cwd, state.worktreeInfo, worktree.MergeOptions{
		AutoCommit:           true,
		ReviewedAutoCommit:   true,
		AutoMerge:            true,
		OverrideVerification: state.worktreeMergeOverride,
		Strategy:             worktree.MergeStrategyMerge,
		VerificationCommands: state.worktreeVerificationCommands,
		Provenance:           worktreeMergeProvenance(state.sessionState),
	})
	if err != nil {
		fmt.Fprintln(os.Stderr, "worktree: auto-merge failed: "+err.Error())
		fmt.Fprintln(os.Stderr, "worktree: files preserved in "+state.worktreeInfo.Path)
		fmt.Fprintln(os.Stderr, "worktree: retry with: atteler --merge-worktree "+state.sessionState.ID)

		return fmt.Errorf("worktree auto-merge failed: %w", err)
	}

	if saveErr := clearWorktreeMetadataFromLatestSession(state); saveErr != nil {
		fmt.Fprintln(os.Stderr, "warning: could not update session after merge: "+saveErr.Error())
	}

	printWorktreeMergeResult(os.Stderr, result)
	fmt.Fprintln(os.Stderr, "worktree: merged and cleaned up")

	return nil
}

func clearWorktreeMetadataFromLatestSession(state *appState) error {
	if state == nil {
		return nil
	}

	latest := state.sessionState
	if state.sessionStore != nil && latest.ID != "" {
		loaded, err := state.sessionStore.Load(latest.ID)
		if err != nil {
			return fmt.Errorf("reload session: %w", err)
		}

		latest = loaded
	}

	latest.WorktreePath = ""
	latest.WorktreeBranch = ""
	latest.WorktreeBase = ""
	state.sessionState = latest

	if state.sessionStore == nil || latest.ID == "" {
		return nil
	}

	if err := state.sessionStore.Save(latest); err != nil {
		return fmt.Errorf("save session: %w", err)
	}

	saved, err := state.sessionStore.Load(latest.ID)
	if err != nil {
		return fmt.Errorf("reload saved session: %w", err)
	}

	state.sessionState = saved

	return nil
}
