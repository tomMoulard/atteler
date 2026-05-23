package main

import (
	"path/filepath"
	"strings"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/tommoulard/atteler/pkg/config"
	"github.com/tommoulard/atteler/pkg/events"
	"github.com/tommoulard/atteler/pkg/llm"
	"github.com/tommoulard/atteler/pkg/lsp"
	"github.com/tommoulard/atteler/pkg/mcp"
	"github.com/tommoulard/atteler/pkg/subagent"
)

func TestFormatMCPServer(t *testing.T) {
	t.Parallel()

	got := formatMCPServer(mcp.Server{
		Name:         "repo",
		Command:      "atteler-mcp",
		Args:         []string{"--repo", "."},
		CWD:          "/repo",
		Capabilities: []string{"symbols", "memory"},
	})
	for _, want := range []string{
		"repo",
		"command=atteler-mcp",
		"args=--repo,.",
		"cwd=/repo",
		"capabilities=memory,symbols",
	} {
		if !strings.Contains(got, want) {
			require.Failf(t, "formatted MCP server missing content", "missing %q in %q", want, got)
		}
	}
}

func TestMCPInvokeHelpers(t *testing.T) {
	t.Parallel()

	args, err := parseMCPToolArgs(`{"query":"symbols"}`)
	require.NoError(t, err)
	assert.Equal(t, map[string]any{"query": "symbols"}, args)

	param, err := parseJSONParam(`[1,"two"]`, "mcp params")
	require.NoError(t, err)
	assert.Equal(t, []any{float64(1), "two"}, param)

	response := &mcp.Response{Result: []byte(`{"ok":true,"count":2}`)}
	got := formatMCPResponse(response)
	assert.Contains(t, got, `"ok": true`)
	assert.Contains(t, got, `"count": 2`)
}

func TestRunMCPInvokeRequiresManifest(t *testing.T) {
	t.Parallel()

	err := runMCPInvoke(t.Context(), mcpInvokeCommandInput{ToolName: "lookup"})

	require.Error(t, err)
	assert.Contains(t, err.Error(), "--mcp-manifest")
	assert.Contains(t, err.Error(), "atteler help plugins")
}

func TestMCPCommandInputsFromOptions(t *testing.T) {
	t.Parallel()

	opts := cliOptions{
		mcpManifestPath: "mcp.yaml",
		mcpCapability:   "symbols",
		mcpServerName:   "repo",
		mcpMethod:       "tools/list",
		mcpParamsJSON:   `{"cursor":"next"}`,
		mcpToolName:     "lookup",
		mcpToolArgsJSON: `{"query":"Registry"}`,
		mcpTimeout: positiveIntFlag{
			value: 3,
			set:   true,
		},
	}

	assert.Equal(t, mcpManifestCommandInput{
		Path:       "mcp.yaml",
		Capability: "symbols",
	}, mcpManifestCommandInputFromOptions(opts))

	assert.Equal(t, mcpInvokeCommandInput{
		ManifestPath:   "mcp.yaml",
		ServerName:     "repo",
		Method:         "tools/list",
		ParamsJSON:     `{"cursor":"next"}`,
		ToolName:       "lookup",
		ToolArgsJSON:   `{"query":"Registry"}`,
		TimeoutSeconds: 3,
	}, mcpInvokeCommandInputFromOptions(opts))
}

func TestFormatLSPSymbols(t *testing.T) {
	t.Parallel()

	got := formatLSPSymbols([]lsp.Symbol{{
		Name:           "Handle",
		Kind:           12,
		Detail:         "func()",
		ContainerName:  "server",
		URI:            "file:///repo/main.go",
		Range:          lsp.Range{Start: lsp.Position{Line: 2, Character: 1}, End: lsp.Position{Line: 4, Character: 2}},
		SelectionRange: lsp.Range{Start: lsp.Position{Line: 2, Character: 6}, End: lsp.Position{Line: 2, Character: 12}},
		Children: []lsp.Symbol{{
			Name:  "child",
			Kind:  13,
			Range: lsp.Range{Start: lsp.Position{Line: 3, Character: 1}, End: lsp.Position{Line: 3, Character: 5}},
		}},
	}})

	assert.Contains(t, got, "Handle\tkind=12\trange=2:1-4:2\tdetail=func()\tcontainer=server\turi=file:///repo/main.go")
	assert.Contains(t, got, "  child\tkind=13\trange=3:1-3:5")
}

