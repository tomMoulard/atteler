package symphony

import (
	"context"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestDefaultAgentRunner_AfterRunHonorsCanceledContext(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	runner := NewDefaultAgentRunner(nil, slog.New(slog.NewTextHandler(io.Discard, nil)))
	req := RunRequest{
		Config: Config{
			Workspace: WorkspaceConfig{Root: root},
			Hooks: HooksConfig{
				AfterRun: "printf ran > after-run.txt",
				Timeout:  time.Second,
			},
			Agent: AgentConfig{MaxTurns: 1},
		},
		Workflow: WorkflowDefinition{PromptTemplate: "fix the issue"},
		Issue:    Issue{ID: "1", Identifier: "GH-1", Title: "Fix", State: "OPEN"},
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	_, err := runner.Run(ctx, req, nil)
	require.Error(t, err)
	require.ErrorIs(t, err, context.Canceled)

	_, statErr := os.Stat(filepath.Join(root, "GH-1", "after-run.txt"))
	require.ErrorIs(t, statErr, os.ErrNotExist)
}

func TestShouldRunBeforeRunHookSkipsExistingGitCheckoutForPullRequestRework(t *testing.T) {
	t.Parallel()

	workspacePath := t.TempDir()
	require.NoError(t, os.Mkdir(filepath.Join(workspacePath, ".git"), 0o750))

	req := RunRequest{
		Config: Config{
			Hooks: HooksConfig{BeforeRun: "echo prepare"},
		},
		Context: &RunContext{
			Kind:        RunKindPullRequestRework,
			PullRequest: &PullRequestReworkContext{Branch: "symphony/GH-2"},
		},
	}

	assert.False(t, shouldRunBeforeRunHook(req, Workspace{Path: workspacePath}))
}

func TestShouldRunBeforeRunHookStillRunsForFreshPullRequestReworkWorkspace(t *testing.T) {
	t.Parallel()

	req := RunRequest{
		Config: Config{
			Hooks: HooksConfig{BeforeRun: "echo prepare"},
		},
		Context: &RunContext{
			Kind:        RunKindPullRequestRework,
			PullRequest: &PullRequestReworkContext{Branch: "symphony/GH-2"},
		},
	}

	assert.True(t, shouldRunBeforeRunHook(req, Workspace{Path: t.TempDir()}))
}
