package contextref

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestExpand_AppendsReferencedFile(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	writeFile(t, dir, "README.md", "hello\n")

	result, err := Expand("summarize @README.md", Options{Root: dir})
	if err != nil {
		require.NoError(t, err)
	}

	if len(result.References) != 1 {
		require.Failf(t, "unexpected failure", "references len = %d, want 1", len(result.References))
	}

	if result.References[0].Path != "README.md" {
		assert.Failf(t, "assertion failed", "path = %q", result.References[0].Path)
	}

	if result.References[0].Kind != "file" {
		assert.Failf(t, "assertion failed", "kind = %q, want file", result.References[0].Kind)
	}

	if !strings.Contains(result.Prompt, `<file path="README.md" truncated="false">`) {
		require.Failf(t, "unexpected failure", "prompt missing file tag:\n%s", result.Prompt)
	}

	if !strings.Contains(result.Prompt, "hello\n") {
		require.Failf(t, "unexpected failure", "prompt missing content:\n%s", result.Prompt)
	}
}

func TestExpand_TruncatesByLimit(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	writeFile(t, dir, "big.txt", "abcdef")

	result, err := Expand("read @big.txt", Options{
		Root:          dir,
		MaxFileBytes:  3,
		MaxTotalBytes: 10,
	})
	if err != nil {
		require.NoError(t, err)
	}

	if !result.References[0].Truncated {
		require.FailNow(t, "expected truncated reference")
	}

	if !strings.Contains(result.Prompt, "abc\n</file>") {
		require.Failf(t, "unexpected failure", "prompt = %q", result.Prompt)
	}
}

func TestExpand_AppendsDirectoryTree(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	writeFile(t, dir, "pkg/a.go", "package pkg\n")
	writeFile(t, dir, "pkg/nested/b.go", "package nested\n")

	result, err := Expand("map @pkg", Options{Root: dir})
	if err != nil {
		require.NoError(t, err)
	}

	if len(result.References) != 1 {
		require.Failf(t, "unexpected failure", "references len = %d, want 1", len(result.References))
	}

	if result.References[0].Kind != "directory" {
		require.Failf(t, "unexpected failure", "kind = %q, want directory", result.References[0].Kind)
	}

	if !strings.Contains(result.Prompt, `<directory path="pkg" truncated="false">`) {
		require.Failf(t, "unexpected failure", "prompt missing directory tag:\n%s", result.Prompt)
	}

	for _, want := range []string{"a.go", "nested/", "nested/b.go"} {
		if !strings.Contains(result.Prompt, want) {
			require.Failf(t, "unexpected failure", "prompt missing %q:\n%s", want, result.Prompt)
		}
	}
}

func TestExpand_TruncatesDirectoryTree(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	writeFile(t, dir, "pkg/alpha.go", "package pkg\n")
	writeFile(t, dir, "pkg/beta.go", "package pkg\n")

	result, err := Expand("map @pkg", Options{
		Root:          dir,
		MaxFileBytes:  5,
		MaxTotalBytes: 20,
	})
	if err != nil {
		require.NoError(t, err)
	}

	if !result.References[0].Truncated {
		require.FailNow(t, "expected truncated directory reference")
	}
}

func TestExpand_RejectsEscapingRoot(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	parent := filepath.Dir(dir)
	writeFile(t, parent, "outside.txt", "secret")

	_, err := Expand("read @../outside.txt", Options{Root: dir})
	if err == nil {
		require.FailNow(t, "expected root escape error")
	}
}

func TestExpand_RejectsSymlinkEscapingRoot(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	outsideDir := t.TempDir()
	outsidePath := writeFile(t, outsideDir, "outside.txt", "secret")

	linkPath := filepath.Join(dir, "linked.txt")
	if err := os.Symlink(outsidePath, linkPath); err != nil {
		t.Skipf("symlink unsupported: %v", err)
	}

	_, err := Expand("read @linked.txt", Options{Root: dir})
	if err == nil {
		require.FailNow(t, "expected symlink escape error")
	}

	if !strings.Contains(err.Error(), "escapes root") {
		require.Failf(t, "unexpected failure", "error = %q, want root escape", err)
	}
}

func TestExpand_IgnoresMentionsAndEmails(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()

	result, err := Expand("email a@example.com and ask @reviewer", Options{Root: dir})
	if err != nil {
		require.NoError(t, err)
	}

	if result.Prompt != "email a@example.com and ask @reviewer" {
		require.Failf(t, "unexpected failure", "prompt = %q", result.Prompt)
	}

	if len(result.References) != 0 {
		require.Failf(t, "unexpected failure", "references = %+v", result.References)
	}
}

func writeFile(t *testing.T, dir, name, content string) string {
	t.Helper()

	path := filepath.Join(dir, name)
	if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
		require.NoError(t, err)
	}

	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		require.NoError(t, err)
	}

	return path
}
