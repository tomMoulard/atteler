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
	"log"
	"maps"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/charmbracelet/bubbles/key"
	"github.com/charmbracelet/bubbles/textarea"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"gopkg.in/yaml.v3"

	"github.com/tommoulard/atteler/pkg/agent"
	"github.com/tommoulard/atteler/pkg/agentmemory"
	"github.com/tommoulard/atteler/pkg/artifactmerge"
	attasync "github.com/tommoulard/atteler/pkg/async"
	"github.com/tommoulard/atteler/pkg/codegraph"
	"github.com/tommoulard/atteler/pkg/codeintel"
	appconfig "github.com/tommoulard/atteler/pkg/config"
	"github.com/tommoulard/atteler/pkg/contextpack"
	"github.com/tommoulard/atteler/pkg/contextref"
	atteval "github.com/tommoulard/atteler/pkg/eval"
	"github.com/tommoulard/atteler/pkg/events"
	"github.com/tommoulard/atteler/pkg/feedback"
	"github.com/tommoulard/atteler/pkg/githistory"
	"github.com/tommoulard/atteler/pkg/llm"
	"github.com/tommoulard/atteler/pkg/lsp"
	"github.com/tommoulard/atteler/pkg/mcp"
	"github.com/tommoulard/atteler/pkg/memory"
	"github.com/tommoulard/atteler/pkg/modelroute"
	attelerplugin "github.com/tommoulard/atteler/pkg/plugin"
	"github.com/tommoulard/atteler/pkg/promptcomplete"
	"github.com/tommoulard/atteler/pkg/review"
	"github.com/tommoulard/atteler/pkg/session"
	attshell "github.com/tommoulard/atteler/pkg/shell"
	attskill "github.com/tommoulard/atteler/pkg/skill"
	"github.com/tommoulard/atteler/pkg/speculate"
	"github.com/tommoulard/atteler/pkg/subagent"
	"github.com/tommoulard/atteler/pkg/vector"
	"github.com/tommoulard/atteler/pkg/watch"
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
	keyCtrlC         = "ctrl+c"
	keyEnter         = "enter"
	keyEsc           = "esc"
	outputFormatJSON = "json"
	outputFormatText = "text"

	maxPromptHistoryEntries = 100
)

// ---------------------------------------------------------------------------
// Messages (tea.Msg)
// ---------------------------------------------------------------------------

// llmResponseMsg is sent when the LLM call completes.
type llmResponseMsg struct {
	err        error
	content    string
	model      string
	tokenUsage tokenUsage
}

type tokenUsage struct {
	InputTokens       int `json:"input_tokens"`
	CachedInputTokens int `json:"cached_input_tokens"`
	OutputTokens      int `json:"output_tokens"`
	Responses         int `json:"responses"`
}

func (u *tokenUsage) addResponse(resp *llm.Response) {
	if resp == nil {
		return
	}
	u.InputTokens += resp.InputTokens
	u.CachedInputTokens += resp.CachedInputTokens
	u.OutputTokens += resp.OutputTokens
	u.Responses++
}

func (u *tokenUsage) add(next tokenUsage) {
	u.InputTokens += next.InputTokens
	u.CachedInputTokens += next.CachedInputTokens
	u.OutputTokens += next.OutputTokens
	u.Responses += next.Responses
}

