package contextref

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// urlFetchTimeout is the maximum duration for fetching a single URL reference.
const urlFetchTimeout = 15 * time.Second

// LoadedReference describes one resolved reference entry with its content.
type LoadedReference struct {
	// Source is the original reference string (path or URL).
	Source string
	// Kind is "file", "directory", or "url".
	Kind string
	// Content is the resolved text content.
	Content string
	// Bytes is the byte length of Content.
	Bytes int
	// Truncated is true when the content was capped by size limits.
	Truncated bool
}

// LoadReferences resolves a list of reference strings (local paths or HTTP/HTTPS
// URLs) and returns their content. Local paths are resolved relative to root and
// must not escape it. Each entry is subject to the per-file and aggregate byte
// limits from opts.
func LoadReferences(ctx context.Context, refs []string, opts Options) ([]LoadedReference, error) {
	if len(refs) == 0 {
		return nil, nil
	}

	opts = normalizeOptions(opts)

	var out []LoadedReference

	total := 0

	for _, ref := range refs {
		ref = strings.TrimSpace(ref)
		if ref == "" {
			continue
		}

		remaining := opts.MaxTotalBytes - total
		if remaining <= 0 {
			break
		}

		limit := min(opts.MaxFileBytes, remaining)

		var (
			loaded LoadedReference
			err    error
		)

		if isURL(ref) {
			loaded, err = loadURL(ctx, ref, limit)
		} else {
			loaded, err = loadPath(ref, opts.Root, limit)
		}

		if err != nil {
			return out, fmt.Errorf("reference %q: %w", ref, err)
		}

		total += loaded.Bytes
		out = append(out, loaded)
	}

	return out, nil
}

// FormatReferences renders loaded references as an XML block suitable for
// prepending to a system prompt or appending to a user message.
func FormatReferences(refs []LoadedReference) string {
	if len(refs) == 0 {
		return ""
	}

	var b strings.Builder

	b.WriteString("<configured_references>\n")

	for _, ref := range refs {
		tag := ref.Kind
		if tag == "" {
			tag = kindFile
		}

		b.WriteString(`<`)
		b.WriteString(tag)
		b.WriteString(` source="`)
		b.WriteString(escapeAttr(ref.Source))
		b.WriteString(`" truncated="`)

		if ref.Truncated {
			b.WriteString("true")
		} else {
			b.WriteString("false")
		}

		b.WriteString("\">\n")
		b.WriteString(ref.Content)

		if !strings.HasSuffix(ref.Content, "\n") {
			b.WriteString("\n")
		}

		b.WriteString("</")
		b.WriteString(tag)
		b.WriteString(">\n")
	}

	b.WriteString("</configured_references>")

	return b.String()
}

func isURL(ref string) bool {
	return strings.HasPrefix(ref, "http://") || strings.HasPrefix(ref, "https://")
}

func loadURL(ctx context.Context, rawURL string, limit int) (LoadedReference, error) {
	ctx, cancel := context.WithTimeout(ctx, urlFetchTimeout)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, http.NoBody)
	if err != nil {
		return LoadedReference{}, fmt.Errorf("build request: %w", err)
	}

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return LoadedReference{}, fmt.Errorf("fetch: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return LoadedReference{}, fmt.Errorf("HTTP %d", resp.StatusCode)
	}

	data, err := io.ReadAll(io.LimitReader(resp.Body, int64(limit+1)))
	if err != nil {
		return LoadedReference{}, fmt.Errorf("read body: %w", err)
	}

	truncated := len(data) > limit
	if truncated {
		data = data[:limit]
	}

	return LoadedReference{
		Source:    rawURL,
		Kind:      "url",
		Content:   string(data),
		Bytes:     len(data),
		Truncated: truncated,
	}, nil
}

func loadPath(ref, root string, limit int) (LoadedReference, error) {
	realPath, err := resolveContainedPath(ref, root)
	if err != nil {
		return LoadedReference{}, err
	}

	info, err := os.Stat(realPath)
	if err != nil {
		return LoadedReference{}, fmt.Errorf("stat: %w", err)
	}

	if info.IsDir() {
		content, truncated, dirErr := directoryTree(realPath, limit)
		if dirErr != nil {
			return LoadedReference{}, fmt.Errorf("list directory: %w", dirErr)
		}

		return LoadedReference{
			Source:    ref,
			Kind:      "directory",
			Content:   string(content),
			Bytes:     len(content),
			Truncated: truncated,
		}, nil
	}

	if !info.Mode().IsRegular() {
		return LoadedReference{}, fmt.Errorf("not a regular file: %s", ref)
	}

	content, truncated, err := readLimited(realPath, limit)
	if err != nil {
		return LoadedReference{}, fmt.Errorf("read file: %w", err)
	}

	return LoadedReference{
		Source:    ref,
		Kind:      kindFile,
		Content:   string(content),
		Bytes:     len(content),
		Truncated: truncated,
	}, nil
}

// resolveContainedPath resolves ref relative to root, validates that it stays
// within root (including after symlink resolution), and returns the real
// filesystem path.
func resolveContainedPath(ref, root string) (string, error) {
	absRoot, err := filepath.Abs(root)
	if err != nil {
		return "", fmt.Errorf("resolve root: %w", err)
	}

	path := ref
	if !filepath.IsAbs(path) {
		path = filepath.Join(absRoot, filepath.FromSlash(ref))
	}

	path, err = filepath.Abs(filepath.Clean(path))
	if err != nil {
		return "", fmt.Errorf("resolve path: %w", err)
	}

	// Ensure the path stays within root.
	rel, err := filepath.Rel(absRoot, path)
	if err != nil {
		return "", fmt.Errorf("resolve relative path: %w", err)
	}

	if rel == ".." || strings.HasPrefix(rel, ".."+string(os.PathSeparator)) {
		return "", fmt.Errorf("path escapes root %s", absRoot)
	}

	// Follow symlinks and re-verify containment.
	realRoot, err := filepath.EvalSymlinks(absRoot)
	if err != nil {
		return "", fmt.Errorf("resolve root symlinks: %w", err)
	}

	realPath, err := filepath.EvalSymlinks(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return "", fmt.Errorf("path does not exist: %s", ref)
		}

		return "", fmt.Errorf("resolve symlinks: %w", err)
	}

	rel, err = filepath.Rel(realRoot, realPath)
	if err != nil {
		return "", fmt.Errorf("resolve relative path after symlink: %w", err)
	}

	if rel == ".." || strings.HasPrefix(rel, ".."+string(os.PathSeparator)) {
		return "", errors.New("path escapes root after symlink resolution")
	}

	return realPath, nil
}
