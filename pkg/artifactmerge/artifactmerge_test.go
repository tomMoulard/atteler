package artifactmerge

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/tommoulard/atteler/pkg/session"
)

func TestMerge_IncludesTextArtifactsDeterministically(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	writeFile(t, root, "b.md", "second")
	writeFile(t, root, "a.txt", "first with ``` fence\n")

	result, err := Merge(root, []session.Artifact{
		{Path: "b.md", Kind: "patch", Summary: "second summary", SourceAgent: "agent-b"},
		{Path: "./nested/../a.txt", Kind: "note", Summary: "first\nsummary", SourceAgent: "agent-a"},
	}, 1024)
	require.NoError(t, err)
	require.Empty(t, result.Warnings)
	require.Len(t, result.Entries, 2)

	assert.Equal(t, []string{"a.txt", "b.md"}, []string{result.Entries[0].Path, result.Entries[1].Path})
	assert.Contains(t, result.Markdown, "# Merged Artifacts\n")
	assert.Less(t, strings.Index(result.Markdown, "## a.txt"), strings.Index(result.Markdown, "## b.md"))
	assert.Contains(t, result.Markdown, "- **Kind:** note\n")
	assert.Contains(t, result.Markdown, "- **Source:** agent-a\n")
	assert.Contains(t, result.Markdown, "- **Summary:** first summary\n")
	assert.Contains(t, result.Markdown, "````text\nfirst with ``` fence\n````\n")
	assert.Contains(t, result.Markdown, "```text\nsecond\n```\n")
}

func TestMerge_SkipsUnsafeDuplicatesNonTextAndTooLarge(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	writeFile(t, root, "ok.txt", "ok")
	writeFile(t, root, "large.txt", "12345")
	require.NoError(t, os.WriteFile(filepath.Join(root, "binary.bin"), []byte{0xff, 0xfe}, 0o600))

	result, err := Merge(root, []session.Artifact{
		{Path: "ok.txt", Kind: "text"},
		{Path: "./ok.txt", Kind: "text"},
		{Path: "../outside.txt", Kind: "text"},
		{Path: "binary.bin", Kind: "binary"},
		{Path: "large.txt", Kind: "large"},
	}, 4)
	require.NoError(t, err)

	require.Len(t, result.Entries, 1)
	assert.Equal(t, "ok.txt", result.Entries[0].Path)
	assert.Equal(t, "ok", result.Entries[0].Content)
	require.Len(t, result.Warnings, 4)
	assertWarning(t, result.Warnings, "ok.txt", "duplicate artifact")
	assertWarning(t, result.Warnings, "../outside.txt", "path escapes root")
	assertWarning(t, result.Warnings, "binary.bin", "non-text artifact")
	assertWarningContains(t, result.Warnings, "large.txt", "too large")
}

func TestMerge_SkipsAbsolutePathOutsideRootAndAllowsInsideRoot(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	inside := filepath.Join(root, "inside.txt")
	require.NoError(t, os.WriteFile(inside, []byte("inside"), 0o600))
	outside := filepath.Join(t.TempDir(), "outside.txt")
	require.NoError(t, os.WriteFile(outside, []byte("outside"), 0o600))

	result, err := Merge(root, []session.Artifact{
		{Path: inside, Kind: "text"},
		{Path: outside, Kind: "text"},
	}, 1024)
	require.NoError(t, err)

	require.Len(t, result.Entries, 1)
	assert.Equal(t, "inside.txt", result.Entries[0].Path)
	require.Len(t, result.Warnings, 1)
	assert.Equal(t, outside, result.Warnings[0].Path)
	assert.Equal(t, "path escapes root", result.Warnings[0].Reason)
}

func TestMerge_SkipsSymlinkEscape(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	outside := filepath.Join(t.TempDir(), "outside.txt")
	require.NoError(t, os.WriteFile(outside, []byte("outside"), 0o600))

	link := filepath.Join(root, "link.txt")
	if err := os.Symlink(outside, link); err != nil {
		t.Skipf("symlink unavailable: %v", err)
	}

	result, err := Merge(root, []session.Artifact{{Path: "link.txt", Kind: "text"}}, 1024)
	require.NoError(t, err)
	assert.Empty(t, result.Entries)
	require.Len(t, result.Warnings, 1)
	assert.Equal(t, "link.txt", result.Warnings[0].Path)
	assert.Equal(t, "path escapes root", result.Warnings[0].Reason)
}

func TestMerge_ValidatesInputsAndRendersEmptyDocument(t *testing.T) {
	t.Parallel()

	_, err := Merge("", nil, 1)
	require.Error(t, err)
	_, err = Merge(t.TempDir(), nil, 0)
	require.Error(t, err)

	result, err := Merge(t.TempDir(), nil, 1)
	require.NoError(t, err)
	assert.Equal(t, "# Merged Artifacts\n\n_No text artifacts included._\n", result.Markdown)
	assert.Empty(t, result.Entries)
	assert.Empty(t, result.Warnings)
}

func writeFile(t *testing.T, root, relPath, content string) {
	t.Helper()

	path := filepath.Join(root, relPath)
	require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o750))
	require.NoError(t, os.WriteFile(path, []byte(content), 0o600))
}

func assertWarning(t *testing.T, warnings []Warning, path, reason string) {
	t.Helper()

	for _, warning := range warnings {
		if warning.Path == path && warning.Reason == reason {
			return
		}
	}

	assert.Failf(t, "warning not found", "path=%q reason=%q warnings=%v", path, reason, warnings)
}

func assertWarningContains(t *testing.T, warnings []Warning, path, reasonPart string) {
	t.Helper()

	for _, warning := range warnings {
		if warning.Path == path && strings.Contains(warning.Reason, reasonPart) {
			return
		}
	}

	assert.Failf(t, "warning not found", "path=%q reason containing %q warnings=%v", path, reasonPart, warnings)
}
