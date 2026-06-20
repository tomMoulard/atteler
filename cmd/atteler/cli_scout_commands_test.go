package main

import (
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/tommoulard/atteler/pkg/autonomy"
)

func TestRunScoutCommandWithAutonomy_CreatesArtifacts(t *testing.T) {
	t.Parallel()

	root := t.TempDir()

	err := runScoutCommandWithAutonomy(t.Context(), root, scoutCommandInput{
		Prompt:        "Find 3 feature ideas",
		OutputDir:     "scout/out",
		Competitors:   []string{"cursor"},
		GenerateTasks: true,
		Tournament:    true,
		Variants:      2,
	}, autonomy.High)
	require.NoError(t, err)

	runDir := filepath.Join(root, "scout", "out")
	assert.FileExists(t, filepath.Join(runDir, "scout.md"))
	assert.FileExists(t, filepath.Join(runDir, "ideas.jsonl"))
	assert.FileExists(t, filepath.Join(runDir, "competitors.jsonl"))
	assert.FileExists(t, filepath.Join(runDir, "roadmaps.jsonl"))
	assert.FileExists(t, filepath.Join(runDir, "tasks.generated.yaml"))
}

func TestRunScoutCommandWithAutonomy_DefaultPathWorks(t *testing.T) {
	t.Parallel()

	root := t.TempDir()

	err := runScoutCommandWithAutonomy(t.Context(), root, scoutCommandInput{
		Prompt: "Find 1 feature idea",
	}, autonomy.DefaultLevel)
	require.NoError(t, err)

	matches, err := filepath.Glob(filepath.Join(root, ".atteler", "runs", "scout", "*", "scout.md"))
	require.NoError(t, err)
	require.Len(t, matches, 1)
	assert.FileExists(t, filepath.Join(filepath.Dir(matches[0]), "ideas.jsonl"))
	assert.FileExists(t, filepath.Join(filepath.Dir(matches[0]), "run.json"))
}

func TestRunScoutCommandWithAutonomy_RequiresWriteAutonomy(t *testing.T) {
	t.Parallel()

	root := t.TempDir()

	err := runScoutCommandWithAutonomy(t.Context(), root, scoutCommandInput{
		Prompt:    "Find feature ideas",
		OutputDir: "scout/out",
	}, autonomy.Low)

	require.Error(t, err)
	assert.Contains(t, err.Error(), "autonomy low blocks file writes")
	assert.NoFileExists(t, filepath.Join(root, "scout", "out", "scout.md"))
}

func TestScoutCommandInputUsesOutputFlagAsPath(t *testing.T) {
	t.Parallel()

	input := scoutCommandInputFromOptions(cliOptions{
		scoutRunPrompt: "Find features",
		outputFormat:   ".atteler/scout/features",
		generateTasks:  true,
		tournament:     true,
		variants:       positiveIntFlag{value: 5, set: true},
	})

	assert.Equal(t, ".atteler/scout/features", input.OutputDir)
	assert.True(t, input.GenerateTasks)
	assert.True(t, input.Tournament)
	assert.Equal(t, 5, input.Variants)
}

func TestScoutCommandInputVariantsFlagEnablesTournament(t *testing.T) {
	t.Parallel()

	input := scoutCommandInputFromOptions(cliOptions{
		scoutRunPrompt: "Find features",
		variants:       positiveIntFlag{value: 1, set: true},
	})

	assert.True(t, input.Tournament)
	assert.Equal(t, 1, input.Variants)
}

func TestScoutGroupedCommandParsesScopedFlags(t *testing.T) {
	t.Parallel()

	opts, fs := newCLIOptionsAndFlagSetForTest(t)
	plan := translateCLIArgsWithFlagSet([]string{
		"scout", "run",
		"--area", "autoresearch",
		"--scout-output", ".atteler/scout/autoresearch",
		"--competitors", "cursor,codex",
		"--scout-source", "docs/notes.md",
		"--tournament",
		"--variants", "5",
		"--generate-tasks",
		"Find", "features",
	}, fs)

	require.NoError(t, plan.Err)
	require.False(t, plan.Help)
	require.NoError(t, fs.Parse(plan.Args))

	assert.Equal(t, "Find features", opts.scoutRunPrompt)
	assert.Equal(t, "autoresearch", opts.scoutArea)
	assert.Equal(t, ".atteler/scout/autoresearch", opts.scoutOutputDir)
	assert.Equal(t, stringListFlag{"cursor", "codex"}, opts.scoutCompetitors)
	assert.Equal(t, stringListFlag{"docs/notes.md"}, opts.scoutSources)
	assert.True(t, opts.tournament)
	require.True(t, opts.variants.set)
	assert.Equal(t, 5, opts.variants.value)
	assert.True(t, opts.generateTasks)
}

func TestScoutGroupedCommandAcceptsOutputAlias(t *testing.T) {
	t.Parallel()

	opts, fs := newCLIOptionsAndFlagSetForTest(t)
	plan := translateCLIArgsWithFlagSet([]string{
		"scout", "run",
		"--output", ".atteler/scout/features",
		"Find", "features",
	}, fs)

	require.NoError(t, plan.Err)
	require.False(t, plan.Help)
	require.NoError(t, fs.Parse(plan.Args))

	input := scoutCommandInputFromOptions(*opts)
	assert.Equal(t, "Find features", input.Prompt)
	assert.Equal(t, ".atteler/scout/features", input.OutputDir)
}

func TestValidateScoutCommandSelection_RejectsScoutAdjunctsWithoutScoutRun(t *testing.T) {
	t.Parallel()

	err := validateScoutCommandSelection(cliOptions{
		scoutSources: stringListFlag{"docs/notes.md"},
	})

	require.Error(t, err)
	assert.Contains(t, err.Error(), "require --scout-run")
}

func TestValidateScoutCommandSelection_AllowsTournamentForAutoresearch(t *testing.T) {
	t.Parallel()

	err := validateScoutCommandSelection(cliOptions{
		autoresearch: true,
		tournament:   true,
		variants:     positiveIntFlag{value: 3, set: true},
	})

	require.NoError(t, err)
}
