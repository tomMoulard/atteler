package main

import (
	"path/filepath"
	"reflect"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/tommoulard/atteler/pkg/codeintel"
)

func TestSummarizeAndFormatCodeImportFiles(t *testing.T) {
	t.Parallel()

	root := filepath.Join("tmp", "repo")
	idx := codeintel.Index{Files: []codeintel.File{
		{Path: filepath.Join(root, "pkg", "b.go"), Package: "pkg", Imports: []string{"fmt"}},
		{Path: filepath.Join(root, "cmd", "a.go"), Package: "main", Imports: []string{"context", "fmt"}},
		{Path: filepath.Join(root, "pkg", "empty.go"), Package: "pkg"},
		{Path: filepath.Join(root, "pkg", "c.go"), Package: "pkg", Imports: []string{"bytes", "errors"}},
	}}
	summaries := summarizeCodeImportFiles(root, idx)

	want := []codeImportFileSummary{
		{Path: "cmd/a.go", Package: "main", Imports: 2},
		{Path: "pkg/c.go", Package: "pkg", Imports: 2},
		{Path: "pkg/b.go", Package: "pkg", Imports: 1},
	}
	if !reflect.DeepEqual(summaries, want) {
		require.Failf(t, "unexpected import file summaries", "got %#v, want %#v", summaries, want)
	}

	got := formatCodeImportFileSummary(summaries[0])
	if got != "path=cmd/a.go	package=main	imports=2" {
		require.Failf(t, "unexpected import file summary format", "got %q", got)
	}
}

func TestSummarizeAndFormatCodeImports(t *testing.T) {
	t.Parallel()

	idx := codeintel.Index{ImportEdges: []codeintel.ImportEdge{
		{From: "a.go", Import: "fmt"},
		{From: "b.go", Import: "context"},
		{From: "a.go", Import: "fmt"},
		{From: "c.go", Import: "fmt"},
		{From: "d.go", Import: "context"},
	}}
	summaries := summarizeCodeImports(idx)

	want := []codeImportSummary{{Path: "context", Files: 2}, {Path: "fmt", Files: 2}}
	if !reflect.DeepEqual(summaries, want) {
		require.Failf(t, "unexpected import summaries", "got %#v, want %#v", summaries, want)
	}

	got := formatCodeImportSummary(summaries[1])
	if got != "import=fmt	files=2" {
		require.Failf(t, "unexpected import summary format", "got %q", got)
	}
}

func TestSummarizeCodeImportPrefix(t *testing.T) {
	t.Parallel()

	idx := codeintel.Index{ImportEdges: []codeintel.ImportEdge{
		{From: "b.go", Import: "github.com/tommoulard/atteler/pkg/llm"},
		{From: "a.go", Import: "context"},
		{From: "c.go", Import: "github.com/tommoulard/atteler/pkg/agent"},
		{From: "d.go", Import: "github.com/tommoulard/atteler/pkg/llm"},
		{From: "d.go", Import: "github.com/tommoulard/atteler/pkg/llm"},
	}}
	summaries := summarizeCodeImportPrefix(idx, "github.com/tommoulard/atteler/pkg/")

	want := []codeImportSummary{
		{Path: "github.com/tommoulard/atteler/pkg/llm", Files: 2},
		{Path: "github.com/tommoulard/atteler/pkg/agent", Files: 1},
	}
	if !reflect.DeepEqual(summaries, want) {
		require.Failf(t, "unexpected import prefix summaries", "got %#v, want %#v", summaries, want)
	}

	if got := summarizeCodeImportPrefix(idx, "missing"); len(got) != 0 {
		require.Failf(t, "missing prefix should return empty", "got %#v", got)
	}

	if got := summarizeCodeImportPrefix(idx, " "); got != nil {
		require.Failf(t, "blank prefix should return nil", "got %#v", got)
	}
}

func TestSummarizeCodeImportPrefixFiles(t *testing.T) {
	t.Parallel()

	root := filepath.Join("tmp", "repo")
	idx := codeintel.Index{
		Files: []codeintel.File{
			{Path: filepath.Join(root, "pkg", "b.go"), Package: "pkg"},
			{Path: filepath.Join(root, "cmd", "a.go"), Package: "main"},
			{Path: filepath.Join(root, "pkg", "c.go"), Package: "pkg"},
		},
		ImportEdges: []codeintel.ImportEdge{
			{From: filepath.Join(root, "pkg", "b.go"), Import: "github.com/example/beta"},
			{From: filepath.Join(root, "pkg", "b.go"), Import: "github.com/example/beta"},
			{From: filepath.Join(root, "cmd", "a.go"), Import: "github.com/example/alpha"},
			{From: filepath.Join(root, "cmd", "a.go"), Import: "github.com/example/beta"},
			{From: filepath.Join(root, "pkg", "c.go"), Import: "context"},
		},
	}
	summaries := summarizeCodeImportPrefixFiles(root, idx, "github.com/example/")

	want := []codeImportFileSummary{
		{Path: "cmd/a.go", Package: "main", Imports: 2},
		{Path: "pkg/b.go", Package: "pkg", Imports: 1},
	}
	if !reflect.DeepEqual(summaries, want) {
		require.Failf(t, "unexpected import prefix file summaries", "got %#v, want %#v", summaries, want)
	}

	if got := summarizeCodeImportPrefixFiles(root, idx, "missing"); len(got) != 0 {
		require.Failf(t, "missing prefix should return empty", "got %#v", got)
	}

	if got := summarizeCodeImportPrefixFiles(root, idx, " "); got != nil {
		require.Failf(t, "blank prefix should return nil", "got %#v", got)
	}
}

