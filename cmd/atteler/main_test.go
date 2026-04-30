package main

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/tommoulard/atteler/pkg/agent"
	"github.com/tommoulard/atteler/pkg/config"
	"github.com/tommoulard/atteler/pkg/llm"
	"github.com/tommoulard/atteler/pkg/session"
)

const testCodexModel = "codex/gpt-5.5"

func TestResolveAgent_InlineOverridesSelected(t *testing.T) {
	t.Parallel()
	registry := agent.NewRegistry(map[string]config.AgentConfig{
		"default":  {SystemPrompt: "default"},
		"reviewer": {SystemPrompt: "review"},
	})

	selected, prompt, err := resolveAgent(registry, "default", "@reviewer check this")
	if err != nil {
		require.NoError(t, err)
	}
	if selected.name != "reviewer" {
		assert.Failf(t, "assertion failed", "agent = %q, want reviewer", selected.name)
	}
	if prompt != "check this" {
		assert.Failf(t, "assertion failed", "prompt = %q, want check this", prompt)
	}
}

func TestResolveAgent_Unknown(t *testing.T) {
	t.Parallel()
	_, _, err := resolveAgent(agent.NewRegistry(nil), "", "@missing hi")
	if err == nil {
		require.FailNow(t, "expected unknown agent error")
	}
}

func TestResolveSelection_ExportSkipsUnknownSavedAgent(t *testing.T) {
	t.Parallel()
	const removedAgent = "removed-agent"

	store := session.NewStore(t.TempDir())
	saved := session.New("gpt-test", nil)
	saved.DefaultAgent = removedAgent
	if err := store.Save(saved); err != nil {
		require.NoError(t, err)
	}

	selection, err := resolveSelection(
		cliOptions{exportRef: saved.ID},
		config.Config{},
		"",
		agent.NewRegistry(nil),
		store,
	)
	if err != nil {
		require.NoError(t, err)
	}
	if selection.sessionState.DefaultAgent != removedAgent {
		require.Failf(t, "unexpected failure", "DefaultAgent = %q, want saved agent", selection.sessionState.DefaultAgent)
	}
}

func TestResolveSelection_ShowSkipsUnknownSavedAgent(t *testing.T) {
	t.Parallel()
	const removedAgent = "removed-agent"

	store := session.NewStore(t.TempDir())
	saved := session.New("gpt-test", nil)
	saved.DefaultAgent = removedAgent
	if err := store.Save(saved); err != nil {
		require.NoError(t, err)
	}

	selection, err := resolveSelection(
		cliOptions{showSessionRef: saved.ID},
		config.Config{},
		"",
		agent.NewRegistry(nil),
		store,
	)
	if err != nil {
		require.NoError(t, err)
	}
	if selection.sessionState.DefaultAgent != removedAgent {
		require.Failf(t, "unexpected failure", "DefaultAgent = %q, want saved agent", selection.sessionState.DefaultAgent)
	}
}

func TestResolveSelection_UsesPersistedModelBeforeConfigDefault(t *testing.T) {
	t.Parallel()
	selection, err := resolveSelection(
		cliOptions{},
		config.Config{DefaultModel: "config-model"},
		testCodexModel,
		agent.NewRegistry(nil),
		session.NewStore(t.TempDir()),
	)
	if err != nil {
		require.NoError(t, err)
	}
	if selection.selectedModel != testCodexModel {
		require.Failf(t, "unexpected failure", "selectedModel = %q", selection.selectedModel)
	}
}

func TestResolveSelection_LoadedSessionWinsOverPersistedModel(t *testing.T) {
	t.Parallel()
	store := session.NewStore(t.TempDir())
	saved := session.New("session-model", nil)
	if err := store.Save(saved); err != nil {
		require.NoError(t, err)
	}

	selection, err := resolveSelection(
		cliOptions{sessionRef: saved.ID},
		config.Config{DefaultModel: "config-model"},
		"persisted-model",
		agent.NewRegistry(nil),
		store,
	)
	if err != nil {
		require.NoError(t, err)
	}
	if selection.selectedModel != "session-model" {
		require.Failf(t, "unexpected failure", "selectedModel = %q", selection.selectedModel)
	}
}

func TestVersionString(t *testing.T) {
	t.Parallel()
	got := versionString()
	for _, want := range []string{"atteler", "commit", "built"} {
		if !strings.Contains(got, want) {
			require.Failf(t, "unexpected failure", "version string %q missing %q", got, want)
		}
	}
}

