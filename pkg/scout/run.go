// Package scout creates local-first product discovery and roadmap artifacts.
//
//nolint:wsl_v5 // Artifact assembly reads as a sequential pipeline; blank-line cuddling adds noise here.
package scout

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/tommoulard/atteler/pkg/privacy"
	"github.com/tommoulard/atteler/pkg/tournament"
)

const (
	// SchemaVersion is the machine-readable scout run metadata schema version.
	SchemaVersion = "atteler.scout.run.v1"

	scoutReportFile      = "scout.md"
	ideasFile            = "ideas.jsonl"
	competitorsFile      = "competitors.jsonl"
	roadmapsFile         = "roadmaps.jsonl"
	tasksFile            = "tasks.generated.yaml"
	runFile              = "run.json"
	sourceTypeGuidance   = "project_guidance"
	sourceTypeRepository = "repository_file"

	maxGuidanceFiles   = 64
	maxSourceFiles     = 48
	maxSourceBytes     = 64 * 1024
	maxReportLineRunes = 180
	defaultIdeaCount   = 8
	defaultVariants    = 3
	labelHigh          = "high"
	labelMedium        = "medium"
	labelLow           = "low"
)

// EvidenceBestPractice is the recommended reliability posture for scout reports.
const EvidenceBestPractice = "Scout recommendations should cite evidence where available: competitor docs, public product pages, repository files, project guidance, prior sessions, command output, issue history, or tests. Speculative ideas are allowed, but should be labeled clearly."

// RunRequest configures one scout run.
//
// The MVP is local-first: it reads repository files and harness guidance, records
// competitor names or URLs supplied by the user, and writes deterministic
// discovery artifacts. Autonomous web search can be added later without changing
// the artifact contract.
type RunRequest struct {
	Now           time.Time
	Prompt        string
	Root          string
	OutputDir     string
	RunID         string
	Area          string
	Competitors   []string
	Sources       []string
	GenerateTasks bool
	Tournament    bool
	VariantCount  int
}

// RunResult describes the created scout run artifacts.
type RunResult struct {
	CreatedAt   time.Time
	RunID       string
	Dir         string
	Files       []string
	Ideas       []Idea
	Competitors []Competitor
}

// Evidence maps an idea back to a concrete supporting artifact or source.
type Evidence struct {
	Kind    string `json:"kind"`
	URL     string `json:"url,omitempty"`
	Path    string `json:"path,omitempty"`
	Source  string `json:"source,omitempty"`
	Excerpt string `json:"excerpt,omitempty"`
}

// Idea is one structured product or engineering idea.
type Idea struct {
	Title               string     `json:"title"`
	Summary             string     `json:"summary"`
	Fit                 string     `json:"fit"`
	Complexity          string     `json:"complexity"`
	Risk                string     `json:"risk"`
	SuggestedMVP        string     `json:"suggested_mvp"`
	RelatedFilesOrAreas []string   `json:"related_files_or_areas"`
	Evidence            []Evidence `json:"evidence,omitempty"`
	SourceVariants      []string   `json:"source_variants,omitempty"`
	Rank                int        `json:"rank"`
	Score               int        `json:"score"`
	Speculative         bool       `json:"speculative"`
}

// Competitor records one user-supplied inspiration source.
type Competitor struct {
	RecordedAt time.Time `json:"recorded_at"`
	Name       string    `json:"name"`
	URL        string    `json:"url,omitempty"`
	SourceType string    `json:"source_type"`
	Notes      string    `json:"notes"`
}

//nolint:govet // JSON field order keeps the human-facing artifact metadata grouped.
type runRecord struct {
	CreatedAt       time.Time           `json:"created_at"`
	Tournament      tournamentRunRecord `json:"tournament"`
	Artifacts       map[string]string   `json:"artifacts"`
	GuidanceFiles   []string            `json:"guidance_files,omitempty"`
	RepositoryFiles []string            `json:"repository_files,omitempty"`
	Competitors     []string            `json:"competitors,omitempty"`
	Sources         []string            `json:"sources,omitempty"`
	Notes           []string            `json:"notes,omitempty"`
	Schema          string              `json:"schema"`
	RunID           string              `json:"run_id"`
	Prompt          string              `json:"prompt"`
	Area            string              `json:"area,omitempty"`
	Root            string              `json:"root"`
	OutputDir       string              `json:"output_dir"`
	IdeaCount       int                 `json:"idea_count"`
	CompetitorCount int                 `json:"competitor_count"`
	GenerateTasks   bool                `json:"generate_tasks"`
}

type tournamentRunRecord struct {
	SharedPackage string `json:"shared_package,omitempty"`
	Enabled       bool   `json:"enabled"`
	Variants      int    `json:"variants,omitempty"`
}

type guidanceFile struct {
	Path    string
	Kind    string
	Content string
}

type repositoryFile struct {
	Path    string
	Title   string
	Summary string
	Content string
}

type projectContext struct {
	Guidance       []guidanceFile
	Files          []repositoryFile
	Packages       []string
	CommandFiles   []string
	Capabilities   []string
	ValidationHint []string
	SourceEvidence []Evidence
}

//nolint:govet // JSON field order mirrors the roadmap artifact users read.
type roadmapRecord struct {
	VariantID string   `json:"variant_id"`
	Name      string   `json:"name"`
	Lens      string   `json:"lens"`
	Ideas     []string `json:"ideas"`
	Notes     string   `json:"notes"`
}

//nolint:govet // Field order mirrors the comparison table users read.
type tournamentDecisionRecord struct {
	SourceVariants []string `json:"source_variants,omitempty"`
	Title          string   `json:"title"`
	Decision       string   `json:"decision"`
	Rationale      string   `json:"rationale"`
	Rank           int      `json:"rank"`
	Score          int      `json:"score"`
}

var requestedIdeaCountPattern = regexp.MustCompile(`(?i)\b(\d{1,2})\s+(?:feature\s+|roadmap\s+)?ideas?\b`)

// Run creates a scout run directory and writes scout.md, ideas.jsonl,
// competitors.jsonl, run.json, and optional tournament/tasks artifacts.
//
//nolint:cyclop // The top-level artifact pipeline keeps each failure annotated at the call site.
func Run(ctx context.Context, req RunRequest) (RunResult, error) {
	if err := requireContext(ctx); err != nil {
		return RunResult{}, err
	}

	req.Prompt = strings.TrimSpace(req.Prompt)
	if req.Prompt == "" {
		return RunResult{}, errors.New("scout: prompt is required")
	}

	root, err := normalizeRoot(req.Root)
	if err != nil {
		return RunResult{}, err
	}

	createdAt := req.Now.UTC()
	if createdAt.IsZero() {
		createdAt = time.Now().UTC()
	}

	runID := scoutRunID(req, createdAt)
	runDir := scoutRunDir(root, req.OutputDir, runID)
	if mkErr := os.MkdirAll(runDir, 0o750); mkErr != nil {
		return RunResult{}, fmt.Errorf("scout: create run dir %s: %w", runDir, mkErr)
	}

	project, err := discoverProjectContext(ctx, root, req.Area)
	if err != nil {
		return RunResult{}, err
	}
	project.SourceEvidence = buildSourceEvidence(ctx, root, req.Sources)

	competitors := normalizeCompetitors(req.Competitors, createdAt)
	ideas, roadmaps, tournamentDecisions, tournamentRecord, err := buildRecommendations(req, project, competitors)
	if err != nil {
		return RunResult{}, err
	}

	artifacts := scoutArtifactPaths(req.GenerateTasks, tournamentRecord.Enabled)
	record := runRecord{
		Schema:          SchemaVersion,
		RunID:           runID,
		Prompt:          req.Prompt,
		Area:            strings.TrimSpace(req.Area),
		CreatedAt:       createdAt,
		Root:            root,
		OutputDir:       runDir,
		Artifacts:       artifacts,
		GuidanceFiles:   guidancePaths(project.Guidance),
		RepositoryFiles: repositoryPaths(project.Files),
		Competitors:     competitorNames(competitors),
		Sources:         redactedInputs(req.Sources),
		IdeaCount:       len(ideas),
		CompetitorCount: len(competitors),
		GenerateTasks:   req.GenerateTasks,
		Tournament:      tournamentRecord,
		Notes: []string{
			"Local-first MVP: autonomous web search is not performed; competitor names and URLs are recorded for audit/follow-up.",
			EvidenceBestPractice,
			"Tournament comparison is implemented through pkg/tournament so scout and autoresearch can share the ranking primitive.",
		},
	}

	if err := removeDisabledScoutArtifacts(runDir, req.GenerateTasks, tournamentRecord.Enabled); err != nil {
		return RunResult{}, err
	}
	if err := writeIdeasJSONL(filepath.Join(runDir, ideasFile), ideas); err != nil {
		return RunResult{}, err
	}
	if err := writeCompetitorsJSONL(filepath.Join(runDir, competitorsFile), competitors); err != nil {
		return RunResult{}, err
	}
	if tournamentRecord.Enabled {
		if err := writeRoadmapsJSONL(filepath.Join(runDir, roadmapsFile), roadmaps); err != nil {
			return RunResult{}, err
		}
	}

	report := renderReport(req, project, competitors, ideas, roadmaps, tournamentDecisions, record)
	if err := writeTextFile(filepath.Join(runDir, scoutReportFile), report); err != nil {
		return RunResult{}, err
	}

	if req.GenerateTasks {
		if err := writeTextFile(filepath.Join(runDir, tasksFile), renderGeneratedTasks(req, ideas, project, runID)); err != nil {
			return RunResult{}, err
		}
	}

	if err := writeRunJSON(filepath.Join(runDir, runFile), record); err != nil {
		return RunResult{}, err
	}

	return RunResult{
		RunID:       runID,
		Dir:         runDir,
		Files:       resultFiles(req.GenerateTasks, tournamentRecord.Enabled),
		Ideas:       ideas,
		Competitors: competitors,
		CreatedAt:   createdAt,
	}, nil
}

