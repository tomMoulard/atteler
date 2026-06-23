// Package issuewatch prepares local-first implementation attempts for tracker issues.
//
//nolint:wsl_v5 // Artifact assembly uses explicit sequential write/read steps for auditability.
package issuewatch

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"hash/fnv"
	"maps"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/tommoulard/atteler/internal/atomicfile"
	"github.com/tommoulard/atteler/pkg/permission"
	attshell "github.com/tommoulard/atteler/pkg/shell"
	"github.com/tommoulard/atteler/pkg/sourcepolicy"
	"github.com/tommoulard/atteler/pkg/symphony"
	"github.com/tommoulard/atteler/pkg/worktree"
)

const (
	// RunSchemaVersion is the machine-readable issue-watch run metadata schema.
	RunSchemaVersion = "atteler.issue_watch.run.v1"
	// StateSchemaVersion is the machine-readable duplicate-avoidance state schema.
	StateSchemaVersion = "atteler.issue_watch.state.v1"

	defaultStatePath = ".atteler/issue-watch/state.json"
	defaultRunsRoot  = ".atteler/runs/issues"

	issueJSONFile         = "issue.json"
	planFile              = "plan.md"
	implementationFile    = "implementation.md"
	validationLogFile     = "validation.log"
	patchFile             = "patch.diff"
	runJSONFile           = "run.json"
	maxGuidancePlanRunes  = 1600
	maxIssuePlanComments  = 20
	maxIssueCommentRunes  = 1200
	maxSafeIDLength       = 80
	maxWorktreeSessionID  = 120
	commandOutputMaxBytes = 64 * 1024

	statusNeedsHuman = "needs-human"
	statusSucceeded  = "succeeded"
	statusFailed     = "failed"

	defaultCommandTimeout = 10 * time.Minute
)

// Tracker is the read-only issue tracker surface required by issue watch.
type Tracker interface {
	FetchCandidateIssues(context.Context) ([]symphony.Issue, error)
}

// WorktreeCreator creates a local isolated implementation workspace.
type WorktreeCreator func(context.Context, string, string) (*worktree.Info, error)

// Options configures one issue-watch polling iteration.
type Options struct {
	Now                   func() time.Time
	Tracker               Tracker
	CreateWorktree        WorktreeCreator
	Root                  string
	Repository            string
	StatePath             string
	RunsRoot              string
	Command               string
	Labels                []string
	ValidationCommands    []string
	CommandTimeout        time.Duration
	CommandOutputMaxBytes int64
	DryRun                bool
	AllowEmptyLabels      bool
	IgnoreState           bool
}

// Result reports one issue-watch polling iteration.
//
//nolint:govet // JSON field order follows the run lifecycle.
type Result struct {
	StartedAt  time.Time     `json:"started_at"`
	FinishedAt time.Time     `json:"finished_at"`
	StatePath  string        `json:"state_path,omitempty"`
	DryRun     bool          `json:"dry_run,omitempty"`
	Candidates []Candidate   `json:"candidates,omitempty"`
	Runs       []RunMetadata `json:"runs,omitempty"`
}

// Candidate is an eligible issue selected for local work.
//
//nolint:govet // JSON field order keeps issue identity before derived state keys.
type Candidate struct {
	UpdatedAt   *time.Time              `json:"updated_at,omitempty"`
	CreatedAt   *time.Time              `json:"created_at,omitempty"`
	Description *string                 `json:"description,omitempty"`
	Priority    *int                    `json:"priority,omitempty"`
	URL         *string                 `json:"url,omitempty"`
	ID          string                  `json:"id"`
	Identifier  string                  `json:"identifier"`
	Title       string                  `json:"title"`
	State       string                  `json:"state"`
	Labels      []string                `json:"labels,omitempty"`
	BlockedBy   []symphony.BlockerRef   `json:"blocked_by,omitempty"`
	Comments    []symphony.IssueComment `json:"comments,omitempty"`
	StateKey    string                  `json:"state_key"`
	IssueIDPath string                  `json:"issue_id_path"`
}

// State records processed issues so future watch iterations do not duplicate
// local worktrees/runs for the same tracker issue.
//
//nolint:govet // JSON field order keeps schema metadata first.
type State struct {
	UpdatedAt     time.Time           `json:"updated_at,omitzero"`
	SchemaVersion string              `json:"schema_version"`
	Runs          map[string]StateRun `json:"runs"`
}

// StateRun is the last local attempt recorded for one issue state key.
type StateRun struct {
	UpdatedAt       time.Time  `json:"updated_at,omitzero"`
	IssueUpdatedAt  *time.Time `json:"issue_updated_at,omitempty"`
	IssueID         string     `json:"issue_id"`
	IssueIdentifier string     `json:"issue_identifier"`
	RunID           string     `json:"run_id"`
	Status          string     `json:"status"`
	ArtifactDir     string     `json:"artifact_dir"`
	WorktreePath    string     `json:"worktree_path"`
	Branch          string     `json:"branch"`
}

// RunMetadata is written to run.json and returned to the CLI.
//
//nolint:govet // JSON field order follows the documented artifact lifecycle.
type RunMetadata struct {
	StartedAt       time.Time        `json:"started_at"`
	CompletedAt     time.Time        `json:"completed_at"`
	IssueUpdatedAt  *time.Time       `json:"issue_updated_at,omitempty"`
	IssueURL        *string          `json:"issue_url,omitempty"`
	Guidance        []GuidanceFile   `json:"guidance,omitempty"`
	Workflow        WorkflowReport   `json:"workflow"`
	Validation      ValidationReport `json:"validation"`
	Artifacts       RunArtifacts     `json:"artifacts"`
	SchemaVersion   string           `json:"schema_version"`
	RunID           string           `json:"run_id"`
	Status          string           `json:"status"`
	StatusReason    string           `json:"status_reason"`
	Repository      string           `json:"repository,omitempty"`
	IssueID         string           `json:"issue_id"`
	IssueIdentifier string           `json:"issue_identifier"`
	IssueTitle      string           `json:"issue_title"`
	IssueState      string           `json:"issue_state"`
	StateKey        string           `json:"state_key"`
	WorktreePath    string           `json:"worktree_path"`
	WorktreeBranch  string           `json:"worktree_branch"`
	BaseBranch      string           `json:"base_branch,omitempty"`
	Safety          string           `json:"safety"`
	Labels          []string         `json:"labels,omitempty"`
	ChangedFiles    []string         `json:"changed_files,omitempty"`
}

// RunArtifacts lists the files created for a local issue-watch run.
type RunArtifacts struct {
	Dir            string `json:"dir"`
	IssueJSON      string `json:"issue_json"`
	Plan           string `json:"plan"`
	Implementation string `json:"implementation"`
	ValidationLog  string `json:"validation_log"`
	Patch          string `json:"patch"`
	RunJSON        string `json:"run_json"`
}

