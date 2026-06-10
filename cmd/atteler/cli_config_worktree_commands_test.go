package main

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/url"
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

func TestDoctorOfflineFailsClosedForInvalidEnvConfig(t *testing.T) {
	tempDir := t.TempDir()
	setDoctorOfflineTestEnv(t, tempDir)

	configPath := filepath.Join(tempDir, "bad-env-config.yaml")
	require.NoError(t, os.WriteFile(configPath, []byte(`
not_a_valid_atteler_key: true
agent_loop:
  nope: 1
`), 0o600))
	t.Setenv(appconfig.EnvPath, configPath)

	var (
		err    error
		stdout string
	)

	stderr := captureStderr(t, func() {
		stdout = captureStdoutForStateDiagnostics(t, func() {
			err = doctorOffline(cliOptions{sessionDir: filepath.Join(tempDir, "sessions")})
		})
	})

	require.Error(t, err)
	assert.Contains(t, err.Error(), "config doctor-offline:")
	assert.Contains(t, err.Error(), "fatal config error")
	assert.Contains(t, err.Error(), configPath)
	assert.Contains(t, stdout, "Atteler offline doctor")
	assert.Contains(t, stdout, "diagnostic_levels:")
	assert.Contains(t, stdout, "config_status: failed")
	assert.Contains(t, stdout, "config: no config files loaded successfully")
	assert.Contains(t, stdout, "doctor_status: failed")
	assert.NotContains(t, stdout, "\nfatal:\n")
	assert.NotContains(t, stdout, "config_error:")
	assert.Contains(t, stderr, "fatal:")
	assert.Contains(t, stderr, configPath)
}

func TestDoctorOfflineFailsClosedForInvalidProjectConfig(t *testing.T) { //nolint:paralleltest // mutates process env/cwd and captures stdout.
	tempDir := t.TempDir()
	projectDir := filepath.Join(tempDir, "project")
	configPath := filepath.Join(projectDir, ".atteler", "config.yaml")
	require.NoError(t, os.MkdirAll(filepath.Dir(configPath), 0o700))
	require.NoError(t, os.WriteFile(configPath, []byte(`agent_loop:
  nope: 1
`), 0o600))
	setDoctorOfflineTestEnv(t, projectDir)

	var (
		err    error
		stdout string
	)

	stderr := captureStderr(t, func() {
		stdout = captureStdoutForStateDiagnostics(t, func() {
			err = doctorOffline(cliOptions{sessionDir: filepath.Join(tempDir, "sessions")})
		})
	})

	require.Error(t, err)
	assert.Contains(t, err.Error(), configPath)
	assert.Contains(t, stdout, "config_status: failed")
	assert.Contains(t, stdout, "doctor_status: failed")
	assert.NotContains(t, stdout, "\nfatal:\n")
	assert.Contains(t, stderr, "fatal:")
	assert.Contains(t, stderr, configPath)
}

func TestDoctorOfflineFailsClosedForConfigSchemaDiagnostics(t *testing.T) {
	tempDir := t.TempDir()
	setDoctorOfflineTestEnv(t, tempDir)

	configPath := filepath.Join(tempDir, "invalid-schema.yaml")
	require.NoError(t, os.WriteFile(configPath, []byte(`model_aliases:
  fast: missing-provider-prefix
`), 0o600))
	t.Setenv(appconfig.EnvPath, configPath)

	var (
		err    error
		stdout string
	)

	stderr := captureStderr(t, func() {
		stdout = captureStdoutForStateDiagnostics(t, func() {
			err = doctorOffline(cliOptions{sessionDir: filepath.Join(tempDir, "sessions")})
		})
	})

	require.Error(t, err)
	assert.Contains(t, err.Error(), "config doctor-offline:")
	assert.Contains(t, err.Error(), "model_aliases.fast")
	assert.Contains(t, stdout, "config_status: failed")
	assert.Contains(t, stdout, "config: "+configPath)
	assert.Contains(t, stdout, "config_diagnostics: errors=1")
	assert.Contains(t, stdout, "doctor_status: failed")
	assert.Contains(t, stderr, "fatal:")
	assert.Contains(t, stderr, "model_aliases.fast")
}

func TestDoctorOfflineTreatsMissingOptionalConfigAsNonFatal(t *testing.T) { //nolint:paralleltest // mutates process env/cwd and captures stdout.
	tempDir := t.TempDir()
	setDoctorOfflineTestEnv(t, tempDir)

	var err error

	stdout := captureStdoutForStateDiagnostics(t, func() {
		err = doctorOffline(cliOptions{sessionDir: filepath.Join(tempDir, "sessions")})
	})

	require.NoError(t, err)
	assert.Contains(t, stdout, "config_status: ok")
	assert.Contains(t, stdout, "config: no config files loaded")
	assert.Contains(t, stdout, "config_diagnostics: errors=0")
	assert.Contains(t, stdout, "doctor_status: ok")
	assert.NotContains(t, stdout, "\nfatal:\n")
}

