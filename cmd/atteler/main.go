// Package main is the entry point for the atteler TUI application.
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/charmbracelet/bubbles/key"
	"github.com/charmbracelet/bubbles/textarea"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"gopkg.in/yaml.v3"

	"github.com/tommoulard/atteler/pkg/agent"
	appconfig "github.com/tommoulard/atteler/pkg/config"
	"github.com/tommoulard/atteler/pkg/contextref"
	"github.com/tommoulard/atteler/pkg/events"
	"github.com/tommoulard/atteler/pkg/llm"
	"github.com/tommoulard/atteler/pkg/session"
	"github.com/tommoulard/atteler/pkg/worktree"
)

// ---------------------------------------------------------------------------
// Styles
// ---------------------------------------------------------------------------

var (
	version = "dev"
	commit  = "none"
	date    = "unknown"
)

var (
	promptStyle = lipgloss.NewStyle().
			Foreground(lipgloss.Color("170")).
			Bold(true)

	statusStyle = lipgloss.NewStyle().
			Foreground(lipgloss.Color("241"))

	errStyle = lipgloss.NewStyle().
			Foreground(lipgloss.Color("196"))

	assistantLabel = lipgloss.NewStyle().
			Foreground(lipgloss.Color("39")).
			Bold(true)

	userLabel = lipgloss.NewStyle().
			Foreground(lipgloss.Color("178")).
			Bold(true)

	dimStyle = lipgloss.NewStyle().
			Foreground(lipgloss.Color("241"))

	pickerHeaderStyle = lipgloss.NewStyle().
				Foreground(lipgloss.Color("39")).
				Bold(true)

	pickerSelectedStyle = lipgloss.NewStyle().
				Foreground(lipgloss.Color("170")).
				Bold(true)

	pickerNormalStyle = lipgloss.NewStyle().
				Foreground(lipgloss.Color("252"))

	pickerProviderStyle = lipgloss.NewStyle().
				Foreground(lipgloss.Color("39")).
				Bold(true)
)

// Key binding constants.
const (
	keyCtrlC = "ctrl+c"
	keyEnter = "enter"
	keyEsc   = "esc"
)

// ---------------------------------------------------------------------------
// Messages (tea.Msg)
// ---------------------------------------------------------------------------

// llmResponseMsg is sent when the LLM call completes.
type llmResponseMsg struct {
	err     error
	content string
	model   string
}

type llmRequest struct {
	generation     generationSettings
	model          string
	messages       []llm.Message
	fallbackModels []string
	refs           []contextref.Reference
	agent          agent.Agent
	hasAgent       bool
}

// modelsLoadedMsg is sent when model discovery from the API completes.
type modelsLoadedMsg struct {
	err   error
	items []pickerItem
}

// fzfModelSelectedMsg is sent after the external fzf model picker exits.
type fzfModelSelectedMsg struct {
	err      error
	item     pickerItem
	selected bool
}

type modelPreferenceSavedMsg struct {
	err   error
	scope appconfig.ModelScope
}

// sessionSavedMsg is sent when a session save attempt completes.
type sessionSavedMsg struct {
	err error
}

type hookMsg struct {
	err  error
	line string // non-empty when the event should be printed by the TUI
}

// pickerItem represents one selectable entry in the model picker.
type pickerItem struct {
	provider string
	model    string
}

func (p pickerItem) label() string {
	if p.provider == "" {
		return p.model
	}
	return p.provider + "/" + p.model
}

// ---------------------------------------------------------------------------
// Model
// ---------------------------------------------------------------------------

//nolint:govet // Field order groups related TUI state instead of optimizing padding.
type model struct {
	textarea            textarea.Model
	registry            *llm.Registry
	agentRegistry       *agent.Registry
	hookRunner          *events.Runner
	sessionStore        *session.Store
	stateStore          *appconfig.StateStore
	cancel              context.CancelFunc
	pendingModel        pickerItem
	selectedModel       string
	selectedAgent       string
	sessionPath         string
	cwd                 string
	selectedProvider    string
	fallbackModels      []string
	generationDefaults  generationSettings
	generationOverrides generationSettings
	sessionState        session.Session
	history             []llm.Message
	pickerItems         []pickerItem
	contextOptions      contextref.Options
	worktreeInfo        *worktree.Info
	pickerCursor        int
	width               int
	quitting            bool
	waiting             bool
	pickerOpen          bool
	pickerLoading       bool
	scopePickerOpen     bool
	modelLocked         bool
}

func initialModel(
	reg *llm.Registry,
	agents *agent.Registry,
	hooks *events.Runner,
	store *session.Store,
	stateStore *appconfig.StateStore,
	sessionState session.Session,
	contextOptions contextref.Options,
	sessionPath string,
	cwd string,
	selectedModel string,
	selectedAgent string,
	fallbackModels []string,
	generationDefaults generationSettings,
	generationOverrides generationSettings,
	modelLocked bool,
	wtInfo *worktree.Info,
) model {
	ta := textarea.New()
	ta.Placeholder = "Send a message (Alt+Enter to send, Ctrl+O to pick model)"
	ta.Focus()
	ta.CharLimit = 0 // unlimited
	ta.ShowLineNumbers = false
	ta.SetHeight(3)

	// Remap newline insertion to Alt+Enter so plain Enter submits.
	// Bubbletea v1 cannot distinguish Shift+Enter from Enter (terminals emit
	// the same \r byte for both), so Alt+Enter is the only reliable modifier.
	ta.KeyMap.InsertNewline = key.NewBinding(key.WithKeys("alt+enter"))
	selectedProvider, _ := reg.ProviderForModel(selectedModel)
	return model{
		registry:            reg,
		agentRegistry:       agents,
		hookRunner:          hooks,
		sessionStore:        store,
		stateStore:          stateStore,
		sessionState:        sessionState,
		contextOptions:      contextOptions,
		sessionPath:         sessionPath,
		cwd:                 cwd,
		selectedModel:       selectedModel,
		selectedAgent:       selectedAgent,
		selectedProvider:    selectedProvider,
		fallbackModels:      append([]string(nil), fallbackModels...),
		generationDefaults:  generationDefaults,
		generationOverrides: generationOverrides,
		history:             append([]llm.Message(nil), sessionState.Messages...),
		textarea:            ta,
		modelLocked:         modelLocked,
		worktreeInfo:        wtInfo,
	}
}

// Init returns the initial command.
func (m model) Init() tea.Cmd {
	return textarea.Blink
}

// Update handles incoming messages.
func (m model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.width = msg.Width
		m.textarea.SetWidth(msg.Width)
		return m.updateTextarea(msg)

	case modelsLoadedMsg:
		return m.updateModelsLoaded(msg)

	case fzfModelSelectedMsg:
		return m.updateFZFModelSelected(msg)

	case modelPreferenceSavedMsg:
		return m.updateModelPreferenceSaved(msg)

	case tea.KeyMsg:
		if m.scopePickerOpen {
			return m.updateModelScopePicker(msg)
		}
		if m.pickerOpen {
			return m.updatePicker(msg)
		}
		return m.updateChat(msg)

	case llmResponseMsg:
		return m.updateLLMResponse(msg)

	case sessionSavedMsg:
		return m.updateSessionSaved(msg)

	case hookMsg:
		return m.updateHook(msg)
	}

	return m.updateTextarea(msg)
}

