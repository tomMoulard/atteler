package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/signal"
	"path/filepath"
	"sort"
	"strings"
	"syscall"

	"github.com/tommoulard/atteler/pkg/agent"
	appconfig "github.com/tommoulard/atteler/pkg/config"
	"github.com/tommoulard/atteler/pkg/events"
	"github.com/tommoulard/atteler/pkg/llm"
	"github.com/tommoulard/atteler/pkg/session"
	"github.com/tommoulard/atteler/pkg/worktree"
)

const affirmativeYes = "yes"

func parseOptions() cliOptions {
	var opts cliOptions

	opts.temperature = floatFlag{name: "temperature", min: 0}
	opts.topP = floatFlag{name: "top-p", min: 0, max: 1, hasMax: true}
	opts.routeBudget = floatFlag{name: "route-budget", min: 0}
	opts.routeCacheReuse = floatFlag{name: "route-cache-reuse", min: 0, max: 1, hasMax: true}
	opts.maxTokens = positiveIntFlag{name: "max-tokens"}
	opts.maxInputTokens = positiveIntFlag{name: "max-input-tokens"}
	opts.seed = nonNegativeIntFlag{name: "seed"}
	opts.mcpTimeout = positiveIntFlag{name: "mcp-timeout-seconds"}
	opts.spawnTimeout = positiveIntFlag{name: "spawn-timeout-seconds"}
	registerCLIFlags(&opts)

	flag.Usage = groupedUsage

	argPlan := translateCLIArgs(os.Args[1:])
	if argPlan.Err != nil {
		opts.parseErr = argPlan.Err
		return opts
	}

	if argPlan.Help {
		opts.helpRequested = true
		opts.helpDomain = argPlan.HelpDomain

		return opts
	}

	if err := flag.CommandLine.Parse(argPlan.Args); err != nil {
		opts.parseErr = err
		return opts
	}

	applyPositionalOptions(&opts, flag.Args())

	applyDebugEnvOptions(&opts, os.Getenv)

	return opts
}

func applyPositionalOptions(opts *cliOptions, args []string) {
	if opts == nil {
		return
	}

	if opts.explainConfigPath != "" {
		opts.explainConfig = true
	}

	if len(args) == 0 {
		return
	}

	positional := strings.Join(args, " ")

	if opts.explainConfig {
		if opts.explainConfigPath == "" {
			opts.explainConfigPath = positional
		}

		return
	}

	if opts.oncePrompt == "" {
		opts.oncePrompt = positional
	}
}

