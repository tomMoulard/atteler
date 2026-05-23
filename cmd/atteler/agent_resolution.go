package main

import (
	"fmt"
	"strings"

	"github.com/tommoulard/atteler/pkg/agent"
	appconfig "github.com/tommoulard/atteler/pkg/config"
	"github.com/tommoulard/atteler/pkg/llm"
	"github.com/tommoulard/atteler/pkg/session"
)

func requestModelAndFallbacks(
	selectedModel string,
	modelLocked bool,
	fallbackModels []string,
	activeAgent agentSelection,
) (requestModel string, modelFallbacks []string) {
	requestModel = selectedModel

	modelFallbacks = fallbackModels
	if !activeAgent.ok || modelLocked {
		return requestModel, modelFallbacks
	}

	if activeAgent.agent.Model != "" {
		requestModel = activeAgent.agent.Model
	}

	if len(activeAgent.agent.FallbackModels) > 0 {
		modelFallbacks = activeAgent.agent.FallbackModels
	}

	return requestModel, modelFallbacks
}

func resolveAgent(agents *agent.Registry, selectedAgent, input string) (agentSelection, string, error) {
	agentName := selectedAgent

	prompt := input
	if inlineName, inlinePrompt, ok := agent.ParseInvocation(input); ok {
		agentName = inlineName
		prompt = inlinePrompt
	}

	if agentName == "" {
		if matchedAgent, ok := agents.MatchPrompt(prompt); ok {
			return agentSelection{name: matchedAgent.Name, agent: matchedAgent, ok: true}, prompt, nil
		}

		return agentSelection{}, prompt, nil
	}

	activeAgent, ok := agents.Get(agentName)
	if !ok {
		return agentSelection{}, input, fmt.Errorf("unknown agent %q", agentName)
	}

	if strings.TrimSpace(prompt) == "" {
		return agentSelection{}, input, fmt.Errorf("agent %q needs a prompt", agentName)
	}

	return agentSelection{name: agentName, agent: activeAgent, ok: true}, prompt, nil
}

func generationOverridesFromState(opts cliOptions, selection selectionState, persistedState appconfig.State, cwd string) generationSettings {
	generation := generationFromOptions(opts)

	if generation.ReasoningLevel == "" {
		if level := strings.TrimSpace(selection.sessionState.DefaultReasoningLevel); level != "" {
			generation.ReasoningLevel = level
		} else if level := strings.TrimSpace(persistedState.ReasoningLevelForFolder(cwd)); level != "" {
			generation.ReasoningLevel = level
		}
	}

	if generation.ModelMode == "" {
		generation.ModelMode = modelModeOverrideFromState(selection, persistedState, cwd)
	}

	return generation
}

func modelModeOverrideFromState(selection selectionState, persistedState appconfig.State, cwd string) string {
	if mode := strings.TrimSpace(selection.sessionState.DefaultModelMode); mode != "" {
		return mode
	}

	resolution := persistedState.ResolveModelModePreference(cwd)
	if mode := strings.TrimSpace(resolution.Value); mode != "" {
		return mode
	}

	if resolution.Source != "" {
		return llm.ModelModeDefault
	}

	return ""
}

type selectionState struct {
	sessionState   session.Session
	selectedModel  string
	selectedAgent  string
	fallbackModels []string
	modelLocked    bool
}

