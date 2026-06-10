package main

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/charmbracelet/bubbles/textarea"
	tea "github.com/charmbracelet/bubbletea"

	"github.com/tommoulard/atteler/pkg/contextref"
	"github.com/tommoulard/atteler/pkg/events"
	"github.com/tommoulard/atteler/pkg/llm"
	"github.com/tommoulard/atteler/pkg/modelroute"
	"github.com/tommoulard/atteler/pkg/promptcomplete"
	attshell "github.com/tommoulard/atteler/pkg/shell"
)

// Init returns the initial command.
func (m model) Init() tea.Cmd {
	return textarea.Blink
}

// Update handles incoming messages.
func (m model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	msg = normalizeTerminalControlKeyMsg(msg)
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.width = msg.Width
		m.textarea.SetWidth(msg.Width)

		return m.updateTextarea(msg)

	case modelsLoadedMsg:
		return m.updateModelsLoaded(msg)

	case fzfModelSelectedMsg:
		return m.updateFZFModelSelected(msg)

	case modelPreferenceSavedMsg, promptSuggestionPreferenceSavedMsg:
		return m.updatePreferenceSaved(msg)

	case tea.KeyMsg:
		// When waiting for user confirmation from the agent loop, intercept Y/N
		// before any other key handler.
		if m.checkpointResponseCh != nil {
			return m.handleCheckpointKey(msg)
		}

		if m.scopePickerOpen {
			return m.updateModelScopePicker(msg)
		}

		if m.pickerOpen {
			return m.updatePicker(msg)
		}

		return m.updateChat(msg)

	case llmResponseMsg, llmEventLineMsg, llmToolOutputMsg:
		return m.updateLLMMessage(msg)

	case shellResultMsg, shellOutputMsg:
		return m.updateShellMessage(msg)

	case idleSuggestionRequestMsg, idleSuggestionMsg:
		return m.updateIdleSuggestionMessage(msg)

	case sessionSavedMsg, hookMsg:
		return m.updateLifecycleMessage(msg)

	case taskTickMsg:
		return m.updateTaskTick(msg)

	case loopCheckpointMsg:
		return m.updateLoopCheckpoint(msg)
	}

	return m.updateTextarea(msg)
}

func normalizeTerminalControlKeyMsg(msg tea.Msg) tea.Msg {
	if keyMsg, ok := terminalControlKeyMsg(msg); ok {
		return keyMsg
	}

	return msg
}

func (m model) updateLLMMessage(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case llmResponseMsg:
		return m.updateLLMResponse(msg)
	case llmEventLineMsg, llmToolOutputMsg:
		return m.updateLLMLiveMessage(msg)
	default:
		return m, nil
	}
}

func (m model) updateLLMLiveMessage(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case llmEventLineMsg:
		return m, tea.Sequence(
			tea.Println(dimStyle.Render(msg.line)),
			listenForLLMLiveMessage(msg.liveCh),
		)
	case llmToolOutputMsg:
		return m, tea.Sequence(
			tea.Printf("%s", formatLLMToolOutputChunk(msg)),
			listenForLLMLiveMessage(msg.liveCh),
		)
	default:
		return m, nil
	}
}

func formatLLMToolOutputChunk(msg llmToolOutputMsg) string {
	if msg.stream == string(attshell.OutputStreamStderr) {
		return errStyle.Render(msg.data)
	}

	return msg.data
}

func (m model) updateShellMessage(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case shellResultMsg:
		return m.updateShellResult(msg)
	case shellOutputMsg:
		return m.updateShellOutput(msg)
	default:
		return m, nil
	}
}

func (m model) updateIdleSuggestionMessage(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case idleSuggestionRequestMsg:
		return m.updateIdleSuggestionRequest(msg)
	case idleSuggestionMsg:
		return m.updateIdleSuggestion(msg)
	default:
		return m, nil
	}
}

