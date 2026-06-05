//nolint:wsl_v5 // Tests keep fixture setup and assertions together.
package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/tommoulard/atteler/pkg/githistory"
	"github.com/tommoulard/atteler/pkg/incident"
	attshell "github.com/tommoulard/atteler/pkg/shell"
)

const (
	testSentryIssuePath   = "/api/0/organizations/acme/issues/123456/"
	testSentryEventsPath  = "/api/0/organizations/acme/issues/123456/events/"
	testSentryShortIDPath = "/api/0/organizations/acme/shortids/API-912/"
	testSentryOpaqueToken = "opaque-sentry-token" // #nosec G101 -- synthetic test fixture, not a real credential.
)

func TestRunIncidentDiagnoseWithFileBuildsRedactedReport(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	require.NoError(t, os.MkdirAll(filepath.Join(root, "pkg", "auth"), 0o750))
	require.NoError(t, os.WriteFile(filepath.Join(root, "pkg", "auth", "session.go"), []byte("package auth\nfunc Refresh() {}\n"), 0o600))

	incidentPath := filepath.Join(root, "incident.json")
	require.NoError(t, os.WriteFile(incidentPath, []byte(`{
		"issue": {
			"id": "123",
			"shortId": "API-912",
			"title": "OAuth refresh panic",
			"project": {"slug": "api-gateway"},
			"metadata": {"type": "Panic", "value": "nil OAuth state for alice@example.com"},
			"tags": [{"key": "environment", "value": "production"}]
		},
		"event": {
			"entries": [{
				"type": "exception",
				"data": {"values": [{
					"type": "Panic",
					"value": "nil OAuth state access_token=secret-token",
					"stacktrace": {"frames": [{
						"filename": "github.com/acme/api/pkg/auth/session.go",
						"function": "Refresh",
						"lineno": 184,
						"in_app": true
					}]}
				}]}
			}]
		}
	}`), 0o600))

	reportPath := filepath.Join(root, "incident-report.md")
	var out bytes.Buffer
	err := runIncidentDiagnoseWithWriter(t.Context(), &out, root, incidentDiagnoseCommandInput{
		SentryIssue:  "API-912",
		FilePath:     incidentPath,
		ReportPath:   reportPath,
		OutputFormat: outputFormatText,
	})
	require.NoError(t, err)

	got := out.String()
	assert.Contains(t, got, "Incident: sentry API-912")
	assert.Contains(t, got, "Stack trace points to pkg/auth/session.go:184")
	assert.Contains(t, got, "Write the failing regression test before changing production code")
	assert.NotContains(t, got, "alice@example.com")
	assert.NotContains(t, got, "secret-token")

	report, err := os.ReadFile(reportPath)
	require.NoError(t, err)
	assert.Contains(t, string(report), "Production incident diagnosis")
	assert.NotContains(t, string(report), "secret-token")
}

func TestRunIncidentDiagnoseWithFileRendersMarkdownOutput(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(root, "main.go"), []byte("package main\nfunc main() {}\n"), 0o600))

	incidentPath := filepath.Join(root, "incident.json")
	require.NoError(t, os.WriteFile(incidentPath, []byte(`{
		"source": "file",
		"reference": "markdown-incident",
		"message": "nil OAuth state for alice@example.com access_token=secret-token",
		"stack_trace": [{"file": "main.go", "line": 1, "function": "main"}]
	}`), 0o600))

	var out bytes.Buffer
	err := runIncidentDiagnoseWithWriter(t.Context(), &out, root, incidentDiagnoseCommandInput{
		FilePath:     incidentPath,
		OutputFormat: commandOutputMarkdown,
	})
	require.NoError(t, err)

	got := out.String()
	assert.Contains(t, got, "# Production incident diagnosis")
	assert.Contains(t, got, "- **Incident:** file markdown-incident")
	assert.Contains(t, got, "Stack trace points to main.go:1")
	assert.Contains(t, got, "[REDACTED")
	assert.NotContains(t, got, "alice@example.com")
	assert.NotContains(t, got, "secret-token")
}

func TestRunIncidentDiagnoseCapturesReproAndValidationEvidence(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	require.NoError(t, os.MkdirAll(filepath.Join(root, "pkg", "auth"), 0o750))
	require.NoError(t, os.WriteFile(filepath.Join(root, "pkg", "auth", "session.go"), []byte("package auth\nfunc Refresh() {}\n"), 0o600))

	incidentPath := filepath.Join(root, "incident.json")
	require.NoError(t, os.WriteFile(incidentPath, []byte(`{
		"source": "file",
		"reference": "local-incident",
		"service": "api-gateway",
		"message": "nil OAuth state for alice@example.com access_token=secret-token",
		"stack_trace": [{"file": "pkg/auth/session.go", "line": 184, "function": "Refresh"}]
	}`), 0o600))

	var out bytes.Buffer
	err := runIncidentDiagnoseWithWriter(t.Context(), &out, root, incidentDiagnoseCommandInput{
		FilePath:           incidentPath,
		ReproCommand:       "printf '%s\\n' 'repro failed for bob@example.com access_token=secret-token'; exit 7",
		ValidationCommands: []string{"printf '%s\\n' 'validation passed for user_id=user-123'"},
		OutputFormat:       outputFormatJSON,
	})
	require.NoError(t, err)

	raw := out.String()
	assert.NotContains(t, raw, "alice@example.com")
	assert.NotContains(t, raw, "bob@example.com")
	assert.NotContains(t, raw, "secret-token")
	assert.NotContains(t, raw, "user-123")

	var analysis incident.Analysis
	require.NoError(t, json.Unmarshal(out.Bytes(), &analysis))
	assert.Equal(t, incidentCommandStatusFailed, analysis.Reproduction.Status)
	require.Len(t, analysis.Validation, 1)
	assert.Equal(t, incidentCommandStatusPassed, analysis.Validation[0].Status)
	assert.Contains(t, analysis.Validation[0].Stdout, "validation passed")
	assert.NotContains(t, analysis.FixPrompt, "secret-token")
}

func TestRunIncidentDiagnoseApplyFixRequiresStatefulLLMRegistry(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	incidentPath := filepath.Join(root, "incident.json")
	require.NoError(t, os.WriteFile(incidentPath, []byte(`{
		"source": "file",
		"reference": "local-incident",
		"message": "nil OAuth state",
		"stack_trace": [{"file": "pkg/auth/session.go", "line": 184, "function": "Refresh"}]
	}`), 0o600))

	var out bytes.Buffer
	err := runIncidentDiagnoseWithWriter(t.Context(), &out, root, incidentDiagnoseCommandInput{
		FilePath: incidentPath,
		ApplyFix: true,
	})

	require.Error(t, err)
	assert.Contains(t, err.Error(), "incident apply fix: LLM registry is required")
	assert.Empty(t, out.String())
}

func TestRunIncidentDiagnoseApplyFixRejectsStructuredOutput(t *testing.T) {
	t.Parallel()

	var out bytes.Buffer
	err := runIncidentDiagnoseWithWriter(t.Context(), &out, t.TempDir(), incidentDiagnoseCommandInput{
		ApplyFix:     true,
		OutputFormat: outputFormatJSON,
	})

	require.Error(t, err)
	assert.Contains(t, err.Error(), "supports --output text only")
	assert.Empty(t, out.String())
}

func TestRunIncidentDiagnoseOpenPRRequiresRepairGate(t *testing.T) {
	t.Parallel()

	var out bytes.Buffer
	err := runIncidentDiagnoseWithWriter(t.Context(), &out, t.TempDir(), incidentDiagnoseCommandInput{
		OpenPR: true,
	})

	require.Error(t, err)
	assert.Contains(t, err.Error(), "--incident-open-pr requires --incident-apply-fix")
	assert.Empty(t, out.String())
}

