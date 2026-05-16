package contextref

import (
	"context"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

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

func TestLoadReferences_LocalDirectory(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	writeFile(t, dir, "src/main.go", "package main\n")
	writeFile(t, dir, "src/util.go", "package main\n")

	refs, err := LoadReferences(context.Background(), []string{"src"}, Options{Root: dir})
	require.NoError(t, err)
	require.Len(t, refs, 1)

	assert.Equal(t, "src", refs[0].Source)
	assert.Equal(t, "directory", refs[0].Kind)
	assert.Contains(t, refs[0].Content, "main.go")
	assert.Contains(t, refs[0].Content, "util.go")
}

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

func TestLoadReferences_RejectsPathEscapingRoot(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	parentDir := filepath.Dir(dir)
	writeFile(t, parentDir, "secret.txt", "top secret")

	_, err := LoadReferences(context.Background(), []string{"../secret.txt"}, Options{Root: dir})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "escapes root")
}

func TestLoadReferences_SkipsEmptyEntries(t *testing.T) {
	t.Parallel()

	refs, err := LoadReferences(context.Background(), []string{"", "  ", ""}, Options{Root: t.TempDir()})
	require.NoError(t, err)
	assert.Empty(t, refs)
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

func TestLoadReferences_NilSlice(t *testing.T) {
	t.Parallel()

	refs, err := LoadReferences(context.Background(), nil, Options{Root: t.TempDir()})
	require.NoError(t, err)
	assert.Nil(t, refs)
}

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
		{Source: "src", Kind: "directory", Content: "main.go\n", Bytes: 8},
	}

	got := FormatReferences(refs)
	assert.Contains(t, got, `<file source="a.txt"`)
	assert.Contains(t, got, `<url source="https://example.com"`)
	assert.Contains(t, got, `<directory source="src"`)
}