func requireContext(ctx context.Context) error {
	if ctx == nil {
		return errors.New("scout: context is required")
	}

	if err := ctx.Err(); err != nil {
		return fmt.Errorf("scout: context already done: %w", err)
	}

	return nil
}

func normalizeRoot(root string) (string, error) {
	root = strings.TrimSpace(root)
	if root == "" {
		cwd, err := os.Getwd()
		if err != nil {
			return "", fmt.Errorf("scout: locate working directory: %w", err)
		}
		root = cwd
	}

	abs, err := filepath.Abs(root)
	if err != nil {
		return "", fmt.Errorf("scout: resolve root %s: %w", root, err)
	}

	return filepath.Clean(abs), nil
}

func scoutRunID(req RunRequest, now time.Time) string {
	if id := sanitizeRunID(req.RunID); id != "" {
		return id
	}

	if strings.TrimSpace(req.OutputDir) != "" {
		if id := sanitizeRunID(filepath.Base(filepath.Clean(req.OutputDir))); id != "" && id != "." {
			return id
		}
	}

	digest := sha256.Sum256([]byte(strings.TrimSpace(req.Prompt) + "\x00" + now.Format(time.RFC3339Nano)))

	return now.Format("20060102-150405.000000000") + "-" + hex.EncodeToString(digest[:4])
}

func sanitizeRunID(raw string) string {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return ""
	}

	var b strings.Builder
	for _, r := range raw {
		switch {
		case unicode.IsLetter(r), unicode.IsDigit(r):
			b.WriteRune(r)
		case r == '-', r == '_', r == '.':
			b.WriteRune(r)
		default:
			b.WriteRune('-')
		}
	}

	return strings.Trim(b.String(), "-_.")
}

func scoutRunDir(root, outputDir, runID string) string {
	outputDir = strings.TrimSpace(outputDir)
	if outputDir == "" {
		return filepath.Join(root, ".atteler", "runs", "scout", runID)
	}

	if filepath.IsAbs(outputDir) {
		return filepath.Clean(outputDir)
	}

	return filepath.Join(root, filepath.Clean(outputDir))
}

func scoutArtifactPaths(generateTasks, tournamentEnabled bool) map[string]string {
	artifacts := map[string]string{
		"scout":       scoutReportFile,
		"ideas":       ideasFile,
		"competitors": competitorsFile,
		"run":         runFile,
	}
	if tournamentEnabled {
		artifacts["roadmaps"] = roadmapsFile
	}
	if generateTasks {
		artifacts["tasks"] = tasksFile
	}

	return artifacts
}

func resultFiles(generateTasks, tournamentEnabled bool) []string {
	files := []string{scoutReportFile, ideasFile, competitorsFile, runFile}
	if tournamentEnabled {
		files = append(files, roadmapsFile)
	}
	if generateTasks {
		files = append(files, tasksFile)
	}
	sort.Strings(files)

	return files
}

func removeDisabledScoutArtifacts(runDir string, generateTasks, tournamentEnabled bool) error {
	if !tournamentEnabled {
		if err := removeScoutArtifact(filepath.Join(runDir, roadmapsFile)); err != nil {
			return err
		}
	}
	if !generateTasks {
		if err := removeScoutArtifact(filepath.Join(runDir, tasksFile)); err != nil {
			return err
		}
	}

	return nil
}

func removeScoutArtifact(path string) error {
	if err := os.Remove(path); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("scout: remove stale artifact %s: %w", path, err)
	}

	return nil
}

func discoverProjectContext(ctx context.Context, root, area string) (projectContext, error) {
	guidance, err := discoverGuidance(ctx, root)
	if err != nil {
		return projectContext{}, err
	}

	files, err := discoverRepositoryFiles(ctx, root, area)
	if err != nil {
		return projectContext{}, err
	}

	packages, err := discoverPackageDirs(root)
	if err != nil {
		return projectContext{}, err
	}

	commands, err := discoverCommandFiles(root)
	if err != nil {
		return projectContext{}, err
	}

	return projectContext{
		Guidance:       guidance,
		Files:          files,
		Packages:       packages,
		CommandFiles:   commands,
		Capabilities:   inferCapabilities(files, packages, commands),
		ValidationHint: inferValidationHints(root, guidance, files),
	}, nil
}

//nolint:nilerr // Unreadable optional guidance files are skipped so one bad file does not abort scout.
func discoverGuidance(ctx context.Context, root string) ([]guidanceFile, error) {
	var guidance []guidanceFile

	err := filepath.WalkDir(root, func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return nil
		}
		if ctxErr := ctx.Err(); ctxErr != nil {
			return fmt.Errorf("scout: context canceled while discovering guidance: %w", ctxErr)
		}
		skip, err := skipGuidanceDirectory(root, path, entry)
		if err != nil {
			return err
		}
		if skip {
			return filepath.SkipDir
		}
		if entry.IsDir() || len(guidance) >= maxGuidanceFiles {
			return nil
		}

		file, ok, err := guidanceFileForPath(root, path)
		if err != nil {
			return err
		}
		if ok {
			guidance = append(guidance, file)
		}

		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("scout: discover guidance: %w", err)
	}

	sort.Slice(guidance, func(i, j int) bool {
		return guidance[i].Path < guidance[j].Path
	})

	return guidance, nil
}

func skipGuidanceDirectory(root, path string, entry fs.DirEntry) (bool, error) {
	if path == root || !entry.IsDir() {
		return false, nil
	}
	if shouldSkipGuidanceDir(entry.Name()) {
		return true, nil
	}

	rel, err := filepath.Rel(root, path)
	if err != nil {
		return false, fmt.Errorf("scout: relativize guidance directory: %w", err)
	}

	return shouldSkipGuidanceSubtree(rel), nil
}

//nolint:nilerr // Unreadable optional guidance files are skipped.
func guidanceFileForPath(root, path string) (guidanceFile, bool, error) {
	rel, err := filepath.Rel(root, path)
	if err != nil {
		return guidanceFile{}, false, fmt.Errorf("scout: relativize guidance path: %w", err)
	}

	kind, ok := guidanceKind(rel)
	if !ok {
		return guidanceFile{}, false, nil
	}

	content, err := readTextFile(path, maxSourceBytes)
	if err != nil {
		return guidanceFile{}, false, nil
	}

	return guidanceFile{Path: filepath.ToSlash(rel), Kind: kind, Content: content}, true, nil
}

func shouldSkipDir(name string) bool {
	switch name {
	case ".git", ".atteler", ".symphony", ".codex", "node_modules", "vendor", "dist", "site", "tmp", "build":
		return true
	default:
		return false
	}
}

func shouldSkipGuidanceDir(name string) bool {
	switch name {
	case ".git", ".atteler", ".symphony", "node_modules", "vendor", "dist", "site", "tmp", "build":
		return true
	default:
		return false
	}
}

func shouldSkipGuidanceSubtree(rel string) bool {
	lower := strings.ToLower(filepath.ToSlash(rel))
	if !strings.HasPrefix(lower, ".codex/") {
		return false
	}

	switch {
	case lower == ".codex/prompts", lower == ".codex/agents", lower == ".codex/skills":
		return false
	case strings.HasPrefix(lower, ".codex/skills/") && strings.Count(lower, "/") == 2:
		return false
	default:
		return true
	}
}

