// Package main is the entry point for the atteler TUI application.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"maps"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/charmbracelet/bubbles/key"
	"github.com/charmbracelet/bubbles/textarea"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/charmbracelet/x/ansi"
	"gopkg.in/yaml.v3"

	"github.com/tommoulard/atteler/pkg/agent"
	"github.com/tommoulard/atteler/pkg/agentmemory"
	attasync "github.com/tommoulard/atteler/pkg/async"
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
	"github.com/tommoulard/atteler/pkg/tasklist"
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

var runInteractiveProgram = func(m model) (tea.Model, error) {
	return tea.NewProgram(m).Run()
}

var (
	promptStyle = lipgloss.NewStyle().
			Foreground(lipgloss.Color("170")).
			Bold(true)

	statusStyle = lipgloss.NewStyle().
			Foreground(lipgloss.Color("241"))

	errStyle = lipgloss.NewStyle().
			Foreground(lipgloss.Color("196"))

	warnStyle = lipgloss.NewStyle().
			Foreground(lipgloss.Color("214"))

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
	keyDown          = "down"
	keyEnter         = "enter"
	keyEsc           = "esc"
	keyUp            = "up"
	outputFormatJSON = "json"
	outputFormatText = "text"
	statusError      = "error"

	maxPromptHistoryEntries = 100
	taskTickInterval        = time.Second
	idleSuggestionDelay     = time.Second
	idleSuggestionTimeout   = 8 * time.Second
)

var terminalTitleSpinnerFrames = []string{"⠋", "⠙", "⠹", "⠸", "⠼", "⠴", "⠦", "⠧", "⠇", "⠏"}

// ---------------------------------------------------------------------------
// Messages (tea.Msg)
// ---------------------------------------------------------------------------

// llmResponseMsg is sent when the LLM call completes.
type llmResponseMsg struct {
	err         error
	completedAt time.Time
	content     string
	model       string
	eventLines  []string
	toolLog     []string // tool call summaries (command + truncated output)
	tokenUsage  tokenUsage
}

// shellResultMsg is sent when a `!command` finishes executing.
type shellResultMsg struct {
	err         error
	completedAt time.Time
	command     string
	stdout      string
	stderr      string
}

type idleSuggestionRequestMsg struct {
	input string
	id    int
}

type idleSuggestionMsg struct {
	err        error
	input      string
	suggestion string
	id         int
}

type taskTickMsg struct {
	id int
}

// loopCheckpointMsg is sent from the agent loop goroutine when it reaches a
// checkpoint interval. The TUI displays a prompt and sends the user's answer
// back on responseCh.
type loopCheckpointMsg struct {
	responseCh chan<- bool
	requestCh  <-chan int // kept so we can re-listen after confirming
	iterations int
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
	eventBase        events.Event
	hookRunner       *events.Runner
	generation       generationSettings
	maxInputTokens   int
	model            string
	referenceContext string
	workingDir       string
	messages         []llm.Message
	fallbackModels   []string
	refs             []contextref.Reference
	agent            agent.Agent
	hasAgent         bool
	useTools         bool

	// confirmContinueCh is used by the agent loop to ask the caller whether
	// to continue when a checkpoint interval is reached. The agent loop
	// goroutine sends the iteration count on this channel and blocks until
	// it receives a boolean on confirmResponseCh.
	confirmContinueCh chan int
	confirmResponseCh chan bool
}

type completionCandidate struct {
	label string
	value string
	kind  string
}

// ---------------------------------------------------------------------------
// Model
// ---------------------------------------------------------------------------

//nolint:govet,recvcheck // Field order groups related TUI state; task helpers mutate the local Bubble Tea model copy before returning it.
type model struct {
	ctx                 context.Context
	textarea            textarea.Model
	registry            *llm.Registry
	agentRegistry       *agent.Registry
	hookRunner          *events.Runner
	sessionStore        *session.Store
	stateStore          *appconfig.StateStore
	cancel              context.CancelFunc
	pickerCancel        context.CancelFunc
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
	queuedPrompts       []string
	promptHistoryDraft  string
	pickerItems         []pickerItem
	contextOptions      contextref.Options
	referenceContext    string
	worktreeInfo        *worktree.Info
	tokenUsage          tokenUsage
	runningTaskStarted  time.Time
	idleSuggestionInput string
	idleSuggestionText  string
	pickerCursor        int
	idleSuggestionID    int
	terminalTitleFrame  int
	modelFetchID        int
	modelFetchesPending int
	completionCursor    int
	promptHistoryCursor int
	runningTaskID       int
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
	runningTaskLabel    string
	revampUndo          string

	// checkpointResponseCh is non-nil when the TUI is waiting for the user
	// to confirm whether to continue the agent loop. The Y/N key handler
	// sends the answer and nils this field.
	checkpointResponseCh chan<- bool
	checkpointRequestCh  <-chan int
	checkpointIterations int
	pinnedMessages       map[int]bool
	executionMode        string
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
	referenceContext string,
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
		ctx:                 ctx,
		registry:            reg,
		agentRegistry:       agents,
		hookRunner:          hooks,
		sessionStore:        store,
		stateStore:          stateStore,
		sessionState:        sessionState,
		contextOptions:      contextOptions,
		referenceContext:    referenceContext,
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
		pinnedMessages:      make(map[int]bool),
		executionMode:       "execute",
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
		// When waiting for user confirmation on an agent loop checkpoint,
		// intercept Y/N before any other key handler.
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

	case llmResponseMsg:
		return m.updateLLMResponse(msg)

	case shellResultMsg:
		return m.updateShellResult(msg)

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

// updateLoopCheckpoint handles the agent loop reaching a checkpoint. It shows
// a prompt and waits for the user to press Y or N.
func (m model) updateLoopCheckpoint(msg loopCheckpointMsg) (tea.Model, tea.Cmd) {
	m.checkpointResponseCh = msg.responseCh
	m.checkpointRequestCh = msg.requestCh
	m.checkpointIterations = msg.iterations

	prompt := fmt.Sprintf(
		"Agent loop reached %d iterations. Continue? [Y/n] ",
		msg.iterations,
	)

	return m, tea.Batch(tea.Println(warnStyle.Render(prompt)), tea.SetWindowTitle(terminalIdleTitle()))
}

// handleCheckpointKey handles Y/N key presses during a checkpoint prompt.
// Y (or Enter) continues the loop, N (or Esc) stops it.
func (m model) handleCheckpointKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	ch := m.checkpointResponseCh
	reqCh := m.checkpointRequestCh

	switch msg.String() {
	case "y", "Y", "enter":
		m.checkpointResponseCh = nil
		m.checkpointRequestCh = nil
		m.checkpointIterations = 0

		ch <- true

		// Re-listen for the next checkpoint.
		return m, tea.Batch(
			tea.Println(dimStyle.Render("Continuing agent loop...")),
			tea.SetWindowTitle(m.terminalWorkingTitle()),
			taskTickCmd(m.runningTaskID),
			relistenForCheckpoint(reqCh, ch),
		)

	case "n", "N", "esc":
		m.checkpointResponseCh = nil
		m.checkpointRequestCh = nil
		m.checkpointIterations = 0

		ch <- false

		return m, tea.Batch(tea.Println(warnStyle.Render("Stopping agent loop.")), tea.SetWindowTitle(m.terminalWorkingTitle()))

	default:
		// Ignore other keys; keep waiting for Y/N.
		return m, nil
	}
}

// relistenForCheckpoint wraps the response channel back into a bidirectional
// chan so listenForCheckpoint can be reused for subsequent checkpoints.
func relistenForCheckpoint(requestCh <-chan int, responseCh chan<- bool) tea.Cmd {
	return func() tea.Msg {
		iterations, ok := <-requestCh
		if !ok {
			return nil
		}

		return loopCheckpointMsg{
			iterations: iterations,
			responseCh: responseCh,
			requestCh:  requestCh,
		}
	}
}

