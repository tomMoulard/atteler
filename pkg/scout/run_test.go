//nolint:wsl_v5 // Tests keep artifact setup and assertions close for readability.
package scout

import (
	"bufio"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gopkg.in/yaml.v3"
)

func TestRun_CreatesArtifactsReadsGuidanceAndGeneratesTasks(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(root, "AGENTS.md"), []byte("# Agent rules\nRun tests before changes.\n"), 0o600))
	require.NoError(t, os.MkdirAll(filepath.Join(root, ".cursor", "rules"), 0o750))
	require.NoError(t, os.WriteFile(filepath.Join(root, ".cursor", "rules", "style.mdc"), []byte("# Cursor style\nKeep diffs focused.\n"), 0o600))
	require.NoError(t, os.MkdirAll(filepath.Join(root, "docs"), 0o750))
	require.NoError(t, os.WriteFile(filepath.Join(root, "README.md"), []byte("# Demo CLI\nAtteler supports research and autoresearch workflows.\n"), 0o600))
	require.NoError(t, os.WriteFile(filepath.Join(root, "docs", "common-workflows.md"), []byte("# Workflows\nResearch and review workflows cite evidence.\n"), 0o600))
	require.NoError(t, os.MkdirAll(filepath.Join(root, "cmd", "atteler"), 0o750))
	require.NoError(t, os.WriteFile(filepath.Join(root, "cmd", "atteler", "cli_research_commands.go"), []byte("package main\n"), 0o600))
	require.NoError(t, os.MkdirAll(filepath.Join(root, "pkg", "memory"), 0o750))
	require.NoError(t, os.WriteFile(filepath.Join(root, "Makefile"), []byte("test:\n\tgo test ./...\n"), 0o600))

	result, err := Run(t.Context(), RunRequest{
		Prompt:        "Find 5 feature ideas for Atteler",
		Root:          root,
		OutputDir:     "scout/out",
		Competitors:   []string{"cursor", "https://github.com/All-Hands-AI/OpenHands"},
		GenerateTasks: true,
		Now:           time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC),
	})
	require.NoError(t, err)

	assert.Equal(t, "out", result.RunID)
	assert.Equal(t, filepath.Join(root, "scout", "out"), result.Dir)
	for _, name := range []string{scoutReportFile, ideasFile, competitorsFile, tasksFile, runFile} {
		assert.FileExists(t, filepath.Join(result.Dir, name))
	}

	report := readFile(t, filepath.Join(result.Dir, scoutReportFile))
	assert.Contains(t, report, "## Project understanding")
	assert.Contains(t, report, "## Ranked feature ideas")
	assert.Contains(t, report, "Scout recommendations should cite evidence")
	assert.Contains(t, report, "- Rationale:")
	assert.Contains(t, report, "AGENTS.md")

	ideas := readIdeas(t, filepath.Join(result.Dir, ideasFile))
	require.Len(t, ideas, 5)
	assert.Equal(t, 1, ideas[0].Rank)
	assert.NotEmpty(t, ideas[0].SuggestedMVP)
	assert.NotEmpty(t, ideas[0].RelatedFilesOrAreas)

	competitors := readCompetitors(t, filepath.Join(result.Dir, competitorsFile))
	require.Len(t, competitors, 2)
	assert.Equal(t, "cursor", competitors[0].Name)
	assert.Equal(t, "OpenHands", competitors[1].Name)

	tasks := readFile(t, filepath.Join(result.Dir, tasksFile))
	assert.Contains(t, tasks, "tasks:")
	assert.Contains(t, tasks, "id: \"scout-out-01\"")
	assert.Contains(t, tasks, "status: \"pending\"")
	assert.Contains(t, tasks, "agent: \"executor\"")
	assert.Contains(t, tasks, "metadata:")
	assert.Contains(t, tasks, "source: \"atteler.scout\"")
	assert.Contains(t, tasks, "make test")

	var generated generatedTasksYAML
	require.NoError(t, yaml.Unmarshal([]byte(tasks), &generated))
	require.Len(t, generated.Tasks, 5)
	assert.Equal(t, "scout-out-01", generated.Tasks[0].ID)
	assert.Equal(t, "pending", generated.Tasks[0].Status)
	assert.Equal(t, "executor", generated.Tasks[0].Agent)
	assert.Equal(t, "atteler.scout", generated.Tasks[0].Metadata["source"])
	assert.Equal(t, "out", generated.Tasks[0].Metadata["scout_run_id"])
	assert.Contains(t, generated.Tasks[0].SuggestedValidation, "make test")

	var record runRecord
	require.NoError(t, json.Unmarshal([]byte(readFile(t, filepath.Join(result.Dir, runFile))), &record))
	assert.Equal(t, SchemaVersion, record.Schema)
	assert.Contains(t, record.GuidanceFiles, "AGENTS.md")
	assert.Contains(t, record.GuidanceFiles, ".cursor/rules/style.mdc")
	assert.Equal(t, 5, record.IdeaCount)
	assert.True(t, record.GenerateTasks)
}

