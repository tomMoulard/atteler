package githistory

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/tommoulard/atteler/pkg/retrieval"
	"github.com/tommoulard/atteler/pkg/shell"
)

const (
	defaultMaxCommits   = 200
	defaultMaxHunkBytes = 24 * 1024
)

// Query describes git-native history constraints. Empty fields keep git's
// default history semantics for the current branch.
type Query struct {
	Range       string
	Refs        []string
	Paths       []string
	Authors     []string
	Since       time.Time
	Until       time.Time
	All         bool
	FirstParent bool
	NoMerges    bool
	MergesOnly  bool
}

// CollectorOptions configures policy-controlled collection from a repository.
type CollectorOptions struct {
	Query        Query
	Policy       *shell.Policy
	Audit        shell.AuditContext
	Runner       GitRunner
	RepoDir      string
	MaxCommits   int
	MaxHunkBytes int
	IncludeHunks bool
}

// GitRunner executes git with already-tokenized arguments.
type GitRunner interface {
	RunGit(context.Context, string, []string, *shell.Policy, shell.AuditContext) (stdout string, stderr string, err error)
}

type shellGitRunner struct{}

// Collect queries git directly and returns parsed commits with metadata,
// changed files, relationship signals, and optional bounded diff hunks.
func Collect(ctx context.Context, opts CollectorOptions) ([]Commit, error) {
	if ctx == nil {
		return nil, errors.New("githistory: context is required")
	}

	runner := opts.Runner
	if runner == nil {
		runner = shellGitRunner{}
	}

	args := logArgs(opts)
	stdout, stderr, err := runner.RunGit(ctx, opts.RepoDir, args, opts.Policy, auditWithDefaultCaller(opts.Audit))
	if err != nil {
		return nil, fmt.Errorf("githistory: git log: %w%s", err, stderrSuffix(stderr))
	}

	commits, err := parseCollectedLog(stdout)
	if err != nil {
		return nil, err
	}

	if !opts.IncludeHunks {
		return commits, nil
	}

	maxBytes := opts.MaxHunkBytes
	if maxBytes <= 0 {
		maxBytes = defaultMaxHunkBytes
	}

	for i := range commits {
		if err := ctx.Err(); err != nil {
			return nil, fmt.Errorf("githistory: collect hunks: %w", err)
		}

		diffArgs := []string{"show", "--format=", "--find-renames", "--patch", "--unified=3", commits[i].Hash}
		diff, diffStderr, diffErr := runner.RunGit(ctx, opts.RepoDir, diffArgs, opts.Policy, auditWithDefaultCaller(opts.Audit))
		if diffErr != nil {
			return nil, fmt.Errorf("githistory: git show %s: %w%s", commits[i].Hash, diffErr, stderrSuffix(diffStderr))
		}

		commits[i].Diff, commits[i].DiffTruncated = boundedSanitizedDiff(commits[i].Hash, diff, maxBytes)
	}

	return commits, nil
}

func (shellGitRunner) RunGit(ctx context.Context, dir string, args []string, policy *shell.Policy, audit shell.AuditContext) (string, string, error) {
	var stdout, stderr bytes.Buffer

	cmd, invocation, err := shell.CommandContext(ctx, shell.CommandOptions{
		Program: "git",
		Args:    args,
		Dir:     dir,
		Stdout:  &stdout,
		Stderr:  &stderr,
		Mode:    shell.ModeCaptured,
		Policy:  policy,
		Audit:   audit,
	})
	if err != nil {
		return "", "", err
	}

	runErr := cmd.Run()
	finishErr := invocation.Finish(shell.FinishOptions{
		Stdout:        stdout.String(),
		Stderr:        stderr.String(),
		Error:         runErr,
		OutputCapture: shell.OutputCaptured,
	})
	if finishErr != nil {
		return stdout.String(), stderr.String(), finishErr
	}

	return stdout.String(), stderr.String(), runErr
}

func logArgs(opts CollectorOptions) []string {
	maxCommits := opts.MaxCommits
	if maxCommits <= 0 {
		maxCommits = defaultMaxCommits
	}

	args := []string{
		"log",
		"--date=iso-strict",
		"--find-renames",
		"--numstat",
		"--summary",
		"--decorate=short",
		"--pretty=format:%H%x1f%an%x1f%ae%x1f%aI%x1f%s%x1f%b%x1f%D%x1e",
		"--max-count=" + strconv.Itoa(maxCommits),
	}

	query := opts.Query
	if query.All {
		args = append(args, "--all")
	}
	if query.FirstParent {
		args = append(args, "--first-parent")
	}
	if query.NoMerges {
		args = append(args, "--no-merges")
	}
	if query.MergesOnly {
		args = append(args, "--merges")
	}
	if !query.Since.IsZero() {
		args = append(args, "--since="+query.Since.Format(time.RFC3339))
	}
	if !query.Until.IsZero() {
		args = append(args, "--until="+query.Until.Format(time.RFC3339))
	}
	for _, author := range query.Authors {
		if author = strings.TrimSpace(author); author != "" {
			args = append(args, "--author="+author)
		}
	}

	if strings.TrimSpace(query.Range) != "" {
		args = append(args, strings.TrimSpace(query.Range))
	}
	for _, ref := range query.Refs {
		if ref = strings.TrimSpace(ref); ref != "" {
			args = append(args, ref)
		}
	}

	args = append(args, "--")
	for _, path := range query.Paths {
		if path = strings.TrimSpace(path); path != "" {
			args = append(args, path)
		}
	}

	return args
}