func applyDebugEnvOptions(opts *cliOptions, getenv func(string) string) {
	if opts == nil || getenv == nil {
		return
	}

	applyDebugBool(getenv, "DEBUG_ATTELER_DOCTOR", &opts.doctor)
	applyDebugBool(getenv, "DEBUG_ATTELER_DOCTOR_OFFLINE", &opts.doctorOffline)
	applyDebugBool(getenv, "DEBUG_ATTELER_VALIDATE_CONFIG", &opts.validateConfig)
	applyDebugBool(getenv, "DEBUG_ATTELER_LIST_CONFIG_PATHS", &opts.listConfigPaths)
	applyDebugBool(getenv, "DEBUG_ATTELER_LIST_PROVIDERS", &opts.listProviders)
	applyDebugBool(getenv, "DEBUG_ATTELER_LIST_KNOWN_MODELS", &opts.listKnownModels)
	applyDebugBool(getenv, "DEBUG_ATTELER_LIST_MODELS", &opts.listModels)
	applyDebugBool(getenv, "DEBUG_ATTELER_LIST_AGENTS", &opts.listAgents)
	applyDebugBool(getenv, "DEBUG_ATTELER_LIST_PLUGINS", &opts.listPlugins)
	applyDebugBool(getenv, "DEBUG_ATTELER_LIST_HOOK_EVENTS", &opts.listHookEvents)
	applyDebugBool(getenv, "DEBUG_ATTELER_LIST_HOOK_EVENTS_JSON", &opts.listHookEventsJSON)
	applyDebugBool(getenv, "DEBUG_ATTELER_CODE_SUMMARY", &opts.codeSummary)
	applyDebugBool(getenv, "DEBUG_ATTELER_CODE_FILES", &opts.listCodeFiles)
	applyDebugBool(getenv, "DEBUG_ATTELER_CODE_IMPORTS", &opts.listCodeImports)
	applyDebugBool(getenv, "DEBUG_ATTELER_CODE_IMPORT_SUMMARY", &opts.listCodeImportSummary)
	applyDebugBool(getenv, "DEBUG_ATTELER_CODE_IMPORT_FILE_SUMMARY", &opts.listCodeImportFileSummary)
	applyDebugBool(getenv, "DEBUG_ATTELER_CODE_LAYERS", &opts.listCodeLayers)
	applyDebugBool(getenv, "DEBUG_ATTELER_CODE_CYCLES", &opts.listCodeCycles)
	applyDebugBool(getenv, "DEBUG_ATTELER_REVIEW_PLAN", &opts.reviewPlan)
	applyDebugBool(getenv, "DEBUG_ATTELER_REVIEW_RUN", &opts.reviewRun)
	applyDebugBool(getenv, "DEBUG_ATTELER_REVIEW_SCAN", &opts.reviewScan)
	applyDebugBool(getenv, "DEBUG_ATTELER_AGENT_PERFORMANCE_SUMMARY", &opts.agentPerformanceSummary)
	applyDebugBool(getenv, "DEBUG_ATTELER_WATCH_SCAN", &opts.watchScan)
	applyDebugBool(getenv, "DEBUG_ATTELER_WATCH_JSON", &opts.watchJSON)
	applyDebugBool(getenv, "DEBUG_ATTELER_WATCH_LOOP", &opts.watchLoop)
	applyDebugBool(getenv, "DEBUG_ATTELER_LSP_SYMBOLS", &opts.lspSymbols)

	applyDebugString(getenv, "DEBUG_ATTELER_MCP_MANIFEST", &opts.mcpManifestPath)
	applyDebugString(getenv, "DEBUG_ATTELER_MCP_CAPABILITY", &opts.mcpCapability)
	applyDebugString(getenv, "DEBUG_ATTELER_MCP_SERVER", &opts.mcpServerName)
	applyDebugString(getenv, "DEBUG_ATTELER_MCP_METHOD", &opts.mcpMethod)
	applyDebugString(getenv, "DEBUG_ATTELER_MCP_PARAMS", &opts.mcpParamsJSON)
	applyDebugString(getenv, "DEBUG_ATTELER_MCP_TOOL", &opts.mcpToolName)
	applyDebugString(getenv, "DEBUG_ATTELER_MCP_TOOL_ARGS", &opts.mcpToolArgsJSON)
	applyDebugString(getenv, "DEBUG_ATTELER_LSP_COMMAND", &opts.lspCommand)
	applyDebugRawStringList(getenv, "DEBUG_ATTELER_LSP_ARGS", &opts.lspArgs)
	applyDebugString(getenv, "DEBUG_ATTELER_LSP_FILE", &opts.lspFilePath)
	applyDebugString(getenv, "DEBUG_ATTELER_LSP_ROOT", &opts.lspRootPath)
	applyDebugString(getenv, "DEBUG_ATTELER_LSP_LANGUAGE", &opts.lspLanguageID)
	applyDebugString(getenv, "DEBUG_ATTELER_LSP_WORKSPACE_SYMBOLS", &opts.lspWorkspaceSymbols)
	applyDebugString(getenv, "DEBUG_ATTELER_GIT_HISTORY_SEARCH", &opts.gitHistorySearch)
	applyDebugPositiveInt(getenv, "DEBUG_ATTELER_GIT_HISTORY_LIMIT", &opts.gitHistoryLimit)
	applyDebugPositiveInt(getenv, "DEBUG_ATTELER_MCP_TIMEOUT_SECONDS", &opts.mcpTimeout)
	applyDebugPositiveInt(getenv, "DEBUG_ATTELER_WATCH_LARGE_FILE_BYTES", &opts.watchLargeFileBytes)
	applyDebugPositiveInt(getenv, "DEBUG_ATTELER_WATCH_INTERVAL_SECONDS", &opts.watchIntervalSeconds)
	applyDebugPositiveInt(getenv, "DEBUG_ATTELER_WATCH_MAX_ITERATIONS", &opts.watchMaxIterations)
}

