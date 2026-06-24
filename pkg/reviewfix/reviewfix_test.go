//nolint:wsl_v5 // Tests keep fixture setup, action, and artifact assertions close together.
package reviewfix

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/tommoulard/atteler/pkg/review"
)

func TestNormalizeFindings_AcceptsAttelerReportShape(t *testing.T) {
	t.Parallel()

	raw := []byte(`{
		"reviewer":"watch-scan",
		"findings":[
			{"severity":"warning","category":"tests","path":"pkg/example/example.go","line":12,"message":"missing regression test","suggestion":"add table test"},
			{"Severity":"High","Category":"Correctness","Path":"pkg/example/example.go","Line":8,"EndLine":9,"Message":"nil dereference","SuggestedVerification":"go test ./pkg/example"}
		]
	}`)

	findings, err := NormalizeFindings(raw)
	require.NoError(t, err)
	require.Len(t, findings, 2)

	assert.Equal(t, "pkg/example/example.go", findings[0].File)
	assert.Equal(t, "high", findings[0].Severity)
	assert.Equal(t, "correctness", findings[0].Category)
	assert.Equal(t, 8, findings[0].Line)
	assert.Equal(t, 9, findings[0].EndLine)
	assert.Equal(t, "watch-scan", findings[0].Source)
	assert.Equal(t, "go test ./pkg/example", findings[0].SuggestedVerification)
	assert.NotEmpty(t, findings[0].ID)

	assert.Equal(t, "medium", findings[1].Severity)
	assert.Equal(t, "add table test", findings[1].SuggestedFix)
}

func TestNormalizeFindings_AcceptsMarshaledAttelerReviewReport(t *testing.T) {
	t.Parallel()

	raw, err := json.Marshal(review.Report{ //nolint:musttag // Exercises the current Atteler-native review.Report JSON shape.
		Reviewer: "quality",
		Findings: []review.Finding{{
			Severity:              review.SeverityHigh,
			Category:              review.CategoryCorrectness,
			Path:                  "pkg/auth.go",
			Line:                  42,
			EndLine:               43,
			Message:               "nil dereference can panic",
			Suggestion:            "check nil before dereference",
			SuggestedVerification: "go test ./pkg/auth",
		}},
	})
	require.NoError(t, err)

	findings, err := NormalizeFindings(raw)
	require.NoError(t, err)
	require.Len(t, findings, 1)

	assert.Equal(t, "quality", findings[0].Source)
	assert.Equal(t, "high", findings[0].Severity)
	assert.Equal(t, "correctness", findings[0].Category)
	assert.Equal(t, "pkg/auth.go", findings[0].File)
	assert.Equal(t, 42, findings[0].Line)
	assert.Equal(t, 43, findings[0].EndLine)
	assert.Equal(t, "check nil before dereference", findings[0].SuggestedFix)
	assert.Equal(t, "go test ./pkg/auth", findings[0].SuggestedVerification)
}

func TestNormalizeFindings_AcceptsMarshaledAttelerReviewResultSessionReports(t *testing.T) {
	t.Parallel()

	raw, err := json.Marshal(review.Result{ //nolint:musttag // Exercises the current Atteler-native review.Result JSON shape.
		Session: review.Session{
			Reports: []review.Report{{
				Reviewer: "quality",
				Findings: []review.Finding{{
					Severity:              review.SeverityMedium,
					Category:              review.CategoryTests,
					Path:                  "pkg/session.go",
					Line:                  7,
					Message:               "missing regression test",
					Suggestion:            "add coverage for session report findings",
					SuggestedVerification: "go test ./pkg/session",
				}},
			}},
		},
	})
	require.NoError(t, err)

	findings, err := NormalizeFindings(raw)
	require.NoError(t, err)
	require.Len(t, findings, 1)

	assert.Equal(t, "quality", findings[0].Source)
	assert.Equal(t, "medium", findings[0].Severity)
	assert.Equal(t, "tests", findings[0].Category)
	assert.Equal(t, "pkg/session.go", findings[0].File)
	assert.Equal(t, "add coverage for session report findings", findings[0].SuggestedFix)
	assert.Equal(t, "go test ./pkg/session", findings[0].SuggestedVerification)
}

func TestNormalizeFindings_AcceptsAttelerLLMJSONShape(t *testing.T) {
	t.Parallel()

	raw := []byte(`{
		"reviewer":"quality-reviewer",
		"findings":[{
			"severity":"high",
			"category":"correctness",
			"path":"pkg/auth.go",
			"line_start":42,
			"line_end":43,
			"message":"nil dereference can panic",
			"evidence":"auth.go dereferences user without checking nil",
			"suggestion":"check nil before dereferencing",
			"suggested_verification":"go test ./pkg/auth"
		}]
	}`)

	findings, err := NormalizeFindings(raw)
	require.NoError(t, err)
	require.Len(t, findings, 1)

	assert.Equal(t, "quality-reviewer", findings[0].Source)
	assert.Equal(t, "high", findings[0].Severity)
	assert.Equal(t, "correctness", findings[0].Category)
	assert.Equal(t, "pkg/auth.go", findings[0].File)
	assert.Equal(t, 42, findings[0].Line)
	assert.Equal(t, 43, findings[0].EndLine)
	assert.Equal(t, "auth.go dereferences user without checking nil", findings[0].Evidence)
	assert.Equal(t, "check nil before dereferencing", findings[0].SuggestedFix)
	assert.Equal(t, "go test ./pkg/auth", findings[0].SuggestedVerification)
}

