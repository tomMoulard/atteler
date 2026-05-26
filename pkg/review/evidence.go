package review

import (
	"errors"
	"fmt"
	"strconv"
	"strings"
)

const (
	confidenceHigh   = "high"
	confidenceMedium = "medium"
	confidenceLow    = "low"
)

type evidenceValidationOptions struct {
	aggregateSupporters map[string]struct{}
}

func validateEvidenceBackedReport(
	report Report,
	requiredGates []string,
	snapshot reviewSnapshot,
	options evidenceValidationOptions,
) error {
	if strings.TrimSpace(report.Reviewer) == "" {
		return errors.New("report reviewer is required")
	}

	seenFindings := make(map[string]struct{}, len(report.Findings))
	for i := range report.Findings {
		finding := report.Findings[i]
		if err := validateEvidenceBackedFinding(report.Reviewer, finding, snapshot, options); err != nil {
			return fmt.Errorf("finding %d: %w", i, err)
		}

		if isAggregateReviewer(report.Reviewer) && !hasAggregateFindingSupport(report.Reviewer, finding.Provenance, options.aggregateSupporters) {
			return fmt.Errorf("finding %d: aggregate finding provenance must include supporting reviewer or command-output source", i)
		}

		key := evidenceFindingKey(finding)
		if _, exists := seenFindings[key]; exists {
			return fmt.Errorf("duplicate finding %q", key)
		}

		seenFindings[key] = struct{}{}
	}

	return validateGateCheckEvidence(report.Reviewer, requiredGates, report.GateChecks, snapshot, options)
}

func validateEvidenceBackedFinding(
	reportReviewer string,
	finding Finding,
	snapshot reviewSnapshot,
	options evidenceValidationOptions,
) error {
	if err := ValidateFinding(finding); err != nil {
		return err
	}

	if !validCategory(finding.Category) {
		return fmt.Errorf("invalid category %q", finding.Category)
	}

	if finding.Line <= 0 {
		return fmt.Errorf("finding line must be positive for evidence-backed review, got %d", finding.Line)
	}

	if finding.EndLine <= 0 {
		return fmt.Errorf("finding end line must be positive for evidence-backed review, got %d", finding.EndLine)
	}

	if err := snapshot.validateRange(finding.Path, finding.Line, finding.EndLine); err != nil {
		return err
	}

	if err := validateFindingEvidenceFields(finding); err != nil {
		return err
	}

	if !snapshot.containsEvidenceInRange(finding.Path, finding.Line, finding.EndLine, finding.Evidence) {
		return fmt.Errorf("finding evidence must quote reviewed snapshot range %s:%d-%d", finding.Path, finding.Line, finding.EndLine)
	}

	return validateFindingProvenance(reportReviewer, finding, snapshot, options)
}

func validateFindingEvidenceFields(finding Finding) error {
	if strings.TrimSpace(finding.Evidence) == "" {
		return errors.New("finding evidence is required")
	}

	if strings.TrimSpace(finding.SeverityRationale) == "" {
		return errors.New("finding severity rationale is required")
	}

	if strings.TrimSpace(finding.SuggestedVerification) == "" {
		return errors.New("finding suggested verification is required")
	}

	if !validConfidence(finding.Confidence) {
		return fmt.Errorf("finding confidence must be one of high, medium, or low, got %q", finding.Confidence)
	}

	return nil
}

func validateFindingProvenance(
	reportReviewer string,
	finding Finding,
	snapshot reviewSnapshot,
	options evidenceValidationOptions,
) error {
	if err := validateEvidenceSources("finding provenance", finding.Provenance); err != nil {
		return err
	}

	if err := validateModelJudgmentSources("finding provenance", reportReviewer, finding.Provenance, options.aggregateSupporters); err != nil {
		return err
	}

	if err := validateCommandOutputSources("finding provenance", finding.Provenance, snapshot); err != nil {
		return err
	}

	if isAggregateReviewer(reportReviewer) {
		if !hasEvidenceType(finding.Provenance, EvidenceModelJudgment) {
			return errors.New("finding provenance must include model-judgment source")
		}
	} else if !hasModelJudgmentFrom(reportReviewer, finding.Provenance) {
		return fmt.Errorf("finding provenance must include model-judgment source %q", strings.TrimSpace(reportReviewer))
	}

	if !hasReviewContextForFinding(finding) {
		return fmt.Errorf("finding provenance must include review-context source for %s:%d-%d", finding.Path, finding.Line, finding.EndLine)
	}

	if err := validateEvidenceSourceContextRanges("finding provenance", finding.Provenance, snapshot); err != nil {
		return err
	}

	if err := validateOptionalEvidenceSources("finding dissent", finding.Dissent); err != nil {
		return err
	}

	if err := validateCommandOutputSources("finding dissent", finding.Dissent, snapshot); err != nil {
		return err
	}

	if err := validateEvidenceSourceContextRanges("finding dissent", finding.Dissent, snapshot); err != nil {
		return err
	}

	return nil
}

