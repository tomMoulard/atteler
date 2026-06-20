package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"html"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/tommoulard/atteler/pkg/config"
	"github.com/tommoulard/atteler/pkg/contextpack"
	"github.com/tommoulard/atteler/pkg/contextref"
	"github.com/tommoulard/atteler/pkg/llm"
)

const (
	projectInstructionPrimaryFile  = "AGENTS.md"
	projectInstructionFallbackFile = "CLAUDE.md"
	projectInstructionScope        = "project-instructions"
	projectInstructionChunkRunes   = 600
	projectInstructionWrapperSlack = 256
)

type projectInstructionFile struct {
	Source string
}

func appendProjectInstructionContext(ctx context.Context, base configuredReferenceContext, opts contextref.Options) configuredReferenceContext {
	projectCtx := loadProjectInstructionContext(ctx, opts)
	if projectCtx.Content == "" && len(projectCtx.Manifest.Entries) == 0 {
		if base.Estimator == "" {
			base.Estimator = estimatorSummaryForContextOptions(opts)
		}

		return base
	}

	estimatorSummary := estimatorSummaryForContextOptions(opts)

	return configuredReferenceContext{
		Content:   appendReferenceContext(projectCtx.Content, base.Content),
		Manifest:  mergeReferenceManifests(projectCtx.Manifest, base.Manifest),
		Estimator: estimatorSummary,
	}
}

func loadProjectInstructionContext(ctx context.Context, opts contextref.Options) configuredReferenceContext {
	estimatorSummary := estimatorSummaryForContextOptions(opts)
	if opts.ProjectInstructionsDisabled {
		return configuredReferenceContext{
			Manifest:  withReferenceManifestEstimator(contextref.BuildReferenceManifest(nil), estimatorSummary),
			Estimator: estimatorSummary,
		}
	}

	repoRoot, files, err := discoverProjectInstructionFiles(opts.Root)
	if err != nil || len(files) == 0 {
		return configuredReferenceContext{
			Manifest:  withReferenceManifestEstimator(contextref.BuildReferenceManifest(nil), estimatorSummary),
			Estimator: estimatorSummary,
		}
	}

	refs := make([]string, 0, len(files))
	for _, file := range files {
		refs = append(refs, file.Source)
	}

	instructionOpts := opts
	instructionOpts.Root = repoRoot
	instructionOpts.ReferenceScope = projectInstructionScope
	// Project instructions are discovered deterministically under repoRoot.
	// Keep contextref's size, binary, and symlink safety checks, but do not let
	// context.reference_policy globs meant for user-configured references
	// accidentally suppress AGENTS.md/CLAUDE.md project memory.
	instructionOpts.ReferencePolicy = projectInstructionReferencePolicy(opts.ReferencePolicy)

	loaded, referenceEvents, loadErr := contextref.LoadReferencesWithReport(ctx, refs, instructionOpts)

	manifest := withReferenceManifestEstimator(contextref.BuildReferenceManifest(referenceEvents), estimatorSummary)
	if loadErr != nil {
		return configuredReferenceContext{
			Manifest:  withReferenceManifestEstimator(contextref.BuildReferenceManifest(omitLoadedConfiguredReferenceEvents(referenceEvents, "project instruction block omitted because loading failed")), estimatorSummary),
			Estimator: estimatorSummary,
		}
	}

	content := formatProjectInstructionBlock(loaded, opts)

	return configuredReferenceContext{
		Content:   content,
		Manifest:  manifest,
		Estimator: estimatorSummary,
	}
}

func projectInstructionReferencePolicy(policy contextref.ReferencePolicy) contextref.ReferencePolicy {
	policy.AllowedGlobs = nil
	policy.DeniedGlobs = nil

	return policy
}

func discoverProjectInstructionFiles(start string) (string, []projectInstructionFile, error) {
	start = strings.TrimSpace(start)
	if start == "" {
		cwd, err := os.Getwd()
		if err != nil {
			return "", nil, fmt.Errorf("project instructions: locate cwd: %w", err)
		}

		start = cwd
	}

	absStart, err := filepath.Abs(start)
	if err != nil {
		return "", nil, fmt.Errorf("project instructions: resolve cwd: %w", err)
	}

	info, err := os.Stat(absStart)
	if err != nil {
		return "", nil, fmt.Errorf("project instructions: stat cwd: %w", err)
	}

	if !info.IsDir() {
		absStart = filepath.Dir(absStart)
	}

	repoRoot := nearestGitRoot(absStart)
	if repoRoot == "" {
		repoRoot = absStart
	}

	dirs := directoriesFromRootToCWD(repoRoot, absStart)
	files := make([]projectInstructionFile, 0, len(dirs))

	for _, dir := range dirs {
		file, ok := firstProjectInstructionFile(repoRoot, dir)
		if ok {
			files = append(files, file)
		}
	}

	return repoRoot, files, nil
}

