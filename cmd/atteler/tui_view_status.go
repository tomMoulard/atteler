package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/charmbracelet/bubbles/textarea"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/x/ansi"

	"github.com/tommoulard/atteler/pkg/contextref"
	"github.com/tommoulard/atteler/pkg/events"
	"github.com/tommoulard/atteler/pkg/llm"
	"github.com/tommoulard/atteler/pkg/privacy"
	"github.com/tommoulard/atteler/pkg/promptcomplete"
	"github.com/tommoulard/atteler/pkg/session"
)

func (m *model) startRunningTask(label string) tea.Cmd {
	m.waiting = true
	m.runningTaskID++
	m.runningTaskLabel = label
	m.runningTaskStarted = time.Now()
	m.terminalTitleFrame = 0

	return tea.Batch(tea.SetWindowTitle(m.terminalWorkingTitle()), taskTickCmd(m.runningTaskID))
}

func (m *model) finishRunningTask(completedAt time.Time) time.Duration {
	if m.runningTaskStarted.IsZero() {
		m.clearRunningTask()
		return 0
	}

	if completedAt.IsZero() {
		completedAt = time.Now()
	}

	elapsed := max(completedAt.Sub(m.runningTaskStarted), 0)

	m.clearRunningTask()

	return elapsed
}

func (m *model) clearRunningTask() {
	m.runningTaskLabel = ""
	m.runningTaskStarted = time.Time{}
}

func taskTickCmd(id int) tea.Cmd {
	return tea.Tick(taskTickInterval, func(time.Time) tea.Msg {
		return taskTickMsg{id: id}
	})
}

func terminalIdleTitle() string {
	return "atteler"
}

func (m model) terminalWorkingTitle() string {
	label := strings.TrimSpace(m.runningTaskLabel)
	if label == "" {
		label = "working"
	}

	frame := "-"
	if len(terminalTitleSpinnerFrames) > 0 {
		frame = terminalTitleSpinnerFrames[m.terminalTitleFrame%len(terminalTitleSpinnerFrames)]
	}

	return frame + " atteler — " + label
}

// updatePicker handles key events while the model picker is open.

// View renders only the current input area (past messages are already printed).
func (m model) View() string {
	if m.quitting {
		return ""
	}

	if m.pickerOpen {
		return m.viewPicker()
	}

	if m.scopePickerOpen {
		return m.viewModelScopePicker()
	}

	status := m.statusLine()

	if m.waiting {
		waiting := statusStyle.Render(m.waitingStatus())
		if status != "" {
			status = status + "\n" + waiting
		} else {
			status = waiting
		}

		if m.completionOpen && len(m.completionItems) > 0 {
			status += "\n" + m.viewCompletions()
		}

		return status + "\n" + m.viewInput()
	}

	if m.completionOpen && len(m.completionItems) > 0 {
		status += "\n" + m.viewCompletions()
	}

	return status + "\n" + m.viewInput()
}

func (m model) statusLine() string {
	parts := make([]string, 0, 5)

	if m.selectedAgent != "" {
		parts = append(parts, "agent:"+m.selectedAgent)
	}

	if m.executionMode != "" {
		parts = append(parts, "mode:"+m.executionMode)
	}

	if modelLabel := m.modelStatusLabel(); modelLabel != "" {
		parts = append(parts, "model:"+modelLabel)
	}

	if reasoningLabel := m.reasoningStatusLabel(); reasoningLabel != "" {
		parts = append(parts, "effort:"+reasoningLabel)
	}

	if budget := formatAgentLoopBudgetCompact(m.agentLoopBudget); budget != "" {
		parts = append(parts, "budget:"+budget)
	}

	if ctx := m.contextUsage(); ctx != "" {
		parts = append(parts, ctx)
	}

	if suggestionLabel := m.idleSuggestionStatusLabel(); suggestionLabel != "" {
		parts = append(parts, "suggestion:"+suggestionLabel)
	}

	if m.promptContextStatus != "" {
		parts = append(parts, "promptctx:"+m.promptContextStatus)
	}

	if len(parts) == 0 {
		return ""
	}

	return dimStyle.Render("  [") + pickerSelectedStyle.Render(strings.Join(parts, " ")) + dimStyle.Render("]")
}

func (m model) idleSuggestionStatusLabel() string {
	status := strings.TrimSpace(m.idleSuggestionStatus)
	if status == "" {
		return ""
	}

	switch status {
	case idleSuggestionStatusSending, idleSuggestionStatusPendingForced:
		label := status
		if modelID := m.idleSuggestionModelStatusLabel(); modelID != "" {
			label += ":" + modelID
		}

		if contextSummary := strings.TrimSpace(m.idleSuggestionContext); contextSummary != "" {
			label += ":ctx=" + contextSummary
		}

		return label
	default:
		return status
	}
}

