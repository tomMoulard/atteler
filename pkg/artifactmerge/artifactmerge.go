// Package artifactmerge captures session artifacts with provenance and aggregates them into
// machine-readable bundles with a Markdown export for humans.
package artifactmerge

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/tommoulard/atteler/pkg/autonomy"
	"github.com/tommoulard/atteler/pkg/session"
	"github.com/tommoulard/atteler/pkg/shell"
)

const (
	// BundleSchemaVersion is bumped when the machine-readable merge contract changes.
	BundleSchemaVersion = 1

	// SeverityWarning marks non-fatal structured merge warnings.
	SeverityWarning = "warning"
	// SeverityError marks warnings that should fail downstream review gates.
	SeverityError = "error"

	// WarningDuplicate marks a duplicate artifact path that was skipped.
	WarningDuplicate = "duplicate_artifact"
	// WarningPathEscape marks an artifact path that resolves outside the merge root.
	WarningPathEscape = "path_escape"
	// WarningReadFailed marks an artifact that could not be read.
	WarningReadFailed = "read_failed"
	// WarningStatFailed marks an artifact whose metadata could not be read.
	WarningStatFailed = "stat_failed"
	// WarningNotFile marks a directory or non-regular artifact.
	WarningNotFile = "not_file"
	// WarningTooLarge marks an artifact larger than the configured byte limit.
	WarningTooLarge = "too_large"
	// WarningNonText marks an artifact that is not UTF-8 text.
	WarningNonText = "non_text"
	// WarningHashMismatch marks an artifact whose content changed after record time.
	WarningHashMismatch = "hash_mismatch"
	// WarningMissingHash marks an artifact that lacks a record-time content hash.
	WarningMissingHash = "missing_recorded_hash"
	// WarningSizeMismatch marks an artifact whose size changed after record time.
	WarningSizeMismatch = "size_mismatch"
	// WarningConflict marks multiple artifacts with incompatible logical targets.
	WarningConflict = "conflict"
)

// Warning records why an artifact could not be trusted or was skipped while building a bundle.
//
//nolint:govet // Field order keeps warning identity, provenance, and comparison values grouped for JSON/YAML readers.
type Warning struct {
	Path            string    `json:"path" yaml:"path"`
	Code            string    `json:"code" yaml:"code"`
	Severity        string    `json:"severity" yaml:"severity"`
	Reason          string    `json:"reason" yaml:"reason"`
	SourceSessionID string    `json:"source_session_id,omitempty" yaml:"source_session_id,omitempty"`
	SourceAgent     string    `json:"source_agent,omitempty" yaml:"source_agent,omitempty"`
	SourceCommand   string    `json:"source_command,omitempty" yaml:"source_command,omitempty"`
	SourceTool      string    `json:"source_tool,omitempty" yaml:"source_tool,omitempty"`
	SourceCommit    string    `json:"source_commit,omitempty" yaml:"source_commit,omitempty"`
	WorktreePath    string    `json:"worktree_path,omitempty" yaml:"worktree_path,omitempty"`
	WorktreeBranch  string    `json:"worktree_branch,omitempty" yaml:"worktree_branch,omitempty"`
	WorktreeBase    string    `json:"worktree_base,omitempty" yaml:"worktree_base,omitempty"`
	ReviewStatus    string    `json:"review_status,omitempty" yaml:"review_status,omitempty"`
	Expected        string    `json:"expected,omitempty" yaml:"expected,omitempty"`
	Actual          string    `json:"actual,omitempty" yaml:"actual,omitempty"`
	RecordedAt      time.Time `json:"recorded_at,omitzero" yaml:"recorded_at,omitempty"`
	SourceTurn      int       `json:"source_turn,omitempty" yaml:"source_turn,omitempty"`
	WorktreeDirty   bool      `json:"worktree_dirty,omitempty" yaml:"worktree_dirty,omitempty"`
}