func TestRunIncidentDiagnoseOpenPRRequiresValidationEvidence(t *testing.T) {
	t.Parallel()

	var out bytes.Buffer
	err := runIncidentDiagnoseWithWriter(t.Context(), &out, t.TempDir(), incidentDiagnoseCommandInput{
		ApplyFix: true,
		OpenPR:   true,
	})

	require.Error(t, err)
	assert.Contains(t, err.Error(), "--incident-open-pr requires at least one --incident-validation-command")
	assert.Empty(t, out.String())
}

func TestIncidentFileErrorsRedactSensitivePaths(t *testing.T) {
	t.Parallel()

	_, err := fetchIncidentFromFile(incidentDiagnoseCommandInput{
		FilePath: filepath.Join(t.TempDir(), "missing-bob@example.com-access_token=secret-token.json"),
	})
	require.Error(t, err)

	assert.NotContains(t, err.Error(), "bob@example.com")
	assert.NotContains(t, err.Error(), "secret-token")
	assert.Contains(t, err.Error(), "[REDACTED")
}

func TestWriteIncidentFileErrorsRedactSensitivePaths(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	blockingFile := filepath.Join(root, "bob@example.com-access_token=secret-token")
	require.NoError(t, os.WriteFile(blockingFile, []byte("not a dir"), 0o600))

	err := writeIncidentFile(root, "bob@example.com-access_token=secret-token/report.md", "report")
	require.Error(t, err)

	assert.NotContains(t, err.Error(), "bob@example.com")
	assert.NotContains(t, err.Error(), "secret-token")
	assert.Contains(t, err.Error(), "[REDACTED")
}

func TestWriteIncidentFileRejectsPathsOutsideWorkspace(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	outside := filepath.Join(filepath.Dir(root), "bob@example.com-access_token=secret-token-report.md")

	err := writeIncidentFile(root, outside, "report")
	require.Error(t, err)

	assert.Contains(t, err.Error(), "outside workspace")
	assert.NotContains(t, err.Error(), "bob@example.com")
	assert.NotContains(t, err.Error(), "secret-token")
	assert.NoFileExists(t, outside)
}

func TestWriteIncidentFileRejectsSymlinkedArtifactPath(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	outside := t.TempDir()
	link := filepath.Join(root, "report-link")
	if err := os.Symlink(outside, link); err != nil {
		t.Skipf("symlink unavailable: %v", err)
	}

	err := writeIncidentFile(root, filepath.Join("report-link", "bob@example.com-access_token=secret-token.md"), "report")
	require.Error(t, err)

	assert.Contains(t, err.Error(), "must not traverse symlink")
	assert.NotContains(t, err.Error(), "bob@example.com")
	assert.NotContains(t, err.Error(), "secret-token")
	assert.NoFileExists(t, filepath.Join(outside, "bob@example.com-access_token=secret-token.md"))
}

func TestWriteIncidentDiagnosisOutputRedactsJSON(t *testing.T) {
	t.Parallel()

	var out bytes.Buffer
	err := writeIncidentDiagnosisOutput(&out, outputFormatJSON, incident.Analysis{
		Incident: incident.Context{
			Source:    incident.SourceFile,
			Reference: "local-incident",
			Message:   "failed for alice@example.com access_token=secret-token",
		},
		FixPrompt: "use user_id=user-123",
	})
	require.NoError(t, err)

	assert.NotContains(t, out.String(), "alice@example.com")
	assert.NotContains(t, out.String(), "secret-token")
	assert.NotContains(t, out.String(), "user-123")
	assert.Contains(t, out.String(), incident.RedactionPolicyVersion)
}

func TestIncidentDiagnosisOutputFormatHonorsJSONFlag(t *testing.T) {
	t.Parallel()

	got, err := incidentDiagnosisOutputFormat(incidentDiagnoseCommandInput{JSON: true})

	require.NoError(t, err)
	if got != outputFormatJSON {
		t.Fatalf("output format = %q, want %q", got, outputFormatJSON)
	}
}

func TestRunIncidentLocalCommandRedactsAuditOutput(t *testing.T) {
	root := t.TempDir()
	auditDir := filepath.Join(root, "audit")
	t.Setenv(attshell.EnvAuditDir, auditDir)

	result := runIncidentLocalCommand(t.Context(), root, "printf '%s\\n' 'bob@example.com access_token=secret-token user_id=user-123'", 5*time.Second, incident.Context{
		Source:    incident.SourceFile,
		Reference: "local-incident bob@example.com access_token=secret-token",
	})

	assert.Equal(t, incidentCommandStatusPassed, result.Status)
	assert.NotContains(t, result.Stdout, "bob@example.com")
	assert.NotContains(t, result.Stdout, "secret-token")

	outputs, err := filepath.Glob(filepath.Join(auditDir, "outputs", "*.json"))
	require.NoError(t, err)
	require.Len(t, outputs, 1)

	auditOutput, err := os.ReadFile(outputs[0])
	require.NoError(t, err)
	assert.NotContains(t, string(auditOutput), "bob@example.com")
	assert.NotContains(t, string(auditOutput), "secret-token")
	assert.Contains(t, string(auditOutput), "[REDACTED")

	ledger, err := os.ReadFile(filepath.Join(auditDir, "commands.jsonl"))
	require.NoError(t, err)
	assert.NotContains(t, string(ledger), "bob@example.com")
	assert.NotContains(t, string(ledger), "secret-token")
	assert.NotContains(t, string(ledger), "user-123")
}

func TestRunIncidentLocalCommandTruncatesPassedOutputWithoutFailing(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	result := runIncidentLocalCommand(t.Context(), root, "printf '%20000s' ''", 5*time.Second, incident.Context{
		Source:    incident.SourceFile,
		Reference: "local-incident",
	})

	assert.Equal(t, incidentCommandStatusPassed, result.Status)
	assert.Contains(t, result.Error, "output truncated")
	assert.LessOrEqual(t, len(result.Stdout), defaultIncidentOutputBytes)
}

func TestRunIncidentLocalCommandDeniesNetworkLikeCommands(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	result := runIncidentLocalCommand(t.Context(), root, "curl https://example.invalid > network-started", 5*time.Second, incident.Context{
		Source:    incident.SourceFile,
		Reference: "local-incident",
	})

	assert.Equal(t, incidentCommandStatusFailed, result.Status)
	assert.Contains(t, result.Error, "network-like command requires explicit policy allowance")
	assert.NoFileExists(t, filepath.Join(root, "network-started"))
}

func TestParseIncidentWorktreeStatus(t *testing.T) {
	t.Parallel()

	got := parseIncidentWorktreeStatus(" M pkg/auth/session.go\n?? pkg/auth/session_test.go\nR  old.go -> new.go\n")

	require.Len(t, got, 3)
	assert.Equal(t, incident.WorktreeChange{Status: "M", Path: "pkg/auth/session.go"}, got[0])
	assert.Equal(t, incident.WorktreeChange{Status: "??", Path: "pkg/auth/session_test.go"}, got[1])
	assert.Equal(t, incident.WorktreeChange{Status: "R", Path: "new.go"}, got[2])
}

func TestIncidentNewWorktreeChangesFiltersPreRepairBaseline(t *testing.T) {
	t.Parallel()

	before := []incident.WorktreeChange{
		{Status: "M", Path: "pkg/auth/session.go"},
		{Status: "??", Path: "scratch.txt"},
	}
	after := []incident.WorktreeChange{
		{Status: "M", Path: "pkg/auth/session.go"},
		{Status: "D", Path: "scratch.txt"},
		{Status: "??", Path: "pkg/auth/session_test.go"},
	}

	got := incidentNewWorktreeChanges(after, before)

	assert.Equal(t, []incident.WorktreeChange{
		{Status: "D", Path: "scratch.txt"},
		{Status: "??", Path: "pkg/auth/session_test.go"},
	}, got)
}

