// Package research creates local-first research run artifacts.
//
//nolint:wsl_v5,nilerr // Artifact assembly uses compact sequential builders; discovery intentionally skips unreadable optional guidance files.
package research

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
)

const (
	// SchemaVersion is the machine-readable run metadata schema version.
	SchemaVersion = "atteler.research.run.v1"

	researchReportFile = "research.md"
	sourcesFile        = "sources.jsonl"
	claimsFile         = "claims.jsonl"
	tasksFile          = "tasks.generated.yaml"
	runFile            = "run.json"

	defaultSourceTrustScore = 0.70
	guidanceTrustScore      = 0.90
	repositoryTrustScore    = 0.80
	trustedURLTrustScore    = 0.95
	untrustedURLTrustScore  = 0.50

	maxGuidanceFiles       = 64
	maxResearchSourceFiles = 64
	maxSourceBytes         = 64 * 1024
	maxReportLineRunes     = 180

	sourceTypeProjectGuidance = "project_guidance"
	sourceTypeRepositoryFile  = "repository_file"
)

// EvidenceBestPractice is the recommended reliability posture for research reports.
const EvidenceBestPractice = "Atteler research reports should include evidence for important claims whenever possible. Evidence can include URLs, documentation links, repository files, command output, tests, logs, or prior session artifacts. This improves reliability and makes research easier to audit, but evidence is not mandatory for every statement."

// RunRequest configures one research run.
//
// The MVP is intentionally local-first: it records URL references supplied by
// the user, reads local repository/context files, and discovers harness guidance
// files. Autonomous web search can be added behind a provider later without
// changing the artifact contract.
type RunRequest struct {
	Now            time.Time
	Question       string
	Root           string
	OutputDir      string
	RunID          string
	TrustedSources []string
	Sources        []string
	GenerateTasks  bool
}

// RunResult describes the created research run artifacts.
type RunResult struct {
	CreatedAt time.Time
	RunID     string
	Dir       string
	Files     []string
	Sources   []Source
	Claims    []Claim
}

// Source records one consulted or user-supplied source.
//
// URL is populated for remote references; Path is populated for local files.
//
//nolint:govet // JSON field order follows the documented source artifact shape.
type Source struct {
	RetrievedAt time.Time `json:"retrieved_at"`
	TrustScore  float64   `json:"trust_score"`
	ID          string    `json:"id"`
	URL         string    `json:"url,omitempty"`
	Path        string    `json:"path,omitempty"`
	Title       string    `json:"title"`
	SourceType  string    `json:"source_type"`
	Notes       string    `json:"notes,omitempty"`
}

// Evidence maps a claim back to a concrete supporting artifact or source.
type Evidence struct {
	Kind    string `json:"kind"`
	URL     string `json:"url,omitempty"`
	Path    string `json:"path,omitempty"`
	Source  string `json:"source,omitempty"`
	Excerpt string `json:"excerpt,omitempty"`
}

// Claim records an important research claim and its supporting evidence.
//
//nolint:govet // JSON field order keeps claim text before evidence and confidence.
type Claim struct {
	Claim      string     `json:"claim"`
	Evidence   []Evidence `json:"evidence,omitempty"`
	Confidence string     `json:"confidence"`
}

//nolint:govet // JSON field order favors stable, human-readable run metadata.
type runRecord struct {
	CreatedAt      time.Time         `json:"created_at"`
	Artifacts      map[string]string `json:"artifacts"`
	TrustedSources []string          `json:"trusted_sources,omitempty"`
	GuidanceFiles  []string          `json:"guidance_files,omitempty"`
	SourceInputs   []string          `json:"source_inputs,omitempty"`
	Notes          []string          `json:"notes,omitempty"`
	Schema         string            `json:"schema"`
	RunID          string            `json:"run_id"`
	Question       string            `json:"question"`
	Root           string            `json:"root"`
	OutputDir      string            `json:"output_dir"`
	SourceCount    int               `json:"source_count"`
	ClaimCount     int               `json:"claim_count"`
	GenerateTasks  bool              `json:"generate_tasks"`
}

