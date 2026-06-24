//nolint:gocritic,wsl_v5 // Table-style git history tests keep setup and assertions compact.
package githistory

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/tommoulard/atteler/pkg/retrieval"
	"github.com/tommoulard/atteler/pkg/shell"
)

const sampleLog = "bbb\x1fBob\x1fbob@example.com\x1f2026-04-02T12:00:00Z\x1fFix memory search regression\x1e\npkg/memory/memory.go\nREADME.md\n" +
	"aaa\x1fAda\x1fada@example.com\x1f2026-04-01T10:00:00Z\x1fAdd agent orchestration planning\x1e\npkg/agent/orchestration.go\n"

func TestParseLog_ParsesHeadersAndFiles(t *testing.T) {
	t.Parallel()

	commits, err := ParseLog(sampleLog)
	if err != nil {
		t.Fatalf("ParseLog() error = %v", err)
	}

	if len(commits) != 2 {
		t.Fatalf("len(commits) = %d, want 2: %#v", len(commits), commits)
	}

	wantDate := time.Date(2026, 4, 2, 12, 0, 0, 0, time.UTC)
	if commits[0].Hash != "bbb" || commits[0].AuthorName != "Bob" || !commits[0].Date.Equal(wantDate) {
		t.Fatalf("first commit metadata = %#v, want parsed metadata", commits[0])
	}

	if !reflect.DeepEqual(commits[0].Files, []string{"pkg/memory/memory.go", "README.md"}) {
		t.Fatalf("Files = %#v, want memory and README", commits[0].Files)
	}
}

func TestParseLog_RejectsMalformedInput(t *testing.T) {
	t.Parallel()

	if _, err := ParseLog("pkg/memory/memory.go\n"); err == nil || !strings.Contains(err.Error(), "file listed before commit header") {
		t.Fatalf("ParseLog(file before header) error = %v, want file-before-header error", err)
	}

	if _, err := ParseLog("bad\x1fBob\x1fbob@example.com\x1fnot-a-date\x1fSubject\n"); err == nil || !strings.Contains(err.Error(), "invalid author date") {
		t.Fatalf("ParseLog(bad date) error = %v, want date error", err)
	}
}

func TestIndex_SearchRanksSubjectFilesAndAuthor(t *testing.T) {
	t.Parallel()

	commits, err := ParseLog(sampleLog)
	if err != nil {
		t.Fatalf("ParseLog() error = %v", err)
	}

	idx := NewIndex(commits)

	results := idx.Search("memory regression", 0)
	if len(results) != 1 {
		t.Fatalf("Search(memory regression) returned %#v, want one result", results)
	}

	if results[0].Commit.Hash != "bbb" {
		t.Fatalf("Search(memory regression) first hash = %q, want bbb", results[0].Commit.Hash)
	}

	if results[0].Score == 0 || len(results[0].Snippets) == 0 {
		t.Fatalf("Search(memory regression) result = %#v, want score and snippets", results[0])
	}

	results = idx.Search("ada orchestration", 1)
	if len(results) != 1 || results[0].Commit.Hash != "aaa" {
		t.Fatalf("Search(ada orchestration) = %#v, want aaa only", results)
	}

	commits[1].Body = "Durable NOTES local RAG context"

	results = NewIndex(commits).Search("durable rag", 1)
	if len(results) != 1 || results[0].Commit.Hash != "aaa" {
		t.Fatalf("Search(body) = %#v, want aaa only", results)
	}
}

func TestIndex_SearchIsDeterministicWithTieBreakers(t *testing.T) {
	t.Parallel()

	older := Commit{Hash: "bbb", Date: time.Date(2026, 4, 1, 0, 0, 0, 0, time.UTC), Subject: "same subject"}
	newer := Commit{Hash: "aaa", Date: time.Date(2026, 4, 2, 0, 0, 0, 0, time.UTC), Subject: "same subject"}
	results := NewIndex([]Commit{older, newer}).Search("same", 0)

	if got := commitHashes(results); !reflect.DeepEqual(got, []string{"aaa", "bbb"}) {
		t.Fatalf("Search tie order = %#v, want newer first", got)
	}
}

