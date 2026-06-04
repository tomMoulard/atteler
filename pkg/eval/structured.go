//nolint:cyclop,gocognit,goconst,gocritic,govet,modernize,nestif,perfsprint,revive,staticcheck,wrapcheck,wsl_v5 // Structured eval keeps explicit assertion, path, workflow, and schema logic.
package eval

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

// AssertionType identifies one structured output assertion.
type AssertionType string

const (
	AssertionExact          AssertionType = "exact"
	AssertionContains       AssertionType = "contains"
	AssertionNotContains    AssertionType = "not_contains"
	AssertionRegex          AssertionType = "regex"
	AssertionJSONPath       AssertionType = "json_path"
	AssertionYAMLPath       AssertionType = "yaml_path"
	AssertionUnorderedList  AssertionType = "unordered_list"
	AssertionSchema         AssertionType = "schema"
	AssertionNumeric        AssertionType = "numeric"
	AssertionToolCalled     AssertionType = "tool_called"
	AssertionFileTouched    AssertionType = "file_touched"
	AssertionCommandRun     AssertionType = "command_run"
	AssertionGatePassed     AssertionType = "gate_passed"
	AssertionArtifactMade   AssertionType = "artifact_produced"
	AssertionArtifactExists AssertionType = "artifact_exists"
	AssertionExitCode       AssertionType = "exit_code"
	AssertionJudgeDecision  AssertionType = "judge_decision"
)

// Severity labels how important one assertion is in the machine-readable
// report. All failed assertions make the report fail; consumers can use
// severity to prioritize remediation.
type Severity string

const (
	SeverityError    Severity = "error"
	SeverityWarning  Severity = "warning"
	SeverityCritical Severity = "critical"
)

const (
	reportVersion = 1

	assertionStatusPass  = "pass"
	assertionStatusFail  = "fail"
	assertionStatusFlaky = "flaky"
)

// Metadata records the provenance of a structured eval fixture.
type Metadata struct {
	TargetCommand  string            `json:"target_command,omitempty" yaml:"target_command,omitempty"`
	Provider       string            `json:"provider,omitempty" yaml:"provider,omitempty"`
	Model          string            `json:"model,omitempty" yaml:"model,omitempty"`
	Agent          string            `json:"agent,omitempty" yaml:"agent,omitempty"`
	InputFixture   string            `json:"input_fixture,omitempty" yaml:"input_fixture,omitempty"`
	FixtureVersion string            `json:"fixture_version,omitempty" yaml:"fixture_version,omitempty"`
	Environment    map[string]string `json:"environment,omitempty" yaml:"environment,omitempty"`
	CreatedAt      string            `json:"created_at,omitempty" yaml:"created_at,omitempty"`
	UpdatedAt      string            `json:"updated_at,omitempty" yaml:"updated_at,omitempty"`
	Owner          string            `json:"owner,omitempty" yaml:"owner,omitempty"`
}

// Suite is a YAML/JSON assertion file for one output eval.
type Suite struct {
	Version      int           `json:"version,omitempty" yaml:"version,omitempty"`
	Metadata     Metadata      `json:"metadata,omitempty" yaml:"metadata,omitempty"`
	Metrics      ReportMetrics `json:"metrics,omitempty,omitzero" yaml:"metrics,omitempty"`
	Workflow     WorkflowTrace `json:"workflow,omitempty,omitzero" yaml:"workflow,omitempty"`
	ActualPath   string        `json:"actual,omitempty" yaml:"actual,omitempty"`
	GoldenPath   string        `json:"golden,omitempty" yaml:"golden,omitempty"`
	WorkflowPath string        `json:"workflow_file,omitempty" yaml:"workflow_file,omitempty"`
	ExitCode     *int          `json:"exit_code,omitempty" yaml:"exit_code,omitempty"`
	Assertions   []Assertion   `json:"assertions,omitempty" yaml:"assertions,omitempty"`
}

// Assertion describes one typed check in a structured eval suite.
type Assertion struct {
	ID          string         `json:"id,omitempty" yaml:"id,omitempty"`
	Type        AssertionType  `json:"type" yaml:"type"`
	Severity    Severity       `json:"severity,omitempty" yaml:"severity,omitempty"`
	Value       string         `json:"value,omitempty" yaml:"value,omitempty"`
	Items       []string       `json:"items,omitempty" yaml:"items,omitempty"`
	Required    []string       `json:"required,omitempty" yaml:"required,omitempty"`
	Forbidden   []string       `json:"forbidden,omitempty" yaml:"forbidden,omitempty"`
	AllowExtra  bool           `json:"allow_extra,omitempty" yaml:"allow_extra,omitempty"`
	Pattern     string         `json:"pattern,omitempty" yaml:"pattern,omitempty"`
	Path        string         `json:"path,omitempty" yaml:"path,omitempty"`
	Name        string         `json:"name,omitempty" yaml:"name,omitempty"`
	Args        map[string]any `json:"args,omitempty" yaml:"args,omitempty"`
	Command     string         `json:"command,omitempty" yaml:"command,omitempty"`
	Gate        string         `json:"gate,omitempty" yaml:"gate,omitempty"`
	Kind        string         `json:"kind,omitempty" yaml:"kind,omitempty"`
	Action      string         `json:"action,omitempty" yaml:"action,omitempty"`
	Status      string         `json:"status,omitempty" yaml:"status,omitempty"`
	Equals      any            `json:"equals,omitempty" yaml:"equals,omitempty"`
	Contains    string         `json:"contains,omitempty" yaml:"contains,omitempty"`
	NotContains string         `json:"not_contains,omitempty" yaml:"not_contains,omitempty"`
	Matches     string         `json:"matches,omitempty" yaml:"matches,omitempty"`
	Min         *float64       `json:"min,omitempty" yaml:"min,omitempty"`
	Max         *float64       `json:"max,omitempty" yaml:"max,omitempty"`
	Tolerance   *float64       `json:"tolerance,omitempty" yaml:"tolerance,omitempty"`
	Schema      map[string]any `json:"schema,omitempty" yaml:"schema,omitempty"`
	SchemaPath  string         `json:"schema_file,omitempty" yaml:"schema_file,omitempty"`
	ExitCode    *int           `json:"exit_code,omitempty" yaml:"exit_code,omitempty"`
	Exists      *bool          `json:"exists,omitempty" yaml:"exists,omitempty"`
	FlakeGroup  string         `json:"flake_group,omitempty" yaml:"flake_group,omitempty"`
	Judge       *JudgeDecision `json:"judge,omitempty" yaml:"judge,omitempty"`
	Remediation string         `json:"remediation,omitempty" yaml:"remediation,omitempty"`
}

// RunOptions controls structured eval execution.
type RunOptions struct {
	ActualText          string
	UseActualText       bool
	ActualPath          string
	ExitCode            *int
	ArtifactRoot        string
	SuitePath           string
	Metrics             ReportMetrics
	Workflow            WorkflowTrace
	Now                 time.Time
	UpdateGolden        bool
	ApproveGoldenUpdate bool
}

// Report is the stable machine-readable outcome of a structured eval run.
type Report struct {
	Version   int               `json:"version"`
	Suite     string            `json:"suite,omitempty"`
	Passed    bool              `json:"passed"`
	RunAt     string            `json:"run_at,omitempty"`
	Metadata  Metadata          `json:"metadata,omitempty"`
	Metrics   ReportMetrics     `json:"metrics,omitempty,omitzero"`
	Summary   ReportSummary     `json:"summary"`
	Suites    []SuiteReport     `json:"suites,omitempty"`
	Results   []AssertionResult `json:"results"`
	ActualRef string            `json:"actual_ref,omitempty"`
}

// SuiteReport records per-suite provenance when a fixture directory evaluates
// multiple structured eval suites in one run.
type SuiteReport struct {
	Path      string        `json:"path"`
	Passed    bool          `json:"passed"`
	Metadata  Metadata      `json:"metadata,omitempty"`
	Metrics   ReportMetrics `json:"metrics,omitempty,omitzero"`
	Summary   ReportSummary `json:"summary"`
	ActualRef string        `json:"actual_ref,omitempty"`
}

// ReportSummary counts assertion outcomes in a structured eval report.
type ReportSummary struct {
	Total      int     `json:"total"`
	Passed     int     `json:"passed"`
	Failed     int     `json:"failed"`
	FlakeCount int     `json:"flake_count,omitempty"`
	Warnings   int     `json:"warnings"`
	PassRate   float64 `json:"pass_rate"`
}

// ReportMetrics carries repeatable run metrics alongside pass/fail results.
// Zero values are omitted from JSON/YAML so old fixtures remain compact.
type ReportMetrics struct {
	LatencyMillis int64   `json:"latency_millis,omitempty" yaml:"latency_millis,omitempty"`
	InputTokens   int     `json:"input_tokens,omitempty" yaml:"input_tokens,omitempty"`
	OutputTokens  int     `json:"output_tokens,omitempty" yaml:"output_tokens,omitempty"`
	TotalTokens   int     `json:"total_tokens,omitempty" yaml:"total_tokens,omitempty"`
	Cost          float64 `json:"cost,omitempty" yaml:"cost,omitempty"`
}

// WorkflowTrace records observed side effects from an agent or harness run.
type WorkflowTrace struct {
	ToolCalls []WorkflowToolCall `json:"tool_calls,omitempty" yaml:"tool_calls,omitempty"`
	Files     []WorkflowFile     `json:"files,omitempty" yaml:"files,omitempty"`
	Commands  []WorkflowCommand  `json:"commands,omitempty" yaml:"commands,omitempty"`
	Gates     []WorkflowGate     `json:"gates,omitempty" yaml:"gates,omitempty"`
	Artifacts []WorkflowArtifact `json:"artifacts,omitempty" yaml:"artifacts,omitempty"`
}

// WorkflowToolCall records one observed tool invocation.
type WorkflowToolCall struct {
	Name   string         `json:"name" yaml:"name"`
	Status string         `json:"status,omitempty" yaml:"status,omitempty"`
	Args   map[string]any `json:"args,omitempty" yaml:"args,omitempty"`
}