type generatedTasksYAML struct {
	Tasks []generatedTaskYAML `yaml:"tasks"`
}

type generatedTaskYAML struct {
	Metadata            map[string]string `yaml:"metadata"`
	ID                  string            `yaml:"id"`
	Title               string            `yaml:"title"`
	Status              string            `yaml:"status"`
	Agent               string            `yaml:"agent"`
	Rationale           string            `yaml:"rationale"`
	SuggestedMVP        string            `yaml:"suggested_mvp"`
	RelatedFilesOrAreas []string          `yaml:"related_files_or_areas"`
	SuggestedValidation []string          `yaml:"suggested_validation"`
	Priority            int               `yaml:"priority"`
}

func TestRun_TournamentWritesRoadmapVariants(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(root, "README.md"), []byte("# Demo CLI\nAutoresearch and review workflows.\n"), 0o600))

	result, err := Run(t.Context(), RunRequest{
		Prompt:       "Find the best next features",
		Root:         root,
		Tournament:   true,
		VariantCount: 4,
		Now:          time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC),
	})
	require.NoError(t, err)

	assert.FileExists(t, filepath.Join(result.Dir, roadmapsFile))
	roadmaps := readRoadmaps(t, filepath.Join(result.Dir, roadmapsFile))
	require.Len(t, roadmaps, 4)

	report := readFile(t, filepath.Join(result.Dir, scoutReportFile))
	assert.Contains(t, report, "### Final comparison")
	assert.Contains(t, report, "| Rank | Decision | Idea | Score | Reason |")
	assert.Contains(t, report, "discarded")

	ideas := readIdeas(t, filepath.Join(result.Dir, ideasFile))
	require.Len(t, ideas, defaultIdeaCount)
	assert.NotEmpty(t, ideas[0].SourceVariants)

	var record runRecord
	require.NoError(t, json.Unmarshal([]byte(readFile(t, filepath.Join(result.Dir, runFile))), &record))
	assert.True(t, record.Tournament.Enabled)
	assert.Equal(t, 4, record.Tournament.Variants)
	assert.Contains(t, record.Tournament.SharedPackage, "pkg/tournament")
}

func TestRun_VariantCountEnablesTournament(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(root, "README.md"), []byte("# Demo CLI\nReview workflows.\n"), 0o600))

	result, err := Run(t.Context(), RunRequest{
		Prompt:       "Find roadmap ideas",
		Root:         root,
		VariantCount: 1,
		Now:          time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC),
	})
	require.NoError(t, err)

	assert.FileExists(t, filepath.Join(result.Dir, roadmapsFile))

	var record runRecord
	require.NoError(t, json.Unmarshal([]byte(readFile(t, filepath.Join(result.Dir, runFile))), &record))
	assert.True(t, record.Tournament.Enabled)
	assert.Equal(t, 1, record.Tournament.Variants)
}

func TestRun_TournamentCanUseSingleExplicitVariant(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(root, "README.md"), []byte("# Demo CLI\nReview workflows.\n"), 0o600))

	result, err := Run(t.Context(), RunRequest{
		Prompt:       "Find roadmap ideas",
		Root:         root,
		Tournament:   true,
		VariantCount: 1,
		Now:          time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC),
	})
	require.NoError(t, err)

	roadmaps := readRoadmaps(t, filepath.Join(result.Dir, roadmapsFile))
	require.Len(t, roadmaps, 1)

	var record runRecord
	require.NoError(t, json.Unmarshal([]byte(readFile(t, filepath.Join(result.Dir, runFile))), &record))
	assert.True(t, record.Tournament.Enabled)
	assert.Equal(t, 1, record.Tournament.Variants)
}