func TestDoctorOfflineReportsHarnessImporterWarningsAsNonFatal(t *testing.T) {
	tempDir := t.TempDir()
	codexHome := filepath.Join(tempDir, "codex")
	require.NoError(t, os.MkdirAll(codexHome, 0o700))
	codexConfig := filepath.Join(codexHome, "config.toml")
	require.NoError(t, os.WriteFile(codexConfig, []byte(`
model = "gpt-5.5"
trusted_project_roots = ["/repo"]
`), 0o600))
	setDoctorOfflineTestEnv(t, tempDir)
	t.Setenv("CODEX_HOME", codexHome)

	var err error

	stdout := captureStdoutForStateDiagnostics(t, func() {
		err = doctorOffline(cliOptions{sessionDir: filepath.Join(tempDir, "sessions")})
	})

	require.NoError(t, err)
	assert.Contains(t, stdout, "config_status: ok")
	assert.Contains(t, stdout, "Config importer warnings:")
	assert.Contains(t, stdout, "[warning] codex: "+codexConfig+" trusted_project_roots: ignored unsupported field")
	assert.Contains(t, stdout, "config_diagnostics: errors=0 warnings=1")
	assert.Contains(t, stdout, "doctor_status: ok")
	assert.NotContains(t, stdout, "\nfatal:\n")

	stdout = captureStdoutForStateDiagnostics(t, func() {
		err = doctorOffline(cliOptions{
			sessionDir:   filepath.Join(tempDir, "sessions"),
			outputFormat: outputFormatJSON,
		})
	})

	require.NoError(t, err)

	var report doctorOfflineJSONReport
	require.NoError(t, json.Unmarshal([]byte(stdout), &report))
	assert.Equal(t, "ok", report.Status)
	assert.Equal(t, 1, report.ConfigDiagnostics.Warnings)
	require.NotEmpty(t, report.Diagnostics)
	assert.Equal(t, "warning", report.Diagnostics[0].Severity)
	assert.Equal(t, "codex", report.Diagnostics[0].Importer)
}

func TestDoctorOfflineJSONReportsFatalConfigStatus(t *testing.T) {
	tempDir := t.TempDir()
	setDoctorOfflineTestEnv(t, tempDir)

	configPath := filepath.Join(tempDir, "bad-env-config.yaml")
	require.NoError(t, os.WriteFile(configPath, []byte(`agent_loop:
  nope: 1
`), 0o600))
	t.Setenv(appconfig.EnvPath, configPath)

	var err error

	stdout := captureStdoutForStateDiagnostics(t, func() {
		err = doctorOffline(cliOptions{
			sessionDir: filepath.Join(tempDir, "sessions"),
			jsonOutput: true,
		})
	})

	require.Error(t, err)

	var report struct {
		Status string `json:"status"`
		Config struct {
			Status    string   `json:"status"`
			LoadError string   `json:"load_error"`
			Loaded    []string `json:"loaded"`
		} `json:"config"`
		Diagnostics []struct {
			Severity string `json:"severity"`
			Path     string `json:"path"`
			Message  string `json:"message"`
		} `json:"diagnostics"`
	}
	require.NoError(t, json.Unmarshal([]byte(stdout), &report))
	assert.Equal(t, "failed", report.Status)
	assert.Equal(t, "failed", report.Config.Status)
	assert.Contains(t, report.Config.LoadError, configPath)
	require.NotEmpty(t, report.Diagnostics)
	assert.Equal(t, "fatal", report.Diagnostics[0].Severity)
	assert.Equal(t, configPath, report.Diagnostics[0].Path)
	assert.Contains(t, report.Diagnostics[0].Message, configPath)
	assert.Empty(t, report.Config.Loaded)
}

func TestDoctorOfflineJSONReportsStrictParseFatalPath(t *testing.T) {
	tempDir := t.TempDir()
	setDoctorOfflineTestEnv(t, tempDir)

	configPath := filepath.Join(tempDir, "bad-type-config.yaml")
	require.NoError(t, os.WriteFile(configPath, []byte(`generation:
  max_tokens: not-a-number
`), 0o600))
	t.Setenv(appconfig.EnvPath, configPath)

	var err error

	stdout := captureStdoutForStateDiagnostics(t, func() {
		err = doctorOffline(cliOptions{
			sessionDir:   filepath.Join(tempDir, "sessions"),
			outputFormat: outputFormatJSON,
		})
	})

	require.Error(t, err)

	var report doctorOfflineJSONReport
	require.NoError(t, json.Unmarshal([]byte(stdout), &report))
	assert.Equal(t, "failed", report.Status)
	assert.Equal(t, "failed", report.Config.Status)
	assert.Contains(t, report.Config.LoadError, configPath)
	assert.Equal(t, 1, report.ConfigDiagnostics.Fatal)
	require.NotEmpty(t, report.Diagnostics)
	assert.Equal(t, "fatal", report.Diagnostics[0].Severity)
	assert.Equal(t, configPath, report.Diagnostics[0].Path)
	assert.Contains(t, report.Diagnostics[0].Message, "cannot unmarshal")
}

func TestDoctorOfflineJSONReportsOKForMissingOptionalConfig(t *testing.T) { //nolint:paralleltest // mutates process env/cwd and captures stdout.
	tempDir := t.TempDir()
	setDoctorOfflineTestEnv(t, tempDir)

	var err error

	stdout := captureStdoutForStateDiagnostics(t, func() {
		err = doctorOffline(cliOptions{
			sessionDir:   filepath.Join(tempDir, "sessions"),
			outputFormat: outputFormatJSON,
		})
	})

	require.NoError(t, err)
	assert.NotContains(t, stdout, "Atteler offline doctor")

	var report struct {
		Status string `json:"status"`
		Config struct {
			Status    string   `json:"status"`
			LoadError string   `json:"load_error"`
			Loaded    []string `json:"loaded"`
		} `json:"config"`
		Diagnostics []struct {
			Severity string `json:"severity"`
		} `json:"diagnostics"`
	}
	require.NoError(t, json.Unmarshal([]byte(stdout), &report))
	assert.Equal(t, "ok", report.Status)
	assert.Equal(t, "ok", report.Config.Status)
	assert.Empty(t, report.Config.LoadError)
	assert.Empty(t, report.Config.Loaded)
	assert.Empty(t, report.Diagnostics)
}

