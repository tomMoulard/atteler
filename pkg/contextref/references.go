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
	"net/netip"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/tommoulard/atteler/pkg/contextpack"
	"github.com/tommoulard/atteler/pkg/eval"
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
	referenceRedactedValue         = "[REDACTED]"
)

var defaultAllowedContentTypes = []string{
	"text/*",
	"application/json",
	"application/xml",
	"application/x-yaml",
	"application/yaml",
	"application/toml",
}

var (
	referenceURLInTextPattern   = regexp.MustCompile(`(?i)[a-z][a-z0-9+.-]*://[^\s"'<>]+`)
	referenceURLUserinfoPattern = regexp.MustCompile(`(?i)\b([a-z][a-z0-9+.-]*://)([^/?#\s"'<>@]+@)`)
)

// blockedReferenceIPRanges is deliberately conservative: configured URL
// references are prompt-ingestion inputs, not a general-purpose HTTP client, so
// special-use, private, loopback, link-local, multicast, and transition
// prefixes are blocked unless policy explicitly opts into private networks.
var blockedReferenceIPRanges = mustReferenceIPPrefixes(
	"0.0.0.0/8",
	"10.0.0.0/8",
	"100.64.0.0/10",
	"127.0.0.0/8",
	"169.254.0.0/16",
	"172.16.0.0/12",
	"192.0.0.0/24",
	"192.0.2.0/24",
	"192.168.0.0/16",
	"192.88.99.0/24",
	"198.18.0.0/15",
	"198.51.100.0/24",
	"203.0.113.0/24",
	"224.0.0.0/4",
	"240.0.0.0/4",
	"::/128",
	"::/96",
	"::1/128",
	"64:ff9b::/96",
	"64:ff9b:1::/48",
	"100::/64",
	"2001::/23",
	"2001:db8::/32",
	"2002::/16",
	"3fff::/20",
	"fc00::/7",
	"fe80::/10",
	"ff00::/8",
)

// skipDirs is the set of directory names that are skipped when walking a
// directory reference. These are build artifacts, VCS metadata, and
// dependency caches that are almost never useful as style references.
var skipDirs = map[string]bool{
	".git":         true,
	"node_modules": true,
	"__pycache__":  true,
}

// ReferencePolicy defines the trust boundary for configured references.
// Local references are limited to Options.Root plus LocalRoots. Absolute local
// references additionally require AllowAbsolutePaths. Remote references must
// match AllowedSchemes, AllowedHosts, and port rules, must not resolve to a
// private/local network unless AllowPrivateNetworks is true, must satisfy the
// redirect limit, and must return an allowed Content-Type.
type ReferencePolicy struct {
	AllowedSchemes       []string
	DeniedSchemes        []string
	AllowedHosts         []string
	DeniedHosts          []string
	AllowedPorts         []int
	DeniedPorts          []int
	LocalRoots           []string
	DeniedLocalRoots     []string
	AllowedGlobs         []string
	DeniedGlobs          []string
	ContentTypes         []string
	MaxRedirects         int
	MaxFiles             int
	AllowAbsolutePaths   bool
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
// a compact, machine-readable audit record. Entries preserves the original
// decision order; outcome-specific slices are kept for callers that need grouped
// access but are omitted from JSON to avoid duplicating entries in audit logs.
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
	Included                      []ReferenceEvent `json:"-"`
	Skipped                       []ReferenceEvent `json:"-"`
	Rejected                      []ReferenceEvent `json:"-"`
	Truncated                     []ReferenceEvent `json:"-"`
}

// LoadedReference describes one resolved reference entry with its content.
//
//nolint:govet // field order preserves the historical API shape first.
type LoadedReference struct {
	// Source is the original reference string (path or redacted URL).
	Source string
	// Kind is "file" or "url".
	Kind string
	// Content is the sanitized resolved text content.
	Content string
	// Bytes is the raw byte length loaded before sanitization.
	Bytes int
	// Truncated is true when the content was capped by size limits.
	Truncated bool
	// Provenance records policy and source metadata for this reference.
	Provenance ReferenceProvenance
}

type normalizedReferencePolicy struct {
	allowedSchemes       map[string]bool
	deniedSchemes        map[string]bool
	allowedHosts         []string
	deniedHosts          []string
	allowedPorts         map[int]bool
	deniedPorts          map[int]bool
	rootPaths            []string
	localRoots           []string
	deniedLocalRoots     []string
	allowedGlobs         []string
	deniedGlobs          []string
	contentTypes         []string
	maxRedirects         int
	maxFiles             int
	allowAbsolutePaths   bool
	allowPrivateNetworks bool
}

// LoadReferences resolves a list of reference strings (local file paths,
// directory paths, glob patterns, or HTTP/HTTPS URLs) and returns their
// content. Local references must stay under opts.Root or one of the explicit
// policy local roots, and absolute local paths require explicit policy opt-in.
// Remote references are denied unless the reference policy explicitly allows
// their scheme and host, and private-network targets remain blocked unless the
// policy opts in.
//
// Glob patterns (containing *, ?, [, or **) are expanded before loading.
// Directories are walked recursively and each text file is returned as a
// separate LoadedReference. Each entry is subject to the per-file and aggregate
// byte limits from opts. Use LoadReferencesWithManifest when callers need the
// full audit log; this compatibility helper returns an error when any manifest
// entry is rejected.
func LoadReferences(ctx context.Context, refs []string, opts Options) ([]LoadedReference, error) {
	loaded, _, err := LoadReferencesWithReport(ctx, refs, opts)
	return loaded, err
}

// LoadReferencesWithReport resolves references with per-reference policy and
// loading events suitable for CLI diagnostics.
func LoadReferencesWithReport(ctx context.Context, refs []string, opts Options) ([]LoadedReference, []ReferenceEvent, error) {
	loaded, manifest, err := LoadReferencesWithManifest(ctx, refs, opts)
	rejectedErr := manifestRejectedError(manifest)

	if err != nil || rejectedErr != nil {
		return loaded, manifest.Entries, errors.Join(err, rejectedErr)
	}

	return loaded, manifest.Entries, nil
}

// LoadReferencesWithManifest is LoadReferences plus an audit manifest that
// records every included, skipped, truncated, and rejected reference decision.
func LoadReferencesWithManifest(ctx context.Context, refs []string, opts Options) ([]LoadedReference, ReferenceManifest, error) {
	if err := requireReferenceContext(ctx); err != nil {
		return nil, ReferenceManifest{}, err
	}

	if len(refs) == 0 {
		return nil, ReferenceManifest{}, nil
	}

	opts = normalizeOptions(opts)

	policy, err := normalizeReferencePolicy(opts)
	if err != nil {
		policyErr := sanitizedReferencePolicyError(err)

		return nil, rejectedPolicyManifest(refs, opts, policyErr), policyErr
	}

	if len(policy.localRoots) > 0 {
		opts.Root = policy.localRoots[0]
	}

	var (
		out        []LoadedReference
		manifest   ReferenceManifest
		errs       []error
		total      int
		entryCount int
	)

	for _, raw := range refs {
		if err := requireReferenceContext(ctx); err != nil {
			return out, BuildReferenceManifest(manifest.Entries), errors.Join(append(errs, err)...)
		}

		ref := strings.TrimSpace(raw)
		if ref == "" {
			manifest.Append(newReferenceEvent(raw, "", "", opts, ReferenceDecisionSkipped, "empty reference"))
			continue
		}

		if total >= opts.MaxTotalBytes {
			kind, location := referenceKindLocation(ref)
			manifest.Append(newReferenceEvent(ref, kind, location, opts, ReferenceDecisionSkipped, "max_total_bytes already reached"))

			continue
		}

		if policy.maxFiles > 0 && entryCount >= policy.maxFiles {
			kind, location := referenceKindLocation(ref)
			manifest.Append(newReferenceEvent(ref, kind, location, opts, ReferenceDecisionSkipped, "max_files already reached"))

			continue
		}

		loaded, loadEvents, loadErr := loadReference(ctx, ref, opts, policy, total, entryCount)

		manifest.Append(loadEvents...)

		if loadErr != nil {
			errs = append(errs, sanitizedReferenceError(ref, loadErr))
			continue
		}

		for i := range loaded {
			total += loaded[i].Bytes
			entryCount++

			out = append(out, loaded[i])
		}
	}

	return out, BuildReferenceManifest(manifest.Entries), errors.Join(errs...)
}

func rejectedPolicyManifest(refs []string, opts Options, policyErr error) ReferenceManifest {
	var manifest ReferenceManifest

	for _, raw := range refs {
		ref := strings.TrimSpace(raw)
		if ref == "" {
			manifest.Append(newReferenceEvent(raw, "", "", opts, ReferenceDecisionSkipped, "empty reference"))
			continue
		}

		kind, location := referenceKindLocation(ref)
		manifest.Append(newReferenceEvent(ref, kind, location, opts, ReferenceDecisionRejected, policyErr.Error()))
	}

	return BuildReferenceManifest(manifest.Entries)
}

