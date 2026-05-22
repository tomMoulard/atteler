//nolint:wsl_v5 // JSON-RPC framing code uses compact protocol state-machine branches.
package lsp

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
)

//nolint:govet // Field order groups process, write lock, pending requests, and lifecycle.
type rpcClient struct {
	cmd   *exec.Cmd
	stdin io.WriteCloser

	writeMu sync.Mutex

	pendingMu sync.Mutex
	pending   map[int]chan rpcResponse
	nextID    int

	doneOnce sync.Once
	done     chan struct{}
	doneMu   sync.Mutex
	doneErr  error

	waitOnce sync.Once
	waitDone chan struct{}
	waitErr  error

	handlerMu sync.RWMutex
	handler   func(string, json.RawMessage)

	stderr *diagnosticBuffer
	stdout *diagnosticBuffer
}

//nolint:govet // JSON field order mirrors JSON-RPC messages.
type rpcMessage struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      any             `json:"id,omitempty"`
	Method  string          `json:"method,omitempty"`
	Params  any             `json:"params,omitempty"`
	Result  json.RawMessage `json:"result,omitempty"`
	Error   *rpcError       `json:"error,omitempty"`
}

//nolint:govet // Result before Error mirrors JSON-RPC responses.
type rpcResponse struct {
	Result json.RawMessage
	Error  *rpcError
}

//nolint:govet // JSON field order mirrors JSON-RPC error objects.
type rpcError struct {
	Code    int             `json:"code"`
	Message string          `json:"message"`
	Data    json.RawMessage `json:"data,omitempty"`
}

func (e *rpcError) Error() string {
	return fmt.Sprintf("language server error %d: %s", e.Code, e.Message)
}

func startClient(
	startCtx context.Context,
	command string,
	args []string,
	env []string,
	maxDiagnosticBytes int,
) (*rpcClient, error) {
	select {
	case <-startCtx.Done():
		return nil, fmt.Errorf("start language server: %w", startCtx.Err())
	default:
	}

	cmd := exec.CommandContext(startCtx, command, args...)
	// A managed language server should outlive a single request context. Keep
	// CommandContext's pre-start cancellation check for startup timeouts while
	// leaving shutdown/force-close as the only process termination paths after
	// the server has started.
	cmd.Cancel = nil
	cmd.Env = append(os.Environ(), env...)

	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, fmt.Errorf("open language server stdin: %w", err)
	}

	stdoutPipe, err := cmd.StdoutPipe()
	if err != nil {
		return nil, fmt.Errorf("open language server stdout: %w", err)
	}

	secrets := secretValuesFromEnv(env)
	stderr := newDiagnosticBuffer(maxDiagnosticBytes, secrets)
	stdout := newDiagnosticBuffer(maxDiagnosticBytes, secrets)
	cmd.Stderr = stderr

	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("start language server %q: %w", command, err)
	}

	client := &rpcClient{
		cmd:      cmd,
		stdin:    stdin,
		pending:  make(map[int]chan rpcResponse),
		done:     make(chan struct{}),
		waitDone: make(chan struct{}),
		stderr:   stderr,
		stdout:   stdout,
	}
	go client.readLoop(stdoutPipe)

	return client, nil
}

func (c *rpcClient) setNotificationHandler(handler func(string, json.RawMessage)) {
	c.handlerMu.Lock()
	c.handler = handler
	c.handlerMu.Unlock()
}

func (c *rpcClient) request(ctx context.Context, method string, params any) (json.RawMessage, error) {
	id, ch := c.registerRequest()
	if err := c.write(ctx, rpcMessage{JSONRPC: "2.0", ID: id, Method: method, Params: params}); err != nil {
		c.unregisterRequest(id)
		return nil, err
	}

	select {
	case response := <-ch:
		return response.result()
	case <-c.done:
		c.unregisterRequest(id)
		select {
		case response := <-ch:
			return response.result()
		default:
		}

		err := c.doneError()
		if err == nil {
			err = io.EOF
		}

		return nil, fmt.Errorf("%w before %s response: %w", errLanguageServerStopped, method, err)
	case <-ctx.Done():
		c.unregisterRequest(id)
		select {
		case response := <-ch:
			return response.result()
		default:
		}

		return nil, fmt.Errorf("wait for %s response: %w", method, ctx.Err())
	}
}

