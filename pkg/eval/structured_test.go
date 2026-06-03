package eval

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestRunSuite_MixedAssertionsAndRedactedReport(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(dir, "report.md"), []byte("artifact"), 0o600))

	minScore := 0.9
	maxScore := 1.0
	scoreTolerance := 0.05
	exitCode := 0
	actual := `{
		"status": "ok",
		"score": 0.96,
		"items": [{"name": "alpha"}],
		"citation.url": "https://example.test/source",
		"debug": "api_key=supersecret"
	}`
	suite := Suite{
		Metadata: Metadata{
			TargetCommand: "atteler eval output actual.json",
			Model:         "gpt-test",
			Agent:         "reviewer",
			InputFixture:  "prompt.txt",
			Environment:   map[string]string{"CI": "true"},
			CreatedAt:     "2026-05-22T00:00:00Z",
			UpdatedAt:     "2026-05-22T00:00:00Z",
			Owner:         "quality",
		},
		Assertions: []Assertion{
			{ID: "contains-status", Type: AssertionContains, Value: `"status"`},
			{ID: "forbidden", Type: AssertionNotContains, Value: "panic"},
			{ID: "regex", Type: AssertionRegex, Pattern: `"score"\s*:\s*0\.96`},
			{ID: "json-status", Type: AssertionJSONPath, Path: "$.status", Equals: "ok"},
			{ID: "json-quoted-key", Type: AssertionJSONPath, Path: `$['citation.url']`, Equals: "https://example.test/source"},
			{ID: "yaml-name", Type: AssertionYAMLPath, Path: "$.items[0].name", Equals: "alpha"},
			{ID: "yaml-quoted-key", Type: AssertionYAMLPath, Path: `$["citation.url"]`, Equals: "https://example.test/source"},
			{ID: "schema", Type: AssertionSchema, Schema: map[string]any{
				"type":     "object",
				"required": []any{"status", "score"},
				"properties": map[string]any{
					"status": map[string]any{"type": "string"},
					"score":  map[string]any{"type": "number"},
				},
			}},
			{ID: "score", Type: AssertionNumeric, Path: "$.score", Min: &minScore, Max: &maxScore},
			{ID: "score-tolerance", Type: AssertionNumeric, Path: "$.score", Equals: 1.0, Tolerance: &scoreTolerance},
			{ID: "artifact", Type: AssertionArtifactExists, Path: "report.md"},
			{ID: "exit", Type: AssertionExitCode, Equals: 0},
			{
				ID:          "missing-secret",
				Type:        AssertionContains,
				Value:       "api_key=othersecret",
				Severity:    SeverityCritical,
				Remediation: "Refresh the fixture only after reviewing secret handling.",
			},
		},
	}

	report, err := RunSuite(suite, RunOptions{
		ActualText:    actual,
		UseActualText: true,
		ArtifactRoot:  dir,
		ExitCode:      &exitCode,
		Now:           time.Date(2026, 5, 22, 12, 0, 0, 0, time.UTC),
	})
	require.NoError(t, err)

	assert.False(t, report.Passed)
	assert.Equal(t, 13, report.Summary.Total)
	assert.Equal(t, 12, report.Summary.Passed)
	assert.Equal(t, 1, report.Summary.Failed)
	assert.InEpsilon(t, 12.0/13.0, report.Summary.PassRate, 0.0001)
	assert.Equal(t, "quality", report.Metadata.Owner)

	failed := findResult(t, report, "missing-secret")
	assert.Equal(t, assertionStatusFail, failed.Status)
	assert.Equal(t, SeverityCritical, failed.Severity)
	assert.Contains(t, failed.ExpectedSnippet, "[REDACTED]")
	assert.Contains(t, failed.ActualSnippet, "[REDACTED]")
	assert.NotContains(t, failed.ExpectedSnippet, "othersecret")
	assert.NotContains(t, failed.ActualSnippet, "supersecret")
	assert.Equal(t, "Refresh the fixture only after reviewing secret handling.", failed.Remediation)

	reportJSON, err := report.JSON()
	require.NoError(t, err)
	assert.Contains(t, string(reportJSON), `"status": "fail"`)
	assert.NotContains(t, string(reportJSON), "supersecret")
}

