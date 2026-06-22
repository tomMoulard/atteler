// Package reviewfix turns review findings into deterministic repair plans and
// auditable local artifacts for the `atteler review fix` workflow.
//
//nolint:wsl_v5 // Review-fix planning/reporting keeps adjacent artifact-building steps grouped for readability.
package reviewfix

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"
)

const (
	// SchemaVersion is the JSON artifact schema version for review-fix runs.
	SchemaVersion = 1

	defaultGuidanceReadLimit = 128 * 1024
	shortHashBytes           = 8
)

// Finding is Atteler's normalized review-fix finding model. It intentionally
// accepts fields commonly emitted by Atteler review reports, CodeRabbit-style
// exports, static analysis JSON, and simple custom finding arrays.
//
//nolint:govet // Field order follows user-facing finding readability.
type Finding struct {
	ID                    string `json:"id"`
	Severity              string `json:"severity"`
	Category              string `json:"category,omitempty"`
	File                  string `json:"file,omitempty"`
	Line                  int    `json:"line,omitempty"`
	EndLine               int    `json:"end_line,omitempty"`
	Message               string `json:"message"`
	Source                string `json:"source,omitempty"`
	SuggestedFix          string `json:"suggested_fix,omitempty"`
	SuggestedVerification string `json:"suggested_verification,omitempty"`
	Evidence              string `json:"evidence,omitempty"`
}

// GuidanceFile records one harness/project instruction file consulted before a
// fix prompt is generated.
type GuidanceFile struct {
	Path      string `json:"path"`
	Content   string `json:"content,omitempty"`
	SizeBytes int64  `json:"size_bytes"`
	Truncated bool   `json:"truncated,omitempty"`
}

// Group clusters related findings by file and root cause.
type Group struct {
	Key       string    `json:"key"`
	File      string    `json:"file,omitempty"`
	RootCause string    `json:"root_cause"`
	Findings  []Finding `json:"findings"`
}

// Plan is the deterministic repair plan emitted before local changes are
// attempted.
type Plan struct {
	GeneratedAt        time.Time      `json:"generated_at"`
	Source             string         `json:"source,omitempty"`
	ValidationCommands []string       `json:"validation_commands,omitempty"`
	Guidance           []GuidanceFile `json:"guidance,omitempty"`
	Groups             []Group        `json:"groups"`
	Findings           []Finding      `json:"findings"`
	Worktree           bool           `json:"worktree,omitempty"`
}

// ArtifactPaths contains every file written for a review-fix run.
type ArtifactPaths struct {
	RunDir        string `json:"run_dir"`
	FindingsInput string `json:"findings_input"`
	FixPlan       string `json:"fix_plan"`
	Changes       string `json:"changes"`
	ValidationLog string `json:"validation_log"`
	PatchDiff     string `json:"patch_diff"`
	RunJSON       string `json:"run_json"`
}

// ValidationResult records one local validation command outcome.
//
//nolint:govet // Field order mirrors process execution lifecycle.
type ValidationResult struct {
	StartedAt    time.Time     `json:"started_at,omitzero"`
	Duration     time.Duration `json:"duration,omitzero"`
	Command      string        `json:"command,omitempty"`
	Status       string        `json:"status"`
	Stdout       string        `json:"stdout,omitempty"`
	Stderr       string        `json:"stderr,omitempty"`
	Error        string        `json:"error,omitempty"`
	NotRunReason string        `json:"not_run_reason,omitempty"`
}

// ChangedFile summarizes one file changed by the repair workflow.
type ChangedFile struct {
	Status string `json:"status"`
	Path   string `json:"path"`
}

// RunRecord is persisted as run.json for auditability.
//
//nolint:govet // Field order follows artifact readability.
type RunRecord struct {
	StartedAt          time.Time          `json:"started_at"`
	CompletedAt        time.Time          `json:"completed_at"`
	Artifacts          ArtifactPaths      `json:"artifacts"`
	Source             string             `json:"source,omitempty"`
	ApplyMode          string             `json:"apply_mode,omitempty"`
	ApplyError         string             `json:"apply_error,omitempty"`
	PatchBytes         int                `json:"patch_bytes"`
	FindingCount       int                `json:"finding_count"`
	GroupCount         int                `json:"group_count"`
	SchemaVersion      int                `json:"schema_version"`
	Findings           []Finding          `json:"findings,omitempty"`
	Groups             []GroupSummary     `json:"groups"`
	Guidance           []GuidanceSummary  `json:"guidance,omitempty"`
	ChangedFiles       []ChangedFile      `json:"changed_files,omitempty"`
	Validation         []ValidationResult `json:"validation,omitempty"`
	RemotePublishing   bool               `json:"remote_publishing"`
	NoRemotePublishing bool               `json:"no_remote_publishing"`
	Worktree           bool               `json:"worktree,omitempty"`
}

// GroupSummary is the compact run.json group entry.
type GroupSummary struct {
	Key          string `json:"key"`
	File         string `json:"file,omitempty"`
	RootCause    string `json:"root_cause"`
	FindingCount int    `json:"finding_count"`
}

