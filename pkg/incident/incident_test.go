package incident

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestParseSentryIncident_RedactsAndExtractsContext(t *testing.T) {
	t.Parallel()

	issue := []byte(`{
		"id": "123",
		"shortId": "API-912",
		"title": "OAuth refresh panic",
		"permalink": "https://sentry.example/issues/123",
		"firstSeen": "2026-05-29T10:14:00Z",
		"project": {"slug": "api-gateway"},
		"metadata": {"type": "Panic", "value": "nil OAuth state for user alice@example.com"},
		"firstRelease": {"version": "2026.05.29-1", "ref": "abc123def456", "dateReleased": "2026-05-29T10:00:00Z"},
		"tags": [{"key": "environment", "value": "production"}, {"key": "release", "value": "2026.05.29-1"}]
	}`)
	event := []byte(`{
		"eventID": "evt-1",
		"entries": [
			{"type": "exception", "data": {"values": [{
				"type": "Panic",
				"value": "nil OAuth state refresh_token=secret-refresh",
				"stacktrace": {"frames": [{
					"filename": "github.com/acme/api/pkg/auth/session.go",
					"function": "Refresh",
					"lineno": 184,
					"in_app": true,
					"context": [[183, "before"], [184, "return refresh_token=secret-refresh"], [185, "after"]]
				}]}
			}]}},
			{"type": "request", "data": {
				"method": "POST",
				"url": "https://api.example.test/session?access_token=secret-token",
				"headers": [["Authorization", "Bearer secret-token"], ["Cookie", "sid=secret"]],
				"query": [["project", "42"], ["user_id", "user-123"], ["access_token", "secret-query-token"]],
				"data": "email=alice@example.com&password=hunter2"
			}}
		]
	}`)

	inc, err := ParseSentryIncident(issue, event)
	require.NoError(t, err)

	assert.Equal(t, SourceSentry, inc.Source)
	assert.Equal(t, "API-912", inc.Reference)
	assert.Equal(t, "api-gateway", inc.Service)
	assert.Equal(t, "production", inc.Environment)
	assert.Equal(t, "Panic", inc.ErrorType)
	assert.Equal(t, "2026.05.29-1", inc.Release)
	require.NotEmpty(t, inc.Deployments)
	assert.Equal(t, "2026.05.29-1", inc.Deployments[0].Version)
	assert.Equal(t, "abc123def456", inc.Deployments[0].Commit)
	assert.Equal(t, time.Date(2026, 5, 29, 10, 0, 0, 0, time.UTC), inc.Deployments[0].DeployedAt)
	require.Len(t, inc.StackTrace, 1)
	assert.Equal(t, "github.com/acme/api/pkg/auth/session.go", inc.StackTrace[0].File)
	assert.Equal(t, 184, inc.StackTrace[0].Line)
	assert.Contains(t, inc.StackTrace[0].ContextLine, redactedIncidentValue)
	assert.NotContains(t, inc.StackTrace[0].ContextLine, "secret-refresh")
	assert.Equal(t, redactedIncidentValue, inc.Request.Headers["Authorization"])
	assert.Equal(t, "42", inc.Request.Metadata["query.project"])
	assert.Equal(t, redactedIncidentValue, inc.Request.Metadata["query.user_id"])
	assert.Equal(t, redactedIncidentValue, inc.Request.Metadata["query.access_token"])
	assert.NotContains(t, strings.Join([]string{inc.Message, inc.Request.URL, inc.Request.Data}, " "), "alice@example.com")
	assert.NotContains(t, strings.Join([]string{inc.Message, inc.Request.URL, inc.Request.Data}, " "), "secret-token")
	queryMetadataText := inc.Request.Metadata["query.user_id"] + " " + inc.Request.Metadata["query.access_token"]
	assert.NotContains(t, queryMetadataText, "user-123")
	assert.NotContains(t, queryMetadataText, "secret-query-token")
	assert.Contains(t, inc.Request.Data, redactedIncidentValue)
}

func TestParseSentryIncident_UsesEmbeddedLatestEventWhenSeparateEventMissing(t *testing.T) {
	t.Parallel()

	issue := []byte(`{
		"id": "123",
		"shortId": "API-912",
		"title": "OAuth refresh panic",
		"project": {"slug": "api-gateway"},
		"metadata": {"type": "Panic", "value": "nil OAuth state"},
		"latestEvent": {
			"eventID": "evt-1",
			"entries": [{
				"type": "exception",
				"data": {"values": [{
					"type": "Panic",
					"value": "nil OAuth state for alice@example.com",
					"stacktrace": {"frames": [{
						"filename": "pkg/auth/session.go",
						"function": "Refresh",
						"lineno": 184,
						"in_app": true
					}]}
				}]}
			}]
		}
	}`)

	inc, err := ParseSentryIncident(issue, nil)
	require.NoError(t, err)

	assert.Equal(t, "API-912", inc.Reference)
	assert.Equal(t, "api-gateway", inc.Service)
	assert.NotContains(t, inc.Message, "alice@example.com")
	require.Len(t, inc.StackTrace, 1)
	assert.Equal(t, "pkg/auth/session.go", inc.StackTrace[0].File)
	assert.Equal(t, "Refresh", inc.StackTrace[0].Function)
	assert.Equal(t, 184, inc.StackTrace[0].Line)
}

func TestParseJSONIncident_GenericMetadataDoesNotRequireSentryFields(t *testing.T) {
	t.Parallel()

	inc, err := ParseJSONIncident([]byte(`{
		"source": "datadog",
		"reference": "alert-42",
		"message": "payment queue depth exceeded",
		"metadata": {"team": "billing"}
	}`), SourceMCP, "alert-42")
	require.NoError(t, err)

	assert.Equal(t, SourceMCP, inc.Source)
	assert.Equal(t, "alert-42", inc.Reference)
	assert.Equal(t, "payment queue depth exceeded", inc.Message)
}

func TestParseJSONIncident_InferReferenceFromObservabilityIDs(t *testing.T) {
	t.Parallel()

	inc, err := ParseJSONIncident([]byte(`{
		"source": "tempo",
		"trace_id": "trace-abc123",
		"service": "api-gateway",
		"message": "trace failed"
	}`), SourceMCP, "")
	require.NoError(t, err)

	assert.Equal(t, "trace-abc123", inc.Reference)

	inc, err = ParseJSONIncident([]byte(`{
		"source": "prometheus",
		"alert_id": "alert-99",
		"message": "queue depth exceeded"
	}`), SourceMCP, "cli-ref")
	require.NoError(t, err)

	assert.Equal(t, "cli-ref", inc.Reference)
}

func TestParseJSONIncident_UnwrapsNestedConnectorIncidentPayload(t *testing.T) {
	t.Parallel()

	inc, err := ParseJSONIncident([]byte(`{
		"source": "mcp",
		"incident": {
			"reference": "alert-77",
			"service": "api-gateway",
			"environment": "production",
			"message": "nil OAuth state for alice@example.com access_token=secret-token",
			"stack_trace": [{"file": "pkg/auth/session.go", "line": 184, "function": "Refresh"}]
		}
	}`), SourceMCP, "")
	require.NoError(t, err)

	assert.Equal(t, SourceMCP, inc.Source)
	assert.Equal(t, "alert-77", inc.Reference)
	assert.Equal(t, "api-gateway", inc.Service)
	assert.Equal(t, "production", inc.Environment)
	assert.NotContains(t, inc.Message, "alice@example.com")
	assert.NotContains(t, inc.Message, "secret-token")
	require.Len(t, inc.StackTrace, 1)
	assert.Equal(t, "pkg/auth/session.go", inc.StackTrace[0].File)
	assert.Equal(t, 184, inc.StackTrace[0].Line)
}

func TestParseJSONIncident_GenericEventEnvelopeIsNotMisreadAsSentry(t *testing.T) {
	t.Parallel()

	inc, err := ParseJSONIncident([]byte(`{
		"source": "mcp",
		"event": {
			"reference": "event-99",
			"service": "worker",
			"message": "queue failed for bob@example.com access_token=secret-token"
		}
	}`), SourceMCP, "")
	require.NoError(t, err)

	assert.Equal(t, SourceMCP, inc.Source)
	assert.Equal(t, "event-99", inc.Reference)
	assert.Equal(t, "worker", inc.Service)
	assert.NotContains(t, inc.Message, "bob@example.com")
	assert.NotContains(t, inc.Message, "secret-token")
}

func TestParseJSONIncident_GenericIssueEnvelopeIsUnwrapped(t *testing.T) {
	t.Parallel()

	inc, err := ParseJSONIncident([]byte(`{
		"source": "mcp",
		"issue": {
			"reference": "ticket-123",
			"service": "checkout",
			"message": "payment failed for bob@example.com access_token=secret-token"
		}
	}`), SourceMCP, "")
	require.NoError(t, err)

	assert.Equal(t, SourceMCP, inc.Source)
	assert.Equal(t, "ticket-123", inc.Reference)
	assert.Equal(t, "checkout", inc.Service)
	assert.NotContains(t, inc.Message, "bob@example.com")
	assert.NotContains(t, inc.Message, "secret-token")
}

