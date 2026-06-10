package main

import (
	"context"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"

	appconfig "github.com/tommoulard/atteler/pkg/config"
	"github.com/tommoulard/atteler/pkg/llm"
)

func explainConfig(ctx context.Context, opts cliOptions) error {
	if err := authorizeConfigStackRead(ctx, "explain config", "atteler.config.explain"); err != nil {
		return fmt.Errorf("explain config: %w", err)
	}

	cfg, loaded, origins, diagnostics, err := appconfig.LoadWithDiagnostics()
	if err != nil {
		printDiagnostics(os.Stdout, diagnostics)

		return fmt.Errorf("explain config: %w", err)
	}

	stateStore := appconfig.NewStateStore("")

	if err := authorizeStateFileRead(ctx, "load persisted state for config explanation", "atteler.config.explain", stateStore.Path()); err != nil {
		return fmt.Errorf("explain config: %w", err)
	}

	persistedState, stateErr := stateStore.Load()
	if stateErr != nil {
		fmt.Fprintln(os.Stderr, "warning: "+stateErr.Error())
	}

	cwd, cwdErr := os.Getwd()
	if cwdErr != nil {
		cwd = ""
	}

	addRuntimeConfigOrigins(origins, cfg, opts, persistedState, cwd, stateStore.Path())
	writeConfigExplanationWithDiagnostics(os.Stdout, loaded, origins, diagnostics, opts.explainConfigPath)

	return nil
}

func writeConfigExplanation(w io.Writer, loaded []string, origins appconfig.OriginMap, fieldFilter string) {
	writeConfigExplanationWithDiagnostics(w, loaded, origins, nil, fieldFilter)
}

func writeConfigExplanationWithDiagnostics(
	w io.Writer,
	loaded []string,
	origins appconfig.OriginMap,
	diagnostics []appconfig.Diagnostic,
	fieldFilter string,
) {
	fieldFilter = strings.TrimSpace(fieldFilter)

	fmt.Fprintln(w, "Config explanation")
	fmt.Fprintln(w)
	fmt.Fprintln(w, "Schema versions:")
	fmt.Fprintf(w, "  config: %d\n", appconfig.ConfigSchemaVersion)
	fmt.Fprintf(w, "  state: %d\n", appconfig.StateSchemaVersion)
	fmt.Fprintln(w)
	fmt.Fprintln(w, "Precedence (lowest to highest):")
	fmt.Fprintln(w, "  harness-import < global-file < project-file < env-file < state-override < cli-flag < runtime-selection")
	fmt.Fprintln(w)
	fmt.Fprintln(w, "Replacement semantics:")
	fmt.Fprintln(w, "  - Scalar fields override earlier scalar values.")
	fmt.Fprintln(w, "  - Provider and agent maps merge by name; fields inside the same name override independently.")
	fmt.Fprintln(w, "  - Lists and per-agent tool maps replace the earlier value in full when set later.")
	fmt.Fprintln(w)

	writeConfigExplanationLoadedSources(w, loaded, origins)
	printDiagnostics(w, diagnostics)

	if len(diagnostics) > 0 {
		fmt.Fprintln(w)
	}

	writeConfigExplanationDefaults(w, fieldFilter)

	heading := "Field origins:"
	if fieldFilter != "" {
		heading = fmt.Sprintf("Field origins matching %q:", fieldFilter)
	}

	fmt.Fprintln(w, heading)

	matched := false

	for _, path := range origins.Paths() {
		if !configExplainPathMatches(path, fieldFilter) {
			continue
		}

		matched = true
		origin := origins[path]

		final, ok := origin.Final()
		if !ok {
			continue
		}

		fmt.Fprintf(w, "%s: %s\n", path, truncateConfigExplainValue(appconfig.RedactOriginValue(path, final.Value)))

		for _, event := range origin.Chain {
			note := ""
			if event.Note != "" {
				note = " (" + event.Note + ")"
			}

			fmt.Fprintf(
				w,
				"  - %s by %s [%s] => %s%s\n",
				event.Operation,
				event.Source,
				event.Kind,
				truncateConfigExplainValue(appconfig.RedactOriginValue(path, event.Value)),
				note,
			)
		}
	}

	if !matched {
		fmt.Fprintln(w, "  (no fields matched)")
	}
}