func TestDoctorOfflineJSONReportsFatalSchemaDiagnostics(t *testing.T) {
	tempDir := t.TempDir()
	setDoctorOfflineTestEnv(t, tempDir)

	configPath := filepath.Join(tempDir, "invalid-schema.yaml")
	require.NoError(t, os.WriteFile(configPath, []byte(`model_aliases:
  fast: missing-provider-prefix
`), 0o600))
	t.Setenv(appconfig.EnvPath, configPath)

	var err error

	stdout := captureStdoutForStateDiagnostics(t, func() {
		err = doctorOffline(cliOptions{
			sessionDir:   filepath.Join(tempDir, "sessions"),
			outputFormat: outputFormatJSON,
		})
	})

	require.Error(t, err)

	var report struct {
		Status string `json:"status"`
		Config struct {
			Status     string `json:"status"`
			LoadError  string `json:"load_error"`
			FatalError string `json:"fatal_error"`
		} `json:"config"`
		Diagnostics []struct {
			Severity string `json:"severity"`
			Path     string `json:"path"`
			Field    string `json:"field"`
			Message  string `json:"message"`
		} `json:"diagnostics"`
	}
	require.NoError(t, json.Unmarshal([]byte(stdout), &report))
	assert.Equal(t, "failed", report.Status)
	assert.Equal(t, "failed", report.Config.Status)
	assert.Empty(t, report.Config.LoadError)
	assert.Contains(t, report.Config.FatalError, "model_aliases.fast")
	require.NotEmpty(t, report.Diagnostics)
	assert.Equal(t, "fatal", report.Diagnostics[0].Severity)
	assert.Equal(t, configPath, report.Diagnostics[0].Path)
	assert.Equal(t, "model_aliases.fast", report.Diagnostics[0].Field)
	assert.Contains(t, report.Diagnostics[0].Message, "model alias target must be provider/model")
}

func TestValidateConfigFailsClosedForConfigSchemaDiagnostics(t *testing.T) {
	tempDir := t.TempDir()
	setDoctorOfflineTestEnv(t, tempDir)

	configPath := filepath.Join(tempDir, "invalid-schema.yaml")
	require.NoError(t, os.WriteFile(configPath, []byte(`model_aliases:
  fast: missing-provider-prefix
`), 0o600))
	t.Setenv(appconfig.EnvPath, configPath)

	err := validateConfig()
	require.Error(t, err)
	assert.Contains(t, err.Error(), "validate config:")
	assert.Contains(t, err.Error(), "model_aliases.fast")
	assert.Contains(t, err.Error(), "model alias target must be provider/model")
}

func TestDoctorFailsClosedForInvalidConfig(t *testing.T) {
	tempDir := t.TempDir()
	setDoctorOfflineTestEnv(t, tempDir)

	configPath := filepath.Join(tempDir, "bad-env-config.yaml")
	require.NoError(t, os.WriteFile(configPath, []byte(`agent_loop:
  nope: 1
`), 0o600))
	t.Setenv(appconfig.EnvPath, configPath)

	var (
		state appState
		err   error
	)

	loadStderr := captureStderr(t, func() {
		state, err = loadAppState(t.Context(), cliOptions{
			doctor:     true,
			sessionDir: filepath.Join(tempDir, "sessions"),
		})
	})
	require.NoError(t, err)
	assert.NotContains(t, loadStderr, "warning:")

	var (
		doctorErr error
		stdout    string
	)

	doctorStderr := captureStderr(t, func() {
		stdout = captureStdoutForStateDiagnostics(t, func() {
			doctorErr = doctor(t.Context(), state)
		})
	})

	require.Error(t, doctorErr)
	assert.Contains(t, doctorErr.Error(), "config doctor:")
	assert.Contains(t, doctorErr.Error(), "fatal config error")
	assert.Contains(t, doctorErr.Error(), configPath)
	assert.Contains(t, stdout, "Atteler doctor")
	assert.Contains(t, stdout, "config_status: failed")
	assert.NotContains(t, stdout, "\nfatal:\n")
	assert.NotContains(t, stdout, "[ok]")
	assert.Contains(t, doctorStderr, "fatal:")
	assert.Contains(t, doctorStderr, configPath)
}

func TestDoctorFailsClosedForConfigSchemaDiagnostics(t *testing.T) {
	tempDir := t.TempDir()
	setDoctorOfflineTestEnv(t, tempDir)

	configPath := filepath.Join(tempDir, "invalid-schema.yaml")
	require.NoError(t, os.WriteFile(configPath, []byte(`model_aliases:
  fast: missing-provider-prefix
`), 0o600))
	t.Setenv(appconfig.EnvPath, configPath)

	state, err := loadAppState(t.Context(), cliOptions{
		doctor:     true,
		sessionDir: filepath.Join(tempDir, "sessions"),
	})
	require.NoError(t, err)

	var (
		doctorErr error
		stdout    string
	)

	doctorStderr := captureStderr(t, func() {
		stdout = captureStdoutForStateDiagnostics(t, func() {
			doctorErr = doctor(t.Context(), state)
		})
	})

	require.Error(t, doctorErr)
	assert.Contains(t, doctorErr.Error(), "model_aliases.fast")
	assert.Contains(t, stdout, "config_status: failed")
	assert.Contains(t, stdout, "doctor_status: failed")
	assert.NotContains(t, stdout, "[ok]")
	assert.Contains(t, doctorStderr, "fatal:")
	assert.Contains(t, doctorStderr, "model_aliases.fast")
}