func parseCollectedLog(text string) ([]Commit, error) {
	text = strings.ReplaceAll(text, "\r\n", "\n")
	text = strings.ReplaceAll(text, recordSeparator, "\n")

	var (
		commits []Commit
		current *Commit
	)

	for lineNumber, rawLine := range strings.Split(text, "\n") {
		line := strings.TrimRight(rawLine, "\r")
		if strings.TrimSpace(line) == "" {
			continue
		}

		if isHeaderLine(line) {
			if current != nil {
				finalizeCollectedCommit(current)
				commits = append(commits, cloneCommit(*current))
			}

			commit, err := parseCollectedHeader(line)
			if err != nil {
				return nil, fmt.Errorf("githistory: parse collected line %d: %w", lineNumber+1, err)
			}

			current = &commit
			continue
		}

		if current == nil {
			return nil, fmt.Errorf("githistory: parse collected line %d: change listed before commit header", lineNumber+1)
		}

		if change, ok := parseNumstat(line); ok {
			current.Changes = append(current.Changes, change)
			current.Files = appendUnique(current.Files, change.Path)
			if change.OldPath != "" {
				current.Files = appendUnique(current.Files, change.OldPath)
			}
			continue
		}

		if oldPath, newPath, ok := parseRenameSummary(line); ok {
			mergeRename(current, oldPath, newPath)
		}
	}

	if current != nil {
		finalizeCollectedCommit(current)
		commits = append(commits, cloneCommit(*current))
	}

	return commits, nil
}

func parseCollectedHeader(line string) (Commit, error) {
	fields := strings.SplitN(line, fieldSeparator, 7)
	if len(fields) < 5 {
		return Commit{}, errors.New("commit header requires hash, author name, author email, date, and subject")
	}

	date, err := time.Parse(time.RFC3339, strings.TrimSpace(fields[3]))
	if err != nil {
		return Commit{}, fmt.Errorf("invalid author date: %w", err)
	}

	commit := Commit{
		Hash:        strings.TrimSpace(fields[0]),
		AuthorName:  strings.TrimSpace(fields[1]),
		AuthorEmail: strings.TrimSpace(fields[2]),
		Date:        date,
		Subject:     strings.TrimSpace(fields[4]),
	}
	if len(fields) >= 6 {
		commit.Body = strings.TrimSpace(fields[5])
	}
	if len(fields) == 7 {
		commit.Refs = splitRefs(fields[6])
	}
	if commit.Hash == "" {
		return Commit{}, errors.New("commit hash is required")
	}

	return commit, nil
}

func parseNumstat(line string) (ChangedFile, bool) {
	fields := strings.Split(line, "\t")
	if len(fields) < 3 {
		return ChangedFile{}, false
	}

	change := ChangedFile{Path: normalizeRenamePath(strings.Join(fields[2:], "\t")), Status: "modified"}
	if strings.Contains(change.Path, " => ") {
		change.OldPath, change.Path = splitRenamePath(change.Path)
		change.Renamed = true
		change.Status = "renamed"
	}

	if fields[0] == "-" || fields[1] == "-" {
		change.Binary = true
		return change, true
	}

	added, err := strconv.Atoi(fields[0])
	if err != nil {
		return ChangedFile{}, false
	}
	deleted, err := strconv.Atoi(fields[1])
	if err != nil {
		return ChangedFile{}, false
	}

	change.Added = added
	change.Deleted = deleted

	return change, true
}

func parseRenameSummary(line string) (oldPath, newPath string, ok bool) {
	line = strings.TrimSpace(line)
	if !strings.HasPrefix(line, "rename ") || !strings.Contains(line, " => ") {
		return "", "", false
	}

	line = strings.TrimPrefix(line, "rename ")
	if idx := strings.LastIndex(line, " ("); idx >= 0 {
		line = strings.TrimSpace(line[:idx])
	}

	oldPath, newPath = splitRenamePath(line)
	return oldPath, newPath, oldPath != "" && newPath != ""
}

func finalizeCollectedCommit(commit *Commit) {
	commit.Relations = inferRelations(commit.Hash, commit.Subject+"\n"+commit.Body)
}

