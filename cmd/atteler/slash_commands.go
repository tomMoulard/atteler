package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/tommoulard/atteler/pkg/autonomy"
	appconfig "github.com/tommoulard/atteler/pkg/config"
	"github.com/tommoulard/atteler/pkg/events"
	"github.com/tommoulard/atteler/pkg/llm"
	"github.com/tommoulard/atteler/pkg/permission"
	"github.com/tommoulard/atteler/pkg/session"
	"github.com/tommoulard/atteler/pkg/shell"
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

func writeSessionExport(ctx context.Context, s session.Session, path string) error {
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

	if err := authorizeWritePermission(ctx, "export session transcript", "atteler.slash.export", path); err != nil {
		return fmt.Errorf("export session: %w", err)
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

func slashFileWriteAutonomyBlock(m model, detail string) (tea.Cmd, bool) {
	if m.autonomy.Allows(autonomy.ActionFileWrite) {
		return nil, false
	}

	return tea.Println(errStyle.Render(autonomy.DenialMessage(m.autonomy, autonomy.ActionFileWrite, detail))), true
}

func runClearSlashCommand(m model, _ slashNoArgsInput) (model, tea.Cmd, bool) {
	if cmd, blocked := slashFileWriteAutonomyBlock(m, "/clear"); blocked {
		return m, cmd, true
	}

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

	if cmd, blocked := slashFileWriteAutonomyBlock(m, "/model"); blocked {
		return m, cmd, true
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

	if cmd, blocked := slashFileWriteAutonomyBlock(m, "/profile"); blocked {
		return m, cmd, true
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
	if cmd, blocked := slashFileWriteAutonomyBlock(m, "/save"); blocked {
		return m, cmd, true
	}

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

	if path == "-" {
		if err := writeSessionExport(m.ctx, m.sessionState, path); err != nil {
			return m, tea.Println(errStyle.Render("export: " + err.Error())), true
		}

		return m, tea.Println(dimStyle.Render("exported " + path)), true
	}

	cmd := runPermissionPromptedLocalActionCmd(
		m.ctx,
		m.permissionPolicy,
		func(ctx context.Context) error {
			return writeSessionExport(ctx, m.sessionState, path)
		},
		"exported "+path,
	)

	return m, cmd, true
}

func runRetrySlashCommand(m model, _ slashNoArgsInput) (model, tea.Cmd, bool) {
	if cmd, blocked := slashFileWriteAutonomyBlock(m, "/retry"); blocked {
		return m, cmd, true
	}

	return m.regenerateLast()
}

func runEditSlashCommand(m model, _ slashNoArgsInput) (model, tea.Cmd, bool) {
	if cmd, blocked := slashFileWriteAutonomyBlock(m, "/edit"); blocked {
		return m, cmd, true
	}

	return m.editLastUser()
}

func runForkSlashCommand(m model, input slashForkInput) (model, tea.Cmd, bool) {
	if cmd, blocked := slashFileWriteAutonomyBlock(m, "/fork"); blocked {
		return m, cmd, true
	}

	return m.forkAt(input)
}

func runTokensSlashCommand(m model, _ slashNoArgsInput) (model, tea.Cmd, bool) {
	return m, tea.Println(formatTokenUsageSummary(m.tokenUsage)), true
}

func runSearchSlashCommand(m model, input slashSearchInput) (model, tea.Cmd, bool) {
	return m, tea.Println(searchMessages(m.history, strings.ToLower(input.Query))), true
}

func runPinSlashCommand(m model, input slashMessageNumberInput) (model, tea.Cmd, bool) {
	if cmd, blocked := slashFileWriteAutonomyBlock(m, "/pin"); blocked {
		return m, cmd, true
	}

	return m.pinMessage(input, true)
}

func runUnpinSlashCommand(m model, input slashMessageNumberInput) (model, tea.Cmd, bool) {
	if cmd, blocked := slashFileWriteAutonomyBlock(m, "/unpin"); blocked {
		return m, cmd, true
	}

	return m.pinMessage(input, false)
}

func runContextSlashCommand(m model, input slashContextInput) (model, tea.Cmd, bool) {
	if input.Prune {
		if cmd, blocked := slashFileWriteAutonomyBlock(m, "/context prune"); blocked {
			return m, cmd, true
		}

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

func runSuggestionsSlashCommand(m model, input slashSuggestionsInput) (model, tea.Cmd, bool) {
	if input.Show {
		return m, tea.Println(m.promptSuggestionsSummary()), true
	}

	if cmd, blocked := slashFileWriteAutonomyBlock(m, "/suggestions"); blocked {
		return m, cmd, true
	}

	switch input.Mode {
	case string(promptSuggestionConsentLocalOnly), "local":
		scope := m.promptSuggestionLocalOnlyScope()
		m.promptSuggestionConsent = promptSuggestionConsentLocalOnly
		m.sessionState.PromptSuggestions = string(appconfig.PromptSuggestionPreferenceLocalOnly)
		m.clearIdleSuggestion()

		// Persist before printing confirmation so a user can quit immediately
		// after seeing the opt-in/out result without racing Bubble Tea commands.
		sessionSaveMsg := saveSession(m.ctx, m.sessionStore, m.sessionState, m.hookRunner)()
		preferenceSaveMsg := savePromptSuggestionPreference(
			m.ctx,
			m.stateStore,
			m.cwd,
			appconfig.PromptSuggestionPreferenceLocalOnly,
			scope,
			m.autonomy,
			m.hookRunner,
		)()

		return m, tea.Batch(
			func() tea.Msg { return sessionSaveMsg },
			func() tea.Msg { return preferenceSaveMsg },
			tea.Println(dimStyle.Render("model-backed idle suggestions disabled (local-only).")),
		), true
	case string(promptSuggestionConsentSession):
		m.promptSuggestionConsent = promptSuggestionConsentSession
		m.sessionState.PromptSuggestions = string(appconfig.PromptSuggestionPreferenceModelBacked)

		// Persist before printing confirmation so the session opt-in is durable.
		sessionSaveMsg := saveSession(m.ctx, m.sessionStore, m.sessionState, m.hookRunner)()

		return m, tea.Batch(
			func() tea.Msg { return sessionSaveMsg },
			tea.Println(warnStyle.Render(m.promptSuggestionOptInWarning("for this session"))),
		), true
	case string(promptSuggestionConsentFolder):
		m.promptSuggestionConsent = promptSuggestionConsentFolder
		m.sessionState.PromptSuggestions = ""

		// Persist before printing confirmation so the folder opt-in is durable.
		sessionSaveMsg := saveSession(m.ctx, m.sessionStore, m.sessionState, m.hookRunner)()
		preferenceSaveMsg := savePromptSuggestionPreference(
			m.ctx,
			m.stateStore,
			m.cwd,
			appconfig.PromptSuggestionPreferenceModelBacked,
			appconfig.ModelScopeFolder,
			m.autonomy,
			m.hookRunner,
		)()

		return m, tea.Batch(
			func() tea.Msg { return sessionSaveMsg },
			func() tea.Msg { return preferenceSaveMsg },
			tea.Println(warnStyle.Render(m.promptSuggestionOptInWarning("for this folder"))),
		), true
	case string(promptSuggestionConsentGlobal):
		m.promptSuggestionConsent = promptSuggestionConsentGlobal
		m.sessionState.PromptSuggestions = ""

		// Persist before printing confirmation so the global opt-in is durable.
		sessionSaveMsg := saveSession(m.ctx, m.sessionStore, m.sessionState, m.hookRunner)()
		preferenceSaveMsg := savePromptSuggestionPreference(
			m.ctx,
			m.stateStore,
			m.cwd,
			appconfig.PromptSuggestionPreferenceModelBacked,
			appconfig.ModelScopeGlobal,
			m.autonomy,
			m.hookRunner,
		)()

		return m, tea.Batch(
			func() tea.Msg { return sessionSaveMsg },
			func() tea.Msg { return preferenceSaveMsg },
			tea.Println(warnStyle.Render(m.promptSuggestionOptInWarning("globally"))),
		), true
	default:
		return m, tea.Println(errStyle.Render("suggestions: unknown mode")), true
	}
}

func (m model) promptSuggestionLocalOnlyScope() appconfig.ModelScope {
	switch m.promptSuggestionConsent {
	case promptSuggestionConsentGlobal:
		return appconfig.ModelScopeGlobal
	case promptSuggestionConsentFolder:
		return appconfig.ModelScopeFolder
	default:
		if appconfig.FolderKey(m.cwd) == "" {
			return appconfig.ModelScopeGlobal
		}

		return appconfig.ModelScopeFolder
	}
}

func (m model) promptSuggestionOptInWarning(scope string) string {
	if m.promptLocalOnly {
		return "model-backed idle suggestions saved " + scope + "; current process remains local-only because --prompt-local-only is set."
	}

	destination := m.modelStatusLabel()
	if destination == "" {
		destination = "the active provider"
	}

	return "model-backed idle suggestions enabled " + scope + "; drafts may be sent to " + destination + " with private file/task/issue context omitted."
}

func (m model) promptSuggestionsSummary() string {
	budget := normalizeIdleSuggestionBudget(m.idleSuggestionBudget)
	usedTokens := max(m.idleSuggestionUsage.InputTokens+m.idleSuggestionUsage.OutputTokens, m.idleSuggestionTokens)
	recentRequests := idleSuggestionRecentRequestCount(m.idleSuggestionTimes, time.Now())

	providerCalls := 0
	if m.sessionState.BackgroundSuggestions != nil {
		providerCalls = m.sessionState.BackgroundSuggestions.ProviderCalls
	}

	mode := string(m.promptSuggestionConsent)
	if m.promptLocalOnly {
		mode = string(promptSuggestionConsentLocalOnly) + " (--prompt-local-only)"
	}

	if mode == "" {
		mode = string(promptSuggestionConsentLocalOnly)
	}

	return strings.Join([]string{
		"suggestions: mode=" + mode,
		"model=" + m.modelStatusLabel(),
		fmt.Sprintf("budget=requests≤%d rate≤%d/min input≤%d output≤%d session_tokens≤%d cost≤$%.2f",
			budget.MaxRequestsPerSession,
			budget.MaxRequestsPerMinute,
			budget.MaxInputTokens,
			budget.MaxOutputTokens,
			budget.MaxSessionTokens,
			budget.MaxEstimatedCostUSD,
		),
		fmt.Sprintf("usage=requests=%d/%d rate=%d/%d_per_min provider_calls=%d responses=%d session_tokens=%d/%d cost=$%.6f/$%.2f",
			m.idleSuggestionRequests,
			budget.MaxRequestsPerSession,
			recentRequests,
			budget.MaxRequestsPerMinute,
			providerCalls,
			m.idleSuggestionUsage.Responses,
			usedTokens,
			budget.MaxSessionTokens,
			m.idleSuggestionCostUSD,
			budget.MaxEstimatedCostUSD,
		),
		"privacy=file/task/issue context omitted from provider-backed suggestions",
	}, "\n")
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
	m.sessionState.AgentLoopBudget = m.agentLoopBudget
	m.sessionState.Autonomy = m.autonomy.String()

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
	estimate, estimatorSummary := estimateMessagesForModel(m.registry, m.selectedModel, m.history)

	return fmt.Sprintf("messages=%d pinned=%d tokens=%s upper_bound=%s error_bound=%s estimator=%s", len(m.history), len(m.pinnedMessages), formatTokenCount(estimate.Tokens), formatTokenCount(estimate.UpperBoundTokens), formatTokenCount(estimate.ErrorBoundTokens), estimatorSummary)
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
	if !m.autonomy.Allows(autonomy.ActionFileWrite) {
		return m, tea.Println(errStyle.Render(autonomy.DenialMessage(m.autonomy, autonomy.ActionFileWrite, "/save-code"))), true
	}

	bs := extractCodeBlocks(lastAssistantContent(m.history))
	if input.Block < 1 || input.Block > len(bs) {
		return m, tea.Println(errStyle.Render("invalid code block")), true
	}

	cmd := runPermissionGatedLocalActionCmd(
		m.ctx,
		m.permissionPolicy,
		writePermissionRequest("save code block", "atteler.slash.save_code", input.Path),
		func() error {
			if err := os.WriteFile(input.Path, []byte(bs[input.Block-1]), 0o600); err != nil {
				return fmt.Errorf("save code block: %w", err)
			}

			return nil
		},
		"saved "+input.Path,
	)

	return m, cmd, true
}

func runCopySlashCommand(m model, input slashCopyInput) (model, tea.Cmd, bool) {
	if !m.autonomy.Allows(autonomy.ActionMutatingShell) {
		return m, tea.Println(errStyle.Render(autonomy.DenialMessage(m.autonomy, autonomy.ActionMutatingShell, "/copy clipboard"))), true
	}

	text := lastAssistantContent(m.history)
	if input.Target == sessionCommandName {
		text = plainTranscript(m.history)
	}

	cmd := runPermissionPromptedLocalActionCmd(
		m.ctx,
		m.permissionPolicy,
		func(ctx context.Context) error {
			if err := copyToClipboard(ctx, text); err != nil {
				return fmt.Errorf("copy: %w", err)
			}

			return nil
		},
		"copied",
	)

	return m, cmd, true
}

func runCopyCodeSlashCommand(m model, input slashCopyCodeInput) (model, tea.Cmd, bool) {
	if !m.autonomy.Allows(autonomy.ActionMutatingShell) {
		return m, tea.Println(errStyle.Render(autonomy.DenialMessage(m.autonomy, autonomy.ActionMutatingShell, "/copy-code clipboard"))), true
	}

	text := lastAssistantContent(m.history)
	blocks := extractCodeBlocks(text)

	if input.Block < 1 || input.Block > len(blocks) {
		return m, tea.Println(errStyle.Render("invalid code block")), true
	}

	cmd := runPermissionPromptedLocalActionCmd(
		m.ctx,
		m.permissionPolicy,
		func(ctx context.Context) error {
			if err := copyToClipboard(ctx, blocks[input.Block-1]); err != nil {
				return fmt.Errorf("copy: %w", err)
			}

			return nil
		},
		"copied",
	)

	return m, cmd, true
}

func copyToClipboard(ctx context.Context, text string) error {
	return copyToClipboardWithAudit(ctx, text, shell.AuditContext{Caller: "atteler.clipboard"})
}

func slashCommandAuditContext(m model, caller string) shell.AuditContext {
	return shell.AuditContext{
		Caller:      caller,
		SessionID:   m.sessionState.ID,
		SessionPath: m.sessionPath,
		Autonomy:    m.autonomy.String(),
	}
}

func copyToClipboardWithAudit(ctx context.Context, text string, audit shell.AuditContext) error {
	if autonomy.Normalize(autonomy.Level(audit.Autonomy)) == autonomy.Low {
		return errors.New("clipboard command blocked: autonomy low is advisory-only and blocks clipboard command execution; rerun with --autonomy medium or higher")
	}

	if ctx == nil {
		return errors.New("context is required")
	}

	if err := ctx.Err(); err != nil {
		return fmt.Errorf("context already done: %w", err)
	}

	if err := authorizeReadPermission(ctx, "locate clipboard command", "atteler.clipboard", "PATH"); err != nil {
		return fmt.Errorf("authorize clipboard command lookup: %w", err)
	}

	cmds := [][]string{{"pbcopy"}, {"wl-copy"}, {"xclip", "-selection", "clipboard"}}

	if strings.TrimSpace(audit.Caller) == "" {
		audit.Caller = "atteler.clipboard"
	}

	for _, c := range cmds {
		if _, err := execLookPath(c[0]); err != nil {
			continue
		}

		cmd, invocation, err := shell.CommandContext(ctx, shell.CommandOptions{
			Program: c[0],
			Args:    c[1:],
			Stdin:   strings.NewReader(text),
			Mode:    shell.ModeCaptured,
			PermissionOperations: []permission.Operation{{
				Kind:   permission.OperationWrite,
				Action: "copy to clipboard",
				Target: c[0],
				Source: "atteler.clipboard",
			}},
			Audit: shell.AuditContext{Caller: "atteler.clipboard"},
		})
		if err != nil {
			return fmt.Errorf("authorize %s: %w", c[0], err)
		}

		runErr := cmd.Run()
		if finishErr := invocation.Finish(shell.FinishOptions{Error: runErr, OutputCapture: shell.OutputSensitive, OutputNote: "clipboard input is intentionally not captured"}); finishErr != nil {
			return fmt.Errorf("finish %s: %w", c[0], finishErr)
		}

		if runErr != nil {
			return fmt.Errorf("run %s: %w", c[0], runErr)
		}

		return nil
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

const gitApplyPatchDisplayCommand = "git apply --check - && git apply -"

func (m model) runGitApplyPatch(patch string) (model, tea.Cmd) {
	if decision := llm.BashAutonomyDecision(m.autonomy, gitApplyPatchDisplayCommand); decision.Verdict == llm.ToolPolicyDeny {
		return m, tea.Println(errStyle.Render(decision.Reason))
	}

	line := userLabel.Render("$") + " " + gitApplyPatchDisplayCommand
	ctx, cancel := context.WithCancel(m.ctx)
	m.cancel = cancel
	tickCmd := m.startRunningTask("apply-patch")

	displayCommand := gitApplyPatchDisplayCommand
	confirmCh := make(chan agentLoopConfirmRequest, 1)
	responseCh := make(chan bool, 1)
	ctx = contextWithTUIPermissionPrompt(ctx, m.permissionPolicy, confirmCh, responseCh)
	commandEvent := events.Event{
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
	}
	applyCmd := runGitApplyPatchCmd(ctx, patch, m.cwd, slashCommandAuditContext(m, "atteler.slash.apply_patch.git"), func() {
		emitHookWarning(m.ctx, m.hookRunner, commandEvent)
	})
	wrappedApplyCmd := func() tea.Msg {
		defer close(confirmCh)

		return applyCmd()
	}

	return m, tea.Batch(tea.Sequence(
		tea.Println(line),
		wrappedApplyCmd,
	), tickCmd, listenForCheckpoint(confirmCh, responseCh))
}

func runGitApplyPatchCmd(ctx context.Context, patch, dir string, options ...any) tea.Cmd {
	audit, startCallback := gitApplyPatchOptions(options...)

	return func() tea.Msg {
		autonomyLevel := autonomy.Normalize(autonomy.Level(audit.Autonomy))
		if decision := llm.BashAutonomyDecision(autonomyLevel, gitApplyPatchDisplayCommand); decision.Verdict == llm.ToolPolicyDeny {
			err := errors.New(decision.Reason)

			return shellResultMsg{
				err:         err,
				completedAt: time.Now(),
				command:     gitApplyPatchDisplayCommand,
				stderr:      err.Error(),
			}
		}

		stdout, stderr, err := runGitApply(ctx, dir, []string{"--check", "-"}, patch, audit, nil)
		if err == nil {
			var applyStdout, applyStderr string

			applyStdout, applyStderr, err = runGitApply(ctx, dir, []string{"-"}, patch, audit, startCallback)
			stdout += applyStdout
			stderr += applyStderr
		}

		return shellResultMsg{
			err:         err,
			completedAt: time.Now(),
			command:     gitApplyPatchDisplayCommand,
			stdout:      stdout,
			stderr:      stderr,
		}
	}
}

func gitApplyPatchOptions(options ...any) (audit shell.AuditContext, startCallback func()) {
	audit = shell.AuditContext{Caller: "atteler.slash.apply_patch"}

	for _, option := range options {
		switch value := option.(type) {
		case nil:
		case shell.AuditContext:
			audit = value
		case func():
			startCallback = value
		}
	}

	return audit, startCallback
}

func runGitApply(
	ctx context.Context,
	dir string,
	args []string,
	patch string,
	audit shell.AuditContext,
	startCallback func(),
) (stdoutText, stderrText string, err error) {
	var stdout, stderr bytes.Buffer

	cmd, invocation, err := shell.CommandContext(ctx, shell.CommandOptions{
		Program:       "git",
		Args:          append([]string{"apply"}, args...),
		Dir:           dir,
		Stdin:         strings.NewReader(patch),
		Stdout:        &stdout,
		Stderr:        &stderr,
		Mode:          shell.ModeCaptured,
		Audit:         audit,
		StartCallback: startCallback,
	})
	if err != nil {
		return stdout.String(), stderr.String(), fmt.Errorf("git apply %s authorize: %w", strings.Join(args, " "), err)
	}

	runErr := cmd.Run()
	if finishErr := invocation.Finish(shell.FinishOptions{Stdout: stdout.String(), Stderr: stderr.String(), Error: runErr, OutputCapture: shell.OutputCaptured}); finishErr != nil {
		return stdout.String(), stderr.String(), fmt.Errorf("git apply %s audit: %w", strings.Join(args, " "), finishErr)
	}

	if runErr != nil {
		return stdout.String(), stderr.String(), fmt.Errorf("git apply %s: %w", strings.Join(args, " "), runErr)
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
		if !m.autonomy.Allows(autonomy.ActionFileWrite) {
			return m, tea.Println(errStyle.Render(autonomy.DenialMessage(m.autonomy, autonomy.ActionFileWrite, "/eval add"))), true
		}

		path := filepath.Join(evalCasesDir(m.cwd), m.sessionState.ID+".json")

		data, err := json.MarshalIndent(m.sessionState, "", "  ")
		if err != nil {
			return m, tea.Println(errStyle.Render("eval add: " + err.Error())), true
		}

		cmd := runPermissionGatedLocalActionCmd(
			m.ctx,
			m.permissionPolicy,
			writePermissionRequest("add eval case", "atteler.slash.eval", path),
			func() error {
				if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
					return fmt.Errorf("create eval dir: %w", err)
				}

				if err := os.WriteFile(path, data, 0o600); err != nil {
					return fmt.Errorf("write eval case: %w", err)
				}

				return nil
			},
			"added eval "+path,
		)

		return m, cmd, true
	case "run":
		dir := evalCasesDir(m.cwd)
		cmd := runPermissionPromptedLocalMessageCmd(
			m.ctx,
			m.permissionPolicy,
			func(ctx context.Context) (string, error) {
				if err := authorizeReadPermission(ctx, "count eval cases", "atteler.slash.eval", dir); err != nil {
					return "", fmt.Errorf("eval run: %w", err)
				}

				count, err := countEvalCases(dir)
				if err != nil {
					return "", fmt.Errorf("eval run: %w", err)
				}

				return fmt.Sprintf("eval cases: %d", count), nil
			},
		)

		return m, cmd, true
	}

	return m, tea.Println("usage: /eval add|run"), true
}

func evalCasesDir(cwd string) string {
	if strings.TrimSpace(cwd) == "" {
		return filepath.Join(".atteler", "evals")
	}

	return filepath.Join(cwd, ".atteler", "evals")
}

func writePermissionRequest(action, source, target string) permission.Request {
	return permission.Request{
		Action: action,
		Source: source,
		Target: target,
		Operations: []permission.Operation{{
			Kind:   permission.OperationWrite,
			Action: action,
			Source: source,
			Target: target,
		}},
	}
}

func runPermissionGatedLocalActionCmd(
	ctx context.Context,
	policy *permission.Policy,
	request permission.Request,
	run func() error,
	success string,
) tea.Cmd {
	return runPermissionPromptedLocalActionCmd(ctx, policy, func(ctx context.Context) error {
		decision := permission.Evaluate(ctx, policy, request)
		if !decision.Allowed {
			return &permission.Error{Decision: decision}
		}

		return run()
	}, success)
}

func runPermissionPromptedLocalActionCmd(
	ctx context.Context,
	policy *permission.Policy,
	run func(context.Context) error,
	success string,
) tea.Cmd {
	return runPermissionPromptedLocalMessageCmd(ctx, policy, func(ctx context.Context) (string, error) {
		if err := run(ctx); err != nil {
			return "", err
		}

		return success, nil
	})
}

func runPermissionPromptedLocalMessageCmd(
	ctx context.Context,
	policy *permission.Policy,
	run func(context.Context) (string, error),
) tea.Cmd {
	var requestCh chan agentLoopConfirmRequest

	var responseCh chan bool

	if permissionPolicyNeedsPrompt(policy) {
		requestCh = make(chan agentLoopConfirmRequest, 1)
		responseCh = make(chan bool, 1)
		ctx = contextWithTUIPermissionPrompt(ctx, policy, requestCh, responseCh)
	} else {
		ctx = permission.ContextWithPolicy(ctx, policy)
	}

	actionCmd := func() tea.Msg {
		if requestCh != nil {
			defer close(requestCh)
		}

		message, err := run(ctx)
		if err != nil {
			return tea.Println(errStyle.Render(err.Error()))()
		}

		return tea.Println(dimStyle.Render(message))()
	}

	if requestCh == nil {
		return actionCmd
	}

	return tea.Batch(actionCmd, listenForCheckpoint(requestCh, responseCh))
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