// Provenance records where a merged artifact entry came from.
//
//nolint:govet // Field order keeps provenance metadata grouped for JSON/YAML readers.
type Provenance struct {
	SourceSessionID string    `json:"source_session_id,omitempty" yaml:"source_session_id,omitempty"`
	SourceAgent     string    `json:"source_agent,omitempty" yaml:"source_agent,omitempty"`
	SourceCommand   string    `json:"source_command,omitempty" yaml:"source_command,omitempty"`
	SourceTool      string    `json:"source_tool,omitempty" yaml:"source_tool,omitempty"`
	SourceCommit    string    `json:"source_commit,omitempty" yaml:"source_commit,omitempty"`
	WorktreePath    string    `json:"worktree_path,omitempty" yaml:"worktree_path,omitempty"`
	WorktreeBranch  string    `json:"worktree_branch,omitempty" yaml:"worktree_branch,omitempty"`
	WorktreeBase    string    `json:"worktree_base,omitempty" yaml:"worktree_base,omitempty"`
	RecordedAt      time.Time `json:"recorded_at,omitzero" yaml:"recorded_at,omitempty"`
	SourceTurn      int       `json:"source_turn,omitempty" yaml:"source_turn,omitempty"`
	WorktreeDirty   bool      `json:"worktree_dirty,omitempty" yaml:"worktree_dirty,omitempty"`
}

// Entry is a normalized artifact included in a merge bundle.
//
//nolint:govet // Field order keeps identity, provenance, and content grouped for JSON/YAML readers.
type Entry struct {
	Provenance   Provenance `json:"provenance" yaml:"provenance"`
	Path         string     `json:"path" yaml:"path"`
	LogicalPath  string     `json:"logical_path,omitempty" yaml:"logical_path,omitempty"`
	Kind         string     `json:"kind,omitempty" yaml:"kind,omitempty"`
	Source       string     `json:"source_agent,omitempty" yaml:"source_agent,omitempty"`
	Summary      string     `json:"summary,omitempty" yaml:"summary,omitempty"`
	ReviewStatus string     `json:"review_status,omitempty" yaml:"review_status,omitempty"`
	SHA256       string     `json:"sha256" yaml:"sha256"`
	Content      string     `json:"content" yaml:"content"`
	ConsumedAt   *time.Time `json:"consumed_at,omitempty" yaml:"consumed_at,omitempty"`
	SizeBytes    int64      `json:"size_bytes" yaml:"size_bytes"`
	Consumed     bool       `json:"consumed" yaml:"consumed"`
}

// ConflictEntry identifies one artifact participating in a merge conflict.
type ConflictEntry struct {
	Path            string `json:"path" yaml:"path"`
	SHA256          string `json:"sha256" yaml:"sha256"`
	SourceAgent     string `json:"source_agent,omitempty" yaml:"source_agent,omitempty"`
	SourceSessionID string `json:"source_session_id,omitempty" yaml:"source_session_id,omitempty"`
	SourceCommand   string `json:"source_command,omitempty" yaml:"source_command,omitempty"`
	SourceTool      string `json:"source_tool,omitempty" yaml:"source_tool,omitempty"`
	SourceCommit    string `json:"source_commit,omitempty" yaml:"source_commit,omitempty"`
	WorktreePath    string `json:"worktree_path,omitempty" yaml:"worktree_path,omitempty"`
	WorktreeBranch  string `json:"worktree_branch,omitempty" yaml:"worktree_branch,omitempty"`
	WorktreeBase    string `json:"worktree_base,omitempty" yaml:"worktree_base,omitempty"`
	ReviewStatus    string `json:"review_status,omitempty" yaml:"review_status,omitempty"`
	SourceTurn      int    `json:"source_turn,omitempty" yaml:"source_turn,omitempty"`
	WorktreeDirty   bool   `json:"worktree_dirty,omitempty" yaml:"worktree_dirty,omitempty"`
}

// Conflict records multiple artifacts that claim the same logical target with different content.
type Conflict struct {
	Target   string          `json:"target" yaml:"target"`
	Severity string          `json:"severity" yaml:"severity"`
	Reason   string          `json:"reason" yaml:"reason"`
	Entries  []ConflictEntry `json:"entries" yaml:"entries"`
}

