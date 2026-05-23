package main

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/charmbracelet/bubbles/textarea"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/x/ansi"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/tommoulard/atteler/pkg/agent"
	"github.com/tommoulard/atteler/pkg/config"
	"github.com/tommoulard/atteler/pkg/contextref"
	"github.com/tommoulard/atteler/pkg/events"
	"github.com/tommoulard/atteler/pkg/llm"
	"github.com/tommoulard/atteler/pkg/session"
	"github.com/tommoulard/atteler/pkg/tasklist"
)

func TestFZFInputAndSelection(t *testing.T) {
	t.Parallel()

	items := []pickerItem{
		{provider: "claude-code", model: "claude-opus-4-6"},
		{provider: "codex", model: "gpt-5.5"},
		{provider: "codex", model: "gpt-5.5", reasoning: testReasoningXHigh},
	}

	input := fzfInput(items)
	for _, want := range []string{
		"claude-code/claude-opus-4-6\tclaude-code\tclaude-opus-4-6\t\t\n",
		"codex/gpt-5.5\tcodex\tgpt-5.5\t\t\n",
		"codex/gpt-5.5:xhigh\tcodex\tgpt-5.5\txhigh\t\n",
	} {
		if !strings.Contains(input, want) {
			require.Failf(t, "unexpected failure", "fzf input missing %q in:\n%s", want, input)
		}
	}

	item, ok := parseFZFSelection("codex/gpt-5.5:xhigh\tcodex\tgpt-5.5\txhigh\n", items)
	if !ok {
		require.FailNow(t, "expected fzf selection to parse")
	}

	if item.provider != "codex" || item.model != "gpt-5.5" || item.reasoning != testReasoningXHigh {
		require.Failf(t, "unexpected failure", "selection = %+v, want codex/gpt-5.5:xhigh", item)
	}

	fastItems := []pickerItem{
		{provider: "codex", model: "gpt-5.4", modelMode: llm.ModelModeDefault, reasoning: llm.ReasoningLevelDefault},
		{provider: "codex", model: "gpt-5.4", modelMode: llm.ModelModeFast, reasoning: testReasoningXHigh},
	}
	item, ok = parseFZFSelection("codex/gpt-5.4:mode=fast:effort=xhigh\tcodex\tgpt-5.4\txhigh\tfast\n", fastItems)
	require.True(t, ok)
	assert.Equal(t, llm.ModelModeFast, item.modelMode)
	assert.Equal(t, testReasoningXHigh, item.reasoning)

	item, ok = parseFZFSelection("codex/gpt-5.5\tcodex\tgpt-5.5\n", []pickerItem{
		{provider: "codex", model: "gpt-5.5", reasoning: llm.ReasoningLevelDefault},
		{provider: "codex", model: "gpt-5.5", reasoning: testReasoningXHigh},
	})
	require.True(t, ok)
	assert.Equal(t, llm.ReasoningLevelDefault, item.reasoning)

	if _, ok := parseFZFSelection("", items); ok {
		require.FailNow(t, "empty fzf selection should be canceled")
	}
}

type modelPickerProvider struct {
	name          string
	models        []string
	fetchedModels []string
}

func (p modelPickerProvider) Name() string { return p.name }

func (p modelPickerProvider) Models() []string { return p.models }

func (p modelPickerProvider) FetchModels(context.Context) ([]string, error) {
	return p.fetchedModels, nil
}

func (p modelPickerProvider) HealthCheck(context.Context) error { return nil }

func (p modelPickerProvider) Complete(context.Context, llm.CompleteParams) (*llm.Response, error) {
	return &llm.Response{}, nil
}

func (p modelPickerProvider) ModelContextWindow(string) int { return 0 }

type activityLoggingProvider struct{}

func (p activityLoggingProvider) Name() string { return "activity" }

func (p activityLoggingProvider) Models() []string { return []string{"activity-model"} }

func (p activityLoggingProvider) FetchModels(context.Context) ([]string, error) {
	return p.Models(), nil
}

func (p activityLoggingProvider) HealthCheck(context.Context) error { return nil }

func (p activityLoggingProvider) Complete(ctx context.Context, params llm.CompleteParams) (*llm.Response, error) {
	if err := events.EmitFromContext(ctx, events.Event{
		Type: events.CommandExecute,
		Metadata: map[string]string{
			"command": "fake-provider-command",
		},
	}); err != nil {
		return nil, err
	}

	return &llm.Response{Content: "ok", Model: params.Model}, nil
}

func (p activityLoggingProvider) ModelContextWindow(string) int { return 0 }

type idleSuggestionProvider struct {
	response string
	model    string
}

func (p idleSuggestionProvider) Name() string { return "suggest" }

func (p idleSuggestionProvider) Models() []string { return []string{p.model} }