// GuidanceSummary is the compact run.json guidance entry.
type GuidanceSummary struct {
	Path      string `json:"path"`
	SizeBytes int64  `json:"size_bytes"`
	Truncated bool   `json:"truncated,omitempty"`
}

// LoadFindingsFile reads a JSON finding artifact and returns both the exact raw
// input bytes and normalized findings.
func LoadFindingsFile(ctx context.Context, filename string) ([]byte, []Finding, error) {
	if err := requireContext(ctx); err != nil {
		return nil, nil, err
	}

	filename = strings.TrimSpace(filename)
	if filename == "" {
		return nil, nil, errors.New("review fix: input path is required")
	}

	raw, err := os.ReadFile(filename)
	if err != nil {
		return nil, nil, fmt.Errorf("review fix: read findings %s: %w", filename, err)
	}

	findings, err := NormalizeFindings(raw)
	if err != nil {
		return raw, nil, err
	}

	return raw, findings, nil
}

// NormalizeFindings parses supported JSON shapes into the common finding model.
func NormalizeFindings(raw []byte) ([]Finding, error) {
	raw = bytes.TrimSpace(raw)
	if len(raw) == 0 {
		return nil, errors.New("review fix: findings JSON is empty")
	}

	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()

	var value any
	if err := decoder.Decode(&value); err != nil {
		return nil, fmt.Errorf("review fix: parse findings JSON: %w", err)
	}

	var extra any
	if err := decoder.Decode(&extra); err != io.EOF {
		if err == nil {
			return nil, errors.New("review fix: findings JSON must contain a single value")
		}

		return nil, fmt.Errorf("review fix: parse findings JSON: %w", err)
	}

	findings, err := collectFindings(value, "")
	if err != nil {
		return nil, err
	}
	if len(findings) == 0 {
		return nil, errors.New("review fix: no findings found in input")
	}

	findings = SortedFindings(findings)
	for i := range findings {
		if findings[i].ID == "" {
			findings[i].ID = generatedFindingID(findings[i], i)
		}
	}

	return findings, nil
}

func collectFindings(value any, parentSource string) ([]Finding, error) {
	switch typed := value.(type) {
	case []any:
		return collectFindingArray(typed, parentSource)
	case map[string]any:
		return collectFindingObject(typed, parentSource)
	default:
		return nil, errors.New("review fix: findings JSON must be an object or array")
	}
}

func collectFindingArray(values []any, parentSource string) ([]Finding, error) {
	findings := make([]Finding, 0, len(values))
	for i, value := range values {
		object, ok := value.(map[string]any)
		if !ok {
			return nil, fmt.Errorf("review fix: finding %d must be an object", i)
		}

		nested, ok, err := nestedFindingCollection(object, parentSource)
		if err != nil {
			return nil, err
		}
		if ok {
			findings = append(findings, nested...)
			continue
		}

		finding, err := parseFindingObject(object, parentSource, i)
		if err != nil {
			return nil, fmt.Errorf("review fix: finding %d: %w", i, err)
		}

		findings = append(findings, finding)
	}

	return findings, nil
}

func collectFindingObject(object map[string]any, parentSource string) ([]Finding, error) {
	nested, ok, err := nestedFindingCollection(object, parentSource)
	if err != nil {
		return nil, err
	}
	if ok {
		return nested, nil
	}

	finding, err := parseFindingObject(object, parentSource, 0)
	if err != nil {
		return nil, fmt.Errorf("review fix: finding 0: %w", err)
	}

	return []Finding{finding}, nil
}

func nestedFindingCollection(object map[string]any, parentSource string) ([]Finding, bool, error) {
	source := firstString(stringField(object, "source", "Source", "reviewer", "Reviewer", "tool", "Tool"), parentSource)

	if findingsValue, ok := fieldValue(object, "findings", "Findings", "results", "Results", "comments", "Comments"); ok {
		findings, err := collectFindings(findingsValue, source)
		if err != nil {
			return nil, true, err
		}

		return findings, true, nil
	}

	var all []Finding
	for _, key := range []string{"verdict", "Verdict", "report", "Report"} {
		if nested, ok := object[key]; ok {
			findings, err := collectFindings(nested, source)
			if err != nil {
				return nil, true, err
			}

			all = append(all, findings...)
		}
	}

	for _, key := range []string{"reports", "Reports"} {
		if nested, ok := object[key]; ok {
			findings, err := collectFindings(nested, source)
			if err != nil {
				return nil, true, err
			}

			all = append(all, findings...)
		}
	}

	if len(all) > 0 {
		return all, true, nil
	}

	return nil, false, nil
}

