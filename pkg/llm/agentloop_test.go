package llm

import (
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// agentTestProvider is a fake provider for testing the agent loop.
type agentTestProvider struct {
	responses    []*Response
	seenMessages [][]Message
	calls        int
}

func (p *agentTestProvider) Complete(_ context.Context, params CompleteParams) (*Response, error) {
	if p.calls >= len(p.responses) {
		return nil, errors.New("no more responses")
	}

	p.seenMessages = append(p.seenMessages, append([]Message(nil), params.Messages...))

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

func TestAgentLoop_OnContentFiresForFinalResponse(t *testing.T) {
	t.Parallel()

	reg := NewRegistry()
	reg.Register(&agentTestProvider{
		responses: []*Response{
			{Content: "final content", Model: "test-model", StopReason: StopEndTurn},
		},
	})

	var chunks []string

	resp, _, err := AgentLoop(context.Background(), reg, CompleteParams{
		Model:    "test-model",
		Messages: []Message{{Role: RoleUser, Content: "hi"}},
	}, nil, nil, AgentLoopConfig{
		OnContent: func(content string) {
			chunks = append(chunks, content)
		},
	})
	require.NoError(t, err)
	assert.Equal(t, "final content", resp.Content)
	assert.Equal(t, []string{"final content"}, chunks)
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

func TestAgentLoop_BeforeModelCallSeesEachIterationAndCanStop(t *testing.T) {
	t.Parallel()

	provider := &agentTestProvider{
		responses: []*Response{
			{
				Model:      "test-model",
				StopReason: StopToolUse,
				ToolCalls: []ToolCall{
					{ID: "call_1", Name: "bash", Input: map[string]any{"command": "echo ok"}},
				},
			},
			{Content: "should not be called", Model: "test-model", StopReason: StopEndTurn},
		},
	}
	reg := NewRegistry()
	reg.Register(provider)

	var messageCounts []int

	_, history, err := AgentLoop(
		context.Background(),
		reg,
		CompleteParams{
			Model:    "test-model",
			Messages: []Message{{Role: RoleUser, Content: "run echo ok"}},
			Tools:    DefaultTools(),
		},
		nil,
		func(_ context.Context, call ToolCall) ToolResult {
			return ToolResult{ToolCallID: call.ID, Content: "ok\n"}
		},
		AgentLoopConfig{
			MaxIterations: 5,
			BeforeModelCall: func(iteration int, params CompleteParams) error {
				messageCounts = append(messageCounts, len(params.Messages))

				if iteration == 1 {
					return errors.New("token budget exceeded")
				}

				return nil
			},
		},
	)

	require.Error(t, err)
	assert.Contains(t, err.Error(), "preflight")
	assert.Contains(t, err.Error(), "token budget exceeded")
	assert.Equal(t, []int{1, 3}, messageCounts)
	assert.Equal(t, 1, provider.calls, "second provider call should be blocked by preflight")
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

func TestAgentLoop_DefaultMaxIterationsIsUnlimited(t *testing.T) {
	t.Parallel()

	// With no caller-supplied MaxIterations, the budget should not impose a
	// per-iteration hard stop — model and tool call ceilings remain the
	// primary safeguards against runaway loops.
	normalized := normalizeAgentLoopBudget(AgentLoopBudget{}, 0)
	assert.Equal(t, 0, normalized.MaxIterations)
	assert.Nil(t, budgetExceededStop(normalized, AgentLoopUsage{Iterations: 10_000}))
}

func TestAgentLoop_DefaultMaxWallTimeIsUnlimited(t *testing.T) {
	t.Parallel()

	// With no caller-supplied MaxWallTime, normalization must not silently
	// install a 30-minute (or any) ceiling — long-running loops would
	// otherwise be killed without the caller opting in.
	normalized := normalizeAgentLoopBudget(AgentLoopBudget{}, 0)
	assert.Zero(t, normalized.MaxWallTime)
	assert.Nil(t, budgetExceededStop(normalized, AgentLoopUsage{Elapsed: 24 * time.Hour}))
}

func TestAgentLoop_NormalizesNegativeBudgetCeilings(t *testing.T) {
	t.Parallel()

	normalized := normalizeAgentLoopBudget(AgentLoopBudget{
		MaxWallTime:     -time.Second,
		MaxOutputBytes:  -1,
		MaxCostMicros:   -1,
		MaxIterations:   -1,
		MaxModelCalls:   -1,
		MaxToolCalls:    -1,
		MaxInputTokens:  -1,
		MaxOutputTokens: -1,
		MaxTotalTokens:  -1,
	}, 0)

	assert.True(t, normalized.IsZero())
}

func TestAgentLoop_ZeroCheckpointIntervalNeverPrompts(t *testing.T) {
	t.Parallel()

	const toolTurns = 50

	responses := make([]*Response, toolTurns+1)

	for i := range toolTurns {
		responses[i] = &Response{
			Model:      "test-model",
			StopReason: StopToolUse,
			ToolCalls:  []ToolCall{{ID: "call", Name: "bash", Input: map[string]any{"command": "step"}}},
		}
	}

	responses[toolTurns] = &Response{Content: "done", Model: "test-model", StopReason: StopEndTurn}

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
		// CheckpointInterval intentionally left at zero — the loop must run
		// to completion without ever invoking ConfirmContinue.
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

func TestAgentLoop_ContextCancellationDoesNotCheckpointAfterCallerCanceled(t *testing.T) {
	t.Parallel()

	reg := NewRegistry()
	reg.Register(&agentTestProvider{
		responses: []*Response{{Content: "should not run", Model: "test-model", StopReason: StopEndTurn}},
	})

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	ledger := &AgentLoopLedger{}
	_, _, err := AgentLoop(ctx, reg, CompleteParams{
		Model:    "test-model",
		Messages: []Message{{Role: RoleUser, Content: "test"}},
	}, nil, nil, AgentLoopConfig{
		CheckpointSink: ledger,
	})

	require.Error(t, err)
	require.ErrorIs(t, err, context.Canceled)
	assert.Empty(t, ledger.Steps)
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

func TestAgentLoop_ExplicitModelCallBudgetStopsHighMaxIterationsToolLoop(t *testing.T) {
	t.Parallel()

	const modelCallBudget = 25

	responses := make([]*Response, modelCallBudget+5)
	for i := range responses {
		responses[i] = &Response{
			Model:      "test-model",
			StopReason: StopToolUse,
			ToolCalls:  []ToolCall{{ID: "call", Name: "bash", Input: map[string]any{"command": "loop"}}},
		}
	}

	provider := &agentTestProvider{responses: responses}
	reg := NewRegistry()
	reg.Register(provider)

	executions := 0
	executor := func(_ context.Context, call ToolCall) ToolResult {
		executions++

		return ToolResult{ToolCallID: call.ID, Content: "ok"}
	}

	_, _, err := AgentLoop(context.Background(), reg, CompleteParams{
		Model:    "test-model",
		Messages: []Message{{Role: RoleUser, Content: "loop forever"}},
		Tools:    DefaultTools(),
	}, nil, executor, AgentLoopConfig{
		Budget:        AgentLoopBudget{MaxModelCalls: modelCallBudget},
		MaxIterations: 2000,
	})
	require.Error(t, err)
	require.ErrorContains(t, err, "model call budget exhausted")
	assert.Equal(t, modelCallBudget, provider.calls)
	assert.Equal(t, modelCallBudget, executions)
}

func TestAgentLoop_ModelCallBudgetStopsBeforeNextModelCall(t *testing.T) {
	t.Parallel()

	ledger := &AgentLoopLedger{}
	provider := &agentTestProvider{
		responses: []*Response{
			{
				Model:      "test-model",
				StopReason: StopToolUse,
				ToolCalls:  []ToolCall{{ID: "call_1", Name: "bash", Input: map[string]any{"command": "echo ok"}}},
			},
			{Content: "unreachable", Model: "test-model", StopReason: StopEndTurn},
		},
	}
	reg := NewRegistry()
	reg.Register(provider)

	executions := 0
	executor := func(_ context.Context, call ToolCall) ToolResult {
		executions++

		return ToolResult{ToolCallID: call.ID, Content: "ok"}
	}

	_, _, err := AgentLoop(context.Background(), reg, CompleteParams{
		Model:    "test-model",
		Messages: []Message{{Role: RoleUser, Content: "run once"}},
		Tools:    DefaultTools(),
	}, nil, executor, AgentLoopConfig{
		Budget:         AgentLoopBudget{MaxModelCalls: 1, MaxIterations: 10},
		CheckpointSink: ledger,
	})
	require.ErrorContains(t, err, "model call budget exhausted")
	assert.Equal(t, 1, provider.calls)
	assert.Equal(t, 1, executions)
	require.Len(t, ledger.Steps, 3)
	require.NotNil(t, ledger.Steps[2].StopCondition)
	assert.Equal(t, AgentLoopStopMaxModelCalls, ledger.Steps[2].StopCondition.Kind)
}

func TestAgentLoop_ToolCallBudgetRecordsDeniedCallWithoutExecution(t *testing.T) {
	t.Parallel()

	ledger := &AgentLoopLedger{}
	reg := NewRegistry()
	reg.Register(&agentTestProvider{
		responses: []*Response{
			{
				Model:      "test-model",
				StopReason: StopToolUse,
				ToolCalls: []ToolCall{
					{ID: "call_1", Name: "bash", Input: map[string]any{"command": "echo first"}},
					{ID: "call_2", Name: "bash", Input: map[string]any{"command": "echo second"}},
				},
			},
		},
	})

	var executed []string

	executor := func(_ context.Context, call ToolCall) ToolResult {
		command, ok := call.Input["command"].(string)
		require.True(t, ok)

		executed = append(executed, command)

		return ToolResult{ToolCallID: call.ID, Content: "ok"}
	}

	_, history, err := AgentLoop(context.Background(), reg, CompleteParams{
		Model:    "test-model",
		Messages: []Message{{Role: RoleUser, Content: "run two"}},
		Tools:    DefaultTools(),
	}, nil, executor, AgentLoopConfig{
		Budget:         AgentLoopBudget{MaxToolCalls: 1, MaxIterations: 10},
		CheckpointSink: ledger,
	})
	require.ErrorContains(t, err, "tool call budget exhausted")
	assert.Equal(t, []string{"echo first"}, executed)
	require.Len(t, history, 4)
	require.Len(t, ledger.Steps, 4)

	require.NotNil(t, ledger.Steps[2].Policy)
	assert.Equal(t, ToolPolicyDeny, ledger.Steps[2].Policy.Verdict)
	assert.Equal(t, "budget.max_tool_calls", ledger.Steps[2].Policy.MatchedRule)
	require.NotNil(t, ledger.Steps[2].ToolBudget)
	assert.Equal(t, 0, ledger.Steps[2].ToolBudget.RemainingToolCalls)
	require.NotNil(t, ledger.Steps[3].StopCondition)
	assert.Equal(t, AgentLoopStopMaxToolCalls, ledger.Steps[3].StopCondition.Kind)
}

func TestAgentLoop_TokenBudgetStopIsCheckpointed(t *testing.T) {
	t.Parallel()

	ledger := &AgentLoopLedger{}
	reg := NewRegistry()
	reg.Register(&agentTestProvider{
		responses: []*Response{
			{
				Content:      "too many tokens",
				Model:        "test-model",
				StopReason:   StopEndTurn,
				InputTokens:  7,
				OutputTokens: 6,
			},
		},
	})

	_, _, err := AgentLoop(context.Background(), reg, CompleteParams{
		Model:    "test-model",
		Messages: []Message{{Role: RoleUser, Content: "hi"}},
	}, nil, nil, AgentLoopConfig{
		Budget:         AgentLoopBudget{MaxTotalTokens: 10},
		CheckpointSink: ledger,
	})
	require.Error(t, err)
	require.ErrorContains(t, err, "total token budget exceeded")
	require.Len(t, ledger.Steps, 2)
	assert.Equal(t, AgentLoopStepModelResponse, ledger.Steps[0].Kind)
	assert.Equal(t, 13, ledger.Steps[0].Usage.TotalTokens)
	assert.Equal(t, 10, ledger.Steps[0].Budget.MaxTotalTokens)
	require.NotNil(t, ledger.Steps[1].StopCondition)
	assert.Equal(t, 10, ledger.Steps[1].Budget.MaxTotalTokens)
	assert.Equal(t, AgentLoopStopTokenBudget, ledger.Steps[1].StopCondition.Kind)
}

func TestAgentLoop_CheckpointsRecordEffectiveBudget(t *testing.T) {
	t.Parallel()

	budget := AgentLoopBudget{
		MaxWallTime:     time.Minute,
		MaxOutputBytes:  4096,
		MaxCostMicros:   1_000_000,
		MaxIterations:   3,
		MaxModelCalls:   2,
		MaxToolCalls:    2,
		MaxInputTokens:  1_000,
		MaxOutputTokens: 500,
		MaxTotalTokens:  1_500,
	}
	ledger := &AgentLoopLedger{}
	reg := NewRegistry()
	reg.Register(&agentTestProvider{
		responses: []*Response{
			{
				Content:           "done",
				Model:             "test-model",
				StopReason:        StopEndTurn,
				Latency:           123 * time.Millisecond,
				FirstTokenLatency: 45 * time.Millisecond,
				InputTokens:       10,
				OutputTokens:      5,
			},
		},
	})

	_, _, err := AgentLoop(context.Background(), reg, CompleteParams{
		Model:    "test-model",
		Messages: []Message{{Role: RoleUser, Content: "hi"}},
	}, nil, nil, AgentLoopConfig{
		Budget: budget,
		EstimateCostMicros: func(_ *Response) (int64, error) {
			return 100, nil
		},
		CheckpointSink: ledger,
	})
	require.NoError(t, err)
	require.Len(t, ledger.Steps, 2)
	assert.Equal(t, budget, ledger.Budget)
	assert.Equal(t, budget, ledger.Steps[0].Budget)
	assert.Equal(t, budget, ledger.Steps[1].Budget)
	require.NotNil(t, ledger.Steps[0].ModelResponse)
	assert.Equal(t, "test", ledger.Steps[0].ModelResponse.Provider)
	assert.Equal(t, "test-model", ledger.Steps[0].ModelResponse.Model)
	assert.EqualValues(t, 100, ledger.Steps[0].ModelResponse.EstimatedCostMicros)
	assert.Equal(t, 123, ledger.Steps[0].ModelResponse.LatencyMS)
	assert.Equal(t, 45, ledger.Steps[0].ModelResponse.FirstTokenLatencyMS)
}

func TestAgentLoop_JSONLCheckpointPersistsEffectiveBudget(t *testing.T) {
	t.Parallel()

	budget := AgentLoopBudget{
		MaxWallTime:     time.Minute,
		MaxOutputBytes:  4096,
		MaxCostMicros:   1_000_000,
		MaxIterations:   3,
		MaxModelCalls:   2,
		MaxToolCalls:    2,
		MaxInputTokens:  1_000,
		MaxOutputTokens: 500,
		MaxTotalTokens:  1_500,
	}
	checkpointPath := filepath.Join(t.TempDir(), "agentloop.jsonl")
	reg := NewRegistry()
	reg.Register(&agentTestProvider{
		responses: []*Response{
			{
				Content:      "done",
				Model:        "test-model",
				StopReason:   StopEndTurn,
				InputTokens:  10,
				OutputTokens: 5,
			},
		},
	})

	_, _, err := AgentLoop(context.Background(), reg, CompleteParams{
		Model:    "test-model",
		Messages: []Message{{Role: RoleUser, Content: "hi"}},
	}, nil, nil, AgentLoopConfig{
		Budget: budget,
		EstimateCostMicros: func(_ *Response) (int64, error) {
			return 100, nil
		},
		CheckpointSink: NewAgentLoopJSONLCheckpoint(checkpointPath),
	})
	require.NoError(t, err)

	loaded, err := LoadAgentLoopLedger(checkpointPath)
	require.NoError(t, err)
	require.Len(t, loaded.Steps, 2)
	assert.Equal(t, budget, loaded.Budget)
	assert.Equal(t, budget, loaded.Steps[0].Budget)
	assert.Equal(t, budget, loaded.Steps[1].Budget)
	require.NotNil(t, loaded.Steps[0].ModelResponse)
	assert.Equal(t, "test", loaded.Steps[0].ModelResponse.Provider)
	assert.EqualValues(t, 100, loaded.Steps[0].ModelResponse.EstimatedCostMicros)
}

func TestAgentLoopBudgetSnapshotJSONPreservesZeroRemainingConfiguredCeilings(t *testing.T) {
	t.Parallel()

	snapshot := AgentLoopBudgetSnapshot{
		Budget: AgentLoopBudget{
			MaxCostMicros:   10,
			MaxInputTokens:  5,
			MaxOutputTokens: 7,
		},
		Used: AgentLoopUsage{
			EstimatedCostMicros: 10,
			InputTokens:         5,
			OutputTokens:        7,
		},
	}

	data, err := json.Marshal(snapshot)
	require.NoError(t, err)

	assert.Contains(t, string(data), `"remaining_cost_micros":0`)
	assert.Contains(t, string(data), `"remaining_input_tokens":0`)
	assert.Contains(t, string(data), `"remaining_output_tokens":0`)
}

func TestAgentLoopBudgetStopConditionsCoverEveryCeiling(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name             string
		rule             string
		kind             AgentLoopStopKind
		budget           AgentLoopBudget
		used             AgentLoopUsage
		useModelCallStop bool
		useToolStop      bool
	}{
		{
			name:   "wall time",
			budget: AgentLoopBudget{MaxWallTime: time.Second},
			used:   AgentLoopUsage{Elapsed: time.Second},
			rule:   "budget.max_wall_time",
			kind:   AgentLoopStopWallTime,
		},
		{
			name:   "output bytes",
			budget: AgentLoopBudget{MaxOutputBytes: 10},
			used:   AgentLoopUsage{OutputBytes: 11},
			rule:   "budget.max_output_bytes",
			kind:   AgentLoopStopOutputBytes,
		},
		{
			name:   "cost",
			budget: AgentLoopBudget{MaxCostMicros: 10},
			used:   AgentLoopUsage{EstimatedCostMicros: 11},
			rule:   "budget.max_cost_micros",
			kind:   AgentLoopStopCostBudget,
		},
		{
			name:   "iterations",
			budget: AgentLoopBudget{MaxIterations: 2},
			used:   AgentLoopUsage{Iterations: 2},
			rule:   "budget.max_iterations",
			kind:   AgentLoopStopMaxIterations,
		},
		{
			name:             "model calls",
			budget:           AgentLoopBudget{MaxModelCalls: 2},
			used:             AgentLoopUsage{ModelCalls: 2},
			rule:             "budget.max_model_calls",
			kind:             AgentLoopStopMaxModelCalls,
			useModelCallStop: true,
		},
		{
			name:        "tool calls",
			budget:      AgentLoopBudget{MaxToolCalls: 2},
			used:        AgentLoopUsage{ToolCalls: 2},
			rule:        "budget.max_tool_calls",
			kind:        AgentLoopStopMaxToolCalls,
			useToolStop: true,
		},
		{
			name:   "input tokens",
			budget: AgentLoopBudget{MaxInputTokens: 10},
			used:   AgentLoopUsage{InputTokens: 11},
			rule:   "budget.max_input_tokens",
			kind:   AgentLoopStopTokenBudget,
		},
		{
			name:   "output tokens",
			budget: AgentLoopBudget{MaxOutputTokens: 10},
			used:   AgentLoopUsage{OutputTokens: 11},
			rule:   "budget.max_output_tokens",
			kind:   AgentLoopStopTokenBudget,
		},
		{
			name:   "total tokens",
			budget: AgentLoopBudget{MaxTotalTokens: 10},
			used:   AgentLoopUsage{TotalTokens: 11},
			rule:   "budget.max_total_tokens",
			kind:   AgentLoopStopTokenBudget,
		},
	}

	coveredFields := make(map[string]bool, len(cases))
	for _, tc := range cases {
		coveredFields[strings.TrimPrefix(tc.rule, "budget.")] = true
	}

	assert.Equal(t, agentLoopBudgetJSONFieldsForTest(), coveredFields)

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			cond := budgetExceededStop(tc.budget, tc.used)
			if tc.useModelCallStop {
				cond = modelCallBudgetStop(tc.budget, tc.used)
			}

			if tc.useToolStop {
				state := newAgentLoopState(nil, AgentLoopConfig{Budget: tc.budget})
				state.usage = tc.used
				cond = state.preToolStopCondition()
			}

			require.NotNil(t, cond)
			assert.Equal(t, tc.rule, cond.MatchedRule)
			assert.Equal(t, tc.kind, cond.Kind)
		})
	}
}

func agentLoopBudgetJSONFieldsForTest() map[string]bool {
	budgetType := reflect.TypeFor[AgentLoopBudget]()
	fields := make(map[string]bool, budgetType.NumField())

	for field := range budgetType.Fields() {
		name, _, _ := strings.Cut(field.Tag.Get("json"), ",")

		if name == "" || name == "-" {
			continue
		}

		fields[name] = true
	}

	return fields
}

func TestAgentLoop_InputTokenBudgetIsDistinctFromTotalTokenBudget(t *testing.T) {
	t.Parallel()

	ledger := &AgentLoopLedger{}
	reg := NewRegistry()
	reg.Register(&agentTestProvider{
		responses: []*Response{
			{
				Content:      "too much input",
				Model:        "test-model",
				StopReason:   StopEndTurn,
				InputTokens:  7,
				OutputTokens: 1,
			},
		},
	})

	_, _, err := AgentLoop(context.Background(), reg, CompleteParams{
		Model:    "test-model",
		Messages: []Message{{Role: RoleUser, Content: "hi"}},
	}, nil, nil, AgentLoopConfig{
		Budget:         AgentLoopBudget{MaxInputTokens: 6, MaxTotalTokens: 100},
		CheckpointSink: ledger,
	})
	require.ErrorContains(t, err, "input token budget exceeded")
	require.Len(t, ledger.Steps, 2)
	require.NotNil(t, ledger.Steps[1].StopCondition)
	assert.Equal(t, "budget.max_input_tokens", ledger.Steps[1].StopCondition.MatchedRule)
	assert.Equal(t, AgentLoopStopTokenBudget, ledger.Steps[1].StopCondition.Kind)
}

func TestAgentLoop_OutputTokenBudgetIsDistinctFromTotalTokenBudget(t *testing.T) {
	t.Parallel()

	ledger := &AgentLoopLedger{}
	reg := NewRegistry()
	reg.Register(&agentTestProvider{
		responses: []*Response{
			{
				Content:      "too much output",
				Model:        "test-model",
				StopReason:   StopEndTurn,
				InputTokens:  1,
				OutputTokens: 7,
			},
		},
	})

	_, _, err := AgentLoop(context.Background(), reg, CompleteParams{
		Model:    "test-model",
		Messages: []Message{{Role: RoleUser, Content: "hi"}},
	}, nil, nil, AgentLoopConfig{
		Budget:         AgentLoopBudget{MaxOutputTokens: 6, MaxTotalTokens: 100},
		CheckpointSink: ledger,
	})
	require.ErrorContains(t, err, "output token budget exceeded")
	require.Len(t, ledger.Steps, 2)
	require.NotNil(t, ledger.Steps[1].StopCondition)
	assert.Equal(t, "budget.max_output_tokens", ledger.Steps[1].StopCondition.MatchedRule)
	assert.Equal(t, AgentLoopStopTokenBudget, ledger.Steps[1].StopCondition.Kind)
}

func TestAgentLoop_ExactUsageBudgetStopsBeforeFollowUpToolWork(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct { //nolint:govet // Test table order favors readable setup/expectation grouping over field packing.
		name          string
		budget        AgentLoopBudget
		response      *Response
		costEstimator AgentLoopCostEstimator
		wantRule      string
		wantText      string
		wantKind      AgentLoopStopKind
	}{
		{
			name:   "input tokens",
			budget: AgentLoopBudget{MaxInputTokens: 10},
			response: &Response{
				Model:        "test-model",
				StopReason:   StopToolUse,
				ToolCalls:    []ToolCall{{ID: "call-input", Name: "bash"}},
				InputTokens:  10,
				OutputTokens: 1,
			},
			wantRule: "budget.max_input_tokens",
			wantText: "input token budget exhausted",
			wantKind: AgentLoopStopTokenBudget,
		},
		{
			name:   "output tokens",
			budget: AgentLoopBudget{MaxOutputTokens: 10},
			response: &Response{
				Model:        "test-model",
				StopReason:   StopToolUse,
				ToolCalls:    []ToolCall{{ID: "call-output", Name: "bash"}},
				InputTokens:  1,
				OutputTokens: 10,
			},
			wantRule: "budget.max_output_tokens",
			wantText: "output token budget exhausted",
			wantKind: AgentLoopStopTokenBudget,
		},
		{
			name:   "total tokens",
			budget: AgentLoopBudget{MaxTotalTokens: 11},
			response: &Response{
				Model:        "test-model",
				StopReason:   StopToolUse,
				ToolCalls:    []ToolCall{{ID: "call-total", Name: "bash"}},
				InputTokens:  10,
				OutputTokens: 1,
			},
			wantRule: "budget.max_total_tokens",
			wantText: "total token budget exhausted",
			wantKind: AgentLoopStopTokenBudget,
		},
		{
			name:   "cost",
			budget: AgentLoopBudget{MaxCostMicros: 10},
			response: &Response{
				Model:        "test-model",
				StopReason:   StopToolUse,
				ToolCalls:    []ToolCall{{ID: "call-cost", Name: "bash"}},
				InputTokens:  1,
				OutputTokens: 1,
			},
			costEstimator: func(_ *Response) (int64, error) {
				return 10, nil
			},
			wantRule: "budget.max_cost_micros",
			wantText: "cost budget exhausted",
			wantKind: AgentLoopStopCostBudget,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			ledger := &AgentLoopLedger{}
			provider := &agentTestProvider{responses: []*Response{tc.response}}
			reg := NewRegistry()
			reg.Register(provider)

			executions := 0
			executor := func(_ context.Context, call ToolCall) ToolResult {
				executions++

				return ToolResult{ToolCallID: call.ID, Content: "should not run"}
			}

			_, _, err := AgentLoop(context.Background(), reg, CompleteParams{
				Model:    "test-model",
				Messages: []Message{{Role: RoleUser, Content: "hi"}},
			}, nil, executor, AgentLoopConfig{
				Budget:             tc.budget,
				EstimateCostMicros: tc.costEstimator,
				CheckpointSink:     ledger,
			})
			require.ErrorContains(t, err, tc.wantText)
			assert.Equal(t, 1, provider.calls)
			assert.Zero(t, executions, "exactly exhausted usage budget should deny follow-up tool work")
			require.Len(t, ledger.Steps, 3)
			assert.Equal(t, AgentLoopStepToolCall, ledger.Steps[1].Kind)
			require.NotNil(t, ledger.Steps[2].StopCondition)
			assert.Equal(t, tc.wantRule, ledger.Steps[2].StopCondition.MatchedRule)
			assert.Equal(t, tc.wantKind, ledger.Steps[2].StopCondition.Kind)
		})
	}
}

func TestAgentLoop_ExactOutputByteBudgetStopsBeforeNextModelCall(t *testing.T) {
	t.Parallel()

	ledger := &AgentLoopLedger{}
	provider := &agentTestProvider{
		responses: []*Response{
			{
				Model:      "test-model",
				StopReason: StopToolUse,
				ToolCalls:  []ToolCall{{ID: "call-output-bytes", Name: "bash"}},
			},
			{
				Content:    "should not be called",
				Model:      "test-model",
				StopReason: StopEndTurn,
			},
		},
	}
	reg := NewRegistry()
	reg.Register(provider)

	executions := 0
	executor := func(_ context.Context, call ToolCall) ToolResult {
		executions++

		return ToolResult{ToolCallID: call.ID, Content: "1234"}
	}

	_, _, err := AgentLoop(context.Background(), reg, CompleteParams{
		Model:    "test-model",
		Messages: []Message{{Role: RoleUser, Content: "hi"}},
	}, nil, executor, AgentLoopConfig{
		Budget:         AgentLoopBudget{MaxOutputBytes: 4},
		CheckpointSink: ledger,
	})
	require.ErrorContains(t, err, "tool output byte budget exhausted")
	assert.Equal(t, 1, provider.calls, "exact output-byte budget should block the next model call")
	assert.Equal(t, 1, executions)
	require.Len(t, ledger.Steps, 3)
	require.NotNil(t, ledger.Steps[2].StopCondition)
	assert.Equal(t, "budget.max_output_bytes", ledger.Steps[2].StopCondition.MatchedRule)
	assert.Equal(t, AgentLoopStopOutputBytes, ledger.Steps[2].StopCondition.Kind)
}

func TestAgentLoop_TokenBudgetFailsClosedWithoutUsageMetadata(t *testing.T) {
	t.Parallel()

	ledger := &AgentLoopLedger{}
	reg := NewRegistry()
	reg.Register(&agentTestProvider{
		responses: []*Response{
			{
				Content:    "unmetered answer",
				Model:      "test-model",
				StopReason: StopEndTurn,
			},
		},
	})

	_, _, err := AgentLoop(context.Background(), reg, CompleteParams{
		Model:    "test-model",
		Messages: []Message{{Role: RoleUser, Content: "hi"}},
	}, nil, nil, AgentLoopConfig{
		Budget:         AgentLoopBudget{MaxInputTokens: 100},
		CheckpointSink: ledger,
	})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "token budget could not be enforced")
	assert.Contains(t, err.Error(), ErrAgentLoopTokenUsageUnavailable.Error())
	require.Len(t, ledger.Steps, 2)
	require.NotNil(t, ledger.Steps[1].StopCondition)
	assert.Equal(t, AgentLoopStopTokenBudget, ledger.Steps[1].StopCondition.Kind)
	assert.Equal(t, "budget.max_input_tokens", ledger.Steps[1].StopCondition.MatchedRule)
}

