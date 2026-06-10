package vector

import (
	"context"
	"encoding/json"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/tommoulard/atteler/pkg/permission"
	"github.com/tommoulard/atteler/pkg/privacy"
	"github.com/tommoulard/atteler/pkg/retrieval"
)

const (
	workspaceAuthText   = "OAuth workspace retrieval"
	workspaceSecretText = "OAuth retry notes include api_" + "key=super-" + "secret-token"
)

func TestDiscoverWorkspaceSources_RespectsDefaultGitAndAttelerIgnores(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	writeWorkspaceFile(t, root, ".gitignore", "ignored.md\nbuild/\n!.env\n")
	writeWorkspaceFile(t, root, ".attelerignore", "private.txt\n")
	writeWorkspaceFile(t, root, "main.go", "package main\n// workspace retrieval\n")
	writeWorkspaceFile(t, root, "ignored.md", "ignored by gitignore")
	writeWorkspaceFile(t, root, "build/out.go", "package build")
	writeWorkspaceFile(t, root, "generated/client.go", "package generated")
	writeWorkspaceFile(t, root, "out/bundle.js", "console.log('build output')")
	writeWorkspaceFile(t, root, "api.pb.go", "package api")
	writeWorkspaceFile(t, root, "model.gen.go", "package model")
	writeWorkspaceFile(t, root, "schema.generated.ts", "export const schema = {}")
	writeWorkspaceFile(t, root, "node_modules/pkg/index.js", "console.log('ignored')")
	writeWorkspaceFile(t, root, ".pytest_cache/readme.md", "python cache")
	writeWorkspaceFile(t, root, ".mypy_cache/readme.md", "mypy cache")
	writeWorkspaceFile(t, root, ".ruff_cache/readme.md", "ruff cache")
	writeWorkspaceFile(t, root, ".parcel-cache/index.js", "parcel cache")
	writeWorkspaceFile(t, root, ".aws/config.md", "aws profile notes")
	writeWorkspaceFile(t, root, ".docker/config.json", `{"auths":{}}`)
	writeWorkspaceFile(t, root, ".gnupg/readme.md", "gpg notes")
	writeWorkspaceFile(t, root, ".kube/config.yaml", "cluster: private")
	writeWorkspaceFile(t, root, ".ssh/config.md", "ssh host notes")
	writeWorkspaceFile(t, root, "package-lock.json", `{"lockfileVersion": 3}`)
	writeWorkspaceFile(t, root, "pnpm-lock.yaml", "lockfileVersion: '9.0'\n")
	writeWorkspaceFile(t, root, "bun.lockb", "binary-ish dependency lock")
	writeWorkspaceFile(t, root, "go.sum", "example.com/module v1.0.0 h1:hash\n")
	writeWorkspaceFile(t, root, ".env", "API_KEY=secret")
	writeWorkspaceFile(t, root, ".Env.Local", "API_KEY=secret")
	writeWorkspaceFile(t, root, "SECRET_NOTES.md", "secret token")
	writeWorkspaceFile(t, root, "Credentials.md", "credential notes")
	writeWorkspaceFile(t, root, "TOKENS.md", "token notes")
	writeWorkspaceFile(t, root, "Passwords.txt", "password notes")
	writeWorkspaceFile(t, root, "api_key_notes.md", "key notes")
	writeWorkspaceFile(t, root, "id_ed25519.pub", "ssh-ed25519 public key")
	writeWorkspaceFile(t, root, "private.txt", "credential notes")
	writeWorkspaceFile(t, root, "image.go", string([]byte{'p', 'k', 0, 'g'}))
	writeWorkspaceFile(t, root, "outside.md", "outside workspace")
	writeWorkspaceFile(t, root, "workspace-index.json", `{"documents":["generated"]}`)
	writeWorkspaceFile(t, root, "workspace-index.lexical.json", `{"documents":["generated fallback"]}`)
	symlinkCreated := os.Symlink(filepath.Join(root, "outside.md"), filepath.Join(root, "linked.md")) == nil

	sources, skipped, err := DiscoverWorkspaceSources(context.TODO(), WorkspaceOptions{
		Root:      root,
		IndexPath: filepath.Join(root, "workspace-index.json"),
	})
	require.NoError(t, err)

	require.Len(t, sources, 2)
	assert.Equal(t, "main.go", sources[0].Path)
	assert.Equal(t, "outside.md", sources[1].Path)
	assertWorkspaceSkipped(t, skipped, "ignored.md")
	assertWorkspaceSkipped(t, skipped, "build")
	assertWorkspaceSkipped(t, skipped, "generated")
	assertWorkspaceSkipped(t, skipped, "out")
	assertWorkspaceSkipped(t, skipped, "api.pb.go")
	assertWorkspaceSkipped(t, skipped, "model.gen.go")
	assertWorkspaceSkipped(t, skipped, "schema.generated.ts")
	assertWorkspaceSkipped(t, skipped, "node_modules")
	assertWorkspaceSkipped(t, skipped, ".pytest_cache")
	assertWorkspaceSkipped(t, skipped, ".mypy_cache")
	assertWorkspaceSkipped(t, skipped, ".ruff_cache")
	assertWorkspaceSkipped(t, skipped, ".parcel-cache")
	assertWorkspaceSkipped(t, skipped, ".aws")
	assertWorkspaceSkipped(t, skipped, ".docker")
	assertWorkspaceSkipped(t, skipped, ".gnupg")
	assertWorkspaceSkipped(t, skipped, ".kube")
	assertWorkspaceSkipped(t, skipped, ".ssh")
	assertWorkspaceSkipped(t, skipped, "package-lock.json")
	assertWorkspaceSkipped(t, skipped, "pnpm-lock.yaml")
	assertWorkspaceSkipped(t, skipped, "bun.lockb")
	assertWorkspaceSkipped(t, skipped, "go.sum")
	assertWorkspaceSkipped(t, skipped, ".env")
	assertWorkspaceSkipped(t, skipped, ".Env.Local")
	assertWorkspaceSkipped(t, skipped, "SECRET_NOTES.md")
	assertWorkspaceSkipped(t, skipped, "Credentials.md")
	assertWorkspaceSkipped(t, skipped, "TOKENS.md")
	assertWorkspaceSkipped(t, skipped, "Passwords.txt")
	assertWorkspaceSkipped(t, skipped, "api_key_notes.md")
	assertWorkspaceSkipped(t, skipped, "id_ed25519.pub")
	assertWorkspaceSkipped(t, skipped, "private.txt")
	assertWorkspaceSkipped(t, skipped, "image.go")
	assertWorkspaceSkipped(t, skipped, "workspace-index.json")
	assertWorkspaceSkipped(t, skipped, "workspace-index.lexical.json")

	if symlinkCreated {
		assertWorkspaceSkipped(t, skipped, "linked.md")
	}
}

func TestDiscoverWorkspaceSources_IncludeDirectoryPatternIncludesContents(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	writeWorkspaceFile(t, root, "docs/auth.md", workspaceAuthText)
	writeWorkspaceFile(t, root, "docs/nested/shell.md", "Shell workspace retrieval")
	writeWorkspaceFile(t, root, "src/main.go", "package main")

	sources, skipped, err := DiscoverWorkspaceSources(context.TODO(), WorkspaceOptions{
		Root:            root,
		IncludePatterns: []string{"docs/"},
	})
	require.NoError(t, err)

	assert.ElementsMatch(t, []string{"docs/auth.md", "docs/nested/shell.md"}, sourcePaths(sources))
	assertWorkspaceSkipped(t, skipped, "src/main.go")
}

func TestDiscoverWorkspaceSources_EmptyIncludeListDoesNotExcludeEverything(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	writeWorkspaceFile(t, root, "docs/auth.md", workspaceAuthText)

	sources, skipped, err := DiscoverWorkspaceSources(context.TODO(), WorkspaceOptions{
		Root:            root,
		IncludePatterns: []string{},
	})
	require.NoError(t, err)

	assert.ElementsMatch(t, []string{"docs/auth.md"}, sourcePaths(sources))
	assert.Empty(t, skipped)
}