func (m model) updateModelsLoaded(msg modelsLoadedMsg) (tea.Model, tea.Cmd) {
	m.pickerLoading = false
	if msg.err != nil {
		m.pickerOpen = false
		return m, tea.Println(errStyle.Render("Error loading models: " + msg.err.Error()))
	}
	if fzfPath, ok := findFZF(); ok {
		m.pickerOpen = false
		return m, tea.Batch(
			emitHook(m.hookRunner, events.Event{
				Type:        events.CommandExecute,
				SessionID:   m.sessionState.ID,
				SessionPath: m.sessionPath,
				Agent:       m.selectedAgent,
				Model:       m.selectedModel,
				Metadata: map[string]string{
					"command": "fzf",
				},
			}),
			runFZFModelPicker(fzfPath, msg.items),
		)
	}
	m.pickerItems = msg.items
	m.pickerCursor = 0
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

func (m model) updateSessionSaved(msg sessionSavedMsg) (tea.Model, tea.Cmd) {
	if msg.err != nil {
		return m, tea.Println(errStyle.Render("Warning: " + msg.err.Error()))
	}
	return m, nil
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

func (m model) updateTextarea(msg tea.Msg) (tea.Model, tea.Cmd) {
	var cmds []tea.Cmd
	if !m.waiting && !m.pickerOpen && !m.scopePickerOpen {
		var taCmd tea.Cmd
		m.textarea, taCmd = m.textarea.Update(msg)
		cmds = append(cmds, taCmd)
	}

	return m, tea.Batch(cmds...)
}

// updateChat handles key events in normal chat mode.
func (m model) updateChat(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	var cmds []tea.Cmd

	switch msg.String() {
	case "ctrl+d":
		if !m.waiting {
			m.quitting = true
			return m, tea.Quit
		}
	case keyCtrlC:
		if m.waiting {
			if m.cancel != nil {
				m.cancel()
				m.cancel = nil
			}
			m.waiting = false
			cmds = append(cmds, tea.Println(errStyle.Render("(canceled)")))
			return m, tea.Batch(cmds...)
		}
		m.quitting = true
		return m, tea.Quit

	case "ctrl+o":
		if m.waiting {
			break
		}
		m.pickerOpen = true
		m.pickerLoading = true
		m.pickerItems = nil
		m.pickerCursor = 0
		return m, loadModels(m.registry)

	case keyEnter:
		return m.submitInput()
	}

	// Propagate to the textarea when not waiting.
	if !m.waiting {
		var taCmd tea.Cmd
		m.textarea, taCmd = m.textarea.Update(msg)
		cmds = append(cmds, taCmd)
	}

	return m, tea.Batch(cmds...)
}

// submitInput handles the enter key — sends user input to the LLM.
func (m model) submitInput() (tea.Model, tea.Cmd) {
	if m.waiting {
		return m, nil
	}
	input := strings.TrimSpace(m.textarea.Value())
	if input == "" {
		return m, nil
	}
	m.textarea.Reset()

	activeAgent, prompt, err := m.resolveAgent(input)
	if err != nil {
		return m, tea.Println(errStyle.Render("Error: " + err.Error()))
	}
	input = prompt

	nextHistory := append(append([]llm.Message(nil), m.history...), llm.Message{
		Role:    llm.RoleUser,
		Content: input,
	})
	requestMessages, refs, err := expandReferences(nextHistory, m.contextOptions)
	if err != nil {
		return m, tea.Println(errStyle.Render("Error: " + err.Error()))
	}

	m.history = nextHistory
	m.sessionState.Messages = append([]llm.Message(nil), m.history...)
	m.sessionState.DefaultAgent = activeAgent.name
	if m.selectedModel != "" {
		m.sessionState.DefaultModel = m.selectedModel
	}

	// Print the user message above the input area.
	line := userLabel.Render("You") + " " + input
	if activeAgent.name != "" {
		line = userLabel.Render("You") + dimStyle.Render(" (@"+activeAgent.name+") ") + input
	}

	// Launch the LLM call.
	m.waiting = true
	ctx, cancel := context.WithCancel(context.Background()) //nolint:gosec // cancel is stored in m.cancel and invoked on ctrl+c
	m.cancel = cancel
	msgs := make([]llm.Message, len(m.history))
	copy(msgs, requestMessages)
	requestModel, fallbackModels := requestModelAndFallbacks(m.selectedModel, m.modelLocked, m.fallbackModels, activeAgent)
	generation := generationForRequest(m.generationDefaults, m.generationOverrides, activeAgent)
	request := llmRequest{
		agent:          activeAgent.agent,
		hasAgent:       activeAgent.ok,
		model:          requestModel,
		messages:       msgs,
		fallbackModels: fallbackModels,
		generation:     generation,
		refs:           refs,
	}

	cmds := []tea.Cmd{
		tea.Println(line),
	}
	if len(refs) > 0 {
		cmds = append(cmds, tea.Println(dimStyle.Render("Context: "+referenceSummary(refs))))
	}
	for _, ref := range refs {
		cmds = append(
			cmds,
			emitFileRead(m.hookRunner, m.sessionState.ID, m.sessionPath, activeAgent.name, m.sessionState.DefaultModel, ref),
			emitContextAdd(m.hookRunner, m.sessionState.ID, m.sessionPath, activeAgent.name, m.sessionState.DefaultModel, ref),
		)
	}
	if activeAgent.ok {
		cmds = append(cmds, emitAgentExecute(m.hookRunner, m.sessionState.ID, m.sessionPath, activeAgent.name, requestModel))
	}
	cmds = append(cmds,
		saveSession(m.sessionStore, m.sessionState, m.hookRunner),
		emitHook(m.hookRunner, events.Event{
			Type:        events.UserMessage,
			SessionID:   m.sessionState.ID,
			SessionPath: m.sessionPath,
			Agent:       activeAgent.name,
			Model:       m.sessionState.DefaultModel,
			Role:        string(llm.RoleUser),
			Content:     input,
			Metadata:    referenceMetadata(refs),
		}),
		callLLM(m.eventContext(ctx, activeAgent.name, requestModel), m.registry, request),
	)
	return m, tea.Sequence(cmds...)
}

type agentSelection struct {
	name  string
	agent agent.Agent
	ok    bool
}

func (m model) resolveAgent(input string) (agentSelection, string, error) {
	return resolveAgent(m.agentRegistry, m.selectedAgent, input)
}

func (m model) eventContext(ctx context.Context, agentName, modelName string) context.Context {
	return events.WithEmitter(ctx, m.hookRunner, events.Event{
		SessionID:   m.sessionState.ID,
		SessionPath: m.sessionPath,
		Agent:       agentName,
		Model:       modelName,
	})
}

// updateLLMResponse handles the message received when an LLM call completes.
func (m model) updateLLMResponse(msg llmResponseMsg) (tea.Model, tea.Cmd) {
	m.waiting = false
	m.cancel = nil
	if msg.err != nil {
		return m, tea.Batch(
			tea.Println(errStyle.Render("Error: "+msg.err.Error())),
			emitHook(m.hookRunner, events.Event{
				Type:        events.Error,
				SessionID:   m.sessionState.ID,
				SessionPath: m.sessionPath,
				Agent:       m.sessionState.DefaultAgent,
				Model:       m.sessionState.DefaultModel,
				Error:       msg.err.Error(),
			}),
		)
	}

	m.history = append(m.history, llm.Message{
		Role:    llm.RoleAssistant,
		Content: msg.content,
	})
	m.sessionState.Messages = append([]llm.Message(nil), m.history...)
	if msg.model != "" {
		m.sessionState.DefaultModel = msg.model
		if m.modelLocked && m.selectedModel != "" {
			m.sessionState.DefaultModel = m.selectedModel
		}
	}
	header := assistantLabel.Render("Assistant") + " " +
		dimStyle.Render("("+msg.model+")")
	return m, tea.Batch(
		tea.Println(header+"\n"+msg.content),
		saveSession(m.sessionStore, m.sessionState, m.hookRunner),
		emitHook(m.hookRunner, events.Event{
			Type:        events.AssistantMessage,
			SessionID:   m.sessionState.ID,
			SessionPath: m.sessionPath,
			Agent:       m.sessionState.DefaultAgent,
			Model:       msg.model,
			Role:        string(llm.RoleAssistant),
			Content:     msg.content,
		}),
	)
}

// updatePicker handles key events while the model picker is open.
func (m model) updatePicker(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	if m.pickerLoading {
		// Only allow escape while loading.
		if msg.String() == keyEsc || msg.String() == keyCtrlC {
			m.pickerOpen = false
			m.pickerLoading = false
		}
		return m, nil
	}

	switch msg.String() {
	case keyEsc, keyCtrlC, "ctrl+o":
		m.pickerOpen = false
		return m, nil

	case "up", "k":
		if m.pickerCursor > 0 {
			m.pickerCursor--
		}

	case "down", "j":
		if m.pickerCursor < len(m.pickerItems)-1 {
			m.pickerCursor++
		}

	case keyEnter:
		if len(m.pickerItems) > 0 {
			item := m.pickerItems[m.pickerCursor]
			m.pickerOpen = false
			return m.openModelScopePicker(item)
		}
	}

	return m, nil
}

func (m model) openModelScopePicker(item pickerItem) (tea.Model, tea.Cmd) {
	m.pendingModel = item
	m.scopePickerOpen = true
	return m, nil
}

func (m model) updateModelScopePicker(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	switch msg.String() {
	case keyEsc, keyCtrlC:
		m.scopePickerOpen = false
		m.pendingModel = pickerItem{}
		return m, nil
	case "1", "s", keyEnter:
		return m.selectModel(m.pendingModel, appconfig.ModelScopeSession)
	case "2", "f":
		return m.selectModel(m.pendingModel, appconfig.ModelScopeFolder)
	case "3", "g":
		return m.selectModel(m.pendingModel, appconfig.ModelScopeGlobal)
	}
	return m, nil
}

func (m model) selectModel(item pickerItem, scope appconfig.ModelScope) (tea.Model, tea.Cmd) {
	m.scopePickerOpen = false
	m.pendingModel = pickerItem{}
	m.selectedProvider = item.provider
	m.selectedModel = item.label()
	m.fallbackModels = nil
	m.modelLocked = true
	m.sessionState.DefaultModel = m.selectedModel
	cmds := []tea.Cmd{tea.Println(
		dimStyle.Render("Model set to ") +
			pickerProviderStyle.Render(item.provider) +
			dimStyle.Render("/") +
			pickerSelectedStyle.Render(item.model) +
			dimStyle.Render(" ("+modelScopeLabel(scope)+")"),
	)}
	if scope != appconfig.ModelScopeSession {
		cmds = append(cmds, saveModelPreference(m.stateStore, m.cwd, m.selectedModel, scope, m.hookRunner))
	}
	return m, tea.Batch(cmds...)
}

func modelScopeLabel(scope appconfig.ModelScope) string {
	switch scope {
	case appconfig.ModelScopeFolder:
		return "folder default"
	case appconfig.ModelScopeGlobal:
		return "global default"
	default:
		return "session only"
	}
}

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

	if m.waiting {
		return statusStyle.Render("  Thinking... (Ctrl+C to cancel)")
	}

	var status string
	var parts []string
	if m.selectedAgent != "" {
		parts = append(parts, "agent:"+m.selectedAgent)
	}
	if m.selectedModel != "" {
		label := m.selectedModel
		if m.selectedProvider != "" && !strings.Contains(label, "/") {
			label = m.selectedProvider + "/" + label
		}
		parts = append(parts, "model:"+label)
	}
	if ctx := m.contextUsage(); ctx != "" {
		parts = append(parts, ctx)
	}
	if len(parts) > 0 {
		status = dimStyle.Render("  [") + pickerSelectedStyle.Render(strings.Join(parts, " ")) + dimStyle.Render("]")
	}

	return status + "\n" + m.textarea.View()
}

// viewPicker renders the model selection overlay.
func (m model) viewPicker() string {
	var b strings.Builder

	b.WriteString(pickerHeaderStyle.Render("Select a model") +
		dimStyle.Render("  (j/k to move, Enter to select, Esc to cancel)") + "\n\n")

	if m.pickerLoading {
		b.WriteString(statusStyle.Render("  Loading models from API..."))
		return b.String()
	}

	if len(m.pickerItems) == 0 {
		b.WriteString(errStyle.Render("  No models available. Check your API keys."))
		return b.String()
	}

	currentProvider := ""
	for i, item := range m.pickerItems {
		// Print provider header when it changes.
		if item.provider != currentProvider {
			if currentProvider != "" {
				b.WriteString("\n")
			}
			currentProvider = item.provider
			b.WriteString("  " + pickerProviderStyle.Render(item.provider) + "\n")
		}

		cursor := "    "
		style := pickerNormalStyle
		if i == m.pickerCursor {
			cursor = "  > "
			style = pickerSelectedStyle
		}
		b.WriteString(cursor + style.Render(item.model) + "\n")
	}

	return b.String()
}

func (m model) viewModelScopePicker() string {
	var b strings.Builder
	b.WriteString(pickerHeaderStyle.Render("Keep selected model?") + "\n\n")
	b.WriteString("  " + pickerSelectedStyle.Render(m.pendingModel.label()) + "\n\n")
	b.WriteString(pickerNormalStyle.Render("  1 / Enter  Session only") + "\n")
	b.WriteString(pickerNormalStyle.Render("  2 / f      This folder") + "\n")
	b.WriteString(pickerNormalStyle.Render("  3 / g      Globally") + "\n\n")
	b.WriteString(dimStyle.Render("  Esc cancels model selection"))
	return b.String()
}

// contextUsage returns a compact "ctx:~1.2k/200k" string showing the
// estimated token usage relative to the model's context window. Returns ""
// when the model is unknown or has no context window metadata.
func (m model) contextUsage() string {
	if m.selectedModel == "" {
		return ""
	}
	limit := m.registry.ContextWindow(m.selectedModel)
	used := llm.EstimateTokens(m.history)
	if limit > 0 {
		return "ctx:" + formatTokenCount(used) + "/" + formatTokenCount(limit)
	}
	if used > 0 {
		return "ctx:~" + formatTokenCount(used)
	}
	return ""
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

// ---------------------------------------------------------------------------
// Helpers
// ---------------------------------------------------------------------------

// loadModels fetches the model list from all registered providers.
func loadModels(reg *llm.Registry) tea.Cmd {
	return func() tea.Msg {
		ctx := context.Background()
		providers := reg.ListProviders()
		sort.Strings(providers)

		var items []pickerItem
		for _, pName := range providers {
			models, err := reg.ProviderModels(ctx, pName)
			if err != nil {
				continue
			}
			sort.Strings(models)
			for _, m := range models {
				items = append(items, pickerItem{provider: pName, model: m})
			}
		}

		if len(items) == 0 {
			return modelsLoadedMsg{err: errors.New("no models available from any provider")}
		}
		return modelsLoadedMsg{items: items}
	}
}

var execLookPath = exec.LookPath

func findFZF() (string, bool) {
	path, err := execLookPath("fzf")
	if err != nil {
		return "", false
	}
	return path, true
}

func runFZFModelPicker(fzfPath string, items []pickerItem) tea.Cmd {
	var stdout bytes.Buffer
	input := fzfInput(items)
	cmd := exec.CommandContext(
		context.Background(),
		fzfPath,
		"--prompt", "atteler model> ",
		"--height", "80%",
		"--border",
		"--delimiter", "\t",
		"--with-nth", "1",
	)
	cmd.Stdin = strings.NewReader(input)
	cmd.Stdout = &stdout

	return tea.ExecProcess(cmd, func(err error) tea.Msg {
		if item, ok := parseFZFSelection(stdout.String(), items); ok {
			return fzfModelSelectedMsg{item: item, selected: true}
		}
		if err != nil {
			return fzfModelSelectedMsg{}
		}
		return fzfModelSelectedMsg{}
	})
}

func fzfInput(items []pickerItem) string {
	var b strings.Builder
	for _, item := range items {
		b.WriteString(item.label())
		b.WriteString("\t")
		b.WriteString(item.provider)
		b.WriteString("\t")
		b.WriteString(item.model)
		b.WriteString("\n")
	}
	return b.String()
}

func parseFZFSelection(selection string, items []pickerItem) (pickerItem, bool) {
	selection = strings.TrimSpace(selection)
	if selection == "" {
		return pickerItem{}, false
	}
	label, _, _ := strings.Cut(selection, "\t")
	for _, item := range items {
		if item.label() == label {
			return item, true
		}
	}
	return pickerItem{}, false
}

// callLLM sends the messages to the selected LLM and returns a command that
// resolves with an llmResponseMsg. If no model is selected it uses the
// registry default.
func callLLM(ctx context.Context, reg *llm.Registry, request llmRequest) tea.Cmd {
	return func() tea.Msg {
		params := llm.CompleteParams{
			Model:    request.model,
			Messages: request.messages,
		}
		if request.hasAgent {
			params = request.agent.CompleteParams(request.model, request.messages)
		}
		applyGenerationParams(&params, request.generation)

		resp, err := reg.CompleteWithFallback(ctx, params, request.fallbackModels)
		if err != nil {
			return llmResponseMsg{err: err}
		}
		return llmResponseMsg{
			content: resp.Content,
			model:   resp.Model,
		}
	}
}

func expandReferences(messages []llm.Message, opts contextref.Options) ([]llm.Message, []contextref.Reference, error) {
	if len(messages) == 0 {
		return nil, nil, nil
	}

	out := append([]llm.Message(nil), messages...)
	for i := len(out) - 1; i >= 0; i-- {
		if out[i].Role != llm.RoleUser {
			continue
		}

		result, err := contextref.Expand(out[i].Content, opts)
		if err != nil {
			return nil, nil, fmt.Errorf("expand context references: %w", err)
		}
		out[i].Content = result.Prompt
		return out, result.References, nil
	}

	return out, nil, nil
}

func referenceSummary(refs []contextref.Reference) string {
	parts := make([]string, 0, len(refs))
	for _, ref := range refs {
		path := ref.Path
		if ref.Kind != "" && ref.Kind != "file" {
			path = ref.Kind + ":" + path
		}
		label := fmt.Sprintf("%s (%d bytes", path, ref.Bytes)
		if ref.Truncated {
			label += ", truncated"
		}
		label += ")"
		parts = append(parts, label)
	}
	return strings.Join(parts, ", ")
}

func referenceMetadata(refs []contextref.Reference) map[string]string {
	if len(refs) == 0 {
		return nil
	}

	return map[string]string{
		"context_references": referenceSummary(refs),
	}
}

func saveSession(store *session.Store, sessionState session.Session, runner *events.Runner) tea.Cmd {
	return func() tea.Msg {
		if store == nil || sessionState.ID == "" {
			return sessionSavedMsg{}
		}
		if err := store.Save(sessionState); err != nil {
			return sessionSavedMsg{err: err}
		}
		emitHookWarning(context.Background(), runner, events.Event{
			Type:        events.FileWrite,
			SessionID:   sessionState.ID,
			SessionPath: store.Path(sessionState.ID),
			Agent:       sessionState.DefaultAgent,
			Model:       sessionState.DefaultModel,
			Metadata: map[string]string{
				"path": store.Path(sessionState.ID),
				"kind": "session",
			},
		})
		return sessionSavedMsg{}
	}
}

func saveModelPreference(
	store *appconfig.StateStore,
	cwd string,
	model string,
	scope appconfig.ModelScope,
	runner *events.Runner,
) tea.Cmd {
	return func() tea.Msg {
		if store == nil {
			return modelPreferenceSavedMsg{scope: scope}
		}
		state, err := store.Load()
		if err != nil {
			return modelPreferenceSavedMsg{err: err, scope: scope}
		}
		state.SetModel(scope, cwd, model)
		if err := store.Save(state); err != nil {
			return modelPreferenceSavedMsg{err: err, scope: scope}
		}
		emitHookWarning(context.Background(), runner, events.Event{
			Type: events.FileWrite,
			Metadata: map[string]string{
				"path": store.Path(),
				"kind": "state",
			},
		})
		return modelPreferenceSavedMsg{scope: scope}
	}
}

func emitFileRead(
	runner *events.Runner,
	sessionID, sessionPath, agentName, modelName string,
	ref contextref.Reference,
) tea.Cmd {
	return emitHook(runner, fileReadEvent(sessionID, sessionPath, agentName, modelName, ref))
}

func fileReadEvent(
	sessionID, sessionPath, agentName, modelName string,
	ref contextref.Reference,
) events.Event {
	return events.Event{
		Type:        events.FileRead,
		SessionID:   sessionID,
		SessionPath: sessionPath,
		Agent:       agentName,
		Model:       modelName,
		Metadata: map[string]string{
			"path":      ref.Path,
			"kind":      ref.Kind,
			"bytes":     strconv.Itoa(ref.Bytes),
			"truncated": strconv.FormatBool(ref.Truncated),
		},
	}
}

func emitContextAdd(
	runner *events.Runner,
	sessionID, sessionPath, agentName, modelName string,
	ref contextref.Reference,
) tea.Cmd {
	return emitHook(runner, contextAddEvent(sessionID, sessionPath, agentName, modelName, ref))
}

func contextAddEvent(
	sessionID, sessionPath, agentName, modelName string,
	ref contextref.Reference,
) events.Event {
	return events.Event{
		Type:        events.ContextAdd,
		SessionID:   sessionID,
		SessionPath: sessionPath,
		Agent:       agentName,
		Model:       modelName,
		Metadata: map[string]string{
			"path":      ref.Path,
			"kind":      ref.Kind,
			"bytes":     strconv.Itoa(ref.Bytes),
			"truncated": strconv.FormatBool(ref.Truncated),
		},
	}
}

func emitFileWriteWarning(
	ctx context.Context,
	runner *events.Runner,
	sessionState session.Session,
	path string,
	agentName string,
	kind string,
) {
	emitHookWarning(ctx, runner, events.Event{
		Type:        events.FileWrite,
		SessionID:   sessionState.ID,
		SessionPath: path,
		Agent:       agentName,
		Model:       sessionState.DefaultModel,
		Metadata: map[string]string{
			"path": path,
			"kind": kind,
		},
	})
}

func emitAgentExecute(runner *events.Runner, sessionID, sessionPath, agentName, modelName string) tea.Cmd {
	return emitHook(runner, events.Event{
		Type:        events.AgentExecute,
		SessionID:   sessionID,
		SessionPath: sessionPath,
		Agent:       agentName,
		Model:       modelName,
		Metadata: map[string]string{
			"agent": agentName,
		},
	})
}

func emitHook(runner *events.Runner, event events.Event) tea.Cmd {
	return func() tea.Msg {
		if runner == nil {
			return hookMsg{}
		}
		line := events.FormatLine(event)
		return hookMsg{err: runner.Emit(context.Background(), event), line: line}
	}
}

func emitHookWarning(ctx context.Context, runner *events.Runner, event events.Event) {
	if runner == nil {
		return
	}
	if err := runner.Emit(ctx, event); err != nil {
		fmt.Fprintln(os.Stderr, "warning: "+err.Error())
	}
}

// ---------------------------------------------------------------------------
// Main
// ---------------------------------------------------------------------------

type floatFlag struct {
	name   string
	value  float64
	min    float64
	max    float64
	set    bool
	hasMax bool
}

func (f *floatFlag) Set(raw string) error {
	value, err := strconv.ParseFloat(raw, 64)
	if err != nil {
		return fmt.Errorf("parse %s: %w", f.name, err)
	}
	if value < f.min {
		return fmt.Errorf("%s must be >= %g", f.name, f.min)
	}
	if f.hasMax && value > f.max {
		return fmt.Errorf("%s must be <= %g", f.name, f.max)
	}
	f.value = value
	f.set = true
	return nil
}

func (f *floatFlag) String() string {
	if f == nil || !f.set {
		return ""
	}
	return strconv.FormatFloat(f.value, 'f', -1, 64)
}

type positiveIntFlag struct {
	name  string
	value int
	set   bool
}

func (f *positiveIntFlag) Set(raw string) error {
	value, err := strconv.Atoi(raw)
	if err != nil {
		return fmt.Errorf("parse %s: %w", f.name, err)
	}
	if value <= 0 {
		return fmt.Errorf("%s must be > 0", f.name)
	}
	f.value = value
	f.set = true
	return nil
}

func (f *positiveIntFlag) String() string {
	if f == nil || !f.set {
		return ""
	}
	return strconv.Itoa(f.value)
}

type stringListFlag []string

func (f *stringListFlag) Set(raw string) error {
	for value := range strings.SplitSeq(raw, ",") {
		value = strings.TrimSpace(value)
		if value != "" {
			*f = append(*f, value)
		}
	}
	return nil
}

func (f *stringListFlag) String() string {
	if f == nil {
		return ""
	}
	return strings.Join(*f, ",")
}

type generationSettings struct {
	Temperature *float64
	TopP        *float64
	MaxTokens   int
}

type cliOptions struct {
	oncePrompt          string
	agentName           string
	sessionDir          string
	sessionRef          string
	showSessionRef      string
	replayRef           string
	exportRef           string
	exportFormat        string
	searchQuery         string
	initConfigPath      string
	configPaths         string
	model               string
	describeAgentName   string
	sessionTitle        string
	mergeWorktreeRef    string
	sessionTags         stringListFlag
	maxTokens           positiveIntFlag
	temperature         floatFlag
	topP                floatFlag
	listModels          bool
	listKnownModels     bool
	listProviders       bool
	listAgents          bool
	listSessions        bool
	listSessionTags     bool
	listConfigPaths     bool
	validateConfig      bool
	printConfigTemplate bool
	doctor              bool
	readStdin           bool
	showVersion         bool
	useWorktree         bool
	listWorktrees       bool
	noAutoMerge         bool
}

//nolint:govet // field order follows app state grouping; padding is not performance-sensitive.
type appState struct {
	sessionState        session.Session
	contextOptions      contextref.Options
	generationDefaults  generationSettings
	generationOverrides generationSettings
	hookConfig          map[string][]appconfig.HookConfig
	agentRegistry       *agent.Registry
	hookRunner          *events.Runner
	sessionStore        *session.Store
	stateStore          *appconfig.StateStore
	registry            *llm.Registry
	worktreeInfo        *worktree.Info
	fallbackModels      []string
	providers           []string
	loadedConfigPaths   []string
	selectedModel       string
	selectedAgent       string
	cwd                 string
	modelLocked         bool
	autoMergeWorktree   bool
}

func parseOptions() cliOptions {
	var opts cliOptions
	opts.temperature = floatFlag{name: "temperature", min: 0}
	opts.topP = floatFlag{name: "top-p", min: 0, max: 1, hasMax: true}
	opts.maxTokens = positiveIntFlag{name: "max-tokens"}
	flag.StringVar(&opts.configPaths, "config", "", "additional YAML/JSON config file path(s); same format as ATTELER_CONFIG")
	flag.StringVar(&opts.initConfigPath, "init-config", "", "write a starter YAML config to this path without overwriting")
	flag.StringVar(&opts.sessionDir, "session-dir", "", "directory for session JSON files")
	flag.StringVar(&opts.sessionRef, "session", "", "session ID or path to continue")
	flag.StringVar(&opts.showSessionRef, "show-session", "", "print saved session details as YAML and exit")
	flag.StringVar(&opts.sessionTitle, "session-title", "", "set or update the saved session title")
	flag.Var(&opts.sessionTags, "session-tag", "add a saved session tag (repeatable or comma-separated)")
	flag.StringVar(&opts.replayRef, "replay", "", "session ID or path to print and exit")
	flag.StringVar(&opts.exportRef, "export-session", "", "session ID or path to export and exit")
	flag.StringVar(&opts.exportFormat, "export-format", "markdown", "session export format: markdown or json")
	flag.StringVar(&opts.searchQuery, "search-sessions", "", "search saved session transcripts and exit")
	flag.StringVar(&opts.oncePrompt, "once", "", "send one prompt and exit")
	flag.StringVar(&opts.model, "model", "", "model ID to use")
	flag.StringVar(&opts.agentName, "agent", "", "agent name to use for prompts")
	flag.StringVar(&opts.describeAgentName, "describe-agent", "", "print a configured agent as YAML and exit")
	flag.Var(&opts.temperature, "temperature", "override request temperature")
	flag.Var(&opts.topP, "top-p", "override request nucleus sampling value (0..1)")
	flag.Var(&opts.maxTokens, "max-tokens", "override request max output tokens")
	flag.BoolVar(&opts.listModels, "list-models", false, "list available models and exit")
	flag.BoolVar(&opts.listKnownModels, "list-known-models", false, "list built-in provider/model IDs without API calls and exit")
	flag.BoolVar(&opts.listProviders, "list-providers", false, "list built-in provider names without API calls and exit")
	flag.BoolVar(&opts.listAgents, "list-agents", false, "list configured agents and exit")
	flag.BoolVar(&opts.listSessions, "list-sessions", false, "list saved sessions and exit")
	flag.BoolVar(&opts.listSessionTags, "list-session-tags", false, "list saved session tags with counts and exit")
	flag.BoolVar(&opts.listConfigPaths, "list-config-paths", false, "list config files in load order and exit")
	flag.BoolVar(&opts.validateConfig, "validate-config", false, "validate merged YAML/JSON config and exit")
	flag.BoolVar(&opts.printConfigTemplate, "print-config-template", false, "print a starter YAML config and exit")
	flag.BoolVar(&opts.doctor, "doctor", false, "print local readiness diagnostics and exit")
	flag.BoolVar(&opts.readStdin, "stdin", false, "append stdin to a one-shot prompt")
	flag.BoolVar(&opts.showVersion, "version", false, "print version and exit")
	flag.BoolVar(&opts.useWorktree, "worktree", false, "isolate session in a git worktree")
	flag.BoolVar(&opts.listWorktrees, "list-worktrees", false, "list active atteler worktrees and exit")
	flag.BoolVar(&opts.noAutoMerge, "no-auto-merge", false, "keep worktree alive on exit instead of auto-merging")
	flag.StringVar(&opts.mergeWorktreeRef, "merge-worktree", "", "merge a session worktree back into its base branch and exit")
	flag.Parse()

	if opts.oncePrompt == "" && flag.NArg() > 0 {
		opts.oncePrompt = strings.Join(flag.Args(), " ")
	}

	return opts
}

func main() {
	if err := run(); err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		os.Exit(1)
	}
}

func versionString() string {
	return fmt.Sprintf("atteler %s (commit %s, built %s)", version, commit, date)
}

func initConfig(path string) error {
	path = strings.TrimSpace(path)
	if path == "" {
		return errors.New("config path is required")
	}

	dir := filepath.Dir(path)
	if dir != "." && dir != "" {
		if err := os.MkdirAll(dir, 0o750); err != nil {
			return fmt.Errorf("create config dir %s: %w", dir, err)
		}
	}

	file, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		if errors.Is(err, os.ErrExist) {
			return fmt.Errorf("config %s already exists", path)
		}
		return fmt.Errorf("create config %s: %w", path, err)
	}
	if _, err := file.WriteString(appconfig.TemplateYAML()); err != nil {
		_ = file.Close()
		return fmt.Errorf("write config %s: %w", path, err)
	}
	if err := file.Close(); err != nil {
		return fmt.Errorf("close config %s: %w", path, err)
	}

	fmt.Println("Wrote " + path)
	return nil
}