// BundleSummary gives downstream review gates a stable pass/fail summary.
type BundleSummary struct {
	InputCount    int `json:"input_count" yaml:"input_count"`
	IncludedCount int `json:"included_count" yaml:"included_count"`
	SkippedCount  int `json:"skipped_count" yaml:"skipped_count"`
	WarningCount  int `json:"warning_count" yaml:"warning_count"`
	ErrorCount    int `json:"error_count" yaml:"error_count"`
	ConflictCount int `json:"conflict_count" yaml:"conflict_count"`
}

// Bundle is the machine-readable artifact merge contract.
//
//nolint:govet // Schema first keeps emitted JSON easy to identify for downstream tools.
type Bundle struct {
	SchemaVersion int           `json:"schema_version" yaml:"schema_version"`
	OK            bool          `json:"ok" yaml:"ok"`
	Summary       BundleSummary `json:"summary" yaml:"summary"`
	Entries       []Entry       `json:"entries" yaml:"entries"`
	Warnings      []Warning     `json:"warnings,omitempty" yaml:"warnings,omitempty"`
	Conflicts     []Conflict    `json:"conflicts,omitempty" yaml:"conflicts,omitempty"`
}

// Result contains the human Markdown export plus structured provenance and warnings.
type Result struct {
	Markdown   string
	Entries    []Entry
	Warnings   []Warning
	Conflicts  []Conflict
	InputCount int
}

// CaptureOptions controls record-time artifact validation and provenance capture.
type CaptureOptions struct {
	LogicalPath   string
	SourceCommand string
	SourceTool    string
	Autonomy      string
	AuditDir      string
	MaxBytes      int64
}

type safeRoot struct {
	abs  string
	real string
}

type textFile struct {
	Content   string
	SHA256    string
	SizeBytes int64
}

type gitMetadata struct {
	Commit string
	Branch string
	Dirty  bool
}

// Bundle returns the machine-readable merge contract for downstream review gates.
func (r *Result) Bundle() Bundle {
	summary := r.Summary()

	return Bundle{
		SchemaVersion: BundleSchemaVersion,
		OK:            summary.ErrorCount == 0 && summary.ConflictCount == 0,
		Summary:       summary,
		Entries:       copyEntries(r.Entries),
		Warnings:      append([]Warning(nil), r.Warnings...),
		Conflicts:     copyConflicts(r.Conflicts),
	}
}

// MarkConsumedAt records when included entries were consumed and refreshes the Markdown export.
func (r *Result) MarkConsumedAt(consumedAt time.Time) {
	if r == nil || len(r.Entries) == 0 {
		return
	}

	if consumedAt.IsZero() {
		consumedAt = time.Now().UTC()
	} else {
		consumedAt = consumedAt.UTC()
	}

	for i := range r.Entries {
		if r.Entries[i].ConsumedAt != nil && !r.Entries[i].ConsumedAt.IsZero() {
			continue
		}

		copied := consumedAt
		r.Entries[i].ConsumedAt = &copied
	}

	r.Markdown = renderMarkdown(r.Entries)
}

func copyEntries(entries []Entry) []Entry {
	copied := append([]Entry(nil), entries...)
	for i := range copied {
		copied[i].ConsumedAt = cloneTime(copied[i].ConsumedAt)
	}

	return copied
}

func copyConflicts(conflicts []Conflict) []Conflict {
	copied := append([]Conflict(nil), conflicts...)
	for i := range copied {
		copied[i].Entries = append([]ConflictEntry(nil), copied[i].Entries...)
	}

	return copied
}