func TestBuildTournamentVariantsCyclesWithUniqueIDs(t *testing.T) {
	t.Parallel()

	variants := buildTournamentVariants([]Idea{
		{
			Title:      "Feature idea",
			Summary:    "A useful feature.",
			Fit:        labelHigh,
			Complexity: labelLow,
			Risk:       labelLow,
		},
	}, 7)

	require.Len(t, variants, 7)
	assert.Equal(t, "user-value", variants[0].ID)
	assert.Equal(t, "user-value-2", variants[5].ID)
	assert.Equal(t, "User value 2", variants[5].Name)
	assert.Equal(t, "feasibility-2", variants[6].ID)
	assert.Equal(t, "feasibility-2-1", variants[6].Candidates[0].ID)
	assert.Equal(t, variants[1].Candidates[0].Feasibility, variants[6].Candidates[0].Feasibility)
}

func TestBuildTournamentVariantsOrdersCandidatesByLensScore(t *testing.T) {
	t.Parallel()

	variants := buildTournamentVariants([]Idea{
		{
			Title:      "Feasible foundation",
			Summary:    "Small implementation that is easy to validate.",
			Fit:        labelHigh,
			Complexity: labelLow,
			Risk:       labelLow,
		},
		{
			Title:      "Evidence-backed bet",
			Summary:    "Riskier idea with stronger citations.",
			Fit:        labelLow,
			Complexity: labelHigh,
			Risk:       labelHigh,
			Evidence: []Evidence{
				{Kind: sourceTypeRepository, Path: "README.md"},
				{Kind: sourceTypeRepository, Path: "docs/common-workflows.md"},
				{Kind: sourceTypeRepository, Path: "docs/architecture.md"},
				{Kind: sourceTypeGuidance, Path: "AGENTS.md"},
				{Kind: "competitor", Source: "cursor"},
			},
		},
	}, 3)

	require.Len(t, variants, 3)
	assert.Equal(t, "Feasible foundation", variants[0].Candidates[0].Title)
	assert.Equal(t, "Feasible foundation", variants[1].Candidates[0].Title)
	assert.Equal(t, "Evidence-backed bet", variants[2].Candidates[0].Title)
	assert.Equal(t, "evidence", variants[2].ID)
}

func TestRun_ReusedOutputDirRemovesDisabledOptionalArtifacts(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(root, "README.md"), []byte("# Demo CLI\nReview workflows.\n"), 0o600))

	first, err := Run(t.Context(), RunRequest{
		Prompt:        "Find feature ideas",
		Root:          root,
		OutputDir:     "scout/out",
		GenerateTasks: true,
		Tournament:    true,
		VariantCount:  2,
		Now:           time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC),
	})
	require.NoError(t, err)
	assert.FileExists(t, filepath.Join(first.Dir, roadmapsFile))
	assert.FileExists(t, filepath.Join(first.Dir, tasksFile))

	second, err := Run(t.Context(), RunRequest{
		Prompt:    "Find feature ideas",
		Root:      root,
		OutputDir: "scout/out",
		Now:       time.Date(2026, 1, 2, 3, 4, 6, 0, time.UTC),
	})
	require.NoError(t, err)
	require.Equal(t, first.Dir, second.Dir)
	assert.NoFileExists(t, filepath.Join(second.Dir, roadmapsFile))
	assert.NoFileExists(t, filepath.Join(second.Dir, tasksFile))

	var record runRecord
	require.NoError(t, json.Unmarshal([]byte(readFile(t, filepath.Join(second.Dir, runFile))), &record))
	assert.False(t, record.GenerateTasks)
	assert.False(t, record.Tournament.Enabled)
	assert.NotContains(t, record.Artifacts, "roadmaps")
	assert.NotContains(t, record.Artifacts, "tasks")
}