func TestParseJSONIncident_GenericObservabilityPayloadIsTolerant(t *testing.T) {
	t.Parallel()

	inc, err := ParseJSONIncident([]byte(`{
		"source": "grafana",
		"reference": "alert-42",
		"service": "payments",
		"message": "queue failed for bob@example.com",
		"request": {
			"method": "POST",
			"url": "https://api.example.test/payments?access_token=secret-token",
			"headers": {"Authorization": "Bearer secret-token"},
			"metadata": {"user_id": "user-123"}
		},
		"stacktrace": {"frames": [{"filename": "pkg/payments/worker.go", "lineno": 42, "function": "Process"}]},
		"logs": ["worker failed for bob@example.com", {"source":"loki","message":"retry access_token=secret-token","fields":{"pod":"payments-1","user_id":"user-123"}}],
		"traces": [{"source":"tempo","trace_id":"trace-1","status":"error"}],
		"metrics": [{"name":"queue_depth","value":"42"}],
		"deployments": [{"version":"2026.05.29-1","commit":"abc123def456","environment":"production","deployed_at":"2026-05-29T10:00:00Z"}]
	}`), SourceMCP, "alert-42")
	require.NoError(t, err)

	assert.Equal(t, SourceMCP, inc.Source)
	assert.Equal(t, "payments", inc.Service)
	require.Len(t, inc.StackTrace, 1)
	assert.Equal(t, "pkg/payments/worker.go", inc.StackTrace[0].File)
	require.Len(t, inc.Logs, 2)
	assert.Equal(t, "log", inc.Logs[0].Source)
	require.Len(t, inc.Traces, 1)
	assert.Equal(t, "trace-1", inc.Traces[0].Name)
	require.Len(t, inc.Metrics, 1)
	assert.Equal(t, "queue_depth", inc.Metrics[0].Name)
	require.Len(t, inc.Deployments, 1)
	assert.Equal(t, "2026.05.29-1", inc.Deployments[0].Version)
	assert.Equal(t, time.Date(2026, 5, 29, 10, 0, 0, 0, time.UTC), inc.Deployments[0].DeployedAt)
	assert.Equal(t, redactedIncidentValue, inc.Request.Headers["Authorization"])
	assert.Equal(t, redactedIncidentValue, inc.Request.Metadata["user_id"])
	assert.NotContains(t, inc.Message+inc.Request.URL+inc.Logs[0].Message+inc.Logs[1].Message, "bob@example.com")
	assert.NotContains(t, inc.Message+inc.Request.URL+inc.Logs[0].Message+inc.Logs[1].Message, "secret-token")
}

func TestParseJSONIncident_GitHubDeploymentPayloadExtractsDeployContext(t *testing.T) {
	t.Parallel()

	inc, err := ParseJSONIncident([]byte(`{
		"source": "github",
		"deployment": {
			"environment": "production",
			"ref": "2026.05.29-1",
			"sha": "abc123def456",
			"created_at": "2026-05-29T10:00:00Z"
		}
	}`), SourceMCP, "deployment-77")
	require.NoError(t, err)

	require.Len(t, inc.Deployments, 1)
	assert.Equal(t, "production", inc.Deployments[0].Environment)
	assert.Equal(t, "2026.05.29-1", inc.Deployments[0].Version)
	assert.Equal(t, "abc123def456", inc.Deployments[0].Commit)
	assert.Equal(t, time.Date(2026, 5, 29, 10, 0, 0, 0, time.UTC), inc.Deployments[0].DeployedAt)
}

func TestParseJSONIncident_TopLevelLogArrayIsWrappedAsLogs(t *testing.T) {
	t.Parallel()

	inc, err := ParseJSONIncident([]byte(`[
		{
			"source": "loki",
			"service": "api-gateway",
			"namespace": "production",
			"timestamp": "2026-05-29T10:14:00Z",
			"message": "TypeError: nil OAuth state for bob@example.com access_token=secret-token\n    at Refresh (pkg/auth/session.go:184:9)"
		}
	]`), SourceMCP, "query-logs")
	require.NoError(t, err)

	assert.Equal(t, SourceMCP, inc.Source)
	assert.Equal(t, "query-logs", inc.Reference)
	assert.Equal(t, "api-gateway", inc.Service)
	assert.Equal(t, "production", inc.Environment)
	require.Len(t, inc.Logs, 1)
	assert.Equal(t, "loki", inc.Logs[0].Source)
	assert.Equal(t, time.Date(2026, 5, 29, 10, 14, 0, 0, time.UTC), inc.Logs[0].Timestamp)
	assert.NotContains(t, inc.Logs[0].Message, "bob@example.com")
	assert.NotContains(t, inc.Logs[0].Message, "secret-token")
	require.Len(t, inc.StackTrace, 1)
	assert.Equal(t, "pkg/auth/session.go", inc.StackTrace[0].File)
	assert.Equal(t, 184, inc.StackTrace[0].Line)
}

func TestParseJSONIncident_JSONLinesPayloadIsWrappedAsLogs(t *testing.T) {
	t.Parallel()

	raw := `{"source":"cloud-logging","service":"api-gateway","namespace":"production","timestamp":"2026-05-29T10:14:00Z","message":"panic for bob@example.com access_token=secret-token\n    at Refresh (pkg/auth/session.go:184:9)"}` +
		"\n" +
		`{"source":"cloud-logging","message":"retry failed"}`

	inc, err := ParseJSONIncident([]byte(raw), SourceMCP, "jsonl-logs")
	require.NoError(t, err)

	assert.Equal(t, SourceMCP, inc.Source)
	assert.Equal(t, "jsonl-logs", inc.Reference)
	assert.Equal(t, "api-gateway", inc.Service)
	assert.Equal(t, "production", inc.Environment)
	require.Len(t, inc.Logs, 2)
	assert.Equal(t, "cloud-logging", inc.Logs[0].Source)
	assert.NotContains(t, inc.Logs[0].Message, "bob@example.com")
	assert.NotContains(t, inc.Logs[0].Message, "secret-token")
	require.Len(t, inc.StackTrace, 1)
	assert.Equal(t, "pkg/auth/session.go", inc.StackTrace[0].File)
	assert.Equal(t, 184, inc.StackTrace[0].Line)
	assert.Equal(t, 9, inc.StackTrace[0].Column)
}

func TestParseJSONIncident_GenericNestedErrorObject(t *testing.T) {
	t.Parallel()

	inc, err := ParseJSONIncident([]byte(`{
		"source": "cloudwatch",
		"reference": "alarm-9",
		"service": "api",
		"exception": {
			"type": "TypeError",
			"message": "failed for bob@example.com access_token=secret-token",
			"stacktrace": [{"file": "src/auth/session.ts", "line": "88", "function": "refresh"}]
		}
	}`), SourceMCP, "alarm-9")
	require.NoError(t, err)

	assert.Equal(t, "TypeError", inc.ErrorType)
	assert.NotContains(t, inc.Message, "bob@example.com")
	assert.NotContains(t, inc.Message, "secret-token")
	require.Len(t, inc.StackTrace, 1)
	assert.Equal(t, "src/auth/session.ts", inc.StackTrace[0].File)
	assert.Equal(t, 88, inc.StackTrace[0].Line)
	assert.Equal(t, "refresh", inc.StackTrace[0].Function)
}

func TestParseJSONIncident_GenericRequestObjectBodyIsCapturedAndRedacted(t *testing.T) {
	t.Parallel()

	inc, err := ParseJSONIncident([]byte(`{
		"source": "cloud-logging",
		"reference": "log-7",
		"message": "request failed",
		"request": {
			"method": "POST",
			"path": "/oauth/refresh",
			"body": {
				"email": "bob@example.com",
				"refresh_token": "secret-refresh",
				"state": "oauth-state-secret",
				"code_verifier": "verifier-secret",
				"session_id": "sid-secret"
			}
		}
	}`), SourceMCP, "log-7")
	require.NoError(t, err)

	assert.Equal(t, "POST", inc.Request.Method)
	assert.Equal(t, "/oauth/refresh", inc.Request.URL)
	assert.NotEmpty(t, inc.Request.Data)
	assert.Contains(t, inc.Request.Data, "refresh_token")
	assert.NotContains(t, inc.Request.Data, "bob@example.com")
	assert.NotContains(t, inc.Request.Data, "secret-refresh")
	assert.NotContains(t, inc.Request.Data, "oauth-state-secret")
	assert.NotContains(t, inc.Request.Data, "verifier-secret")
	assert.NotContains(t, inc.Request.Data, "sid-secret")
	assert.Contains(t, inc.Request.Data, redactedIncidentValue)
}

func TestParseJSONIncident_OpenTelemetryAttributesExtractContext(t *testing.T) {
	t.Parallel()

	inc, err := ParseJSONIncident([]byte(`{
		"source": "otel",
		"reference": "trace-123",
		"resource": {
			"attributes": {
				"service.name": "api-gateway",
				"deployment.environment.name": "production",
				"service.version": "2026.05.29-1"
			}
		},
		"attributes": {
			"exception.type": "panic",
			"exception.message": "nil OAuth state for bob@example.com access_token=secret-token",
			"exception.stacktrace": "Error: failed\n    at Refresh (pkg/auth/session.go:184:9)",
			"http.request.method": "POST",
			"url.full": "https://api.example.test/oauth/refresh?access_token=secret-token",
			"http.route": "/oauth/refresh"
		}
	}`), SourceMCP, "trace-123")
	require.NoError(t, err)

	assert.Equal(t, SourceMCP, inc.Source)
	assert.Equal(t, "api-gateway", inc.Service)
	assert.Equal(t, "production", inc.Environment)
	assert.Equal(t, "panic", inc.ErrorType)
	assert.Equal(t, "2026.05.29-1", inc.Version)
	assert.Equal(t, "POST", inc.Request.Method)
	assert.Equal(t, "/oauth/refresh", inc.Request.Metadata["http.route"])
	assert.NotContains(t, inc.Message+inc.Request.URL, "bob@example.com")
	assert.NotContains(t, inc.Message+inc.Request.URL, "secret-token")
	require.Len(t, inc.StackTrace, 1)
	assert.Equal(t, "pkg/auth/session.go", inc.StackTrace[0].File)
	assert.Equal(t, "Refresh", inc.StackTrace[0].Function)
	assert.Equal(t, 184, inc.StackTrace[0].Line)
	assert.Equal(t, 9, inc.StackTrace[0].Column)
}

func TestParseJSONIncident_OTLPAttributeArraysExtractContext(t *testing.T) {
	t.Parallel()

	inc, err := ParseJSONIncident([]byte(`{
		"source": "otel",
		"reference": "span-7",
		"resource": {
			"attributes": [
				{"key": "service.name", "value": {"stringValue": "checkout-api"}},
				{"key": "deployment.environment.name", "value": {"stringValue": "production"}}
			]
		},
		"attributes": [
			{"key": "exception.type", "value": {"stringValue": "RuntimeError"}},
			{"key": "exception.message", "value": {"stringValue": "failed for bob@example.com"}},
			{"key": "exception.stacktrace", "value": {"stringValue": "File \"pkg/checkout/worker.py\", line 52, in process"}},
			{"key": "http.request.method", "value": {"stringValue": "POST"}},
			{"key": "url.path", "value": {"stringValue": "/checkout"}},
			{"key": "http.response.status_code", "value": {"intValue": 500}}
		]
	}`), SourceMCP, "span-7")
	require.NoError(t, err)

	assert.Equal(t, "checkout-api", inc.Service)
	assert.Equal(t, "production", inc.Environment)
	assert.Equal(t, "RuntimeError", inc.ErrorType)
	assert.NotContains(t, inc.Message, "bob@example.com")
	assert.Equal(t, "POST", inc.Request.Method)
	assert.Equal(t, "/checkout", inc.Request.URL)
	assert.Equal(t, "500", inc.Request.Metadata["http.response.status_code"])
	require.Len(t, inc.StackTrace, 1)
	assert.Equal(t, "pkg/checkout/worker.py", inc.StackTrace[0].File)
	assert.Equal(t, "process", inc.StackTrace[0].Function)
	assert.Equal(t, 52, inc.StackTrace[0].Line)
}

