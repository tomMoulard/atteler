package main

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/tommoulard/atteler/pkg/autonomy"
	appconfig "github.com/tommoulard/atteler/pkg/config"
	"github.com/tommoulard/atteler/pkg/sourcepolicy"
)

func TestRunResearchCommandCreatesArtifacts(t *testing.T) {
	root := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	t.Setenv(appconfig.EnvPath, "")
	require.NoError(t, os.WriteFile(filepath.Join(root, "AGENTS.md"), []byte("# Rules\nUse citations.\n"), 0o600))

	stdout := captureProcessOutput(t, &os.Stdout)
	err := runResearchCommandWithAutonomy(context.Background(), root, researchCommandInput{
		Question:       "Compare plugin sandboxing approaches",
		OutputDir:      "research/out",
		TrustedSources: []string{"go.dev"},
		DeniedSources:  []string{"example-content-farm.com"},
		Sources:        []string{"AGENTS.md"},
		WarnLowTrust:   true,
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

func TestRunResearchCommandUsesConfiguredSourcePolicy(t *testing.T) {
	root := t.TempDir()
	t.Chdir(root)
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	t.Setenv(appconfig.EnvPath, "")
	require.NoError(t, os.MkdirAll(filepath.Join(root, ".atteler"), 0o700))
	require.NoError(t, os.WriteFile(filepath.Join(root, ".atteler", "config.yaml"), []byte(`
research:
  source_policy:
    denied_domains:
      - blocked.example
`), 0o600))

	stdout := captureProcessOutput(t, &os.Stdout)
	err := runResearchCommandWithAutonomy(context.Background(), root, researchCommandInput{
		Question:  "Compare source policy behavior",
		OutputDir: "research/out",
		Sources:   []string{"https://blocked.example/article"},
	}, autonomy.Medium)
	require.NoError(t, err)
	assert.Contains(t, requireLineBefore(t, stdout.lines, time.Second), "Research run out written to")

	runData, err := os.ReadFile(filepath.Join(root, "research", "out", "run.json"))
	require.NoError(t, err)

	var record struct {
		SourcePolicy struct {
			DeniedDomains []string `json:"denied_domains"`
		} `json:"source_policy"`
		Excluded []struct {
			URL    string `json:"url"`
			Domain string `json:"domain"`
			Reason string `json:"reason"`
		} `json:"excluded_sources"`
	}
	require.NoError(t, json.Unmarshal(runData, &record))
	assert.Equal(t, []string{"blocked.example"}, record.SourcePolicy.DeniedDomains)
	require.Len(t, record.Excluded, 1)
	assert.Equal(t, "https://blocked.example/article", record.Excluded[0].URL)
	assert.Equal(t, "blocked.example", record.Excluded[0].Domain)
	assert.Contains(t, record.Excluded[0].Reason, "denied")
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
		trustedSources:        stringListFlag{"docs.github.com"},
		deniedSources:         stringListFlag{"example-content-farm.com", "blocked.example"},
		warnLowTrustSources:   true,
		researchGenerateTasks: true,
	})

	assert.Equal(t, ".atteler/research/worktrees", input.OutputDir)
	assert.Equal(t, []string{"docs.github.com"}, input.TrustedSources)
	assert.Equal(t, []string{"example-content-farm.com", "blocked.example"}, input.DeniedSources)
	assert.True(t, input.WarnLowTrust)
	assert.True(t, input.GenerateTasks)

	policy := researchSourcePolicyFromInput(appconfig.Config{
		Research: appconfig.ResearchConfig{
			SourcePolicy: sourcepolicy.Policy{
				TrustedDomains: []string{"blocked.example"},
				DeniedDomains:  []string{"docs.github.com"},
			},
		},
	}, input)
	assert.Equal(t, []string{"docs.github.com"}, policy.TrustedDomains)
	assert.Equal(t, []string{"blocked.example", "example-content-farm.com"}, policy.DeniedDomains)
	require.NotNil(t, policy.WarnOnLowTrustSources)
	assert.True(t, *policy.WarnOnLowTrustSources)
}

func TestRetrievalCommandInputUsesSourcePolicyFlags(t *testing.T) {
	t.Parallel()

	input := retrievalCommandInputFromOptions(cliOptions{
		retrievalSearch:        "oauth",
		trustedSources:         stringListFlag{"docs.github.com"},
		deniedSources:          stringListFlag{"example-content-farm.com"},
		warnLowTrustSources:    true,
		retrievalIncludeUnsafe: true,
	})

	assert.Equal(t, []string{"docs.github.com"}, input.TrustedSources)
	assert.Equal(t, []string{"example-content-farm.com"}, input.DeniedSources)
	assert.True(t, input.WarnLowTrust)

	policy := retrievalSourcePolicyFromInput(sourcepolicy.Policy{
		TrustedDomains: []string{"go.dev"},
	}, input)
	assert.ElementsMatch(t, []string{"docs.github.com", "go.dev"}, policy.TrustedDomains)
	assert.Equal(t, []string{"example-content-farm.com"}, policy.DeniedDomains)
	require.NotNil(t, policy.WarnOnLowTrustSources)
	assert.True(t, *policy.WarnOnLowTrustSources)

	overridePolicy := retrievalSourcePolicyFromInput(sourcepolicy.Policy{
		TrustedDomains: []string{"blocked.example"},
		DeniedDomains:  []string{"docs.github.com"},
	}, retrievalCommandInput{
		TrustedSources: []string{"docs.github.com"},
		DeniedSources:  []string{"blocked.example"},
	})
	assert.Equal(t, []string{"docs.github.com"}, overridePolicy.TrustedDomains)
	assert.Equal(t, []string{"blocked.example"}, overridePolicy.DeniedDomains)

	parentDenyPolicy := retrievalSourcePolicyFromInput(sourcepolicy.Policy{
		DeniedDomains: []string{"example.com"},
	}, retrievalCommandInput{
		TrustedSources: []string{"docs.example.com"},
	})
	assert.Equal(t, []string{"docs.example.com"}, parentDenyPolicy.TrustedDomains)
	assert.Empty(t, parentDenyPolicy.DeniedDomains)
}

func TestRetrievalSourcePolicyForStateReadsHarnessGuidance(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(root, "AGENTS.md"), []byte(`
Prefer official documentation from docs.github.com.
Deny example-content-farm.com and exclude low-trust sources.
`), 0o600))

	policy, err := retrievalSourcePolicyForState(appState{cwd: root}, retrievalCommandInput{
		WarnLowTrust: true,
	})
	require.NoError(t, err)

	assert.Contains(t, policy.TrustedDomains, "docs.github.com")
	assert.Contains(t, policy.DeniedDomains, "example-content-farm.com")
	assert.Contains(t, policy.PreferSourceTypes, sourcepolicy.SourceTypeOfficialDocs)
	require.NotNil(t, policy.AllowLowTrustSources)
	assert.False(t, *policy.AllowLowTrustSources)
	require.NotNil(t, policy.WarnOnLowTrustSources)
	assert.True(t, *policy.WarnOnLowTrustSources)
}

func TestSourcePolicyFlagsAllowedForRetrievalSearch(t *testing.T) {
	t.Parallel()

	err := validateResearchCommandSelection(cliOptions{
		retrievalSearch: "oauth",
		trustedSources:  stringListFlag{"docs.github.com"},
		deniedSources:   stringListFlag{"example-content-farm.com"},
	})
	require.NoError(t, err)

	err = validateResearchCommandSelection(cliOptions{trustedSources: stringListFlag{"docs.github.com"}})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "--retrieval-search")
}