func oneShotPrompt(prompt string, readStdin bool) (string, error) {
	if readStdin {
		data, err := io.ReadAll(os.Stdin)
		if err != nil {
			return "", fmt.Errorf("read stdin: %w", err)
		}
		prompt = appendStdinContext(prompt, string(data))
	}
	if strings.TrimSpace(prompt) == "" {
		return "", errors.New("one-shot prompt is empty")
	}
	return prompt, nil
}

func appendStdinContext(prompt, stdin string) string {
	stdin = strings.TrimRight(stdin, "\n")
	if strings.TrimSpace(stdin) == "" {
		return prompt
	}
	if strings.TrimSpace(prompt) == "" {
		return stdin
	}
	return prompt + "\n\n<stdin>\n" + stdin + "\n</stdin>"
}

func listConfigPaths() {
	for _, path := range appconfig.DefaultPaths() {
		fmt.Println(path + "\t" + configPathStatus(path))
	}
}

func validateConfig() error {
	_, loaded, err := appconfig.Load()
	if err != nil {
		return fmt.Errorf("validate config: %w", err)
	}
	if len(loaded) == 0 {
		fmt.Println("Config valid: no config files loaded.")
		return nil
	}
	fmt.Println("Config valid: " + strings.Join(loaded, ", "))
	return nil
}

