package main

import (
	"context"
	"flag"
	"io"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/tommoulard/atteler/pkg/autonomy"
)

func TestRunScoutCommandCreatesArtifacts(t *testing.T) { //nolint:paralleltest // captures process-global stdout.
	root := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(root, "README.md"), []byte("# Atteler\nGo CLI harness.\n"), 0o600))
	require.NoError(t, os.WriteFile(filepath.Join(root, "AGENTS.md"), []byte("# Rules\nUse tests and citations.\n"), 0o600))

	stdout := captureProcessOutput(t, &os.Stdout)
	err := runScoutCommandWithAutonomy(context.Background(), root, scoutCommandInput{
		Prompt:        "Find features to add",
		OutputDir:     "scout/out",
		Competitors:   []string{"cursor", "codex"},
		Area:          "ux",
		GenerateTasks: true,
		Tournament:    true,
		Variants:      3,
	}, autonomy.Medium)
	require.NoError(t, err)

	firstLine := requireLineBefore(t, stdout.lines, time.Second)
	assert.Contains(t, firstLine, "Scout run out written to")

	runDir := filepath.Join(root, "scout", "out")
	assert.FileExists(t, filepath.Join(runDir, "scout.md"))
	assert.FileExists(t, filepath.Join(runDir, "ideas.jsonl"))
	assert.FileExists(t, filepath.Join(runDir, "competitors.jsonl"))
	assert.FileExists(t, filepath.Join(runDir, "tasks.generated.yaml"))
	assert.FileExists(t, filepath.Join(runDir, "run.json"))
}

func TestScoutGroupedCommandRunsProviderless(t *testing.T) { //nolint:paralleltest // mutates process-global cwd, args, flag set, and stdout.
	root := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(root, "README.md"), []byte("# Atteler\nGo CLI harness.\n"), 0o600))
	require.NoError(t, os.WriteFile(filepath.Join(root, "AGENTS.md"), []byte("# Rules\nUse tests and citations.\n"), 0o600))

	oldArgs := os.Args
	oldCommandLine := flag.CommandLine

	t.Cleanup(func() {
		os.Args = oldArgs
		flag.CommandLine = oldCommandLine
	})

	t.Chdir(root)

	flag.CommandLine = flag.NewFlagSet("atteler", flag.ContinueOnError)
	flag.CommandLine.SetOutput(io.Discard)

	os.Args = []string{
		"atteler",
		"--session-dir", filepath.Join(root, "sessions"),
		"--autonomy", "medium",
		"scout", "run",
		"--scout-output", "scout/grouped",
		"--generate-tasks",
		"Find", "features",
	}

	output := captureSessionCommandStdout(t, func() {
		require.NoError(t, run(context.Background()))
	})

	assert.Contains(t, output, "Scout run grouped written to")
	assert.FileExists(t, filepath.Join(root, "scout", "grouped", "scout.md"))
	assert.FileExists(t, filepath.Join(root, "scout", "grouped", "tasks.generated.yaml"))
}

func TestRunScoutCommandHonorsAutonomy(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	err := runScoutCommandWithAutonomy(context.Background(), root, scoutCommandInput{
		Prompt:    "Find features to add",
		OutputDir: "scout/out",
	}, autonomy.Low)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "autonomy low blocks file writes")
	assert.NoFileExists(t, filepath.Join(root, "scout", "out", "scout.md"))
}

func TestScoutCommandInputUsesOutputFlagAsPath(t *testing.T) {
	t.Parallel()

	input := scoutCommandInputFromOptions(cliOptions{
		scoutRunPrompt:        "Find features to add",
		outputFormat:          ".atteler/scout/features",
		researchGenerateTasks: true,
		tournament:            true,
		variants:              positiveIntFlag{value: 5, set: true},
	})

	assert.Equal(t, ".atteler/scout/features", input.OutputDir)
	assert.True(t, input.GenerateTasks)
	assert.True(t, input.Tournament)
	assert.Equal(t, 5, input.Variants)
}

func TestValidateScoutCommandSelection_RequiresScoutForAdjuncts(t *testing.T) {
	t.Parallel()

	err := validateScoutCommandSelection(cliOptions{scoutCompetitors: stringListFlag{"cursor"}})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "require --scout-run")
}

func TestValidateScoutCommandSelection_AllowsTournamentForAutoresearch(t *testing.T) {
	t.Parallel()

	err := validateScoutCommandSelection(cliOptions{autoresearch: true, tournament: true})
	require.NoError(t, err)
}
