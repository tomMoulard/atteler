package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"math"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/tommoulard/atteler/pkg/autonomy"
	"github.com/tommoulard/atteler/pkg/events"
	"github.com/tommoulard/atteler/pkg/llm"
)

const (
	defaultFileToolGlobResults = 200
	defaultFileToolGrepResults = 100
	defaultFileToolOutputBytes = 200_000
	fileToolEventKind          = "file"
)

var fileToolSkippedDirs = map[string]struct{}{
	".git":           {},
	".hg":            {},
	".svn":           {},
	"node_modules":   {},
	"vendor":         {},
	"__pycache__":    {},
	".pytest_cache":  {},
	".ruff_cache":    {},
	".mypy_cache":    {},
	".tox":           {},
	".venv":          {},
	"dist":           {},
	"build":          {},
	"target":         {},
	"coverage":       {},
	".symphony":      {},
	".atteler":       {},
	".atteler-state": {},
}

type fileToolExecutorOptions struct {
	WorkingDir string
	Autonomy   autonomy.Level
}

func executeFileTool(ctx context.Context, call llm.ToolCall, opts fileToolExecutorOptions) llm.ToolResult {
	if !llm.IsFileToolName(call.Name) {
		return llm.ToolResult{
			ToolCallID: call.ID,
			Content:    "unknown file tool: " + call.Name,
			IsError:    true,
		}
	}

	if llm.IsWriteFileToolName(call.Name) && !autonomy.Normalize(opts.Autonomy).Allows(autonomy.ActionFileWrite) {
		return llm.ToolResult{
			ToolCallID: call.ID,
			Content:    autonomy.DenialMessage(opts.Autonomy, autonomy.ActionFileWrite, call.Name+" tool"),
			IsError:    true,
		}
	}

	var (
		content string
		err     error
	)

	switch strings.TrimSpace(call.Name) {
	case llm.ToolNameRead:
		content, err = executeReadTool(ctx, call, opts)
	case llm.ToolNameWrite:
		content, err = executeWriteTool(ctx, call, opts)
	case llm.ToolNameEdit:
		content, err = executeEditTool(ctx, call, opts)
	case llm.ToolNameGlob:
		content, err = executeGlobTool(ctx, call, opts)
	case llm.ToolNameGrep:
		content, err = executeGrepTool(ctx, call, opts)
	default:
		err = fmt.Errorf("unknown file tool: %s", call.Name)
	}

	result := llm.ToolResult{ToolCallID: call.ID, Content: content}
	if err != nil {
		result.IsError = true
		if content == "" {
			result.Content = "error: " + err.Error()
		}
	}

	return result
}

func executeReadTool(ctx context.Context, call llm.ToolCall, opts fileToolExecutorOptions) (string, error) {
	path, err := requiredStringInput(call.Input, "path")
	if err != nil {
		return "", err
	}

	offset, offsetSet, err := optionalPositiveIntInput(call.Input, "offset")
	if err != nil {
		return "", err
	}

	limit, limitSet, err := optionalPositiveIntInput(call.Input, "limit")
	if err != nil {
		return "", err
	}

	resolved, err := resolveFileToolPath(opts.WorkingDir, path, true)
	if err != nil {
		return "", err
	}

	authErr := authorizeReadPermission(ctx, "read file tool "+resolved.Rel, "llm.read_tool", resolved.Abs)
	if authErr != nil {
		return "", authErr
	}

	data, err := os.ReadFile(resolved.Abs) // #nosec G304 -- path is resolved and workspace-bounded.
	if err != nil {
		return "", fmt.Errorf("read %s: %w", resolved.Rel, err)
	}

	validateErr := validateTextFileBytes(resolved.Rel, data)
	if validateErr != nil {
		return "", validateErr
	}

	content, truncated := formatReadToolContent(resolved.Rel, data, offset, offsetSet, limit, limitSet, fileToolOutputLimit(ctx))
	emitFileToolRead(ctx, resolved, "read", fileToolCallMetadata(call, map[string]string{
		"bytes":         strconv.Itoa(len(data)),
		"digest_sha256": fileToolDigest(data),
		"truncated":     strconv.FormatBool(truncated),
	}))

	return content, nil
}

