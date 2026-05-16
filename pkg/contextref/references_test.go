package contextref

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// ---------------------------------------------------------------------------
// Single file loading
// ---------------------------------------------------------------------------

func TestLoadReferences_LocalFile(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	writeFile(t, dir, "docs/guide.md", "# Guide\nHello world\n")

	refs, err := LoadReferences(context.Background(), []string{"docs/guide.md"}, Options{Root: dir})
	require.NoError(t, err)
	require.Len(t, refs, 1)

	assert.Equal(t, "docs/guide.md", refs[0].Source)
	assert.Equal(t, "file", refs[0].Kind)
	assert.Contains(t, refs[0].Content, "# Guide")
	assert.False(t, refs[0].Truncated)
}

func TestLoadReferences_TruncatesLargeFile(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	writeFile(t, dir, "big.txt", "abcdefghij")

	refs, err := LoadReferences(context.Background(), []string{"big.txt"}, Options{
		Root:          dir,
		MaxFileBytes:  5,
		MaxTotalBytes: 100,
	})
	require.NoError(t, err)
	require.Len(t, refs, 1)

	assert.True(t, refs[0].Truncated)
	assert.Equal(t, "abcde", refs[0].Content)
}

func TestLoadReferences_SkipsEmptyEntries(t *testing.T) {
	t.Parallel()

	refs, err := LoadReferences(context.Background(), []string{"", "  ", ""}, Options{Root: t.TempDir()})
	require.NoError(t, err)
	assert.Empty(t, refs)
}

func TestLoadReferences_NilSlice(t *testing.T) {
	t.Parallel()

	refs, err := LoadReferences(context.Background(), nil, Options{Root: t.TempDir()})
	require.NoError(t, err)
	assert.Nil(t, refs)
}

func TestLoadReferences_RespectsAggregateByteBudget(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	writeFile(t, dir, "a.txt", "aaaa") // 4 bytes
	writeFile(t, dir, "b.txt", "bbbb") // 4 bytes
	writeFile(t, dir, "c.txt", "cccc") // 4 bytes -- should be dropped

	refs, err := LoadReferences(context.Background(), []string{"a.txt", "b.txt", "c.txt"}, Options{
		Root:          dir,
		MaxFileBytes:  100,
		MaxTotalBytes: 8,
	})
	require.NoError(t, err)
	assert.Len(t, refs, 2)
	assert.Equal(t, "a.txt", refs[0].Source)
	assert.Equal(t, "b.txt", refs[1].Source)
}

func TestLoadReferences_SkipsBinaryFiles(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	writeFile(t, dir, "text.txt", "hello world")
	writeBinaryFile(t, dir, "image.png", []byte{0x89, 'P', 'N', 'G', 0, 0, 0})

	refs, err := LoadReferences(context.Background(), []string{"text.txt"}, Options{Root: dir})
	require.NoError(t, err)
	require.Len(t, refs, 1)
	assert.Equal(t, "hello world", refs[0].Content)

	// Binary file as direct reference returns an error.
	_, err = LoadReferences(context.Background(), []string{"image.png"}, Options{Root: dir})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "binary file")
}

// ---------------------------------------------------------------------------
// Paths outside root (allowed for configured references)
// ---------------------------------------------------------------------------

func TestLoadReferences_AllowsPathOutsideRoot(t *testing.T) {
	t.Parallel()

	outer := t.TempDir()
	inner := filepath.Join(outer, "project")
	require.NoError(t, os.MkdirAll(inner, 0o750))
	writeFile(t, outer, "style-guide.md", "# Style Guide\nUse gofmt.\n")

	refs, err := LoadReferences(context.Background(), []string{"../style-guide.md"}, Options{Root: inner})
	require.NoError(t, err)
	require.Len(t, refs, 1)

	assert.Equal(t, "../style-guide.md", refs[0].Source)
	assert.Contains(t, refs[0].Content, "# Style Guide")
}

func TestLoadReferences_AllowsAbsolutePath(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	writeFile(t, dir, "ref.txt", "reference content")

	absPath := filepath.Join(dir, "ref.txt")
	refs, err := LoadReferences(context.Background(), []string{absPath}, Options{Root: t.TempDir()})
	require.NoError(t, err)
	require.Len(t, refs, 1)

	assert.Equal(t, absPath, refs[0].Source)
	assert.Equal(t, "reference content", refs[0].Content)
}

// ---------------------------------------------------------------------------
// Directory loading (reads file contents)
// ---------------------------------------------------------------------------

