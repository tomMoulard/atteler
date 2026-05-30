package main

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

	appconfig "github.com/tommoulard/atteler/pkg/config"
	"github.com/tommoulard/atteler/pkg/llm"
)

func TestAgentLoopCheckpointPath(t *testing.T) {
	t.Parallel()

	assert.Empty(t, agentLoopCheckpointPath(" \t "))
	assert.Equal(t,
		"/tmp/sessions/abc.agentloop.jsonl",
		agentLoopCheckpointPath("/tmp/sessions/abc.json"),
	)
	assert.Equal(t,
		"/tmp/sessions/abc.agentloop.jsonl",
		agentLoopCheckpointPath("/tmp/sessions/abc"),
	)
}

func TestAgentLoopCheckpointSinkWritesJSONL(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), "session.agentloop.jsonl")
	sink := agentLoopCheckpointSink(path)
	require.NotNil(t, sink)

	require.NoError(t, sink.SaveAgentLoopStep(t.Context(), llm.AgentLoopStep{Kind: llm.AgentLoopStepStop}))

	ledger, err := llm.LoadAgentLoopLedger(path)
	require.NoError(t, err)
	require.Len(t, ledger.Steps, 1)
	assert.Equal(t, llm.AgentLoopStepStop, ledger.Steps[0].Kind)
}

func TestAgentLoopErrorMentionsCheckpointPath(t *testing.T) {
	t.Parallel()

	err := agentLoopError(errors.New("budget exhausted"), "/tmp/session.agentloop.jsonl")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "budget exhausted")
	assert.Contains(t, err.Error(), "/tmp/session.agentloop.jsonl")

	assert.EqualError(t, agentLoopError(errors.New("plain"), ""), "plain")
}

func TestAgentLoopToolOutputLimitDefaultsToUnlimitedWithoutLoopContext(t *testing.T) {
	t.Parallel()

	assert.Zero(t, agentLoopToolOutputLimit(context.Background()))
}

