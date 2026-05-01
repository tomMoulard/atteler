package main

import (
	"context"
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
	attasync "github.com/tommoulard/atteler/pkg/async"
	"github.com/tommoulard/atteler/pkg/codegraph"
	"github.com/tommoulard/atteler/pkg/codeintel"
	"github.com/tommoulard/atteler/pkg/config"
	"github.com/tommoulard/atteler/pkg/contextpack"
	"github.com/tommoulard/atteler/pkg/contextref"
	atteval "github.com/tommoulard/atteler/pkg/eval"
	"github.com/tommoulard/atteler/pkg/feedback"
	"github.com/tommoulard/atteler/pkg/githistory"
	"github.com/tommoulard/atteler/pkg/llm"
	"github.com/tommoulard/atteler/pkg/mcp"
	"github.com/tommoulard/atteler/pkg/memory"
	"github.com/tommoulard/atteler/pkg/modelroute"
	attelerplugin "github.com/tommoulard/atteler/pkg/plugin"
	"github.com/tommoulard/atteler/pkg/promptcomplete"
	"github.com/tommoulard/atteler/pkg/review"
	"github.com/tommoulard/atteler/pkg/session"
	attskill "github.com/tommoulard/atteler/pkg/skill"
	"github.com/tommoulard/atteler/pkg/speculate"
	"github.com/tommoulard/atteler/pkg/vector"
	"github.com/tommoulard/atteler/pkg/watch"
)

const (
	testCodexModel   = "codex/gpt-5.5"
	testReviewerName = "reviewer"
)

func TestResolveAgent_InlineOverridesSelected(t *testing.T) {
	t.Parallel()
	registry := agent.NewRegistry(map[string]config.AgentConfig{
		"default":        {SystemPrompt: "default"},
		testReviewerName: {SystemPrompt: "review"},
	})

	selected, prompt, err := resolveAgent(registry, "default", "@reviewer check this")
	if err != nil {
		require.NoError(t, err)
	}
	if selected.name != testReviewerName {
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
	sessionState.RecordNegativeKnowledge("try cache bust", "broke auth", "abc123", "reviewer")
	sessionState.RecordEvaluation("reviewer", "pass", "caught auth regression", "eval.md", 5)
	sessionState.RecordArtifact("docs/research.md", "research", "auth notes", "reviewer")

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
		"negative_knowledge:",
		"approach: try cache bust",
		"evaluations:",
		"outcome: pass",
		"artifacts:",
		"path: docs/research.md",
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
	agentSeed := 11
	cliSeed := 22
	activeAgent := agentSelection{
		ok: true,
		agent: agent.Agent{
			Temperature:    &agentTemp,
			Seed:           &agentSeed,
			ReasoningLevel: "high",
			MaxTokens:      100,
		},
	}

	generation := generationForRequest(
		generationSettings{Temperature: &globalTemp, ReasoningLevel: "medium", MaxTokens: 200},
		generationSettings{TopP: &cliTopP, Seed: &cliSeed},
		activeAgent,
	)

	if generation.Temperature == nil || *generation.Temperature != agentTemp {
		require.Failf(t, "unexpected failure", "temperature = %v, want agent override", generation.Temperature)
	}
	if generation.TopP == nil || *generation.TopP != cliTopP {
		require.Failf(t, "unexpected failure", "top_p = %v, want CLI override", generation.TopP)
	}
	if generation.Seed == nil || *generation.Seed != cliSeed {
		require.Failf(t, "unexpected failure", "seed = %v, want CLI override", generation.Seed)
	}
	if generation.ReasoningLevel != "high" {
		require.Failf(t, "unexpected failure", "reasoning level = %q, want agent override", generation.ReasoningLevel)
	}
	if generation.MaxTokens != 100 {
		require.Failf(t, "unexpected failure", "max tokens = %d, want agent override", generation.MaxTokens)
	}
}

func TestApplyGenerationParams_AllowsExplicitZeroTemperature(t *testing.T) {
	t.Parallel()
	temperature := 0.0
	seed := 0
	params := llm.CompleteParams{}

	applyGenerationParams(&params, generationSettings{Temperature: &temperature, Seed: &seed, ReasoningLevel: "low"})

	if params.Temperature == nil || *params.Temperature != 0 {
		require.Failf(t, "unexpected failure", "temperature = %v, want explicit zero", params.Temperature)
	}
	if params.Seed == nil || *params.Seed != 0 {
		require.Failf(t, "unexpected failure", "seed = %v, want explicit zero", params.Seed)
	}
}

func TestValidateRequestBudget_MaxInputTokens(t *testing.T) {
	t.Parallel()
	err := validateRequestBudget(nil, "", []llm.Message{{Role: llm.RoleUser, Content: strings.Repeat("x", 80)}}, 10)
	if err == nil {
		require.FailNow(t, "expected budget error")
	}
	if got := err.Error(); !strings.Contains(got, "max_input_tokens") {
		require.Failf(t, "unexpected error", "error = %q", got)
	}
}

func TestRecordedResponseRoundTrip(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "response.json")
	temperature := 0.0
	seed := 12
	params := llm.CompleteParams{
		Model:       "gpt-test",
		Temperature: &temperature,
		Seed:        &seed,
		Messages:    []llm.Message{{Role: llm.RoleUser, Content: "hello"}},
	}
	resp := &llm.Response{Content: "hi back", Model: "gpt-test", InputTokens: 2, OutputTokens: 3}

	if err := saveRecordedResponse(path, params, []string{"backup"}, resp); err != nil {
		require.NoError(t, err)
	}
	got, err := loadRecordedResponse(path)
	if err != nil {
		require.NoError(t, err)
	}
	if got.Content != "hi back" || got.Model != "gpt-test" || got.InputTokens != 2 || got.OutputTokens != 3 {
		require.Failf(t, "unexpected replay response", "got = %+v", got)
	}
}