func TestSentryReferencePartsParsesIssueURL(t *testing.T) {
	t.Parallel()

	org, issueID := sentryReferenceParts("https://sentry.io/organizations/acme/issues/123456/?project=42")

	assert.Equal(t, "acme", org)
	assert.Equal(t, "123456", issueID)
}

func TestSentryReferencePartsInfersHostedOrgSubdomain(t *testing.T) {
	t.Parallel()

	org, issueID := sentryReferenceParts("https://acme.sentry.io/issues/123456/events/evt-1/")

	assert.Equal(t, "acme", org)
	assert.Equal(t, "123456", issueID)
}

func TestSentryReferencePartsParsesOrgQualifiedReference(t *testing.T) {
	t.Parallel()

	org, issueID := sentryReferenceParts("acme/123456")

	assert.Equal(t, "acme", org)
	assert.Equal(t, "123456", issueID)
}

func TestSentryEventIDFromReferenceExtractsEventURL(t *testing.T) {
	t.Parallel()

	got := sentryEventIDFromReference("https://sentry.io/organizations/acme/issues/123456/events/abcdef123/")

	assert.Equal(t, "abcdef123", got)
	assert.Empty(t, sentryEventIDFromReference("API-912"))
}

func TestFetchSentryIncidentResolvesShortIDWithIssueSearchAndFetchesFullEvent(t *testing.T) {
	var paths []string

	oldClientFactory := incidentHTTPClient
	t.Cleanup(func() { incidentHTTPClient = oldClientFactory })
	incidentHTTPClient = func(time.Duration) *http.Client {
		return &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
			paths = append(paths, r.URL.RequestURI())
			assert.Equal(t, "Bearer test-token", r.Header.Get("Authorization"))

			switch r.URL.Path {
			case "/api/0/organizations/acme/issues/":
				assert.Equal(t, "1", r.URL.Query().Get("shortIdLookup"))
				assert.Equal(t, "API-912", r.URL.Query().Get("query"))
				return jsonHTTPResponse(t, []map[string]any{{
					"id":      "123456",
					"shortId": "API-912",
					"title":   "OAuth refresh panic",
					"project": map[string]any{"slug": "api-gateway"},
					"metadata": map[string]any{
						"type":  "Panic",
						"value": "nil OAuth state",
					},
				}}), nil
			case testSentryShortIDPath:
				return &http.Response{
					StatusCode: http.StatusNotFound,
					Status:     "404 Not Found",
					Body:       io.NopCloser(strings.NewReader(`{"detail":"not found"}`)),
					Header:     make(http.Header),
					Request:    r,
				}, nil
			case testSentryIssuePath:
				return jsonHTTPResponse(t, map[string]any{
					"id":        "123456",
					"shortId":   "API-912",
					"title":     "OAuth refresh panic",
					"firstSeen": "2026-05-29T10:14:00Z",
					"project":   map[string]any{"slug": "api-gateway"},
					"metadata": map[string]any{
						"type":  "Panic",
						"value": "nil OAuth state",
					},
				}), nil
			case testSentryEventsPath:
				assert.Equal(t, "true", r.URL.Query().Get("full"))
				return jsonHTTPResponse(t, []map[string]any{{
					"eventID": "evt-1",
					"entries": []map[string]any{{
						"type": "exception",
						"data": map[string]any{"values": []map[string]any{{
							"type":  "Panic",
							"value": "nil OAuth state",
							"stacktrace": map[string]any{"frames": []map[string]any{{
								"filename": "pkg/auth/session.go",
								"function": "Refresh",
								"lineno":   184,
								"in_app":   true,
							}}},
						}}},
					}},
				}}), nil
			default:
				return &http.Response{
					StatusCode: http.StatusNotFound,
					Status:     "404 Not Found",
					Body:       io.NopCloser(strings.NewReader(`{"detail":"not found"}`)),
					Header:     make(http.Header),
					Request:    r,
				}, nil
			}
		})}
	}

	t.Setenv("SENTRY_TEST_TOKEN", "test-token")

	got, err := fetchSentryIncident(t.Context(), incidentDiagnoseCommandInput{
		SentryIssue:    "API-912",
		SentryOrg:      "acme",
		SentryBaseURL:  "https://sentry.test",
		SentryTokenEnv: "SENTRY_TEST_TOKEN",
	}, 5*time.Second)
	require.NoError(t, err)

	assert.Equal(t, "API-912", got.Context.Reference)
	assert.Equal(t, "api-gateway", got.Context.Service)
	require.Len(t, got.Context.StackTrace, 1)
	assert.Equal(t, "pkg/auth/session.go", got.Context.StackTrace[0].File)
	assert.Equal(t, 184, got.Context.StackTrace[0].Line)
	assert.True(t, containsPath(paths, "/api/0/organizations/acme/issues/?"))
	assert.False(t, containsPath(paths, testSentryShortIDPath))
	assert.True(t, containsPath(paths, testSentryIssuePath))
	assert.True(t, containsPath(paths, testSentryEventsPath+"?"))
}

func TestFetchSentryIncidentFallsBackToLegacyShortIDLookup(t *testing.T) {
	var paths []string

	oldClientFactory := incidentHTTPClient
	t.Cleanup(func() { incidentHTTPClient = oldClientFactory })
	incidentHTTPClient = func(time.Duration) *http.Client {
		return &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
			paths = append(paths, r.URL.RequestURI())

			switch r.URL.Path {
			case testSentryShortIDPath:
				return jsonHTTPResponse(t, map[string]any{
					"groupId": "123456",
					"shortId": "API-912",
					"group": map[string]any{
						"id":      "123456",
						"shortId": "API-912",
						"title":   "OAuth refresh panic",
						"project": map[string]any{"slug": "api-gateway"},
					},
				}), nil
			case "/api/0/organizations/acme/issues/":
				assert.Equal(t, "1", r.URL.Query().Get("shortIdLookup"))
				assert.Equal(t, "API-912", r.URL.Query().Get("query"))
				return &http.Response{
					StatusCode: http.StatusServiceUnavailable,
					Status:     "503 Service Unavailable",
					Body:       io.NopCloser(strings.NewReader(`{"detail":"search unavailable"}`)),
					Header:     make(http.Header),
					Request:    r,
				}, nil
			case testSentryIssuePath:
				return jsonHTTPResponse(t, map[string]any{
					"id":      "123456",
					"shortId": "API-912",
					"project": map[string]any{"slug": "api-gateway"},
				}), nil
			case testSentryEventsPath:
				return jsonHTTPResponse(t, []map[string]any{}), nil
			default:
				return &http.Response{
					StatusCode: http.StatusNotFound,
					Status:     "404 Not Found",
					Body:       io.NopCloser(strings.NewReader(`{"detail":"not found"}`)),
					Header:     make(http.Header),
					Request:    r,
				}, nil
			}
		})}
	}

	t.Setenv("SENTRY_TEST_TOKEN", "test-token")

	got, err := fetchSentryIncident(t.Context(), incidentDiagnoseCommandInput{
		SentryIssue:    "API-912",
		SentryOrg:      "acme",
		SentryBaseURL:  "https://sentry.test",
		SentryTokenEnv: "SENTRY_TEST_TOKEN",
	}, 5*time.Second)
	require.NoError(t, err)

	assert.Equal(t, "API-912", got.Context.Reference)
	assert.Equal(t, "api-gateway", got.Context.Service)
	assert.True(t, containsPath(paths, testSentryShortIDPath))
	assert.True(t, containsPath(paths, "/api/0/organizations/acme/issues/?"))
}