func (m model) idleSuggestionModelStatusLabel() string {
	modelID := strings.TrimSpace(m.idleSuggestionModel)

	provider := strings.TrimSpace(m.idleSuggestionProvider)
	if modelID == "" {
		modelID = m.modelStatusLabel()
	}

	if provider != "" && modelID != "" && !strings.Contains(modelID, "/") {
		modelID = provider + "/" + modelID
	}

	return strings.ReplaceAll(modelID, " ", "_")
}

func (m model) modelStatusLabel() string {
	if m.selectedModel == "" {
		return ""
	}

	label := m.selectedModel
	if m.selectedProvider != "" && !strings.Contains(label, "/") {
		label = m.selectedProvider + "/" + label
	}

	return label
}

func (m model) reasoningStatusLabel() string {
	if level := strings.TrimSpace(m.generationOverrides.ReasoningLevel); level != "" {
		return level
	}

	if m.selectedAgent != "" && m.agentRegistry != nil {
		if activeAgent, ok := m.agentRegistry.Get(m.selectedAgent); ok {
			if level := strings.TrimSpace(activeAgent.ReasoningLevel); level != "" {
				return level
			}
		}
	}

	return strings.TrimSpace(m.generationDefaults.ReasoningLevel)
}

func (m model) waitingStatus() string {
	return m.waitingStatusAt(time.Now())
}

func (m model) waitingStatusAt(now time.Time) string {
	parts := make([]string, 0, 3)

	if !m.runningTaskStarted.IsZero() {
		label := m.runningTaskLabel
		if label == "" {
			label = "task"
		}

		elapsed := max(now.Sub(m.runningTaskStarted), 0)

		parts = append(parts, label+" running for "+formatTaskDuration(elapsed))
	}

	if len(m.queuedPrompts) > 0 {
		parts = append(parts, fmt.Sprintf("%d queued", len(m.queuedPrompts)))
	}

	parts = append(parts, "Ctrl+C to cancel")

	return "  Thinking... (" + strings.Join(parts, ", ") + ")"
}

func (m model) viewInput() string {
	inputView := m.textarea.View()
	if suggestion, ok := m.promptSuggestion(); ok && !m.completionOpen {
		inputView = renderInlinePromptSuggestion(m.textarea, inputView, suggestion.Suffix)
	} else if suffix, ok := m.visibleIdleSuggestion(); ok && !m.completionOpen {
		inputView = renderInlinePromptSuggestion(m.textarea, inputView, suffix)
	}

	return inputView
}

func renderInlinePromptSuggestion(input textarea.Model, inputView, suffix string) string {
	if suffix == "" || inputView == "" {
		return inputView
	}

	lines := strings.Split(inputView, "\n")
	line := max(input.Line(), 0)

	if line >= len(lines) {
		line = len(lines) - 1
	}

	// The textarea pads the cursor line with trailing spaces interleaved
	// with ANSI escape codes. strings.TrimRight(…, " ") only removes
	// literal trailing spaces and misses those wrapped inside escape
	// sequences.  Strip ANSI first, trim whitespace to find the visible
	// content width, then use ansi.Truncate to cut the raw line at that
	// width so the suffix starts immediately after the typed text.
	visibleLen := ansi.StringWidth(strings.TrimRight(ansi.Strip(lines[line]), " "))
	lines[line] = ansi.Truncate(lines[line], visibleLen, "") + dimStyle.Render(suffix)

	return strings.Join(lines, "\n")
}

func (m model) viewCompletions() string {
	parts := make([]string, 0, len(m.completionItems))
	for i, item := range m.completionItems {
		label := item.label
		if item.kind != "" {
			label = item.kind + ":" + label
		}

		if i == m.completionCursor {
			label = pickerSelectedStyle.Render(label)
		}

		parts = append(parts, label)
	}

	return dimStyle.Render("  completions: ") + strings.Join(parts, dimStyle.Render("  "))
}

// viewPicker renders the model selection overlay.
// when the model is unknown or has no context window metadata.
func (m model) contextUsage() string {
	if m.selectedModel == "" {
		return ""
	}

	limit := 0
	if m.registry != nil {
		limit = m.registry.ContextWindow(m.selectedModel)
	}

	estimate, _ := estimateMessagesForModel(m.registry, m.selectedModel, m.history)

	used := estimate.UpperBoundTokens
	if limit > 0 {
		return "ctx≤" + formatTokenCount(used) + "/" + formatTokenCount(limit)
	}

	if used > 0 {
		return "ctx≤" + formatTokenCount(used)
	}

	return ""
}

