package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"math"
	"os"
	"os/signal"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/tommoulard/atteler/pkg/agent"
	"github.com/tommoulard/atteler/pkg/autonomy"
	"github.com/tommoulard/atteler/pkg/autopilot"
	appconfig "github.com/tommoulard/atteler/pkg/config"
	"github.com/tommoulard/atteler/pkg/events"
	"github.com/tommoulard/atteler/pkg/llm"
	"github.com/tommoulard/atteler/pkg/modelroute"
	"github.com/tommoulard/atteler/pkg/permission"
	"github.com/tommoulard/atteler/pkg/session"
	"github.com/tommoulard/atteler/pkg/worktree"
)

const (
	affirmativeTrue          = "true"
	affirmativeYes           = "yes"
	negativeFalse            = "false"
	configPathStatusMissing  = "missing"
	configPathStatusUpToDate = "up-to-date"

	providerNameOpenAI      = "openai"
	providerNameAnthropic   = "anthropic"
	providerNameCodex       = "codex"
	providerNameClaudeCode  = "claude-code"
	providerTypeClaudeAlias = "claude"

	hookShutdownTimeout = 2 * time.Second
)

func parseOptions() cliOptions {
	var opts cliOptions
	initCLIFlagValues(&opts)
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
	applyAutoresearchShortcutOptions(&opts)

	return opts
}

func initCLIFlagValues(opts *cliOptions) {
	opts.temperature = floatFlag{name: "temperature", min: 0}
	opts.topP = floatFlag{name: "top-p", min: 0, max: 1, hasMax: true}
	opts.routeBudget = floatFlag{name: "route-budget", min: 0}
	opts.routeCacheReuse = floatFlag{name: "route-cache-reuse", min: 0, max: 1, hasMax: true}
	opts.evaluationCost = floatFlag{name: "evaluation-cost", min: 0}
	opts.evaluationConfidence = floatFlag{name: "evaluation-confidence", min: 0, max: 1, hasMax: true}
	opts.evaluationPassRate = floatFlag{name: "evaluation-pass-rate", min: 0, max: 1, hasMax: true}
	opts.evaluationScore = nonNegativeIntFlag{name: "evaluation-score"}
	opts.maxTokens = positiveIntFlag{name: "max-tokens"}
	opts.maxInputTokens = positiveIntFlag{name: "max-input-tokens"}
	opts.contextPackTokens = positiveIntFlag{name: "context-pack-tokens"}
	opts.planMaxAgents = positiveIntFlag{name: "plan-max-agents"}
	opts.memoryLimit = positiveIntFlag{name: "memory-limit"}
	opts.memoryTTL = positiveIntFlag{name: "memory-ttl-seconds"}
	opts.memoryRetentionDays = positiveIntFlag{name: "memory-retention-days"}
	opts.agentMemoryLimit = positiveIntFlag{name: "agent-memory-limit"}
	opts.agentMemoryTTL = positiveIntFlag{name: "agent-memory-ttl-seconds"}
	opts.retrievalLimit = positiveIntFlag{name: "retrieval-limit"}
	opts.vectorLimit = positiveIntFlag{name: "vector-limit"}
	opts.codeLimit = positiveIntFlag{name: "code-limit"}
	opts.vectorTimeout = positiveIntFlag{name: "vector-timeout-seconds"}
	opts.vectorChunkMaxRunes = positiveIntFlag{name: "vector-chunk-max-runes"}
	opts.vectorChunkOverlapRunes = positiveIntFlag{name: "vector-chunk-overlap-runes"}
	opts.mergeArtifactMaxBytes = positiveIntFlag{name: "merge-artifact-max-bytes"}
	opts.routeInputTokens = positiveIntFlag{name: "route-input-tokens"}
	opts.routeOutputTokens = positiveIntFlag{name: "route-output-tokens"}
	opts.routeCacheWriteTokens = positiveIntFlag{name: "route-cache-write-tokens"}
	opts.gitHistoryLimit = positiveIntFlag{name: "git-history-limit"}
	opts.incidentTimeout = positiveIntFlag{name: "incident-timeout-seconds"}
	opts.pluginTimeout = positiveIntFlag{name: "plugin-timeout-seconds"}
	opts.bashTimeout = positiveIntFlag{name: "bash-timeout-seconds"}
	opts.mcpTimeout = positiveIntFlag{name: "mcp-timeout-seconds"}
	opts.spawnTimeout = positiveIntFlag{name: "spawn-timeout-seconds"}
	opts.spawnTaskTimeout = positiveIntFlag{name: "spawn-task-timeout-seconds"}
	opts.spawnMaxConcurrency = positiveIntFlag{name: "spawn-max-concurrency"}
	opts.spawnTokenBudget = positiveIntFlag{name: "spawn-token-budget"}
	opts.spawnCostBudgetMicros = positiveIntFlag{name: "spawn-cost-budget-micros"}
	opts.spawnOutputBudgetBytes = positiveIntFlag{name: "spawn-output-budget-bytes"}
	opts.spawnRetryBackoff = positiveIntFlag{name: "spawn-retry-backoff-seconds"}
	opts.promptCompleteLimit = positiveIntFlag{name: "prompt-complete-limit"}
	opts.watchLargeFileBytes = positiveIntFlag{name: "watch-large-file-bytes"}
	opts.watchIntervalSeconds = positiveIntFlag{name: "watch-interval-seconds"}
	opts.watchMaxIterations = positiveIntFlag{name: "watch-max-iterations"}
	opts.skillMaxSteps = positiveIntFlag{name: "skill-max-steps"}
	opts.skillMinOccurrences = positiveIntFlag{name: "skill-min-occurrences"}
	opts.codeOffset = nonNegativeIntFlag{name: "code-offset"}
	opts.spawnRetries = nonNegativeIntFlag{name: "spawn-retries"}
	opts.seed = nonNegativeIntFlag{name: "seed"}
	opts.evalExitCode = nonNegativeIntFlag{name: "eval-exit-code"}
	opts.evaluationDurationMillis = nonNegativeIntFlag{name: "evaluation-duration-millis"}
	opts.evaluationFlakeCount = nonNegativeIntFlag{name: "evaluation-flake-count"}
	opts.evaluationInputTokens = nonNegativeIntFlag{name: "evaluation-input-tokens"}
	opts.evaluationOutputTokens = nonNegativeIntFlag{name: "evaluation-output-tokens"}
	opts.evaluationTotalTokens = nonNegativeIntFlag{name: "evaluation-total-tokens"}
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
	applyDebugBool(getenv, "DEBUG_ATTELER_CONFIG_REPORT", &opts.configReport)
	applyDebugBool(getenv, "DEBUG_ATTELER_EXPLAIN_CONFIG", &opts.explainConfig)
	applyDebugBool(getenv, "DEBUG_ATTELER_STATE_DIAGNOSTICS", &opts.stateDiagnostics)
	applyDebugBool(getenv, "DEBUG_ATTELER_LIST_CONFIG_PATHS", &opts.listConfigPaths)
	applyDebugBool(getenv, "DEBUG_ATTELER_LIST_PROVIDERS", &opts.listProviders)
	applyDebugBool(getenv, "DEBUG_ATTELER_LIST_KNOWN_MODELS", &opts.listKnownModels)
	applyDebugBool(getenv, "DEBUG_ATTELER_LIST_MODELS", &opts.listModels)
	applyDebugBool(getenv, "DEBUG_ATTELER_OLLAMA_STATUS", &opts.ollamaStatus)
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
	applyDebugString(getenv, "DEBUG_ATTELER_EXPLAIN_CONFIG_FIELD", &opts.explainConfigPath)
	applyDebugString(getenv, "DEBUG_ATTELER_GIT_HISTORY_SEARCH", &opts.gitHistorySearch)
	applyDebugPositiveInt(getenv, "DEBUG_ATTELER_GIT_HISTORY_LIMIT", &opts.gitHistoryLimit)
	applyDebugPositiveInt(getenv, "DEBUG_ATTELER_MCP_TIMEOUT_SECONDS", &opts.mcpTimeout)
	applyDebugPositiveInt(getenv, "DEBUG_ATTELER_WATCH_LARGE_FILE_BYTES", &opts.watchLargeFileBytes)
	applyDebugPositiveInt(getenv, "DEBUG_ATTELER_WATCH_INTERVAL_SECONDS", &opts.watchIntervalSeconds)
	applyDebugPositiveInt(getenv, "DEBUG_ATTELER_WATCH_MAX_ITERATIONS", &opts.watchMaxIterations)

	if opts.explainConfigPath != "" {
		opts.explainConfig = true
	}
}