func TestFetchSentryIncidentUsesEventURLForExplicitEvent(t *testing.T) {
	var paths []string

	oldClientFactory := incidentHTTPClient
	t.Cleanup(func() { incidentHTTPClient = oldClientFactory })
	incidentHTTPClient = func(time.Duration) *http.Client {
		return &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
			paths = append(paths, r.URL.RequestURI())

			switch r.URL.Path {
			case testSentryIssuePath:
				return jsonHTTPResponse(t, map[string]any{
					"id":      "123456",
					"shortId": "API-912",
					"project": map[string]any{"slug": "api-gateway"},
					"metadata": map[string]any{
						"type":  "Panic",
						"value": "nil OAuth state",
					},
				}), nil
			case testSentryEventsPath + "evt-1/":
				return jsonHTTPResponse(t, map[string]any{
					"eventID": "evt-1",
					"entries": []map[string]any{{
						"type": "exception",
						"data": map[string]any{"values": []map[string]any{{
							"type":  "Panic",
							"value": "nil OAuth state",
							"stacktrace": map[string]any{"frames": []map[string]any{{
								"filename": "pkg/auth/session.go",
								"function": "Refresh",
								"lineno":   184,
								"in_app":   true,
							}}},
						}}},
					}},
				}), nil
			default:
				return &http.Response{
					StatusCode: http.StatusNotFound,
					Status:     "404 Not Found",
					Body:       io.NopCloser(strings.NewReader(`{"detail":"not found"}`)),
					Header:     make(http.Header),
					Request:    r,
				}, nil
			}
		})}
	}

	t.Setenv("SENTRY_TEST_TOKEN", "test-token")

	got, err := fetchSentryIncident(t.Context(), incidentDiagnoseCommandInput{
		SentryIssue:    "https://sentry.test/organizations/acme/issues/123456/events/evt-1/",
		SentryBaseURL:  "https://sentry.test",
		SentryTokenEnv: "SENTRY_TEST_TOKEN",
	}, 5*time.Second)
	require.NoError(t, err)

	require.Len(t, got.Context.StackTrace, 1)
	assert.Equal(t, "pkg/auth/session.go", got.Context.StackTrace[0].File)
	assert.True(t, containsPath(paths, testSentryEventsPath+"evt-1/"))
	assert.False(t, containsPath(paths, testSentryEventsPath+"?"))
}

func TestFetchSentryIncidentUsesReferenceHostAsBaseURL(t *testing.T) {
	var hosts []string

	oldClientFactory := incidentHTTPClient
	t.Cleanup(func() { incidentHTTPClient = oldClientFactory })
	incidentHTTPClient = func(time.Duration) *http.Client {
		return &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
			hosts = append(hosts, r.URL.Host)

			switch r.URL.Path {
			case testSentryIssuePath:
				return jsonHTTPResponse(t, map[string]any{
					"id":      "123456",
					"shortId": "API-912",
					"project": map[string]any{"slug": "api-gateway"},
					"metadata": map[string]any{
						"type":  "Panic",
						"value": "nil OAuth state",
					},
				}), nil
			case testSentryEventsPath:
				return jsonHTTPResponse(t, []map[string]any{}), nil
			default:
				return &http.Response{
					StatusCode: http.StatusNotFound,
					Status:     "404 Not Found",
					Body:       io.NopCloser(strings.NewReader(`{"detail":"not found"}`)),
					Header:     make(http.Header),
					Request:    r,
				}, nil
			}
		})}
	}

	t.Setenv("SENTRY_TEST_TOKEN", "test-token")
	t.Setenv("SENTRY_BASE_URL", "")
	t.Setenv("SENTRY_HOST", "")

	got, err := fetchSentryIncident(t.Context(), incidentDiagnoseCommandInput{
		SentryIssue:    "https://sentry.example.test/organizations/acme/issues/123456/",
		SentryTokenEnv: "SENTRY_TEST_TOKEN",
	}, 5*time.Second)
	require.NoError(t, err)

	assert.Equal(t, "API-912", got.Context.Reference)
	assert.NotEmpty(t, hosts)
	for _, host := range hosts {
		assert.Equal(t, "sentry.example.test", host)
	}
}

func TestFetchSentryIncidentRedactsTransportErrors(t *testing.T) {
	oldClientFactory := incidentHTTPClient
	t.Cleanup(func() { incidentHTTPClient = oldClientFactory })
	incidentHTTPClient = func(time.Duration) *http.Client {
		return &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
			return nil, errors.New("transport failed for bob@example.com access_token=secret-token " + testSentryOpaqueToken)
		})}
	}

	t.Setenv("SENTRY_TEST_TOKEN", testSentryOpaqueToken)

	_, err := fetchSentryIncident(t.Context(), incidentDiagnoseCommandInput{
		SentryIssue:    "123456",
		SentryOrg:      "acme",
		SentryBaseURL:  "https://sentry.test",
		SentryTokenEnv: "SENTRY_TEST_TOKEN",
	}, 5*time.Second)
	require.Error(t, err)

	assert.NotContains(t, err.Error(), "bob@example.com")
	assert.NotContains(t, err.Error(), "secret-token")
	assert.NotContains(t, err.Error(), testSentryOpaqueToken)
	assert.Contains(t, err.Error(), "[REDACTED")
}

func TestFetchSentryIncidentRedactsTokenEchoedInStatusBody(t *testing.T) {
	oldClientFactory := incidentHTTPClient
	t.Cleanup(func() { incidentHTTPClient = oldClientFactory })
	incidentHTTPClient = func(time.Duration) *http.Client {
		return &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
			return &http.Response{
				StatusCode: http.StatusForbidden,
				Status:     "403 Forbidden",
				Body:       io.NopCloser(strings.NewReader(`{"detail":"token ` + testSentryOpaqueToken + ` rejected"}`)),
				Header:     make(http.Header),
			}, nil
		})}
	}

	t.Setenv("SENTRY_TEST_TOKEN", testSentryOpaqueToken)

	_, err := fetchSentryIncident(t.Context(), incidentDiagnoseCommandInput{
		SentryIssue:    "123456",
		SentryOrg:      "acme",
		SentryBaseURL:  "https://sentry.test",
		SentryTokenEnv: "SENTRY_TEST_TOKEN",
	}, 5*time.Second)
	require.Error(t, err)

	assert.NotContains(t, err.Error(), testSentryOpaqueToken)
	assert.Contains(t, err.Error(), "[REDACTED:sentry_token]")
}

func TestFetchSentryIncidentUsesFallbackTokenEnv(t *testing.T) {
	oldClientFactory := incidentHTTPClient
	t.Cleanup(func() { incidentHTTPClient = oldClientFactory })

	var authorization string
	incidentHTTPClient = func(time.Duration) *http.Client {
		return &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
			authorization = r.Header.Get("Authorization")
			switch r.URL.Path {
			case testSentryIssuePath:
				return jsonHTTPResponse(t, map[string]any{
					"id":      "123456",
					"shortId": "API-912",
					"project": map[string]any{"slug": "api-gateway"},
					"metadata": map[string]any{
						"type":  "Panic",
						"value": "nil OAuth state",
					},
				}), nil
			case testSentryEventsPath:
				return jsonHTTPResponse(t, []map[string]any{}), nil
			default:
				return &http.Response{
					StatusCode: http.StatusNotFound,
					Status:     "404 Not Found",
					Body:       io.NopCloser(strings.NewReader(`{"detail":"not found"}`)),
					Header:     make(http.Header),
					Request:    r,
				}, nil
			}
		})}
	}

	t.Setenv("SENTRY_AUTH_TOKEN", "")
	t.Setenv("SENTRY_ACCESS_TOKEN", "fallback-token")
	t.Setenv("SENTRY_TOKEN", "")

	got, err := fetchSentryIncident(t.Context(), incidentDiagnoseCommandInput{
		SentryIssue:   "123456",
		SentryOrg:     "acme",
		SentryBaseURL: "https://sentry.test",
	}, 5*time.Second)
	require.NoError(t, err)

	assert.Equal(t, "Bearer fallback-token", authorization)
	assert.Equal(t, "API-912", got.Context.Reference)
	assert.Equal(t, "api-gateway", got.Context.Service)
}

