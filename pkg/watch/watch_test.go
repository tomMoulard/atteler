package watch

import (
	"os"
	"path/filepath"
	"reflect"
	"runtime"
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

func TestScanWithOptions_EntrypointsAllowOnlyOneBackgroundRoot(t *testing.T) {
	t.Parallel()

	t.Run("allows one background root", func(t *testing.T) {
		t.Parallel()

		root := t.TempDir()
		writeFile(t, root, "cmd/atteler/main.go", `package main

import "context"

func rootContext() context.Context {
	return context.Background()
}
`)

		findings, err := ScanWithOptions(root, Options{LargeFileBytes: 1024})
		require.NoError(t, err)
		assert.NotContains(t, findingKeys(findings), "cmd/atteler/main.go|convention_drift|maintenance")
	})

	t.Run("flags extra background roots", func(t *testing.T) {
		t.Parallel()

		root := t.TempDir()
		writeFile(t, root, "cmd/atteler/main.go", `package main

import "context"

func rootContext() context.Context {
	_ = context.Background()
	return context.Background()
}
`)

		findings, err := ScanWithOptions(root, Options{LargeFileBytes: 1024})
		require.NoError(t, err)
		assert.Contains(t, findingKeys(findings), "cmd/atteler/main.go|convention_drift|maintenance")
	})

	t.Run("flags TODO roots", func(t *testing.T) {
		t.Parallel()

		root := t.TempDir()
		writeFile(t, root, "cmd/atteler/main.go", `package main

import "context"

func rootContext() context.Context {
	return context.TODO()
}
`)

		findings, err := ScanWithOptions(root, Options{LargeFileBytes: 1024})
		require.NoError(t, err)
		assert.Contains(t, findingKeys(findings), "cmd/atteler/main.go|convention_drift|maintenance")
	})

	t.Run("flags context detachment", func(t *testing.T) {
		t.Parallel()

		root := t.TempDir()
		writeFile(t, root, "cmd/atteler/main.go", `package main

import "context"

func detached(ctx context.Context) context.Context {
	return context.WithoutCancel(ctx)
}
`)

		findings, err := ScanWithOptions(root, Options{LargeFileBytes: 1024})
		require.NoError(t, err)
		assert.Contains(t, findingKeys(findings), "cmd/atteler/main.go|convention_drift|maintenance")
	})
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
	assert.Contains(t, findings[0].RuleDescription, "TODO/FIXME")
	assert.Contains(t, findings[0].Help, "tracked issues")
	assert.NotEmpty(t, findings[0].ID)
	assert.NotEmpty(t, findings[0].Fingerprint)
	assert.Contains(t, findings[0].ID, findings[0].Fingerprint)
}

func TestScanWithOptions_StableFingerprintIgnoresVolatileLargeFileMessage(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	writeFile(t, root, "blob.txt", strings.Repeat("a", 32))

	first, err := ScanWithOptions(root, Options{LargeFileBytes: 10})
	require.NoError(t, err)
	require.Len(t, first, 1)

	writeFile(t, root, "blob.txt", strings.Repeat("a", 48))
	second, err := ScanWithOptions(root, Options{LargeFileBytes: 10})
	require.NoError(t, err)
	require.Len(t, second, 1)

	assert.NotEqual(t, first[0].Message, second[0].Message)
	assert.Equal(t, first[0].Fingerprint, second[0].Fingerprint)
	assert.Equal(t, first[0].ID, second[0].ID)
}

func TestScanWithOptions_SuppressesFindingsWithReason(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	writeFile(t, root, "todo.txt", "TODO: tracked elsewhere\n")

	baseline, err := ScanWithOptions(root, Options{LargeFileBytes: 1024})
	require.NoError(t, err)
	require.Len(t, baseline, 1)

	findings, err := ScanWithOptions(root, Options{
		LargeFileBytes: 1024,
		Suppressions: []Suppression{{
			ID:     baseline[0].ID,
			Reason: "tracked in GH-123",
		}},
	})
	require.NoError(t, err)
	require.Len(t, findings, 1)

	assert.True(t, findings[0].Suppressed)
	assert.Equal(t, "tracked in GH-123", findings[0].SuppressionReason)
}

func TestScanWithOptions_SuppressesFindingsByFingerprintOrRulePath(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	writeFile(t, root, "fingerprint.txt", "TODO: tracked by fingerprint\n")
	writeFile(t, root, "scoped.txt", "TODO: tracked by rule and path\n")

	baseline, err := ScanWithOptions(root, Options{LargeFileBytes: 1024})
	require.NoError(t, err)
	require.Len(t, baseline, 2)

	findings, err := ScanWithOptions(root, Options{
		LargeFileBytes: 1024,
		Suppressions: []Suppression{
			{
				Fingerprint: findingByPath(t, baseline, "fingerprint.txt").Fingerprint,
				Reason:      "tracked by stable fingerprint",
			},
			{
				RuleID: "watch." + KindStaleTODO,
				Path:   "./scoped.txt",
				Reason: "tracked by hand-authored suppression",
			},
		},
	})
	require.NoError(t, err)
	require.Len(t, findings, 2)

	fingerprintFinding := findingByPath(t, findings, "fingerprint.txt")
	assert.True(t, fingerprintFinding.Suppressed)
	assert.Equal(t, "tracked by stable fingerprint", fingerprintFinding.SuppressionReason)

	scopedFinding := findingByPath(t, findings, "scoped.txt")
	assert.True(t, scopedFinding.Suppressed)
	assert.Equal(t, "tracked by hand-authored suppression", scopedFinding.SuppressionReason)
}

func TestScanWithOptions_BaselineComparisonUsesSyntheticRepoFindings(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	writeFile(t, root, "existing.txt", "TODO: keep tracked debt\n")
	writeFile(t, root, "fixed.txt", "TODO: debt removed on branch\n")

	baseline, err := ScanWithOptions(root, Options{LargeFileBytes: 1024})
	require.NoError(t, err)
	require.Len(t, baseline, 2)

	require.NoError(t, os.Remove(filepath.Join(root, "fixed.txt")))
	writeFile(t, root, "new.txt", "TODO: new branch regression\n")

	current, err := ScanWithOptions(root, Options{LargeFileBytes: 1024})
	require.NoError(t, err)

	comparison := CompareFindings(baseline, current)
	assert.Equal(t, TrendMetrics{New: 1, Fixed: 1, Unchanged: 1, Suppressed: 0, Unstable: 0}, comparison.Metrics)
	assert.Equal(t, []string{"new.txt"}, findingPaths(comparison.NewFindings))
	assert.Equal(t, []string{"fixed.txt"}, findingPaths(comparison.FixedFindings))
	assert.Equal(t, []string{"existing.txt"}, findingPaths(comparison.UnchangedFindings))

	gate := EvaluateGate(comparison, GateOptions{MinSeverity: SeverityMaintenance})
	require.False(t, gate.Passed)
	require.Len(t, gate.BlockingFindings, 1)
	assert.Equal(t, "new.txt", gate.BlockingFindings[0].Path)
}

func TestScanWithOptions_RejectsSuppressionsWithoutReasons(t *testing.T) {
	t.Parallel()

	_, err := ScanWithOptions(t.TempDir(), Options{
		Suppressions: []Suppression{{Fingerprint: "abc123"}},
	})

	require.Error(t, err)
	assert.Contains(t, err.Error(), "reason is required")
}

func TestScanWithOptions_RejectsInvalidRuleConfigs(t *testing.T) {
	t.Parallel()

	assertInvalidRuleConfig := func(name string, config RuleConfig, wantErr string) {
		t.Helper()

		t.Run(name, func(t *testing.T) {
			t.Parallel()

			_, err := ScanWithOptions(t.TempDir(), Options{
				Rules: []RuleConfig{config},
			})

			require.Error(t, err)
			assert.Contains(t, err.Error(), wantErr)
		})
	}

	assertInvalidRuleConfig("missing rule id", RuleConfig{}, "rule_id is required")
	assertInvalidRuleConfig("unknown rule id", RuleConfig{RuleID: "watch.unknown"}, `unknown rule_id "watch.unknown"`)
	assertInvalidRuleConfig(
		"invalid severity",
		RuleConfig{RuleID: "watch." + KindLargeFile, Severity: "urgent"},
		`invalid severity "urgent"`,
	)
}

func TestScanWithOptions_ConfiguresRulesAndIgnores(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	writeFile(t, root, "todo.txt", "TODO: disabled rule\n")
	writeFile(t, root, "ignored.txt", "TODO: ignored by option\n")
	writeFile(t, root, "blob.txt", strings.Repeat("x", 32))

	findings, err := ScanWithOptions(root, Options{
		LargeFileBytes: 24,
		IgnorePaths:    []string{"ignored.txt"},
		Rules: []RuleConfig{
			{RuleID: "watch." + KindStaleTODO, Disabled: true},
			{
				RuleID:   "watch." + KindLargeFile,
				Severity: SeverityHigh,
				Help:     "custom large file remediation",
				Owner:    "platform-quality",
			},
		},
	})
	require.NoError(t, err)
	require.Len(t, findings, 1)

	assert.Equal(t, KindLargeFile, findings[0].Kind)
	assert.Equal(t, SeverityHigh, findings[0].Severity)
	assert.Equal(t, "custom large file remediation", findings[0].Help)
	assert.Equal(t, "platform-quality", findings[0].Owner)
}

func TestScanWithOptions_SkipsSymlinksAndGeneratedFolders(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	outside := t.TempDir()
	writeFile(t, root, "generated/generated.go", "package generated\n")
	writeFile(t, outside, "external.txt", "TODO: outside target\n")
	writeFile(t, root, "visible.txt", "TODO: visible\n")

	if runtime.GOOS == "windows" {
		t.Skip("symlink creation requires extra privileges on Windows")
	}

	err := os.Symlink(filepath.Join(outside, "external.txt"), filepath.Join(root, "linked.txt"))
	require.NoError(t, err)

	findings, err := ScanWithOptions(root, Options{LargeFileBytes: 1024})
	require.NoError(t, err)

	keys := findingKeys(findings)
	assert.Contains(t, keys, "visible.txt|stale_todo|maintenance")
	assert.NotContains(t, keys, "linked.txt|stale_todo|maintenance")
	assert.NotContains(t, keys, "generated/generated.go|missing_test|info")
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

	for i := range findings {
		finding := findings[i]
		keys = append(keys, finding.Path+"|"+finding.Kind+"|"+finding.Severity)
	}

	return keys
}

func findingMessages(findings []Finding) []string {
	messages := make([]string, 0, len(findings))

	for i := range findings {
		finding := findings[i]
		messages = append(messages, finding.Message)
	}

	return messages
}

func findingByPath(t *testing.T, findings []Finding, path string) Finding {
	t.Helper()

	for i := range findings {
		if findings[i].Path == path {
			return findings[i]
		}
	}

	require.FailNowf(t, "missing finding", "path %q not found in %#v", path, findings)

	return Finding{}
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
