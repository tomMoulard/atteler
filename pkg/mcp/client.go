package mcp

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
	"time"

	"github.com/tommoulard/atteler/pkg/shell"
)

// Request is a JSON-RPC 2.0 request sent to an MCP server over stdio.
//
//nolint:govet // JSON field order mirrors the JSON-RPC envelope for readability.
type Request struct {
	JSONRPC string `json:"jsonrpc"`
	ID      any    `json:"id,omitempty"`
	Method  string `json:"method"`
	Params  any    `json:"params,omitempty"`
}

// Response is a JSON-RPC 2.0 response received from an MCP server over stdio.
//
//nolint:govet // JSON field order mirrors the JSON-RPC envelope for readability.
type Response struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      any             `json:"id,omitempty"`
	Result  json.RawMessage `json:"result,omitempty"`
	Error   *ResponseError  `json:"error,omitempty"`
}

// ResponseError is a JSON-RPC 2.0 error object.
//
//nolint:govet // JSON field order mirrors JSON-RPC error objects.
type ResponseError struct {
	Code    int             `json:"code"`
	Message string          `json:"message"`
	Data    json.RawMessage `json:"data,omitempty"`
}

// CallToolParams are the MCP tools/call request parameters.
//
//nolint:govet // JSON field order keeps name before arguments.
type CallToolParams struct {
	Name      string         `json:"name"`
	Arguments map[string]any `json:"arguments,omitempty"`
}

// Invoke sends one newline-delimited JSON-RPC request to server over stdio and
// returns the first response with the same id. A positive timeout is applied on
// top of ctx; pass 0 to rely on ctx only.
//
//nolint:cyclop // Stdio process lifecycle has several distinct error exits.
func Invoke(ctx context.Context, server Server, request Request, timeout time.Duration) (*Response, error) {
	if err := requireInvokeContext(ctx); err != nil {
		return nil, err
	}

	if timeout > 0 {
		var cancel context.CancelFunc

		ctx, cancel = context.WithTimeout(ctx, timeout)
		defer cancel()
	}

	if err := server.Validate(); err != nil {
		return nil, fmt.Errorf("invoke mcp server: %w", err)
	}

	if err := request.Validate(); err != nil {
		return nil, fmt.Errorf("invoke mcp server %q: %w", strings.TrimSpace(server.Name), err)
	}

	request.JSONRPC = "2.0"
	if request.ID == nil {
		request.ID = 1
	}

	serverSecretValues := explicitServerSecretValues(server.Env)

	cmd, invocation, err := shell.CommandContext(ctx, shell.CommandOptions{
		Program: strings.TrimSpace(server.Command),
		Args:    server.Args,
		Dir:     server.CWD,
		Env:     server.Env,
		Mode:    shell.ModeStreaming,
		Policy: &shell.Policy{
			AllowCredentialEnv: explicitServerEnvNames(server.Env),
		},
		SecretValues: serverSecretValues,
		Audit:        shell.AuditContext{Caller: "atteler.mcp." + strings.TrimSpace(server.Name)},
	})
	if err != nil {
		return nil, fmt.Errorf("authorize mcp server %q: %w", strings.TrimSpace(server.Name), err)
	}

	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, fmt.Errorf("open stdin for mcp server %q: %w", strings.TrimSpace(server.Name), finishMCPSetupError(invocation, err))
	}

	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, fmt.Errorf("open stdout for mcp server %q: %w", strings.TrimSpace(server.Name), finishMCPSetupError(invocation, err))
	}

	stderr, err := cmd.StderrPipe()
	if err != nil {
		return nil, fmt.Errorf("open stderr for mcp server %q: %w", strings.TrimSpace(server.Name), finishMCPSetupError(invocation, err))
	}

	if err := cmd.Start(); err != nil {
		return nil, finishMCPStartError(ctx, invocation, strings.TrimSpace(server.Name), err)
	}

	stderrCh := readAll(stderr)
	responseCh := readResponse(stdout, request.ID)
	writeErr := writeRequest(stdin, request)

	readResult := <-responseCh
	// Start Wait only after the stdout reader has delivered a response. Cmd.Wait
	// closes command pipes after the process exits; racing it against
	// readResponse can turn a valid one-shot server response into
	// "file already closed" on fast-exiting helpers.
	waitCh := waitFor(cmd)
	killed, waitErr := finishProcess(ctx, cmd, waitCh)

	stderrText := strings.TrimSpace(redactServerSecretValues(<-stderrCh, serverSecretValues))
	if finishErr := invocation.Finish(shell.FinishOptions{Stderr: stderrText, Error: waitErr, OutputCapture: shell.OutputNotCaptured, OutputNote: "MCP JSON-RPC protocol output was not captured"}); finishErr != nil {
		return nil, fmt.Errorf("audit mcp server %q: %w", strings.TrimSpace(server.Name), finishErr)
	}

	if waitErr != nil && ctx.Err() != nil {
		return nil, withProcessOutput(fmt.Errorf("mcp server %q timed out or was canceled: %w", strings.TrimSpace(server.Name), ctx.Err()), stderrText)
	}

	if writeErr != nil {
		return nil, withProcessOutput(fmt.Errorf("write request to mcp server %q: %w", strings.TrimSpace(server.Name), writeErr), stderrText)
	}

	if readResult.err != nil {
		return nil, withProcessOutput(fmt.Errorf("read response from mcp server %q: %w", strings.TrimSpace(server.Name), readResult.err), stderrText)
	}

	response := readResult.response

	if waitErr != nil && !killed {
		return nil, withProcessOutput(fmt.Errorf("mcp server %q exited: %w", strings.TrimSpace(server.Name), waitErr), stderrText)
	}

	if response.Error != nil {
		response.Error.Message = redactServerSecretValues(response.Error.Message, serverSecretValues)
		return response, fmt.Errorf("mcp server %q returned error %d: %s", strings.TrimSpace(server.Name), response.Error.Code, response.Error.Message)
	}

	return response, nil
}

