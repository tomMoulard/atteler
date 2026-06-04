package main

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/tommoulard/atteler/pkg/agent"
	"github.com/tommoulard/atteler/pkg/autonomy"
	"github.com/tommoulard/atteler/pkg/config"
	"github.com/tommoulard/atteler/pkg/contextpack"
	"github.com/tommoulard/atteler/pkg/contextref"
	"github.com/tommoulard/atteler/pkg/events"
	"github.com/tommoulard/atteler/pkg/feedback"
	"github.com/tommoulard/atteler/pkg/llm"
	"github.com/tommoulard/atteler/pkg/modelroute"
	"github.com/tommoulard/atteler/pkg/review"
	"github.com/tommoulard/atteler/pkg/session"
	"github.com/tommoulard/atteler/pkg/watch"
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

	err := mergeArtifacts(t.Context(), state, outputPath, "markdown", 1024)

	require.NoError(t, err)
	data, err := os.ReadFile(outputPath)
	require.NoError(t, err)

	out := string(data)
	assert.Contains(t, out, "# Merged Artifacts")
	assert.Contains(t, out, "## research.md")
	assert.Contains(t, out, "research notes")
}

func TestMergeArtifactsWritesJSONBundle(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	artifactPath := filepath.Join(dir, "research.md")
	require.NoError(t, os.WriteFile(artifactPath, []byte("research notes"), 0o600))

	outputPath := filepath.Join(dir, "merged.json")
	store := session.NewStore(filepath.Join(dir, "sessions"))
	sessionState := session.New("gpt-test", nil)
	assert.True(t, sessionState.RecordArtifact("research.md", "research", "notes", "reviewer"))
	require.NoError(t, store.Save(sessionState))

	state := appState{
		cwd:           dir,
		sessionStore:  store,
		sessionState:  sessionState,
		selectedAgent: "reviewer",
	}

	err := mergeArtifacts(t.Context(), state, outputPath, "json", 1024)
	require.NoError(t, err)

	data, err := os.ReadFile(outputPath)
	require.NoError(t, err)

	var out struct {
		Entries []struct {
			Path       string `json:"path"`
			SHA256     string `json:"sha256"`
			Content    string `json:"content"`
			ConsumedAt string `json:"consumed_at"`
		} `json:"entries"`
		SchemaVersion int `json:"schema_version"`
		Summary       struct {
			InputCount    int `json:"input_count"`
			IncludedCount int `json:"included_count"`
			SkippedCount  int `json:"skipped_count"`
			WarningCount  int `json:"warning_count"`
		} `json:"summary"`
	}
	require.NoError(t, json.Unmarshal(data, &out))
	assert.Equal(t, 1, out.SchemaVersion)
	assert.Equal(t, 1, out.Summary.InputCount)
	assert.Equal(t, 1, out.Summary.IncludedCount)
	assert.Zero(t, out.Summary.SkippedCount)
	assert.Equal(t, 1, out.Summary.WarningCount)
	require.Len(t, out.Entries, 1)
	assert.Equal(t, "research.md", out.Entries[0].Path)
	assert.NotEmpty(t, out.Entries[0].SHA256)
	assert.Equal(t, "research notes", out.Entries[0].Content)
	assert.NotEmpty(t, out.Entries[0].ConsumedAt)

	loaded, err := store.Load(sessionState.ID)
	require.NoError(t, err)
	require.Len(t, loaded.Artifacts, 1)
	require.NotNil(t, loaded.Artifacts[0].ConsumedAt)
	jsonConsumedAt, err := time.Parse(time.RFC3339Nano, out.Entries[0].ConsumedAt)
	require.NoError(t, err)
	assert.True(t, loaded.Artifacts[0].ConsumedAt.Equal(jsonConsumedAt))
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
		GateChecks: []review.GateCheck{{Name: "tests pass", Passed: false, Notes: "unit evidence missing"}},
	}

	got := formatReviewReport(report)
	for _, want := range []string{
		"reviewer: watch-scan\n",
		"summary: critical=0 high=0 medium=1 low=0 info=1 total=2\n",
		"findings:\n",
		"severity=medium\tcategory=maintainability\tpath=assets/blob.txt\tmessage=file is above threshold",
		"severity=info\tcategory=tests\tpath=pkg/example/example.go\tmessage=missing _test.go companion",
		"gates:\n",
		"tests pass: FAIL unit evidence missing",
	} {
		if !strings.Contains(got, want) {
			require.Failf(t, "formatted review report missing content", "missing %q in:\n%s", want, got)
		}
	}
}

func TestFormatReviewReportIncludesGateChecks(t *testing.T) {
	t.Parallel()

	report := review.Report{
		Reviewer: "watch-scan",
		GateChecks: []review.GateCheck{{
			Name:   "watch-quality-gate",
			Passed: false,
			Notes:  "new findings meet or exceed high severity (blocking_findings=1)",
		}},
	}

	got := formatReviewReport(report)

	assert.Contains(t, got, "gate_checks:\n")
	assert.Contains(t, got, "name=watch-quality-gate\tpassed=false\tnotes=new findings meet or exceed high severity (blocking_findings=1)")
	assert.Contains(t, got, "findings: none\n")
}

func TestWatchGateChecksToReview(t *testing.T) {
	t.Parallel()

	checks := watchGateChecksToReview(&watch.GateResult{
		Name:             "watch-quality-gate",
		Reason:           "new findings meet or exceed high severity",
		BlockingFindings: []watch.Finding{{Path: "pkg/new.go"}},
		Passed:           false,
	})

	require.Len(t, checks, 1)
	assert.Equal(t, "watch-quality-gate", checks[0].Name)
	assert.False(t, checks[0].Passed)
	assert.Equal(t, "new findings meet or exceed high severity (blocking_findings=1)", checks[0].Notes)
}

func TestBuildReviewContext_LoadsPathsAndInstructions(t *testing.T) { //nolint:paralleltest // captures process-global stderr.
	dir := t.TempDir()
	path := filepath.Join(dir, "auth.go")
	require.NoError(t, os.WriteFile(path, []byte("package auth\n"), 0o600))

	var got string

	captureStderr(t, func() {
		var err error

		got, err = buildReviewContext(
			t.Context(),
			[]string{"auth.go"},
			"focus on cancellation",
			contextref.Options{Root: dir, MaxFileBytes: 1024, MaxTotalBytes: 1024},
		)
		require.NoError(t, err)
	})

	assert.Contains(t, got, "Review instructions:\nfocus on cancellation")
	assert.Contains(t, got, `<file source="auth.go" truncated="false"`)
	assert.Contains(t, got, `scope="review"`)
	assert.Contains(t, got, "package auth")
}

