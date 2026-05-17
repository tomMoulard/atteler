package main

import (
	"context"

	atteval "github.com/tommoulard/atteler/pkg/eval"
	"github.com/tommoulard/atteler/pkg/session"
)

// commandTier controls at which phase of the dispatch pipeline a command
// runs. Providerless commands run before loadAppState (no LLM registry
// needed); stateful commands run after a full appState is available.
type commandTier int

const (
	// tierInline commands are handled directly in run() before the registry
	// is consulted (version, config template, etc.).  They are listed here
	// only for documentation; the registry does not store them.
	_ commandTier = iota

	// tierProviderless commands need only a session.Store and cwd -- no LLM
	// providers.  They run before loadAppState to keep startup fast.
	tierProviderless

	// tierProviderlessConfig commands need a session.Store plus loaded
	// config (agent registry, plugin paths) but still no LLM provider.
	tierProviderlessConfig

	// tierStateful commands require a fully initialized appState including
	// an LLM registry and hook runner.
	tierStateful
)

// command describes a single dispatchable CLI action.
type command struct {
	// match returns true when the user's flags indicate this command should
	// run.  Commands are checked in registration order; the first match wins.
	match func(cliOptions) bool

	// runProviderless is called for tierProviderless commands.
	runProviderless func(ctx context.Context, opts cliOptions, store *session.Store) error

	// runProviderlessConfig is called for tierProviderlessConfig commands.
	runProviderlessConfig func(ctx context.Context, opts cliOptions, state appState) error

	// runStateful is called for tierStateful commands.
	runStateful func(ctx context.Context, opts cliOptions, state appState) error

	// name is a human-readable label for debugging / logging.
	name string

	// tier controls when in the dispatch pipeline this command runs.
	tier commandTier
}

// commandRegistry is the ordered list of all CLI commands.  The first command
// whose match function returns true wins.  The order within each tier matters
// for compound conditions (e.g. MCP invoke before MCP manifest).
//
//nolint:gochecknoglobals // registry is initialized once at program start and never mutated.
var commandRegistry = buildCommandRegistry()