func TestIndex_SearchDefensivelyCopiesCommits(t *testing.T) {
	t.Parallel()

	commit := Commit{Hash: "abc", Subject: "memory", Files: []string{"a.go"}}
	idx := NewIndex([]Commit{commit})
	commit.Files[0] = "mutated.go"

	results := idx.Search("memory", 1)
	if results[0].Commit.Files[0] != "a.go" {
		t.Fatalf("indexed file = %q, want defensive copy", results[0].Commit.Files[0])
	}

	results[0].Commit.Files[0] = "mutated-result.go"

	results = idx.Search("memory", 1)
	if results[0].Commit.Files[0] != "a.go" {
		t.Fatalf("result mutation leaked into index: %#v", results[0].Commit.Files)
	}
}

func TestIndex_SearchEmptyInputsReturnNoResults(t *testing.T) {
	t.Parallel()

	if got := NewIndex(nil).Search("memory", 0); len(got) != 0 {
		t.Fatalf("Search on empty index = %#v, want none", got)
	}

	if got := NewIndex([]Commit{{Hash: "abc", Subject: "memory"}}).Search("   ", 0); len(got) != 0 {
		t.Fatalf("Search empty query = %#v, want none", got)
	}
}

func commitHashes(results []Result) []string {
	hashes := make([]string, 0, len(results))
	for i := range results {
		hashes = append(hashes, results[i].Commit.Hash)
	}

	return hashes
}

func TestIndex_SearchRetrievalUsesSharedContract(t *testing.T) {
	t.Parallel()

	commits, err := ParseLog(sampleLog)
	require.NoError(t, err)

	results, err := NewIndex(commits).SearchRetrieval(context.Background(), retrieval.Query{Text: "memory regression", Limit: 1, Explain: true})
	require.NoError(t, err)
	require.Len(t, results, 1)

	result := results[0]
	assert.Equal(t, retrieval.SourceGitHistory, result.Source.Type)
	assert.Equal(t, "bbb", result.DocumentID)
	assert.NotEmpty(t, result.Chunk.ID)
	assert.NotEmpty(t, result.Metadata[retrieval.MetadataStableID])
	assert.NotEmpty(t, result.Metadata[retrieval.MetadataContentHash])
	assert.Equal(t, "git-history-explainable-weighted", result.Scorer.Name)
	assert.NotEmpty(t, result.Scorer.Explanation)
}

func TestIndex_SearchRetrievalIncludesRefsAndDiffMetadata(t *testing.T) {
	t.Parallel()

	commit := Commit{
		Hash:          "abc",
		Date:          time.Date(2026, 4, 3, 0, 0, 0, 0, time.UTC),
		Subject:       "Release history collector",
		Refs:          []string{"main", "tag: v1.2.3"},
		DiffTruncated: true,
	}

	results, err := NewIndex([]Commit{commit}).SearchRetrieval(context.Background(), retrieval.Query{
		Text:  "release",
		Limit: 1,
	})
	require.NoError(t, err)
	require.Len(t, results, 1)

	assert.Equal(t, "main\ntag: v1.2.3", results[0].Metadata["refs"])
	assert.Equal(t, "true", results[0].Metadata["diff_truncated"])
	assert.Contains(t, results[0].Metadata["range_context"], "refs=main,tag: v1.2.3")
}

func TestIndex_SearchRetrievalRedactsSensitiveCommitText(t *testing.T) {
	t.Parallel()

	commit := Commit{
		Hash:    "secret",
		Date:    time.Date(2026, 4, 3, 0, 0, 0, 0, time.UTC),
		Subject: "Rotate OAuth api_key=super-secret-token",
	}

	results, err := NewIndex([]Commit{commit}).SearchRetrieval(context.Background(), retrieval.Query{
		Text:          "oauth",
		Limit:         1,
		IncludeUnsafe: true,
	})
	require.NoError(t, err)
	require.Len(t, results, 1)

	assert.False(t, results[0].Safety.InjectAllowed)
	assert.True(t, results[0].Safety.Redacted)
	assert.NotContains(t, results[0].Snippet, "super-secret-token")
	assert.Contains(t, results[0].Snippet, "[REDACTED]")
	assert.NotContains(t, results[0].Metadata["subject"], "super-secret-token")
	assert.Contains(t, results[0].Metadata["subject"], "[REDACTED]")

	data, err := json.Marshal(results[0])
	require.NoError(t, err)
	assert.NotContains(t, string(data), "super-secret-token")
}