func applyDebugBool(getenv func(string) string, name string, target *bool) {
	if target == nil || *target {
		return
	}

	switch strings.ToLower(strings.TrimSpace(getenv(name))) {
	case "1", affirmativeTrue, affirmativeYes, "on":
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

func initConfig(ctx context.Context, path string) error {
	path = strings.TrimSpace(path)
	if path == "" {
		return errors.New("config path is required")
	}

	if err := authorizeWritePermission(ctx, "write starter config", "atteler.config.init", path); err != nil {
		return err
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

func listConfigPaths(ctx context.Context) error {
	for _, path := range appconfig.DefaultPaths() {
		if err := authorizeReadPermission(ctx, "list config path status", "atteler.config.paths", path); err != nil {
			return fmt.Errorf("list config paths: %w", err)
		}

		fmt.Println(path + "\t" + configPathStatus(path))
	}

	return nil
}

func authorizeConfigStackRead(ctx context.Context, action, source string) error {
	return authorizeReadPermission(ctx, action, source, "default config stack")
}

func authorizeStateFileRead(ctx context.Context, action, source, path string) error {
	return authorizeReadPermission(ctx, action, source, path)
}

func validateConfig(ctx context.Context) error {
	if err := authorizeConfigStackRead(ctx, "validate config", "atteler.config.validate"); err != nil {
		return fmt.Errorf("validate config: %w", err)
	}

	cfg, loaded, _, diagnostics, err := appconfig.LoadWithDiagnostics()
	printDiagnostics(os.Stdout, diagnostics)

	if err != nil {
		return fmt.Errorf("validate config: %w", err)
	}

	if err := configDoctorFatalError(nil, appconfig.InspectPathSources(appconfig.DefaultPathSources())); err != nil {
		return fmt.Errorf("validate config: %w", err)
	}

	if err := validateHookConfig(cfg.Hooks); err != nil {
		return fmt.Errorf("validate config: %w", err)
	}

	if err := validateRoutingConstraints(cfg); err != nil {
		return fmt.Errorf("validate config: %w", err)
	}

	if err := appconfig.ValidateVectorConfigWithAgents(cfg.Vector, cfg.Agents); err != nil {
		return fmt.Errorf("validate config: %w", err)
	}

	if _, err := autonomy.FromConfig(cfg.Autonomy); err != nil {
		return fmt.Errorf("validate config: %w", err)
	}

	if _, err := agentLoopBudgetFromConfig(cfg); err != nil {
		return fmt.Errorf("validate config: %w", err)
	}

	if _, err := agentLoopCheckpointIntervalFromConfig(cfg); err != nil {
		return fmt.Errorf("validate config: %w", err)
	}

	if len(loaded) == 0 {
		fmt.Println("Config valid: no config files loaded.")
		return nil
	}

	fmt.Println("Config valid: " + strings.Join(loaded, ", "))

	return nil
}

func validateRoutingConstraints(cfg appconfig.Config) error {
	providerNames := make([]string, 0, len(cfg.Providers))
	for name := range cfg.Providers {
		providerNames = append(providerNames, name)
	}

	sort.Strings(providerNames)

	for _, name := range providerNames {
		if err := validateProviderRoutingConstraints(name, cfg.Providers[name]); err != nil {
			return err
		}
	}

	roleNames := make([]string, 0, len(cfg.ModelRoles))
	for name := range cfg.ModelRoles {
		roleNames = append(roleNames, name)
	}

	sort.Strings(roleNames)

	for _, name := range roleNames {
		if err := validateModelRoleConstraints(name, cfg.ModelRoles[name]); err != nil {
			return err
		}
	}

	agentNames := make([]string, 0, len(cfg.Agents))
	for name := range cfg.Agents {
		agentNames = append(agentNames, name)
	}

	sort.Strings(agentNames)

	for _, name := range agentNames {
		if err := validateRoutingPolicyConstraints("agents."+name+".routing_policy", cfg.Agents[name].RoutingPolicy); err != nil {
			return err
		}
	}

	return nil
}

func validateProviderRoutingConstraints(name string, provider appconfig.ProviderConfig) error {
	path := "providers." + name
	if err := validateProviderType(name, path+".type", provider.Type); err != nil {
		return err
	}

	if err := validateOpenAICompatibleProviderEndpoint(name, path, provider); err != nil {
		return err
	}

	return validateRouteCapabilityList(path+".capabilities", provider.Capabilities)
}

func validateProviderType(providerName, path, providerType string) error {
	providerName = strings.ToLower(strings.TrimSpace(providerName))
	providerType = strings.TrimSpace(providerType)

	if providerType == "" {
		if providerNameIsBuiltin(providerName) || llm.IsOpenAICompatibleProviderType(providerName) {
			return nil
		}

		return fmt.Errorf(
			"%s missing for custom provider %q (set type: openai_compatible, azure_openai, or a documented OpenAI-compatible alias)",
			path,
			providerName,
		)
	}

	if providerTypeMatchesBuiltinProvider(providerName, providerType) {
		return nil
	}

	if !providerNameIsBuiltin(providerName) && llm.IsOpenAICompatibleProviderType(providerType) {
		return nil
	}

	return fmt.Errorf(
		"%s unsupported provider type %q (supported: openai_compatible, azure_openai, or a documented OpenAI-compatible alias)",
		path,
		providerType,
	)
}

func validateOpenAICompatibleProviderEndpoint(providerName, path string, provider appconfig.ProviderConfig) error {
	if !providerUsesOpenAICompatibleEndpoint(providerName, provider.Type) {
		return nil
	}

	if strings.TrimSpace(provider.BaseURL) != "" {
		return validateOpenAICompatibleProviderPaths(path, provider)
	}

	return fmt.Errorf("%s.base_url missing for OpenAI-compatible provider %q", path, strings.TrimSpace(providerName))
}

func validateOpenAICompatibleProviderPaths(path string, provider appconfig.ProviderConfig) error {
	checks := []struct {
		field string
		value string
	}{
		{field: "chat_completions_path", value: provider.ChatCompletionsPath},
		{field: "embeddings_path", value: provider.EmbeddingsPath},
		{field: "models_path", value: provider.ModelsPath},
	}

	for _, check := range checks {
		value := strings.TrimSpace(check.value)
		if value == "" || strings.HasPrefix(value, "/") {
			continue
		}

		return fmt.Errorf("%s.%s must start with /", path, check.field)
	}

	return nil
}

func providerUsesOpenAICompatibleEndpoint(providerName, providerType string) bool {
	providerName = strings.ToLower(strings.TrimSpace(providerName))
	providerType = strings.TrimSpace(providerType)

	if providerNameIsBuiltin(providerName) {
		return false
	}

	if providerType != "" {
		return llm.IsOpenAICompatibleProviderType(providerType)
	}

	return llm.IsOpenAICompatibleProviderType(providerName)
}

func providerTypeMatchesBuiltinProvider(providerName, providerType string) bool {
	providerName = strings.ToLower(strings.TrimSpace(providerName))
	providerType = strings.ToLower(strings.TrimSpace(providerType))

	switch providerName {
	case providerNameOpenAI:
		return providerType == providerNameOpenAI
	case providerNameAnthropic:
		return providerType == providerNameAnthropic || providerType == providerTypeClaudeAlias
	case ollamaProviderName:
		return providerType == ollamaProviderName
	case providerNameCodex:
		return providerType == providerNameCodex
	case providerNameClaudeCode:
		return providerType == providerNameClaudeCode || providerType == "claude_code"
	default:
		return false
	}
}

func providerNameIsBuiltin(providerName string) bool {
	switch strings.ToLower(strings.TrimSpace(providerName)) {
	case providerNameOpenAI, providerNameAnthropic, ollamaProviderName, providerNameCodex, providerNameClaudeCode:
		return true
	default:
		return false
	}
}

func validateRouteCapabilityList(path string, capabilities []string) error {
	for _, capability := range capabilities {
		capability = strings.ToLower(strings.TrimSpace(capability))
		if capability == "" || modelroute.IsKnownCapability(capability) {
			continue
		}

		return fmt.Errorf(
			"%s contains unknown capability %q (valid: %s)",
			path,
			capability,
			strings.Join(modelroute.KnownCapabilities(), ","),
		)
	}

	return nil
}

func validateModelRoleConstraints(name string, role appconfig.ModelRoleConfig) error {
	prefix := "models." + name
	trimmedName := strings.TrimSpace(name)

	if trimmedName == "" {
		return errors.New("models role name cannot be empty")
	}

	if strings.Contains(trimmedName, "/") {
		return fmt.Errorf("%s role name must be a bare name", prefix)
	}

	if strings.TrimSpace(role.Preferred) == "" && !hasConfiguredModelRoleFallback(role.FallbackModels) {
		return fmt.Errorf("%s needs a preferred model or fallback model", prefix)
	}

	if !isFiniteRouteFloat(role.MaxCostUSD) {
		return fmt.Errorf("%s.max_cost_usd must be finite", prefix)
	}

	if role.MaxCostUSD < 0 {
		return fmt.Errorf("%s.max_cost_usd must be >= 0", prefix)
	}

	if role.MaxLatencyMS < 0 {
		return fmt.Errorf("%s.max_latency_ms must be >= 0", prefix)
	}

	if role.MaxTTFTMS < 0 {
		return fmt.Errorf("%s.max_ttft_ms must be >= 0", prefix)
	}

	if err := validateRouteCapabilityList(prefix+".required_capabilities", role.RequiredCapabilities); err != nil {
		return err
	}

	if err := validateRoutingPolicyConstraints(prefix+".routing_policy", role.RoutingPolicy); err != nil {
		return err
	}

	return nil
}

func hasConfiguredModelRoleFallback(models []string) bool {
	for _, model := range models {
		if strings.TrimSpace(model) != "" {
			return true
		}
	}

	return false
}

func validateRoutingPolicyConstraints(path string, policy appconfig.RoutingPolicyConfig) error {
	if !isFiniteRouteFloat(policy.MaxBudget) {
		return fmt.Errorf("%s.max_budget must be finite", path)
	}

	if policy.MaxBudget < 0 {
		return fmt.Errorf("%s.max_budget must be >= 0", path)
	}

	if policy.MaxLatencyMS < 0 {
		return fmt.Errorf("%s.max_latency_ms must be >= 0", path)
	}

	if policy.MaxTTFTMS < 0 {
		return fmt.Errorf("%s.max_ttft_ms must be >= 0", path)
	}

	if err := validateRouteCapabilityList(path+".required_capabilities", policy.RequiredCapabilities); err != nil {
		return err
	}

	return nil
}

func isFiniteRouteFloat(value float64) bool {
	return !math.IsNaN(value) && !math.IsInf(value, 0)
}

func validateHookConfig(hooks map[string][]appconfig.HookConfig) error {
	for eventType, hookList := range hooks {
		for index, hook := range hookList {
			payload := strings.ToLower(strings.TrimSpace(hook.Payload))
			if payload == "" {
				continue
			}

			switch events.PayloadMode(payload) {
			case events.PayloadMetadata, events.PayloadSummary, events.PayloadFull:
				continue
			default:
				return fmt.Errorf(
					"%s: unknown payload mode (want metadata, summary, or full)",
					hookConfigPayloadPath(eventType, index),
				)
			}
		}
	}

	return nil
}

func hookConfigPayloadPath(eventType string, index int) string {
	for _, supported := range events.SupportedEventTypes() {
		if eventType == supported.Type {
			return fmt.Sprintf("hooks.%s[%d].payload", eventType, index)
		}
	}

	return fmt.Sprintf("hooks.event[%d].payload", index)
}

func warnInvalidHookConfig(hooks map[string][]appconfig.HookConfig) {
	if err := validateHookConfig(hooks); err != nil {
		fmt.Fprintln(os.Stderr, "warning: validate config: "+err.Error())
	}
}

func configPathStatus(path string) string {
	info, err := os.Stat(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return configPathStatusMissing
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

func listKnownModels(ctx context.Context) error {
	providers, err := llm.KnownProvidersContext(ctx)
	if err != nil {
		return fmt.Errorf("list known models: %w", err)
	}

	sort.Slice(providers, func(i, j int) bool {
		return providers[i].Name < providers[j].Name
	})

	for _, provider := range providers {
		sort.Strings(provider.Models)

		for _, model := range provider.Models {
			fmt.Println(provider.Name + "/" + model)
		}
	}

	return nil
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

	if handled, err := runParseControlCommand(opts); handled {
		return err
	}

	if err := validateCLICommandSelection(opts); err != nil {
		return err
	}

	if handled, err := runInlineCommand(ctx, opts); handled {
		return err
	}

	// Phase 1: providerless commands (no LLM registry needed).
	store := session.NewStore(opts.sessionDir)
	if !opts.recoverHeadless {
		reconcileHeadlessRunsAtStartup(ctx, opts, store)
	}

	if handled, err := dispatchProviderless(ctx, opts, store); handled {
		return err
	}

	// Phase 2: providerless-config commands (need config/agents but no LLM).
	if providerlessConfigRequested(opts) {
		state, stateErr := providerlessState(ctx, store, opts)
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
		recordHeadlessLoadStateFailure(ctx, store, opts, err)

		return err
	}

	return runWithState(ctx, opts, state)
}

func reconcileHeadlessRunsAtStartup(ctx context.Context, opts cliOptions, store *session.Store) {
	if store == nil {
		return
	}

	ctx, _, err := contextWithPermissionPolicyFromOptions(ctx, opts)
	if err != nil {
		fmt.Fprintln(os.Stderr, "warning: reconcile headless runs: "+err.Error())

		return
	}

	if err := authorizeHeadlessPermission(ctx, "reconcile headless runs at startup", store, "", permission.OperationRead, permission.OperationWrite); err != nil {
		fmt.Fprintln(os.Stderr, "warning: reconcile headless runs: "+err.Error())

		return
	}

	if _, err := store.RecoverStaleHeadlessRuns(0); err != nil {
		fmt.Fprintln(os.Stderr, "warning: reconcile headless runs: "+err.Error())
	}
}

func shouldReconcileHeadlessRunsAtStartup(opts cliOptions) bool {
	level, err := autonomyForEarlyCommand(opts)
	if err != nil {
		fmt.Fprintln(os.Stderr, "warning: "+err.Error())

		return false
	}

	return autonomy.Normalize(level).Allows(autonomy.ActionFileWrite)
}

func runParseControlCommand(opts cliOptions) (bool, error) {
	switch {
	case opts.parseErr != nil:
		return true, opts.parseErr
	case opts.helpRequested:
		return true, printCLIHelp(os.Stdout, flag.CommandLine, opts.helpDomain)
	default:
		return false, nil
	}
}

// runInlineCommand handles trivial early-exit commands (version, config
// template, etc.) that need no session store or provider.
func runInlineCommand(ctx context.Context, opts cliOptions) (bool, error) {
	if handled, err := runInlineConfigCommand(ctx, opts); handled {
		return true, err
	}

	ctx, _, err := contextWithPermissionPolicyFromOptions(ctx, opts)
	if err != nil {
		return true, err
	}

	switch {
	case opts.listProviders:
		listKnownProviders()
		return true, nil
	case opts.listKnownModels:
		return true, listKnownModels(ctx)
	case opts.ollamaStatus:
		return true, printOllamaStatus(ctx)
	case opts.ollamaStop:
		if err := authorizeEarlyAutonomyAction(opts, autonomy.ActionMutatingShell, "--ollama-stop"); err != nil {
			return true, err
		}

		return true, stopOllamaDaemon(ctx)
	case opts.listWorktrees:
		level, err := autonomyForEarlyCommand(opts)
		if err != nil {
			return true, err
		}

		return true, listWorktrees(ctx, level)
	case opts.mergeWorktreeRef != "":
		level, err := autonomyForEarlyCommand(opts)
		if err != nil {
			return true, err
		}

		return true, mergeWorktreeBySession(ctx, opts.mergeWorktreeRef, worktreeManualMergePolicyFromOptions(opts), level)
	default:
		return false, nil
	}
}

func runInlineConfigCommand(ctx context.Context, opts cliOptions) (bool, error) {
	if handled, err := runPureInlineConfigCommand(ctx, opts); handled {
		return true, err
	}

	return runPermissionedInlineConfigCommand(ctx, opts)
}

func runPureInlineConfigCommand(ctx context.Context, opts cliOptions) (bool, error) {
	switch {
	case opts.printConfigTemplate:
		fmt.Print(appconfig.TemplateYAML())
		return true, nil
	case opts.showVersion:
		fmt.Println(versionString())
		return true, nil
	case opts.commandSurfaceJSON:
		return true, printCommandSurfaceJSON(os.Stdout)
	case opts.commandSurfaceDocs:
		return true, printCommandSurfaceMarkdown(os.Stdout)
	case opts.doctorOffline:
		permissionCtx, _, err := contextWithPermissionPolicyFromOptions(ctx, opts)
		if err != nil {
			return true, err
		}

		return true, doctorOffline(permissionCtx, opts)
	default:
		return false, nil
	}
}

func runPermissionedInlineConfigCommand(ctx context.Context, opts cliOptions) (bool, error) {
	if !permissionedInlineConfigRequested(opts) {
		return false, nil
	}

	permissionCtx, err := inlineConfigPermissionContext(ctx, opts)
	if err != nil {
		return true, err
	}

	switch {
	case opts.initConfigPath != "":
		if err := authorizeEarlyAutonomyAction(opts, autonomy.ActionFileWrite, "--init-config"); err != nil {
			return true, err
		}

		return true, initConfig(permissionCtx, opts.initConfigPath)
	case opts.listConfigPaths:
		return true, listConfigPaths(permissionCtx)
	case opts.validateConfig:
		return true, validateConfig(permissionCtx)
	case opts.configMigrate:
		if err := authorizeEarlyAutonomyAction(opts, autonomy.ActionFileWrite, "--config-migrate"); err != nil {
			return true, err
		}

		return true, migrateConfigAndState(permissionCtx)
	case opts.configReport:
		return true, printConfigReport(permissionCtx)
	case opts.explainConfig:
		return true, explainConfig(permissionCtx, opts)
	default:
		return false, nil
	}
}

func permissionedInlineConfigRequested(opts cliOptions) bool {
	return opts.initConfigPath != "" ||
		opts.listConfigPaths ||
		opts.validateConfig ||
		opts.configMigrate ||
		opts.configReport ||
		opts.explainConfig
}

func inlineConfigPermissionContext(ctx context.Context, opts cliOptions) (context.Context, error) {
	ctx, _, err := contextWithPermissionPolicyFromOptions(ctx, opts)
	if err != nil {
		return ctx, err
	}

	return ctx, nil
}

func authorizeEarlyAutonomyAction(opts cliOptions, action autonomy.Action, contextLabel string) error {
	level, err := autonomyForEarlyCommand(opts)
	if err != nil {
		return err
	}

	if !autonomy.Normalize(level).Allows(action) {
		return fmt.Errorf("%s", autonomy.DenialMessage(level, action, contextLabel))
	}

	return nil
}

func providerlessState(ctx context.Context, store *session.Store, opts cliOptions) (appState, error) {
	cwd, err := os.Getwd()
	if err != nil {
		return appState{}, fmt.Errorf("locate working directory: %w", err)
	}

	ctx, permissionPolicy, err := contextWithPermissionPolicyFromOptions(ctx, opts)
	if err != nil {
		return appState{}, err
	}

	cfg, loadedConfigPaths, err := loadConfigWithPermission(
		ctx,
		"load config for providerless command",
		"atteler.config.load",
		"load providerless config",
	)
	if err != nil {
		return appState{}, err
	}

	autonomyLevel, autonomyErr := autonomyFromConfigOptions(cfg, opts)
	if autonomyErr != nil {
		return appState{}, autonomyErr
	}

	return appState{
		config:             cfg,
		agentRegistry:      agent.NewRegistry(cfg.Agents),
		sessionStore:       store,
		cwd:                cwd,
		loadedConfigPaths:  loadedConfigPaths,
		pluginPaths:        append([]string(nil), cfg.Plugins.Paths...),
		pluginPolicy:       clonePluginPolicy(cfg.Plugins.Policy),
		permissionPolicy:   permissionPolicy,
		vectorConfig:       cfg.Vector,
		promptContextCache: newPromptContextCache(promptContextCachePath(store)),
		autonomy:           autonomyLevel,
	}, nil
}

func loadConfigWithPermission(ctx context.Context, action, source, errorPrefix string) (appconfig.Config, []string, error) {
	if authErr := authorizeConfigStackRead(ctx, action, source); authErr != nil {
		return appconfig.Config{}, nil, fmt.Errorf("%s: %w", errorPrefix, authErr)
	}

	cfg, loadedConfigPaths, err := appconfig.Load()
	if err != nil {
		fmt.Fprintln(os.Stderr, "warning: "+err.Error())
	}

	return cfg, loadedConfigPaths, nil
}

func loadStateWithPermission(
	ctx context.Context,
	stateStore *appconfig.StateStore,
	action, source, errorPrefix string,
) (appconfig.State, error) {
	if authErr := authorizeStateFileRead(ctx, action, source, stateStore.Path()); authErr != nil {
		return appconfig.State{}, fmt.Errorf("%s: %w", errorPrefix, authErr)
	}

	persistedState, err := stateStore.Load()
	if err != nil {
		fmt.Fprintln(os.Stderr, "warning: "+err.Error())
	}

	return persistedState, nil
}

func runWithState(ctx context.Context, opts cliOptions, state appState) error {
	ctx = contextWithPermissionPolicyForOptions(ctx, opts, state.permissionPolicy)

	ctx = contextWithPermissionAuditMetadata(ctx, state.sessionStore, state.sessionState, state.selectedAgent, state.selectedModel)
	defer flushEventObservers(ctx, state.eventObservers)

	if handled, err := dispatchStateful(ctx, opts, state); handled {
		return err
	}

	executionOptions := runOnceExecutionOptionsFromOptions(opts)
	executionOptions.AgentLoopBudget = state.agentLoopBudget
	executionOptions.AgentLoopCheckpointInterval = state.agentLoopCheckpointInterval
	executionOptions.SkillLearningStoreDir = state.skillLearningStoreDir
	executionOptions.SkillLearningSkillDir = state.skillLearningSkillDir
	executionOptions.SkillLearningEnabled = state.skillLearningEnabled
	executionOptions.VectorConfig = state.vectorConfig
	executionOptions.PermissionPolicy = state.permissionPolicy

	if opts.headless && opts.oncePrompt == "" && !opts.readStdin {
		err := errors.New("headless mode requires --once, positional prompt text, or --stdin")
		recordHeadlessPreflightFailure(
			ctx,
			state.sessionStore,
			executionOptions,
			state.sessionState,
			opts.oncePrompt,
			state.selectedModel,
			appStateSessionGeneration(state).ModelMode,
			state.selectedAgent,
			err,
		)

		return err
	}

	if opts.oncePrompt == "" && !opts.readStdin {
		if _, err := normalizeOutputFormat(opts.outputFormat); err != nil {
			return err
		}

		return runInteractive(ctx, state)
	}

	prompt, err := oneShotPrompt(opts.oncePrompt, opts.readStdin)
	if err != nil {
		recordHeadlessPreflightFailure(
			ctx,
			state.sessionStore,
			executionOptions,
			state.sessionState,
			opts.oncePrompt,
			state.selectedModel,
			appStateSessionGeneration(state).ModelMode,
			state.selectedAgent,
			err,
		)

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
		state.referenceManifest,
		state.referenceContextEstimator,
		state.configuredReferences,
		state.selectedModel,
		state.selectedAgent,
		state.fallbackModels,
		state.generationDefaults,
		state.generationOverrides,
		state.maxInputTokens,
		executionOptions,
		state.modelLocked,
		prompt,
	)
	if runErr != nil && state.worktreeInfo != nil && state.autoMergeWorktree {
		fmt.Fprintln(os.Stderr, "worktree: auto-merge skipped because session failed: "+runErr.Error())

		state.autoMergeWorktree = false
	}

	return errors.Join(runErr, finalizeWorktree(ctx, &state))
}

func runOnceExecutionOptionsFromOptions(opts cliOptions) runOnceExecutionOptions {
	autonomyLevel := autonomy.Level("")
	if opts.autonomy.set {
		autonomyLevel = opts.autonomy.value
	}

	return runOnceExecutionOptions{
		Response: responseRecordOptions{
			RecordPath: opts.recordResponsePath,
			ReplayPath: opts.replayResponsePath,
		},
		OutputFormat:       opts.outputFormat,
		Headless:           opts.headless,
		HeadlessID:         opts.headlessID,
		HeadlessPrivateLog: opts.headlessPrivateLog,
		Autonomy:           autonomyLevel,
	}
}

func autonomyFromConfigOptions(cfg appconfig.Config, opts cliOptions) (autonomy.Level, error) {
	level := opts.autonomy.value
	if !opts.autonomy.set {
		resolved, err := autonomy.FromConfig(cfg.Autonomy)
		if err != nil {
			return "", fmt.Errorf("resolve autonomy: %w", err)
		}

		level = resolved
	}

	// Auto mode needs the bash tool to fork worker children, so raise the floor
	// to the tool-allowing default when the resolved level is advisory-only.
	if _, autoRequested := autoModeRequest(opts, cfg); autoRequested && !level.AllowsAgentTools() {
		level = autonomy.Medium
	}

	return level, nil
}

// autoModePlan captures how --auto should be applied for a run.
type autoModePlan struct {
	mode         autopilot.Mode
	active       bool
	downgraded   bool
	currentDepth int
	maxDepth     int
}

// autoModeRequest reports the requested orchestration mode and whether auto
// mode was asked for. The --auto flag wins; otherwise the config `auto:` default
// applies to interactive (non-headless) runs only, so headless one-shots stay
// strictly opt-in.
func autoModeRequest(opts cliOptions, cfg appconfig.Config) (string, bool) {
	if opts.auto.set {
		return opts.auto.value, true
	}

	if !opts.headless {
		if mode := strings.TrimSpace(cfg.Auto); mode != "" {
			return mode, true
		}
	}

	return "", false
}

// resolveAutoModePlan resolves the requested auto mode (from --auto or the
// interactive config default) and reads the current recursion depth. When the
// depth budget is exhausted the plan is marked downgraded so the run proceeds as
// an ordinary single agent instead of forking further.
func resolveAutoModePlan(opts cliOptions, cfg appconfig.Config) (autoModePlan, error) {
	modeName, requested := autoModeRequest(opts, cfg)
	if !requested {
		return autoModePlan{}, nil
	}

	mode, ok := autopilot.ModeByName(modeName)
	if !ok {
		return autoModePlan{}, fmt.Errorf("unknown auto mode %q (known: %s)", modeName, strings.Join(autopilot.ModeNames(), ", "))
	}

	maxDepth := max(opts.autoMaxDepth, 0)
	current := autoDepthFromEnv()
	plan := autoModePlan{mode: mode, currentDepth: current, maxDepth: maxDepth}

	if current >= maxDepth {
		plan.downgraded = true

		return plan, nil
	}

	plan.active = true

	return plan, nil
}

// registerAutopilotWorkers makes the built-in worker personas available to
// every run (including child processes spawned as `atteler --agent explorer`,
// which do not pass --auto). Config-defined agents of the same name win.
func registerAutopilotWorkers(agentRegistry *agent.Registry) {
	workers := autopilot.WorkerAgents()
	for i := range workers {
		if _, exists := agentRegistry.Get(workers[i].Name); !exists {
			agentRegistry.Upsert(workers[i])
		}
	}
}

func autoDepthFromEnv() int {
	raw := strings.TrimSpace(os.Getenv("ATTELER_AUTO_DEPTH"))
	if raw == "" {
		return 0
	}

	depth, err := strconv.Atoi(raw)
	if err != nil || depth < 0 {
		return 0
	}

	return depth
}

// applyAutoMode renders the orchestrator self-fork manual, merges the
// orchestrator and worker personas into the registry, force-selects the
// orchestrator, and propagates the incremented recursion depth to children
// spawned via the bash tool.
func applyAutoMode(plan autoModePlan, agentRegistry *agent.Registry, reg *llm.Registry, selection *selectionState, autonomyLevel autonomy.Level) error {
	if !plan.active {
		return nil
	}

	binary, execErr := os.Executable()
	if execErr != nil || strings.TrimSpace(binary) == "" {
		binary = attelerCommandName
	}

	models := reg.ListModels()
	sort.Strings(models)

	manual := autopilot.RenderSystemPrompt(plan.mode, autopilot.ManualInput{
		BinaryPath:    binary,
		Autonomy:      autonomyLevel.String(),
		WorkerAgents:  autopilot.WorkerAgentNames(),
		Models:        models,
		CLIFlags:      registeredCLIFlagSummaries(),
		CLICommands:   commandSurfaceSummaries(buildCommandSurface(commandRegistry).Domains),
		SlashCommands: slashCommandSurfaceSummaries(commandSurfaceSlashCommands()),
		Tools:         toolDefinitionSummaries(llm.DefaultTools()),
		CurrentDepth:  plan.currentDepth,
		MaxDepth:      plan.maxDepth,
	})

	agentRegistry.Upsert(autopilot.OrchestratorAgent(manual))
	registerAutopilotWorkers(agentRegistry)

	selection.selectedAgent = autopilot.OrchestratorAgentName
	selection.sessionState.DefaultAgent = autopilot.OrchestratorAgentName

	if err := os.Setenv("ATTELER_AUTO_DEPTH", strconv.Itoa(plan.currentDepth+1)); err != nil {
		return fmt.Errorf("set auto depth: %w", err)
	}

	return nil
}

func autonomyForEarlyCommand(opts cliOptions) (autonomy.Level, error) {
	cfg, _, err := appconfig.Load()
	if err != nil {
		fmt.Fprintln(os.Stderr, "warning: "+err.Error())
	}

	return autonomyFromConfigOptions(cfg, opts)
}

func recordHeadlessLoadStateFailure(ctx context.Context, store *session.Store, opts cliOptions, failure error) {
	if failure == nil || !opts.headless {
		return
	}

	if permissionPolicy, err := permissionPolicyFromOptions(opts); err == nil {
		ctx = contextWithPermissionPolicyForOptions(ctx, opts, permissionPolicy)
	}

	autonomyLevel, err := autonomyForEarlyCommand(opts)
	if err != nil {
		fmt.Fprintln(os.Stderr, "warning: "+err.Error())

		return
	}

	sessionState := session.New(opts.model, nil)
	sessionState.DefaultAgent = opts.agentName
	sessionState.Autonomy = autonomyLevel.String()

	executionOptions := runOnceExecutionOptionsFromOptions(opts)
	executionOptions.Autonomy = autonomyLevel

	recordHeadlessPreflightFailure(
		ctx,
		store,
		executionOptions,
		sessionState,
		opts.oncePrompt,
		opts.model,
		opts.modelMode,
		opts.agentName,
		failure,
	)
}

//nolint:cyclop // App-state bootstrapping deliberately centralizes config, hooks, selection, and worktree setup.
func loadAppState(ctx context.Context, opts cliOptions) (appState, error) {
	ctx, permissionPolicy, err := contextWithPermissionPolicyFromOptions(ctx, opts)
	if err != nil {
		return appState{}, err
	}

	if authErr := authorizeConfigStackRead(ctx, "load config", "atteler.config.load"); authErr != nil {
		return appState{}, fmt.Errorf("load config: %w", authErr)
	}

	cfg, loadedConfigPaths, configFatalErr := loadConfigForAppState(opts)
	agentRegistry := agent.NewRegistry(cfg.Agents)
	registerAutopilotWorkers(agentRegistry)

	store := session.NewStore(opts.sessionDir)

	autoPlan, err := resolveAutoModePlan(opts, cfg)
	if err != nil {
		return appState{}, err
	}

	if autoPlan.downgraded {
		fmt.Fprintf(os.Stderr, "warning: --auto suppressed at recursion depth %d (limit %d); running a single agent\n", autoPlan.currentDepth, autoPlan.maxDepth)
	}

	stateStore := appconfig.NewStateStore("")
	cwd := currentWorkingDirectoryOrEmpty()

	if configFatalErr != nil && opts.doctor {
		return appState{
			config:             cfg,
			agentRegistry:      agentRegistry,
			sessionStore:       store,
			stateStore:         stateStore,
			cwd:                cwd,
			loadedConfigPaths:  loadedConfigPaths,
			configLoadErr:      configFatalErr,
			pluginPaths:        append([]string(nil), cfg.Plugins.Paths...),
			pluginPolicy:       clonePluginPolicy(cfg.Plugins.Policy),
			permissionPolicy:   permissionPolicy,
			promptContextCache: newPromptContextCache(promptContextCachePath(store)),
			vectorConfig:       cfg.Vector,
		}, nil
	}

	warnInvalidHookConfig(cfg.Hooks)

	autonomyLevel, err := autonomyFromConfigOptions(cfg, opts)
	if err != nil {
		return appState{}, err
	}

	// Default to a stderr logger so events from utility commands (--bash,
	// --mcp, one-shot, etc.) are visible without extra configuration. Headless
	// runs stay quiet so JSON output isn't polluted; runInteractive replaces
	// this with a logger-less runner so stderr writes don't bleed onto the TUI.
	hookLogWriter := hookLogWriterForOptions(opts)

	hookLedger, err := events.NewFileLedger(cfg.EventLedgerPath)
	if err != nil {
		return appState{}, fmt.Errorf("open event ledger: %w", err)
	}

	skillLearningOpts, configuredSkillLearningEnabled := skillLearningOptionsFromConfig(cfg, opts, os.Getenv)
	skillLearningEnabled := skillLearningEffectiveEnabled(skillLearningOpts, configuredSkillLearningEnabled)

	persistedState, err := loadStateWithPermission(ctx, stateStore, "load persisted state", "atteler.state.load", "load persisted state")
	if err != nil {
		return appState{}, err
	}

	warnInvalidHookConfig(cfg.Hooks)

	selection, err := resolveSelection(ctx, opts, cfg, persistedState.ModelForFolder(cwd), agentRegistry, store)
	if err != nil {
		return appState{}, err
	}

	ctx = contextWithPermissionAuditMetadata(ctx, store, selection.sessionState, selection.selectedAgent, selection.selectedModel)

	skillLearningEnabled = skillLearningEnabledForAutonomy(skillLearningEnabled, autonomyLevel)
	eventObservers := skillLearningObserversFromOptions(ctx, skillLearningOpts, skillLearningEnabled)
	hookRunner := events.NewRunnerWithOptions(cfg.Hooks, events.RunnerOptions{
		DeliveryContext: events.DetachContext(ctx),
		LogWriter:       hookLogWriter,
		Ledger:          hookLedger,
		Observers:       eventObservers,
	}).WithAutonomy(autonomyLevel)

	ledgerTransferred := false
	defer closeHookRunnerUnlessTransferred(ctx, hookRunner, &ledgerTransferred)

	reg, providerReadiness := autoRegisterForOptions(
		ctx,
		opts,
		cfg,
		selection.selectedModel,
		selection.fallbackModels,
		selection.sessionState.ID,
		autonomyLevel,
	)

	if autoErr := applyAutoMode(autoPlan, agentRegistry, reg, &selection, autonomyLevel); autoErr != nil {
		return appState{}, autoErr
	}

	contextOptions := contextOptionsFromConfig(cfg)
	contextOptions = contextOptionsForRequestModels(contextOptions, reg, selection.selectedModel, selection.fallbackModels)
	generationDefaults := generationFromConfig(cfg)
	generationOverrides := generationOverridesFromState(opts, selection, persistedState, cwd)

	maxInputTokens := maxInputTokensFromConfigOptions(cfg, opts)

	agentLoopBudget, err := agentLoopBudgetFromConfig(cfg)
	if err != nil {
		return appState{}, err
	}

	agentLoopCheckpointInterval, err := agentLoopCheckpointIntervalFromConfig(cfg)
	if err != nil {
		return appState{}, err
	}

	worktreePolicy := worktreeMergePolicyFromConfigOptions(cfg, opts)

	providers := reg.ListProviders()
	if len(providers) == 0 && !opts.headless && !opts.doctor {
		fmt.Fprintln(os.Stderr, "warning: no LLM providers configured, set ANTHROPIC_API_KEY or OPENAI_API_KEY")
	}

	wtInfo, err := setupWorktreeIfRequested(ctx, opts, cwd, &selection, worktreePolicy, autonomyLevel)
	if err != nil {
		return appState{}, err
	}

	if wtInfo != nil {
		contextOptions.Root = wtInfo.Path
	}

	referenceContext := loadConfiguredReferenceContext(ctx, cfg.Context.References, contextOptions)
	selection.sessionState.AgentLoopBudget = agentLoopBudget
	selection.sessionState.Autonomy = autonomyLevel.String()
	suggestionConsent := promptSuggestionConsentFromPreferences(
		opts.promptLocalOnly,
		selection.sessionState.PromptSuggestions,
		persistedState.ResolvePromptSuggestionPreference(cwd),
	)

	state := appState{
		config:                       cfg,
		registry:                     reg,
		providerReadiness:            providerReadiness,
		agentRegistry:                agentRegistry,
		hookRunner:                   hookRunner,
		eventObservers:               eventObservers,
		sessionStore:                 store,
		stateStore:                   stateStore,
		contextOptions:               contextOptions,
		configuredReferences:         append([]string(nil), cfg.Context.References...),
		referenceContext:             referenceContext.Content,
		referenceManifest:            referenceContext.Manifest,
		referenceContextEstimator:    referenceContext.Estimator,
		skillLearningStoreDir:        skillLearningOpts.StoreDir,
		skillLearningSkillDir:        skillLearningOpts.SkillDir,
		sessionState:                 selection.sessionState,
		worktreeInfo:                 wtInfo,
		cwd:                          cwd,
		loadedConfigPaths:            loadedConfigPaths,
		providers:                    providers,
		selectedModel:                selection.selectedModel,
		selectedAgent:                selection.selectedAgent,
		promptSuggestionConsent:      suggestionConsent,
		configLoadErr:                configFatalErr,
		idleSuggestionBudget:         defaultIdleSuggestionBudget(),
		fallbackModels:               selection.fallbackModels,
		pluginPaths:                  append([]string(nil), cfg.Plugins.Paths...),
		pluginPolicy:                 clonePluginPolicy(cfg.Plugins.Policy),
		permissionPolicy:             permissionPolicy,
		promptContextCache:           newPromptContextCache(promptContextCachePath(store)),
		generationDefaults:           generationDefaults,
		generationOverrides:          generationOverrides,
		agentLoopBudget:              agentLoopBudget,
		agentLoopCheckpointInterval:  agentLoopCheckpointInterval,
		autonomy:                     autonomyLevel,
		maxInputTokens:               maxInputTokens,
		hookConfig:                   cfg.Hooks,
		vectorConfig:                 cfg.Vector,
		modelLocked:                  selection.modelLocked,
		autoMergeWorktree:            opts.useWorktree && worktreePolicy.AutoMerge,
		worktreeMergeOverride:        worktreePolicy.OverrideVerification,
		worktreeVerificationCommands: worktreePolicy.VerificationCommands,
		promptLocalOnly:              opts.promptLocalOnly,
		skillLearningEnabled:         skillLearningEnabled,
	}

	ledgerTransferred = true

	return state, nil
}

func closeHookRunnerUnlessTransferred(ctx context.Context, hookRunner *events.Runner, transferred *bool) {
	if transferred != nil && *transferred {
		return
	}

	closeHookRunner(ctx, hookRunner)
}

func closeHookRunner(ctx context.Context, hookRunner *events.Runner) {
	if hookRunner == nil {
		return
	}

	if ctx == nil {
		fmt.Fprintln(os.Stderr, "warning: events: context is required")
		return
	}

	closeCtx, cancel := context.WithTimeout(events.DetachContext(ctx), hookShutdownTimeout)
	defer cancel()

	if err := hookRunner.Close(closeCtx); err != nil {
		fmt.Fprintln(os.Stderr, "warning: "+err.Error())
	}
}

func loadConfigForAppState(opts cliOptions) (appconfig.Config, []string, error) {
	cfg, loadedConfigPaths, loadErr := appconfig.Load()
	if loadErr != nil && !opts.doctor {
		fmt.Fprintln(os.Stderr, "warning: "+loadErr.Error())
	}

	return cfg, loadedConfigPaths, configFatalErrorForOptions(opts, loadErr)
}

func configFatalErrorForOptions(opts cliOptions, loadErr error) error {
	if !opts.doctor || loadErr != nil {
		return loadErr
	}

	return configDoctorFatalError(nil, appconfig.InspectPathSources(appconfig.DefaultPathSources()))
}

func hookLogWriterForOptions(opts cliOptions) io.Writer {
	if opts.headless {
		return nil
	}

	return os.Stderr
}

func currentWorkingDirectoryOrEmpty() string {
	cwd, err := os.Getwd()
	if err != nil {
		return ""
	}

	return cwd
}

type cliWorktreeMergePolicy struct {
	VerificationCommands []string
	AutoMerge            bool
	OverrideVerification bool
	AllowBaseMismatch    bool
}

func worktreeMergePolicyFromConfigOptions(cfg appconfig.Config, opts cliOptions) cliWorktreeMergePolicy {
	autoMerge := false
	if cfg.Worktree.AutoMerge != nil {
		autoMerge = *cfg.Worktree.AutoMerge
	}

	if opts.worktreeAutoMerge {
		autoMerge = true
	}

	if opts.noAutoMerge {
		autoMerge = false
	}

	commands := make([]string, 0, len(cfg.Worktree.VerificationCommands)+len(opts.worktreeVerificationCommands))
	commands = append(commands, cfg.Worktree.VerificationCommands...)
	commands = append(commands, opts.worktreeVerificationCommands...)

	return cliWorktreeMergePolicy{
		AutoMerge:            autoMerge,
		VerificationCommands: cleanCLIStrings(commands),
		OverrideVerification: cfg.Worktree.OverrideVerification || opts.worktreeMergeOverride,
	}
}

func validateWorktreeAutoMergePolicy(policy cliWorktreeMergePolicy, _ ...autonomy.Level) error {
	if !policy.AutoMerge {
		return nil
	}

	if len(policy.VerificationCommands) > 0 || policy.OverrideVerification {
		return nil
	}

	return errors.New("worktree auto-merge requires at least one --worktree-verify-command or worktree.verification_commands entry; use --worktree-merge-override only for an explicit no-verification override")
}

func worktreeManualMergePolicyFromOptions(opts cliOptions) cliWorktreeMergePolicy {
	commands := cleanCLIStrings(opts.worktreeVerificationCommands)

	return cliWorktreeMergePolicy{
		VerificationCommands: commands,
		OverrideVerification: opts.worktreeMergeOverride || len(commands) == 0,
		AllowBaseMismatch:    opts.mergeWorktreeAllowBaseMismatch,
	}
}

func cleanCLIStrings(values []string) []string {
	cleaned := make([]string, 0, len(values))
	for _, value := range values {
		value = strings.TrimSpace(value)
		if value != "" {
			cleaned = append(cleaned, value)
		}
	}

	return cleaned
}

func setupWorktreeIfRequested(
	ctx context.Context,
	opts cliOptions,
	cwd string,
	selection *selectionState,
	policy cliWorktreeMergePolicy,
	autonomyLevel autonomy.Level,
) (*worktree.Info, error) {
	if !opts.useWorktree || cwd == "" {
		return nil, nil
	}

	if !autonomy.Normalize(autonomyLevel).Allows(autonomy.ActionBranch) {
		return nil, fmt.Errorf("%s", autonomy.DenialMessage(autonomyLevel, autonomy.ActionBranch, "--worktree"))
	}

	if err := validateWorktreeAutoMergePolicy(policy); err != nil {
		return nil, err
	}

	// If continuing a session that already has a worktree, re-use it.
	if selection.sessionState.WorktreePath != "" {
		wtInfo := &worktree.Info{
			Path:       selection.sessionState.WorktreePath,
			Branch:     selection.sessionState.WorktreeBranch,
			BaseBranch: selection.sessionState.WorktreeBase,
			SessionID:  selection.sessionState.ID,
		}
		fmt.Fprintln(os.Stderr, "worktree: reusing "+wtInfo.Path)

		return wtInfo, nil
	}

	wtInfo, err := worktree.CreateContext(
		worktree.WithAuditContext(ctx, worktreeShellAuditContext(selection.sessionState, autonomyLevel)),
		cwd,
		selection.sessionState.ID,
	)
	if err != nil {
		return nil, fmt.Errorf("worktree setup: %w", err)
	}

	selection.sessionState.WorktreePath = wtInfo.Path
	selection.sessionState.WorktreeBranch = wtInfo.Branch
	selection.sessionState.WorktreeBase = wtInfo.BaseBranch
	fmt.Fprintln(os.Stderr, "worktree: created "+wtInfo.Path+" (branch "+wtInfo.Branch+")")

	return wtInfo, nil
}

func autoRegisterForOptions(
	ctx context.Context,
	opts cliOptions,
	cfg appconfig.Config,
	selectedModel string,
	fallbackModels []string,
	sessionID string,
	autonomyLevel autonomy.Level,
) (*llm.Registry, llm.ProviderReadinessReport) {
	regCfg := llmConfig(
		cfg,
		providerRegistrationSelectedModel(opts, selectedModel),
		fallbackModels,
		sessionID,
		os.Args,
	)

	if shouldDisableProviderAutoStart(opts, autonomyLevel) {
		regCfg.DisableAutoStart = true
	}

	if opts.headless {
		regCfg.Logger = slog.New(slog.NewTextHandler(io.Discard, nil))
	}

	return llm.AutoRegisterWithConfigContextReport(ctx, regCfg)
}

func shouldDisableProviderAutoStart(opts cliOptions, level autonomy.Level) bool {
	return providerInspectionUtilityRequested(opts) ||
		!autonomy.Normalize(level).Allows(autonomy.ActionMutatingShell)
}

func providerRegistrationSelectedModel(opts cliOptions, selectedModel string) string {
	if strings.TrimSpace(opts.explainModelResolution) != "" {
		return opts.explainModelResolution
	}

	return selectedModel
}
