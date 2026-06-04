//nolint:wsl_v5 // Planning helpers are clearer with compact guard/append blocks.
package incident

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	pathpkg "path"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"
)

const (
	commandStatusPassed = "passed"
	commandStatusFailed = "failed"
	commandStatusNotRun = "not_run"
)

// AnalysisOptions controls local repository correlation.
type AnalysisOptions struct {
	RepoRoot        string
	RecentChanges   []Change
	WorktreeChanges []WorktreeChange
	Reproduction    CommandResult
	Validation      []CommandResult
	MaxFrames       int
	MaxSourceFiles  int
}

// Analysis is the incident-to-fix report model rendered for humans, JSON, and
// PR bodies.
//
//nolint:govet // Field order groups report concepts instead of optimizing struct packing.
type Analysis struct {
	Incident        Context          `json:"incident"`
	Risk            Risk             `json:"risk"`
	Reproduction    CommandResult    `json:"reproduction"`
	RedactionPolicy string           `json:"redaction_policy"`
	CodeCandidates  []CodeCandidate  `json:"code_candidates,omitempty"`
	RecentChanges   []Change         `json:"recent_changes,omitempty"`
	WorktreeChanges []WorktreeChange `json:"worktree_changes,omitempty"`
	TestPlan        Plan             `json:"test_plan"`
	FixPlan         Plan             `json:"fix_plan"`
	PRPlan          PRPlan           `json:"pr_plan"`
	Validation      []CommandResult  `json:"validation,omitempty"`
	Warnings        []string         `json:"warnings,omitempty"`
	FixPrompt       string           `json:"fix_prompt,omitempty"`
}

// CodeCandidate links an incident stack frame to a local source location.
//
//nolint:govet // Field order follows report readability.
type CodeCandidate struct {
	Frame      StackFrame `json:"frame,omitzero"`
	Path       string     `json:"path"`
	Function   string     `json:"function,omitempty"`
	Reason     string     `json:"reason"`
	Confidence string     `json:"confidence"`
	Owners     []string   `json:"owners,omitempty"`
	Line       int        `json:"line,omitempty"`
}

// Change is one recent commit correlated with the incident or candidate files.
type Change struct {
	Date         time.Time `json:"date,omitzero"`
	Hash         string    `json:"hash,omitempty"`
	Subject      string    `json:"subject,omitempty"`
	Author       string    `json:"author,omitempty"`
	Match        string    `json:"match,omitempty"`
	Files        []string  `json:"files,omitempty"`
	PullRequests []string  `json:"pull_requests,omitempty"`
}

// WorktreeChange is one local source change produced by the repair loop.
type WorktreeChange struct {
	Status string `json:"status,omitempty"`
	Path   string `json:"path,omitempty"`
}

// Plan captures planned test/fix/PR steps or why they could not run.
//
//nolint:govet // Field order follows report readability.
type Plan struct {
	Summary      string   `json:"summary,omitempty"`
	Steps        []string `json:"steps,omitempty"`
	NotRunReason string   `json:"not_run_reason,omitempty"`
}

// CommandResult records a local repro or validation command outcome.
//
//nolint:govet // Field order follows report readability.
type CommandResult struct {
	StartedAt    time.Time     `json:"started_at,omitzero"`
	Duration     time.Duration `json:"duration,omitzero"`
	Command      string        `json:"command,omitempty"`
	Status       string        `json:"status,omitempty"`
	Stdout       string        `json:"stdout,omitempty"`
	Stderr       string        `json:"stderr,omitempty"`
	Error        string        `json:"error,omitempty"`
	NotRunReason string        `json:"not_run_reason,omitempty"`
}

// Risk summarizes change risk and reviewer routing.
type Risk struct {
	Level              string   `json:"level"`
	Rationale          string   `json:"rationale"`
	SuggestedReviewers []string `json:"suggested_reviewers,omitempty"`
}

// PRPlan describes PR creation support without requiring production access.
type PRPlan struct {
	Title              string   `json:"title,omitempty"`
	Summary            string   `json:"summary,omitempty"`
	BodySections       []string `json:"body_sections,omitempty"`
	SuggestedReviewers []string `json:"suggested_reviewers,omitempty"`
}

