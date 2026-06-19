//nolint:wsl_v5 // Tests keep setup/assertion blocks compact for artifact readability.
package scout

import (
	"bufio"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestRun_CreatesRoadmapArtifactsWithGuidanceAndTournament(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(root, "README.md"), []byte("# Atteler\nA Go LLM harness for terminal workflows.\n"), 0o600))
	require.NoError(t, os.WriteFile(filepath.Join(root, "AGENTS.md"), []byte("# Agent rules\nAdd tests before changes and cite evidence.\n"), 0o600))
	require.NoError(t, os.WriteFile(filepath.Join(root, "CLAUDE.md"), []byte("# Claude rules\nPrefer small tasks.\n"), 0o600))
	require.NoError(t, os.WriteFile(filepath.Join(root, "notes.md"), []byte("# Product notes\nUsers want cited roadmap reports and importable tasks.\n"), 0o600))
	require.NoError(t, os.MkdirAll(filepath.Join(root, ".cursor", "rules"), 0o750))
	require.NoError(t, os.WriteFile(filepath.Join(root, ".cursor", "rules", "style.mdc"), []byte("# Cursor style\nKeep diffs reviewable.\n"), 0o600))
	require.NoError(t, os.MkdirAll(filepath.Join(root, ".codex", "prompts"), 0o750))
	require.NoError(t, os.WriteFile(filepath.Join(root, ".codex", "prompts", "executor.md"), []byte("# Codex prompt\nRespect project guidance.\n"), 0o600))
	require.NoError(t, os.WriteFile(filepath.Join(root, ".windsurfrules"), []byte("# Windsurf rules\nPrefer evidence-first recommendations.\n"), 0o600))
	require.NoError(t, os.WriteFile(filepath.Join(root, "Makefile"), []byte("test:\n\tgo test ./...\n"), 0o600))
	require.NoError(t, os.MkdirAll(filepath.Join(root, "cmd", "atteler"), 0o750))
	require.NoError(t, os.MkdirAll(filepath.Join(root, "pkg", "research"), 0o750))
	require.NoError(t, os.MkdirAll(filepath.Join(root, "docs"), 0o750))

	result, err := Run(context.Background(), RunRequest{
		Prompt:        "Find the best next features for Atteler",
		Root:          root,
		OutputDir:     "scout/out",
		Area:          "autoresearch",
		Competitors:   []string{"cursor", "https://docs.github.com/copilot"},
		Sources:       []string{"notes.md"},
		GenerateTasks: true,
		Tournament:    true,
		Variants:      4,
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
	assert.Contains(t, report, "## Inspiration sources")
	assert.Contains(t, report, "## Ranked feature ideas")
	assert.Contains(t, report, "## Tournament comparison")
	assert.Contains(t, report, "AGENTS.md")
	assert.Contains(t, report, "CLAUDE.md")
	assert.Contains(t, report, "notes.md")
	assert.Contains(t, report, ".cursor/rules/style.mdc")
	assert.Contains(t, report, ".codex/prompts/executor.md")
	assert.Contains(t, report, ".windsurfrules")
	assert.Contains(t, report, "Atteler scout recommendations should cite evidence")
	assert.NotContains(t, report, ".; validate")

	ideas := readIdeas(t, filepath.Join(result.Dir, ideasFile))
	require.NotEmpty(t, ideas)
	assert.Contains(t, ideaTitlesForTest(ideas), "Evidence-backed discovery reports")
	assert.GreaterOrEqual(t, ideas[0].Score, ideas[len(ideas)-1].Score)
	assert.True(t, hasEvidencePath(ideas, "AGENTS.md"), "expected guidance evidence in ideas: %#v", ideas)
	assert.True(t, hasEvidencePath(ideas, "notes.md"), "expected scout-source evidence in ideas: %#v", ideas)

	competitors := readCompetitors(t, filepath.Join(result.Dir, competitorsFile))
	require.Len(t, competitors, 2)
	assert.Equal(t, "cursor", competitors[0].Name)
	assert.True(t, competitors[0].Speculative)
	assert.Equal(t, "https://docs.github.com/copilot", competitors[1].URL)
	assert.False(t, competitors[1].Speculative)

	tasks := readFile(t, filepath.Join(result.Dir, tasksFile))
	assert.Contains(t, tasks, "tasks:")
	assert.Contains(t, tasks, "make test")
	assert.Contains(t, tasks, "source_run: \"out\"")

	var record runRecord
	require.NoError(t, json.Unmarshal([]byte(readFile(t, filepath.Join(result.Dir, runFile))), &record))
	assert.Equal(t, SchemaVersion, record.Schema)
	assert.Equal(t, "Find the best next features for Atteler", record.Prompt)
	assert.Equal(t, 4, record.Tournament.Variants)
	assert.True(t, record.Tournament.Enabled)
	require.Len(t, record.Variants, 4)
	assert.Contains(t, guidancePaths(record.GuidanceFiles), "AGENTS.md")
	assert.Contains(t, guidancePaths(record.GuidanceFiles), ".codex/prompts/executor.md")
	assert.Contains(t, guidancePaths(record.GuidanceFiles), ".windsurfrules")
}

func TestRun_DefaultOutputDirUsesScoutRunsRoot(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	result, err := Run(context.Background(), RunRequest{
		Prompt: "Find features to add",
		Root:   root,
		Now:    time.Date(2026, 1, 2, 3, 4, 5, 6, time.UTC),
	})
	require.NoError(t, err)

	assert.Contains(t, result.Dir, filepath.Join(root, ".atteler", "runs", "scout"))
	assert.FileExists(t, filepath.Join(result.Dir, scoutReportFile))
	assert.NotEmpty(t, result.RunID)
}

func TestRun_GeneratedTasksReflectLintGuidanceWhenMakefileSupportsIt(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(root, "AGENTS.md"), []byte("# Rules\nRun tests and lint before finishing.\n"), 0o600))
	require.NoError(t, os.WriteFile(filepath.Join(root, "Makefile"), []byte("test:\n\tgo test ./...\n\nlint:\n\tgolangci-lint run\n"), 0o600))

	result, err := Run(context.Background(), RunRequest{
		Prompt:        "Find feature ideas",
		Root:          root,
		OutputDir:     "scout/out",
		GenerateTasks: true,
		Now:           time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC),
	})
	require.NoError(t, err)

	tasks := readFile(t, filepath.Join(result.Dir, tasksFile))
	assert.Contains(t, tasks, "make test && make lint")

	var record runRecord
	require.NoError(t, json.Unmarshal([]byte(readFile(t, filepath.Join(result.Dir, runFile))), &record))
	assert.Contains(t, record.OutputDir, filepath.Join("scout", "out"))
}