func TestRunSuite_SchemaValidationConstraints(t *testing.T) {
	t.Parallel()

	report, err := RunSuite(Suite{Assertions: []Assertion{
		{ID: "schema-pass", Type: AssertionSchema, Schema: map[string]any{
			"type":                 "object",
			"required":             []any{"name", "score", "items"},
			"additionalProperties": false,
			"properties": map[string]any{
				"name":  map[string]any{"type": "string", "minLength": 3, "maxLength": 10, "pattern": "^alp", "enum": []string{"alpha"}},
				"score": map[string]any{"type": "number", "minimum": 0.8, "maximum": 1.0},
				"items": map[string]any{"type": "array", "minItems": 1, "maxItems": 2},
			},
		}},
		{ID: "schema-fail", Type: AssertionSchema, Schema: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"name":  map[string]any{"type": "string", "minLength": 8, "pattern": "^beta$", "enum": []string{"beta"}},
				"score": map[string]any{"type": "number", "maximum": 0.5},
				"items": map[string]any{"type": "array", "maxItems": 1},
			},
		}},
		{ID: "schema-additional-fail", Type: AssertionSchema, Schema: map[string]any{
			"type":                 "object",
			"additionalProperties": false,
		}},
		{ID: "schema-enum-definition-fail", Type: AssertionSchema, Schema: map[string]any{
			"enum": "alpha",
		}},
	}}, RunOptions{
		ActualText:    `{"name":"alpha","score":0.96,"items":["one","two"]}`,
		UseActualText: true,
		Now:           time.Date(2026, 5, 22, 12, 0, 0, 0, time.UTC),
	})
	require.NoError(t, err)

	assert.False(t, report.Passed)
	assert.Equal(t, assertionStatusPass, findResult(t, report, "schema-pass").Status)

	failed := findResult(t, report, "schema-fail")
	assert.Equal(t, assertionStatusFail, failed.Status)
	assert.Contains(t, failed.Error, "minLength")
	assert.Contains(t, failed.Error, "pattern")
	assert.Contains(t, failed.Error, "enum")
	assert.Contains(t, failed.Error, "maximum")
	assert.Contains(t, failed.Error, "maxItems")

	failed = findResult(t, report, "schema-additional-fail")
	assert.Equal(t, assertionStatusFail, failed.Status)
	assert.Contains(t, failed.Error, "additional property not allowed")

	failed = findResult(t, report, "schema-enum-definition-fail")
	assert.Equal(t, assertionStatusFail, failed.Status)
	assert.Contains(t, failed.Error, "enum must be an array")
}

func TestRunSuite_TopLevelExitCodeExpectation(t *testing.T) {
	t.Parallel()

	wantExit := 2
	actualExit := 1
	report, err := RunSuite(Suite{
		ExitCode: &wantExit,
		Assertions: []Assertion{
			{ID: "output", Type: AssertionContains, Value: "done"},
		},
	}, RunOptions{
		ActualText:    "done",
		UseActualText: true,
		ExitCode:      &actualExit,
		Now:           time.Date(2026, 5, 22, 12, 0, 0, 0, time.UTC),
	})
	require.NoError(t, err)

	assert.False(t, report.Passed)
	assert.Equal(t, 2, report.Summary.Total)
	assert.Equal(t, 1, report.Summary.Passed)
	assert.Equal(t, 1, report.Summary.Failed)
	assert.InEpsilon(t, 0.5, report.Summary.PassRate, 0.0001)
	failed := findResult(t, report, "exit_code")
	assert.Equal(t, AssertionExitCode, failed.Type)
	assert.Equal(t, "2", failed.ExpectedSnippet)
	assert.Equal(t, "1", failed.ActualSnippet)
}

func TestRunSuite_ExitCodeAssertionAcceptsExitCodeField(t *testing.T) {
	t.Parallel()

	actualExit := 0
	report, err := RunSuite(Suite{Assertions: []Assertion{
		{ID: "exit", Type: AssertionExitCode, ExitCode: &actualExit},
	}}, RunOptions{
		ActualText:    "ok",
		UseActualText: true,
		ExitCode:      &actualExit,
		Now:           time.Date(2026, 5, 22, 12, 0, 0, 0, time.UTC),
	})
	require.NoError(t, err)

	assert.True(t, report.Passed)
	assert.Equal(t, assertionStatusPass, findResult(t, report, "exit").Status)
}