// Analyze correlates normalized incident context with the local repository and
// returns a redacted diagnosis plan. It never mutates the repository.
func Analyze(ctx context.Context, inc Context, opts AnalysisOptions) (Analysis, error) {
	if ctx == nil {
		return Analysis{}, errors.New("incident analyze: context is required")
	}
	if err := ctx.Err(); err != nil {
		return Analysis{}, fmt.Errorf("incident analyze: %w", err)
	}

	inc = RedactContext(inc)
	candidates, warnings, err := LinkCodeCandidates(ctx, inc, opts.RepoRoot, opts.MaxFrames, opts.MaxSourceFiles)
	if err != nil {
		return Analysis{}, fmt.Errorf("incident analyze: link source candidates: %w", err)
	}

	reproduction := opts.Reproduction
	if reproduction.Status == "" {
		reproduction = CommandResult{
			Status:       commandStatusNotRun,
			NotRunReason: "no safe reproduction command was supplied; diagnose produced a local repro plan instead",
		}
	}
	reproduction = RedactCommandResult(reproduction)

	validation := make([]CommandResult, 0, len(opts.Validation))
	for i := range opts.Validation {
		validation = append(validation, RedactCommandResult(opts.Validation[i]))
	}

	changes := redactChanges(opts.RecentChanges)
	worktreeChanges := redactWorktreeChanges(opts.WorktreeChanges)
	risk := assessRisk(inc, candidates, worktreeChanges)
	analysis := Analysis{
		Incident:        inc,
		CodeCandidates:  candidates,
		RecentChanges:   changes,
		WorktreeChanges: worktreeChanges,
		Reproduction:    reproduction,
		TestPlan:        buildTestPlan(inc, candidates),
		FixPlan:         buildFixPlan(inc, candidates),
		Validation:      validation,
		Risk:            risk,
		PRPlan:          buildPRPlan(inc, risk),
		Warnings:        warnings,
		RedactionPolicy: RedactionPolicyVersion,
	}
	analysis.FixPrompt = BuildFixPrompt(analysis)

	return analysis, nil
}

// RedactAnalysis returns a defensive copy suitable for stdout, artifacts, and
// PR bodies even if a caller assembled an Analysis without using Analyze.
func RedactAnalysis(analysis Analysis) Analysis {
	analysis.Incident = RedactContext(analysis.Incident)
	analysis.Reproduction = RedactCommandResult(analysis.Reproduction)
	analysis.CodeCandidates = redactCodeCandidates(analysis.CodeCandidates)
	analysis.RecentChanges = redactChanges(analysis.RecentChanges)
	analysis.WorktreeChanges = redactWorktreeChanges(analysis.WorktreeChanges)
	analysis.TestPlan = redactPlan(analysis.TestPlan)
	analysis.FixPlan = redactPlan(analysis.FixPlan)
	analysis.PRPlan = redactPRPlan(analysis.PRPlan)
	analysis.Validation = redactCommandResults(analysis.Validation)
	analysis.Risk = redactRisk(analysis.Risk)
	analysis.Warnings = redactStringSlice(analysis.Warnings, RedactText)
	analysis.FixPrompt = RedactText(analysis.FixPrompt)
	if analysis.RedactionPolicy == "" {
		analysis.RedactionPolicy = RedactionPolicyVersion
	}

	return analysis
}