func (m model) promptSuggestion() (promptcomplete.Suggestion, bool) {
	value := m.textarea.Value()
	if strings.TrimSpace(value) == "" {
		return promptcomplete.Suggestion{}, false
	}

	cursor := textareaCursorOffset(m.textarea)
	if cursor != len(value) {
		return promptcomplete.Suggestion{}, false
	}

	contextResult := m.promptCompletionContextResult(value)

	suggestion, ok := promptcomplete.Suggest(contextResult.Context, promptcomplete.Options{Limit: 1})
	if !ok || suggestion.Suffix == "" {
		return promptcomplete.Suggestion{}, false
	}

	if !promptSuggestionAppendable(value, suggestion) {
		return promptcomplete.Suggestion{}, false
	}

	return suggestion, true
}

func promptSuggestionAppendable(input string, suggestion promptcomplete.Suggestion) bool {
	if suggestion.ReplacementStart < 0 ||
		suggestion.ReplacementStart > suggestion.ReplacementEnd ||
		suggestion.ReplacementEnd > len(input) {
		return false
	}

	current := input[suggestion.ReplacementStart:suggestion.ReplacementEnd]

	return strings.HasPrefix(strings.ToLower(suggestion.Text), strings.ToLower(current))
}

func (m model) promptCompletionContextResult(input string) promptCompletionContextResult {
	return promptCompletionContextInteractive(m.ctx, appState{
		agentRegistry:      m.agentRegistry,
		sessionStore:       m.sessionStore,
		sessionState:       m.sessionState,
		contextOptions:     m.contextOptions,
		selectedAgent:      m.selectedAgent,
		cwd:                m.cwd,
		worktreeInfo:       m.worktreeInfo,
		promptContextCache: m.promptContextCache,
	}, input, true)
}

func (m *model) refreshPromptContextStatus(input string) {
	if strings.TrimSpace(input) == "" {
		m.promptContextStatus = ""

		return
	}

	if strings.TrimSpace(m.cwd) == "" &&
		strings.TrimSpace(m.contextOptions.Root) == "" &&
		m.worktreeInfo == nil {
		m.promptContextStatus = ""

		return
	}

	contextResult := m.promptCompletionContextResult(input)
	m.promptContextStatus = promptContextStatusLabel(contextResult.Sources)
}

func (m *model) clearIdleSuggestion() {
	m.cancelIdleSuggestionRequest()
	m.idleSuggestionID++
	m.idleSuggestionInput = ""
	m.idleSuggestionText = ""
	m.idleSuggestionStatus = ""
	m.idleSuggestionProvider = ""
	m.idleSuggestionModel = ""
	m.idleSuggestionContext = ""
	m.promptContextStatus = ""
}

func (m *model) cancelIdleSuggestionRequest() {
	if m.idleSuggestionCancel == nil {
		return
	}

	m.idleSuggestionCancel()
	m.idleSuggestionCancel = nil
}

func (m *model) scheduleIdleSuggestion() tea.Cmd {
	value := m.textarea.Value()
	if m.waiting ||
		strings.TrimSpace(value) == "" ||
		textareaCursorOffset(m.textarea) != len(value) {
		if strings.TrimSpace(value) == "" {
			m.promptContextStatus = ""
		}

		return nil
	}

	if m.registry == nil || !m.modelBackedIdleSuggestionsEnabled() {
		m.refreshPromptContextStatus(value)

		return nil
	}

	if m.idleSuggestionBudgetBeforeRequestError() != nil {
		return nil
	}

	m.idleSuggestionID++
	m.idleSuggestionInput = value
	m.idleSuggestionText = ""
	m.idleSuggestionStatus = idleSuggestionStatusPending
	id := m.idleSuggestionID

	return tea.Tick(idleSuggestionDelay, func(time.Time) tea.Msg {
		return idleSuggestionRequestMsg{id: id, input: value}
	})
}

