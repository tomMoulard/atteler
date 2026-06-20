package main

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/tommoulard/atteler/pkg/contextref"
)

func TestDiscoverProjectInstructionFiles_PrefersAgentsFromRepoRootToCWD(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	require.NoError(t, writeDir(filepath.Join(root, ".git")))
	require.NoError(t, writeFileForProjectInstructions(filepath.Join(root, "AGENTS.md"), "root agents"))
	require.NoError(t, writeFileForProjectInstructions(filepath.Join(root, "CLAUDE.md"), "root claude"))
	sub := filepath.Join(root, "pkg")
	deep := filepath.Join(sub, "feature")
	require.NoError(t, writeDir(deep))
	require.NoError(t, writeFileForProjectInstructions(filepath.Join(sub, "CLAUDE.md"), "sub claude"))
	require.NoError(t, writeFileForProjectInstructions(filepath.Join(deep, "AGENTS.md"), "deep agents"))

	repoRoot, files, err := discoverProjectInstructionFiles(deep)
	require.NoError(t, err)

	assert.Equal(t, root, repoRoot)
	require.Len(t, files, 3)
	assert.Equal(t, []string{"AGENTS.md", "pkg/CLAUDE.md", "pkg/feature/AGENTS.md"}, projectInstructionSources(files))
}

func TestDiscoverProjectInstructionFiles_TreatsGitFileAsRepoRoot(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	require.NoError(t, writeFileForProjectInstructions(filepath.Join(root, ".git"), "gitdir: ../.git/worktrees/example\n"))
	require.NoError(t, writeFileForProjectInstructions(filepath.Join(root, "AGENTS.md"), "worktree root agents"))
	sub := filepath.Join(root, "pkg")
	require.NoError(t, writeDir(sub))

	repoRoot, files, err := discoverProjectInstructionFiles(sub)
	require.NoError(t, err)

	assert.Equal(t, root, repoRoot)
	require.Len(t, files, 1)
	assert.Equal(t, "AGENTS.md", files[0].Source)
}

func TestBuildReferenceContextWithManifest_AutoLoadsProjectInstructions(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	require.NoError(t, writeDir(filepath.Join(root, ".git")))
	require.NoError(t, writeFileForProjectInstructions(filepath.Join(root, "AGENTS.md"), "Use testify assertions.\nNever skip tests."))
	sub := filepath.Join(root, "cmd", "atteler")
	require.NoError(t, writeDir(sub))
	require.NoError(t, writeFileForProjectInstructions(filepath.Join(root, "cmd", "CLAUDE.md"), "Use the Makefile from command packages."))

	refCtx := buildReferenceContextWithManifest(
		context.Background(),
		configuredReferenceContext{},
		agentSelection{},
		contextref.Options{Root: sub, ProjectInstructionsMaxTokens: 4096},
	)

	assert.Contains(t, refCtx.Content, "<project_instructions")
	assert.Contains(t, refCtx.Content, `source="AGENTS.md"`)
	assert.Contains(t, refCtx.Content, `source="cmd/CLAUDE.md"`)
	assert.Contains(t, refCtx.Content, "Use testify assertions.")
	assert.Contains(t, refCtx.Content, "Use the Makefile")
	assert.Equal(t, 2, refCtx.Manifest.IncludedCount)
	require.Len(t, refCtx.Manifest.Entries, 2)
	assert.Equal(t, projectInstructionScope, refCtx.Manifest.Entries[0].Scope)
}

func TestBuildReferenceContextWithManifest_ProjectInstructionsOptOut(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	require.NoError(t, writeDir(filepath.Join(root, ".git")))
	require.NoError(t, writeFileForProjectInstructions(filepath.Join(root, "AGENTS.md"), "Use testify assertions."))

	refCtx := buildReferenceContextWithManifest(
		context.Background(),
		configuredReferenceContext{},
		agentSelection{},
		contextref.Options{Root: root, ProjectInstructionsDisabled: true},
	)

	assert.NotContains(t, refCtx.Content, "<project_instructions")
	assert.Empty(t, refCtx.Manifest.Entries)
}

