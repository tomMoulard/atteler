// Package review provides dependency-free code review primitives.
package review

import (
	"errors"
	"fmt"
	"sort"
	"strings"
)

// Severity describes the impact of a finding.
type Severity string

const (
	// SeverityCritical marks issues that should block release immediately.
	SeverityCritical Severity = "critical"
	// SeverityHigh marks issues that are likely to cause user-visible failures.
	SeverityHigh Severity = "high"
	// SeverityMedium marks issues that should be fixed before merging when practical.
	SeverityMedium Severity = "medium"
	// SeverityLow marks minor defects or maintainability concerns.
	SeverityLow Severity = "low"
	// SeverityInfo marks non-blocking observations.
	SeverityInfo Severity = "info"
)

// Category describes the review lens that produced a finding.
type Category string

const (
	// CategoryCorrectness covers behavioral bugs and regressions.
	CategoryCorrectness Category = "correctness"
	// CategorySecurity covers trust-boundary and vulnerability concerns.
	CategorySecurity Category = "security"
	// CategoryTests covers missing or weak verification.
	CategoryTests Category = "tests"
	// CategoryPerformance covers latency, memory, and scalability concerns.
	CategoryPerformance Category = "performance"
	// CategoryMaintainability covers readability and long-term change risk.
	CategoryMaintainability Category = "maintainability"
	// CategoryStyle covers local style and consistency issues.
	CategoryStyle Category = "style"
)

// DefaultRequiredGates names the baseline gates expected for a code-review verdict.
var DefaultRequiredGates = []string{
	"tests pass",
	"types pass",
	"lint pass",
	"no new flakes",
	"behavioral diff reviewed",
}

// Finding captures one actionable code-review observation.
//
//nolint:govet // Public field order follows review readability.
type Finding struct {
	Severity   Severity
	Category   Category
	Path       string
	Line       int
	Message    string
	Suggestion string
}

// Reviewer describes a review participant and the categories they cover.
type Reviewer struct {
	Name       string
	Categories []Category
}

// ChangedFile identifies a file included in a review request.
type ChangedFile struct {
	Path   string
	Status string
}

// Request describes the code surface being reviewed.
type Request struct {
	ChangedFiles []ChangedFile
	Paths        []string
}

// GateCheck records whether one required review gate passed.
//
//nolint:govet // Public field order follows semantic readability.
type GateCheck struct {
	Name   string
	Passed bool
	Notes  string
}

// Report is the structured output of a code review.
type Report struct {
	Reviewer   string
	Findings   []Finding
	GateChecks []GateCheck
}

// SeveritySummary counts findings by severity.
type SeveritySummary struct {
	Critical int
	High     int
	Medium   int
	Low      int
	Info     int
}

// FindingGroup groups sorted findings under a deterministic key.
type FindingGroup struct {
	Key      string
	Findings []Finding
}

var severityRank = map[Severity]int{
	SeverityCritical: 0,
	SeverityHigh:     1,
	SeverityMedium:   2,
	SeverityLow:      3,
	SeverityInfo:     4,
}

// ValidateReviewer verifies reviewer identity and category metadata.
func ValidateReviewer(reviewer Reviewer) error {
	if strings.TrimSpace(reviewer.Name) == "" {
		return errors.New("reviewer name is required")
	}
	_, err := normalizeCategories(reviewer.Categories)
	return err
}

// ValidateRequest verifies that a review request names at least one target path.
func ValidateRequest(request Request) error {
	paths := make([]string, 0, len(request.Paths)+len(request.ChangedFiles))
	paths = append(paths, request.Paths...)
	for _, file := range request.ChangedFiles {
		if strings.TrimSpace(file.Status) == "" {
			return fmt.Errorf("changed file %q status is required", strings.TrimSpace(file.Path))
		}
		paths = append(paths, file.Path)
	}

	normalized, err := normalizeUnique("review path", paths)
	if err != nil {
		return err
	}
	if len(normalized) == 0 {
		return errors.New("at least one review path is required")
	}
	return nil
}

// ValidateFinding verifies that a finding has complete actionable metadata.
func ValidateFinding(finding Finding) error {
	if !validSeverity(finding.Severity) {
		return fmt.Errorf("invalid severity %q", finding.Severity)
	}
	if strings.TrimSpace(string(finding.Category)) == "" {
		return errors.New("finding category is required")
	}
	if strings.TrimSpace(finding.Path) == "" {
		return errors.New("finding path is required")
	}
	if finding.Line < 0 {
		return fmt.Errorf("finding line must be non-negative, got %d", finding.Line)
	}
	if strings.TrimSpace(finding.Message) == "" {
		return errors.New("finding message is required")
	}
	return nil
}

// ValidateReport validates findings and every required gate check in a report.
func ValidateReport(report Report, requiredGates []string) error {
	if strings.TrimSpace(report.Reviewer) == "" {
		return errors.New("report reviewer is required")
	}
	for i, finding := range report.Findings {
		if err := ValidateFinding(finding); err != nil {
			return fmt.Errorf("finding %d: %w", i, err)
		}
	}
	return ValidateGateChecks(requiredGates, report.GateChecks)
}