func TestNormalizeFindings_PreservesNestedReportSources(t *testing.T) {
	t.Parallel()

	raw := []byte(`{
		"reviewer":"aggregate",
		"reports":[
			{"reviewer":"quality","findings":[{"severity":"medium","category":"tests","path":"pkg/quality.go","line":7,"message":"missing regression test"}]},
			{"reviewer":"security","findings":[{"severity":"high","category":"security","path":"pkg/security.go","line":11,"message":"secret may be logged","source":"semgrep"}]}
		]
	}`)

	findings, err := NormalizeFindings(raw)
	require.NoError(t, err)
	require.Len(t, findings, 2)

	assert.Equal(t, "quality", findings[0].Source)
	assert.Equal(t, "pkg/quality.go", findings[0].File)
	assert.Equal(t, "semgrep", findings[1].Source)
	assert.Equal(t, "pkg/security.go", findings[1].File)
}

func TestNormalizeFindings_AcceptsCommonFindingArray(t *testing.T) {
	t.Parallel()

	raw := []byte(`[
		{"id":"finding-123","severity":"important","file":"internal/example.go","line":"42","message":"Potential nil dereference","source":"coderabbit","suggested_fix":"Check for nil before dereferencing."}
	]`)

	findings, err := NormalizeFindings(raw)
	require.NoError(t, err)
	require.Len(t, findings, 1)

	assert.Equal(t, "finding-123", findings[0].ID)
	assert.Equal(t, "high", findings[0].Severity)
	assert.Equal(t, "internal/example.go", findings[0].File)
	assert.Equal(t, 42, findings[0].Line)
	assert.Equal(t, "coderabbit", findings[0].Source)
}

func TestDiscoverGuidance_ReadsHarnessFiles(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(root, "AGENTS.md"), []byte("follow repo rules"), 0o600))
	require.NoError(t, os.WriteFile(filepath.Join(root, "CLAUDE.md"), []byte("follow claude rules"), 0o600))
	require.NoError(t, os.WriteFile(filepath.Join(root, ".cursorrules"), []byte("legacy cursor rules"), 0o600))
	require.NoError(t, os.MkdirAll(filepath.Join(root, ".cursor", "rules"), 0o750))
	require.NoError(t, os.WriteFile(filepath.Join(root, ".cursor", "rules", "go.md"), []byte("prefer gofmt"), 0o600))

	guidance, err := DiscoverGuidance(t.Context(), root)
	require.NoError(t, err)
	require.Len(t, guidance, 4)

	byPath := map[string]string{}
	for i := range guidance {
		byPath[guidance[i].Path] = guidance[i].Content
	}

	assert.Equal(t, "prefer gofmt", byPath[".cursor/rules/go.md"])
	assert.Equal(t, "legacy cursor rules", byPath[".cursorrules"])
	assert.Equal(t, "follow repo rules", byPath["AGENTS.md"])
	assert.Equal(t, "follow claude rules", byPath["CLAUDE.md"])
}

func TestDiscoverGuidanceForFindings_ReadsNestedHarnessFiles(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(root, "AGENTS.md"), []byte("root rules"), 0o600))
	require.NoError(t, os.MkdirAll(filepath.Join(root, "pkg", "auth"), 0o750))
	require.NoError(t, os.WriteFile(filepath.Join(root, "pkg", "AGENTS.md"), []byte("pkg rules"), 0o600))
	require.NoError(t, os.WriteFile(filepath.Join(root, "pkg", "auth", "CLAUDE.md"), []byte("auth rules"), 0o600))

	guidance, err := DiscoverGuidanceForFindings(t.Context(), root, []Finding{{File: "pkg/auth/token.go"}})
	require.NoError(t, err)

	byPath := map[string]string{}
	for i := range guidance {
		byPath[guidance[i].Path] = guidance[i].Content
	}

	assert.Equal(t, "root rules", byPath["AGENTS.md"])
	assert.Equal(t, "pkg rules", byPath["pkg/AGENTS.md"])
	assert.Equal(t, "auth rules", byPath["pkg/auth/CLAUDE.md"])
	assert.NotContains(t, byPath, "../AGENTS.md")
}