// ValidationReport records local validation evidence when issue watch has any.
type ValidationReport struct {
	LogPath  string          `json:"log_path"`
	Summary  string          `json:"summary"`
	Commands []string        `json:"commands,omitempty"`
	Results  []CommandResult `json:"results,omitempty"`
	Passed   bool            `json:"passed"`
	Run      bool            `json:"run"`
}

// GuidanceFile records one discovered repository-specific agent instruction file.
type GuidanceFile struct {
	Path    string `json:"path"`
	Kind    string `json:"kind"`
	Snippet string `json:"snippet,omitempty"`
}

// WorkflowReport records an optional local implementation command run in the
// isolated worktree before validation.
type WorkflowReport struct {
	Result  *CommandResult `json:"result,omitempty"`
	Command string         `json:"command,omitempty"`
	Summary string         `json:"summary"`
	Passed  bool           `json:"passed"`
	Run     bool           `json:"run"`
}

// CommandResult captures bounded local command evidence for issue-watch runs.
//
//nolint:govet // JSON field order follows command lifecycle then captured evidence.
type CommandResult struct {
	StartedAt       time.Time     `json:"started_at,omitzero"`
	CompletedAt     time.Time     `json:"completed_at,omitzero"`
	Duration        time.Duration `json:"duration,omitempty"`
	Command         string        `json:"command"`
	Stdout          string        `json:"stdout,omitempty"`
	Stderr          string        `json:"stderr,omitempty"`
	Error           string        `json:"error,omitempty"`
	ExitError       string        `json:"exit_error,omitempty"`
	OutputTruncated bool          `json:"output_truncated,omitempty"`
	Passed          bool          `json:"passed"`
}

// RunOnce fetches eligible tracker issues and, unless dry-run is enabled,
// prepares local run artifacts plus an isolated git worktree for each new issue.
func RunOnce(ctx context.Context, options Options) (Result, error) {
	if ctx == nil {
		return Result{}, errors.New("issue watch: context is required")
	}

	options, err := normalizeOptions(options)
	if err != nil {
		return Result{}, err
	}

	startedAt := options.Now().UTC()
	result := Result{
		StartedAt: startedAt,
		StatePath: options.StatePath,
		DryRun:    options.DryRun,
	}

	state, err := LoadState(options.StatePath)
	if err != nil {
		return result, err
	}

	issues, err := options.Tracker.FetchCandidateIssues(ctx)
	if err != nil {
		return result, fmt.Errorf("issue watch: fetch candidate issues: %w", err)
	}

	selectionState := state
	if options.IgnoreState {
		selectionState = emptyState()
	}
	candidates := SelectCandidates(issues, options.Labels, selectionState)
	result.Candidates = candidates

	if options.DryRun {
		result.FinishedAt = options.Now().UTC()
		return result, nil
	}

	if err := authorizeIssueWatchLocalRun(ctx, options.Root, candidates); err != nil {
		result.FinishedAt = options.Now().UTC()
		return result, err
	}

	if len(candidates) > 0 {
		if err := ensureIssueWatchLocalExcludes(ctx, options.Root, filepath.Dir(options.StatePath), options.RunsRoot); err != nil {
			result.FinishedAt = options.Now().UTC()
			return result, err
		}
	}

	for i := range candidates {
		run, runErr := prepareIssueRun(ctx, options, candidates[i])
		if runErr != nil {
			result.FinishedAt = options.Now().UTC()
			return result, runErr
		}

		result.Runs = append(result.Runs, run)
		state.Runs[candidates[i].StateKey] = StateRun{
			UpdatedAt:       run.CompletedAt,
			IssueUpdatedAt:  run.IssueUpdatedAt,
			IssueID:         run.IssueID,
			IssueIdentifier: run.IssueIdentifier,
			RunID:           run.RunID,
			Status:          run.Status,
			ArtifactDir:     run.Artifacts.Dir,
			WorktreePath:    run.WorktreePath,
			Branch:          run.WorktreeBranch,
		}
		state.SchemaVersion = StateSchemaVersion
		state.UpdatedAt = options.Now().UTC()

		if err := SaveState(options.StatePath, state); err != nil {
			result.FinishedAt = options.Now().UTC()
			return result, err
		}
	}

	result.FinishedAt = options.Now().UTC()

	return result, nil
}

// SelectCandidates filters tracker issues by required labels and local state.
func SelectCandidates(issues []symphony.Issue, requiredLabels []string, state State) []Candidate {
	if len(issues) == 0 {
		return nil
	}

	var candidates []Candidate
	seen := map[string]struct{}{}
	for i := range issues {
		issue := issues[i]
		key := issueStateKey(issue)
		if key == "" {
			continue
		}

		if isTerminalIssueWatchState(issue.State) {
			continue
		}

		if state.Runs != nil {
			if existing := state.Runs[key]; strings.TrimSpace(existing.RunID) != "" {
				if !issueUpdatedAfterRun(issue, existing) {
					continue
				}
			}
		}

		if !hasRequiredLabels(issue.Labels, requiredLabels) {
			continue
		}

		if _, ok := seen[key]; ok {
			continue
		}
		seen[key] = struct{}{}

		candidates = append(candidates, candidateFromIssue(issue, key))
	}

	return candidates
}

// DiscoverGuidance finds repository-specific agent/harness instruction files.
func DiscoverGuidance(root string) ([]GuidanceFile, error) {
	root = filepath.Clean(strings.TrimSpace(root))
	if root == "" || root == "." {
		return nil, errors.New("issue watch guidance: root is required")
	}

	files, err := sourcepolicy.DiscoverHarnessFiles(root)
	if err != nil {
		return nil, fmt.Errorf("issue watch: discover guidance: %w", err)
	}

	guidance := make([]GuidanceFile, 0, len(files))
	for _, file := range files {
		guidance = append(guidance, GuidanceFile{
			Path:    file.Path,
			Kind:    file.Kind,
			Snippet: guidanceSnippet(file.Content),
		})
	}

	return guidance, nil
}

func discoverRunGuidance(root, worktreePath string) ([]GuidanceFile, error) {
	worktreeGuidance, err := DiscoverGuidance(worktreePath)
	if err != nil {
		return nil, err
	}

	if realCleanPath(root) == realCleanPath(worktreePath) {
		return worktreeGuidance, nil
	}

	rootGuidance, err := DiscoverGuidance(root)
	if err != nil {
		return nil, err
	}

	byPath := make(map[string]GuidanceFile, len(rootGuidance)+len(worktreeGuidance))
	for _, guidance := range rootGuidance {
		byPath[guidance.Path] = guidance
	}
	for _, guidance := range worktreeGuidance {
		byPath[guidance.Path] = guidance
	}

	merged := make([]GuidanceFile, 0, len(byPath))
	for _, guidance := range byPath {
		merged = append(merged, guidance)
	}
	sort.Slice(merged, func(i, j int) bool {
		return merged[i].Path < merged[j].Path
	})

	return merged, nil
}