func (p idleSuggestionProvider) FetchModels(context.Context) ([]string, error) {
	return p.Models(), nil
}

func (p idleSuggestionProvider) HealthCheck(context.Context) error { return nil }

func (p idleSuggestionProvider) Complete(context.Context, llm.CompleteParams) (*llm.Response, error) {
	return &llm.Response{Content: p.response, Model: p.model}, nil
}

func (p idleSuggestionProvider) ModelContextWindow(string) int { return 0 }

type capturingIdleSuggestionProvider struct {
	params   *llm.CompleteParams
	response string
	model    string
}

func (p *capturingIdleSuggestionProvider) Name() string { return "capture" }

func (p *capturingIdleSuggestionProvider) Models() []string { return []string{p.model} }

func (p *capturingIdleSuggestionProvider) FetchModels(context.Context) ([]string, error) {
	return p.Models(), nil
}

func (p *capturingIdleSuggestionProvider) HealthCheck(context.Context) error { return nil }

func (p *capturingIdleSuggestionProvider) Complete(_ context.Context, params llm.CompleteParams) (*llm.Response, error) {
	p.params = &params

	return &llm.Response{Content: p.response, Model: p.model}, nil
}

func (p *capturingIdleSuggestionProvider) ModelContextWindow(string) int { return 0 }

type cancelAwareIdleSuggestionProvider struct {
	model  string
	called bool
}

func (p *cancelAwareIdleSuggestionProvider) Name() string { return "cancel" }

func (p *cancelAwareIdleSuggestionProvider) Models() []string { return []string{p.model} }

func (p *cancelAwareIdleSuggestionProvider) FetchModels(context.Context) ([]string, error) {
	return p.Models(), nil
}

func (p *cancelAwareIdleSuggestionProvider) HealthCheck(context.Context) error { return nil }

func (p *cancelAwareIdleSuggestionProvider) Complete(ctx context.Context, params llm.CompleteParams) (*llm.Response, error) {
	p.called = true

	if err := ctx.Err(); err != nil {
		return nil, err
	}

	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-time.After(10 * time.Millisecond):
		return &llm.Response{Content: "unused", Model: params.Model}, nil
	}
}

func (p *cancelAwareIdleSuggestionProvider) ModelContextWindow(string) int { return 0 }

func TestCallLLMBuffersProviderActivityEvents(t *testing.T) {
	t.Parallel()

	registry := llm.NewRegistry()
	registry.Register(activityLoggingProvider{})

	msg, ok := callLLM(context.Background(), registry, llmRequest{
		eventBase: events.Event{
			SessionID: "session-1",
			Model:     "activity/activity-model",
		},
		hookRunner: events.NewRunner(nil),
		model:      "activity/activity-model",
		messages: []llm.Message{{
			Role:    llm.RoleUser,
			Content: "hello",
		}},
	})().(llmResponseMsg)
	require.True(t, ok)
	require.NoError(t, msg.err)
	assert.Equal(t, "ok", msg.content)

	lines := strings.Join(msg.eventLines, "\n")
	assert.Contains(t, lines, "event:tool_execute")
	assert.Contains(t, lines, "provider=activity")
	assert.Contains(t, lines, "tool=llm.complete")
	assert.Contains(t, lines, "event:command_execute")
	assert.Contains(t, lines, "command=fake-provider-command")
	assert.Contains(t, lines, "session=session-1")
}

func TestOpenModelPickerFetchesProviderModelsInBackground(t *testing.T) {
	t.Parallel()

	originalLookPath := execLookPath
	execLookPath = func(string) (string, error) {
		return "", os.ErrNotExist
	}

	t.Cleanup(func() {
		execLookPath = originalLookPath
	})

	registry := llm.NewRegistry()
	registry.Register(modelPickerProvider{
		name:          "beta",
		models:        []string{"beta-static"},
		fetchedModels: []string{"beta-live"},
	})
	registry.Register(modelPickerProvider{
		name:          "alpha",
		models:        []string{"alpha-static"},
		fetchedModels: []string{"alpha-live"},
	})

	next, cmd, handled := (model{ctx: context.Background(), registry: registry}).openModelPicker()
	require.True(t, handled)

	picker, ok := next.(model)
	require.True(t, ok)
	require.True(t, picker.pickerOpen)
	require.True(t, picker.pickerLoading)
	assert.Equal(t, 2, picker.modelFetchesPending)
	assert.Equal(t,
		expandReasoningItems([]pickerItem{
			{provider: "alpha", model: "alpha-static"},
			{provider: "beta", model: "beta-static"},
		}),
		picker.pickerItems,
	)

	raw := cmd()
	batch, ok := raw.(tea.BatchMsg)
	require.True(t, ok)
	require.Len(t, batch, 2)

	alphaMsg, ok := batch[0]().(modelsLoadedMsg)
	require.True(t, ok)
	require.Equal(t, "alpha", alphaMsg.provider)
	next, _ = picker.updateModelsLoaded(alphaMsg)
	picker, ok = next.(model)
	require.True(t, ok)
	require.True(t, picker.pickerLoading)
	assert.Equal(t, 1, picker.modelFetchesPending)
	assert.Equal(t,
		expandReasoningItems([]pickerItem{
			{provider: "alpha", model: "alpha-live"},
			{provider: "beta", model: "beta-static"},
		}),
		picker.pickerItems,
	)

	betaMsg, ok := batch[1]().(modelsLoadedMsg)
	require.True(t, ok)
	require.Equal(t, "beta", betaMsg.provider)
	next, _ = picker.updateModelsLoaded(betaMsg)
	picker, ok = next.(model)
	require.True(t, ok)
	require.False(t, picker.pickerLoading)
	assert.Equal(t, 0, picker.modelFetchesPending)
	assert.Equal(t,
		expandReasoningItems([]pickerItem{
			{provider: "alpha", model: "alpha-live"},
			{provider: "beta", model: "beta-live"},
		}),
		picker.pickerItems,
	)
}