func TestParseJSONIncident_OTLPResourceSpansExtractContext(t *testing.T) {
	t.Parallel()

	inc, err := ParseJSONIncident([]byte(`{
		"resourceSpans": [{
			"resource": {"attributes": [
				{"key": "service.name", "value": {"stringValue": "api-gateway"}},
				{"key": "deployment.environment.name", "value": {"stringValue": "production"}},
				{"key": "service.version", "value": {"stringValue": "2026.05.29-1"}}
			]},
			"scopeSpans": [{
				"spans": [{
					"name": "POST /oauth/refresh",
					"traceId": "trace-123",
					"spanId": "span-456",
					"startTimeUnixNano": "1770000000123456789",
					"attributes": [
						{"key": "http.request.method", "value": {"stringValue": "POST"}},
						{"key": "url.path", "value": {"stringValue": "/oauth/refresh"}}
					],
					"events": [{
						"name": "exception",
						"timeUnixNano": "1770000000123456799",
						"attributes": [
							{"key": "exception.type", "value": {"stringValue": "RuntimeError"}},
							{"key": "exception.message", "value": {"stringValue": "nil OAuth state for bob@example.com access_token=secret-token"}},
							{"key": "exception.stacktrace", "value": {"stringValue": "Error: failed\n    at Refresh (pkg/auth/session.go:184:9)"}}
						]
					}]
				}]
			}]
		}]
	}`), SourceMCP, "")
	require.NoError(t, err)

	assert.Equal(t, "trace-123", inc.Reference)
	assert.Equal(t, "api-gateway", inc.Service)
	assert.Equal(t, "production", inc.Environment)
	assert.Equal(t, "2026.05.29-1", inc.Version)
	assert.Equal(t, "RuntimeError", inc.ErrorType)
	assert.NotContains(t, inc.Message, "bob@example.com")
	assert.NotContains(t, inc.Message, "secret-token")
	assert.Equal(t, "POST", inc.Request.Method)
	assert.Equal(t, "/oauth/refresh", inc.Request.URL)
	require.Len(t, inc.Traces, 1)
	assert.Equal(t, "otel", inc.Traces[0].Source)
	assert.Equal(t, "POST /oauth/refresh", inc.Traces[0].Name)
	assert.Equal(t, "trace-123", inc.Traces[0].Fields["traceId"])
	require.Len(t, inc.Logs, 1)
	assert.Equal(t, "exception", inc.Logs[0].Name)
	assert.NotContains(t, inc.Logs[0].Message, "bob@example.com")
	require.Len(t, inc.StackTrace, 1)
	assert.Equal(t, "pkg/auth/session.go", inc.StackTrace[0].File)
	assert.Equal(t, "Refresh", inc.StackTrace[0].Function)
	assert.Equal(t, 184, inc.StackTrace[0].Line)
}

func TestParseJSONIncident_OTLPResourceLogsExtractContext(t *testing.T) {
	t.Parallel()

	inc, err := ParseJSONIncident([]byte(`{
		"resourceLogs": [{
			"resource": {"attributes": [
				{"key": "service.name", "value": {"stringValue": "checkout-api"}},
				{"key": "deployment.environment.name", "value": {"stringValue": "production"}}
			]},
			"scopeLogs": [{
				"logRecords": [{
					"timeUnixNano": "1770000000123456789",
					"severityText": "ERROR",
					"body": {"stringValue": "checkout failed for bob@example.com access_token=secret-token"},
					"attributes": [
						{"key": "exception.type", "value": {"stringValue": "RuntimeError"}},
						{"key": "exception.stacktrace", "value": {"stringValue": "File \"pkg/checkout/worker.py\", line 52, in process"}}
					]
				}]
			}]
		}]
	}`), SourceMCP, "logs-123")
	require.NoError(t, err)

	assert.Equal(t, "checkout-api", inc.Service)
	assert.Equal(t, "production", inc.Environment)
	assert.Equal(t, "RuntimeError", inc.ErrorType)
	require.Len(t, inc.Logs, 1)
	assert.Equal(t, "otel", inc.Logs[0].Source)
	assert.Equal(t, "ERROR", inc.Logs[0].Name)
	assert.NotContains(t, inc.Logs[0].Message, "bob@example.com")
	assert.NotContains(t, inc.Logs[0].Message, "secret-token")
	require.Len(t, inc.StackTrace, 1)
	assert.Equal(t, "pkg/checkout/worker.py", inc.StackTrace[0].File)
	assert.Equal(t, "process", inc.StackTrace[0].Function)
	assert.Equal(t, 52, inc.StackTrace[0].Line)
}

func TestParseJSONIncident_CloudLoggingTextPayloadExtractsResourceAndStack(t *testing.T) {
	t.Parallel()

	inc, err := ParseJSONIncident([]byte(`{
		"source": "gcp-logging",
		"reference": "log-entry-7",
		"resource": {
			"type": "k8s_container",
			"labels": {
				"container_name": "api-gateway",
				"namespace_name": "production",
				"pod_name": "api-1"
			}
		},
		"textPayload": "Traceback (most recent call last):\n  File \"pkg/auth/session.py\", line 77, in refresh\nRuntimeError: failed for alice@example.com access_token=secret-token"
	}`), SourceMCP, "log-entry-7")
	require.NoError(t, err)

	assert.Equal(t, "api-gateway", inc.Service)
	assert.Equal(t, "production", inc.Environment)
	assert.Equal(t, "api-1", inc.Tags["pod_name"])
	assert.NotContains(t, inc.Message, "alice@example.com")
	assert.NotContains(t, inc.Message, "secret-token")
	require.Len(t, inc.StackTrace, 1)
	assert.Equal(t, "pkg/auth/session.py", inc.StackTrace[0].File)
	assert.Equal(t, "refresh", inc.StackTrace[0].Function)
	assert.Equal(t, 77, inc.StackTrace[0].Line)
}

func TestParseJSONIncident_CloudLoggingJSONPayloadExtractsHTTPContext(t *testing.T) {
	t.Parallel()

	inc, err := ParseJSONIncident([]byte(`{
		"source": "gcp-logging",
		"reference": "log-entry-8",
		"timestamp": "2026-05-29T10:14:00Z",
		"resource": {
			"type": "k8s_container",
			"labels": {
				"container_name": "api-gateway",
				"namespace_name": "production"
			}
		},
		"httpRequest": {
			"requestMethod": "POST",
			"requestUrl": "https://api.example.test/oauth/refresh?state=oauth-state-secret",
			"status": 500,
			"remoteIp": "203.0.113.10"
		},
		"jsonPayload": {
			"exception": {
				"type": "RuntimeError",
				"message": "nil OAuth state",
				"stacktrace": "File \"pkg/auth/session.py\", line 91, in refresh"
			}
		}
	}`), SourceMCP, "log-entry-8")
	require.NoError(t, err)

	assert.Equal(t, "api-gateway", inc.Service)
	assert.Equal(t, "production", inc.Environment)
	assert.Equal(t, "RuntimeError", inc.ErrorType)
	assert.Equal(t, "nil OAuth state", inc.Message)
	assert.Equal(t, "POST", inc.Request.Method)
	assert.Equal(t, "500", inc.Request.Metadata["status"])
	assert.NotContains(t, inc.Request.URL, "oauth-state-secret")
	assert.NotContains(t, inc.Request.Metadata["remoteIp"], "203.0.113.10")
	assert.Equal(t, time.Date(2026, 5, 29, 10, 14, 0, 0, time.UTC), inc.Timestamp)
	require.Len(t, inc.StackTrace, 1)
	assert.Equal(t, "pkg/auth/session.py", inc.StackTrace[0].File)
	assert.Equal(t, "refresh", inc.StackTrace[0].Function)
	assert.Equal(t, 91, inc.StackTrace[0].Line)
}

func TestParseJSONIncident_CloudLoggingProtoPayloadExtractsAuditContext(t *testing.T) {
	t.Parallel()

	inc, err := ParseJSONIncident([]byte(`{
		"source": "gcp-logging",
		"reference": "audit-log-9",
		"timestamp": "2026-05-29T10:14:00Z",
		"protoPayload": {
			"serviceName": "container.googleapis.com",
			"methodName": "io.k8s.core.v1.pods.update",
			"resourceName": "namespaces/production/pods/api-gateway-1",
			"status": {
				"code": 13,
				"message": "pod update failed for alice@example.com access_token=secret-token"
			},
			"requestMetadata": {
				"callerIp": "203.0.113.10",
				"callerSuppliedUserAgent": "kubectl"
			},
			"authenticationInfo": {
				"principalEmail": "alice@example.com"
			}
		}
	}`), SourceMCP, "audit-log-9")
	require.NoError(t, err)

	assert.Equal(t, "container.googleapis.com", inc.Service)
	assert.Equal(t, "13", inc.ErrorType)
	assert.NotContains(t, inc.Message, "alice@example.com")
	assert.NotContains(t, inc.Message, "secret-token")
	assert.NotContains(t, inc.Request.Metadata["callerIp"], "203.0.113.10")
	assert.NotContains(t, inc.Request.Metadata["principalEmail"], "alice@example.com")
	require.Len(t, inc.Logs, 1)
	assert.Equal(t, "gcp-logging", inc.Logs[0].Source)
	assert.Equal(t, "io.k8s.core.v1.pods.update", inc.Logs[0].Name)
	assert.Equal(t, time.Date(2026, 5, 29, 10, 14, 0, 0, time.UTC), inc.Logs[0].Timestamp)
	assert.NotContains(t, inc.Logs[0].Message, "secret-token")
}