func TestDiscoverWorkspaceSources_HonorsWorkspaceExcludePatterns(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	writeWorkspaceFile(t, root, "docs/auth.md", workspaceAuthText)
	writeWorkspaceFile(t, root, "docs/generated.client.md", "generated docs")
	writeWorkspaceFile(t, root, "tmp/notes.md", "temporary notes")
	writeWorkspaceFile(t, root, "src/main.go", "package main\n")

	sources, skipped, err := DiscoverWorkspaceSources(context.TODO(), WorkspaceOptions{
		Root:            root,
		ExcludePatterns: []string{"tmp/", "docs/*.client.md"},
	})
	require.NoError(t, err)

	assert.ElementsMatch(t, []string{"docs/auth.md", "src/main.go"}, sourcePaths(sources))
	assertWorkspaceSkipped(t, skipped, "tmp")
	assertWorkspaceSkipped(t, skipped, "docs/generated.client.md")
}

func TestDiscoverWorkspaceSources_RejectsNilContext(t *testing.T) {
	t.Parallel()

	_, _, err := DiscoverWorkspaceSources(nil, WorkspaceOptions{Root: t.TempDir()}) //nolint:staticcheck // Verify nil contexts are rejected instead of panicking.
	require.Error(t, err)
	require.ErrorIs(t, err, ErrContextRequired)
}

func TestDiscoverWorkspaceSources_HonorsCanceledContextBeforeRootStat(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithCancel(context.TODO())
	cancel()

	_, _, err := DiscoverWorkspaceSources(ctx, WorkspaceOptions{
		Root: filepath.Join(t.TempDir(), "missing"),
	})
	require.Error(t, err)
	require.ErrorIs(t, err, context.Canceled)
	assert.NotContains(t, err.Error(), "stat root")
}

func TestDiscoverWorkspaceSources_MaxFilesPrunesRemainingDirectories(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	writeWorkspaceFile(t, root, "a.md", workspaceAuthText)
	writeWorkspaceFile(t, root, "z/deep.md", "Shell workspace retrieval")

	sources, skipped, err := DiscoverWorkspaceSources(context.TODO(), WorkspaceOptions{
		Root:     root,
		MaxFiles: 1,
	})
	require.NoError(t, err)

	assert.ElementsMatch(t, []string{"a.md"}, sourcePaths(sources))
	assertWorkspaceSkipped(t, skipped, "z")
	assert.NotContains(t, fmt.Sprint(skipped), "z/deep.md")
}

func TestDiscoverWorkspaceSources_IncludeGlobPatternDescendsIntoCandidateDirectories(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	writeWorkspaceFile(t, root, "docs/auth.md", workspaceAuthText)
	writeWorkspaceFile(t, root, "docs/nested/shell.md", "Shell workspace retrieval")
	writeWorkspaceFile(t, root, "src/main.go", "package main")

	sources, skipped, err := DiscoverWorkspaceSources(context.TODO(), WorkspaceOptions{
		Root:            root,
		IncludePatterns: []string{"docs/*.md"},
	})
	require.NoError(t, err)

	assert.ElementsMatch(t, []string{"docs/auth.md"}, sourcePaths(sources))
	assertWorkspaceSkipped(t, skipped, "docs/nested")
	assertWorkspaceSkipped(t, skipped, "src")
}

func TestDiscoverWorkspaceSources_DoubleStarIgnorePatternsMatchMultipleDirectoryLevels(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	writeWorkspaceFile(t, root, ".gitignore", "ignored/**/*.md\n")
	writeWorkspaceFile(t, root, "ignored/auth.md", workspaceAuthText)
	writeWorkspaceFile(t, root, "ignored/nested/shell.md", "Shell workspace retrieval")
	writeWorkspaceFile(t, root, "docs/auth.md", workspaceAuthText)
	writeWorkspaceFile(t, root, "docs/nested/shell.md", "Shell workspace retrieval")
	writeWorkspaceFile(t, root, "src/main.go", "package main")

	sources, skipped, err := DiscoverWorkspaceSources(context.TODO(), WorkspaceOptions{Root: root})
	require.NoError(t, err)

	assert.ElementsMatch(t, []string{"docs/auth.md", "docs/nested/shell.md", "src/main.go"}, sourcePaths(sources))
	assertWorkspaceSkipped(t, skipped, "ignored/auth.md")
	assertWorkspaceSkipped(t, skipped, "ignored/nested/shell.md")
}

func TestDiscoverWorkspaceSources_DoubleStarIncludePatternsMatchMultipleDirectoryLevels(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	writeWorkspaceFile(t, root, "docs/auth.md", workspaceAuthText)
	writeWorkspaceFile(t, root, "docs/nested/shell.md", "Shell workspace retrieval")
	writeWorkspaceFile(t, root, "src/main.go", "package main")

	sources, skipped, err := DiscoverWorkspaceSources(context.TODO(), WorkspaceOptions{
		Root:            root,
		IncludePatterns: []string{"docs/**/*.md"},
	})
	require.NoError(t, err)

	assert.ElementsMatch(t, []string{"docs/auth.md", "docs/nested/shell.md"}, sourcePaths(sources))
	assertWorkspaceSkipped(t, skipped, "src")
}

func TestDiscoverWorkspaceSources_IncludeGlobDirectoryPrefixPrunesNonMatchingDirectories(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	writeWorkspaceFile(t, root, "src-api/main.go", "package api")
	writeWorkspaceFile(t, root, "src-worker/main.go", "package worker")
	writeWorkspaceFile(t, root, "docs/main.go", "package docs")

	sources, skipped, err := DiscoverWorkspaceSources(context.TODO(), WorkspaceOptions{
		Root:            root,
		IncludePatterns: []string{"src*/**/*.go"},
	})
	require.NoError(t, err)

	assert.ElementsMatch(t, []string{"src-api/main.go", "src-worker/main.go"}, sourcePaths(sources))
	assertWorkspaceSkipped(t, skipped, "docs")
}

func TestDiscoverWorkspaceSources_FallbackIndexPathSkipsPrimaryIndexArtifact(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	writeWorkspaceFile(t, root, "docs/auth.md", workspaceAuthText)
	writeWorkspaceFile(t, root, "workspace-index.json", `{"documents":["embedding index"]}`)
	writeWorkspaceFile(t, root, "workspace-index.lexical.json", `{"documents":["lexical index"]}`)

	sources, skipped, err := DiscoverWorkspaceSources(context.TODO(), WorkspaceOptions{
		Root:      root,
		IndexPath: filepath.Join(root, "workspace-index.lexical.json"),
	})
	require.NoError(t, err)

	assert.ElementsMatch(t, []string{"docs/auth.md"}, sourcePaths(sources))
	assertWorkspaceSkipped(t, skipped, "workspace-index.json")
	assertWorkspaceSkipped(t, skipped, "workspace-index.lexical.json")
}

func TestRefreshWorkspaceIndex_SkipsLexicalFilesWithoutIndexableTokens(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	writeWorkspaceFile(t, root, "docs/auth.md", workspaceAuthText)
	writeWorkspaceFile(t, root, "docs/punctuation.json", "{}")

	vectorizer, err := NewTextVectorizer(16)
	require.NoError(t, err)

	refresh, err := RefreshWorkspaceIndex(context.TODO(), WorkspaceOptions{
		Root:               root,
		IndexPath:          filepath.Join(root, ".atteler", "workspace-index.json"),
		Vectorizer:         vectorizer,
		VectorizerMetadata: vectorizer.Metadata(),
		Chunk:              ChunkOptions{MaxRunes: 200, OverlapRunes: 20},
	})
	require.NoError(t, err)
	require.NotNil(t, refresh.Index)

	assert.ElementsMatch(t, []string{"docs/auth.md"}, sourceMetadataPaths(refresh.Index.Sources))
	assertWorkspaceSkipped(t, refresh.Skipped, "docs/punctuation.json")
}

func TestDiscoverWorkspaceSources_InfersEmbeddingVectorizerMetadata(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	writeWorkspaceFile(t, root, "docs/punctuation.md", "!!!")

	sources, skipped, err := DiscoverWorkspaceSources(context.TODO(), WorkspaceOptions{
		Root:       root,
		Vectorizer: NewEmbeddingVectorizer(),
	})
	require.NoError(t, err)

	assert.ElementsMatch(t, []string{"docs/punctuation.md"}, sourcePaths(sources))
	assert.Empty(t, skipped)
}

