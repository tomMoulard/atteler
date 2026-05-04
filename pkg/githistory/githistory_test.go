package githistory

import (
	"reflect"
	"strings"
	"testing"
	"time"
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
