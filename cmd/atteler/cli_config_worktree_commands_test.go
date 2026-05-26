package main

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/tommoulard/atteler/pkg/agent"
	appconfig "github.com/tommoulard/atteler/pkg/config"
	"github.com/tommoulard/atteler/pkg/contextref"
	"github.com/tommoulard/atteler/pkg/llm"
	"github.com/tommoulard/atteler/pkg/session"
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

func TestDoctorPrintsPrivateAdapterContractAndModelProvenance(t *testing.T) { //nolint:paralleltest // captures process-global stdout.
	registry := llm.NewRegistry()
	registry.Register(doctorDiagnosticsProvider{})

	state := appState{
		config: appconfig.Config{
			Providers: map[string]appconfig.ProviderConfig{
				"claude-code": {Disabled: true},
			},
		},
		registry:      registry,
		agentRegistry: agent.NewRegistry(nil),
		sessionStore:  session.NewStore(t.TempDir()),
	}

	stdout := captureStdoutForStateDiagnostics(t, func() {
		require.NoError(t, doctor(t.Context(), state))
	})

	assert.Contains(t, stdout, "[ok] codex adapter=codex-test-contract-v1")
	assert.Contains(t, stdout, "adapter_contract: passed")
	assert.Contains(t, stdout, "contract: source=Codex CLI auth.json")
	assert.Contains(t, stdout, "source_cli_version=codex cli test fixture")
	assert.Contains(t, stdout, "kill_switches: providers.codex.disable_private_adapter")
	assert.Contains(t, stdout, "[ok] local_credentials")
	assert.Contains(t, stdout, "[warn] model_availability")
	assert.Contains(t, stdout, "warning: private adapter")
	assert.Contains(t, stdout, "- gpt-test (context=12345; provenance=static fixture; reviewed=2029-01-01; review_after=2030-01-01; notes=test note)")
}

func TestDoctorReportsUnregisteredPrivateAdapterCredentialFailures(t *testing.T) {
	tempDir := t.TempDir()
	t.Setenv("CODEX_HOME", filepath.Join(tempDir, "codex"))
	t.Setenv("HOME", tempDir)
	t.Setenv("ATTELER_CLAUDE_CODE_SKIP_KEYCHAIN", "1")

	state := appState{
		registry:      llm.NewRegistry(),
		agentRegistry: agent.NewRegistry(nil),
		sessionStore:  session.NewStore(t.TempDir()),
	}

	var doctorErr error

	stdout := captureStdoutForStateDiagnostics(t, func() {
		doctorErr = doctor(t.Context(), state)
	})

	require.Error(t, doctorErr)
	assert.Contains(t, doctorErr.Error(), "all providers failed")
	assert.Contains(t, stdout, "[FAIL] claude-code adapter=claude-code-oauth-messages-v1")
	assert.Contains(t, stdout, "[FAIL] codex adapter=codex-chatgpt-responses-v1")
	assert.Contains(t, stdout, "adapter_contract: failed: local_credentials")
	assert.Contains(t, stdout, "[fail] local_credentials")
	assert.Contains(t, stdout, "[skip] token_refresh")
	assert.Contains(t, stdout, "[skip] network_reachability")
	assert.Contains(t, stdout, "- gpt-5.5 (context=400000; provenance=static Codex adapter catalog")
	assert.Contains(t, stdout, "reviewed=2026-05-22; review_after=2026-08-22; notes=private Codex backend")
}