func (m model) updateModelsLoaded(msg modelsLoadedMsg) (tea.Model, tea.Cmd) {
	if msg.fetchID != m.modelFetchID {
		return m, nil
	}

	if msg.provider == "" {
		m.pickerLoading = false

		m.modelFetchesPending = 0
		if msg.err != nil {
			m.pickerOpen = false
			return m, tea.Println(errStyle.Render("Error loading models: " + msg.err.Error()))
		}

		if externalModelPickerAllowed(m.autonomy) {
			if fzfPath, ok := findFZF(); ok {
				m.pickerOpen = false

				return m, tea.Batch(
					emitHook(m.ctx, m.hookRunner, events.Event{
						Type:        events.CommandExecute,
						SessionID:   m.sessionState.ID,
						SessionPath: m.sessionPath,
						Agent:       m.selectedAgent,
						Model:       m.selectedModel,
						Metadata: map[string]string{
							"autonomy": m.autonomy.String(),
							"command":  "fzf",
						},
					}),
					runFZFModelPicker(m.ctx, fzfPath, msg.items, attshell.AuditContext{
						Caller:      "atteler.fzf_model_picker",
						SessionID:   m.sessionState.ID,
						SessionPath: m.sessionPath,
						Autonomy:    m.autonomy.String(),
					}),
				)
			}
		}

		m.pickerItems = msg.items
		m.pickerCursor = 0

		return m, nil
	}

	if m.modelFetchesPending > 0 {
		m.modelFetchesPending--
	}

	m.pickerLoading = m.modelFetchesPending > 0
	if msg.err != nil {
		if len(m.pickerItems) == 0 && !m.pickerLoading {
			m.pickerOpen = false
			return m, tea.Println(errStyle.Render("Error loading models: " + msg.err.Error()))
		}

		return m, nil
	}

	m.pickerItems = mergeProviderPickerItems(m.pickerItems, msg.provider, msg.items)
	if m.pickerCursor >= len(m.pickerItems) {
		m.pickerCursor = max(0, len(m.pickerItems)-1)
	}

	return m, nil
}

func (m model) updateFZFModelSelected(msg fzfModelSelectedMsg) (tea.Model, tea.Cmd) {
	if msg.err != nil {
		return m, tea.Println(errStyle.Render("Error selecting model: " + msg.err.Error()))
	}

	if !msg.selected {
		return m, nil
	}

	return m.openModelScopePicker(msg.item)
}

func (m model) updateModelPreferenceSaved(msg modelPreferenceSavedMsg) (tea.Model, tea.Cmd) {
	if msg.err != nil {
		return m, tea.Println(errStyle.Render("Warning: " + msg.err.Error()))
	}

	return m, nil
}

func (m model) updatePromptSuggestionPreferenceSaved(msg promptSuggestionPreferenceSavedMsg) (tea.Model, tea.Cmd) {
	if msg.err != nil {
		return m, tea.Println(errStyle.Render("Warning: " + msg.err.Error()))
	}

	return m, nil
}

func (m model) updatePreferenceSaved(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case modelPreferenceSavedMsg:
		return m.updateModelPreferenceSaved(msg)
	case promptSuggestionPreferenceSavedMsg:
		return m.updatePromptSuggestionPreferenceSaved(msg)
	default:
		return m, nil
	}
}

func (m model) updateSessionSaved(msg sessionSavedMsg) (tea.Model, tea.Cmd) {
	if msg.err != nil {
		return m, tea.Println(errStyle.Render("Warning: " + msg.err.Error()))
	}

	return m, nil
}

func (m model) updateLifecycleMessage(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case sessionSavedMsg:
		return m.updateSessionSaved(msg)
	case hookMsg:
		return m.updateHook(msg)
	default:
		return m, nil
	}
}

func (m model) updateHook(msg hookMsg) (tea.Model, tea.Cmd) {
	var cmds []tea.Cmd
	if msg.line != "" {
		cmds = append(cmds, tea.Println(dimStyle.Render(msg.line)))
	}

	if msg.err != nil {
		cmds = append(cmds, tea.Println(errStyle.Render("Warning: "+msg.err.Error())))
	}

	return m, tea.Batch(cmds...)
}

