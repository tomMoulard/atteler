package main

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"strconv"
	"strings"
	"sync"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/tommoulard/atteler/pkg/autonomy"
	appconfig "github.com/tommoulard/atteler/pkg/config"
	"github.com/tommoulard/atteler/pkg/contextref"
	"github.com/tommoulard/atteler/pkg/events"
	"github.com/tommoulard/atteler/pkg/session"
)

type eventLineBuffer struct {
	liveCh chan<- tea.Msg
	lines  []string
	mu     sync.Mutex
}

func newEventLineBuffer(liveCh ...chan<- tea.Msg) *eventLineBuffer {
	var ch chan<- tea.Msg
	if len(liveCh) > 0 {
		ch = liveCh[0]
	}

	return &eventLineBuffer{liveCh: ch}
}

func (b *eventLineBuffer) Write(p []byte) (int, error) {
	if b == nil {
		return len(p), nil
	}

	text := strings.TrimRight(string(p), "\r\n")
	if text == "" {
		return len(p), nil
	}

	b.mu.Lock()
	defer b.mu.Unlock()

	for line := range strings.SplitSeq(text, "\n") {
		line = strings.TrimRight(line, "\r")
		if line == "" {
			continue
		}

		b.lines = append(b.lines, line)
		if b.liveCh != nil {
			b.liveCh <- llmEventLineMsg{line: line}
		}
	}

	return len(p), nil
}

func (b *eventLineBuffer) Lines() []string {
	if b == nil {
		return nil
	}

	b.mu.Lock()
	defer b.mu.Unlock()

	return append([]string(nil), b.lines...)
}

func eventLineCommands(lines []string) []tea.Cmd {
	cmds := make([]tea.Cmd, 0, len(lines))
	for _, line := range lines {
		if strings.TrimSpace(line) == "" {
			continue
		}

		cmds = append(cmds, tea.Println(dimStyle.Render(line)))
	}

	return cmds
}

func saveSession(ctx context.Context, store *session.Store, sessionState session.Session, runner *events.Runner) tea.Cmd {
	return func() tea.Msg {
		if store == nil || sessionState.ID == "" {
			return sessionSavedMsg{}
		}

		if !sessionPersistenceAllowed(sessionState) {
			return sessionSavedMsg{}
		}

		if err := authorizeSessionStoreWrite(ctx, store, sessionState, "save TUI session"); err != nil {
			return sessionSavedMsg{err: err}
		}

		if err := store.Save(sessionState); err != nil {
			return sessionSavedMsg{err: err}
		}

		emitHookWarning(ctx, runner, events.Event{
			Type:        events.FileWrite,
			SessionID:   sessionState.ID,
			SessionPath: store.Path(sessionState.ID),
			Agent:       sessionState.DefaultAgent,
			Model:       sessionState.DefaultModel,
			Metadata: map[string]string{
				"autonomy": sessionState.Autonomy,
				"path":     store.Path(sessionState.ID),
				"kind":     "session",
			},
		})

		return sessionSavedMsg{}
	}
}

func sessionPersistenceAllowed(sessionState session.Session) bool {
	return autonomy.Normalize(autonomy.Level(sessionState.Autonomy)).Allows(autonomy.ActionFileWrite)
}

func saveModelPreference(
	ctx context.Context,
	store *appconfig.StateStore,
	cwd string,
	model string,
	reasoningLevel string,
	reasoningSelected bool,
	modelMode string,
	modeSelected bool,
	scope appconfig.ModelScope,
	level autonomy.Level,
	runner *events.Runner,
) tea.Cmd {
	return func() tea.Msg {
		if store == nil {
			return modelPreferenceSavedMsg{scope: scope}
		}

		if !autonomy.Normalize(level).Allows(autonomy.ActionFileWrite) {
			return modelPreferenceSavedMsg{
				err:   fmt.Errorf("%s", autonomy.DenialMessage(level, autonomy.ActionFileWrite, "model preference")),
				scope: scope,
			}
		}

		if err := authorizeStateStoreWrite(ctx, store, "save model preference"); err != nil {
			return modelPreferenceSavedMsg{err: err, scope: scope}
		}

		_, err := store.Update(func(state *appconfig.State) error {
			state.SetModel(scope, cwd, model)

			if reasoningSelected {
				state.SetReasoningLevel(scope, cwd, reasoningLevel)
			}

			if modeSelected {
				state.SetModelMode(scope, cwd, modelMode)
			}

			return nil
		})
		if err != nil {
			return modelPreferenceSavedMsg{err: err, scope: scope}
		}

		emitHookWarning(ctx, runner, events.Event{
			Type: events.FileWrite,
			Metadata: map[string]string{
				"autonomy": level.String(),
				"path":     store.Path(),
				"kind":     "state",
			},
		})

		return modelPreferenceSavedMsg{scope: scope}
	}
}

