//nolint:cyclop,errcheck,errchkjson,gocognit,gofumpt,gosec,intrange,noctx,perfsprint,wrapcheck,wsl_v5 // Legacy slash-command dispatcher predates strict lint cleanup.
package main

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/tommoulard/atteler/pkg/llm"
	"github.com/tommoulard/atteler/pkg/session"
)

func (m model) handleSlashCommand(input string) (model, tea.Cmd, bool) {
	fields := strings.Fields(input)
	if len(fields) == 0 || !strings.HasPrefix(fields[0], "/") {
		return m, nil, false
	}

	cmd := strings.TrimPrefix(strings.ToLower(fields[0]), "/")
	args := fields[1:]

	switch cmd {
	case helpCommandName:
		return m, tea.Println(slashHelp()), true
	case "clear":
		m.history = nil
		m.sessionState.Messages = nil
		m.tokenUsage = tokenUsage{}
		return m, tea.Sequence(saveSession(m.ctx, m.sessionStore, m.sessionState, m.hookRunner), tea.Println(dimStyle.Render("(conversation cleared)"))), true
	case "model":
		if len(args) == 0 {
			return m, tea.Println("model: " + m.selectedModel), true
		}
		m.selectedModel = args[0]
		m.modelLocked = true
		m.sessionState.DefaultModel = args[0]
		return m, tea.Println(dimStyle.Render("model set to " + args[0])), true
	case "profile":
		// Profiles map to configured agents, which already carry model/system/generation presets.
		if len(args) == 0 {
			return m, tea.Println("profile: " + m.selectedAgent), true
		}
		if _, ok := m.agentRegistry.Get(args[0]); !ok {
			return m, tea.Println(errStyle.Render("unknown profile/agent: " + args[0])), true
		}
		m.selectedAgent = args[0]
		m.sessionState.DefaultAgent = args[0]
		return m, tea.Println(dimStyle.Render("profile set to " + args[0])), true
	case "save":
		return m, tea.Sequence(saveSession(m.ctx, m.sessionStore, m.sessionState, m.hookRunner), tea.Println(dimStyle.Render("saved session "+m.sessionState.ID))), true
	case "export":
		path := "session.md"
		if len(args) > 0 {
			path = args[0]
		}
		if err := writeSessionExport(m.sessionState, path); err != nil {
			return m, tea.Println(errStyle.Render("export: " + err.Error())), true
		}
		return m, tea.Println(dimStyle.Render("exported " + path)), true
	case "retry", "regenerate":
		return m.regenerateLast()
	case "edit":
		return m.editLastUser()
	case "fork":
		return m.forkAt(args)
	case "cost", "tokens":
		return m, tea.Println(formatTokenUsageSummary(m.tokenUsage)), true
	case "search":
		q := strings.ToLower(strings.Join(args, " "))
		return m, tea.Println(searchMessages(m.history, q)), true
	case "pin":
		return m.pinMessage(args, true)
	case "unpin":
		return m.pinMessage(args, false)
	case "context":
		if len(args) > 0 && args[0] == "prune" {
			m.pruneToPinned()
			return m, tea.Println(dimStyle.Render("context pruned to pinned messages")), true
		}
		return m, tea.Println(m.contextSummary()), true
	case "mode":
		if len(args) == 0 {
			return m, tea.Println("mode: " + m.executionMode), true
		}
		if args[0] != "plan" && args[0] != "execute" {
			return m, tea.Println(errStyle.Render("mode must be plan or execute")), true
		}
		m.executionMode = args[0]
		return m, tea.Println(dimStyle.Render("mode set to " + args[0])), true
	case "codeblocks":
		return m, tea.Println(listCodeBlocks(lastAssistantContent(m.history))), true
	case "save-code":
		return m.saveCodeBlock(args)
	case "copy", "copy-code":
		return m.copyCommand(args, cmd == "copy-code")
	case "apply-patch":
		return m.applyPatch()
	case "template":
		return m.templateCommand(args)
	case "eval":
		return m.evalCommand(args)
	default:
		return m, tea.Println(errStyle.Render("unknown command: /" + cmd + " (try /help)")), true
	}
}

