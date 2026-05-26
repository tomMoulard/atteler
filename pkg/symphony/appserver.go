//nolint:gocognit,gocritic,gosec,govet,wrapcheck,wsl_v5,errcheck,perfsprint,misspell // The app-server transport mirrors an external JSONL protocol with several lifecycle branches.
package symphony

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os/exec"
	"strings"
	"sync"
	"time"

	"github.com/tommoulard/atteler/pkg/shell"
)

// AppServerClient speaks Codex app-server JSONL over stdio.
type AppServerClient struct {
	stdin  io.WriteCloser
	cmd    *exec.Cmd
	emit   func(CodexEvent)
	lines  chan appServerMessage
	done   chan error
	stderr <-chan string
	mu     sync.Mutex
	nextID int64
	pid    string
}

type appServerMessage struct {
	Result json.RawMessage `json:"result,omitempty"`
	Params json.RawMessage `json:"params,omitempty"`
	Error  *appServerError `json:"error,omitempty"`
	ID     any             `json:"id,omitempty"`
	Method string          `json:"method,omitempty"`
	raw    json.RawMessage
}

type appServerError struct {
	Data    json.RawMessage `json:"data,omitempty"`
	Message string          `json:"message"`
	Code    int64           `json:"code"`
}

func (e *appServerError) Error() string {
	if e == nil {
		return ""
	}

	return fmt.Sprintf("response_error: %d: %s", e.Code, e.Message)
}

// StartAppServer launches the configured Codex app-server command.
func StartAppServer(ctx context.Context, cfg CodexConfig, workspacePath string, emit func(CodexEvent)) (*AppServerClient, error) {
	return startAppServer(ctx, cfg, workspacePath, shell.AuditContext{Caller: "symphony.codex_app_server"}, emit)
}

// StartAppServerForIssue launches the configured Codex app-server command and
// ties its audit records to the issue currently being worked.
func StartAppServerForIssue(ctx context.Context, cfg CodexConfig, issue Issue, workspacePath string, emit func(CodexEvent)) (*AppServerClient, error) {
	return startAppServer(ctx, cfg, workspacePath, shell.AuditContext{
		Caller:          "symphony.codex_app_server",
		IssueID:         issue.ID,
		IssueIdentifier: issue.Identifier,
	}, emit)
}

func startAppServer(ctx context.Context, cfg CodexConfig, workspacePath string, audit shell.AuditContext, emit func(CodexEvent)) (*AppServerClient, error) {
	if err := requireAppServerContext(ctx); err != nil {
		return nil, err
	}

	command := strings.TrimSpace(cfg.Command)
	if command == "" {
		command = defaultCodexCommand
	}

	if strings.TrimSpace(audit.Caller) == "" {
		audit.Caller = "symphony.codex_app_server"
	}

	cmd, invocation, err := shell.CommandContext(ctx, shell.CommandOptions{
		Program: "bash",
		Args:    []string{"--noprofile", "--norc", "-lc", command},
		Command: command,
		Dir:     workspacePath,
		Mode:    shell.ModeStreaming,
		Audit:   audit,
	})
	if err != nil {
		return nil, fmt.Errorf("codex_not_found: authorize %q: %w", command, err)
	}

	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, fmt.Errorf("codex_not_found: open stdin: %w", finishAppServerSetupError(invocation, err))
	}

	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, fmt.Errorf("codex_not_found: open stdout: %w", finishAppServerSetupError(invocation, err))
	}

	stderr, err := cmd.StderrPipe()
	if err != nil {
		return nil, fmt.Errorf("codex_not_found: open stderr: %w", finishAppServerSetupError(invocation, err))
	}

	if err := cmd.Start(); err != nil {
		if finishErr := invocation.Finish(shell.FinishOptions{Error: err, OutputCapture: shell.OutputNotCaptured, OutputNote: "process failed before stdio protocol output capture"}); finishErr != nil {
			return nil, fmt.Errorf("codex_not_found: start %q: %w", command, errors.Join(err, finishErr))
		}

		return nil, fmt.Errorf("codex_not_found: start %q: %w", command, err)
	}

	client := &AppServerClient{
		stdin:  stdin,
		cmd:    cmd,
		emit:   emit,
		lines:  make(chan appServerMessage, 64),
		done:   make(chan error, 1),
		stderr: readString(stderr),
		nextID: 1,
	}
	if cmd.Process != nil {
		client.pid = fmt.Sprint(cmd.Process.Pid)
	}

	go client.readLoop(stdout)
	go func() {
		err := cmd.Wait()
		if finishErr := invocation.Finish(shell.FinishOptions{Error: err, OutputCapture: shell.OutputNotCaptured, OutputNote: "stdio protocol streamed to Symphony app-server client"}); finishErr != nil && err == nil {
			err = finishErr
		}
		client.done <- err
	}()

	if err := client.initialize(ctx, cfg); err != nil {
		client.Close()
		return nil, err
	}

	return client, nil
}

