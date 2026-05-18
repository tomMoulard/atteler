package llm

import (
	"context"
	"fmt"
)

// ToolExecutor executes a single tool call and returns the result content.
// Implementations should handle the tool by name and return the textual
// output. If the tool fails, return is_error=true with the error message
// as content.
type ToolExecutor func(ctx context.Context, call ToolCall) ToolResult

// AgentLoopConfig configures the multi-turn agentic completion loop.
type AgentLoopConfig struct {
	OnToolCall    func(call ToolCall)
	OnToolResult  func(call ToolCall, result ToolResult)
	OnContent     func(content string)
	MaxIterations int
}

const defaultMaxIterations = 20

// AgentLoop runs a multi-turn completion loop where the LLM can request
// tool executions. It keeps calling Complete until the model stops asking
// for tools (StopReason != StopToolUse) or the iteration limit is reached.
//
// The loop:
//  1. Calls Complete with the provided params (which should include Tools).
//  2. If the response has ToolCalls, executes each via executor.
//  3. Appends the assistant message (with tool calls) and tool results to
//     the message history.
//  4. Calls Complete again with the updated history.
//  5. Repeats until the model produces a final text response.
//
// The returned Response is the final one (with text content). The updated
// messages slice (including all tool-use turns) is also returned so callers
// can persist the full conversation.
func AgentLoop(
	ctx context.Context,
	reg *Registry,
	params CompleteParams,
	fallbackModels []string,
	executor ToolExecutor,
	cfg AgentLoopConfig,
) (*Response, []Message, error) {
	maxIter := cfg.MaxIterations
	if maxIter <= 0 {
		maxIter = defaultMaxIterations
	}

	messages := append([]Message(nil), params.Messages...)

	var totalUsage tokenUsage

	for i := range maxIter {
		if err := ctx.Err(); err != nil {
			return nil, messages, fmt.Errorf("llm: agent loop canceled: %w", err)
		}

		iterParams := params
		iterParams.Messages = messages

		resp, err := reg.CompleteWithFallback(ctx, iterParams, fallbackModels)
		if err != nil {
			return nil, messages, fmt.Errorf("llm: agent loop iteration %d: %w", i, err)
		}

		totalUsage.addResponse(resp)

		if !resp.WantsToolUse() {
			// Final response -- model is done.
			resp.InputTokens = totalUsage.input
			resp.CachedInputTokens = totalUsage.cached
			resp.OutputTokens = totalUsage.output

			return resp, messages, nil
		}

		// The model wants to call tools.
		// Append the assistant message with tool calls to history.
		assistantMsg := Message{
			Role:      RoleAssistant,
			Content:   resp.Content,
			ToolCalls: resp.ToolCalls,
		}
		messages = append(messages, assistantMsg)

		// Execute each tool call and append results.
		for _, call := range resp.ToolCalls {
			if cfg.OnToolCall != nil {
				cfg.OnToolCall(call)
			}

			result := executor(ctx, call)

			if cfg.OnToolResult != nil {
				cfg.OnToolResult(call, result)
			}

			messages = append(messages, Message{
				Role:       RoleTool,
				Content:    result.Content,
				ToolResult: &result,
			})
		}

		// Emit intermediate content if any.
		if resp.Content != "" && cfg.OnContent != nil {
			cfg.OnContent(resp.Content)
		}
	}

	return nil, messages, fmt.Errorf("llm: agent loop exceeded %d iterations", maxIter)
}

// tokenUsage accumulates token counts across multiple LLM calls.
type tokenUsage struct {
	input  int
	cached int
	output int
}

func (u *tokenUsage) addResponse(resp *Response) {
	u.input += resp.InputTokens
	u.cached += resp.CachedInputTokens
	u.output += resp.OutputTokens
}