// WorkflowFile records one observed file side effect.
type WorkflowFile struct {
	Path   string `json:"path" yaml:"path"`
	Action string `json:"action,omitempty" yaml:"action,omitempty"`
	Status string `json:"status,omitempty" yaml:"status,omitempty"`
}

// WorkflowCommand records one observed command execution.
type WorkflowCommand struct {
	Command  string `json:"command" yaml:"command"`
	Status   string `json:"status,omitempty" yaml:"status,omitempty"`
	ExitCode *int   `json:"exit_code,omitempty" yaml:"exit_code,omitempty"`
}

// WorkflowGate records one observed review/test/workflow gate outcome.
type WorkflowGate struct {
	Name   string `json:"name" yaml:"name"`
	Status string `json:"status,omitempty" yaml:"status,omitempty"`
	Passed *bool  `json:"passed,omitempty" yaml:"passed,omitempty"`
}

// WorkflowArtifact records one produced artifact from an agent or harness run.
type WorkflowArtifact struct {
	Path   string `json:"path" yaml:"path"`
	Kind   string `json:"kind,omitempty" yaml:"kind,omitempty"`
	Status string `json:"status,omitempty" yaml:"status,omitempty"`
}

// JudgeDecision records a replayable rubric or judge decision. The eval runner
// only checks recorded decisions; it never calls a judge while evaluating.
type JudgeDecision struct {
	Judge         string  `json:"judge,omitempty" yaml:"judge,omitempty"`
	Model         string  `json:"model,omitempty" yaml:"model,omitempty"`
	Rubric        string  `json:"rubric,omitempty" yaml:"rubric,omitempty"`
	RubricVersion string  `json:"rubric_version,omitempty" yaml:"rubric_version,omitempty"`
	InputRef      string  `json:"input_ref,omitempty" yaml:"input_ref,omitempty"`
	OutputRef     string  `json:"output_ref,omitempty" yaml:"output_ref,omitempty"`
	Decision      string  `json:"decision,omitempty" yaml:"decision,omitempty"`
	Rationale     string  `json:"rationale,omitempty" yaml:"rationale,omitempty"`
	Score         float64 `json:"score,omitempty" yaml:"score,omitempty"`
	RecordedAt    string  `json:"recorded_at,omitempty" yaml:"recorded_at,omitempty"`
}

// AssertionResult is one per-assertion machine-readable result.
type AssertionResult struct {
	ID              string         `json:"id"`
	Type            AssertionType  `json:"type"`
	Severity        Severity       `json:"severity"`
	Status          string         `json:"status"`
	Passed          bool           `json:"passed"`
	Flaky           bool           `json:"flaky,omitempty"`
	Suite           string         `json:"suite,omitempty"`
	FlakeGroup      string         `json:"flake_group,omitempty"`
	Judge           *JudgeDecision `json:"judge,omitempty"`
	Evidence        string         `json:"evidence,omitempty"`
	ExpectedSnippet string         `json:"expected_snippet,omitempty"`
	ActualSnippet   string         `json:"actual_snippet,omitempty"`
	Remediation     string         `json:"remediation,omitempty"`
	Error           string         `json:"error,omitempty"`
}

// LoadSuiteFile reads a YAML or JSON structured eval suite.
func LoadSuiteFile(path string) (Suite, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return Suite{}, fmt.Errorf("read eval suite %s: %w", path, err)
	}

	var suite Suite
	if strings.EqualFold(filepath.Ext(path), ".json") {
		if err := decodeSuiteJSON(data, &suite); err != nil {
			return Suite{}, fmt.Errorf("parse eval suite %s: %w", path, err)
		}
	} else {
		if err := decodeSuiteYAML(data, &suite); err != nil {
			return Suite{}, fmt.Errorf("parse eval suite %s: %w", path, err)
		}
	}

	return suite, nil
}

func decodeSuiteJSON(data []byte, suite *Suite) error {
	decoder := json.NewDecoder(strings.NewReader(string(data)))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(suite); err != nil {
		return err
	}

	var extra any
	if err := decoder.Decode(&extra); !errors.Is(err, io.EOF) {
		if err == nil {
			return errors.New("unexpected trailing JSON value")
		}
		return fmt.Errorf("decode trailing JSON: %w", err)
	}

	return nil
}

func decodeSuiteYAML(data []byte, suite *Suite) error {
	decoder := yaml.NewDecoder(strings.NewReader(string(data)))
	decoder.KnownFields(true)
	if err := decoder.Decode(suite); err != nil {
		return err
	}

	var extra any
	if err := decoder.Decode(&extra); !errors.Is(err, io.EOF) {
		if err == nil {
			return errors.New("unexpected extra YAML document")
		}
		return fmt.Errorf("decode extra YAML document: %w", err)
	}

	return nil
}

// DiscoverSuiteFiles returns structured eval suite files under dir in stable
// lexical order. It recognizes eval.yaml, eval.yml, eval.json, and files named
// *.eval.yaml, *.eval.yml, or *.eval.json.
func DiscoverSuiteFiles(dir string) ([]string, error) {
	var paths []string
	if err := filepath.WalkDir(dir, func(path string, entry os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.IsDir() {
			return nil
		}
		if isSuiteFileName(entry.Name()) {
			paths = append(paths, path)
		}
		return nil
	}); err != nil {
		return nil, fmt.Errorf("discover eval fixtures in %s: %w", dir, err)
	}

	sort.Strings(paths)
	return paths, nil
}

// RunSuiteFile loads and evaluates one structured eval suite file.
func RunSuiteFile(path string, opts RunOptions) (Report, error) {
	suite, err := LoadSuiteFile(path)
	if err != nil {
		return Report{}, err
	}

	if opts.ArtifactRoot == "" {
		opts.ArtifactRoot = filepath.Dir(path)
	}
	if opts.SuitePath == "" {
		opts.SuitePath = path
	}

	return RunSuite(suite, opts)
}

// RunFixtureDir discovers and evaluates all structured eval suites in dir.
func RunFixtureDir(dir string, opts RunOptions) (Report, error) {
	paths, err := DiscoverSuiteFiles(dir)
	if err != nil {
		return Report{}, err
	}
	if len(paths) == 0 {
		return Report{}, fmt.Errorf("eval fixtures: no suite files found in %s", dir)
	}

	return RunSuiteFiles(paths, opts)
}

// RunSuiteFiles evaluates multiple suite files and flattens their assertion
// results into one report for CI consumers.
func RunSuiteFiles(paths []string, opts RunOptions) (Report, error) {
	report := newReport(Metadata{}, opts)
	runMetrics := opts.Metrics
	for _, path := range paths {
		suiteOpts := opts
		suiteOpts.ArtifactRoot = filepath.Dir(path)
		suiteOpts.SuitePath = path
		// Run-level metrics describe the evaluated run, not each fixture file.
		// Apply them once to the aggregate report after per-suite metrics are
		// combined so multi-suite runs do not multiply tokens, cost, or latency.
		suiteOpts.Metrics = ReportMetrics{}

		suiteReport, err := RunSuiteFile(path, suiteOpts)
		if err != nil {
			return Report{}, err
		}

		report.Results = append(report.Results, suiteReport.Results...)
		report.Metrics = combineReportMetrics(report.Metrics, suiteReport.Metrics)
		report.Suites = append(report.Suites, SuiteReport{
			Path:      path,
			Passed:    suiteReport.Passed,
			Metadata:  suiteReport.Metadata,
			Metrics:   suiteReport.Metrics,
			Summary:   suiteReport.Summary,
			ActualRef: suiteReport.ActualRef,
		})
	}

	report.Metadata = commonSuiteMetadata(report.Suites)
	report.Metrics = mergeReportMetrics(report.Metrics, runMetrics)
	finalizeReport(&report)
	return report, nil
}

// RunSuite evaluates one structured eval suite against actual output.
func RunSuite(suite Suite, opts RunOptions) (Report, error) {
	if suite.Version != 0 && suite.Version != reportVersion {
		return Report{}, fmt.Errorf("eval suite version %d is unsupported", suite.Version)
	}

	actual, actualRef, err := actualTextForSuite(suite, opts)
	if err != nil {
		return Report{}, err
	}

	report := newReport(suite.Metadata, opts)
	report.ActualRef = actualRef
	report.Metrics = mergeReportMetrics(suite.Metrics, opts.Metrics)

	workflow, err := workflowTraceForSuite(suite, opts)
	if err != nil {
		return Report{}, err
	}

	assertions := append([]Assertion(nil), suite.Assertions...)
	if suite.ExitCode != nil {
		assertions = append(assertions, Assertion{
			ID:       "exit_code",
			Type:     AssertionExitCode,
			Severity: SeverityError,
			Equals:   *suite.ExitCode,
		})
	}

	if opts.UpdateGolden && strings.TrimSpace(suite.GoldenPath) == "" {
		return Report{}, errors.New("eval golden update requested but suite has no golden path")
	}

	if opts.UpdateGolden && !opts.UseActualText && strings.TrimSpace(opts.ActualPath) == "" && strings.TrimSpace(suite.ActualPath) == "" {
		return Report{}, errors.New("eval golden update requires actual output")
	}

	if strings.TrimSpace(suite.GoldenPath) != "" {
		goldenPath := resolveSuitePath(opts.ArtifactRoot, suite.GoldenPath)
		if opts.UpdateGolden {
			if !opts.ApproveGoldenUpdate {
				return Report{}, errors.New("eval golden update requires explicit review approval")
			}
			if err := os.MkdirAll(filepath.Dir(goldenPath), 0o750); err != nil {
				return Report{}, fmt.Errorf("prepare golden directory: %w", err)
			}
			if err := os.WriteFile(goldenPath, []byte(actual), 0o600); err != nil {
				return Report{}, fmt.Errorf("update golden %s: %w", goldenPath, err)
			}
			report.Results = append(report.Results, passResult(Assertion{
				ID:       "golden_update",
				Type:     AssertionExact,
				Severity: SeverityWarning,
			}, opts, "updated golden file; review the diff before committing"))
		} else {
			golden, err := os.ReadFile(goldenPath)
			if err != nil {
				return Report{}, fmt.Errorf("read golden %s: %w", goldenPath, err)
			}
			assertions = append(assertions, Assertion{
				ID:          "golden",
				Type:        AssertionExact,
				Severity:    SeverityError,
				Value:       string(golden),
				Remediation: "Review the actual output. If the change is intended, rerun with golden update approval.",
			})
		}
	}

	if judgeAssertionsRequireIndependentSignal(assertions) {
		return Report{}, errors.New("judge assertions require at least one non-judge assertion")
	}

	if len(assertions) == 0 && len(report.Results) == 0 {
		return Report{}, errors.New("eval suite has no assertions")
	}

	for i, assertion := range assertions {
		report.Results = append(report.Results, evaluateAssertion(assertion, i, actual, workflow, opts))
	}

	finalizeReport(&report)
	return report, nil
}