func TestLoadReferences_DirectoryReadsFileContents(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	writeFile(t, dir, "src/main.go", "package main\n")
	writeFile(t, dir, "src/util.go", "package util\n")

	refs, err := LoadReferences(context.Background(), []string{"src"}, Options{Root: dir})
	require.NoError(t, err)
	require.Len(t, refs, 2, "directory should produce one LoadedReference per file")

	sources := collectSources(refs)
	assert.Contains(t, sources, "src/main.go")
	assert.Contains(t, sources, "src/util.go")

	for _, ref := range refs {
		assert.Equal(t, "file", ref.Kind)
		assert.Contains(t, ref.Content, "package")
	}
}

func TestLoadReferences_DirectoryReadsNestedFiles(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	writeFile(t, dir, "repo/pkg/handler/auth.go", "package handler\n")
	writeFile(t, dir, "repo/pkg/model/user.go", "package model\n")
	writeFile(t, dir, "repo/README.md", "# Repo\n")

	refs, err := LoadReferences(context.Background(), []string{"repo"}, Options{Root: dir})
	require.NoError(t, err)
	require.Len(t, refs, 3)

	sources := collectSources(refs)
	assert.Contains(t, sources, "repo/pkg/handler/auth.go")
	assert.Contains(t, sources, "repo/pkg/model/user.go")
	assert.Contains(t, sources, "repo/README.md")
}

func TestLoadReferences_DirectorySkipsBinaryFiles(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	writeFile(t, dir, "src/main.go", "package main\n")
	writeBinaryFile(t, dir, "src/icon.png", []byte{0x89, 'P', 'N', 'G', 0, 0})

	refs, err := LoadReferences(context.Background(), []string{"src"}, Options{Root: dir})
	require.NoError(t, err)
	require.Len(t, refs, 1)
	assert.Equal(t, "src/main.go", refs[0].Source)
}

func TestLoadReferences_DirectorySkipsGitDir(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	writeFile(t, dir, "repo/main.go", "package main\n")
	writeFile(t, dir, "repo/.git/HEAD", "ref: refs/heads/main\n")

	refs, err := LoadReferences(context.Background(), []string{"repo"}, Options{Root: dir})
	require.NoError(t, err)
	require.Len(t, refs, 1)
	assert.Equal(t, "repo/main.go", refs[0].Source)
}

func TestLoadReferences_DirectoryRespectsAggregateBudget(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	writeFile(t, dir, "src/a.go", "aaaa") // 4 bytes
	writeFile(t, dir, "src/b.go", "bbbb") // 4 bytes
	writeFile(t, dir, "src/c.go", "cccc") // 4 bytes -- may be dropped

	refs, err := LoadReferences(context.Background(), []string{"src"}, Options{
		Root:          dir,
		MaxFileBytes:  100,
		MaxTotalBytes: 8,
	})
	require.NoError(t, err)
	assert.Len(t, refs, 2, "aggregate budget should cap at 2 files (8 bytes)")
}

func TestLoadReferences_DirectoryOutsideRoot(t *testing.T) {
	t.Parallel()

	outer := t.TempDir()
	inner := filepath.Join(outer, "project")
	require.NoError(t, os.MkdirAll(inner, 0o750))
	writeFile(t, outer, "reference/style.go", "package style\n")

	refs, err := LoadReferences(context.Background(), []string{"../reference"}, Options{Root: inner})
	require.NoError(t, err)
	require.Len(t, refs, 1)
	assert.Equal(t, "../reference/style.go", refs[0].Source)
	assert.Contains(t, refs[0].Content, "package style")
}

// ---------------------------------------------------------------------------
// Glob pattern support
// ---------------------------------------------------------------------------

func TestLoadReferences_GlobStar(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	writeFile(t, dir, "pkg/main.go", "package main\n")
	writeFile(t, dir, "pkg/util.go", "package util\n")
	writeFile(t, dir, "pkg/readme.md", "# Pkg\n")

	refs, err := LoadReferences(context.Background(), []string{"pkg/*.go"}, Options{Root: dir})
	require.NoError(t, err)
	require.Len(t, refs, 2, "*.go should match only .go files")

	for _, ref := range refs {
		assert.True(t, filepath.Ext(ref.Source) == ".go" || filepath.Ext(filepath.Base(ref.Source)) == ".go",
			"unexpected source: %s", ref.Source)
	}
}