func TestSummarizeCodeImportPrefixPackages(t *testing.T) {
	t.Parallel()

	idx := codeintel.Index{
		Files: []codeintel.File{
			{Path: "pkg/b.go", Package: "pkg"},
			{Path: "cmd/a.go", Package: "main"},
			{Path: "pkg/c.go", Package: "pkg"},
			{Path: "empty.go"},
		},
		ImportEdges: []codeintel.ImportEdge{
			{From: "pkg/b.go", Import: "github.com/example/beta"},
			{From: "pkg/b.go", Import: "github.com/example/beta"},
			{From: "cmd/a.go", Import: "github.com/example/alpha"},
			{From: "cmd/a.go", Import: "github.com/example/beta"},
			{From: "pkg/c.go", Import: "context"},
			{From: "empty.go", Import: "github.com/example/ignored"},
		},
	}
	summaries := summarizeCodeImportPrefixPackages(idx, "github.com/example/")

	want := []codePackageImportMatchSummary{
		{Name: "main", Files: 1, Imports: 2},
		{Name: "pkg", Files: 1, Imports: 1},
	}
	if !reflect.DeepEqual(summaries, want) {
		require.Failf(t, "unexpected import prefix package summaries", "got %#v, want %#v", summaries, want)
	}

	if got := formatCodePackageImportMatchSummary(summaries[0]); got != "package=main\tfiles=1\timports=2" {
		require.Failf(t, "unexpected import package summary format", "got %q", got)
	}

	if got := summarizeCodeImportPrefixPackages(idx, "missing"); len(got) != 0 {
		require.Failf(t, "missing prefix should return empty", "got %#v", got)
	}

	if got := summarizeCodeImportPrefixPackages(idx, " "); got != nil {
		require.Failf(t, "blank prefix should return nil", "got %#v", got)
	}
}

func TestCodeImportEdgesWithPrefix(t *testing.T) {
	t.Parallel()

	idx := codeintel.Index{ImportEdges: []codeintel.ImportEdge{
		{From: "b.go", Import: "github.com/tommoulard/atteler/pkg/llm"},
		{From: "a.go", Import: "context"},
		{From: "c.go", Import: "github.com/tommoulard/atteler/pkg/agent"},
	}}
	edges := codeImportEdgesWithPrefix(idx, "github.com/tommoulard/atteler/pkg/")

	want := []codeintel.ImportEdge{
		{From: "b.go", Import: "github.com/tommoulard/atteler/pkg/llm"},
		{From: "c.go", Import: "github.com/tommoulard/atteler/pkg/agent"},
	}
	if !reflect.DeepEqual(edges, want) {
		require.Failf(t, "unexpected import prefix edges", "got %#v, want %#v", edges, want)
	}

	if got := codeImportEdgesWithPrefix(idx, " "); got != nil {
		require.Failf(t, "blank import prefix should return nil", "got %#v", got)
	}
}

func TestCodeImportEdgesForPath(t *testing.T) {
	t.Parallel()

	idx := codeintel.Index{ImportEdges: []codeintel.ImportEdge{
		{From: "b.go", Import: "fmt"},
		{From: "a.go", Import: "context"},
		{From: "c.go", Import: "context"},
	}}
	edges := codeImportEdgesForPath(idx, "context")

	want := []codeintel.ImportEdge{{From: "a.go", Import: "context"}, {From: "c.go", Import: "context"}}
	if !reflect.DeepEqual(edges, want) {
		require.Failf(t, "unexpected import path edges", "got %#v, want %#v", edges, want)
	}

	if got := codeImportEdgesForPath(idx, " "); got != nil {
		require.Failf(t, "blank import path should return nil", "got %#v", got)
	}
}

