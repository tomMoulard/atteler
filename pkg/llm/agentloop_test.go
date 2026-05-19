package llm

import (
	"context"
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// agentTestProvider is a fake provider for testing the agent loop.
type agentTestProvider struct {
	responses []*Response
	calls     int
}

func (p *agentTestProvider) Complete(_ context.Context, _ CompleteParams) (*Response, error) {
	if p.calls >= len(p.responses) {
		return nil, errors.New("no more responses")
	}

	resp := p.responses[p.calls]
	p.calls++

	return resp, nil
}

func (p *agentTestProvider) Models() []string                    { return []string{"test-model"} }
func (p *agentTestProvider) HealthCheck(_ context.Context) error { return nil }
func (p *agentTestProvider) FetchModels(_ context.Context) ([]string, error) {
	return []string{"test-model"}, nil
}
func (p *agentTestProvider) ModelContextWindow(_ string) int { return 128_000 }
func (p *agentTestProvider) Name() string                    { return "test" }

func TestAgentLoop_NoToolCalls(t *testing.T) {
	t.Parallel()

	reg := NewRegistry()
	reg.Register(&agentTestProvider{
		responses: []*Response{
			{Content: "hello world", Model: "test-model", StopReason: StopEndTurn},
		},
	})

	params := CompleteParams{
		Model:    "test-model",
		Messages: []Message{{Role: RoleUser, Content: "hi"}},
		Tools:    DefaultTools(),
	}

	executor := func(_ context.Context, call ToolCall) ToolResult {
		t.Fatalf("executor should not be called, got tool call: %s", call.Name)

		return ToolResult{}
	}

	resp, history, err := AgentLoop(context.Background(), reg, params, nil, executor, AgentLoopConfig{
		MaxIterations: 5,
	})
	require.NoError(t, err)

	assert.Equal(t, "hello world", resp.Content)
	// history includes the original user message only (no tool turns).
	assert.Len(t, history, 1)
}

func TestAgentLoop_ToolCallThenFinalResponse(t *testing.T) {
	t.Parallel()

	reg := NewRegistry()
	reg.Register(&agentTestProvider{
		responses: []*Response{
			{
				Content:    "",
				Model:      "test-model",
				StopReason: StopToolUse,
				ToolCalls: []ToolCall{
					{ID: "call_1", Name: "bash", Input: map[string]any{"command": "echo ok"}},
				},
			},
			{Content: "done", Model: "test-model", StopReason: StopEndTurn},
		},
	})

	params := CompleteParams{
		Model:    "test-model",
		Messages: []Message{{Role: RoleUser, Content: "run echo ok"}},
		Tools:    DefaultTools(),
	}

	var executedCommands []string

	executor := func(_ context.Context, call ToolCall) ToolResult {
		cmd, ok := call.Input["command"].(string)
		require.True(t, ok, "command input must be a string")

		executedCommands = append(executedCommands, cmd)

		return ToolResult{
			ToolCallID: call.ID,
			Content:    "ok\n",
		}
	}

	resp, history, err := AgentLoop(context.Background(), reg, params, nil, executor, AgentLoopConfig{
		MaxIterations: 5,
	})
	require.NoError(t, err)

	assert.Equal(t, "done", resp.Content)
	assert.Equal(t, []string{"echo ok"}, executedCommands)
	// history = original user msg + assistant tool-call msg + tool result msg = 3.
	assert.Len(t, history, 3)
}

func TestAgentLoop_MaxIterationsExceeded(t *testing.T) {
	t.Parallel()

	// Provider always requests tool calls (never terminates).
	infiniteProvider := &agentTestProvider{
		responses: make([]*Response, 10),
	}
	for i := range infiniteProvider.responses {
		infiniteProvider.responses[i] = &Response{
			Model:      "test-model",
			StopReason: StopToolUse,
			ToolCalls:  []ToolCall{{ID: "call", Name: "bash", Input: map[string]any{"command": "loop"}}},
		}
	}

	reg := NewRegistry()
	reg.Register(infiniteProvider)

	params := CompleteParams{
		Model:    "test-model",
		Messages: []Message{{Role: RoleUser, Content: "loop forever"}},
		Tools:    DefaultTools(),
	}

	executor := func(_ context.Context, call ToolCall) ToolResult {
		return ToolResult{ToolCallID: call.ID, Content: "ok"}
	}

	_, _, err := AgentLoop(context.Background(), reg, params, nil, executor, AgentLoopConfig{
		MaxIterations: 3,
	})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "exceeded")
}

