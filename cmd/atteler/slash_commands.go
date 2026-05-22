package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/tommoulard/atteler/pkg/events"
	"github.com/tommoulard/atteler/pkg/llm"
	"github.com/tommoulard/atteler/pkg/session"
)

func (m model) handleSlashCommand(input string) (model, tea.Cmd, bool) {
	descriptor, parsed, handled, err := parseSlashCommandInput(input)
	if !handled {
		return m, nil, false
	}

	if err != nil {
		return m, tea.Println(errStyle.Render(err.Error())), true
	}

	return descriptor.Run(m, parsed)
}

func writeSessionExport(s session.Session, path string) error {
	var data []byte

	var err error

	switch strings.ToLower(filepath.Ext(path)) {
	case ".json":
		data, err = session.JSON(s)
	case ".jsonl":
		data, err = marshalJSONLines(s)
	case ".txt":
		data = []byte(plainTranscript(s.Messages))
	default:
		data = []byte(session.Markdown(s))
	}

	if err != nil {
		return fmt.Errorf("marshal session export: %w", err)
	}

	if path == "-" {
		fmt.Print(string(data))

		return nil
	}

	if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil && filepath.Dir(path) != "." {
		return fmt.Errorf("create session export directory: %w", err)
	}

	if err := os.WriteFile(path, data, 0o600); err != nil {
		return fmt.Errorf("write session export %q: %w", path, err)
	}

	return nil
}

func marshalJSONLines(v any) ([]byte, error) {
	rv := reflect.ValueOf(v)
	if rv.IsValid() && (rv.Kind() == reflect.Slice || rv.Kind() == reflect.Array) {
		var b strings.Builder

		for i := range rv.Len() {
			line, err := json.Marshal(rv.Index(i).Interface())
			if err != nil {
				return nil, fmt.Errorf("marshal json line: %w", err)
			}

			b.Write(line)
			b.WriteByte('\n')
		}

		return []byte(b.String()), nil
	}

	line, err := json.Marshal(v)
	if err != nil {
		return nil, fmt.Errorf("marshal json line: %w", err)
	}

	return append(line, '\n'), nil
}

func plainTranscript(messages []llm.Message) string {
	var b strings.Builder
	for _, msg := range messages {
		fmt.Fprintf(&b, "%s: %s\n\n", msg.Role, msg.Content)
	}

	return b.String()
}

func runHelpSlashCommand(m model, _ slashNoArgsInput) (model, tea.Cmd, bool) {
	return m, tea.Println(slashHelp()), true
}

func runClearSlashCommand(m model, _ slashNoArgsInput) (model, tea.Cmd, bool) {
	m.history = nil
	m.sessionState.Messages = nil
	m.tokenUsage = tokenUsage{}

	return m, tea.Sequence(
		saveSession(m.ctx, m.sessionStore, m.sessionState, m.hookRunner),
		tea.Println(dimStyle.Render("(conversation cleared)")),
	), true
}

func runModelSlashCommand(m model, input slashOptionalValueInput) (model, tea.Cmd, bool) {
	if input.Value == "" {
		return m, tea.Println("model: " + m.selectedModel), true
	}

	m.selectedModel = input.Value
	m.modelLocked = true
	m.sessionState.DefaultModel = input.Value

	return m, tea.Println(dimStyle.Render("model set to " + input.Value)), true
}

func runProfileSlashCommand(m model, input slashOptionalValueInput) (model, tea.Cmd, bool) {
	if input.Value == "" {
		return m, tea.Println("profile: " + m.selectedAgent), true
	}

	// Profiles map to configured agents, which already carry model/system/generation presets.
	if _, ok := m.agentRegistry.Get(input.Value); !ok {
		return m, tea.Println(errStyle.Render("unknown profile/agent: " + input.Value)), true
	}

	m.selectedAgent = input.Value
	m.sessionState.DefaultAgent = input.Value

	return m, tea.Println(dimStyle.Render("profile set to " + input.Value)), true
}