func parseFindingObject(object map[string]any, parentSource string, index int) (Finding, error) {
	message := stringField(object, "message", "Message", "body", "Body", "title", "Title", "description", "Description")
	if strings.TrimSpace(message) == "" {
		return Finding{}, errors.New("message is required")
	}

	finding := Finding{
		ID:                    stringField(object, "id", "ID", "finding_id", "findingId", "rule_id", "ruleID", "RuleID"),
		Severity:              normalizeSeverity(stringField(object, "severity", "Severity", "level", "Level", "priority", "Priority")),
		Category:              normalizeRootCauseLabel(stringField(object, "category", "Category", "kind", "Kind", "type", "Type")),
		File:                  cleanFindingPath(stringField(object, "file", "File", "path", "Path", "filename", "Filename", "filepath", "Filepath")),
		Line:                  intField(object, "line", "Line", "line_start", "lineStart", "LineStart", "start_line", "startLine", "lineno", "Lineno"),
		EndLine:               intField(object, "end_line", "endLine", "EndLine", "line_end", "lineEnd", "LineEnd"),
		Message:               strings.TrimSpace(message),
		Source:                firstString(stringField(object, "source", "Source", "reviewer", "Reviewer", "tool", "Tool"), parentSource),
		SuggestedFix:          stringField(object, "suggested_fix", "suggestedFix", "SuggestedFix", "Suggestion", "suggestion", "fix", "Fix", "patch", "Patch", "diff", "Diff"),
		SuggestedVerification: stringField(object, "suggested_verification", "suggestedVerification", "SuggestedVerification", "verification", "Verification"),
		Evidence:              stringField(object, "evidence", "Evidence"),
	}

	if finding.EndLine == 0 && finding.Line > 0 {
		finding.EndLine = finding.Line
	}
	if finding.ID == "" {
		finding.ID = generatedFindingID(finding, index)
	}

	return finding, nil
}

func fieldValue(object map[string]any, keys ...string) (any, bool) {
	for _, key := range keys {
		if value, ok := object[key]; ok {
			return value, true
		}
	}

	lower := make(map[string]any, len(object))
	for key, value := range object {
		lower[strings.ToLower(key)] = value
	}
	for _, key := range keys {
		if value, ok := lower[strings.ToLower(key)]; ok {
			return value, true
		}
	}

	return nil, false
}

func stringField(object map[string]any, keys ...string) string {
	value, ok := fieldValue(object, keys...)
	if !ok || value == nil {
		return ""
	}

	switch typed := value.(type) {
	case string:
		return strings.TrimSpace(typed)
	case json.Number:
		return typed.String()
	case float64:
		return strconv.FormatFloat(typed, 'f', -1, 64)
	case bool:
		return strconv.FormatBool(typed)
	default:
		return ""
	}
}

func intField(object map[string]any, keys ...string) int {
	value, ok := fieldValue(object, keys...)
	if !ok || value == nil {
		return 0
	}

	switch typed := value.(type) {
	case json.Number:
		return parseIntOrZero(typed.String())
	case float64:
		return int(typed)
	case string:
		return parseIntOrZero(typed)
	default:
		return 0
	}
}

func parseIntOrZero(value string) int {
	parsed, err := strconv.Atoi(strings.TrimSpace(value))
	if err == nil {
		return parsed
	}

	asFloat, err := strconv.ParseFloat(strings.TrimSpace(value), 64)
	if err != nil {
		return 0
	}

	return int(asFloat)
}

func normalizeSeverity(raw string) string {
	switch strings.ToLower(strings.TrimSpace(raw)) {
	case "critical", "blocker":
		return "critical"
	case "high", "important", "major", "error":
		return "high"
	case "medium", "warning", "warn", "moderate":
		return "medium"
	case "low", "minor", "maintenance":
		return "low"
	case "info", "informational", "notice", "":
		return "info"
	default:
		return strings.ToLower(strings.TrimSpace(raw))
	}
}

func cleanFindingPath(value string) string {
	value = strings.TrimSpace(strings.ReplaceAll(value, "\\", "/"))
	if value == "" {
		return ""
	}

	cleaned := path.Clean(value)
	if cleaned == "." {
		return value
	}

	return strings.TrimPrefix(cleaned, "./")
}

func generatedFindingID(finding Finding, index int) string {
	text := strings.Join([]string{
		finding.Source,
		finding.Severity,
		finding.Category,
		finding.File,
		strconv.Itoa(finding.Line),
		finding.Message,
	}, "\x00")
	sum := sha256.Sum256([]byte(text))

	return fmt.Sprintf("finding-%03d-%s", index+1, hex.EncodeToString(sum[:])[:shortHashBytes])
}

// SortedFindings returns a deterministic copy ordered by file, line, severity,
// source, and ID.
func SortedFindings(findings []Finding) []Finding {
	out := append([]Finding(nil), findings...)
	sort.SliceStable(out, func(i, j int) bool {
		left, right := out[i], out[j]
		for _, cmp := range []int{
			strings.Compare(left.File, right.File),
			left.Line - right.Line,
			strings.Compare(left.Severity, right.Severity),
			strings.Compare(left.Source, right.Source),
			strings.Compare(left.ID, right.ID),
			strings.Compare(left.Message, right.Message),
		} {
			if cmp != 0 {
				return cmp < 0
			}
		}

		return false
	})

	return out
}

// DiscoverGuidance reads harness-specific instruction files under root.
func DiscoverGuidance(ctx context.Context, root string) ([]GuidanceFile, error) {
	return DiscoverGuidanceForFindings(ctx, root, nil)
}

