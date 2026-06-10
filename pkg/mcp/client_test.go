//nolint:goconst,misspell,paralleltest,wsl_v5 // Tests use literal MCP protocol methods and serialize process-heavy helpers to avoid fork-limit flakes.
package mcp

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/tommoulard/atteler/pkg/permission"
	"github.com/tommoulard/atteler/pkg/shell"
)

const (
	helperTimeout     = 5 * time.Second
	helperSecretToken = "mcp-secret-token"

	// largeHelperPayloadBytes keeps the response line just under the 4 MiB
	// message cap while staying well above the legacy 1 MiB scanner limit.
	largeHelperPayloadBytes = maxIncomingMessageBytes - 4096

	// oversizedHelperPayloadBytes pushes the response line over the 4 MiB cap.
	oversizedHelperPayloadBytes = maxIncomingMessageBytes + 4096
)

func TestInvoke_PerformsLifecycleAndReadsResponse(t *testing.T) {
	server := helperServer(t, "echo")

	response, err := Invoke(t.Context(), server, Request{
		ID:     "req-1",
		Method: "echo",
		Params: map[string]any{"message": "hello"},
	}, helperTimeout)

	require.NoError(t, err)
	assert.Equal(t, "2.0", response.JSONRPC)
	assert.Equal(t, "req-1", response.ID)

	var result map[string]any
	require.NoError(t, json.Unmarshal(response.Result, &result))
	assert.Equal(t, "echo", result["method"])
	assert.Equal(t, map[string]any{"message": "hello"}, result["params"])
	assert.Equal(t, "from-test", result["env"])
	assert.Equal(t, realPath(t, server.CWD), result["cwd"])
	assert.Equal(t, true, result["initialized"], "request should run after initialized notification")
}

func TestInvoke_ReadsFastExitingLifecycleServerResponse(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("shell helper uses POSIX sh")
	}

	helper := filepath.Join(t.TempDir(), "mcp-helper")
	script := "#!/bin/sh\n" +
		"read init\n" +
		"printf '{\"jsonrpc\":\"2.0\",\"id\":1,\"result\":{\"protocolVersion\":\"2025-11-25\",\"capabilities\":{},\"serverInfo\":{\"name\":\"fast\",\"version\":\"1.0.0\"}}}\n'\n" +
		"read initialized\n" +
		"read line\n" +
		"printf '{\"jsonrpc\":\"2.0\",\"id\":2,\"result\":{\"ok\":true,\"source\":\"fast-helper\"}}\n'\n"
	require.NoError(t, os.WriteFile(helper, []byte(script), 0o600))

	response, err := Invoke(t.Context(), Server{Name: "helper", Command: "sh", Args: []string{helper}}, Request{
		ID:     2,
		Method: "ping",
	}, helperTimeout)

	require.NoError(t, err)
	assert.JSONEq(t, `{"ok":true,"source":"fast-helper"}`, string(response.Result))
}

func TestInvoke_PreservesLargeNumericRequestIDs(t *testing.T) {
	server := helperServer(t, "echo")
	id := json.Number("9007199254740993")

	response, err := Invoke(t.Context(), server, Request{
		ID:     id,
		Method: "echo",
	}, helperTimeout)

	require.NoError(t, err)
	assert.Equal(t, id, response.ID)

	var result map[string]any
	require.NoError(t, json.Unmarshal(response.Result, &result))
	assert.Equal(t, "echo", result["method"])
	assert.Equal(t, true, result["initialized"])
}

func TestSession_ReusesProcessDiscoversToolsAndValidatesSchema(t *testing.T) {
	session := NewSession(helperServer(t, "tools"), SessionOptions{})
	require.NoError(t, session.Start(t.Context()))
	defer func() { require.NoError(t, session.Close(context.WithoutCancel(t.Context()))) }()

	tools := session.Tools()
	require.Len(t, tools, 1)
	assert.Equal(t, "search", tools[0].Name)

	_, err := session.CallTool(t.Context(), "search", map[string]any{})
	require.Error(t, err)
	assert.Contains(t, err.Error(), `missing required argument "query"`)

	_, err = session.CallTool(t.Context(), "search", map[string]any{"query": 42})
	require.Error(t, err)
	assert.Contains(t, err.Error(), `argument "query" has type number, want string`)

	_, err = session.CallTool(t.Context(), "search", map[string]any{"query": "mcp", "extra": true})
	require.Error(t, err)
	assert.Contains(t, err.Error(), `unexpected argument "extra"`)

	_, err = session.CallTool(t.Context(), "unknown", map[string]any{"query": "mcp"})
	require.Error(t, err)
	assert.Contains(t, err.Error(), `tool was not discovered by tools/list`)

	response, err := session.CallTool(t.Context(), "search", map[string]any{"query": "mcp"})
	require.NoError(t, err)

	var result callToolResult
	require.NoError(t, json.Unmarshal(response.Result, &result))
	assert.Equal(t, "tools/call", result.Method)
	assert.Equal(t, "search", result.Params.Name)
	assert.Equal(t, map[string]any{"query": "mcp"}, result.Params.Arguments)
	assert.Equal(t, 1, result.Count)

	response, err = session.Invoke(t.Context(), Request{Method: "ping"})
	require.NoError(t, err)
	assert.JSONEq(t, `{}`, string(response.Result))

	health := session.Health(t.Context())
	assert.True(t, health.Healthy)
	assert.True(t, health.Running)
	assert.True(t, health.Initialized)
}

func TestSession_ToolCallUsesCentralPermissionPolicy(t *testing.T) {
	session := NewSession(helperServer(t, "tools"), SessionOptions{})
	require.NoError(t, session.Start(t.Context()))
	defer func() { require.NoError(t, session.Close(context.WithoutCancel(t.Context()))) }()

	policy := permission.DefaultPolicy()
	policy.SetMode(permission.OperationExecute, permission.ModeDeny)
	ctx := permission.ContextWithPolicy(t.Context(), &policy)

	response, err := session.CallTool(ctx, "search", map[string]any{"query": "blocked"})

	require.Error(t, err)
	assert.Nil(t, response)
	assert.True(t, permission.ErrDenied(err))
	assert.Contains(t, err.Error(), `execute operation "mcp tool helper/search" is denied by permission policy`)

	response, err = session.CallTool(t.Context(), "search", map[string]any{"query": "allowed"})
	require.NoError(t, err)

	var result callToolResult
	require.NoError(t, json.Unmarshal(response.Result, &result))
	assert.Equal(t, 1, result.Count, "denied tool call should not be sent to the server")
	assert.Equal(t, "allowed", result.Params.Arguments["query"])
}

func TestSession_ValidatesCompositeToolArgumentSchemas(t *testing.T) {
	session := NewSession(helperServer(t, "complex-tool"), SessionOptions{})
	require.NoError(t, session.Start(t.Context()))
	defer func() { require.NoError(t, session.Close(context.WithoutCancel(t.Context()))) }()

	response, err := session.CallTool(t.Context(), "complex", map[string]any{
		"tags":    []string{"mcp", "lifecycle"},
		"filters": map[string]string{"kind": "tool"},
	})
	require.NoError(t, err)

	var result callToolResult
	require.NoError(t, json.Unmarshal(response.Result, &result))
	assert.Equal(t, "complex", result.Params.Name)
	assert.Equal(t, []any{"mcp", "lifecycle"}, result.Params.Arguments["tags"])
	assert.Equal(t, map[string]any{"kind": "tool"}, result.Params.Arguments["filters"])

	_, err = session.CallTool(t.Context(), "complex", map[string]any{
		"tags":    "not-array",
		"filters": map[string]string{"kind": "tool"},
	})
	require.Error(t, err)
	assert.Contains(t, err.Error(), `argument "tags" has type string, want array`)
}

func TestSession_PreservesLargeNumericToolArguments(t *testing.T) {
	session := NewSession(helperServer(t, "number-tool"), SessionOptions{})
	require.NoError(t, session.Start(t.Context()))
	defer func() { require.NoError(t, session.Close(context.WithoutCancel(t.Context()))) }()

	large := json.Number("9007199254740993")
	response, err := session.CallTool(t.Context(), "number", map[string]any{"n": large})
	require.NoError(t, err)

	var result callToolResult
	require.NoError(t, decodeJSONUseNumber(response.Result, &result))
	assert.Equal(t, large, result.Params.Arguments["n"])
}

func TestCallTool_UsesDiscoveredToolsCallMethod(t *testing.T) {
	server := helperServer(t, "tools")

	response, err := CallTool(t.Context(), server, "search", map[string]any{"query": "mcp"}, helperTimeout)

	require.NoError(t, err)

	var result callToolResult
	require.NoError(t, json.Unmarshal(response.Result, &result))
	assert.Equal(t, "tools/call", result.Method)
	assert.Equal(t, "search", result.Params.Name)
	assert.Equal(t, map[string]any{"query": "mcp"}, result.Params.Arguments)
}