func TestRunSuiteFile_LoadsStructuredAssertionFile(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	actualPath := filepath.Join(dir, "actual.yaml")
	require.NoError(t, os.WriteFile(actualPath, []byte("status: ok\nscore: 7\n"), 0o600))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "schema.yaml"), []byte(`
type: object
required: [status, score]
properties:
  status:
    type: string
    const: ok
  score:
    type: number
    minimum: 6
    maximum: 8
`), 0o600))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "workflow.yaml"), []byte(`
tool_calls:
  - name: read_file
    status: ok
    args:
      path: README.md
      limit: 4096
commands:
  - command: go test ./pkg/eval
    status: pass
    exit_code: 0
`), 0o600))

	suitePath := filepath.Join(dir, "cli.eval.yaml")
	require.NoError(t, os.WriteFile(suitePath, []byte(`
version: 1
metadata:
  target_command: atteler demo
  owner: qa
actual: actual.yaml
workflow_file: workflow.yaml
assertions:
  - id: status
    type: yaml_path
    path: $.status
    equals: ok
  - id: score
    type: numeric
    path: $.score
    min: 6
    max: 8
  - id: inline-schema
    type: schema
    schema:
      type: object
      required: [status]
  - id: schema-file
    type: schema
    schema_file: schema.yaml
  - id: tool
    type: tool_called
    name: read_file
  - id: tool-args
    type: tool_called
    name: read_file
    args:
      path: README.md
  - id: command
    type: command_run
    command: go test ./pkg/eval
    exit_code: 0
`), 0o600))

	report, err := RunSuiteFile(suitePath, RunOptions{Now: time.Date(2026, 5, 22, 12, 0, 0, 0, time.UTC)})
	require.NoError(t, err)

	assert.True(t, report.Passed)
	assert.Equal(t, 7, report.Summary.Total)
	assert.Equal(t, 7, report.Summary.Passed)
	assert.InEpsilon(t, 1.0, report.Summary.PassRate, 0.0001)
	assert.Equal(t, actualPath, report.ActualRef)
	assert.Equal(t, "qa", report.Metadata.Owner)
}

func TestRunSuite_UnorderedListsRequiredContentAndMetrics(t *testing.T) {
	t.Parallel()

	report, err := RunSuite(Suite{
		Metadata: Metadata{
			Provider:       "openai",
			Model:          "gpt-test",
			FixtureVersion: "fixture-v2",
		},
		Metrics: ReportMetrics{
			LatencyMillis: 1200,
			InputTokens:   10,
			OutputTokens:  5,
			Cost:          0.002,
		},
		Assertions: []Assertion{
			{ID: "unordered", Type: AssertionUnorderedList, Items: []string{"Fix bug", "Run tests", "Write report"}},
			{ID: "required", Type: AssertionContains, Required: []string{"Fix bug", "Run tests"}},
			{ID: "contains-field", Type: AssertionContains, Contains: "Write report"},
			{ID: "forbidden", Type: AssertionNotContains, Forbidden: []string{"panic", "TODO"}},
			{ID: "not-contains-field", Type: AssertionNotContains, NotContains: "deploy prod"},
		},
	}, RunOptions{
		ActualText:    "- Run tests\n- Write report\n- Fix bug\n",
		UseActualText: true,
		Now:           time.Date(2026, 5, 22, 12, 0, 0, 0, time.UTC),
	})
	require.NoError(t, err)

	assert.True(t, report.Passed)
	assert.Equal(t, "openai", report.Metadata.Provider)
	assert.Equal(t, "fixture-v2", report.Metadata.FixtureVersion)
	assert.Equal(t, int64(1200), report.Metrics.LatencyMillis)
	assert.Equal(t, 10, report.Metrics.InputTokens)
	assert.Equal(t, 5, report.Metrics.OutputTokens)
	assert.Equal(t, 15, report.Metrics.TotalTokens)
	assert.InEpsilon(t, 0.002, report.Metrics.Cost, 0.0001)
	assert.InEpsilon(t, 1.0, report.Summary.PassRate, 0.0001)
}

func TestRunSuite_OverrideTokenMetricsRecomputesTotal(t *testing.T) {
	t.Parallel()

	report, err := RunSuite(Suite{
		Metrics: ReportMetrics{
			InputTokens:  1,
			OutputTokens: 2,
			TotalTokens:  99,
		},
		Assertions: []Assertion{
			{ID: "ok", Type: AssertionContains, Value: "ok"},
		},
	}, RunOptions{
		ActualText:    "ok",
		UseActualText: true,
		Metrics: ReportMetrics{
			InputTokens:  10,
			OutputTokens: 5,
		},
		Now: time.Date(2026, 5, 22, 12, 0, 0, 0, time.UTC),
	})
	require.NoError(t, err)

	assert.True(t, report.Passed)
	assert.Equal(t, 10, report.Metrics.InputTokens)
	assert.Equal(t, 5, report.Metrics.OutputTokens)
	assert.Equal(t, 15, report.Metrics.TotalTokens)
}