func executeWriteTool(ctx context.Context, call llm.ToolCall, opts fileToolExecutorOptions) (string, error) {
	path, err := requiredStringInput(call.Input, "path")
	if err != nil {
		return "", err
	}

	content, err := requiredRawStringInput(call.Input, "content", true)
	if err != nil {
		return "", err
	}

	createParents, err := optionalBoolInput(call.Input, "create_parent_dirs", false)
	if err != nil {
		return "", err
	}

	resolved, err := resolveFileToolPath(opts.WorkingDir, path, false)
	if err != nil {
		return "", err
	}

	validateErr := validateTextFileBytes(resolved.Rel, []byte(content))
	if validateErr != nil {
		return "", validateErr
	}

	if err := authorizeWritePermission(ctx, "write file tool "+resolved.Rel, "llm.write_tool", resolved.Abs); err != nil {
		return "", err
	}

	if createParents {
		if err := os.MkdirAll(filepath.Dir(resolved.Abs), 0o750); err != nil {
			return "", fmt.Errorf("create parent directories for %s: %w", resolved.Rel, err)
		}
	}

	if err := writeTextFileToolFile(resolved.Abs, []byte(content)); err != nil {
		return "", fmt.Errorf("write %s: %w", resolved.Rel, err)
	}

	emitFileToolWrite(ctx, resolved, "write", fileToolCallMetadata(call, map[string]string{
		"bytes":         strconv.Itoa(len([]byte(content))),
		"digest_sha256": fileToolDigest([]byte(content)),
	}))

	return fmt.Sprintf("wrote %s (%d bytes)", resolved.Rel, len([]byte(content))), nil
}

func executeEditTool(ctx context.Context, call llm.ToolCall, opts fileToolExecutorOptions) (string, error) {
	path, err := requiredStringInput(call.Input, "path")
	if err != nil {
		return "", err
	}

	oldString, err := requiredRawStringInput(call.Input, "old_string", false)
	if err != nil {
		return "", err
	}

	newString, err := requiredRawStringInput(call.Input, "new_string", true)
	if err != nil {
		return "", err
	}

	replaceAll, err := optionalBoolInput(call.Input, "replace_all", false)
	if err != nil {
		return "", err
	}

	if oldString == "" {
		return "", errors.New("old_string must not be empty")
	}

	resolved, err := resolveFileToolPath(opts.WorkingDir, path, true)
	if err != nil {
		return "", err
	}

	authErr := authorizeReadPermission(ctx, "edit file tool read "+resolved.Rel, "llm.edit_tool", resolved.Abs)
	if authErr != nil {
		return "", authErr
	}

	data, err := os.ReadFile(resolved.Abs) // #nosec G304 -- path is resolved and workspace-bounded.
	if err != nil {
		return "", fmt.Errorf("read %s for edit: %w", resolved.Rel, err)
	}

	validateErr := validateTextFileBytes(resolved.Rel, data)
	if validateErr != nil {
		return "", validateErr
	}

	emitFileToolRead(ctx, resolved, "edit", fileToolCallMetadata(call, map[string]string{
		"bytes":         strconv.Itoa(len(data)),
		"digest_sha256": fileToolDigest(data),
	}))

	current := string(data)

	updated, count, err := editedFileContent(resolved.Rel, current, oldString, newString, replaceAll)
	if err != nil {
		return "", err
	}

	validateErr = validateTextFileBytes(resolved.Rel, []byte(updated))
	if validateErr != nil {
		return "", validateErr
	}

	authErr = authorizeWritePermission(ctx, "edit file tool write "+resolved.Rel, "llm.edit_tool", resolved.Abs)
	if authErr != nil {
		return "", authErr
	}

	if err := writeTextFileToolFile(resolved.Abs, []byte(updated)); err != nil {
		return "", fmt.Errorf("write edited %s: %w", resolved.Rel, err)
	}

	emitFileToolWrite(ctx, resolved, "edit", fileToolCallMetadata(call, map[string]string{
		"bytes":             strconv.Itoa(len([]byte(updated))),
		"digest_sha256":     fileToolDigest([]byte(updated)),
		"old_bytes":         strconv.Itoa(len(data)),
		"old_digest_sha256": fileToolDigest(data),
		"replacements":      strconv.Itoa(count),
	}))

	return fmt.Sprintf("edited %s (%d replacement%s, %d -> %d bytes)",
		resolved.Rel,
		count,
		pluralSuffix(count),
		len(data),
		len([]byte(updated)),
	), nil
}

func editedFileContent(rel, current, oldString, newString string, replaceAll bool) (updated string, count int, err error) {
	count = strings.Count(current, oldString)
	if count == 0 {
		return "", 0, fmt.Errorf("edit %s: old_string was not found", rel)
	}

	if !replaceAll && count != 1 {
		return "", 0, fmt.Errorf("edit %s: old_string matched %d times; set replace_all=true or provide a unique old_string", rel, count)
	}

	updated = strings.Replace(current, oldString, newString, replaceCount(replaceAll))
	if updated == current {
		return "", 0, fmt.Errorf("edit %s: replacement did not change file contents", rel)
	}

	return updated, count, nil
}