func TestSession_LongRunningToolCanBeReused(t *testing.T) {
	session := NewSession(helperServer(t, "slow-tool"), SessionOptions{})
	require.NoError(t, session.Start(t.Context()))
	defer func() { require.NoError(t, session.Close(context.WithoutCancel(t.Context()))) }()

	response, err := session.CallTool(t.Context(), "slow", map[string]any{"delay_ms": 40})
	require.NoError(t, err)
	assert.JSONEq(t, `{"count":1,"slept":true}`, string(response.Result))

	response, err = session.CallTool(t.Context(), "slow", map[string]any{"delay_ms": 1})
	require.NoError(t, err)
	assert.JSONEq(t, `{"count":2,"slept":true}`, string(response.Result))
}

func TestSession_MatchesOutOfOrderConcurrentResponses(t *testing.T) {
	session := NewSession(helperServer(t, "out-of-order-tool"), SessionOptions{})
	require.NoError(t, session.Start(t.Context()))
	defer func() { require.NoError(t, session.Close(context.WithoutCancel(t.Context()))) }()

	firstCh := make(chan callResult, 1)
	secondCh := make(chan callResult, 1)

	go func() {
		response, err := session.CallTool(t.Context(), "slow", map[string]any{"delay_ms": 120})
		firstCh <- callResult{response: response, err: err}
	}()
	go func() {
		response, err := session.CallTool(t.Context(), "slow", map[string]any{"delay_ms": 5})
		secondCh <- callResult{response: response, err: err}
	}()

	first := receiveCallResult(t, firstCh)
	second := receiveCallResult(t, secondCh)

	require.NoError(t, first.err)
	require.NoError(t, second.err)

	var firstResult, secondResult struct {
		Slept   bool `json:"slept"`
		DelayMS int  `json:"delay_ms"`
		Count   int  `json:"count"`
	}
	require.NoError(t, json.Unmarshal(first.response.Result, &firstResult))
	require.NoError(t, json.Unmarshal(second.response.Result, &secondResult))
	assert.True(t, firstResult.Slept)
	assert.True(t, secondResult.Slept)
	assert.Equal(t, 120, firstResult.DelayMS)
	assert.Equal(t, 5, secondResult.DelayMS)
	assert.ElementsMatch(t, []int{1, 2}, []int{firstResult.Count, secondResult.Count})
}

func TestSession_CancelsLongRunningToolAndSendsNotification(t *testing.T) {
	server := helperServer(t, "slow-tool")
	cancelPath := filepath.Join(server.CWD, "cancelled.txt")
	session := NewSession(server, SessionOptions{})
	require.NoError(t, session.Start(t.Context()))
	defer func() { require.NoError(t, session.Close(context.WithoutCancel(t.Context()))) }()

	ctx, cancel := context.WithTimeout(t.Context(), 25*time.Millisecond)
	defer cancel()

	response, err := session.CallTool(ctx, "slow", map[string]any{"delay_ms": 500})

	require.Error(t, err)
	assert.Nil(t, response)
	require.ErrorIs(t, err, context.DeadlineExceeded)
	assert.Eventually(t, func() bool {
		data, readErr := os.ReadFile(cancelPath)
		return readErr == nil && strings.Contains(string(data), "context deadline exceeded")
	}, time.Second, 10*time.Millisecond)
}

func TestSession_RetiresCanceledRequestIDUntilStaleResponseIsDiscarded(t *testing.T) {
	session := NewSession(helperServer(t, "slow-tool"), SessionOptions{})
	require.NoError(t, session.Start(t.Context()))
	defer func() { require.NoError(t, session.Close(context.WithoutCancel(t.Context()))) }()

	request := Request{
		ID:     "caller-id",
		Method: "tools/call",
		Params: CallToolParams{
			Name:      "slow",
			Arguments: map[string]any{"delay_ms": 500},
		},
	}

	ctx, cancel := context.WithTimeout(t.Context(), 25*time.Millisecond)
	defer cancel()
	_, err := session.Invoke(ctx, request)
	require.ErrorIs(t, err, context.DeadlineExceeded)

	_, err = session.Invoke(t.Context(), request)
	require.Error(t, err)
	assert.Contains(t, err.Error(), `mcp request id "caller-id" was canceled`)
}

func TestCallToolWithOptions_CleansUpAfterCallerCancellation(t *testing.T) {
	server := helperServer(t, "slow-tool-close-marker")
	startedPath := filepath.Join(server.CWD, "slow-started.txt")
	closedPath := filepath.Join(server.CWD, "closed.txt")
	ctx, cancel := context.WithCancel(t.Context())
	resultCh := make(chan callResult, 1)

	go func() {
		response, err := CallToolWithOptions(
			ctx,
			server,
			"slow",
			map[string]any{"delay_ms": 500},
			0,
			SessionOptions{ShutdownTimeout: 500 * time.Millisecond},
		)
		resultCh <- callResult{response: response, err: err}
	}()

	require.Eventually(t, func() bool {
		_, err := os.Stat(startedPath)
		return err == nil
	}, time.Second, 10*time.Millisecond)

	cancel()

	var result callResult
	select {
	case result = <-resultCh:
	case <-time.After(2 * time.Second):
		require.FailNow(t, "timed out waiting for canceled MCP tool call")
	}
	require.Error(t, result.err)
	require.ErrorIs(t, result.err, context.Canceled)
	assert.Nil(t, result.response)

	assert.Eventually(t, func() bool {
		data, err := os.ReadFile(closedPath)
		return err == nil && string(data) == "stdin closed"
	}, time.Second, 10*time.Millisecond)
}

func TestSession_StartContextCancellationDoesNotKillReusableSession(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	session := NewSession(helperServer(t, "tools"), SessionOptions{})
	require.NoError(t, session.Start(ctx))
	defer func() { require.NoError(t, session.Close(context.WithoutCancel(t.Context()))) }()

	cancel()

	response, err := session.CallTool(t.Context(), "search", map[string]any{"query": "still-running"})
	require.NoError(t, err)

	var result callToolResult
	require.NoError(t, json.Unmarshal(response.Result, &result))
	assert.Equal(t, "still-running", result.Params.Arguments["query"])
}

func TestSession_RejectsCallsAfterServerExits(t *testing.T) {
	session := NewSession(helperServer(t, "exit-after-init"), SessionOptions{})
	require.NoError(t, session.Start(t.Context()))
	defer func() { require.NoError(t, session.Close(context.WithoutCancel(t.Context()))) }()

	require.Eventually(t, func() bool {
		return !session.running()
	}, 5*time.Second, 10*time.Millisecond)

	_, err := session.Invoke(t.Context(), Request{Method: "ping"})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "session is not running")

	err = session.Start(t.Context())
	require.Error(t, err)
	assert.Contains(t, err.Error(), "session is not running")
}

func TestSession_ListsResourcesAndPromptsWhenCapabilitiesAreNegotiated(t *testing.T) {
	session := NewSession(helperServer(t, "catalog"), SessionOptions{})
	require.NoError(t, session.Start(t.Context()))
	defer func() { require.NoError(t, session.Close(context.WithoutCancel(t.Context()))) }()

	resources, err := session.ListResources(t.Context())
	require.NoError(t, err)
	require.Len(t, resources, 1)
	assert.Equal(t, "file:///repo/README.md", resources[0].URI)
	assert.Equal(t, "README", resources[0].Name)
	assert.Equal(t, resources, session.Resources())

	prompts, err := session.ListPrompts(t.Context())
	require.NoError(t, err)
	require.Len(t, prompts, 1)
	assert.Equal(t, "summarize", prompts[0].Name)
	require.Len(t, prompts[0].Arguments, 1)
	assert.Equal(t, "topic", prompts[0].Arguments[0].Name)
	assert.True(t, prompts[0].Arguments[0].Required)
	assert.Equal(t, prompts, session.Prompts())
}

func TestSession_RejectsProtocolMethodsWithoutNegotiatedCapability(t *testing.T) {
	session := NewSession(helperServer(t, "echo"), SessionOptions{})
	require.NoError(t, session.Start(t.Context()))
	defer func() { require.NoError(t, session.Close(context.WithoutCancel(t.Context()))) }()

	_, err := session.ListResources(t.Context())
	require.Error(t, err)
	assert.Contains(t, err.Error(), `does not advertise "resources" capability`)

	_, err = session.ListPrompts(t.Context())
	require.Error(t, err)
	assert.Contains(t, err.Error(), `does not advertise "prompts" capability`)

	for _, tc := range []struct {
		method     string
		capability string
	}{
		{method: "logging/setLevel", capability: "logging"},
		{method: "completion/complete", capability: "completions"},
		{method: "tasks/list", capability: "tasks"},
	} {
		_, err = session.Invoke(t.Context(), Request{Method: tc.method})
		require.Error(t, err, tc.method)
		assert.Contains(t, err.Error(), fmt.Sprintf("does not advertise %q capability", tc.capability), tc.method)
	}
}