func TestRunSuite_WorkflowAssertions(t *testing.T) {
	t.Parallel()

	exitCode := 0
	gatePassed := true
	absent := false
	report, err := RunSuite(Suite{Assertions: []Assertion{
		{ID: "tool", Type: AssertionToolCalled, Name: "read_file"},
		{ID: "tool-pattern", Type: AssertionToolCalled, Pattern: `read_.*`},
		{ID: "tool-args", Type: AssertionToolCalled, Name: "read_file", Args: map[string]any{"path": "README.md"}},
		{ID: "no-deploy", Type: AssertionToolCalled, Name: "deploy_prod", Exists: &absent},
		{ID: "no-secret-tool", Type: AssertionToolCalled, Name: "read_file", Args: map[string]any{"path": ".env"}, Exists: &absent},
		{ID: "file", Type: AssertionFileTouched, Path: "pkg/eval/structured.go", Action: "write"},
		{ID: "file-pattern", Type: AssertionFileTouched, Pattern: `pkg/eval/.*\.go`, Action: "write"},
		{ID: "no-secret-file", Type: AssertionFileTouched, Path: "secrets.env", Exists: &absent},
		{ID: "command", Type: AssertionCommandRun, Pattern: `go test ./pkg/eval`, ExitCode: &exitCode},
		{ID: "no-deploy-command", Type: AssertionCommandRun, Pattern: `deploy prod`, Exists: &absent},
		{ID: "gate", Type: AssertionGatePassed, Gate: "tests"},
		{ID: "no-security-waiver", Type: AssertionGatePassed, Gate: "security-waiver", Exists: &absent},
		{ID: "artifact-produced", Type: AssertionArtifactMade, Path: "reports/eval.json", Kind: "report"},
		{ID: "no-debug-artifact", Type: AssertionArtifactMade, Pattern: `debug/.*`, Exists: &absent},
	}}, RunOptions{
		ActualText:    "ok",
		UseActualText: true,
		Workflow: WorkflowTrace{
			ToolCalls: []WorkflowToolCall{{Name: "read_file", Status: "ok", Args: map[string]any{"path": "README.md", "limit": 4096}}},
			Files:     []WorkflowFile{{Path: "pkg/eval/structured.go", Action: "write"}},
			Commands:  []WorkflowCommand{{Command: "go test ./pkg/eval", ExitCode: &exitCode, Status: "pass"}},
			Gates:     []WorkflowGate{{Name: "tests", Passed: &gatePassed}},
			Artifacts: []WorkflowArtifact{{Path: "reports/eval.json", Kind: "report", Status: "ok"}},
		},
		Now: time.Date(2026, 5, 22, 12, 0, 0, 0, time.UTC),
	})
	require.NoError(t, err)

	assert.True(t, report.Passed)
	assert.Equal(t, 14, report.Summary.Passed)
}

func TestRunSuite_WorkflowAssertionRejectsInvalidPattern(t *testing.T) {
	t.Parallel()

	report, err := RunSuite(Suite{Assertions: []Assertion{
		{ID: "tool", Type: AssertionToolCalled, Pattern: "["},
	}}, RunOptions{
		ActualText:    "ok",
		UseActualText: true,
		Workflow:      WorkflowTrace{ToolCalls: []WorkflowToolCall{{Name: "read_file"}}},
		Now:           time.Date(2026, 5, 22, 12, 0, 0, 0, time.UTC),
	})
	require.NoError(t, err)

	assert.False(t, report.Passed)
	failed := findResult(t, report, "tool")
	assert.Equal(t, assertionStatusFail, failed.Status)
	assert.Contains(t, failed.Evidence, "pattern is invalid")
	assert.NotEmpty(t, failed.Error)
}