func TestAgentLoop_TokenBudgetFailsClosedWithPartialUsageMetadata(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct { //nolint:govet // Test table order favors readable setup/expectation grouping over field packing.
		response *Response
		budget   AgentLoopBudget
		name     string
		wantRule string
		wantText string
	}{
		{
			name:     "input ceiling requires input usage",
			budget:   AgentLoopBudget{MaxInputTokens: 100},
			response: &Response{Content: "answer", Model: "test-model", StopReason: StopEndTurn, OutputTokens: 5},
			wantRule: "budget.max_input_tokens",
			wantText: "input token usage unavailable",
		},
		{
			name:     "output ceiling requires output usage for visible output",
			budget:   AgentLoopBudget{MaxOutputTokens: 100},
			response: &Response{Content: "answer", Model: "test-model", StopReason: StopEndTurn, InputTokens: 5},
			wantRule: "budget.max_output_tokens",
			wantText: "output token usage unavailable",
		},
		{
			name:     "total ceiling requires output usage for visible output",
			budget:   AgentLoopBudget{MaxTotalTokens: 100},
			response: &Response{Content: "answer", Model: "test-model", StopReason: StopEndTurn, InputTokens: 5},
			wantRule: "budget.max_total_tokens",
			wantText: "output token usage unavailable",
		},
		{
			name:     "combined input and output ceilings attribute missing output usage",
			budget:   AgentLoopBudget{MaxInputTokens: 100, MaxOutputTokens: 100},
			response: &Response{Content: "answer", Model: "test-model", StopReason: StopEndTurn, InputTokens: 5},
			wantRule: "budget.max_output_tokens",
			wantText: "output token usage unavailable",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			ledger := &AgentLoopLedger{}
			reg := NewRegistry()
			reg.Register(&agentTestProvider{responses: []*Response{tc.response}})

			_, _, err := AgentLoop(context.Background(), reg, CompleteParams{
				Model:    "test-model",
				Messages: []Message{{Role: RoleUser, Content: "hi"}},
			}, nil, nil, AgentLoopConfig{
				Budget:         tc.budget,
				CheckpointSink: ledger,
			})
			require.Error(t, err)
			assert.Contains(t, err.Error(), "token budget could not be enforced")
			assert.Contains(t, err.Error(), tc.wantText)
			require.Len(t, ledger.Steps, 2)
			require.NotNil(t, ledger.Steps[1].StopCondition)
			assert.Equal(t, AgentLoopStopTokenBudget, ledger.Steps[1].StopCondition.Kind)
			assert.Equal(t, tc.wantRule, ledger.Steps[1].StopCondition.MatchedRule)
		})
	}
}