func guidanceKind(rel string) (string, bool) {
	slash := filepath.ToSlash(rel)
	base := filepath.Base(slash)

	switch base {
	case "AGENTS.md":
		return "agents_instructions", true
	case "CLAUDE.md":
		return "claude_instructions", true
	case "GEMINI.md":
		return "gemini_instructions", true
	case "CODEX.md":
		return "codex_instructions", true
	case ".cursorrules":
		return "cursor_rules", true
	case ".windsurfrules":
		return "windsurf_rules", true
	case ".clinerules":
		return "cline_rules", true
	}

	if slash == ".github/copilot-instructions.md" {
		return "copilot_instructions", true
	}

	return prefixedGuidanceKind(slash)
}

func prefixedGuidanceKind(slash string) (string, bool) {
	switch {
	case strings.HasPrefix(slash, ".cursor/rules/"):
		return "cursor_rules", true
	case strings.HasPrefix(slash, ".windsurf/rules/"):
		return "windsurf_rules", true
	case strings.HasPrefix(slash, ".cline/rules/"):
		return "cline_rules", true
	case strings.HasPrefix(slash, ".roo/rules/"):
		return "roo_rules", true
	case strings.HasPrefix(slash, ".agents/") && strings.HasSuffix(strings.ToLower(slash), ".md"):
		return "agent_instructions", true
	case codexGuidanceFile(slash):
		return "codex_instructions", true
	default:
		return "", false
	}
}

func codexGuidanceFile(slash string) bool {
	lower := strings.ToLower(filepath.ToSlash(slash))
	if !strings.HasPrefix(lower, ".codex/") {
		return false
	}

	base := filepath.Base(lower)
	switch {
	case lower == ".codex/instructions.md", lower == ".codex/config.md":
		return true
	case strings.HasPrefix(lower, ".codex/prompts/") && strings.HasSuffix(lower, ".md"):
		return true
	case strings.HasPrefix(lower, ".codex/agents/") && strings.HasSuffix(lower, ".md"):
		return true
	case strings.HasPrefix(lower, ".codex/skills/") && base == "skill.md":
		return true
	default:
		return false
	}
}

func discoverRepositoryFiles(ctx context.Context, root, area string) ([]repositoryFile, error) {
	candidates := []string{
		"README.md",
		"WORKFLOW.md",
		"NOTES.md",
		"docs/common-workflows.md",
		"docs/architecture.md",
		"docs/cli-reference.md",
	}

	area = strings.ToLower(strings.TrimSpace(area))
	if area != "" {
		areaMatches, err := findAreaFiles(ctx, root, area)
		if err != nil {
			return nil, err
		}
		candidates = append(candidates, areaMatches...)
	}

	seen := make(map[string]bool, len(candidates))
	files := make([]repositoryFile, 0, len(candidates))
	for _, rel := range candidates {
		rel = filepath.ToSlash(strings.TrimSpace(rel))
		if rel == "" || seen[rel] || len(files) >= maxSourceFiles {
			continue
		}
		seen[rel] = true

		content, err := readTextFile(filepath.Join(root, filepath.FromSlash(rel)), maxSourceBytes)
		if err != nil {
			continue
		}
		files = append(files, repositoryFile{
			Path:    rel,
			Title:   sourceTitle(rel, content),
			Summary: sourceSummary(content),
			Content: content,
		})
	}

	return files, nil
}

//nolint:nilerr // Area discovery is best-effort; unreadable files are skipped.
func findAreaFiles(ctx context.Context, root, area string) ([]string, error) {
	var matches []string
	err := filepath.WalkDir(root, func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return nil
		}
		if ctxErr := ctx.Err(); ctxErr != nil {
			return fmt.Errorf("scout: context canceled while finding area files: %w", ctxErr)
		}
		if path != root && entry.IsDir() && shouldSkipDir(entry.Name()) {
			return filepath.SkipDir
		}
		if entry.IsDir() || len(matches) >= 12 {
			return nil
		}

		rel, err := filepath.Rel(root, path)
		if err != nil {
			return fmt.Errorf("scout: relativize area path: %w", err)
		}
		slash := filepath.ToSlash(rel)
		lower := strings.ToLower(slash)
		if !strings.Contains(lower, area) {
			return nil
		}
		if !strings.HasSuffix(lower, ".md") && !strings.HasSuffix(lower, ".go") {
			return nil
		}

		matches = append(matches, slash)

		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("scout: find area files: %w", err)
	}

	sort.Strings(matches)

	return matches, nil
}

func discoverPackageDirs(root string) ([]string, error) {
	entries, err := os.ReadDir(filepath.Join(root, "pkg"))
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, nil
		}

		return nil, fmt.Errorf("scout: read pkg dirs: %w", err)
	}

	var packages []string
	for _, entry := range entries {
		if entry.IsDir() {
			packages = append(packages, filepath.ToSlash(filepath.Join("pkg", entry.Name())))
		}
	}
	sort.Strings(packages)

	return packages, nil
}

func discoverCommandFiles(root string) ([]string, error) {
	matches, err := filepath.Glob(filepath.Join(root, "cmd", "atteler", "cli_*.go"))
	if err != nil {
		return nil, fmt.Errorf("scout: discover command files: %w", err)
	}

	out := make([]string, 0, len(matches))
	for _, match := range matches {
		rel, err := filepath.Rel(root, match)
		if err != nil {
			continue
		}
		out = append(out, filepath.ToSlash(rel))
	}
	sort.Strings(out)

	return out, nil
}

func readTextFile(path string, maxBytes int64) (string, error) {
	file, err := os.Open(path)
	if err != nil {
		return "", fmt.Errorf("open %s: %w", path, err)
	}
	defer file.Close()

	data, err := readAtMost(file, maxBytes)
	if err != nil {
		return "", fmt.Errorf("read %s: %w", path, err)
	}
	if !utf8.Valid(data) {
		return "", errors.New("not UTF-8 text")
	}

	return string(data), nil
}

func readAtMost(file *os.File, maxBytes int64) ([]byte, error) {
	if maxBytes <= 0 {
		maxBytes = maxSourceBytes
	}

	var b bytes.Buffer
	reader := bufio.NewReader(file)
	for b.Len() < int(maxBytes) {
		chunk := make([]byte, min(4096, int(maxBytes)-b.Len()))
		n, err := reader.Read(chunk)
		if n > 0 {
			b.Write(chunk[:n])
		}
		if err != nil {
			if errors.Is(err, io.EOF) {
				return b.Bytes(), nil
			}

			return b.Bytes(), err
		}
	}

	return b.Bytes(), nil
}

func sourceTitle(path, content string) string {
	if heading := firstMarkdownHeading(content); heading != "" {
		return heading
	}

	base := filepath.Base(path)
	if base == "." || base == string(filepath.Separator) || base == "" {
		return "Repository source"
	}

	return base
}

func firstMarkdownHeading(content string) string {
	scanner := bufio.NewScanner(strings.NewReader(content))
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if strings.HasPrefix(line, "#") {
			line = strings.TrimLeft(line, "#")
			line = strings.TrimSpace(line)
			if line != "" {
				return truncateRunes(line, maxReportLineRunes)
			}
		}
	}

	return ""
}

func sourceSummary(content string) string {
	scanner := bufio.NewScanner(strings.NewReader(content))
	for scanner.Scan() {
		line := summaryCandidateLine(scanner.Text())
		if line != "" {
			return truncateRunes(line, maxReportLineRunes)
		}
	}

	return "Readable text source."
}

func summaryCandidateLine(line string) string {
	line = strings.TrimSpace(line)
	switch line {
	case "", "---", "...":
		return ""
	default:
		return strings.Trim(line, "#*` ")
	}
}

func truncateRunes(value string, limit int) string {
	value = strings.TrimSpace(value)
	if limit <= 0 || utf8.RuneCountInString(value) <= limit {
		return value
	}

	runes := []rune(value)
	if limit <= 1 {
		return string(runes[:limit])
	}

	return string(runes[:limit-1]) + "…"
}

