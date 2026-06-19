// Package scout creates local-first product discovery and roadmap artifacts.
//
// The MVP intentionally avoids mandatory network access. It inspects the local
// repository, discovered harness instructions, user-supplied competitor names or
// URLs, and deterministic roadmap heuristics to produce auditable artifacts.
//
//nolint:wsl_v5,gocognit,cyclop,nilerr // Artifact assembly uses compact sequential builders; guidance discovery intentionally skips unreadable optional files.
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
	"sort"
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
	tasksFile            = "tasks.generated.yaml"
	runFile              = "run.json"
	maxGuidanceFiles     = 64
	maxScoutSourceFiles  = 64
	maxSourceBytes       = 64 * 1024
	maxReportLineRunes   = 180
	defaultIdeaCount     = 10
	maxRelatedFileAreas  = 4
	maxIdeaEvidence      = 8
	sourceTypeGuidance   = "project_guidance"
	sourceTypeCompetitor = "competitor"
	fitHigh              = "high"
	fitMedium            = "medium"
	complexityLow        = "low"
	complexityMedium     = "medium"
	complexityHigh       = "high"
	riskLow              = "low"
	riskMedium           = "medium"
)

// EvidenceBestPractice is the recommended reliability posture for scout runs.
const EvidenceBestPractice = "Atteler scout recommendations should cite evidence where available, including competitor docs, public product pages, repository files, existing project docs, prior sessions, command output, or issue history. Speculative ideas are allowed, but scout labels them clearly so users can distinguish evidence-backed recommendations from hypotheses."

var urlPattern = regexp.MustCompile(`(?i)\bhttps?://[^\s"'<>]+`)

// RunRequest configures one scout run.
type RunRequest struct {
	Now           time.Time
	Prompt        string
	Root          string
	OutputDir     string
	RunID         string
	Area          string
	Competitors   []string
	Sources       []string
	Variants      int
	GenerateTasks bool
	Tournament    bool
}

// RunResult describes created scout artifacts.
type RunResult struct {
	CreatedAt   time.Time
	RunID       string
	Dir         string
	Files       []string
	Ideas       []Idea
	Competitors []Competitor
	Guidance    []GuidanceFile
}

// GuidanceFile is a harness/project instruction file discovered before
// recommendations are generated.
type GuidanceFile struct {
	Path    string `json:"path"`
	Kind    string `json:"kind"`
	Summary string `json:"summary"`
}

// Evidence points an idea or competitor note at an auditable source.
type Evidence struct {
	Kind    string `json:"kind"`
	Path    string `json:"path,omitempty"`
	URL     string `json:"url,omitempty"`
	Source  string `json:"source,omitempty"`
	Excerpt string `json:"excerpt,omitempty"`
}

// Idea is one structured feature or roadmap recommendation.
//
//nolint:govet // JSON field order mirrors the public artifact shape.
type Idea struct {
	Title               string     `json:"title"`
	Summary             string     `json:"summary"`
	Fit                 string     `json:"fit"`
	Complexity          string     `json:"complexity"`
	Risk                string     `json:"risk"`
	SuggestedMVP        string     `json:"suggested_mvp"`
	RelatedFilesOrAreas []string   `json:"related_files_or_areas,omitempty"`
	Evidence            []Evidence `json:"evidence,omitempty"`
	Rationale           string     `json:"rationale"`
	Speculative         bool       `json:"speculative"`
	Score               int        `json:"score"`
}

// Competitor records a named inspiration source. URL references are recorded
// but not fetched by the MVP.
type Competitor struct {
	ID          string     `json:"id"`
	Name        string     `json:"name"`
	URL         string     `json:"url,omitempty"`
	Notes       string     `json:"notes"`
	SourceType  string     `json:"source_type"`
	Evidence    []Evidence `json:"evidence,omitempty"`
	Speculative bool       `json:"speculative"`
}

// RoadmapVariant records one independent proposal lens for tournament mode.
//
//nolint:govet // JSON field order favors stable report rendering.
type RoadmapVariant struct {
	IdeaTitles []string `json:"idea_titles"`
	Kept       []string `json:"kept,omitempty"`
	Discarded  []string `json:"discarded,omitempty"`
	ID         string   `json:"id"`
	Lens       string   `json:"lens"`
	Summary    string   `json:"summary"`
	Rationale  string   `json:"rationale"`
	Score      int      `json:"score"`
}

//nolint:govet // JSON field order favors stable run metadata.
type runRecord struct {
	CreatedAt       time.Time          `json:"created_at"`
	Artifacts       map[string]string  `json:"artifacts"`
	GuidanceFiles   []GuidanceFile     `json:"guidance_files,omitempty"`
	Competitors     []string           `json:"competitors,omitempty"`
	Sources         []string           `json:"sources,omitempty"`
	Tournament      tournament.Options `json:"tournament"`
	Variants        []RoadmapVariant   `json:"variants,omitempty"`
	Notes           []string           `json:"notes,omitempty"`
	Schema          string             `json:"schema"`
	RunID           string             `json:"run_id"`
	Prompt          string             `json:"prompt"`
	Area            string             `json:"area,omitempty"`
	Root            string             `json:"root"`
	OutputDir       string             `json:"output_dir"`
	IdeaCount       int                `json:"idea_count"`
	CompetitorCount int                `json:"competitor_count"`
	GenerateTasks   bool               `json:"generate_tasks"`
}

type guidanceDocument struct {
	GuidanceFile
	Content string
}

type projectContext struct {
	Name              string
	Summary           string
	ReadmeSummary     string
	ValidationCommand string
	Files             []string
	Directories       []string
	Areas             []string
	Evidence          []Evidence
}

type ideaSeed struct {
	title      string
	summary    string
	mvp        string
	rationale  string
	fit        string
	complexity string
	risk       string
	areas      []string
	evidence   []Evidence
	spec       bool
}

