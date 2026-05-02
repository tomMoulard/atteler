package skill

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// PersistSuggestion writes an accepted skill suggestion as a reusable markdown
// artifact under dir. Existing files are left untouched so acceptance remains a
// safe, explicit action.
func PersistSuggestion(dir string, suggestion Suggestion) (string, error) {
	dir = strings.TrimSpace(dir)
	if dir == "" {
		return "", errors.New("skill: save directory is required")
	}

	slug := strings.TrimSpace(suggestion.Slug)
	if slug == "" {
		return "", errors.New("skill: suggestion slug is required")
	}
	if filepath.Base(slug) != slug || strings.Contains(slug, string(filepath.Separator)) {
		return "", fmt.Errorf("skill: invalid suggestion slug %q", slug)
	}

	if len(suggestion.Steps) == 0 {
		return "", errors.New("skill: suggestion steps are required")
	}

	if err := os.MkdirAll(dir, 0o750); err != nil {
		return "", fmt.Errorf("skill: create save directory: %w", err)
	}

	path := filepath.Join(dir, slug+".md")
	file, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return "", fmt.Errorf("skill: save suggestion %s: %w", path, err)
	}
	defer file.Close()

	if _, err := file.WriteString(formatPersistedSuggestion(suggestion)); err != nil {
		return "", fmt.Errorf("skill: write suggestion %s: %w", path, err)
	}
	return path, nil
}

func formatPersistedSuggestion(suggestion Suggestion) string {
	var b strings.Builder
	fmt.Fprintf(&b, "# %s\n\n", suggestion.Name)
	fmt.Fprintf(&b, "slug: %s\n", suggestion.Slug)
	fmt.Fprintf(&b, "occurrences: %d\n\n", suggestion.Occurrences)
	b.WriteString("## Steps\n\n")
	for _, step := range suggestion.Steps {
		fmt.Fprintf(&b, "- %s\n", step)
	}
	if strings.TrimSpace(suggestion.Rationale) != "" {
		fmt.Fprintf(&b, "\n## Rationale\n\n%s\n", suggestion.Rationale)
	}
	return b.String()
}
