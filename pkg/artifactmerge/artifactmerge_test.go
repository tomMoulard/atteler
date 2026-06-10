package artifactmerge

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/tommoulard/atteler/pkg/llm"
	"github.com/tommoulard/atteler/pkg/session"
	attshell "github.com/tommoulard/atteler/pkg/shell"
)

const testWorktreeBase = "main"

func TestCaptureArtifact_RecordsProvenanceHashAndSize(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	writeFile(t, root, "notes/plan.md", "research notes")

	sessionState := session.New("gpt-test", []llm.Message{
		{Role: llm.RoleUser, Content: "make a plan"},
		{Role: llm.RoleAssistant, Content: "done"},
	})
	sessionState.WorktreePath = filepath.Join(root, "stale-session-worktree")
	sessionState.WorktreeBranch = "atteler/session"
	sessionState.WorktreeBase = testWorktreeBase

	artifact, err := CaptureArtifact(t.Context(), root, sessionState, "./notes/plan.md", " research ", " useful ", " planner ", CaptureOptions{
		MaxBytes:      1024,
		SourceCommand: "record-artifact",
		SourceTool:    "atteler",
	})
	require.NoError(t, err)

	assert.Equal(t, "notes/plan.md", artifact.Path)
	assert.Equal(t, "notes/plan.md", artifact.LogicalPath)
	assert.Equal(t, "research", artifact.Kind)
	assert.Equal(t, "useful", artifact.Summary)
	assert.Equal(t, "planner", artifact.SourceAgent)
	assert.Equal(t, sessionState.ID, artifact.SourceSessionID)
	assert.Equal(t, 2, artifact.SourceTurn)
	assert.Equal(t, "record-artifact", artifact.SourceCommand)
	assert.Equal(t, "atteler", artifact.SourceTool)
	assert.Equal(t, filepath.Clean(root), artifact.WorktreePath)
	assert.Equal(t, "atteler/session", artifact.WorktreeBranch)
	assert.Equal(t, testWorktreeBase, artifact.WorktreeBase)
	assert.Equal(t, sha256Hex("research notes"), artifact.SHA256)
	assert.Equal(t, int64(len("research notes")), artifact.SizeBytes)
	assert.False(t, artifact.CreatedAt.IsZero())
}

func TestCaptureArtifact_RecordsGitMetadata(t *testing.T) {
	t.Parallel()

	if _, err := exec.LookPath("git"); err != nil {
		t.Skipf("git unavailable: %v", err)
	}

	root := t.TempDir()
	runGit(t, root, "init")
	writeFile(t, root, "README.md", "repo\n")
	runGit(t, root, "add", "README.md")
	runGit(t, root, "-c", "user.name=Atteler Test", "-c", "user.email=atteler@example.com", "-c", "commit.gpgsign=false", "commit", "-m", "initial")
	commit := runGit(t, root, "rev-parse", "HEAD")

	writeFile(t, root, "notes/plan.md", "dirty artifact")
	auditDir := filepath.Join(t.TempDir(), "audit")

	artifact, err := CaptureArtifact(t.Context(), root, session.New("gpt-test", nil), "notes/plan.md", "note", "", "agent", CaptureOptions{
		MaxBytes: 1024,
		Autonomy: "high",
		AuditDir: auditDir,
	})
	require.NoError(t, err)

	assert.Equal(t, commit, artifact.SourceCommit)
	assert.NotEmpty(t, artifact.WorktreeBranch)
	assert.True(t, artifact.WorktreeDirty)
	assert.Equal(t, filepath.Clean(root), artifact.WorktreePath)

	records := readCommandAuditRecords(t, auditDir)
	require.Len(t, records, 6)

	for _, record := range records {
		assert.Equal(t, "artifactmerge.git_metadata", record.Caller)
		assert.Equal(t, "high", record.Autonomy)
		assert.Equal(t, filepath.Clean(root), record.CWD)
	}
}

