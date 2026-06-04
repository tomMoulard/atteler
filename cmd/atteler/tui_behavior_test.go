package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/charmbracelet/bubbles/textarea"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/x/ansi"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/tommoulard/atteler/pkg/agent"
	"github.com/tommoulard/atteler/pkg/autonomy"
	"github.com/tommoulard/atteler/pkg/codeintel"
	"github.com/tommoulard/atteler/pkg/config"
	"github.com/tommoulard/atteler/pkg/contextref"
	"github.com/tommoulard/atteler/pkg/events"
	"github.com/tommoulard/atteler/pkg/llm"
	"github.com/tommoulard/atteler/pkg/promptcomplete"
	"github.com/tommoulard/atteler/pkg/session"
	attshell "github.com/tommoulard/atteler/pkg/shell"
	"github.com/tommoulard/atteler/pkg/tasklist"
)

const liveLLMResponseTimeout = 3 * time.Second

func TestFZFInputAndSelection(t *testing.T) {
	t.Parallel()

	items := []pickerItem{
		{provider: "claude-code", model: "claude-opus-4-6"},
		{provider: "codex", model: "gpt-5.5"},
		{provider: "codex", model: "gpt-5.5", reasoning: testReasoningXHigh},
		{provider: "codex", model: "gpt-5.5", modelMode: llm.ModelModeFast, reasoning: llm.ReasoningLevelDefault},
	}

	input := fzfInput(items)
	for _, want := range []string{
		"claude-code/claude-opus-4-6\tclaude-code\tclaude-opus-4-6\t\t\n",
		"codex/gpt-5.5\tcodex\tgpt-5.5\t\t\n",
		"codex/gpt-5.5:xhigh\tcodex\tgpt-5.5\txhigh\t\n",
		"codex/gpt-5.5:mode=fast:effort=default\tcodex\tgpt-5.5\tdefault\tfast\n",
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

	item, ok = parseFZFSelection("codex/gpt-5.5:mode=fast:effort=default\tcodex\tgpt-5.5\tdefault\tfast\n", items)
	require.True(t, ok)
	assert.Equal(t, llm.ModelModeFast, item.modelMode)

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

type toolCallingProvider struct {
	command string
	calls   int
}

func (p *toolCallingProvider) Name() string { return "tool" }

func (p *toolCallingProvider) Models() []string { return []string{"tool-model"} }

func (p *toolCallingProvider) FetchModels(context.Context) ([]string, error) {
	return p.Models(), nil
}

func (p *toolCallingProvider) HealthCheck(context.Context) error { return nil }

func (p *toolCallingProvider) Complete(_ context.Context, params llm.CompleteParams) (*llm.Response, error) {
	p.calls++
	if p.calls == 1 {
		command := p.command
		if command == "" {
			command = `printf 'live\n'; sleep 0.4; printf 'done\n'`
		}

		return &llm.Response{
			Model:      params.Model,
			StopReason: llm.StopToolUse,
			ToolCalls: []llm.ToolCall{{
				ID:    "call-1",
				Name:  "bash",
				Input: map[string]any{"command": command},
			}},
		}, nil
	}

	return &llm.Response{Content: "finished", Model: params.Model, StopReason: llm.StopEndTurn}, nil
}

func (p *toolCallingProvider) ModelContextWindow(string) int { return 0 }

type idleSuggestionProvider struct {
	response     string
	model        string
	inputTokens  int
	outputTokens int
}

func (p idleSuggestionProvider) Name() string { return "suggest" }

func (p idleSuggestionProvider) Models() []string { return []string{p.model} }

func (p idleSuggestionProvider) FetchModels(context.Context) ([]string, error) {
	return p.Models(), nil
}

func (p idleSuggestionProvider) HealthCheck(context.Context) error { return nil }

func (p idleSuggestionProvider) Complete(context.Context, llm.CompleteParams) (*llm.Response, error) {
	return &llm.Response{
		Content:      p.response,
		Model:        p.model,
		InputTokens:  p.inputTokens,
		OutputTokens: p.outputTokens,
	}, nil
}

func (p idleSuggestionProvider) ModelContextWindow(string) int { return 0 }

func newIdleSuggestionTestModel(response string) model {
	registry := llm.NewRegistry()
	registry.Register(idleSuggestionProvider{model: "model", response: response})

	return model{
		ctx:                     context.Background(),
		registry:                registry,
		selectedModel:           "suggest/model",
		promptSuggestionConsent: promptSuggestionConsentSession,
		idleSuggestionBudget:    defaultIdleSuggestionBudget(),
		textarea:                textarea.New(),
	}
}

type failingIdleSuggestionProvider struct {
	model string
}

func (p failingIdleSuggestionProvider) Name() string { return "suggest" }

func (p failingIdleSuggestionProvider) Models() []string { return []string{p.model} }

func (p failingIdleSuggestionProvider) FetchModels(context.Context) ([]string, error) {
	return p.Models(), nil
}

func (p failingIdleSuggestionProvider) HealthCheck(context.Context) error { return nil }

func (p failingIdleSuggestionProvider) Complete(context.Context, llm.CompleteParams) (*llm.Response, error) {
	return nil, errors.New("provider unavailable")
}

func (p failingIdleSuggestionProvider) ModelContextWindow(string) int { return 0 }

type capturingIdleSuggestionProvider struct {
	params       *llm.CompleteParams
	response     string
	model        string
	providerName string
}

func (p *capturingIdleSuggestionProvider) Name() string {
	if p.providerName != "" {
		return p.providerName
	}

	return "capture"
}

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
	assert.NotContains(t, lines, "command=fake-provider-command")
	assert.Contains(t, lines, "redacted=true")
	assert.Contains(t, lines, "session=session-1")
}

func TestCallLLMWithToolsStreamsCommandOutputBeforeCompletion(t *testing.T) {
	t.Parallel()

	const liveToolCompletionTimeout = 3 * time.Second

	registry := llm.NewRegistry()
	registry.Register(&toolCallingProvider{})

	liveCh := make(chan tea.Msg, 16)
	done := make(chan tea.Msg, 1)

	go func() {
		done <- callLLM(context.Background(), registry, llmRequest{
			eventBase: events.Event{
				SessionID: "session-1",
				Model:     "tool-model",
			},
			hookRunner: events.NewRunner(nil),
			model:      "tool-model",
			messages: []llm.Message{{
				Role:    llm.RoleUser,
				Content: "run tool",
			}},
			useTools:   true,
			workingDir: t.TempDir(),
			liveCh:     liveCh,
		})()
	}()

	output := requireLiveToolOutputBefore(t, liveCh, 3*liveOutputTimeout)
	assert.Equal(t, "live\n", output.data)
	assert.Equal(t, string(attshell.OutputStreamStdout), output.stream)

	select {
	case msg := <-done:
		require.Failf(t, "LLM completed before delayed tool output", "msg=%+v", msg)
	default:
	}

	msg := requireLiveLLMResponseBefore(t, liveCh, liveLLMResponseTimeout)
	require.NoError(t, msg.err)
	assert.Equal(t, "finished", msg.content)
	assert.True(t, msg.liveEvents)

	select {
	case raw := <-done:
		assert.Nil(t, raw)
	case <-time.After(liveToolCompletionTimeout):
		require.FailNow(t, "timed out waiting for callLLM command return")
	}
}

func TestCallLLMWithToolsStreamsCommandStderrBeforeCompletion(t *testing.T) {
	t.Parallel()

	const liveToolCompletionTimeout = 3 * time.Second

	registry := llm.NewRegistry()
	registry.Register(&toolCallingProvider{command: `printf 'warn\n' >&2; sleep 0.4; printf 'done\n' >&2`})

	liveCh := make(chan tea.Msg, 16)
	done := make(chan tea.Msg, 1)

	go func() {
		done <- callLLM(context.Background(), registry, llmRequest{
			eventBase: events.Event{
				SessionID: "session-1",
				Model:     "tool-model",
			},
			hookRunner: events.NewRunner(nil),
			model:      "tool-model",
			messages: []llm.Message{{
				Role:    llm.RoleUser,
				Content: "run tool",
			}},
			useTools:   true,
			workingDir: t.TempDir(),
			liveCh:     liveCh,
		})()
	}()

	output := requireLiveToolOutputBefore(t, liveCh, 3*liveOutputTimeout)
	assert.Equal(t, "warn\n", output.data)
	assert.Equal(t, string(attshell.OutputStreamStderr), output.stream)

	select {
	case msg := <-done:
		require.Failf(t, "LLM completed before delayed tool stderr", "msg=%+v", msg)
	default:
	}

	msg := requireLiveLLMResponseBefore(t, liveCh, liveLLMResponseTimeout)
	require.NoError(t, msg.err)
	assert.Equal(t, "finished", msg.content)

	select {
	case raw := <-done:
		assert.Nil(t, raw)
	case <-time.After(liveToolCompletionTimeout):
		require.FailNow(t, "timed out waiting for callLLM command return")
	}
}

func TestFinishLLMResponseDeliversFinalMessage(t *testing.T) {
	t.Parallel()

	liveCh := make(chan tea.Msg, 1)
	raw := finishLLMResponse(liveCh, llmResponseMsg{err: context.Canceled})
	assert.Nil(t, raw)

	select {
	case raw := <-liveCh:
		msg, ok := raw.(llmResponseMsg)
		require.True(t, ok)
		require.ErrorIs(t, msg.err, context.Canceled)
		assert.True(t, msg.liveEvents)
	case <-time.After(time.Second):
		require.FailNow(t, "timed out waiting for final LLM response")
	}
}

func TestCallLLMWithToolsCostBudgetFailsClosedWithoutPricing(t *testing.T) {
	t.Parallel()

	provider := &runOnceCostProvider{
		name:   "ollama",
		models: []string{"llama3.2"},
		response: &llm.Response{
			Content:    "unpriced answer",
			Provider:   "ollama",
			Model:      "llama3.2",
			StopReason: llm.StopEndTurn,
		},
	}
	registry := llm.NewRegistry()
	registry.Register(provider)

	msg := callLLMWithTools(context.Background(), registry, llm.CompleteParams{
		Model: "ollama/llama3.2",
		Messages: []llm.Message{{
			Role:    llm.RoleUser,
			Content: "use tools",
		}},
	}, llmRequest{
		agentLoopBudget: llm.AgentLoopBudget{MaxCostMicros: 1},
		workingDir:      t.TempDir(),
	}, newEventLineBuffer())

	require.ErrorIs(t, msg.err, llm.ErrAgentLoopCostPricingUnavailable)
	assert.Contains(t, msg.err.Error(), "agent_loop.max_cost_micros")
	assert.Zero(t, provider.calls, "TUI cost budgets should fail before unpriced model usage")
}

func TestCallLLMWithToolsCostBudgetPassesWithCatalogPricing(t *testing.T) {
	t.Parallel()

	provider := &runOnceCostProvider{
		name:   "openai",
		models: []string{"gpt-4.1-mini"},
		response: &llm.Response{
			Content:      "priced answer",
			Provider:     "openai",
			Model:        "gpt-4.1-mini",
			StopReason:   llm.StopEndTurn,
			InputTokens:  1,
			OutputTokens: 1,
		},
	}
	registry := llm.NewRegistry()
	registry.Register(provider)

	msg := callLLMWithTools(context.Background(), registry, llm.CompleteParams{
		Model: "openai/gpt-4.1-mini",
		Messages: []llm.Message{{
			Role:    llm.RoleUser,
			Content: "use tools",
		}},
	}, llmRequest{
		agentLoopBudget: llm.AgentLoopBudget{MaxCostMicros: 10},
		workingDir:      t.TempDir(),
	}, newEventLineBuffer())

	require.NoError(t, msg.err)
	assert.Equal(t, "priced answer", msg.content)
	assert.Equal(t, 1, provider.calls)
	assert.Equal(t, 1, msg.tokenUsage.InputTokens)
	assert.Equal(t, 1, msg.tokenUsage.OutputTokens)
}

func requireLiveToolOutputBefore(t *testing.T, liveCh <-chan tea.Msg, timeout time.Duration) llmToolOutputMsg {
	t.Helper()

	deadline := time.After(timeout)

	for {
		select {
		case raw := <-liveCh:
			if output, ok := raw.(llmToolOutputMsg); ok {
				return output
			}
		case <-deadline:
			require.FailNow(t, "timed out waiting for live tool output")
		}
	}
}

func requireLiveLLMResponseBefore(t *testing.T, liveCh <-chan tea.Msg, timeout time.Duration) llmResponseMsg {
	t.Helper()

	deadline := time.After(timeout)

	for {
		select {
		case raw, ok := <-liveCh:
			require.True(t, ok, "live LLM channel closed before final response")

			if msg, ok := raw.(llmResponseMsg); ok {
				return msg
			}
		case <-deadline:
			require.FailNow(t, "timed out waiting for live LLM response")
		}
	}
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

func TestExternalModelPickerAllowedRespectsLowAutonomy(t *testing.T) {
	t.Parallel()

	assert.False(t, externalModelPickerAllowed(autonomy.Low))
	assert.True(t, externalModelPickerAllowed(autonomy.Medium))
	assert.True(t, externalModelPickerAllowed(autonomy.High))
	assert.True(t, externalModelPickerAllowed(autonomy.Full))
}

func TestPickerItemsForProviderCatalogUsesBareConfiguredAlias(t *testing.T) {
	t.Parallel()

	items := pickerItemsForProviderCatalog("openai", llm.ProviderModelCatalog{
		Models: []string{"gpt-5.4-nano", "gpt-5.5", "fast"},
		ModelProvenance: map[string]llm.ModelProvenance{
			"fast":         llm.ModelProvenanceConfiguredAlias,
			"gpt-5.4-nano": llm.ModelProvenanceStatic,
			"gpt-5.5":      llm.ModelProvenanceStatic,
		},
	})

	assert.Contains(t, items, pickerItem{provider: "", model: "fast", modelMode: llm.ModelModeDefault, reasoning: llm.ReasoningLevelDefault})
	assert.Contains(t, items, pickerItem{provider: "openai", model: "gpt-5.4-nano", modelMode: llm.ModelModeDefault, reasoning: llm.ReasoningLevelDefault})
	assert.Contains(t, items, pickerItem{provider: "openai", model: "gpt-5.5", modelMode: llm.ModelModeFast, reasoning: llm.ReasoningLevelDefault})
	assert.NotContains(t, items, pickerItem{provider: "openai", model: "fast", modelMode: llm.ModelModeDefault, reasoning: llm.ReasoningLevelDefault})
	assert.NotContains(t, items, pickerItem{provider: "", model: "fast", modelMode: llm.ModelModeFast, reasoning: llm.ReasoningLevelDefault})
	assert.NotContains(t, items, pickerItem{provider: "openai", model: "gpt-5.4-nano", modelMode: llm.ModelModeFast, reasoning: llm.ReasoningLevelDefault})
}

func TestPickerItemsForProviderCatalogWithRegistryUsesConfiguredAliasTargetModelModes(t *testing.T) {
	t.Parallel()

	registry := llm.NewRegistry()
	registry.Register(routeFakeProvider{name: "openai", models: []string{"gpt-5.4-nano", "gpt-5.5"}})
	require.NoError(t, registry.SetModelAlias("frontier", "openai", "gpt-5.5"))
	require.NoError(t, registry.SetModelAlias("nano", "openai", "gpt-5.4-nano"))

	catalog, err := registry.ProviderModelCatalog(context.Background(), "openai")
	require.NoError(t, err)

	items := pickerItemsForProviderCatalogWithRegistry(registry, "openai", catalog)

	assert.Contains(t, items, pickerItem{provider: "", model: "frontier", modelMode: llm.ModelModeDefault, reasoning: llm.ReasoningLevelDefault})
	assert.Contains(t, items, pickerItem{provider: "", model: "frontier", modelMode: llm.ModelModeFast, reasoning: llm.ReasoningLevelDefault})
	assert.Contains(t, items, pickerItem{provider: "", model: "nano", modelMode: llm.ModelModeDefault, reasoning: llm.ReasoningLevelDefault})
	assert.NotContains(t, items, pickerItem{provider: "", model: "nano", modelMode: llm.ModelModeFast, reasoning: llm.ReasoningLevelDefault})
	assert.NotContains(t, items, pickerItem{provider: "openai", model: "frontier", modelMode: llm.ModelModeFast, reasoning: llm.ReasoningLevelDefault})
}

func TestFallbackModelPickerItemsIncludesModelRoles(t *testing.T) {
	t.Parallel()

	registry := llm.NewRegistry()
	registry.Register(modelPickerProvider{name: "openai", models: []string{"gpt-4.1-mini"}})
	require.NoError(t, registry.SetModelRole("planner", llm.ModelRole{
		Preferred: "openai/gpt-4.1-mini",
	}))

	items := fallbackModelPickerItems(registry)

	assert.Contains(t, items, pickerItem{provider: "", model: "planner", modelMode: llm.ModelModeDefault, reasoning: llm.ReasoningLevelDefault})
	assert.Contains(t, items, pickerItem{provider: "openai", model: "gpt-4.1-mini", modelMode: llm.ModelModeDefault, reasoning: llm.ReasoningLevelDefault})
}

// expandReasoningItems expands each base picker item into one entry per picker
// reasoning level (default + each canonical level), matching the shape
// produced by pickerItemsForProvider.
func expandReasoningItems(bases []pickerItem) []pickerItem {
	levels := llm.ReasoningPickerLevels()

	out := make([]pickerItem, 0, len(bases)*len(levels))
	for _, base := range bases {
		for _, level := range levels {
			out = append(out, pickerItem{provider: base.provider, model: base.model, modelMode: llm.ModelModeDefault, reasoning: level})
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

	items, ok := completionCandidates(t.Context(), "Ask @rev", registry, dir)
	if !ok {
		require.FailNow(t, "expected active completion token")
	}

	if len(items) == 0 || items[0].value != "@reviewer " {
		require.Failf(t, "unexpected candidates", "items = %+v", items)
	}

	if got := applyCompletionCandidate("Ask @rev", items[0].value); got != "Ask @reviewer " {
		require.Failf(t, "unexpected completion", "got %q", got)
	}

	items, ok = completionCandidates(t.Context(), "Read @REA", registry, dir)
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

	got := promptHistoryFromStore(t.Context(), store, current, 4)

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

func TestPromptSuggestionUsesRepoBackedPromptCacheWithoutModel(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	store := session.NewStore(filepath.Join(dir, ".atteler", "sessions"))
	cache := newPromptContextCache()
	state := appState{
		cwd:                dir,
		sessionStore:       store,
		selectedAgent:      "planner",
		promptContextCache: cache,
	}
	key := promptContextCacheKeyForState(state)
	cache.store(promptRepoContextSnapshot{
		CapturedAt: time.Now(),
		Key:        key,
		ProjectSymbols: []promptcomplete.Candidate{{
			Text:        "RepoBackedSymbol",
			Kind:        "project-symbol",
			Description: "func in pkg/repo.go:12",
		}},
		Sources: []promptContextSourceStatus{
			promptContextSourceReport(promptContextSourceProjectSymbols, promptContextFreshnessFresh, "1 candidate"),
		},
	})

	m := model{
		ctx:                context.Background(),
		sessionStore:       store,
		cwd:                dir,
		selectedAgent:      "planner",
		promptContextCache: cache,
		textarea:           textarea.New(),
	}
	m.textarea.SetValue("call Repo")
	m.textarea.CursorEnd()

	suggestion, ok := m.promptSuggestion()
	require.True(t, ok)
	assert.Equal(t, "RepoBackedSymbol", suggestion.Text)
	assert.Equal(t, "call RepoBackedSymbol", applyPromptSuggestion(m.textarea.Value(), suggestion))
}

func TestPromptSuggestionUsesContextRootForPromptCache(t *testing.T) {
	t.Parallel()

	outer := t.TempDir()
	repoRoot := filepath.Join(outer, "worktree")
	cwd := filepath.Join(outer, "launcher")

	require.NoError(t, os.MkdirAll(repoRoot, 0o700))
	require.NoError(t, os.MkdirAll(cwd, 0o700))

	store := session.NewStore(filepath.Join(outer, ".atteler", "sessions"))
	cache := newPromptContextCache()
	state := appState{
		cwd:                cwd,
		contextOptions:     contextref.Options{Root: repoRoot},
		sessionStore:       store,
		selectedAgent:      "planner",
		promptContextCache: cache,
	}
	key := promptContextCacheKeyForState(state)
	cache.store(promptRepoContextSnapshot{
		CapturedAt: time.Now(),
		Key:        key,
		ProjectSymbols: []promptcomplete.Candidate{{
			Text:        "ContextRootSymbol",
			Kind:        "project-symbol",
			Description: "func in worktree/repo.go:12",
		}},
		Sources: []promptContextSourceStatus{
			promptContextSourceReport(promptContextSourceProjectSymbols, promptContextFreshnessFresh, "1 candidate"),
		},
	})

	m := model{
		ctx:                context.Background(),
		sessionStore:       store,
		cwd:                cwd,
		contextOptions:     contextref.Options{Root: repoRoot},
		selectedAgent:      "planner",
		promptContextCache: cache,
		textarea:           textarea.New(),
	}
	m.textarea.SetValue("call Context")
	m.textarea.CursorEnd()

	suggestion, ok := m.promptSuggestion()
	require.True(t, ok)
	assert.Equal(t, "ContextRootSymbol", suggestion.Text)
	assert.Equal(t, "call ContextRootSymbol", applyPromptSuggestion(m.textarea.Value(), suggestion))
}

//nolint:paralleltest // Uses process-wide prompt context source hooks.
func TestPromptSuggestionSharesDurablePromptCacheWithPromptComplete(t *testing.T) {
	dir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(dir, "shared.go"), []byte("package shared\n\nfunc SharedPromptSymbol() {}\n"), 0o600))

	indexCalls := 0
	restoreIndex := setPromptCodeIndexDirContextForTest(func(_ context.Context, root string) (codeintel.Index, error) {
		indexCalls++

		return codeintel.Index{Symbols: []codeintel.Symbol{{
			Name: "SharedPromptSymbol",
			Kind: "func",
			File: filepath.Join(root, "shared.go"),
			Line: 3,
		}}}, nil
	})
	t.Cleanup(restoreIndex)

	cachePath := filepath.Join(dir, ".atteler", promptContextCacheFileName)
	cliState := appState{
		cwd:                dir,
		selectedAgent:      "planner",
		promptContextCache: newPromptContextCache(cachePath),
	}
	cliResult := promptCompletionContextWithOptions(context.Background(), cliState, "Shared", true, defaultPromptCompletionContextOptions())
	require.Contains(t, candidateTexts(cliResult.Context.ProjectSymbols), "SharedPromptSymbol")
	require.FileExists(t, cachePath)
	require.Equal(t, 1, indexCalls)

	m := model{
		ctx:                context.Background(),
		cwd:                dir,
		selectedAgent:      "planner",
		promptContextCache: newPromptContextCache(cachePath),
		textarea:           textarea.New(),
	}
	m.textarea.SetValue("Shared")
	m.textarea.CursorEnd()

	suggestion, ok := m.promptSuggestion()
	require.True(t, ok)
	assert.Equal(t, "SharedPromptSymbol", suggestion.Text)
	assert.Equal(t, 1, indexCalls)
}

func TestIdleSuggestionContextUsesRepoBackedPromptCache(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	store := session.NewStore(filepath.Join(dir, ".atteler", "sessions"))
	cache := newPromptContextCache()
	state := appState{
		cwd:                dir,
		sessionStore:       store,
		selectedAgent:      "planner",
		promptContextCache: cache,
	}
	key := promptContextCacheKeyForState(state)
	cache.store(promptRepoContextSnapshot{
		CapturedAt: time.Now(),
		Key:        key,
		ProjectSymbols: []promptcomplete.Candidate{{
			Text:        "RepoBackedSymbol",
			Kind:        "project-symbol",
			Description: "func in pkg/repo.go:12",
		}},
		Sources: []promptContextSourceStatus{
			promptContextSourceReport(promptContextSourceProjectSymbols, promptContextFreshnessFresh, "1 candidate"),
		},
	})

	m := model{
		ctx:                context.Background(),
		sessionStore:       store,
		cwd:                dir,
		selectedAgent:      "planner",
		promptContextCache: cache,
		textarea:           textarea.New(),
	}
	m.textarea.SetValue("Repo")
	m.textarea.CursorEnd()

	summary, status := m.idleSuggestionContextForPrompt()

	assert.Contains(t, summary, "symbol: RepoBackedSymbol")
	assert.Contains(t, summary, "context project symbols: fresh")
	assert.Equal(t, "fresh", status)
}

//nolint:paralleltest // Uses process-wide prompt context source hooks.
func TestIdleSuggestionContextReusesPromptCacheWithoutReindex(t *testing.T) {
	dir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(dir, "repo.go"), []byte("package repo\n\nfunc RepoBackedSymbol() {}\n"), 0o600))

	indexCalls := 0
	indexed := make(chan struct{}, 1)
	restoreIndex := setPromptCodeIndexDirContextForTest(func(_ context.Context, root string) (codeintel.Index, error) {
		indexCalls++

		indexed <- struct{}{}

		return codeintel.Index{Symbols: []codeintel.Symbol{{
			Name: "RepoBackedSymbol",
			Kind: "func",
			File: filepath.Join(root, "repo.go"),
			Line: 3,
		}}}, nil
	})
	t.Cleanup(restoreIndex)

	cache := newPromptContextCache()
	m := model{
		ctx:                context.Background(),
		sessionStore:       session.NewStore(filepath.Join(dir, ".atteler", "sessions")),
		cwd:                dir,
		selectedAgent:      "planner",
		promptContextCache: cache,
		textarea:           textarea.New(),
	}
	m.textarea.SetValue("Repo")
	m.textarea.CursorEnd()

	firstSummary, firstStatus := m.idleSuggestionContextForPrompt()
	assert.NotContains(t, firstSummary, "symbol: RepoBackedSymbol")
	assert.Contains(t, firstStatus, "skipped")

	select {
	case <-indexed:
	case <-time.After(promptContextTestBackgroundBudget):
		require.Fail(t, "prompt context cache did not warm project symbols")
	}

	var secondSummary, secondStatus string

	require.Eventually(t, func() bool {
		secondSummary, secondStatus = m.idleSuggestionContextForPrompt()

		return strings.Contains(secondSummary, "symbol: RepoBackedSymbol")
	}, promptContextTestBackgroundBudget, 10*time.Millisecond)

	thirdSummary, thirdStatus := m.idleSuggestionContextForPrompt()

	assert.Contains(t, secondSummary, "symbol: RepoBackedSymbol")
	assert.Contains(t, secondSummary, "context project symbols: fresh")
	assert.NotContains(t, secondStatus, "project-symbols")
	assert.Contains(t, thirdSummary, "symbol: RepoBackedSymbol")
	assert.Contains(t, thirdSummary, "context project symbols: fresh")
	assert.NotContains(t, thirdStatus, "project-symbols")
	assert.Equal(t, 1, indexCalls)
}

//nolint:paralleltest // Uses process-wide prompt context source hooks.
func TestPromptSuggestionDeduplicatesRepoWarmupDuringRender(t *testing.T) {
	dir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(dir, "render.go"), []byte("package render\n\nfunc RenderSymbol() {}\n"), 0o600))

	var (
		indexCalls  atomic.Int32
		releaseOnce sync.Once
	)

	indexStarted := make(chan struct{}, 1)
	indexDone := make(chan struct{}, 1)
	releaseIndex := make(chan struct{})

	restoreIndex := setPromptCodeIndexDirContextForTest(func(context.Context, string) (codeintel.Index, error) {
		indexCalls.Add(1)

		defer func() {
			indexDone <- struct{}{}
		}()

		select {
		case indexStarted <- struct{}{}:
		default:
		}

		<-releaseIndex

		return codeintel.Index{}, nil
	})
	t.Cleanup(restoreIndex)
	t.Cleanup(func() {
		releaseOnce.Do(func() {
			close(releaseIndex)
		})
	})

	m := model{
		ctx:                context.Background(),
		sessionStore:       session.NewStore(filepath.Join(dir, ".atteler", "sessions")),
		cwd:                dir,
		selectedAgent:      "planner",
		promptContextCache: newPromptContextCache(),
		textarea:           textarea.New(),
	}
	m.textarea.SetValue("Render")
	m.textarea.CursorEnd()

	_, firstOK := m.promptSuggestion()
	_, secondOK := m.promptSuggestion()

	assert.False(t, firstOK)
	assert.False(t, secondOK)

	select {
	case <-indexStarted:
	case <-time.After(promptContextTestBackgroundBudget):
		require.Fail(t, "render-time prompt suggestion did not start repo warmup")
	}

	assert.Equal(t, int32(1), indexCalls.Load(), "render-time prompt suggestions should share one in-flight repo warmup")

	releaseOnce.Do(func() {
		close(releaseIndex)
	})

	select {
	case <-indexDone:
	case <-time.After(promptContextTestBackgroundBudget):
		require.Fail(t, "render-time prompt context warmup did not stop")
	}
}

