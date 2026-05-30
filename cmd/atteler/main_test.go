package main

import (
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/tommoulard/atteler/pkg/agent"
	"github.com/tommoulard/atteler/pkg/config"
	atteval "github.com/tommoulard/atteler/pkg/eval"
	"github.com/tommoulard/atteler/pkg/githistory"
	"github.com/tommoulard/atteler/pkg/llm"
	"github.com/tommoulard/atteler/pkg/memory"
	attelerplugin "github.com/tommoulard/atteler/pkg/plugin"
	"github.com/tommoulard/atteler/pkg/promptcomplete"
	"github.com/tommoulard/atteler/pkg/session"
	attskill "github.com/tommoulard/atteler/pkg/skill"
)

const (
	testCodexModel     = "codex/gpt-5.5"
	testContextImport  = "context"
	testReviewerName   = "reviewer"
	testReasoningXHigh = "xhigh"
)

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
	summary.AgentLoopBudget = llm.AgentLoopBudget{MaxInputTokens: 100, MaxOutputTokens: 50, MaxCostMicros: 25_000}
	got = formatSessionSummary(summary)

	want = "abc\t2026-04-30T12:00:00Z\t3 messages\tagent=reviewer\tmodel=gpt-test\tbudget=in=100,out=50,costµ=25000\ttitle=Auth review\ttags=auth,review\t/tmp/abc.json"
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
	sessionState.AgentLoopBudget = llm.AgentLoopBudget{
		MaxWallTime:     time.Minute,
		MaxOutputBytes:  4096,
		MaxCostMicros:   25_000,
		MaxIterations:   3,
		MaxModelCalls:   4,
		MaxToolCalls:    5,
		MaxInputTokens:  100,
		MaxOutputTokens: 50,
		MaxTotalTokens:  150,
	}
	sessionState.RecordNegativeKnowledge("try cache bust", "broke auth", "abc123", "reviewer")
	sessionState.RecordEvaluation("reviewer", "pass", "caught auth regression", "eval.md", 5)
	sessionState.RecordArtifact("docs/research.md", "research", "auth notes", "reviewer")
	sessionState.MultiAgentRuns = []session.MultiAgentRun{{
		ID:        "run-1",
		ReceiptID: "receipt-1",
		Kind:      session.MultiAgentRunKindReview,
		Status:    session.MultiAgentRunStatusCompleted,
	}}

	out, err := formatSessionDetails(sessionState, "/tmp/session.json")
	if err != nil {
		require.NoError(t, err)
	}

	for _, want := range []string{
		"id: " + sessionState.ID,
		"path: /tmp/session.json",
		"title: Demo",
		"- demo",
		"agent_loop_budget:",
		"max_output_bytes: 4096",
		"max_cost_micros: 25000",
		"max_input_tokens: 100",
		"max_output_tokens: 50",
		"max_total_tokens: 150",
		"max_iterations: 3",
		"max_model_calls: 4",
		"max_tool_calls: 5",
		"max_wall_time: 1m0s",
		"role: user",
		"content: hello",
		"negative_knowledge:",
		"approach: try cache bust",
		"evaluations:",
		"outcome: pass",
		"artifacts:",
		"path: docs/research.md",
		"multi_agent_runs:",
		"receipt_id: receipt-1",
	} {
		if !strings.Contains(out, want) {
			require.Failf(t, "unexpected failure", "session details missing %q in:\n%s", want, out)
		}
	}
}