func executeGlobTool(ctx context.Context, call llm.ToolCall, opts fileToolExecutorOptions) (string, error) {
	pattern, err := requiredStringInput(call.Input, "pattern")
	if err != nil {
		return "", err
	}

	if filepath.IsAbs(pattern) {
		return "", errors.New("pattern must be relative; use path to choose an absolute search root")
	}

	validateErr := validateFileToolGlobPattern(pattern)
	if validateErr != nil {
		return "", validateErr
	}

	baseRaw, err := optionalStringInput(call.Input, "path", ".")
	if err != nil {
		return "", err
	}

	maxResults, err := optionalMaxResults(call.Input, "max_results", defaultFileToolGlobResults)
	if err != nil {
		return "", err
	}

	base, err := resolveFileToolPath(opts.WorkingDir, baseRaw, true)
	if err != nil {
		return "", err
	}

	stat, err := os.Stat(base.Abs)
	if err != nil {
		return "", fmt.Errorf("stat %s: %w", base.Rel, err)
	}

	if !stat.IsDir() {
		return "", fmt.Errorf("glob path %s is not a directory", base.Rel)
	}

	authErr := authorizeReadPermission(ctx, "glob file tool "+base.Rel, "llm.glob_tool", base.Abs)
	if authErr != nil {
		return "", authErr
	}

	matches, truncated, err := globWorkspaceFiles(ctx, base.Abs, pattern, maxResults)
	if err != nil {
		return "", err
	}

	matches = qualifyFileToolPaths(base.Rel, matches)

	var b strings.Builder
	fmt.Fprintf(&b, "matched %d file%s under %s for %q", len(matches), pluralSuffix(len(matches)), base.Rel, pattern)

	if truncated {
		fmt.Fprintf(&b, " (truncated at %d)", maxResults)
	}

	b.WriteString(":\n")

	for _, match := range matches {
		b.WriteString(match)
		b.WriteByte('\n')
	}

	content, outputTruncated := truncateFileToolOutput(strings.TrimRight(b.String(), "\n"), fileToolOutputLimit(ctx))
	emitFileToolRead(ctx, base, "glob", fileToolCallMetadata(call, map[string]string{
		"pattern":               pattern,
		"matches":               strconv.Itoa(len(matches)),
		"max_results_truncated": strconv.FormatBool(truncated),
		"output_truncated":      strconv.FormatBool(outputTruncated),
		"truncated":             strconv.FormatBool(truncated || outputTruncated),
	}))

	return content, nil
}

func executeGrepTool(ctx context.Context, call llm.ToolCall, opts fileToolExecutorOptions) (string, error) {
	inputs, err := parseGrepToolInputs(call.Input)
	if err != nil {
		return "", err
	}

	searchRoot, err := resolveFileToolPath(opts.WorkingDir, inputs.path, true)
	if err != nil {
		return "", err
	}

	authErr := authorizeReadPermission(ctx, "grep file tool "+searchRoot.Rel, "llm.grep_tool", searchRoot.Abs)
	if authErr != nil {
		return "", authErr
	}

	regexPattern := inputs.pattern
	if !inputs.caseSensitive {
		regexPattern = "(?i)" + inputs.pattern
	}

	re, err := regexp.Compile(regexPattern)
	if err != nil {
		return "", fmt.Errorf("compile grep pattern: %w", err)
	}

	matches, filesScanned, truncated, err := grepWorkspaceFiles(ctx, searchRoot.Abs, searchRoot.Rel, re, inputs.include, inputs.maxResults)
	if err != nil {
		return "", err
	}

	return formatGrepToolResult(ctx, call, searchRoot, inputs, matches, filesScanned, truncated), nil
}

type grepToolInputs struct {
	pattern       string
	path          string
	include       string
	maxResults    int
	caseSensitive bool
}

func parseGrepToolInputs(input map[string]any) (grepToolInputs, error) {
	pattern, err := requiredStringInput(input, "pattern")
	if err != nil {
		return grepToolInputs{}, err
	}

	pathRaw, err := optionalStringInput(input, "path", ".")
	if err != nil {
		return grepToolInputs{}, err
	}

	include, err := optionalStringInput(input, "include", "")
	if err != nil {
		return grepToolInputs{}, err
	}

	if filepath.IsAbs(include) {
		return grepToolInputs{}, errors.New("include must be a relative glob")
	}

	if include != "" {
		validateErr := validateFileToolGlobPattern(include)
		if validateErr != nil {
			return grepToolInputs{}, fmt.Errorf("include %w", validateErr)
		}
	}

	caseSensitive, err := optionalBoolInput(input, "case_sensitive", true)
	if err != nil {
		return grepToolInputs{}, err
	}

	maxResults, err := optionalMaxResults(input, "max_results", defaultFileToolGrepResults)
	if err != nil {
		return grepToolInputs{}, err
	}

	return grepToolInputs{
		pattern:       pattern,
		path:          pathRaw,
		include:       include,
		maxResults:    maxResults,
		caseSensitive: caseSensitive,
	}, nil
}