func configPathStatus(path string) string {
	info, err := os.Stat(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return "missing"
		}
		return "error: " + err.Error()
	}
	if info.IsDir() {
		return "directory"
	}
	return "present"
}

func listKnownProviders() {
	for _, provider := range knownProvidersSorted() {
		fmt.Println(provider.Name)
	}
}

func listKnownModels() {
	for _, provider := range knownProvidersSorted() {
		sort.Strings(provider.Models)
		for _, model := range provider.Models {
			fmt.Println(provider.Name + "/" + model)
		}
	}
}

func knownProvidersSorted() []llm.ProviderInfo {
	providers := llm.KnownProviders()
	sort.Slice(providers, func(i, j int) bool {
		return providers[i].Name < providers[j].Name
	})
	return providers
}

func run() error {
	opts := parseOptions()
	if opts.configPaths != "" {
		if err := os.Setenv(appconfig.EnvPath, opts.configPaths); err != nil {
			fmt.Fprintln(os.Stderr, "warning: cannot set config path override: "+err.Error())
		}
	}
	if opts.printConfigTemplate {
		fmt.Print(appconfig.TemplateYAML())
		return nil
	}
	if opts.showVersion {
		fmt.Println(versionString())
		return nil
	}
	if opts.initConfigPath != "" {
		return initConfig(opts.initConfigPath)
	}
	if opts.listConfigPaths {
		listConfigPaths()
		return nil
	}
	if opts.validateConfig {
		return validateConfig()
	}
	if opts.listProviders {
		listKnownProviders()
		return nil
	}
	if opts.listKnownModels {
		listKnownModels()
		return nil
	}
	if opts.listWorktrees {
		return listWorktrees()
	}
	if opts.mergeWorktreeRef != "" {
		return mergeWorktreeBySession(opts.mergeWorktreeRef)
	}

	state, err := loadAppState(opts)
	if err != nil {
		return err
	}

	return runWithState(opts, state)
}

