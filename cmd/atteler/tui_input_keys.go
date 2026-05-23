package main

import (
	"fmt"
	"io"
	"strconv"
	"strings"

	"github.com/charmbracelet/bubbles/key"
	"github.com/charmbracelet/bubbles/textarea"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/x/ansi"
)

const promptInputHelp = "Enter to send, Shift/Alt+Enter newline, Ctrl+O to pick model"

const (
	terminalShiftModifier = 1 << iota
	terminalAltModifier
	terminalCtrlModifier
	terminalSuperModifier
	terminalHyperModifier
	terminalMetaModifier
	terminalCapsLockModifier
	terminalNumLockModifier
)

type terminalCSIUKey struct {
	code     int
	modifier int
}

func enableTerminalShiftEnterReporting(w io.Writer) func() {
	if w == nil {
		return func() {}
	}

	// Request only disambiguated escape codes so modified Enter can be
	// distinguished without making regular text input arrive as CSI-u escapes.
	fmt.Fprint(w, ansi.PushKittyKeyboard(ansi.KittyDisambiguateEscapeCodes))

	return func() {
		fmt.Fprint(w, ansi.PopKittyKeyboard(1))
	}
}

func newPromptTextarea() textarea.Model {
	ta := textarea.New()
	ta.Placeholder = "Send a message (" + promptInputHelp + ")"
	ta.Focus()
	ta.CharLimit = 0 // unlimited
	ta.ShowLineNumbers = false
	ta.SetHeight(3)

	// Remap newline insertion so plain Enter can remain the submit key.
	// Shift+Enter is recognized by terminals that report modified Enter;
	// Alt+Enter remains as a fallback for terminals that collapse Shift+Enter
	// to an unmodified Enter byte.
	ta.KeyMap.InsertNewline = key.NewBinding(
		key.WithKeys(keyShiftEnter, keyAltEnter),
		key.WithHelp("shift/alt+enter", "insert newline"),
	)

	return ta
}

func (m model) insertInputNewline() (tea.Model, tea.Cmd) {
	var taCmd tea.Cmd

	m.textarea, taCmd = m.textarea.Update(tea.KeyMsg{Type: tea.KeyEnter, Alt: true})
	cmds := []tea.Cmd{taCmd}
	m.promptHistoryCursor = -1
	m.promptHistoryDraft = ""
	m.completionOpen = false
	m.completionItems = nil
	m.clearIdleSuggestion()

	if cmd := m.scheduleIdleSuggestion(); cmd != nil {
		cmds = append(cmds, cmd)
	}

	return m, tea.Batch(cmds...)
}

func isTerminalInputNewlineMsg(msg fmt.Stringer) bool {
	keyName := msg.String()

	return isShiftEnterKeyName(keyName) || isAltEnterKeyName(keyName)
}

func terminalControlKeyMsg(msg tea.Msg) (tea.KeyMsg, bool) {
	if _, ok := msg.(tea.KeyMsg); ok {
		return tea.KeyMsg{}, false
	}

	stringer, ok := msg.(fmt.Stringer)
	if !ok {
		return tea.KeyMsg{}, false
	}

	payload, ok := terminalCSIPayload(stringer.String())
	if !ok {
		return tea.KeyMsg{}, false
	}

	return terminalControlCSIKeyMsg(payload)
}

func terminalControlCSIKeyMsg(payload string) (tea.KeyMsg, bool) {
	csiKey, ok := parseTerminalCSIUKey(payload)
	if !ok {
		return tea.KeyMsg{}, false
	}

	return terminalControlKeyMsgForCode(csiKey.code, csiKey.modifier)
}

func parseTerminalCSIUKey(payload string) (terminalCSIUKey, bool) {
	if !strings.HasSuffix(payload, "u") {
		return terminalCSIUKey{}, false
	}

	fields := strings.Split(strings.TrimSuffix(payload, "u"), ";")
	if len(fields) == 0 || len(fields) > 2 {
		return terminalCSIUKey{}, false
	}

	code, err := strconv.Atoi(fields[0])
	if err != nil {
		return terminalCSIUKey{}, false
	}

	modifier := 0

	if len(fields) == 2 {
		var ok bool

		modifier, ok = terminalModifierBits(fields[1])
		if !ok {
			return terminalCSIUKey{}, false
		}
	}

	return terminalCSIUKey{code: code, modifier: modifier}, true
}