func inferCapabilities(files []repositoryFile, packages, commands []string) []string {
	signalParts := append(append(repositoryPaths(files), packages...), commands...)
	for _, file := range files {
		signalParts = append(signalParts, file.Title, file.Summary, file.Content)
	}

	signals := strings.ToLower(strings.Join(signalParts, " "))
	candidates := []struct {
		Signal     string
		Capability string
	}{
		{"research", "local-first research artifacts"},
		{"scout", "product discovery and roadmap generation"},
		{"autoresearch", "autonomous experiment loops"},
		{"review", "review and feedback workflows"},
		{"memory", "local memory and retrieval"},
		{"codeintel", "Go code intelligence"},
		{"plugin", "plugin execution and policy"},
		{"incident", "incident diagnosis"},
		{"eval", "structured evaluation"},
		{"worktree", "isolated worktree execution"},
	}

	var out []string
	for _, candidate := range candidates {
		if strings.Contains(signals, candidate.Signal) {
			out = append(out, candidate.Capability)
		}
	}
	if len(out) == 0 {
		out = append(out, "repository-aware CLI workflows")
	}

	return out
}

func inferValidationHints(root string, guidance []guidanceFile, files []repositoryFile) []string {
	text := strings.ToLower(strings.Join(append(guidanceContents(guidance), fileContents(files)...), "\n"))
	var hints []string
	if fileExists(filepath.Join(root, "Makefile")) {
		hints = append(hints, "make test")
	}
	if strings.Contains(text, "lint") {
		hints = append(hints, "make lint")
	}
	if strings.Contains(text, "test") && !containsString(hints, "make test") {
		hints = append(hints, "go test ./...")
	}
	if len(hints) == 0 {
		hints = append(hints, "go test ./...")
	}

	return hints
}

func fileExists(path string) bool {
	_, err := os.Stat(path)

	return err == nil
}

func guidanceContents(files []guidanceFile) []string {
	out := make([]string, len(files))
	for i := range files {
		out[i] = files[i].Content
	}

	return out
}

func fileContents(files []repositoryFile) []string {
	out := make([]string, len(files))
	for i := range files {
		out[i] = files[i].Content
	}

	return out
}

func buildSourceEvidence(ctx context.Context, root string, inputs []string) []Evidence {
	inputs = uniqueTrimmed(inputs)
	out := make([]Evidence, 0, len(inputs))
	for _, input := range inputs {
		if ctx.Err() != nil {
			return out
		}
		if isURL(input) {
			out = append(out, Evidence{
				Kind:    "scout_source_url",
				URL:     privacy.RedactIdentifier(input),
				Source:  "scout_source",
				Excerpt: "URL recorded as scout context; autonomous web fetching is outside the MVP.",
			})
			continue
		}

		out = append(out, localSourceEvidence(ctx, root, input)...)
	}

	return out
}

func uniqueTrimmed(values []string) []string {
	seen := make(map[string]bool, len(values))
	out := make([]string, 0, len(values))
	for _, value := range values {
		value = strings.TrimSpace(value)
		if value == "" {
			continue
		}
		key := strings.ToLower(value)
		if seen[key] {
			continue
		}

		seen[key] = true
		out = append(out, value)
	}

	return out
}

func isURL(raw string) bool {
	parsed, err := url.Parse(strings.TrimSpace(raw))

	return err == nil && parsed.Scheme != "" && parsed.Host != ""
}

func localSourceEvidence(ctx context.Context, root, input string) []Evidence {
	path := strings.TrimSpace(input)
	if path == "" {
		return nil
	}

	displayPath := privacy.RedactIdentifier(filepath.ToSlash(path))
	if !filepath.IsAbs(path) {
		path = filepath.Join(root, path)
	}

	info, err := os.Stat(path)
	if err != nil {
		return []Evidence{sourceLoadErrorEvidence(displayPath, "source could not be loaded: "+err.Error())}
	}

	if !info.IsDir() {
		return []Evidence{localFileEvidence(root, path)}
	}

	var evidence []Evidence
	err = filepath.WalkDir(path, func(child string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if ctxErr := ctx.Err(); ctxErr != nil {
			return fmt.Errorf("scout: context canceled while loading scout source: %w", ctxErr)
		}
		if child != path && entry.IsDir() && shouldSkipDir(entry.Name()) {
			return filepath.SkipDir
		}
		if entry.IsDir() || len(evidence) >= maxSourceFiles {
			return nil
		}

		evidence = append(evidence, localFileEvidence(root, child))

		return nil
	})
	if err != nil {
		return []Evidence{sourceLoadErrorEvidence(displayPath, "source directory could not be walked: "+err.Error())}
	}
	if len(evidence) == 0 {
		return []Evidence{{
			Kind:    "scout_source_directory",
			Path:    displayPath,
			Source:  "scout_source",
			Excerpt: "directory source did not contain readable text files within scout limits.",
		}}
	}

	return evidence
}

func localFileEvidence(root, path string) Evidence {
	rel, err := filepath.Rel(root, path)
	if err != nil {
		rel = path
	}
	rel = privacy.RedactIdentifier(filepath.ToSlash(rel))

	content, err := readTextFile(path, maxSourceBytes)
	if err != nil {
		return sourceLoadErrorEvidence(rel, "source could not be loaded: "+err.Error())
	}

	return Evidence{
		Kind:    "scout_source_file",
		Path:    rel,
		Source:  "scout_source",
		Excerpt: privacy.RedactText(sourceSummary(content)),
	}
}

func sourceLoadErrorEvidence(path, message string) Evidence {
	return Evidence{
		Kind:    "scout_source",
		Path:    privacy.RedactIdentifier(path),
		Source:  "scout_source",
		Excerpt: privacy.RedactText(message),
	}
}

func normalizeCompetitors(values []string, recordedAt time.Time) []Competitor {
	seen := make(map[string]bool, len(values))
	competitors := make([]Competitor, 0, len(values))
	for _, value := range values {
		value = strings.TrimSpace(value)
		if value == "" {
			continue
		}

		name, competitorURL := competitorNameAndURL(value)
		key := strings.ToLower(name + "\x00" + competitorURL)
		if name == "" || seen[key] {
			continue
		}

		seen[key] = true
		competitors = append(competitors, Competitor{
			Name:       name,
			URL:        competitorURL,
			SourceType: "user_supplied_competitor",
			RecordedAt: recordedAt,
			Notes:      "Recorded as external inspiration; scout MVP does not fetch web pages, so verify current product/docs before acting.",
		})
	}

	return competitors
}

func competitorNameAndURL(value string) (name, competitorURL string) {
	if parsed, err := url.Parse(value); err == nil && parsed.Host != "" {
		host := strings.TrimPrefix(strings.ToLower(parsed.Hostname()), "www.")
		pathBase := strings.TrimSpace(filepath.Base(strings.Trim(parsed.Path, "/")))
		if strings.EqualFold(host, "github.com") && pathBase != "" && pathBase != "." {
			return pathBase, value
		}

		return host, value
	}

	name = strings.Trim(value, " \t\r\n,")
	name = strings.TrimPrefix(name, "@")

	return name, ""
}

func buildRecommendations(req RunRequest, project projectContext, competitors []Competitor) ([]Idea, []roadmapRecord, []tournamentDecisionRecord, tournamentRunRecord, error) {
	limit := requestedIdeaCount(req.Prompt)
	ideas := rankIdeas(generateIdeas(req, project, competitors), 0)
	tournamentRecord := tournamentRunRecord{}
	if !req.Tournament && req.VariantCount <= 0 {
		return limitIdeas(ideas, limit), nil, nil, tournamentRecord, nil
	}

	variantCount := req.VariantCount
	if variantCount <= 0 {
		variantCount = defaultVariants
	}

	variants := buildTournamentVariants(ideas, variantCount)
	result, err := tournament.Merge(variants, tournament.Options{KeepTop: min(limit, len(ideas))})
	if err != nil {
		return nil, nil, nil, tournamentRecord, fmt.Errorf("scout: merge tournament variants: %w", err)
	}

	ideas = ideasFromTournament(ideas, result.Kept)
	roadmaps := roadmapRecords(result.Variants)
	decisions := tournamentDecisionRecords(result.Ranked)
	tournamentRecord = tournamentRunRecord{
		Enabled:       true,
		Variants:      len(result.Variants),
		SharedPackage: "github.com/tommoulard/atteler/pkg/tournament",
	}

	return ideas, roadmaps, decisions, tournamentRecord, nil
}

func limitIdeas(ideas []Idea, limit int) []Idea {
	if limit <= 0 || limit >= len(ideas) {
		return ideas
	}

	return ideas[:limit]
}

func requestedIdeaCount(prompt string) int {
	match := requestedIdeaCountPattern.FindStringSubmatch(prompt)
	if len(match) < 2 {
		return defaultIdeaCount
	}

	count, err := strconv.Atoi(match[1])
	if err != nil || count <= 0 {
		return defaultIdeaCount
	}

	return min(count, 25)
}

