package main

import (
	"fmt"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/tommoulard/atteler/pkg/agent"
	"github.com/tommoulard/atteler/pkg/events"
	"github.com/tommoulard/atteler/pkg/llm"
)

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
	if event, ok := routeDecisionEvent(m.sessionState.ID, m.sessionPath, m.sessionState.DefaultAgent, routeResponseModelID(msg.provider, msg.model), msg.routeDecision); ok {
		cmds = append(cmds, emitHook(m.ctx, m.hookRunner, event))
	}

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