// expandReasoningItems expands each base picker item into one entry per picker
// reasoning level (default + each canonical level), matching the shape
// produced by pickerItemsForProvider.
func expandReasoningItems(bases []pickerItem) []pickerItem {
	levels := llm.ReasoningPickerLevels()

	out := make([]pickerItem, 0, len(bases)*len(levels))
	for _, base := range bases {
		for _, mode := range llm.ModelModePickerModes(base.provider, base.model) {
			for _, level := range levels {
				out = append(out, pickerItem{provider: base.provider, model: base.model, modelMode: mode, reasoning: level})
			}
		}
	}

	return out
}

func TestCompletionCandidates_AgentAndPath(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "README.md"), []byte("# demo\n"), 0o600); err != nil {
		require.NoError(t, err)
	}

	if err := os.Mkdir(filepath.Join(dir, "pkg"), 0o750); err != nil {
		require.NoError(t, err)
	}

	registry := agent.NewRegistry(map[string]config.AgentConfig{
		"reviewer": {Description: "reviews code"},
	})

	items, ok := completionCandidates("Ask @rev", registry, dir, 8)
	if !ok {
		require.FailNow(t, "expected active completion token")
	}

	if len(items) == 0 || items[0].value != "@reviewer " {
		require.Failf(t, "unexpected candidates", "items = %+v", items)
	}

	if got := applyCompletionCandidate("Ask @rev", items[0].value); got != "Ask @reviewer " {
		require.Failf(t, "unexpected completion", "got %q", got)
	}

	items, ok = completionCandidates("Read @REA", registry, dir, 8)
	if !ok {
		require.FailNow(t, "expected path completion token")
	}

	found := false

	for _, item := range items {
		if item.value == "@README.md" {
			found = true
		}
	}

	if !found {
		require.Failf(t, "README completion missing", "items = %+v", items)
	}
}

func TestPromptHistoryFromStore_LoadsNewestUserPrompts(t *testing.T) {
	t.Parallel()

	store := session.NewStore(t.TempDir())
	older := session.New("gpt-test", []llm.Message{
		{Role: llm.RoleUser, Content: "older prompt"},
		{Role: llm.RoleAssistant, Content: "answer"},
		{Role: llm.RoleUser, Content: "duplicate prompt"},
	})
	require.NoError(t, store.Save(older))

	current := session.New("gpt-test", []llm.Message{
		{Role: llm.RoleUser, Content: "duplicate prompt"},
		{Role: llm.RoleAssistant, Content: "answer"},
		{Role: llm.RoleUser, Content: "current prompt"},
	})

	got := promptHistoryFromStore(store, current, 4)

	assert.Equal(t, []string{"current prompt", "duplicate prompt", "older prompt"}, got)
}

func TestNavigatePromptHistory_CyclesAndRestoresDraft(t *testing.T) {
	t.Parallel()

	m := model{
		textarea:            textarea.New(),
		promptHistory:       []string{"latest prompt", "older prompt"},
		promptHistoryCursor: -1,
	}
	m.textarea.SetValue("draft")

	next, ok := m.navigatePromptHistory(1)
	require.True(t, ok)
	assert.Equal(t, "latest prompt", next.textarea.Value())

	next, ok = next.navigatePromptHistory(1)
	require.True(t, ok)
	assert.Equal(t, "older prompt", next.textarea.Value())

	next, ok = next.navigatePromptHistory(-1)
	require.True(t, ok)
	assert.Equal(t, "latest prompt", next.textarea.Value())

	next, ok = next.navigatePromptHistory(-1)
	require.True(t, ok)
	assert.Equal(t, "draft", next.textarea.Value())
}