// LinkCodeCandidates resolves incident stack frames to files under repoRoot.
func LinkCodeCandidates(ctx context.Context, inc Context, repoRoot string, maxFrames, maxFiles int) ([]CodeCandidate, []string, error) {
	repoRoot = strings.TrimSpace(repoRoot)
	if repoRoot == "" {
		return nil, []string{"source-code linking skipped: repository root is not set"}, nil
	}

	files, err := repoFiles(ctx, repoRoot, maxFiles)
	if err != nil {
		return nil, nil, fmt.Errorf("incident source link: list repo files: %w", err)
	}
	if len(files) == 0 {
		return nil, []string{"source-code linking skipped: no repository files found"}, nil
	}

	if maxFrames <= 0 {
		maxFrames = 12
	}

	var warnings []string
	ownerRules, ownerWarning := loadCodeOwnerRules(repoRoot)
	if ownerWarning != "" {
		warnings = append(warnings, ownerWarning)
	}

	candidates := make([]CodeCandidate, 0)
	seen := make(map[string]bool)
	frames := rankedFrames(inc.StackTrace)
	for i := range frames {
		if i >= maxFrames {
			break
		}

		frame := frames[i]
		frameCandidates := candidatesForFrame(frame, files)
		for j := range frameCandidates {
			candidate := frameCandidates[j]
			key := fmt.Sprintf("%s:%d:%s", candidate.Path, candidate.Line, candidate.Function)
			if seen[key] {
				continue
			}

			candidate.Owners = codeOwnersForPath(candidate.Path, ownerRules)
			seen[key] = true
			candidates = append(candidates, candidate)
		}
	}

	sort.SliceStable(candidates, func(i, j int) bool {
		left, right := candidates[i], candidates[j]
		if confidenceRank(left.Confidence) != confidenceRank(right.Confidence) {
			return confidenceRank(left.Confidence) > confidenceRank(right.Confidence)
		}

		return false
	})

	if len(candidates) > maxFrames {
		candidates = candidates[:maxFrames]
	}

	if len(candidates) == 0 && len(inc.StackTrace) > 0 {
		warnings = append(warnings, "stack trace was present, but no frames matched local repository files")
	}

	return candidates, warnings, nil
}

func repoFiles(ctx context.Context, root string, maxFiles int) ([]string, error) {
	if maxFiles <= 0 {
		maxFiles = 20_000
	}

	var files []string
	err := filepath.WalkDir(root, func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return fmt.Errorf("walk %s: %w", path, walkErr)
		}
		if err := ctx.Err(); err != nil {
			return fmt.Errorf("check context: %w", err)
		}
		if entry.IsDir() {
			switch entry.Name() {
			case ".git", ".atteler", ".omx", ".symphony", "node_modules", "vendor":
				if path != root {
					return filepath.SkipDir
				}
			}

			return nil
		}
		if len(files) >= maxFiles {
			return filepath.SkipAll
		}

		rel, err := filepath.Rel(root, path)
		if err != nil {
			return fmt.Errorf("relative path for %s: %w", path, err)
		}

		files = append(files, filepath.ToSlash(rel))

		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("incident source link: scan repo: %w", err)
	}

	sort.Strings(files)

	return files, nil
}

type codeOwnerRule struct {
	Pattern string
	Owners  []string
}

func loadCodeOwnerRules(root string) (rules []codeOwnerRule, warning string) {
	for _, rel := range []string{".github/CODEOWNERS", "CODEOWNERS", "docs/CODEOWNERS"} {
		data, err := os.ReadFile(filepath.Join(root, rel))
		if err == nil {
			return parseCodeOwnerRules(string(data)), ""
		}
		if !errors.Is(err, os.ErrNotExist) {
			return nil, "ownership lookup unavailable: " + RedactText(err.Error())
		}
	}

	return nil, ""
}

func parseCodeOwnerRules(raw string) []codeOwnerRule {
	var rules []codeOwnerRule
	for line := range strings.Lines(raw) {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		if before, _, ok := strings.Cut(line, " #"); ok {
			line = strings.TrimSpace(before)
		}

		fields := strings.Fields(line)
		if len(fields) < 2 {
			continue
		}

		owners := normalizeOwners(fields[1:])
		if len(owners) == 0 {
			continue
		}

		rules = append(rules, codeOwnerRule{Pattern: fields[0], Owners: owners})
	}

	return rules
}

func normalizeOwners(values []string) []string {
	seen := make(map[string]bool)
	owners := make([]string, 0, len(values))
	for _, value := range values {
		value = RedactIdentifier(strings.TrimSpace(value))
		if value == "" || seen[value] {
			continue
		}

		seen[value] = true
		owners = append(owners, value)
	}

	return owners
}