func TestBuildReviewContextWithManifestReportsReviewReferences(t *testing.T) { //nolint:paralleltest // captures process-global stderr.
	dir := t.TempDir()
	path := filepath.Join(dir, "auth.go")
	require.NoError(t, os.WriteFile(path, []byte("package auth\n"), 0o600))

	var reviewContext configuredReferenceContext

	stderr := captureStderr(t, func() {
		var err error

		reviewContext, err = buildReviewContextWithManifest(
			t.Context(),
			[]string{"auth.go"},
			"",
			contextOptionsForProviderModel(contextref.Options{Root: dir, MaxFileBytes: 1024, MaxTotalBytes: 1024}, "openai", "gpt-test"),
		)
		require.NoError(t, err)
	})

	require.NotEmpty(t, reviewContext.Content)
	assert.Equal(t, 1, reviewContext.Manifest.IncludedCount)
	assert.Equal(t, 0, reviewContext.Manifest.RejectedCount)
	assert.Contains(t, reviewContext.Manifest.TokenEstimator, "openai-calibrated")
	require.Len(t, reviewContext.Manifest.Entries, 1)
	assert.Equal(t, contextref.ReferenceScopeReview, reviewContext.Manifest.Entries[0].Scope)
	assert.Contains(t, stderr, "reference loaded")
	assert.Contains(t, stderr, "reference manifest")
}

func TestBuildReviewContextWithManifestMarksLoadedReferencesOmittedOnFailure(t *testing.T) { //nolint:paralleltest // captures process-global stderr.
	dir := t.TempDir()
	root := filepath.Join(dir, "project")
	require.NoError(t, os.MkdirAll(root, 0o750))
	require.NoError(t, os.WriteFile(filepath.Join(root, "auth.go"), []byte("package auth\n"), 0o600))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "secret.go"), []byte("package secret\n"), 0o600))

	var reviewContext configuredReferenceContext

	stderr := captureStderr(t, func() {
		var err error

		reviewContext, err = buildReviewContextWithManifest(
			t.Context(),
			[]string{"auth.go", "../secret.go"},
			"",
			contextOptionsForProviderModel(contextref.Options{Root: root, MaxFileBytes: 1024, MaxTotalBytes: 1024}, "openai", "gpt-test"),
		)
		require.Error(t, err)
	})

	assert.Empty(t, reviewContext.Content)
	assert.Equal(t, 0, reviewContext.Manifest.IncludedCount)
	assert.Equal(t, 1, reviewContext.Manifest.OmittedCount)
	assert.Equal(t, 1, reviewContext.Manifest.RejectedCount)
	require.Len(t, reviewContext.Manifest.Entries, 2)
	assert.Equal(t, contextref.ReferenceDecisionOmitted, reviewContext.Manifest.Entries[0].PolicyDecision)
	assert.Contains(t, reviewContext.Manifest.Entries[0].PolicyReason, "review reference context omitted")
	assert.Equal(t, "omitted.omitted", reviewContext.Manifest.Entries[0].PolicyReasonCode)
	assert.Equal(t, contextref.ReferenceDecisionRejected, reviewContext.Manifest.Entries[1].PolicyDecision)
	assert.Contains(t, stderr, "reference loaded")
	assert.Contains(t, stderr, "reference omitted")
	assert.Contains(t, stderr, `"included_count":0`)
	assert.Contains(t, stderr, `"omitted_count":1`)
	assert.Contains(t, stderr, `"rejected_count":1`)
}

func TestReviewCompleterEmitsContextManifestBeforeBudgetFailure(t *testing.T) {
	t.Parallel()

	registry := llm.NewRegistry()
	registry.Register(modelPickerProvider{name: "openai", models: []string{"gpt-test"}})

	var eventLog bytes.Buffer

	referenceManifest := contextref.BuildReferenceManifest([]contextref.ReferenceEvent{
		{
			Source:         "auth.go",
			Kind:           "file",
			Scope:          contextref.ReferenceScopeReview,
			Location:       "local",
			PolicyDecision: contextref.ReferenceDecisionLoaded,
			PolicyReason:   "allowed by policy",
			TokenEstimate:  contextpack.TokenEstimate{Tokens: 2, ErrorBoundTokens: 1, UpperBoundTokens: 3},
			TokenEstimator: "test-estimator",
			Bytes:          12,
		},
	})

	completer := reviewCompleter{
		registry:          registry,
		agents:            agent.NewRegistry(nil),
		hookRunner:        events.NewRunnerWithLogger(nil, &eventLog),
		selectedModel:     "gpt-test",
		referenceManifest: referenceManifest,
		maxInputTokens:    1,
	}

	_, err := completer.Complete(
		t.Context(),
		"quality-reviewer",
		"system",
		"Review context:\n"+strings.Repeat("x", 80),
	)

	require.Error(t, err)
	assert.Contains(t, err.Error(), "review LLM budget")

	log := eventLog.String()
	assert.Contains(t, log, "context_manifest")
	assert.Contains(t, log, "agent=quality-reviewer")
	assert.Contains(t, log, "configured_reference_entry_count=1")
	assert.Contains(t, log, "fits_configured_token_budget=false")
}

func TestReviewCompleterIncludesAgentReferencesInPromptAndManifest(t *testing.T) { //nolint:paralleltest // captures process-global stderr.
	dir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(dir, "rubric.md"), []byte("review rubric"), 0o600))

	provider := &capturingIdleSuggestionProvider{model: "gpt-test", response: "ok"}
	registry := llm.NewRegistry()
	registry.Register(provider)

	var eventLog bytes.Buffer

	completer := reviewCompleter{
		registry:       registry,
		agents:         agent.NewRegistry(map[string]config.AgentConfig{"quality-reviewer": {References: []string{"rubric.md"}}}),
		hookRunner:     events.NewRunnerWithLogger(nil, &eventLog),
		contextOptions: contextref.Options{Root: dir},
		selectedModel:  "gpt-test",
		maxInputTokens: 10_000,
	}

	captureStderr(t, func() {
		_, err := completer.Complete(
			t.Context(),
			"quality-reviewer",
			"system",
			"Review context:\nprimary review surface",
		)
		require.NoError(t, err)
	})

	require.NotNil(t, provider.params)
	require.NotEmpty(t, provider.params.Messages)
	assertRunOnceRequestContains(t, *provider.params, "Autonomy: medium")
	assertRunOnceRequestContains(t, *provider.params, "<configured_references>")
	assertRunOnceRequestContains(t, *provider.params, `source="rubric.md"`)

	log := eventLog.String()
	assert.Contains(t, log, "context_manifest")
	assert.Contains(t, log, "configured_reference_entry_count=1")
	assert.Contains(t, log, "rubric.md")
}