func TestCaptureArtifact_RejectsUnsafeOrNonTextArtifacts(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(root, "binary.bin"), []byte{0x00, 0x01}, 0o600))

	sessionState := session.New("gpt-test", nil)

	_, err := CaptureArtifact(t.Context(), root, sessionState, "../outside.txt", "text", "", "agent", CaptureOptions{MaxBytes: 1024})
	require.ErrorContains(t, err, "path escapes root")

	_, err = CaptureArtifact(t.Context(), root, sessionState, "missing.txt", "text", "", "agent", CaptureOptions{MaxBytes: 1024})
	require.ErrorContains(t, err, "validate missing.txt")

	_, err = CaptureArtifact(t.Context(), root, sessionState, "binary.bin", "binary", "", "agent", CaptureOptions{MaxBytes: 1024})
	require.ErrorContains(t, err, "non-text artifact")
}

func TestMerge_IncludesTextArtifactsDeterministically(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	writeFile(t, root, "b.md", "second")
	writeFile(t, root, "a.txt", "first with ``` fence\n")

	result, err := Merge(root, []session.Artifact{
		{Path: "b.md", Kind: "patch", Summary: "second summary", SourceAgent: "agent-b", SHA256: sha256Hex("second"), SizeBytes: int64(len("second"))},
		{Path: "./nested/../a.txt", Kind: "note", Summary: "first\nsummary", SourceAgent: "agent-a", SHA256: sha256Hex("first with ``` fence\n"), SizeBytes: int64(len("first with ``` fence\n"))},
	}, 1024)
	require.NoError(t, err)
	require.Empty(t, result.Warnings)
	require.Empty(t, result.Conflicts)
	require.Len(t, result.Entries, 2)

	assert.Equal(t, []string{"a.txt", "b.md"}, []string{result.Entries[0].Path, result.Entries[1].Path})
	assert.Equal(t, sha256Hex("first with ``` fence\n"), result.Entries[0].SHA256)
	assert.Equal(t, int64(len("first with ``` fence\n")), result.Entries[0].SizeBytes)
	assert.Contains(t, result.Markdown, "# Merged Artifacts\n")
	assert.Less(t, strings.Index(result.Markdown, "## a.txt"), strings.Index(result.Markdown, "## b.md"))
	assert.Contains(t, result.Markdown, "- **Kind:** note\n")
	assert.Contains(t, result.Markdown, "- **Source:** agent-a\n")
	assert.Contains(t, result.Markdown, "- **Summary:** first summary\n")
	assert.Contains(t, result.Markdown, "- **SHA-256:** "+sha256Hex("first with ``` fence\n")+"\n")
	assert.Contains(t, result.Markdown, "````text\nfirst with ``` fence\n````\n")
	assert.Contains(t, result.Markdown, "```text\nsecond\n```\n")
}