func nearestGitRoot(start string) string {
	dir := filepath.Clean(start)
	for {
		if gitMetadataExists(filepath.Join(dir, ".git")) {
			return dir
		}

		parent := filepath.Dir(dir)
		if parent == dir {
			return ""
		}

		dir = parent
	}
}

func gitMetadataExists(path string) bool {
	if _, err := os.Stat(path); err == nil {
		return true
	} else if !errors.Is(err, os.ErrNotExist) {
		return true
	}

	return false
}

func directoriesFromRootToCWD(root, cwd string) []string {
	root = filepath.Clean(root)
	cwd = filepath.Clean(cwd)

	rel, err := filepath.Rel(root, cwd)
	if err != nil || rel == "." {
		return []string{root}
	}

	if strings.HasPrefix(rel, ".."+string(filepath.Separator)) || rel == ".." {
		return []string{root}
	}

	dirs := []string{root}
	current := root

	for part := range strings.SplitSeq(rel, string(filepath.Separator)) {
		if part == "" || part == "." {
			continue
		}

		current = filepath.Join(current, part)
		dirs = append(dirs, current)
	}

	return dirs
}

func firstProjectInstructionFile(root, dir string) (projectInstructionFile, bool) {
	for _, name := range []string{projectInstructionPrimaryFile, projectInstructionFallbackFile} {
		path := filepath.Join(dir, name)

		info, err := os.Stat(path)
		if err != nil || info.IsDir() || !info.Mode().IsRegular() {
			continue
		}

		source, relErr := filepath.Rel(root, path)
		if relErr != nil {
			source = path
		}

		return projectInstructionFile{
			Source: filepath.ToSlash(source),
		}, true
	}

	return projectInstructionFile{}, false
}

func formatProjectInstructionBlock(refs []contextref.LoadedReference, opts contextref.Options) string {
	if len(refs) == 0 {
		return ""
	}

	maxTokens := opts.ProjectInstructionsMaxTokens
	if maxTokens <= 0 {
		maxTokens = config.DefaultProjectInstructionsMaxTokens
	}

	chunkMessages := projectInstructionChunkMessages(refs)
	budget := max(1, maxTokens-projectInstructionWrapperSlack)
	result := contextpack.CompactWithOptions(chunkMessages, contextpack.Options{
		Estimator: projectInstructionEstimator(opts),
		MaxTokens: budget,
		Policy: contextpack.Policy{
			DropPinnedWhenNeeded: true,
			ManifestMaxItems:     8,
			ManifestMaxRanges:    16,
			ManifestSummaryRunes: 96,
		},
	})

	messages := result.Messages
	if result.Stats.HardBudgetFailure {
		messages = []llm.Message{{
			Role:    llm.RoleUser,
			Content: projectInstructionBudgetFailureMessage(refs, maxTokens, result.Stats.BudgetFailureReason),
		}}
	}

	var b strings.Builder
	b.WriteString(`<project_instructions source_policy="`)
	b.WriteString(escapeProjectInstructionAttr(projectInstructionPrimaryFile + " preferred; " + projectInstructionFallbackFile + " fallback"))
	b.WriteString(`" precedence="repo-root-to-cwd" max_tokens="`)
	b.WriteString(strconv.Itoa(maxTokens))
	b.WriteString(`" compressed="`)
	b.WriteString(strconv.FormatBool(result.Stats.Compressed))
	b.WriteString(`" omitted_messages="`)
	b.WriteString(strconv.Itoa(result.Stats.OmittedCount))
	b.WriteString(`">`)
	b.WriteString("\n")
	b.WriteString("These project instruction files were auto-loaded. Apply later/deeper files as more specific when they conflict.\n")

	for _, message := range messages {
		if strings.TrimSpace(message.Content) == "" {
			continue
		}

		b.WriteString(message.Content)

		if !strings.HasSuffix(message.Content, "\n") {
			b.WriteString("\n")
		}
	}

	b.WriteString("</project_instructions>")

	return b.String()
}

func projectInstructionEstimator(opts contextref.Options) contextpack.Estimator {
	if opts.TokenEstimator != nil {
		return opts.TokenEstimator
	}

	return contextpack.DefaultEstimator()
}

func projectInstructionChunkMessages(refs []contextref.LoadedReference) []llm.Message {
	var messages []llm.Message

	for i := range refs {
		ref := &refs[i]
		source := strings.TrimSpace(ref.Source)

		chunks := splitProjectInstructionChunks(ref.Content)

		for i, chunk := range chunks {
			messages = append(messages, llm.Message{
				Role: llm.RoleUser,
				Content: formatProjectInstructionChunk(
					source,
					i+1,
					len(chunks),
					ref.Truncated,
					chunk,
				),
			})
		}
	}

	return messages
}