func terminalControlKeyMsgForCode(code, modifier int) (tea.KeyMsg, bool) {
	effectiveModifier := modifier &^ (terminalCapsLockModifier | terminalNumLockModifier)
	switch {
	case code == 13 && effectiveModifier == 0:
		return tea.KeyMsg{Type: tea.KeyEnter}, true
	case code == 27 && effectiveModifier == 0:
		return tea.KeyMsg{Type: tea.KeyEsc}, true
	case code == 9 && effectiveModifier == 0:
		return tea.KeyMsg{Type: tea.KeyTab}, true
	case code == 9 && effectiveModifier == terminalShiftModifier:
		return tea.KeyMsg{Type: tea.KeyShiftTab}, true
	case code == 127 && effectiveModifier == 0:
		return tea.KeyMsg{Type: tea.KeyBackspace}, true
	default:
		return terminalCtrlLetterKeyMsg(code, effectiveModifier)
	}
}

func terminalCtrlLetterKeyMsg(code, modifier int) (tea.KeyMsg, bool) {
	if modifier != terminalCtrlModifier {
		return tea.KeyMsg{}, false
	}

	if code >= 'A' && code <= 'Z' {
		code += 'a' - 'A'
	}

	if code < 'a' || code > 'z' {
		return tea.KeyMsg{}, false
	}

	return tea.KeyMsg{Type: tea.KeyType(code - 'a' + 1)}, true
}

func isShiftEnterKeyName(keyName string) bool {
	return isModifiedEnterKeyName(
		keyName,
		keyShiftEnter,
		terminalShiftModifier,
		terminalAltModifier|terminalCtrlModifier|terminalSuperModifier|
			terminalHyperModifier|terminalMetaModifier,
	)
}

func isAltEnterKeyName(keyName string) bool {
	return isModifiedEnterKeyName(
		keyName,
		keyAltEnter,
		terminalAltModifier,
		terminalShiftModifier|terminalCtrlModifier|terminalSuperModifier|
			terminalHyperModifier|terminalMetaModifier,
	)
}

func terminalCSIPayload(keyName string) (string, bool) {
	if payload, ok := bubbleTeaUnknownCSIPayload(keyName); ok {
		return payload, true
	}

	return strings.CutPrefix(keyName, "\x1b[")
}

func bubbleTeaUnknownCSIPayload(keyName string) (string, bool) {
	const (
		prefix = "?CSI["
		suffix = "]?"
	)

	if !strings.HasPrefix(keyName, prefix) || !strings.HasSuffix(keyName, suffix) {
		return "", false
	}

	fields := strings.Fields(strings.TrimSuffix(strings.TrimPrefix(keyName, prefix), suffix))
	if len(fields) == 0 {
		return "", false
	}

	payload := make([]byte, 0, len(fields))
	for _, field := range fields {
		value, err := strconv.Atoi(field)
		if err != nil || value < 0 || value > 255 {
			return "", false
		}

		payload = append(payload, byte(value))
	}

	return string(payload), true
}

func isModifiedEnterKeyName(keyName, nativeName string, requiredModifiers, disallowedModifiers int) bool {
	if keyName == nativeName {
		return true
	}

	payload, ok := terminalCSIPayload(keyName)
	if !ok {
		return false
	}

	body, ok := strings.CutSuffix(payload, "u")
	if ok {
		fields := strings.Split(body, ";")

		return len(fields) == 2 &&
			fields[0] == "13" &&
			terminalModifierFieldMatches(fields[1], requiredModifiers, disallowedModifiers)
	}

	body, ok = strings.CutSuffix(payload, "~")
	if !ok {
		return false
	}

	fields := strings.Split(body, ";")
	switch {
	case len(fields) == 2 && fields[0] == "13":
		return terminalModifierFieldMatches(fields[1], requiredModifiers, disallowedModifiers)
	case len(fields) == 3 && fields[0] == "27" && fields[2] == "13":
		return terminalModifierFieldMatches(fields[1], requiredModifiers, disallowedModifiers)
	default:
		return false
	}
}

func terminalModifierFieldMatches(field string, requiredModifiers, disallowedModifiers int) bool {
	actual, ok := terminalModifierBits(field)
	if !ok {
		return false
	}

	allowedModifiers := requiredModifiers | terminalCapsLockModifier | terminalNumLockModifier

	return actual&requiredModifiers == requiredModifiers &&
		actual&disallowedModifiers == 0 &&
		actual&^allowedModifiers == 0
}

func terminalModifierBits(field string) (int, bool) {
	parts := strings.Split(field, ":")
	if len(parts) > 2 {
		return 0, false
	}

	if len(parts) > 1 && parts[1] != "" && parts[1] != "1" && parts[1] != "2" {
		return 0, false
	}

	if parts[0] == "" {
		return 0, true
	}

	modifier, err := strconv.Atoi(parts[0])
	if err != nil || modifier < 1 {
		return 0, false
	}

	return modifier - 1, true
}