func TestHistoryArrowMovesCursorWhenInputIsMidline(t *testing.T) {
	t.Parallel()

	m := model{
		textarea:            textarea.New(),
		promptHistory:       []string{"latest prompt"},
		promptHistoryCursor: -1,
	}
	m.textarea.SetValue("draft prompt")
	m.textarea.SetCursor(len("draft"))

	nextModel, cmd, handled := m.handleChatCommand("up")
	next, ok := nextModel.(model)
	require.True(t, ok)
	require.True(t, handled)
	require.Nil(t, cmd)
	assert.Equal(t, "draft prompt", next.textarea.Value())
	assert.Equal(t, 0, textareaCursorOffset(next.textarea))
	assert.Equal(t, -1, next.promptHistoryCursor)

	next.textarea.SetCursor(len("draft"))
	nextModel, _, handled = next.handleChatCommand("down")
	next, ok = nextModel.(model)
	require.True(t, ok)
	require.True(t, handled)
	assert.Equal(t, len("draft prompt"), textareaCursorOffset(next.textarea))
	assert.Equal(t, -1, next.promptHistoryCursor)
}

func TestPromptSuggestionAndApply(t *testing.T) {
	t.Parallel()

	m := model{textarea: textarea.New()}
	m.textarea.SetValue("summ")
	suggestion, ok := m.promptSuggestion()
	require.True(t, ok)

	assert.Equal(t, "summarize this session with changed files and verification evidence", applyPromptSuggestion(m.textarea.Value(), suggestion))

	m.textarea.SetValue("summ now")
	m.textarea.SetCursor(len("summ"))
	_, ok = m.promptSuggestion()
	assert.False(t, ok)
}

func TestPromptSuggestionUsesTaskIDsWithoutModel(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	store := session.NewStore(filepath.Join(dir, "sessions"))
	taskStore := tasklist.NewStore(taskListPath(store, ""))
	_, err := taskStore.Add(context.Background(), tasklist.AddRequest{
		ID:    "GH-27",
		Title: "Make prompt completion context-aware",
	})
	require.NoError(t, err)

	m := model{
		ctx:          context.Background(),
		sessionStore: store,
		cwd:          dir,
		textarea:     textarea.New(),
	}
	m.textarea.SetValue("task GH")
	m.textarea.CursorEnd()

	suggestion, ok := m.promptSuggestion()
	require.True(t, ok)
	assert.Equal(t, "GH-27", suggestion.Text)
	assert.Equal(t, "task GH-27", applyPromptSuggestion(m.textarea.Value(), suggestion))
}

func TestIdleSuggestionRequestUsesProviderAndAcceptsSuffix(t *testing.T) {
	t.Parallel()

	registry := llm.NewRegistry()
	registry.Register(idleSuggestionProvider{model: "model", response: "draft prompt with tests"})

	m := model{
		ctx:           context.Background(),
		registry:      registry,
		selectedModel: "suggest/model",
		textarea:      textarea.New(),
	}
	m.textarea.SetValue("draft prompt")
	m.textarea.CursorEnd()
	cmd := m.scheduleIdleSuggestion()
	require.NotNil(t, cmd)

	nextModel, requestCmd := m.updateIdleSuggestionRequest(idleSuggestionRequestMsg{
		id:    m.idleSuggestionID,
		input: "draft prompt",
	})
	next, ok := nextModel.(model)
	require.True(t, ok)
	require.NotNil(t, requestCmd)

	msg, ok := requestCmd().(idleSuggestionMsg)
	require.True(t, ok)
	require.NoError(t, msg.err)

	nextModel, _ = next.updateIdleSuggestion(msg)
	next, ok = nextModel.(model)
	require.True(t, ok)

	suffix, ok := next.visibleIdleSuggestion()
	require.True(t, ok)
	assert.Equal(t, " with tests", suffix)

	nextModel, _, handled := next.acceptCompletion()
	next, ok = nextModel.(model)
	require.True(t, ok)
	require.True(t, handled)
	assert.Equal(t, "draft prompt with tests", next.textarea.Value())
}

