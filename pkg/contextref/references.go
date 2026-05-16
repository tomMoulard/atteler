package contextref

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"
)

const (
	// urlFetchTimeout is the maximum duration for fetching a single URL reference.
	urlFetchTimeout = 15 * time.Second
	// binaryProbeBytes is the number of bytes sampled to detect binary files.
	binaryProbeBytes = 512
)

// skipDirs is the set of directory names that are skipped when walking a
// directory reference. These are build artifacts, VCS metadata, and
// dependency caches that are almost never useful as style references.
var skipDirs = map[string]bool{
	".git":         true,
	"node_modules": true,
	"__pycache__":  true,
}

// LoadedReference describes one resolved reference entry with its content.
type LoadedReference struct {
	// Source is the original reference string (path or URL).
	Source string
	// Kind is "file" or "url".
	Kind string
	// Content is the resolved text content.
	Content string
	// Bytes is the byte length of Content.
	Bytes int
	// Truncated is true when the content was capped by size limits.
	Truncated bool
}

// LoadReferences resolves a list of reference strings (local file paths,
// directory paths, glob patterns, or HTTP/HTTPS URLs) and returns their
// content. Local relative paths are resolved against opts.Root but may also
// be absolute or escape the root (configured references are trusted).
// Glob patterns (containing *, ?, [, or **) are expanded before loading.
// Directories are walked recursively and each text file is returned as a
// separate LoadedReference. Each entry is subject to the per-file and
// aggregate byte limits from opts.
func LoadReferences(ctx context.Context, refs []string, opts Options) ([]LoadedReference, error) {
	if len(refs) == 0 {
		return nil, nil
	}

	opts = normalizeOptions(opts)

	var (
		out   []LoadedReference
		total int
	)

	for _, ref := range refs {
		ref = strings.TrimSpace(ref)
		if ref == "" {
			continue
		}

		if total >= opts.MaxTotalBytes {
			break
		}

		loaded, err := loadReference(ctx, ref, opts, total)
		if err != nil {
			return out, fmt.Errorf("reference %q: %w", ref, err)
		}

		for i := range loaded {
			total += loaded[i].Bytes
			out = append(out, loaded[i])
		}
	}

	return out, nil
}

// loadReference dispatches a single reference string to the appropriate
// loader: URL, glob pattern, directory, or plain file.
func loadReference(ctx context.Context, ref string, opts Options, total int) ([]LoadedReference, error) {
	if isURL(ref) {
		remaining := opts.MaxTotalBytes - total
		limit := min(opts.MaxFileBytes, remaining)

		loaded, err := loadURL(ctx, ref, limit)
		if err != nil {
			return nil, err
		}

		return []LoadedReference{loaded}, nil
	}

	if isGlob(ref) {
		return loadGlob(ref, opts, total)
	}

	resolved := resolvePath(ref, opts.Root)

	info, err := os.Stat(resolved)
	if err != nil {
		return nil, fmt.Errorf("stat: %w", err)
	}

	if info.IsDir() {
		return loadDirectory(resolved, ref, opts, total)
	}

	if !info.Mode().IsRegular() {
		return nil, fmt.Errorf("not a regular file: %s", ref)
	}

	remaining := opts.MaxTotalBytes - total
	limit := min(opts.MaxFileBytes, remaining)

	loaded, err := loadSingleFile(resolved, ref, limit)
	if err != nil {
		return nil, err
	}

	return []LoadedReference{loaded}, nil
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

// ---------------------------------------------------------------------------
// Path resolution (no root containment -- configured references are trusted)
// ---------------------------------------------------------------------------

// resolvePath resolves ref to an absolute filesystem path. Relative refs are
// joined against root. Absolute paths and paths outside root are permitted
// because configured references are explicitly set by the user.
func resolvePath(ref, root string) string {
	if filepath.IsAbs(ref) {
		return filepath.Clean(ref)
	}

	return filepath.Clean(filepath.Join(root, filepath.FromSlash(ref)))
}

// ---------------------------------------------------------------------------
// Glob expansion with ** (doublestar) support
// ---------------------------------------------------------------------------

// isGlob reports whether ref contains glob metacharacters.
func isGlob(ref string) bool {
	return strings.ContainsAny(ref, "*?[")
}

// loadGlob expands a glob pattern and loads every matching text file. The
// pattern may contain ** to match zero or more directory levels.
func loadGlob(pattern string, opts Options, total int) ([]LoadedReference, error) {
	base, _ := globBase(pattern)
	base = resolvePath(base, opts.Root)

	absPattern := resolvePath(pattern, opts.Root)

	matches, err := expandGlob(base, absPattern)
	if err != nil {
		return nil, fmt.Errorf("expand glob: %w", err)
	}

	var out []LoadedReference

	for _, path := range matches {
		remaining := opts.MaxTotalBytes - total
		if remaining <= 0 {
			break
		}

		limit := min(opts.MaxFileBytes, remaining)
		source := displaySource(path, opts.Root)

		loaded, loadErr := loadSingleFile(path, source, limit)
		if loadErr != nil {
			continue // skip unreadable files in a glob
		}

		total += loaded.Bytes
		out = append(out, loaded)
	}

	return out, nil
}

// globBase returns the longest non-glob prefix of a pattern and the remaining
// glob suffix. For example, "../repo/pkg/**/*.go" returns ("../repo/pkg", "**/*.go").
func globBase(pattern string) (base, rest string) {
	pattern = filepath.ToSlash(pattern)
	parts := strings.Split(pattern, "/")

	for i, part := range parts {
		if strings.ContainsAny(part, "*?[") {
			base = strings.Join(parts[:i], "/")
			rest = strings.Join(parts[i:], "/")

			if base == "" {
				base = "."
			}

			return base, rest
		}
	}

	return pattern, ""
}

// expandGlob walks base and returns every regular text file whose path matches
// absPattern. The pattern may use ** for recursive directory matching.
func expandGlob(base, absPattern string) ([]string, error) {
	absPattern = filepath.ToSlash(absPattern)

	var matches []string

	err := filepath.WalkDir(base, func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return nil //nolint:nilerr // skip inaccessible entries in glob expansion
		}

		if entry.IsDir() {
			if skipDirs[entry.Name()] {
				return filepath.SkipDir
			}

			return nil
		}

		if !entry.Type().IsRegular() {
			return nil
		}

		if matchGlob(absPattern, filepath.ToSlash(path)) {
			matches = append(matches, path)
		}

		return nil
	})
	if err != nil {
		return matches, fmt.Errorf("walk %s: %w", base, err)
	}

	return matches, nil
}