// JSON returns an indented JSON representation of the report.
func (r Report) JSON() ([]byte, error) {
	return json.MarshalIndent(r, "", "  ")
}

func isSuiteFileName(name string) bool {
	name = strings.ToLower(name)
	return name == "eval.yaml" ||
		name == "eval.yml" ||
		name == "eval.json" ||
		strings.HasSuffix(name, ".eval.yaml") ||
		strings.HasSuffix(name, ".eval.yml") ||
		strings.HasSuffix(name, ".eval.json")
}

func newReport(metadata Metadata, opts RunOptions) Report {
	now := opts.Now
	if now.IsZero() {
		now = time.Now().UTC()
	}

	return Report{
		Version:  reportVersion,
		Suite:    opts.SuitePath,
		Passed:   true,
		RunAt:    now.UTC().Format(time.RFC3339),
		Metadata: metadata,
	}
}

func finalizeReport(report *Report) {
	markFlakyResults(report.Results)
	report.Summary = ReportSummary{Total: len(report.Results)}
	report.Passed = true
	for i := range report.Results {
		if report.Results[i].Passed {
			report.Summary.Passed++
		} else {
			report.Summary.Failed++
			report.Passed = false
		}
		if report.Results[i].Flaky || report.Results[i].Status == assertionStatusFlaky {
			report.Summary.FlakeCount++
		}
		if report.Results[i].Severity == SeverityWarning {
			report.Summary.Warnings++
		}
	}
	if report.Summary.Total > 0 {
		report.Summary.PassRate = float64(report.Summary.Passed) / float64(report.Summary.Total)
	}
}

func mergeReportMetrics(suite, opts ReportMetrics) ReportMetrics {
	merged := suite
	tokenPartsOverridden := false
	if opts.LatencyMillis != 0 {
		merged.LatencyMillis = opts.LatencyMillis
	}
	if opts.InputTokens != 0 {
		merged.InputTokens = opts.InputTokens
		tokenPartsOverridden = true
	}
	if opts.OutputTokens != 0 {
		merged.OutputTokens = opts.OutputTokens
		tokenPartsOverridden = true
	}
	if opts.TotalTokens != 0 {
		merged.TotalTokens = opts.TotalTokens
	} else if tokenPartsOverridden {
		merged.TotalTokens = merged.InputTokens + merged.OutputTokens
	}
	if opts.Cost != 0 {
		merged.Cost = opts.Cost
	}
	if merged.TotalTokens == 0 {
		merged.TotalTokens = merged.InputTokens + merged.OutputTokens
	}

	return merged
}

func combineReportMetrics(left, right ReportMetrics) ReportMetrics {
	return ReportMetrics{
		LatencyMillis: left.LatencyMillis + right.LatencyMillis,
		InputTokens:   left.InputTokens + right.InputTokens,
		OutputTokens:  left.OutputTokens + right.OutputTokens,
		TotalTokens:   left.TotalTokens + right.TotalTokens,
		Cost:          left.Cost + right.Cost,
	}
}

func commonSuiteMetadata(suites []SuiteReport) Metadata {
	if len(suites) == 0 {
		return Metadata{}
	}

	common := suites[0].Metadata
	for i := 1; i < len(suites); i++ {
		common = intersectMetadata(common, suites[i].Metadata)
	}

	return common
}

func intersectMetadata(left, right Metadata) Metadata {
	return Metadata{
		TargetCommand:  commonMetadataString(left.TargetCommand, right.TargetCommand),
		Provider:       commonMetadataString(left.Provider, right.Provider),
		Model:          commonMetadataString(left.Model, right.Model),
		Agent:          commonMetadataString(left.Agent, right.Agent),
		InputFixture:   commonMetadataString(left.InputFixture, right.InputFixture),
		FixtureVersion: commonMetadataString(left.FixtureVersion, right.FixtureVersion),
		Environment:    commonMetadataEnvironment(left.Environment, right.Environment),
		CreatedAt:      commonMetadataString(left.CreatedAt, right.CreatedAt),
		UpdatedAt:      commonMetadataString(left.UpdatedAt, right.UpdatedAt),
		Owner:          commonMetadataString(left.Owner, right.Owner),
	}
}

func commonMetadataString(left, right string) string {
	if left == right {
		return left
	}

	return ""
}

func commonMetadataEnvironment(left, right map[string]string) map[string]string {
	if !reflect.DeepEqual(left, right) {
		return nil
	}

	return left
}

func workflowTraceForSuite(suite Suite, opts RunOptions) (WorkflowTrace, error) {
	workflow := suite.Workflow
	if !workflowTraceEmpty(opts.Workflow) {
		workflow = opts.Workflow
	}
	if strings.TrimSpace(suite.WorkflowPath) == "" {
		return workflow, nil
	}
	if !workflowTraceEmpty(opts.Workflow) {
		return workflow, nil
	}

	path := resolveSuitePath(opts.ArtifactRoot, suite.WorkflowPath)
	data, err := os.ReadFile(path)
	if err != nil {
		return WorkflowTrace{}, fmt.Errorf("read workflow trace %s: %w", path, err)
	}

	var loaded WorkflowTrace
	if strings.EqualFold(filepath.Ext(path), ".json") {
		decoder := json.NewDecoder(strings.NewReader(string(data)))
		decoder.DisallowUnknownFields()
		if err := decoder.Decode(&loaded); err != nil {
			return WorkflowTrace{}, fmt.Errorf("parse workflow trace %s: %w", path, err)
		}
	} else {
		decoder := yaml.NewDecoder(strings.NewReader(string(data)))
		decoder.KnownFields(true)
		if err := decoder.Decode(&loaded); err != nil {
			return WorkflowTrace{}, fmt.Errorf("parse workflow trace %s: %w", path, err)
		}
	}

	return loaded, nil
}

func workflowTraceEmpty(workflow WorkflowTrace) bool {
	return len(workflow.ToolCalls) == 0 &&
		len(workflow.Files) == 0 &&
		len(workflow.Commands) == 0 &&
		len(workflow.Gates) == 0 &&
		len(workflow.Artifacts) == 0
}

func judgeAssertionsRequireIndependentSignal(assertions []Assertion) bool {
	judgeCount := 0
	nonJudgeCount := 0
	for i := range assertions {
		assertionType := normalizeAssertionType(assertions[i].Type)
		if assertionType == AssertionJudgeDecision {
			judgeCount++
			continue
		}
		nonJudgeCount++
	}

	return judgeCount > 0 && nonJudgeCount == 0
}

func markFlakyResults(results []AssertionResult) {
	type state struct {
		passed bool
		failed bool
	}

	states := make(map[string]state)
	for i := range results {
		key := flakeKey(results[i])
		if key == "" {
			continue
		}
		current := states[key]
		if results[i].Passed {
			current.passed = true
		} else {
			current.failed = true
		}
		states[key] = current
	}

	for i := range results {
		key := flakeKey(results[i])
		current := states[key]
		if key == "" || !current.passed || !current.failed {
			continue
		}

		results[i].Status = assertionStatusFlaky
		results[i].Passed = false
		results[i].Flaky = true
		results[i].Evidence = flakyResultEvidence(results[i].Evidence)
	}
}

func flakyResultEvidence(evidence string) string {
	const flakyEvidence = "non-deterministic assertion result across repeated runs"

	evidence = strings.TrimSpace(evidence)
	if strings.Contains(evidence, flakyEvidence) {
		return evidence
	}
	if evidence == "" {
		return flakyEvidence
	}

	return Redact(flakyEvidence + "; original evidence: " + evidence)
}

func flakeKey(result AssertionResult) string {
	if result.FlakeGroup != "" {
		return "group:" + strings.ToLower(strings.TrimSpace(result.FlakeGroup))
	}
	if result.ID == "" {
		return ""
	}

	return strings.ToLower(strings.TrimSpace(result.Suite)) + "\x00" + strings.ToLower(strings.TrimSpace(result.ID))
}

func actualTextForSuite(suite Suite, opts RunOptions) (string, string, error) {
	if opts.UseActualText {
		return opts.ActualText, opts.ActualPath, nil
	}

	if strings.TrimSpace(opts.ActualPath) != "" {
		data, err := os.ReadFile(opts.ActualPath)
		if err != nil {
			return "", "", fmt.Errorf("eval output: read actual %s: %w", opts.ActualPath, err)
		}
		return string(data), opts.ActualPath, nil
	}

	if strings.TrimSpace(suite.ActualPath) != "" {
		path := resolveSuitePath(opts.ArtifactRoot, suite.ActualPath)
		data, err := os.ReadFile(path)
		if err != nil {
			return "", "", fmt.Errorf("eval output: read actual %s: %w", path, err)
		}
		return string(data), path, nil
	}

	return "", "", nil
}