func TestRefreshWorkspaceIndex_RejectsNilContext(t *testing.T) {
	t.Parallel()

	refresh, err := RefreshWorkspaceIndex(nil, WorkspaceOptions{Root: t.TempDir()}) //nolint:staticcheck // Verify nil contexts are rejected instead of panicking.
	require.Error(t, err)
	require.ErrorIs(t, err, ErrContextRequired)
	assert.Nil(t, refresh.Index)
}

func TestRefreshWorkspaceIndex_HonorsCanceledContextBeforeRootStat(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithCancel(context.TODO())
	cancel()

	refresh, err := RefreshWorkspaceIndex(ctx, WorkspaceOptions{
		Root: filepath.Join(t.TempDir(), "missing"),
	})
	require.Error(t, err)
	require.ErrorIs(t, err, context.Canceled)
	assert.NotContains(t, err.Error(), "stat root")
	assert.Nil(t, refresh.Index)
}

func TestWorkspaceWalkerSourceFromFileSkipsFileLargerThanStatSize(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	writeWorkspaceFile(t, root, "docs/growing.md", strings.Repeat("OAuth workspace retrieval ", 10))

	walker := workspaceWalker{
		opts: WorkspaceOptions{MaxFileBytes: 8},
	}
	source, ok := walker.sourceFromFile(
		filepath.Join(root, "docs", "growing.md"),
		"docs/growing.md",
		stubDirEntry{name: "growing.md", info: stubFileInfo{name: "growing.md", size: 1}},
	)

	assert.False(t, ok)
	assert.Empty(t, source)
	assertWorkspaceSkipped(t, walker.skipped, "docs/growing.md")
}

func TestWorkspaceWalkerSourceFromFileSkipsNonRegularFiles(t *testing.T) {
	t.Parallel()

	walker := workspaceWalker{}
	source, ok := walker.sourceFromFile(
		filepath.Join(t.TempDir(), "device.md"),
		"device.md",
		stubDirEntry{name: "device.md", info: stubFileInfo{name: "device.md", mode: fs.ModeDevice}},
	)

	assert.False(t, ok)
	assert.Empty(t, source)
	assertWorkspaceSkipped(t, walker.skipped, "device.md")
}

func TestDiscoverWorkspaceSources_RespectsAnchoredIgnorePatterns(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	writeWorkspaceFile(t, root, ".gitignore", "/ignored.md\n/*.txt\n")
	writeWorkspaceFile(t, root, "docs/.gitignore", "/local.md\n")
	writeWorkspaceFile(t, root, "ignored.md", "root ignored")
	writeWorkspaceFile(t, root, "keep.txt", "root ignored by anchored glob")
	writeWorkspaceFile(t, root, "docs/ignored.md", "nested ignored should be indexed")
	writeWorkspaceFile(t, root, "docs/local.md", "local ignored")
	writeWorkspaceFile(t, root, "docs/nested/local.md", "nested local should be indexed")
	writeWorkspaceFile(t, root, "docs/nested/keep.txt", "nested txt should be indexed")

	sources, skipped, err := DiscoverWorkspaceSources(context.TODO(), WorkspaceOptions{Root: root})
	require.NoError(t, err)

	assert.ElementsMatch(t, []string{
		"docs/ignored.md",
		"docs/nested/keep.txt",
		"docs/nested/local.md",
	}, sourcePaths(sources))
	assertWorkspaceSkipped(t, skipped, "ignored.md")
	assertWorkspaceSkipped(t, skipped, "keep.txt")
	assertWorkspaceSkipped(t, skipped, "docs/local.md")
}

func TestDiscoverWorkspaceSources_SkipsRedactedPathCollisions(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	writeWorkspaceFile(t, root, "docs/bearer abc123456789/auth.md", "OAuth callback state validation.")
	writeWorkspaceFile(t, root, "docs/bearer def987654321/auth.md", "OAuth callback state validation duplicate.")

	sources, skipped, err := DiscoverWorkspaceSources(context.TODO(), WorkspaceOptions{Root: root})
	require.NoError(t, err)

	require.Len(t, sources, 1)
	assert.Equal(t, "docs/bearer abc123456789/auth.md", sources[0].Path)
	assertWorkspaceSkipped(t, skipped, "docs/Bearer [REDACTED]/auth.md")
	assert.NotContains(t, fmt.Sprint(skipped), "def987654321")
}

func TestRefreshWorkspaceIndex_PersistsAndIncrementallyUpdates(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	writeWorkspaceFile(t, root, "a.md", "alpha workspace retrieval")
	writeWorkspaceFile(t, root, "b.md", "beta workspace retrieval")

	vectorizer, err := NewTextVectorizer(16)
	require.NoError(t, err)

	counting := &countingVectorizer{inner: vectorizer}
	indexPath := filepath.Join(root, ".atteler", "workspace-index.json")
	now := time.Unix(10, 0).UTC()
	opts := WorkspaceOptions{
		Root:               root,
		IndexPath:          indexPath,
		Vectorizer:         counting,
		VectorizerMetadata: vectorizer.Metadata(),
		Chunk:              ChunkOptions{MaxRunes: 200, OverlapRunes: 20},
		Now:                func() time.Time { return now },
	}

	first, err := RefreshWorkspaceIndex(context.TODO(), opts)
	require.NoError(t, err)
	require.FileExists(t, indexPath)
	require.NotNil(t, first.Index)
	assert.True(t, first.Refreshed)
	assert.Equal(t, 2, first.Added)
	assert.Equal(t, 2, counting.calls)
	assert.Equal(t, time.Unix(10, 0).UTC(), first.Index.CreatedAt)
	assert.Equal(t, time.Unix(10, 0).UTC(), first.Index.UpdatedAt)

	loaded, err := LoadIndex(indexPath)
	require.NoError(t, err)
	require.Len(t, loaded.Sources, 2)
	require.Len(t, loaded.Documents, 2)

	for i := range loaded.Documents {
		assert.Equal(t, time.Unix(10, 0).UTC(), loaded.Documents[i].CreatedAt)
		assert.Equal(t, time.Unix(10, 0).UTC(), loaded.Documents[i].UpdatedAt)
		assert.NotEmpty(t, loaded.Documents[i].Metadata[retrieval.MetadataSourceUpdatedAt])
	}

	writeWorkspaceFile(t, root, "a.md", "alpha changed workspace retrieval")
	require.NoError(t, os.Remove(filepath.Join(root, "b.md")))
	writeWorkspaceFile(t, root, "c.md", "gamma workspace retrieval")

	counting.calls = 0
	now = time.Unix(20, 0).UTC()
	second, err := RefreshWorkspaceIndex(context.TODO(), opts)
	require.NoError(t, err)

	assert.True(t, second.Refreshed)
	assert.False(t, second.Rebuilt)
	assert.Equal(t, 1, second.Added)
	assert.Equal(t, 1, second.Updated)
	assert.Equal(t, 1, second.Deleted)
	assert.Equal(t, 2, counting.calls, "only changed and added sources should be vectorized")
	assert.Equal(t, time.Unix(10, 0).UTC(), second.Index.CreatedAt)
	assert.Equal(t, time.Unix(20, 0).UTC(), second.Index.UpdatedAt)

	loaded, err = LoadIndex(indexPath)
	require.NoError(t, err)
	assert.ElementsMatch(t, []string{"a.md", "c.md"}, sourceMetadataPaths(loaded.Sources))
	assert.ElementsMatch(t, []string{"a.md#chunk=0000", "c.md#chunk=0000"}, documentIDs(loaded.Documents))
	assert.Equal(t, time.Unix(10, 0).UTC(), loaded.CreatedAt)
	assert.Equal(t, time.Unix(20, 0).UTC(), loaded.UpdatedAt)

	require.NoError(t, os.Remove(filepath.Join(root, "a.md")))
	require.NoError(t, os.Remove(filepath.Join(root, "c.md")))

	counting.calls = 0
	third, err := RefreshWorkspaceIndex(context.TODO(), opts)
	require.Error(t, err)
	require.ErrorIs(t, err, ErrNoSources)
	assert.True(t, third.Refreshed)
	assert.Equal(t, 2, third.Deleted)
	assert.Equal(t, 0, counting.calls, "deleting the last sources should not vectorize")

	_, statErr := os.Stat(indexPath)
	assert.ErrorIs(t, statErr, os.ErrNotExist)
}

