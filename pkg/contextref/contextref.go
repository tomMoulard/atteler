// Package contextref expands local @file prompt references into LLM context.
package contextref

import (
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"unicode"
)

const (
	// DefaultMaxFileBytes is the default per-file reference cap.
	DefaultMaxFileBytes = 64 * 1024
	// DefaultMaxTotalBytes is the default aggregate reference cap.
	DefaultMaxTotalBytes = 256 * 1024
	maxDirectoryEntries  = 200

	// kindFile is the reference kind for regular files.
	kindFile = "file"
)

var errDirectoryLimit = errors.New("directory reference limit reached")

// Options configures reference expansion.
//
//nolint:govet // field order keeps root/size options first for callers.
type Options struct {
	Root          string
	MaxFileBytes  int
	MaxTotalBytes int
	// ReferencePolicy constrains configured reference ingestion. It is used by
	// LoadReferences; inline @path expansion remains rooted under Root.
	ReferencePolicy ReferencePolicy
	// ReferenceScope records where configured references came from, for example
	// "global" or "agent:reviewer".
	ReferenceScope string
}

// Reference describes one expanded local file or directory tree.
type Reference struct {
	Path      string
	Kind      string
	Bytes     int
	Truncated bool
}

// Result is the expanded prompt and reference metadata.
type Result struct {
	Prompt     string
	References []Reference
}

// Expand appends referenced local file contents or directory trees to prompt.
// References are written as @path tokens and must resolve under Options.Root.
func Expand(prompt string, opts Options) (Result, error) {
	opts = normalizeOptions(opts)

	candidates := parseCandidates(prompt)
	if len(candidates) == 0 {
		return Result{Prompt: prompt}, nil
	}

	seen := make(map[string]bool, len(candidates))

	var refs []expandedReference

	total := 0
	for _, candidate := range candidates {
		ref, nextTotal, ok, err := expandCandidate(candidate, opts, total, seen)
		if err != nil {
			return Result{}, err
		}

		if !ok {
			continue
		}

		total = nextTotal

		refs = append(refs, ref)
	}

	if len(refs) == 0 {
		return Result{Prompt: prompt}, nil
	}

	return Result{
		Prompt:     appendReferences(prompt, refs),
		References: references(refs),
	}, nil
}

type expandedReference struct {
	content   string
	Path      string
	Kind      string
	Bytes     int
	Truncated bool
}

func (r expandedReference) Reference() Reference {
	return Reference{
		Path:      r.Path,
		Kind:      r.Kind,
		Bytes:     r.Bytes,
		Truncated: r.Truncated,
	}
}

func normalizeOptions(opts Options) Options {
	if opts.Root == "" {
		if cwd, err := os.Getwd(); err == nil {
			opts.Root = cwd
		}
	}

	if opts.MaxFileBytes <= 0 {
		opts.MaxFileBytes = DefaultMaxFileBytes
	}

	if opts.MaxTotalBytes <= 0 {
		opts.MaxTotalBytes = DefaultMaxTotalBytes
	}

	return opts
}

func parseCandidates(prompt string) []string {
	var out []string

	for i := 0; i < len(prompt); i++ {
		if prompt[i] != '@' {
			continue
		}

		if i > 0 && isWord(rune(prompt[i-1])) {
			continue
		}

		j := i + 1
		for j < len(prompt) && isPathRune(rune(prompt[j])) {
			j++
		}

		if j == i+1 {
			continue
		}

		candidate := strings.Trim(prompt[i+1:j], ".,;:!?)]}")
		if candidate != "" {
			out = append(out, candidate)
		}

		i = j - 1
	}

	return out
}

func isPathRune(r rune) bool {
	return unicode.IsLetter(r) ||
		unicode.IsDigit(r) ||
		r == '/' ||
		r == '\\' ||
		r == '.' ||
		r == '_' ||
		r == '-'
}

func isWord(r rune) bool {
	return unicode.IsLetter(r) || unicode.IsDigit(r) || r == '_'
}

func pathLike(candidate string) bool {
	return strings.ContainsAny(candidate, `/\.`)
}

func expandCandidate(
	candidate string,
	opts Options,
	total int,
	seen map[string]bool,
) (ref expandedReference, nextTotal int, ok bool, err error) {
	resolved, displayPath, err := resolve(opts.Root, candidate)
	if err != nil {
		return expandedReference{}, total, false, err
	}

	info, err := os.Stat(resolved)
	if err != nil {
		if !pathLike(candidate) && errors.Is(err, os.ErrNotExist) {
			return expandedReference{}, total, false, nil
		}

		return expandedReference{}, total, false, fmt.Errorf("context: stat @%s: %w", candidate, err)
	}

	resolved, err = resolveSymlinksInsideRoot(opts.Root, resolved, candidate)
	if err != nil {
		return expandedReference{}, total, false, err
	}

	if seen[resolved] {
		return expandedReference{}, total, false, nil
	}

	seen[resolved] = true

	if info.IsDir() {
		remaining := opts.MaxTotalBytes - total
		if remaining <= 0 {
			return expandedReference{}, total, false, errors.New("context: max_total_bytes exceeded")
		}

		limit := min(opts.MaxFileBytes, remaining)

		content, truncated, treeErr := directoryTree(resolved, limit)
		if treeErr != nil {
			return expandedReference{}, total, false, fmt.Errorf("context: list @%s: %w", candidate, treeErr)
		}

		return expandedReference{
			Path:      displayPath,
			Kind:      "directory",
			Bytes:     len(content),
			Truncated: truncated,
			content:   string(content),
		}, total + len(content), true, nil
	}

	if !info.Mode().IsRegular() {
		return expandedReference{}, total, false, fmt.Errorf("context: @%s is not a regular file", candidate)
	}

	remaining := opts.MaxTotalBytes - total
	if remaining <= 0 {
		return expandedReference{}, total, false, errors.New("context: max_total_bytes exceeded")
	}

	limit := min(opts.MaxFileBytes, remaining)

	content, truncated, err := readLimited(resolved, limit)
	if err != nil {
		return expandedReference{}, total, false, fmt.Errorf("context: read @%s: %w", candidate, err)
	}

	return expandedReference{
		Path:      displayPath,
		Kind:      kindFile,
		Bytes:     len(content),
		Truncated: truncated,
		content:   string(content),
	}, total + len(content), true, nil
}