func codeOwnersForPath(file string, rules []codeOwnerRule) []string {
	var owners []string
	for _, rule := range rules {
		if codeOwnerPatternMatches(rule.Pattern, file) {
			owners = rule.Owners
		}
	}

	return append([]string(nil), owners...)
}

func codeOwnerPatternMatches(pattern, file string) bool {
	rawPattern := strings.TrimSpace(pattern)
	file = pathpkg.Clean(normalizePath(file))
	if rawPattern == "" || file == "." {
		return false
	}

	anchored := strings.HasPrefix(rawPattern, "/")
	directory := strings.HasSuffix(rawPattern, "/")
	pattern = pathpkg.Clean(strings.TrimPrefix(normalizePath(rawPattern), "/"))
	if pattern == "." || pattern == "" {
		return false
	}

	if directory {
		prefix := strings.TrimSuffix(pattern, "/")
		return file == prefix || strings.HasPrefix(file, prefix+"/")
	}

	if strings.ContainsAny(pattern, "*?[") {
		return codeOwnerGlobPatternMatches(pattern, file, anchored)
	}

	return codeOwnerLiteralPatternMatches(pattern, file, anchored)
}

func codeOwnerGlobPatternMatches(pattern, file string, anchored bool) bool {
	if strings.Contains(pattern, "**") && codeOwnerDoubleStarPatternMatches(pattern, file) {
		return true
	}
	if ok, err := pathpkg.Match(pattern, file); err == nil && ok {
		return true
	}
	if anchored || strings.Contains(pattern, "/") {
		return false
	}

	ok, err := pathpkg.Match(pattern, pathpkg.Base(file))
	return err == nil && ok
}

func codeOwnerDoubleStarPatternMatches(pattern, file string) bool {
	expr, err := regexp.Compile(codeOwnerDoubleStarRegexp(pattern))
	if err != nil {
		return false
	}

	return expr.MatchString(file)
}

func codeOwnerDoubleStarRegexp(pattern string) string {
	var b strings.Builder
	b.WriteString("^")

	for i := 0; i < len(pattern); {
		switch pattern[i] {
		case '*':
			if i+1 < len(pattern) && pattern[i+1] == '*' {
				i += 2
				if i < len(pattern) && pattern[i] == '/' {
					b.WriteString("(?:.*/)?")
					i++
				} else {
					b.WriteString(".*")
				}
			} else {
				b.WriteString("[^/]*")
				i++
			}
		case '?':
			b.WriteString("[^/]")
			i++
		default:
			b.WriteString(regexp.QuoteMeta(string(pattern[i])))
			i++
		}
	}

	b.WriteString("$")

	return b.String()
}

func codeOwnerLiteralPatternMatches(pattern, file string, anchored bool) bool {
	if !anchored && !strings.Contains(pattern, "/") {
		return pathpkg.Base(file) == pattern
	}

	return file == pattern || strings.HasPrefix(file, strings.TrimSuffix(pattern, "/")+"/")
}

func rankedFrames(frames []StackFrame) []StackFrame {
	ranked := make([]rankedFrame, 0, len(frames))
	for i := range frames {
		ranked = append(ranked, rankedFrame{Frame: frames[i], OriginalIndex: i})
	}

	sort.SliceStable(ranked, func(i, j int) bool {
		if ranked[i].Frame.InApp != ranked[j].Frame.InApp {
			return ranked[i].Frame.InApp
		}

		return ranked[i].OriginalIndex > ranked[j].OriginalIndex
	})

	out := make([]StackFrame, 0, len(ranked))
	for i := range ranked {
		out = append(out, ranked[i].Frame)
	}

	return out
}

type rankedFrame struct {
	Frame         StackFrame
	OriginalIndex int
}