// DiscoverGuidanceForFindings reads root harness instructions plus nested
// instructions in directories that govern the normalized finding paths.
func DiscoverGuidanceForFindings(ctx context.Context, root string, findings []Finding) ([]GuidanceFile, error) {
	if err := requireContext(ctx); err != nil {
		return nil, err
	}

	root = strings.TrimSpace(root)
	if root == "" {
		return nil, errors.New("review fix: guidance root is required")
	}

	paths, err := guidancePaths(ctx, root, findingPaths(findings))
	if err != nil {
		return nil, err
	}

	files := make([]GuidanceFile, 0, len(paths))
	for _, filename := range paths {
		if err := requireContext(ctx); err != nil {
			return nil, err
		}

		file, err := readGuidanceFile(root, filename, defaultGuidanceReadLimit)
		if err != nil {
			return nil, err
		}

		files = append(files, file)
	}

	return files, nil
}

func guidancePaths(ctx context.Context, root string, findingPaths []string) ([]string, error) {
	collector := newGuidancePathCollector()
	addRootGuidancePaths(collector, root)
	addNestedGuidancePaths(collector, root, findingPaths)

	if err := addCursorGuidancePaths(ctx, collector, root); err != nil {
		return nil, err
	}

	return collector.sorted(), nil
}

type guidancePathCollector struct {
	seen  map[string]bool
	paths []string
}

func newGuidancePathCollector() *guidancePathCollector {
	return &guidancePathCollector{seen: make(map[string]bool)}
}

func (c *guidancePathCollector) add(filename string) {
	if strings.TrimSpace(filename) == "" || c.seen[filename] {
		return
	}

	info, err := os.Stat(filename)
	if err != nil || info.IsDir() {
		return
	}

	c.seen[filename] = true
	c.paths = append(c.paths, filename)
}

func (c *guidancePathCollector) sorted() []string {
	sort.Strings(c.paths)

	return c.paths
}

func addRootGuidancePaths(collector *guidancePathCollector, root string) {
	for _, rel := range []string{
		"AGENTS.md",
		"CLAUDE.md",
		"GEMINI.md",
		".cursorrules",
		".windsurfrules",
		".github/copilot-instructions.md",
		".codex/instructions.md",
	} {
		collector.add(filepath.Join(root, rel))
	}
}

func addNestedGuidancePaths(collector *guidancePathCollector, root string, findingPaths []string) {
	for _, rel := range nestedGuidancePaths(root, findingPaths) {
		collector.add(filepath.Join(root, rel))
	}
}

func addCursorGuidancePaths(ctx context.Context, collector *guidancePathCollector, root string) error {
	cursorRules := filepath.Join(root, ".cursor", "rules")
	if info, err := os.Stat(cursorRules); err == nil && info.IsDir() {
		walkErr := filepath.WalkDir(cursorRules, func(filename string, entry fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if ctxErr := ctx.Err(); ctxErr != nil {
				return fmt.Errorf("review fix: context done while scanning cursor rules: %w", ctxErr)
			}
			if entry.IsDir() {
				return nil
			}

			collector.add(filename)

			return nil
		})
		if walkErr != nil {
			return fmt.Errorf("review fix: scan cursor rules: %w", walkErr)
		}
	}

	return nil
}

func findingPaths(findings []Finding) []string {
	paths := make([]string, 0, len(findings))
	for i := range findings {
		if strings.TrimSpace(findings[i].File) != "" {
			paths = append(paths, findings[i].File)
		}
	}

	return paths
}

func nestedGuidancePaths(root string, paths []string) []string {
	seen := map[string]bool{}
	var out []string
	for _, pathValue := range paths {
		rel, ok := safeRelativeFindingPath(root, pathValue)
		if !ok {
			continue
		}

		dir := filepath.Dir(rel)
		for _, candidateDir := range ancestorDirs(dir) {
			if candidateDir == "." {
				continue
			}
			for _, name := range []string{"AGENTS.md", "CLAUDE.md", "GEMINI.md"} {
				candidate := filepath.Join(candidateDir, name)
				if !seen[candidate] {
					seen[candidate] = true
					out = append(out, candidate)
				}
			}
		}
	}

	sort.Strings(out)

	return out
}

func safeRelativeFindingPath(root, pathValue string) (string, bool) {
	pathValue = strings.TrimSpace(filepath.FromSlash(pathValue))
	if pathValue == "" {
		return "", false
	}
	if filepath.IsAbs(pathValue) {
		rel, err := filepath.Rel(root, pathValue)
		if err != nil {
			return "", false
		}

		pathValue = rel
	}

	rel := filepath.Clean(pathValue)
	if rel == "." || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return "", false
	}

	return rel, true
}

func ancestorDirs(dir string) []string {
	dir = filepath.Clean(dir)
	if dir == "." || dir == string(filepath.Separator) {
		return []string{"."}
	}

	parts := strings.Split(filepath.ToSlash(dir), "/")
	out := []string{"."}
	current := ""
	for _, part := range parts {
		if part == "" || part == "." {
			continue
		}
		current = filepath.Join(current, part)
		out = append(out, current)
	}

	return out
}