func TestFormatAgentPlanParticipant(t *testing.T) {
	t.Parallel()
	got := formatAgentPlanParticipant(&agent.Participant{
		Agent: agent.Agent{
			Name:         "reviewer",
			Model:        "gpt-test",
			Capabilities: []string{"review", "security"},
		},
		Source:  agent.ParticipantSourceCapability,
		Pattern: "review",
	})
	want := "reviewer\tsource=capability\tmatch=review\tcapabilities=review,security\tmodel=gpt-test"
	assert.Equal(t, want, got)
}

func TestEvalOutput_PassAndFail(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	actual := filepath.Join(dir, "actual.txt")
	if err := os.WriteFile(actual, []byte("hello brave world\n"), 0o600); err != nil {
		require.NoError(t, err)
	}

	require.NoError(t, evalOutput(actual, "brave world", "", atteval.ModeContains))
	require.Error(t, evalOutput(actual, "missing", "", atteval.ModeContains))
}

func TestExpectedEvalText_RejectsAmbiguousInput(t *testing.T) {
	t.Parallel()
	_, err := expectedEvalText("inline", "file.txt")
	require.Error(t, err)
}

func TestFormatSkillSuggestion(t *testing.T) {
	t.Parallel()
	got := formatSkillSuggestion(attskill.Suggestion{
		Name:        "Plan Code Test Skill",
		Slug:        "plan-code-test",
		Steps:       []string{"plan", "code", "test"},
		Occurrences: 2,
		Rationale:   "Observed repeated workflow.",
	})
	for _, want := range []string{
		"name: Plan Code Test Skill",
		"slug: plan-code-test",
		"occurrences: 2",
		"  - plan",
		"rationale: Observed repeated workflow.",
	} {
		assert.Contains(t, got, want)
	}
}

func TestParsePluginTarget(t *testing.T) {
	t.Parallel()
	pluginName, entrypoint, err := parsePluginTarget("reviewer/check", "")
	require.NoError(t, err)
	assert.Equal(t, "reviewer", pluginName)
	assert.Equal(t, "check", entrypoint)

	pluginName, entrypoint, err = parsePluginTarget("reviewer", "run")
	require.NoError(t, err)
	assert.Equal(t, "reviewer", pluginName)
	assert.Equal(t, "run", entrypoint)

	_, _, err = parsePluginTarget("reviewer", "")
	require.Error(t, err)
}

func TestFormatPluginDryRun(t *testing.T) {
	t.Parallel()
	got := formatPluginDryRun(attelerplugin.DryRun{
		Description: "would run plugin",
		Entrypoint: attelerplugin.Entrypoint{
			PluginName:     "reviewer",
			EntrypointName: "run",
			Path:           "/tmp/plugin/bin/run",
			Root:           "/tmp/plugin",
		},
	})
	for _, want := range []string{
		"would run plugin",
		"plugin=reviewer",
		"entrypoint=run",
		"path=/tmp/plugin/bin/run",
		"cwd=/tmp/plugin",
	} {
		assert.Contains(t, got, want)
	}
}