// LoadState reads duplicate-avoidance state. Missing state is treated as empty.
func LoadState(path string) (State, error) {
	path = strings.TrimSpace(path)
	if path == "" {
		return State{}, errors.New("issue watch state: path is required")
	}

	data, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return emptyState(), nil
		}

		return State{}, fmt.Errorf("issue watch state: read %s: %w", path, err)
	}

	var state State
	if err := json.Unmarshal(data, &state); err != nil {
		return State{}, fmt.Errorf("issue watch state: decode %s: %w", path, err)
	}

	if state.Runs == nil {
		state.Runs = map[string]StateRun{}
	}

	if strings.TrimSpace(state.SchemaVersion) == "" {
		state.SchemaVersion = StateSchemaVersion
	}

	return state, nil
}

// SaveState writes duplicate-avoidance state atomically.
func SaveState(path string, state State) error {
	if state.Runs == nil {
		state.Runs = map[string]StateRun{}
	}
	if strings.TrimSpace(state.SchemaVersion) == "" {
		state.SchemaVersion = StateSchemaVersion
	}

	data, err := json.MarshalIndent(state, "", "  ")
	if err != nil {
		return fmt.Errorf("issue watch state: encode: %w", err)
	}
	data = append(data, '\n')

	if err := atomicfile.WriteFile(path, data, 0o600, ""); err != nil {
		return fmt.Errorf("issue watch state: write %s: %w", path, err)
	}

	return nil
}

func authorizeIssueWatchLocalRun(ctx context.Context, root string, candidates []Candidate) error {
	if len(candidates) == 0 {
		return nil
	}

	decision := permission.Evaluate(ctx, nil, permission.Request{
		Action: "prepare issue-watch local run",
		Source: "atteler.issue_watch.local",
		Target: root,
		Operations: []permission.Operation{
			{
				Kind:   permission.OperationWrite,
				Action: "write issue-watch local artifacts and state",
				Source: "atteler.issue_watch.local",
				Target: root,
			},
			{
				Kind:   permission.OperationExecute,
				Action: "inspect git metadata for issue-watch worktrees",
				Source: "atteler.issue_watch.local",
				Target: root,
			},
			{
				Kind:   permission.OperationGitMutation,
				Action: "prepare issue-watch git worktree isolation",
				Source: "atteler.issue_watch.local",
				Target: root,
			},
		},
	})
	if decision.Allowed {
		return nil
	}

	return &permission.Error{Decision: decision}
}

func ensureIssueWatchLocalExcludes(ctx context.Context, root string, dirs ...string) error {
	repoRoot, excludePath, err := issueWatchLocalExcludeTarget(ctx, root)
	if err != nil {
		return err
	}

	return appendIssueWatchLocalExcludes(excludePath, issueWatchExcludePatterns(repoRoot, root, dirs))
}

func issueWatchLocalExcludeTarget(ctx context.Context, root string) (repoRoot, excludePath string, err error) {
	repoRoot, err = gitOutput(ctx, root, "rev-parse", "--show-toplevel")
	if err != nil {
		return "", "", fmt.Errorf("issue watch: locate git repository for local excludes: %w", err)
	}

	excludePath, err = gitOutput(ctx, root, "rev-parse", "--git-path", "info/exclude")
	if err != nil {
		return "", "", fmt.Errorf("issue watch: locate git excludes file: %w", err)
	}
	if !filepath.IsAbs(excludePath) {
		excludePath = filepath.Join(root, excludePath)
	}

	return repoRoot, filepath.Clean(excludePath), nil
}

func appendIssueWatchLocalExcludes(excludePath string, patterns []string) error {
	if len(patterns) == 0 {
		return nil
	}

	data, readErr := os.ReadFile(excludePath)
	if readErr != nil && !errors.Is(readErr, os.ErrNotExist) {
		return fmt.Errorf("issue watch: read local git excludes %s: %w", excludePath, readErr)
	}

	missing := missingExactLines(string(data), patterns)
	if len(missing) == 0 {
		return nil
	}

	return writeIssueWatchLocalExcludes(excludePath, data, missing)
}

func writeIssueWatchLocalExcludes(excludePath string, data []byte, missing []string) error {
	if err := os.MkdirAll(filepath.Dir(excludePath), 0o750); err != nil {
		return fmt.Errorf("issue watch: create local git excludes dir %s: %w", filepath.Dir(excludePath), err)
	}

	mode := os.FileMode(0o644)
	if info, statErr := os.Stat(excludePath); statErr == nil {
		mode = info.Mode().Perm()
	}

	content := string(data)
	var builder strings.Builder
	builder.Write(data)
	if len(data) > 0 && !strings.HasSuffix(content, "\n") {
		builder.WriteByte('\n')
	}
	if !strings.Contains(content, "# Atteler issue-watch local artifacts") {
		builder.WriteString("# Atteler issue-watch local artifacts\n")
	}
	for _, pattern := range missing {
		builder.WriteString(pattern)
		builder.WriteByte('\n')
	}

	if err := os.WriteFile(excludePath, []byte(builder.String()), mode); err != nil {
		return fmt.Errorf("issue watch: update local git excludes %s: %w", excludePath, err)
	}

	return nil
}

func issueWatchExcludePatterns(repoRoot, root string, dirs []string) []string {
	originalRoot := cleanPath(root)
	repoRoot = realCleanPath(repoRoot)
	root = realCleanPath(root)
	if repoRoot == "" || repoRoot == "." || root == "" || root == "." {
		return nil
	}

	seen := map[string]struct{}{}
	var patterns []string
	for _, dir := range dirs {
		pattern, ok := issueWatchExcludePattern(repoRoot, originalRoot, root, dir)
		if !ok {
			continue
		}
		if _, ok := seen[pattern]; ok {
			continue
		}
		seen[pattern] = struct{}{}
		patterns = append(patterns, pattern)
	}
	sort.Strings(patterns)

	return patterns
}