func generateIdeas(req RunRequest, project projectContext, competitors []Competitor) []Idea {
	area := strings.ToLower(strings.TrimSpace(req.Area + " " + req.Prompt))
	evidence := appendEvidence(baselineEvidence(project), project.SourceEvidence...)
	guidanceEvidence := guidanceEvidence(project)

	ideas := []Idea{
		scoutWorkflowIdea(project, evidence, guidanceEvidence),
		{
			Title:               "Harness-aware implementation task generation",
			Summary:             "Turn selected roadmap ideas into task YAML that inherits repository instructions such as required tests, lint, and review steps.",
			Fit:                 fitFor(len(project.Guidance) > 0),
			Complexity:          labelLow,
			Risk:                labelLow,
			SuggestedMVP:        "Add task stubs with title, rationale, related files, and validation commands inferred from guidance and Makefile.",
			RelatedFilesOrAreas: relatedAreas("AGENTS.md", ".cursor/rules", "tasks.generated.yaml"),
			Evidence:            guidanceEvidence,
		},
		{
			Title:               "Evidence ledger for speculative ideas",
			Summary:             "Label recommendations as evidence-backed or speculative so users can distinguish grounded roadmap bets from creative hypotheses.",
			Fit:                 labelHigh,
			Complexity:          labelLow,
			Risk:                labelLow,
			SuggestedMVP:        "Include evidence arrays in ideas.jsonl and a citations/constraints section in scout.md.",
			RelatedFilesOrAreas: relatedAreas("pkg/scout", "docs/common-workflows.md"),
			Evidence:            evidence,
		},
		{
			Title:               "Shared tournament ranking primitive",
			Summary:             "Use a reusable comparison package so scout can rank roadmap variants and autoresearch can later rank implementation hypotheses.",
			Fit:                 labelHigh,
			Complexity:          labelMedium,
			Risk:                labelMedium,
			SuggestedMVP:        "Implement dependency-free candidate scoring and deduplication in pkg/tournament, then call it from scout tournament mode.",
			RelatedFilesOrAreas: relatedAreas("pkg/tournament", "pkg/scout", "pkg/autopilot"),
			Evidence:            evidence,
		},
		{
			Title:               "Repository capability map in scout reports",
			Summary:             "Summarize existing commands, packages, and docs before recommending new work so proposals fit the codebase.",
			Fit:                 labelHigh,
			Complexity:          labelMedium,
			Risk:                labelLow,
			SuggestedMVP:        "List detected capabilities from cmd/atteler CLI files, pkg directories, README, and docs in scout.md.",
			RelatedFilesOrAreas: relatedAreas("cmd/atteler", "pkg", "README.md"),
			Evidence:            evidence,
		},
		{
			Title:               "Prior-session and issue-history inspiration",
			Summary:             "Mine existing Atteler sessions, feedback records, and issue metadata for recurring pain points before generating roadmap ideas.",
			Fit:                 labelMedium,
			Complexity:          labelMedium,
			Risk:                labelMedium,
			SuggestedMVP:        "Add optional local session and git-history sources to scout context, with citations back to session IDs or commits.",
			RelatedFilesOrAreas: relatedAreas("pkg/session", "pkg/githistory", ".github"),
			Evidence:            evidenceForCapability(project, "session", "githistory"),
		},
		{
			Title:               "Review and eval gates for roadmap candidates",
			Summary:             "Attach suggested validation gates to each idea so feasibility and acceptance criteria are visible before implementation starts.",
			Fit:                 labelMedium,
			Complexity:          labelLow,
			Risk:                labelLow,
			SuggestedMVP:        "Add suggested_validation entries for generated tasks using existing review/eval command patterns.",
			RelatedFilesOrAreas: relatedAreas("pkg/review", "pkg/eval", "cmd/atteler"),
			Evidence:            evidenceForCapability(project, "review", "eval"),
		},
		{
			Title:               "Source packs for competitor comparisons",
			Summary:             "Let users provide competitor docs or URLs and preserve them as auditable inspiration sources for later product analysis.",
			Fit:                 fitFor(len(competitors) > 0),
			Complexity:          labelMedium,
			Risk:                labelMedium,
			SuggestedMVP:        "Record supplied competitor names/URLs in competitors.jsonl and render a follow-up checklist for fresh verification.",
			RelatedFilesOrAreas: relatedAreas("pkg/scout", "competitors.jsonl", "docs"),
			Evidence:            appendEvidence(competitorEvidence(competitors), project.SourceEvidence...),
			Speculative:         len(competitors) == 0 && len(project.SourceEvidence) == 0,
		},
		{
			Title:               "Memory-backed opportunity discovery",
			Summary:             "Use local memory and vector retrieval to surface repeated user needs, stalled tasks, and high-frequency workflows.",
			Fit:                 fitFor(hasCapability(project, "memory")),
			Complexity:          labelMedium,
			Risk:                labelMedium,
			SuggestedMVP:        "Add optional retrieval-source inputs and cite matching memory documents in scout.md.",
			RelatedFilesOrAreas: relatedAreas("pkg/memory", "pkg/retrieval", "cmd/atteler"),
			Evidence:            evidenceForCapability(project, "memory", "retrieval"),
		},
		{
			Title:               "Code-intelligence feasibility scoring",
			Summary:             "Use the code-intel index to estimate touched packages, likely dependencies, and implementation risk for each idea.",
			Fit:                 fitFor(hasCapability(project, "code intelligence")),
			Complexity:          labelMedium,
			Risk:                labelLow,
			SuggestedMVP:        "Map ideas to candidate files/packages and include a simple low/medium/high implementation complexity label.",
			RelatedFilesOrAreas: relatedAreas("pkg/codeintel", "cmd/atteler/codeintel_*"),
			Evidence:            evidenceForCapability(project, "codeintel", "code-intel"),
		},
	}

	if strings.Contains(area, "autoresearch") {
		ideas = append([]Idea{{
			Title:               "Autoresearch hypothesis tournaments",
			Summary:             "Run multiple research or implementation hypotheses under the same evaluator, then keep only the best validated result.",
			Fit:                 labelHigh,
			Complexity:          labelLow,
			Risk:                labelLow,
			SuggestedMVP:        "Teach autoresearch prompts to use pkg/tournament-style candidate comparison for hypotheses before committing to an edit loop.",
			RelatedFilesOrAreas: relatedAreas("pkg/autopilot", "cmd/atteler/cli_autoresearch.go", "pkg/tournament"),
			Evidence:            appendEvidence(evidence, guidanceEvidence...),
		}}, ideas...)
	}

	if len(project.Guidance) == 0 {
		ideas = append(ideas, Idea{
			Title:               "Project guidance bootstrap check",
			Summary:             "Warn when scout cannot find AGENTS.md, CLAUDE.md, Cursor rules, or similar harness files before generating tasks.",
			Fit:                 labelMedium,
			Complexity:          labelLow,
			Risk:                labelLow,
			SuggestedMVP:        "Render an explicit no-guidance-found section with recommended files to add.",
			RelatedFilesOrAreas: relatedAreas("AGENTS.md", "CLAUDE.md", ".cursor/rules"),
			Speculative:         true,
		})
	}

	return ideas
}

func scoutWorkflowIdea(project projectContext, evidence, guidanceEvidence []Evidence) Idea {
	idea := Idea{
		Title:               "Add a scout product-discovery workflow",
		Summary:             "Create a local-first workflow that inspects project guidance and repository surfaces, then emits ranked roadmap artifacts.",
		Fit:                 labelHigh,
		Complexity:          labelMedium,
		Risk:                labelMedium,
		SuggestedMVP:        "Generate scout.md, ideas.jsonl, competitors.jsonl, run.json, and optional tasks.generated.yaml from repository context.",
		RelatedFilesOrAreas: relatedAreas("cmd/atteler", "pkg/scout", "docs"),
		Evidence:            appendEvidence(evidence, guidanceEvidence...),
	}
	if !hasScoutImplementation(project) {
		return idea
	}

	idea.Title = "Improve scout product-discovery workflow"
	idea.Summary = "Extend the existing scout workflow with richer source inputs, stronger evidence capture, and better roadmap import paths."
	idea.SuggestedMVP = "Add optional source ingestion and richer task/import metadata while preserving the existing scout.md, ideas.jsonl, and run.json artifact contract."

	return idea
}

func hasScoutImplementation(project projectContext) bool {
	return containsString(project.Packages, "pkg/scout") ||
		containsString(project.CommandFiles, "cmd/atteler/cli_scout_commands.go")
}