func applyDebugBool(getenv func(string) string, name string, target *bool) {
	if target == nil || *target {
		return
	}

	switch strings.ToLower(strings.TrimSpace(getenv(name))) {
	case "1", "true", affirmativeYes, "on":
		*target = true
	}
}

func applyDebugString(getenv func(string) string, name string, target *string) {
	if target == nil || strings.TrimSpace(*target) != "" {
		return
	}

	if value := strings.TrimSpace(getenv(name)); value != "" {
		*target = value
	}
}

func applyDebugRawStringList(getenv func(string) string, name string, target *rawStringListFlag) {
	if target == nil || len(*target) > 0 {
		return
	}

	if value := strings.TrimSpace(getenv(name)); value != "" {
		*target = append(*target, value)
	}
}

func applyDebugPositiveInt(getenv func(string) string, name string, target *positiveIntFlag) {
	if target == nil || target.set {
		return
	}

	value := strings.TrimSpace(getenv(name))
	if value == "" {
		return
	}

	if err := target.Set(value); err != nil {
		fmt.Fprintln(os.Stderr, "warning: ignoring "+name+": "+err.Error())
	}
}

func main() {
	configureSlog()

	ctx, stop := signal.NotifyContext(rootContext(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	if err := run(ctx); err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)

		stop()     // ensure signal handler is deregistered before exit
		os.Exit(1) //nolint:gocritic // defer stop() handles the normal exit path
	}
}

// configureSlog sets up the global slog handler. It reads the SLOG_LEVEL
// environment variable (debug, info, warn, error) and defaults to info.
// Output goes to stderr so it doesn't interfere with normal TUI output.
func configureSlog() {
	level := slog.LevelInfo

	switch strings.ToLower(os.Getenv("SLOG_LEVEL")) {
	case "debug":
		level = slog.LevelDebug
	case "warn", "warning":
		level = slog.LevelWarn
	case statusError:
		level = slog.LevelError
	}

	handler := slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: level})
	slog.SetDefault(slog.New(handler))
}

func versionString() string {
	return fmt.Sprintf("atteler %s (commit %s, built %s)", version, commit, date)
}

func initConfig(path string) error {
	path = strings.TrimSpace(path)
	if path == "" {
		return errors.New("config path is required")
	}

	dir := filepath.Dir(path)
	if dir != "." && dir != "" {
		if err := os.MkdirAll(dir, 0o750); err != nil {
			return fmt.Errorf("create config dir %s: %w", dir, err)
		}
	}

	file, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		if errors.Is(err, os.ErrExist) {
			return fmt.Errorf("config %s already exists", path)
		}

		return fmt.Errorf("create config %s: %w", path, err)
	}

	if _, err := file.WriteString(appconfig.TemplateYAML()); err != nil {
		_ = file.Close()
		return fmt.Errorf("write config %s: %w", path, err)
	}

	if err := file.Close(); err != nil {
		return fmt.Errorf("close config %s: %w", path, err)
	}

	fmt.Println("Wrote " + path)

	return nil
}

func oneShotPrompt(prompt string, readStdin bool) (string, error) {
	if readStdin {
		data, err := io.ReadAll(os.Stdin)
		if err != nil {
			return "", fmt.Errorf("read stdin: %w", err)
		}

		prompt = appendStdinContext(prompt, string(data))
	}

	if strings.TrimSpace(prompt) == "" {
		return "", errors.New("one-shot prompt is empty")
	}

	return prompt, nil
}