func issueWatchExcludePattern(repoRoot, originalRoot, realRoot, dir string) (string, bool) {
	dir = strings.TrimSpace(dir)
	if dir == "" || dir == "." {
		return "", false
	}
	if !filepath.IsAbs(dir) {
		dir = filepath.Join(originalRoot, dir)
	} else {
		dir = cleanPath(dir)
	}
	if relToRoot, err := filepath.Rel(originalRoot, dir); err == nil && pathIsRelativeInside(relToRoot) {
		dir = filepath.Join(realRoot, relToRoot)
	}

	rel, err := filepath.Rel(repoRoot, filepath.Clean(dir))
	if err != nil || !pathIsRelativeInside(rel) {
		return "", false
	}

	return "/" + strings.Trim(filepath.ToSlash(rel), "/") + "/", true
}

func cleanPath(path string) string {
	path = strings.TrimSpace(path)
	if path == "" {
		return ""
	}

	if abs, err := filepath.Abs(path); err == nil {
		path = abs
	}

	return filepath.Clean(path)
}

func realCleanPath(path string) string {
	path = cleanPath(path)
	if path == "" {
		return ""
	}

	if realPath, err := filepath.EvalSymlinks(path); err == nil {
		return filepath.Clean(realPath)
	}

	return path
}

func pathIsRelativeInside(rel string) bool {
	return rel != "." && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}

func gitOutput(ctx context.Context, root string, args ...string) (string, error) {
	out, err := gitOutputBytes(ctx, root, args...)
	if err != nil {
		return "", err
	}

	return strings.TrimSpace(string(out)), nil
}

//nolint:gosec // Static git invocations are limited to repository metadata/diff lookups.
func gitOutputBytes(ctx context.Context, root string, args ...string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, "git", append([]string{"-C", root}, args...)...)
	out, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("git %s: %w", strings.Join(args, " "), err)
	}

	return out, nil
}

func missingExactLines(content string, lines []string) []string {
	var missing []string
	for _, line := range lines {
		if !hasExactLine(content, line) {
			missing = append(missing, line)
		}
	}

	return missing
}

func hasExactLine(content, line string) bool {
	for existing := range strings.SplitSeq(content, "\n") {
		if strings.TrimSpace(existing) == line {
			return true
		}
	}

	return false
}

func normalizeOptions(options Options) (Options, error) {
	options.Root = filepath.Clean(strings.TrimSpace(options.Root))
	if options.Root == "" || options.Root == "." {
		return Options{}, errors.New("issue watch: root is required")
	}

	if options.Tracker == nil {
		return Options{}, errors.New("issue watch: tracker is required")
	}

	if len(trimNonEmpty(options.Labels)) == 0 && !options.AllowEmptyLabels {
		return Options{}, errors.New("issue watch: at least one label is required")
	}

	if options.Now == nil {
		options.Now = time.Now
	}

	if options.CreateWorktree == nil {
		options.CreateWorktree = worktree.CreateContext
	}

	options.Command = strings.TrimSpace(options.Command)
	options.ValidationCommands = trimNonEmpty(options.ValidationCommands)
	if options.CommandTimeout <= 0 {
		options.CommandTimeout = defaultCommandTimeout
	}
	if options.CommandOutputMaxBytes <= 0 {
		options.CommandOutputMaxBytes = commandOutputMaxBytes
	}

	if strings.TrimSpace(options.StatePath) == "" {
		options.StatePath = filepath.Join(options.Root, defaultStatePath)
	} else {
		options.StatePath = pathUnderRoot(options.Root, options.StatePath)
	}

	if strings.TrimSpace(options.RunsRoot) == "" {
		options.RunsRoot = filepath.Join(options.Root, defaultRunsRoot)
	} else {
		options.RunsRoot = pathUnderRoot(options.Root, options.RunsRoot)
	}

	options.Labels = trimNonEmpty(options.Labels)

	return options, nil
}

func pathUnderRoot(root, path string) string {
	path = strings.TrimSpace(path)
	if path == "" {
		return ""
	}
	if filepath.IsAbs(path) {
		return filepath.Clean(path)
	}

	return filepath.Join(root, path)
}

func prepareIssueRun(ctx context.Context, options Options, candidate Candidate) (RunMetadata, error) {
	startedAt := options.Now().UTC()
	runID := runIDFor(candidate, startedAt)
	issueIDPath := candidate.IssueIDPath
	artifactDir := filepath.Join(options.RunsRoot, issueIDPath, runID)
	artifacts := runArtifacts(artifactDir)
	worktreeSessionID := worktreeSessionIDFor(issueIDPath, startedAt)

	info, err := options.CreateWorktree(ctx, options.Root, worktreeSessionID)
	if err != nil {
		return RunMetadata{}, fmt.Errorf("issue watch: create worktree for %s: %w", candidate.Identifier, err)
	}

	guidance, err := discoverRunGuidance(options.Root, info.Path)
	if err != nil {
		return RunMetadata{}, err
	}

	if err := os.MkdirAll(artifactDir, 0o750); err != nil {
		return RunMetadata{}, fmt.Errorf("issue watch: create run dir %s: %w", artifactDir, err)
	}

	validation := ValidationReport{
		LogPath: artifacts.ValidationLog,
		Summary: "validation not run; issue watch MVP prepared a local worktree and artifacts only",
		Passed:  false,
		Run:     false,
	}
	workflow := WorkflowReport{
		Summary: "implementation command not configured; local issue-watch artifacts prepared only",
		Passed:  false,
		Run:     false,
	}

	metadata := RunMetadata{
		StartedAt:       startedAt,
		CompletedAt:     options.Now().UTC(),
		IssueUpdatedAt:  candidate.UpdatedAt,
		IssueURL:        candidate.URL,
		Guidance:        guidance,
		Workflow:        workflow,
		Validation:      validation,
		Artifacts:       artifacts,
		SchemaVersion:   RunSchemaVersion,
		RunID:           runID,
		Status:          statusNeedsHuman,
		StatusReason:    "local issue-watch run prepared; implementation and validation await an explicit agent execution step",
		Repository:      strings.TrimSpace(options.Repository),
		IssueID:         candidate.ID,
		IssueIdentifier: candidate.Identifier,
		IssueTitle:      candidate.Title,
		IssueState:      candidate.State,
		StateKey:        candidate.StateKey,
		WorktreePath:    info.Path,
		WorktreeBranch:  info.Branch,
		BaseBranch:      info.BaseBranch,
		Safety:          "local-only: no push, pull request, or tracker comment is performed by issue watch",
		Labels:          append([]string(nil), candidate.Labels...),
	}

	if err := writeRunArtifacts(candidate, metadata); err != nil {
		return RunMetadata{}, err
	}
	if err := writePatchFile(metadata.Artifacts.Patch, nil); err != nil {
		return RunMetadata{}, err
	}

	if issueWatchWorkflowConfigured(options) {
		if err := runIssueWorkflow(ctx, options, candidate, &metadata); err != nil {
			return RunMetadata{}, err
		}
		metadata.CompletedAt = options.Now().UTC()
		if err := writeRunArtifacts(candidate, metadata); err != nil {
			return RunMetadata{}, err
		}
		if err := refreshPatchArtifact(ctx, metadata.WorktreePath, metadata.Artifacts.Patch); err != nil {
			return RunMetadata{}, err
		}
	}

	return metadata, nil
}