func TestAgentLoop_AggregatesCacheWriteUsage(t *testing.T) {
	t.Parallel()

	ledger := &AgentLoopLedger{}
	reg := NewRegistry()
	reg.Register(&agentTestProvider{
		responses: []*Response{
			{
				Content:               "cached answer",
				Model:                 "test-model",
				StopReason:            StopEndTurn,
				InputTokens:           10,
				CachedInputTokens:     3,
				CacheWriteInputTokens: 2,
				OutputTokens:          5,
			},
		},
	})

	resp, _, err := AgentLoop(context.Background(), reg, CompleteParams{
		Model:    "test-model",
		Messages: []Message{{Role: RoleUser, Content: "hi"}},
	}, nil, nil, AgentLoopConfig{CheckpointSink: ledger})

	require.NoError(t, err)
	assert.Equal(t, 2, resp.CacheWriteInputTokens)
	require.Len(t, ledger.Steps, 2)
	assert.Equal(t, 2, ledger.Steps[0].Usage.CacheWriteTokens)
	require.NotNil(t, ledger.Steps[0].ModelResponse)
	assert.Equal(t, 2, ledger.Steps[0].ModelResponse.CacheWriteTokens)
}

func TestAgentLoop_DefaultTotalTokenBudgetIsUnlimited(t *testing.T) {
	t.Parallel()

	reg := NewRegistry()
	reg.Register(&agentTestProvider{
		responses: []*Response{
			{
				Content:      "large token response",
				Model:        "test-model",
				StopReason:   StopEndTurn,
				InputTokens:  120_000,
				OutputTokens: 90_000,
			},
		},
	})

	resp, _, err := AgentLoop(context.Background(), reg, CompleteParams{
		Model:    "test-model",
		Messages: []Message{{Role: RoleUser, Content: "hi"}},
	}, nil, nil, AgentLoopConfig{})
	require.NoError(t, err)
	assert.Equal(t, 210_000, resp.InputTokens+resp.OutputTokens)
}