func TestFormatMemoryResult(t *testing.T) {
	t.Parallel()
	got := formatMemoryResult(memory.Result{
		Score:   1.25,
		Matches: []string{"oauth", "token"},
		Snippet: "Content: OAuth token refresh",
		Document: memory.Document{
			ID:   "session/demo/message/0",
			Path: "demo",
			Metadata: map[string]string{
				"kind": "message",
			},
		},
	})
	for _, want := range []string{
		"session/demo/message/0",
		"score=1.2500",
		"path=demo",
		"matches=oauth,token",
		"kind=message",
		"Content: OAuth token refresh",
	} {
		assert.Contains(t, got, want)
	}
}

func TestBuildMemoryStore_IndexesSessionsAndFiles(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	filePath := filepath.Join(dir, "notes.txt")
	require.NoError(t, os.WriteFile(filePath, []byte("OAuth file notes"), 0o600))

	store := session.NewStore(filepath.Join(dir, "sessions"))
	sessionState := session.New("gpt-test", []llm.Message{{Role: llm.RoleUser, Content: "OAuth session notes"}})
	sessionState.ID = "demo"
	require.NoError(t, store.Save(sessionState))

	mem, err := buildMemoryStore(store, cliOptions{memoryIndexFiles: stringListFlag{filePath}})
	require.NoError(t, err)
	results, err := mem.Search("oauth", 10)
	require.NoError(t, err)
	require.Len(t, results, 2)
}

func TestFormatMessageSummary(t *testing.T) {
	t.Parallel()

	message := llm.Message{Role: llm.RoleAssistant, Content: "hello\nworld " + strings.Repeat("x", 140)}
	got := formatMessageSummary(2, message)
	for _, want := range []string{
		"index=2",
		"role=assistant",
		"chars=152",
		"preview=hello world ",
	} {
		if !strings.Contains(got, want) {
			require.Failf(t, "formatted message summary missing content", "missing %q in %q", want, got)
		}
	}
	if !strings.HasSuffix(got, "…") {
		require.Failf(t, "formatted message summary should truncate", "got %q", got)
	}
}

func TestTruncateRunes(t *testing.T) {
	t.Parallel()

	if got := truncateRunes("abcd", 3); got != "ab…" {
		require.Failf(t, "unexpected truncated string", "got %q", got)
	}
	if got := truncateRunes("éclair", 20); got != "éclair" {
		require.Failf(t, "unexpected untruncated string", "got %q", got)
	}
}

func TestFormatSessionDetailsSummary(t *testing.T) {
	t.Parallel()

	sessionState := session.Session{
		ID:           "demo",
		Title:        "Auth refresh",
		DefaultAgent: "reviewer",
		DefaultModel: "gpt-test",
		CreatedAt:    time.Date(2026, 5, 1, 13, 15, 0, 0, time.UTC),
		UpdatedAt:    time.Date(2026, 5, 1, 13, 30, 0, 0, time.UTC),
		Tags:         []string{"auth", "regression"},
		Messages:     []llm.Message{{Role: llm.RoleUser, Content: "hello"}},
		NegativeKnowledge: []session.NegativeKnowledge{
			{Approach: "timer", Reason: "storm"},
		},
		Evaluations: []session.AgentEvaluation{{Agent: "reviewer", Outcome: "pass"}},
		Artifacts:   []session.Artifact{{Path: "plan.md", Kind: "plan"}},
	}
	got := formatSessionDetailsSummary(sessionState, "/tmp/demo.json")
	for _, want := range []string{
		"id=demo",
		"path=/tmp/demo.json",
		"messages=1",
		"failures=1",
		"evaluations=1",
		"artifacts=1",
		"created_at=2026-05-01T13:15:00Z",
		"updated_at=2026-05-01T13:30:00Z",
		"title=Auth refresh",
		"agent=reviewer",
		"model=gpt-test",
		"tags=auth,regression",
	} {
		if !strings.Contains(got, want) {
			require.Failf(t, "formatted session details summary missing content", "missing %q in %q", want, got)
		}
	}
}

func TestFormatFailure(t *testing.T) {
	t.Parallel()

	failure := session.NegativeKnowledge{
		Approach:  "retry timer",
		Reason:    "created retry storms",
		Commit:    "abc123",
		Agent:     "debugger",
		CreatedAt: time.Date(2026, 5, 1, 13, 0, 0, 0, time.UTC),
	}
	got := formatFailure(failure)
	for _, want := range []string{
		"approach=retry timer",
		"reason=created retry storms",
		"created_at=2026-05-01T13:00:00Z",
		"agent=debugger",
		"commit=abc123",
	} {
		if !strings.Contains(got, want) {
			require.Failf(t, "formatted failure missing content", "missing %q in %q", want, got)
		}
	}
}

