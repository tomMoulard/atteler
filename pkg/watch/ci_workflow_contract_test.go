package watch

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gopkg.in/yaml.v3"
)

const (
	ciWorkflowName              = "CI"
	ciBranchProtectionCheckName = "Generate, lint, test, build, and package snapshot"
	ciBranchProtectionStatus    = ciWorkflowName + " / " + ciBranchProtectionCheckName
)

func TestRepositoryCIWorkflowMatchesDocumentation(t *testing.T) {
	t.Parallel()

	root, err := filepath.Abs(filepath.Join("..", ".."))
	require.NoError(t, err)

	ciWorkflow := readRepositoryFile(t, root, ".github/workflows/ci.yml")
	releaseWorkflow := readRepositoryFile(t, root, ".github/workflows/release.yml")
	readme := readRepositoryFile(t, root, "README.md")

	ci := requireGitHubWorkflowYAML(t, ciWorkflow)
	release := requireGitHubWorkflowYAML(t, releaseWorkflow)

	assert.Equal(t, ciWorkflowName, ci.Name, "workflow name should stay stable for branch-protection guidance")
	require.Contains(t, ci.On, "pull_request", "documented PR CI must be backed by a workflow trigger")
	assert.Contains(t, ci.On["pull_request"].Branches, "**")
	assert.ElementsMatch(t, []string{"opened", "reopened", "synchronize", "ready_for_review"}, ci.On["pull_request"].Types)
	assert.NotContains(t, ci.On, "pull_request_target", "fork PR CI must not run with target-repository privileges")

	require.Contains(t, ci.On, "push", "branch-push CI trigger should remain explicit")
	assert.Contains(t, ci.On["push"].Branches, "**")
	assert.Empty(t, ci.On["push"].Tags, "CI workflow should stay branch/PR scoped unless README is updated")

	assert.Equal(t, "read", ci.Permissions["contents"], "PR-safe CI should not request write permissions")
	assert.NotContains(t, ciWorkflow, "${{ secrets.", "PR-safe CI should not depend on repository secrets")
	assert.Contains(t, ciWorkflow, "never references repository secrets")
	assert.Contains(t, ciWorkflow, "avoids\n# pull_request_target privileges")
	require.Contains(t, ci.Jobs, "test")
	assert.Equal(t, ciBranchProtectionCheckName, ci.Jobs["test"].Name, "required-check name should remain stable")

	assert.Equal(t, "Release", release.Name, "README refers to the release workflow by name")
	require.Contains(t, release.On, "push")
	assert.Contains(t, release.On["push"].Tags, "*", "tag publishing should stay isolated in the release workflow")
	assert.NotContains(t, release.On, "pull_request")

	assert.Contains(t, readme, "GitHub Actions and opt-in local checks")
	assert.Contains(t, readme, "Pull requests targeting any branch")
	assert.Contains(t, readme, "open, reopen, commit update, or ready-for-review events")
	assert.Contains(t, readme, "including non-live `test/e2e`")
	assert.Contains(t, readme, "Branch pushes to any branch")
	assert.Contains(t, readme, "Tag pushes")
	assert.Contains(t, readme, "Local E2E shortcuts and live-provider checks")
	assert.NotContains(t, readme, "GitHub Actions runs CI on pull requests and branch pushes")
	assert.Contains(t, readme, ciBranchProtectionStatus)
	assert.True(t,
		strings.Contains(readme, "Fork PRs run this same PR-safe job") &&
			strings.Contains(readme, "no repository secrets"),
		"README should document fork PR secret-safety",
	)
	assert.Contains(t, readme, "`make e2e` is a local shortcut")
	assert.Contains(t, readme, "There is currently no manual GitHub Actions workflow for live-provider checks")
	assert.Contains(t, readme, "requires `ATTELER_E2E_LIVE=1` and provider credentials")
}

func readRepositoryFile(t *testing.T, root, path string) string {
	t.Helper()

	content, err := os.ReadFile(filepath.Join(root, path))
	require.NoError(t, err)

	return string(content)
}

type githubWorkflow struct {
	On          map[string]githubWorkflowTrigger `yaml:"on"`
	Permissions map[string]string                `yaml:"permissions"`
	Jobs        map[string]githubWorkflowJob     `yaml:"jobs"`
	Name        string                           `yaml:"name"`
}

type githubWorkflowTrigger struct {
	Branches []string `yaml:"branches"`
	Tags     []string `yaml:"tags"`
	Types    []string `yaml:"types"`
}

type githubWorkflowJob struct {
	Name string `yaml:"name"`
}

func requireGitHubWorkflowYAML(t *testing.T, content string) githubWorkflow {
	t.Helper()

	var workflow githubWorkflow
	require.NoError(t, yaml.Unmarshal([]byte(content), &workflow))
	require.NotEmpty(t, workflow.On)
	require.NotEmpty(t, workflow.Jobs)

	return workflow
}