func TestAgentLoop_CostBudgetRequiresEstimatorAndStops(t *testing.T) {
	t.Parallel()

	reg := NewRegistry()
	reg.Register(&agentTestProvider{
		responses: []*Response{{Content: "done", Model: "test-model", StopReason: StopEndTurn}},
	})

	_, _, err := AgentLoop(context.Background(), reg, CompleteParams{Model: "test-model"}, nil, nil, AgentLoopConfig{
		Budget: AgentLoopBudget{MaxCostMicros: 100},
	})
	require.ErrorContains(t, err, "cost budget requires EstimateCostMicros")

	ledger := &AgentLoopLedger{}
	reg = NewRegistry()
	reg.Register(&agentTestProvider{
		responses: []*Response{{Content: "done", Model: "test-model", StopReason: StopEndTurn}},
	})

	_, _, err = AgentLoop(context.Background(), reg, CompleteParams{Model: "test-model"}, nil, nil, AgentLoopConfig{
		Budget: AgentLoopBudget{MaxCostMicros: 100},
		EstimateCostMicros: func(_ *Response) (int64, error) {
			return 150, nil
		},
		CheckpointSink: ledger,
	})
	require.ErrorContains(t, err, "cost budget exceeded")
	require.Len(t, ledger.Steps, 2)
	require.NotNil(t, ledger.Steps[1].StopCondition)
	assert.Equal(t, AgentLoopStopCostBudget, ledger.Steps[1].StopCondition.Kind)
}