func requireAppServerContext(ctx context.Context) error {
	if ctx == nil {
		return errors.New("codex app-server: context is required")
	}

	if err := ctx.Err(); err != nil {
		return fmt.Errorf("codex app-server: context already done: %w", err)
	}

	return nil
}

func finishAppServerSetupError(invocation *shell.Invocation, err error) error {
	if finishErr := invocation.Finish(shell.FinishOptions{
		Error:         err,
		OutputCapture: shell.OutputNotCaptured,
		OutputNote:    "process failed before stdio protocol output capture",
	}); finishErr != nil {
		return errors.Join(err, finishErr)
	}

	return err
}

// Close stops the app-server subprocess.
func (c *AppServerClient) Close() error {
	if c == nil {
		return nil
	}

	_ = c.stdin.Close()
	if c.cmd != nil && c.cmd.Process != nil {
		_ = c.cmd.Process.Kill()
	}

	return nil
}

// StartThread starts a Codex app-server thread and returns the thread ID.
func (c *AppServerClient) StartThread(ctx context.Context, cfg Config, issue Issue, workspacePath string) (string, error) {
	if err := requireAppServerContext(ctx); err != nil {
		return "", err
	}

	params := map[string]any{
		"cwd":                   workspacePath,
		"runtimeWorkspaceRoots": []string{workspacePath},
		"ephemeral":             true,
		"serviceName":           symphonyServiceName,
		"baseInstructions":      "You are running inside Symphony, a scheduler for issue-driven coding-agent work. Work only inside the configured workspace and follow the issue prompt.",
		"developerInstructions": fmt.Sprintf("Current Symphony issue: %s: %s", issue.Identifier, issue.Title),
		"config":                nil,
	}

	if cfg.Codex.ExtraConfig != nil {
		params["config"] = cfg.Codex.ExtraConfig
	}

	if cfg.Codex.ApprovalPolicy != nil {
		params["approvalPolicy"] = cfg.Codex.ApprovalPolicy
	}

	if cfg.Codex.ThreadSandbox != nil {
		params["sandbox"] = cfg.Codex.ThreadSandbox
	}

	result, err := c.call(ctx, "thread/start", params, cfg.Codex.ReadTimeout)
	if err != nil {
		return "", err
	}

	var response struct {
		Thread struct {
			ID string `json:"id"`
		} `json:"thread"`
	}
	if err := json.Unmarshal(result, &response); err != nil {
		return "", fmt.Errorf("response_error: decode thread/start: %w", err)
	}

	if response.Thread.ID == "" {
		return "", errors.New("response_error: thread/start returned empty thread id")
	}

	c.emitEvent(CodexEvent{
		Event:        "session_started",
		ThreadID:     response.Thread.ID,
		AppServerPID: c.pid,
		Message:      issue.Identifier + ": " + issue.Title,
	})

	return response.Thread.ID, nil
}