func (m model) updateTextarea(msg tea.Msg) (tea.Model, tea.Cmd) {
	var cmds []tea.Cmd

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

	requestMessages, refs, err := expandReferences(nextHistory, m.contextOptions)
	if err != nil {
		return m, tea.Println(errStyle.Render("Error: " + err.Error()))
	}

	// Print the user message above the input area.
	line := userLabel.Render("You") + " " + input
	if activeAgent.name != "" {
		line = userLabel.Render("You") + dimStyle.Render(" (@"+activeAgent.name+") ") + input
	}

	msgs := make([]llm.Message, len(requestMessages))
	copy(msgs, requestMessages)

	requestModel, fallbackModels := requestModelAndFallbacks(m.selectedModel, m.modelLocked, m.fallbackModels, activeAgent)

	generation := generationForRequest(m.generationDefaults, m.generationOverrides, activeAgent)
	if err := validateRequestBudget(m.registry, requestModel, requestMessagesForBudget(requestModel, msgs, activeAgent, generation), m.maxInputTokens); err != nil {
		return m, tea.Println(errStyle.Render("Error: " + err.Error()))
	}

	// Launch the LLM call.
	// cancel is stored in m.cancel and invoked from handleCtrlC and
	// updateLLMResponse once the request finishes; gosec can't see that.
	ctx, cancel := context.WithCancel(m.ctx) //nolint:gosec // see comment above
	m.cancel = cancel
	tickCmd := m.startRunningTask("LLM")

	m.history = nextHistory
	m.sessionState.Messages = append([]llm.Message(nil), m.history...)

	m.sessionState.DefaultAgent = activeAgent.name
	if m.selectedModel != "" {
		m.sessionState.DefaultModel = m.selectedModel
	}

	m.sessionState.DefaultReasoningLevel = strings.TrimSpace(m.generationOverrides.ReasoningLevel)

	confirmCh := make(chan int, 1)
	responseCh := make(chan bool, 1)

	request := llmRequest{
		eventBase: events.Event{
			SessionID:   m.sessionState.ID,
			SessionPath: m.sessionPath,
			Agent:       activeAgent.name,
			Model:       requestModel,
		},
		hookRunner:        m.hookRunner,
		agent:             activeAgent.agent,
		hasAgent:          activeAgent.ok,
		model:             requestModel,
		referenceContext:  buildReferenceContext(m.ctx, m.referenceContext, activeAgent, m.contextOptions),
		workingDir:        m.cwd,
		messages:          msgs,
		fallbackModels:    fallbackModels,
		generation:        generation,
		maxInputTokens:    m.maxInputTokens,
		refs:              refs,
		useTools:          m.executionMode != "plan",
		confirmContinueCh: confirmCh,
		confirmResponseCh: responseCh,
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
		callLLM(ctx, m.registry, request),
	)

	return m, tea.Batch(tea.Sequence(cmds...), tickCmd, listenForCheckpoint(confirmCh, responseCh))
}

// runShellCommand executes a `!command` and queues the result for display
// and inclusion in the chat history (so future LLM calls see it as context).
// When the command is interactive (e.g. vim, less), the TUI suspends and the
// command takes over the terminal via tea.ExecProcess.
func (m model) runShellCommand(command string) (tea.Model, tea.Cmd) {
	if command == "" {
		return m, nil
	}

	line := userLabel.Render("$") + " " + command

	if isInteractiveCommand(command) {
		return m.runInteractiveShellCommand(command, line)
	}

	// cancel is stored in m.cancel and invoked from handleCtrlC and
	// updateShellResult once the command finishes; gosec can't see that.
	ctx, cancel := context.WithCancel(m.ctx) //nolint:gosec // see comment above
	m.cancel = cancel
	tickCmd := m.startRunningTask("command")

	return m, tea.Batch(tea.Sequence(
		tea.Println(line),
		emitHook(m.ctx, m.hookRunner, events.Event{
			Type:        events.CommandExecute,
			SessionID:   m.sessionState.ID,
			SessionPath: m.sessionPath,
			Agent:       m.selectedAgent,
			Model:       m.sessionState.DefaultModel,
			Content:     command,
			Metadata: map[string]string{
				"command": command,
				"cwd":     m.cwd,
				"input":   "!" + command,
				"source":  "user",
			},
		}),
		runShellCommandCmd(ctx, command, m.cwd),
	), tickCmd)
}

// runInteractiveShellCommand hands the terminal to a child process via
// tea.ExecProcess so interactive programs (vim, less, htop, nested atteler)
// can use the PTY directly.
func (m model) runInteractiveShellCommand(command, line string) (model, tea.Cmd) {
	cmd := exec.CommandContext(m.ctx, "bash", "-lc", command)
	if m.cwd != "" {
		cmd.Dir = m.cwd
	}

	return m, tea.Sequence(
		tea.Println(line),
		emitHook(m.ctx, m.hookRunner, events.Event{
			Type:        events.CommandExecute,
			SessionID:   m.sessionState.ID,
			SessionPath: m.sessionPath,
			Agent:       m.selectedAgent,
			Model:       m.sessionState.DefaultModel,
			Content:     command,
			Metadata: map[string]string{
				"command": command,
				"cwd":     m.cwd,
				"input":   "!" + command,
				"mode":    "interactive",
				"source":  "user",
			},
		}),
		tea.ExecProcess(cmd, func(err error) tea.Msg {
			exitError := ""
			if err != nil {
				exitError = err.Error()
			}

			return shellResultMsg{
				err:         err,
				completedAt: time.Now(),
				command:     command,
				stdout:      "(interactive session" + exitErrorSuffix(exitError) + ")",
			}
		}),
	)
}

// interactiveCommands is the set of commands known to require a PTY.
var interactiveCommands = map[string]struct{}{
	"vim": {}, "nvim": {}, "vi": {}, "nano": {}, "emacs": {},
	"less": {}, "more": {}, "top": {}, "htop": {}, "btop": {},
	"ssh": {}, "tmux": {}, "screen": {},
	"atteler": {}, "python": {}, "python3": {}, "node": {}, "irb": {},
}

// prependToolReminder injects a system message that tells the model which
// tools are available. This prevents the LLM from refusing tool use when
// the agent's system prompt mentions tools (e.g. "Edit tool", "Read tool")
// that are not actually wired up -- the model might otherwise conclude its
// tool environment is broken and fall back to plain text.
func prependToolReminder(params *llm.CompleteParams, tools []llm.ToolDefinition) {
	var names []string
	for _, t := range tools {
		names = append(names, t.Name)
	}

	reminder := llm.Message{
		Role: "system",
		Content: "You have the following tools available and MUST use them " +
			"when the task requires running commands or inspecting files: " +
			strings.Join(names, ", ") + ". " +
			"Do NOT say you are unable to run commands. " +
			"Use the bash tool to execute shell commands.",
	}

	// Prepend so the reminder sits right before the conversation history.
	params.Messages = append([]llm.Message{reminder}, params.Messages...)
}

// listenForCheckpoint returns a tea.Cmd that waits for the agent loop to
// request a checkpoint confirmation. When the loop sends the iteration count
// on requestCh, this produces a loopCheckpointMsg for the TUI. The goroutine
// exits when requestCh is closed (i.e. when callLLMWithTools finishes).
func listenForCheckpoint(requestCh <-chan int, responseCh chan bool) tea.Cmd {
	return func() tea.Msg {
		iterations, ok := <-requestCh
		if !ok {
			// Channel closed -- agent loop finished without hitting a checkpoint.
			return nil
		}

		return loopCheckpointMsg{
			iterations: iterations,
			responseCh: responseCh,
			requestCh:  requestCh,
		}
	}
}