func TestRunSuite_GatePassedExistsFalseAllowsObservedFailedGate(t *testing.T) {
	t.Parallel()

	absent := false
	failedGate := false
	report, err := RunSuite(Suite{Assertions: []Assertion{
		{ID: "no-passed-waiver", Type: AssertionGatePassed, Gate: "security-waiver", Exists: &absent},
	}}, RunOptions{
		ActualText:    "ok",
		UseActualText: true,
		Workflow: WorkflowTrace{Gates: []WorkflowGate{
			{Name: "security-waiver", Status: "fail", Passed: &failedGate},
		}},
		Now: time.Date(2026, 5, 22, 12, 0, 0, 0, time.UTC),
	})
	require.NoError(t, err)

	assert.True(t, report.Passed)
	assert.Equal(t, assertionStatusPass, findResult(t, report, "no-passed-waiver").Status)
}

func TestRunSuite_GatePassedExistsFalseFailsObservedPassedGate(t *testing.T) {
	t.Parallel()

	absent := false
	passedGate := true
	report, err := RunSuite(Suite{Assertions: []Assertion{
		{ID: "no-passed-waiver", Type: AssertionGatePassed, Gate: "security-waiver", Exists: &absent},
	}}, RunOptions{
		ActualText:    "ok",
		UseActualText: true,
		Workflow: WorkflowTrace{Gates: []WorkflowGate{
			{Name: "security-waiver", Status: "pass", Passed: &passedGate},
		}},
		Now: time.Date(2026, 5, 22, 12, 0, 0, 0, time.UTC),
	})
	require.NoError(t, err)

	assert.False(t, report.Passed)
	failed := findResult(t, report, "no-passed-waiver")
	assert.Equal(t, assertionStatusFail, failed.Status)
	assert.Contains(t, failed.Evidence, "forbidden gate passed")
}

func TestRunSuite_GatePassedFindsLaterPassingObservation(t *testing.T) {
	t.Parallel()

	failedGate := false
	passedGate := true
	report, err := RunSuite(Suite{Assertions: []Assertion{
		{ID: "eventual-pass", Type: AssertionGatePassed, Gate: "tests"},
	}}, RunOptions{
		ActualText:    "ok",
		UseActualText: true,
		Workflow: WorkflowTrace{Gates: []WorkflowGate{
			{Name: "tests", Status: "fail", Passed: &failedGate},
			{Name: "tests", Status: "pass", Passed: &passedGate},
		}},
		Now: time.Date(2026, 5, 22, 12, 0, 0, 0, time.UTC),
	})
	require.NoError(t, err)

	assert.True(t, report.Passed)
	assert.Equal(t, assertionStatusPass, findResult(t, report, "eventual-pass").Status)
}

func TestRunSuite_WorkflowMissingDiagnosticsShowObservedSideEffects(t *testing.T) {
	t.Parallel()

	exitCode := 0
	gatePassed := false
	report, err := RunSuite(Suite{Assertions: []Assertion{
		{ID: "tool", Type: AssertionToolCalled, Name: "write_file"},
		{ID: "file", Type: AssertionFileTouched, Path: "pkg/eval/eval.go", Action: "write"},
		{ID: "command", Type: AssertionCommandRun, Command: "go test ./pkg/eval -run Missing"},
		{ID: "gate", Type: AssertionGatePassed, Gate: "lint"},
		{ID: "artifact", Type: AssertionArtifactMade, Path: "reports/final.json", Kind: "report"},
	}}, RunOptions{
		ActualText:    "ok",
		UseActualText: true,
		Workflow: WorkflowTrace{
			ToolCalls: []WorkflowToolCall{{Name: "read_file", Status: "ok", Args: map[string]any{"path": "README.md"}}},
			Files:     []WorkflowFile{{Path: "pkg/eval/structured.go", Action: "write", Status: "ok"}},
			Commands:  []WorkflowCommand{{Command: "go test ./pkg/eval", ExitCode: &exitCode, Status: "pass"}},
			Gates:     []WorkflowGate{{Name: "tests", Passed: &gatePassed, Status: "fail"}},
			Artifacts: []WorkflowArtifact{{Path: "reports/draft.json", Kind: "report", Status: "ok"}},
		},
		Now: time.Date(2026, 5, 22, 12, 0, 0, 0, time.UTC),
	})
	require.NoError(t, err)

	assert.False(t, report.Passed)
	assert.Contains(t, findResult(t, report, "tool").ActualSnippet, "read_file")
	assert.Contains(t, findResult(t, report, "tool").ActualSnippet, "README.md")
	assert.Contains(t, findResult(t, report, "file").ActualSnippet, "pkg/eval/structured.go")
	assert.Contains(t, findResult(t, report, "command").ActualSnippet, "go test ./pkg/eval")
	assert.Contains(t, findResult(t, report, "gate").ActualSnippet, "tests")
	assert.Contains(t, findResult(t, report, "artifact").ActualSnippet, "reports/draft.json")
}

