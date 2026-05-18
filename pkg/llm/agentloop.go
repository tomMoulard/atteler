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
	OnToolCall   func(call ToolCall)
	OnToolResult func(call ToolCall, result ToolResult)
	OnContent    func(content string)

	// ConfirmContinue is called when the loop reaches a checkpoint
	// (every CheckpointInterval iterations). It receives the current
	// iteration count and should return true to continue or false to
	// stop. If nil, the loop always continues.
	ConfirmContinue func(iterations int) bool

	MaxIterations int

	// CheckpointInterval is the number of iterations between continuation
	// prompts. When the loop reaches a multiple of this value,
	// ConfirmContinue is called. If zero, defaults to
	// defaultCheckpointInterval (20).
	CheckpointInterval int
}

const defaultCheckpointInterval = 20

const defaultMaxIterations = 2000

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

	checkpoint := cfg.CheckpointInterval
	if checkpoint <= 0 {
		checkpoint = defaultCheckpointInterval
	}

	messages := append([]Message(nil), params.Messages...)

	var totalUsage tokenUsage

	for i := range maxIter {
		if err := ctx.Err(); err != nil {
			return nil, messages, fmt.Errorf("llm: agent loop canceled: %w", err)
		}

		if err := checkpointGate(i, checkpoint, cfg.ConfirmContinue); err != nil {
			return nil, messages, err
		}

		iterParams := params
		iterParams.Messages = messages

		resp, err := reg.CompleteWithFallback(ctx, iterParams, fallbackModels)
		if err != nil {
			return nil, messages, fmt.Errorf("llm: agent loop iteration %d: %w", i, err)
		}

		totalUsage.addResponse(resp)

		if !resp.WantsToolUse() {
			resp.InputTokens = totalUsage.input
			resp.CachedInputTokens = totalUsage.cached
			resp.OutputTokens = totalUsage.output

			return resp, messages, nil
		}

		messages = executeToolCalls(ctx, resp, messages, executor, cfg)
	}

	return nil, messages, fmt.Errorf("llm: agent loop exceeded %d iterations", maxIter)
}

// checkpointGate asks the caller whether to continue when a checkpoint
// interval is reached. Returns nil to continue, or an error to stop.
func checkpointGate(iteration, interval int, confirm func(int) bool) error {
	if iteration == 0 || iteration%interval != 0 || confirm == nil {
		return nil
	}

	if !confirm(iteration) {
		return fmt.Errorf("llm: agent loop stopped by user after %d iterations", iteration)
	}

	return nil
}

// executeToolCalls runs each requested tool call, invoking optional callbacks,
// and appends the assistant + tool-result messages to the history.
func executeToolCalls(
	ctx context.Context,
	resp *Response,
	messages []Message,
	executor ToolExecutor,
	cfg AgentLoopConfig,
) []Message {
	// Append the assistant message with tool calls to history.
	messages = append(messages, Message{
		Role:      RoleAssistant,
		Content:   resp.Content,
		ToolCalls: resp.ToolCalls,
	})

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

	return messages
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