func TestBuildPlan_GroupsByFileAndRootCause(t *testing.T) {
	t.Parallel()

	findings := []Finding{
		{ID: "f1", Severity: "medium", File: "pkg/a.go", Line: 10, Message: "missing test for retry", SuggestedVerification: "go test ./pkg/a"},
		{ID: "f2", Severity: "high", File: "pkg/a.go", Line: 20, Message: "nil pointer can panic"},
		{ID: "f3", Severity: "low", File: "pkg/b.go", Category: "style", Message: "format drift"},
	}

	plan := BuildPlan("review.json", findings, []GuidanceFile{{Path: "AGENTS.md", Content: "rules", SizeBytes: 5}}, []string{"go test ./..."}, true, time.Date(2026, 6, 19, 12, 0, 0, 0, time.UTC))

	require.Len(t, plan.Groups, 3)
	assert.Equal(t, "pkg/a.go:nil-safety", plan.Groups[0].Key)
	assert.Equal(t, "pkg/a.go:tests", plan.Groups[1].Key)
	assert.Equal(t, "pkg/b.go:style", plan.Groups[2].Key)
	assert.Contains(t, RenderPlanMarkdown(plan), "AGENTS.md")
	assert.Contains(t, RenderPlanMarkdown(plan), "Suggested verification: `go test ./pkg/a`")
	assert.Contains(t, BuildAgentPrompt(plan), "Do not push branches")
	assert.Contains(t, BuildAgentPrompt(plan), "rules")
}

func TestWriteArtifacts_WritesExpectedRunFiles(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	paths := ArtifactPathsFor(root, "20260619-120000-test")
	plan := BuildPlan("review.json", []Finding{{
		ID:                    "f1",
		Severity:              "high",
		File:                  "pkg/a.go",
		Message:               "nil panic",
		Source:                "quality",
		Evidence:              "user may be nil before dereference",
		SuggestedFix:          "guard nil user",
		SuggestedVerification: "go test ./pkg/a",
	}}, nil, []string{"go test ./..."}, false, time.Date(2026, 6, 19, 12, 0, 0, 0, time.UTC))

	require.NoError(t, WriteInitialArtifacts(t.Context(), paths, []byte(`{"findings":[]}`), plan))

	record := NewRunRecord(
		time.Date(2026, 6, 19, 12, 0, 0, 0, time.UTC),
		time.Date(2026, 6, 19, 12, 1, 0, 0, time.UTC),
		paths,
		plan,
		[]ChangedFile{{Status: "M", Path: "pkg/a.go"}},
		[]ValidationResult{{Command: "go test ./...", Status: "passed", Stdout: "ok\n"}},
		"agent",
		"",
		"diff --git a/pkg/a.go b/pkg/a.go\n",
	)
	require.NoError(t, WriteFinalArtifacts(t.Context(), paths, record, "diff --git a/pkg/a.go b/pkg/a.go\n"))

	for _, path := range []string{paths.FindingsInput, paths.FixPlan, paths.Changes, paths.ValidationLog, paths.PatchDiff, paths.RunJSON} {
		require.FileExists(t, path)
	}

	data, err := os.ReadFile(paths.RunJSON)
	require.NoError(t, err)
	var decoded RunRecord
	require.NoError(t, json.Unmarshal(data, &decoded))
	assert.Equal(t, SchemaVersion, decoded.SchemaVersion)
	assert.False(t, decoded.RemotePublishing)
	assert.True(t, decoded.NoRemotePublishing)
	assert.Equal(t, 1, decoded.FindingCount)
	assert.Equal(t, 1, decoded.GroupCount)
	require.Len(t, decoded.Findings, 1)
	assert.Equal(t, "f1", decoded.Findings[0].ID)
	assert.Equal(t, "M", decoded.ChangedFiles[0].Status)

	changes, err := os.ReadFile(paths.Changes)
	require.NoError(t, err)
	assert.Contains(t, string(changes), "## Original findings")
	assert.Contains(t, string(changes), "nil panic")
	assert.Contains(t, string(changes), "Source: quality")
	assert.Contains(t, string(changes), "Evidence: user may be nil before dereference")
	assert.Contains(t, string(changes), "Suggested fix: guard nil user")
	assert.Contains(t, string(changes), "Suggested verification: `go test ./pkg/a`")
	assert.Contains(t, string(changes), "## Remaining known issues")
}

func TestSuggestedUnifiedDiff_DetectsPatchSuggestions(t *testing.T) {
	t.Parallel()

	diff := "--- a/pkg/a.go\n+++ b/pkg/a.go\n@@ -1 +1 @@\n-old\n+new"
	combined, ok := SuggestedUnifiedDiff([]Finding{{SuggestedFix: "prose only"}, {SuggestedFix: diff}})

	require.True(t, ok)
	assert.Contains(t, combined, "@@ -1 +1 @@")
	assert.False(t, LooksLikeUnifiedDiff("Check for nil first."))
}
