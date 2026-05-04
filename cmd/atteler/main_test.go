package main

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/charmbracelet/bubbles/textarea"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/tommoulard/atteler/pkg/agent"
	"github.com/tommoulard/atteler/pkg/agentmemory"
	attasync "github.com/tommoulard/atteler/pkg/async"
	"github.com/tommoulard/atteler/pkg/codegraph"
	"github.com/tommoulard/atteler/pkg/codeintel"
	"github.com/tommoulard/atteler/pkg/config"
	"github.com/tommoulard/atteler/pkg/contextpack"
	"github.com/tommoulard/atteler/pkg/contextref"
	atteval "github.com/tommoulard/atteler/pkg/eval"
	"github.com/tommoulard/atteler/pkg/events"
	"github.com/tommoulard/atteler/pkg/feedback"
	"github.com/tommoulard/atteler/pkg/githistory"
	"github.com/tommoulard/atteler/pkg/llm"
	"github.com/tommoulard/atteler/pkg/lsp"
	"github.com/tommoulard/atteler/pkg/mcp"
	"github.com/tommoulard/atteler/pkg/memory"
	"github.com/tommoulard/atteler/pkg/modelroute"
	attelerplugin "github.com/tommoulard/atteler/pkg/plugin"
	"github.com/tommoulard/atteler/pkg/promptcomplete"
	"github.com/tommoulard/atteler/pkg/review"
	"github.com/tommoulard/atteler/pkg/session"
	attskill "github.com/tommoulard/atteler/pkg/skill"
	"github.com/tommoulard/atteler/pkg/speculate"
	"github.com/tommoulard/atteler/pkg/subagent"
	"github.com/tommoulard/atteler/pkg/vector"
	"github.com/tommoulard/atteler/pkg/watch"
)