func TestParseJSONIncident_GCPErrorReportingEventExtractsContext(t *testing.T) {
	t.Parallel()

	inc, err := ParseJSONIncident([]byte(`{
		"source": "gcp-error-reporting",
		"groupId": "CJ6lm9fVvNa5CQ",
		"eventTime": "2026-05-29T10:14:00Z",
		"message": "Traceback (most recent call last):\nRuntimeError: nil OAuth state for bob@example.com access_token=secret-token",
		"serviceContext": {
			"service": "api-gateway",
			"version": "2026.05.29-1",
			"resourceType": "k8s_container"
		},
		"context": {
			"httpRequest": {
				"method": "POST",
				"url": "https://api.example.test/oauth/refresh?state=oauth-state-secret",
				"responseStatusCode": 500,
				"remoteIp": "203.0.113.10"
			},
			"reportLocation": {
				"filePath": "pkg/auth/session.go",
				"lineNumber": 184,
				"functionName": "Refresh"
			}
		}
	}`), SourceMCP, "")
	require.NoError(t, err)

	assert.Equal(t, "CJ6lm9fVvNa5CQ", inc.Reference)
	assert.Equal(t, "api-gateway", inc.Service)
	assert.Equal(t, "2026.05.29-1", inc.Version)
	assert.Equal(t, "POST", inc.Request.Method)
	assert.Equal(t, "500", inc.Request.Metadata["responseStatusCode"])
	assert.Equal(t, "k8s_container", inc.Tags["resourceType"])
	assert.NotContains(t, inc.Message+inc.Request.URL+inc.Request.Metadata["remoteIp"], "bob@example.com")
	assert.NotContains(t, inc.Message+inc.Request.URL+inc.Request.Metadata["remoteIp"], "secret-token")
	assert.NotContains(t, inc.Message+inc.Request.URL+inc.Request.Metadata["remoteIp"], "oauth-state-secret")
	assert.NotContains(t, inc.Message+inc.Request.URL+inc.Request.Metadata["remoteIp"], "203.0.113.10")
	assert.Equal(t, time.Date(2026, 5, 29, 10, 14, 0, 0, time.UTC), inc.Timestamp)
	require.Len(t, inc.StackTrace, 1)
	assert.Equal(t, "pkg/auth/session.go", inc.StackTrace[0].File)
	assert.Equal(t, "Refresh", inc.StackTrace[0].Function)
	assert.Equal(t, 184, inc.StackTrace[0].Line)
}

func TestParseJSONIncident_CloudWatchLogEventsExtractStackAndMillisTimestamp(t *testing.T) {
	t.Parallel()

	inc, err := ParseJSONIncident([]byte(`{
		"source": "cloudwatch",
		"reference": "log-group-1",
		"service": "api-gateway",
		"logEvents": [{
			"timestamp": 1770000000123,
			"logStream": "api-gateway/1",
			"message": "TypeError: nil OAuth state for bob@example.com\n    at Refresh (pkg/auth/session.go:184:9)"
		}]
	}`), SourceMCP, "log-group-1")
	require.NoError(t, err)

	assert.Equal(t, "api-gateway", inc.Service)
	require.Len(t, inc.Logs, 1)
	assert.Equal(t, time.UnixMilli(1770000000123).UTC(), inc.Logs[0].Timestamp)
	assert.Equal(t, "api-gateway/1", inc.Logs[0].Fields["logStream"])
	assert.NotContains(t, inc.Logs[0].Message, "bob@example.com")
	require.Len(t, inc.StackTrace, 1)
	assert.Equal(t, "pkg/auth/session.go", inc.StackTrace[0].File)
	assert.Equal(t, "Refresh", inc.StackTrace[0].Function)
	assert.Equal(t, 184, inc.StackTrace[0].Line)
	assert.Equal(t, 9, inc.StackTrace[0].Column)
}

func TestParseJSONIncident_CloudWatchLogsInsightsResultsExtractStack(t *testing.T) {
	t.Parallel()

	inc, err := ParseJSONIncident([]byte(`{
		"source": "cloudwatch",
		"reference": "query-77",
		"results": [[
			{"field": "@timestamp", "value": "2026-05-29T10:14:00Z"},
			{"field": "@message", "value": "TypeError: nil OAuth state for bob@example.com access_token=secret-token\n    at Refresh (pkg/auth/session.go:184:9)"},
			{"field": "@log", "value": "/aws/eks/prod/api"},
			{"field": "@logStream", "value": "api-gateway/1"},
			{"field": "service_name", "value": "api-gateway"},
			{"field": "environment", "value": "production"},
			{"field": "user_id", "value": "user-123"}
		]]
	}`), SourceMCP, "query-77")
	require.NoError(t, err)

	assert.Equal(t, "api-gateway", inc.Service)
	assert.Equal(t, "production", inc.Environment)
	require.Len(t, inc.Logs, 1)
	assert.Equal(t, "/aws/eks/prod/api", inc.Logs[0].Source)
	assert.Equal(t, "api-gateway/1", inc.Logs[0].Name)
	assert.Equal(t, time.Date(2026, 5, 29, 10, 14, 0, 0, time.UTC), inc.Logs[0].Timestamp)
	assert.Equal(t, redactedIncidentValue, inc.Logs[0].Fields["user_id"])
	assert.NotContains(t, inc.Logs[0].Message, "bob@example.com")
	assert.NotContains(t, inc.Logs[0].Message, "secret-token")
	require.Len(t, inc.StackTrace, 1)
	assert.Equal(t, "pkg/auth/session.go", inc.StackTrace[0].File)
	assert.Equal(t, "Refresh", inc.StackTrace[0].Function)
	assert.Equal(t, 184, inc.StackTrace[0].Line)
	assert.Equal(t, 9, inc.StackTrace[0].Column)
}

func TestParseJSONIncident_CloudWatchAlarmExtractsMetricContext(t *testing.T) {
	t.Parallel()

	inc, err := ParseJSONIncident([]byte(`{
		"source": "aws.cloudwatch",
		"detail-type": "CloudWatch Alarm State Change",
		"id": "event-123",
		"time": "2026-05-29T10:15:00Z",
		"detail": {
			"alarmName": "api-gateway-errors-high",
			"alarmArn": "arn:aws:cloudwatch:us-east-1:123456789012:alarm:api-gateway-errors-high",
			"state": {
				"value": "ALARM",
				"reason": "Threshold Crossed: 5 datapoints for bob@example.com access_token=secret-token",
				"timestamp": "2026-05-29T10:14:00Z"
			},
			"configuration": {
				"metrics": [{
					"id": "m1",
					"metricStat": {
						"period": 60,
						"metric": {
							"namespace": "AWS/Lambda",
							"name": "Errors",
							"dimensions": [
								{"Name": "FunctionName", "Value": "api-gateway"},
								{"Name": "Environment", "Value": "production"},
								{"Name": "user_id", "Value": "user-123"}
							]
						}
					}
				}]
			}
		}
	}`), SourceMCP, "")
	require.NoError(t, err)

	assert.Equal(t, "arn:aws:cloudwatch:us-east-1:123456789012:alarm:api-gateway-errors-high", inc.Reference)
	assert.Equal(t, "api-gateway", inc.Service)
	assert.Equal(t, "production", inc.Environment)
	assert.Equal(t, "ALARM", inc.ErrorType)
	assert.Equal(t, "api-gateway-errors-high", inc.Title)
	assert.NotContains(t, inc.Message, "bob@example.com")
	assert.NotContains(t, inc.Message, "secret-token")
	assert.Equal(t, time.Date(2026, 5, 29, 10, 14, 0, 0, time.UTC), inc.Timestamp)
	require.Len(t, inc.Metrics, 1)
	assert.Equal(t, "cloudwatch", inc.Metrics[0].Source)
	assert.Equal(t, "api-gateway-errors-high", inc.Metrics[0].Name)
	assert.Equal(t, "AWS/Lambda", inc.Metrics[0].Fields["metric.namespace"])
	assert.Equal(t, "Errors", inc.Metrics[0].Fields["metric.name"])
	assert.Equal(t, redactedIncidentValue, inc.Metrics[0].Fields["user_id"])
	assert.NotContains(t, inc.Metrics[0].Message, "bob@example.com")
	assert.NotContains(t, inc.Metrics[0].Message, "secret-token")
}

func TestParseJSONIncident_LokiStreamsExtractLabelsAndStack(t *testing.T) {
	t.Parallel()

	inc, err := ParseJSONIncident([]byte(`{
		"source": "grafana-loki",
		"reference": "query-1",
		"data": {
			"resultType": "streams",
			"result": [{
				"stream": {
					"job": "api-gateway",
					"namespace": "production",
					"pod": "api-gateway-abc123"
				},
				"values": [
					["1770000000123456789", "TypeError: nil OAuth state for bob@example.com access_token=secret-token\n    at Refresh (pkg/auth/session.go:184:9)"]
				]
			}]
		}
	}`), SourceMCP, "query-1")
	require.NoError(t, err)

	assert.Equal(t, "api-gateway", inc.Service)
	assert.Equal(t, "production", inc.Environment)
	require.Len(t, inc.Logs, 1)
	assert.Equal(t, "loki", inc.Logs[0].Source)
	assert.Equal(t, "api-gateway-abc123", inc.Logs[0].Name)
	assert.Equal(t, "api-gateway", inc.Logs[0].Fields["job"])
	assert.Equal(t, time.Unix(0, 1770000000123456789).UTC(), inc.Logs[0].Timestamp)
	assert.NotContains(t, inc.Logs[0].Message, "bob@example.com")
	assert.NotContains(t, inc.Logs[0].Message, "secret-token")
	require.Len(t, inc.StackTrace, 1)
	assert.Equal(t, "pkg/auth/session.go", inc.StackTrace[0].File)
	assert.Equal(t, "Refresh", inc.StackTrace[0].Function)
	assert.Equal(t, 184, inc.StackTrace[0].Line)
	assert.Equal(t, 9, inc.StackTrace[0].Column)
}

