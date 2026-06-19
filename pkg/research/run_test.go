//nolint:wsl_v5 // Tests keep setup/assertion blocks compact for artifact readability.
package research

import (
	"bufio"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/tommoulard/atteler/pkg/sourcepolicy"
)

func TestRun_CreatesArtifactsAndReadsGuidance(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(root, "AGENTS.md"), []byte("# Agent rules\nPrefer tests and cite evidence.\n"), 0o600))
	require.NoError(t, os.MkdirAll(filepath.Join(root, ".cursor", "rules"), 0o750))
	require.NoError(t, os.WriteFile(filepath.Join(root, ".cursor", "rules", "style.mdc"), []byte("# Cursor style\nUse small Go packages.\n"), 0o600))
	require.NoError(t, os.WriteFile(filepath.Join(root, "notes.md"), []byte("# Plugin sandboxing notes\nProcess isolation is relevant to plugin sandboxing.\n"), 0o600))

	result, err := Run(context.Background(), RunRequest{
		Question:       "Compare plugin sandboxing approaches",
		Root:           root,
		OutputDir:      "research/plugin-sandboxing",
		TrustedSources: []string{"go.dev", "github.com"},
		Sources:        []string{"notes.md", "https://go.dev/doc/"},
		GenerateTasks:  true,
		Now:            time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC),
	})
	require.NoError(t, err)

	assert.Equal(t, "plugin-sandboxing", result.RunID)
	assert.Equal(t, filepath.Join(root, "research", "plugin-sandboxing"), result.Dir)

	for _, name := range []string{researchReportFile, sourcesFile, claimsFile, tasksFile, runFile} {
		assert.FileExists(t, filepath.Join(result.Dir, name))
	}

	report := readFile(t, filepath.Join(result.Dir, researchReportFile))
	assert.Contains(t, report, "## Summary")
	assert.Contains(t, report, "## Key findings")
	assert.Contains(t, report, "Atteler research reports should include evidence")
	assert.Contains(t, report, "[S1]")
	assert.Contains(t, report, "Process isolation is relevant")

	sources := readSources(t, filepath.Join(result.Dir, sourcesFile))
	require.GreaterOrEqual(t, len(sources), 4)
	assert.Contains(t, sourcePaths(sources), "AGENTS.md")
	assert.Contains(t, sourcePaths(sources), ".cursor/rules/style.mdc")
	assert.Contains(t, sourcePaths(sources), "notes.md")
	assert.Contains(t, sourceURLs(sources), "https://go.dev/doc/")
	assert.Equal(t, sourcepolicy.SourceTypeSourceCode, sourceByPath(t, sources, "notes.md").SourceType)
	assert.Equal(t, "official_docs", sourceByURL(t, sources, "https://go.dev/doc/").SourceType)
	assert.Equal(t, "high", sourceByURL(t, sources, "https://go.dev/doc/").TrustLevel)
	assert.Equal(t, "trusted_domain", sourceByURL(t, sources, "https://go.dev/doc/").PolicyMatch)
	assert.InEpsilon(t, trustedURLTrustScore, sourceByURL(t, sources, "https://go.dev/doc/").TrustScore, 0.001)

	claims := readClaims(t, filepath.Join(result.Dir, claimsFile))
	require.NotEmpty(t, claims)
	assert.True(t, hasEvidencePath(claims, "AGENTS.md"), "expected AGENTS.md evidence in claims: %#v", claims)
	assert.True(t, hasEvidencePath(claims, runFile), "expected run.json evidence in claims: %#v", claims)

	tasks := readFile(t, filepath.Join(result.Dir, tasksFile))
	assert.Contains(t, tasks, "tasks:")
	assert.Contains(t, tasks, "Review research findings")

	var record runRecord
	require.NoError(t, json.Unmarshal([]byte(readFile(t, filepath.Join(result.Dir, runFile))), &record))
	assert.Equal(t, SchemaVersion, record.Schema)
	assert.Equal(t, "Compare plugin sandboxing approaches", record.Question)
	assert.True(t, record.GenerateTasks)
	assert.Contains(t, record.TrustedSources, "go.dev")
	assert.Contains(t, record.SourcePolicy.TrustedDomains, "go.dev")
	assert.Contains(t, record.SourcePolicy.TrustedDomains, "github.com")
	assert.True(t, record.SourcePolicy.RequireEvidenceForHighImpactClaims)
}

func TestRun_DefaultOutputDirUsesResearchRunsRoot(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	result, err := Run(context.Background(), RunRequest{
		Question: "Research safe worktrees",
		Root:     root,
		Now:      time.Date(2026, 1, 2, 3, 4, 5, 6, time.UTC),
	})
	require.NoError(t, err)

	assert.Contains(t, result.Dir, filepath.Join(root, ".atteler", "runs", "research"))
	assert.FileExists(t, filepath.Join(result.Dir, researchReportFile))
	assert.NotEmpty(t, result.RunID)
}