//nolint:paralleltest // Uses process-wide prompt context source hooks.
func TestPromptSuggestionSkipsLargeRepoIndexDuringRender(t *testing.T) {
	dir := t.TempDir()

	for i := range defaultPromptContextMaxIndexFiles + 1 {
		subdir := filepath.Join(dir, fmt.Sprintf("pkg%02d", i/100))
		require.NoError(t, os.MkdirAll(subdir, 0o700))
		require.NoError(
			t,
			os.WriteFile(
				filepath.Join(subdir, fmt.Sprintf("large%04d.go", i)),
				fmt.Appendf(nil, "package pkg%02d\n\nfunc LargeSymbol%04d() {}\n", i/100, i),
				0o600,
			),
		)
	}

	var indexCalls atomic.Int32

	t.Cleanup(setPromptCodeIndexDirContextForTest(func(context.Context, string) (codeintel.Index, error) {
		indexCalls.Add(1)

		return codeintel.Index{}, nil
	}))

	store := session.NewStore(filepath.Join(dir, ".atteler", "sessions"))
	cache := newPromptContextCache()
	m := model{
		ctx:                context.Background(),
		sessionStore:       store,
		cwd:                dir,
		promptContextCache: cache,
		textarea:           textarea.New(),
	}
	m.textarea.SetValue("Large")
	m.textarea.CursorEnd()

	start := time.Now()
	suggestion, ok := m.promptSuggestion()
	elapsed := time.Since(start)

	assert.False(t, ok)
	assert.Empty(t, suggestion.Text)
	assert.Less(t, elapsed, promptContextTestReturnBudget)

	key := promptContextCacheKeyForState(appState{
		cwd:                dir,
		sessionStore:       store,
		promptContextCache: cache,
	})

	var snapshot promptRepoContextSnapshot

	require.Eventually(t, func() bool {
		var snapshotOK bool

		snapshot, snapshotOK = cache.freshSnapshot(key, time.Now(), time.Minute)

		return snapshotOK
	}, promptContextTestBackgroundBudget, 10*time.Millisecond)

	projectSymbols := statusForSource(t, snapshot.Sources, promptContextSourceProjectSymbols)
	assert.Equal(t, promptContextFreshnessSkipped, projectSymbols.Status)
	assert.Contains(t, projectSymbols.Detail, "go file count exceeds limit")
	assert.Equal(t, int32(0), indexCalls.Load(), "large render-time repos should skip indexing before invoking codeintel")
}

