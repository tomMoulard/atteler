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
	"regexp"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/tommoulard/atteler/pkg/contextpack"
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
	// ReferenceDecisionOmitted means content was read but intentionally left out of the final context.
	ReferenceDecisionOmitted = "omitted"
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
	// ReferenceScopeInline identifies user prompt @path references.
	ReferenceScopeInline = "inline"

	referenceManifestSchemaVersion = 1
)

var defaultAllowedContentTypes = []string{
	"text/*",
	"application/json",
	"application/xml",
	"application/x-yaml",
	"application/yaml",
	"application/toml",
}

var urlInTextPattern = regexp.MustCompile(`(?i)\b[a-z][a-z0-9+.-]*://[^\s"'<>()]+`)

// skipDirs is the set of directory names that are skipped when walking a
// directory reference. These are build artifacts, VCS metadata, and
// dependency caches that are almost never useful as style references.
var skipDirs = map[string]bool{
	".git":         true,
	"node_modules": true,
	"__pycache__":  true,
}

// ReferencePolicy defines the trust boundary for configured references. Local
// references are limited to Options.Root plus LocalRoots. Absolute local
// reference strings must also resolve under an explicit LocalRoots entry, even
// when they point back into Options.Root. Remote references must match
// AllowedSchemes and AllowedHosts, must not resolve to a private/local network
// unless AllowPrivateNetworks is true, must satisfy the redirect limit, and must
// return an allowed Content-Type.
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
	Scope            string                    `json:"scope,omitempty"`
	Location         string                    `json:"location,omitempty"`
	ResolvedSource   string                    `json:"resolved_source,omitempty"`
	TokenEstimator   string                    `json:"token_estimator,omitempty"`
	Size             int                       `json:"size,omitempty"`
	Truncated        bool                      `json:"truncated,omitempty"`
	DigestSHA256     string                    `json:"digest_sha256,omitempty"`
	FetchedAt        time.Time                 `json:"fetched_at,omitzero"`
	PolicyDecision   string                    `json:"policy_decision,omitempty"`
	PolicyReason     string                    `json:"policy_reason,omitempty"`
	PolicyReasonCode string                    `json:"policy_reason_code,omitempty"`
	TokenEstimate    contextpack.TokenEstimate `json:"token_estimate,omitzero"`
}

// ReferenceEvent describes one attempted reference ingestion decision. CLI
// callers should surface these events so policy rejections and skipped files are
// visible instead of silently changing the context supplied to the model.
//
//nolint:govet // field order follows the CLI/provenance report order.
type ReferenceEvent struct {
	Source           string                    `json:"source,omitempty"`
	ResolvedSource   string                    `json:"resolved_source,omitempty"`
	Kind             string                    `json:"kind,omitempty"`
	Scope            string                    `json:"scope,omitempty"`
	Location         string                    `json:"location,omitempty"`
	TokenEstimator   string                    `json:"token_estimator,omitempty"`
	Bytes            int                       `json:"bytes,omitempty"`
	Truncated        bool                      `json:"truncated,omitempty"`
	DigestSHA256     string                    `json:"digest_sha256,omitempty"`
	FetchedAt        time.Time                 `json:"fetched_at,omitzero"`
	PolicyDecision   string                    `json:"policy_decision,omitempty"`
	PolicyReason     string                    `json:"policy_reason,omitempty"`
	PolicyReasonCode string                    `json:"policy_reason_code,omitempty"`
	TokenEstimate    contextpack.TokenEstimate `json:"token_estimate,omitzero"`
}

// ReferenceManifest summarizes every configured-reference ingestion decision in
// a compact, machine-readable audit record.
//
//nolint:govet // Field order keeps schema/estimator/entries before aggregate counts in JSON.
type ReferenceManifest struct {
	SchemaVersion                 int              `json:"schema_version"`
	TokenEstimator                string           `json:"token_estimator,omitempty"`
	Entries                       []ReferenceEvent `json:"entries,omitempty"`
	TotalBytes                    int              `json:"total_bytes"`
	TotalEstimatedTokens          int              `json:"total_estimated_tokens"`
	TotalEstimatedTokenErrorBound int              `json:"total_estimated_token_error_bound"`
	TotalEstimatedTokenUpperBound int              `json:"total_estimated_token_upper_bound"`
	IncludedCount                 int              `json:"included_count"`
	TruncatedCount                int              `json:"truncated_count"`
	OmittedCount                  int              `json:"omitted_count"`
	SkippedCount                  int              `json:"skipped_count"`
	RejectedCount                 int              `json:"rejected_count"`
}