func TestAgentLoop_WallClockBudgetStopsBeforeModelCall(t *testing.T) {
	t.Parallel()

	base := time.Date(2026, 5, 21, 10, 0, 0, 0, time.UTC)
	times := []time.Time{base, base.Add(2 * time.Second), base.Add(2 * time.Second)}
	nextTime := func() time.Time {
		if len(times) == 0 {
			return base.Add(2 * time.Second)
		}

		t := times[0]
		times = times[1:]

		return t
	}

	provider := &agentTestProvider{
		responses: []*Response{{Content: "should not be called", Model: "test-model", StopReason: StopEndTurn}},
	}
	reg := NewRegistry()
	reg.Register(provider)

	_, _, err := AgentLoop(context.Background(), reg, CompleteParams{Model: "test-model"}, nil, nil, AgentLoopConfig{
		Budget: AgentLoopBudget{MaxWallTime: time.Second},
		Now:    nextTime,
	})
	require.ErrorContains(t, err, "wall-clock budget exhausted")
	assert.Zero(t, provider.calls)
}

func TestAgentLoop_ToolContextCarriesRemainingWallTimeDeadline(t *testing.T) {
	t.Parallel()

	reg := NewRegistry()
	reg.Register(&agentTestProvider{
		responses: []*Response{
			{
				Model:      "test-model",
				StopReason: StopToolUse,
				ToolCalls:  []ToolCall{{ID: "call_1", Name: "bash", Input: map[string]any{"command": "sleep"}}},
			},
			{Content: "done", Model: "test-model", StopReason: StopEndTurn},
		},
	})

	executor := func(ctx context.Context, call ToolCall) ToolResult {
		deadline, ok := ctx.Deadline()
		require.True(t, ok, "tool context should carry a wall-time budget deadline")
		assert.Positive(t, time.Until(deadline))
		assert.LessOrEqual(t, time.Until(deadline), 2*time.Second)

		return ToolResult{ToolCallID: call.ID, Content: "ok"}
	}

	resp, _, err := AgentLoop(context.Background(), reg, CompleteParams{
		Model:    "test-model",
		Messages: []Message{{Role: RoleUser, Content: "run"}},
		Tools:    DefaultTools(),
	}, nil, executor, AgentLoopConfig{
		Budget: AgentLoopBudget{MaxWallTime: 2 * time.Second, MaxIterations: 5},
	})
	require.NoError(t, err)
	assert.Equal(t, "done", resp.Content)
}