func TestRefreshWorkspaceIndexPermissionPolicyDeniesIndexWrite(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	writeWorkspaceFile(t, root, "notes.md", "OAuth workspace retrieval")

	vectorizer, err := NewTextVectorizer(0)
	require.NoError(t, err)

	indexPath := filepath.Join(root, ".atteler", "workspace-vector-index.json")
	policy := permission.DefaultPolicy()
	policy.SetMode(permission.OperationWrite, permission.ModeDeny)

	ctx := permission.ContextWithAuditDir(t.Context(), filepath.Join(root, "audit"))
	ctx = permission.ContextWithPolicy(ctx, &policy)

	refresh, err := RefreshWorkspaceIndex(ctx, WorkspaceOptions{
		Root:               root,
		IndexPath:          indexPath,
		Vectorizer:         vectorizer,
		VectorizerMetadata: vectorizer.Metadata(),
	})

	require.Error(t, err)
	require.True(t, permission.ErrDenied(err))
	assert.Contains(t, err.Error(), "permission.write.deny")
	assert.False(t, refresh.Refreshed)
	assert.NoFileExists(t, indexPath)
}

func TestRefreshWorkspaceIndex_TightensReusableIndexPermissions(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	writeWorkspaceFile(t, root, "a.md", "alpha workspace retrieval")

	vectorizer, err := NewTextVectorizer(16)
	require.NoError(t, err)

	indexPath := filepath.Join(root, ".atteler", "workspace-index.json")
	opts := WorkspaceOptions{
		Root:               root,
		IndexPath:          indexPath,
		Vectorizer:         vectorizer,
		VectorizerMetadata: vectorizer.Metadata(),
		Chunk:              ChunkOptions{MaxRunes: 200, OverlapRunes: 20},
	}

	first, err := RefreshWorkspaceIndex(context.TODO(), opts)
	require.NoError(t, err)
	require.NotNil(t, first.Index)
	require.FileExists(t, indexPath)

	//nolint:gosec // Intentionally loosen permissions to prove reuse tightens them.
	require.NoError(t, os.Chmod(indexPath, 0o644))

	second, err := RefreshWorkspaceIndex(context.TODO(), opts)
	require.NoError(t, err)
	assert.False(t, second.Refreshed)
	assert.Equal(t, 1, second.Unchanged)

	info, err := os.Stat(indexPath)
	require.NoError(t, err)
	assert.Equal(t, os.FileMode(0o600), info.Mode().Perm())
}

func TestRefreshWorkspaceIndex_RemovesSourceWhenIgnoreRulesChange(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	writeWorkspaceFile(t, root, "a.md", "alpha workspace retrieval")
	writeWorkspaceFile(t, root, "b.md", "beta workspace retrieval")

	vectorizer, err := NewTextVectorizer(16)
	require.NoError(t, err)

	counting := &countingVectorizer{inner: vectorizer}
	opts := WorkspaceOptions{
		Root:               root,
		IndexPath:          filepath.Join(root, ".atteler", "workspace-index.json"),
		Vectorizer:         counting,
		VectorizerMetadata: vectorizer.Metadata(),
		Chunk:              ChunkOptions{MaxRunes: 200, OverlapRunes: 20},
	}

	first, err := RefreshWorkspaceIndex(context.TODO(), opts)
	require.NoError(t, err)
	assert.Equal(t, 2, first.Added)
	assert.Equal(t, 2, counting.calls)

	writeWorkspaceFile(t, root, ".attelerignore", "b.md\n")

	counting.calls = 0
	second, err := RefreshWorkspaceIndex(context.TODO(), opts)
	require.NoError(t, err)
	require.NotNil(t, second.Index)

	assert.True(t, second.Refreshed)
	assert.False(t, second.Rebuilt)
	assert.Equal(t, 1, second.Deleted)
	assert.Equal(t, 1, second.Unchanged)
	assert.Equal(t, 0, counting.calls, "ignore-only deletion should not re-vectorize retained sources")
	assertWorkspaceSkipped(t, second.Skipped, "b.md")

	loaded, err := LoadIndex(opts.IndexPath)
	require.NoError(t, err)
	assert.ElementsMatch(t, []string{"a.md"}, sourceMetadataPaths(loaded.Sources))
	assert.ElementsMatch(t, []string{"a.md#chunk=0000"}, documentIDs(loaded.Documents))
}

func TestRefreshWorkspaceIndex_RebuildsIndexWithDocumentMissingSourceMetadata(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	writeWorkspaceFile(t, root, "docs/auth.md", workspaceAuthText)

	vectorizer, err := NewTextVectorizer(16)
	require.NoError(t, err)

	stale, err := BuildIndex(
		context.TODO(),
		[]Source{
			{Path: "docs/auth.md", Text: workspaceAuthText},
			{Path: "docs/deleted.md", Text: "deleted workspace retrieval"},
		},
		vectorizer,
		vectorizer.Metadata(),
		ChunkOptions{MaxRunes: 200, OverlapRunes: 20},
		time.Unix(1, 0),
	)
	require.NoError(t, err)
	require.Len(t, stale.Sources, 2)

	// Simulate a stale/corrupt persisted workspace index where a deleted file's
	// document survived but its source metadata did not. Refresh must rebuild so
	// deleted content cannot remain retrievable from an otherwise "unchanged"
	// index.
	stale.Sources = stale.Sources[:1]

	indexPath := filepath.Join(root, ".atteler", "workspace-index.json")
	writeRawWorkspaceIndex(t, indexPath, stale)

	counting := &countingVectorizer{inner: vectorizer}
	refresh, err := RefreshWorkspaceIndex(context.TODO(), WorkspaceOptions{
		Root:               root,
		IndexPath:          indexPath,
		Vectorizer:         counting,
		VectorizerMetadata: vectorizer.Metadata(),
		Chunk:              ChunkOptions{MaxRunes: 200, OverlapRunes: 20},
	})
	require.NoError(t, err)
	require.NotNil(t, refresh.Index)
	assert.True(t, refresh.Rebuilt)
	assert.Equal(t, 1, counting.calls)

	loaded, err := LoadIndex(indexPath)
	require.NoError(t, err)
	assert.ElementsMatch(t, []string{"docs/auth.md"}, sourceMetadataPaths(loaded.Sources))
	assert.ElementsMatch(t, []string{"docs/auth.md#chunk=0000"}, documentIDs(loaded.Documents))
}

func TestDiscoverWorkspaceSourcesSkipsUnreadableSubdirectory(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	writeWorkspaceFile(t, root, "docs/auth.md", workspaceAuthText)
	writeWorkspaceFile(t, root, "private/notes.md", "private notes")

	privateDir := filepath.Join(root, "private")
	require.NoError(t, os.Chmod(privateDir, 0))
	t.Cleanup(func() {
		//nolint:gosec // Restore test directory permissions so t.TempDir cleanup can remove it.
		assert.NoError(t, os.Chmod(privateDir, 0o700))
	})

	sources, skipped, err := DiscoverWorkspaceSources(context.TODO(), WorkspaceOptions{Root: root})
	require.NoError(t, err)

	if workspaceSourcePathExists(sources, "private/notes.md") {
		t.Skip("filesystem permissions did not prevent reading unreadable test directory")
	}

	require.Len(t, sources, 1)
	assert.Equal(t, "docs/auth.md", sources[0].Path)
	assertWorkspaceSkipped(t, skipped, "private")
}

func TestRefreshWorkspaceIndex_RetainsRedactedSourcePaths(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	rawSecret := "abc123456789"
	writeWorkspaceFile(t, root, "docs/bearer "+rawSecret+"/auth.md", workspaceAuthText)

	vectorizer, err := NewTextVectorizer(16)
	require.NoError(t, err)

	counting := &countingVectorizer{inner: vectorizer}
	indexPath := filepath.Join(root, ".atteler", "workspace-index.json")
	opts := WorkspaceOptions{
		Root:               root,
		IndexPath:          indexPath,
		Vectorizer:         counting,
		VectorizerMetadata: vectorizer.Metadata(),
		Chunk:              ChunkOptions{MaxRunes: 200, OverlapRunes: 20},
	}

	first, err := RefreshWorkspaceIndex(context.TODO(), opts)
	require.NoError(t, err)
	assert.True(t, first.Refreshed)
	assert.Equal(t, 1, first.Added)

	loaded, err := LoadIndex(indexPath)
	require.NoError(t, err)
	require.Len(t, loaded.Sources, 1)
	require.Len(t, loaded.Documents, 1)
	assert.Equal(t, "docs/Bearer [REDACTED]/auth.md", loaded.Sources[0].Path)
	assert.Equal(t, loaded.Sources[0].Path, loaded.Documents[0].Metadata["path"])
	assert.NotContains(t, loaded.Sources[0].Path, rawSecret)

	counting.calls = 0
	second, err := RefreshWorkspaceIndex(context.TODO(), opts)
	require.NoError(t, err)
	assert.False(t, second.Refreshed)
	assert.Equal(t, 1, second.Unchanged)
	assert.Equal(t, 0, counting.calls)
}

