package main

import (
	"context"
	"fmt"
	"strings"

	"github.com/tommoulard/atteler/pkg/autonomy"
)

func providerlessConfigAgentPluginCommands() []command {
	return []command{
		// ---------------------------------------------------------------
		// Tier: providerless config -- agents, plugins, prompt-complete
		// ---------------------------------------------------------------
		{
			name:                  "state-diagnostics",
			tier:                  tierProviderlessConfig,
			match:                 func(o cliOptions) bool { return o.stateDiagnostics },
			runProviderlessConfig: printStateDiagnostics,
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
				return planAgents(
					s.agentRegistry,
					o.planAgentsPrompt,
					o.planAgentNames,
					o.planMaxAgents.value,
					recentAgentNamesForPlan(s),
				)
			},
		},
		{
			name:  "prompt-complete-providerless",
			tier:  tierProviderlessConfig,
			match: func(o cliOptions) bool { return o.promptCompleteInput != "" },
			runProviderlessConfig: func(ctx context.Context, o cliOptions, s appState) error {
				if o.sessionRef != "" {
					if err := authorizeSessionStoreRead(ctx, s.sessionStore, o.sessionRef, "load session for prompt completion"); err != nil {
						return fmt.Errorf("load session for prompt completion: %w", err)
					}

					saved, err := s.sessionStore.Load(o.sessionRef)
					if err != nil {
						return fmt.Errorf("load session for prompt completion: %w", err)
					}

					s.sessionState = saved
				}

				s.selectedAgent = strings.TrimSpace(o.agentName)
				if s.selectedAgent == "" {
					s.selectedAgent = strings.TrimSpace(s.sessionState.DefaultAgent)
				}

				promptComplete(ctx, s, o.promptCompleteInput, o.promptCompleteLimit.value)

				return nil
			},
		},
		{
			name:  "feedback-rollback",
			tier:  tierProviderlessConfig,
			match: func(o cliOptions) bool { return o.feedbackRollbackConfig != "" },
			runProviderlessConfig: func(ctx context.Context, o cliOptions, state appState) error {
				if !autonomy.Normalize(state.autonomy).Allows(autonomy.ActionFileWrite) {
					return fmt.Errorf("%s", autonomy.DenialMessage(state.autonomy, autonomy.ActionFileWrite, "--feedback-rollback"))
				}

				return rollbackFeedbackGuidance(
					ctx,
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
			runProviderlessConfig: func(ctx context.Context, o cliOptions, state appState) error {
				if !autonomy.Normalize(state.autonomy).Allows(autonomy.ActionFileWrite) {
					return fmt.Errorf("%s", autonomy.DenialMessage(state.autonomy, autonomy.ActionFileWrite, "--feedback-approve"))
				}

				return approveFeedbackGuidance(
					ctx,
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
			runProviderlessConfig: func(ctx context.Context, _ cliOptions, s appState) error {
				return listPlugins(ctx, s.pluginPaths)
			},
		},
		{
			name:  "describe-plugin",
			tier:  tierProviderlessConfig,
			match: func(o cliOptions) bool { return o.describePluginName != "" },
			runProviderlessConfig: func(ctx context.Context, o cliOptions, s appState) error {
				return describePlugin(ctx, s.pluginPaths, o.describePluginName)
			},
		},
		{
			name:  "run-plugin-providerless",
			tier:  tierProviderlessConfig,
			match: func(o cliOptions) bool { return o.runPluginTarget != "" },
			runProviderlessConfig: func(ctx context.Context, o cliOptions, s appState) error {
				if !o.pluginDryRun && !autonomy.Normalize(s.autonomy).Allows(autonomy.ActionMutatingShell) {
					return fmt.Errorf("%s", autonomy.DenialMessage(s.autonomy, autonomy.ActionMutatingShell, "--run-plugin"))
				}

				return runPluginEntrypoint(
					ctx,
					s.pluginPaths,
					s.pluginPolicy,
					s.permissionPolicy,
					o.runPluginTarget,
					o.pluginEntrypoint,
					o.pluginDryRun,
					o.pluginTimeout.value,
					s.autonomy,
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
			name:  "incident-diagnose-providerless",
			tier:  tierProviderlessConfig,
			match: func(o cliOptions) bool { return o.incidentDiagnose && !o.incidentApplyFix },
			runProviderlessConfig: func(ctx context.Context, o cliOptions, s appState) error {
				return runIncidentDiagnose(ctx, s, incidentDiagnoseCommandInputFromOptions(o))
			},
		},
		{
			name:  "git-history-search-providerless",
			tier:  tierProviderlessConfig,
			match: func(o cliOptions) bool { return o.gitHistorySearch != "" },
			runProviderlessConfig: func(ctx context.Context, o cliOptions, s appState) error {
				return runGitHistorySearch(ctx, s.cwd, o.gitHistorySearch, o.gitHistoryLimit.value, s.autonomy)
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
				return runWatchLoop(ctx, s.cwd, watchCLIOptionsFrom(o, s.autonomy), o.watchIntervalSeconds.value, o.watchMaxIterations.value)
			},
		},
		{
			name:  "watch-scan-providerless",
			tier:  tierProviderlessConfig,
			match: func(o cliOptions) bool { return o.watchScan },
			runProviderlessConfig: func(ctx context.Context, o cliOptions, s appState) error {
				return runWatchScan(ctx, s.cwd, watchCLIOptionsFrom(o, s.autonomy))
			},
		},
		{
			name:  "review-scan-providerless",
			tier:  tierProviderlessConfig,
			match: func(o cliOptions) bool { return o.reviewScan },
			runProviderlessConfig: func(ctx context.Context, o cliOptions, s appState) error {
				return runReviewScan(ctx, s.cwd, watchCLIOptionsFrom(o, s.autonomy))
			},
		},
	}
}