func baselineEvidence(project projectContext) []Evidence {
	var evidence []Evidence
	for _, path := range []string{"README.md", "docs/common-workflows.md", "docs/architecture.md"} {
		if file, ok := repositoryFileByPath(project.Files, path); ok {
			evidence = append(evidence, Evidence{Kind: sourceTypeRepository, Path: file.Path, Excerpt: file.Summary})
		}
	}
	if len(evidence) == 0 && len(project.Files) > 0 {
		file := project.Files[0]
		evidence = append(evidence, Evidence{Kind: sourceTypeRepository, Path: file.Path, Excerpt: file.Summary})
	}

	return evidence
}

func guidanceEvidence(project projectContext) []Evidence {
	evidence := make([]Evidence, 0, len(project.Guidance))
	for _, file := range project.Guidance {
		evidence = append(evidence, Evidence{
			Kind:    sourceTypeGuidance,
			Path:    file.Path,
			Excerpt: sourceSummary(file.Content),
		})
	}

	return evidence
}

func evidenceForCapability(project projectContext, needles ...string) []Evidence {
	var evidence []Evidence
	for _, file := range project.Files {
		content := strings.ToLower(file.Path + "\n" + file.Title + "\n" + file.Content)
		for _, needle := range needles {
			if strings.Contains(content, strings.ToLower(needle)) {
				evidence = append(evidence, Evidence{Kind: sourceTypeRepository, Path: file.Path, Excerpt: file.Summary})
				break
			}
		}
	}
	if len(evidence) > 0 {
		return evidence
	}

	for _, path := range append(project.Packages, project.CommandFiles...) {
		lower := strings.ToLower(path)
		for _, needle := range needles {
			if strings.Contains(lower, strings.ToLower(needle)) {
				evidence = append(evidence, Evidence{Kind: "repository_path", Path: path})
				break
			}
		}
	}

	return evidence
}

func competitorEvidence(competitors []Competitor) []Evidence {
	evidence := make([]Evidence, 0, len(competitors))
	for _, competitor := range competitors {
		item := Evidence{Kind: "competitor", Excerpt: competitor.Name}
		if competitor.URL != "" {
			item.URL = competitor.URL
		}
		evidence = append(evidence, item)
	}

	return evidence
}

func appendEvidence(first []Evidence, rest ...Evidence) []Evidence {
	out := append([]Evidence(nil), first...)
	out = append(out, rest...)

	return out
}

func fitFor(condition bool) string {
	if condition {
		return labelHigh
	}

	return labelMedium
}

func relatedAreas(values ...string) []string {
	return append([]string(nil), values...)
}

func hasCapability(project projectContext, needle string) bool {
	needle = strings.ToLower(needle)
	for _, capability := range project.Capabilities {
		if strings.Contains(strings.ToLower(capability), needle) {
			return true
		}
	}

	return false
}

func repositoryFileByPath(files []repositoryFile, path string) (repositoryFile, bool) {
	for _, file := range files {
		if file.Path == path {
			return file, true
		}
	}

	return repositoryFile{}, false
}

func rankIdeas(ideas []Idea, limit int) []Idea {
	for i := range ideas {
		if len(ideas[i].Evidence) == 0 {
			ideas[i].Speculative = true
		}
		ideas[i].Score = scoreIdea(ideas[i])
	}

	sort.SliceStable(ideas, func(i, j int) bool {
		if ideas[i].Score != ideas[j].Score {
			return ideas[i].Score > ideas[j].Score
		}

		return ideas[i].Title < ideas[j].Title
	})

	if limit > 0 && limit < len(ideas) {
		ideas = ideas[:limit]
	}

	for i := range ideas {
		ideas[i].Rank = i + 1
	}

	return ideas
}

func scoreIdea(idea Idea) int {
	score := scoreFit(idea.Fit) + scoreComplexity(idea.Complexity) + scoreRisk(idea.Risk)
	score += len(idea.Evidence) * 2
	if !idea.Speculative {
		score += 5
	}

	return score
}

func scoreFit(fit string) int {
	switch strings.ToLower(strings.TrimSpace(fit)) {
	case labelHigh:
		return 30
	case labelMedium:
		return 20
	default:
		return 10
	}
}

func scoreComplexity(complexity string) int {
	switch strings.ToLower(strings.TrimSpace(complexity)) {
	case labelLow:
		return 15
	case labelMedium:
		return 10
	default:
		return 5
	}
}

func scoreRisk(risk string) int {
	switch strings.ToLower(strings.TrimSpace(risk)) {
	case labelLow:
		return 10
	case labelMedium:
		return 5
	default:
		return 0
	}
}

func buildTournamentVariants(ideas []Idea, variantCount int) []tournament.Variant {
	lenses := []tournament.Variant{
		{ID: "user-value", Name: "User value", Lens: "prioritize high-leverage user and product value"},
		{ID: "feasibility", Name: "Feasibility", Lens: "prioritize low-risk implementation paths"},
		{ID: "evidence", Name: "Evidence", Lens: "prioritize ideas with concrete repository or competitor evidence"},
		{ID: "strategy", Name: "Strategy", Lens: "prioritize differentiating roadmap bets"},
		{ID: "workflow", Name: "Workflow", Lens: "prioritize ideas that improve the existing Atteler workflow loop"},
	}

	variants := make([]tournament.Variant, 0, variantCount)
	for i := range variantCount {
		variant := lenses[i%len(lenses)]
		lensID := variant.ID
		if cycle := i/len(lenses) + 1; cycle > 1 {
			variant.ID = fmt.Sprintf("%s-%d", variant.ID, cycle)
			variant.Name = fmt.Sprintf("%s %d", variant.Name, cycle)
		}
		variant.Candidates = candidatesForLens(ideas, variant.ID, lensID)
		variants = append(variants, variant)
	}

	return variants
}

func candidatesForLens(ideas []Idea, variantID, lensID string) []tournament.Candidate {
	candidates := make([]tournament.Candidate, 0, len(ideas))
	for i := range ideas {
		idea := ideas[i]
		candidate := tournament.Candidate{
			ID:            fmt.Sprintf("%s-%d", variantID, i+1),
			Title:         idea.Title,
			Summary:       idea.Summary,
			Fit:           scoreFit(idea.Fit) / 10,
			Feasibility:   (scoreComplexity(idea.Complexity) + scoreRisk(idea.Risk)) / 10,
			Evidence:      len(idea.Evidence),
			RiskPenalty:   riskPenalty(idea.Risk),
			OriginalIndex: i,
		}

		switch lensID {
		case "feasibility":
			candidate.Feasibility += 2
		case "evidence":
			candidate.Evidence *= 2
		case "strategy":
			candidate.Fit++
			if idea.Speculative {
				candidate.RiskPenalty++
			}
		case "workflow":
			if containsAny(idea.RelatedFilesOrAreas, "cmd/atteler", "pkg/autopilot", "pkg/scout") {
				candidate.Fit += 2
			}
		default:
			candidate.Fit++
		}

		candidates = append(candidates, candidate)
	}
	sortCandidatesForLens(candidates)

	return candidates
}

func sortCandidatesForLens(candidates []tournament.Candidate) {
	sort.SliceStable(candidates, func(i, j int) bool {
		if candidates[i].Score() != candidates[j].Score() {
			return candidates[i].Score() > candidates[j].Score()
		}
		if candidates[i].Evidence != candidates[j].Evidence {
			return candidates[i].Evidence > candidates[j].Evidence
		}
		if candidates[i].Feasibility != candidates[j].Feasibility {
			return candidates[i].Feasibility > candidates[j].Feasibility
		}
		if candidates[i].RiskPenalty != candidates[j].RiskPenalty {
			return candidates[i].RiskPenalty < candidates[j].RiskPenalty
		}

		return candidates[i].OriginalIndex < candidates[j].OriginalIndex
	})
}

func riskPenalty(risk string) int {
	switch strings.ToLower(strings.TrimSpace(risk)) {
	case labelHigh:
		return 2
	case labelMedium:
		return 1
	default:
		return 0
	}
}

func containsAny(values []string, needles ...string) bool {
	for i := range values {
		for _, needle := range needles {
			if strings.Contains(strings.ToLower(values[i]), strings.ToLower(needle)) {
				return true
			}
		}
	}

	return false
}

func ideasFromTournament(ideas []Idea, ranked []tournament.RankedCandidate) []Idea {
	byTitle := make(map[string]Idea, len(ideas))
	for i := range ideas {
		idea := ideas[i]
		byTitle[ideaKey(idea.Title)] = idea
	}

	out := make([]Idea, 0, len(ranked))
	for i := range ranked {
		candidate := ranked[i]
		idea, ok := byTitle[ideaKey(candidate.Title)]
		if !ok {
			continue
		}

		idea.Rank = candidate.Rank
		idea.Score = candidate.Score
		idea.SourceVariants = append([]string(nil), candidate.SourceVariants...)
		out = append(out, idea)
	}

	return out
}

