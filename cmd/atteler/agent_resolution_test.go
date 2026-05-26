package main

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/tommoulard/atteler/pkg/agent"
	"github.com/tommoulard/atteler/pkg/config"
	"github.com/tommoulard/atteler/pkg/session"
)

func TestResolveAgent_InlineOverridesSelected(t *testing.T) {
	t.Parallel()

	registry := agent.NewRegistry(map[string]config.AgentConfig{
		"default":        {SystemPrompt: "default"},
		testReviewerName: {SystemPrompt: "review"},
	})

	selected, prompt, err := resolveAgent(registry, "default", "@reviewer check this")
	if err != nil {
		require.NoError(t, err)
	}

	if selected.name != testReviewerName {
		assert.Failf(t, "assertion failed", "agent = %q, want reviewer", selected.name)
	}

	if prompt != "check this" {
		assert.Failf(t, "assertion failed", "prompt = %q, want check this", prompt)
	}
}

func TestResolveAgent_Unknown(t *testing.T) {
	t.Parallel()

	_, _, err := resolveAgent(agent.NewRegistry(nil), "", "@missing hi")
	if err == nil {
		require.FailNow(t, "expected unknown agent error")
	}
}

func TestResolveSelection_ExportSkipsUnknownSavedAgent(t *testing.T) {
	t.Parallel()

	const removedAgent = "removed-agent"

	store := session.NewStore(t.TempDir())
	saved := session.New("gpt-test", nil)

	saved.DefaultAgent = removedAgent
	if err := store.Save(saved); err != nil {
		require.NoError(t, err)
	}

	selection, err := resolveSelection(
		cliOptions{exportRef: saved.ID},
		config.Config{},
		"",
		agent.NewRegistry(nil),
		store,
	)
	if err != nil {
		require.NoError(t, err)
	}

	if selection.sessionState.DefaultAgent != removedAgent {
		require.Failf(t, "unexpected failure", "DefaultAgent = %q, want saved agent", selection.sessionState.DefaultAgent)
	}
}

func TestResolveSelection_ShowSkipsUnknownSavedAgent(t *testing.T) {
	t.Parallel()

	const removedAgent = "removed-agent"

	store := session.NewStore(t.TempDir())
	saved := session.New("gpt-test", nil)

	saved.DefaultAgent = removedAgent
	if err := store.Save(saved); err != nil {
		require.NoError(t, err)
	}

	selection, err := resolveSelection(
		cliOptions{showSessionRef: saved.ID},
		config.Config{},
		"",
		agent.NewRegistry(nil),
		store,
	)
	if err != nil {
		require.NoError(t, err)
	}

	if selection.sessionState.DefaultAgent != removedAgent {
		require.Failf(t, "unexpected failure", "DefaultAgent = %q, want saved agent", selection.sessionState.DefaultAgent)
	}
}