func TestRunSuite_JudgeDecisionRequiresIndependentSignal(t *testing.T) {
	t.Parallel()

	judge := &JudgeDecision{
		Judge:         "rubric-bot",
		Model:         "gpt-judge",
		RubricVersion: "rubric/v1",
		InputRef:      "inputs/case.json",
		OutputRef:     "outputs/case.json",
		Decision:      "pass",
		Rationale:     "meets rubric api_key=supersecret",
		Score:         0.9,
		RecordedAt:    "2026-05-22T12:00:00Z",
	}
	_, err := RunSuite(Suite{Assertions: []Assertion{
		{ID: "judge", Type: AssertionJudgeDecision, Judge: judge, Equals: "pass"},
	}}, RunOptions{ActualText: "ok", UseActualText: true})
	require.Error(t, err)
	require.ErrorContains(t, err, "at least one non-judge")

	report, err := RunSuite(Suite{Assertions: []Assertion{
		{ID: "content", Type: AssertionContains, Value: "ok"},
		{ID: "judge", Type: AssertionJudgeDecision, Judge: judge, Equals: "pass"},
	}}, RunOptions{
		ActualText:    "ok",
		UseActualText: true,
		Now:           time.Date(2026, 5, 22, 12, 0, 0, 0, time.UTC),
	})
	require.NoError(t, err)

	assert.True(t, report.Passed)
	result := findResult(t, report, "judge")
	assert.Contains(t, result.Evidence, "input_ref=inputs/case.json")
	require.NotNil(t, result.Judge)
	assert.Equal(t, "rubric-bot", result.Judge.Judge)
	assert.Equal(t, "gpt-judge", result.Judge.Model)
	assert.Equal(t, "rubric/v1", result.Judge.RubricVersion)
	assert.Equal(t, "inputs/case.json", result.Judge.InputRef)
	assert.Equal(t, "outputs/case.json", result.Judge.OutputRef)
	assert.Equal(t, "pass", result.Judge.Decision)
	assert.Equal(t, "meets rubric api_key=[REDACTED]", result.Judge.Rationale)
	assert.InEpsilon(t, 0.9, result.Judge.Score, 0.0001)

	reportJSON, err := report.JSON()
	require.NoError(t, err)
	assert.Contains(t, string(reportJSON), `"judge": {`)
	assert.Contains(t, string(reportJSON), `"input_ref": "inputs/case.json"`)
	assert.NotContains(t, string(reportJSON), "supersecret")
}

func TestLoadSuiteFile_RejectsUnknownFields(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	suitePath := filepath.Join(dir, "typo.eval.yaml")
	require.NoError(t, os.WriteFile(suitePath, []byte(`
version: 1
assertions:
  - id: typo
    type: contains
    vale: ok
`), 0o600))

	_, err := LoadSuiteFile(suitePath)
	require.Error(t, err)
	assert.ErrorContains(t, err, "field vale")
}

func TestRunSuite_RejectsEmptySuiteAndTrailingJSON(t *testing.T) {
	t.Parallel()

	_, err := RunSuite(Suite{}, RunOptions{ActualText: "ok", UseActualText: true})
	require.Error(t, err)
	require.ErrorContains(t, err, "no assertions")

	report, err := RunSuite(Suite{Assertions: []Assertion{{
		ID:     "status",
		Type:   AssertionJSONPath,
		Path:   "$.status",
		Equals: "ok",
	}}}, RunOptions{
		ActualText:    `{"status":"ok"} trailing`,
		UseActualText: true,
		Now:           time.Date(2026, 5, 22, 12, 0, 0, 0, time.UTC),
	})
	require.NoError(t, err)
	assert.False(t, report.Passed)
	assert.Contains(t, findResult(t, report, "status").Error, "trailing")
}