func TestRun_AreaAutoresearchPrioritizesAutoresearchIdea(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(root, "README.md"), []byte("# Demo CLI\nAutoresearch workflows validate implementation hypotheses.\n"), 0o600))
	require.NoError(t, os.MkdirAll(filepath.Join(root, "cmd", "atteler"), 0o750))
	require.NoError(t, os.WriteFile(filepath.Join(root, "cmd", "atteler", "cli_autoresearch.go"), []byte("package main\n"), 0o600))

	result, err := Run(t.Context(), RunRequest{
		Prompt: "Find 3 feature ideas",
		Root:   root,
		Area:   "autoresearch",
		Now:    time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC),
	})
	require.NoError(t, err)

	ideas := readIdeas(t, filepath.Join(result.Dir, ideasFile))
	require.NotEmpty(t, ideas)
	assert.Equal(t, "Autoresearch hypothesis tournaments", ideas[0].Title)
	assert.Contains(t, ideas[0].RelatedFilesOrAreas, "cmd/atteler/cli_autoresearch.go")

	report := readFile(t, filepath.Join(result.Dir, scoutReportFile))
	assert.Contains(t, report, "Focus area: `autoresearch`.")
}

func TestRun_TournamentAreaAutoresearchPrioritizesAutoresearchIdea(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(root, "AGENTS.md"), []byte("# Agent rules\nRun tests before changes.\n"), 0o600))
	require.NoError(t, os.WriteFile(filepath.Join(root, "README.md"), []byte("# Demo CLI\nAutoresearch workflows validate implementation hypotheses.\n"), 0o600))

	result, err := Run(t.Context(), RunRequest{
		Prompt:       "Find 3 feature ideas",
		Root:         root,
		Area:         "autoresearch",
		Tournament:   true,
		VariantCount: 2,
		Now:          time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC),
	})
	require.NoError(t, err)

	ideas := readIdeas(t, filepath.Join(result.Dir, ideasFile))
	require.NotEmpty(t, ideas)
	assert.Equal(t, "Autoresearch hypothesis tournaments", ideas[0].Title)
	assert.Contains(t, ideas[0].SourceVariants, "user-value")
}

func TestRankIdeas_MarksSpeculativeBeforeScoring(t *testing.T) {
	t.Parallel()

	ideas := rankIdeas([]Idea{
		{
			Title:      "Speculative idea",
			Fit:        labelHigh,
			Complexity: labelLow,
			Risk:       labelLow,
		},
		{
			Title:      "Evidence-backed idea",
			Fit:        labelHigh,
			Complexity: labelLow,
			Risk:       labelLow,
			Evidence:   []Evidence{{Kind: sourceTypeRepository, Path: "README.md"}},
		},
	}, 0)

	require.Len(t, ideas, 2)
	assert.Equal(t, "Evidence-backed idea", ideas[0].Title)
	assert.False(t, ideas[0].Speculative)
	assert.Equal(t, "Speculative idea", ideas[1].Title)
	assert.True(t, ideas[1].Speculative)
	assert.Less(t, ideas[1].Score, ideas[0].Score)
}

func TestInferCapabilitiesDetectsScoutCommandSurface(t *testing.T) {
	t.Parallel()

	capabilities := inferCapabilities(nil, nil, []string{"cmd/atteler/cli_scout_commands.go"})

	assert.Contains(t, capabilities, "product discovery and roadmap generation")
}

func TestInferCapabilitiesUsesRepositoryContent(t *testing.T) {
	t.Parallel()

	capabilities := inferCapabilities([]repositoryFile{
		{
			Path:    "README.md",
			Title:   "Project overview",
			Summary: "CLI overview",
			Content: "This project documents memory workflows and plugin execution.",
		},
	}, nil, nil)

	assert.Contains(t, capabilities, "local memory and retrieval")
	assert.Contains(t, capabilities, "plugin execution and policy")
}

func TestGenerateIdeasImprovesExistingScoutSurface(t *testing.T) {
	t.Parallel()

	ideas := generateIdeas(RunRequest{Prompt: "Find feature ideas"}, projectContext{
		Packages:     []string{"pkg/scout"},
		CommandFiles: []string{"cmd/atteler/cli_scout_commands.go"},
	}, nil)

	require.NotEmpty(t, ideas)
	assert.Equal(t, "Improve scout product-discovery workflow", ideas[0].Title)
	assert.Contains(t, ideas[0].SuggestedMVP, "preserving the existing")
}