// RunTurn starts a turn and streams app-server events until it completes.
func (c *AppServerClient) RunTurn(ctx context.Context, cfg Config, threadID, prompt, workspacePath string) error {
	params := map[string]any{
		"threadId":              threadID,
		"cwd":                   workspacePath,
		"runtimeWorkspaceRoots": []string{workspacePath},
		"input": []map[string]any{
			{
				"type": "text",
				"text": prompt,
			},
		},
	}

	if cfg.Codex.ApprovalPolicy != nil {
		params["approvalPolicy"] = cfg.Codex.ApprovalPolicy
	}

	if cfg.Codex.TurnSandboxPolicy != nil {
		params["sandboxPolicy"] = cfg.Codex.TurnSandboxPolicy
	}

	result, err := c.call(ctx, "turn/start", params, cfg.Codex.ReadTimeout)
	if err != nil {
		return err
	}

	var response struct {
		Turn struct {
			ID     string `json:"id"`
			Status string `json:"status"`
		} `json:"turn"`
	}
	if err := json.Unmarshal(result, &response); err != nil {
		return fmt.Errorf("response_error: decode turn/start: %w", err)
	}

	if response.Turn.ID == "" {
		return errors.New("response_error: turn/start returned empty turn id")
	}

	c.emitEvent(CodexEvent{
		Event:        "turn_started",
		ThreadID:     threadID,
		TurnID:       response.Turn.ID,
		SessionID:    threadID + "-" + response.Turn.ID,
		AppServerPID: c.pid,
	})

	timeout := cfg.Codex.TurnTimeout
	if timeout <= 0 {
		timeout = defaultCodexTurnTimeout
	}

	turnCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	for {
		select {
		case <-turnCtx.Done():
			if errors.Is(turnCtx.Err(), context.DeadlineExceeded) {
				c.emitEvent(CodexEvent{Event: "turn_failed", ThreadID: threadID, TurnID: response.Turn.ID, SessionID: threadID + "-" + response.Turn.ID, AppServerPID: c.pid, Message: "turn_timeout"})
				return fmt.Errorf("turn_timeout: %w", turnCtx.Err())
			}

			return turnCtx.Err()
		case err := <-c.done:
			return withStderr(fmt.Errorf("port_exit: %w", err), c.stderr)
		case msg, ok := <-c.lines:
			if !ok {
				return fmt.Errorf("port_exit: %w", io.ErrUnexpectedEOF)
			}

			done, err := c.handleMessageDuringTurn(turnCtx, msg, threadID, response.Turn.ID)
			if err != nil {
				return err
			}

			if done {
				return nil
			}
		}
	}
}

func (c *AppServerClient) initialize(ctx context.Context, cfg CodexConfig) error {
	params := map[string]any{
		"clientInfo": map[string]any{
			"name":    symphonyServiceName,
			"title":   "Atteler Symphony",
			"version": "dev",
		},
		"capabilities": map[string]any{
			"experimentalApi": true,
		},
	}

	if _, err := c.call(ctx, "initialize", params, cfg.ReadTimeout); err != nil {
		return err
	}

	return c.notify(ctx, "initialized", nil)
}

func (c *AppServerClient) call(ctx context.Context, method string, params any, timeout time.Duration) (json.RawMessage, error) {
	if err := requireAppServerContext(ctx); err != nil {
		return nil, err
	}

	if timeout <= 0 {
		timeout = defaultCodexReadTimeout
	}

	callCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	id := c.nextRequestID()
	if err := c.writeContext(callCtx, appServerMessage{ID: id, Method: method, Params: mustRaw(params)}); err != nil {
		return nil, err
	}

	for {
		select {
		case <-callCtx.Done():
			return nil, fmt.Errorf("response_timeout: %s: %w", method, callCtx.Err())
		case err := <-c.done:
			return nil, withStderr(fmt.Errorf("port_exit: %w", err), c.stderr)
		case msg, ok := <-c.lines:
			if !ok {
				return nil, fmt.Errorf("port_exit: %w", io.ErrUnexpectedEOF)
			}

			if msg.ID != nil && sameRequestID(msg.ID, id) {
				if msg.Error != nil {
					return nil, msg.Error
				}

				return msg.Result, nil
			}

			if err := c.handleOutOfBand(callCtx, msg); err != nil {
				return nil, err
			}
		}
	}
}