func inferRelations(hash, text string) CommitRelations {
	normalized := strings.ToLower(text)
	relations := CommitRelations{
		Fixup:  strings.HasPrefix(normalized, "fixup!"),
		Squash: strings.HasPrefix(normalized, "squash!"),
	}

	for _, token := range strings.FieldsFunc(text, func(r rune) bool {
		return !(r == '#' || r == '-' || r == '_' || r == '/' || (r >= '0' && r <= '9') || (r >= 'A' && r <= 'Z') || (r >= 'a' && r <= 'z'))
	}) {
		token = strings.TrimSpace(token)
		lower := strings.ToLower(token)
		if strings.HasPrefix(token, "#") && len(token) > 1 {
			relations.IssueRefs = appendUnique(relations.IssueRefs, token)
			continue
		}
		if strings.HasPrefix(lower, "gh-") && len(token) > 3 {
			relations.IssueRefs = appendUnique(relations.IssueRefs, token)
		}
		if strings.HasPrefix(lower, "pr-") && len(token) > 3 {
			relations.PRRefs = appendUnique(relations.PRRefs, token)
		}
	}

	if reverted := revertedHash(text); reverted != "" && reverted != hash {
		relations.Reverts = append(relations.Reverts, reverted)
	}

	return relations
}

func revertedHash(text string) string {
	const prefix = "This reverts commit "
	idx := strings.Index(text, prefix)
	if idx < 0 {
		return ""
	}

	remainder := text[idx+len(prefix):]
	remainder = strings.TrimLeft(remainder, " \t")
	end := strings.IndexFunc(remainder, func(r rune) bool {
		return !((r >= '0' && r <= '9') || (r >= 'a' && r <= 'f') || (r >= 'A' && r <= 'F'))
	})
	if end >= 0 {
		remainder = remainder[:end]
	}
	if len(remainder) < 7 {
		return ""
	}

	return strings.ToLower(remainder)
}

func mergeRename(commit *Commit, oldPath, newPath string) {
	for i := range commit.Changes {
		if commit.Changes[i].Path == newPath {
			commit.Changes[i].OldPath = oldPath
			commit.Changes[i].Renamed = true
			commit.Changes[i].Status = "renamed"
			commit.Files = appendUnique(commit.Files, oldPath)
			return
		}
	}

	commit.Changes = append(commit.Changes, ChangedFile{
		Path:    newPath,
		OldPath: oldPath,
		Status:  "renamed",
		Renamed: true,
	})
	commit.Files = appendUnique(appendUnique(commit.Files, oldPath), newPath)
}

func boundedSanitizedDiff(hash, diff string, maxBytes int) (string, bool) {
	truncated := false
	if len(diff) > maxBytes {
		diff = diff[:maxBytes]
		truncated = true
	}

	sanitized, _ := retrieval.Sanitize(diff, retrieval.PolicyContext{
		Source:     retrieval.Source{Type: retrieval.SourceGitHistory, Name: "git show"},
		DocumentID: hash,
	})

	return sanitized, truncated
}

func splitRenamePath(path string) (string, string) {
	left, right, ok := strings.Cut(path, " => ")
	if !ok {
		return "", normalizeRenamePath(path)
	}

	left = normalizeRenamePath(left)
	right = normalizeRenamePath(right)
	if strings.Contains(left, "{") && strings.Contains(right, "}") {
		return expandBraceRename(left + " => " + right)
	}

	return left, right
}

func expandBraceRename(path string) (string, string) {
	prefix, rest, ok := strings.Cut(path, "{")
	if !ok {
		return "", normalizeRenamePath(path)
	}
	mid, suffix, ok := strings.Cut(rest, "}")
	if !ok {
		return "", normalizeRenamePath(path)
	}
	left, right, ok := strings.Cut(mid, " => ")
	if !ok {
		return "", normalizeRenamePath(path)
	}

	return normalizeRenamePath(prefix + left + suffix), normalizeRenamePath(prefix + right + suffix)
}

func normalizeRenamePath(path string) string {
	return strings.Trim(strings.TrimSpace(path), `"`)
}

func splitRefs(text string) []string {
	parts := strings.Split(text, ",")
	refs := make([]string, 0, len(parts))
	for _, part := range parts {
		part = strings.TrimSpace(part)
		part = strings.TrimPrefix(part, "HEAD -> ")
		if part != "" {
			refs = append(refs, part)
		}
	}

	return refs
}

func appendUnique(values []string, value string) []string {
	value = strings.TrimSpace(value)
	if value == "" {
		return values
	}
	for _, existing := range values {
		if existing == value {
			return values
		}
	}

	return append(values, value)
}

func auditWithDefaultCaller(audit shell.AuditContext) shell.AuditContext {
	if strings.TrimSpace(audit.Caller) == "" {
		audit.Caller = "atteler.githistory"
	}

	return audit
}

func stderrSuffix(stderr string) string {
	stderr = strings.TrimSpace(stderr)
	if stderr == "" {
		return ""
	}

	return ": " + stderr
}
