package atteler_test

import (
	"os"
	"os/exec"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestAttelerArtifactPolicy_DocsAndGitignoreAgree(t *testing.T) {
	t.Parallel()

	gitignoreBytes, err := os.ReadFile(".gitignore")
	require.NoError(t, err)

	gitignore := string(gitignoreBytes)

	readmeBytes, err := os.ReadFile("README.md")
	require.NoError(t, err)

	readme := string(readmeBytes)

	policyBytes, err := os.ReadFile(".atteler/README.md")
	require.NoError(t, err)

	policy := string(policyBytes)

	for _, privatePath := range []string{
		"/.atteler-state*",
		"/.atteler-config-*.tmp",
		"/.atteler/*",
		"/.atteler/fixtures/*",
		"/.atteler/evals/*",
		"/.atteler/skills/*",
		"**/.atteler/agent-memory.json",
		"**/.atteler/vector-index*.json",
		"**/.atteler/.agent-memory-*.tmp",
		"**/.atteler/.vector-index-*.tmp",
		"**/.atteler/.vector-store-*.tmp",
		"**/.atteler/.state-*.json",
	} {
		assert.Contains(t, gitignore, privatePath)
	}

	for _, exception := range []string{
		"!/.atteler/README.md",
		"!/.atteler/fixtures/**/*.fixture.json",
		"!/.atteler/fixtures/**/*.fixture.yaml",
		"!/.atteler/fixtures/**/*.fixture.yml",
		"!/.atteler/evals/**/*.eval.yaml",
		"!/.atteler/evals/**/*.eval.yml",
		"!/.atteler/evals/**/*.eval.json",
		"!/.atteler/skills/curated/**",
	} {
		assert.Contains(t, gitignore, exception)
	}

	for _, privateDefault := range []string{
		".atteler/sessions/",
		".atteler/runs/",
		".atteler/research/",
		".atteler/worktrees/",
		".atteler/tasks.json",
		".atteler/config.{yaml,yml,json}",
		".atteler/events.jsonl",
		".atteler-state*",
		".atteler-config-*.tmp",
		".atteler/eval-report*.json",
		".atteler/codeintel-index.json",
		".atteler/agent-memory.json",
		"nested `.atteler/` memory/vector temp files",
		".atteler/skill-learning/",
		".atteler/fixtures/once.json",
		".atteler/mcp.yaml",
		".atteler/plugins/",
		".atteler/watch-baseline.json",
		".atteler/incident.md",
	} {
		assert.Contains(t, readme, privateDefault)
		assert.Contains(t, policy, privateDefault)
	}

	for _, reviewablePattern := range []string{
		".atteler/evals/**/*.eval.{yaml,yml,json}",
		".atteler/fixtures/**/*.fixture.{json,yaml,yml}",
		".atteler/skills/curated/",
	} {
		assert.Contains(t, readme, reviewablePattern)
	}

	assert.Contains(t, policy, "raw transcripts")
	assert.Contains(t, policy, "evals/**/*.eval.{json,yaml,yml}")
}

func TestAttelerArtifactPolicy_GitIgnoreResolution(t *testing.T) {
	t.Parallel()

	if _, err := exec.LookPath("git"); err != nil {
		t.Skipf("git is unavailable: %v", err)
	}

	for _, path := range []string{
		".atteler/tasks.json",
		".atteler/config.yaml",
		".atteler/config.yml",
		".atteler/config.json",
		".atteler/events.jsonl",
		".atteler-state.yaml",
		".atteler-state-123.tmp",
		".atteler-config-123.tmp",
		".atteler/sessions/session.json",
		".atteler/runs/research/run.json",
		".atteler/research/plugin-sandboxing/tasks.generated.yaml",
		".atteler/worktrees/session-1",
		".atteler/eval-report.json",
		".atteler/codeintel-index.json",
		".atteler/adr-vector-index.json",
		".atteler/git-history-vector-index.json",
		".atteler/vector-index.json",
		".atteler/agent-memory.json",
		".atteler/skill-learning/state.json",
		".atteler/fixtures/once.json",
		".atteler/fixtures/readme-summary.txt",
		".atteler/evals/report.json",
		".atteler/mcp.yaml",
		".atteler/plugins/rtk/manifest.yaml",
		".atteler/watch-baseline.json",
		".atteler/incident.md",
		".atteler/skills/generated/foo/SKILL.md",
		"pkg/foo/.atteler/agent-memory.json",
		"pkg/foo/.atteler/vector-index.json",
		"pkg/foo/.atteler/vector-index.lexical.json",
		"pkg/foo/.atteler/session-vector-index.json",
		"pkg/foo/.atteler/git-history-vector-index.json",
		"pkg/foo/.atteler/adr-vector-index.json",
		"pkg/foo/.atteler/.agent-memory-123.tmp",
		"pkg/foo/.atteler/.vector-index-123.tmp",
		"pkg/foo/.atteler/.vector-store-123.tmp",
		"pkg/foo/.atteler/.state-123.json",
	} {
		assert.True(t, gitCheckIgnored(t, path), "%s should stay ignored/private", path)
	}

	for _, path := range []string{
		".atteler/README.md",
		".atteler/fixtures/example.fixture.json",
		".atteler/fixtures/nested/example.fixture.yaml",
		".atteler/fixtures/nested/example.fixture.yml",
		".atteler/evals/readme.eval.yaml",
		".atteler/evals/nested/readme.eval.json",
		".atteler/evals/nested/readme.eval.yml",
		".atteler/skills/curated/foo.md",
	} {
		assert.False(t, gitCheckIgnored(t, path), "%s should remain reviewable", path)
	}
}

func gitCheckIgnored(t *testing.T, path string) bool {
	t.Helper()

	cmd := exec.CommandContext(t.Context(), "git", "check-ignore", "--quiet", "--", path)

	err := cmd.Run()
	if err == nil {
		return true
	}

	var exitErr *exec.ExitError
	if ok := assert.ErrorAs(t, err, &exitErr); !ok {
		return false
	}

	if exitErr.ExitCode() == 1 {
		return false
	}

	require.NoError(t, err, "git check-ignore failed for %s", path)

	return false
}