func TestReviewCompleterEmitsRouteDecisionForModelRole(t *testing.T) {
	t.Parallel()

	provider := &capturingIdleSuggestionProvider{
		providerName: "openai",
		model:        "gpt-4.1-mini",
		response:     "review ok",
	}
	registry := llm.NewRegistry()
	registry.Register(provider)
	require.NoError(t, registry.SetModelRole("planner", llm.ModelRole{
		Preferred: "openai/gpt-4.1-mini",
	}))

	var eventLog bytes.Buffer

	completer := reviewCompleter{
		registry:       registry,
		agents:         agent.NewRegistry(nil),
		hookRunner:     events.NewRunnerWithLogger(nil, &eventLog),
		selectedModel:  "planner",
		maxInputTokens: 10_000,
	}

	got, err := completer.Complete(t.Context(), "quality-reviewer", "system", "review this")

	require.NoError(t, err)
	assert.Equal(t, "review ok", got)

	log := eventLog.String()
	assert.Contains(t, log, "event:route_decision")
	assert.Contains(t, log, "agent=quality-reviewer")
	assert.Contains(t, log, "model_role=planner")
	assert.Contains(t, log, "phase=estimated")
	assert.Contains(t, log, "phase=actual")
	assert.Contains(t, log, "selected=openai/gpt-4.1-mini")
	assert.Contains(t, log, "fallback_order=openai/gpt-4.1-mini")
	assert.Contains(t, log, "actual_selected=openai/gpt-4.1-mini")
}

func TestReviewCompleterPrependsAutonomyInstructions(t *testing.T) {
	t.Parallel()

	provider := &capturingIdleSuggestionProvider{model: "gpt-test", response: "ok"}
	registry := llm.NewRegistry()
	registry.Register(provider)

	completer := reviewCompleter{
		registry:       registry,
		agents:         agent.NewRegistry(nil),
		selectedModel:  "gpt-test",
		maxInputTokens: 10_000,
		autonomy:       autonomy.Low,
	}

	_, err := completer.Complete(t.Context(), "quality-reviewer", "system", "Review context:\nprimary review surface")
	require.NoError(t, err)
	require.NotNil(t, provider.params)

	assertRunOnceRequestContains(t, *provider.params, "Autonomy: low")
}

func TestRegistryCompleterEmitsContextManifestBeforeBudgetFailure(t *testing.T) {
	t.Parallel()

	registry := llm.NewRegistry()
	registry.Register(modelPickerProvider{name: "openai", models: []string{"gpt-test"}})

	var eventLog bytes.Buffer

	completer := registryCompleter{
		registry:       registry,
		hookRunner:     events.NewRunnerWithLogger(nil, &eventLog),
		maxInputTokens: 1,
	}

	_, err := completer.Complete(t.Context(), "gpt-test", "system", strings.Repeat("x", 80))

	require.Error(t, err)
	assert.Contains(t, err.Error(), "speculate LLM budget")

	log := eventLog.String()
	assert.Contains(t, log, "context_manifest")
	assert.Contains(t, log, "agent=gpt-test")
	assert.Contains(t, log, "fits_configured_token_budget=false")
	assert.Contains(t, log, "estimated_token_upper_bound=")
}

func TestRegistryCompleterPrependsAutonomyInstructions(t *testing.T) {
	t.Parallel()

	provider := &capturingIdleSuggestionProvider{model: "gpt-test", response: "ok"}
	registry := llm.NewRegistry()
	registry.Register(provider)

	completer := registryCompleter{
		registry:       registry,
		autonomy:       autonomy.Full,
		maxInputTokens: 10_000,
	}

	_, err := completer.Complete(t.Context(), "gpt-test", "system", "user")
	require.NoError(t, err)
	require.NotNil(t, provider.params)

	assertRunOnceRequestContains(t, *provider.params, "Autonomy: full")
	assertRunOnceRequestContains(t, *provider.params, "Do not merge PRs")
}

func TestFormatReviewRunResult(t *testing.T) {
	t.Parallel()

	result := review.Result{
		Report: review.Report{
			Reviewer: "aggregate-verdict",
			Findings: []review.Finding{
				{
					Severity:              review.SeverityHigh,
					Category:              review.CategoryCorrectness,
					Path:                  "pkg/auth.go",
					Line:                  12,
					EndLine:               13,
					Message:               "nil token panic",
					Evidence:              "token dereference lacks nil guard",
					SeverityRationale:     "can panic on missing token",
					SuggestedVerification: "add nil-token regression test",
					Confidence:            "medium",
					Provenance: []review.EvidenceSource{
						{Type: review.EvidenceModelJudgment, Source: "quality", Summary: "reviewer identified the panic"},
						{Type: review.EvidenceReviewContext, Source: "pkg/auth.go:12-13", Summary: "nil guard is absent"},
					},
					Dissent: []review.EvidenceSource{
						{Type: review.EvidenceModelJudgment, Source: "tests", Summary: "uncertain until a regression test is added"},
					},
				},
			},
			GateChecks: []review.GateCheck{
				{
					Name:   "tests pass",
					Passed: true,
					Notes:  "unit tests passed",
					Proof:  "go test ./... PASS",
					Provenance: []review.EvidenceSource{
						{Type: review.EvidenceCommandOutput, Source: "go test ./...", Summary: "PASS"},
					},
				},
			},
		},
		Session: review.Session{
			Reports: []review.Report{
				{Reviewer: "quality", Findings: []review.Finding{{Severity: review.SeverityMedium, Category: review.CategoryTests, Path: "pkg/auth_test.go", Message: "missing test"}}},
			},
			CrossReviews: []review.CrossReviewNote{
				{Reviewer: "tests", ReviewedReviewer: "quality", Notes: "keep finding"},
			},
			Errors: []review.RunError{
				{Stage: "aggregate-verdict", Reviewer: "review-judge", Message: "missing gate proof"},
			},
		},
	}

	got := formatReviewRunResult(result)
	for _, want := range []string{
		"independent_reports:\n",
		"reviewer=quality\tfindings=1",
		"cross_reviews:\n  - tests -> quality\tnotes=keep finding\n",
		"errors:\n  - stage=aggregate-verdict\treviewer=review-judge\tmessage=missing gate proof\n",
		"aggregate_report:\nreviewer: aggregate-verdict\n",
		"severity=high\tcategory=correctness\tpath=pkg/auth.go\tline=12-13\tmessage=nil token panic",
		"evidence=token dereference lacks nil guard",
		"severity_rationale=can panic on missing token",
		"suggested_verification=add nil-token regression test",
		"confidence=medium",
		"provenance=model-judgment:quality:reviewer identified the panic;review-context:pkg/auth.go:12-13:nil guard is absent",
		"dissent=model-judgment:tests:uncertain until a regression test is added",
		"gate_checks:\n  - name=tests pass\tpassed=true\tnotes=unit tests passed\tproof=go test ./... PASS\tprovenance=command-output:go test ./...:PASS",
	} {
		assert.Contains(t, got, want)
	}
}