func TestFetchSentryIncidentMissingTokenMentionsFallbackEnvs(t *testing.T) {
	t.Setenv("SENTRY_AUTH_TOKEN", "")
	t.Setenv("SENTRY_ACCESS_TOKEN", "")
	t.Setenv("SENTRY_TOKEN", "")

	_, err := fetchSentryIncident(t.Context(), incidentDiagnoseCommandInput{
		SentryIssue:   "123456",
		SentryOrg:     "acme",
		SentryBaseURL: "https://sentry.test",
	}, 5*time.Second)
	require.Error(t, err)

	assert.Contains(t, err.Error(), "SENTRY_AUTH_TOKEN")
	assert.Contains(t, err.Error(), "SENTRY_ACCESS_TOKEN")
	assert.Contains(t, err.Error(), "SENTRY_TOKEN")
}

func TestSentryBaseURLUsesHostFallbackAndHTTPSDefault(t *testing.T) {
	t.Setenv("SENTRY_BASE_URL", "")
	t.Setenv("SENTRY_HOST", "sentry.example.test/")

	assert.Equal(t, "https://sentry.example.test", sentryBaseURL(""))
	assert.Equal(t, "http://localhost:9000", sentryBaseURL("http://localhost:9000/"))
}

func TestIncidentPayloadFromMCPResultWrapsTextContent(t *testing.T) {
	t.Parallel()

	payload := incidentPayloadFromMCPResult(json.RawMessage(`{
		"content": [{"type": "text", "text": "payment queue depth exceeded for bob@example.com"}]
	}`), "alert-42")

	assert.JSONEq(t, `{
		"source": "mcp",
		"reference": "alert-42",
		"message": "payment queue depth exceeded for bob@example.com"
	}`, string(payload))
}

func TestIncidentPayloadFromMCPResultUsesStructuredContent(t *testing.T) {
	t.Parallel()

	payload := incidentPayloadFromMCPResult(json.RawMessage(`{
		"structuredContent": {
			"source": "grafana",
			"reference": "alert-42",
			"service": "payments",
			"message": "queue depth exceeded"
		},
		"content": [{"type": "text", "text": "fallback text"}]
	}`), "alert-42")

	assert.JSONEq(t, `{
		"source": "grafana",
		"reference": "alert-42",
		"service": "payments",
		"message": "queue depth exceeded"
	}`, string(payload))
}

func TestIncidentPayloadFromMCPResultUsesSnakeCaseStructuredContent(t *testing.T) {
	t.Parallel()

	payload := incidentPayloadFromMCPResult(json.RawMessage(`{
		"structured_content": {
			"source": "grafana",
			"reference": "alert-42",
			"service": "payments",
			"message": "queue depth exceeded"
		}
	}`), "alert-42")

	assert.JSONEq(t, `{
		"source": "grafana",
		"reference": "alert-42",
		"service": "payments",
		"message": "queue depth exceeded"
	}`, string(payload))
}

func TestIncidentPayloadFromMCPResultWrapsArrayContentAsLogs(t *testing.T) {
	t.Parallel()

	payload := incidentPayloadFromMCPResult(json.RawMessage(`{
		"structuredContent": [
			{"source": "loki", "message": "failed for bob@example.com"}
		]
	}`), "alert-42")

	assert.JSONEq(t, `{
		"source": "mcp",
		"reference": "alert-42",
		"logs": [
			{"source": "loki", "message": "failed for bob@example.com"}
		]
	}`, string(payload))
}

func TestIncidentPayloadFromMCPResultUsesResourceTextContent(t *testing.T) {
	t.Parallel()

	payload := incidentPayloadFromMCPResult(json.RawMessage(`{
		"content": [{
			"type": "resource",
			"resource": {
				"uri": "incident://alert-42",
				"mimeType": "application/json",
				"text": "{\"source\":\"grafana\",\"reference\":\"alert-42\",\"service\":\"payments\",\"message\":\"queue depth exceeded\"}"
			}
		}]
	}`), "alert-42")

	assert.JSONEq(t, `{
		"source": "grafana",
		"reference": "alert-42",
		"service": "payments",
		"message": "queue depth exceeded"
	}`, string(payload))
}

func TestFetchMCPIncidentCallsConfiguredTool(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	manifestPath := filepath.Join(root, "mcp.yaml")
	manifest := fmt.Sprintf(`servers:
  - name: grafana
    command: %q
    args:
      - "-test.run=TestIncidentMCPHelperProcess"
      - "--"
    env:
      GO_WANT_INCIDENT_MCP_HELPER_PROCESS: "1"
`, os.Args[0])
	require.NoError(t, os.WriteFile(manifestPath, []byte(manifest), 0o600)) //nolint:gosec // manifestPath is scoped to t.TempDir.

	got, err := fetchMCPIncident(t.Context(), incidentDiagnoseCommandInput{
		Reference:          "alert-42",
		MCPManifestPath:    manifestPath,
		MCPServerName:      "grafana",
		MCPToolName:        "get_incident",
		MCPToolArgsJSON:    `{"severity":"critical"}`,
		TimeoutSeconds:     5,
		ValidationCommands: nil,
	}, 5*time.Second)
	require.NoError(t, err)

	assert.Equal(t, incident.SourceMCP, got.Context.Source)
	assert.Equal(t, "alert-42", got.Context.Reference)
	assert.Equal(t, "payments", got.Context.Service)
	assert.NotContains(t, got.Context.Message, "bob@example.com")
	require.Len(t, got.Context.Logs, 1)
	assert.NotContains(t, got.Context.Logs[0].Message, "bob@example.com")
}

func TestFetchMCPIncidentRedactsToolErrors(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	manifestPath := filepath.Join(root, "mcp.yaml")
	manifest := fmt.Sprintf(`servers:
  - name: grafana
    command: %q
    args:
      - "-test.run=TestIncidentMCPHelperProcess"
      - "--"
    env:
      GO_WANT_INCIDENT_MCP_HELPER_PROCESS: "1"
`, os.Args[0])
	require.NoError(t, os.WriteFile(manifestPath, []byte(manifest), 0o600)) //nolint:gosec // manifestPath is scoped to t.TempDir.

	_, err := fetchMCPIncident(t.Context(), incidentDiagnoseCommandInput{
		Reference:       "alert-42",
		MCPManifestPath: manifestPath,
		MCPServerName:   "grafana",
		MCPToolName:     "get_incident",
		MCPToolArgsJSON: `{"severity":"explode"}`,
	}, 5*time.Second)
	require.Error(t, err)

	assert.NotContains(t, err.Error(), "bob@example.com")
	assert.NotContains(t, err.Error(), "secret-token")
	assert.Contains(t, err.Error(), "[REDACTED")
}

