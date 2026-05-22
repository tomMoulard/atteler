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
	assert.Equal(t, ReportSummary{Total: 13, Passed: 12, Failed: 1}, report.Summary)
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
			"type":     "object",
			"required": []any{"name", "score", "items"},
			"properties": map[string]any{
				"name":  map[string]any{"type": "string", "minLength": 3, "maxLength": 10},
				"score": map[string]any{"type": "number", "minimum": 0.8, "maximum": 1.0},
				"items": map[string]any{"type": "array", "minItems": 1, "maxItems": 2},
			},
		}},
		{ID: "schema-fail", Type: AssertionSchema, Schema: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"name":  map[string]any{"type": "string", "minLength": 8},
				"score": map[string]any{"type": "number", "maximum": 0.5},
				"items": map[string]any{"type": "array", "maxItems": 1},
			},
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
	assert.Contains(t, failed.Error, "maximum")
	assert.Contains(t, failed.Error, "maxItems")
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
	assert.Equal(t, ReportSummary{Total: 2, Passed: 1, Failed: 1}, report.Summary)
	failed := findResult(t, report, "exit_code")
	assert.Equal(t, AssertionExitCode, failed.Type)
	assert.Equal(t, "2", failed.ExpectedSnippet)
	assert.Equal(t, "1", failed.ActualSnippet)
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

	suitePath := filepath.Join(dir, "cli.eval.yaml")
	require.NoError(t, os.WriteFile(suitePath, []byte(`
version: 1
metadata:
  target_command: atteler demo
  owner: qa
actual: actual.yaml
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
`), 0o600))

	report, err := RunSuiteFile(suitePath, RunOptions{Now: time.Date(2026, 5, 22, 12, 0, 0, 0, time.UTC)})
	require.NoError(t, err)

	assert.True(t, report.Passed)
	assert.Equal(t, ReportSummary{Total: 4, Passed: 4}, report.Summary)
	assert.Equal(t, actualPath, report.ActualRef)
	assert.Equal(t, "qa", report.Metadata.Owner)
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