func TestSession_AllowsNegotiatedUtilityCapabilities(t *testing.T) {
	session := NewSession(helperServer(t, "utilities"), SessionOptions{})
	require.NoError(t, session.Start(t.Context()))
	defer func() { require.NoError(t, session.Close(context.WithoutCancel(t.Context()))) }()

	for _, method := range []string{"logging/setLevel", "completion/complete", "tasks/list"} {
		response, err := session.Invoke(t.Context(), Request{Method: method})
		require.NoError(t, err, method)

		var result map[string]any
		require.NoError(t, json.Unmarshal(response.Result, &result), method)
		assert.Equal(t, method, result["method"], method)
	}
}

func TestSession_RejectsCallerManagedLifecycleMethods(t *testing.T) {
	session := NewSession(helperServer(t, "echo"), SessionOptions{})
	require.NoError(t, session.Start(t.Context()))
	defer func() { require.NoError(t, session.Close(context.WithoutCancel(t.Context()))) }()

	_, err := session.Invoke(t.Context(), Request{Method: "initialize"})
	require.Error(t, err)
	assert.Contains(t, err.Error(), `lifecycle method "initialize" is managed by the session`)

	_, err = session.Invoke(t.Context(), Request{Method: "notifications/initialized"})
	require.Error(t, err)
	assert.Contains(t, err.Error(), `lifecycle method "notifications/initialized" is managed by the session`)

	_, err = session.Invoke(t.Context(), Request{Method: "notifications/cancelled"})
	require.Error(t, err)
	assert.Contains(t, err.Error(), `notification method "notifications/cancelled" cannot be invoked as a request`)
}

func TestSession_RejectsClientSideProtocolMethods(t *testing.T) {
	session := NewSession(helperServer(t, "echo"), SessionOptions{})
	require.NoError(t, session.Start(t.Context()))
	defer func() { require.NoError(t, session.Close(context.WithoutCancel(t.Context()))) }()

	for _, method := range []string{"roots/list", "sampling/createMessage", "elicitation/create"} {
		_, err := session.Invoke(t.Context(), Request{Method: method})
		require.Error(t, err, method)
		assert.Contains(t, err.Error(), fmt.Sprintf("client-side method %q cannot be invoked against a server", method), method)
	}
}

func TestSession_HandlesServerPingDuringInitialize(t *testing.T) {
	server := helperServer(t, "ping-before-init")
	pongPath := filepath.Join(server.CWD, "client-pong.txt")
	session := NewSession(server, SessionOptions{})
	require.NoError(t, session.Start(t.Context()))
	defer func() { require.NoError(t, session.Close(context.WithoutCancel(t.Context()))) }()

	assert.Eventually(t, func() bool {
		data, err := os.ReadFile(pongPath)
		return err == nil && string(data) == "pong"
	}, time.Second, 10*time.Millisecond)
}

func TestSession_RejectsUnsupportedServerRequestsDuringInitialize(t *testing.T) {
	server := helperServer(t, "unsupported-server-request-before-init")
	rejectedPath := filepath.Join(server.CWD, "unsupported-server-request.txt")
	session := NewSession(server, SessionOptions{})
	require.NoError(t, session.Start(t.Context()))
	defer func() { require.NoError(t, session.Close(context.WithoutCancel(t.Context()))) }()

	assert.Eventually(t, func() bool {
		data, err := os.ReadFile(rejectedPath)
		return err == nil && string(data) == "method not found"
	}, time.Second, 10*time.Millisecond)
}

func TestSession_InitializeFailureIncludesRPCError(t *testing.T) {
	session := NewSession(helperServer(t, "init-error"), SessionOptions{})

	err := session.Start(t.Context())

	require.Error(t, err)
	assert.Contains(t, err.Error(), `mcp server "helper" returned error -32000: init boom`)
}

func TestSession_RejectsInitializeResultMissingCapabilities(t *testing.T) {
	session := NewSession(helperServer(t, "missing-init-capabilities"), SessionOptions{})

	err := session.Start(t.Context())

	require.Error(t, err)
	assert.Contains(t, err.Error(), "initialize result missing capabilities")
}

func TestSession_RejectsUnsupportedNegotiatedProtocolVersion(t *testing.T) {
	session := NewSession(helperServer(t, "unsupported-protocol"), SessionOptions{})

	err := session.Start(t.Context())

	require.Error(t, err)
	assert.Contains(t, err.Error(), `unsupported protocol version "1900-01-01"`)
}

func TestSession_MalformedServerResponseFailsInitialize(t *testing.T) {
	session := NewSession(helperServer(t, "malformed"), SessionOptions{})

	err := session.Start(t.Context())

	require.Error(t, err)
	assert.Contains(t, err.Error(), "unexpected EOF")
	assert.Contains(t, err.Error(), "skipped malformed mcp stdout line")
}

func TestSession_SkipsMalformedStdoutLineAndKeepsTransport(t *testing.T) {
	session := NewSession(helperServer(t, "malformed-operation"), SessionOptions{})
	require.NoError(t, session.Start(t.Context()))
	defer func() { require.NoError(t, session.Close(context.WithoutCancel(t.Context()))) }()

	response, err := session.Invoke(t.Context(), Request{Method: "broken"})

	require.NoError(t, err, "a junk stdout line before a valid response must be skipped")
	require.NotNil(t, response)

	var result map[string]any
	require.NoError(t, json.Unmarshal(response.Result, &result))
	assert.Equal(t, "broken", result["method"])

	response, err = session.Invoke(t.Context(), Request{Method: "ping"})
	require.NoError(t, err, "session must remain usable after a malformed stdout line")
	assert.JSONEq(t, `{}`, string(response.Result))

	health := session.Health(t.Context())
	assert.True(t, health.Healthy)
	assert.Contains(t, session.Stderr(), "skipped malformed mcp stdout line")
}

func TestSession_ReadsResponsesLargerThanLegacyOneMiBCap(t *testing.T) {
	session := NewSession(helperServer(t, "large-response"), SessionOptions{})
	require.NoError(t, session.Start(t.Context()))
	defer func() { require.NoError(t, session.Close(context.WithoutCancel(t.Context()))) }()

	response, err := session.Invoke(t.Context(), Request{Method: "large"})

	require.NoError(t, err)

	var result struct {
		Payload string `json:"payload"`
	}
	require.NoError(t, json.Unmarshal(response.Result, &result))
	assert.Len(t, result.Payload, largeHelperPayloadBytes)
}

func TestSession_SurvivesOversizedStdoutMessage(t *testing.T) {
	session := NewSession(helperServer(t, "oversized-response"), SessionOptions{})
	require.NoError(t, session.Start(t.Context()))
	defer func() { require.NoError(t, session.Close(context.WithoutCancel(t.Context()))) }()

	response, err := session.Invoke(t.Context(), Request{Method: "huge"})

	require.Error(t, err)
	assert.Nil(t, response)
	assert.Contains(t, err.Error(), "larger than")
	assert.Contains(t, err.Error(), "the message was discarded")

	response, err = session.Invoke(t.Context(), Request{Method: "ping"})
	require.NoError(t, err, "session must remain usable after an oversized message is discarded")
	assert.JSONEq(t, `{}`, string(response.Result))

	health := session.Health(t.Context())
	assert.True(t, health.Healthy)
}

func TestSession_RejectsResponseWithMethodField(t *testing.T) {
	session := NewSession(helperServer(t, "response-with-method"), SessionOptions{})
	require.NoError(t, session.Start(t.Context()))
	defer func() { require.NoError(t, session.Close(context.WithoutCancel(t.Context()))) }()

	response, err := session.Invoke(t.Context(), Request{Method: "hybrid"})

	require.Error(t, err)
	assert.Nil(t, response)
	assert.Contains(t, err.Error(), `method "unexpected/serverMethod" cannot appear on a response`)
}

func TestSession_RejectsMalformedErrorObject(t *testing.T) {
	session := NewSession(helperServer(t, "malformed-error-object"), SessionOptions{})
	require.NoError(t, session.Start(t.Context()))
	defer func() { require.NoError(t, session.Close(context.WithoutCancel(t.Context()))) }()

	response, err := session.Invoke(t.Context(), Request{Method: "bad-error"})

	require.Error(t, err)
	assert.Nil(t, response)
	assert.Contains(t, err.Error(), "error missing code")
}

func TestSession_RejectsMalformedResponseID(t *testing.T) {
	session := NewSession(helperServer(t, "malformed-response-id"), SessionOptions{})
	require.NoError(t, session.Start(t.Context()))
	defer func() { require.NoError(t, session.Close(context.WithoutCancel(t.Context()))) }()

	response, err := session.Invoke(t.Context(), Request{Method: "bad-id"})

	require.Error(t, err)
	assert.Nil(t, response)
	assert.Contains(t, err.Error(), "invalid id")
	assert.Contains(t, err.Error(), "id must be a string or number")
}

func TestSession_StderrDiagnosticsAreAttached(t *testing.T) {
	session := NewSession(helperServer(t, "stderr-init-error"), SessionOptions{})

	err := session.Start(t.Context())

	require.Error(t, err)
	assert.Contains(t, err.Error(), "stderr: diagnostic before failure")
}

