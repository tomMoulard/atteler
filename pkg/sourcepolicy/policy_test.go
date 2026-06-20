package sourcepolicy

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestEvaluate_DeniedDomainExcludesSource(t *testing.T) {
	t.Parallel()

	evaluation := Evaluate(Source{URL: "https://spam.example.com/post"}, Policy{
		TrustedDomains: []string{"example.com"},
		DeniedDomains:  []string{"spam.example.com"},
	})

	assert.False(t, evaluation.Allowed)
	assert.Equal(t, TrustLevelDenied, evaluation.Quality.TrustLevel)
	assert.Equal(t, PolicyMatchDeniedDomain, evaluation.Quality.PolicyMatch)
	assert.Equal(t, "spam.example.com", evaluation.Quality.Domain)
	assert.Contains(t, evaluation.Warnings, "source denied by source policy")
}

func TestEvaluate_TrustedDomainMarksHighTrust(t *testing.T) {
	t.Parallel()

	evaluation := Evaluate(Source{URL: "https://docs.example.com/api"}, Policy{
		TrustedDomains: []string{"example.com"},
	})

	require.True(t, evaluation.Allowed)
	assert.Equal(t, SourceTypeOfficialDocs, evaluation.Quality.SourceType)
	assert.Equal(t, TrustLevelHigh, evaluation.Quality.TrustLevel)
	assert.Equal(t, PolicyMatchTrustedDomain, evaluation.Quality.PolicyMatch)
	assert.InEpsilon(t, 0.95, evaluation.Quality.TrustScore, 0.001)
}

func TestEvaluate_WarnsOrExcludesLowTrustSources(t *testing.T) {
	t.Parallel()

	warn := Evaluate(Source{URL: "https://stackoverflow.com/questions/1"}, Policy{
		WarnOnLowTrustSources: Bool(true),
	})
	require.True(t, warn.Allowed)
	assert.Equal(t, TrustLevelLow, warn.Quality.TrustLevel)
	assert.NotEmpty(t, warn.Warnings)

	block := Evaluate(Source{URL: "https://stackoverflow.com/questions/1"}, Policy{
		AllowLowTrustSources:  Bool(false),
		WarnOnLowTrustSources: Bool(true),
	})
	assert.False(t, block.Allowed)
	assert.Equal(t, PolicyMatchLowTrustDisallowed, block.Quality.PolicyMatch)
}

func TestEvaluate_ClassifiesGitHubIssueBeforeRepository(t *testing.T) {
	t.Parallel()

	evaluation := Evaluate(Source{URL: "https://github.com/tommoulard/atteler/issues/234"}, Policy{})

	require.True(t, evaluation.Allowed)
	assert.Equal(t, SourceTypeIssueDiscussion, evaluation.Quality.SourceType)
	assert.Equal(t, TrustLevelMedium, evaluation.Quality.TrustLevel)
}

func TestMergeAndExtend_NormalizePolicyFields(t *testing.T) {
	t.Parallel()

	base := Policy{
		TrustedDomains:       []string{"https://Go.dev/doc/"},
		PreferSourceTypes:    []string{"standards"},
		AllowLowTrustSources: Bool(true),
	}
	overlay := Policy{
		TrustedDomains:                     []string{"docs.github.com"},
		RequireEvidenceForHighImpactClaims: Bool(true),
	}

	merged := Merge(base, overlay)
	assert.Equal(t, []string{"docs.github.com"}, merged.TrustedDomains)
	assert.Equal(t, []string{"standard_or_spec"}, merged.PreferSourceTypes)
	assert.True(t, *merged.AllowLowTrustSources)
	assert.True(t, *merged.RequireEvidenceForHighImpactClaims)

	extended := Extend(base, overlay)
	assert.Equal(t, []string{"docs.github.com", "go.dev"}, extended.TrustedDomains)
	assert.Equal(t, []string{"standard_or_spec"}, extended.PreferSourceTypes)
}

func TestRemoveDomains_NormalizesAndRemovesConflictingDomains(t *testing.T) {
	t.Parallel()

	assert.Equal(t, []string{"github.com"}, RemoveDomains(
		[]string{"https://go.dev/doc/", "github.com", "docs.example.com", "example.org"},
		[]string{"go.dev", "example.com", "docs.example.org"},
	))
	assert.Nil(t, RemoveDomains(nil, []string{"go.dev"}))
}

func TestPolicyFromGuidance_ExtractsSourceRestrictions(t *testing.T) {
	t.Parallel()

	policy := PolicyFromGuidance("AGENTS.md", `
Prefer official documentation, source repositories, standards, and go.dev.
Deny example-content-farm.com and do not use spam.example.
Require citations for security claims; warn on low-trust sources.
`)

	assert.Contains(t, policy.TrustedDomains, "go.dev")
	assert.Contains(t, policy.DeniedDomains, "example-content-farm.com")
	assert.Contains(t, policy.DeniedDomains, "spam.example")
	assert.Contains(t, policy.PreferSourceTypes, SourceTypeOfficialDocs)
	assert.Contains(t, policy.PreferSourceTypes, SourceTypeSourceCode)
	assert.Contains(t, policy.PreferSourceTypes, SourceTypeStandardOrSpec)
	require.NotNil(t, policy.RequireEvidenceForHighImpactClaims)
	assert.True(t, *policy.RequireEvidenceForHighImpactClaims)
	require.NotNil(t, policy.WarnOnLowTrustSources)
	assert.True(t, *policy.WarnOnLowTrustSources)
}

