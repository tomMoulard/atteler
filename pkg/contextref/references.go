package contextref

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"mime"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

const (
	// urlFetchTimeout is the maximum duration for fetching a single URL reference.
	urlFetchTimeout = 15 * time.Second
	// binaryProbeBytes is the number of bytes sampled to detect binary files.
	binaryProbeBytes = 512

	kindURL = "url"

	referenceLocationLocal  = "local"
	referenceLocationRemote = "remote"

	// ReferenceDecisionLoaded means a reference was loaded without truncation.
	ReferenceDecisionLoaded = "loaded"
	// ReferenceDecisionTruncated means a reference was loaded but capped by byte limits.
	ReferenceDecisionTruncated = "truncated"
	// ReferenceDecisionSkipped means a non-fatal entry was ignored, such as a binary file in a directory.
	ReferenceDecisionSkipped = "skipped"
	// ReferenceDecisionRejected means policy or loading rejected a configured reference.
	ReferenceDecisionRejected = "rejected"

	// ReferenceScopeGlobal identifies references from context.references.
	ReferenceScopeGlobal = "global"
	// ReferenceScopeAgent identifies references configured on an agent.
	ReferenceScopeAgent = "agent"
	// ReferenceScopeReview identifies ad-hoc review references.
	ReferenceScopeReview = "review"
)

var defaultAllowedContentTypes = []string{
	"text/*",
	"application/json",
	"application/xml",
	"application/x-yaml",
	"application/yaml",
	"application/toml",
}

// skipDirs is the set of directory names that are skipped when walking a
// directory reference. These are build artifacts, VCS metadata, and
// dependency caches that are almost never useful as style references.
var skipDirs = map[string]bool{
	".git":         true,
	"node_modules": true,
	"__pycache__":  true,
}

// ReferencePolicy defines the trust boundary for configured references.
// Local references are limited to Options.Root plus LocalRoots. Remote
// references must match AllowedSchemes and AllowedHosts, must not resolve to a
// private/local network unless AllowPrivateNetworks is true, must satisfy the
// redirect limit, and must return an allowed Content-Type.
type ReferencePolicy struct {
	AllowedSchemes       []string
	AllowedHosts         []string
	LocalRoots           []string
	ContentTypes         []string
	MaxRedirects         int
	AllowPrivateNetworks bool
}

// ReferenceProvenance records how a loaded reference crossed the configured
// reference trust boundary.
//
//nolint:govet // field order follows the CLI/provenance report order.
type ReferenceProvenance struct {
	Scope          string
	Location       string
	Size           int
	Truncated      bool
	DigestSHA256   string
	FetchedAt      time.Time
	PolicyDecision string
	PolicyReason   string
}

// ReferenceEvent describes one attempted reference ingestion decision. CLI
// callers should surface these events so policy rejections and skipped files are
// visible instead of silently changing the context supplied to the model.
//
//nolint:govet // field order follows the CLI/provenance report order.
type ReferenceEvent struct {
	Source         string
	Kind           string
	Scope          string
	Location       string
	Bytes          int
	Truncated      bool
	DigestSHA256   string
	FetchedAt      time.Time
	PolicyDecision string
	PolicyReason   string
}

// LoadedReference describes one resolved reference entry with its content.
//
//nolint:govet // field order preserves the historical API shape first.
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
	// Provenance records policy and source metadata for this reference.
	Provenance ReferenceProvenance
}

type normalizedReferencePolicy struct {
	allowedSchemes       map[string]bool
	allowedHosts         []string
	localRoots           []string
	contentTypes         []string
	maxRedirects         int
	allowPrivateNetworks bool
}

// LoadReferences resolves a list of reference strings (local file paths,
// directory paths, glob patterns, or HTTP/HTTPS URLs) and returns their
// content. Local references must stay under opts.Root or one of the explicit
// policy local roots. Remote references are denied unless the reference policy
// explicitly allows their scheme and host, and private-network targets remain
// blocked unless the policy opts in.
//
// Glob patterns (containing *, ?, [, or **) are expanded before loading.
// Directories are walked recursively and each text file is returned as a
// separate LoadedReference. Each entry is subject to the per-file and aggregate
// byte limits from opts.
func LoadReferences(ctx context.Context, refs []string, opts Options) ([]LoadedReference, error) {
	loaded, _, err := LoadReferencesWithReport(ctx, refs, opts)
	return loaded, err
}

