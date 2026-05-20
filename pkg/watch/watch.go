// Package watch provides dependency-free repository scan primitives for
// background agent health checks.
package watch

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	pathmatch "path"
	"path/filepath"
	"sort"
	"strings"
	"unicode/utf8"
)

const (
	// DefaultLargeFileBytes is the default byte threshold for large-file findings.
	DefaultLargeFileBytes int64 = 1 << 20

	// KindLargeFile identifies files above the configured byte threshold.
	KindLargeFile = "large_file"
	// KindMissingTest identifies Go files without same-directory test companions.
	KindMissingTest = "missing_test"
	// KindStaleTODO identifies files containing TODO or FIXME markers.
	KindStaleTODO = "stale_todo"
	// KindConventionDrift identifies code that violates repository conventions.
	KindConventionDrift = "convention_drift"

	// SeverityInfo marks informational findings.
	SeverityInfo = "info"
	// SeverityWarning marks findings likely to need attention.
	SeverityWarning = "warning"
	// SeverityMaintenance marks maintenance debt findings.
	SeverityMaintenance = "maintenance"
)

// Finding describes a repository scan result.
type Finding struct {
	Path     string `json:"path"`
	Kind     string `json:"kind"`
	Message  string `json:"message"`
	Severity string `json:"severity"`
	RuleID   string `json:"rule_id,omitempty"`
	Help     string `json:"help,omitempty"`
}

// Options configures repository scans.
type Options struct {
	// LargeFileBytes is the byte threshold for large-file findings.
	// Values less than or equal to zero use DefaultLargeFileBytes.
	LargeFileBytes int64
}

// Scan scans root using default options.
func Scan(root string) ([]Finding, error) {
	return ScanWithOptions(root, Options{})
}

// ScanWithOptions scans root for stale TODO/FIXME markers, large files, Go
// files missing same-directory _test.go companions, and package-level
// convention drift.
func ScanWithOptions(root string, options Options) ([]Finding, error) {
	largeFileBytes := options.LargeFileBytes
	if largeFileBytes <= 0 {
		largeFileBytes = DefaultLargeFileBytes
	}

	state := scanState{
		root:           root,
		largeFileBytes: largeFileBytes,
		ignore:         loadGitIgnore(root),
		goFiles:        make(map[string]bool),
		testFiles:      make(map[string]bool),
	}
	if err := filepath.WalkDir(root, state.visit); err != nil {
		return nil, fmt.Errorf("scan %s: %w", root, err)
	}

	state.addMissingTests()
	sortFindings(state.findings)

	return state.findings, nil
}

//nolint:govet // Private scan state is ordered by lifecycle/readability.
type scanState struct {
	root           string
	largeFileBytes int64
	ignore         ignoreMatcher
	findings       []Finding
	goFiles        map[string]bool
	testFiles      map[string]bool
}

func (s *scanState) visit(filePath string, entry fs.DirEntry, err error) error {
	if err != nil {
		return fmt.Errorf("visit %s: %w", filePath, err)
	}

	if entry.IsDir() {
		if shouldSkipDir(entry.Name()) && filePath != s.root {
			return filepath.SkipDir
		}

		relativePath, relativeErr := relative(s.root, filePath)
		if relativeErr != nil {
			return relativeErr
		}

		if relativePath != "." && s.ignore.ignored(relativePath, true) {
			return filepath.SkipDir
		}

		return nil
	}

	return s.scanFile(filePath, entry)
}

func (s *scanState) scanFile(filePath string, entry fs.DirEntry) error {
	relativePath, err := relative(s.root, filePath)
	if err != nil {
		return err
	}

	if s.ignore.ignored(relativePath, false) {
		return nil
	}

	info, err := entry.Info()
	if err != nil {
		return fmt.Errorf("stat %s: %w", filePath, err)
	}

	if info.Size() > s.largeFileBytes {
		s.findings = append(s.findings, newFinding(
			relativePath,
			KindLargeFile,
			fmt.Sprintf("file is %d bytes, above %d byte threshold", info.Size(), s.largeFileBytes),
			SeverityWarning,
		))
	}

	recordGoFile(relativePath, s.goFiles, s.testFiles)

	if shouldScanStaleMarkers(relativePath) {
		ok, err := hasStaleTODOMarker(filePath)
		if err != nil {
			return err
		}

		if ok {
			s.findings = append(s.findings, newFinding(
				relativePath,
				KindStaleTODO,
				"contains stale TODO/FIXME marker",
				SeverityMaintenance,
			))
		}
	}

	if hasContextBackgroundDrift(filePath, relativePath) {
		s.findings = append(s.findings, newFinding(
			relativePath,
			KindConventionDrift,
			"uses context.Background() outside allowed entrypoints/tests",
			SeverityMaintenance,
		))
	}

	return nil
}