func runWithState(opts cliOptions, state appState) error {
	switch {
	case opts.replayRef != "":
		printTranscript(state.sessionState)
		return nil
	case opts.showSessionRef != "":
		return showSession(state.sessionState, state.sessionStore.Path(state.sessionState.ID))
	case opts.exportRef != "":
		return exportSession(state.sessionState, opts.exportFormat)
	case opts.listModels:
		return listModels(context.Background(), state.registry)
	case opts.listAgents:
		listAgents(state.agentRegistry)
		return nil
	case opts.describeAgentName != "":
		return describeAgent(state.agentRegistry, opts.describeAgentName)
	case opts.listSessions:
		return listSessions(state.sessionStore)
	case opts.listSessionTags:
		return listSessionTags(state.sessionStore)
	case opts.searchQuery != "":
		return searchSessions(state.sessionStore, opts.searchQuery)
	case opts.doctor:
		return doctor(state)
	case opts.oncePrompt != "" || opts.readStdin:
		prompt, err := oneShotPrompt(opts.oncePrompt, opts.readStdin)
		if err != nil {
			return err
		}
		// One-shot mode uses a logger-enabled runner so context-based events
		// (e.g. tool_execute from providers) are visible on stderr.
		state.hookRunner = events.NewRunnerWithLogger(state.hookConfig, os.Stderr)
		runErr := runOnce(
			context.Background(),
			state.registry,
			state.agentRegistry,
			state.hookRunner,
			state.sessionStore,
			state.sessionState,
			state.contextOptions,
			state.selectedModel,
			state.selectedAgent,
			state.fallbackModels,
			state.generationDefaults,
			state.generationOverrides,
			state.modelLocked,
			prompt,
		)
		finalizeWorktree(&state)
		return runErr
	default:
		return runInteractive(state)
	}
}

