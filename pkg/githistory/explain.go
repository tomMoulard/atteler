//nolint:wsl_v5 // Small explanation helpers keep closely related branches together.
package githistory

import (
	"fmt"
	"sort"
	"strings"
)

func changedFilesMetadata(changes []ChangedFile) string {
	if len(changes) == 0 {
		return ""
	}

	lines := make([]string, 0, len(changes))
	for _, change := range changes {
		parts := []string{change.Path}
		if change.Status != "" {
			parts = append(parts, "status="+change.Status)
		}
		if change.Renamed {
			parts = append(parts, "renamed=true")
		}
		if change.OldPath != "" {
			parts = append(parts, "old_path="+change.OldPath)
		}
		if change.Binary {
			parts = append(parts, "binary=true")
		} else {
			parts = append(parts, fmt.Sprintf("+%d", change.Added), fmt.Sprintf("-%d", change.Deleted))
		}

		lines = append(lines, strings.Join(parts, " "))
	}

	return strings.Join(lines, "\n")
}

func relationsText(relations CommitRelations) string {
	var parts []string
	if relations.Fixup {
		parts = append(parts, "fixup")
	}
	if relations.Squash {
		parts = append(parts, "squash")
	}
	if len(relations.Reverts) > 0 {
		parts = append(parts, "reverts "+strings.Join(relations.Reverts, " "))
	}
	if len(relations.IssueRefs) > 0 {
		parts = append(parts, "issues "+strings.Join(relations.IssueRefs, " "))
	}
	if len(relations.PRRefs) > 0 {
		parts = append(parts, "prs "+strings.Join(relations.PRRefs, " "))
	}

	return strings.Join(parts, "\n")
}

func matchedFields(matches []MatchEvidence) string {
	seen := make(map[string]struct{})
	fields := make([]string, 0)
	for _, match := range matches {
		if match.Field == "" {
			continue
		}
		if _, ok := seen[match.Field]; ok {
			continue
		}
		seen[match.Field] = struct{}{}
		fields = append(fields, match.Field)
	}
	sort.Strings(fields)

	return strings.Join(fields, ",")
}

func rangeContext(commit Commit) string {
	var parts []string
	if len(commit.Refs) > 0 {
		parts = append(parts, "refs="+strings.Join(commit.Refs, ","))
	}
	if !commit.Date.IsZero() {
		parts = append(parts, "date="+commit.Date.Format("2006-01-02"))
	}
	if len(commit.Files) > 0 {
		parts = append(parts, fmt.Sprintf("files=%d", len(commit.Files)))
	}
	if len(commit.Changes) > 0 {
		parts = append(parts, fmt.Sprintf("changes=%d", len(commit.Changes)))
	}

	return strings.Join(parts, " ")
}

func confidenceForScore(score int, matches []MatchEvidence) float64 {
	if score <= 0 {
		return 0
	}

	fields := strings.Count(matchedFields(matches), ",") + 1
	if len(matches) == 0 {
		fields = 0
	}

	confidence := 0.35 + float64(score)/200.0 + float64(fields)*0.08
	if confidence > 0.95 {
		return 0.95
	}

	return confidence
}

func rankingExplanation(result Result) []string {
	explanations := []string{
		"ranked by weighted matches across commit metadata, changed files, inferred relations, and optional diff hunks",
	}
	if fields := matchedFields(result.Matches); fields != "" {
		explanations = append(explanations, "matched fields: "+fields)
	}
	if result.RangeContext != "" {
		explanations = append(explanations, "range context: "+result.RangeContext)
	}
	if result.Confidence > 0 {
		explanations = append(explanations, fmt.Sprintf("confidence: %.2f", result.Confidence))
	}

	return explanations
}