func normalizeOutputFormat(format string) (string, error) {
	format = strings.ToLower(strings.TrimSpace(format))
	if format == "" {
		return outputFormatText, nil
	}

	switch format {
	case outputFormatText, outputFormatJSON:
		return format, nil
	default:
		return "", fmt.Errorf("unsupported output format %q (supported: %s, %s)", format, outputFormatText, outputFormatJSON)
	}
}

func appendStdinContext(prompt, stdin string) string {
	stdin = strings.TrimRight(stdin, "\n")
	if strings.TrimSpace(stdin) == "" {
		return prompt
	}

	if strings.TrimSpace(prompt) == "" {
		return stdin
	}

	return prompt + "\n\n<stdin>\n" + stdin + "\n</stdin>"
}

func listConfigPaths() {
	for _, path := range appconfig.DefaultPaths() {
		fmt.Println(path + "\t" + configPathStatus(path))
	}
}

func validateConfig() error {
	cfg, loaded, err := appconfig.Load()
	if err != nil {
		return fmt.Errorf("validate config: %w", err)
	}

	if _, err := agentLoopBudgetFromConfig(cfg); err != nil {
		return fmt.Errorf("validate config: %w", err)
	}

	if len(loaded) == 0 {
		fmt.Println("Config valid: no config files loaded.")
		return nil
	}

	fmt.Println("Config valid: " + strings.Join(loaded, ", "))

	return nil
}

func configPathStatus(path string) string {
	info, err := os.Stat(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return "missing"
		}

		return "error: " + err.Error()
	}

	if info.IsDir() {
		return "directory"
	}

	return "present"
}

func listKnownProviders() {
	for _, provider := range knownProvidersSorted() {
		fmt.Println(provider.Name)
	}
}

func listKnownModels() {
	for _, provider := range knownProvidersSorted() {
		sort.Strings(provider.Models)

		for _, model := range provider.Models {
			fmt.Println(provider.Name + "/" + model)
		}
	}
}

func knownProvidersSorted() []llm.ProviderInfo {
	providers := llm.KnownProviders()
	sort.Slice(providers, func(i, j int) bool {
		return providers[i].Name < providers[j].Name
	})

	return providers
}

func run(ctx context.Context) error {
	opts := parseOptions()
	if opts.configPaths != "" {
		if err := os.Setenv(appconfig.EnvPath, opts.configPaths); err != nil {
			fmt.Fprintln(os.Stderr, "warning: cannot set config path override: "+err.Error())
		}
	}

	if handled, err := runInlineCommand(ctx, opts); handled {
		return err
	}

	// Phase 1: providerless commands (no LLM registry needed).
	store := session.NewStore(opts.sessionDir)

	if handled, err := dispatchProviderless(ctx, opts, store); handled {
		return err
	}

	// Phase 2: providerless-config commands (need config/agents but no LLM).
	if providerlessConfigRequested(opts) {
		state, stateErr := providerlessState(store)
		if stateErr != nil {
			return stateErr
		}

		if handled, err := dispatchProviderlessConfig(ctx, opts, state); handled {
			return err
		}
	}

	// Phase 3: full state (LLM providers, hooks, sessions).
	state, err := loadAppState(ctx, opts)
	if err != nil {
		return err
	}

	return runWithState(ctx, opts, state)
}

// runInlineCommand handles trivial early-exit commands (version, config
// template, etc.) that need no session store or provider.
func runInlineCommand(ctx context.Context, opts cliOptions) (bool, error) {
	switch {
	case opts.parseErr != nil:
		return true, opts.parseErr
	case opts.helpRequested:
		return true, printCLIHelp(os.Stdout, flag.CommandLine, opts.helpDomain)
	case opts.printConfigTemplate:
		fmt.Print(appconfig.TemplateYAML())
		return true, nil
	case opts.showVersion:
		fmt.Println(versionString())
		return true, nil
	case opts.initConfigPath != "":
		return true, initConfig(opts.initConfigPath)
	case opts.listConfigPaths:
		listConfigPaths()
		return true, nil
	case opts.validateConfig:
		return true, validateConfig()
	case opts.explainConfig:
		return true, explainConfig(opts)
	case opts.doctorOffline:
		return true, doctorOffline(opts)
	case opts.listProviders:
		listKnownProviders()
		return true, nil
	case opts.listKnownModels:
		listKnownModels()
		return true, nil
	case opts.listWorktrees:
		return true, listWorktrees(ctx)
	case opts.mergeWorktreeRef != "":
		return true, mergeWorktreeBySession(ctx, opts.mergeWorktreeRef, opts.mergeWorktreeAllowBaseMismatch)
	default:
		return false, nil
	}
}