// Summary returns deterministic counts for review-gate decisions.
func (r *Result) Summary() BundleSummary {
	inputCount := r.InputCount
	if inputCount == 0 && len(r.Entries) > 0 {
		inputCount = len(r.Entries)
	}

	skippedCount := max(0, inputCount-len(r.Entries))

	summary := BundleSummary{
		InputCount:    inputCount,
		IncludedCount: len(r.Entries),
		SkippedCount:  skippedCount,
		WarningCount:  len(r.Warnings),
		ConflictCount: len(r.Conflicts),
	}

	for i := range r.Warnings {
		if r.Warnings[i].Severity == SeverityError {
			summary.ErrorCount++
		}
	}

	return summary
}

// JSON renders the machine-readable merge bundle deterministically.
func (r *Result) JSON() ([]byte, error) {
	data, err := json.MarshalIndent(r.Bundle(), "", "  ")
	if err != nil {
		return nil, fmt.Errorf("artifactmerge: marshal bundle: %w", err)
	}

	return append(data, '\n'), nil
}

// CaptureArtifact validates an artifact under root at record time and returns a provenance-rich record.
func CaptureArtifact(
	ctx context.Context,
	root string,
	sessionState session.Session,
	path string,
	kind string,
	summary string,
	sourceAgent string,
	options CaptureOptions,
) (session.Artifact, error) {
	if ctx == nil {
		return session.Artifact{}, errors.New("artifactmerge: context is required")
	}

	kind = strings.TrimSpace(kind)
	if kind == "" {
		return session.Artifact{}, errors.New("artifactmerge: kind is required")
	}

	if options.MaxBytes <= 0 {
		return session.Artifact{}, errors.New("artifactmerge: maxBytes must be positive")
	}

	rootInfo, err := resolveRoot(root)
	if err != nil {
		return session.Artifact{}, err
	}

	relPath, fullPath, ok := normalizePath(rootInfo.abs, path)
	if !ok {
		return session.Artifact{}, errors.New("artifactmerge: path escapes root")
	}

	file, warning, ok := readArtifact(rootInfo, relPath, fullPath, options.MaxBytes)
	if !ok {
		return session.Artifact{}, fmt.Errorf("artifactmerge: validate %s: %s", warning.Path, warning.Reason)
	}

	metadata := captureGitMetadata(ctx, rootInfo.abs, options)
	logicalPath := normalizeLogicalPath(options.LogicalPath)

	if logicalPath == "" {
		logicalPath = relPath
	}

	worktreeBranch := metadata.Branch
	if worktreeBranch == "" {
		worktreeBranch = strings.TrimSpace(sessionState.WorktreeBranch)
	}

	return session.Artifact{
		CreatedAt:       time.Now().UTC(),
		Path:            relPath,
		LogicalPath:     logicalPath,
		Kind:            kind,
		Summary:         strings.TrimSpace(summary),
		SourceAgent:     strings.TrimSpace(sourceAgent),
		SourceSessionID: strings.TrimSpace(sessionState.ID),
		SourceTurn:      len(sessionState.Messages),
		SourceCommand:   strings.TrimSpace(options.SourceCommand),
		SourceTool:      strings.TrimSpace(options.SourceTool),
		SourceCommit:    metadata.Commit,
		WorktreePath:    rootInfo.abs,
		WorktreeBranch:  worktreeBranch,
		WorktreeBase:    strings.TrimSpace(sessionState.WorktreeBase),
		WorktreeDirty:   metadata.Dirty,
		SHA256:          file.SHA256,
		SizeBytes:       file.SizeBytes,
	}, nil
}