func TestSession_RejectsDeclaredCapabilityMismatch(t *testing.T) {
	server := helperServer(t, "record-initialized")
	server.Capabilities = []string{"tools"}
	initializedPath := filepath.Join(server.CWD, "initialized.txt")
	session := NewSession(server, SessionOptions{})

	err := session.Start(t.Context())

	require.Error(t, err)
	assert.Contains(t, err.Error(), `declares "tools" capability`)

	_, statErr := os.Stat(initializedPath)
	assert.True(t, os.IsNotExist(statErr), "capability mismatch should fail before notifications/initialized")
}

func TestSession_RejectsMalformedToolsList(t *testing.T) {
	session := NewSession(helperServer(t, "malformed-tools"), SessionOptions{})

	err := session.Start(t.Context())

	require.Error(t, err)
	assert.Contains(t, err.Error(), "discover tools")
	assert.Contains(t, err.Error(), "tools/list result missing tools")
}

func TestSession_RejectsMalformedToolInputSchema(t *testing.T) {
	session := NewSession(helperServer(t, "malformed-tool-schema"), SessionOptions{})

	err := session.Start(t.Context())

	require.Error(t, err)
	assert.Contains(t, err.Error(), "discover tools")
	assert.Contains(t, err.Error(), "input schema required must be an array")
}

func TestSession_RejectsMissingToolInputSchema(t *testing.T) {
	session := NewSession(helperServer(t, "missing-tool-schema"), SessionOptions{})

	err := session.Start(t.Context())

	require.Error(t, err)
	assert.Contains(t, err.Error(), "discover tools")
	assert.Contains(t, err.Error(), "inputSchema is required")
}

func TestSession_RejectsDuplicateToolInputSchemaRequiredEntries(t *testing.T) {
	session := NewSession(helperServer(t, "duplicate-required-tool-schema"), SessionOptions{})

	err := session.Start(t.Context())

	require.Error(t, err)
	assert.Contains(t, err.Error(), "discover tools")
	assert.Contains(t, err.Error(), `duplicates "query"`)
}

func TestSession_RejectsRepeatedToolsListCursor(t *testing.T) {
	session := NewSession(helperServer(t, "repeated-tools-cursor"), SessionOptions{})

	err := session.Start(t.Context())

	require.Error(t, err)
	assert.Contains(t, err.Error(), "discover tools")
	assert.Contains(t, err.Error(), `tools/list returned repeated nextCursor "loop"`)
}

func TestSession_RejectsTooManyToolsListPages(t *testing.T) {
	session := NewSession(helperServer(t, "too-many-tools-pages"), SessionOptions{MaxDiscoveryPages: 2})

	err := session.Start(t.Context())

	require.Error(t, err)
	assert.Contains(t, err.Error(), "discover tools")
	assert.Contains(t, err.Error(), "tools/list exceeded discovery page limit 2")
}

func TestSession_RoutesProcessStartThroughShellPolicy(t *testing.T) {
	policy := shell.DefaultPolicy()
	policy.DenyCommands = []string{filepath.Base(os.Args[0])}
	session := NewSession(helperServer(t, "echo"), SessionOptions{Policy: &policy})

	err := session.Start(t.Context())

	require.Error(t, err)
	assert.Contains(t, err.Error(), "command denied by policy")
}

func TestSession_RoutesProcessStartThroughCentralPermissionPolicy(t *testing.T) {
	policy := permission.DefaultPolicy()
	policy.SetMode(permission.OperationExecute, permission.ModeDeny)
	session := NewSession(helperServer(t, "echo"), SessionOptions{Permission: &policy})

	err := session.Start(t.Context())

	require.Error(t, err)
	assert.True(t, permission.ErrDenied(err))
	assert.Contains(t, err.Error(), `execute operation "mcp server helper" is denied by permission policy`)
}

func TestSession_RoutesProcessStartThroughContextPermissionPolicy(t *testing.T) {
	policy := permission.DefaultPolicy()
	policy.SetMode(permission.OperationExecute, permission.ModeDeny)
	ctx := permission.ContextWithPolicy(t.Context(), &policy)
	session := NewSession(helperServer(t, "echo"), SessionOptions{})

	err := session.Start(ctx)

	require.Error(t, err)
	assert.True(t, permission.ErrDenied(err))
	assert.Contains(t, err.Error(), `execute operation "mcp server helper" is denied by permission policy`)
}

func TestSession_RejectsUnsupportedAdvertisedClientCapabilities(t *testing.T) {
	for _, tc := range []struct {
		capabilities ClientCapabilities
		name         string
	}{
		{name: "experimental", capabilities: ClientCapabilities{Experimental: map[string]any{"example": true}}},
		{name: "roots", capabilities: ClientCapabilities{Roots: map[string]any{"listChanged": true}}},
		{name: "sampling", capabilities: ClientCapabilities{Sampling: map[string]any{"supported": true}}},
		{name: "elicitation", capabilities: ClientCapabilities{Elicitation: map[string]any{"supported": true}}},
		{name: "tasks", capabilities: ClientCapabilities{Tasks: map[string]any{"supported": true}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			session := NewSession(helperServer(t, "echo"), SessionOptions{ClientCapabilities: tc.capabilities})

			err := session.Start(t.Context())

			require.Error(t, err)
			assert.Contains(t, err.Error(), fmt.Sprintf("client capability %q is not implemented", tc.name))
		})
	}
}

func TestInvokeWithOptions_RoutesProcessStartThroughShellPolicy(t *testing.T) {
	policy := shell.DefaultPolicy()
	policy.DenyCommands = []string{filepath.Base(os.Args[0])}

	response, err := InvokeWithOptions(
		t.Context(),
		helperServer(t, "echo"),
		Request{Method: "ping"},
		helperTimeout,
		SessionOptions{Policy: &policy},
	)

	require.Error(t, err)
	assert.Nil(t, response)
	assert.Contains(t, err.Error(), "command denied by policy")
}

func TestSession_CloseBeforeStartIsNoop(t *testing.T) {
	session := NewSession(helperServer(t, "echo"), SessionOptions{})

	require.NoError(t, session.Close(t.Context()))
	require.NoError(t, session.Close(t.Context()))
}

func TestSession_CloseSignalsServerShutdownByClosingStdin(t *testing.T) {
	server := helperServer(t, "close-marker")
	session := NewSession(server, SessionOptions{})
	require.NoError(t, session.Start(t.Context()))

	require.NoError(t, session.Close(context.WithoutCancel(t.Context())))

	data, err := os.ReadFile(filepath.Join(server.CWD, "closed.txt"))
	require.NoError(t, err)
	assert.Equal(t, "stdin closed", string(data))
}

func TestSession_CloseUsesTransportShutdownWithoutRPC(t *testing.T) {
	server := helperServer(t, "strict-close")
	session := NewSession(server, SessionOptions{})
	require.NoError(t, session.Start(t.Context()))

	require.NoError(t, session.Close(context.WithoutCancel(t.Context())))

	data, err := os.ReadFile(filepath.Join(server.CWD, "closed.txt"))
	require.NoError(t, err)
	assert.Equal(t, "stdin closed", string(data))

	_, err = os.Stat(filepath.Join(server.CWD, "unexpected-after-initialized.txt"))
	assert.True(t, os.IsNotExist(err), "shutdown should not send extra JSON-RPC messages")
}

func TestSession_CloseIsIdempotentUnderConcurrency(t *testing.T) {
	server := helperServer(t, "close-marker")
	session := NewSession(server, SessionOptions{})
	require.NoError(t, session.Start(t.Context()))

	var wg sync.WaitGroup
	errs := make(chan error, 8)
	for range 8 {
		wg.Go(func() {
			errs <- session.Close(context.WithoutCancel(t.Context()))
		})
	}
	wg.Wait()
	close(errs)

	for err := range errs {
		require.NoError(t, err)
	}

	data, err := os.ReadFile(filepath.Join(server.CWD, "closed.txt"))
	require.NoError(t, err)
	assert.Equal(t, "stdin closed", string(data))
}

func TestSession_CloseWaitsForInFlightStart(t *testing.T) {
	server := helperServer(t, "slow-initialize-response")
	session := NewSession(server, SessionOptions{InitializeTimeout: 5 * time.Second})
	startCh := make(chan error, 1)
	closeStarted := make(chan struct{})
	closeCh := make(chan error, 1)

	go func() {
		startCh <- session.Start(t.Context())
	}()

	require.Eventually(t, func() bool {
		_, err := os.Stat(filepath.Join(server.CWD, "initialize-started.txt"))
		return err == nil
	}, 2*time.Second, 10*time.Millisecond)

	go func() {
		close(closeStarted)
		closeCh <- session.Close(context.WithoutCancel(t.Context()))
	}()

	<-closeStarted
	select {
	case err := <-closeCh:
		require.Failf(t, "Close returned before Start finished", "err=%v", err)
	case <-time.After(50 * time.Millisecond):
	}

	require.NoError(t, <-startCh)
	require.NoError(t, <-closeCh)

	data, err := os.ReadFile(filepath.Join(server.CWD, "closed.txt"))
	require.NoError(t, err)
	assert.Equal(t, "stdin closed", string(data))
}