// LoadedReference describes one resolved reference entry with its content.
//
//nolint:govet // field order preserves the historical API shape first.
type LoadedReference struct {
	// Source is the original reference string (path or redacted URL).
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
	explicitLocalRoots   []string
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
			events = append(events, skippedReferenceEvent(ref, opts, "max_total_bytes already reached"))
			continue
		}

		loaded, loadEvents, loadErr := loadReference(ctx, ref, opts, policy, total)

		events = append(events, loadEvents...)
		if loadErr != nil {
			errs = append(errs, fmt.Errorf("reference %q: %w", referenceErrorSource(ref, loadEvents), loadErr))
			continue
		}

		for i := range loaded {
			total += loaded[i].Bytes
			out = append(out, loaded[i])
		}
	}

	return out, events, errors.Join(errs...)
}

// BuildReferenceManifest converts reference events into an aggregate manifest
// for logs, hooks, and tests. Loaded and truncated entries count as included;
// skipped/rejected entries remain in Entries with their reason code so absence
// of context is auditable.
func BuildReferenceManifest(events []ReferenceEvent) ReferenceManifest {
	entries := make([]ReferenceEvent, len(events))
	for i := range events {
		entries[i] = sanitizeReferenceEvent(events[i])
	}

	manifest := ReferenceManifest{
		SchemaVersion: referenceManifestSchemaVersion,
		Entries:       entries,
	}

	for i := range entries {
		event := &entries[i]
		if manifest.TokenEstimator == "" && event.TokenEstimator != "" {
			manifest.TokenEstimator = event.TokenEstimator
		}

		switch event.PolicyDecision {
		case ReferenceDecisionLoaded, ReferenceDecisionTruncated:
			manifest.IncludedCount++
			manifest.TotalBytes += event.Bytes
			manifest.TotalEstimatedTokens += event.TokenEstimate.Tokens
			manifest.TotalEstimatedTokenErrorBound += event.TokenEstimate.ErrorBoundTokens
			manifest.TotalEstimatedTokenUpperBound += event.TokenEstimate.UpperBoundTokens

			if event.PolicyDecision == ReferenceDecisionTruncated || event.Truncated {
				manifest.TruncatedCount++
			}
		case ReferenceDecisionSkipped:
			manifest.SkippedCount++
		case ReferenceDecisionOmitted:
			manifest.OmittedCount++
		case ReferenceDecisionRejected:
			manifest.RejectedCount++
		}
	}

	return manifest
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

	_, err = os.Lstat(resolved)
	if err != nil {
		reason := "stat: " + safePathErrorMessage(err)
		event := newReferenceEvent(ref, kindFile, referenceLocationLocal, opts, ReferenceDecisionRejected, reason)

		return nil, []ReferenceEvent{event}, errors.New(reason)
	}

	resolved, err = resolvePolicySymlinks(resolved, ref, policy)
	if err != nil {
		event := newReferenceEvent(ref, kindFile, referenceLocationLocal, opts, ReferenceDecisionRejected, err.Error())
		return nil, []ReferenceEvent{event}, err
	}

	info, err := os.Stat(resolved)
	if err != nil {
		reason := "stat: " + safePathErrorMessage(err)
		event := newReferenceEvent(ref, kindFile, referenceLocationLocal, opts, ReferenceDecisionRejected, reason)

		return nil, []ReferenceEvent{event}, errors.New(reason)
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

func referenceErrorSource(ref string, events []ReferenceEvent) string {
	for i := range events {
		if strings.TrimSpace(events[i].Source) != "" {
			return events[i].Source
		}
	}

	if isURL(ref) {
		return referenceURLSource(ref)
	}

	return ref
}

func skippedReferenceEvent(ref string, opts Options, reason string) ReferenceEvent {
	if isURL(ref) {
		return newReferenceEvent(referenceURLSource(ref), kindURL, referenceLocationRemote, opts, ReferenceDecisionSkipped, reason)
	}

	return newReferenceEvent(ref, kindFile, referenceLocationLocal, opts, ReferenceDecisionSkipped, reason)
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
		b.WriteString(escapeAttr(displayReferenceSource(ref)))

		if prov.ResolvedSource != "" {
			b.WriteString(`" resolved_source="`)
			b.WriteString(escapeAttr(prov.ResolvedSource))
		}

		b.WriteString(`" truncated="`)
		b.WriteString(strconv.FormatBool(ref.Truncated))
		b.WriteString(`" scope="`)
		b.WriteString(escapeAttr(prov.Scope))
		b.WriteString(`" location="`)
		b.WriteString(escapeAttr(prov.Location))
		b.WriteString(`" bytes="`)
		b.WriteString(strconv.Itoa(prov.Size))

		if prov.TokenEstimate.Tokens > 0 || prov.TokenEstimate.UpperBoundTokens > 0 {
			b.WriteString(`" estimated_tokens="`)
			b.WriteString(strconv.Itoa(prov.TokenEstimate.Tokens))
			b.WriteString(`" estimated_token_error_bound="`)
			b.WriteString(strconv.Itoa(prov.TokenEstimate.ErrorBoundTokens))
			b.WriteString(`" estimated_token_upper_bound="`)
			b.WriteString(strconv.Itoa(prov.TokenEstimate.UpperBoundTokens))
		}

		if prov.TokenEstimator != "" {
			b.WriteString(`" token_estimator="`)
			b.WriteString(escapeAttr(prov.TokenEstimator))
		}

		b.WriteString(`" digest_sha256="`)
		b.WriteString(escapeAttr(prov.DigestSHA256))
		b.WriteString(`" policy_decision="`)
		b.WriteString(escapeAttr(prov.PolicyDecision))
		b.WriteString(`" policy_reason_code="`)
		b.WriteString(escapeAttr(prov.PolicyReasonCode))
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

func displayReferenceSource(ref LoadedReference) string {
	if ref.Kind == kindURL || isURL(ref.Source) {
		return referenceURLSource(ref.Source)
	}

	return ref.Source
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
	explicitRoots := make([]string, 0, len(opts.ReferencePolicy.LocalRoots))

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
		explicitRoots = append(explicitRoots, absRoot)
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
		explicitLocalRoots:   explicitRoots,
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

	if filepath.IsAbs(ref) && !pathInsideAnyRoot(containmentPath, policy.explicitLocalRoots) {
		return "", fmt.Errorf("absolute path %q requires an explicit local_roots entry", ref)
	}

	return resolved, nil
}

func resolvePolicySymlinks(path, ref string, policy normalizedReferencePolicy) (string, error) {
	resolved, err := filepath.EvalSymlinks(path)
	if err != nil {
		return "", fmt.Errorf("resolve %q symlinks: %s", ref, safePathErrorMessage(err))
	}

	resolved, err = cleanAbs(resolved)
	if err != nil {
		return "", fmt.Errorf("resolve %q symlinks: %s", ref, safePathErrorMessage(err))
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
	base, globSuffix := globBase(pattern)
	base = resolvePath(base, opts.Root)

	absBase, err := cleanAbsForPolicy(base)
	if err != nil {
		event := newReferenceEvent(pattern, kindFile, referenceLocationLocal, opts, ReferenceDecisionRejected, err.Error())

		return nil, []ReferenceEvent{event}, err
	}

	sourceBase, err := cleanAbs(base)
	if err != nil {
		reason := "resolve glob source base: " + safePathErrorMessage(err)
		event := newReferenceEvent(pattern, kindFile, referenceLocationLocal, opts, ReferenceDecisionRejected, reason)

		return nil, []ReferenceEvent{event}, errors.New(reason)
	}

	if !pathInsideAnyRoot(absBase, policy.localRoots) {
		reason := fmt.Sprintf("glob base %q is outside allowed local roots", pattern)
		event := newReferenceEvent(pattern, kindFile, referenceLocationLocal, opts, ReferenceDecisionRejected, reason)

		return nil, []ReferenceEvent{event}, errors.New(reason)
	}

	if filepath.IsAbs(pattern) && !pathInsideAnyRoot(absBase, policy.explicitLocalRoots) {
		reason := fmt.Sprintf("absolute glob %q requires an explicit local_roots entry", pattern)
		event := newReferenceEvent(pattern, kindFile, referenceLocationLocal, opts, ReferenceDecisionRejected, reason)

		return nil, []ReferenceEvent{event}, errors.New(reason)
	}

	absPattern := globPatternForWalkBase(absBase, globSuffix)

	matches, err := expandGlob(absBase, absPattern)
	if err != nil {
		reason := "expand glob: " + safePathErrorMessage(err)
		event := newReferenceEvent(pattern, kindFile, referenceLocationLocal, opts, ReferenceDecisionRejected, reason)

		return nil, []ReferenceEvent{event}, errors.New(reason)
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
		source := displayGlobSource(path, sourceBase, absBase, opts.Root)

		loadPath, skipEvent, ok := globLoadCandidate(path, source, opts, policy)
		if skipEvent.PolicyDecision != "" {
			events = append(events, skipEvent)
		}

		if !ok {
			continue
		}

		remaining := opts.MaxTotalBytes - total
		if remaining <= 0 {
			events = append(events, newReferenceEvent(pattern, kindFile, referenceLocationLocal, opts, ReferenceDecisionSkipped, "max_total_bytes reached"))
			break
		}

		limit := min(opts.MaxFileBytes, remaining)

		loaded, event, loadErr := loadSingleFile(loadPath, source, limit, opts)
		if loadErr != nil {
			event = withReferenceDecision(event, ReferenceDecisionSkipped, event.PolicyReason)
			events = append(events, event)

			continue // skip unreadable files in a glob
		}

		total += loaded.Bytes
		out = append(out, loaded)
		events = append(events, event)
	}

	return out, events, nil
}

func globPatternForWalkBase(walkBase, globSuffix string) string {
	if globSuffix == "" {
		return filepath.ToSlash(walkBase)
	}

	return filepath.ToSlash(filepath.Join(walkBase, filepath.FromSlash(globSuffix)))
}

func displayGlobSource(path, sourceBase, walkBase, root string) string {
	rel, err := filepath.Rel(walkBase, path)
	if err != nil || rel == "." || rel == ".." || strings.HasPrefix(rel, ".."+string(os.PathSeparator)) {
		return displaySource(path, root)
	}

	return displaySource(filepath.Join(sourceBase, rel), root)
}

func globLoadCandidate(path, source string, opts Options, policy normalizedReferencePolicy) (string, ReferenceEvent, bool) {
	info, err := os.Lstat(path)
	if err != nil {
		reason := "stat glob match: " + safePathErrorMessage(err)

		return "", newReferenceEvent(source, kindFile, referenceLocationLocal, opts, ReferenceDecisionSkipped, reason), false
	}

	if info.Mode()&fs.ModeSymlink != 0 {
		return symlinkTargetLoadPath(path, source, opts, policy)
	}

	if !info.Mode().IsRegular() {
		return "", ReferenceEvent{}, false
	}

	if !pathInsideAnyRoot(path, policy.localRoots) {
		reason := "file outside allowed local roots"

		return "", newReferenceEvent(source, kindFile, referenceLocationLocal, opts, ReferenceDecisionSkipped, reason), false
	}

	return path, ReferenceEvent{}, true
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

		entryType := entry.Type()
		if !entryType.IsRegular() && entryType&fs.ModeSymlink == 0 {
			return nil
		}

		if matchGlob(absPattern, filepath.ToSlash(path)) {
			matches = append(matches, path)
		}

		return nil
	})
	if err != nil {
		return matches, fmt.Errorf("walk glob base: %s", safePathErrorMessage(err))
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
			events = append(events, newReferenceEvent(displaySource(path, opts.Root), kindFile, referenceLocationLocal, opts, ReferenceDecisionSkipped, safePathErrorMessage(walkErr)))
			return nil // skip inaccessible entries in directory walk
		}

		if entry.IsDir() {
			if skipDirs[entry.Name()] {
				return filepath.SkipDir
			}

			return nil
		}

		source, loadPath, skipEvent, ok := directoryLoadCandidate(dir, ref, path, entry, opts, policy)
		if skipEvent.PolicyDecision != "" {
			events = append(events, skipEvent)
		}

		if !ok {
			return nil
		}

		remaining := opts.MaxTotalBytes - total
		if remaining <= 0 {
			events = append(events, newReferenceEvent(ref, kindFile, referenceLocationLocal, opts, ReferenceDecisionSkipped, "max_total_bytes reached"))
			return fs.SkipAll
		}

		limit := min(opts.MaxFileBytes, remaining)

		loaded, event, loadErr := loadSingleFile(loadPath, source, limit, opts)
		if loadErr != nil {
			event = withReferenceDecision(event, ReferenceDecisionSkipped, event.PolicyReason)
			events = append(events, event)

			return nil //nolint:nilerr // skip unreadable / binary files
		}

		total += loaded.Bytes
		out = append(out, loaded)
		events = append(events, event)

		return nil
	})
	if err != nil {
		return out, events, fmt.Errorf("walk directory reference: %s", safePathErrorMessage(err))
	}

	if len(out) == 0 && len(events) == 0 {
		events = append(events, newReferenceEvent(ref, kindFile, referenceLocationLocal, opts, ReferenceDecisionSkipped, "directory contained no loadable files"))
	}

	return out, events, nil
}