func TestSessionAgentLoopBudgetDetailsExposeEveryBudgetField(t *testing.T) {
	t.Parallel()

	assert.Equal(t, tagFieldsForType[llm.AgentLoopBudget]("json"), tagFieldsForType[agentLoopBudgetDetails]("yaml"))
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

//nolint:paralleltest // Temporarily redirects process stdout.
func TestRunInlineConfigCommandTemplateMatchesConfigTemplate(t *testing.T) {
	originalStdout := os.Stdout
	reader, writer, err := os.Pipe()
	require.NoError(t, err)

	os.Stdout = writer

	defer func() {
		os.Stdout = originalStdout
		_ = reader.Close()
		_ = writer.Close()
	}()

	handled, runErr := runInlineConfigCommand(cliOptions{printConfigTemplate: true})
	require.NoError(t, runErr)
	assert.True(t, handled)
	require.NoError(t, writer.Close())

	data, err := io.ReadAll(reader)
	require.NoError(t, err)
	assert.Equal(t, config.TemplateYAML(), string(data))
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

func TestGenerationForRequest_ModelModeMergesLikeReasoning(t *testing.T) {
	t.Parallel()

	generation := generationForRequest(
		generationSettings{ModelMode: "default"},
		generationSettings{ModelMode: "fast"},
		agentSelection{ok: true, agent: agent.Agent{ModelMode: "default"}},
	)

	assert.Equal(t, "fast", generation.ModelMode)
}

func TestApplyGenerationParams_AllowsExplicitZeroTemperature(t *testing.T) {
	t.Parallel()

	temperature := 0.0
	seed := 0
	params := llm.CompleteParams{}

	applyGenerationParams(&params, generationSettings{Temperature: &temperature, Seed: &seed, ModelMode: "fast", ReasoningLevel: "low"})

	if params.Temperature == nil || *params.Temperature != 0 {
		require.Failf(t, "unexpected failure", "temperature = %v, want explicit zero", params.Temperature)
	}

	if params.Seed == nil || *params.Seed != 0 {
		require.Failf(t, "unexpected failure", "seed = %v, want explicit zero", params.Seed)
	}

	assert.Equal(t, "fast", params.ModelMode)
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

	if got := err.Error(); !strings.Contains(got, "upper bound") || !strings.Contains(got, "generic-conservative") {
		require.Failf(t, "unexpected budget detail", "error = %q", got)
	}
}

func TestRequestMessagesForBudgetIncludesFinalReferenceAndToolContext(t *testing.T) {
	t.Parallel()

	messages := []llm.Message{{Role: llm.RoleUser, Content: "inspect repo"}}
	got := requestMessagesForBudget(
		"gpt-test",
		messages,
		agentSelection{},
		generationSettings{},
		"<configured_references>\nreference\n</configured_references>",
		true,
	)

	require.Len(t, got, 3)
	assert.Contains(t, got[0].Content, "You have the following tools available")
	assert.Contains(t, got[1].Content, "<configured_references>")
	assert.Equal(t, llm.RoleUser, got[2].Role)
	assert.Equal(t, "inspect repo", got[2].Content)
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
	resp := &llm.Response{
		Content:               "hi back",
		Provider:              "openai",
		Model:                 "gpt-test",
		Latency:               42 * time.Millisecond,
		FirstTokenLatency:     7 * time.Millisecond,
		InputTokens:           2,
		CachedInputTokens:     1,
		CacheWriteInputTokens: 4,
		OutputTokens:          3,
	}

	if err := saveRecordedResponse(path, params, []string{"backup"}, resp); err != nil {
		require.NoError(t, err)
	}

	got, err := loadRecordedResponse(path)
	if err != nil {
		require.NoError(t, err)
	}

	if got.Content != "hi back" ||
		got.Provider != "openai" ||
		got.Model != "gpt-test" ||
		got.Latency != 42*time.Millisecond ||
		got.FirstTokenLatency != 7*time.Millisecond ||
		got.InputTokens != 2 ||
		got.CachedInputTokens != 1 ||
		got.CacheWriteInputTokens != 4 ||
		got.OutputTokens != 3 {
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

func TestEvalOutputCommand_StructuredAssertionsReport(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	actual := filepath.Join(dir, "actual.json")
	report := filepath.Join(dir, "report.json")
	suite := filepath.Join(dir, "suite.eval.yaml")

	require.NoError(t, os.WriteFile(actual, []byte(`{"status":"bad","debug":"api_key=supersecret"}`), 0o600))
	require.NoError(t, os.WriteFile(suite, []byte(`
version: 1
assertions:
  - id: status
    type: json_path
    path: $.status
    equals: ok
  - id: no-secret
    type: not_contains
    value: api_key=supersecret
`), 0o600))

	err := evalOutputCommand(cliOptions{
		evalOutputPath:     actual,
		evalAssertionsPath: suite,
		evalReportPath:     report,
	})
	require.Error(t, err)

	data, err := os.ReadFile(report)
	require.NoError(t, err)

	reportData := string(data)
	assert.Contains(t, reportData, `"id": "status"`)
	assert.Contains(t, reportData, `"status": "fail"`)
	assert.Contains(t, reportData, "[REDACTED]")
	assert.NotContains(t, reportData, "supersecret")
}

func TestEvalOutputCommand_StructuredExitCodeOnlyReport(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	report := filepath.Join(dir, "report.json")
	suite := filepath.Join(dir, "suite.eval.yaml")
	require.NoError(t, os.WriteFile(suite, []byte(`
version: 1
exit_code: 5
`), 0o600))

	err := evalOutputCommand(cliOptions{
		evalAssertionsPath: suite,
		evalReportPath:     report,
		evalExitCode:       nonNegativeIntFlag{value: 5, set: true},
	})
	require.NoError(t, err)

	data, err := os.ReadFile(report)
	require.NoError(t, err)

	reportData := string(data)
	assert.Contains(t, reportData, `"id": "exit_code"`)
	assert.Contains(t, reportData, `"passed": true`)
}

func TestEvalOutputCommand_RejectsEvalReportWithoutTarget(t *testing.T) {
	t.Parallel()

	require.ErrorContains(t, evalOutputCommand(cliOptions{evalJSON: true}), "--eval-output")
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

func TestRunPluginEntrypointRequiresConfiguredPolicy(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	marker := filepath.Join(root, "executed")
	require.NoError(t, os.MkdirAll(filepath.Join(root, "bin"), 0o700))
	require.NoError(t, os.WriteFile(filepath.Join(root, "plugin.yaml"), []byte(`
name: runner
version: "1.0.0"
entrypoints:
  run: bin/run
entrypoint_args:
  run: []
permissions:
  filesystem:
    read:
      - "."
    write: []
  network:
    allow: false
    hosts: []
  shell:
    allow: true
  env: []
  secrets: []
  tools: []
output:
  stdout_max_bytes: 4096
  stderr_max_bytes: 4096
trust:
  enabled: true
  install_source: test
  checksum: sha256:test
  audit:
    - action: accepted
`), 0o600))
	scriptPath := filepath.Join(root, "bin", "run")
	require.NoError(t, os.WriteFile(scriptPath, []byte(`#!/bin/sh
printf executed > executed
`), 0o600))
	//nolint:gosec // Test fixture intentionally creates an executable plugin script.
	require.NoError(t, os.Chmod(scriptPath, 0o700))

	err := runPluginEntrypoint(t.Context(), []string{root}, nil, "runner/run", "", false, 5)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "plugins.policy must accept requested permissions")
	require.NoFileExists(t, marker)
}

func TestFormatPluginDescriptionIncludesSecurityMetadata(t *testing.T) {
	t.Parallel()

	out, err := formatPluginDescription(attelerplugin.Plugin{
		Manifest: attelerplugin.Manifest{
			Name:        "reviewer",
			Version:     "1.0.0",
			Description: "Reviews code",
			Entrypoints: map[string]string{
				"run": "bin/run",
			},
			EntrypointArgs: map[string][]attelerplugin.ArgumentSpec{
				"run": nil,
			},
			Permissions: &attelerplugin.PermissionSet{
				Filesystem: attelerplugin.FilesystemPermissions{
					Read: []string{"."},
				},
				Env: []string{"PATH"},
			},
			Output: &attelerplugin.OutputLimits{
				StdoutMaxBytes: 4096,
				StderrMaxBytes: 4096,
			},
			Trust: &attelerplugin.Trust{
				Enabled:       true,
				InstallSource: "test",
				Checksum:      "sha256:test",
				Audit: []attelerplugin.TrustAudit{{
					Action: "accepted",
					Actor:  "test",
				}},
			},
		},
		Root:         "/tmp/plugin",
		ManifestPath: "/tmp/plugin/plugin.yaml",
	})
	require.NoError(t, err)

	for _, want := range []string{
		"permissions:",
		"filesystem:",
		"env:",
		"entrypoint_args:",
		"output:",
		"stdout_max_bytes: 4096",
		"trust:",
		"install_source: test",
		"checksum: sha256:test",
	} {
		assert.Contains(t, out, want)
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
	sessionState.WorktreePath = dir
	require.NoError(t, store.Save(sessionState))

	mem, err := buildMemoryStore(store, cliOptions{memoryIndexFiles: stringListFlag{filePath}, memoryRepoPath: dir})
	require.NoError(t, err)
	results, err := mem.Search("oauth", 10)
	require.NoError(t, err)
	require.Len(t, results, 1)
	assert.Equal(t, filepath.Clean(filePath), results[0].Document.ID)
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
		AgentLoopBudget: llm.AgentLoopBudget{
			MaxCostMicros:   25_000,
			MaxInputTokens:  100,
			MaxOutputTokens: 50,
		},
		Tags:     []string{"auth", "regression"},
		Messages: []llm.Message{{Role: llm.RoleUser, Content: "hello"}},
		NegativeKnowledge: []session.NegativeKnowledge{
			{Approach: "timer", Reason: "storm"},
		},
		Evaluations: []session.AgentEvaluation{{Agent: "reviewer", Outcome: "pass"}},
		Artifacts:   []session.Artifact{{Path: "plan.md", Kind: "plan"}},
		MultiAgentRuns: []session.MultiAgentRun{{
			ID:     "run-1",
			Kind:   session.MultiAgentRunKindSpeculation,
			Status: session.MultiAgentRunStatusCompleted,
		}},
	}

	got := formatSessionDetailsSummary(sessionState, "/tmp/demo.json")
	for _, want := range []string{
		"id=demo",
		"path=/tmp/demo.json",
		"messages=1",
		"failures=1",
		"evaluations=1",
		"artifacts=1",
		"multi_agent_runs=1",
		"created_at=2026-05-01T13:15:00Z",
		"updated_at=2026-05-01T13:30:00Z",
		"title=Auth refresh",
		"agent=reviewer",
		"model=gpt-test",
		"budget=in=100,out=50,costµ=25000",
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
		RecentWindowDays:         30,
		EvaluationProvenance:     []session.ProvenanceCount{{Source: "human", Count: 2}},
		RubricVersions:           []session.RubricVersionCount{{RubricVersion: "review/v1", Count: 2}},
		Evaluators:               []session.EvaluatorCount{{Evaluator: "alice", Count: 2}},
		ScoreBuckets: []session.ScoreBucketSummary{{
			Source:                 "human",
			RubricVersion:          "review/v1",
			TaskType:               "code-review",
			Difficulty:             "medium",
			Model:                  "gpt-test",
			AgentVersion:           "reviewer@1",
			RoutingEligible:        false,
			ValidityReasons:        []string{"sample size 2 is below required 10", "recent sample size 1 is below required 3"},
			SampleSize:             2,
			AverageScore:           7.5,
			ConfidenceIntervalLow:  4.56,
			ConfidenceIntervalHigh: 10.44,
			StandardError:          1.5,
			Uncertainty:            "low_sample",
			RecentSampleSize:       1,
			RecentAverageScore:     9,
			PreviousSampleSize:     1,
			PreviousAverageScore:   6,
			RegressionDelta:        3,
			RegressionStatus:       "insufficient_history",
			LatestScoreAt:          time.Date(2026, 5, 2, 10, 0, 0, 0, time.UTC),
			RecentWindowStart:      time.Date(2026, 4, 2, 10, 0, 0, 0, time.UTC),
			ConfidenceSampleCount:  2,
			AverageConfidence:      0.8,
			DurationSampleCount:    1,
			AverageDurationMillis:  1234,
			CostSampleCount:        1,
			TotalCost:              0.012345,
			AverageCost:            0.012345,
		}},
		Outcomes: []session.OutcomeCount{{Outcome: "pass", Count: 1}, {Outcome: "fail", Count: 1}},
		NegativeKnowledgeBreakdown: []session.NegativeKnowledgeCategoryCount{
			{TaskType: "code-review", Severity: "medium", Count: 1},
		},
		Validity: session.PerformanceValidity{
			RoutingEligible:       false,
			Checks:                []string{"routing requires a versioned rubric"},
			Reasons:               []string{"no compatible scored bucket has at least 10 samples"},
			MinimumSampleSize:     10,
			MinimumRecentSamples:  3,
			MaximumStandardError:  5,
			MinimumMeanConfidence: 0.7,
		},
		LatestActivity: time.Date(2026, 5, 2, 10, 30, 0, 0, time.UTC),
	}

	got := formatAgentPerformanceSummary(summary)
	for _, want := range []string{
		"agent=reviewer",
		"evaluations=2",
		"failures=1",
		"negative_knowledge=1",
		"default_agent_sessions=1",
		"routing_eligible=false",
		"recency_window_days=30",
		"provenance=human:2",
		"rubrics=review/v1:2",
		"evaluators=alice:2",
		"score_buckets=source=human/rubric=review/v1/task=code-review/difficulty=medium/model=gpt-test/agent_version=reviewer@1/routing_eligible=false/sample=2/avg=7.50/ci95=4.56..10.44/stderr=1.50/uncertainty=low_sample/recent_sample=1/recent_avg=9.00/previous_sample=1/previous_avg=6.00/regression=insufficient_history/regression_delta=3.00/latest_score=2026-05-02T10:00:00Z/recent_since=2026-04-02T10:00:00Z/confidence_sample=2/avg_confidence=0.80/duration_sample=1/avg_duration_ms=1234.00/cost_sample=1/total_cost=0.012345/avg_cost=0.012345/validity_reasons=sample size 2 is below required 10|recent sample size 1 is below required 3",
		"scored=2",
		"avg_score=7.50",
		"min_score=6",
		"max_score=9",
		"outcomes=pass:1,fail:1",
		"negative_knowledge_breakdown=code-review/medium:1",
		"validity_eligible_buckets=0",
		"validity_min_sample_size=10",
		"validity_min_recent_samples=3",
		"validity_max_stderr=5.00",
		"validity_min_confidence=0.70",
		"validity_checks=routing requires a versioned rubric",
		"validity_reasons=no compatible scored bucket has at least 10 samples",
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
		Agent:           "reviewer",
		Outcome:         "pass",
		Notes:           "caught regression",
		Reference:       "eval.md",
		Source:          session.EvaluationSourceHarness,
		Evaluator:       "ci-eval",
		RubricVersion:   "review/v2",
		TaskType:        "code-review",
		Difficulty:      "medium",
		ExpectedOutcome: "catch regression",
		Model:           "gpt-test",
		AgentVersion:    "reviewer@abc123",
		SchemaVersion:   session.AgentEvaluationSchemaVersion,
		Score:           9,
		DurationMillis:  1234,
		Cost:            0.012345,
		Confidence:      0.85,
		CreatedAt:       time.Date(2026, 5, 1, 12, 45, 0, 0, time.UTC),
	}

	got := formatEvaluation(evaluation)
	for _, want := range []string{
		"agent=reviewer",
		"outcome=pass",
		"created_at=2026-05-01T12:45:00Z",
		"score=9",
		"source=harness",
		"evaluator=ci-eval",
		"rubric_version=review/v2",
		"task_type=code-review",
		"difficulty=medium",
		"expected_outcome=catch regression",
		"model=gpt-test",
		"agent_version=reviewer@abc123",
		"schema_version=1",
		"duration_millis=1234",
		"cost=0.012345",
		"confidence=0.85",
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
		Path:            "docs/research.md",
		LogicalPath:     "docs/decision.md",
		Kind:            "research",
		Summary:         "useful plan",
		SourceAgent:     "reviewer",
		SourceSessionID: "session-1",
		SHA256:          "abc123",
		ReviewStatus:    "approved",
		CreatedAt:       time.Date(2026, 5, 1, 12, 30, 0, 0, time.UTC),
		SizeBytes:       42,
	}

	got := formatArtifact(artifact)
	for _, want := range []string{
		"path=docs/research.md",
		"kind=research",
		"created_at=2026-05-01T12:30:00Z",
		"agent=reviewer",
		"logical_path=docs/decision.md",
		"session=session-1",
		"sha256=abc123",
		"size=42",
		"review=approved",
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

	err = recordEvaluationDetails(store, loaded, session.AgentEvaluation{
		Agent:          "planner",
		Outcome:        "pass",
		Source:         session.EvaluationSourceCI,
		Evaluator:      "ci-eval",
		RubricVersion:  "planning/v3",
		TaskType:       "planning",
		Difficulty:     "hard",
		Model:          "gpt-plan",
		AgentVersion:   "planner@2",
		Score:          88,
		Confidence:     0.9,
		DurationMillis: 2500,
		Cost:           0.02,
	})
	if err != nil {
		require.NoError(t, err)
	}

	loaded, err = store.Load(sessionState.ID)
	if err != nil {
		require.NoError(t, err)
	}

	require.Len(t, loaded.Evaluations, 2)
	assert.Equal(t, session.EvaluationSourceCI, loaded.Evaluations[1].Source)
	assert.Equal(t, "planning/v3", loaded.Evaluations[1].RubricVersion)
	assert.Equal(t, "planning", loaded.Evaluations[1].TaskType)
	assert.Equal(t, int64(2500), loaded.Evaluations[1].DurationMillis)
	assert.InEpsilon(t, 0.9, loaded.Evaluations[1].Confidence, 0.0001)

	cwd := t.TempDir()
	require.NoError(t, os.MkdirAll(filepath.Join(cwd, "docs"), 0o750))
	require.NoError(t, os.WriteFile(filepath.Join(cwd, "docs", "research.md"), []byte("research notes"), 0o600))

	err = recordArtifact(t.Context(), store, loaded, cwd, "docs/research.md", "research", "docs/research.md", "approved", "useful", "reviewer")
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
	assert.Equal(t, int64(len("research notes")), loaded.Artifacts[0].SizeBytes)
	assert.NotEmpty(t, loaded.Artifacts[0].SHA256)
	assert.Equal(t, loaded.ID, loaded.Artifacts[0].SourceSessionID)
	assert.Equal(t, "record-artifact", loaded.Artifacts[0].SourceCommand)
	assert.Equal(t, "atteler", loaded.Artifacts[0].SourceTool)
	assert.Equal(t, "approved", loaded.Artifacts[0].ReviewStatus)
}

func TestFormatSessionReuseCommand(t *testing.T) {
	t.Parallel()

	store := session.NewStore("/tmp/atteler sessions")
	command := formatSessionReuseCommand("/usr/local/bin/atteler", store, "session-1")

	assert.Contains(t, command, "/usr/local/bin/atteler")
	assert.Contains(t, command, "--session-dir")
	assert.Contains(t, command, shellQuote("/tmp/atteler sessions"))
	assert.Contains(t, command, "--session-id session-1")
}

func TestInitRTKPluginWritesManifestAndScripts(t *testing.T) {
	t.Parallel()

	dir := filepath.Join(t.TempDir(), "rtk")
	require.NoError(t, initRTKPlugin(dir))

	manifest, err := attelerplugin.LoadDir(dir)
	require.NoError(t, err)
	assert.Equal(t, "rtk", manifest.Name)
	assert.Contains(t, manifest.Capabilities, "token-optimization")
	assert.Equal(t, "bin/init-codex", manifest.Entrypoints["init-codex"])
	require.NotNil(t, manifest.Permissions)
	assert.Equal(t, []string{"."}, manifest.Permissions.Filesystem.Read)
	assert.True(t, manifest.Permissions.Shell.Allow)
	assert.Contains(t, manifest.Permissions.Env, "PATH")
	require.NotNil(t, manifest.Output)
	assert.Equal(t, 65536, manifest.Output.StdoutMaxBytes)
	require.NotNil(t, manifest.Trust)
	assert.True(t, manifest.Trust.Enabled)
	assert.Equal(t, "atteler plugins init-rtk", manifest.Trust.InstallSource)
	_, ok := manifest.EntrypointArgs["init-codex"]
	assert.True(t, ok)

	snippet := rtkPluginConfigSnippet(dir)
	assert.Contains(t, snippet, "policy:")
	assert.Contains(t, snippet, "trusted_install_sources:")
	assert.Contains(t, snippet, "atteler plugins init-rtk")

	script, err := os.ReadFile(filepath.Join(dir, "bin", "init-codex"))
	require.NoError(t, err)
	assert.Contains(t, string(script), "rtk init -g --codex")

	info, err := os.Stat(filepath.Join(dir, "bin", "init-codex"))
	require.NoError(t, err)
	assert.NotZero(t, info.Mode()&0o100)
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

	atSuggestions := promptcomplete.SuggestAll(promptcomplete.Context{
		Input:  "ask @rev",
		Cursor: len("ask @rev"),
		Agents: promptAgentCandidates(registry),
	}, promptcomplete.Options{})
	require.NotEmpty(t, atSuggestions)
	assert.Equal(t, "@"+testReviewerName, atSuggestions[0].Text)
	assert.Equal(t, "iewer", atSuggestions[0].Suffix)

	formatted := formatPromptSuggestions(suggestions[:1])
	for _, want := range []string{
		"text: " + testReviewerName + "\n",
		"suffix: iewer\n",
		"kind: agent\n",
		"source: configured agents\n",
		"replace: 4:7\n",
		"rank:\n",
		"  - prefix",
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