func TestFormatHookEventType(t *testing.T) {
	t.Parallel()

	got := formatHookEventType(events.SupportedEventType{
		Type:        events.AgentExecute,
		Description: "Emitted when a configured agent is selected for work.",
	})

	assert.Equal(t, "agent_execute\tEmitted when a configured agent is selected for work.", got)
}

func TestCommandOutputEventCarriesRenderedOutput(t *testing.T) {
	t.Parallel()

	event := commandOutputEvent(
		"session-id",
		"/tmp/session.json",
		"reviewer",
		"gpt-test",
		"/repo",
		"go test ./...",
		"$ go test ./...\nok",
		assert.AnError,
		map[string]string{"source": "user"},
	)

	assert.Equal(t, events.CommandOutput, event.Type)
	assert.Equal(t, "session-id", event.SessionID)
	assert.Equal(t, "$ go test ./...\nok", event.Content)
	assert.Equal(t, assert.AnError.Error(), event.Error)
	assert.Equal(t, "go test ./...", event.Metadata["command"])
	assert.Equal(t, "/repo", event.Metadata["cwd"])
	assert.Equal(t, "user", event.Metadata["source"])
}

func TestShellCommandInputsFromOptions(t *testing.T) {
	t.Parallel()

	opts := cliOptions{
		bashCommand: "go test ./cmd/atteler",
		bashDir:     "cmd/atteler",
		bashTimeout: positiveIntFlag{
			value: 10,
			set:   true,
		},
		spawnAgentSpecs: rawStringListFlag{"reviewer|check diff"},
		spawnBinary:     "atteler-dev",
		spawnTimeout: positiveIntFlag{
			value: 20,
			set:   true,
		},
		spawnDryRun: true,
	}

	assert.Equal(t, bashCommandInput{
		Command:        "go test ./cmd/atteler",
		Dir:            "cmd/atteler",
		TimeoutSeconds: 10,
	}, bashCommandInputFromOptions(opts))

	assert.Equal(t, spawnAgentsCommandInput{
		Specs:          []string{"reviewer|check diff"},
		Binary:         "atteler-dev",
		TimeoutSeconds: 20,
		DryRun:         true,
	}, spawnAgentsCommandInputFromOptions(opts))
}

func TestParseSpawnAgentSpecs(t *testing.T) {
	t.Parallel()

	requests, err := parseSpawnAgentSpecs([]string{
		"architect|draft design",
		"child-review|reviewer|check the diff",
	})
	require.NoError(t, err)
	assert.Equal(t, []subagent.Request{
		{ID: "child-1", Agent: "architect", Prompt: "draft design"},
		{ID: "child-review", Agent: "reviewer", Prompt: "check the diff"},
	}, requests)

	got := formatSpawnDryRun(requests)
	assert.Contains(t, got, "Would spawn 2 sub-agent(s).")
	assert.Contains(t, got, "id=child-review\tagent=reviewer\tprompt=check the diff")
}

func TestFormatSpawnResults(t *testing.T) {
	t.Parallel()

	got := formatSpawnResults([]subagent.Result{{
		Request:  subagent.Request{ID: "child-1", Agent: "reviewer"},
		Output:   "done\n",
		Duration: 1500 * time.Millisecond,
	}, {
		Request:  subagent.Request{ID: "child-2", Agent: "critic"},
		Error:    "boom",
		Duration: time.Millisecond,
	}})

	assert.Contains(t, got, "id=child-1\tagent=reviewer\tstatus=ok\tduration=1.5s")
	assert.Contains(t, got, "output=done")
	assert.Contains(t, got, "id=child-2\tagent=critic\tstatus=error\tduration=1ms")
	assert.Contains(t, got, "error=boom")
}