func printDiagnostics(w io.Writer, diagnostics []appconfig.Diagnostic) {
	if len(diagnostics) == 0 {
		return
	}

	fmt.Fprintln(w, "Config importer warnings:")

	for _, diagnostic := range diagnostics {
		severity := diagnostic.Severity
		if severity == "" {
			severity = appconfig.DiagnosticWarning
		}

		fmt.Fprintf(w, "  - [%s] %s\n", severity, diagnostic.String())
	}
}

func writeConfigExplanationLoadedSources(w io.Writer, loaded []string, origins appconfig.OriginMap) {
	fmt.Fprintln(w, "Loaded sources:")

	if len(loaded) == 0 {
		fmt.Fprintln(w, "  (none)")
		fmt.Fprintln(w)

		return
	}

	for i, path := range loaded {
		kind := originKindForSource(origins, path)
		if kind == "" {
			kind = "loaded"
		}

		fmt.Fprintf(w, "  %d. [%s] %s\n", i+1, kind, path)
	}

	fmt.Fprintln(w)
}

func writeConfigExplanationDefaults(w io.Writer, fieldFilter string) {
	heading := "Implicit defaults (used when no higher-precedence source sets the field):"
	if fieldFilter != "" {
		heading = fmt.Sprintf("Implicit defaults matching %q:", fieldFilter)
	}

	fmt.Fprintln(w, heading)

	matched := false

	for _, defaultInfo := range appconfig.DefaultDiagnostics() {
		if !configExplainPathMatches(defaultInfo.Field, fieldFilter) {
			continue
		}

		matched = true

		fmt.Fprintf(w, "%s: %s\n", defaultInfo.Field, defaultInfo.Value)
		fmt.Fprintf(w, "  - %s\n", defaultInfo.Message)
	}

	if !matched {
		fmt.Fprintln(w, "  (no defaults matched)")
	}

	fmt.Fprintln(w)
}

func addRuntimeConfigOrigins(
	origins appconfig.OriginMap,
	cfg appconfig.Config,
	opts cliOptions,
	persistedState appconfig.State,
	cwd string,
	statePath string,
) {
	selectedModel := addSelectedModelOrigins(origins, cfg, opts, persistedState, cwd, statePath)
	if selectedModel != "" {
		appendDiagnosticOrigin(origins, "runtime.request_model", appconfig.OriginEvent{
			Kind:   appconfig.OriginRuntimeSelection,
			Source: "atteler selection pipeline",
			Value:  selectedModel,
			Note:   "model used for provider registration and requests before any response-level provider fallback",
		})
	}

	addSelectedProviderOrigin(origins, cfg, selectedModel)

	if opts.agentName != "" {
		appendDiagnosticOrigin(origins, "runtime.selected_agent", appconfig.OriginEvent{
			Kind:   appconfig.OriginCLIFlag,
			Source: "--agent",
			Value:  opts.agentName,
		})
	}

	addRuntimeGenerationOrigins(origins, cfg, opts, persistedState, cwd, statePath)
}

func addSelectedModelOrigins(
	origins appconfig.OriginMap,
	cfg appconfig.Config,
	opts cliOptions,
	persistedState appconfig.State,
	cwd string,
	statePath string,
) string {
	if opts.model != "" {
		appendDiagnosticOrigin(origins, "runtime.selected_model", appconfig.OriginEvent{
			Kind:   appconfig.OriginCLIFlag,
			Source: "--model",
			Value:  opts.model,
		})

		return opts.model
	}

	if opts.agentName != "" {
		if configuredAgent, ok := cfg.Agents[opts.agentName]; ok && configuredAgent.Model != "" {
			copyOriginChain(origins, "runtime.selected_model", fmt.Sprintf("agents.%s.model", opts.agentName))
			appendDiagnosticOrigin(origins, "runtime.selected_model", appconfig.OriginEvent{
				Kind:   appconfig.OriginRuntimeSelection,
				Source: "--agent " + opts.agentName,
				Value:  configuredAgent.Model,
				Note:   "selected agent model is used before persisted state and default_model",
			})

			return configuredAgent.Model
		}
	}

	if model, source := stateModelOrigin(persistedState, cwd, statePath); model != "" {
		appendDiagnosticOrigin(origins, "runtime.selected_model", appconfig.OriginEvent{
			Kind:   appconfig.OriginStateOverride,
			Source: source,
			Value:  model,
		})

		return model
	}

	if cfg.DefaultModel != "" {
		copyOriginChain(origins, "runtime.selected_model", "default_model")
		appendDiagnosticOrigin(origins, "runtime.selected_model", appconfig.OriginEvent{
			Kind:   appconfig.OriginRuntimeSelection,
			Source: "default_model",
			Value:  cfg.DefaultModel,
			Note:   "used because no CLI, selected-agent, session, or state model was set",
		})

		return cfg.DefaultModel
	}

	return ""
}

