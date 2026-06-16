package main

import (
	"os"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/tommoulard/atteler/pkg/agent"
	"github.com/tommoulard/atteler/pkg/autonomy"
	"github.com/tommoulard/atteler/pkg/autopilot"
	appconfig "github.com/tommoulard/atteler/pkg/config"
	"github.com/tommoulard/atteler/pkg/llm"
)

func TestAutonomyFromConfigOptions_AutoRaisesFloorToMedium(t *testing.T) {
	t.Parallel()

	var opts cliOptions
	require.NoError(t, opts.autonomy.Set("low"))
	require.NoError(t, opts.auto.Set(""))

	level, err := autonomyFromConfigOptions(appconfig.Config{}, opts)
	require.NoError(t, err)
	assert.Equal(t, autonomy.Medium, level)
}

func TestAutonomyFromConfigOptions_AutoLeavesToolLevelUntouched(t *testing.T) {
	t.Parallel()

	var opts cliOptions
	require.NoError(t, opts.autonomy.Set("high"))
	require.NoError(t, opts.auto.Set(""))

	level, err := autonomyFromConfigOptions(appconfig.Config{}, opts)
	require.NoError(t, err)
	assert.Equal(t, autonomy.High, level)
}

func TestResolveAutoModePlan_InactiveWhenUnset(t *testing.T) {
	t.Parallel()

	plan, err := resolveAutoModePlan(cliOptions{}, appconfig.Config{})
	require.NoError(t, err)
	assert.False(t, plan.active)
	assert.False(t, plan.downgraded)
}

func TestResolveAutoModePlan_ActiveForValidMode(t *testing.T) {
	t.Setenv("ATTELER_AUTO_DEPTH", "")

	opts := cliOptions{autoMaxDepth: 2, auto: autoFlag{value: "bug-hunt", set: true}}

	plan, err := resolveAutoModePlan(opts, appconfig.Config{})
	require.NoError(t, err)
	assert.True(t, plan.active)
	assert.Equal(t, "bug-hunt", plan.mode.Name)
	assert.Equal(t, 0, plan.currentDepth)
}

func TestResolveAutoModePlan_UnknownModeErrors(t *testing.T) {
	t.Parallel()

	opts := cliOptions{autoMaxDepth: 2, auto: autoFlag{value: "nope", set: true}}

	_, err := resolveAutoModePlan(opts, appconfig.Config{})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "unknown auto mode")
}

func TestResolveAutoModePlan_DowngradesAtDepthCap(t *testing.T) {
	t.Setenv("ATTELER_AUTO_DEPTH", "2")

	opts := cliOptions{autoMaxDepth: 2, auto: autoFlag{value: "auto", set: true}}

	plan, err := resolveAutoModePlan(opts, appconfig.Config{})
	require.NoError(t, err)
	assert.False(t, plan.active)
	assert.True(t, plan.downgraded)
	assert.Equal(t, 2, plan.currentDepth)
}

func TestAutoModeRequest_FlagWinsOverConfig(t *testing.T) {
	t.Parallel()

	opts := cliOptions{auto: autoFlag{value: "bug-hunt", set: true}}
	mode, requested := autoModeRequest(opts, appconfig.Config{Auto: "auto"})
	require.True(t, requested)
	assert.Equal(t, "bug-hunt", mode)
}

func TestAutoModeRequest_ConfigDefaultInteractiveOnly(t *testing.T) {
	t.Parallel()

	cfg := appconfig.Config{Auto: "auto"}

	// Interactive run: config default applies.
	mode, requested := autoModeRequest(cliOptions{}, cfg)
	require.True(t, requested)
	assert.Equal(t, "auto", mode)

	// Headless run: config default does NOT apply (stays opt-in via --auto).
	_, requested = autoModeRequest(cliOptions{headless: true}, cfg)
	assert.False(t, requested)
}

func TestResolveAutoModePlan_ConfigDefaultActivatesInteractive(t *testing.T) {
	t.Setenv("ATTELER_AUTO_DEPTH", "")

	plan, err := resolveAutoModePlan(cliOptions{autoMaxDepth: 2}, appconfig.Config{Auto: "bug-hunt"})
	require.NoError(t, err)
	assert.True(t, plan.active)
	assert.Equal(t, "bug-hunt", plan.mode.Name)
}

func TestResolveAutoModePlan_ConfigDefaultIgnoredInHeadless(t *testing.T) {
	t.Setenv("ATTELER_AUTO_DEPTH", "")

	plan, err := resolveAutoModePlan(cliOptions{autoMaxDepth: 2, headless: true}, appconfig.Config{Auto: "auto"})
	require.NoError(t, err)
	assert.False(t, plan.active)
}

func TestApplyAutoMode_RegistersOrchestratorSelectsAndSetsDepth(t *testing.T) {
	t.Setenv("ATTELER_AUTO_DEPTH", "0")

	mode, ok := autopilot.ModeByName(autopilot.DefaultMode)
	require.True(t, ok)

	plan := autoModePlan{mode: mode, active: true, currentDepth: 0, maxDepth: 2}
	registry := agent.NewRegistry(nil)

	var selection selectionState

	err := applyAutoMode(plan, registry, llm.NewRegistry(), &selection, autonomy.Medium)
	require.NoError(t, err)

	assert.Equal(t, autopilot.OrchestratorAgentName, selection.selectedAgent)
	assert.Equal(t, autopilot.OrchestratorAgentName, selection.sessionState.DefaultAgent)

	orchestrator, ok := registry.Get(autopilot.OrchestratorAgentName)
	require.True(t, ok)
	assert.Contains(t, orchestrator.SystemPrompt, "Self-Fork Orchestration")

	for _, name := range autopilot.WorkerAgentNames() {
		_, ok := registry.Get(name)
		assert.True(t, ok, "worker %q should be registered", name)
	}

	// Children inherit the incremented depth.
	assert.Equal(t, "1", os.Getenv("ATTELER_AUTO_DEPTH"))
}

func TestApplyAutoMode_NoopWhenInactive(t *testing.T) {
	t.Parallel()

	registry := agent.NewRegistry(nil)

	var selection selectionState

	err := applyAutoMode(autoModePlan{}, registry, llm.NewRegistry(), &selection, autonomy.Medium)
	require.NoError(t, err)
	assert.Empty(t, selection.selectedAgent)

	_, ok := registry.Get(autopilot.OrchestratorAgentName)
	assert.False(t, ok)
}