func readGuidanceFile(root, filename string, limit int64) (GuidanceFile, error) {
	info, err := os.Stat(filename)
	if err != nil {
		return GuidanceFile{}, fmt.Errorf("review fix: inspect guidance %s: %w", filename, err)
	}

	file, err := os.Open(filename) // #nosec G304 -- filename is discovered under the requested workspace root.
	if err != nil {
		return GuidanceFile{}, fmt.Errorf("review fix: read guidance %s: %w", filename, err)
	}
	defer file.Close()

	reader := io.LimitReader(file, limit+1)
	data, err := io.ReadAll(reader)
	if err != nil {
		return GuidanceFile{}, fmt.Errorf("review fix: read guidance %s: %w", filename, err)
	}

	truncated := int64(len(data)) > limit
	if truncated {
		data = data[:limit]
	}

	rel, err := filepath.Rel(root, filename)
	if err != nil {
		rel = filename
	}

	return GuidanceFile{
		Path:      filepath.ToSlash(rel),
		Content:   string(data),
		SizeBytes: info.Size(),
		Truncated: truncated,
	}, nil
}

// BuildPlan creates a deterministic repair plan from normalized findings and
// already-loaded project guidance.
func BuildPlan(source string, findings []Finding, guidance []GuidanceFile, validationCommands []string, worktree bool, generatedAt time.Time) Plan {
	findings = SortedFindings(findings)
	return Plan{
		GeneratedAt:        generatedAt.UTC(),
		Source:             strings.TrimSpace(source),
		ValidationCommands: cleanStrings(validationCommands),
		Guidance:           cloneGuidance(guidance),
		Groups:             GroupFindings(findings),
		Findings:           findings,
		Worktree:           worktree,
	}
}

// GroupFindings clusters findings by file and inferred root cause.
func GroupFindings(findings []Finding) []Group {
	findings = SortedFindings(findings)
	byKey := make(map[string]*Group)
	for i := range findings {
		finding := findings[i]
		file := finding.File
		rootCause := inferRootCause(finding)
		key := groupKey(file, rootCause)
		group := byKey[key]
		if group == nil {
			group = &Group{Key: key, File: file, RootCause: rootCause}
			byKey[key] = group
		}

		group.Findings = append(group.Findings, finding)
	}

	groups := make([]Group, 0, len(byKey))
	for _, group := range byKey {
		group.Findings = SortedFindings(group.Findings)
		groups = append(groups, *group)
	}

	sort.SliceStable(groups, func(i, j int) bool {
		if groups[i].File != groups[j].File {
			return groups[i].File < groups[j].File
		}

		return groups[i].RootCause < groups[j].RootCause
	})

	return groups
}

func groupKey(file, rootCause string) string {
	if file == "" {
		file = "global"
	}

	return file + ":" + rootCause
}

func inferRootCause(finding Finding) string {
	if category := normalizeRootCauseLabel(finding.Category); category != "" {
		return category
	}

	text := strings.ToLower(finding.Message + " " + finding.SuggestedFix)
	for _, rule := range rootCauseKeywordRules {
		if containsAny(text, rule.keywords) {
			return rule.label
		}
	}

	return "general"
}

var rootCauseKeywordRules = []struct {
	label    string
	keywords []string
}{
	{label: "tests", keywords: []string{"test", "coverage"}},
	{label: "nil-safety", keywords: []string{"nil", "null", "panic"}},
	{label: "error-handling", keywords: []string{"error", "wrap", "return"}},
	{label: "security", keywords: []string{"secret", "token", "security"}},
	{label: "concurrency", keywords: []string{"race", "concurrent", "goroutine"}},
	{label: "performance", keywords: []string{"slow", "latency", "alloc", "performance"}},
	{label: "style", keywords: []string{"format", "lint", "style"}},
	{label: "documentation", keywords: []string{"doc", "readme"}},
}

func containsAny(text string, keywords []string) bool {
	for _, keyword := range keywords {
		if strings.Contains(text, keyword) {
			return true
		}
	}

	return false
}

func normalizeRootCauseLabel(value string) string {
	value = strings.ToLower(strings.TrimSpace(value))
	value = strings.ReplaceAll(value, "_", "-")
	value = strings.Join(strings.Fields(value), "-")

	return value
}

// RenderPlanMarkdown renders fix-plan.md.
func RenderPlanMarkdown(plan Plan) string {
	var b strings.Builder
	b.WriteString("# Review fix plan\n\n")
	if !plan.GeneratedAt.IsZero() {
		fmt.Fprintf(&b, "- Generated: %s\n", plan.GeneratedAt.Format(time.RFC3339))
	}
	if plan.Source != "" {
		fmt.Fprintf(&b, "- Source: `%s`\n", plan.Source)
	}
	fmt.Fprintf(&b, "- Findings: %d\n", len(plan.Findings))
	fmt.Fprintf(&b, "- Groups: %d\n", len(plan.Groups))
	fmt.Fprintf(&b, "- Worktree requested: %t\n", plan.Worktree)
	b.WriteString("- Remote publishing: disabled by default\n")

	b.WriteString("\n## Harness guidance consulted\n\n")
	if len(plan.Guidance) == 0 {
		b.WriteString("- none found\n")
	} else {
		for _, guidance := range plan.Guidance {
			truncated := ""
			if guidance.Truncated {
				truncated = ", truncated"
			}
			fmt.Fprintf(&b, "- `%s` (%d bytes%s)\n", guidance.Path, guidance.SizeBytes, truncated)
		}
	}

	b.WriteString("\n## Repair groups\n")
	for i, group := range plan.Groups {
		fmt.Fprintf(&b, "\n### %d. %s\n\n", i+1, groupTitle(group))
		fmt.Fprintf(&b, "- Root cause: `%s`\n", group.RootCause)
		fmt.Fprintf(&b, "- Findings: %d\n", len(group.Findings))
		for i := range group.Findings {
			finding := group.Findings[i]
			fmt.Fprintf(&b, "  - `%s` %s%s: %s\n", finding.ID, severityLabel(finding.Severity), locationSuffix(finding), finding.Message)
			if finding.SuggestedFix != "" {
				fmt.Fprintf(&b, "    - Suggested fix: %s\n", firstLine(finding.SuggestedFix))
			}
		}
	}

	b.WriteString("\n## Validation\n\n")
	if len(plan.ValidationCommands) == 0 {
		b.WriteString("- no validation command supplied\n")
	} else {
		for _, command := range plan.ValidationCommands {
			fmt.Fprintf(&b, "- `%s`\n", command)
		}
	}

	return b.String()
}