func directoryLoadCandidate(
	dir, ref, path string,
	entry fs.DirEntry,
	opts Options,
	policy normalizedReferencePolicy,
) (source, loadPath string, skipEvent ReferenceEvent, ok bool) {
	source, sourceErr := directoryEntrySource(dir, ref, path)
	if sourceErr != nil {
		return "", "", newReferenceEvent(path, kindFile, referenceLocationLocal, opts, ReferenceDecisionSkipped, sourceErr.Error()), false
	}

	loadPath, skipEvent, ok = directoryEntryLoadPath(path, source, entry, opts, policy)
	if !ok {
		return source, "", skipEvent, false
	}

	if !pathInsideAnyRoot(loadPath, policy.localRoots) {
		reason := "file outside allowed local roots"

		return source, "", newReferenceEvent(source, kindFile, referenceLocationLocal, opts, ReferenceDecisionSkipped, reason), false
	}

	return source, loadPath, ReferenceEvent{}, true
}

func directoryEntryLoadPath(
	path, source string,
	entry fs.DirEntry,
	opts Options,
	policy normalizedReferencePolicy,
) (string, ReferenceEvent, bool) {
	if entry.Type()&fs.ModeSymlink == 0 {
		return path, ReferenceEvent{}, entry.Type().IsRegular()
	}

	return symlinkTargetLoadPath(path, source, opts, policy)
}