func (m model) forceIdleSuggestion() (tea.Model, tea.Cmd, bool) {
	value := m.textarea.Value()
	if m.idleSuggestionStatus == idleSuggestionStatusPendingForced &&
		m.idleSuggestionInput == value &&
		textareaCursorOffset(m.textarea) == len(value) {
		return m, nil, true
	}

	if strings.TrimSpace(value) == "" {
		m.idleSuggestionStatus = idleSuggestionStatusRejectedEmpty

		return m, tea.Println(errStyle.Render("Suggestion unavailable: prompt is empty.")), true
	}

	if textareaCursorOffset(m.textarea) != len(value) {
		m.idleSuggestionStatus = idleSuggestionStatusRejectedStale

		return m, nil, true
	}

	if m.waiting {
		m.idleSuggestionStatus = idleSuggestionStatusRejectedError

		return m, tea.Println(errStyle.Render("Suggestion unavailable while a task is running.")), true
	}

	if m.promptLocalOnly {
		m.idleSuggestionStatus = idleSuggestionStatusRejectedError

		return m, tea.Println(errStyle.Render("Suggestion unavailable: model-backed suggestions are disabled by --prompt-local-only.")), true
	}

	if !m.promptSuggestionConsent.allowsModelBacked() {
		m.idleSuggestionStatus = idleSuggestionStatusRejectedError

		return m, tea.Println(errStyle.Render("Suggestion unavailable: model-backed suggestions are local-only by default. Use /suggestions session, /suggestions folder, or /suggestions global to opt in.")), true
	}

	if m.registry == nil {
		m.idleSuggestionStatus = idleSuggestionStatusRejectedError

		return m, tea.Println(errStyle.Render("Suggestion unavailable: no model registry is configured.")), true
	}

	if err := m.idleSuggestionBudgetBeforeRequestError(); err != nil {
		m.idleSuggestionStatus = idleSuggestionStatusRejectedBudget

		return m, tea.Println(errStyle.Render("Suggestion unavailable: " + err.Error())), true
	}

	m.idleSuggestionID++

	next, cmd := m.startIdleSuggestionRequest(m.idleSuggestionID, value, true)

	return next, cmd, true
}

func (m model) updateIdleSuggestionRequest(msg idleSuggestionRequestMsg) (tea.Model, tea.Cmd) {
	if msg.id != m.idleSuggestionID ||
		msg.input != m.idleSuggestionInput {
		return m, nil
	}

	if msg.input != m.textarea.Value() ||
		textareaCursorOffset(m.textarea) != len(m.textarea.Value()) ||
		m.waiting ||
		m.registry == nil ||
		!m.modelBackedIdleSuggestionsEnabled() {
		m.idleSuggestionStatus = idleSuggestionStatusRejectedStale
		return m, nil
	}

	if err := m.idleSuggestionBudgetBeforeRequestError(); err != nil {
		m.idleSuggestionStatus = idleSuggestionStatusRejectedBudget
		return m, nil
	}

	return m.startIdleSuggestionRequest(msg.id, msg.input, false)
}

func (m model) startIdleSuggestionRequest(id int, input string, force bool) (tea.Model, tea.Cmd) {
	contextPayload := m.idleSuggestionContextPayload()
	provider, modelName, _ := resolveRegistryModel(m.registry, m.selectedModel)

	displayModel := m.selectedModel
	if displayModel == "" {
		displayModel = modelName
	}

	budget := normalizeIdleSuggestionBudget(m.idleSuggestionBudget)
	currentUsage := m.idleSuggestionUsage
	currentEstimatedTokens := m.idleSuggestionTokens
	currentCostUSD := m.idleSuggestionCostUSD

	requestCtx := m.ctx
	if requestCtx != nil {
		var cancel context.CancelFunc

		requestCtx, cancel = context.WithCancel(requestCtx)

		m.cancelIdleSuggestionRequest()
		m.idleSuggestionCancel = cancel
	}

	m.idleSuggestionID = id
	m.idleSuggestionInput = input
	m.idleSuggestionText = ""
	m.idleSuggestionProvider = provider
	m.idleSuggestionModel = displayModel
	m.idleSuggestionContext = contextPayload.Summary
	m.idleSuggestionRequests++
	m.idleSuggestionTimes = appendIdleSuggestionRequestTime(m.idleSuggestionTimes, time.Now())

	if force {
		m.idleSuggestionStatus = idleSuggestionStatusPendingForced
	} else {
		m.idleSuggestionStatus = idleSuggestionStatusSending
	}

	m.promptContextStatus = contextPayload.Status

	return m, requestIdleSuggestion(
		requestCtx,
		m.registry,
		m.selectedModel,
		m.fallbackModels,
		m.generationDefaults,
		m.generationOverrides,
		m.hookRunner,
		m.maxInputTokens,
		budget,
		currentUsage,
		currentEstimatedTokens,
		currentCostUSD,
		id,
		input,
		contextPayload.Content,
		contextPayload.Summary,
		force,
	)
}