func TestFormatSessionSummary(t *testing.T) {
	t.Parallel()
	summary := session.Summary{
		ID:           "abc",
		Path:         "/tmp/abc.json",
		UpdatedAt:    time.Date(2026, 4, 30, 12, 0, 0, 0, time.UTC),
		DefaultAgent: "reviewer",
		DefaultModel: "gpt-test",
		Messages:     3,
	}

	got := formatSessionSummary(summary)
	want := "abc\t2026-04-30T12:00:00Z\t3 messages\tagent=reviewer\tmodel=gpt-test\t/tmp/abc.json"
	if got != want {
		require.Failf(t, "unexpected failure", "summary = %q, want %q", got, want)
	}

	summary.Title = "Auth review"
	summary.Tags = []string{"auth", "review"}
	got = formatSessionSummary(summary)
	want = "abc\t2026-04-30T12:00:00Z\t3 messages\tagent=reviewer\tmodel=gpt-test\ttitle=Auth review\ttags=auth,review\t/tmp/abc.json"
	if got != want {
		require.Failf(t, "unexpected failure", "titled summary = %q, want %q", got, want)
	}
}

func TestFormatSearchSnippet(t *testing.T) {
	t.Parallel()
	snippet := session.SearchSnippet{
		Role: "assistant",
		Text: "matching excerpt",
	}

	got := formatSearchSnippet(snippet)
	want := "  assistant: matching excerpt"
	if got != want {
		require.Failf(t, "unexpected failure", "snippet = %q, want %q", got, want)
	}
}

func TestFormatTagSummary(t *testing.T) {
	t.Parallel()
	got := formatTagSummary(session.TagSummary{Tag: "auth", Sessions: 2})
	want := "auth\t2 sessions"
	if got != want {
		require.Failf(t, "unexpected failure", "tag summary = %q, want %q", got, want)
	}
}

func TestFormatSessionDetails(t *testing.T) {
	t.Parallel()
	sessionState := session.New("gpt-test", []llm.Message{{Role: llm.RoleUser, Content: "hello"}})
	sessionState.Title = "Demo"
	sessionState.Tags = []string{"demo"}

	out, err := formatSessionDetails(sessionState, "/tmp/session.json")
	if err != nil {
		require.NoError(t, err)
	}
	for _, want := range []string{
		"id: " + sessionState.ID,
		"path: /tmp/session.json",
		"title: Demo",
		"- demo",
		"role: user",
		"content: hello",
	} {
		if !strings.Contains(out, want) {
			require.Failf(t, "unexpected failure", "session details missing %q in:\n%s", want, out)
		}
	}
}

func TestInitConfigWritesTemplateWithoutOverwrite(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "nested", "config.yaml")

	if err := initConfig(path); err != nil {
		require.NoError(t, err)
	}

	data, err := os.ReadFile(path)
	if err != nil {
		require.NoError(t, err)
	}
	if string(data) != config.TemplateYAML() {
		require.Failf(t, "unexpected failure", "config template mismatch")
	}
	if err := initConfig(path); err == nil {
		require.FailNow(t, "expected existing config error")
	}
}

func TestAppendStdinContext(t *testing.T) {
	t.Parallel()
	got := appendStdinContext("Review this diff", "diff --git a/file b/file\n")
	want := "Review this diff\n\n<stdin>\ndiff --git a/file b/file\n</stdin>"
	if got != want {
		require.Failf(t, "unexpected failure", "prompt = %q, want %q", got, want)
	}

	got = appendStdinContext("", "plain input\n")
	if got != "plain input" {
		require.Failf(t, "unexpected failure", "stdin-only prompt = %q", got)
	}
}

func TestConfigPathStatus(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	if err := os.WriteFile(path, []byte("default_model: test\n"), 0o600); err != nil {
		require.NoError(t, err)
	}

	if got := configPathStatus(path); got != "present" {
		require.Failf(t, "unexpected failure", "configPathStatus(file) = %q, want present", got)
	}
	if got := configPathStatus(dir); got != "directory" {
		require.Failf(t, "unexpected failure", "configPathStatus(dir) = %q, want directory", got)
	}
	if got := configPathStatus(filepath.Join(dir, "missing.yaml")); got != "missing" {
		require.Failf(t, "unexpected failure", "configPathStatus(missing) = %q, want missing", got)
	}
}

func TestKnownProvidersSorted(t *testing.T) {
	t.Parallel()
	providers := knownProvidersSorted()
	if len(providers) < 2 {
		require.Failf(t, "unexpected failure", "providers len = %d, want at least 2", len(providers))
	}
	for i := 1; i < len(providers); i++ {
		if providers[i-1].Name > providers[i].Name {
			require.Failf(t, "unexpected failure", "providers not sorted: %+v", providers)
		}
	}
}

