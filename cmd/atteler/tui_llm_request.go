package main

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/tommoulard/atteler/pkg/contextref"
	"github.com/tommoulard/atteler/pkg/events"
	"github.com/tommoulard/atteler/pkg/llm"
	attshell "github.com/tommoulard/atteler/pkg/shell"
)

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
			completedAt:   time.Now(),
			content:       resp.Content,
			provider:      resp.Provider,
			model:         resp.Model,
			eventLines:    eventLines.Lines(),
			routeDecision: routeDecisionWithResponse(request.routeDecision, resp, routeTelemetryFromRegistry(reg)),
			tokenUsage:    usage,
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
			Command:        command,
			Dir:            request.workingDir,
			Timeout:        5 * time.Minute, // Generous timeout for tool calls.
			MaxOutputBytes: agentLoopToolOutputLimit(ctx),
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

	confirmContinueFn, confirmToolFn := agentLoopConfirmCallbacks(ctx, request)

	resp, _, err := llm.AgentLoop(ctx, reg, params, request.fallbackModels, executor, llm.AgentLoopConfig{
		ConfirmContinue:    confirmContinueFn,
		ConfirmToolCall:    confirmToolFn,
		Budget:             request.agentLoopBudget,
		CheckpointInterval: request.agentLoopCheckpointInterval,
		Policy:             llm.BashToolPolicy,
		CheckpointSink:     agentLoopCheckpointSink(request.agentLoopCheckpointPath),
	})

	// Close the request channel so the listenForCheckpoint goroutine exits.
	if request.confirmRequestCh != nil {
		close(request.confirmRequestCh)
	}

	if err != nil {
		return llmResponseMsg{
			err:         agentLoopError(err, request.agentLoopCheckpointPath),
			completedAt: time.Now(),
			eventLines:  eventLines.Lines(),
			toolLog:     toolLog,
		}
	}

	var usage tokenUsage
	usage.addResponse(resp)

	return llmResponseMsg{
		completedAt:   time.Now(),
		content:       resp.Content,
		provider:      resp.Provider,
		model:         resp.Model,
		eventLines:    eventLines.Lines(),
		routeDecision: routeDecisionWithResponse(request.routeDecision, resp, routeTelemetryFromRegistry(reg)),
		toolLog:       toolLog,
		tokenUsage:    usage,
	}
}

// agentLoopConfirmCallbacks builds TUI-backed callbacks for both legacy loop
// continuation checkpoints and require-confirm tool policy decisions.
func agentLoopConfirmCallbacks(ctx context.Context, request llmRequest) (func(int) bool, llm.ConfirmToolCallFunc) {
	if request.confirmRequestCh == nil || request.confirmResponseCh == nil {
		return nil, nil
	}

	confirmContinue := func(iterations int) bool {
		return sendAgentLoopConfirmation(ctx, request.confirmRequestCh, request.confirmResponseCh, agentLoopConfirmRequest{
			kind:       agentLoopConfirmCheckpoint,
			iterations: iterations,
			prompt:     fmt.Sprintf("Agent loop reached %d iterations. Continue? [Y/n] ", iterations),
		})
	}

	confirmTool := func(ctx context.Context, call llm.ToolCall, decision llm.ToolPolicyDecision) bool {
		return sendAgentLoopConfirmation(ctx, request.confirmRequestCh, request.confirmResponseCh, agentLoopConfirmRequest{
			kind:   agentLoopConfirmToolCall,
			prompt: agentLoopToolConfirmPrompt(call, decision),
		})
	}

	return confirmContinue, confirmTool
}

func sendAgentLoopConfirmation(
	ctx context.Context,
	requestCh chan<- agentLoopConfirmRequest,
	responseCh <-chan bool,
	request agentLoopConfirmRequest,
) bool {
	select {
	case requestCh <- request:
	case <-ctx.Done():
		return false
	}

	select {
	case answer := <-responseCh:
		return answer
	case <-ctx.Done():
		return false
	}
}

func agentLoopToolConfirmPrompt(call llm.ToolCall, decision llm.ToolPolicyDecision) string {
	command, ok := call.Input["command"].(string)
	if !ok {
		command = "<missing command>"
	}

	return fmt.Sprintf(
		"Agent tool call requires confirmation (%s): %s\n$ %s\nExecute? [y/N] ",
		decision.MatchedRule,
		decision.Reason,
		command,
	)
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
	referenceContext string,
) []llm.Message {
	params := llm.CompleteParams{
		Model:    modelName,
		Messages: messages,
	}
	if activeAgent.ok {
		params = activeAgent.agent.CompleteParams(modelName, messages)
	}

	prependReferenceContext(&params, referenceContext)

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