// BuildAgentPrompt builds the local-only repair prompt sent to the selected
// Atteler agent.
func BuildAgentPrompt(plan Plan) string {
	var b strings.Builder
	b.WriteString("Convert the following review findings into a verified local patch.\n\n")
	b.WriteString("Safety rules:\n")
	b.WriteString("- Apply changes locally only. Do not push branches, open PRs, post comments, resolve GitHub conversations, or mutate remote services.\n")
	b.WriteString("- Respect all harness guidance included below before editing.\n")
	b.WriteString("- Group related comments by file/root cause and implement the smallest coherent fix.\n")
	b.WriteString("- Prefer tests when a finding concerns behavior or regression risk.\n")
	b.WriteString("- The Atteler review-fix command will write artifacts and run configured validation after you finish.\n")
	b.WriteString("- Stop after local edits and a concise summary of changed files and any remaining risks.\n\n")

	if len(plan.Guidance) > 0 {
		b.WriteString("Harness guidance files:\n")
		for _, guidance := range plan.Guidance {
			fmt.Fprintf(&b, "\n--- %s ---\n", guidance.Path)
			b.WriteString(guidance.Content)
			if !strings.HasSuffix(guidance.Content, "\n") {
				b.WriteByte('\n')
			}
			if guidance.Truncated {
				b.WriteString("[truncated]\n")
			}
		}
		b.WriteByte('\n')
	}

	b.WriteString(RenderPlanMarkdown(plan))
	b.WriteString("\nNormalized findings JSON:\n")
	data, err := json.MarshalIndent(plan.Findings, "", "  ")
	if err == nil {
		b.WriteString("\n```json\n")
		b.Write(data)
		b.WriteString("\n```\n")
	}

	return b.String()
}

// ArtifactPathsFor returns the standard review-fix artifact paths.
func ArtifactPathsFor(root, runID string) ArtifactPaths {
	runDir := filepath.Join(root, ".atteler", "runs", "review-fix", runID)

	return ArtifactPaths{
		RunDir:        runDir,
		FindingsInput: filepath.Join(runDir, "findings.input.json"),
		FixPlan:       filepath.Join(runDir, "fix-plan.md"),
		Changes:       filepath.Join(runDir, "changes.md"),
		ValidationLog: filepath.Join(runDir, "validation.log"),
		PatchDiff:     filepath.Join(runDir, "patch.diff"),
		RunJSON:       filepath.Join(runDir, "run.json"),
	}
}

// NewRunID returns a timestamped run ID with a short random suffix.
func NewRunID(now time.Time) string {
	if now.IsZero() {
		now = time.Now()
	}

	suffix := make([]byte, 4)
	if _, err := rand.Read(suffix); err != nil {
		return fmt.Sprintf("%s-%x", now.UTC().Format("20060102-150405"), now.UTC().UnixNano())
	}

	return fmt.Sprintf("%s-%s", now.UTC().Format("20060102-150405"), hex.EncodeToString(suffix))
}

// WriteInitialArtifacts writes findings.input.json and fix-plan.md before any
// local repair attempt starts.
func WriteInitialArtifacts(ctx context.Context, paths ArtifactPaths, rawInput []byte, plan Plan) error {
	if err := requireContext(ctx); err != nil {
		return err
	}
	if err := os.MkdirAll(paths.RunDir, 0o750); err != nil {
		return fmt.Errorf("review fix: create artifact directory: %w", err)
	}
	if err := writeFile(paths.FindingsInput, rawInput); err != nil {
		return err
	}
	if err := writeFile(paths.FixPlan, []byte(RenderPlanMarkdown(plan))); err != nil {
		return err
	}

	return nil
}

