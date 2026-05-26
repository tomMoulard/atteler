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
	"github.com/tommoulard/atteler/pkg/promptcomplete"
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

	if ctx := m.contextUsage(); ctx != "" {
		parts = append(parts, ctx)
	}

	if m.idleSuggestionStatus != "" {
		parts = append(parts, "suggestion:"+m.idleSuggestionStatus)
	}

	if len(parts) == 0 {
		return ""
	}

	return dimStyle.Render("  [") + pickerSelectedStyle.Render(strings.Join(parts, " ")) + dimStyle.Render("]")
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

	suggestion, ok := promptcomplete.Suggest(promptCompletionContext(m.ctx, appState{
		agentRegistry: m.agentRegistry,
		sessionStore:  m.sessionStore,
		sessionState:  m.sessionState,
		selectedAgent: m.selectedAgent,
		cwd:           m.cwd,
	}, value, false), promptcomplete.Options{Limit: 1})
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

func (m *model) clearIdleSuggestion() {
	m.cancelIdleSuggestionRequest()
	m.idleSuggestionID++
	m.idleSuggestionInput = ""
	m.idleSuggestionText = ""
	m.idleSuggestionStatus = ""
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
		m.registry == nil ||
		m.promptLocalOnly ||
		strings.TrimSpace(value) == "" ||
		textareaCursorOffset(m.textarea) != len(value) {
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

	if m.registry == nil {
		m.idleSuggestionStatus = idleSuggestionStatusRejectedError

		return m, tea.Println(errStyle.Render("Suggestion unavailable: no model registry is configured.")), true
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
		m.registry == nil {
		m.idleSuggestionStatus = idleSuggestionStatusRejectedStale
		return m, nil
	}

	return m.startIdleSuggestionRequest(msg.id, msg.input, false)
}

func (m model) startIdleSuggestionRequest(id int, input string, force bool) (tea.Model, tea.Cmd) {
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

	if force {
		m.idleSuggestionStatus = idleSuggestionStatusPendingForced
	} else {
		m.idleSuggestionStatus = idleSuggestionStatusPending
	}

	return m, requestIdleSuggestion(
		requestCtx,
		m.registry,
		m.selectedModel,
		m.fallbackModels,
		m.generationDefaults,
		m.generationOverrides,
		m.hookRunner,
		m.maxInputTokens,
		id,
		input,
		m.idleSuggestionContext(),
		force,
	)
}

func (m model) updateIdleSuggestion(msg idleSuggestionMsg) (tea.Model, tea.Cmd) {
	if msg.id != m.idleSuggestionID ||
		msg.input != m.idleSuggestionInput {
		return m, nil
	}

	if msg.input != m.textarea.Value() ||
		textareaCursorOffset(m.textarea) != len(m.textarea.Value()) {
		m.idleSuggestionStatus = idleSuggestionStatusRejectedStale
		return m, nil
	}

	m.cancelIdleSuggestionRequest()

	if msg.err != nil {
		// Idle suggestions are opportunistic; avoid interrupting the user for
		// provider/network failures.
		slog.Debug("idle prompt suggestion failed", "error", msg.err)

		m.idleSuggestionStatus = idleSuggestionStatusRejectedError

		if msg.force {
			return m, tea.Println(errStyle.Render("Suggestion failed: " + msg.err.Error()))
		}

		return m, nil
	}

	if reason := unsafeIdleSuggestionReason(msg.suggestion); reason != "" {
		m.idleSuggestionText = ""
		m.idleSuggestionStatus = "rejected:" + reason

		if msg.force {
			return m, tea.Println(errStyle.Render("Suggestion rejected: " + reason))
		}

		return m, nil
	}

	suggestion := normalizeIdleSuggestion(msg.input, msg.suggestion)
	if suggestion == "" {
		m.idleSuggestionText = ""
		m.idleSuggestionStatus = idleSuggestionStatusRejectedEmpty

		if msg.force {
			return m, tea.Println(errStyle.Render("Suggestion failed: empty response."))
		}
	} else {
		if msg.force {
			m.textarea.SetValue(msg.input + suggestion)
			m.textarea.CursorEnd()
			m.clearIdleSuggestion()

			return m, nil
		}

		m.idleSuggestionText = suggestion
		m.idleSuggestionStatus = idleSuggestionStatusReadyModel
	}

	return m, nil
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
	id int,
	input string,
	contextSummary string,
	force bool,
) tea.Cmd {
	return func() tea.Msg {
		if ctx == nil {
			return idleSuggestionMsg{id: id, input: input, err: errors.New("idle suggestion: context is required"), force: force}
		}

		reqCtx, cancel := context.WithTimeout(ctx, idleSuggestionTimeout)
		defer cancel()

		generation := mergeGenerationSettings(defaults, overrides)
		generation.MaxTokens = 32

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
			maxInputTokens,
			contextref.ReferenceManifest{},
		))
		setExplicitContextManifestEventModel(&manifestEvent, params.Model)
		emitHookWarning(reqCtx, hookRunner, manifestEvent)

		if err := validateRequestBudgetWithFallbacks(reg, params.Model, fallbackModels, params.Messages, maxInputTokens); err != nil {
			return idleSuggestionMsg{id: id, input: input, err: err, force: force}
		}

		resp, err := reg.CompleteWithFallback(reqCtx, params, fallbackModels)
		if err != nil {
			return idleSuggestionMsg{id: id, input: input, err: err, force: force}
		}

		return idleSuggestionMsg{id: id, input: input, suggestion: resp.Content, force: force}
	}
}

func (m model) idleSuggestionContext() string {
	completionContext := promptCompletionContext(m.ctx, appState{
		agentRegistry: m.agentRegistry,
		sessionStore:  m.sessionStore,
		sessionState:  m.sessionState,
		selectedAgent: m.selectedAgent,
		cwd:           m.cwd,
	}, m.textarea.Value(), false)

	var lines []string

	appendCandidates := func(label string, candidates []promptcomplete.Candidate, limit int) {
		for i, candidate := range candidates {
			if i >= limit {
				break
			}

			lines = append(lines, label+": "+candidate.Text+" — "+candidate.Description)
		}
	}

	appendCandidates("agent", completionContext.Agents, 4)
	appendCandidates("tool", completionContext.Tools, 4)
	appendCandidates("slash", completionContext.SlashCommands, 6)
	appendCandidates("file", completionContext.RecentFiles, 4)
	appendCandidates("task", completionContext.Tasks, 4)
	appendCandidates("issue", completionContext.Issues, 4)
	appendCandidates("permission", completionContext.Permissions, 4)

	if len(lines) == 0 {
		return "local-only deterministic context available; no live context candidates matched."
	}

	return strings.Join(lines, "\n")
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