func TestMerge_RendersMachineReadableBundle(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	writeFile(t, root, "artifact.md", "machine readable")
	createdAt := mustParseTime(t, "2026-05-01T10:00:00Z")
	consumedAt := mustParseTime(t, "2026-05-01T10:05:00Z")

	result, err := Merge(root, []session.Artifact{{
		CreatedAt:       createdAt,
		ConsumedAt:      &consumedAt,
		Path:            "artifact.md",
		Kind:            "research",
		Summary:         "bundle",
		SourceAgent:     "reviewer",
		SourceSessionID: "session-1",
		SourceTurn:      3,
		SourceCommand:   "record-artifact",
		SourceTool:      "atteler",
		SourceCommit:    "abc123",
		WorktreePath:    root,
		WorktreeBranch:  "atteler/session-1",
		WorktreeBase:    testWorktreeBase,
		ReviewStatus:    "approved",
		SHA256:          sha256Hex("machine readable"),
		SizeBytes:       int64(len("machine readable")),
	}}, 1024)
	require.NoError(t, err)

	bundle := result.Bundle()
	require.Equal(t, BundleSchemaVersion, bundle.SchemaVersion)
	assert.True(t, bundle.OK)
	assert.Equal(t, BundleSummary{InputCount: 1, IncludedCount: 1}, bundle.Summary)
	require.Len(t, bundle.Entries, 1)
	entry := bundle.Entries[0]
	assert.Equal(t, "artifact.md", entry.Path)
	assert.Equal(t, "artifact.md", entry.LogicalPath)
	assert.Equal(t, "approved", entry.ReviewStatus)
	assert.Equal(t, "machine readable", entry.Content)
	assert.True(t, entry.Consumed)
	require.NotNil(t, entry.ConsumedAt)
	assert.Equal(t, consumedAt, *entry.ConsumedAt)
	assert.Equal(t, "session-1", entry.Provenance.SourceSessionID)
	assert.Equal(t, 3, entry.Provenance.SourceTurn)
	assert.Equal(t, "record-artifact", entry.Provenance.SourceCommand)
	assert.Equal(t, "atteler", entry.Provenance.SourceTool)
	assert.Equal(t, "abc123", entry.Provenance.SourceCommit)

	data, err := result.JSON()
	require.NoError(t, err)

	var decoded Bundle
	require.NoError(t, json.Unmarshal(data, &decoded))
	assert.Equal(t, bundle.SchemaVersion, decoded.SchemaVersion)
	assert.True(t, decoded.OK)
	assert.Equal(t, BundleSummary{InputCount: 1, IncludedCount: 1}, decoded.Summary)
	assert.Equal(t, entry.SHA256, decoded.Entries[0].SHA256)
	assert.Equal(t, "approved", decoded.Entries[0].ReviewStatus)
	assert.True(t, decoded.Entries[0].Consumed)
	require.NotNil(t, decoded.Entries[0].ConsumedAt)
	assert.Equal(t, consumedAt, *decoded.Entries[0].ConsumedAt)
	assert.Equal(t, "machine readable", decoded.Entries[0].Content)
}

func TestResult_BundleCopiesNestedConflictEntries(t *testing.T) {
	t.Parallel()

	consumedAt := mustParseTime(t, "2026-05-01T10:05:00Z")
	result := Result{
		Entries: []Entry{{
			Path:       "agent-a/decision.md",
			ConsumedAt: &consumedAt,
		}},
		Conflicts: []Conflict{{
			Target:   "docs/decision.md",
			Severity: SeverityError,
			Reason:   "conflict",
			Entries: []ConflictEntry{{
				Path:   "agent-a/decision.md",
				SHA256: "abc123",
			}},
		}},
	}

	bundle := result.Bundle()
	*bundle.Entries[0].ConsumedAt = consumedAt.Add(time.Hour)
	bundle.Conflicts[0].Entries[0].Path = "mutated.md"

	assert.Equal(t, consumedAt, *result.Entries[0].ConsumedAt)
	assert.Equal(t, "agent-a/decision.md", result.Conflicts[0].Entries[0].Path)
}

func TestResult_MarkConsumedAtUpdatesBundleAndMarkdown(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	writeFile(t, root, "artifact.md", "content")

	result, err := Merge(root, []session.Artifact{{
		Path:      "artifact.md",
		Kind:      "research",
		SHA256:    sha256Hex("content"),
		SizeBytes: int64(len("content")),
	}}, 1024)
	require.NoError(t, err)
	require.Len(t, result.Entries, 1)
	assert.Nil(t, result.Entries[0].ConsumedAt)
	assert.NotContains(t, result.Markdown, "Consumed at")

	consumedAt := mustParseTime(t, "2026-05-01T10:05:00Z")
	result.MarkConsumedAt(consumedAt)

	require.NotNil(t, result.Entries[0].ConsumedAt)
	assert.Equal(t, consumedAt, *result.Entries[0].ConsumedAt)
	require.NotNil(t, result.Bundle().Entries[0].ConsumedAt)
	assert.Equal(t, consumedAt, *result.Bundle().Entries[0].ConsumedAt)
	assert.Contains(t, result.Markdown, "- **Consumed at:** 2026-05-01T10:05:00Z\n")
}