func symlinkTargetLoadPath(
	path, source string,
	opts Options,
	policy normalizedReferencePolicy,
) (string, ReferenceEvent, bool) {
	resolved, symlinkErr := resolvePolicySymlinks(path, source, policy)
	if symlinkErr != nil {
		return "", newReferenceEvent(source, kindFile, referenceLocationLocal, opts, ReferenceDecisionSkipped, symlinkErr.Error()), false
	}

	info, statErr := os.Stat(resolved)
	if statErr != nil {
		reason := "stat symlink target: " + safePathErrorMessage(statErr)

		return "", newReferenceEvent(source, kindFile, referenceLocationLocal, opts, ReferenceDecisionSkipped, reason), false
	}

	if info.IsDir() {
		return "", newReferenceEvent(source, kindFile, referenceLocationLocal, opts, ReferenceDecisionSkipped, "symlinked directory skipped"), false
	}

	if !info.Mode().IsRegular() {
		return "", newReferenceEvent(source, kindFile, referenceLocationLocal, opts, ReferenceDecisionSkipped, "symlink target is not a regular file"), false
	}

	return resolved, ReferenceEvent{}, true
}

func directoryEntrySource(dir, ref, path string) (string, error) {
	rel, err := filepath.Rel(dir, path)
	if err != nil {
		return "", fmt.Errorf("resolve relative directory entry: %w", err)
	}

	return filepath.ToSlash(filepath.Join(ref, rel)), nil
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
		reason := "read file: " + safePathErrorMessage(err)
		event := newReferenceEvent(source, kindFile, referenceLocationLocal, opts, ReferenceDecisionRejected, reason)

		return LoadedReference{}, event, errors.New(reason)
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
	event.TokenEstimate, event.TokenEstimator = estimateReferenceContent(opts, content)

	return LoadedReference{
		Source:     source,
		Kind:       kindFile,
		Content:    string(content),
		Bytes:      len(content),
		Truncated:  truncated,
		Provenance: provenanceFromEvent(event),
	}, event, nil
}

