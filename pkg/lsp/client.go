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

// DocumentSymbols starts an external language server, opens Options.FilePath,
// requests textDocument/documentSymbol, normalizes the result, and shuts down.
//
//nolint:cyclop // The one-shot LSP lifecycle is deliberately linear and explicit.
func DocumentSymbols(ctx context.Context, opts Options) ([]Symbol, error) {
	if err := requireContext(ctx, "symbols"); err != nil {
		return nil, err
	}

	if err := validateOptions(opts); err != nil {
		return nil, err
	}

	filePath, err := filepath.Abs(opts.FilePath)
	if err != nil {
		return nil, fmt.Errorf("resolve file path: %w", err)
	}

	content, err := os.ReadFile(filePath)
	if err != nil {
		return nil, fmt.Errorf("read file %q: %w", filePath, err)
	}

	rootPath := opts.RootPath
	if rootPath == "" {
		rootPath = filepath.Dir(filePath)
	}

	rootPath, err = filepath.Abs(rootPath)
	if err != nil {
		return nil, fmt.Errorf("resolve root path: %w", err)
	}

	client, err := startClient(ctx, opts.Command, opts.Args, opts.Env)
	if err != nil {
		return nil, err
	}
	defer client.close()

	documentURI := fileURI(filePath)
	rootURI := fileURI(rootPath)

	languageID := opts.LanguageID
	if languageID == "" {
		languageID = inferLanguageID(filePath)
	}

	if _, requestErr := client.request(ctx, "initialize", initializeParams{ProcessID: os.Getpid(), RootURI: rootURI, Capabilities: map[string]any{}}); requestErr != nil {
		return nil, fmt.Errorf("initialize language server: %w", requestErr)
	}

	if notifyErr := client.notify(ctx, "initialized", map[string]any{}); notifyErr != nil {
		return nil, fmt.Errorf("send initialized notification: %w", notifyErr)
	}

	if notifyErr := client.notify(ctx, "textDocument/didOpen", didOpenParams{TextDocument: textDocumentItem{URI: documentURI, LanguageID: languageID, Version: 1, Text: string(content)}}); notifyErr != nil {
		return nil, fmt.Errorf("open document: %w", notifyErr)
	}

	raw, err := client.request(ctx, "textDocument/documentSymbol", documentSymbolParams{TextDocument: textDocumentIdentifier{URI: documentURI}})
	if err != nil {
		return nil, fmt.Errorf("request document symbols: %w", err)
	}

	symbols, err := normalizeSymbols(raw)
	if err != nil {
		return nil, fmt.Errorf("normalize document symbols: %w", err)
	}

	if _, requestErr := client.request(ctx, "shutdown", nil); requestErr != nil {
		return nil, fmt.Errorf("shutdown language server: %w", requestErr)
	}

	if notifyErr := client.notify(ctx, "exit", nil); notifyErr != nil {
		return nil, fmt.Errorf("send exit notification: %w", notifyErr)
	}

	return symbols, nil
}

// WorkspaceSymbols starts an external language server, requests workspace/symbol
// for query, normalizes SymbolInformation results, and shuts down.
func WorkspaceSymbols(ctx context.Context, opts Options, query string) ([]Symbol, error) {
	if err := requireContext(ctx, "workspace symbols"); err != nil {
		return nil, err
	}

	if err := validateWorkspaceOptions(opts); err != nil {
		return nil, err
	}

	rootPath := opts.RootPath
	if rootPath == "" {
		var err error

		rootPath, err = os.Getwd()
		if err != nil {
			return nil, fmt.Errorf("resolve current directory: %w", err)
		}
	}

	rootPath, err := filepath.Abs(rootPath)
	if err != nil {
		return nil, fmt.Errorf("resolve root path: %w", err)
	}

	client, err := startClient(ctx, opts.Command, opts.Args, opts.Env)
	if err != nil {
		return nil, err
	}
	defer client.close()

	rootURI := fileURI(rootPath)
	if _, requestErr := client.request(ctx, "initialize", initializeParams{ProcessID: os.Getpid(), RootURI: rootURI, Capabilities: map[string]any{}}); requestErr != nil {
		return nil, fmt.Errorf("initialize language server: %w", requestErr)
	}

	if notifyErr := client.notify(ctx, "initialized", map[string]any{}); notifyErr != nil {
		return nil, fmt.Errorf("send initialized notification: %w", notifyErr)
	}

	raw, err := client.request(ctx, "workspace/symbol", workspaceSymbolParams{Query: query})
	if err != nil {
		return nil, fmt.Errorf("request workspace symbols: %w", err)
	}

	symbols, err := normalizeSymbols(raw)
	if err != nil {
		return nil, fmt.Errorf("normalize workspace symbols: %w", err)
	}

	if _, requestErr := client.request(ctx, "shutdown", nil); requestErr != nil {
		return nil, fmt.Errorf("shutdown language server: %w", requestErr)
	}

	if notifyErr := client.notify(ctx, "exit", nil); notifyErr != nil {
		return nil, fmt.Errorf("send exit notification: %w", notifyErr)
	}

	return symbols, nil
}