func explicitServerEnvNames(env map[string]string) []string {
	if len(env) == 0 {
		return nil
	}

	names := make([]string, 0, len(env))
	for name := range env {
		if strings.TrimSpace(name) != "" {
			names = append(names, name)
		}
	}

	return names
}

func explicitServerSecretValues(env map[string]string) []string {
	if len(env) == 0 {
		return nil
	}

	values := make([]string, 0, len(env))
	for name, value := range env {
		value = strings.TrimSpace(value)
		if value != "" && mcpCredentialEnvName(name) {
			values = append(values, value)
		}
	}

	return values
}

func mcpCredentialEnvName(name string) bool {
	name = strings.ToUpper(strings.TrimSpace(name))
	for _, marker := range []string{"TOKEN", "SECRET", "KEY", "AUTH", "PASSWORD", "PASSWD", "COOKIE", "CREDENTIAL", "PRIVATE"} {
		if strings.Contains(name, marker) {
			return true
		}
	}

	return false
}

func redactServerSecretValues(text string, secrets []string) string {
	for _, secret := range secrets {
		secret = strings.TrimSpace(secret)
		if secret == "" {
			continue
		}

		text = strings.ReplaceAll(text, secret, "<redacted:mcp_server_env>")
	}

	return text
}

func requireInvokeContext(ctx context.Context) error {
	if ctx == nil {
		return errors.New("invoke mcp server: context is required")
	}

	if err := ctx.Err(); err != nil {
		return fmt.Errorf("invoke mcp server: context already done: %w", err)
	}

	return nil
}

func finishMCPSetupError(invocation *shell.Invocation, err error) error {
	if finishErr := invocation.Finish(shell.FinishOptions{
		Error:         err,
		OutputCapture: shell.OutputNotCaptured,
		OutputNote:    "MCP server failed before JSON-RPC streaming",
	}); finishErr != nil {
		return errors.Join(err, finishErr)
	}

	return err
}

func finishMCPStartError(ctx context.Context, invocation *shell.Invocation, serverName string, err error) error {
	startErr := fmt.Errorf("start mcp server %q: %w", serverName, err)
	if ctxErr := mcpStartContextError(ctx, err); ctxErr != nil {
		startErr = fmt.Errorf("mcp server %q timed out or was canceled: %w", serverName, ctxErr)
	}

	if finishErr := invocation.Finish(shell.FinishOptions{Error: err, OutputCapture: shell.OutputNotCaptured, OutputNote: "MCP server failed before JSON-RPC streaming"}); finishErr != nil {
		return errors.Join(startErr, finishErr)
	}

	return startErr
}