func loadAppState(opts cliOptions) (appState, error) {
	cfg, loadedConfigPaths, err := appconfig.Load()
	if err != nil {
		fmt.Fprintln(os.Stderr, "warning: "+err.Error())
	}

	reg := llm.AutoRegisterWithConfig(llmConfig(cfg))
	agentRegistry := agent.NewRegistry(cfg.Agents)
	hookRunner := events.NewRunnerWithLogger(cfg.Hooks, nil)
	store := session.NewStore(opts.sessionDir)
	stateStore := appconfig.NewStateStore("")
	persistedState, stateErr := stateStore.Load()
	if stateErr != nil {
		fmt.Fprintln(os.Stderr, "warning: "+stateErr.Error())
	}
	cwd, cwdErr := os.Getwd()
	if cwdErr != nil {
		cwd = ""
	}
	contextOptions := contextOptionsFromConfig(cfg)
	generationDefaults := generationFromConfig(cfg)
	generationOverrides := generationFromOptions(opts)

	providers := reg.ListProviders()
	if len(providers) == 0 {
		fmt.Fprintln(os.Stderr, "warning: no LLM providers configured, set ANTHROPIC_API_KEY or OPENAI_API_KEY")
	}

	selection, err := resolveSelection(opts, cfg, persistedState.ModelForFolder(cwd), agentRegistry, store)
	if err != nil {
		return appState{}, err
	}

	// Set up git worktree isolation when requested.
	var wtInfo *worktree.Info
	if opts.useWorktree && cwd != "" {
		// If continuing a session that already has a worktree, re-use it.
		if selection.sessionState.WorktreePath != "" {
			wtInfo = &worktree.Info{
				Path:       selection.sessionState.WorktreePath,
				Branch:     selection.sessionState.WorktreeBranch,
				BaseBranch: selection.sessionState.WorktreeBase,
				SessionID:  selection.sessionState.ID,
			}
			fmt.Fprintln(os.Stderr, "worktree: reusing "+wtInfo.Path)
		} else {
			wtInfo, err = worktree.Create(cwd, selection.sessionState.ID)
			if err != nil {
				return appState{}, fmt.Errorf("worktree setup: %w", err)
			}
			selection.sessionState.WorktreePath = wtInfo.Path
			selection.sessionState.WorktreeBranch = wtInfo.Branch
			selection.sessionState.WorktreeBase = wtInfo.BaseBranch
			fmt.Fprintln(os.Stderr, "worktree: created "+wtInfo.Path+" (branch "+wtInfo.Branch+")")
		}

		// Update context references to point at the worktree.
		contextOptions.Root = wtInfo.Path
	}

	return appState{
		registry:            reg,
		agentRegistry:       agentRegistry,
		hookRunner:          hookRunner,
		sessionStore:        store,
		stateStore:          stateStore,
		contextOptions:      contextOptions,
		sessionState:        selection.sessionState,
		worktreeInfo:        wtInfo,
		cwd:                 cwd,
		loadedConfigPaths:   loadedConfigPaths,
		providers:           providers,
		selectedModel:       selection.selectedModel,
		selectedAgent:       selection.selectedAgent,
		fallbackModels:      selection.fallbackModels,
		generationDefaults:  generationDefaults,
		generationOverrides: generationOverrides,
		hookConfig:          cfg.Hooks,
		modelLocked:         selection.modelLocked,
		autoMergeWorktree:   opts.useWorktree && !opts.noAutoMerge,
	}, nil
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
	if opts.sessionRef == "" && opts.replayRef == "" && opts.exportRef == "" && opts.showSessionRef == "" {
		return nil
	}

	ref := firstNonEmpty(opts.replayRef, opts.showSessionRef, opts.exportRef, opts.sessionRef)
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
	if state.selectedAgent == "" || opts.replayRef != "" || opts.exportRef != "" || opts.showSessionRef != "" {
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

func runInteractive(state appState) error {
	fmt.Println(promptStyle.Render("atteler") + dimStyle.Render("  Ctrl+D to quit, Ctrl+O to pick model"))
	if len(state.loadedConfigPaths) > 0 {
		fmt.Println(dimStyle.Render("  Config: " + strings.Join(state.loadedConfigPaths, ", ")))
	}
	fmt.Println(dimStyle.Render("  Session: " + state.sessionState.ID + " (" + state.sessionStore.Path(state.sessionState.ID) + ")"))
	if state.sessionState.Title != "" {
		fmt.Println(dimStyle.Render("  Title: ") + pickerSelectedStyle.Render(state.sessionState.Title))
	}
	if len(state.sessionState.Tags) > 0 {
		fmt.Println(dimStyle.Render("  Tags: ") + pickerSelectedStyle.Render(strings.Join(state.sessionState.Tags, ", ")))
	}
	if len(state.providers) > 0 {
		sort.Strings(state.providers)
		fmt.Println(dimStyle.Render("  Connected providers: ") + pickerProviderStyle.Render(strings.Join(state.providers, ", ")))
	}
	if agents := state.agentRegistry.List(); len(agents) > 0 {
		fmt.Println(dimStyle.Render("  Agents: ") + pickerProviderStyle.Render(strings.Join(agents, ", ")))
	}
	if state.worktreeInfo != nil {
		fmt.Println(dimStyle.Render("  Worktree: ") + pickerProviderStyle.Render(state.worktreeInfo.Path) +
			dimStyle.Render(" (branch ") + pickerSelectedStyle.Render(state.worktreeInfo.Branch) + dimStyle.Render(")"))
	}
	if len(state.sessionState.Messages) > 0 {
		fmt.Println(dimStyle.Render("  Loaded transcript:"))
		printTranscript(state.sessionState)
	}

	emitHookWarning(context.Background(), state.hookRunner, events.Event{
		Type:        events.SessionStart,
		SessionID:   state.sessionState.ID,
		SessionPath: state.sessionStore.Path(state.sessionState.ID),
		Agent:       state.selectedAgent,
		Model:       state.selectedModel,
	})

	p := tea.NewProgram(initialModel(
		state.registry,
		state.agentRegistry,
		state.hookRunner,
		state.sessionStore,
		state.stateStore,
		state.sessionState,
		state.contextOptions,
		state.sessionStore.Path(state.sessionState.ID),
		state.cwd,
		state.selectedModel,
		state.selectedAgent,
		state.fallbackModels,
		state.generationDefaults,
		state.generationOverrides,
		state.modelLocked,
		state.worktreeInfo,
	))
	if _, err := p.Run(); err != nil {
		emitHookWarning(context.Background(), state.hookRunner, events.Event{
			Type:        events.Error,
			SessionID:   state.sessionState.ID,
			SessionPath: state.sessionStore.Path(state.sessionState.ID),
			Agent:       state.selectedAgent,
			Model:       state.selectedModel,
			Error:       err.Error(),
		})
		return fmt.Errorf("run TUI: %w", err)
	}
	emitHookWarning(context.Background(), state.hookRunner, events.Event{
		Type:        events.SessionEnd,
		SessionID:   state.sessionState.ID,
		SessionPath: state.sessionStore.Path(state.sessionState.ID),
		Agent:       state.selectedAgent,
		Model:       state.selectedModel,
	})

	finalizeWorktree(&state)

	return nil
}

func runOnce(
	ctx context.Context,
	reg *llm.Registry,
	agents *agent.Registry,
	hooks *events.Runner,
	store *session.Store,
	sessionState session.Session,
	contextOptions contextref.Options,
	selectedModel string,
	selectedAgent string,
	fallbackModels []string,
	generationDefaults generationSettings,
	generationOverrides generationSettings,
	modelLocked bool,
	prompt string,
) error {
	activeAgent, userPrompt, selectionErr := resolveAgent(agents, selectedAgent, prompt)
	if selectionErr != nil {
		return selectionErr
	}
	prompt = userPrompt
	requestMessages, refs, err := expandReferences([]llm.Message{{Role: llm.RoleUser, Content: prompt}}, contextOptions)
	if err != nil {
		return err
	}
	requestModel, fallbackModels := requestModelAndFallbacks(selectedModel, modelLocked, fallbackModels, activeAgent)
	generation := generationForRequest(generationDefaults, generationOverrides, activeAgent)
	if requestModel != "" {
		sessionState.DefaultModel = requestModel
	}
	sessionState.DefaultAgent = activeAgent.name

	emitHookWarning(ctx, hooks, events.Event{
		Type:        events.SessionStart,
		SessionID:   sessionState.ID,
		SessionPath: store.Path(sessionState.ID),
		Agent:       activeAgent.name,
		Model:       sessionState.DefaultModel,
	})
	defer emitHookWarning(ctx, hooks, events.Event{
		Type:        events.SessionEnd,
		SessionID:   sessionState.ID,
		SessionPath: store.Path(sessionState.ID),
		Agent:       activeAgent.name,
		Model:       sessionState.DefaultModel,
	})

	sessionState.Append(llm.RoleUser, prompt)
	if saveErr := store.Save(sessionState); saveErr != nil {
		emitHookWarning(ctx, hooks, events.Event{
			Type:        events.Error,
			SessionID:   sessionState.ID,
			SessionPath: store.Path(sessionState.ID),
			Agent:       activeAgent.name,
			Model:       sessionState.DefaultModel,
			Error:       saveErr.Error(),
		})
		return fmt.Errorf("save session before request: %w", saveErr)
	}
	emitFileWriteWarning(ctx, hooks, sessionState, store.Path(sessionState.ID), activeAgent.name, "session")
	for _, ref := range refs {
		emitHookWarning(ctx, hooks, fileReadEvent(sessionState.ID, store.Path(sessionState.ID), activeAgent.name, sessionState.DefaultModel, ref))
		emitHookWarning(ctx, hooks, contextAddEvent(sessionState.ID, store.Path(sessionState.ID), activeAgent.name, sessionState.DefaultModel, ref))
	}
	if activeAgent.ok {
		emitHookWarning(ctx, hooks, events.Event{
			Type:        events.AgentExecute,
			SessionID:   sessionState.ID,
			SessionPath: store.Path(sessionState.ID),
			Agent:       activeAgent.name,
			Model:       sessionState.DefaultModel,
			Metadata: map[string]string{
				"agent": activeAgent.name,
			},
		})
	}
	emitHookWarning(ctx, hooks, events.Event{
		Type:        events.UserMessage,
		SessionID:   sessionState.ID,
		SessionPath: store.Path(sessionState.ID),
		Agent:       activeAgent.name,
		Model:       sessionState.DefaultModel,
		Role:        string(llm.RoleUser),
		Content:     prompt,
		Metadata:    referenceMetadata(refs),
	})
	if len(refs) > 0 {
		fmt.Fprintln(os.Stderr, "context: "+referenceSummary(refs))
	}

	params := llm.CompleteParams{
		Model:    requestModel,
		Messages: append(append([]llm.Message(nil), sessionState.Messages[:len(sessionState.Messages)-1]...), requestMessages...),
	}
	if activeAgent.ok {
		params = activeAgent.agent.CompleteParams(requestModel, params.Messages)
	}
	applyGenerationParams(&params, generation)

	ctx = events.WithEmitter(ctx, hooks, events.Event{
		SessionID:   sessionState.ID,
		SessionPath: store.Path(sessionState.ID),
		Agent:       activeAgent.name,
		Model:       requestModel,
	})
	resp, err := reg.CompleteWithFallback(ctx, params, fallbackModels)
	if err != nil {
		emitHookWarning(ctx, hooks, events.Event{
			Type:        events.Error,
			SessionID:   sessionState.ID,
			SessionPath: store.Path(sessionState.ID),
			Agent:       activeAgent.name,
			Model:       sessionState.DefaultModel,
			Error:       err.Error(),
		})
		return fmt.Errorf("complete prompt: %w", err)
	}

	sessionState.Append(llm.RoleAssistant, resp.Content)
	if resp.Model != "" {
		sessionState.DefaultModel = resp.Model
	}
	if err := store.Save(sessionState); err != nil {
		emitHookWarning(ctx, hooks, events.Event{
			Type:        events.Error,
			SessionID:   sessionState.ID,
			SessionPath: store.Path(sessionState.ID),
			Agent:       activeAgent.name,
			Model:       sessionState.DefaultModel,
			Error:       err.Error(),
		})
		return fmt.Errorf("save session after response: %w", err)
	}
	emitFileWriteWarning(ctx, hooks, sessionState, store.Path(sessionState.ID), activeAgent.name, "session")
	emitHookWarning(ctx, hooks, events.Event{
		Type:        events.AssistantMessage,
		SessionID:   sessionState.ID,
		SessionPath: store.Path(sessionState.ID),
		Agent:       activeAgent.name,
		Model:       resp.Model,
		Role:        string(llm.RoleAssistant),
		Content:     resp.Content,
	})

	fmt.Println(resp.Content)
	fmt.Fprintln(os.Stderr, "session: "+sessionState.ID+" ("+store.Path(sessionState.ID)+")")
	return nil
}

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

func listModels(ctx context.Context, reg *llm.Registry) error {
	providers := reg.ListProviders()
	sort.Strings(providers)
	if len(providers) == 0 {
		return errors.New("no providers registered")
	}

	for _, provider := range providers {
		models, err := reg.ProviderModels(ctx, provider)
		if err != nil {
			return fmt.Errorf("list %s models: %w", provider, err)
		}
		sort.Strings(models)
		for _, model := range models {
			fmt.Println(provider + "/" + model)
		}
	}
	return nil
}

func listAgents(agents *agent.Registry) {
	for _, name := range agents.List() {
		fmt.Println(name)
	}
}

type agentDescription struct {
	Temperature    *float64 `yaml:"temperature,omitempty"`
	TopP           *float64 `yaml:"top_p,omitempty"`
	Name           string   `yaml:"name"`
	Model          string   `yaml:"model,omitempty"`
	SystemPrompt   string   `yaml:"system_prompt,omitempty"`
	FallbackModels []string `yaml:"fallback_models,omitempty"`
	Triggers       []string `yaml:"triggers,omitempty"`
	MaxTokens      int      `yaml:"max_tokens,omitempty"`
}

func describeAgent(agents *agent.Registry, name string) error {
	activeAgent, ok := agents.Get(name)
	if !ok {
		return fmt.Errorf("unknown agent %q", name)
	}

	out, err := formatAgentDescription(activeAgent)
	if err != nil {
		return fmt.Errorf("format agent %q: %w", name, err)
	}
	fmt.Print(out)
	return nil
}

func formatAgentDescription(activeAgent agent.Agent) (string, error) {
	out, err := yaml.Marshal(agentDescription{
		Name:           activeAgent.Name,
		Model:          activeAgent.Model,
		SystemPrompt:   activeAgent.SystemPrompt,
		FallbackModels: activeAgent.FallbackModels,
		Temperature:    activeAgent.Temperature,
		TopP:           activeAgent.TopP,
		Triggers:       activeAgent.Triggers,
		MaxTokens:      activeAgent.MaxTokens,
	})
	if err != nil {
		return "", fmt.Errorf("marshal agent description: %w", err)
	}
	return string(out), nil
}

func doctor(state appState) error {
	fmt.Println("Atteler doctor")
	if len(state.loadedConfigPaths) == 0 {
		fmt.Println("config: no config files loaded")
	} else {
		fmt.Println("config: " + strings.Join(state.loadedConfigPaths, ", "))
	}
	fmt.Println("sessions: " + state.sessionStore.Dir() + " (" + pathStatus(state.sessionStore.Dir()) + ")")

	providers := state.registry.ListProviders()
	sort.Strings(providers)
	if len(providers) == 0 {
		fmt.Println("providers: none registered")
	} else {
		fmt.Println("providers: " + strings.Join(providers, ", "))
	}

	agents := state.agentRegistry.List()
	if len(agents) == 0 {
		fmt.Println("agents: none configured")
	} else {
		fmt.Println("agents: " + strings.Join(agents, ", "))
	}

	if state.worktreeInfo != nil {
		fmt.Println("worktree: " + worktree.Status(state.worktreeInfo))
	}

	if len(providers) == 0 {
		return errors.New("doctor: no providers registered; set provider credentials or config")
	}

	// Health check every registered provider and list their models.
	fmt.Println()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	results := state.registry.CheckHealth(ctx)
	healthy := 0
	for _, r := range results {
		if r.Healthy {
			fmt.Printf("  [ok] %s\n", r.Name)
			healthy++
		} else {
			fmt.Printf("  [FAIL] %s: %v\n", r.Name, r.Error)
		}
		for _, m := range r.Models {
			fmt.Printf("         - %s\n", m)
		}
	}

	if healthy == 0 {
		return errors.New("doctor: all providers failed their health check")
	}
	return nil
}

func pathStatus(path string) string {
	info, err := os.Stat(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return "will be created on first save"
		}
		return "error: " + err.Error()
	}
	if !info.IsDir() {
		return "not a directory"
	}
	return "ok"
}

func listSessions(store *session.Store) error {
	summaries, err := store.List()
	if err != nil {
		return fmt.Errorf("list sessions: %w", err)
	}
	if len(summaries) == 0 {
		fmt.Println("No sessions found.")
		return nil
	}

	for i := range summaries {
		fmt.Println(formatSessionSummary(summaries[i]))
	}
	return nil
}

func listSessionTags(store *session.Store) error {
	tags, err := store.Tags()
	if err != nil {
		return fmt.Errorf("list session tags: %w", err)
	}
	if len(tags) == 0 {
		fmt.Println("No session tags found.")
		return nil
	}
	for _, tag := range tags {
		fmt.Println(formatTagSummary(tag))
	}
	return nil
}

func formatTagSummary(tag session.TagSummary) string {
	return fmt.Sprintf("%s\t%d sessions", tag.Tag, tag.Sessions)
}

func searchSessions(store *session.Store, query string) error {
	results, err := store.Search(query)
	if err != nil {
		return fmt.Errorf("search sessions: %w", err)
	}
	if len(results) == 0 {
		fmt.Println("No matching sessions found.")
		return nil
	}

	for i := range results {
		result := &results[i]
		fmt.Println(formatSessionSummary(result.Summary))
		for _, snippet := range result.Snippets {
			fmt.Println(formatSearchSnippet(snippet))
		}
	}
	return nil
}

func formatSessionSummary(summary session.Summary) string {
	updated := "-"
	if !summary.UpdatedAt.IsZero() {
		updated = summary.UpdatedAt.UTC().Format(time.RFC3339)
	}
	agentName := "-"
	if summary.DefaultAgent != "" {
		agentName = summary.DefaultAgent
	}
	modelName := "-"
	if summary.DefaultModel != "" {
		modelName = summary.DefaultModel
	}

	parts := []string{
		summary.ID,
		updated,
		fmt.Sprintf("%d messages", summary.Messages),
		"agent=" + agentName,
		"model=" + modelName,
	}
	if summary.Title != "" {
		parts = append(parts, "title="+summary.Title)
	}
	if len(summary.Tags) > 0 {
		parts = append(parts, "tags="+strings.Join(summary.Tags, ","))
	}
	parts = append(parts, summary.Path)
	return strings.Join(parts, "\t")
}

func formatSearchSnippet(snippet session.SearchSnippet) string {
	role := string(snippet.Role)
	if role == "" {
		role = "message"
	}
	if snippet.Text == "" {
		return "  " + role + ":"
	}
	return "  " + role + ": " + snippet.Text
}

type sessionDetails struct {
	CreatedAt      time.Time     `yaml:"created_at"`
	UpdatedAt      time.Time     `yaml:"updated_at"`
	ID             string        `yaml:"id"`
	Path           string        `yaml:"path"`
	Title          string        `yaml:"title,omitempty"`
	DefaultAgent   string        `yaml:"default_agent,omitempty"`
	DefaultModel   string        `yaml:"default_model,omitempty"`
	WorktreePath   string        `yaml:"worktree_path,omitempty"`
	WorktreeBranch string        `yaml:"worktree_branch,omitempty"`
	WorktreeBase   string        `yaml:"worktree_base,omitempty"`
	Tags           []string      `yaml:"tags,omitempty"`
	Messages       []yamlMessage `yaml:"messages,omitempty"`
}

type yamlMessage struct {
	Role    llm.Role `yaml:"role"`
	Content string   `yaml:"content"`
}

func showSession(sessionState session.Session, path string) error {
	out, err := formatSessionDetails(sessionState, path)
	if err != nil {
		return fmt.Errorf("format session %q: %w", sessionState.ID, err)
	}
	fmt.Print(out)
	return nil
}

func formatSessionDetails(sessionState session.Session, path string) (string, error) {
	out, err := yaml.Marshal(sessionDetails{
		ID:             sessionState.ID,
		Path:           path,
		Title:          sessionState.Title,
		CreatedAt:      sessionState.CreatedAt,
		UpdatedAt:      sessionState.UpdatedAt,
		DefaultAgent:   sessionState.DefaultAgent,
		DefaultModel:   sessionState.DefaultModel,
		WorktreePath:   sessionState.WorktreePath,
		WorktreeBranch: sessionState.WorktreeBranch,
		WorktreeBase:   sessionState.WorktreeBase,
		Tags:           sessionState.Tags,
		Messages:       yamlMessages(sessionState.Messages),
	})
	if err != nil {
		return "", fmt.Errorf("marshal session details: %w", err)
	}
	return string(out), nil
}

func yamlMessages(messages []llm.Message) []yamlMessage {
	out := make([]yamlMessage, 0, len(messages))
	for _, message := range messages {
		out = append(out, yamlMessage{
			Role:    message.Role,
			Content: message.Content,
		})
	}
	return out
}

func exportSession(sessionState session.Session, format string) error {
	switch strings.ToLower(strings.TrimSpace(format)) {
	case "", "markdown", "md":
		fmt.Print(session.Markdown(sessionState))
	case "json":
		encoder := json.NewEncoder(os.Stdout)
		encoder.SetIndent("", "  ")
		if err := encoder.Encode(sessionState); err != nil {
			return fmt.Errorf("encode session json: %w", err)
		}
	default:
		return fmt.Errorf("unsupported export format %q (supported: markdown, json)", format)
	}
	return nil
}

func printTranscript(sessionState session.Session) {
	for _, msg := range sessionState.Messages {
		switch msg.Role {
		case llm.RoleUser:
			fmt.Println(userLabel.Render("You") + " " + msg.Content)
		case llm.RoleAssistant:
			fmt.Println(assistantLabel.Render("Assistant") + " " + msg.Content)
		default:
			fmt.Println(dimStyle.Render(string(msg.Role)) + " " + msg.Content)
		}
	}
}

func llmConfig(cfg appconfig.Config) llm.AutoRegisterConfig {
	providers := make(map[string]llm.ProviderConfig, len(cfg.Providers))
	for name, provider := range cfg.Providers {
		providers[name] = llm.ProviderConfig{
			Disabled: provider.Disabled,
			BaseURL:  provider.BaseURL,
		}
	}

	if len(providers) == 0 {
		providers = nil
	}

	return llm.AutoRegisterConfig{
		DefaultProvider: cfg.DefaultProvider,
		DefaultModel:    cfg.DefaultModel,
		Providers:       providers,
	}
}

func generationFromConfig(cfg appconfig.Config) generationSettings {
	return generationSettings{
		Temperature: cfg.Generation.Temperature,
		TopP:        cfg.Generation.TopP,
		MaxTokens:   cfg.Generation.MaxTokens,
	}
}

func generationFromOptions(opts cliOptions) generationSettings {
	var generation generationSettings
	if opts.temperature.set {
		generation.Temperature = &opts.temperature.value
	}
	if opts.topP.set {
		generation.TopP = &opts.topP.value
	}
	if opts.maxTokens.set {
		generation.MaxTokens = opts.maxTokens.value
	}
	return generation
}

func generationForRequest(
	defaults generationSettings,
	overrides generationSettings,
	activeAgent agentSelection,
) generationSettings {
	generation := defaults
	if activeAgent.ok {
		generation = mergeGenerationSettings(generation, generationSettings{
			Temperature: activeAgent.agent.Temperature,
			TopP:        activeAgent.agent.TopP,
			MaxTokens:   activeAgent.agent.MaxTokens,
		})
	}
	return mergeGenerationSettings(generation, overrides)
}

func mergeGenerationSettings(base, override generationSettings) generationSettings {
	if override.Temperature != nil {
		base.Temperature = override.Temperature
	}
	if override.TopP != nil {
		base.TopP = override.TopP
	}
	if override.MaxTokens > 0 {
		base.MaxTokens = override.MaxTokens
	}
	return base
}

func applyGenerationParams(params *llm.CompleteParams, generation generationSettings) {
	params.Temperature = generation.Temperature
	params.TopP = generation.TopP
	if generation.MaxTokens > 0 {
		params.MaxTokens = generation.MaxTokens
	}
}

func mergeTags(existing, next []string) []string {
	out := make([]string, 0, len(existing)+len(next))
	seen := make(map[string]bool, len(existing)+len(next))
	for _, tag := range append(append([]string(nil), existing...), next...) {
		tag = strings.TrimSpace(tag)
		tagKey := strings.ToLower(tag)
		if tag == "" || seen[tagKey] {
			continue
		}
		seen[tagKey] = true
		out = append(out, tag)
	}
	return out
}

func contextOptionsFromConfig(cfg appconfig.Config) contextref.Options {
	opts := contextref.Options{
		MaxFileBytes:  cfg.Context.MaxFileBytes,
		MaxTotalBytes: cfg.Context.MaxTotalBytes,
	}
	if cwd, err := os.Getwd(); err == nil {
		opts.Root = cwd
	}
	return opts
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if value != "" {
			return value
		}
	}
	return ""
}