func candidatesForFrame(frame StackFrame, repoFiles []string) []CodeCandidate {
	var candidates []CodeCandidate
	for _, raw := range []string{frame.File, frame.AbsPath} {
		raw = normalizePath(raw)
		if raw == "" {
			continue
		}

		if exact := exactRepoPath(raw, repoFiles); exact != "" {
			candidates = append(candidates, codeCandidate(frame, exact, "high", "stack frame path matches a repository file"))
			continue
		}

		if suffix := suffixRepoPath(raw, repoFiles); suffix != "" {
			candidates = append(candidates, codeCandidate(frame, suffix, "medium", "stack frame path suffix matches a repository file"))
			continue
		}

		if base := basenameRepoPath(raw, repoFiles); base != "" {
			candidates = append(candidates, codeCandidate(frame, base, "low", "stack frame basename matches a repository file"))
		}
	}

	return candidates
}

func codeCandidate(frame StackFrame, path, confidence, reason string) CodeCandidate {
	return CodeCandidate{
		Path:       RedactIdentifier(path),
		Line:       frame.Line,
		Function:   RedactText(frame.Function),
		Confidence: confidence,
		Reason:     reason,
		Frame:      frame,
	}
}

func normalizePath(path string) string {
	path = strings.TrimSpace(path)
	path = strings.TrimPrefix(path, "file://")
	path = strings.ReplaceAll(path, "\\", "/")
	path = strings.TrimPrefix(path, "./")
	if strings.Contains(path, "://") {
		if before, after, ok := strings.Cut(path, "://"); ok && before != "" {
			path = after
			if slash := strings.IndexByte(path, '/'); slash >= 0 {
				path = path[slash+1:]
			}
		}
	}

	return strings.Trim(path, "/")
}

func exactRepoPath(raw string, repoFiles []string) string {
	raw = normalizePath(raw)
	for _, file := range repoFiles {
		if file == raw {
			return file
		}
	}

	return ""
}

func suffixRepoPath(raw string, repoFiles []string) string {
	raw = normalizePath(raw)
	for _, file := range repoFiles {
		if strings.HasSuffix(raw, file) || strings.HasSuffix(file, raw) {
			return file
		}
	}

	return ""
}

func basenameRepoPath(raw string, repoFiles []string) string {
	base := filepath.Base(normalizePath(raw))
	if base == "." || base == "" {
		return ""
	}

	for _, file := range repoFiles {
		if filepath.Base(file) == base {
			return file
		}
	}

	return ""
}

func confidenceRank(confidence string) int {
	switch confidence {
	case "high":
		return 3
	case "medium":
		return 2
	case "low":
		return 1
	default:
		return 0
	}
}

func buildTestPlan(inc Context, candidates []CodeCandidate) Plan {
	if len(candidates) == 0 {
		return Plan{
			Summary:      "No failing test was created automatically.",
			NotRunReason: "no local source file could be linked to the incident stack trace",
			Steps: []string{
				"Use the redacted incident message and stack trace to identify the owner manually.",
				"Add the smallest regression test that reproduces the production failure before applying a fix.",
			},
		}
	}

	first := candidates[0]
	steps := []string{
		fmt.Sprintf("Create a regression test near %s that exercises %s.", first.Path, candidateFunction(first)),
		"Use only redacted request metadata and synthetic values; do not copy production secrets, tokens, cookies, user IDs, or email addresses into tests.",
		"Run the targeted package test and confirm it fails for the production symptom before implementing the fix.",
	}
	if inc.Message != "" {
		steps = append(steps, "Assert on the observed failure mode: "+inc.Message)
	}

	return Plan{
		Summary: "Write the failing regression test before changing production code.",
		Steps:   steps,
	}
}

func buildFixPlan(_ Context, candidates []CodeCandidate) Plan {
	if len(candidates) == 0 {
		return Plan{
			Summary:      "Fix plan requires manual source-code confirmation.",
			NotRunReason: "no local source candidates were found",
		}
	}

	first := candidates[0]

	return Plan{
		Summary: "Patch the smallest code path that explains the incident.",
		Steps: []string{
			fmt.Sprintf("Inspect %s around line %d and verify whether %s can produce the observed error.", first.Path, first.Line, candidateFunction(first)),
			"Implement the narrowest guard or behavior correction that makes the new regression test pass.",
			"Run targeted tests first, then broader validation before opening a PR.",
		},
	}
}

const maxPRTitleLength = 180

