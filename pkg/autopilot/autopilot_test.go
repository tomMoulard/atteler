package autopilot_test

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/tommoulard/atteler/pkg/autopilot"
)

func TestModeByName_DefaultsAndUnknown(t *testing.T) {
	t.Parallel()

	mode, ok := autopilot.ModeByName("")
	require.True(t, ok)
	assert.Equal(t, autopilot.DefaultMode, mode.Name)

	_, ok = autopilot.ModeByName("does-not-exist")
	assert.False(t, ok)

	bug, ok := autopilot.ModeByName("bug-hunt")
	require.True(t, ok)
	assert.Equal(t, "bug-hunt", bug.Name)

	research, ok := autopilot.ModeByName("autoresearch")
	require.True(t, ok)
	assert.Contains(t, research.Playbook, "results.tsv")
	assert.Contains(t, research.Playbook, "KEEP")
}

func TestRenderSystemPrompt_IncludesBinaryWorkersAndModels(t *testing.T) {
	t.Parallel()

	mode, ok := autopilot.ModeByName(autopilot.DefaultMode)
	require.True(t, ok)

	prompt := autopilot.RenderSystemPrompt(mode, autopilot.ManualInput{
		BinaryPath:   "/usr/local/bin/atteler",
		Autonomy:     "high",
		WorkerAgents: []string{"explorer", "reviewer"},
		Models:       []string{"claude-opus-4-8", "gpt-5"},
		CurrentDepth: 0,
		MaxDepth:     2,
	})

	assert.Contains(t, prompt, "/usr/local/bin/atteler --headless --once")
	assert.Contains(t, prompt, "--output-format json")
	assert.Contains(t, prompt, "--autonomy high")
	assert.Contains(t, prompt, "- explorer")
	assert.Contains(t, prompt, "- reviewer")
	assert.Contains(t, prompt, "- claude-opus-4-8")
	assert.Contains(t, prompt, "- gpt-5")
}

func TestRenderSystemPrompt_AppendsModePlaybook(t *testing.T) {
	t.Parallel()

	mode, ok := autopilot.ModeByName("bug-hunt")
	require.True(t, ok)

	prompt := autopilot.RenderSystemPrompt(mode, autopilot.ManualInput{BinaryPath: "atteler"})

	assert.Contains(t, prompt, "## Playbook: bug-hunt")
	assert.Contains(t, prompt, strings.TrimSpace(mode.Playbook))
}

func TestRenderSystemPrompt_StatesDepthCap(t *testing.T) {
	t.Parallel()

	mode, _ := autopilot.ModeByName(autopilot.DefaultMode)
	prompt := autopilot.RenderSystemPrompt(mode, autopilot.ManualInput{
		BinaryPath:   "atteler",
		CurrentDepth: 1,
		MaxDepth:     3,
	})

	assert.Contains(t, prompt, "depth 1 of a maximum 3")
	assert.Contains(t, prompt, "NEVER spawn a child with")
}

func TestRenderSystemPrompt_FallsBackWhenEmpty(t *testing.T) {
	t.Parallel()

	mode, _ := autopilot.ModeByName(autopilot.DefaultMode)
	prompt := autopilot.RenderSystemPrompt(mode, autopilot.ManualInput{})

	// Binary, autonomy, and lists fall back to sensible defaults.
	assert.Contains(t, prompt, "atteler --headless --once")
	assert.Contains(t, prompt, "--autonomy medium")
	assert.Contains(t, prompt, "(none available)")
}

func TestWorkerAgents_HaveBashAccessAndPrompts(t *testing.T) {
	t.Parallel()

	workers := autopilot.WorkerAgents()
	require.NotEmpty(t, workers)

	names := autopilot.WorkerAgentNames()
	assert.Equal(t, []string{"explorer", "implementer", "planner", "reviewer"}, names)

	for _, w := range workers {
		assert.NotEmpty(t, w.SystemPrompt, "worker %q should carry a system prompt", w.Name)
		// nil ToolPermissions means all tools (including bash) pass through.
		assert.Nil(t, w.ToolPermissions, "worker %q should not restrict tools", w.Name)
		assert.True(t, w.HasToolPermission("bash"))
	}
}

func TestOrchestratorAgent_CarriesPrompt(t *testing.T) {
	t.Parallel()

	a := autopilot.OrchestratorAgent("MANUAL-BODY")
	assert.Equal(t, autopilot.OrchestratorAgentName, a.Name)
	assert.Equal(t, "MANUAL-BODY", a.SystemPrompt)
	assert.Nil(t, a.ToolPermissions)
	assert.True(t, a.HasToolPermission("bash"))
}