func TestRefreshWorkspaceIndexRejectsIndexPathOutsideRoot(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	writeWorkspaceFile(t, root, "docs/auth.md", workspaceAuthText)

	outsideIndexPath := filepath.Join(t.TempDir(), "workspace-index.json")
	refresh, err := RefreshWorkspaceIndex(context.TODO(), WorkspaceOptions{
		Root:      root,
		IndexPath: outsideIndexPath,
	})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "must be inside workspace root")
	assert.Nil(t, refresh.Index)
	assert.NoFileExists(t, outsideIndexPath)
}

func TestRefreshWorkspaceIndexRejectsSymlinkedIndexDirectory(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	writeWorkspaceFile(t, root, "docs/auth.md", workspaceAuthText)

	outside := t.TempDir()

	link := filepath.Join(root, ".atteler")
	if err := os.Symlink(outside, link); err != nil {
		t.Skipf("symlink unavailable: %v", err)
	}

	refresh, err := RefreshWorkspaceIndex(context.TODO(), WorkspaceOptions{
		Root:      root,
		IndexPath: filepath.Join(root, ".atteler", "workspace-index.json"),
	})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "must not traverse symlink")
	assert.Nil(t, refresh.Index)
	assert.NoFileExists(t, filepath.Join(outside, "workspace-index.json"))
}

func TestRefreshWorkspaceIndexRefusesToOverwriteExistingNonIndexPath(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	writeWorkspaceFile(t, root, "docs/auth.md", workspaceAuthText)

	indexPath := filepath.Join(root, ".atteler", "config.json")
	original := `{"version":1,"documents":[]}`
	writeWorkspaceFile(t, root, ".atteler/config.json", original)

	refresh, err := RefreshWorkspaceIndex(context.TODO(), WorkspaceOptions{
		Root:      root,
		IndexPath: indexPath,
		Chunk:     ChunkOptions{MaxRunes: 200, OverlapRunes: 20},
	})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "refusing to overwrite existing non-index file")
	assert.Nil(t, refresh.Index)

	data, readErr := os.ReadFile(indexPath)
	require.NoError(t, readErr)
	assert.Equal(t, original, string(data))
}

func TestRefreshWorkspaceIndexDoesNotRemoveExistingNonIndexPathWhenWorkspaceHasNoSources(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	indexPath := filepath.Join(root, ".atteler", "config.json")
	original := `{"version":1,"documents":[]}`
	writeWorkspaceFile(t, root, ".atteler/config.json", original)

	refresh, err := RefreshWorkspaceIndex(context.TODO(), WorkspaceOptions{
		Root:      root,
		IndexPath: indexPath,
	})
	require.Error(t, err)
	require.ErrorIs(t, err, ErrNoSources)
	assert.False(t, refresh.Refreshed)

	data, readErr := os.ReadFile(indexPath)
	require.NoError(t, readErr)
	assert.Equal(t, original, string(data))
}

func TestRefreshWorkspaceIndex_RebuildsInvalidReusableIndex(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	writeWorkspaceFile(t, root, "docs/auth.md", workspaceAuthText)

	indexPath := filepath.Join(root, ".atteler", "workspace-index.json")
	require.NoError(t, os.MkdirAll(filepath.Dir(indexPath), 0o750))
	require.NoError(t, os.WriteFile(indexPath, []byte(`{
  "version": 1,
  "vectorizer": {"kind": "lexical", "model": "hashed-token-frequency"},
  "dimensions": 0,
  "chunk": {"max_runes": 200, "overlap_runes": 20},
  "sources": [{"path": "docs/auth.md", "digest": "stale", "bytes": 1}],
  "documents": []
}`), 0o600))

	vectorizer, err := NewTextVectorizer(16)
	require.NoError(t, err)

	refresh, err := RefreshWorkspaceIndex(context.TODO(), WorkspaceOptions{
		Root:               root,
		IndexPath:          indexPath,
		Vectorizer:         vectorizer,
		VectorizerMetadata: vectorizer.Metadata(),
		Chunk:              ChunkOptions{MaxRunes: 200, OverlapRunes: 20},
	})
	require.NoError(t, err)
	require.NotNil(t, refresh.Index)
	assert.True(t, refresh.Rebuilt)
	assert.False(t, refresh.Initialized)

	loaded, err := LoadIndex(indexPath)
	require.NoError(t, err)
	assert.ElementsMatch(t, []string{"docs/auth.md"}, sourceMetadataPaths(loaded.Sources))
}

func TestRefreshWorkspaceIndex_RebuildsCorruptIndex(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	writeWorkspaceFile(t, root, "docs/auth.md", workspaceAuthText)

	indexPath := filepath.Join(root, ".atteler", "workspace-index.json")
	require.NoError(t, os.MkdirAll(filepath.Dir(indexPath), 0o750))
	require.NoError(t, os.WriteFile(indexPath, []byte(`{"version":`), 0o600))

	refresh, err := RefreshWorkspaceIndex(context.TODO(), WorkspaceOptions{
		Root:      root,
		IndexPath: indexPath,
		Chunk:     ChunkOptions{MaxRunes: 200, OverlapRunes: 20},
	})
	require.NoError(t, err)
	require.NotNil(t, refresh.Index)
	assert.True(t, refresh.Rebuilt)
	assert.False(t, refresh.Initialized)

	loaded, err := LoadIndex(indexPath)
	require.NoError(t, err)
	assert.ElementsMatch(t, []string{"docs/auth.md"}, sourceMetadataPaths(loaded.Sources))
}

func TestRefreshWorkspaceIndex_RebuildsIndexWithDuplicateDocumentIDs(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	writeWorkspaceFile(t, root, "docs/auth.md", workspaceAuthText)

	indexPath := filepath.Join(root, ".atteler", "workspace-index.json")
	require.NoError(t, os.MkdirAll(filepath.Dir(indexPath), 0o750))

	duplicateIndex := fmt.Sprintf(`{
  "version": 1,
  "vectorizer": {"kind": "lexical", "model": "hashed-token-frequency", "dimensions": 2},
  "dimensions": 2,
  "chunk": {"max_runes": 200, "overlap_runes": 20},
  "sources": [%s],
  "documents": [
    {"id": "docs/auth.md#chunk=0000", "text": %q, "vector": [1, 0], "metadata": {"path": "docs/auth.md"}},
    {"id": "docs/auth.md#chunk=0000", "text": %q, "vector": [0, 1], "metadata": {"path": "docs/auth.md"}}
  ]
}`, mustJSON(t, SourceMetadataForText("docs/auth.md", workspaceAuthText)), workspaceAuthText, workspaceAuthText)
	require.NoError(t, os.WriteFile(indexPath, []byte(duplicateIndex), 0o600))

	vectorizer, err := NewTextVectorizer(16)
	require.NoError(t, err)

	refresh, err := RefreshWorkspaceIndex(context.TODO(), WorkspaceOptions{
		Root:               root,
		IndexPath:          indexPath,
		Vectorizer:         vectorizer,
		VectorizerMetadata: vectorizer.Metadata(),
		Chunk:              ChunkOptions{MaxRunes: 200, OverlapRunes: 20},
	})
	require.NoError(t, err)
	require.NotNil(t, refresh.Index)
	assert.True(t, refresh.Rebuilt)

	loaded, err := LoadIndex(indexPath)
	require.NoError(t, err)
	assert.ElementsMatch(t, []string{"docs/auth.md#chunk=0000"}, documentIDs(loaded.Documents))
}