// isInteractiveCommand returns true when the command's base name is a known
// interactive program or the command is prefixed with "!!" as a user hint.
func isInteractiveCommand(command string) bool {
	if strings.HasPrefix(command, "!") {
		return true // "!!" prefix signals interactive mode
	}

	base := strings.Fields(command)
	if len(base) == 0 {
		return false
	}

	name := filepath.Base(base[0])
	_, ok := interactiveCommands[name]

	return ok
}

func exitErrorSuffix(exitError string) string {
	if exitError == "" {
		return ""
	}

	return ": " + exitError
}

func runShellCommandCmd(ctx context.Context, command, dir string) tea.Cmd {
	return func() tea.Msg {
		result, err := attshell.RunBash(ctx, attshell.Options{
			Command: command,
			Dir:     dir,
		})

		return shellResultMsg{
			err:         err,
			completedAt: time.Now(),
			command:     command,
			stdout:      result.Stdout,
			stderr:      result.Stderr,
		}
	}
}

// updateShellResult appends the executed command and its output to the chat
// history as a synthetic user message and prints the output.
func (m model) updateShellResult(msg shellResultMsg) (tea.Model, tea.Cmd) {
	m.waiting = false
	m.cancel = nil
	elapsed := m.finishRunningTask(msg.completedAt)

	content := formatShellContext(msg)
	outputEvent := commandOutputEvent(
		m.sessionState.ID,
		m.sessionPath,
		m.selectedAgent,
		m.sessionState.DefaultModel,
		m.cwd,
		msg.command,
		content,
		msg.err,
		map[string]string{"source": "user"},
	)
	m.history = append(m.history, llm.Message{
		Role:    llm.RoleUser,
		Content: content,
	})
	m.sessionState.Messages = append([]llm.Message(nil), m.history...)

	cmds := []tea.Cmd{tea.SetWindowTitle(terminalIdleTitle()), emitHook(m.ctx, m.hookRunner, outputEvent)}
	if msg.stdout != "" {
		cmds = append(cmds, tea.Println(strings.TrimRight(msg.stdout, "\n")))
	}

	if msg.stderr != "" {
		cmds = append(cmds, tea.Println(errStyle.Render(strings.TrimRight(msg.stderr, "\n"))))
	}

	if msg.err != nil {
		cmds = append(cmds, tea.Println(errStyle.Render("(command error: "+msg.err.Error()+")")))
	}

	if elapsed > 0 {
		cmds = append(cmds, tea.Println(dimStyle.Render("(command ran for "+formatTaskDuration(elapsed)+")")))
	}

	cmds = append(cmds, saveSession(m.ctx, m.sessionStore, m.sessionState, m.hookRunner))

	return m.continueWithQueuedPrompt(tea.Sequence(cmds...))
}

// formatShellContext renders an executed shell command and its output as a
// chat-history entry that future LLM calls can use as context.
func formatShellContext(msg shellResultMsg) string {
	var b strings.Builder
	fmt.Fprintf(&b, "$ %s\n", msg.command)

	if msg.stdout != "" {
		b.WriteString(strings.TrimRight(msg.stdout, "\n"))
		b.WriteString("\n")
	}

	if msg.stderr != "" {
		b.WriteString("[stderr]\n")
		b.WriteString(strings.TrimRight(msg.stderr, "\n"))
		b.WriteString("\n")
	}

	if msg.err != nil {
		fmt.Fprintf(&b, "[error] %s\n", msg.err.Error())

		// Include a recovery hint for timeouts so the LLM can reason about
		// retry strategies when this context appears in subsequent prompts.
		if strings.Contains(msg.err.Error(), "timed out") {
			b.WriteString("[timeout] The command exceeded its time limit. " +
				"Consider retrying with a smaller scope or splitting the work.\n")
		}
	}

	return strings.TrimRight(b.String(), "\n")
}

func commandOutputEvent(
	sessionID, sessionPath, agentName, modelName, cwd, command, content string,
	err error,
	extra map[string]string,
) events.Event {
	metadata := map[string]string{
		"command": command,
		"cwd":     cwd,
	}
	maps.Copy(metadata, extra)

	event := events.Event{
		Type:        events.CommandOutput,
		SessionID:   sessionID,
		SessionPath: sessionPath,
		Agent:       agentName,
		Model:       modelName,
		Content:     content,
		Metadata:    metadata,
	}
	if err != nil {
		event.Error = err.Error()
	}

	return event
}

type agentSelection struct {
	name  string
	agent agent.Agent
	ok    bool
}

func (m model) resolveAgent(input string) (agentSelection, string, error) {
	return resolveAgent(m.agentRegistry, m.selectedAgent, input)
}