func TestParseJSONIncident_PrometheusResultExtractsMetricContext(t *testing.T) {
	t.Parallel()

	inc, err := ParseJSONIncident([]byte(`{
		"source": "prometheus",
		"reference": "query-2",
		"status": "success",
		"data": {
			"resultType": "vector",
			"result": [{
				"metric": {
					"__name__": "http_server_errors_total",
					"job": "api-gateway",
					"namespace": "production",
					"pod": "api-gateway-abc123"
				},
				"value": [1770000000.123, "5"]
			}]
		}
	}`), SourceMCP, "query-2")
	require.NoError(t, err)

	assert.Equal(t, "api-gateway", inc.Service)
	assert.Equal(t, "production", inc.Environment)
	require.Len(t, inc.Metrics, 1)
	assert.Equal(t, "prometheus", inc.Metrics[0].Source)
	assert.Equal(t, "http_server_errors_total", inc.Metrics[0].Name)
	assert.Equal(t, "5", inc.Metrics[0].Message)
	assert.Equal(t, "5", inc.Metrics[0].Fields["value"])
	assert.Equal(t, "api-gateway-abc123", inc.Metrics[0].Fields["pod"])
	assert.WithinDuration(t, time.Unix(1770000000, 123000000).UTC(), inc.Metrics[0].Timestamp, time.Millisecond)
}

func TestParseJSONIncident_AlertmanagerWebhookExtractsAlertContext(t *testing.T) {
	t.Parallel()

	inc, err := ParseJSONIncident([]byte(`{
		"receiver": "backend-team",
		"status": "firing",
		"groupKey": "alert-group-123",
		"externalURL": "https://alerts.example.test",
		"groupLabels": {"alertname": "HighErrorRate"},
		"commonLabels": {
			"alertname": "HighErrorRate",
			"service": "api-gateway",
			"namespace": "production",
			"severity": "critical"
		},
		"commonAnnotations": {
			"summary": "high error rate on api-gateway",
			"description": "OAuth refresh failures for bob@example.com access_token=secret-token"
		},
		"alerts": [{
			"status": "firing",
			"fingerprint": "fp-123",
			"startsAt": "2026-05-29T10:14:00Z",
			"generatorURL": "https://prometheus.example.test/graph?g0.expr=http_errors",
			"labels": {
				"pod": "api-gateway-abc123",
				"user_id": "user-123"
			},
			"annotations": {
				"description": "trace_id=trace-123 failed for bob@example.com"
			}
		}]
	}`), SourceMCP, "")
	require.NoError(t, err)

	assert.Equal(t, SourceMCP, inc.Source)
	assert.Equal(t, "alert-group-123", inc.Reference)
	assert.Equal(t, "api-gateway", inc.Service)
	assert.Equal(t, "production", inc.Environment)
	assert.Equal(t, "high error rate on api-gateway", inc.Title)
	assert.NotContains(t, inc.Message, "bob@example.com")
	assert.NotContains(t, inc.Message, "secret-token")
	assert.Equal(t, "critical", inc.Tags["severity"])
	require.Len(t, inc.Logs, 1)
	assert.Equal(t, "alertmanager", inc.Logs[0].Source)
	assert.Equal(t, "HighErrorRate", inc.Logs[0].Name)
	assert.Equal(t, "api-gateway-abc123", inc.Logs[0].Fields["pod"])
	assert.Equal(t, redactedIncidentValue, inc.Logs[0].Fields["user_id"])
	assert.NotContains(t, inc.Logs[0].Message, "bob@example.com")
	assert.Equal(t, time.Date(2026, 5, 29, 10, 14, 0, 0, time.UTC), inc.Logs[0].Timestamp)
}

func TestParseJSONIncident_TraceOnlyPayloadExtractsServiceHints(t *testing.T) {
	t.Parallel()

	inc, err := ParseJSONIncident([]byte(`{
		"source": "tempo",
		"reference": "trace-42",
		"traces": [{
			"source": "tempo",
			"name": "trace-42",
			"message": "error",
			"fields": {
				"service.name": "api-gateway",
				"deployment.environment.name": "production",
				"span": "POST /oauth/refresh",
				"user_id": "user-123"
			}
		}]
	}`), SourceMCP, "trace-42")
	require.NoError(t, err)

	assert.Equal(t, "api-gateway", inc.Service)
	assert.Equal(t, "production", inc.Environment)
	require.Len(t, inc.Traces, 1)
	assert.Equal(t, redactedIncidentValue, inc.Traces[0].Fields["user_id"])
	assert.Equal(t, "POST /oauth/refresh", inc.Traces[0].Fields["span"])
}

func TestParseJSONIncident_TraceObservationStackFieldsExtractStackTrace(t *testing.T) {
	t.Parallel()

	inc, err := ParseJSONIncident([]byte(`{
		"source": "tempo",
		"reference": "trace-42",
		"traces": [{
			"source": "tempo",
			"name": "trace-42",
			"fields": {
				"service.name": "api-gateway",
				"exception.stacktrace": "TypeError: nil OAuth state\n    at Refresh (pkg/auth/session.go:184:9)"
			}
		}]
	}`), SourceMCP, "trace-42")
	require.NoError(t, err)

	require.Len(t, inc.StackTrace, 1)
	assert.Equal(t, "pkg/auth/session.go", inc.StackTrace[0].File)
	assert.Equal(t, "Refresh", inc.StackTrace[0].Function)
	assert.Equal(t, 184, inc.StackTrace[0].Line)
	assert.Equal(t, 9, inc.StackTrace[0].Column)
}

func TestParseJSONIncident_KubernetesEventListExtractsAffectedObject(t *testing.T) {
	t.Parallel()

	inc, err := ParseJSONIncident([]byte(`{
		"apiVersion": "v1",
		"kind": "EventList",
		"items": [{
			"metadata": {
				"name": "api-gateway.1770000000",
				"namespace": "production",
				"creationTimestamp": "2026-05-29T10:14:00Z"
			},
			"involvedObject": {
				"kind": "Pod",
				"namespace": "production",
				"name": "api-gateway-abc123",
				"fieldPath": "spec.containers{api}"
			},
			"reason": "CrashLoopBackOff",
			"message": "Back-off restarting failed container api for bob@example.com",
			"source": {"component": "kubelet", "host": "node-1"},
			"lastTimestamp": "2026-05-29T10:15:00Z"
		}]
	}`), SourceMCP, "k8s-events")
	require.NoError(t, err)

	assert.Equal(t, SourceMCP, inc.Source)
	assert.Equal(t, "k8s-events", inc.Reference)
	assert.Equal(t, "api-gateway-abc123", inc.Service)
	assert.Equal(t, "production", inc.Environment)
	require.Len(t, inc.Logs, 1)
	assert.Equal(t, "kubelet", inc.Logs[0].Source)
	assert.Equal(t, "CrashLoopBackOff", inc.Logs[0].Name)
	assert.NotContains(t, inc.Logs[0].Message, "bob@example.com")
	assert.Equal(t, "Pod", inc.Logs[0].Fields["involvedObject.kind"])
	assert.Equal(t, "api-gateway-abc123", inc.Logs[0].Fields["involvedObject.name"])
	assert.Equal(t, "production", inc.Logs[0].Fields["metadata.namespace"])
	assert.Equal(t, time.Date(2026, 5, 29, 10, 15, 0, 0, time.UTC), inc.Logs[0].Timestamp)
}

func TestParseJSONIncident_KubernetesSingleEventExtractsRegardingObject(t *testing.T) {
	t.Parallel()

	inc, err := ParseJSONIncident([]byte(`{
		"apiVersion": "events.k8s.io/v1",
		"kind": "Event",
		"metadata": {
			"name": "api-gateway.1770000001",
			"namespace": "production",
			"creationTimestamp": "2026-05-29T10:14:00Z"
		},
		"regarding": {
			"kind": "Pod",
			"namespace": "production",
			"name": "api-gateway-xyz789",
			"fieldPath": "spec.containers{api}"
		},
		"reason": "BackOff",
		"note": "Back-off restarting container api for bob@example.com access_token=secret-token",
		"reportingController": "kubelet",
		"eventTime": "2026-05-29T10:15:00Z"
	}`), SourceMCP, "k8s-event")
	require.NoError(t, err)

	assert.Equal(t, "api-gateway-xyz789", inc.Service)
	assert.Equal(t, "production", inc.Environment)
	require.Len(t, inc.Logs, 1)
	assert.Equal(t, "kubelet", inc.Logs[0].Source)
	assert.Equal(t, "BackOff", inc.Logs[0].Name)
	assert.NotContains(t, inc.Logs[0].Message, "bob@example.com")
	assert.NotContains(t, inc.Logs[0].Message, "secret-token")
	assert.Equal(t, "Pod", inc.Logs[0].Fields["regarding.kind"])
	assert.Equal(t, "api-gateway-xyz789", inc.Logs[0].Fields["regarding.name"])
	assert.Equal(t, "production", inc.Logs[0].Fields["regarding.namespace"])
	assert.Equal(t, time.Date(2026, 5, 29, 10, 15, 0, 0, time.UTC), inc.Logs[0].Timestamp)
}

func TestParseJSONIncident_AzureMonitorExceptionExtractsContext(t *testing.T) {
	t.Parallel()

	inc, err := ParseJSONIncident([]byte(`{
		"source": "azure-monitor",
		"operation_Id": "op-123",
		"cloud_RoleName": "checkout-api",
		"problemId": "System.NullReferenceException",
		"outerMessage": "nil OAuth state for bob@example.com",
		"timestamp": "2026-05-29T10:14:00Z",
		"customDimensions": {
			"AspNetCoreEnvironment": "Production",
			"DeploymentVersion": "2026.05.29-1",
			"user_Id": "user-123",
			"authorization_code": "auth-code-secret"
		},
		"details": [{
			"parsedStack": [{
				"method": "Checkout.Auth.Session.Refresh",
				"fileName": "src/Checkout/Auth/Session.cs",
				"line": 184
			}]
		}]
	}`), SourceMCP, "")
	require.NoError(t, err)

	assert.Equal(t, "op-123", inc.Reference)
	assert.Equal(t, "checkout-api", inc.Service)
	assert.Equal(t, "Production", inc.Environment)
	assert.Equal(t, "System.NullReferenceException", inc.ErrorType)
	assert.Equal(t, "2026.05.29-1", inc.Version)
	assert.Equal(t, "2026.05.29-1", inc.Release)
	assert.NotContains(t, inc.Message, "bob@example.com")
	assert.Equal(t, time.Date(2026, 5, 29, 10, 14, 0, 0, time.UTC), inc.Timestamp)
	require.Len(t, inc.StackTrace, 1)
	assert.Equal(t, "src/Checkout/Auth/Session.cs", inc.StackTrace[0].File)
	assert.Equal(t, "Checkout.Auth.Session.Refresh", inc.StackTrace[0].Function)
	assert.Equal(t, 184, inc.StackTrace[0].Line)
}