func (c *AppServerClient) notify(ctx context.Context, method string, params any) error {
	return c.writeContext(ctx, appServerMessage{Method: method, Params: mustRaw(params)})
}

func (c *AppServerClient) handleMessageDuringTurn(ctx context.Context, msg appServerMessage, threadID, turnID string) (bool, error) {
	if msg.Method == "" && msg.ID != nil {
		return false, nil
	}

	if msg.ID != nil && msg.Method != "" {
		return false, c.handleServerRequest(ctx, msg)
	}

	c.emitNotification(msg, threadID, turnID)
	switch msg.Method {
	case "turn/completed":
		status, errMessage := turnCompletionStatus(msg.Params)
		switch status {
		case "completed":
			c.emitEvent(CodexEvent{Event: "turn_completed", ThreadID: threadID, TurnID: turnID, SessionID: threadID + "-" + turnID, AppServerPID: c.pid})
			return true, nil
		case "interrupted":
			return false, errors.New("turn_cancelled")
		case "failed":
			if errMessage == "" {
				errMessage = "turn_failed"
			}

			return false, fmt.Errorf("turn_failed: %s", errMessage)
		default:
			return true, nil
		}
	case "error":
		return false, fmt.Errorf("turn_ended_with_error: %s", summarizeRaw(msg.Params))
	default:
		return false, nil
	}
}

func (c *AppServerClient) handleOutOfBand(ctx context.Context, msg appServerMessage) error {
	if msg.ID != nil && msg.Method != "" {
		return c.handleServerRequest(ctx, msg)
	}

	c.emitNotification(msg, "", "")
	return nil
}

func (c *AppServerClient) handleServerRequest(ctx context.Context, msg appServerMessage) error {
	switch msg.Method {
	case "item/commandExecution/requestApproval", "execCommandApproval":
		c.emitEvent(CodexEvent{Event: "approval_auto_approved", AppServerPID: c.pid, Payload: msg.raw, Message: msg.Method})
		return c.respond(ctx, msg.ID, map[string]any{"decision": "acceptForSession"})
	case "item/fileChange/requestApproval", "applyPatchApproval":
		c.emitEvent(CodexEvent{Event: "approval_auto_approved", AppServerPID: c.pid, Payload: msg.raw, Message: msg.Method})
		return c.respond(ctx, msg.ID, map[string]any{"decision": "acceptForSession"})
	case "item/permissions/requestApproval":
		c.emitEvent(CodexEvent{Event: "approval_auto_approved", AppServerPID: c.pid, Payload: msg.raw, Message: msg.Method})
		return c.respond(ctx, msg.ID, map[string]any{
			"permissions": map[string]any{
				"fileSystem": map[string]any{},
				"network":    map[string]any{"enabled": true},
			},
			"scope": "session",
		})
	case "item/tool/call":
		c.emitEvent(CodexEvent{Event: "unsupported_tool_call", AppServerPID: c.pid, Payload: msg.raw, Message: msg.Method})
		return c.respond(ctx, msg.ID, map[string]any{
			"success": false,
			"contentItems": []map[string]string{
				{"type": "inputText", "text": "Unsupported Symphony client-side tool."},
			},
		})
	case "item/tool/requestUserInput", "mcpServer/elicitation/request":
		c.emitEvent(CodexEvent{Event: "turn_input_required", AppServerPID: c.pid, Payload: msg.raw, Message: msg.Method})
		return errors.New("turn_input_required")
	default:
		c.emitEvent(CodexEvent{Event: "other_message", AppServerPID: c.pid, Payload: msg.raw, Message: msg.Method})
		return c.respondError(ctx, msg.ID, -32601, "unsupported client request: "+msg.Method)
	}
}