func TestAcceptCompletionAcceptsValidPromptSuggestion(t *testing.T) {
	t.Parallel()

	m := model{textarea: textarea.New()}
	m.textarea.SetValue("summ")
	m.textarea.CursorEnd()

	nextModel, cmd, handled := m.acceptCompletion()
	next, ok := nextModel.(model)
	require.True(t, ok)
	require.True(t, handled)
	require.Nil(t, cmd)
	assert.Equal(t, "summarize this session with changed files and verification evidence", next.textarea.Value())
}

func TestAcceptCompletionForcesSuggestionWhenMissing(t *testing.T) {
	t.Parallel()

	m := newIdleSuggestionTestModel("draft prompt with tests")
	m.textarea.SetValue("draft prompt")
	m.textarea.CursorEnd()

	nextModel, cmd, handled := m.acceptCompletion()
	next, ok := nextModel.(model)
	require.True(t, ok)
	require.True(t, handled)
	require.NotNil(t, cmd)
	assert.Equal(t, "draft prompt", next.textarea.Value())
	assert.Equal(t, idleSuggestionStatusPendingForced, next.idleSuggestionStatus)
	assert.Equal(t, "draft prompt", next.idleSuggestionInput)

	msg, ok := cmd().(idleSuggestionMsg)
	require.True(t, ok)
	require.True(t, msg.force)
	require.NoError(t, msg.err)

	nextModel, cmd = next.updateIdleSuggestion(msg)
	next, ok = nextModel.(model)
	require.True(t, ok)
	require.NotNil(t, cmd)
	assert.Equal(t, "draft prompt with tests", next.textarea.Value())
	assert.Empty(t, next.idleSuggestionStatus)
	assert.Empty(t, next.idleSuggestionText)
}

func TestAcceptCompletionSkipsForcedSuggestionWhenInputEmpty(t *testing.T) {
	t.Parallel()

	provider := &capturingIdleSuggestionProvider{model: "model", response: "draft prompt"}
	registry := llm.NewRegistry()
	registry.Register(provider)

	m := model{
		ctx:                     context.Background(),
		registry:                registry,
		selectedModel:           "capture/model",
		promptSuggestionConsent: promptSuggestionConsentSession,
		idleSuggestionBudget:    defaultIdleSuggestionBudget(),
		textarea:                textarea.New(),
	}
	m.textarea.CursorEnd()

	nextModel, cmd, handled := m.acceptCompletion()
	next, ok := nextModel.(model)
	require.True(t, ok)
	require.True(t, handled)
	require.NotNil(t, cmd)
	assert.Equal(t, idleSuggestionStatusRejectedEmpty, next.idleSuggestionStatus)
	assert.Empty(t, next.idleSuggestionInput)

	msg, ok := cmd().(idleSuggestionMsg)
	assert.False(t, ok, "empty prompts must not send provider-backed idle suggestions")
	assert.Equal(t, idleSuggestionMsg{}, msg)
	assert.Nil(t, provider.params)
}

func TestAcceptCompletionShowsForcedSuggestionStatus(t *testing.T) {
	t.Parallel()

	m := newIdleSuggestionTestModel("draft prompt with tests")
	m.textarea.SetValue("draft prompt")
	m.textarea.CursorEnd()

	nextModel, cmd, handled := m.acceptCompletion()
	next, ok := nextModel.(model)
	require.True(t, ok)
	require.True(t, handled)
	require.NotNil(t, cmd)

	status := stripANSI(next.statusLine())
	assert.Contains(t, status, "suggestion:"+idleSuggestionStatusPendingForced+":suggest/model")
	assert.Contains(t, status, "ctx=agent=")
	assert.Contains(t, status, "file/task/issue=omitted-private")
}

func TestAcceptCompletionRetriesRejectedSuggestionStates(t *testing.T) {
	t.Parallel()

	for _, status := range []string{idleSuggestionStatusRejectedStale, idleSuggestionStatusRejectedError} {
		t.Run(status, func(t *testing.T) {
			t.Parallel()

			m := newIdleSuggestionTestModel("draft prompt with tests")
			m.idleSuggestionID = 7
			m.idleSuggestionInput = "draft prompt"
			m.idleSuggestionText = " stale"
			m.idleSuggestionStatus = status
			m.textarea.SetValue("draft prompt")
			m.textarea.CursorEnd()

			nextModel, cmd, handled := m.acceptCompletion()
			next, ok := nextModel.(model)
			require.True(t, ok)
			require.True(t, handled)
			require.NotNil(t, cmd)
			assert.Equal(t, idleSuggestionStatusPendingForced, next.idleSuggestionStatus)
			assert.Equal(t, 8, next.idleSuggestionID)

			msg, ok := cmd().(idleSuggestionMsg)
			require.True(t, ok)
			require.True(t, msg.force)
			require.NoError(t, msg.err)

			nextModel, _ = next.updateIdleSuggestion(msg)
			next, ok = nextModel.(model)
			require.True(t, ok)
			assert.Equal(t, "draft prompt with tests", next.textarea.Value())
		})
	}
}

func TestAcceptCompletionDebouncesForcedSuggestion(t *testing.T) {
	t.Parallel()

	m := newIdleSuggestionTestModel("draft prompt with tests")
	m.textarea.SetValue("draft prompt")
	m.textarea.CursorEnd()

	nextModel, firstCmd, handled := m.acceptCompletion()
	next, ok := nextModel.(model)
	require.True(t, ok)
	require.True(t, handled)
	require.NotNil(t, firstCmd)

	nextModel, secondCmd, handled := next.acceptCompletion()
	next, ok = nextModel.(model)
	require.True(t, ok)
	require.True(t, handled)
	require.Nil(t, secondCmd)
	assert.Equal(t, idleSuggestionStatusPendingForced, next.idleSuggestionStatus)
	assert.Equal(t, "draft prompt", next.textarea.Value())
}

