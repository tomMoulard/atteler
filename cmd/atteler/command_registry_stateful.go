package main

import (
	"context"
	"fmt"
	"strings"
)

type statefulSessionCommand[T any] struct {
	match func(T) bool
	run   func(context.Context, T, appState) error
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
	return matchingStatefulSessionReadCommand(sessionReadCommandInputFromOptions(opts)) != nil
}

func runStatefulSessionReadCommand(ctx context.Context, opts cliOptions, state appState) error {
	input := sessionReadCommandInputFromOptions(opts)

	cmd, err := selectStatefulSessionReadCommand(input)
	if err != nil {
		return err
	}

	if cmd == nil {
		return nil
	}

	return cmd.run(ctx, input, state)
}

func matchingStatefulSessionReadCommand(input sessionReadCommandInput) *statefulSessionCommand[sessionReadCommandInput] {
	matches := matchingStatefulSessionCommands(statefulSessionReadCommandSet(), input)
	if len(matches) == 0 {
		return nil
	}

	return matches[0]
}

func selectStatefulSessionReadCommand(input sessionReadCommandInput) (*statefulSessionCommand[sessionReadCommandInput], error) {
	return selectStatefulSessionCommand("session read", statefulSessionReadCommandSet(), input)
}

func selectStatefulSessionCommand[T any](
	scope string,
	commands []statefulSessionCommand[T],
	input T,
) (*statefulSessionCommand[T], error) {
	matches := matchingStatefulSessionCommands(commands, input)
	switch len(matches) {
	case 0:
		return nil, nil
	case 1:
		return matches[0], nil
	default:
		return nil, fmt.Errorf("ambiguous CLI command: flags match multiple %s commands (%s); choose one command or remove conflicting flags",
			scope, statefulSessionCommandNames(matches))
	}
}

func matchingStatefulSessionCommands[T any](commands []statefulSessionCommand[T], input T) []*statefulSessionCommand[T] {
	matches := make([]*statefulSessionCommand[T], 0, 1)

	for i := range commands {
		cmd := &commands[i]
		if cmd.match(input) {
			matches = append(matches, cmd)
		}
	}

	return matches
}

func statefulSessionCommandNames[T any](commands []*statefulSessionCommand[T]) string {
	names := make([]string, 0, len(commands))
	for _, command := range commands {
		names = append(names, command.name)
	}

	return strings.Join(names, ", ")
}

func statefulSessionReadCommandSet() []statefulSessionCommand[sessionReadCommandInput] {
	return []statefulSessionCommand[sessionReadCommandInput]{
		statefulSessionCmd("replay-session", func(input sessionReadCommandInput) bool { return input.ReplayRef != "" },
			func(_ context.Context, _ sessionReadCommandInput, s appState) error {
				printTranscript(s.sessionState)
				return nil
			}),
		statefulSessionCmd("show-session", func(input sessionReadCommandInput) bool { return input.ShowSessionRef != "" },
			func(_ context.Context, _ sessionReadCommandInput, s appState) error {
				return showSession(s.sessionState, s.sessionStore.Path(s.sessionState.ID))
			}),
		statefulSessionCmd("summary-session", func(input sessionReadCommandInput) bool { return input.SummarySessionRef != "" },
			func(_ context.Context, _ sessionReadCommandInput, s appState) error {
				printSessionSummary(s.sessionState, s.sessionStore.Path(s.sessionState.ID))
				return nil
			}),
		statefulSessionCmd("export-session", func(input sessionReadCommandInput) bool { return input.ExportRef != "" },
			func(_ context.Context, input sessionReadCommandInput, s appState) error {
				return exportSession(s.sessionState, input.ExportFormat)
			}),
		statefulSessionCmd("list-artifacts", func(input sessionReadCommandInput) bool { return input.ListArtifacts },
			func(_ context.Context, _ sessionReadCommandInput, s appState) error {
				listArtifacts(s.sessionState)
				return nil
			}),
		statefulSessionCmd("list-evaluations", func(input sessionReadCommandInput) bool { return input.ListEvaluations },
			func(_ context.Context, _ sessionReadCommandInput, s appState) error {
				listEvaluations(s.sessionState)
				return nil
			}),
		statefulSessionCmd("list-failures", func(input sessionReadCommandInput) bool { return input.ListFailures },
			func(_ context.Context, _ sessionReadCommandInput, s appState) error {
				listFailures(s.sessionState)
				return nil
			}),
		statefulSessionCmd("list-messages", func(input sessionReadCommandInput) bool { return input.ListMessages },
			func(_ context.Context, _ sessionReadCommandInput, s appState) error {
				listMessages(s.sessionState)
				return nil
			}),
	}
}

