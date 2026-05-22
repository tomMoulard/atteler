package watch

import (
	"context"
	"errors"
	"fmt"
	"strings"
)

const (
	// IssueActionCreated means UpsertIssues created a new tracker issue.
	IssueActionCreated = "created"
	// IssueActionUpdated means UpsertIssues updated an existing tracker issue.
	IssueActionUpdated = "updated"

	defaultIssueTitlePrefix = "Watch finding"
)

// IssueFingerprintMarker returns the stable hidden marker used to deduplicate
// external tracker issues for a watch finding.
func IssueFingerprintMarker(fingerprint string) string {
	return fmt.Sprintf("<!-- atteler-watch:fingerprint=%s -->", strings.TrimSpace(fingerprint))
}

// IssueTracker is the minimal GitHub-compatible issue surface watch needs to
// avoid duplicate issues for the same stable finding fingerprint.
type IssueTracker interface {
	FindIssueByFingerprint(context.Context, string) (*IssueRef, error)
	CreateIssue(context.Context, IssueDraft) (IssueRef, error)
	UpdateIssue(context.Context, IssueRef, IssueDraft) (IssueRef, error)
}

// IssueOptions controls which comparison findings become issue upserts.
type IssueOptions struct {
	TitlePrefix string   `json:"title_prefix,omitempty"`
	MinSeverity string   `json:"min_severity,omitempty"`
	Labels      []string `json:"labels,omitempty"`
}

// IssueDraft is a deterministic issue payload for one watch finding.
type IssueDraft struct {
	Title       string   `json:"title"`
	Body        string   `json:"body"`
	Fingerprint string   `json:"fingerprint"`
	Labels      []string `json:"labels,omitempty"`
	Finding     Finding  `json:"finding"`
}

// IssueRef identifies an issue in an external tracker.
type IssueRef struct {
	ID          string `json:"id,omitempty"`
	URL         string `json:"url,omitempty"`
	Fingerprint string `json:"fingerprint,omitempty"`
	State       string `json:"state,omitempty"`
	Number      int    `json:"number,omitempty"`
}

// IssueUpsertResult records the create/update action for one finding.
type IssueUpsertResult struct {
	Action  string   `json:"action"`
	Issue   IssueRef `json:"issue"`
	Finding Finding  `json:"finding"`
}

// UpsertIssues creates or updates issues for new, unsuppressed findings that
// meet the severity threshold. It first looks up the stable fingerprint, so
// repeated scans update the same issue instead of creating duplicates.
func UpsertIssues(ctx context.Context, tracker IssueTracker, comparison Comparison, options IssueOptions) ([]IssueUpsertResult, error) {
	if ctx == nil {
		return nil, errors.New("watch issue upsert: nil context")
	}

	if tracker == nil {
		return nil, errors.New("watch issue upsert: tracker is required")
	}

	minSeverity := strings.TrimSpace(options.MinSeverity)
	if minSeverity == "" {
		minSeverity = SeverityHigh
	}

	if !validSeverity(minSeverity) {
		return nil, fmt.Errorf("watch issue upsert: invalid min severity %q", minSeverity)
	}

	unstable := unstableFindingFingerprints(comparison)
	processed := make(map[string]struct{}, len(comparison.NewFindings))

	var results []IssueUpsertResult

	for i := range comparison.NewFindings {
		finding := comparison.NewFindings[i]
		finding = completeFindingIdentity(finding)

		if finding.Suppressed || unstable[finding.Fingerprint] || !severityAtLeast(finding.Severity, minSeverity) {
			continue
		}

		if _, ok := processed[finding.Fingerprint]; ok {
			continue
		}

		processed[finding.Fingerprint] = struct{}{}

		existing, err := tracker.FindIssueByFingerprint(ctx, finding.Fingerprint)
		if err != nil {
			return results, fmt.Errorf("watch issue upsert %s: find issue: %w", finding.ID, err)
		}

		draft := issueDraft(finding, options)
		if existing == nil {
			created, createErr := tracker.CreateIssue(ctx, draft)
			if createErr != nil {
				return results, fmt.Errorf("watch issue upsert %s: create issue: %w", finding.ID, createErr)
			}

			results = append(results, IssueUpsertResult{
				Action:  IssueActionCreated,
				Issue:   created,
				Finding: finding,
			})

			continue
		}

		updated, err := tracker.UpdateIssue(ctx, *existing, draft)
		if err != nil {
			return results, fmt.Errorf("watch issue upsert %s: update issue: %w", finding.ID, err)
		}

		results = append(results, IssueUpsertResult{
			Action:  IssueActionUpdated,
			Issue:   updated,
			Finding: finding,
		})
	}

	return results, nil
}

func issueDraft(finding Finding, options IssueOptions) IssueDraft {
	finding = completeFindingIdentity(finding)

	prefix := strings.TrimSpace(options.TitlePrefix)
	if prefix == "" {
		prefix = defaultIssueTitlePrefix
	}

	title := fmt.Sprintf("%s: %s in %s", prefix, finding.Kind, finding.Path)
	body := strings.Join([]string{
		IssueFingerprintMarker(finding.Fingerprint),
		"",
		"Atteler watch found a new actionable quality finding.",
		"",
		"- Finding ID: `" + finding.ID + "`",
		"- Fingerprint: `" + finding.Fingerprint + "`",
		"- Rule: `" + finding.RuleID + "`",
		"- Rule description: " + firstNonEmptyString(finding.RuleDescription, "not provided"),
		"- Severity: `" + finding.Severity + "`",
		"- Path: `" + finding.Path + "`",
		"- Message: " + finding.Message,
		issueOwnerLine(finding),
		"",
		"Suggested remediation: " + issueHelp(finding),
	}, "\n")

	return IssueDraft{
		Title:       title,
		Body:        body,
		Fingerprint: finding.Fingerprint,
		Labels:      append([]string(nil), options.Labels...),
		Finding:     finding,
	}
}

func firstNonEmptyString(values ...string) string {
	for _, value := range values {
		value = strings.TrimSpace(value)
		if value != "" {
			return value
		}
	}

	return ""
}

func issueOwnerLine(finding Finding) string {
	if strings.TrimSpace(finding.Owner) == "" {
		return "- Owner: unassigned"
	}

	return "- Owner: " + strings.TrimSpace(finding.Owner)
}

func issueHelp(finding Finding) string {
	return firstNonEmptyString(
		finding.Help,
		"Review the finding evidence and update the code, rule configuration, or suppression with an explicit reason.",
	)
}