func TestParseAndFormatRouteCandidate(t *testing.T) {
	t.Parallel()

	candidate, err := parseRouteCandidate("openai/gpt-mini,input=0.001,output=0.002,priority=2,max=1000,max_output=200,latency=500,ttft=100,capabilities=chat|tools|chat")
	if err != nil {
		require.NoError(t, err)
	}

	if candidate.Provider != "openai" || candidate.Name != "gpt-mini" {
		require.Failf(t, "unexpected route candidate id", "candidate = %+v", candidate)
	}

	assert.Equal(t, []string{modelroute.CapabilityChat, modelroute.CapabilityTools}, candidate.Capabilities)

	got := formatRouteCandidate(candidate, modelroute.RequestProfile{
		EstimatedInputTokens:  100,
		EstimatedOutputTokens: 50,
	})
	for _, want := range []string{
		"openai/gpt-mini",
		"cost=0.200000",
		"priority=2",
		"max_input=1000",
		"max_output=200",
		"latency_ms=500",
		"ttft_ms=100",
		"capabilities=chat,tools",
	} {
		if !strings.Contains(got, want) {
			require.Failf(t, "formatted route candidate missing content", "missing %q in %q", want, got)
		}
	}
}

func TestParseRouteCandidateUsesBuiltinMetadata(t *testing.T) {
	t.Parallel()

	candidate, err := parseRouteCandidate("openai/gpt-4.1-mini")
	require.NoError(t, err)

	assert.Equal(t, "openai", candidate.Provider)
	assert.Equal(t, "gpt-4.1-mini", candidate.Name)
	assert.Equal(t, modelroute.BuiltinCatalogVersion, candidate.MetadataVersion)
	assert.Positive(t, candidate.MaxInputTokens)
	assert.Positive(t, candidate.MaxOutputTokens)
	assert.Positive(t, candidate.InputTokenCost)
	assert.Positive(t, candidate.CachedInputTokenCost)
	assert.Positive(t, candidate.OutputTokenCost)
	assert.NotEmpty(t, candidate.MetadataSourceURL)
}

func TestParseRouteCandidateKeepsDeprecationMetadata(t *testing.T) {
	t.Parallel()

	candidate, err := parseRouteCandidate("anthropic/claude-sonnet-4-20250514")
	require.NoError(t, err)

	assert.True(t, candidate.Deprecated)

	got := formatRouteCandidate(candidate, modelroute.RequestProfile{EstimatedInputTokens: 1})
	assert.Contains(t, got, "deprecated=true")
}

func TestDecideRouteCandidatesAnnotatesCatalogMetadata(t *testing.T) {
	t.Parallel()

	candidate, err := parseRouteCandidate("openai/gpt-4.1-mini")
	require.NoError(t, err)

	decision := decideRouteCandidatesAt(
		[]modelroute.Candidate{candidate},
		modelroute.RequestProfile{EstimatedInputTokens: 10},
		time.Date(2026, time.May, 22, 12, 0, 0, 0, time.UTC),
	)

	assert.Equal(t, modelroute.BuiltinCatalogVersion, decision.CatalogVersion)
	assert.Contains(t, decision.Constraints, modelroute.ConstraintCatalogMetadata)
	assert.Contains(t, decision.Constraints, modelroute.ConstraintMetadataFreshness)

	got := formatRouteDecision(decision)
	assert.Contains(t, got, "catalog_version="+modelroute.BuiltinCatalogVersion)
	assert.Contains(t, got, "constraints=context_window,estimated_cost,catalog_metadata,metadata_freshness")
}

func TestParseRouteCandidateRejectsUnknownMetadataWithoutExplicitFields(t *testing.T) {
	t.Parallel()

	_, err := parseRouteCandidate("openai/not-real")

	require.Error(t, err)
	assert.Contains(t, err.Error(), "unknown or ambiguous model metadata")
	assert.Contains(t, err.Error(), "explicit key=value metadata")
}

func TestParseRouteCandidateRejectsAmbiguousBuiltinMetadataWithoutProvider(t *testing.T) {
	t.Parallel()

	_, err := parseRouteCandidate("gpt-5.5")

	require.Error(t, err)
	assert.Contains(t, err.Error(), "unknown or ambiguous model metadata")
}

func TestParseRouteCandidateAllowsExplicitManualMetadata(t *testing.T) {
	t.Parallel()

	candidate, err := parseRouteCandidate("openai/not-real,input=0.001,output=0.002,max=1000")

	require.NoError(t, err)
	assert.Equal(t, "openai/not-real", candidate.ID())
	assert.InDelta(t, 0.001, candidate.InputTokenCost, 0.000000001)
	assert.InDelta(t, 0.002, candidate.OutputTokenCost, 0.000000001)
	assert.Equal(t, 1000, candidate.MaxInputTokens)
}

func TestParseRouteCandidateRejectsUnknownManualCapability(t *testing.T) {
	t.Parallel()

	_, err := parseRouteCandidate("openai/not-real,input=0.001,capabilities=chat|teleport")

	require.Error(t, err)
	assert.Contains(t, err.Error(), `unknown capability "teleport"`)
	assert.Contains(t, err.Error(), "valid: text,chat,tools")
}