func tournamentDecisionRecords(ranked []tournament.RankedCandidate) []tournamentDecisionRecord {
	out := make([]tournamentDecisionRecord, 0, len(ranked))
	for i := range ranked {
		candidate := ranked[i]
		out = append(out, tournamentDecisionRecord{
			Rank:           candidate.Rank,
			Title:          candidate.Title,
			Score:          candidate.Score,
			Decision:       candidate.Decision,
			Rationale:      candidate.Rationale,
			SourceVariants: append([]string(nil), candidate.SourceVariants...),
		})
	}

	return out
}

func ideaKey(title string) string {
	var b strings.Builder
	lastDash := false
	for _, r := range strings.ToLower(strings.TrimSpace(title)) {
		switch {
		case unicode.IsLetter(r), unicode.IsDigit(r):
			b.WriteRune(r)
			lastDash = false
		default:
			if !lastDash {
				b.WriteByte('-')
				lastDash = true
			}
		}
	}

	return strings.Trim(b.String(), "-")
}

func roadmapRecords(variants []tournament.Variant) []roadmapRecord {
	out := make([]roadmapRecord, 0, len(variants))
	for _, variant := range variants {
		ideas := make([]string, 0, len(variant.Candidates))
		for _, candidate := range variant.Candidates {
			ideas = append(ideas, candidate.Title)
		}
		out = append(out, roadmapRecord{
			VariantID: variant.ID,
			Name:      variant.Name,
			Lens:      variant.Lens,
			Ideas:     ideas,
			Notes:     "Deterministic local variant; future versions can replace this with independent agent-generated proposals.",
		})
	}

	return out
}

func writeIdeasJSONL(path string, ideas []Idea) error {
	file, err := os.OpenFile(path, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o600)
	if err != nil {
		return fmt.Errorf("scout: create ideas %s: %w", path, err)
	}
	defer file.Close()

	encoder := json.NewEncoder(file)
	for i := range ideas {
		idea := ideas[i]
		if err := encoder.Encode(idea); err != nil {
			return fmt.Errorf("scout: encode idea %q: %w", idea.Title, err)
		}
	}

	return nil
}

func writeCompetitorsJSONL(path string, competitors []Competitor) error {
	file, err := os.OpenFile(path, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o600)
	if err != nil {
		return fmt.Errorf("scout: create competitors %s: %w", path, err)
	}
	defer file.Close()

	encoder := json.NewEncoder(file)
	for _, competitor := range competitors {
		if err := encoder.Encode(competitor); err != nil {
			return fmt.Errorf("scout: encode competitor %q: %w", competitor.Name, err)
		}
	}

	return nil
}

func writeRoadmapsJSONL(path string, roadmaps []roadmapRecord) error {
	file, err := os.OpenFile(path, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o600)
	if err != nil {
		return fmt.Errorf("scout: create roadmaps %s: %w", path, err)
	}
	defer file.Close()

	encoder := json.NewEncoder(file)
	for _, roadmap := range roadmaps {
		if err := encoder.Encode(roadmap); err != nil {
			return fmt.Errorf("scout: encode roadmap %q: %w", roadmap.VariantID, err)
		}
	}

	return nil
}

func writeTextFile(path, content string) error {
	if err := os.WriteFile(path, []byte(strings.TrimRight(content, "\n")+"\n"), 0o600); err != nil {
		return fmt.Errorf("scout: write %s: %w", path, err)
	}

	return nil
}

func writeRunJSON(path string, record runRecord) error {
	data, err := json.MarshalIndent(record, "", "  ")
	if err != nil {
		return fmt.Errorf("scout: marshal run metadata: %w", err)
	}

	data = append(data, '\n')
	if err := os.WriteFile(path, data, 0o600); err != nil {
		return fmt.Errorf("scout: write %s: %w", path, err)
	}

	return nil
}

func renderReport(
	req RunRequest,
	project projectContext,
	competitors []Competitor,
	ideas []Idea,
	roadmaps []roadmapRecord,
	tournamentDecisions []tournamentDecisionRecord,
	record runRecord,
) string {
	var b strings.Builder

	fmt.Fprintf(&b, "# Scout: %s\n\n", req.Prompt)
	fmt.Fprintf(&b, "Run ID: `%s`  \n", record.RunID)
	fmt.Fprintf(&b, "Created: `%s`\n\n", record.CreatedAt.Format(time.RFC3339))

	fmt.Fprintln(&b, "## Project understanding")
	fmt.Fprintf(&b, "Scout inspected %d repository source file(s), %d harness guidance file(s), %d command surface file(s), and %d package area(s).\n\n", len(project.Files), len(project.Guidance), len(project.CommandFiles), len(project.Packages))
	if record.Area != "" {
		fmt.Fprintf(&b, "Focus area: `%s`.\n\n", record.Area)
	}
	fmt.Fprintf(&b, "Detected capabilities: %s.\n\n", markdownListInline(project.Capabilities))
	if len(project.Guidance) > 0 {
		fmt.Fprintf(&b, "Harness constraints loaded before recommendations: %s.\n\n", markdownListInline(guidancePaths(project.Guidance)))
	} else {
		fmt.Fprintln(&b, "No AGENTS.md, CLAUDE.md, Cursor rules, or similar harness guidance files were found under the repository root.")
		fmt.Fprintln(&b)
	}

	fmt.Fprintln(&b, "## Inspiration sources")
	fmt.Fprintln(&b, EvidenceBestPractice)
	fmt.Fprintln(&b)
	if len(competitors) == 0 {
		fmt.Fprintln(&b, "- No competitors were supplied; recommendations rely on repository context plus clearly labeled speculation.")
	} else {
		for _, competitor := range competitors {
			target := competitor.Name
			if competitor.URL != "" {
				target += " — " + competitor.URL
			}
			fmt.Fprintf(&b, "- %s (verify current public docs before implementation decisions).\n", target)
		}
	}
	if len(project.SourceEvidence) > 0 {
		fmt.Fprintln(&b, "- Additional scout sources loaded or recorded:")
		for i := range project.SourceEvidence {
			fmt.Fprintf(&b, "  - %s\n", evidenceSummary([]Evidence{project.SourceEvidence[i]}))
		}
	}
	for _, file := range project.Files {
		fmt.Fprintf(&b, "- `%s`: %s\n", file.Path, file.Summary)
	}
	fmt.Fprintln(&b)

	renderTournamentSection(&b, roadmaps, tournamentDecisions, record)

	fmt.Fprintln(&b, "## Ranked feature ideas")
	for i := range ideas {
		idea := ideas[i]
		fmt.Fprintf(&b, "### %d. %s\n\n", idea.Rank, idea.Title)
		fmt.Fprintf(&b, "%s\n\n", idea.Summary)
		fmt.Fprintf(&b, "- Rationale: %s\n", ideaRationale(idea))
		fmt.Fprintf(&b, "- Fit: `%s`\n", idea.Fit)
		fmt.Fprintf(&b, "- Estimated complexity: `%s`\n", idea.Complexity)
		fmt.Fprintf(&b, "- Risk: `%s`\n", idea.Risk)
		fmt.Fprintf(&b, "- MVP shape: %s\n", idea.SuggestedMVP)
		fmt.Fprintf(&b, "- Related files or areas: %s\n", markdownListInline(idea.RelatedFilesOrAreas))
		if len(idea.SourceVariants) > 0 {
			fmt.Fprintf(&b, "- Tournament variants: %s\n", markdownListInline(idea.SourceVariants))
		}
		if len(idea.Evidence) > 0 {
			fmt.Fprintf(&b, "- Evidence: %s\n", evidenceSummary(idea.Evidence))
		} else {
			fmt.Fprintln(&b, "- Evidence: speculative; verify with fresh repository, user, or competitor evidence before implementation.")
		}
		fmt.Fprintln(&b)
	}

	fmt.Fprintln(&b, "## Suggested implementation order")
	for i := range ideas {
		idea := ideas[i]
		fmt.Fprintf(&b, "%d. %s — start with %s\n", idea.Rank, idea.Title, ensureTerminalPunctuation(idea.SuggestedMVP))
	}
	fmt.Fprintln(&b)

	fmt.Fprintln(&b, "## Risks and follow-up")
	fmt.Fprintln(&b, "- Competitor intelligence is not fetched in this MVP; supplied competitor names/URLs are an audit trail, not proof of current product behavior.")
	fmt.Fprintln(&b, "- Speculative ideas should be validated with users, issue history, docs, or fresh source links before high-cost implementation.")
	fmt.Fprintf(&b, "- Generated implementation tasks should include validation steps such as %s.\n", markdownListInline(project.ValidationHint))
	fmt.Fprintln(&b)

	fmt.Fprintln(&b, "## Artifact index")
	for _, entry := range sortedArtifactEntries(record.Artifacts) {
		fmt.Fprintf(&b, "- `%s`: `%s`\n", entry.Key, entry.Value)
	}

	return b.String()
}