// Merge reads text artifacts under root and renders a provenance-preserving bundle plus Markdown export.
func Merge(root string, artifacts []session.Artifact, maxBytes int64) (Result, error) {
	if maxBytes <= 0 {
		return Result{}, errors.New("artifactmerge: maxBytes must be positive")
	}

	rootInfo, err := resolveRoot(root)
	if err != nil {
		return Result{}, err
	}

	seen := make(map[string]struct{}, len(artifacts))
	result := Result{InputCount: len(artifacts)}

	for i := range artifacts {
		artifact := &artifacts[i]

		relPath, fullPath, ok := normalizePath(rootInfo.abs, artifact.Path)
		if !ok {
			result.Warnings = append(result.Warnings, warningWithProvenance(
				newWarning(strings.TrimSpace(artifact.Path), WarningPathEscape, SeverityError, "path escapes root"),
				artifact,
			))

			continue
		}

		if _, exists := seen[relPath]; exists {
			result.Warnings = append(result.Warnings, warningWithProvenance(
				newWarning(relPath, WarningDuplicate, SeverityWarning, "duplicate artifact"),
				artifact,
			))

			continue
		}

		seen[relPath] = struct{}{}

		file, warning, ok := readArtifact(rootInfo, relPath, fullPath, maxBytes)
		if !ok {
			result.Warnings = append(result.Warnings, warningWithProvenance(warning, artifact))
			continue
		}

		if warning, ok := integrityWarning(relPath, artifact, file); ok {
			result.Warnings = append(result.Warnings, warningWithProvenance(warning, artifact))
			continue
		}

		result.Warnings = append(result.Warnings, integrityMetadataWarnings(relPath, artifact)...)
		result.Entries = append(result.Entries, entryFor(artifact, relPath, file))
	}

	sort.Slice(result.Entries, func(i, j int) bool {
		if result.Entries[i].Path == result.Entries[j].Path {
			return result.Entries[i].SHA256 < result.Entries[j].SHA256
		}

		return result.Entries[i].Path < result.Entries[j].Path
	})

	result.Conflicts = detectConflicts(result.Entries)
	for _, conflict := range result.Conflicts {
		result.Warnings = append(result.Warnings, Warning{
			Path:     conflict.Target,
			Code:     WarningConflict,
			Severity: conflict.Severity,
			Reason:   conflict.Reason,
		})
	}

	result.Markdown = renderMarkdown(result.Entries)

	return result, nil
}