const (
	testCodexModel     = "codex/gpt-5.5"
	testReviewerName   = "reviewer"
	testReasoningXHigh = "xhigh"
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

func TestListSessionSummariesFiltersByTag(t *testing.T) {
	t.Parallel()

	store := session.NewStore(t.TempDir())
	auth := session.New("gpt-test", nil)
	auth.Title = "Auth review"
	auth.Tags = []string{"auth", "review"}
	require.NoError(t, store.Save(auth))

	docs := session.New("gpt-test", nil)
	docs.Title = "Docs"
	docs.Tags = []string{"docs"}
	require.NoError(t, store.Save(docs))

	summaries, err := listSessionSummaries(store, " AUTH ")
	require.NoError(t, err)
	require.Len(t, summaries, 1)
	assert.Equal(t, "Auth review", summaries[0].Title)
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

func TestGenerationForRequest_CLIReasoningLevelOverridesAgent(t *testing.T) {
	t.Parallel()

	generation := generationForRequest(
		generationSettings{ReasoningLevel: "medium"},
		generationSettings{ReasoningLevel: "xhigh"},
		agentSelection{ok: true, agent: agent.Agent{ReasoningLevel: "high"}},
	)

	if generation.ReasoningLevel != "xhigh" {
		require.Failf(t, "unexpected failure", "reasoning level = %q, want CLI override", generation.ReasoningLevel)
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
	resp := &llm.Response{Content: "hi back", Model: "gpt-test", InputTokens: 2, CachedInputTokens: 1, OutputTokens: 3}

	if err := saveRecordedResponse(path, params, []string{"backup"}, resp); err != nil {
		require.NoError(t, err)
	}

	got, err := loadRecordedResponse(path)
	if err != nil {
		require.NoError(t, err)
	}

	if got.Content != "hi back" || got.Model != "gpt-test" || got.InputTokens != 2 || got.CachedInputTokens != 1 || got.OutputTokens != 3 {
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

func TestFormatAgentPerformanceSummary(t *testing.T) {
	t.Parallel()

	summary := session.AgentPerformanceSummary{
		Agent:                    "reviewer",
		EvaluationCount:          2,
		NegativeKnowledgeCount:   1,
		FailureCount:             1,
		DefaultAgentSessionCount: 1,
		ScoredEvaluationCount:    2,
		AverageScore:             7.5,
		MinScore:                 6,
		MaxScore:                 9,
		Outcomes:                 []session.OutcomeCount{{Outcome: "pass", Count: 1}, {Outcome: "fail", Count: 1}},
		LatestActivity:           time.Date(2026, 5, 2, 10, 30, 0, 0, time.UTC),
	}

	got := formatAgentPerformanceSummary(summary)
	for _, want := range []string{
		"agent=reviewer",
		"evaluations=2",
		"failures=1",
		"negative_knowledge=1",
		"default_agent_sessions=1",
		"scored=2",
		"avg_score=7.50",
		"min_score=6",
		"max_score=9",
		"outcomes=pass:1,fail:1",
		"latest=2026-05-02T10:30:00Z",
	} {
		if !strings.Contains(got, want) {
			require.Failf(t, "formatted agent performance missing content", "missing %q in %q", want, got)
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
		&llm.Response{Content: "recorded answer", Model: "gpt-test", InputTokens: 2, CachedInputTokens: 1, OutputTokens: 3},
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
		{provider: "codex", model: "gpt-5.5", reasoning: testReasoningXHigh},
	}

	input := fzfInput(items)
	for _, want := range []string{
		"claude-code/claude-opus-4-6\tclaude-code\tclaude-opus-4-6\t\n",
		"codex/gpt-5.5\tcodex\tgpt-5.5\t\n",
		"codex/gpt-5.5:xhigh\tcodex\tgpt-5.5\txhigh\n",
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
		for _, level := range levels {
			out = append(out, pickerItem{provider: base.provider, model: base.model, reasoning: level})
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
	var b strings.Builder

	for i := 0; i < len(value); i++ {
		if value[i] != '\x1b' {
			b.WriteByte(value[i])
			continue
		}

		i++
		if i >= len(value) || value[i] != '[' {
			continue
		}

		for i < len(value) && (value[i] < '@' || value[i] > '~') {
			i++
		}
	}

	return b.String()
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

func TestWriteRunOnceResult_JSONAndHeadlessText(t *testing.T) {
	t.Parallel()

	result := runOnceResult{
		SessionID:   "session-id",
		SessionPath: "/tmp/session.json",
		HeadlessID:  "headless-id",
		Model:       "gpt-test",
		Content:     "answer",
		TokenUsage:  tokenUsage{InputTokens: 1, CachedInputTokens: 2, OutputTokens: 3, Responses: 1},
	}

	var (
		stdout bytes.Buffer
		stderr bytes.Buffer
	)

	require.NoError(t, writeRunOnceResult(&stdout, &stderr, result, "json", true))

	var decoded runOnceResult
	require.NoError(t, json.Unmarshal(stdout.Bytes(), &decoded))
	assert.Equal(t, result.SessionID, decoded.SessionID)
	assert.Equal(t, result.HeadlessID, decoded.HeadlessID)
	assert.Equal(t, result.TokenUsage.OutputTokens, decoded.TokenUsage.OutputTokens)
	assert.Empty(t, stderr.String())

	stdout.Reset()
	stderr.Reset()
	require.NoError(t, writeRunOnceResult(&stdout, &stderr, result, "text", true))
	assert.Empty(t, stdout.String())
	assert.Empty(t, stderr.String())
}

func TestRunOnceWithOptions_HeadlessReplayCreatesMetadata(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	replayPath := filepath.Join(dir, "response.json")
	require.NoError(t, saveRecordedResponse(
		replayPath,
		llm.CompleteParams{Model: "gpt-test", Messages: []llm.Message{{Role: llm.RoleUser, Content: "hello"}}},
		nil,
		&llm.Response{Content: "recorded answer", Model: "gpt-test", InputTokens: 2, CachedInputTokens: 1, OutputTokens: 3},
	))

	store := session.NewStore(filepath.Join(dir, "sessions"))
	headlessID := "test-headless"

	err := runOnceWithOptions(
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
		runOnceExecutionOptions{
			OutputFormat: outputFormatText,
			HeadlessID:   headlessID,
			Response:     responseRecordOptions{ReplayPath: replayPath},
			Headless:     true,
		},
		true,
		"hello",
	)
	require.NoError(t, err)

	run, err := store.LoadHeadlessRun(headlessID)
	require.NoError(t, err)
	assert.Equal(t, session.HeadlessStatusCompleted, run.Status)
	assert.Equal(t, "gpt-test", run.Model)
	assert.NotNil(t, run.CompletedAt)

	log, err := store.ReadHeadlessLog(headlessID)
	require.NoError(t, err)
	assert.Contains(t, log, "started")
	assert.Contains(t, log, "assistant_message")
	assert.Contains(t, log, "completed")

	runs, err := store.ListHeadlessRuns()
	require.NoError(t, err)
	require.Len(t, runs, 1)
	assert.Equal(t, headlessID, runs[0].ID)
	require.NoError(t, streamHeadlessLog(context.Background(), store, headlessID))
}

func TestFormatHeadlessRun(t *testing.T) {
	t.Parallel()

	run := session.HeadlessRun{
		ID:        "headless-id",
		SessionID: "session-id",
		LogPath:   "/tmp/headless.log",
		Model:     "gpt-test",
		Status:    session.HeadlessStatusRunning,
		StartedAt: time.Date(2026, 5, 3, 10, 0, 0, 0, time.UTC),
		UpdatedAt: time.Date(2026, 5, 3, 10, 1, 0, 0, time.UTC),
	}

	got := formatHeadlessRun(run)
	for _, want := range []string{
		"headless-id",
		"status=running",
		"session=session-id",
		"model=gpt-test",
		"started=2026-05-03T10:00:00Z",
		"updated=2026-05-03T10:01:00Z",
		"log=/tmp/headless.log",
	} {
		assert.Contains(t, got, want)
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

func TestSummarizeAndFormatCodeSymbolFiles(t *testing.T) {
	t.Parallel()

	root := filepath.Join("tmp", "repo")
	idx := codeintel.Index{Files: []codeintel.File{
		{Path: filepath.Join(root, "pkg", "b.go"), Package: "pkg", Symbols: []codeintel.Symbol{{Name: "B"}}},
		{Path: filepath.Join(root, "cmd", "a.go"), Package: "main", Symbols: []codeintel.Symbol{{Name: "A"}, {Name: "Run"}}},
		{Path: filepath.Join(root, "pkg", "empty.go"), Package: "pkg"},
		{Path: filepath.Join(root, "pkg", "c.go"), Package: "pkg", Symbols: []codeintel.Symbol{{Name: "C"}, {Name: "Build"}}},
	}}
	summaries := summarizeCodeSymbolFiles(root, idx)

	want := []codeSymbolFileSummary{
		{Path: "cmd/a.go", Package: "main", Symbols: 2},
		{Path: "pkg/c.go", Package: "pkg", Symbols: 2},
		{Path: "pkg/b.go", Package: "pkg", Symbols: 1},
	}
	if !reflect.DeepEqual(summaries, want) {
		require.Failf(t, "unexpected symbol file summaries", "got %#v, want %#v", summaries, want)
	}

	got := formatCodeSymbolFileSummary(summaries[0])
	if got != "path=cmd/a.go	package=main	symbols=2" {
		require.Failf(t, "unexpected symbol file summary format", "got %q", got)
	}
}

func TestSummarizeAndFormatCodeSymbols(t *testing.T) {
	t.Parallel()

	idx := codeintel.Index{Symbols: []codeintel.Symbol{
		{Kind: "func"},
		{Kind: "type"},
		{Kind: "func"},
		{Kind: "const"},
		{Kind: ""},
	}}
	summaries := summarizeCodeSymbols(idx)

	want := []codeSymbolSummary{{Kind: "func", Count: 2}, {Kind: "const", Count: 1}, {Kind: "type", Count: 1}}
	if !reflect.DeepEqual(summaries, want) {
		require.Failf(t, "unexpected symbol summaries", "got %#v, want %#v", summaries, want)
	}

	got := formatCodeSymbolSummary(summaries[0])
	if got != "kind=func	symbols=2" {
		require.Failf(t, "unexpected symbol summary format", "got %q", got)
	}
}

func TestSummarizeCodeSymbolKindFiles(t *testing.T) {
	t.Parallel()

	root := filepath.Join("tmp", "repo")
	idx := codeintel.Index{Files: []codeintel.File{
		{Path: filepath.Join(root, "pkg", "b.go"), Package: "pkg", Symbols: []codeintel.Symbol{{Kind: "func"}, {Kind: "type"}}},
		{Path: filepath.Join(root, "cmd", "a.go"), Package: "main", Symbols: []codeintel.Symbol{{Kind: "func"}, {Kind: "FUNC"}}},
		{Path: filepath.Join(root, "pkg", "c.go"), Package: "pkg", Symbols: []codeintel.Symbol{{Kind: "const"}}},
	}}
	summaries := summarizeCodeSymbolKindFiles(root, idx, "func")

	want := []codeSymbolFileSummary{
		{Path: "cmd/a.go", Package: "main", Symbols: 2},
		{Path: "pkg/b.go", Package: "pkg", Symbols: 1},
	}
	if !reflect.DeepEqual(summaries, want) {
		require.Failf(t, "unexpected symbol kind file summaries", "got %#v, want %#v", summaries, want)
	}

	if got := summarizeCodeSymbolKindFiles(root, idx, "missing"); len(got) != 0 {
		require.Failf(t, "missing kind should return empty", "got %#v", got)
	}

	if got := summarizeCodeSymbolKindFiles(root, idx, " "); got != nil {
		require.Failf(t, "blank kind should return nil", "got %#v", got)
	}
}

func TestSummarizeCodeSymbolKindPackages(t *testing.T) {
	t.Parallel()

	idx := codeintel.Index{Files: []codeintel.File{
		{Path: "pkg/b.go", Package: "pkg", Symbols: []codeintel.Symbol{{Kind: "func"}, {Kind: "type"}}},
		{Path: "cmd/a.go", Package: "main", Symbols: []codeintel.Symbol{{Kind: "func"}, {Kind: "FUNC"}}},
		{Path: "pkg/c.go", Package: "pkg", Symbols: []codeintel.Symbol{{Kind: "func"}, {Kind: "FUNC"}, {Kind: "const"}}},
		{Path: "empty.go", Symbols: []codeintel.Symbol{{Kind: "func"}}},
	}}
	summaries := summarizeCodeSymbolKindPackages(idx, "func")

	want := []codePackageSummary{
		{Name: "pkg", Files: 2, Symbols: 3},
		{Name: "main", Files: 1, Symbols: 2},
	}
	if !reflect.DeepEqual(summaries, want) {
		require.Failf(t, "unexpected symbol kind package summaries", "got %#v, want %#v", summaries, want)
	}

	if got := summarizeCodeSymbolKindPackages(idx, "missing"); len(got) != 0 {
		require.Failf(t, "missing kind should return empty", "got %#v", got)
	}

	if got := summarizeCodeSymbolKindPackages(idx, " "); got != nil {
		require.Failf(t, "blank kind should return nil", "got %#v", got)
	}
}

func TestCodeSymbolsByKind(t *testing.T) {
	t.Parallel()

	idx := codeintel.Index{Symbols: []codeintel.Symbol{
		{Name: "Run", Kind: "func", File: "b.go", Line: 20},
		{Name: "Client", Kind: "type", File: "a.go", Line: 1},
		{Name: "Build", Kind: "func", File: "a.go", Line: 10},
		{Name: "Count", Kind: "var", File: "c.go", Line: 3},
	}}
	matches := codeSymbolsByKind(idx, " FUNC ")

	want := []codeintel.Symbol{
		{Name: "Build", Kind: "func", File: "a.go", Line: 10},
		{Name: "Run", Kind: "func", File: "b.go", Line: 20},
	}
	if !reflect.DeepEqual(matches, want) {
		require.Failf(t, "unexpected kind matches", "got %#v, want %#v", matches, want)
	}

	if got := codeSymbolsByKind(idx, " "); got != nil {
		require.Failf(t, "blank kind should return nil", "got %#v", got)
	}
}

func TestSummarizeCodeSymbolNameFiles(t *testing.T) {
	t.Parallel()

	root := filepath.Join("tmp", "repo")
	idx := codeintel.Index{Files: []codeintel.File{
		{Path: filepath.Join(root, "pkg", "b.go"), Package: "pkg", Symbols: []codeintel.Symbol{{Name: "Run"}, {Name: "Build"}}},
		{Path: filepath.Join(root, "cmd", "a.go"), Package: "main", Symbols: []codeintel.Symbol{{Name: "Run"}, {Name: "Run"}}},
		{Path: filepath.Join(root, "pkg", "c.go"), Package: "pkg", Symbols: []codeintel.Symbol{{Name: "Client"}}},
	}}
	summaries := summarizeCodeSymbolNameFiles(root, idx, "Run")

	want := []codeSymbolFileSummary{
		{Path: "cmd/a.go", Package: "main", Symbols: 2},
		{Path: "pkg/b.go", Package: "pkg", Symbols: 1},
	}
	if !reflect.DeepEqual(summaries, want) {
		require.Failf(t, "unexpected symbol name file summaries", "got %#v, want %#v", summaries, want)
	}

	if got := summarizeCodeSymbolNameFiles(root, idx, "missing"); len(got) != 0 {
		require.Failf(t, "missing name should return empty", "got %#v", got)
	}

	if got := summarizeCodeSymbolNameFiles(root, idx, " "); got != nil {
		require.Failf(t, "blank name should return nil", "got %#v", got)
	}
}

func TestSummarizeCodeSymbolNamePackages(t *testing.T) {
	t.Parallel()

	idx := codeintel.Index{Files: []codeintel.File{
		{Path: "pkg/b.go", Package: "pkg", Symbols: []codeintel.Symbol{{Name: "Run"}, {Name: "Build"}}},
		{Path: "cmd/a.go", Package: "main", Symbols: []codeintel.Symbol{{Name: "Run"}, {Name: "Run"}}},
		{Path: "pkg/c.go", Package: "pkg", Symbols: []codeintel.Symbol{{Name: "Run"}, {Name: "Run"}, {Name: "Client"}}},
		{Path: "empty.go", Symbols: []codeintel.Symbol{{Name: "Run"}}},
	}}
	summaries := summarizeCodeSymbolNamePackages(idx, "Run")

	want := []codePackageSummary{
		{Name: "pkg", Files: 2, Symbols: 3},
		{Name: "main", Files: 1, Symbols: 2},
	}
	if !reflect.DeepEqual(summaries, want) {
		require.Failf(t, "unexpected symbol name package summaries", "got %#v, want %#v", summaries, want)
	}

	if got := summarizeCodeSymbolNamePackages(idx, "missing"); len(got) != 0 {
		require.Failf(t, "missing name should return empty", "got %#v", got)
	}

	if got := summarizeCodeSymbolNamePackages(idx, " "); got != nil {
		require.Failf(t, "blank name should return nil", "got %#v", got)
	}
}

func TestSummarizeCodeSymbolPrefixFiles(t *testing.T) {
	t.Parallel()

	root := filepath.Join("tmp", "repo")
	idx := codeintel.Index{Files: []codeintel.File{
		{Path: filepath.Join(root, "pkg", "b.go"), Package: "pkg", Symbols: []codeintel.Symbol{{Name: "Run"}, {Name: "Build"}}},
		{Path: filepath.Join(root, "cmd", "a.go"), Package: "main", Symbols: []codeintel.Symbol{{Name: "Render"}, {Name: "Run"}}},
		{Path: filepath.Join(root, "pkg", "c.go"), Package: "pkg", Symbols: []codeintel.Symbol{{Name: "Client"}}},
	}}
	summaries := summarizeCodeSymbolPrefixFiles(root, idx, "R")

	want := []codeSymbolFileSummary{
		{Path: "cmd/a.go", Package: "main", Symbols: 2},
		{Path: "pkg/b.go", Package: "pkg", Symbols: 1},
	}
	if !reflect.DeepEqual(summaries, want) {
		require.Failf(t, "unexpected symbol prefix file summaries", "got %#v, want %#v", summaries, want)
	}

	if got := summarizeCodeSymbolPrefixFiles(root, idx, "missing"); len(got) != 0 {
		require.Failf(t, "missing prefix should return empty", "got %#v", got)
	}

	if got := summarizeCodeSymbolPrefixFiles(root, idx, " "); got != nil {
		require.Failf(t, "blank prefix should return nil", "got %#v", got)
	}
}

func TestSummarizeCodeSymbolPrefixPackages(t *testing.T) {
	t.Parallel()

	idx := codeintel.Index{Files: []codeintel.File{
		{Path: "pkg/b.go", Package: "pkg", Symbols: []codeintel.Symbol{{Name: "Run"}, {Name: "Build"}}},
		{Path: "cmd/a.go", Package: "main", Symbols: []codeintel.Symbol{{Name: "Render"}, {Name: "Run"}}},
		{Path: "pkg/c.go", Package: "pkg", Symbols: []codeintel.Symbol{{Name: "Render"}, {Name: "Run"}, {Name: "Client"}}},
		{Path: "empty.go", Symbols: []codeintel.Symbol{{Name: "Run"}}},
	}}
	summaries := summarizeCodeSymbolPrefixPackages(idx, "R")

	want := []codePackageSummary{
		{Name: "pkg", Files: 2, Symbols: 3},
		{Name: "main", Files: 1, Symbols: 2},
	}
	if !reflect.DeepEqual(summaries, want) {
		require.Failf(t, "unexpected symbol prefix package summaries", "got %#v, want %#v", summaries, want)
	}

	if got := summarizeCodeSymbolPrefixPackages(idx, "missing"); len(got) != 0 {
		require.Failf(t, "missing prefix should return empty", "got %#v", got)
	}

	if got := summarizeCodeSymbolPrefixPackages(idx, " "); got != nil {
		require.Failf(t, "blank prefix should return nil", "got %#v", got)
	}
}

func TestCodeSymbolsWithPrefix(t *testing.T) {
	t.Parallel()

	idx := codeintel.Index{Symbols: []codeintel.Symbol{
		{Name: "RunOnce", File: "b.go", Line: 20},
		{Name: "Build", File: "a.go", Line: 1},
		{Name: "Run", File: "a.go", Line: 10},
		{Name: "Run", File: "a.go", Line: 8},
	}}
	matches := codeSymbolsWithPrefix(idx, "Run")

	want := []codeintel.Symbol{
		{Name: "Run", File: "a.go", Line: 8},
		{Name: "Run", File: "a.go", Line: 10},
		{Name: "RunOnce", File: "b.go", Line: 20},
	}
	if !reflect.DeepEqual(matches, want) {
		require.Failf(t, "unexpected prefix matches", "got %#v, want %#v", matches, want)
	}

	if got := codeSymbolsWithPrefix(idx, " "); got != nil {
		require.Failf(t, "blank prefix should return nil", "got %#v", got)
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

func TestSummarizeAndFormatCodeImportFiles(t *testing.T) {
	t.Parallel()

	root := filepath.Join("tmp", "repo")
	idx := codeintel.Index{Files: []codeintel.File{
		{Path: filepath.Join(root, "pkg", "b.go"), Package: "pkg", Imports: []string{"fmt"}},
		{Path: filepath.Join(root, "cmd", "a.go"), Package: "main", Imports: []string{"context", "fmt"}},
		{Path: filepath.Join(root, "pkg", "empty.go"), Package: "pkg"},
		{Path: filepath.Join(root, "pkg", "c.go"), Package: "pkg", Imports: []string{"bytes", "errors"}},
	}}
	summaries := summarizeCodeImportFiles(root, idx)

	want := []codeImportFileSummary{
		{Path: "cmd/a.go", Package: "main", Imports: 2},
		{Path: "pkg/c.go", Package: "pkg", Imports: 2},
		{Path: "pkg/b.go", Package: "pkg", Imports: 1},
	}
	if !reflect.DeepEqual(summaries, want) {
		require.Failf(t, "unexpected import file summaries", "got %#v, want %#v", summaries, want)
	}

	got := formatCodeImportFileSummary(summaries[0])
	if got != "path=cmd/a.go	package=main	imports=2" {
		require.Failf(t, "unexpected import file summary format", "got %q", got)
	}
}

func TestSummarizeAndFormatCodeImports(t *testing.T) {
	t.Parallel()

	idx := codeintel.Index{ImportEdges: []codeintel.ImportEdge{
		{From: "a.go", Import: "fmt"},
		{From: "b.go", Import: "context"},
		{From: "a.go", Import: "fmt"},
		{From: "c.go", Import: "fmt"},
		{From: "d.go", Import: "context"},
	}}
	summaries := summarizeCodeImports(idx)

	want := []codeImportSummary{{Path: "context", Files: 2}, {Path: "fmt", Files: 2}}
	if !reflect.DeepEqual(summaries, want) {
		require.Failf(t, "unexpected import summaries", "got %#v, want %#v", summaries, want)
	}

	got := formatCodeImportSummary(summaries[1])
	if got != "import=fmt	files=2" {
		require.Failf(t, "unexpected import summary format", "got %q", got)
	}
}

func TestSummarizeCodeImportPrefix(t *testing.T) {
	t.Parallel()

	idx := codeintel.Index{ImportEdges: []codeintel.ImportEdge{
		{From: "b.go", Import: "github.com/tommoulard/atteler/pkg/llm"},
		{From: "a.go", Import: "context"},
		{From: "c.go", Import: "github.com/tommoulard/atteler/pkg/agent"},
		{From: "d.go", Import: "github.com/tommoulard/atteler/pkg/llm"},
		{From: "d.go", Import: "github.com/tommoulard/atteler/pkg/llm"},
	}}
	summaries := summarizeCodeImportPrefix(idx, "github.com/tommoulard/atteler/pkg/")

	want := []codeImportSummary{
		{Path: "github.com/tommoulard/atteler/pkg/llm", Files: 2},
		{Path: "github.com/tommoulard/atteler/pkg/agent", Files: 1},
	}
	if !reflect.DeepEqual(summaries, want) {
		require.Failf(t, "unexpected import prefix summaries", "got %#v, want %#v", summaries, want)
	}

	if got := summarizeCodeImportPrefix(idx, "missing"); len(got) != 0 {
		require.Failf(t, "missing prefix should return empty", "got %#v", got)
	}

	if got := summarizeCodeImportPrefix(idx, " "); got != nil {
		require.Failf(t, "blank prefix should return nil", "got %#v", got)
	}
}

func TestSummarizeCodeImportPrefixFiles(t *testing.T) {
	t.Parallel()

	root := filepath.Join("tmp", "repo")
	idx := codeintel.Index{
		Files: []codeintel.File{
			{Path: filepath.Join(root, "pkg", "b.go"), Package: "pkg"},
			{Path: filepath.Join(root, "cmd", "a.go"), Package: "main"},
			{Path: filepath.Join(root, "pkg", "c.go"), Package: "pkg"},
		},
		ImportEdges: []codeintel.ImportEdge{
			{From: filepath.Join(root, "pkg", "b.go"), Import: "github.com/example/beta"},
			{From: filepath.Join(root, "pkg", "b.go"), Import: "github.com/example/beta"},
			{From: filepath.Join(root, "cmd", "a.go"), Import: "github.com/example/alpha"},
			{From: filepath.Join(root, "cmd", "a.go"), Import: "github.com/example/beta"},
			{From: filepath.Join(root, "pkg", "c.go"), Import: "context"},
		},
	}
	summaries := summarizeCodeImportPrefixFiles(root, idx, "github.com/example/")

	want := []codeImportFileSummary{
		{Path: "cmd/a.go", Package: "main", Imports: 2},
		{Path: "pkg/b.go", Package: "pkg", Imports: 1},
	}
	if !reflect.DeepEqual(summaries, want) {
		require.Failf(t, "unexpected import prefix file summaries", "got %#v, want %#v", summaries, want)
	}

	if got := summarizeCodeImportPrefixFiles(root, idx, "missing"); len(got) != 0 {
		require.Failf(t, "missing prefix should return empty", "got %#v", got)
	}

	if got := summarizeCodeImportPrefixFiles(root, idx, " "); got != nil {
		require.Failf(t, "blank prefix should return nil", "got %#v", got)
	}
}

func TestSummarizeCodeImportPrefixPackages(t *testing.T) {
	t.Parallel()

	idx := codeintel.Index{
		Files: []codeintel.File{
			{Path: "pkg/b.go", Package: "pkg"},
			{Path: "cmd/a.go", Package: "main"},
			{Path: "pkg/c.go", Package: "pkg"},
			{Path: "empty.go"},
		},
		ImportEdges: []codeintel.ImportEdge{
			{From: "pkg/b.go", Import: "github.com/example/beta"},
			{From: "pkg/b.go", Import: "github.com/example/beta"},
			{From: "cmd/a.go", Import: "github.com/example/alpha"},
			{From: "cmd/a.go", Import: "github.com/example/beta"},
			{From: "pkg/c.go", Import: "context"},
			{From: "empty.go", Import: "github.com/example/ignored"},
		},
	}
	summaries := summarizeCodeImportPrefixPackages(idx, "github.com/example/")

	want := []codePackageImportMatchSummary{
		{Name: "main", Files: 1, Imports: 2},
		{Name: "pkg", Files: 1, Imports: 1},
	}
	if !reflect.DeepEqual(summaries, want) {
		require.Failf(t, "unexpected import prefix package summaries", "got %#v, want %#v", summaries, want)
	}

	if got := formatCodePackageImportMatchSummary(summaries[0]); got != "package=main\tfiles=1\timports=2" {
		require.Failf(t, "unexpected import package summary format", "got %q", got)
	}

	if got := summarizeCodeImportPrefixPackages(idx, "missing"); len(got) != 0 {
		require.Failf(t, "missing prefix should return empty", "got %#v", got)
	}

	if got := summarizeCodeImportPrefixPackages(idx, " "); got != nil {
		require.Failf(t, "blank prefix should return nil", "got %#v", got)
	}
}

func TestCodeImportEdgesWithPrefix(t *testing.T) {
	t.Parallel()

	idx := codeintel.Index{ImportEdges: []codeintel.ImportEdge{
		{From: "b.go", Import: "github.com/tommoulard/atteler/pkg/llm"},
		{From: "a.go", Import: "context"},
		{From: "c.go", Import: "github.com/tommoulard/atteler/pkg/agent"},
	}}
	edges := codeImportEdgesWithPrefix(idx, "github.com/tommoulard/atteler/pkg/")

	want := []codeintel.ImportEdge{
		{From: "b.go", Import: "github.com/tommoulard/atteler/pkg/llm"},
		{From: "c.go", Import: "github.com/tommoulard/atteler/pkg/agent"},
	}
	if !reflect.DeepEqual(edges, want) {
		require.Failf(t, "unexpected import prefix edges", "got %#v, want %#v", edges, want)
	}

	if got := codeImportEdgesWithPrefix(idx, " "); got != nil {
		require.Failf(t, "blank import prefix should return nil", "got %#v", got)
	}
}

func TestCodeImportEdgesForPath(t *testing.T) {
	t.Parallel()

	idx := codeintel.Index{ImportEdges: []codeintel.ImportEdge{
		{From: "b.go", Import: "fmt"},
		{From: "a.go", Import: "context"},
		{From: "c.go", Import: "context"},
	}}
	edges := codeImportEdgesForPath(idx, "context")

	want := []codeintel.ImportEdge{{From: "a.go", Import: "context"}, {From: "c.go", Import: "context"}}
	if !reflect.DeepEqual(edges, want) {
		require.Failf(t, "unexpected import path edges", "got %#v, want %#v", edges, want)
	}

	if got := codeImportEdgesForPath(idx, " "); got != nil {
		require.Failf(t, "blank import path should return nil", "got %#v", got)
	}
}

func TestSummarizeCodeImportPath(t *testing.T) {
	t.Parallel()

	idx := codeintel.Index{ImportEdges: []codeintel.ImportEdge{
		{From: "b.go", Import: "fmt"},
		{From: "a.go", Import: "context"},
		{From: "a.go", Import: "context"},
		{From: "c.go", Import: "context"},
	}}
	summaries := summarizeCodeImportPath(idx, "context")

	want := []codeImportSummary{{Path: "context", Files: 2}}
	if !reflect.DeepEqual(summaries, want) {
		require.Failf(t, "unexpected import path summary", "got %#v, want %#v", summaries, want)
	}

	if got := summarizeCodeImportPath(idx, "missing"); len(got) != 0 {
		require.Failf(t, "missing import path should return empty", "got %#v", got)
	}

	if got := summarizeCodeImportPath(idx, " "); got != nil {
		require.Failf(t, "blank import path should return nil", "got %#v", got)
	}
}

func TestSummarizeCodeImportPathFiles(t *testing.T) {
	t.Parallel()

	root := filepath.Join("tmp", "repo")
	idx := codeintel.Index{
		Files: []codeintel.File{
			{Path: filepath.Join(root, "pkg", "b.go"), Package: "pkg"},
			{Path: filepath.Join(root, "cmd", "a.go"), Package: "main"},
			{Path: filepath.Join(root, "pkg", "c.go"), Package: "pkg"},
		},
		ImportEdges: []codeintel.ImportEdge{
			{From: filepath.Join(root, "pkg", "b.go"), Import: "context"},
			{From: filepath.Join(root, "pkg", "b.go"), Import: "context"},
			{From: filepath.Join(root, "cmd", "a.go"), Import: "context"},
			{From: filepath.Join(root, "pkg", "c.go"), Import: "fmt"},
		},
	}
	summaries := summarizeCodeImportPathFiles(root, idx, "context")

	want := []codeImportFileSummary{
		{Path: "cmd/a.go", Package: "main", Imports: 1},
		{Path: "pkg/b.go", Package: "pkg", Imports: 1},
	}
	if !reflect.DeepEqual(summaries, want) {
		require.Failf(t, "unexpected import path file summaries", "got %#v, want %#v", summaries, want)
	}

	if got := summarizeCodeImportPathFiles(root, idx, "missing"); len(got) != 0 {
		require.Failf(t, "missing import path should return empty", "got %#v", got)
	}

	if got := summarizeCodeImportPathFiles(root, idx, " "); got != nil {
		require.Failf(t, "blank import path should return nil", "got %#v", got)
	}
}

func TestSummarizeAndFormatCodeImportPathPackages(t *testing.T) {
	t.Parallel()

	idx := codeintel.Index{
		Files: []codeintel.File{
			{Path: "pkg/b.go", Package: "pkg"},
			{Path: "cmd/a.go", Package: "main"},
			{Path: "pkg/c.go", Package: "pkg"},
			{Path: "empty.go"},
		},
		ImportEdges: []codeintel.ImportEdge{
			{From: "pkg/b.go", Import: "context"},
			{From: "pkg/b.go", Import: "context"},
			{From: "cmd/a.go", Import: "context"},
			{From: "pkg/c.go", Import: "fmt"},
			{From: "empty.go", Import: "context"},
		},
	}
	summaries := summarizeCodeImportPathPackages(idx, "context")

	want := []codePackageImportMatchSummary{
		{Name: "main", Files: 1},
		{Name: "pkg", Files: 1},
	}
	if !reflect.DeepEqual(summaries, want) {
		require.Failf(t, "unexpected import path package summaries", "got %#v, want %#v", summaries, want)
	}

	if got := formatCodePackageImportMatchSummary(summaries[0]); got != "package=main\tfiles=1" {
		require.Failf(t, "unexpected import package summary format", "got %q", got)
	}

	if got := summarizeCodeImportPathPackages(idx, "missing"); len(got) != 0 {
		require.Failf(t, "missing import path should return empty", "got %#v", got)
	}

	if got := summarizeCodeImportPathPackages(idx, " "); got != nil {
		require.Failf(t, "blank import path should return nil", "got %#v", got)
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

func TestSummarizeCodePackageSymbolFiles(t *testing.T) {
	t.Parallel()

	root := filepath.Join("tmp", "repo")
	idx := codeintel.Index{Files: []codeintel.File{
		{Path: filepath.Join(root, "pkg", "b.go"), Package: "pkg", Symbols: []codeintel.Symbol{{Name: "B"}}},
		{Path: filepath.Join(root, "cmd", "a.go"), Package: "main", Symbols: []codeintel.Symbol{{Name: "A"}, {Name: "Run"}}},
		{Path: filepath.Join(root, "pkg", "empty.go"), Package: "pkg"},
		{Path: filepath.Join(root, "pkg", "c.go"), Package: "pkg", Symbols: []codeintel.Symbol{{Name: "C"}, {Name: "Build"}}},
	}}
	summaries := summarizeCodePackageSymbolFiles(root, idx, "pkg")

	want := []codeSymbolFileSummary{
		{Path: "pkg/c.go", Package: "pkg", Symbols: 2},
		{Path: "pkg/b.go", Package: "pkg", Symbols: 1},
	}
	if !reflect.DeepEqual(summaries, want) {
		require.Failf(t, "unexpected package symbol file summaries", "got %#v, want %#v", summaries, want)
	}

	if got := summarizeCodePackageSymbolFiles(root, idx, "missing"); len(got) != 0 {
		require.Failf(t, "missing package should return empty", "got %#v", got)
	}

	if got := summarizeCodePackageSymbolFiles(root, idx, " "); got != nil {
		require.Failf(t, "blank package should return nil", "got %#v", got)
	}
}

func TestCodePackageSymbolsByName(t *testing.T) {
	t.Parallel()

	const testPackageName = "core"

	packageName, name, err := parseCodeFileSymbolFilterSpec(testPackageName+":Run", "code package symbol", "package:name")
	if err != nil {
		require.NoError(t, err)
	}

	if packageName != testPackageName || name != "Run" {
		require.Failf(t, "unexpected parsed spec", "package=%q name=%q", packageName, name)
	}

	if _, _, err := parseCodeFileSymbolFilterSpec(testPackageName, "code package symbol", "package:name"); err == nil {
		require.Fail(t, "expected parse error for missing symbol name")
	}

	idx := codeintel.Index{Files: []codeintel.File{
		{Package: testPackageName, Symbols: []codeintel.Symbol{{Name: "Run", Kind: "func", File: "b.go", Line: 2}, {Name: "Client", Kind: "type", File: "a.go", Line: 1}}},
		{Package: "main", Symbols: []codeintel.Symbol{{Name: "Run", Kind: "func", File: "main.go", Line: 1}}},
		{Package: testPackageName, Symbols: []codeintel.Symbol{{Name: "Run", Kind: "method", File: "a.go", Line: 3}, {Name: "Build", Kind: "func", File: "c.go", Line: 4}}},
	}}
	symbols := codePackageSymbolsByName(idx, testPackageName, "Run")

	want := []codeintel.Symbol{
		{Name: "Run", Kind: "method", File: "a.go", Line: 3},
		{Name: "Run", Kind: "func", File: "b.go", Line: 2},
	}
	if !reflect.DeepEqual(symbols, want) {
		require.Failf(t, "unexpected package symbols by name", "got %#v, want %#v", symbols, want)
	}

	if got := codePackageSymbolsByName(idx, testPackageName, " "); got != nil {
		require.Failf(t, "blank symbol name should return nil", "got %#v", got)
	}

	if got := codePackageSymbolsByName(idx, "missing", "Run"); got != nil {
		require.Failf(t, "missing package should return nil", "got %#v", got)
	}
}

func TestSummarizeCodePackageSymbolNameFiles(t *testing.T) {
	t.Parallel()

	root := filepath.Join("tmp", "repo")
	idx := codeintel.Index{Files: []codeintel.File{
		{Path: filepath.Join(root, "pkg", "b.go"), Package: "pkg", Symbols: []codeintel.Symbol{{Name: "Run"}, {Name: "Build"}}},
		{Path: filepath.Join(root, "cmd", "a.go"), Package: "main", Symbols: []codeintel.Symbol{{Name: "Run"}, {Name: "Run"}}},
		{Path: filepath.Join(root, "pkg", "c.go"), Package: "pkg", Symbols: []codeintel.Symbol{{Name: "Run"}, {Name: "Run"}, {Name: "Client"}}},
	}}
	summaries := summarizeCodePackageSymbolNameFiles(root, idx, "pkg", "Run")

	want := []codeSymbolFileSummary{
		{Path: "pkg/c.go", Package: "pkg", Symbols: 2},
		{Path: "pkg/b.go", Package: "pkg", Symbols: 1},
	}
	if !reflect.DeepEqual(summaries, want) {
		require.Failf(t, "unexpected package symbol name file summaries", "got %#v, want %#v", summaries, want)
	}

	if got := summarizeCodePackageSymbolNameFiles(root, idx, "pkg", "missing"); len(got) != 0 {
		require.Failf(t, "missing name should return empty", "got %#v", got)
	}

	if got := summarizeCodePackageSymbolNameFiles(root, idx, "pkg", " "); got != nil {
		require.Failf(t, "blank name should return nil", "got %#v", got)
	}

	if got := summarizeCodePackageSymbolNameFiles(root, idx, "missing", "Run"); len(got) != 0 {
		require.Failf(t, "missing package should return empty", "got %#v", got)
	}

	if got := summarizeCodePackageSymbolNameFiles(root, idx, " ", "Run"); got != nil {
		require.Failf(t, "blank package should return nil", "got %#v", got)
	}
}

func TestCodePackageSymbolsByKindAndParseSpec(t *testing.T) {
	t.Parallel()

	packageName, kind, err := parseCodePackageSymbolKindSpec("llm:func")
	if err != nil {
		require.NoError(t, err)
	}

	if packageName != "llm" || kind != "func" {
		require.Failf(t, "unexpected parsed spec", "package=%q kind=%q", packageName, kind)
	}

	if _, _, err := parseCodePackageSymbolKindSpec("llm"); err == nil {
		require.Fail(t, "expected parse error for missing kind")
	}

	idx := codeintel.Index{Files: []codeintel.File{
		{Package: "llm", Symbols: []codeintel.Symbol{{Name: "Run", Kind: "func", File: "b.go", Line: 2}, {Name: "Client", Kind: "type", File: "a.go", Line: 1}}},
		{Package: "llm", Symbols: []codeintel.Symbol{{Name: "Build", Kind: "func", File: "c.go", Line: 3}}},
	}}
	symbols := codePackageSymbolsByKind(idx, "llm", "FUNC")

	want := []codeintel.Symbol{
		{Name: "Build", Kind: "func", File: "c.go", Line: 3},
		{Name: "Run", Kind: "func", File: "b.go", Line: 2},
	}
	if !reflect.DeepEqual(symbols, want) {
		require.Failf(t, "unexpected package symbols by kind", "got %#v, want %#v", symbols, want)
	}
}

func TestSummarizeCodePackageSymbolPrefixFiles(t *testing.T) {
	t.Parallel()

	root := filepath.Join("tmp", "repo")
	idx := codeintel.Index{Files: []codeintel.File{
		{Path: filepath.Join(root, "pkg", "b.go"), Package: "pkg", Symbols: []codeintel.Symbol{{Name: "Run"}, {Name: "Build"}}},
		{Path: filepath.Join(root, "cmd", "a.go"), Package: "main", Symbols: []codeintel.Symbol{{Name: "Render"}, {Name: "Run"}}},
		{Path: filepath.Join(root, "pkg", "c.go"), Package: "pkg", Symbols: []codeintel.Symbol{{Name: "Render"}, {Name: "Run"}, {Name: "Client"}}},
	}}
	summaries := summarizeCodePackageSymbolPrefixFiles(root, idx, "pkg", "R")

	want := []codeSymbolFileSummary{
		{Path: "pkg/c.go", Package: "pkg", Symbols: 2},
		{Path: "pkg/b.go", Package: "pkg", Symbols: 1},
	}
	if !reflect.DeepEqual(summaries, want) {
		require.Failf(t, "unexpected package symbol prefix file summaries", "got %#v, want %#v", summaries, want)
	}

	if got := summarizeCodePackageSymbolPrefixFiles(root, idx, "pkg", "missing"); len(got) != 0 {
		require.Failf(t, "missing prefix should return empty", "got %#v", got)
	}

	if got := summarizeCodePackageSymbolPrefixFiles(root, idx, "pkg", " "); got != nil {
		require.Failf(t, "blank prefix should return nil", "got %#v", got)
	}

	if got := summarizeCodePackageSymbolPrefixFiles(root, idx, "missing", "R"); len(got) != 0 {
		require.Failf(t, "missing package should return empty", "got %#v", got)
	}
}

func TestSummarizeCodePackageSymbolKindFiles(t *testing.T) {
	t.Parallel()

	root := filepath.Join("tmp", "repo")
	idx := codeintel.Index{Files: []codeintel.File{
		{Path: filepath.Join(root, "pkg", "b.go"), Package: "pkg", Symbols: []codeintel.Symbol{{Kind: "func"}, {Kind: "type"}}},
		{Path: filepath.Join(root, "cmd", "a.go"), Package: "main", Symbols: []codeintel.Symbol{{Kind: "func"}, {Kind: "FUNC"}}},
		{Path: filepath.Join(root, "pkg", "c.go"), Package: "pkg", Symbols: []codeintel.Symbol{{Kind: "func"}, {Kind: "FUNC"}, {Kind: "const"}}},
	}}
	summaries := summarizeCodePackageSymbolKindFiles(root, idx, "pkg", "func")

	want := []codeSymbolFileSummary{
		{Path: "pkg/c.go", Package: "pkg", Symbols: 2},
		{Path: "pkg/b.go", Package: "pkg", Symbols: 1},
	}
	if !reflect.DeepEqual(summaries, want) {
		require.Failf(t, "unexpected package symbol kind file summaries", "got %#v, want %#v", summaries, want)
	}

	if got := summarizeCodePackageSymbolKindFiles(root, idx, "pkg", "missing"); len(got) != 0 {
		require.Failf(t, "missing kind should return empty", "got %#v", got)
	}

	if got := summarizeCodePackageSymbolKindFiles(root, idx, "pkg", " "); got != nil {
		require.Failf(t, "blank kind should return nil", "got %#v", got)
	}

	if got := summarizeCodePackageSymbolKindFiles(root, idx, "missing", "func"); len(got) != 0 {
		require.Failf(t, "missing package should return empty", "got %#v", got)
	}
}

func TestCodePackageSymbolsWithPrefix(t *testing.T) {
	t.Parallel()

	packageName, prefix, err := parseCodeFileSymbolFilterSpec("llm:Ru", "code package symbol prefix", "package:prefix")
	if err != nil {
		require.NoError(t, err)
	}

	if packageName != "llm" || prefix != "Ru" {
		require.Failf(t, "unexpected parsed spec", "package=%q prefix=%q", packageName, prefix)
	}

	if _, _, err := parseCodeFileSymbolFilterSpec("llm", "code package symbol prefix", "package:prefix"); err == nil {
		require.Fail(t, "expected parse error for missing prefix")
	}

	idx := codeintel.Index{Files: []codeintel.File{
		{Package: "llm", Symbols: []codeintel.Symbol{{Name: "Run", File: "b.go", Line: 2}, {Name: "Client", File: "a.go", Line: 1}}},
		{Package: "main", Symbols: []codeintel.Symbol{{Name: "Main", File: "main.go", Line: 1}}},
		{Package: "llm", Symbols: []codeintel.Symbol{{Name: "Runtime", File: "c.go", Line: 3}, {Name: "Build", File: "c.go", Line: 4}}},
	}}
	symbols := codePackageSymbolsWithPrefix(idx, "llm", "Ru")

	want := []codeintel.Symbol{
		{Name: "Run", File: "b.go", Line: 2},
		{Name: "Runtime", File: "c.go", Line: 3},
	}
	if !reflect.DeepEqual(symbols, want) {
		require.Failf(t, "unexpected package symbols by prefix", "got %#v, want %#v", symbols, want)
	}

	if got := codePackageSymbolsWithPrefix(idx, "llm", " "); got != nil {
		require.Failf(t, "blank prefix should return nil", "got %#v", got)
	}

	if got := codePackageSymbolsWithPrefix(idx, "missing", "Ru"); got != nil {
		require.Failf(t, "missing package should return nil", "got %#v", got)
	}
}

func TestSummarizeAndFormatCodePackageImportCounts(t *testing.T) {
	t.Parallel()

	idx := codeintel.Index{Files: []codeintel.File{
		{Package: "pkg", Imports: []string{"fmt"}},
		{Package: "main", Imports: []string{"context", "fmt"}},
		{Package: "pkg"},
		{Package: "pkg", Imports: []string{"bytes", "fmt"}},
		{Package: "empty"},
	}}
	summaries := summarizeCodePackageImportCounts(idx)

	want := []codePackageImportSummary{
		{Name: "pkg", Files: 3, Imports: 3, UniqueImports: 2},
		{Name: "main", Files: 1, Imports: 2, UniqueImports: 2},
	}
	if !reflect.DeepEqual(summaries, want) {
		require.Failf(t, "unexpected package import summaries", "got %#v, want %#v", summaries, want)
	}

	got := formatCodePackageImportSummary(summaries[0])
	if got != "package=pkg	files=3	imports=3	unique_imports=2" {
		require.Failf(t, "unexpected package import summary format", "got %q", got)
	}
}

func TestCodePackageSymbols(t *testing.T) {
	t.Parallel()

	idx := codeintel.Index{Files: []codeintel.File{
		{Package: "llm", Symbols: []codeintel.Symbol{{Name: "Run", File: "b.go", Line: 2}, {Name: "Client", File: "a.go", Line: 1}}},
		{Package: "main", Symbols: []codeintel.Symbol{{Name: "Main", File: "main.go", Line: 1}}},
		{Package: "llm", Symbols: []codeintel.Symbol{{Name: "Build", File: "c.go", Line: 3}}},
	}}
	symbols := codePackageSymbols(idx, "llm")

	want := []codeintel.Symbol{
		{Name: "Build", File: "c.go", Line: 3},
		{Name: "Client", File: "a.go", Line: 1},
		{Name: "Run", File: "b.go", Line: 2},
	}
	if !reflect.DeepEqual(symbols, want) {
		require.Failf(t, "unexpected package symbols", "got %#v, want %#v", symbols, want)
	}

	if got := codePackageSymbols(idx, " "); got != nil {
		require.Failf(t, "blank package should return nil", "got %#v", got)
	}
}

func TestSummarizeCodePackageSymbols(t *testing.T) {
	t.Parallel()

	idx := codeintel.Index{Files: []codeintel.File{
		{Package: "llm", Symbols: []codeintel.Symbol{{Kind: "func"}, {Kind: "type"}}},
		{Package: "main", Symbols: []codeintel.Symbol{{Kind: "func"}}},
		{Package: "llm", Symbols: []codeintel.Symbol{{Kind: "func"}, {Kind: "const"}}},
	}}
	summaries := summarizeCodePackageSymbols(idx, "llm")

	want := []codeSymbolSummary{{Kind: "func", Count: 2}, {Kind: "const", Count: 1}, {Kind: "type", Count: 1}}
	if !reflect.DeepEqual(summaries, want) {
		require.Failf(t, "unexpected package symbol summaries", "got %#v, want %#v", summaries, want)
	}

	if got := summarizeCodePackageSymbols(idx, "missing"); len(got) != 0 {
		require.Failf(t, "missing package should return empty", "got %#v", got)
	}
}

func TestSummarizeCodePackageImportFiles(t *testing.T) {
	t.Parallel()

	root := filepath.Join("tmp", "repo")
	idx := codeintel.Index{Files: []codeintel.File{
		{Path: filepath.Join(root, "pkg", "b.go"), Package: "pkg", Imports: []string{"fmt"}},
		{Path: filepath.Join(root, "cmd", "a.go"), Package: "main", Imports: []string{"context", "fmt"}},
		{Path: filepath.Join(root, "pkg", "empty.go"), Package: "pkg"},
		{Path: filepath.Join(root, "pkg", "c.go"), Package: "pkg", Imports: []string{"bytes", "errors"}},
	}}
	summaries := summarizeCodePackageImportFiles(root, idx, "pkg")

	want := []codeImportFileSummary{
		{Path: "pkg/c.go", Package: "pkg", Imports: 2},
		{Path: "pkg/b.go", Package: "pkg", Imports: 1},
	}
	if !reflect.DeepEqual(summaries, want) {
		require.Failf(t, "unexpected package import file summaries", "got %#v, want %#v", summaries, want)
	}

	if got := summarizeCodePackageImportFiles(root, idx, "missing"); len(got) != 0 {
		require.Failf(t, "missing package should return empty", "got %#v", got)
	}

	if got := summarizeCodePackageImportFiles(root, idx, " "); got != nil {
		require.Failf(t, "blank package should return nil", "got %#v", got)
	}
}

func TestSummarizeCodePackageImportPrefixFiles(t *testing.T) {
	t.Parallel()

	const testImportPackageName = "core"

	root := filepath.Join("tmp", "repo")
	idx := codeintel.Index{
		Files: []codeintel.File{
			{Path: filepath.Join(root, "pkg", "core", "b.go"), Package: testImportPackageName},
			{Path: filepath.Join(root, "pkg", "core", "a.go"), Package: testImportPackageName},
			{Path: filepath.Join(root, "cmd", "main.go"), Package: "main"},
		},
		ImportEdges: []codeintel.ImportEdge{
			{From: filepath.Join(root, "pkg", "core", "b.go"), Import: "github.com/example/beta"},
			{From: filepath.Join(root, "pkg", "core", "b.go"), Import: "github.com/example/beta"},
			{From: filepath.Join(root, "pkg", "core", "a.go"), Import: "github.com/example/alpha"},
			{From: filepath.Join(root, "pkg", "core", "a.go"), Import: "github.com/example/beta"},
			{From: filepath.Join(root, "cmd", "main.go"), Import: "github.com/example/alpha"},
			{From: filepath.Join(root, "pkg", "core", "a.go"), Import: "context"},
		},
	}
	summaries := summarizeCodePackageImportPrefixFiles(root, idx, testImportPackageName, "github.com/example/")

	want := []codeImportFileSummary{
		{Path: "pkg/core/a.go", Package: testImportPackageName, Imports: 2},
		{Path: "pkg/core/b.go", Package: testImportPackageName, Imports: 1},
	}
	if !reflect.DeepEqual(summaries, want) {
		require.Failf(t, "unexpected package import prefix file summaries", "got %#v, want %#v", summaries, want)
	}

	if got := summarizeCodePackageImportPrefixFiles(root, idx, testImportPackageName, "missing"); len(got) != 0 {
		require.Failf(t, "missing prefix should return empty", "got %#v", got)
	}

	if got := summarizeCodePackageImportPrefixFiles(root, idx, testImportPackageName, " "); got != nil {
		require.Failf(t, "blank prefix should return nil", "got %#v", got)
	}
}

func TestCodePackageImportPrefixFiles(t *testing.T) {
	t.Parallel()

	const testImportPackageName = "core"

	root := filepath.Join("tmp", "repo")

	packageName, prefix, err := parseCodeFileSymbolFilterSpec(testImportPackageName+":github.com/example/", "code package import prefix files", "package:prefix")
	if err != nil {
		require.NoError(t, err)
	}

	if packageName != testImportPackageName || prefix != "github.com/example/" {
		require.Failf(t, "unexpected parsed spec", "package=%q prefix=%q", packageName, prefix)
	}

	if _, _, err := parseCodeFileSymbolFilterSpec(testImportPackageName, "code package import prefix files", "package:prefix"); err == nil {
		require.Fail(t, "expected parse error for missing prefix")
	}

	idx := codeintel.Index{
		Files: []codeintel.File{
			{Path: filepath.Join(root, "pkg", "core", "b.go"), Package: testImportPackageName},
			{Path: filepath.Join(root, "pkg", "core", "a.go"), Package: testImportPackageName},
			{Path: filepath.Join(root, "cmd", "main.go"), Package: "main"},
		},
		ImportEdges: []codeintel.ImportEdge{
			{From: filepath.Join(root, "pkg", "core", "b.go"), Import: "github.com/example/beta"},
			{From: filepath.Join(root, "pkg", "core", "b.go"), Import: "github.com/example/beta"},
			{From: filepath.Join(root, "pkg", "core", "a.go"), Import: "github.com/example/alpha"},
			{From: filepath.Join(root, "pkg", "core", "a.go"), Import: "github.com/example/beta"},
			{From: filepath.Join(root, "cmd", "main.go"), Import: "github.com/example/alpha"},
			{From: filepath.Join(root, "pkg", "core", "a.go"), Import: "context"},
		},
	}
	edges := codePackageImportPrefixFiles(idx, testImportPackageName, "github.com/example/")

	want := []codeintel.ImportEdge{
		{From: filepath.Join(root, "pkg", "core", "a.go"), Import: "github.com/example/alpha"},
		{From: filepath.Join(root, "pkg", "core", "a.go"), Import: "github.com/example/beta"},
		{From: filepath.Join(root, "pkg", "core", "b.go"), Import: "github.com/example/beta"},
	}
	if !reflect.DeepEqual(edges, want) {
		require.Failf(t, "unexpected package import prefix files", "got %#v, want %#v", edges, want)
	}

	if got := codePackageImportPrefixFiles(idx, testImportPackageName, "missing"); len(got) != 0 {
		require.Failf(t, "missing prefix should return empty", "got %#v", got)
	}

	if got := codePackageImportPrefixFiles(idx, testImportPackageName, " "); got != nil {
		require.Failf(t, "blank prefix should return nil", "got %#v", got)
	}
}

func TestCodePackageImportFiles(t *testing.T) {
	t.Parallel()

	root := filepath.Join("tmp", "repo")

	packageName, importPath, err := parseCodeFileSymbolFilterSpec("core:context", "code package import files", "package:import")
	if err != nil {
		require.NoError(t, err)
	}

	if packageName != "core" || importPath != "context" {
		require.Failf(t, "unexpected parsed spec", "package=%q import=%q", packageName, importPath)
	}

	if _, _, err := parseCodeFileSymbolFilterSpec("core", "code package import files", "package:import"); err == nil {
		require.Fail(t, "expected parse error for missing import path")
	}

	idx := codeintel.Index{
		Files: []codeintel.File{
			{Path: filepath.Join(root, "pkg", "core", "b.go"), Package: "core"},
			{Path: filepath.Join(root, "pkg", "core", "a.go"), Package: "core"},
			{Path: filepath.Join(root, "cmd", "main.go"), Package: "main"},
		},
		ImportEdges: []codeintel.ImportEdge{
			{From: filepath.Join(root, "pkg", "core", "b.go"), Import: "context"},
			{From: filepath.Join(root, "pkg", "core", "b.go"), Import: "context"},
			{From: filepath.Join(root, "pkg", "core", "a.go"), Import: "fmt"},
			{From: filepath.Join(root, "cmd", "main.go"), Import: "context"},
		},
	}
	files := codePackageImportFiles(root, idx, "core", "context")

	want := []string{"pkg/core/b.go"}
	if !reflect.DeepEqual(files, want) {
		require.Failf(t, "unexpected package import files", "got %#v, want %#v", files, want)
	}

	if got := codePackageImportFiles(root, idx, "core", "missing"); len(got) != 0 {
		require.Failf(t, "missing import should return empty", "got %#v", got)
	}

	if got := codePackageImportFiles(root, idx, "core", " "); got != nil {
		require.Failf(t, "blank import path should return nil", "got %#v", got)
	}
}

func TestSummarizeCodePackageImportPathFiles(t *testing.T) {
	t.Parallel()

	root := filepath.Join("tmp", "repo")
	idx := codeintel.Index{
		Files: []codeintel.File{
			{Path: filepath.Join(root, "pkg", "core", "b.go"), Package: "core"},
			{Path: filepath.Join(root, "pkg", "core", "a.go"), Package: "core"},
			{Path: filepath.Join(root, "cmd", "main.go"), Package: "main"},
		},
		ImportEdges: []codeintel.ImportEdge{
			{From: filepath.Join(root, "pkg", "core", "b.go"), Import: "context"},
			{From: filepath.Join(root, "pkg", "core", "b.go"), Import: "context"},
			{From: filepath.Join(root, "pkg", "core", "a.go"), Import: "fmt"},
			{From: filepath.Join(root, "cmd", "main.go"), Import: "context"},
		},
	}
	summaries := summarizeCodePackageImportPathFiles(root, idx, "core", "context")

	want := []codeImportFileSummary{{Path: "pkg/core/b.go", Package: "core", Imports: 1}}
	if !reflect.DeepEqual(summaries, want) {
		require.Failf(t, "unexpected package import path file summaries", "got %#v, want %#v", summaries, want)
	}

	if got := summarizeCodePackageImportPathFiles(root, idx, "core", "missing"); len(got) != 0 {
		require.Failf(t, "missing import should return empty", "got %#v", got)
	}

	if got := summarizeCodePackageImportPathFiles(root, idx, "core", " "); got != nil {
		require.Failf(t, "blank import path should return nil", "got %#v", got)
	}

	if got := summarizeCodePackageImportPathFiles(root, idx, "missing", "context"); len(got) != 0 {
		require.Failf(t, "missing package should return empty", "got %#v", got)
	}
}

func TestSummarizeCodePackageImportPath(t *testing.T) {
	t.Parallel()

	packageName, importPath, err := parseCodeFileSymbolFilterSpec("core:context", "code package import path", "package:import")
	if err != nil {
		require.NoError(t, err)
	}

	if packageName != "core" || importPath != "context" {
		require.Failf(t, "unexpected parsed spec", "package=%q import=%q", packageName, importPath)
	}

	if _, _, err := parseCodeFileSymbolFilterSpec("core", "code package import path", "package:import"); err == nil {
		require.Fail(t, "expected parse error for missing import path")
	}

	idx := codeintel.Index{
		Files: []codeintel.File{
			{Path: "pkg/core/a.go", Package: "core"},
			{Path: "pkg/core/b.go", Package: "core"},
			{Path: "cmd/main.go", Package: "main"},
		},
		ImportEdges: []codeintel.ImportEdge{
			{From: "pkg/core/a.go", Import: "context"},
			{From: "pkg/core/b.go", Import: "context"},
			{From: "pkg/core/b.go", Import: "fmt"},
			{From: "cmd/main.go", Import: "context"},
		},
	}
	summaries := summarizeCodePackageImportPath(idx, "core", "context")

	want := []codeImportSummary{{Path: "context", Files: 2}}
	if !reflect.DeepEqual(summaries, want) {
		require.Failf(t, "unexpected package import path summaries", "got %#v, want %#v", summaries, want)
	}

	if got := summarizeCodePackageImportPath(idx, "core", "missing"); len(got) != 0 {
		require.Failf(t, "missing import should return empty", "got %#v", got)
	}

	if got := summarizeCodePackageImportPath(idx, "core", " "); got != nil {
		require.Failf(t, "blank import path should return nil", "got %#v", got)
	}

	if got := summarizeCodePackageImportPath(idx, "missing", "context"); len(got) != 0 {
		require.Failf(t, "missing package should return empty", "got %#v", got)
	}
}

func TestSummarizeCodePackageImportPrefix(t *testing.T) {
	t.Parallel()

	idx := codeintel.Index{
		Files: []codeintel.File{
			{Path: "pkg/llm/a.go", Package: "llm"},
			{Path: "pkg/llm/b.go", Package: "llm"},
			{Path: "cmd/main.go", Package: "main"},
		},
		ImportEdges: []codeintel.ImportEdge{
			{From: "pkg/llm/a.go", Import: "github.com/tommoulard/atteler/pkg/events"},
			{From: "pkg/llm/b.go", Import: "github.com/tommoulard/atteler/pkg/events"},
			{From: "pkg/llm/b.go", Import: "context"},
			{From: "cmd/main.go", Import: "github.com/tommoulard/atteler/pkg/llm"},
		},
	}
	summaries := summarizeCodePackageImportPrefix(idx, "llm", "github.com/tommoulard/atteler/pkg/")

	want := []codeImportSummary{{Path: "github.com/tommoulard/atteler/pkg/events", Files: 2}}
	if !reflect.DeepEqual(summaries, want) {
		require.Failf(t, "unexpected package import prefix summaries", "got %#v, want %#v", summaries, want)
	}

	if got := summarizeCodePackageImportPrefix(idx, "llm", " "); got != nil {
		require.Failf(t, "blank prefix should return nil", "got %#v", got)
	}
}

func TestSummarizeCodePackageImports(t *testing.T) {
	t.Parallel()

	idx := codeintel.Index{
		Files: []codeintel.File{
			{Path: "pkg/llm/a.go", Package: "llm"},
			{Path: "pkg/llm/b.go", Package: "llm"},
			{Path: "cmd/main.go", Package: "main"},
		},
		ImportEdges: []codeintel.ImportEdge{
			{From: "pkg/llm/a.go", Import: "context"},
			{From: "pkg/llm/a.go", Import: "fmt"},
			{From: "pkg/llm/b.go", Import: "context"},
			{From: "cmd/main.go", Import: "context"},
		},
	}
	summaries := summarizeCodePackageImports(idx, "llm")

	want := []codeImportSummary{{Path: "context", Files: 2}, {Path: "fmt", Files: 1}}
	if !reflect.DeepEqual(summaries, want) {
		require.Failf(t, "unexpected package import summaries", "got %#v, want %#v", summaries, want)
	}

	if got := summarizeCodePackageImports(idx, "missing"); got != nil {
		require.Failf(t, "missing package should return nil", "got %#v", got)
	}
}

func TestCodeFileImports(t *testing.T) {
	t.Parallel()

	file := codeintel.File{Imports: []string{"fmt", "context", "errors"}}
	imports := codeFileImports(file)

	want := []string{"context", "errors", "fmt"}
	if !reflect.DeepEqual(imports, want) {
		require.Failf(t, "unexpected code file imports", "got %#v, want %#v", imports, want)
	}

	if got := codeFileImports(codeintel.File{}); got != nil {
		require.Failf(t, "empty imports should return nil", "got %#v", got)
	}
}

func TestCodeFileImportsForPath(t *testing.T) {
	t.Parallel()

	file := codeintel.File{Imports: []string{"context", "fmt", "context"}}
	imports := codeFileImportsForPath(file, "context")

	want := []string{"context", "context"}
	if !reflect.DeepEqual(imports, want) {
		require.Failf(t, "unexpected file imports by path", "got %#v, want %#v", imports, want)
	}

	if got := codeFileImportsForPath(file, " "); got != nil {
		require.Failf(t, "blank import path should return nil", "got %#v", got)
	}
}

func TestCodeFileImportsWithPrefix(t *testing.T) {
	t.Parallel()

	file := codeintel.File{Imports: []string{
		"github.com/tommoulard/atteler/pkg/llm",
		"context",
		"github.com/tommoulard/atteler/pkg/agent",
	}}
	imports := codeFileImportsWithPrefix(file, "github.com/tommoulard/atteler/pkg/")

	want := []string{"github.com/tommoulard/atteler/pkg/agent", "github.com/tommoulard/atteler/pkg/llm"}
	if !reflect.DeepEqual(imports, want) {
		require.Failf(t, "unexpected file imports by prefix", "got %#v, want %#v", imports, want)
	}

	if got := codeFileImportsWithPrefix(file, " "); got != nil {
		require.Failf(t, "blank prefix should return nil", "got %#v", got)
	}
}

func TestSummarizeCodeFileSymbols(t *testing.T) {
	t.Parallel()

	file := codeintel.File{Symbols: []codeintel.Symbol{
		{Kind: "func"},
		{Kind: "type"},
		{Kind: "func"},
		{Kind: "const"},
		{Kind: ""},
	}}
	summaries := summarizeCodeFileSymbols(file)

	want := []codeSymbolSummary{{Kind: "func", Count: 2}, {Kind: "const", Count: 1}, {Kind: "type", Count: 1}}
	if !reflect.DeepEqual(summaries, want) {
		require.Failf(t, "unexpected file symbol summaries", "got %#v, want %#v", summaries, want)
	}

	if got := summarizeCodeFileSymbols(codeintel.File{}); len(got) != 0 {
		require.Failf(t, "empty file should return empty summaries", "got %#v", got)
	}
}

func TestCodeFileSymbols(t *testing.T) {
	t.Parallel()

	file := codeintel.File{Symbols: []codeintel.Symbol{
		{Name: "Run", Kind: "func", File: "b.go", Line: 2},
		{Name: "Client", Kind: "type", File: "a.go", Line: 1},
		{Name: "Build", Kind: "func", File: "c.go", Line: 3},
	}}
	symbols := codeFileSymbols(file)

	want := []codeintel.Symbol{
		{Name: "Build", Kind: "func", File: "c.go", Line: 3},
		{Name: "Client", Kind: "type", File: "a.go", Line: 1},
		{Name: "Run", Kind: "func", File: "b.go", Line: 2},
	}
	if !reflect.DeepEqual(symbols, want) {
		require.Failf(t, "unexpected code file symbols", "got %#v, want %#v", symbols, want)
	}

	if got := codeFileSymbols(codeintel.File{}); got != nil {
		require.Failf(t, "empty symbols should return nil", "got %#v", got)
	}
}

func TestCodeFileSymbolsByName(t *testing.T) {
	t.Parallel()

	target, name, err := parseCodeFileSymbolFilterSpec("pkg/llm/client.go:Run", "code file symbol", "path:name")
	if err != nil {
		require.NoError(t, err)
	}

	if target != "pkg/llm/client.go" || name != "Run" {
		require.Failf(t, "unexpected parsed spec", "target=%q name=%q", target, name)
	}

	if _, _, err := parseCodeFileSymbolFilterSpec("pkg/llm/client.go", "code file symbol", "path:name"); err == nil {
		require.Fail(t, "expected parse error for missing name")
	}

	file := codeintel.File{Symbols: []codeintel.Symbol{
		{Name: "Run", Kind: "method", File: "b.go", Line: 2},
		{Name: "Client", Kind: "type", File: "a.go", Line: 1},
		{Name: "Run", Kind: "func", File: "a.go", Line: 3},
	}}
	symbols := codeFileSymbolsByName(file, "Run")

	want := []codeintel.Symbol{
		{Name: "Run", Kind: "func", File: "a.go", Line: 3},
		{Name: "Run", Kind: "method", File: "b.go", Line: 2},
	}
	if !reflect.DeepEqual(symbols, want) {
		require.Failf(t, "unexpected file symbols by name", "got %#v, want %#v", symbols, want)
	}

	if got := codeFileSymbolsByName(file, " "); got != nil {
		require.Failf(t, "blank name should return nil", "got %#v", got)
	}
}

func TestCodeFileSymbolsWithPrefix(t *testing.T) {
	t.Parallel()

	file := codeintel.File{Symbols: []codeintel.Symbol{
		{Name: "NewClient", Kind: "func", File: "b.go", Line: 2},
		{Name: "Client", Kind: "type", File: "a.go", Line: 1},
		{Name: "NewRegistry", Kind: "func", File: "c.go", Line: 3},
	}}
	symbols := codeFileSymbolsWithPrefix(file, "New")

	want := []codeintel.Symbol{
		{Name: "NewClient", Kind: "func", File: "b.go", Line: 2},
		{Name: "NewRegistry", Kind: "func", File: "c.go", Line: 3},
	}
	if !reflect.DeepEqual(symbols, want) {
		require.Failf(t, "unexpected file symbols by prefix", "got %#v, want %#v", symbols, want)
	}

	if got := codeFileSymbolsWithPrefix(file, " "); got != nil {
		require.Failf(t, "blank prefix should return nil", "got %#v", got)
	}
}

func TestCodeFileSymbolsByKindAndParseSpec(t *testing.T) {
	t.Parallel()

	target, kind, err := parseCodeFileSymbolKindSpec("pkg/llm/llm.go:func")
	if err != nil {
		require.NoError(t, err)
	}

	if target != "pkg/llm/llm.go" || kind != "func" {
		require.Failf(t, "unexpected parsed file symbol kind spec", "target=%q kind=%q", target, kind)
	}

	if _, _, err := parseCodeFileSymbolKindSpec("pkg/llm/llm.go"); err == nil {
		require.Fail(t, "expected parse error for missing kind")
	}

	file := codeintel.File{Symbols: []codeintel.Symbol{
		{Name: "Run", Kind: "func", File: "b.go", Line: 2},
		{Name: "Client", Kind: "type", File: "a.go", Line: 1},
		{Name: "Build", Kind: "func", File: "c.go", Line: 3},
	}}
	symbols := codeFileSymbolsByKind(file, "FUNC")

	want := []codeintel.Symbol{
		{Name: "Build", Kind: "func", File: "c.go", Line: 3},
		{Name: "Run", Kind: "func", File: "b.go", Line: 2},
	}
	if !reflect.DeepEqual(symbols, want) {
		require.Failf(t, "unexpected file symbols by kind", "got %#v, want %#v", symbols, want)
	}
}

func TestSummarizeCodeFiles(t *testing.T) {
	t.Parallel()

	root := filepath.Join("tmp", "repo")
	idx := codeintel.Index{Files: []codeintel.File{
		{Path: filepath.Join(root, "pkg", "b.go"), Package: "pkg", Imports: []string{"fmt"}, Symbols: []codeintel.Symbol{{Name: "B"}}},
		{Path: filepath.Join(root, "cmd", "a.go"), Package: "main", Imports: []string{"context", "fmt"}, Symbols: []codeintel.Symbol{{Name: "A"}, {Name: "Run"}}},
	}}
	files := summarizeCodeFiles(root, idx)

	want := []codePackageFile{
		{Path: "cmd/a.go", Package: "main", Symbols: 2, Imports: 2},
		{Path: "pkg/b.go", Package: "pkg", Symbols: 1, Imports: 1},
	}
	if !reflect.DeepEqual(files, want) {
		require.Failf(t, "unexpected code file summaries", "got %#v, want %#v", files, want)
	}
}

func TestFindAndFormatCodeFile(t *testing.T) {
	t.Parallel()

	root := filepath.Join("tmp", "repo")
	file := codeintel.File{
		Path:    filepath.Join(root, "pkg", "llm", "client.go"),
		Package: "llm",
		Imports: []string{"context", "fmt"},
		Symbols: []codeintel.Symbol{{Name: "Client", Kind: "type", Line: 12}},
	}
	idx := codeintel.Index{Files: []codeintel.File{file}}

	found, ok := findCodeFile(root, idx, "pkg/llm/client.go")
	if !ok || found.Path != file.Path {
		require.Failf(t, "expected to find code file", "found=%#v ok=%v", found, ok)
	}

	got := formatCodeFile(root, file)

	want := "path=pkg/llm/client.go	package=llm	imports=2	symbols=1"
	if got != want {
		require.Failf(t, "unexpected code file format", "got %q, want %q", got, want)
	}

	symbol := formatCodeFileSymbol(file.Symbols[0])
	if symbol != "Client	kind=type	line=12" {
		require.Failf(t, "unexpected code file symbol format", "got %q", symbol)
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

func TestCodeGraphDirectDependencies(t *testing.T) {
	t.Parallel()

	root := filepath.Join("tmp", "repo")
	idx := codeintel.Index{
		Files: []codeintel.File{
			{Path: filepath.Join(root, "pkg", "runner.go")},
			{Path: filepath.Join(root, "pkg", "worker.go")},
		},
		ImportEdges: []codeintel.ImportEdge{
			{From: filepath.Join(root, "pkg", "runner.go"), Import: "context"},
			{From: filepath.Join(root, "pkg", "runner.go"), Import: "fmt"},
			{From: filepath.Join(root, "pkg", "worker.go"), Import: "context"},
		},
	}
	graph := importGraphFromIndex(root, idx)

	deps := codeGraphDependencies(graph, root, "pkg/runner.go")

	wantDeps := []codegraph.NodeID{"context", "fmt"}
	if !reflect.DeepEqual(deps, wantDeps) {
		require.Failf(t, "unexpected direct deps", "got %#v, want %#v", deps, wantDeps)
	}

	rdeps := codeGraphReverseDependencies(graph, root, "context")

	wantRdeps := []codegraph.NodeID{"pkg/runner.go", "pkg/worker.go"}
	if !reflect.DeepEqual(rdeps, wantRdeps) {
		require.Failf(t, "unexpected direct reverse deps", "got %#v, want %#v", rdeps, wantRdeps)
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

func TestFormatWatchIteration(t *testing.T) {
	t.Parallel()

	started := time.Date(2026, 5, 2, 9, 30, 0, 0, time.UTC)
	got := formatWatchIteration(watch.IterationResult{
		Iteration: 1,
		StartedAt: started,
		Duration:  2 * time.Second,
		Findings: []watch.Finding{
			{Path: "TODO.md", Kind: watch.KindStaleTODO},
			{Path: "pkg/example/example.go", Kind: watch.KindMissingTest},
		},
	})

	want := "iteration=1\tfindings=2\tstarted=2026-05-02T09:30:00Z\tduration=2s"
	if got != want {
		require.Failf(t, "unexpected watch iteration format", "got %q, want %q", got, want)
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

func TestFormatReviewPlan(t *testing.T) {
	t.Parallel()

	plan, err := review.NewPlan(
		[]review.Reviewer{{Name: "alpha"}, {Name: "beta"}},
		[]string{"pkg/auth.go"},
		[]string{"tests pass"},
	)
	if err != nil {
		require.NoError(t, err)
	}

	got := formatReviewPlan(plan)
	for _, want := range []string{
		"reviewers:\n",
		"  - alpha\n",
		"paths:\n  - pkg/auth.go\n",
		"rounds:\n",
		"1\tindependent-review\tIndependent review\treviewers=alpha,beta",
		"cross_reviews:\n",
		"alpha -> beta",
		"beta -> alpha",
		"gates:\n  - tests pass\n",
	} {
		if !strings.Contains(got, want) {
			require.Failf(t, "formatted review plan missing content", "missing %q in:\n%s", want, got)
		}
	}
}

func TestReviewPlanDefaults(t *testing.T) {
	t.Parallel()

	plan, err := review.NewPlan(reviewPlanReviewers(nil), reviewPlanPaths(nil), nil)
	if err != nil {
		require.NoError(t, err)
	}

	got := formatReviewPlan(plan)
	for _, want := range []string{
		"quality-reviewer\tcategories=correctness,maintainability",
		"test-engineer\tcategories=tests",
		"paths:\n  - .\n",
		"behavioral diff reviewed",
	} {
		if !strings.Contains(got, want) {
			require.Failf(t, "default review plan missing content", "missing %q in:\n%s", want, got)
		}
	}
}

func TestFormatSpeculatePromptCacheEstimate(t *testing.T) {
	t.Parallel()

	plan, err := speculate.NewPlan([]string{"alpha", "beta"}, []string{"tests pass"})
	if err != nil {
		require.NoError(t, err)
	}

	estimate, err := speculate.EstimatePromptCacheReuse(speculateBranchPrompts(plan, "implement auth flow"))
	if err != nil {
		require.NoError(t, err)
	}

	got := formatSpeculatePromptCacheEstimate(estimate)
	for _, want := range []string{
		"prompt_cache:\n",
		"shared_prefix_bytes:",
		"reusable_prompt_bytes:",
		"reuse_ratio:",
		"alpha\tprompt_bytes=",
		"beta\tprompt_bytes=",
	} {
		if !strings.Contains(got, want) {
			require.Failf(t, "formatted speculate prompt cache missing content", "missing %q in:\n%s", want, got)
		}
	}

	if estimate.SharedPrefixBytes == 0 {
		require.FailNow(t, "expected shared branch prompt prefix")
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

func TestRunAgentMemoryCommandIndexesAndSearchesSelectedAgent(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()

	note := filepath.Join(dir, "note.txt")
	if err := os.WriteFile(note, []byte("OAuth callback retry memory"), 0o600); err != nil {
		require.NoError(t, err)
	}

	storePath := filepath.Join(dir, "agent-memory.json")

	err := runAgentMemoryCommand(dir, "reviewer", cliOptions{
		agentMemoryStorePath:  storePath,
		agentMemorySearch:     "callback retry",
		agentMemoryIndexFiles: stringListFlag{note},
		agentMemoryLimit:      positiveIntFlag{value: 1, set: true},
	})

	require.NoError(t, err)
	loaded, err := agentmemory.Load(storePath)
	require.NoError(t, err)
	results, err := loaded.Search("reviewer", "callback", 1)
	require.NoError(t, err)
	require.Len(t, results, 1)
	assert.Equal(t, filepath.Clean(note), results[0].Document.ID)
}

func TestFormatAgentMemoryResult(t *testing.T) {
	t.Parallel()

	got := formatAgentMemoryResult(agentmemory.Result{
		Document: agentmemory.Document{
			ID:       "docs/memory.md",
			Path:     "docs/memory.md",
			Metadata: map[string]string{"kind": "note"},
		},
		Score: 0.5,
	})

	want := "docs/memory.md\tscore=0.5000\tpath=docs/memory.md\tkind=note"
	if got != want {
		require.Failf(t, "unexpected agent memory result format", "got %q, want %q", got, want)
	}
}

func TestMergeArtifactsWritesMarkdown(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()

	artifactPath := filepath.Join(dir, "research.md")
	if err := os.WriteFile(artifactPath, []byte("research notes"), 0o600); err != nil {
		require.NoError(t, err)
	}

	outputPath := filepath.Join(dir, "merged.md")
	state := appState{
		cwd:           dir,
		sessionState:  session.New("gpt-test", nil),
		selectedAgent: "reviewer",
	}
	assert.True(t, state.sessionState.RecordArtifact("research.md", "research", "notes", "reviewer"))

	err := mergeArtifacts(t.Context(), state, outputPath, 1024)

	require.NoError(t, err)
	data, err := os.ReadFile(outputPath)
	require.NoError(t, err)

	out := string(data)
	assert.Contains(t, out, "# Merged Artifacts")
	assert.Contains(t, out, "## research.md")
	assert.Contains(t, out, "research notes")
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

func TestApplyRouteSelectionChoosesBudgetedFallbackChain(t *testing.T) {
	t.Parallel()

	opts := cliOptions{
		routeCandidates: rawStringListFlag{
			"openai/too-expensive,input=0.01,output=0.01,max=1000",
			"openai/fast,input=0.001,output=0.001,priority=0,max=1000",
			"openai/backup,input=0.001,output=0.001,priority=1,max=1000",
		},
		routeInputTokens:  positiveIntFlag{value: 100, set: true},
		routeOutputTokens: positiveIntFlag{value: 50, set: true},
		routeBudget:       floatFlag{value: 0.2, set: true},
	}
	state := selectionState{sessionState: session.New("", nil)}

	err := applyRouteSelection(opts, &state)

	require.NoError(t, err)
	assert.Equal(t, "openai/fast", state.selectedModel)
	assert.Equal(t, []string{"openai/backup"}, state.fallbackModels)
	assert.True(t, state.modelLocked)
	assert.Equal(t, "openai/fast", state.sessionState.DefaultModel)
}

func TestApplyRouteSelectionErrorsWhenBudgetFiltersAllCandidates(t *testing.T) {
	t.Parallel()

	opts := cliOptions{
		routeCandidates:   rawStringListFlag{"openai/too-expensive,input=0.01,output=0.01,max=1000"},
		routeInputTokens:  positiveIntFlag{value: 100, set: true},
		routeOutputTokens: positiveIntFlag{value: 50, set: true},
		routeBudget:       floatFlag{value: 0.01, set: true},
	}
	state := selectionState{sessionState: session.New("", nil)}

	err := applyRouteSelection(opts, &state)

	require.Error(t, err)
	assert.Contains(t, err.Error(), "no candidates fit")
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

func TestApplyFeedbackProposalsWritesConfigAndHistory(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	configPath := filepath.Join(dir, "atteler.yaml")
	historyPath := filepath.Join(dir, "feedback.md")

	if err := os.WriteFile(configPath, []byte(`agents:
  reviewer:
    system_prompt: Review code.
`), 0o600); err != nil {
		require.NoError(t, err)
	}

	saved := session.New("gpt-test", nil)
	if !saved.RecordNegativeKnowledge("skip regression tests", "hid an auth regression", "abc123", "reviewer") {
		require.FailNow(t, "expected negative knowledge to be recorded")
	}

	err := applyFeedbackProposals(saved, configPath, historyPath)

	require.NoError(t, err)
	cfg, _, err := config.LoadFiles([]string{configPath})
	require.NoError(t, err)
	require.Contains(t, cfg.Agents, "reviewer")
	assert.Contains(t, cfg.Agents["reviewer"].SystemPrompt, "Review code.")
	assert.Contains(t, cfg.Agents["reviewer"].SystemPrompt, "Feedback-derived guidance:")
	assert.Contains(t, cfg.Agents["reviewer"].SystemPrompt, "skip regression tests")

	historyData, err := os.ReadFile(historyPath)
	require.NoError(t, err)

	history := string(historyData)
	assert.Contains(t, history, "## Applied feedback")
	assert.Contains(t, history, "agent: reviewer")
	assert.Contains(t, history, "negative knowledge: skip regression tests -> hid an auth regression")
}

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

func TestParseSpawnAgentSpecs(t *testing.T) {
	t.Parallel()

	requests, err := parseSpawnAgentSpecs(rawStringListFlag{
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

func TestFormatTokenUsageSummary(t *testing.T) {
	t.Parallel()

	got := formatTokenUsageSummary(tokenUsage{InputTokens: 1500, CachedInputTokens: 500, OutputTokens: 42, Responses: 2})

	want := "tokens:\tin=1.5k\tcached=500\tout=42\tresponses=2"
	if got != want {
		require.Failf(t, "unexpected token usage summary", "got %q, want %q", got, want)
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

func TestApplyDebugEnvOptions(t *testing.T) {
	t.Parallel()

	values := map[string]string{
		"DEBUG_ATTELER_DOCTOR":                    "true",
		"DEBUG_ATTELER_DOCTOR_OFFLINE":            "true",
		"DEBUG_ATTELER_LIST_HOOK_EVENTS":          "true",
		"DEBUG_ATTELER_LIST_HOOK_EVENTS_JSON":     "true",
		"DEBUG_ATTELER_WATCH_SCAN":                "1",
		"DEBUG_ATTELER_WATCH_JSON":                "1",
		"DEBUG_ATTELER_REVIEW_PLAN":               "true",
		"DEBUG_ATTELER_AGENT_PERFORMANCE_SUMMARY": "true",
		"DEBUG_ATTELER_MCP_MANIFEST":              "mcp.yaml",
		"DEBUG_ATTELER_MCP_CAPABILITY":            "symbols",
		"DEBUG_ATTELER_MCP_SERVER":                "repo",
		"DEBUG_ATTELER_MCP_TOOL":                  "search",
		"DEBUG_ATTELER_MCP_TOOL_ARGS":             `{"query":"symbols"}`,
		"DEBUG_ATTELER_LSP_SYMBOLS":               "yes",
		"DEBUG_ATTELER_LSP_COMMAND":               "gopls",
		"DEBUG_ATTELER_LSP_ARGS":                  "serve",
		"DEBUG_ATTELER_LSP_FILE":                  "main.go",
		"DEBUG_ATTELER_LSP_WORKSPACE_SYMBOLS":     "Handler",
		"DEBUG_ATTELER_WATCH_MAX_ITERATIONS":      "3",
		"DEBUG_ATTELER_WATCH_INTERVAL_SECONDS":    "5",
	}
	opts := cliOptions{}

	applyDebugEnvOptions(&opts, func(name string) string { return values[name] })

	assert.True(t, opts.doctor)
	assert.True(t, opts.doctorOffline)
	assert.True(t, opts.listHookEvents)
	assert.True(t, opts.listHookEventsJSON)
	assert.True(t, opts.watchScan)
	assert.True(t, opts.watchJSON)
	assert.True(t, opts.reviewPlan)
	assert.True(t, opts.agentPerformanceSummary)
	assert.Equal(t, "mcp.yaml", opts.mcpManifestPath)
	assert.Equal(t, "symbols", opts.mcpCapability)
	assert.Equal(t, "repo", opts.mcpServerName)
	assert.Equal(t, "search", opts.mcpToolName)
	assert.JSONEq(t, `{"query":"symbols"}`, opts.mcpToolArgsJSON)
	assert.True(t, opts.lspSymbols)
	assert.Equal(t, "gopls", opts.lspCommand)
	assert.Equal(t, rawStringListFlag{"serve"}, opts.lspArgs)
	assert.Equal(t, "main.go", opts.lspFilePath)
	assert.Equal(t, "Handler", opts.lspWorkspaceSymbols)
	assert.Equal(t, 3, opts.watchMaxIterations.value)
	assert.True(t, opts.watchMaxIterations.set)
	assert.Equal(t, 5, opts.watchIntervalSeconds.value)
	assert.True(t, opts.watchIntervalSeconds.set)
}

func TestApplyDebugEnvOptionsDoesNotOverrideExplicitOptions(t *testing.T) {
	t.Parallel()

	opts := cliOptions{
		mcpManifestPath: "explicit.yaml",
		watchMaxIterations: positiveIntFlag{
			value: 2,
			set:   true,
		},
	}

	applyDebugEnvOptions(&opts, func(name string) string {
		switch name {
		case "DEBUG_ATTELER_MCP_MANIFEST":
			return "env.yaml"
		case "DEBUG_ATTELER_WATCH_MAX_ITERATIONS":
			return "9"
		default:
			return ""
		}
	})

	assert.Equal(t, "explicit.yaml", opts.mcpManifestPath)
	assert.Equal(t, 2, opts.watchMaxIterations.value)
}

func TestFormatShellContext(t *testing.T) {
	t.Parallel()

	t.Run("stdout only", func(t *testing.T) {
		t.Parallel()

		got := formatShellContext(shellResultMsg{
			command: "ls",
			stdout:  "a.go\nb.go\n",
		})
		assert.Equal(t, "$ ls\na.go\nb.go", got)
	})

	t.Run("stdout and stderr", func(t *testing.T) {
		t.Parallel()

		got := formatShellContext(shellResultMsg{
			command: "ls /nope",
			stdout:  "",
			stderr:  "ls: /nope: No such file or directory\n",
		})
		assert.Equal(t, "$ ls /nope\n[stderr]\nls: /nope: No such file or directory", got)
	})

	t.Run("includes error message", func(t *testing.T) {
		t.Parallel()

		got := formatShellContext(shellResultMsg{
			command: "false",
			err:     assert.AnError,
		})
		assert.Contains(t, got, "$ false")
		assert.Contains(t, got, "[error] "+assert.AnError.Error())
	})
}

func TestUpdateShellResult_ClearsCompletedTaskTimer(t *testing.T) {
	t.Parallel()

	startedAt := time.Date(2026, 5, 5, 12, 0, 0, 0, time.UTC)
	m := model{
		runningTaskLabel:   "command",
		runningTaskStarted: startedAt,
		runningTaskID:      3,
		sessionState:       session.New("gpt-test", nil),
	}

	nextModel, cmd := m.updateShellResult(shellResultMsg{
		completedAt: startedAt.Add(2 * time.Second),
		command:     "echo hi",
		stdout:      "hi\n",
	})
	next, ok := nextModel.(model)
	require.True(t, ok)
	require.NotNil(t, cmd)
	assert.False(t, next.waiting)
	assert.Empty(t, next.runningTaskLabel)
	assert.True(t, next.runningTaskStarted.IsZero())
}