func TestCollect_BuildsGitLogArgsForPathDateAuthorAndRange(t *testing.T) {
	t.Parallel()

	since := time.Date(2026, 4, 1, 0, 0, 0, 0, time.UTC)
	until := time.Date(2026, 4, 5, 0, 0, 0, 0, time.UTC)
	runner := &fakeGitRunner{responses: map[string]string{
		"log": "abc\x1fAda\x1fada@example.com\x1f2026-04-02T12:00:00Z\x1fTouch history\x1f\x1fmain\x1e\n1\t0\tpkg/githistory/githistory.go\n",
	}}

	commits, err := Collect(context.Background(), CollectorOptions{
		RepoDir: "/repo",
		Runner:  runner,
		Query: Query{
			Range:       "v1.0..HEAD",
			Paths:       []string{"pkg/githistory"},
			Authors:     []string{"Ada"},
			Since:       since,
			Until:       until,
			Refs:        []string{"main"},
			All:         true,
			FirstParent: true,
			NoMerges:    true,
		},
		MaxCommits: 25,
	})
	require.NoError(t, err)
	require.Len(t, commits, 1)

	args := runner.calls[0]
	assert.Contains(t, args, "--since="+since.Format(time.RFC3339))
	assert.Contains(t, args, "--until="+until.Format(time.RFC3339))
	assert.Contains(t, args, "--author=Ada")
	assert.Contains(t, args, "--all")
	assert.Contains(t, args, "--first-parent")
	assert.Contains(t, args, "--no-merges")
	assert.Contains(t, args, "--max-count=25")
	assertLess(t, indexOf(args, "v1.0..HEAD"), indexOf(args, "--"))
	assertLess(t, indexOf(args, "main"), indexOf(args, "--"))
	assert.Greater(t, indexOf(args, "pkg/githistory"), indexOf(args, "--"))
	assert.Equal(t, []string{"pkg/githistory/githistory.go"}, commits[0].Files)
}

func TestCollect_QueriesRealGitRepositoryWithPathAuthorAndDate(t *testing.T) {
	t.Parallel()

	if _, err := exec.LookPath("git"); err != nil {
		t.Skipf("git binary not available: %v", err)
	}

	root := t.TempDir()
	runTestGit(t, root, nil, "init")
	runTestGit(t, root, nil, "config", "user.name", "Test Committer")
	runTestGit(t, root, nil, "config", "user.email", "committer@example.com")
	runTestGit(t, root, nil, "config", "commit.gpgsign", "false")

	require.NoError(t, os.MkdirAll(filepath.Join(root, "pkg"), 0o750))
	require.NoError(t, os.WriteFile(filepath.Join(root, "pkg", "target.go"), []byte("package pkg\n"), 0o600))
	runTestGit(t, root, nil, "add", "pkg/target.go")
	runTestGit(t, root, gitCommitEnv("Ada", "ada@example.com", "2026-04-02T12:00:00Z"), "commit", "-m", "touch target history")

	require.NoError(t, os.MkdirAll(filepath.Join(root, "docs"), 0o750))
	require.NoError(t, os.WriteFile(filepath.Join(root, "docs", "other.md"), []byte("other history\n"), 0o600))
	runTestGit(t, root, nil, "add", "docs/other.md")
	runTestGit(t, root, gitCommitEnv("Bob", "bob@example.com", "2026-04-03T12:00:00Z"), "commit", "-m", "touch docs history")

	commits, err := Collect(context.Background(), CollectorOptions{
		RepoDir: root,
		Query: Query{
			Paths:   []string{"pkg"},
			Authors: []string{"Ada"},
			Since:   time.Date(2026, 4, 2, 0, 0, 0, 0, time.UTC),
			Until:   time.Date(2026, 4, 2, 23, 59, 59, 0, time.UTC),
		},
		MaxCommits: 10,
	})
	require.NoError(t, err)
	require.Len(t, commits, 1)

	assert.Equal(t, "Ada", commits[0].AuthorName)
	assert.Equal(t, "touch target history", commits[0].Subject)
	assert.Equal(t, []string{"pkg/target.go"}, commits[0].Files)
	require.Len(t, commits[0].Changes, 1)
	assert.Equal(t, "pkg/target.go", commits[0].Changes[0].Path)
}

