package contextref

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestExpand_AppendsReferencedFile(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, "README.md", "hello\n")

	result, err := Expand("summarize @README.md", Options{Root: dir})
	if err != nil {
		t.Fatal(err)
	}

	if len(result.References) != 1 {
		t.Fatalf("references len = %d, want 1", len(result.References))
	}
	if result.References[0].Path != "README.md" {
		t.Errorf("path = %q", result.References[0].Path)
	}
	if result.References[0].Kind != "file" {
		t.Errorf("kind = %q, want file", result.References[0].Kind)
	}
	if !strings.Contains(result.Prompt, `<file path="README.md" truncated="false">`) {
		t.Fatalf("prompt missing file tag:\n%s", result.Prompt)
	}
	if !strings.Contains(result.Prompt, "hello\n") {
		t.Fatalf("prompt missing content:\n%s", result.Prompt)
	}
}

func TestExpand_TruncatesByLimit(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, "big.txt", "abcdef")

	result, err := Expand("read @big.txt", Options{
		Root:          dir,
		MaxFileBytes:  3,
		MaxTotalBytes: 10,
	})
	if err != nil {
		t.Fatal(err)
	}

	if !result.References[0].Truncated {
		t.Fatal("expected truncated reference")
	}
	if !strings.Contains(result.Prompt, "abc\n</file>") {
		t.Fatalf("prompt = %q", result.Prompt)
	}
}

func TestExpand_AppendsDirectoryTree(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, "pkg/a.go", "package pkg\n")
	writeFile(t, dir, "pkg/nested/b.go", "package nested\n")

	result, err := Expand("map @pkg", Options{Root: dir})
	if err != nil {
		t.Fatal(err)
	}

	if len(result.References) != 1 {
		t.Fatalf("references len = %d, want 1", len(result.References))
	}
	if result.References[0].Kind != "directory" {
		t.Fatalf("kind = %q, want directory", result.References[0].Kind)
	}
	if !strings.Contains(result.Prompt, `<directory path="pkg" truncated="false">`) {
		t.Fatalf("prompt missing directory tag:\n%s", result.Prompt)
	}
	for _, want := range []string{"a.go", "nested/", "nested/b.go"} {
		if !strings.Contains(result.Prompt, want) {
			t.Fatalf("prompt missing %q:\n%s", want, result.Prompt)
		}
	}
}

func TestExpand_TruncatesDirectoryTree(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, "pkg/alpha.go", "package pkg\n")
	writeFile(t, dir, "pkg/beta.go", "package pkg\n")

	result, err := Expand("map @pkg", Options{
		Root:          dir,
		MaxFileBytes:  5,
		MaxTotalBytes: 20,
	})
	if err != nil {
		t.Fatal(err)
	}
	if !result.References[0].Truncated {
		t.Fatal("expected truncated directory reference")
	}
}

func TestExpand_RejectsEscapingRoot(t *testing.T) {
	dir := t.TempDir()
	parent := filepath.Dir(dir)
	writeFile(t, parent, "outside.txt", "secret")

	_, err := Expand("read @../outside.txt", Options{Root: dir})
	if err == nil {
		t.Fatal("expected root escape error")
	}
}

func TestExpand_RejectsSymlinkEscapingRoot(t *testing.T) {
	dir := t.TempDir()
	outsideDir := t.TempDir()
	outsidePath := writeFile(t, outsideDir, "outside.txt", "secret")
	linkPath := filepath.Join(dir, "linked.txt")
	if err := os.Symlink(outsidePath, linkPath); err != nil {
		t.Skipf("symlink unsupported: %v", err)
	}

	_, err := Expand("read @linked.txt", Options{Root: dir})
	if err == nil {
		t.Fatal("expected symlink escape error")
	}
	if !strings.Contains(err.Error(), "escapes root") {
		t.Fatalf("error = %q, want root escape", err)
	}
}

func TestExpand_IgnoresMentionsAndEmails(t *testing.T) {
	dir := t.TempDir()

	result, err := Expand("email a@example.com and ask @reviewer", Options{Root: dir})
	if err != nil {
		t.Fatal(err)
	}
	if result.Prompt != "email a@example.com and ask @reviewer" {
		t.Fatalf("prompt = %q", result.Prompt)
	}
	if len(result.References) != 0 {
		t.Fatalf("references = %+v", result.References)
	}
}

func writeFile(t *testing.T, dir, name, content string) string {
	t.Helper()

	path := filepath.Join(dir, name)
	if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}