func evaluateAssertion(assertion Assertion, index int, actual string, workflow WorkflowTrace, opts RunOptions) AssertionResult {
	assertion = normalizeAssertion(assertion, index)

	switch assertion.Type {
	case AssertionExact:
		expected := textExpectation(assertion)
		if expected == "" && assertion.Equals == nil {
			return failResult(assertion, opts, "exact assertion is missing value or equals", "", actual, "")
		}
		if actual == expected {
			return passResult(assertion, opts, "actual output exactly matched expected text")
		}
		return failResult(assertion, opts, "actual output differed from expected text", expected, actual, diffSnippet(expected, actual))
	case AssertionContains:
		expected := requiredExpectations(assertion)
		if len(expected) == 0 {
			return failResult(assertion, opts, "contains assertion is missing value", "", actual, "")
		}
		missing := missingContents(actual, expected)
		if len(missing) == 0 {
			return passResult(assertion, opts, "actual output contained expected text")
		}
		return failResult(assertion, opts, "actual output did not contain required text", strings.Join(missing, "\n"), actual, containsSnippet(strings.Join(missing, "\n"), actual))
	case AssertionNotContains:
		forbidden := forbiddenExpectations(assertion)
		if len(forbidden) == 0 {
			return failResult(assertion, opts, "not_contains assertion is missing value", "", actual, "")
		}
		present := presentContents(actual, forbidden)
		if len(present) == 0 {
			return passResult(assertion, opts, "forbidden content was absent")
		}
		return failResult(assertion, opts, "actual output contained forbidden content", strings.Join(present, "\n"), actual, "forbidden content was present")
	case AssertionRegex:
		pattern := assertion.Pattern
		if pattern == "" {
			pattern = assertion.Value
		}
		if pattern == "" {
			return failResult(assertion, opts, "regex assertion is missing pattern", "", actual, "")
		}
		matched, err := regexp.MatchString(pattern, actual)
		if err != nil {
			return failResult(assertion, opts, "regex pattern is invalid", pattern, actual, err.Error())
		}
		if matched {
			return passResult(assertion, opts, "actual output matched regex")
		}
		return failResult(assertion, opts, "actual output did not match regex", pattern, actual, "")
	case AssertionJSONPath:
		return evaluatePathAssertion(assertion, actual, opts, decodeJSONValue)
	case AssertionYAMLPath:
		return evaluatePathAssertion(assertion, actual, opts, decodeYAMLValue)
	case AssertionUnorderedList:
		return evaluateUnorderedListAssertion(assertion, actual, opts)
	case AssertionSchema:
		return evaluateSchemaAssertion(assertion, actual, opts)
	case AssertionNumeric:
		return evaluateNumericAssertion(assertion, actual, opts)
	case AssertionToolCalled:
		return evaluateToolCallAssertion(assertion, workflow, opts)
	case AssertionFileTouched:
		return evaluateFileTouchedAssertion(assertion, workflow, opts)
	case AssertionCommandRun:
		return evaluateCommandRunAssertion(assertion, workflow, opts)
	case AssertionGatePassed:
		return evaluateGatePassedAssertion(assertion, workflow, opts)
	case AssertionArtifactMade:
		return evaluateArtifactProducedAssertion(assertion, workflow, opts)
	case AssertionArtifactExists:
		return evaluateArtifactAssertion(assertion, opts)
	case AssertionExitCode:
		return evaluateExitCodeAssertion(assertion, opts)
	case AssertionJudgeDecision:
		return evaluateJudgeAssertion(assertion, opts)
	default:
		return failResult(assertion, opts, fmt.Sprintf("unsupported assertion type %q", assertion.Type), "", actual, "")
	}
}

func normalizeAssertion(assertion Assertion, index int) Assertion {
	assertion.Type = normalizeAssertionType(assertion.Type)
	if assertion.ID == "" {
		assertion.ID = fmt.Sprintf("%s-%d", assertion.Type, index+1)
	}
	if assertion.Severity == "" {
		assertion.Severity = SeverityError
	}
	return assertion
}

func normalizeAssertionType(assertionType AssertionType) AssertionType {
	switch strings.ToLower(strings.TrimSpace(string(assertionType))) {
	case "exact":
		return AssertionExact
	case "contains", "required", "required_content", "required-content":
		return AssertionContains
	case "not_contains", "not-contains", "forbidden", "forbidden_content", "forbidden-content":
		return AssertionNotContains
	case "regex", "matches":
		return AssertionRegex
	case "json_path", "json-path", "jsonpath":
		return AssertionJSONPath
	case "yaml_path", "yaml-path", "yamlpath":
		return AssertionYAMLPath
	case "unordered_list", "unordered-list", "unordered_bullets", "unordered-bullets", "list":
		return AssertionUnorderedList
	case "schema", "json_schema", "json-schema":
		return AssertionSchema
	case "numeric", "number":
		return AssertionNumeric
	case "tool_called", "tool-called", "tool_call", "tool-call":
		return AssertionToolCalled
	case "file_touched", "file-touched", "file_touch", "file-touch":
		return AssertionFileTouched
	case "command_run", "command-run", "command":
		return AssertionCommandRun
	case "gate_passed", "gate-passed", "gate":
		return AssertionGatePassed
	case "artifact_produced", "artifact-produced", "artifact_made", "artifact-made", "produced_artifact", "produced-artifact":
		return AssertionArtifactMade
	case "artifact_exists", "artifact-exists", "artifact":
		return AssertionArtifactExists
	case "exit_code", "exit-code":
		return AssertionExitCode
	case "judge_decision", "judge-decision", "judge", "rubric":
		return AssertionJudgeDecision
	default:
		return assertionType
	}
}

func textExpectation(assertion Assertion) string {
	if assertion.Value != "" {
		return assertion.Value
	}
	if assertion.Equals != nil {
		return valueToString(assertion.Equals)
	}
	return ""
}

func requiredExpectations(assertion Assertion) []string {
	values := append([]string(nil), assertion.Required...)
	if assertion.Contains != "" {
		values = append(values, assertion.Contains)
	}
	if assertion.Value != "" {
		values = append(values, assertion.Value)
	} else if assertion.Equals != nil {
		values = append(values, valueToString(assertion.Equals))
	}

	return nonEmptyStrings(values)
}

func forbiddenExpectations(assertion Assertion) []string {
	values := append([]string(nil), assertion.Forbidden...)
	if assertion.NotContains != "" {
		values = append(values, assertion.NotContains)
	}
	if assertion.Value != "" {
		values = append(values, assertion.Value)
	} else if assertion.Equals != nil {
		values = append(values, valueToString(assertion.Equals))
	}

	return nonEmptyStrings(values)
}

func nonEmptyStrings(values []string) []string {
	out := make([]string, 0, len(values))
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			out = append(out, value)
		}
	}

	return out
}

func firstNonBlank(values ...string) string {
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			return value
		}
	}

	return ""
}

func missingContents(actual string, required []string) []string {
	missing := make([]string, 0, len(required))
	for _, value := range required {
		if !strings.Contains(actual, value) {
			missing = append(missing, value)
		}
	}

	return missing
}

func presentContents(actual string, forbidden []string) []string {
	present := make([]string, 0, len(forbidden))
	for _, value := range forbidden {
		if strings.Contains(actual, value) {
			present = append(present, value)
		}
	}

	return present
}

func evaluatePathAssertion(assertion Assertion, actual string, opts RunOptions, decode func(string) (any, error)) AssertionResult {
	if assertion.Path == "" {
		return failResult(assertion, opts, "path assertion is missing path", "", actual, "")
	}

	document, err := decode(actual)
	if err != nil {
		return failResult(assertion, opts, "actual output could not be parsed", assertion.Path, actual, err.Error())
	}

	value, err := lookupPath(document, assertion.Path)
	if err != nil {
		return failResult(assertion, opts, "path was not found", assertion.Path, actual, err.Error())
	}

	return evaluateValueExpectation(assertion, value, opts)
}

func evaluateValueExpectation(assertion Assertion, value any, opts RunOptions) AssertionResult {
	actualText := valueToString(value)
	switch {
	case assertion.Equals != nil:
		if valuesEqual(value, assertion.Equals) {
			return passResult(assertion, opts, fmt.Sprintf("%s equals expected value", assertion.Path))
		}
		return failResult(assertion, opts, fmt.Sprintf("%s did not equal expected value", assertion.Path), valueToString(assertion.Equals), actualText, "")
	case assertion.Contains != "":
		if strings.Contains(actualText, assertion.Contains) {
			return passResult(assertion, opts, fmt.Sprintf("%s contained expected text", assertion.Path))
		}
		return failResult(assertion, opts, fmt.Sprintf("%s did not contain expected text", assertion.Path), assertion.Contains, actualText, "")
	case assertion.NotContains != "":
		if !strings.Contains(actualText, assertion.NotContains) {
			return passResult(assertion, opts, fmt.Sprintf("%s did not contain forbidden text", assertion.Path))
		}
		return failResult(assertion, opts, fmt.Sprintf("%s contained forbidden text", assertion.Path), assertion.NotContains, actualText, "")
	case assertion.Matches != "":
		matched, err := regexp.MatchString(assertion.Matches, actualText)
		if err != nil {
			return failResult(assertion, opts, "path regex pattern is invalid", assertion.Matches, actualText, err.Error())
		}
		if matched {
			return passResult(assertion, opts, fmt.Sprintf("%s matched regex", assertion.Path))
		}
		return failResult(assertion, opts, fmt.Sprintf("%s did not match regex", assertion.Path), assertion.Matches, actualText, "")
	default:
		return passResult(assertion, opts, fmt.Sprintf("%s existed with value %s", assertion.Path, Redact(compact(actualText, 80))))
	}
}

func evaluateUnorderedListAssertion(assertion Assertion, actual string, opts RunOptions) AssertionResult {
	expected := unorderedExpectedItems(assertion)
	if len(expected) == 0 {
		return failResult(assertion, opts, "unordered_list assertion is missing items or value", "", actual, "")
	}

	actualItems, err := unorderedActualItems(assertion, actual)
	if err != nil {
		return failResult(assertion, opts, "unordered_list actual items were unavailable", strings.Join(expected, "\n"), actual, err.Error())
	}

	missing, extra := compareUnorderedItems(expected, actualItems, assertion.AllowExtra)
	if len(missing) == 0 && len(extra) == 0 {
		return passResult(assertion, opts, "actual list contained expected items regardless of order")
	}

	var details []string
	if len(missing) > 0 {
		details = append(details, "missing: "+strings.Join(missing, ", "))
	}
	if len(extra) > 0 {
		details = append(details, "extra: "+strings.Join(extra, ", "))
	}

	return failResult(assertion, opts, "actual list did not match expected unordered items", strings.Join(expected, "\n"), strings.Join(actualItems, "\n"), strings.Join(details, "; "))
}