func TestAgentLoop_OutputBudgetStopsAndKeepsFullResultOutOfHistory(t *testing.T) {
	t.Parallel()

	ledger := &AgentLoopLedger{}
	reg := NewRegistry()
	reg.Register(&agentTestProvider{
		responses: []*Response{
			{
				Model:      "test-model",
				StopReason: StopToolUse,
				ToolCalls:  []ToolCall{{ID: "call_1", Name: "bash", Input: map[string]any{"command": "cat big"}}},
			},
			{Content: "unreachable", Model: "test-model", StopReason: StopEndTurn},
		},
	})

	fullOutput := strings.Repeat("x", 200)
	executor := func(ctx context.Context, call ToolCall) ToolResult {
		snapshot, ok := AgentLoopBudgetSnapshotFromContext(ctx)
		assert.True(t, ok)
		assert.Equal(t, int64(3), snapshot.RemainingOutputBytes)

		return ToolResult{ToolCallID: call.ID, Content: fullOutput}
	}

	_, history, err := AgentLoop(context.Background(), reg, CompleteParams{
		Model:    "test-model",
		Messages: []Message{{Role: RoleUser, Content: "run"}},
		Tools:    DefaultTools(),
	}, nil, executor, AgentLoopConfig{
		Budget:                    AgentLoopBudget{MaxOutputBytes: 3},
		MaxHistoryToolResultBytes: 128,
		CheckpointSink:            ledger,
	})
	require.ErrorContains(t, err, "tool output byte budget exceeded")
	require.Len(t, history, 3)
	assert.Contains(t, history[2].Content, "truncated")
	assert.LessOrEqual(t, len([]byte(history[2].Content)), 128)

	require.Len(t, ledger.Steps, 3)
	toolStep := ledger.Steps[1]
	require.NotNil(t, toolStep.ToolResult)
	require.NotNil(t, toolStep.PromptResult)
	assert.Equal(t, fullOutput, toolStep.ToolResult.Content)
	assert.NotEqual(t, toolStep.ToolResult.Content, toolStep.PromptResult.Content)
	require.NotNil(t, ledger.Steps[2].StopCondition)
	assert.Equal(t, AgentLoopStopOutputBytes, ledger.Steps[2].StopCondition.Kind)
}