func runSaveSlashCommand(m model, _ slashNoArgsInput) (model, tea.Cmd, bool) {
	return m, tea.Sequence(
		saveSession(m.ctx, m.sessionStore, m.sessionState, m.hookRunner),
		tea.Println(dimStyle.Render("saved session "+m.sessionState.ID)),
	), true
}

func runExportSlashCommand(m model, input slashOptionalValueInput) (model, tea.Cmd, bool) {
	path := input.Value
	if path == "" {
		path = "session.md"
	}

	if err := writeSessionExport(m.sessionState, path); err != nil {
		return m, tea.Println(errStyle.Render("export: " + err.Error())), true
	}

	return m, tea.Println(dimStyle.Render("exported " + path)), true
}

func runRetrySlashCommand(m model, _ slashNoArgsInput) (model, tea.Cmd, bool) {
	return m.regenerateLast()
}

func runEditSlashCommand(m model, _ slashNoArgsInput) (model, tea.Cmd, bool) {
	return m.editLastUser()
}

func runForkSlashCommand(m model, input slashForkInput) (model, tea.Cmd, bool) {
	return m.forkAt(input)
}

func runTokensSlashCommand(m model, _ slashNoArgsInput) (model, tea.Cmd, bool) {
	return m, tea.Println(formatTokenUsageSummary(m.tokenUsage)), true
}

func runSearchSlashCommand(m model, input slashSearchInput) (model, tea.Cmd, bool) {
	return m, tea.Println(searchMessages(m.history, strings.ToLower(input.Query))), true
}

func runPinSlashCommand(m model, input slashMessageNumberInput) (model, tea.Cmd, bool) {
	return m.pinMessage(input, true)
}

func runUnpinSlashCommand(m model, input slashMessageNumberInput) (model, tea.Cmd, bool) {
	return m.pinMessage(input, false)
}

func runContextSlashCommand(m model, input slashContextInput) (model, tea.Cmd, bool) {
	if input.Prune {
		m.pruneToPinned()

		return m, tea.Println(dimStyle.Render("context pruned to pinned messages")), true
	}

	return m, tea.Println(m.contextSummary()), true
}

func runModeSlashCommand(m model, input slashModeInput) (model, tea.Cmd, bool) {
	if input.Show {
		return m, tea.Println("mode: " + m.executionMode), true
	}

	m.executionMode = input.Mode

	return m, tea.Println(dimStyle.Render("mode set to " + input.Mode)), true
}

func runCodeblocksSlashCommand(m model, _ slashNoArgsInput) (model, tea.Cmd, bool) {
	return m, tea.Println(listCodeBlocks(lastAssistantContent(m.history))), true
}

func (m model) regenerateLast() (model, tea.Cmd, bool) {
	idx := lastUserIndex(m.history)
	if idx < 0 {
		return m, tea.Println(errStyle.Render("no user message to retry")), true
	}

	prompt := m.history[idx].Content
	m.history = append([]llm.Message(nil), m.history[:idx]...)
	m.sessionState.Messages = append([]llm.Message(nil), m.history...)

	next, cmd := m.submitPrompt(prompt)
	if nm, ok := next.(model); ok {
		return nm, cmd, true
	}

	return m, cmd, true
}

func (m model) editLastUser() (model, tea.Cmd, bool) {
	idx := lastUserIndex(m.history)
	if idx < 0 {
		return m, tea.Println(errStyle.Render("no user message to edit")), true
	}

	m.textarea.SetValue(m.history[idx].Content)
	m.textarea.CursorEnd()
	m.history = append([]llm.Message(nil), m.history[:idx]...)
	m.sessionState.Messages = append([]llm.Message(nil), m.history...)

	return m, tea.Println(dimStyle.Render("editing last prompt")), true
}

