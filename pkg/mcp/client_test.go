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
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const helperTimeout = 5 * time.Second

func TestInvoke_SendsNewlineDelimitedJSONAndReadsResponse(t *testing.T) {
	t.Parallel()

	server := helperServer(t, "echo")

	response, err := Invoke(t.Context(), server, Request{
		ID:     "req-1",
		Method: "ping",
		Params: map[string]any{"message": "hello"},
	}, helperTimeout)

	require.NoError(t, err)
	assert.Equal(t, "2.0", response.JSONRPC)
	assert.Equal(t, "req-1", response.ID)

	var result map[string]any
	require.NoError(t, json.Unmarshal(response.Result, &result))
	assert.Equal(t, "ping", result["method"])
	assert.Equal(t, map[string]any{"message": "hello"}, result["params"])
	assert.Equal(t, "from-test", result["env"])
	assert.Equal(t, realPath(t, server.CWD), result["cwd"])
}

func TestInvoke_ReadsFastExitingOneShotServerResponse(t *testing.T) {
	t.Parallel()

	if runtime.GOOS == "windows" {
		t.Skip("shell helper uses POSIX sh")
	}

	helper := filepath.Join(t.TempDir(), "mcp-helper")
	script := "#!/bin/sh\n" +
		"read line\n" +
		"printf '{\"jsonrpc\":\"2.0\",\"id\":1,\"result\":{\"ok\":true,\"source\":\"fast-helper\"}}\\n'\n"
	require.NoError(t, os.WriteFile(helper, []byte(script), 0o600))

	response, err := Invoke(t.Context(), Server{Name: "helper", Command: "sh", Args: []string{helper}}, Request{
		ID:     1,
		Method: "ping",
	}, helperTimeout)

	require.NoError(t, err)
	assert.JSONEq(t, `{"ok":true,"source":"fast-helper"}`, string(response.Result))
}

func TestCallTool_UsesToolsCallMethod(t *testing.T) {
	t.Parallel()

	server := helperServer(t, "echo")

	response, err := CallTool(t.Context(), server, "search", map[string]any{"query": "mcp"}, helperTimeout)

	require.NoError(t, err)

	var result callToolResult
	require.NoError(t, json.Unmarshal(response.Result, &result))
	assert.Equal(t, "tools/call", result.Method)
	assert.Equal(t, "search", result.Params.Name)
	assert.Equal(t, map[string]any{"query": "mcp"}, result.Params.Arguments)
}

func TestInvoke_ReturnsRPCError(t *testing.T) {
	t.Parallel()

	server := helperServer(t, "rpc-error")

	response, err := Invoke(t.Context(), server, Request{Method: "fail"}, helperTimeout)

	require.Error(t, err)
	assert.Contains(t, err.Error(), `mcp server "helper" returned error -32000: boom`)
	require.NotNil(t, response)
	require.NotNil(t, response.Error)
	assert.Equal(t, -32000, response.Error.Code)
}

func TestInvoke_HonorsTimeout(t *testing.T) {
	t.Parallel()

	server := helperServer(t, "sleep")

	response, err := Invoke(t.Context(), server, Request{Method: "slow"}, 25*time.Millisecond)

	require.Error(t, err)
	assert.Nil(t, response)
	assert.Contains(t, err.Error(), "timed out or was canceled")
}

func TestInvoke_ValidatesInputs(t *testing.T) {
	t.Parallel()

	_, err := Invoke(t.Context(), Server{Name: "bad"}, Request{Method: "ping"}, helperTimeout)
	require.Error(t, err)
	assert.Contains(t, err.Error(), `server "bad": missing command`)

	_, err = Invoke(t.Context(), helperServer(t, "echo"), Request{}, helperTimeout)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "missing method")

	_, err = CallTool(t.Context(), helperServer(t, "echo"), " ", nil, helperTimeout)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "missing tool name")
}

func TestInvoke_RequiresActiveContext(t *testing.T) {
	t.Parallel()

	_, err := Invoke(nil, helperServer(t, "echo"), Request{Method: "ping"}, helperTimeout) //nolint:staticcheck // Verifies the required-context contract.
	require.Error(t, err)
	assert.Contains(t, err.Error(), "context is required")

	ctx, cancel := context.WithCancel(context.Background())
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
	case "sleep":
		time.Sleep(2 * time.Second)
		os.Exit(0)
	case "echo":
		runEchoHelper()
	case "rpc-error":
		runErrorHelper()
	default:
		fmt.Fprintf(os.Stderr, "unknown helper mode %q", mode)
		os.Exit(2)
	}

	os.Exit(0)
}

func runEchoHelper() {
	request := readHelperRequest()
	result := map[string]any{
		"method": request.Method,
		"params": request.Params,
		"env":    os.Getenv("MCP_HELPER_ENV"),
		"cwd":    mustGetwd(),
	}
	writeHelperResponse(Response{JSONRPC: "2.0", ID: request.ID, Result: mustMarshal(result)})
}

func runErrorHelper() {
	request := readHelperRequest()
	writeHelperResponse(Response{JSONRPC: "2.0", ID: request.ID, Error: &ResponseError{Code: -32000, Message: "boom"}})
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
	if err := json.Unmarshal([]byte(line), &request); err != nil {
		fmt.Fprintf(os.Stderr, "decode request: %v", err)
		os.Exit(2)
	}

	return request
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
