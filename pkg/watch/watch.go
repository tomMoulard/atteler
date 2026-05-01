// Package watch provides dependency-free repository scan primitives for
// background agent health checks.
package watch

import (
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
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

	// SeverityInfo marks informational findings.
	SeverityInfo = "info"
	// SeverityWarning marks findings likely to need attention.
	SeverityWarning = "warning"
	// SeverityMaintenance marks maintenance debt findings.
	SeverityMaintenance = "maintenance"
)

// Finding describes a repository scan result.
type Finding struct {
	Path     string
	Kind     string
	Message  string
	Severity string
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

// ScanWithOptions scans root for stale TODO/FIXME markers, large files, and Go
// files missing same-directory _test.go companions.
func ScanWithOptions(root string, options Options) ([]Finding, error) {
	largeFileBytes := options.LargeFileBytes
	if largeFileBytes <= 0 {
		largeFileBytes = DefaultLargeFileBytes
	}

	state := scanState{
		root:           root,
		largeFileBytes: largeFileBytes,
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
	findings       []Finding
	goFiles        map[string]bool
	testFiles      map[string]bool
}

func (s *scanState) visit(path string, entry fs.DirEntry, err error) error {
	if err != nil {
		return fmt.Errorf("visit %s: %w", path, err)
	}
	if entry.IsDir() {
		if shouldSkipDir(entry.Name()) && path != s.root {
			return filepath.SkipDir
		}
		return nil
	}
	return s.scanFile(path, entry)
}

func (s *scanState) scanFile(path string, entry fs.DirEntry) error {
	info, err := entry.Info()
	if err != nil {
		return fmt.Errorf("stat %s: %w", path, err)
	}
	relativePath, err := relative(s.root, path)
	if err != nil {
		return err
	}

	if info.Size() > s.largeFileBytes {
		s.findings = append(s.findings, Finding{
			Path:     relativePath,
			Kind:     KindLargeFile,
			Message:  fmt.Sprintf("file is %d bytes, above %d byte threshold", info.Size(), s.largeFileBytes),
			Severity: SeverityWarning,
		})
	}

	recordGoFile(relativePath, s.goFiles, s.testFiles)

	ok, err := hasStaleTODOMarker(path)
	if err != nil {
		return err
	}
	if ok {
		s.findings = append(s.findings, Finding{
			Path:     relativePath,
			Kind:     KindStaleTODO,
			Message:  "contains stale TODO/FIXME marker",
			Severity: SeverityMaintenance,
		})
	}
	return nil
}

func (s *scanState) addMissingTests() {
	for path := range s.goFiles {
		if !s.testFiles[testPath(path)] {
			s.findings = append(s.findings, Finding{
				Path:     path,
				Kind:     KindMissingTest,
				Message:  "missing _test.go companion",
				Severity: SeverityInfo,
			})
		}
	}
}

func shouldSkipDir(name string) bool {
	switch name {
	case ".git", "vendor":
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
	text := strings.ToUpper(string(content))
	return strings.Contains(text, "TODO") || strings.Contains(text, "FIXME"), nil
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