func (m model) updateIdleSuggestion(msg idleSuggestionMsg) (tea.Model, tea.Cmd) {
	if next, cmd, stale := m.updateStaleIdleSuggestion(msg); stale {
		return next, cmd
	}

	m.cancelIdleSuggestionRequest()

	if msg.err != nil {
		// Idle suggestions are opportunistic; avoid interrupting the user for
		// provider/network failures.
		slog.Debug("idle prompt suggestion failed", "error", msg.err)

		m.idleSuggestionStatus = idleSuggestionStatusRejectedError
		if errors.Is(msg.err, errIdleSuggestionBudget) {
			m.idleSuggestionStatus = idleSuggestionStatusRejectedBudget
		}

		m.recordIdleSuggestionResult(msg, true, false)

		if msg.force {
			return m, tea.Batch(
				saveSession(m.ctx, m.sessionStore, m.sessionState, m.hookRunner),
				tea.Println(errStyle.Render("Suggestion failed: "+msg.err.Error())),
			)
		}

		return m, saveSession(m.ctx, m.sessionStore, m.sessionState, m.hookRunner)
	}

	if reason := unsafeIdleSuggestionReason(msg.suggestion); reason != "" {
		m.idleSuggestionText = ""
		m.idleSuggestionStatus = "rejected:" + reason
		m.recordIdleSuggestionResult(msg, false, true)

		if msg.force {
			return m, tea.Batch(
				saveSession(m.ctx, m.sessionStore, m.sessionState, m.hookRunner),
				tea.Println(errStyle.Render("Suggestion rejected: "+reason)),
			)
		}

		return m, saveSession(m.ctx, m.sessionStore, m.sessionState, m.hookRunner)
	}

	suggestion := normalizeIdleSuggestion(msg.input, msg.suggestion)
	if suggestion == "" {
		m.idleSuggestionText = ""
		m.idleSuggestionStatus = idleSuggestionStatusRejectedEmpty
		m.recordIdleSuggestionResult(msg, false, true)

		if msg.force {
			return m, tea.Batch(
				saveSession(m.ctx, m.sessionStore, m.sessionState, m.hookRunner),
				tea.Println(errStyle.Render("Suggestion failed: empty response.")),
			)
		}
	} else {
		if msg.force {
			m.textarea.SetValue(msg.input + suggestion)
			m.textarea.CursorEnd()
			m.idleSuggestionStatus = idleSuggestionStatusReadyModel
			m.recordIdleSuggestionResult(msg, false, false)
			m.clearIdleSuggestion()

			return m, saveSession(m.ctx, m.sessionStore, m.sessionState, m.hookRunner)
		}

		m.idleSuggestionText = suggestion
		m.idleSuggestionStatus = idleSuggestionStatusReadyModel
		m.recordIdleSuggestionResult(msg, false, false)
	}

	return m, saveSession(m.ctx, m.sessionStore, m.sessionState, m.hookRunner)
}

func (m model) updateStaleIdleSuggestion(msg idleSuggestionMsg) (model, tea.Cmd, bool) {
	if msg.id != m.idleSuggestionID || msg.input != m.idleSuggestionInput {
		if msg.id == m.idleSuggestionID {
			m.cancelIdleSuggestionRequest()
		}

		if m.recordStaleIdleSuggestionResult(msg) {
			return m, saveSession(m.ctx, m.sessionStore, m.sessionState, m.hookRunner), true
		}

		return m, nil, true
	}

	if msg.input == m.textarea.Value() &&
		textareaCursorOffset(m.textarea) == len(m.textarea.Value()) {
		return m, nil, false
	}

	m.cancelIdleSuggestionRequest()

	m.idleSuggestionStatus = idleSuggestionStatusRejectedStale
	if m.recordStaleIdleSuggestionResult(msg) {
		return m, saveSession(m.ctx, m.sessionStore, m.sessionState, m.hookRunner), true
	}

	return m, nil, true
}

func (m *model) recordIdleSuggestionResult(msg idleSuggestionMsg, failed, rejected bool) {
	m.recordIdleSuggestionResultWithStatus(msg, m.idleSuggestionStatus, failed, rejected)
}

func (m *model) recordStaleIdleSuggestionResult(msg idleSuggestionMsg) bool {
	if !msg.providerCall &&
		msg.usage == (tokenUsage{}) &&
		msg.estimatedInputTokens == 0 &&
		msg.estimatedOutputTokens == 0 &&
		msg.estimatedCostUSD == 0 &&
		msg.err == nil {
		return false
	}

	m.recordIdleSuggestionResultWithStatus(msg, idleSuggestionStatusRejectedStale, msg.err != nil, true)

	return true
}