func setDoctorOfflineTestEnv(t *testing.T, cwd string) {
	t.Helper()

	tempDir := t.TempDir()
	t.Setenv("HOME", tempDir)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(tempDir, "config"))
	t.Setenv("CODEX_HOME", filepath.Join(tempDir, "missing-codex"))
	t.Setenv("OPENCODE_CONFIG", "")
	t.Setenv("OPENCODE_CONFIG_DIR", "")
	t.Setenv("FORGE_CONFIG", filepath.Join(tempDir, "missing-forge"))
	t.Setenv(appconfig.EnvPath, "")
	t.Chdir(cwd)
}

func TestContextOptionsFromConfig_MapsReferencePolicy(t *testing.T) {
	t.Parallel()

	opts := contextOptionsFromConfig(appconfig.Config{
		Context: appconfig.ContextConfig{
			ReferencePolicy: appconfig.ReferencePolicyConfig{
				AllowedSchemes:       []string{"https"},
				DeniedSchemes:        []string{"http"},
				AllowedHosts:         []string{"docs.example.com"},
				DeniedHosts:          []string{"bad.example.com"},
				AllowedPorts:         []int{443},
				DeniedPorts:          []int{81},
				LocalRoots:           []string{"../shared"},
				DeniedLocalRoots:     []string{"../shared/secrets"},
				AllowedGlobs:         []string{"docs/**/*.md"},
				DeniedGlobs:          []string{"**/*.pem"},
				MaxRedirects:         2,
				MaxFiles:             17,
				ContentTypes:         []string{"text/*"},
				AllowAbsolutePaths:   true,
				AllowPrivateNetworks: true,
			},
		},
	})

	assert.Equal(t, []string{"https"}, opts.ReferencePolicy.AllowedSchemes)
	assert.Equal(t, []string{"http"}, opts.ReferencePolicy.DeniedSchemes)
	assert.Equal(t, []string{"docs.example.com"}, opts.ReferencePolicy.AllowedHosts)
	assert.Equal(t, []string{"bad.example.com"}, opts.ReferencePolicy.DeniedHosts)
	assert.Equal(t, []int{443}, opts.ReferencePolicy.AllowedPorts)
	assert.Equal(t, []int{81}, opts.ReferencePolicy.DeniedPorts)
	assert.Equal(t, []string{"../shared"}, opts.ReferencePolicy.LocalRoots)
	assert.Equal(t, []string{"../shared/secrets"}, opts.ReferencePolicy.DeniedLocalRoots)
	assert.Equal(t, []string{"docs/**/*.md"}, opts.ReferencePolicy.AllowedGlobs)
	assert.Equal(t, []string{"**/*.pem"}, opts.ReferencePolicy.DeniedGlobs)
	assert.Equal(t, 2, opts.ReferencePolicy.MaxRedirects)
	assert.Equal(t, 17, opts.ReferencePolicy.MaxFiles)
	assert.Equal(t, []string{"text/*"}, opts.ReferencePolicy.ContentTypes)
	assert.True(t, opts.ReferencePolicy.AllowAbsolutePaths)
	assert.True(t, opts.ReferencePolicy.AllowPrivateNetworks)
}

func TestContextOptionsForProviderModelUsesProviderCalibration(t *testing.T) {
	t.Parallel()

	opts := contextOptionsForProviderModel(contextref.Options{}, "anthropic", "custom-model")
	require.NotNil(t, opts.TokenEstimator)

	profile := opts.TokenEstimator.Profile()
	assert.Equal(t, "anthropic", profile.Provider)
	assert.Equal(t, "custom-model", profile.Model)
	assert.Contains(t, profile.Name, "anthropic")
	assert.Positive(t, profile.ErrorBoundPercent)
}

func TestContextOptionsForRequestModelsUsesFirstFallbackWhenPrimaryEmpty(t *testing.T) {
	t.Parallel()

	registry := llm.NewRegistry()
	registry.Register(contextManifestBudgetProvider{name: "fallback", models: []string{"tiny"}, window: 10_000})

	opts := contextOptionsForRequestModels(contextref.Options{}, registry, "", []string{"fallback/tiny"})
	require.NotNil(t, opts.TokenEstimator)

	profile := opts.TokenEstimator.Profile()
	assert.Equal(t, "fallback", profile.Provider)
	assert.Equal(t, "tiny", profile.Model)
}

func TestContextOptionsFromConfig_PreservesExplicitEmptyDefaultLists(t *testing.T) {
	t.Parallel()

	opts := contextOptionsFromConfig(appconfig.Config{
		Context: appconfig.ContextConfig{
			ReferencePolicy: appconfig.ReferencePolicyConfig{
				AllowedSchemes: []string{},
				ContentTypes:   []string{},
			},
		},
	})

	assert.NotNil(t, opts.ReferencePolicy.AllowedSchemes)
	assert.Empty(t, opts.ReferencePolicy.AllowedSchemes)
	assert.NotNil(t, opts.ReferencePolicy.ContentTypes)
	assert.Empty(t, opts.ReferencePolicy.ContentTypes)
}