func TestResolveSelection_SessionUtilitiesSkipUnknownSavedAgent(t *testing.T) {
	t.Parallel()

	const removedAgent = "removed-agent"

	tests := map[string]func(string) cliOptions{
		"summary":            func(id string) cliOptions { return cliOptions{summarySessionRef: id} },
		"list messages":      func(id string) cliOptions { return cliOptions{sessionRef: id, listMessages: true} },
		"list artifacts":     func(id string) cliOptions { return cliOptions{sessionRef: id, listArtifacts: true} },
		"list evaluations":   func(id string) cliOptions { return cliOptions{sessionRef: id, listEvaluations: true} },
		"list failures":      func(id string) cliOptions { return cliOptions{sessionRef: id, listFailures: true} },
		"record failure":     func(id string) cliOptions { return cliOptions{sessionRef: id, recordFailure: "bad path"} },
		"record evaluation":  func(id string) cliOptions { return cliOptions{sessionRef: id, recordEvaluation: "reviewer"} },
		"record artifact":    func(id string) cliOptions { return cliOptions{sessionRef: id, recordArtifact: "artifact.md"} },
		"feedback proposals": func(id string) cliOptions { return cliOptions{sessionRef: id, feedbackProposals: true} },
		"merge artifacts":    func(id string) cliOptions { return cliOptions{sessionRef: id, mergeArtifactsPath: "-"} },
		"agent memory":       func(id string) cliOptions { return cliOptions{sessionRef: id, agentMemorySearch: "auth"} },
		"agent memory delete": func(id string) cliOptions {
			return cliOptions{sessionRef: id, agentMemoryDelete: "memory-id"}
		},
		"agent memory compact": func(id string) cliOptions {
			return cliOptions{sessionRef: id, agentMemoryCompact: true}
		},
		"agent memory migrate": func(id string) cliOptions {
			return cliOptions{sessionRef: id, agentMemoryMigrate: true}
		},
		"bash":      func(id string) cliOptions { return cliOptions{sessionRef: id, bashCommand: "echo ok"} },
		"async run": func(id string) cliOptions { return cliOptions{sessionRef: id, asyncRun: true} },
		"spawn agents": func(id string) cliOptions {
			return cliOptions{sessionRef: id, spawnAgentSpecs: []string{"reviewer|check"}}
		},
		"speculate run": func(id string) cliOptions { return cliOptions{sessionRef: id, speculateRun: true} },
		"review run":    func(id string) cliOptions { return cliOptions{sessionRef: id, reviewRun: true} },
		"list models":   func(id string) cliOptions { return cliOptions{sessionRef: id, listModels: true} },
		"doctor":        func(id string) cliOptions { return cliOptions{sessionRef: id, doctor: true} },
	}

	for name, optsForID := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			store := session.NewStore(t.TempDir())
			saved := session.New("gpt-test", nil)
			saved.DefaultAgent = removedAgent

			err := store.Save(saved)
			require.NoError(t, err)

			selection, err := resolveSelection(
				optsForID(saved.ID),
				config.Config{},
				"",
				agent.NewRegistry(nil),
				store,
			)
			require.NoError(t, err)
			assert.Equal(t, removedAgent, selection.sessionState.DefaultAgent)
		})
	}
}

func TestResolveSelection_ExplicitUnknownAgentStillErrorsForSessionUtilities(t *testing.T) {
	t.Parallel()

	store := session.NewStore(t.TempDir())
	saved := session.New("gpt-test", nil)
	err := store.Save(saved)
	require.NoError(t, err)

	_, err = resolveSelection(
		cliOptions{sessionRef: saved.ID, listMessages: true, agentName: "missing"},
		config.Config{},
		"",
		agent.NewRegistry(nil),
		store,
	)
	require.Error(t, err)
	assert.Contains(t, err.Error(), `unknown agent "missing"`)
}

func TestResolveSelection_UsesPersistedModelBeforeConfigDefault(t *testing.T) {
	t.Parallel()

	selection, err := resolveSelection(
		cliOptions{},
		config.Config{DefaultModel: "config-model"},
		testCodexModel,
		agent.NewRegistry(nil),
		session.NewStore(t.TempDir()),
	)
	if err != nil {
		require.NoError(t, err)
	}

	if selection.selectedModel != testCodexModel {
		require.Failf(t, "unexpected failure", "selectedModel = %q", selection.selectedModel)
	}
}

func TestResolveSelection_LoadedSessionWinsOverPersistedModel(t *testing.T) {
	t.Parallel()
	store := session.NewStore(t.TempDir())

	saved := session.New("session-model", nil)
	if err := store.Save(saved); err != nil {
		require.NoError(t, err)
	}

	selection, err := resolveSelection(
		cliOptions{sessionRef: saved.ID},
		config.Config{DefaultModel: "config-model"},
		"persisted-model",
		agent.NewRegistry(nil),
		store,
	)
	if err != nil {
		require.NoError(t, err)
	}

	if selection.selectedModel != "session-model" {
		require.Failf(t, "unexpected failure", "selectedModel = %q", selection.selectedModel)
	}
}