func TestSummarizeCodeImportPath(t *testing.T) {
	t.Parallel()

	idx := codeintel.Index{ImportEdges: []codeintel.ImportEdge{
		{From: "b.go", Import: "fmt"},
		{From: "a.go", Import: "context"},
		{From: "a.go", Import: "context"},
		{From: "c.go", Import: "context"},
	}}
	summaries := summarizeCodeImportPath(idx, "context")

	want := []codeImportSummary{{Path: "context", Files: 2}}
	if !reflect.DeepEqual(summaries, want) {
		require.Failf(t, "unexpected import path summary", "got %#v, want %#v", summaries, want)
	}

	if got := summarizeCodeImportPath(idx, "missing"); len(got) != 0 {
		require.Failf(t, "missing import path should return empty", "got %#v", got)
	}

	if got := summarizeCodeImportPath(idx, " "); got != nil {
		require.Failf(t, "blank import path should return nil", "got %#v", got)
	}
}

func TestSummarizeCodeImportPathFiles(t *testing.T) {
	t.Parallel()

	root := filepath.Join("tmp", "repo")
	idx := codeintel.Index{
		Files: []codeintel.File{
			{Path: filepath.Join(root, "pkg", "b.go"), Package: "pkg"},
			{Path: filepath.Join(root, "cmd", "a.go"), Package: "main"},
			{Path: filepath.Join(root, "pkg", "c.go"), Package: "pkg"},
		},
		ImportEdges: []codeintel.ImportEdge{
			{From: filepath.Join(root, "pkg", "b.go"), Import: "context"},
			{From: filepath.Join(root, "pkg", "b.go"), Import: "context"},
			{From: filepath.Join(root, "cmd", "a.go"), Import: "context"},
			{From: filepath.Join(root, "pkg", "c.go"), Import: "fmt"},
		},
	}
	summaries := summarizeCodeImportPathFiles(root, idx, "context")

	want := []codeImportFileSummary{
		{Path: "cmd/a.go", Package: "main", Imports: 1},
		{Path: "pkg/b.go", Package: "pkg", Imports: 1},
	}
	if !reflect.DeepEqual(summaries, want) {
		require.Failf(t, "unexpected import path file summaries", "got %#v, want %#v", summaries, want)
	}

	if got := summarizeCodeImportPathFiles(root, idx, "missing"); len(got) != 0 {
		require.Failf(t, "missing import path should return empty", "got %#v", got)
	}

	if got := summarizeCodeImportPathFiles(root, idx, " "); got != nil {
		require.Failf(t, "blank import path should return nil", "got %#v", got)
	}
}

func TestSummarizeAndFormatCodeImportPathPackages(t *testing.T) {
	t.Parallel()

	idx := codeintel.Index{
		Files: []codeintel.File{
			{Path: "pkg/b.go", Package: "pkg"},
			{Path: "cmd/a.go", Package: "main"},
			{Path: "pkg/c.go", Package: "pkg"},
			{Path: "empty.go"},
		},
		ImportEdges: []codeintel.ImportEdge{
			{From: "pkg/b.go", Import: "context"},
			{From: "pkg/b.go", Import: "context"},
			{From: "cmd/a.go", Import: "context"},
			{From: "pkg/c.go", Import: "fmt"},
			{From: "empty.go", Import: "context"},
		},
	}
	summaries := summarizeCodeImportPathPackages(idx, "context")

	want := []codePackageImportMatchSummary{
		{Name: "main", Files: 1},
		{Name: "pkg", Files: 1},
	}
	if !reflect.DeepEqual(summaries, want) {
		require.Failf(t, "unexpected import path package summaries", "got %#v, want %#v", summaries, want)
	}

	if got := formatCodePackageImportMatchSummary(summaries[0]); got != "package=main\tfiles=1" {
		require.Failf(t, "unexpected import package summary format", "got %q", got)
	}

	if got := summarizeCodeImportPathPackages(idx, "missing"); len(got) != 0 {
		require.Failf(t, "missing import path should return empty", "got %#v", got)
	}

	if got := summarizeCodeImportPathPackages(idx, " "); got != nil {
		require.Failf(t, "blank import path should return nil", "got %#v", got)
	}
}

func TestFormatCodeImportEdge(t *testing.T) {
	t.Parallel()

	root := filepath.Join("tmp", "repo")
	got := formatCodeImportEdge(root, codeintel.ImportEdge{
		From:   filepath.Join(root, "pkg", "runner.go"),
		Import: "context",
	})

	want := "path=pkg/runner.go\timport=context"
	if got != want {
		require.Failf(t, "unexpected code import format", "got %q, want %q", got, want)
	}
}

func TestRelativeCodePath(t *testing.T) {
	t.Parallel()

	root := filepath.Join("tmp", "repo")

	got := relativeCodePath(root, filepath.Join(root, "cmd", "atteler", "main.go"))
	if got != "cmd/atteler/main.go" {
		require.Failf(t, "unexpected relative code path", "got %q", got)
	}
}
