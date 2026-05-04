package watch

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
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

func TestScanWithOptions_ReportsContextBackgroundConventionDrift(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	writeFile(t, root, "pkg/service/service.go", `package service

import "context"

func run() {
	_ = context.Background()
}
`)
	writeFile(t, root, "pkg/service/service_test.go", `package service

import "context"

func TestRun() {
	_ = context.Background()
}
`)
	writeFile(t, root, "cmd/tool/main.go", `package main

import "context"

func main() {
	_ = context.Background()
}
`)

	findings, err := ScanWithOptions(root, Options{LargeFileBytes: 1024})
	require.NoError(t, err)

	keys := findingKeys(findings)
	assert.Contains(t, keys, "pkg/service/service.go|convention_drift|maintenance")
	assert.NotContains(t, keys, "pkg/service/service_test.go|convention_drift|maintenance")
	assert.NotContains(t, keys, "cmd/tool/main.go|convention_drift|maintenance")
}

func TestScanWithOptions_ContextBackgroundDriftIgnoresCommentsAndStrings(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	writeFile(t, root, "pkg/service/service.go", `package service

const example = "context.Background()"

// context.Background() is mentioned in docs only.
func run() {}
`)

	findings, err := ScanWithOptions(root, Options{LargeFileBytes: 1024})
	require.NoError(t, err)

	assert.NotContains(t, findingKeys(findings), "pkg/service/service.go|convention_drift|maintenance")
}

func TestScanWithOptions_ReportsAliasedContextBackgroundConventionDrift(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	writeFile(t, root, "pkg/service/alias.go", `package service

import ctxpkg "context"

func run() {
	_ = ctxpkg.Background()
}
`)
	writeFile(t, root, "pkg/service/dot.go", `package service

import . "context"

func other() {
	_ = Background()
}
`)

	findings, err := ScanWithOptions(root, Options{LargeFileBytes: 1024})
	require.NoError(t, err)

	keys := findingKeys(findings)
	assert.Contains(t, keys, "pkg/service/alias.go|convention_drift|maintenance")
	assert.Contains(t, keys, "pkg/service/dot.go|convention_drift|maintenance")
}

func TestScanWithOptions_SkipsRuntimeArtifactDirectoriesAndBinaryStaleMarkers(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	writeFile(t, root, ".atteler/session.json", "TODO: runtime session\n")
	writeFile(t, root, ".omx/state.json", "TODO: runtime state\n")
	writeFile(t, root, "dist/generated.txt", "TODO: generated artifact\n")
	writeFile(t, root, "binary.bin", string([]byte{0xff, 0xfe, 'T', 'O', 'D', 'O'}))
	writeFile(t, root, "docs/todo.md", "TODO: real note\n")

	findings, err := ScanWithOptions(root, Options{LargeFileBytes: 1024})
	require.NoError(t, err)

	keys := findingKeys(findings)
	assert.Contains(t, keys, "docs/todo.md|stale_todo|maintenance")
	assert.NotContains(t, keys, ".atteler/session.json|stale_todo|maintenance")
	assert.NotContains(t, keys, ".omx/state.json|stale_todo|maintenance")
	assert.NotContains(t, keys, "dist/generated.txt|stale_todo|maintenance")
	assert.NotContains(t, keys, "binary.bin|stale_todo|maintenance")
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
