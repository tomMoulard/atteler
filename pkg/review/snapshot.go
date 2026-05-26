package review

import (
	"encoding/xml"
	"errors"
	"fmt"
	"html"
	"path"
	"strings"
)

const configuredReferencesCloseTag = "</configured_references>"

type reviewSnapshot struct {
	files                  map[string]reviewSnapshotFile
	commandEvidenceContext string
}

type reviewSnapshotFile struct {
	path  string
	lines []string
}

type configuredReferencesSnapshot struct {
	Files []configuredReferenceFile `xml:"file"`
}

type configuredReferenceFile struct {
	Source  string `xml:"source,attr"`
	Content string `xml:",chardata"`
}

func newReviewSnapshotFromContext(reviewContext string) (reviewSnapshot, error) {
	xmlText, evidenceContext, err := configuredReferencesBlock(reviewContext)
	if err != nil {
		return reviewSnapshot{}, err
	}

	var parsed configuredReferencesSnapshot
	if err := xml.Unmarshal([]byte(xmlText), &parsed); err != nil {
		return reviewSnapshot{}, fmt.Errorf("parse reviewed snapshot: %w", err)
	}

	files := make(map[string]reviewSnapshotFile, len(parsed.Files))
	for _, file := range parsed.Files {
		source := cleanReviewPath(file.Source)
		if source == "" {
			return reviewSnapshot{}, errors.New("reviewed snapshot file source is required")
		}

		if _, exists := files[source]; exists {
			return reviewSnapshot{}, fmt.Errorf("duplicate reviewed snapshot file %q", source)
		}

		content := strings.TrimPrefix(file.Content, "\n")

		files[source] = reviewSnapshotFile{
			path:  source,
			lines: reviewContentLines(content),
		}
	}

	if len(files) == 0 {
		return reviewSnapshot{}, errors.New("review context must include at least one reviewed file")
	}

	return reviewSnapshot{
		commandEvidenceContext: commandEvidenceContext(evidenceContext),
		files:                  files,
	}, nil
}

func configuredReferencesBlock(reviewContext string) (xmlText, evidenceContext string, err error) {
	// buildReviewContext appends the loaded reference block after free-form
	// review instructions. Use the last block so instructions cannot spoof the
	// reviewed snapshot by mentioning configured_references before the real
	// contextref.FormatReferences output.
	start := strings.LastIndex(reviewContext, "<configured_references")
	if start == -1 {
		return "", "", errors.New("review context must include configured file references for evidence validation")
	}

	rest := reviewContext[start:]

	end := strings.Index(rest, configuredReferencesCloseTag)
	if end == -1 {
		return "", "", errors.New("review context configured_references block is not closed")
	}

	end += len(configuredReferencesCloseTag)

	return rest[:end], rest[end:], nil
}

func (snapshot reviewSnapshot) validateRange(filePath string, startLine, endLine int) error {
	filePath = cleanReviewPath(filePath)

	file, ok := snapshot.files[filePath]
	if !ok {
		return fmt.Errorf("finding path %q was not in reviewed snapshot", filePath)
	}

	if startLine <= 0 {
		return fmt.Errorf("finding line must be positive, got %d", startLine)
	}

	if endLine <= 0 {
		return fmt.Errorf("finding end line must be positive, got %d", endLine)
	}

	if endLine < startLine {
		return fmt.Errorf("finding end line %d is before line %d", endLine, startLine)
	}

	if len(file.lines) == 0 {
		return fmt.Errorf("finding path %q has no lines in reviewed snapshot", file.path)
	}

	if endLine > len(file.lines) {
		return fmt.Errorf("finding line range %d-%d exceeds %q line count %d", startLine, endLine, file.path, len(file.lines))
	}

	return nil
}

func (snapshot reviewSnapshot) containsEvidenceInRange(filePath string, startLine, endLine int, evidence string) bool {
	if err := snapshot.validateRange(filePath, startLine, endLine); err != nil {
		return false
	}

	file := snapshot.files[cleanReviewPath(filePath)]
	rangeText := normalizeEvidenceText(strings.Join(file.lines[startLine-1:endLine], "\n"))
	evidence = normalizeEvidenceText(evidence)

	return rangeText != "" && evidence != "" && (strings.Contains(rangeText, evidence) || strings.Contains(evidence, rangeText))
}

func (snapshot reviewSnapshot) containsCommandEvidence(value string) bool {
	value = normalizeEvidenceText(value)
	if value == "" {
		return false
	}

	return strings.Contains(normalizeEvidenceText(snapshot.commandEvidenceContext), value)
}

func cleanReviewPath(value string) string {
	value = strings.TrimSpace(strings.ReplaceAll(value, "\\", "/"))
	if value == "" {
		return ""
	}

	cleaned := path.Clean(value)
	if cleaned == "." {
		return value
	}

	return strings.TrimPrefix(cleaned, "./")
}

func reviewContentLines(content string) []string {
	if content == "" {
		return nil
	}

	return strings.Split(strings.TrimSuffix(content, "\n"), "\n")
}

func normalizeEvidenceText(value string) string {
	value = strings.ReplaceAll(value, "`", "")
	value = html.UnescapeString(value)

	return strings.ToLower(strings.Join(strings.Fields(value), " "))
}

func commandEvidenceContext(evidenceContext string) string {
	var b strings.Builder

	lines := strings.Split(evidenceContext, "\n")

	inCommandSection := false

	for _, line := range lines {
		if isCommandOutputHeading(line) {
			inCommandSection = true
		}

		if !inCommandSection {
			continue
		}

		b.WriteString(line)
		b.WriteByte('\n')
	}

	return b.String()
}

func isCommandOutputHeading(line string) bool {
	heading := strings.TrimSpace(line)
	heading = strings.TrimSpace(strings.TrimLeft(heading, "#"))
	heading = strings.TrimSuffix(strings.ToLower(heading), ":")

	switch heading {
	case "command output", "command outputs":
		return true
	default:
		return false
	}
}