func (m model) forkAt(input slashForkInput) (model, tea.Cmd, bool) {
	n := len(m.history)
	if input.HasCount {
		n = input.Count
	}

	if n < 0 {
		n = 0
	}

	if n > len(m.history) {
		n = len(m.history)
	}

	m.history = append([]llm.Message(nil), m.history[:n]...)
	m.sessionState = session.New(m.selectedModel, m.history)

	return m, tea.Println(dimStyle.Render("forked session " + m.sessionState.ID)), true
}

func lastUserIndex(ms []llm.Message) int {
	for i := len(ms) - 1; i >= 0; i-- {
		if ms[i].Role == llm.RoleUser {
			return i
		}
	}

	return -1
}

func lastAssistantContent(ms []llm.Message) string {
	for i := len(ms) - 1; i >= 0; i-- {
		if ms[i].Role == llm.RoleAssistant {
			return ms[i].Content
		}
	}

	return ""
}

func searchMessages(ms []llm.Message, q string) string {
	if q == "" {
		return "usage: /search <query>"
	}

	var out []string

	for i, msg := range ms {
		if strings.Contains(strings.ToLower(msg.Content), q) {
			out = append(out, fmt.Sprintf("%d\t%s\t%s", i+1, msg.Role, truncateRunes(compactMessageWhitespace(msg.Content), 120)))
		}
	}

	if len(out) == 0 {
		return "no matches"
	}

	return strings.Join(out, "\n")
}

func (m model) pinMessage(input slashMessageNumberInput, pin bool) (model, tea.Cmd, bool) {
	if input.Number < 1 || input.Number > len(m.history) {
		return m, tea.Println(errStyle.Render("invalid message number")), true
	}

	if m.pinnedMessages == nil {
		m.pinnedMessages = map[int]bool{}
	}

	if pin {
		m.pinnedMessages[input.Number-1] = true
	} else {
		delete(m.pinnedMessages, input.Number-1)
	}

	return m, tea.Println(dimStyle.Render("pin updated")), true
}

func (m *model) pruneToPinned() {
	if len(m.pinnedMessages) == 0 {
		return
	}

	var out []llm.Message

	newPinned := make(map[int]bool, len(m.pinnedMessages))

	for i, msg := range m.history {
		if m.pinnedMessages[i] {
			out = append(out, msg)
			newPinned[len(out)-1] = true
		}
	}

	m.history = out
	m.sessionState.Messages = append([]llm.Message(nil), out...)
	m.pinnedMessages = newPinned
}

func (m model) contextSummary() string {
	return fmt.Sprintf("messages=%d pinned=%d tokens~%s", len(m.history), len(m.pinnedMessages), formatTokenCount(llm.EstimateTokens(m.history)))
}

// fenced code helpers
func extractCodeBlocks(s string) []string {
	var blocks []string

	lines := strings.Split(s, "\n")
	in := false

	var b strings.Builder

	for _, ln := range lines {
		if strings.HasPrefix(strings.TrimSpace(ln), "```") {
			if in {
				blocks = append(blocks, strings.TrimRight(b.String(), "\n"))
				b.Reset()

				in = false
			} else {
				in = true
			}

			continue
		}

		if in {
			b.WriteString(ln)
			b.WriteByte('\n')
		}
	}

	return blocks
}

func listCodeBlocks(s string) string {
	bs := extractCodeBlocks(s)
	if len(bs) == 0 {
		return "no code blocks"
	}

	var out []string

	for i, b := range bs {
		out = append(out, fmt.Sprintf("%d\t%d chars\t%s", i+1, len(b), truncateRunes(compactMessageWhitespace(b), 80)))
	}

	return strings.Join(out, "\n")
}

func runSaveCodeSlashCommand(m model, input slashSaveCodeInput) (model, tea.Cmd, bool) {
	bs := extractCodeBlocks(lastAssistantContent(m.history))
	if input.Block < 1 || input.Block > len(bs) {
		return m, tea.Println(errStyle.Render("invalid code block")), true
	}

	if err := os.WriteFile(input.Path, []byte(bs[input.Block-1]), 0o600); err != nil {
		return m, tea.Println(errStyle.Render(err.Error())), true
	}

	return m, tea.Println(dimStyle.Render("saved " + input.Path)), true
}