func TestFormatReferenceEventIncludesPolicyReasonAndProvenance(t *testing.T) {
	t.Parallel()

	event := contextref.ReferenceEvent{
		Source:         "https://docs.example.com/style.md",
		ResolvedSource: "https://cdn.docs.example.com/style.md",
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
	assert.Contains(t, got, `resolved_source="https://cdn.docs.example.com/style.md"`)
	assert.Contains(t, got, "bytes=42")
	assert.Equal(t, 1, strings.Count(got, "bytes=42"))
	assert.Contains(t, got, "truncated=true")
	assert.Contains(t, got, "sha256="+strings.Repeat("a", 64))
	assert.Contains(t, got, "fetched_at=2026-05-21T12:00:00Z")
	assert.Contains(t, got, `reason="byte limit reached"`)
}

func TestFormatReferenceEventRedactsCredentialBearingURLFields(t *testing.T) {
	t.Parallel()

	parsed := url.URL{
		Scheme: "https",
		Host:   "docs.example.com",
		Path:   "/style.md",
	}
	parsed.User = url.UserPassword("token-user", "password-secret")
	query := parsed.Query()
	query.Set("access_token", "query-secret")
	query.Set("topic", "context")
	parsed.RawQuery = query.Encode()

	got := formatReferenceEvent(contextref.ReferenceEvent{
		Source:         parsed.String(),
		ResolvedSource: parsed.String(),
		Kind:           "url",
		PolicyDecision: contextref.ReferenceDecisionRejected,
		PolicyReason:   "fetch failed for " + parsed.String(),
	})

	assert.NotContains(t, got, "token-user")
	assert.NotContains(t, got, "password-secret")
	assert.NotContains(t, got, "query-secret")
	assert.Contains(t, got, "REDACTED@docs.example.com")
	assert.Contains(t, got, "access_token=REDACTED")
	assert.Contains(t, got, `source="https://REDACTED@docs.example.com/style.md?access_token=REDACTED&topic=context"`)
	assert.Contains(t, got, `resolved_source="https://REDACTED@docs.example.com/style.md?access_token=REDACTED&topic=context"`)
}

func TestFormatReferenceManifestRedactsCredentialBearingURLFields(t *testing.T) {
	t.Parallel()

	parsed := url.URL{
		Scheme: "https",
		Host:   "docs.example.com",
		Path:   "/style.md",
	}
	parsed.User = url.UserPassword("token-user", "password-secret")
	query := parsed.Query()
	query.Set("access_token", "query-secret")
	query.Set("topic", "context")
	parsed.RawQuery = query.Encode()

	got := formatReferenceManifest(contextref.ReferenceManifest{
		TokenEstimator: "test-estimator",
		Entries: []contextref.ReferenceEvent{
			{
				Source:         parsed.String(),
				ResolvedSource: parsed.String(),
				Kind:           "url",
				PolicyDecision: contextref.ReferenceDecisionRejected,
				PolicyReason:   "fetch failed for " + parsed.String(),
			},
		},
	})

	assert.NotContains(t, got, "token-user")
	assert.NotContains(t, got, "password-secret")
	assert.NotContains(t, got, "query-secret")
	assert.Contains(t, got, `"schema_version":1`)
	assert.Contains(t, got, "REDACTED@docs.example.com")
	assert.Contains(t, got, "access_token=REDACTED")
	assert.Contains(t, got, "test-estimator")
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
	assert.Contains(t, stderr, "reference omitted")
	assert.Contains(t, stderr, "configured reference block omitted because loading failed")
	assert.Contains(t, stderr, "reference rejected")
	assert.Contains(t, stderr, `source="../secret.md"`)
	assert.Contains(t, stderr, "outside allowed local roots")
	assert.Contains(t, stderr, "reference manifest")
	assert.Contains(t, stderr, `"included_count":0`)
	assert.Contains(t, stderr, `"omitted_count":1`)
	assert.Contains(t, stderr, `"rejected_count":1`)
	assert.Contains(t, stderr, "omitting configured reference context")
}

func TestLoadConfiguredReferencesFailsClosedWhenManifestHasRejectedEntries(t *testing.T) { //nolint:paralleltest // captures process-global stderr.
	root := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(root, "good.md"), []byte("trusted docs\n"), 0o600))
	require.NoError(t, os.WriteFile(filepath.Join(root, "secret.pem"), []byte("private key\n"), 0o600))

	stderr := captureStderr(t, func() {
		got := loadConfiguredReferences(t.Context(), []string{"."}, contextref.Options{
			Root: root,
			ReferencePolicy: contextref.ReferencePolicy{
				DeniedGlobs: []string{"**/*.pem"},
			},
		})
		assert.Empty(t, got, "manifest-level rejections should omit the whole configured-reference block")
	})

	assert.Contains(t, stderr, "reference loaded")
	assert.Contains(t, stderr, `source="good.md"`)
	assert.Contains(t, stderr, "reference rejected")
	assert.Contains(t, stderr, `source="secret.pem"`)
	assert.Contains(t, stderr, "matches denied_globs")
	assert.Contains(t, stderr, "rejected 1 reference(s); omitting configured reference context")
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
	assert.Contains(t, stderr, "tokens=")
	assert.Contains(t, stderr, "token_upper=")
	assert.Contains(t, stderr, "truncated=true")
	assert.Contains(t, stderr, "reference skipped")
	assert.Contains(t, stderr, "reference manifest")
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
	assert.Contains(t, stdout, "- gpt-5.5 (context=1050000; provenance=built-in provider/model catalog")
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