// updateLLMResponse handles the message received when an LLM call completes.
func (m model) updateLLMResponse(msg llmResponseMsg) (tea.Model, tea.Cmd) {
	m.waiting = false

	m.cancel = nil
	elapsed := m.finishRunningTask(msg.completedAt)

	cmds := append(eventLineCommands(msg.eventLines), tea.SetWindowTitle(terminalIdleTitle()))
	if msg.err != nil {
		errorLine := "Error: " + msg.err.Error()
		if elapsed > 0 {
			errorLine += " (ran for " + formatTaskDuration(elapsed) + ")"
		}

		cmds = append(
			cmds,
			tea.Println(errStyle.Render(errorLine)),
			emitHook(m.ctx, m.hookRunner, events.Event{
				Type:        events.Error,
				SessionID:   m.sessionState.ID,
				SessionPath: m.sessionPath,
				Agent:       m.sessionState.DefaultAgent,
				Model:       m.sessionState.DefaultModel,
				Error:       msg.err.Error(),
			}),
		)

		return m.continueWithQueuedPrompt(tea.Sequence(cmds...))
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
	if elapsed > 0 {
		header += dimStyle.Render(" (ran for " + formatTaskDuration(elapsed) + ")")
	}

	if len(msg.toolLog) > 0 {
		header += dimStyle.Render(fmt.Sprintf(" [%d tool calls]", len(msg.toolLog)))
	}

	// Print tool call logs before the final response.
	for _, entry := range msg.toolLog {
		cmds = append(cmds, tea.Println(dimStyle.Render("  "+entry)))
	}

	cmds = append(
		cmds,
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

	return m.continueWithQueuedPrompt(tea.Sequence(cmds...))
}

func (m model) continueWithQueuedPrompt(current tea.Cmd) (tea.Model, tea.Cmd) {
	if len(m.queuedPrompts) == 0 {
		return m, current
	}

	nextPrompt := m.queuedPrompts[0]
	m.queuedPrompts = append([]string(nil), m.queuedPrompts[1:]...)

	nextModel, nextCmd := m.submitPrompt(nextPrompt)

	next, ok := nextModel.(model)
	if !ok {
		return m, current
	}

	return next, tea.Sequence(current, nextCmd)
}

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

func (m *model) clearIdleSuggestion() {
	m.idleSuggestionID++
	m.idleSuggestionInput = ""
	m.idleSuggestionText = ""
}

func (m *model) scheduleIdleSuggestion() tea.Cmd {
	value := m.textarea.Value()
	if m.waiting ||
		m.registry == nil ||
		strings.TrimSpace(value) == "" ||
		textareaCursorOffset(m.textarea) != len(value) {
		return nil
	}

	m.idleSuggestionID++
	m.idleSuggestionInput = value
	m.idleSuggestionText = ""
	id := m.idleSuggestionID

	return tea.Tick(idleSuggestionDelay, func(time.Time) tea.Msg {
		return idleSuggestionRequestMsg{id: id, input: value}
	})
}

func (m model) updateIdleSuggestionRequest(msg idleSuggestionRequestMsg) (tea.Model, tea.Cmd) {
	if msg.id != m.idleSuggestionID ||
		msg.input != m.idleSuggestionInput ||
		msg.input != m.textarea.Value() ||
		m.waiting ||
		m.registry == nil {
		return m, nil
	}

	return m, requestIdleSuggestion(m.ctx, m.registry, m.selectedModel, m.fallbackModels, m.generationDefaults, m.generationOverrides, msg.id, msg.input)
}

func (m model) updateIdleSuggestion(msg idleSuggestionMsg) (tea.Model, tea.Cmd) {
	if msg.id != m.idleSuggestionID ||
		msg.input != m.idleSuggestionInput ||
		msg.input != m.textarea.Value() ||
		textareaCursorOffset(m.textarea) != len(m.textarea.Value()) {
		return m, nil
	}

	if msg.err != nil {
		// Idle suggestions are opportunistic; avoid interrupting the user for
		// provider/network failures.
		slog.Debug("idle prompt suggestion failed", "error", msg.err)
		return m, nil
	}

	m.idleSuggestionText = normalizeIdleSuggestion(msg.input, msg.suggestion)

	return m, nil
}

func (m model) visibleIdleSuggestion() (string, bool) {
	value := m.textarea.Value()
	if strings.TrimSpace(value) == "" ||
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
	id int,
	input string,
) tea.Cmd {
	return func() tea.Msg {
		if ctx == nil {
			return idleSuggestionMsg{id: id, input: input, err: errors.New("idle suggestion: context is required")}
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
						"Do not repeat the current text, do not add explanations, and return an empty response if no useful suffix exists.",
				},
				{
					Role:    llm.RoleUser,
					Content: "Current text:\n" + input,
				},
			},
		}
		applyGenerationParams(&params, generation)

		resp, err := reg.CompleteWithFallback(reqCtx, params, fallbackModels)
		if err != nil {
			return idleSuggestionMsg{id: id, input: input, err: err}
		}

		return idleSuggestionMsg{id: id, input: input, suggestion: resp.Content}
	}
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

// loadModels starts one background fetch per provider so the picker can update
// incrementally as live model catalogs return.

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

	// Bound the number of sessions loaded from disk. The list is already
	// sorted by UpdatedAt descending, so we scan only the most recent ones.
	const maxSessionsToScan = 20

	scanned := 0
	for i := range summaries {
		if scanned >= maxSessionsToScan || len(out) >= limit {
			break
		}

		summary := &summaries[i]
		if summary.ID == current.ID {
			continue
		}

		saved, err := store.Load(summary.ID)
		if err != nil {
			continue
		}

		scanned++

		out = appendUserPromptsNewestFirst(out, seen, saved.Messages, limit)
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
// registry default. When useTools is true, the call runs an agentic loop
// that lets the LLM invoke tools (bash commands) iteratively.
func callLLM(ctx context.Context, reg *llm.Registry, request llmRequest) tea.Cmd {
	return func() tea.Msg {
		eventLines := newEventLineBuffer()
		ctx = events.WithEmitter(
			ctx,
			request.hookRunner.WithLogger(eventLines),
			request.eventBase,
		)

		params := llm.CompleteParams{
			Model:    request.model,
			Messages: request.messages,
		}
		if request.hasAgent {
			params = request.agent.CompleteParams(request.model, request.messages)
		}

		prependReferenceContext(&params, request.referenceContext)

		applyGenerationParams(&params, request.generation)

		if err := validateRequestBudget(reg, params.Model, params.Messages, request.maxInputTokens); err != nil {
			return llmResponseMsg{err: err, completedAt: time.Now(), eventLines: eventLines.Lines()}
		}

		// When tools are enabled, run the agentic loop.
		if request.useTools {
			return callLLMWithTools(ctx, reg, params, request, eventLines)
		}

		resp, err := reg.CompleteWithFallback(ctx, params, request.fallbackModels)
		if err != nil {
			return llmResponseMsg{err: err, completedAt: time.Now(), eventLines: eventLines.Lines()}
		}

		var usage tokenUsage
		usage.addResponse(resp)

		return llmResponseMsg{
			completedAt: time.Now(),
			content:     resp.Content,
			model:       resp.Model,
			eventLines:  eventLines.Lines(),
			tokenUsage:  usage,
		}
	}
}

// callLLMWithTools runs an agent loop where the LLM can execute bash commands.
func callLLMWithTools(
	ctx context.Context,
	reg *llm.Registry,
	params llm.CompleteParams,
	request llmRequest,
	eventLines *eventLineBuffer,
) llmResponseMsg {
	tools := llm.DefaultTools()
	if request.hasAgent {
		tools = request.agent.FilterTools(tools)
	}

	params.Tools = tools

	// Inject a tool-availability reminder so the model knows it can (and
	// should) use the bash tool, even when the agent's system prompt
	// mentions other tools that are not wired up in this environment.
	if len(tools) > 0 {
		prependToolReminder(&params, tools)
	}

	toolNames := make([]string, 0, len(tools))
	for _, t := range tools {
		toolNames = append(toolNames, t.Name)
	}

	slog.Debug("callLLMWithTools",
		"agent", request.agent.Name,
		"hasAgent", request.hasAgent,
		"model", params.Model,
		"tools", toolNames,
		"messages", len(params.Messages),
	)

	var toolLog []string

	executor := func(ctx context.Context, call llm.ToolCall) llm.ToolResult {
		if call.Name != "bash" {
			return llm.ToolResult{
				ToolCallID: call.ID,
				Content:    "unknown tool: " + call.Name,
				IsError:    true,
			}
		}

		command, ok := call.Input["command"].(string)
		if !ok || command == "" {
			return llm.ToolResult{
				ToolCallID: call.ID,
				Content:    "error: empty command",
				IsError:    true,
			}
		}

		emitFromContextWarning(ctx, events.Event{
			Type:    events.CommandExecute,
			Content: command,
			Metadata: map[string]string{
				"command":      command,
				"cwd":          request.workingDir,
				"input":        command,
				"source":       "llm_tool",
				"tool_call_id": call.ID,
			},
		})

		result, err := attshell.RunBash(ctx, attshell.Options{
			Command: command,
			Dir:     request.workingDir,
			Timeout: 5 * time.Minute, // Generous timeout for tool calls.
		})

		output := formatShellContext(shellResultMsg{
			command: command,
			stdout:  result.Stdout,
			stderr:  result.Stderr,
			err:     err,
		})
		toolLog = append(toolLog, output)
		emitFromContextWarning(ctx, commandOutputEvent(
			"", "", "", "", request.workingDir, command, output, err,
			map[string]string{
				"source":       "llm_tool",
				"tool_call_id": call.ID,
			},
		))

		if err != nil {
			content := output
			// When the command timed out, append a recovery hint so the LLM
			// can decide to retry with a smaller scope or take corrective
			// action autonomously.
			if strings.Contains(err.Error(), "timed out") {
				content += "\n\n[TIMEOUT RECOVERY] The command timed out after the configured limit. " +
					"Consider: (1) retrying with a smaller scope or simpler command, " +
					"(2) splitting the work into smaller steps, " +
					"(3) checking if the command is hanging on user input, or " +
					"(4) increasing the timeout if the operation legitimately requires more time."
			}

			return llm.ToolResult{
				ToolCallID: call.ID,
				Content:    content,
				IsError:    true,
			}
		}

		return llm.ToolResult{
			ToolCallID: call.ID,
			Content:    output,
		}
	}

	// Build the ConfirmContinue callback for the checkpoint mechanism.
	// This sends the iteration count to the TUI and blocks until the user
	// responds. When channels are nil (e.g. one-shot path), no prompting
	// occurs.
	var confirmFn func(int) bool
	if request.confirmContinueCh != nil {
		confirmFn = func(iterations int) bool {
			request.confirmContinueCh <- iterations
			return <-request.confirmResponseCh
		}
	}

	resp, _, err := llm.AgentLoop(ctx, reg, params, request.fallbackModels, executor, llm.AgentLoopConfig{
		ConfirmContinue: confirmFn,
	})

	// Close the request channel so the listenForCheckpoint goroutine exits.
	if request.confirmContinueCh != nil {
		close(request.confirmContinueCh)
	}

	if err != nil {
		return llmResponseMsg{err: err, completedAt: time.Now(), eventLines: eventLines.Lines(), toolLog: toolLog}
	}

	var usage tokenUsage
	usage.addResponse(resp)

	return llmResponseMsg{
		completedAt: time.Now(),
		content:     resp.Content,
		model:       resp.Model,
		eventLines:  eventLines.Lines(),
		toolLog:     toolLog,
		tokenUsage:  usage,
	}
}

// prependReferenceContext injects pre-rendered reference content as a system
// message at the beginning of the messages array. This makes configured
// repository paths, documentation links, and other reference material available
// to the LLM for every request.
func prependReferenceContext(params *llm.CompleteParams, refCtx string) {
	if refCtx == "" {
		return
	}

	params.Messages = append(
		[]llm.Message{{Role: llm.RoleSystem, Content: refCtx}},
		params.Messages...,
	)
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

type eventLineBuffer struct {
	lines []string
	mu    sync.Mutex
}

func newEventLineBuffer() *eventLineBuffer {
	return &eventLineBuffer{}
}

func (b *eventLineBuffer) Write(p []byte) (int, error) {
	if b == nil {
		return len(p), nil
	}

	text := strings.TrimRight(string(p), "\r\n")
	if text == "" {
		return len(p), nil
	}

	b.mu.Lock()
	defer b.mu.Unlock()

	for line := range strings.SplitSeq(text, "\n") {
		line = strings.TrimRight(line, "\r")
		if line == "" {
			continue
		}

		b.lines = append(b.lines, line)
	}

	return len(p), nil
}

func (b *eventLineBuffer) Lines() []string {
	if b == nil {
		return nil
	}

	b.mu.Lock()
	defer b.mu.Unlock()

	return append([]string(nil), b.lines...)
}

func eventLineCommands(lines []string) []tea.Cmd {
	cmds := make([]tea.Cmd, 0, len(lines))
	for _, line := range lines {
		if strings.TrimSpace(line) == "" {
			continue
		}

		cmds = append(cmds, tea.Println(dimStyle.Render(line)))
	}

	return cmds
}

func saveSession(ctx context.Context, store *session.Store, sessionState session.Session, runner *events.Runner) tea.Cmd {
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
	reasoningLevel string,
	reasoningSelected bool,
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

		if reasoningSelected {
			state.SetReasoningLevel(scope, cwd, reasoningLevel)
		}

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

func emitFromContextWarning(ctx context.Context, event events.Event) {
	if err := events.EmitFromContext(ctx, event); err != nil {
		slog.Warn("emit hook from context", "event_type", event.Type, "error", err)
	}
}

// ---------------------------------------------------------------------------
// Main
// ---------------------------------------------------------------------------

func runInteractive(ctx context.Context, state appState) error {
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

	// In TUI mode the runner's logger has to stay quiet — stderr writes would
	// bleed onto bubbletea's alt-screen rendering. Replace the stderr-logger
	// runner that loadAppState set up with a logger-less one. Utility commands
	// and one-shot mode keep the stderr logger.
	state.hookRunner = events.NewRunner(state.hookConfig)

	emitHookWarning(ctx, state.hookRunner, events.Event{
		Type:        events.SessionStart,
		SessionID:   state.sessionState.ID,
		SessionPath: state.sessionStore.Path(state.sessionState.ID),
		Agent:       state.selectedAgent,
		Model:       state.selectedModel,
	})

	finalModel, err := runInteractiveProgram(initialModel(
		ctx,
		state.registry,
		state.agentRegistry,
		state.hookRunner,
		state.sessionStore,
		state.stateStore,
		state.sessionState,
		state.contextOptions,
		state.referenceContext,
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

	// Once the program exits, restore the stderr logger so SessionEnd / Error
	// events are visible after the TUI has released the screen.
	state.hookRunner = events.NewRunnerWithLogger(state.hookConfig, os.Stderr)

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

	finalSession := state.sessionState

	if m, ok := finalModel.(model); ok {
		printTokenUsageSummary(os.Stderr, m.tokenUsage)
		finalSession = m.sessionState
	}

	if state.sessionStore != nil && finalSession.ID != "" {
		if err := state.sessionStore.Save(finalSession); err != nil {
			fmt.Fprintln(os.Stderr, "warning: could not save session on exit: "+err.Error())
		} else {
			emitFileWriteWarning(ctx, state.hookRunner, finalSession, state.sessionStore.Path(finalSession.ID), finalSession.DefaultAgent, "session")
		}

		printSessionReuseHint(os.Stderr, resolveSpawnBinary(""), state.sessionStore, finalSession.ID)
	}

	emitHookWarning(ctx, state.hookRunner, events.Event{
		Type:        events.SessionEnd,
		SessionID:   finalSession.ID,
		SessionPath: state.sessionStore.Path(finalSession.ID),
		Agent:       finalSession.DefaultAgent,
		Model:       finalSession.DefaultModel,
	})

	finalizeWorktree(ctx, &state)

	return nil
}

func printSessionReuseHint(w io.Writer, binary string, store *session.Store, sessionID string) {
	command := formatSessionReuseCommand(binary, store, sessionID)
	if command == "" || w == nil {
		return
	}

	fmt.Fprintln(w, "reuse session: "+command)
}

func formatSessionReuseCommand(binary string, store *session.Store, sessionID string) string {
	sessionID = strings.TrimSpace(sessionID)
	if sessionID == "" {
		return ""
	}

	binary = strings.TrimSpace(binary)
	if binary == "" {
		binary = "atteler"
	}

	args := []string{binary}
	if store != nil && strings.TrimSpace(store.Dir()) != "" {
		args = append(args, "--session-dir", store.Dir())
	}

	args = append(args, "--session-id", sessionID)

	quoted := make([]string, len(args))
	for i, arg := range args {
		if isSimpleShellWord(arg) {
			quoted[i] = arg
		} else {
			quoted[i] = shellQuote(arg)
		}
	}

	return strings.Join(quoted, " ")
}

func isSimpleShellWord(value string) bool {
	if value == "" {
		return false
	}

	for _, r := range value {
		if !isSimpleShellWordRune(r) {
			return false
		}
	}

	return true
}

func isSimpleShellWordRune(r rune) bool {
	return (r >= 'a' && r <= 'z') ||
		(r >= 'A' && r <= 'Z') ||
		(r >= '0' && r <= '9') ||
		strings.ContainsRune("@%_+=:,./-", r)
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
	referenceContext string,
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
		referenceContext,
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
	referenceContext string,
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

	applyRunOnceSessionDefaults(&sessionState, prepared, generationOverrides)
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

	refCtx := buildReferenceContext(ctx, referenceContext, prepared.activeAgent, contextOptions)
	prependReferenceContext(&params, refCtx)

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

	// Enable tool use for one-shot calls: the LLM can invoke bash commands.
	// Apply agent-level tool filtering when an agent is active.
	tools := llm.DefaultTools()
	if prepared.activeAgent.ok {
		tools = prepared.activeAgent.agent.FilterTools(tools)
	}

	params.Tools = tools

	if len(tools) > 0 {
		prependToolReminder(&params, tools)
	}

	slog.Debug("one-shot LLM request",
		"agent", prepared.activeAgent.name,
		"model", params.Model,
		"tools", len(params.Tools),
		"messages", len(params.Messages),
	)

	resp, err := runOnceComplete(ctx, reg, params, prepared.fallbackModels, executionOptions.Response)
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

		return fmt.Errorf("one-shot complete: %w", err)
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

func applyRunOnceSessionDefaults(sessionState *session.Session, prepared runOncePrepared, generationOverrides generationSettings) {
	if prepared.requestModel != "" {
		sessionState.DefaultModel = prepared.requestModel
	}

	if level := strings.TrimSpace(generationOverrides.ReasoningLevel); level != "" {
		sessionState.DefaultReasoningLevel = level
	}
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

// runOnceComplete handles replay or live agent-loop completion for one-shot
// mode. If a replay path is set, it loads the recorded response; otherwise it
// runs the agentic loop with tool execution support.
func runOnceComplete(
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

	cwd, err := os.Getwd()
	if err != nil {
		return nil, fmt.Errorf("get working directory: %w", err)
	}

	executor := newBashExecutor(cwd, os.Stderr)

	resp, _, err := llm.AgentLoop(ctx, reg, params, fallbackModels, executor, llm.AgentLoopConfig{
		ConfirmContinue: confirmContinueStdin,
	})
	if err != nil {
		return nil, fmt.Errorf("agent loop: %w", err)
	}

	if err := saveRecordedResponse(responseOptions.RecordPath, params, fallbackModels, resp); err != nil {
		return nil, err
	}

	return resp, nil
}

// confirmContinueStdin prompts the user on stdin/stderr when the agent loop
// reaches a checkpoint. Used in one-shot (non-TUI) mode.
func confirmContinueStdin(iterations int) bool {
	fmt.Fprintf(os.Stderr, "\nAgent loop reached %d iterations. Continue? [Y/n] ", iterations)

	var answer string
	if _, err := fmt.Scanln(&answer); err != nil {
		// On EOF or error, treat as "yes" to avoid blocking headless runs.
		return true
	}

	answer = strings.TrimSpace(strings.ToLower(answer))

	return answer == "" || answer == "y" || answer == "yes"
}

// newBashExecutor creates a ToolExecutor that runs bash commands in the given
// working directory, logging output to the provided writer.
func newBashExecutor(cwd string, logw io.Writer) llm.ToolExecutor {
	return func(ctx context.Context, call llm.ToolCall) llm.ToolResult {
		if call.Name != "bash" {
			return llm.ToolResult{
				ToolCallID: call.ID,
				Content:    "unknown tool: " + call.Name,
				IsError:    true,
			}
		}

		command, ok := call.Input["command"].(string)
		if !ok || command == "" {
			return llm.ToolResult{
				ToolCallID: call.ID,
				Content:    "error: empty command",
				IsError:    true,
			}
		}

		emitFromContextWarning(ctx, events.Event{
			Type:    events.CommandExecute,
			Content: command,
			Metadata: map[string]string{
				"command":      command,
				"cwd":          cwd,
				"input":        command,
				"source":       "llm_tool",
				"tool_call_id": call.ID,
			},
		})

		result, shellErr := attshell.RunBash(ctx, attshell.Options{
			Command: command,
			Dir:     cwd,
			Timeout: 5 * time.Minute,
		})

		output := formatShellContext(shellResultMsg{
			command: command,
			stdout:  result.Stdout,
			stderr:  result.Stderr,
			err:     shellErr,
		})
		fmt.Fprintln(logw, dimStyle.Render("  "+output))
		emitFromContextWarning(ctx, commandOutputEvent(
			"", "", "", "", cwd, command, output, shellErr,
			map[string]string{
				"source":       "llm_tool",
				"tool_call_id": call.ID,
			},
		))

		if shellErr != nil {
			return llm.ToolResult{ToolCallID: call.ID, Content: output, IsError: true}
		}

		return llm.ToolResult{ToolCallID: call.ID, Content: output}
	}
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

func initRTKPlugin(dir string) error {
	dir = strings.TrimSpace(dir)
	if dir == "" {
		return errors.New("init rtk plugin: directory is required")
	}

	files := map[string]rtkPluginFile{
		"plugin.yaml": {
			mode: 0o600,
			content: `name: rtk
version: "0.1.0"
description: RTK token-saving CLI proxy helpers for Atteler.
capabilities:
  - rtk
  - shell-output
  - token-optimization
entrypoints:
  version: bin/version
  gain: bin/gain
  show: bin/show
  init-codex: bin/init-codex
`,
		},
		"bin/version": {
			mode:    0o700,
			content: "#!/bin/sh\nexec rtk --version \"$@\"\n",
		},
		"bin/gain": {
			mode:    0o700,
			content: "#!/bin/sh\nexec rtk gain \"$@\"\n",
		},
		"bin/show": {
			mode:    0o700,
			content: "#!/bin/sh\nexec rtk init --show \"$@\"\n",
		},
		"bin/init-codex": {
			mode:    0o700,
			content: "#!/bin/sh\nexec rtk init -g --codex \"$@\"\n",
		},
	}

	for name, file := range files {
		path := filepath.Join(dir, name)
		if err := writeRTKPluginFile(path, file.content, file.mode); err != nil {
			return err
		}
	}

	fmt.Println("RTK plugin written to " + dir)
	fmt.Println("Add this to your atteler config:")
	fmt.Println("plugins:")
	fmt.Println("  paths: [" + strconv.Quote(dir) + "]")
	fmt.Println("Then run: atteler --run-plugin rtk/version")

	return nil
}

type rtkPluginFile struct {
	content string
	mode    os.FileMode
}

func writeRTKPluginFile(path, content string, mode os.FileMode) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
		return fmt.Errorf("init rtk plugin: create dir: %w", err)
	}

	if existing, err := os.ReadFile(path); err == nil {
		if string(existing) != content {
			return fmt.Errorf("init rtk plugin: refusing to overwrite modified file %s", path)
		}

		if chmodErr := os.Chmod(path, mode); chmodErr != nil {
			return fmt.Errorf("init rtk plugin: chmod %s: %w", path, chmodErr)
		}

		return nil
	} else if !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("init rtk plugin: read %s: %w", path, err)
	}

	if err := os.WriteFile(path, []byte(content), mode); err != nil {
		return fmt.Errorf("init rtk plugin: write %s: %w", path, err)
	}

	return nil
}

func runPluginEntrypoint(
	ctx context.Context,
	paths []string,
	target, entrypointName string,
	dryRun bool,
	timeoutSeconds int,
) error {
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
	// Default to 120s for the CLI --bash command (builds, tests, etc. can be
	// long-running). The shell package has its own 30s default for interactive
	// TUI commands which is intentionally shorter.
	const defaultBashCLITimeout = 120

	timeoutSeconds := opts.bashTimeout.value
	if timeoutSeconds == 0 {
		timeoutSeconds = defaultBashCLITimeout
	}

	timeout := time.Duration(timeoutSeconds) * time.Second

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
		Content:     opts.bashCommand,
		Metadata: map[string]string{
			"command": opts.bashCommand,
			"cwd":     dir,
			"input":   opts.bashCommand,
			"source":  "cli",
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

	output := formatShellContext(shellResultMsg{
		command: opts.bashCommand,
		stdout:  result.Stdout,
		stderr:  result.Stderr,
		err:     err,
	})
	emitHookWarning(ctx, state.hookRunner, commandOutputEvent(
		state.sessionState.ID,
		state.sessionStore.Path(state.sessionState.ID),
		state.selectedAgent,
		state.selectedModel,
		dir,
		opts.bashCommand,
		output,
		err,
		map[string]string{"source": "cli"},
	))

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

	if opts.spawnTimeout.value > 0 {
		var cancel context.CancelFunc

		ctx, cancel = context.WithTimeout(ctx, time.Duration(opts.spawnTimeout.value)*time.Second)
		defer cancel()
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
		Binary: resolveSpawnBinary(opts.spawnBinary),
		Dir:    state.cwd,
	}))
	fmt.Print(formatSpawnResults(results))

	if runErr != nil {
		return fmt.Errorf("spawn agents: %w", runErr)
	}

	return nil
}

func resolveSpawnBinary(explicit string) string {
	if binary := strings.TrimSpace(explicit); binary != "" {
		return binary
	}

	binary, err := os.Executable()
	if err != nil || strings.TrimSpace(binary) == "" {
		return os.Args[0]
	}

	return binary
}

func subagentCommandArgs(state appState) []string {
	var args []string
	if strings.TrimSpace(state.selectedModel) != "" {
		args = append(args, "--model", state.selectedModel)
	}

	if state.sessionStore != nil && strings.TrimSpace(state.sessionStore.Dir()) != "" {
		args = append(args, "--session-dir", state.sessionStore.Dir())
	}

	return args
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
			status = statusError
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
			fmt.Fprintf(&b, "%s=%s\n", statusError, result.Error)
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
		symbols, err = lsp.WorkspaceSymbols(ctx, lspOptions, opts.lspWorkspaceSymbols)
	} else {
		symbols, err = lsp.DocumentSymbols(ctx, lspOptions)
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
	plan, err := asyncPlanFromSpecs(specs)
	if err != nil {
		return fmt.Errorf("async plan: %w", err)
	}

	fmt.Print(formatAsyncPlanBatches(plan.ReadyBatches()))

	return nil
}

func runAsyncTasks(ctx context.Context, state appState, opts cliOptions) error {
	plan, err := asyncPlanFromSpecs(opts.asyncTaskSpecs)
	if err != nil {
		return fmt.Errorf("async run: %w", err)
	}

	tasks := plan.Tasks()
	if err := validateAsyncRunTasks(tasks); err != nil {
		return fmt.Errorf("async run: %w", err)
	}

	if opts.spawnTimeout.value > 0 {
		var cancel context.CancelFunc

		ctx, cancel = context.WithTimeout(ctx, time.Duration(opts.spawnTimeout.value)*time.Second)
		defer cancel()
	}

	emitHookWarning(ctx, state.hookRunner, events.Event{
		Type:        events.CommandExecute,
		SessionID:   state.sessionState.ID,
		SessionPath: state.sessionStore.Path(state.sessionState.ID),
		Agent:       state.selectedAgent,
		Model:       state.selectedModel,
		Metadata: map[string]string{
			"command": "async-run",
			"count":   strconv.Itoa(len(tasks)),
			"waves":   strconv.Itoa(len(plan.ReadyBatches())),
		},
	})

	runner := subagent.AttelerCommandWithOptions(subagent.CommandOptions{
		Args:   subagentCommandArgs(state),
		Binary: resolveSpawnBinary(opts.spawnBinary),
		Dir:    state.cwd,
	})
	results, runErr := plan.Run(ctx, func(ctx context.Context, task attasync.Task) (string, error) {
		return runner(ctx, subagent.Request{
			ID:     task.ID,
			Agent:  task.Agent,
			Prompt: task.Prompt,
		})
	})

	fmt.Print(formatAsyncRunResults(results))

	if runErr != nil {
		return fmt.Errorf("async run: %w", runErr)
	}

	return nil
}

func asyncPlanFromSpecs(specs []string) (*attasync.Plan, error) {
	if len(specs) == 0 {
		return nil, errors.New("at least one --async-task is required")
	}

	tasks := make([]attasync.Task, 0, len(specs))
	for _, spec := range specs {
		task, err := parseAsyncTaskSpec(spec)
		if err != nil {
			return nil, err
		}

		tasks = append(tasks, task)
	}

	plan, err := attasync.NewPlan(tasks)
	if err != nil {
		return nil, fmt.Errorf("new async plan: %w", err)
	}

	return plan, nil
}

func validateAsyncRunTasks(tasks []attasync.Task) error {
	for _, task := range tasks {
		if strings.TrimSpace(task.Agent) == "" {
			return fmt.Errorf("task %q agent is required for --async-run", task.ID)
		}

		if strings.TrimSpace(task.Prompt) == "" {
			return fmt.Errorf("task %q prompt is required for --async-run", task.ID)
		}
	}

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

func formatAsyncRunResults(results []attasync.TaskResult) string {
	if len(results) == 0 {
		return ""
	}

	var b strings.Builder

	for i := range results {
		result := results[i]

		status := "ok"
		if result.Error != "" {
			status = statusError
		}

		fmt.Fprintf(
			&b,
			"wave=%d\torder=%d\tid=%s\tagent=%s\tstatus=%s\tduration=%s\n",
			result.Wave+1,
			result.Order+1,
			result.Task.ID,
			result.Task.Agent,
			status,
			result.Duration.Round(time.Millisecond),
		)

		if strings.TrimSpace(result.Output) != "" {
			fmt.Fprintf(&b, "output=%s\n", strings.TrimSpace(result.Output))
		}

		if result.Error != "" {
			fmt.Fprintf(&b, "%s=%s\n", statusError, result.Error)
		}
	}

	return b.String()
}

func taskCommandRequested(opts cliOptions) bool {
	return opts.taskAddTitle != "" || opts.taskList || opts.taskAssignSpec != "" || opts.taskCompleteID != ""
}

func runTaskListCommand(ctx context.Context, sessionStore *session.Store, opts cliOptions) error {
	if err := validateSingleTaskOperation(opts); err != nil {
		return err
	}

	store := tasklist.NewStore(taskListPath(sessionStore, opts.taskFilePath))
	switch {
	case opts.taskAddTitle != "":
		task, err := store.Add(ctx, tasklist.AddRequest{
			ID:    opts.taskAddID,
			Title: opts.taskAddTitle,
			Agent: opts.taskAgent,
		})
		if err != nil {
			return fmt.Errorf("task add: %w", err)
		}

		fmt.Println(formatTaskListItem(task))

		return nil
	case opts.taskAssignSpec != "":
		id, agentName, err := parseTaskAssignmentSpec(opts.taskAssignSpec)
		if err != nil {
			return err
		}

		task, err := store.Assign(ctx, id, agentName)
		if err != nil {
			return fmt.Errorf("task assign: %w", err)
		}

		fmt.Println(formatTaskListItem(task))

		return nil
	case opts.taskCompleteID != "":
		task, err := store.Complete(ctx, opts.taskCompleteID, opts.taskAgent)
		if err != nil {
			return fmt.Errorf("task complete: %w", err)
		}

		fmt.Println(formatTaskListItem(task))

		return nil
	case opts.taskList:
		tasks, err := store.List(ctx)
		if err != nil {
			return fmt.Errorf("task list: %w", err)
		}

		if len(tasks) == 0 {
			fmt.Println("No tasks found.")
			return nil
		}

		for i := range tasks {
			fmt.Println(formatTaskListItem(tasks[i]))
		}

		return nil
	default:
		return errors.New("task list: no task operation requested")
	}
}

func validateSingleTaskOperation(opts cliOptions) error {
	operations := 0

	for _, requested := range []bool{
		opts.taskAddTitle != "",
		opts.taskList,
		opts.taskAssignSpec != "",
		opts.taskCompleteID != "",
	} {
		if requested {
			operations++
		}
	}

	if operations > 1 {
		return errors.New("task list: choose only one of --task-add, --task-list, --task-assign, or --task-complete")
	}

	return nil
}

func taskListPath(sessionStore *session.Store, explicit string) string {
	if path := strings.TrimSpace(explicit); path != "" {
		return path
	}

	if sessionStore == nil || strings.TrimSpace(sessionStore.Dir()) == "" {
		return filepath.Join(".atteler", "tasks.json")
	}

	return filepath.Join(filepath.Dir(sessionStore.Dir()), "tasks.json")
}

func parseTaskAssignmentSpec(raw string) (id, agentName string, err error) {
	id, agentName, ok := strings.Cut(raw, ":")
	if !ok {
		return "", "", fmt.Errorf("task assign %q: expected id:agent", raw)
	}

	id = strings.TrimSpace(id)
	agentName = strings.TrimSpace(agentName)

	if id == "" {
		return "", "", fmt.Errorf("task assign %q: id is required", raw)
	}

	if agentName == "" {
		return "", "", fmt.Errorf("task assign %q: agent is required", raw)
	}

	return id, agentName, nil
}

func formatTaskListItem(task tasklist.Task) string {
	parts := []string{
		"id=" + task.ID,
		"status=" + string(task.Status),
		"title=" + task.Title,
	}

	if task.Agent != "" {
		parts = append(parts, "agent="+task.Agent)
	}

	if !task.CreatedAt.IsZero() {
		parts = append(parts, "created_at="+task.CreatedAt.UTC().Format(time.RFC3339))
	}

	if !task.UpdatedAt.IsZero() {
		parts = append(parts, "updated_at="+task.UpdatedAt.UTC().Format(time.RFC3339))
	}

	if task.CompletedAt != nil && !task.CompletedAt.IsZero() {
		parts = append(parts, "completed_at="+task.CompletedAt.UTC().Format(time.RFC3339))
	}

	if metadata := formatTaskMetadata(task.Metadata); metadata != "" {
		parts = append(parts, "metadata="+metadata)
	}

	return strings.Join(parts, "	")
}

func formatTaskMetadata(metadata map[string]string) string {
	if len(metadata) == 0 {
		return ""
	}

	keys := make([]string, 0, len(metadata))
	for key := range metadata {
		keys = append(keys, key)
	}

	sort.Strings(keys)

	parts := make([]string, 0, len(keys))
	for _, key := range keys {
		parts = append(parts, key+":"+metadata[key])
	}

	return strings.Join(parts, ",")
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

// registryCompleter adapts the llm.Registry to the speculate.LLMCompleter
// interface so the speculative execution pipeline can make real LLM calls.
type registryCompleter struct {
	registry       *llm.Registry
	fallbackModels []string
	generation     generationSettings
}

func (rc *registryCompleter) Complete(ctx context.Context, model, systemPrompt, userPrompt string) (string, error) {
	params := llm.CompleteParams{
		Model: model,
		Messages: []llm.Message{
			{Role: llm.RoleSystem, Content: systemPrompt},
			{Role: llm.RoleUser, Content: userPrompt},
		},
	}

	applyGenerationParams(&params, rc.generation)

	resp, err := rc.registry.CompleteWithFallback(ctx, params, rc.fallbackModels)
	if err != nil {
		return "", fmt.Errorf("speculate LLM complete: %w", err)
	}

	return resp.Content, nil
}

func runSpeculateExecution(ctx context.Context, state appState, opts cliOptions) error {
	prompt := strings.TrimSpace(opts.speculatePrompt)
	if prompt == "" {
		return errors.New("speculate-run requires --speculate-prompt")
	}

	agents := []string(opts.speculateAgents)
	if len(agents) == 0 {
		return errors.New("speculate-run requires at least one --speculate-agent")
	}

	gates := []string(opts.speculateGates)
	if len(gates) == 0 {
		gates = []string{"tests pass", "lint pass", "types pass"}
	}

	plan, err := speculate.NewPlan(agents, gates)
	if err != nil {
		return fmt.Errorf("speculate-run: %w", err)
	}

	completer := &registryCompleter{
		registry:       state.registry,
		fallbackModels: state.fallbackModels,
		generation:     mergeGenerationSettings(state.generationDefaults, state.generationOverrides),
	}

	fmt.Fprintln(os.Stderr, "speculate: running three-round pipeline with "+strings.Join(agents, ", ")+"...")

	result, err := speculate.RunWithLLM(ctx, plan, completer, prompt)
	if err != nil {
		// Print partial results even on error.
		if len(result.Session.Proposals) > 0 {
			fmt.Println(formatSpeculateResult(result))
		}

		return fmt.Errorf("speculate-run: %w", err)
	}

	fmt.Print(formatSpeculateResult(result))

	return nil
}

func formatSpeculateResult(result speculate.Result) string {
	var b strings.Builder

	b.WriteString("winner: " + result.Winner + "\n")
	b.WriteString("reason: " + result.Reason + "\n")

	if len(result.Session.Proposals) > 0 {
		b.WriteString("proposals:\n")

		for _, p := range result.Session.Proposals {
			fmt.Fprintf(&b, "  - agent: %s\n    content: %s\n", p.Agent, truncatePreview(p.Content, 200))
		}
	}

	if len(result.Session.Reviews) > 0 {
		b.WriteString("reviews:\n")

		for _, r := range result.Session.Reviews {
			fmt.Fprintf(&b, "  - reviewer: %s -> %s\n    notes: %s\n", r.Reviewer, r.TargetAgent, truncatePreview(r.Notes, 200))
		}
	}

	if len(result.Session.Verdict.GateChecks) > 0 {
		b.WriteString("gates:\n")

		for _, gc := range result.Session.Verdict.GateChecks {
			status := "FAIL"
			if gc.Passed {
				status = "PASS"
			}

			fmt.Fprintf(&b, "  - %s: %s %s\n", gc.Name, status, gc.Notes)
		}
	}

	return b.String()
}

func truncatePreview(s string, maxLen int) string {
	s = strings.TrimSpace(s)
	s = strings.ReplaceAll(s, "\n", " ")

	if len(s) <= maxLen {
		return s
	}

	return s[:maxLen] + "..."
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
		fmt.Fprintf(
			&b, "    - %s\tprompt_bytes=%d\tshared_prefix_bytes=%d\treuse_ratio=%.4f\n",
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
		ctx,
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

//nolint:unparam // error return kept for consistency with other command handlers.
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

func llmConfig(cfg appconfig.Config, selectedModel string) llm.AutoRegisterConfig {
	providers := make(map[string]llm.ProviderConfig, len(cfg.Providers))
	for name, provider := range cfg.Providers {
		providers[name] = llm.ProviderConfig{
			Disabled:       provider.Disabled,
			BaseURL:        provider.BaseURL,
			TimeoutSeconds: provider.TimeoutSeconds,
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

// loadConfiguredReferences resolves the configured reference paths/URLs at
// startup and returns a pre-rendered reference block that can be injected into
// every LLM request as additional context. Errors are logged but not fatal so
// the session can still start with whatever references succeeded.
func loadConfiguredReferences(ctx context.Context, refs []string, opts contextref.Options) string {
	if len(refs) == 0 {
		return ""
	}

	loaded, err := contextref.LoadReferences(ctx, refs, opts)
	if err != nil {
		fmt.Fprintf(os.Stderr, "warning: loading configured references: %v\n", err)
	}

	return contextref.FormatReferences(loaded)
}

// buildReferenceContext combines the pre-loaded global reference context with
// any agent-specific references. If the agent has its own references they are
// loaded on the fly and appended after the global block.
func buildReferenceContext(ctx context.Context, globalRefCtx string, activeAgent agentSelection, opts contextref.Options) string {
	if !activeAgent.ok || len(activeAgent.agent.References) == 0 {
		return globalRefCtx
	}

	agentRefCtx := loadConfiguredReferences(ctx, activeAgent.agent.References, opts)
	if agentRefCtx == "" {
		return globalRefCtx
	}

	if globalRefCtx == "" {
		return agentRefCtx
	}

	return globalRefCtx + "\n\n" + agentRefCtx
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