func addSelectedProviderOrigin(origins appconfig.OriginMap, cfg appconfig.Config, selectedModel string) {
	if provider, ok := providerPrefix(selectedModel); ok {
		appendDiagnosticOrigin(origins, "runtime.selected_provider", appconfig.OriginEvent{
			Kind:   appconfig.OriginRuntimeSelection,
			Source: "runtime.selected_model",
			Value:  provider,
			Note:   "provider-qualified model prefix",
		})

		return
	}

	if cfg.DefaultProvider == "" {
		return
	}

	copyOriginChain(origins, "runtime.selected_provider", "default_provider")
	appendDiagnosticOrigin(origins, "runtime.selected_provider", appconfig.OriginEvent{
		Kind:   appconfig.OriginRuntimeSelection,
		Source: "default_provider",
		Value:  cfg.DefaultProvider,
		Note:   "selected model is not provider-qualified",
	})
}

func providerPrefix(model string) (string, bool) {
	provider, _, ok := strings.Cut(strings.TrimSpace(model), "/")
	if !ok || strings.TrimSpace(provider) == "" {
		return "", false
	}

	return strings.TrimSpace(provider), true
}

func addRuntimeGenerationOrigins(
	origins appconfig.OriginMap,
	cfg appconfig.Config,
	opts cliOptions,
	persistedState appconfig.State,
	cwd string,
	statePath string,
) {
	if cfg.Generation.Temperature != nil {
		copyOriginChain(origins, "runtime.generation.temperature", "generation.temperature")
	}

	if opts.temperature.set {
		appendDiagnosticOrigin(origins, "runtime.generation.temperature", appconfig.OriginEvent{
			Kind:   appconfig.OriginCLIFlag,
			Source: "--temperature",
			Value:  fmt.Sprint(opts.temperature.value),
		})
	}

	if cfg.Generation.TopP != nil {
		copyOriginChain(origins, "runtime.generation.top_p", "generation.top_p")
	}

	if opts.topP.set {
		appendDiagnosticOrigin(origins, "runtime.generation.top_p", appconfig.OriginEvent{
			Kind:   appconfig.OriginCLIFlag,
			Source: "--top-p",
			Value:  fmt.Sprint(opts.topP.value),
		})
	}

	if cfg.Generation.Seed != nil {
		copyOriginChain(origins, "runtime.generation.seed", "generation.seed")
	}

	if opts.seed.set {
		appendDiagnosticOrigin(origins, "runtime.generation.seed", appconfig.OriginEvent{
			Kind:   appconfig.OriginCLIFlag,
			Source: "--seed",
			Value:  strconv.Itoa(opts.seed.value),
		})
	}

	if cfg.Generation.MaxTokens > 0 {
		copyOriginChain(origins, "runtime.generation.max_tokens", "generation.max_tokens")
	}

	if opts.maxTokens.set {
		appendDiagnosticOrigin(origins, "runtime.generation.max_tokens", appconfig.OriginEvent{
			Kind:   appconfig.OriginCLIFlag,
			Source: "--max-tokens",
			Value:  strconv.Itoa(opts.maxTokens.value),
		})
	}

	if cfg.Generation.ModelMode != "" {
		copyOriginChain(origins, "runtime.generation.model_mode", "generation.model_mode")
	}

	if mode, source := stateModelModeOrigin(persistedState, cwd, statePath); mode != "" {
		appendDiagnosticOrigin(origins, "runtime.generation.model_mode", appconfig.OriginEvent{
			Kind:   appconfig.OriginStateOverride,
			Source: source,
			Value:  mode,
		})
	}

	if opts.modelMode != "" {
		appendDiagnosticOrigin(origins, "runtime.generation.model_mode", appconfig.OriginEvent{
			Kind:   appconfig.OriginCLIFlag,
			Source: "--model-mode",
			Value:  strings.TrimSpace(opts.modelMode),
		})
	}

	if cfg.Generation.ReasoningLevel != "" {
		copyOriginChain(origins, "runtime.generation.reasoning_level", "generation.reasoning_level")
	}

	if level, source := stateReasoningOrigin(persistedState, cwd, statePath); level != "" {
		appendDiagnosticOrigin(origins, "runtime.generation.reasoning_level", appconfig.OriginEvent{
			Kind:   appconfig.OriginStateOverride,
			Source: source,
			Value:  level,
		})
	}

	if opts.reasoningLevel != "" {
		appendDiagnosticOrigin(origins, "runtime.generation.reasoning_level", appconfig.OriginEvent{
			Kind:   appconfig.OriginCLIFlag,
			Source: "--reasoning-level",
			Value:  strings.TrimSpace(opts.reasoningLevel),
		})
	}
}

