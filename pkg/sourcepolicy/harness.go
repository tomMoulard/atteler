package sourcepolicy

import (
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	pathpkg "path"
	"path/filepath"
	"sort"
	"strings"
	"unicode/utf8"
)

const (
	maxHarnessPolicyBytes = 64 * 1024
	maxHarnessPolicyFiles = 64

	harnessKindCursorRules = "cursor_rules"
)

// HarnessFile is a project or tool instruction file that may contain
// source-related research or retrieval guidance.
type HarnessFile struct {
	Path    string
	Kind    string
	Content string
}

// PolicyFromHarnessFiles reads common project/harness instruction files under
// root and extracts their source-policy guidance.
func PolicyFromHarnessFiles(root string) (Policy, []string, error) {
	files, err := DiscoverHarnessFiles(root)
	if err != nil {
		return Policy{}, nil, err
	}

	var policy Policy
	for _, file := range files {
		policy = Extend(policy, PolicyFromGuidance(file.Path, file.Content))
	}

	return policy, harnessFilePaths(files), nil
}

// DiscoverHarnessFiles returns readable UTF-8 harness instruction files under
// root. Unreadable or binary files are ignored so project-local guidance never
// breaks unrelated retrieval/research commands.
func DiscoverHarnessFiles(root string) ([]HarnessFile, error) {
	root, err := normalizeHarnessPolicyRoot(root)
	if err != nil {
		return nil, err
	}

	files := make([]HarnessFile, 0)

	err = filepath.WalkDir(root, func(path string, entry fs.DirEntry, walkErr error) error {
		if shouldIgnoreHarnessWalkError(walkErr) {
			return nil
		}

		if path != root && entry.IsDir() && shouldSkipHarnessPolicyDir(entry.Name()) {
			return filepath.SkipDir
		}

		if entry.IsDir() || len(files) >= maxHarnessPolicyFiles {
			return nil
		}

		rel, relErr := filepath.Rel(root, path)
		if relErr != nil {
			return fmt.Errorf("source policy: relative harness path %s: %w", path, relErr)
		}

		kind, ok := harnessPolicyKind(rel)
		if !ok {
			return nil
		}

		content, ok := tryReadHarnessPolicyText(path)
		if !ok {
			return nil
		}

		files = append(files, HarnessFile{
			Path:    filepath.ToSlash(rel),
			Kind:    kind,
			Content: content,
		})

		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("source policy: discover harness guidance: %w", err)
	}

	sort.Slice(files, func(i, j int) bool {
		return files[i].Path < files[j].Path
	})

	return files, nil
}

func normalizeHarnessPolicyRoot(root string) (string, error) {
	root = strings.TrimSpace(root)
	if root == "" {
		cwd, err := os.Getwd()
		if err != nil {
			return "", fmt.Errorf("source policy: resolve current directory: %w", err)
		}

		root = cwd
	}

	abs, err := filepath.Abs(root)
	if err != nil {
		return "", fmt.Errorf("source policy: resolve harness root %s: %w", root, err)
	}

	return abs, nil
}

func shouldIgnoreHarnessWalkError(err error) bool {
	return err != nil
}

func shouldSkipHarnessPolicyDir(name string) bool {
	switch name {
	case ".git", ".atteler", ".symphony", ".codex", "node_modules", "vendor", "dist", "site", "tmp", "build":
		return true
	default:
		return false
	}
}

func harnessPolicyKind(rel string) (string, bool) {
	slash := filepath.ToSlash(rel)
	base := pathpkg.Base(slash)

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
		return harnessKindCursorRules, true
	}

	if strings.HasPrefix(slash, ".cursor/rules/") {
		return harnessKindCursorRules, true
	}

	if slash == ".github/copilot-instructions.md" {
		return "copilot_instructions", true
	}

	return "", false
}

func tryReadHarnessPolicyText(path string) (string, bool) {
	content, err := readHarnessPolicyText(path)
	if err != nil {
		return "", false
	}

	return content, true
}

func readHarnessPolicyText(path string) (string, error) {
	file, err := os.Open(path)
	if err != nil {
		return "", fmt.Errorf("open %s: %w", path, err)
	}
	defer file.Close()

	data, err := io.ReadAll(io.LimitReader(file, maxHarnessPolicyBytes))
	if err != nil {
		return "", fmt.Errorf("read %s: %w", path, err)
	}

	if !utf8.Valid(data) {
		return "", errors.New("not UTF-8 text")
	}

	return string(data), nil
}

func harnessFilePaths(files []HarnessFile) []string {
	paths := make([]string, 0, len(files))
	for _, file := range files {
		paths = append(paths, file.Path)
	}

	return paths
}