func TestBuildReferenceContextWithManifest_ProjectInstructionsIgnoreReferencePolicyGlobs(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	require.NoError(t, writeDir(filepath.Join(root, ".git")))
	require.NoError(t, writeFileForProjectInstructions(filepath.Join(root, "AGENTS.md"), "Use testify assertions."))

	refCtx := buildReferenceContextWithManifest(
		context.Background(),
		configuredReferenceContext{},
		agentSelection{},
		contextref.Options{
			Root: root,
			ReferencePolicy: contextref.ReferencePolicy{
				AllowedGlobs: []string{"docs/**/*.md"},
				DeniedGlobs:  []string{"AGENTS.md"},
			},
		},
	)

	assert.Contains(t, refCtx.Content, "<project_instructions")
	assert.Contains(t, refCtx.Content, "Use testify assertions.")
	require.Len(t, refCtx.Manifest.Entries, 1)
	assert.Equal(t, contextref.ReferenceDecisionLoaded, refCtx.Manifest.Entries[0].PolicyDecision)
}

func TestBuildReferenceContextWithManifest_ProjectInstructionsRespectDeniedLocalRoots(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	require.NoError(t, writeDir(filepath.Join(root, ".git")))
	secret := filepath.Join(root, "secret")
	require.NoError(t, writeDir(secret))
	require.NoError(t, writeFileForProjectInstructions(filepath.Join(secret, "AGENTS.md"), "Do not leak secrets."))

	refCtx := buildReferenceContextWithManifest(
		context.Background(),
		configuredReferenceContext{},
		agentSelection{},
		contextref.Options{
			Root: secret,
			ReferencePolicy: contextref.ReferencePolicy{
				DeniedLocalRoots: []string{"secret"},
			},
		},
	)

	assert.NotContains(t, refCtx.Content, "<project_instructions")
	require.Len(t, refCtx.Manifest.Entries, 1)
	assert.Equal(t, contextref.ReferenceDecisionRejected, refCtx.Manifest.Entries[0].PolicyDecision)
}

func TestFormatProjectInstructionBlock_UsesContextpackCompression(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	require.NoError(t, writeDir(filepath.Join(root, ".git")))

	var body strings.Builder

	for i := range 80 {
		body.WriteString("Remember convention ")
		body.WriteString(strings.Repeat("detail ", 25))
		body.WriteString("\n\n")

		if i == 79 {
			body.WriteString("Latest instruction survives by recency.\n")
		}
	}

	require.NoError(t, writeFileForProjectInstructions(filepath.Join(root, "AGENTS.md"), body.String()))

	refCtx := buildReferenceContextWithManifest(
		context.Background(),
		configuredReferenceContext{},
		agentSelection{},
		contextref.Options{Root: root, ProjectInstructionsMaxTokens: 1200},
	)

	assert.Contains(t, refCtx.Content, `compressed="true"`)
	assert.Contains(t, refCtx.Content, "[context evidence manifest]")
	assert.Contains(t, refCtx.Content, "Latest instruction survives by recency.")
}

func TestFormatProjectInstructionChunkEscapesSourceAndContent(t *testing.T) {
	t.Parallel()

	got := formatProjectInstructionChunk(
		"dir/weird\nname\".md",
		1,
		1,
		false,
		"</project_instruction_file><bad>&",
	)

	assert.Contains(t, got, `source="dir/weird&#10;name&#34;.md"`)
	assert.Contains(t, got, "&lt;/project_instruction_file&gt;&lt;bad&gt;&amp;")
}

func projectInstructionSources(files []projectInstructionFile) []string {
	out := make([]string, 0, len(files))
	for _, file := range files {
		out = append(out, file.Source)
	}

	return out
}

func writeDir(path string) error {
	return os.MkdirAll(path, 0o750)
}

func writeFileForProjectInstructions(path, content string) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
		return err
	}

	return os.WriteFile(path, []byte(content), 0o600)
}