func TestRefreshWorkspaceIndex_RebuildsIndexWithInvalidDocumentVectorizer(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	writeWorkspaceFile(t, root, "docs/auth.md", workspaceAuthText)

	indexPath := filepath.Join(root, ".atteler", "workspace-index.json")
	require.NoError(t, os.MkdirAll(filepath.Dir(indexPath), 0o750))

	stale := fmt.Sprintf(`{
  "version": 1,
  "vectorizer": {"kind": "lexical", "model": "hashed-token-frequency", "dimensions": 2},
  "dimensions": 2,
  "chunk": {"max_runes": 200, "overlap_runes": 20},
  "sources": [%s],
  "documents": [{
    "id": "docs/auth.md#chunk=0000",
    "text": %q,
    "source_hash": %q,
    "vector": [1, 0],
    "vectorizer": {"id": %q, "dimensions": 2},
    "metadata": {"path": "docs/auth.md"},
    "provenance": {"source_type": "file", "privacy_policy": %q}
  }]
}`, mustJSON(t, SourceMetadataForText("docs/auth.md", workspaceAuthText)), workspaceAuthText, sourceHash(workspaceAuthText), TextHashVectorizerID, privacy.RedactionPolicyVersion)
	require.NoError(t, os.WriteFile(indexPath, []byte(stale), 0o600))

	vectorizer, err := NewTextVectorizer(2)
	require.NoError(t, err)

	refresh, err := RefreshWorkspaceIndex(context.TODO(), WorkspaceOptions{
		Root:               root,
		IndexPath:          indexPath,
		Vectorizer:         vectorizer,
		VectorizerMetadata: vectorizer.Metadata(),
		Chunk:              ChunkOptions{MaxRunes: 200, OverlapRunes: 20},
	})
	require.NoError(t, err)
	require.NotNil(t, refresh.Index)
	assert.True(t, refresh.Rebuilt)

	loaded, err := LoadIndex(indexPath)
	require.NoError(t, err)
	require.Len(t, loaded.Documents, 1)
	assert.Equal(t, TextHashVectorizerID, loaded.Documents[0].Vectorizer.ID)
	assert.Equal(t, TextHashVectorizerModel, loaded.Documents[0].Vectorizer.Model)
}

func TestRefreshWorkspaceIndex_RemovesInvalidIndexWhenWorkspaceHasNoSources(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	indexPath := filepath.Join(root, ".atteler", "workspace-index.json")
	require.NoError(t, os.MkdirAll(filepath.Dir(indexPath), 0o750))
	require.NoError(t, os.WriteFile(indexPath, []byte(`{
  "version": 1,
  "vectorizer": {"kind": "lexical", "model": "hashed-token-frequency"},
  "dimensions": 0,
  "chunk": {"max_runes": 200, "overlap_runes": 20},
  "sources": [{"path": "deleted.md", "digest": "stale", "bytes": 1}],
  "documents": []
}`), 0o600))

	refresh, err := RefreshWorkspaceIndex(context.TODO(), WorkspaceOptions{Root: root, IndexPath: indexPath})
	require.Error(t, err)
	require.ErrorIs(t, err, ErrNoSources)
	assert.True(t, refresh.Refreshed)

	_, statErr := os.Stat(indexPath)
	assert.ErrorIs(t, statErr, os.ErrNotExist)
}

func TestRefreshWorkspaceIndex_RebuildsIndexWithPrivateVectorizerMetadata(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	text := workspaceAuthText
	writeWorkspaceFile(t, root, "docs/auth.md", text)

	indexPath := filepath.Join(root, ".atteler", "workspace-index.json")
	require.NoError(t, os.MkdirAll(filepath.Dir(indexPath), 0o750))

	staleIndex := fmt.Sprintf(`{
  "version": 1,
  "vectorizer": {"kind": "lexical", "model": "api_key=secret-token"},
  "dimensions": 2,
  "chunk": {"max_runes": 200, "overlap_runes": 20},
  "sources": [{"path": "docs/auth.md", "digest": "stale", "bytes": 1}],
  "documents": [{
    "id": "docs/auth.md#chunk=0000",
    "text": %q,
    "source_hash": %q,
    "vector": [1, 0],
    "metadata": {"path": "docs/auth.md"},
    "provenance": {"source_type": "file", "privacy_policy": "atteler-redaction-v1"}
  }]
}`, text, sourceHash(text))
	require.NoError(t, os.WriteFile(indexPath, []byte(staleIndex), 0o600))

	vectorizer, err := NewTextVectorizer(16)
	require.NoError(t, err)

	refresh, err := RefreshWorkspaceIndex(context.TODO(), WorkspaceOptions{
		Root:               root,
		IndexPath:          indexPath,
		Vectorizer:         vectorizer,
		VectorizerMetadata: vectorizer.Metadata(),
		Chunk:              ChunkOptions{MaxRunes: 200, OverlapRunes: 20},
	})
	require.NoError(t, err)
	assert.True(t, refresh.Rebuilt)

	data, err := os.ReadFile(indexPath)
	require.NoError(t, err)
	assert.NotContains(t, string(data), "secret-token")
}

func TestRefreshWorkspaceIndex_RemovesFallbackIndexWhenWorkspaceHasNoSources(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	indexPath := filepath.Join(root, ".atteler", "workspace-index.json")
	fallbackPath := workspaceLexicalFallbackIndexPath(indexPath)
	require.NoError(t, os.MkdirAll(filepath.Dir(fallbackPath), 0o750))
	require.NoError(t, os.WriteFile(fallbackPath, []byte(`{"stale":true}`), 0o600))

	refresh, err := RefreshWorkspaceIndex(context.TODO(), WorkspaceOptions{Root: root, IndexPath: indexPath})
	require.Error(t, err)
	require.ErrorIs(t, err, ErrNoSources)
	assert.True(t, refresh.Refreshed)

	_, statErr := os.Stat(fallbackPath)
	assert.ErrorIs(t, statErr, os.ErrNotExist)
}

func TestWorkspaceLexicalFallbackIndexPathIsIdempotent(t *testing.T) {
	t.Parallel()

	assert.Equal(t, ".atteler/workspace-index.lexical.json", workspaceLexicalFallbackIndexPath(".atteler/workspace-index.json"))
	assert.Equal(t, ".atteler/workspace-index.lexical", workspaceLexicalFallbackIndexPath(".atteler/workspace-index"))
	assert.Equal(t, ".atteler/workspace-index.lexical.json", workspaceLexicalFallbackIndexPath(".atteler/workspace-index.lexical.json"))
	assert.Equal(t, ".atteler/workspace-index.lexical", workspaceLexicalFallbackIndexPath(".atteler/workspace-index.lexical"))
}

func TestRefreshWorkspaceIndex_ReindexesLegacyDocumentsWithoutProvenance(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	text := workspaceAuthText
	writeWorkspaceFile(t, root, "docs/auth.md", text)

	indexPath := filepath.Join(root, ".atteler", "workspace-index.json")
	vectorizer, err := NewTextVectorizer(16)
	require.NoError(t, err)

	vec, err := vectorizer.Vectorize(text)
	require.NoError(t, err)

	legacy := &Index{
		Version:    IndexVersion,
		CreatedAt:  time.Unix(1, 0).UTC(),
		Vectorizer: vectorizer.Metadata(),
		Dimensions: len(vec),
		Chunk:      ChunkOptions{MaxRunes: 200, OverlapRunes: 20}.Normalize(),
		Sources:    []SourceMetadata{SourceMetadataForText("docs/auth.md", text)},
		Documents: []Document{{
			ID:       "docs/auth.md#chunk=0000",
			Text:     text,
			Vector:   vec,
			Metadata: map[string]string{"path": "docs/auth.md"},
		}},
	}
	writeRawWorkspaceIndex(t, indexPath, legacy)

	counting := &countingVectorizer{inner: vectorizer}
	refresh, err := RefreshWorkspaceIndex(context.TODO(), WorkspaceOptions{
		Root:               root,
		IndexPath:          indexPath,
		Vectorizer:         counting,
		VectorizerMetadata: vectorizer.Metadata(),
		Chunk:              ChunkOptions{MaxRunes: 200, OverlapRunes: 20},
	})
	require.NoError(t, err)

	assert.True(t, refresh.Refreshed)
	assert.Equal(t, 1, refresh.Updated)
	assert.Equal(t, 1, counting.calls)

	loaded, err := LoadIndex(indexPath)
	require.NoError(t, err)
	require.Len(t, loaded.Documents, 1)
	assert.Equal(t, "file", loaded.Documents[0].Provenance["source_type"])
	assert.Equal(t, privacy.RedactionPolicyVersion, loaded.Documents[0].Provenance["privacy_policy"])
	assert.Equal(t, sourceHash(text), loaded.Documents[0].SourceHash)
}