//nolint:govet // Field order groups request concerns; padding is not performance-sensitive.
type llmRequest struct {
	generation     generationSettings
	maxInputTokens int
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

type completionCandidate struct {
	label string
	value string
	kind  string
}

// ---------------------------------------------------------------------------
// Model
// ---------------------------------------------------------------------------

//nolint:govet // Field order groups related TUI state instead of optimizing padding.
type model struct {
	ctx                 context.Context
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
	promptHistory       []string
	promptHistoryDraft  string
	pickerItems         []pickerItem
	contextOptions      contextref.Options
	worktreeInfo        *worktree.Info
	tokenUsage          tokenUsage
	pickerCursor        int
	completionCursor    int
	promptHistoryCursor int
	maxInputTokens      int
	width               int
	quitting            bool
	waiting             bool
	pickerOpen          bool
	pickerLoading       bool
	scopePickerOpen     bool
	completionOpen      bool
	modelLocked         bool
	revampUndoActive    bool
	completionItems     []completionCandidate
	revampUndo          string
}

func initialModel(
	ctx context.Context,
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
	maxInputTokens int,
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
		ctx:                 nonNilContext(ctx),
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
		maxInputTokens:      maxInputTokens,
		history:             append([]llm.Message(nil), sessionState.Messages...),
		promptHistory:       promptHistoryFromStore(store, sessionState, maxPromptHistoryEntries),
		promptHistoryCursor: -1,
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
			emitHook(m.ctx, m.hookRunner, events.Event{
				Type:        events.CommandExecute,
				SessionID:   m.sessionState.ID,
				SessionPath: m.sessionPath,
				Agent:       m.selectedAgent,
				Model:       m.selectedModel,
				Metadata: map[string]string{
					"command": "fzf",
				},
			}),
			runFZFModelPicker(m.ctx, fzfPath, msg.items),
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

	if next, cmd, handled := m.handleChatCommand(msg.String()); handled {
		return next, cmd
	}

	// Propagate to the textarea when not waiting.
	if !m.waiting {
		m.completionOpen = false
		m.completionItems = nil
		var taCmd tea.Cmd
		m.textarea, taCmd = m.textarea.Update(msg)
		cmds = append(cmds, taCmd)
		m.promptHistoryCursor = -1
		m.promptHistoryDraft = ""
	}

	return m, tea.Batch(cmds...)
}

func (m model) handleChatCommand(keyName string) (tea.Model, tea.Cmd, bool) {
	switch keyName {
	case "ctrl+d":
		if m.waiting {
			return m, nil, false
		}
		m.quitting = true
		return m, tea.Quit, true
	case keyCtrlC:
		return m.handleCtrlC()
	case "ctrl+o":
		return m.openModelPicker()
	case "ctrl+r":
		next, cmd := m.revampPrompt()
		return next, cmd, true
	case "ctrl+z":
		next, cmd := m.undoPromptRevamp()
		return next, cmd, true
	case "up":
		next, ok := m.navigatePromptHistory(1)
		return next, nil, ok
	case "down":
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
	if m.waiting {
		if m.cancel != nil {
			m.cancel()
			m.cancel = nil
		}
		m.waiting = false
		return m, tea.Println(errStyle.Render("(canceled)")), true
	}
	m.quitting = true
	return m, tea.Quit, true
}

func (m model) openModelPicker() (tea.Model, tea.Cmd, bool) {
	if m.waiting {
		return m, nil, false
	}
	m.pickerOpen = true
	m.pickerLoading = true
	m.pickerItems = nil
	m.pickerCursor = 0
	return m, loadModels(m.ctx, m.registry), true
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
		return m, nil, true
	}
	return m, nil, false
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

func (m model) navigatePromptHistory(delta int) (model, bool) {
	if m.waiting || len(m.promptHistory) == 0 {
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
	if m.waiting {
		return m, nil
	}
	input := strings.TrimSpace(m.textarea.Value())
	if input == "" {
		return m, nil
	}
	m.promptHistory = prependPromptHistory(input, m.promptHistory, maxPromptHistoryEntries)
	m.promptHistoryCursor = -1
	m.promptHistoryDraft = ""
	m.revampUndoActive = false
	m.revampUndo = ""
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

	// Print the user message above the input area.
	line := userLabel.Render("You") + " " + input
	if activeAgent.name != "" {
		line = userLabel.Render("You") + dimStyle.Render(" (@"+activeAgent.name+") ") + input
	}

	// Launch the LLM call.
	m.waiting = true
	ctx, cancel := context.WithCancel(nonNilContext(m.ctx))
	m.cancel = cancel
	msgs := make([]llm.Message, len(requestMessages))
	copy(msgs, requestMessages)
	requestModel, fallbackModels := requestModelAndFallbacks(m.selectedModel, m.modelLocked, m.fallbackModels, activeAgent)
	generation := generationForRequest(m.generationDefaults, m.generationOverrides, activeAgent)
	if err := validateRequestBudget(m.registry, requestModel, requestMessagesForBudget(requestModel, msgs, activeAgent, generation), m.maxInputTokens); err != nil {
		m.waiting = false
		m.cancel = nil
		cancel()
		return m, tea.Println(errStyle.Render("Error: " + err.Error()))
	}

	m.history = nextHistory
	m.sessionState.Messages = append([]llm.Message(nil), m.history...)
	m.sessionState.DefaultAgent = activeAgent.name
	if m.selectedModel != "" {
		m.sessionState.DefaultModel = m.selectedModel
	}
	request := llmRequest{
		agent:          activeAgent.agent,
		hasAgent:       activeAgent.ok,
		model:          requestModel,
		messages:       msgs,
		fallbackModels: fallbackModels,
		generation:     generation,
		maxInputTokens: m.maxInputTokens,
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
			emitFileRead(m.ctx, m.hookRunner, m.sessionState.ID, m.sessionPath, activeAgent.name, m.sessionState.DefaultModel, ref),
			emitContextAdd(m.ctx, m.hookRunner, m.sessionState.ID, m.sessionPath, activeAgent.name, m.sessionState.DefaultModel, ref),
		)
	}
	if activeAgent.ok {
		cmds = append(cmds, emitAgentExecute(m.ctx, m.hookRunner, m.sessionState.ID, m.sessionPath, activeAgent.name, requestModel))
	}
	cmds = append(cmds,
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
			emitHook(m.ctx, m.hookRunner, events.Event{
				Type:        events.Error,
				SessionID:   m.sessionState.ID,
				SessionPath: m.sessionPath,
				Agent:       m.sessionState.DefaultAgent,
				Model:       m.sessionState.DefaultModel,
				Error:       msg.err.Error(),
			}),
		)
	}

	m.tokenUsage.add(msg.tokenUsage)
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
		saveSession(m.ctx, m.sessionStore, m.sessionState, m.hookRunner),
		emitHook(m.ctx, m.hookRunner, events.Event{
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
		cmds = append(cmds, saveModelPreference(m.ctx, m.stateStore, m.cwd, m.selectedModel, scope, m.hookRunner))
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
	if m.completionOpen && len(m.completionItems) > 0 {
		status += "\n" + m.viewCompletions()
	}

	inputView := m.textarea.View()
	if suggestion, ok := m.promptSuggestion(); ok && !m.completionOpen {
		inputView += dimStyle.Render(suggestion.Suffix)
	}

	return status + "\n" + inputView
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

func (m model) promptSuggestion() (promptcomplete.Suggestion, bool) {
	value := m.textarea.Value()
	if strings.TrimSpace(value) == "" {
		return promptcomplete.Suggestion{}, false
	}
	cursor := textareaCursorOffset(m.textarea)
	if cursor != len(value) {
		return promptcomplete.Suggestion{}, false
	}
	suggestion, ok := promptcomplete.Suggest(promptcomplete.Context{
		Input:     value,
		Cursor:    cursor,
		Agents:    promptAgentCandidates(m.agentRegistry),
		Tools:     promptToolCandidates(),
		Templates: promptTemplateCandidates(),
	}, promptcomplete.Options{Limit: 1})
	if !ok || suggestion.Suffix == "" {
		return promptcomplete.Suggestion{}, false
	}
	return suggestion, true
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
	if usage.Responses > 0 {
		parts = append(parts, "responses="+strconv.Itoa(usage.Responses))
	}
	return strings.Join(parts, "\t")
}

func printTokenUsageSummary(w io.Writer, usage tokenUsage) {
	if usage.InputTokens == 0 && usage.CachedInputTokens == 0 && usage.OutputTokens == 0 {
		return
	}
	fmt.Fprintln(w, formatTokenUsageSummary(usage))
}

// ---------------------------------------------------------------------------
// Helpers
// ---------------------------------------------------------------------------

// loadModels fetches the model list from all registered providers.
func loadModels(ctx context.Context, reg *llm.Registry) tea.Cmd {
	ctx = nonNilContext(ctx)
	return func() tea.Msg {
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

func runFZFModelPicker(ctx context.Context, fzfPath string, items []pickerItem) tea.Cmd {
	var stdout bytes.Buffer
	input := fzfInput(items)
	ctx = nonNilContext(ctx)
	cmd := exec.CommandContext(
		ctx,
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

func completionCandidates(input string, agents *agent.Registry, root string, limit int) ([]completionCandidate, bool) {
	_, prefix, ok := activeAtToken(input)
	if !ok {
		return nil, false
	}
	if limit <= 0 {
		limit = 8
	}

	var out []completionCandidate
	prefixLower := strings.ToLower(prefix)
	if !strings.ContainsAny(prefix, `/\.`) {
		for _, name := range agents.List() {
			if strings.HasPrefix(strings.ToLower(name), prefixLower) {
				out = append(out, completionCandidate{
					kind:  "agent",
					label: "@" + name,
					value: "@" + name + " ",
				})
				if len(out) >= limit {
					return out, true
				}
			}
		}
	}

	fileCandidates := pathCompletionCandidates(root, prefix, limit-len(out))
	out = append(out, fileCandidates...)
	return out, true
}

func activeAtToken(input string) (start int, prefix string, ok bool) {
	if input == "" {
		return 0, "", false
	}
	end := len(input)
	start = end
	for start > 0 {
		r, size := lastRune(input[:start])
		if r == 0 || r == '\n' || r == '\t' || r == ' ' {
			break
		}
		start -= size
	}
	token := input[start:end]
	if !strings.HasPrefix(token, "@") {
		return 0, "", false
	}
	return start, strings.TrimPrefix(token, "@"), true
}

func lastRune(value string) (r rune, size int) {
	if value == "" {
		return 0, 0
	}
	r = rune(value[len(value)-1])
	if r < utf8.RuneSelf {
		return r, 1
	}
	r, size = utf8.DecodeLastRuneInString(value)
	return r, size
}

func pathCompletionCandidates(root, prefix string, limit int) []completionCandidate {
	if limit <= 0 || filepath.IsAbs(prefix) {
		return nil
	}
	if root == "" {
		var err error
		root, err = os.Getwd()
		if err != nil {
			return nil
		}
	}

	dirPart, base := pathCompletionParts(prefix)
	dir := filepath.Join(root, dirPart)
	if !pathInsideRoot(root, dir) {
		return nil
	}

	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].Name() < entries[j].Name() })

	out := make([]completionCandidate, 0, min(limit, len(entries)))
	for _, entry := range entries {
		name := entry.Name()
		if strings.HasPrefix(name, ".") && !strings.HasPrefix(base, ".") {
			continue
		}
		if base != "" && !strings.HasPrefix(strings.ToLower(name), strings.ToLower(base)) {
			continue
		}
		rel := name
		if dirPart != "." {
			rel = filepath.Join(dirPart, name)
		}
		value := "@" + filepath.ToSlash(rel)
		if entry.IsDir() {
			value += "/"
		}
		out = append(out, completionCandidate{
			kind:  "path",
			label: value,
			value: value,
		})
		if len(out) >= limit {
			break
		}
	}
	return out
}

func pathCompletionParts(prefix string) (dirPart, base string) {
	cleanPrefix := filepath.Clean(filepath.FromSlash(prefix))
	if cleanPrefix == "." {
		cleanPrefix = ""
	}
	dirPart = filepath.Dir(cleanPrefix)
	base = filepath.Base(cleanPrefix)
	if prefix == "" || !strings.ContainsAny(prefix, `/\`) {
		return ".", cleanPrefix
	}
	return dirPart, base
}

func pathInsideRoot(root, path string) bool {
	rootAbs, err := filepath.Abs(root)
	if err != nil {
		return false
	}
	pathAbs, err := filepath.Abs(path)
	if err != nil {
		return false
	}
	rel, err := filepath.Rel(rootAbs, pathAbs)
	if err != nil {
		return false
	}
	return rel == "." || (!strings.HasPrefix(rel, ".."+string(filepath.Separator)) && rel != ".." && !filepath.IsAbs(rel))
}

func applyCompletionCandidate(input, value string) string {
	start, _, ok := activeAtToken(input)
	if !ok {
		return input
	}
	return input[:start] + value
}

func applyPromptSuggestion(input string, suggestion promptcomplete.Suggestion) string {
	if suggestion.ReplacementStart < 0 ||
		suggestion.ReplacementEnd < suggestion.ReplacementStart ||
		suggestion.ReplacementEnd > len(input) {
		return input + suggestion.Suffix
	}
	return input[:suggestion.ReplacementStart] + suggestion.Text + input[suggestion.ReplacementEnd:]
}

func promptHistoryFromStore(store *session.Store, current session.Session, limit int) []string {
	if limit <= 0 {
		return nil
	}
	seen := make(map[string]bool)
	out := appendUserPromptsNewestFirst(nil, seen, current.Messages, limit)
	if len(out) >= limit || store == nil {
		return out
	}

	summaries, err := store.List()
	if err != nil {
		return out
	}
	for i := range summaries {
		summary := &summaries[i]
		if summary.ID == current.ID {
			continue
		}
		saved, err := store.Load(summary.ID)
		if err != nil {
			continue
		}
		out = appendUserPromptsNewestFirst(out, seen, saved.Messages, limit)
		if len(out) >= limit {
			break
		}
	}
	return out
}

func appendUserPromptsNewestFirst(out []string, seen map[string]bool, messages []llm.Message, limit int) []string {
	for i := len(messages) - 1; i >= 0 && len(out) < limit; i-- {
		if messages[i].Role != llm.RoleUser {
			continue
		}
		prompt := strings.TrimSpace(messages[i].Content)
		promptKey := normalizePromptHistoryKey(prompt)
		if promptKey == "" || seen[promptKey] {
			continue
		}
		seen[promptKey] = true
		out = append(out, prompt)
	}
	return out
}

func prependPromptHistory(prompt string, history []string, limit int) []string {
	if limit <= 0 {
		return nil
	}
	prompt = strings.TrimSpace(prompt)
	promptKey := normalizePromptHistoryKey(prompt)
	if promptKey == "" {
		return append([]string(nil), history...)
	}

	out := []string{prompt}
	for _, item := range history {
		if normalizePromptHistoryKey(item) == promptKey {
			continue
		}
		out = append(out, item)
		if len(out) >= limit {
			break
		}
	}
	return out
}

func normalizePromptHistoryKey(prompt string) string {
	return strings.ToLower(strings.Join(strings.Fields(prompt), " "))
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
		if err := validateRequestBudget(reg, params.Model, params.Messages, request.maxInputTokens); err != nil {
			return llmResponseMsg{err: err}
		}

		resp, err := reg.CompleteWithFallback(ctx, params, request.fallbackModels)
		if err != nil {
			return llmResponseMsg{err: err}
		}
		var usage tokenUsage
		usage.addResponse(resp)
		return llmResponseMsg{
			content:    resp.Content,
			model:      resp.Model,
			tokenUsage: usage,
		}
	}
}

func requestMessagesForBudget(
	modelName string,
	messages []llm.Message,
	activeAgent agentSelection,
	generation generationSettings,
) []llm.Message {
	params := llm.CompleteParams{
		Model:    modelName,
		Messages: messages,
	}
	if activeAgent.ok {
		params = activeAgent.agent.CompleteParams(modelName, messages)
	}
	applyGenerationParams(&params, generation)
	return params.Messages
}

func validateRequestBudget(reg *llm.Registry, modelName string, messages []llm.Message, maxInputTokens int) error {
	used := llm.EstimateTokens(messages)
	if maxInputTokens > 0 && used > maxInputTokens {
		return fmt.Errorf("estimated input tokens %s exceed configured max_input_tokens %s", formatTokenCount(used), formatTokenCount(maxInputTokens))
	}
	if reg == nil || modelName == "" {
		return nil
	}
	if limit := reg.ContextWindow(modelName); limit > 0 && used > limit {
		return fmt.Errorf("estimated input tokens %s exceed %s context window %s", formatTokenCount(used), modelName, formatTokenCount(limit))
	}
	return nil
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

func saveSession(ctx context.Context, store *session.Store, sessionState session.Session, runner *events.Runner) tea.Cmd {
	ctx = nonNilContext(ctx)
	return func() tea.Msg {
		if store == nil || sessionState.ID == "" {
			return sessionSavedMsg{}
		}
		if err := store.Save(sessionState); err != nil {
			return sessionSavedMsg{err: err}
		}
		emitHookWarning(ctx, runner, events.Event{
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
	ctx context.Context,
	store *appconfig.StateStore,
	cwd string,
	model string,
	scope appconfig.ModelScope,
	runner *events.Runner,
) tea.Cmd {
	ctx = nonNilContext(ctx)
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
		emitHookWarning(ctx, runner, events.Event{
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
	ctx context.Context,
	runner *events.Runner,
	sessionID, sessionPath, agentName, modelName string,
	ref contextref.Reference,
) tea.Cmd {
	return emitHook(ctx, runner, fileReadEvent(sessionID, sessionPath, agentName, modelName, ref))
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
	ctx context.Context,
	runner *events.Runner,
	sessionID, sessionPath, agentName, modelName string,
	ref contextref.Reference,
) tea.Cmd {
	return emitHook(ctx, runner, contextAddEvent(sessionID, sessionPath, agentName, modelName, ref))
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

func emitAgentExecute(ctx context.Context, runner *events.Runner, sessionID, sessionPath, agentName, modelName string) tea.Cmd {
	return emitHook(ctx, runner, events.Event{
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

func emitHook(ctx context.Context, runner *events.Runner, event events.Event) tea.Cmd {
	ctx = nonNilContext(ctx)
	return func() tea.Msg {
		if runner == nil {
			return hookMsg{}
		}
		line := events.FormatLine(event)
		return hookMsg{err: runner.Emit(ctx, event), line: line}
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

type nonNegativeIntFlag struct {
	name  string
	value int
	set   bool
}

func (f *nonNegativeIntFlag) Set(raw string) error {
	value, err := strconv.Atoi(raw)
	if err != nil {
		return fmt.Errorf("parse %s: %w", f.name, err)
	}
	if value < 0 {
		return fmt.Errorf("%s must be >= 0", f.name)
	}
	f.value = value
	f.set = true
	return nil
}

func (f *nonNegativeIntFlag) String() string {
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

type rawStringListFlag []string

func (f *rawStringListFlag) Set(raw string) error {
	raw = strings.TrimSpace(raw)
	if raw != "" {
		*f = append(*f, raw)
	}
	return nil
}

func (f *rawStringListFlag) String() string {
	if f == nil {
		return ""
	}
	return strings.Join(*f, ",")
}

type generationSettings struct {
	Temperature    *float64
	TopP           *float64
	Seed           *int
	ReasoningLevel string
	MaxTokens      int
}

type cliOptions struct {
	oncePrompt                         string
	agentName                          string
	sessionDir                         string
	sessionRef                         string
	showSessionRef                     string
	summarySessionRef                  string
	replayRef                          string
	exportRef                          string
	exportFormat                       string
	outputFormat                       string
	listSessionsTag                    string
	streamHeadlessID                   string
	searchQuery                        string
	initConfigPath                     string
	configPaths                        string
	contextPackPath                    string
	model                              string
	describeAgentName                  string
	codeSymbolName                     string
	codeSymbolFileSummary              string
	codeSymbolPackageSummary           string
	codeSymbolPrefix                   string
	codeSymbolPrefixFileSummary        string
	codeSymbolPrefixPackageSummary     string
	codeSymbolKind                     string
	codeSymbolKindFileSummary          string
	codeSymbolKindPackageSummary       string
	codeImpactTarget                   string
	codeImportPath                     string
	codeImportPathSummary              string
	codeImportPathFileSummary          string
	codeImportPathPackageSummary       string
	codeImportPrefix                   string
	codeImportPrefixSummary            string
	codeImportPrefixFileSummary        string
	codeImportPrefixPackageSummary     string
	codeReachTarget                    string
	codeDepsTarget                     string
	codeRdepsTarget                    string
	codePackageName                    string
	codePackageImports                 string
	codePackageImportPath              string
	codePackageImportFiles             string
	codePackageImportPathFileSummary   string
	codePackageImportPrefix            string
	codePackageImportPrefixFiles       string
	codePackageImportPrefixFileSummary string
	codePackageImportFileSummary       string
	codePackageSymbols                 string
	codePackageSymbolFileSummary       string
	codePackageSymbolName              string
	codePackageSymbolNameFileSummary   string
	codePackageSymbolList              string
	codePackageSymbolKind              string
	codePackageSymbolKindFileSummary   string
	codePackageSymbolPrefix            string
	codePackageSymbolPrefixFileSummary string
	codeFilePath                       string
	codeFileImports                    string
	codeFileSymbols                    string
	codeFileSymbolSummary              string
	codeFileSymbolName                 string
	codeFileSymbolKind                 string
	codeFileSymbolPrefix               string
	codeFileImportPrefix               string
	codeFileImportPath                 string
	sessionTitle                       string
	mergeWorktreeRef                   string
	recordFailure                      string
	failureReason                      string
	failureCommit                      string
	recordEvaluation                   string
	evaluationOutcome                  string
	evaluationNotes                    string
	evaluationReference                string
	planAgentsPrompt                   string
	evalOutputPath                     string
	evalExpected                       string
	evalExpectedPath                   string
	evalMode                           string
	gitHistorySearch                   string
	describePluginName                 string
	runPluginTarget                    string
	pluginEntrypoint                   string
	bashCommand                        string
	bashDir                            string
	feedbackApplyConfig                string
	feedbackHistoryPath                string
	agentMemoryAgent                   string
	agentMemorySearch                  string
	agentMemoryStorePath               string
	memorySearch                       string
	memoryStorePath                    string
	mcpManifestPath                    string
	mcpCapability                      string
	mcpServerName                      string
	mcpMethod                          string
	mcpParamsJSON                      string
	mcpToolName                        string
	mcpToolArgsJSON                    string
	lspCommand                         string
	lspFilePath                        string
	lspRootPath                        string
	lspLanguageID                      string
	lspWorkspaceSymbols                string
	spawnBinary                        string
	promptCompleteInput                string
	speculatePrompt                    string
	skillSaveDir                       string
	asyncTaskSpecs                     stringListFlag
	reviewAgents                       stringListFlag
	reviewPaths                        stringListFlag
	reviewGates                        stringListFlag
	vectorSearch                       string
	mergeArtifactsPath                 string
	recordArtifact                     string
	artifactKind                       string
	artifactSummary                    string
	recordResponsePath                 string
	replayResponsePath                 string
	sessionTags                        stringListFlag
	agentMemoryIndexFiles              stringListFlag
	planAgentNames                     stringListFlag
	suggestSkillSteps                  stringListFlag
	routeCandidates                    rawStringListFlag
	lspArgs                            rawStringListFlag
	spawnAgentSpecs                    rawStringListFlag
	speculateAgents                    stringListFlag
	speculateGates                     stringListFlag
	memoryIndexFiles                   stringListFlag
	vectorIndexFiles                   stringListFlag
	maxTokens                          positiveIntFlag
	maxInputTokens                     positiveIntFlag
	contextPackTokens                  positiveIntFlag
	planMaxAgents                      positiveIntFlag
	memoryLimit                        positiveIntFlag
	agentMemoryLimit                   positiveIntFlag
	vectorLimit                        positiveIntFlag
	mergeArtifactMaxBytes              positiveIntFlag
	routeInputTokens                   positiveIntFlag
	routeOutputTokens                  positiveIntFlag
	gitHistoryLimit                    positiveIntFlag
	pluginTimeout                      positiveIntFlag
	bashTimeout                        positiveIntFlag
	mcpTimeout                         positiveIntFlag
	spawnTimeout                       positiveIntFlag
	promptCompleteLimit                positiveIntFlag
	watchLargeFileBytes                positiveIntFlag
	watchIntervalSeconds               positiveIntFlag
	watchMaxIterations                 positiveIntFlag
	skillMaxSteps                      positiveIntFlag
	skillMinOccurrences                positiveIntFlag
	evaluationScore                    nonNegativeIntFlag
	seed                               nonNegativeIntFlag
	reasoningLevel                     string
	temperature                        floatFlag
	routeBudget                        floatFlag
	routeCacheReuse                    floatFlag
	topP                               floatFlag
	listModels                         bool
	listKnownModels                    bool
	listProviders                      bool
	speculatePlan                      bool
	reviewPlan                         bool
	routeInteractive                   bool
	routeBatch                         bool
	listAgents                         bool
	listCodeImports                    bool
	listCodeImportSummary              bool
	listCodeImportFileSummary          bool
	listCodeLayers                     bool
	listCodeCycles                     bool
	codeSummary                        bool
	listCodeFiles                      bool
	listCodeSymbolSummary              bool
	listCodeSymbolFileSummary          bool
	listCodePackageImportSummary       bool
	listCodePackages                   bool
	listSessions                       bool
	listHeadless                       bool
	listSessionTags                    bool
	agentPerformanceSummary            bool
	listArtifacts                      bool
	listEvaluations                    bool
	listFailures                       bool
	listMessages                       bool
	listConfigPaths                    bool
	listPlugins                        bool
	listHookEvents                     bool
	listHookEventsJSON                 bool
	watchScan                          bool
	watchJSON                          bool
	watchLoop                          bool
	reviewScan                         bool
	lspSymbols                         bool
	asyncPlan                          bool
	spawnDryRun                        bool
	feedbackProposals                  bool
	validateConfig                     bool
	printConfigTemplate                bool
	doctor                             bool
	doctorOffline                      bool
	readStdin                          bool
	headless                           bool
	showVersion                        bool
	useWorktree                        bool
	pluginDryRun                       bool
	listWorktrees                      bool
	noAutoMerge                        bool
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
	pluginPaths         []string
	providers           []string
	loadedConfigPaths   []string
	selectedModel       string
	selectedAgent       string
	cwd                 string
	maxInputTokens      int
	modelLocked         bool
	autoMergeWorktree   bool
}

func parseOptions() cliOptions {
	var opts cliOptions
	opts.temperature = floatFlag{name: "temperature", min: 0}
	opts.topP = floatFlag{name: "top-p", min: 0, max: 1, hasMax: true}
	opts.routeBudget = floatFlag{name: "route-budget", min: 0}
	opts.routeCacheReuse = floatFlag{name: "route-cache-reuse", min: 0, max: 1, hasMax: true}
	opts.maxTokens = positiveIntFlag{name: "max-tokens"}
	opts.maxInputTokens = positiveIntFlag{name: "max-input-tokens"}
	opts.seed = nonNegativeIntFlag{name: "seed"}
	opts.mcpTimeout = positiveIntFlag{name: "mcp-timeout-seconds"}
	opts.spawnTimeout = positiveIntFlag{name: "spawn-timeout-seconds"}
	flag.StringVar(&opts.configPaths, "config", "", "additional YAML/JSON config file path(s); same format as ATTELER_CONFIG")
	flag.StringVar(&opts.contextPackPath, "context-pack-file", "", "compact a role-prefixed transcript file and exit")
	flag.StringVar(&opts.initConfigPath, "init-config", "", "write a starter YAML config to this path without overwriting")
	flag.StringVar(&opts.sessionDir, "session-dir", "", "directory for session JSON files")
	flag.StringVar(&opts.sessionRef, "session", "", "session ID or path to continue")
	flag.StringVar(&opts.showSessionRef, "show-session", "", "print saved session details as YAML and exit")
	flag.StringVar(&opts.summarySessionRef, "session-summary", "", "print compact saved session metadata and counts and exit")
	flag.StringVar(&opts.sessionTitle, "session-title", "", "set or update the saved session title")
	flag.Var(&opts.sessionTags, "session-tag", "add a saved session tag (repeatable or comma-separated)")
	flag.StringVar(&opts.replayRef, "replay", "", "session ID or path to print and exit")
	flag.StringVar(&opts.exportRef, "export-session", "", "session ID or path to export and exit")
	flag.StringVar(&opts.exportFormat, "export-format", "markdown", "session export format: markdown or json")
	flag.StringVar(&opts.outputFormat, "output", outputFormatText, "one-shot output format: text or json")
	flag.StringVar(&opts.searchQuery, "search-sessions", "", "search saved session transcripts and exit")
	flag.StringVar(&opts.oncePrompt, "once", "", "send one prompt and exit")
	flag.StringVar(&opts.model, "model", "", "model ID to use")
	flag.StringVar(&opts.agentName, "agent", "", "agent name to use for prompts")
	flag.StringVar(&opts.describeAgentName, "describe-agent", "", "print a configured agent as YAML and exit")
	flag.StringVar(&opts.codeSymbolName, "code-symbol", "", "find Go symbols by exact name in the current repository and exit")
	flag.StringVar(&opts.codeSymbolFileSummary, "code-symbol-name-file-summary", "", "list Go files with symbol counts for this exact name and exit")
	flag.StringVar(&opts.codeSymbolPackageSummary, "code-symbol-name-package-summary", "", "list Go packages with file and symbol counts for this exact name and exit")
	flag.StringVar(&opts.codeSymbolPrefix, "code-symbol-prefix", "", "find Go symbols by name prefix in the current repository and exit")
	flag.StringVar(&opts.codeSymbolPrefixFileSummary, "code-symbol-prefix-file-summary", "", "list Go files with symbol counts for names matching this prefix and exit")
	flag.StringVar(&opts.codeSymbolPrefixPackageSummary, "code-symbol-prefix-package-summary", "", "list Go packages with file and symbol counts for names matching this prefix and exit")
	flag.StringVar(&opts.codeSymbolKind, "code-symbol-kind", "", "list Go symbols by kind (func, method, type, const, var) and exit")
	flag.StringVar(&opts.codeSymbolKindFileSummary, "code-symbol-kind-file-summary", "", "list Go files with symbol counts for one kind and exit")
	flag.StringVar(&opts.codeSymbolKindPackageSummary, "code-symbol-kind-package-summary", "", "list Go packages with file and symbol counts for one kind and exit")
	flag.StringVar(&opts.codeImpactTarget, "code-impact", "", "list Go files that directly or transitively import this path and exit")
	flag.StringVar(&opts.codeImportPath, "code-import-path", "", "list Go files that directly import this import path and exit")
	flag.StringVar(&opts.codeImportPathSummary, "code-import-path-summary", "", "summarize file usage count for one exact import path and exit")
	flag.StringVar(&opts.codeImportPathFileSummary, "code-import-path-file-summary", "", "list Go files with import counts for one exact import path and exit")
	flag.StringVar(&opts.codeImportPathPackageSummary, "code-import-path-package-summary", "", "list Go packages with file counts for one exact import path and exit")
	flag.StringVar(&opts.codeImportPrefix, "code-import-prefix", "", "list Go files that directly import paths with this prefix and exit")
	flag.StringVar(&opts.codeImportPrefixSummary, "code-import-prefix-summary", "", "summarize import usage counts for paths with this prefix and exit")
	flag.StringVar(&opts.codeImportPrefixFileSummary, "code-import-prefix-file-summary", "", "list Go files with import counts for paths matching this prefix and exit")
	flag.StringVar(&opts.codeImportPrefixPackageSummary, "code-import-prefix-package-summary", "", "list Go packages with file and import counts for paths matching this prefix and exit")
	flag.StringVar(&opts.codeReachTarget, "code-reachable", "", "list Go import graph nodes reachable from this file path or import path and exit")
	flag.StringVar(&opts.codeDepsTarget, "code-deps", "", "list direct Go import graph dependencies for this file path or import path and exit")
	flag.StringVar(&opts.codeRdepsTarget, "code-rdeps", "", "list direct Go import graph reverse dependencies for this file path or import path and exit")
	flag.StringVar(&opts.codePackageName, "code-package", "", "list Go files and symbol counts for one package and exit")
	flag.StringVar(&opts.codePackageImports, "code-package-imports", "", "list import usage counts for one Go package and exit")
	flag.StringVar(&opts.codePackageImportPath, "code-package-import-path", "", "list exact import usage for one package as package:import and exit")
	flag.StringVar(&opts.codePackageImportFiles, "code-package-import-files", "", "list files in one package importing an exact path as package:import and exit")
	flag.StringVar(&opts.codePackageImportPathFileSummary, "code-package-import-path-file-summary", "", "list files in one package with import counts for package:import and exit")
	flag.StringVar(&opts.codePackageImportPrefix, "code-package-import-prefix", "", "list import usage for one package and import prefix as package:prefix and exit")
	flag.StringVar(&opts.codePackageImportPrefixFiles, "code-package-import-prefix-files", "", "list files in one package importing paths with prefix as package:prefix and exit")
	flag.StringVar(&opts.codePackageImportPrefixFileSummary, "code-package-import-prefix-file-summary", "", "list files in one package with import counts for paths matching package:prefix and exit")
	flag.StringVar(&opts.codePackageImportFileSummary, "code-package-import-file-summary", "", "list files in one Go package with import counts and exit")
	flag.StringVar(&opts.codePackageSymbols, "code-package-symbols", "", "list symbol kind counts for one Go package and exit")
	flag.StringVar(&opts.codePackageSymbolFileSummary, "code-package-symbol-file-summary", "", "list files in one Go package with symbol counts and exit")
	flag.StringVar(&opts.codePackageSymbolName, "code-package-symbol", "", "list Go symbols for one package and exact name as package:name and exit")
	flag.StringVar(&opts.codePackageSymbolNameFileSummary, "code-package-symbol-name-file-summary", "", "list files in one Go package with symbol counts for package:name and exit")
	flag.StringVar(&opts.codePackageSymbolList, "code-package-symbol-list", "", "list Go symbols declared in one package and exit")
	flag.StringVar(&opts.codePackageSymbolKind, "code-package-symbol-kind", "", "list Go symbols for one package and kind as package:kind and exit")
	flag.StringVar(&opts.codePackageSymbolKindFileSummary, "code-package-symbol-kind-file-summary", "", "list files in one Go package with symbol counts for package:kind and exit")
	flag.StringVar(&opts.codePackageSymbolPrefix, "code-package-symbol-prefix", "", "list Go symbols for one package and name prefix as package:prefix and exit")
	flag.StringVar(&opts.codePackageSymbolPrefixFileSummary, "code-package-symbol-prefix-file-summary", "", "list files in one Go package with symbol counts for package:prefix and exit")
	flag.StringVar(&opts.codeFilePath, "code-file", "", "print Go package, symbols, and imports for one file and exit")
	flag.StringVar(&opts.codeFileImports, "code-file-imports", "", "list imports for one Go file and exit")
	flag.StringVar(&opts.codeFileSymbols, "code-file-symbols", "", "list symbols for one Go file and exit")
	flag.StringVar(&opts.codeFileSymbolSummary, "code-file-symbol-summary", "", "list symbol kind counts for one Go file and exit")
	flag.StringVar(&opts.codeFileSymbolName, "code-file-symbol", "", "list Go symbols for one file and exact name as path:name and exit")
	flag.StringVar(&opts.codeFileSymbolKind, "code-file-symbol-kind", "", "list Go symbols for one file and kind as path:kind and exit")
	flag.StringVar(&opts.codeFileSymbolPrefix, "code-file-symbol-prefix", "", "list Go symbols for one file and name prefix as path:prefix and exit")
	flag.StringVar(&opts.codeFileImportPrefix, "code-file-import-prefix", "", "list imports for one Go file matching path:prefix and exit")
	flag.StringVar(&opts.codeFileImportPath, "code-file-import-path", "", "check/list one Go file import as path:import and exit")
	flag.StringVar(&opts.recordFailure, "record-failure", "", "record a failed approach/negative-knowledge note on the selected session and exit")
	flag.StringVar(&opts.failureReason, "failure-reason", "", "reason for --record-failure")
	flag.StringVar(&opts.failureCommit, "failure-commit", "", "commit or reference associated with --record-failure")
	flag.StringVar(&opts.recordEvaluation, "record-evaluation", "", "record an evaluation for this agent on the selected session and exit")
	flag.StringVar(&opts.evaluationOutcome, "evaluation-outcome", "", "outcome for --record-evaluation")
	flag.StringVar(&opts.evaluationNotes, "evaluation-notes", "", "notes for --record-evaluation")
	flag.StringVar(&opts.evaluationReference, "evaluation-reference", "", "reference for --record-evaluation")
	flag.StringVar(&opts.planAgentsPrompt, "plan-agents", "", "plan configured agents for this prompt and exit")
	flag.Var(&opts.planAgentNames, "plan-agent", "explicit agent name to include in --plan-agents (repeatable or comma-separated)")
	flag.Var(&opts.planMaxAgents, "plan-max-agents", "maximum agents to include in --plan-agents")
	flag.StringVar(&opts.evalOutputPath, "eval-output", "", "actual output file to compare and exit")
	flag.StringVar(&opts.evalExpected, "eval-expected", "", "expected text for --eval-output")
	flag.StringVar(&opts.evalExpectedPath, "eval-expected-file", "", "expected output file for --eval-output")
	flag.StringVar(&opts.evalMode, "eval-mode", string(atteval.ModeContains), "eval mode: exact, contains, or normalized")
	flag.StringVar(&opts.gitHistorySearch, "git-history-search", "", "search local git history subjects/files/authors and exit")
	flag.Var(&opts.gitHistoryLimit, "git-history-limit", "maximum --git-history-search results")
	flag.StringVar(&opts.describePluginName, "describe-plugin", "", "print a configured plugin manifest as YAML and exit")
	flag.StringVar(&opts.runPluginTarget, "run-plugin", "", "run configured plugin name, or plugin/entrypoint when --plugin-entrypoint is omitted")
	flag.StringVar(&opts.pluginEntrypoint, "plugin-entrypoint", "", "entrypoint name for --run-plugin")
	flag.Var(&opts.pluginTimeout, "plugin-timeout-seconds", "timeout in seconds for --run-plugin")
	flag.BoolVar(&opts.pluginDryRun, "plugin-dry-run", false, "describe --run-plugin without executing it")
	flag.StringVar(&opts.bashCommand, "bash", "", "run an explicit local bash command and exit")
	flag.StringVar(&opts.bashDir, "bash-dir", "", "working directory for --bash")
	flag.Var(&opts.bashTimeout, "bash-timeout-seconds", "timeout in seconds for --bash")
	flag.StringVar(&opts.memorySearch, "memory-search", "", "search local memory built from sessions, --memory-store, and --memory-index files")
	flag.StringVar(&opts.memoryStorePath, "memory-store", "", "JSON memory store path to load and/or save")
	flag.StringVar(&opts.mcpManifestPath, "mcp-manifest", "", "validate/list or invoke an MCP manifest YAML/JSON file and exit")
	flag.StringVar(&opts.mcpCapability, "mcp-capability", "", "find servers declaring this capability in --mcp-manifest")
	flag.StringVar(&opts.mcpServerName, "mcp-server", "", "server name in --mcp-manifest for --mcp-method or --mcp-tool")
	flag.StringVar(&opts.mcpMethod, "mcp-method", "", "invoke this JSON-RPC method on --mcp-server")
	flag.StringVar(&opts.mcpParamsJSON, "mcp-params", "", "JSON params for --mcp-method")
	flag.StringVar(&opts.mcpToolName, "mcp-tool", "", "invoke this MCP tool through tools/call on --mcp-server")
	flag.StringVar(&opts.mcpToolArgsJSON, "mcp-tool-args", "", "JSON object arguments for --mcp-tool")
	flag.Var(&opts.mcpTimeout, "mcp-timeout-seconds", "timeout in seconds for --mcp-method or --mcp-tool")
	flag.Var(&opts.memoryIndexFiles, "memory-index", "file to add to memory before saving/searching (repeatable or comma-separated)")
	flag.Var(&opts.memoryLimit, "memory-limit", "maximum memory search results")
	flag.StringVar(&opts.agentMemoryStorePath, "agent-memory-store", "", "JSON store for per-agent vector memory")
	flag.StringVar(&opts.agentMemoryAgent, "agent-memory-agent", "", "agent namespace for per-agent vector memory; defaults to --agent")
	flag.StringVar(&opts.agentMemorySearch, "agent-memory-search", "", "search one agent's vector memory and exit")
	flag.Var(&opts.agentMemoryIndexFiles, "agent-memory-index", "file to add to one agent's vector memory (repeatable or comma-separated)")
	flag.Var(&opts.agentMemoryLimit, "agent-memory-limit", "maximum per-agent memory search results")
	flag.StringVar(&opts.vectorSearch, "vector-search", "", "search --vector-index files with dependency-free local vector retrieval and exit")
	flag.Var(&opts.vectorIndexFiles, "vector-index", "file to add to vector search (repeatable or comma-separated)")
	flag.Var(&opts.vectorLimit, "vector-limit", "maximum vector search results")
	flag.BoolVar(&opts.lspSymbols, "lsp-symbols", false, "request document symbols from an external language server and exit")
	flag.StringVar(&opts.lspWorkspaceSymbols, "lsp-workspace-symbols", "", "query workspace symbols from an external language server and exit")
	flag.StringVar(&opts.lspCommand, "lsp-command", "", "language server command for --lsp-symbols or --lsp-workspace-symbols")
	flag.Var(&opts.lspArgs, "lsp-arg", "language server argument for LSP commands (repeatable)")
	flag.StringVar(&opts.lspFilePath, "lsp-file", "", "source file to inspect with --lsp-symbols")
	flag.StringVar(&opts.lspRootPath, "lsp-root", "", "workspace root for --lsp-symbols")
	flag.StringVar(&opts.lspLanguageID, "lsp-language", "", "language ID for --lsp-symbols; inferred from --lsp-file when omitted")
	flag.StringVar(&opts.promptCompleteInput, "prompt-complete", "", "suggest deterministic rest-of-line prompt completions and exit")
	flag.Var(&opts.promptCompleteLimit, "prompt-complete-limit", "maximum --prompt-complete suggestions")
	flag.BoolVar(&opts.asyncPlan, "async-plan", false, "print dependency-aware async task batches and exit")
	flag.Var(&opts.asyncTaskSpecs, "async-task", "task spec for --async-plan: id|agent|prompt|dep1+dep2 (repeatable or comma-separated)")
	flag.Var(&opts.spawnAgentSpecs, "spawn-agent", "spawn sub-agent spec: id|agent|prompt or agent|prompt (repeatable)")
	flag.BoolVar(&opts.spawnDryRun, "spawn-dry-run", false, "print --spawn-agent invocations without executing them")
	flag.StringVar(&opts.spawnBinary, "spawn-binary", "", "atteler binary for --spawn-agent; defaults to the current executable")
	flag.Var(&opts.spawnTimeout, "spawn-timeout-seconds", "overall timeout in seconds for --spawn-agent")
	flag.Var(&opts.suggestSkillSteps, "skill-step", "observed action for skill suggestion (repeatable or comma-separated)")
	flag.StringVar(&opts.skillSaveDir, "skill-save-dir", "", "persist accepted --skill-step suggestion to this directory")
	flag.BoolVar(&opts.speculatePlan, "speculate-plan", false, "print a speculative three-round execution plan and exit")
	flag.StringVar(&opts.speculatePrompt, "speculate-prompt", "", "base task prompt for --speculate-plan prompt-cache reuse estimates")
	flag.Var(&opts.routeCandidates, "route-candidate", "model route candidate spec: provider/model,key=value... (repeatable or comma-separated)")
	flag.Var(&opts.routeInputTokens, "route-input-tokens", "estimated input tokens for model routing")
	flag.Var(&opts.routeOutputTokens, "route-output-tokens", "estimated output tokens for model routing")
	flag.Var(&opts.routeBudget, "route-budget", "maximum estimated request cost for model routing")
	flag.Var(&opts.routeCacheReuse, "route-cache-reuse", "prompt-cache reuse estimate for model routing (0..1)")
	flag.BoolVar(&opts.routeInteractive, "route-interactive", false, "rank model route candidates for low TTFT")
	flag.BoolVar(&opts.routeBatch, "route-batch", false, "rank model route candidates for batch/cost preference")
	flag.Var(&opts.speculateAgents, "speculate-agent", "agent name for --speculate-plan (repeatable or comma-separated)")
	flag.Var(&opts.speculateGates, "speculate-gate", "required gate check for --speculate-plan (repeatable or comma-separated)")
	flag.Var(&opts.reviewAgents, "review-agent", "reviewer name for --review-plan (repeatable or comma-separated)")
	flag.Var(&opts.reviewPaths, "review-path", "path for --review-plan review surface (repeatable or comma-separated)")
	flag.Var(&opts.reviewGates, "review-gate", "required gate check for --review-plan (repeatable or comma-separated)")
	flag.Var(&opts.skillMaxSteps, "skill-max-steps", "maximum repeated sequence length for --skill-step suggestions")
	flag.Var(&opts.skillMinOccurrences, "skill-min-occurrences", "minimum repeated occurrences for --skill-step suggestions")
	flag.StringVar(&opts.recordArtifact, "record-artifact", "", "record a session artifact path and exit")
	flag.StringVar(&opts.artifactKind, "artifact-kind", "", "kind for --record-artifact")
	flag.StringVar(&opts.artifactSummary, "artifact-summary", "", "summary for --record-artifact")
	flag.StringVar(&opts.mergeArtifactsPath, "merge-artifacts", "", "write selected-session text artifacts as merged Markdown; use '-' for stdout")
	flag.Var(&opts.mergeArtifactMaxBytes, "merge-artifact-max-bytes", "maximum bytes to read from each --merge-artifacts input")
	flag.StringVar(&opts.recordResponsePath, "record-response", "", "record a one-shot response to this JSON file")
	flag.StringVar(&opts.replayResponsePath, "replay-response", "", "replay a recorded one-shot response JSON file without calling an LLM")
	flag.Var(&opts.temperature, "temperature", "override request temperature")
	flag.Var(&opts.topP, "top-p", "override request nucleus sampling value (0..1)")
	flag.Var(&opts.maxTokens, "max-tokens", "override request max output tokens")
	flag.Var(&opts.seed, "seed", "best-effort deterministic seed for providers that support it")
	flag.StringVar(&opts.reasoningLevel, "reasoning-level", "", "override request reasoning level/effort")
	flag.Var(&opts.maxInputTokens, "max-input-tokens", "hard cap on estimated input tokens before an LLM call")
	flag.Var(&opts.contextPackTokens, "context-pack-tokens", "maximum estimated tokens for --context-pack-file")
	flag.Var(&opts.evaluationScore, "evaluation-score", "score for --record-evaluation")
	flag.BoolVar(&opts.listModels, "list-models", false, "list available models and exit")
	flag.BoolVar(&opts.listKnownModels, "list-known-models", false, "list built-in provider/model IDs without API calls and exit")
	flag.BoolVar(&opts.listProviders, "list-providers", false, "list built-in provider names without API calls and exit")
	flag.BoolVar(&opts.listAgents, "list-agents", false, "list configured agents and exit")
	flag.BoolVar(&opts.listCodeImports, "code-imports", false, "list Go import edges in the current repository and exit")
	flag.BoolVar(&opts.listCodeImportSummary, "code-import-summary", false, "list Go import paths with usage counts and exit")
	flag.BoolVar(&opts.listCodeImportFileSummary, "code-import-file-summary", false, "list Go files with import counts and exit")
	flag.BoolVar(&opts.listCodeLayers, "code-layers", false, "list topological Go import graph layers for the current repository and exit")
	flag.BoolVar(&opts.listCodeCycles, "code-cycles", false, "list Go import graph cycles for the current repository and exit")
	flag.BoolVar(&opts.codeSummary, "code-summary", false, "print compact Go code index and import graph counts and exit")
	flag.BoolVar(&opts.listCodeFiles, "code-files", false, "list Go files with package, import, and symbol counts and exit")
	flag.BoolVar(&opts.listCodeSymbolSummary, "code-symbol-summary", false, "list Go symbol kinds with counts and exit")
	flag.BoolVar(&opts.listCodeSymbolFileSummary, "code-symbol-file-summary", false, "list Go files with symbol counts and exit")
	flag.BoolVar(&opts.listCodePackages, "code-packages", false, "list Go packages with file and symbol counts and exit")
	flag.BoolVar(&opts.listCodePackageImportSummary, "code-package-import-summary", false, "list Go packages with import counts and exit")
	flag.BoolVar(&opts.listSessions, "list-sessions", false, "list saved sessions and exit")
	flag.BoolVar(&opts.listHeadless, "list-headless", false, "list active headless sessions and exit")
	flag.StringVar(&opts.streamHeadlessID, "stream-headless", "", "stream one headless session log by headless ID and exit when it finishes")
	flag.BoolVar(&opts.listSessionTags, "list-session-tags", false, "list saved session tags with counts and exit")
	flag.BoolVar(&opts.agentPerformanceSummary, "agent-performance-summary", false, "summarize recorded agent performance across saved sessions and exit")
	flag.BoolVar(&opts.listArtifacts, "list-artifacts", false, "list artifacts recorded on the selected session and exit")
	flag.BoolVar(&opts.listEvaluations, "list-evaluations", false, "list agent evaluations recorded on the selected session and exit")
	flag.BoolVar(&opts.listFailures, "list-failures", false, "list negative-knowledge records on the selected session and exit")
	flag.BoolVar(&opts.listMessages, "list-messages", false, "list compact message records on the selected session and exit")
	flag.StringVar(&opts.listSessionsTag, "list-sessions-tag", "", "filter --list-sessions to sessions containing this exact tag")
	flag.BoolVar(&opts.listConfigPaths, "list-config-paths", false, "list config files in load order and exit")
	flag.BoolVar(&opts.listPlugins, "list-plugins", false, "list configured local plugin manifests and exit")
	flag.BoolVar(&opts.listHookEvents, "list-hook-events", false, "list supported lifecycle hook event types and exit")
	flag.BoolVar(&opts.listHookEventsJSON, "list-hook-events-json", false, "list supported lifecycle hook event types as JSON and exit")
	flag.BoolVar(&opts.watchScan, "watch-scan", false, "scan the current repository for background-agent health findings and exit")
	flag.BoolVar(&opts.watchJSON, "watch-json", false, "emit --watch-scan findings as JSON")
	flag.BoolVar(&opts.watchLoop, "watch-loop", false, "continuously scan the current repository for background-agent health findings")
	flag.Var(&opts.watchIntervalSeconds, "watch-interval-seconds", "seconds between --watch-loop scans")
	flag.Var(&opts.watchMaxIterations, "watch-max-iterations", "maximum --watch-loop scans before exiting; omit to run until interrupted")
	flag.BoolVar(&opts.reviewPlan, "review-plan", false, "print speculative review-agent plan and exit")
	flag.BoolVar(&opts.reviewScan, "review-scan", false, "scan the current repository and print a structured review report and exit")
	flag.BoolVar(&opts.feedbackProposals, "feedback-proposals", false, "derive agent improvement proposals from the selected session and exit")
	flag.StringVar(&opts.feedbackApplyConfig, "feedback-apply-config", "", "apply feedback proposals from the selected session to this agent config file")
	flag.StringVar(&opts.feedbackHistoryPath, "feedback-history", "", "append --feedback-apply-config decisions to this history log")
	flag.Var(&opts.watchLargeFileBytes, "watch-large-file-bytes", "large-file byte threshold for --watch-scan")
	flag.BoolVar(&opts.validateConfig, "validate-config", false, "validate merged YAML/JSON config and exit")
	flag.BoolVar(&opts.printConfigTemplate, "print-config-template", false, "print a starter YAML config and exit")
	flag.BoolVar(&opts.doctor, "doctor", false, "print local readiness diagnostics and exit")
	flag.BoolVar(&opts.doctorOffline, "doctor-offline", false, "print offline readiness diagnostics without provider health checks and exit")
	flag.BoolVar(&opts.readStdin, "stdin", false, "append stdin to a one-shot prompt")
	flag.BoolVar(&opts.headless, "headless", false, "run one-shot prompt without TUI output while recording headless metadata and logs")
	flag.BoolVar(&opts.showVersion, "version", false, "print version and exit")
	flag.BoolVar(&opts.useWorktree, "worktree", false, "isolate session in a git worktree")
	flag.BoolVar(&opts.listWorktrees, "list-worktrees", false, "list active atteler worktrees and exit")
	flag.BoolVar(&opts.noAutoMerge, "no-auto-merge", false, "keep worktree alive on exit instead of auto-merging")
	flag.StringVar(&opts.mergeWorktreeRef, "merge-worktree", "", "merge a session worktree back into its base branch and exit")
	flag.Parse()

	if opts.oncePrompt == "" && flag.NArg() > 0 {
		opts.oncePrompt = strings.Join(flag.Args(), " ")
	}

	applyDebugEnvOptions(&opts, os.Getenv)

	return opts
}

func applyDebugEnvOptions(opts *cliOptions, getenv func(string) string) {
	if opts == nil || getenv == nil {
		return
	}

	applyDebugBool(getenv, "DEBUG_ATTELER_DOCTOR", &opts.doctor)
	applyDebugBool(getenv, "DEBUG_ATTELER_DOCTOR_OFFLINE", &opts.doctorOffline)
	applyDebugBool(getenv, "DEBUG_ATTELER_VALIDATE_CONFIG", &opts.validateConfig)
	applyDebugBool(getenv, "DEBUG_ATTELER_LIST_CONFIG_PATHS", &opts.listConfigPaths)
	applyDebugBool(getenv, "DEBUG_ATTELER_LIST_PROVIDERS", &opts.listProviders)
	applyDebugBool(getenv, "DEBUG_ATTELER_LIST_KNOWN_MODELS", &opts.listKnownModels)
	applyDebugBool(getenv, "DEBUG_ATTELER_LIST_MODELS", &opts.listModels)
	applyDebugBool(getenv, "DEBUG_ATTELER_LIST_AGENTS", &opts.listAgents)
	applyDebugBool(getenv, "DEBUG_ATTELER_LIST_PLUGINS", &opts.listPlugins)
	applyDebugBool(getenv, "DEBUG_ATTELER_LIST_HOOK_EVENTS", &opts.listHookEvents)
	applyDebugBool(getenv, "DEBUG_ATTELER_LIST_HOOK_EVENTS_JSON", &opts.listHookEventsJSON)
	applyDebugBool(getenv, "DEBUG_ATTELER_CODE_SUMMARY", &opts.codeSummary)
	applyDebugBool(getenv, "DEBUG_ATTELER_CODE_FILES", &opts.listCodeFiles)
	applyDebugBool(getenv, "DEBUG_ATTELER_CODE_IMPORTS", &opts.listCodeImports)
	applyDebugBool(getenv, "DEBUG_ATTELER_CODE_IMPORT_SUMMARY", &opts.listCodeImportSummary)
	applyDebugBool(getenv, "DEBUG_ATTELER_CODE_IMPORT_FILE_SUMMARY", &opts.listCodeImportFileSummary)
	applyDebugBool(getenv, "DEBUG_ATTELER_CODE_LAYERS", &opts.listCodeLayers)
	applyDebugBool(getenv, "DEBUG_ATTELER_CODE_CYCLES", &opts.listCodeCycles)
	applyDebugBool(getenv, "DEBUG_ATTELER_REVIEW_PLAN", &opts.reviewPlan)
	applyDebugBool(getenv, "DEBUG_ATTELER_REVIEW_SCAN", &opts.reviewScan)
	applyDebugBool(getenv, "DEBUG_ATTELER_AGENT_PERFORMANCE_SUMMARY", &opts.agentPerformanceSummary)
	applyDebugBool(getenv, "DEBUG_ATTELER_WATCH_SCAN", &opts.watchScan)
	applyDebugBool(getenv, "DEBUG_ATTELER_WATCH_JSON", &opts.watchJSON)
	applyDebugBool(getenv, "DEBUG_ATTELER_WATCH_LOOP", &opts.watchLoop)
	applyDebugBool(getenv, "DEBUG_ATTELER_LSP_SYMBOLS", &opts.lspSymbols)

	applyDebugString(getenv, "DEBUG_ATTELER_MCP_MANIFEST", &opts.mcpManifestPath)
	applyDebugString(getenv, "DEBUG_ATTELER_MCP_CAPABILITY", &opts.mcpCapability)
	applyDebugString(getenv, "DEBUG_ATTELER_MCP_SERVER", &opts.mcpServerName)
	applyDebugString(getenv, "DEBUG_ATTELER_MCP_METHOD", &opts.mcpMethod)
	applyDebugString(getenv, "DEBUG_ATTELER_MCP_PARAMS", &opts.mcpParamsJSON)
	applyDebugString(getenv, "DEBUG_ATTELER_MCP_TOOL", &opts.mcpToolName)
	applyDebugString(getenv, "DEBUG_ATTELER_MCP_TOOL_ARGS", &opts.mcpToolArgsJSON)
	applyDebugString(getenv, "DEBUG_ATTELER_LSP_COMMAND", &opts.lspCommand)
	applyDebugRawStringList(getenv, "DEBUG_ATTELER_LSP_ARGS", &opts.lspArgs)
	applyDebugString(getenv, "DEBUG_ATTELER_LSP_FILE", &opts.lspFilePath)
	applyDebugString(getenv, "DEBUG_ATTELER_LSP_ROOT", &opts.lspRootPath)
	applyDebugString(getenv, "DEBUG_ATTELER_LSP_LANGUAGE", &opts.lspLanguageID)
	applyDebugString(getenv, "DEBUG_ATTELER_LSP_WORKSPACE_SYMBOLS", &opts.lspWorkspaceSymbols)
	applyDebugString(getenv, "DEBUG_ATTELER_GIT_HISTORY_SEARCH", &opts.gitHistorySearch)
	applyDebugPositiveInt(getenv, "DEBUG_ATTELER_GIT_HISTORY_LIMIT", &opts.gitHistoryLimit)
	applyDebugPositiveInt(getenv, "DEBUG_ATTELER_MCP_TIMEOUT_SECONDS", &opts.mcpTimeout)
	applyDebugPositiveInt(getenv, "DEBUG_ATTELER_WATCH_LARGE_FILE_BYTES", &opts.watchLargeFileBytes)
	applyDebugPositiveInt(getenv, "DEBUG_ATTELER_WATCH_INTERVAL_SECONDS", &opts.watchIntervalSeconds)
	applyDebugPositiveInt(getenv, "DEBUG_ATTELER_WATCH_MAX_ITERATIONS", &opts.watchMaxIterations)
}

func applyDebugBool(getenv func(string) string, name string, target *bool) {
	if target == nil || *target {
		return
	}
	switch strings.ToLower(strings.TrimSpace(getenv(name))) {
	case "1", "true", "yes", "on":
		*target = true
	}
}

func applyDebugString(getenv func(string) string, name string, target *string) {
	if target == nil || strings.TrimSpace(*target) != "" {
		return
	}
	if value := strings.TrimSpace(getenv(name)); value != "" {
		*target = value
	}
}

func applyDebugRawStringList(getenv func(string) string, name string, target *rawStringListFlag) {
	if target == nil || len(*target) > 0 {
		return
	}
	if value := strings.TrimSpace(getenv(name)); value != "" {
		*target = append(*target, value)
	}
}

func applyDebugPositiveInt(getenv func(string) string, name string, target *positiveIntFlag) {
	if target == nil || target.set {
		return
	}
	value := strings.TrimSpace(getenv(name))
	if value == "" {
		return
	}
	if err := target.Set(value); err != nil {
		fmt.Fprintln(os.Stderr, "warning: ignoring "+name+": "+err.Error())
	}
}

func main() {
	if err := run(rootContext()); err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		os.Exit(1)
	}
}

func rootContext() context.Context {
	return context.Background()
}

func nonNilContext(ctx context.Context) context.Context {
	if ctx == nil {
		return rootContext()
	}
	return ctx
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

func normalizeOutputFormat(format string) (string, error) {
	format = strings.ToLower(strings.TrimSpace(format))
	if format == "" {
		return outputFormatText, nil
	}
	switch format {
	case outputFormatText, outputFormatJSON:
		return format, nil
	default:
		return "", fmt.Errorf("unsupported output format %q (supported: %s, %s)", format, outputFormatText, outputFormatJSON)
	}
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

func run(ctx context.Context) error {
	ctx = nonNilContext(ctx)
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
	if opts.doctorOffline {
		return doctorOffline(opts)
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
		return listWorktrees(ctx)
	}
	if opts.mergeWorktreeRef != "" {
		return mergeWorktreeBySession(ctx, opts.mergeWorktreeRef)
	}
	if handled, err := runProviderlessCommand(ctx, opts); handled {
		return err
	}

	state, err := loadAppState(ctx, opts)
	if err != nil {
		return err
	}

	return runWithState(ctx, opts, state)
}

func runProviderlessCommand(ctx context.Context, opts cliOptions) (bool, error) {
	ctx = nonNilContext(ctx)
	store := session.NewStore(opts.sessionDir)

	if handled, err := runProviderlessSessionCommand(ctx, opts, store); handled {
		return true, err
	}
	if handled, err := runProviderlessUtilityCommand(ctx, opts, store); handled {
		return true, err
	}
	if !providerlessConfigCommandRequested(opts) && !providerlessLocalAnalysisRequested(opts) {
		return false, nil
	}

	state, stateErr := providerlessState(store)
	if stateErr != nil {
		return true, stateErr
	}
	if handled, err := runProviderlessConfigCommand(ctx, opts, state); handled {
		return true, err
	}
	if handled, err := runProviderlessLocalAnalysisCommand(ctx, opts, state); handled {
		return true, err
	}
	return false, nil
}

func runProviderlessSessionCommand(ctx context.Context, opts cliOptions, store *session.Store) (bool, error) {
	ctx = nonNilContext(ctx)
	switch {
	case opts.listHookEvents || opts.listHookEventsJSON:
		return true, listHookEvents(opts.listHookEventsJSON)
	case opts.listHeadless:
		return true, listHeadlessRuns(store)
	case opts.streamHeadlessID != "":
		return true, streamHeadlessLog(ctx, store, opts.streamHeadlessID)
	case opts.listSessions:
		return true, listSessions(store, opts.listSessionsTag)
	case opts.listSessionTags:
		return true, listSessionTags(store)
	case opts.agentPerformanceSummary:
		return true, listAgentPerformance(store)
	case opts.searchQuery != "":
		return true, searchSessions(store, opts.searchQuery)
	default:
		return false, nil
	}
}

func runProviderlessUtilityCommand(ctx context.Context, opts cliOptions, store *session.Store) (bool, error) {
	if handled, err := runProviderlessFileUtilityCommand(ctx, opts, store); handled {
		return true, err
	}
	return runProviderlessPlanningUtilityCommand(ctx, opts)
}

func runProviderlessFileUtilityCommand(ctx context.Context, opts cliOptions, store *session.Store) (bool, error) {
	switch {
	case opts.evalOutputPath != "":
		return true, evalOutput(opts.evalOutputPath, opts.evalExpected, opts.evalExpectedPath, atteval.MatchMode(opts.evalMode))
	case opts.contextPackPath != "":
		return true, runContextPack(opts.contextPackPath, opts.contextPackTokens.value)
	case opts.lspSymbols || opts.lspWorkspaceSymbols != "":
		return true, runLSPSymbols(ctx, opts)
	case opts.memorySearch != "" || len(opts.memoryIndexFiles) > 0:
		return true, runMemoryCommand(store, opts)
	case opts.vectorSearch != "" || len(opts.vectorIndexFiles) > 0:
		return true, runVectorSearch(opts.vectorSearch, opts.vectorIndexFiles, opts.vectorLimit.value)
	default:
		return false, nil
	}
}

func runProviderlessPlanningUtilityCommand(ctx context.Context, opts cliOptions) (bool, error) {
	switch {
	case opts.mcpManifestPath != "" && (opts.mcpMethod != "" || opts.mcpToolName != ""):
		return true, runMCPInvoke(ctx, opts)
	case opts.mcpManifestPath != "":
		return true, runMCPManifest(opts.mcpManifestPath, opts.mcpCapability)
	case opts.speculatePlan:
		return true, runSpeculatePlan(opts.speculateAgents, opts.speculateGates, opts.speculatePrompt)
	case opts.reviewPlan:
		return true, runReviewPlan(opts.reviewAgents, opts.reviewPaths, opts.reviewGates)
	case opts.asyncPlan:
		return true, runAsyncPlan(opts.asyncTaskSpecs)
	case len(opts.routeCandidates) > 0 && opts.oncePrompt == "" && !opts.readStdin:
		return true, runRouteModels(opts)
	case len(opts.suggestSkillSteps) > 0:
		return true, suggestSkill(opts.suggestSkillSteps, opts.skillMaxSteps.value, opts.skillMinOccurrences.value, opts.skillSaveDir)
	default:
		return false, nil
	}
}

func providerlessConfigCommandRequested(opts cliOptions) bool {
	requested := []bool{
		opts.listAgents,
		opts.describeAgentName != "",
		opts.planAgentsPrompt != "",
		opts.promptCompleteInput != "",
		opts.listPlugins,
		opts.describePluginName != "",
		opts.runPluginTarget != "",
	}
	return anyTrue(requested)
}

func providerlessLocalAnalysisRequested(opts cliOptions) bool {
	requested := []bool{
		opts.gitHistorySearch != "",
		opts.watchLoop,
		opts.watchScan,
		opts.reviewScan,
		providerlessCodeAnalysisRequested(opts),
	}
	return anyTrue(requested)
}

func providerlessCodeAnalysisRequested(opts cliOptions) bool {
	requested := []bool{
		opts.listCodeImports,
		opts.listCodeImportSummary,
		opts.listCodeImportFileSummary,
		opts.listCodeLayers,
		opts.listCodeCycles,
		opts.codeSummary,
		opts.listCodeFiles,
		opts.listCodeSymbolSummary,
		opts.listCodeSymbolFileSummary,
		opts.codeSymbolName != "",
		opts.codeSymbolFileSummary != "",
		opts.codeSymbolPackageSummary != "",
		opts.codeSymbolPrefix != "",
		opts.codeSymbolPrefixFileSummary != "",
		opts.codeSymbolPrefixPackageSummary != "",
		opts.codeSymbolKind != "",
		opts.codeSymbolKindFileSummary != "",
		opts.listCodePackages,
		opts.codePackageName != "",
		opts.listCodePackageImportSummary,
		opts.codePackageImports != "",
		opts.codePackageImportPath != "",
		opts.codePackageImportFiles != "",
		opts.codePackageImportPathFileSummary != "",
		opts.codePackageImportPrefix != "",
		opts.codePackageImportPrefixFiles != "",
		opts.codePackageImportPrefixFileSummary != "",
		opts.codePackageImportFileSummary != "",
		opts.codePackageSymbols != "",
		opts.codePackageSymbolFileSummary != "",
		opts.codePackageSymbolName != "",
		opts.codePackageSymbolNameFileSummary != "",
		opts.codePackageSymbolList != "",
		opts.codePackageSymbolKind != "",
		opts.codePackageSymbolKindFileSummary != "",
		opts.codePackageSymbolPrefix != "",
		opts.codePackageSymbolPrefixFileSummary != "",
		opts.codeFilePath != "",
		opts.codeFileImports != "",
		opts.codeFileSymbols != "",
		opts.codeFileSymbolSummary != "",
		opts.codeFileSymbolName != "",
		opts.codeFileSymbolKind != "",
		opts.codeFileSymbolPrefix != "",
		opts.codeFileImportPrefix != "",
		opts.codeFileImportPath != "",
		opts.codeImpactTarget != "",
		opts.codeReachTarget != "",
		opts.codeDepsTarget != "",
		opts.codeRdepsTarget != "",
	}
	return anyTrue(requested)
}

func anyTrue(values []bool) bool {
	for _, value := range values {
		if value {
			return true
		}
	}
	return false
}

func providerlessState(store *session.Store) (appState, error) {
	cwd, err := os.Getwd()
	if err != nil {
		return appState{}, fmt.Errorf("locate working directory: %w", err)
	}

	cfg, loadedConfigPaths, cfgErr := appconfig.Load()
	if cfgErr != nil {
		fmt.Fprintln(os.Stderr, "warning: "+cfgErr.Error())
	}
	return appState{
		agentRegistry:     agent.NewRegistry(cfg.Agents),
		sessionStore:      store,
		cwd:               cwd,
		loadedConfigPaths: loadedConfigPaths,
		pluginPaths:       append([]string(nil), cfg.Plugins.Paths...),
	}, nil
}

func runProviderlessConfigCommand(ctx context.Context, opts cliOptions, state appState) (bool, error) {
	switch {
	case opts.listAgents:
		listAgents(state.agentRegistry)
		return true, nil
	case opts.describeAgentName != "":
		return true, describeAgent(state.agentRegistry, opts.describeAgentName)
	case opts.planAgentsPrompt != "":
		return true, planAgents(state.agentRegistry, opts.planAgentsPrompt, opts.planAgentNames, opts.planMaxAgents.value)
	case opts.promptCompleteInput != "":
		promptComplete(state.agentRegistry, opts.promptCompleteInput, opts.promptCompleteLimit.value)
		return true, nil
	case opts.listPlugins:
		return true, listPlugins(state.pluginPaths)
	case opts.describePluginName != "":
		return true, describePlugin(state.pluginPaths, opts.describePluginName)
	case opts.runPluginTarget != "":
		return true, runPluginEntrypoint(ctx, state.pluginPaths, opts.runPluginTarget, opts.pluginEntrypoint, opts.pluginDryRun, opts.pluginTimeout.value)
	default:
		return false, nil
	}
}

func runProviderlessLocalAnalysisCommand(ctx context.Context, opts cliOptions, state appState) (bool, error) {
	if handled, err := runStateCodeAnalysisCommand(opts, state); handled {
		return true, err
	}
	switch {
	case opts.gitHistorySearch != "":
		return true, runGitHistorySearch(ctx, state.cwd, opts.gitHistorySearch, opts.gitHistoryLimit.value)
	case opts.watchLoop:
		return true, runWatchLoop(ctx, state.cwd, opts.watchLargeFileBytes.value, opts.watchIntervalSeconds.value, opts.watchMaxIterations.value)
	case opts.watchScan:
		return true, runWatchScan(state.cwd, opts.watchLargeFileBytes.value, opts.watchJSON)
	case opts.reviewScan:
		return true, runReviewScan(state.cwd, opts.watchLargeFileBytes.value)
	default:
		return false, nil
	}
}

func runWithState(ctx context.Context, opts cliOptions, state appState) error {
	ctx = nonNilContext(ctx)
	if handled, err := runStateCommand(ctx, opts, state); handled {
		return err
	}

	outputFormat, err := normalizeOutputFormat(opts.outputFormat)
	if err != nil {
		return err
	}
	if opts.headless && opts.oncePrompt == "" && !opts.readStdin {
		return errors.New("headless mode requires --once, positional prompt text, or --stdin")
	}

	if opts.oncePrompt == "" && !opts.readStdin {
		return runInteractive(ctx, state)
	}

	prompt, err := oneShotPrompt(opts.oncePrompt, opts.readStdin)
	if err != nil {
		return err
	}
	// One-shot mode uses a logger-enabled runner so context-based events
	// (e.g. tool_execute from providers) are visible on stderr.
	if opts.headless {
		state.hookRunner = events.NewRunner(state.hookConfig)
	} else {
		state.hookRunner = events.NewRunnerWithLogger(state.hookConfig, os.Stderr)
	}
	runErr := runOnceWithOptions(
		ctx,
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
		state.maxInputTokens,
		runOnceExecutionOptions{
			Response: responseRecordOptions{
				RecordPath: opts.recordResponsePath,
				ReplayPath: opts.replayResponsePath,
			},
			OutputFormat: outputFormat,
			Headless:     opts.headless,
		},
		state.modelLocked,
		prompt,
	)
	finalizeWorktree(ctx, &state)
	return runErr
}

func runStateCommand(ctx context.Context, opts cliOptions, state appState) (bool, error) {
	if handled, err := runStateReadCommand(opts, state); handled {
		return true, err
	}
	if handled, err := runStateWriteCommand(opts, state); handled {
		return true, err
	}
	if handled, err := runStateUtilityCommand(ctx, opts, state); handled {
		return true, err
	}
	switch {
	case opts.listModels:
		return true, listModels(ctx, state.registry)
	case opts.planAgentsPrompt != "":
		return true, planAgents(state.agentRegistry, opts.planAgentsPrompt, opts.planAgentNames, opts.planMaxAgents.value)
	default:
		return false, nil
	}
}

func runStateUtilityCommand(ctx context.Context, opts cliOptions, state appState) (bool, error) {
	if handled, err := runStateLocalAnalysisCommand(ctx, opts, state); handled {
		return true, err
	}
	if handled, err := runStateExecutionCommand(ctx, opts, state); handled {
		return true, err
	}
	if handled, err := runStateRetrievalCommand(ctx, opts, state); handled {
		return true, err
	}
	switch {
	case opts.evalOutputPath != "":
		return true, evalOutput(opts.evalOutputPath, opts.evalExpected, opts.evalExpectedPath, atteval.MatchMode(opts.evalMode))
	case opts.contextPackPath != "":
		return true, runContextPack(opts.contextPackPath, opts.contextPackTokens.value)
	case len(opts.suggestSkillSteps) > 0:
		return true, suggestSkill(opts.suggestSkillSteps, opts.skillMaxSteps.value, opts.skillMinOccurrences.value, opts.skillSaveDir)
	case opts.promptCompleteInput != "":
		promptComplete(state.agentRegistry, opts.promptCompleteInput, opts.promptCompleteLimit.value)
		return true, nil
	case opts.searchQuery != "":
		return true, searchSessions(state.sessionStore, opts.searchQuery)
	case opts.doctor:
		return true, doctor(ctx, state)
	default:
		return false, nil
	}
}

func runStateRetrievalCommand(ctx context.Context, opts cliOptions, state appState) (bool, error) {
	switch {
	case opts.memorySearch != "" || len(opts.memoryIndexFiles) > 0:
		return true, runMemoryCommand(state.sessionStore, opts)
	case opts.agentMemorySearch != "" || len(opts.agentMemoryIndexFiles) > 0:
		return true, runAgentMemoryCommand(state.cwd, state.selectedAgent, opts)
	case opts.vectorSearch != "" || len(opts.vectorIndexFiles) > 0:
		return true, runVectorSearch(opts.vectorSearch, opts.vectorIndexFiles, opts.vectorLimit.value)
	case opts.mergeArtifactsPath != "":
		return true, mergeArtifacts(ctx, state, opts.mergeArtifactsPath, opts.mergeArtifactMaxBytes.value)
	default:
		return false, nil
	}
}

func runStateExecutionCommand(ctx context.Context, opts cliOptions, state appState) (bool, error) {
	switch {
	case opts.runPluginTarget != "":
		return true, runPluginEntrypoint(ctx, state.pluginPaths, opts.runPluginTarget, opts.pluginEntrypoint, opts.pluginDryRun, opts.pluginTimeout.value)
	case len(opts.spawnAgentSpecs) > 0:
		return true, runSpawnAgents(ctx, state, opts)
	case opts.bashCommand != "":
		return true, runBashCommand(ctx, state, opts)
	case opts.mcpManifestPath != "" && (opts.mcpMethod != "" || opts.mcpToolName != ""):
		return true, runMCPInvoke(ctx, opts)
	case opts.mcpManifestPath != "":
		return true, runMCPManifest(opts.mcpManifestPath, opts.mcpCapability)
	default:
		return false, nil
	}
}

func runStateLocalAnalysisCommand(ctx context.Context, opts cliOptions, state appState) (bool, error) {
	if handled, err := runStateCodeAnalysisCommand(opts, state); handled {
		return true, err
	}
	if handled, err := runStateWorkflowAnalysisCommand(ctx, opts, state); handled {
		return true, err
	}
	switch {
	case opts.gitHistorySearch != "":
		return true, runGitHistorySearch(ctx, state.cwd, opts.gitHistorySearch, opts.gitHistoryLimit.value)
	case opts.lspSymbols || opts.lspWorkspaceSymbols != "":
		return true, runLSPSymbols(ctx, opts)
	case opts.feedbackProposals:
		printFeedbackProposals(state.sessionState)
		return true, nil
	case len(opts.routeCandidates) > 0 && opts.oncePrompt == "" && !opts.readStdin:
		return true, runRouteModels(opts)
	default:
		return false, nil
	}
}

func runStateWorkflowAnalysisCommand(ctx context.Context, opts cliOptions, state appState) (bool, error) {
	switch {
	case opts.watchLoop:
		return true, runWatchLoop(ctx, state.cwd, opts.watchLargeFileBytes.value, opts.watchIntervalSeconds.value, opts.watchMaxIterations.value)
	case opts.watchScan:
		return true, runWatchScan(state.cwd, opts.watchLargeFileBytes.value, opts.watchJSON)
	case opts.reviewScan:
		return true, runReviewScan(state.cwd, opts.watchLargeFileBytes.value)
	case opts.speculatePlan:
		return true, runSpeculatePlan(opts.speculateAgents, opts.speculateGates, opts.speculatePrompt)
	case opts.reviewPlan:
		return true, runReviewPlan(opts.reviewAgents, opts.reviewPaths, opts.reviewGates)
	case opts.asyncPlan:
		return true, runAsyncPlan(opts.asyncTaskSpecs)
	default:
		return false, nil
	}
}

func runStateCodeAnalysisCommand(opts cliOptions, state appState) (bool, error) {
	if handled, err := runStateCodeSymbolCommand(opts, state); handled {
		return true, err
	}
	if handled, err := runStateCodeImportCommand(opts, state); handled {
		return true, err
	}
	if handled, err := runStateCodePackageCommand(opts, state); handled {
		return true, err
	}
	if handled, err := runStateCodeFileCommand(opts, state); handled {
		return true, err
	}
	switch {
	case opts.listCodeLayers:
		return true, listCodeLayers(state.cwd)
	case opts.listCodeCycles:
		return true, listCodeCycles(state.cwd)
	case opts.codeSummary:
		return true, printCodeSummary(state.cwd)
	case opts.listCodeFiles:
		return true, listCodeFiles(state.cwd)
	case opts.codeImpactTarget != "":
		return true, listCodeImpact(state.cwd, opts.codeImpactTarget)
	case opts.codeReachTarget != "":
		return true, listCodeReachable(state.cwd, opts.codeReachTarget)
	case opts.codeDepsTarget != "":
		return true, listCodeDeps(state.cwd, opts.codeDepsTarget)
	case opts.codeRdepsTarget != "":
		return true, listCodeReverseDeps(state.cwd, opts.codeRdepsTarget)
	default:
		return false, nil
	}
}

func runStateCodeSymbolCommand(opts cliOptions, state appState) (bool, error) {
	switch {
	case opts.codeSymbolName != "":
		return true, findCodeSymbol(state.cwd, opts.codeSymbolName)
	case opts.codeSymbolFileSummary != "":
		return true, listCodeSymbolNameFileSummary(state.cwd, opts.codeSymbolFileSummary)
	case opts.codeSymbolPackageSummary != "":
		return true, listCodeSymbolNamePackageSummary(state.cwd, opts.codeSymbolPackageSummary)
	case opts.codeSymbolPrefix != "":
		return true, findCodeSymbolPrefix(state.cwd, opts.codeSymbolPrefix)
	case opts.codeSymbolPrefixFileSummary != "":
		return true, listCodeSymbolPrefixFileSummary(state.cwd, opts.codeSymbolPrefixFileSummary)
	case opts.codeSymbolPrefixPackageSummary != "":
		return true, listCodeSymbolPrefixPackageSummary(state.cwd, opts.codeSymbolPrefixPackageSummary)
	case opts.codeSymbolKind != "":
		return true, findCodeSymbolsByKind(state.cwd, opts.codeSymbolKind)
	case opts.codeSymbolKindFileSummary != "":
		return true, listCodeSymbolKindFileSummary(state.cwd, opts.codeSymbolKindFileSummary)
	case opts.codeSymbolKindPackageSummary != "":
		return true, listCodeSymbolKindPackageSummary(state.cwd, opts.codeSymbolKindPackageSummary)
	case opts.listCodeSymbolSummary:
		return true, listCodeSymbolSummary(state.cwd)
	case opts.listCodeSymbolFileSummary:
		return true, listCodeSymbolFileSummary(state.cwd)
	default:
		return false, nil
	}
}

func runStateCodeImportCommand(opts cliOptions, state appState) (bool, error) {
	switch {
	case opts.listCodeImports:
		return true, listCodeImports(state.cwd)
	case opts.listCodeImportSummary:
		return true, listCodeImportSummary(state.cwd)
	case opts.listCodeImportFileSummary:
		return true, listCodeImportFileSummary(state.cwd)
	case opts.codeImportPath != "":
		return true, listCodeImportPath(state.cwd, opts.codeImportPath)
	case opts.codeImportPathSummary != "":
		return true, listCodeImportPathSummary(state.cwd, opts.codeImportPathSummary)
	case opts.codeImportPathFileSummary != "":
		return true, listCodeImportPathFileSummary(state.cwd, opts.codeImportPathFileSummary)
	case opts.codeImportPathPackageSummary != "":
		return true, listCodeImportPathPackageSummary(state.cwd, opts.codeImportPathPackageSummary)
	case opts.codeImportPrefix != "":
		return true, listCodeImportPrefix(state.cwd, opts.codeImportPrefix)
	case opts.codeImportPrefixSummary != "":
		return true, listCodeImportPrefixSummary(state.cwd, opts.codeImportPrefixSummary)
	case opts.codeImportPrefixFileSummary != "":
		return true, listCodeImportPrefixFileSummary(state.cwd, opts.codeImportPrefixFileSummary)
	case opts.codeImportPrefixPackageSummary != "":
		return true, listCodeImportPrefixPackageSummary(state.cwd, opts.codeImportPrefixPackageSummary)
	default:
		return false, nil
	}
}

func runStateCodePackageCommand(opts cliOptions, state appState) (bool, error) {
	if handled, err := runStateCodePackageImportCommand(opts, state); handled {
		return true, err
	}
	if handled, err := runStateCodePackageSymbolCommand(opts, state); handled {
		return true, err
	}
	switch {
	case opts.listCodePackages:
		return true, listCodePackages(state.cwd)
	case opts.codePackageName != "":
		return true, listCodePackageFiles(state.cwd, opts.codePackageName)
	default:
		return false, nil
	}
}

func runStateCodePackageImportCommand(opts cliOptions, state appState) (bool, error) {
	switch {
	case opts.listCodePackageImportSummary:
		return true, listCodePackageImportSummary(state.cwd)
	case opts.codePackageImports != "":
		return true, listCodePackageImports(state.cwd, opts.codePackageImports)
	case opts.codePackageImportPath != "":
		return true, listCodePackageImportPath(state.cwd, opts.codePackageImportPath)
	case opts.codePackageImportFiles != "":
		return true, listCodePackageImportFiles(state.cwd, opts.codePackageImportFiles)
	case opts.codePackageImportPathFileSummary != "":
		return true, listCodePackageImportPathFileSummary(state.cwd, opts.codePackageImportPathFileSummary)
	case opts.codePackageImportPrefix != "":
		return true, listCodePackageImportPrefix(state.cwd, opts.codePackageImportPrefix)
	case opts.codePackageImportPrefixFiles != "":
		return true, listCodePackageImportPrefixFiles(state.cwd, opts.codePackageImportPrefixFiles)
	case opts.codePackageImportPrefixFileSummary != "":
		return true, listCodePackageImportPrefixFileSummary(state.cwd, opts.codePackageImportPrefixFileSummary)
	case opts.codePackageImportFileSummary != "":
		return true, listCodePackageImportFileSummary(state.cwd, opts.codePackageImportFileSummary)
	default:
		return false, nil
	}
}

func runStateCodePackageSymbolCommand(opts cliOptions, state appState) (bool, error) {
	switch {
	case opts.codePackageSymbols != "":
		return true, listCodePackageSymbols(state.cwd, opts.codePackageSymbols)
	case opts.codePackageSymbolFileSummary != "":
		return true, listCodePackageSymbolFileSummary(state.cwd, opts.codePackageSymbolFileSummary)
	case opts.codePackageSymbolName != "":
		return true, listCodePackageSymbol(state.cwd, opts.codePackageSymbolName)
	case opts.codePackageSymbolNameFileSummary != "":
		return true, listCodePackageSymbolNameFileSummary(state.cwd, opts.codePackageSymbolNameFileSummary)
	case opts.codePackageSymbolList != "":
		return true, listCodePackageSymbolList(state.cwd, opts.codePackageSymbolList)
	case opts.codePackageSymbolKind != "":
		return true, listCodePackageSymbolKind(state.cwd, opts.codePackageSymbolKind)
	case opts.codePackageSymbolKindFileSummary != "":
		return true, listCodePackageSymbolKindFileSummary(state.cwd, opts.codePackageSymbolKindFileSummary)
	case opts.codePackageSymbolPrefix != "":
		return true, listCodePackageSymbolPrefix(state.cwd, opts.codePackageSymbolPrefix)
	case opts.codePackageSymbolPrefixFileSummary != "":
		return true, listCodePackageSymbolPrefixFileSummary(state.cwd, opts.codePackageSymbolPrefixFileSummary)
	default:
		return false, nil
	}
}

func runStateCodeFileCommand(opts cliOptions, state appState) (bool, error) {
	switch {
	case opts.codeFilePath != "":
		return true, showCodeFile(state.cwd, opts.codeFilePath)
	case opts.codeFileImports != "":
		return true, listCodeFileImports(state.cwd, opts.codeFileImports)
	case opts.codeFileSymbols != "":
		return true, listCodeFileSymbols(state.cwd, opts.codeFileSymbols)
	case opts.codeFileSymbolSummary != "":
		return true, listCodeFileSymbolSummary(state.cwd, opts.codeFileSymbolSummary)
	case opts.codeFileSymbolName != "":
		return true, listCodeFileSymbol(state.cwd, opts.codeFileSymbolName)
	case opts.codeFileSymbolKind != "":
		return true, listCodeFileSymbolKind(state.cwd, opts.codeFileSymbolKind)
	case opts.codeFileSymbolPrefix != "":
		return true, listCodeFileSymbolPrefix(state.cwd, opts.codeFileSymbolPrefix)
	case opts.codeFileImportPrefix != "":
		return true, listCodeFileImportPrefix(state.cwd, opts.codeFileImportPrefix)
	case opts.codeFileImportPath != "":
		return true, listCodeFileImportPath(state.cwd, opts.codeFileImportPath)
	default:
		return false, nil
	}
}

func runStateReadCommand(opts cliOptions, state appState) (bool, error) {
	if handled, err := runStateSessionInventoryCommand(opts, state); handled {
		return true, err
	}
	switch {
	case opts.replayRef != "":
		printTranscript(state.sessionState)
		return true, nil
	case opts.showSessionRef != "":
		return true, showSession(state.sessionState, state.sessionStore.Path(state.sessionState.ID))
	case opts.summarySessionRef != "":
		printSessionSummary(state.sessionState, state.sessionStore.Path(state.sessionState.ID))
		return true, nil
	case opts.exportRef != "":
		return true, exportSession(state.sessionState, opts.exportFormat)
	case opts.listAgents:
		listAgents(state.agentRegistry)
		return true, nil
	case opts.describeAgentName != "":
		return true, describeAgent(state.agentRegistry, opts.describeAgentName)
	case opts.listPlugins:
		return true, listPlugins(state.pluginPaths)
	case opts.describePluginName != "":
		return true, describePlugin(state.pluginPaths, opts.describePluginName)
	case opts.listHookEvents || opts.listHookEventsJSON:
		return true, listHookEvents(opts.listHookEventsJSON)
	default:
		return false, nil
	}
}

func runStateSessionInventoryCommand(opts cliOptions, state appState) (bool, error) {
	switch {
	case opts.listSessions:
		return true, listSessions(state.sessionStore, opts.listSessionsTag)
	case opts.listSessionTags:
		return true, listSessionTags(state.sessionStore)
	case opts.agentPerformanceSummary:
		return true, listAgentPerformance(state.sessionStore)
	case opts.listArtifacts:
		listArtifacts(state.sessionState)
		return true, nil
	case opts.listEvaluations:
		listEvaluations(state.sessionState)
		return true, nil
	case opts.listFailures:
		listFailures(state.sessionState)
		return true, nil
	case opts.listMessages:
		listMessages(state.sessionState)
		return true, nil
	default:
		return false, nil
	}
}

func runStateWriteCommand(opts cliOptions, state appState) (bool, error) {
	switch {
	case opts.recordFailure != "":
		return true, recordFailure(state.sessionStore, state.sessionState, opts.recordFailure, opts.failureReason, opts.failureCommit, state.selectedAgent)
	case opts.recordEvaluation != "":
		return true, recordEvaluation(state.sessionStore, state.sessionState, opts.recordEvaluation, opts.evaluationOutcome, opts.evaluationNotes, opts.evaluationReference, opts.evaluationScore.value)
	case opts.recordArtifact != "":
		return true, recordArtifact(state.sessionStore, state.sessionState, opts.recordArtifact, opts.artifactKind, opts.artifactSummary, state.selectedAgent)
	case opts.feedbackApplyConfig != "":
		return true, applyFeedbackProposals(state.sessionState, opts.feedbackApplyConfig, opts.feedbackHistoryPath)
	default:
		return false, nil
	}
}

func loadAppState(ctx context.Context, opts cliOptions) (appState, error) {
	ctx = nonNilContext(ctx)
	cfg, loadedConfigPaths, err := appconfig.Load()
	if err != nil {
		fmt.Fprintln(os.Stderr, "warning: "+err.Error())
	}

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
	selection, err := resolveSelection(opts, cfg, persistedState.ModelForFolder(cwd), agentRegistry, store)
	if err != nil {
		return appState{}, err
	}

	reg := autoRegisterForOptions(ctx, opts, cfg, selection.selectedModel)
	contextOptions := contextOptionsFromConfig(cfg)
	generationDefaults := generationFromConfig(cfg)
	generationOverrides := generationFromOptions(opts)
	maxInputTokens := maxInputTokensFromConfigOptions(cfg, opts)

	providers := reg.ListProviders()
	if len(providers) == 0 && !opts.headless {
		fmt.Fprintln(os.Stderr, "warning: no LLM providers configured, set ANTHROPIC_API_KEY or OPENAI_API_KEY")
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
			wtInfo, err = worktree.CreateContext(ctx, cwd, selection.sessionState.ID)
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
		pluginPaths:         append([]string(nil), cfg.Plugins.Paths...),
		generationDefaults:  generationDefaults,
		generationOverrides: generationOverrides,
		maxInputTokens:      maxInputTokens,
		hookConfig:          cfg.Hooks,
		modelLocked:         selection.modelLocked,
		autoMergeWorktree:   opts.useWorktree && !opts.noAutoMerge,
	}, nil
}

func autoRegisterForOptions(ctx context.Context, opts cliOptions, cfg appconfig.Config, selectedModel string) *llm.Registry {
	if !opts.headless {
		return llm.AutoRegisterWithConfigContext(ctx, llmConfig(cfg, selectedModel))
	}
	previousLogOutput := log.Writer()
	log.SetOutput(io.Discard)
	defer log.SetOutput(previousLogOutput)
	return llm.AutoRegisterWithConfigContext(ctx, llmConfig(cfg, selectedModel))
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
	if err := applyRouteSelection(opts, &state); err != nil {
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

func runInteractive(ctx context.Context, state appState) error {
	ctx = nonNilContext(ctx)
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

	emitHookWarning(ctx, state.hookRunner, events.Event{
		Type:        events.SessionStart,
		SessionID:   state.sessionState.ID,
		SessionPath: state.sessionStore.Path(state.sessionState.ID),
		Agent:       state.selectedAgent,
		Model:       state.selectedModel,
	})

	p := tea.NewProgram(initialModel(
		ctx,
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
		state.maxInputTokens,
		state.modelLocked,
		state.worktreeInfo,
	))
	finalModel, err := p.Run()
	if err != nil {
		emitHookWarning(ctx, state.hookRunner, events.Event{
			Type:        events.Error,
			SessionID:   state.sessionState.ID,
			SessionPath: state.sessionStore.Path(state.sessionState.ID),
			Agent:       state.selectedAgent,
			Model:       state.selectedModel,
			Error:       err.Error(),
		})
		return fmt.Errorf("run TUI: %w", err)
	}
	if m, ok := finalModel.(model); ok {
		printTokenUsageSummary(os.Stderr, m.tokenUsage)
	}
	emitHookWarning(ctx, state.hookRunner, events.Event{
		Type:        events.SessionEnd,
		SessionID:   state.sessionState.ID,
		SessionPath: state.sessionStore.Path(state.sessionState.ID),
		Agent:       state.selectedAgent,
		Model:       state.selectedModel,
	})

	finalizeWorktree(ctx, &state)

	return nil
}

type responseRecordOptions struct {
	RecordPath string
	ReplayPath string
}

type runOnceExecutionOptions struct {
	OutputFormat string
	HeadlessID   string
	Response     responseRecordOptions
	Headless     bool
}

type runOnceResult struct {
	SessionID   string     `json:"session_id"`
	SessionPath string     `json:"session_path"`
	HeadlessID  string     `json:"headless_id,omitempty"`
	Agent       string     `json:"agent,omitempty"`
	Model       string     `json:"model,omitempty"`
	Content     string     `json:"content"`
	TokenUsage  tokenUsage `json:"token_usage"`
}

//nolint:govet // Field order follows request-preparation flow; padding is irrelevant here.
type runOncePrepared struct {
	activeAgent     agentSelection
	generation      generationSettings
	requestMessages []llm.Message
	refs            []contextref.Reference
	fallbackModels  []string
	prompt          string
	requestModel    string
}

type responseRecordFile struct {
	RecordedAt time.Time             `json:"recorded_at"`
	Request    responseRecordRequest `json:"request"`
	Response   responseRecordPayload `json:"response"`
}

//nolint:govet // JSON field order is grouped for stable recording readability.
type responseRecordRequest struct {
	Temperature    *float64      `json:"temperature,omitempty"`
	TopP           *float64      `json:"top_p,omitempty"`
	Seed           *int          `json:"seed,omitempty"`
	Model          string        `json:"model,omitempty"`
	FallbackModels []string      `json:"fallback_models,omitempty"`
	Messages       []llm.Message `json:"messages"`
	MaxTokens      int           `json:"max_tokens,omitempty"`
	ReasoningLevel string        `json:"reasoning_level,omitempty"`
}

type responseRecordPayload struct {
	Content           string `json:"content"`
	Model             string `json:"model,omitempty"`
	InputTokens       int    `json:"input_tokens,omitempty"`
	CachedInputTokens int    `json:"cached_input_tokens,omitempty"`
	OutputTokens      int    `json:"output_tokens,omitempty"`
}

func saveRecordedResponse(path string, params llm.CompleteParams, fallbackModels []string, resp *llm.Response) error {
	if strings.TrimSpace(path) == "" || resp == nil {
		return nil
	}
	record := responseRecordFile{
		RecordedAt: time.Now().UTC(),
		Request: responseRecordRequest{
			Model:          params.Model,
			Messages:       append([]llm.Message(nil), params.Messages...),
			FallbackModels: append([]string(nil), fallbackModels...),
			MaxTokens:      params.MaxTokens,
			Temperature:    params.Temperature,
			TopP:           params.TopP,
			Seed:           params.Seed,
			ReasoningLevel: params.ReasoningLevel,
		},
		Response: responseRecordPayload{
			Content:           resp.Content,
			Model:             resp.Model,
			InputTokens:       resp.InputTokens,
			CachedInputTokens: resp.CachedInputTokens,
			OutputTokens:      resp.OutputTokens,
		},
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil && filepath.Dir(path) != "." {
		return fmt.Errorf("record response: create dir: %w", err)
	}
	data, err := json.MarshalIndent(record, "", "  ")
	if err != nil {
		return fmt.Errorf("record response: marshal: %w", err)
	}
	data = append(data, '\n')
	if err := os.WriteFile(path, data, 0o600); err != nil {
		return fmt.Errorf("record response: write %s: %w", path, err)
	}
	return nil
}

func loadRecordedResponse(path string) (*llm.Response, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("replay response: read %s: %w", path, err)
	}
	var record responseRecordFile
	if err := json.Unmarshal(data, &record); err != nil {
		return nil, fmt.Errorf("replay response: parse %s: %w", path, err)
	}
	if strings.TrimSpace(record.Response.Content) == "" {
		return nil, fmt.Errorf("replay response: %s has empty response content", path)
	}
	return &llm.Response{
		Content:           record.Response.Content,
		Model:             record.Response.Model,
		InputTokens:       record.Response.InputTokens,
		CachedInputTokens: record.Response.CachedInputTokens,
		OutputTokens:      record.Response.OutputTokens,
	}, nil
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
	maxInputTokens int,
	responseOptions responseRecordOptions,
	modelLocked bool,
	prompt string,
) error {
	return runOnceWithOptions(
		ctx,
		reg,
		agents,
		hooks,
		store,
		sessionState,
		contextOptions,
		selectedModel,
		selectedAgent,
		fallbackModels,
		generationDefaults,
		generationOverrides,
		maxInputTokens,
		runOnceExecutionOptions{Response: responseOptions},
		modelLocked,
		prompt,
	)
}

func runOnceWithOptions(
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
	maxInputTokens int,
	executionOptions runOnceExecutionOptions,
	modelLocked bool,
	prompt string,
) error {
	outputFormat, err := normalizeOutputFormat(executionOptions.OutputFormat)
	if err != nil {
		return err
	}
	prepared, err := prepareRunOnceRequest(
		agents,
		contextOptions,
		selectedModel,
		selectedAgent,
		fallbackModels,
		generationDefaults,
		generationOverrides,
		modelLocked,
		prompt,
	)
	if err != nil {
		return err
	}
	if prepared.requestModel != "" {
		sessionState.DefaultModel = prepared.requestModel
	}
	sessionState.DefaultAgent = prepared.activeAgent.name

	emitHookWarning(ctx, hooks, events.Event{
		Type:        events.SessionStart,
		SessionID:   sessionState.ID,
		SessionPath: store.Path(sessionState.ID),
		Agent:       prepared.activeAgent.name,
		Model:       sessionState.DefaultModel,
	})
	defer emitHookWarning(ctx, hooks, events.Event{
		Type:        events.SessionEnd,
		SessionID:   sessionState.ID,
		SessionPath: store.Path(sessionState.ID),
		Agent:       prepared.activeAgent.name,
		Model:       sessionState.DefaultModel,
	})

	if userSaveErr := saveRunOnceUserMessage(ctx, hooks, store, &sessionState, prepared); userSaveErr != nil {
		return userSaveErr
	}

	params := llm.CompleteParams{
		Model:    prepared.requestModel,
		Messages: append(append([]llm.Message(nil), sessionState.Messages[:len(sessionState.Messages)-1]...), prepared.requestMessages...),
	}
	if prepared.activeAgent.ok {
		params = prepared.activeAgent.agent.CompleteParams(prepared.requestModel, params.Messages)
	}
	applyGenerationParams(&params, prepared.generation)
	if budgetErr := validateRequestBudget(reg, params.Model, params.Messages, maxInputTokens); budgetErr != nil {
		return budgetErr
	}

	ctx = events.WithEmitter(ctx, hooks, events.Event{
		SessionID:   sessionState.ID,
		SessionPath: store.Path(sessionState.ID),
		Agent:       prepared.activeAgent.name,
		Model:       prepared.requestModel,
	})
	headlessRun, err := startHeadlessRun(store, executionOptions, sessionState, prepared.prompt, prepared.requestModel, prepared.activeAgent.name)
	if err != nil {
		return err
	}

	resp, err := completeWithRecording(ctx, reg, params, prepared.fallbackModels, executionOptions.Response)
	if err != nil {
		emitHookWarning(ctx, hooks, events.Event{
			Type:        events.Error,
			SessionID:   sessionState.ID,
			SessionPath: store.Path(sessionState.ID),
			Agent:       prepared.activeAgent.name,
			Model:       sessionState.DefaultModel,
			Error:       err.Error(),
		})
		finishHeadlessRun(store, headlessRun, session.HeadlessStatusFailed, err.Error())
		return err
	}

	if err := saveRunOnceAssistantResponse(ctx, hooks, store, &sessionState, prepared.activeAgent.name, resp); err != nil {
		finishHeadlessRun(store, headlessRun, session.HeadlessStatusFailed, err.Error())
		return err
	}

	var usage tokenUsage
	usage.addResponse(resp)
	result := runOnceResult{
		SessionID:   sessionState.ID,
		SessionPath: store.Path(sessionState.ID),
		Agent:       prepared.activeAgent.name,
		Model:       resp.Model,
		Content:     resp.Content,
		TokenUsage:  usage,
	}
	if headlessRun != nil {
		result.HeadlessID = headlessRun.ID
		if resp.Model != "" {
			headlessRun.Model = resp.Model
		}
		if err := store.AppendHeadlessLog(headlessRun.ID, fmt.Sprintf("assistant_message\t%s\tbytes=%d\n", time.Now().UTC().Format(time.RFC3339), len(resp.Content))); err != nil {
			fmt.Fprintln(os.Stderr, "warning: "+err.Error())
		}
		finishHeadlessRun(store, headlessRun, session.HeadlessStatusCompleted, "")
	}
	return writeRunOnceResult(os.Stdout, os.Stderr, result, outputFormat, executionOptions.Headless)
}

func prepareRunOnceRequest(
	agents *agent.Registry,
	contextOptions contextref.Options,
	selectedModel string,
	selectedAgent string,
	fallbackModels []string,
	generationDefaults generationSettings,
	generationOverrides generationSettings,
	modelLocked bool,
	prompt string,
) (runOncePrepared, error) {
	activeAgent, userPrompt, err := resolveAgent(agents, selectedAgent, prompt)
	if err != nil {
		return runOncePrepared{}, err
	}
	requestMessages, refs, err := expandReferences([]llm.Message{{Role: llm.RoleUser, Content: userPrompt}}, contextOptions)
	if err != nil {
		return runOncePrepared{}, err
	}
	requestModel, fallbackModels := requestModelAndFallbacks(selectedModel, modelLocked, fallbackModels, activeAgent)
	return runOncePrepared{
		activeAgent:     activeAgent,
		generation:      generationForRequest(generationDefaults, generationOverrides, activeAgent),
		requestMessages: requestMessages,
		refs:            refs,
		fallbackModels:  fallbackModels,
		prompt:          userPrompt,
		requestModel:    requestModel,
	}, nil
}

func saveRunOnceUserMessage(
	ctx context.Context,
	hooks *events.Runner,
	store *session.Store,
	sessionState *session.Session,
	prepared runOncePrepared,
) error {
	sessionState.Append(llm.RoleUser, prepared.prompt)
	if err := store.Save(*sessionState); err != nil {
		emitHookWarning(ctx, hooks, events.Event{
			Type:        events.Error,
			SessionID:   sessionState.ID,
			SessionPath: store.Path(sessionState.ID),
			Agent:       prepared.activeAgent.name,
			Model:       sessionState.DefaultModel,
			Error:       err.Error(),
		})
		return fmt.Errorf("save session before request: %w", err)
	}
	emitFileWriteWarning(ctx, hooks, *sessionState, store.Path(sessionState.ID), prepared.activeAgent.name, "session")
	for _, ref := range prepared.refs {
		emitHookWarning(ctx, hooks, fileReadEvent(sessionState.ID, store.Path(sessionState.ID), prepared.activeAgent.name, sessionState.DefaultModel, ref))
		emitHookWarning(ctx, hooks, contextAddEvent(sessionState.ID, store.Path(sessionState.ID), prepared.activeAgent.name, sessionState.DefaultModel, ref))
	}
	if prepared.activeAgent.ok {
		emitHookWarning(ctx, hooks, events.Event{
			Type:        events.AgentExecute,
			SessionID:   sessionState.ID,
			SessionPath: store.Path(sessionState.ID),
			Agent:       prepared.activeAgent.name,
			Model:       sessionState.DefaultModel,
			Metadata: map[string]string{
				"agent": prepared.activeAgent.name,
			},
		})
	}
	emitHookWarning(ctx, hooks, events.Event{
		Type:        events.UserMessage,
		SessionID:   sessionState.ID,
		SessionPath: store.Path(sessionState.ID),
		Agent:       prepared.activeAgent.name,
		Model:       sessionState.DefaultModel,
		Role:        string(llm.RoleUser),
		Content:     prepared.prompt,
		Metadata:    referenceMetadata(prepared.refs),
	})
	if len(prepared.refs) > 0 {
		fmt.Fprintln(os.Stderr, "context: "+referenceSummary(prepared.refs))
	}
	return nil
}

func saveRunOnceAssistantResponse(
	ctx context.Context,
	hooks *events.Runner,
	store *session.Store,
	sessionState *session.Session,
	agentName string,
	resp *llm.Response,
) error {
	sessionState.Append(llm.RoleAssistant, resp.Content)
	if resp.Model != "" {
		sessionState.DefaultModel = resp.Model
	}
	if err := store.Save(*sessionState); err != nil {
		emitHookWarning(ctx, hooks, events.Event{
			Type:        events.Error,
			SessionID:   sessionState.ID,
			SessionPath: store.Path(sessionState.ID),
			Agent:       agentName,
			Model:       sessionState.DefaultModel,
			Error:       err.Error(),
		})
		return fmt.Errorf("save session after response: %w", err)
	}
	emitFileWriteWarning(ctx, hooks, *sessionState, store.Path(sessionState.ID), agentName, "session")
	emitHookWarning(ctx, hooks, events.Event{
		Type:        events.AssistantMessage,
		SessionID:   sessionState.ID,
		SessionPath: store.Path(sessionState.ID),
		Agent:       agentName,
		Model:       resp.Model,
		Role:        string(llm.RoleAssistant),
		Content:     resp.Content,
	})
	return nil
}

func startHeadlessRun(
	store *session.Store,
	options runOnceExecutionOptions,
	sessionState session.Session,
	prompt string,
	modelName string,
	agentName string,
) (*session.HeadlessRun, error) {
	if !options.Headless {
		return nil, nil
	}
	if store == nil {
		return nil, errors.New("headless mode requires a session store")
	}
	id := strings.TrimSpace(options.HeadlessID)
	if id == "" {
		id = session.New("", nil).ID
	}
	run := session.HeadlessRun{
		ID:          id,
		SessionID:   sessionState.ID,
		SessionPath: store.Path(sessionState.ID),
		Prompt:      strings.TrimSpace(prompt),
		Model:       modelName,
		Agent:       agentName,
		Status:      session.HeadlessStatusRunning,
	}
	if err := store.SaveHeadlessRun(run); err != nil {
		return nil, fmt.Errorf("start headless run: %w", err)
	}
	saved, err := store.LoadHeadlessRun(id)
	if err != nil {
		return nil, fmt.Errorf("load started headless run: %w", err)
	}
	if err := store.AppendHeadlessLog(id, "started\t"+time.Now().UTC().Format(time.RFC3339)+"\tsession="+sessionState.ID+"\n"); err != nil {
		return nil, fmt.Errorf("write headless start log: %w", err)
	}
	return &saved, nil
}

func finishHeadlessRun(store *session.Store, run *session.HeadlessRun, status session.HeadlessStatus, message string) {
	if store == nil || run == nil {
		return
	}
	now := time.Now().UTC()
	run.Status = status
	run.CompletedAt = &now
	run.Error = strings.TrimSpace(message)
	if err := store.SaveHeadlessRun(*run); err != nil {
		fmt.Fprintln(os.Stderr, "warning: "+err.Error())
	}
	logLine := string(status) + "\t" + now.Format(time.RFC3339)
	if run.Error != "" {
		logLine += "\terror=" + run.Error
	}
	if err := store.AppendHeadlessLog(run.ID, logLine+"\n"); err != nil {
		fmt.Fprintln(os.Stderr, "warning: "+err.Error())
	}
}

func writeRunOnceResult(stdout, stderr io.Writer, result runOnceResult, outputFormat string, headless bool) error {
	switch outputFormat {
	case outputFormatJSON:
		encoder := json.NewEncoder(stdout)
		encoder.SetIndent("", "  ")
		if err := encoder.Encode(result); err != nil {
			return fmt.Errorf("encode one-shot result: %w", err)
		}
		return nil
	default:
		if !headless {
			fmt.Fprintln(stdout, result.Content)
			printTokenUsageSummary(stderr, result.TokenUsage)
			fmt.Fprintln(stderr, "session: "+result.SessionID+" ("+result.SessionPath+")")
		}
		return nil
	}
}

func completeWithRecording(
	ctx context.Context,
	reg *llm.Registry,
	params llm.CompleteParams,
	fallbackModels []string,
	responseOptions responseRecordOptions,
) (*llm.Response, error) {
	if responseOptions.ReplayPath != "" {
		resp, err := loadRecordedResponse(responseOptions.ReplayPath)
		if err != nil {
			return nil, err
		}
		if err := saveRecordedResponse(responseOptions.RecordPath, params, fallbackModels, resp); err != nil {
			return nil, err
		}
		return resp, nil
	}

	resp, err := reg.CompleteWithFallback(ctx, params, fallbackModels)
	if err != nil {
		return nil, fmt.Errorf("complete prompt: %w", err)
	}
	if err := saveRecordedResponse(responseOptions.RecordPath, params, fallbackModels, resp); err != nil {
		return nil, err
	}

	return resp, nil
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

func listPlugins(paths []string) error {
	if len(paths) == 0 {
		fmt.Println("No plugins configured.")
		return nil
	}
	for _, path := range paths {
		manifest, err := attelerplugin.Load(path)
		if err != nil {
			return fmt.Errorf("list plugins: %w", err)
		}
		parts := []string{manifest.Name, manifest.Version}
		if len(manifest.Capabilities) > 0 {
			parts = append(parts, "capabilities="+strings.Join(manifest.Capabilities, ","))
		}
		if manifest.Description != "" {
			parts = append(parts, "description="+manifest.Description)
		}
		parts = append(parts, path)
		fmt.Println(strings.Join(parts, "\t"))
	}
	return nil
}

//nolint:govet // YAML readability is more important than pointer-byte packing here.
type pluginDescription struct {
	Entrypoints  map[string]string `yaml:"entrypoints,omitempty"`
	Capabilities []string          `yaml:"capabilities,omitempty"`
	Name         string            `yaml:"name"`
	Version      string            `yaml:"version"`
	Description  string            `yaml:"description,omitempty"`
	Root         string            `yaml:"root"`
	ManifestPath string            `yaml:"manifest_path"`
}

func describePlugin(paths []string, name string) error {
	registry, err := attelerplugin.NewRegistry(paths)
	if err != nil {
		return fmt.Errorf("describe plugin: %w", err)
	}
	plugin, ok := registry.Get(name)
	if !ok {
		return fmt.Errorf("describe plugin: plugin %q not found", strings.TrimSpace(name))
	}
	out, err := yaml.Marshal(pluginDescription{
		Name:         plugin.Manifest.Name,
		Version:      plugin.Manifest.Version,
		Description:  plugin.Manifest.Description,
		Capabilities: append([]string(nil), plugin.Manifest.Capabilities...),
		Entrypoints:  copyStringMap(plugin.Manifest.Entrypoints),
		Root:         plugin.Root,
		ManifestPath: plugin.ManifestPath,
	})
	if err != nil {
		return fmt.Errorf("describe plugin: marshal %q: %w", name, err)
	}
	fmt.Print(string(out))
	return nil
}

func runPluginEntrypoint(
	ctx context.Context,
	paths []string,
	target, entrypointName string,
	dryRun bool,
	timeoutSeconds int,
) error {
	ctx = nonNilContext(ctx)
	pluginName, entrypointName, err := parsePluginTarget(target, entrypointName)
	if err != nil {
		return err
	}
	registry, err := attelerplugin.NewRegistry(paths)
	if err != nil {
		return fmt.Errorf("run plugin: %w", err)
	}
	if dryRun {
		preview, previewErr := registry.DryRunEntrypoint(pluginName, entrypointName)
		if previewErr != nil {
			return fmt.Errorf("run plugin: %w", previewErr)
		}
		fmt.Println(formatPluginDryRun(preview))
		return nil
	}

	plugin, ok := registry.Get(pluginName)
	if !ok {
		return fmt.Errorf("run plugin: plugin %q not found", pluginName)
	}
	timeout := time.Duration(timeoutSeconds) * time.Second
	if timeout <= 0 {
		timeout = 30 * time.Second
	}
	result, err := attelerplugin.RunEntrypoint(ctx, plugin.Root, plugin.Manifest, entrypointName, timeout)
	if result.Stdout != "" {
		fmt.Print(result.Stdout)
	}
	if result.Stderr != "" {
		fmt.Fprint(os.Stderr, result.Stderr)
	}
	if err != nil {
		return fmt.Errorf("run plugin: %w", err)
	}
	return nil
}

func runBashCommand(ctx context.Context, state appState, opts cliOptions) error {
	ctx = nonNilContext(ctx)
	timeout := time.Duration(opts.bashTimeout.value) * time.Second
	dir := strings.TrimSpace(opts.bashDir)
	if dir == "" {
		dir = state.cwd
	}
	emitHookWarning(ctx, state.hookRunner, events.Event{
		Type:        events.CommandExecute,
		SessionID:   state.sessionState.ID,
		SessionPath: state.sessionStore.Path(state.sessionState.ID),
		Agent:       state.selectedAgent,
		Model:       state.selectedModel,
		Metadata: map[string]string{
			"command": "bash -lc",
			"cwd":     dir,
		},
	})
	result, err := attshell.RunBash(ctx, attshell.Options{
		Command: opts.bashCommand,
		Dir:     dir,
		Timeout: timeout,
	})
	if result.Stdout != "" {
		fmt.Print(result.Stdout)
	}
	if result.Stderr != "" {
		fmt.Fprint(os.Stderr, result.Stderr)
	}
	if err != nil {
		return fmt.Errorf("run bash: %w", err)
	}
	return nil
}

func runSpawnAgents(ctx context.Context, state appState, opts cliOptions) error {
	requests, err := parseSpawnAgentSpecs(opts.spawnAgentSpecs)
	if err != nil {
		return err
	}
	if opts.spawnDryRun {
		fmt.Print(formatSpawnDryRun(requests))
		return nil
	}

	ctx = nonNilContext(ctx)
	if opts.spawnTimeout.value > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, time.Duration(opts.spawnTimeout.value)*time.Second)
		defer cancel()
	}

	binary := strings.TrimSpace(opts.spawnBinary)
	if binary == "" {
		var exeErr error
		binary, exeErr = os.Executable()
		if exeErr != nil || strings.TrimSpace(binary) == "" {
			binary = os.Args[0]
		}
	}

	emitHookWarning(ctx, state.hookRunner, events.Event{
		Type:        events.CommandExecute,
		SessionID:   state.sessionState.ID,
		SessionPath: state.sessionStore.Path(state.sessionState.ID),
		Agent:       state.selectedAgent,
		Model:       state.selectedModel,
		Metadata: map[string]string{
			"command": "spawn-agent",
			"count":   strconv.Itoa(len(requests)),
		},
	})

	results, runErr := subagent.SpawnAll(ctx, requests, subagent.AttelerCommandWithOptions(subagent.CommandOptions{
		Binary: binary,
		Dir:    state.cwd,
	}))
	fmt.Print(formatSpawnResults(results))
	if runErr != nil {
		return fmt.Errorf("spawn agents: %w", runErr)
	}
	return nil
}

func parseSpawnAgentSpecs(specs rawStringListFlag) ([]subagent.Request, error) {
	requests := make([]subagent.Request, 0, len(specs))
	for i, raw := range specs {
		request, err := parseSpawnAgentSpec(raw, i+1)
		if err != nil {
			return nil, err
		}
		requests = append(requests, request)
	}
	if err := validateSpawnRequests(requests); err != nil {
		return nil, err
	}
	return requests, nil
}

func parseSpawnAgentSpec(raw string, index int) (subagent.Request, error) {
	parts := strings.SplitN(strings.TrimSpace(raw), "|", 3)
	switch len(parts) {
	case 2:
		return subagent.Request{
			ID:     fmt.Sprintf("child-%d", index),
			Agent:  strings.TrimSpace(parts[0]),
			Prompt: strings.TrimSpace(parts[1]),
		}, nil
	case 3:
		return subagent.Request{
			ID:     strings.TrimSpace(parts[0]),
			Agent:  strings.TrimSpace(parts[1]),
			Prompt: strings.TrimSpace(parts[2]),
		}, nil
	default:
		return subagent.Request{}, fmt.Errorf("spawn agent spec %q: expected agent|prompt or id|agent|prompt", raw)
	}
}

func validateSpawnRequests(requests []subagent.Request) error {
	seen := make(map[string]struct{}, len(requests))
	for i, request := range requests {
		if strings.TrimSpace(request.ID) == "" {
			return fmt.Errorf("spawn agent request %d: id is required", i)
		}
		if strings.TrimSpace(request.Agent) == "" {
			return fmt.Errorf("spawn agent request %q: agent is required", request.ID)
		}
		if strings.TrimSpace(request.Prompt) == "" {
			return fmt.Errorf("spawn agent request %q: prompt is required", request.ID)
		}
		if _, ok := seen[request.ID]; ok {
			return fmt.Errorf("spawn agent: duplicate request id %q", request.ID)
		}
		seen[request.ID] = struct{}{}
	}
	return nil
}

func formatSpawnDryRun(requests []subagent.Request) string {
	var b strings.Builder
	fmt.Fprintf(&b, "Would spawn %d sub-agent(s).\n", len(requests))
	for _, request := range requests {
		fmt.Fprintf(&b, "id=%s\tagent=%s\tprompt=%s\n", request.ID, request.Agent, request.Prompt)
	}
	return b.String()
}

func formatSpawnResults(results []subagent.Result) string {
	if len(results) == 0 {
		return ""
	}
	var b strings.Builder
	for _, result := range results {
		status := "ok"
		if result.Error != "" {
			status = "error"
		}
		fmt.Fprintf(
			&b,
			"id=%s\tagent=%s\tstatus=%s\tduration=%s\n",
			result.Request.ID,
			result.Request.Agent,
			status,
			result.Duration.Round(time.Millisecond),
		)
		if strings.TrimSpace(result.Output) != "" {
			fmt.Fprintf(&b, "output=%s\n", strings.TrimSpace(result.Output))
		}
		if result.Error != "" {
			fmt.Fprintf(&b, "error=%s\n", result.Error)
		}
	}
	return b.String()
}

func parsePluginTarget(target, entrypointName string) (pluginName, entrypoint string, err error) {
	target = strings.TrimSpace(target)
	entrypointName = strings.TrimSpace(entrypointName)
	if target == "" {
		return "", "", errors.New("run plugin: plugin name is required")
	}
	if entrypointName != "" {
		return target, entrypointName, nil
	}
	pluginName, entrypoint, ok := strings.Cut(target, "/")
	if !ok || strings.TrimSpace(pluginName) == "" || strings.TrimSpace(entrypoint) == "" {
		return "", "", errors.New("run plugin: pass --plugin-entrypoint or use plugin/entrypoint")
	}
	return strings.TrimSpace(pluginName), strings.TrimSpace(entrypoint), nil
}

func formatPluginDryRun(dryRun attelerplugin.DryRun) string {
	entrypoint := dryRun.Entrypoint
	return strings.Join([]string{
		dryRun.Description,
		"plugin=" + entrypoint.PluginName,
		"entrypoint=" + entrypoint.EntrypointName,
		"path=" + entrypoint.Path,
		"cwd=" + entrypoint.Root,
	}, "\n")
}

func copyStringMap(in map[string]string) map[string]string {
	if len(in) == 0 {
		return nil
	}
	out := make(map[string]string, len(in))
	maps.Copy(out, in)
	return out
}

func runMCPManifest(path, capability string) error {
	manifest, err := loadMCPManifest(path)
	if err != nil {
		return err
	}
	if err := manifest.Validate(); err != nil {
		return fmt.Errorf("mcp manifest: validate: %w", err)
	}
	if strings.TrimSpace(capability) != "" {
		servers := manifest.Find(capability)
		if len(servers) == 0 {
			fmt.Println("No MCP servers found.")
			return nil
		}
		for i := range servers {
			fmt.Println(formatMCPServer(servers[i]))
		}
		return nil
	}
	for _, name := range manifest.List() {
		fmt.Println(name)
	}
	return nil
}

func runMCPInvoke(ctx context.Context, opts cliOptions) error {
	ctx = nonNilContext(ctx)
	if strings.TrimSpace(opts.mcpMethod) != "" && strings.TrimSpace(opts.mcpToolName) != "" {
		return errors.New("mcp invoke: use either --mcp-method or --mcp-tool, not both")
	}
	if strings.TrimSpace(opts.mcpServerName) == "" {
		return errors.New("mcp invoke: --mcp-server is required")
	}
	manifest, err := loadMCPManifest(opts.mcpManifestPath)
	if err != nil {
		return err
	}
	if validateErr := manifest.Validate(); validateErr != nil {
		return fmt.Errorf("mcp invoke: validate manifest: %w", validateErr)
	}
	server, ok := findMCPServer(manifest, opts.mcpServerName)
	if !ok {
		return fmt.Errorf("mcp invoke: server %q not found", strings.TrimSpace(opts.mcpServerName))
	}

	timeout := time.Duration(opts.mcpTimeout.value) * time.Second
	var response *mcp.Response
	if strings.TrimSpace(opts.mcpToolName) != "" {
		args, parseErr := parseMCPToolArgs(opts.mcpToolArgsJSON)
		if parseErr != nil {
			return parseErr
		}
		response, err = mcp.CallTool(ctx, server, opts.mcpToolName, args, timeout)
	} else {
		params, parseErr := parseJSONParam(opts.mcpParamsJSON, "mcp params")
		if parseErr != nil {
			return parseErr
		}
		response, err = mcp.Invoke(ctx, server, mcp.Request{Method: opts.mcpMethod, Params: params}, timeout)
	}
	if response != nil {
		fmt.Println(formatMCPResponse(response))
	}
	if err != nil {
		return fmt.Errorf("mcp invoke: %w", err)
	}
	return nil
}

func loadMCPManifest(path string) (mcp.Manifest, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return mcp.Manifest{}, fmt.Errorf("mcp manifest: read %s: %w", path, err)
	}
	var manifest mcp.Manifest
	if err := yaml.Unmarshal(data, &manifest); err != nil {
		return mcp.Manifest{}, fmt.Errorf("mcp manifest: parse %s: %w", path, err)
	}
	return manifest, nil
}

func findMCPServer(manifest mcp.Manifest, name string) (mcp.Server, bool) {
	name = strings.TrimSpace(name)
	if name == "" {
		return mcp.Server{}, false
	}
	for _, server := range manifest.Servers {
		if strings.TrimSpace(server.Name) == name {
			return server, true
		}
	}
	return mcp.Server{}, false
}

func formatMCPServer(server mcp.Server) string {
	parts := []string{server.Name, "command=" + server.Command}
	if len(server.Args) > 0 {
		parts = append(parts, "args="+strings.Join(server.Args, ","))
	}
	if strings.TrimSpace(server.CWD) != "" {
		parts = append(parts, "cwd="+strings.TrimSpace(server.CWD))
	}
	if len(server.Capabilities) > 0 {
		capabilities := append([]string(nil), server.Capabilities...)
		sort.Strings(capabilities)
		parts = append(parts, "capabilities="+strings.Join(capabilities, ","))
	}
	return strings.Join(parts, "\t")
}

func parseMCPToolArgs(raw string) (map[string]any, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil, nil
	}
	var args map[string]any
	if err := json.Unmarshal([]byte(raw), &args); err != nil {
		return nil, fmt.Errorf("mcp tool args: parse JSON object: %w", err)
	}
	if args == nil {
		return nil, errors.New("mcp tool args: expected JSON object")
	}
	return args, nil
}

func parseJSONParam(raw, label string) (any, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil, nil
	}
	var value any
	if err := json.Unmarshal([]byte(raw), &value); err != nil {
		return nil, fmt.Errorf("%s: parse JSON: %w", label, err)
	}
	return value, nil
}

func formatMCPResponse(response *mcp.Response) string {
	if response == nil {
		return ""
	}
	if response.Error != nil {
		data, err := json.MarshalIndent(response.Error, "", "  ")
		if err == nil {
			return string(data)
		}
		return response.Error.Message
	}
	if len(response.Result) == 0 {
		return "{}"
	}
	var value any
	if err := json.Unmarshal(response.Result, &value); err != nil {
		return string(response.Result)
	}
	data, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		return string(response.Result)
	}
	return string(data)
}

func runLSPSymbols(ctx context.Context, opts cliOptions) error {
	lspOptions := lsp.Options{
		Command:    strings.TrimSpace(opts.lspCommand),
		Args:       append([]string(nil), opts.lspArgs...),
		FilePath:   strings.TrimSpace(opts.lspFilePath),
		RootPath:   strings.TrimSpace(opts.lspRootPath),
		LanguageID: strings.TrimSpace(opts.lspLanguageID),
	}
	var (
		symbols []lsp.Symbol
		err     error
	)
	if strings.TrimSpace(opts.lspWorkspaceSymbols) != "" {
		symbols, err = lsp.WorkspaceSymbols(nonNilContext(ctx), lspOptions, opts.lspWorkspaceSymbols)
	} else {
		symbols, err = lsp.DocumentSymbols(nonNilContext(ctx), lspOptions)
	}
	if err != nil {
		return fmt.Errorf("lsp symbols: %w", err)
	}
	fmt.Print(formatLSPSymbols(symbols))
	return nil
}

func formatLSPSymbols(symbols []lsp.Symbol) string {
	if len(symbols) == 0 {
		return "No LSP symbols found.\n"
	}
	var b strings.Builder
	writeLSPSymbols(&b, symbols, 0)
	return b.String()
}

func writeLSPSymbols(b *strings.Builder, symbols []lsp.Symbol, depth int) {
	indent := strings.Repeat("  ", depth)
	for i := range symbols {
		symbol := symbols[i]
		parts := []string{
			indent + symbol.Name,
			"kind=" + strconv.Itoa(symbol.Kind),
			"range=" + formatLSPRange(symbol.Range),
		}
		if symbol.Detail != "" {
			parts = append(parts, "detail="+symbol.Detail)
		}
		if symbol.ContainerName != "" {
			parts = append(parts, "container="+symbol.ContainerName)
		}
		if symbol.URI != "" {
			parts = append(parts, "uri="+symbol.URI)
		}
		b.WriteString(strings.Join(parts, "\t"))
		b.WriteString("\n")
		writeLSPSymbols(b, symbol.Children, depth+1)
	}
}

func formatLSPRange(r lsp.Range) string {
	return fmt.Sprintf("%d:%d-%d:%d", r.Start.Line, r.Start.Character, r.End.Line, r.End.Character)
}

func runContextPack(path string, maxTokens int) error {
	data, err := os.ReadFile(path)
	if err != nil {
		return fmt.Errorf("context pack: read %s: %w", path, err)
	}
	messages := parseContextPackMessages(string(data))
	result := contextpack.Compact(messages, maxTokens)
	fmt.Print(formatContextPackResult(result))
	return nil
}

func parseContextPackMessages(text string) []llm.Message {
	var messages []llm.Message
	for rawLine := range strings.SplitSeq(strings.ReplaceAll(text, "\r\n", "\n"), "\n") {
		line := strings.TrimRight(rawLine, "\r")
		role, content, ok := parseRoleLine(line)
		if ok {
			messages = append(messages, llm.Message{Role: role, Content: content})
			continue
		}
		if len(messages) == 0 {
			if strings.TrimSpace(line) != "" {
				messages = append(messages, llm.Message{Role: llm.RoleUser, Content: line})
			}
			continue
		}
		if line != "" {
			messages[len(messages)-1].Content += "\n" + line
		}
	}
	return messages
}

func parseRoleLine(line string) (llm.Role, string, bool) {
	roleText, content, ok := strings.Cut(line, ":")
	if !ok {
		return "", "", false
	}
	switch strings.ToLower(strings.TrimSpace(roleText)) {
	case string(llm.RoleSystem):
		return llm.RoleSystem, strings.TrimSpace(content), true
	case string(llm.RoleUser):
		return llm.RoleUser, strings.TrimSpace(content), true
	case string(llm.RoleAssistant):
		return llm.RoleAssistant, strings.TrimSpace(content), true
	default:
		return "", "", false
	}
}

func formatContextPackResult(result contextpack.Result) string {
	var b strings.Builder
	stats := result.Stats
	fmt.Fprintf(&b, "compressed: %t\n", stats.Compressed)
	fmt.Fprintf(&b, "messages: %d/%d\n", stats.OutputCount, stats.OriginalCount)
	fmt.Fprintf(&b, "omitted: %d\n", stats.OmittedCount)
	fmt.Fprintf(&b, "tokens: %d/%d", stats.OutputEstimatedTokens, stats.OriginalEstimatedTokens)
	if stats.MaxEstimatedTokens > 0 {
		fmt.Fprintf(&b, " max=%d", stats.MaxEstimatedTokens)
	}
	b.WriteString("\n")
	b.WriteString("output:\n")
	for _, message := range result.Messages {
		fmt.Fprintf(&b, "  %s: %s\n", message.Role, strings.ReplaceAll(message.Content, "\n", "\n    "))
	}
	return b.String()
}

func runVectorSearch(query string, paths []string, limit int) error {
	if strings.TrimSpace(query) == "" {
		return errors.New("vector search: --vector-search is required")
	}
	if len(paths) == 0 {
		return errors.New("vector search: at least one --vector-index file is required")
	}
	if limit == 0 {
		limit = 5
	}
	vectorizer, err := vector.NewTextVectorizer(0)
	if err != nil {
		return fmt.Errorf("vector search: create vectorizer: %w", err)
	}
	store, err := vector.NewStore(vectorizer.Dimensions)
	if err != nil {
		return fmt.Errorf("vector search: create store: %w", err)
	}
	for _, path := range paths {
		addErr := addVectorFile(store, vectorizer, path)
		if addErr != nil {
			return addErr
		}
	}
	queryVector, err := vectorizer.Vectorize(query)
	if err != nil {
		return fmt.Errorf("vector search: vectorize query: %w", err)
	}
	results, err := store.Search(queryVector, limit)
	if err != nil {
		return fmt.Errorf("vector search failed: %w", err)
	}
	if len(results) == 0 {
		fmt.Println("No vector results found.")
		return nil
	}
	for i := range results {
		fmt.Println(formatVectorResult(results[i]))
	}
	return nil
}

func addVectorFile(store *vector.Store, vectorizer *vector.TextVectorizer, path string) error {
	data, err := os.ReadFile(path)
	if err != nil {
		return fmt.Errorf("vector search: read %s: %w", path, err)
	}
	if !utf8.Valid(data) {
		return fmt.Errorf("vector search: %s is not valid UTF-8", path)
	}
	vec, err := vectorizer.Vectorize(string(data))
	if err != nil {
		return fmt.Errorf("vector search: vectorize %s: %w", path, err)
	}
	clean := filepath.Clean(path)
	if err := store.Add(vector.Document{ID: clean, Text: string(data), Vector: vec, Metadata: map[string]string{"path": clean}}); err != nil {
		return fmt.Errorf("vector search: index %s: %w", path, err)
	}
	return nil
}

func formatVectorResult(result vector.Result) string {
	parts := []string{
		result.Document.ID,
		fmt.Sprintf("score=%.4f", result.Score),
	}
	if path := result.Document.Metadata["path"]; path != "" {
		parts = append(parts, "path="+path)
	}
	return strings.Join(parts, "\t")
}

func runAgentMemoryCommand(root, selectedAgent string, opts cliOptions) error {
	agentName := strings.TrimSpace(opts.agentMemoryAgent)
	if agentName == "" {
		agentName = strings.TrimSpace(selectedAgent)
	}
	if agentName == "" {
		return errors.New("agent memory: --agent-memory-agent or --agent is required")
	}

	storePath := strings.TrimSpace(opts.agentMemoryStorePath)
	if storePath == "" {
		storePath = filepath.Join(root, ".atteler", "agent-memory.json")
	}

	store, err := loadAgentMemoryStore(storePath)
	if err != nil {
		return err
	}
	for _, path := range opts.agentMemoryIndexFiles {
		if addErr := store.AddFile(agentName, path); addErr != nil {
			return fmt.Errorf("agent memory: index %s: %w", path, addErr)
		}
	}
	if len(opts.agentMemoryIndexFiles) > 0 {
		if saveErr := store.Save(storePath); saveErr != nil {
			return fmt.Errorf("agent memory: save store: %w", saveErr)
		}
		fmt.Printf("Indexed %d file(s) for agent %s in %s\n", len(opts.agentMemoryIndexFiles), agentName, storePath)
	}
	if strings.TrimSpace(opts.agentMemorySearch) == "" {
		return nil
	}
	limit := opts.agentMemoryLimit.value
	if limit == 0 {
		limit = 5
	}
	results, err := store.Search(agentName, opts.agentMemorySearch, limit)
	if err != nil {
		return fmt.Errorf("agent memory: search: %w", err)
	}
	if len(results) == 0 {
		fmt.Println("No agent memory results found.")
		return nil
	}
	for i := range results {
		fmt.Println(formatAgentMemoryResult(results[i]))
	}
	return nil
}

func loadAgentMemoryStore(path string) (*agentmemory.Store, error) {
	if _, err := os.Stat(path); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			store, newErr := agentmemory.NewStore(0)
			if newErr != nil {
				return nil, fmt.Errorf("agent memory: create store: %w", newErr)
			}
			return store, nil
		}
		return nil, fmt.Errorf("agent memory: stat store %s: %w", path, err)
	}
	store, err := agentmemory.Load(path)
	if err != nil {
		return nil, fmt.Errorf("agent memory: load store: %w", err)
	}
	return store, nil
}

func formatAgentMemoryResult(result agentmemory.Result) string {
	parts := []string{
		result.Document.ID,
		fmt.Sprintf("score=%.4f", result.Score),
	}
	if result.Document.Path != "" {
		parts = append(parts, "path="+result.Document.Path)
	}
	if kind := result.Document.Metadata["kind"]; kind != "" {
		parts = append(parts, "kind="+kind)
	}
	return strings.Join(parts, "\t")
}

func runMemoryCommand(store *session.Store, opts cliOptions) error {
	mem, err := buildMemoryStore(store, opts)
	if err != nil {
		return err
	}
	if opts.memoryStorePath != "" && len(opts.memoryIndexFiles) > 0 {
		if saveErr := mem.Save(opts.memoryStorePath); saveErr != nil {
			return fmt.Errorf("memory: save store: %w", saveErr)
		}
		if opts.memorySearch == "" {
			fmt.Printf("Indexed %d document(s) into %s\n", len(mem.Documents), opts.memoryStorePath)
			return nil
		}
	}
	if opts.memorySearch == "" {
		return errors.New("memory: --memory-search is required unless indexing into --memory-store")
	}

	limit := opts.memoryLimit.value
	if limit == 0 {
		limit = 5
	}
	results, err := mem.Search(opts.memorySearch, limit)
	if err != nil {
		return fmt.Errorf("memory: search: %w", err)
	}
	if len(results) == 0 {
		fmt.Println("No memory results found.")
		return nil
	}
	for i := range results {
		fmt.Println(formatMemoryResult(results[i]))
	}
	return nil
}

func buildMemoryStore(store *session.Store, opts cliOptions) (*memory.Store, error) {
	mem, err := loadMemoryStore(opts.memoryStorePath)
	if err != nil {
		return nil, err
	}
	if len(opts.memoryIndexFiles) > 0 {
		if err := mem.AddFiles(opts.memoryIndexFiles...); err != nil {
			return nil, fmt.Errorf("memory: index files: %w", err)
		}
	}
	if opts.memoryStorePath == "" || len(mem.Documents) == 0 {
		if err := addSessionMemory(mem, store); err != nil {
			return nil, err
		}
	}
	return mem, nil
}

func loadMemoryStore(path string) (*memory.Store, error) {
	if strings.TrimSpace(path) == "" {
		return memory.NewStore(), nil
	}
	if _, err := os.Stat(path); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return memory.NewStore(), nil
		}
		return nil, fmt.Errorf("memory: stat store %s: %w", path, err)
	}
	store, err := memory.Load(path)
	if err != nil {
		return nil, fmt.Errorf("memory: load store: %w", err)
	}
	return store, nil
}

func addSessionMemory(mem *memory.Store, store *session.Store) error {
	summaries, err := store.List()
	if err != nil {
		return fmt.Errorf("memory: list sessions: %w", err)
	}
	for i := range summaries {
		summary := &summaries[i]
		saved, err := store.Load(summary.Path)
		if err != nil {
			return fmt.Errorf("memory: load session %s: %w", summary.ID, err)
		}
		if err := mem.AddSession(saved); err != nil {
			return fmt.Errorf("memory: index session %s: %w", summary.ID, err)
		}
	}
	return nil
}

func formatMemoryResult(result memory.Result) string {
	parts := []string{
		result.Document.ID,
		fmt.Sprintf("score=%.4f", result.Score),
	}
	if result.Document.Path != "" {
		parts = append(parts, "path="+result.Document.Path)
	}
	if len(result.Matches) > 0 {
		parts = append(parts, "matches="+strings.Join(result.Matches, ","))
	}
	if kind := result.Document.Metadata["kind"]; kind != "" {
		parts = append(parts, "kind="+kind)
	}
	line := strings.Join(parts, "\t")
	if result.Snippet == "" {
		return line
	}
	return line + "\n  " + result.Snippet
}

func planAgents(registry *agent.Registry, prompt string, requested []string, maxAgents int) error {
	plan, err := registry.PlanAgents(prompt, requested, maxAgents)
	if err != nil {
		return fmt.Errorf("plan agents: %w", err)
	}
	if len(plan.Participants) == 0 {
		fmt.Println("No agents matched.")
		return nil
	}
	for i := range plan.Participants {
		fmt.Println(formatAgentPlanParticipant(&plan.Participants[i]))
	}
	return nil
}

func formatAgentPlanParticipant(participant *agent.Participant) string {
	parts := []string{participant.Agent.Name, "source=" + participant.Source}
	if participant.Pattern != "" {
		parts = append(parts, "match="+participant.Pattern)
	}
	if len(participant.Agent.Capabilities) > 0 {
		parts = append(parts, "capabilities="+strings.Join(participant.Agent.Capabilities, ","))
	}
	if participant.Agent.Model != "" {
		parts = append(parts, "model="+participant.Agent.Model)
	}
	return strings.Join(parts, "\t")
}

func findCodeSymbol(root, name string) error {
	idx, err := codeintel.IndexDir(root)
	if err != nil {
		return fmt.Errorf("code symbol: index %s: %w", root, err)
	}
	matches := idx.FindSymbol(name)
	if len(matches) == 0 {
		fmt.Println("No code symbols found.")
		return nil
	}
	for i := range matches {
		fmt.Println(formatCodeSymbol(root, matches[i]))
	}
	return nil
}

type codeSymbolFileSummary struct {
	Path    string
	Package string
	Symbols int
}

func listCodeSymbolFileSummary(root string) error {
	idx, err := codeintel.IndexDir(root)
	if err != nil {
		return fmt.Errorf("code symbol file summary: index %s: %w", root, err)
	}
	summaries := summarizeCodeSymbolFiles(root, idx)
	if len(summaries) == 0 {
		fmt.Println("No code symbols found.")
		return nil
	}
	for i := range summaries {
		fmt.Println(formatCodeSymbolFileSummary(summaries[i]))
	}
	return nil
}

func summarizeCodeSymbolFiles(root string, idx codeintel.Index) []codeSymbolFileSummary {
	summaries := make([]codeSymbolFileSummary, 0)
	for i := range idx.Files {
		file := idx.Files[i]
		if len(file.Symbols) == 0 {
			continue
		}
		summaries = append(summaries, codeSymbolFileSummary{
			Path:    relativeCodePath(root, file.Path),
			Package: file.Package,
			Symbols: len(file.Symbols),
		})
	}
	sort.Slice(summaries, func(i, j int) bool {
		if summaries[i].Symbols != summaries[j].Symbols {
			return summaries[i].Symbols > summaries[j].Symbols
		}
		return summaries[i].Path < summaries[j].Path
	})
	return summaries
}

func formatCodeSymbolFileSummary(summary codeSymbolFileSummary) string {
	return "path=" + summary.Path + "	package=" + summary.Package + "	symbols=" + strconv.Itoa(summary.Symbols)
}

type codeSymbolSummary struct {
	Kind  string
	Count int
}

func listCodeSymbolSummary(root string) error {
	idx, err := codeintel.IndexDir(root)
	if err != nil {
		return fmt.Errorf("code symbol summary: index %s: %w", root, err)
	}
	summaries := summarizeCodeSymbols(idx)
	if len(summaries) == 0 {
		fmt.Println("No code symbols found.")
		return nil
	}
	for i := range summaries {
		fmt.Println(formatCodeSymbolSummary(summaries[i]))
	}
	return nil
}

func summarizeCodeSymbols(idx codeintel.Index) []codeSymbolSummary {
	counts := make(map[string]int)
	for i := range idx.Symbols {
		if idx.Symbols[i].Kind != "" {
			counts[idx.Symbols[i].Kind]++
		}
	}
	summaries := make([]codeSymbolSummary, 0, len(counts))
	for kind, count := range counts {
		summaries = append(summaries, codeSymbolSummary{Kind: kind, Count: count})
	}
	sort.Slice(summaries, func(i, j int) bool {
		if summaries[i].Count != summaries[j].Count {
			return summaries[i].Count > summaries[j].Count
		}
		return summaries[i].Kind < summaries[j].Kind
	})
	return summaries
}

func formatCodeSymbolSummary(summary codeSymbolSummary) string {
	return "kind=" + summary.Kind + "	symbols=" + strconv.Itoa(summary.Count)
}

func listCodeSymbolKindFileSummary(root, kind string) error {
	idx, err := codeintel.IndexDir(root)
	if err != nil {
		return fmt.Errorf("code symbol kind file summary: index %s: %w", root, err)
	}
	summaries := summarizeCodeSymbolKindFiles(root, idx, kind)
	if len(summaries) == 0 {
		fmt.Println("No code symbols found.")
		return nil
	}
	for i := range summaries {
		fmt.Println(formatCodeSymbolFileSummary(summaries[i]))
	}
	return nil
}

func summarizeCodeSymbolKindFiles(root string, idx codeintel.Index, kind string) []codeSymbolFileSummary {
	kind = strings.TrimSpace(kind)
	if kind == "" {
		return nil
	}
	summaries := make([]codeSymbolFileSummary, 0)
	for i := range idx.Files {
		file := idx.Files[i]
		count := 0
		for j := range file.Symbols {
			if strings.EqualFold(file.Symbols[j].Kind, kind) {
				count++
			}
		}
		if count == 0 {
			continue
		}
		summaries = append(summaries, codeSymbolFileSummary{
			Path:    relativeCodePath(root, file.Path),
			Package: file.Package,
			Symbols: count,
		})
	}
	sort.Slice(summaries, func(i, j int) bool {
		if summaries[i].Symbols != summaries[j].Symbols {
			return summaries[i].Symbols > summaries[j].Symbols
		}
		return summaries[i].Path < summaries[j].Path
	})
	return summaries
}

func listCodeSymbolKindPackageSummary(root, kind string) error {
	idx, err := codeintel.IndexDir(root)
	if err != nil {
		return fmt.Errorf("code symbol kind package summary: index %s: %w", root, err)
	}
	summaries := summarizeCodeSymbolKindPackages(idx, kind)
	if len(summaries) == 0 {
		fmt.Println("No code symbols found.")
		return nil
	}
	for i := range summaries {
		fmt.Println(formatCodePackageSummary(summaries[i]))
	}
	return nil
}

func summarizeCodeSymbolKindPackages(idx codeintel.Index, kind string) []codePackageSummary {
	kind = strings.TrimSpace(kind)
	if kind == "" {
		return nil
	}
	byPackage := make(map[string]*codePackageSummary)
	filesByPackage := make(map[string]map[string]struct{})
	for i := range idx.Files {
		file := idx.Files[i]
		if file.Package == "" {
			continue
		}
		count := 0
		for j := range file.Symbols {
			if strings.EqualFold(file.Symbols[j].Kind, kind) {
				count++
			}
		}
		if count == 0 {
			continue
		}
		summary, ok := byPackage[file.Package]
		if !ok {
			summary = &codePackageSummary{Name: file.Package}
			byPackage[file.Package] = summary
			filesByPackage[file.Package] = make(map[string]struct{})
		}
		summary.Symbols += count
		filesByPackage[file.Package][file.Path] = struct{}{}
	}
	summaries := make([]codePackageSummary, 0, len(byPackage))
	for packageName, summary := range byPackage {
		summary.Files = len(filesByPackage[packageName])
		summaries = append(summaries, *summary)
	}
	sort.Slice(summaries, func(i, j int) bool {
		if summaries[i].Symbols != summaries[j].Symbols {
			return summaries[i].Symbols > summaries[j].Symbols
		}
		if summaries[i].Files != summaries[j].Files {
			return summaries[i].Files > summaries[j].Files
		}
		return summaries[i].Name < summaries[j].Name
	})
	return summaries
}

func findCodeSymbolsByKind(root, kind string) error {
	idx, err := codeintel.IndexDir(root)
	if err != nil {
		return fmt.Errorf("code symbol kind: index %s: %w", root, err)
	}
	matches := codeSymbolsByKind(idx, kind)
	if len(matches) == 0 {
		fmt.Println("No code symbols found.")
		return nil
	}
	for i := range matches {
		fmt.Println(formatCodeSymbol(root, matches[i]))
	}
	return nil
}

func codeSymbolsByKind(idx codeintel.Index, kind string) []codeintel.Symbol {
	kind = strings.TrimSpace(strings.ToLower(kind))
	if kind == "" {
		return nil
	}
	matches := make([]codeintel.Symbol, 0)
	for i := range idx.Symbols {
		if strings.EqualFold(idx.Symbols[i].Kind, kind) {
			matches = append(matches, idx.Symbols[i])
		}
	}
	sortCodeSymbols(matches)
	return matches
}

func listCodeSymbolPrefixFileSummary(root, prefix string) error {
	idx, err := codeintel.IndexDir(root)
	if err != nil {
		return fmt.Errorf("code symbol prefix file summary: index %s: %w", root, err)
	}
	summaries := summarizeCodeSymbolPrefixFiles(root, idx, prefix)
	if len(summaries) == 0 {
		fmt.Println("No code symbols found.")
		return nil
	}
	for i := range summaries {
		fmt.Println(formatCodeSymbolFileSummary(summaries[i]))
	}
	return nil
}

func summarizeCodeSymbolPrefixFiles(root string, idx codeintel.Index, prefix string) []codeSymbolFileSummary {
	prefix = strings.TrimSpace(prefix)
	if prefix == "" {
		return nil
	}
	summaries := make([]codeSymbolFileSummary, 0)
	for i := range idx.Files {
		file := idx.Files[i]
		count := 0
		for j := range file.Symbols {
			if strings.HasPrefix(file.Symbols[j].Name, prefix) {
				count++
			}
		}
		if count == 0 {
			continue
		}
		summaries = append(summaries, codeSymbolFileSummary{
			Path:    relativeCodePath(root, file.Path),
			Package: file.Package,
			Symbols: count,
		})
	}
	sort.Slice(summaries, func(i, j int) bool {
		if summaries[i].Symbols != summaries[j].Symbols {
			return summaries[i].Symbols > summaries[j].Symbols
		}
		return summaries[i].Path < summaries[j].Path
	})
	return summaries
}

func listCodeSymbolPrefixPackageSummary(root, prefix string) error {
	idx, err := codeintel.IndexDir(root)
	if err != nil {
		return fmt.Errorf("code symbol prefix package summary: index %s: %w", root, err)
	}
	summaries := summarizeCodeSymbolPrefixPackages(idx, prefix)
	if len(summaries) == 0 {
		fmt.Println("No code symbols found.")
		return nil
	}
	for i := range summaries {
		fmt.Println(formatCodePackageSummary(summaries[i]))
	}
	return nil
}

func summarizeCodeSymbolPrefixPackages(idx codeintel.Index, prefix string) []codePackageSummary {
	prefix = strings.TrimSpace(prefix)
	if prefix == "" {
		return nil
	}
	byPackage := make(map[string]*codePackageSummary)
	filesByPackage := make(map[string]map[string]struct{})
	for i := range idx.Files {
		file := idx.Files[i]
		if file.Package == "" {
			continue
		}
		count := 0
		for j := range file.Symbols {
			if strings.HasPrefix(file.Symbols[j].Name, prefix) {
				count++
			}
		}
		if count == 0 {
			continue
		}
		summary, ok := byPackage[file.Package]
		if !ok {
			summary = &codePackageSummary{Name: file.Package}
			byPackage[file.Package] = summary
			filesByPackage[file.Package] = make(map[string]struct{})
		}
		summary.Symbols += count
		filesByPackage[file.Package][file.Path] = struct{}{}
	}
	summaries := make([]codePackageSummary, 0, len(byPackage))
	for packageName, summary := range byPackage {
		summary.Files = len(filesByPackage[packageName])
		summaries = append(summaries, *summary)
	}
	sort.Slice(summaries, func(i, j int) bool {
		if summaries[i].Symbols != summaries[j].Symbols {
			return summaries[i].Symbols > summaries[j].Symbols
		}
		if summaries[i].Files != summaries[j].Files {
			return summaries[i].Files > summaries[j].Files
		}
		return summaries[i].Name < summaries[j].Name
	})
	return summaries
}

func listCodeSymbolNameFileSummary(root, name string) error {
	idx, err := codeintel.IndexDir(root)
	if err != nil {
		return fmt.Errorf("code symbol name file summary: index %s: %w", root, err)
	}
	summaries := summarizeCodeSymbolNameFiles(root, idx, name)
	if len(summaries) == 0 {
		fmt.Println("No code symbols found.")
		return nil
	}
	for i := range summaries {
		fmt.Println(formatCodeSymbolFileSummary(summaries[i]))
	}
	return nil
}

func summarizeCodeSymbolNameFiles(root string, idx codeintel.Index, name string) []codeSymbolFileSummary {
	name = strings.TrimSpace(name)
	if name == "" {
		return nil
	}
	summaries := make([]codeSymbolFileSummary, 0)
	for i := range idx.Files {
		file := idx.Files[i]
		count := 0
		for j := range file.Symbols {
			if file.Symbols[j].Name == name {
				count++
			}
		}
		if count == 0 {
			continue
		}
		summaries = append(summaries, codeSymbolFileSummary{
			Path:    relativeCodePath(root, file.Path),
			Package: file.Package,
			Symbols: count,
		})
	}
	sort.Slice(summaries, func(i, j int) bool {
		if summaries[i].Symbols != summaries[j].Symbols {
			return summaries[i].Symbols > summaries[j].Symbols
		}
		return summaries[i].Path < summaries[j].Path
	})
	return summaries
}

func listCodeSymbolNamePackageSummary(root, name string) error {
	idx, err := codeintel.IndexDir(root)
	if err != nil {
		return fmt.Errorf("code symbol name package summary: index %s: %w", root, err)
	}
	summaries := summarizeCodeSymbolNamePackages(idx, name)
	if len(summaries) == 0 {
		fmt.Println("No code symbols found.")
		return nil
	}
	for i := range summaries {
		fmt.Println(formatCodePackageSummary(summaries[i]))
	}
	return nil
}

func summarizeCodeSymbolNamePackages(idx codeintel.Index, name string) []codePackageSummary {
	name = strings.TrimSpace(name)
	if name == "" {
		return nil
	}
	byPackage := make(map[string]*codePackageSummary)
	filesByPackage := make(map[string]map[string]struct{})
	for i := range idx.Files {
		file := idx.Files[i]
		if file.Package == "" {
			continue
		}
		count := 0
		for j := range file.Symbols {
			if file.Symbols[j].Name == name {
				count++
			}
		}
		if count == 0 {
			continue
		}
		summary, ok := byPackage[file.Package]
		if !ok {
			summary = &codePackageSummary{Name: file.Package}
			byPackage[file.Package] = summary
			filesByPackage[file.Package] = make(map[string]struct{})
		}
		summary.Symbols += count
		filesByPackage[file.Package][file.Path] = struct{}{}
	}
	summaries := make([]codePackageSummary, 0, len(byPackage))
	for packageName, summary := range byPackage {
		summary.Files = len(filesByPackage[packageName])
		summaries = append(summaries, *summary)
	}
	sort.Slice(summaries, func(i, j int) bool {
		if summaries[i].Symbols != summaries[j].Symbols {
			return summaries[i].Symbols > summaries[j].Symbols
		}
		if summaries[i].Files != summaries[j].Files {
			return summaries[i].Files > summaries[j].Files
		}
		return summaries[i].Name < summaries[j].Name
	})
	return summaries
}

func findCodeSymbolPrefix(root, prefix string) error {
	idx, err := codeintel.IndexDir(root)
	if err != nil {
		return fmt.Errorf("code symbol prefix: index %s: %w", root, err)
	}
	matches := codeSymbolsWithPrefix(idx, prefix)
	if len(matches) == 0 {
		fmt.Println("No code symbols found.")
		return nil
	}
	for i := range matches {
		fmt.Println(formatCodeSymbol(root, matches[i]))
	}
	return nil
}

func codeSymbolsWithPrefix(idx codeintel.Index, prefix string) []codeintel.Symbol {
	prefix = strings.TrimSpace(prefix)
	if prefix == "" {
		return nil
	}
	matches := make([]codeintel.Symbol, 0)
	for i := range idx.Symbols {
		if strings.HasPrefix(idx.Symbols[i].Name, prefix) {
			matches = append(matches, idx.Symbols[i])
		}
	}
	sortCodeSymbols(matches)
	return matches
}

func sortCodeSymbols(symbols []codeintel.Symbol) {
	sort.Slice(symbols, func(i, j int) bool {
		if symbols[i].Name != symbols[j].Name {
			return symbols[i].Name < symbols[j].Name
		}
		if symbols[i].File != symbols[j].File {
			return symbols[i].File < symbols[j].File
		}
		return symbols[i].Line < symbols[j].Line
	})
}

func formatCodeSymbol(root string, symbol codeintel.Symbol) string {
	path := relativeCodePath(root, symbol.File)
	return strings.Join([]string{
		symbol.Name,
		"kind=" + symbol.Kind,
		"path=" + path,
		"line=" + strconv.Itoa(symbol.Line),
	}, "\t")
}

type codeImportFileSummary struct {
	Path    string
	Package string
	Imports int
}

func listCodeImportFileSummary(root string) error {
	idx, err := codeintel.IndexDir(root)
	if err != nil {
		return fmt.Errorf("code import file summary: index %s: %w", root, err)
	}
	summaries := summarizeCodeImportFiles(root, idx)
	if len(summaries) == 0 {
		fmt.Println("No code imports found.")
		return nil
	}
	for i := range summaries {
		fmt.Println(formatCodeImportFileSummary(summaries[i]))
	}
	return nil
}

func summarizeCodeImportFiles(root string, idx codeintel.Index) []codeImportFileSummary {
	summaries := make([]codeImportFileSummary, 0)
	for i := range idx.Files {
		file := idx.Files[i]
		if len(file.Imports) == 0 {
			continue
		}
		summaries = append(summaries, codeImportFileSummary{
			Path:    relativeCodePath(root, file.Path),
			Package: file.Package,
			Imports: len(file.Imports),
		})
	}
	sort.Slice(summaries, func(i, j int) bool {
		if summaries[i].Imports != summaries[j].Imports {
			return summaries[i].Imports > summaries[j].Imports
		}
		return summaries[i].Path < summaries[j].Path
	})
	return summaries
}

func formatCodeImportFileSummary(summary codeImportFileSummary) string {
	return "path=" + summary.Path + "	package=" + summary.Package + "	imports=" + strconv.Itoa(summary.Imports)
}

type codeImportSummary struct {
	Path  string
	Files int
}

func listCodeImportSummary(root string) error {
	idx, err := codeintel.IndexDir(root)
	if err != nil {
		return fmt.Errorf("code import summary: index %s: %w", root, err)
	}
	summaries := summarizeCodeImports(idx)
	if len(summaries) == 0 {
		fmt.Println("No code imports found.")
		return nil
	}
	for i := range summaries {
		fmt.Println(formatCodeImportSummary(summaries[i]))
	}
	return nil
}

func summarizeCodeImports(idx codeintel.Index) []codeImportSummary {
	filesByImport := make(map[string]map[string]struct{})
	for i := range idx.ImportEdges {
		edge := idx.ImportEdges[i]
		if edge.Import == "" {
			continue
		}
		if _, ok := filesByImport[edge.Import]; !ok {
			filesByImport[edge.Import] = make(map[string]struct{})
		}
		filesByImport[edge.Import][edge.From] = struct{}{}
	}
	summaries := make([]codeImportSummary, 0, len(filesByImport))
	for importPath, files := range filesByImport {
		summaries = append(summaries, codeImportSummary{Path: importPath, Files: len(files)})
	}
	sort.Slice(summaries, func(i, j int) bool {
		if summaries[i].Files != summaries[j].Files {
			return summaries[i].Files > summaries[j].Files
		}
		return summaries[i].Path < summaries[j].Path
	})
	return summaries
}

func formatCodeImportSummary(summary codeImportSummary) string {
	return "import=" + summary.Path + "	files=" + strconv.Itoa(summary.Files)
}

func listCodeImportPrefixSummary(root, prefix string) error {
	idx, err := codeintel.IndexDir(root)
	if err != nil {
		return fmt.Errorf("code import prefix summary: index %s: %w", root, err)
	}
	summaries := summarizeCodeImportPrefix(idx, prefix)
	if len(summaries) == 0 {
		fmt.Println("No code imports found.")
		return nil
	}
	for i := range summaries {
		fmt.Println(formatCodeImportSummary(summaries[i]))
	}
	return nil
}

func summarizeCodeImportPrefix(idx codeintel.Index, prefix string) []codeImportSummary {
	prefix = strings.TrimSpace(prefix)
	if prefix == "" {
		return nil
	}
	filtered := codeintel.Index{ImportEdges: make([]codeintel.ImportEdge, 0)}
	for i := range idx.ImportEdges {
		if strings.HasPrefix(idx.ImportEdges[i].Import, prefix) {
			filtered.ImportEdges = append(filtered.ImportEdges, idx.ImportEdges[i])
		}
	}
	return summarizeCodeImports(filtered)
}

func listCodeImportPrefixFileSummary(root, prefix string) error {
	idx, err := codeintel.IndexDir(root)
	if err != nil {
		return fmt.Errorf("code import prefix file summary: index %s: %w", root, err)
	}
	summaries := summarizeCodeImportPrefixFiles(root, idx, prefix)
	if len(summaries) == 0 {
		fmt.Println("No code imports found.")
		return nil
	}
	for i := range summaries {
		fmt.Println(formatCodeImportFileSummary(summaries[i]))
	}
	return nil
}

func summarizeCodeImportPrefixFiles(root string, idx codeintel.Index, prefix string) []codeImportFileSummary {
	prefix = strings.TrimSpace(prefix)
	if prefix == "" {
		return nil
	}
	packagesByFile := make(map[string]string, len(idx.Files))
	for i := range idx.Files {
		packagesByFile[idx.Files[i].Path] = idx.Files[i].Package
	}
	importsByFile := make(map[string]map[string]struct{})
	for i := range idx.ImportEdges {
		edge := idx.ImportEdges[i]
		if !strings.HasPrefix(edge.Import, prefix) {
			continue
		}
		if _, ok := importsByFile[edge.From]; !ok {
			importsByFile[edge.From] = make(map[string]struct{})
		}
		importsByFile[edge.From][edge.Import] = struct{}{}
	}
	summaries := make([]codeImportFileSummary, 0, len(importsByFile))
	for file, imports := range importsByFile {
		summaries = append(summaries, codeImportFileSummary{
			Path:    relativeCodePath(root, file),
			Package: packagesByFile[file],
			Imports: len(imports),
		})
	}
	sort.Slice(summaries, func(i, j int) bool {
		if summaries[i].Imports != summaries[j].Imports {
			return summaries[i].Imports > summaries[j].Imports
		}
		return summaries[i].Path < summaries[j].Path
	})
	return summaries
}

func listCodeImportPrefixPackageSummary(root, prefix string) error {
	idx, err := codeintel.IndexDir(root)
	if err != nil {
		return fmt.Errorf("code import prefix package summary: index %s: %w", root, err)
	}
	summaries := summarizeCodeImportPrefixPackages(idx, prefix)
	if len(summaries) == 0 {
		fmt.Println("No code imports found.")
		return nil
	}
	for i := range summaries {
		fmt.Println(formatCodePackageImportMatchSummary(summaries[i]))
	}
	return nil
}

func summarizeCodeImportPrefixPackages(idx codeintel.Index, prefix string) []codePackageImportMatchSummary {
	prefix = strings.TrimSpace(prefix)
	if prefix == "" {
		return nil
	}
	packagesByFile := make(map[string]string, len(idx.Files))
	for i := range idx.Files {
		packagesByFile[idx.Files[i].Path] = idx.Files[i].Package
	}
	filesByPackage := make(map[string]map[string]struct{})
	importsByPackage := make(map[string]map[string]struct{})
	for i := range idx.ImportEdges {
		edge := idx.ImportEdges[i]
		if !strings.HasPrefix(edge.Import, prefix) {
			continue
		}
		packageName := packagesByFile[edge.From]
		if packageName == "" {
			continue
		}
		if _, ok := filesByPackage[packageName]; !ok {
			filesByPackage[packageName] = make(map[string]struct{})
			importsByPackage[packageName] = make(map[string]struct{})
		}
		filesByPackage[packageName][edge.From] = struct{}{}
		importsByPackage[packageName][edge.Import] = struct{}{}
	}
	summaries := make([]codePackageImportMatchSummary, 0, len(filesByPackage))
	for packageName, files := range filesByPackage {
		summaries = append(summaries, codePackageImportMatchSummary{
			Name:    packageName,
			Files:   len(files),
			Imports: len(importsByPackage[packageName]),
		})
	}
	sortCodePackageImportMatchSummaries(summaries)
	return summaries
}

func listCodeImportPrefix(root, prefix string) error {
	idx, err := codeintel.IndexDir(root)
	if err != nil {
		return fmt.Errorf("code import prefix: index %s: %w", root, err)
	}
	edges := codeImportEdgesWithPrefix(idx, prefix)
	if len(edges) == 0 {
		fmt.Println("No code imports found.")
		return nil
	}
	for i := range edges {
		fmt.Println(formatCodeImportEdge(root, edges[i]))
	}
	return nil
}

func codeImportEdgesWithPrefix(idx codeintel.Index, prefix string) []codeintel.ImportEdge {
	prefix = strings.TrimSpace(prefix)
	if prefix == "" {
		return nil
	}
	edges := make([]codeintel.ImportEdge, 0)
	for i := range idx.ImportEdges {
		if strings.HasPrefix(idx.ImportEdges[i].Import, prefix) {
			edges = append(edges, idx.ImportEdges[i])
		}
	}
	sortCodeImportEdges(edges)
	return edges
}

func listCodeImportPath(root, importPath string) error {
	idx, err := codeintel.IndexDir(root)
	if err != nil {
		return fmt.Errorf("code import path: index %s: %w", root, err)
	}
	edges := codeImportEdgesForPath(idx, importPath)
	if len(edges) == 0 {
		fmt.Println("No code imports found.")
		return nil
	}
	for i := range edges {
		fmt.Println(formatCodeImportEdge(root, edges[i]))
	}
	return nil
}

func codeImportEdgesForPath(idx codeintel.Index, importPath string) []codeintel.ImportEdge {
	importPath = strings.TrimSpace(importPath)
	if importPath == "" {
		return nil
	}
	edges := make([]codeintel.ImportEdge, 0)
	for i := range idx.ImportEdges {
		if idx.ImportEdges[i].Import == importPath {
			edges = append(edges, idx.ImportEdges[i])
		}
	}
	sortCodeImportEdges(edges)
	return edges
}

func listCodeImportPathSummary(root, importPath string) error {
	idx, err := codeintel.IndexDir(root)
	if err != nil {
		return fmt.Errorf("code import path summary: index %s: %w", root, err)
	}
	summaries := summarizeCodeImportPath(idx, importPath)
	if len(summaries) == 0 {
		fmt.Println("No code imports found.")
		return nil
	}
	for i := range summaries {
		fmt.Println(formatCodeImportSummary(summaries[i]))
	}
	return nil
}

func summarizeCodeImportPath(idx codeintel.Index, importPath string) []codeImportSummary {
	edges := codeImportEdgesForPath(idx, importPath)
	if len(edges) == 0 {
		return nil
	}
	return summarizeCodeImports(codeintel.Index{ImportEdges: edges})
}

func listCodeImportPathFileSummary(root, importPath string) error {
	idx, err := codeintel.IndexDir(root)
	if err != nil {
		return fmt.Errorf("code import path file summary: index %s: %w", root, err)
	}
	summaries := summarizeCodeImportPathFiles(root, idx, importPath)
	if len(summaries) == 0 {
		fmt.Println("No code imports found.")
		return nil
	}
	for i := range summaries {
		fmt.Println(formatCodeImportFileSummary(summaries[i]))
	}
	return nil
}

func summarizeCodeImportPathFiles(root string, idx codeintel.Index, importPath string) []codeImportFileSummary {
	importPath = strings.TrimSpace(importPath)
	if importPath == "" {
		return nil
	}
	packagesByFile := make(map[string]string, len(idx.Files))
	for i := range idx.Files {
		packagesByFile[idx.Files[i].Path] = idx.Files[i].Package
	}
	files := make(map[string]struct{})
	for i := range idx.ImportEdges {
		edge := idx.ImportEdges[i]
		if edge.Import == importPath {
			files[edge.From] = struct{}{}
		}
	}
	summaries := make([]codeImportFileSummary, 0, len(files))
	for file := range files {
		summaries = append(summaries, codeImportFileSummary{
			Path:    relativeCodePath(root, file),
			Package: packagesByFile[file],
			Imports: 1,
		})
	}
	sort.Slice(summaries, func(i, j int) bool {
		return summaries[i].Path < summaries[j].Path
	})
	return summaries
}

func listCodeImportPathPackageSummary(root, importPath string) error {
	idx, err := codeintel.IndexDir(root)
	if err != nil {
		return fmt.Errorf("code import path package summary: index %s: %w", root, err)
	}
	summaries := summarizeCodeImportPathPackages(idx, importPath)
	if len(summaries) == 0 {
		fmt.Println("No code imports found.")
		return nil
	}
	for i := range summaries {
		fmt.Println(formatCodePackageImportMatchSummary(summaries[i]))
	}
	return nil
}

func summarizeCodeImportPathPackages(idx codeintel.Index, importPath string) []codePackageImportMatchSummary {
	importPath = strings.TrimSpace(importPath)
	if importPath == "" {
		return nil
	}
	packagesByFile := make(map[string]string, len(idx.Files))
	for i := range idx.Files {
		packagesByFile[idx.Files[i].Path] = idx.Files[i].Package
	}
	filesByPackage := make(map[string]map[string]struct{})
	for i := range idx.ImportEdges {
		edge := idx.ImportEdges[i]
		if edge.Import != importPath {
			continue
		}
		packageName := packagesByFile[edge.From]
		if packageName == "" {
			continue
		}
		if _, ok := filesByPackage[packageName]; !ok {
			filesByPackage[packageName] = make(map[string]struct{})
		}
		filesByPackage[packageName][edge.From] = struct{}{}
	}
	summaries := make([]codePackageImportMatchSummary, 0, len(filesByPackage))
	for packageName, files := range filesByPackage {
		summaries = append(summaries, codePackageImportMatchSummary{
			Name:  packageName,
			Files: len(files),
		})
	}
	sortCodePackageImportMatchSummaries(summaries)
	return summaries
}

func sortCodeImportEdges(edges []codeintel.ImportEdge) {
	sort.Slice(edges, func(i, j int) bool {
		if edges[i].From != edges[j].From {
			return edges[i].From < edges[j].From
		}
		return edges[i].Import < edges[j].Import
	})
}

func listCodeImports(root string) error {
	idx, err := codeintel.IndexDir(root)
	if err != nil {
		return fmt.Errorf("code imports: index %s: %w", root, err)
	}
	if len(idx.ImportEdges) == 0 {
		fmt.Println("No code imports found.")
		return nil
	}
	for i := range idx.ImportEdges {
		fmt.Println(formatCodeImportEdge(root, idx.ImportEdges[i]))
	}
	return nil
}

func listCodeImpact(root, target string) error {
	graph, err := importGraph(root)
	if err != nil {
		return fmt.Errorf("code impact: %w", err)
	}
	impact := graph.ImpactSet(normalizeCodeGraphTarget(root, target))
	if len(impact) == 0 {
		fmt.Println("No code impact found.")
		return nil
	}
	for _, node := range impact {
		fmt.Println("path=" + string(node))
	}
	return nil
}

func listCodeFileImports(root, target string) error {
	idx, err := codeintel.IndexDir(root)
	if err != nil {
		return fmt.Errorf("code file imports: index %s: %w", root, err)
	}
	file, ok := findCodeFile(root, idx, target)
	if !ok {
		fmt.Println("No Go code file found.")
		return nil
	}
	imports := codeFileImports(file)
	if len(imports) == 0 {
		fmt.Println("No Go code file imports found.")
		return nil
	}
	for _, imp := range imports {
		fmt.Println("import=" + imp)
	}
	return nil
}

func codeFileImports(file codeintel.File) []string {
	if len(file.Imports) == 0 {
		return nil
	}
	imports := append([]string(nil), file.Imports...)
	sort.Strings(imports)
	return imports
}

func listCodeFileImportPath(root, spec string) error {
	target, importPath, err := parseCodeFileSymbolFilterSpec(spec, "code file import path", "path:import")
	if err != nil {
		return err
	}
	idx, err := codeintel.IndexDir(root)
	if err != nil {
		return fmt.Errorf("code file import path: index %s: %w", root, err)
	}
	file, ok := findCodeFile(root, idx, target)
	if !ok {
		fmt.Println("No Go code file found.")
		return nil
	}
	imports := codeFileImportsForPath(file, importPath)
	if len(imports) == 0 {
		fmt.Println("No Go code file imports found.")
		return nil
	}
	for _, imp := range imports {
		fmt.Println("import=" + imp)
	}
	return nil
}

func codeFileImportsForPath(file codeintel.File, importPath string) []string {
	importPath = strings.TrimSpace(importPath)
	if importPath == "" {
		return nil
	}
	imports := make([]string, 0, 1)
	for _, imp := range file.Imports {
		if imp == importPath {
			imports = append(imports, imp)
		}
	}
	sort.Strings(imports)
	return imports
}

func listCodeFileImportPrefix(root, spec string) error {
	target, prefix, err := parseCodeFileSymbolFilterSpec(spec, "code file import prefix", "path:prefix")
	if err != nil {
		return err
	}
	idx, err := codeintel.IndexDir(root)
	if err != nil {
		return fmt.Errorf("code file import prefix: index %s: %w", root, err)
	}
	file, ok := findCodeFile(root, idx, target)
	if !ok {
		fmt.Println("No Go code file found.")
		return nil
	}
	imports := codeFileImportsWithPrefix(file, prefix)
	if len(imports) == 0 {
		fmt.Println("No Go code file imports found.")
		return nil
	}
	for _, imp := range imports {
		fmt.Println("import=" + imp)
	}
	return nil
}

func codeFileImportsWithPrefix(file codeintel.File, prefix string) []string {
	prefix = strings.TrimSpace(prefix)
	if prefix == "" {
		return nil
	}
	imports := make([]string, 0)
	for _, imp := range file.Imports {
		if strings.HasPrefix(imp, prefix) {
			imports = append(imports, imp)
		}
	}
	sort.Strings(imports)
	return imports
}

func listCodeFileSymbolSummary(root, target string) error {
	idx, err := codeintel.IndexDir(root)
	if err != nil {
		return fmt.Errorf("code file symbol summary: index %s: %w", root, err)
	}
	file, ok := findCodeFile(root, idx, target)
	if !ok {
		fmt.Println("No Go code file found.")
		return nil
	}
	summaries := summarizeCodeFileSymbols(file)
	if len(summaries) == 0 {
		fmt.Println("No Go code file symbols found.")
		return nil
	}
	for i := range summaries {
		fmt.Println(formatCodeSymbolSummary(summaries[i]))
	}
	return nil
}

func summarizeCodeFileSymbols(file codeintel.File) []codeSymbolSummary {
	return summarizeCodeSymbols(codeintel.Index{Symbols: file.Symbols})
}

func listCodeFileSymbols(root, target string) error {
	idx, err := codeintel.IndexDir(root)
	if err != nil {
		return fmt.Errorf("code file symbols: index %s: %w", root, err)
	}
	file, ok := findCodeFile(root, idx, target)
	if !ok {
		fmt.Println("No Go code file found.")
		return nil
	}
	symbols := codeFileSymbols(file)
	if len(symbols) == 0 {
		fmt.Println("No Go code file symbols found.")
		return nil
	}
	for i := range symbols {
		fmt.Println(formatCodeFileSymbol(symbols[i]))
	}
	return nil
}

func codeFileSymbols(file codeintel.File) []codeintel.Symbol {
	if len(file.Symbols) == 0 {
		return nil
	}
	symbols := append([]codeintel.Symbol(nil), file.Symbols...)
	sortCodeSymbols(symbols)
	return symbols
}

func listCodeFileSymbol(root, spec string) error {
	target, name, err := parseCodeFileSymbolFilterSpec(spec, "code file symbol", "path:name")
	if err != nil {
		return err
	}
	idx, err := codeintel.IndexDir(root)
	if err != nil {
		return fmt.Errorf("code file symbol: index %s: %w", root, err)
	}
	file, ok := findCodeFile(root, idx, target)
	if !ok {
		fmt.Println("No Go code file found.")
		return nil
	}
	symbols := codeFileSymbolsByName(file, name)
	if len(symbols) == 0 {
		fmt.Println("No Go code file symbols found.")
		return nil
	}
	for i := range symbols {
		fmt.Println(formatCodeFileSymbol(symbols[i]))
	}
	return nil
}

func codeFileSymbolsByName(file codeintel.File, name string) []codeintel.Symbol {
	name = strings.TrimSpace(name)
	if name == "" {
		return nil
	}
	symbols := make([]codeintel.Symbol, 0)
	for i := range file.Symbols {
		if file.Symbols[i].Name == name {
			symbols = append(symbols, file.Symbols[i])
		}
	}
	sortCodeSymbols(symbols)
	return symbols
}

func listCodeFileSymbolPrefix(root, spec string) error {
	target, prefix, err := parseCodeFileSymbolFilterSpec(spec, "code file symbol prefix", "path:prefix")
	if err != nil {
		return err
	}
	idx, err := codeintel.IndexDir(root)
	if err != nil {
		return fmt.Errorf("code file symbol prefix: index %s: %w", root, err)
	}
	file, ok := findCodeFile(root, idx, target)
	if !ok {
		fmt.Println("No Go code file found.")
		return nil
	}
	symbols := codeFileSymbolsWithPrefix(file, prefix)
	if len(symbols) == 0 {
		fmt.Println("No Go code file symbols found.")
		return nil
	}
	for i := range symbols {
		fmt.Println(formatCodeFileSymbol(symbols[i]))
	}
	return nil
}

func codeFileSymbolsWithPrefix(file codeintel.File, prefix string) []codeintel.Symbol {
	prefix = strings.TrimSpace(prefix)
	if prefix == "" {
		return nil
	}
	symbols := make([]codeintel.Symbol, 0)
	for i := range file.Symbols {
		if strings.HasPrefix(file.Symbols[i].Name, prefix) {
			symbols = append(symbols, file.Symbols[i])
		}
	}
	sortCodeSymbols(symbols)
	return symbols
}

func listCodeFileSymbolKind(root, spec string) error {
	target, kind, err := parseCodeFileSymbolKindSpec(spec)
	if err != nil {
		return err
	}
	idx, err := codeintel.IndexDir(root)
	if err != nil {
		return fmt.Errorf("code file symbol kind: index %s: %w", root, err)
	}
	file, ok := findCodeFile(root, idx, target)
	if !ok {
		fmt.Println("No Go code file found.")
		return nil
	}
	symbols := codeFileSymbolsByKind(file, kind)
	if len(symbols) == 0 {
		fmt.Println("No Go code file symbols found.")
		return nil
	}
	for i := range symbols {
		fmt.Println(formatCodeFileSymbol(symbols[i]))
	}
	return nil
}

func parseCodeFileSymbolKindSpec(spec string) (target, kind string, err error) {
	return parseCodeFileSymbolFilterSpec(spec, "code file symbol kind", "path:kind")
}

func parseCodeFileSymbolFilterSpec(spec, label, expected string) (target, value string, err error) {
	parts := strings.SplitN(spec, ":", 2)
	if len(parts) != 2 {
		return "", "", fmt.Errorf("%s: expected %s", label, expected)
	}
	target = strings.TrimSpace(parts[0])
	value = strings.TrimSpace(parts[1])
	if target == "" || value == "" {
		return "", "", fmt.Errorf("%s: path and value are required", label)
	}
	return target, value, nil
}

func codeFileSymbolsByKind(file codeintel.File, kind string) []codeintel.Symbol {
	kind = strings.TrimSpace(kind)
	if kind == "" {
		return nil
	}
	symbols := make([]codeintel.Symbol, 0)
	for i := range file.Symbols {
		if strings.EqualFold(file.Symbols[i].Kind, kind) {
			symbols = append(symbols, file.Symbols[i])
		}
	}
	sortCodeSymbols(symbols)
	return symbols
}

func showCodeFile(root, target string) error {
	idx, err := codeintel.IndexDir(root)
	if err != nil {
		return fmt.Errorf("code file: index %s: %w", root, err)
	}
	file, ok := findCodeFile(root, idx, target)
	if !ok {
		fmt.Println("No Go code file found.")
		return nil
	}
	printCodeFile(root, file)
	return nil
}

func findCodeFile(root string, idx codeintel.Index, target string) (codeintel.File, bool) {
	target = filepath.ToSlash(strings.TrimSpace(target))
	for i := range idx.Files {
		rel := relativeCodePath(root, idx.Files[i].Path)
		abs := filepath.ToSlash(idx.Files[i].Path)
		if rel == target || abs == target {
			return idx.Files[i], true
		}
	}
	return codeintel.File{}, false
}

func printCodeFile(root string, file codeintel.File) {
	fmt.Println(formatCodeFile(root, file))
	if len(file.Imports) > 0 {
		fmt.Println("imports:")
		for _, imp := range file.Imports {
			fmt.Println("  - " + imp)
		}
	}
	if len(file.Symbols) > 0 {
		fmt.Println("symbols:")
		for i := range file.Symbols {
			fmt.Println("  - " + formatCodeFileSymbol(file.Symbols[i]))
		}
	}
}

func formatCodeFile(root string, file codeintel.File) string {
	return "path=" + relativeCodePath(root, file.Path) + "	package=" + file.Package + "	imports=" + strconv.Itoa(len(file.Imports)) + "	symbols=" + strconv.Itoa(len(file.Symbols))
}

func formatCodeFileSymbol(symbol codeintel.Symbol) string {
	return symbol.Name + "	kind=" + symbol.Kind + "	line=" + strconv.Itoa(symbol.Line)
}

func listCodePackageSymbolFileSummary(root, packageName string) error {
	idx, err := codeintel.IndexDir(root)
	if err != nil {
		return fmt.Errorf("code package symbol file summary: index %s: %w", root, err)
	}
	summaries := summarizeCodePackageSymbolFiles(root, idx, packageName)
	if len(summaries) == 0 {
		fmt.Println("No Go package symbols found.")
		return nil
	}
	for i := range summaries {
		fmt.Println(formatCodeSymbolFileSummary(summaries[i]))
	}
	return nil
}

func summarizeCodePackageSymbolFiles(root string, idx codeintel.Index, packageName string) []codeSymbolFileSummary {
	packageName = strings.TrimSpace(packageName)
	if packageName == "" {
		return nil
	}
	summaries := make([]codeSymbolFileSummary, 0)
	for i := range idx.Files {
		file := idx.Files[i]
		if file.Package != packageName || len(file.Symbols) == 0 {
			continue
		}
		summaries = append(summaries, codeSymbolFileSummary{
			Path:    relativeCodePath(root, file.Path),
			Package: file.Package,
			Symbols: len(file.Symbols),
		})
	}
	sort.Slice(summaries, func(i, j int) bool {
		if summaries[i].Symbols != summaries[j].Symbols {
			return summaries[i].Symbols > summaries[j].Symbols
		}
		return summaries[i].Path < summaries[j].Path
	})
	return summaries
}

func listCodePackageSymbol(root, spec string) error {
	packageName, name, err := parseCodeFileSymbolFilterSpec(spec, "code package symbol", "package:name")
	if err != nil {
		return err
	}
	idx, err := codeintel.IndexDir(root)
	if err != nil {
		return fmt.Errorf("code package symbol: index %s: %w", root, err)
	}
	symbols := codePackageSymbolsByName(idx, packageName, name)
	if len(symbols) == 0 {
		fmt.Println("No Go package symbols found.")
		return nil
	}
	for i := range symbols {
		fmt.Println(formatCodeSymbol(root, symbols[i]))
	}
	return nil
}

func codePackageSymbolsByName(idx codeintel.Index, packageName, name string) []codeintel.Symbol {
	symbols := codePackageSymbols(idx, packageName)
	if len(symbols) == 0 {
		return nil
	}
	name = strings.TrimSpace(name)
	if name == "" {
		return nil
	}
	filtered := make([]codeintel.Symbol, 0, len(symbols))
	for i := range symbols {
		if symbols[i].Name == name {
			filtered = append(filtered, symbols[i])
		}
	}
	return filtered
}

func listCodePackageSymbolNameFileSummary(root, spec string) error {
	packageName, name, err := parseCodeFileSymbolFilterSpec(spec, "code package symbol name file summary", "package:name")
	if err != nil {
		return err
	}
	idx, err := codeintel.IndexDir(root)
	if err != nil {
		return fmt.Errorf("code package symbol name file summary: index %s: %w", root, err)
	}
	summaries := summarizeCodePackageSymbolNameFiles(root, idx, packageName, name)
	if len(summaries) == 0 {
		fmt.Println("No Go package symbols found.")
		return nil
	}
	for i := range summaries {
		fmt.Println(formatCodeSymbolFileSummary(summaries[i]))
	}
	return nil
}

func summarizeCodePackageSymbolNameFiles(root string, idx codeintel.Index, packageName, name string) []codeSymbolFileSummary {
	packageName = strings.TrimSpace(packageName)
	name = strings.TrimSpace(name)
	if packageName == "" || name == "" {
		return nil
	}
	summaries := make([]codeSymbolFileSummary, 0)
	for i := range idx.Files {
		file := idx.Files[i]
		if file.Package != packageName {
			continue
		}
		count := 0
		for j := range file.Symbols {
			if file.Symbols[j].Name == name {
				count++
			}
		}
		if count == 0 {
			continue
		}
		summaries = append(summaries, codeSymbolFileSummary{
			Path:    relativeCodePath(root, file.Path),
			Package: file.Package,
			Symbols: count,
		})
	}
	sort.Slice(summaries, func(i, j int) bool {
		if summaries[i].Symbols != summaries[j].Symbols {
			return summaries[i].Symbols > summaries[j].Symbols
		}
		return summaries[i].Path < summaries[j].Path
	})
	return summaries
}

func listCodePackageSymbolKind(root, spec string) error {
	packageName, kind, err := parseCodePackageSymbolKindSpec(spec)
	if err != nil {
		return err
	}
	idx, err := codeintel.IndexDir(root)
	if err != nil {
		return fmt.Errorf("code package symbol kind: index %s: %w", root, err)
	}
	symbols := codePackageSymbolsByKind(idx, packageName, kind)
	if len(symbols) == 0 {
		fmt.Println("No Go package symbols found.")
		return nil
	}
	for i := range symbols {
		fmt.Println(formatCodeSymbol(root, symbols[i]))
	}
	return nil
}

func parseCodePackageSymbolKindSpec(spec string) (packageName, kind string, err error) {
	parts := strings.SplitN(spec, ":", 2)
	if len(parts) != 2 {
		return "", "", errors.New("code package symbol kind: expected package:kind")
	}
	packageName = strings.TrimSpace(parts[0])
	kind = strings.TrimSpace(parts[1])
	if packageName == "" || kind == "" {
		return "", "", errors.New("code package symbol kind: package and kind are required")
	}
	return packageName, kind, nil
}

func codePackageSymbolsByKind(idx codeintel.Index, packageName, kind string) []codeintel.Symbol {
	symbols := codePackageSymbols(idx, packageName)
	if len(symbols) == 0 {
		return nil
	}
	kind = strings.TrimSpace(kind)
	if kind == "" {
		return nil
	}
	filtered := make([]codeintel.Symbol, 0, len(symbols))
	for i := range symbols {
		if strings.EqualFold(symbols[i].Kind, kind) {
			filtered = append(filtered, symbols[i])
		}
	}
	return filtered
}

func listCodePackageSymbolPrefixFileSummary(root, spec string) error {
	packageName, prefix, err := parseCodeFileSymbolFilterSpec(spec, "code package symbol prefix file summary", "package:prefix")
	if err != nil {
		return err
	}
	idx, err := codeintel.IndexDir(root)
	if err != nil {
		return fmt.Errorf("code package symbol prefix file summary: index %s: %w", root, err)
	}
	summaries := summarizeCodePackageSymbolPrefixFiles(root, idx, packageName, prefix)
	if len(summaries) == 0 {
		fmt.Println("No Go package symbols found.")
		return nil
	}
	for i := range summaries {
		fmt.Println(formatCodeSymbolFileSummary(summaries[i]))
	}
	return nil
}

func summarizeCodePackageSymbolPrefixFiles(root string, idx codeintel.Index, packageName, prefix string) []codeSymbolFileSummary {
	packageName = strings.TrimSpace(packageName)
	prefix = strings.TrimSpace(prefix)
	if packageName == "" || prefix == "" {
		return nil
	}
	summaries := make([]codeSymbolFileSummary, 0)
	for i := range idx.Files {
		file := idx.Files[i]
		if file.Package != packageName {
			continue
		}
		count := 0
		for j := range file.Symbols {
			if strings.HasPrefix(file.Symbols[j].Name, prefix) {
				count++
			}
		}
		if count == 0 {
			continue
		}
		summaries = append(summaries, codeSymbolFileSummary{
			Path:    relativeCodePath(root, file.Path),
			Package: file.Package,
			Symbols: count,
		})
	}
	sort.Slice(summaries, func(i, j int) bool {
		if summaries[i].Symbols != summaries[j].Symbols {
			return summaries[i].Symbols > summaries[j].Symbols
		}
		return summaries[i].Path < summaries[j].Path
	})
	return summaries
}

func listCodePackageSymbolKindFileSummary(root, spec string) error {
	packageName, kind, err := parseCodePackageSymbolKindSpec(spec)
	if err != nil {
		return err
	}
	idx, err := codeintel.IndexDir(root)
	if err != nil {
		return fmt.Errorf("code package symbol kind file summary: index %s: %w", root, err)
	}
	summaries := summarizeCodePackageSymbolKindFiles(root, idx, packageName, kind)
	if len(summaries) == 0 {
		fmt.Println("No Go package symbols found.")
		return nil
	}
	for i := range summaries {
		fmt.Println(formatCodeSymbolFileSummary(summaries[i]))
	}
	return nil
}

func summarizeCodePackageSymbolKindFiles(root string, idx codeintel.Index, packageName, kind string) []codeSymbolFileSummary {
	packageName = strings.TrimSpace(packageName)
	kind = strings.TrimSpace(kind)
	if packageName == "" || kind == "" {
		return nil
	}
	summaries := make([]codeSymbolFileSummary, 0)
	for i := range idx.Files {
		file := idx.Files[i]
		if file.Package != packageName {
			continue
		}
		count := 0
		for j := range file.Symbols {
			if strings.EqualFold(file.Symbols[j].Kind, kind) {
				count++
			}
		}
		if count == 0 {
			continue
		}
		summaries = append(summaries, codeSymbolFileSummary{
			Path:    relativeCodePath(root, file.Path),
			Package: file.Package,
			Symbols: count,
		})
	}
	sort.Slice(summaries, func(i, j int) bool {
		if summaries[i].Symbols != summaries[j].Symbols {
			return summaries[i].Symbols > summaries[j].Symbols
		}
		return summaries[i].Path < summaries[j].Path
	})
	return summaries
}

func listCodePackageSymbolPrefix(root, spec string) error {
	packageName, prefix, err := parseCodeFileSymbolFilterSpec(spec, "code package symbol prefix", "package:prefix")
	if err != nil {
		return err
	}
	idx, err := codeintel.IndexDir(root)
	if err != nil {
		return fmt.Errorf("code package symbol prefix: index %s: %w", root, err)
	}
	symbols := codePackageSymbolsWithPrefix(idx, packageName, prefix)
	if len(symbols) == 0 {
		fmt.Println("No Go package symbols found.")
		return nil
	}
	for i := range symbols {
		fmt.Println(formatCodeSymbol(root, symbols[i]))
	}
	return nil
}

func codePackageSymbolsWithPrefix(idx codeintel.Index, packageName, prefix string) []codeintel.Symbol {
	symbols := codePackageSymbols(idx, packageName)
	if len(symbols) == 0 {
		return nil
	}
	prefix = strings.TrimSpace(prefix)
	if prefix == "" {
		return nil
	}
	filtered := make([]codeintel.Symbol, 0, len(symbols))
	for i := range symbols {
		if strings.HasPrefix(symbols[i].Name, prefix) {
			filtered = append(filtered, symbols[i])
		}
	}
	return filtered
}

func listCodePackageSymbolList(root, packageName string) error {
	idx, err := codeintel.IndexDir(root)
	if err != nil {
		return fmt.Errorf("code package symbol list: index %s: %w", root, err)
	}
	symbols := codePackageSymbols(idx, packageName)
	if len(symbols) == 0 {
		fmt.Println("No Go package symbols found.")
		return nil
	}
	for i := range symbols {
		fmt.Println(formatCodeSymbol(root, symbols[i]))
	}
	return nil
}

func codePackageSymbols(idx codeintel.Index, packageName string) []codeintel.Symbol {
	packageName = strings.TrimSpace(packageName)
	if packageName == "" {
		return nil
	}
	symbols := make([]codeintel.Symbol, 0)
	for i := range idx.Files {
		if idx.Files[i].Package == packageName {
			symbols = append(symbols, idx.Files[i].Symbols...)
		}
	}
	sortCodeSymbols(symbols)
	return symbols
}

func listCodePackageSymbols(root, packageName string) error {
	idx, err := codeintel.IndexDir(root)
	if err != nil {
		return fmt.Errorf("code package symbols: index %s: %w", root, err)
	}
	summaries := summarizeCodePackageSymbols(idx, packageName)
	if len(summaries) == 0 {
		fmt.Println("No Go package symbols found.")
		return nil
	}
	for i := range summaries {
		fmt.Println(formatCodeSymbolSummary(summaries[i]))
	}
	return nil
}

func summarizeCodePackageSymbols(idx codeintel.Index, packageName string) []codeSymbolSummary {
	packageName = strings.TrimSpace(packageName)
	if packageName == "" {
		return nil
	}
	filtered := codeintel.Index{Symbols: make([]codeintel.Symbol, 0)}
	for i := range idx.Files {
		if idx.Files[i].Package == packageName {
			filtered.Symbols = append(filtered.Symbols, idx.Files[i].Symbols...)
		}
	}
	return summarizeCodeSymbols(filtered)
}

func listCodePackageImportFileSummary(root, packageName string) error {
	idx, err := codeintel.IndexDir(root)
	if err != nil {
		return fmt.Errorf("code package import file summary: index %s: %w", root, err)
	}
	summaries := summarizeCodePackageImportFiles(root, idx, packageName)
	if len(summaries) == 0 {
		fmt.Println("No Go package imports found.")
		return nil
	}
	for i := range summaries {
		fmt.Println(formatCodeImportFileSummary(summaries[i]))
	}
	return nil
}

func summarizeCodePackageImportFiles(root string, idx codeintel.Index, packageName string) []codeImportFileSummary {
	packageName = strings.TrimSpace(packageName)
	if packageName == "" {
		return nil
	}
	summaries := make([]codeImportFileSummary, 0)
	for i := range idx.Files {
		file := idx.Files[i]
		if file.Package != packageName || len(file.Imports) == 0 {
			continue
		}
		summaries = append(summaries, codeImportFileSummary{
			Path:    relativeCodePath(root, file.Path),
			Package: file.Package,
			Imports: len(file.Imports),
		})
	}
	sort.Slice(summaries, func(i, j int) bool {
		if summaries[i].Imports != summaries[j].Imports {
			return summaries[i].Imports > summaries[j].Imports
		}
		return summaries[i].Path < summaries[j].Path
	})
	return summaries
}

func listCodePackageImportPrefixFileSummary(root, spec string) error {
	packageName, prefix, err := parseCodeFileSymbolFilterSpec(spec, "code package import prefix file summary", "package:prefix")
	if err != nil {
		return err
	}
	idx, err := codeintel.IndexDir(root)
	if err != nil {
		return fmt.Errorf("code package import prefix file summary: index %s: %w", root, err)
	}
	summaries := summarizeCodePackageImportPrefixFiles(root, idx, packageName, prefix)
	if len(summaries) == 0 {
		fmt.Println("No Go package import files found.")
		return nil
	}
	for i := range summaries {
		fmt.Println(formatCodeImportFileSummary(summaries[i]))
	}
	return nil
}

func summarizeCodePackageImportPrefixFiles(root string, idx codeintel.Index, packageName, prefix string) []codeImportFileSummary {
	edges := codePackageImportPrefixFiles(idx, packageName, prefix)
	if len(edges) == 0 {
		return edgesToCodeImportFileSummaries(root, idx, nil)
	}
	return edgesToCodeImportFileSummaries(root, idx, edges)
}

func edgesToCodeImportFileSummaries(root string, idx codeintel.Index, edges []codeintel.ImportEdge) []codeImportFileSummary {
	if len(edges) == 0 {
		return nil
	}
	packagesByFile := make(map[string]string, len(idx.Files))
	for i := range idx.Files {
		packagesByFile[idx.Files[i].Path] = idx.Files[i].Package
	}
	importsByFile := make(map[string]map[string]struct{})
	for i := range edges {
		edge := edges[i]
		if _, ok := importsByFile[edge.From]; !ok {
			importsByFile[edge.From] = make(map[string]struct{})
		}
		importsByFile[edge.From][edge.Import] = struct{}{}
	}
	summaries := make([]codeImportFileSummary, 0, len(importsByFile))
	for file, imports := range importsByFile {
		summaries = append(summaries, codeImportFileSummary{
			Path:    relativeCodePath(root, file),
			Package: packagesByFile[file],
			Imports: len(imports),
		})
	}
	sort.Slice(summaries, func(i, j int) bool {
		if summaries[i].Imports != summaries[j].Imports {
			return summaries[i].Imports > summaries[j].Imports
		}
		return summaries[i].Path < summaries[j].Path
	})
	return summaries
}

func listCodePackageImportPrefixFiles(root, spec string) error {
	packageName, prefix, err := parseCodeFileSymbolFilterSpec(spec, "code package import prefix files", "package:prefix")
	if err != nil {
		return err
	}
	idx, err := codeintel.IndexDir(root)
	if err != nil {
		return fmt.Errorf("code package import prefix files: index %s: %w", root, err)
	}
	edges := codePackageImportPrefixFiles(idx, packageName, prefix)
	if len(edges) == 0 {
		fmt.Println("No Go package import files found.")
		return nil
	}
	for i := range edges {
		fmt.Println(formatCodeImportEdge(root, edges[i]))
	}
	return nil
}

func codePackageImportPrefixFiles(idx codeintel.Index, packageName, prefix string) []codeintel.ImportEdge {
	packageName = strings.TrimSpace(packageName)
	prefix = strings.TrimSpace(prefix)
	if packageName == "" || prefix == "" {
		return nil
	}
	packageFiles := make(map[string]struct{})
	for i := range idx.Files {
		if idx.Files[i].Package == packageName {
			packageFiles[idx.Files[i].Path] = struct{}{}
		}
	}
	if len(packageFiles) == 0 {
		return nil
	}
	seen := make(map[string]struct{})
	edges := make([]codeintel.ImportEdge, 0)
	for i := range idx.ImportEdges {
		edge := idx.ImportEdges[i]
		if !strings.HasPrefix(edge.Import, prefix) {
			continue
		}
		if _, ok := packageFiles[edge.From]; !ok {
			continue
		}
		seenKey := edge.From + "\x00" + edge.Import
		if _, ok := seen[seenKey]; ok {
			continue
		}
		seen[seenKey] = struct{}{}
		edges = append(edges, edge)
	}
	sortCodeImportEdges(edges)
	return edges
}

func listCodePackageImportFiles(root, spec string) error {
	packageName, importPath, err := parseCodeFileSymbolFilterSpec(spec, "code package import files", "package:import")
	if err != nil {
		return err
	}
	idx, err := codeintel.IndexDir(root)
	if err != nil {
		return fmt.Errorf("code package import files: index %s: %w", root, err)
	}
	files := codePackageImportFiles(root, idx, packageName, importPath)
	if len(files) == 0 {
		fmt.Println("No Go package import files found.")
		return nil
	}
	for _, file := range files {
		fmt.Println("path=" + file + "	import=" + importPath)
	}
	return nil
}

func codePackageImportFiles(root string, idx codeintel.Index, packageName, importPath string) []string {
	packageName = strings.TrimSpace(packageName)
	importPath = strings.TrimSpace(importPath)
	if packageName == "" || importPath == "" {
		return nil
	}
	packageFiles := make(map[string]struct{})
	for i := range idx.Files {
		if idx.Files[i].Package == packageName {
			packageFiles[idx.Files[i].Path] = struct{}{}
		}
	}
	if len(packageFiles) == 0 {
		return nil
	}
	seen := make(map[string]struct{})
	files := make([]string, 0)
	for i := range idx.ImportEdges {
		edge := idx.ImportEdges[i]
		if edge.Import != importPath {
			continue
		}
		if _, ok := packageFiles[edge.From]; !ok {
			continue
		}
		rel := relativeCodePath(root, edge.From)
		if _, ok := seen[rel]; ok {
			continue
		}
		seen[rel] = struct{}{}
		files = append(files, rel)
	}
	sort.Strings(files)
	return files
}

func listCodePackageImportPathFileSummary(root, spec string) error {
	packageName, importPath, err := parseCodeFileSymbolFilterSpec(spec, "code package import path file summary", "package:import")
	if err != nil {
		return err
	}
	idx, err := codeintel.IndexDir(root)
	if err != nil {
		return fmt.Errorf("code package import path file summary: index %s: %w", root, err)
	}
	summaries := summarizeCodePackageImportPathFiles(root, idx, packageName, importPath)
	if len(summaries) == 0 {
		fmt.Println("No Go package import files found.")
		return nil
	}
	for i := range summaries {
		fmt.Println(formatCodeImportFileSummary(summaries[i]))
	}
	return nil
}

func summarizeCodePackageImportPathFiles(root string, idx codeintel.Index, packageName, importPath string) []codeImportFileSummary {
	packageName = strings.TrimSpace(packageName)
	importPath = strings.TrimSpace(importPath)
	if packageName == "" || importPath == "" {
		return nil
	}
	packageFiles := make(map[string]struct{})
	for i := range idx.Files {
		if idx.Files[i].Package == packageName {
			packageFiles[idx.Files[i].Path] = struct{}{}
		}
	}
	if len(packageFiles) == 0 {
		return nil
	}
	files := make(map[string]struct{})
	for i := range idx.ImportEdges {
		edge := idx.ImportEdges[i]
		if edge.Import != importPath {
			continue
		}
		if _, ok := packageFiles[edge.From]; !ok {
			continue
		}
		files[edge.From] = struct{}{}
	}
	summaries := make([]codeImportFileSummary, 0, len(files))
	for file := range files {
		summaries = append(summaries, codeImportFileSummary{
			Path:    relativeCodePath(root, file),
			Package: packageName,
			Imports: 1,
		})
	}
	sort.Slice(summaries, func(i, j int) bool {
		return summaries[i].Path < summaries[j].Path
	})
	return summaries
}

func listCodePackageImportPath(root, spec string) error {
	packageName, importPath, err := parseCodeFileSymbolFilterSpec(spec, "code package import path", "package:import")
	if err != nil {
		return err
	}
	idx, err := codeintel.IndexDir(root)
	if err != nil {
		return fmt.Errorf("code package import path: index %s: %w", root, err)
	}
	summaries := summarizeCodePackageImportPath(idx, packageName, importPath)
	if len(summaries) == 0 {
		fmt.Println("No Go package imports found.")
		return nil
	}
	for i := range summaries {
		fmt.Println(formatCodeImportSummary(summaries[i]))
	}
	return nil
}

func summarizeCodePackageImportPath(idx codeintel.Index, packageName, importPath string) []codeImportSummary {
	packageName = strings.TrimSpace(packageName)
	importPath = strings.TrimSpace(importPath)
	if packageName == "" || importPath == "" {
		return nil
	}
	all := summarizeCodePackageImports(idx, packageName)
	filtered := make([]codeImportSummary, 0, 1)
	for i := range all {
		if all[i].Path == importPath {
			filtered = append(filtered, all[i])
		}
	}
	return filtered
}

func listCodePackageImportPrefix(root, spec string) error {
	packageName, prefix, err := parseCodeFileSymbolFilterSpec(spec, "code package import prefix", "package:prefix")
	if err != nil {
		return err
	}
	idx, err := codeintel.IndexDir(root)
	if err != nil {
		return fmt.Errorf("code package import prefix: index %s: %w", root, err)
	}
	summaries := summarizeCodePackageImportPrefix(idx, packageName, prefix)
	if len(summaries) == 0 {
		fmt.Println("No Go package imports found.")
		return nil
	}
	for i := range summaries {
		fmt.Println(formatCodeImportSummary(summaries[i]))
	}
	return nil
}

func summarizeCodePackageImportPrefix(idx codeintel.Index, packageName, prefix string) []codeImportSummary {
	packageName = strings.TrimSpace(packageName)
	prefix = strings.TrimSpace(prefix)
	if packageName == "" || prefix == "" {
		return nil
	}
	all := summarizeCodePackageImports(idx, packageName)
	filtered := make([]codeImportSummary, 0, len(all))
	for i := range all {
		if strings.HasPrefix(all[i].Path, prefix) {
			filtered = append(filtered, all[i])
		}
	}
	return filtered
}

func listCodePackageImports(root, packageName string) error {
	idx, err := codeintel.IndexDir(root)
	if err != nil {
		return fmt.Errorf("code package imports: index %s: %w", root, err)
	}
	summaries := summarizeCodePackageImports(idx, packageName)
	if len(summaries) == 0 {
		fmt.Println("No Go package imports found.")
		return nil
	}
	for i := range summaries {
		fmt.Println(formatCodeImportSummary(summaries[i]))
	}
	return nil
}

func summarizeCodePackageImports(idx codeintel.Index, packageName string) []codeImportSummary {
	packageName = strings.TrimSpace(packageName)
	if packageName == "" {
		return nil
	}
	files := make(map[string]struct{})
	for i := range idx.Files {
		if idx.Files[i].Package == packageName {
			files[idx.Files[i].Path] = struct{}{}
		}
	}
	if len(files) == 0 {
		return nil
	}
	filtered := codeintel.Index{ImportEdges: make([]codeintel.ImportEdge, 0)}
	for i := range idx.ImportEdges {
		if _, ok := files[idx.ImportEdges[i].From]; ok {
			filtered.ImportEdges = append(filtered.ImportEdges, idx.ImportEdges[i])
		}
	}
	return summarizeCodeImports(filtered)
}

func listCodeFiles(root string) error {
	idx, err := codeintel.IndexDir(root)
	if err != nil {
		return fmt.Errorf("code files: index %s: %w", root, err)
	}
	files := summarizeCodeFiles(root, idx)
	if len(files) == 0 {
		fmt.Println("No Go files found.")
		return nil
	}
	for i := range files {
		fmt.Println(formatCodePackageFile(files[i]))
	}
	return nil
}

func summarizeCodeFiles(root string, idx codeintel.Index) []codePackageFile {
	files := make([]codePackageFile, 0, len(idx.Files))
	for i := range idx.Files {
		file := idx.Files[i]
		files = append(files, codePackageFile{
			Path:    relativeCodePath(root, file.Path),
			Package: file.Package,
			Symbols: len(file.Symbols),
			Imports: len(file.Imports),
		})
	}
	sort.Slice(files, func(i, j int) bool { return files[i].Path < files[j].Path })
	return files
}

func listCodePackageFiles(root, name string) error {
	idx, err := codeintel.IndexDir(root)
	if err != nil {
		return fmt.Errorf("code package: index %s: %w", root, err)
	}
	files := summarizeCodePackageFiles(root, idx, name)
	if len(files) == 0 {
		fmt.Println("No Go package files found.")
		return nil
	}
	for i := range files {
		fmt.Println(formatCodePackageFile(files[i]))
	}
	return nil
}

type codePackageFile struct {
	Path    string
	Package string
	Symbols int
	Imports int
}

func summarizeCodePackageFiles(root string, idx codeintel.Index, name string) []codePackageFile {
	name = strings.TrimSpace(name)
	files := make([]codePackageFile, 0)
	for i := range idx.Files {
		file := idx.Files[i]
		if file.Package != name {
			continue
		}
		files = append(files, codePackageFile{
			Path:    relativeCodePath(root, file.Path),
			Package: file.Package,
			Symbols: len(file.Symbols),
			Imports: len(file.Imports),
		})
	}
	sort.Slice(files, func(i, j int) bool { return files[i].Path < files[j].Path })
	return files
}

func formatCodePackageFile(file codePackageFile) string {
	return "path=" + file.Path + "	package=" + file.Package + "	symbols=" + strconv.Itoa(file.Symbols) + "	imports=" + strconv.Itoa(file.Imports)
}

type codePackageImportSummary struct {
	Name          string
	Files         int
	Imports       int
	UniqueImports int
}

type codePackageImportMatchSummary struct {
	Name    string
	Files   int
	Imports int
}

func formatCodePackageImportMatchSummary(summary codePackageImportMatchSummary) string {
	parts := []string{
		"package=" + summary.Name,
		"files=" + strconv.Itoa(summary.Files),
	}
	if summary.Imports > 0 {
		parts = append(parts, "imports="+strconv.Itoa(summary.Imports))
	}
	return strings.Join(parts, "\t")
}

func sortCodePackageImportMatchSummaries(summaries []codePackageImportMatchSummary) {
	sort.Slice(summaries, func(i, j int) bool {
		if summaries[i].Imports != summaries[j].Imports {
			return summaries[i].Imports > summaries[j].Imports
		}
		if summaries[i].Files != summaries[j].Files {
			return summaries[i].Files > summaries[j].Files
		}
		return summaries[i].Name < summaries[j].Name
	})
}

func listCodePackageImportSummary(root string) error {
	idx, err := codeintel.IndexDir(root)
	if err != nil {
		return fmt.Errorf("code package import summary: index %s: %w", root, err)
	}
	summaries := summarizeCodePackageImportCounts(idx)
	if len(summaries) == 0 {
		fmt.Println("No Go package imports found.")
		return nil
	}
	for i := range summaries {
		fmt.Println(formatCodePackageImportSummary(summaries[i]))
	}
	return nil
}

func summarizeCodePackageImportCounts(idx codeintel.Index) []codePackageImportSummary {
	byPackage := make(map[string]*codePackageImportSummary)
	uniqueByPackage := make(map[string]map[string]struct{})
	for i := range idx.Files {
		file := idx.Files[i]
		if file.Package == "" {
			continue
		}
		summary, ok := byPackage[file.Package]
		if !ok {
			summary = &codePackageImportSummary{Name: file.Package}
			byPackage[file.Package] = summary
			uniqueByPackage[file.Package] = make(map[string]struct{})
		}
		summary.Files++
		summary.Imports += len(file.Imports)
		for _, imp := range file.Imports {
			if imp != "" {
				uniqueByPackage[file.Package][imp] = struct{}{}
			}
		}
	}
	summaries := make([]codePackageImportSummary, 0, len(byPackage))
	for name, summary := range byPackage {
		summary.UniqueImports = len(uniqueByPackage[name])
		if summary.Imports == 0 {
			continue
		}
		summaries = append(summaries, *summary)
	}
	sort.Slice(summaries, func(i, j int) bool {
		if summaries[i].Imports != summaries[j].Imports {
			return summaries[i].Imports > summaries[j].Imports
		}
		return summaries[i].Name < summaries[j].Name
	})
	return summaries
}

func formatCodePackageImportSummary(summary codePackageImportSummary) string {
	return "package=" + summary.Name + "	files=" + strconv.Itoa(summary.Files) + "	imports=" + strconv.Itoa(summary.Imports) + "	unique_imports=" + strconv.Itoa(summary.UniqueImports)
}

type codePackageSummary struct {
	Name    string
	Files   int
	Symbols int
}

func listCodePackages(root string) error {
	idx, err := codeintel.IndexDir(root)
	if err != nil {
		return fmt.Errorf("code packages: index %s: %w", root, err)
	}
	packages := summarizeCodePackages(idx)
	if len(packages) == 0 {
		fmt.Println("No Go packages found.")
		return nil
	}
	for i := range packages {
		fmt.Println(formatCodePackageSummary(packages[i]))
	}
	return nil
}

func summarizeCodePackages(idx codeintel.Index) []codePackageSummary {
	byPackage := make(map[string]*codePackageSummary)
	for i := range idx.Files {
		name := idx.Files[i].Package
		if name == "" {
			continue
		}
		summary, ok := byPackage[name]
		if !ok {
			summary = &codePackageSummary{Name: name}
			byPackage[name] = summary
		}
		summary.Files++
		summary.Symbols += len(idx.Files[i].Symbols)
	}
	packages := make([]codePackageSummary, 0, len(byPackage))
	for _, summary := range byPackage {
		packages = append(packages, *summary)
	}
	sort.Slice(packages, func(i, j int) bool {
		if packages[i].Name != packages[j].Name {
			return packages[i].Name < packages[j].Name
		}
		return packages[i].Files < packages[j].Files
	})
	return packages
}

func formatCodePackageSummary(summary codePackageSummary) string {
	return "package=" + summary.Name + "	files=" + strconv.Itoa(summary.Files) + "	symbols=" + strconv.Itoa(summary.Symbols)
}

type codeSummary struct {
	Files    int
	Packages int
	Symbols  int
	Imports  int
	Nodes    int
	Edges    int
	Cycles   int
	Layers   int
}

func printCodeSummary(root string) error {
	idx, err := codeintel.IndexDir(root)
	if err != nil {
		return fmt.Errorf("code summary: index %s: %w", root, err)
	}
	graph := importGraphFromIndex(root, idx)
	layers, layerErr := graph.TopologicalLayers()
	summary := codeSummary{
		Files:    len(idx.Files),
		Packages: countPackages(idx.Files),
		Symbols:  len(idx.Symbols),
		Imports:  len(idx.ImportEdges),
		Nodes:    len(graph.Nodes()),
		Edges:    len(graph.Edges()),
		Cycles:   len(graph.Cycles()),
	}
	if layerErr == nil {
		summary.Layers = len(layers)
	}
	fmt.Println(formatCodeSummary(summary))
	return nil
}

func countPackages(files []codeintel.File) int {
	seen := make(map[string]struct{})
	for i := range files {
		if files[i].Package != "" {
			seen[files[i].Package] = struct{}{}
		}
	}
	return len(seen)
}

func formatCodeSummary(summary codeSummary) string {
	return strings.Join([]string{
		"files=" + strconv.Itoa(summary.Files),
		"packages=" + strconv.Itoa(summary.Packages),
		"symbols=" + strconv.Itoa(summary.Symbols),
		"imports=" + strconv.Itoa(summary.Imports),
		"nodes=" + strconv.Itoa(summary.Nodes),
		"edges=" + strconv.Itoa(summary.Edges),
		"cycles=" + strconv.Itoa(summary.Cycles),
		"layers=" + strconv.Itoa(summary.Layers),
	}, "	")
}

func listCodeCycles(root string) error {
	graph, err := importGraph(root)
	if err != nil {
		return fmt.Errorf("code cycles: %w", err)
	}
	cycles := graph.Cycles()
	if len(cycles) == 0 {
		fmt.Println("No code graph cycles found.")
		return nil
	}
	for i := range cycles {
		fmt.Println(formatCodeCycle(i+1, cycles[i]))
	}
	return nil
}

func formatCodeCycle(index int, cycle []codegraph.NodeID) string {
	labels := make([]string, 0, len(cycle))
	for _, node := range cycle {
		labels = append(labels, string(node))
	}
	return "cycle=" + strconv.Itoa(index) + "	nodes=" + strings.Join(labels, " -> ")
}

func listCodeLayers(root string) error {
	graph, err := importGraph(root)
	if err != nil {
		return fmt.Errorf("code layers: %w", err)
	}
	layers, err := graph.TopologicalLayers()
	if err != nil {
		return fmt.Errorf("code layers: %w", err)
	}
	if len(layers) == 0 {
		fmt.Println("No code graph layers found.")
		return nil
	}
	for i := range layers {
		fmt.Println(formatCodeLayer(i+1, layers[i]))
	}
	return nil
}

func formatCodeLayer(index int, nodes []codegraph.NodeID) string {
	labels := make([]string, 0, len(nodes))
	for _, node := range nodes {
		labels = append(labels, string(node))
	}
	return "layer=" + strconv.Itoa(index) + "	nodes=" + strings.Join(labels, ",")
}

func listCodeDeps(root, target string) error {
	graph, err := importGraph(root)
	if err != nil {
		return fmt.Errorf("code deps: %w", err)
	}
	deps := codeGraphDependencies(graph, root, target)
	if len(deps) == 0 {
		fmt.Println("No direct code dependencies found.")
		return nil
	}
	for _, node := range deps {
		fmt.Println("node=" + string(node))
	}
	return nil
}

func listCodeReverseDeps(root, target string) error {
	graph, err := importGraph(root)
	if err != nil {
		return fmt.Errorf("code rdeps: %w", err)
	}
	rdeps := codeGraphReverseDependencies(graph, root, target)
	if len(rdeps) == 0 {
		fmt.Println("No direct code reverse dependencies found.")
		return nil
	}
	for _, node := range rdeps {
		fmt.Println("node=" + string(node))
	}
	return nil
}

func codeGraphDependencies(graph *codegraph.Graph, root, target string) []codegraph.NodeID {
	return graph.Neighbors(normalizeCodeGraphTarget(root, target))
}

func codeGraphReverseDependencies(graph *codegraph.Graph, root, target string) []codegraph.NodeID {
	return graph.ReverseDependencies(normalizeCodeGraphTarget(root, target))
}

func listCodeReachable(root, target string) error {
	graph, err := importGraph(root)
	if err != nil {
		return fmt.Errorf("code reachable: %w", err)
	}
	reachable := graph.ReachableFrom(normalizeCodeGraphTarget(root, target))
	if len(reachable) == 0 {
		fmt.Println("No reachable code graph nodes found.")
		return nil
	}
	for _, node := range reachable {
		fmt.Println("node=" + string(node))
	}
	return nil
}

func importGraph(root string) (*codegraph.Graph, error) {
	idx, err := codeintel.IndexDir(root)
	if err != nil {
		return nil, fmt.Errorf("index %s: %w", root, err)
	}
	return importGraphFromIndex(root, idx), nil
}

func importGraphFromIndex(root string, idx codeintel.Index) *codegraph.Graph {
	graph := codegraph.New()
	for i := range idx.Files {
		graph.AddNode(codegraph.NodeID(relativeCodePath(root, idx.Files[i].Path)))
	}
	for i := range idx.ImportEdges {
		edge := idx.ImportEdges[i]
		from := codegraph.NodeID(relativeCodePath(root, edge.From))
		graph.AddEdge(from, codegraph.NodeID(edge.Import))
	}
	return graph
}

func normalizeCodeGraphTarget(root, target string) codegraph.NodeID {
	target = strings.TrimSpace(target)
	if target == "" {
		return ""
	}
	if filepath.IsAbs(target) {
		return codegraph.NodeID(relativeCodePath(root, target))
	}
	return codegraph.NodeID(filepath.ToSlash(target))
}

func relativeCodePath(root, path string) string {
	if relativePath, err := filepath.Rel(root, path); err == nil {
		return filepath.ToSlash(relativePath)
	}
	return filepath.ToSlash(path)
}

func formatCodeImportEdge(root string, edge codeintel.ImportEdge) string {
	path := relativeCodePath(root, edge.From)
	return "path=" + path + "\timport=" + edge.Import
}

func evalOutput(actualPath, expectedText, expectedPath string, mode atteval.MatchMode) error {
	actual, err := os.ReadFile(actualPath)
	if err != nil {
		return fmt.Errorf("eval output: read actual %s: %w", actualPath, err)
	}
	expected, err := expectedEvalText(expectedText, expectedPath)
	if err != nil {
		return err
	}
	result := atteval.Check(string(actual), expected, mode)
	if result.Passed {
		fmt.Printf("PASS\tmode=%s\tactual=%s\n", result.Mode, actualPath)
		return nil
	}
	report := result.Failure()
	if report == "" {
		report = result.Summary
	}
	fmt.Printf("FAIL\tmode=%s\tactual=%s\n%s\n", result.Mode, actualPath, report)
	return errors.New("eval output failed")
}

func expectedEvalText(expectedText, expectedPath string) (string, error) {
	switch {
	case expectedText != "" && expectedPath != "":
		return "", errors.New("eval output: pass either --eval-expected or --eval-expected-file, not both")
	case expectedText != "":
		return expectedText, nil
	case expectedPath != "":
		data, err := os.ReadFile(expectedPath)
		if err != nil {
			return "", fmt.Errorf("eval output: read expected %s: %w", expectedPath, err)
		}
		return string(data), nil
	default:
		return "", errors.New("eval output: expected text is required")
	}
}

func suggestSkill(steps []string, maxSteps, minOccurrences int, saveDir string) error {
	suggestion, ok := attskill.SuggestWithOptions(steps, attskill.Options{
		MaxSteps:       maxSteps,
		MinOccurrences: minOccurrences,
	})
	if !ok {
		fmt.Println("No repeated multi-step skill candidate found.")
		return nil
	}
	fmt.Print(formatSkillSuggestion(suggestion))
	if strings.TrimSpace(saveDir) == "" {
		return nil
	}
	path, err := attskill.PersistSuggestion(saveDir, suggestion)
	if err != nil {
		return fmt.Errorf("save skill suggestion: %w", err)
	}
	fmt.Println("saved: " + path)
	return nil
}

func formatSkillSuggestion(suggestion attskill.Suggestion) string {
	var b strings.Builder
	fmt.Fprintf(&b, "name: %s\n", suggestion.Name)
	fmt.Fprintf(&b, "slug: %s\n", suggestion.Slug)
	fmt.Fprintf(&b, "occurrences: %d\n", suggestion.Occurrences)
	b.WriteString("steps:\n")
	for _, step := range suggestion.Steps {
		fmt.Fprintf(&b, "  - %s\n", step)
	}
	if suggestion.Rationale != "" {
		fmt.Fprintf(&b, "rationale: %s\n", suggestion.Rationale)
	}
	return b.String()
}

func promptComplete(registry *agent.Registry, input string, limit int) {
	suggestions := promptcomplete.SuggestAll(promptcomplete.Context{
		Input:     input,
		Cursor:    len(input),
		Agents:    promptAgentCandidates(registry),
		Tools:     promptToolCandidates(),
		Templates: promptTemplateCandidates(),
	}, promptcomplete.Options{Limit: limit})
	if len(suggestions) == 0 {
		fmt.Println("No prompt completion found.")
		return
	}
	fmt.Print(formatPromptSuggestions(suggestions))
}

func promptAgentCandidates(registry *agent.Registry) []promptcomplete.Candidate {
	if registry == nil {
		return nil
	}
	names := registry.List()
	out := make([]promptcomplete.Candidate, 0, len(names))
	for _, name := range names {
		configuredAgent, _ := registry.Get(name)
		out = append(out, promptcomplete.Candidate{
			Text:        name,
			Kind:        "agent",
			Description: configuredAgent.Description,
			Tokens:      append([]string(nil), configuredAgent.Capabilities...),
		})
	}
	return out
}

func promptToolCandidates() []promptcomplete.Candidate {
	return []promptcomplete.Candidate{
		{Text: "memory-search", Kind: "tool", Description: "search local memory and saved sessions"},
		{Text: "plan-agents", Kind: "tool", Description: "preview agent orchestration"},
		{Text: "review", Kind: "tool", Description: "run a structured code review"},
		{Text: "test", Kind: "tool", Description: "run verification tests"},
	}
}

func promptTemplateCandidates() []promptcomplete.Candidate {
	return []promptcomplete.Candidate{
		{
			Text:        "review this change for correctness, tests, and regressions",
			Kind:        "template",
			Description: "code review prompt",
		},
		{
			Text:        "summarize this session with changed files and verification evidence",
			Kind:        "template",
			Description: "session summary prompt",
		},
		{
			Text:        "plan agents for this task and list the verification gates",
			Kind:        "template",
			Description: "agent orchestration prompt",
		},
	}
}

func formatPromptSuggestions(suggestions []promptcomplete.Suggestion) string {
	var b strings.Builder
	for i := range suggestions {
		suggestion := &suggestions[i]
		fmt.Fprintf(&b, "text: %s\n", suggestion.Text)
		fmt.Fprintf(&b, "suffix: %s\n", suggestion.Suffix)
		fmt.Fprintf(&b, "kind: %s\n", suggestion.Candidate.Kind)
		fmt.Fprintf(&b, "score: %d\n", suggestion.Score)
		fmt.Fprintf(&b, "replace: %d:%d\n", suggestion.ReplacementStart, suggestion.ReplacementEnd)
		if suggestion.Explanation != "" {
			fmt.Fprintf(&b, "explanation: %s\n", suggestion.Explanation)
		}
		b.WriteByte('\n')
	}
	return b.String()
}

func printFeedbackProposals(saved session.Session) {
	proposals := feedback.FromSession(saved)
	if len(proposals) == 0 {
		fmt.Println("No feedback proposals found.")
		return
	}
	for i := range proposals {
		fmt.Print(formatFeedbackProposal(proposals[i]))
	}
}

func formatFeedbackProposal(proposal feedback.Proposal) string {
	var b strings.Builder
	fmt.Fprintf(&b, "agent: %s\n", proposal.Agent)
	fmt.Fprintf(&b, "confidence: %.2f\n", proposal.Confidence)
	fmt.Fprintf(&b, "action: %s\n", proposal.Action)
	fmt.Fprintf(&b, "reason: %s\n", proposal.Reason)
	if len(proposal.Evidence) > 0 {
		b.WriteString("evidence:\n")
		for _, evidence := range proposal.Evidence {
			fmt.Fprintf(&b, "  - %s\n", evidence)
		}
	}
	b.WriteByte('\n')
	return b.String()
}

func applyFeedbackProposals(saved session.Session, configPath, historyPath string) error {
	configPath = strings.TrimSpace(configPath)
	if configPath == "" {
		return errors.New("feedback apply: config path is required")
	}

	cfg, loaded, err := appconfig.LoadFiles([]string{configPath})
	if err != nil {
		return fmt.Errorf("feedback apply: load config: %w", err)
	}
	if len(loaded) == 0 {
		return fmt.Errorf("feedback apply: config %s not found", configPath)
	}

	updatedAgents, history := feedback.ApplyProposals(cfg.Agents, feedback.FromSession(saved))
	if len(history) == 0 {
		fmt.Println("No feedback proposals applied.")
		return nil
	}

	cfg.Agents = updatedAgents
	if err := writeConfigFile(configPath, cfg); err != nil {
		return fmt.Errorf("feedback apply: %w", err)
	}

	historyPath = feedbackHistoryDefault(configPath, historyPath)
	if err := appendFeedbackHistory(historyPath, history, time.Now().UTC()); err != nil {
		return fmt.Errorf("feedback apply: %w", err)
	}

	fmt.Printf("Applied %d feedback proposal(s).\n", len(history))
	fmt.Println("config: " + configPath)
	fmt.Println("history: " + historyPath)
	return nil
}

func feedbackHistoryDefault(configPath, historyPath string) string {
	historyPath = strings.TrimSpace(historyPath)
	if historyPath != "" {
		return historyPath
	}
	return configPath + ".feedback.md"
}

func writeConfigFile(path string, cfg appconfig.Config) error {
	data, err := yaml.Marshal(cfg)
	if err != nil {
		return fmt.Errorf("marshal config: %w", err)
	}
	if err := os.WriteFile(path, data, 0o600); err != nil {
		return fmt.Errorf("write config %s: %w", path, err)
	}
	return nil
}

func appendFeedbackHistory(path string, entries []feedback.HistoryEntry, appliedAt time.Time) error {
	dir := filepath.Dir(path)
	if dir != "." && dir != "" {
		if err := os.MkdirAll(dir, 0o750); err != nil {
			return fmt.Errorf("create feedback history dir %s: %w", dir, err)
		}
	}

	file, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return fmt.Errorf("open feedback history %s: %w", path, err)
	}
	if _, err := file.WriteString(formatFeedbackHistory(entries, appliedAt)); err != nil {
		_ = file.Close()
		return fmt.Errorf("write feedback history %s: %w", path, err)
	}
	if err := file.Close(); err != nil {
		return fmt.Errorf("close feedback history %s: %w", path, err)
	}
	return nil
}

func formatFeedbackHistory(entries []feedback.HistoryEntry, appliedAt time.Time) string {
	var b strings.Builder
	fmt.Fprintf(&b, "## Applied feedback %s\n\n", appliedAt.Format(time.RFC3339))
	for i := range entries {
		b.WriteString(feedback.FormatHistoryEntry(entries[i]))
		b.WriteByte('\n')
	}
	return b.String()
}

func runRouteModels(opts cliOptions) error {
	candidates, profile, err := routeCandidatesAndProfile(opts)
	if err != nil {
		return err
	}
	chain := modelroute.FallbackChain(candidates, profile)
	if len(chain) == 0 {
		fmt.Println("No model route candidates fit.")
		return nil
	}
	for i := range chain {
		fmt.Println(formatRouteCandidate(chain[i], profile))
	}
	return nil
}

func routeCandidatesAndProfile(opts cliOptions) ([]modelroute.Candidate, modelroute.RequestProfile, error) {
	candidates := make([]modelroute.Candidate, 0, len(opts.routeCandidates))
	for _, raw := range opts.routeCandidates {
		candidate, err := parseRouteCandidate(raw)
		if err != nil {
			return nil, modelroute.RequestProfile{}, err
		}
		candidates = append(candidates, candidate)
	}
	profile := modelroute.RequestProfile{
		EstimatedInputTokens:  opts.routeInputTokens.value,
		EstimatedOutputTokens: opts.routeOutputTokens.value,
		Interactive:           opts.routeInteractive,
		Batch:                 opts.routeBatch,
	}
	if opts.routeBudget.set {
		profile.Budget = opts.routeBudget.value
	}
	if opts.routeCacheReuse.set {
		profile.PromptCacheReuseEstimate = opts.routeCacheReuse.value
	}
	return candidates, profile, nil
}

func applyRouteSelection(opts cliOptions, state *selectionState) error {
	if len(opts.routeCandidates) == 0 {
		return nil
	}
	candidates, profile, err := routeCandidatesAndProfile(opts)
	if err != nil {
		return err
	}
	chain := modelroute.FallbackChain(candidates, profile)
	if len(chain) == 0 {
		return errors.New("model route: no candidates fit request budget/context")
	}
	state.selectedModel = chain[0].ID()
	state.fallbackModels = routeFallbackIDs(chain[1:])
	state.modelLocked = true
	state.sessionState.DefaultModel = state.selectedModel
	return nil
}

func routeFallbackIDs(candidates []modelroute.Candidate) []string {
	if len(candidates) == 0 {
		return nil
	}
	out := make([]string, 0, len(candidates))
	for _, candidate := range candidates {
		out = append(out, candidate.ID())
	}
	return out
}

func parseRouteCandidate(raw string) (modelroute.Candidate, error) {
	parts := strings.Split(raw, ",")
	if len(parts) == 0 || strings.TrimSpace(parts[0]) == "" {
		return modelroute.Candidate{}, errors.New("route candidate: model id is required")
	}
	candidate := modelroute.Candidate{}
	id := strings.TrimSpace(parts[0])
	if provider, name, ok := strings.Cut(id, "/"); ok {
		candidate.Provider = strings.TrimSpace(provider)
		candidate.Name = strings.TrimSpace(name)
	} else {
		candidate.Name = id
	}
	if candidate.Name == "" && candidate.Provider == "" {
		return modelroute.Candidate{}, errors.New("route candidate: model id is required")
	}
	for _, part := range parts[1:] {
		field, value, ok := strings.Cut(strings.TrimSpace(part), "=")
		if !ok {
			return modelroute.Candidate{}, fmt.Errorf("route candidate %q: expected key=value", raw)
		}
		if err := applyRouteCandidateField(&candidate, strings.TrimSpace(field), strings.TrimSpace(value)); err != nil {
			return modelroute.Candidate{}, err
		}
	}
	return candidate, nil
}

func applyRouteCandidateField(candidate *modelroute.Candidate, field, value string) error {
	switch field {
	case "input", "input_cost":
		parsed, err := strconv.ParseFloat(value, 64)
		if err != nil {
			return fmt.Errorf("route candidate input cost: %w", err)
		}
		candidate.InputTokenCost = parsed
	case "output", "output_cost":
		parsed, err := strconv.ParseFloat(value, 64)
		if err != nil {
			return fmt.Errorf("route candidate output cost: %w", err)
		}
		candidate.OutputTokenCost = parsed
	case "priority":
		parsed, err := strconv.Atoi(value)
		if err != nil {
			return fmt.Errorf("route candidate priority: %w", err)
		}
		candidate.Priority = parsed
	case "max", "max_input":
		parsed, err := strconv.Atoi(value)
		if err != nil {
			return fmt.Errorf("route candidate max input: %w", err)
		}
		candidate.MaxInputTokens = parsed
	case "latency":
		parsed, err := strconv.Atoi(value)
		if err != nil {
			return fmt.Errorf("route candidate latency: %w", err)
		}
		candidate.ExpectedLatencyMS = parsed
	case "ttft":
		parsed, err := strconv.Atoi(value)
		if err != nil {
			return fmt.Errorf("route candidate ttft: %w", err)
		}
		candidate.ExpectedTTFTMS = parsed
	default:
		return fmt.Errorf("route candidate: unknown field %q", field)
	}
	return nil
}

func formatRouteCandidate(candidate modelroute.Candidate, profile modelroute.RequestProfile) string {
	parts := []string{
		candidate.ID(),
		fmt.Sprintf("cost=%.6f", modelroute.EstimateCost(candidate, profile)),
	}
	if candidate.Priority != 0 {
		parts = append(parts, "priority="+strconv.Itoa(candidate.Priority))
	}
	if candidate.MaxInputTokens > 0 {
		parts = append(parts, "max_input="+strconv.Itoa(candidate.MaxInputTokens))
	}
	if candidate.ExpectedLatencyMS > 0 {
		parts = append(parts, "latency_ms="+strconv.Itoa(candidate.ExpectedLatencyMS))
	}
	if candidate.ExpectedTTFTMS > 0 {
		parts = append(parts, "ttft_ms="+strconv.Itoa(candidate.ExpectedTTFTMS))
	}
	return strings.Join(parts, "\t")
}

func runReviewPlan(reviewerNames, paths, gates []string) error {
	plan, err := review.NewPlan(reviewPlanReviewers(reviewerNames), reviewPlanPaths(paths), gates)
	if err != nil {
		return fmt.Errorf("review plan: %w", err)
	}
	fmt.Print(formatReviewPlan(plan))
	return nil
}

func reviewPlanReviewers(names []string) []review.Reviewer {
	if len(names) == 0 {
		return []review.Reviewer{
			{Name: "quality-reviewer", Categories: []review.Category{review.CategoryCorrectness, review.CategoryMaintainability}},
			{Name: "test-engineer", Categories: []review.Category{review.CategoryTests}},
		}
	}
	reviewers := make([]review.Reviewer, 0, len(names))
	for _, name := range names {
		reviewers = append(reviewers, review.Reviewer{Name: strings.TrimSpace(name)})
	}
	return reviewers
}

func reviewPlanPaths(paths []string) []string {
	if len(paths) == 0 {
		return []string{"."}
	}
	return append([]string(nil), paths...)
}

func formatReviewPlan(plan review.Plan) string {
	var b strings.Builder
	b.WriteString("reviewers:\n")
	for _, reviewer := range plan.Reviewers() {
		fmt.Fprintf(&b, "  - %s\n", formatReviewPlanReviewer(reviewer))
	}
	b.WriteString("paths:\n")
	for _, path := range plan.Paths() {
		fmt.Fprintf(&b, "  - %s\n", path)
	}
	b.WriteString("rounds:\n")
	rounds := plan.Rounds()
	for i := range rounds {
		round := rounds[i]
		fmt.Fprintf(&b, "  - %d\t%s\t%s\treviewers=%s\n", round.Number, round.Kind, round.Name, strings.Join(round.Reviewers, ","))
	}
	if crossReviews := plan.CrossReviews(); len(crossReviews) > 0 {
		b.WriteString("cross_reviews:\n")
		for _, crossReview := range crossReviews {
			fmt.Fprintf(&b, "  - %s -> %s\n", crossReview.Reviewer, crossReview.ReviewedReviewer)
		}
	}
	b.WriteString("gates:\n")
	for _, gate := range plan.RequiredGates() {
		fmt.Fprintf(&b, "  - %s\n", gate)
	}
	return b.String()
}

func formatReviewPlanReviewer(reviewer review.Reviewer) string {
	parts := []string{reviewer.Name}
	if len(reviewer.Categories) > 0 {
		categories := make([]string, 0, len(reviewer.Categories))
		for _, category := range reviewer.Categories {
			categories = append(categories, string(category))
		}
		parts = append(parts, "categories="+strings.Join(categories, ","))
	}
	return strings.Join(parts, "\t")
}

func runReviewScan(root string, largeFileBytes int) error {
	findings, err := watch.ScanWithOptions(root, watch.Options{LargeFileBytes: int64(largeFileBytes)})
	if err != nil {
		return fmt.Errorf("review scan: %w", err)
	}
	report := review.Report{
		Reviewer: "watch-scan",
		Findings: watchFindingsToReview(findings),
	}
	fmt.Print(formatReviewReport(report))
	return nil
}

func watchFindingsToReview(findings []watch.Finding) []review.Finding {
	out := make([]review.Finding, 0, len(findings))
	for i := range findings {
		finding := findings[i]
		out = append(out, review.Finding{
			Severity: reviewSeverity(finding.Severity),
			Category: reviewCategory(finding.Kind),
			Path:     finding.Path,
			Message:  finding.Message,
		})
	}
	return review.SortedFindings(out)
}

func reviewSeverity(severity string) review.Severity {
	switch severity {
	case watch.SeverityWarning:
		return review.SeverityMedium
	case watch.SeverityMaintenance:
		return review.SeverityLow
	default:
		return review.SeverityInfo
	}
}

func reviewCategory(kind string) review.Category {
	switch kind {
	case watch.KindMissingTest:
		return review.CategoryTests
	case watch.KindConventionDrift:
		return review.CategoryMaintainability
	default:
		return review.CategoryMaintainability
	}
}

func formatReviewReport(report review.Report) string {
	var b strings.Builder
	fmt.Fprintf(&b, "reviewer: %s\n", report.Reviewer)
	summary := report.SeveritySummary()
	fmt.Fprintf(&b, "summary: critical=%d high=%d medium=%d low=%d info=%d total=%d\n", summary.Critical, summary.High, summary.Medium, summary.Low, summary.Info, summary.Total())
	findings := report.SortedFindings()
	if len(findings) == 0 {
		b.WriteString("findings: none\n")
		return b.String()
	}
	b.WriteString("findings:\n")
	for i := range findings {
		fmt.Fprintf(&b, "  - %s\n", formatReviewFinding(findings[i]))
	}
	return b.String()
}

func formatReviewFinding(finding review.Finding) string {
	parts := []string{
		"severity=" + string(finding.Severity),
		"category=" + string(finding.Category),
		"path=" + finding.Path,
	}
	if finding.Line > 0 {
		parts = append(parts, "line="+strconv.Itoa(finding.Line))
	}
	if finding.Message != "" {
		parts = append(parts, "message="+finding.Message)
	}
	if finding.Suggestion != "" {
		parts = append(parts, "suggestion="+finding.Suggestion)
	}
	return strings.Join(parts, "\t")
}

func runAsyncPlan(specs []string) error {
	if len(specs) == 0 {
		return errors.New("async plan: at least one --async-task is required")
	}
	tasks := make([]attasync.Task, 0, len(specs))
	for _, spec := range specs {
		task, err := parseAsyncTaskSpec(spec)
		if err != nil {
			return err
		}
		tasks = append(tasks, task)
	}
	plan, err := attasync.NewPlan(tasks)
	if err != nil {
		return fmt.Errorf("async plan: %w", err)
	}
	fmt.Print(formatAsyncPlanBatches(plan.ReadyBatches()))
	return nil
}

func parseAsyncTaskSpec(spec string) (attasync.Task, error) {
	parts := strings.SplitN(spec, "|", 4)
	if len(parts) < 3 {
		return attasync.Task{}, fmt.Errorf("async task %q: expected id|agent|prompt|dep1+dep2", spec)
	}
	task := attasync.Task{
		ID:     strings.TrimSpace(parts[0]),
		Agent:  strings.TrimSpace(parts[1]),
		Prompt: strings.TrimSpace(parts[2]),
	}
	if task.ID == "" {
		return attasync.Task{}, fmt.Errorf("async task %q: id is required", spec)
	}
	if len(parts) == 4 {
		task.DependsOn = parseAsyncDependencies(parts[3])
	}
	return task, nil
}

func parseAsyncDependencies(raw string) []string {
	fields := strings.FieldsFunc(raw, func(r rune) bool { return r == '+' || r == ';' })
	deps := make([]string, 0, len(fields))
	for _, field := range fields {
		dep := strings.TrimSpace(field)
		if dep != "" {
			deps = append(deps, dep)
		}
	}
	return deps
}

func formatAsyncPlanBatches(batches [][]attasync.Task) string {
	if len(batches) == 0 {
		return "waves: none\n"
	}
	var b strings.Builder
	for i, batch := range batches {
		fmt.Fprintf(&b, "wave %d:\n", i+1)
		for j := range batch {
			fmt.Fprintf(&b, "  - %s\n", formatAsyncTask(batch[j]))
		}
	}
	return b.String()
}

func formatAsyncTask(task attasync.Task) string {
	parts := []string{"id=" + task.ID}
	if task.Agent != "" {
		parts = append(parts, "agent="+task.Agent)
	}
	if len(task.DependsOn) > 0 {
		parts = append(parts, "depends="+strings.Join(task.DependsOn, "+"))
	}
	if task.Prompt != "" {
		parts = append(parts, "prompt="+task.Prompt)
	}
	return strings.Join(parts, "	")
}

func runSpeculatePlan(agents, gates []string, prompt string) error {
	if len(gates) == 0 {
		gates = []string{"tests pass", "lint pass", "types pass"}
	}
	plan, err := speculate.NewPlan(agents, gates)
	if err != nil {
		return fmt.Errorf("speculate plan: %w", err)
	}
	fmt.Print(formatSpeculatePlan(plan))
	if strings.TrimSpace(prompt) != "" {
		estimate, estimateErr := speculate.EstimatePromptCacheReuse(speculateBranchPrompts(plan, prompt))
		if estimateErr != nil {
			return fmt.Errorf("speculate prompt cache: %w", estimateErr)
		}
		fmt.Print(formatSpeculatePromptCacheEstimate(estimate))
	}
	return nil
}

func formatSpeculatePlan(plan speculate.Plan) string {
	var b strings.Builder
	fmt.Fprintf(&b, "agents: %s\n", strings.Join(plan.Agents, ","))
	b.WriteString("rounds:\n")
	for _, round := range plan.Rounds {
		fmt.Fprintf(&b, "  - %d\t%s\t%s\n", round.Number, round.Name, round.Purpose)
	}
	proposals := make([]speculate.Proposal, 0, len(plan.Agents))
	for _, name := range plan.Agents {
		proposals = append(proposals, speculate.Proposal{Agent: name, Round: speculate.RoundProposal})
	}
	reviews, err := speculate.CrossReviews(proposals)
	if err == nil && len(reviews) > 0 {
		b.WriteString("cross_reviews:\n")
		for _, review := range reviews {
			fmt.Fprintf(&b, "  - %s -> %s\n", review.Reviewer, review.TargetAgent)
		}
	}
	b.WriteString("gates:\n")
	for _, gate := range plan.GateChecks {
		fmt.Fprintf(&b, "  - %s\n", gate)
	}
	return b.String()
}

func speculateBranchPrompts(plan speculate.Plan, prompt string) []speculate.BranchPrompt {
	prompt = strings.TrimSpace(prompt)
	if prompt == "" {
		return nil
	}

	var shared strings.Builder
	shared.WriteString("Task:\n")
	shared.WriteString(prompt)
	shared.WriteString("\n\nRequired gate checks:\n")
	for _, gate := range plan.GateChecks {
		shared.WriteString("- ")
		shared.WriteString(gate)
		shared.WriteByte('\n')
	}
	shared.WriteString("\nSpeculative round: independent proposal\n")

	branches := make([]speculate.BranchPrompt, 0, len(plan.Agents))
	for _, name := range plan.Agents {
		branches = append(branches, speculate.BranchPrompt{
			Branch: name,
			Prompt: shared.String() +
				"Branch agent: " + name + "\n" +
				"Produce a self-contained proposal that can be cross-reviewed.\n",
		})
	}
	return branches
}

func formatSpeculatePromptCacheEstimate(estimate speculate.PromptCacheReuseEstimate) string {
	var b strings.Builder
	b.WriteString("prompt_cache:\n")
	fmt.Fprintf(&b, "  shared_prefix_bytes: %d\n", estimate.SharedPrefixBytes)
	fmt.Fprintf(&b, "  reusable_prompt_bytes: %d\n", estimate.ReusablePromptBytes)
	fmt.Fprintf(&b, "  total_prompt_bytes: %d\n", estimate.TotalPromptBytes)
	fmt.Fprintf(&b, "  reuse_ratio: %.4f\n", estimate.ReuseRatio)
	b.WriteString("  branches:\n")
	for _, branch := range estimate.Branches {
		fmt.Fprintf(&b, "    - %s\tprompt_bytes=%d\tshared_prefix_bytes=%d\treuse_ratio=%.4f\n",
			branch.Branch,
			branch.PromptBytes,
			branch.SharedPrefixBytes,
			branch.ReuseRatio,
		)
	}
	return b.String()
}

func runWatchScan(root string, largeFileBytes int, jsonOutput bool) error {
	findings, err := watch.ScanWithOptions(root, watch.Options{LargeFileBytes: int64(largeFileBytes)})
	if err != nil {
		return fmt.Errorf("watch scan: %w", err)
	}
	if jsonOutput {
		if findings == nil {
			findings = []watch.Finding{}
		}
		if err := json.NewEncoder(os.Stdout).Encode(struct {
			Findings []watch.Finding `json:"findings"`
		}{Findings: findings}); err != nil {
			return fmt.Errorf("watch scan: encode JSON: %w", err)
		}
		return nil
	}
	if len(findings) == 0 {
		fmt.Println("No watch findings found.")
		return nil
	}
	for i := range findings {
		fmt.Println(formatWatchFinding(findings[i]))
	}
	return nil
}

func runWatchLoop(ctx context.Context, root string, largeFileBytes, intervalSeconds, maxIterations int) error {
	interval := time.Duration(intervalSeconds) * time.Second
	results, err := watch.Run(ctx, root, watch.RunOptions{
		ScanOptions:   watch.Options{LargeFileBytes: int64(largeFileBytes)},
		Interval:      interval,
		MaxIterations: maxIterations,
	})
	for i := range results {
		fmt.Println(formatWatchIteration(results[i]))
		if len(results[i].Findings) == 0 {
			fmt.Println("No watch findings found.")
			continue
		}
		for j := range results[i].Findings {
			fmt.Println(formatWatchFinding(results[i].Findings[j]))
		}
	}
	if err != nil {
		return fmt.Errorf("watch loop: %w", err)
	}
	return nil
}

func formatWatchIteration(result watch.IterationResult) string {
	parts := []string{
		"iteration=" + strconv.Itoa(result.Iteration),
		"findings=" + strconv.Itoa(len(result.Findings)),
	}
	if !result.StartedAt.IsZero() {
		parts = append(parts, "started="+result.StartedAt.Format(time.RFC3339))
	}
	if result.Duration > 0 {
		parts = append(parts, "duration="+result.Duration.String())
	}
	return strings.Join(parts, "\t")
}

func formatWatchFinding(finding watch.Finding) string {
	parts := []string{
		"path=" + finding.Path,
		"kind=" + finding.Kind,
		"severity=" + finding.Severity,
	}
	if finding.Message != "" {
		parts = append(parts, "message="+finding.Message)
	}
	return strings.Join(parts, "\t")
}

func runGitHistorySearch(ctx context.Context, root, query string, limit int) error {
	if limit == 0 {
		limit = 5
	}
	logText, err := gitHistoryLog(ctx, root)
	if err != nil {
		return err
	}
	commits, err := githistory.ParseLog(logText)
	if err != nil {
		return fmt.Errorf("git history: parse log: %w", err)
	}
	results := githistory.NewIndex(commits).Search(query, limit)
	if len(results) == 0 {
		fmt.Println("No git history results found.")
		return nil
	}
	for i := range results {
		fmt.Println(formatGitHistoryResult(results[i]))
	}
	return nil
}

func gitHistoryLog(ctx context.Context, root string) (string, error) {
	cmd := exec.CommandContext(
		nonNilContext(ctx),
		"git",
		"log",
		"--name-only",
		"--date=iso-strict",
		"--pretty=format:%H%x1f%an%x1f%ae%x1f%aI%x1f%s%x1e",
		"--",
	)
	cmd.Dir = root
	out, err := cmd.Output()
	if err != nil {
		return "", fmt.Errorf("git history: run git log: %w", err)
	}
	return string(out), nil
}

func formatGitHistoryResult(result githistory.Result) string {
	commit := result.Commit
	parts := []string{
		shortCommitHash(commit.Hash),
		fmt.Sprintf("score=%d", result.Score),
	}
	if !commit.Date.IsZero() {
		parts = append(parts, "date="+commit.Date.Format(time.RFC3339))
	}
	if commit.AuthorName != "" {
		parts = append(parts, "author="+commit.AuthorName)
	}
	if commit.Subject != "" {
		parts = append(parts, "subject="+commit.Subject)
	}
	for _, snippet := range result.Snippets {
		if snippet.Text != "" {
			parts = append(parts, snippet.Field+"="+snippet.Text)
			break
		}
	}
	return strings.Join(parts, "\t")
}

func shortCommitHash(hash string) string {
	if len(hash) <= 12 {
		return hash
	}
	return hash[:12]
}

func recordFailure(
	store *session.Store,
	sessionState session.Session,
	approach string,
	reason string,
	commit string,
	agentName string,
) error {
	if !sessionState.RecordNegativeKnowledge(approach, reason, commit, agentName) {
		return errors.New("record failure: approach and reason are required, or this failure is already recorded")
	}
	if err := store.Save(sessionState); err != nil {
		return fmt.Errorf("record failure: save session: %w", err)
	}
	fmt.Println("Recorded failure on session " + sessionState.ID)
	return nil
}

func recordEvaluation(
	store *session.Store,
	sessionState session.Session,
	agentName string,
	outcome string,
	notes string,
	reference string,
	score int,
) error {
	if !sessionState.RecordEvaluation(agentName, outcome, notes, reference, score) {
		return errors.New("record evaluation: agent and outcome are required")
	}
	if err := store.Save(sessionState); err != nil {
		return fmt.Errorf("record evaluation: save session: %w", err)
	}
	fmt.Println("Recorded evaluation on session " + sessionState.ID)
	return nil
}

const messagePreviewRunes = 120

func listMessages(sessionState session.Session) {
	if len(sessionState.Messages) == 0 {
		fmt.Println("No messages recorded.")
		return
	}
	for i := range sessionState.Messages {
		fmt.Println(formatMessageSummary(i+1, sessionState.Messages[i]))
	}
}

func formatMessageSummary(index int, message llm.Message) string {
	content := compactMessageWhitespace(message.Content)
	parts := []string{
		"index=" + strconv.Itoa(index),
		"role=" + string(message.Role),
		"chars=" + strconv.Itoa(len([]rune(message.Content))),
	}
	if content != "" {
		parts = append(parts, "preview="+truncateRunes(content, messagePreviewRunes))
	}
	return strings.Join(parts, "	")
}

func compactMessageWhitespace(value string) string {
	return strings.Join(strings.Fields(value), " ")
}

func truncateRunes(value string, limit int) string {
	if limit <= 0 {
		return ""
	}
	runes := []rune(value)
	if len(runes) <= limit {
		return value
	}
	if limit == 1 {
		return "…"
	}
	return string(runes[:limit-1]) + "…"
}

func listFailures(sessionState session.Session) {
	if len(sessionState.NegativeKnowledge) == 0 {
		fmt.Println("No failures recorded.")
		return
	}
	failures := append([]session.NegativeKnowledge(nil), sessionState.NegativeKnowledge...)
	sort.SliceStable(failures, func(i, j int) bool {
		return failures[i].CreatedAt.Before(failures[j].CreatedAt)
	})
	for i := range failures {
		fmt.Println(formatFailure(failures[i]))
	}
}

func formatFailure(failure session.NegativeKnowledge) string {
	parts := []string{"approach=" + failure.Approach, "reason=" + failure.Reason}
	if !failure.CreatedAt.IsZero() {
		parts = append(parts, "created_at="+failure.CreatedAt.Format(time.RFC3339))
	}
	if failure.Agent != "" {
		parts = append(parts, "agent="+failure.Agent)
	}
	if failure.Commit != "" {
		parts = append(parts, "commit="+failure.Commit)
	}
	return strings.Join(parts, "	")
}

func listEvaluations(sessionState session.Session) {
	if len(sessionState.Evaluations) == 0 {
		fmt.Println("No evaluations recorded.")
		return
	}
	evaluations := append([]session.AgentEvaluation(nil), sessionState.Evaluations...)
	sort.SliceStable(evaluations, func(i, j int) bool {
		return evaluations[i].CreatedAt.Before(evaluations[j].CreatedAt)
	})
	for i := range evaluations {
		fmt.Println(formatEvaluation(evaluations[i]))
	}
}

func formatEvaluation(evaluation session.AgentEvaluation) string {
	parts := []string{"agent=" + evaluation.Agent, "outcome=" + evaluation.Outcome}
	if !evaluation.CreatedAt.IsZero() {
		parts = append(parts, "created_at="+evaluation.CreatedAt.Format(time.RFC3339))
	}
	if evaluation.Score != 0 {
		parts = append(parts, "score="+strconv.Itoa(evaluation.Score))
	}
	if evaluation.Reference != "" {
		parts = append(parts, "reference="+evaluation.Reference)
	}
	if evaluation.Notes != "" {
		parts = append(parts, "notes="+evaluation.Notes)
	}
	return strings.Join(parts, "	")
}

func listArtifacts(sessionState session.Session) {
	if len(sessionState.Artifacts) == 0 {
		fmt.Println("No artifacts recorded.")
		return
	}
	artifacts := append([]session.Artifact(nil), sessionState.Artifacts...)
	sort.SliceStable(artifacts, func(i, j int) bool {
		return artifacts[i].CreatedAt.Before(artifacts[j].CreatedAt)
	})
	for i := range artifacts {
		fmt.Println(formatArtifact(artifacts[i]))
	}
}

func formatArtifact(artifact session.Artifact) string {
	parts := []string{"path=" + artifact.Path, "kind=" + artifact.Kind}
	if !artifact.CreatedAt.IsZero() {
		parts = append(parts, "created_at="+artifact.CreatedAt.Format(time.RFC3339))
	}
	if artifact.SourceAgent != "" {
		parts = append(parts, "agent="+artifact.SourceAgent)
	}
	if artifact.Summary != "" {
		parts = append(parts, "summary="+artifact.Summary)
	}
	return strings.Join(parts, "	")
}

func mergeArtifacts(ctx context.Context, state appState, outputPath string, maxBytes int) error {
	if maxBytes == 0 {
		maxBytes = int(watch.DefaultLargeFileBytes)
	}
	result, err := artifactmerge.Merge(state.cwd, state.sessionState.Artifacts, int64(maxBytes))
	if err != nil {
		return fmt.Errorf("merge artifacts: %w", err)
	}
	for _, warning := range result.Warnings {
		fmt.Fprintf(os.Stderr, "warning: artifact %s skipped: %s\n", warning.Path, warning.Reason)
	}
	if strings.TrimSpace(outputPath) == "-" {
		fmt.Print(result.Markdown)
		return nil
	}
	if err := os.MkdirAll(filepath.Dir(outputPath), 0o750); err != nil {
		return fmt.Errorf("merge artifacts: create output dir: %w", err)
	}
	if err := os.WriteFile(outputPath, []byte(result.Markdown), 0o600); err != nil {
		return fmt.Errorf("merge artifacts: write %s: %w", outputPath, err)
	}
	emitFileWriteWarning(ctx, state.hookRunner, state.sessionState, outputPath, state.selectedAgent, "merged-artifacts")
	fmt.Println("Merged artifacts into " + outputPath)
	return nil
}

func recordArtifact(
	store *session.Store,
	sessionState session.Session,
	path string,
	kind string,
	summary string,
	sourceAgent string,
) error {
	if !sessionState.RecordArtifact(path, kind, summary, sourceAgent) {
		return errors.New("record artifact: path and kind are required")
	}
	if err := store.Save(sessionState); err != nil {
		return fmt.Errorf("record artifact: save session: %w", err)
	}
	fmt.Println("Recorded artifact on session " + sessionState.ID)
	return nil
}

type agentDescription struct {
	Temperature    *float64 `yaml:"temperature,omitempty"`
	TopP           *float64 `yaml:"top_p,omitempty"`
	Seed           *int     `yaml:"seed,omitempty"`
	Name           string   `yaml:"name"`
	ReasoningLevel string   `yaml:"reasoning_level,omitempty"`
	Model          string   `yaml:"model,omitempty"`
	Description    string   `yaml:"description,omitempty"`
	Personality    string   `yaml:"personality,omitempty"`
	SystemPrompt   string   `yaml:"system_prompt,omitempty"`
	FallbackModels []string `yaml:"fallback_models,omitempty"`
	Capabilities   []string `yaml:"capabilities,omitempty"`
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
		Description:    activeAgent.Description,
		Personality:    activeAgent.Personality,
		SystemPrompt:   activeAgent.SystemPrompt,
		FallbackModels: activeAgent.FallbackModels,
		Capabilities:   activeAgent.Capabilities,
		Temperature:    activeAgent.Temperature,
		TopP:           activeAgent.TopP,
		Seed:           activeAgent.Seed,
		ReasoningLevel: activeAgent.ReasoningLevel,
		Triggers:       activeAgent.Triggers,
		MaxTokens:      activeAgent.MaxTokens,
	})
	if err != nil {
		return "", fmt.Errorf("marshal agent description: %w", err)
	}
	return string(out), nil
}

func doctor(ctx context.Context, state appState) error {
	ctx = nonNilContext(ctx)
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
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
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

func doctorOffline(opts cliOptions) error {
	cfg, loadedConfigPaths, err := appconfig.Load()
	if err != nil {
		fmt.Println("config_error: " + err.Error())
	}

	fmt.Println("Atteler offline doctor")
	if len(loadedConfigPaths) == 0 {
		fmt.Println("config: no config files loaded")
	} else {
		fmt.Println("config: " + strings.Join(loadedConfigPaths, ", "))
	}
	store := session.NewStore(opts.sessionDir)
	fmt.Println("sessions: " + store.Dir() + " (" + pathStatus(store.Dir()) + ")")

	providerNames := make([]string, 0)
	for _, provider := range llm.KnownProviders() {
		providerNames = append(providerNames, provider.Name)
	}
	sort.Strings(providerNames)
	if len(providerNames) == 0 {
		fmt.Println("known_providers: none")
	} else {
		fmt.Println("known_providers: " + strings.Join(providerNames, ", "))
	}

	agents := agent.NewRegistry(cfg.Agents).List()
	if len(agents) == 0 {
		fmt.Println("agents: none configured")
	} else {
		fmt.Println("agents: " + strings.Join(agents, ", "))
	}

	fmt.Println("hook_events: " + strconv.Itoa(len(events.SupportedEventTypes())))
	if len(cfg.Plugins.Paths) == 0 {
		fmt.Println("plugins: none configured")
	} else {
		fmt.Println("plugins: " + strings.Join(cfg.Plugins.Paths, ", "))
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

func listSessions(store *session.Store, tag string) error {
	summaries, err := listSessionSummaries(store, tag)
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

func listHeadlessRuns(store *session.Store) error {
	runs, err := store.ListHeadlessRuns()
	if err != nil {
		return fmt.Errorf("list headless sessions: %w", err)
	}
	active := make([]session.HeadlessRun, 0, len(runs))
	for i := range runs {
		run := &runs[i]
		if run.Status == session.HeadlessStatusRunning {
			active = append(active, *run)
		}
	}
	if len(active) == 0 {
		fmt.Println("No active headless sessions found.")
		return nil
	}
	for i := range active {
		fmt.Println(formatHeadlessRun(active[i]))
	}
	return nil
}

func streamHeadlessLog(ctx context.Context, store *session.Store, id string) error {
	ctx = nonNilContext(ctx)
	id = strings.TrimSpace(id)
	if id == "" {
		return errors.New("stream headless: id is required")
	}

	offset := 0
	for {
		text, err := store.ReadHeadlessLog(id)
		if err != nil && !errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("stream headless: %w", err)
		}
		if len(text) > offset {
			fmt.Print(text[offset:])
			offset = len(text)
		}

		run, err := store.LoadHeadlessRun(id)
		if err != nil {
			return fmt.Errorf("stream headless: %w", err)
		}
		if run.Status != session.HeadlessStatusRunning {
			return nil
		}

		timer := time.NewTimer(time.Second)
		select {
		case <-ctx.Done():
			timer.Stop()
			return fmt.Errorf("stream headless: %w", ctx.Err())
		case <-timer.C:
		}
	}
}

func listSessionSummaries(store *session.Store, tag string) ([]session.Summary, error) {
	tag = strings.TrimSpace(tag)
	if tag == "" {
		summaries, err := store.List()
		if err != nil {
			return nil, fmt.Errorf("list all sessions: %w", err)
		}
		return summaries, nil
	}
	summaries, err := store.ListByTag(tag)
	if err != nil {
		return nil, fmt.Errorf("list sessions by tag %q: %w", tag, err)
	}
	return summaries, nil
}

func listAgentPerformance(store *session.Store) error {
	summaries, err := store.AgentPerformanceSummary()
	if err != nil {
		return fmt.Errorf("agent performance summary: %w", err)
	}
	if len(summaries) == 0 {
		fmt.Println("No agent performance records found.")
		return nil
	}
	for i := range summaries {
		fmt.Println(formatAgentPerformanceSummary(summaries[i]))
	}
	return nil
}

func formatAgentPerformanceSummary(summary session.AgentPerformanceSummary) string {
	parts := []string{
		"agent=" + summary.Agent,
		"evaluations=" + strconv.Itoa(summary.EvaluationCount),
		"failures=" + strconv.Itoa(summary.FailureCount),
		"negative_knowledge=" + strconv.Itoa(summary.NegativeKnowledgeCount),
		"default_agent_sessions=" + strconv.Itoa(summary.DefaultAgentSessionCount),
	}
	if summary.ScoredEvaluationCount > 0 {
		parts = append(parts,
			"scored="+strconv.Itoa(summary.ScoredEvaluationCount),
			fmt.Sprintf("avg_score=%.2f", summary.AverageScore),
			"min_score="+strconv.Itoa(summary.MinScore),
			"max_score="+strconv.Itoa(summary.MaxScore),
		)
	}
	if len(summary.Outcomes) > 0 {
		parts = append(parts, "outcomes="+formatOutcomeCounts(summary.Outcomes))
	}
	if !summary.LatestActivity.IsZero() {
		parts = append(parts, "latest="+summary.LatestActivity.UTC().Format(time.RFC3339))
	}
	return strings.Join(parts, "\t")
}

func formatOutcomeCounts(outcomes []session.OutcomeCount) string {
	parts := make([]string, 0, len(outcomes))
	for _, outcome := range outcomes {
		parts = append(parts, outcome.Outcome+":"+strconv.Itoa(outcome.Count))
	}
	return strings.Join(parts, ",")
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

func listHookEvents(jsonOutput bool) error {
	if jsonOutput {
		if err := json.NewEncoder(os.Stdout).Encode(events.SupportedEventTypes()); err != nil {
			return fmt.Errorf("list hook events: encode JSON: %w", err)
		}
		return nil
	}
	for _, eventType := range events.SupportedEventTypes() {
		fmt.Println(formatHookEventType(eventType))
	}
	return nil
}

func formatHookEventType(eventType events.SupportedEventType) string {
	return eventType.Type + "\t" + eventType.Description
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

func formatHeadlessRun(run session.HeadlessRun) string {
	started := "-"
	if !run.StartedAt.IsZero() {
		started = run.StartedAt.UTC().Format(time.RFC3339)
	}
	updated := "-"
	if !run.UpdatedAt.IsZero() {
		updated = run.UpdatedAt.UTC().Format(time.RFC3339)
	}
	agentName := fallbackDash(run.Agent)
	modelName := fallbackDash(run.Model)
	parts := []string{
		run.ID,
		"status=" + string(run.Status),
		"session=" + fallbackDash(run.SessionID),
		"agent=" + agentName,
		"model=" + modelName,
		"started=" + started,
		"updated=" + updated,
		"log=" + fallbackDash(run.LogPath),
	}
	if run.Error != "" {
		parts = append(parts, "error="+run.Error)
	}
	return strings.Join(parts, "\t")
}

func fallbackDash(value string) string {
	if strings.TrimSpace(value) == "" {
		return "-"
	}
	return value
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
	CreatedAt         time.Time                   `yaml:"created_at"`
	UpdatedAt         time.Time                   `yaml:"updated_at"`
	ID                string                      `yaml:"id"`
	Path              string                      `yaml:"path"`
	Title             string                      `yaml:"title,omitempty"`
	DefaultAgent      string                      `yaml:"default_agent,omitempty"`
	DefaultModel      string                      `yaml:"default_model,omitempty"`
	WorktreePath      string                      `yaml:"worktree_path,omitempty"`
	WorktreeBranch    string                      `yaml:"worktree_branch,omitempty"`
	WorktreeBase      string                      `yaml:"worktree_base,omitempty"`
	Tags              []string                    `yaml:"tags,omitempty"`
	Messages          []yamlMessage               `yaml:"messages,omitempty"`
	NegativeKnowledge []session.NegativeKnowledge `yaml:"negative_knowledge,omitempty"`
	Evaluations       []session.AgentEvaluation   `yaml:"evaluations,omitempty"`
	Artifacts         []session.Artifact          `yaml:"artifacts,omitempty"`
}

type yamlMessage struct {
	Role    llm.Role `yaml:"role"`
	Content string   `yaml:"content"`
}

func printSessionSummary(sessionState session.Session, path string) {
	fmt.Println(formatSessionDetailsSummary(sessionState, path))
}

func formatSessionDetailsSummary(sessionState session.Session, path string) string {
	parts := []string{
		"id=" + sessionState.ID,
		"path=" + path,
		"messages=" + strconv.Itoa(len(sessionState.Messages)),
		"failures=" + strconv.Itoa(len(sessionState.NegativeKnowledge)),
		"evaluations=" + strconv.Itoa(len(sessionState.Evaluations)),
		"artifacts=" + strconv.Itoa(len(sessionState.Artifacts)),
	}
	if !sessionState.CreatedAt.IsZero() {
		parts = append(parts, "created_at="+sessionState.CreatedAt.Format(time.RFC3339))
	}
	if !sessionState.UpdatedAt.IsZero() {
		parts = append(parts, "updated_at="+sessionState.UpdatedAt.Format(time.RFC3339))
	}
	if sessionState.Title != "" {
		parts = append(parts, "title="+sessionState.Title)
	}
	if sessionState.DefaultAgent != "" {
		parts = append(parts, "agent="+sessionState.DefaultAgent)
	}
	if sessionState.DefaultModel != "" {
		parts = append(parts, "model="+sessionState.DefaultModel)
	}
	if len(sessionState.Tags) > 0 {
		parts = append(parts, "tags="+strings.Join(sessionState.Tags, ","))
	}
	return strings.Join(parts, "	")
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
		NegativeKnowledge: append(
			[]session.NegativeKnowledge(nil),
			sessionState.NegativeKnowledge...,
		),
		Evaluations: append([]session.AgentEvaluation(nil), sessionState.Evaluations...),
		Artifacts:   append([]session.Artifact(nil), sessionState.Artifacts...),
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

func llmConfig(cfg appconfig.Config, selectedModel string) llm.AutoRegisterConfig {
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
		SelectedModel:   selectedModel,
		Providers:       providers,
	}
}

func generationFromConfig(cfg appconfig.Config) generationSettings {
	return generationSettings{
		Temperature:    cfg.Generation.Temperature,
		TopP:           cfg.Generation.TopP,
		Seed:           cfg.Generation.Seed,
		ReasoningLevel: strings.TrimSpace(cfg.Generation.ReasoningLevel),
		MaxTokens:      cfg.Generation.MaxTokens,
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
	if opts.seed.set {
		generation.Seed = &opts.seed.value
	}
	if opts.maxTokens.set {
		generation.MaxTokens = opts.maxTokens.value
	}
	if strings.TrimSpace(opts.reasoningLevel) != "" {
		generation.ReasoningLevel = strings.TrimSpace(opts.reasoningLevel)
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
			Temperature:    activeAgent.agent.Temperature,
			TopP:           activeAgent.agent.TopP,
			Seed:           activeAgent.agent.Seed,
			ReasoningLevel: activeAgent.agent.ReasoningLevel,
			MaxTokens:      activeAgent.agent.MaxTokens,
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
	if override.Seed != nil {
		base.Seed = override.Seed
	}
	if override.ReasoningLevel != "" {
		base.ReasoningLevel = strings.TrimSpace(override.ReasoningLevel)
	}
	if override.MaxTokens > 0 {
		base.MaxTokens = override.MaxTokens
	}
	return base
}

func applyGenerationParams(params *llm.CompleteParams, generation generationSettings) {
	params.Temperature = generation.Temperature
	params.TopP = generation.TopP
	params.Seed = generation.Seed
	params.ReasoningLevel = generation.ReasoningLevel
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

func maxInputTokensFromConfigOptions(cfg appconfig.Config, opts cliOptions) int {
	if opts.maxInputTokens.set {
		return opts.maxInputTokens.value
	}
	return cfg.Context.MaxInputTokens
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
func finalizeWorktree(ctx context.Context, state *appState) {
	ctx = nonNilContext(ctx)
	if state.worktreeInfo == nil {
		return
	}

	if !state.autoMergeWorktree {
		fmt.Fprintln(os.Stderr, "worktree: session files are in "+state.worktreeInfo.Path)
		fmt.Fprintln(os.Stderr, "worktree: merge with: atteler --merge-worktree "+state.sessionState.ID)
		return
	}

	fmt.Fprintln(os.Stderr, "worktree: merging "+state.worktreeInfo.Branch+" into "+state.worktreeInfo.BaseBranch+"...")

	if err := worktree.MergeContext(ctx, state.cwd, state.worktreeInfo); err != nil {
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

func listWorktrees(ctx context.Context) error {
	ctx = nonNilContext(ctx)
	cwd, err := os.Getwd()
	if err != nil {
		return fmt.Errorf("list worktrees: %w", err)
	}
	if !worktree.IsGitRepoContext(ctx, cwd) {
		return errors.New("list worktrees: not inside a git repository")
	}

	infos, err := worktree.ListContext(ctx, cwd)
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

func mergeWorktreeBySession(ctx context.Context, sessionRef string) error {
	ctx = nonNilContext(ctx)
	cwd, err := os.Getwd()
	if err != nil {
		return fmt.Errorf("merge worktree: %w", err)
	}
	if !worktree.IsGitRepoContext(ctx, cwd) {
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
	if err := worktree.MergeContext(ctx, cwd, info); err != nil {
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