func TestDoctorIncludesProviderRuntimeMetadata(t *testing.T) { //nolint:paralleltest // captures process-global stdout.
	registry := llm.NewRegistry()
	registry.Register(doctorHealthyProvider{name: "openai", models: []string{"gpt-4.1"}})

	state := appState{
		registry:      registry,
		agentRegistry: agent.NewRegistry(nil),
		sessionStore:  session.NewStore(t.TempDir()),
	}

	stdout := captureStdoutForStateDiagnostics(t, func() {
		require.NoError(t, doctor(t.Context(), state))
	})

	assert.Contains(t, stdout, "  [ok] openai")
	assert.Contains(t, stdout, "         - gpt-4.1")
	assert.Contains(t, stdout, "         runtime: Direct HTTPS calls from atteler to the OpenAI Chat Completions API.")
	assert.Contains(t, stdout, "         health: Network check: calls `GET /v1/models` through `FetchModels`.")
	assert.Contains(t, stdout, "compatibility_matrix:")
	assert.Contains(t, stdout, "  - openai: auth_source=api-key")
	assert.Contains(t, stdout, "temperature_top_p=supported")
	assert.Contains(t, stdout, "  - codex: auth_source=borrowed-chatgpt")
}

func TestDoctorOfflineIncludesProviderCompatibilityMatrix(t *testing.T) {
	tempDir := t.TempDir()
	t.Setenv("HOME", tempDir)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(tempDir, "config"))
	t.Setenv(appconfig.EnvPath, "")

	stdout := captureStdoutForStateDiagnostics(t, func() {
		require.NoError(t, doctorOffline(cliOptions{sessionDir: filepath.Join(tempDir, "sessions")}))
	})

	assert.Contains(t, stdout, "Atteler offline doctor")
	assert.Contains(t, stdout, "compatibility_matrix:")
	assert.Contains(t, stdout, "  - anthropic: auth_source=api-key/oauth")
	assert.Contains(t, stdout, "model_discovery=live+static")
	assert.Contains(t, stdout, "offline_mode=metadata-only")
	assert.Contains(t, stdout, "  - ollama: auth_source=none")
}

func TestDoctorAndOfflineDoctorReportSameProviderCompatibilityMatrix(t *testing.T) {
	tempDir := t.TempDir()
	t.Setenv("HOME", tempDir)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(tempDir, "config"))
	t.Setenv(appconfig.EnvPath, "")

	registry := llm.NewRegistry()
	registry.Register(doctorHealthyProvider{name: "openai", models: []string{"gpt-4.1"}})

	state := appState{
		config: appconfig.Config{
			Providers: map[string]appconfig.ProviderConfig{
				"codex":       {DisablePrivateAdapter: true},
				"claude-code": {DisablePrivateAdapter: true},
			},
		},
		registry:      registry,
		agentRegistry: agent.NewRegistry(nil),
		sessionStore:  session.NewStore(filepath.Join(tempDir, "sessions")),
	}

	doctorOut := captureStdoutForStateDiagnostics(t, func() {
		require.NoError(t, doctor(t.Context(), state))
	})
	offlineOut := captureStdoutForStateDiagnostics(t, func() {
		require.NoError(t, doctorOffline(cliOptions{sessionDir: filepath.Join(tempDir, "sessions")}))
	})

	doctorBlock := providerCompatibilityMatrixBlock(t, doctorOut)
	offlineBlock := providerCompatibilityMatrixBlock(t, offlineOut)
	assert.Equal(t, offlineBlock, doctorBlock)
	assert.Equal(t, len(llm.KnownProviders()), strings.Count(doctorBlock, "\n  - "))
}

func providerCompatibilityMatrixBlock(t *testing.T, output string) string {
	t.Helper()

	lines := strings.Split(output, "\n")
	for i, line := range lines {
		if line != "compatibility_matrix:" {
			continue
		}

		block := []string{line}

		for _, next := range lines[i+1:] {
			if !strings.HasPrefix(next, "  - ") {
				break
			}

			block = append(block, next)
		}

		return strings.Join(block, "\n") + "\n"
	}

	require.FailNow(t, "missing compatibility_matrix block", output)

	return ""
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

func TestLoadConfiguredReferenceContextRecordsEstimatorForEmptyReferences(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	opts := contextOptionsForProviderModel(contextref.Options{Root: dir}, "anthropic", "claude-test")

	refCtx := loadConfiguredReferenceContext(t.Context(), nil, opts)

	assert.Empty(t, refCtx.Content)
	assert.Empty(t, refCtx.Manifest.Entries)
	assert.Equal(t, 1, refCtx.Manifest.SchemaVersion)
	assert.Contains(t, refCtx.Estimator, "anthropic-calibrated")
	assert.Contains(t, refCtx.Manifest.TokenEstimator, "anthropic-calibrated")
}

func TestLoadConfiguredReferenceContextRecordsEstimatorForRejectedOnlyManifest(t *testing.T) { //nolint:paralleltest // captures process-global stderr.
	dir := t.TempDir()
	opts := contextOptionsForProviderModel(contextref.Options{Root: dir}, "anthropic", "claude-test")

	var refCtx configuredReferenceContext

	stderr := captureStderr(t, func() {
		refCtx = loadConfiguredReferenceContext(t.Context(), []string{"../secret.md"}, opts)
	})

	assert.Empty(t, refCtx.Content)
	assert.Contains(t, refCtx.Estimator, "anthropic-calibrated")
	assert.Contains(t, refCtx.Manifest.TokenEstimator, "anthropic-calibrated")
	assert.Equal(t, 1, refCtx.Manifest.RejectedCount)
	assert.Contains(t, stderr, `"token_estimator":"anthropic-calibrated`)
}

func TestBuildReferenceContextWithManifestUsesRequestEstimatorForAgentReferences(t *testing.T) { //nolint:paralleltest // captures process-global stderr.
	dir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(dir, "rubric.md"), []byte("review carefully"), 0o600))

	opts := contextOptionsForProviderModel(contextref.Options{Root: dir}, "anthropic", "claude-test")
	activeAgent := agentSelection{
		name: "reviewer",
		ok:   true,
		agent: agent.Agent{
			Name:       "reviewer",
			References: []string{"rubric.md"},
		},
	}

	var refCtx configuredReferenceContext

	captureStderr(t, func() {
		refCtx = buildReferenceContextWithManifest(t.Context(), configuredReferenceContext{}, activeAgent, opts)
	})

	require.NotEmpty(t, refCtx.Content)
	require.Len(t, refCtx.Manifest.Entries, 1)
	assert.Contains(t, refCtx.Content, "anthropic-calibrated")
	assert.Contains(t, refCtx.Manifest.Entries[0].TokenEstimator, "anthropic-calibrated")
}