func validateGateCheckEvidence(
	reportReviewer string,
	required []string,
	checks []GateCheck,
	snapshot reviewSnapshot,
	options evidenceValidationOptions,
) error {
	required, err := normalizeUnique("required gate check", required)
	if err != nil {
		return err
	}

	requiredSet := make(map[string]struct{}, len(required))
	for _, name := range required {
		requiredSet[name] = struct{}{}
	}

	seen := make(map[string]struct{}, len(checks))
	for i, check := range checks {
		name, err := validateKnownGateCheck(i, check, reportReviewer, requiredSet, seen, snapshot, options)
		if err != nil {
			return err
		}

		seen[name] = struct{}{}
	}

	for _, name := range required {
		if _, ok := seen[name]; !ok {
			return fmt.Errorf("missing gate check %q", name)
		}
	}

	return nil
}

func validateKnownGateCheck(
	index int,
	check GateCheck,
	reportReviewer string,
	requiredSet map[string]struct{},
	seen map[string]struct{},
	snapshot reviewSnapshot,
	options evidenceValidationOptions,
) (string, error) {
	name := strings.TrimSpace(check.Name)
	if name == "" {
		return "", fmt.Errorf("gate check %d: name is required", index)
	}

	if _, exists := seen[name]; exists {
		return "", fmt.Errorf("duplicate gate check %q", name)
	}

	if _, ok := requiredSet[name]; !ok {
		return "", fmt.Errorf("unknown gate check %q", name)
	}

	if err := validateGateCheckProof(check, name); err != nil {
		return "", err
	}

	if err := validateEvidenceSources("gate check provenance", check.Provenance); err != nil {
		return "", fmt.Errorf("gate check %q: %w", name, err)
	}

	if err := validateModelJudgmentSources("gate check provenance", reportReviewer, check.Provenance, options.aggregateSupporters); err != nil {
		return "", fmt.Errorf("gate check %q: %w", name, err)
	}

	if err := validateCommandOutputSources("gate check provenance", check.Provenance, snapshot); err != nil {
		return "", fmt.Errorf("gate check %q: %w", name, err)
	}

	if err := validateEvidenceSourceContextRanges("gate check provenance", check.Provenance, snapshot); err != nil {
		return "", fmt.Errorf("gate check %q: %w", name, err)
	}

	if err := validateGateCheckProvenanceRequirements(check, name); err != nil {
		return "", err
	}

	if err := validateGateCheckProofEvidence(check, name, snapshot); err != nil {
		return "", err
	}

	return name, nil
}

func validateGateCheckProvenanceRequirements(check GateCheck, name string) error {
	if strings.TrimSpace(check.NotRunReason) != "" && !hasEvidenceType(check.Provenance, EvidenceModelJudgment) {
		return fmt.Errorf("gate check %q not_run_reason requires model-judgment provenance", name)
	}

	if strings.TrimSpace(check.Proof) == "" {
		return nil
	}

	if gateRequiresCommandOutput(name) && !hasEvidenceType(check.Provenance, EvidenceCommandOutput) {
		return fmt.Errorf("gate check %q requires command-output provenance", name)
	}

	if !hasHardEvidence(check.Provenance) {
		return fmt.Errorf("gate check %q requires review-context or command-output provenance for proof", name)
	}

	return nil
}

func validateGateCheckProofEvidence(check GateCheck, name string, snapshot reviewSnapshot) error {
	proof := strings.TrimSpace(check.Proof)
	if proof == "" {
		return nil
	}

	if hasEvidenceType(check.Provenance, EvidenceCommandOutput) {
		if !hasCommandOutputGateProof(proof, check.Provenance) {
			return fmt.Errorf("gate check %q command-output provenance must support proof", name)
		}

		if !snapshot.containsCommandEvidence(proof) {
			return fmt.Errorf("gate check %q proof was not found in review context", name)
		}

		return nil
	}

	if hasEvidenceType(check.Provenance, EvidenceReviewContext) && !hasReviewContextGateProof(proof, check.Provenance, snapshot) {
		return fmt.Errorf("gate check %q proof was not found in review-context provenance ranges", name)
	}

	return nil
}