func (c *AppServerClient) respond(ctx context.Context, id any, result any) error {
	return c.writeContext(ctx, appServerMessage{ID: id, Result: mustRaw(result)})
}

func (c *AppServerClient) respondError(ctx context.Context, id any, code int64, message string) error {
	return c.writeContext(ctx, appServerMessage{
		ID: id,
		Error: &appServerError{
			Code:    code,
			Message: message,
		},
	})
}

func (c *AppServerClient) emitNotification(msg appServerMessage, fallbackThreadID, fallbackTurnID string) {
	if msg.Method == "" {
		c.emitEvent(CodexEvent{Event: "other_message", AppServerPID: c.pid, Payload: msg.raw})
		return
	}

	event := CodexEvent{
		Event:        "notification",
		Timestamp:    time.Now().UTC(),
		Payload:      msg.raw,
		ThreadID:     fallbackThreadID,
		TurnID:       fallbackTurnID,
		AppServerPID: c.pid,
		Message:      msg.Method,
	}

	switch msg.Method {
	case "thread/tokenUsage/updated":
		if usage := parseTokenUsage(msg.Params); usage != nil {
			event.Usage = usage
			event.InputTokens = usage.InputTokens
			event.OutputTokens = usage.OutputTokens
			event.TotalTokens = usage.TotalTokens
		}
	case "account/rateLimits/updated":
		event.RateLimits = msg.Params
	case "agentMessage/delta":
		event.Message = summarizeRaw(msg.Params)
	}

	if ids := parseThreadTurnIDs(msg.Params); ids.threadID != "" || ids.turnID != "" {
		event.ThreadID = firstNonEmpty(ids.threadID, event.ThreadID)
		event.TurnID = firstNonEmpty(ids.turnID, event.TurnID)
	}

	if event.ThreadID != "" && event.TurnID != "" {
		event.SessionID = event.ThreadID + "-" + event.TurnID
	}

	c.emitEvent(event)
}

func (c *AppServerClient) emitEvent(event CodexEvent) {
	if c == nil || c.emit == nil {
		return
	}

	if event.Timestamp.IsZero() {
		event.Timestamp = time.Now().UTC()
	}

	if event.AppServerPID == "" {
		event.AppServerPID = c.pid
	}

	c.emit(event)
}

func (c *AppServerClient) nextRequestID() int64 {
	c.mu.Lock()
	defer c.mu.Unlock()

	id := c.nextID
	c.nextID++

	return id
}

func (c *AppServerClient) writeContext(ctx context.Context, msg appServerMessage) error {
	if err := requireAppServerContext(ctx); err != nil {
		return err
	}

	return c.write(msg)
}

func (c *AppServerClient) write(msg appServerMessage) error {
	c.mu.Lock()
	defer c.mu.Unlock()

	wire := map[string]any{}
	if msg.ID != nil {
		wire["id"] = msg.ID
	}

	if msg.Method != "" {
		wire["method"] = msg.Method
	}

	if msg.Params != nil {
		var params any
		if err := json.Unmarshal(msg.Params, &params); err != nil {
			return fmt.Errorf("response_error: decode params: %w", err)
		}

		wire["params"] = params
	}

	if msg.Result != nil {
		var result any
		if err := json.Unmarshal(msg.Result, &result); err != nil {
			return fmt.Errorf("response_error: decode result: %w", err)
		}

		wire["result"] = result
	}

	if msg.Error != nil {
		wire["error"] = msg.Error
	}

	data, err := json.Marshal(wire)
	if err != nil {
		return fmt.Errorf("response_error: marshal app-server message: %w", err)
	}

	data = append(data, '\n')
	if _, err := c.stdin.Write(data); err != nil {
		return fmt.Errorf("port_exit: write app-server stdin: %w", err)
	}

	return nil
}