func TestMerge_SkipsUnsafeDuplicatesNonTextAndTooLarge(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	writeFile(t, root, "ok.txt", "ok")
	writeFile(t, root, "large.txt", "12345")
	require.NoError(t, os.WriteFile(filepath.Join(root, "binary.bin"), []byte{0xff, 0xfe}, 0o600))

	result, err := Merge(root, []session.Artifact{
		{Path: "ok.txt", Kind: "text"},
		{Path: "./ok.txt", Kind: "text"},
		{Path: "../outside.txt", Kind: "text"},
		{Path: "binary.bin", Kind: "binary"},
		{Path: "large.txt", Kind: "large"},
	}, 4)
	require.NoError(t, err)

	require.Len(t, result.Entries, 1)
	assert.Equal(t, "ok.txt", result.Entries[0].Path)
	assert.Equal(t, "ok", result.Entries[0].Content)
	require.Len(t, result.Warnings, 4)
	assertWarning(t, result.Warnings, "ok.txt", WarningDuplicate, SeverityWarning, "duplicate artifact")
	assertWarning(t, result.Warnings, "../outside.txt", WarningPathEscape, SeverityError, "path escapes root")
	assertWarning(t, result.Warnings, "binary.bin", WarningNonText, SeverityError, "non-text artifact")
	assertWarningContains(t, result.Warnings, "large.txt", WarningTooLarge, SeverityError, "too large")
}

func TestMerge_SkipsHashMismatch(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	writeFile(t, root, "artifact.txt", "original")
	artifact, err := CaptureArtifact(t.Context(), root, session.New("gpt-test", nil), "artifact.txt", "text", "", "agent", CaptureOptions{MaxBytes: 1024})
	require.NoError(t, err)

	artifact.SourceTurn = 7
	artifact.SourceCommand = "record-artifact"
	artifact.SourceTool = "atteler"
	artifact.SourceCommit = "abc123"
	artifact.WorktreeBranch = "feature/hash"
	artifact.WorktreeBase = testWorktreeBase
	artifact.WorktreeDirty = true
	artifact.ReviewStatus = "pending"

	writeFile(t, root, "artifact.txt", "changed")

	result, err := Merge(root, []session.Artifact{artifact}, 1024)
	require.NoError(t, err)
	assert.Empty(t, result.Entries)
	require.Len(t, result.Warnings, 1)
	assertWarning(t, result.Warnings, "artifact.txt", WarningHashMismatch, SeverityError, "hash mismatch")
	assert.Equalf(t, result.Warnings[0].Expected, sha256Hex("original"), "expected original hash warning")
	assert.Equalf(t, result.Warnings[0].Actual, sha256Hex("changed"), "expected changed hash warning")
	assert.Equal(t, "agent", result.Warnings[0].SourceAgent)
	assert.NotEmpty(t, result.Warnings[0].SourceSessionID)
	assert.Equal(t, 7, result.Warnings[0].SourceTurn)
	assert.Equal(t, "record-artifact", result.Warnings[0].SourceCommand)
	assert.Equal(t, "atteler", result.Warnings[0].SourceTool)
	assert.Equal(t, "abc123", result.Warnings[0].SourceCommit)
	assert.Equal(t, filepath.Clean(root), result.Warnings[0].WorktreePath)
	assert.Equal(t, "feature/hash", result.Warnings[0].WorktreeBranch)
	assert.Equal(t, testWorktreeBase, result.Warnings[0].WorktreeBase)
	assert.True(t, result.Warnings[0].WorktreeDirty)
	assert.Equal(t, "pending", result.Warnings[0].ReviewStatus)
	assert.Equal(t, artifact.CreatedAt, result.Warnings[0].RecordedAt)
	assert.False(t, result.Bundle().OK)
	assert.Equal(t, BundleSummary{InputCount: 1, SkippedCount: 1, WarningCount: 1, ErrorCount: 1}, result.Summary())
}