func TestAgentLoop_DefaultOutputBudgetIsUnlimited(t *testing.T) {
	t.Parallel()

	reg := NewRegistry()
	reg.Register(&agentTestProvider{
		responses: []*Response{
			{
				Model:      "test-model",
				StopReason: StopToolUse,
				ToolCalls:  []ToolCall{{ID: "call_1", Name: "bash", Input: map[string]any{"command": "cat big"}}},
			},
			{Content: "done", Model: "test-model", StopReason: StopEndTurn},
		},
	})

	executor := func(ctx context.Context, call ToolCall) ToolResult {
		snapshot, ok := AgentLoopBudgetSnapshotFromContext(ctx)
		assert.True(t, ok)
		assert.Zero(t, snapshot.RemainingOutputBytes)

		return ToolResult{ToolCallID: call.ID, Content: strings.Repeat("x", 1<<20+1)}
	}

	resp, _, err := AgentLoop(context.Background(), reg, CompleteParams{
		Model:    "test-model",
		Messages: []Message{{Role: RoleUser, Content: "run"}},
		Tools:    DefaultTools(),
	}, nil, executor, AgentLoopConfig{})
	require.NoError(t, err)
	assert.Equal(t, "done", resp.Content)
}

func TestPromptToolResultCapsHistoryBytes(t *testing.T) {
	t.Parallel()

	result := promptToolResult(ToolResult{ToolCallID: "call_1", Content: "abcdef"}, 4)

	assert.LessOrEqual(t, len([]byte(result.Content)), 4)
	assert.NotEqual(t, "abcdef", result.Content)
	assert.Equal(t, "call_1", result.ToolCallID)

	result = promptToolResult(ToolResult{ToolCallID: "call_2", Content: strings.Repeat("x", 200)}, 128)

	assert.LessOrEqual(t, len([]byte(result.Content)), 128)
	assert.Contains(t, result.Content, "truncated")
	assert.Equal(t, "call_2", result.ToolCallID)
}

func TestAgentLoop_PolicyDenialStopsBeforeExecution(t *testing.T) {
	t.Parallel()

	ledger := &AgentLoopLedger{}
	reg := NewRegistry()
	reg.Register(&agentTestProvider{
		responses: []*Response{
			{
				Model:      "test-model",
				StopReason: StopToolUse,
				ToolCalls:  []ToolCall{{ID: "call_1", Name: "bash", Input: map[string]any{"command": "rm -rf /tmp/nope"}}},
			},
		},
	})

	executed := false
	executor := func(_ context.Context, call ToolCall) ToolResult {
		executed = true

		return ToolResult{ToolCallID: call.ID, Content: "should not run"}
	}

	_, history, err := AgentLoop(context.Background(), reg, CompleteParams{
		Model:    "test-model",
		Messages: []Message{{Role: RoleUser, Content: "delete"}},
		Tools:    DefaultTools(),
	}, nil, executor, AgentLoopConfig{
		CheckpointSink: ledger,
		Policy: func(_ context.Context, _ ToolCall, _ AgentLoopBudgetSnapshot) ToolPolicyDecision {
			return ToolPolicyDecision{
				Verdict:     ToolPolicyDeny,
				Reason:      "destructive shell command",
				MatchedRule: "test.deny_rm",
			}
		},
	})
	require.ErrorContains(t, err, "tool call denied by policy")
	assert.False(t, executed)
	require.Len(t, history, 3)
	assert.True(t, history[2].ToolResult.IsError)
	require.Len(t, ledger.Steps, 3)
	toolStep := ledger.Steps[1]
	require.NotNil(t, toolStep.Policy)
	assert.Equal(t, ToolPolicyDeny, toolStep.Policy.Verdict)
	assert.Equal(t, "test.deny_rm", toolStep.Policy.MatchedRule)
	require.NotNil(t, toolStep.ToolBudget)
	assert.Equal(t, 0, toolStep.ToolBudget.Budget.MaxToolCalls)
	require.NotNil(t, ledger.Steps[2].StopCondition)
	assert.Equal(t, AgentLoopStopPolicyDenied, ledger.Steps[2].StopCondition.Kind)
}

func TestAgentLoop_RequireConfirmWithoutCallbackStopsBeforeExecution(t *testing.T) {
	t.Parallel()

	ledger := &AgentLoopLedger{}
	reg := NewRegistry()
	reg.Register(&agentTestProvider{
		responses: []*Response{
			{
				Model:      "test-model",
				StopReason: StopToolUse,
				ToolCalls:  []ToolCall{{ID: "call_1", Name: "bash", Input: map[string]any{"command": "sudo make install"}}},
			},
		},
	})

	executed := false
	executor := func(_ context.Context, call ToolCall) ToolResult {
		executed = true

		return ToolResult{ToolCallID: call.ID, Content: "should not run"}
	}

	_, _, err := AgentLoop(context.Background(), reg, CompleteParams{
		Model:    "test-model",
		Messages: []Message{{Role: RoleUser, Content: "install"}},
		Tools:    DefaultTools(),
	}, nil, executor, AgentLoopConfig{
		CheckpointSink: ledger,
		Policy:         BashToolPolicy,
	})
	require.ErrorContains(t, err, "requires confirmation")
	assert.False(t, executed)
	require.Len(t, ledger.Steps, 3)
	require.NotNil(t, ledger.Steps[1].Policy)
	assert.Equal(t, ToolPolicyRequireConfirm, ledger.Steps[1].Policy.Verdict)
	require.NotNil(t, ledger.Steps[2].StopCondition)
	assert.Equal(t, AgentLoopStopConfirmationRequired, ledger.Steps[2].StopCondition.Kind)
}

