package watch

import (
	"strings"
)

const (
	// DefaultQualityGateName is the default gate label used by EvaluateGate.
	DefaultQualityGateName = "watch-quality-gate"
)

// Baseline captures acknowledged findings at a branch point or previous scan.
type Baseline struct {
	Findings []Finding `json:"findings"`
}

// TrendMetrics summarizes how a scan changed relative to a baseline.
type TrendMetrics struct {
	New        int `json:"new"`
	Fixed      int `json:"fixed"`
	Unchanged  int `json:"unchanged"`
	Suppressed int `json:"suppressed"`
	Unstable   int `json:"unstable"`
}

// Comparison distinguishes new regressions from acknowledged debt.
type Comparison struct {
	NewFindings        []Finding    `json:"new_findings"`
	FixedFindings      []Finding    `json:"fixed_findings"`
	UnchangedFindings  []Finding    `json:"unchanged_findings"`
	SuppressedFindings []Finding    `json:"suppressed_findings"`
	UnstableFindings   []Finding    `json:"unstable_findings"`
	Metrics            TrendMetrics `json:"metrics"`
}

// GateOptions configures quality gate evaluation.
type GateOptions struct {
	Name        string `json:"name,omitempty"`
	MinSeverity string `json:"min_severity,omitempty"`
	Enabled     bool   `json:"enabled,omitempty"`
}

// GateResult records whether a comparison should block work.
type GateResult struct {
	Name             string    `json:"name"`
	Reason           string    `json:"reason"`
	BlockingFindings []Finding `json:"blocking_findings,omitempty"`
	Passed           bool      `json:"passed"`
}

// CompareFindings compares a current scan against a baseline. Matching is based
// on stable finding fingerprints, not volatile finding text.
func CompareFindings(baseline, current []Finding) Comparison {
	baselineIndex, baselineDuplicates := indexFindings(baseline)
	currentIndex, currentDuplicates := indexFindings(current)

	comparison := Comparison{
		NewFindings:        []Finding{},
		FixedFindings:      []Finding{},
		UnchangedFindings:  []Finding{},
		SuppressedFindings: []Finding{},
		UnstableFindings:   []Finding{},
	}
	unstableSeen := make(map[string]struct{})

	for i := range baselineDuplicates {
		comparison.addUnstableFinding(baselineDuplicates[i], unstableSeen)
	}

	for i := range currentDuplicates {
		comparison.addUnstableFinding(currentDuplicates[i], unstableSeen)
	}

	for fingerprint := range currentIndex {
		finding := currentIndex[fingerprint]
		if finding.Suppressed {
			comparison.SuppressedFindings = append(comparison.SuppressedFindings, finding)
			continue
		}

		if _, ok := baselineIndex[fingerprint]; ok {
			comparison.UnchangedFindings = append(comparison.UnchangedFindings, finding)
			continue
		}

		comparison.NewFindings = append(comparison.NewFindings, finding)
	}

	for fingerprint := range baselineIndex {
		finding := baselineIndex[fingerprint]
		if _, ok := currentIndex[fingerprint]; !ok {
			comparison.FixedFindings = append(comparison.FixedFindings, finding)
		}
	}

	sortFindings(comparison.NewFindings)
	sortFindings(comparison.FixedFindings)
	sortFindings(comparison.UnchangedFindings)
	sortFindings(comparison.SuppressedFindings)
	sortFindings(comparison.UnstableFindings)

	comparison.removeUnstableFromClassifiedFindings()
	comparison.recalculateMetrics()

	return comparison
}

func (c *Comparison) addUnstableFinding(finding Finding, seen map[string]struct{}) {
	key := finding.Fingerprint
	if key == "" {
		key = finding.ID
	}

	if _, ok := seen[key]; ok {
		return
	}

	seen[key] = struct{}{}

	c.UnstableFindings = append(c.UnstableFindings, finding)
}

// EvaluateGate fails when new, unsuppressed findings meet the configured
// severity threshold. Existing baseline findings do not keep failing forever.
func EvaluateGate(comparison Comparison, options GateOptions) GateResult {
	name := strings.TrimSpace(options.Name)
	if name == "" {
		name = DefaultQualityGateName
	}

	minSeverity := strings.TrimSpace(options.MinSeverity)
	if minSeverity == "" {
		minSeverity = SeverityHigh
	}

	result := GateResult{Name: name, Passed: true}
	if !validSeverity(minSeverity) {
		result.Passed = false
		result.Reason = "invalid minimum severity " + minSeverity

		return result
	}

	unstable := unstableFindingFingerprints(comparison)
	for i := range comparison.NewFindings {
		finding := comparison.NewFindings[i]
		if finding.Suppressed {
			continue
		}

		finding = completeFindingIdentity(finding)
		if unstable[finding.Fingerprint] {
			continue
		}

		if severityAtLeast(finding.Severity, minSeverity) {
			result.BlockingFindings = append(result.BlockingFindings, finding)
		}
	}

	if len(result.BlockingFindings) > 0 {
		result.Passed = false
		result.Reason = "new findings meet or exceed " + minSeverity + " severity"

		return result
	}

	result.Reason = "no new findings meet or exceed " + minSeverity + " severity"

	return result
}

func unstableFindingFingerprints(comparison Comparison) map[string]bool {
	unstable := make(map[string]bool, len(comparison.UnstableFindings))
	for i := range comparison.UnstableFindings {
		finding := completeFindingIdentity(comparison.UnstableFindings[i])
		if finding.Fingerprint != "" {
			unstable[finding.Fingerprint] = true
		}
	}

	return unstable
}

func (c *Comparison) removeUnstableFromClassifiedFindings() {
	unstable := unstableFindingFingerprints(*c)
	if len(unstable) == 0 {
		return
	}

	c.NewFindings = withoutFingerprints(c.NewFindings, unstable)
	c.FixedFindings = withoutFingerprints(c.FixedFindings, unstable)
	c.UnchangedFindings = withoutFingerprints(c.UnchangedFindings, unstable)
	c.SuppressedFindings = withoutFingerprints(c.SuppressedFindings, unstable)
}

func withoutFingerprints(findings []Finding, fingerprints map[string]bool) []Finding {
	filtered := make([]Finding, 0, len(findings))
	for i := range findings {
		finding := completeFindingIdentity(findings[i])
		if !fingerprints[finding.Fingerprint] {
			filtered = append(filtered, finding)
		}
	}

	return filtered
}

func (c *Comparison) recalculateMetrics() {
	c.Metrics = TrendMetrics{
		New:        len(c.NewFindings),
		Fixed:      len(c.FixedFindings),
		Unchanged:  len(c.UnchangedFindings),
		Suppressed: len(c.SuppressedFindings),
		Unstable:   len(c.UnstableFindings),
	}
}

func indexFindings(findings []Finding) (map[string]Finding, []Finding) {
	index := make(map[string]Finding, len(findings))

	var duplicates []Finding

	for i := range findings {
		finding := findings[i]
		finding = completeFindingIdentity(finding)

		key := finding.Fingerprint
		if key == "" {
			key = finding.ID
		}

		if _, exists := index[key]; exists {
			duplicates = append(duplicates, finding)
			continue
		}

		index[key] = finding
	}

	return index, duplicates
}

func severityAtLeast(got, minimum string) bool {
	return severityRank(got) <= severityRank(minimum)
}

func severityRank(severity string) int {
	switch strings.TrimSpace(severity) {
	case SeverityHigh:
		return 0
	case SeverityWarning:
		return 1
	case SeverityMaintenance:
		return 2
	case SeverityInfo:
		return 3
	default:
		return 4
	}
}
