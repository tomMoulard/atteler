package main

import (
	"context"
	"fmt"
	"maps"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/tommoulard/atteler/pkg/autonomy"
	"github.com/tommoulard/atteler/pkg/events"
	"github.com/tommoulard/atteler/pkg/llm"
	"github.com/tommoulard/atteler/pkg/permission"
	attshell "github.com/tommoulard/atteler/pkg/shell"
)

type interactiveShellReadyMsg struct {
	cancel     context.CancelFunc
	cmd        *exec.Cmd
	invocation *attshell.Invocation
	err        error
	command    string
}

func (m model) runShellCommand(command string) (tea.Model, tea.Cmd) {
	if command == "" {
		return m, nil
	}

	line := userLabel.Render("$") + " " + command

	if isInteractiveCommand(command) {
		return m.runInteractiveShellCommand(command, line)
	}

	// cancel is stored in m.cancel and invoked from handleCtrlC and
	// updateShellResult once the command finishes; gosec can't see that.
	ctx, cancel := context.WithCancel(m.ctx) //nolint:gosec // see comment above
	m.cancel = cancel
	tickCmd := m.startRunningTask("command")
	outputCh := make(chan tea.Msg, 32)
	confirmCh := make(chan agentLoopConfirmRequest, 1)
	responseCh := make(chan bool, 1)

	ctx = contextWithTUIPermissionPrompt(ctx, m.permissionPolicy, confirmCh, responseCh)

	commandEvent := events.Event{
		Type:        events.CommandExecute,
		SessionID:   m.sessionState.ID,
		SessionPath: m.sessionPath,
		Agent:       m.selectedAgent,
		Model:       m.sessionState.DefaultModel,
		Content:     command,
		Metadata: map[string]string{
			"autonomy": m.autonomy.String(),
			"command":  command,
			"cwd":      m.cwd,
			"input":    "!" + command,
			"source":   "user",
		},
	}

	shellCmd := runShellCommandCmdWithAutonomyAndPermission(ctx, command, m.cwd, outputCh, m.autonomy, attshell.AuditContext{
		Caller:      "atteler.tui.shell",
		SessionID:   m.sessionState.ID,
		SessionPath: m.sessionPath,
		Autonomy:    m.autonomy.String(),
	}, m.permissionPolicy, func() {
		emitHookWarning(m.ctx, m.hookRunner, commandEvent)
	})
	wrappedShellCmd := func() tea.Msg {
		defer close(confirmCh)

		return shellCmd()
	}

	return m, tea.Batch(tea.Sequence(
		tea.Println(line),
		wrappedShellCmd,
	), tickCmd, listenForShellOutput(outputCh), listenForCheckpoint(confirmCh, responseCh))
}

// runInteractiveShellCommand hands the terminal to a child process via
// tea.ExecProcess so interactive programs (vim, less, htop, nested atteler)
// can use the PTY directly.
func (m model) runInteractiveShellCommand(command, line string) (model, tea.Cmd) {
	ctx, cancel := context.WithCancel(m.ctx)
	m.cancel = cancel

	confirmCh := make(chan agentLoopConfirmRequest, 1)
	responseCh := make(chan bool, 1)

	ctx = contextWithTUIPermissionPrompt(ctx, m.permissionPolicy, confirmCh, responseCh)

	authorizeCmd := authorizeInteractiveShellCommandCmd(ctx, cancel, command, m.cwd, attshell.AuditContext{
		Caller:      "atteler.tui.interactive_shell",
		SessionID:   m.sessionState.ID,
		SessionPath: m.sessionPath,
	}, m.permissionPolicy)
	wrappedAuthorizeCmd := func() tea.Msg {
		defer close(confirmCh)

		return authorizeCmd()
	}

	return m, tea.Batch(
		tea.Sequence(tea.Println(line), wrappedAuthorizeCmd),
		listenForCheckpoint(confirmCh, responseCh),
	)
}