func runCopySlashCommand(m model, input slashCopyInput) (model, tea.Cmd, bool) {
	text := lastAssistantContent(m.history)
	if input.Target == sessionCommandName {
		text = plainTranscript(m.history)
	}

	if err := copyToClipboard(m.ctx, text); err != nil {
		return m, tea.Println(errStyle.Render("copy: " + err.Error())), true
	}

	return m, tea.Println(dimStyle.Render("copied")), true
}

func runCopyCodeSlashCommand(m model, input slashCopyCodeInput) (model, tea.Cmd, bool) {
	text := lastAssistantContent(m.history)
	blocks := extractCodeBlocks(text)

	if input.Block < 1 || input.Block > len(blocks) {
		return m, tea.Println(errStyle.Render("invalid code block")), true
	}

	if err := copyToClipboard(m.ctx, blocks[input.Block-1]); err != nil {
		return m, tea.Println(errStyle.Render("copy: " + err.Error())), true
	}

	return m, tea.Println(dimStyle.Render("copied")), true
}

func copyToClipboard(ctx context.Context, text string) error {
	if ctx == nil {
		return errors.New("context is required")
	}

	if err := ctx.Err(); err != nil {
		return fmt.Errorf("context already done: %w", err)
	}

	cmds := [][]string{{"pbcopy"}, {"wl-copy"}, {"xclip", "-selection", "clipboard"}}
	for _, c := range cmds {
		if _, err := exec.LookPath(c[0]); err == nil {
			cmd := exec.CommandContext(ctx, c[0], c[1:]...) //nolint:gosec // clipboard commands are selected from the fixed allowlist above.

			cmd.Stdin = strings.NewReader(text)
			if err := cmd.Run(); err != nil {
				return fmt.Errorf("run %s: %w", c[0], err)
			}

			return nil
		}
	}

	return errors.New("no clipboard command found")
}

func runApplyPatchSlashCommand(m model, _ slashNoArgsInput) (model, tea.Cmd, bool) {
	patch, ok := lastAssistantUnifiedDiff(m.history)
	if !ok {
		return m, tea.Println(errStyle.Render("no unified diff found")), true
	}

	next, cmd := m.runGitApplyPatch(patch)

	return next, cmd, true
}

func lastAssistantUnifiedDiff(messages []llm.Message) (string, bool) {
	patch := lastAssistantContent(messages)
	if isUnifiedDiff(patch) {
		return patch, true
	}

	for _, block := range extractCodeBlocks(patch) {
		if isUnifiedDiff(block) {
			return block, true
		}
	}

	return "", false
}

func isUnifiedDiff(value string) bool {
	return strings.Contains(value, "---") && strings.Contains(value, "+++")
}

func (m model) runGitApplyPatch(patch string) (model, tea.Cmd) {
	const displayCommand = "git apply --check - && git apply -"

	line := userLabel.Render("$") + " " + displayCommand
	ctx, cancel := context.WithCancel(m.ctx)
	m.cancel = cancel
	tickCmd := m.startRunningTask("apply-patch")

	return m, tea.Batch(tea.Sequence(
		tea.Println(line),
		emitHook(m.ctx, m.hookRunner, events.Event{
			Type:        events.CommandExecute,
			SessionID:   m.sessionState.ID,
			SessionPath: m.sessionPath,
			Agent:       m.selectedAgent,
			Model:       m.sessionState.DefaultModel,
			Content:     displayCommand,
			Metadata: map[string]string{
				"command": displayCommand,
				"cwd":     m.cwd,
				"input":   "/apply-patch",
				"source":  "slash",
			},
		}),
		runGitApplyPatchCmd(ctx, patch, m.cwd, displayCommand),
	), tickCmd)
}