// isBinary returns true if data is not valid UTF-8 text or has a null byte in
// the first binaryProbeBytes, which is the same null-byte heuristic git uses.
func isBinary(data []byte) bool {
	if !utf8.Valid(data) {
		return true
	}

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

func referenceURLSource(rawURL string) string {
	parsed, err := url.Parse(rawURL)
	if err != nil {
		return redactMalformedURLSource(rawURL)
	}

	return referenceURLSourceFromParsed(parsed)
}

func referenceURLSourceFromParsed(parsed *url.URL) string {
	if parsed == nil {
		return ""
	}

	safe := *parsed
	if safe.User != nil {
		safe.User = url.User("REDACTED")
	}

	query := safe.Query()
	for key, values := range query {
		if !sensitiveURLQueryKey(key) {
			continue
		}

		for i := range values {
			values[i] = "REDACTED"
		}

		query[key] = values
	}

	safe.RawQuery = query.Encode()

	return safe.String()
}

func sensitiveURLQueryKey(key string) bool {
	key = strings.ToLower(strings.TrimSpace(key))
	key = strings.NewReplacer("-", "_", ".", "_").Replace(key)

	switch key {
	case "api_key", "apikey", "access_key", "auth", "authorization", "key", "password", "passwd", "pwd", "secret":
		return true
	}

	return strings.Contains(key, "token") ||
		strings.Contains(key, "secret") ||
		strings.Contains(key, "credential") ||
		strings.Contains(key, "password") ||
		strings.Contains(key, "private_key")
}

func redactMalformedURLSource(rawURL string) string {
	safe := redactURLUserInfoFallback(rawURL)

	queryStart := strings.Index(safe, "?")
	if queryStart < 0 {
		return safe
	}

	queryEnd := len(safe)
	if fragmentStart := strings.Index(safe[queryStart+1:], "#"); fragmentStart >= 0 {
		queryEnd = queryStart + 1 + fragmentStart
	}

	query := safe[queryStart+1 : queryEnd]
	if query == "" {
		return safe
	}

	parts := strings.Split(query, "&")
	for i, part := range parts {
		key, _, _ := strings.Cut(part, "=")

		decodedKey, err := url.QueryUnescape(key)
		if err != nil {
			decodedKey = key
		}

		if sensitiveURLQueryKey(decodedKey) {
			parts[i] = key + "=REDACTED"
		}
	}

	return safe[:queryStart+1] + strings.Join(parts, "&") + safe[queryEnd:]
}

func redactURLUserInfoFallback(rawURL string) string {
	schemeIdx := strings.Index(rawURL, "://")
	if schemeIdx < 0 {
		return rawURL
	}

	authorityStart := schemeIdx + len("://")
	authorityEnd := len(rawURL)

	for _, sep := range []string{"/", "?", "#"} {
		if idx := strings.Index(rawURL[authorityStart:], sep); idx >= 0 && authorityStart+idx < authorityEnd {
			authorityEnd = authorityStart + idx
		}
	}

	authority := rawURL[authorityStart:authorityEnd]

	at := strings.LastIndex(authority, "@")
	if at < 0 {
		return rawURL
	}

	return rawURL[:authorityStart] + "REDACTED" + authority[at:] + rawURL[authorityEnd:]
}

func redactURLsInText(text string) string {
	return urlInTextPattern.ReplaceAllStringFunc(text, func(candidate string) string {
		urlText, suffix := trimURLTrailingPunctuation(candidate)

		return referenceURLSource(urlText) + suffix
	})
}

func trimURLTrailingPunctuation(candidate string) (urlText, suffix string) {
	for candidate != "" {
		last := candidate[len(candidate)-1]
		if !strings.ContainsRune(".,;:!?", rune(last)) {
			break
		}

		suffix = string(last) + suffix
		candidate = candidate[:len(candidate)-1]
	}

	return candidate, suffix
}

func sanitizeURLPolicyReason(reason, rawURL, source string) string {
	if source != "" && rawURL != "" && source != rawURL {
		reason = strings.ReplaceAll(reason, rawURL, source)
	}

	return redactURLsInText(reason)
}

func loadURL(ctx context.Context, rawURL string, limit int, opts Options, policy normalizedReferencePolicy) (LoadedReference, ReferenceEvent, error) {
	ctx, cancel := context.WithTimeout(ctx, urlFetchTimeout)
	defer cancel()

	source := referenceURLSource(rawURL)

	parsed, err := url.Parse(rawURL)
	if err != nil {
		reason := sanitizeURLPolicyReason(err.Error(), rawURL, source)
		event := newReferenceEvent(source, kindURL, referenceLocationRemote, opts, ReferenceDecisionRejected, reason)

		return LoadedReference{}, event, fmt.Errorf("parse URL: %s", reason)
	}

	source = referenceURLSourceFromParsed(parsed)

	if policyErr := validateURLPolicy(parsed, policy); policyErr != nil {
		event := newReferenceEvent(source, kindURL, referenceLocationRemote, opts, ReferenceDecisionRejected, sanitizeURLPolicyReason(policyErr.Error(), rawURL, source))
		return LoadedReference{}, event, policyErr
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, http.NoBody)
	if err != nil {
		reason := sanitizeURLPolicyReason(err.Error(), rawURL, source)
		event := newReferenceEvent(source, kindURL, referenceLocationRemote, opts, ReferenceDecisionRejected, reason)

		return LoadedReference{}, event, fmt.Errorf("build request: %s", reason)
	}

	client := referenceHTTPClient(policy)

	resp, err := client.Do(req)
	if err != nil {
		reason := sanitizeURLPolicyReason(fmt.Sprintf("fetch: %v", err), rawURL, source)
		event := newReferenceEvent(source, kindURL, referenceLocationRemote, opts, ReferenceDecisionRejected, reason)

		return LoadedReference{}, event, errors.New(reason)
	}
	defer resp.Body.Close()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		reason := fmt.Sprintf("HTTP %d", resp.StatusCode)
		event := newReferenceEvent(source, kindURL, referenceLocationRemote, opts, ReferenceDecisionRejected, reason)
		withURLResolvedSource(&event, source, responseURL(resp))

		return LoadedReference{}, event, errors.New(reason)
	}

	if contentTypeErr := validateContentType(resp.Header.Get("Content-Type"), policy); contentTypeErr != nil {
		event := newReferenceEvent(source, kindURL, referenceLocationRemote, opts, ReferenceDecisionRejected, contentTypeErr.Error())
		withURLResolvedSource(&event, source, responseURL(resp))

		return LoadedReference{}, event, contentTypeErr
	}

	data, err := io.ReadAll(io.LimitReader(resp.Body, int64(limit+1)))
	if err != nil {
		reason := fmt.Sprintf("read body: %v", err)
		event := newReferenceEvent(source, kindURL, referenceLocationRemote, opts, ReferenceDecisionRejected, reason)
		withURLResolvedSource(&event, source, responseURL(resp))

		return LoadedReference{}, event, fmt.Errorf("read body: %w", err)
	}

	truncated := len(data) > limit
	if truncated {
		data = data[:limit]
	}

	if isBinary(data) {
		reason := "binary response body"
		event := newReferenceEvent(source, kindURL, referenceLocationRemote, opts, ReferenceDecisionRejected, reason)
		withURLResolvedSource(&event, source, responseURL(resp))

		return LoadedReference{}, event, errors.New(reason)
	}

	decision, reason := loadedDecision(truncated)
	event := newReferenceEvent(source, kindURL, referenceLocationRemote, opts, decision, reason)
	withURLResolvedSource(&event, source, responseURL(resp))
	event.Bytes = len(data)
	event.Truncated = truncated
	event.DigestSHA256 = digestHex(data)
	event.TokenEstimate, event.TokenEstimator = estimateReferenceContent(opts, data)

	return LoadedReference{
		Source:     source,
		Kind:       kindURL,
		Content:    string(data),
		Bytes:      len(data),
		Truncated:  truncated,
		Provenance: provenanceFromEvent(event),
	}, event, nil
}

