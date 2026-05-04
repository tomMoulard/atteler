// Package artifactmerge aggregates session artifacts into a deterministic Markdown document.
package artifactmerge

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"unicode/utf8"

	"github.com/tommoulard/atteler/pkg/session"
)

// Warning records why an artifact was skipped while building a merge document.
type Warning struct {
	Path   string
	Reason string
}

// Entry is a normalized artifact included in a merge document.
type Entry struct {
	Path    string
	Kind    string
	Source  string
	Summary string
	Content string
}

// Result contains the merged Markdown document and skipped-artifact warnings.
type Result struct {
	Markdown string
	Entries  []Entry
	Warnings []Warning
}

// Merge reads text artifacts under root and renders them into a deterministic Markdown document.
func Merge(root string, artifacts []session.Artifact, maxBytes int64) (Result, error) {
	if strings.TrimSpace(root) == "" {
		return Result{}, errors.New("artifactmerge: root is required")
	}

	if maxBytes <= 0 {
		return Result{}, errors.New("artifactmerge: maxBytes must be positive")
	}

	rootAbs, err := filepath.Abs(root)
	if err != nil {
		return Result{}, fmt.Errorf("artifactmerge: resolve root: %w", err)
	}

	rootAbs = filepath.Clean(rootAbs)

	rootReal, err := filepath.EvalSymlinks(rootAbs)
	if err != nil {
		return Result{}, fmt.Errorf("artifactmerge: resolve root symlinks: %w", err)
	}

	rootReal = filepath.Clean(rootReal)

	seen := make(map[string]struct{}, len(artifacts))
	result := Result{}

	for _, artifact := range artifacts {
		relPath, fullPath, ok := normalizePath(rootAbs, artifact.Path)
		if !ok {
			result.Warnings = append(result.Warnings, Warning{Path: strings.TrimSpace(artifact.Path), Reason: "path escapes root"})
			continue
		}

		if _, exists := seen[relPath]; exists {
			result.Warnings = append(result.Warnings, Warning{Path: relPath, Reason: "duplicate artifact"})
			continue
		}

		seen[relPath] = struct{}{}

		resolvedPath, safe, err := resolveSafePath(rootReal, fullPath)
		if err != nil {
			result.Warnings = append(result.Warnings, Warning{Path: relPath, Reason: "read failed: " + err.Error()})
			continue
		}

		if !safe {
			result.Warnings = append(result.Warnings, Warning{Path: relPath, Reason: "path escapes root"})
			continue
		}

		content, warning, ok := readTextFile(resolvedPath, maxBytes)
		if !ok {
			warning.Path = relPath
			result.Warnings = append(result.Warnings, warning)

			continue
		}

		result.Entries = append(result.Entries, Entry{
			Path:    relPath,
			Kind:    strings.TrimSpace(artifact.Kind),
			Source:  strings.TrimSpace(artifact.SourceAgent),
			Summary: strings.TrimSpace(artifact.Summary),
			Content: content,
		})
	}

	sort.Slice(result.Entries, func(i, j int) bool {
		return result.Entries[i].Path < result.Entries[j].Path
	})
	result.Markdown = renderMarkdown(result.Entries)

	return result, nil
}

func normalizePath(rootAbs, artifactPath string) (relPath, fullPath string, ok bool) {
	artifactPath = strings.TrimSpace(artifactPath)
	if artifactPath == "" {
		return "", "", false
	}

	fullPath = artifactPath
	if !filepath.IsAbs(fullPath) {
		fullPath = filepath.Join(rootAbs, fullPath)
	}

	fullPath = filepath.Clean(fullPath)

	rel, err := filepath.Rel(rootAbs, fullPath)
	if err != nil || rel == "." || strings.HasPrefix(rel, ".."+string(os.PathSeparator)) || rel == ".." || filepath.IsAbs(rel) {
		return "", "", false
	}

	return filepath.ToSlash(rel), fullPath, true
}

func resolveSafePath(rootReal, fullPath string) (resolvedPath string, safe bool, err error) {
	resolved, err := filepath.EvalSymlinks(fullPath)
	if err != nil {
		return "", false, fmt.Errorf("resolve symlink %s: %w", fullPath, err)
	}

	resolved = filepath.Clean(resolved)

	rel, err := filepath.Rel(rootReal, resolved)
	if err != nil {
		return "", false, fmt.Errorf("relative resolved path %s: %w", resolved, err)
	}

	if rel == "." || rel == ".." || filepath.IsAbs(rel) || strings.HasPrefix(rel, ".."+string(os.PathSeparator)) {
		return "", false, nil
	}

	return resolved, true, nil
}

func readTextFile(path string, maxBytes int64) (string, Warning, bool) {
	file, err := os.Open(path)
	if err != nil {
		return "", Warning{Reason: "read failed: " + err.Error()}, false
	}
	defer file.Close()

	info, err := file.Stat()
	if err != nil {
		return "", Warning{Reason: "stat failed: " + err.Error()}, false
	}

	if info.IsDir() {
		return "", Warning{Reason: "not a file"}, false
	}

	if info.Size() > maxBytes {
		return "", Warning{Reason: fmt.Sprintf("too large: %d bytes exceeds limit %d", info.Size(), maxBytes)}, false
	}

	data, err := io.ReadAll(io.LimitReader(file, maxBytes+1))
	if err != nil {
		return "", Warning{Reason: "read failed: " + err.Error()}, false
	}

	if int64(len(data)) > maxBytes {
		return "", Warning{Reason: fmt.Sprintf("too large: exceeds limit %d", maxBytes)}, false
	}

	if !utf8.Valid(data) || bytes.Contains(data, []byte{0}) {
		return "", Warning{Reason: "non-text artifact"}, false
	}

	return string(data), Warning{}, true
}

func renderMarkdown(entries []Entry) string {
	var b strings.Builder
	b.WriteString("# Merged Artifacts\n")

	if len(entries) == 0 {
		b.WriteString("\n_No text artifacts included._\n")
		return b.String()
	}

	for _, entry := range entries {
		fmt.Fprintf(&b, "\n## %s\n\n", entry.Path)
		writeMetadata(&b, "Path", entry.Path)
		writeMetadata(&b, "Kind", entry.Kind)
		writeMetadata(&b, "Source", entry.Source)
		writeMetadata(&b, "Summary", entry.Summary)
		b.WriteString("\n")

		fence := fenceFor(entry.Content)
		fmt.Fprintf(&b, "%stext\n%s", fence, entry.Content)

		if !strings.HasSuffix(entry.Content, "\n") {
			b.WriteString("\n")
		}

		fmt.Fprintf(&b, "%s\n", fence)
	}

	return b.String()
}

func writeMetadata(b *strings.Builder, key, value string) {
	value = oneLine(value)
	if value == "" {
		value = "-"
	}

	fmt.Fprintf(b, "- **%s:** %s\n", key, value)
}

func oneLine(value string) string {
	return strings.Join(strings.Fields(strings.TrimSpace(value)), " ")
}

func fenceFor(content string) string {
	longest := 0
	current := 0

	for _, r := range content {
		if r == '`' {
			current++
			if current > longest {
				longest = current
			}

			continue
		}

		current = 0
	}

	if longest < 3 {
		longest = 3
	} else {
		longest++
	}

	return strings.Repeat("`", longest)
}