func formatGrepToolResult(
	ctx context.Context,
	call llm.ToolCall,
	searchRoot resolvedFileToolPath,
	inputs grepToolInputs,
	matches []string,
	filesScanned int,
	truncated bool,
) string {
	var b strings.Builder
	fmt.Fprintf(&b, "matched %d line%s in %d file%s under %s",
		len(matches),
		pluralSuffix(len(matches)),
		filesScanned,
		pluralSuffix(filesScanned),
		searchRoot.Rel,
	)

	if truncated {
		fmt.Fprintf(&b, " (truncated at %d)", inputs.maxResults)
	}

	b.WriteString(":\n")

	for _, match := range matches {
		b.WriteString(match)
		b.WriteByte('\n')
	}

	content, outputTruncated := truncateFileToolOutput(strings.TrimRight(b.String(), "\n"), fileToolOutputLimit(ctx))
	emitFileToolRead(ctx, searchRoot, "grep", fileToolCallMetadata(call, map[string]string{
		"pattern":               inputs.pattern,
		"include":               inputs.include,
		"matches":               strconv.Itoa(len(matches)),
		"files_scanned":         strconv.Itoa(filesScanned),
		"max_results_truncated": strconv.FormatBool(truncated),
		"output_truncated":      strconv.FormatBool(outputTruncated),
		"truncated":             strconv.FormatBool(truncated || outputTruncated),
	}))

	return content
}

type resolvedFileToolPath struct {
	Abs string
	Rel string
}

func resolveFileToolPath(root, rawPath string, mustExist bool) (resolvedFileToolPath, error) {
	rawPath = strings.TrimSpace(rawPath)
	if rawPath == "" {
		return resolvedFileToolPath{}, errors.New("path must not be empty")
	}

	if strings.ContainsRune(rawPath, '\x00') {
		return resolvedFileToolPath{}, errors.New("path contains a NUL byte")
	}

	root = strings.TrimSpace(root)
	if root == "" {
		cwd, err := os.Getwd()
		if err != nil {
			return resolvedFileToolPath{}, fmt.Errorf("get working directory: %w", err)
		}

		root = cwd
	}

	rootAbs, err := filepath.Abs(root)
	if err != nil {
		return resolvedFileToolPath{}, fmt.Errorf("resolve workspace root: %w", err)
	}

	rootAbs = filepath.Clean(rootAbs)

	target := rawPath
	if !filepath.IsAbs(target) {
		target = filepath.Join(rootAbs, target)
	}

	targetAbs, err := filepath.Abs(target)
	if err != nil {
		return resolvedFileToolPath{}, fmt.Errorf("resolve %s: %w", rawPath, err)
	}

	targetAbs = filepath.Clean(targetAbs)

	if !pathWithinRoot(rootAbs, targetAbs) {
		return resolvedFileToolPath{}, fmt.Errorf("path %s is outside workspace root %s", rawPath, rootAbs)
	}

	symlinkErr := validateSymlinkBoundedPath(rootAbs, targetAbs, mustExist)
	if symlinkErr != nil {
		return resolvedFileToolPath{}, symlinkErr
	}

	if mustExist {
		if _, statErr := os.Stat(targetAbs); statErr != nil {
			return resolvedFileToolPath{}, fmt.Errorf("stat %s: %w", rawPath, statErr)
		}
	}

	rel, err := filepath.Rel(rootAbs, targetAbs)
	if err != nil {
		rel = targetAbs
	}

	if rel == "." {
		rel = "."
	}

	return resolvedFileToolPath{Abs: targetAbs, Rel: filepath.ToSlash(rel)}, nil
}

func validateSymlinkBoundedPath(rootAbs, targetAbs string, mustExist bool) error {
	rootEval, err := filepath.EvalSymlinks(rootAbs)
	if err != nil {
		return fmt.Errorf("resolve workspace root symlinks: %w", err)
	}

	targetEval, targetEvalErr := filepath.EvalSymlinks(targetAbs)
	if targetEvalErr == nil {
		if !pathWithinRoot(rootEval, targetEval) {
			return fmt.Errorf("path %s resolves outside workspace root %s", targetAbs, rootAbs)
		}

		return nil
	} else if mustExist || !errors.Is(targetEvalErr, fs.ErrNotExist) {
		return fmt.Errorf("resolve %s symlinks: %w", targetAbs, targetEvalErr)
	}

	ancestor := targetAbs
	for {
		ancestor = filepath.Dir(ancestor)
		if ancestor == "." || ancestor == string(filepath.Separator) || ancestor == filepath.Dir(ancestor) {
			return fmt.Errorf("resolve %s symlinks: no existing parent inside workspace", targetAbs)
		}

		if _, statErr := os.Stat(ancestor); statErr == nil {
			break
		} else if !errors.Is(statErr, fs.ErrNotExist) {
			return fmt.Errorf("stat parent %s: %w", ancestor, statErr)
		}
	}

	ancestorEval, err := filepath.EvalSymlinks(ancestor)
	if err != nil {
		return fmt.Errorf("resolve parent %s symlinks: %w", ancestor, err)
	}

	if !pathWithinRoot(rootEval, ancestorEval) {
		return fmt.Errorf("path %s parent resolves outside workspace root %s", targetAbs, rootAbs)
	}

	return nil
}