// LoadReferencesWithReport is LoadReferences plus per-reference policy and
// loading events suitable for CLI diagnostics.
func LoadReferencesWithReport(ctx context.Context, refs []string, opts Options) ([]LoadedReference, []ReferenceEvent, error) {
	if err := requireReferenceContext(ctx); err != nil {
		return nil, nil, err
	}

	if len(refs) == 0 {
		return nil, nil, nil
	}

	opts = normalizeOptions(opts)

	policy, err := normalizeReferencePolicy(opts)
	if err != nil {
		return nil, nil, err
	}

	if len(policy.localRoots) > 0 {
		opts.Root = policy.localRoots[0]
	}

	var (
		out    []LoadedReference
		events []ReferenceEvent
		errs   []error
		total  int
	)

	for _, raw := range refs {
		if err := requireReferenceContext(ctx); err != nil {
			return out, events, errors.Join(append(errs, err)...)
		}

		ref := strings.TrimSpace(raw)
		if ref == "" {
			events = append(events, newReferenceEvent(raw, "", "", opts, ReferenceDecisionSkipped, "empty reference"))
			continue
		}

		if total >= opts.MaxTotalBytes {
			events = append(events, newReferenceEvent(ref, "", "", opts, ReferenceDecisionSkipped, "max_total_bytes already reached"))
			continue
		}

		loaded, loadEvents, loadErr := loadReference(ctx, ref, opts, policy, total)

		events = append(events, loadEvents...)
		if loadErr != nil {
			errs = append(errs, fmt.Errorf("reference %q: %w", ref, loadErr))
			continue
		}

		for i := range loaded {
			total += loaded[i].Bytes
			out = append(out, loaded[i])
		}
	}

	return out, events, errors.Join(errs...)
}

func requireReferenceContext(ctx context.Context) error {
	if ctx == nil {
		return errors.New("context references: context is required")
	}

	if err := ctx.Err(); err != nil {
		return fmt.Errorf("context references: context already done: %w", err)
	}

	return nil
}

// loadReference dispatches a single reference string to the appropriate
// loader: URL, glob pattern, directory, or plain file.
func loadReference(
	ctx context.Context,
	ref string,
	opts Options,
	policy normalizedReferencePolicy,
	total int,
) ([]LoadedReference, []ReferenceEvent, error) {
	if isURL(ref) {
		remaining := opts.MaxTotalBytes - total
		limit := min(opts.MaxFileBytes, remaining)

		loaded, event, err := loadURL(ctx, ref, limit, opts, policy)

		return singletonLoaded(loaded, event, err)
	}

	if isGlob(ref) {
		return loadGlob(ref, opts, policy, total)
	}

	resolved, err := resolvePolicyPath(ref, opts.Root, policy)
	if err != nil {
		event := newReferenceEvent(ref, kindFile, referenceLocationLocal, opts, ReferenceDecisionRejected, err.Error())
		return nil, []ReferenceEvent{event}, err
	}

	info, err := os.Stat(resolved)
	if err != nil {
		reason := fmt.Sprintf("stat: %v", err)
		event := newReferenceEvent(ref, kindFile, referenceLocationLocal, opts, ReferenceDecisionRejected, reason)

		return nil, []ReferenceEvent{event}, fmt.Errorf("stat: %w", err)
	}

	resolved, err = resolvePolicySymlinks(resolved, ref, policy)
	if err != nil {
		event := newReferenceEvent(ref, kindFile, referenceLocationLocal, opts, ReferenceDecisionRejected, err.Error())
		return nil, []ReferenceEvent{event}, err
	}

	if info.IsDir() {
		return loadDirectory(resolved, ref, opts, policy, total)
	}

	if !info.Mode().IsRegular() {
		reason := "not a regular file: " + ref
		event := newReferenceEvent(ref, kindFile, referenceLocationLocal, opts, ReferenceDecisionRejected, reason)

		return nil, []ReferenceEvent{event}, fmt.Errorf("not a regular file: %s", ref)
	}

	remaining := opts.MaxTotalBytes - total
	limit := min(opts.MaxFileBytes, remaining)

	loaded, event, err := loadSingleFile(resolved, ref, limit, opts)

	return singletonLoaded(loaded, event, err)
}

