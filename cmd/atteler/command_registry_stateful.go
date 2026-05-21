package main

import "context"

type statefulSessionCommand struct {
	match func(cliOptions) bool
	run   func(context.Context, cliOptions, appState) error
	name  string
}

func statefulSessionReadCommands() []command {
	return []command{
		// ---------------------------------------------------------------
		// Tier: stateful -- session read (require loaded session)
		// ---------------------------------------------------------------
		{
			name:        "session-read",
			tier:        tierStateful,
			match:       statefulSessionReadRequested,
			runStateful: runStatefulSessionReadCommand,
		},
	}
}

func statefulSessionWriteCommands() []command {
	return []command{
		// ---------------------------------------------------------------
		// Tier: stateful -- session write
		// ---------------------------------------------------------------
		{
			name:        "session-write",
			tier:        tierStateful,
			match:       statefulSessionWriteRequested,
			runStateful: runStatefulSessionWriteCommand,
		},
	}
}

func statefulSessionReadRequested(opts cliOptions) bool {
	return matchingStatefulSessionReadCommand(opts) != nil
}

func runStatefulSessionReadCommand(ctx context.Context, opts cliOptions, state appState) error {
	cmd := matchingStatefulSessionReadCommand(opts)
	if cmd == nil {
		return nil
	}

	return cmd.run(ctx, opts, state)
}

func matchingStatefulSessionReadCommand(opts cliOptions) *statefulSessionCommand {
	commands := statefulSessionReadCommandSet()
	for i := range commands {
		cmd := &commands[i]
		if cmd.match(opts) {
			return cmd
		}
	}

	return nil
}

func statefulSessionReadCommandSet() []statefulSessionCommand {
	return []statefulSessionCommand{
		statefulSessionCmd("replay-session", func(o cliOptions) bool { return o.replayRef != "" },
			func(_ context.Context, _ cliOptions, s appState) error {
				printTranscript(s.sessionState)
				return nil
			}),
		statefulSessionCmd("show-session", func(o cliOptions) bool { return o.showSessionRef != "" },
			func(_ context.Context, _ cliOptions, s appState) error {
				return showSession(s.sessionState, s.sessionStore.Path(s.sessionState.ID))
			}),
		statefulSessionCmd("summary-session", func(o cliOptions) bool { return o.summarySessionRef != "" },
			func(_ context.Context, _ cliOptions, s appState) error {
				printSessionSummary(s.sessionState, s.sessionStore.Path(s.sessionState.ID))
				return nil
			}),
		statefulSessionCmd("export-session", func(o cliOptions) bool { return o.exportRef != "" },
			func(_ context.Context, o cliOptions, s appState) error {
				return exportSession(s.sessionState, o.exportFormat)
			}),
		statefulSessionCmd("list-artifacts", func(o cliOptions) bool { return o.listArtifacts },
			func(_ context.Context, _ cliOptions, s appState) error {
				listArtifacts(s.sessionState)
				return nil
			}),
		statefulSessionCmd("list-evaluations", func(o cliOptions) bool { return o.listEvaluations },
			func(_ context.Context, _ cliOptions, s appState) error {
				listEvaluations(s.sessionState)
				return nil
			}),
		statefulSessionCmd("list-failures", func(o cliOptions) bool { return o.listFailures },
			func(_ context.Context, _ cliOptions, s appState) error {
				listFailures(s.sessionState)
				return nil
			}),
		statefulSessionCmd("list-messages", func(o cliOptions) bool { return o.listMessages },
			func(_ context.Context, _ cliOptions, s appState) error {
				listMessages(s.sessionState)
				return nil
			}),
	}
}

func statefulSessionWriteRequested(opts cliOptions) bool {
	return matchingStatefulSessionWriteCommand(opts) != nil
}

func runStatefulSessionWriteCommand(ctx context.Context, opts cliOptions, state appState) error {
	cmd := matchingStatefulSessionWriteCommand(opts)
	if cmd == nil {
		return nil
	}

	return cmd.run(ctx, opts, state)
}

func matchingStatefulSessionWriteCommand(opts cliOptions) *statefulSessionCommand {
	commands := statefulSessionWriteCommandSet()
	for i := range commands {
		cmd := &commands[i]
		if cmd.match(opts) {
			return cmd
		}
	}

	return nil
}

func statefulSessionWriteCommandSet() []statefulSessionCommand {
	return []statefulSessionCommand{
		statefulSessionCmd("record-failure", func(o cliOptions) bool { return o.recordFailure != "" },
			func(_ context.Context, o cliOptions, s appState) error {
				return recordFailure(s.sessionStore, s.sessionState, o.recordFailure, o.failureReason, o.failureCommit, s.selectedAgent)
			}),
		statefulSessionCmd("record-evaluation", func(o cliOptions) bool { return o.recordEvaluation != "" },
			func(_ context.Context, o cliOptions, s appState) error {
				return recordEvaluation(s.sessionStore, s.sessionState, o.recordEvaluation, o.evaluationOutcome, o.evaluationNotes, o.evaluationReference, o.evaluationScore.value)
			}),
		statefulSessionCmd("record-artifact", func(o cliOptions) bool { return o.recordArtifact != "" },
			func(_ context.Context, o cliOptions, s appState) error {
				return recordArtifact(s.sessionStore, s.sessionState, o.recordArtifact, o.artifactKind, o.artifactSummary, s.selectedAgent)
			}),
		statefulSessionCmd("feedback-apply", func(o cliOptions) bool { return o.feedbackApplyConfig != "" },
			func(_ context.Context, o cliOptions, s appState) error {
				return applyFeedbackProposals(s.sessionState, o.feedbackApplyConfig, o.feedbackHistoryPath)
			}),
	}
}

func statefulSessionCmd(
	name string,
	match func(cliOptions) bool,
	run func(context.Context, cliOptions, appState) error,
) statefulSessionCommand {
	return statefulSessionCommand{
		match: match,
		run:   run,
		name:  name,
	}
}

func statefulExecutionCommands() []command {
	return []command{
		// ---------------------------------------------------------------
		// Tier: stateful -- execution
		// ---------------------------------------------------------------
		{
			name:  "speculate-run",
			tier:  tierStateful,
			match: func(o cliOptions) bool { return o.speculateRun },
			runStateful: func(ctx context.Context, o cliOptions, s appState) error {
				return runSpeculateExecution(ctx, s, o)
			},
		},
		{
			name:  "review-run",
			tier:  tierStateful,
			match: func(o cliOptions) bool { return o.reviewRun },
			runStateful: func(ctx context.Context, o cliOptions, s appState) error {
				return runReviewExecution(ctx, s, o)
			},
		},
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
	}
}

func statefulRetrievalCommands() []command {
	return []command{
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
	}
}

func statefulLocalAnalysisCommands() []command {
	return []command{
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
	}
}

func statefulProviderCommands() []command {
	return []command{
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