func providerlessState(store *session.Store) (appState, error) {
	cwd, err := os.Getwd()
	if err != nil {
		return appState{}, fmt.Errorf("locate working directory: %w", err)
	}

	cfg, loadedConfigPaths, cfgErr := appconfig.Load()
	if cfgErr != nil {
		fmt.Fprintln(os.Stderr, "warning: "+cfgErr.Error())
	}

	return appState{
		config:            cfg,
		agentRegistry:     agent.NewRegistry(cfg.Agents),
		sessionStore:      store,
		cwd:               cwd,
		loadedConfigPaths: loadedConfigPaths,
		pluginPaths:       append([]string(nil), cfg.Plugins.Paths...),
		pluginPolicy:      clonePluginPolicy(cfg.Plugins.Policy),
	}, nil
}

func runWithState(ctx context.Context, opts cliOptions, state appState) error {
	if handled, err := dispatchStateful(ctx, opts, state); handled {
		return err
	}

	outputFormat, err := normalizeOutputFormat(opts.outputFormat)
	if err != nil {
		return err
	}

	if opts.headless && opts.oncePrompt == "" && !opts.readStdin {
		return errors.New("headless mode requires --once, positional prompt text, or --stdin")
	}

	if opts.oncePrompt == "" && !opts.readStdin {
		return runInteractive(ctx, state)
	}

	prompt, err := oneShotPrompt(opts.oncePrompt, opts.readStdin)
	if err != nil {
		return err
	}

	runErr := runOnceWithOptions(
		ctx,
		state.registry,
		state.agentRegistry,
		state.hookRunner,
		state.sessionStore,
		state.sessionState,
		state.contextOptions,
		state.referenceContext,
		state.selectedModel,
		state.selectedAgent,
		state.fallbackModels,
		state.generationDefaults,
		state.generationOverrides,
		state.maxInputTokens,
		runOnceExecutionOptions{
			AgentLoopBudget: state.agentLoopBudget,
			Response: responseRecordOptions{
				RecordPath: opts.recordResponsePath,
				ReplayPath: opts.replayResponsePath,
			},
			OutputFormat: outputFormat,
			Headless:     opts.headless,
		},
		state.modelLocked,
		prompt,
	)
	finalizeWorktree(ctx, &state)

	return runErr
}