func singletonLoaded(loaded LoadedReference, event ReferenceEvent, err error) ([]LoadedReference, []ReferenceEvent, error) {
	if err != nil {
		return nil, []ReferenceEvent{event}, err
	}

	return []LoadedReference{loaded}, []ReferenceEvent{event}, nil
}

// FormatReferences renders loaded references as an XML-ish block suitable for
// prepending to a system prompt or appending to a user message. Reference text
// is XML-escaped so content cannot close tags or forge sibling prompt sections.
func FormatReferences(refs []LoadedReference) string {
	if len(refs) == 0 {
		return ""
	}

	var b strings.Builder

	b.WriteString("<configured_references>\n")

	for i := range refs {
		ref := refs[i]
		tag := safeReferenceTag(ref.Kind)
		prov := provenanceForFormat(ref)

		b.WriteString(`<`)
		b.WriteString(tag)
		b.WriteString(` source="`)
		b.WriteString(escapeAttr(ref.Source))
		b.WriteString(`" truncated="`)
		b.WriteString(strconv.FormatBool(ref.Truncated))
		b.WriteString(`" scope="`)
		b.WriteString(escapeAttr(prov.Scope))
		b.WriteString(`" location="`)
		b.WriteString(escapeAttr(prov.Location))
		b.WriteString(`" bytes="`)
		b.WriteString(strconv.Itoa(prov.Size))
		b.WriteString(`" digest_sha256="`)
		b.WriteString(escapeAttr(prov.DigestSHA256))
		b.WriteString(`" policy_decision="`)
		b.WriteString(escapeAttr(prov.PolicyDecision))
		b.WriteString(`" policy_reason="`)
		b.WriteString(escapeAttr(prov.PolicyReason))

		if !prov.FetchedAt.IsZero() {
			b.WriteString(`" fetched_at="`)
			b.WriteString(escapeAttr(prov.FetchedAt.UTC().Format(time.RFC3339)))
		}

		b.WriteString("\">\n")
		b.WriteString(escapeText(ref.Content))

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
// Reference policy
// ---------------------------------------------------------------------------

func normalizeReferencePolicy(opts Options) (normalizedReferencePolicy, error) {
	root, err := cleanAbsForPolicy(opts.Root)
	if err != nil {
		return normalizedReferencePolicy{}, fmt.Errorf("reference policy: root: %w", err)
	}

	roots := []string{root}

	for _, configuredRoot := range opts.ReferencePolicy.LocalRoots {
		configuredRoot = strings.TrimSpace(configuredRoot)
		if configuredRoot == "" {
			continue
		}

		path := configuredRoot
		if !filepath.IsAbs(path) {
			path = filepath.Join(root, filepath.FromSlash(path))
		}

		absRoot, rootErr := cleanAbsForPolicy(path)
		if rootErr != nil {
			return normalizedReferencePolicy{}, fmt.Errorf("reference policy: local root %q: %w", configuredRoot, rootErr)
		}

		roots = append(roots, absRoot)
	}

	allowedSchemes := opts.ReferencePolicy.AllowedSchemes
	if len(allowedSchemes) == 0 {
		allowedSchemes = []string{"https"}
	}

	schemes := make(map[string]bool, len(allowedSchemes))
	for _, scheme := range allowedSchemes {
		scheme = strings.ToLower(strings.TrimSpace(scheme))
		if scheme != "" {
			schemes[scheme] = true
		}
	}

	contentTypes := opts.ReferencePolicy.ContentTypes
	if len(contentTypes) == 0 {
		contentTypes = defaultAllowedContentTypes
	}

	return normalizedReferencePolicy{
		allowedSchemes:       schemes,
		allowedHosts:         cleanList(opts.ReferencePolicy.AllowedHosts),
		localRoots:           roots,
		maxRedirects:         max(0, opts.ReferencePolicy.MaxRedirects),
		contentTypes:         cleanList(contentTypes),
		allowPrivateNetworks: opts.ReferencePolicy.AllowPrivateNetworks,
	}, nil
}

func cleanList(values []string) []string {
	out := make([]string, 0, len(values))
	for _, value := range values {
		value = strings.TrimSpace(value)
		if value != "" {
			out = append(out, value)
		}
	}

	return out
}

func cleanAbs(path string) (string, error) {
	abs, err := filepath.Abs(filepath.Clean(path))
	if err != nil {
		return "", fmt.Errorf("abs: %w", err)
	}

	return abs, nil
}

func cleanAbsForPolicy(path string) (string, error) {
	abs, err := cleanAbs(path)
	if err != nil {
		return "", err
	}

	evaluated, err := filepath.EvalSymlinks(abs)
	if err != nil {
		if !errors.Is(err, os.ErrNotExist) {
			return "", fmt.Errorf("eval symlinks: %w", err)
		}

		return abs, nil
	}

	return cleanAbs(evaluated)
}

func resolvePolicyPath(ref, root string, policy normalizedReferencePolicy) (string, error) {
	resolved := resolvePath(ref, root)

	resolved, err := cleanAbs(resolved)
	if err != nil {
		return "", fmt.Errorf("resolve path: %w", err)
	}

	containmentPath, err := cleanAbsForPolicy(resolved)
	if err != nil {
		return "", fmt.Errorf("resolve path: %w", err)
	}

	if !pathInsideAnyRoot(containmentPath, policy.localRoots) {
		return "", fmt.Errorf("path %q is outside allowed local roots", ref)
	}

	return resolved, nil
}

func resolvePolicySymlinks(path, ref string, policy normalizedReferencePolicy) (string, error) {
	resolved, err := filepath.EvalSymlinks(path)
	if err != nil {
		return "", fmt.Errorf("resolve %q symlinks: %w", ref, err)
	}

	resolved, err = cleanAbs(resolved)
	if err != nil {
		return "", fmt.Errorf("resolve %q symlinks: %w", ref, err)
	}

	if !pathInsideAnyRoot(resolved, policy.localRoots) {
		return "", fmt.Errorf("path %q symlink target is outside allowed local roots", ref)
	}

	return resolved, nil
}

func pathInsideAnyRoot(path string, roots []string) bool {
	for _, root := range roots {
		if pathInsideRoot(path, root) {
			return true
		}
	}

	return false
}

func pathInsideRoot(path, root string) bool {
	rel, err := filepath.Rel(root, path)
	if err != nil {
		return false
	}

	return rel == "." || (rel != ".." && !strings.HasPrefix(rel, ".."+string(os.PathSeparator)))
}

// ---------------------------------------------------------------------------
// Path resolution
// ---------------------------------------------------------------------------

// resolvePath resolves ref to an absolute filesystem path. Relative refs are
// joined against root. Callers must apply reference policy before reading.
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
func loadGlob(pattern string, opts Options, policy normalizedReferencePolicy, total int) ([]LoadedReference, []ReferenceEvent, error) {
	base, _ := globBase(pattern)
	base = resolvePath(base, opts.Root)

	absBase, err := cleanAbsForPolicy(base)
	if err != nil {
		event := newReferenceEvent(pattern, kindFile, referenceLocationLocal, opts, ReferenceDecisionRejected, err.Error())

		return nil, []ReferenceEvent{event}, err
	}

	if !pathInsideAnyRoot(absBase, policy.localRoots) {
		reason := fmt.Sprintf("glob base %q is outside allowed local roots", pattern)
		event := newReferenceEvent(pattern, kindFile, referenceLocationLocal, opts, ReferenceDecisionRejected, reason)

		return nil, []ReferenceEvent{event}, errors.New(reason)
	}

	absPattern := resolvePath(pattern, opts.Root)

	matches, err := expandGlob(absBase, absPattern)
	if err != nil {
		reason := fmt.Sprintf("expand glob: %v", err)
		event := newReferenceEvent(pattern, kindFile, referenceLocationLocal, opts, ReferenceDecisionRejected, reason)

		return nil, []ReferenceEvent{event}, fmt.Errorf("expand glob: %w", err)
	}

	if len(matches) == 0 {
		event := newReferenceEvent(pattern, kindFile, referenceLocationLocal, opts, ReferenceDecisionSkipped, "glob matched no files")

		return nil, []ReferenceEvent{event}, nil
	}

	var (
		out    []LoadedReference
		events []ReferenceEvent
	)

	for _, path := range matches {
		remaining := opts.MaxTotalBytes - total
		if remaining <= 0 {
			events = append(events, newReferenceEvent(pattern, kindFile, referenceLocationLocal, opts, ReferenceDecisionSkipped, "max_total_bytes reached"))
			break
		}

		limit := min(opts.MaxFileBytes, remaining)
		source := displaySource(path, opts.Root)

		loaded, event, loadErr := loadSingleFile(path, source, limit, opts)
		if loadErr != nil {
			event.PolicyDecision = ReferenceDecisionSkipped
			events = append(events, event)

			continue // skip unreadable files in a glob
		}

		total += loaded.Bytes
		out = append(out, loaded)
		events = append(events, event)
	}

	return out, events, nil
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
func loadDirectory(dir, ref string, opts Options, policy normalizedReferencePolicy, total int) ([]LoadedReference, []ReferenceEvent, error) {
	var (
		out    []LoadedReference
		events []ReferenceEvent
	)

	err := filepath.WalkDir(dir, func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			events = append(events, newReferenceEvent(path, kindFile, referenceLocationLocal, opts, ReferenceDecisionSkipped, walkErr.Error()))
			return nil // skip inaccessible entries in directory walk
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

		if !pathInsideAnyRoot(path, policy.localRoots) {
			reason := "file outside allowed local roots"
			events = append(events, newReferenceEvent(path, kindFile, referenceLocationLocal, opts, ReferenceDecisionSkipped, reason))

			return nil
		}

		remaining := opts.MaxTotalBytes - total
		if remaining <= 0 {
			events = append(events, newReferenceEvent(ref, kindFile, referenceLocationLocal, opts, ReferenceDecisionSkipped, "max_total_bytes reached"))
			return fs.SkipAll
		}

		limit := min(opts.MaxFileBytes, remaining)

		rel, relErr := filepath.Rel(dir, path)
		if relErr != nil {
			events = append(events, newReferenceEvent(path, kindFile, referenceLocationLocal, opts, ReferenceDecisionSkipped, relErr.Error()))
			return nil // skip files with unresolvable relative paths
		}

		source := filepath.ToSlash(filepath.Join(ref, rel))

		loaded, event, loadErr := loadSingleFile(path, source, limit, opts)
		if loadErr != nil {
			event.PolicyDecision = ReferenceDecisionSkipped
			events = append(events, event)

			return nil //nolint:nilerr // skip unreadable / binary files
		}

		total += loaded.Bytes
		out = append(out, loaded)
		events = append(events, event)

		return nil
	})
	if err != nil {
		return out, events, fmt.Errorf("walk %s: %w", dir, err)
	}

	if len(out) == 0 && len(events) == 0 {
		events = append(events, newReferenceEvent(ref, kindFile, referenceLocationLocal, opts, ReferenceDecisionSkipped, "directory contained no loadable files"))
	}

	return out, events, nil
}

// ---------------------------------------------------------------------------
// Single-file loading with binary detection
// ---------------------------------------------------------------------------

// loadSingleFile reads a single file up to limit bytes and returns it as a
// LoadedReference. Binary files (detected by null bytes in the first 512
// bytes) are rejected.
func loadSingleFile(path, source string, limit int, opts Options) (LoadedReference, ReferenceEvent, error) {
	content, truncated, err := readLimited(path, limit)
	if err != nil {
		reason := fmt.Sprintf("read file: %v", err)
		event := newReferenceEvent(source, kindFile, referenceLocationLocal, opts, ReferenceDecisionRejected, reason)

		return LoadedReference{}, event, fmt.Errorf("read file: %w", err)
	}

	if isBinary(content) {
		reason := "binary file: " + source
		event := newReferenceEvent(source, kindFile, referenceLocationLocal, opts, ReferenceDecisionRejected, reason)

		return LoadedReference{}, event, errors.New(reason)
	}

	decision, reason := loadedDecision(truncated)
	event := newReferenceEvent(source, kindFile, referenceLocationLocal, opts, decision, reason)
	event.Bytes = len(content)
	event.Truncated = truncated
	event.DigestSHA256 = digestHex(content)

	return LoadedReference{
		Source:     source,
		Kind:       kindFile,
		Content:    string(content),
		Bytes:      len(content),
		Truncated:  truncated,
		Provenance: provenanceFromEvent(event),
	}, event, nil
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
	parsed, err := url.Parse(ref)
	if err != nil {
		return false
	}

	return parsed.Scheme != "" && strings.Contains(ref, "://")
}

func loadURL(ctx context.Context, rawURL string, limit int, opts Options, policy normalizedReferencePolicy) (LoadedReference, ReferenceEvent, error) {
	ctx, cancel := context.WithTimeout(ctx, urlFetchTimeout)
	defer cancel()

	parsed, err := url.Parse(rawURL)
	if err != nil {
		event := newReferenceEvent(rawURL, kindURL, referenceLocationRemote, opts, ReferenceDecisionRejected, err.Error())
		return LoadedReference{}, event, fmt.Errorf("parse URL: %w", err)
	}

	if policyErr := validateURLPolicy(parsed, policy); policyErr != nil {
		event := newReferenceEvent(rawURL, kindURL, referenceLocationRemote, opts, ReferenceDecisionRejected, policyErr.Error())
		return LoadedReference{}, event, policyErr
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, http.NoBody)
	if err != nil {
		event := newReferenceEvent(rawURL, kindURL, referenceLocationRemote, opts, ReferenceDecisionRejected, err.Error())
		return LoadedReference{}, event, fmt.Errorf("build request: %w", err)
	}

	client := referenceHTTPClient(policy)

	resp, err := client.Do(req)
	if err != nil {
		reason := fmt.Sprintf("fetch: %v", err)
		event := newReferenceEvent(rawURL, kindURL, referenceLocationRemote, opts, ReferenceDecisionRejected, reason)

		return LoadedReference{}, event, fmt.Errorf("fetch: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		reason := fmt.Sprintf("HTTP %d", resp.StatusCode)
		event := newReferenceEvent(rawURL, kindURL, referenceLocationRemote, opts, ReferenceDecisionRejected, reason)

		return LoadedReference{}, event, errors.New(reason)
	}

	if contentTypeErr := validateContentType(resp.Header.Get("Content-Type"), policy); contentTypeErr != nil {
		event := newReferenceEvent(rawURL, kindURL, referenceLocationRemote, opts, ReferenceDecisionRejected, contentTypeErr.Error())
		return LoadedReference{}, event, contentTypeErr
	}

	data, err := io.ReadAll(io.LimitReader(resp.Body, int64(limit+1)))
	if err != nil {
		reason := fmt.Sprintf("read body: %v", err)
		event := newReferenceEvent(rawURL, kindURL, referenceLocationRemote, opts, ReferenceDecisionRejected, reason)

		return LoadedReference{}, event, fmt.Errorf("read body: %w", err)
	}

	truncated := len(data) > limit
	if truncated {
		data = data[:limit]
	}

	if isBinary(data) {
		reason := "binary response body"
		event := newReferenceEvent(rawURL, kindURL, referenceLocationRemote, opts, ReferenceDecisionRejected, reason)

		return LoadedReference{}, event, errors.New(reason)
	}

	decision, reason := loadedDecision(truncated)
	event := newReferenceEvent(rawURL, kindURL, referenceLocationRemote, opts, decision, reason)
	event.Bytes = len(data)
	event.Truncated = truncated
	event.DigestSHA256 = digestHex(data)

	return LoadedReference{
		Source:     rawURL,
		Kind:       kindURL,
		Content:    string(data),
		Bytes:      len(data),
		Truncated:  truncated,
		Provenance: provenanceFromEvent(event),
	}, event, nil
}

func referenceHTTPClient(policy normalizedReferencePolicy) *http.Client {
	baseTransport, ok := http.DefaultTransport.(*http.Transport)
	if !ok {
		baseTransport = &http.Transport{}
	}

	transport := baseTransport.Clone()
	transport.Proxy = nil
	transport.DialContext = safeReferenceDialContext(policy)

	return &http.Client{
		Timeout:   urlFetchTimeout,
		Transport: transport,
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			if len(via) > policy.maxRedirects {
				return fmt.Errorf("redirect rejected: max_redirects=%d", policy.maxRedirects)
			}

			if err := validateURLPolicy(req.URL, policy); err != nil {
				return fmt.Errorf("redirect rejected: %w", err)
			}

			return nil
		},
	}
}

func safeReferenceDialContext(policy normalizedReferencePolicy) func(context.Context, string, string) (net.Conn, error) {
	return func(ctx context.Context, network, address string) (net.Conn, error) {
		host, port, err := splitDialAddress(address)
		if err != nil {
			return nil, err
		}

		ips, err := resolveReferenceDialIPs(ctx, host, policy)
		if err != nil {
			return nil, err
		}

		return dialReferenceIPs(ctx, network, port, ips)
	}
}

func splitDialAddress(address string) (host, port string, err error) {
	host, port, err = net.SplitHostPort(address)
	if err != nil {
		return "", "", fmt.Errorf("split host/port: %w", err)
	}

	return host, port, nil
}

func resolveReferenceDialIPs(ctx context.Context, host string, policy normalizedReferencePolicy) ([]net.IPAddr, error) {
	ips, err := net.DefaultResolver.LookupIPAddr(ctx, host)
	if err != nil {
		return nil, fmt.Errorf("resolve %s: %w", host, err)
	}

	if len(ips) == 0 {
		return nil, fmt.Errorf("resolve %s: no addresses", host)
	}

	if policy.allowPrivateNetworks {
		return ips, nil
	}

	for i := range ips {
		if isBlockedNetworkIP(ips[i].IP) {
			return nil, fmt.Errorf("private network address %s blocked", ips[i].IP.String())
		}
	}

	return ips, nil
}

func dialReferenceIPs(ctx context.Context, network, port string, ips []net.IPAddr) (net.Conn, error) {
	dialer := &net.Dialer{Timeout: urlFetchTimeout}

	var firstErr error

	for i := range ips {
		conn, dialErr := dialer.DialContext(ctx, network, net.JoinHostPort(ips[i].String(), port))
		if dialErr == nil {
			return conn, nil
		}

		if firstErr == nil {
			firstErr = dialErr
		}
	}

	return nil, fmt.Errorf("dial resolved reference address: %w", firstErr)
}

func validateURLPolicy(parsed *url.URL, policy normalizedReferencePolicy) error {
	scheme := strings.ToLower(parsed.Scheme)
	if scheme != "http" && scheme != "https" {
		return fmt.Errorf("scheme %q is not supported", parsed.Scheme)
	}

	if !policy.allowedSchemes[scheme] {
		return fmt.Errorf("scheme %q is not allowed", parsed.Scheme)
	}

	host := strings.ToLower(parsed.Hostname())
	if host == "" {
		return errors.New("URL host is required")
	}

	if len(policy.allowedHosts) == 0 {
		return fmt.Errorf("host %q rejected: no allowed_hosts configured", host)
	}

	if !hostAllowed(host, strings.ToLower(parsed.Host), policy.allowedHosts) {
		return fmt.Errorf("host %q is not in allowed_hosts", host)
	}

	return nil
}

func hostAllowed(host, hostWithPort string, allowedHosts []string) bool {
	for _, allowed := range allowedHosts {
		allowed = strings.ToLower(strings.TrimSpace(allowed))
		if allowed == "" {
			continue
		}

		if allowed == "*" || allowed == host || allowed == hostWithPort {
			return true
		}

		if domain, ok := strings.CutPrefix(allowed, "*."); ok {
			if strings.HasSuffix(host, "."+domain) {
				return true
			}
		}
	}

	return false
}

func isBlockedNetworkIP(ip net.IP) bool {
	if ip == nil {
		return true
	}

	return ip.IsUnspecified() ||
		ip.IsLoopback() ||
		ip.IsPrivate() ||
		ip.IsLinkLocalUnicast() ||
		ip.IsLinkLocalMulticast() ||
		ip.IsMulticast()
}

func validateContentType(header string, policy normalizedReferencePolicy) error {
	if strings.TrimSpace(header) == "" {
		return errors.New("missing Content-Type")
	}

	mediaType, _, err := mime.ParseMediaType(header)
	if err != nil {
		return fmt.Errorf("parse Content-Type: %w", err)
	}

	mediaType = strings.ToLower(mediaType)
	for _, allowed := range policy.contentTypes {
		if contentTypeAllowed(mediaType, allowed) {
			return nil
		}
	}

	return fmt.Errorf("Content-Type %q is not allowed", mediaType)
}

func contentTypeAllowed(mediaType, allowed string) bool {
	allowed = strings.ToLower(strings.TrimSpace(allowed))
	if allowed == "*/*" || allowed == mediaType {
		return true
	}

	if strings.HasSuffix(allowed, "/*") {
		return strings.HasPrefix(mediaType, strings.TrimSuffix(allowed, "*"))
	}

	return false
}

// ---------------------------------------------------------------------------
// Helpers
// ---------------------------------------------------------------------------

// displaySource builds a human-readable source label for a matched file.
// When the matched path starts inside root, it is shown relative to root.
// Otherwise it is shown as a relative escape path when possible.
func displaySource(path, root string) string {
	rel, err := filepath.Rel(root, path)
	if err != nil {
		return path
	}

	return filepath.ToSlash(rel)
}

func newReferenceEvent(source, kind, location string, opts Options, decision, reason string) ReferenceEvent {
	return ReferenceEvent{
		Source:         source,
		Kind:           kind,
		Scope:          referenceScope(opts),
		Location:       location,
		FetchedAt:      time.Now().UTC(),
		PolicyDecision: decision,
		PolicyReason:   reason,
	}
}

func referenceScope(opts Options) string {
	if strings.TrimSpace(opts.ReferenceScope) != "" {
		return strings.TrimSpace(opts.ReferenceScope)
	}

	return ReferenceScopeGlobal
}

func loadedDecision(truncated bool) (decision, reason string) {
	if truncated {
		return ReferenceDecisionTruncated, "byte limit reached"
	}

	return ReferenceDecisionLoaded, "allowed by policy"
}

func provenanceFromEvent(event ReferenceEvent) ReferenceProvenance {
	return ReferenceProvenance{
		Scope:          event.Scope,
		Location:       event.Location,
		Size:           event.Bytes,
		Truncated:      event.Truncated,
		DigestSHA256:   event.DigestSHA256,
		FetchedAt:      event.FetchedAt,
		PolicyDecision: event.PolicyDecision,
		PolicyReason:   event.PolicyReason,
	}
}

func provenanceForFormat(ref LoadedReference) ReferenceProvenance {
	prov := ref.Provenance
	if prov.Scope == "" {
		prov.Scope = ReferenceScopeGlobal
	}

	if prov.Location == "" {
		if ref.Kind == kindURL {
			prov.Location = referenceLocationRemote
		} else {
			prov.Location = referenceLocationLocal
		}
	}

	if prov.Size == 0 && ref.Bytes > 0 {
		prov.Size = ref.Bytes
	}

	if prov.Size == 0 && ref.Content != "" {
		prov.Size = len(ref.Content)
	}

	if prov.DigestSHA256 == "" && ref.Content != "" {
		prov.DigestSHA256 = digestHex([]byte(ref.Content))
	}

	if prov.PolicyDecision == "" {
		decision, reason := loadedDecision(ref.Truncated)
		prov.PolicyDecision = decision
		prov.PolicyReason = reason
	}

	if prov.PolicyReason == "" {
		_, prov.PolicyReason = loadedDecision(ref.Truncated)
	}

	return prov
}

func digestHex(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

func safeReferenceTag(kind string) string {
	switch kind {
	case kindFile, kindURL:
		return kind
	default:
		return kindFile
	}
}

func escapeText(value string) string {
	value = strings.ReplaceAll(value, "&", "&amp;")
	value = strings.ReplaceAll(value, "<", "&lt;")
	value = strings.ReplaceAll(value, ">", "&gt;")

	return value
}