func TestParseRouteCandidateRejectsNegativeManualMetadata(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name      string
		candidate string
		wantError string
	}{
		{
			name:      "input cost",
			candidate: "openai/not-real,input=-0.001,output=0.002,max=1000",
			wantError: "route candidate input cost must be >= 0",
		},
		{
			name:      "output cost",
			candidate: "openai/not-real,input=0.001,output=-0.002,max=1000",
			wantError: "route candidate output cost must be >= 0",
		},
		{
			name:      "cached cost",
			candidate: "openai/not-real,input=0.001,output=0.002,cached=-0.001,max=1000",
			wantError: "route candidate cached input cost must be >= 0",
		},
		{
			name:      "cache write cost",
			candidate: "openai/not-real,input=0.001,output=0.002,cache_write=-0.001,max=1000",
			wantError: "route candidate cache write cost must be >= 0",
		},
		{
			name:      "max input",
			candidate: "openai/not-real,input=0.001,output=0.002,max=-1000",
			wantError: "route candidate max input must be >= 0",
		},
		{
			name:      "max output",
			candidate: "openai/not-real,input=0.001,output=0.002,max=1000,max_output=-10",
			wantError: "route candidate max output must be >= 0",
		},
		{
			name:      "latency",
			candidate: "openai/not-real,input=0.001,output=0.002,max=1000,latency=-10",
			wantError: "route candidate latency must be >= 0",
		},
		{
			name:      "ttft",
			candidate: "openai/not-real,input=0.001,output=0.002,max=1000,ttft=-10",
			wantError: "route candidate ttft must be >= 0",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			_, err := parseRouteCandidate(tc.candidate)

			require.Error(t, err)
			assert.Contains(t, err.Error(), tc.wantError)
		})
	}
}

func TestParseRouteCandidateRejectsNonFiniteManualCosts(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name      string
		candidate string
		wantError string
	}{
		{
			name:      "nan input",
			candidate: "openai/not-real,input=NaN,output=0.002,max=1000",
			wantError: "route candidate input cost must be finite",
		},
		{
			name:      "infinite output",
			candidate: "openai/not-real,input=0.001,output=+Inf,max=1000",
			wantError: "route candidate output cost must be finite",
		},
		{
			name:      "infinite cached",
			candidate: "openai/not-real,input=0.001,output=0.002,cached=Inf,max=1000",
			wantError: "route candidate cached input cost must be finite",
		},
		{
			name:      "infinite cache write",
			candidate: "openai/not-real,input=0.001,output=0.002,cache_write=Inf,max=1000",
			wantError: "route candidate cache write cost must be finite",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			_, err := parseRouteCandidate(tc.candidate)

			require.Error(t, err)
			assert.Contains(t, err.Error(), tc.wantError)
		})
	}
}

func TestFormatRouteDecisionIncludesRejectedCandidates(t *testing.T) {
	t.Parallel()

	candidates := []modelroute.Candidate{
		{Name: "small", Provider: "openai", MaxInputTokens: 10, InputTokenCost: 0.001},
		{Name: "ok", Provider: "openai", MaxInputTokens: 1000, InputTokenCost: 0.001},
	}
	decision := modelroute.Decide(candidates, modelroute.RequestProfile{EstimatedInputTokens: 100}, modelroute.Policy{}, nil)

	got := formatRouteDecision(decision)

	assert.Contains(t, got, "route_decision\tselected=openai/ok")
	assert.Contains(t, got, "constraints=context_window,estimated_cost")
	assert.Contains(t, got, "candidate\topenai/small\tstatus=rejected")
	assert.Contains(t, got, "rejected=context overflow")
	assert.Contains(t, got, "candidate\topenai/ok\tstatus=selected")
	assert.Contains(t, got, "fallback_order=openai/ok")
}

func TestFormatRouteDecisionIncludesMetadataAndTelemetryEvidence(t *testing.T) {
	t.Parallel()

	catalog := modelroute.BuiltinCatalog()
	candidate, ok := catalog.Candidate("anthropic/claude-sonnet-4-20250514")
	require.True(t, ok)

	telemetry := modelroute.NewTelemetry()
	observedAt := time.Date(2026, time.May, 22, 12, 0, 0, 0, time.UTC)
	telemetry.RecordFailure(candidate, modelroute.Failure{
		RetryAfter:  time.Second,
		Error:       "openai: HTTP 429: rate limited",
		Retryable:   true,
		RateLimited: true,
	}, observedAt)

	decision := modelroute.DecideAt(
		[]modelroute.Candidate{candidate},
		modelroute.RequestProfile{EstimatedInputTokens: 100},
		modelroute.Policy{},
		telemetry,
		observedAt,
	)

	got := formatRouteDecision(decision)

	assert.Contains(t, got, "metadata_version="+modelroute.BuiltinCatalogVersion)
	assert.Contains(t, got, "metadata_source=https://")
	assert.Contains(t, got, "deprecated=true")
	assert.Contains(t, got, "failure_count=1")
	assert.Contains(t, got, "rate_limit_count=1")
	assert.Contains(t, got, "rate_limit_until=")
	assert.Contains(t, got, modelroute.ReasonRateLimited)
}

func TestFormatRouteDecisionIncludesLatencyEvidence(t *testing.T) {
	t.Parallel()

	candidate := modelroute.Candidate{
		Name:              "fast",
		Provider:          "openai",
		InputTokenCost:    0.000001,
		ExpectedLatencyMS: 80,
		ExpectedTTFTMS:    30,
	}
	telemetry := modelroute.NewTelemetry()
	telemetry.Record(candidate, modelroute.ActualUsage{
		Latency:     40 * time.Millisecond,
		TTFT:        10 * time.Millisecond,
		InputTokens: 100,
	}, time.Date(2026, time.May, 22, 12, 0, 0, 0, time.UTC))

	decision := modelroute.Decide(
		[]modelroute.Candidate{candidate},
		modelroute.RequestProfile{EstimatedInputTokens: 100, Interactive: true},
		modelroute.Policy{},
		telemetry,
	)

	got := formatRouteDecision(decision)

	assert.Contains(t, got, "expected_latency_ms=80")
	assert.Contains(t, got, "expected_ttft_ms=30")
	assert.Contains(t, got, "observed_latency_ms=40")
	assert.Contains(t, got, "observed_ttft_ms=10")
}

func TestFormatRouteDecisionIncludesProfileModeEvidence(t *testing.T) {
	t.Parallel()

	decision := modelroute.Decide(
		[]modelroute.Candidate{{
			Name:           "batch-friendly",
			Provider:       "openai",
			InputTokenCost: 0.000001,
		}},
		modelroute.RequestProfile{EstimatedInputTokens: 100, Interactive: true, Batch: true},
		modelroute.Policy{},
		nil,
	)

	got := formatRouteDecision(decision)

	assert.Contains(t, got, "interactive=true")
	assert.Contains(t, got, "batch=true")
}

func TestFormatRouteDecisionIncludesActualCostDelta(t *testing.T) {
	t.Parallel()

	decision := modelroute.Decide(
		[]modelroute.Candidate{{Name: "gpt-test", Provider: "openai", InputTokenCost: 0.000001, OutputTokenCost: 0.000004}},
		modelroute.RequestProfile{EstimatedInputTokens: 100},
		modelroute.Policy{},
		nil,
	)
	decision = modelroute.DecisionWithActualUsage(decision, "", modelroute.ActualUsage{
		InputTokens:  100,
		OutputTokens: 10,
	})

	got := formatRouteDecision(decision)

	assert.Contains(t, got, "actual_cost=0.000140")
	assert.Contains(t, got, "actual_cost_delta=0.000040")
	assert.Contains(t, got, "actual_input_tokens=100")
	assert.Contains(t, got, "actual_output_tokens=10")
}