func (r rpcResponse) result() (json.RawMessage, error) {
	if r.Error != nil {
		return nil, r.Error
	}

	return r.Result, nil
}

func (c *rpcClient) notify(ctx context.Context, method string, params any) error {
	return c.write(ctx, rpcMessage{JSONRPC: "2.0", Method: method, Params: params})
}

func (c *rpcClient) registerRequest() (id int, ch chan rpcResponse) {
	c.pendingMu.Lock()
	defer c.pendingMu.Unlock()

	c.nextID++
	id = c.nextID
	ch = make(chan rpcResponse, 1)
	c.pending[id] = ch

	return id, ch
}

func (c *rpcClient) unregisterRequest(id int) {
	c.pendingMu.Lock()
	delete(c.pending, id)
	c.pendingMu.Unlock()
}

func (c *rpcClient) write(ctx context.Context, message rpcMessage) error {
	if !c.isRunning() {
		err := c.doneError()
		if err == nil {
			err = io.EOF
		}

		return fmt.Errorf("%w: %w", errLanguageServerStopped, err)
	}

	payload, err := json.Marshal(message)
	if err != nil {
		return fmt.Errorf("marshal json-rpc message: %w", err)
	}

	framed := fmt.Appendf(nil, "Content-Length: %d\r\n\r\n%s", len(payload), payload)

	c.writeMu.Lock()
	defer c.writeMu.Unlock()

	select {
	case <-ctx.Done():
		return fmt.Errorf("write json-rpc message: %w", ctx.Err())
	default:
	}

	if _, err := c.stdin.Write(framed); err != nil {
		return fmt.Errorf("%w: write json-rpc message: %w", errLanguageServerStopped, err)
	}

	return nil
}

func (c *rpcClient) readLoop(reader io.Reader) {
	buffered := bufio.NewReader(reader)
	for {
		payload, err := readFrame(buffered)
		if err != nil {
			c.stdout.WriteString(err.Error())
			c.finish(err)
			return
		}

		var message rpcMessage
		if err := json.Unmarshal(payload, &message); err != nil {
			c.stdout.WriteString(string(payload))
			c.finish(fmt.Errorf("decode json-rpc response: %w", err))
			return
		}

		if message.Method != "" {
			c.handleServerMessage(message)
			continue
		}

		id, ok := numericID(message.ID)
		if !ok {
			continue
		}

		c.pendingMu.Lock()
		ch := c.pending[id]
		delete(c.pending, id)
		c.pendingMu.Unlock()

		if ch != nil {
			ch <- rpcResponse{Result: message.Result, Error: message.Error}
		}
	}
}

func (c *rpcClient) handleServerMessage(message rpcMessage) {
	if message.ID != nil {
		if err := c.respondToServerRequest(message); err != nil {
			c.finish(err)
		}

		return
	}

	c.handleNotification(message)
}

func (c *rpcClient) respondToServerRequest(message rpcMessage) error {
	response := rpcMessage{JSONRPC: "2.0", ID: message.ID}
	switch message.Method {
	case "workspace/configuration":
		response.Result = json.RawMessage("[]")
	case "client/registerCapability", "client/unregisterCapability":
		response.Result = json.RawMessage(jsonNull)
	default:
		response.Error = &rpcError{Code: -32601, Message: "method not found"}
	}

	return c.writeServerResponse(response)
}

func (c *rpcClient) writeServerResponse(message rpcMessage) error {
	payload, err := json.Marshal(message)
	if err != nil {
		return fmt.Errorf("marshal json-rpc response: %w", err)
	}

	framed := fmt.Appendf(nil, "Content-Length: %d\r\n\r\n%s", len(payload), payload)
	c.writeMu.Lock()
	defer c.writeMu.Unlock()

	if _, err := c.stdin.Write(framed); err != nil {
		return fmt.Errorf("%w: write json-rpc response: %w", errLanguageServerStopped, err)
	}

	return nil
}

func (c *rpcClient) handleNotification(message rpcMessage) {
	var params json.RawMessage
	if message.Params != nil {
		raw, err := json.Marshal(message.Params)
		if err != nil {
			return
		}
		params = raw
	}

	c.handlerMu.RLock()
	handler := c.handler
	c.handlerMu.RUnlock()

	if handler != nil {
		go handler(message.Method, params)
	}
}