func TestFetchMCPIncidentRedactsSensitiveToolArgsInRPCError(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	manifestPath := filepath.Join(root, "mcp.yaml")
	manifest := fmt.Sprintf(`servers:
  - name: grafana
    command: %q
    args:
      - "-test.run=TestIncidentMCPHelperProcess"
      - "--"
    env:
      GO_WANT_INCIDENT_MCP_HELPER_PROCESS: "1"
`, os.Args[0])
	require.NoError(t, os.WriteFile(manifestPath, []byte(manifest), 0o600)) //nolint:gosec // manifestPath is scoped to t.TempDir.

	sensitiveArg := strings.Join([]string{"opaque", "mcp", "value"}, "-")
	_, err := fetchMCPIncident(t.Context(), incidentDiagnoseCommandInput{
		Reference:       "alert-42",
		MCPManifestPath: manifestPath,
		MCPServerName:   "grafana",
		MCPToolName:     "get_incident",
		MCPToolArgsJSON: fmt.Sprintf(`{"severity":"explode","api_key":%q}`, sensitiveArg),
	}, 5*time.Second)
	require.Error(t, err)

	assert.NotContains(t, err.Error(), sensitiveArg)
	assert.Contains(t, err.Error(), "[REDACTED")
}

func TestFetchMCPIncidentRejectsToolResultErrors(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	manifestPath := filepath.Join(root, "mcp.yaml")
	manifest := fmt.Sprintf(`servers:
  - name: grafana
    command: %q
    args:
      - "-test.run=TestIncidentMCPHelperProcess"
      - "--"
    env:
      GO_WANT_INCIDENT_MCP_HELPER_PROCESS: "1"
`, os.Args[0])
	require.NoError(t, os.WriteFile(manifestPath, []byte(manifest), 0o600)) //nolint:gosec // manifestPath is scoped to t.TempDir.

	_, err := fetchMCPIncident(t.Context(), incidentDiagnoseCommandInput{
		Reference:       "alert-42",
		MCPManifestPath: manifestPath,
		MCPServerName:   "grafana",
		MCPToolName:     "get_incident",
		MCPToolArgsJSON: `{"severity":"result-error"}`,
	}, 5*time.Second)
	require.Error(t, err)

	assert.Contains(t, err.Error(), "tool returned error result")
	assert.NotContains(t, err.Error(), "bob@example.com")
	assert.NotContains(t, err.Error(), "secret-token")
	assert.Contains(t, err.Error(), "[REDACTED")
}

func TestFetchMCPIncidentRedactsSensitiveToolArgsInResultError(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	manifestPath := filepath.Join(root, "mcp.yaml")
	manifest := fmt.Sprintf(`servers:
  - name: grafana
    command: %q
    args:
      - "-test.run=TestIncidentMCPHelperProcess"
      - "--"
    env:
      GO_WANT_INCIDENT_MCP_HELPER_PROCESS: "1"
`, os.Args[0])
	require.NoError(t, os.WriteFile(manifestPath, []byte(manifest), 0o600)) //nolint:gosec // manifestPath is scoped to t.TempDir.

	sensitiveArg := strings.Join([]string{"opaque", "mcp", "result", "value"}, "-")
	_, err := fetchMCPIncident(t.Context(), incidentDiagnoseCommandInput{
		Reference:       "alert-42",
		MCPManifestPath: manifestPath,
		MCPServerName:   "grafana",
		MCPToolName:     "get_incident",
		MCPToolArgsJSON: fmt.Sprintf(`{"severity":"result-error","api_key":%q}`, sensitiveArg),
	}, 5*time.Second)
	require.Error(t, err)

	assert.NotContains(t, err.Error(), sensitiveArg)
	assert.Contains(t, err.Error(), "[REDACTED")
}

func TestFetchMCPIncidentRedactsManifestErrors(t *testing.T) {
	t.Parallel()

	_, err := fetchMCPIncident(t.Context(), incidentDiagnoseCommandInput{
		MCPManifestPath: filepath.Join(t.TempDir(), "missing-bob@example.com-access_token=secret-token.yaml"),
		MCPServerName:   "grafana",
		MCPToolName:     "get_incident",
	}, 5*time.Second)
	require.Error(t, err)

	assert.NotContains(t, err.Error(), "bob@example.com")
	assert.NotContains(t, err.Error(), "secret-token")
	assert.Contains(t, err.Error(), "[REDACTED")
}

func TestOpenIncidentPRWritesRedactedBodyAndCallsGitHubCLI(t *testing.T) {
	root := t.TempDir()
	binDir := filepath.Join(root, "bin")
	require.NoError(t, os.MkdirAll(binDir, 0o750))

	capturePath := filepath.Join(root, "gh-args.json")
	fakeGHPath := filepath.Join(binDir, "gh")
	fakeGH := `#!/bin/sh
if [ -z "$GH_TOKEN" ]; then
  printf 'missing GH_TOKEN\n' >&2
  exit 42
fi
printf 'https://github.com/acme/api/pull/123\ncreated for bob@example.com access_token=secret-token\n'
: > "$GH_CAPTURE"
for arg in "$@"; do
  printf '%s\n' "$arg" >> "$GH_CAPTURE"
done
`
	require.NoError(t, os.WriteFile(fakeGHPath, []byte(fakeGH), 0o700)) //nolint:gosec // executable fake gh is scoped to t.TempDir.
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("GH_CAPTURE", capturePath)
	t.Setenv("GH_TOKEN", "ghp_secret-token")

	auditDir := filepath.Join(root, "audit")
	t.Setenv(attshell.EnvAuditDir, auditDir)

	prURL, err := openIncidentPR(t.Context(), root, incident.Analysis{
		Incident: incident.Context{
			Source:    incident.SourceSentry,
			Reference: "API-912 bob@example.com access_token=secret-token",
			Message:   "failed for alice@example.com access_token=secret-token",
		},
		RedactionPolicy: incident.RedactionPolicyVersion,
		Risk:            incident.Risk{Level: "Medium", Rationale: "test"},
		PRPlan: incident.PRPlan{
			Title: "Fix incident for bob@example.com access_token=secret-token",
		},
		Validation: []incident.CommandResult{{
			Command: "go test ./pkg/auth",
			Status:  incidentCommandStatusPassed,
		}},
	}, incidentDiagnoseCommandInput{}, 5*time.Second)
	require.NoError(t, err)
	assert.Equal(t, "https://github.com/acme/api/pull/123", prURL)

	argsData, err := os.ReadFile(capturePath)
	require.NoError(t, err)
	args := strings.Split(strings.TrimSpace(string(argsData)), "\n")
	require.Len(t, args, 6)
	assert.Equal(t, []string{"pr", "create", "--title"}, args[:3])
	assert.NotContains(t, args[3], "bob@example.com")
	assert.NotContains(t, args[3], "secret-token")
	assert.Contains(t, args[3], "[REDACTED")
	assert.Equal(t, "--body-file", args[4])

	bodyPath := filepath.Clean(args[5])
	require.True(t, strings.HasPrefix(bodyPath, root+string(os.PathSeparator)), "body path should stay under test root: %s", bodyPath)
	assert.NotContains(t, bodyPath, "bob@example.com")
	assert.NotContains(t, bodyPath, "secret-token")

	body, err := os.ReadFile(bodyPath)
	require.NoError(t, err)
	assert.Contains(t, string(body), "Production incident diagnosis")
	assert.NotContains(t, string(body), "alice@example.com")
	assert.NotContains(t, string(body), "bob@example.com")
	assert.NotContains(t, string(body), "secret-token")

	outputs, err := filepath.Glob(filepath.Join(auditDir, "outputs", "*.json"))
	require.NoError(t, err)
	require.Len(t, outputs, 1)

	auditOutput, err := os.ReadFile(outputs[0])
	require.NoError(t, err)
	assert.NotContains(t, string(auditOutput), "bob@example.com")
	assert.NotContains(t, string(auditOutput), "secret-token")
	assert.Contains(t, string(auditOutput), "[REDACTED")

	ledger, err := os.ReadFile(filepath.Join(auditDir, "commands.jsonl"))
	require.NoError(t, err)
	assert.NotContains(t, string(ledger), "bob@example.com")
	assert.NotContains(t, string(ledger), "secret-token")
	assert.NotContains(t, string(ledger), "ghp_secret-token")
}

