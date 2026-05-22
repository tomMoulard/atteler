package watch

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
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
	writeFile(t, root, "pkg/service/todo.go", `package service

import "context"

func runLater() {
	_ = context.TODO()
}
`)
	writeFile(t, root, "pkg/service/without_cancel.go", `package service

import "context"

func bypassCaller(ctx context.Context) context.Context {
	return context.WithoutCancel(ctx)
}
`)
	writeFile(t, root, "pkg/service/factory.go", `package service

import "context"

var defaultContextFactory = context.Background
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
	writeFile(t, root, "cmd/atteler/main.go", `package main

import "context"

func main() {
	_ = context.Background()
}
`)

	findings, err := ScanWithOptions(root, Options{LargeFileBytes: 1024})
	require.NoError(t, err)

	keys := findingKeys(findings)
	assert.Contains(t, keys, "pkg/service/service.go|convention_drift|maintenance")
	assert.Contains(t, keys, "pkg/service/todo.go|convention_drift|maintenance")
	assert.Contains(t, keys, "pkg/service/without_cancel.go|convention_drift|maintenance")
	assert.Contains(t, keys, "pkg/service/factory.go|convention_drift|maintenance")
	assert.Contains(t, keys, "cmd/tool/main.go|convention_drift|maintenance")
	assert.NotContains(t, keys, "pkg/service/service_test.go|convention_drift|maintenance")
	assert.NotContains(t, keys, "cmd/atteler/main.go|convention_drift|maintenance")
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
	writeFile(t, root, "pkg/service/alias_without_cancel.go", `package service

import ctxpkg "context"

func run(ctx context.Context) context.Context {
	return ctxpkg.WithoutCancel(ctx)
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
	assert.Contains(t, keys, "pkg/service/alias_without_cancel.go|convention_drift|maintenance")
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

func TestScanWithOptions_RespectsRootGitIgnore(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	writeFile(t, root, ".gitignore", strings.Join([]string{
		"/atteler",
		"ignored.log",
		"ignored-dir/",
		"*.tmp",
		"!keep.tmp",
	}, "\n"))
	writeFile(t, root, "atteler", strings.Repeat("x", 128))
	writeFile(t, root, "ignored.log", "TODO: ignored log\n")
	writeFile(t, root, "ignored-dir/ignored.go", "package ignored\n")
	writeFile(t, root, "src/generated.tmp", "TODO: ignored generated tmp\n")
	writeFile(t, root, "keep.tmp", "TODO: keep negated file\n")
	writeFile(t, root, "visible.txt", "TODO: visible marker\n")

	findings, err := ScanWithOptions(root, Options{LargeFileBytes: 64})
	require.NoError(t, err)

	keys := findingKeys(findings)
	assert.Contains(t, keys, "keep.tmp|stale_todo|maintenance")
	assert.Contains(t, keys, "visible.txt|stale_todo|maintenance")
	assert.NotContains(t, keys, "atteler|large_file|warning")
	assert.NotContains(t, keys, "ignored.log|stale_todo|maintenance")
	assert.NotContains(t, keys, "ignored-dir/ignored.go|missing_test|info")
	assert.NotContains(t, keys, "src/generated.tmp|stale_todo|maintenance")
}

func TestScanWithOptions_IgnoresRoadmapHeadingsAndTestFixtures(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	writeFile(t, root, "README.md", "# Project\n\n## TODO\n\n- [x] persistent task/TODO list\n")
	writeFile(t, root, "TODO.md", "# atteler TODO\n\n## P4 -- Feature gaps\n\n- [x] completed work\n")
	writeFile(t, root, "pkg/watch/watch_test.go", `package watch

func TestFixture() {
	_ = "TODO: fixture text"
}
`)
	writeFile(t, root, "docs/action.md", "TODO: convert this real marker into an issue\n")

	findings, err := ScanWithOptions(root, Options{LargeFileBytes: 1024})
	require.NoError(t, err)

	keys := findingKeys(findings)
	assert.Contains(t, keys, "docs/action.md|stale_todo|maintenance")
	assert.NotContains(t, keys, "README.md|stale_todo|maintenance")
	assert.NotContains(t, keys, "TODO.md|stale_todo|maintenance")
	assert.NotContains(t, keys, "pkg/watch/watch_test.go|stale_todo|maintenance")
}

func TestScanWithOptions_AddsRuleMetadata(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	writeFile(t, root, "todo.txt", "TODO: real work\n")

	findings, err := ScanWithOptions(root, Options{LargeFileBytes: 1024})
	require.NoError(t, err)
	require.Len(t, findings, 1)

	assert.Equal(t, "watch.stale_todo", findings[0].RuleID)
	assert.Contains(t, findings[0].Help, "tracked issues")
}

func TestRepositoryContextRootsStayAtEntrypoints(t *testing.T) {
	t.Parallel()

	root, err := filepath.Abs(filepath.Join("..", ".."))
	require.NoError(t, err)

	findings, err := ScanWithOptions(root, Options{LargeFileBytes: 1 << 30})
	require.NoError(t, err)

	var drift []Finding

	for _, finding := range findings {
		if finding.Kind == KindConventionDrift {
			drift = append(drift, finding)
		}
	}

	require.Empty(t, drift, "production context roots must stay in process entrypoints")
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