func TestDoctorHonorsPrivateAdapterKillSwitchesWithoutFailingNormalProvider(t *testing.T) {
	tempDir := t.TempDir()
	t.Setenv("CODEX_HOME", filepath.Join(tempDir, "codex"))
	t.Setenv("HOME", tempDir)
	t.Setenv("ATTELER_CLAUDE_CODE_SKIP_KEYCHAIN", "1")

	registry := llm.NewRegistry()
	registry.Register(doctorHealthyProvider{name: "openai", models: []string{"gpt-test"}})

	state := appState{
		config: appconfig.Config{
			Providers: map[string]appconfig.ProviderConfig{
				"codex":       {DisablePrivateAdapter: true},
				"claude-code": {DisablePrivateAdapter: true},
			},
		},
		registry:      registry,
		agentRegistry: agent.NewRegistry(nil),
		sessionStore:  session.NewStore(t.TempDir()),
	}

	stdout := captureStdoutForStateDiagnostics(t, func() {
		require.NoError(t, doctor(t.Context(), state))
	})

	assert.Contains(t, stdout, "[ok] openai")
	assert.NotContains(t, stdout, "[FAIL] codex")
	assert.NotContains(t, stdout, "[FAIL] claude-code")
}

type doctorDiagnosticsProvider struct{}

const doctorDiagnosticsProviderName = "codex"

func (doctorDiagnosticsProvider) Name() string { return doctorDiagnosticsProviderName }

func (doctorDiagnosticsProvider) Models() []string { return []string{"gpt-test"} }

func (doctorDiagnosticsProvider) FetchModels(context.Context) ([]string, error) {
	return []string{"gpt-test"}, nil
}

func (doctorDiagnosticsProvider) HealthCheck(context.Context) error { return nil }

func (doctorDiagnosticsProvider) Complete(context.Context, llm.CompleteParams) (*llm.Response, error) {
	return &llm.Response{Content: "ok", Model: "gpt-test"}, nil
}

func (doctorDiagnosticsProvider) ModelContextWindow(string) int { return 12345 }

func (doctorDiagnosticsProvider) AdapterDiagnostics() llm.AdapterDiagnostics {
	return llm.AdapterDiagnostics{
		Contract: llm.AdapterContract{
			Provider:         doctorDiagnosticsProviderName,
			AdapterVersion:   "codex-test-contract-v1",
			SourceCLI:        "Codex CLI auth.json",
			SourceCLIVersion: "codex cli test fixture",
			Protocol:         "test protocol",
			KillSwitches:     []string{"providers.codex.disable_private_adapter"},
			ReviewedAt:       "2029-01-01",
			ReviewAfter:      "2030-01-01",
		},
		Checks: []llm.ReadinessCheck{
			{Name: "local_credentials", Status: llm.ReadinessOK, Detail: "loaded"},
			{Name: "model_availability", Status: llm.ReadinessWarning, Detail: "static"},
		},
		Warnings: []string{"private adapter"},
		Models:   []string{"gpt-test"},
	}
}

func (doctorDiagnosticsProvider) ModelCatalog() []llm.ModelMetadata {
	return []llm.ModelMetadata{doctorModelMetadata()}
}

func (doctorDiagnosticsProvider) ModelMetadata(model string) (llm.ModelMetadata, bool) {
	if model != "gpt-test" {
		return llm.ModelMetadata{}, false
	}

	return doctorModelMetadata(), true
}

func doctorModelMetadata() llm.ModelMetadata {
	return llm.ModelMetadata{
		ID:            "gpt-test",
		ContextWindow: 12345,
		Provenance:    "static fixture",
		ReviewedAt:    "2029-01-01",
		ReviewAfter:   "2030-01-01",
		Notes:         "test note",
	}
}

type doctorHealthyProvider struct {
	name   string
	models []string
}

func (p doctorHealthyProvider) Name() string { return p.name }

func (p doctorHealthyProvider) Models() []string { return append([]string(nil), p.models...) }

func (p doctorHealthyProvider) FetchModels(context.Context) ([]string, error) {
	return append([]string(nil), p.models...), nil
}

func (p doctorHealthyProvider) HealthCheck(context.Context) error { return nil }

func (p doctorHealthyProvider) Complete(context.Context, llm.CompleteParams) (*llm.Response, error) {
	return &llm.Response{Content: "ok", Model: p.models[0]}, nil
}

func (p doctorHealthyProvider) ModelContextWindow(string) int { return 12345 }

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