func ideaRationale(idea Idea) string {
	evidencePosture := "evidence-backed"
	if idea.Speculative {
		evidencePosture = "speculative"
	}

	return fmt.Sprintf(
		"%s fit with %s complexity and %s risk; %s recommendation scored %d.",
		idea.Fit,
		idea.Complexity,
		idea.Risk,
		evidencePosture,
		idea.Score,
	)
}

func renderTournamentSection(
	b *strings.Builder,
	roadmaps []roadmapRecord,
	decisions []tournamentDecisionRecord,
	record runRecord,
) {
	if !record.Tournament.Enabled {
		return
	}

	fmt.Fprintln(b, "## Tournament comparison")
	fmt.Fprintf(b, "Tournament mode compared %d deterministic roadmap variant(s) through `%s`.\n\n", record.Tournament.Variants, record.Tournament.SharedPackage)
	fmt.Fprintln(b, "| Variant | Lens | Candidate ideas |")
	fmt.Fprintln(b, "| --- | --- | --- |")
	for i := range roadmaps {
		roadmap := roadmaps[i]
		fmt.Fprintf(b, "| %s | %s | %d |\n", markdownTableCell(roadmap.Name), markdownTableCell(roadmap.Lens), len(roadmap.Ideas))
	}
	fmt.Fprintln(b)
	renderTournamentDecisionTable(b, decisions)
	fmt.Fprintln(b, "Kept ideas were merged into the final ranked list when they scored well across fit, feasibility, evidence, and risk. Lower-scoring duplicates or weaker bets are marked discarded during ranking.")
	fmt.Fprintln(b)
}

func renderTournamentDecisionTable(b *strings.Builder, decisions []tournamentDecisionRecord) {
	if len(decisions) == 0 {
		return
	}

	fmt.Fprintln(b, "### Final comparison")
	fmt.Fprintln(b)
	fmt.Fprintln(b, "| Rank | Decision | Idea | Score | Reason |")
	fmt.Fprintln(b, "| --- | --- | --- | --- | --- |")
	for i := range decisions {
		decision := decisions[i]
		reason := decision.Rationale
		if len(decision.SourceVariants) > 0 {
			reason += "; variants=" + strings.Join(decision.SourceVariants, ", ")
		}
		fmt.Fprintf(b, "| %d | %s | %s | %d | %s |\n", decision.Rank, markdownTableCell(decision.Decision), markdownTableCell(decision.Title), decision.Score, markdownTableCell(reason))
	}
	fmt.Fprintln(b)
}

func markdownTableCell(value string) string {
	value = strings.ReplaceAll(value, "\r\n", " ")
	value = strings.ReplaceAll(value, "\n", " ")
	value = strings.ReplaceAll(value, "\r", " ")
	value = strings.ReplaceAll(value, "|", `\|`)

	return strings.TrimSpace(value)
}

func renderGeneratedTasks(req RunRequest, ideas []Idea, project projectContext, runID string) string {
	var b strings.Builder
	fmt.Fprintln(&b, "tasks:")
	for i := range ideas {
		idea := ideas[i]
		fmt.Fprintf(&b, "  - id: \"%s\"\n", escapeYAMLDoubleQuoted(generatedTaskID(runID, idea.Rank)))
		fmt.Fprintf(&b, "    title: \"%s\"\n", escapeYAMLDoubleQuoted(idea.Title))
		fmt.Fprintln(&b, "    status: \"pending\"")
		fmt.Fprintln(&b, "    agent: \"executor\"")
		fmt.Fprintf(&b, "    priority: %d\n", generatedTaskPriority(idea.Rank))
		fmt.Fprintf(&b, "    rationale: \"Scout run %s ranked this idea for: %s\"\n", escapeYAMLDoubleQuoted(runID), escapeYAMLDoubleQuoted(req.Prompt))
		fmt.Fprintf(&b, "    suggested_mvp: \"%s\"\n", escapeYAMLDoubleQuoted(idea.SuggestedMVP))
		fmt.Fprintln(&b, "    metadata:")
		fmt.Fprintln(&b, "      source: \"atteler.scout\"")
		fmt.Fprintf(&b, "      scout_run_id: \"%s\"\n", escapeYAMLDoubleQuoted(runID))
		fmt.Fprintf(&b, "      scout_rank: \"%d\"\n", idea.Rank)
		fmt.Fprintf(&b, "      scout_score: \"%d\"\n", idea.Score)
		fmt.Fprintln(&b, "    related_files_or_areas:")
		for _, area := range idea.RelatedFilesOrAreas {
			fmt.Fprintf(&b, "      - \"%s\"\n", escapeYAMLDoubleQuoted(area))
		}
		fmt.Fprintln(&b, "    suggested_validation:")
		for _, hint := range project.ValidationHint {
			fmt.Fprintf(&b, "      - \"%s\"\n", escapeYAMLDoubleQuoted(hint))
		}
		fmt.Fprintln(&b, "      - \"Review scout.md evidence and speculative labels before implementation\"")
	}

	return b.String()
}

func generatedTaskID(runID string, rank int) string {
	id := sanitizeRunID(runID)
	if id == "" {
		id = "run"
	}
	if rank <= 0 {
		rank = 1
	}

	return fmt.Sprintf("scout-%s-%02d", id, rank)
}

func generatedTaskPriority(rank int) int {
	if rank <= 0 {
		return 1
	}

	return max(100-rank, 1)
}

func markdownListInline(values []string) string {
	if len(values) == 0 {
		return "none"
	}

	quoted := make([]string, len(values))
	for i, value := range values {
		quoted[i] = "`" + value + "`"
	}

	return strings.Join(quoted, ", ")
}

func evidenceSummary(evidence []Evidence) string {
	parts := make([]string, 0, len(evidence))
	for _, item := range evidence {
		switch {
		case item.Source != "":
			parts = append(parts, "["+item.Source+"]")
		case item.URL != "":
			parts = append(parts, item.URL)
		case item.Path != "":
			parts = append(parts, item.Path)
		case item.Excerpt != "":
			parts = append(parts, item.Excerpt)
		default:
			parts = append(parts, item.Kind)
		}
	}

	return strings.Join(parts, ", ")
}

func ensureTerminalPunctuation(value string) string {
	value = strings.TrimSpace(value)
	if value == "" {
		return "."
	}

	last := value[len(value)-1]
	switch last {
	case '.', '!', '?':
		return value
	default:
		return value + "."
	}
}

type artifactEntry struct {
	Key   string
	Value string
}

func sortedArtifactEntries(artifacts map[string]string) []artifactEntry {
	entries := make([]artifactEntry, 0, len(artifacts))
	for key, value := range artifacts {
		entries = append(entries, artifactEntry{Key: key, Value: value})
	}

	sort.Slice(entries, func(i, j int) bool {
		return entries[i].Key < entries[j].Key
	})

	return entries
}

func escapeYAMLDoubleQuoted(value string) string {
	value = strings.ReplaceAll(value, "\\", "\\\\")
	value = strings.ReplaceAll(value, "\"", "\\\"")
	value = strings.ReplaceAll(value, "\n", "\\n")
	value = strings.ReplaceAll(value, "\r", "\\r")
	value = strings.ReplaceAll(value, "\t", "\\t")

	return value
}

func guidancePaths(guidance []guidanceFile) []string {
	out := make([]string, 0, len(guidance))
	for _, file := range guidance {
		out = append(out, file.Path)
	}

	return out
}

func repositoryPaths(files []repositoryFile) []string {
	out := make([]string, 0, len(files))
	for _, file := range files {
		out = append(out, file.Path)
	}

	return out
}

func competitorNames(competitors []Competitor) []string {
	out := make([]string, 0, len(competitors))
	for _, competitor := range competitors {
		out = append(out, competitor.Name)
	}

	return out
}

func redactedInputs(values []string) []string {
	out := make([]string, 0, len(values))
	for _, value := range uniqueTrimmed(values) {
		out = append(out, privacy.RedactIdentifier(value))
	}

	return out
}

func containsString(values []string, target string) bool {
	return slices.Contains(values, target)
}