// ---------------------------------------------------------------------------
// Worktree commands
// ---------------------------------------------------------------------------

// finalizeWorktree auto-merges the session worktree when enabled, or prints
// a reminder for manual merge.
func finalizeWorktree(state *appState) {
	if state.worktreeInfo == nil {
		return
	}

	if !state.autoMergeWorktree {
		fmt.Fprintln(os.Stderr, "worktree: session files are in "+state.worktreeInfo.Path)
		fmt.Fprintln(os.Stderr, "worktree: merge with: atteler --merge-worktree "+state.sessionState.ID)
		return
	}

	fmt.Fprintln(os.Stderr, "worktree: merging "+state.worktreeInfo.Branch+" into "+state.worktreeInfo.BaseBranch+"...")

	if err := worktree.Merge(state.cwd, state.worktreeInfo); err != nil {
		fmt.Fprintln(os.Stderr, "worktree: auto-merge failed: "+err.Error())
		fmt.Fprintln(os.Stderr, "worktree: files preserved in "+state.worktreeInfo.Path)
		fmt.Fprintln(os.Stderr, "worktree: retry with: atteler --merge-worktree "+state.sessionState.ID)
		return
	}

	state.sessionState.WorktreePath = ""
	state.sessionState.WorktreeBranch = ""
	state.sessionState.WorktreeBase = ""
	if saveErr := state.sessionStore.Save(state.sessionState); saveErr != nil {
		fmt.Fprintln(os.Stderr, "warning: could not update session after merge: "+saveErr.Error())
	}
	fmt.Fprintln(os.Stderr, "worktree: merged and cleaned up")
}