func statefulSessionWriteRequested(opts cliOptions) bool {
	return matchingStatefulSessionWriteCommand(sessionWriteCommandInputFromOptions(opts)) != nil
}

func runStatefulSessionWriteCommand(ctx context.Context, opts cliOptions, state appState) error {
	input := sessionWriteCommandInputFromOptions(opts)

	cmd, err := selectStatefulSessionWriteCommand(input)
	if err != nil {
		return err
	}

	if cmd == nil {
		return nil
	}

	return cmd.run(ctx, input, state)
}

func matchingStatefulSessionWriteCommand(input sessionWriteCommandInput) *statefulSessionCommand[sessionWriteCommandInput] {
	matches := matchingStatefulSessionCommands(statefulSessionWriteCommandSet(), input)
	if len(matches) == 0 {
		return nil
	}

	return matches[0]
}

func selectStatefulSessionWriteCommand(input sessionWriteCommandInput) (*statefulSessionCommand[sessionWriteCommandInput], error) {
	return selectStatefulSessionCommand("session write", statefulSessionWriteCommandSet(), input)
}

func statefulSessionWriteCommandSet() []statefulSessionCommand[sessionWriteCommandInput] {
	return []statefulSessionCommand[sessionWriteCommandInput]{
		statefulSessionCmd("record-failure", func(input sessionWriteCommandInput) bool { return input.RecordFailure != "" },
			func(_ context.Context, input sessionWriteCommandInput, s appState) error {
				return recordFailure(s.sessionStore, s.sessionState, input.RecordFailure, input.FailureReason, input.FailureCommit, s.selectedAgent)
			}),
		statefulSessionCmd("record-evaluation", func(input sessionWriteCommandInput) bool { return input.RecordEvaluation != "" },
			func(_ context.Context, input sessionWriteCommandInput, s appState) error {
				return recordEvaluation(s.sessionStore, s.sessionState, input.RecordEvaluation, input.EvaluationOutcome, input.EvaluationNotes, input.EvaluationReference, input.EvaluationScore)
			}),
		statefulSessionCmd("record-artifact", func(input sessionWriteCommandInput) bool { return input.RecordArtifact != "" },
			func(_ context.Context, input sessionWriteCommandInput, s appState) error {
				return recordArtifact(s.sessionStore, s.sessionState, input.RecordArtifact, input.ArtifactKind, input.ArtifactSummary, s.selectedAgent)
			}),
		statefulSessionCmd("feedback-apply", func(input sessionWriteCommandInput) bool { return input.FeedbackApplyConfig != "" },
			func(_ context.Context, input sessionWriteCommandInput, s appState) error {
				return applyFeedbackProposals(s.sessionState, input.FeedbackApplyConfig, input.FeedbackHistoryPath)
			}),
	}
}

func statefulSessionCmd[T any](
	name string,
	match func(T) bool,
	run func(context.Context, T, appState) error,
) statefulSessionCommand[T] {
	return statefulSessionCommand[T]{
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
				return runSpeculateExecution(ctx, s, speculateRunCommandInputFromOptions(o))
			},
		},
		{
			name:  "review-run",
			tier:  tierStateful,
			match: func(o cliOptions) bool { return o.reviewRun },
			runStateful: func(ctx context.Context, o cliOptions, s appState) error {
				return runReviewExecution(ctx, s, reviewRunCommandInputFromOptions(o))
			},
		},
		{
			name:  "async-run",
			tier:  tierStateful,
			match: func(o cliOptions) bool { return o.asyncRun },
			runStateful: func(ctx context.Context, o cliOptions, s appState) error {
				return runAsyncTasks(ctx, s, asyncRunCommandInputFromOptions(o))
			},
		},
		{
			name:  "spawn-agents",
			tier:  tierStateful,
			match: func(o cliOptions) bool { return len(o.spawnAgentSpecs) > 0 },
			runStateful: func(ctx context.Context, o cliOptions, s appState) error {
				return runSpawnAgents(ctx, s, spawnAgentsCommandInputFromOptions(o))
			},
		},
		{
			name:  "bash-command",
			tier:  tierStateful,
			match: func(o cliOptions) bool { return o.bashCommand != "" },
			runStateful: func(ctx context.Context, o cliOptions, s appState) error {
				return runBashCommand(ctx, s, bashCommandInputFromOptions(o))
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
				return o.retrievalSearch == "" && (o.agentMemorySearch != "" || len(o.agentMemoryIndexFiles) > 0)
			},
			runStateful: func(_ context.Context, o cliOptions, s appState) error {
				return runAgentMemoryCommand(s.cwd, s.selectedAgent, agentMemoryCommandInputFromOptions(o))
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