func (m model) updateTaskTick(msg taskTickMsg) (tea.Model, tea.Cmd) {
	if !m.waiting || msg.id != m.runningTaskID {
		return m, nil
	}

	if m.checkpointResponseCh != nil {
		return m, tea.SetWindowTitle(terminalIdleTitle())
	}

	m.terminalTitleFrame++

	return m, tea.Batch(taskTickCmd(msg.id), tea.SetWindowTitle(m.terminalWorkingTitle()))
}

// updateLoopCheckpoint handles the agent loop requesting confirmation. It
// shows a prompt and waits for the user to press Y or N.
func (m model) updateLoopCheckpoint(msg loopCheckpointMsg) (tea.Model, tea.Cmd) {
	m.checkpointResponseCh = msg.responseCh
	m.checkpointRequestCh = msg.requestCh
	m.checkpointPrompt = msg.request.prompt

	if m.checkpointPrompt == "" {
		m.checkpointPrompt = fmt.Sprintf("Agent loop reached %d iterations. Continue? [Y/n] ", msg.request.iterations)
	}

	return m, tea.Batch(tea.Println(warnStyle.Render(m.checkpointPrompt)), tea.SetWindowTitle(terminalIdleTitle()))
}

// handleCheckpointKey handles Y/N key presses during an agent-loop prompt.
// Y (or Enter) continues the loop, N (or Esc) stops it.
func (m model) handleCheckpointKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	ch := m.checkpointResponseCh
	reqCh := m.checkpointRequestCh

	switch msg.String() {
	case "y", "Y", "enter":
		m.checkpointResponseCh = nil
		m.checkpointRequestCh = nil
		m.checkpointPrompt = ""

		ch <- true

		// Re-listen for the next checkpoint or require-confirm tool call.
		return m, tea.Batch(
			tea.Println(dimStyle.Render("Continuing agent loop...")),
			tea.SetWindowTitle(m.terminalWorkingTitle()),
			taskTickCmd(m.runningTaskID),
			relistenForCheckpoint(reqCh, ch),
		)

	case "n", "N", "esc":
		m.checkpointResponseCh = nil
		m.checkpointRequestCh = nil
		m.checkpointPrompt = ""

		ch <- false

		return m, tea.Batch(tea.Println(warnStyle.Render("Stopping agent loop.")), tea.SetWindowTitle(m.terminalWorkingTitle()))

	default:
		// Ignore other keys; keep waiting for Y/N.
		return m, nil
	}
}

// relistenForCheckpoint wraps the response channel back into a bidirectional
// chan so listenForCheckpoint can be reused for subsequent confirmations.
func relistenForCheckpoint(requestCh <-chan agentLoopConfirmRequest, responseCh chan<- bool) tea.Cmd {
	return func() tea.Msg {
		request, ok := <-requestCh
		if !ok {
			return nil
		}

		return loopCheckpointMsg{
			request:    request,
			responseCh: responseCh,
			requestCh:  requestCh,
		}
	}
}

func (m model) updateTextarea(msg tea.Msg) (tea.Model, tea.Cmd) {
	var cmds []tea.Cmd

	if stringer, ok := msg.(fmt.Stringer); ok &&
		m.checkpointResponseCh == nil &&
		!m.pickerOpen &&
		!m.scopePickerOpen &&
		isTerminalInputNewlineMsg(stringer) {
		return m.insertInputNewline()
	}

	if !m.pickerOpen && !m.scopePickerOpen {
		var taCmd tea.Cmd

		m.textarea, taCmd = m.textarea.Update(msg)
		cmds = append(cmds, taCmd)
	}

	return m, tea.Batch(cmds...)
}