func TestCollect_RejectsMalformedCollectedLog(t *testing.T) {
	t.Parallel()

	runner := &fakeGitRunner{responses: map[string]string{
		"log": "1\t0\tfile.go\n",
	}}

	_, err := Collect(context.Background(), CollectorOptions{Runner: runner})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "change listed before commit header")
}

func TestCollect_AcceptsLegacyNameOnlyPathLines(t *testing.T) {
	t.Parallel()

	runner := &fakeGitRunner{responses: map[string]string{
		"log": "abc\x1fAda\x1fada@example.com\x1f2026-04-02T12:00:00Z\x1fTouch history\x1f\x1fmain\x1e\npkg/history.go\n",
	}}

	commits, err := Collect(context.Background(), CollectorOptions{Runner: runner})
	require.NoError(t, err)
	require.Len(t, commits, 1)
	assert.Equal(t, []string{"pkg/history.go"}, commits[0].Files)
}

func TestCollect_PreservesMultilineBodiesBeforeChangeEvidence(t *testing.T) {
	t.Parallel()

	runner := &fakeGitRunner{responses: map[string]string{
		"log": strings.Join([]string{
			"abc\x1fAda\x1fada@example.com\x1f2026-04-02T12:00:00Z\x1fExplain history\x1fFirst rationale line",
			"Second rationale line mentions rollout and ownership",
			"Refs #58\x1fmain\x1e",
			"1\t0\tpkg/history.go",
		}, "\n"),
	}}

	commits, err := Collect(context.Background(), CollectorOptions{Runner: runner})
	require.NoError(t, err)
	require.Len(t, commits, 1)

	assert.Equal(t, strings.Join([]string{
		"First rationale line",
		"Second rationale line mentions rollout and ownership",
		"Refs #58",
	}, "\n"), commits[0].Body)
	assert.Equal(t, []string{"main"}, commits[0].Refs)
	assert.Equal(t, []string{"pkg/history.go"}, commits[0].Files)
	assert.Contains(t, commits[0].Relations.IssueRefs, "#58")
}

func TestCollect_DetectsAddedAndDeletedFileStatuses(t *testing.T) {
	t.Parallel()

	runner := &fakeGitRunner{responses: map[string]string{
		"log": strings.Join([]string{
			"abc\x1fAda\x1fada@example.com\x1f2026-04-02T12:00:00Z\x1fAdd and delete files\x1f\x1fmain\x1e",
			"3\t0\tpkg/new.go",
			"0\t2\tpkg/old.go",
			" create mode 100644 pkg/new.go",
			" delete mode 100644 pkg/old.go",
		}, "\n"),
	}}

	commits, err := Collect(context.Background(), CollectorOptions{Runner: runner})
	require.NoError(t, err)
	require.Len(t, commits, 1)
	require.Len(t, commits[0].Changes, 2)

	assert.Equal(t, "added", commits[0].Changes[0].Status)
	assert.Equal(t, "pkg/new.go", commits[0].Changes[0].Path)
	assert.Equal(t, "deleted", commits[0].Changes[1].Status)
	assert.Equal(t, "pkg/old.go", commits[0].Changes[1].Path)
}

func TestCollect_DetectsRenamesRevertsFixupsAndIssueRefs(t *testing.T) {
	t.Parallel()

	runner := &fakeGitRunner{responses: map[string]string{
		"log": strings.Join([]string{
			"def\x1fBob\x1fbob@example.com\x1f2026-04-03T12:00:00Z\x1fRevert \"rename package\" #58\x1fThis reverts commit abc1234def5678.\x1ftag: v1.1\x1e",
			"2\t1\tpkg/{old => new}/history.go",
			" rename pkg/{old => new}/history.go (90%)",
			"abc\x1fAda\x1fada@example.com\x1f2026-04-02T12:00:00Z\x1ffixup! history collector GH-58 PR-7\x1fPull request #99 closes #100\x1fmain\x1e",
			"1\t0\tpkg/history.go",
		}, "\n"),
	}}

	commits, err := Collect(context.Background(), CollectorOptions{Runner: runner})
	require.NoError(t, err)
	require.Len(t, commits, 2)

	rename := commits[0].Changes[0]
	assert.True(t, rename.Renamed)
	assert.Equal(t, "renamed", rename.Status)
	assert.Equal(t, "pkg/old/history.go", rename.OldPath)
	assert.Equal(t, "pkg/new/history.go", rename.Path)
	assert.Equal(t, []string{"abc1234def5678"}, commits[0].Relations.Reverts)
	assert.Contains(t, commits[0].Relations.IssueRefs, "#58")
	assert.Contains(t, commits[0].Refs, "tag: v1.1")

	assert.True(t, commits[1].Relations.Fixup)
	assert.Contains(t, commits[1].Relations.IssueRefs, "GH-58")
	assert.Contains(t, commits[1].Relations.IssueRefs, "#100")
	assert.Contains(t, commits[1].Relations.PRRefs, "PR-7")
	assert.Contains(t, commits[1].Relations.PRRefs, "#99")
}