func TestIdleSuggestionRequestIncludesLocalContext(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	store := session.NewStore(filepath.Join(dir, "sessions"))
	taskStore := tasklist.NewStore(taskListPath(store, ""))
	_, err := taskStore.Add(context.Background(), tasklist.AddRequest{
		ID:    "GH-27",
		Title: "Make prompt completion context-aware",
	})
	require.NoError(t, err)

	provider := &capturingIdleSuggestionProvider{model: "model", response: "draft prompt with GH-27"}
	registry := llm.NewRegistry()
	registry.Register(provider)

	m := model{
		ctx:      context.Background(),
		registry: registry,
		agentRegistry: agent.NewRegistry(map[string]config.AgentConfig{
			"planner": {
				Description:     "plans implementation work",
				ToolPermissions: map[string]bool{"bash": true},
			},
		}),
		sessionStore:  store,
		sessionState:  session.Session{Title: "Follow up on #15", Artifacts: []session.Artifact{{Path: "docs/notes.md", Kind: "notes"}}},
		selectedAgent: "planner",
		selectedModel: "capture/model",
		cwd:           dir,
		textarea:      textarea.New(),
	}
	m.textarea.SetValue("draft prompt")
	m.textarea.CursorEnd()
	cmd := m.scheduleIdleSuggestion()
	require.NotNil(t, cmd)

	_, requestCmd := m.updateIdleSuggestionRequest(idleSuggestionRequestMsg{
		id:    m.idleSuggestionID,
		input: "draft prompt",
	})
	require.NotNil(t, requestCmd)

	msg, ok := requestCmd().(idleSuggestionMsg)
	require.True(t, ok)
	require.NoError(t, msg.err)
	require.NotNil(t, provider.params)
	require.Len(t, provider.params.Messages, 2)

	localContext := provider.params.Messages[1].Content
	for _, want := range []string{
		"Local context:",
		"agent: planner",
		"slash: /help",
		"file: docs/notes.md",
		"task: GH-27",
		"issue: #15",
		"issue: GH-27",
		"permission: bash",
	} {
		assert.Contains(t, localContext, want)
	}
}

func TestPromptLocalOnlySkipsIdleModelSuggestion(t *testing.T) {
	t.Parallel()

	registry := llm.NewRegistry()
	registry.Register(idleSuggestionProvider{model: "model", response: "draft prompt with tests"})

	m := model{
		ctx:             context.Background(),
		registry:        registry,
		selectedModel:   "suggest/model",
		promptLocalOnly: true,
		textarea:        textarea.New(),
	}
	m.textarea.SetValue("draft prompt")
	m.textarea.CursorEnd()

	assert.Nil(t, m.scheduleIdleSuggestion())
	assert.Empty(t, m.idleSuggestionInput)
	assert.Empty(t, m.idleSuggestionStatus)
}

func TestIdleSuggestionRequestRejectsCursorMoved(t *testing.T) {
	t.Parallel()

	registry := llm.NewRegistry()
	registry.Register(idleSuggestionProvider{model: "model", response: "draft prompt with tests"})

	m := model{
		ctx:           context.Background(),
		registry:      registry,
		selectedModel: "suggest/model",
		textarea:      textarea.New(),
	}
	m.textarea.SetValue("draft prompt")
	m.textarea.CursorEnd()
	cmd := m.scheduleIdleSuggestion()
	require.NotNil(t, cmd)

	m.textarea.SetCursor(len("draft"))
	require.NotEqual(t, len(m.textarea.Value()), textareaCursorOffset(m.textarea))

	nextModel, requestCmd := m.updateIdleSuggestionRequest(idleSuggestionRequestMsg{
		id:    m.idleSuggestionID,
		input: "draft prompt",
	})
	next, ok := nextModel.(model)
	require.True(t, ok)
	assert.Nil(t, requestCmd)
	assert.Equal(t, "rejected:stale", next.idleSuggestionStatus)
}

func TestClearIdleSuggestionCancelsInFlightRequest(t *testing.T) {
	t.Parallel()

	provider := &cancelAwareIdleSuggestionProvider{model: "model"}
	registry := llm.NewRegistry()
	registry.Register(provider)

	m := model{
		ctx:           context.Background(),
		registry:      registry,
		selectedModel: "cancel/model",
		textarea:      textarea.New(),
	}
	m.textarea.SetValue("draft prompt")
	m.textarea.CursorEnd()
	cmd := m.scheduleIdleSuggestion()
	require.NotNil(t, cmd)

	nextModel, requestCmd := m.updateIdleSuggestionRequest(idleSuggestionRequestMsg{
		id:    m.idleSuggestionID,
		input: "draft prompt",
	})
	next, ok := nextModel.(model)
	require.True(t, ok)
	require.NotNil(t, requestCmd)
	require.NotNil(t, next.idleSuggestionCancel)

	next.clearIdleSuggestion()

	msg, ok := requestCmd().(idleSuggestionMsg)
	require.True(t, ok)
	require.Error(t, msg.err)
	assert.Contains(t, msg.err.Error(), "canceled")
	assert.False(t, provider.called)
	assert.Nil(t, next.idleSuggestionCancel)
}