// updateChat handles key events in normal chat mode.
func (m model) updateChat(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	var cmds []tea.Cmd

	if next, cmd, handled := m.handleChatCommand(msg.String()); handled {
		return next, cmd
	}

	// Propagate to the textarea in chat mode. While an agent is running, this
	// keeps the input editable so the next prompt can be queued.
	if !m.pickerOpen && !m.scopePickerOpen {
		m.completionOpen = false
		m.completionItems = nil

		var taCmd tea.Cmd

		m.textarea, taCmd = m.textarea.Update(msg)
		cmds = append(cmds, taCmd)
		m.promptHistoryCursor = -1
		m.promptHistoryDraft = ""
		m.clearIdleSuggestion()

		if cmd := m.scheduleIdleSuggestion(); cmd != nil {
			cmds = append(cmds, cmd)
		}
	}

	return m, tea.Batch(cmds...)
}

func (m model) handleChatCommand(keyName string) (tea.Model, tea.Cmd, bool) {
	switch keyName {
	case "ctrl+d":
		if m.waiting {
			return m, nil, true
		}

		m.quitting = true

		return m, tea.Quit, true
	case keyCtrlC:
		return m.handleCtrlC()
	case "ctrl+o":
		if m.waiting {
			return m, nil, true
		}

		return m.openModelPicker()
	case "ctrl+r":
		next, cmd := m.revampPrompt()
		return next, cmd, true
	case "ctrl+z":
		next, cmd := m.undoPromptRevamp()
		return next, cmd, true
	case keyUp:
		if next, ok := m.moveCursorForHistoryEdge(keyUp); ok {
			return next, nil, true
		}

		next, ok := m.navigatePromptHistory(1)

		return next, nil, ok
	case keyDown:
		if next, ok := m.moveCursorForHistoryEdge(keyDown); ok {
			return next, nil, true
		}

		next, ok := m.navigatePromptHistory(-1)

		return next, nil, ok
	case keyEnter:
		next, cmd := m.submitInput()
		return next, cmd, true
	case "tab":
		return m.acceptCompletion()
	default:
		return m, nil, false
	}
}

func (m model) handleCtrlC() (tea.Model, tea.Cmd, bool) {
	if m.textarea.Value() != "" {
		m.textarea.Reset()
		m.promptHistoryCursor = -1
		m.promptHistoryDraft = ""
		m.completionOpen = false
		m.completionItems = nil
		m.clearIdleSuggestion()
		m.revampUndoActive = false
		m.revampUndo = ""

		return m, nil, true
	}

	if m.waiting {
		if m.cancel != nil {
			m.cancel()
			m.cancel = nil
		}

		elapsed := m.finishRunningTask(time.Now())
		m.waiting = false

		return m, tea.Batch(
			tea.Println(errStyle.Render("(canceled"+taskDurationSuffix(elapsed)+")")),
			tea.SetWindowTitle(terminalIdleTitle()),
		), true
	}

	m.quitting = true

	return m, tea.Quit, true
}

func (m model) acceptCompletion() (tea.Model, tea.Cmd, bool) {
	items, ok := completionCandidates(m.textarea.Value(), m.agentRegistry, m.contextOptions.Root, 8)
	if ok && len(items) > 0 {
		m.completionOpen = true
		m.completionItems = items
		m.completionCursor = 0
		m.textarea.SetValue(applyCompletionCandidate(m.textarea.Value(), items[0].value))
		m.textarea.CursorEnd()

		return m, nil, true
	}

	if suggestion, ok := m.promptSuggestion(); ok {
		m.textarea.SetValue(applyPromptSuggestion(m.textarea.Value(), suggestion))
		m.textarea.CursorEnd()
		m.clearIdleSuggestion()

		return m, nil, true
	}

	if suffix, ok := m.visibleIdleSuggestion(); ok {
		m.textarea.SetValue(m.textarea.Value() + suffix)
		m.textarea.CursorEnd()
		m.clearIdleSuggestion()

		return m, nil, true
	}

	return m.forceIdleSuggestion()
}

func (m model) revampPrompt() (tea.Model, tea.Cmd) {
	if m.waiting {
		return m, nil
	}

	current := m.textarea.Value()

	revamped, ok := promptcomplete.Revamp(current, promptcomplete.RevampStyleDetailed)
	if !ok || revamped == strings.TrimSpace(current) {
		return m, nil
	}

	m.revampUndo = current
	m.revampUndoActive = true
	m.textarea.SetValue(revamped)
	m.textarea.CursorEnd()

	return m, tea.Println(dimStyle.Render("(prompt revamped; Ctrl+Z to undo)"))
}