func writeRunArtifacts(candidate Candidate, metadata RunMetadata) error {
	if err := writeJSONFile(metadata.Artifacts.IssueJSON, candidate); err != nil {
		return err
	}

	if err := os.WriteFile(metadata.Artifacts.Plan, []byte(renderPlan(candidate, metadata)), 0o600); err != nil {
		return fmt.Errorf("issue watch: write plan %s: %w", metadata.Artifacts.Plan, err)
	}

	if err := os.WriteFile(metadata.Artifacts.Implementation, []byte(renderImplementationReport(metadata)), 0o600); err != nil {
		return fmt.Errorf("issue watch: write implementation report %s: %w", metadata.Artifacts.Implementation, err)
	}

	if err := os.WriteFile(metadata.Artifacts.ValidationLog, []byte(renderValidationLog(metadata)), 0o600); err != nil {
		return fmt.Errorf("issue watch: write validation log %s: %w", metadata.Artifacts.ValidationLog, err)
	}

	if err := writeJSONFile(metadata.Artifacts.RunJSON, metadata); err != nil {
		return err
	}

	return nil
}

func issueWatchWorkflowConfigured(options Options) bool {
	return strings.TrimSpace(options.Command) != "" || len(trimNonEmpty(options.ValidationCommands)) > 0
}

func runIssueWorkflow(ctx context.Context, options Options, candidate Candidate, metadata *RunMetadata) error {
	if metadata == nil {
		return errors.New("issue watch workflow: metadata is required")
	}

	if command := strings.TrimSpace(options.Command); command != "" {
		result := runIssueWatchLocalCommand(ctx, options, candidate, *metadata, "implementation", command)
		metadata.Workflow = WorkflowReport{
			Result:  &result,
			Command: command,
			Summary: issueWatchCommandSummary("implementation command", result),
			Passed:  result.Passed,
			Run:     true,
		}
		if !result.Passed {
			metadata.Status = statusFailed
			metadata.StatusReason = metadata.Workflow.Summary
			metadata.Validation = ValidationReport{
				LogPath: metadata.Artifacts.ValidationLog,
				Summary: "validation skipped because implementation command failed",
				Passed:  false,
				Run:     false,
			}

			return refreshRunEvidence(ctx, metadata)
		}
	} else {
		metadata.Workflow = WorkflowReport{
			Summary: "implementation command not configured",
			Passed:  false,
			Run:     false,
		}
	}

	metadata.Validation = runIssueWatchValidation(ctx, options, candidate, *metadata)
	switch {
	case metadata.Validation.Run && metadata.Validation.Passed:
		metadata.Status = statusSucceeded
		metadata.StatusReason = "local issue-watch workflow completed and validation passed"
	case metadata.Validation.Run:
		metadata.Status = statusFailed
		metadata.StatusReason = metadata.Validation.Summary
	case metadata.Workflow.Run && metadata.Workflow.Passed:
		metadata.Status = statusNeedsHuman
		metadata.StatusReason = "implementation command completed; validation is not configured"
	default:
		metadata.Status = statusNeedsHuman
		metadata.StatusReason = "local issue-watch run prepared; implementation awaits an explicit agent execution step"
	}

	return refreshRunEvidence(ctx, metadata)
}

func refreshRunEvidence(ctx context.Context, metadata *RunMetadata) error {
	changed, err := changedFiles(ctx, metadata.WorktreePath)
	if err != nil {
		return err
	}

	metadata.ChangedFiles = changed

	return nil
}

func runIssueWatchValidation(ctx context.Context, options Options, candidate Candidate, metadata RunMetadata) ValidationReport {
	commands := trimNonEmpty(options.ValidationCommands)
	if len(commands) == 0 {
		return ValidationReport{
			LogPath: metadata.Artifacts.ValidationLog,
			Summary: "validation not configured",
			Passed:  false,
			Run:     false,
		}
	}

	report := ValidationReport{
		LogPath:  metadata.Artifacts.ValidationLog,
		Commands: append([]string(nil), commands...),
		Passed:   true,
		Run:      true,
	}
	for _, command := range commands {
		result := runIssueWatchLocalCommand(ctx, options, candidate, metadata, "validation", command)
		report.Results = append(report.Results, result)
		if !result.Passed {
			report.Passed = false
		}
	}

	if report.Passed {
		report.Summary = fmt.Sprintf("validation passed (%d command(s))", len(report.Results))
	} else {
		report.Summary = fmt.Sprintf("validation failed (%d command(s))", len(report.Results))
	}

	return report
}

func runIssueWatchLocalCommand(ctx context.Context, options Options, candidate Candidate, metadata RunMetadata, phase, command string) CommandResult {
	started := options.Now().UTC()
	result, err := attshell.RunBash(ctx, attshell.Options{
		Command:        command,
		Dir:            metadata.WorktreePath,
		Timeout:        options.CommandTimeout,
		MaxOutputBytes: options.CommandOutputMaxBytes,
		Env:            issueWatchCommandEnv(options, candidate, metadata),
		Permission:     issueWatchLocalCommandPermission(ctx),
		Audit: attshell.AuditContext{
			Caller:          "issuewatch." + phase,
			SessionID:       metadata.RunID,
			SessionPath:     metadata.Artifacts.Dir,
			IssueID:         metadata.IssueID,
			IssueIdentifier: metadata.IssueIdentifier,
			Autonomy:        "local-only",
		},
	})

	finished := options.Now().UTC()
	if !result.StartedAt.IsZero() {
		started = result.StartedAt.UTC()
		finished = result.StartedAt.Add(result.Duration).UTC()
	}

	commandResult := CommandResult{
		StartedAt:       started,
		CompletedAt:     finished,
		Duration:        result.Duration,
		Command:         command,
		Stdout:          result.Stdout,
		Stderr:          result.Stderr,
		ExitError:       result.ExitError,
		OutputTruncated: result.OutputTruncated,
		Passed:          err == nil,
	}
	if err != nil {
		commandResult.Error = err.Error()
	}

	return commandResult
}

func issueWatchLocalCommandPermission(ctx context.Context) *permission.Policy {
	policy := permission.DefaultPolicy()
	if inherited := permission.PolicyFromContext(ctx); inherited != nil {
		policy = clonePermissionPolicy(*inherited)
	}

	policy.Name = "issue-watch-local"
	policy.SetMode(permission.OperationNetwork, permission.ModeDeny)
	policy.SetMode(permission.OperationCredentialAccess, permission.ModeDeny)

	return &policy
}