func resolveSelection(
	opts cliOptions,
	cfg appconfig.Config,
	persistedModel string,
	agentRegistry *agent.Registry,
	store *session.Store,
) (selectionState, error) {
	state := selectionState{
		selectedAgent:  opts.agentName,
		selectedModel:  opts.model,
		modelLocked:    opts.model != "",
		fallbackModels: append([]string(nil), cfg.FallbackModels...),
	}
	if state.modelLocked {
		state.fallbackModels = nil
	}

	state.sessionState = session.New(state.selectedModel, nil)
	if err := loadRequestedSession(opts, store, &state); err != nil {
		return selectionState{}, err
	}

	if err := applySelectedAgent(opts, agentRegistry, &state); err != nil {
		return selectionState{}, err
	}

	if err := applyRouteSelection(routeModelsCommandInputFromOptions(opts), &state); err != nil {
		return selectionState{}, err
	}

	if state.selectedModel == "" {
		state.selectedModel = persistedModel
	}

	if state.selectedModel == "" {
		state.selectedModel = cfg.DefaultModel
	}

	if state.selectedModel != "" {
		state.sessionState.DefaultModel = state.selectedModel
	}

	if opts.sessionTitle != "" {
		state.sessionState.Title = opts.sessionTitle
	}

	if len(opts.sessionTags) > 0 {
		state.sessionState.Tags = mergeTags(state.sessionState.Tags, opts.sessionTags)
	}

	return state, nil
}

func loadRequestedSession(opts cliOptions, store *session.Store, state *selectionState) error {
	if opts.sessionRef == "" && opts.replayRef == "" && opts.exportRef == "" && opts.showSessionRef == "" && opts.summarySessionRef == "" {
		return nil
	}

	ref := firstNonEmpty(opts.replayRef, opts.showSessionRef, opts.summarySessionRef, opts.exportRef, opts.sessionRef)

	loadedSession, err := store.Load(ref)
	if err != nil {
		return fmt.Errorf("load session: %w", err)
	}

	state.sessionState = loadedSession
	if state.selectedAgent == "" {
		state.selectedAgent = state.sessionState.DefaultAgent
	}

	if state.selectedModel == "" {
		state.selectedModel = state.sessionState.DefaultModel
	}

	return nil
}

func applySelectedAgent(opts cliOptions, agentRegistry *agent.Registry, state *selectionState) error {
	if state.selectedAgent == "" || (opts.agentName == "" && sessionUtilityCommandRequested(opts)) {
		return nil
	}

	activeAgent, ok := agentRegistry.Get(state.selectedAgent)
	if !ok {
		return fmt.Errorf("unknown agent %q", state.selectedAgent)
	}

	if state.selectedModel == "" {
		state.selectedModel = activeAgent.Model
	}

	if !state.modelLocked && len(activeAgent.FallbackModels) > 0 {
		state.fallbackModels = activeAgent.FallbackModels
	}

	state.sessionState.DefaultAgent = state.selectedAgent

	return nil
}

func sessionUtilityCommandRequested(opts cliOptions) bool {
	return sessionReadUtilityRequested(opts) ||
		sessionWriteUtilityRequested(opts) ||
		sessionLocalUtilityRequested(opts) ||
		workflowExecutionUtilityRequested(opts) ||
		providerInspectionUtilityRequested(opts)
}

func sessionReadUtilityRequested(opts cliOptions) bool {
	return opts.replayRef != "" ||
		opts.exportRef != "" ||
		opts.showSessionRef != "" ||
		opts.summarySessionRef != "" ||
		opts.listArtifacts ||
		opts.listEvaluations ||
		opts.listFailures ||
		opts.listMessages
}

func sessionWriteUtilityRequested(opts cliOptions) bool {
	return opts.recordFailure != "" ||
		opts.recordEvaluation != "" ||
		opts.recordArtifact != "" ||
		opts.feedbackApplyConfig != ""
}

func sessionLocalUtilityRequested(opts cliOptions) bool {
	return opts.mergeArtifactsPath != "" ||
		opts.feedbackProposals ||
		opts.agentMemorySearch != "" ||
		len(opts.agentMemoryIndexFiles) > 0
}

func workflowExecutionUtilityRequested(opts cliOptions) bool {
	return opts.bashCommand != "" ||
		opts.asyncRun ||
		len(opts.spawnAgentSpecs) > 0 ||
		opts.speculateRun ||
		opts.reviewRun
}

func providerInspectionUtilityRequested(opts cliOptions) bool {
	return opts.listModels ||
		opts.doctor
}