func TestSelectModelStoresProviderQualifiedModel(t *testing.T) {
	t.Parallel()

	m := model{}
	next, _ := m.selectModel(pickerItem{provider: "codex", model: "gpt-5.5"}, config.ModelScopeSession)

	selected, ok := next.(model)
	if !ok {
		require.Failf(t, "unexpected failure", "selectModel returned %T, want model", next)
	}

	if selected.selectedModel != testCodexModel {
		require.Failf(t, "unexpected failure", "selectedModel = %q, want codex/gpt-5.5", selected.selectedModel)
	}

	if selected.sessionState.DefaultModel != testCodexModel {
		require.Failf(t, "unexpected failure", "DefaultModel = %q, want codex/gpt-5.5", selected.sessionState.DefaultModel)
	}

	if !selected.modelLocked {
		require.FailNow(t, "model should be locked after selection")
	}

	if selected.generationOverrides.ReasoningLevel != "" {
		require.Failf(t, "unexpected failure", "ReasoningLevel = %q, want empty", selected.generationOverrides.ReasoningLevel)
	}

	if selected.generationOverrides.ModelMode != "" {
		require.Failf(t, "unexpected failure", "ModelMode = %q, want empty", selected.generationOverrides.ModelMode)
	}
}

func TestSelectModelDefaultReasoningClearsOverride(t *testing.T) {
	t.Parallel()

	m := model{
		generationOverrides: generationSettings{ReasoningLevel: testReasoningXHigh},
	}
	next, _ := m.selectModel(
		pickerItem{provider: "codex", model: "gpt-5.5", reasoning: llm.ReasoningLevelDefault},
		config.ModelScopeSession,
	)
	selected, ok := next.(model)
	require.True(t, ok)

	if selected.selectedModel != testCodexModel {
		require.Failf(t, "unexpected failure", "selectedModel = %q, want codex/gpt-5.5", selected.selectedModel)
	}

	if selected.generationOverrides.ReasoningLevel != "" {
		require.Failf(t, "unexpected failure", "ReasoningLevel = %q, want cleared", selected.generationOverrides.ReasoningLevel)
	}
}

func TestSelectModelAppliesReasoningOverride(t *testing.T) {
	t.Parallel()

	m := model{}
	next, _ := m.selectModel(
		pickerItem{provider: "codex", model: "gpt-5.5", reasoning: testReasoningXHigh},
		config.ModelScopeSession,
	)
	selected, ok := next.(model)
	require.True(t, ok)

	if selected.selectedModel != testCodexModel {
		require.Failf(t, "unexpected failure", "selectedModel = %q, want codex/gpt-5.5 (no reasoning suffix)", selected.selectedModel)
	}

	if selected.sessionState.DefaultModel != testCodexModel {
		require.Failf(t, "unexpected failure", "DefaultModel = %q, want codex/gpt-5.5", selected.sessionState.DefaultModel)
	}

	if selected.generationOverrides.ReasoningLevel != testReasoningXHigh {
		require.Failf(t, "unexpected failure", "ReasoningLevel = %q, want xhigh", selected.generationOverrides.ReasoningLevel)
	}
}

func TestSelectModelAppliesModelModeOverride(t *testing.T) {
	t.Parallel()

	m := model{}
	next, _ := m.selectModel(
		pickerItem{provider: "codex", model: "gpt-5.4", modelMode: llm.ModelModeFast, reasoning: llm.ReasoningLevelDefault},
		config.ModelScopeSession,
	)
	selected, ok := next.(model)
	require.True(t, ok)

	assert.Equal(t, "codex/gpt-5.4", selected.selectedModel)
	assert.Equal(t, llm.ModelModeFast, selected.generationOverrides.ModelMode)
	assert.Equal(t, llm.ModelModeFast, selected.sessionState.DefaultModelMode)
}

func TestSelectModelDefaultModeAppliesExplicitDefault(t *testing.T) {
	t.Parallel()

	m := model{
		generationOverrides: generationSettings{ModelMode: llm.ModelModeFast},
	}
	next, _ := m.selectModel(
		pickerItem{provider: "codex", model: "gpt-5.4", modelMode: llm.ModelModeDefault, reasoning: llm.ReasoningLevelDefault},
		config.ModelScopeSession,
	)
	selected, ok := next.(model)
	require.True(t, ok)

	assert.Equal(t, llm.ModelModeDefault, selected.generationOverrides.ModelMode)
	assert.Equal(t, llm.ModelModeDefault, selected.sessionState.DefaultModelMode)
}