func loadAppState(ctx context.Context, opts cliOptions) (appState, error) {
	cfg, loadedConfigPaths, err := appconfig.Load()
	if err != nil {
		fmt.Fprintln(os.Stderr, "warning: "+err.Error())
	}

	agentRegistry := agent.NewRegistry(cfg.Agents)
	// Default to a stderr logger so events from utility commands (--bash,
	// --mcp, one-shot, etc.) are visible without extra configuration. Headless
	// runs stay quiet so JSON output isn't polluted; runInteractive replaces
	// this with a logger-less runner so stderr writes don't bleed onto the TUI.
	var hookLogWriter io.Writer
	if !opts.headless {
		hookLogWriter = os.Stderr
	}

	hookRunner := events.NewRunnerWithLogger(cfg.Hooks, hookLogWriter)
	store := session.NewStore(opts.sessionDir)
	stateStore := appconfig.NewStateStore("")

	persistedState, stateErr := stateStore.Load()
	if stateErr != nil {
		fmt.Fprintln(os.Stderr, "warning: "+stateErr.Error())
	}

	cwd, cwdErr := os.Getwd()
	if cwdErr != nil {
		cwd = ""
	}

	selection, err := resolveSelection(opts, cfg, persistedState.ModelForFolder(cwd), agentRegistry, store)
	if err != nil {
		return appState{}, err
	}

	reg := autoRegisterForOptions(ctx, opts, cfg, selection.selectedModel)
	contextOptions := contextOptionsFromConfig(cfg)
	referenceContext := loadConfiguredReferences(ctx, cfg.Context.References, contextOptions)
	generationDefaults := generationFromConfig(cfg)
	generationOverrides := generationOverridesFromState(opts, selection, persistedState, cwd)

	maxInputTokens := maxInputTokensFromConfigOptions(cfg, opts)

	agentLoopBudget, err := agentLoopBudgetFromConfig(cfg)
	if err != nil {
		return appState{}, err
	}

	providers := reg.ListProviders()
	if len(providers) == 0 && !opts.headless {
		fmt.Fprintln(os.Stderr, "warning: no LLM providers configured, set ANTHROPIC_API_KEY or OPENAI_API_KEY")
	}

	// Set up git worktree isolation when requested.
	var wtInfo *worktree.Info

	if opts.useWorktree && cwd != "" {
		// If continuing a session that already has a worktree, re-use it.
		if selection.sessionState.WorktreePath != "" {
			wtInfo = &worktree.Info{
				Path:       selection.sessionState.WorktreePath,
				Branch:     selection.sessionState.WorktreeBranch,
				BaseBranch: selection.sessionState.WorktreeBase,
				SessionID:  selection.sessionState.ID,
			}
			fmt.Fprintln(os.Stderr, "worktree: reusing "+wtInfo.Path)
		} else {
			wtInfo, err = worktree.CreateContext(ctx, cwd, selection.sessionState.ID)
			if err != nil {
				return appState{}, fmt.Errorf("worktree setup: %w", err)
			}

			selection.sessionState.WorktreePath = wtInfo.Path
			selection.sessionState.WorktreeBranch = wtInfo.Branch
			selection.sessionState.WorktreeBase = wtInfo.BaseBranch
			fmt.Fprintln(os.Stderr, "worktree: created "+wtInfo.Path+" (branch "+wtInfo.Branch+")")
		}

		// Update context references to point at the worktree.
		contextOptions.Root = wtInfo.Path
	}

	return appState{
		config:              cfg,
		registry:            reg,
		agentRegistry:       agentRegistry,
		hookRunner:          hookRunner,
		sessionStore:        store,
		stateStore:          stateStore,
		contextOptions:      contextOptions,
		referenceContext:    referenceContext,
		sessionState:        selection.sessionState,
		worktreeInfo:        wtInfo,
		cwd:                 cwd,
		loadedConfigPaths:   loadedConfigPaths,
		providers:           providers,
		selectedModel:       selection.selectedModel,
		selectedAgent:       selection.selectedAgent,
		fallbackModels:      selection.fallbackModels,
		pluginPaths:         append([]string(nil), cfg.Plugins.Paths...),
		pluginPolicy:        clonePluginPolicy(cfg.Plugins.Policy),
		generationDefaults:  generationDefaults,
		generationOverrides: generationOverrides,
		agentLoopBudget:     agentLoopBudget,
		maxInputTokens:      maxInputTokens,
		hookConfig:          cfg.Hooks,
		modelLocked:         selection.modelLocked,
		autoMergeWorktree:   opts.useWorktree && !opts.noAutoMerge,
		promptLocalOnly:     opts.promptLocalOnly,
	}, nil
}

func autoRegisterForOptions(ctx context.Context, opts cliOptions, cfg appconfig.Config, selectedModel string) *llm.Registry {
	regCfg := llmConfig(cfg, selectedModel)

	if opts.headless {
		regCfg.Logger = slog.New(slog.NewTextHandler(io.Discard, nil))
	}

	return llm.AutoRegisterWithConfigContext(ctx, regCfg)
}