func slashHelp() string {
	return `/help /model /profile /save /export [path] /clear /retry /edit /fork [n]
/tokens /cost /search <query> /pin <n> /unpin <n> /context [prune] /mode plan|execute
/template [name] /codeblocks /save-code <n> <path> /copy [last|session] /copy-code <n> /apply-patch /eval add|run`
}

func writeSessionExport(s session.Session, path string) error {
	var data []byte
	var err error
	switch strings.ToLower(filepath.Ext(path)) {
	case ".json":
		data, err = json.MarshalIndent(s, "", "  ")
	case ".jsonl":
		data, err = marshalJSONLines(s)
	case ".txt":
		data = []byte(plainTranscript(s.Messages))
	default:
		data = []byte(session.Markdown(s))
	}
	if err != nil {
		return err
	}
	if path == "-" {
		fmt.Print(string(data))
		return nil
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil && filepath.Dir(path) != "." {
		return err
	}
	return os.WriteFile(path, data, 0o600)
}

func marshalJSONLines(v any) ([]byte, error) {
	rv := reflect.ValueOf(v)
	if rv.IsValid() && (rv.Kind() == reflect.Slice || rv.Kind() == reflect.Array) {
		var b strings.Builder
		for i := 0; i < rv.Len(); i++ {
			line, err := json.Marshal(rv.Index(i).Interface())
			if err != nil {
				return nil, err
			}
			b.Write(line)
			b.WriteByte('\n')
		}
		return []byte(b.String()), nil
	}

	line, err := json.Marshal(v)
	if err != nil {
		return nil, err
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
func (m model) forkAt(args []string) (model, tea.Cmd, bool) {
	n := len(m.history)
	if len(args) > 0 {
		if v, err := strconv.Atoi(args[0]); err == nil {
			n = v
		}
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
func (m model) pinMessage(args []string, pin bool) (model, tea.Cmd, bool) {
	if len(args) == 0 {
		return m, tea.Println("usage: /pin <message-number>"), true
	}
	n, err := strconv.Atoi(args[0])
	if err != nil || n < 1 || n > len(m.history) {
		return m, tea.Println(errStyle.Render("invalid message number")), true
	}
	if m.pinnedMessages == nil {
		m.pinnedMessages = map[int]bool{}
	}
	if pin {
		m.pinnedMessages[n-1] = true
	} else {
		delete(m.pinnedMessages, n-1)
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
func (m model) saveCodeBlock(args []string) (model, tea.Cmd, bool) {
	if len(args) < 2 {
		return m, tea.Println("usage: /save-code <n> <path>"), true
	}
	n, err := strconv.Atoi(args[0])
	bs := extractCodeBlocks(lastAssistantContent(m.history))
	if err != nil || n < 1 || n > len(bs) {
		return m, tea.Println(errStyle.Render("invalid code block")), true
	}
	if err := os.WriteFile(args[1], []byte(bs[n-1]), 0o600); err != nil {
		return m, tea.Println(errStyle.Render(err.Error())), true
	}
	return m, tea.Println(dimStyle.Render("saved " + args[1])), true
}
func (m model) copyCommand(args []string, code bool) (model, tea.Cmd, bool) {
	text := lastAssistantContent(m.history)
	if code {
		n := 1
		if len(args) > 0 {
			n, _ = strconv.Atoi(args[0])
		}
		bs := extractCodeBlocks(text)
		if n < 1 || n > len(bs) {
			return m, tea.Println(errStyle.Render("invalid code block")), true
		}
		text = bs[n-1]
	} else if len(args) > 0 && args[0] == "session" {
		text = plainTranscript(m.history)
	}
	if err := copyToClipboard(text); err != nil {
		return m, tea.Println(errStyle.Render("copy: " + err.Error())), true
	}
	return m, tea.Println(dimStyle.Render("copied")), true
}
func copyToClipboard(text string) error {
	cmds := [][]string{{"pbcopy"}, {"wl-copy"}, {"xclip", "-selection", "clipboard"}}
	for _, c := range cmds {
		if _, err := exec.LookPath(c[0]); err == nil {
			cmd := exec.Command(c[0], c[1:]...)
			cmd.Stdin = strings.NewReader(text)
			return cmd.Run()
		}
	}
	return fmt.Errorf("no clipboard command found")
}
func (m model) applyPatch() (model, tea.Cmd, bool) {
	patch := lastAssistantContent(m.history)
	if !strings.Contains(patch, "---") || !strings.Contains(patch, "+++") {
		bs := extractCodeBlocks(patch)
		for _, b := range bs {
			if strings.Contains(b, "---") && strings.Contains(b, "+++") {
				patch = b
				break
			}
		}
	}
	if !strings.Contains(patch, "+++") {
		return m, tea.Println(errStyle.Render("no unified diff found")), true
	}
	patchFile, err := os.CreateTemp("", "atteler-patch-*.diff")
	if err != nil {
		return m, tea.Println(errStyle.Render("apply-patch: " + err.Error())), true
	}
	patchPath := patchFile.Name()
	if _, err := patchFile.WriteString(patch); err != nil {
		_ = patchFile.Close()
		_ = os.Remove(patchPath)
		return m, tea.Println(errStyle.Render("apply-patch: " + err.Error())), true
	}
	if err := patchFile.Close(); err != nil {
		_ = os.Remove(patchPath)
		return m, tea.Println(errStyle.Render("apply-patch: " + err.Error())), true
	}

	command := gitApplyPatchCommand(patchPath)
	next, cmd := m.runShellCommand(command)
	if nm, ok := next.(model); ok {
		return nm, cmd, true
	}
	return m, cmd, true
}

func gitApplyPatchCommand(patchPath string) string {
	quotedPatchPath := shellQuote(patchPath)
	return "git apply --check " + quotedPatchPath + " && git apply " + quotedPatchPath + "; status=$?; rm -f " + quotedPatchPath + "; exit $status"
}

func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", "'\\''") + "'"
}

var builtInTemplates = map[string]string{"code-review": "Review this change for correctness, tests, security, and maintainability.", "explain-error": "Explain this error, likely causes, and the smallest fix.", "write-tests": "Write focused tests for this behavior.", "refactor-plan": "Propose a safe step-by-step refactor plan.", "commit-message": "Write a concise conventional commit message for the current change."}

func (m model) templateCommand(args []string) (model, tea.Cmd, bool) {
	if len(args) == 0 {
		keys := make([]string, 0, len(builtInTemplates))
		for k := range builtInTemplates {
			keys = append(keys, k)
		}
		return m, tea.Println("templates: " + strings.Join(keys, ", ")), true
	}
	t, ok := builtInTemplates[args[0]]
	if !ok {
		return m, tea.Println(errStyle.Render("unknown template")), true
	}
	m.textarea.SetValue(t)
	m.textarea.CursorEnd()
	return m, nil, true
}
func (m model) evalCommand(args []string) (model, tea.Cmd, bool) {
	if len(args) == 0 {
		return m, tea.Println("usage: /eval add|run"), true
	}
	switch args[0] {
	case "add":
		path := filepath.Join(".atteler", "evals", m.sessionState.ID+".json")
		_ = os.MkdirAll(filepath.Dir(path), 0o750)
		data, _ := json.MarshalIndent(m.sessionState, "", "  ")
		if err := os.WriteFile(path, data, 0o600); err != nil {
			return m, tea.Println(errStyle.Render(err.Error())), true
		}
		return m, tea.Println(dimStyle.Render("added eval " + path)), true
	case "run":
		next, cmd := m.runShellCommand("find .atteler/evals -type f -name '*.json' -maxdepth 1 -print 2>/dev/null | wc -l | awk '{print \"eval cases: \"$1}'")
		if nm, ok := next.(model); ok {
			return nm, cmd, true
		}
		return m, cmd, true
	}
	return m, tea.Println("usage: /eval add|run"), true
}