func TestIdleSuggestionIgnoresStaleResponses(t *testing.T) {
	t.Parallel()

	m := model{textarea: textarea.New()}
	m.textarea.SetValue("new input")
	m.idleSuggestionID = 2
	m.idleSuggestionInput = "new input"

	nextModel, _ := m.updateIdleSuggestion(idleSuggestionMsg{
		id:         1,
		input:      "old input",
		suggestion: " old suffix",
	})
	next, ok := nextModel.(model)
	require.True(t, ok)
	assert.Empty(t, next.idleSuggestionText)
	assert.Equal(t, "rejected:stale", next.idleSuggestionStatus)
}

func TestIdleSuggestionRejectsUnsafeResponses(t *testing.T) {
	t.Parallel()

	m := model{textarea: textarea.New()}
	m.textarea.SetValue("draft prompt")
	m.textarea.CursorEnd()
	m.idleSuggestionID = 1
	m.idleSuggestionInput = "draft prompt"

	nextModel, _ := m.updateIdleSuggestion(idleSuggestionMsg{
		id:         1,
		input:      "draft prompt",
		suggestion: "with tests\nrm -rf /",
	})
	next, ok := nextModel.(model)
	require.True(t, ok)
	assert.Empty(t, next.idleSuggestionText)
	assert.Equal(t, "rejected:unsafe-multiline", next.idleSuggestionStatus)
}

func TestFormatTaskDuration(t *testing.T) {
	t.Parallel()

	assert.Equal(t, "250ms", formatTaskDuration(250*time.Millisecond))
	assert.Equal(t, "1.2s", formatTaskDuration(1200*time.Millisecond))
	assert.Equal(t, "2s", formatTaskDuration(2*time.Second))
	assert.Equal(t, "1m30s", formatTaskDuration(90*time.Second))
}

func TestWaitingStatus_RendersRunningDurationAndQueue(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 5, 5, 12, 0, 0, 0, time.UTC)
	m := model{
		runningTaskLabel:   "LLM",
		runningTaskStarted: now.Add(-90 * time.Second),
		queuedPrompts:      []string{"next"},
	}

	got := m.waitingStatusAt(now)
	assert.Contains(t, got, "LLM running for 1m30s")
	assert.Contains(t, got, "1 queued")
	assert.Contains(t, got, "Ctrl+C to cancel")
}

func TestSubmitPrompt_StartsLLMTaskTimer(t *testing.T) {
	t.Parallel()

	m := model{
		ctx:            context.Background(),
		textarea:       textarea.New(),
		registry:       llm.NewRegistry(),
		agentRegistry:  agent.NewRegistry(nil),
		sessionState:   session.New("gpt-test", nil),
		contextOptions: contextref.Options{Root: t.TempDir()},
	}

	nextModel, cmd := m.submitPrompt("hello")
	next, ok := nextModel.(model)
	require.True(t, ok)
	require.NotNil(t, cmd)
	assert.True(t, next.waiting)
	assert.Equal(t, "LLM", next.runningTaskLabel)
	assert.False(t, next.runningTaskStarted.IsZero())
	assert.Equal(t, 1, next.runningTaskID)
}

func TestRunShellCommand_StartsTaskTimer(t *testing.T) {
	t.Parallel()

	nextModel, cmd := (model{ctx: context.Background()}).runShellCommand("echo hi")
	next, ok := nextModel.(model)
	require.True(t, ok)
	require.NotNil(t, cmd)
	assert.True(t, next.waiting)
	assert.Equal(t, "command", next.runningTaskLabel)
	assert.False(t, next.runningTaskStarted.IsZero())
	assert.Equal(t, 1, next.runningTaskID)
}

func TestRunningTaskUpdatesTerminalTitle(t *testing.T) {
	t.Parallel()

	m := model{}
	cmd := m.startRunningTask("LLM")
	require.NotNil(t, cmd)

	raw := cmd()
	batch, ok := raw.(tea.BatchMsg)
	require.True(t, ok)
	require.Len(t, batch, 2)
	assert.Contains(t, stripANSI(toStringMsg(batch[0]())), "atteler")
	assert.Contains(t, stripANSI(toStringMsg(batch[0]())), "LLM")

	nextModel, tickCmd := m.updateTaskTick(taskTickMsg{id: m.runningTaskID})
	next, ok := nextModel.(model)
	require.True(t, ok)
	require.NotNil(t, tickCmd)
	assert.Equal(t, 1, next.terminalTitleFrame)
}

func TestView_RendersInlinePromptSuggestion(t *testing.T) {
	t.Parallel()

	m := model{textarea: textarea.New()}
	m.textarea.SetHeight(3)
	m.textarea.ShowLineNumbers = false
	m.textarea.SetValue("summ")

	plain := stripANSI(m.View())
	lines := strings.Split(plain, "\n")
	require.NotEmpty(t, lines)
	assert.Contains(t, lines[1], "summarize this session with changed files and verification evidence")
}