func TestOpenIncidentPRRejectsOutsideWorkspacePRBodyPath(t *testing.T) {
	root := t.TempDir()
	capturePath := filepath.Join(root, "gh-called")
	binDir := filepath.Join(root, "bin")
	require.NoError(t, os.MkdirAll(binDir, 0o750))
	require.NoError(t, os.WriteFile(filepath.Join(binDir, "gh"), []byte("#!/bin/sh\ntouch \"$GH_CAPTURE\"\n"), 0o700)) //nolint:gosec // executable fake gh is scoped to t.TempDir.
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("GH_CAPTURE", capturePath)

	outside := filepath.Join(filepath.Dir(root), "bob@example.com-access_token=secret-token-pr.md")
	_, err := openIncidentPR(t.Context(), root, incident.Analysis{
		Incident:        incident.Context{Source: incident.SourceFile, Reference: "local-incident"},
		RedactionPolicy: incident.RedactionPolicyVersion,
		Risk:            incident.Risk{Level: "Medium", Rationale: "test"},
		PRPlan:          incident.PRPlan{Title: "Fix incident"},
		Validation: []incident.CommandResult{{
			Command: "go test ./pkg/auth",
			Status:  incidentCommandStatusPassed,
		}},
	}, incidentDiagnoseCommandInput{PRBodyPath: outside}, 5*time.Second)
	require.Error(t, err)

	assert.Contains(t, err.Error(), "outside workspace")
	assert.NotContains(t, err.Error(), "bob@example.com")
	assert.NotContains(t, err.Error(), "secret-token")
	assert.NoFileExists(t, capturePath)
}

func TestOpenIncidentPRRejectsMissingValidation(t *testing.T) {
	t.Parallel()

	_, err := openIncidentPR(t.Context(), t.TempDir(), incident.Analysis{
		Incident:        incident.Context{Source: incident.SourceFile, Reference: "local-incident"},
		RedactionPolicy: incident.RedactionPolicyVersion,
		Risk:            incident.Risk{Level: "Medium", Rationale: "test"},
		PRPlan:          incident.PRPlan{Title: "Fix incident"},
	}, incidentDiagnoseCommandInput{}, 5*time.Second)
	require.Error(t, err)

	assert.Contains(t, err.Error(), "validation was not captured")
}

func TestOpenIncidentPRRejectsFailedValidation(t *testing.T) {
	root := t.TempDir()
	capturePath := filepath.Join(root, "gh-called")
	binDir := filepath.Join(root, "bin")
	require.NoError(t, os.MkdirAll(binDir, 0o750))
	require.NoError(t, os.WriteFile(filepath.Join(binDir, "gh"), []byte("#!/bin/sh\ntouch \"$GH_CAPTURE\"\n"), 0o700)) //nolint:gosec // executable fake gh is scoped to t.TempDir.
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("GH_CAPTURE", capturePath)

	_, err := openIncidentPR(t.Context(), root, incident.Analysis{
		Incident:        incident.Context{Source: incident.SourceFile, Reference: "local-incident"},
		RedactionPolicy: incident.RedactionPolicyVersion,
		Risk:            incident.Risk{Level: "Medium", Rationale: "test"},
		PRPlan:          incident.PRPlan{Title: "Fix incident"},
		Validation: []incident.CommandResult{{
			Command: "go test ./pkg/auth --token=secret-token",
			Status:  incidentCommandStatusFailed,
		}},
	}, incidentDiagnoseCommandInput{}, 5*time.Second)
	require.Error(t, err)

	assert.Contains(t, err.Error(), "validation failed")
	assert.NotContains(t, err.Error(), "secret-token")
	assert.NoFileExists(t, capturePath)
}

func TestOpenIncidentPRRejectsValidationWithoutPassedStatus(t *testing.T) {
	root := t.TempDir()
	capturePath := filepath.Join(root, "gh-called")
	binDir := filepath.Join(root, "bin")
	require.NoError(t, os.MkdirAll(binDir, 0o750))
	require.NoError(t, os.WriteFile(filepath.Join(binDir, "gh"), []byte("#!/bin/sh\ntouch \"$GH_CAPTURE\"\n"), 0o700)) //nolint:gosec // executable fake gh is scoped to t.TempDir.
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("GH_CAPTURE", capturePath)

	_, err := openIncidentPR(t.Context(), root, incident.Analysis{
		Incident:        incident.Context{Source: incident.SourceFile, Reference: "local-incident"},
		RedactionPolicy: incident.RedactionPolicyVersion,
		Risk:            incident.Risk{Level: "Medium", Rationale: "test"},
		PRPlan:          incident.PRPlan{Title: "Fix incident"},
		Validation: []incident.CommandResult{{
			Command:      "go test ./pkg/auth --token=secret-token",
			Status:       "not_run",
			NotRunReason: "validation command was skipped",
		}},
	}, incidentDiagnoseCommandInput{}, 5*time.Second)
	require.Error(t, err)

	assert.Contains(t, err.Error(), "validation failed")
	assert.NotContains(t, err.Error(), "secret-token")
	assert.NoFileExists(t, capturePath)
}

func TestIncidentCreatedPRURLRedactsSensitiveURLFragments(t *testing.T) {
	t.Parallel()

	got := incidentCreatedPRURL("created https://github.com/acme/api/pull/123?access_token=secret-token")

	assert.NotContains(t, got, "secret-token")
	assert.Contains(t, got, "https://github.com/acme/api/pull/123")
	assert.Contains(t, got, "[REDACTED")
}

func TestSafeIncidentFilenameFallsBackAndBoundsLength(t *testing.T) {
	t.Parallel()

	assert.Equal(t, incidentUnknownReference, safeIncidentFilename("!!!"))
	assert.Equal(t, "API-912-oauth-state", safeIncidentFilename("API-912 oauth/state"))
	assert.LessOrEqual(t, len(safeIncidentFilename(strings.Repeat("a", maxIncidentFilenameLength+20))), maxIncidentFilenameLength)
}

func TestNormalizeIncidentOutputFormatAcceptsMarkdown(t *testing.T) {
	t.Parallel()

	got, err := normalizeIncidentOutputFormat("markdown")

	require.NoError(t, err)
	assert.Equal(t, commandOutputMarkdown, got)
}

func TestIncidentHistoryPathQueriesRequireTouchedFiles(t *testing.T) {
	t.Parallel()

	result := githistory.Result{
		Commit: githistory.Commit{
			Files: []string{"pkg/auth/session.go", "README.md"},
		},
	}

	assert.True(t, incidentHistoryResultMatchesQuery(result, "github.com/acme/api/pkg/auth/session.go"))
	assert.True(t, incidentHistoryResultMatchesQuery(result, "session.go"))
	assert.False(t, incidentHistoryResultMatchesQuery(result, "pkg/payments/handler.go"))
	assert.True(t, incidentHistoryResultMatchesQuery(result, "token refresh panic"))
}

func TestIncidentHistoryQueriesIncludeDeploymentMetadata(t *testing.T) {
	t.Parallel()

	got := incidentHistoryQueries(incident.Context{
		Deployments: []incident.Deployment{{
			Version:     "2026.05.29-1",
			Commit:      "abc123def456",
			Environment: "production",
		}},
	})

	assert.Contains(t, got, "2026.05.29-1")
	assert.Contains(t, got, "abc123def456")
	assert.Contains(t, got, "production")
}