func TestParseJSONIncident_AzureMonitorTablesExtractRows(t *testing.T) {
	t.Parallel()

	inc, err := ParseJSONIncident([]byte(`{
		"source": "azure-monitor",
		"tables": [{
			"name": "AppExceptions",
			"columns": [
				{"name": "TimeGenerated"},
				{"name": "OperationId"},
				{"name": "AppRoleName"},
				{"name": "ProblemId"},
				{"name": "OuterMessage"},
				{"name": "Details"},
				{"name": "CustomDimensions"}
			],
			"rows": [[
				"2026-05-29T10:14:00Z",
				"op-789",
				"checkout-api",
				"System.NullReferenceException",
				"nil OAuth state for bob@example.com access_token=secret-token",
				"[{\"parsedStack\":[{\"method\":\"Checkout.Auth.Session.Refresh\",\"fileName\":\"src/Checkout/Auth/Session.cs\",\"line\":184}]}]",
				"{\"AspNetCoreEnvironment\":\"Production\",\"DeploymentVersion\":\"2026.05.29-1\",\"user_Id\":\"user-123\"}"
			]]
		}]
	}`), SourceMCP, "")
	require.NoError(t, err)

	assert.Equal(t, "op-789", inc.Reference)
	assert.Equal(t, "checkout-api", inc.Service)
	assert.Equal(t, "Production", inc.Environment)
	assert.Equal(t, "System.NullReferenceException", inc.ErrorType)
	assert.Equal(t, "2026.05.29-1", inc.Release)
	assert.Equal(t, "2026.05.29-1", inc.Version)
	assert.NotContains(t, inc.Message, "bob@example.com")
	assert.NotContains(t, inc.Message, "secret-token")
	assert.Equal(t, time.Date(2026, 5, 29, 10, 14, 0, 0, time.UTC), inc.Timestamp)
	require.Len(t, inc.Logs, 1)
	assert.Equal(t, "azure-monitor", inc.Logs[0].Source)
	assert.Equal(t, "AppExceptions", inc.Logs[0].Name)
	assert.NotContains(t, inc.Logs[0].Message, "bob@example.com")
	assert.Equal(t, redactedIncidentValue, inc.Logs[0].Fields["user_id"])
	require.Len(t, inc.StackTrace, 1)
	assert.Equal(t, "src/Checkout/Auth/Session.cs", inc.StackTrace[0].File)
	assert.Equal(t, "Checkout.Auth.Session.Refresh", inc.StackTrace[0].Function)
	assert.Equal(t, 184, inc.StackTrace[0].Line)
}

func TestParseJSONIncident_DotNetStackTraceTextExtractsFrame(t *testing.T) {
	t.Parallel()

	inc, err := ParseJSONIncident([]byte(`{
		"source": "azure-monitor",
		"operation_Id": "op-124",
		"problemId": "System.NullReferenceException",
		"outerMessage": "System.NullReferenceException: nil OAuth state\n   at Checkout.Auth.Session.Refresh(String token) in /src/Checkout/Auth/Session.cs:line 184\n   at Checkout.Api.Handler.Invoke() in /src/Checkout/Api/Handler.cs:line 42"
	}`), SourceMCP, "")
	require.NoError(t, err)

	require.Len(t, inc.StackTrace, 2)
	assert.Equal(t, "/src/Checkout/Auth/Session.cs", inc.StackTrace[0].File)
	assert.Equal(t, "Checkout.Auth.Session.Refresh(String token)", inc.StackTrace[0].Function)
	assert.Equal(t, 184, inc.StackTrace[0].Line)
	assert.Equal(t, "/src/Checkout/Api/Handler.cs", inc.StackTrace[1].File)
	assert.Equal(t, 42, inc.StackTrace[1].Line)
}

func TestParseJSONIncident_NestedAttributesExtractDatadogLikeContext(t *testing.T) {
	t.Parallel()

	inc, err := ParseJSONIncident([]byte(`{
		"source": "datadog",
		"id": "log-42",
		"attributes": {
			"service": "payments",
			"timestamp": "2026-05-29T10:14:00Z",
			"attributes": {
				"error": {
					"type": "TypeError",
					"message": "nil OAuth state access_token=secret-token",
					"stack": "TypeError: nil OAuth state\n    at Charge (pkg/payments/charge.ts:42:7)"
				},
				"http": {
					"method": "POST",
					"url": "https://api.example.test/payments?access_token=secret-token",
					"route": "/payments/{id}"
				}
			}
		}
	}`), SourceMCP, "log-42")
	require.NoError(t, err)

	assert.Equal(t, "payments", inc.Service)
	assert.Equal(t, "TypeError", inc.ErrorType)
	assert.Equal(t, time.Date(2026, 5, 29, 10, 14, 0, 0, time.UTC), inc.Timestamp)
	assert.Equal(t, "POST", inc.Request.Method)
	assert.Equal(t, "/payments/{id}", inc.Request.Metadata["http.route"])
	assert.NotContains(t, inc.Message+inc.Request.URL, "secret-token")
	require.Len(t, inc.StackTrace, 1)
	assert.Equal(t, "pkg/payments/charge.ts", inc.StackTrace[0].File)
	assert.Equal(t, "Charge", inc.StackTrace[0].Function)
	assert.Equal(t, 42, inc.StackTrace[0].Line)
	assert.Equal(t, 7, inc.StackTrace[0].Column)
}

func TestParseJSONIncident_DatadogMonitorTagsExtractServiceHints(t *testing.T) {
	t.Parallel()

	inc, err := ParseJSONIncident([]byte(`{
		"source": "datadog",
		"alert_id": "monitor-42",
		"title": "High OAuth refresh errors",
		"message": "nil OAuth state for bob@example.com access_token=secret-token",
		"tags": [
			"service:api-gateway",
			"env:production",
			"version:2026.05.29-1",
			"user_id:user-123"
		]
	}`), SourceMCP, "")
	require.NoError(t, err)

	assert.Equal(t, "monitor-42", inc.Reference)
	assert.Equal(t, "api-gateway", inc.Service)
	assert.Equal(t, "production", inc.Environment)
	assert.Equal(t, "2026.05.29-1", inc.Version)
	assert.Equal(t, "2026.05.29-1", inc.Release)
	assert.Equal(t, "api-gateway", inc.Tags["service"])
	assert.Equal(t, redactedIncidentValue, inc.Tags["user_id"])
	assert.NotContains(t, inc.Message, "bob@example.com")
	assert.NotContains(t, inc.Message, "secret-token")

	analysis, err := Analyze(t.Context(), inc, AnalysisOptions{})
	require.NoError(t, err)

	report := RenderMarkdown(analysis)
	assert.Contains(t, report, "Tags/context present")
	assert.Contains(t, report, "service")
	assert.NotContains(t, report, "user-123")
	assert.Contains(t, analysis.FixPrompt, "Tags/context present")
	assert.NotContains(t, analysis.FixPrompt, "user-123")
}

func TestParseJSONIncident_DatadogDataArrayExtractsLogContext(t *testing.T) {
	t.Parallel()

	inc, err := ParseJSONIncident([]byte(`{
		"data": [{
			"id": "log-42",
			"type": "logs",
			"attributes": {
				"service": "payments",
				"timestamp": "2026-05-29T10:14:00Z",
				"message": "TypeError: nil OAuth state for bob@example.com access_token=secret-token",
				"attributes": {
					"env": "production",
					"user_id": "user-123",
					"error": {
						"type": "TypeError",
						"message": "nil OAuth state for bob@example.com",
						"stack": "TypeError: nil OAuth state\n    at Charge (pkg/payments/charge.ts:42:7)"
					},
					"http": {
						"method": "POST",
						"url": "https://api.example.test/payments?access_token=secret-token",
						"route": "/payments/{id}"
					}
				}
			}
		}]
	}`), SourceMCP, "")
	require.NoError(t, err)

	assert.Equal(t, "log-42", inc.Reference)
	assert.Equal(t, "payments", inc.Service)
	assert.Equal(t, "production", inc.Environment)
	assert.Equal(t, "TypeError", inc.ErrorType)
	assert.Equal(t, time.Date(2026, 5, 29, 10, 14, 0, 0, time.UTC), inc.Timestamp)
	assert.Equal(t, "POST", inc.Request.Method)
	assert.Equal(t, "/payments/{id}", inc.Request.Metadata["http.route"])
	require.Len(t, inc.Logs, 1)
	assert.Equal(t, "logs", inc.Logs[0].Source)
	assert.Equal(t, "log-42", inc.Logs[0].Name)
	assert.Equal(t, redactedIncidentValue, inc.Logs[0].Fields["user_id"])
	assert.NotContains(t, inc.Logs[0].Message+inc.Message+inc.Request.URL, "bob@example.com")
	assert.NotContains(t, inc.Logs[0].Message+inc.Message+inc.Request.URL, "secret-token")
	require.Len(t, inc.StackTrace, 1)
	assert.Equal(t, "pkg/payments/charge.ts", inc.StackTrace[0].File)
	assert.Equal(t, "Charge", inc.StackTrace[0].Function)
	assert.Equal(t, 42, inc.StackTrace[0].Line)
	assert.Equal(t, 7, inc.StackTrace[0].Column)
}

func TestRedactMetadataRedactsDottedUserKeys(t *testing.T) {
	t.Parallel()

	got := RedactMetadata(map[string]string{
		"user.id":        "user-123",
		"enduser.email":  "bob@example.com",
		"http.route":     "/checkout/{id}",
		"client.address": "203.0.113.10",
		"oauth.state":    "oauth-state-secret",
		"session.id":     "sid-secret",
	})

	assert.Equal(t, redactedIncidentValue, got["user.id"])
	assert.Equal(t, redactedIncidentValue, got["enduser.email"])
	assert.Equal(t, "/checkout/{id}", got["http.route"])
	assert.NotContains(t, got["client.address"], "203.0.113.10")
	assert.Equal(t, redactedIncidentValue, got["oauth.state"])
	assert.Equal(t, redactedIncidentValue, got["session.id"])
}