func TestView_SuppressesInlineSuggestionWhenCompletionMenuOpen(t *testing.T) {
	t.Parallel()

	m := model{
		textarea:        textarea.New(),
		completionOpen:  true,
		completionItems: []completionCandidate{{kind: "agent", label: "@reviewer", value: "@reviewer "}},
	}
	m.textarea.SetValue("summ")

	plain := stripANSI(m.View())
	assert.Contains(t, plain, "completions:")
	assert.NotContains(t, plain, "summarize this session")
}

func TestStatusLineShowsReasoningEffortOverride(t *testing.T) {
	t.Parallel()

	m := model{
		selectedModel:       testCodexModel,
		executionMode:       "execute",
		generationOverrides: generationSettings{ReasoningLevel: testReasoningXHigh, ModelMode: llm.ModelModeFast},
	}

	plain := stripANSI(m.statusLine())
	assert.Contains(t, plain, "model:"+testCodexModel)
	assert.Contains(t, plain, "effort:"+testReasoningXHigh)
	assert.Contains(t, plain, "model_mode:"+llm.ModelModeFast)
}

func TestStatusLineShowsAgentReasoningEffort(t *testing.T) {
	t.Parallel()

	agents := agent.NewRegistry(map[string]config.AgentConfig{
		testReviewerName: {ReasoningLevel: "high"},
	})
	m := model{
		agentRegistry:      agents,
		selectedAgent:      testReviewerName,
		selectedModel:      "gpt-test",
		generationDefaults: generationSettings{ReasoningLevel: "medium"},
	}

	plain := stripANSI(m.statusLine())
	assert.Contains(t, plain, "agent:"+testReviewerName)
	assert.Contains(t, plain, "effort:high")
}

func TestViewShowsReasoningEffortWhileWaiting(t *testing.T) {
	t.Parallel()

	now := time.Now()
	m := model{
		textarea:           textarea.New(),
		selectedModel:      testCodexModel,
		generationDefaults: generationSettings{ReasoningLevel: "medium"},
		runningTaskLabel:   "LLM",
		runningTaskStarted: now.Add(-time.Second),
		waiting:            true,
	}

	plain := stripANSI(m.View())
	assert.Contains(t, plain, "effort:medium")
	assert.Contains(t, plain, "Thinking")
}

func TestSubmitInput_QueuesFollowUpWhileWaiting(t *testing.T) {
	t.Parallel()

	m := model{
		textarea:            textarea.New(),
		waiting:             true,
		promptHistoryCursor: -1,
	}
	m.textarea.SetValue("follow up")

	nextModel, cmd := m.submitInput()
	next, ok := nextModel.(model)
	require.True(t, ok)
	require.NotNil(t, cmd)
	assert.True(t, next.waiting)
	assert.Empty(t, next.textarea.Value())
	assert.Equal(t, []string{"follow up"}, next.queuedPrompts)
	assert.Equal(t, []string{"follow up"}, next.promptHistory)
}

func TestUpdateLLMResponse_DrainsQueuedPrompt(t *testing.T) {
	t.Parallel()

	initialHistory := []llm.Message{{Role: llm.RoleUser, Content: "first"}}
	sessionState := session.New("gpt-test", initialHistory)
	m := model{
		ctx:            context.Background(),
		textarea:       textarea.New(),
		registry:       llm.NewRegistry(),
		agentRegistry:  agent.NewRegistry(nil),
		sessionStore:   session.NewStore(t.TempDir()),
		sessionState:   sessionState,
		sessionPath:    "/tmp/session.json",
		selectedModel:  "gpt-test",
		history:        append([]llm.Message(nil), initialHistory...),
		queuedPrompts:  []string{"follow up", "third"},
		contextOptions: contextref.Options{Root: t.TempDir()},
	}

	nextModel, cmd := m.updateLLMResponse(llmResponseMsg{content: "answer", model: "gpt-test"})
	next, ok := nextModel.(model)
	require.True(t, ok)
	require.NotNil(t, cmd)
	assert.True(t, next.waiting)
	assert.Len(t, next.queuedPrompts, 1)
	assert.Equal(t, "third", next.queuedPrompts[0])
	assert.Equal(t, []llm.Message{
		{Role: llm.RoleUser, Content: "first"},
		{Role: llm.RoleAssistant, Content: "answer"},
		{Role: llm.RoleUser, Content: "follow up"},
	}, next.history)
	assert.Equal(t, next.history, next.sessionState.Messages)
}