func responseURL(resp *http.Response) *url.URL {
	if resp == nil || resp.Request == nil {
		return nil
	}

	return resp.Request.URL
}

func withURLResolvedSource(event *ReferenceEvent, source string, finalURL *url.URL) {
	if event == nil {
		return
	}

	resolvedSource := referenceURLSourceFromParsed(finalURL)
	if resolvedSource == "" || resolvedSource == source {
		return
	}

	event.ResolvedSource = resolvedSource
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
		Source:           source,
		Kind:             kind,
		Scope:            referenceScope(opts),
		Location:         location,
		FetchedAt:        time.Now().UTC(),
		PolicyDecision:   decision,
		PolicyReason:     reason,
		PolicyReasonCode: ReferenceReasonCode(decision, reason),
	}
}

func withReferenceDecision(event ReferenceEvent, decision, reason string) ReferenceEvent {
	event.PolicyDecision = decision
	event.PolicyReason = reason
	event.PolicyReasonCode = ReferenceReasonCode(decision, reason)

	return event
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
	event = sanitizeReferenceEvent(event)

	return ReferenceProvenance{
		Scope:            event.Scope,
		Location:         event.Location,
		ResolvedSource:   event.ResolvedSource,
		TokenEstimator:   event.TokenEstimator,
		Size:             event.Bytes,
		Truncated:        event.Truncated,
		DigestSHA256:     event.DigestSHA256,
		FetchedAt:        event.FetchedAt,
		PolicyDecision:   event.PolicyDecision,
		PolicyReason:     event.PolicyReason,
		PolicyReasonCode: event.PolicyReasonCode,
		TokenEstimate:    event.TokenEstimate,
	}
}