func TestRedactTextRedactsIPv6AddressesWithoutStackFrameFalsePositive(t *testing.T) {
	t.Parallel()

	got := RedactText("remote_addr=2001:db8::1 peer=[2001:db8::2] stack=pkg/auth/session.go:184:9")

	assert.NotContains(t, got, "2001:db8::1")
	assert.NotContains(t, got, "2001:db8::2")
	assert.Contains(t, got, redactedIPValue)
	assert.Contains(t, got, "pkg/auth/session.go:184:9")
}

func TestParseJSONIncident_RawSentryEventExtractsStackTrace(t *testing.T) {
	t.Parallel()

	inc, err := ParseJSONIncident([]byte(`{
		"eventID": "evt-1",
		"groupID": "API-912",
		"message": "nil OAuth state",
		"entries": [{
			"type": "exception",
			"data": {"values": [{
				"type": "Panic",
				"value": "nil OAuth state",
				"stacktrace": {"frames": [{
					"filename": "pkg/auth/session.go",
					"function": "Refresh",
					"lineno": 184,
					"colNo": 7,
					"in_app": true
				}]}
			}]}
		}]
	}`), SourceSentry, "")
	require.NoError(t, err)

	assert.Equal(t, "API-912", inc.Reference)
	require.Len(t, inc.StackTrace, 1)
	assert.Equal(t, "pkg/auth/session.go", inc.StackTrace[0].File)
	assert.Equal(t, 184, inc.StackTrace[0].Line)
	assert.Equal(t, 7, inc.StackTrace[0].Column)
}

func TestAnalyze_LinksJVMStackTraceUsingInferredPackagePath(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	require.NoError(t, writeTestFile(filepath.Join(root, "src", "main", "java", "com", "acme", "auth", "Session.java"), "package com.acme.auth;\nclass Session {}\n"))

	inc, err := ParseJSONIncident([]byte(`{
		"source": "mcp",
		"reference": "trace-42",
		"message": "java.lang.NullPointerException: nil OAuth state\n\tat com.acme.auth.Session.refresh(Session.java:184)\n\tat com.acme.auth.Handler.handle(Handler.java:42)"
	}`), SourceMCP, "trace-42")
	require.NoError(t, err)
	require.NotEmpty(t, inc.StackTrace)
	assert.Equal(t, "Session.java", inc.StackTrace[0].File)
	assert.Equal(t, "com/acme/auth/Session.java", inc.StackTrace[0].AbsPath)

	analysis, err := Analyze(t.Context(), inc, AnalysisOptions{RepoRoot: root})
	require.NoError(t, err)

	require.NotEmpty(t, analysis.CodeCandidates)
	assert.Equal(t, "src/main/java/com/acme/auth/Session.java", analysis.CodeCandidates[0].Path)
	assert.Equal(t, "medium", analysis.CodeCandidates[0].Confidence)
	assert.Contains(t, analysis.CodeCandidates[0].Reason, "suffix")
}

func TestAnalyze_LinksGoPanicStackTraceWithFunctionName(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	require.NoError(t, writeTestFile(filepath.Join(root, "pkg", "auth", "session.go"), "package auth\nfunc Refresh() {}\n"))

	inc, err := ParseJSONIncident([]byte(`{
		"source": "mcp",
		"reference": "trace-43",
		"message": "panic: nil OAuth state\n\ngithub.com/acme/api/pkg/auth.(*Session).Refresh(...)\n\t/workspace/pkg/auth/session.go:184 +0x123\ngithub.com/acme/api/pkg/http.(*Handler).Serve(...)\n\t/workspace/pkg/http/handler.go:42 +0x456"
	}`), SourceMCP, "trace-43")
	require.NoError(t, err)
	require.NotEmpty(t, inc.StackTrace)
	assert.Equal(t, "workspace/pkg/auth/session.go", inc.StackTrace[0].File)
	assert.Equal(t, "github.com/acme/api/pkg/auth.(*Session).Refresh", inc.StackTrace[0].Function)

	analysis, err := Analyze(t.Context(), inc, AnalysisOptions{RepoRoot: root})
	require.NoError(t, err)

	require.NotEmpty(t, analysis.CodeCandidates)
	assert.Equal(t, "pkg/auth/session.go", analysis.CodeCandidates[0].Path)
	assert.Equal(t, "github.com/acme/api/pkg/auth.(*Session).Refresh", analysis.CodeCandidates[0].Function)
	assert.Contains(t, analysis.TestPlan.Steps[0], "github.com/acme/api/pkg/auth.(*Session).Refresh")
}

func TestAnalyze_LinksStackTraceAndBuildsTestFirstPlan(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	require.NoError(t, writeTestFile(filepath.Join(root, "pkg", "auth", "session.go"), "package auth\nfunc Refresh() {}\n"))
	require.NoError(t, writeTestFile(filepath.Join(root, ".github", "CODEOWNERS"), "/pkg/auth/ @auth-team\n"))

	inc := Context{
		Source:    SourceSentry,
		Reference: "API-912",
		Service:   "api-gateway",
		Message:   "token refresh panic",
		StackTrace: []StackFrame{{
			File:     "github.com/acme/api/pkg/auth/session.go",
			Function: "Refresh",
			Line:     184,
			InApp:    true,
		}},
	}

	analysis, err := Analyze(t.Context(), inc, AnalysisOptions{RepoRoot: root})
	require.NoError(t, err)

	require.NotEmpty(t, analysis.CodeCandidates)
	assert.Equal(t, "pkg/auth/session.go", analysis.CodeCandidates[0].Path)
	assert.Equal(t, []string{"@auth-team"}, analysis.CodeCandidates[0].Owners)
	assert.Equal(t, "High", analysis.Risk.Level)
	assert.Contains(t, analysis.PRPlan.SuggestedReviewers, "@auth-team")
	assert.Contains(t, analysis.TestPlan.Steps[0], "pkg/auth/session.go")
	assert.Contains(t, analysis.FixPrompt, "Prefer creating a failing regression test")
	assert.Contains(t, analysis.FixPrompt, "Owners: @auth-team")
}

func TestAnalyze_CodeownersDoubleStarRoutesNestedOwners(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	require.NoError(t, writeTestFile(filepath.Join(root, "pkg", "auth", "internal", "session.go"), "package internal\nfunc Refresh() {}\n"))
	require.NoError(t, writeTestFile(filepath.Join(root, ".github", "CODEOWNERS"), "/pkg/auth/** @security-team\n"))

	analysis, err := Analyze(t.Context(), Context{
		Source:    SourceFile,
		Reference: "incident-owners",
		StackTrace: []StackFrame{{
			File:     "/workspace/pkg/auth/internal/session.go",
			Function: "Refresh",
			Line:     42,
			InApp:    true,
		}},
	}, AnalysisOptions{RepoRoot: root})
	require.NoError(t, err)

	require.NotEmpty(t, analysis.CodeCandidates)
	assert.Equal(t, "pkg/auth/internal/session.go", analysis.CodeCandidates[0].Path)
	assert.Equal(t, []string{"@security-team"}, analysis.CodeCandidates[0].Owners)
	assert.Contains(t, analysis.PRPlan.SuggestedReviewers, "@security-team")
}

func TestAnalyze_BoundsPRTitle(t *testing.T) {
	t.Parallel()

	analysis, err := Analyze(t.Context(), Context{
		Source:    SourceFile,
		Reference: strings.Repeat("incident-reference-", 20),
		Message:   strings.Repeat("token refresh panic ", 20),
	}, AnalysisOptions{})
	require.NoError(t, err)

	assert.LessOrEqual(t, len(analysis.PRPlan.Title), maxPRTitleLength)
	assert.Contains(t, analysis.PRPlan.Title, "...")
}

func TestLinkCodeCandidates_PrefersMostRecentInAppFrame(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	require.NoError(t, writeTestFile(filepath.Join(root, "pkg", "db", "driver.go"), "package db\nfunc Query() {}\n"))
	require.NoError(t, writeTestFile(filepath.Join(root, "pkg", "auth", "session.go"), "package auth\nfunc Refresh() {}\n"))
	require.NoError(t, writeTestFile(filepath.Join(root, "pkg", "http", "handler.go"), "package http\nfunc Serve() {}\n"))

	candidates, warnings, err := LinkCodeCandidates(t.Context(), Context{
		StackTrace: []StackFrame{
			{File: "pkg/db/driver.go", Function: "Query", Line: 10, InApp: false},
			{File: "pkg/auth/session.go", Function: "Refresh", Line: 184, InApp: true},
			{File: "pkg/http/handler.go", Function: "Serve", Line: 42, InApp: true},
		},
	}, root, 3, 100)
	require.NoError(t, err)

	require.Empty(t, warnings)
	require.Len(t, candidates, 3)
	assert.Equal(t, "pkg/http/handler.go", candidates[0].Path)
	assert.Equal(t, "pkg/auth/session.go", candidates[1].Path)
	assert.Equal(t, "pkg/db/driver.go", candidates[2].Path)
}

func TestRenderText_DoesNotLeakSensitiveIncidentData(t *testing.T) {
	t.Parallel()

	analysis, err := Analyze(t.Context(), Context{
		Source:    SourceFile,
		Reference: "local-1",
		Message:   "failed for bob@example.com Authorization: Bearer secret-token",
		Request: Request{
			URL:     "https://api.example.test/session?user_id=user-123&access_token=secret-token",
			Headers: map[string]string{"Cookie": "sid=secret-cookie", "X-Forwarded-For": "203.0.113.10"},
			Data:    `{"user_id":"user-123","account_id":"acct-456","ip_address":"203.0.113.10"}`,
		},
	}, AnalysisOptions{})
	require.NoError(t, err)

	report := RenderText(analysis)
	assert.NotContains(t, report, "bob@example.com")
	assert.NotContains(t, report, "secret-token")
	assert.NotContains(t, report, "user-123")
	assert.NotContains(t, report, "acct-456")
	assert.NotContains(t, report, "203.0.113.10")
	assert.Contains(t, report, RedactionPolicyVersion)
}