func (m *model) recordIdleSuggestionResultWithStatus(msg idleSuggestionMsg, status string, failed, rejected bool) {
	m.idleSuggestionUsage.add(msg.usage)

	if msg.providerCall && (msg.estimatedInputTokens > 0 || msg.estimatedOutputTokens > 0) {
		m.idleSuggestionTokens += msg.estimatedInputTokens + msg.estimatedOutputTokens
	}

	if msg.providerCall {
		m.idleSuggestionCostUSD += msg.estimatedCostUSD
	}

	provider := strings.TrimSpace(msg.provider)

	modelName := strings.TrimSpace(msg.model)
	if modelName == "" {
		modelName = strings.TrimSpace(m.idleSuggestionModel)
	}

	if provider == "" {
		provider = strings.TrimSpace(m.idleSuggestionProvider)
	}

	if provider == "" && modelName != "" {
		provider = providerNameForModel(m.registry, modelName)
	}

	record := session.BackgroundSuggestionRecord{
		Provider:              provider,
		Model:                 modelName,
		Status:                status,
		ContextSummary:        msg.contextSummary,
		ProviderCall:          msg.providerCall,
		Response:              msg.providerCall && msg.err == nil,
		Error:                 failed,
		Rejected:              rejected || strings.HasPrefix(status, "rejected:"),
		InputTokens:           msg.usage.InputTokens,
		CachedInputTokens:     msg.usage.CachedInputTokens,
		CacheWriteInputTokens: msg.usage.CacheWriteInputTokens,
		OutputTokens:          msg.usage.OutputTokens,
	}
	if msg.providerCall {
		record.EstimatedCostUSD = msg.estimatedCostUSD
		record.EstimatedInputTokens = msg.estimatedInputTokens
		record.EstimatedOutputTokens = msg.estimatedOutputTokens
	}

	m.sessionState.RecordBackgroundSuggestionUsage(record)
}

func (m model) visibleIdleSuggestion() (string, bool) {
	value := m.textarea.Value()
	if strings.TrimSpace(value) == "" ||
		m.idleSuggestionStatus != idleSuggestionStatusReadyModel ||
		m.idleSuggestionInput != value ||
		m.idleSuggestionText == "" ||
		textareaCursorOffset(m.textarea) != len(value) {
		return "", false
	}

	return m.idleSuggestionText, true
}

func requestIdleSuggestion(
	ctx context.Context,
	reg *llm.Registry,
	modelName string,
	fallbackModels []string,
	defaults generationSettings,
	overrides generationSettings,
	hookRunner *events.Runner,
	maxInputTokens int,
	idleBudget idleSuggestionBudget,
	currentUsage tokenUsage,
	currentEstimatedTokens int,
	currentCostUSD float64,
	id int,
	input string,
	contextSummary string,
	contextStatusSummary string,
	force bool,
) tea.Cmd {
	return func() tea.Msg {
		if ctx == nil {
			return idleSuggestionMsg{id: id, input: input, err: errors.New("idle suggestion: context is required"), contextSummary: contextStatusSummary, force: force}
		}

		reqCtx, cancel := context.WithTimeout(ctx, idleSuggestionTimeout)
		defer cancel()

		generation := mergeGenerationSettings(defaults, overrides)
		idleBudget = normalizeIdleSuggestionBudget(idleBudget)
		effectiveMaxInputTokens := effectiveIdleSuggestionMaxInputTokens(maxInputTokens, idleBudget)
		generation.MaxTokens = idleBudget.MaxOutputTokens

		params := llm.CompleteParams{
			Model: modelName,
			Messages: []llm.Message{
				{
					Role: llm.RoleSystem,
					Content: "You complete an in-progress CLI prompt. " +
						"Return only a short suffix that should be appended to the user's current text. " +
						"Use the supplied local context for relevance and do not invent files, tools, agents, tasks, or issue IDs. " +
						"Do not repeat the current text, do not add explanations, and return an empty response if no useful suffix exists.",
				},
				{
					Role:    llm.RoleUser,
					Content: "Current text:\n" + input + "\n\nLocal context:\n" + contextSummary,
				},
			},
		}
		applyGenerationParams(&params, generation)

		manifestEvent := requestContextManifestEvent(newRequestContextManifestForModels(
			reg,
			params.Model,
			fallbackModels,
			params.Messages,
			effectiveMaxInputTokens,
			contextref.ReferenceManifest{},
		))
		setExplicitContextManifestEventModel(&manifestEvent, params.Model)
		manifestEvent.Metadata["request_kind"] = "background_suggestion"
		manifestEvent.Metadata["background_suggestion"] = "true"
		manifestEvent.Metadata["context_summary"] = contextStatusSummary
		emitHookWarning(reqCtx, hookRunner, manifestEvent)

		estimatedInputTokens, estimatedCostUSD, budgetErr := validateIdleSuggestionRequestBudget(
			reg,
			params.Model,
			fallbackModels,
			params.Messages,
			maxInputTokens,
			idleBudget,
			currentUsage,
			currentEstimatedTokens,
			currentCostUSD,
		)
		if budgetErr != nil {
			return idleSuggestionMsg{
				id:                    id,
				input:                 input,
				err:                   budgetErr,
				contextSummary:        contextStatusSummary,
				estimatedInputTokens:  estimatedInputTokens,
				estimatedOutputTokens: idleBudget.MaxOutputTokens,
				estimatedCostUSD:      estimatedCostUSD,
				force:                 force,
			}
		}

		if err := reqCtx.Err(); err != nil {
			return idleSuggestionMsg{
				id:                    id,
				input:                 input,
				err:                   err,
				provider:              providerNameForModel(reg, params.Model),
				model:                 params.Model,
				contextSummary:        contextStatusSummary,
				estimatedInputTokens:  estimatedInputTokens,
				estimatedOutputTokens: idleBudget.MaxOutputTokens,
				estimatedCostUSD:      estimatedCostUSD,
				force:                 force,
			}
		}

		resp, err := reg.CompleteWithFallback(reqCtx, params, fallbackModels)
		if err != nil {
			return idleSuggestionMsg{
				id:                    id,
				input:                 input,
				err:                   err,
				provider:              providerNameForModel(reg, params.Model),
				model:                 params.Model,
				contextSummary:        contextStatusSummary,
				estimatedInputTokens:  estimatedInputTokens,
				estimatedOutputTokens: idleBudget.MaxOutputTokens,
				estimatedCostUSD:      estimatedCostUSD,
				force:                 force,
				providerCall:          true,
			}
		}

		usage := tokenUsage{}
		usage.addResponse(resp)

		provider := strings.TrimSpace(resp.Provider)
		if provider == "" {
			provider = providerNameForModel(reg, params.Model)
		}

		responseModel := strings.TrimSpace(resp.Model)
		if responseModel == "" {
			responseModel = params.Model
		}

		return idleSuggestionMsg{
			id:                    id,
			input:                 input,
			suggestion:            resp.Content,
			provider:              provider,
			model:                 responseModel,
			contextSummary:        contextStatusSummary,
			usage:                 usage,
			estimatedInputTokens:  estimatedInputTokens,
			estimatedOutputTokens: idleBudget.MaxOutputTokens,
			estimatedCostUSD:      estimatedCostUSD,
			force:                 force,
			providerCall:          true,
		}
	}
}