func savePromptSuggestionPreference(
	ctx context.Context,
	store *appconfig.StateStore,
	cwd string,
	preference appconfig.PromptSuggestionPreference,
	scope appconfig.ModelScope,
	level autonomy.Level,
	runner *events.Runner,
) tea.Cmd {
	return func() tea.Msg {
		if store == nil {
			return promptSuggestionPreferenceSavedMsg{scope: scope}
		}

		if err := authorizeStateStoreWrite(ctx, store, "save prompt suggestion preference"); err != nil {
			return promptSuggestionPreferenceSavedMsg{err: err, scope: scope}
		}

		_, err := store.Update(func(state *appconfig.State) error {
			state.SetPromptSuggestionPreference(scope, cwd, preference)

			return nil
		})
		if err != nil {
			return promptSuggestionPreferenceSavedMsg{err: err, scope: scope}
		}

		emitHookWarning(ctx, runner, events.Event{
			Type: events.FileWrite,
			Metadata: map[string]string{
				"autonomy": level.String(),
				"path":     store.Path(),
				"kind":     "state",
			},
		})

		return promptSuggestionPreferenceSavedMsg{scope: scope}
	}
}

func authorizeStateStoreWrite(ctx context.Context, store *appconfig.StateStore, action string) error {
	target := "state store"
	if store != nil {
		target = store.Path()
	}

	return authorizeWritePermission(ctx, action, "atteler.state", target)
}

func emitFileRead(
	ctx context.Context,
	runner *events.Runner,
	sessionID, sessionPath, agentName, modelName string,
	ref contextref.Reference,
) tea.Cmd {
	return emitHook(ctx, runner, fileReadEvent(sessionID, sessionPath, agentName, modelName, ref))
}

func fileReadEvent(
	sessionID, sessionPath, agentName, modelName string,
	ref contextref.Reference,
) events.Event {
	metadata := referenceEventMetadata(ref)

	return events.Event{
		Type:        events.FileRead,
		SessionID:   sessionID,
		SessionPath: sessionPath,
		Agent:       agentName,
		Model:       modelName,
		Metadata:    metadata,
	}
}

func emitContextAdd(
	ctx context.Context,
	runner *events.Runner,
	sessionID, sessionPath, agentName, modelName string,
	ref contextref.Reference,
) tea.Cmd {
	return emitHook(ctx, runner, contextAddEvent(sessionID, sessionPath, agentName, modelName, ref))
}

func contextAddEvent(
	sessionID, sessionPath, agentName, modelName string,
	ref contextref.Reference,
) events.Event {
	metadata := referenceEventMetadata(ref)

	return events.Event{
		Type:        events.ContextAdd,
		SessionID:   sessionID,
		SessionPath: sessionPath,
		Agent:       agentName,
		Model:       modelName,
		Metadata:    metadata,
	}
}

func referenceEventMetadata(ref contextref.Reference) map[string]string {
	metadata := map[string]string{
		"path":      ref.Path,
		"kind":      ref.Kind,
		"bytes":     strconv.Itoa(ref.Bytes),
		"truncated": strconv.FormatBool(ref.Truncated),
	}

	if ref.TokenEstimate.Tokens > 0 || ref.TokenEstimate.UpperBoundTokens > 0 {
		metadata["estimated_tokens"] = strconv.Itoa(ref.TokenEstimate.Tokens)
		metadata["estimated_token_error_bound"] = strconv.Itoa(ref.TokenEstimate.ErrorBoundTokens)
		metadata["estimated_token_upper_bound"] = strconv.Itoa(ref.TokenEstimate.UpperBoundTokens)
	}

	if ref.TokenEstimator != "" {
		metadata["token_estimator"] = ref.TokenEstimator
	}

	if ref.DigestSHA256 != "" {
		metadata["digest_sha256"] = ref.DigestSHA256
	}

	return metadata
}

func emitFileWriteWarning(
	ctx context.Context,
	runner *events.Runner,
	sessionState session.Session,
	path string,
	agentName string,
	kind string,
) {
	emitHookWarning(ctx, runner, events.Event{
		Type:        events.FileWrite,
		SessionID:   sessionState.ID,
		SessionPath: path,
		Agent:       agentName,
		Model:       sessionState.DefaultModel,
		Metadata: map[string]string{
			"autonomy": sessionState.Autonomy,
			"path":     path,
			"kind":     kind,
		},
	})
}

func emitAgentExecute(ctx context.Context, runner *events.Runner, sessionID, sessionPath, agentName, modelName string) tea.Cmd {
	return emitHook(ctx, runner, events.Event{
		Type:        events.AgentExecute,
		SessionID:   sessionID,
		SessionPath: sessionPath,
		Agent:       agentName,
		Model:       modelName,
		Metadata: map[string]string{
			"agent": agentName,
		},
	})
}

func emitHook(ctx context.Context, runner *events.Runner, event events.Event) tea.Cmd {
	return func() tea.Msg {
		if runner == nil {
			return hookMsg{}
		}

		line := runner.FormatLine(event)

		return hookMsg{err: runner.Emit(ctx, event), line: line}
	}
}

func emitHookQuiet(ctx context.Context, runner *events.Runner, event events.Event) tea.Cmd {
	return func() tea.Msg {
		if runner == nil {
			return hookMsg{}
		}

		return hookMsg{err: runner.Emit(ctx, event)}
	}
}

func emitHookWarning(ctx context.Context, runner *events.Runner, event events.Event) {
	if runner == nil {
		return
	}

	if err := runner.Emit(ctx, event); err != nil {
		fmt.Fprintln(os.Stderr, "warning: "+err.Error())
	}
}

func emitFromContextWarning(ctx context.Context, event events.Event) {
	if err := events.EmitFromContext(ctx, event); err != nil {
		slog.Warn("emit hook from context", "event", events.FormatLine(event), "error", err)
	}
}

// ---------------------------------------------------------------------------
// Main
// ---------------------------------------------------------------------------