func TestFormatRouteDecisionIncludesCapabilityMetadata(t *testing.T) {
	t.Parallel()

	decision := modelroute.Decide(
		[]modelroute.Candidate{{
			Name:           "gpt-test",
			Provider:       "openai",
			InputTokenCost: 0.000001,
			Capabilities:   []string{modelroute.CapabilityChat, modelroute.CapabilityTools},
		}},
		modelroute.RequestProfile{EstimatedInputTokens: 100},
		modelroute.Policy{},
		nil,
	)

	got := formatRouteDecision(decision)

	assert.Contains(t, got, "capabilities=chat,tools")
}

func TestFormatRouteDecisionIncludesRequiredCapabilities(t *testing.T) {
	t.Parallel()

	decision := modelroute.Decide(
		[]modelroute.Candidate{{
			Name:           "gpt-test",
			Provider:       "openai",
			InputTokenCost: 0.000001,
			Capabilities:   []string{modelroute.CapabilityChat, modelroute.CapabilityTools},
		}},
		modelroute.RequestProfile{EstimatedInputTokens: 100},
		modelroute.Policy{RequiredCapabilities: []string{modelroute.CapabilityTools}},
		nil,
	)

	got := formatRouteDecision(decision)

	assert.Contains(t, got, "required_capabilities=tools")
	assert.Contains(t, got, "constraints=context_window,estimated_cost,routing_policy,required_capabilities")
}

func TestApplyRouteSelectionChoosesBudgetedFallbackChain(t *testing.T) {
	t.Parallel()

	input := routeModelsCommandInputFromOptions(cliOptions{
		routeCandidates: rawStringListFlag{
			"openai/too-expensive,input=0.01,output=0.01,max=1000",
			"openai/fast,input=0.001,output=0.001,priority=0,max=1000",
			"openai/backup,input=0.001,output=0.001,priority=1,max=1000",
		},
		routeInputTokens:  positiveIntFlag{value: 100, set: true},
		routeOutputTokens: positiveIntFlag{value: 50, set: true},
		routeBudget:       floatFlag{value: 0.2, set: true},
	})
	state := selectionState{sessionState: session.New("", nil)}

	err := applyRouteSelection(input, &state)

	require.NoError(t, err)
	assert.Equal(t, "openai/fast", state.selectedModel)
	assert.Equal(t, []string{"openai/backup"}, state.fallbackModels)
	assert.True(t, state.modelLocked)
	assert.Equal(t, "openai/fast", state.sessionState.DefaultModel)
}

func TestApplyRouteSelectionRespectsRequiredCapability(t *testing.T) {
	t.Parallel()

	input := routeModelsCommandInputFromOptions(cliOptions{
		routeCandidates: rawStringListFlag{
			"openai/chatty,input=0.001,output=0.001,max=1000,capabilities=chat",
			"openai/tooly,input=0.002,output=0.002,max=1000,capabilities=chat|tools",
		},
		routeInputTokens:          positiveIntFlag{value: 100, set: true},
		routeRequiredCapabilities: stringListFlag{modelroute.CapabilityTools},
	})
	state := selectionState{sessionState: session.New("", nil)}

	err := applyRouteSelection(input, &state)

	require.NoError(t, err)
	assert.Equal(t, "openai/tooly", state.selectedModel)
	assert.Empty(t, state.fallbackModels)
	assert.True(t, state.modelLocked)
}

func TestApplyRouteSelectionRejectsUnknownRequiredCapability(t *testing.T) {
	t.Parallel()

	input := routeModelsCommandInputFromOptions(cliOptions{
		routeCandidates:           rawStringListFlag{"openai/chatty,input=0.001,output=0.001,max=1000,capabilities=chat"},
		routeInputTokens:          positiveIntFlag{value: 100, set: true},
		routeRequiredCapabilities: stringListFlag{"teleport"},
	})
	state := selectionState{sessionState: session.New("", nil)}

	err := applyRouteSelection(input, &state)

	require.Error(t, err)
	assert.Contains(t, err.Error(), `unknown capability "teleport"`)
	assert.Contains(t, err.Error(), "valid: text,chat,tools")
}

func TestApplyRouteSelectionErrorsWhenBudgetFiltersAllCandidates(t *testing.T) {
	t.Parallel()

	input := routeModelsCommandInputFromOptions(cliOptions{
		routeCandidates:   rawStringListFlag{"openai/too-expensive,input=0.01,output=0.01,max=1000"},
		routeInputTokens:  positiveIntFlag{value: 100, set: true},
		routeOutputTokens: positiveIntFlag{value: 50, set: true},
		routeBudget:       floatFlag{value: 0.01, set: true},
	})
	state := selectionState{sessionState: session.New("", nil)}

	err := applyRouteSelection(input, &state)

	require.Error(t, err)
	assert.Contains(t, err.Error(), "no candidates fit")
}

func TestRunRouteModelsRequiresCandidate(t *testing.T) {
	t.Parallel()

	err := runRouteModels(routeModelsCommandInputFromOptions(cliOptions{routeInteractive: true}))

	require.Error(t, err)
	assert.Contains(t, err.Error(), "--route-candidate")
	assert.Contains(t, err.Error(), "atteler help providers")
}

func TestRouteModelsCommandInputFromOptions(t *testing.T) {
	t.Parallel()

	got := routeModelsCommandInputFromOptions(cliOptions{
		routeCandidates:       rawStringListFlag{"openai/fast,input=0.001,output=0.002,max=1000"},
		routeInputTokens:      positiveIntFlag{value: 100, set: true},
		routeOutputTokens:     positiveIntFlag{value: 50, set: true},
		routeCacheWriteTokens: positiveIntFlag{value: 20, set: true},
		routeBudget:           floatFlag{value: 0.25, set: true},
		routeCacheReuse:       floatFlag{value: 0.75, set: true},
		routeRequiredCapabilities: stringListFlag{
			modelroute.CapabilityTools,
			modelroute.CapabilityJSONSchema,
		},
		routeInteractive: true,
	})

	assert.Equal(t, []string{"openai/fast,input=0.001,output=0.002,max=1000"}, got.Candidates)
	assert.Equal(t, modelroute.RequestProfile{
		EstimatedInputTokens:      100,
		EstimatedOutputTokens:     50,
		EstimatedCacheWriteTokens: 20,
		Budget:                    0.25,
		Interactive:               true,
		PromptCacheReuseEstimate:  0.75,
	}, got.Profile)
	assert.Equal(t, []string{
		modelroute.CapabilityTools,
		modelroute.CapabilityJSONSchema,
	}, got.Policy.RequiredCapabilities)
}