func unorderedExpectedItems(assertion Assertion) []string {
	if len(assertion.Items) > 0 {
		return nonEmptyStrings(assertion.Items)
	}
	if assertion.Value != "" {
		return nonEmptyStrings(strings.Split(assertion.Value, "\n"))
	}
	if assertion.Equals != nil {
		if values, ok := stringSlice(normalizeValue(assertion.Equals)); ok {
			return nonEmptyStrings(values)
		}
	}

	return nil
}

func unorderedActualItems(assertion Assertion, actual string) ([]string, error) {
	if assertion.Path == "" {
		return bulletListItems(actual), nil
	}

	document, err := decodeStructuredValue(actual)
	if err != nil {
		return nil, fmt.Errorf("parse structured list output: %w", err)
	}

	value, err := lookupPath(document, assertion.Path)
	if err != nil {
		return nil, err
	}

	items, ok := value.([]any)
	if !ok {
		return nil, fmt.Errorf("%s is %T, not an array", assertion.Path, value)
	}

	out := make([]string, 0, len(items))
	for _, item := range items {
		out = append(out, valueToString(item))
	}

	return nonEmptyStrings(out), nil
}

func bulletListItems(actual string) []string {
	var items []string
	for _, line := range strings.Split(actual, "\n") {
		item, ok := stripListMarker(line)
		if ok {
			items = append(items, item)
		}
	}
	if len(items) > 0 {
		return nonEmptyStrings(items)
	}

	return nonEmptyStrings(strings.Split(actual, "\n"))
}

func stripListMarker(line string) (string, bool) {
	line = strings.TrimSpace(line)
	if line == "" {
		return "", false
	}
	for _, prefix := range []string{"- ", "* ", "+ "} {
		if strings.HasPrefix(line, prefix) {
			return strings.TrimSpace(strings.TrimPrefix(line, prefix)), true
		}
	}

	for i, r := range line {
		if r < '0' || r > '9' {
			if i > 0 && (r == '.' || r == ')') {
				return strings.TrimSpace(line[i+1:]), true
			}
			break
		}
	}

	return line, false
}

func compareUnorderedItems(expected, actual []string, allowExtra bool) (missing, extra []string) {
	expectedCounts := normalizedItemCounts(expected)
	actualCounts := normalizedItemCounts(actual)

	for key, want := range expectedCounts {
		if got := actualCounts[key]; got < want {
			missing = append(missing, key)
		}
	}

	if !allowExtra {
		for key, got := range actualCounts {
			if want := expectedCounts[key]; got > want {
				extra = append(extra, key)
			}
		}
	}

	sort.Strings(missing)
	sort.Strings(extra)

	return missing, extra
}

func normalizedItemCounts(items []string) map[string]int {
	counts := make(map[string]int, len(items))
	for _, item := range items {
		item = Normalize(item)
		if item == "" {
			continue
		}
		counts[item]++
	}

	return counts
}

func evaluateSchemaAssertion(assertion Assertion, actual string, opts RunOptions) AssertionResult {
	schema, err := assertionSchema(assertion, opts)
	if err != nil {
		return failResult(assertion, opts, "schema assertion could not load schema", assertion.SchemaPath, actual, err.Error())
	}

	if len(schema) == 0 {
		return failResult(assertion, opts, "schema assertion is missing schema", "", actual, "")
	}

	value, err := decodeStructuredValue(actual)
	if err != nil {
		return failResult(assertion, opts, "actual output could not be parsed as JSON or YAML", "schema", actual, err.Error())
	}

	failures := validateSchema(value, schema, "$")
	if len(failures) == 0 {
		return passResult(assertion, opts, "actual output satisfied schema")
	}

	return failResult(assertion, opts, "actual output violated schema", valueToString(schema), actual, strings.Join(failures, "; "))
}

func assertionSchema(assertion Assertion, opts RunOptions) (map[string]any, error) {
	if assertion.SchemaPath != "" && len(assertion.Schema) > 0 {
		return nil, errors.New("pass either schema or schema_file, not both")
	}

	if assertion.SchemaPath == "" {
		return assertion.Schema, nil
	}

	path := resolveSuitePath(opts.ArtifactRoot, assertion.SchemaPath)
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read schema file %s: %w", path, err)
	}

	value, err := decodeStructuredValue(string(data))
	if err != nil {
		return nil, fmt.Errorf("parse schema file %s: %w", path, err)
	}

	schema, ok := normalizeValue(value).(map[string]any)
	if !ok {
		return nil, fmt.Errorf("schema file %s must contain an object", path)
	}

	return schema, nil
}

func evaluateNumericAssertion(assertion Assertion, actual string, opts RunOptions) AssertionResult {
	valueText := actual
	if assertion.Path != "" {
		document, err := decodeStructuredValue(actual)
		if err != nil {
			return failResult(assertion, opts, "actual output could not be parsed as JSON or YAML", assertion.Path, actual, err.Error())
		}
		value, err := lookupPath(document, assertion.Path)
		if err != nil {
			return failResult(assertion, opts, "numeric path was not found", assertion.Path, actual, err.Error())
		}
		valueText = valueToString(value)
	}

	value, ok := valueFloat(valueText)
	if !ok {
		return failResult(assertion, opts, "actual value was not numeric", numericExpectationDescription(assertion), valueText, "")
	}

	if assertion.Equals != nil {
		expected, ok := valueFloat(assertion.Equals)
		if !ok {
			return failResult(assertion, opts, "numeric equals value was not numeric", valueToString(assertion.Equals), valueText, "")
		}
		tolerance := 0.0
		if assertion.Tolerance != nil {
			tolerance = *assertion.Tolerance
		}
		if math.Abs(value-expected) <= tolerance {
			return passResult(assertion, opts, "numeric value was within tolerance")
		}
		return failResult(assertion, opts, "numeric value was outside tolerance", numericExpectationDescription(assertion), valueText, "")
	}

	if assertion.Min != nil && value < *assertion.Min {
		return failResult(assertion, opts, "numeric value was below minimum", numericExpectationDescription(assertion), valueText, "")
	}
	if assertion.Max != nil && value > *assertion.Max {
		return failResult(assertion, opts, "numeric value was above maximum", numericExpectationDescription(assertion), valueText, "")
	}

	return passResult(assertion, opts, "numeric value satisfied tolerance")
}

func evaluateToolCallAssertion(assertion Assertion, workflow WorkflowTrace, opts RunOptions) AssertionResult {
	name := firstNonBlank(assertion.Name, assertion.Value)
	if name == "" && assertion.Pattern == "" {
		return failResult(assertion, opts, "tool_called assertion is missing name, value, or pattern", "", "", "")
	}
	if err := validateWorkflowPattern(assertion.Pattern); err != nil {
		return failResult(assertion, opts, "tool_called pattern is invalid", assertion.Pattern, "", err.Error())
	}

	wantExists := expectedExists(assertion)
	for i := range workflow.ToolCalls {
		call := workflow.ToolCalls[i]
		if workflowStringMatches(call.Name, name, assertion.Pattern) &&
			workflowStatusMatches(call.Status, assertion.Status) &&
			workflowArgsMatch(call.Args, assertion.Args) {
			if wantExists {
				return passResult(assertion, opts, "required tool call was observed")
			}
			return failResult(assertion, opts, "forbidden tool call was observed", "absent", call.Name, "")
		}
	}

	if wantExists {
		return failResult(assertion, opts, "required tool call was missing", firstNonBlank(name, assertion.Pattern), observedToolCalls(workflow), "")
	}

	return passResult(assertion, opts, "forbidden tool call was absent")
}

func workflowArgsMatch(actual, expected map[string]any) bool {
	if len(expected) == 0 {
		return true
	}
	if len(actual) == 0 {
		return false
	}

	for key, expectedValue := range expected {
		actualValue, ok := actual[key]
		if !ok || !valuesEqual(actualValue, expectedValue) {
			return false
		}
	}

	return true
}

func evaluateFileTouchedAssertion(assertion Assertion, workflow WorkflowTrace, opts RunOptions) AssertionResult {
	path := firstNonBlank(assertion.Path, assertion.Value)
	if path == "" && assertion.Pattern == "" {
		return failResult(assertion, opts, "file_touched assertion is missing path, value, or pattern", "", "", "")
	}
	if err := validateWorkflowPattern(assertion.Pattern); err != nil {
		return failResult(assertion, opts, "file_touched pattern is invalid", assertion.Pattern, "", err.Error())
	}

	wantExists := expectedExists(assertion)
	for i := range workflow.Files {
		file := workflow.Files[i]
		if workflowPathMatches(file.Path, path, assertion.Pattern) &&
			workflowStatusMatches(file.Status, assertion.Status) &&
			workflowStatusMatches(file.Action, assertion.Action) {
			if wantExists {
				return passResult(assertion, opts, "required file touch was observed")
			}
			return failResult(assertion, opts, "forbidden file touch was observed", "absent", file.Path, "")
		}
	}

	if wantExists {
		return failResult(assertion, opts, "required file touch was missing", firstNonBlank(path, assertion.Pattern), observedFiles(workflow), "")
	}

	return passResult(assertion, opts, "forbidden file touch was absent")
}

func evaluateCommandRunAssertion(assertion Assertion, workflow WorkflowTrace, opts RunOptions) AssertionResult {
	command := firstNonBlank(assertion.Command, assertion.Value)
	if command == "" && assertion.Pattern == "" {
		return failResult(assertion, opts, "command_run assertion is missing command, value, or pattern", "", "", "")
	}
	if err := validateWorkflowPattern(assertion.Pattern); err != nil {
		return failResult(assertion, opts, "command_run pattern is invalid", assertion.Pattern, "", err.Error())
	}

	wantExists := expectedExists(assertion)
	for i := range workflow.Commands {
		run := workflow.Commands[i]
		if !workflowCommandMatches(run.Command, command, assertion.Pattern) ||
			!workflowStatusMatches(run.Status, assertion.Status) ||
			!workflowExitCodeMatches(run.ExitCode, assertion.ExitCode) {
			continue
		}

		if wantExists {
			return passResult(assertion, opts, "required command run was observed")
		}
		return failResult(assertion, opts, "forbidden command run was observed", "absent", run.Command, "")
	}

	if wantExists {
		return failResult(assertion, opts, "required command run was missing", firstNonBlank(command, assertion.Pattern), observedCommands(workflow), "")
	}

	return passResult(assertion, opts, "forbidden command run was absent")
}

