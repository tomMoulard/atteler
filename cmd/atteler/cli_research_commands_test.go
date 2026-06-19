package main

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/tommoulard/atteler/pkg/autonomy"
)

func TestRunResearchCommandCreatesArtifacts(t *testing.T) { //nolint:paralleltest // captures process-global stdout.
	root := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(root, "AGENTS.md"), []byte("# Rules\nUse citations.\n"), 0o600))

	stdout := captureProcessOutput(t, &os.Stdout)
	err := runResearchCommandWithAutonomy(context.Background(), root, researchCommandInput{
		Question:       "Compare plugin sandboxing approaches",
		OutputDir:      "research/out",
		TrustedSources: []string{"go.dev"},
		Sources:        []string{"AGENTS.md"},
		GenerateTasks:  true,
	}, autonomy.Medium)
	require.NoError(t, err)

	firstLine := requireLineBefore(t, stdout.lines, time.Second)
	assert.Contains(t, firstLine, "Research run out written to")

	runDir := filepath.Join(root, "research", "out")
	assert.FileExists(t, filepath.Join(runDir, "research.md"))
	assert.FileExists(t, filepath.Join(runDir, "sources.jsonl"))
	assert.FileExists(t, filepath.Join(runDir, "claims.jsonl"))
	assert.FileExists(t, filepath.Join(runDir, "tasks.generated.yaml"))
	assert.FileExists(t, filepath.Join(runDir, "run.json"))
}

func TestRunResearchCommandHonorsAutonomy(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	err := runResearchCommandWithAutonomy(context.Background(), root, researchCommandInput{
		Question:  "Compare plugin sandboxing approaches",
		OutputDir: "research/out",
	}, autonomy.Low)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "autonomy low blocks file writes")
	assert.NoFileExists(t, filepath.Join(root, "research", "out", "research.md"))
}

func TestResearchCommandInputUsesOutputFlagAsPath(t *testing.T) {
	t.Parallel()

	input := researchCommandInputFromOptions(cliOptions{
		researchRunQuestion:   "Research safe worktrees",
		outputFormat:          ".atteler/research/worktrees",
		researchGenerateTasks: true,
	})

	assert.Equal(t, ".atteler/research/worktrees", input.OutputDir)
	assert.True(t, input.GenerateTasks)
}