func validateGateCheckProof(check GateCheck, name string) error {
	proof := strings.TrimSpace(check.Proof)
	notRunReason := strings.TrimSpace(check.NotRunReason)

	switch {
	case proof == "" && notRunReason == "":
		return fmt.Errorf("gate check %q requires proof or not_run_reason", name)
	case proof != "" && notRunReason != "":
		return fmt.Errorf("gate check %q cannot include both proof and not_run_reason", name)
	case check.Passed && notRunReason != "":
		return fmt.Errorf("gate check %q cannot pass with not_run_reason", name)
	default:
		return nil
	}
}

func validateEvidenceSources(label string, sources []EvidenceSource) error {
	if len(sources) == 0 {
		return fmt.Errorf("%s is required", label)
	}

	return validateOptionalEvidenceSources(label, sources)
}

func validateOptionalEvidenceSources(label string, sources []EvidenceSource) error {
	for i, source := range sources {
		if !validEvidenceType(source.Type) {
			return fmt.Errorf("%s %d has invalid type %q", label, i, source.Type)
		}

		if strings.TrimSpace(source.Source) == "" {
			return fmt.Errorf("%s %d source is required", label, i)
		}

		if strings.TrimSpace(source.Summary) == "" {
			return fmt.Errorf("%s %d summary is required", label, i)
		}
	}

	return nil
}

func validateModelJudgmentSources(
	label,
	reportReviewer string,
	sources []EvidenceSource,
	allowedAggregateSources map[string]struct{},
) error {
	reportReviewer = strings.TrimSpace(reportReviewer)

	for i, source := range sources {
		if source.Type != EvidenceModelJudgment {
			continue
		}

		sourceName := strings.TrimSpace(source.Source)
		if sourceName == reportReviewer {
			continue
		}

		if isAggregateReviewer(reportReviewer) {
			if sourceName == reviewJudgeName {
				continue
			}

			if len(allowedAggregateSources) == 0 {
				continue
			}

			if _, ok := allowedAggregateSources[sourceName]; ok {
				continue
			}
		}

		return fmt.Errorf("%s %d model-judgment source %q does not match reviewer %q", label, i, sourceName, reportReviewer)
	}

	return nil
}

func validateCommandOutputSources(label string, sources []EvidenceSource, snapshot reviewSnapshot) error {
	for i, source := range sources {
		if source.Type != EvidenceCommandOutput {
			continue
		}

		if hasCommandOutputSourceEvidence(source, snapshot) {
			continue
		}

		return fmt.Errorf("%s %d command-output source was not found in review context", label, i)
	}

	return nil
}

func validateEvidenceSourceContextRanges(label string, sources []EvidenceSource, snapshot reviewSnapshot) error {
	for i, source := range sources {
		if source.Type != EvidenceReviewContext {
			continue
		}

		path, startLine, endLine, ok := parseReviewContextSourceRange(source.Source)
		if !ok {
			return fmt.Errorf("%s %d review-context source must include path:line or path:line-line range", label, i)
		}

		if err := snapshot.validateRange(path, startLine, endLine); err != nil {
			return fmt.Errorf("%s %d review-context source: %w", label, i, err)
		}
	}

	return nil
}

func validEvidenceType(evidenceType EvidenceType) bool {
	switch evidenceType {
	case EvidenceModelJudgment, EvidenceReviewContext, EvidenceCommandOutput:
		return true
	default:
		return false
	}
}

func gateRequiresCommandOutput(name string) bool {
	name = strings.ToLower(strings.TrimSpace(name))
	for _, marker := range []string{"test", "type", "lint", "flake"} {
		if strings.Contains(name, marker) {
			return true
		}
	}

	return false
}

func hasEvidenceType(sources []EvidenceSource, evidenceType EvidenceType) bool {
	for _, source := range sources {
		if source.Type == evidenceType {
			return true
		}
	}

	return false
}

func hasModelJudgmentFrom(reviewer string, sources []EvidenceSource) bool {
	reviewer = strings.TrimSpace(reviewer)
	if reviewer == "" {
		return false
	}

	for _, source := range sources {
		if source.Type == EvidenceModelJudgment && strings.TrimSpace(source.Source) == reviewer {
			return true
		}
	}

	return false
}

func hasHardEvidence(sources []EvidenceSource) bool {
	return hasEvidenceType(sources, EvidenceReviewContext) || hasEvidenceType(sources, EvidenceCommandOutput)
}

func hasReviewContextForFinding(finding Finding) bool {
	for _, source := range finding.Provenance {
		if source.Type == EvidenceReviewContext && evidenceSourceMatchesFinding(source.Source, finding) {
			return true
		}
	}

	return false
}