func provenanceForFormat(ref LoadedReference) ReferenceProvenance {
	prov := ref.Provenance

	prov = withDefaultReferenceScopeAndLocation(prov, ref.Kind)
	prov = withDefaultReferenceSizeDigestAndTokens(prov, ref)
	prov = withDefaultReferencePolicyDecision(prov, ref.Truncated)

	if prov.ResolvedSource != "" && (ref.Kind == kindURL || isURL(prov.ResolvedSource)) {
		prov.ResolvedSource = referenceURLSource(prov.ResolvedSource)
	}

	prov.PolicyReason = redactURLsInText(prov.PolicyReason)
	if prov.PolicyReasonCode == "" {
		prov.PolicyReasonCode = ReferenceReasonCode(prov.PolicyDecision, prov.PolicyReason)
	}

	return prov
}

func sanitizeReferenceEvent(event ReferenceEvent) ReferenceEvent {
	if event.Kind == kindURL || isURL(event.Source) {
		event.Source = referenceURLSource(event.Source)
	}

	if event.ResolvedSource != "" && (event.Kind == kindURL || isURL(event.ResolvedSource)) {
		event.ResolvedSource = referenceURLSource(event.ResolvedSource)
	}

	event.PolicyReason = redactURLsInText(event.PolicyReason)
	if event.PolicyReasonCode == "" {
		event.PolicyReasonCode = ReferenceReasonCode(event.PolicyDecision, event.PolicyReason)
	}

	return event
}

