//nolint:cyclop,gocognit,goconst,gocritic,govet,modernize,nestif,perfsprint,revive,staticcheck,wrapcheck,wsl_v5 // Structured eval keeps explicit dependency-free assertion, path, and schema logic.
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
	AssertionSchema         AssertionType = "schema"
	AssertionNumeric        AssertionType = "numeric"
	AssertionArtifactExists AssertionType = "artifact_exists"
	AssertionExitCode       AssertionType = "exit_code"
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

	assertionStatusPass = "pass"
	assertionStatusFail = "fail"
)

// Metadata records the provenance of a structured eval fixture.
type Metadata struct {
	TargetCommand string            `json:"target_command,omitempty" yaml:"target_command,omitempty"`
	Model         string            `json:"model,omitempty" yaml:"model,omitempty"`
	Agent         string            `json:"agent,omitempty" yaml:"agent,omitempty"`
	InputFixture  string            `json:"input_fixture,omitempty" yaml:"input_fixture,omitempty"`
	Environment   map[string]string `json:"environment,omitempty" yaml:"environment,omitempty"`
	CreatedAt     string            `json:"created_at,omitempty" yaml:"created_at,omitempty"`
	UpdatedAt     string            `json:"updated_at,omitempty" yaml:"updated_at,omitempty"`
	Owner         string            `json:"owner,omitempty" yaml:"owner,omitempty"`
}

// Suite is a YAML/JSON assertion file for one output eval.
type Suite struct {
	Version    int         `json:"version,omitempty" yaml:"version,omitempty"`
	Metadata   Metadata    `json:"metadata,omitempty" yaml:"metadata,omitempty"`
	ActualPath string      `json:"actual,omitempty" yaml:"actual,omitempty"`
	GoldenPath string      `json:"golden,omitempty" yaml:"golden,omitempty"`
	ExitCode   *int        `json:"exit_code,omitempty" yaml:"exit_code,omitempty"`
	Assertions []Assertion `json:"assertions,omitempty" yaml:"assertions,omitempty"`
}

// Assertion describes one typed check in a structured eval suite.
type Assertion struct {
	ID          string         `json:"id,omitempty" yaml:"id,omitempty"`
	Type        AssertionType  `json:"type" yaml:"type"`
	Severity    Severity       `json:"severity,omitempty" yaml:"severity,omitempty"`
	Value       string         `json:"value,omitempty" yaml:"value,omitempty"`
	Pattern     string         `json:"pattern,omitempty" yaml:"pattern,omitempty"`
	Path        string         `json:"path,omitempty" yaml:"path,omitempty"`
	Equals      any            `json:"equals,omitempty" yaml:"equals,omitempty"`
	Contains    string         `json:"contains,omitempty" yaml:"contains,omitempty"`
	NotContains string         `json:"not_contains,omitempty" yaml:"not_contains,omitempty"`
	Matches     string         `json:"matches,omitempty" yaml:"matches,omitempty"`
	Min         *float64       `json:"min,omitempty" yaml:"min,omitempty"`
	Max         *float64       `json:"max,omitempty" yaml:"max,omitempty"`
	Tolerance   *float64       `json:"tolerance,omitempty" yaml:"tolerance,omitempty"`
	Schema      map[string]any `json:"schema,omitempty" yaml:"schema,omitempty"`
	SchemaPath  string         `json:"schema_file,omitempty" yaml:"schema_file,omitempty"`
	Exists      *bool          `json:"exists,omitempty" yaml:"exists,omitempty"`
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
	Summary   ReportSummary `json:"summary"`
	ActualRef string        `json:"actual_ref,omitempty"`
}

// ReportSummary counts assertion outcomes in a structured eval report.
type ReportSummary struct {
	Total    int `json:"total"`
	Passed   int `json:"passed"`
	Failed   int `json:"failed"`
	Warnings int `json:"warnings"`
}