func buildCommandRegistry() []command {
	return []command{
		// ---------------------------------------------------------------
		// Tier: providerless -- session inventory
		// ---------------------------------------------------------------
		{
			name: "list-hook-events",
			tier: tierProviderless,
			match: func(o cliOptions) bool {
				return o.listHookEvents || o.listHookEventsJSON
			},
			runProviderless: func(_ context.Context, o cliOptions, _ *session.Store) error {
				return listHookEvents(o.listHookEventsJSON)
			},
		},
		{
			name:  "list-headless",
			tier:  tierProviderless,
			match: func(o cliOptions) bool { return o.listHeadless },
			runProviderless: func(_ context.Context, _ cliOptions, s *session.Store) error {
				return listHeadlessRuns(s)
			},
		},
		{
			name:  "stream-headless",
			tier:  tierProviderless,
			match: func(o cliOptions) bool { return o.streamHeadlessID != "" },
			runProviderless: func(ctx context.Context, o cliOptions, s *session.Store) error {
				return streamHeadlessLog(ctx, s, o.streamHeadlessID)
			},
		},
		{
			name:  "list-sessions",
			tier:  tierProviderless,
			match: func(o cliOptions) bool { return o.listSessions },
			runProviderless: func(_ context.Context, o cliOptions, s *session.Store) error {
				return listSessions(s, o.listSessionsTag)
			},
		},
		{
			name:  "list-session-tags",
			tier:  tierProviderless,
			match: func(o cliOptions) bool { return o.listSessionTags },
			runProviderless: func(_ context.Context, _ cliOptions, s *session.Store) error {
				return listSessionTags(s)
			},
		},
		{
			name:  "agent-performance-summary",
			tier:  tierProviderless,
			match: func(o cliOptions) bool { return o.agentPerformanceSummary },
			runProviderless: func(_ context.Context, _ cliOptions, s *session.Store) error {
				return listAgentPerformance(s)
			},
		},
		{
			name:  "search-sessions",
			tier:  tierProviderless,
			match: func(o cliOptions) bool { return o.searchQuery != "" },
			runProviderless: func(_ context.Context, o cliOptions, s *session.Store) error {
				return searchSessions(s, o.searchQuery)
			},
		},

		// ---------------------------------------------------------------
		// Tier: providerless -- file utilities
		// ---------------------------------------------------------------
		{
			name:  "task-command",
			tier:  tierProviderless,
			match: taskCommandRequested,
			runProviderless: func(ctx context.Context, o cliOptions, s *session.Store) error {
				return runTaskListCommand(ctx, s, o)
			},
		},
		{
			name:  "eval-output",
			tier:  tierProviderless,
			match: func(o cliOptions) bool { return o.evalOutputPath != "" },
			runProviderless: func(_ context.Context, o cliOptions, _ *session.Store) error {
				return evalOutput(o.evalOutputPath, o.evalExpected, o.evalExpectedPath, atteval.MatchMode(o.evalMode))
			},
		},
		{
			name:  "context-pack",
			tier:  tierProviderless,
			match: func(o cliOptions) bool { return o.contextPackPath != "" },
			runProviderless: func(_ context.Context, o cliOptions, _ *session.Store) error {
				return runContextPack(o.contextPackPath, o.contextPackTokens.value)
			},
		},
		{
			name: "lsp-symbols",
			tier: tierProviderless,
			match: func(o cliOptions) bool {
				return o.lspSymbols || o.lspWorkspaceSymbols != ""
			},
			runProviderless: func(ctx context.Context, o cliOptions, _ *session.Store) error {
				return runLSPSymbols(ctx, o)
			},
		},
		{
			name: "memory-command",
			tier: tierProviderless,
			match: func(o cliOptions) bool {
				return o.memorySearch != "" || len(o.memoryIndexFiles) > 0
			},
			runProviderless: func(_ context.Context, o cliOptions, s *session.Store) error {
				return runMemoryCommand(s, o)
			},
		},
		{
			name: "vector-search",
			tier: tierProviderless,
			match: func(o cliOptions) bool {
				return o.vectorSearch != "" || len(o.vectorIndexFiles) > 0
			},
			runProviderless: func(_ context.Context, o cliOptions, _ *session.Store) error {
				return runVectorSearch(o.vectorSearch, o.vectorIndexFiles, o.vectorLimit.value)
			},
		},

		// ---------------------------------------------------------------
		// Tier: providerless -- planning utilities
		// ---------------------------------------------------------------
		// MCP invoke must precede MCP manifest (compound condition).
		{
			name: "mcp-invoke",
			tier: tierProviderless,
			match: func(o cliOptions) bool {
				return o.mcpManifestPath != "" && (o.mcpMethod != "" || o.mcpToolName != "")
			},
			runProviderless: func(ctx context.Context, o cliOptions, _ *session.Store) error {
				return runMCPInvoke(ctx, o)
			},
		},
		{
			name:  "mcp-manifest",
			tier:  tierProviderless,
			match: func(o cliOptions) bool { return o.mcpManifestPath != "" },
			runProviderless: func(_ context.Context, o cliOptions, _ *session.Store) error {
				return runMCPManifest(o.mcpManifestPath, o.mcpCapability)
			},
		},
		{
			name:  "speculate-plan",
			tier:  tierProviderless,
			match: func(o cliOptions) bool { return o.speculatePlan },
			runProviderless: func(_ context.Context, o cliOptions, _ *session.Store) error {
				return runSpeculatePlan(o.speculateAgents, o.speculateGates, o.speculatePrompt)
			},
		},
		{
			name:  "review-plan",
			tier:  tierProviderless,
			match: func(o cliOptions) bool { return o.reviewPlan },
			runProviderless: func(_ context.Context, o cliOptions, _ *session.Store) error {
				return runReviewPlan(o.reviewAgents, o.reviewPaths, o.reviewGates)
			},
		},
		{
			name:  "async-plan",
			tier:  tierProviderless,
			match: func(o cliOptions) bool { return o.asyncPlan },
			runProviderless: func(_ context.Context, o cliOptions, _ *session.Store) error {
				return runAsyncPlan(o.asyncTaskSpecs)
			},
		},
		{
			name: "route-models-providerless",
			tier: tierProviderless,
			match: func(o cliOptions) bool {
				return len(o.routeCandidates) > 0 && o.oncePrompt == "" && !o.readStdin
			},
			runProviderless: func(_ context.Context, o cliOptions, _ *session.Store) error {
				return runRouteModels(o)
			},
		},
		{
			name:  "suggest-skill",
			tier:  tierProviderless,
			match: func(o cliOptions) bool { return len(o.suggestSkillSteps) > 0 },
			runProviderless: func(_ context.Context, o cliOptions, _ *session.Store) error {
				return suggestSkill(o.suggestSkillSteps, o.skillMaxSteps.value, o.skillMinOccurrences.value, o.skillSaveDir)
			},
		},

		// ---------------------------------------------------------------
		// Tier: providerless config -- agents, plugins, prompt-complete
		// ---------------------------------------------------------------
		{
			name:  "list-agents",
			tier:  tierProviderlessConfig,
			match: func(o cliOptions) bool { return o.listAgents },
			runProviderlessConfig: func(_ context.Context, _ cliOptions, s appState) error {
				listAgents(s.agentRegistry)
				return nil
			},
		},
		{
			name:  "describe-agent",
			tier:  tierProviderlessConfig,
			match: func(o cliOptions) bool { return o.describeAgentName != "" },
			runProviderlessConfig: func(_ context.Context, o cliOptions, s appState) error {
				return describeAgent(s.agentRegistry, o.describeAgentName)
			},
		},
		{
			name:  "plan-agents-providerless",
			tier:  tierProviderlessConfig,
			match: func(o cliOptions) bool { return o.planAgentsPrompt != "" },
			runProviderlessConfig: func(_ context.Context, o cliOptions, s appState) error {
				return planAgents(s.agentRegistry, o.planAgentsPrompt, o.planAgentNames, o.planMaxAgents.value)
			},
		},
		{
			name:  "prompt-complete-providerless",
			tier:  tierProviderlessConfig,
			match: func(o cliOptions) bool { return o.promptCompleteInput != "" },
			runProviderlessConfig: func(_ context.Context, o cliOptions, s appState) error {
				promptComplete(s.agentRegistry, o.promptCompleteInput, o.promptCompleteLimit.value)
				return nil
			},
		},
		{
			name:  "list-plugins",
			tier:  tierProviderlessConfig,
			match: func(o cliOptions) bool { return o.listPlugins },
			runProviderlessConfig: func(_ context.Context, _ cliOptions, s appState) error {
				return listPlugins(s.pluginPaths)
			},
		},
		{
			name:  "describe-plugin",
			tier:  tierProviderlessConfig,
			match: func(o cliOptions) bool { return o.describePluginName != "" },
			runProviderlessConfig: func(_ context.Context, o cliOptions, s appState) error {
				return describePlugin(s.pluginPaths, o.describePluginName)
			},
		},
		{
			name:  "run-plugin-providerless",
			tier:  tierProviderlessConfig,
			match: func(o cliOptions) bool { return o.runPluginTarget != "" },
			runProviderlessConfig: func(ctx context.Context, o cliOptions, s appState) error {
				return runPluginEntrypoint(ctx, s.pluginPaths, o.runPluginTarget, o.pluginEntrypoint, o.pluginDryRun, o.pluginTimeout.value)
			},
		},

		// ---------------------------------------------------------------
		// Tier: providerless config -- code analysis
		// ---------------------------------------------------------------
		codeSymbolCmd("code-symbol-name", func(o cliOptions) bool { return o.codeSymbolName != "" },
			func(cwd string, o cliOptions) error { return findCodeSymbol(cwd, o.codeSymbolName) }),
		codeSymbolCmd("code-symbol-file-summary", func(o cliOptions) bool { return o.codeSymbolFileSummary != "" },
			func(cwd string, o cliOptions) error {
				return listCodeSymbolNameFileSummary(cwd, o.codeSymbolFileSummary)
			}),
		codeSymbolCmd("code-symbol-package-summary", func(o cliOptions) bool { return o.codeSymbolPackageSummary != "" },
			func(cwd string, o cliOptions) error {
				return listCodeSymbolNamePackageSummary(cwd, o.codeSymbolPackageSummary)
			}),
		codeSymbolCmd("code-symbol-prefix", func(o cliOptions) bool { return o.codeSymbolPrefix != "" },
			func(cwd string, o cliOptions) error { return findCodeSymbolPrefix(cwd, o.codeSymbolPrefix) }),
		codeSymbolCmd("code-symbol-prefix-file-summary", func(o cliOptions) bool { return o.codeSymbolPrefixFileSummary != "" },
			func(cwd string, o cliOptions) error {
				return listCodeSymbolPrefixFileSummary(cwd, o.codeSymbolPrefixFileSummary)
			}),
		codeSymbolCmd("code-symbol-prefix-package-summary", func(o cliOptions) bool { return o.codeSymbolPrefixPackageSummary != "" },
			func(cwd string, o cliOptions) error {
				return listCodeSymbolPrefixPackageSummary(cwd, o.codeSymbolPrefixPackageSummary)
			}),
		codeSymbolCmd("code-symbol-kind", func(o cliOptions) bool { return o.codeSymbolKind != "" },
			func(cwd string, o cliOptions) error { return findCodeSymbolsByKind(cwd, o.codeSymbolKind) }),
		codeSymbolCmd("code-symbol-kind-file-summary", func(o cliOptions) bool { return o.codeSymbolKindFileSummary != "" },
			func(cwd string, o cliOptions) error {
				return listCodeSymbolKindFileSummary(cwd, o.codeSymbolKindFileSummary)
			}),
		codeSymbolCmd("code-symbol-kind-package-summary", func(o cliOptions) bool { return o.codeSymbolKindPackageSummary != "" },
			func(cwd string, o cliOptions) error {
				return listCodeSymbolKindPackageSummary(cwd, o.codeSymbolKindPackageSummary)
			}),
		codeSymbolCmd("list-code-symbol-summary", func(o cliOptions) bool { return o.listCodeSymbolSummary },
			func(cwd string, _ cliOptions) error { return listCodeSymbolSummary(cwd) }),
		codeSymbolCmd("list-code-symbol-file-summary", func(o cliOptions) bool { return o.listCodeSymbolFileSummary },
			func(cwd string, _ cliOptions) error { return listCodeSymbolFileSummary(cwd) }),

		// Code imports
		codeSymbolCmd("list-code-imports", func(o cliOptions) bool { return o.listCodeImports },
			func(cwd string, _ cliOptions) error { return listCodeImports(cwd) }),
		codeSymbolCmd("list-code-import-summary", func(o cliOptions) bool { return o.listCodeImportSummary },
			func(cwd string, _ cliOptions) error { return listCodeImportSummary(cwd) }),
		codeSymbolCmd("list-code-import-file-summary", func(o cliOptions) bool { return o.listCodeImportFileSummary },
			func(cwd string, _ cliOptions) error { return listCodeImportFileSummary(cwd) }),
		codeSymbolCmd("code-import-path", func(o cliOptions) bool { return o.codeImportPath != "" },
			func(cwd string, o cliOptions) error { return listCodeImportPath(cwd, o.codeImportPath) }),
		codeSymbolCmd("code-import-path-summary", func(o cliOptions) bool { return o.codeImportPathSummary != "" },
			func(cwd string, o cliOptions) error { return listCodeImportPathSummary(cwd, o.codeImportPathSummary) }),
		codeSymbolCmd("code-import-path-file-summary", func(o cliOptions) bool { return o.codeImportPathFileSummary != "" },
			func(cwd string, o cliOptions) error {
				return listCodeImportPathFileSummary(cwd, o.codeImportPathFileSummary)
			}),
		codeSymbolCmd("code-import-path-package-summary", func(o cliOptions) bool { return o.codeImportPathPackageSummary != "" },
			func(cwd string, o cliOptions) error {
				return listCodeImportPathPackageSummary(cwd, o.codeImportPathPackageSummary)
			}),
		codeSymbolCmd("code-import-prefix", func(o cliOptions) bool { return o.codeImportPrefix != "" },
			func(cwd string, o cliOptions) error { return listCodeImportPrefix(cwd, o.codeImportPrefix) }),
		codeSymbolCmd("code-import-prefix-summary", func(o cliOptions) bool { return o.codeImportPrefixSummary != "" },
			func(cwd string, o cliOptions) error {
				return listCodeImportPrefixSummary(cwd, o.codeImportPrefixSummary)
			}),
		codeSymbolCmd("code-import-prefix-file-summary", func(o cliOptions) bool { return o.codeImportPrefixFileSummary != "" },
			func(cwd string, o cliOptions) error {
				return listCodeImportPrefixFileSummary(cwd, o.codeImportPrefixFileSummary)
			}),
		codeSymbolCmd("code-import-prefix-package-summary", func(o cliOptions) bool { return o.codeImportPrefixPackageSummary != "" },
			func(cwd string, o cliOptions) error {
				return listCodeImportPrefixPackageSummary(cwd, o.codeImportPrefixPackageSummary)
			}),

		// Code packages
		codeSymbolCmd("list-code-packages", func(o cliOptions) bool { return o.listCodePackages },
			func(cwd string, _ cliOptions) error { return listCodePackages(cwd) }),
		codeSymbolCmd("code-package-name", func(o cliOptions) bool { return o.codePackageName != "" },
			func(cwd string, o cliOptions) error { return listCodePackageFiles(cwd, o.codePackageName) }),
		codeSymbolCmd("list-code-package-import-summary", func(o cliOptions) bool { return o.listCodePackageImportSummary },
			func(cwd string, _ cliOptions) error { return listCodePackageImportSummary(cwd) }),
		codeSymbolCmd("code-package-imports", func(o cliOptions) bool { return o.codePackageImports != "" },
			func(cwd string, o cliOptions) error { return listCodePackageImports(cwd, o.codePackageImports) }),
		codeSymbolCmd("code-package-import-path", func(o cliOptions) bool { return o.codePackageImportPath != "" },
			func(cwd string, o cliOptions) error { return listCodePackageImportPath(cwd, o.codePackageImportPath) }),
		codeSymbolCmd("code-package-import-files", func(o cliOptions) bool { return o.codePackageImportFiles != "" },
			func(cwd string, o cliOptions) error { return listCodePackageImportFiles(cwd, o.codePackageImportFiles) }),
		codeSymbolCmd("code-package-import-path-file-summary", func(o cliOptions) bool { return o.codePackageImportPathFileSummary != "" },
			func(cwd string, o cliOptions) error {
				return listCodePackageImportPathFileSummary(cwd, o.codePackageImportPathFileSummary)
			}),
		codeSymbolCmd("code-package-import-prefix", func(o cliOptions) bool { return o.codePackageImportPrefix != "" },
			func(cwd string, o cliOptions) error {
				return listCodePackageImportPrefix(cwd, o.codePackageImportPrefix)
			}),
		codeSymbolCmd("code-package-import-prefix-files", func(o cliOptions) bool { return o.codePackageImportPrefixFiles != "" },
			func(cwd string, o cliOptions) error {
				return listCodePackageImportPrefixFiles(cwd, o.codePackageImportPrefixFiles)
			}),
		codeSymbolCmd("code-package-import-prefix-file-summary", func(o cliOptions) bool { return o.codePackageImportPrefixFileSummary != "" },
			func(cwd string, o cliOptions) error {
				return listCodePackageImportPrefixFileSummary(cwd, o.codePackageImportPrefixFileSummary)
			}),
		codeSymbolCmd("code-package-import-file-summary", func(o cliOptions) bool { return o.codePackageImportFileSummary != "" },
			func(cwd string, o cliOptions) error {
				return listCodePackageImportFileSummary(cwd, o.codePackageImportFileSummary)
			}),
		codeSymbolCmd("code-package-symbols", func(o cliOptions) bool { return o.codePackageSymbols != "" },
			func(cwd string, o cliOptions) error { return listCodePackageSymbols(cwd, o.codePackageSymbols) }),
		codeSymbolCmd("code-package-symbol-file-summary", func(o cliOptions) bool { return o.codePackageSymbolFileSummary != "" },
			func(cwd string, o cliOptions) error {
				return listCodePackageSymbolFileSummary(cwd, o.codePackageSymbolFileSummary)
			}),
		codeSymbolCmd("code-package-symbol-name", func(o cliOptions) bool { return o.codePackageSymbolName != "" },
			func(cwd string, o cliOptions) error { return listCodePackageSymbol(cwd, o.codePackageSymbolName) }),
		codeSymbolCmd("code-package-symbol-name-file-summary", func(o cliOptions) bool { return o.codePackageSymbolNameFileSummary != "" },
			func(cwd string, o cliOptions) error {
				return listCodePackageSymbolNameFileSummary(cwd, o.codePackageSymbolNameFileSummary)
			}),
		codeSymbolCmd("code-package-symbol-list", func(o cliOptions) bool { return o.codePackageSymbolList != "" },
			func(cwd string, o cliOptions) error { return listCodePackageSymbolList(cwd, o.codePackageSymbolList) }),
		codeSymbolCmd("code-package-symbol-kind", func(o cliOptions) bool { return o.codePackageSymbolKind != "" },
			func(cwd string, o cliOptions) error { return listCodePackageSymbolKind(cwd, o.codePackageSymbolKind) }),
		codeSymbolCmd("code-package-symbol-kind-file-summary", func(o cliOptions) bool { return o.codePackageSymbolKindFileSummary != "" },
			func(cwd string, o cliOptions) error {
				return listCodePackageSymbolKindFileSummary(cwd, o.codePackageSymbolKindFileSummary)
			}),
		codeSymbolCmd("code-package-symbol-prefix", func(o cliOptions) bool { return o.codePackageSymbolPrefix != "" },
			func(cwd string, o cliOptions) error {
				return listCodePackageSymbolPrefix(cwd, o.codePackageSymbolPrefix)
			}),
		codeSymbolCmd("code-package-symbol-prefix-file-summary", func(o cliOptions) bool { return o.codePackageSymbolPrefixFileSummary != "" },
			func(cwd string, o cliOptions) error {
				return listCodePackageSymbolPrefixFileSummary(cwd, o.codePackageSymbolPrefixFileSummary)
			}),

		// Code files
		codeSymbolCmd("code-file-path", func(o cliOptions) bool { return o.codeFilePath != "" },
			func(cwd string, o cliOptions) error { return showCodeFile(cwd, o.codeFilePath) }),
		codeSymbolCmd("code-file-imports", func(o cliOptions) bool { return o.codeFileImports != "" },
			func(cwd string, o cliOptions) error { return listCodeFileImports(cwd, o.codeFileImports) }),
		codeSymbolCmd("code-file-symbols", func(o cliOptions) bool { return o.codeFileSymbols != "" },
			func(cwd string, o cliOptions) error { return listCodeFileSymbols(cwd, o.codeFileSymbols) }),
		codeSymbolCmd("code-file-symbol-summary", func(o cliOptions) bool { return o.codeFileSymbolSummary != "" },
			func(cwd string, o cliOptions) error { return listCodeFileSymbolSummary(cwd, o.codeFileSymbolSummary) }),
		codeSymbolCmd("code-file-symbol-name", func(o cliOptions) bool { return o.codeFileSymbolName != "" },
			func(cwd string, o cliOptions) error { return listCodeFileSymbol(cwd, o.codeFileSymbolName) }),
		codeSymbolCmd("code-file-symbol-kind", func(o cliOptions) bool { return o.codeFileSymbolKind != "" },
			func(cwd string, o cliOptions) error { return listCodeFileSymbolKind(cwd, o.codeFileSymbolKind) }),
		codeSymbolCmd("code-file-symbol-prefix", func(o cliOptions) bool { return o.codeFileSymbolPrefix != "" },
			func(cwd string, o cliOptions) error { return listCodeFileSymbolPrefix(cwd, o.codeFileSymbolPrefix) }),
		codeSymbolCmd("code-file-import-prefix", func(o cliOptions) bool { return o.codeFileImportPrefix != "" },
			func(cwd string, o cliOptions) error { return listCodeFileImportPrefix(cwd, o.codeFileImportPrefix) }),
		codeSymbolCmd("code-file-import-path", func(o cliOptions) bool { return o.codeFileImportPath != "" },
			func(cwd string, o cliOptions) error { return listCodeFileImportPath(cwd, o.codeFileImportPath) }),

		// Code graph / structure
		codeSymbolCmd("list-code-layers", func(o cliOptions) bool { return o.listCodeLayers },
			func(cwd string, _ cliOptions) error { return listCodeLayers(cwd) }),
		codeSymbolCmd("list-code-cycles", func(o cliOptions) bool { return o.listCodeCycles },
			func(cwd string, _ cliOptions) error { return listCodeCycles(cwd) }),
		codeSymbolCmd("code-summary", func(o cliOptions) bool { return o.codeSummary },
			func(cwd string, _ cliOptions) error { return printCodeSummary(cwd) }),
		codeSymbolCmd("list-code-files", func(o cliOptions) bool { return o.listCodeFiles },
			func(cwd string, _ cliOptions) error { return listCodeFiles(cwd) }),
		codeSymbolCmd("code-impact-target", func(o cliOptions) bool { return o.codeImpactTarget != "" },
			func(cwd string, o cliOptions) error { return listCodeImpact(cwd, o.codeImpactTarget) }),
		codeSymbolCmd("code-reach-target", func(o cliOptions) bool { return o.codeReachTarget != "" },
			func(cwd string, o cliOptions) error { return listCodeReachable(cwd, o.codeReachTarget) }),
		codeSymbolCmd("code-deps-target", func(o cliOptions) bool { return o.codeDepsTarget != "" },
			func(cwd string, o cliOptions) error { return listCodeDeps(cwd, o.codeDepsTarget) }),
		codeSymbolCmd("code-rdeps-target", func(o cliOptions) bool { return o.codeRdepsTarget != "" },
			func(cwd string, o cliOptions) error { return listCodeReverseDeps(cwd, o.codeRdepsTarget) }),

		// ---------------------------------------------------------------
		// Tier: providerless config -- local analysis (non-code)
		// ---------------------------------------------------------------
		{
			name:  "git-history-search-providerless",
			tier:  tierProviderlessConfig,
			match: func(o cliOptions) bool { return o.gitHistorySearch != "" },
			runProviderlessConfig: func(ctx context.Context, o cliOptions, s appState) error {
				return runGitHistorySearch(ctx, s.cwd, o.gitHistorySearch, o.gitHistoryLimit.value)
			},
		},
		{
			name:  "watch-loop-providerless",
			tier:  tierProviderlessConfig,
			match: func(o cliOptions) bool { return o.watchLoop },
			runProviderlessConfig: func(ctx context.Context, o cliOptions, s appState) error {
				return runWatchLoop(ctx, s.cwd, o.watchLargeFileBytes.value, o.watchIntervalSeconds.value, o.watchMaxIterations.value)
			},
		},
		{
			name:  "watch-scan-providerless",
			tier:  tierProviderlessConfig,
			match: func(o cliOptions) bool { return o.watchScan },
			runProviderlessConfig: func(_ context.Context, o cliOptions, s appState) error {
				return runWatchScan(s.cwd, o.watchLargeFileBytes.value, o.watchJSON)
			},
		},
		{
			name:  "review-scan-providerless",
			tier:  tierProviderlessConfig,
			match: func(o cliOptions) bool { return o.reviewScan },
			runProviderlessConfig: func(_ context.Context, o cliOptions, s appState) error {
				return runReviewScan(s.cwd, o.watchLargeFileBytes.value)
			},
		},

		// ---------------------------------------------------------------
		// Tier: stateful -- session read (require loaded session)
		// ---------------------------------------------------------------
		{
			name:  "replay-session",
			tier:  tierStateful,
			match: func(o cliOptions) bool { return o.replayRef != "" },
			runStateful: func(_ context.Context, _ cliOptions, s appState) error {
				printTranscript(s.sessionState)
				return nil
			},
		},
		{
			name:  "show-session",
			tier:  tierStateful,
			match: func(o cliOptions) bool { return o.showSessionRef != "" },
			runStateful: func(_ context.Context, _ cliOptions, s appState) error {
				return showSession(s.sessionState, s.sessionStore.Path(s.sessionState.ID))
			},
		},
		{
			name:  "summary-session",
			tier:  tierStateful,
			match: func(o cliOptions) bool { return o.summarySessionRef != "" },
			runStateful: func(_ context.Context, _ cliOptions, s appState) error {
				printSessionSummary(s.sessionState, s.sessionStore.Path(s.sessionState.ID))
				return nil
			},
		},
		{
			name:  "export-session",
			tier:  tierStateful,
			match: func(o cliOptions) bool { return o.exportRef != "" },
			runStateful: func(_ context.Context, o cliOptions, s appState) error {
				return exportSession(s.sessionState, o.exportFormat)
			},
		},
		{
			name:  "list-artifacts",
			tier:  tierStateful,
			match: func(o cliOptions) bool { return o.listArtifacts },
			runStateful: func(_ context.Context, _ cliOptions, s appState) error {
				listArtifacts(s.sessionState)
				return nil
			},
		},
		{
			name:  "list-evaluations",
			tier:  tierStateful,
			match: func(o cliOptions) bool { return o.listEvaluations },
			runStateful: func(_ context.Context, _ cliOptions, s appState) error {
				listEvaluations(s.sessionState)
				return nil
			},
		},
		{
			name:  "list-failures",
			tier:  tierStateful,
			match: func(o cliOptions) bool { return o.listFailures },
			runStateful: func(_ context.Context, _ cliOptions, s appState) error {
				listFailures(s.sessionState)
				return nil
			},
		},
		{
			name:  "list-messages",
			tier:  tierStateful,
			match: func(o cliOptions) bool { return o.listMessages },
			runStateful: func(_ context.Context, _ cliOptions, s appState) error {
				listMessages(s.sessionState)
				return nil
			},
		},

		// ---------------------------------------------------------------
		// Tier: stateful -- session write
		// ---------------------------------------------------------------
		{
			name:  "record-failure",
			tier:  tierStateful,
			match: func(o cliOptions) bool { return o.recordFailure != "" },
			runStateful: func(_ context.Context, o cliOptions, s appState) error {
				return recordFailure(s.sessionStore, s.sessionState, o.recordFailure, o.failureReason, o.failureCommit, s.selectedAgent)
			},
		},
		{
			name:  "record-evaluation",
			tier:  tierStateful,
			match: func(o cliOptions) bool { return o.recordEvaluation != "" },
			runStateful: func(_ context.Context, o cliOptions, s appState) error {
				return recordEvaluation(s.sessionStore, s.sessionState, o.recordEvaluation, o.evaluationOutcome, o.evaluationNotes, o.evaluationReference, o.evaluationScore.value)
			},
		},
		{
			name:  "record-artifact",
			tier:  tierStateful,
			match: func(o cliOptions) bool { return o.recordArtifact != "" },
			runStateful: func(_ context.Context, o cliOptions, s appState) error {
				return recordArtifact(s.sessionStore, s.sessionState, o.recordArtifact, o.artifactKind, o.artifactSummary, s.selectedAgent)
			},
		},
		{
			name:  "feedback-apply",
			tier:  tierStateful,
			match: func(o cliOptions) bool { return o.feedbackApplyConfig != "" },
			runStateful: func(_ context.Context, o cliOptions, s appState) error {
				return applyFeedbackProposals(s.sessionState, o.feedbackApplyConfig, o.feedbackHistoryPath)
			},
		},

		// ---------------------------------------------------------------
		// Tier: stateful -- execution
		// ---------------------------------------------------------------
		{
			name:  "async-run",
			tier:  tierStateful,
			match: func(o cliOptions) bool { return o.asyncRun },
			runStateful: func(ctx context.Context, o cliOptions, s appState) error {
				return runAsyncTasks(ctx, s, o)
			},
		},
		{
			name:  "spawn-agents",
			tier:  tierStateful,
			match: func(o cliOptions) bool { return len(o.spawnAgentSpecs) > 0 },
			runStateful: func(ctx context.Context, o cliOptions, s appState) error {
				return runSpawnAgents(ctx, s, o)
			},
		},
		{
			name:  "bash-command",
			tier:  tierStateful,
			match: func(o cliOptions) bool { return o.bashCommand != "" },
			runStateful: func(ctx context.Context, o cliOptions, s appState) error {
				return runBashCommand(ctx, s, o)
			},
		},

		// ---------------------------------------------------------------
		// Tier: stateful -- retrieval
		// ---------------------------------------------------------------
		{
			name: "agent-memory",
			tier: tierStateful,
			match: func(o cliOptions) bool {
				return o.agentMemorySearch != "" || len(o.agentMemoryIndexFiles) > 0
			},
			runStateful: func(_ context.Context, o cliOptions, s appState) error {
				return runAgentMemoryCommand(s.cwd, s.selectedAgent, o)
			},
		},
		{
			name:  "merge-artifacts",
			tier:  tierStateful,
			match: func(o cliOptions) bool { return o.mergeArtifactsPath != "" },
			runStateful: func(ctx context.Context, o cliOptions, s appState) error {
				return mergeArtifacts(ctx, s, o.mergeArtifactsPath, o.mergeArtifactMaxBytes.value)
			},
		},

		// ---------------------------------------------------------------
		// Tier: stateful -- local analysis
		// ---------------------------------------------------------------
		{
			name:  "feedback-proposals",
			tier:  tierStateful,
			match: func(o cliOptions) bool { return o.feedbackProposals },
			runStateful: func(_ context.Context, _ cliOptions, s appState) error {
				printFeedbackProposals(s.sessionState)
				return nil
			},
		},

		// ---------------------------------------------------------------
		// Tier: stateful -- provider-dependent
		// ---------------------------------------------------------------
		{
			name:  "list-models",
			tier:  tierStateful,
			match: func(o cliOptions) bool { return o.listModels },
			runStateful: func(ctx context.Context, _ cliOptions, s appState) error {
				return listModels(ctx, s.registry)
			},
		},
		{
			name:  "doctor",
			tier:  tierStateful,
			match: func(o cliOptions) bool { return o.doctor },
			runStateful: func(ctx context.Context, _ cliOptions, s appState) error {
				return doctor(ctx, s)
			},
		},
	}
}