func TestNormalizeCompetitorsNamesProductURLsByHost(t *testing.T) {
	t.Parallel()

	competitors := normalizeCompetitors([]string{
		"https://docs.cursor.com/context/rules",
		"https://github.com/All-Hands-AI/OpenHands",
	}, time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC))

	require.Len(t, competitors, 2)
	assert.Equal(t, "docs.cursor.com", competitors[0].Name)
	assert.Equal(t, "https://docs.cursor.com/context/rules", competitors[0].URL)
	assert.Equal(t, "OpenHands", competitors[1].Name)
}

func TestSourceSummarySkipsFrontMatterDelimiters(t *testing.T) {
	t.Parallel()

	summary := sourceSummary("---\n# Product overview\nAtteler helps agents.\n")

	assert.Equal(t, "Product overview", summary)
}

func TestRenderTournamentSectionEscapesMarkdownTableCells(t *testing.T) {
	t.Parallel()

	var b strings.Builder
	renderTournamentSection(&b, []roadmapRecord{
		{
			Name:  "Value | feasibility",
			Lens:  "line one\nline two | risk",
			Ideas: []string{"Idea"},
		},
	}, []tournamentDecisionRecord{
		{
			Rank:           1,
			Decision:       "kept | merged",
			Title:          "Idea | with pipe",
			Score:          42,
			Rationale:      "score=42\nfit=4 | evidence=2",
			SourceVariants: []string{"value|lane"},
		},
	}, runRecord{
		Tournament: tournamentRunRecord{
			Enabled:       true,
			Variants:      1,
			SharedPackage: "pkg/tournament",
		},
	})

	report := b.String()
	assert.Contains(t, report, "| Value \\| feasibility | line one line two \\| risk | 1 |")
	assert.Contains(t, report, "| 1 | kept \\| merged | Idea \\| with pipe | 42 | score=42 fit=4 \\| evidence=2; variants=value\\|lane |")
	assert.NotContains(t, report, "score=42\nfit=4")
}

func TestRun_ReadsSupportedHarnessGuidanceFiles(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(root, "README.md"), []byte("# Demo CLI\n"), 0o600))
	require.NoError(t, os.WriteFile(filepath.Join(root, "CLAUDE.md"), []byte("# Claude rules\n"), 0o600))
	require.NoError(t, os.WriteFile(filepath.Join(root, "GEMINI.md"), []byte("# Gemini rules\n"), 0o600))
	require.NoError(t, os.WriteFile(filepath.Join(root, "CODEX.md"), []byte("# Codex rules\n"), 0o600))
	require.NoError(t, os.WriteFile(filepath.Join(root, ".cursorrules"), []byte("keep edits focused\n"), 0o600))
	require.NoError(t, os.WriteFile(filepath.Join(root, ".windsurfrules"), []byte("prefer small diffs\n"), 0o600))
	require.NoError(t, os.WriteFile(filepath.Join(root, ".clinerules"), []byte("cite sources\n"), 0o600))
	require.NoError(t, os.MkdirAll(filepath.Join(root, ".windsurf", "rules"), 0o750))
	require.NoError(t, os.WriteFile(filepath.Join(root, ".windsurf", "rules", "testing.md"), []byte("# Testing\nRun focused tests.\n"), 0o600))
	require.NoError(t, os.MkdirAll(filepath.Join(root, ".cline", "rules"), 0o750))
	require.NoError(t, os.WriteFile(filepath.Join(root, ".cline", "rules", "workflow.md"), []byte("# Workflow\nValidate changes.\n"), 0o600))
	require.NoError(t, os.MkdirAll(filepath.Join(root, ".roo", "rules"), 0o750))
	require.NoError(t, os.WriteFile(filepath.Join(root, ".roo", "rules", "review.md"), []byte("# Review\nKeep evidence.\n"), 0o600))
	require.NoError(t, os.MkdirAll(filepath.Join(root, ".codex", "prompts"), 0o750))
	require.NoError(t, os.MkdirAll(filepath.Join(root, ".codex", "skills", "demo"), 0o750))
	require.NoError(t, os.MkdirAll(filepath.Join(root, ".codex", "plugins", "cache"), 0o750))
	require.NoError(t, os.WriteFile(filepath.Join(root, ".codex", "instructions.md"), []byte("# Codex project instructions\n"), 0o600))
	require.NoError(t, os.WriteFile(filepath.Join(root, ".codex", "prompts", "review.md"), []byte("# Review prompt\n"), 0o600))
	require.NoError(t, os.WriteFile(filepath.Join(root, ".codex", "skills", "demo", "SKILL.md"), []byte("# Demo skill\n"), 0o600))
	require.NoError(t, os.WriteFile(filepath.Join(root, ".codex", "plugins", "cache", "ignored.md"), []byte("# Generated cache\n"), 0o600))
	require.NoError(t, os.MkdirAll(filepath.Join(root, ".agents"), 0o750))
	require.NoError(t, os.WriteFile(filepath.Join(root, ".agents", "reviewer.md"), []byte("# Reviewer agent\n"), 0o600))
	require.NoError(t, os.MkdirAll(filepath.Join(root, ".github"), 0o750))
	require.NoError(t, os.WriteFile(filepath.Join(root, ".github", "copilot-instructions.md"), []byte("# Copilot rules\n"), 0o600))

	result, err := Run(t.Context(), RunRequest{
		Prompt: "Find feature ideas",
		Root:   root,
		Now:    time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC),
	})
	require.NoError(t, err)

	var record runRecord
	require.NoError(t, json.Unmarshal([]byte(readFile(t, filepath.Join(result.Dir, runFile))), &record))
	assert.Contains(t, record.GuidanceFiles, "CLAUDE.md")
	assert.Contains(t, record.GuidanceFiles, "GEMINI.md")
	assert.Contains(t, record.GuidanceFiles, "CODEX.md")
	assert.Contains(t, record.GuidanceFiles, ".cursorrules")
	assert.Contains(t, record.GuidanceFiles, ".windsurfrules")
	assert.Contains(t, record.GuidanceFiles, ".clinerules")
	assert.Contains(t, record.GuidanceFiles, ".windsurf/rules/testing.md")
	assert.Contains(t, record.GuidanceFiles, ".cline/rules/workflow.md")
	assert.Contains(t, record.GuidanceFiles, ".roo/rules/review.md")
	assert.Contains(t, record.GuidanceFiles, ".codex/instructions.md")
	assert.Contains(t, record.GuidanceFiles, ".codex/prompts/review.md")
	assert.Contains(t, record.GuidanceFiles, ".codex/skills/demo/SKILL.md")
	assert.NotContains(t, record.GuidanceFiles, ".codex/plugins/cache/ignored.md")
	assert.Contains(t, record.GuidanceFiles, ".agents/reviewer.md")
	assert.Contains(t, record.GuidanceFiles, ".github/copilot-instructions.md")
}