func TestRenderMarkdownIncludesRedactedObservabilityContext(t *testing.T) {
	t.Parallel()

	analysis, err := Analyze(t.Context(), Context{
		Source:    SourceMCP,
		Reference: "alert-42",
		Message:   "payment queue failed for bob@example.com",
		Request: Request{
			Method:   "POST",
			URL:      "https://api.example.test/payments?access_token=secret-token",
			Headers:  map[string]string{"Authorization": "Bearer secret-token", "X-Request-ID": "req-123"},
			Metadata: map[string]string{"route": "/payments", "user_id": "user-123"},
			Data:     `{"email":"bob@example.com","refresh_token":"secret-refresh"}`,
		},
		Logs: []Observation{{
			Source:  "loki",
			Name:    "error",
			Message: "refresh_token=secret-refresh failed for bob@example.com",
			Fields:  map[string]string{"pod": "api-1", "user_id": "user-123"},
		}},
		Traces: []Observation{{
			Source: "tempo",
			Name:   "trace-123",
			Fields: map[string]string{"span": "POST /payments"},
		}},
		Metrics: []Observation{{
			Source:  "prometheus",
			Name:    "payment_queue_depth",
			Message: "queue depth exceeded",
		}},
		Deployments: []Deployment{{
			Version:     "2026.05.29-1",
			Commit:      "abc123def456",
			Environment: "production",
		}},
	}, AnalysisOptions{})
	require.NoError(t, err)

	report := RenderMarkdown(analysis)
	assert.Contains(t, report, "## Observability context")
	assert.Contains(t, report, "Request: POST")
	assert.Contains(t, report, "Request headers present with values redacted")
	assert.Contains(t, report, "Trace: tempo")
	assert.Contains(t, report, "Log/breadcrumb: loki")
	assert.Contains(t, report, "Metric: prometheus")
	assert.Contains(t, report, "Deployment: version=2026.05.29-1 commit=abc123def456 environment=production")
	assert.NotContains(t, report, "bob@example.com")
	assert.NotContains(t, report, "secret-token")
	assert.NotContains(t, report, "secret-refresh")
	assert.NotContains(t, analysis.FixPrompt, "bob@example.com")
	assert.Contains(t, analysis.FixPrompt, "Redacted observability context")
}

func TestRenderMarkdownIncludesIncidentTitle(t *testing.T) {
	t.Parallel()

	analysis, err := Analyze(t.Context(), Context{
		Source:    SourceMCP,
		Reference: "alert-42",
		Title:     "High error rate for bob@example.com access_token=secret-token",
	}, AnalysisOptions{})
	require.NoError(t, err)

	report := RenderMarkdown(analysis)
	assert.Contains(t, report, "- **Title:** High error rate")
	assert.NotContains(t, report, "bob@example.com")
	assert.NotContains(t, report, "secret-token")
	assert.Contains(t, analysis.FixPrompt, "- Title: High error rate")
	assert.NotContains(t, analysis.FixPrompt, "bob@example.com")
	assert.NotContains(t, analysis.FixPrompt, "secret-token")
}

func TestRenderMarkdownIncludesPRReadiness(t *testing.T) {
	t.Parallel()

	report := RenderMarkdown(Analysis{
		Incident: Context{Source: SourceFile, Reference: "incident-1"},
		PRPlan: PRPlan{
			Summary:            "Open a PR only after validation evidence is present.",
			BodySections:       []string{"Linked incident reference", "Validation results"},
			SuggestedReviewers: []string{"@auth-team"},
		},
		Risk:            Risk{Level: "Medium", Rationale: "test"},
		RedactionPolicy: RedactionPolicyVersion,
	})

	assert.Contains(t, report, "## PR readiness")
	assert.Contains(t, report, "Open a PR only after validation evidence is present.")
	assert.Contains(t, report, "Linked incident reference, Validation results")
	assert.Contains(t, report, "@auth-team")
}

func TestSensitiveValuesExtractsCommandAuditFragments(t *testing.T) {
	t.Parallel()

	values := SensitiveValues(`curl "https://api.example.test?access_token=secret-token&user_id=user-123&state=oauth-state-secret" --token cli-secret-token --password hunter2 -H "Authorization: Bearer bearer-secret" -d '{"email":"bob@example.com","ip_address":"203.0.113.10","remote_addr":"2001:db8::1","code_verifier":"verifier-secret","session_id":"sid-secret"}'`)

	assert.Contains(t, values, "secret-token")
	assert.Contains(t, values, "cli-secret-token")
	assert.Contains(t, values, "hunter2")
	assert.Contains(t, values, "bearer-secret")
	assert.Contains(t, values, "user-123")
	assert.Contains(t, values, "bob@example.com")
	assert.Contains(t, values, "203.0.113.10")
	assert.Contains(t, values, "2001:db8::1")
	assert.Contains(t, values, "oauth-state-secret")
	assert.Contains(t, values, "verifier-secret")
	assert.Contains(t, values, "sid-secret")
}

func TestRedactCommandResultRedactsSpaceSeparatedSecretFlags(t *testing.T) {
	t.Parallel()

	result := RedactCommandResult(CommandResult{
		Command: `curl --token cli-secret-token --password hunter2 -H "Authorization: Bearer bearer-secret"`,
	})

	assert.NotContains(t, result.Command, "cli-secret-token")
	assert.NotContains(t, result.Command, "hunter2")
	assert.NotContains(t, result.Command, "bearer-secret")
	assert.Contains(t, result.Command, redactedIncidentValue)
}

func TestRedactTextPreservesQuotedCommandBoundaries(t *testing.T) {
	t.Parallel()

	got := RedactText("printf 'reproduced access_token=secret-token\\n'")

	assert.Equal(t, "printf 'reproduced access_token="+redactedIncidentValue+"\\n'", got)
	assert.NotContains(t, got, "secret-token")
}

func TestPullRequestsFromTextExtractsCommonCommitRefs(t *testing.T) {
	t.Parallel()

	got := PullRequestsFromText("Fix auth refresh (#218); follow-up PR #219 and pull request 220")

	assert.Equal(t, []string{"#218", "#219", "#220"}, got)
}

func TestRenderTextIncludesCorrelatedPullRequests(t *testing.T) {
	t.Parallel()

	report := RenderText(Analysis{
		Incident: Context{Source: SourceFile, Reference: "incident-1"},
		RecentChanges: []Change{{
			Hash:         "abc123def456",
			Subject:      "Fix auth refresh (#218)",
			Match:        "pkg/auth/session.go",
			PullRequests: []string{"#218"},
		}},
		Risk:            Risk{Level: "Low", Rationale: "test"},
		RedactionPolicy: RedactionPolicyVersion,
	})

	assert.Contains(t, report, "PRs: #218")
}

func TestRenderTextHighlightsLikelyRegressionFromDeploymentCommit(t *testing.T) {
	t.Parallel()

	report := RenderText(Analysis{
		Incident: Context{
			Source:    SourceMCP,
			Reference: "alert-42",
			Deployments: []Deployment{{
				Commit: "abc123def456",
			}},
		},
		RecentChanges: []Change{
			{Hash: "fff999000111", Subject: "Unrelated cleanup"},
			{Hash: "abc123def4567890", Subject: "Fix auth refresh (#218)", PullRequests: []string{"#218"}},
		},
		Risk:            Risk{Level: "Low", Rationale: "test"},
		RedactionPolicy: RedactionPolicyVersion,
	})

	assert.Contains(t, report, "Likely regression: PR #218 / commit abc123def456 / Fix auth refresh (#218)")
}

func TestRenderTextIncludesRedactedCommandEvidence(t *testing.T) {
	t.Parallel()

	analysis := Analysis{
		Incident: Context{Source: SourceFile, Reference: "incident-1"},
		Reproduction: CommandResult{
			Command:  "go test ./pkg/auth",
			Status:   commandStatusFailed,
			Duration: 1500 * time.Millisecond,
			Stdout:   "failed for bob@example.com access_token=secret-token",
			Stderr:   "panic user_id=user-123",
		},
		TestPlan:        Plan{Summary: "Add failing regression test."},
		FixPlan:         Plan{Summary: "Patch narrow failing path."},
		Risk:            Risk{Level: "Low", Rationale: "test"},
		RedactionPolicy: RedactionPolicyVersion,
	}

	report := RenderText(analysis)

	assert.Contains(t, report, "`go test ./pkg/auth`: failed in 1.5s")
	assert.Contains(t, report, "stdout:")
	assert.Contains(t, report, "stderr:")
	assert.NotContains(t, report, "bob@example.com")
	assert.NotContains(t, report, "secret-token")
	assert.NotContains(t, report, "user-123")

	prompt := BuildFixPrompt(analysis)
	assert.Contains(t, prompt, "Reproduction result")
	assert.Contains(t, prompt, "`go test ./pkg/auth`: failed in 1.5s")
	assert.Contains(t, prompt, "Test-first plan")
	assert.Contains(t, prompt, "Fix plan")
	assert.NotContains(t, prompt, "bob@example.com")
	assert.NotContains(t, prompt, "secret-token")
	assert.NotContains(t, prompt, "user-123")
}

func TestRenderMarkdownIncludesWorktreeChanges(t *testing.T) {
	t.Parallel()

	report := RenderMarkdown(Analysis{
		Incident: Context{Source: SourceFile, Reference: "incident-1"},
		WorktreeChanges: []WorktreeChange{
			{Status: "M", Path: "pkg/auth/session.go"},
			{Status: "??", Path: "pkg/auth/session_test.go"},
		},
		Risk:            Risk{Level: "Low", Rationale: "test"},
		RedactionPolicy: RedactionPolicyVersion,
	})

	assert.Contains(t, report, "- M pkg/auth/session.go")
	assert.Contains(t, report, "- ?? pkg/auth/session_test.go")
	assert.Contains(t, report, "Tests added/changed")
	assert.Contains(t, report, "  - ?? pkg/auth/session_test.go")
}

func TestAnalyze_RiskIncludesRepairLoopWorktreeChanges(t *testing.T) {
	t.Parallel()

	analysis, err := Analyze(t.Context(), Context{
		Source:    SourceFile,
		Reference: "incident-1",
		Message:   "unexpected refresh error",
	}, AnalysisOptions{
		WorktreeChanges: []WorktreeChange{{Status: "M", Path: "pkg/auth/session.go"}},
	})
	require.NoError(t, err)

	assert.Equal(t, "High", analysis.Risk.Level)
	assert.Contains(t, analysis.Risk.Rationale, "repair changes")
	assert.Contains(t, analysis.Risk.SuggestedReviewers, "security-team")
}

func writeTestFile(path, content string) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
		return err
	}

	return os.WriteFile(path, []byte(content), 0o600)
}