func stateModelOrigin(state appconfig.State, cwd, statePath string) (model, source string) {
	key := appconfig.FolderKey(cwd)
	if key != "" && state.Folders != nil {
		if folder := state.Folders[key]; folder.DefaultModel != "" {
			return folder.DefaultModel, statePath + " folder " + key
		}
	}

	if state.DefaultModel != "" {
		return state.DefaultModel, statePath + " global"
	}

	return "", ""
}

func stateReasoningOrigin(state appconfig.State, cwd, statePath string) (level, source string) {
	level = state.ReasoningLevelForFolder(cwd)
	if level == "" {
		return "", ""
	}

	key := appconfig.FolderKey(cwd)
	if key != "" && state.Folders != nil {
		if folder := state.Folders[key]; folder.DefaultReasoningLevel == level {
			return level, statePath + " folder " + key
		}
	}

	return level, statePath + " global"
}

func stateModelModeOrigin(state appconfig.State, cwd, statePath string) (mode, source string) {
	resolution := state.ResolveModelModePreference(cwd)
	if resolution.Source == "" {
		return "", ""
	}

	mode = resolution.Value
	if mode == "" {
		mode = llm.ModelModeDefault
	}

	if resolution.Scope == appconfig.ModelScopeFolder {
		return mode, statePath + " folder " + resolution.FolderKey
	}

	return mode, statePath + " global"
}

func copyOriginChain(origins appconfig.OriginMap, targetPath, sourcePath string) {
	sourceOrigin, ok := origins[sourcePath]
	if !ok {
		return
	}

	for _, event := range sourceOrigin.Chain {
		appendDiagnosticOrigin(origins, targetPath, event)
	}
}

func appendDiagnosticOrigin(origins appconfig.OriginMap, path string, event appconfig.OriginEvent) {
	if event.Operation == "" {
		event.Operation = appconfig.OriginSet
	}

	origin := origins[path]
	if len(origin.Chain) > 0 && event.Operation == appconfig.OriginSet {
		event.Operation = appconfig.OriginOverride
	}

	origin.Chain = append(origin.Chain, event)
	origins[path] = origin
}

func originKindForSource(origins appconfig.OriginMap, source string) appconfig.OriginKind {
	for _, field := range origins {
		for _, event := range field.Chain {
			if event.Source == source {
				return event.Kind
			}
		}
	}

	return ""
}

func truncateConfigExplainValue(value string) string {
	value = strings.ReplaceAll(value, "\n", `\n`)
	if len(value) <= 120 {
		return value
	}

	return value[:117] + "..."
}

func configExplainPathMatches(path, fieldFilter string) bool {
	fieldFilter = strings.TrimSpace(fieldFilter)
	if fieldFilter == "" {
		return true
	}

	return path == fieldFilter || strings.HasPrefix(path, fieldFilter+".") ||
		configExplainWildcardPathMatches(path, fieldFilter)
}

func configExplainWildcardPathMatches(pattern, fieldFilter string) bool {
	if !strings.Contains(pattern, "*") {
		return false
	}

	patternParts := strings.Split(pattern, ".")
	filterParts := strings.Split(fieldFilter, ".")

	if len(patternParts) < len(filterParts) {
		return false
	}

	for i, filterPart := range filterParts {
		if patternParts[i] != "*" && patternParts[i] != filterPart {
			return false
		}
	}

	return true
}