func resolveRoot(root string) (safeRoot, error) {
	if strings.TrimSpace(root) == "" {
		return safeRoot{}, errors.New("artifactmerge: root is required")
	}

	rootAbs, err := filepath.Abs(root)
	if err != nil {
		return safeRoot{}, fmt.Errorf("artifactmerge: resolve root: %w", err)
	}

	rootAbs = filepath.Clean(rootAbs)

	rootReal, err := filepath.EvalSymlinks(rootAbs)
	if err != nil {
		return safeRoot{}, fmt.Errorf("artifactmerge: resolve root symlinks: %w", err)
	}

	return safeRoot{abs: rootAbs, real: filepath.Clean(rootReal)}, nil
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

func readArtifact(rootInfo safeRoot, relPath, fullPath string, maxBytes int64) (textFile, Warning, bool) {
	resolvedPath, safe, err := resolveSafePath(rootInfo.real, fullPath)
	if err != nil {
		return textFile{}, newWarning(relPath, WarningReadFailed, SeverityError, "read failed: "+err.Error()), false
	}

	if !safe {
		return textFile{}, newWarning(relPath, WarningPathEscape, SeverityError, "path escapes root"), false
	}

	file, warning, ok := readTextFile(resolvedPath, maxBytes)
	if !ok {
		warning.Path = relPath
	}

	return file, warning, ok
}

func readTextFile(path string, maxBytes int64) (textFile, Warning, bool) {
	file, err := os.Open(path)
	if err != nil {
		return textFile{}, newWarning("", WarningReadFailed, SeverityError, "read failed: "+err.Error()), false
	}
	defer file.Close()

	info, err := file.Stat()
	if err != nil {
		return textFile{}, newWarning("", WarningStatFailed, SeverityError, "stat failed: "+err.Error()), false
	}

	if info.IsDir() {
		return textFile{}, newWarning("", WarningNotFile, SeverityError, "not a file"), false
	}

	if info.Size() > maxBytes {
		return textFile{}, newWarning("", WarningTooLarge, SeverityError, fmt.Sprintf("too large: %d bytes exceeds limit %d", info.Size(), maxBytes)), false
	}

	data, err := io.ReadAll(io.LimitReader(file, maxBytes+1))
	if err != nil {
		return textFile{}, newWarning("", WarningReadFailed, SeverityError, "read failed: "+err.Error()), false
	}

	if int64(len(data)) > maxBytes {
		return textFile{}, newWarning("", WarningTooLarge, SeverityError, fmt.Sprintf("too large: exceeds limit %d", maxBytes)), false
	}

	if !utf8.Valid(data) || bytes.Contains(data, []byte{0}) {
		return textFile{}, newWarning("", WarningNonText, SeverityError, "non-text artifact"), false
	}

	sum := sha256.Sum256(data)

	return textFile{
		Content:   string(data),
		SHA256:    hex.EncodeToString(sum[:]),
		SizeBytes: int64(len(data)),
	}, Warning{}, true
}

func newWarning(path, code, severity, reason string) Warning {
	return Warning{Path: path, Code: code, Severity: severity, Reason: reason}
}

func warningWithProvenance(warning Warning, artifact *session.Artifact) Warning {
	if warning.SourceSessionID == "" {
		warning.SourceSessionID = strings.TrimSpace(artifact.SourceSessionID)
	}

	if warning.SourceAgent == "" {
		warning.SourceAgent = strings.TrimSpace(artifact.SourceAgent)
	}

	if warning.SourceCommand == "" {
		warning.SourceCommand = strings.TrimSpace(artifact.SourceCommand)
	}

	if warning.SourceTool == "" {
		warning.SourceTool = strings.TrimSpace(artifact.SourceTool)
	}

	if warning.SourceCommit == "" {
		warning.SourceCommit = strings.TrimSpace(artifact.SourceCommit)
	}

	if warning.WorktreePath == "" {
		warning.WorktreePath = strings.TrimSpace(artifact.WorktreePath)
	}

	if warning.WorktreeBranch == "" {
		warning.WorktreeBranch = strings.TrimSpace(artifact.WorktreeBranch)
	}

	if warning.WorktreeBase == "" {
		warning.WorktreeBase = strings.TrimSpace(artifact.WorktreeBase)
	}

	if warning.ReviewStatus == "" {
		warning.ReviewStatus = strings.TrimSpace(artifact.ReviewStatus)
	}

	if warning.RecordedAt.IsZero() {
		warning.RecordedAt = artifact.CreatedAt
	}

	if warning.SourceTurn == 0 {
		warning.SourceTurn = artifact.SourceTurn
	}

	if !warning.WorktreeDirty {
		warning.WorktreeDirty = artifact.WorktreeDirty
	}

	return warning
}

func integrityWarning(relPath string, artifact *session.Artifact, file textFile) (Warning, bool) {
	expectedHash := strings.ToLower(strings.TrimSpace(artifact.SHA256))
	if expectedHash != "" && expectedHash != file.SHA256 {
		warning := newWarning(relPath, WarningHashMismatch, SeverityError, "hash mismatch")
		warning.Expected = expectedHash
		warning.Actual = file.SHA256

		return warning, true
	}

	if artifact.SizeBytes != 0 && artifact.SizeBytes != file.SizeBytes {
		warning := newWarning(relPath, WarningSizeMismatch, SeverityError, "size mismatch")
		warning.Expected = strconv.FormatInt(artifact.SizeBytes, 10)
		warning.Actual = strconv.FormatInt(file.SizeBytes, 10)

		return warning, true
	}

	return Warning{}, false
}

func integrityMetadataWarnings(relPath string, artifact *session.Artifact) []Warning {
	if strings.TrimSpace(artifact.SHA256) != "" || !hasRecordProvenance(artifact) {
		return nil
	}

	return []Warning{warningWithProvenance(
		newWarning(relPath, WarningMissingHash, SeverityWarning, "artifact has no record-time hash"),
		artifact,
	)}
}

func hasRecordProvenance(artifact *session.Artifact) bool {
	return strings.TrimSpace(artifact.SourceSessionID) != "" ||
		strings.TrimSpace(artifact.SourceAgent) != "" ||
		!artifact.CreatedAt.IsZero()
}

func entryFor(artifact *session.Artifact, relPath string, file textFile) Entry {
	logicalPath := normalizeLogicalPath(artifact.LogicalPath)
	if logicalPath == "" {
		logicalPath = relPath
	}

	return Entry{
		Path:         relPath,
		LogicalPath:  logicalPath,
		Kind:         strings.TrimSpace(artifact.Kind),
		Source:       strings.TrimSpace(artifact.SourceAgent),
		Summary:      strings.TrimSpace(artifact.Summary),
		ReviewStatus: strings.TrimSpace(artifact.ReviewStatus),
		SHA256:       file.SHA256,
		SizeBytes:    file.SizeBytes,
		Consumed:     true,
		ConsumedAt:   cloneTime(artifact.ConsumedAt),
		Provenance: Provenance{
			RecordedAt:      artifact.CreatedAt,
			SourceSessionID: strings.TrimSpace(artifact.SourceSessionID),
			SourceAgent:     strings.TrimSpace(artifact.SourceAgent),
			SourceTurn:      artifact.SourceTurn,
			SourceCommand:   strings.TrimSpace(artifact.SourceCommand),
			SourceTool:      strings.TrimSpace(artifact.SourceTool),
			SourceCommit:    strings.TrimSpace(artifact.SourceCommit),
			WorktreePath:    strings.TrimSpace(artifact.WorktreePath),
			WorktreeBranch:  strings.TrimSpace(artifact.WorktreeBranch),
			WorktreeBase:    strings.TrimSpace(artifact.WorktreeBase),
			WorktreeDirty:   artifact.WorktreeDirty,
		},
		Content: file.Content,
	}
}

func cloneTime(value *time.Time) *time.Time {
	if value == nil || value.IsZero() {
		return nil
	}

	copied := value.UTC()

	return &copied
}

func normalizeLogicalPath(path string) string {
	path = strings.TrimSpace(path)
	if path == "" {
		return ""
	}

	return filepath.ToSlash(filepath.Clean(path))
}

func detectConflicts(entries []Entry) []Conflict {
	byTarget := make(map[string][]*Entry)

	for i := range entries {
		entry := &entries[i]

		target := entry.LogicalPath
		if target == "" {
			target = entry.Path
		}

		byTarget[target] = append(byTarget[target], entry)
	}

	targets := make([]string, 0, len(byTarget))
	for target := range byTarget {
		targets = append(targets, target)
	}

	sort.Strings(targets)

	conflicts := make([]Conflict, 0)

	for _, target := range targets {
		entriesForTarget := byTarget[target]
		if len(entriesForTarget) < 2 || sameContent(entriesForTarget) {
			continue
		}

		conflictEntries := make([]ConflictEntry, 0, len(entriesForTarget))
		for i := range entriesForTarget {
			entry := entriesForTarget[i]
			conflictEntries = append(conflictEntries, ConflictEntry{
				Path:            entry.Path,
				SHA256:          entry.SHA256,
				SourceAgent:     entry.Source,
				SourceSessionID: entry.Provenance.SourceSessionID,
				SourceTurn:      entry.Provenance.SourceTurn,
				SourceCommand:   entry.Provenance.SourceCommand,
				SourceTool:      entry.Provenance.SourceTool,
				SourceCommit:    entry.Provenance.SourceCommit,
				WorktreePath:    entry.Provenance.WorktreePath,
				WorktreeBranch:  entry.Provenance.WorktreeBranch,
				WorktreeBase:    entry.Provenance.WorktreeBase,
				WorktreeDirty:   entry.Provenance.WorktreeDirty,
				ReviewStatus:    entry.ReviewStatus,
			})
		}

		sort.Slice(conflictEntries, func(i, j int) bool {
			if conflictEntries[i].Path == conflictEntries[j].Path {
				return conflictEntries[i].SHA256 < conflictEntries[j].SHA256
			}

			return conflictEntries[i].Path < conflictEntries[j].Path
		})

		conflicts = append(conflicts, Conflict{
			Target:   target,
			Severity: SeverityError,
			Reason:   "multiple artifacts target the same logical path with different content",
			Entries:  conflictEntries,
		})
	}

	return conflicts
}

func sameContent(entries []*Entry) bool {
	first := entries[0].SHA256
	for i := 1; i < len(entries); i++ {
		if entries[i].SHA256 != first {
			return false
		}
	}

	return true
}

func captureGitMetadata(ctx context.Context, root string, options CaptureOptions) gitMetadata {
	metadata := gitMetadata{}
	audit := artifactGitAuditContext(options)

	if commit, ok := gitOutput(ctx, root, audit, "rev-parse", "HEAD"); ok {
		metadata.Commit = commit
	}

	if branch, ok := gitOutput(ctx, root, audit, "branch", "--show-current"); ok {
		metadata.Branch = branch
	}

	if status, ok := gitOutput(ctx, root, audit, "status", "--porcelain"); ok {
		metadata.Dirty = strings.TrimSpace(status) != ""
	}

	return metadata
}

func artifactGitAuditContext(options CaptureOptions) shell.AuditContext {
	return shell.AuditContext{
		Caller:   "artifactmerge.git_metadata",
		Autonomy: autonomy.Normalize(autonomy.Level(options.Autonomy)).String(),
		AuditDir: options.AuditDir,
	}
}

func gitOutput(ctx context.Context, root string, audit shell.AuditContext, args ...string) (string, bool) {
	result, err := shell.RunCommand(ctx, shell.CommandOptions{
		Program: "git",
		Args:    args,
		Dir:     root,
		Mode:    shell.ModeCaptured,
		Audit:   audit,
	})
	if err != nil {
		return "", false
	}

	return strings.TrimSpace(result.Stdout), true
}

func renderMarkdown(entries []Entry) string {
	var b strings.Builder
	b.WriteString("# Merged Artifacts\n")

	if len(entries) == 0 {
		b.WriteString("\n_No text artifacts included._\n")
		return b.String()
	}

	for i := range entries {
		entry := &entries[i]
		fmt.Fprintf(&b, "\n## %s\n\n", entry.Path)
		writeMetadata(&b, "Path", entry.Path)
		writeMetadata(&b, "Logical path", entry.LogicalPath)
		writeMetadata(&b, "Kind", entry.Kind)
		writeMetadata(&b, "Source", entry.Source)
		writeMetadata(&b, "Summary", entry.Summary)
		writeMetadata(&b, "Review status", entry.ReviewStatus)
		writeMetadata(&b, "SHA-256", entry.SHA256)
		writeMetadata(&b, "Size", strconv.FormatInt(entry.SizeBytes, 10)+" bytes")
		writeMetadata(&b, "Consumed", strconv.FormatBool(entry.Consumed))

		if entry.ConsumedAt != nil && !entry.ConsumedAt.IsZero() {
			writeMetadata(&b, "Consumed at", entry.ConsumedAt.UTC().Format(time.RFC3339))
		}

		writeMetadata(&b, "Source session", entry.Provenance.SourceSessionID)

		if entry.Provenance.SourceTurn != 0 {
			writeMetadata(&b, "Source turn", strconv.Itoa(entry.Provenance.SourceTurn))
		}

		writeMetadata(&b, "Source command", entry.Provenance.SourceCommand)
		writeMetadata(&b, "Source tool", entry.Provenance.SourceTool)
		writeMetadata(&b, "Source commit", entry.Provenance.SourceCommit)
		writeMetadata(&b, "Worktree", entry.Provenance.WorktreePath)
		writeMetadata(&b, "Worktree branch", entry.Provenance.WorktreeBranch)
		writeMetadata(&b, "Worktree base", entry.Provenance.WorktreeBase)

		if entry.Provenance.WorktreeDirty {
			writeMetadata(&b, "Worktree dirty", "true")
		}

		if !entry.Provenance.RecordedAt.IsZero() {
			writeMetadata(&b, "Recorded", entry.Provenance.RecordedAt.UTC().Format(time.RFC3339))
		}

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
