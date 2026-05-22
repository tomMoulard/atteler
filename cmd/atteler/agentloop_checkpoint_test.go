package main

import (
	"context"
	"errors"
	"path/filepath"
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
	assert.Zero(t, budget.MaxTotalTokens)
	assert.Zero(t, budget.MaxIterations)
	assert.Zero(t, budget.MaxWallTime, "MaxWallTime defaults to unlimited (zero)")

	zeroBytes := int64(0)
	zeroTokens := 0
	zeroIterations := 0
	emptyWallTime := ""
	budget, err = agentLoopBudgetFromConfig(appconfig.Config{
		AgentLoop: appconfig.AgentLoopConfig{
			MaxOutputBytes: &zeroBytes,
			MaxTotalTokens: &zeroTokens,
			MaxIterations:  &zeroIterations,
			MaxWallTime:    &emptyWallTime,
		},
	})
	require.NoError(t, err)
	assert.Zero(t, budget.MaxOutputBytes)
	assert.Zero(t, budget.MaxTotalTokens)
	assert.Zero(t, budget.MaxIterations)
	assert.Zero(t, budget.MaxWallTime)

	byteLimit := int64(4096)
	tokenLimit := 200000
	iterationLimit := 50
	wallTime := "1h30m"
	budget, err = agentLoopBudgetFromConfig(appconfig.Config{
		AgentLoop: appconfig.AgentLoopConfig{
			MaxOutputBytes: &byteLimit,
			MaxTotalTokens: &tokenLimit,
			MaxIterations:  &iterationLimit,
			MaxWallTime:    &wallTime,
		},
	})
	require.NoError(t, err)
	assert.Equal(t, byteLimit, budget.MaxOutputBytes)
	assert.Equal(t, tokenLimit, budget.MaxTotalTokens)
	assert.Equal(t, iterationLimit, budget.MaxIterations)
	assert.Equal(t, 90*time.Minute, budget.MaxWallTime)

	negativeBytes := int64(-1)
	_, err = agentLoopBudgetFromConfig(appconfig.Config{
		AgentLoop: appconfig.AgentLoopConfig{MaxOutputBytes: &negativeBytes},
	})
	require.ErrorContains(t, err, "agent_loop.max_output_bytes must be >= 0")

	negativeTokens := -1
	_, err = agentLoopBudgetFromConfig(appconfig.Config{
		AgentLoop: appconfig.AgentLoopConfig{MaxTotalTokens: &negativeTokens},
	})
	require.ErrorContains(t, err, "agent_loop.max_total_tokens must be >= 0")

	negativeIterations := -1
	_, err = agentLoopBudgetFromConfig(appconfig.Config{
		AgentLoop: appconfig.AgentLoopConfig{MaxIterations: &negativeIterations},
	})
	require.ErrorContains(t, err, "agent_loop.max_iterations must be >= 0")

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