func evaluateGatePassedAssertion(assertion Assertion, workflow WorkflowTrace, opts RunOptions) AssertionResult {
	name := firstNonBlank(assertion.Gate, assertion.Name, assertion.Value)
	if name == "" {
		return failResult(assertion, opts, "gate_passed assertion is missing gate, name, or value", "", "", "")
	}

	wantExists := expectedExists(assertion)
	matching := WorkflowTrace{}
	for i := range workflow.Gates {
		gate := workflow.Gates[i]
		if !workflowNameMatches(gate.Name, name) || !workflowStatusMatches(gate.Status, assertion.Status) {
			continue
		}

		matching.Gates = append(matching.Gates, gate)
		passed := gatePassed(gate)
		if wantExists && passed {
			return passResult(assertion, opts, "required gate passed")
		}
		if passed {
			return failResult(assertion, opts, "forbidden gate passed", "not passed", gate.Name, "")
		}
	}

	if wantExists {
		if len(matching.Gates) > 0 {
			return failResult(assertion, opts, "gate was observed but did not pass", "passed", observedGates(matching), "")
		}

		return failResult(assertion, opts, "required gate was missing", name, observedGates(workflow), "")
	}

	return passResult(assertion, opts, "forbidden gate did not pass")
}

func evaluateArtifactProducedAssertion(assertion Assertion, workflow WorkflowTrace, opts RunOptions) AssertionResult {
	path := firstNonBlank(assertion.Path, assertion.Value)
	if path == "" && assertion.Pattern == "" {
		return failResult(assertion, opts, "artifact_produced assertion is missing path, value, or pattern", "", "", "")
	}
	if err := validateWorkflowPattern(assertion.Pattern); err != nil {
		return failResult(assertion, opts, "artifact_produced pattern is invalid", assertion.Pattern, "", err.Error())
	}

	wantExists := expectedExists(assertion)
	for i := range workflow.Artifacts {
		artifact := workflow.Artifacts[i]
		if !workflowPathMatches(artifact.Path, path, assertion.Pattern) ||
			!workflowStatusMatches(artifact.Status, assertion.Status) ||
			!workflowStatusMatches(artifact.Kind, assertion.Kind) {
			continue
		}

		if wantExists {
			return passResult(assertion, opts, "required artifact production was observed")
		}
		return failResult(assertion, opts, "forbidden artifact production was observed", "absent", artifact.Path, "")
	}

	if wantExists {
		return failResult(assertion, opts, "required artifact production was missing", firstNonBlank(path, assertion.Pattern), observedArtifacts(workflow), "")
	}

	return passResult(assertion, opts, "forbidden artifact production was absent")
}

func observedToolCalls(workflow WorkflowTrace) string {
	parts := make([]string, 0, len(workflow.ToolCalls))
	for i := range workflow.ToolCalls {
		call := workflow.ToolCalls[i]
		part := call.Name
		if call.Status != "" {
			part += " status=" + call.Status
		}
		if len(call.Args) > 0 {
			part += " args=" + valueToString(call.Args)
		}
		parts = append(parts, strings.TrimSpace(part))
	}

	return strings.Join(nonEmptyStrings(parts), "\n")
}

func observedFiles(workflow WorkflowTrace) string {
	parts := make([]string, 0, len(workflow.Files))
	for i := range workflow.Files {
		file := workflow.Files[i]
		part := file.Path
		if file.Action != "" {
			part += " action=" + file.Action
		}
		if file.Status != "" {
			part += " status=" + file.Status
		}
		parts = append(parts, strings.TrimSpace(part))
	}

	return strings.Join(nonEmptyStrings(parts), "\n")
}

func observedCommands(workflow WorkflowTrace) string {
	parts := make([]string, 0, len(workflow.Commands))
	for i := range workflow.Commands {
		command := workflow.Commands[i]
		part := command.Command
		if command.Status != "" {
			part += " status=" + command.Status
		}
		if command.ExitCode != nil {
			part += " exit_code=" + strconv.Itoa(*command.ExitCode)
		}
		parts = append(parts, strings.TrimSpace(part))
	}

	return strings.Join(nonEmptyStrings(parts), "\n")
}

func observedGates(workflow WorkflowTrace) string {
	parts := make([]string, 0, len(workflow.Gates))
	for i := range workflow.Gates {
		gate := workflow.Gates[i]
		part := gate.Name
		if gate.Status != "" {
			part += " status=" + gate.Status
		}
		if gate.Passed != nil {
			part += " passed=" + strconv.FormatBool(*gate.Passed)
		}
		parts = append(parts, strings.TrimSpace(part))
	}

	return strings.Join(nonEmptyStrings(parts), "\n")
}

func observedArtifacts(workflow WorkflowTrace) string {
	parts := make([]string, 0, len(workflow.Artifacts))
	for i := range workflow.Artifacts {
		artifact := workflow.Artifacts[i]
		part := artifact.Path
		if artifact.Kind != "" {
			part += " kind=" + artifact.Kind
		}
		if artifact.Status != "" {
			part += " status=" + artifact.Status
		}
		parts = append(parts, strings.TrimSpace(part))
	}

	return strings.Join(nonEmptyStrings(parts), "\n")
}

func evaluateJudgeAssertion(assertion Assertion, opts RunOptions) AssertionResult {
	if assertion.Judge == nil {
		return failResult(assertion, opts, "judge_decision assertion is missing recorded judge", "", "", "")
	}

	decision := strings.TrimSpace(assertion.Judge.Decision)
	if decision == "" {
		return withJudgeDecision(failResult(assertion, opts, "judge_decision is missing decision", expectedJudgeDecision(assertion), "", ""), *assertion.Judge)
	}
	if strings.TrimSpace(assertion.Judge.InputRef) == "" || strings.TrimSpace(assertion.Judge.OutputRef) == "" {
		return withJudgeDecision(failResult(assertion, opts, "judge_decision must record input_ref and output_ref for replay", expectedJudgeDecision(assertion), decision, ""), *assertion.Judge)
	}
	if strings.TrimSpace(assertion.Judge.RubricVersion) == "" && strings.TrimSpace(assertion.Judge.Rubric) == "" {
		return withJudgeDecision(failResult(assertion, opts, "judge_decision must record rubric or rubric_version", expectedJudgeDecision(assertion), decision, ""), *assertion.Judge)
	}

	expected := expectedJudgeDecision(assertion)
	if Normalize(decision) != Normalize(expected) {
		return withJudgeDecision(failResult(assertion, opts, "recorded judge decision did not match expected decision", expected, decision, ""), *assertion.Judge)
	}

	return withJudgeDecision(passResult(assertion, opts, judgeEvidence(*assertion.Judge)), *assertion.Judge)
}

func withJudgeDecision(result AssertionResult, decision JudgeDecision) AssertionResult {
	sanitized := sanitizeJudgeDecision(decision)
	result.Judge = &sanitized

	return result
}

func sanitizeJudgeDecision(decision JudgeDecision) JudgeDecision {
	return JudgeDecision{
		Judge:         Redact(strings.TrimSpace(decision.Judge)),
		Model:         Redact(strings.TrimSpace(decision.Model)),
		Rubric:        Redact(strings.TrimSpace(decision.Rubric)),
		RubricVersion: Redact(strings.TrimSpace(decision.RubricVersion)),
		InputRef:      Redact(strings.TrimSpace(decision.InputRef)),
		OutputRef:     Redact(strings.TrimSpace(decision.OutputRef)),
		Decision:      Redact(strings.TrimSpace(decision.Decision)),
		Rationale:     Redact(strings.TrimSpace(decision.Rationale)),
		Score:         decision.Score,
		RecordedAt:    Redact(strings.TrimSpace(decision.RecordedAt)),
	}
}

func expectedExists(assertion Assertion) bool {
	if assertion.Exists == nil {
		return true
	}

	return *assertion.Exists
}

func validateWorkflowPattern(pattern string) error {
	pattern = strings.TrimSpace(pattern)
	if pattern == "" {
		return nil
	}

	_, err := regexp.Compile(pattern)

	return err
}

func workflowNameMatches(actual, expected string) bool {
	return strings.EqualFold(strings.TrimSpace(actual), strings.TrimSpace(expected))
}

func workflowStringMatches(actual, expected, pattern string) bool {
	actual = strings.TrimSpace(actual)
	if strings.TrimSpace(pattern) != "" {
		matched, err := regexp.MatchString(pattern, actual)
		return err == nil && matched
	}

	return workflowNameMatches(actual, expected)
}

func workflowPathMatches(actual, expected, pattern string) bool {
	actual = filepath.Clean(strings.TrimSpace(actual))
	if strings.TrimSpace(pattern) != "" {
		matched, err := regexp.MatchString(pattern, actual)
		return err == nil && matched
	}

	expected = filepath.Clean(strings.TrimSpace(expected))

	return actual == expected
}

func workflowStatusMatches(actual, expected string) bool {
	expected = strings.TrimSpace(expected)
	if expected == "" {
		return true
	}

	return strings.EqualFold(strings.TrimSpace(actual), expected)
}

func workflowCommandMatches(actual, expected, pattern string) bool {
	actual = strings.TrimSpace(actual)
	if pattern != "" {
		matched, err := regexp.MatchString(pattern, actual)
		return err == nil && matched
	}

	return actual == strings.TrimSpace(expected)
}

func workflowExitCodeMatches(actual, expected *int) bool {
	if expected == nil {
		return true
	}
	if actual == nil {
		return false
	}

	return *actual == *expected
}