func withDefaultReferenceScopeAndLocation(prov ReferenceProvenance, kind string) ReferenceProvenance {
	if prov.Scope == "" {
		prov.Scope = ReferenceScopeGlobal
	}

	if prov.Location == "" {
		if kind == kindURL {
			prov.Location = referenceLocationRemote
		} else {
			prov.Location = referenceLocationLocal
		}
	}

	return prov
}

func withDefaultReferenceSizeDigestAndTokens(prov ReferenceProvenance, ref LoadedReference) ReferenceProvenance {
	if prov.Size == 0 && ref.Bytes > 0 {
		prov.Size = ref.Bytes
	}

	if prov.Size == 0 && ref.Content != "" {
		prov.Size = len(ref.Content)
	}

	if prov.DigestSHA256 == "" && ref.Content != "" {
		prov.DigestSHA256 = digestHex([]byte(ref.Content))
	}

	if prov.TokenEstimate == (contextpack.TokenEstimate{}) && ref.Content != "" {
		prov.TokenEstimate, prov.TokenEstimator = estimateReferenceContent(Options{}, []byte(ref.Content))
	}

	if prov.TokenEstimator == "" && prov.TokenEstimate != (contextpack.TokenEstimate{}) {
		prov.TokenEstimator = referenceEstimatorSummary(contextpack.DefaultEstimator().Profile())
	}

	return prov
}

func withDefaultReferencePolicyDecision(prov ReferenceProvenance, truncated bool) ReferenceProvenance {
	if prov.PolicyDecision == "" {
		decision, reason := loadedDecision(truncated)
		prov.PolicyDecision = decision
		prov.PolicyReason = reason
	}

	if prov.PolicyReason == "" {
		_, prov.PolicyReason = loadedDecision(truncated)
	}

	if prov.PolicyReasonCode == "" {
		prov.PolicyReasonCode = ReferenceReasonCode(prov.PolicyDecision, prov.PolicyReason)
	}

	return prov
}

// ReferenceReasonCode returns the stable machine-readable code for a reference
// policy decision and human-readable reason.
func ReferenceReasonCode(decision, reason string) string {
	decision = strings.ToLower(strings.TrimSpace(decision))
	if decision == "" {
		decision = "unknown"
	}

	reason = strings.ToLower(strings.TrimSpace(reason))

	for _, matcher := range referenceReasonCodeMatchers() {
		if containsAll(reason, matcher.needles...) {
			return decision + "." + matcher.code
		}
	}

	return decision + ".unspecified"
}

type reasonCodeMatcher struct {
	code    string
	needles []string
}

func referenceReasonCodeMatchers() []reasonCodeMatcher {
	return []reasonCodeMatcher{
		{code: "allowed", needles: []string{"allowed by policy"}},
		{code: "allowed", needles: []string{"resolved inside root"}},
		{code: "byte_limit", needles: []string{"byte limit"}},
		{code: "empty_reference", needles: []string{"empty reference"}},
		{code: "max_total_bytes", needles: []string{"max_total_bytes"}},
		{code: "outside_allowed_roots", needles: []string{"outside allowed local roots"}},
		{code: "absolute_requires_local_roots", needles: []string{"requires an explicit local_roots"}},
		{code: "root_escape", needles: []string{"escapes root"}},
		{code: "binary", needles: []string{"binary"}},
		{code: "content_type", needles: []string{"content-type"}},
		{code: "scheme", needles: []string{"scheme"}},
		{code: "host", needles: []string{"host"}},
		{code: "private_network", needles: []string{"private network"}},
		{code: "redirect", needles: []string{"redirect"}},
		{code: "http_status", needles: []string{"http "}},
		{code: "request_aborted", needles: []string{"aborted"}},
		{code: "omitted", needles: []string{"omitted"}},
		{code: "glob_no_match", needles: []string{"glob matched no files"}},
		{code: "io_error", needles: []string{"stat"}},
		{code: "io_error", needles: []string{"read"}},
		{code: "io_error", needles: []string{"open"}},
		{code: "io_error", needles: []string{"resolve"}},
		{code: "io_error", needles: []string{"fetch"}},
	}
}

func containsAll(text string, needles ...string) bool {
	for _, needle := range needles {
		if !strings.Contains(text, needle) {
			return false
		}
	}

	return true
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