func TestSession_CloseTerminatesServerThatIgnoresStdinShutdown(t *testing.T) {
	session := NewSession(helperServer(t, "stubborn-close"), SessionOptions{ShutdownTimeout: 100 * time.Millisecond})
	require.NoError(t, session.Start(t.Context()))

	started := time.Now()
	require.NoError(t, session.Close(context.WithoutCancel(t.Context())))

	assert.Less(t, time.Since(started), 2*time.Second)
	assert.Contains(t, session.Stderr(), "ignoring stdin shutdown")
}

func TestSession_CloseReturnsWhenGrandchildHoldsStderr(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("grandchild fixture uses POSIX sh")
	}

	// The background sleep inherits the wrapper's stdout/stderr write ends and
	// outlives the exec'd server, so Close must not wait for pipe EOF forever.
	wrapped := fmt.Sprintf("( sleep 30 & ) ; exec %q -test.run=TestMCPHelperProcess -- close-marker", os.Args[0])
	server := Server{
		Name:    "helper",
		Command: "sh",
		Args:    []string{"-c", wrapped},
		Env: map[string]string{
			"GO_WANT_MCP_HELPER_PROCESS": "1",
			"MCP_HELPER_ENV":             "from-test",
		},
		CWD: t.TempDir(),
	}

	session := NewSession(server, SessionOptions{ShutdownTimeout: 200 * time.Millisecond})
	require.NoError(t, session.Start(t.Context()))

	started := time.Now()
	closed := make(chan error, 1)
	go func() {
		closed <- session.Close(context.WithoutCancel(t.Context()))
	}()

	select {
	case err := <-closed:
		require.NoError(t, err)
		assert.Less(t, time.Since(started), 5*time.Second)
	case <-time.After(5 * time.Second):
		require.FailNow(t, "Session.Close blocked on a grandchild holding stderr")
	}
}

func TestSession_StartIsIdempotentUnderConcurrency(t *testing.T) {
	server := helperServer(t, "start-count")
	session := NewSession(server, SessionOptions{})
	defer func() {
		require.NoError(t, session.Close(context.WithoutCancel(t.Context())))
	}()

	var wg sync.WaitGroup
	errs := make(chan error, 8)
	for range 8 {
		wg.Go(func() {
			errs <- session.Start(t.Context())
		})
	}
	wg.Wait()
	close(errs)

	for err := range errs {
		require.NoError(t, err)
	}

	data, err := os.ReadFile(filepath.Join(server.CWD, "starts.txt"))
	require.NoError(t, err)
	assert.Len(t, strings.Fields(string(data)), 1)
}

func TestSessionPool_ReusesInitializedSession(t *testing.T) {
	pool := NewSessionPool(SessionOptions{})
	defer func() {
		require.NoError(t, pool.CloseAll(context.WithoutCancel(t.Context())))
	}()

	server := helperServer(t, "tools")

	first, err := pool.CallTool(t.Context(), server, "search", map[string]any{"query": "first"})
	require.NoError(t, err)

	tools, err := pool.ListTools(t.Context(), server)
	require.NoError(t, err)
	require.Len(t, tools, 1)
	assert.Equal(t, "search", tools[0].Name)

	second, err := pool.CallTool(t.Context(), server, "search", map[string]any{"query": "second"})
	require.NoError(t, err)

	var firstResult callToolResult
	require.NoError(t, json.Unmarshal(first.Result, &firstResult))
	assert.Equal(t, 1, firstResult.Count)

	var secondResult callToolResult
	require.NoError(t, json.Unmarshal(second.Result, &secondResult))
	assert.Equal(t, 2, secondResult.Count)
}

func TestSessionPool_RoutesProcessStartThroughCentralPermissionPolicy(t *testing.T) {
	policy := permission.DefaultPolicy()
	policy.SetMode(permission.OperationExecute, permission.ModeDeny)
	pool := NewSessionPool(SessionOptions{Permission: &policy})

	session, err := pool.Session(t.Context(), helperServer(t, "echo"))

	require.Error(t, err)
	assert.Nil(t, session)
	assert.True(t, permission.ErrDenied(err))
	assert.Contains(t, err.Error(), `execute operation "mcp server helper" is denied by permission policy`)
}

func TestSessionPool_ToolCallUsesContextPermissionPolicyOnReusedSession(t *testing.T) {
	pool := NewSessionPool(SessionOptions{})
	defer func() {
		require.NoError(t, pool.CloseAll(context.WithoutCancel(t.Context())))
	}()

	server := helperServer(t, "tools")
	session, err := pool.Session(t.Context(), server)
	require.NoError(t, err)
	require.True(t, session.Health(t.Context()).Healthy)

	policy := permission.DefaultPolicy()
	policy.SetMode(permission.OperationExecute, permission.ModeDeny)
	ctx := permission.ContextWithPolicy(t.Context(), &policy)

	response, err := pool.CallTool(ctx, server, "search", map[string]any{"query": "blocked"})

	require.Error(t, err)
	assert.Nil(t, response)
	assert.True(t, permission.ErrDenied(err))
	assert.Contains(t, err.Error(), `execute operation "mcp tool helper/search" is denied by permission policy`)

	response, err = pool.CallTool(t.Context(), server, "search", map[string]any{"query": "allowed"})
	require.NoError(t, err)

	var result callToolResult
	require.NoError(t, json.Unmarshal(response.Result, &result))
	assert.Equal(t, 1, result.Count, "denied pooled tool call should not be sent to the server")
	assert.Equal(t, "allowed", result.Params.Arguments["query"])
}

func TestSessionPool_CloseAllShutsDownAndRestarts(t *testing.T) {
	pool := NewSessionPool(SessionOptions{})
	server := helperServer(t, "tools")

	first, err := pool.Session(t.Context(), server)
	require.NoError(t, err)
	require.NoError(t, pool.CloseAll(context.WithoutCancel(t.Context())))

	second, err := pool.Session(t.Context(), server)
	require.NoError(t, err)
	defer func() {
		require.NoError(t, pool.CloseAll(context.WithoutCancel(t.Context())))
	}()

	assert.NotSame(t, first, second)
}

func TestSessionPool_ReplacesUnhealthyReusableSession(t *testing.T) {
	pool := NewSessionPool(SessionOptions{})
	defer func() {
		require.NoError(t, pool.CloseAll(context.WithoutCancel(t.Context())))
	}()

	server := helperServer(t, "health-fails-after-call")

	first, err := pool.CallTool(t.Context(), server, "search", map[string]any{"query": "first"})
	require.NoError(t, err)

	second, err := pool.CallTool(t.Context(), server, "search", map[string]any{"query": "second"})
	require.NoError(t, err)

	var firstResult callToolResult
	require.NoError(t, json.Unmarshal(first.Result, &firstResult))
	assert.Equal(t, 1, firstResult.Count)

	var secondResult callToolResult
	require.NoError(t, json.Unmarshal(second.Result, &secondResult))
	assert.Equal(t, 1, secondResult.Count, "unhealthy cached session should be replaced before reuse")
	assert.Equal(t, "second", secondResult.Params.Arguments["query"])
}

func TestSessionPool_ReplacesSessionWhenHealthCheckTimesOut(t *testing.T) {
	pool := NewSessionPool(SessionOptions{
		HealthTimeout:   20 * time.Millisecond,
		ShutdownTimeout: 20 * time.Millisecond,
	})
	defer func() {
		require.NoError(t, pool.CloseAll(context.WithoutCancel(t.Context())))
	}()

	server := helperServer(t, "health-hangs-after-call")

	first, err := pool.CallTool(t.Context(), server, "search", map[string]any{"query": "first"})
	require.NoError(t, err)

	started := time.Now()
	second, err := pool.CallTool(t.Context(), server, "search", map[string]any{"query": "second"})
	require.NoError(t, err)
	assert.Less(t, time.Since(started), time.Second, "pool should not wait indefinitely for an unhealthy session")

	var firstResult callToolResult
	require.NoError(t, json.Unmarshal(first.Result, &firstResult))
	assert.Equal(t, 1, firstResult.Count)

	var secondResult callToolResult
	require.NoError(t, json.Unmarshal(second.Result, &secondResult))
	assert.Equal(t, 1, secondResult.Count, "timed-out cached session should be replaced before reuse")
	assert.Equal(t, "second", secondResult.Params.Arguments["query"])
}