// matchGlob matches a path against a pattern that may contain ** segments.
// Both pattern and name must use forward slashes.
func matchGlob(pattern, name string) bool {
	patParts := strings.Split(pattern, "/")
	nameParts := strings.Split(name, "/")

	return matchParts(patParts, nameParts)
}

// matchParts recursively matches pattern segments against name segments.
// A "**" segment matches zero or more name segments.
func matchParts(patParts, nameParts []string) bool {
	for len(patParts) > 0 && len(nameParts) > 0 {
		if patParts[0] == "**" {
			return matchDoublestar(patParts[1:], nameParts)
		}

		matched, err := filepath.Match(patParts[0], nameParts[0])
		if err != nil || !matched {
			return false
		}

		patParts = patParts[1:]
		nameParts = nameParts[1:]
	}

	// Consume any trailing ** in pattern.
	for len(patParts) > 0 && patParts[0] == "**" {
		patParts = patParts[1:]
	}

	return len(patParts) == 0 && len(nameParts) == 0
}

// matchDoublestar handles a ** segment: the remaining pattern (after **)
// is tried against every suffix of nameParts.
func matchDoublestar(patAfter, nameParts []string) bool {
	if len(patAfter) == 0 {
		return true // trailing ** matches everything
	}

	for skip := 0; skip <= len(nameParts); skip++ {
		if matchParts(patAfter, nameParts[skip:]) {
			return true
		}
	}

	return false
}

// ---------------------------------------------------------------------------
// Directory loading (reads file contents, not just a tree listing)
// ---------------------------------------------------------------------------

// loadDirectory walks dir recursively and returns one LoadedReference per text
// file. Binary files and common non-source directories (.git, node_modules,
// __pycache__) are skipped. The source field uses ref as a prefix so the LLM
// sees the original reference path the user configured.
func loadDirectory(dir, ref string, opts Options, total int) ([]LoadedReference, error) {
	var out []LoadedReference

	err := filepath.WalkDir(dir, func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return nil //nolint:nilerr // skip inaccessible entries in directory walk
		}

		if entry.IsDir() {
			if skipDirs[entry.Name()] {
				return filepath.SkipDir
			}

			return nil
		}

		if !entry.Type().IsRegular() {
			return nil
		}

		remaining := opts.MaxTotalBytes - total
		if remaining <= 0 {
			return fs.SkipAll
		}

		limit := min(opts.MaxFileBytes, remaining)

		rel, relErr := filepath.Rel(dir, path)
		if relErr != nil {
			return nil //nolint:nilerr // skip files with unresolvable relative paths
		}

		source := filepath.ToSlash(filepath.Join(ref, rel))

		loaded, loadErr := loadSingleFile(path, source, limit)
		if loadErr != nil {
			return nil //nolint:nilerr // skip unreadable / binary files
		}

		total += loaded.Bytes
		out = append(out, loaded)

		return nil
	})
	if err != nil {
		return out, fmt.Errorf("walk %s: %w", dir, err)
	}

	return out, nil
}

// ---------------------------------------------------------------------------
// Single-file loading with binary detection
// ---------------------------------------------------------------------------

// loadSingleFile reads a single file up to limit bytes and returns it as a
// LoadedReference. Binary files (detected by null bytes in the first 512
// bytes) are rejected.
func loadSingleFile(path, source string, limit int) (LoadedReference, error) {
	content, truncated, err := readLimited(path, limit)
	if err != nil {
		return LoadedReference{}, fmt.Errorf("read file: %w", err)
	}

	if isBinary(content) {
		return LoadedReference{}, fmt.Errorf("binary file: %s", source)
	}

	return LoadedReference{
		Source:    source,
		Kind:      kindFile,
		Content:   string(content),
		Bytes:     len(content),
		Truncated: truncated,
	}, nil
}

// isBinary returns true if data looks like a binary file. It checks the first
// binaryProbeBytes for a null byte, which is the same heuristic git uses.
func isBinary(data []byte) bool {
	probe := data
	if len(probe) > binaryProbeBytes {
		probe = probe[:binaryProbeBytes]
	}

	return bytes.ContainsRune(probe, 0)
}

// ---------------------------------------------------------------------------
// URL loading
// ---------------------------------------------------------------------------

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

// ---------------------------------------------------------------------------
// Helpers
// ---------------------------------------------------------------------------

// displaySource builds a human-readable source label for a matched file.
// When the matched path starts inside root, it is shown relative to root.
// Otherwise it is shown as an absolute path.
func displaySource(path, root string) string {
	rel, err := filepath.Rel(root, path)
	if err != nil {
		return path
	}

	return filepath.ToSlash(rel)
}