func buildPRPlan(inc Context, risk Risk) PRPlan {
	title := "Fix production incident"
	if inc.Reference != "" {
		title += " " + truncateForTitle(inc.Reference, 64)
	}
	if inc.Message != "" {
		title += ": " + truncateForTitle(inc.Message, 72)
	}
	title = truncateForTitle(title, maxPRTitleLength)

	return PRPlan{
		Title:   title,
		Summary: "Open a PR only after the failing test, fix, and validation evidence are present.",
		BodySections: []string{
			"Linked incident reference",
			"Diagnosis summary",
			"Code changes",
			"Tests added",
			"Validation results",
			"Risk assessment",
			"Suggested reviewers",
		},
		SuggestedReviewers: append([]string(nil), risk.SuggestedReviewers...),
	}
}

func assessRisk(inc Context, candidates []CodeCandidate, worktreeChanges []WorktreeChange) Risk {
	haystack := strings.ToLower(strings.Join([]string{
		inc.Service,
		inc.ErrorType,
		inc.Message,
		inc.Title,
		candidatePaths(candidates),
		worktreeChangePaths(worktreeChanges),
	}, " "))

	security := containsAny(haystack, "auth", "oauth", "token", "session", "secret", "password", "permission")
	level := "Low"
	rationale := "diagnosis has limited local code impact until a fix is implemented"
	reviewers := []string{"backend-team"}

	if len(candidates) > 0 {
		level = "Medium"
		rationale = "stack trace maps to local production code"
	}
	if len(candidates) == 0 && len(worktreeChanges) > 0 {
		level = "Medium"
		rationale = "repair loop changed local repository files"
	}
	if security {
		level = "High"
		rationale = "incident or repair changes appear to touch authentication, session, token, or other security-sensitive paths"
		reviewers = append([]string{"security-team"}, reviewers...)
	}
	reviewers = appendUniqueReviewers(reviewers, candidateOwners(candidates)...)

	return Risk{Level: level, Rationale: rationale, SuggestedReviewers: reviewers}
}

// RedactCommandResult redacts command output before it appears in reports.
func RedactCommandResult(result CommandResult) CommandResult {
	result.Command = RedactText(result.Command)
	result.Stdout = RedactText(result.Stdout)
	result.Stderr = RedactText(result.Stderr)
	result.Error = RedactText(result.Error)
	result.NotRunReason = RedactText(result.NotRunReason)

	return result
}

var pullRequestReferencePattern = regexp.MustCompile(`(?i)(?:\(#([0-9]{1,10})\)|\b(?:pr|pull request)\s*#?\s*([0-9]{1,10})\b)`)

// PullRequestsFromText extracts common PR references from commit subjects.
func PullRequestsFromText(text string) []string {
	matches := pullRequestReferencePattern.FindAllStringSubmatch(text, -1)
	if len(matches) == 0 {
		return nil
	}

	seen := make(map[string]bool, len(matches))
	out := make([]string, 0, len(matches))
	for _, match := range matches {
		number := firstNonEmpty(match[1:]...)
		if number == "" {
			continue
		}

		ref := "#" + number
		if seen[ref] {
			continue
		}

		seen[ref] = true
		out = append(out, ref)
	}

	return out
}

func redactCommandResults(values []CommandResult) []CommandResult {
	if len(values) == 0 {
		return nil
	}

	out := make([]CommandResult, 0, len(values))
	for i := range values {
		out = append(out, RedactCommandResult(values[i]))
	}

	return out
}

func redactCodeCandidates(values []CodeCandidate) []CodeCandidate {
	if len(values) == 0 {
		return nil
	}

	out := make([]CodeCandidate, 0, len(values))
	for i := range values {
		value := values[i]
		value.Frame = RedactContext(Context{StackTrace: []StackFrame{value.Frame}}).StackTrace[0]
		value.Path = RedactIdentifier(value.Path)
		value.Function = RedactText(value.Function)
		value.Reason = RedactText(value.Reason)
		value.Confidence = RedactText(value.Confidence)
		value.Owners = redactStringSlice(value.Owners, RedactIdentifier)
		out = append(out, value)
	}

	return out
}