func authorizeInteractiveShellCommandCmd(
	ctx context.Context,
	cancel context.CancelFunc,
	command, cwd string,
	audit attshell.AuditContext,
	permissionPolicy *permission.Policy,
) tea.Cmd {
	return func() tea.Msg {
		cmd, invocation, err := attshell.CommandContext(ctx, attshell.CommandOptions{
			Program:    "bash",
			Args:       []string{"--noprofile", "--norc", "-lc", command},
			Command:    command,
			Dir:        cwd,
			Mode:       attshell.ModeInteractive,
			Permission: permissionPolicy,
			Audit:      audit,
		})
		if err != nil && cancel != nil {
			cancel()
		}

		return interactiveShellReadyMsg{
			cancel:     cancel,
			cmd:        cmd,
			invocation: invocation,
			err:        err,
			command:    command,
		}
	}
}

func (m model) updateInteractiveShellReady(msg interactiveShellReadyMsg) (tea.Model, tea.Cmd) {
	command := msg.command
	if msg.err != nil {
		return m.updateShellResult(shellResultMsg{
			err:         msg.err,
			completedAt: time.Now(),
			command:     command,
			stdout:      "(interactive session" + exitErrorSuffix(msg.err.Error()) + ")",
		})
	}

	return m, tea.Sequence(
		emitHook(m.ctx, m.hookRunner, events.Event{
			Type:        events.CommandExecute,
			SessionID:   m.sessionState.ID,
			SessionPath: m.sessionPath,
			Agent:       m.selectedAgent,
			Model:       m.sessionState.DefaultModel,
			Content:     command,
			Metadata: map[string]string{
				"command":  command,
				"cwd":      m.cwd,
				"input":    "!" + command,
				"mode":     "interactive",
				"source":   "user",
				"autonomy": m.autonomy.String(),
			},
		}),
		tea.ExecProcess(msg.cmd, func(err error) tea.Msg {
			if msg.cancel != nil {
				msg.cancel()
			}

			exitError := ""
			if err != nil {
				exitError = err.Error()
			}

			if finishErr := msg.invocation.Finish(attshell.FinishOptions{
				Error:         err,
				OutputCapture: attshell.OutputNotCaptured,
				OutputNote:    "interactive terminal takeover; stdout/stderr not captured",
			}); finishErr != nil && exitError == "" {
				exitError = finishErr.Error()
				err = finishErr
			}

			return shellResultMsg{
				err:         err,
				completedAt: time.Now(),
				command:     command,
				stdout:      "(interactive session" + exitErrorSuffix(exitError) + ")",
			}
		}),
	)
}

// interactiveCommands is the set of commands known to require a PTY.
var interactiveCommands = map[string]struct{}{
	"vim": {}, "nvim": {}, "vi": {}, "nano": {}, "emacs": {},
	"less": {}, "more": {}, "top": {}, "htop": {}, "btop": {},
	"ssh": {}, "tmux": {}, "screen": {},
	"atteler": {}, "python": {}, "python3": {}, "node": {}, "irb": {},
}

// prependToolReminder injects a system message that tells the model which
// tools are available. This prevents the LLM from refusing tool use when
// the agent's system prompt mentions tools (e.g. "Edit tool", "Read tool")
// that are not actually wired up -- the model might otherwise conclude its
// tool environment is broken and fall back to plain text.
func prependToolReminder(params *llm.CompleteParams, tools []llm.ToolDefinition) {
	var names []string
	for _, t := range tools {
		names = append(names, t.Name)
	}

	reminder := llm.Message{
		Role: llm.RoleSystem,
		Content: "You have the following tools available and MUST use them " +
			"when the task requires running commands or inspecting files: " +
			strings.Join(names, ", ") + ". " +
			"Do NOT say you are unable to run commands. " +
			"Use the bash tool to execute shell commands.",
	}

	// Prepend so the reminder sits right before the conversation history.
	params.Messages = append([]llm.Message{reminder}, params.Messages...)
}

// listenForCheckpoint returns a tea.Cmd that waits for the agent loop to
// request a continuation/tool confirmation. When the loop sends a request on
// requestCh, this produces a loopCheckpointMsg for the TUI. The goroutine exits
// when requestCh is closed (i.e. when callLLMWithTools finishes).
func listenForCheckpoint(requestCh <-chan agentLoopConfirmRequest, responseCh chan bool) tea.Cmd {
	return func() tea.Msg {
		request, ok := <-requestCh
		if !ok {
			// Channel closed -- agent loop finished without hitting a confirmation.
			return nil
		}

		return loopCheckpointMsg{
			request:    request,
			responseCh: responseCh,
			requestCh:  requestCh,
		}
	}
}