func TestLoadReferences_GlobDoublestar(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	writeFile(t, dir, "repo/pkg/handler/auth.go", "package handler\n")
	writeFile(t, dir, "repo/pkg/model/user.go", "package model\n")
	writeFile(t, dir, "repo/docs/design.md", "# Design\n")

	refs, err := LoadReferences(context.Background(), []string{"repo/**/*.go"}, Options{Root: dir})
	require.NoError(t, err)
	require.Len(t, refs, 2)

	sources := collectSources(refs)
	assert.Contains(t, sources, "repo/pkg/handler/auth.go")
	assert.Contains(t, sources, "repo/pkg/model/user.go")
}

func TestLoadReferences_GlobDoublestarSkipsBinaryFiles(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	writeFile(t, dir, "src/main.go", "package main\n")
	writeBinaryFile(t, dir, "src/data.bin", []byte{0x00, 0x01, 0x02})

	refs, err := LoadReferences(context.Background(), []string{"src/**/*"}, Options{Root: dir})
	require.NoError(t, err)
	require.Len(t, refs, 1)
	assert.Equal(t, "src/main.go", refs[0].Source)
}

func TestLoadReferences_GlobOutsideRoot(t *testing.T) {
	t.Parallel()

	outer := t.TempDir()
	inner := filepath.Join(outer, "project")
	require.NoError(t, os.MkdirAll(inner, 0o750))
	writeFile(t, outer, "reference/handler.go", "package handler\n")
	writeFile(t, outer, "reference/model.go", "package model\n")
	writeFile(t, outer, "reference/readme.md", "# Reference\n")

	refs, err := LoadReferences(context.Background(), []string{"../reference/*.go"}, Options{Root: inner})
	require.NoError(t, err)
	require.Len(t, refs, 2)

	for _, ref := range refs {
		assert.Contains(t, ref.Content, "package")
	}
}

func TestLoadReferences_GlobRespectsAggregateBudget(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	writeFile(t, dir, "src/a.go", "aaaa") // 4 bytes
	writeFile(t, dir, "src/b.go", "bbbb") // 4 bytes
	writeFile(t, dir, "src/c.go", "cccc") // 4 bytes

	refs, err := LoadReferences(context.Background(), []string{"src/*.go"}, Options{
		Root:          dir,
		MaxFileBytes:  100,
		MaxTotalBytes: 8,
	})
	require.NoError(t, err)
	assert.Len(t, refs, 2, "budget should cap the glob expansion")
}

// ---------------------------------------------------------------------------
// URL loading
// ---------------------------------------------------------------------------

func TestLoadReferences_URL(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if _, err := w.Write([]byte("remote content")); err != nil {
			t.Error(err)
		}
	}))
	defer srv.Close()

	refs, err := LoadReferences(context.Background(), []string{srv.URL + "/doc.txt"}, Options{Root: t.TempDir()})
	require.NoError(t, err)
	require.Len(t, refs, 1)

	assert.Equal(t, srv.URL+"/doc.txt", refs[0].Source)
	assert.Equal(t, "url", refs[0].Kind)
	assert.Equal(t, "remote content", refs[0].Content)
	assert.False(t, refs[0].Truncated)
}

func TestLoadReferences_URLHTTPError(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	}))
	defer srv.Close()

	_, err := LoadReferences(context.Background(), []string{srv.URL + "/missing"}, Options{Root: t.TempDir()})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "HTTP 404")
}

func TestLoadReferences_MixedPathsAndURLs(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	writeFile(t, dir, "local.md", "local content")

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if _, err := w.Write([]byte("remote content")); err != nil {
			t.Error(err)
		}
	}))
	defer srv.Close()

	refs, err := LoadReferences(context.Background(), []string{"local.md", srv.URL + "/remote.md"}, Options{Root: dir})
	require.NoError(t, err)
	require.Len(t, refs, 2)

	assert.Equal(t, "file", refs[0].Kind)
	assert.Equal(t, "url", refs[1].Kind)
}

// ---------------------------------------------------------------------------
// FormatReferences
// ---------------------------------------------------------------------------

func TestFormatReferences_Empty(t *testing.T) {
	t.Parallel()
	assert.Empty(t, FormatReferences(nil))
}