func pathWithinRoot(root, path string) bool {
	rel, err := filepath.Rel(filepath.Clean(root), filepath.Clean(path))
	if err != nil {
		return false
	}

	return rel == "." || (rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)))
}

func globWorkspaceFiles(ctx context.Context, baseAbs, pattern string, maxResults int) (matches []string, truncated bool, err error) {
	pattern = filepath.ToSlash(filepath.Clean(pattern))
	if pattern == "." {
		return nil, false, errors.New("glob pattern must match files")
	}

	walker := globWorkspaceWalker{
		ctx:        ctx,
		baseAbs:    baseAbs,
		pattern:    pattern,
		maxResults: maxResults,
	}

	err = filepath.WalkDir(baseAbs, walker.walk)
	if errors.Is(err, errStopWalk) {
		err = nil
	}

	if err != nil {
		return nil, false, fmt.Errorf("glob %q: %w", pattern, err)
	}

	matches = walker.matches
	truncated = walker.truncated

	sort.Strings(matches)

	if truncated {
		matches = matches[:maxResults]
	}

	return matches, truncated, nil
}

type globWorkspaceWalker struct {
	ctx        context.Context
	baseAbs    string
	pattern    string
	matches    []string
	maxResults int
	truncated  bool
}

func (w *globWorkspaceWalker) walk(path string, entry fs.DirEntry, walkErr error) error {
	if err := fileToolContextError(w.ctx, "glob canceled"); err != nil {
		return err
	}

	if walkErr != nil {
		return walkErr
	}

	if entry.IsDir() {
		if shouldSkipFileToolDir(entry.Name()) && path != w.baseAbs {
			return filepath.SkipDir
		}

		return nil
	}

	if !entry.Type().IsRegular() {
		return nil
	}

	rel, err := filepath.Rel(w.baseAbs, path)
	if err != nil {
		return fmt.Errorf("relative glob path: %w", err)
	}

	rel = filepath.ToSlash(rel)
	if !matchFileToolGlob(w.pattern, rel) {
		return nil
	}

	w.matches = append(w.matches, rel)
	if len(w.matches) > w.maxResults {
		w.truncated = true

		return errStopWalk
	}

	return nil
}

var errStopWalk = errors.New("stop walk")

func grepWorkspaceFiles(ctx context.Context, searchAbs, searchRel string, re *regexp.Regexp, include string, maxResults int) (matches []string, filesScanned int, truncated bool, err error) {
	stat, err := os.Stat(searchAbs)
	if err != nil {
		return nil, 0, false, fmt.Errorf("stat %s: %w", searchRel, err)
	}

	if !stat.IsDir() {
		return grepWorkspaceSingleFile(ctx, searchAbs, searchRel, re, maxResults)
	}

	return grepWorkspaceDir(ctx, searchAbs, searchRel, re, include, maxResults)
}

func grepWorkspaceSingleFile(ctx context.Context, searchAbs, searchRel string, re *regexp.Regexp, maxResults int) (matches []string, filesScanned int, truncated bool, err error) {
	matches, skipped, truncated, err := grepFile(ctx, searchAbs, searchRel, re, maxResults)
	if err != nil {
		return nil, 0, false, err
	}

	if skipped {
		return nil, 0, false, nil
	}

	return matches, 1, truncated, nil
}

func grepWorkspaceDir(ctx context.Context, searchAbs, searchRel string, re *regexp.Regexp, include string, maxResults int) (matches []string, filesScanned int, truncated bool, err error) {
	walker := grepWorkspaceDirWalker{
		ctx:        ctx,
		searchAbs:  searchAbs,
		searchRel:  searchRel,
		include:    filepath.ToSlash(include),
		re:         re,
		maxResults: maxResults,
	}

	err = filepath.WalkDir(searchAbs, walker.walk)
	if errors.Is(err, errStopWalk) {
		err = nil
	}

	if err != nil {
		return nil, 0, false, fmt.Errorf("grep %s: %w", searchRel, err)
	}

	return walker.matches, walker.filesScanned, walker.truncated, nil
}

type grepWorkspaceDirWalker struct {
	ctx          context.Context
	re           *regexp.Regexp
	searchAbs    string
	searchRel    string
	include      string
	matches      []string
	maxResults   int
	filesScanned int
	truncated    bool
}

