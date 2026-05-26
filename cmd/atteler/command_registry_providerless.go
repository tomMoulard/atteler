package main

import (
	"context"
	"fmt"
	"os"

	appconfig "github.com/tommoulard/atteler/pkg/config"
	"github.com/tommoulard/atteler/pkg/session"
)

func providerlessSessionCommands() []command {
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
			name:            "headless-command",
			tier:            tierProviderless,
			match:           headlessCommandRequested,
			runProviderless: runHeadlessCommand,
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
	}
}

func providerlessFileCommands() []command {
	return []command{
		// ---------------------------------------------------------------
		// Tier: providerless -- file utilities
		// ---------------------------------------------------------------
		{
			name:  "task-command",
			tier:  tierProviderless,
			match: taskCommandRequested,
			runProviderless: func(ctx context.Context, o cliOptions, s *session.Store) error {
				return runTaskListCommand(ctx, s, taskCommandInputFromOptions(o))
			},
		},
		{
			name:  "eval-output",
			tier:  tierProviderless,
			match: evalCommandRequested,
			runProviderless: func(_ context.Context, o cliOptions, _ *session.Store) error {
				return evalOutputCommand(o)
			},
		},
		{
			name:  "context-pack",
			tier:  tierProviderless,
			match: func(o cliOptions) bool { return o.contextPackPath != "" },
			runProviderless: func(_ context.Context, o cliOptions, _ *session.Store) error {
				return runContextPack(o.contextPackPath, o.contextPackTokens.value, o.model)
			},
		},
		{
			name:  "init-rtk-plugin",
			tier:  tierProviderless,
			match: func(o cliOptions) bool { return o.initRTKPluginDir != "" },
			runProviderless: func(_ context.Context, o cliOptions, _ *session.Store) error {
				return initRTKPlugin(o.initRTKPluginDir)
			},
		},
		{
			name: "lsp-symbols",
			tier: tierProviderless,
			match: func(o cliOptions) bool {
				return o.lspSymbols || o.lspWorkspaceSymbols != ""
			},
			runProviderless: func(ctx context.Context, o cliOptions, _ *session.Store) error {
				return runLSPSymbols(ctx, lspSymbolsCommandInputFromOptions(o))
			},
		},
		{
			name: "memory-command",
			tier: tierProviderless,
			match: func(o cliOptions) bool {
				return o.retrievalSearch == "" && (o.memorySearch != "" ||
					len(o.memoryIndexFiles) > 0 ||
					o.memoryDelete != "" ||
					o.memoryCompact ||
					o.memoryMigrate)
			},
			runProviderless: func(_ context.Context, o cliOptions, s *session.Store) error {
				return runMemoryCommand(s, memoryCommandInputFromOptions(o))
			},
		},
		{
			name: "vector-search",
			tier: tierProviderless,
			match: func(o cliOptions) bool {
				return o.retrievalSearch == "" && (o.vectorSearch != "" || len(o.vectorIndexFiles) > 0)
			},
			runProviderless: func(_ context.Context, o cliOptions, _ *session.Store) error {
				return runVectorSearch(vectorSearchCommandInputFromOptions(o))
			},
		},
	}
}

func providerlessPlanningCommands() []command {
	return []command{
		// ---------------------------------------------------------------
		// Tier: providerless -- planning utilities
		// ---------------------------------------------------------------
		// MCP invoke must precede MCP manifest (compound condition).
		{
			name: "mcp-invoke",
			tier: tierProviderless,
			match: func(o cliOptions) bool {
				return o.mcpMethod != "" || o.mcpToolName != ""
			},
			runProviderless: func(ctx context.Context, o cliOptions, _ *session.Store) error {
				return runMCPInvoke(ctx, mcpInvokeCommandInputFromOptions(o))
			},
		},
		{
			name:  "mcp-manifest",
			tier:  tierProviderless,
			match: func(o cliOptions) bool { return o.mcpManifestPath != "" },
			runProviderless: func(_ context.Context, o cliOptions, _ *session.Store) error {
				return runMCPManifest(mcpManifestCommandInputFromOptions(o))
			},
		},
		{
			name:  "speculate-plan",
			tier:  tierProviderless,
			match: func(o cliOptions) bool { return o.speculatePlan },
			runProviderless: func(_ context.Context, o cliOptions, _ *session.Store) error {
				return runSpeculatePlan(speculatePlanCommandInputFromOptions(o))
			},
		},
		{
			name:  "review-plan",
			tier:  tierProviderless,
			match: func(o cliOptions) bool { return o.reviewPlan },
			runProviderless: func(_ context.Context, o cliOptions, _ *session.Store) error {
				return runReviewPlan(reviewPlanCommandInputFromOptions(o))
			},
		},
		{
			name:  "async-plan",
			tier:  tierProviderless,
			match: func(o cliOptions) bool { return o.asyncPlan },
			runProviderless: func(_ context.Context, o cliOptions, _ *session.Store) error {
				return runAsyncPlan(asyncPlanCommandInputFromOptions(o))
			},
		},
		{
			name: "route-models-providerless",
			tier: tierProviderless,
			match: func(o cliOptions) bool {
				routeRequested := len(o.routeCandidates) > 0 || o.routeInteractive || o.routeBatch

				return routeRequested && o.oncePrompt == "" && !o.readStdin
			},
			runProviderless: func(_ context.Context, o cliOptions, _ *session.Store) error {
				return runRouteModels(routeModelsCommandInputFromOptions(o))
			},
		},
		{
			name:  "suggest-skill",
			tier:  tierProviderless,
			match: func(o cliOptions) bool { return len(o.suggestSkillSteps) > 0 || skillLearningCommandRequested(o) },
			runProviderless: func(ctx context.Context, o cliOptions, _ *session.Store) error {
				if skillLearningCommandRequested(o) {
					cfg, _, cfgErr := appconfig.Load()
					if cfgErr != nil {
						fmt.Fprintln(os.Stderr, "warning: "+cfgErr.Error())
					}

					input := skillLearningCommandInputFromOptions(o)
					learningOpts, _ := skillLearningOptionsFromLoadedConfig(cfg, o)
					input.Dir = learningOpts.StoreDir
					input.SkillDir = learningOpts.SkillDir
					input.EffectiveEnabled = learningOpts.Enabled

					return runSkillLearningCommand(ctx, input)
				}

				return suggestSkill(o.suggestSkillSteps, o.skillMaxSteps.value, o.skillMinOccurrences.value, o.skillSaveDir, o.skillReviewOnly)
			},
		},
	}
}