func TestMerge_SkipsSizeMismatchWhenHashIsUnavailable(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	writeFile(t, root, "artifact.txt", "actual content")

	result, err := Merge(root, []session.Artifact{{
		Path:            "artifact.txt",
		Kind:            "text",
		SourceAgent:     "agent",
		SourceSessionID: "session-1",
		SizeBytes:       99,
	}}, 1024)
	require.NoError(t, err)
	assert.Empty(t, result.Entries)
	require.Len(t, result.Warnings, 1)
	assertWarning(t, result.Warnings, "artifact.txt", WarningSizeMismatch, SeverityError, "size mismatch")
	assert.Equal(t, "99", result.Warnings[0].Expected)
	assert.Equal(t, strconv.Itoa(len("actual content")), result.Warnings[0].Actual)
	assert.Equal(t, "session-1", result.Warnings[0].SourceSessionID)
	assert.False(t, result.Bundle().OK)
	assert.Equal(t, BundleSummary{InputCount: 1, SkippedCount: 1, WarningCount: 1, ErrorCount: 1}, result.Summary())
}

func TestMerge_WarnsWhenRecordedArtifactLacksHash(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	writeFile(t, root, "legacy.txt", "legacy content")

	sessionState := session.New("gpt-test", nil)
	require.True(t, sessionState.RecordArtifact("legacy.txt", "text", "legacy record", "agent"))

	result, err := Merge(root, sessionState.Artifacts, 1024)
	require.NoError(t, err)
	require.Len(t, result.Entries, 1)
	assert.Equal(t, "legacy.txt", result.Entries[0].Path)
	assert.Equal(t, sha256Hex("legacy content"), result.Entries[0].SHA256)
	require.Len(t, result.Warnings, 1)
	assertWarning(t, result.Warnings, "legacy.txt", WarningMissingHash, SeverityWarning, "artifact has no record-time hash")
	assert.Equal(t, sessionState.ID, result.Warnings[0].SourceSessionID)
	assert.Equal(t, "agent", result.Warnings[0].SourceAgent)
	assert.True(t, result.Bundle().OK)
	assert.Equal(t, BundleSummary{InputCount: 1, IncludedCount: 1, WarningCount: 1}, result.Summary())
}

func TestMerge_DuplicatePathDoesNotMaskEarlierIntegrityFailure(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	writeFile(t, root, "artifact.txt", "original")
	original, err := CaptureArtifact(t.Context(), root, session.New("gpt-test", nil), "artifact.txt", "text", "", "agent-a", CaptureOptions{MaxBytes: 1024})
	require.NoError(t, err)

	writeFile(t, root, "artifact.txt", "changed")
	changed, err := CaptureArtifact(t.Context(), root, session.New("gpt-test", nil), "artifact.txt", "text", "", "agent-b", CaptureOptions{MaxBytes: 1024})
	require.NoError(t, err)

	result, err := Merge(root, []session.Artifact{original, changed}, 1024)
	require.NoError(t, err)
	assert.Empty(t, result.Entries)
	require.Len(t, result.Warnings, 2)
	assertWarning(t, result.Warnings, "artifact.txt", WarningHashMismatch, SeverityError, "hash mismatch")
	assertWarning(t, result.Warnings, "artifact.txt", WarningDuplicate, SeverityWarning, "duplicate artifact")
	assert.False(t, result.Bundle().OK)
	assert.Equal(t, BundleSummary{InputCount: 2, SkippedCount: 2, WarningCount: 2, ErrorCount: 1}, result.Summary())
}

func TestMerge_WarnsForDeletedArtifact(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	writeFile(t, root, "artifact.txt", "content")
	artifact, err := CaptureArtifact(t.Context(), root, session.New("gpt-test", nil), "artifact.txt", "text", "", "agent", CaptureOptions{MaxBytes: 1024})
	require.NoError(t, err)
	require.NoError(t, os.Remove(filepath.Join(root, "artifact.txt")))

	result, err := Merge(root, []session.Artifact{artifact}, 1024)
	require.NoError(t, err)
	assert.Empty(t, result.Entries)
	require.Len(t, result.Warnings, 1)
	assertWarningContains(t, result.Warnings, "artifact.txt", WarningReadFailed, SeverityError, "read failed")
}