// isInteractiveCommand returns true when the command's base name is a known
// interactive program or the command is prefixed with "!!" as a user hint.
func isInteractiveCommand(command string) bool {
	if strings.HasPrefix(command, "!") {
		return true // "!!" prefix signals interactive mode
	}

	base := strings.Fields(command)
	if len(base) == 0 {
		return false
	}

	name := filepath.Base(base[0])
	_, ok := interactiveCommands[name]

	return ok
}

func exitErrorSuffix(exitError string) string {
	if exitError == "" {
		return ""
	}

	return ": " + exitError
}

func listenForShellOutput(outputCh <-chan tea.Msg) tea.Cmd {
	return func() tea.Msg {
		msg, ok := <-outputCh
		if !ok {
			return nil
		}

		if outputMsg, ok := msg.(shellOutputMsg); ok {
			outputMsg.outputCh = outputCh

			return outputMsg
		}

		return msg
	}
}

func runShellCommandCmd(
	ctx context.Context,
	command, dir string,
	outputCh chan<- tea.Msg,
	audit attshell.AuditContext,
	permissionPolicy *permission.Policy,
	startCallback func(),
) tea.Cmd {
	return runShellCommandCmdWithAutonomyAndPermission(
		ctx,
		command,
		dir,
		outputCh,
		autonomy.DefaultLevel,
		audit,
		permissionPolicy,
		startCallback,
	)
}

func runShellCommandCmdWithAutonomy(
	ctx context.Context,
	command, dir string,
	outputCh chan<- tea.Msg,
	level autonomy.Level,
	audit attshell.AuditContext,
) tea.Cmd {
	return runShellCommandCmdWithAutonomyAndPermission(ctx, command, dir, outputCh, level, audit, nil, nil)
}

func runShellCommandCmdWithAutonomyAndPermission(
	ctx context.Context,
	command, dir string,
	outputCh chan<- tea.Msg,
	level autonomy.Level,
	audit attshell.AuditContext,
	permissionPolicy *permission.Policy,
	startCallback func(),
) tea.Cmd {
	level = autonomy.Normalize(level)
	if strings.TrimSpace(audit.Autonomy) == "" {
		audit.Autonomy = level.String()
	}

	return func() tea.Msg {
		if outputCh != nil {
			defer close(outputCh)
		}

		if err := authorizeBashCommandWithAutonomy(ctx, level, command, "TUI shell"); err != nil {
			msg := shellResultMsg{
				err:         err,
				completedAt: time.Now(),
				command:     command,
				stderr:      err.Error(),
				streamed:    outputCh != nil,
			}
			if outputCh != nil {
				outputCh <- msg

				return nil
			}

			return msg
		}

		result, err := attshell.RunBash(ctx, attshell.Options{
			Command:       command,
			Dir:           dir,
			Audit:         audit,
			Permission:    permissionPolicy,
			StartCallback: startCallback,
			OutputCallback: func(chunk attshell.OutputChunk) {
				if outputCh == nil {
					return
				}

				outputCh <- shellOutputMsg{
					command:  command,
					stream:   string(chunk.Stream),
					data:     string(chunk.Data),
					sequence: chunk.Sequence,
				}
			},
		})

		msg := shellResultMsg{
			err:         err,
			completedAt: time.Now(),
			command:     command,
			stdout:      result.Stdout,
			stderr:      result.Stderr,
			streamed:    outputCh != nil,
		}
		if outputCh != nil {
			outputCh <- msg

			return nil
		}

		return msg
	}
}

func (m model) updateShellOutput(msg shellOutputMsg) (tea.Model, tea.Cmd) {
	metadata := map[string]string{
		"command":  msg.command,
		"cwd":      m.cwd,
		"partial":  "true",
		"sequence": strconv.FormatInt(msg.sequence, 10),
		"source":   "user",
		"stream":   msg.stream,
		"autonomy": m.autonomy.String(),
	}

	cmds := []tea.Cmd{
		tea.Printf("%s", formatShellOutputChunk(msg)),
		emitHookQuiet(m.ctx, m.hookRunner, events.Event{
			Type:        events.CommandOutput,
			SessionID:   m.sessionState.ID,
			SessionPath: m.sessionPath,
			Agent:       m.selectedAgent,
			Model:       m.sessionState.DefaultModel,
			Content:     msg.data,
			Metadata:    metadata,
		}),
		listenForShellOutput(msg.outputCh),
	}

	return m, tea.Sequence(cmds...)
}

