package main

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

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