func requireContext(ctx context.Context, scope string) error {
	if ctx == nil {
		return fmt.Errorf("lsp %s: context is required", scope)
	}

	if err := ctx.Err(); err != nil {
		return fmt.Errorf("lsp %s: context already done: %w", scope, err)
	}

	return nil
}

func validateOptions(opts Options) error {
	if err := validateWorkspaceOptions(opts); err != nil {
		return err
	}

	if opts.FilePath == "" {
		return errors.New("file path is required")
	}

	return nil
}

func validateWorkspaceOptions(opts Options) error {
	if opts.Command == "" {
		return errors.New("lsp command is required")
	}

	return nil
}

//nolint:govet // Field order groups process, write lock, pending requests, and lifecycle.
type rpcClient struct {
	cmd     *exec.Cmd
	stdin   io.WriteCloser
	writeMu sync.Mutex

	pendingMu sync.Mutex
	pending   map[int]chan rpcResponse
	nextID    int
	done      chan error
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

func startClient(ctx context.Context, command string, args, env []string) (*rpcClient, error) {
	cmd := exec.CommandContext(ctx, command, args...)

	cmd.Env = append(os.Environ(), env...)

	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, fmt.Errorf("open language server stdin: %w", err)
	}

	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, fmt.Errorf("open language server stdout: %w", err)
	}

	cmd.Stderr = os.Stderr
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("start language server %q: %w", command, err)
	}

	client := &rpcClient{cmd: cmd, stdin: stdin, pending: make(map[int]chan rpcResponse), done: make(chan error, 1)}
	go client.readLoop(stdout)

	return client, nil
}

func (c *rpcClient) request(ctx context.Context, method string, params any) (json.RawMessage, error) {
	id, ch := c.registerRequest()
	if err := c.write(ctx, rpcMessage{JSONRPC: "2.0", ID: id, Method: method, Params: params}); err != nil {
		c.unregisterRequest(id)
		return nil, err
	}

	select {
	case response := <-ch:
		if response.Error != nil {
			return nil, response.Error
		}

		return response.Result, nil
	case err := <-c.done:
		if err == nil {
			err = io.EOF
		}

		return nil, fmt.Errorf("language server stopped before %s response: %w", method, err)
	case <-ctx.Done():
		c.unregisterRequest(id)
		return nil, fmt.Errorf("wait for %s response: %w", method, ctx.Err())
	}
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
		return fmt.Errorf("write json-rpc message: %w", err)
	}

	return nil
}

func (c *rpcClient) readLoop(reader io.Reader) {
	buffered := bufio.NewReader(reader)
	for {
		payload, err := readFrame(buffered)
		if err != nil {
			c.done <- err
			return
		}

		var message rpcMessage
		if err := json.Unmarshal(payload, &message); err != nil {
			c.done <- fmt.Errorf("decode json-rpc response: %w", err)
			return
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

func (c *rpcClient) close() {
	_ = c.stdin.Close()
	if c.cmd.Process != nil {
		if err := c.cmd.Process.Kill(); err != nil && !errors.Is(err, os.ErrProcessDone) {
			return
		}
	}

	if err := c.cmd.Wait(); err != nil {
		return
	}
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

//nolint:govet // JSON field order mirrors LSP initialize params.
type initializeParams struct {
	ProcessID    int            `json:"processId"`
	RootURI      string         `json:"rootUri"`
	Capabilities map[string]any `json:"capabilities"`
}

//nolint:govet // JSON field order mirrors LSP TextDocumentItem.
type textDocumentItem struct {
	URI        string `json:"uri"`
	LanguageID string `json:"languageId"`
	Version    int    `json:"version"`
	Text       string `json:"text"`
}

type didOpenParams struct {
	TextDocument textDocumentItem `json:"textDocument"`
}

type textDocumentIdentifier struct {
	URI string `json:"uri"`
}

type documentSymbolParams struct {
	TextDocument textDocumentIdentifier `json:"textDocument"`
}

type workspaceSymbolParams struct {
	Query string `json:"query"`
}