func TestAgentLoop_DryRunPolicyRecordsWithoutExecuting(t *testing.T) {
	t.Parallel()

	ledger := &AgentLoopLedger{}
	reg := NewRegistry()
	reg.Register(&agentTestProvider{
		responses: []*Response{
			{
				Model:      "test-model",
				StopReason: StopToolUse,
				ToolCalls:  []ToolCall{{ID: "call_1", Name: "bash", Input: map[string]any{"command": "echo dry"}}},
			},
			{Content: "done", Model: "test-model", StopReason: StopEndTurn},
		},
	})

	executed := false
	executor := func(_ context.Context, call ToolCall) ToolResult {
		executed = true

		return ToolResult{ToolCallID: call.ID, Content: "should not run"}
	}

	resp, history, err := AgentLoop(context.Background(), reg, CompleteParams{
		Model:    "test-model",
		Messages: []Message{{Role: RoleUser, Content: "dry"}},
		Tools:    DefaultTools(),
	}, nil, executor, AgentLoopConfig{
		CheckpointSink: ledger,
		Policy: func(_ context.Context, _ ToolCall, _ AgentLoopBudgetSnapshot) ToolPolicyDecision {
			return ToolPolicyDecision{
				Verdict:     ToolPolicyDryRun,
				Reason:      "test dry run",
				MatchedRule: "test.dry_run",
			}
		},
	})
	require.NoError(t, err)
	assert.Equal(t, "done", resp.Content)
	assert.False(t, executed)
	require.Len(t, history, 3)
	assert.Contains(t, history[2].Content, "dry run")
	require.NotNil(t, ledger.Steps[1].Policy)
	assert.Equal(t, ToolPolicyDryRun, ledger.Steps[1].Policy.Verdict)
}

func TestAgentLoop_ReplayStepsResumeWithoutReexecutingTool(t *testing.T) {
	t.Parallel()

	checkpointPath := filepath.Join(t.TempDir(), "agent-loop.jsonl")
	firstProvider := &agentTestProvider{
		responses: []*Response{
			{
				Model:      "test-model",
				StopReason: StopToolUse,
				ToolCalls:  []ToolCall{{ID: "call_1", Name: "bash", Input: map[string]any{"command": "echo ok"}}},
			},
		},
	}
	firstReg := NewRegistry()
	firstReg.Register(firstProvider)

	firstExecutorCalls := 0
	firstExecutor := func(_ context.Context, call ToolCall) ToolResult {
		firstExecutorCalls++

		return ToolResult{ToolCallID: call.ID, Content: "ok\n"}
	}

	initialMessages := []Message{{Role: RoleUser, Content: "run echo ok"}}
	_, _, err := AgentLoop(context.Background(), firstReg, CompleteParams{
		Model:    "test-model",
		Messages: initialMessages,
		Tools:    DefaultTools(),
	}, nil, firstExecutor, AgentLoopConfig{
		Budget:         AgentLoopBudget{MaxIterations: 1},
		CheckpointSink: NewAgentLoopJSONLCheckpoint(checkpointPath),
	})
	require.ErrorContains(t, err, "exceeded 1 iterations")
	assert.Equal(t, 1, firstExecutorCalls)

	loaded, err := LoadAgentLoopLedger(checkpointPath)
	require.NoError(t, err)
	require.Len(t, loaded.Steps, 3)

	secondProvider := &agentTestProvider{
		responses: []*Response{{Content: "done", Model: "test-model", StopReason: StopEndTurn}},
	}
	secondReg := NewRegistry()
	secondReg.Register(secondProvider)

	secondExecutor := func(_ context.Context, call ToolCall) ToolResult {
		t.Fatalf("replayed tool call should not execute again: %s", call.ID)

		return ToolResult{}
	}

	resp, history, err := AgentLoop(context.Background(), secondReg, CompleteParams{
		Model:    "test-model",
		Messages: initialMessages,
		Tools:    DefaultTools(),
	}, nil, secondExecutor, AgentLoopConfig{
		Budget:      AgentLoopBudget{MaxIterations: 3},
		ReplaySteps: loaded.Steps,
	})
	require.NoError(t, err)
	assert.Equal(t, "done", resp.Content)
	require.Len(t, history, 3)
	require.Len(t, secondProvider.seenMessages, 1)
	assert.Len(t, secondProvider.seenMessages[0], 3)
	assert.Equal(t, RoleTool, secondProvider.seenMessages[0][2].Role)
	assert.Equal(t, "ok\n", secondProvider.seenMessages[0][2].Content)
}

func TestAgentLoop_PartialToolCallFailureIsRecordedAndContinues(t *testing.T) {
	t.Parallel()

	ledger := &AgentLoopLedger{}
	reg := NewRegistry()
	reg.Register(&agentTestProvider{
		responses: []*Response{
			{
				Model:      "test-model",
				StopReason: StopToolUse,
				ToolCalls: []ToolCall{
					{ID: "call_1", Name: "bash", Input: map[string]any{"command": "false"}},
					{ID: "call_2", Name: "bash", Input: map[string]any{"command": "echo ok"}},
				},
			},
			{Content: "recovered", Model: "test-model", StopReason: StopEndTurn},
		},
	})

	executor := func(_ context.Context, call ToolCall) ToolResult {
		if call.ID == "call_1" {
			return ToolResult{ToolCallID: call.ID, Content: "exit status 1", IsError: true}
		}

		return ToolResult{ToolCallID: call.ID, Content: "ok\n"}
	}

	resp, history, err := AgentLoop(context.Background(), reg, CompleteParams{
		Model:    "test-model",
		Messages: []Message{{Role: RoleUser, Content: "run both"}},
		Tools:    DefaultTools(),
	}, nil, executor, AgentLoopConfig{
		CheckpointSink: ledger,
	})
	require.NoError(t, err)
	assert.Equal(t, "recovered", resp.Content)
	require.Len(t, history, 4)

	var toolSteps []AgentLoopStep

	for _, step := range ledger.Steps {
		if step.Kind == AgentLoopStepToolCall {
			toolSteps = append(toolSteps, step)
		}
	}

	require.Len(t, toolSteps, 2)
	require.NotNil(t, toolSteps[0].ToolResult)
	require.NotNil(t, toolSteps[1].ToolResult)
	assert.True(t, toolSteps[0].ToolResult.IsError)
	assert.False(t, toolSteps[1].ToolResult.IsError)
}