func (c *AppServerClient) readLoop(stdout io.Reader) {
	defer close(c.lines)

	scanner := bufio.NewScanner(stdout)
	scanner.Buffer(make([]byte, 0, 64*1024), maxAppServerProtocolLine)
	for scanner.Scan() {
		line := bytes.TrimSpace(scanner.Bytes())
		if len(line) == 0 {
			continue
		}

		var msg appServerMessage
		if err := json.Unmarshal(line, &msg); err != nil {
			c.emitEvent(CodexEvent{Event: "malformed", AppServerPID: c.pid, Payload: append([]byte(nil), line...), Message: err.Error()})
			continue
		}

		msg.raw = append([]byte(nil), line...)
		c.lines <- msg
	}

	if err := scanner.Err(); err != nil {
		c.emitEvent(CodexEvent{Event: "malformed", AppServerPID: c.pid, Message: err.Error()})
	}
}

func mustRaw(value any) json.RawMessage {
	if value == nil {
		return nil
	}

	data, err := json.Marshal(value)
	if err != nil {
		return nil
	}

	return data
}

func readString(r io.Reader) <-chan string {
	ch := make(chan string, 1)
	go func() {
		data, _ := io.ReadAll(r)
		ch <- string(data)
	}()

	return ch
}

func withStderr(err error, stderr <-chan string) error {
	if stderr == nil {
		return err
	}

	select {
	case text := <-stderr:
		text = strings.TrimSpace(text)
		if text != "" {
			return fmt.Errorf("%w: %s", err, text)
		}
	default:
	}

	return err
}

func sameRequestID(a any, b int64) bool {
	switch typed := a.(type) {
	case float64:
		return int64(typed) == b
	case int64:
		return typed == b
	case int:
		return int64(typed) == b
	case string:
		return typed == strconvFormatInt(b)
	default:
		return fmt.Sprint(a) == strconvFormatInt(b)
	}
}

func strconvFormatInt(value int64) string {
	return fmt.Sprintf("%d", value)
}

func turnCompletionStatus(raw json.RawMessage) (string, string) {
	var payload struct {
		Turn struct {
			Status string `json:"status"`
			Error  *struct {
				Message string `json:"message"`
			} `json:"error"`
		} `json:"turn"`
	}

	if err := json.Unmarshal(raw, &payload); err != nil {
		return "", err.Error()
	}

	message := ""
	if payload.Turn.Error != nil {
		message = payload.Turn.Error.Message
	}

	return payload.Turn.Status, message
}

func parseTokenUsage(raw json.RawMessage) *TokenUsage {
	var payload struct {
		TokenUsage struct {
			Total struct {
				InputTokens  int64 `json:"inputTokens"`
				OutputTokens int64 `json:"outputTokens"`
				TotalTokens  int64 `json:"totalTokens"`
			} `json:"total"`
		} `json:"tokenUsage"`
	}

	if err := json.Unmarshal(raw, &payload); err != nil {
		return nil
	}

	return &TokenUsage{
		InputTokens:  payload.TokenUsage.Total.InputTokens,
		OutputTokens: payload.TokenUsage.Total.OutputTokens,
		TotalTokens:  payload.TokenUsage.Total.TotalTokens,
	}
}

type threadTurnIDs struct {
	threadID string
	turnID   string
}

func parseThreadTurnIDs(raw json.RawMessage) threadTurnIDs {
	var payload struct {
		ThreadID string `json:"threadId"`
		TurnID   string `json:"turnId"`
	}
	_ = json.Unmarshal(raw, &payload)

	return threadTurnIDs{threadID: payload.ThreadID, turnID: payload.TurnID}
}

func summarizeRaw(raw json.RawMessage) string {
	text := strings.TrimSpace(string(raw))
	if len(text) > 300 {
		return text[:300] + "..."
	}

	return text
}