// ValidateGateChecks verifies that required gates are present, unique, known, and passed.
func ValidateGateChecks(required []string, checks []GateCheck) error {
	required, err := normalizeUnique("required gate check", required)
	if err != nil {
		return err
	}

	requiredSet := make(map[string]struct{}, len(required))
	for _, name := range required {
		requiredSet[name] = struct{}{}
	}

	seen := make(map[string]GateCheck, len(checks))
	for _, check := range checks {
		name := strings.TrimSpace(check.Name)
		if name == "" {
			return errors.New("gate check name is required")
		}
		if _, exists := seen[name]; exists {
			return fmt.Errorf("duplicate gate check %q", name)
		}
		if _, ok := requiredSet[name]; !ok {
			return fmt.Errorf("unknown gate check %q", name)
		}
		check.Name = name
		seen[name] = check
	}

	for _, name := range required {
		check, ok := seen[name]
		if !ok {
			return fmt.Errorf("missing gate check %q", name)
		}
		if !check.Passed {
			return fmt.Errorf("gate check %q failed", name)
		}
	}
	return nil
}

// SortedFindings returns a sorted copy of findings.
func SortedFindings(findings []Finding) []Finding {
	sorted := append([]Finding(nil), findings...)
	sort.SliceStable(sorted, func(i, j int) bool {
		return lessFinding(sorted[i], sorted[j])
	})
	return sorted
}

// SortedFindings returns the report findings in deterministic order.
func (report Report) SortedFindings() []Finding {
	return SortedFindings(report.Findings)
}

// GroupFindingsBySeverity returns deterministic groups in descending severity order.
func GroupFindingsBySeverity(findings []Finding) []FindingGroup {
	sorted := SortedFindings(findings)
	groups := make([]FindingGroup, 0, len(severityRank))
	for _, severity := range []Severity{SeverityCritical, SeverityHigh, SeverityMedium, SeverityLow, SeverityInfo} {
		group := FindingGroup{Key: string(severity)}
		for _, finding := range sorted {
			if finding.Severity == severity {
				group.Findings = append(group.Findings, finding)
			}
		}
		if len(group.Findings) > 0 {
			groups = append(groups, group)
		}
	}
	return groups
}

// GroupFindingsByCategory returns deterministic groups in category name order.
func GroupFindingsByCategory(findings []Finding) []FindingGroup {
	sorted := SortedFindings(findings)
	byCategory := make(map[string][]Finding)
	keys := make([]string, 0)
	for _, finding := range sorted {
		key := string(finding.Category)
		if _, ok := byCategory[key]; !ok {
			keys = append(keys, key)
		}
		byCategory[key] = append(byCategory[key], finding)
	}
	sort.Strings(keys)

	groups := make([]FindingGroup, 0, len(keys))
	for _, key := range keys {
		groups = append(groups, FindingGroup{Key: key, Findings: byCategory[key]})
	}
	return groups
}

// Summary returns counts of findings by severity.
func Summary(findings []Finding) SeveritySummary {
	var summary SeveritySummary
	for _, finding := range findings {
		switch finding.Severity {
		case SeverityCritical:
			summary.Critical++
		case SeverityHigh:
			summary.High++
		case SeverityMedium:
			summary.Medium++
		case SeverityLow:
			summary.Low++
		case SeverityInfo:
			summary.Info++
		}
	}
	return summary
}

// SeveritySummary returns counts of the report findings by severity.
func (report Report) SeveritySummary() SeveritySummary {
	return Summary(report.Findings)
}

// Total returns the total number of findings in the summary.
func (summary SeveritySummary) Total() int {
	return summary.Critical + summary.High + summary.Medium + summary.Low + summary.Info
}

func lessFinding(left, right Finding) bool {
	leftRank := severityRank[left.Severity]
	rightRank := severityRank[right.Severity]
	if leftRank != rightRank {
		return leftRank < rightRank
	}
	if left.Path != right.Path {
		return left.Path < right.Path
	}
	if left.Line != right.Line {
		return left.Line < right.Line
	}
	if left.Category != right.Category {
		return left.Category < right.Category
	}
	if left.Message != right.Message {
		return left.Message < right.Message
	}
	return left.Suggestion < right.Suggestion
}

func validSeverity(severity Severity) bool {
	_, ok := severityRank[severity]
	return ok
}

func normalizeCategories(categories []Category) ([]Category, error) {
	normalized := make([]Category, 0, len(categories))
	seen := make(map[Category]struct{}, len(categories))
	for _, category := range categories {
		category = Category(strings.TrimSpace(string(category)))
		if category == "" {
			return nil, errors.New("reviewer category is required")
		}
		if _, exists := seen[category]; exists {
			return nil, fmt.Errorf("duplicate reviewer category %q", category)
		}
		seen[category] = struct{}{}
		normalized = append(normalized, category)
	}
	return normalized, nil
}

func normalizeUnique(label string, values []string) ([]string, error) {
	normalized := make([]string, 0, len(values))
	seen := make(map[string]struct{}, len(values))
	for _, value := range values {
		value = strings.TrimSpace(value)
		if value == "" {
			return nil, fmt.Errorf("%s is required", label)
		}
		if _, exists := seen[value]; exists {
			return nil, fmt.Errorf("duplicate %s %q", label, value)
		}
		seen[value] = struct{}{}
		normalized = append(normalized, value)
	}
	return normalized, nil
}