func (m model) undoPromptRevamp() (tea.Model, tea.Cmd) {
	if !m.revampUndoActive {
		return m, nil
	}

	m.textarea.SetValue(m.revampUndo)
	m.textarea.CursorEnd()
	m.revampUndo = ""
	m.revampUndoActive = false

	return m, tea.Println(dimStyle.Render("(prompt revamp undone)"))
}

func (m model) moveCursorForHistoryEdge(direction string) (model, bool) {
	value := m.textarea.Value()
	if value == "" {
		return m, false
	}

	cursor := textareaCursorOffset(m.textarea)
	if cursor <= 0 || cursor >= len(value) {
		return m, false
	}

	switch direction {
	case keyUp:
		m.textarea.CursorStart()
	case keyDown:
		m.textarea.CursorEnd()
	default:
		return m, false
	}

	return m, true
}

func (m model) navigatePromptHistory(delta int) (model, bool) {
	if len(m.promptHistory) == 0 {
		return m, false
	}

	if delta > 0 {
		switch {
		case m.promptHistoryCursor == -1:
			m.promptHistoryDraft = m.textarea.Value()
			m.promptHistoryCursor = 0
		case m.promptHistoryCursor < len(m.promptHistory)-1:
			m.promptHistoryCursor++
		default:
			return m, true
		}

		m.textarea.SetValue(m.promptHistory[m.promptHistoryCursor])
		m.textarea.CursorEnd()

		return m, true
	}

	if m.promptHistoryCursor == -1 {
		return m, false
	}

	if m.promptHistoryCursor > 0 {
		m.promptHistoryCursor--
		m.textarea.SetValue(m.promptHistory[m.promptHistoryCursor])
	} else {
		m.promptHistoryCursor = -1
		m.textarea.SetValue(m.promptHistoryDraft)
		m.promptHistoryDraft = ""
	}

	m.textarea.CursorEnd()

	return m, true
}

// submitInput handles the enter key — sends user input to the LLM.
func (m model) submitInput() (tea.Model, tea.Cmd) {
	input := strings.TrimSpace(m.textarea.Value())
	if input == "" {
		return m, nil
	}

	if strings.HasPrefix(input, "/") {
		next, cmd, handled := m.handleSlashCommand(input)
		if handled {
			next.textarea.Reset()
			return next, cmd
		}
	}

	m.promptHistory = prependPromptHistory(input, m.promptHistory, maxPromptHistoryEntries)
	m.promptHistoryCursor = -1
	m.promptHistoryDraft = ""
	m.revampUndoActive = false
	m.revampUndo = ""
	m.clearIdleSuggestion()
	m.textarea.Reset()

	if m.waiting {
		m.queuedPrompts = append(m.queuedPrompts, input)
		return m, tea.Println(dimStyle.Render(fmt.Sprintf("(queued follow-up #%d)", len(m.queuedPrompts))))
	}

	return m.submitPrompt(input)
}

func routeDecisionErrorCommand(
	ctx context.Context,
	hooks *events.Runner,
	sessionID string,
	sessionPath string,
	agentName string,
	routeDecision *modelroute.Decision,
	err error,
) tea.Cmd {
	cmds := []tea.Cmd{tea.Println(errStyle.Render("Error: " + err.Error()))}
	if event, ok := routeDecisionEvent(sessionID, sessionPath, agentName, "", routeDecision); ok {
		cmds = append(cmds, emitHook(ctx, hooks, event))
	}

	return tea.Batch(cmds...)
}