func runGitApplyPatchCmd(ctx context.Context, patch, dir, displayCommand string) tea.Cmd {
	return func() tea.Msg {
		stdout, stderr, err := runGitApply(ctx, dir, []string{"--check", "-"}, patch)
		if err == nil {
			var applyStdout, applyStderr string

			applyStdout, applyStderr, err = runGitApply(ctx, dir, []string{"-"}, patch)
			stdout += applyStdout
			stderr += applyStderr
		}

		return shellResultMsg{
			err:         err,
			completedAt: time.Now(),
			command:     displayCommand,
			stdout:      stdout,
			stderr:      stderr,
		}
	}
}

func runGitApply(ctx context.Context, dir string, args []string, patch string) (stdoutText, stderrText string, err error) {
	cmd := exec.CommandContext(ctx, "git", append([]string{"apply"}, args...)...) //nolint:gosec // git args are static slash-command internals and patch content is passed on stdin.
	if dir != "" {
		cmd.Dir = dir
	}

	cmd.Stdin = strings.NewReader(patch)

	var stdout, stderr bytes.Buffer

	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	if err := cmd.Run(); err != nil {
		return stdout.String(), stderr.String(), fmt.Errorf("git apply %s: %w", strings.Join(args, " "), err)
	}

	return stdout.String(), stderr.String(), nil
}

func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", "'\\''") + "'"
}

var builtInTemplates = map[string]string{"code-review": "Review this change for correctness, tests, security, and maintainability.", "explain-error": "Explain this error, likely causes, and the smallest fix.", "write-tests": "Write focused tests for this behavior.", "refactor-plan": "Propose a safe step-by-step refactor plan.", "commit-message": "Write a concise conventional commit message for the current change."}

func runTemplateSlashCommand(m model, input slashOptionalValueInput) (model, tea.Cmd, bool) {
	if input.Value == "" {
		keys := make([]string, 0, len(builtInTemplates))
		for k := range builtInTemplates {
			keys = append(keys, k)
		}

		return m, tea.Println("templates: " + strings.Join(keys, ", ")), true
	}

	t, ok := builtInTemplates[input.Value]
	if !ok {
		return m, tea.Println(errStyle.Render("unknown template")), true
	}

	m.textarea.SetValue(t)
	m.textarea.CursorEnd()

	return m, nil, true
}

func runEvalSlashCommand(m model, input slashEvalInput) (model, tea.Cmd, bool) {
	switch input.Action {
	case "add":
		path := filepath.Join(evalCasesDir(m.cwd), m.sessionState.ID+".json")
		if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
			return m, tea.Println(errStyle.Render(err.Error())), true
		}

		data, err := json.MarshalIndent(m.sessionState, "", "  ")
		if err != nil {
			return m, tea.Println(errStyle.Render("eval add: " + err.Error())), true
		}

		if err := os.WriteFile(path, data, 0o600); err != nil {
			return m, tea.Println(errStyle.Render(err.Error())), true
		}

		return m, tea.Println(dimStyle.Render("added eval " + path)), true
	case "run":
		count, err := countEvalCases(evalCasesDir(m.cwd))
		if err != nil {
			return m, tea.Println(errStyle.Render("eval run: " + err.Error())), true
		}

		return m, tea.Println(fmt.Sprintf("eval cases: %d", count)), true
	}

	return m, tea.Println("usage: /eval add|run"), true
}

func evalCasesDir(cwd string) string {
	if strings.TrimSpace(cwd) == "" {
		return filepath.Join(".atteler", "evals")
	}

	return filepath.Join(cwd, ".atteler", "evals")
}

func countEvalCases(dir string) (int, error) {
	entries, err := os.ReadDir(dir)
	if os.IsNotExist(err) {
		return 0, nil
	}

	if err != nil {
		return 0, fmt.Errorf("read eval cases: %w", err)
	}

	count := 0

	for _, entry := range entries {
		if !entry.IsDir() && strings.EqualFold(filepath.Ext(entry.Name()), ".json") {
			count++
		}
	}

	return count, nil
}