// AssertionResult is one per-assertion machine-readable result.
type AssertionResult struct {
	ID              string        `json:"id"`
	Type            AssertionType `json:"type"`
	Severity        Severity      `json:"severity"`
	Status          string        `json:"status"`
	Passed          bool          `json:"passed"`
	Suite           string        `json:"suite,omitempty"`
	Evidence        string        `json:"evidence,omitempty"`
	ExpectedSnippet string        `json:"expected_snippet,omitempty"`
	ActualSnippet   string        `json:"actual_snippet,omitempty"`
	Remediation     string        `json:"remediation,omitempty"`
	Error           string        `json:"error,omitempty"`
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
	for _, path := range paths {
		suiteOpts := opts
		suiteOpts.ArtifactRoot = filepath.Dir(path)
		suiteOpts.SuitePath = path

		suiteReport, err := RunSuiteFile(path, suiteOpts)
		if err != nil {
			return Report{}, err
		}

		report.Results = append(report.Results, suiteReport.Results...)
		report.Suites = append(report.Suites, SuiteReport{
			Path:      path,
			Passed:    suiteReport.Passed,
			Metadata:  suiteReport.Metadata,
			Summary:   suiteReport.Summary,
			ActualRef: suiteReport.ActualRef,
		})
	}

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

	if len(assertions) == 0 && len(report.Results) == 0 {
		return Report{}, errors.New("eval suite has no assertions")
	}

	for i, assertion := range assertions {
		report.Results = append(report.Results, evaluateAssertion(assertion, i, actual, opts))
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
	report.Summary = ReportSummary{Total: len(report.Results)}
	report.Passed = true
	for i := range report.Results {
		if report.Results[i].Passed {
			report.Summary.Passed++
		} else {
			report.Summary.Failed++
			report.Passed = false
		}
		if report.Results[i].Severity == SeverityWarning {
			report.Summary.Warnings++
		}
	}
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

func evaluateAssertion(assertion Assertion, index int, actual string, opts RunOptions) AssertionResult {
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
		expected := textExpectation(assertion)
		if expected == "" {
			return failResult(assertion, opts, "contains assertion is missing value", "", actual, "")
		}
		if strings.Contains(actual, expected) {
			return passResult(assertion, opts, "actual output contained expected text")
		}
		return failResult(assertion, opts, "actual output did not contain expected text", expected, actual, containsSnippet(expected, actual))
	case AssertionNotContains:
		forbidden := textExpectation(assertion)
		if forbidden == "" {
			return failResult(assertion, opts, "not_contains assertion is missing value", "", actual, "")
		}
		if !strings.Contains(actual, forbidden) {
			return passResult(assertion, opts, "forbidden content was absent")
		}
		return failResult(assertion, opts, "actual output contained forbidden content", forbidden, actual, "forbidden content was present")
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
	case AssertionSchema:
		return evaluateSchemaAssertion(assertion, actual, opts)
	case AssertionNumeric:
		return evaluateNumericAssertion(assertion, actual, opts)
	case AssertionArtifactExists:
		return evaluateArtifactAssertion(assertion, opts)
	case AssertionExitCode:
		return evaluateExitCodeAssertion(assertion, opts)
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
	case "contains":
		return AssertionContains
	case "not_contains", "not-contains", "forbidden", "forbidden_content", "forbidden-content":
		return AssertionNotContains
	case "regex", "matches":
		return AssertionRegex
	case "json_path", "json-path", "jsonpath":
		return AssertionJSONPath
	case "yaml_path", "yaml-path", "yamlpath":
		return AssertionYAMLPath
	case "schema", "json_schema", "json-schema":
		return AssertionSchema
	case "numeric", "number":
		return AssertionNumeric
	case "artifact_exists", "artifact-exists", "artifact":
		return AssertionArtifactExists
	case "exit_code", "exit-code":
		return AssertionExitCode
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

	if enumValues, ok := schemaMap["enum"].([]any); ok {
		matched := false
		for _, enumValue := range enumValues {
			if valuesEqual(value, enumValue) {
				matched = true
				break
			}
		}
		if !matched {
			failures = append(failures, fmt.Sprintf("%s: value is not in enum", path))
		}
	}

	failures = append(failures, validateSchemaNumericBounds(value, schemaMap, path)...)
	failures = append(failures, validateSchemaStringBounds(value, schemaMap, path)...)
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

	if properties, ok := schemaMap["properties"].(map[string]any); ok {
		if !objectOK {
			failures = append(failures, path+": properties need an object")
		} else {
			for field, propertySchema := range properties {
				if fieldValue, exists := object[field]; exists {
					failures = append(failures, validateSchema(fieldValue, propertySchema, path+"."+field)...)
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