func TestAgentLoopBudgetFromConfig(t *testing.T) {
	t.Parallel()

	budget, err := agentLoopBudgetFromConfig(appconfig.Config{})
	require.NoError(t, err)
	assert.Zero(t, budget.MaxOutputBytes)
	assert.Zero(t, budget.MaxCostMicros)
	assert.Zero(t, budget.MaxInputTokens)
	assert.Zero(t, budget.MaxOutputTokens)
	assert.Zero(t, budget.MaxTotalTokens)
	assert.Zero(t, budget.MaxIterations)
	assert.Zero(t, budget.MaxModelCalls)
	assert.Zero(t, budget.MaxToolCalls)
	assert.Zero(t, budget.MaxWallTime, "MaxWallTime defaults to unlimited (zero)")

	zeroBytes := int64(0)
	zeroCost := int64(0)
	zeroTokens := 0
	zeroIterations := 0
	zeroCalls := 0
	emptyWallTime := ""
	budget, err = agentLoopBudgetFromConfig(appconfig.Config{
		AgentLoop: appconfig.AgentLoopConfig{
			MaxOutputBytes:  &zeroBytes,
			MaxCostMicros:   &zeroCost,
			MaxInputTokens:  &zeroTokens,
			MaxOutputTokens: &zeroTokens,
			MaxTotalTokens:  &zeroTokens,
			MaxIterations:   &zeroIterations,
			MaxModelCalls:   &zeroCalls,
			MaxToolCalls:    &zeroCalls,
			MaxWallTime:     &emptyWallTime,
		},
	})
	require.NoError(t, err)
	assert.Zero(t, budget.MaxOutputBytes)
	assert.Zero(t, budget.MaxCostMicros)
	assert.Zero(t, budget.MaxInputTokens)
	assert.Zero(t, budget.MaxOutputTokens)
	assert.Zero(t, budget.MaxTotalTokens)
	assert.Zero(t, budget.MaxIterations)
	assert.Zero(t, budget.MaxModelCalls)
	assert.Zero(t, budget.MaxToolCalls)
	assert.Zero(t, budget.MaxWallTime)

	byteLimit := int64(4096)
	costLimit := int64(250_000)
	tokenLimit := 200000
	inputTokenLimit := 120000
	outputTokenLimit := 80000
	iterationLimit := 50
	modelCallLimit := 12
	toolCallLimit := 34
	wallTime := "1h30m"
	budget, err = agentLoopBudgetFromConfig(appconfig.Config{
		AgentLoop: appconfig.AgentLoopConfig{
			MaxOutputBytes:  &byteLimit,
			MaxCostMicros:   &costLimit,
			MaxInputTokens:  &inputTokenLimit,
			MaxOutputTokens: &outputTokenLimit,
			MaxTotalTokens:  &tokenLimit,
			MaxIterations:   &iterationLimit,
			MaxModelCalls:   &modelCallLimit,
			MaxToolCalls:    &toolCallLimit,
			MaxWallTime:     &wallTime,
		},
	})
	require.NoError(t, err)
	assert.Equal(t, byteLimit, budget.MaxOutputBytes)
	assert.Equal(t, costLimit, budget.MaxCostMicros)
	assert.Equal(t, inputTokenLimit, budget.MaxInputTokens)
	assert.Equal(t, outputTokenLimit, budget.MaxOutputTokens)
	assert.Equal(t, tokenLimit, budget.MaxTotalTokens)
	assert.Equal(t, iterationLimit, budget.MaxIterations)
	assert.Equal(t, modelCallLimit, budget.MaxModelCalls)
	assert.Equal(t, toolCallLimit, budget.MaxToolCalls)
	assert.Equal(t, 90*time.Minute, budget.MaxWallTime)

	negativeBytes := int64(-1)
	_, err = agentLoopBudgetFromConfig(appconfig.Config{
		AgentLoop: appconfig.AgentLoopConfig{MaxOutputBytes: &negativeBytes},
	})
	require.ErrorContains(t, err, "agent_loop.max_output_bytes must be >= 0")

	negativeTokens := -1
	negativeCost := int64(-1)
	_, err = agentLoopBudgetFromConfig(appconfig.Config{
		AgentLoop: appconfig.AgentLoopConfig{MaxCostMicros: &negativeCost},
	})
	require.ErrorContains(t, err, "agent_loop.max_cost_micros must be >= 0")

	_, err = agentLoopBudgetFromConfig(appconfig.Config{
		AgentLoop: appconfig.AgentLoopConfig{MaxInputTokens: &negativeTokens},
	})
	require.ErrorContains(t, err, "agent_loop.max_input_tokens must be >= 0")

	_, err = agentLoopBudgetFromConfig(appconfig.Config{
		AgentLoop: appconfig.AgentLoopConfig{MaxOutputTokens: &negativeTokens},
	})
	require.ErrorContains(t, err, "agent_loop.max_output_tokens must be >= 0")

	_, err = agentLoopBudgetFromConfig(appconfig.Config{
		AgentLoop: appconfig.AgentLoopConfig{MaxTotalTokens: &negativeTokens},
	})
	require.ErrorContains(t, err, "agent_loop.max_total_tokens must be >= 0")

	negativeIterations := -1
	_, err = agentLoopBudgetFromConfig(appconfig.Config{
		AgentLoop: appconfig.AgentLoopConfig{MaxIterations: &negativeIterations},
	})
	require.ErrorContains(t, err, "agent_loop.max_iterations must be >= 0")

	negativeCalls := -1
	_, err = agentLoopBudgetFromConfig(appconfig.Config{
		AgentLoop: appconfig.AgentLoopConfig{MaxModelCalls: &negativeCalls},
	})
	require.ErrorContains(t, err, "agent_loop.max_model_calls must be >= 0")

	_, err = agentLoopBudgetFromConfig(appconfig.Config{
		AgentLoop: appconfig.AgentLoopConfig{MaxToolCalls: &negativeCalls},
	})
	require.ErrorContains(t, err, "agent_loop.max_tool_calls must be >= 0")

	negativeWallTime := "-5m"
	_, err = agentLoopBudgetFromConfig(appconfig.Config{
		AgentLoop: appconfig.AgentLoopConfig{MaxWallTime: &negativeWallTime},
	})
	require.ErrorContains(t, err, "agent_loop.max_wall_time must be >= 0")

	invalidWallTime := "not-a-duration"
	_, err = agentLoopBudgetFromConfig(appconfig.Config{
		AgentLoop: appconfig.AgentLoopConfig{MaxWallTime: &invalidWallTime},
	})
	require.ErrorContains(t, err, "agent_loop.max_wall_time")
}

func TestAgentLoopConfigExposesEveryBudgetField(t *testing.T) {
	t.Parallel()

	configFields := tagFieldsForType[appconfig.AgentLoopConfig]("yaml")
	budgetFields := tagFieldsForType[llm.AgentLoopBudget]("json")
	require.NotEmpty(t, budgetFields)

	for field := range budgetFields {
		assert.Containsf(t, configFields, field, "AgentLoopConfig should expose llm.AgentLoopBudget field %s", field)
	}
}