func referenceKindLocation(ref string) (kind, location string) {
	if isURL(ref) {
		return kindURL, referenceLocationRemote
	}

	return kindFile, referenceLocationLocal
}

// Append adds events to the manifest and updates outcome-specific indexes.
func (m *ReferenceManifest) Append(events ...ReferenceEvent) {
	for i := range events {
		event := events[i]

		m.Entries = append(m.Entries, event)

		switch event.PolicyDecision {
		case ReferenceDecisionLoaded:
			m.Included = append(m.Included, event)
		case ReferenceDecisionTruncated:
			m.Included = append(m.Included, event)
			m.Truncated = append(m.Truncated, event)
		case ReferenceDecisionSkipped:
			m.Skipped = append(m.Skipped, event)
		case ReferenceDecisionRejected:
			m.Rejected = append(m.Rejected, event)
		}
	}
}

func manifestRejectedError(manifest ReferenceManifest) error {
	if len(manifest.Rejected) == 0 {
		return nil
	}

	first := manifest.Rejected[0]

	return fmt.Errorf(
		"reference ingestion rejected %d reference(s); first rejection source %q: %s",
		len(manifest.Rejected),
		first.Source,
		first.PolicyReason,
	)
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
			manifest.Included = append(manifest.Included, *event)
			manifest.TotalBytes += event.Bytes
			manifest.TotalEstimatedTokens += event.TokenEstimate.Tokens
			manifest.TotalEstimatedTokenErrorBound += event.TokenEstimate.ErrorBoundTokens
			manifest.TotalEstimatedTokenUpperBound += event.TokenEstimate.UpperBoundTokens

			if event.PolicyDecision == ReferenceDecisionTruncated || event.Truncated {
				manifest.TruncatedCount++
				manifest.Truncated = append(manifest.Truncated, *event)
			}
		case ReferenceDecisionSkipped:
			manifest.SkippedCount++
			manifest.Skipped = append(manifest.Skipped, *event)
		case ReferenceDecisionOmitted:
			manifest.OmittedCount++
		case ReferenceDecisionRejected:
			manifest.RejectedCount++
			manifest.Rejected = append(manifest.Rejected, *event)
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

type sanitizedReferenceLoadError struct {
	message string
}

func (err sanitizedReferenceLoadError) Error() string {
	return err.message
}

func sanitizedReferenceError(ref string, err error) error {
	if err == nil {
		return nil
	}

	message := fmt.Sprintf(
		"reference %q: %s",
		sanitizeReferenceDiagnostic(ref),
		sanitizeReferenceDiagnostic(err.Error()),
	)

	return sanitizedReferenceLoadError{message: message}
}

func sanitizedReferencePolicyError(err error) error {
	if err == nil {
		return nil
	}

	return sanitizedReferenceLoadError{
		message: sanitizeReferenceDiagnostic(err.Error()),
	}
}

// loadReference dispatches a single reference string to the appropriate
// loader: URL, glob pattern, directory, or plain file.
func loadReference(
	ctx context.Context,
	ref string,
	opts Options,
	policy normalizedReferencePolicy,
	total int,
	entryCount int,
) ([]LoadedReference, []ReferenceEvent, error) {
	if isURL(ref) {
		remaining := opts.MaxTotalBytes - total
		limit := min(opts.MaxFileBytes, remaining)

		loaded, event, err := loadURL(ctx, ref, limit, opts, policy)

		return singletonLoaded(loaded, event, err)
	}

	if err := validateAbsoluteReferencePolicy(ref, policy); err != nil {
		event := newReferenceEvent(ref, kindFile, referenceLocationLocal, opts, ReferenceDecisionRejected, err.Error())
		return nil, []ReferenceEvent{event}, err
	}

	if isGlob(ref) {
		return loadGlob(ref, opts, policy, total, entryCount)
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

	policyPath := resolved

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
		return loadDirectory(resolved, ref, opts, policy, total, entryCount, policyPath)
	}

	if !info.Mode().IsRegular() {
		reason := "not a regular file: " + ref
		event := newReferenceEvent(ref, kindFile, referenceLocationLocal, opts, ReferenceDecisionRejected, reason)

		return nil, []ReferenceEvent{event}, fmt.Errorf("not a regular file: %s", ref)
	}

	remaining := opts.MaxTotalBytes - total
	limit := min(opts.MaxFileBytes, remaining)

	rawPolicySource := localGlobPolicySource(policyPath, opts, policy)
	policySource := localGlobPolicySource(resolved, opts, policy)

	if globErr := validateLocalGlobPolicySources(policy, rawPolicySource, policySource); globErr != nil {
		event := newReferenceEvent(ref, kindFile, referenceLocationLocal, opts, ReferenceDecisionRejected, globErr.Error())
		return nil, []ReferenceEvent{event}, globErr
	}

	loaded, event, err := loadSingleFile(resolved, ref, limit, opts)
	if err == nil {
		annotateOutOfRootLocalReference(&loaded, &event, policy, policyPath, resolved)
	}

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
		formattedContent := sanitizeReferenceText(ref.Content).Content
		prov := provenanceForFormat(ref, formattedContent)
		source := sanitizeReferenceSource(ref.Source)

		b.WriteString(`<`)
		b.WriteString(tag)
		b.WriteString(` source="`)
		b.WriteString(escapeAttr(source))

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
		b.WriteString(escapeText(formattedContent))

		if !strings.HasSuffix(formattedContent, "\n") {
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
	rawRoot, err := cleanAbs(opts.Root)
	if err != nil {
		return normalizedReferencePolicy{}, fmt.Errorf("reference policy: root: %w", err)
	}

	root, err := cleanAbsForPolicy(rawRoot)
	if err != nil {
		return normalizedReferencePolicy{}, fmt.Errorf("reference policy: root: %w", err)
	}

	rootPaths := appendUniquePath(nil, root, rawRoot)

	roots, err := normalizeAllowedPolicyRoots(rootPaths, rawRoot, opts.ReferencePolicy.LocalRoots)
	if err != nil {
		return normalizedReferencePolicy{}, err
	}

	deniedRoots, err := normalizePolicyRoots(opts.ReferencePolicy.DeniedLocalRoots, rawRoot)
	if err != nil {
		return normalizedReferencePolicy{}, err
	}

	allowedPorts, err := cleanPortList("allowed_ports", opts.ReferencePolicy.AllowedPorts)
	if err != nil {
		return normalizedReferencePolicy{}, err
	}

	deniedPorts, err := cleanPortList("denied_ports", opts.ReferencePolicy.DeniedPorts)
	if err != nil {
		return normalizedReferencePolicy{}, err
	}

	allowedGlobs, err := cleanGlobList("allowed_globs", opts.ReferencePolicy.AllowedGlobs)
	if err != nil {
		return normalizedReferencePolicy{}, err
	}

	deniedGlobs, err := cleanGlobList("denied_globs", opts.ReferencePolicy.DeniedGlobs)
	if err != nil {
		return normalizedReferencePolicy{}, err
	}

	contentTypes := opts.ReferencePolicy.ContentTypes
	if contentTypes == nil {
		contentTypes = defaultAllowedContentTypes
	}

	maxFiles := opts.ReferencePolicy.MaxFiles
	if maxFiles <= 0 {
		maxFiles = maxDirectoryEntries
	}

	return normalizedReferencePolicy{
		allowedSchemes:       cleanSchemeSet(opts.ReferencePolicy.AllowedSchemes, []string{"https"}),
		deniedSchemes:        cleanSchemeSet(opts.ReferencePolicy.DeniedSchemes, nil),
		allowedHosts:         cleanList(opts.ReferencePolicy.AllowedHosts),
		deniedHosts:          cleanList(opts.ReferencePolicy.DeniedHosts),
		allowedPorts:         allowedPorts,
		deniedPorts:          deniedPorts,
		rootPaths:            rootPaths,
		localRoots:           roots,
		deniedLocalRoots:     deniedRoots,
		allowedGlobs:         allowedGlobs,
		deniedGlobs:          deniedGlobs,
		maxRedirects:         max(0, opts.ReferencePolicy.MaxRedirects),
		maxFiles:             maxFiles,
		contentTypes:         cleanList(contentTypes),
		allowAbsolutePaths:   opts.ReferencePolicy.AllowAbsolutePaths,
		allowPrivateNetworks: opts.ReferencePolicy.AllowPrivateNetworks,
	}, nil
}

func normalizeAllowedPolicyRoots(rootPaths []string, rawRoot string, configuredRoots []string) ([]string, error) {
	roots := append([]string(nil), rootPaths...)

	for _, configuredRoot := range configuredRoots {
		rootVariants, err := normalizePolicyRootVariants(configuredRoot, rawRoot)
		if err != nil {
			return nil, fmt.Errorf("reference policy: local root %q: %w", configuredRoot, err)
		}

		roots = appendUniquePath(roots, rootVariants...)
	}

	return roots, nil
}

func normalizePolicyRoots(values []string, rawRoot string) ([]string, error) {
	out := make([]string, 0, len(values))
	for _, value := range values {
		rootVariants, err := normalizePolicyRootVariants(value, rawRoot)
		if err != nil {
			return nil, fmt.Errorf("reference policy: local root %q: %w", value, err)
		}

		out = appendUniquePath(out, rootVariants...)
	}

	return out, nil
}

func normalizePolicyRootVariants(value, rawRoot string) ([]string, error) {
	value = strings.TrimSpace(value)
	if value == "" {
		return nil, nil
	}

	path := value
	if !filepath.IsAbs(path) {
		path = filepath.Join(rawRoot, filepath.FromSlash(path))
	}

	rawPath, err := cleanAbs(path)
	if err != nil {
		return nil, err
	}

	evaluatedPath, err := cleanAbsForPolicy(rawPath)
	if err != nil {
		return nil, err
	}

	return appendUniquePath(nil, evaluatedPath, rawPath), nil
}

func appendUniquePath(paths []string, values ...string) []string {
	for _, value := range values {
		if value == "" {
			continue
		}

		if !slices.Contains(paths, value) {
			paths = append(paths, value)
		}
	}

	return paths
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

func cleanSchemeSet(values, defaults []string) map[string]bool {
	if values == nil {
		values = defaults
	}

	out := make(map[string]bool, len(values))
	for _, value := range values {
		value = strings.ToLower(strings.TrimSpace(value))
		if value != "" {
			out[value] = true
		}
	}

	return out
}

func cleanGlobList(field string, values []string) ([]string, error) {
	out := make([]string, 0, len(values))
	for _, value := range values {
		value = cleanPolicyGlob(strings.TrimSpace(value))
		if value != "" {
			if err := validatePolicyGlob(value); err != nil {
				return nil, fmt.Errorf("reference policy: %s %q: %w", field, value, err)
			}

			out = append(out, value)
		}
	}

	return out, nil
}

func cleanPolicyGlob(pattern string) string {
	if pattern == "" {
		return ""
	}

	return filepath.ToSlash(filepath.Clean(filepath.FromSlash(pattern)))
}

func validatePolicyGlob(pattern string) error {
	for part := range strings.SplitSeq(pattern, "/") {
		if part == "**" {
			continue
		}

		if _, err := filepath.Match(part, ""); err != nil {
			return fmt.Errorf("invalid glob pattern: %w", err)
		}
	}

	return nil
}

func cleanPortList(field string, values []int) (map[int]bool, error) {
	out := make(map[int]bool, len(values))
	for _, value := range values {
		if value <= 0 || value > 65535 {
			return nil, fmt.Errorf("reference policy: %s contains invalid port %d", field, value)
		}

		out[value] = true
	}

	return out, nil
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

	if !pathInsideAnyRoot(resolved, policy.localRoots) {
		return "", fmt.Errorf("path %q is outside allowed local roots", ref)
	}

	if !pathInsideAnyRoot(containmentPath, policy.localRoots) {
		return "", fmt.Errorf("path %q is outside allowed local roots", ref)
	}

	if pathInsideAnyRoot(resolved, policy.deniedLocalRoots) {
		return "", fmt.Errorf("path %q is inside denied local roots", ref)
	}

	if pathInsideAnyRoot(containmentPath, policy.deniedLocalRoots) {
		return "", fmt.Errorf("path %q is inside denied local roots", ref)
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

	if pathInsideAnyRoot(resolved, policy.deniedLocalRoots) {
		return "", fmt.Errorf("path %q symlink target is inside denied local roots", ref)
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

func validateAbsoluteReferencePolicy(ref string, policy normalizedReferencePolicy) error {
	if !filepath.IsAbs(ref) || policy.allowAbsolutePaths {
		return nil
	}

	return fmt.Errorf("absolute path %q requires reference_policy.allow_absolute_paths", ref)
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
func loadGlob(pattern string, opts Options, policy normalizedReferencePolicy, total, entryCount int) ([]LoadedReference, []ReferenceEvent, error) {
	if err := validateReferenceGlob(pattern); err != nil {
		event := newReferenceEvent(pattern, kindFile, referenceLocationLocal, opts, ReferenceDecisionRejected, err.Error())

		return nil, []ReferenceEvent{event}, err
	}

	rawBase, absBase, absPattern, err := resolveGlobBaseAndPattern(pattern, opts)
	if err != nil {
		event := newReferenceEvent(pattern, kindFile, referenceLocationLocal, opts, ReferenceDecisionRejected, err.Error())

		return nil, []ReferenceEvent{event}, err
	}

	baseEvents, baseErr := validateGlobBasePolicy(pattern, rawBase, absBase, opts, policy)
	if baseErr != nil {
		return nil, baseEvents, baseErr
	}

	matches, skipped, err := expandGlob(absBase, absPattern, policy.deniedLocalRoots)
	if err != nil {
		reason := "expand glob: " + safePathErrorMessage(err)
		event := newReferenceEvent(pattern, kindFile, referenceLocationLocal, opts, ReferenceDecisionRejected, reason)

		return nil, []ReferenceEvent{event}, errors.New(reason)
	}

	events := globSkipEvents(skipped, rawBase, absBase, opts, policy, len(matches))

	if len(matches) == 0 {
		event := newReferenceEvent(pattern, kindFile, referenceLocationLocal, opts, ReferenceDecisionSkipped, "glob matched no files")

		events = append(events, event)

		return nil, events, nil
	}

	var out []LoadedReference

	for _, path := range matches {
		source := globMatchSource(path, rawBase, absBase, opts, policy)
		rawPolicySource, resolvedSource := globPolicySources(path, rawBase, absBase, opts, policy)

		if pathInsideAnyRoot(path, policy.deniedLocalRoots) {
			events = append(events, newReferenceEvent(source, kindFile, referenceLocationLocal, opts, ReferenceDecisionRejected, "file is inside denied local roots"))
			continue
		}

		if err := validateLocalGlobPolicySources(policy, rawPolicySource, resolvedSource); err != nil {
			events = append(events, newReferenceEvent(source, kindFile, referenceLocationLocal, opts, ReferenceDecisionRejected, err.Error()))
			continue
		}

		if policy.maxFiles > 0 && entryCount >= policy.maxFiles {
			events = append(events, newReferenceEvent(source, kindFile, referenceLocationLocal, opts, ReferenceDecisionSkipped, "max_files reached"))
			break
		}

		remaining := opts.MaxTotalBytes - total
		if remaining <= 0 {
			events = append(events, newReferenceEvent(source, kindFile, referenceLocationLocal, opts, ReferenceDecisionSkipped, "max_total_bytes reached"))
			break
		}

		limit := min(opts.MaxFileBytes, remaining)
		loadPath := path
		if isSymlink(path) {
			var skipEvent ReferenceEvent
			var ok bool

			loadPath, skipEvent, ok = symlinkTargetLoadPath(path, source, opts, policy)
			if skipEvent.PolicyDecision != "" {
				events = append(events, skipEvent)
			}

			if !ok {
				continue
			}
		}

		loaded, event, loadErr := loadSingleFile(loadPath, source, limit, opts)
		if loadErr != nil {
			event = withReferenceDecision(event, ReferenceDecisionSkipped, event.PolicyReason)
			events = append(events, event)

			continue // skip unreadable files in a glob
		}

		total += loaded.Bytes
		entryCount++

		if filepath.IsAbs(pattern) {
			annotateAbsoluteLocalReference(&loaded, &event)
		}

		annotateOutOfRootLocalReference(&loaded, &event, policy, rawBase, absBase, path, loadPath)

		out = append(out, loaded)
		events = append(events, event)
	}

	return out, events, nil
}

func globMatchSource(path, rawBase, absBase string, opts Options, policy normalizedReferencePolicy) string {
	rel, err := filepath.Rel(absBase, path)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(os.PathSeparator)) {
		return displayPolicySource(path, opts, policy)
	}

	baseSource := displayPolicySource(rawBase, opts, policy)
	if baseSource == "." {
		return filepath.ToSlash(rel)
	}

	return filepath.ToSlash(filepath.Join(baseSource, rel))
}

func globPolicySources(path, rawBase, absBase string, opts Options, policy normalizedReferencePolicy) (rawSource, resolvedSource string) {
	rawPath := path
	if rel, err := filepath.Rel(absBase, path); err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(os.PathSeparator)) {
		rawPath = filepath.Join(rawBase, rel)
	}

	return localGlobPolicySource(rawPath, opts, policy), localGlobPolicySource(path, opts, policy)
}

func validateReferenceGlob(pattern string) error {
	if err := validatePolicyGlob(filepath.ToSlash(pattern)); err != nil {
		return fmt.Errorf("invalid glob pattern %q: %w", pattern, err)
	}

	return nil
}

func validateGlobBasePolicy(pattern, rawBase, absBase string, opts Options, policy normalizedReferencePolicy) ([]ReferenceEvent, error) {
	for _, base := range []string{rawBase, absBase} {
		if !pathInsideAnyRoot(base, policy.localRoots) {
			return rejectedGlobBase(pattern, opts, "outside allowed local roots")
		}
	}

	for _, base := range []string{rawBase, absBase} {
		if pathInsideAnyRoot(base, policy.deniedLocalRoots) {
			return rejectedGlobBase(pattern, opts, "inside denied local roots")
		}
	}

	return nil, nil
}

func rejectedGlobBase(pattern string, opts Options, reasonDetail string) ([]ReferenceEvent, error) {
	reason := fmt.Sprintf("glob base %q is %s", pattern, reasonDetail)
	event := newReferenceEvent(pattern, kindFile, referenceLocationLocal, opts, ReferenceDecisionRejected, reason)

	return []ReferenceEvent{event}, errors.New(reason)
}

func globSkipEvents(skipped []globSkip, rawBase, absBase string, opts Options, policy normalizedReferencePolicy, matchCount int) []ReferenceEvent {
	events := make([]ReferenceEvent, 0, len(skipped)+matchCount)
	for i := range skipped {
		decision := skipped[i].Decision
		if decision == "" {
			decision = ReferenceDecisionSkipped
		}

		source := globMatchSource(skipped[i].Source, rawBase, absBase, opts, policy)
		events = append(events, newReferenceEvent(source, kindFile, referenceLocationLocal, opts, decision, skipped[i].Reason))
	}

	return events
}

func resolveGlobBaseAndPattern(pattern string, opts Options) (rawBase, base, absPattern string, err error) {
	globBasePath, rest := globBase(pattern)
	globBasePath = resolvePath(globBasePath, opts.Root)

	rawBase, err = cleanAbs(globBasePath)
	if err != nil {
		return "", "", "", err
	}

	base, err = cleanAbsForPolicy(rawBase)
	if err != nil {
		return "", "", "", err
	}

	absPattern = base
	if rest != "" {
		absPattern = filepath.Join(base, filepath.FromSlash(rest))
	}

	return rawBase, base, absPattern, nil
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

func validateLocalGlobPolicy(source string, policy normalizedReferencePolicy) error {
	for _, pattern := range policy.deniedGlobs {
		if matchLocalPolicyGlob(pattern, source) {
			return fmt.Errorf("source %q matches denied_globs pattern %q", source, pattern)
		}

		if parentMatchesLocalPolicyGlob(pattern, source) {
			return fmt.Errorf("source %q is under denied_globs pattern %q", source, pattern)
		}
	}

	if len(policy.allowedGlobs) == 0 {
		return nil
	}

	for _, pattern := range policy.allowedGlobs {
		if matchLocalPolicyGlob(pattern, source) {
			return nil
		}

		if parentMatchesLocalPolicyGlob(pattern, source) {
			return nil
		}
	}

	return fmt.Errorf("source %q is not in allowed_globs", source)
}

func parentMatchesLocalPolicyGlob(pattern, source string) bool {
	source = filepath.ToSlash(filepath.Clean(strings.TrimSpace(source)))
	for {
		parent := filepath.ToSlash(filepath.Dir(source))
		if parent == "/" || parent == source {
			return false
		}

		if matchLocalPolicyGlob(pattern, parent) {
			return true
		}

		if parent == "." {
			return false
		}

		source = parent
	}
}

func validateLocalGlobPolicySources(policy normalizedReferencePolicy, sources ...string) error {
	seen := make(map[string]bool, len(sources))

	for _, source := range sources {
		source = filepath.ToSlash(filepath.Clean(strings.TrimSpace(source)))
		if source == "" || seen[source] {
			continue
		}

		seen[source] = true

		if err := validateLocalGlobPolicy(source, policy); err != nil {
			return err
		}
	}

	return nil
}

func matchLocalPolicyGlob(pattern, source string) bool {
	pattern = filepath.ToSlash(strings.TrimSpace(pattern))
	source = filepath.ToSlash(filepath.Clean(strings.TrimSpace(source)))

	if pattern == "" || source == "" {
		return false
	}

	if matchGlob(pattern, source) {
		return true
	}

	matched, err := filepath.Match(pattern, source)

	return err == nil && matched
}

type globSkip struct {
	Source   string
	Reason   string
	Decision string
}

type globExpansion struct {
	absPattern  string
	deniedRoots []string
	matches     []string
	skipped     []globSkip
}

// expandGlob walks base and returns regular files whose path matches
// absPattern. The pattern may use ** for recursive directory matching. File
// count policy is applied after expansion so denied/rejected matches do not
// consume max_files slots or hide later allowed files.
func expandGlob(base, absPattern string, deniedRoots []string) ([]string, []globSkip, error) {
	expansion := globExpansion{
		absPattern:  filepath.ToSlash(absPattern),
		deniedRoots: deniedRoots,
	}

	err := filepath.WalkDir(base, expansion.walk)
	if err != nil {
		return expansion.matches, expansion.skipped, fmt.Errorf("walk %s: %w", base, err)
	}

	return expansion.matches, expansion.skipped, nil
}

func (g *globExpansion) walk(path string, entry fs.DirEntry, walkErr error) error {
	if walkErr != nil {
		g.skipped = append(g.skipped, globSkip{Source: path, Reason: walkErr.Error()})

		return nil //nolint:nilerr // manifest records inaccessible entries as skipped
	}

	if g.skipDeniedRoot(path) {
		if entry.IsDir() {
			return filepath.SkipDir
		}

		return nil
	}

	if entry.IsDir() {
		return g.skipDirectory(entry, path)
	}

	if entry.Type()&os.ModeSymlink != 0 {
		g.matches = append(g.matches, path)
		return nil
	}

	if !entry.Type().IsRegular() {
		g.skipNonRegular(entry, path)

		return nil
	}

	return g.collectMatch(path)
}

func (g *globExpansion) skipDeniedRoot(path string) bool {
	if !pathInsideAnyRoot(path, g.deniedRoots) {
		return false
	}

	g.skipped = append(g.skipped, globSkip{
		Source:   path,
		Reason:   "entry is inside denied local roots",
		Decision: ReferenceDecisionRejected,
	})

	return true
}

func (g *globExpansion) skipDirectory(entry fs.DirEntry, path string) error {
	if skipDirs[entry.Name()] {
		g.skipped = append(g.skipped, globSkip{Source: path, Reason: "directory skipped by reference policy"})
		return filepath.SkipDir
	}

	return nil
}

func (g *globExpansion) skipNonRegular(entry fs.DirEntry, path string) {
	if shouldReportGlobNonRegularSkip(entry, path, g.absPattern) {
		g.skipped = append(g.skipped, globSkip{Source: path, Reason: nonRegularEntryReason(entry)})
	}
}

func (g *globExpansion) collectMatch(path string) error {
	if !matchGlob(g.absPattern, filepath.ToSlash(path)) {
		return nil
	}

	g.matches = append(g.matches, path)

	return nil
}

func shouldReportGlobNonRegularSkip(entry fs.DirEntry, path, absPattern string) bool {
	return entry.Type()&os.ModeSymlink != 0 || matchGlob(absPattern, filepath.ToSlash(path))
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
func loadDirectory(dir, ref string, opts Options, policy normalizedReferencePolicy, total, entryCount int, auditPaths ...string) ([]LoadedReference, []ReferenceEvent, error) {
	var (
		out    []LoadedReference
		events []ReferenceEvent
	)

	err := filepath.WalkDir(dir, func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			source := directoryEventSource(path, dir, ref, opts, policy)
			events = append(events, newReferenceEvent(source, kindFile, referenceLocationLocal, opts, ReferenceDecisionSkipped, walkErr.Error()))

			return nil // skip inaccessible entries in directory walk
		}

		deniedRootEvent, deniedRoot, deniedRootErr := directoryDeniedRootEvent(path, dir, ref, entry, opts, policy)
		if deniedRoot {
			events = append(events, deniedRootEvent)
			return deniedRootErr
		}

		shouldLoad, skipEvent, skipErr := directoryEntryShouldLoad(path, dir, ref, entry, opts, policy)
		if skipEvent.PolicyDecision != "" {
			events = append(events, skipEvent)
		}

		if skipErr != nil {
			return skipErr
		}

		if !shouldLoad {
			return nil
		}

		source, event, ok := directoryFileSource(path, dir, ref, opts, policy)
		if !ok {
			events = append(events, event)
			return nil
		}

		policySources := directoryPolicySources(path, dir, auditPaths, opts, policy)
		loaded, event, included, stopWalk := loadDirectoryFile(path, source, policySources, opts, policy, total, entryCount, auditPaths)
		events = append(events, event)

		if stopWalk {
			return fs.SkipAll
		}

		if !included {
			return nil
		}

		total += loaded.Bytes
		entryCount++

		out = append(out, loaded)

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

func directoryDeniedRootEvent(path, dir, ref string, entry fs.DirEntry, opts Options, policy normalizedReferencePolicy) (ReferenceEvent, bool, error) {
	if !pathInsideAnyRoot(path, policy.deniedLocalRoots) {
		return ReferenceEvent{}, false, nil
	}

	source := directoryEventSource(path, dir, ref, opts, policy)

	event := newReferenceEvent(source, kindFile, referenceLocationLocal, opts, ReferenceDecisionRejected, "entry is inside denied local roots")
	if entry.IsDir() {
		return event, true, filepath.SkipDir
	}

	return event, true, nil
}

func loadDirectoryFile(
	path string,
	source string,
	policySources []string,
	opts Options,
	policy normalizedReferencePolicy,
	total int,
	entryCount int,
	auditPaths []string,
) (LoadedReference, ReferenceEvent, bool, bool) {
	if err := validateLocalGlobPolicySources(policy, policySources...); err != nil {
		event := newReferenceEvent(source, kindFile, referenceLocationLocal, opts, ReferenceDecisionRejected, err.Error())
		return LoadedReference{}, event, false, false
	}

	if policy.maxFiles > 0 && entryCount >= policy.maxFiles {
		event := newReferenceEvent(source, kindFile, referenceLocationLocal, opts, ReferenceDecisionSkipped, "max_files reached")
		return LoadedReference{}, event, false, true
	}

	remaining := opts.MaxTotalBytes - total
	if remaining <= 0 {
		event := newReferenceEvent(source, kindFile, referenceLocationLocal, opts, ReferenceDecisionSkipped, "max_total_bytes reached")
		return LoadedReference{}, event, false, true
	}

	limit := min(opts.MaxFileBytes, remaining)
	loadPath := path
	var event ReferenceEvent
	if isSymlink(path) {
		var ok bool

		loadPath, event, ok = symlinkTargetLoadPath(path, source, opts, policy)
		if !ok {
			return LoadedReference{}, event, false, false
		}
	}

	loaded, event, loadErr := loadSingleFile(loadPath, source, limit, opts)
	if loadErr != nil {
		event.PolicyDecision = ReferenceDecisionSkipped
		return LoadedReference{}, event, false, false
	}

	paths := append([]string{path, loadPath}, auditPaths...)
	annotateOutOfRootLocalReference(&loaded, &event, policy, paths...)

	return loaded, event, true, false
}

func directoryPolicySources(path, dir string, auditPaths []string, opts Options, policy normalizedReferencePolicy) []string {
	sources := []string{localGlobPolicySource(path, opts, policy)}

	rel, err := filepath.Rel(dir, path)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(os.PathSeparator)) {
		return sources
	}

	for _, auditPath := range auditPaths {
		if auditPath == "" {
			continue
		}

		sources = append(sources, localGlobPolicySource(filepath.Join(auditPath, rel), opts, policy))
	}

	return sources
}

func directoryEntryShouldLoad(path, dir, ref string, entry fs.DirEntry, opts Options, policy normalizedReferencePolicy) (bool, ReferenceEvent, error) {
	if entry.IsDir() {
		if skipDirs[entry.Name()] {
			source := directoryEventSource(path, dir, ref, opts, policy)
			event := newReferenceEvent(source, kindFile, referenceLocationLocal, opts, ReferenceDecisionSkipped, "directory skipped by reference policy")

			return false, event, filepath.SkipDir
		}

		return false, ReferenceEvent{}, nil
	}

	if entry.Type()&os.ModeSymlink != 0 {
		return true, ReferenceEvent{}, nil
	}

	if !entry.Type().IsRegular() {
		source := directoryEventSource(path, dir, ref, opts, policy)
		event := newReferenceEvent(source, kindFile, referenceLocationLocal, opts, ReferenceDecisionSkipped, nonRegularEntryReason(entry))

		return false, event, nil
	}

	return true, ReferenceEvent{}, nil
}

func nonRegularEntryReason(entry fs.DirEntry) string {
	if entry.Type()&os.ModeSymlink != 0 {
		return "symlink entry skipped by reference policy"
	}

	return "non-regular entry skipped by reference policy"
}

func isSymlink(path string) bool {
	info, err := os.Lstat(path)
	if err != nil {
		return false
	}

	return info.Mode()&fs.ModeSymlink != 0
}

func symlinkTargetLoadPath(path, source string, opts Options, policy normalizedReferencePolicy) (string, ReferenceEvent, bool) {
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

func directoryFileSource(path, dir, ref string, opts Options, policy normalizedReferencePolicy) (string, ReferenceEvent, bool) {
	if !pathInsideAnyRoot(path, policy.localRoots) {
		reason := "file outside allowed local roots"
		event := newReferenceEvent(displayPolicySource(path, opts, policy), kindFile, referenceLocationLocal, opts, ReferenceDecisionSkipped, reason)

		return "", event, false
	}

	rel, err := filepath.Rel(dir, path)
	if err != nil {
		event := newReferenceEvent(displayPolicySource(path, opts, policy), kindFile, referenceLocationLocal, opts, ReferenceDecisionSkipped, err.Error())

		return "", event, false
	}

	return filepath.ToSlash(filepath.Join(ref, rel)), ReferenceEvent{}, true
}

func directoryEventSource(path, dir, ref string, opts Options, policy normalizedReferencePolicy) string {
	rel, err := filepath.Rel(dir, path)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(os.PathSeparator)) {
		return displayPolicySource(path, opts, policy)
	}

	if rel == "." {
		return filepath.ToSlash(filepath.Clean(ref))
	}

	return filepath.ToSlash(filepath.Join(ref, rel))
}

func annotateOutOfRootLocalReference(loaded *LoadedReference, event *ReferenceEvent, policy normalizedReferencePolicy, paths ...string) {
	if loaded.Source != "" && filepath.IsAbs(loaded.Source) {
		annotateAbsoluteLocalReference(loaded, event)
	}

	if referenceOutsidePolicyRoot(policy, paths...) {
		event.PolicyReason = appendPolicyReason(event.PolicyReason, "outside root allowed by reference policy local_roots")
	}

	loaded.Provenance.PolicyReason = event.PolicyReason
}

func referenceOutsidePolicyRoot(policy normalizedReferencePolicy, paths ...string) bool {
	if len(policy.rootPaths) == 0 {
		return false
	}

	for _, path := range paths {
		if path != "" && !pathInsideAnyRoot(path, policy.rootPaths) {
			return true
		}
	}

	return false
}

func annotateAbsoluteLocalReference(loaded *LoadedReference, event *ReferenceEvent) {
	event.PolicyReason = appendPolicyReason(event.PolicyReason, "absolute path allowed by reference policy")
	loaded.Provenance.PolicyReason = event.PolicyReason
}

// ---------------------------------------------------------------------------
// Single-file loading with binary detection
// ---------------------------------------------------------------------------

// loadSingleFile reads a single file up to limit bytes and returns it as a
// LoadedReference. Binary files (detected by null bytes in the first 512
// bytes) are rejected.
func loadSingleFile(path, source string, limit int, opts Options) (LoadedReference, ReferenceEvent, error) {
	content, probe, truncated, err := readReferenceFile(path, limit)
	if err != nil {
		reason := "read file: " + safePathErrorMessage(err)
		event := newReferenceEvent(source, kindFile, referenceLocationLocal, opts, ReferenceDecisionRejected, reason)

		return LoadedReference{}, event, errors.New(reason)
	}

	if isReferenceBinary(probe) {
		reason := "binary file: " + source
		event := newReferenceEvent(source, kindFile, referenceLocationLocal, opts, ReferenceDecisionRejected, reason)

		return LoadedReference{}, event, errors.New(reason)
	}

	sanitized := sanitizeReferenceBytes(content)
	sanitizedBytes := []byte(sanitized.Content)
	rawBytes := len(content)
	decision, reason := loadedDecision(truncated)
	reason = reasonWithSanitization(reason, sanitized)
	event := newReferenceEvent(source, kindFile, referenceLocationLocal, opts, decision, reason)
	event.Bytes = rawBytes
	event.Truncated = truncated
	event.DigestSHA256 = digestHex(sanitizedBytes)
	event.TokenEstimate, event.TokenEstimator = estimateReferenceContent(opts, sanitizedBytes)

	return LoadedReference{
		Source:     source,
		Kind:       kindFile,
		Content:    sanitized.Content,
		Bytes:      rawBytes,
		Truncated:  truncated,
		Provenance: provenanceFromEvent(event),
	}, event, nil
}

func readReferenceFile(path string, limit int) (content, probe []byte, truncated bool, err error) {
	if limit <= 0 {
		return nil, nil, false, nil
	}

	file, err := os.Open(path)
	if err != nil {
		return nil, nil, false, fmt.Errorf("open: %w", err)
	}
	defer file.Close()

	return readReferenceLimited(file, limit)
}

func readReferenceLimited(reader io.Reader, limit int) (content, probe []byte, truncated bool, err error) {
	if limit <= 0 {
		return nil, nil, false, nil
	}

	readLimit := max(limit+1, binaryProbeBytes)

	data, err := io.ReadAll(io.LimitReader(reader, int64(readLimit)))
	if err != nil {
		return nil, nil, false, fmt.Errorf("read: %w", err)
	}

	content = data

	truncated = len(data) > limit
	if truncated {
		content = data[:limit]
	}

	return content, data, truncated, nil
}

// isBinary returns true if data is not valid UTF-8 text or has a null byte in
// the first binaryProbeBytes, which is the same null-byte heuristic git uses.
func isBinary(data []byte) bool {
	if !utf8.Valid(data) {
		return true
	}

	return isReferenceBinary(data)
}

// isReferenceBinary returns true if configured reference data looks binary.
// Configured references sanitize invalid UTF-8 before prompt formatting, but
// still reject NUL-bearing content as binary.
func isReferenceBinary(data []byte) bool {
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
	return strings.Contains(ref, "://")
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
	return referenceURLInTextPattern.ReplaceAllStringFunc(text, func(candidate string) string {
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
	defer client.CloseIdleConnections()

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

	data, probe, truncated, err := readReferenceLimited(resp.Body, limit)
	if err != nil {
		reason := fmt.Sprintf("read body: %v", err)
		event := newReferenceEvent(source, kindURL, referenceLocationRemote, opts, ReferenceDecisionRejected, reason)
		withURLResolvedSource(&event, source, responseURL(resp))

		return LoadedReference{}, event, fmt.Errorf("read body: %w", err)
	}

	if isReferenceBinary(probe) {
		reason := "binary response body"
		event := newReferenceEvent(source, kindURL, referenceLocationRemote, opts, ReferenceDecisionRejected, reason)
		withURLResolvedSource(&event, source, responseURL(resp))

		return LoadedReference{}, event, errors.New(reason)
	}

	sanitized := sanitizeReferenceBytes(data)
	sanitizedBytes := []byte(sanitized.Content)
	rawBytes := len(data)
	decision, reason := loadedDecision(truncated)
	reason = reasonWithSanitization(reason, sanitized)
	event := newReferenceEvent(source, kindURL, referenceLocationRemote, opts, decision, reason)
	withURLResolvedSource(&event, source, responseURL(resp))
	event.Bytes = rawBytes
	event.Truncated = truncated
	event.DigestSHA256 = digestHex(sanitizedBytes)
	event.TokenEstimate, event.TokenEstimator = estimateReferenceContent(opts, sanitizedBytes)

	return LoadedReference{
		Source:     source,
		Kind:       kindURL,
		Content:    sanitized.Content,
		Bytes:      rawBytes,
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
	transport := &http.Transport{
		Proxy:                  nil,
		DialContext:            safeReferenceDialContext(policy),
		DisableCompression:     true,
		ForceAttemptHTTP2:      false,
		IdleConnTimeout:        30 * time.Second,
		ResponseHeaderTimeout:  urlFetchTimeout,
		TLSHandshakeTimeout:    urlFetchTimeout,
		ExpectContinueTimeout:  time.Second,
		MaxResponseHeaderBytes: 64 * 1024,
	}

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

	if policy.deniedSchemes[scheme] {
		return fmt.Errorf("scheme %q is denied", parsed.Scheme)
	}

	if !policy.allowedSchemes[scheme] {
		return fmt.Errorf("scheme %q is not allowed", parsed.Scheme)
	}

	if parsed.User != nil {
		return errors.New("URL userinfo is not allowed")
	}

	if err := validateURLHostSyntax(parsed); err != nil {
		return err
	}

	host, hostWithPort := canonicalURLHost(parsed)
	if host == "" {
		return errors.New("URL host is required")
	}

	if len(policy.allowedHosts) == 0 {
		return fmt.Errorf("host %q rejected: no allowed_hosts configured", host)
	}

	if hostAllowed(host, hostWithPort, policy.deniedHosts) {
		return fmt.Errorf("host %q is in denied_hosts", host)
	}

	if !hostAllowed(host, hostWithPort, policy.allowedHosts) {
		return fmt.Errorf("host %q is not in allowed_hosts", host)
	}

	if err := validateURLPrivateIPLiteral(host, policy); err != nil {
		return err
	}

	if err := validateURLPort(parsed, policy); err != nil {
		return err
	}

	return nil
}

func validateURLPrivateIPLiteral(host string, policy normalizedReferencePolicy) error {
	if policy.allowPrivateNetworks {
		return nil
	}

	ip := parseHostIPLiteral(host)
	if ip == nil || !isBlockedNetworkIP(ip) {
		return nil
	}

	return fmt.Errorf("private network address %s blocked", ip.String())
}

func parseHostIPLiteral(host string) net.IP {
	if ip := net.ParseIP(host); ip != nil {
		return ip
	}

	baseHost, _, hasZone := strings.Cut(host, "%")
	if hasZone {
		if ip := net.ParseIP(baseHost); ip != nil {
			return ip
		}
	} else {
		baseHost = host
	}

	return parseLegacyIPv4Literal(baseHost)
}

// parseLegacyIPv4Literal recognizes inet_aton-style IPv4 forms such as
// 2130706433, 0x7f000001, and 127.1. Some OS resolvers normalize these to
// loopback/private addresses, so block them before DNS or dialing.
func parseLegacyIPv4Literal(host string) net.IP {
	decimal := parseIPv4Literal(host, parseDecimalIPv4Part)
	legacy := parseIPv4Literal(host, parseLegacyIPv4Part)

	for _, candidate := range []net.IP{decimal, legacy} {
		if candidate != nil && isBlockedNetworkIP(candidate) {
			return candidate
		}
	}

	if decimal != nil {
		return decimal
	}

	return legacy
}

func parseIPv4Literal(host string, parsePart func(string) (uint64, bool)) net.IP {
	if host == "" || strings.Contains(host, ":") {
		return nil
	}

	values, ok := parseIPv4Values(host, parsePart)
	if !ok {
		return nil
	}

	addr, ok := legacyIPv4Address(values)
	if !ok {
		return nil
	}

	return ipv4FromUint64(addr)
}

func parseIPv4Values(host string, parsePart func(string) (uint64, bool)) ([]uint64, bool) {
	parts := strings.Split(host, ".")
	if len(parts) > 4 {
		return nil, false
	}

	values := make([]uint64, 0, len(parts))
	for _, part := range parts {
		value, ok := parsePart(part)
		if !ok {
			return nil, false
		}

		values = append(values, value)
	}

	return values, true
}

func legacyIPv4Address(values []uint64) (uint64, bool) {
	partMasks := [][]uint64{
		nil,
		{0xffffffff},
		{0xff, 0xffffff},
		{0xff, 0xff, 0xffff},
		{0xff, 0xff, 0xff, 0xff},
	}
	partShifts := [][]uint{
		nil,
		{0},
		{24, 0},
		{24, 16, 0},
		{24, 16, 8, 0},
	}

	if len(values) == 0 || len(values) >= len(partMasks) {
		return 0, false
	}

	masks := partMasks[len(values)]
	shifts := partShifts[len(values)]

	var addr uint64

	for i, value := range values {
		if value > masks[i] {
			return 0, false
		}

		addr |= value << shifts[i]
	}

	return addr, true
}

//nolint:gosec // addr is assembled only after IPv4 part bounds checks.
func ipv4FromUint64(addr uint64) net.IP {
	return net.IPv4(byte(addr>>24), byte(addr>>16), byte(addr>>8), byte(addr))
}

func parseDecimalIPv4Part(part string) (uint64, bool) {
	return parseIPv4Part(part, 10, part)
}

func parseLegacyIPv4Part(part string) (uint64, bool) {
	if part == "" || strings.HasPrefix(part, "+") || strings.HasPrefix(part, "-") {
		return 0, false
	}

	base := 10
	digits := part

	if strings.HasPrefix(part, "0x") || strings.HasPrefix(part, "0X") {
		base = 16
		digits = part[2:]
	} else if len(part) > 1 && part[0] == '0' {
		base = 8
	}

	if digits == "" {
		return 0, false
	}

	return parseIPv4Part(part, base, digits)
}

func parseIPv4Part(raw string, base int, digits string) (uint64, bool) {
	if raw == "" || strings.HasPrefix(raw, "+") || strings.HasPrefix(raw, "-") || digits == "" {
		return 0, false
	}

	value, err := strconv.ParseUint(digits, base, 32)
	if err != nil {
		return 0, false
	}

	return value, true
}

func validateURLHostSyntax(parsed *url.URL) error {
	host := parsed.Host
	if host == "" {
		return nil
	}

	if strings.HasPrefix(host, "[") {
		end := strings.LastIndex(host, "]")
		if end == -1 {
			return fmt.Errorf("invalid URL host %q", host)
		}

		tail := host[end+1:]
		if tail == "" {
			return nil
		}

		if !strings.HasPrefix(tail, ":") {
			return fmt.Errorf("invalid URL host %q", host)
		}

		if parsed.Port() == "" {
			return fmt.Errorf("invalid URL port %q", strings.TrimPrefix(tail, ":"))
		}

		return nil
	}

	if !strings.Contains(host, ":") {
		return nil
	}

	if strings.Count(host, ":") == 1 {
		rawPort := host[strings.LastIndex(host, ":")+1:]
		if parsed.Port() == "" {
			return fmt.Errorf("invalid URL port %q", rawPort)
		}

		return nil
	}

	return fmt.Errorf("invalid URL host %q: IPv6 literals must be bracketed", host)
}

func validateURLPort(parsed *url.URL, policy normalizedReferencePolicy) error {
	port, explicit, err := effectiveURLPort(parsed)
	if err != nil {
		return err
	}

	if policy.deniedPorts[port] {
		return fmt.Errorf("port %d is denied", port)
	}

	if len(policy.allowedPorts) > 0 {
		if !policy.allowedPorts[port] {
			return fmt.Errorf("port %d is not in allowed_ports", port)
		}

		return nil
	}

	if !explicit || isDefaultSchemePort(strings.ToLower(parsed.Scheme), port) {
		return nil
	}

	host, hostWithPort := canonicalURLHost(parsed)

	if exactHostPortAllowed(host, hostWithPort, policy.allowedHosts) {
		return nil
	}

	return fmt.Errorf("port %d is not allowed", port)
}

func canonicalURLHost(parsed *url.URL) (host, hostWithPort string) {
	host = canonicalHostName(parsed.Hostname())
	hostWithPort = host

	port, _, err := effectiveURLPort(parsed)
	if err == nil {
		if strings.Contains(host, ":") {
			hostWithPort = "[" + host + "]:" + strconv.Itoa(port)
		} else {
			hostWithPort = host + ":" + strconv.Itoa(port)
		}
	}

	return host, hostWithPort
}

func canonicalHostName(host string) string {
	return strings.TrimRight(strings.ToLower(strings.TrimSpace(host)), ".")
}

func effectiveURLPort(parsed *url.URL) (port int, explicit bool, err error) {
	rawPort := parsed.Port()
	if rawPort == "" {
		defaultPort, ok := defaultPortForScheme(strings.ToLower(parsed.Scheme))
		if !ok {
			return 0, false, fmt.Errorf("scheme %q has no default port", parsed.Scheme)
		}

		return defaultPort, false, nil
	}

	parsedPort, err := strconv.Atoi(rawPort)
	if err != nil || parsedPort <= 0 || parsedPort > 65535 {
		return 0, true, fmt.Errorf("invalid URL port %q", rawPort)
	}

	return parsedPort, true, nil
}

func defaultPortForScheme(scheme string) (int, bool) {
	switch scheme {
	case "http":
		return 80, true
	case "https":
		return 443, true
	default:
		return 0, false
	}
}

func isDefaultSchemePort(scheme string, port int) bool {
	defaultPort, ok := defaultPortForScheme(scheme)
	return ok && port == defaultPort
}

func hostAllowed(host, hostWithPort string, allowedHosts []string) bool {
	host = canonicalHostName(host)
	hostWithPort = strings.ToLower(strings.TrimSpace(hostWithPort))

	for _, allowed := range allowedHosts {
		allowed = canonicalPolicyHostPattern(allowed)
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

func canonicalPolicyHostPattern(pattern string) string {
	pattern = strings.ToLower(strings.TrimSpace(pattern))
	if pattern == "" || pattern == "*" {
		return pattern
	}

	if domain, ok := strings.CutPrefix(pattern, "*."); ok {
		return "*." + canonicalHostName(domain)
	}

	if strings.HasPrefix(pattern, "[") {
		end := strings.LastIndex(pattern, "]")
		if end == -1 {
			return pattern
		}

		host := canonicalHostName(pattern[1:end])

		tail := pattern[end+1:]
		if tail == "" {
			return host
		}

		if strings.HasPrefix(tail, ":") {
			return "[" + host + "]" + tail
		}

		return pattern
	}

	if strings.Count(pattern, ":") == 1 {
		host, port, ok := strings.Cut(pattern, ":")
		if ok {
			return canonicalHostName(host) + ":" + port
		}
	}

	return canonicalHostName(pattern)
}

func exactHostPortAllowed(host, hostWithPort string, allowedHosts []string) bool {
	if !strings.Contains(hostWithPort, ":") {
		return false
	}

	for _, allowed := range allowedHosts {
		allowed = canonicalPolicyHostPattern(allowed)
		if allowed == "" || allowed == "*" || strings.HasPrefix(allowed, "*.") {
			continue
		}

		if allowed == hostWithPort && allowed != host {
			return true
		}
	}

	return false
}

func isBlockedNetworkIP(ip net.IP) bool {
	if ip == nil {
		return true
	}

	addr, ok := netip.AddrFromSlice(ip)
	if !ok {
		return true
	}

	addr = addr.Unmap()
	for _, prefix := range blockedReferenceIPRanges {
		if prefix.Contains(addr) {
			return true
		}
	}

	return false
}

func mustReferenceIPPrefixes(values ...string) []netip.Prefix {
	out := make([]netip.Prefix, 0, len(values))
	for _, value := range values {
		prefix, err := netip.ParsePrefix(value)
		if err != nil {
			panic(fmt.Sprintf("invalid blocked reference IP range %q: %v", value, err))
		}

		out = append(out, prefix)
	}

	return out
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

func displayPolicySource(path string, opts Options, policy normalizedReferencePolicy) string {
	for _, root := range policy.rootPaths {
		if !pathInsideRoot(path, root) {
			continue
		}

		return displaySource(path, root)
	}

	return displaySource(path, opts.Root)
}

func localGlobPolicySource(path string, opts Options, policy normalizedReferencePolicy) string {
	if !filepath.IsAbs(path) {
		return path
	}

	if pathInsideAnyRoot(path, policy.rootPaths) {
		return displayPolicySource(path, opts, policy)
	}

	for _, root := range policy.localRoots {
		if !pathInsideRoot(path, root) {
			continue
		}

		return displaySource(path, root)
	}

	return displayPolicySource(path, opts, policy)
}

func newReferenceEvent(source, kind, location string, opts Options, decision, reason string) ReferenceEvent {
	sanitizedReason := sanitizeReferenceDiagnostic(reason)

	return ReferenceEvent{
		Source:           sanitizeReferenceDiagnostic(source),
		Kind:             sanitizeReferenceDiagnostic(kind),
		Scope:            sanitizeReferenceDiagnostic(referenceScope(opts)),
		Location:         sanitizeReferenceDiagnostic(location),
		FetchedAt:        time.Now().UTC(),
		PolicyDecision:   sanitizeReferenceDiagnostic(decision),
		PolicyReason:     sanitizedReason,
		PolicyReasonCode: ReferenceReasonCode(decision, sanitizedReason),
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

type referenceSanitization struct {
	Content         string
	ControlRunes    int
	Redacted        bool
	InvalidEncoding bool
}

func sanitizeReferenceBytes(data []byte) referenceSanitization {
	text := string(data)
	result := sanitizeReferenceText(text)
	result.InvalidEncoding = !utf8.Valid(data)

	return result
}

func sanitizeReferenceText(text string) referenceSanitization {
	result := referenceSanitization{
		Content:         text,
		InvalidEncoding: !utf8.ValidString(text),
	}

	if result.InvalidEncoding {
		result.Content = strings.ToValidUTF8(result.Content, "�")
	}

	result.Content, result.ControlRunes = sanitizeControlRunes(result.Content)

	redacted := eval.Redact(result.Content)
	result.Redacted = redacted != result.Content
	result.Content = redacted

	return result
}

func sanitizeControlRunes(value string) (sanitized string, count int) {
	sanitized = strings.Map(func(r rune) rune {
		switch r {
		case '\n', '\r', '\t':
			return r
		}

		if r < 0x20 || r == 0x7f || (r >= 0x80 && r <= 0x9f) {
			count++
			return '�'
		}

		return r
	}, value)

	return sanitized, count
}

func reasonWithSanitization(reason string, sanitized referenceSanitization) string {
	var details []string
	if sanitized.Redacted {
		details = append(details, "redacted sensitive content")
	}

	if sanitized.ControlRunes > 0 {
		details = append(details, "sanitized control characters")
	}

	if sanitized.InvalidEncoding {
		details = append(details, "sanitized invalid UTF-8")
	}

	if len(details) == 0 {
		return reason
	}

	return reason + "; " + strings.Join(details, "; ")
}

func appendPolicyReason(reason, detail string) string {
	if reason == "" {
		return detail
	}

	if detail == "" || strings.Contains(reason, detail) {
		return reason
	}

	return reason + "; " + detail
}

func sanitizeReferenceDiagnostic(value string) string {
	matches := referenceURLInTextPattern.FindAllStringIndex(value, -1)
	if len(matches) == 0 {
		return sanitizeReferenceDiagnosticPlain(value)
	}

	var (
		b    strings.Builder
		last int
	)

	for _, match := range matches {
		b.WriteString(sanitizeReferenceDiagnosticPlain(value[last:match[0]]))
		b.WriteString(sanitizeReferenceSource(value[match[0]:match[1]]))

		last = match[1]
	}

	b.WriteString(sanitizeReferenceDiagnosticPlain(value[last:]))

	return b.String()
}

func sanitizeReferenceSource(source string) string {
	parsed, err := url.Parse(source)
	if err != nil || parsed.Scheme == "" || parsed.Host == "" {
		return sanitizeMalformedReferenceSource(source)
	}

	if parsed.User != nil {
		parsed.User = url.User(referenceRedactedValue)
	}

	parsed.Host = sanitizeReferenceDiagnosticPlain(parsed.Host)
	parsed.Path = sanitizeReferenceDiagnosticPlain(parsed.Path)
	parsed.RawPath = ""
	parsed.Fragment = sanitizeReferenceDiagnosticPlain(parsed.Fragment)

	parsed.RawQuery = sanitizeParsedReferenceQuery(parsed.Query()).Encode()

	return parsed.String()
}

func sanitizeParsedReferenceQuery(query url.Values) url.Values {
	sanitized := make(url.Values, len(query))
	for key, values := range query {
		sensitive := sensitiveQueryKey(key)
		sanitizedKey := sanitizeReferenceQueryKey(key, key, sensitive)
		sanitizedValues := make([]string, len(values))

		for i := range values {
			if sensitive {
				sanitizedValues[i] = referenceRedactedValue
				continue
			}

			sanitizedValues[i] = sanitizeReferenceDiagnosticPlain(values[i])
		}

		sanitized[sanitizedKey] = append(sanitized[sanitizedKey], sanitizedValues...)
	}

	return sanitized
}

func sanitizeMalformedReferenceSource(source string) string {
	source = referenceURLUserinfoPattern.ReplaceAllString(source, "${1}[REDACTED]@")

	prefix, queryAndFragment, hasQuery := strings.Cut(source, "?")
	if !hasQuery {
		return sanitizeReferenceDiagnosticPlain(source)
	}

	query := queryAndFragment
	fragment := ""
	hasFragment := false

	if beforeFragment, afterFragment, ok := strings.Cut(queryAndFragment, "#"); ok {
		query = beforeFragment
		fragment = afterFragment
		hasFragment = true
	}

	sanitized := sanitizeReferenceDiagnosticPlain(prefix) + "?" + sanitizeRawReferenceQuery(query)
	if hasFragment {
		sanitized += "#" + sanitizeReferenceDiagnosticPlain(fragment)
	}

	return sanitized
}

func sanitizeRawReferenceQuery(query string) string {
	if query == "" {
		return ""
	}

	parts := strings.Split(query, "&")
	for i, part := range parts {
		key, value, hasValue := strings.Cut(part, "=")

		keyForPolicy := key
		if decodedKey, err := url.QueryUnescape(key); err == nil {
			keyForPolicy = decodedKey
		}

		sensitive := sensitiveQueryKey(keyForPolicy)
		sanitizedKey := sanitizeReferenceQueryKey(key, keyForPolicy, sensitive)

		if !hasValue {
			parts[i] = sanitizedKey
			continue
		}

		if sensitive {
			parts[i] = sanitizedKey + "=[REDACTED]"
			continue
		}

		parts[i] = sanitizedKey + "=" + sanitizeReferenceDiagnosticPlain(value)
	}

	return strings.Join(parts, "&")
}

func sanitizeReferenceQueryKey(rawKey, decodedKey string, sensitive bool) string {
	if !sensitive {
		return sanitizeReferenceDiagnosticPlain(rawKey)
	}

	decoded := sanitizeReferenceDiagnosticPlain(decodedKey)
	if decoded != decodedKey && strings.Contains(decoded, referenceRedactedValue) {
		return decoded
	}

	if knownSensitiveQueryKey(decodedKey) {
		return sanitizeReferenceDiagnosticPlain(rawKey)
	}

	return referenceRedactedValue
}

func knownSensitiveQueryKey(key string) bool {
	normalized := normalizeSensitiveQueryKey(key)
	known := map[string]bool{
		"api_key":               true,
		"apikey":                true,
		"access_token":          true,
		"auth":                  true,
		"auth_token":            true,
		"authorization":         true,
		"aws_access_key_id":     true,
		"awsaccesskeyid":        true,
		"client_secret":         true,
		"cookie":                true,
		"credential":            true,
		"jwt":                   true,
		"key":                   true,
		"password":              true,
		"passwd":                true,
		"secret":                true,
		"session":               true,
		"session_id":            true,
		"sig":                   true,
		"signature":             true,
		"token":                 true,
		"x_amz_credential":      true,
		"x_amz_signature":       true,
		"x_api_key":             true,
		"x_aws_access_key_id":   true,
		"x_awsaccesskeyid":      true,
		"x_amz_security_token":  true,
		"aws_session_token":     true,
		"aws_secret_access_key": true,
	}

	return known[normalized]
}

func normalizeSensitiveQueryKey(key string) string {
	replacer := strings.NewReplacer("-", "_", ".", "_")

	return strings.ToLower(replacer.Replace(strings.TrimSpace(key)))
}

func sensitiveQueryKey(key string) bool {
	normalized := normalizeSensitiveQueryKey(key)

	for _, marker := range []string{"token", "secret", "password", "passwd", "api_key", "apikey", "access_key", "accesskey", "accesskeyid", "auth", "credential", "signature", "session", "cookie"} {
		if strings.Contains(normalized, marker) {
			return true
		}
	}

	for _, marker := range []string{"key", "sig", "jwt"} {
		if normalized == marker || strings.HasPrefix(normalized, marker+"_") || strings.HasSuffix(normalized, "_"+marker) {
			return true
		}
	}

	return false
}

func sanitizeReferenceDiagnosticPlain(value string) string {
	value = strings.ToValidUTF8(value, "�")

	value = strings.Map(func(r rune) rune {
		if r < 0x20 || r == 0x7f || (r >= 0x80 && r <= 0x9f) {
			return '�'
		}

		return r
	}, value)

	if decoded, err := url.QueryUnescape(value); err == nil && decoded != value {
		decoded = strings.ToValidUTF8(decoded, "�")
		if eval.Redact(decoded) != decoded {
			return referenceRedactedValue
		}
	}

	return eval.Redact(value)
}

func provenanceForFormat(ref LoadedReference, formattedContent string) ReferenceProvenance {
	prov := ref.Provenance

	prov = withDefaultReferenceScopeAndLocation(prov, ref.Kind)
	prov = withDefaultReferenceSizeDigestAndTokens(prov, ref, formattedContent)
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

func withDefaultReferenceSizeDigestAndTokens(prov ReferenceProvenance, ref LoadedReference, formattedContent string) ReferenceProvenance {
	if prov.Size == 0 && ref.Bytes > 0 {
		prov.Size = ref.Bytes
	}

	if prov.Size == 0 && formattedContent != "" {
		prov.Size = len(formattedContent)
	}

	if prov.DigestSHA256 == "" && formattedContent != "" {
		prov.DigestSHA256 = digestHex([]byte(formattedContent))
	}

	if prov.TokenEstimate == (contextpack.TokenEstimate{}) && formattedContent != "" {
		prov.TokenEstimate, prov.TokenEstimator = estimateReferenceContent(Options{}, []byte(formattedContent))
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

	prov.Scope = sanitizeReferenceDiagnostic(prov.Scope)
	prov.Location = sanitizeReferenceDiagnostic(prov.Location)
	prov.DigestSHA256 = sanitizeReferenceDiagnostic(prov.DigestSHA256)
	prov.PolicyDecision = sanitizeReferenceDiagnostic(prov.PolicyDecision)
	prov.PolicyReason = sanitizeReferenceDiagnostic(prov.PolicyReason)

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
		{code: "generated_skill", needles: []string{"generated skill"}},
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
