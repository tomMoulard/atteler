//nolint:gocognit,gocritic,gosec,govet,wrapcheck,wsl_v5,errcheck,perfsprint,misspell // The app-server transport mirrors an external JSONL protocol with several lifecycle branches.
package symphony

import (
	"bufio"
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os/exec"
	"strings"
	"sync"
	"time"

	"github.com/tommoulard/atteler/pkg/autonomy"
	"github.com/tommoulard/atteler/pkg/llm"
	"github.com/tommoulard/atteler/pkg/shell"
)

// AppServerClient speaks Codex app-server JSONL over stdio.
type AppServerClient struct {
	stdin        io.WriteCloser
	cmd          *exec.Cmd
	emit         func(CodexEvent)
	lines        chan appServerMessage
	done         chan error
	waitDone     chan struct{}
	quit         chan struct{}
	readDone     chan struct{}
	stderr       <-chan string
	commandLinks map[string]commandLink
	mu           sync.Mutex
	commandMu    sync.Mutex
	quitOnce     sync.Once
	nextID       int64
	pid          string
	autonomy     autonomy.Level
}

type commandLink struct {
	commandID string
	command   string
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
	return StartAppServerForIssueWithAutonomy(ctx, cfg, autonomy.DefaultLevel, issue, workspacePath, emit)
}