func clonePermissionPolicy(policy permission.Policy) permission.Policy {
	if len(policy.Modes) == 0 {
		return policy
	}

	policy.Modes = maps.Clone(policy.Modes)

	return policy
}

func issueWatchCommandEnv(options Options, candidate Candidate, metadata RunMetadata) map[string]string {
	env := map[string]string{
		"ATTELER_ISSUE_WATCH":            "1",
		"ATTELER_ISSUE_WATCH_RUN_ID":     metadata.RunID,
		"ATTELER_ISSUE_WATCH_RUN_DIR":    metadata.Artifacts.Dir,
		"ATTELER_ISSUE_WATCH_PLAN":       metadata.Artifacts.Plan,
		"ATTELER_ISSUE_WATCH_ISSUE_JSON": metadata.Artifacts.IssueJSON,
		"ATTELER_ISSUE_WATCH_PATCH":      metadata.Artifacts.Patch,
		"ATTELER_ISSUE_ID":               candidate.ID,
		"ATTELER_ISSUE_IDENTIFIER":       candidate.Identifier,
		"ATTELER_ISSUE_TITLE":            candidate.Title,
		"ATTELER_REPOSITORY":             strings.TrimSpace(options.Repository),
	}
	if candidate.URL != nil {
		env["ATTELER_ISSUE_URL"] = strings.TrimSpace(*candidate.URL)
	}

	return env
}

func issueWatchCommandSummary(prefix string, result CommandResult) string {
	if result.Passed {
		return prefix + " passed"
	}
	if result.Error != "" {
		return prefix + " failed: " + result.Error
	}
	if result.ExitError != "" {
		return prefix + " failed: " + result.ExitError
	}

	return prefix + " failed"
}

func changedFiles(ctx context.Context, worktreePath string) ([]string, error) {
	out, err := gitOutput(ctx, worktreePath, "status", "--short", "--untracked-files=all")
	if err != nil {
		return nil, fmt.Errorf("issue watch: collect changed files: %w", err)
	}
	if strings.TrimSpace(out) == "" {
		return nil, nil
	}

	var changed []string
	for line := range strings.SplitSeq(out, "\n") {
		line = strings.TrimSpace(line)
		if line != "" {
			changed = append(changed, line)
		}
	}

	return changed, nil
}

func refreshPatchArtifact(ctx context.Context, worktreePath, patchPath string) error {
	data, err := gitOutputBytes(ctx, worktreePath, "diff", "--binary", "HEAD", "--")
	if err != nil {
		return fmt.Errorf("issue watch: refresh patch artifact: %w", err)
	}

	untracked, err := untrackedFiles(ctx, worktreePath)
	if err != nil {
		return err
	}

	patch := append([]byte(nil), data...)
	for _, path := range untracked {
		diff, diffErr := untrackedFilePatch(ctx, worktreePath, path)
		if diffErr != nil {
			return diffErr
		}
		if len(diff) == 0 {
			continue
		}
		if len(patch) > 0 && patch[len(patch)-1] != '\n' {
			patch = append(patch, '\n')
		}
		patch = append(patch, diff...)
	}

	return writePatchFile(patchPath, patch)
}

func untrackedFiles(ctx context.Context, worktreePath string) ([]string, error) {
	data, err := gitOutputBytes(ctx, worktreePath, "ls-files", "--others", "--exclude-standard", "-z")
	if err != nil {
		return nil, fmt.Errorf("issue watch: collect untracked files: %w", err)
	}
	if len(data) == 0 {
		return nil, nil
	}

	var files []string
	for path := range strings.SplitSeq(string(data), "\x00") {
		path = strings.TrimSpace(path)
		if path == "" {
			continue
		}
		if !pathIsRelativeInside(filepath.Clean(filepath.FromSlash(path))) {
			return nil, fmt.Errorf("issue watch: unsafe untracked path %q", path)
		}
		files = append(files, filepath.ToSlash(filepath.Clean(filepath.FromSlash(path))))
	}
	sort.Strings(files)

	return files, nil
}

func untrackedFilePatch(ctx context.Context, worktreePath, path string) ([]byte, error) {
	path = strings.TrimSpace(path)
	if path == "" {
		return nil, nil
	}

	cmd := exec.CommandContext(ctx, "git", "-C", worktreePath, "diff", "--binary", "--no-index", "--", os.DevNull, path)
	data, err := cmd.Output()
	if err == nil {
		return data, nil
	}

	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) && exitErr.ExitCode() == 1 {
		return data, nil
	}

	return nil, fmt.Errorf("issue watch: render untracked patch for %s: %w", path, err)
}

func writePatchFile(path string, data []byte) error {
	if err := os.WriteFile(path, data, 0o600); err != nil {
		return fmt.Errorf("issue watch: write patch %s: %w", path, err)
	}

	return nil
}

func renderPlan(candidate Candidate, metadata RunMetadata) string {
	var b strings.Builder
	fmt.Fprintf(&b, "# Issue watch plan for %s\n\n", firstNonEmpty(candidate.Identifier, candidate.ID))
	fmt.Fprintf(&b, "- Title: %s\n", candidate.Title)
	fmt.Fprintf(&b, "- State: %s\n", candidate.State)
	if candidate.URL != nil && strings.TrimSpace(*candidate.URL) != "" {
		fmt.Fprintf(&b, "- URL: %s\n", strings.TrimSpace(*candidate.URL))
	}
	fmt.Fprintf(&b, "- Local worktree: %s\n", metadata.WorktreePath)
	fmt.Fprintf(&b, "- Branch: %s\n", metadata.WorktreeBranch)
	fmt.Fprintf(&b, "- Safety: %s\n\n", metadata.Safety)
	if candidate.Description != nil && strings.TrimSpace(*candidate.Description) != "" {
		b.WriteString("## Issue description\n\n")
		b.WriteString(strings.TrimSpace(*candidate.Description))
		b.WriteString("\n\n")
	}
	b.WriteString(renderIssueDiscussion(candidate.Comments))

	b.WriteString("## Harness constraints discovered\n\n")
	if len(metadata.Guidance) == 0 {
		b.WriteString("No AGENTS.md, CLAUDE.md, Cursor rules, or similar harness instruction files were found in the prepared worktree.\n\n")
	} else {
		for _, guidance := range metadata.Guidance {
			fmt.Fprintf(&b, "### %s (%s)\n\n", guidance.Path, guidance.Kind)
			if strings.TrimSpace(guidance.Snippet) == "" {
				b.WriteString("Discovered; content was empty after trimming.\n\n")
				continue
			}
			b.WriteString("```text\n")
			b.WriteString(guidance.Snippet)
			if !strings.HasSuffix(guidance.Snippet, "\n") {
				b.WriteByte('\n')
			}
			b.WriteString("```\n\n")
		}
	}

	b.WriteString("## Local-first implementation loop\n\n")
	b.WriteString("1. Inspect the issue metadata in issue.json and this plan.\n")
	b.WriteString("2. Implement changes only inside the isolated worktree.\n")
	b.WriteString("3. Run focused validation and keep command evidence available for the final report.\n")
	b.WriteString("4. Summarize files changed, tests run, validation status, and remaining risks.\n")
	b.WriteString("5. Leave reviewable local changes in the worktree; Atteler refreshes implementation.md, validation.log, patch.diff, and run.json from captured evidence when command hooks finish.\n\n")
	b.WriteString("Issue watch intentionally does not push branches, open pull requests, or post tracker comments.\n")

	return b.String()
}