// submitPrompt sends a prompt that is already detached from the textarea.
func (m model) submitPrompt(input string) (tea.Model, tea.Cmd) {
	if strings.HasPrefix(input, "!") {
		return m.runShellCommand(strings.TrimSpace(input[1:]))
	}

	activeAgent, prompt, err := m.resolveAgent(input)
	if err != nil {
		return m, tea.Println(errStyle.Render("Error: " + err.Error()))
	}

	input = prompt

	nextHistory := append(append([]llm.Message(nil), m.history...), llm.Message{
		Role:    llm.RoleUser,
		Content: input,
	})

	requestModel := m.selectedModel
	fallbackModels := append([]string(nil), m.fallbackModels...)

	if activeAgent.ok && !m.modelLocked {
		requestModel, fallbackModels = effectiveAgentModelSelection(m.selectedModel, m.fallbackModels, activeAgent)
	}

	contextOptions := contextOptionsForRequestModels(m.contextOptions, m.registry, requestModel, fallbackModels)

	requestMessages, _, inlineEvents, err := expandReferences(nextHistory, contextOptions)
	if err != nil {
		return m, m.inlineReferenceErrorCommand(requestModel, fallbackModels, nextHistory, activeAgent.name, inlineEvents, err)
	}

	// Print the user message above the input area.
	line := userLabel.Render("You") + " " + input
	if activeAgent.name != "" {
		line = userLabel.Render("You") + dimStyle.Render(" (@"+activeAgent.name+") ") + input
	}

	msgs := make([]llm.Message, len(requestMessages))
	copy(msgs, requestMessages)

	generation := generationForRequest(m.generationDefaults, m.generationOverrides, activeAgent)
	routeGlobalReferenceContext := configuredReferenceContextForRequest(m.ctx, m.configuredReferences, configuredReferenceContext{
		Content:   m.referenceContext,
		Manifest:  m.referenceManifest,
		Estimator: m.referenceContextEstimator,
	}, contextOptions)
	routeReferenceContext := buildReferenceContextWithManifest(m.ctx, routeGlobalReferenceContext, activeAgent, contextOptions)
	useTools := m.requestUsesTools()
	budgetMessages := requestMessagesForBudget(requestModel, msgs, activeAgent, generation, routeReferenceContext.Content, useTools)

	requestModel, fallbackModels, routeDecision, err := requestModelRoutingAndFallbacks(
		m.ctx,
		m.registry,
		m.selectedModel,
		m.modelLocked,
		m.fallbackModels,
		activeAgent,
		requestModel,
		fallbackModels,
		routeCompleteParamsForRequest(
			requestModel,
			budgetMessages,
			generation,
			activeAgent,
			m.executionMode != executionModePlan,
		),
		routeProfileForMessages(budgetMessages, generation),
		routeTelemetryFromRegistry(m.registry),
		routeAvailabilityFromRegistryWithRefresh(m.ctx, m.registry, effectiveRouteCandidateChain(m.selectedModel, m.fallbackModels, activeAgent, m.modelLocked)),
	)
	if err != nil {
		return m, routeDecisionErrorCommand(m.ctx, m.hookRunner, m.sessionState.ID, m.sessionPath, activeAgent.name, routeDecision, err)
	}

	contextOptions = contextOptionsForRequestModels(m.contextOptions, m.registry, requestModel, fallbackModels)

	requestMessages, refs, inlineEvents, err := expandReferences(nextHistory, contextOptions)
	if err != nil {
		return m, m.inlineReferenceErrorCommand(requestModel, fallbackModels, nextHistory, activeAgent.name, inlineEvents, err)
	}

	msgs = make([]llm.Message, len(requestMessages))
	copy(msgs, requestMessages)

	globalReferenceContext := configuredReferenceContextForRequest(m.ctx, m.configuredReferences, configuredReferenceContext{
		Content:   m.referenceContext,
		Manifest:  m.referenceManifest,
		Estimator: m.referenceContextEstimator,
	}, contextOptions)
	m.referenceContext = globalReferenceContext.Content
	m.referenceManifest = globalReferenceContext.Manifest
	m.referenceContextEstimator = globalReferenceContext.Estimator

	referenceContext := buildReferenceContextWithManifest(m.ctx, globalReferenceContext, activeAgent, contextOptions)
	generatedSkillRefCtx := generatedSkillReferenceContextWithManifest(
		input,
		m.skillLearningStoreDir,
		m.skillLearningSkillDir,
		m.skillLearningEnabled,
		contextOptions,
	)
	referenceContext.Content = appendReferenceContext(
		referenceContext.Content,
		generatedSkillRefCtx.Content,
	)

	referenceContext.Manifest = mergeReferenceManifests(referenceContext.Manifest, generatedSkillRefCtx.Manifest)
	if referenceContext.Estimator == "" {
		referenceContext.Estimator = generatedSkillRefCtx.Estimator
	}

	referenceContext = appendWorkspaceVectorReferenceContextForAutonomy(
		m.ctx,
		referenceContext,
		m.autonomy,
		firstNonEmpty(m.contextOptions.Root, m.cwd),
		m.vectorConfig,
		input,
		false,
		contextOptions,
	)

	preflightMessages := requestMessagesForBudget(requestModel, msgs, activeAgent, generation, referenceContext.Content, useTools)
	if err := validateRequestBudgetWithFallbacks(m.registry, requestModel, fallbackModels, preflightMessages, m.maxInputTokens); err != nil {
		manifestEvent := requestContextManifestEvent(newRequestContextManifestForModelsWithInlineEvents(
			m.registry,
			requestModel,
			fallbackModels,
			preflightMessages,
			m.maxInputTokens,
			inlineEvents,
			referenceContext.Manifest,
		))
		manifestEvent.SessionID = m.sessionState.ID
		manifestEvent.SessionPath = m.sessionPath
		manifestEvent.Agent = activeAgent.name
		setExplicitContextManifestEventModel(&manifestEvent, requestModel)

		return m, tea.Sequence(
			emitHook(m.ctx, m.hookRunner, manifestEvent),
			tea.Println(errStyle.Render("Error: "+err.Error())),
		)
	}

	// Launch the LLM call.
	// cancel is stored in m.cancel and invoked from handleCtrlC and
	// updateLLMResponse once the request finishes; gosec can't see that.
	ctx, cancel := context.WithCancel(m.ctx) //nolint:gosec // see comment above
	m.cancel = cancel
	tickCmd := m.startRunningTask("LLM")

	m.history = nextHistory
	m.sessionState.Messages = append([]llm.Message(nil), m.history...)
	m.sessionState.AgentLoopBudget = m.agentLoopBudget
	m.sessionState.Autonomy = m.autonomy.String()

	m.sessionState.DefaultAgent = activeAgent.name
	if requestModel != "" {
		m.sessionState.DefaultModel = requestModel
	} else if m.selectedModel != "" {
		m.sessionState.DefaultModel = m.selectedModel
	}

	m.sessionState.DefaultReasoningLevel = strings.TrimSpace(m.generationOverrides.ReasoningLevel)
	m.sessionState.DefaultModelMode = strings.TrimSpace(m.generationOverrides.ModelMode)

	confirmCh := make(chan agentLoopConfirmRequest, 1)
	responseCh := make(chan bool, 1)
	liveCh := make(chan tea.Msg, 64)

	request := llmRequest{
		eventBase: events.Event{
			SessionID:   m.sessionState.ID,
			SessionPath: m.sessionPath,
			Agent:       activeAgent.name,
			Model:       requestModel,
		},
		hookRunner:                  m.hookRunner,
		agent:                       activeAgent.agent,
		hasAgent:                    activeAgent.ok,
		model:                       requestModel,
		agentLoopCheckpointPath:     agentLoopCheckpointPathForAutonomy(m.sessionPath, m.autonomy),
		referenceContext:            referenceContext.Content,
		referenceManifest:           referenceContext.Manifest,
		workingDir:                  m.cwd,
		messages:                    msgs,
		fallbackModels:              fallbackModels,
		generation:                  generation,
		agentLoopBudget:             m.agentLoopBudget,
		autonomy:                    m.autonomy,
		agentLoopCheckpointInterval: m.agentLoopCheckpointInterval,
		maxInputTokens:              m.maxInputTokens,
		routeDecision:               routeDecision,
		refs:                        refs,
		inlineReferenceEvents:       inlineEvents,
		useTools:                    useTools,
		confirmRequestCh:            confirmCh,
		confirmResponseCh:           responseCh,
		liveCh:                      liveCh,
	}

	return m, m.submitPromptRequestCommand(
		line,
		input,
		requestModel,
		activeAgent,
		refs,
		routeDecision,
		callLLM(ctx, m.registry, request),
		tickCmd,
		confirmCh,
		responseCh,
		liveCh,
	)
}