func TestAgentLoop_DefaultCheckpointIntervalMatchesMaxIterations(t *testing.T) {
	t.Parallel()

	// With the default config, the continuation checkpoint should line up with
	// the loop's hard limit. A value of 20 here would reintroduce the noisy
	// prompt long before the 2000-iteration safety cap.
	assert.Equal(t, defaultMaxIterations, defaultCheckpointInterval)
}

func TestAgentLoop_ZeroCheckpointIntervalDefaultsToMaxIterations(t *testing.T) {
	t.Parallel()

	responses := make([]*Response, 21)
	for i := range 20 {
		responses[i] = &Response{
			Model:      "test-model",
			StopReason: StopToolUse,
			ToolCalls:  []ToolCall{{ID: "call", Name: "bash", Input: map[string]any{"command": "step"}}},
		}
	}

	responses[20] = &Response{Content: "done", Model: "test-model", StopReason: StopEndTurn}

	reg := NewRegistry()
	reg.Register(&agentTestProvider{responses: responses})

	params := CompleteParams{
		Model:    "test-model",
		Messages: []Message{{Role: RoleUser, Content: "work"}},
		Tools:    DefaultTools(),
	}

	executor := func(_ context.Context, call ToolCall) ToolResult {
		return ToolResult{ToolCallID: call.ID, Content: "ok"}
	}

	var checkpoints []int

	resp, _, err := AgentLoop(context.Background(), reg, params, nil, executor, AgentLoopConfig{
		MaxIterations: 100,
		ConfirmContinue: func(iterations int) bool {
			checkpoints = append(checkpoints, iterations)
			return true
		},
	})
	require.NoError(t, err)

	assert.Equal(t, "done", resp.Content)
	assert.Empty(t, checkpoints)
}

func TestAgentLoop_CheckpointContinue(t *testing.T) {
	t.Parallel()

	// 5 tool-call iterations then a final text response.
	responses := make([]*Response, 6)
	for i := range 5 {
		responses[i] = &Response{
			Model:      "test-model",
			StopReason: StopToolUse,
			ToolCalls:  []ToolCall{{ID: "call", Name: "bash", Input: map[string]any{"command": "step"}}},
		}
	}

	responses[5] = &Response{Content: "done", Model: "test-model", StopReason: StopEndTurn}

	reg := NewRegistry()
	reg.Register(&agentTestProvider{responses: responses})

	params := CompleteParams{
		Model:    "test-model",
		Messages: []Message{{Role: RoleUser, Content: "work"}},
		Tools:    DefaultTools(),
	}

	executor := func(_ context.Context, call ToolCall) ToolResult {
		return ToolResult{ToolCallID: call.ID, Content: "ok"}
	}

	var checkpoints []int

	resp, _, err := AgentLoop(context.Background(), reg, params, nil, executor, AgentLoopConfig{
		MaxIterations:      10,
		CheckpointInterval: 3,
		ConfirmContinue: func(iterations int) bool {
			checkpoints = append(checkpoints, iterations)
			return true // always continue
		},
	})
	require.NoError(t, err)

	assert.Equal(t, "done", resp.Content)
	// Checkpoint fires at iteration 3 (0-indexed, so 3 is the 4th iteration).
	assert.Equal(t, []int{3}, checkpoints)
}

func TestAgentLoop_CheckpointStop(t *testing.T) {
	t.Parallel()

	// Provider always requests tool calls.
	responses := make([]*Response, 20)
	for i := range responses {
		responses[i] = &Response{
			Model:      "test-model",
			StopReason: StopToolUse,
			ToolCalls:  []ToolCall{{ID: "call", Name: "bash", Input: map[string]any{"command": "loop"}}},
		}
	}

	reg := NewRegistry()
	reg.Register(&agentTestProvider{responses: responses})

	params := CompleteParams{
		Model:    "test-model",
		Messages: []Message{{Role: RoleUser, Content: "loop"}},
		Tools:    DefaultTools(),
	}

	executor := func(_ context.Context, call ToolCall) ToolResult {
		return ToolResult{ToolCallID: call.ID, Content: "ok"}
	}

	_, _, err := AgentLoop(context.Background(), reg, params, nil, executor, AgentLoopConfig{
		MaxIterations:      100,
		CheckpointInterval: 5,
		ConfirmContinue: func(_ int) bool {
			return false // stop immediately at first checkpoint
		},
	})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "stopped by user")
	assert.Contains(t, err.Error(), "5 iterations")
}