func formatShellOutputChunk(msg shellOutputMsg) string {
	if msg.stream == string(attshell.OutputStreamStderr) {
		return errStyle.Render(msg.data)
	}

	return msg.data
}

// updateShellResult appends the executed command and its output to the chat
// history as a synthetic user message and prints the output.
func (m model) updateShellResult(msg shellResultMsg) (tea.Model, tea.Cmd) {
	m.waiting = false
	m.cancel = nil
	elapsed := m.finishRunningTask(msg.completedAt)

	content := formatShellContext(msg)
	outputEvent := commandOutputEvent(
		m.sessionState.ID,
		m.sessionPath,
		m.selectedAgent,
		m.sessionState.DefaultModel,
		m.cwd,
		msg.command,
		content,
		msg.err,
		map[string]string{"source": "user", "autonomy": m.autonomy.String()},
	)
	m.history = append(m.history, llm.Message{
		Role:    llm.RoleUser,
		Content: content,
	})
	m.sessionState.Messages = append([]llm.Message(nil), m.history...)

	cmds := []tea.Cmd{tea.SetWindowTitle(terminalIdleTitle()), emitHook(m.ctx, m.hookRunner, outputEvent)}
	if !msg.streamed && msg.stdout != "" {
		cmds = append(cmds, tea.Println(strings.TrimRight(msg.stdout, "\n")))
	}

	if !msg.streamed && msg.stderr != "" {
		cmds = append(cmds, tea.Println(errStyle.Render(strings.TrimRight(msg.stderr, "\n"))))
	}

	if msg.err != nil {
		cmds = append(cmds, tea.Println(errStyle.Render("(command error: "+msg.err.Error()+")")))
	}

	if elapsed > 0 {
		cmds = append(cmds, tea.Println(dimStyle.Render("(command ran for "+formatTaskDuration(elapsed)+")")))
	}

	cmds = append(cmds, saveSession(m.ctx, m.sessionStore, m.sessionState, m.hookRunner))

	return m.continueWithQueuedPrompt(tea.Sequence(cmds...))
}

// formatShellContext renders an executed shell command and its output as a
// chat-history entry that future LLM calls can use as context.
func formatShellContext(msg shellResultMsg) string {
	var b strings.Builder
	fmt.Fprintf(&b, "$ %s\n", msg.command)

	if msg.stdout != "" {
		b.WriteString(strings.TrimRight(msg.stdout, "\n"))
		b.WriteString("\n")
	}

	if msg.stderr != "" {
		b.WriteString("[stderr]\n")
		b.WriteString(strings.TrimRight(msg.stderr, "\n"))
		b.WriteString("\n")
	}

	if msg.err != nil {
		fmt.Fprintf(&b, "[error] %s\n", msg.err.Error())

		// Include a recovery hint for timeouts so the LLM can reason about
		// retry strategies when this context appears in subsequent prompts.
		if strings.Contains(msg.err.Error(), "timed out") {
			b.WriteString("[timeout] The command exceeded its time limit. " +
				"Consider retrying with a smaller scope or splitting the work.\n")
		}
	}

	return strings.TrimRight(b.String(), "\n")
}

func commandOutputEvent(
	sessionID, sessionPath, agentName, modelName, cwd, command, content string,
	err error,
	extra map[string]string,
) events.Event {
	metadata := map[string]string{
		"command": command,
		"cwd":     cwd,
		"partial": "false",
	}
	maps.Copy(metadata, extra)

	event := events.Event{
		Type:        events.CommandOutput,
		SessionID:   sessionID,
		SessionPath: sessionPath,
		Agent:       agentName,
		Model:       modelName,
		Content:     content,
		Metadata:    metadata,
	}
	if err != nil {
		event.Error = err.Error()
	}

	return event
}