func TestAcceptCompletionSupersedesScheduledIdleSuggestion(t *testing.T) {
	t.Parallel()

	m := newIdleSuggestionTestModel("draft prompt with tests")
	m.textarea.SetValue("draft prompt")
	m.textarea.CursorEnd()
	cmd := m.scheduleIdleSuggestion()
	require.NotNil(t, cmd)

	oldID := m.idleSuggestionID

	nextModel, forceCmd, handled := m.acceptCompletion()
	next, ok := nextModel.(model)
	require.True(t, ok)
	require.True(t, handled)
	require.NotNil(t, forceCmd)
	assert.Equal(t, oldID+1, next.idleSuggestionID)
	assert.Equal(t, idleSuggestionStatusPendingForced, next.idleSuggestionStatus)

	nextModel, oldRequestCmd := next.updateIdleSuggestionRequest(idleSuggestionRequestMsg{
		id:    oldID,
		input: "draft prompt",
	})
	next, ok = nextModel.(model)
	require.True(t, ok)
	require.Nil(t, oldRequestCmd)
	assert.Equal(t, idleSuggestionStatusPendingForced, next.idleSuggestionStatus)
}

func TestAcceptCompletionReportsUnavailableForcedSuggestion(t *testing.T) {
	t.Parallel()

	registry := llm.NewRegistry()
	registry.Register(idleSuggestionProvider{model: "model", response: "draft prompt with tests"})

	for _, tc := range []struct {
		name  string
		model model
	}{
		{name: "no registry", model: model{}},
		{name: "local only", model: model{registry: registry, selectedModel: "suggest/model", promptLocalOnly: true}},
		{name: "waiting", model: model{registry: registry, selectedModel: "suggest/model", promptSuggestionConsent: promptSuggestionConsentSession, waiting: true}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			m := tc.model
			m.textarea = textarea.New()
			m.textarea.SetValue("draft prompt")
			m.textarea.CursorEnd()

			nextModel, cmd, handled := m.acceptCompletion()
			next, ok := nextModel.(model)
			require.True(t, ok)
			require.True(t, handled)
			require.NotNil(t, cmd)
			assert.Equal(t, "draft prompt", next.textarea.Value())
			assert.Equal(t, idleSuggestionStatusRejectedError, next.idleSuggestionStatus)
		})
	}
}

func TestAcceptCompletionDoesNotForceSuggestionWhenCursorIsMidline(t *testing.T) {
	t.Parallel()

	m := newIdleSuggestionTestModel("draft prompt with tests")
	m.textarea.SetValue("draft prompt")
	m.textarea.SetCursor(len("draft"))

	nextModel, cmd, handled := m.acceptCompletion()
	next, ok := nextModel.(model)
	require.True(t, ok)
	require.True(t, handled)
	require.Nil(t, cmd)
	assert.Equal(t, "draft prompt", next.textarea.Value())
	assert.Equal(t, idleSuggestionStatusRejectedStale, next.idleSuggestionStatus)
}

func TestAcceptCompletionCancelsInFlightIdleSuggestionBeforeForcing(t *testing.T) {
	t.Parallel()

	provider := &cancelAwareIdleSuggestionProvider{model: "model"}
	registry := llm.NewRegistry()
	registry.Register(provider)

	m := model{
		ctx:                     context.Background(),
		registry:                registry,
		selectedModel:           "cancel/model",
		promptSuggestionConsent: promptSuggestionConsentSession,
		idleSuggestionBudget:    defaultIdleSuggestionBudget(),
		textarea:                textarea.New(),
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
	idleID := next.idleSuggestionID

	nextModel, forceCmd, handled := next.acceptCompletion()
	forced, ok := nextModel.(model)
	require.True(t, ok)
	require.True(t, handled)
	require.NotNil(t, forceCmd)
	assert.Equal(t, idleID+1, forced.idleSuggestionID)
	assert.Equal(t, idleSuggestionStatusPendingForced, forced.idleSuggestionStatus)

	msg, ok := requestCmd().(idleSuggestionMsg)
	require.True(t, ok)
	require.Error(t, msg.err)
	assert.Contains(t, msg.err.Error(), "canceled")
	assert.False(t, provider.called)
}

func TestAcceptCompletionReportsForcedSuggestionErrors(t *testing.T) {
	t.Parallel()

	registry := llm.NewRegistry()
	registry.Register(failingIdleSuggestionProvider{model: "model"})

	m := model{
		ctx:                     context.Background(),
		registry:                registry,
		selectedModel:           "suggest/model",
		promptSuggestionConsent: promptSuggestionConsentSession,
		idleSuggestionBudget:    defaultIdleSuggestionBudget(),
		textarea:                textarea.New(),
	}
	m.textarea.SetValue("draft prompt")
	m.textarea.CursorEnd()

	nextModel, cmd, handled := m.acceptCompletion()
	next, ok := nextModel.(model)
	require.True(t, ok)
	require.True(t, handled)
	require.NotNil(t, cmd)

	msg, ok := cmd().(idleSuggestionMsg)
	require.True(t, ok)
	require.True(t, msg.force)
	require.Error(t, msg.err)

	nextModel, printCmd := next.updateIdleSuggestion(msg)
	next, ok = nextModel.(model)
	require.True(t, ok)
	require.NotNil(t, printCmd)
	assert.Equal(t, "draft prompt", next.textarea.Value())
	assert.Equal(t, idleSuggestionStatusRejectedError, next.idleSuggestionStatus)
}

func TestAcceptCompletionRejectsInvalidForcedSuggestions(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name       string
		response   string
		wantStatus string
	}{
		{name: "empty", response: "", wantStatus: idleSuggestionStatusRejectedEmpty},
		{name: "unsafe", response: "with tests\nrm -rf /", wantStatus: "rejected:unsafe-multiline"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			m := newIdleSuggestionTestModel(tc.response)
			m.textarea.SetValue("draft prompt")
			m.textarea.CursorEnd()

			nextModel, cmd, handled := m.acceptCompletion()
			next, ok := nextModel.(model)
			require.True(t, ok)
			require.True(t, handled)
			require.NotNil(t, cmd)

			msg, ok := cmd().(idleSuggestionMsg)
			require.True(t, ok)
			require.True(t, msg.force)

			nextModel, printCmd := next.updateIdleSuggestion(msg)
			next, ok = nextModel.(model)
			require.True(t, ok)
			require.NotNil(t, printCmd)
			assert.Equal(t, "draft prompt", next.textarea.Value())
			assert.Equal(t, tc.wantStatus, next.idleSuggestionStatus)
		})
	}
}

func TestIdleSuggestionRequestUsesProviderAndAcceptsSuffix(t *testing.T) {
	t.Parallel()

	m := newIdleSuggestionTestModel("draft prompt with tests")
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

func TestIdleSuggestionRequestEmitsContextManifestBeforeBudgetFailure(t *testing.T) {
	t.Parallel()

	registry := llm.NewRegistry()
	registry.Register(idleSuggestionProvider{model: "model", response: "draft prompt with tests"})

	var eventLog bytes.Buffer

	m := model{
		ctx:                     context.Background(),
		registry:                registry,
		hookRunner:              events.NewRunnerWithLogger(nil, &eventLog),
		selectedModel:           "suggest/model",
		promptSuggestionConsent: promptSuggestionConsentSession,
		idleSuggestionBudget:    defaultIdleSuggestionBudget(),
		maxInputTokens:          1,
		textarea:                textarea.New(),
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
	require.Error(t, msg.err)
	assert.Contains(t, msg.err.Error(), "max_input_tokens")

	log := eventLog.String()
	assert.Contains(t, log, "context_manifest")
	assert.Contains(t, log, "model=suggest/model")
	assert.Contains(t, log, "request_kind=background_suggestion")
	assert.Contains(t, log, "background_suggestion=true")
	assert.Contains(t, log, "context_summary=")
	assert.Contains(t, log, "file/task/issue=omitted-private")
	assert.Contains(t, log, "fits_configured_token_budget=false")
	assert.Contains(t, log, "max_input_tokens=1")
}

func TestIdleSuggestionRequestRoutesModelRoleAndEmitsDecision(t *testing.T) {
	t.Parallel()

	provider := &capturingIdleSuggestionProvider{
		providerName: "openai",
		model:        "gpt-4.1-mini",
		response:     "draft prompt with tests",
	}
	registry := llm.NewRegistry()
	registry.Register(provider)
	require.NoError(t, registry.SetModelRole("planner", llm.ModelRole{
		Preferred: "openai/gpt-4.1-mini",
	}))

	var eventLog bytes.Buffer

	m := model{
		ctx:                     context.Background(),
		registry:                registry,
		hookRunner:              events.NewRunnerWithLogger(nil, &eventLog),
		selectedModel:           "planner",
		promptSuggestionConsent: promptSuggestionConsentSession,
		idleSuggestionBudget:    defaultIdleSuggestionBudget(),
		maxInputTokens:          10_000,
		textarea:                textarea.New(),
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
	assert.Equal(t, "gpt-4.1-mini", provider.params.Model)
	assert.Equal(t, "openai", msg.provider)
	assert.Equal(t, "gpt-4.1-mini", msg.model)

	log := eventLog.String()
	assert.Contains(t, log, "event:context_manifest")
	assert.Contains(t, log, "model=openai/gpt-4.1-mini")
	assert.Contains(t, log, "event:route_decision")
	assert.Contains(t, log, "model_role=planner")
	assert.Contains(t, log, "phase=estimated")
	assert.Contains(t, log, "phase=actual")
	assert.Contains(t, log, "selected=openai/gpt-4.1-mini")
	assert.Contains(t, log, "fallback_order=openai/gpt-4.1-mini")
	assert.Contains(t, log, "actual_selected=openai/gpt-4.1-mini")
}

func TestIdleSuggestionContextManifestUsesBackgroundInputBudget(t *testing.T) {
	t.Parallel()

	registry := llm.NewRegistry()
	registry.Register(idleSuggestionProvider{model: "model", response: "draft prompt with tests"})

	var eventLog bytes.Buffer

	m := model{
		ctx:                     context.Background(),
		registry:                registry,
		hookRunner:              events.NewRunnerWithLogger(nil, &eventLog),
		selectedModel:           "suggest/model",
		promptSuggestionConsent: promptSuggestionConsentSession,
		idleSuggestionBudget:    defaultIdleSuggestionBudget(),
		textarea:                textarea.New(),
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

	log := eventLog.String()
	assert.Contains(t, log, "context_manifest")
	assert.Contains(t, log, "max_input_tokens=1024")
	assert.Contains(t, log, "fits_configured_token_budget=true")
}

func TestIdleSuggestionRequestUsesPrivacyScopedLocalContext(t *testing.T) {
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
				Description:     "plans implementation work with api_key=sk-testsecretvalue",
				ToolPermissions: map[string]bool{"bash": true},
			},
		}),
		sessionStore:            store,
		sessionState:            session.Session{Title: "Follow up on #15", Artifacts: []session.Artifact{{Path: "docs/notes.md", Kind: "notes"}}},
		selectedAgent:           "planner",
		selectedModel:           "capture/model",
		promptSuggestionConsent: promptSuggestionConsentSession,
		idleSuggestionBudget:    defaultIdleSuggestionBudget(),
		cwd:                     dir,
		textarea:                textarea.New(),
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
		"permission: bash",
		"privacy: private file/task/issue context omitted",
	} {
		assert.Contains(t, localContext, want)
	}

	for _, private := range []string{
		"sk-testsecretvalue",
		"plans implementation work",
		"file: docs/notes.md",
		"task: GH-27",
		"issue: #15",
		"issue: GH-27",
	} {
		assert.NotContains(t, localContext, private)
	}

	assert.Contains(t, msg.contextSummary, "file/task/issue=omitted-private")
}