func TestMerge_DetectsLogicalPathConflicts(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	writeFile(t, root, "agent-a/decision.md", "choose redis")
	writeFile(t, root, "agent-b/decision.md", "choose postgres")

	artifactA, err := CaptureArtifact(t.Context(), root, session.New("gpt-test", nil), "agent-a/decision.md", "decision", "", "agent-a", CaptureOptions{
		MaxBytes:    1024,
		LogicalPath: "docs/decision.md",
	})
	require.NoError(t, err)

	artifactA.SourceTurn = 4
	artifactA.SourceCommand = "record-artifact"
	artifactA.SourceTool = "atteler"
	artifactA.SourceCommit = "commit-a"
	artifactA.WorktreeBranch = "branch-a"
	artifactA.WorktreeBase = testWorktreeBase
	artifactA.WorktreeDirty = true
	artifactA.ReviewStatus = "approved"

	artifactB, err := CaptureArtifact(t.Context(), root, session.New("gpt-test", nil), "agent-b/decision.md", "decision", "", "agent-b", CaptureOptions{
		MaxBytes:    1024,
		LogicalPath: "docs/decision.md",
	})
	require.NoError(t, err)

	result, err := Merge(root, []session.Artifact{artifactB, artifactA}, 1024)
	require.NoError(t, err)
	require.Len(t, result.Entries, 2)
	require.Len(t, result.Conflicts, 1)
	conflict := result.Conflicts[0]
	assert.Equal(t, "docs/decision.md", conflict.Target)
	assert.Equal(t, SeverityError, conflict.Severity)
	assert.Len(t, conflict.Entries, 2)
	assert.Equal(t, []string{"agent-a/decision.md", "agent-b/decision.md"}, []string{conflict.Entries[0].Path, conflict.Entries[1].Path})
	assert.Equal(t, 4, conflict.Entries[0].SourceTurn)
	assert.Equal(t, "record-artifact", conflict.Entries[0].SourceCommand)
	assert.Equal(t, "atteler", conflict.Entries[0].SourceTool)
	assert.Equal(t, "commit-a", conflict.Entries[0].SourceCommit)
	assert.Equal(t, "branch-a", conflict.Entries[0].WorktreeBranch)
	assert.Equal(t, testWorktreeBase, conflict.Entries[0].WorktreeBase)
	assert.True(t, conflict.Entries[0].WorktreeDirty)
	assert.Equal(t, "approved", conflict.Entries[0].ReviewStatus)
	assertWarning(t, result.Warnings, "docs/decision.md", WarningConflict, SeverityError, "multiple artifacts target the same logical path with different content")
	assert.False(t, result.Bundle().OK)
	assert.Equal(t, BundleSummary{InputCount: 2, IncludedCount: 2, WarningCount: 1, ErrorCount: 1, ConflictCount: 1}, result.Summary())
}

func TestMerge_SkipsAbsolutePathOutsideRootAndAllowsInsideRoot(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	inside := filepath.Join(root, "inside.txt")
	require.NoError(t, os.WriteFile(inside, []byte("inside"), 0o600))
	outside := filepath.Join(t.TempDir(), "outside.txt")
	require.NoError(t, os.WriteFile(outside, []byte("outside"), 0o600))

	result, err := Merge(root, []session.Artifact{
		{Path: inside, Kind: "text"},
		{Path: outside, Kind: "text"},
	}, 1024)
	require.NoError(t, err)

	require.Len(t, result.Entries, 1)
	assert.Equal(t, "inside.txt", result.Entries[0].Path)
	require.Len(t, result.Warnings, 1)
	assert.Equal(t, outside, result.Warnings[0].Path)
	assert.Equal(t, WarningPathEscape, result.Warnings[0].Code)
	assert.Equal(t, SeverityError, result.Warnings[0].Severity)
	assert.Equal(t, "path escapes root", result.Warnings[0].Reason)
}