func TestPolicyFromGuidance_MixedDenyAndPreferenceLineKeepsDomainsSeparate(t *testing.T) {
	t.Parallel()

	policy := PolicyFromGuidance("CLAUDE.md", "Do not use spam.example and prefer go.dev official documentation.")

	assert.Contains(t, policy.DeniedDomains, "spam.example")
	assert.NotContains(t, policy.DeniedDomains, "go.dev")
	assert.Contains(t, policy.TrustedDomains, "go.dev")
	assert.NotContains(t, policy.TrustedDomains, "spam.example")
	assert.Contains(t, policy.PreferSourceTypes, SourceTypeOfficialDocs)

	commaPolicy := PolicyFromGuidance("AGENTS.md", "Do not use farm.example, prefer github.com and go.dev.")
	assert.Contains(t, commaPolicy.DeniedDomains, "farm.example")
	assert.NotContains(t, commaPolicy.DeniedDomains, "github.com")
	assert.Contains(t, commaPolicy.TrustedDomains, "github.com")
	assert.Contains(t, commaPolicy.TrustedDomains, "go.dev")
}

func TestPolicyFromGuidance_DomainOnlyGuidanceKeepsDefaultPreferredTypes(t *testing.T) {
	t.Parallel()

	policy := PolicyFromGuidance("AGENTS.md", "Prefer docs.github.com.")

	require.Nil(t, policy.PreferSourceTypes)
	assert.Contains(t, policy.TrustedDomains, "docs.github.com")

	effective := Effective(policy)
	assert.Contains(t, effective.PreferSourceTypes, SourceTypeOfficialDocs)
	assert.Contains(t, effective.PreferSourceTypes, SourceTypeSourceCode)
	assert.Contains(t, effective.PreferSourceTypes, SourceTypeStandardOrSpec)

	explicitEmpty := Effective(Policy{PreferSourceTypes: []string{}})
	assert.Empty(t, explicitEmpty.PreferSourceTypes)
}

func TestPolicyFromHarnessFiles_ReadsKnownHarnessGuidance(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	require.NoError(t, os.MkdirAll(filepath.Join(root, ".cursor", "rules"), 0o700))
	require.NoError(t, os.WriteFile(filepath.Join(root, "AGENTS.md"), []byte(`
Prefer official documentation from docs.github.com.
Deny example-content-farm.com and avoid spam.example.
Require citations for security claims.
`), 0o600))
	require.NoError(t, os.WriteFile(filepath.Join(root, ".cursor", "rules", "research.mdc"), []byte(`
Warn on low-trust sources and exclude low-trust sources.
Prefer standards and source repositories.
`), 0o600))

	policy, paths, err := PolicyFromHarnessFiles(root)
	require.NoError(t, err)

	assert.Equal(t, []string{".cursor/rules/research.mdc", "AGENTS.md"}, paths)
	assert.Contains(t, policy.TrustedDomains, "docs.github.com")
	assert.Contains(t, policy.DeniedDomains, "example-content-farm.com")
	assert.Contains(t, policy.DeniedDomains, "spam.example")
	assert.Contains(t, policy.PreferSourceTypes, SourceTypeOfficialDocs)
	assert.Contains(t, policy.PreferSourceTypes, SourceTypeSourceCode)
	assert.Contains(t, policy.PreferSourceTypes, SourceTypeStandardOrSpec)
	require.NotNil(t, policy.WarnOnLowTrustSources)
	assert.True(t, *policy.WarnOnLowTrustSources)
	require.NotNil(t, policy.AllowLowTrustSources)
	assert.False(t, *policy.AllowLowTrustSources)
	require.NotNil(t, policy.RequireEvidenceForHighImpactClaims)
	assert.True(t, *policy.RequireEvidenceForHighImpactClaims)
}

func TestPolicyFromHarnessFiles_SkipsIgnoredDirsAndUnreadableText(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	require.NoError(t, os.MkdirAll(filepath.Join(root, ".git"), 0o700))
	require.NoError(t, os.MkdirAll(filepath.Join(root, ".github"), 0o700))
	require.NoError(t, os.WriteFile(filepath.Join(root, ".git", "AGENTS.md"), []byte("Deny ignored.example."), 0o600))
	require.NoError(t, os.WriteFile(filepath.Join(root, ".github", "copilot-instructions.md"), []byte("Prefer github.com."), 0o600))
	require.NoError(t, os.WriteFile(filepath.Join(root, "CLAUDE.md"), []byte{0xff, 0xfe, 0xfd}, 0o600))

	policy, paths, err := PolicyFromHarnessFiles(root)
	require.NoError(t, err)

	assert.Equal(t, []string{".github/copilot-instructions.md"}, paths)
	assert.NotContains(t, policy.DeniedDomains, "ignored.example")
	assert.Contains(t, policy.TrustedDomains, "github.com")
}