func splitProjectInstructionChunks(content string) []string {
	content = strings.TrimSpace(strings.ToValidUTF8(content, "�"))
	if content == "" {
		return nil
	}

	var chunks []string

	var current strings.Builder

	for paragraph := range strings.SplitSeq(content, "\n\n") {
		paragraph = strings.TrimSpace(paragraph)
		if paragraph == "" {
			continue
		}

		if current.Len() > 0 && current.Len()+len(paragraph)+2 > projectInstructionChunkRunes {
			chunks = append(chunks, current.String())
			current.Reset()
		}

		if utf8.RuneCountInString(paragraph) > projectInstructionChunkRunes {
			if current.Len() > 0 {
				chunks = append(chunks, current.String())
				current.Reset()
			}

			chunks = append(chunks, splitLongProjectInstructionParagraph(paragraph)...)

			continue
		}

		if current.Len() > 0 {
			current.WriteString("\n\n")
		}

		current.WriteString(paragraph)
	}

	if current.Len() > 0 {
		chunks = append(chunks, current.String())
	}

	return chunks
}

func splitLongProjectInstructionParagraph(paragraph string) []string {
	runes := []rune(paragraph)

	chunks := make([]string, 0, (len(runes)/projectInstructionChunkRunes)+1)
	for len(runes) > 0 {
		n := min(projectInstructionChunkRunes, len(runes))
		chunks = append(chunks, string(runes[:n]))
		runes = runes[n:]
	}

	return chunks
}

func formatProjectInstructionChunk(source string, chunk, total int, truncated bool, content string) string {
	var b strings.Builder
	b.WriteString(`<project_instruction_file source="`)
	b.WriteString(escapeProjectInstructionAttr(source))
	b.WriteString(`" chunk="`)
	b.WriteString(strconv.Itoa(chunk))
	b.WriteString(`" chunks="`)
	b.WriteString(strconv.Itoa(total))
	b.WriteString(`" truncated="`)
	b.WriteString(strconv.FormatBool(truncated))
	b.WriteString(`">`)
	b.WriteString("\n")
	b.WriteString(escapeProjectInstructionText(content))
	b.WriteString("\n</project_instruction_file>\n")

	return b.String()
}

func projectInstructionBudgetFailureMessage(refs []contextref.LoadedReference, maxTokens int, reason string) string {
	var b strings.Builder
	b.WriteString(`<project_instruction_budget_failure max_tokens="`)
	b.WriteString(strconv.Itoa(maxTokens))
	b.WriteString(`" reason="`)
	b.WriteString(escapeProjectInstructionAttr(reason))
	b.WriteString(`">`)
	b.WriteString("\n")

	for i := range refs {
		ref := &refs[i]

		b.WriteString(`<project_instruction_source source="`)
		b.WriteString(escapeProjectInstructionAttr(ref.Source))
		b.WriteString(`" />`)
		b.WriteString("\n")
	}

	b.WriteString("</project_instruction_budget_failure>\n")

	return b.String()
}

func escapeProjectInstructionText(value string) string {
	return html.EscapeString(strings.ToValidUTF8(value, "�"))
}

func escapeProjectInstructionAttr(value string) string {
	value = escapeProjectInstructionText(value)
	value = strings.ReplaceAll(value, "\r", "&#13;")
	value = strings.ReplaceAll(value, "\n", "&#10;")
	value = strings.ReplaceAll(value, "\t", "&#9;")

	return value
}

func projectInstructionRuntimeSummary(cwd string, cfg config.ProjectInstructionsConfig) (enabled bool, repoRoot string, sources []string, note string) {
	enabled = cfg.EffectiveEnabled()
	if !enabled {
		return false, "", nil, "disabled by context.project_instructions.enabled"
	}

	repoRoot, files, err := discoverProjectInstructionFiles(cwd)
	if err != nil {
		return true, "", nil, err.Error()
	}

	sources = make([]string, 0, len(files))
	for _, file := range files {
		sources = append(sources, file.Source)
	}

	if len(sources) == 0 {
		note = "no AGENTS.md or CLAUDE.md files discovered from repository root to cwd"
	} else {
		note = projectInstructionPrimaryFile + " preferred over " + projectInstructionFallbackFile + " in each directory"
	}

	return true, repoRoot, sources, note
}

func projectInstructionRuntimeSourceValue(sources []string) string {
	if len(sources) == 0 {
		return "[]"
	}

	data, err := json.Marshal(sources)
	if err != nil {
		return "[]"
	}

	return string(data)
}