func TestRefreshWorkspaceIndex_ReindexesLegacyDocumentsWithoutSourceHash(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	text := "Shell workspace retrieval"
	writeWorkspaceFile(t, root, "docs/shell.md", text)

	indexPath := filepath.Join(root, ".atteler", "workspace-index.json")
	vectorizer, err := NewTextVectorizer(16)
	require.NoError(t, err)

	vec, err := vectorizer.Vectorize(text)
	require.NoError(t, err)

	legacy := &Index{
		Version:    IndexVersion,
		CreatedAt:  time.Unix(1, 0).UTC(),
		Vectorizer: vectorizer.Metadata(),
		Dimensions: len(vec),
		Chunk:      ChunkOptions{MaxRunes: 200, OverlapRunes: 20}.Normalize(),
		Sources:    []SourceMetadata{SourceMetadataForText("docs/shell.md", text)},
		Documents: []Document{{
			ID:         "docs/shell.md#chunk=0000",
			Text:       text,
			Vector:     vec,
			Metadata:   map[string]string{"path": "docs/shell.md"},
			Provenance: ensureProvenance(map[string]string{"source_type": "file"}, "file"),
		}},
	}
	writeRawWorkspaceIndex(t, indexPath, legacy)

	counting := &countingVectorizer{inner: vectorizer}
	refresh, err := RefreshWorkspaceIndex(context.TODO(), WorkspaceOptions{
		Root:               root,
		IndexPath:          indexPath,
		Vectorizer:         counting,
		VectorizerMetadata: vectorizer.Metadata(),
		Chunk:              ChunkOptions{MaxRunes: 200, OverlapRunes: 20},
	})
	require.NoError(t, err)

	assert.True(t, refresh.Refreshed)
	assert.Equal(t, 1, refresh.Updated)
	assert.Equal(t, 1, counting.calls)

	loaded, err := LoadIndex(indexPath)
	require.NoError(t, err)
	require.Len(t, loaded.Documents, 1)
	assert.Equal(t, sourceHash(text), loaded.Documents[0].SourceHash)
}

func TestRefreshWorkspaceIndex_ReindexesLegacyDocumentsWithoutText(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	text := "Agent workspace retrieval"
	writeWorkspaceFile(t, root, "docs/agent.md", text)

	indexPath := filepath.Join(root, ".atteler", "workspace-index.json")
	vectorizer, err := NewTextVectorizer(16)
	require.NoError(t, err)

	vec, err := vectorizer.Vectorize(text)
	require.NoError(t, err)

	legacy := &Index{
		Version:    IndexVersion,
		CreatedAt:  time.Unix(1, 0).UTC(),
		Vectorizer: vectorizer.Metadata(),
		Dimensions: len(vec),
		Chunk:      ChunkOptions{MaxRunes: 200, OverlapRunes: 20}.Normalize(),
		Sources:    []SourceMetadata{SourceMetadataForText("docs/agent.md", text)},
		Documents: []Document{{
			ID:         "docs/agent.md#chunk=0000",
			SourceHash: sourceHash(text),
			Vector:     vec,
			Metadata:   map[string]string{"path": "docs/agent.md"},
			Provenance: ensureProvenance(map[string]string{"source_type": "file"}, "file"),
		}},
	}
	writeRawWorkspaceIndex(t, indexPath, legacy)

	counting := &countingVectorizer{inner: vectorizer}
	refresh, err := RefreshWorkspaceIndex(context.TODO(), WorkspaceOptions{
		Root:               root,
		IndexPath:          indexPath,
		Vectorizer:         counting,
		VectorizerMetadata: vectorizer.Metadata(),
		Chunk:              ChunkOptions{MaxRunes: 200, OverlapRunes: 20},
	})
	require.NoError(t, err)

	assert.True(t, refresh.Refreshed)
	assert.Equal(t, 1, refresh.Updated)
	assert.Equal(t, 1, counting.calls)

	loaded, err := LoadIndex(indexPath)
	require.NoError(t, err)
	require.Len(t, loaded.Documents, 1)
	assert.Equal(t, text, loaded.Documents[0].Text)
}

func TestRefreshWorkspaceIndex_ReindexesReusableDocumentWithMismatchedTextHashVector(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	text := workspaceAuthText
	writeWorkspaceFile(t, root, "docs/auth.md", text)

	indexPath := filepath.Join(root, ".atteler", "workspace-index.json")
	vectorizer, err := NewTextVectorizer(16)
	require.NoError(t, err)

	_, err = RefreshWorkspaceIndex(context.TODO(), WorkspaceOptions{
		Root:               root,
		IndexPath:          indexPath,
		Vectorizer:         vectorizer,
		VectorizerMetadata: vectorizer.Metadata(),
		Chunk:              ChunkOptions{MaxRunes: 200, OverlapRunes: 20},
	})
	require.NoError(t, err)

	loaded, err := LoadIndex(indexPath)
	require.NoError(t, err)
	require.NotEmpty(t, loaded.Documents[0].Vector)

	for i, value := range loaded.Documents[0].Vector {
		if value != 0 {
			loaded.Documents[0].Vector[i] = -value
			break
		}
	}

	data, err := json.MarshalIndent(loaded, "", "  ")
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(indexPath, append(data, '\n'), 0o600))

	counting := &countingVectorizer{inner: vectorizer}
	refresh, err := RefreshWorkspaceIndex(context.TODO(), WorkspaceOptions{
		Root:               root,
		IndexPath:          indexPath,
		Vectorizer:         counting,
		VectorizerMetadata: vectorizer.Metadata(),
		Chunk:              ChunkOptions{MaxRunes: 200, OverlapRunes: 20},
	})
	require.NoError(t, err)

	assert.True(t, refresh.Refreshed)
	assert.Equal(t, 1, refresh.Updated)
	assert.Equal(t, 1, counting.calls)

	loaded, err = LoadIndex(indexPath)
	require.NoError(t, err)
	assert.True(t, workspaceLexicalVectorMatchesText(loaded.Documents[0]))
}

func TestRefreshWorkspaceIndex_ReindexesReusableDocumentWithStaleChunkText(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	text := workspaceAuthText
	writeWorkspaceFile(t, root, "docs/auth.md", text)

	indexPath := filepath.Join(root, ".atteler", "workspace-index.json")
	vectorizer, err := NewTextVectorizer(16)
	require.NoError(t, err)

	_, err = RefreshWorkspaceIndex(context.TODO(), WorkspaceOptions{
		Root:               root,
		IndexPath:          indexPath,
		Vectorizer:         vectorizer,
		VectorizerMetadata: vectorizer.Metadata(),
		Chunk:              ChunkOptions{MaxRunes: 200, OverlapRunes: 20},
	})
	require.NoError(t, err)

	loaded, err := LoadIndex(indexPath)
	require.NoError(t, err)
	require.Len(t, loaded.Documents, 1)

	staleText := "Shell workspace retrieval stale chunk"
	staleVector, err := vectorizer.Vectorize(staleText)
	require.NoError(t, err)

	loaded.Documents[0].Text = staleText
	loaded.Documents[0].SourceHash = sourceHash(staleText)
	loaded.Documents[0].Vector = staleVector
	writeRawWorkspaceIndex(t, indexPath, loaded)

	counting := &countingVectorizer{inner: vectorizer}
	refresh, err := RefreshWorkspaceIndex(context.TODO(), WorkspaceOptions{
		Root:               root,
		IndexPath:          indexPath,
		Vectorizer:         counting,
		VectorizerMetadata: vectorizer.Metadata(),
		Chunk:              ChunkOptions{MaxRunes: 200, OverlapRunes: 20},
	})
	require.NoError(t, err)

	assert.True(t, refresh.Refreshed)
	assert.Equal(t, 1, refresh.Updated)
	assert.Equal(t, 1, counting.calls)

	loaded, err = LoadIndex(indexPath)
	require.NoError(t, err)
	require.Len(t, loaded.Documents, 1)
	assert.Equal(t, text, loaded.Documents[0].Text)
	assert.NotContains(t, loaded.Documents[0].Text, staleText)
}