func gatePassed(gate WorkflowGate) bool {
	if gate.Passed != nil {
		return *gate.Passed
	}

	switch strings.ToLower(strings.TrimSpace(gate.Status)) {
	case "pass", "passed", "success", "succeeded", "ok":
		return true
	default:
		return false
	}
}

func expectedJudgeDecision(assertion Assertion) string {
	expected := textExpectation(assertion)
	if expected == "" {
		return "pass"
	}

	return expected
}

func judgeEvidence(decision JudgeDecision) string {
	parts := []string{"recorded judge decision " + strings.TrimSpace(decision.Decision)}
	if decision.Judge != "" {
		parts = append(parts, "judge="+decision.Judge)
	}
	if decision.Model != "" {
		parts = append(parts, "model="+decision.Model)
	}
	if decision.RubricVersion != "" {
		parts = append(parts, "rubric_version="+decision.RubricVersion)
	} else if decision.Rubric != "" {
		parts = append(parts, "rubric="+decision.Rubric)
	}
	if decision.InputRef != "" {
		parts = append(parts, "input_ref="+decision.InputRef)
	}
	if decision.OutputRef != "" {
		parts = append(parts, "output_ref="+decision.OutputRef)
	}

	return strings.Join(parts, " ")
}

func evaluateArtifactAssertion(assertion Assertion, opts RunOptions) AssertionResult {
	if assertion.Path == "" {
		return failResult(assertion, opts, "artifact assertion is missing path", "", "", "")
	}

	wantExists := true
	if assertion.Exists != nil {
		wantExists = *assertion.Exists
	}

	path := resolveSuitePath(opts.ArtifactRoot, assertion.Path)
	_, err := os.Stat(path)
	exists := err == nil
	if err != nil && !os.IsNotExist(err) {
		return failResult(assertion, opts, "artifact could not be inspected", strconv.FormatBool(wantExists), path, err.Error())
	}

	if exists == wantExists {
		if exists {
			return passResult(assertion, opts, "artifact exists")
		}
		return passResult(assertion, opts, "artifact is absent as expected")
	}

	if wantExists {
		return failResult(assertion, opts, "required artifact was missing", "exists", path, "")
	}
	return failResult(assertion, opts, "artifact existed but should be absent", "absent", path, "")
}

func evaluateExitCodeAssertion(assertion Assertion, opts RunOptions) AssertionResult {
	if opts.ExitCode == nil {
		return failResult(assertion, opts, "actual exit code was unavailable", valueToString(assertion.Equals), "", "")
	}

	expected, ok := valueInt(assertion.Equals)
	if assertion.Equals == nil {
		expected, ok = valueInt(assertion.Value)
	}
	if assertion.ExitCode != nil {
		expected = *assertion.ExitCode
		ok = true
	}
	if !ok {
		return failResult(assertion, opts, "exit_code assertion is missing numeric equals/value", "", strconv.Itoa(*opts.ExitCode), "")
	}

	if *opts.ExitCode == expected {
		return passResult(assertion, opts, "exit code matched")
	}

	return failResult(assertion, opts, "exit code did not match", strconv.Itoa(expected), strconv.Itoa(*opts.ExitCode), "")
}

func passResult(assertion Assertion, opts RunOptions, evidence string) AssertionResult {
	return AssertionResult{
		ID:          assertion.ID,
		Type:        assertion.Type,
		Severity:    assertion.Severity,
		Status:      assertionStatusPass,
		Passed:      true,
		Suite:       opts.SuitePath,
		FlakeGroup:  strings.TrimSpace(assertion.FlakeGroup),
		Evidence:    Redact(evidence),
		Remediation: assertion.Remediation,
	}
}

func failResult(assertion Assertion, opts RunOptions, evidence, expected, actual, errText string) AssertionResult {
	return AssertionResult{
		ID:              assertion.ID,
		Type:            assertion.Type,
		Severity:        assertion.Severity,
		Status:          assertionStatusFail,
		Passed:          false,
		Suite:           opts.SuitePath,
		FlakeGroup:      strings.TrimSpace(assertion.FlakeGroup),
		Evidence:        Redact(evidence),
		ExpectedSnippet: compact(Redact(expected), 160),
		ActualSnippet:   compact(Redact(actual), 160),
		Remediation:     assertion.Remediation,
		Error:           Redact(errText),
	}
}

func resolveSuitePath(root, path string) string {
	if filepath.IsAbs(path) || root == "" {
		return path
	}
	return filepath.Join(root, path)
}

func decodeStructuredValue(src string) (any, error) {
	value, err := decodeJSONValue(src)
	if err == nil {
		return value, nil
	}

	yamlValue, yamlErr := decodeYAMLValue(src)
	if yamlErr == nil {
		return yamlValue, nil
	}

	return nil, fmt.Errorf("parse structured output: %w", errors.Join(err, yamlErr))
}

func decodeJSONValue(src string) (any, error) {
	decoder := json.NewDecoder(strings.NewReader(src))
	decoder.UseNumber()

	var value any
	if err := decoder.Decode(&value); err != nil {
		return nil, err
	}

	var extra any
	if err := decoder.Decode(&extra); err != io.EOF {
		if err == nil {
			return nil, errors.New("unexpected trailing JSON value")
		}
		return nil, fmt.Errorf("decode trailing JSON: %w", err)
	}

	return normalizeValue(value), nil
}

func decodeYAMLValue(src string) (any, error) {
	var value any
	if err := yaml.Unmarshal([]byte(src), &value); err != nil {
		return nil, err
	}

	return normalizeValue(value), nil
}

func normalizeValue(value any) any {
	switch typed := value.(type) {
	case map[string]any:
		normalized := make(map[string]any, len(typed))
		for key, child := range typed {
			normalized[key] = normalizeValue(child)
		}
		return normalized
	case map[any]any:
		normalized := make(map[string]any, len(typed))
		for key, child := range typed {
			normalized[fmt.Sprint(key)] = normalizeValue(child)
		}
		return normalized
	case []any:
		for i := range typed {
			typed[i] = normalizeValue(typed[i])
		}
		return typed
	default:
		return value
	}
}

type pathToken struct {
	key   string
	index *int
}

func lookupPath(document any, path string) (any, error) {
	tokens, err := parsePath(path)
	if err != nil {
		return nil, err
	}

	current := document
	for _, token := range tokens {
		if token.index != nil {
			items, ok := current.([]any)
			if !ok {
				return nil, fmt.Errorf("path segment [%d] found non-array %T", *token.index, current)
			}
			if *token.index < 0 || *token.index >= len(items) {
				return nil, fmt.Errorf("array index %d out of range", *token.index)
			}
			current = items[*token.index]
			continue
		}

		object, ok := current.(map[string]any)
		if !ok {
			return nil, fmt.Errorf("path segment %q found non-object %T", token.key, current)
		}
		value, ok := object[token.key]
		if !ok {
			return nil, fmt.Errorf("object key %q not found", token.key)
		}
		current = value
	}

	return current, nil
}

func parsePath(path string) ([]pathToken, error) {
	path = strings.TrimSpace(path)
	if path == "" || path == "$" || path == "." {
		return nil, nil
	}
	path = strings.TrimPrefix(path, "$")
	path = strings.TrimPrefix(path, ".")

	var tokens []pathToken
	for path != "" {
		switch {
		case path[0] == '.':
			path = path[1:]
		case path[0] == '[':
			end := strings.IndexByte(path, ']')
			if end < 0 {
				return nil, fmt.Errorf("unterminated array segment in path")
			}

			segment := strings.TrimSpace(path[1:end])
			if bracketSegmentIsKey(segment) {
				key, err := unquotePathKey(segment)
				if err != nil {
					return nil, err
				}
				tokens = append(tokens, pathToken{key: key})
				path = path[end+1:]

				continue
			}

			index, err := strconv.Atoi(segment)
			if err != nil {
				return nil, fmt.Errorf("parse array index %q: %w", segment, err)
			}

			tokens = append(tokens, pathToken{index: &index})
			path = path[end+1:]
		default:
			end := len(path)
			if dot := strings.IndexByte(path, '.'); dot >= 0 && dot < end {
				end = dot
			}
			if bracket := strings.IndexByte(path, '['); bracket >= 0 && bracket < end {
				end = bracket
			}
			key := strings.TrimSpace(path[:end])
			if key == "" {
				return nil, fmt.Errorf("empty object key in path")
			}
			tokens = append(tokens, pathToken{key: key})
			path = path[end:]
		}
	}

	return tokens, nil
}

func bracketSegmentIsKey(segment string) bool {
	return strings.HasPrefix(segment, `"`) || strings.HasPrefix(segment, `'`)
}