func TestBuildReferenceContextWithManifestRetainsGlobalAuditWhenOnlyAgentHasContent(t *testing.T) { //nolint:paralleltest // captures process-global stderr.
	dir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(dir, "rubric.md"), []byte("review carefully"), 0o600))

	opts := contextOptionsForProviderModel(contextref.Options{Root: dir}, "openai", "gpt-test")
	globalManifest := contextref.BuildReferenceManifest([]contextref.ReferenceEvent{
		{
			Source:         "../secret.md",
			Kind:           "file",
			Scope:          contextref.ReferenceScopeGlobal,
			Location:       "local",
			PolicyDecision: contextref.ReferenceDecisionRejected,
			PolicyReason:   "outside allowed local roots",
			TokenEstimator: "openai-calibrated",
		},
	})
	globalRefCtx := configuredReferenceContext{Manifest: globalManifest, Estimator: "openai-calibrated"}
	activeAgent := agentSelection{
		name: "reviewer",
		ok:   true,
		agent: agent.Agent{
			Name:       "reviewer",
			References: []string{"rubric.md"},
		},
	}

	var refCtx configuredReferenceContext

	captureStderr(t, func() {
		refCtx = buildReferenceContextWithManifest(t.Context(), globalRefCtx, activeAgent, opts)
	})

	require.NotEmpty(t, refCtx.Content)
	assert.Contains(t, refCtx.Content, `source="rubric.md"`)
	assert.Equal(t, 1, refCtx.Manifest.RejectedCount)
	assert.Equal(t, 1, refCtx.Manifest.IncludedCount)
	require.Len(t, refCtx.Manifest.Entries, 2)
	assert.Equal(t, "../secret.md", refCtx.Manifest.Entries[0].Source)
	assert.Equal(t, "rubric.md", refCtx.Manifest.Entries[1].Source)
}

func TestConfiguredReferenceContextForRequestReloadsWhenEstimatorChanges(t *testing.T) { //nolint:paralleltest // captures process-global stderr.
	dir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(dir, "style.md"), []byte("provider aware"), 0o600))

	refs := []string{"style.md"}
	openAIOpts := contextOptionsForProviderModel(contextref.Options{Root: dir}, "openai", "gpt-test")
	anthropicOpts := contextOptionsForProviderModel(contextref.Options{Root: dir}, "anthropic", "claude-test")

	var current configuredReferenceContext

	captureStderr(t, func() {
		current = loadConfiguredReferenceContext(t.Context(), refs, openAIOpts)
	})

	require.Contains(t, current.Estimator, "openai-calibrated")
	require.Contains(t, current.Manifest.Entries[0].TokenEstimator, "openai-calibrated")

	var reloaded configuredReferenceContext

	captureStderr(t, func() {
		reloaded = configuredReferenceContextForRequest(t.Context(), refs, current, anthropicOpts)
	})

	require.Contains(t, reloaded.Estimator, "anthropic-calibrated")
	require.Contains(t, reloaded.Content, "anthropic-calibrated")
	require.Contains(t, reloaded.Manifest.Entries[0].TokenEstimator, "anthropic-calibrated")

	same := configuredReferenceContextForRequest(t.Context(), refs, reloaded, anthropicOpts)
	assert.Equal(t, reloaded.Estimator, same.Estimator)
	assert.Equal(t, reloaded.Content, same.Content)
}

func TestLLMConfigUsesResolvedFallbackModels(t *testing.T) {
	t.Parallel()

	cfg := appconfig.Config{
		FallbackModels: []string{"config-fallback"},
		ModelAliases:   map[string]string{"fast": "openai/gpt-4.1-mini"},
	}

	got := llmConfig(cfg, "selected-model", []string{"agent-fallback"}, "session-123", []string{"atteler", "--model", "selected-model"})

	assert.Equal(t, "selected-model", got.SelectedModel)
	assert.Equal(t, map[string]string{"fast": "openai/gpt-4.1-mini"}, got.ModelAliases)
	assert.Equal(t, []string{"agent-fallback"}, got.FallbackModels)
	assert.Equal(t, "session-123", got.SessionID)
	assert.Equal(t, []string{"atteler", "--model", "selected-model"}, got.CommandLine)
}