func TestIncidentRecentChangesRedactsGitHistoryAuditOutput(t *testing.T) {
	root := t.TempDir()
	binDir := filepath.Join(root, "bin")
	require.NoError(t, os.MkdirAll(binDir, 0o750))
	fakeGitPath := filepath.Join(binDir, "git")
	fakeGit := `#!/bin/sh
if [ "$1" = "log" ]; then
  printf 'abc123def456\037Alice alice@example.com\037alice@example.com\0372026-05-29T10:00:00Z\037Fix OAuth state access_token=secret-token (#218)\036\npkg/auth/session.go\n'
  exit 0
fi
printf '%s\n' "unexpected git command for bob@example.com access_token=secret-token" >&2
exit 1
`
	require.NoError(t, os.WriteFile(fakeGitPath, []byte(fakeGit), 0o700)) //nolint:gosec // executable fake git is scoped to t.TempDir.
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))

	auditDir := filepath.Join(root, "audit")
	t.Setenv(attshell.EnvAuditDir, auditDir)

	changes, warnings := incidentRecentChanges(t.Context(), root, incident.Context{
		Source:    incident.SourceSentry,
		Reference: "API-912 alice@example.com access_token=secret-token",
		StackTrace: []incident.StackFrame{{
			File: "pkg/auth/session.go",
		}},
	})
	require.Empty(t, warnings)
	require.Len(t, changes, 1)
	assert.Contains(t, changes[0].Subject, "Fix OAuth state")
	assert.Contains(t, changes[0].PullRequests, "#218")
	assert.NotContains(t, changes[0].Author+changes[0].Subject+strings.Join(changes[0].Files, " "), "alice@example.com")
	assert.NotContains(t, changes[0].Author+changes[0].Subject+strings.Join(changes[0].Files, " "), "secret-token")

	outputs, err := filepath.Glob(filepath.Join(auditDir, "outputs", "*.json"))
	require.NoError(t, err)
	require.Len(t, outputs, 1)

	auditOutput, err := os.ReadFile(outputs[0])
	require.NoError(t, err)
	assert.NotContains(t, string(auditOutput), "alice@example.com")
	assert.NotContains(t, string(auditOutput), "secret-token")
	assert.Contains(t, string(auditOutput), "[REDACTED")

	ledger, err := os.ReadFile(filepath.Join(auditDir, "commands.jsonl"))
	require.NoError(t, err)
	assert.Contains(t, string(ledger), "atteler.incident.git_history")
	assert.NotContains(t, string(ledger), "alice@example.com")
	assert.NotContains(t, string(ledger), "secret-token")
}

//nolint:paralleltest // Helper process entry point must run synchronously when env-gated.
func TestIncidentMCPHelperProcess(_ *testing.T) {
	if os.Getenv("GO_WANT_INCIDENT_MCP_HELPER_PROCESS") != "1" {
		return
	}

	scanner := bufio.NewScanner(os.Stdin)
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for scanner.Scan() {
		var request incidentMCPHelperRequest
		if err := json.Unmarshal(scanner.Bytes(), &request); err != nil {
			os.Exit(2)
		}

		switch request.Method {
		case "initialize":
			writeIncidentMCPHelperResponse(map[string]any{
				"jsonrpc": "2.0",
				"id":      request.ID,
				"result": map[string]any{
					"protocolVersion": "2025-11-25",
					"capabilities": map[string]any{
						"tools": map[string]any{},
					},
					"serverInfo": map[string]any{
						"name":    "incident-helper",
						"version": "1.0.0",
					},
				},
			})
		case "notifications/initialized":
			continue
		case "tools/list":
			writeIncidentMCPToolsListResponse(request.ID)
		case "tools/call":
			handleIncidentMCPToolCall(request)
			os.Exit(0)
		default:
			writeIncidentMCPUnexpectedResponse(request.ID)
			os.Exit(0)
		}
	}
	if err := scanner.Err(); err != nil {
		os.Exit(2)
	}
	os.Exit(0)
}

type incidentMCPHelperRequest struct {
	ID     any    `json:"id"`
	Method string `json:"method"`
	Params struct {
		Arguments map[string]any `json:"arguments"`
		Name      string         `json:"name"`
	} `json:"params"`
}

func writeIncidentMCPToolsListResponse(id any) {
	writeIncidentMCPHelperResponse(map[string]any{
		"jsonrpc": "2.0",
		"id":      id,
		"result": map[string]any{
			"tools": []map[string]any{{
				"name": "get_incident",
				"inputSchema": map[string]any{
					"type": "object",
					"properties": map[string]any{
						"reference": map[string]any{"type": "string"},
						"severity":  map[string]any{"type": "string"},
						"api_key":   map[string]any{"type": "string"},
					},
					"required":             []string{"reference"},
					"additionalProperties": false,
				},
			}},
		},
	})
}

func handleIncidentMCPToolCall(request incidentMCPHelperRequest) {
	if request.Method != "tools/call" ||
		request.Params.Name != "get_incident" ||
		request.Params.Arguments["reference"] != "alert-42" ||
		!incidentMCPHelperSeverityAllowed(request.Params.Arguments["severity"]) {
		writeIncidentMCPUnexpectedResponse(request.ID)
		return
	}

	if request.Params.Arguments["severity"] == "explode" {
		writeIncidentMCPHelperResponse(map[string]any{
			"jsonrpc": "2.0",
			"id":      request.ID,
			"error": map[string]any{
				"code":    -32000,
				"message": "failed for bob@example.com access_token=secret-token " + incidentMCPHelperAPIKeySuffix(request.Params.Arguments),
			},
		})
		return
	}

	if request.Params.Arguments["severity"] == "result-error" {
		writeIncidentMCPHelperResponse(map[string]any{
			"jsonrpc": "2.0",
			"id":      request.ID,
			"result": map[string]any{
				"isError": true,
				"content": []map[string]string{{
					"type": "text",
					"text": "failed for bob@example.com access_token=secret-token " + incidentMCPHelperAPIKeySuffix(request.Params.Arguments),
				}},
			},
		})
		return
	}

	toolPayload := `{
		"source": "grafana",
		"reference": "alert-from-tool",
		"service": "payments",
		"message": "queue depth exceeded for bob@example.com",
		"logs": [{"source":"loki","message":"failed for bob@example.com"}]
	}`
	writeIncidentMCPHelperResponse(map[string]any{
		"jsonrpc": "2.0",
		"id":      request.ID,
		"result": map[string]any{
			"content": []map[string]string{{
				"type": "text",
				"text": toolPayload,
			}},
		},
	})
}

func writeIncidentMCPUnexpectedResponse(id any) {
	writeIncidentMCPHelperResponse(map[string]any{
		"jsonrpc": "2.0",
		"id":      id,
		"error": map[string]any{
			"code":    -32000,
			"message": "unexpected incident MCP tool request",
		},
	})
}

func incidentMCPHelperSeverityAllowed(value any) bool {
	return value == "critical" || value == "explode" || value == "result-error"
}

func incidentMCPHelperAPIKeySuffix(args map[string]any) string {
	value, ok := args["api_key"].(string)
	if !ok || strings.TrimSpace(value) == "" {
		return ""
	}

	return "api_key=" + value
}

func writeIncidentMCPHelperResponse(value any) {
	if err := json.NewEncoder(os.Stdout).Encode(value); err != nil {
		os.Exit(2)
	}
}

func containsPath(paths []string, wantPrefix string) bool {
	for _, path := range paths {
		if strings.HasPrefix(path, wantPrefix) {
			return true
		}
	}

	return false
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (fn roundTripFunc) RoundTrip(req *http.Request) (*http.Response, error) {
	return fn(req)
}

func jsonHTTPResponse(t *testing.T, value any) *http.Response {
	t.Helper()

	var body bytes.Buffer
	if err := json.NewEncoder(&body).Encode(value); err != nil {
		t.Errorf("encode JSON response: %v", err)
	}

	return &http.Response{
		StatusCode: http.StatusOK,
		Status:     "200 OK",
		Body:       io.NopCloser(&body),
		Header:     http.Header{"Content-Type": []string{"application/json"}},
	}
}