func TestRun_RunMetadataIncludesPromptURLsAsCompetitorInputs(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	result, err := Run(context.Background(), RunRequest{
		Prompt: "Compare roadmap ideas inspired by https://example.com/product.",
		Root:   root,
		Now:    time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC),
	})
	require.NoError(t, err)

	var record runRecord
	require.NoError(t, json.Unmarshal([]byte(readFile(t, filepath.Join(result.Dir, runFile))), &record))
	assert.Contains(t, record.Competitors, "https://example.com/product")
	assert.Len(t, record.Competitors, record.CompetitorCount)
}

func TestRun_RedactsSensitiveEvidenceAndURLMetadata(t *testing.T) {
	t.Parallel()

	secret := "sk-1234567890abcdefSECRET"
	urlSecret := "super-secret-query-token"
	root := filepath.Join(t.TempDir(), "repo-"+secret)
	require.NoError(t, os.MkdirAll(root, 0o750))
	require.NoError(t, os.WriteFile(filepath.Join(root, "AGENTS.md"), []byte("OPENAI_API_KEY="+secret+"\nRun tests.\n"), 0o600))
	require.NoError(t, os.WriteFile(filepath.Join(root, "notes.md"), []byte("access_token="+secret+"\nFeature notes.\n"), 0o600))

	result, err := Run(context.Background(), RunRequest{
		Prompt:    "Compare ideas from https://example.com/product?api_key=" + urlSecret,
		Root:      root,
		OutputDir: "out-" + secret,
		Area:      "auth_token=" + secret,
		Sources:   []string{"notes.md"},
		Now:       time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC),
	})
	require.NoError(t, err)

	artifacts := strings.Join([]string{
		readFile(t, filepath.Join(result.Dir, scoutReportFile)),
		readFile(t, filepath.Join(result.Dir, ideasFile)),
		readFile(t, filepath.Join(result.Dir, competitorsFile)),
		readFile(t, filepath.Join(result.Dir, runFile)),
	}, "\n")
	assert.NotContains(t, artifacts, secret)
	assert.NotContains(t, artifacts, urlSecret)
	assert.Contains(t, artifacts, "[REDACTED]")
}

func TestRun_RequiresPrompt(t *testing.T) {
	t.Parallel()

	_, err := Run(context.Background(), RunRequest{Root: t.TempDir()})
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

func ideaTitlesForTest(ideas []Idea) []string {
	out := make([]string, 0, len(ideas))
	for i := range ideas {
		out = append(out, ideas[i].Title)
	}

	return out
}

func hasEvidencePath(ideas []Idea, want string) bool {
	for i := range ideas {
		for _, evidence := range ideas[i].Evidence {
			if evidence.Path == want || strings.HasSuffix(evidence.Path, "/"+want) {
				return true
			}
		}
	}

	return false
}

func guidancePaths(guidance []GuidanceFile) []string {
	out := make([]string, 0, len(guidance))
	for _, file := range guidance {
		out = append(out, file.Path)
	}

	return out
}