func TestGenerationForRequest_Precedence(t *testing.T) {
	t.Parallel()
	globalTemp := 0.7
	agentTemp := 0.2
	cliTopP := 0.9
	activeAgent := agentSelection{
		ok: true,
		agent: agent.Agent{
			Temperature: &agentTemp,
			MaxTokens:   100,
		},
	}

	generation := generationForRequest(
		generationSettings{Temperature: &globalTemp, MaxTokens: 200},
		generationSettings{TopP: &cliTopP},
		activeAgent,
	)

	if generation.Temperature == nil || *generation.Temperature != agentTemp {
		require.Failf(t, "unexpected failure", "temperature = %v, want agent override", generation.Temperature)
	}
	if generation.TopP == nil || *generation.TopP != cliTopP {
		require.Failf(t, "unexpected failure", "top_p = %v, want CLI override", generation.TopP)
	}
	if generation.MaxTokens != 100 {
		require.Failf(t, "unexpected failure", "max tokens = %d, want agent override", generation.MaxTokens)
	}
}

func TestApplyGenerationParams_AllowsExplicitZeroTemperature(t *testing.T) {
	t.Parallel()
	temperature := 0.0
	params := llm.CompleteParams{}

	applyGenerationParams(&params, generationSettings{Temperature: &temperature})

	if params.Temperature == nil || *params.Temperature != 0 {
		require.Failf(t, "unexpected failure", "temperature = %v, want explicit zero", params.Temperature)
	}
}

func TestFZFInputAndSelection(t *testing.T) {
	t.Parallel()
	items := []pickerItem{
		{provider: "claude-code", model: "claude-opus-4-6"},
		{provider: "codex", model: "gpt-5.5"},
	}

	input := fzfInput(items)
	for _, want := range []string{
		"claude-code/claude-opus-4-6\tclaude-code\tclaude-opus-4-6\n",
		"codex/gpt-5.5\tcodex\tgpt-5.5\n",
	} {
		if !strings.Contains(input, want) {
			require.Failf(t, "unexpected failure", "fzf input missing %q in:\n%s", want, input)
		}
	}

	item, ok := parseFZFSelection("codex/gpt-5.5\tcodex\tgpt-5.5\n", items)
	if !ok {
		require.FailNow(t, "expected fzf selection to parse")
	}
	if item.provider != "codex" || item.model != "gpt-5.5" {
		require.Failf(t, "unexpected failure", "selection = %+v, want codex/gpt-5.5", item)
	}

	if _, ok := parseFZFSelection("", items); ok {
		require.FailNow(t, "empty fzf selection should be canceled")
	}
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

func TestMergeTags_DeduplicatesCaseInsensitive(t *testing.T) {
	t.Parallel()
	got := mergeTags([]string{"auth"}, []string{"Auth", "review", " "})
	want := []string{"auth", "review"}
	if !reflect.DeepEqual(got, want) {
		require.Failf(t, "unexpected failure", "tags = %v, want %v", got, want)
	}
}

func TestPathStatus(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	if got := pathStatus(dir); got != "ok" {
		require.Failf(t, "unexpected failure", "pathStatus(dir) = %q, want ok", got)
	}

	missing := filepath.Join(dir, "missing")
	if got := pathStatus(missing); got != "will be created on first save" {
		require.Failf(t, "unexpected failure", "pathStatus(missing) = %q", got)
	}
}

func TestFormatAgentDescription(t *testing.T) {
	t.Parallel()
	temperature := 0.1
	out, err := formatAgentDescription(agent.Agent{
		Name:           "reviewer",
		Model:          "gpt-test",
		FallbackModels: []string{"gpt-fallback"},
		Triggers:       []string{"review this"},
		Temperature:    &temperature,
		MaxTokens:      100,
	})
	if err != nil {
		require.NoError(t, err)
	}
	for _, want := range []string{
		"name: reviewer",
		"model: gpt-test",
		"fallback_models:",
		"triggers:",
		"temperature: 0.1",
		"max_tokens: 100",
	} {
		if !strings.Contains(out, want) {
			require.Failf(t, "unexpected failure", "agent description missing %q in:\n%s", want, out)
		}
	}
}

func TestFormatTokenCount(t *testing.T) {
	t.Parallel()
	tests := []struct {
		want  string
		input int
	}{
		{"0", 0},
		{"1", 1},
		{"999", 999},
		{"1k", 1000},
		{"1.5k", 1500},
		{"4.1k", 4096},
		{"128k", 128_000},
		{"200k", 200_000},
		{"1.0M", 1_000_000},
		{"1.0M", 1_047_576},
		{"2.5M", 2_500_000},
	}
	for _, tt := range tests {
		got := formatTokenCount(tt.input)
		if got != tt.want {
			assert.Failf(t, "assertion failed", "formatTokenCount(%d) = %q, want %q", tt.input, got, tt.want)
		}
	}
}