func TestSelectModelPersistsFolderModel(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	store := config.NewStateStore(filepath.Join(t.TempDir(), "state.yaml"))
	m := model{stateStore: store, cwd: dir}

	next, cmd := m.selectModel(
		pickerItem{provider: "claude-code", model: "claude-opus-4-6"},
		config.ModelScopeFolder,
	)

	selected, ok := next.(model)
	if !ok {
		require.Failf(t, "unexpected failure", "selectModel returned %T, want model", next)
	}

	if !selected.modelLocked {
		require.FailNow(t, "model should be locked")
	}

	raw := cmd()

	batch, ok := raw.(tea.BatchMsg)
	if !ok {
		require.Failf(t, "unexpected failure", "cmd returned %T, want tea.BatchMsg", raw)
	}

	if len(batch) != 2 {
		require.Failf(t, "unexpected failure", "batched commands = %d, want 2", len(batch))
	}

	saveRaw := batch[1]()

	saveMsg, ok := saveRaw.(modelPreferenceSavedMsg)
	if !ok {
		require.Failf(t, "unexpected failure", "save cmd returned %T, want modelPreferenceSavedMsg", saveRaw)
	}

	if saveMsg.err != nil {
		require.NoError(t, saveMsg.err)
	}

	state, err := store.Load()
	if err != nil {
		require.NoError(t, err)
	}

	if got := state.ModelForFolder(dir); got != "claude-code/claude-opus-4-6" {
		require.Failf(t, "unexpected failure", "folder model = %q", got)
	}
}

func TestSelectModelPersistsFolderReasoning(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	store := config.NewStateStore(filepath.Join(t.TempDir(), "state.yaml"))
	m := model{stateStore: store, cwd: dir}

	next, cmd := m.selectModel(
		pickerItem{provider: "codex", model: "gpt-5.5", reasoning: testReasoningXHigh},
		config.ModelScopeFolder,
	)

	selected, ok := next.(model)
	require.True(t, ok)
	assert.Equal(t, testReasoningXHigh, selected.generationOverrides.ReasoningLevel)
	assert.Equal(t, testReasoningXHigh, selected.sessionState.DefaultReasoningLevel)

	raw := cmd()
	batch, ok := raw.(tea.BatchMsg)
	require.True(t, ok)
	require.Len(t, batch, 2)
	saveRaw := batch[1]()
	saveMsg, ok := saveRaw.(modelPreferenceSavedMsg)
	require.True(t, ok)
	require.NoError(t, saveMsg.err)

	state, err := store.Load()
	require.NoError(t, err)
	assert.Equal(t, testCodexModel, state.ModelForFolder(dir))
	assert.Equal(t, testReasoningXHigh, state.ReasoningLevelForFolder(dir))
}

func TestSelectModelPersistsFolderModelMode(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	store := config.NewStateStore(filepath.Join(t.TempDir(), "state.yaml"))
	m := model{stateStore: store, cwd: dir}

	next, cmd := m.selectModel(
		pickerItem{provider: "codex", model: "gpt-5.4", modelMode: llm.ModelModeFast, reasoning: llm.ReasoningLevelDefault},
		config.ModelScopeFolder,
	)

	selected, ok := next.(model)
	require.True(t, ok)
	assert.Equal(t, llm.ModelModeFast, selected.generationOverrides.ModelMode)
	assert.Equal(t, llm.ModelModeFast, selected.sessionState.DefaultModelMode)

	raw := cmd()
	batch, ok := raw.(tea.BatchMsg)
	require.True(t, ok)
	require.Len(t, batch, 2)
	saveRaw := batch[1]()
	saveMsg, ok := saveRaw.(modelPreferenceSavedMsg)
	require.True(t, ok)
	require.NoError(t, saveMsg.err)

	state, err := store.Load()
	require.NoError(t, err)
	assert.Equal(t, "codex/gpt-5.4", state.ModelForFolder(dir))
	assert.Equal(t, llm.ModelModeFast, state.ModelModeForFolder(dir))
}