func (m model) submitPromptRequestCommand(
	line string,
	input string,
	requestModel string,
	activeAgent agentSelection,
	refs []contextref.Reference,
	routeDecision *modelroute.Decision,
	llmCmd tea.Cmd,
	tickCmd tea.Cmd,
	confirmCh chan agentLoopConfirmRequest,
	responseCh chan bool,
	liveCh <-chan tea.Msg,
) tea.Cmd {
	cmds := []tea.Cmd{
		tea.Println(line),
	}
	if len(refs) > 0 {
		cmds = append(cmds, tea.Println(dimStyle.Render("Context: "+referenceSummary(refs))))
	}

	for _, ref := range refs {
		cmds = append(
			cmds,
			emitFileRead(m.ctx, m.hookRunner, m.sessionState.ID, m.sessionPath, activeAgent.name, m.sessionState.DefaultModel, ref),
			emitContextAdd(m.ctx, m.hookRunner, m.sessionState.ID, m.sessionPath, activeAgent.name, m.sessionState.DefaultModel, ref),
		)
	}

	if activeAgent.ok {
		cmds = append(cmds, emitAgentExecute(m.ctx, m.hookRunner, m.sessionState.ID, m.sessionPath, activeAgent.name, requestModel))
	}

	if event, ok := routeDecisionEvent(m.sessionState.ID, m.sessionPath, activeAgent.name, requestModel, routeDecision); ok {
		cmds = append(cmds, emitHook(m.ctx, m.hookRunner, event))
	}

	cmds = append(
		cmds,
		saveSession(m.ctx, m.sessionStore, m.sessionState, m.hookRunner),
		emitHook(m.ctx, m.hookRunner, events.Event{
			Type:        events.UserMessage,
			SessionID:   m.sessionState.ID,
			SessionPath: m.sessionPath,
			Agent:       activeAgent.name,
			Model:       m.sessionState.DefaultModel,
			Role:        string(llm.RoleUser),
			Content:     input,
			Metadata:    referenceMetadata(refs),
		}),
		llmCmd,
	)

	return tea.Batch(tea.Sequence(cmds...), tickCmd, listenForCheckpoint(confirmCh, responseCh), listenForLLMLiveMessage(liveCh))
}