func newFinding(path, kind, message, severity string) Finding {
	return Finding{
		Path:     path,
		Kind:     kind,
		Message:  message,
		Severity: severity,
		RuleID:   "watch." + kind,
		Help:     findingHelp(kind),
	}
}

func findingHelp(kind string) string {
	switch kind {
	case KindLargeFile:
		return "Remove the large file, move it to release artifacts, or ignore generated/binary output through .gitignore."
	case KindMissingTest:
		return "Add a same-directory _test.go companion or move generated/adapter code behind an ignored path."
	case KindStaleTODO:
		return "Convert stale TODO/FIXME markers into tracked issues or remove completed notes."
	case KindConventionDrift:
		return "Propagate caller contexts; only the process entrypoint should create the root context."
	default:
		return ""
	}
}

func shouldScanStaleMarkers(path string) bool {
	return !strings.HasSuffix(path, "_test.go")
}

func loadGitIgnore(root string) ignoreMatcher {
	content, err := os.ReadFile(filepath.Join(root, ".gitignore"))
	if err != nil {
		return ignoreMatcher{}
	}

	var matcher ignoreMatcher

	for rawLine := range strings.SplitSeq(string(content), "\n") {
		rule, ok := parseIgnoreRule(rawLine)
		if ok {
			matcher.rules = append(matcher.rules, rule)
		}
	}

	return matcher
}

type ignoreMatcher struct {
	rules []ignoreRule
}

func (m ignoreMatcher) ignored(relativePath string, isDir bool) bool {
	relativePath = strings.TrimPrefix(filepath.ToSlash(relativePath), "./")
	if relativePath == "." || relativePath == "" {
		return false
	}

	ignored := false

	for _, rule := range m.rules {
		if rule.matches(relativePath, isDir) {
			ignored = !rule.negated
		}
	}

	return ignored
}

type ignoreRule struct {
	pattern       string
	negated       bool
	rooted        bool
	directoryOnly bool
}

func parseIgnoreRule(line string) (ignoreRule, bool) {
	line = strings.TrimSpace(line)
	if line == "" || strings.HasPrefix(line, "#") {
		return ignoreRule{}, false
	}

	rule := ignoreRule{}
	if strings.HasPrefix(line, "!") {
		rule.negated = true
		line = strings.TrimSpace(strings.TrimPrefix(line, "!"))
	}

	if line == "" {
		return ignoreRule{}, false
	}

	if strings.HasSuffix(line, "/") {
		rule.directoryOnly = true
		line = strings.TrimSuffix(line, "/")
	}

	if strings.HasPrefix(line, "/") {
		rule.rooted = true
		line = strings.TrimPrefix(line, "/")
	}

	line = filepath.ToSlash(strings.TrimPrefix(line, "./"))
	if line == "" {
		return ignoreRule{}, false
	}

	rule.pattern = line

	return rule, true
}

func (r ignoreRule) matches(relativePath string, isDir bool) bool {
	if r.directoryOnly && !isDir && !hasPathPrefix(relativePath, r.pattern) {
		return false
	}

	if r.rooted {
		return pathMatches(r.pattern, relativePath) || hasPathPrefix(relativePath, r.pattern)
	}

	if strings.Contains(r.pattern, "/") {
		return pathMatches(r.pattern, relativePath) || strings.HasSuffix(relativePath, "/"+r.pattern)
	}

	for segment := range strings.SplitSeq(relativePath, "/") {
		if pathMatches(r.pattern, segment) {
			return true
		}
	}

	return false
}

func pathMatches(pattern, value string) bool {
	ok, err := pathmatch.Match(pattern, value)
	if err == nil && ok {
		return true
	}

	return pattern == value
}

func hasPathPrefix(relativePath, prefix string) bool {
	return relativePath == prefix || strings.HasPrefix(relativePath, prefix+"/")
}

func (s *scanState) addMissingTests() {
	for path := range s.goFiles {
		if !s.testFiles[testPath(path)] {
			s.findings = append(s.findings, newFinding(
				path,
				KindMissingTest,
				"missing _test.go companion",
				SeverityInfo,
			))
		}
	}
}