// WriteFinalArtifacts writes changes.md, validation.log, patch.diff, and
// run.json after the repair attempt and validation have completed.
func WriteFinalArtifacts(ctx context.Context, paths ArtifactPaths, record RunRecord, patch string) error {
	if err := requireContext(ctx); err != nil {
		return err
	}
	if err := os.MkdirAll(paths.RunDir, 0o750); err != nil {
		return fmt.Errorf("review fix: create artifact directory: %w", err)
	}
	if err := writeFile(paths.Changes, []byte(RenderChangesMarkdown(record))); err != nil {
		return err
	}
	if err := writeFile(paths.ValidationLog, []byte(RenderValidationLog(record.Validation))); err != nil {
		return err
	}
	if err := writeFile(paths.PatchDiff, []byte(patch)); err != nil {
		return err
	}

	record.Artifacts = paths
	record.PatchBytes = len([]byte(patch))
	data, err := json.MarshalIndent(record, "", "  ")
	if err != nil {
		return fmt.Errorf("review fix: marshal run.json: %w", err)
	}
	data = append(data, '\n')
	if err := writeFile(paths.RunJSON, data); err != nil {
		return err
	}

	return nil
}

func writeFile(filename string, data []byte) error {
	if err := os.MkdirAll(filepath.Dir(filename), 0o750); err != nil {
		return fmt.Errorf("review fix: create dir %s: %w", filepath.Dir(filename), err)
	}
	if err := os.WriteFile(filename, data, 0o600); err != nil {
		return fmt.Errorf("review fix: write %s: %w", filename, err)
	}

	return nil
}

// NewRunRecord builds the compact run.json representation for a completed run.
func NewRunRecord(startedAt, completedAt time.Time, paths ArtifactPaths, plan Plan, changedFiles []ChangedFile, validation []ValidationResult, applyMode, applyError, patch string) RunRecord {
	return RunRecord{
		StartedAt:          startedAt.UTC(),
		CompletedAt:        completedAt.UTC(),
		Artifacts:          paths,
		Source:             plan.Source,
		ApplyMode:          strings.TrimSpace(applyMode),
		ApplyError:         strings.TrimSpace(applyError),
		PatchBytes:         len([]byte(patch)),
		FindingCount:       len(plan.Findings),
		GroupCount:         len(plan.Groups),
		SchemaVersion:      SchemaVersion,
		Findings:           SortedFindings(plan.Findings),
		Groups:             summarizeGroups(plan.Groups),
		Guidance:           summarizeGuidance(plan.Guidance),
		ChangedFiles:       append([]ChangedFile(nil), changedFiles...),
		Validation:         append([]ValidationResult(nil), validation...),
		RemotePublishing:   false,
		NoRemotePublishing: true,
		Worktree:           plan.Worktree,
	}
}

func summarizeGroups(groups []Group) []GroupSummary {
	out := make([]GroupSummary, 0, len(groups))
	for _, group := range groups {
		out = append(out, GroupSummary{
			Key:          group.Key,
			File:         group.File,
			RootCause:    group.RootCause,
			FindingCount: len(group.Findings),
		})
	}

	return out
}

func summarizeGuidance(guidance []GuidanceFile) []GuidanceSummary {
	out := make([]GuidanceSummary, 0, len(guidance))
	for _, file := range guidance {
		out = append(out, GuidanceSummary{Path: file.Path, SizeBytes: file.SizeBytes, Truncated: file.Truncated})
	}

	return out
}

// RenderChangesMarkdown renders changes.md.
func RenderChangesMarkdown(record RunRecord) string {
	var b strings.Builder
	b.WriteString("# Review fix changes\n\n")
	fmt.Fprintf(&b, "- Findings: %d\n", record.FindingCount)
	fmt.Fprintf(&b, "- Groups: %d\n", record.GroupCount)
	if record.ApplyMode != "" {
		fmt.Fprintf(&b, "- Apply mode: `%s`\n", record.ApplyMode)
	}
	if record.ApplyError != "" {
		fmt.Fprintf(&b, "- Apply error: %s\n", record.ApplyError)
	}
	fmt.Fprintf(&b, "- Patch bytes: %d\n", record.PatchBytes)
	b.WriteString("- Remote publishing: not performed\n")

	b.WriteString("\n## Original findings\n\n")
	if len(record.Findings) == 0 {
		b.WriteString("- none recorded\n")
	} else {
		for i := range record.Findings {
			finding := record.Findings[i]
			fmt.Fprintf(&b, "- `%s` %s%s: %s\n", finding.ID, severityLabel(finding.Severity), locationSuffix(finding), finding.Message)
		}
	}

	b.WriteString("\n## Groups addressed\n\n")
	if len(record.Groups) == 0 {
		b.WriteString("- none\n")
	} else {
		for _, group := range record.Groups {
			fmt.Fprintf(&b, "- `%s` (%d findings)\n", group.Key, group.FindingCount)
		}
	}

	b.WriteString("\n## Changed files\n\n")
	if len(record.ChangedFiles) == 0 {
		b.WriteString("- none detected\n")
	} else {
		for _, file := range record.ChangedFiles {
			fmt.Fprintf(&b, "- `%s` %s\n", file.Status, file.Path)
		}
	}

	b.WriteString("\n## Validation\n\n")
	if len(record.Validation) == 0 {
		b.WriteString("- not run: no validation command supplied\n")
	} else {
		for i := range record.Validation {
			result := record.Validation[i]
			fmt.Fprintf(&b, "- `%s`: %s\n", result.Command, result.Status)
		}
	}

	b.WriteString("\n## Remaining known issues\n\n")
	switch {
	case record.ApplyError != "":
		fmt.Fprintf(&b, "- apply/reporting issue: %s\n", record.ApplyError)
	case validationFailed(record.Validation):
		b.WriteString("- validation failed; inspect validation.log for command output\n")
	default:
		b.WriteString("- none recorded\n")
	}

	b.WriteString("\n## Artifacts\n\n")
	fmt.Fprintf(&b, "- Plan: `%s`\n", record.Artifacts.FixPlan)
	fmt.Fprintf(&b, "- Validation log: `%s`\n", record.Artifacts.ValidationLog)
	fmt.Fprintf(&b, "- Patch: `%s`\n", record.Artifacts.PatchDiff)
	fmt.Fprintf(&b, "- Run JSON: `%s`\n", record.Artifacts.RunJSON)

	return b.String()
}