func renderIssueDiscussion(comments []symphony.IssueComment) string {
	if len(comments) == 0 {
		return ""
	}

	var b strings.Builder
	b.WriteString("## Issue discussion\n\n")
	limit := min(len(comments), maxIssuePlanComments)
	for i := range limit {
		b.WriteString(renderIssueComment(i+1, comments[i]))
	}
	if len(comments) > limit {
		fmt.Fprintf(&b, "_%d additional issue comment(s) are available in issue.json._\n\n", len(comments)-limit)
	}

	return b.String()
}

func renderIssueComment(number int, comment symphony.IssueComment) string {
	var b strings.Builder
	author := firstNonEmpty(comment.Author, "unknown")
	fmt.Fprintf(&b, "### Comment %d by %s\n\n", number, author)
	if comment.CreatedAt != nil {
		fmt.Fprintf(&b, "- Created: %s\n", comment.CreatedAt.UTC().Format(time.RFC3339))
	}
	if comment.URL != nil && strings.TrimSpace(*comment.URL) != "" {
		fmt.Fprintf(&b, "- URL: %s\n", strings.TrimSpace(*comment.URL))
	}

	body := truncateRunes(strings.TrimSpace(comment.Body), maxIssueCommentRunes)
	if body == "" {
		b.WriteString("No comment body.\n\n")
		return b.String()
	}

	b.WriteString("\n```text\n")
	b.WriteString(body)
	if !strings.HasSuffix(body, "\n") {
		b.WriteByte('\n')
	}
	b.WriteString("```\n\n")

	return b.String()
}

func renderImplementationReport(metadata RunMetadata) string {
	var b strings.Builder
	fmt.Fprintf(&b, "# Issue watch implementation report for %s\n\n", firstNonEmpty(metadata.IssueIdentifier, metadata.IssueID))
	fmt.Fprintf(&b, "Status: %s\n\n", metadata.Status)
	fmt.Fprintf(&b, "Reason: %s\n\n", metadata.StatusReason)
	fmt.Fprintf(&b, "Worktree: %s\n\n", metadata.WorktreePath)
	b.WriteString("## Implementation workflow\n\n")
	if metadata.Workflow.Run {
		fmt.Fprintf(&b, "%s\n\n", metadata.Workflow.Summary)
		if metadata.Workflow.Command != "" {
			fmt.Fprintf(&b, "- Command: `%s`\n", metadata.Workflow.Command)
		}
		if metadata.Workflow.Result != nil {
			fmt.Fprintf(&b, "- Passed: %t\n", metadata.Workflow.Result.Passed)
		}
		b.WriteByte('\n')
	} else {
		fmt.Fprintf(&b, "%s\n\n", firstNonEmpty(metadata.Workflow.Summary, "implementation command not configured"))
	}
	b.WriteString("## Validation\n\n")
	fmt.Fprintf(&b, "%s\n\n", metadata.Validation.Summary)
	if len(metadata.Validation.Commands) > 0 {
		b.WriteString("Validation commands:\n")
		for _, command := range metadata.Validation.Commands {
			fmt.Fprintf(&b, "- `%s`\n", command)
		}
		b.WriteByte('\n')
	}
	b.WriteString("## Files changed\n\n")
	if len(metadata.ChangedFiles) == 0 {
		b.WriteString("No changed files recorded yet.\n\n")
	} else {
		for _, file := range metadata.ChangedFiles {
			fmt.Fprintf(&b, "- `%s`\n", file)
		}
		b.WriteByte('\n')
	}
	b.WriteString("## Evidence checklist\n\n")
	if len(metadata.ChangedFiles) == 0 {
		b.WriteString("- Files changed: none recorded\n")
	} else {
		b.WriteString("- Files changed: see Files changed section\n")
	}
	if metadata.Validation.Run {
		b.WriteString("- Tests run: see Validation section and validation.log\n")
	} else {
		b.WriteString("- Tests run: not recorded yet\n")
	}
	b.WriteString("- Command outputs: see validation.log\n")
	b.WriteString("- Patch: see patch.diff\n")

	return b.String()
}

func renderValidationLog(metadata RunMetadata) string {
	var b strings.Builder
	b.WriteString("implementation_workflow:\n")
	fmt.Fprintf(&b, "  run: %t\n", metadata.Workflow.Run)
	fmt.Fprintf(&b, "  passed: %t\n", metadata.Workflow.Passed)
	fmt.Fprintf(&b, "  summary: %s\n", metadata.Workflow.Summary)
	if metadata.Workflow.Command != "" {
		fmt.Fprintf(&b, "  command: %s\n", metadata.Workflow.Command)
	}
	if metadata.Workflow.Result != nil {
		renderCommandResult(&b, "  result", *metadata.Workflow.Result)
	}

	report := metadata.Validation
	fmt.Fprintf(&b, "validation_run: %t\n", report.Run)
	fmt.Fprintf(&b, "validation_passed: %t\n", report.Passed)
	fmt.Fprintf(&b, "summary: %s\n", report.Summary)
	if len(report.Commands) > 0 {
		b.WriteString("commands:\n")
		for _, command := range report.Commands {
			fmt.Fprintf(&b, "- %s\n", command)
		}
	}
	for i := range report.Results {
		renderCommandResult(&b, fmt.Sprintf("validation_result_%d", i+1), report.Results[i])
	}

	return b.String()
}

