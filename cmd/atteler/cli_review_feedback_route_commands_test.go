package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/tommoulard/atteler/pkg/config"
	"github.com/tommoulard/atteler/pkg/contextpack"
	"github.com/tommoulard/atteler/pkg/contextref"
	"github.com/tommoulard/atteler/pkg/feedback"
	"github.com/tommoulard/atteler/pkg/modelroute"
	"github.com/tommoulard/atteler/pkg/review"
	"github.com/tommoulard/atteler/pkg/session"
)

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

func TestBuildReviewContext_LoadsPathsAndInstructions(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	path := filepath.Join(dir, "auth.go")
	require.NoError(t, os.WriteFile(path, []byte("package auth\n"), 0o600))

	got, err := buildReviewContext(
		t.Context(),
		[]string{"auth.go"},
		"focus on cancellation",
		contextref.Options{Root: dir, MaxFileBytes: 1024, MaxTotalBytes: 1024},
	)
	require.NoError(t, err)

	assert.Contains(t, got, "Review instructions:\nfocus on cancellation")
	assert.Contains(t, got, `<file source="auth.go" truncated="false">`)
	assert.Contains(t, got, "package auth")
}

func TestFormatReviewRunResult(t *testing.T) {
	t.Parallel()

	result := review.Result{
		Report: review.Report{
			Reviewer: "aggregate-verdict",
			Findings: []review.Finding{
				{Severity: review.SeverityHigh, Category: review.CategoryCorrectness, Path: "pkg/auth.go", Line: 12, Message: "nil token panic"},
			},
		},
		Session: review.Session{
			Reports: []review.Report{
				{Reviewer: "quality", Findings: []review.Finding{{Severity: review.SeverityMedium, Category: review.CategoryTests, Path: "pkg/auth_test.go", Message: "missing test"}}},
			},
			CrossReviews: []review.CrossReviewNote{
				{Reviewer: "tests", ReviewedReviewer: "quality", Notes: "keep finding"},
			},
		},
	}

	got := formatReviewRunResult(result)
	for _, want := range []string{
		"independent_reports:\n",
		"reviewer=quality\tfindings=1",
		"cross_reviews:\n  - tests -> quality\tnotes=keep finding\n",
		"aggregate_report:\nreviewer: aggregate-verdict\n",
		"severity=high\tcategory=correctness\tpath=pkg/auth.go\tline=12\tmessage=nil token panic",
	} {
		assert.Contains(t, got, want)
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

func TestRunRouteModelsRequiresCandidate(t *testing.T) {
	t.Parallel()

	err := runRouteModels(cliOptions{routeInteractive: true})

	require.Error(t, err)
	assert.Contains(t, err.Error(), "--route-candidate")
	assert.Contains(t, err.Error(), "atteler help providers")
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