func TestSessionPool_ReusesBusyLongRunningSessionWithoutHealthProbe(t *testing.T) {
	pool := NewSessionPool(SessionOptions{
		HealthTimeout:   20 * time.Millisecond,
		ShutdownTimeout: 20 * time.Millisecond,
	})
	defer func() {
		require.NoError(t, pool.CloseAll(context.WithoutCancel(t.Context())))
	}()

	server := helperServer(t, "blocking-slow-tool")
	startedPath := filepath.Join(server.CWD, "blocking-started.txt")

	firstCh := make(chan callResult, 1)
	go func() {
		response, err := pool.CallTool(t.Context(), server, "slow", map[string]any{"delay_ms": 150})
		firstCh <- callResult{response: response, err: err}
	}()

	require.Eventually(t, func() bool {
		_, err := os.Stat(startedPath)
		return err == nil
	}, time.Second, 10*time.Millisecond)

	second, err := pool.CallTool(t.Context(), server, "slow", map[string]any{"delay_ms": 1})
	require.NoError(t, err)

	var first callResult
	select {
	case first = <-firstCh:
	case <-time.After(time.Second):
		require.FailNow(t, "timed out waiting for first blocking tool call")
	}
	require.NoError(t, first.err)
	require.NotNil(t, first.response)

	var firstResult callToolResult
	require.NoError(t, json.Unmarshal(first.response.Result, &firstResult))
	assert.Equal(t, 1, firstResult.Count)

	var secondResult callToolResult
	require.NoError(t, json.Unmarshal(second.Result, &secondResult))
	assert.Equal(t, 2, secondResult.Count, "busy cached session should be reused instead of health-probed and replaced")
}

func TestSessionPool_ZeroValueIsUsable(t *testing.T) {
	var pool SessionPool
	server := helperServer(t, "tools")
	defer func() {
		require.NoError(t, pool.CloseAll(context.WithoutCancel(t.Context())))
	}()

	response, err := pool.CallTool(t.Context(), server, "search", map[string]any{"query": "zero"})

	require.NoError(t, err)

	var result callToolResult
	require.NoError(t, json.Unmarshal(response.Result, &result))
	assert.Equal(t, "zero", result.Params.Arguments["query"])
}

func TestSessionPool_DoesNotBypassDeclaredCapabilityMismatch(t *testing.T) {
	pool := NewSessionPool(SessionOptions{})
	defer func() {
		require.NoError(t, pool.CloseAll(context.WithoutCancel(t.Context())))
	}()

	server := helperServer(t, "echo")
	_, err := pool.Session(t.Context(), server)
	require.NoError(t, err)

	server.Capabilities = []string{"tools"}
	_, err = pool.Session(t.Context(), server)
	require.Error(t, err)
	assert.Contains(t, err.Error(), `declares "tools" capability`)
}

func receiveCallResult(t *testing.T, ch <-chan callResult) callResult {
	t.Helper()

	select {
	case result := <-ch:
		require.NotNil(t, result.response)
		return result
	case <-time.After(time.Second):
		require.FailNow(t, "timed out waiting for MCP tool call")
		return callResult{}
	}
}

func TestInvoke_ReturnsRPCError(t *testing.T) {
	server := helperServer(t, "rpc-error")

	response, err := Invoke(t.Context(), server, Request{Method: "fail"}, helperTimeout)

	require.Error(t, err)
	assert.Contains(t, err.Error(), `mcp server "helper" returned error -32000: boom`)
	require.NotNil(t, response)
	require.NotNil(t, response.Error)
	assert.Equal(t, -32000, response.Error.Code)
}

func TestInvoke_RedactsExplicitCredentialEnvFromErrors(t *testing.T) {
	server := helperServer(t, "stderr-secret-exit")
	server.Env["MCP_SECRET_TOKEN"] = helperSecretToken

	response, err := Invoke(t.Context(), server, Request{Method: "fail"}, helperTimeout)

	require.Error(t, err)
	assert.Nil(t, response)
	assert.NotContains(t, err.Error(), helperSecretToken)
	assert.Contains(t, err.Error(), "<redacted:mcp_server_env>")
}

func TestInvoke_RedactsExplicitCredentialEnvFromRPCErrorMessage(t *testing.T) {
	server := helperServer(t, "rpc-error-secret")
	server.Env["MCP_SECRET_TOKEN"] = helperSecretToken

	response, err := Invoke(t.Context(), server, Request{Method: "fail"}, helperTimeout)

	require.Error(t, err)
	require.NotNil(t, response)
	require.NotNil(t, response.Error)
	assert.NotContains(t, err.Error(), helperSecretToken)
	assert.NotContains(t, response.Error.Message, helperSecretToken)
	assert.Contains(t, err.Error(), "<redacted:mcp_server_env>")
	assert.Contains(t, response.Error.Message, "<redacted:mcp_server_env>")
}

func TestInvoke_HonorsTimeout(t *testing.T) {
	server := helperServer(t, "slow-init")

	response, err := Invoke(t.Context(), server, Request{Method: "slow"}, 25*time.Millisecond)

	require.Error(t, err)
	assert.Nil(t, response)
	assert.Contains(t, err.Error(), "context deadline exceeded")
}

func TestInvoke_ValidatesInputs(t *testing.T) {
	_, err := Invoke(t.Context(), Server{Name: "bad"}, Request{Method: "ping"}, helperTimeout)
	require.Error(t, err)
	assert.Contains(t, err.Error(), `server "bad": missing command`)

	_, err = Invoke(t.Context(), helperServer(t, "echo"), Request{}, helperTimeout)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "missing method")

	_, err = Invoke(t.Context(), helperServer(t, "echo"), Request{ID: map[string]any{"bad": true}, Method: "ping"}, helperTimeout)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "invalid request id")
	assert.Contains(t, err.Error(), "id must be a string or number")

	_, err = CallTool(t.Context(), helperServer(t, "tools"), " ", nil, helperTimeout)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "missing tool name")
}

func TestInvoke_RequiresActiveContext(t *testing.T) {
	_, err := Invoke(nil, helperServer(t, "echo"), Request{Method: "ping"}, helperTimeout) //nolint:staticcheck // Verifies the required-context contract.
	require.Error(t, err)
	assert.Contains(t, err.Error(), "context is required")

	ctx, cancel := context.WithCancel(t.Context())
	cancel()

	_, err = Invoke(ctx, helperServer(t, "echo"), Request{Method: "ping"}, helperTimeout)
	require.Error(t, err)
	require.ErrorIs(t, err, context.Canceled)
}

func helperServer(t *testing.T, mode string) Server {
	t.Helper()

	cwd := t.TempDir()

	return Server{
		Name:    "helper",
		Command: os.Args[0],
		Args:    []string{"-test.run=TestMCPHelperProcess", "--", mode},
		Env: map[string]string{
			"GO_WANT_MCP_HELPER_PROCESS": "1",
			"MCP_HELPER_ENV":             "from-test",
		},
		CWD: cwd,
	}
}

type callToolResult struct {
	Params CallToolParams `json:"params"`
	Method string         `json:"method"`
	Count  int            `json:"count"`
}

type callResult struct {
	response *Response
	err      error
}

//nolint:paralleltest // Helper process entry point must run synchronously when env-gated.
func TestMCPHelperProcess(_ *testing.T) {
	if os.Getenv("GO_WANT_MCP_HELPER_PROCESS") != "1" {
		return
	}

	mode := "echo"
	for i, arg := range os.Args {
		if arg == "--" && i+1 < len(os.Args) {
			mode = os.Args[i+1]
			break
		}
	}

	switch mode {
	case "slow-init":
		time.Sleep(2 * time.Second)
	case "malformed":
		_ = readHelperRequest()
		fmt.Println(`{"jsonrpc":"2.0",`)
	case "init-error":
		runInitErrorHelper(false)
	case "stderr-init-error":
		runInitErrorHelper(true)
	case "stderr-secret-exit":
		fmt.Fprintf(os.Stderr, "server failed with token %s", os.Getenv("MCP_SECRET_TOKEN"))
		os.Exit(2)
	case "echo", "rpc-error", "rpc-error-secret", "tools", "complex-tool", "number-tool", "utilities", "ping-before-init", "unsupported-server-request-before-init", "health-fails-after-call", "health-hangs-after-call", "slow-tool", "slow-tool-close-marker", "out-of-order-tool", "blocking-slow-tool", "malformed-tools", "malformed-tool-schema", "missing-tool-schema", "duplicate-required-tool-schema", "repeated-tools-cursor", "too-many-tools-pages", "malformed-operation", "large-response", "oversized-response", "response-with-method", "malformed-error-object", "malformed-response-id", "catalog", "close-marker", "strict-close", "slow-initialize-response", "stubborn-close", "start-count", "unsupported-protocol", "missing-init-capabilities", "exit-after-init", "record-initialized":
		runLifecycleHelper(mode)
	default:
		fmt.Fprintf(os.Stderr, "unknown helper mode %q", mode)
		os.Exit(2)
	}

	os.Exit(0)
}

func runInitErrorHelper(withStderr bool) {
	request := readHelperRequest()
	if withStderr {
		fmt.Fprintln(os.Stderr, "diagnostic before failure")
	}

	writeHelperResponse(Response{JSONRPC: "2.0", ID: request.ID, Error: &ResponseError{Code: -32000, Message: "init boom"}})
}