func mcpStartContextError(ctx context.Context, err error) error {
	if ctxErr := ctx.Err(); ctxErr != nil {
		return errors.Join(ctxErr, err)
	}

	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return err
	}

	return nil
}

// CallTool invokes the MCP tools/call method for toolName.
func CallTool(ctx context.Context, server Server, toolName string, arguments map[string]any, timeout time.Duration) (*Response, error) {
	toolName = strings.TrimSpace(toolName)
	if toolName == "" {
		return nil, errors.New("call mcp tool: missing tool name")
	}

	return Invoke(ctx, server, Request{
		Method: "tools/call",
		Params: CallToolParams{
			Name:      toolName,
			Arguments: arguments,
		},
	}, timeout)
}

// Validate checks the server fields required to invoke a configured MCP server.
func (s Server) Validate() error {
	name := strings.TrimSpace(s.Name)
	if name == "" {
		return errors.New("missing name")
	}

	if strings.TrimSpace(s.Command) == "" {
		return fmt.Errorf("server %q: missing command", name)
	}

	if err := validateCapabilities(name, s.Capabilities); err != nil {
		return err
	}

	return nil
}

// Validate checks the request fields required for JSON-RPC invocation.
func (r Request) Validate() error {
	if strings.TrimSpace(r.Method) == "" {
		return errors.New("missing method")
	}

	return nil
}

func writeRequest(w io.WriteCloser, request Request) error {
	defer w.Close()

	encoded, err := json.Marshal(request)
	if err != nil {
		return fmt.Errorf("marshal json-rpc request: %w", err)
	}

	encoded = append(encoded, '\n')
	if _, err := w.Write(encoded); err != nil {
		return fmt.Errorf("write newline-delimited json: %w", err)
	}

	return nil
}

func readResponse(r io.Reader, wantID any) <-chan responseResult {
	ch := make(chan responseResult, 1)

	go func() {
		defer close(ch)

		scanner := bufio.NewScanner(r)
		scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)

		for scanner.Scan() {
			line := bytes.TrimSpace(scanner.Bytes())
			if len(line) == 0 {
				continue
			}

			var response Response
			if err := json.Unmarshal(line, &response); err != nil {
				ch <- responseResult{err: fmt.Errorf("decode newline-delimited json response: %w", err)}
				return
			}

			if sameJSONValue(response.ID, wantID) {
				ch <- responseResult{response: &response}
				return
			}
		}

		if err := scanner.Err(); err != nil {
			ch <- responseResult{err: fmt.Errorf("scan response: %w", err)}
			return
		}

		ch <- responseResult{err: io.ErrUnexpectedEOF}
	}()

	return ch
}

func readAll(r io.Reader) <-chan string {
	ch := make(chan string, 1)

	go func() {
		data, err := io.ReadAll(r)
		if err != nil {
			ch <- string(data)
			return
		}

		ch <- string(data)
	}()

	return ch
}

func waitFor(cmd *exec.Cmd) <-chan error {
	ch := make(chan error, 1)

	go func() {
		ch <- cmd.Wait()
	}()

	return ch
}

func finishProcess(ctx context.Context, cmd *exec.Cmd, waitCh <-chan error) (bool, error) {
	select {
	case err := <-waitCh:
		return false, err
	case <-ctx.Done():
		return false, <-waitCh
	default:
		// MCP servers are normally long-running. This primitive is one request per
		// process, so stop the helper after its response has been read.
		if cmd.Process != nil {
			if err := cmd.Process.Kill(); err != nil && !errors.Is(err, os.ErrProcessDone) {
				return false, fmt.Errorf("kill mcp server process: %w", err)
			}
		}

		return true, <-waitCh
	}
}

type responseResult struct {
	response *Response
	err      error
}

func sameJSONValue(got, want any) bool {
	gotJSON, gotErr := json.Marshal(got)
	wantJSON, wantErr := json.Marshal(want)

	return gotErr == nil && wantErr == nil && bytes.Equal(gotJSON, wantJSON)
}

func withProcessOutput(err error, stderr string) error {
	if stderr == "" {
		return err
	}

	return fmt.Errorf("%w: stderr: %s", err, stderr)
}
