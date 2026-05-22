package main

import (
	"context"
	"fmt"
	"io"
	"os"
	"sort"
	"strings"

	"github.com/tommoulard/atteler/pkg/events"
	"github.com/tommoulard/atteler/pkg/session"
)

func runInteractive(ctx context.Context, state appState) error {
	fmt.Println(promptStyle.Render("atteler") + dimStyle.Render("  Ctrl+D to quit, Ctrl+O to pick model"))

	if len(state.loadedConfigPaths) > 0 {
		fmt.Println(dimStyle.Render("  Config: " + strings.Join(state.loadedConfigPaths, ", ")))
	}

	fmt.Println(dimStyle.Render("  Session: " + state.sessionState.ID + " (" + state.sessionStore.Path(state.sessionState.ID) + ")"))

	if state.sessionState.Title != "" {
		fmt.Println(dimStyle.Render("  Title: ") + pickerSelectedStyle.Render(state.sessionState.Title))
	}

	if len(state.sessionState.Tags) > 0 {
		fmt.Println(dimStyle.Render("  Tags: ") + pickerSelectedStyle.Render(strings.Join(state.sessionState.Tags, ", ")))
	}

	if len(state.providers) > 0 {
		sort.Strings(state.providers)
		fmt.Println(dimStyle.Render("  Connected providers: ") + pickerProviderStyle.Render(strings.Join(state.providers, ", ")))
	}

	if agents := state.agentRegistry.List(); len(agents) > 0 {
		fmt.Println(dimStyle.Render("  Agents: ") + pickerProviderStyle.Render(strings.Join(agents, ", ")))
	}

	if state.worktreeInfo != nil {
		fmt.Println(dimStyle.Render("  Worktree: ") + pickerProviderStyle.Render(state.worktreeInfo.Path) +
			dimStyle.Render(" (branch ") + pickerSelectedStyle.Render(state.worktreeInfo.Branch) + dimStyle.Render(")"))
	}

	if len(state.sessionState.Messages) > 0 {
		fmt.Println(dimStyle.Render("  Loaded transcript:"))
		printTranscript(state.sessionState)
	}

	// In TUI mode the runner's logger has to stay quiet — stderr writes would
	// bleed onto bubbletea's alt-screen rendering. Replace the stderr-logger
	// runner that loadAppState set up with a logger-less one. Utility commands
	// and one-shot mode keep the stderr logger.
	state.hookRunner = events.NewRunner(state.hookConfig)

	emitHookWarning(ctx, state.hookRunner, events.Event{
		Type:        events.SessionStart,
		SessionID:   state.sessionState.ID,
		SessionPath: state.sessionStore.Path(state.sessionState.ID),
		Agent:       state.selectedAgent,
		Model:       state.selectedModel,
	})

	finalModel, err := runInteractiveProgram(initialModel(
		ctx,
		state.registry,
		state.agentRegistry,
		state.hookRunner,
		state.sessionStore,
		state.stateStore,
		state.sessionState,
		state.contextOptions,
		state.referenceContext,
		state.sessionStore.Path(state.sessionState.ID),
		state.cwd,
		state.selectedModel,
		state.selectedAgent,
		state.fallbackModels,
		state.generationDefaults,
		state.generationOverrides,
		state.agentLoopBudget,
		state.agentLoopCheckpointInterval,
		state.maxInputTokens,
		state.modelLocked,
		state.promptLocalOnly,
		state.worktreeInfo,
	))

	// Once the program exits, restore the stderr logger so SessionEnd / Error
	// events are visible after the TUI has released the screen.
	state.hookRunner = events.NewRunnerWithLogger(state.hookConfig, os.Stderr)

	if err != nil {
		emitHookWarning(ctx, state.hookRunner, events.Event{
			Type:        events.Error,
			SessionID:   state.sessionState.ID,
			SessionPath: state.sessionStore.Path(state.sessionState.ID),
			Agent:       state.selectedAgent,
			Model:       state.selectedModel,
			Error:       err.Error(),
		})

		return fmt.Errorf("run TUI: %w", err)
	}

	finalSession := state.sessionState

	if m, ok := finalModel.(model); ok {
		printTokenUsageSummary(os.Stderr, m.tokenUsage)
		finalSession = m.sessionState
	}

	if state.sessionStore != nil && finalSession.ID != "" {
		if err := state.sessionStore.Save(finalSession); err != nil {
			fmt.Fprintln(os.Stderr, "warning: could not save session on exit: "+err.Error())
		} else {
			emitFileWriteWarning(ctx, state.hookRunner, finalSession, state.sessionStore.Path(finalSession.ID), finalSession.DefaultAgent, "session")
		}

		printSessionReuseHint(os.Stderr, resolveSpawnBinary(""), state.sessionStore, finalSession.ID)
	}

	emitHookWarning(ctx, state.hookRunner, events.Event{
		Type:        events.SessionEnd,
		SessionID:   finalSession.ID,
		SessionPath: state.sessionStore.Path(finalSession.ID),
		Agent:       finalSession.DefaultAgent,
		Model:       finalSession.DefaultModel,
	})

	finalizeWorktree(ctx, &state)

	return nil
}

func printSessionReuseHint(w io.Writer, binary string, store *session.Store, sessionID string) {
	command := formatSessionReuseCommand(binary, store, sessionID)
	if command == "" || w == nil {
		return
	}

	fmt.Fprintln(w, "reuse session: "+command)
}

func formatSessionReuseCommand(binary string, store *session.Store, sessionID string) string {
	sessionID = strings.TrimSpace(sessionID)
	if sessionID == "" {
		return ""
	}

	binary = strings.TrimSpace(binary)
	if binary == "" {
		binary = "atteler"
	}

	args := []string{binary}
	if store != nil && strings.TrimSpace(store.Dir()) != "" {
		args = append(args, "--session-dir", store.Dir())
	}

	args = append(args, "--session-id", sessionID)

	quoted := make([]string, len(args))
	for i, arg := range args {
		if isSimpleShellWord(arg) {
			quoted[i] = arg
		} else {
			quoted[i] = shellQuote(arg)
		}
	}

	return strings.Join(quoted, " ")
}

func isSimpleShellWord(value string) bool {
	if value == "" {
		return false
	}

	for _, r := range value {
		if !isSimpleShellWordRune(r) {
			return false
		}
	}

	return true
}

func isSimpleShellWordRune(r rune) bool {
	return (r >= 'a' && r <= 'z') ||
		(r >= 'A' && r <= 'Z') ||
		(r >= '0' && r <= '9') ||
		strings.ContainsRune("@%_+=:,./-", r)
}