func TestFormatEvaluation(t *testing.T) {
	t.Parallel()

	evaluation := session.AgentEvaluation{
		Agent:     "reviewer",
		Outcome:   "pass",
		Notes:     "caught regression",
		Reference: "eval.md",
		Score:     9,
		CreatedAt: time.Date(2026, 5, 1, 12, 45, 0, 0, time.UTC),
	}
	got := formatEvaluation(evaluation)
	for _, want := range []string{
		"agent=reviewer",
		"outcome=pass",
		"created_at=2026-05-01T12:45:00Z",
		"score=9",
		"reference=eval.md",
		"notes=caught regression",
	} {
		if !strings.Contains(got, want) {
			require.Failf(t, "formatted evaluation missing content", "missing %q in %q", want, got)
		}
	}
}

func TestFormatArtifact(t *testing.T) {
	t.Parallel()

	artifact := session.Artifact{
		Path:        "docs/research.md",
		Kind:        "research",
		Summary:     "useful plan",
		SourceAgent: "reviewer",
		CreatedAt:   time.Date(2026, 5, 1, 12, 30, 0, 0, time.UTC),
	}
	got := formatArtifact(artifact)
	for _, want := range []string{
		"path=docs/research.md",
		"kind=research",
		"created_at=2026-05-01T12:30:00Z",
		"agent=reviewer",
		"summary=useful plan",
	} {
		if !strings.Contains(got, want) {
			require.Failf(t, "formatted artifact missing content", "missing %q in %q", want, got)
		}
	}
}

func TestRecordEvaluationAndArtifactCommands(t *testing.T) {
	t.Parallel()
	store := session.NewStore(t.TempDir())
	sessionState := session.New("gpt-test", nil)
	if err := store.Save(sessionState); err != nil {
		require.NoError(t, err)
	}

	err := recordEvaluation(store, sessionState, "reviewer", "pass", "solid", "eval.md", 9)
	if err != nil {
		require.NoError(t, err)
	}
	loaded, err := store.Load(sessionState.ID)
	if err != nil {
		require.NoError(t, err)
	}
	require.Len(t, loaded.Evaluations, 1)
	assert.Equal(t, "reviewer", loaded.Evaluations[0].Agent)
	assert.Equal(t, 9, loaded.Evaluations[0].Score)

	err = recordArtifact(store, loaded, "docs/research.md", "research", "useful", "reviewer")
	if err != nil {
		require.NoError(t, err)
	}
	loaded, err = store.Load(sessionState.ID)
	if err != nil {
		require.NoError(t, err)
	}
	require.Len(t, loaded.Artifacts, 1)
	assert.Equal(t, "docs/research.md", loaded.Artifacts[0].Path)
	assert.Equal(t, "reviewer", loaded.Artifacts[0].SourceAgent)
}