func (w *grepWorkspaceDirWalker) walk(path string, entry fs.DirEntry, walkErr error) error {
	if err := fileToolContextError(w.ctx, "grep canceled"); err != nil {
		return err
	}

	if walkErr != nil {
		return walkErr
	}

	if entry.IsDir() {
		if shouldSkipFileToolDir(entry.Name()) && path != w.searchAbs {
			return filepath.SkipDir
		}

		return nil
	}

	if !entry.Type().IsRegular() {
		return nil
	}

	rel, err := filepath.Rel(w.searchAbs, path)
	if err != nil {
		return fmt.Errorf("relative grep path: %w", err)
	}

	rel = filepath.ToSlash(rel)
	if w.include != "" && !matchFileToolGlob(w.include, rel) {
		return nil
	}

	return w.grepFile(path, joinFileToolRel(w.searchRel, rel))
}

func (w *grepWorkspaceDirWalker) grepFile(path, rel string) error {
	remaining := w.maxResults - len(w.matches)
	if remaining <= 0 {
		fileMatches, skipped, _, err := grepFile(w.ctx, path, rel, w.re, 1)
		if err != nil {
			return err
		}

		if skipped {
			return nil
		}

		w.filesScanned++
		if len(fileMatches) > 0 {
			w.truncated = true

			return errStopWalk
		}

		return nil
	}

	fileMatches, skipped, fileTruncated, err := grepFile(w.ctx, path, rel, w.re, remaining)
	if err != nil {
		return err
	}

	if skipped {
		return nil
	}

	w.filesScanned++

	w.matches = append(w.matches, fileMatches...)
	if fileTruncated {
		w.truncated = true

		return errStopWalk
	}

	return nil
}

func grepFile(ctx context.Context, path, displayPath string, re *regexp.Regexp, maxResults int) (matches []string, skipped, truncated bool, err error) {
	contextErr := fileToolContextError(ctx, "grep canceled")
	if contextErr != nil {
		return nil, false, false, contextErr
	}

	data, err := os.ReadFile(path) // #nosec G304 -- path is resolved/walked inside workspace.
	if err != nil {
		return nil, false, false, fmt.Errorf("read %s: %w", displayPath, err)
	}

	if isBinaryData(data) || !utf8.Valid(data) {
		return nil, true, false, nil
	}

	lines := strings.Split(string(data), "\n")
	if len(lines) > 0 && lines[len(lines)-1] == "" {
		lines = lines[:len(lines)-1]
	}

	for lineNo, line := range lines {
		contextErr = fileToolContextError(ctx, "grep canceled")
		if contextErr != nil {
			return nil, false, false, contextErr
		}

		if !re.MatchString(line) {
			continue
		}

		matches = append(matches, fmt.Sprintf("%s:%d:%s", displayPath, lineNo+1, line))
		if len(matches) > maxResults {
			truncated = true
			matches = matches[:maxResults]

			break
		}
	}

	return matches, false, truncated, nil
}

func shouldSkipFileToolDir(name string) bool {
	_, ok := fileToolSkippedDirs[name]

	return ok
}

func fileToolContextError(ctx context.Context, message string) error {
	if ctx == nil {
		return nil
	}

	if err := ctx.Err(); err != nil {
		return fmt.Errorf("%s: %w", message, err)
	}

	return nil
}

func qualifyFileToolPaths(baseRel string, rels []string) []string {
	if len(rels) == 0 {
		return rels
	}

	qualified := make([]string, 0, len(rels))
	for _, rel := range rels {
		qualified = append(qualified, joinFileToolRel(baseRel, rel))
	}

	return qualified
}

func joinFileToolRel(baseRel, rel string) string {
	baseRel = filepath.ToSlash(strings.TrimSpace(baseRel))
	rel = filepath.ToSlash(strings.TrimSpace(rel))

	if baseRel == "" || baseRel == "." {
		return rel
	}

	if rel == "" || rel == "." {
		return baseRel
	}

	return baseRel + "/" + rel
}

func validateFileToolGlobPattern(pattern string) error {
	pattern = filepath.ToSlash(strings.TrimSpace(pattern))
	if pattern == "" {
		return errors.New("glob pattern must not be empty")
	}

	for part := range strings.SplitSeq(pattern, "/") {
		if part == "" || part == "**" {
			continue
		}

		if _, err := filepath.Match(part, ""); err != nil {
			return fmt.Errorf("glob pattern %q is invalid: %w", pattern, err)
		}
	}

	return nil
}

func validateTextFileBytes(path string, data []byte) error {
	if isBinaryData(data) {
		return fmt.Errorf("%s appears to be binary; file tools only support text files", path)
	}

	if !utf8.Valid(data) {
		return fmt.Errorf("%s is not valid UTF-8; file tools only support UTF-8 text", path)
	}

	return nil
}