func unquotePathKey(segment string) (string, error) {
	if len(segment) < 2 {
		return "", fmt.Errorf("empty quoted key in path")
	}

	quote := segment[0]
	if segment[len(segment)-1] != quote {
		return "", fmt.Errorf("unterminated quoted key in path")
	}

	if quote == '"' {
		key, err := strconv.Unquote(segment)
		if err != nil {
			return "", fmt.Errorf("parse quoted key %q: %w", segment, err)
		}

		return key, nil
	}

	key := strings.TrimSuffix(strings.TrimPrefix(segment, "'"), "'")
	key = strings.ReplaceAll(key, `\'`, `'`)
	key = strings.ReplaceAll(key, `\\`, `\`)

	return key, nil
}

func validateSchema(value any, schema any, path string) []string {
	schemaMap, ok := normalizeValue(schema).(map[string]any)
	if !ok {
		return []string{path + ": schema must be an object"}
	}

	var failures []string
	if typeSpec, ok := schemaMap["type"]; ok && !schemaTypeMatches(value, typeSpec) {
		failures = append(failures, fmt.Sprintf("%s: expected type %s, got %s", path, valueToString(typeSpec), schemaTypeName(value)))
	}

	if constValue, ok := schemaMap["const"]; ok && !valuesEqual(value, constValue) {
		failures = append(failures, fmt.Sprintf("%s: expected const %s", path, valueToString(constValue)))
	}

	failures = append(failures, validateSchemaEnum(value, schemaMap, path)...)
	failures = append(failures, validateSchemaNumericBounds(value, schemaMap, path)...)
	failures = append(failures, validateSchemaStringBounds(value, schemaMap, path)...)
	failures = append(failures, validateSchemaStringPattern(value, schemaMap, path)...)
	failures = append(failures, validateSchemaArrayBounds(value, schemaMap, path)...)

	object, objectOK := value.(map[string]any)
	if required, ok := stringSlice(schemaMap["required"]); ok {
		if !objectOK {
			failures = append(failures, path+": required fields need an object")
		} else {
			for _, field := range required {
				if _, exists := object[field]; !exists {
					failures = append(failures, fmt.Sprintf("%s.%s: required field missing", path, field))
				}
			}
		}
	}

	properties, propertiesOK := schemaMap["properties"].(map[string]any)
	if _, exists := schemaMap["properties"]; exists && !propertiesOK {
		failures = append(failures, path+": properties must be an object")
	}
	if propertiesOK {
		if !objectOK {
			failures = append(failures, path+": properties need an object")
		} else {
			for field, propertySchema := range properties {
				if fieldValue, exists := object[field]; exists {
					failures = append(failures, validateSchema(fieldValue, propertySchema, path+"."+field)...)
				}
			}
		}
	}

	if additional, ok := boolValue(schemaMap["additionalProperties"]); ok && !additional && objectOK {
		for field := range object {
			if _, allowed := properties[field]; !allowed {
				failures = append(failures, fmt.Sprintf("%s.%s: additional property not allowed", path, field))
			}
		}
	}

	if itemSchema, ok := schemaMap["items"]; ok {
		items, ok := value.([]any)
		if !ok {
			failures = append(failures, path+": items need an array")
		} else {
			for i, item := range items {
				failures = append(failures, validateSchema(item, itemSchema, fmt.Sprintf("%s[%d]", path, i))...)
			}
		}
	}

	return failures
}

func validateSchemaEnum(value any, schemaMap map[string]any, path string) []string {
	rawEnum, exists := schemaMap["enum"]
	if !exists {
		return nil
	}

	enumValues, ok := schemaEnumValues(rawEnum)
	if !ok {
		return []string{path + ": enum must be an array"}
	}

	for _, enumValue := range enumValues {
		if valuesEqual(value, enumValue) {
			return nil
		}
	}

	return []string{fmt.Sprintf("%s: value is not in enum", path)}
}

func schemaEnumValues(value any) ([]any, bool) {
	value = normalizeValue(value)
	if values, ok := value.([]any); ok {
		return values, true
	}
	if value == nil {
		return nil, false
	}

	reflected := reflect.ValueOf(value)
	if reflected.Kind() != reflect.Slice && reflected.Kind() != reflect.Array {
		return nil, false
	}

	values := make([]any, 0, reflected.Len())
	for i := range reflected.Len() {
		values = append(values, normalizeValue(reflected.Index(i).Interface()))
	}

	return values, true
}

func validateSchemaNumericBounds(value any, schemaMap map[string]any, path string) []string {
	number, numberOK := valueFloat(value)
	var failures []string

	if minimum, ok := valueFloat(schemaMap["minimum"]); ok {
		if !numberOK {
			failures = append(failures, path+": minimum needs a number")
		} else if number < minimum {
			failures = append(failures, fmt.Sprintf("%s: number below minimum %s", path, valueToString(schemaMap["minimum"])))
		}
	}

	if maximum, ok := valueFloat(schemaMap["maximum"]); ok {
		if !numberOK {
			failures = append(failures, path+": maximum needs a number")
		} else if number > maximum {
			failures = append(failures, fmt.Sprintf("%s: number above maximum %s", path, valueToString(schemaMap["maximum"])))
		}
	}

	return failures
}

func validateSchemaStringBounds(value any, schemaMap map[string]any, path string) []string {
	text, textOK := value.(string)
	length := len([]rune(text))
	var failures []string

	if minLength, ok := valueInt(schemaMap["minLength"]); ok {
		if !textOK {
			failures = append(failures, path+": minLength needs a string")
		} else if length < minLength {
			failures = append(failures, fmt.Sprintf("%s: string shorter than minLength %d", path, minLength))
		}
	}

	if maxLength, ok := valueInt(schemaMap["maxLength"]); ok {
		if !textOK {
			failures = append(failures, path+": maxLength needs a string")
		} else if length > maxLength {
			failures = append(failures, fmt.Sprintf("%s: string longer than maxLength %d", path, maxLength))
		}
	}

	return failures
}

func validateSchemaStringPattern(value any, schemaMap map[string]any, path string) []string {
	pattern, ok := schemaMap["pattern"].(string)
	if !ok {
		return nil
	}

	text, textOK := value.(string)
	if !textOK {
		return []string{path + ": pattern needs a string"}
	}

	re, err := regexp.Compile(pattern)
	if err != nil {
		return []string{fmt.Sprintf("%s: invalid pattern %q: %v", path, pattern, err)}
	}

	if !re.MatchString(text) {
		return []string{fmt.Sprintf("%s: string does not match pattern %q", path, pattern)}
	}

	return nil
}

func validateSchemaArrayBounds(value any, schemaMap map[string]any, path string) []string {
	items, itemsOK := value.([]any)
	var failures []string

	if minItems, ok := valueInt(schemaMap["minItems"]); ok {
		if !itemsOK {
			failures = append(failures, path+": minItems needs an array")
		} else if len(items) < minItems {
			failures = append(failures, fmt.Sprintf("%s: array shorter than minItems %d", path, minItems))
		}
	}

	if maxItems, ok := valueInt(schemaMap["maxItems"]); ok {
		if !itemsOK {
			failures = append(failures, path+": maxItems needs an array")
		} else if len(items) > maxItems {
			failures = append(failures, fmt.Sprintf("%s: array longer than maxItems %d", path, maxItems))
		}
	}

	return failures
}

func schemaTypeMatches(value any, typeSpec any) bool {
	if schemaType, ok := typeSpec.(string); ok {
		return schemaSingleTypeMatches(value, schemaType)
	}

	if types, ok := stringSlice(typeSpec); ok {
		for _, schemaType := range types {
			if schemaSingleTypeMatches(value, schemaType) {
				return true
			}
		}
		return false
	}

	return false
}

func schemaSingleTypeMatches(value any, schemaType string) bool {
	switch schemaType {
	case "object":
		_, ok := value.(map[string]any)
		return ok
	case "array":
		_, ok := value.([]any)
		return ok
	case "string":
		_, ok := value.(string)
		return ok
	case "number":
		_, ok := valueFloat(value)
		return ok
	case "integer":
		number, ok := valueFloat(value)
		return ok && math.Trunc(number) == number
	case "boolean":
		_, ok := value.(bool)
		return ok
	case "null":
		return value == nil
	default:
		return false
	}
}

func schemaTypeName(value any) string {
	switch value.(type) {
	case map[string]any:
		return "object"
	case []any:
		return "array"
	case string:
		return "string"
	case json.Number, float64, float32, int, int64, int32, uint, uint64, uint32:
		return "number"
	case bool:
		return "boolean"
	case nil:
		return "null"
	default:
		return fmt.Sprintf("%T", value)
	}
}

func stringSlice(value any) ([]string, bool) {
	switch typed := value.(type) {
	case []string:
		return typed, true
	case []any:
		out := make([]string, 0, len(typed))
		for _, item := range typed {
			text, ok := item.(string)
			if !ok {
				return nil, false
			}
			out = append(out, text)
		}
		return out, true
	case string:
		return []string{typed}, true
	default:
		return nil, false
	}
}

func boolValue(value any) (bool, bool) {
	typed, ok := value.(bool)
	return typed, ok
}

func valuesEqual(left, right any) bool {
	left = normalizeValue(left)
	right = normalizeValue(right)

	if leftNumber, ok := valueFloat(left); ok {
		if rightNumber, ok := valueFloat(right); ok {
			return leftNumber == rightNumber
		}
	}

	return reflect.DeepEqual(left, right)
}

func valueFloat(value any) (float64, bool) {
	switch typed := value.(type) {
	case float64:
		return typed, true
	case float32:
		return float64(typed), true
	case int:
		return float64(typed), true
	case int64:
		return float64(typed), true
	case int32:
		return float64(typed), true
	case uint:
		return float64(typed), true
	case uint64:
		return float64(typed), true
	case uint32:
		return float64(typed), true
	case json.Number:
		parsed, err := typed.Float64()
		return parsed, err == nil
	case string:
		parsed, err := strconv.ParseFloat(strings.TrimSpace(typed), 64)
		return parsed, err == nil
	default:
		return 0, false
	}
}

func valueInt(value any) (int, bool) {
	switch typed := value.(type) {
	case int:
		return typed, true
	case int64:
		return int(typed), true
	case int32:
		return int(typed), true
	case float64:
		if math.Trunc(typed) != typed {
			return 0, false
		}
		return int(typed), true
	case json.Number:
		parsed, err := typed.Int64()
		return int(parsed), err == nil
	case string:
		parsed, err := strconv.Atoi(strings.TrimSpace(typed))
		return parsed, err == nil
	default:
		return 0, false
	}
}

func valueToString(value any) string {
	value = normalizeValue(value)
	switch typed := value.(type) {
	case string:
		return typed
	case json.Number:
		return typed.String()
	default:
		data, err := json.Marshal(typed)
		if err == nil {
			return string(data)
		}
		return fmt.Sprint(typed)
	}
}

func numericExpectationDescription(assertion Assertion) string {
	var parts []string
	if assertion.Equals != nil {
		parts = append(parts, "equals="+valueToString(assertion.Equals))
	}
	if assertion.Tolerance != nil {
		parts = append(parts, "tolerance="+strconv.FormatFloat(*assertion.Tolerance, 'f', -1, 64))
	}
	if assertion.Min != nil {
		parts = append(parts, "min="+strconv.FormatFloat(*assertion.Min, 'f', -1, 64))
	}
	if assertion.Max != nil {
		parts = append(parts, "max="+strconv.FormatFloat(*assertion.Max, 'f', -1, 64))
	}
	return strings.Join(parts, " ")
}