// StartAppServerForIssueWithAutonomy launches the configured Codex app-server
// command and records the selected autonomy in launch audit metadata.
func StartAppServerForIssueWithAutonomy(ctx context.Context, cfg CodexConfig, level autonomy.Level, issue Issue, workspacePath string, emit func(CodexEvent)) (*AppServerClient, error) {
	return startAppServer(ctx, cfg, workspacePath, shell.AuditContext{
		Caller:          "symphony.codex_app_server",
		IssueID:         issue.ID,
		IssueIdentifier: issue.Identifier,
		Autonomy:        autonomy.Normalize(level).String(),
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
	if strings.TrimSpace(audit.Autonomy) == "" {
		audit.Autonomy = autonomy.DefaultLevel.String()
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
		stdin:        stdin,
		cmd:          cmd,
		emit:         emit,
		lines:        make(chan appServerMessage, 64),
		done:         make(chan error, 1),
		waitDone:     make(chan struct{}),
		quit:         make(chan struct{}),
		readDone:     make(chan struct{}),
		stderr:       readString(stderr),
		commandLinks: make(map[string]commandLink),
		nextID:       1,
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
		close(client.waitDone)
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

// Close stops the app-server subprocess and releases the read loop, even when
// it is parked on a send into the full lines buffer.
func (c *AppServerClient) Close() error {
	if c == nil {
		return nil
	}

	if c.quit != nil {
		c.quitOnce.Do(func() { close(c.quit) })
	}

	_ = c.stdin.Close()
	if c.cmd != nil && c.cmd.Process != nil {
		_ = c.cmd.Process.Kill()
	}

	if c.waitDone != nil {
		select {
		case <-c.waitDone:
		case <-time.After(defaultCodexReadTimeout):
			return errors.New("codex app-server: close timeout")
		}
	}

	if c.readDone != nil {
		select {
		case <-c.readDone:
		case <-time.After(defaultCodexReadTimeout):
			return errors.New("codex app-server: read loop did not exit")
		}
	}

	return nil
}

// StartThread starts a Codex app-server thread and returns the thread ID.
func (c *AppServerClient) StartThread(ctx context.Context, cfg Config, issue Issue, workspacePath string) (string, error) {
	if err := requireAppServerContext(ctx); err != nil {
		return "", err
	}
	c.autonomy = autonomy.Normalize(cfg.Autonomy)

	params := map[string]any{
		"cwd":                   workspacePath,
		"runtimeWorkspaceRoots": []string{workspacePath},
		"ephemeral":             true,
		"serviceName":           symphonyServiceName,
		"baseInstructions":      "You are running inside Symphony, a scheduler for issue-driven coding-agent work. Work only inside the configured workspace and follow the issue prompt.",
		"developerInstructions": fmt.Sprintf("Current Symphony issue: %s: %s\nAutonomy: %s. Respect this risk-based capability boundary. Do not merge pull requests; final merge remains a human action.", issue.Identifier, issue.Title, c.autonomy.String()),
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
		Autonomy:     c.autonomy.String(),
	})

	return response.Thread.ID, nil
}

// RunTurn starts a turn and streams app-server events until it completes.
func (c *AppServerClient) RunTurn(ctx context.Context, cfg Config, threadID, prompt, workspacePath string) error {
	c.autonomy = autonomy.Normalize(cfg.Autonomy)

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
		Autonomy:     c.autonomy.String(),
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
		if autonomy.Normalize(c.autonomy) == autonomy.Low {
			return c.denyApproval(ctx, msg, "autonomy low is advisory-only and blocks command execution approvals")
		}

		command := extractCommandFromApproval(msg.Params)
		if command == "" {
			return c.denyApproval(ctx, msg, "command approval omitted a command; cannot evaluate against selected autonomy")
		}

		decision := llm.BashToolPolicyForAutonomy(c.autonomy)(ctx, llm.ToolCall{
			Name:  "bash",
			Input: map[string]any{"command": command},
		}, llm.AgentLoopBudgetSnapshot{})
		switch decision.Verdict {
		case llm.ToolPolicyDeny:
			return c.denyApproval(ctx, msg, decision.Reason)
		case llm.ToolPolicyRequireConfirm:
			return c.denyApproval(ctx, msg, decision.Reason+"; Symphony cannot auto-approve sensitive commands")
		}

		c.emitEvent(CodexEvent{Event: "approval_auto_approved", AppServerPID: c.pid, Payload: msg.raw, Message: msg.Method})
		return c.respond(ctx, msg.ID, map[string]any{"decision": "acceptForSession"})
	case "item/fileChange/requestApproval", "applyPatchApproval":
		if !autonomy.Normalize(c.autonomy).Allows(autonomy.ActionFileWrite) {
			return c.denyApproval(ctx, msg, autonomy.DenialMessage(c.autonomy, autonomy.ActionFileWrite, msg.Method))
		}

		c.emitEvent(CodexEvent{Event: "approval_auto_approved", AppServerPID: c.pid, Payload: msg.raw, Message: msg.Method})
		return c.respond(ctx, msg.ID, map[string]any{"decision": "acceptForSession"})
	case "item/permissions/requestApproval":
		action, detail := permissionApprovalAutonomyAction(msg.Params)
		if !autonomy.Normalize(c.autonomy).Allows(action) {
			return c.denyApproval(ctx, msg, autonomy.DenialMessage(c.autonomy, action, detail))
		}

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

func (c *AppServerClient) denyApproval(ctx context.Context, msg appServerMessage, reason string) error {
	reason = strings.TrimSpace(reason)
	if reason == "" {
		reason = "request exceeds selected autonomy"
	}

	c.emitEvent(CodexEvent{Event: "approval_denied", AppServerPID: c.pid, Payload: msg.raw, Message: reason})

	return c.respond(ctx, msg.ID, map[string]any{
		"decision": "deny",
		"message":  reason,
	})
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

func permissionApprovalAutonomyAction(raw json.RawMessage) (autonomy.Action, string) {
	fields := rawObject(raw)
	if jsonContainsKey(fields, "network") {
		return autonomy.ActionRemoteMutation, "network permission escalation"
	}

	if jsonContainsAnyKey(fields, "fileSystem", "filesystem", "file_system", "workspace", "write") {
		return autonomy.ActionFileWrite, "file-system permission escalation"
	}

	return autonomy.ActionRemoteMutation, "permission escalation"
}

func jsonContainsAnyKey(value any, keys ...string) bool {
	for _, key := range keys {
		if jsonContainsKey(value, key) {
			return true
		}
	}

	return false
}

func jsonContainsKey(value any, key string) bool {
	key = strings.ToLower(strings.TrimSpace(key))
	if key == "" {
		return false
	}

	switch typed := value.(type) {
	case map[string]any:
		for candidate, child := range typed {
			if strings.EqualFold(strings.TrimSpace(candidate), key) {
				return true
			}

			if jsonContainsKey(child, key) {
				return true
			}
		}
	case []any:
		for _, child := range typed {
			if jsonContainsKey(child, key) {
				return true
			}
		}
	}

	return false
}

func extractCommandFromApproval(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}

	var value any
	if err := json.Unmarshal(raw, &value); err != nil {
		return ""
	}

	return firstCommandString(value)
}

func firstCommandString(value any) string {
	switch typed := value.(type) {
	case map[string]any:
		for _, key := range []string{"command", "cmd", "commandLine", "command_line", "shellCommand", "shell_command", "argv", "args"} {
			if command := stringFromJSONValue(typed[key]); command != "" {
				return command
			}
		}

		for _, key := range []string{"item", "params", "request", "approval", "exec", "commandExecution"} {
			if command := firstCommandString(typed[key]); command != "" {
				return command
			}
		}
	case []any:
		var parts []string
		for _, item := range typed {
			switch nested := item.(type) {
			case string:
				if strings.TrimSpace(nested) != "" {
					parts = append(parts, nested)
				}
			default:
				if command := firstCommandString(nested); command != "" {
					return command
				}
			}
		}
		if len(parts) > 0 {
			return strings.Join(parts, " ")
		}
	case string:
		return strings.TrimSpace(typed)
	}

	return ""
}

func stringFromJSONValue(value any) string {
	switch typed := value.(type) {
	case string:
		return strings.TrimSpace(typed)
	case []any:
		parts := make([]string, 0, len(typed))
		for _, item := range typed {
			text, ok := item.(string)
			if !ok || strings.TrimSpace(text) == "" {
				return ""
			}
			parts = append(parts, text)
		}

		return strings.Join(parts, " ")
	default:
		return ""
	}
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

	if commandEvent := c.enrichCommandNotification(parseCommandNotification(msg.Method, msg.Params)); commandEvent.ok {
		event.Event = commandEvent.event
		event.Message = commandEvent.message
		event.CommandID = commandEvent.commandID
		event.ProcessID = commandEvent.processID
		event.Command = commandEvent.command
		event.OutputStream = commandEvent.stream
		event.OutputChunk = commandEvent.chunk
		event.ExitCode = commandEvent.exitCode
		event.ExitCodeSet = commandEvent.exitCodeSet
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

func (c *AppServerClient) enrichCommandNotification(notification commandNotification) commandNotification {
	if c == nil || !notification.ok || (notification.processID == "" && notification.commandID == "") {
		return notification
	}

	c.commandMu.Lock()
	defer c.commandMu.Unlock()

	c.rememberCommandLinkLocked(notification)

	if notification.processID != "" {
		notification = applyCommandLink(notification, c.commandLinks[notification.processID])
	}
	if notification.commandID != "" {
		notification = applyCommandLink(notification, c.commandLinks[notification.commandID])
	}

	return notification
}

func (c *AppServerClient) rememberCommandLinkLocked(notification commandNotification) {
	link := commandLink{}
	if notification.commandID != "" && notification.commandID != notification.processID {
		link.commandID = notification.commandID
	}
	if notification.command != "" {
		link.command = notification.command
	}
	if link.commandID == "" && link.command == "" {
		return
	}

	if c.commandLinks == nil {
		c.commandLinks = make(map[string]commandLink)
	}

	if notification.processID != "" {
		c.commandLinks[notification.processID] = mergeCommandLink(c.commandLinks[notification.processID], link)
	}
	if notification.commandID != "" {
		c.commandLinks[notification.commandID] = mergeCommandLink(c.commandLinks[notification.commandID], link)
	}
}

func mergeCommandLink(previous, next commandLink) commandLink {
	if next.commandID != "" {
		previous.commandID = next.commandID
	}
	if next.command != "" {
		previous.command = next.command
	}

	return previous
}

func applyCommandLink(notification commandNotification, link commandLink) commandNotification {
	if link.commandID != "" && (notification.commandID == "" || notification.commandID == notification.processID) {
		notification.commandID = link.commandID
	}
	if link.command != "" && notification.command == "" {
		notification.command = link.command
	}

	return notification
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
	if event.Autonomy == "" {
		event.Autonomy = autonomy.Normalize(c.autonomy).String()
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
	defer close(c.readDone)
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
		select {
		case c.lines <- msg:
		case <-c.quit:
			return
		}
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

type commandNotification struct {
	event       string
	message     string
	commandID   string
	processID   string
	command     string
	stream      string
	chunk       string
	exitCode    int
	exitCodeSet bool
	ok          bool
}

const (
	codexEventExecCommandBegin       = "exec_command_begin"
	codexEventExecCommandOutputDelta = "exec_command_output_delta"
	codexEventExecCommandEnd         = "exec_command_end"
	codexEventTerminalInteraction    = "terminal_interaction"
	codexEventExecCommand            = "exec_command_event"
	commandOutputStreamStdout        = "stdout"
	commandOutputStreamStderr        = "stderr"
)

func parseCommandNotification(method string, raw json.RawMessage) commandNotification {
	fields := rawObject(raw)
	eventName := commandEventName(method, fields)
	if eventName == "" {
		return commandNotification{}
	}

	itemFields := mapField(fields, "item")
	fieldSets := []map[string]any{fields, itemFields}
	processID := firstStringFromFields(fieldSets, "processId", "process_id", "processHandle", "process_handle")
	commandID := firstStringFromFields(fieldSets, "itemId", "item_id", "commandId", "command_id", "callId", "call_id", "id")
	if commandID == "" {
		commandID = processID
	}

	notification := commandNotification{
		ok:        true,
		event:     eventName,
		commandID: commandID,
		processID: processID,
		command:   commandString(fieldSets...),
		stream:    firstStreamFromFields(fieldSets),
	}
	notification.exitCode, notification.exitCodeSet = intFromFields(fieldSets, "exitCode", "exit_code")

	if stream, chunk := commandOutputChunk(fieldSets...); chunk != "" {
		notification.chunk = chunk
		notification.stream = firstNonEmpty(notification.stream, stream)
	}
	if notification.event == codexEventExecCommandOutputDelta && notification.chunk != "" && notification.stream == "" {
		notification.stream = commandOutputStreamStdout
	}

	switch notification.event {
	case codexEventExecCommandOutputDelta:
		notification.message = notification.chunk
	case codexEventExecCommandEnd:
		notification.message = notification.command
		if notification.message == "" && notification.exitCodeSet {
			notification.message = fmt.Sprintf("exit_code=%d", notification.exitCode)
		}
	case codexEventTerminalInteraction:
		notification.message = firstStringFromFields(fieldSets, "stdin")
		if notification.message == "" {
			notification.message = notification.chunk
		}
	default:
		notification.message = firstNonEmpty(notification.command, notification.commandID)
	}

	if notification.message == "" {
		notification.message = method
	}

	return notification
}

func commandEventName(method string, fields map[string]any) string {
	normalized := strings.ToLower(strings.TrimSpace(method))
	if normalized == "" {
		return ""
	}

	if eventName := commandItemLifecycleEventName(normalized, fields); eventName != "" {
		return eventName
	}

	if commandOutputEventMethod(normalized) {
		return codexEventExecCommandOutputDelta
	}

	if strings.Contains(normalized, "terminalinteraction") || strings.Contains(normalized, "terminal_interaction") {
		return codexEventTerminalInteraction
	}

	if normalized == "process/exited" {
		return codexEventExecCommandEnd
	}

	if !isCommandMethod(normalized) {
		return ""
	}

	return genericCommandEventName(normalized)
}

func commandItemLifecycleEventName(normalized string, fields map[string]any) string {
	if normalized != "item/started" && normalized != "item/completed" {
		return ""
	}

	if !isCommandItemType(eventStringValue(mapField(fields, "item")["type"])) {
		return ""
	}

	if strings.HasSuffix(normalized, "started") {
		return codexEventExecCommandBegin
	}

	return codexEventExecCommandEnd
}

func commandOutputEventMethod(normalized string) bool {
	if !strings.Contains(normalized, "outputdelta") && !strings.Contains(normalized, "output_delta") {
		return false
	}

	return isCommandMethod(normalized) || strings.Contains(normalized, "process/")
}

func genericCommandEventName(normalized string) string {
	switch {
	case strings.Contains(normalized, "begin"), strings.Contains(normalized, "start"):
		return codexEventExecCommandBegin
	case strings.Contains(normalized, "end"), strings.Contains(normalized, "complete"), strings.Contains(normalized, "finish"):
		return codexEventExecCommandEnd
	default:
		return codexEventExecCommand
	}
}

func isCommandMethod(normalized string) bool {
	return strings.Contains(normalized, "commandexecution") ||
		strings.Contains(normalized, "command_execution") ||
		strings.Contains(normalized, "command/exec") ||
		strings.Contains(normalized, "execcommand") ||
		strings.Contains(normalized, "exec_command")
}

func isCommandItemType(value string) bool {
	normalized := strings.ToLower(strings.TrimSpace(value))
	normalized = strings.ReplaceAll(normalized, "_", "")
	normalized = strings.ReplaceAll(normalized, "-", "")

	return normalized == "commandexecution" || normalized == "execcommand"
}

func rawObject(raw json.RawMessage) map[string]any {
	var fields map[string]any
	if err := json.Unmarshal(raw, &fields); err != nil {
		return nil
	}

	return fields
}

func mapField(fields map[string]any, key string) map[string]any {
	value, _ := fields[key].(map[string]any)

	return value
}

func firstStringField(fields map[string]any, keys ...string) string {
	for _, key := range keys {
		if value := eventStringValue(fields[key]); value != "" {
			return value
		}
	}

	return ""
}

func firstStringFromFields(fieldSets []map[string]any, keys ...string) string {
	for _, fields := range fieldSets {
		if value := firstStringField(fields, keys...); value != "" {
			return value
		}
	}

	return ""
}

func firstStreamFromFields(fieldSets []map[string]any) string {
	for _, fields := range fieldSets {
		if value := streamFromFields(fields); value != "" {
			return value
		}
	}

	return ""
}

func streamFromFields(fields map[string]any) string {
	for _, key := range []string{"stream", "outputStream", "output_stream", "fd"} {
		if value := outputStreamValue(fields[key]); value != "" {
			return value
		}
	}

	return ""
}

func outputStreamValue(value any) string {
	if value := normalizeOutputStream(eventStringValue(value)); value != "" {
		return value
	}

	switch typed := value.(type) {
	case float64:
		return normalizeOutputStream(fmt.Sprintf("%.0f", typed))
	case int:
		return normalizeOutputStream(strconvFormatInt(int64(typed)))
	case int64:
		return normalizeOutputStream(strconvFormatInt(typed))
	default:
		return ""
	}
}

func normalizeOutputStream(value string) string {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case commandOutputStreamStdout, "out", "1":
		return commandOutputStreamStdout
	case commandOutputStreamStderr, "err", "2":
		return commandOutputStreamStderr
	default:
		return strings.TrimSpace(value)
	}
}

func commandString(fieldSets ...map[string]any) string {
	for _, fields := range fieldSets {
		if value := firstStringField(fields, "command", "cmd", "parsedCmd", "parsed_cmd"); value != "" {
			return value
		}

		for _, key := range []string{"command", "cmd", "parsedCmd", "parsed_cmd"} {
			if values, ok := fields[key].([]any); ok && len(values) > 0 {
				parts := make([]string, 0, len(values))
				for _, value := range values {
					if part := eventStringValue(value); part != "" {
						parts = append(parts, part)
					}
				}

				if len(parts) > 0 {
					return strings.Join(parts, " ")
				}
			}
		}
	}

	return ""
}

func commandOutputChunk(fieldSets ...map[string]any) (stream string, chunk string) {
	for _, fields := range fieldSets {
		stream, chunk = commandOutputChunkFromFields(fields)
		if chunk != "" {
			return stream, chunk
		}
	}

	return "", ""
}

func commandOutputChunkFromFields(fields map[string]any) (stream string, chunk string) {
	if value, ok := fields["chunk"].(map[string]any); ok {
		return streamFromFields(value),
			firstStringField(value, "text", "delta", "data", "output", "chunk")
	}

	if value := firstStringField(fields, "deltaBase64", "delta_base64"); value != "" {
		decoded, err := base64.StdEncoding.DecodeString(value)
		if err == nil {
			return "", string(decoded)
		}

		return "", value
	}

	if value := firstStringField(fields, "chunk", "delta", "text", "data", "output"); value != "" {
		return "", value
	}

	if value := eventStringValue(fields[commandOutputStreamStdout]); value != "" {
		return normalizeOutputStream(commandOutputStreamStdout), value
	}

	if value := eventStringValue(fields[commandOutputStreamStderr]); value != "" {
		return normalizeOutputStream(commandOutputStreamStderr), value
	}

	return "", ""
}

func eventStringValue(value any) string {
	switch typed := value.(type) {
	case string:
		return typed
	case json.Number:
		return typed.String()
	case fmt.Stringer:
		return typed.String()
	default:
		return ""
	}
}

func intField(fields map[string]any, keys ...string) (int, bool) {
	for _, key := range keys {
		switch value := fields[key].(type) {
		case float64:
			return int(value), true
		case int:
			return value, true
		case int64:
			return int(value), true
		case json.Number:
			parsed, err := value.Int64()
			if err == nil {
				return int(parsed), true
			}
		}
	}

	return 0, false
}

func intFromFields(fieldSets []map[string]any, keys ...string) (int, bool) {
	for _, fields := range fieldSets {
		value, ok := intField(fields, keys...)
		if ok {
			return value, true
		}
	}

	return 0, false
}

func summarizeRaw(raw json.RawMessage) string {
	text := strings.TrimSpace(string(raw))
	if len(text) > 300 {
		return text[:300] + "..."
	}

	return text
}