func TestFormatReferences_SingleFile(t *testing.T) {
	t.Parallel()

	refs := []LoadedReference{
		{Source: "README.md", Kind: "file", Content: "hello\n", Bytes: 6},
	}

	got := FormatReferences(refs)
	assert.Contains(t, got, "<configured_references>")
	assert.Contains(t, got, `<file source="README.md" truncated="false">`)
	assert.Contains(t, got, "hello\n")
	assert.Contains(t, got, "</file>")
	assert.Contains(t, got, "</configured_references>")
}

func TestFormatReferences_URL(t *testing.T) {
	t.Parallel()

	refs := []LoadedReference{
		{Source: "https://example.com/docs", Kind: "url", Content: "doc content\n", Bytes: 12},
	}

	got := FormatReferences(refs)
	assert.Contains(t, got, `<url source="https://example.com/docs" truncated="false">`)
	assert.Contains(t, got, "doc content")
	assert.Contains(t, got, "</url>")
}

func TestFormatReferences_Truncated(t *testing.T) {
	t.Parallel()

	refs := []LoadedReference{
		{Source: "big.txt", Kind: "file", Content: "abc", Bytes: 3, Truncated: true},
	}

	got := FormatReferences(refs)
	assert.Contains(t, got, `truncated="true"`)
}

func TestFormatReferences_MultipleEntries(t *testing.T) {
	t.Parallel()

	refs := []LoadedReference{
		{Source: "a.txt", Kind: "file", Content: "aaa\n", Bytes: 4},
		{Source: "https://example.com", Kind: "url", Content: "bbb\n", Bytes: 4},
	}

	got := FormatReferences(refs)
	assert.Contains(t, got, `<file source="a.txt"`)
	assert.Contains(t, got, `<url source="https://example.com"`)
}

// ---------------------------------------------------------------------------
// Glob matching internals
// ---------------------------------------------------------------------------

func TestMatchGlob(t *testing.T) {
	t.Parallel()

	tests := []struct {
		pattern string
		name    string
		want    bool
	}{
		{"src/*.go", "src/main.go", true},
		{"src/*.go", "src/sub/main.go", false},
		{"src/**/*.go", "src/main.go", true},
		{"src/**/*.go", "src/sub/main.go", true},
		{"src/**/*.go", "src/sub/deep/main.go", true},
		{"src/**/*.go", "src/main.md", false},
		{"**/*.go", "main.go", true},
		{"**/*.go", "pkg/main.go", true},
		{"**", "any/path/works", true},
		{"a/**/z/*.go", "a/z/file.go", true},
		{"a/**/z/*.go", "a/b/c/z/file.go", true},
		{"a/**/z/*.go", "a/b/c/z/file.md", false},
	}

	for _, tt := range tests {
		t.Run(tt.pattern+"_"+tt.name, func(t *testing.T) {
			t.Parallel()
			assert.Equal(t, tt.want, matchGlob(tt.pattern, tt.name))
		})
	}
}

func TestGlobBase(t *testing.T) {
	t.Parallel()

	tests := []struct {
		pattern  string
		wantBase string
		wantRest string
	}{
		{"src/**/*.go", "src", "**/*.go"},
		{"../repo/pkg/**/*.go", "../repo/pkg", "**/*.go"},
		{"*.go", ".", "*.go"},
		{"pkg/handler.go", "pkg/handler.go", ""},
		{"a/b/c", "a/b/c", ""},
	}

	for _, tt := range tests {
		t.Run(tt.pattern, func(t *testing.T) {
			t.Parallel()

			base, rest := globBase(tt.pattern)
			assert.Equal(t, tt.wantBase, base)
			assert.Equal(t, tt.wantRest, rest)
		})
	}
}

func TestIsBinary(t *testing.T) {
	t.Parallel()

	assert.True(t, isBinary([]byte{0x00, 0x01, 0x02}))
	assert.True(t, isBinary([]byte("hello\x00world")))
	assert.False(t, isBinary([]byte("hello world")))
	assert.False(t, isBinary([]byte("package main\nfunc main() {}\n")))
	assert.False(t, isBinary(nil))
	assert.False(t, isBinary([]byte{}))
}

// ---------------------------------------------------------------------------
// Helpers
// ---------------------------------------------------------------------------

func collectSources(refs []LoadedReference) []string {
	var sources []string
	for _, ref := range refs {
		sources = append(sources, ref.Source)
	}

	return sources
}

func writeBinaryFile(t *testing.T, dir, rel string, content []byte) {
	t.Helper()

	path := filepath.Join(dir, filepath.FromSlash(rel))
	require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o750))
	require.NoError(t, os.WriteFile(path, content, 0o600))
}
