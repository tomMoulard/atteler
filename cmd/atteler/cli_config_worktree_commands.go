package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"sort"
	"strconv"
	"strings"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/tommoulard/atteler/pkg/agent"
	appconfig "github.com/tommoulard/atteler/pkg/config"
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

	if len(state.loadedConfigPaths) == 0 {
		fmt.Println("config: no config files loaded")
	} else {
		fmt.Println("config: " + strings.Join(state.loadedConfigPaths, ", "))
	}

	fmt.Println("sessions: " + state.sessionStore.Dir() + " (" + pathStatus(state.sessionStore.Dir()) + ")")

	providers := state.registry.ListProviders()
	sort.Strings(providers)

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

	if len(providers) == 0 {
		return errors.New("doctor: no providers registered; set provider credentials or config")
	}

	// Health check every registered provider and list their models.
	fmt.Println()

	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()

	results := state.registry.CheckHealth(ctx)
	healthy := 0

	for _, r := range results {
		if r.Healthy {
			fmt.Printf("  [ok] %s\n", r.Name)

			healthy++
		} else {
			fmt.Printf("  [FAIL] %s: %v\n", r.Name, r.Error)
		}

		for _, m := range r.Models {
			fmt.Printf("         - %s\n", m)
		}
	}

	if healthy == 0 {
		return errors.New("doctor: all providers failed their health check")
	}

	return nil
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

func llmConfig(cfg appconfig.Config, selectedModel string) llm.AutoRegisterConfig {
	providers := make(map[string]llm.ProviderConfig, len(cfg.Providers))
	for name, provider := range cfg.Providers {
		providers[name] = llm.ProviderConfig{
			Disabled:       provider.Disabled,
			BaseURL:        provider.BaseURL,
			TimeoutSeconds: provider.TimeoutSeconds,
		}
	}

	if len(providers) == 0 {
		providers = nil
	}

	return llm.AutoRegisterConfig{
		DefaultProvider: cfg.DefaultProvider,
		DefaultModel:    cfg.DefaultModel,
		SelectedModel:   selectedModel,
		Providers:       providers,
	}
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
		MaxFileBytes:  cfg.Context.MaxFileBytes,
		MaxTotalBytes: cfg.Context.MaxTotalBytes,
	}
	if cwd, err := os.Getwd(); err == nil {
		opts.Root = cwd
	}

	return opts
}

// loadConfiguredReferences resolves the configured reference paths/URLs at
// startup and returns a pre-rendered reference block that can be injected into
// every LLM request as additional context. Errors are logged but not fatal so
// the session can still start with whatever references succeeded.
func loadConfiguredReferences(ctx context.Context, refs []string, opts contextref.Options) string {
	if len(refs) == 0 {
		return ""
	}

	loaded, err := contextref.LoadReferences(ctx, refs, opts)
	if err != nil {
		fmt.Fprintf(os.Stderr, "warning: loading configured references: %v\n", err)
	}

	return contextref.FormatReferences(loaded)
}

// buildReferenceContext combines the pre-loaded global reference context with
// any agent-specific references. If the agent has its own references they are
// loaded on the fly and appended after the global block.
func buildReferenceContext(ctx context.Context, globalRefCtx string, activeAgent agentSelection, opts contextref.Options) string {
	if !activeAgent.ok || len(activeAgent.agent.References) == 0 {
		return globalRefCtx
	}

	agentRefCtx := loadConfiguredReferences(ctx, activeAgent.agent.References, opts)
	if agentRefCtx == "" {
		return globalRefCtx
	}

	if globalRefCtx == "" {
		return agentRefCtx
	}

	return globalRefCtx + "\n\n" + agentRefCtx
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

	if err := worktree.MergeContext(ctx, state.cwd, state.worktreeInfo); err != nil {
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