func TestRouteModelsCacheWriteTokensAffectEstimatedCost(t *testing.T) {
	t.Parallel()

	input := routeModelsCommandInputFromOptions(cliOptions{
		routeCandidates: rawStringListFlag{
			"openai/cache-aware,input=0.001,cached=0.0001,cache_write=0.003,output=0.002,max=1000",
		},
		routeInputTokens:      positiveIntFlag{value: 100, set: true},
		routeOutputTokens:     positiveIntFlag{value: 10, set: true},
		routeCacheWriteTokens: positiveIntFlag{value: 20, set: true},
		routeCacheReuse:       floatFlag{value: 0.5, set: true},
	})

	candidates, profile, err := routeCandidatesAndProfile(input)
	require.NoError(t, err)

	decision := decideRouteCandidates(candidates, profile)
	require.Len(t, decision.Candidates, 1)
	assert.InDelta(t, 0.115, decision.Candidates[0].EstimatedCost, 0.000000001)

	got := formatRouteDecision(decision)
	assert.Contains(t, got, "estimated_input_tokens=100")
	assert.Contains(t, got, "estimated_output_tokens=10")
	assert.Contains(t, got, "estimated_cache_write_tokens=20")
	assert.Contains(t, got, "prompt_cache_reuse_estimate=0.5")
	assert.Contains(t, got, "estimated_cost=0.115000")
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

	estimator := contextpack.NewEstimator("", "openai/gpt-4.1")
	result := contextpack.CompactWithOptions(messages, contextpack.Options{
		Model:     "openai/gpt-4.1",
		MaxTokens: estimator.EstimateMessages(messages).UpperBoundTokens,
	})

	got := formatContextPackResult(result)
	for _, want := range []string{
		"compressed:",
		"omitted:",
		"tokens:",
		"upper=",
		"error_bound=",
		"estimator:",
		"openai-calibrated",
		"model=gpt-4.1",
		"policy:",
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
		RootCause: feedback.RootCauseClassification{
			Category: "evaluation-regression",
			Summary:  "Failed eval caught missed auth regression.",
			Signals:  []string{"failed-evaluation"},
		},
		TargetBehavior: "Run auth regression checks before approval.",
		RejectedAlternatives: []feedback.RejectedAlternative{{
			Alternative: "Append generic guidance",
			Reason:      "not auditable",
		}},
		Evidence:       []string{"evaluation: fail; score 1"},
		LinkedEvidence: []feedback.EvidenceLink{{Kind: feedback.VerificationKindEval, Reference: "eval-before.md", Description: "missed auth regression"}},
		Verification: []feedback.VerificationRecord{
			{Kind: feedback.VerificationKindEval, Phase: feedback.VerificationPhaseBefore, Outcome: "fail", Reference: "eval-before.md", Score: 1, Passed: false},
			{Kind: feedback.VerificationKindEval, Phase: feedback.VerificationPhaseAfter, Outcome: "pass", Reference: "eval-after.md", Score: 5, Passed: true},
		},
	})
	for _, want := range []string{
		"agent: reviewer\n",
		"confidence: 0.80\n",
		"root_cause: evaluation-regression",
		"target_behavior: Run auth regression checks before approval.\n",
		"action: Revise instructions.\n",
		"reason: Failed evaluations.\n",
		"rejected_alternatives:\n",
		"evidence:\n",
		"  - evaluation: fail; score 1\n",
		"linked_evidence:\n",
		"verification:\n",
		"phase=before\tkind=eval\tpassed=false",
		"phase=after\tkind=eval\tpassed=true",
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

	if !saved.RecordEvaluation("reviewer", "fail", "missed auth regression", "eval-before.md", 1) {
		require.FailNow(t, "expected before evaluation to be recorded")
	}

	if !saved.RecordEvaluation("reviewer", "pass", "auth regression covered", "eval-after.md", 5) {
		require.FailNow(t, "expected after evaluation to be recorded")
	}

	err := applyFeedbackProposals(saved, configPath, historyPath)

	require.NoError(t, err)
	cfg, _, err := config.LoadFiles([]string{configPath})
	require.NoError(t, err)
	require.Contains(t, cfg.Agents, "reviewer")
	assert.Equal(t, "Review code.", cfg.Agents["reviewer"].SystemPrompt)
	require.Len(t, cfg.Agents["reviewer"].FeedbackGuidance, 1)
	record := cfg.Agents["reviewer"].FeedbackGuidance[0]
	assert.NotEmpty(t, record.ID)
	assert.Equal(t, feedback.GuidanceStatusPending, record.Status)
	assert.Equal(t, saved.ID, record.SourceRun)
	assert.Equal(t, "feedback-apply", record.Reviewer)
	assert.Contains(t, record.Evidence, "negative knowledge: skip regression tests -> hid an auth regression")
	rendered := feedback.RenderSystemPrompt(cfg.Agents["reviewer"].SystemPrompt, cfg.Agents["reviewer"].FeedbackGuidance, record.CreatedAt)
	assert.Equal(t, "Review code.", rendered)

	historyData, err := os.ReadFile(historyPath)
	require.NoError(t, err)

	history := string(historyData)
	assert.Contains(t, history, "## Feedback guidance decisions")
	assert.Contains(t, history, "status: pending")
	assert.Contains(t, history, "source_run: "+saved.ID)
	assert.Contains(t, history, "agent: reviewer")
	assert.Contains(t, history, "negative knowledge: skip regression tests -> hid an auth regression")
}

func TestApplyFeedbackProposalsRestoresConfigWhenHistoryWriteFails(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	configPath := filepath.Join(dir, "atteler.yaml")
	historyPath := filepath.Join(dir, "history-dir")

	if err := os.WriteFile(configPath, []byte(`agents:
  reviewer:
    system_prompt: Review code.
`), 0o600); err != nil {
		require.NoError(t, err)
	}

	require.NoError(t, os.Mkdir(historyPath, 0o750))

	saved := session.New("gpt-test", nil)
	require.True(t, saved.RecordNegativeKnowledge("skip regression tests", "hid an auth regression", "abc123", "reviewer"))
	require.True(t, saved.RecordEvaluation("reviewer", "fail", "missed auth regression", "eval-before.md", 1))
	require.True(t, saved.RecordEvaluation("reviewer", "pass", "auth regression covered", "eval-after.md", 5))

	err := applyFeedbackProposals(saved, configPath, historyPath)

	require.Error(t, err)

	cfg, _, loadErr := config.LoadFiles([]string{configPath})
	require.NoError(t, loadErr)
	require.Contains(t, cfg.Agents, "reviewer")
	assert.Equal(t, "Review code.", cfg.Agents["reviewer"].SystemPrompt)
}