func renderCommandResult(b *strings.Builder, heading string, result CommandResult) {
	fmt.Fprintf(b, "%s:\n", heading)
	fmt.Fprintf(b, "  command: %s\n", result.Command)
	fmt.Fprintf(b, "  passed: %t\n", result.Passed)
	if !result.StartedAt.IsZero() {
		fmt.Fprintf(b, "  started_at: %s\n", result.StartedAt.UTC().Format(time.RFC3339))
	}
	if !result.CompletedAt.IsZero() {
		fmt.Fprintf(b, "  completed_at: %s\n", result.CompletedAt.UTC().Format(time.RFC3339))
	}
	if result.Duration > 0 {
		fmt.Fprintf(b, "  duration: %s\n", result.Duration)
	}
	if result.ExitError != "" {
		fmt.Fprintf(b, "  exit_error: %s\n", result.ExitError)
	}
	if result.Error != "" {
		fmt.Fprintf(b, "  error: %s\n", result.Error)
	}
	if result.OutputTruncated {
		b.WriteString("  output_truncated: true\n")
	}
	if strings.TrimSpace(result.Stdout) != "" {
		b.WriteString("  stdout: |\n")
		writeIndentedBlock(b, result.Stdout, "    ")
	}
	if strings.TrimSpace(result.Stderr) != "" {
		b.WriteString("  stderr: |\n")
		writeIndentedBlock(b, result.Stderr, "    ")
	}
}

func writeIndentedBlock(b *strings.Builder, content, indent string) {
	content = strings.TrimRight(content, "\n")
	for line := range strings.SplitSeq(content, "\n") {
		b.WriteString(indent)
		b.WriteString(line)
		b.WriteByte('\n')
	}
}

func runArtifacts(dir string) RunArtifacts {
	return RunArtifacts{
		Dir:            dir,
		IssueJSON:      filepath.Join(dir, issueJSONFile),
		Plan:           filepath.Join(dir, planFile),
		Implementation: filepath.Join(dir, implementationFile),
		ValidationLog:  filepath.Join(dir, validationLogFile),
		Patch:          filepath.Join(dir, patchFile),
		RunJSON:        filepath.Join(dir, runJSONFile),
	}
}

func writeJSONFile(path string, value any) error {
	data, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		return fmt.Errorf("issue watch: encode %s: %w", path, err)
	}
	data = append(data, '\n')

	if err := os.WriteFile(path, data, 0o600); err != nil {
		return fmt.Errorf("issue watch: write %s: %w", path, err)
	}

	return nil
}

func candidateFromIssue(issue symphony.Issue, key string) Candidate {
	identifier := strings.TrimSpace(issue.Identifier)
	if identifier == "" {
		identifier = strings.TrimSpace(issue.ID)
	}

	return Candidate{
		UpdatedAt:   issue.UpdatedAt,
		CreatedAt:   issue.CreatedAt,
		Description: issue.Description,
		Priority:    issue.Priority,
		URL:         issue.URL,
		ID:          strings.TrimSpace(issue.ID),
		Identifier:  identifier,
		Title:       strings.TrimSpace(issue.Title),
		State:       strings.TrimSpace(issue.State),
		Labels:      append([]string(nil), issue.Labels...),
		BlockedBy:   append([]symphony.BlockerRef(nil), issue.BlockedBy...),
		Comments:    append([]symphony.IssueComment(nil), issue.Comments...),
		StateKey:    key,
		IssueIDPath: safeID(firstNonEmpty(identifier, issue.ID, "issue"), maxSafeIDLength),
	}
}

func hasRequiredLabels(issueLabels, requiredLabels []string) bool {
	required := normalizedSet(requiredLabels)
	if len(required) == 0 {
		return true
	}

	have := normalizedSet(issueLabels)
	for label := range required {
		if !have[label] {
			return false
		}
	}

	return true
}

func normalizedSet(values []string) map[string]bool {
	set := map[string]bool{}
	for _, value := range values {
		value = strings.ToLower(strings.TrimSpace(value))
		if value != "" {
			set[value] = true
		}
	}

	return set
}

func trimNonEmpty(values []string) []string {
	out := make([]string, 0, len(values))
	for _, value := range values {
		value = strings.TrimSpace(value)
		if value != "" {
			out = append(out, value)
		}
	}

	return out
}

func issueStateKey(issue symphony.Issue) string {
	return firstNonEmpty(issue.ID, issue.Identifier)
}

func isTerminalIssueWatchState(state string) bool {
	switch strings.ToLower(strings.TrimSpace(state)) {
	case "closed":
		return true
	default:
		return false
	}
}

func issueUpdatedAfterRun(issue symphony.Issue, existing StateRun) bool {
	if issue.UpdatedAt == nil || existing.IssueUpdatedAt == nil {
		return false
	}

	return issue.UpdatedAt.After(existing.IssueUpdatedAt.UTC())
}

func emptyState() State {
	return State{SchemaVersion: StateSchemaVersion, Runs: map[string]StateRun{}}
}

func runIDFor(candidate Candidate, now time.Time) string {
	return now.UTC().Format("20060102T150405Z") + "-" + safeID(firstNonEmpty(candidate.Identifier, candidate.ID, "issue"), maxSafeIDLength)
}

func worktreeSessionIDFor(issueIDPath string, now time.Time) string {
	base := safeID("issue-"+issueIDPath+"-"+now.UTC().Format("20060102T150405Z"), maxWorktreeSessionID)
	if base == "" {
		return "issue-" + now.UTC().Format("20060102T150405Z")
	}

	return base
}

//nolint:cyclop // The sanitizer is deliberately explicit about allowed and replacement characters.
func safeID(value string, maxLen int) string {
	value = strings.TrimSpace(value)
	if value == "" {
		return ""
	}

	var b strings.Builder
	lastDash := false
	for _, r := range value {
		ok := (r >= 'A' && r <= 'Z') || (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') || r == '.' || r == '_' || r == '-'
		if ok {
			b.WriteRune(r)
			lastDash = false
			continue
		}

		if !lastDash {
			b.WriteByte('-')
			lastDash = true
		}
	}

	out := strings.Trim(b.String(), "-._")
	if out == "" {
		out = "issue"
	}

	if maxLen <= 0 || len(out) <= maxLen {
		return out
	}

	hash := fnv.New32a()
	_, _ = hash.Write([]byte(out))
	suffix := fmt.Sprintf("-%08x", hash.Sum32())
	keep := maxLen - len(suffix)
	if keep < 1 {
		keep = maxLen
		suffix = ""
	}

	return strings.TrimRight(out[:keep], "-._") + suffix
}

func guidanceSnippet(content string) string {
	content = strings.TrimSpace(content)
	if len([]rune(content)) <= maxGuidancePlanRunes {
		return content
	}

	return truncateRunes(content, maxGuidancePlanRunes)
}

func truncateRunes(content string, maxRunes int) string {
	if maxRunes <= 0 {
		return ""
	}

	runes := []rune(content)
	if len(runes) <= maxRunes {
		return content
	}

	return string(runes[:maxRunes]) + "\n..."
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		value = strings.TrimSpace(value)
		if value != "" {
			return value
		}
	}

	return ""
}