func TestRefreshWorkspaceIndex_ReindexesReusableDocumentWithMissingSafetyMetadata(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	text := workspaceSecretText
	writeWorkspaceFile(t, root, "docs/auth.md", text)

	indexPath := filepath.Join(root, ".atteler", "workspace-index.json")
	vectorizer, err := NewTextVectorizer(16)
	require.NoError(t, err)

	_, err = RefreshWorkspaceIndex(context.TODO(), WorkspaceOptions{
		Root:               root,
		IndexPath:          indexPath,
		Vectorizer:         vectorizer,
		VectorizerMetadata: vectorizer.Metadata(),
		Chunk:              ChunkOptions{MaxRunes: 200, OverlapRunes: 20},
	})
	require.NoError(t, err)

	loaded, err := LoadIndex(indexPath)
	require.NoError(t, err)
	require.Len(t, loaded.Documents, 1)
	assert.Equal(t, "false", loaded.Documents[0].Metadata[retrieval.MetadataSafetyInjectAllowed])
	assert.Equal(t, "true", loaded.Documents[0].Metadata[retrieval.MetadataSafetyRedacted])

	delete(loaded.Documents[0].Metadata, retrieval.MetadataSafetyInjectAllowed)
	delete(loaded.Documents[0].Metadata, retrieval.MetadataSafetyRedacted)
	delete(loaded.Documents[0].Metadata, retrieval.MetadataSafetySensitive)
	delete(loaded.Documents[0].Metadata, retrieval.MetadataSafetyReasons)
	writeRawWorkspaceIndex(t, indexPath, loaded)

	counting := &countingVectorizer{inner: vectorizer}
	refresh, err := RefreshWorkspaceIndex(context.TODO(), WorkspaceOptions{
		Root:               root,
		IndexPath:          indexPath,
		Vectorizer:         counting,
		VectorizerMetadata: vectorizer.Metadata(),
		Chunk:              ChunkOptions{MaxRunes: 200, OverlapRunes: 20},
	})
	require.NoError(t, err)

	assert.True(t, refresh.Refreshed)
	assert.Equal(t, 1, refresh.Updated)
	assert.Equal(t, 1, counting.calls)

	loaded, err = LoadIndex(indexPath)
	require.NoError(t, err)
	require.Len(t, loaded.Documents, 1)
	assert.Equal(t, "false", loaded.Documents[0].Metadata[retrieval.MetadataSafetyInjectAllowed])
	assert.Equal(t, "true", loaded.Documents[0].Metadata[retrieval.MetadataSafetyRedacted])
	assert.NotContains(t, loaded.Documents[0].Text, "super-secret-token")
}

func TestWorkspaceIndexSearcher_ReturnsANNRetrievalResults(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	writeWorkspaceFile(t, root, "docs/auth.md", "OAuth callbacks validate state before retrying token exchange failures.")
	writeWorkspaceFile(t, root, "docs/shell.md", "Shell commands capture process output and timeout failures.")

	vectorizer, err := NewTextVectorizer(64)
	require.NoError(t, err)

	refresh, err := RefreshWorkspaceIndex(context.TODO(), WorkspaceOptions{
		Root:               root,
		IndexPath:          filepath.Join(root, ".atteler", "workspace-index.json"),
		Vectorizer:         vectorizer,
		VectorizerMetadata: vectorizer.Metadata(),
		Chunk:              ChunkOptions{MaxRunes: 200},
	})
	require.NoError(t, err)

	results, err := retrieval.Search(context.TODO(), retrieval.Query{
		Text:  "Where do OAuth callbacks validate state?",
		Limit: 1,
	}, IndexSearcher{
		Index:      refresh.Index,
		Vectorizer: vectorizer,
		Source:     retrieval.Source{Type: retrieval.SourceVector, Name: "workspace", URI: root},
		ANN:        ANNOptions{BucketBits: 4, Probes: 4, MinCandidates: 1, CandidateMultiplier: 1},
	})
	require.NoError(t, err)
	require.Len(t, results, 1)
	assert.Equal(t, "docs/auth.md", results[0].Metadata["path"])
	assert.Contains(t, results[0].Snippet, "OAuth")
}

func TestIndexSearcher_EmbeddingIndexRequiresConfiguredVectorizer(t *testing.T) {
	t.Parallel()

	index := &Index{
		Version:    IndexVersion,
		CreatedAt:  time.Unix(1, 0).UTC(),
		Vectorizer: NewEmbeddingMetadata("ollama", "nomic-embed-text", "http://127.0.0.1:11434", 2),
		Dimensions: 2,
		Chunk:      ChunkOptions{MaxRunes: 100, OverlapRunes: 10},
		Sources: []SourceMetadata{{
			Path:   "docs/auth.md",
			Digest: DigestText("OAuth callback state validation"),
			Bytes:  31,
		}},
		Documents: []Document{{
			ID:       "docs/auth.md#chunk=0000",
			Text:     "OAuth callback state validation",
			Vector:   Vector{1, 0},
			Metadata: map[string]string{"path": "docs/auth.md"},
		}},
	}

	_, err := IndexSearcher{Index: index}.SearchRetrieval(context.TODO(), retrieval.Query{
		Text:  "OAuth state",
		Limit: 1,
	})
	require.Error(t, err)
	assert.ErrorIs(t, err, ErrMetadataMismatch)
}

func TestIndexSearcher_RejectsNilContext(t *testing.T) {
	t.Parallel()

	_, err := IndexSearcher{Index: &Index{}}.SearchRetrieval(nil, retrieval.Query{Text: "OAuth"}) //nolint:staticcheck // Verify nil contexts are rejected instead of panicking.
	require.Error(t, err)
	require.ErrorIs(t, err, ErrContextRequired)
}

func writeWorkspaceFile(t *testing.T, root, rel, content string) {
	t.Helper()

	path := filepath.Join(root, rel)
	require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o750))
	require.NoError(t, os.WriteFile(path, []byte(content), 0o600))
}

func assertWorkspaceSkipped(t *testing.T, skipped []WorkspaceSkip, path string) {
	t.Helper()

	for _, skip := range skipped {
		if skip.Path == path {
			return
		}
	}

	require.Failf(t, "path was not skipped", "%s not in %+v", path, skipped)
}

func mustJSON(t *testing.T, value any) string {
	t.Helper()

	data, err := json.Marshal(value)
	require.NoError(t, err)

	return string(data)
}

func writeRawWorkspaceIndex(t *testing.T, path string, idx *Index) {
	t.Helper()

	data, err := json.MarshalIndent(idx, "", "  ")
	require.NoError(t, err)
	require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o750))
	require.NoError(t, os.WriteFile(path, append(data, '\n'), 0o600))
}

type countingVectorizer struct {
	inner *TextVectorizer
	calls int
}

type stubDirEntry struct {
	info fs.FileInfo
	name string
}

func (e stubDirEntry) Name() string {
	return e.name
}

func (e stubDirEntry) IsDir() bool {
	return false
}

func (e stubDirEntry) Type() fs.FileMode {
	return 0
}

func (e stubDirEntry) Info() (fs.FileInfo, error) {
	return e.info, nil
}

type stubFileInfo struct {
	name string
	size int64
	mode fs.FileMode
}

func (i stubFileInfo) Name() string {
	return i.name
}

func (i stubFileInfo) Size() int64 {
	return i.size
}

func (i stubFileInfo) Mode() fs.FileMode {
	if i.mode != 0 {
		return i.mode
	}

	return 0o600
}

func (i stubFileInfo) ModTime() time.Time {
	return time.Unix(1, 0).UTC()
}

func (i stubFileInfo) IsDir() bool {
	return false
}

func (i stubFileInfo) Sys() any {
	return nil
}

func (v *countingVectorizer) Vectorize(text string) (Vector, error) {
	v.calls++

	return v.inner.Vectorize(text)
}

func sourceMetadataPaths(sources []SourceMetadata) []string {
	out := make([]string, 0, len(sources))
	for _, source := range sources {
		out = append(out, source.Path)
	}

	return out
}

func sourcePaths(sources []Source) []string {
	out := make([]string, 0, len(sources))
	for _, source := range sources {
		out = append(out, source.Path)
	}

	return out
}

func workspaceSourcePathExists(sources []Source, path string) bool {
	for _, source := range sources {
		if source.Path == path {
			return true
		}
	}

	return false
}

func documentIDs(docs []Document) []string {
	out := make([]string, 0, len(docs))
	for i := range docs {
		doc := docs[i]
		out = append(out, doc.ID)
	}

	return out
}
