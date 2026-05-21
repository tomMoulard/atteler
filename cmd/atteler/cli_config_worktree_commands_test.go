package main

import (
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	appconfig "github.com/tommoulard/atteler/pkg/config"
	"github.com/tommoulard/atteler/pkg/contextref"
)

func TestContextOptionsFromConfig_MapsReferencePolicy(t *testing.T) {
	t.Parallel()

	opts := contextOptionsFromConfig(appconfig.Config{
		Context: appconfig.ContextConfig{
			ReferencePolicy: appconfig.ReferencePolicyConfig{
				AllowedSchemes:       []string{"https"},
				AllowedHosts:         []string{"docs.example.com"},
				LocalRoots:           []string{"../shared"},
				MaxRedirects:         2,
				ContentTypes:         []string{"text/*"},
				AllowPrivateNetworks: true,
			},
		},
	})

	assert.Equal(t, []string{"https"}, opts.ReferencePolicy.AllowedSchemes)
	assert.Equal(t, []string{"docs.example.com"}, opts.ReferencePolicy.AllowedHosts)
	assert.Equal(t, []string{"../shared"}, opts.ReferencePolicy.LocalRoots)
	assert.Equal(t, 2, opts.ReferencePolicy.MaxRedirects)
	assert.Equal(t, []string{"text/*"}, opts.ReferencePolicy.ContentTypes)
	assert.True(t, opts.ReferencePolicy.AllowPrivateNetworks)
}

func TestFormatReferenceEventIncludesPolicyReasonAndProvenance(t *testing.T) {
	t.Parallel()

	event := contextref.ReferenceEvent{
		Source:         "https://docs.example.com/style.md",
		Kind:           "url",
		Scope:          "agent:reviewer",
		Location:       "remote",
		Bytes:          42,
		Truncated:      true,
		DigestSHA256:   strings.Repeat("a", 64),
		FetchedAt:      time.Date(2026, 5, 21, 12, 0, 0, 0, time.UTC),
		PolicyDecision: contextref.ReferenceDecisionTruncated,
		PolicyReason:   "byte limit reached",
	}

	got := formatReferenceEvent(event)
	assert.Contains(t, got, "reference truncated")
	assert.Contains(t, got, "scope=agent:reviewer")
	assert.Contains(t, got, "kind=url")
	assert.Contains(t, got, "location=remote")
	assert.Contains(t, got, `source="https://docs.example.com/style.md"`)
	assert.Contains(t, got, "bytes=42")
	assert.Contains(t, got, "truncated=true")
	assert.Contains(t, got, "sha256="+strings.Repeat("a", 64))
	assert.Contains(t, got, "fetched_at=2026-05-21T12:00:00Z")
	assert.Contains(t, got, `reason="byte limit reached"`)
}

func TestFormatReferenceEventDecisionLabels(t *testing.T) {
	t.Parallel()

	for _, decision := range []string{
		contextref.ReferenceDecisionLoaded,
		contextref.ReferenceDecisionTruncated,
		contextref.ReferenceDecisionSkipped,
		contextref.ReferenceDecisionRejected,
	} {
		t.Run(decision, func(t *testing.T) {
			t.Parallel()

			got := formatReferenceEvent(contextref.ReferenceEvent{
				Source:         "ref.md",
				PolicyDecision: decision,
				PolicyReason:   "because",
			})

			assert.Contains(t, got, "reference "+decision)
			assert.Contains(t, got, `source="ref.md"`)
			assert.Contains(t, got, `reason="because"`)
		})
	}
}

func TestLoadConfiguredReferencesFailsClosedAndReportsEveryDecision(t *testing.T) { //nolint:paralleltest // captures process-global stderr.
	dir := t.TempDir()
	root := filepath.Join(dir, "project")
	require.NoError(t, os.MkdirAll(root, 0o750))
	require.NoError(t, os.WriteFile(filepath.Join(root, "good.md"), []byte("trusted docs\n"), 0o600))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "secret.md"), []byte("secret\n"), 0o600))

	stderr := captureStderr(t, func() {
		got := loadConfiguredReferences(t.Context(), []string{"good.md", "../secret.md"}, contextref.Options{Root: root})
		assert.Empty(t, got, "rejected configured references should omit the whole configured-reference block")
	})

	assert.Contains(t, stderr, "reference loaded")
	assert.Contains(t, stderr, `source="good.md"`)
	assert.Contains(t, stderr, "reference rejected")
	assert.Contains(t, stderr, `source="../secret.md"`)
	assert.Contains(t, stderr, "outside allowed local roots")
	assert.Contains(t, stderr, "omitting configured reference context")
}

func TestLoadConfiguredReferencesReportsTruncatedAndSkipped(t *testing.T) { //nolint:paralleltest // captures process-global stderr.
	dir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(dir, "big.md"), []byte("abcdef"), 0o600))

	stderr := captureStderr(t, func() {
		got := loadConfiguredReferences(t.Context(), []string{"big.md", ""}, contextref.Options{
			Root:          dir,
			MaxFileBytes:  3,
			MaxTotalBytes: 100,
		})
		assert.Contains(t, got, `source="big.md"`)
		assert.Contains(t, got, `truncated="true"`)
	})

	assert.Contains(t, stderr, "reference truncated")
	assert.Contains(t, stderr, `source="big.md"`)
	assert.Contains(t, stderr, "bytes=3")
	assert.Contains(t, stderr, "truncated=true")
	assert.Contains(t, stderr, "reference skipped")
	assert.Contains(t, stderr, `reason="empty reference"`)
}

func captureStderr(t *testing.T, fn func()) string {
	t.Helper()

	oldStderr := os.Stderr
	reader, writer, err := os.Pipe()
	require.NoError(t, err)

	os.Stderr = writer

	defer func() {
		os.Stderr = oldStderr
	}()

	fn()

	require.NoError(t, writer.Close())

	out, err := io.ReadAll(reader)
	require.NoError(t, err)
	require.NoError(t, reader.Close())

	os.Stderr = oldStderr

	return string(out)
}