func TestAgentLoop_CheckpointNilAlwaysContinues(t *testing.T) {
	t.Parallel()

	// 5 tool-call iterations then final response.
	responses := make([]*Response, 6)
	for i := range 5 {
		responses[i] = &Response{
			Model:      "test-model",
			StopReason: StopToolUse,
			ToolCalls:  []ToolCall{{ID: "call", Name: "bash", Input: map[string]any{"command": "step"}}},
		}
	}

	responses[5] = &Response{Content: "done", Model: "test-model", StopReason: StopEndTurn}

	reg := NewRegistry()
	reg.Register(&agentTestProvider{responses: responses})

	params := CompleteParams{
		Model:    "test-model",
		Messages: []Message{{Role: RoleUser, Content: "work"}},
		Tools:    DefaultTools(),
	}

	executor := func(_ context.Context, call ToolCall) ToolResult {
		return ToolResult{ToolCallID: call.ID, Content: "ok"}
	}

	// nil ConfirmContinue should not block.
	resp, _, err := AgentLoop(context.Background(), reg, params, nil, executor, AgentLoopConfig{
		MaxIterations:      10,
		CheckpointInterval: 2,
	})
	require.NoError(t, err)
	assert.Equal(t, "done", resp.Content)
}

func TestAgentLoop_ContextCancellation(t *testing.T) {
	t.Parallel()

	reg := NewRegistry()
	reg.Register(&agentTestProvider{
		responses: []*Response{
			{
				Model:      "test-model",
				StopReason: StopToolUse,
				ToolCalls:  []ToolCall{{ID: "call_1", Name: "bash", Input: map[string]any{"command": "sleep 10"}}},
			},
		},
	})

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // Cancel immediately.

	params := CompleteParams{
		Model:    "test-model",
		Messages: []Message{{Role: RoleUser, Content: "test"}},
		Tools:    DefaultTools(),
	}

	executor := func(_ context.Context, call ToolCall) ToolResult {
		return ToolResult{ToolCallID: call.ID, Content: "ok"}
	}

	// The second Complete call should fail because context is canceled.
	_, _, err := AgentLoop(ctx, reg, params, nil, executor, AgentLoopConfig{
		MaxIterations: 5,
	})

	// Either the loop catches the canceled context or the provider fails.
	require.Error(t, err)
}

func TestAgentLoop_MultipleToolCalls(t *testing.T) {
	t.Parallel()

	reg := NewRegistry()
	reg.Register(&agentTestProvider{
		responses: []*Response{
			{
				Model:      "test-model",
				StopReason: StopToolUse,
				ToolCalls: []ToolCall{
					{ID: "call_1", Name: "bash", Input: map[string]any{"command": "ls"}},
					{ID: "call_2", Name: "bash", Input: map[string]any{"command": "pwd"}},
				},
			},
			{Content: "files listed", Model: "test-model", StopReason: StopEndTurn},
		},
	})

	params := CompleteParams{
		Model:    "test-model",
		Messages: []Message{{Role: RoleUser, Content: "list files"}},
		Tools:    DefaultTools(),
	}

	var executedCommands []string

	executor := func(_ context.Context, call ToolCall) ToolResult {
		cmd, ok := call.Input["command"].(string)
		require.True(t, ok, "command input must be a string")

		executedCommands = append(executedCommands, cmd)

		return ToolResult{ToolCallID: call.ID, Content: cmd + " output"}
	}

	resp, history, err := AgentLoop(context.Background(), reg, params, nil, executor, AgentLoopConfig{
		MaxIterations: 5,
	})
	require.NoError(t, err)

	assert.Equal(t, "files listed", resp.Content)
	assert.Equal(t, []string{"ls", "pwd"}, executedCommands)
	// history = original user msg + assistant msg + 2 tool result msgs = 4.
	assert.Len(t, history, 4)
}