func TestMerge_SkipsSymlinkEscape(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	outside := filepath.Join(t.TempDir(), "outside.txt")
	require.NoError(t, os.WriteFile(outside, []byte("outside"), 0o600))

	link := filepath.Join(root, "link.txt")
	if err := os.Symlink(outside, link); err != nil {
		t.Skipf("symlink unavailable: %v", err)
	}

	result, err := Merge(root, []session.Artifact{{Path: "link.txt", Kind: "text"}}, 1024)
	require.NoError(t, err)
	assert.Empty(t, result.Entries)
	require.Len(t, result.Warnings, 1)
	assert.Equal(t, "link.txt", result.Warnings[0].Path)
	assert.Equal(t, WarningPathEscape, result.Warnings[0].Code)
	assert.Equal(t, SeverityError, result.Warnings[0].Severity)
	assert.Equal(t, "path escapes root", result.Warnings[0].Reason)
}

func TestMerge_ValidatesInputsAndRendersEmptyDocument(t *testing.T) {
	t.Parallel()

	_, err := Merge("", nil, 1)
	require.Error(t, err)
	_, err = Merge(t.TempDir(), nil, 0)
	require.Error(t, err)

	result, err := Merge(t.TempDir(), nil, 1)
	require.NoError(t, err)
	assert.Equal(t, "# Merged Artifacts\n\n_No text artifacts included._\n", result.Markdown)
	assert.Empty(t, result.Entries)
	assert.Empty(t, result.Warnings)
	assert.Empty(t, result.Conflicts)
}

func writeFile(t *testing.T, root, relPath, content string) {
	t.Helper()

	path := filepath.Join(root, relPath)
	require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o750))
	require.NoError(t, os.WriteFile(path, []byte(content), 0o600))
}

func sha256Hex(content string) string {
	sum := sha256.Sum256([]byte(content))
	return hex.EncodeToString(sum[:])
}

func mustParseTime(t *testing.T, value string) time.Time {
	t.Helper()

	parsed, err := time.Parse(time.RFC3339, value)
	require.NoError(t, err)

	return parsed
}

func runGit(t *testing.T, root string, args ...string) string {
	t.Helper()

	//nolint:gosec // Test helper uses a fixed git binary with test-owned arguments.
	cmd := exec.CommandContext(t.Context(), "git", append([]string{"-C", root}, args...)...)
	out, err := cmd.CombinedOutput()
	require.NoErrorf(t, err, "git %s failed:\n%s", strings.Join(args, " "), string(out))

	return strings.TrimSpace(string(out))
}

func readCommandAuditRecords(t *testing.T, auditDir string) []attshell.AuditRecord {
	t.Helper()

	data, err := os.ReadFile(filepath.Join(auditDir, "commands.jsonl"))
	require.NoError(t, err)

	lines := strings.Split(strings.TrimSpace(string(data)), "\n")
	records := make([]attshell.AuditRecord, 0, len(lines))

	for _, line := range lines {
		if strings.TrimSpace(line) == "" {
			continue
		}

		var record attshell.AuditRecord
		require.NoError(t, json.Unmarshal([]byte(line), &record))
		records = append(records, record)
	}

	return records
}

func assertWarning(t *testing.T, warnings []Warning, path, code, severity, reason string) {
	t.Helper()

	for i := range warnings {
		warning := &warnings[i]
		if warning.Path == path && warning.Code == code && warning.Severity == severity && warning.Reason == reason {
			return
		}
	}

	assert.Failf(t, "warning not found", "path=%q code=%q severity=%q reason=%q warnings=%v", path, code, severity, reason, warnings)
}

func assertWarningContains(t *testing.T, warnings []Warning, path, code, severity, reasonPart string) {
	t.Helper()

	for i := range warnings {
		warning := &warnings[i]
		if warning.Path == path && warning.Code == code && warning.Severity == severity && strings.Contains(warning.Reason, reasonPart) {
			return
		}
	}

	assert.Failf(t, "warning not found", "path=%q code=%q severity=%q reason containing %q warnings=%v", path, code, severity, reasonPart, warnings)
}