func TestUpdateLLMResponse_ClearsCompletedTaskTimer(t *testing.T) {
	t.Parallel()

	startedAt := time.Date(2026, 5, 5, 12, 0, 0, 0, time.UTC)
	m := model{
		textarea:           textarea.New(),
		runningTaskLabel:   "LLM",
		runningTaskStarted: startedAt,
		runningTaskID:      7,
		sessionState:       session.New("gpt-test", []llm.Message{{Role: llm.RoleUser, Content: "hello"}}),
		history:            []llm.Message{{Role: llm.RoleUser, Content: "hello"}},
	}

	nextModel, cmd := m.updateLLMResponse(llmResponseMsg{
		completedAt: startedAt.Add(1500 * time.Millisecond),
		content:     "answer",
		model:       "gpt-test",
	})
	next, ok := nextModel.(model)
	require.True(t, ok)
	require.NotNil(t, cmd)
	assert.False(t, next.waiting)
	assert.Empty(t, next.runningTaskLabel)
	assert.True(t, next.runningTaskStarted.IsZero())
}

func stripANSI(value string) string {
	return ansi.Strip(value)
}

func toStringMsg(msg tea.Msg) string {
	if msg == nil {
		return ""
	}

	return fmt.Sprint(msg)
}

func TestRevampPromptAndUndo(t *testing.T) {
	t.Parallel()

	m := model{textarea: textarea.New()}
	m.textarea.SetValue("write release notes")

	nextModel, _ := m.revampPrompt()
	next, ok := nextModel.(model)
	require.True(t, ok)
	assert.Contains(t, next.textarea.Value(), "Goal:")
	assert.True(t, next.revampUndoActive)

	undoneModel, _ := next.undoPromptRevamp()
	undone, ok := undoneModel.(model)
	require.True(t, ok)
	assert.Equal(t, "write release notes", undone.textarea.Value())
	assert.False(t, undone.revampUndoActive)
}

func TestHandleCtrlC_ClearsDraftBeforeQuit(t *testing.T) {
	t.Parallel()

	m := model{
		textarea:       textarea.New(),
		completionOpen: true,
		completionItems: []completionCandidate{{
			kind:  "agent",
			label: "@reviewer",
			value: "@reviewer ",
		}},
		revampUndoActive: true,
		revampUndo:       "original",
	}
	m.textarea.SetValue("draft prompt")

	nextModel, cmd, handled := m.handleCtrlC()
	next, ok := nextModel.(model)
	require.True(t, ok)
	require.True(t, handled)
	assert.Nil(t, cmd)
	assert.False(t, next.quitting)
	assert.Empty(t, next.textarea.Value())
	assert.False(t, next.completionOpen)
	assert.Empty(t, next.completionItems)
	assert.False(t, next.revampUndoActive)
	assert.Empty(t, next.revampUndo)

	nextModel, cmd, handled = next.handleCtrlC()
	next, ok = nextModel.(model)
	require.True(t, ok)
	require.True(t, handled)
	assert.True(t, next.quitting)
	require.NotNil(t, cmd)
}

func TestHandleCtrlC_CancelsWaitingWhenDraftEmpty(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	m := model{
		textarea:           textarea.New(),
		waiting:            true,
		cancel:             cancel,
		runningTaskLabel:   "LLM",
		runningTaskStarted: time.Now(),
	}

	nextModel, cmd, handled := m.handleCtrlC()
	next, ok := nextModel.(model)
	require.True(t, ok)
	require.True(t, handled)
	assert.False(t, next.waiting)
	assert.Nil(t, next.cancel)
	assert.False(t, next.quitting)
	require.NotNil(t, cmd)

	select {
	case <-ctx.Done():
	case <-time.After(time.Second):
		require.Fail(t, "cancel function was not called")
	}
}

//nolint:paralleltest // Mutates the package-level runInteractiveProgram seam.
func TestRunInteractive_ReplacesHookLoggerBeforeSessionStart(t *testing.T) {
	originalRunInteractiveProgram := runInteractiveProgram
	runInteractiveProgram = func(m model) (tea.Model, error) {
		return m, nil
	}

	t.Cleanup(func() {
		runInteractiveProgram = originalRunInteractiveProgram
	})

	store := session.NewStore(t.TempDir())
	state := appState{
		registry:       llm.NewRegistry(),
		agentRegistry:  agent.NewRegistry(nil),
		hookRunner:     events.NewRunnerWithLogger(nil, panicWriter{}),
		sessionStore:   store,
		sessionState:   session.New("gpt-test", nil),
		contextOptions: contextref.Options{Root: t.TempDir()},
		selectedModel:  "gpt-test",
		cwd:            t.TempDir(),
	}

	require.NoError(t, runInteractive(context.Background(), state))
}

type panicWriter struct{}

func (panicWriter) Write([]byte) (int, error) {
	panic("hook logger should not be used before the TUI program starts")
}