func writeTextFileToolFile(path string, data []byte) error {
	mode, err := fileToolWriteMode(path)
	if err != nil {
		return err
	}

	if err := os.WriteFile(path, data, mode); err != nil { // #nosec G304,G703 -- caller resolves path inside the workspace root.
		return fmt.Errorf("write %s: %w", path, err)
	}

	return nil
}

func fileToolWriteMode(path string) (fs.FileMode, error) {
	info, err := os.Stat(path)
	if err == nil {
		if info.IsDir() {
			return 0, fmt.Errorf("%s is a directory", path)
		}

		return info.Mode().Perm(), nil
	}

	if !errors.Is(err, fs.ErrNotExist) {
		return 0, fmt.Errorf("stat %s: %w", path, err)
	}

	return 0o600, nil
}

func isBinaryData(data []byte) bool {
	return bytes.IndexByte(data, 0) >= 0
}

func formatReadToolContent(rel string, data []byte, offset int, offsetSet bool, limit int, limitSet bool, outputLimit int64) (string, bool) {
	text := string(data)
	header := fmt.Sprintf("%s (%d bytes)", rel, len(data))

	if offsetSet || limitSet {
		lines := strings.Split(text, "\n")
		if len(lines) > 0 && lines[len(lines)-1] == "" {
			lines = lines[:len(lines)-1]
		}

		start := 1
		if offsetSet {
			start = offset
		}

		if start > len(lines) {
			return fmt.Sprintf("%s\nrequested start line %d is beyond end of file (%d lines)", header, start, len(lines)), false
		}

		end := len(lines)
		if limitSet {
			end = min(end, start+limit-1)
		}

		var b strings.Builder
		fmt.Fprintf(&b, "%s lines %d-%d of %d\n", header, start, end, len(lines))

		for i := start - 1; i < end; i++ {
			fmt.Fprintf(&b, "%6d | %s\n", i+1, lines[i])
		}

		return truncateFileToolOutput(strings.TrimRight(b.String(), "\n"), outputLimit)
	}

	return truncateFileToolOutput(header+"\n"+text, outputLimit)
}

func truncateFileToolOutput(content string, limit int64) (string, bool) {
	if limit <= 0 {
		limit = defaultFileToolOutputBytes
	}

	if int64(len([]byte(content))) <= limit {
		return content, false
	}

	if limit < 128 {
		limit = 128
	}

	data := []byte(content)

	truncated := string(data[:limit])
	for !utf8.ValidString(truncated) && truncated != "" {
		truncated = truncated[:len(truncated)-1]
	}

	return truncated + "\n\n[truncated]", true
}

func fileToolOutputLimit(ctx context.Context) int64 {
	limit := agentLoopToolOutputLimit(ctx)
	if limit <= 0 {
		return defaultFileToolOutputBytes
	}

	return min(limit, defaultFileToolOutputBytes)
}

func fileToolCallMetadata(call llm.ToolCall, metadata map[string]string) map[string]string {
	if metadata == nil {
		metadata = make(map[string]string)
	}

	if call.ID != "" {
		metadata["tool_call_id"] = call.ID
	}

	return metadata
}

func fileToolDigest(data []byte) string {
	sum := sha256.Sum256(data)

	return fmt.Sprintf("sha256:%x", sum[:])
}

func emitFileToolRead(ctx context.Context, path resolvedFileToolPath, toolKind string, metadata map[string]string) {
	emitFileToolEvent(ctx, events.FileRead, path, toolKind, metadata)
}

func emitFileToolWrite(ctx context.Context, path resolvedFileToolPath, toolKind string, metadata map[string]string) {
	emitFileToolEvent(ctx, events.FileWrite, path, toolKind, metadata)
}

func emitFileToolEvent(ctx context.Context, eventType string, path resolvedFileToolPath, toolKind string, metadata map[string]string) {
	if metadata == nil {
		metadata = make(map[string]string)
	}

	metadata["path"] = path.Rel
	metadata["absolute_path"] = path.Abs
	metadata["kind"] = fileToolEventKind
	metadata["tool"] = toolKind
	metadata["source"] = "llm_tool"

	emitFromContextWarning(ctx, events.Event{
		Type:     eventType,
		Metadata: metadata,
	})
}

func requiredStringInput(input map[string]any, key string) (string, error) {
	value := stringInputDefault(input, key, "")
	if value == "" {
		return "", fmt.Errorf("%s must be a non-empty string", key)
	}

	return value, nil
}

func requiredRawStringInput(input map[string]any, key string, allowEmpty bool) (string, error) {
	if input == nil {
		return "", fmt.Errorf("%s must be a string", key)
	}

	value, ok := input[key]
	if !ok || value == nil {
		return "", fmt.Errorf("%s must be a string", key)
	}

	text, ok := value.(string)
	if !ok {
		return "", fmt.Errorf("%s must be a string", key)
	}

	if !allowEmpty && text == "" {
		return "", fmt.Errorf("%s must be a non-empty string", key)
	}

	return text, nil
}

