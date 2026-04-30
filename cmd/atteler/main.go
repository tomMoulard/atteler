// Package main is the entry point for the atteler TUI application.
package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"sort"
	"strings"

	"github.com/charmbracelet/bubbles/key"
	"github.com/charmbracelet/bubbles/textarea"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"

	"github.com/tommoulard/atteler/pkg/llm"
)

// ---------------------------------------------------------------------------
// Styles
// ---------------------------------------------------------------------------

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
const keyCtrlC = "ctrl+c"

// ---------------------------------------------------------------------------
// Messages (tea.Msg)
// ---------------------------------------------------------------------------

// llmResponseMsg is sent when the LLM call completes.
type llmResponseMsg struct {
	err     error
	content string
	model   string
}

// modelsLoadedMsg is sent when model discovery from the API completes.
type modelsLoadedMsg struct {
	err   error
	items []pickerItem
}

// pickerItem represents one selectable entry in the model picker.
type pickerItem struct {
	provider string
	model    string
}

// ---------------------------------------------------------------------------
// Model
// ---------------------------------------------------------------------------

type model struct {
	textarea         textarea.Model
	registry         *llm.Registry
	cancel           context.CancelFunc
	selectedModel    string
	selectedProvider string
	pickerItems      []pickerItem
	history          []llm.Message
	pickerCursor     int
	width            int
	quitting         bool
	waiting          bool
	pickerOpen       bool
	pickerLoading    bool
}

func initialModel(reg *llm.Registry) model {
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
	return model{
		registry: reg,
		textarea: ta,
	}
}

// Init returns the initial command.
func (m model) Init() tea.Cmd {
	return textarea.Blink
}

// Update handles incoming messages.
func (m model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	var cmds []tea.Cmd

	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.width = msg.Width
		m.textarea.SetWidth(msg.Width)

	case modelsLoadedMsg:
		m.pickerLoading = false
		if msg.err != nil {
			cmds = append(cmds, tea.Println(errStyle.Render("Error loading models: "+msg.err.Error())))
			m.pickerOpen = false
			return m, tea.Batch(cmds...)
		}
		m.pickerItems = msg.items
		m.pickerCursor = 0
		return m, nil

	case tea.KeyMsg:
		if m.pickerOpen {
			return m.updatePicker(msg)
		}
		return m.updateChat(msg)

	case llmResponseMsg:
		return m.updateLLMResponse(msg)
	}

	// Propagate to the textarea when not waiting and picker is closed.
	if !m.waiting && !m.pickerOpen {
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

	case "enter":
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

	m.history = append(m.history, llm.Message{
		Role:    llm.RoleUser,
		Content: input,
	})

	// Print the user message above the input area.
	line := userLabel.Render("You") + " " + input

	// Launch the LLM call.
	m.waiting = true
	ctx, cancel := context.WithCancel(context.Background()) //nolint:gosec // cancel is stored in m.cancel and invoked on ctrl+c
	m.cancel = cancel
	msgs := make([]llm.Message, len(m.history))
	copy(msgs, m.history)

	return m, tea.Sequence(
		tea.Println(line),
		callLLM(ctx, m.registry, m.selectedModel, msgs),
	)
}

// updateLLMResponse handles the message received when an LLM call completes.
func (m model) updateLLMResponse(msg llmResponseMsg) (tea.Model, tea.Cmd) {
	m.waiting = false
	m.cancel = nil
	if msg.err != nil {
		return m, tea.Println(errStyle.Render("Error: " + msg.err.Error()))
	}

	m.history = append(m.history, llm.Message{
		Role:    llm.RoleAssistant,
		Content: msg.content,
	})
	header := assistantLabel.Render("Assistant") + " " +
		dimStyle.Render("("+msg.model+")")
	return m, tea.Println(header + "\n" + msg.content)
}

// updatePicker handles key events while the model picker is open.
func (m model) updatePicker(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	if m.pickerLoading {
		// Only allow escape while loading.
		if msg.String() == "esc" || msg.String() == keyCtrlC {
			m.pickerOpen = false
			m.pickerLoading = false
		}
		return m, nil
	}

	switch msg.String() {
	case "esc", keyCtrlC, "ctrl+o":
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

	case "enter":
		if len(m.pickerItems) > 0 {
			item := m.pickerItems[m.pickerCursor]
			m.selectedProvider = item.provider
			m.selectedModel = item.model
			m.pickerOpen = false
			return m, tea.Println(
				dimStyle.Render("Model set to ") +
					pickerProviderStyle.Render(item.provider) +
					dimStyle.Render("/") +
					pickerSelectedStyle.Render(item.model),
			)
		}
	}

	return m, nil
}

// View renders only the current input area (past messages are already printed).
func (m model) View() string {
	if m.quitting {
		return ""
	}

	if m.pickerOpen {
		return m.viewPicker()
	}

	if m.waiting {
		return statusStyle.Render("  Thinking... (Ctrl+C to cancel)")
	}

	var status string
	if m.selectedModel != "" {
		status = dimStyle.Render("  [") +
			pickerProviderStyle.Render(m.selectedProvider) +
			dimStyle.Render("/") +
			pickerSelectedStyle.Render(m.selectedModel) +
			dimStyle.Render("]")
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

// callLLM sends the messages to the selected LLM and returns a command that
// resolves with an llmResponseMsg. If no model is selected it uses the
// registry default.
func callLLM(ctx context.Context, reg *llm.Registry, model string, msgs []llm.Message) tea.Cmd {
	return func() tea.Msg {
		resp, err := reg.Complete(ctx, llm.CompleteParams{
			Model:    model,
			Messages: msgs,
		})
		if err != nil {
			return llmResponseMsg{err: err}
		}
		return llmResponseMsg{
			content: resp.Content,
			model:   resp.Model,
		}
	}
}

// ---------------------------------------------------------------------------
// Main
// ---------------------------------------------------------------------------

func main() {
	reg := llm.AutoRegister()

	providers := reg.ListProviders()
	if len(providers) == 0 {
		fmt.Fprintln(os.Stderr, "warning: no LLM providers configured, set ANTHROPIC_API_KEY or OPENAI_API_KEY")
	}

	fmt.Println(promptStyle.Render("atteler") + dimStyle.Render("  Ctrl+D to quit, Ctrl+O to pick model"))
	if len(providers) > 0 {
		sort.Strings(providers)
		fmt.Println(dimStyle.Render("  Connected providers: ") + pickerProviderStyle.Render(strings.Join(providers, ", ")))
	}

	p := tea.NewProgram(initialModel(reg))
	if _, err := p.Run(); err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		os.Exit(1)
	}
}