type idleSuggestionContextPayload struct {
	Content string
	Summary string
	Status  string
}

func (m model) idleSuggestionContextPayload() idleSuggestionContextPayload {
	contextResult := m.promptCompletionContextResult(m.textarea.Value())
	completionContext := contextResult.Context

	var lines []string

	counts := make(map[string]int)

	appendCandidates := func(label string, candidates []promptcomplete.Candidate, limit int) {
		for i, candidate := range candidates {
			if i >= limit {
				break
			}

			lines = append(lines, idleSuggestionContextLine(label, candidate))
			counts[label]++
		}
	}

	appendCandidates("agent", completionContext.Agents, 4)
	appendCandidates("tool", completionContext.Tools, 4)
	appendCandidates("slash", completionContext.SlashCommands, 6)
	appendCandidates("symbol", completionContext.ProjectSymbols, 4)
	appendCandidates("permission", completionContext.Permissions, 4)

	// Files, tasks, and issue references often contain private repository or
	// incident context. Keep model-backed idle suggestions on a minimal
	// provider-visible context unless a future explicit policy permits those
	// categories. Local deterministic completions still use the full context.
	privateSummary := "file/task/issue=omitted-private"

	lines = append(lines, promptContextSourceStatusesForSummary(contextResult.Sources)...)
	contextStatus := promptContextStatusLabel(contextResult.Sources)

	if len(lines) == 0 {
		return idleSuggestionContextPayload{
			Content: "minimal deterministic context available; private file/task/issue context omitted.",
			Summary: privateSummary,
			Status:  contextStatus,
		}
	}

	summary := []string{
		"agent=" + strconv.Itoa(counts["agent"]),
		"tool=" + strconv.Itoa(counts["tool"]),
		"slash=" + strconv.Itoa(counts["slash"]),
		"symbol=" + strconv.Itoa(counts["symbol"]),
		"permission=" + strconv.Itoa(counts["permission"]),
		privateSummary,
	}

	return idleSuggestionContextPayload{
		Content: strings.Join(lines, "\n") + "\nprivacy: private file/task/issue context omitted; candidate descriptions omitted from background suggestions.",
		Summary: strings.Join(summary, ","),
		Status:  contextStatus,
	}
}

func (m model) idleSuggestionContextForPrompt() (summary, status string) {
	payload := m.idleSuggestionContextPayload()

	return payload.Content, payload.Status
}

func idleSuggestionContextLine(label string, candidate promptcomplete.Candidate) string {
	text := privacy.RedactIdentifier(candidate.Text)

	return label + ": " + text
}

func unsafeIdleSuggestionReason(suggestion string) string {
	if strings.ContainsRune(suggestion, '\x00') {
		return "unsafe-control"
	}

	if strings.ContainsAny(suggestion, "\r\n") {
		return "unsafe-multiline"
	}

	normalized := strings.ToLower(strings.Join(strings.Fields(suggestion), " "))
	for _, marker := range []string{"rm -rf /", "curl | sh", "curl | bash", "wget | sh", "wget | bash"} {
		if strings.Contains(normalized, marker) {
			return "unsafe-shell"
		}
	}

	return ""
}