func TestIdleSuggestionRequestAppliesOutputTokenBudget(t *testing.T) {
	t.Parallel()

	provider := &capturingIdleSuggestionProvider{model: "model", response: "draft prompt with tests"}
	registry := llm.NewRegistry()
	registry.Register(provider)

	m := model{
		ctx:                     context.Background(),
		registry:                registry,
		selectedModel:           "capture/model",
		promptSuggestionConsent: promptSuggestionConsentSession,
		idleSuggestionBudget: idleSuggestionBudget{
			MaxOutputTokens: 7,
		},
		textarea: textarea.New(),
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
	assert.Equal(t, 7, provider.params.MaxTokens)
	assert.Equal(t, 7, msg.estimatedOutputTokens)
}

func TestPromptLocalOnlySkipsIdleModelSuggestion(t *testing.T) {
	t.Parallel()

	registry := llm.NewRegistry()
	registry.Register(idleSuggestionProvider{model: "model", response: "draft prompt with tests"})

	m := model{
		ctx:                     context.Background(),
		registry:                registry,
		selectedModel:           "suggest/model",
		promptLocalOnly:         true,
		promptSuggestionConsent: promptSuggestionConsentSession,
		textarea:                textarea.New(),
	}
	m.textarea.SetValue("draft prompt")
	m.textarea.CursorEnd()

	assert.Nil(t, m.scheduleIdleSuggestion())
	assert.Empty(t, m.idleSuggestionInput)
	assert.Empty(t, m.idleSuggestionStatus)
}

func TestPromptLocalOnlyUpdatesPromptContextStatusFromCache(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	cache := newPromptContextCache()
	key := promptContextCacheKeyForState(appState{cwd: dir, promptContextCache: cache})
	cache.store(promptRepoContextSnapshot{
		CapturedAt: time.Now(),
		Key:        key,
		ProjectSymbols: []promptcomplete.Candidate{{
			Text: "CachedSymbol",
			Kind: "project-symbol",
		}},
		Sources: []promptContextSourceStatus{
			promptContextSourceReport(promptContextSourceProjectSymbols, promptContextFreshnessFresh, "1 candidate"),
		},
	})

	registry := llm.NewRegistry()
	registry.Register(idleSuggestionProvider{model: "model", response: "draft prompt with tests"})

	m := model{
		ctx:                context.Background(),
		registry:           registry,
		selectedModel:      "suggest/model",
		promptLocalOnly:    true,
		cwd:                dir,
		promptContextCache: cache,
		textarea:           textarea.New(),
	}
	m.textarea.SetValue("Cached")
	m.textarea.CursorEnd()

	assert.Nil(t, m.scheduleIdleSuggestion())
	assert.Empty(t, m.idleSuggestionInput)
	assert.Empty(t, m.idleSuggestionStatus)
	assert.Equal(t, string(promptContextFreshnessFresh), m.promptContextStatus)
}

func TestPromptLocalOnlySkipsForcedIdleModelSuggestion(t *testing.T) {
	t.Parallel()

	provider := &capturingIdleSuggestionProvider{model: "model", response: "draft prompt with tests"}
	registry := llm.NewRegistry()
	registry.Register(provider)

	m := model{
		ctx:                     context.Background(),
		registry:                registry,
		selectedModel:           "capture/model",
		promptLocalOnly:         true,
		promptSuggestionConsent: promptSuggestionConsentSession,
		idleSuggestionBudget:    defaultIdleSuggestionBudget(),
		textarea:                textarea.New(),
	}
	m.textarea.SetValue("draft prompt")
	m.textarea.CursorEnd()

	nextModel, cmd, handled := m.acceptCompletion()
	next, ok := nextModel.(model)
	require.True(t, ok)
	require.True(t, handled)
	require.NotNil(t, cmd)
	assert.Equal(t, idleSuggestionStatusRejectedError, next.idleSuggestionStatus)

	_, isProviderRequest := cmd().(idleSuggestionMsg)
	assert.False(t, isProviderRequest)
	assert.Nil(t, provider.params)
}

func TestEmptyPromptSkipsForcedIdleModelSuggestion(t *testing.T) {
	t.Parallel()

	provider := &capturingIdleSuggestionProvider{model: "model", response: "draft prompt with tests"}
	registry := llm.NewRegistry()
	registry.Register(provider)

	m := model{
		ctx:                     context.Background(),
		registry:                registry,
		selectedModel:           "capture/model",
		promptSuggestionConsent: promptSuggestionConsentSession,
		idleSuggestionBudget:    defaultIdleSuggestionBudget(),
		textarea:                textarea.New(),
	}
	m.textarea.SetValue("  ")
	m.textarea.CursorEnd()

	nextModel, cmd, handled := m.acceptCompletion()
	next, ok := nextModel.(model)
	require.True(t, ok)
	require.True(t, handled)
	require.NotNil(t, cmd)
	assert.Equal(t, idleSuggestionStatusRejectedEmpty, next.idleSuggestionStatus)

	_, isProviderRequest := cmd().(idleSuggestionMsg)
	assert.False(t, isProviderRequest)
	assert.Nil(t, provider.params)
}

func TestPromptSuggestionConsentFromPreferences(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		statePreference   config.PreferenceResolution
		name              string
		sessionPreference string
		want              promptSuggestionConsent
		promptLocalOnly   bool
	}{
		{
			name: "fresh install defaults local only",
			want: promptSuggestionConsentLocalOnly,
		},
		{
			name:              "prompt local only overrides model backed session",
			promptLocalOnly:   true,
			sessionPreference: string(config.PromptSuggestionPreferenceModelBacked),
			want:              promptSuggestionConsentLocalOnly,
		},
		{
			name:              "session opt in wins over persisted local only",
			sessionPreference: string(config.PromptSuggestionPreferenceModelBacked),
			statePreference: config.PreferenceResolution{
				Value: string(config.PromptSuggestionPreferenceLocalOnly),
				Scope: config.ModelScopeFolder,
			},
			want: promptSuggestionConsentSession,
		},
		{
			name:              "session local only wins over persisted model backed",
			sessionPreference: string(config.PromptSuggestionPreferenceLocalOnly),
			statePreference: config.PreferenceResolution{
				Value: string(config.PromptSuggestionPreferenceModelBacked),
				Scope: config.ModelScopeGlobal,
			},
			want: promptSuggestionConsentLocalOnly,
		},
		{
			name:              "session no-network alias is local only",
			sessionPreference: "no-network",
			statePreference: config.PreferenceResolution{
				Value: string(config.PromptSuggestionPreferenceModelBacked),
				Scope: config.ModelScopeGlobal,
			},
			want: promptSuggestionConsentLocalOnly,
		},
		{
			name: "folder model backed",
			statePreference: config.PreferenceResolution{
				Value: string(config.PromptSuggestionPreferenceModelBacked),
				Scope: config.ModelScopeFolder,
			},
			want: promptSuggestionConsentFolder,
		},
		{
			name: "state alias model backed",
			statePreference: config.PreferenceResolution{
				Value: "provider",
				Scope: config.ModelScopeFolder,
			},
			want: promptSuggestionConsentFolder,
		},
		{
			name: "global model backed",
			statePreference: config.PreferenceResolution{
				Value: string(config.PromptSuggestionPreferenceModelBacked),
				Scope: config.ModelScopeGlobal,
			},
			want: promptSuggestionConsentGlobal,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			assert.Equal(
				t,
				tc.want,
				promptSuggestionConsentFromPreferences(tc.promptLocalOnly, tc.sessionPreference, tc.statePreference),
			)
		})
	}
}

func TestIdleSuggestionDefaultRequiresModelBackedOptIn(t *testing.T) {
	t.Parallel()

	provider := &capturingIdleSuggestionProvider{model: "model", response: "draft prompt with tests"}
	registry := llm.NewRegistry()
	registry.Register(provider)

	m := model{
		ctx:           context.Background(),
		registry:      registry,
		selectedModel: "capture/model",
		textarea:      textarea.New(),
	}
	m.textarea.SetValue("draft prompt")
	m.textarea.CursorEnd()

	assert.Nil(t, m.scheduleIdleSuggestion())

	nextModel, cmd, handled := m.acceptCompletion()
	next, ok := nextModel.(model)
	require.True(t, ok)
	require.True(t, handled)
	require.NotNil(t, cmd)
	assert.Equal(t, idleSuggestionStatusRejectedError, next.idleSuggestionStatus)

	_, isProviderRequest := cmd().(idleSuggestionMsg)
	assert.False(t, isProviderRequest)
	assert.Nil(t, provider.params)
}

func TestIdleSuggestionStatusShowsProviderModelAndContextSummary(t *testing.T) {
	t.Parallel()

	m := newIdleSuggestionTestModel("draft prompt with tests")
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

	status := stripANSI(next.statusLine())
	assert.Contains(t, status, "suggestion:"+idleSuggestionStatusSending+":suggest/model")
	assert.Contains(t, status, "ctx=agent=")
	assert.Contains(t, status, "file/task/issue=omitted-private")
}

func TestIdleSuggestionRecordsBackgroundUsageMetadata(t *testing.T) {
	t.Parallel()

	registry := llm.NewRegistry()
	registry.Register(idleSuggestionProvider{
		model:    "model",
		response: "draft prompt with tests",
	})

	m := model{
		ctx:                     context.Background(),
		registry:                registry,
		sessionState:            session.Session{ID: "session-id"},
		selectedModel:           "suggest/model",
		promptSuggestionConsent: promptSuggestionConsentSession,
		idleSuggestionBudget:    defaultIdleSuggestionBudget(),
		textarea:                textarea.New(),
	}
	m.textarea.SetValue("draft prompt")
	m.textarea.CursorEnd()

	_, requestCmd := m.updateIdleSuggestionRequest(idleSuggestionRequestMsg{
		id:    m.idleSuggestionID + 1,
		input: "draft prompt",
	})
	assert.Nil(t, requestCmd, "unscheduled request should stay stale")

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

	nextModel, _ = next.updateIdleSuggestion(msg)
	next, ok = nextModel.(model)
	require.True(t, ok)

	require.NotNil(t, next.sessionState.BackgroundSuggestions)
	assert.Equal(t, 1, next.sessionState.BackgroundSuggestions.Requests)
	assert.Equal(t, 1, next.sessionState.BackgroundSuggestions.ProviderCalls)
	assert.Equal(t, idleSuggestionStatusReadyModel, next.sessionState.BackgroundSuggestions.LastStatus)
	assert.Contains(t, next.sessionState.BackgroundSuggestions.LastContextSummary, "omitted-private")
}