func TestDiscoverSuiteFiles_StableOrder(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	nested := filepath.Join(dir, "nested")
	require.NoError(t, os.MkdirAll(nested, 0o750))

	want := []string{
		filepath.Join(dir, "alpha.eval.yaml"),
		filepath.Join(dir, "eval.json"),
		filepath.Join(nested, "beta.eval.yml"),
	}
	for _, path := range want {
		require.NoError(t, os.WriteFile(path, []byte("version: 1\n"), 0o600))
	}

	require.NoError(t, os.WriteFile(filepath.Join(dir, "ignore.yaml"), []byte("version: 1\n"), 0o600))

	got, err := DiscoverSuiteFiles(dir)
	require.NoError(t, err)

	assert.Equal(t, want, got)
}

func TestRunFixtureDir_TracksPerSuiteMetadata(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	writeEvalFile(t, filepath.Join(dir, "actual-one.txt"), "one ok")
	writeEvalFile(t, filepath.Join(dir, "actual-two.txt"), "two ok")
	writeEvalFile(t, filepath.Join(dir, "one.eval.yaml"), `
version: 1
metadata:
  target_command: atteler one
  model: gpt-one
  agent: reviewer
  input_fixture: one.prompt
  environment:
    CI: "true"
  created_at: "2026-05-22T00:00:00Z"
  updated_at: "2026-05-22T00:00:00Z"
  owner: qa-one
actual: actual-one.txt
assertions:
  - id: has-one
    type: contains
    value: one
`)
	writeEvalFile(t, filepath.Join(dir, "two.eval.yaml"), `
version: 1
metadata:
  target_command: atteler two
  owner: qa-two
actual: actual-two.txt
assertions:
  - id: has-two
    type: contains
    value: two
`)

	report, err := RunFixtureDir(dir, RunOptions{Now: time.Date(2026, 5, 22, 12, 0, 0, 0, time.UTC)})
	require.NoError(t, err)

	assert.True(t, report.Passed)
	assert.Len(t, report.Suites, 2)
	assert.Equal(t, filepath.Join(dir, "one.eval.yaml"), report.Suites[0].Path)
	assert.Equal(t, "qa-one", report.Suites[0].Metadata.Owner)
	assert.Equal(t, "gpt-one", report.Suites[0].Metadata.Model)
	assert.Equal(t, "true", report.Suites[0].Metadata.Environment["CI"])
	assert.Equal(t, filepath.Join(dir, "two.eval.yaml"), report.Suites[1].Path)
	assert.Equal(t, "qa-two", report.Suites[1].Metadata.Owner)

	reportJSON, err := report.JSON()
	require.NoError(t, err)
	assert.Contains(t, string(reportJSON), `"suites"`)
	assert.Contains(t, string(reportJSON), `"owner": "qa-one"`)
}

func TestRunSuiteFiles_DetectsFlakyAssertionGroups(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	writeEvalFile(t, filepath.Join(dir, "pass.txt"), "status ok")
	writeEvalFile(t, filepath.Join(dir, "fail.txt"), "status regressed")
	passSuite := filepath.Join(dir, "pass.eval.yaml")
	failSuite := filepath.Join(dir, "fail.eval.yaml")

	writeEvalFile(t, passSuite, `
version: 1
actual: pass.txt
assertions:
  - id: status
    type: contains
    value: ok
    flake_group: status-case
`)
	writeEvalFile(t, failSuite, `
version: 1
actual: fail.txt
assertions:
  - id: status
    type: contains
    value: ok
    flake_group: status-case
`)

	report, err := RunSuiteFiles([]string{passSuite, failSuite}, RunOptions{Now: time.Date(2026, 5, 22, 12, 0, 0, 0, time.UTC)})
	require.NoError(t, err)

	assert.False(t, report.Passed)
	assert.Equal(t, 2, report.Summary.Total)
	assert.Equal(t, 2, report.Summary.Failed)
	assert.Equal(t, 2, report.Summary.FlakeCount)
	assert.Equal(t, assertionStatusFlaky, report.Results[0].Status)
	assert.Equal(t, assertionStatusFlaky, report.Results[1].Status)
	assert.True(t, report.Results[0].Flaky)
	assert.True(t, report.Results[1].Flaky)
	assert.Contains(t, report.Results[0].Evidence, "non-deterministic assertion result")
	assert.Contains(t, report.Results[0].Evidence, "original evidence: actual output contained expected text")
	assert.Contains(t, report.Results[1].Evidence, "non-deterministic assertion result")
	assert.Contains(t, report.Results[1].Evidence, "original evidence: actual output did not contain required text")
}