func (c *rpcClient) finish(err error) {
	c.doneOnce.Do(func() {
		c.doneMu.Lock()
		c.doneErr = err
		c.doneMu.Unlock()
		close(c.done)
	})
}

func (c *rpcClient) doneError() error {
	c.doneMu.Lock()
	defer c.doneMu.Unlock()

	return c.doneErr
}

func (c *rpcClient) isRunning() bool {
	select {
	case <-c.done:
		return false
	default:
		return true
	}
}

func (c *rpcClient) closeGraceful(ctx context.Context) error {
	_ = c.stdin.Close()

	if err := c.wait(ctx); err != nil {
		if errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled) {
			c.kill()
		}

		return fmt.Errorf("wait for language server exit: %w", err)
	}

	return nil
}

func (c *rpcClient) kill() {
	_ = c.stdin.Close()
	if c.cmd.Process != nil {
		if err := c.cmd.Process.Kill(); err != nil && !errors.Is(err, os.ErrProcessDone) {
			return
		}
	}

	<-c.waitDoneAfterStart()
}

func (c *rpcClient) wait(ctx context.Context) error {
	waitDone := c.waitDoneAfterStart()

	select {
	case <-waitDone:
		return c.waitErr
	case <-ctx.Done():
		return fmt.Errorf("wait for process: %w", ctx.Err())
	}
}

func (c *rpcClient) waitDoneAfterStart() <-chan struct{} {
	c.waitOnce.Do(func() {
		go func() {
			c.waitErr = c.cmd.Wait()
			close(c.waitDone)
		}()
	})

	return c.waitDone
}

func (c *rpcClient) stderrLog() (string, bool) {
	return c.stderr.String()
}

func (c *rpcClient) stdoutLog() (string, bool) {
	return c.stdout.String()
}

func numericID(id any) (int, bool) {
	switch v := id.(type) {
	case float64:
		return int(v), true
	case int:
		return v, true
	default:
		return 0, false
	}
}

func readFrame(reader io.Reader) ([]byte, error) {
	buffered, ok := reader.(*bufio.Reader)
	if !ok {
		buffered = bufio.NewReader(reader)
	}

	length := -1

	for {
		line, err := buffered.ReadString('\n')
		if err != nil {
			return nil, fmt.Errorf("read json-rpc header: %w", err)
		}

		line = strings.TrimSpace(line)
		if line == "" {
			break
		}

		parts := strings.SplitN(line, ":", 2)
		if len(parts) != 2 {
			return nil, fmt.Errorf("read json-rpc header: malformed header %q", line)
		}

		if !strings.EqualFold(parts[0], "Content-Length") {
			continue
		}

		parsed, err := strconv.Atoi(strings.TrimSpace(parts[1]))
		if err != nil {
			return nil, fmt.Errorf("parse content length %q: %w", strings.TrimSpace(parts[1]), err)
		}

		if parsed < 0 {
			return nil, fmt.Errorf("parse content length %q: negative length", strings.TrimSpace(parts[1]))
		}

		length = parsed
	}

	if length < 0 {
		return nil, errors.New("read json-rpc header: missing Content-Length")
	}

	payload := make([]byte, length)
	if _, err := io.ReadFull(buffered, payload); err != nil {
		return nil, fmt.Errorf("read json-rpc payload: %w", err)
	}

	return payload, nil
}

func writeFrame(writer io.Writer, payload []byte) error {
	_, err := fmt.Fprintf(writer, "Content-Length: %d\r\n\r\n%s", len(payload), payload)
	if err != nil {
		return fmt.Errorf("write json-rpc frame: %w", err)
	}

	return nil
}

func fileURI(path string) string {
	return (&url.URL{Scheme: "file", Path: filepath.ToSlash(path)}).String()
}

func inferLanguageID(path string) string {
	ext := strings.TrimPrefix(strings.ToLower(filepath.Ext(path)), ".")
	switch ext {
	case "go":
		return "go"
	case "js":
		return "javascript"
	case "ts":
		return "typescript"
	case "py":
		return "python"
	default:
		return ext
	}
}