func TestRun_RequiresQuestion(t *testing.T) {
	t.Parallel()

	_, err := Run(context.Background(), RunRequest{Root: t.TempDir()})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "question is required")
}

func TestRun_SourcePolicyExcludesDeniedAndWarnsLowTrust(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	result, err := Run(context.Background(), RunRequest{
		Question: "Compare OAuth callback advice",
		Root:     root,
		Sources: []string{
			"https://example-content-farm.com/oauth",
			"https://stackoverflow.com/questions/1",
			"https://docs.github.com/en/apps",
		},
		SourcePolicy: sourcepolicy.Policy{
			TrustedDomains:        []string{"docs.github.com"},
			DeniedDomains:         []string{"example-content-farm.com"},
			WarnOnLowTrustSources: sourcepolicy.Bool(true),
		},
		Now: time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC),
	})
	require.NoError(t, err)

	sources := readSources(t, filepath.Join(result.Dir, sourcesFile))
	assert.NotContains(t, sourceURLs(sources), "https://example-content-farm.com/oauth")
	assert.Contains(t, sourceURLs(sources), "https://stackoverflow.com/questions/1")
	lowTrust := sourceByURL(t, sources, "https://stackoverflow.com/questions/1")
	assert.Equal(t, "low", lowTrust.TrustLevel)
	assert.NotEmpty(t, lowTrust.Warnings)
	trusted := sourceByURL(t, sources, "https://docs.github.com/en/apps")
	assert.Equal(t, "trusted_domain", trusted.PolicyMatch)

	var record runRecord
	require.NoError(t, json.Unmarshal([]byte(readFile(t, filepath.Join(result.Dir, runFile))), &record))
	require.Len(t, record.Excluded, 1)
	assert.Equal(t, "example-content-farm.com", record.Excluded[0].Domain)
	assert.Equal(t, "denied_domain", record.Excluded[0].PolicyMatch)

	report := readFile(t, filepath.Join(result.Dir, researchReportFile))
	assert.Contains(t, report, "## Source quality")
	assert.Contains(t, report, "low-trust")
	assert.Contains(t, report, "Excluded source inputs: 1")
}

func readFile(t *testing.T, path string) string {
	t.Helper()

	data, err := os.ReadFile(path)
	require.NoError(t, err)

	return string(data)
}

func readSources(t *testing.T, path string) []Source {
	t.Helper()

	file, err := os.Open(path)
	require.NoError(t, err)
	defer file.Close()

	var out []Source
	scanner := bufio.NewScanner(file)
	for scanner.Scan() {
		var source Source
		require.NoError(t, json.Unmarshal(scanner.Bytes(), &source))
		out = append(out, source)
	}
	require.NoError(t, scanner.Err())

	return out
}

func readClaims(t *testing.T, path string) []Claim {
	t.Helper()

	file, err := os.Open(path)
	require.NoError(t, err)
	defer file.Close()

	var out []Claim
	scanner := bufio.NewScanner(file)
	for scanner.Scan() {
		var claim Claim
		require.NoError(t, json.Unmarshal(scanner.Bytes(), &claim))
		out = append(out, claim)
	}
	require.NoError(t, scanner.Err())

	return out
}

func sourcePaths(sources []Source) []string {
	out := make([]string, 0, len(sources))
	for i := range sources {
		if sources[i].Path != "" {
			out = append(out, sources[i].Path)
		}
	}

	return out
}

func sourceURLs(sources []Source) []string {
	out := make([]string, 0, len(sources))
	for i := range sources {
		if sources[i].URL != "" {
			out = append(out, sources[i].URL)
		}
	}

	return out
}

func sourceByURL(t *testing.T, sources []Source, target string) Source {
	t.Helper()

	for i := range sources {
		if sources[i].URL == target {
			return sources[i]
		}
	}

	require.Fail(t, "source URL not found", target)
	return Source{}
}

func sourceByPath(t *testing.T, sources []Source, target string) Source {
	t.Helper()

	for i := range sources {
		if sources[i].Path == target {
			return sources[i]
		}
	}

	require.Fail(t, "source path not found", target)
	return Source{}
}

func hasEvidencePath(claims []Claim, want string) bool {
	for _, claim := range claims {
		for _, evidence := range claim.Evidence {
			if evidence.Path == want || strings.HasSuffix(evidence.Path, "/"+want) {
				return true
			}
		}
	}

	return false
}