type sourceDocument struct {
	Source
	Content       string
	Summary       string
	RelevantLines []string
}

type guidanceFile struct {
	Path    string
	Kind    string
	Content string
}

var urlPattern = regexp.MustCompile(`(?i)\bhttps?://[^\s"'<>]+`)

// Run creates a research run directory and writes research.md, sources.jsonl,
// claims.jsonl, run.json, and optionally tasks.generated.yaml.
func Run(ctx context.Context, req RunRequest) (RunResult, error) {
	if err := requireContext(ctx); err != nil {
		return RunResult{}, err
	}

	req.Question = strings.TrimSpace(req.Question)
	if req.Question == "" {
		return RunResult{}, errors.New("research: question is required")
	}

	root, err := normalizeRoot(req.Root)
	if err != nil {
		return RunResult{}, err
	}

	createdAt := req.Now.UTC()
	if createdAt.IsZero() {
		createdAt = time.Now().UTC()
	}

	runID := researchRunID(req, createdAt)
	runDir := researchRunDir(root, req.OutputDir, runID)
	if mkErr := os.MkdirAll(runDir, 0o750); mkErr != nil {
		return RunResult{}, fmt.Errorf("research: create run dir %s: %w", runDir, mkErr)
	}

	trustedSources := normalizeTrustedSources(req.TrustedSources)
	sourceInputs := researchSourceInputs(req)

	guidance, err := discoverGuidance(root)
	if err != nil {
		return RunResult{}, err
	}

	docs, err := buildSourceDocuments(ctx, root, req.Question, guidance, sourceInputs, trustedSources, createdAt)
	if err != nil {
		return RunResult{}, err
	}

	artifacts := researchArtifactPaths(req.GenerateTasks)
	record := runRecord{
		Schema:         SchemaVersion,
		RunID:          runID,
		Question:       req.Question,
		CreatedAt:      createdAt,
		Root:           root,
		OutputDir:      runDir,
		Artifacts:      artifacts,
		TrustedSources: trustedSources,
		SourceInputs:   sourceInputs,
		GuidanceFiles:  guidancePaths(guidance),
		SourceCount:    len(docs),
		GenerateTasks:  req.GenerateTasks,
		Notes: []string{
			"Local-first MVP: autonomous web search is not performed; URL sources are recorded for audit/follow-up.",
			EvidenceBestPractice,
		},
	}

	claims := buildClaims(req.Question, docs, trustedSources, record)
	record.ClaimCount = len(claims)

	if err := writeSourcesJSONL(filepath.Join(runDir, sourcesFile), docs); err != nil {
		return RunResult{}, err
	}

	if err := writeClaimsJSONL(filepath.Join(runDir, claimsFile), claims); err != nil {
		return RunResult{}, err
	}

	report := renderReport(req.Question, docs, claims, trustedSources, record)
	if err := writeTextFile(filepath.Join(runDir, researchReportFile), report); err != nil {
		return RunResult{}, err
	}

	if req.GenerateTasks {
		if err := writeTextFile(filepath.Join(runDir, tasksFile), renderGeneratedTasks(req.Question, runID)); err != nil {
			return RunResult{}, err
		}
	}

	if err := writeRunJSON(filepath.Join(runDir, runFile), record); err != nil {
		return RunResult{}, err
	}

	files := resultFiles(req.GenerateTasks)

	return RunResult{
		RunID:     runID,
		Dir:       runDir,
		Files:     files,
		Sources:   sourcesFromDocuments(docs),
		Claims:    claims,
		CreatedAt: createdAt,
	}, nil
}

func requireContext(ctx context.Context) error {
	if ctx == nil {
		return errors.New("research: context is required")
	}

	if err := ctx.Err(); err != nil {
		return fmt.Errorf("research: context already done: %w", err)
	}

	return nil
}

func normalizeRoot(root string) (string, error) {
	root = strings.TrimSpace(root)
	if root == "" {
		cwd, err := os.Getwd()
		if err != nil {
			return "", fmt.Errorf("research: locate working directory: %w", err)
		}
		root = cwd
	}

	abs, err := filepath.Abs(root)
	if err != nil {
		return "", fmt.Errorf("research: resolve root %s: %w", root, err)
	}

	return filepath.Clean(abs), nil
}