func shouldSkipDir(name string) bool {
	switch name {
	case ".atteler", ".cache", ".codex", ".git", ".idea", ".omx", ".vscode", "build", "dist", "node_modules", "vendor":
		return true
	default:
		return false
	}
}

func relative(root, path string) (string, error) {
	relativePath, err := filepath.Rel(root, path)
	if err != nil {
		return "", fmt.Errorf("relative path %s from %s: %w", path, root, err)
	}

	return filepath.ToSlash(relativePath), nil
}

func recordGoFile(path string, goFiles, testFiles map[string]bool) {
	if !strings.HasSuffix(path, ".go") {
		return
	}

	if strings.HasSuffix(path, "_test.go") {
		testFiles[path] = true
		return
	}

	goFiles[path] = true
}

func testPath(path string) string {
	return strings.TrimSuffix(path, ".go") + "_test.go"
}

func hasStaleTODOMarker(path string) (bool, error) {
	content, err := os.ReadFile(path)
	if err != nil {
		return false, fmt.Errorf("read %s: %w", path, err)
	}

	if !utf8.Valid(content) {
		return false, nil
	}

	for line := range strings.SplitSeq(string(content), "\n") {
		if isStaleTODOLine(line) {
			return true, nil
		}
	}

	return false, nil
}

func isStaleTODOLine(line string) bool {
	line = strings.TrimSpace(line)
	line = strings.TrimPrefix(line, "//")
	line = strings.TrimPrefix(line, "#")
	line = strings.TrimPrefix(line, "--")
	line = strings.TrimPrefix(line, "/*")
	line = strings.TrimPrefix(line, "*")
	line = strings.TrimSpace(line)
	line = strings.TrimPrefix(line, "-")
	line = strings.TrimSpace(line)

	upper := strings.ToUpper(line)
	for _, marker := range []string{"TODO", "FIXME"} {
		if strings.HasPrefix(upper, marker+":") || strings.HasPrefix(upper, marker+"(") {
			return true
		}
	}

	return false
}

func hasContextBackgroundDrift(path, relativePath string) bool {
	if !isProductionGoFile(relativePath) {
		return false
	}

	file, err := parser.ParseFile(token.NewFileSet(), path, nil, 0)
	if err != nil {
		return false
	}

	if isAllowedContextBackgroundFile(relativePath, file) {
		return false
	}

	return usesContextBackground(file)
}

func isProductionGoFile(path string) bool {
	return strings.HasSuffix(path, ".go") && !strings.HasSuffix(path, "_test.go")
}

func isAllowedContextBackgroundFile(path string, file *ast.File) bool {
	return filepath.Base(path) == "main.go" && file.Name.Name == "main"
}

func usesContextBackground(file *ast.File) bool {
	contextNames, dotImported := contextImportNames(file)
	if len(contextNames) == 0 && !dotImported {
		return false
	}

	found := false

	ast.Inspect(file, func(node ast.Node) bool {
		if found {
			return false
		}

		call, ok := node.(*ast.CallExpr)
		if !ok {
			return true
		}

		switch fun := call.Fun.(type) {
		case *ast.SelectorExpr:
			if fun.Sel.Name != "Background" {
				return true
			}

			ident, ok := fun.X.(*ast.Ident)
			if ok && contextNames[ident.Name] {
				found = true
				return false
			}
		case *ast.Ident:
			if dotImported && fun.Name == "Background" {
				found = true
				return false
			}
		}

		return true
	})

	return found
}

func contextImportNames(file *ast.File) (map[string]bool, bool) {
	names := make(map[string]bool)
	dotImported := false

	for _, spec := range file.Imports {
		if strings.Trim(spec.Path.Value, "\"") != "context" {
			continue
		}

		if spec.Name == nil {
			names["context"] = true
			continue
		}

		switch spec.Name.Name {
		case ".":
			dotImported = true
		case "_":
			continue
		default:
			names[spec.Name.Name] = true
		}
	}

	return names, dotImported
}

func sortFindings(findings []Finding) {
	sort.SliceStable(findings, func(i, j int) bool {
		if findings[i].Path != findings[j].Path {
			return findings[i].Path < findings[j].Path
		}

		if findings[i].Kind != findings[j].Kind {
			return findings[i].Kind < findings[j].Kind
		}

		if findings[i].Message != findings[j].Message {
			return findings[i].Message < findings[j].Message
		}

		return findings[i].Severity < findings[j].Severity
	})
}