func TestApproveFeedbackGuidanceWritesConfigAndHistory(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	configPath := filepath.Join(dir, "atteler.yaml")
	historyPath := filepath.Join(dir, "feedback.md")

	if err := os.WriteFile(configPath, []byte(`agents:
  reviewer:
    system_prompt: Review code.
    feedback_guidance:
      - id: fg-approve
        status: pending
        source_run: run-123
        action: Always run focused tests.
        reason: Previous run skipped tests.
        evidence:
          - "evaluation: fail"
        confidence: 0.8
        reviewer: alice
        created_at: "2026-05-21T10:00:00Z"
        updated_at: "2026-05-21T10:00:00Z"
        audit:
          - at: "2026-05-21T10:00:00Z"
            actor: alice
            action: pending
`), 0o600); err != nil {
		require.NoError(t, err)
	}

	err := approveFeedbackGuidance(configPath, historyPath, "reviewer", "fg-approve")

	require.NoError(t, err)
	cfg, _, err := config.LoadFiles([]string{configPath})
	require.NoError(t, err)
	require.Contains(t, cfg.Agents, "reviewer")
	require.Len(t, cfg.Agents["reviewer"].FeedbackGuidance, 1)
	record := cfg.Agents["reviewer"].FeedbackGuidance[0]
	assert.Equal(t, feedback.GuidanceStatusApproved, record.Status)
	assert.Equal(t, "feedback-approve", record.Reviewer)
	assert.NotEmpty(t, record.Audit)
	assert.Equal(t, "feedback-approve", record.Audit[len(record.Audit)-1].Actor)
	assert.Contains(t, feedback.RenderSystemPrompt(cfg.Agents["reviewer"].SystemPrompt, cfg.Agents["reviewer"].FeedbackGuidance, record.UpdatedAt), "Always run focused tests.")

	historyData, err := os.ReadFile(historyPath)
	require.NoError(t, err)

	history := string(historyData)
	assert.Contains(t, history, "status: approved")
	assert.Contains(t, history, "reviewer: feedback-approve")
	assert.Contains(t, history, "id: fg-approve")
}

func TestApproveFeedbackGuidanceQuarantinesUnapprovableGuidance(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	configPath := filepath.Join(dir, "atteler.yaml")
	historyPath := filepath.Join(dir, "feedback.md")

	if err := os.WriteFile(configPath, []byte(`agents:
  reviewer:
    system_prompt: Review code.
    feedback_guidance:
      - id: fg-missing-source
        status: pending
        source_run: unknown
        action: Always run focused tests.
        reason: Previous run skipped tests.
        evidence:
          - "evaluation: fail"
        confidence: 0.8
        reviewer: alice
        created_at: "2026-05-21T10:00:00Z"
        updated_at: "2026-05-21T10:00:00Z"
        audit:
          - at: "2026-05-21T10:00:00Z"
            actor: alice
            action: pending
`), 0o600); err != nil {
		require.NoError(t, err)
	}

	err := approveFeedbackGuidance(configPath, historyPath, "reviewer", "fg-missing-source")

	require.NoError(t, err)
	cfg, _, err := config.LoadFiles([]string{configPath})
	require.NoError(t, err)
	require.Contains(t, cfg.Agents, "reviewer")
	require.Len(t, cfg.Agents["reviewer"].FeedbackGuidance, 1)
	record := cfg.Agents["reviewer"].FeedbackGuidance[0]
	assert.Equal(t, feedback.GuidanceStatusQuarantined, record.Status)
	assert.Equal(t, "feedback-approve", record.Reviewer)
	require.NotEmpty(t, record.Audit)
	assert.Equal(t, "feedback-approve", record.Audit[len(record.Audit)-1].Actor)
	assert.Equal(t, "Review code.", feedback.RenderSystemPrompt(cfg.Agents["reviewer"].SystemPrompt, cfg.Agents["reviewer"].FeedbackGuidance, record.UpdatedAt))

	historyData, err := os.ReadFile(historyPath)
	require.NoError(t, err)

	history := string(historyData)
	assert.Contains(t, history, "status: quarantined")
	assert.Contains(t, history, "reviewer: feedback-approve")
	assert.Contains(t, history, "id: fg-missing-source")
}

func TestRollbackFeedbackGuidanceWritesConfigAndHistory(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	configPath := filepath.Join(dir, "atteler.yaml")
	historyPath := filepath.Join(dir, "feedback.md")

	if err := os.WriteFile(configPath, []byte(`agents:
  reviewer:
    system_prompt: Review code.
    feedback_guidance:
      - id: fg-rollback
        status: approved
        source_run: run-123
        action: Always run focused tests.
        reason: Previous run skipped tests.
        evidence:
          - "evaluation: fail"
        confidence: 0.8
        reviewer: alice
        created_at: "2026-05-21T10:00:00Z"
        updated_at: "2026-05-21T10:00:00Z"
`), 0o600); err != nil {
		require.NoError(t, err)
	}

	err := rollbackFeedbackGuidance(configPath, historyPath, "reviewer", "fg-rollback", "superseded")

	require.NoError(t, err)
	cfg, _, err := config.LoadFiles([]string{configPath})
	require.NoError(t, err)
	require.Contains(t, cfg.Agents, "reviewer")
	require.Len(t, cfg.Agents["reviewer"].FeedbackGuidance, 1)
	record := cfg.Agents["reviewer"].FeedbackGuidance[0]
	assert.Equal(t, feedback.GuidanceStatusRolledBack, record.Status)
	assert.Equal(t, "superseded", record.RollbackReason)
	assert.NotEmpty(t, record.Audit)
	assert.Equal(t, "Review code.", feedback.RenderSystemPrompt(cfg.Agents["reviewer"].SystemPrompt, cfg.Agents["reviewer"].FeedbackGuidance, record.UpdatedAt))

	historyData, err := os.ReadFile(historyPath)
	require.NoError(t, err)

	history := string(historyData)
	assert.Contains(t, history, "status: rolled_back")
	assert.Contains(t, history, "rollback_reason: superseded")
	assert.Contains(t, history, "id: fg-rollback")
}