func hasAggregateFindingSupport(aggregateReviewer string, sources []EvidenceSource, allowedSupporters map[string]struct{}) bool {
	aggregateReviewer = strings.TrimSpace(aggregateReviewer)

	for _, source := range sources {
		if source.Type == EvidenceCommandOutput {
			return true
		}

		if source.Type != EvidenceModelJudgment {
			continue
		}

		sourceName := strings.TrimSpace(source.Source)
		if sourceName == "" || sourceName == aggregateReviewer || sourceName == reviewJudgeName {
			continue
		}

		if len(allowedSupporters) == 0 {
			return true
		}

		if _, ok := allowedSupporters[sourceName]; ok {
			return true
		}
	}

	return false
}

func hasReviewContextGateProof(proof string, sources []EvidenceSource, snapshot reviewSnapshot) bool {
	for _, source := range sources {
		if source.Type != EvidenceReviewContext {
			continue
		}

		path, startLine, endLine, ok := parseReviewContextSourceRange(source.Source)
		if !ok {
			continue
		}

		if snapshot.containsEvidenceInRange(path, startLine, endLine, proof) {
			return true
		}
	}

	return false
}

func hasCommandOutputGateProof(proof string, sources []EvidenceSource) bool {
	proof = normalizeEvidenceText(proof)
	if proof == "" {
		return false
	}

	for _, source := range sources {
		if source.Type != EvidenceCommandOutput {
			continue
		}

		sourceText := normalizeEvidenceText(source.Source)

		summaryText := normalizeEvidenceText(source.Summary)
		if sourceText != "" && summaryText != "" && strings.Contains(proof, sourceText) && strings.Contains(proof, summaryText) {
			return true
		}
	}

	return false
}

func hasCommandOutputSourceEvidence(source EvidenceSource, snapshot reviewSnapshot) bool {
	sourceText := strings.TrimSpace(source.Source)

	summaryText := strings.TrimSpace(source.Summary)
	if sourceText == "" || summaryText == "" {
		return false
	}

	return snapshot.containsCommandEvidence(strings.TrimSpace(sourceText+" "+summaryText)) ||
		(snapshot.containsCommandEvidence(sourceText) && snapshot.containsCommandEvidence(summaryText))
}

func isAggregateReviewer(reviewer string) bool {
	return strings.TrimSpace(reviewer) == string(RoundAggregateVerdict)
}

func evidenceSourceMatchesFinding(source string, finding Finding) bool {
	sourcePath, startLine, endLine, ok := parseReviewContextSourceRange(source)
	if !ok {
		return false
	}

	if sourcePath != cleanReviewPath(finding.Path) {
		return false
	}

	return startLine <= finding.EndLine && endLine >= finding.Line
}

func parseReviewContextSourceRange(source string) (path string, startLine, endLine int, ok bool) {
	source = strings.TrimSpace(strings.ReplaceAll(source, "\\", "/"))
	if source == "" {
		return "", 0, 0, false
	}

	if path, startLine, endLine, ok := parseColonLineRange(source); ok {
		return path, startLine, endLine, true
	}

	return parseHashLineRange(source)
}

func parseColonLineRange(source string) (path string, startLine, endLine int, ok bool) {
	before, after, found := strings.Cut(source, ":")
	if !found {
		return "", 0, 0, false
	}

	startLine, endLine, ok = parseLineRange(after)
	if !ok {
		return "", 0, 0, false
	}

	return cleanReviewPath(before), startLine, endLine, true
}

func parseHashLineRange(source string) (path string, startLine, endLine int, ok bool) {
	before, after, found := strings.Cut(source, "#L")
	if !found {
		return "", 0, 0, false
	}

	after = strings.Replace(after, "-L", "-", 1)

	startLine, endLine, ok = parseLineRange(after)
	if !ok {
		return "", 0, 0, false
	}

	return cleanReviewPath(before), startLine, endLine, true
}

func parseLineRange(value string) (startLine, endLine int, ok bool) {
	left, right, found := strings.Cut(strings.TrimSpace(value), "-")
	if !found {
		right = left
	}

	startLine, err := strconv.Atoi(left)
	if err != nil || startLine <= 0 {
		return 0, 0, false
	}

	endLine, err = strconv.Atoi(right)
	if err != nil || endLine < startLine {
		return 0, 0, false
	}

	return startLine, endLine, true
}

func validConfidence(confidence string) bool {
	switch strings.TrimSpace(confidence) {
	case confidenceHigh, confidenceMedium, confidenceLow:
		return true
	default:
		return false
	}
}

func evidenceFindingKey(finding Finding) string {
	return strings.Join([]string{
		strings.TrimSpace(finding.Path),
		fmt.Sprintf("%d-%d", finding.Line, finding.EndLine),
		string(finding.Category),
		strings.TrimSpace(finding.Message),
	}, "\x00")
}