func listWorktrees() error {
	cwd, err := os.Getwd()
	if err != nil {
		return fmt.Errorf("list worktrees: %w", err)
	}
	if !worktree.IsGitRepo(cwd) {
		return errors.New("list worktrees: not inside a git repository")
	}

	infos, err := worktree.List(cwd)
	if err != nil {
		return fmt.Errorf("list worktrees: %w", err)
	}
	if len(infos) == 0 {
		fmt.Println("No active atteler worktrees.")
		return nil
	}
	for i := range infos {
		info := &infos[i]
		fmt.Printf("%s\tbranch=%s\tbase=%s\tsession=%s\n",
			info.Path, info.Branch, info.BaseBranch, info.SessionID)
	}
	return nil
}

func mergeWorktreeBySession(sessionRef string) error {
	cwd, err := os.Getwd()
	if err != nil {
		return fmt.Errorf("merge worktree: %w", err)
	}
	if !worktree.IsGitRepo(cwd) {
		return errors.New("merge worktree: not inside a git repository")
	}

	store := session.NewStore("")
	sess, err := store.Load(sessionRef)
	if err != nil {
		return fmt.Errorf("merge worktree: load session: %w", err)
	}
	if sess.WorktreePath == "" {
		return fmt.Errorf("merge worktree: session %s has no worktree", sess.ID)
	}

	info := &worktree.Info{
		Path:       sess.WorktreePath,
		Branch:     sess.WorktreeBranch,
		BaseBranch: sess.WorktreeBase,
		SessionID:  sess.ID,
	}

	fmt.Fprintf(os.Stderr, "worktree: merging %s into %s...\n", info.Branch, info.BaseBranch)
	if err := worktree.Merge(cwd, info); err != nil {
		return fmt.Errorf("merge worktree: %w", err)
	}

	// Clear worktree metadata from the session.
	sess.WorktreePath = ""
	sess.WorktreeBranch = ""
	sess.WorktreeBase = ""
	if err := store.Save(sess); err != nil {
		fmt.Fprintf(os.Stderr, "warning: could not update session after merge: %v\n", err)
	}

	fmt.Fprintf(os.Stderr, "worktree: merged and cleaned up session %s\n", sess.ID)
	return nil
}