func runLifecycleHelper(mode string) {
	var mu sync.Mutex
	callCount := 0
	initialized := false
	cancelled := make(map[string]bool)

	scanner := bufio.NewScanner(os.Stdin)
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)

	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" {
			continue
		}

		var request Request
		if err := decodeJSONUseNumber([]byte(line), &request); err != nil {
			fmt.Fprintf(os.Stderr, "decode request: %v", err)
			os.Exit(2)
		}
		if request.Method == "" {
			handleHelperClientResponse(mode, line)
			continue
		}
		if mode == "strict-close" && initialized {
			writeHelperMarker("unexpected-after-initialized.txt", request.Method)
			os.Exit(2)
		}

		switch request.Method {
		case "initialize":
			if mode == "ping-before-init" {
				fmt.Println(`{"jsonrpc":"2.0","id":"server-ping","method":"ping"}`)
			}
			if mode == "unsupported-server-request-before-init" {
				fmt.Println(`{"jsonrpc":"2.0","id":"server-roots","method":"roots/list"}`)
			}
			if mode == "slow-initialize-response" {
				writeHelperMarker("initialize-started.txt", "started")
				time.Sleep(250 * time.Millisecond)
			}
			writeInitializeResponse(request, mode)
		case "notifications/initialized":
			initialized = true
			if mode == "record-initialized" {
				writeHelperMarker("initialized.txt", "initialized")
			}
			if mode == "exit-after-init" {
				return
			}
		case "notifications/cancelled":
			var params struct {
				RequestID any    `json:"requestId"`
				Reason    string `json:"reason"`
			}
			if err := decodeParams(request.Params, &params); err != nil {
				fmt.Fprintf(os.Stderr, "decode cancellation params: %v", err)
				os.Exit(2)
			}
			key := helperIDKey(params.RequestID)
			mu.Lock()
			cancelled[key] = true
			mu.Unlock()
			if err := os.WriteFile(filepath.Join(mustGetwd(), "cancelled.txt"), []byte(params.Reason), 0o600); err != nil {
				fmt.Fprintf(os.Stderr, "write cancellation marker: %v", err)
				os.Exit(2)
			}
		case "tools/list":
			writeToolsListResponse(request, mode)
		case "resources/list":
			writeResourcesListResponse(request)
		case "prompts/list":
			writePromptsListResponse(request)
		case "tools/call":
			params := decodeHelperCallToolParams(request.Params)
			if mode == "slow-tool" || mode == "slow-tool-close-marker" || mode == "out-of-order-tool" {
				mu.Lock()
				callCount++
				count := callCount
				mu.Unlock()
				if mode == "slow-tool-close-marker" {
					writeHelperMarker("slow-started.txt", "started")
				}
				go respondSlowTool(request, params, count, mode == "out-of-order-tool", &mu, cancelled)
				continue
			}
			if mode == "blocking-slow-tool" {
				mu.Lock()
				callCount++
				count := callCount
				mu.Unlock()
				respondBlockingSlowTool(request, params, count)
				continue
			}

			mu.Lock()
			callCount++
			count := callCount
			mu.Unlock()
			result := map[string]any{"method": request.Method, "params": params, "count": count}
			writeHelperResponse(Response{JSONRPC: "2.0", ID: request.ID, Result: mustMarshal(result)})
		case "ping":
			if mode == "health-fails-after-call" && callCount > 0 {
				writeHelperResponse(Response{JSONRPC: "2.0", ID: request.ID, Error: &ResponseError{Code: -32000, Message: "health degraded"}})
				continue
			}
			if mode == "health-hangs-after-call" && callCount > 0 {
				time.Sleep(5 * time.Second)
				continue
			}

			writeHelperResponse(Response{JSONRPC: "2.0", ID: request.ID, Result: json.RawMessage(`{}`)})
		default:
			if mode == "malformed-operation" {
				// Emit one junk stdout line before the valid response; the
				// client must skip it without killing the transport.
				fmt.Println(`{"jsonrpc":"2.0",`)
			}
			if mode == "large-response" {
				writeHelperResponse(Response{JSONRPC: "2.0", ID: request.ID, Result: mustMarshal(map[string]any{"payload": strings.Repeat("a", largeHelperPayloadBytes)})})
				continue
			}
			if mode == "oversized-response" {
				writeHelperResponse(Response{JSONRPC: "2.0", ID: request.ID, Result: mustMarshal(map[string]any{"payload": strings.Repeat("a", oversizedHelperPayloadBytes)})})
				continue
			}
			if mode == "response-with-method" {
				fmt.Printf(`{"jsonrpc":"2.0","id":%s,"method":"unexpected/serverMethod","result":{}}`+"\n", helperIDKey(request.ID))
				continue
			}
			if mode == "malformed-error-object" {
				fmt.Printf(`{"jsonrpc":"2.0","id":%s,"error":{"message":"missing code"}}`+"\n", helperIDKey(request.ID))
				continue
			}
			if mode == "malformed-response-id" {
				fmt.Println(`{"jsonrpc":"2.0","id":{"bad":true},"result":{}}`)
				continue
			}

			if mode == "rpc-error" || mode == "rpc-error-secret" {
				message := "boom"
				if mode == "rpc-error-secret" {
					message += " " + os.Getenv("MCP_SECRET_TOKEN")
				}

				writeHelperResponse(Response{JSONRPC: "2.0", ID: request.ID, Error: &ResponseError{Code: -32000, Message: message}})
				continue
			}

			result := map[string]any{
				"method":      request.Method,
				"params":      request.Params,
				"env":         os.Getenv("MCP_HELPER_ENV"),
				"cwd":         mustGetwd(),
				"initialized": initialized,
			}
			writeHelperResponse(Response{JSONRPC: "2.0", ID: request.ID, Result: mustMarshal(result)})
		}
	}

	if err := scanner.Err(); err != nil {
		fmt.Fprintf(os.Stderr, "scan requests: %v", err)
		os.Exit(2)
	}

	if mode == "slow-tool-close-marker" {
		time.Sleep(50 * time.Millisecond)
	}
	if mode == "close-marker" || mode == "strict-close" || mode == "slow-tool-close-marker" || mode == "slow-initialize-response" {
		if err := os.WriteFile(filepath.Join(mustGetwd(), "closed.txt"), []byte("stdin closed"), 0o600); err != nil {
			fmt.Fprintf(os.Stderr, "write shutdown marker: %v", err)
			os.Exit(2)
		}
	}

	if mode == "stubborn-close" {
		fmt.Fprintln(os.Stderr, "ignoring stdin shutdown")
		time.Sleep(5 * time.Second)
	}
}

func handleHelperClientResponse(mode, line string) {
	var response Response
	if err := decodeJSONUseNumber([]byte(line), &response); err != nil {
		fmt.Fprintf(os.Stderr, "decode client response: %v", err)
		os.Exit(2)
	}

	switch mode {
	case "ping-before-init":
		writeHelperMarker("client-pong.txt", "pong")
	case "unsupported-server-request-before-init":
		if response.Error != nil && response.Error.Code == -32601 {
			writeHelperMarker("unsupported-server-request.txt", response.Error.Message)
		}
	}
}

func writeHelperMarker(name, value string) {
	if err := os.WriteFile(filepath.Join(mustGetwd(), name), []byte(value), 0o600); err != nil {
		fmt.Fprintf(os.Stderr, "write helper marker %s: %v", name, err)
		os.Exit(2)
	}
}