func TestIdleSuggestionUsageStaysSeparateFromChatUsage(t *testing.T) {
	t.Parallel()

	registry := llm.NewRegistry()
	registry.Register(idleSuggestionProvider{
		model:        "model",
		response:     "draft prompt with tests",
		inputTokens:  11,
		outputTokens: 3,
	})

	m := model{
		ctx:                     context.Background(),
		registry:                registry,
		sessionState:            session.Session{ID: "session-id"},
		selectedModel:           "suggest/model",
		promptSuggestionConsent: promptSuggestionConsentSession,
		idleSuggestionBudget:    defaultIdleSuggestionBudget(),
		tokenUsage:              tokenUsage{InputTokens: 100, OutputTokens: 20, Responses: 1},
		textarea:                textarea.New(),
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

	nextModel, saveCmd := next.updateIdleSuggestion(msg)
	next, ok = nextModel.(model)
	require.True(t, ok)
	require.NotNil(t, saveCmd)

	assert.Equal(t, tokenUsage{InputTokens: 100, OutputTokens: 20, Responses: 1}, next.tokenUsage)
	assert.Equal(t, 11, next.idleSuggestionUsage.InputTokens)
	assert.Equal(t, 3, next.idleSuggestionUsage.OutputTokens)
	assert.Equal(t, 1, next.idleSuggestionUsage.Responses)
	require.NotNil(t, next.sessionState.BackgroundSuggestions)
	assert.Equal(t, 11, next.sessionState.BackgroundSuggestions.InputTokens)
	assert.Equal(t, 3, next.sessionState.BackgroundSuggestions.OutputTokens)
	assert.Equal(t, 1, next.sessionState.BackgroundSuggestions.Responses)
}

func TestIdleSuggestionRecordsProviderErrorsAsBackgroundUsage(t *testing.T) {
	t.Parallel()

	registry := llm.NewRegistry()
	registry.Register(failingIdleSuggestionProvider{model: "model"})

	m := model{
		ctx:                     context.Background(),
		registry:                registry,
		sessionState:            session.Session{ID: "session-id"},
		selectedModel:           "suggest/model",
		promptSuggestionConsent: promptSuggestionConsentSession,
		idleSuggestionBudget:    defaultIdleSuggestionBudget(),
		textarea:                textarea.New(),
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
	require.Error(t, msg.err)
	assert.True(t, msg.providerCall)

	nextModel, saveCmd := next.updateIdleSuggestion(msg)
	next, ok = nextModel.(model)
	require.True(t, ok)
	require.NotNil(t, saveCmd)

	require.NotNil(t, next.sessionState.BackgroundSuggestions)
	assert.Equal(t, idleSuggestionStatusRejectedError, next.idleSuggestionStatus)
	assert.Equal(t, 1, next.sessionState.BackgroundSuggestions.Requests)
	assert.Equal(t, 1, next.sessionState.BackgroundSuggestions.ProviderCalls)
	assert.Equal(t, 0, next.sessionState.BackgroundSuggestions.Responses)
	assert.Equal(t, 1, next.sessionState.BackgroundSuggestions.Errors)
	assert.Equal(t, idleSuggestionStatusRejectedError, next.sessionState.BackgroundSuggestions.LastStatus)
	assert.Contains(t, next.sessionState.BackgroundSuggestions.LastContextSummary, "omitted-private")
	assert.Positive(t, next.sessionState.BackgroundSuggestions.EstimatedInputTokens)
	assert.Positive(t, next.sessionState.BackgroundSuggestions.EstimatedCostUSD)
}

func TestIdleSuggestionBudgetExhaustionSkipsProvider(t *testing.T) {
	t.Parallel()

	provider := &capturingIdleSuggestionProvider{model: "model", response: "draft prompt with tests"}
	registry := llm.NewRegistry()
	registry.Register(provider)

	m := model{
		ctx:                     context.Background(),
		registry:                registry,
		selectedModel:           "capture/model",
		promptSuggestionConsent: promptSuggestionConsentSession,
		idleSuggestionBudget: idleSuggestionBudget{
			MaxRequestsPerSession: 1,
			MaxInputTokens:        idleSuggestionMaxInputTokens,
			MaxOutputTokens:       idleSuggestionMaxOutputTokens,
			MaxSessionTokens:      idleSuggestionMaxSessionTokens,
			MaxEstimatedCostUSD:   idleSuggestionMaxEstimatedCostUSD,
		},
		idleSuggestionRequests: 1,
		textarea:               textarea.New(),
	}
	m.textarea.SetValue("draft prompt")
	m.textarea.CursorEnd()

	cmd := m.scheduleIdleSuggestion()
	assert.Nil(t, cmd)

	nextModel, cmd, handled := m.acceptCompletion()
	next, ok := nextModel.(model)
	require.True(t, ok)
	require.True(t, handled)
	require.NotNil(t, cmd)
	assert.Equal(t, idleSuggestionStatusRejectedBudget, next.idleSuggestionStatus)
	assert.Nil(t, provider.params)
}

func TestIdleSuggestionRateBudgetExhaustionSkipsProvider(t *testing.T) {
	t.Parallel()

	provider := &capturingIdleSuggestionProvider{model: "model", response: "draft prompt with tests"}
	registry := llm.NewRegistry()
	registry.Register(provider)

	m := model{
		ctx:                     context.Background(),
		registry:                registry,
		selectedModel:           "capture/model",
		promptSuggestionConsent: promptSuggestionConsentSession,
		idleSuggestionBudget: idleSuggestionBudget{
			MaxRequestsPerSession: idleSuggestionMaxRequestsPerSession,
			MaxRequestsPerMinute:  1,
			MaxInputTokens:        idleSuggestionMaxInputTokens,
			MaxOutputTokens:       idleSuggestionMaxOutputTokens,
			MaxSessionTokens:      idleSuggestionMaxSessionTokens,
			MaxEstimatedCostUSD:   idleSuggestionMaxEstimatedCostUSD,
		},
		idleSuggestionTimes: []time.Time{time.Now()},
		textarea:            textarea.New(),
	}
	m.textarea.SetValue("draft prompt")
	m.textarea.CursorEnd()

	assert.Nil(t, m.scheduleIdleSuggestion())

	nextModel, cmd, handled := m.acceptCompletion()
	next, ok := nextModel.(model)
	require.True(t, ok)
	require.True(t, handled)
	require.NotNil(t, cmd)
	assert.Equal(t, idleSuggestionStatusRejectedBudget, next.idleSuggestionStatus)
	assert.Nil(t, provider.params)
}

func TestIdleSuggestionRateBudgetIgnoresExpiredRequestTimes(t *testing.T) {
	t.Parallel()

	now := time.Now()
	recent := now.Add(-30 * time.Second)
	expired := now.Add(-2 * time.Minute)

	assert.Equal(t, 1, idleSuggestionRecentRequestCount([]time.Time{expired, recent}, now))

	requestTimes := []time.Time{expired, recent}

	pruned := appendIdleSuggestionRequestTime(requestTimes, now)
	require.Len(t, pruned, 2)
	assert.Equal(t, recent, pruned[0])
	assert.Equal(t, now, pruned[1])
	assert.Equal(t, []time.Time{expired, recent}, requestTimes)
}

func TestInitialModelHonorsPersistedIdleSuggestionBudgetUsage(t *testing.T) {
	t.Parallel()

	registry := llm.NewRegistry()
	registry.Register(idleSuggestionProvider{model: "model", response: "draft prompt with tests"})

	sessionState := session.Session{
		BackgroundSuggestions: &session.BackgroundSuggestionUsage{
			Requests:              idleSuggestionMaxRequestsPerSession,
			InputTokens:           12,
			OutputTokens:          4,
			EstimatedInputTokens:  500,
			EstimatedOutputTokens: 32,
			EstimatedCostUSD:      0.01,
		},
	}

	m := initialModel(
		context.Background(),
		registry,
		agent.NewRegistry(nil),
		nil,
		nil,
		nil,
		sessionState,
		contextref.Options{},
		nil,
		"",
		contextref.ReferenceManifest{},
		"",
		config.VectorConfig{},
		"",
		"",
		false,
		"",
		t.TempDir(),
		"suggest/model",
		"",
		nil,
		generationSettings{},
		generationSettings{},
		llm.AgentLoopBudget{},
		autonomy.DefaultLevel,
		0,
		0,
		false,
		false,
		promptSuggestionConsentSession,
		defaultIdleSuggestionBudget(),
		nil,
		nil,
		nil,
	)
	m.textarea.SetValue("draft prompt")
	m.textarea.CursorEnd()

	assert.Equal(t, idleSuggestionMaxRequestsPerSession, m.idleSuggestionRequests)
	assert.Equal(t, 532, m.idleSuggestionTokens)
	assert.InDelta(t, 0.01, m.idleSuggestionCostUSD, 0.0000001)
	assert.Nil(t, m.scheduleIdleSuggestion())
}

func TestIdleSuggestionCostBudgetUsesFallbackEstimate(t *testing.T) {
	t.Parallel()

	primaryCost := estimateIdleSuggestionCost(nil, "openai/gpt-5.4-nano", 1000, idleSuggestionMaxOutputTokens)
	fallbackCost := estimateIdleSuggestionCost(nil, "openai/gpt-5.5", 1000, idleSuggestionMaxOutputTokens)
	estimatedCost := estimateIdleSuggestionCostWithFallbacks(
		nil,
		"openai/gpt-5.4-nano",
		[]string{"openai/gpt-5.5"},
		1000,
		idleSuggestionMaxOutputTokens,
	)

	require.Positive(t, primaryCost)
	require.Positive(t, fallbackCost)
	assert.Greater(t, fallbackCost, primaryCost)
	assert.InDelta(t, fallbackCost, estimatedCost, 0.0000001)
}

func TestIdleSuggestionBudgetUsesConservativeFallbackInputEstimate(t *testing.T) {
	t.Parallel()

	registry := llm.NewRegistry()
	registry.Register(&capturingIdleSuggestionProvider{providerName: "openai", model: "gpt-5.4-nano"})
	registry.Register(&capturingIdleSuggestionProvider{providerName: "anthropic", model: "claude-haiku-4-5-20251001"})

	messages := []llm.Message{{Role: llm.RoleUser, Content: "draft prompt"}}
	primaryEstimate, _ := estimateMessagesForModel(registry, "openai/gpt-5.4-nano", messages)
	fallbackEstimate, _ := estimateMessagesForModel(registry, "anthropic/claude-haiku-4-5-20251001", messages)
	require.Greater(t, fallbackEstimate.UpperBoundTokens, primaryEstimate.UpperBoundTokens)

	estimatedInputTokens, _, err := validateIdleSuggestionRequestBudget(
		registry,
		"openai/gpt-5.4-nano",
		[]string{"anthropic/claude-haiku-4-5-20251001"},
		messages,
		idleSuggestionMaxInputTokens,
		idleSuggestionBudget{
			MaxRequestsPerSession: idleSuggestionMaxRequestsPerSession,
			MaxInputTokens:        idleSuggestionMaxInputTokens,
			MaxOutputTokens:       idleSuggestionMaxOutputTokens,
			MaxSessionTokens:      idleSuggestionMaxSessionTokens,
			MaxEstimatedCostUSD:   1,
		},
		tokenUsage{},
		0,
		0,
	)
	require.NoError(t, err)
	assert.Equal(t, fallbackEstimate.UpperBoundTokens, estimatedInputTokens)
}

func TestIdleSuggestionCostBudgetUsesConservativeEstimateForUnknownProviderModel(t *testing.T) {
	t.Parallel()

	provider := &capturingIdleSuggestionProvider{
		providerName: "custom",
		model:        "unknown",
		response:     "draft prompt with tests",
	}
	registry := llm.NewRegistry()
	registry.Register(provider)

	estimatedCost := estimateIdleSuggestionCost(registry, "custom/unknown", 1000, idleSuggestionMaxOutputTokens)
	require.Positive(t, estimatedCost)

	_, _, err := validateIdleSuggestionRequestBudget(
		registry,
		"custom/unknown",
		nil,
		[]llm.Message{{Role: llm.RoleUser, Content: "draft prompt"}},
		idleSuggestionMaxInputTokens,
		idleSuggestionBudget{
			MaxRequestsPerSession: idleSuggestionMaxRequestsPerSession,
			MaxInputTokens:        idleSuggestionMaxInputTokens,
			MaxOutputTokens:       idleSuggestionMaxOutputTokens,
			MaxSessionTokens:      idleSuggestionMaxSessionTokens,
			MaxEstimatedCostUSD:   0.000000000001,
		},
		tokenUsage{},
		0,
		0,
	)
	require.ErrorIs(t, err, errIdleSuggestionBudget)
	assert.Contains(t, err.Error(), "estimated cost")
}

func TestIdleSuggestionCostBudgetTreatsOllamaModelsAsZeroCost(t *testing.T) {
	t.Parallel()

	provider := &capturingIdleSuggestionProvider{
		providerName: "ollama",
		model:        "llama3.2",
		response:     "draft prompt with tests",
	}
	registry := llm.NewRegistry()
	registry.Register(provider)

	assert.Zero(t, estimateIdleSuggestionCost(nil, "ollama/llama3.2", 1000, idleSuggestionMaxOutputTokens))
	assert.Zero(t, estimateIdleSuggestionCost(registry, "llama3.2", 1000, idleSuggestionMaxOutputTokens))
}

func TestIdleSuggestionEstimatedCostBudgetExhaustionSkipsProvider(t *testing.T) {
	t.Parallel()

	provider := &capturingIdleSuggestionProvider{
		providerName: "openai",
		model:        "gpt-5.5",
		response:     "draft prompt with tests",
	}
	registry := llm.NewRegistry()
	registry.Register(provider)

	m := model{
		ctx:                     context.Background(),
		registry:                registry,
		selectedModel:           "openai/gpt-5.5",
		promptSuggestionConsent: promptSuggestionConsentSession,
		idleSuggestionBudget: idleSuggestionBudget{
			MaxRequestsPerSession: idleSuggestionMaxRequestsPerSession,
			MaxInputTokens:        idleSuggestionMaxInputTokens,
			MaxOutputTokens:       idleSuggestionMaxOutputTokens,
			MaxSessionTokens:      idleSuggestionMaxSessionTokens,
			MaxEstimatedCostUSD:   0.000000000001,
		},
		textarea: textarea.New(),
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
	require.Error(t, msg.err)
	require.ErrorIs(t, msg.err, errIdleSuggestionBudget)
	assert.Contains(t, msg.err.Error(), "estimated cost")
	assert.False(t, msg.providerCall)
	assert.Nil(t, provider.params)

	nextModel, saveCmd := next.updateIdleSuggestion(msg)
	next, ok = nextModel.(model)
	require.True(t, ok)
	require.NotNil(t, saveCmd)
	assert.Equal(t, idleSuggestionStatusRejectedBudget, next.idleSuggestionStatus)
	require.NotNil(t, next.sessionState.BackgroundSuggestions)
	assert.Equal(t, 1, next.sessionState.BackgroundSuggestions.Requests)
	assert.Equal(t, 0, next.sessionState.BackgroundSuggestions.ProviderCalls)
	assert.Equal(t, 0, next.sessionState.BackgroundSuggestions.Responses)
	assert.Equal(t, 1, next.sessionState.BackgroundSuggestions.Errors)
	assert.InDelta(t, 0, next.sessionState.BackgroundSuggestions.EstimatedCostUSD, 0.0000001)
	assert.Equal(t, 0, next.sessionState.BackgroundSuggestions.EstimatedInputTokens)
	assert.InDelta(t, 0, next.idleSuggestionCostUSD, 0.0000001)
	assert.Equal(t, 0, next.idleSuggestionTokens)
}

func TestIdleSuggestionRequestRejectsCursorMoved(t *testing.T) {
	t.Parallel()

	m := newIdleSuggestionTestModel("draft prompt with tests")
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
	assert.Equal(t, idleSuggestionStatusRejectedStale, next.idleSuggestionStatus)
}

func TestClearIdleSuggestionCancelsInFlightRequest(t *testing.T) {
	t.Parallel()

	provider := &cancelAwareIdleSuggestionProvider{model: "model"}
	registry := llm.NewRegistry()
	registry.Register(provider)

	m := model{
		ctx:                     context.Background(),
		registry:                registry,
		selectedModel:           "cancel/model",
		promptSuggestionConsent: promptSuggestionConsentSession,
		idleSuggestionBudget:    defaultIdleSuggestionBudget(),
		textarea:                textarea.New(),
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
	assert.False(t, msg.providerCall)
	assert.Nil(t, next.idleSuggestionCancel)
}

func TestClearIdleSuggestionClearsPromptContextStatus(t *testing.T) {
	t.Parallel()

	m := model{
		idleSuggestionInput:  "draft",
		idleSuggestionText:   " suffix",
		idleSuggestionStatus: idleSuggestionStatusReadyModel,
		promptContextStatus:  "stale:project-symbols",
	}

	m.clearIdleSuggestion()

	assert.Empty(t, m.idleSuggestionInput)
	assert.Empty(t, m.idleSuggestionText)
	assert.Empty(t, m.idleSuggestionStatus)
	assert.Empty(t, m.promptContextStatus)
}

func TestIdleSuggestionIgnoresStaleResponses(t *testing.T) {
	t.Parallel()

	m := model{textarea: textarea.New()}
	m.textarea.SetValue("new input")
	m.idleSuggestionID = 2
	m.idleSuggestionInput = "new input"
	m.idleSuggestionStatus = idleSuggestionStatusReadyModel
	m.idleSuggestionText = " suffix"

	nextModel, _ := m.updateIdleSuggestion(idleSuggestionMsg{
		id:         1,
		input:      "old input",
		suggestion: " old suffix",
	})
	next, ok := nextModel.(model)
	require.True(t, ok)
	assert.Equal(t, " suffix", next.idleSuggestionText)
	assert.Equal(t, idleSuggestionStatusReadyModel, next.idleSuggestionStatus)
}

func TestIdleSuggestionRejectsCurrentResponseWhenInputChanged(t *testing.T) {
	t.Parallel()

	m := model{textarea: textarea.New()}
	m.textarea.SetValue("new input")
	m.textarea.CursorEnd()
	m.idleSuggestionID = 1
	m.idleSuggestionInput = "old input"

	nextModel, _ := m.updateIdleSuggestion(idleSuggestionMsg{
		id:         1,
		input:      "old input",
		suggestion: " old suffix",
	})
	next, ok := nextModel.(model)
	require.True(t, ok)
	assert.Empty(t, next.idleSuggestionText)
	assert.Equal(t, idleSuggestionStatusRejectedStale, next.idleSuggestionStatus)
}

func TestIdleSuggestionRecordsStaleProviderResponsesAsBackgroundUsage(t *testing.T) {
	t.Parallel()

	m := newIdleSuggestionTestModel("draft prompt with tests")
	m.sessionState = session.Session{ID: "session-id"}
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
	require.True(t, msg.providerCall)

	next.textarea.SetValue("changed prompt")
	next.textarea.CursorEnd()

	nextModel, saveCmd := next.updateIdleSuggestion(msg)
	next, ok = nextModel.(model)
	require.True(t, ok)
	require.NotNil(t, saveCmd)

	assert.Equal(t, "changed prompt", next.textarea.Value())
	assert.Empty(t, next.idleSuggestionText)
	assert.Equal(t, idleSuggestionStatusRejectedStale, next.idleSuggestionStatus)
	assert.Nil(t, next.idleSuggestionCancel)
	require.NotNil(t, next.sessionState.BackgroundSuggestions)
	assert.Equal(t, 1, next.sessionState.BackgroundSuggestions.Requests)
	assert.Equal(t, 1, next.sessionState.BackgroundSuggestions.ProviderCalls)
	assert.Equal(t, 1, next.sessionState.BackgroundSuggestions.Rejected)
	assert.Equal(t, idleSuggestionStatusRejectedStale, next.sessionState.BackgroundSuggestions.LastStatus)
	assert.Contains(t, next.sessionState.BackgroundSuggestions.LastContextSummary, "omitted-private")
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

func TestPromptTextareaBindsShiftEnterForNewline(t *testing.T) {
	t.Parallel()

	ta := newPromptTextarea()

	assert.Equal(t, []string{keyShiftEnter, keyAltEnter}, ta.KeyMap.InsertNewline.Keys())
	assert.Contains(t, ta.KeyMap.InsertNewline.Help().Key, "shift/alt+enter")
	assert.Contains(t, promptInputHelp, "Shift/Alt+Enter newline")
	assert.Contains(t, ta.Placeholder, "Shift/Alt+Enter newline")
	assert.Contains(t, ta.Placeholder, "Enter to send")
}

func TestEnableTerminalShiftEnterReportingUsesKeyboardEnhancementStack(t *testing.T) {
	t.Parallel()

	var out strings.Builder

	restore := enableTerminalShiftEnterReporting(&out)
	assert.Equal(t, "\x1b[>1u", out.String())

	restore()
	assert.Equal(t, "\x1b[>1u\x1b[<1u", out.String())
}

func TestUpdate_ShiftEnterInsertsNewlineWithoutSubmitting(t *testing.T) {
	t.Parallel()

	m := model{
		textarea: newPromptTextarea(),
		waiting:  true,
	}
	m.textarea.SetValue("first line")
	m.textarea.CursorEnd()

	nextModel, _ := m.Update(fakeUnknownCSI("13;2u"))
	next, ok := nextModel.(model)
	require.True(t, ok)
	assert.True(t, next.waiting)
	assert.Empty(t, next.queuedPrompts)
	assert.Equal(t, "first line\n", next.textarea.Value())
}

func TestUpdate_ShiftEnterSplitsInputAtCursor(t *testing.T) {
	t.Parallel()

	m := model{
		textarea: newPromptTextarea(),
		waiting:  true,
	}
	m.textarea.SetValue("firstsecond")
	m.textarea.SetCursor(len("first"))

	nextModel, _ := m.Update(fakeUnknownCSI("13;2u"))
	next, ok := nextModel.(model)
	require.True(t, ok)
	assert.True(t, next.waiting)
	assert.Empty(t, next.queuedPrompts)
	assert.Equal(t, "first\nsecond", next.textarea.Value())
}

func TestUpdate_TerminalReportedAltEnterInsertsNewline(t *testing.T) {
	t.Parallel()

	m := model{
		textarea: newPromptTextarea(),
		waiting:  true,
	}
	m.textarea.SetValue("first line")
	m.textarea.CursorEnd()

	nextModel, _ := m.Update(fakeUnknownCSI("13;3u"))
	next, ok := nextModel.(model)
	require.True(t, ok)
	assert.True(t, next.waiting)
	assert.Empty(t, next.queuedPrompts)
	assert.Equal(t, "first line\n", next.textarea.Value())
}

func TestUpdate_NativeAltEnterInsertsNewline(t *testing.T) {
	t.Parallel()

	m := model{
		textarea: newPromptTextarea(),
		waiting:  true,
	}
	m.textarea.SetValue("first line")
	m.textarea.CursorEnd()

	nextModel, _ := m.Update(tea.KeyMsg{Type: tea.KeyEnter, Alt: true})
	next, ok := nextModel.(model)
	require.True(t, ok)
	assert.True(t, next.waiting)
	assert.Empty(t, next.queuedPrompts)
	assert.Equal(t, "first line\n", next.textarea.Value())
}

func TestUpdate_EnterStillSubmitsInput(t *testing.T) {
	t.Parallel()

	m := model{
		textarea:            newPromptTextarea(),
		waiting:             true,
		promptHistoryCursor: -1,
	}
	m.textarea.SetValue("first line")

	nextModel, cmd := m.Update(tea.KeyMsg{Type: tea.KeyEnter})
	next, ok := nextModel.(model)
	require.True(t, ok)
	require.NotNil(t, cmd)
	assert.True(t, next.waiting)
	assert.Empty(t, next.textarea.Value())
	assert.Equal(t, []string{"first line"}, next.queuedPrompts)
}

func TestUpdate_TerminalReportedEnterStillSubmitsInput(t *testing.T) {
	t.Parallel()

	m := model{
		textarea:            newPromptTextarea(),
		waiting:             true,
		promptHistoryCursor: -1,
	}
	m.textarea.SetValue("first line")

	nextModel, cmd := m.Update(fakeUnknownCSI("13;1u"))
	next, ok := nextModel.(model)
	require.True(t, ok)
	require.NotNil(t, cmd)
	assert.True(t, next.waiting)
	assert.Empty(t, next.textarea.Value())
	assert.Equal(t, []string{"first line"}, next.queuedPrompts)
}

func TestUpdate_TerminalReportedCtrlOStillOpensModelPicker(t *testing.T) {
	t.Parallel()

	nextModel, _ := (model{
		ctx:      context.Background(),
		registry: llm.NewRegistry(),
	}).Update(fakeUnknownCSI("111;5u"))
	next, ok := nextModel.(model)
	require.True(t, ok)
	assert.True(t, next.pickerOpen)
}

func TestUpdate_MultilineInputCanBeSubmitted(t *testing.T) {
	t.Parallel()

	m := model{
		textarea:            newPromptTextarea(),
		waiting:             true,
		promptHistoryCursor: -1,
	}
	m.textarea.SetValue("first line")
	m.textarea.CursorEnd()

	nextModel, _ := m.Update(fakeUnknownCSI("13;2u"))
	next, ok := nextModel.(model)
	require.True(t, ok)

	nextModel, _ = next.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("second line")})
	next, ok = nextModel.(model)
	require.True(t, ok)

	nextModel, cmd := next.Update(tea.KeyMsg{Type: tea.KeyEnter})
	next, ok = nextModel.(model)
	require.True(t, ok)
	require.NotNil(t, cmd)
	assert.Empty(t, next.textarea.Value())
	assert.Equal(t, []string{"first line\nsecond line"}, next.queuedPrompts)
	assert.Equal(t, []string{"first line\nsecond line"}, next.promptHistory)
}

func TestIsShiftEnterKeyName(t *testing.T) {
	t.Parallel()

	for _, keyName := range []string{
		keyShiftEnter,
		fakeUnknownCSI("13;2u").String(),
		fakeUnknownCSI("13;2:1u").String(),
		fakeUnknownCSI("13;2:2u").String(),
		fakeUnknownCSI("13;66u").String(),
		fakeUnknownCSI("13;130u").String(),
		fakeUnknownCSI("13;2~").String(),
		fakeUnknownCSI("27;2;13~").String(),
		fakeUnknownCSI("27;130;13~").String(),
		"\x1b[13;2u",
	} {
		assert.True(t, isShiftEnterKeyName(keyName), "keyName %q", keyName)
	}

	for _, keyName := range []string{
		keyEnter,
		keyAltEnter,
		fakeUnknownCSI("13;1u").String(),
		fakeUnknownCSI("13;2:3u").String(),
		fakeUnknownCSI("13;2:1:1u").String(),
		fakeUnknownCSI("13;2;99u").String(),
		fakeUnknownCSI("13;258u").String(),
		fakeUnknownCSI("13;5u").String(),
		fakeUnknownCSI("13;6u").String(),
		fakeUnknownCSI("27;5;13~").String(),
		"?CSI[wat]?",
	} {
		assert.False(t, isShiftEnterKeyName(keyName), "keyName %q", keyName)
	}
}

func TestIsAltEnterKeyName(t *testing.T) {
	t.Parallel()

	for _, keyName := range []string{
		keyAltEnter,
		fakeUnknownCSI("13;3u").String(),
		fakeUnknownCSI("13;3:1u").String(),
		fakeUnknownCSI("13;3:2u").String(),
		fakeUnknownCSI("13;67u").String(),
		fakeUnknownCSI("13;131u").String(),
		fakeUnknownCSI("13;3~").String(),
		fakeUnknownCSI("27;3;13~").String(),
		fakeUnknownCSI("27;131;13~").String(),
		"\x1b[13;3u",
	} {
		assert.True(t, isAltEnterKeyName(keyName), "keyName %q", keyName)
	}

	for _, keyName := range []string{
		keyEnter,
		keyShiftEnter,
		fakeUnknownCSI("13;1u").String(),
		fakeUnknownCSI("13;3:3u").String(),
		fakeUnknownCSI("13;3:1:1u").String(),
		fakeUnknownCSI("13;3;99u").String(),
		fakeUnknownCSI("13;259u").String(),
		fakeUnknownCSI("13;5u").String(),
		fakeUnknownCSI("13;6u").String(),
		fakeUnknownCSI("27;5;13~").String(),
		"?CSI[wat]?",
	} {
		assert.False(t, isAltEnterKeyName(keyName), "keyName %q", keyName)
	}
}

func TestTerminalControlKeyMsg(t *testing.T) {
	t.Parallel()

	for _, tt := range []struct {
		name string
		msg  tea.Msg
		want string
	}{
		{name: "enter without modifier field", msg: fakeUnknownCSI("13u"), want: keyEnter},
		{name: "enter with explicit no modifier", msg: fakeUnknownCSI("13;1u"), want: keyEnter},
		{name: "enter with empty modifier field", msg: fakeUnknownCSI("13;u"), want: keyEnter},
		{name: "escape", msg: fakeUnknownCSI("27u"), want: keyEsc},
		{name: "tab", msg: fakeUnknownCSI("9u"), want: "tab"},
		{name: "shift tab", msg: fakeUnknownCSI("9;2u"), want: "shift+tab"},
		{name: "backspace", msg: fakeUnknownCSI("127u"), want: "backspace"},
		{name: "ctrl c", msg: fakeUnknownCSI("99;5u"), want: keyCtrlC},
		{name: "ctrl d", msg: fakeUnknownCSI("100;5u"), want: "ctrl+d"},
		{name: "ctrl o", msg: fakeUnknownCSI("111;5u"), want: "ctrl+o"},
		{name: "ctrl r", msg: fakeUnknownCSI("114;5u"), want: "ctrl+r"},
		{name: "ctrl z", msg: fakeUnknownCSI("122;5u"), want: "ctrl+z"},
		{name: "ctrl uppercase o", msg: fakeUnknownCSI("79;5u"), want: "ctrl+o"},
		{name: "ctrl o with caps lock", msg: fakeUnknownCSI("111;69u"), want: "ctrl+o"},
		{name: "ctrl o with num lock", msg: fakeUnknownCSI("111;133u"), want: "ctrl+o"},
		{name: "enter with caps lock", msg: fakeUnknownCSI("13;65u"), want: keyEnter},
		{name: "enter with num lock", msg: fakeUnknownCSI("13;129u"), want: keyEnter},
		{name: "raw enter", msg: stringerMsg("\x1b[13u"), want: keyEnter},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			got, ok := terminalControlKeyMsg(tt.msg)
			require.True(t, ok)
			assert.Equal(t, tt.want, got.String())
		})
	}

	for _, msg := range []tea.Msg{
		tea.KeyMsg{Type: tea.KeyEnter},
		fakeUnknownCSI("13;2u"),
		fakeUnknownCSI("13;1:3u"),
		fakeUnknownCSI("13;258u"),
		fakeUnknownCSI("97;2;65u"),
		fakeUnknownCSI("111;6u"),
		fakeUnknownCSI("13;1;99u"),
	} {
		got, ok := terminalControlKeyMsg(msg)
		assert.False(t, ok, "msg %q produced %q", msg, got.String())
	}
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
		generationOverrides: generationSettings{ReasoningLevel: testReasoningXHigh},
	}

	plain := stripANSI(m.statusLine())
	assert.Contains(t, plain, "model:"+testCodexModel)
	assert.Contains(t, plain, "effort:"+testReasoningXHigh)
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

func TestStatusLineShowsAgentLoopBudget(t *testing.T) {
	t.Parallel()

	m := model{
		selectedModel: "gpt-test",
		agentLoopBudget: llm.AgentLoopBudget{
			MaxInputTokens:  100,
			MaxOutputTokens: 50,
			MaxCostMicros:   25_000,
		},
	}

	plain := stripANSI(m.statusLine())
	assert.Contains(t, plain, "budget:")
	assert.Contains(t, plain, "in=100")
	assert.Contains(t, plain, "out=50")
	assert.Contains(t, plain, "costµ=25000")
}

func TestStatusLineShowsPromptContextFreshness(t *testing.T) {
	t.Parallel()

	m := model{promptContextStatus: "stale:project-symbols"}

	plain := stripANSI(m.statusLine())
	assert.Contains(t, plain, "promptctx:stale:project-symbols")
}

func TestContextUsageUsesProviderAwareUpperBound(t *testing.T) {
	t.Parallel()

	registry := llm.NewRegistry()
	registry.Register(modelPickerProvider{name: "anthropic", models: []string{"claude-test"}})

	m := model{
		registry:      registry,
		selectedModel: "claude-test",
		history:       []llm.Message{{Role: llm.RoleUser, Content: strings.Repeat("context ", 20)}},
	}
	estimate, _ := estimateMessagesForModel(registry, m.selectedModel, m.history)

	got := m.contextUsage()
	assert.Contains(t, got, "ctx≤")
	assert.Contains(t, got, formatTokenCount(estimate.UpperBoundTokens))
}

func TestContextSummaryUsesProviderAwareUpperBound(t *testing.T) {
	t.Parallel()

	registry := llm.NewRegistry()
	registry.Register(modelPickerProvider{name: "anthropic", models: []string{"claude-test"}})

	m := model{
		registry:       registry,
		selectedModel:  "claude-test",
		history:        []llm.Message{{Role: llm.RoleUser, Content: strings.Repeat("context ", 20)}},
		pinnedMessages: map[int]bool{0: true},
	}
	estimate, _ := estimateMessagesForModel(registry, m.selectedModel, m.history)

	got := m.contextSummary()
	assert.Contains(t, got, "pinned=1")
	assert.Contains(t, got, "upper_bound="+formatTokenCount(estimate.UpperBoundTokens))
	assert.Contains(t, got, "estimator=anthropic-calibrated")
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

func TestLLMToolLogCommands_SkipsBufferedLogsAfterLiveStreaming(t *testing.T) {
	t.Parallel()

	assert.Empty(t, llmToolLogCommands([]string{"$ command\nlive output"}, true))

	cmds := llmToolLogCommands([]string{"$ command\nbuffered output"}, false)
	require.Len(t, cmds, 1)
	assert.Contains(t, stripANSI(toStringMsg(cmds[0]())), "buffered output")
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

type fakeUnknownCSI string

func (f fakeUnknownCSI) String() string {
	return fmt.Sprintf("?CSI%+v?", []byte(string(f)))
}

type stringerMsg string

func (s stringerMsg) String() string {
	return string(s)
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
	observer := &recordingTUIObserver{}
	budget := llm.AgentLoopBudget{
		MaxCostMicros:   25_000,
		MaxInputTokens:  100,
		MaxOutputTokens: 50,
	}
	state := appState{
		registry:       llm.NewRegistry(),
		agentRegistry:  agent.NewRegistry(nil),
		hookRunner:     events.NewRunnerWithLogger(nil, panicWriter{}),
		eventObservers: []events.Observer{observer},
		sessionStore:   store,
		sessionState:   session.New("gpt-test", nil),
		contextOptions: contextref.Options{Root: t.TempDir()},
		selectedModel:  "gpt-test",
		generationDefaults: generationSettings{
			ModelMode:      llm.ModelModeFast,
			ReasoningLevel: "high",
		},
		agentLoopBudget: budget,
		cwd:             t.TempDir(),
	}

	require.NoError(t, runInteractive(context.Background(), state))

	require.NotEmpty(t, observer.events)

	for _, eventType := range []string{events.SessionStart, events.SessionEnd} {
		event := observer.eventByType(eventType)
		require.NotNilf(t, event, "missing %s event", eventType)
		require.Contains(t, event.Metadata, "agent_loop_budget")

		var decoded llm.AgentLoopBudget
		require.NoError(t, json.Unmarshal([]byte(event.Metadata["agent_loop_budget"]), &decoded))
		assert.Equal(t, budget, decoded)
		assert.Equal(t, llm.ModelModeFast, event.Metadata["model_mode"])
		assert.Equal(t, "high", event.Metadata["reasoning_level"])
	}
}

type panicWriter struct{}

func (panicWriter) Write([]byte) (int, error) {
	panic("hook logger should not be used before the TUI program starts")
}

type recordingTUIObserver struct {
	events []events.Event
}

func (o *recordingTUIObserver) ObserveEvent(_ context.Context, event events.Event) error {
	o.events = append(o.events, event)

	return nil
}

func (o *recordingTUIObserver) eventByType(eventType string) *events.Event {
	for i := range o.events {
		if o.events[i].Type == eventType {
			return &o.events[i]
		}
	}

	return nil
}

//nolint:paralleltest // Captures process stdout while runInteractive uses fmt.Println.
func TestRunInteractiveHeaderShowsInputNewlineHelp(t *testing.T) {
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
		hookRunner:     events.NewRunner(nil),
		sessionStore:   store,
		sessionState:   session.New("gpt-test", nil),
		contextOptions: contextref.Options{Root: t.TempDir()},
		selectedModel:  "gpt-test",
		cwd:            t.TempDir(),
	}

	output := captureStdout(t, func() {
		require.NoError(t, runInteractive(context.Background(), state))
	})

	assert.Equal(t, 1, strings.Count(output, "\x1b[>1u"))
	assert.Equal(t, 1, strings.Count(output, "\x1b[<1u"))

	plain := stripANSI(output)
	assert.Contains(t, plain, "Ctrl+D to quit")
	assert.Contains(t, plain, "Enter to send")
	assert.Contains(t, plain, "Shift/Alt+Enter newline")
}

//nolint:paralleltest // Captures process stdout while startup status uses fmt.Println.
func TestStartupPromptSuggestionStatusShowsPromptLocalOnlyOverride(t *testing.T) {
	output := captureStdout(t, func() {
		printStartupPromptSuggestionStatus(appState{
			promptLocalOnly:         true,
			promptSuggestionConsent: promptSuggestionConsentGlobal,
		})
	})

	plain := stripANSI(output)
	assert.Contains(t, plain, "Prompt suggestions: local-only")
	assert.Contains(t, plain, "--prompt-local-only overrides saved opt-ins")
}

//nolint:paralleltest // Captures process stdout while startup status uses fmt.Println.
func TestStartupPromptSuggestionStatusShowsOptInHintByDefault(t *testing.T) {
	output := captureStdout(t, func() {
		printStartupPromptSuggestionStatus(appState{})
	})

	plain := stripANSI(output)
	assert.Contains(t, plain, "Prompt suggestions: local-only")
	assert.Contains(t, plain, "/suggestions session|folder|global to opt in")
}

//nolint:paralleltest // Captures process stdout while startup status uses fmt.Println.
func TestStartupPromptSuggestionStatusShowsModelBackedPrivacyScope(t *testing.T) {
	output := captureStdout(t, func() {
		printStartupPromptSuggestionStatus(appState{
			promptSuggestionConsent: promptSuggestionConsentFolder,
		})
	})

	plain := stripANSI(output)
	assert.Contains(t, plain, "Prompt suggestions: model-backed folder scope")
	assert.Contains(t, plain, "private file/task/issue context omitted")
}