func TestRunSuiteFiles_AppliesRunMetricsOnce(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	writeEvalFile(t, filepath.Join(dir, "one.txt"), "one ok")
	writeEvalFile(t, filepath.Join(dir, "two.txt"), "two ok")
	oneSuite := filepath.Join(dir, "one.eval.yaml")
	twoSuite := filepath.Join(dir, "two.eval.yaml")

	writeEvalFile(t, oneSuite, `
version: 1
metadata:
  provider: openai
  model: gpt-test
  fixture_version: fixture-v2
actual: one.txt
assertions:
  - id: one
    type: contains
    value: ok
`)
	writeEvalFile(t, twoSuite, `
version: 1
metadata:
  provider: openai
  model: gpt-test
  fixture_version: fixture-v2
actual: two.txt
assertions:
  - id: two
    type: contains
    value: ok
`)

	report, err := RunSuiteFiles([]string{oneSuite, twoSuite}, RunOptions{
		Metrics: ReportMetrics{
			LatencyMillis: 1200,
			InputTokens:   10,
			OutputTokens:  5,
			Cost:          0.002,
		},
		Now: time.Date(2026, 5, 22, 12, 0, 0, 0, time.UTC),
	})
	require.NoError(t, err)

	assert.True(t, report.Passed)
	assert.Equal(t, "openai", report.Metadata.Provider)
	assert.Equal(t, "gpt-test", report.Metadata.Model)
	assert.Equal(t, "fixture-v2", report.Metadata.FixtureVersion)
	assert.Equal(t, int64(1200), report.Metrics.LatencyMillis)
	assert.Equal(t, 10, report.Metrics.InputTokens)
	assert.Equal(t, 5, report.Metrics.OutputTokens)
	assert.Equal(t, 15, report.Metrics.TotalTokens)
	assert.InEpsilon(t, 0.002, report.Metrics.Cost, 0.0001)
}

func TestRunSuite_GoldenUpdateRequiresExplicitApproval(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	suite := Suite{GoldenPath: "golden.txt"}
	opts := RunOptions{
		ActualText:    "new golden\n",
		UseActualText: true,
		ArtifactRoot:  dir,
		Now:           time.Date(2026, 5, 22, 12, 0, 0, 0, time.UTC),
		UpdateGolden:  true,
	}

	_, err := RunSuite(suite, opts)
	require.Error(t, err)
	require.ErrorContains(t, err, "explicit review approval")
	assert.NoFileExists(t, filepath.Join(dir, "golden.txt"))

	opts.ApproveGoldenUpdate = true
	report, err := RunSuite(suite, opts)
	require.NoError(t, err)
	assert.True(t, report.Passed)
	assert.Equal(t, "new golden\n", readFile(t, filepath.Join(dir, "golden.txt")))

	report, err = RunSuite(suite, RunOptions{
		ActualText:    "regressed\n",
		UseActualText: true,
		ArtifactRoot:  dir,
		Now:           time.Date(2026, 5, 22, 12, 0, 0, 0, time.UTC),
	})
	require.NoError(t, err)
	assert.False(t, report.Passed)
	assert.Equal(t, assertionStatusFail, findResult(t, report, "golden").Status)

	_, err = RunSuite(Suite{}, RunOptions{
		ActualText:          "new golden\n",
		UseActualText:       true,
		ArtifactRoot:        dir,
		UpdateGolden:        true,
		ApproveGoldenUpdate: true,
	})
	require.Error(t, err)
	require.ErrorContains(t, err, "no golden path")

	_, err = RunSuite(Suite{GoldenPath: "missing-actual.txt"}, RunOptions{
		ArtifactRoot:        dir,
		UpdateGolden:        true,
		ApproveGoldenUpdate: true,
	})
	require.Error(t, err)
	require.ErrorContains(t, err, "requires actual output")
}

func findResult(t *testing.T, report Report, id string) AssertionResult {
	t.Helper()

	for i := range report.Results {
		result := report.Results[i]
		if result.ID == id {
			return result
		}
	}

	require.Failf(t, "result not found", "id=%s results=%+v", id, report.Results)

	return AssertionResult{}
}

func readFile(t *testing.T, path string) string {
	t.Helper()

	data, err := os.ReadFile(path)
	require.NoError(t, err)

	return string(data)
}

func writeEvalFile(t *testing.T, path, content string) {
	t.Helper()

	require.NoError(t, os.WriteFile(path, []byte(content), 0o600))
}