func redactChanges(values []Change) []Change {
	if len(values) == 0 {
		return nil
	}

	out := make([]Change, 0, len(values))
	for i := range values {
		value := values[i]
		next := Change{
			Date:         value.Date,
			Hash:         RedactIdentifier(value.Hash),
			Subject:      RedactText(value.Subject),
			Author:       RedactText(value.Author),
			Match:        RedactText(value.Match),
			Files:        make([]string, 0, len(value.Files)),
			PullRequests: redactStringSlice(value.PullRequests, RedactIdentifier),
		}
		for _, file := range value.Files {
			next.Files = append(next.Files, RedactIdentifier(file))
		}
		out = append(out, next)
	}

	return out
}

func redactWorktreeChanges(values []WorktreeChange) []WorktreeChange {
	if len(values) == 0 {
		return nil
	}

	out := make([]WorktreeChange, 0, len(values))
	for i := range values {
		next := WorktreeChange{
			Status: RedactText(values[i].Status),
			Path:   RedactIdentifier(values[i].Path),
		}
		if strings.TrimSpace(next.Path) != "" {
			out = append(out, next)
		}
	}

	return out
}

func redactPlan(plan Plan) Plan {
	return Plan{
		Summary:      RedactText(plan.Summary),
		Steps:        redactStringSlice(plan.Steps, RedactText),
		NotRunReason: RedactText(plan.NotRunReason),
	}
}

func redactPRPlan(plan PRPlan) PRPlan {
	return PRPlan{
		Title:              RedactText(plan.Title),
		Summary:            RedactText(plan.Summary),
		BodySections:       redactStringSlice(plan.BodySections, RedactText),
		SuggestedReviewers: redactStringSlice(plan.SuggestedReviewers, RedactIdentifier),
	}
}

func redactRisk(risk Risk) Risk {
	return Risk{
		Level:              RedactText(risk.Level),
		Rationale:          RedactText(risk.Rationale),
		SuggestedReviewers: redactStringSlice(risk.SuggestedReviewers, RedactIdentifier),
	}
}

func redactStringSlice(values []string, redact func(string) string) []string {
	if len(values) == 0 {
		return nil
	}

	out := make([]string, 0, len(values))
	for _, value := range values {
		if value = redact(value); strings.TrimSpace(value) != "" {
			out = append(out, value)
		}
	}

	return out
}

func candidateFunction(candidate CodeCandidate) string {
	if candidate.Function != "" {
		return candidate.Function
	}

	return "the failing code path"
}

func candidatePaths(candidates []CodeCandidate) string {
	paths := make([]string, 0, len(candidates))
	for i := range candidates {
		paths = append(paths, candidates[i].Path)
	}

	return strings.Join(paths, " ")
}

func worktreeChangePaths(changes []WorktreeChange) string {
	paths := make([]string, 0, len(changes))
	for i := range changes {
		paths = append(paths, changes[i].Path)
	}

	return strings.Join(paths, " ")
}

func candidateOwners(candidates []CodeCandidate) []string {
	owners := make([]string, 0)
	for i := range candidates {
		owners = append(owners, candidates[i].Owners...)
	}

	return owners
}

func appendUniqueReviewers(reviewers []string, values ...string) []string {
	seen := make(map[string]bool, len(reviewers)+len(values))
	for _, reviewer := range reviewers {
		seen[reviewer] = true
	}
	for _, value := range values {
		value = strings.TrimSpace(value)
		if value == "" || seen[value] {
			continue
		}

		seen[value] = true
		reviewers = append(reviewers, value)
	}

	return reviewers
}

func containsAny(value string, needles ...string) bool {
	for _, needle := range needles {
		if strings.Contains(value, needle) {
			return true
		}
	}

	return false
}

func truncateForTitle(value string, limit int) string {
	value = strings.Join(strings.Fields(value), " ")
	if limit <= 0 {
		return ""
	}

	runes := []rune(value)
	if len(runes) <= limit {
		return value
	}
	if limit <= 3 {
		return string(runes[:limit])
	}

	return strings.TrimSpace(string(runes[:limit-3])) + "..."
}