func TestRunOnce_ReplaysResponseWithoutProvider(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	replayPath := filepath.Join(dir, "response.json")
	if err := saveRecordedResponse(
		replayPath,
		llm.CompleteParams{Model: "gpt-test", Messages: []llm.Message{{Role: llm.RoleUser, Content: "hello"}}},
		nil,
		&llm.Response{Content: "recorded answer", Model: "gpt-test"},
	); err != nil {
		require.NoError(t, err)
	}
	store := session.NewStore(filepath.Join(dir, "sessions"))

	err := runOnce(
		context.Background(),
		llm.NewRegistry(),
		agent.NewRegistry(nil),
		nil,
		store,
		session.New("gpt-test", nil),
		contextref.Options{Root: dir},
		"gpt-test",
		"",
		nil,
		generationSettings{},
		generationSettings{},
		0,
		responseRecordOptions{ReplayPath: replayPath},
		true,
		"hello",
	)
	if err != nil {
		require.NoError(t, err)
	}
	summaries, err := store.List()
	if err != nil {
		require.NoError(t, err)
	}
	if len(summaries) != 1 {
		require.Failf(t, "unexpected sessions", "summaries = %+v", summaries)
	}
	loaded, err := store.Load(summaries[0].ID)
	if err != nil {
		require.NoError(t, err)
	}
	if len(loaded.Messages) != 2 || loaded.Messages[1].Content != "recorded answer" {
		require.Failf(t, "unexpected replayed session", "messages = %+v", loaded.Messages)
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

func TestPromptComplete_AgentCandidatesAndFormatting(t *testing.T) {
	t.Parallel()

	registry := agent.NewRegistry(map[string]config.AgentConfig{
		testReviewerName: {
			Description:  "reviews code",
			Capabilities: []string{"review", "tests"},
		},
	})

	suggestions := promptcomplete.SuggestAll(promptcomplete.Context{
		Input:  "ask rev",
		Cursor: len("ask rev"),
		Agents: promptAgentCandidates(registry),
	}, promptcomplete.Options{})
	if len(suggestions) == 0 {
		require.FailNow(t, "expected prompt completion suggestion")
	}
	if suggestions[0].Text != testReviewerName {
		require.Failf(t, "unexpected suggestion", "got %+v", suggestions[0])
	}

	formatted := formatPromptSuggestions(suggestions[:1])
	for _, want := range []string{
		"text: " + testReviewerName + "\n",
		"suffix: iewer\n",
		"kind: agent\n",
		"replace: 4:7\n",
	} {
		if !strings.Contains(formatted, want) {
			require.Failf(t, "formatted suggestion missing content", "missing %q in:\n%s", want, formatted)
		}
	}
}

func TestFormatGitHistoryResult(t *testing.T) {
	t.Parallel()

	result := githistory.Result{
		Commit: githistory.Commit{
			Hash:       "1234567890abcdef",
			Date:       time.Date(2026, 5, 1, 12, 0, 0, 0, time.UTC),
			AuthorName: "Ada",
			Subject:    "Add local git history search",
		},
		Score:    120,
		Snippets: []githistory.Snippet{{Field: "files", Text: "pkg/githistory/githistory.go"}},
	}

	got := formatGitHistoryResult(result)
	for _, want := range []string{
		"1234567890ab",
		"score=120",
		"date=2026-05-01T12:00:00Z",
		"author=Ada",
		"subject=Add local git history search",
		"files=pkg/githistory/githistory.go",
	} {
		if !strings.Contains(got, want) {
			require.Failf(t, "formatted git history missing content", "missing %q in %q", want, got)
		}
	}
}

func TestFormatCodeSymbol(t *testing.T) {
	t.Parallel()

	root := filepath.Join("tmp", "repo")
	got := formatCodeSymbol(root, codeintel.Symbol{
		Name: "Run",
		Kind: "method",
		File: filepath.Join(root, "pkg", "runner.go"),
		Line: 42,
	})
	want := strings.Join([]string{"Run", "kind=method", "path=pkg/runner.go", "line=42"}, "\t")
	if got != want {
		require.Failf(t, "unexpected code symbol format", "got %q, want %q", got, want)
	}
}

func TestFormatCodeImportEdge(t *testing.T) {
	t.Parallel()

	root := filepath.Join("tmp", "repo")
	got := formatCodeImportEdge(root, codeintel.ImportEdge{
		From:   filepath.Join(root, "pkg", "runner.go"),
		Import: "context",
	})
	want := "path=pkg/runner.go\timport=context"
	if got != want {
		require.Failf(t, "unexpected code import format", "got %q, want %q", got, want)
	}
}

func TestRelativeCodePath(t *testing.T) {
	t.Parallel()

	root := filepath.Join("tmp", "repo")
	got := relativeCodePath(root, filepath.Join(root, "cmd", "atteler", "main.go"))
	if got != "cmd/atteler/main.go" {
		require.Failf(t, "unexpected relative code path", "got %q", got)
	}
}

func TestSummarizeAndFormatCodePackageFiles(t *testing.T) {
	t.Parallel()

	root := filepath.Join("tmp", "repo")
	idx := codeintel.Index{Files: []codeintel.File{
		{Path: filepath.Join(root, "pkg", "llm", "a.go"), Package: "llm", Symbols: []codeintel.Symbol{{Name: "A"}}, Imports: []string{"context", "fmt"}},
		{Path: filepath.Join(root, "pkg", "main", "main.go"), Package: "main", Symbols: []codeintel.Symbol{{Name: "Main"}}},
		{Path: filepath.Join(root, "pkg", "llm", "b.go"), Package: "llm", Symbols: []codeintel.Symbol{{Name: "B"}, {Name: "C"}}, Imports: []string{"errors"}},
	}}
	files := summarizeCodePackageFiles(root, idx, "llm")
	wantFiles := []codePackageFile{
		{Path: "pkg/llm/a.go", Package: "llm", Symbols: 1, Imports: 2},
		{Path: "pkg/llm/b.go", Package: "llm", Symbols: 2, Imports: 1},
	}
	if !reflect.DeepEqual(files, wantFiles) {
		require.Failf(t, "unexpected package files", "got %#v, want %#v", files, wantFiles)
	}
	got := formatCodePackageFile(files[0])
	want := "path=pkg/llm/a.go	package=llm	symbols=1	imports=2"
	if got != want {
		require.Failf(t, "unexpected package file format", "got %q, want %q", got, want)
	}
}

func TestSummarizeAndFormatCodePackages(t *testing.T) {
	t.Parallel()

	packages := summarizeCodePackages(codeintel.Index{Files: []codeintel.File{
		{Package: "main", Symbols: []codeintel.Symbol{{Name: "Run"}, {Name: "Stop"}}},
		{Package: "llm", Symbols: []codeintel.Symbol{{Name: "Client"}}},
		{Package: "main", Symbols: []codeintel.Symbol{{Name: "Config"}}},
		{Package: ""},
	}})
	wantPackages := []codePackageSummary{
		{Name: "llm", Files: 1, Symbols: 1},
		{Name: "main", Files: 2, Symbols: 3},
	}
	if !reflect.DeepEqual(packages, wantPackages) {
		require.Failf(t, "unexpected package summaries", "got %#v, want %#v", packages, wantPackages)
	}
	got := formatCodePackageSummary(packages[1])
	want := "package=main	files=2	symbols=3"
	if got != want {
		require.Failf(t, "unexpected package summary format", "got %q, want %q", got, want)
	}
}

func TestFormatCodeSummary(t *testing.T) {
	t.Parallel()

	got := formatCodeSummary(codeSummary{
		Files:    3,
		Packages: 2,
		Symbols:  7,
		Imports:  5,
		Nodes:    6,
		Edges:    5,
		Cycles:   1,
		Layers:   4,
	})
	want := "files=3	packages=2	symbols=7	imports=5	nodes=6	edges=5	cycles=1	layers=4"
	if got != want {
		require.Failf(t, "unexpected code summary format", "got %q, want %q", got, want)
	}
}

func TestCountPackages(t *testing.T) {
	t.Parallel()

	got := countPackages([]codeintel.File{{Package: "main"}, {Package: "main"}, {Package: "llm"}, {Package: ""}})
	if got != 2 {
		require.Failf(t, "unexpected package count", "got %d", got)
	}
}

func TestFormatCodeCycle(t *testing.T) {
	t.Parallel()

	got := formatCodeCycle(1, []codegraph.NodeID{"pkg/a", "pkg/b", "pkg/a"})
	want := "cycle=1	nodes=pkg/a -> pkg/b -> pkg/a"
	if got != want {
		require.Failf(t, "unexpected code cycle format", "got %q, want %q", got, want)
	}
}

func TestFormatCodeLayer(t *testing.T) {
	t.Parallel()

	got := formatCodeLayer(2, []codegraph.NodeID{"pkg/a", "pkg/b"})
	want := "layer=2	nodes=pkg/a,pkg/b"
	if got != want {
		require.Failf(t, "unexpected code layer format", "got %q, want %q", got, want)
	}
}

func TestImportGraphReachableAndNormalizeTarget(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	file := filepath.Join(root, "pkg", "runner.go")
	if err := os.MkdirAll(filepath.Dir(file), 0o750); err != nil {
		require.NoError(t, err)
	}
	if err := os.WriteFile(file, []byte("package runner\nimport \"context\"\n"), 0o600); err != nil {
		require.NoError(t, err)
	}

	graph, err := importGraph(root)
	if err != nil {
		require.NoError(t, err)
	}
	if got := normalizeCodeGraphTarget(root, file); got != "pkg/runner.go" {
		require.Failf(t, "unexpected normalized absolute target", "got %q", got)
	}
	if got := graph.ReachableFrom("pkg/runner.go"); !reflect.DeepEqual(got, []codegraph.NodeID{"context"}) {
		require.Failf(t, "unexpected reachable nodes", "got %#v", got)
	}
}

func TestFormatWatchFinding(t *testing.T) {
	t.Parallel()

	got := formatWatchFinding(watch.Finding{
		Path:     "pkg/example/example.go",
		Kind:     watch.KindMissingTest,
		Severity: watch.SeverityInfo,
		Message:  "missing _test.go companion",
	})
	want := strings.Join([]string{
		"path=pkg/example/example.go",
		"kind=missing_test",
		"severity=info",
		"message=missing _test.go companion",
	}, "\t")
	if got != want {
		require.Failf(t, "unexpected watch finding format", "got %q, want %q", got, want)
	}
}

func TestParseAndFormatAsyncPlan(t *testing.T) {
	t.Parallel()

	task, err := parseAsyncTaskSpec("code|coder|implement feature|plan+review")
	if err != nil {
		require.NoError(t, err)
	}
	if task.ID != "code" || task.Agent != "coder" || task.Prompt != "implement feature" {
		require.Failf(t, "unexpected parsed async task", "task = %+v", task)
	}
	if !reflect.DeepEqual(task.DependsOn, []string{"plan", "review"}) {
		require.Failf(t, "unexpected parsed dependencies", "deps = %#v", task.DependsOn)
	}

	plan, err := attasync.NewPlan([]attasync.Task{
		{ID: "plan", Agent: "planner", Prompt: "draft plan"},
		{ID: "review", Agent: "reviewer", Prompt: "review plan", DependsOn: []string{"plan"}},
		{ID: "code", Agent: "coder", Prompt: "implement feature", DependsOn: []string{"plan", "review"}},
	})
	if err != nil {
		require.NoError(t, err)
	}

	got := formatAsyncPlanBatches(plan.ReadyBatches())
	for _, want := range []string{
		"wave 1:\n",
		"id=plan\tagent=planner\tprompt=draft plan",
		"wave 2:\n",
		"id=review\tagent=reviewer\tdepends=plan\tprompt=review plan",
		"wave 3:\n",
		"id=code\tagent=coder\tdepends=plan+review\tprompt=implement feature",
	} {
		if !strings.Contains(got, want) {
			require.Failf(t, "formatted async plan missing content", "missing %q in:\n%s", want, got)
		}
	}
}

func TestFormatSpeculatePlan(t *testing.T) {
	t.Parallel()

	plan, err := speculate.NewPlan([]string{"alpha", "beta"}, []string{"tests pass"})
	if err != nil {
		require.NoError(t, err)
	}
	got := formatSpeculatePlan(plan)
	for _, want := range []string{
		"agents: alpha,beta\n",
		"rounds:\n",
		"1\tindependent proposals",
		"cross_reviews:\n",
		"alpha -> beta",
		"beta -> alpha",
		"gates:\n  - tests pass\n",
	} {
		if !strings.Contains(got, want) {
			require.Failf(t, "formatted speculate plan missing content", "missing %q in:\n%s", want, got)
		}
	}
}

func TestFormatVectorResult(t *testing.T) {
	t.Parallel()

	got := formatVectorResult(vector.Result{
		Document: vector.Document{
			ID:       "docs/research.md",
			Metadata: map[string]string{"path": "docs/research.md"},
		},
		Score: 0.75,
	})
	want := "docs/research.md\tscore=0.7500\tpath=docs/research.md"
	if got != want {
		require.Failf(t, "unexpected vector result format", "got %q, want %q", got, want)
	}
}

func TestFormatReviewReport(t *testing.T) {
	t.Parallel()

	report := review.Report{
		Reviewer: "watch-scan",
		Findings: []review.Finding{
			{
				Severity: review.SeverityInfo,
				Category: review.CategoryTests,
				Path:     "pkg/example/example.go",
				Message:  "missing _test.go companion",
			},
			{
				Severity: review.SeverityMedium,
				Category: review.CategoryMaintainability,
				Path:     "assets/blob.txt",
				Message:  "file is above threshold",
			},
		},
	}

	got := formatReviewReport(report)
	for _, want := range []string{
		"reviewer: watch-scan\n",
		"summary: critical=0 high=0 medium=1 low=0 info=1 total=2\n",
		"findings:\n",
		"severity=medium\tcategory=maintainability\tpath=assets/blob.txt\tmessage=file is above threshold",
		"severity=info\tcategory=tests\tpath=pkg/example/example.go\tmessage=missing _test.go companion",
	} {
		if !strings.Contains(got, want) {
			require.Failf(t, "formatted review report missing content", "missing %q in:\n%s", want, got)
		}
	}
}

func TestParseAndFormatRouteCandidate(t *testing.T) {
	t.Parallel()

	candidate, err := parseRouteCandidate("openai/gpt-mini,input=0.001,output=0.002,priority=2,max=1000,latency=500,ttft=100")
	if err != nil {
		require.NoError(t, err)
	}
	if candidate.Provider != "openai" || candidate.Name != "gpt-mini" {
		require.Failf(t, "unexpected route candidate id", "candidate = %+v", candidate)
	}

	got := formatRouteCandidate(candidate, modelroute.RequestProfile{
		EstimatedInputTokens:  100,
		EstimatedOutputTokens: 50,
	})
	for _, want := range []string{
		"openai/gpt-mini",
		"cost=0.200000",
		"priority=2",
		"max_input=1000",
		"latency_ms=500",
		"ttft_ms=100",
	} {
		if !strings.Contains(got, want) {
			require.Failf(t, "formatted route candidate missing content", "missing %q in %q", want, got)
		}
	}
}

func TestParseAndFormatContextPack(t *testing.T) {
	t.Parallel()

	messages := parseContextPackMessages("system: keep rules\nuser: first\nassistant: second\ncontinued\n")
	if len(messages) != 3 {
		require.Failf(t, "unexpected parsed message count", "messages = %#v", messages)
	}
	if messages[2].Content != "second\ncontinued" {
		require.Failf(t, "unexpected continuation", "content = %q", messages[2].Content)
	}

	result := contextpack.Compact(messages, 12)
	got := formatContextPackResult(result)
	for _, want := range []string{
		"compressed: true\n",
		"omitted:",
		"tokens:",
		"output:\n",
		"system: keep rules",
	} {
		if !strings.Contains(got, want) {
			require.Failf(t, "formatted context pack missing content", "missing %q in:\n%s", want, got)
		}
	}
}

func TestFormatFeedbackProposal(t *testing.T) {
	t.Parallel()

	got := formatFeedbackProposal(feedback.Proposal{
		Agent:      "reviewer",
		Confidence: 0.8,
		Action:     "Revise instructions.",
		Reason:     "Failed evaluations.",
		Evidence:   []string{"evaluation: fail; score 1"},
	})
	for _, want := range []string{
		"agent: reviewer\n",
		"confidence: 0.80\n",
		"action: Revise instructions.\n",
		"reason: Failed evaluations.\n",
		"evidence:\n",
		"  - evaluation: fail; score 1\n",
	} {
		if !strings.Contains(got, want) {
			require.Failf(t, "formatted feedback proposal missing content", "missing %q in:\n%s", want, got)
		}
	}
}

func TestFormatMCPServer(t *testing.T) {
	t.Parallel()

	got := formatMCPServer(mcp.Server{
		Name:         "repo",
		Command:      "atteler-mcp",
		Args:         []string{"--repo", "."},
		Capabilities: []string{"symbols", "memory"},
	})
	for _, want := range []string{
		"repo",
		"command=atteler-mcp",
		"args=--repo,.",
		"capabilities=memory,symbols",
	} {
		if !strings.Contains(got, want) {
			require.Failf(t, "formatted MCP server missing content", "missing %q in %q", want, got)
		}
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

func TestRecordFailure_SavesNegativeKnowledge(t *testing.T) {
	t.Parallel()
	store := session.NewStore(t.TempDir())
	sessionState := session.New("gpt-test", nil)

	if err := recordFailure(store, sessionState, "try cache bust", "broke auth", "abc123", "reviewer"); err != nil {
		require.NoError(t, err)
	}
	loaded, err := store.Load(sessionState.ID)
	if err != nil {
		require.NoError(t, err)
	}
	if len(loaded.NegativeKnowledge) != 1 {
		require.Failf(t, "unexpected negative knowledge", "entries = %+v", loaded.NegativeKnowledge)
	}
	entry := loaded.NegativeKnowledge[0]
	if entry.Approach != "try cache bust" || entry.Reason != "broke auth" || entry.Commit != "abc123" || entry.Agent != "reviewer" {
		require.Failf(t, "unexpected negative knowledge", "entry = %+v", entry)
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
	seed := 99
	out, err := formatAgentDescription(agent.Agent{
		Name:           "reviewer",
		Model:          "gpt-test",
		Description:    "Reviews code",
		Personality:    "concise",
		FallbackModels: []string{"gpt-fallback"},
		Capabilities:   []string{"review"},
		Triggers:       []string{"review this"},
		Temperature:    &temperature,
		Seed:           &seed,
		ReasoningLevel: "high",
		MaxTokens:      100,
	})
	if err != nil {
		require.NoError(t, err)
	}
	for _, want := range []string{
		"name: reviewer",
		"model: gpt-test",
		"description: Reviews code",
		"personality: concise",
		"capabilities:",
		"fallback_models:",
		"triggers:",
		"temperature: 0.1",
		"seed: 99",
		"reasoning_level: high",
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