func writeInitializeResponse(request Request, mode string) {
	if mode == "start-count" {
		file, err := os.OpenFile(filepath.Join(mustGetwd(), "starts.txt"), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
		if err != nil {
			fmt.Fprintf(os.Stderr, "open starts marker: %v", err)
			os.Exit(2)
		}
		if _, err := fmt.Fprintln(file, "init"); err != nil {
			fmt.Fprintf(os.Stderr, "write starts marker: %v", err)
			os.Exit(2)
		}
		if err := file.Close(); err != nil {
			fmt.Fprintf(os.Stderr, "close starts marker: %v", err)
			os.Exit(2)
		}
	}

	if mode == "missing-init-capabilities" {
		result := map[string]any{
			"protocolVersion": DefaultProtocolVersion,
			"serverInfo": map[string]any{
				"name":    "helper",
				"version": "1.0.0",
			},
		}
		writeHelperResponse(Response{JSONRPC: "2.0", ID: request.ID, Result: mustMarshal(result)})
		return
	}

	caps := ServerCapabilities{}
	if mode == "tools" || mode == "complex-tool" || mode == "number-tool" || mode == "health-fails-after-call" || mode == "health-hangs-after-call" || mode == "slow-tool" || mode == "slow-tool-close-marker" || mode == "out-of-order-tool" || mode == "blocking-slow-tool" || mode == "malformed-tools" || mode == "malformed-tool-schema" || mode == "missing-tool-schema" || mode == "duplicate-required-tool-schema" || mode == "repeated-tools-cursor" || mode == "too-many-tools-pages" {
		caps.Tools = &ListChangedCapability{}
	}
	if mode == "catalog" {
		caps.Resources = &ResourcesCapability{}
		caps.Prompts = &ListChangedCapability{}
	}
	if mode == "utilities" {
		caps.Logging = map[string]any{"supported": true}
		caps.Completions = map[string]any{"supported": true}
		caps.Tasks = map[string]any{"supported": true}
	}

	result := InitializeResult{
		ProtocolVersion: DefaultProtocolVersion,
		Capabilities:    caps,
		ServerInfo: Implementation{
			Name:    "helper",
			Version: "1.0.0",
		},
	}
	if mode == "unsupported-protocol" {
		result.ProtocolVersion = "1900-01-01"
	}

	writeHelperResponse(Response{JSONRPC: "2.0", ID: request.ID, Result: mustMarshal(result)})
}

func writeToolsListResponse(request Request, mode string) {
	if mode == "malformed-tools" {
		writeHelperResponse(Response{JSONRPC: "2.0", ID: request.ID, Result: json.RawMessage(`{"not_tools":[]}`)})
		return
	}
	if mode == "malformed-tool-schema" {
		result := ListToolsResult{Tools: []Tool{{
			Name:        "bad-schema",
			InputSchema: json.RawMessage(`{"type":"object","required":"query"}`),
		}}}
		writeHelperResponse(Response{JSONRPC: "2.0", ID: request.ID, Result: mustMarshal(result)})
		return
	}
	if mode == "missing-tool-schema" {
		result := ListToolsResult{Tools: []Tool{{Name: "missing-schema"}}}
		writeHelperResponse(Response{JSONRPC: "2.0", ID: request.ID, Result: mustMarshal(result)})
		return
	}
	if mode == "duplicate-required-tool-schema" {
		result := ListToolsResult{Tools: []Tool{{
			Name:        "duplicate-required",
			InputSchema: json.RawMessage(`{"type":"object","properties":{"query":{"type":"string"}},"required":["query","query"]}`),
		}}}
		writeHelperResponse(Response{JSONRPC: "2.0", ID: request.ID, Result: mustMarshal(result)})
		return
	}
	if mode == "repeated-tools-cursor" {
		writeHelperResponse(Response{JSONRPC: "2.0", ID: request.ID, Result: mustMarshal(ListToolsResult{
			Tools:      []Tool{},
			NextCursor: "loop",
		})})
		return
	}
	if mode == "too-many-tools-pages" {
		var params struct {
			Cursor string `json:"cursor"`
		}
		if err := decodeParams(request.Params, &params); err != nil {
			fmt.Fprintf(os.Stderr, "decode pagination params: %v", err)
			os.Exit(2)
		}

		nextCursor := "page-1"
		if params.Cursor != "" {
			nextCursor = params.Cursor + "-next"
		}

		writeHelperResponse(Response{JSONRPC: "2.0", ID: request.ID, Result: mustMarshal(ListToolsResult{
			Tools:      []Tool{},
			NextCursor: nextCursor,
		})})
		return
	}

	name := "search"
	schema := map[string]any{
		"type": "object",
		"properties": map[string]any{
			"query": map[string]any{"type": "string"},
		},
		"required":             []string{"query"},
		"additionalProperties": false,
	}
	if mode == "slow-tool" || mode == "slow-tool-close-marker" || mode == "out-of-order-tool" || mode == "blocking-slow-tool" {
		name = "slow"
		schema = map[string]any{
			"type": "object",
			"properties": map[string]any{
				"delay_ms": map[string]any{"type": "integer"},
			},
		}
	}
	if mode == "complex-tool" {
		name = "complex"
		schema = map[string]any{
			"type": "object",
			"properties": map[string]any{
				"tags":    map[string]any{"type": "array"},
				"filters": map[string]any{"type": "object"},
			},
			"required":             []string{"tags", "filters"},
			"additionalProperties": false,
		}
	}
	if mode == "number-tool" {
		name = "number"
		schema = map[string]any{
			"type": "object",
			"properties": map[string]any{
				"n": map[string]any{"type": "integer"},
			},
			"required":             []string{"n"},
			"additionalProperties": false,
		}
	}

	result := ListToolsResult{Tools: []Tool{{Name: name, InputSchema: mustMarshal(schema)}}}
	writeHelperResponse(Response{JSONRPC: "2.0", ID: request.ID, Result: mustMarshal(result)})
}

func writeResourcesListResponse(request Request) {
	result := ListResourcesResult{
		Resources: []Resource{{
			URI:         "file:///repo/README.md",
			Name:        "README",
			Description: "Repository README",
			MimeType:    "text/markdown",
		}},
	}
	writeHelperResponse(Response{JSONRPC: "2.0", ID: request.ID, Result: mustMarshal(result)})
}

func writePromptsListResponse(request Request) {
	result := ListPromptsResult{
		Prompts: []Prompt{{
			Name:        "summarize",
			Description: "Summarize a topic",
			Arguments: []PromptArgument{{
				Name:     "topic",
				Required: true,
			}},
		}},
	}
	writeHelperResponse(Response{JSONRPC: "2.0", ID: request.ID, Result: mustMarshal(result)})
}

func respondSlowTool(request Request, params CallToolParams, count int, includeDelay bool, mu *sync.Mutex, cancelled map[string]bool) {
	delay := helperDelay(params)
	time.Sleep(delay)

	key := helperIDKey(request.ID)
	mu.Lock()
	wasCancelled := cancelled[key]
	mu.Unlock()
	if wasCancelled {
		return
	}

	result := map[string]any{"slept": true, "count": count}
	if includeDelay {
		result["delay_ms"] = int(delay / time.Millisecond)
	}
	writeHelperResponse(Response{JSONRPC: "2.0", ID: request.ID, Result: mustMarshal(result)})
}

func respondBlockingSlowTool(request Request, params CallToolParams, count int) {
	if count == 1 {
		if err := os.WriteFile(filepath.Join(mustGetwd(), "blocking-started.txt"), []byte("started"), 0o600); err != nil {
			fmt.Fprintf(os.Stderr, "write blocking marker: %v", err)
			os.Exit(2)
		}
	}

	time.Sleep(helperDelay(params))
	writeHelperResponse(Response{JSONRPC: "2.0", ID: request.ID, Result: mustMarshal(map[string]any{"slept": true, "count": count})})
}

func helperDelay(params CallToolParams) time.Duration {
	delay := 100 * time.Millisecond
	if raw, ok := params.Arguments["delay_ms"]; ok {
		switch typed := raw.(type) {
		case float64:
			delay = time.Duration(typed) * time.Millisecond
		case int:
			delay = time.Duration(typed) * time.Millisecond
		case json.Number:
			ms, err := typed.Int64()
			if err == nil {
				delay = time.Duration(ms) * time.Millisecond
			}
		}
	}

	return delay
}

func readHelperRequest() Request {
	line, err := bufio.NewReader(os.Stdin).ReadString('\n')
	if err != nil {
		fmt.Fprintf(os.Stderr, "read request: %v", err)
		os.Exit(2)
	}

	if !strings.HasSuffix(line, "\n") {
		fmt.Fprint(os.Stderr, "request was not newline-delimited")
		os.Exit(2)
	}

	var request Request
	if err := decodeJSONUseNumber([]byte(line), &request); err != nil {
		fmt.Fprintf(os.Stderr, "decode request: %v", err)
		os.Exit(2)
	}

	return request
}

func decodeHelperCallToolParams(raw any) CallToolParams {
	var params CallToolParams
	if err := decodeParams(raw, &params); err != nil {
		fmt.Fprintf(os.Stderr, "decode tool params: %v", err)
		os.Exit(2)
	}

	return params
}

func decodeParams(raw, target any) error {
	data, err := json.Marshal(raw)
	if err != nil {
		return err
	}

	return decodeJSONUseNumber(data, target)
}

func helperIDKey(id any) string {
	data, err := json.Marshal(id)
	if err != nil {
		return ""
	}

	return string(data)
}

func writeHelperResponse(response Response) {
	data, err := json.Marshal(response)
	if err != nil {
		fmt.Fprintf(os.Stderr, "marshal response: %v", err)
		os.Exit(2)
	}

	fmt.Printf("%s\n", data)
}

func mustMarshal(value any) json.RawMessage {
	data, err := json.Marshal(value)
	if err != nil {
		fmt.Fprintf(os.Stderr, "marshal result: %v", err)
		os.Exit(2)
	}

	return data
}

func realPath(t *testing.T, path string) string {
	t.Helper()

	resolved, err := filepath.EvalSymlinks(path)
	require.NoError(t, err)

	return filepath.Clean(resolved)
}

func mustGetwd() string {
	wd, err := os.Getwd()
	if err != nil {
		fmt.Fprintf(os.Stderr, "getwd: %v", err)
		os.Exit(2)
	}

	return filepath.Clean(wd)
}