func TestProviderRegistrationSelectedModelUsesExplainTarget(t *testing.T) {
	t.Parallel()

	got := providerRegistrationSelectedModel(
		cliOptions{explainModelResolution: "gpt-live-only"},
		"persisted-model",
	)

	assert.Equal(t, "gpt-live-only", got)
}

func TestWorktreeMergePolicyFromConfigOptions_PreservesByDefault(t *testing.T) {
	t.Parallel()

	got := worktreeMergePolicyFromConfigOptions(appconfig.Config{}, cliOptions{})

	assert.False(t, got.AutoMerge)
	assert.False(t, got.OverrideVerification)
	assert.Empty(t, got.VerificationCommands)
}

func TestWorktreeMergePolicyFromConfigOptions_RequiresExplicitAutoMerge(t *testing.T) {
	t.Parallel()

	configAutoMerge := true
	got := worktreeMergePolicyFromConfigOptions(appconfig.Config{
		Worktree: appconfig.WorktreeConfig{
			AutoMerge:            &configAutoMerge,
			VerificationCommands: []string{" go test ./... "},
		},
	}, cliOptions{
		worktreeVerificationCommands: rawStringListFlag{" make test "},
	})

	assert.True(t, got.AutoMerge)
	assert.Equal(t, []string{"go test ./...", "make test"}, got.VerificationCommands)
	assert.False(t, got.OverrideVerification)

	disabled := worktreeMergePolicyFromConfigOptions(appconfig.Config{
		Worktree: appconfig.WorktreeConfig{AutoMerge: &configAutoMerge},
	}, cliOptions{noAutoMerge: true})
	assert.False(t, disabled.AutoMerge)
}

func TestValidateWorktreeAutoMergePolicy_RejectsUngatedAutoMerge(t *testing.T) {
	t.Parallel()

	err := validateWorktreeAutoMergePolicy(cliWorktreeMergePolicy{AutoMerge: true})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "--worktree-verify-command")
	assert.Contains(t, err.Error(), "--worktree-merge-override")

	require.NoError(t, validateWorktreeAutoMergePolicy(cliWorktreeMergePolicy{}))
	require.NoError(t, validateWorktreeAutoMergePolicy(cliWorktreeMergePolicy{
		AutoMerge:            true,
		VerificationCommands: []string{"go test ./..."},
	}))
	require.NoError(t, validateWorktreeAutoMergePolicy(cliWorktreeMergePolicy{
		AutoMerge:            true,
		OverrideVerification: true,
	}))
}

func TestWorktreeManualMergePolicyFromOptions_UsesManualOverrideWhenNoCommands(t *testing.T) {
	t.Parallel()

	got := worktreeManualMergePolicyFromOptions(cliOptions{mergeWorktreeAllowBaseMismatch: true})

	assert.True(t, got.OverrideVerification)
	assert.True(t, got.AllowBaseMismatch)
	assert.Empty(t, got.VerificationCommands)

	withCommand := worktreeManualMergePolicyFromOptions(cliOptions{
		worktreeVerificationCommands: rawStringListFlag{" test -f reviewed.txt "},
	})
	assert.False(t, withCommand.OverrideVerification)
	assert.Equal(t, []string{"test -f reviewed.txt"}, withCommand.VerificationCommands)
}

func TestClearWorktreeMetadataFromLatestSessionPreservesSavedSessionChanges(t *testing.T) {
	t.Parallel()

	store := session.NewStore(t.TempDir())
	stale := session.New("gpt-test", nil)
	stale.WorktreePath = "/tmp/atteler-worktree"
	stale.WorktreeBranch = "atteler/session"
	stale.WorktreeBase = "main"
	require.NoError(t, store.Save(stale))

	latest := stale
	latest.DefaultAgent = "executor"
	latest.Append(llm.RoleUser, "keep this message")
	require.NoError(t, store.Save(latest))

	state := appState{
		sessionStore: store,
		sessionState: stale,
	}

	require.NoError(t, clearWorktreeMetadataFromLatestSession(&state))

	got, err := store.Load(stale.ID)
	require.NoError(t, err)
	assert.Empty(t, got.WorktreePath)
	assert.Empty(t, got.WorktreeBranch)
	assert.Empty(t, got.WorktreeBase)
	assert.Equal(t, "executor", got.DefaultAgent)
	require.Len(t, got.Messages, 1)
	assert.Equal(t, "keep this message", got.Messages[0].Content)
	assert.Equal(t, got, state.sessionState)
}

func TestDoctorForcesFreshProviderReadiness(t *testing.T) { //nolint:paralleltest // Captures process stdout.
	provider := &providerCommandTestProvider{
		healthErr: errors.New("cached failure"),
		name:      "alpha",
		models:    []string{"alpha-static"},
	}

	reg := llm.NewRegistry()
	reg.Register(provider)

	cached := reg.CheckHealthWithTTL(context.Background(), time.Hour)
	require.Len(t, cached, 1)
	require.False(t, cached[0].Healthy)
	require.Equal(t, 1, provider.healthCalls)

	provider.healthErr = nil

	app := appState{
		agentRegistry: agent.NewRegistry(nil),
		registry:      reg,
		sessionStore:  session.NewStore(t.TempDir()),
	}

	var err error

	out := captureStdoutForStateDiagnostics(t, func() {
		err = doctor(context.Background(), app)
	})

	require.NoError(t, err)
	assert.Contains(t, out, "[ok] alpha")
	assert.NotContains(t, out, "cached")
	assert.Equal(t, 2, provider.healthCalls)
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