// codeSymbolCmd is a helper that creates a command entry for code analysis
// commands.  These commands are available at tierProviderlessConfig (they
// need a cwd from appState but no LLM provider).
func codeSymbolCmd(
	name string,
	matchFn func(cliOptions) bool,
	handler func(cwd string, opts cliOptions) error,
) command {
	return command{
		name:  name,
		tier:  tierProviderlessConfig,
		match: matchFn,
		runProviderlessConfig: func(_ context.Context, o cliOptions, s appState) error {
			return handler(s.cwd, o)
		},
	}
}

// dispatchProviderless runs the first matching providerless command from the
// registry.  Returns (true, err) if a command was handled, (false, nil) if
// none matched.
func dispatchProviderless(ctx context.Context, opts cliOptions, store *session.Store) (bool, error) {
	for i := range commandRegistry {
		cmd := &commandRegistry[i]
		if cmd.tier != tierProviderless || !cmd.match(opts) {
			continue
		}

		return true, cmd.runProviderless(ctx, opts, store)
	}

	return false, nil
}

// dispatchProviderlessConfig runs the first matching providerless-config
// command.  The caller must supply a partially loaded appState (cwd, agent
// registry, plugin paths).
func dispatchProviderlessConfig(ctx context.Context, opts cliOptions, state appState) (bool, error) {
	for i := range commandRegistry {
		cmd := &commandRegistry[i]
		if cmd.tier != tierProviderlessConfig || !cmd.match(opts) {
			continue
		}

		return true, cmd.runProviderlessConfig(ctx, opts, state)
	}

	return false, nil
}

// dispatchStateful runs the first matching stateful command from the
// registry.
func dispatchStateful(ctx context.Context, opts cliOptions, state appState) (bool, error) {
	for i := range commandRegistry {
		cmd := &commandRegistry[i]
		if cmd.tier != tierStateful || !cmd.match(opts) {
			continue
		}

		return true, cmd.runStateful(ctx, opts, state)
	}

	return false, nil
}

// providerlessConfigRequested returns true if any providerless-config
// command matches the current options.
func providerlessConfigRequested(opts cliOptions) bool {
	for i := range commandRegistry {
		cmd := &commandRegistry[i]
		if cmd.tier == tierProviderlessConfig && cmd.match(opts) {
			return true
		}
	}

	return false
}