func validationFailed(results []ValidationResult) bool {
	for i := range results {
		if results[i].Status == "failed" {
			return true
		}
	}

	return false
}

// RenderValidationLog renders validation.log.
func RenderValidationLog(results []ValidationResult) string {
	if len(results) == 0 {
		return "validation: not run (no validation command supplied)\n"
	}

	var b strings.Builder
	for i := range results {
		if i > 0 {
			b.WriteByte('\n')
		}

		renderValidationResultLog(&b, &results[i])
	}

	return b.String()
}

func renderValidationResultLog(b *strings.Builder, result *ValidationResult) {
	fmt.Fprintf(b, "$ %s\n", result.Command)
	fmt.Fprintf(b, "status: %s\n", result.Status)
	if !result.StartedAt.IsZero() {
		fmt.Fprintf(b, "started_at: %s\n", result.StartedAt.Format(time.RFC3339))
	}
	if result.Duration > 0 {
		fmt.Fprintf(b, "duration: %s\n", result.Duration)
	}
	if result.NotRunReason != "" {
		fmt.Fprintf(b, "not_run_reason: %s\n", result.NotRunReason)
	}

	writeValidationStream(b, "stdout", result.Stdout)
	writeValidationStream(b, "stderr", result.Stderr)
	if result.Error != "" {
		fmt.Fprintf(b, "error: %s\n", result.Error)
	}
}

func writeValidationStream(b *strings.Builder, name, value string) {
	if value == "" {
		return
	}

	fmt.Fprintf(b, "%s:\n", name)
	b.WriteString(value)
	if !strings.HasSuffix(value, "\n") {
		b.WriteByte('\n')
	}
}

// SuggestedUnifiedDiff combines patch-like suggested fixes when findings
// already contain machine-applyable unified diffs.
func SuggestedUnifiedDiff(findings []Finding) (string, bool) {
	var b strings.Builder
	count := 0
	for i := range findings {
		finding := findings[i]
		text := strings.TrimSpace(finding.SuggestedFix)
		if !LooksLikeUnifiedDiff(text) {
			continue
		}
		if count > 0 {
			b.WriteString("\n")
		}
		b.WriteString(text)
		if !strings.HasSuffix(text, "\n") {
			b.WriteByte('\n')
		}
		count++
	}

	return b.String(), count > 0
}

// LooksLikeUnifiedDiff reports whether text appears to be a unified diff.
func LooksLikeUnifiedDiff(text string) bool {
	text = strings.TrimSpace(text)
	if text == "" || !strings.Contains(text, "\n@@") {
		return false
	}

	return strings.Contains(text, "diff --git ") || (strings.Contains(text, "\n--- ") && strings.Contains(text, "\n+++ ")) || strings.HasPrefix(text, "--- ")
}

func groupTitle(group Group) string {
	file := group.File
	if file == "" {
		file = "global"
	}

	return file + " / " + group.RootCause
}

func severityLabel(severity string) string {
	severity = strings.TrimSpace(severity)
	if severity == "" {
		return "severity=info"
	}

	return "severity=" + severity
}

func locationSuffix(finding Finding) string {
	if finding.File == "" {
		return ""
	}
	if finding.Line <= 0 {
		return " at " + finding.File
	}
	if finding.EndLine > finding.Line {
		return fmt.Sprintf(" at %s:%d-%d", finding.File, finding.Line, finding.EndLine)
	}

	return fmt.Sprintf(" at %s:%d", finding.File, finding.Line)
}

func firstLine(value string) string {
	value = strings.TrimSpace(value)
	if before, _, ok := strings.Cut(value, "\n"); ok {
		return strings.TrimSpace(before)
	}

	return value
}

func firstString(values ...string) string {
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			return strings.TrimSpace(value)
		}
	}

	return ""
}

func cleanStrings(values []string) []string {
	out := make([]string, 0, len(values))
	for _, value := range values {
		value = strings.TrimSpace(value)
		if value != "" {
			out = append(out, value)
		}
	}

	return out
}

func cloneGuidance(guidance []GuidanceFile) []GuidanceFile {
	out := make([]GuidanceFile, len(guidance))
	copy(out, guidance)

	return out
}

func requireContext(ctx context.Context) error {
	if ctx == nil {
		return errors.New("review fix: context is required")
	}
	if err := ctx.Err(); err != nil {
		return fmt.Errorf("review fix: context already done: %w", err)
	}

	return nil
}