func TestCollect_OptionalDiffHunksAreBoundedAndSanitized(t *testing.T) {
	t.Parallel()

	runner := &fakeGitRunner{responses: map[string]string{
		"log":  "abc\x1fAda\x1fada@example.com\x1f2026-04-02T12:00:00Z\x1fSecret diff\x1f\x1fmain\x1e\n1\t0\tsecret.txt\n",
		"show": "diff --git a/secret.txt b/secret.txt\n+api_key=super-secret-token\n+next line\n",
	}}

	commits, err := Collect(context.Background(), CollectorOptions{
		Runner:       runner,
		IncludeHunks: true,
		MaxHunkBytes: 52,
	})
	require.NoError(t, err)
	require.Len(t, commits, 1)
	assert.True(t, commits[0].DiffTruncated)
	assert.Contains(t, commits[0].Diff, "[REDACTED]")
	assert.NotContains(t, commits[0].Diff, "super-secret-token")
	assert.Len(t, runner.calls, 2)
	assert.Equal(t, "show", runner.calls[1][0])
}

func TestIndex_SearchIncludesExplainableMatchedEvidence(t *testing.T) {
	t.Parallel()

	commit := Commit{
		Hash:    "abc",
		Date:    time.Date(2026, 4, 2, 0, 0, 0, 0, time.UTC),
		Subject: "Rename history collector",
		Changes: []ChangedFile{{
			Path:    "pkg/new/history.go",
			OldPath: "pkg/old/history.go",
			Status:  "renamed",
			Renamed: true,
			Added:   2,
			Deleted: 1,
		}},
	}

	results := NewIndex([]Commit{commit}).Search("old history renamed", 1)
	require.Len(t, results, 1)
	assert.NotEmpty(t, results[0].Matches)
	assert.Greater(t, results[0].Confidence, 0.0)
	assert.Contains(t, matchedFields(results[0].Matches), "files")
}

type fakeGitRunner struct {
	responses map[string]string
	calls     [][]string
}

func (f *fakeGitRunner) RunGit(_ context.Context, _ string, args []string, _ *shell.Policy, _ shell.AuditContext) (string, string, error) {
	f.calls = append(f.calls, append([]string(nil), args...))
	if len(args) == 0 {
		return "", "", nil
	}

	return f.responses[args[0]], "", nil
}

func runTestGit(t *testing.T, dir string, env []string, args ...string) {
	t.Helper()

	cmd := exec.CommandContext(t.Context(), "git", args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), env...)
	out, err := cmd.CombinedOutput()
	require.NoErrorf(t, err, "git %s failed:\n%s", strings.Join(args, " "), string(out))
}

func gitCommitEnv(name, email, date string) []string {
	return []string{
		"GIT_AUTHOR_NAME=" + name,
		"GIT_AUTHOR_EMAIL=" + email,
		"GIT_AUTHOR_DATE=" + date,
		"GIT_COMMITTER_DATE=" + date,
	}
}

func indexOf(values []string, value string) int {
	for i, candidate := range values {
		if candidate == value {
			return i
		}
	}

	return -1
}

func assertLess(t *testing.T, left, right int) {
	t.Helper()
	assert.GreaterOrEqual(t, left, 0)
	assert.GreaterOrEqual(t, right, 0)
	assert.Less(t, left, right)
}