func stringInputDefault(input map[string]any, key, fallback string) string {
	if input == nil {
		return fallback
	}

	value, ok := input[key]
	if !ok || value == nil {
		return fallback
	}

	text, ok := value.(string)
	if !ok {
		return fallback
	}

	text = strings.TrimSpace(text)
	if text == "" {
		return fallback
	}

	return text
}

func optionalStringInput(input map[string]any, key, fallback string) (string, error) {
	if input == nil {
		return fallback, nil
	}

	value, ok := input[key]
	if !ok || value == nil {
		return fallback, nil
	}

	text, ok := value.(string)
	if !ok {
		return "", fmt.Errorf("%s must be a string", key)
	}

	text = strings.TrimSpace(text)
	if text == "" {
		return fallback, nil
	}

	return text, nil
}

func optionalBoolInput(input map[string]any, key string, fallback bool) (bool, error) {
	if input == nil {
		return fallback, nil
	}

	value, ok := input[key]
	if !ok || value == nil {
		return fallback, nil
	}

	typed, ok := value.(bool)
	if !ok {
		return false, fmt.Errorf("%s must be a boolean", key)
	}

	return typed, nil
}

func optionalPositiveIntInput(input map[string]any, key string) (value int, ok bool, err error) {
	if input == nil {
		return 0, false, nil
	}

	rawValue, ok := input[key]
	if !ok || rawValue == nil {
		return 0, false, nil
	}

	intValue, err := numericInputToInt(rawValue)
	if err != nil {
		return 0, true, fmt.Errorf("%s must be a positive integer: %w", key, err)
	}

	if intValue <= 0 {
		return 0, true, fmt.Errorf("%s must be greater than zero", key)
	}

	return intValue, true, nil
}

func optionalMaxResults(input map[string]any, key string, fallback int) (int, error) {
	value, ok, err := optionalPositiveIntInput(input, key)
	if err != nil {
		return 0, err
	}

	if !ok {
		return fallback, nil
	}

	if value > llm.FileToolMaxResults {
		return 0, fmt.Errorf("%s must be less than or equal to %d", key, llm.FileToolMaxResults)
	}

	return value, nil
}

func numericInputToInt(value any) (int, error) {
	switch typed := value.(type) {
	case int:
		return typed, nil
	case int64:
		if typed > int64(maxIntValue) || typed < int64(minIntValue) {
			return 0, fmt.Errorf("%v is outside the supported integer range", typed)
		}

		return int(typed), nil
	case float64:
		if math.IsNaN(typed) || math.IsInf(typed, 0) {
			return 0, fmt.Errorf("%v is not finite", typed)
		}

		if typed > float64(maxIntValue) || typed < float64(minIntValue) {
			return 0, fmt.Errorf("%v is outside the supported integer range", typed)
		}

		if typed != float64(int(typed)) {
			return 0, fmt.Errorf("%v is not an integer", typed)
		}

		return int(typed), nil
	case json.Number:
		i, err := strconv.Atoi(typed.String())
		if err != nil {
			return 0, fmt.Errorf("parse json number: %w", err)
		}

		return i, nil
	default:
		return 0, fmt.Errorf("got %T", value)
	}
}

const (
	maxIntValue = int(^uint(0) >> 1)
	minIntValue = -maxIntValue - 1
)

func replaceCount(replaceAll bool) int {
	if replaceAll {
		return -1
	}

	return 1
}

func pluralSuffix(count int) string {
	if count == 1 {
		return ""
	}

	return "s"
}

// matchFileToolGlob matches a slash-separated path against a pattern that may
// contain ** segments.
func matchFileToolGlob(pattern, name string) bool {
	pattern = filepath.ToSlash(pattern)
	name = filepath.ToSlash(name)

	patParts := strings.Split(pattern, "/")
	nameParts := strings.Split(name, "/")

	return matchFileToolGlobParts(patParts, nameParts)
}

func matchFileToolGlobParts(patParts, nameParts []string) bool {
	for len(patParts) > 0 && len(nameParts) > 0 {
		if patParts[0] == "**" {
			return matchFileToolDoublestar(patParts[1:], nameParts)
		}

		matched, err := filepath.Match(patParts[0], nameParts[0])
		if err != nil || !matched {
			return false
		}

		patParts = patParts[1:]
		nameParts = nameParts[1:]
	}

	for len(patParts) > 0 && patParts[0] == "**" {
		patParts = patParts[1:]
	}

	return len(patParts) == 0 && len(nameParts) == 0
}

func matchFileToolDoublestar(patAfter, nameParts []string) bool {
	if len(patAfter) == 0 {
		return true
	}

	for skip := 0; skip <= len(nameParts); skip++ {
		if matchFileToolGlobParts(patAfter, nameParts[skip:]) {
			return true
		}
	}

	return false
}
