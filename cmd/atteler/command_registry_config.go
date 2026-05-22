package main

import (
	"context"
	"fmt"
)

func providerlessConfigAgentPluginCommands() []command {
	return []command{
		// ---------------------------------------------------------------
		// Tier: providerless config -- agents, plugins, prompt-complete
		// ---------------------------------------------------------------
		{
			name:  "state-diagnostics",
			tier:  tierProviderlessConfig,
			match: func(o cliOptions) bool { return o.stateDiagnostics },
			runProviderlessConfig: func(_ context.Context, o cliOptions, s appState) error {
				return printStateDiagnostics(o, s)
			},
		},
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
			runProviderlessConfig: func(ctx context.Context, o cliOptions, s appState) error {
				if o.sessionRef != "" {
					saved, err := s.sessionStore.Load(o.sessionRef)
					if err != nil {
						return fmt.Errorf("load session for prompt completion: %w", err)
					}

					s.sessionState = saved
				}

				s.selectedAgent = o.agentName
				promptComplete(ctx, s, o.promptCompleteInput, o.promptCompleteLimit.value)

				return nil
			},
		},
		{
			name:  "feedback-rollback",
			tier:  tierProviderlessConfig,
			match: func(o cliOptions) bool { return o.feedbackRollbackConfig != "" },
			runProviderlessConfig: func(_ context.Context, o cliOptions, _ appState) error {
				return rollbackFeedbackGuidance(
					o.feedbackRollbackConfig,
					o.feedbackHistoryPath,
					o.feedbackRollbackAgent,
					o.feedbackRollbackID,
					o.feedbackRollbackReason,
				)
			},
		},
		{
			name:  "feedback-approve",
			tier:  tierProviderlessConfig,
			match: func(o cliOptions) bool { return o.feedbackApproveConfig != "" },
			runProviderlessConfig: func(_ context.Context, o cliOptions, _ appState) error {
				return approveFeedbackGuidance(
					o.feedbackApproveConfig,
					o.feedbackHistoryPath,
					o.feedbackApproveAgent,
					o.feedbackApproveID,
				)
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
				return runPluginEntrypoint(
					ctx,
					s.pluginPaths,
					s.pluginPolicy,
					o.runPluginTarget,
					o.pluginEntrypoint,
					o.pluginDryRun,
					o.pluginTimeout.value,
				)
			},
		},
	}
}

func providerlessConfigLocalAnalysisCommands() []command {
	return []command{
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
			name:  "retrieval-search",
			tier:  tierProviderlessConfig,
			match: func(o cliOptions) bool { return o.retrievalSearch != "" },
			runProviderlessConfig: func(ctx context.Context, o cliOptions, s appState) error {
				return runRetrievalCommand(ctx, s, retrievalCommandInputFromOptions(o))
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
	}
}