func resolve(root, candidate string) (resolved, displayPath string, err error) {
	root, err = filepath.Abs(root)
	if err != nil {
		return "", "", fmt.Errorf("context: resolve root: %w", err)
	}

	path := candidate
	if !filepath.IsAbs(path) {
		path = filepath.Join(root, filepath.FromSlash(candidate))
	}

	path, err = filepath.Abs(filepath.Clean(path))
	if err != nil {
		return "", "", fmt.Errorf("context: resolve @%s: %w", candidate, err)
	}

	rel, err := filepath.Rel(root, path)
	if err != nil {
		return "", "", fmt.Errorf("context: resolve @%s: %w", candidate, err)
	}

	if rel == ".." || strings.HasPrefix(rel, ".."+string(os.PathSeparator)) {
		return "", "", fmt.Errorf("context: @%s escapes root %s", candidate, root)
	}

	return path, filepath.ToSlash(rel), nil
}

func resolveSymlinksInsideRoot(root, path, candidate string) (string, error) {
	root, err := filepath.Abs(root)
	if err != nil {
		return "", fmt.Errorf("context: resolve root: %w", err)
	}

	root, err = filepath.EvalSymlinks(root)
	if err != nil {
		return "", fmt.Errorf("context: resolve root symlinks: %w", err)
	}

	resolved, err := filepath.EvalSymlinks(path)
	if err != nil {
		return "", fmt.Errorf("context: resolve @%s symlinks: %w", candidate, err)
	}

	rel, err := filepath.Rel(root, resolved)
	if err != nil {
		return "", fmt.Errorf("context: resolve @%s: %w", candidate, err)
	}

	if rel == ".." || strings.HasPrefix(rel, ".."+string(os.PathSeparator)) {
		return "", fmt.Errorf("context: @%s escapes root %s", candidate, root)
	}

	return resolved, nil
}

func directoryTree(root string, limit int) (data []byte, truncated bool, err error) {
	if limit <= 0 {
		return nil, false, nil
	}

	var b strings.Builder

	entries := 0

	err = filepath.WalkDir(root, func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}

		if path == root {
			return nil
		}

		if entry.IsDir() && entry.Name() == ".git" {
			return filepath.SkipDir
		}

		if entries >= maxDirectoryEntries {
			truncated = true
			return errDirectoryLimit
		}

		rel, relErr := filepath.Rel(root, path)
		if relErr != nil {
			return fmt.Errorf("rel: %w", relErr)
		}

		line := filepath.ToSlash(rel)
		if entry.IsDir() {
			line += "/"
		}

		line += "\n"
		if b.Len()+len(line) > limit {
			truncated = true
			return errDirectoryLimit
		}

		b.WriteString(line)

		entries++

		return nil
	})
	if errors.Is(err, errDirectoryLimit) {
		err = nil
	}

	return []byte(b.String()), truncated, err
}

func readLimited(path string, limit int) (data []byte, truncated bool, err error) {
	if limit <= 0 {
		return nil, false, nil
	}

	file, err := os.Open(path)
	if err != nil {
		return nil, false, fmt.Errorf("open: %w", err)
	}
	defer file.Close()

	data, err = io.ReadAll(io.LimitReader(file, int64(limit+1)))
	if err != nil {
		return nil, false, fmt.Errorf("read: %w", err)
	}

	if len(data) > limit {
		return data[:limit], true, nil
	}

	return data, false, nil
}

func appendReferences(prompt string, refs []expandedReference) string {
	var b strings.Builder
	b.WriteString(prompt)
	b.WriteString("\n\n<context_references>\n")

	for _, ref := range refs {
		tag := ref.Kind
		if tag == "" {
			tag = kindFile
		}

		b.WriteString(`<`)
		b.WriteString(tag)
		b.WriteString(` path="`)
		b.WriteString(escapeAttr(ref.Path))
		b.WriteString(`" truncated="`)
		b.WriteString(strconv.FormatBool(ref.Truncated))
		b.WriteString("\">\n")
		b.WriteString(escapeText(ref.content))

		if !strings.HasSuffix(ref.content, "\n") {
			b.WriteString("\n")
		}

		b.WriteString("</")
		b.WriteString(tag)
		b.WriteString(">\n")
	}

	b.WriteString("</context_references>")

	return b.String()
}

func escapeAttr(value string) string {
	value = strings.ReplaceAll(value, "&", "&amp;")
	value = strings.ReplaceAll(value, `"`, "&quot;")
	value = strings.ReplaceAll(value, "<", "&lt;")

	return value
}

func references(refs []expandedReference) []Reference {
	out := make([]Reference, 0, len(refs))
	for _, ref := range refs {
		out = append(out, ref.Reference())
	}

	return out
}
