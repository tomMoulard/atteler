package watch

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

func TestScanWithOptions_FindsRepositoryHealthIssues(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	writeFile(t, root, "pkg/covered/covered.go", "package covered\n")
	writeFile(t, root, "pkg/covered/covered_test.go", "package covered\n")
	writeFile(t, root, "pkg/missing/missing.go", "package missing\n")
	writeFile(t, root, "docs/notes.md", "FIXME: remove stale note\n")
	writeFile(t, root, "assets/blob.txt", "12345678901234567890123456789012345678901234567890123456789012345")
	writeFile(t, root, ".git/ignored.go", "package ignored\n")
	writeFile(t, root, "vendor/ignored/ignored.go", "package ignored\n")

	findings, err := ScanWithOptions(root, Options{LargeFileBytes: 64})
	if err != nil {
		t.Fatalf("ScanWithOptions() error = %v", err)
	}

	got := findingKeys(findings)
	want := []string{
		"assets/blob.txt|large_file|warning",
		"docs/notes.md|stale_todo|maintenance",
		"pkg/missing/missing.go|missing_test|info",
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("findings = %#v, want %#v\nfull findings: %#v", got, want, findings)
	}
}

func TestScan_UsesDefaultOptions(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	writeFile(t, root, "todo.txt", "TODO: stale work item\n")

	findings, err := Scan(root)
	if err != nil {
		t.Fatalf("Scan() error = %v", err)
	}
	if len(findings) != 1 {
		t.Fatalf("len(findings) = %d, want 1: %#v", len(findings), findings)
	}
	if findings[0].Path != "todo.txt" || findings[0].Kind != "stale_todo" {
		t.Fatalf("finding = %#v, want stale_todo for todo.txt", findings[0])
	}
}

func TestScanWithOptions_SortsFindingsDeterministically(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	writeFile(t, root, "z/z.go", "package z\n")
	writeFile(t, root, "a/a.go", "package a\n")
	writeFile(t, root, "a/a.txt", "TODO: stale work item\n")

	findings, err := ScanWithOptions(root, Options{LargeFileBytes: 100})
	if err != nil {
		t.Fatalf("ScanWithOptions() error = %v", err)
	}

	got := findingKeys(findings)
	want := []string{
		"a/a.go|missing_test|info",
		"a/a.txt|stale_todo|maintenance",
		"z/z.go|missing_test|info",
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("findings = %#v, want %#v", got, want)
	}
}

func TestScanWithOptions_ReportsStaleMarkers(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	writeFile(t, root, "todo-first.txt", "TODO: one\nFIXME: two\n")
	writeFile(t, root, "fixme.txt", "fixme: one\n")

	findings, err := ScanWithOptions(root, Options{LargeFileBytes: 100})
	if err != nil {
		t.Fatalf("ScanWithOptions() error = %v", err)
	}

	got := findingMessages(findings)
	want := []string{
		"contains stale TODO/FIXME marker",
		"contains stale TODO/FIXME marker",
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("messages = %#v, want %#v", got, want)
	}
}

func TestScanWithOptions_ReportsLargeFilesWithStaleMarkers(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	writeFile(t, root, "large-todo.txt", "TODO: stale work item with enough bytes")

	findings, err := ScanWithOptions(root, Options{LargeFileBytes: 10})
	if err != nil {
		t.Fatalf("ScanWithOptions() error = %v", err)
	}

	got := findingKeys(findings)
	want := []string{
		"large-todo.txt|large_file|warning",
		"large-todo.txt|stale_todo|maintenance",
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("findings = %#v, want %#v", got, want)
	}
}

func findingKeys(findings []Finding) []string {
	keys := make([]string, 0, len(findings))
	for _, finding := range findings {
		keys = append(keys, finding.Path+"|"+finding.Kind+"|"+finding.Severity)
	}
	return keys
}

func findingMessages(findings []Finding) []string {
	messages := make([]string, 0, len(findings))
	for _, finding := range findings {
		messages = append(messages, finding.Message)
	}
	return messages
}

func writeFile(t *testing.T, root, path, content string) {
	t.Helper()

	fullPath := filepath.Join(root, path)
	if err := os.MkdirAll(filepath.Dir(fullPath), 0o750); err != nil {
		t.Fatalf("MkdirAll(%q) error = %v", filepath.Dir(fullPath), err)
	}
	if err := os.WriteFile(fullPath, []byte(content), 0o600); err != nil {
		t.Fatalf("WriteFile(%q) error = %v", fullPath, err)
	}
}