func normalizeIdleSuggestion(input, suggestion string) string {
	suggestion = strings.TrimSpace(suggestion)
	suggestion = strings.Trim(suggestion, "`\"'")

	suggestion = strings.Join(strings.Fields(suggestion), " ")
	if suggestion == "" {
		return ""
	}

	if suffix, ok := strings.CutPrefix(suggestion, input); ok {
		suggestion = suffix
	}

	suggestion = strings.TrimLeft(suggestion, " \t")
	if suggestion == "" {
		return ""
	}

	if needsSuggestionSeparator(input, suggestion) {
		suggestion = " " + suggestion
	}

	const maxSuggestionRunes = 160
	if utf8.RuneCountInString(suggestion) > maxSuggestionRunes {
		runes := []rune(suggestion)
		suggestion = string(runes[:maxSuggestionRunes])
	}

	return suggestion
}

func needsSuggestionSeparator(input, suggestion string) bool {
	if input == "" || suggestion == "" {
		return false
	}

	last, _ := utf8.DecodeLastRuneInString(input)

	first, _ := utf8.DecodeRuneInString(suggestion)
	if last == utf8.RuneError || first == utf8.RuneError {
		return false
	}

	return !isPromptBoundary(last) && !isPromptBoundary(first)
}

func isPromptBoundary(r rune) bool {
	return r == ' ' || r == '\t' || r == '\n' || strings.ContainsRune(".,;:!?)]}/", r)
}

func textareaCursorOffset(input textarea.Model) int {
	value := input.Value()
	lines := strings.Split(value, "\n")

	line := input.Line()
	if line < 0 {
		return 0
	}

	if line >= len(lines) {
		return len(value)
	}

	offset := 0
	for i := range line {
		offset += len(lines[i]) + 1
	}

	info := input.LineInfo()
	column := info.StartColumn + info.ColumnOffset
	column = min(column, len(lines[line]))

	return offset + column
}

func taskDurationSuffix(elapsed time.Duration) string {
	if elapsed <= 0 {
		return ""
	}

	return " after " + formatTaskDuration(elapsed)
}

func formatTaskDuration(elapsed time.Duration) string {
	if elapsed < 0 {
		elapsed = 0
	}

	if elapsed < time.Second {
		return strconv.FormatInt(elapsed.Milliseconds(), 10) + "ms"
	}

	if elapsed < time.Minute {
		elapsed = elapsed.Round(100 * time.Millisecond)

		seconds := elapsed.Seconds()
		if elapsed%time.Second == 0 {
			return strconv.Itoa(int(seconds)) + "s"
		}

		return fmt.Sprintf("%.1fs", seconds)
	}

	return elapsed.Truncate(time.Second).String()
}

// formatTokenCount formats a token count as a compact human-readable string.
// Examples: 0 -> "0", 500 -> "500", 1500 -> "1.5k", 128000 -> "128k",
// 1047576 -> "1.0M".
func formatTokenCount(n int) string {
	switch {
	case n >= 1_000_000:
		f := float64(n) / 1_000_000
		s := strconv.FormatFloat(f, 'f', 1, 64)

		return s + "M"
	case n >= 1_000:
		f := float64(n) / 1_000
		s := strconv.FormatFloat(f, 'f', 1, 64)
		// Drop ".0" for clean whole numbers like "128k" instead of "128.0k".
		s = strings.TrimSuffix(s, ".0")

		return s + "k"
	default:
		return strconv.Itoa(n)
	}
}

func formatTokenUsageSummary(usage tokenUsage) string {
	parts := []string{
		"tokens:",
		"in=" + formatTokenCount(usage.InputTokens),
		"cached=" + formatTokenCount(usage.CachedInputTokens),
		"out=" + formatTokenCount(usage.OutputTokens),
	}
	if usage.CacheWriteInputTokens > 0 {
		parts = append(parts, "cache_write="+formatTokenCount(usage.CacheWriteInputTokens))
	}

	if usage.Responses > 0 {
		parts = append(parts, "responses="+strconv.Itoa(usage.Responses))
	}

	return strings.Join(parts, "\t")
}

func printTokenUsageSummary(w io.Writer, usage tokenUsage) {
	if usage.InputTokens == 0 && usage.CachedInputTokens == 0 && usage.CacheWriteInputTokens == 0 && usage.OutputTokens == 0 {
		return
	}

	fmt.Fprintln(w, formatTokenUsageSummary(usage))
}

// ---------------------------------------------------------------------------
// Helpers
// ---------------------------------------------------------------------------

// loadModels starts one background fetch per provider so the picker can update
// incrementally as live model catalogs return.