func researchRunID(req RunRequest, now time.Time) string {
	if id := sanitizeRunID(req.RunID); id != "" {
		return id
	}

	if strings.TrimSpace(req.OutputDir) != "" {
		if id := sanitizeRunID(filepath.Base(filepath.Clean(req.OutputDir))); id != "" && id != "." {
			return id
		}
	}

	digest := sha256.Sum256([]byte(strings.TrimSpace(req.Question) + "\x00" + now.Format(time.RFC3339Nano)))

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

func researchRunDir(root, outputDir, runID string) string {
	outputDir = strings.TrimSpace(outputDir)
	if outputDir == "" {
		return filepath.Join(root, ".atteler", "runs", "research", runID)
	}

	if filepath.IsAbs(outputDir) {
		return filepath.Clean(outputDir)
	}

	return filepath.Join(root, filepath.Clean(outputDir))
}

func researchArtifactPaths(generateTasks bool) map[string]string {
	artifacts := map[string]string{
		"research": researchReportFile,
		"sources":  sourcesFile,
		"claims":   claimsFile,
		"run":      runFile,
	}
	if generateTasks {
		artifacts["tasks"] = tasksFile
	}

	return artifacts
}

func resultFiles(generateTasks bool) []string {
	files := []string{researchReportFile, sourcesFile, claimsFile, runFile}
	if generateTasks {
		files = append(files, tasksFile)
	}
	sort.Strings(files)

	return files
}

func researchSourceInputs(req RunRequest) []string {
	inputs := append([]string(nil), req.Sources...)
	inputs = append(inputs, extractURLs(req.Question)...)

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

func normalizeTrustedSources(values []string) []string {
	out := make([]string, 0, len(values))
	seen := make(map[string]bool, len(values))

	for _, value := range values {
		normalized := normalizeTrustedSource(value)
		if normalized == "" || seen[normalized] {
			continue
		}

		seen[normalized] = true
		out = append(out, normalized)
	}

	sort.Strings(out)

	return out
}

func normalizeTrustedSource(value string) string {
	value = strings.ToLower(strings.TrimSpace(value))
	if value == "" {
		return ""
	}

	if parsed, err := url.Parse(value); err == nil && parsed.Host != "" {
		value = parsed.Host
	}

	value = strings.TrimPrefix(value, "www.")
	value = strings.Trim(value, "/")
	if host, _, ok := strings.Cut(value, "/"); ok {
		value = host
	}

	return value
}

func discoverGuidance(root string) ([]guidanceFile, error) {
	var guidance []guidanceFile

	err := filepath.WalkDir(root, func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return nil
		}

		if path != root && entry.IsDir() && shouldSkipGuidanceDir(entry.Name()) {
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

		guidance = append(guidance, guidanceFile{Path: filepath.ToSlash(rel), Kind: kind, Content: content})

		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("research: discover guidance: %w", err)
	}

	sort.Slice(guidance, func(i, j int) bool {
		return guidance[i].Path < guidance[j].Path
	})

	return guidance, nil
}

func shouldSkipGuidanceDir(name string) bool {
	switch name {
	case ".git", ".atteler", ".symphony", ".codex", "node_modules", "vendor", "dist", "site", "tmp", "build":
		return true
	default:
		return false
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
	}

	if strings.HasPrefix(slash, ".cursor/rules/") {
		return "cursor_rules", true
	}

	if slash == ".github/copilot-instructions.md" {
		return "copilot_instructions", true
	}

	return "", false
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

func buildSourceDocuments(
	ctx context.Context,
	root string,
	question string,
	guidance []guidanceFile,
	inputs []string,
	trustedSources []string,
	retrievedAt time.Time,
) ([]sourceDocument, error) {
	if err := requireContext(ctx); err != nil {
		return nil, err
	}

	docs := make([]sourceDocument, 0, len(guidance)+len(inputs))
	seen := make(map[string]bool)

	for _, file := range guidance {
		source := Source{
			ID:          sourceID(len(docs) + 1),
			Path:        file.Path,
			Title:       sourceTitle(file.Path, file.Content),
			SourceType:  sourceTypeProjectGuidance,
			RetrievedAt: retrievedAt,
			TrustScore:  guidanceTrustScore,
			Notes:       file.Kind,
		}
		docs = appendSourceDocument(docs, seen, source, file.Content, nil)
	}

	keywords := keywordsForQuestion(question)
	for _, input := range inputs {
		if err := requireContext(ctx); err != nil {
			return nil, err
		}

		if isURL(input) {
			source := urlSource(input, trustedSources, retrievedAt)
			docs = appendSourceDocument(docs, seen, source, "", keywords)
			continue
		}

		loaded, err := loadLocalSource(root, input, retrievedAt)
		if err != nil {
			source := Source{
				ID:          sourceID(len(docs) + 1),
				Path:        filepath.ToSlash(input),
				Title:       filepath.Base(input),
				SourceType:  sourceTypeRepositoryFile,
				RetrievedAt: retrievedAt,
				TrustScore:  repositoryTrustScore,
				Notes:       "source could not be loaded: " + err.Error(),
			}
			docs = appendSourceDocument(docs, seen, source, "", keywords)
			continue
		}

		for i := range loaded {
			docs = appendSourceDocument(docs, seen, loaded[i].Source, loaded[i].Content, keywords)
		}
	}

	for i := range docs {
		docs[i].ID = sourceID(i + 1)
	}

	return docs, nil
}

func appendSourceDocument(docs []sourceDocument, seen map[string]bool, source Source, content string, keywords []string) []sourceDocument {
	key := source.URL + "\x00" + source.Path
	if seen[key] {
		return docs
	}
	seen[key] = true

	if source.TrustScore == 0 {
		source.TrustScore = defaultSourceTrustScore
	}

	doc := sourceDocument{
		Source:        source,
		Content:       content,
		Summary:       sourceSummary(source, content),
		RelevantLines: relevantLines(content, keywords, 2),
	}

	return append(docs, doc)
}

func sourceID(n int) string {
	return fmt.Sprintf("S%d", n)
}

func isURL(raw string) bool {
	parsed, err := url.Parse(strings.TrimSpace(raw))
	return err == nil && parsed.Scheme != "" && parsed.Host != ""
}

func urlSource(raw string, trustedSources []string, retrievedAt time.Time) Source {
	parsed, err := url.Parse(strings.TrimSpace(raw))
	if err != nil {
		parsed = &url.URL{Path: raw}
	}

	host := strings.ToLower(parsed.Hostname())
	trusted := hostTrusted(host, trustedSources)

	trust := untrustedURLTrustScore
	sourceType := "url"
	if trusted {
		trust = trustedURLTrustScore
		sourceType = "trusted_url"
	}
	if officialDocsHost(host) {
		sourceType = "official_docs"
		if trusted {
			trust = trustedURLTrustScore
		}
	}

	return Source{
		URL:         raw,
		Title:       urlTitle(parsed),
		SourceType:  sourceType,
		RetrievedAt: retrievedAt,
		TrustScore:  trust,
		Notes:       "URL recorded for research audit; autonomous web fetching/search is outside the MVP.",
	}
}

func hostTrusted(host string, trustedSources []string) bool {
	host = strings.TrimPrefix(strings.ToLower(strings.TrimSpace(host)), "www.")
	for _, trusted := range trustedSources {
		trusted = strings.TrimPrefix(strings.ToLower(strings.TrimSpace(trusted)), "www.")
		if trusted == "" {
			continue
		}

		if host == trusted || strings.HasSuffix(host, "."+trusted) {
			return true
		}
	}

	return false
}

func officialDocsHost(host string) bool {
	switch strings.TrimPrefix(strings.ToLower(host), "www.") {
	case "go.dev", "pkg.go.dev", "docs.github.com", "github.com", "developer.mozilla.org":
		return true
	default:
		return false
	}
}

func urlTitle(parsed *url.URL) string {
	if parsed == nil {
		return "URL source"
	}

	path := strings.Trim(parsed.Path, "/")
	if path == "" {
		return parsed.Host
	}

	return parsed.Host + "/" + path
}

func loadLocalSource(root, input string, retrievedAt time.Time) ([]sourceDocument, error) {
	path := strings.TrimSpace(input)
	if path == "" {
		return nil, errors.New("empty source path")
	}
	if !filepath.IsAbs(path) {
		path = filepath.Join(root, path)
	}

	info, err := os.Stat(path)
	if err != nil {
		return nil, fmt.Errorf("stat source %s: %w", path, err)
	}

	if !info.IsDir() {
		doc, err := localFileSource(root, path, retrievedAt)
		if err != nil {
			return nil, err
		}

		return []sourceDocument{doc}, nil
	}

	var docs []sourceDocument
	walkErr := filepath.WalkDir(path, func(child string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return nil
		}

		if child != path && entry.IsDir() && shouldSkipGuidanceDir(entry.Name()) {
			return filepath.SkipDir
		}

		if entry.IsDir() || len(docs) >= maxResearchSourceFiles {
			return nil
		}

		doc, err := localFileSource(root, child, retrievedAt)
		if err != nil {
			return nil
		}

		docs = append(docs, doc)

		return nil
	})
	if walkErr != nil {
		return nil, fmt.Errorf("walk source directory %s: %w", path, walkErr)
	}

	return docs, nil
}

func localFileSource(root, path string, retrievedAt time.Time) (sourceDocument, error) {
	content, err := readTextFile(path, maxSourceBytes)
	if err != nil {
		return sourceDocument{}, err
	}

	rel, err := filepath.Rel(root, path)
	if err != nil {
		rel = path
	}
	rel = filepath.ToSlash(rel)

	source := Source{
		Path:        rel,
		Title:       sourceTitle(rel, content),
		SourceType:  sourceTypeRepositoryFile,
		RetrievedAt: retrievedAt,
		TrustScore:  repositoryTrustScore,
	}

	return sourceDocument{Source: source, Content: content, Summary: sourceSummary(source, content)}, nil
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

func sourceSummary(source Source, content string) string {
	if strings.TrimSpace(content) == "" {
		if source.URL != "" {
			return "External URL recorded as a source reference."
		}

		return "Source recorded without readable text content."
	}

	scanner := bufio.NewScanner(strings.NewReader(content))
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		line = strings.Trim(line, "#*` ")
		if line != "" {
			return truncateRunes(line, maxReportLineRunes)
		}
	}

	return "Readable text source."
}

func keywordsForQuestion(question string) []string {
	stop := map[string]bool{
		"about": true, "approach": true, "approaches": true, "best": true, "compare": true,
		"find": true, "from": true, "implementation": true, "into": true, "research": true,
		"safe": true, "that": true, "this": true, "with": true, "work": true,
	}

	fields := strings.FieldsFunc(strings.ToLower(question), func(r rune) bool {
		return !unicode.IsLetter(r) && !unicode.IsDigit(r)
	})

	seen := make(map[string]bool, len(fields))
	out := make([]string, 0, 12)
	for _, field := range fields {
		field = strings.TrimSpace(field)
		if len(field) < 4 || stop[field] || seen[field] {
			continue
		}

		seen[field] = true
		out = append(out, field)
		if len(out) >= 12 {
			break
		}
	}

	return out
}

func relevantLines(content string, keywords []string, limit int) []string {
	if strings.TrimSpace(content) == "" || len(keywords) == 0 || limit <= 0 {
		return nil
	}

	var lines []string
	scanner := bufio.NewScanner(strings.NewReader(content))
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" {
			continue
		}

		lower := strings.ToLower(line)
		for _, keyword := range keywords {
			if strings.Contains(lower, keyword) {
				lines = append(lines, truncateRunes(line, maxReportLineRunes))
				break
			}
		}

		if len(lines) >= limit {
			break
		}
	}

	return lines
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

func guidancePaths(guidance []guidanceFile) []string {
	out := make([]string, 0, len(guidance))
	for _, file := range guidance {
		out = append(out, file.Path)
	}

	return out
}

func buildClaims(question string, docs []sourceDocument, trustedSources []string, record runRecord) []Claim {
	claims := []Claim{
		{
			Claim:      fmt.Sprintf("This research run created auditable artifacts for %q.", question),
			Evidence:   []Evidence{{Kind: "artifact", Path: runFile}},
			Confidence: "high",
		},
		{
			Claim:      "Evidence is encouraged for important claims, but the research workflow does not require evidence for every statement.",
			Evidence:   []Evidence{{Kind: "artifact", Path: runFile, Excerpt: EvidenceBestPractice}},
			Confidence: "high",
		},
	}

	guidanceEvidence := evidenceForSources(docs, func(doc *sourceDocument) bool {
		return doc.SourceType == sourceTypeProjectGuidance
	})
	if len(guidanceEvidence) > 0 {
		claims = append(claims, Claim{
			Claim:      fmt.Sprintf("Project guidance was loaded before recommendations were written (%d file(s)).", len(guidanceEvidence)),
			Evidence:   guidanceEvidence,
			Confidence: "high",
		})
	}

	if len(trustedSources) > 0 {
		claims = append(claims, Claim{
			Claim:      "Trusted-source preferences were recorded for source selection and follow-up verification.",
			Evidence:   []Evidence{{Kind: "artifact", Path: runFile, Excerpt: strings.Join(trustedSources, ", ")}},
			Confidence: "high",
		})
	}

	for i := range docs {
		doc := &docs[i]
		if doc.SourceType == sourceTypeProjectGuidance {
			continue
		}

		evidence := evidenceForSource(doc)
		if strings.TrimSpace(doc.Summary) != "" {
			evidence.Excerpt = doc.Summary
		}

		claims = append(claims, Claim{
			Claim:      fmt.Sprintf("Source %s (%s) was included in the research context.", doc.ID, doc.Title),
			Evidence:   []Evidence{evidence},
			Confidence: "medium",
		})
	}

	claims = append(claims, Claim{
		Claim:      "The MVP did not perform autonomous web search; conclusions are limited to project guidance and user-supplied/local sources.",
		Evidence:   []Evidence{{Kind: "artifact", Path: runFile, Excerpt: strings.Join(record.Notes, " ")}},
		Confidence: "high",
	})

	return claims
}

func evidenceForSources(docs []sourceDocument, keep func(*sourceDocument) bool) []Evidence {
	var out []Evidence
	for i := range docs {
		doc := &docs[i]
		if !keep(doc) {
			continue
		}

		evidence := evidenceForSource(doc)
		if strings.TrimSpace(doc.Summary) != "" {
			evidence.Excerpt = doc.Summary
		}
		out = append(out, evidence)
	}

	return out
}

func evidenceForSource(doc *sourceDocument) Evidence {
	if doc == nil {
		return Evidence{Kind: "source"}
	}

	evidence := Evidence{Source: doc.ID}
	if doc.URL != "" {
		evidence.Kind = "url"
		evidence.URL = doc.URL
		return evidence
	}

	evidence.Kind = "file"
	evidence.Path = doc.Path

	return evidence
}

func writeSourcesJSONL(path string, docs []sourceDocument) error {
	file, err := os.OpenFile(path, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o600)
	if err != nil {
		return fmt.Errorf("research: create sources %s: %w", path, err)
	}
	defer file.Close()

	encoder := json.NewEncoder(file)
	for i := range docs {
		if err := encoder.Encode(docs[i].Source); err != nil {
			return fmt.Errorf("research: encode source %s: %w", docs[i].ID, err)
		}
	}

	return nil
}

func writeClaimsJSONL(path string, claims []Claim) error {
	file, err := os.OpenFile(path, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o600)
	if err != nil {
		return fmt.Errorf("research: create claims %s: %w", path, err)
	}
	defer file.Close()

	encoder := json.NewEncoder(file)
	for _, claim := range claims {
		if err := encoder.Encode(claim); err != nil {
			return fmt.Errorf("research: encode claim: %w", err)
		}
	}

	return nil
}

func writeTextFile(path, content string) error {
	if err := os.WriteFile(path, []byte(strings.TrimRight(content, "\n")+"\n"), 0o600); err != nil {
		return fmt.Errorf("research: write %s: %w", path, err)
	}

	return nil
}

func writeRunJSON(path string, record runRecord) error {
	data, err := json.MarshalIndent(record, "", "  ")
	if err != nil {
		return fmt.Errorf("research: marshal run metadata: %w", err)
	}

	data = append(data, '\n')
	if err := os.WriteFile(path, data, 0o600); err != nil {
		return fmt.Errorf("research: write %s: %w", path, err)
	}

	return nil
}

func renderReport(question string, docs []sourceDocument, claims []Claim, trustedSources []string, record runRecord) string {
	var b strings.Builder

	fmt.Fprintf(&b, "# Research: %s\n\n", question)
	fmt.Fprintf(&b, "Run ID: `%s`  \n", record.RunID)
	fmt.Fprintf(&b, "Created: `%s`\n\n", record.CreatedAt.Format(time.RFC3339))

	fmt.Fprintln(&b, "## Summary")
	fmt.Fprintf(&b, "Atteler created a local-first research packet for the question above. It inspected %d source record(s), including %d project guidance file(s), and wrote structured sources, claims, and run metadata for audit.\n\n", len(docs), countDocsByType(docs, sourceTypeProjectGuidance))
	fmt.Fprintln(&b, EvidenceBestPractice)
	fmt.Fprintln(&b)

	fmt.Fprintln(&b, "## Key findings")
	if countDocsByType(docs, sourceTypeProjectGuidance) > 0 {
		fmt.Fprintf(&b, "- Project/harness guidance was discovered and should constrain downstream recommendations: %s.\n", citationList(docs, func(doc *sourceDocument) bool { return doc.SourceType == sourceTypeProjectGuidance }))
	} else {
		fmt.Fprintln(&b, "- No project-specific harness guidance files were found under the repository root.")
	}

	userSources := countDocsExceptType(docs, sourceTypeProjectGuidance)
	if userSources > 0 {
		fmt.Fprintf(&b, "- User-supplied/local research sources were recorded for follow-up reasoning: %s.\n", citationList(docs, func(doc *sourceDocument) bool { return doc.SourceType != sourceTypeProjectGuidance }))
	} else {
		fmt.Fprintln(&b, "- No explicit research sources were supplied; this run is primarily a scaffold plus discovered project guidance.")
	}

	if len(trustedSources) > 0 {
		fmt.Fprintf(&b, "- Trusted-source preferences are recorded for future source gathering: `%s`.\n", strings.Join(trustedSources, "`, `"))
	}

	fmt.Fprintln(&b, "- Autonomous web search is intentionally out of scope for this MVP; add source URLs/files with `--research-source` or rerun later when a search provider is configured.")
	fmt.Fprintln(&b)

	fmt.Fprintln(&b, "## Source notes")
	if len(docs) == 0 {
		fmt.Fprintln(&b, "No sources were available.")
	} else {
		for i := range docs {
			doc := &docs[i]
			fmt.Fprintf(&b, "- [%s] **%s** (`%s`): %s\n", doc.ID, doc.Title, doc.SourceType, doc.Summary)
			for _, line := range doc.RelevantLines {
				fmt.Fprintf(&b, "  - Relevant excerpt: %s\n", line)
			}
		}
	}
	fmt.Fprintln(&b)

	fmt.Fprintln(&b, "## Tradeoffs")
	fmt.Fprintln(&b, "- Local-first artifacts are reproducible and safe to review, but they may miss current external documentation unless the user supplies URLs or files.")
	fmt.Fprintln(&b, "- Recording source and claim JSONL makes later implementation work auditable, but this MVP does not force every statement to carry a citation.")
	fmt.Fprintln(&b, "- Deferring full web search avoids provider coupling in the CLI surface while preserving an artifact contract for future Perplexity/OpenAI-style retrieval providers.")
	fmt.Fprintln(&b)

	fmt.Fprintln(&b, "## Recommended approach")
	fmt.Fprintln(&b, "1. Treat this report as the initial evidence ledger for the topic.")
	fmt.Fprintln(&b, "2. Add or refresh source files/URLs for any claim that will influence implementation, architecture, dependency, security, or operational decisions.")
	fmt.Fprintln(&b, "3. Convert validated findings into Atteler tasks with `--generate-tasks` when follow-up implementation work is ready.")
	fmt.Fprintln(&b)

	fmt.Fprintln(&b, "## Alternatives considered")
	fmt.Fprintln(&b, "- Brainstorm-only research: faster, but harder to audit and less reliable for technical decisions.")
	fmt.Fprintln(&b, "- Fully autonomous web research in the first implementation: more complete, but outside the MVP and needs provider/search policy boundaries.")
	fmt.Fprintln(&b)

	fmt.Fprintln(&b, "## Risks/open questions")
	fmt.Fprintln(&b, "- External documentation can change; rerun with fresh source URLs or provider-backed search when current facts matter.")
	fmt.Fprintln(&b, "- URL sources in this MVP are recorded for audit/follow-up instead of fetched and summarized automatically.")
	fmt.Fprintln(&b, "- Generated tasks are starting points and still need human/project-specific prioritization.")
	fmt.Fprintln(&b)

	fmt.Fprintln(&b, "## Important claims")
	for i, claim := range claims {
		fmt.Fprintf(&b, "%d. %s", i+1, claim.Claim)
		if len(claim.Evidence) > 0 {
			fmt.Fprintf(&b, " Evidence: %s.", evidenceSummary(claim.Evidence))
		}
		fmt.Fprintf(&b, " Confidence: `%s`.\n", claim.Confidence)
	}
	fmt.Fprintln(&b)

	fmt.Fprintln(&b, "## Citations")
	if len(docs) == 0 {
		fmt.Fprintln(&b, "No citations recorded.")
	} else {
		for i := range docs {
			fmt.Fprintf(&b, "- [%s] %s — %s\n", docs[i].ID, docs[i].Title, citationTarget(docs[i].Source))
		}
	}

	return b.String()
}

func countDocsByType(docs []sourceDocument, sourceType string) int {
	count := 0
	for i := range docs {
		if docs[i].SourceType == sourceType {
			count++
		}
	}

	return count
}

func countDocsExceptType(docs []sourceDocument, sourceType string) int {
	count := 0
	for i := range docs {
		if docs[i].SourceType != sourceType {
			count++
		}
	}

	return count
}

func citationList(docs []sourceDocument, keep func(*sourceDocument) bool) string {
	var ids []string
	for i := range docs {
		if keep(&docs[i]) {
			ids = append(ids, "["+docs[i].ID+"]")
		}
	}

	if len(ids) == 0 {
		return "none"
	}

	return strings.Join(ids, ", ")
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
		default:
			parts = append(parts, item.Kind)
		}
	}

	return strings.Join(parts, ", ")
}

func citationTarget(source Source) string {
	if source.URL != "" {
		return source.URL
	}

	return source.Path
}

func renderGeneratedTasks(question, runID string) string {
	var b strings.Builder
	fmt.Fprintln(&b, "tasks:")
	fmt.Fprintln(&b, "  - title: \"Review research findings for implementation follow-up\"")
	fmt.Fprintf(&b, "    rationale: \"Research run %s captured sources and claims for: %s\"\n", escapeYAMLDoubleQuoted(runID), escapeYAMLDoubleQuoted(question))
	fmt.Fprintln(&b, "    suggested_validation:")
	fmt.Fprintln(&b, "      - \"Review research.md citations and claims.jsonl evidence mappings\"")
	fmt.Fprintln(&b, "      - \"Add fresh source URLs or local evidence before implementing high-impact recommendations\"")

	return b.String()
}

func escapeYAMLDoubleQuoted(value string) string {
	value = strings.ReplaceAll(value, "\\", "\\\\")
	value = strings.ReplaceAll(value, "\"", "\\\"")

	return value
}

func sourcesFromDocuments(docs []sourceDocument) []Source {
	out := make([]Source, len(docs))
	for i := range docs {
		out[i] = docs[i].Source
	}

	return out
}