// Run creates a scout run directory and writes scout.md, ideas.jsonl,
// competitors.jsonl, run.json, and optionally tasks.generated.yaml.
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

	guidanceDocs, err := discoverGuidance(ctx, root)
	if err != nil {
		return RunResult{}, err
	}

	if err := requireContext(ctx); err != nil {
		return RunResult{}, err
	}

	project := inspectProject(root, guidanceDocs)
	sourceEvidence := buildSourceEvidence(root, sourceInputs(req))
	competitors := buildCompetitors(req.Competitors, req.Prompt)
	tournamentOptions := tournament.Normalize(req.Tournament, req.Variants)
	artifactReq := redactedRunRequest(req)
	ideas := rankIdeas(generateIdeas(artifactReq, project, guidanceDocs, sourceEvidence, competitors))
	variants := buildRoadmapVariants(ideas, tournamentOptions)

	artifacts := scoutArtifactPaths(req.GenerateTasks)
	record := runRecord{
		Schema:          SchemaVersion,
		RunID:           runID,
		Prompt:          artifactReq.Prompt,
		Area:            artifactReq.Area,
		CreatedAt:       createdAt,
		Root:            redactIdentifier(root),
		OutputDir:       redactIdentifier(runDir),
		Artifacts:       artifacts,
		GuidanceFiles:   guidanceSummaries(guidanceDocs),
		Competitors:     redactedInputs(competitorInputs(req)),
		Sources:         redactedInputs(sourceInputs(req)),
		Tournament:      tournamentOptions,
		Variants:        variants,
		IdeaCount:       len(ideas),
		CompetitorCount: len(competitors),
		GenerateTasks:   req.GenerateTasks,
		Notes: []string{
			"Local-first MVP: competitor names and URLs are recorded for inspiration; autonomous web search/fetching is not mandatory and is not performed here.",
			EvidenceBestPractice,
		},
	}

	if err := writeIdeasJSONL(filepath.Join(runDir, ideasFile), ideas); err != nil {
		return RunResult{}, err
	}
	if err := writeCompetitorsJSONL(filepath.Join(runDir, competitorsFile), competitors); err != nil {
		return RunResult{}, err
	}

	report := renderReport(artifactReq, project, guidanceDocs, sourceEvidence, competitors, ideas, variants, record)
	if err := writeTextFile(filepath.Join(runDir, scoutReportFile), report); err != nil {
		return RunResult{}, err
	}

	if req.GenerateTasks {
		if err := writeTextFile(filepath.Join(runDir, tasksFile), renderGeneratedTasks(ideas, project, record)); err != nil {
			return RunResult{}, err
		}
	}

	if err := writeRunJSON(filepath.Join(runDir, runFile), record); err != nil {
		return RunResult{}, err
	}

	return RunResult{
		RunID:       runID,
		Dir:         runDir,
		Files:       resultFiles(req.GenerateTasks),
		Ideas:       ideas,
		Competitors: competitors,
		Guidance:    record.GuidanceFiles,
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

func redactedRunRequest(req RunRequest) RunRequest {
	req.Prompt = redactText(req.Prompt)
	req.Area = redactText(req.Area)
	req.Competitors = redactedInputs(req.Competitors)
	req.Sources = redactedInputs(req.Sources)

	return req
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
	if id := sanitizeRunID(redactIdentifier(req.RunID)); id != "" {
		return id
	}

	if strings.TrimSpace(req.OutputDir) != "" {
		if id := sanitizeRunID(redactIdentifier(filepath.Base(filepath.Clean(req.OutputDir)))); id != "" && id != "." {
			return id
		}
	}

	digest := sha256.Sum256([]byte(redactText(strings.TrimSpace(req.Prompt)) + "\x00" + now.Format(time.RFC3339Nano)))

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

func scoutArtifactPaths(generateTasks bool) map[string]string {
	artifacts := map[string]string{
		"scout":       scoutReportFile,
		"ideas":       ideasFile,
		"competitors": competitorsFile,
		"run":         runFile,
	}
	if generateTasks {
		artifacts["tasks"] = tasksFile
	}

	return artifacts
}

func resultFiles(generateTasks bool) []string {
	files := []string{scoutReportFile, ideasFile, competitorsFile, runFile}
	if generateTasks {
		files = append(files, tasksFile)
	}
	sort.Strings(files)

	return files
}

func sourceInputs(req RunRequest) []string {
	inputs := append([]string(nil), req.Sources...)
	inputs = append(inputs, extractURLs(req.Prompt)...)

	return uniqueTrimmed(inputs)
}

func competitorInputs(req RunRequest) []string {
	inputs := append([]string(nil), req.Competitors...)
	inputs = append(inputs, extractURLs(req.Prompt)...)

	return uniqueTrimmed(inputs)
}

func extractURLs(text string) []string {
	matches := urlPattern.FindAllString(text, -1)
	out := make([]string, 0, len(matches))
	for _, match := range matches {
		out = append(out, strings.TrimRight(match, ".,;:!?)]}\""))
	}

	return uniqueTrimmed(out)
}

func uniqueTrimmed(values []string) []string {
	seen := make(map[string]bool, len(values))
	out := make([]string, 0, len(values))

	for _, value := range values {
		value = strings.TrimSpace(value)
		if value == "" || seen[value] {
			continue
		}

		seen[value] = true
		out = append(out, value)
	}

	return out
}

func redactedInputs(values []string) []string {
	out := make([]string, 0, len(values))
	for _, value := range values {
		out = append(out, redactIdentifier(value))
	}

	return uniqueTrimmed(out)
}

func discoverGuidance(ctx context.Context, root string) ([]guidanceDocument, error) {
	var guidance []guidanceDocument

	err := filepath.WalkDir(root, func(path string, entry fs.DirEntry, walkErr error) error {
		if err := requireContext(ctx); err != nil {
			return err
		}

		if walkErr != nil {
			return nil
		}

		if path != root && entry.IsDir() && shouldSkipDir(entry.Name()) {
			return filepath.SkipDir
		}

		if entry.IsDir() || len(guidance) >= maxGuidanceFiles {
			return nil
		}

		rel, err := filepath.Rel(root, path)
		if err != nil {
			return nil
		}

		kind, ok := guidanceKind(rel)
		if !ok {
			return nil
		}

		content, err := readTextFile(path, maxSourceBytes)
		if err != nil {
			return nil
		}

		guidance = append(guidance, guidanceDocument{
			GuidanceFile: GuidanceFile{Path: redactIdentifier(filepath.ToSlash(rel)), Kind: kind, Summary: summarizeText(content)},
			Content:      content,
		})

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

func shouldSkipDir(name string) bool {
	switch name {
	case ".git", ".atteler", ".symphony", "node_modules", "vendor", "dist", "site", "tmp", "build":
		return true
	default:
		return false
	}
}

func guidanceKind(rel string) (string, bool) {
	slash := filepath.ToSlash(rel)
	base := filepath.Base(slash)
	lower := strings.ToLower(slash)

	switch base {
	case "AGENTS.md":
		return "agents_instructions", true
	case "AGENT.md":
		return "agent_instructions", true
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
	case "WORKFLOW.md":
		return "workflow_guidance", true
	}

	switch {
	case strings.HasPrefix(slash, ".cursor/rules/"):
		return "cursor_rules", true
	case strings.HasPrefix(slash, ".codex/") && guidanceTextLikePath(lower):
		return "codex_project_guidance", true
	case strings.HasPrefix(slash, ".claude/") && guidanceTextLikePath(lower):
		return "claude_project_guidance", true
	case strings.HasPrefix(slash, ".gemini/") && guidanceTextLikePath(lower):
		return "gemini_project_guidance", true
	case strings.HasPrefix(slash, ".windsurf/rules/") && guidanceTextLikePath(lower):
		return "windsurf_rules", true
	case slash == ".github/copilot-instructions.md":
		return "copilot_instructions", true
	case strings.HasPrefix(slash, ".github/") && strings.Contains(filepath.Base(lower), "instructions") && strings.HasSuffix(lower, ".md"):
		return "github_instructions", true
	case strings.HasPrefix(slash, ".agents/") && strings.HasSuffix(strings.ToLower(slash), ".md"):
		return "agent_guidance", true
	}

	return "", false
}

func guidanceTextLikePath(lowerSlash string) bool {
	switch {
	case strings.HasSuffix(lowerSlash, ".md"):
		return true
	case strings.HasSuffix(lowerSlash, ".mdc"):
		return true
	case strings.HasSuffix(lowerSlash, ".txt"):
		return true
	case strings.HasSuffix(lowerSlash, ".toml"):
		return true
	case strings.HasSuffix(lowerSlash, ".yaml"):
		return true
	case strings.HasSuffix(lowerSlash, ".yml"):
		return true
	case strings.HasSuffix(lowerSlash, ".json"):
		return true
	default:
		return false
	}
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

func guidanceSummaries(guidance []guidanceDocument) []GuidanceFile {
	out := make([]GuidanceFile, len(guidance))
	for i := range guidance {
		out[i] = guidance[i].GuidanceFile
	}

	return out
}

func inspectProject(root string, guidance []guidanceDocument) projectContext {
	project := projectContext{Name: redactIdentifier(filepath.Base(root))}
	if project.Name == "." || project.Name == string(filepath.Separator) || project.Name == "" {
		project.Name = "current project"
	}

	if readme, ok := readOptionalFile(root, "README.md"); ok {
		if heading := firstMarkdownHeading(readme); heading != "" {
			project.Name = redactText(heading)
		}
		project.ReadmeSummary = summarizeText(readme)
		project.Evidence = append(project.Evidence, Evidence{Kind: "file", Path: "README.md", Excerpt: project.ReadmeSummary})
	}

	if workflow, ok := readOptionalFile(root, "WORKFLOW.md"); ok {
		project.Evidence = append(project.Evidence, Evidence{Kind: "file", Path: "WORKFLOW.md", Excerpt: summarizeText(workflow)})
	}

	project.Directories = existingDirs(root, []string{"cmd", "internal", "pkg", "docs", "test", "scripts"})
	project.Files = existingFiles(root, []string{"go.mod", "Makefile", "README.md", "WORKFLOW.md", "NOTES.md"})
	project.Areas = projectAreas(root, project.Directories)
	project.ValidationCommand = validationCommand(root, guidance)
	project.Summary = redactText(projectSummary(project))

	return project
}

func readOptionalFile(root, rel string) (string, bool) {
	content, err := readTextFile(filepath.Join(root, rel), maxSourceBytes)
	if err != nil {
		return "", false
	}

	return content, true
}

func existingDirs(root string, candidates []string) []string {
	var out []string
	for _, rel := range candidates {
		info, err := os.Stat(filepath.Join(root, rel))
		if err == nil && info.IsDir() {
			out = append(out, filepath.ToSlash(rel))
		}
	}

	return out
}

func existingFiles(root string, candidates []string) []string {
	var out []string
	for _, rel := range candidates {
		info, err := os.Stat(filepath.Join(root, rel))
		if err == nil && !info.IsDir() {
			out = append(out, filepath.ToSlash(rel))
		}
	}

	return out
}

func projectAreas(root string, dirs []string) []string {
	areas := append([]string(nil), dirs...)
	for _, rel := range []string{"cmd/atteler", "pkg/research", "pkg/autopilot", "pkg/symphony", "pkg/tasklist", "pkg/vector", "pkg/watch"} {
		info, err := os.Stat(filepath.Join(root, rel))
		if err == nil && info.IsDir() {
			areas = append(areas, filepath.ToSlash(rel))
		}
	}

	return uniqueTrimmed(areas)
}

func validationCommand(root string, guidance []guidanceDocument) string {
	guidanceText := strings.ToLower(guidanceContent(guidance))
	if makefile, ok := readOptionalFile(root, "Makefile"); ok {
		commands := []string{"make test"}
		if makeTargetExists(makefile, "lint") && strings.Contains(guidanceText, "lint") {
			commands = append(commands, "make lint")
		}
		if makeTargetExists(makefile, "build") && strings.Contains(guidanceText, "build") {
			commands = append(commands, "make build")
		}

		return strings.Join(commands, " && ")
	}

	if strings.Contains(guidanceText, "test") {
		return "go test ./..."
	}

	if _, err := os.Stat(filepath.Join(root, "go.mod")); err == nil {
		return "go test ./..."
	}

	return "Run the smallest meaningful project-specific validation command."
}

func guidanceContent(guidance []guidanceDocument) string {
	var b strings.Builder
	for _, doc := range guidance {
		b.WriteString(doc.Content)
		b.WriteByte('\n')
	}

	return b.String()
}

func makeTargetExists(makefile, target string) bool {
	target = strings.TrimSpace(target)
	if target == "" {
		return false
	}

	scanner := bufio.NewScanner(strings.NewReader(makefile))
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, "#") || strings.HasPrefix(line, ".") {
			continue
		}

		if strings.HasPrefix(line, target+":") {
			return true
		}
	}

	return false
}

func projectSummary(project projectContext) string {
	parts := []string{project.Name + " repository"}
	if project.ReadmeSummary != "" {
		parts = append(parts, project.ReadmeSummary)
	}
	if len(project.Directories) > 0 {
		parts = append(parts, "visible areas: "+strings.Join(project.Directories, ", "))
	}
	if project.ValidationCommand != "" {
		parts = append(parts, "suggested validation: "+project.ValidationCommand)
	}

	return strings.Join(parts, "; ")
}

func buildCompetitors(inputs []string, prompt string) []Competitor {
	values := append([]string(nil), inputs...)
	values = append(values, extractURLs(prompt)...)
	values = uniqueTrimmed(values)

	competitors := make([]Competitor, 0, len(values))
	for i, value := range values {
		competitors = append(competitors, competitorFromInput(i+1, value))
	}

	return competitors
}

func buildSourceEvidence(root string, inputs []string) []Evidence {
	inputs = uniqueTrimmed(inputs)
	out := make([]Evidence, 0, len(inputs))
	for _, input := range inputs {
		if isURL(input) {
			out = append(out, Evidence{
				Kind:    "url",
				URL:     redactIdentifier(input),
				Source:  "scout_source",
				Excerpt: "URL recorded as a scout source; autonomous web fetching/search is outside the MVP.",
			})
			continue
		}

		out = append(out, localSourceEvidence(root, input)...)
	}

	return out
}

func isURL(raw string) bool {
	parsed, err := url.Parse(strings.TrimSpace(raw))
	return err == nil && parsed.Scheme != "" && parsed.Host != ""
}

func localSourceEvidence(root, input string) []Evidence {
	path := strings.TrimSpace(input)
	if path == "" {
		return nil
	}

	displayPath := redactIdentifier(filepath.ToSlash(path))
	if !filepath.IsAbs(path) {
		path = filepath.Join(root, path)
	}

	info, err := os.Stat(path)
	if err != nil {
		return []Evidence{{
			Kind:    "file",
			Path:    displayPath,
			Source:  "scout_source",
			Excerpt: "source could not be loaded: " + redactText(err.Error()),
		}}
	}

	if !info.IsDir() {
		return []Evidence{localFileEvidence(root, path)}
	}

	var evidence []Evidence
	err = filepath.WalkDir(path, func(child string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return nil
		}

		if child != path && entry.IsDir() && shouldSkipDir(entry.Name()) {
			return filepath.SkipDir
		}

		if entry.IsDir() || len(evidence) >= maxScoutSourceFiles {
			return nil
		}

		evidence = append(evidence, localFileEvidence(root, child))

		return nil
	})
	if err != nil {
		return []Evidence{{
			Kind:    "directory",
			Path:    displayPath,
			Source:  "scout_source",
			Excerpt: "source directory could not be walked: " + redactText(err.Error()),
		}}
	}

	if len(evidence) == 0 {
		return []Evidence{{
			Kind:    "directory",
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
	rel = redactIdentifier(filepath.ToSlash(rel))

	content, err := readTextFile(path, maxSourceBytes)
	if err != nil {
		return Evidence{
			Kind:    "file",
			Path:    rel,
			Source:  "scout_source",
			Excerpt: "source could not be loaded: " + redactText(err.Error()),
		}
	}

	return Evidence{
		Kind:    "file",
		Path:    rel,
		Source:  "scout_source",
		Excerpt: summarizeText(content),
	}
}

func competitorFromInput(index int, value string) Competitor {
	id := fmt.Sprintf("C%d", index)
	if parsed, err := url.Parse(value); err == nil && parsed.Scheme != "" && parsed.Host != "" {
		return Competitor{
			ID:          id,
			Name:        competitorNameFromURL(parsed),
			URL:         redactIdentifier(value),
			SourceType:  sourceTypeCompetitor,
			Notes:       "Competitor or inspiration URL recorded for evidence-backed follow-up; the MVP does not fetch external pages automatically.",
			Evidence:    []Evidence{{Kind: "url", URL: redactIdentifier(value)}},
			Speculative: false,
		}
	}

	name := strings.TrimSpace(value)
	return Competitor{
		ID:          id,
		Name:        redactText(name),
		SourceType:  sourceTypeCompetitor,
		Notes:       "Named competitor or inspiration source supplied by the user; add a URL/source for stronger evidence.",
		Speculative: true,
	}
}

func competitorNameFromURL(parsed *url.URL) string {
	host := strings.TrimPrefix(strings.ToLower(parsed.Hostname()), "www.")
	if host == "" {
		return "external source"
	}

	name := strings.TrimSuffix(host, ".com")
	name = strings.TrimSuffix(name, ".dev")
	name = strings.TrimSuffix(name, ".io")
	name = strings.ReplaceAll(name, "-", " ")

	return name
}

func generateIdeas(
	req RunRequest,
	project projectContext,
	guidance []guidanceDocument,
	sourceEvidence []Evidence,
	competitors []Competitor,
) []Idea {
	commonEvidence := append([]Evidence(nil), project.Evidence...)
	for _, doc := range guidance {
		commonEvidence = append(commonEvidence, Evidence{Kind: "file", Path: doc.Path, Source: sourceTypeGuidance, Excerpt: doc.Summary})
	}
	commonEvidence = append(commonEvidence, sourceEvidence...)
	if len(commonEvidence) == 0 {
		commonEvidence = append(commonEvidence, Evidence{Kind: "repository", Path: ".", Excerpt: "Repository structure inspected for local-first fit."})
	}

	areas := prioritizedAreas(req.Area, project.Areas)
	seeds := []ideaSeed{
		{
			title:      "Evidence-backed discovery reports",
			summary:    "Generate a human-readable roadmap report that separates cited findings from speculative product bets.",
			mvp:        "Write scout.md with project understanding, sources, ranked ideas, risks, and citations when available.",
			rationale:  "Discovery work is easier to trust when users can audit why an idea was recommended.",
			fit:        fitHigh,
			complexity: complexityMedium,
			risk:       riskLow,
			areas:      areasForIdea(areas, "docs", "cmd/atteler"),
			evidence:   commonEvidence,
		},
		{
			title:      "Harness-aware implementation task generator",
			summary:    "Turn selected roadmap ideas into task stubs that inherit repository validation and instruction constraints.",
			mvp:        "Write tasks.generated.yaml with one task per top idea plus validation steps such as " + project.ValidationCommand + ".",
			rationale:  "Generated tasks should respect project rules such as test-first workflows instead of being free-floating brainstorm notes.",
			fit:        fitHigh,
			complexity: complexityMedium,
			risk:       riskMedium,
			areas:      areasForIdea(areas, "pkg/tasklist", "docs"),
			evidence:   commonEvidence,
		},
		{
			title:      "Competitor inspiration ledger",
			summary:    "Capture competitor or inspiration sources alongside recommendations so product discovery can be revisited and refreshed.",
			mvp:        "Write competitors.jsonl and cite supplied URLs or names in scout.md without requiring web access.",
			rationale:  "Users asked for competitor-aware discovery, but mandatory live intelligence is explicitly out of MVP scope.",
			fit:        fitHigh,
			complexity: complexityLow,
			risk:       riskLow,
			areas:      areasForIdea(areas, "pkg/scout", "docs"),
			evidence:   competitorEvidence(competitors, commonEvidence),
			spec:       len(competitors) == 0,
		},
		{
			title:      "Tournament roadmap proposal mode",
			summary:    "Generate multiple independent roadmap variants, compare them, and merge the strongest recommendations.",
			mvp:        "Support --tournament and --variants to render candidate roadmaps, a comparison table, and kept/discarded rationale.",
			rationale:  "Parallel proposal comparison helps avoid anchoring on a single brainstorm and can be shared with autoresearch hypotheses.",
			fit:        fitHigh,
			complexity: complexityMedium,
			risk:       riskMedium,
			areas:      areasForIdea(areas, "pkg/tournament", "pkg/autopilot"),
			evidence:   commonEvidence,
		},
		{
			title:      "Repository-fit scoring rubric",
			summary:    "Score ideas by fit, complexity, risk, validation clarity, and nearby code ownership.",
			mvp:        "Add deterministic scoring fields to ideas.jsonl and sort scout.md recommendations by score.",
			rationale:  "A ranked roadmap is more useful than an unordered idea list, especially when implementation cost differs widely.",
			fit:        fitHigh,
			complexity: complexityLow,
			risk:       riskLow,
			areas:      areasForIdea(areas, "pkg/scout", "cmd/atteler"),
			evidence:   commonEvidence,
		},
		{
			title:      "Area-focused scouting presets",
			summary:    "Let users bias discovery toward a subsystem or theme while still considering whole-project constraints.",
			mvp:        "Support --area in run metadata, report sections, and related_files_or_areas scoring.",
			rationale:  "Focused prompts such as autoresearch or UX need different recommendations than broad product discovery.",
			fit:        fitMedium,
			complexity: complexityLow,
			risk:       riskLow,
			areas:      areasForIdea(areas, strings.TrimSpace(req.Area), "docs"),
			evidence:   commonEvidence,
		},
		{
			title:      "Issue and session history mining",
			summary:    "Use prior issues, sessions, and saved artifacts as product evidence for recurring user pain points.",
			mvp:        "Record placeholders for issue/session sources and recommend importing explicit local files or session exports.",
			rationale:  "Roadmaps improve when they learn from historical failures and repeated requests, not only current source files.",
			fit:        fitMedium,
			complexity: complexityHigh,
			risk:       riskMedium,
			areas:      areasForIdea(areas, "pkg/session", "pkg/symphony"),
			evidence:   commonEvidence,
			spec:       true,
		},
		{
			title:      "Validation-plan aware roadmap ordering",
			summary:    "Prefer ideas with clear MVPs and validation commands before high-risk platform bets.",
			mvp:        "Add suggested implementation order and validation notes to scout.md and tasks.generated.yaml.",
			rationale:  "An idea is higher leverage when users can verify it cheaply and safely.",
			fit:        fitHigh,
			complexity: complexityLow,
			risk:       riskLow,
			areas:      areasForIdea(areas, "Makefile", "test"),
			evidence:   commonEvidence,
		},
		{
			title:      "Speculation labels and confidence bands",
			summary:    "Mark low-evidence recommendations as speculative instead of presenting all ideas with equal certainty.",
			mvp:        "Include speculative and evidence fields in ideas.jsonl and explain speculation in scout.md.",
			rationale:  "The issue explicitly allows speculative ideas but asks for clear labeling.",
			fit:        fitHigh,
			complexity: complexityLow,
			risk:       riskLow,
			areas:      areasForIdea(areas, "pkg/scout", "docs"),
			evidence:   commonEvidence,
		},
		{
			title:      "Roadmap import bridge for task workflows",
			summary:    "Make generated task YAML easy to review and import into existing task or issue workflows later.",
			mvp:        "Keep task IDs stable, include related files, validation commands, and source scout run metadata.",
			rationale:  "Scout should decide what to build next without automatically opening issues in the MVP.",
			fit:        fitMedium,
			complexity: complexityMedium,
			risk:       riskMedium,
			areas:      areasForIdea(areas, "pkg/tasklist", "cmd/atteler"),
			evidence:   commonEvidence,
		},
	}

	if strings.TrimSpace(req.Area) != "" {
		seeds = append([]ideaSeed{{
			title:      strings.TrimSpace(req.Area) + "-specific roadmap slice",
			summary:    "Generate a focused set of improvements for the requested area while carrying project-wide constraints forward.",
			mvp:        "Add area-specific notes, related files, and validation steps to the top scout recommendations.",
			rationale:  "The user explicitly scoped discovery to an area, so high-fit ideas should name and prioritize that surface.",
			fit:        fitHigh,
			complexity: complexityLow,
			risk:       riskLow,
			areas:      areasForIdea(areas, req.Area),
			evidence:   commonEvidence,
		}}, seeds...)
	}

	ideas := make([]Idea, 0, len(seeds))
	for i := range seeds {
		seed := &seeds[i]
		idea := Idea{
			Title:               seed.title,
			Summary:             seed.summary,
			Fit:                 seed.fit,
			Complexity:          seed.complexity,
			Risk:                seed.risk,
			SuggestedMVP:        seed.mvp,
			Rationale:           seed.rationale,
			RelatedFilesOrAreas: limitAreas(uniqueTrimmed(seed.areas), maxRelatedFileAreas),
			Evidence:            limitEvidence(seed.evidence, maxIdeaEvidence),
			Speculative:         seed.spec,
		}
		idea.Score = scoreIdea(idea)
		ideas = append(ideas, idea)
	}

	return ideas
}

func prioritizedAreas(area string, projectAreas []string) []string {
	areas := append([]string(nil), projectAreas...)
	if strings.TrimSpace(area) != "" {
		areas = append([]string{strings.TrimSpace(area)}, areas...)
	}

	return uniqueTrimmed(areas)
}

func areasForIdea(available []string, preferred ...string) []string {
	var out []string
	for _, item := range preferred {
		item = strings.TrimSpace(item)
		if item != "" {
			out = append(out, filepath.ToSlash(item))
		}
	}
	for _, item := range available {
		if len(out) >= maxRelatedFileAreas {
			break
		}
		out = append(out, item)
	}

	return uniqueTrimmed(out)
}

func competitorEvidence(competitors []Competitor, fallback []Evidence) []Evidence {
	var out []Evidence
	for _, competitor := range competitors {
		out = append(out, competitor.Evidence...)
	}
	if len(out) == 0 {
		return fallback
	}

	return out
}

func limitAreas(values []string, limit int) []string {
	if limit <= 0 || len(values) <= limit {
		return values
	}

	return append([]string(nil), values[:limit]...)
}

func limitEvidence(values []Evidence, limit int) []Evidence {
	values = uniqueEvidence(values)
	if limit <= 0 || len(values) <= limit {
		return values
	}

	return append([]Evidence(nil), values[:limit]...)
}

func uniqueEvidence(values []Evidence) []Evidence {
	seen := make(map[string]bool, len(values))
	out := make([]Evidence, 0, len(values))
	for i := range values {
		key := evidenceKey(values[i])
		if key == "" {
			out = append(out, values[i])
			continue
		}

		if seen[key] {
			continue
		}

		seen[key] = true
		out = append(out, values[i])
	}

	return out
}

func evidenceKey(evidence Evidence) string {
	switch {
	case strings.TrimSpace(evidence.URL) != "":
		return "url:" + strings.TrimSpace(evidence.URL)
	case strings.TrimSpace(evidence.Path) != "":
		return "path:" + filepath.ToSlash(strings.TrimSpace(evidence.Path))
	case strings.TrimSpace(evidence.Source) != "":
		return "source:" + strings.TrimSpace(evidence.Source)
	default:
		return ""
	}
}

func scoreIdea(idea Idea) int {
	score := 0
	switch idea.Fit {
	case fitHigh:
		score += 50
	case fitMedium:
		score += 35
	default:
		score += 20
	}

	switch idea.Complexity {
	case complexityLow:
		score += 25
	case complexityMedium:
		score += 15
	default:
		score += 5
	}

	switch idea.Risk {
	case riskLow:
		score += 20
	case riskMedium:
		score += 10
	default:
		score += 3
	}

	if len(idea.Evidence) > 0 && !idea.Speculative {
		score += 5
	}
	if idea.Speculative {
		score -= 5
	}

	return score
}

func rankIdeas(ideas []Idea) []Idea {
	seen := make(map[string]bool, len(ideas))
	out := make([]Idea, 0, len(ideas))
	for i := range ideas {
		idea := ideas[i]
		key := strings.ToLower(strings.TrimSpace(idea.Title))
		if key == "" || seen[key] {
			continue
		}
		seen[key] = true
		out = append(out, idea)
	}

	sort.SliceStable(out, func(i, j int) bool {
		if out[i].Score != out[j].Score {
			return out[i].Score > out[j].Score
		}

		return out[i].Title < out[j].Title
	})

	if len(out) > defaultIdeaCount {
		return out[:defaultIdeaCount]
	}

	return out
}

func buildRoadmapVariants(ideas []Idea, options tournament.Options) []RoadmapVariant {
	options = tournament.Normalize(options.Enabled, options.Variants)
	if !options.Active() || len(ideas) == 0 {
		return nil
	}

	lenses := []string{"evidence and auditability", "user experience and adoption", "implementation feasibility", "automation leverage", "risk reduction", "ecosystem integrations"}
	count := options.Count()
	variants := make([]RoadmapVariant, 0, count)
	for i := range count {
		lens := tournament.NormalizeLens(lenses[i%len(lenses)], i+1)
		selected := selectVariantIdeas(ideas, i)
		variant := RoadmapVariant{
			ID:         tournament.VariantID(i + 1),
			Lens:       lens,
			IdeaTitles: ideaTitles(selected),
			Score:      sumIdeaScores(selected),
			Summary:    fmt.Sprintf("Roadmap variant optimized for %s.", lens),
			Rationale:  "Independent deterministic proposal over the same idea pool; compare scores, risk, and implementation clarity before merging.",
		}
		variant.Kept, variant.Discarded = variantDecisions(ideas, selected)
		variants = append(variants, variant)
	}

	return variants
}

func selectVariantIdeas(ideas []Idea, offset int) []Idea {
	if len(ideas) <= 3 {
		return append([]Idea(nil), ideas...)
	}

	selected := []Idea{ideas[0]}
	for i := 1; i < len(ideas) && len(selected) < 4; i++ {
		index := (i + offset) % len(ideas)
		if containsIdeaTitle(selected, ideas[index].Title) {
			continue
		}
		selected = append(selected, ideas[index])
	}

	return selected
}

func containsIdeaTitle(ideas []Idea, title string) bool {
	for i := range ideas {
		if ideas[i].Title == title {
			return true
		}
	}

	return false
}

func ideaTitles(ideas []Idea) []string {
	out := make([]string, 0, len(ideas))
	for i := range ideas {
		out = append(out, ideas[i].Title)
	}

	return out
}

func sumIdeaScores(ideas []Idea) int {
	total := 0
	for i := range ideas {
		total += ideas[i].Score
	}

	return total
}

func variantDecisions(all, selected []Idea) (kept, discarded []string) {
	selectedTitles := make(map[string]bool, len(selected))
	for i := range selected {
		selectedTitles[selected[i].Title] = true
		kept = append(kept, selected[i].Title)
	}

	for i := range all {
		if selectedTitles[all[i].Title] {
			continue
		}
		discarded = append(discarded, all[i].Title)
		if len(discarded) >= 3 {
			break
		}
	}

	return kept, discarded
}

func writeIdeasJSONL(path string, ideas []Idea) error {
	file, err := os.OpenFile(path, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o600)
	if err != nil {
		return fmt.Errorf("scout: create ideas %s: %w", path, err)
	}
	defer file.Close()

	encoder := json.NewEncoder(file)
	for i := range ideas {
		if err := encoder.Encode(ideas[i]); err != nil {
			return fmt.Errorf("scout: encode idea: %w", err)
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
			return fmt.Errorf("scout: encode competitor: %w", err)
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
	guidance []guidanceDocument,
	sourceEvidence []Evidence,
	competitors []Competitor,
	ideas []Idea,
	variants []RoadmapVariant,
	record runRecord,
) string {
	var b strings.Builder

	fmt.Fprintf(&b, "# Scout: %s\n\n", record.Prompt)
	fmt.Fprintf(&b, "Run ID: `%s`  \n", record.RunID)
	fmt.Fprintf(&b, "Created: `%s`\n\n", record.CreatedAt.Format(time.RFC3339))

	fmt.Fprintln(&b, "## Project understanding")
	fmt.Fprintf(&b, "%s\n\n", project.Summary)
	if len(project.Areas) > 0 {
		fmt.Fprintf(&b, "Likely implementation areas: `%s`.\n\n", strings.Join(project.Areas, "`, `"))
	}
	if strings.TrimSpace(req.Area) != "" {
		fmt.Fprintf(&b, "Requested focus area: `%s`.\n\n", strings.TrimSpace(req.Area))
	}

	fmt.Fprintln(&b, "## Harness and project guidance")
	if len(guidance) == 0 {
		fmt.Fprintln(&b, "No AGENTS.md, CLAUDE.md, Cursor rules, or similar harness guidance files were found.")
	} else {
		for _, doc := range guidance {
			fmt.Fprintf(&b, "- `%s` (%s): %s\n", doc.Path, doc.Kind, doc.Summary)
		}
	}
	fmt.Fprintln(&b)

	fmt.Fprintln(&b, "## Inspiration sources")
	fmt.Fprintln(&b, EvidenceBestPractice)
	if len(competitors) == 0 {
		fmt.Fprintln(&b, "- No explicit competitors or inspiration URLs were supplied. Ideas below are local-first and should be treated as project-fit hypotheses.")
	} else {
		for _, competitor := range competitors {
			label := competitor.Name
			if competitor.URL != "" {
				label += " — " + competitor.URL
			}
			fmt.Fprintf(&b, "- [%s] %s: %s\n", competitor.ID, label, competitor.Notes)
		}
	}
	if len(sourceEvidence) > 0 {
		fmt.Fprintln(&b, "- Additional scout sources were loaded or recorded:")
		for i := range sourceEvidence {
			fmt.Fprintf(&b, "  - %s\n", evidenceDetail(sourceEvidence[i]))
		}
	}
	fmt.Fprintln(&b)

	fmt.Fprintln(&b, "## Ranked feature ideas")
	for i := range ideas {
		idea := &ideas[i]
		fmt.Fprintf(&b, "%d. **%s** (score %d, fit `%s`, complexity `%s`, risk `%s`)\n", i+1, idea.Title, idea.Score, idea.Fit, idea.Complexity, idea.Risk)
		fmt.Fprintf(&b, "   - Summary: %s\n", idea.Summary)
		fmt.Fprintf(&b, "   - Rationale: %s\n", idea.Rationale)
		fmt.Fprintf(&b, "   - MVP shape: %s\n", idea.SuggestedMVP)
		if len(idea.RelatedFilesOrAreas) > 0 {
			fmt.Fprintf(&b, "   - Related files/areas: `%s`\n", strings.Join(idea.RelatedFilesOrAreas, "`, `"))
		}
		if idea.Speculative {
			fmt.Fprintln(&b, "   - Evidence posture: speculative; validate with fresh sources before committing roadmap capacity.")
		} else if len(idea.Evidence) > 0 {
			fmt.Fprintf(&b, "   - Evidence: %s\n", evidenceSummary(idea.Evidence))
		}
	}
	fmt.Fprintln(&b)

	if len(variants) > 0 {
		fmt.Fprintln(&b, "## Tournament comparison")
		fmt.Fprintln(&b, "| Variant | Lens | Score | Kept ideas | Discarded examples |")
		fmt.Fprintln(&b, "|---|---|---:|---|---|")
		for i := range variants {
			variant := &variants[i]
			fmt.Fprintf(&b, "| %s | %s | %d | %s | %s |\n", variant.ID, variant.Lens, variant.Score, strings.Join(variant.Kept, "; "), strings.Join(variant.Discarded, "; "))
		}
		fmt.Fprintln(&b)
		fmt.Fprintln(&b, "Merged final recommendation: keep the highest-scoring common ideas first, then pull in lens-specific ideas when they have clear validation steps and low coordination risk.")
		fmt.Fprintln(&b)
	}

	fmt.Fprintln(&b, "## Suggested implementation order")
	for i := range ideas {
		if i >= 5 {
			break
		}
		idea := &ideas[i]
		fmt.Fprintf(&b, "%d. %s — start with %s; validate with `%s`.\n", i+1, idea.Title, lowerFirst(idea.SuggestedMVP), project.ValidationCommand)
	}
	fmt.Fprintln(&b)

	fmt.Fprintln(&b, "## Risks and open questions")
	fmt.Fprintln(&b, "- Competitor intelligence is only as fresh as supplied source names/URLs; rerun with explicit docs or product pages when current market details matter.")
	fmt.Fprintln(&b, "- Ideas marked speculative need stronger evidence before large implementation investment.")
	fmt.Fprintln(&b, "- Generated implementation tasks are reviewable starting points, not automatic issue creation or PR creation.")
	fmt.Fprintln(&b)

	fmt.Fprintln(&b, "## Artifact map")
	for _, name := range sortedArtifactNames(record.Artifacts) {
		fmt.Fprintf(&b, "- `%s`: `%s`\n", name, record.Artifacts[name])
	}

	return b.String()
}

func sortedArtifactNames(artifacts map[string]string) []string {
	names := make([]string, 0, len(artifacts))
	for name := range artifacts {
		names = append(names, name)
	}
	sort.Strings(names)

	return names
}

func evidenceSummary(evidence []Evidence) string {
	parts := make([]string, 0, len(evidence))
	for _, item := range evidence {
		switch {
		case item.URL != "":
			parts = append(parts, item.URL)
		case item.Path != "":
			parts = append(parts, item.Path)
		case item.Source != "":
			parts = append(parts, item.Source)
		default:
			parts = append(parts, item.Kind)
		}
	}

	return strings.Join(parts, ", ")
}

func evidenceDetail(evidence Evidence) string {
	target := evidence.Kind
	switch {
	case evidence.URL != "":
		target = evidence.URL
	case evidence.Path != "":
		target = evidence.Path
	case evidence.Source != "":
		target = evidence.Source
	}

	if strings.TrimSpace(evidence.Excerpt) == "" {
		return target
	}

	return target + " — " + evidence.Excerpt
}

func renderGeneratedTasks(ideas []Idea, project projectContext, record runRecord) string {
	var b strings.Builder
	fmt.Fprintln(&b, "tasks:")
	limit := min(5, len(ideas))
	for i := range limit {
		idea := ideas[i]
		fmt.Fprintf(&b, "  - id: \"scout-%s-%02d\"\n", escapeYAMLDoubleQuoted(record.RunID), i+1)
		fmt.Fprintf(&b, "    title: \"%s\"\n", escapeYAMLDoubleQuoted(idea.Title))
		fmt.Fprintf(&b, "    rationale: \"%s\"\n", escapeYAMLDoubleQuoted(idea.Rationale))
		fmt.Fprintf(&b, "    suggested_mvp: \"%s\"\n", escapeYAMLDoubleQuoted(idea.SuggestedMVP))
		fmt.Fprintln(&b, "    related_files_or_areas:")
		for _, area := range idea.RelatedFilesOrAreas {
			fmt.Fprintf(&b, "      - \"%s\"\n", escapeYAMLDoubleQuoted(area))
		}
		fmt.Fprintln(&b, "    suggested_validation:")
		fmt.Fprintln(&b, "      - \"Review scout.md rationale and ideas.jsonl evidence before implementation\"")
		fmt.Fprintf(&b, "      - \"%s\"\n", escapeYAMLDoubleQuoted(project.ValidationCommand))
		fmt.Fprintf(&b, "    source_run: \"%s\"\n", escapeYAMLDoubleQuoted(record.RunID))
		fmt.Fprintf(&b, "    source_prompt: \"%s\"\n", escapeYAMLDoubleQuoted(record.Prompt))
	}

	return b.String()
}

func escapeYAMLDoubleQuoted(value string) string {
	value = strings.ReplaceAll(value, "\\", "\\\\")
	value = strings.ReplaceAll(value, "\"", "\\\"")

	return value
}

func firstMarkdownHeading(content string) string {
	scanner := bufio.NewScanner(strings.NewReader(content))
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if strings.HasPrefix(line, "#") {
			line = strings.TrimLeft(line, "#")
			line = strings.TrimSpace(line)
			if line != "" {
				return truncateRunes(redactText(line), maxReportLineRunes)
			}
		}
	}

	return ""
}

func summarizeText(content string) string {
	scanner := bufio.NewScanner(strings.NewReader(content))
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		line = strings.Trim(line, "#*` ")
		if line != "" {
			return truncateRunes(redactText(line), maxReportLineRunes)
		}
	}

	return "Readable text source."
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

func lowerFirst(value string) string {
	value = strings.TrimSpace(value)
	if value == "" {
		return value
	}
	value = strings.TrimRight(value, ".!?")

	runes := []rune(value)
	runes[0] = unicode.ToLower(runes[0])

	return string(runes)
}

func redactText(value string) string {
	return privacy.RedactText(privacy.RedactIdentifier(value))
}

func redactIdentifier(value string) string {
	return privacy.RedactIdentifier(value)
}