func TestRun_RequiresPrompt(t *testing.T) {
	t.Parallel()

	_, err := Run(t.Context(), RunRequest{Root: t.TempDir()})

	require.Error(t, err)
	assert.Contains(t, err.Error(), "prompt is required")
}

func readFile(t *testing.T, path string) string {
	t.Helper()

	data, err := os.ReadFile(path)
	require.NoError(t, err)

	return string(data)
}

func readIdeas(t *testing.T, path string) []Idea {
	t.Helper()

	file, err := os.Open(path)
	require.NoError(t, err)
	defer file.Close()

	var out []Idea
	scanner := bufio.NewScanner(file)
	for scanner.Scan() {
		var idea Idea
		require.NoError(t, json.Unmarshal(scanner.Bytes(), &idea))
		out = append(out, idea)
	}
	require.NoError(t, scanner.Err())

	return out
}

func readCompetitors(t *testing.T, path string) []Competitor {
	t.Helper()

	file, err := os.Open(path)
	require.NoError(t, err)
	defer file.Close()

	var out []Competitor
	scanner := bufio.NewScanner(file)
	for scanner.Scan() {
		var competitor Competitor
		require.NoError(t, json.Unmarshal(scanner.Bytes(), &competitor))
		out = append(out, competitor)
	}
	require.NoError(t, scanner.Err())

	return out
}

func readRoadmaps(t *testing.T, path string) []roadmapRecord {
	t.Helper()

	file, err := os.Open(path)
	require.NoError(t, err)
	defer file.Close()

	var out []roadmapRecord
	scanner := bufio.NewScanner(file)
	for scanner.Scan() {
		var roadmap roadmapRecord
		require.NoError(t, json.Unmarshal(scanner.Bytes(), &roadmap))
		out = append(out, roadmap)
	}
	require.NoError(t, scanner.Err())

	return out
}