func TestAgentLoopCheckpointIntervalFromConfig(t *testing.T) {
	t.Parallel()

	interval, err := agentLoopCheckpointIntervalFromConfig(appconfig.Config{})
	require.NoError(t, err)
	assert.Zero(t, interval, "default checkpoint interval is zero — no continuation prompt")

	custom := 10
	interval, err = agentLoopCheckpointIntervalFromConfig(appconfig.Config{
		AgentLoop: appconfig.AgentLoopConfig{CheckpointInterval: &custom},
	})
	require.NoError(t, err)
	assert.Equal(t, 10, interval)

	negative := -1
	_, err = agentLoopCheckpointIntervalFromConfig(appconfig.Config{
		AgentLoop: appconfig.AgentLoopConfig{CheckpointInterval: &negative},
	})
	require.ErrorContains(t, err, "agent_loop.checkpoint_interval must be >= 0")
}

func tagFieldsForType[T any](tagName string) map[string]bool {
	fields := make(map[string]bool)
	typ := reflect.TypeFor[T]()

	for field := range typ.Fields() {
		name, _, _ := strings.Cut(field.Tag.Get(tagName), ",")
		if name == "" || name == "-" {
			continue
		}

		fields[name] = true
	}

	return fields
}

func TestAgentLoopBudgetEventMetadataIncludesEveryCeiling(t *testing.T) {
	t.Parallel()

	budget := llm.AgentLoopBudget{
		MaxWallTime:     45 * time.Second,
		MaxOutputBytes:  4096,
		MaxCostMicros:   25_000,
		MaxIterations:   3,
		MaxModelCalls:   4,
		MaxToolCalls:    5,
		MaxInputTokens:  100,
		MaxOutputTokens: 50,
		MaxTotalTokens:  150,
	}

	metadata := agentLoopBudgetEventMetadata(budget)
	require.NotNil(t, metadata)
	require.Contains(t, metadata, "agent_loop_budget")

	var decoded llm.AgentLoopBudget
	require.NoError(t, json.Unmarshal([]byte(metadata["agent_loop_budget"]), &decoded))
	assert.Equal(t, budget, decoded)
}

func TestFormatAgentLoopBudgetCompactIncludesEveryCeiling(t *testing.T) {
	t.Parallel()

	summary := formatAgentLoopBudgetCompact(llm.AgentLoopBudget{
		MaxWallTime:     30 * time.Second,
		MaxOutputBytes:  4096,
		MaxCostMicros:   25_000,
		MaxIterations:   3,
		MaxModelCalls:   4,
		MaxToolCalls:    5,
		MaxInputTokens:  100,
		MaxOutputTokens: 50,
		MaxTotalTokens:  150,
	})

	for _, want := range []string{
		"iter=3",
		"model=4",
		"tool=5",
		"wall=30s",
		"in=100",
		"out=50",
		"total=150",
		"bytes=4096",
		"costµ=25000",
	} {
		assert.Contains(t, summary, want)
	}
}

func TestAgentLoopConfirmCallbacksSendToolConfirmationToTUI(t *testing.T) {
	t.Parallel()

	requestCh := make(chan agentLoopConfirmRequest, 1)
	responseCh := make(chan bool, 1)

	_, confirmTool := agentLoopConfirmCallbacks(t.Context(), llmRequest{
		confirmRequestCh:  requestCh,
		confirmResponseCh: responseCh,
	})
	require.NotNil(t, confirmTool)

	done := make(chan bool, 1)

	go func() {
		done <- confirmTool(t.Context(), llm.ToolCall{
			ID:    "call_1",
			Name:  "bash",
			Input: map[string]any{"command": "sudo make install"},
		}, llm.ToolPolicyDecision{
			Verdict:     llm.ToolPolicyRequireConfirm,
			Reason:      "privileged command requires confirmation",
			MatchedRule: "bash.confirm.privileged",
		})
	}()

	select {
	case req := <-requestCh:
		assert.Equal(t, agentLoopConfirmToolCall, req.kind)
		assert.Contains(t, req.prompt, "bash.confirm.privileged")
		assert.Contains(t, req.prompt, "sudo make install")
	case <-time.After(time.Second):
		require.Fail(t, "timed out waiting for tool confirmation request")
	}

	responseCh <- true

	select {
	case ok := <-done:
		assert.True(t, ok)
	case <-time.After(time.Second):
		require.Fail(t, "timed out waiting for tool confirmation response")
	}
}