func (m model) requestUsesTools() bool {
	if m.executionMode == executionModePlan {
		return false
	}

	return m.autonomy.AllowsAgentTools()
}

func (m model) inlineReferenceErrorCommand(
	requestModel string,
	fallbackModels []string,
	messages []llm.Message,
	agentName string,
	inlineEvents []contextref.ReferenceEvent,
	err error,
) tea.Cmd {
	cmds := []tea.Cmd{tea.Println(errStyle.Render("Error: " + err.Error()))}
	if len(inlineEvents) == 0 {
		return tea.Sequence(cmds...)
	}

	configuredManifest := omitIncludedReferenceManifestEntries(
		m.referenceManifest,
		"request assembly aborted before configured reference context was sent",
	)
	manifestEvent := requestContextManifestEvent(newRequestContextManifestForModelsWithInlineEvents(
		m.registry,
		requestModel,
		fallbackModels,
		messages,
		m.maxInputTokens,
		inlineEvents,
		configuredManifest,
	))
	manifestEvent.SessionID = m.sessionState.ID
	manifestEvent.SessionPath = m.sessionPath
	manifestEvent.Agent = agentName
	setExplicitContextManifestEventModel(&manifestEvent, requestModel)

	return tea.Sequence(append([]tea.Cmd{emitHook(m.ctx, m.hookRunner, manifestEvent)}, cmds...)...)
}
